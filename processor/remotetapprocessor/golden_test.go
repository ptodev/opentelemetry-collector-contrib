// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/golden"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/pdatatest/plogtest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/pdatatest/pmetrictest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/pdatatest/ptracetest"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const goldenDir = "testdata/golden"

// normalizedValue replaces the tap.* resource attributes that differ between
// runs.
const normalizedValue = "normalized"

// goldenRule keeps every record at the level, except payments', so the
// golden files also show that records, scopes, and resources that match no
// rule are left out.
func goldenRule(level string) []RuleConfig {
	return []RuleConfig{{Where: []string{`resource.attributes["service.name"] != "payments"`}, Detail: level}}
}

// TestGoldenFiles sends the same fixture telemetry through the processor at
// each detail level, for every signal. It checks that every batch passes on
// unchanged, which files the processor writes, and their counters, and
// compares what's in the files with testdata/golden. Run it with -update to
// rewrite the golden files.
func TestGoldenFiles(t *testing.T) {
	for _, level := range []string{DetailSizes, DetailKeys, DetailFull} {
		t.Run(level, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Traces.Rules, cfg.Logs.Rules, cfg.Metrics.Rules = goldenRule(level), goldenRule(level), goldenRule(level)
			ctx := t.Context()
			traceSink, logSink, metricSink := &consumertest.TracesSink{}, &consumertest.LogsSink{}, &consumertest.MetricsSink{}

			traces, err := NewFactory().CreateTraces(ctx, testSettings(), cfg, traceSink)
			require.NoError(t, err)
			logs, err := NewFactory().CreateLogs(ctx, testSettings(), cfg, logSink)
			require.NoError(t, err)
			metrics, err := NewFactory().CreateMetrics(ctx, testSettings(), cfg, metricSink)
			require.NoError(t, err)
			components := []component.Component{traces, logs, metrics}
			for _, c := range components {
				require.NoError(t, c.Start(ctx, componenttest.NewNopHost()))
			}
			require.NoError(t, startRecording(t, cfg))

			// seen and kept count records by signal. Every service has the
			// same number of records.
			seen := map[string]int{}
			for batch := range fixtureBatches {
				td, ld, md := fixtureTraces(batch), fixtureLogs(batch), fixtureMetrics(batch)
				seen["traces"] += td.SpanCount()
				seen["logs"] += ld.LogRecordCount()
				seen["metrics"] += md.DataPointCount()
				require.NoError(t, traces.ConsumeTraces(ctx, td))
				require.NoError(t, logs.ConsumeLogs(ctx, ld))
				require.NoError(t, metrics.ConsumeMetrics(ctx, md))
				assert.Equal(t, fixtureTraces(batch), traceSink.AllTraces()[batch], "traces pass on unchanged")
				assert.Equal(t, fixtureLogs(batch), logSink.AllLogs()[batch], "logs pass on unchanged")
				assert.Equal(t, fixtureMetrics(batch), metricSink.AllMetrics()[batch], "metrics pass on unchanged")
			}
			for _, c := range components {
				require.NoError(t, c.Shutdown(ctx))
			}

			sessions := readSessions(t, cfg.Directory)
			require.Len(t, sessions, 3, "one session per signal")
			for _, r := range sessions {
				signal := r.header.Signal
				t.Run(signal, func(t *testing.T) {
					kept := seen[signal] * (len(fixtureServices) - 1) / len(fixtureServices)
					c := r.last().Counters
					assert.EqualValues(t, seen[signal], c.RecordsSeen)
					assert.EqualValues(t, kept, c.RecordsWritten)
					assert.Equal(t, []int64{int64(kept)}, c.RuleMatches)
					assert.Equal(t, stopReasonShutdown, r.last().StopReason)
					require.Len(t, r.data, 1, "one file")
					assert.Equal(t, signal+"-000001.pb.zst", r.last().Files[0].Name)
					checkGolden(t, signal, r.header.SessionID, r.data[0], filepath.Join(goldenDir, signal+"-"+level+".yaml"))
				})
			}
		})
	}
}

// checkGolden parses a signal's OTLP protobuf, replaces the tap.* resource
// attributes that change between runs, and compares it with the golden file.
func checkGolden(t *testing.T, signal, sessionID string, data []byte, path string) {
	t.Helper()
	switch signal {
	case "traces":
		got, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(data)
		require.NoError(t, err)
		for i := 0; i < got.ResourceSpans().Len(); i++ {
			normalizeResource(t, sessionID, got.ResourceSpans().At(i).Resource())
		}
		if *update {
			require.NoError(t, golden.WriteTracesToFile(path, got))
		}
		want, err := golden.ReadTraces(path)
		require.NoError(t, err)
		require.NoError(t, ptracetest.CompareTraces(want, got))
	case "logs":
		got, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(data)
		require.NoError(t, err)
		for i := 0; i < got.ResourceLogs().Len(); i++ {
			normalizeResource(t, sessionID, got.ResourceLogs().At(i).Resource())
		}
		if *update {
			require.NoError(t, golden.WriteLogsToFile(path, got))
		}
		want, err := golden.ReadLogs(path)
		require.NoError(t, err)
		require.NoError(t, plogtest.CompareLogs(want, got))
	case "metrics":
		got, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(data)
		require.NoError(t, err)
		for i := 0; i < got.ResourceMetrics().Len(); i++ {
			normalizeResource(t, sessionID, got.ResourceMetrics().At(i).Resource())
		}
		if *update {
			// Writing sorts the metrics, so they're compared in any order.
			require.NoError(t, golden.WriteMetricsToFile(path, got, golden.SkipMetricTimestampNormalization()))
		}
		want, err := golden.ReadMetrics(path)
		require.NoError(t, err)
		require.NoError(t, pmetrictest.CompareMetrics(want, got,
			pmetrictest.IgnoreResourceMetricsOrder(),
			pmetrictest.IgnoreScopeMetricsOrder(),
			pmetrictest.IgnoreMetricsOrder(),
			pmetrictest.IgnoreMetricDataPointsOrder(),
		))
	default:
		t.Fatalf("unknown signal %s", signal)
	}
}

func normalizeResource(t *testing.T, sessionID string, res pcommon.Resource) {
	got, _ := res.Attributes().Get(AttrSessionID)
	assert.Equal(t, sessionID, got.Str(), "every resource names its session")
	for _, key := range []string{AttrSessionID, AttrInstance} {
		if _, ok := res.Attributes().Get(key); ok {
			res.Attributes().PutStr(key, normalizedValue)
		}
	}
}
