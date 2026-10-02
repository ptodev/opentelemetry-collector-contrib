// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor/internal/metadata"
)

// setVar sets a variable for the rest of a test.
func setVar[T any](t *testing.T, v *T, value T) {
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

// testConfig returns a config for a test. Sessions get one writer, so each
// has one file, and the disk limit is exact to within one batch. Tests that
// want more set writerCount after calling it.
func testConfig(t *testing.T) *Config {
	setVar(t, &writerCount, 1)
	cfg := createDefaultConfig().(*Config)
	cfg.Directory = t.TempDir()
	cfg.GRPC.NetAddr.Endpoint = "127.0.0.1:0"
	return cfg
}

func testSettings() processor.Settings {
	set := processortest.NewNopSettings(metadata.Type)
	if os.Getenv("TAP_TEST_LOGS") != "" {
		set.Logger = zap.Must(zap.NewDevelopment())
	}
	return set
}

// startTraces starts a traces tap, by default one that keeps everything at
// full detail, and shuts it down at the end of the test.
func startTraces(t *testing.T, cfg *Config) processor.Traces {
	t.Helper()
	if cfg.Traces.Rules == nil {
		cfg.Traces.Rules = []RuleConfig{{Detail: "full"}}
	}
	p, err := NewFactory().CreateTraces(t.Context(), testSettings(), cfg, consumertest.NewNop())
	require.NoError(t, err)
	require.NoError(t, p.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	return p
}

// testServer returns the server for cfg's endpoint, which the taps start.
func testServer(t *testing.T, cfg *Config) *server {
	t.Helper()
	servers.Lock()
	defer servers.Unlock()
	s := servers.byEndpoint[cfg.GRPC.NetAddr.Endpoint]
	require.NotNil(t, s, "no tap has started a server")
	return s
}

// startRecording starts recording every tap behind cfg's endpoint, as a
// client would, and returns the first error.
func startRecording(t *testing.T, cfg *Config) error {
	t.Helper()
	s := testServer(t, cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record, s.filter = true, nil
	var first error
	for _, tp := range s.taps {
		if err := tp.startRecording(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// newTestTracesTap returns a traces tap whose session records, but has no
// writers, so nothing leaves its queue.
func newTestTracesTap(t *testing.T, cfg *Config) *tracesTap {
	t.Helper()
	r, err := newSpanRules(cfg.Traces.Rules, componenttest.NewNopTelemetrySettings())
	require.NoError(t, err)
	tp := &tracesTap{tap: newTap(cfg, "traces", cfg.Traces.Rules, testSettings()), rules: r}
	s := newSession(cfg, "traces", tp.id, cfg.Traces.Rules, zap.NewNop())
	s.recording.Store(true)
	tp.current.Store(s)
	return tp
}

func testTraces() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	ss := rs.ScopeSpans().AppendEmpty()
	for _, name := range []string{"keep-me", "skip-me"} {
		s := ss.Spans().AppendEmpty()
		s.SetName(name)
		s.SetTraceID(traceID(7))
		s.Attributes().PutStr("cart.items", "0123456789")
	}
	return td
}

// randomTraces returns a batch of two spans with about n bytes that don't
// compress.
func randomTraces(n int) ptrace.Traces {
	td := testTraces()
	td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes().PutStr("payload", randomString(n))
	return td
}

func randomLogs(n int) plog.Logs {
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr(randomString(n))
	return ld
}

func randomString(n int) string {
	random := make([]byte, n*3/4)
	_, _ = rand.Read(random)
	return base64.StdEncoding.EncodeToString(random)
}

func decompress(t *testing.T, data []byte) []byte {
	t.Helper()
	dec, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer dec.Close()
	out, err := dec.DecodeAll(data, nil)
	require.NoError(t, err)
	return out
}

// recorded is one session, as a tap wrote it or as a client pulled it.
type recorded struct {
	header    sessionHeader
	snapshots []snapshot
	// data holds every listed file, decompressed, in the order listed.
	data [][]byte
}

func (r recorded) last() snapshot { return r.snapshots[len(r.snapshots)-1] }

// readSessions reads every session directory in dir, except those in skip,
// and checks what every stopped session must have: session.json, stats
// snapshots whose counters never go down, the last one stopped, every
// listed file exactly once and as big as listed, no data file that isn't
// listed, and every kept record written or dropped.
func readSessions(t *testing.T, dir string, skip ...string) []recorded {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []recorded
	for _, e := range entries {
		if !e.IsDir() || slices.Contains(skip, e.Name()) {
			continue
		}
		sessionDir := filepath.Join(dir, e.Name())
		var r recorded
		readJSON(t, filepath.Join(sessionDir, headerFile), &r.header)
		assert.Equal(t, e.Name(), r.header.SessionID)

		stats, err := filepath.Glob(filepath.Join(sessionDir, "stats-*.json"))
		require.NoError(t, err)
		require.NotEmpty(t, stats, "%s has no stats snapshots", e.Name())
		listed := map[string]bool{}
		var records int64
		for _, path := range stats {
			var snap snapshot
			readJSON(t, path, &snap)
			if n := len(r.snapshots); n > 0 {
				prev := r.snapshots[n-1].Counters
				assert.GreaterOrEqual(t, snap.Counters.RecordsSeen, prev.RecordsSeen, "counters never go down")
				assert.GreaterOrEqual(t, snap.Counters.RecordsWritten, prev.RecordsWritten, "counters never go down")
				assert.Equal(t, statusRunning, r.snapshots[n-1].Status, "only the last snapshot is stopped")
			}
			r.snapshots = append(r.snapshots, snap)
			for _, f := range snap.Files {
				assert.False(t, listed[f.Name], "%s is listed once", f.Name)
				listed[f.Name] = true
				compressed, readErr := os.ReadFile(filepath.Join(sessionDir, f.Name))
				require.NoError(t, readErr, "every listed file is there")
				assert.EqualValues(t, len(compressed), f.Bytes)
				data := decompress(t, compressed)
				assert.EqualValues(t, len(data), f.UncompressedBytes)
				r.data = append(r.data, data)
				records += f.Records
			}
		}
		data, err := filepath.Glob(filepath.Join(sessionDir, "*.pb.zst"))
		require.NoError(t, err)
		assert.Len(t, data, len(listed), "every data file is listed")

		last := r.last()
		assert.Equal(t, statusStopped, last.Status)
		c := last.Counters
		assert.Equal(t, c.RecordsKept, c.RecordsWritten+c.RecordsDropped, "every kept record is written or dropped")
		assert.Equal(t, c.RecordsWritten, records, "the listed files hold every written record")
		assert.Equal(t, int64(len(r.header.Rules)), int64(len(c.RuleMatches)))
		out = append(out, r)
	}
	return out
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v), path)
}

func TestPassesDataOnWithoutRecording(t *testing.T) {
	tests := map[string]struct {
		rules []RuleConfig
		// configure changes the config, and reports whether starting to
		// record fails.
		configure func(t *testing.T, cfg *Config) (recordFails bool)
		noStart   bool
	}{
		"no client": {rules: []RuleConfig{{Detail: "full"}}},
		"no rules":  {},
		"directory can't be created": {
			rules: []RuleConfig{{Detail: "full"}},
			configure: func(t *testing.T, cfg *Config) bool {
				// A file where the directory should be.
				blocked := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(blocked, nil, 0o600))
				cfg.Directory = filepath.Join(blocked, "tap")
				return true
			},
		},
		"never started": {rules: []RuleConfig{{Detail: "full"}}, noStart: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Traces.Rules = tt.rules
			recordFails := false
			if tt.configure != nil {
				recordFails = tt.configure(t, cfg)
			}
			sink := &consumertest.TracesSink{}
			p, err := NewFactory().CreateTraces(t.Context(), testSettings(), cfg, sink)
			require.NoError(t, err)
			if !tt.noStart {
				require.NoError(t, p.Start(t.Context(), componenttest.NewNopHost()), "the collector keeps running")
			}
			if recordFails {
				require.Error(t, startRecording(t, cfg), "the client learns that recording failed")
			}

			want := testTraces()
			require.NoError(t, p.ConsumeTraces(t.Context(), testTraces()))
			require.NoError(t, p.Shutdown(t.Context()))
			require.NoError(t, p.Shutdown(t.Context()), "shutting down twice is fine")

			require.Len(t, sink.AllTraces(), 1)
			assert.Equal(t, want, sink.AllTraces()[0], "the batch passes on unchanged")
			entries, _ := os.ReadDir(cfg.Directory)
			assert.Empty(t, entries, "nothing is recorded")
		})
	}
}

// TestBufferLimits fills a session's buffer. No writer runs, so nothing
// leaves it.
func TestBufferLimits(t *testing.T) {
	tests := map[string]struct {
		maxBufferMiB int64
		batches      int
		batch        func() ptrace.Traces
	}{
		"queue capacity": {64, queueCapacity + 6, testTraces},
		"max_buffer_mib": {1, 10, func() ptrace.Traces { return randomTraces(300 << 10) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.MaxBufferMiB = tt.maxBufferMiB
			cfg.Traces.Rules = []RuleConfig{{Detail: "full"}}
			tp := newTestTracesTap(t, cfg)

			for range tt.batches {
				out, err := tp.process(t.Context(), tt.batch())
				require.NoError(t, err)
				assert.Equal(t, 2, out.SpanCount(), "the batch passes on")
			}

			s := tp.current.Load()
			seen, kept, skipped, dropped := s.recordsSeen.Load(), s.recordsKept.Load(), s.recordsSkipped.Load(), s.recordsDropped.Load()
			assert.EqualValues(t, 2*tt.batches, seen)
			assert.Equal(t, seen, kept+skipped, "every record is kept or skipped")
			assert.EqualValues(t, kept, 2*len(s.queue)+int(dropped), "every kept record is queued or dropped")
			assert.Positive(t, skipped+dropped, "a full buffer loses records")
			assert.LessOrEqual(t, s.queuedBytes.Load(), s.maxQueuedBytes)
		})
	}
}

func TestDiskLimit(t *testing.T) {
	const crashed = "20260101T000000Z-traces-dead"
	tests := map[string]struct {
		logs       bool
		batches    int
		batchBytes int
		// leftover is the size of a .tmp file that a crashed session left.
		leftover int
	}{
		"one signal": {batches: 3, batchBytes: 600 << 10},
		// Each signal alone stays under the limit. Together they go over it.
		"shared by signals": {logs: true, batches: 3, batchBytes: 300 << 10},
		// The session alone stays under the limit. With what the crash left,
		// it goes over it.
		"a crashed session's leftovers count": {batches: 2, batchBytes: 500 << 10, leftover: 700 << 10},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.MaxDiskMiB = 1
			cfg.Traces.Rules = []RuleConfig{{Detail: "full"}}
			if tt.logs {
				cfg.Logs.Rules = []RuleConfig{{Detail: "full"}}
			}
			if tt.leftover > 0 {
				dir := filepath.Join(cfg.Directory, crashed)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "traces-000001.pb.zst.tmp"), make([]byte, tt.leftover), 0o600))
			}
			traces, err := NewFactory().CreateTraces(t.Context(), testSettings(), cfg, consumertest.NewNop())
			require.NoError(t, err)
			logs, err := NewFactory().CreateLogs(t.Context(), testSettings(), cfg, consumertest.NewNop())
			require.NoError(t, err)
			for _, c := range []component.Component{traces, logs} {
				require.NoError(t, c.Start(t.Context(), componenttest.NewNopHost()))
			}
			require.NoError(t, startRecording(t, cfg))
			for range tt.batches {
				// Fresh random data in every batch, so zstd can't compress it.
				require.NoError(t, traces.ConsumeTraces(t.Context(), randomTraces(tt.batchBytes)))
				if tt.logs {
					require.NoError(t, logs.ConsumeLogs(t.Context(), randomLogs(tt.batchBytes)))
				}
			}
			for _, c := range []component.Component{traces, logs} {
				require.NoError(t, c.Shutdown(t.Context()))
			}

			total, err := directorySize(cfg.Directory)
			require.NoError(t, err)
			// Each session has one writer, and writers check the limit
			// before they know a batch's compressed size.
			writers := 1
			if tt.logs {
				writers = 2
			}
			assert.Less(t, total, int64(1<<20+writers*tt.batchBytes+64<<10), "at most about one batch per writer over the limit")
			var notWritten int64
			for _, r := range readSessions(t, cfg.Directory, crashed) {
				assert.Equal(t, stopReasonShutdown, r.last().StopReason, "the session pauses at the limit, it doesn't stop")
				notWritten += r.last().Counters.RecordsDiskFull + r.last().Counters.RecordsDropped
			}
			assert.Positive(t, notWritten, "records past the limit aren't written")
			leftovers, err := filepath.Glob(filepath.Join(cfg.Directory, "*", "*.tmp"))
			require.NoError(t, err)
			wantLeftovers := 0
			if tt.leftover > 0 {
				wantLeftovers = 1
			}
			assert.Len(t, leftovers, wantLeftovers, "only the crash leaves a .tmp file")
		})
	}
}

