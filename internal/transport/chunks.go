// Copyright 2017-2019 Lei Ni (nilei81@gmail.com) and other Dragonboat authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/lni/dragonboat/v3/internal/fileutil"
	"github.com/lni/dragonboat/v3/internal/rsm"
	"github.com/lni/dragonboat/v3/internal/server"
	"github.com/lni/dragonboat/v3/internal/settings"
	"github.com/lni/dragonboat/v3/internal/vfs"
	"github.com/lni/dragonboat/v3/raftio"
	pb "github.com/lni/dragonboat/v3/raftpb"
)

var (
	// ErrSnapshotOutOfDate is returned when the snapshot being received is
	// considered as out of date.
	ErrSnapshotOutOfDate     = errors.New("snapshot is out of date")
	gcIntervalTick           = settings.Soft.SnapshotGCTick
	snapshotChunkTimeoutTick = settings.Soft.SnapshotChunkTimeoutTick
	maxConcurrentSlot        = settings.Soft.MaxConcurrentStreamingSnapshot
)

func chunkKey(c pb.SnapshotChunk) string {
	return fmt.Sprintf("%d:%d:%d", c.ClusterId, c.NodeId, c.Index)
}

type tracked struct {
	firstChunk pb.SnapshotChunk
	extraFiles []*pb.SnapshotFile
	validator  *rsm.SnapshotValidator
	nextChunk  uint64
	tick       uint64
}

type ssLock struct {
	mu sync.Mutex
}

func (l *ssLock) lock() {
	l.mu.Lock()
}

func (l *ssLock) unlock() {
	l.mu.Unlock()
}

// Chunks managed on the receiving side
type Chunks struct {
	currentTick     uint64
	validate        bool
	folder          server.GetSnapshotDirFunc
	onReceive       func(pb.MessageBatch)
	confirm         func(uint64, uint64, uint64)
	getDeploymentID func() uint64
	tracked         map[string]*tracked
	locks           map[string]*ssLock
	timeoutTick     uint64
	gcTick          uint64
	fs              vfs.IFS
	mu              sync.Mutex
}

// NewChunks creates and returns a new snapshot chunks instance.
func NewChunks(onReceive func(pb.MessageBatch),
	confirm func(uint64, uint64, uint64), getDeploymentID func() uint64,
	folder server.GetSnapshotDirFunc, fs vfs.IFS) *Chunks {
	return &Chunks{
		validate:        true,
		onReceive:       onReceive,
		confirm:         confirm,
		getDeploymentID: getDeploymentID,
		tracked:         make(map[string]*tracked),
		locks:           make(map[string]*ssLock),
		timeoutTick:     snapshotChunkTimeoutTick,
		gcTick:          gcIntervalTick,
		folder:          folder,
		fs:              fs,
	}
}

// AddChunk adds an received trunk to chunks.
func (c *Chunks) AddChunk(chunk pb.SnapshotChunk) bool {
	did := c.getDeploymentID()
	if chunk.DeploymentId != did ||
		chunk.BinVer != raftio.RPCBinVersion {
		plog.Errorf("invalid did or binver, %d, %d, %d, %d",
			chunk.DeploymentId, did, chunk.BinVer, raftio.RPCBinVersion)
		return false
	}
	key := chunkKey(chunk)
	lock := c.getSnapshotLock(key)
	lock.lock()
	defer lock.unlock()
	return c.addLocked(chunk)
}

// Tick moves the internal logical clock forward.
func (c *Chunks) Tick() {
	ct := atomic.AddUint64(&c.currentTick, 1)
	if ct%c.gcTick == 0 {
		c.gc()
	}
}

// Close closes the chunks instance.
func (c *Chunks) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tracked {
		c.removeTempDir(t.firstChunk)
	}
}

func (c *Chunks) gc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	tick := c.getTick()
	for k, td := range c.tracked {
		if tick-td.tick >= c.timeoutTick {
			c.removeTempDir(td.firstChunk)
			c.resetLocked(k)
		}
	}
}

