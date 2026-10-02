// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Everything in this file works on the processor's own copy of a record,
// never on the batch that continues down the pipeline.

// valueSize approximates the encoded size of a value. It's only meant for
// comparing values with each other.
func valueSize(v pcommon.Value) int64 {
	switch v.Type() {
	case pcommon.ValueTypeStr:
		return int64(len(v.Str()))
	case pcommon.ValueTypeBytes:
		return int64(v.Bytes().Len())
	case pcommon.ValueTypeInt, pcommon.ValueTypeDouble:
		return 8
	case pcommon.ValueTypeBool:
		return 1
	case pcommon.ValueTypeMap:
		var n int64
		v.Map().Range(func(k string, e pcommon.Value) bool {
			n += int64(len(k)) + valueSize(e)
			return true
		})
		return n
	case pcommon.ValueTypeSlice:
		var n int64
		for i := 0; i < v.Slice().Len(); i++ {
			n += valueSize(v.Slice().At(i))
		}
		return n
	}
	return 0
}

// describeValue replaces a value with a map of its type and size, unless the
// value is no bigger than the map would be. See DetailKeys.
func describeValue(v pcommon.Value) {
	typ, size := v.Type().String(), valueSize(v)
	switch v.Type() {
	case pcommon.ValueTypeEmpty, pcommon.ValueTypeInt, pcommon.ValueTypeDouble, pcommon.ValueTypeBool:
		return
	case pcommon.ValueTypeStr, pcommon.ValueTypeBytes:
		if size <= MaxKeptValueBytes {
			return
		}
	}
	m := v.SetEmptyMap()
	m.PutStr(KeyType, typ)
	m.PutInt(KeyBytes, size)
}

// summarizeAttributes applies a detail level to a map of attributes.
func summarizeAttributes(attrs pcommon.Map, d detail) {
	switch d {
	case detailSizes:
		attrs.Clear()
	case detailKeys:
		attrs.Range(func(_ string, v pcommon.Value) bool {
			describeValue(v)
			return true
		})
	}
}

// addTapAttributes records the detail level, the matching rules, and the
// original attribute count. It runs after summarizing, so these attributes
// are never summarized themselves.
func addTapAttributes(attrs pcommon.Map, d detail, matched []int64, attributeCount int) {
	attrs.PutStr(AttrDetail, d.String())
	ruleSlice := attrs.PutEmptySlice(AttrRules)
	for _, i := range matched {
		ruleSlice.AppendEmpty().SetInt(i)
	}
	attrs.PutInt(AttrAttributeCount, int64(attributeCount))
}

func summarizeSpan(s ptrace.Span, d detail, matched []int64, size int) {
	attributeCount, eventCount, linkCount := s.Attributes().Len(), s.Events().Len(), s.Links().Len()
	summarizeAttributes(s.Attributes(), d)
	switch d {
	case detailSizes:
		s.Events().RemoveIf(func(ptrace.SpanEvent) bool { return true })
		s.Links().RemoveIf(func(ptrace.SpanLink) bool { return true })
	case detailKeys:
		for i := 0; i < s.Events().Len(); i++ {
			summarizeAttributes(s.Events().At(i).Attributes(), d)
		}
		for i := 0; i < s.Links().Len(); i++ {
			summarizeAttributes(s.Links().At(i).Attributes(), d)
		}
	}
	addTapAttributes(s.Attributes(), d, matched, attributeCount)
	s.Attributes().PutInt(AttrSpanBytes, int64(size))
	s.Attributes().PutInt(AttrEventCount, int64(eventCount))
	s.Attributes().PutInt(AttrLinkCount, int64(linkCount))
}

func summarizeLog(lr plog.LogRecord, d detail, matched []int64, size int) {
	attributeCount, bodyBytes := lr.Attributes().Len(), valueSize(lr.Body())
	summarizeAttributes(lr.Attributes(), d)
	switch d {
	case detailSizes:
		pcommon.NewValueEmpty().CopyTo(lr.Body())
	case detailKeys:
		describeValue(lr.Body())
	}
	addTapAttributes(lr.Attributes(), d, matched, attributeCount)
	lr.Attributes().PutInt(AttrLogBytes, int64(size))
	lr.Attributes().PutInt(AttrBodyBytes, bodyBytes)
}

// dataPoint is what every metric data point type has in common.
type dataPoint[P any] interface {
	Attributes() pcommon.Map
	CopyTo(dest P)
}

func summarizePoint[P dataPoint[P]](p P, d detail, matched []int64, size int) {
	attributeCount := p.Attributes().Len()
	summarizeAttributes(p.Attributes(), d)
	// Summary data points have no exemplars.
	if withExemplars, ok := any(p).(interface{ Exemplars() pmetric.ExemplarSlice }); ok && d != detailFull {
		withExemplars.Exemplars().RemoveIf(func(pmetric.Exemplar) bool { return true })
	}
	addTapAttributes(p.Attributes(), d, matched, attributeCount)
	p.Attributes().PutInt(AttrPointBytes, int64(size))
}
