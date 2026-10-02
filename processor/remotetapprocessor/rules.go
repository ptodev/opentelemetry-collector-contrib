// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
)

// detail is a detail level. Higher values keep more.
type detail int

const (
	detailNone detail = iota
	detailSizes
	detailKeys
	detailFull
)

func parseDetail(s string) (detail, error) {
	switch s {
	case DetailSizes:
		return detailSizes, nil
	case DetailKeys:
		return detailKeys, nil
	case DetailFull:
		return detailFull, nil
	}
	return detailNone, fmt.Errorf("detail must be %s, %s, or %s, got %q", DetailSizes, DetailKeys, DetailFull, s)
}

func (d detail) String() string {
	return [...]string{"none", DetailSizes, DetailKeys, DetailFull}[d]
}

type rule[K any] struct {
	// conditions is nil when the rule has no where conditions.
	conditions *ottl.ConditionSequence[K]
	// threshold is the highest trace ID hash to keep. Zero means no sampling.
	threshold uint64
	detail    detail
}

// rules holds the rules of one signal, in configuration order.
type rules[K any] []rule[K]

// match returns the highest detail of the matching rules, and their indexes.
// A record that matches no rule gets detailNone.
func (rs rules[K]) match(ctx context.Context, tCtx K, traceID pcommon.TraceID) (detail, []int64) {
	best := detailNone
	var matched []int64
	for i, r := range rs {
		if r.threshold != 0 && (traceID.IsEmpty() || hashTraceID(traceID) > r.threshold) {
			continue
		}
		if r.conditions != nil {
			// With IgnoreError, a condition that fails to evaluate is false.
			ok, _ := r.conditions.Eval(ctx, tCtx)
			if !ok {
				continue
			}
		}
		matched = append(matched, int64(i))
		best = max(best, r.detail)
	}
	return best, matched
}

// hashTraceID maps a trace ID to a number that's the same on every instance.
// Not every SDK generates random trace IDs, so the FNV hash is mixed with
// MurmurHash3's finalizer to spread IDs that differ in only a few bits.
func hashTraceID(id pcommon.TraceID) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	x := h.Sum64()
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func percentThreshold(percent float64) uint64 {
	if percent >= 100 {
		return math.MaxUint64
	}
	return uint64(percent / 100 * math.MaxUint64)
}

func buildRules[K any](cfg []RuleConfig, parser ottl.Parser[K], set component.TelemetrySettings) (rules[K], error) {
	out := make(rules[K], 0, len(cfg))
	for i, rc := range cfg {
		d, err := parseDetail(rc.Detail)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		r := rule[K]{detail: d}
		if rc.TracePercent > 0 {
			r.threshold = percentThreshold(rc.TracePercent)
		}
		if len(rc.Where) > 0 {
			conditions, err := parser.ParseConditions(rc.Where)
			if err != nil {
				return nil, fmt.Errorf("rule %d: %w", i, err)
			}
			seq := ottl.NewConditionSequence(conditions, set,
				ottl.WithLogicOperation[K](ottl.And),
				ottl.WithConditionSequenceErrorMode[K](ottl.IgnoreError),
			)
			r.conditions = &seq
		}
		out = append(out, r)
	}
	return out, nil
}

func newSpanRules(cfg []RuleConfig, set component.TelemetrySettings) (rules[*ottlspan.TransformContext], error) {
	parser, err := ottlspan.NewParser(ottlfuncs.StandardConverters[*ottlspan.TransformContext](), set, ottlspan.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	return buildRules(cfg, parser, set)
}

func newLogRules(cfg []RuleConfig, set component.TelemetrySettings) (rules[*ottllog.TransformContext], error) {
	parser, err := ottllog.NewParser(ottlfuncs.StandardConverters[*ottllog.TransformContext](), set, ottllog.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	return buildRules(cfg, parser, set)
}

func newPointRules(cfg []RuleConfig, set component.TelemetrySettings) (rules[*ottldatapoint.TransformContext], error) {
	parser, err := ottldatapoint.NewParser(ottlfuncs.StandardConverters[*ottldatapoint.TransformContext](), set, ottldatapoint.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	return buildRules(cfg, parser, set)
}
