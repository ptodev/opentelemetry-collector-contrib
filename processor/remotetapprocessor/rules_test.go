// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
)

func matchSpan(t *testing.T, cfg []RuleConfig, name string, traceID pcommon.TraceID) (detail, []int64) {
	t.Helper()
	r, err := newSpanRules(cfg, componenttest.NewNopTelemetrySettings())
	require.NoError(t, err)
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetName(name)
	span.SetTraceID(traceID)
	tCtx := ottlspan.NewTransformContext(rs, ss, span)
	defer tCtx.Close()
	return r.match(t.Context(), tCtx, span.TraceID())
}

func traceID(n uint64) pcommon.TraceID {
	var id pcommon.TraceID
	binary.BigEndian.PutUint64(id[8:], n)
	id[0] = 1
	return id
}

func TestRules(t *testing.T) {
	levels := []RuleConfig{
		{Detail: "sizes"},
		{Where: []string{`span.name == "checkout"`}, Detail: "full"},
		{Where: []string{`span.name == "other"`}, Detail: "keys"},
	}
	tests := map[string]struct {
		rules       []RuleConfig
		span        string
		wantDetail  detail
		wantMatched []int64
	}{
		"the highest detail wins": {levels, "checkout", detailFull, []int64{0, 1}},
		"a catch-all rule":        {levels, "something-else", detailSizes, []int64{0}},
		"every condition must be true": {
			[]RuleConfig{{Where: []string{`span.name == "checkout"`, `span.kind == SPAN_KIND_SERVER`}, Detail: "full"}},
			"checkout", detailNone, nil,
		},
		"a condition that fails to evaluate is false": {
			[]RuleConfig{{Where: []string{`Int(span.name) > 0`}, Detail: "full"}},
			"checkout", detailNone, nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, matched := matchSpan(t, tt.rules, tt.span, traceID(1))
			assert.Equal(t, tt.wantDetail, d)
			assert.Equal(t, tt.wantMatched, matched)
		})
	}

	_, err := newSpanRules([]RuleConfig{{Where: []string{`name == "checkout"`}, Detail: "full"}}, componenttest.NewNopTelemetrySettings())
	assert.Error(t, err, "paths must start with their context")
}

func TestTracePercentKeepsWholeTraces(t *testing.T) {
	cfg := []RuleConfig{{TracePercent: 25, Detail: "full"}}
	kept := 0
	const traces = 4000
	for i := range uint64(traces) {
		d1, _ := matchSpan(t, cfg, "a", traceID(i))
		d2, _ := matchSpan(t, cfg, "b", traceID(i))
		// Every span of a trace gets the same decision, on any instance.
		require.Equal(t, d1, d2)
		if d1 != detailNone {
			kept++
		}
	}
	assert.InDelta(t, traces/4, kept, traces/20, "about 25%% of traces kept")

	d, _ := matchSpan(t, cfg, "a", pcommon.TraceID{})
	assert.Equal(t, detailNone, d, "a record without a trace ID isn't sampled in")
}
