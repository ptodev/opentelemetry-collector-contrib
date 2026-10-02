// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
)

// Every process function returns its input unchanged and never an error, so
// the tap can't change or break the pipeline it's in. While a client is
// recording, it copies the records its rules keep into a new batch, and hands
// that to its session. Otherwise it only passes data on.

// tap holds what all signals share.
type tap struct {
	cfg    *Config
	signal string
	rules  []RuleConfig
	id     string
	set    processor.Settings
	logger *zap.Logger

	// server is set in start, when the signal has rules. It's nil if the
	// server couldn't start, and the tap then only passes data on.
	server *server
	// mu serializes starting and stopping sessions.
	mu sync.Mutex
	// current is the session being recorded, or nil. The processing
	// goroutines only read it.
	current atomic.Pointer[session]
}

func newTap(cfg *Config, signal string, rulesCfg []RuleConfig, set processor.Settings) tap {
	return tap{cfg: cfg, signal: signal, rules: rulesCfg, id: set.ID.String(), set: set, logger: set.Logger}
}

// start registers the tap with its server, so clients can record it. A
// server that can't start is logged, and the tap then only passes data on,
// because a debugging tool must never stop the collector from running.
func (t *tap) start(ctx context.Context, host component.Host) error {
	if len(t.rules) == 0 {
		return nil
	}
	srv, err := acquireServer(ctx, &t.cfg.GRPC, host, t.set.TelemetrySettings)
	if err != nil {
		t.logger.Error("Can't serve clients; passing data on without recording", zap.String("endpoint", t.cfg.GRPC.NetAddr.Endpoint), zap.Error(err))
		return nil
	}
	t.server = srv
	srv.add(t)
	return nil
}

func (t *tap) shutdown(context.Context) error {
	if t.server != nil {
		t.server.remove(t)
	}
	t.stopRecording(stopReasonShutdown)
	return nil
}

// matches reports whether a client asking for these component IDs wants this
// tap. No IDs means every tap.
func (t *tap) matches(ids []string) bool {
	return len(ids) == 0 || slices.Contains(ids, t.id)
}

// startRecording starts a new session, unless one is already recording.
func (t *tap) startRecording() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.current.Load(); cur != nil {
		if cur.recording.Load() {
			return nil
		}
		// It stopped by itself, after a write error.
		cur.stopAndWait(stopReasonWriteErr)
	}
	s := newSession(t.cfg, t.signal, t.id, t.rules, t.logger)
	if err := s.start(); err != nil {
		t.logger.Error("Can't record", zap.String("directory", t.cfg.Directory), zap.Error(err))
		return err
	}
	t.current.Store(s)
	return nil
}

// stopRecording stops the current session, if any, and waits until its last
// file is closed.
func (t *tap) stopRecording(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.current.Swap(nil); cur != nil {
		cur.stopAndWait(reason)
	}
}

// admit returns the session to copy a batch of n records into, or nil. The
// caller must release a returned session once it has handed the batch over.
func (t *tap) admit(n int) *session {
	s := t.current.Load()
	if s == nil || !s.admit(n) {
		return nil
	}
	return s
}

// hand marshals the kept records and gives them to the session.
func (s *session) hand(kept int64, marshal func() ([]byte, error)) {
	if kept == 0 {
		return
	}
	data, err := marshal()
	if err != nil {
		s.recordsDropped.Add(kept)
		return
	}
	s.submit(batch{data: data, records: kept})
}

// keep counts a kept record and the rules it matched.
func (s *session) keep(matched []int64) {
	s.recordsKept.Add(1)
	for _, i := range matched {
		s.ruleMatches[i].Add(1)
	}
}

// resourceInfo adds the session, the original resource's size, and its
// schema URL to a copied resource. The size is measured on a batch holding
// only the resource.
func (s *session) resourceInfo(res pcommon.Resource, size int, schemaURL string) {
	putSchemaURL(res.Attributes(), schemaURL)
	res.Attributes().PutStr(AttrSessionID, s.header.SessionID)
	res.Attributes().PutStr(AttrInstance, s.header.Instance)
	res.Attributes().PutStr(AttrProcessor, s.processor)
	res.Attributes().PutInt(AttrResourceBytes, int64(size))
}

// putSchemaURL copies a schema URL into attributes, because OTLP readers such
// as duckdb-otlp don't show schema URLs.
func putSchemaURL(attrs pcommon.Map, schemaURL string) {
	if schemaURL != "" {
		attrs.PutStr(AttrSchemaURL, schemaURL)
	}
}

// lazy creates a value the first time it's needed, so resources and scopes are
// only copied when at least one of their records is kept.
type lazy[T any] struct {
	make func() T
	val  T
	done bool
}

