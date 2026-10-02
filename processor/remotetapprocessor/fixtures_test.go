// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"fmt"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// The fixtures are generated from fixed seeds and a fixed start time, so every
// run produces exactly the same telemetry.

// fixtureStart is when the fixture telemetry happens.
var fixtureStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const fixtureBatches = 1

// fixtureSchemaURL is the semantic conventions version of the fixtures.
const fixtureSchemaURL = "https://opentelemetry.io/schemas/1.26.0"

var (
	fixtureServices = []string{"checkout", "cart", "payments"}
	fixtureRoutes   = []string{"/cart", "/cart/checkout", "/product/{id}", "/api/pay"}
	fixtureMethods  = []string{"GET", "POST"}
	fixtureMessages = []string{"order %d placed", "cache miss for cart %d", "payment %d declined"}
)

func fixtureRand(batch int, signal uint64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(batch), signal))
}

func fixtureHex(r *rand.Rand, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[r.IntN(len(digits))]
	}
	return string(b)
}

func fixtureTraceID(r *rand.Rand) pcommon.TraceID {
	var id pcommon.TraceID
	for i := range id {
		id[i] = byte(r.UintN(256))
	}
	return id
}

func fixtureSpanID(r *rand.Rand) pcommon.SpanID {
	var id pcommon.SpanID
	for i := range id {
		id[i] = byte(r.UintN(256))
	}
	return id
}

func fixtureResource(res pcommon.Resource, service string) {
	res.Attributes().PutStr("service.name", service)
	res.Attributes().PutStr("service.version", "1.2.3")
	res.Attributes().PutStr("k8s.namespace.name", "shop")
	res.Attributes().PutStr("k8s.pod.name", service+"-7d9f8c6b5-x2x9z")
}

func ts(t time.Time) pcommon.Timestamp { return pcommon.NewTimestampFromTime(t) }

// fixtureTraces returns batch number batch: two traces per service, each a
// server span with three children, and an exception event on some.
func fixtureTraces(batch int) ptrace.Traces {
	r := fixtureRand(batch, 1)
	td := ptrace.NewTraces()
	base := fixtureStart.Add(time.Duration(batch) * time.Minute)
	for _, service := range fixtureServices {
		rs := td.ResourceSpans().AppendEmpty()
		fixtureResource(rs.Resource(), service)
		rs.SetSchemaUrl(fixtureSchemaURL)
		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName("go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp")
		ss.Scope().SetVersion("0.63.0")
		ss.SetSchemaUrl(fixtureSchemaURL)
		for i := range 2 {
			traceID := fixtureTraceID(r)
			start := base.Add(time.Duration(i) * 10 * time.Second)
			route := fixtureRoutes[r.IntN(len(fixtureRoutes))]
			method := fixtureMethods[r.IntN(len(fixtureMethods))]

			root := ss.Spans().AppendEmpty()
			root.SetTraceID(traceID)
			root.SetSpanID(fixtureSpanID(r))
			root.SetName(method + " " + route)
			root.SetKind(ptrace.SpanKindServer)
			root.SetStartTimestamp(ts(start))
			root.SetEndTimestamp(ts(start.Add(time.Duration(50+r.IntN(400)) * time.Millisecond)))
			root.Attributes().PutStr("http.request.method", method)
			root.Attributes().PutStr("http.route", route)
			root.Attributes().PutInt("http.response.status_code", []int64{200, 200, 404, 500}[r.IntN(4)])
			root.Attributes().PutStr("user.id", fixtureHex(r, 16))
			if r.IntN(3) == 0 {
				root.Status().SetCode(ptrace.StatusCodeError)
				ev := root.Events().AppendEmpty()
				ev.SetName("exception")
				ev.SetTimestamp(ts(start.Add(20 * time.Millisecond)))
				ev.Attributes().PutStr("exception.type", "*errors.errorString")
				ev.Attributes().PutStr("exception.message", "upstream timed out")
			}

			for j := range 3 {
				child := ss.Spans().AppendEmpty()
				child.SetTraceID(traceID)
				child.SetSpanID(fixtureSpanID(r))
				child.SetParentSpanID(root.SpanID())
				child.SetKind(ptrace.SpanKindClient)
				childStart := start.Add(time.Duration(5+j*10) * time.Millisecond)
				child.SetStartTimestamp(ts(childStart))
				child.SetEndTimestamp(ts(childStart.Add(time.Duration(1+r.IntN(9)) * time.Millisecond)))
				if j == 0 {
					child.SetName("SELECT shop.carts")
					child.Attributes().PutStr("db.system.name", "postgresql")
					child.Attributes().PutStr("db.query.text", fmt.Sprintf("SELECT * FROM carts WHERE id = %d", r.IntN(10000)))
				} else {
					child.SetName("HGET cart")
					child.Attributes().PutStr("db.system.name", "redis")
				}
			}
		}
	}
	return td
}

