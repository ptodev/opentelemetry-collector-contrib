package wasmreceiver

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/wasmreceiver/internal/metadata"
)

// NewFactory creates a factory for filelog receiver
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, metadata.MetricsStability))
}

// ReceiverType implements stanza.LogReceiverType
// to create a file tailing receiver
type ReceiverType struct{}

// Type is the receiver type
func (f ReceiverType) Type() component.Type {
	return metadata.Type
}

// CreateDefaultConfig creates a config with type and version
func (f ReceiverType) CreateDefaultConfig() component.Config {
	return createDefaultConfig()
}

func createDefaultConfig() component.Config {
	return &Config{}
}

// FileLogConfig defines configuration for the filelog receiver
type Config struct {
	//TODO: "file://" or "http://"
	Module string `mapstructure:"module"`
}

func createMetricsReceiver(
	_ context.Context,
	params receiver.Settings,
	rConf component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	//TODO: Use the config
	// cfg := rConf.(*Config)

	return WasmReceiver{
		consumer: consumer,
	}, nil
}

type WasmReceiver struct {
	consumer consumer.Metrics
}

// Shutdown implements receiver.Metrics.
func (w WasmReceiver) Shutdown(ctx context.Context) error {
	//TODO: Implement
	return nil
}

func generateTestMetric() pmetric.Metrics {
	metrics := pmetric.NewMetrics()
	resourceMetric := metrics.ResourceMetrics().AppendEmpty()
	scopeMetric := resourceMetric.ScopeMetrics().AppendEmpty()

	fooBarMetric := scopeMetric.Metrics().AppendEmpty()
	fooBarMetric.SetName("foo.bar")
	fooBarMetric.SetEmptySum().DataPoints().AppendEmpty().SetIntValue(0)

	return metrics
}

// Start implements receiver.Metrics.
func (w WasmReceiver) Start(ctx context.Context, host component.Host) error {
	//TODO: Load the WASM binary, throw errors if not found or if it couldn't be loaded
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.consumer.ConsumeMetrics(ctx, generateTestMetric())
		}
	}()
	return nil
}
