// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor/internal/tappb"
)

// These tests drive the Pull protocol by hand, the way a client would, so
// each one controls what's received, acknowledged, and resumed.

// recording reports whether the server's taps are recording.
func recording(s *server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.taps {
		if cur := t.current.Load(); cur != nil && cur.recording.Load() {
			return true
		}
	}
	return false
}

// assertEmpty checks that the collector deleted every file and session the
// client pulled.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Empty(t, names, "the collector deleted everything the client pulled")
}

// testClient is one Pull stream.
type testClient struct {
	t       *testing.T
	stream  tappb.Tap_PullClient
	cancel  context.CancelFunc
	started *tappb.Started
	sendMu  sync.Mutex

	// files holds what's been received of each file, by session and name.
	files map[[2]string][]byte
	// order is the order in which files started arriving.
	order [][2]string
}

// dial opens a stream, sends Start, and waits for Started.
func dial(t *testing.T, address, clientID string, resume ...*tappb.Resume) (*testClient, error) {
	t.Helper()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := tappb.NewTapClient(conn).Pull(ctx)
	require.NoError(t, err)
	c := &testClient{t: t, stream: stream, cancel: cancel, files: map[[2]string][]byte{}}
	c.send(&tappb.PullRequest{Msg: &tappb.PullRequest_Start{Start: &tappb.Start{ClientId: clientID, Resume: resume}}})
	msg, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	c.started = msg.GetStarted()
	require.NotNil(t, c.started, "the first message is Started")
	return c, nil
}

func (c *testClient) send(m *tappb.PullRequest) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	require.NoError(c.t, c.stream.Send(m))
}

func (c *testClient) stop() {
	c.send(&tappb.PullRequest{Msg: &tappb.PullRequest_Stop{Stop: &tappb.Stop{}}})
}

func (c *testClient) ack(session, file, sum string) {
	c.send(&tappb.PullRequest{Msg: &tappb.PullRequest_Ack{Ack: &tappb.Ack{Session: session, File: file, Sha256: sum}}})
}

// chunk receives the next file chunk, and stores it. It returns nil after
// Done.
func (c *testClient) chunk() *tappb.FileChunk {
	c.t.Helper()
	msg, err := c.stream.Recv()
	require.NoError(c.t, err)
	if msg.GetDone() != nil {
		return nil
	}
	ch := msg.GetChunk()
	require.NotNil(c.t, ch, "only chunks between Started and Done")
	key := [2]string{ch.Session, ch.File}
	if _, ok := c.files[key]; !ok {
		c.order = append(c.order, key)
	}
	data := c.files[key]
	require.LessOrEqual(c.t, int(ch.Offset), len(data), "chunks continue where the file is")
	c.files[key] = append(data[:ch.Offset], ch.Data...)
	return ch
}

// complete checks a file's last chunk against what was received.
func (c *testClient) complete(ch *tappb.FileChunk) {
	c.t.Helper()
	data := c.files[[2]string{ch.Session, ch.File}]
	sum := sha256.Sum256(data)
	require.Len(c.t, data, int(ch.Size))
	require.Equal(c.t, ch.Sha256, hex.EncodeToString(sum[:]))
}

// pull receives and acknowledges every file until Done.
func (c *testClient) pull() {
	c.t.Helper()
	for ch := c.chunk(); ch != nil; ch = c.chunk() {
		if ch.Last {
			c.complete(ch)
			c.ack(ch.Session, ch.File, ch.Sha256)
		}
	}
}

// write stores every received file in dir, in the layout readSessions reads.
func (c *testClient) write(dir string) {
	c.t.Helper()
	for key, data := range c.files {
		require.NoError(c.t, os.MkdirAll(filepath.Join(dir, key[0]), 0o700))
		require.NoError(c.t, os.WriteFile(filepath.Join(dir, key[0], key[1]), data, 0o600))
	}
}

