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

package metrics // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor/external/metrics"

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoints"
)

type Processor struct {
	statements []*ottl.Statement[ottldatapoints.TransformContext]
}

func NewProcessor(statements []string, functions map[string]interface{}, settings component.TelemetrySettings) (*Processor, error) {
	ottlp := ottldatapoints.NewParser(functions, settings)
	parsedStatements, err := ottlp.ParseStatements(statements)
	if err != nil {
		return nil, err
	}
	return &Processor{
		statements: parsedStatements,
	}, nil
}

func (p *Processor) ProcessMetrics(_ context.Context, td pmetric.Metrics) (pmetric.Metrics, error) {
	for i := 0; i < td.ResourceMetrics().Len(); i++ {
		rmetrics := td.ResourceMetrics().At(i)
		for j := 0; j < rmetrics.ScopeMetrics().Len(); j++ {
			smetrics := rmetrics.ScopeMetrics().At(j)
			metrics := smetrics.Metrics()
			for k := 0; k < metrics.Len(); k++ {
				metric := metrics.At(k)
				var err error
				switch metric.Type() {
				case pmetric.MetricTypeSum:
					err = p.handleNumberDataPoints(metric.Sum().DataPoints(), metrics.At(k), metrics, smetrics.Scope(), rmetrics.Resource())
				case pmetric.MetricTypeGauge:
					err = p.handleNumberDataPoints(metric.Gauge().DataPoints(), metrics.At(k), metrics, smetrics.Scope(), rmetrics.Resource())
				case pmetric.MetricTypeHistogram:
					err = p.handleHistogramDataPoints(metric.Histogram().DataPoints(), metrics.At(k), metrics, smetrics.Scope(), rmetrics.Resource())
				case pmetric.MetricTypeExponentialHistogram:
					err = p.handleExponetialHistogramDataPoints(metric.ExponentialHistogram().DataPoints(), metrics.At(k), metrics, smetrics.Scope(), rmetrics.Resource())
				case pmetric.MetricTypeSummary:
					err = p.handleSummaryDataPoints(metric.Summary().DataPoints(), metrics.At(k), metrics, smetrics.Scope(), rmetrics.Resource())
				}
				if err != nil {
					return td, err
				}
			}
		}
	}
	return td, nil
}

func (p *Processor) handleNumberDataPoints(dps pmetric.NumberDataPointSlice, metric pmetric.Metric, metrics pmetric.MetricSlice, is pcommon.InstrumentationScope, resource pcommon.Resource) error {
	for i := 0; i < dps.Len(); i++ {
		ctx := ottldatapoints.NewTransformContext(dps.At(i), metric, metrics, is, resource)
		err := p.callFunctions(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) handleHistogramDataPoints(dps pmetric.HistogramDataPointSlice, metric pmetric.Metric, metrics pmetric.MetricSlice, is pcommon.InstrumentationScope, resource pcommon.Resource) error {
	for i := 0; i < dps.Len(); i++ {
		ctx := ottldatapoints.NewTransformContext(dps.At(i), metric, metrics, is, resource)
		err := p.callFunctions(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) handleExponetialHistogramDataPoints(dps pmetric.ExponentialHistogramDataPointSlice, metric pmetric.Metric, metrics pmetric.MetricSlice, is pcommon.InstrumentationScope, resource pcommon.Resource) error {
	for i := 0; i < dps.Len(); i++ {
		ctx := ottldatapoints.NewTransformContext(dps.At(i), metric, metrics, is, resource)
		err := p.callFunctions(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) handleSummaryDataPoints(dps pmetric.SummaryDataPointSlice, metric pmetric.Metric, metrics pmetric.MetricSlice, is pcommon.InstrumentationScope, resource pcommon.Resource) error {
	for i := 0; i < dps.Len(); i++ {
		ctx := ottldatapoints.NewTransformContext(dps.At(i), metric, metrics, is, resource)
		err := p.callFunctions(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) callFunctions(ctx ottldatapoints.TransformContext) error {
	for _, statement := range p.statements {
		_, _, err := statement.Execute(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}
