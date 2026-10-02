// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor/internal/tappb"
)

// The server lets one client at a time record the taps behind an endpoint,
// and sends it every file they write, once it's complete. Files stay on disk
// until the client acknowledges them, so a dropped connection loses nothing:
// the client reconnects with the same client ID, and the server sends what's
// left.

// gracePeriod is how long taps keep recording after a client's stream ends
// without Stop, so a client that reconnects finds no gap. A variable so tests
// can shorten it.
var gracePeriod = time.Minute

// scanInterval is how often a stream looks for new files.
var scanInterval = time.Second

// chunkSize is how much of a file one message carries. The client can only
// keep whole messages, so a connection must last at least one chunk for a
// file to make progress. Small chunks keep that true on poor links.
const chunkSize = 64 << 10

// servers holds one server per endpoint, shared by every tap in the process
// that uses it.
var servers = struct {
	sync.Mutex
	byEndpoint map[string]*server
}{byEndpoint: map[string]*server{}}

type server struct {
	tappb.UnimplementedTapServer

	endpoint string
	addr     net.Addr
	instance string
	logger   *zap.Logger
	grpc     *grpc.Server
	// refs is guarded by servers.
	refs int

	mu      sync.Mutex
	taps    []*tap
	closed  bool
	stream  *pullStream
	client  string
	filter  []string
	record  bool
	grace   *time.Timer
	graceOf *pullStream
}

// pullStream identifies one open Pull stream.
type pullStream struct {
	cancel context.CancelFunc
}

// acquireServer returns the endpoint's server, and starts it if no tap uses
// it yet.
func acquireServer(ctx context.Context, cfg *configgrpc.ServerConfig, host component.Host, set component.TelemetrySettings) (*server, error) {
	servers.Lock()
	defer servers.Unlock()
	endpoint := cfg.NetAddr.Endpoint
	if s, ok := servers.byEndpoint[endpoint]; ok {
		s.refs++
		return s, nil
	}
	g, err := cfg.ToServer(ctx, host.GetExtensions(), set)
	if err != nil {
		return nil, err
	}
	ln, err := cfg.NetAddr.Listen(ctx)
	if err != nil {
		return nil, err
	}
	instance, _ := os.Hostname()
	s := &server{endpoint: endpoint, addr: ln.Addr(), instance: instance, logger: set.Logger, grpc: g, refs: 1}
	tappb.RegisterTapServer(g, s)
	go func() {
		if err := g.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.logger.Error("Remote tap server stopped", zap.Error(err))
		}
	}()
	servers.byEndpoint[endpoint] = s
	s.logger.Info("Serving remote tap clients", zap.String("address", s.addr.String()))
	return s, nil
}

// add registers a tap. If a client is recording, and wants this tap, it
// starts recording straight away.
func (s *server) add(t *tap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taps = append(s.taps, t)
	if s.record && t.matches(s.filter) {
		_ = t.startRecording()
	}
}

// remove unregisters a tap, and stops the server when it was the last one.
func (s *server) remove(t *tap) {
	s.mu.Lock()
	s.taps = slices.DeleteFunc(s.taps, func(o *tap) bool { return o == t })
	s.mu.Unlock()

	servers.Lock()
	s.refs--
	last := s.refs == 0
	if last {
		delete(servers.byEndpoint, s.endpoint)
	}
	servers.Unlock()
	if !last {
		return
	}
	s.mu.Lock()
	s.closed = true
	if s.grace != nil {
		s.grace.Stop()
	}
	s.mu.Unlock()
	s.grpc.Stop()
}

// attach makes a stream the client's current one. A stream from the same
// client takes over from an earlier one, which is often still open because
// its connection died without anyone noticing yet.
func (s *server) attach(ps *pullStream, start *tappb.Start) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil && s.client != start.ClientId {
		return nil, status.Errorf(codes.FailedPrecondition, "client %q is already pulling from this collector", s.client)
	}
	if s.stream != nil {
		s.stream.cancel()
	}
	if s.grace != nil {
		s.grace.Stop()
		s.grace, s.graceOf = nil, nil
	}
	s.stream, s.client = ps, start.ClientId
	if !start.Drain {
		s.record, s.filter = true, start.Taps
		for _, t := range s.taps {
			if t.matches(s.filter) {
				_ = t.startRecording()
			} else {
				t.stopRecording(stopReasonClientStop)
			}
		}
	}
	var recording []string
	for _, t := range s.taps {
		if cur := t.current.Load(); cur != nil && cur.recording.Load() && !slices.Contains(recording, t.id) {
			recording = append(recording, t.id)
		}
	}
	return recording, nil
}

