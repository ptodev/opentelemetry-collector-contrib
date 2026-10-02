// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

// This file defines what the processor adds to the OTLP it writes. It's the
// only definition of the processor's file format: the records themselves are
// ordinary OTLP.

// ConventionsVersion changes whenever an attribute or a detail level changes.
const ConventionsVersion = 4

// Detail levels, from least to most detail. Each one writes valid OTLP.
const (
	// DetailSizes removes attributes, events, links, exemplars, and log bodies,
	// and adds the record's sizes and counts.
	DetailSizes = "sizes"
	// DetailKeys replaces each attribute value, and each log body, with a map
	// of its type and size. Numbers, booleans, and strings and byte values of
	// up to MaxKeptValueBytes are kept as they are: their description would
	// take about as much space. Maps and slices are always replaced, so a map
	// of type and bytes at this level is always a description.
	DetailKeys = "keys"
	// DetailFull keeps the record unchanged, and adds its sizes and counts.
	DetailFull = "full"
)

// Resource attributes.
const (
	AttrSessionID     = "tap.session_id"
	AttrInstance      = "tap.instance"
	AttrProcessor     = "tap.processor"
	AttrResourceBytes = "tap.resource_bytes"
)

// AttrSchemaURL is the resource's schema URL on a resource, and the scope's on
// a scope, when there is one. OTLP has schema URLs, but readers such as
// duckdb-otlp don't show them.
const AttrSchemaURL = "tap.schema_url"

// Attributes on every span, log record, and data point.
const (
	AttrDetail         = "tap.detail"
	AttrRules          = "tap.rules"
	AttrAttributeCount = "tap.attribute_count"
)

// The record's encoded size, before anything was removed. Its name depends on
// the signal.
const (
	AttrSpanBytes  = "tap.span_bytes"
	AttrLogBytes   = "tap.log_bytes"
	AttrPointBytes = "tap.point_bytes"
)

// Signal-specific counts and sizes.
const (
	AttrEventCount = "tap.event_count"
	AttrLinkCount  = "tap.link_count"
	AttrBodyBytes  = "tap.body_bytes"
)

// Keys of the map that replaces a value at DetailKeys.
const (
	KeyType  = "type"
	KeyBytes = "bytes"
)

// MaxKeptValueBytes is the longest string or byte value DetailKeys keeps.
// Its description, such as {type: Str, bytes: 48930}, takes about 32 bytes
// encoded.
const MaxKeptValueBytes = 32