func (c *Chunks) getTick() uint64 {
	return atomic.LoadUint64(&c.currentTick)
}

func (c *Chunks) reset(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetLocked(key)
}

func (c *Chunks) resetLocked(key string) {
	delete(c.tracked, key)
}

func (c *Chunks) getSnapshotLock(key string) *ssLock {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[key]
	if !ok {
		l = &ssLock{}
		c.locks[key] = l
	}
	return l
}

func (c *Chunks) full() bool {
	return uint64(len(c.tracked)) >= maxConcurrentSlot
}

func (c *Chunks) record(chunk pb.SnapshotChunk) *tracked {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := chunkKey(chunk)
	td := c.tracked[key]
	if chunk.ChunkId == 0 {
		plog.Infof("first chunk of a snapshot %s received", key)
		if td != nil {
			plog.Warningf("removing unclaimed chunks %s", key)
			c.removeTempDir(td.firstChunk)
		} else {
			if c.full() {
				plog.Errorf("max slot count reached, dropped a chunk %s", key)
				return nil
			}
		}
		validator := rsm.NewSnapshotValidator()
		if c.validate && !chunk.HasFileInfo {
			if !validator.AddChunk(chunk.Data, chunk.ChunkId) {
				return nil
			}
		}
		td = &tracked{
			nextChunk:  1,
			firstChunk: chunk,
			validator:  validator,
			extraFiles: make([]*pb.SnapshotFile, 0),
		}
		c.tracked[key] = td
	} else {
		if td == nil {
			plog.Errorf("not tracked chunk %s ignored, id %d", key, chunk.ChunkId)
			return nil
		}
		if td.nextChunk != chunk.ChunkId {
			plog.Errorf("out of order, %s, want %d, got %d",
				key, td.nextChunk, chunk.ChunkId)
			return nil
		}
		from := chunk.From
		want := td.firstChunk.From
		if want != from {
			from := chunk.From
			want := td.firstChunk.From
			plog.Errorf("ignored %s, from %d, want %d", key, from, want)
			return nil
		}
		td.nextChunk = chunk.ChunkId + 1
	}
	if chunk.FileChunkId == 0 && chunk.HasFileInfo {
		td.extraFiles = append(td.extraFiles, &chunk.FileInfo)
	}
	td.tick = c.getTick()
	return td
}

func (c *Chunks) shouldValidate(chunk pb.SnapshotChunk) bool {
	return c.validate && !chunk.HasFileInfo && chunk.ChunkId != 0
}

func (c *Chunks) addLocked(chunk pb.SnapshotChunk) bool {
	key := chunkKey(chunk)
	td := c.record(chunk)
	if td == nil {
		plog.Warningf("ignored a chunk belongs to %s", key)
		return false
	}
	removed, err := c.nodeRemoved(chunk)
	if err != nil {
		panic(err)
	}
	if removed {
		c.removeTempDir(chunk)
		plog.Warningf("node removed, ignored chunk %s", key)
		return false
	}
	if c.shouldValidate(chunk) {
		if !td.validator.AddChunk(chunk.Data, chunk.ChunkId) {
			plog.Warningf("ignored a invalid chunk %s", key)
			return false
		}
	}
	if err := c.save(chunk); err != nil {
		plog.Errorf("failed to save a chunk %s, %v", key, err)
		c.removeTempDir(chunk)
		panic(err)
	}
	if chunk.IsLastChunk() {
		plog.Infof("last chunk %s received", key)
		defer c.reset(key)
		if c.validate {
			if !td.validator.Validate() {
				plog.Warningf("dropped an invalid snapshot %s", key)
				c.removeTempDir(chunk)
				return false
			}
		}
		if err := c.finalize(chunk, td); err != nil {
			c.removeTempDir(chunk)
			if err != ErrSnapshotOutOfDate {
				plog.Panicf("%s failed when finalizing, %v", key, err)
			}
			return false
		}
		snapshotMessage := c.toMessage(td.firstChunk, td.extraFiles)
		plog.Infof("%s received snapshot from %d, idx %d, term %d",
			dn(chunk.ClusterId, chunk.NodeId), chunk.From, chunk.Index, chunk.Term)
		c.onReceive(snapshotMessage)
		c.confirm(chunk.ClusterId, chunk.NodeId, chunk.From)
	}
	return true
}

