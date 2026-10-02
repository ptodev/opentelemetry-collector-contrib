// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func described(typ string, bytes int64) map[string]any {
	return map[string]any{KeyType: typ, KeyBytes: bytes}
}

func TestSummarizeSpan(t *testing.T) {
	long := strings.Repeat("x", MaxKeptValueBytes+1)
	newSpan := func() ptrace.Span {
		s := ptrace.NewSpan()
		s.SetName("checkout")
		s.Attributes().PutStr("k8s.namespace.name", "shop")
		s.Attributes().PutStr("db.query.text", long)
		s.Attributes().PutInt("retry.count", 3)
		s.Attributes().PutEmptySlice("tags").AppendEmpty().SetStr("a")
		ev := s.Events().AppendEmpty()
		ev.Attributes().PutStr("exception.message", "boom")
		ev.Attributes().PutStr("exception.stacktrace", long)
		s.Links().AppendEmpty()
		return s
	}
	tap := func(d string, rules ...any) map[string]any {
		return map[string]any{
			AttrDetail:         d,
			AttrRules:          rules,
			AttrAttributeCount: int64(4),
			AttrSpanBytes:      int64(123),
			AttrEventCount:     int64(1),
			AttrLinkCount:      int64(1),
		}
	}
	with := func(m, attrs map[string]any) map[string]any {
		maps.Copy(m, attrs)
		return m
	}

	tests := map[string]struct {
		detail     detail
		attributes map[string]any
		// eventAttributes is nil when the span has no events left.
		eventAttributes map[string]any
		links           int
	}{
		"sizes removes attributes, events, and links": {
			detail:     detailSizes,
			attributes: tap(DetailSizes, int64(2)),
		},
		"keys describes big values and keeps small ones": {
			detail: detailKeys,
			attributes: with(tap(DetailKeys, int64(2)), map[string]any{
				"k8s.namespace.name": "shop",
				"db.query.text":      described("Str", int64(len(long))),
				"retry.count":        int64(3),
				"tags":               described("Slice", 1),
			}),
			eventAttributes: map[string]any{
				"exception.message":    "boom",
				"exception.stacktrace": described("Str", int64(len(long))),
			},
			links: 1,
		},
		"full keeps everything": {
			detail: detailFull,
			attributes: with(tap(DetailFull, int64(2)), map[string]any{
				"k8s.namespace.name": "shop",
				"db.query.text":      long,
				"retry.count":        int64(3),
				"tags":               []any{"a"},
			}),
			eventAttributes: map[string]any{"exception.message": "boom", "exception.stacktrace": long},
			links:           1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := newSpan()
			summarizeSpan(s, tt.detail, []int64{2}, 123)
			assert.Equal(t, tt.attributes, s.Attributes().AsRaw())
			if tt.eventAttributes == nil {
				assert.Zero(t, s.Events().Len())
			} else {
				assert.Equal(t, tt.eventAttributes, s.Events().At(0).Attributes().AsRaw())
			}
			assert.Equal(t, tt.links, s.Links().Len())
			assert.Equal(t, "checkout", s.Name())
		})
	}
}

func TestSummarizeLogAndPoint(t *testing.T) {
	long := strings.Repeat("x", MaxKeptValueBytes+1)
	tests := map[string]struct {
		body     string
		detail   detail
		wantBody any
	}{
		"sizes removes the body":          {body: "short", detail: detailSizes, wantBody: nil},
		"keys keeps a short body":         {body: "short", detail: detailKeys, wantBody: "short"},
		"keys describes a long body":      {body: long, detail: detailKeys, wantBody: described("Str", int64(len(long)))},
		"full keeps a long body":          {body: long, detail: detailFull, wantBody: long},
		"keys keeps a value at the limit": {body: long[1:], detail: detailKeys, wantBody: long[1:]},
		"keys keeps an empty body":        {body: "", detail: detailKeys, wantBody: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			lr := plog.NewLogRecord()
			lr.Body().SetStr(tt.body)
			summarizeLog(lr, tt.detail, nil, 50)
			assert.Equal(t, tt.wantBody, lr.Body().AsRaw())
			bodyBytes, _ := lr.Attributes().Get(AttrBodyBytes)
			assert.EqualValues(t, len(tt.body), bodyBytes.Int(), "the original body's size")

			p := pmetric.NewNumberDataPoint()
			p.Attributes().PutStr("http.route", tt.body)
			p.Exemplars().AppendEmpty()
			summarizePoint(p, tt.detail, nil, 40)
			route, ok := p.Attributes().Get("http.route")
			if tt.detail == detailSizes {
				assert.False(t, ok)
			} else {
				assert.Equal(t, tt.wantBody, route.AsRaw(), "attributes follow the same rule as bodies")
			}
			wantExemplars := 0
			if tt.detail == detailFull {
				wantExemplars = 1
			}
			assert.Equal(t, wantExemplars, p.Exemplars().Len())
		})
	}
}

// TestKeptValuesAreNoBiggerThanTheirDescription checks MaxKeptValueBytes
// against the encoded sizes.
func TestKeptValuesAreNoBiggerThanTheirDescription(t *testing.T) {
	size := func(set func(pcommon.Value)) int {
		td := ptrace.NewTraces()
		s := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
		set(s.Attributes().PutEmpty("k"))
		return (&ptrace.ProtoMarshaler{}).SpanSize(s)
	}
	kept := size(func(v pcommon.Value) { v.SetStr(strings.Repeat("x", MaxKeptValueBytes)) })
	description := size(func(v pcommon.Value) {
		v.SetStr(strings.Repeat("x", MaxKeptValueBytes+1))
		describeValue(v)
	})
	assert.InDelta(t, description, kept, 4, "a kept value takes about as much space as its description")
}