// detach ends a stream. If the client was recording, recording continues for
// the grace period, in case the client comes back.
func (s *server) detach(ps *pullStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != ps {
		return
	}
	s.stream = nil
	if s.record && !s.closed {
		s.graceOf = ps
		s.grace = time.AfterFunc(gracePeriod, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.stream == nil && s.graceOf == ps {
				s.logger.Info("Client didn't come back; stopping recording", zap.String("client", s.client))
				s.stopTaps(stopReasonClientGone)
			}
		})
	}
}

// stop stops recording on behalf of a stream's client.
func (s *server) stop(ps *pullStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == ps {
		s.stopTaps(stopReasonClientStop)
	}
}

// stopTaps stops every tap's session. s.mu must be held.
func (s *server) stopTaps(reason string) {
	s.record = false
	for _, t := range s.taps {
		t.stopRecording(reason)
	}
}

// directories returns every directory the taps write to.
func (s *server) directories() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dirs []string
	for _, t := range s.taps {
		if !slices.Contains(dirs, t.cfg.Directory) {
			dirs = append(dirs, t.cfg.Directory)
		}
	}
	return dirs
}

// Pull implements tappb.TapServer.
func (s *server) Pull(stream tappb.Tap_PullServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	start := req.GetStart()
	if start == nil || start.ClientId == "" {
		return status.Error(codes.InvalidArgument, "the first message must be Start, with a client_id")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	ps := &pullStream{cancel: cancel}
	taps, err := s.attach(ps, start)
	if err != nil {
		return err
	}
	defer s.detach(ps)
	s.logger.Info("Client connected", zap.String("client", start.ClientId), zap.Strings("recording", taps))

	p := &puller{
		server: s,
		ps:     ps,
		stream: stream,
		resume: map[fileKey]int64{},
		sent:   map[fileKey]sentFile{},
	}
	for _, r := range start.Resume {
		p.resume[fileKey{r.Session, r.File}] = r.Offset
	}
	err = p.run(ctx, taps)
	s.logger.Info("Client disconnected", zap.String("client", start.ClientId), zap.Error(err))
	return err
}

type fileKey struct{ session, file string }

// sentFile is a file sent on this stream, waiting for its acknowledgement.
type sentFile struct {
	path, root, sha256 string
	size               int64
}

// puller serves one stream. Everything in it runs in the stream's goroutine,
// except the goroutine that receives messages.
type puller struct {
	server   *server
	ps       *pullStream
	stream   tappb.Tap_PullServer
	resume   map[fileKey]int64
	sent     map[fileKey]sentFile
	stopping bool
	done     bool
}

func (p *puller) run(ctx context.Context, taps []string) error {
	err := p.stream.Send(&tappb.PullResponse{Msg: &tappb.PullResponse_Started{
		Started: &tappb.Started{Instance: p.server.instance, Taps: taps},
	}})
	if err != nil {
		return err
	}

	msgs := make(chan *tappb.PullRequest, 64)
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := p.stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	// handleReceived handles every message that has already arrived, such as
	// acknowledgements sent just before the connection dropped.
	handleReceived := func() {
		for {
			select {
			case m := <-msgs:
				p.handle(m)
			default:
				return
			}
		}
	}
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		handleReceived()
		if err := p.scan(ctx); err != nil {
			handleReceived()
			return err
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case err := <-recvErr:
			handleReceived()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case m := <-msgs:
			p.handle(m)
		case <-ticker.C:
		}
	}
}

func (p *puller) handle(m *tappb.PullRequest) {
	switch msg := m.Msg.(type) {
	case *tappb.PullRequest_Ack:
		ack := msg.Ack
		key := fileKey{ack.Session, ack.File}
		f, ok := p.sent[key]
		if !ok {
			return
		}
		delete(p.sent, key)
		if ack.Sha256 != f.sha256 {
			// The next scan sends the file again.
			p.server.logger.Warn("Client stored a different file; sending it again", zap.String("file", f.path))
			return
		}
		p.server.logger.Debug("Client acknowledged a file", zap.String("file", f.path))
		if err := os.Remove(f.path); err != nil {
			p.server.logger.Warn("Can't delete a pulled file", zap.String("file", f.path), zap.Error(err))
			return
		}
		if b := existingBudget(f.root); b != nil {
			b.add(-f.size)
		}
	case *tappb.PullRequest_Stop:
		p.stopping = true
		p.server.stop(p.ps)
	}
}