// TestPull records while a client is connected, and pulls everything.
func TestPull(t *testing.T) {
	setVar(t, &rotateInterval, 20*time.Millisecond)
	setVar(t, &statsInterval, 50*time.Millisecond)
	cfg := testConfig(t)
	p := startTraces(t, cfg)
	srv := testServer(t, cfg)

	require.NoError(t, p.ConsumeTraces(t.Context(), testTraces()))
	assert.False(t, recording(srv), "nothing is recorded without a client")
	assertEmpty(t, cfg.Directory)

	c, err := dial(t, srv.addr.String(), "client")
	require.NoError(t, err)
	assert.Equal(t, []string{"remotetap"}, c.started.Taps)
	assert.True(t, recording(srv), "recording starts before Started")
	const batches = 10
	for range batches {
		require.NoError(t, p.ConsumeTraces(t.Context(), randomTraces(100<<10)))
		time.Sleep(20 * time.Millisecond)
	}
	c.stop()
	c.pull()

	assert.False(t, recording(srv), "Stop ends recording")
	require.NoError(t, p.ConsumeTraces(t.Context(), testTraces()))
	assertEmpty(t, cfg.Directory)

	// session.json comes first, and every snapshot after the data files it
	// lists.
	assert.Equal(t, headerFile, c.order[0][1])
	arrived := map[string]bool{}
	for _, key := range c.order {
		arrived[key[1]] = true
		if fileOrder(key[1]) != 3 {
			continue
		}
		var snap snapshot
		require.NoError(t, json.Unmarshal(c.files[key], &snap))
		for _, f := range snap.Files {
			assert.True(t, arrived[f.Name], "%s arrives before %s, which lists it", f.Name, key[1])
		}
	}

	pulled := t.TempDir()
	c.write(pulled)
	sessions := readSessions(t, pulled)
	require.Len(t, sessions, 1)
	r := sessions[0]
	assert.Equal(t, 2*batches, spanCount(t, r.data), "every span sent while the client was connected")
	assert.Greater(t, len(r.data), 1, "several files were rotated")
	assert.Greater(t, len(r.snapshots), 2, "stats were written while recording, not only at the end")
	assert.Equal(t, stopReasonClientStop, r.last().StopReason)
}

// TestResume drops streams partway through a file, and after a file whose
// acknowledgement was lost, and acknowledges a file with the wrong checksum.
// The same client resumes each time, and the collector sends only what's
// missing.
func TestResume(t *testing.T) {
	setVar(t, &rotateInterval, 20*time.Millisecond)
	cfg := testConfig(t)
	p := startTraces(t, cfg)
	srv := testServer(t, cfg)
	address := srv.addr.String()

	c, err := dial(t, address, "client")
	require.NoError(t, err)
	require.NoError(t, p.ConsumeTraces(t.Context(), randomTraces(300<<10)))

	// The first chunk of the data file, then the connection drops.
	var first *tappb.FileChunk
	for first == nil {
		ch := c.chunk()
		switch {
		case ch.File == headerFile:
			if ch.Last {
				c.ack(ch.Session, ch.File, ch.Sha256)
			}
		case fileOrder(ch.File) == 2:
			require.False(t, ch.Last, "the file takes several chunks")
			first = ch
		}
	}
	c.cancel()
	key := [2]string{first.Session, first.File}
	got := c.files[key]

	// The same client resumes from what it has.
	c2, err := dial(t, address, "client", &tappb.Resume{Session: first.Session, File: first.File, Offset: int64(len(got))})
	require.NoError(t, err)
	// A client keeps what it has across streams.
	c2.files = c.files
	var last *tappb.FileChunk
	for resumed := false; last == nil; {
		ch := c2.chunk()
		if ch.File != first.File {
			continue
		}
		if !resumed {
			assert.EqualValues(t, len(got), ch.Offset, "the collector sends only the rest")
			resumed = true
		}
		if ch.Last {
			last = ch
		}
	}
	c2.complete(last)
	// The acknowledgement is lost with the connection.
	c2.cancel()
	whole := c2.files[key]

	// The client has the whole file, so the collector only sends its checksum.
	c3, err := dial(t, address, "client", &tappb.Resume{Session: first.Session, File: first.File, Offset: int64(len(whole))})
	require.NoError(t, err)
	c3.files = c2.files
	ch := c3.chunk()
	for ch.File != first.File {
		ch = c3.chunk()
	}
	assert.True(t, ch.Last)
	assert.Empty(t, ch.Data)
	assert.EqualValues(t, len(whole), ch.Offset)

	// A wrong checksum isn't deleted: the collector sends the file again.
	c3.ack(ch.Session, ch.File, "0000")
	ch = c3.chunk()
	for ch.File != first.File {
		ch = c3.chunk()
	}
	assert.Zero(t, ch.Offset, "sent again from the start")
	assert.FileExists(t, filepath.Join(cfg.Directory, first.Session, first.File), "not deleted")

	c3.stop()
	c3.pull()
	pulled := t.TempDir()
	c3.write(pulled)
	sessions := readSessions(t, pulled)
	require.Len(t, sessions, 1)
	assert.Equal(t, 2, spanCount(t, sessions[0].data), "the resumed file is whole")
	assert.Equal(t, stopReasonClientStop, sessions[0].last().StopReason, "recording continued across every takeover")
	assertEmpty(t, cfg.Directory)
}

