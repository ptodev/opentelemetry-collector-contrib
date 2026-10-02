// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

// queueCapacity is how many marshaled batches can wait for the writers. It's
// large, so max_buffer_mib is what limits the buffer. When the buffer is full,
// new batches are dropped, so a slow disk never slows the pipeline.
const queueCapacity = 4096

// writerCount is how many goroutines compress and write each session's
// batches. Each has its own file. The pipeline hands batches over from many
// goroutines at once, and one writer can't keep up with a burst. A variable
// so tests can use one writer, and get one file.
var writerCount = 4

// Why a session stopped.
const (
	// The client sent Stop.
	stopReasonClientStop = "client_stop"
	// The client's stream ended, and no client came back within the grace
	// period.
	stopReasonClientGone = "client_gone"
	stopReasonShutdown   = "shutdown"
	stopReasonWriteErr   = "write_error"
)

// A snapshot's status.
const (
	statusRunning = "running"
	statusStopped = "stopped"
)

// statsInterval is how often a session writes a stats snapshot, whatever its
// files do, so its counters can be shown over time. A variable so tests can
// shorten it.
var statsInterval = 15 * time.Second

// rotateInterval is how long a file stays open at most, so a client gets data
// soon even when little arrives. A variable so tests can shorten it.
var rotateInterval = 10 * time.Second

// headerFile is written once when a session starts. Stats snapshots are
// stats-NNNNNN.json.
const headerFile = "session.json"

type batch struct {
	data    []byte
	records int64
}

// session writes one signal's kept records to one session directory, from
// when a client starts recording until it stops. The processing goroutines
// only call admit, submit, and release. The writer goroutines compress and
// write batches, each to its own file. The coordinator goroutine (run) writes
// the stats snapshots, and stops the session. Every file in the directory is
// written once and never changed, so a client can pull each one as soon as
// it appears.
type session struct {
	cfg       *Config
	signal    string
	processor string
	logger    *zap.Logger
	dir       string
	// header is set in start, before any batch arrives, and only read
	// afterwards.
	header sessionHeader

	recording      atomic.Bool
	queue          chan batch
	queuedBytes    atomic.Int64
	maxQueuedBytes int64
	// inflight counts batches between admit and release. When recording
	// stops, the coordinator waits for them, so every kept record is written
	// or dropped.
	inflight atomic.Int64
	// end asks the coordinator to stop, with a reason. Only the first reason
	// counts.
	end chan string
	// drain tells the writers to write what's queued, close their files,
	// and exit.
	drain   chan struct{}
	writers sync.WaitGroup
	stopped sync.WaitGroup

	// Counters. Every record the tap sees while recording is either skipped
	// (the buffer was full, or the disk limit was reached), matched no rule,
	// or kept. Every kept record is eventually written or dropped.
	recordsSeen     atomic.Int64
	recordsSkipped  atomic.Int64
	recordsDiskFull atomic.Int64
	recordsKept     atomic.Int64
	recordsDropped  atomic.Int64
	batchesDropped  atomic.Int64
	ruleMatches     []atomic.Int64
	recordsWritten  atomic.Int64
	bytesWritten    atomic.Int64
	// closed is set once a write fails, or the session has stopped. Writers
	// drop what they get after that.
	closed atomic.Bool

	maxDiskBytes int64
	rotateBytes  int64
	// disk counts everything in the directory, shared by every session that
	// writes to it from this process. The server subtracts what it deletes.
	disk *diskBudget

	// mu guards fileCount, newFiles, and failure, which the writers share
	// with the coordinator.
	mu        sync.Mutex
	fileCount int
	// newFiles are the files closed since the last snapshot.
	newFiles []fileInfo
	failure  error
	// snapshots is only used by the coordinator.
	snapshots int
}

// writer is one writer goroutine's state.
type writer struct {
	s    *session
	file *openFile
}

// diskBudget is the size of one directory on disk. Sessions of different
// signals, and of different processors, can write to the same directory, and
// max_disk_mib applies to all of them together.
type diskBudget struct {
	used atomic.Int64
}