// scan sends every complete file that hasn't been sent on this stream, and
// deletes the directories of sessions that have stopped and been pulled.
// After Stop, once every file has been acknowledged, it sends Done.
func (p *puller) scan(ctx context.Context) error {
	left := 0
	for _, root := range p.server.directories() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !isSessionID(e.Name()) {
				continue
			}
			n, err := p.scanSession(ctx, root, e.Name())
			if err != nil {
				return err
			}
			left += n
		}
	}
	if p.stopping && !p.done && left == 0 {
		p.done = true
		return p.stream.Send(&tappb.PullResponse{Msg: &tappb.PullResponse_Done{Done: &tappb.Done{}}})
	}
	return nil
}

// scanSession sends a session's new files, and returns how many files it has
// that the client hasn't acknowledged. session.json goes first, and stats
// snapshots after the data files, so a client that has a snapshot also has
// every file it lists.
func (p *puller) scanSession(ctx context.Context, root, session string) (int, error) {
	dir := filepath.Join(root, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	if len(entries) == 0 && !liveSessions.has(dir) {
		// Stopped, and every file was pulled. A session left by a crash
		// keeps its .tmp file, so it's never deleted.
		if err := os.Remove(dir); err != nil {
			p.server.logger.Warn("Can't delete a pulled session", zap.String("session", dir), zap.Error(err))
		}
		return 0, nil
	}
	var files []string
	for _, e := range entries {
		if name := e.Name(); fileOrder(name) > 0 {
			files = append(files, name)
		}
	}
	slices.SortFunc(files, func(a, b string) int {
		return cmp.Or(cmp.Compare(fileOrder(a), fileOrder(b)), strings.Compare(a, b))
	})
	for _, name := range files {
		if _, ok := p.sent[fileKey{session, name}]; ok {
			continue
		}
		if err := p.sendFile(ctx, root, session, name); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

// sessionIDPattern matches the session IDs session.createDir picks. Other
// directories in a tap's directory aren't the tap's, so they're never sent
// or deleted.
var sessionIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-(traces|logs|metrics)-[0-9a-f]+$`)

func isSessionID(name string) bool { return sessionIDPattern.MatchString(name) }

// fileOrder is the order in which a session's files are sent, or 0 for a
// file that isn't complete, or isn't the tap's.
func fileOrder(name string) int {
	switch {
	case name == headerFile:
		return 1
	case strings.HasSuffix(name, ".pb.zst"):
		return 2
	case strings.HasPrefix(name, "stats-") && strings.HasSuffix(name, ".json"):
		return 3
	}
	return 0
}

// sendFile sends a complete file in chunks, from where the client said it had
// got to, if it did.
func (p *puller) sendFile(ctx context.Context, root, session, name string) error {
	key := fileKey{session, name}
	path := filepath.Join(root, session, name)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		p.server.logger.Warn("Can't read a closed file", zap.String("file", path), zap.Error(err))
		return nil
	}
	sum := hex.EncodeToString(h.Sum(nil))

	offset := p.resume[key]
	delete(p.resume, key)
	if offset < 0 || offset > size {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		// A new buffer for every message: gRPC may still hold the last one.
		buf := make([]byte, min(chunkSize, size-offset))
		n, err := io.ReadFull(f, buf)
		if err != nil && len(buf) > 0 {
			p.server.logger.Warn("Can't read a closed file", zap.String("file", path), zap.Error(err))
			return nil
		}
		chunk := &tappb.FileChunk{Session: session, File: name, Offset: offset, Data: buf[:n]}
		offset += int64(n)
		if offset == size {
			chunk.Last, chunk.Size, chunk.Sha256 = true, size, sum
		}
		if err := p.stream.Send(&tappb.PullResponse{Msg: &tappb.PullResponse_Chunk{Chunk: chunk}}); err != nil {
			return err
		}
		if chunk.Last {
			break
		}
	}
	p.sent[key] = sentFile{path: path, root: root, sha256: sum, size: size}
	p.server.logger.Debug("Sent a file", zap.String("file", path), zap.Int64("bytes", size))
	return nil
}