// fixtureLogs returns batch number batch: four log records per service.
func fixtureLogs(batch int) plog.Logs {
	r := fixtureRand(batch, 2)
	ld := plog.NewLogs()
	base := fixtureStart.Add(time.Duration(batch) * time.Minute)
	for _, service := range fixtureServices {
		rl := ld.ResourceLogs().AppendEmpty()
		fixtureResource(rl.Resource(), service)
		rl.SetSchemaUrl(fixtureSchemaURL)
		sl := rl.ScopeLogs().AppendEmpty()
		sl.Scope().SetName("log/slog")
		for i := range 4 {
			lr := sl.LogRecords().AppendEmpty()
			at := base.Add(time.Duration(i) * 7 * time.Second)
			lr.SetTimestamp(ts(at))
			lr.SetObservedTimestamp(ts(at.Add(time.Millisecond)))
			severity := []plog.SeverityNumber{plog.SeverityNumberInfo, plog.SeverityNumberInfo, plog.SeverityNumberWarn, plog.SeverityNumberError}[r.IntN(4)]
			lr.SetSeverityNumber(severity)
			lr.SetSeverityText(severity.String())
			lr.Body().SetStr(fmt.Sprintf(fixtureMessages[r.IntN(len(fixtureMessages))], r.IntN(100000)))
			lr.Attributes().PutStr("http.route", fixtureRoutes[r.IntN(len(fixtureRoutes))])
			lr.Attributes().PutStr("user.id", fixtureHex(r, 16))
			lr.SetTraceID(fixtureTraceID(r))
			lr.SetSpanID(fixtureSpanID(r))
		}
	}
	return ld
}

// fixtureMetrics returns batch number batch, 15 seconds apart: one metric of
// every type for each service.
func fixtureMetrics(batch int) pmetric.Metrics {
	r := fixtureRand(batch, 3)
	md := pmetric.NewMetrics()
	now := ts(fixtureStart.Add(time.Duration(batch) * 15 * time.Second))
	start := ts(fixtureStart)
	for _, service := range fixtureServices {
		rm := md.ResourceMetrics().AppendEmpty()
		fixtureResource(rm.Resource(), service)
		rm.SetSchemaUrl(fixtureSchemaURL)
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName("go.opentelemetry.io/otel/sdk/metric")

		gauge := sm.Metrics().AppendEmpty()
		gauge.SetName("process.cpu.utilization")
		gauge.SetUnit("1")
		gp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
		gp.SetTimestamp(now)
		gp.SetDoubleValue(float64(r.IntN(1000)) / 1000)

		sum := sm.Metrics().AppendEmpty()
		sum.SetName("http.server.request.count")
		sum.SetUnit("{request}")
		s := sum.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

		h := sm.Metrics().AppendEmpty()
		h.SetName("http.server.request.duration")
		h.SetUnit("s")
		hd := h.SetEmptyHistogram()
		hd.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

		for _, route := range fixtureRoutes {
			p := s.DataPoints().AppendEmpty()
			p.SetStartTimestamp(start)
			p.SetTimestamp(now)
			p.SetIntValue(int64((batch + 1) * (10 + r.IntN(90))))
			p.Attributes().PutStr("http.route", route)
			p.Attributes().PutStr("http.request.method", "GET")

			hp := hd.DataPoints().AppendEmpty()
			hp.SetStartTimestamp(start)
			hp.SetTimestamp(now)
			hp.ExplicitBounds().FromRaw([]float64{0.01, 0.05, 0.1, 0.5, 1})
			counts := make([]uint64, 6)
			var total uint64
			for i := range counts {
				counts[i] = uint64(r.IntN(20))
				total += counts[i]
			}
			hp.BucketCounts().FromRaw(counts)
			hp.SetCount(total)
			hp.SetSum(float64(total) * 0.07)
			hp.Attributes().PutStr("http.route", route)
		}

		exp := sm.Metrics().AppendEmpty()
		exp.SetName("rpc.client.duration")
		exp.SetUnit("ms")
		e := exp.SetEmptyExponentialHistogram()
		e.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
		ep := e.DataPoints().AppendEmpty()
		ep.SetStartTimestamp(start)
		ep.SetTimestamp(now)
		ep.SetScale(2)
		ep.SetCount(12)
		ep.SetSum(48.5)
		ep.Positive().SetOffset(1)
		ep.Positive().BucketCounts().FromRaw([]uint64{3, 5, 4})
		ep.Attributes().PutStr("rpc.service", "payments.Charge")

		summary := sm.Metrics().AppendEmpty()
		summary.SetName("gc.pause")
		summary.SetUnit("s")
		sp := summary.SetEmptySummary().DataPoints().AppendEmpty()
		sp.SetStartTimestamp(start)
		sp.SetTimestamp(now)
		sp.SetCount(uint64(10 + r.IntN(10)))
		sp.SetSum(0.25)
		q := sp.QuantileValues().AppendEmpty()
		q.SetQuantile(0.99)
		q.SetValue(0.004)
	}
	return md
}