var diskBudgets = struct {
	sync.Mutex
	byDir map[string]*diskBudget
}{byDir: map[string]*diskBudget{}}

// budgetFor returns the directory's shared budget, measured from disk again,
// so it includes earlier sessions and files removed since.
func budgetFor(dir string) (*diskBudget, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	used, err := directorySize(abs)
	if err != nil {
		return nil, err
	}
	diskBudgets.Lock()
	defer diskBudgets.Unlock()
	b, ok := diskBudgets.byDir[abs]
	if !ok {
		b = &diskBudget{}
		diskBudgets.byDir[abs] = b
	}
	b.used.Store(used)
	return b, nil
}

// existingBudget returns the directory's budget if a session has measured it,
// or nil.
func existingBudget(dir string) *diskBudget {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	diskBudgets.Lock()
	defer diskBudgets.Unlock()
	return diskBudgets.byDir[abs]
}

// add records n more bytes on disk, or fewer when n is negative, and returns
// the new total.
func (b *diskBudget) add(n int64) int64 {
	return b.used.Add(n)
}

// openFile is the file being written. Batches go through a zstd encoder into
// a counter, so both the uncompressed size (for rotation) and the size on disk
// (for the disk limit) are known.
type openFile struct {
	name    string
	opened  time.Time
	f       *os.File
	enc     *zstd.Encoder
	disk    *countingWriter
	raw     int64
	records int64
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// sessionHeader is the content of session.json: what doesn't change while
// the session runs.
type sessionHeader struct {
	ConventionsVersion int          `json:"conventions_version"`
	SessionID          string       `json:"session_id"`
	Instance           string       `json:"instance"`
	Processor          string       `json:"processor"`
	Signal             string       `json:"signal"`
	ConfigHash         string       `json:"config_hash"`
	Rules              []RuleConfig `json:"rules"`
	StartedAt          time.Time    `json:"started_at"`
	MaxDiskBytes       int64        `json:"max_disk_bytes"`
	MaxBufferBytes     int64        `json:"max_buffer_bytes"`
	MaxFileBytes       int64        `json:"max_file_bytes"`
}

// snapshot is the content of a stats file: the counters so far, the gauges
// right now, and the files closed since the previous snapshot. The last one
// has status stopped.
type snapshot struct {
	SessionID  string     `json:"session_id"`
	Time       time.Time  `json:"time"`
	Status     string     `json:"status"`
	StopReason string     `json:"stop_reason,omitempty"`
	StopError  string     `json:"stop_error,omitempty"`
	Counters   counters   `json:"counters"`
	Gauges     gauges     `json:"gauges"`
	Files      []fileInfo `json:"files"`
}

type gauges struct {
	// BufferBytes is what's waiting for the writers.
	BufferBytes int64 `json:"buffer_bytes"`
	// DiskBytes is everything in the directory, as max_disk_mib counts it.
	DiskBytes int64 `json:"disk_bytes"`
}

type fileInfo struct {
	Name              string `json:"name"`
	Bytes             int64  `json:"bytes"`
	UncompressedBytes int64  `json:"uncompressed_bytes"`
	Records           int64  `json:"records"`
}

type counters struct {
	RecordsSeen     int64   `json:"records_seen"`
	RecordsSkipped  int64   `json:"records_skipped"`
	RecordsDiskFull int64   `json:"records_disk_full"`
	RecordsKept     int64   `json:"records_kept"`
	RecordsDropped  int64   `json:"records_dropped"`
	RecordsWritten  int64   `json:"records_written"`
	BytesWritten    int64   `json:"bytes_written"`
	BatchesDropped  int64   `json:"batches_dropped"`
	RuleMatches     []int64 `json:"rule_matches"`
}

func newSession(cfg *Config, signal, processor string, rulesCfg []RuleConfig, logger *zap.Logger) *session {
	return &session{
		cfg:            cfg,
		signal:         signal,
		processor:      processor,
		logger:         logger,
		queue:          make(chan batch, queueCapacity),
		maxQueuedBytes: cfg.MaxBufferMiB << 20,
		end:            make(chan string, 1),
		drain:          make(chan struct{}),
		ruleMatches:    make([]atomic.Int64, len(rulesCfg)),
		maxDiskBytes:   cfg.MaxDiskMiB << 20,
		rotateBytes:    cfg.MaxFileMiB << 20,
		header: sessionHeader{
			ConventionsVersion: ConventionsVersion,
			Processor:          processor,
			Signal:             signal,
			Rules:              rulesCfg,
			ConfigHash:         configHash(rulesCfg),
			MaxDiskBytes:       cfg.MaxDiskMiB << 20,
			MaxBufferBytes:     cfg.MaxBufferMiB << 20,
			MaxFileBytes:       cfg.MaxFileMiB << 20,
		},
	}
}

// start creates the session directory, writes session.json, and starts the
// coordinator and the writers. If any of that fails, start returns the error
// and the session never records.
func (s *session) start() error {
	now := time.Now().UTC()
	s.header.Instance, _ = os.Hostname()
	s.header.StartedAt = now.Truncate(time.Millisecond)

	if err := os.MkdirAll(s.cfg.Directory, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	budget, err := budgetFor(s.cfg.Directory)
	if err != nil {
		return fmt.Errorf("measure directory: %w", err)
	}
	s.disk = budget
	if err := s.createDir(now); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := s.writeJSON(headerFile, s.header); err != nil {
		liveSessions.remove(s.dir)
		return fmt.Errorf("write %s: %w", headerFile, err)
	}

	s.recording.Store(true)
	s.logger.Info("Session started", zap.String("session", s.dir))
	for range writerCount {
		s.writers.Add(1)
		w := &writer{s: s}
		go w.run()
	}
	s.stopped.Add(1)
	go s.run()
	return nil
}

// sessionIDAttempts is how many session IDs createDir tries.
const sessionIDAttempts = 10

// createDir picks a session ID and creates its directory. Sessions of the same
// signal can start in the same second, from different taps or from the same
// tap in several pipelines, so the directory is created exclusively and
// another suffix is tried if it already exists.
func (s *session) createDir(now time.Time) error {
	for range sessionIDAttempts {
		id := fmt.Sprintf("%s-%s-%s", now.Format("20060102T150405Z"), s.signal, randomSuffix())
		dir := filepath.Join(s.cfg.Directory, id)
		// Live before it exists, so the server never deletes it while it's
		// empty.
		if !liveSessions.add(dir) {
			continue
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			s.header.SessionID, s.dir = id, dir
			return nil
		}
		liveSessions.remove(dir)
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return errors.New("no unused session ID")
}

// stopAndWait stops recording, writes what's already been kept, closes the
// session, and waits for its goroutines to exit. It's safe to call more than
// once, and without start. Only the first reason counts.
func (s *session) stopAndWait(reason string) {
	select {
	case s.end <- reason:
	default:
	}
	s.stopped.Wait()
}

// admit reports whether a batch of n records is worth copying. When it
// returns true, the caller must call release once it has submitted the
// batch. When the buffer is full, or the disk limit is reached, the records
// are counted as seen and skipped, without matching them.
func (s *session) admit(n int) bool {
	s.inflight.Add(1)
	switch {
	case !s.recording.Load():
	case s.queueFull():
		s.recordsSeen.Add(int64(n))
		s.recordsSkipped.Add(int64(n))
	case s.diskFull():
		s.recordsSeen.Add(int64(n))
		s.recordsDiskFull.Add(int64(n))
	default:
		return true
	}
	s.inflight.Add(-1)
	return false
}

func (s *session) release() { s.inflight.Add(-1) }

// diskFull reports whether the directory has reached max_disk_mib. It clears
// once a client has pulled enough files.
func (s *session) diskFull() bool {
	return s.disk != nil && s.disk.used.Load() >= s.maxDiskBytes
}

// submit hands a marshaled batch to the writers without waiting. When the
// queue is full, the batch is dropped and counted.
func (s *session) submit(b batch) {
	size := int64(len(b.data))
	if s.queuedBytes.Add(size) > s.maxQueuedBytes {
		s.queuedBytes.Add(-size)
		s.drop(b)
		return
	}
	select {
	case s.queue <- b:
	default:
		s.queuedBytes.Add(-size)
		s.drop(b)
	}
}

func (s *session) drop(b batch) {
	s.batchesDropped.Add(1)
	s.recordsDropped.Add(b.records)
}

// queueFull reports whether a batch would be dropped right now, so the caller
// can skip copying it.
func (s *session) queueFull() bool {
	return len(s.queue) == cap(s.queue) || s.queuedBytes.Load() >= s.maxQueuedBytes
}

// take removes a batch from the queue's byte count once a writer has it.
func (s *session) take(b batch) batch {
	s.queuedBytes.Add(-int64(len(b.data)))
	return b
}

// run is the coordinator. It writes a stats snapshot every statsInterval,
// and stops the session.
func (s *session) run() {
	defer s.stopped.Done()
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.writeSnapshot(statusRunning, "")
		case reason := <-s.end:
			// Batches that were admitted before recording stopped are still
			// written. None can be admitted after.
			s.recording.Store(false)
			for s.inflight.Load() > 0 {
				time.Sleep(time.Millisecond)
			}
			close(s.drain)
			s.writers.Wait()
			s.closed.Store(true)
			// The last snapshot lists every file that's left.
			s.writeSnapshot(statusStopped, reason)
			liveSessions.remove(s.dir)
			s.logStop(reason)
			return
		}
	}
}

// fail stops recording after a write error. The coordinator then stops the
// session.
func (s *session) fail(err error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	s.mu.Unlock()
	s.recording.Store(false)
	s.closed.Store(true)
	select {
	case s.end <- stopReasonWriteErr:
	default:
	}
}

func (s *session) logStop(reason string) {
	s.mu.Lock()
	cause := s.failure
	s.mu.Unlock()
	fields := []zap.Field{zap.String("session", s.dir), zap.String("reason", reason)}
	if cause != nil {
		s.logger.Error("Session stopped", append(fields, zap.Error(cause))...)
	} else {
		s.logger.Info("Session stopped", fields...)
	}
}

// run writes batches until the session drains. It then writes what's still
// queued, and closes its file.
func (w *writer) run() {
	defer w.s.writers.Done()
	rotateTicker := time.NewTicker(rotateInterval / 4)
	defer rotateTicker.Stop()

	for {
		select {
		case b := <-w.s.queue:
			w.write(w.s.take(b))
		case <-rotateTicker.C:
			if w.file != nil && time.Since(w.file.opened) >= rotateInterval {
				w.rotate()
			}
		case <-w.s.drain:
			for {
				select {
				case b := <-w.s.queue:
					w.write(w.s.take(b))
				default:
					if err := w.closeFile(); err != nil {
						w.s.fail(err)
					}
					return
				}
			}
		}
	}
}

// write appends a batch to the writer's file. The disk limit is checked with
// the compressed size, which is only known after writing, so the directory
// can go over max_disk_mib by about one batch per writer. At the limit, the
// file is closed so a client can pull it and free the space.
func (w *writer) write(b batch) {
	s := w.s
	if s.closed.Load() {
		s.drop(b)
		w.rotate()
		return
	}
	if s.diskFull() {
		s.drop(b)
		w.rotate()
		return
	}
	if w.file == nil {
		if err := w.openFile(); err != nil {
			s.drop(b)
			s.fail(err)
			return
		}
	}
	// Protobuf reads appended messages back as one message, so batches need no
	// framing between them.
	before := w.file.disk.n
	_, err := w.file.enc.Write(b.data)
	used := s.disk.add(w.file.disk.n - before)
	if err != nil {
		s.drop(b)
		s.fail(err)
		return
	}
	w.file.raw += int64(len(b.data))
	w.file.records += b.records
	s.recordsWritten.Add(b.records)

	if used >= s.maxDiskBytes || w.file.raw >= s.rotateBytes {
		w.rotate()
	}
}

// rotate closes the writer's file, if it has one, so a client can pull it.
// The next batch opens a new one.
func (w *writer) rotate() {
	if err := w.closeFile(); err != nil {
		w.s.fail(err)
	}
}

func (w *writer) openFile() error {
	s := w.s
	s.mu.Lock()
	s.fileCount++
	name := fmt.Sprintf("%s-%06d.pb.zst", s.signal, s.fileCount)
	s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, name+".tmp"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	disk := &countingWriter{w: f}
	// One encoder goroutine and low memory mode keep each writer small.
	enc, err := zstd.NewWriter(disk, zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true))
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = &openFile{name: name, opened: time.Now(), f: f, enc: enc, disk: disk}
	return nil
}

