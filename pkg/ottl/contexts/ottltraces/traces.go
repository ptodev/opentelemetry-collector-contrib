// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ottltraces // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottltraces"

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/trace"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/external/ottlcommon"
)

var _ ottlcommon.ResourceContext = TransformContext{}
var _ ottlcommon.InstrumentationScopeContext = TransformContext{}

type TransformContext struct {
	span                 ptrace.Span
	instrumentationScope pcommon.InstrumentationScope
	resource             pcommon.Resource
}

func NewTransformContext(span ptrace.Span, instrumentationScope pcommon.InstrumentationScope, resource pcommon.Resource) TransformContext {
	return TransformContext{
		span:                 span,
		instrumentationScope: instrumentationScope,
		resource:             resource,
	}
}

func (ctx TransformContext) GetSpan() ptrace.Span {
	return ctx.span
}

func (ctx TransformContext) GetInstrumentationScope() pcommon.InstrumentationScope {
	return ctx.instrumentationScope
}

func (ctx TransformContext) GetResource() pcommon.Resource {
	return ctx.resource
}

func NewParser(functions map[string]interface{}, telemetrySettings component.TelemetrySettings) ottl.Parser[TransformContext] {
	return ottl.NewParser[TransformContext](functions, parsePath, parseEnum, telemetrySettings)
}

var symbolTable = map[ottl.EnumSymbol]ottl.Enum{
	"SPAN_KIND_UNSPECIFIED": ottl.Enum(ptrace.SpanKindUnspecified),
	"SPAN_KIND_INTERNAL":    ottl.Enum(ptrace.SpanKindInternal),
	"SPAN_KIND_SERVER":      ottl.Enum(ptrace.SpanKindServer),
	"SPAN_KIND_CLIENT":      ottl.Enum(ptrace.SpanKindClient),
	"SPAN_KIND_PRODUCER":    ottl.Enum(ptrace.SpanKindProducer),
	"SPAN_KIND_CONSUMER":    ottl.Enum(ptrace.SpanKindConsumer),
	"STATUS_CODE_UNSET":     ottl.Enum(ptrace.StatusCodeUnset),
	"STATUS_CODE_OK":        ottl.Enum(ptrace.StatusCodeOk),
	"STATUS_CODE_ERROR":     ottl.Enum(ptrace.StatusCodeError),
}

func parseEnum(val *ottl.EnumSymbol) (*ottl.Enum, error) {
	if val != nil {
		if enum, ok := symbolTable[*val]; ok {
			return &enum, nil
		}
		return nil, fmt.Errorf("enum symbol, %s, not found", *val)
	}
	return nil, fmt.Errorf("enum symbol not provided")
}

func parsePath(val *ottl.Path) (ottl.GetSetter[TransformContext], error) {
	if val != nil && len(val.Fields) > 0 {
		return newPathGetSetter(val.Fields)
	}
	return nil, fmt.Errorf("bad path %v", val)
}

func newPathGetSetter(path []ottl.Field) (ottl.GetSetter[TransformContext], error) {
	switch path[0].Name {
	case "resource":
		return ottlcommon.ResourcePathGetSetter[TransformContext](path[1:])
	case "instrumentation_scope":
		return ottlcommon.ScopePathGetSetter[TransformContext](path[1:])
	case "trace_id":
		if len(path) == 1 {
			return accessTraceID(), nil
		}
		if path[1].Name == "string" {
			return accessStringTraceID(), nil
		}
	case "span_id":
		if len(path) == 1 {
			return accessSpanID(), nil
		}
		if path[1].Name == "string" {
			return accessStringSpanID(), nil
		}
	case "trace_state":
		mapKey := path[0].MapKey
		if mapKey == nil {
			return accessTraceState(), nil
		}
		return accessTraceStateKey(mapKey), nil
	case "parent_span_id":
		return accessParentSpanID(), nil
	case "name":
		return accessName(), nil
	case "kind":
		return accessKind(), nil
	case "start_time_unix_nano":
		return accessStartTimeUnixNano(), nil
	case "end_time_unix_nano":
		return accessEndTimeUnixNano(), nil
	case "attributes":
		mapKey := path[0].MapKey
		if mapKey == nil {
			return accessAttributes(), nil
		}
		return accessAttributesKey(mapKey), nil
	case "dropped_attributes_count":
		return accessDroppedAttributesCount(), nil
	case "events":
		return accessEvents(), nil
	case "dropped_events_count":
		return accessDroppedEventsCount(), nil
	case "links":
		return accessLinks(), nil
	case "dropped_links_count":
		return accessDroppedLinksCount(), nil
	case "status":
		if len(path) == 1 {
			return accessStatus(), nil
		}
		switch path[1].Name {
		case "code":
			return accessStatusCode(), nil
		case "message":
			return accessStatusMessage(), nil
		}
	default:
		return nil, fmt.Errorf("invalid path expression, unrecognized field %v", path[0].Name)
	}

	return nil, fmt.Errorf("invalid path expression %v", path)
}

func accessTraceID() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().TraceID(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if newTraceID, ok := val.(pcommon.TraceID); ok {
				ctx.GetSpan().SetTraceID(newTraceID)
			}
			return nil
		},
	}
}

func accessStringTraceID() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().TraceID().HexString(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				if traceID, err := parseTraceID(str); err == nil {
					ctx.GetSpan().SetTraceID(traceID)
				}
			}
			return nil
		},
	}
}

