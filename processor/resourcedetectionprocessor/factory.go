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

package resourcedetectionprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor"

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/aws/ec2"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/aws/ecs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/aws/eks"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/aws/elasticbeanstalk"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/azure"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/azure/aks"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/consul"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/docker"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/env"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/gcp"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external/system"
)

const (
	// The value of "type" key in configuration.
	typeStr = "resourcedetection"
	// The stability level of the processor.
	stability = component.StabilityLevelBeta
)

var consumerCapabilities = consumer.Capabilities{MutatesData: true}

type factory struct {
	resourceProviderFactory *internal.ResourceProviderFactory

	// providers stores a provider for each named processor that
	// may a different set of detectors configured.
	providers map[component.ID]*internal.ResourceProvider
	lock      sync.Mutex
}

// NewFactory creates a new factory for ResourceDetection processor.
func NewFactory() processor.Factory {
	resourceProviderFactory := internal.NewProviderFactory(map[internal.DetectorType]internal.DetectorFactory{
		aks.TypeStr:              aks.NewDetector,
		azure.TypeStr:            azure.NewDetector,
		consul.TypeStr:           consul.NewDetector,
		docker.TypeStr:           docker.NewDetector,
		ec2.TypeStr:              ec2.NewDetector,
		ecs.TypeStr:              ecs.NewDetector,
		eks.TypeStr:              eks.NewDetector,
		elasticbeanstalk.TypeStr: elasticbeanstalk.NewDetector,
		env.TypeStr:              env.NewDetector,
		gcp.TypeStr:              gcp.NewDetector,
		// TODO(#10348): Remove GKE and GCE after the v0.54.0 release.
		gcp.DeprecatedGKETypeStr: gcp.NewDetector,
		gcp.DeprecatedGCETypeStr: gcp.NewDetector,
		system.TypeStr:           system.NewDetector,
	})

	f := &factory{
		resourceProviderFactory: resourceProviderFactory,
		providers:               map[component.ID]*internal.ResourceProvider{},
	}

	return processor.NewFactory(
		typeStr,
		createDefaultConfig,
		processor.WithTraces(f.createTracesProcessor, stability),
		processor.WithMetrics(f.createMetricsProcessor, stability),
		processor.WithLogs(f.createLogsProcessor, stability))
}

// Type gets the type of the Option config created by this factory.
func (*factory) Type() component.Type {
	return typeStr
}

func createDefaultConfig() component.Config {
	return &Config{
		Detectors:          []string{env.TypeStr},
		HTTPClientSettings: defaultHTTPClientSettings(),
		Override:           true,
		Attributes:         nil,
		// TODO: Once issue(https://github.com/open-telemetry/opentelemetry-collector/issues/4001) gets resolved,
		// 		 Set the default value of 'hostname_source' here instead of 'system' detector
	}
}

func defaultHTTPClientSettings() confighttp.HTTPClientSettings {
	httpClientSettings := confighttp.NewDefaultHTTPClientSettings()
	httpClientSettings.Timeout = 5 * time.Second
	return httpClientSettings
}

func (f *factory) createTracesProcessor(
	ctx context.Context,
	set processor.CreateSettings,
	cfg component.Config,
	nextConsumer consumer.Traces,
) (processor.Traces, error) {
	rdp, err := f.getResourceDetectionProcessor(set, cfg)
	if err != nil {
		return nil, err
	}

	return processorhelper.NewTracesProcessor(
		ctx,
		set,
		cfg,
		nextConsumer,
		rdp.processTraces,
		processorhelper.WithCapabilities(consumerCapabilities),
		processorhelper.WithStart(rdp.Start))
}

func (f *factory) createMetricsProcessor(
	ctx context.Context,
	set processor.CreateSettings,
	cfg component.Config,
	nextConsumer consumer.Metrics,
) (processor.Metrics, error) {
	rdp, err := f.getResourceDetectionProcessor(set, cfg)
	if err != nil {
		return nil, err
	}

	return processorhelper.NewMetricsProcessor(
		ctx,
		set,
		cfg,
		nextConsumer,
		rdp.processMetrics,
		processorhelper.WithCapabilities(consumerCapabilities),
		processorhelper.WithStart(rdp.Start))
}

func (f *factory) createLogsProcessor(
	ctx context.Context,
	set processor.CreateSettings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (processor.Logs, error) {
	rdp, err := f.getResourceDetectionProcessor(set, cfg)
	if err != nil {
		return nil, err
	}

	return processorhelper.NewLogsProcessor(
		ctx,
		set,
		cfg,
		nextConsumer,
		rdp.processLogs,
		processorhelper.WithCapabilities(consumerCapabilities),
		processorhelper.WithStart(rdp.Start))
}

func (f *factory) getResourceDetectionProcessor(
	params processor.CreateSettings,
	cfg component.Config,
) (*resourceDetectionProcessor, error) {
	oCfg := cfg.(*Config)

	provider, err := f.getResourceProvider(params, oCfg.HTTPClientSettings.Timeout, oCfg.Detectors, oCfg.DetectorConfig, oCfg.Attributes)
	if err != nil {
		return nil, err
	}

	return &resourceDetectionProcessor{
		provider:           provider,
		override:           oCfg.Override,
		httpClientSettings: oCfg.HTTPClientSettings,
		telemetrySettings:  params.TelemetrySettings,
	}, nil
}

func (f *factory) getResourceProvider(
	params processor.CreateSettings,
	timeout time.Duration,
	configuredDetectors []string,
	detectorConfigs DetectorConfig,
	attributes []string,
) (*internal.ResourceProvider, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if provider, ok := f.providers[params.ID]; ok {
		return provider, nil
	}

	// TODO(#10348): Remove this after the v0.54.0 release.
	configuredDetectors = gcp.DeduplicateDetectors(params, configuredDetectors)

	detectorTypes := make([]internal.DetectorType, 0, len(configuredDetectors))
	for _, key := range configuredDetectors {
		detectorTypes = append(detectorTypes, internal.DetectorType(strings.TrimSpace(key)))
	}

	provider, err := f.resourceProviderFactory.CreateResourceProvider(params, timeout, attributes, &detectorConfigs, detectorTypes...)
	if err != nil {
		return nil, err
	}

	f.providers[params.ID] = provider
	return provider, nil
}
