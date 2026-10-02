// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate make mdatagen

// Package remotetapprocessor can be positioned anywhere in a pipeline. It passes
// every batch on unchanged and, while a client is connected, copies a
// configurable slice of the telemetry into local OTLP files that the client
// pulls over gRPC.
package remotetapprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/remotetapprocessor"