// burstTraces returns a batch of n small spans, like one export request from
// a busy SDK.
func burstTraces(n int) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "query-scheduler")
	ss := rs.ScopeSpans().AppendEmpty()
	for i := range n {
		s := ss.Spans().AppendEmpty()
		s.SetName(fmt.Sprintf("span-%d", i%17))
		s.SetTraceID(traceID(uint64(i)))
		s.Attributes().PutStr("query", strings.Repeat("x", 100+i%50))
	}
	return td
}

// TestBurstWithSeveralWriters sends one burst from many goroutines at once,
// as a collector's receivers do for one huge trace. With a buffer big enough
// for the burst, nothing is lost, and every writer's files add up.
func TestBurstWithSeveralWriters(t *testing.T) {
	cfg := testConfig(t)
	setVar(t, &writerCount, 4)
	cfg.MaxBufferMiB = 256
	cfg.Traces.Rules = []RuleConfig{{Detail: "keys"}}
	p := startTraces(t, cfg)
	require.NoError(t, startRecording(t, cfg))

	const goroutines, batches, spans = 64, 20, 250
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range batches {
				assert.NoError(t, p.ConsumeTraces(t.Context(), burstTraces(spans)))
			}
		})
	}
	wg.Wait()
	require.NoError(t, p.Shutdown(t.Context()))

	sessions := readSessions(t, cfg.Directory)
	require.Len(t, sessions, 1)
	c := sessions[0].last().Counters
	const total = goroutines * batches * spans
	assert.EqualValues(t, total, c.RecordsSeen)
	assert.Zero(t, c.RecordsSkipped, "the buffer held the burst")
	assert.Zero(t, c.RecordsDropped)
	assert.EqualValues(t, total, c.RecordsWritten)
	assert.GreaterOrEqual(t, len(sessions[0].data), writerCount, "each writer wrote its own file")
	assert.Equal(t, total, spanCount(t, sessions[0].data))
}

