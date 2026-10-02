// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor/internal/metadata"
)

// NewFactory returns the processor's factory.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		metadata.Type,
		createDefaultConfig,
		processor.WithTraces(createTraces, metadata.TracesStability),
		processor.WithLogs(createLogs, metadata.LogsStability),
		processor.WithMetrics(createMetrics, metadata.MetricsStability),
	)
}

// The tap never changes the data it passes on.
var capabilities = processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: false})

func createTraces(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
	c := cfg.(*Config)
	r, err := newSpanRules(c.Traces.Rules, set.TelemetrySettings)
	if err != nil {
		return nil, err
	}
	t := &tracesTap{tap: newTap(c, "traces", c.Traces.Rules, set), rules: r}
	return processorhelper.NewTraces(ctx, set, cfg, next, t.process, capabilities,
		processorhelper.WithStart(t.start), processorhelper.WithShutdown(t.shutdown))
}

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	c := cfg.(*Config)
	r, err := newLogRules(c.Logs.Rules, set.TelemetrySettings)
	if err != nil {
		return nil, err
	}
	t := &logsTap{tap: newTap(c, "logs", c.Logs.Rules, set), rules: r}
	return processorhelper.NewLogs(ctx, set, cfg, next, t.process, capabilities,
		processorhelper.WithStart(t.start), processorhelper.WithShutdown(t.shutdown))
}

func createMetrics(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	c := cfg.(*Config)
	r, err := newPointRules(c.Metrics.Rules, set.TelemetrySettings)
	if err != nil {
		return nil, err
	}
	t := &metricsTap{tap: newTap(c, "metrics", c.Metrics.Rules, set), rules: r}
	return processorhelper.NewMetrics(ctx, set, cfg, next, t.process, capabilities,
		processorhelper.WithStart(t.start), processorhelper.WithShutdown(t.shutdown))
}