// TestTakeoverWhenDiskIsFull fills the disk while a client doesn't pull,
// refuses a second client, and lets the first one take over and free the
// space.
func TestTakeoverWhenDiskIsFull(t *testing.T) {
	setVar(t, &rotateInterval, 20*time.Millisecond)
	cfg := testConfig(t)
	cfg.MaxDiskMiB = 1
	p := startTraces(t, cfg)
	srv := testServer(t, cfg)
	address := srv.addr.String()

	// A client that never acknowledges anything, so nothing is deleted.
	stalled, err := dial(t, address, "client")
	require.NoError(t, err)
	for range 6 {
		require.NoError(t, p.ConsumeTraces(t.Context(), randomTraces(300<<10)))
		time.Sleep(50 * time.Millisecond)
	}
	s := srv.taps[0].current.Load()
	require.NotNil(t, s)
	assert.Positive(t, s.recordsDiskFull.Load(), "the tap paused at the limit")

	_, err = dial(t, address, "other")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "another client is refused")

	// The same client comes back and pulls, which frees the space.
	c, err := dial(t, address, "client")
	require.NoError(t, err)
	for {
		if _, err := stalled.stream.Recv(); err != nil {
			assert.Equal(t, codes.Canceled, status.Code(err), "the earlier stream ends")
			break
		}
	}
	pulled := make(chan struct{})
	go func() {
		defer close(pulled)
		c.pull()
	}()
	require.Eventually(t, func() bool { return !s.diskFull() }, 10*time.Second, 10*time.Millisecond)
	kept := s.recordsKept.Load()
	require.NoError(t, p.ConsumeTraces(t.Context(), randomTraces(100<<10)))
	assert.Greater(t, s.recordsKept.Load(), kept, "the tap records again once there's space")
	c.stop()
	<-pulled

	dir := t.TempDir()
	c.write(dir)
	sessions := readSessions(t, dir)
	require.Len(t, sessions, 1, "the takeover didn't start a new session")
	counts := sessions[0].last().Counters
	assert.EqualValues(t, counts.RecordsWritten, spanCount(t, sessions[0].data))
	assert.Equal(t, counts.RecordsSeen, counts.RecordsKept+counts.RecordsDiskFull+counts.RecordsSkipped, "every record was kept or skipped")
	assertEmpty(t, cfg.Directory)
}

func TestRecordingContinuesForTheGracePeriod(t *testing.T) {
	setVar(t, &gracePeriod, 300*time.Millisecond)
	cfg := testConfig(t)
	startTraces(t, cfg)
	srv := testServer(t, cfg)

	c, err := dial(t, srv.addr.String(), "client")
	require.NoError(t, err)
	// The client goes away without Stop.
	c.cancel()
	// Chunks sent before the cancellation may still arrive first.
	for err == nil {
		_, err = c.stream.Recv()
	}
	require.True(t, errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled)

	assert.True(t, recording(srv), "still recording straight after the stream ended")
	require.Eventually(t, func() bool { return !recording(srv) }, 5*time.Second, 10*time.Millisecond)
	stats, err := filepath.Glob(filepath.Join(cfg.Directory, "*", "stats-*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, stats)
	var last snapshot
	readJSON(t, slices.Max(stats), &last)
	assert.Equal(t, stopReasonClientGone, last.StopReason)
}

// TestPullLeavesOtherDirectoriesAlone puts directories that aren't sessions
// in the tap's directory, such as a volume's lost+found. The collector never
// sends or deletes them.
func TestPullLeavesOtherDirectoriesAlone(t *testing.T) {
	cfg := testConfig(t)
	startTraces(t, cfg)
	srv := testServer(t, cfg)
	empty := filepath.Join(cfg.Directory, "lost+found")
	require.NoError(t, os.Mkdir(empty, 0o700))
	other := filepath.Join(cfg.Directory, "notes")
	require.NoError(t, os.Mkdir(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, "stats-000001.json"), []byte("{}"), 0o600))

	c, err := dial(t, srv.addr.String(), "client")
	require.NoError(t, err)
	c.stop()
	c.pull()

	for _, key := range c.order {
		assert.True(t, isSessionID(key[0]), "only sessions are sent, got %s", key[0])
	}
	assert.DirExists(t, empty)
	assert.FileExists(t, filepath.Join(other, "stats-000001.json"))
}