// spanCount parses each file as one OTLP request, and counts the spans.
func spanCount(t *testing.T, files [][]byte) int {
	t.Helper()
	var n int
	for _, data := range files {
		td, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(data)
		require.NoError(t, err, "every file is one valid OTLP request")
		n += td.SpanCount()
	}
	return n
}

// TestSessionIDsDontCollide starts sessions of the same signal in the same
// second, with suffixes that collide with a live session and with a leftover
// directory. Each session gets a directory of its own.
func TestSessionIDsDontCollide(t *testing.T) {
	suffixes := []string{"aaaa", "aaaa", "bbbb", "cccc"}
	setVar(t, &randomSuffix, func() string {
		s := suffixes[0]
		suffixes = suffixes[1:]
		return s
	})
	cfg := testConfig(t)
	cfg.Traces.Rules = []RuleConfig{{Detail: "full"}}
	now := time.Now().UTC()
	leftover := filepath.Join(cfg.Directory, fmt.Sprintf("%s-traces-bbbb", now.Format("20060102T150405Z")))
	require.NoError(t, os.MkdirAll(leftover, 0o700))

	first := newSession(cfg, "traces", "remotetap/a", cfg.Traces.Rules, zap.NewNop())
	require.NoError(t, first.createDir(now))
	second := newSession(cfg, "traces", "remotetap/b", cfg.Traces.Rules, zap.NewNop())
	require.NoError(t, second.createDir(now))
	t.Cleanup(func() {
		liveSessions.remove(first.dir)
		liveSessions.remove(second.dir)
	})

	assert.True(t, isSessionID(filepath.Base(first.dir)), "the server recognizes the ID")
	assert.True(t, strings.HasSuffix(first.dir, "-aaaa"))
	assert.True(t, strings.HasSuffix(second.dir, "-cccc"), "skips the live session's and the leftover's IDs")
	assert.True(t, liveSessions.has(first.dir), "the first session is still live")
	assert.False(t, liveSessions.has(leftover), "the leftover isn't marked live")
}
