// Copyright  OpenTelemetry Authors
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

package mongodbatlasreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/mongodbatlasreceiver"

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/receiver/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/mongodbatlasreceiver/external/metadata"
)

const (
	typeStr              = "mongodbatlas"
	stability            = component.StabilityLevelBeta
	defaultGranularity   = "PT1M" // 1-minute, as per https://docs.atlas.mongodb.com/reference/api/process-measurements/
	defaultAlertsEnabled = false
	defaultLogsEnabled   = false
)

// NewFactory creates a factory for MongoDB Atlas receiver
func NewFactory() component.ReceiverFactory {
	return component.NewReceiverFactory(
		typeStr,
		createDefaultConfig,
		component.WithMetricsReceiver(createMetricsReceiver, stability),
		component.WithLogsReceiver(createCombinedLogReceiver, stability))

}

func createMetricsReceiver(
	_ context.Context,
	params component.ReceiverCreateSettings,
	rConf config.Receiver,
	consumer consumer.Metrics,
) (component.MetricsReceiver, error) {
	cfg := rConf.(*Config)
	recv := newMongoDBAtlasReceiver(params, cfg)
	ms, err := newMongoDBAtlasScraper(recv)
	if err != nil {
		return nil, fmt.Errorf("unable to create a MongoDB Atlas Scaper instance: %w", err)
	}

	return scraperhelper.NewScraperControllerReceiver(&cfg.ScraperControllerSettings, params, consumer, scraperhelper.AddScraper(ms))
}

func createCombinedLogReceiver(
	ctx context.Context,
	params component.ReceiverCreateSettings,
	rConf config.Receiver,
	consumer consumer.Logs,
) (component.LogsReceiver, error) {
	cfg := rConf.(*Config)

	if !cfg.Alerts.Enabled && !cfg.Logs.Enabled {
		return nil, errors.New("one of 'alerts' or 'logs' must be enabled")
	}

	var err error
	recv := &combinedLogsReceiver{}

	if cfg.Alerts.Enabled {
		recv.alerts, err = newAlertsReceiver(params.Logger, cfg, consumer)
		if err != nil {
			return nil, fmt.Errorf("unable to create a MongoDB Atlas Alerts Receiver instance: %w", err)
		}
	}

	if cfg.Logs.Enabled {
		recv.logs = newMongoDBAtlasLogsReceiver(params, cfg, consumer)
	}

	return recv, nil
}

func createDefaultConfig() config.Receiver {
	return &Config{
		ScraperControllerSettings: scraperhelper.NewDefaultScraperControllerSettings(typeStr),
		Granularity:               defaultGranularity,
		RetrySettings:             exporterhelper.NewDefaultRetrySettings(),
		Metrics:                   metadata.DefaultMetricsSettings(),
		Alerts: AlertConfig{
			Enabled:      defaultAlertsEnabled,
			Mode:         alertModeListen,
			PollInterval: defaultAlertsPollInterval,
			PageSize:     defaultAlertsPageSize,
			MaxPages:     defaultAlertsMaxPages,
		},
		Logs: LogConfig{
			Enabled:  defaultLogsEnabled,
			Projects: []*ProjectConfig{},
		},
	}
}
