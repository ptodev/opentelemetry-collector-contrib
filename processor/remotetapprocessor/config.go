// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"

import (
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configoptional"
)

// Config is the processor's configuration.
type Config struct {
	// Directory is where sessions are written until a client pulls them.
	// Required.
	Directory string `mapstructure:"directory"`
	// MaxDiskMiB is the total size of everything in Directory. While it's
	// reached, the tap copies nothing, until a client has pulled enough files.
	MaxDiskMiB int64 `mapstructure:"max_disk_mib"`
	// MaxFileMiB is the uncompressed size at which a file is closed and a new
	// one started. duckdb-otlp reads files of up to 100 MB after they're
	// decompressed, so it's at most maxFileMiB.
	MaxFileMiB int64 `mapstructure:"max_file_mib"`
	// MaxBufferMiB bounds the memory held by copies waiting to be written.
	// While it's full, the tap copies nothing, so a burst bigger than the
	// buffer loses records.
	MaxBufferMiB int64 `mapstructure:"max_buffer_mib"`
	// GRPC is the server clients pull from. Taps in the same collector with
	// the same endpoint share one server.
	GRPC configgrpc.ServerConfig `mapstructure:"grpc"`

	Traces  SignalConfig `mapstructure:"traces"`
	Logs    SignalConfig `mapstructure:"logs"`
	Metrics SignalConfig `mapstructure:"metrics"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// SignalConfig holds the rules for one signal. A signal without rules is never
// recorded.
type SignalConfig struct {
	Rules []RuleConfig `mapstructure:"rules"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// RuleConfig chooses records and their detail level. A record matches when it
// meets every condition in Where and falls within TracePercent. When several
// rules match, the highest detail wins.
type RuleConfig struct {
	// Where holds OTTL conditions, all of which must be true. Paths must start
	// with their context, such as span.name or resource.attributes["x"].
	Where []string `mapstructure:"where" json:"where,omitempty"`
	// TracePercent keeps this percentage of traces, chosen by hashing the
	// trace ID, so every instance keeps the same traces. Zero means no
	// sampling. Not allowed for metrics.
	TracePercent float64 `mapstructure:"trace_percent" json:"trace_percent,omitempty"`
	// Detail is sizes, keys, or full.
	Detail string `mapstructure:"detail" json:"detail"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// maxFileMiB keeps a decompressed file under duckdb-otlp's 100 MB limit.
const maxFileMiB = 95

// DefaultEndpoint is where clients pull from. It's only reachable from the
// collector's host, through a tunnel such as ssh -L or kubectl port-forward.
const DefaultEndpoint = "127.0.0.1:8866"

// Keepalive pings notice a dead connection within about 15 seconds. Clients
// ping every 10 seconds, so the server must allow that.
const (
	keepaliveTime    = 10 * time.Second
	keepaliveTimeout = 5 * time.Second
)

func createDefaultConfig() component.Config {
	server := configgrpc.NewDefaultServerConfig()
	server.NetAddr.Endpoint = DefaultEndpoint
	server.Keepalive = configoptional.Some(configgrpc.KeepaliveServerConfig{
		ServerParameters: configoptional.Some(configgrpc.KeepaliveServerParameters{
			Time:    keepaliveTime,
			Timeout: keepaliveTimeout,
		}),
		EnforcementPolicy: configoptional.Some(configgrpc.KeepaliveEnforcementPolicy{
			MinTime:             keepaliveTime / 2,
			PermitWithoutStream: true,
		}),
	})
	return &Config{
		MaxDiskMiB:   1024,
		MaxFileMiB:   64,
		MaxBufferMiB: 64,
		GRPC:         server,
	}
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Directory == "" {
		errs = append(errs, errors.New("directory is required"))
	}
	if c.MaxDiskMiB <= 0 {
		errs = append(errs, errors.New("max_disk_mib must be positive"))
	}
	if c.MaxFileMiB <= 0 || c.MaxFileMiB > maxFileMiB {
		errs = append(errs, fmt.Errorf("max_file_mib must be between 1 and %d", maxFileMiB))
	}
	if c.MaxBufferMiB <= 0 {
		errs = append(errs, errors.New("max_buffer_mib must be positive"))
	}
	if c.GRPC.NetAddr.Endpoint == "" {
		errs = append(errs, errors.New("grpc endpoint is required"))
	}
	for name, signal := range map[string]SignalConfig{"traces": c.Traces, "logs": c.Logs, "metrics": c.Metrics} {
		for i, r := range signal.Rules {
			if _, err := parseDetail(r.Detail); err != nil {
				errs = append(errs, fmt.Errorf("%s rule %d: %w", name, i, err))
			}
			if r.TracePercent < 0 || r.TracePercent > 100 {
				errs = append(errs, fmt.Errorf("%s rule %d: trace_percent must be between 0 and 100", name, i))
			}
			if name == "metrics" && r.TracePercent != 0 {
				errs = append(errs, fmt.Errorf("metrics rule %d: trace_percent isn't allowed, because data points have no trace ID", i))
			}
		}
	}
	return errors.Join(errs...)
}
