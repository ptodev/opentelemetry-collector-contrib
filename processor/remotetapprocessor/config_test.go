// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
)

func TestConfigUnmarshal(t *testing.T) {
	conf := confmap.NewFromStringMap(map[string]any{
		"directory":    "/tmp/tap",
		"max_file_mib": 16,
		"traces": map[string]any{
			"rules": []any{
				map[string]any{"where": []any{`span.name == "checkout"`}, "detail": "full"},
				map[string]any{"trace_percent": 5, "detail": "keys"},
			},
		},
	})
	cfg := createDefaultConfig().(*Config)
	require.NoError(t, conf.Unmarshal(cfg))

	assert.Equal(t, "/tmp/tap", cfg.Directory)
	assert.EqualValues(t, 16, cfg.MaxFileMiB)
	assert.EqualValues(t, 1024, cfg.MaxDiskMiB, "default kept")
	assert.Equal(t, DefaultEndpoint, cfg.GRPC.NetAddr.Endpoint, "default kept")
	assert.Equal(t, []RuleConfig{
		{Where: []string{`span.name == "checkout"`}, Detail: "full"},
		{TracePercent: 5, Detail: "keys"},
	}, cfg.Traces.Rules)
	assert.NoError(t, cfg.Validate())
}

func TestConfigValidate(t *testing.T) {
	valid := func() *Config {
		c := createDefaultConfig().(*Config)
		c.Directory = "/tmp/tap"
		return c
	}
	tests := map[string]struct {
		change func(*Config)
		want   string
	}{
		"no directory": {func(c *Config) { c.Directory = "" }, "directory is required"},
		"no disk":      {func(c *Config) { c.MaxDiskMiB = 0 }, "max_disk_mib must be positive"},
		"file over DuckDB's limit": {
			func(c *Config) { c.MaxFileMiB = 96 },
			"max_file_mib must be between 1 and 95",
		},
		"no endpoint": {func(c *Config) { c.GRPC.NetAddr.Endpoint = "" }, "grpc endpoint is required"},
		"no buffer":   {func(c *Config) { c.MaxBufferMiB = 0 }, "max_buffer_mib must be positive"},
		"unknown detail": {
			func(c *Config) { c.Logs.Rules = []RuleConfig{{Detail: "everything"}} },
			`logs rule 0: detail must be sizes, keys, or full, got "everything"`,
		},
		"percent too high": {
			func(c *Config) { c.Traces.Rules = []RuleConfig{{TracePercent: 101, Detail: "full"}} },
			"traces rule 0: trace_percent must be between 0 and 100",
		},
		"metrics with trace percent": {
			func(c *Config) { c.Metrics.Rules = []RuleConfig{{TracePercent: 5, Detail: "full"}} },
			"metrics rule 0: trace_percent isn't allowed",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tt.change(c)
			err := c.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
	assert.NoError(t, valid().Validate())
}