func (l *lazy[T]) get() T {
	if !l.done {
		l.val, l.done = l.make(), true
	}
	return l.val
}

type tracesTap struct {
	tap
	rules     rules[*ottlspan.TransformContext]
	marshaler ptrace.ProtoMarshaler
}

func (t *tracesTap) process(ctx context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	s := t.admit(td.SpanCount())
	if s == nil {
		return td, nil
	}
	defer s.release()
	out := ptrace.NewTraces()
	var seen, kept int64
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		rs := td.ResourceSpans().At(i)
		outRS := lazy[ptrace.ResourceSpans]{make: func() ptrace.ResourceSpans {
			o := out.ResourceSpans().AppendEmpty()
			rs.Resource().CopyTo(o.Resource())
			o.SetSchemaUrl(rs.SchemaUrl())
			res := ptrace.NewResourceSpans()
			rs.Resource().CopyTo(res.Resource())
			s.resourceInfo(o.Resource(), t.marshaler.ResourceSpansSize(res), rs.SchemaUrl())
			return o
		}}
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			outSS := lazy[ptrace.ScopeSpans]{make: func() ptrace.ScopeSpans {
				o := outRS.get().ScopeSpans().AppendEmpty()
				ss.Scope().CopyTo(o.Scope())
				o.SetSchemaUrl(ss.SchemaUrl())
				putSchemaURL(o.Scope().Attributes(), ss.SchemaUrl())
				return o
			}}
			for k := 0; k < ss.Spans().Len(); k++ {
				span := ss.Spans().At(k)
				seen++
				tCtx := ottlspan.NewTransformContext(rs, ss, span)
				d, matched := t.rules.match(ctx, tCtx, span.TraceID())
				tCtx.Close()
				if d == detailNone {
					continue
				}
				dst := outSS.get().Spans().AppendEmpty()
				span.CopyTo(dst)
				summarizeSpan(dst, d, matched, t.marshaler.SpanSize(span))
				s.keep(matched)
				kept++
			}
		}
	}
	s.recordsSeen.Add(seen)
	s.hand(kept, func() ([]byte, error) { return t.marshaler.MarshalTraces(out) })
	return td, nil
}

type logsTap struct {
	tap
	rules     rules[*ottllog.TransformContext]
	marshaler plog.ProtoMarshaler
}

func (t *logsTap) process(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
	s := t.admit(ld.LogRecordCount())
	if s == nil {
		return ld, nil
	}
	defer s.release()
	out := plog.NewLogs()
	var seen, kept int64
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		rl := ld.ResourceLogs().At(i)
		outRL := lazy[plog.ResourceLogs]{make: func() plog.ResourceLogs {
			o := out.ResourceLogs().AppendEmpty()
			rl.Resource().CopyTo(o.Resource())
			o.SetSchemaUrl(rl.SchemaUrl())
			res := plog.NewResourceLogs()
			rl.Resource().CopyTo(res.Resource())
			s.resourceInfo(o.Resource(), t.marshaler.ResourceLogsSize(res), rl.SchemaUrl())
			return o
		}}
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			outSL := lazy[plog.ScopeLogs]{make: func() plog.ScopeLogs {
				o := outRL.get().ScopeLogs().AppendEmpty()
				sl.Scope().CopyTo(o.Scope())
				o.SetSchemaUrl(sl.SchemaUrl())
				putSchemaURL(o.Scope().Attributes(), sl.SchemaUrl())
				return o
			}}
			for k := 0; k < sl.LogRecords().Len(); k++ {
				lr := sl.LogRecords().At(k)
				seen++
				tCtx := ottllog.NewTransformContext(rl, sl, lr)
				d, matched := t.rules.match(ctx, tCtx, lr.TraceID())
				tCtx.Close()
				if d == detailNone {
					continue
				}
				dst := outSL.get().LogRecords().AppendEmpty()
				lr.CopyTo(dst)
				summarizeLog(dst, d, matched, t.marshaler.LogRecordSize(lr))
				s.keep(matched)
				kept++
			}
		}
	}
	s.recordsSeen.Add(seen)
	s.hand(kept, func() ([]byte, error) { return t.marshaler.MarshalLogs(out) })
	return ld, nil
}

type metricsTap struct {
	tap
	rules     rules[*ottldatapoint.TransformContext]
	marshaler pmetric.ProtoMarshaler
}

