// Copyright 2019, OpenTelemetry Authors
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

package sapmreceiver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/confmap/confmaptest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/external/splunk"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	require.NoError(t, err)

	tests := []struct {
		id       config.ComponentID
		expected config.Receiver
	}{
		{
			id:       config.NewComponentIDWithName(typeStr, ""),
			expected: createDefaultConfig(),
		},
		{
			id: config.NewComponentIDWithName(typeStr, "customname"),
			expected: &Config{
				ReceiverSettings: config.NewReceiverSettings(config.NewComponentID(typeStr)),
				HTTPServerSettings: confighttp.HTTPServerSettings{
					Endpoint: "0.0.0.0:7276",
				},
			},
		},
		{
			id: config.NewComponentIDWithName(typeStr, "tls"),
			expected: &Config{
				ReceiverSettings: config.NewReceiverSettings(config.NewComponentID(typeStr)),
				HTTPServerSettings: confighttp.HTTPServerSettings{
					Endpoint: ":7276",
					TLSSetting: &configtls.TLSServerSetting{
						TLSSetting: configtls.TLSSetting{
							CertFile: "/test.crt",
							KeyFile:  "/test.key",
						},
					},
				},
			},
		},
		{
			id: config.NewComponentIDWithName(typeStr, "passthrough"),
			expected: &Config{
				ReceiverSettings: config.NewReceiverSettings(config.NewComponentID(typeStr)),
				HTTPServerSettings: confighttp.HTTPServerSettings{
					Endpoint: ":7276",
				},
				AccessTokenPassthroughConfig: splunk.AccessTokenPassthroughConfig{
					AccessTokenPassthrough: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id.String(), func(t *testing.T) {
			factory := NewFactory()
			cfg := factory.CreateDefaultConfig()

			sub, err := cm.Sub(tt.id.String())
			require.NoError(t, err)
			require.NoError(t, config.UnmarshalReceiver(sub, cfg))

			assert.NoError(t, cfg.Validate())
			assert.Equal(t, tt.expected, cfg)
		})
	}
}