func accessSpanID() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().SpanID(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if newSpanID, ok := val.(pcommon.SpanID); ok {
				ctx.GetSpan().SetSpanID(newSpanID)
			}
			return nil
		},
	}
}

func accessStringSpanID() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().SpanID().HexString(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				if spanID, err := parseSpanID(str); err == nil {
					ctx.GetSpan().SetSpanID(spanID)
				}
			}
			return nil
		},
	}
}

func accessTraceState() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().TraceState().AsRaw(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				ctx.GetSpan().TraceState().FromRaw(str)
			}
			return nil
		},
	}
}

func accessTraceStateKey(mapKey *string) ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			if ts, err := trace.ParseTraceState(ctx.GetSpan().TraceState().AsRaw()); err == nil {
				return ts.Get(*mapKey), nil
			}
			return nil, nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				if ts, err := trace.ParseTraceState(ctx.GetSpan().TraceState().AsRaw()); err == nil {
					if updated, err := ts.Insert(*mapKey, str); err == nil {
						ctx.GetSpan().TraceState().FromRaw(updated.String())
					}
				}
			}
			return nil
		},
	}
}

func accessParentSpanID() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().ParentSpanID(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if newParentSpanID, ok := val.(pcommon.SpanID); ok {
				ctx.GetSpan().SetParentSpanID(newParentSpanID)
			}
			return nil
		},
	}
}

func accessName() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Name(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				ctx.GetSpan().SetName(str)
			}
			return nil
		},
	}
}

func accessKind() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return int64(ctx.GetSpan().Kind()), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetKind(ptrace.SpanKind(i))
			}
			return nil
		},
	}
}

func accessStartTimeUnixNano() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().StartTimestamp().AsTime().UnixNano(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(0, i)))
			}
			return nil
		},
	}
}

func accessEndTimeUnixNano() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().EndTimestamp().AsTime().UnixNano(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetEndTimestamp(pcommon.NewTimestampFromTime(time.Unix(0, i)))
			}
			return nil
		},
	}
}

func accessAttributes() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Attributes(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if attrs, ok := val.(pcommon.Map); ok {
				attrs.CopyTo(ctx.GetSpan().Attributes())
			}
			return nil
		},
	}
}

func accessAttributesKey(mapKey *string) ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ottlcommon.GetMapValue(ctx.GetSpan().Attributes(), *mapKey), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			ottlcommon.SetMapValue(ctx.GetSpan().Attributes(), *mapKey, val)
			return nil
		},
	}
}

func accessDroppedAttributesCount() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return int64(ctx.GetSpan().DroppedAttributesCount()), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetDroppedAttributesCount(uint32(i))
			}
			return nil
		},
	}
}

func accessEvents() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Events(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if slc, ok := val.(ptrace.SpanEventSlice); ok {
				ctx.GetSpan().Events().RemoveIf(func(event ptrace.SpanEvent) bool {
					return true
				})
				slc.CopyTo(ctx.GetSpan().Events())
			}
			return nil
		},
	}
}

func accessDroppedEventsCount() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return int64(ctx.GetSpan().DroppedEventsCount()), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetDroppedEventsCount(uint32(i))
			}
			return nil
		},
	}
}

func accessLinks() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Links(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if slc, ok := val.(ptrace.SpanLinkSlice); ok {
				ctx.GetSpan().Links().RemoveIf(func(event ptrace.SpanLink) bool {
					return true
				})
				slc.CopyTo(ctx.GetSpan().Links())
			}
			return nil
		},
	}
}

func accessDroppedLinksCount() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return int64(ctx.GetSpan().DroppedLinksCount()), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().SetDroppedLinksCount(uint32(i))
			}
			return nil
		},
	}
}

func accessStatus() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Status(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if status, ok := val.(ptrace.Status); ok {
				status.CopyTo(ctx.GetSpan().Status())
			}
			return nil
		},
	}
}

func accessStatusCode() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return int64(ctx.GetSpan().Status().Code()), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if i, ok := val.(int64); ok {
				ctx.GetSpan().Status().SetCode(ptrace.StatusCode(i))
			}
			return nil
		},
	}
}

func accessStatusMessage() ottl.StandardGetSetter[TransformContext] {
	return ottl.StandardGetSetter[TransformContext]{
		Getter: func(ctx TransformContext) (interface{}, error) {
			return ctx.GetSpan().Status().Message(), nil
		},
		Setter: func(ctx TransformContext, val interface{}) error {
			if str, ok := val.(string); ok {
				ctx.GetSpan().Status().SetMessage(str)
			}
			return nil
		},
	}
}

func parseSpanID(spanIDStr string) (pcommon.SpanID, error) {
	id, err := hex.DecodeString(spanIDStr)
	if err != nil {
		return pcommon.SpanID{}, err
	}
	if len(id) != 8 {
		return pcommon.SpanID{}, errors.New("span ids must be 8 bytes")
	}
	var idArr [8]byte
	copy(idArr[:8], id)
	return pcommon.SpanID(idArr), nil
}

func parseTraceID(traceIDStr string) (pcommon.TraceID, error) {
	id, err := hex.DecodeString(traceIDStr)
	if err != nil {
		return pcommon.TraceID{}, err
	}
	if len(id) != 16 {
		return pcommon.TraceID{}, errors.New("traces ids must be 16 bytes")
	}
	var idArr [16]byte
	copy(idArr[:16], id)
	return pcommon.TraceID(idArr), nil
}
