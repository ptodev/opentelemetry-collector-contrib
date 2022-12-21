// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package awsemfexporter

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/confmap/confmaptest"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/external/aws/awsutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/resourcetotelemetry"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	require.NoError(t, err)

	tests := []struct {
		id       config.ComponentID
		expected config.Exporter
	}{
		{
			id:       config.NewComponentIDWithName(typeStr, ""),
			expected: createDefaultConfig(),
		},
		{
			id: config.NewComponentIDWithName(typeStr, "1"),
			expected: &Config{
				ExporterSettings: config.NewExporterSettings(config.NewComponentID(typeStr)),
				AWSSessionSettings: awsutil.AWSSessionSettings{
					NumberOfWorkers:       8,
					Endpoint:              "",
					RequestTimeoutSeconds: 30,
					MaxRetries:            2,
					NoVerifySSL:           false,
					ProxyAddress:          "",
					Region:                "us-west-2",
					RoleARN:               "arn:aws:iam::123456789:role/monitoring-EKS-NodeInstanceRole",
				},
				LogGroupName:          "",
				LogStreamName:         "",
				DimensionRollupOption: "ZeroAndSingleDimensionRollup",
				OutputDestination:     "cloudwatch",
			},
		},
		{
			id: config.NewComponentIDWithName(typeStr, "resource_attr_to_label"),
			expected: &Config{
				ExporterSettings: config.NewExporterSettings(config.NewComponentID(typeStr)),
				AWSSessionSettings: awsutil.AWSSessionSettings{
					NumberOfWorkers:       8,
					Endpoint:              "",
					RequestTimeoutSeconds: 30,
					MaxRetries:            2,
					NoVerifySSL:           false,
					ProxyAddress:          "",
					Region:                "",
					RoleARN:               "",
				},
				LogGroupName:                "",
				LogStreamName:               "",
				DimensionRollupOption:       "ZeroAndSingleDimensionRollup",
				OutputDestination:           "cloudwatch",
				ResourceToTelemetrySettings: resourcetotelemetry.Settings{Enabled: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id.String(), func(t *testing.T) {
			factory := NewFactory()
			cfg := factory.CreateDefaultConfig()

			sub, err := cm.Sub(tt.id.String())
			require.NoError(t, err)
			require.NoError(t, config.UnmarshalExporter(sub, cfg))

			assert.NoError(t, cfg.Validate())
			assert.Equal(t, tt.expected, cfg)
		})
	}
}

func TestConfigValidate(t *testing.T) {
	incorrectDescriptor := []MetricDescriptor{
		{metricName: ""},
		{unit: "Count", metricName: "apiserver_total", overwrite: true},
		{unit: "INVALID", metricName: "404"},
		{unit: "Megabytes", metricName: "memory_usage"},
	}
	cfg := &Config{
		ExporterSettings: config.NewExporterSettings(config.NewComponentIDWithName(typeStr, "1")),
		AWSSessionSettings: awsutil.AWSSessionSettings{
			RequestTimeoutSeconds: 30,
			MaxRetries:            1,
		},
		DimensionRollupOption:       "ZeroAndSingleDimensionRollup",
		ResourceToTelemetrySettings: resourcetotelemetry.Settings{Enabled: true},
		MetricDescriptors:           incorrectDescriptor,
		logger:                      zap.NewNop(),
	}
	assert.NoError(t, cfg.Validate())

	assert.Equal(t, 2, len(cfg.MetricDescriptors))
	assert.Equal(t, []MetricDescriptor{
		{unit: "Count", metricName: "apiserver_total", overwrite: true},
		{unit: "Megabytes", metricName: "memory_usage"},
	}, cfg.MetricDescriptors)
}