func (c *Chunks) nodeRemoved(chunk pb.SnapshotChunk) (bool, error) {
	env := c.getSSEnv(chunk)
	dir := env.GetRootDir()
	return fileutil.IsDirMarkedAsDeleted(dir, c.fs)
}

func (c *Chunks) save(chunk pb.SnapshotChunk) (err error) {
	env := c.getSSEnv(chunk)
	if chunk.ChunkId == 0 {
		if err := env.CreateTempDir(); err != nil {
			return err
		}
	}
	fn := c.fs.PathBase(chunk.Filepath)
	fp := c.fs.PathJoin(env.GetTempDir(), fn)
	var f *ChunkFile
	if chunk.FileChunkId == 0 {
		f, err = CreateChunkFile(fp, c.fs)
	} else {
		f, err = OpenChunkFileForAppend(fp, c.fs)
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	n, err := f.Write(chunk.Data)
	if err != nil {
		return err
	}
	if len(chunk.Data) != n {
		return io.ErrShortWrite
	}
	if chunk.IsLastChunk() || chunk.IsLastFileChunk() {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Chunks) getSSEnv(chunk pb.SnapshotChunk) *server.SSEnv {
	return server.NewSSEnv(c.folder, chunk.ClusterId, chunk.NodeId,
		chunk.Index, chunk.From, server.ReceivingMode, c.fs)
}

func (c *Chunks) finalize(chunk pb.SnapshotChunk, td *tracked) error {
	env := c.getSSEnv(chunk)
	msg := c.toMessage(td.firstChunk, td.extraFiles)
	if len(msg.Requests) != 1 || msg.Requests[0].Type != pb.InstallSnapshot {
		panic("invalid message")
	}
	ss := &msg.Requests[0].Snapshot
	err := env.FinalizeSnapshot(ss)
	if err == server.ErrSnapshotOutOfDate {
		return ErrSnapshotOutOfDate
	}
	return err
}

func (c *Chunks) removeTempDir(chunk pb.SnapshotChunk) {
	env := c.getSSEnv(chunk)
	env.MustRemoveTempDir()
}

func (c *Chunks) toMessage(chunk pb.SnapshotChunk,
	files []*pb.SnapshotFile) pb.MessageBatch {
	if chunk.ChunkId != 0 {
		panic("not first chunk")
	}
	env := c.getSSEnv(chunk)
	snapDir := env.GetFinalDir()
	m := pb.Message{}
	m.Type = pb.InstallSnapshot
	m.From = chunk.From
	m.To = chunk.NodeId
	m.ClusterId = chunk.ClusterId
	s := pb.Snapshot{}
	s.Index = chunk.Index
	s.Term = chunk.Term
	s.OnDiskIndex = chunk.OnDiskIndex
	s.Membership = chunk.Membership
	fn := c.fs.PathBase(chunk.Filepath)
	s.Filepath = c.fs.PathJoin(snapDir, fn)
	s.FileSize = chunk.FileSize
	s.Witness = chunk.Witness
	m.Snapshot = s
	m.Snapshot.Files = files
	for idx := range m.Snapshot.Files {
		fp := c.fs.PathJoin(snapDir, m.Snapshot.Files[idx].Filename())
		m.Snapshot.Files[idx].Filepath = fp
	}
	return pb.MessageBatch{
		BinVer:       chunk.BinVer,
		DeploymentId: chunk.DeploymentId,
		Requests:     []pb.Message{m},
	}
}