func (t *metricsTap) process(ctx context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	sess := t.admit(md.DataPointCount())
	if sess == nil {
		return md, nil
	}
	defer sess.release()
	out := pmetric.NewMetrics()
	var seen, kept int64
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		rm := md.ResourceMetrics().At(i)
		outRM := lazy[pmetric.ResourceMetrics]{make: func() pmetric.ResourceMetrics {
			o := out.ResourceMetrics().AppendEmpty()
			rm.Resource().CopyTo(o.Resource())
			o.SetSchemaUrl(rm.SchemaUrl())
			res := pmetric.NewResourceMetrics()
			rm.Resource().CopyTo(res.Resource())
			sess.resourceInfo(o.Resource(), t.marshaler.ResourceMetricsSize(res), rm.SchemaUrl())
			return o
		}}
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			outSM := lazy[pmetric.ScopeMetrics]{make: func() pmetric.ScopeMetrics {
				o := outRM.get().ScopeMetrics().AppendEmpty()
				sm.Scope().CopyTo(o.Scope())
				o.SetSchemaUrl(sm.SchemaUrl())
				putSchemaURL(o.Scope().Attributes(), sm.SchemaUrl())
				return o
			}}
			for k := 0; k < sm.Metrics().Len(); k++ {
				m := sm.Metrics().At(k)
				// newMetric copies the metric without its data points.
				newMetric := func() pmetric.Metric {
					o := outSM.get().Metrics().AppendEmpty()
					o.SetName(m.Name())
					o.SetDescription(m.Description())
					o.SetUnit(m.Unit())
					m.Metadata().CopyTo(o.Metadata())
					return o
				}
				var s, n int64
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					s, n = keepPoints(ctx, t, sess, rm, sm, m, m.Gauge().DataPoints(),
						func() pmetric.NumberDataPointSlice { return newMetric().SetEmptyGauge().DataPoints() },
						t.marshaler.NumberDataPointSize)
				case pmetric.MetricTypeSum:
					s, n = keepPoints(ctx, t, sess, rm, sm, m, m.Sum().DataPoints(),
						func() pmetric.NumberDataPointSlice {
							sum := newMetric().SetEmptySum()
							sum.SetAggregationTemporality(m.Sum().AggregationTemporality())
							sum.SetIsMonotonic(m.Sum().IsMonotonic())
							return sum.DataPoints()
						},
						t.marshaler.NumberDataPointSize)
				case pmetric.MetricTypeHistogram:
					s, n = keepPoints(ctx, t, sess, rm, sm, m, m.Histogram().DataPoints(),
						func() pmetric.HistogramDataPointSlice {
							h := newMetric().SetEmptyHistogram()
							h.SetAggregationTemporality(m.Histogram().AggregationTemporality())
							return h.DataPoints()
						},
						t.marshaler.HistogramDataPointSize)
				case pmetric.MetricTypeExponentialHistogram:
					s, n = keepPoints(ctx, t, sess, rm, sm, m, m.ExponentialHistogram().DataPoints(),
						func() pmetric.ExponentialHistogramDataPointSlice {
							h := newMetric().SetEmptyExponentialHistogram()
							h.SetAggregationTemporality(m.ExponentialHistogram().AggregationTemporality())
							return h.DataPoints()
						},
						t.marshaler.ExponentialHistogramDataPointSize)
				case pmetric.MetricTypeSummary:
					s, n = keepPoints(ctx, t, sess, rm, sm, m, m.Summary().DataPoints(),
						func() pmetric.SummaryDataPointSlice { return newMetric().SetEmptySummary().DataPoints() },
						t.marshaler.SummaryDataPointSize)
				}
				seen += s
				kept += n
			}
		}
	}
	sess.recordsSeen.Add(seen)
	sess.hand(kept, func() ([]byte, error) { return t.marshaler.MarshalMetrics(out) })
	return md, nil
}

// pointSlice is what every data point slice type has in common.
type pointSlice[P any] interface {
	Len() int
	At(i int) P
	AppendEmpty() P
}

// keepPoints copies the kept data points of one metric. newSlice creates the
// copied metric the first time a point is kept.
func keepPoints[P dataPoint[P], S pointSlice[P]](
	ctx context.Context, t *metricsTap, s *session,
	rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric,
	points S, newSlice func() S, size func(P) int,
) (seen, kept int64) {
	dst := lazy[S]{make: newSlice}
	for i := 0; i < points.Len(); i++ {
		p := points.At(i)
		seen++
		tCtx := ottldatapoint.NewTransformContext(rm, sm, m, p)
		d, matched := t.rules.match(ctx, tCtx, pcommon.TraceID{})
		tCtx.Close()
		if d == detailNone {
			continue
		}
		c := dst.get().AppendEmpty()
		p.CopyTo(c)
		summarizePoint(c, d, matched, size(p))
		s.keep(matched)
		kept++
	}
	return seen, kept
}