// closeFile finishes the writer's file and renames it, so readers never see a
// file that's still being written.
func (w *writer) closeFile() error {
	if w.file == nil {
		return nil
	}
	s := w.s
	file := w.file
	w.file = nil
	before := file.disk.n
	err := file.enc.Close()
	s.disk.add(file.disk.n - before)
	if closeErr := file.f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(s.dir, file.name+".tmp"), filepath.Join(s.dir, file.name)); err != nil {
		return err
	}
	s.bytesWritten.Add(file.disk.n)
	s.mu.Lock()
	s.newFiles = append(s.newFiles, fileInfo{
		Name: file.name, Bytes: file.disk.n, UncompressedBytes: file.raw, Records: file.records,
	})
	s.mu.Unlock()
	return nil
}

// writeSnapshot writes the next stats file. A snapshot that can't be
// written is logged, and its files are listed in the next one.
func (s *session) writeSnapshot(status, reason string) {
	s.mu.Lock()
	files := s.newFiles
	s.newFiles = nil
	cause := s.failure
	s.mu.Unlock()

	snap := snapshot{
		SessionID:  s.header.SessionID,
		Time:       time.Now().UTC().Truncate(time.Millisecond),
		Status:     status,
		StopReason: reason,
		Counters: counters{
			RecordsSeen:     s.recordsSeen.Load(),
			RecordsSkipped:  s.recordsSkipped.Load(),
			RecordsDiskFull: s.recordsDiskFull.Load(),
			RecordsKept:     s.recordsKept.Load(),
			RecordsDropped:  s.recordsDropped.Load(),
			RecordsWritten:  s.recordsWritten.Load(),
			BytesWritten:    s.bytesWritten.Load(),
			BatchesDropped:  s.batchesDropped.Load(),
			RuleMatches:     make([]int64, len(s.ruleMatches)),
		},
		Gauges: gauges{BufferBytes: s.queuedBytes.Load(), DiskBytes: s.disk.used.Load()},
		Files:  files,
	}
	for i := range s.ruleMatches {
		snap.Counters.RuleMatches[i] = s.ruleMatches[i].Load()
	}
	if snap.Files == nil {
		snap.Files = []fileInfo{}
	}
	if cause != nil {
		snap.StopError = cause.Error()
	}
	s.snapshots++
	if err := s.writeJSON(fmt.Sprintf("stats-%06d.json", s.snapshots), snap); err != nil {
		s.snapshots--
		s.logger.Warn("Couldn't write a stats snapshot", zap.String("session", s.dir), zap.Error(err))
		s.mu.Lock()
		s.newFiles = append(files, s.newFiles...)
		s.mu.Unlock()
	}
}

// writeJSON writes a file in the session directory. It writes a temporary
// file and renames it, so a reader never sees half a file.
func (s *session) writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	s.disk.add(int64(len(data)))
	return nil
}

// liveSessions holds the directories of the sessions this process is writing.
// The server deletes a session's directory once it's empty and not live.
var liveSessions = dirSet{dirs: map[string]bool{}}

type dirSet struct {
	mu   sync.Mutex
	dirs map[string]bool
}

// add adds a directory, and reports whether it wasn't there already.
func (d *dirSet) add(dir string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirs[dir] {
		return false
	}
	d.dirs[dir] = true
	return true
}

func (d *dirSet) remove(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.dirs, dir)
}

func (d *dirSet) has(dir string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirs[dir]
}

func directorySize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func configHash(rulesCfg []RuleConfig) string {
	data, _ := json.Marshal(rulesCfg)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// randomSuffix tells apart sessions that start in the same second. A
// variable so tests can make suffixes collide.
var randomSuffix = func() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
