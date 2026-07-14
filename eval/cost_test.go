package eval_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The evaluation harness reports what each phase costs in LLM usage (calls,
// tokens, latency), alongside the quality metrics: quality numbers without
// their cost hide the trade-off every amoxtli stage is about. amoxtli is
// already instrumented (llmx.ObservableClient records into the global OTel
// meter); the harness only has to install an in-memory metrics pipeline and
// diff its counters around each phase.

var (
	costMeterOnce   sync.Once
	costMeterReader *sdkmetric.ManualReader
)

// llmCostMeter installs (once per process) a ManualReader-backed MeterProvider
// as the global OTel provider, so amoxtli's instruments record into it. It must
// run before the first LLM call of the process: telemetry.Metrics() binds its
// instruments to whatever global meter is installed at that moment.
func llmCostMeter() *sdkmetric.ManualReader {
	costMeterOnce.Do(func() {
		costMeterReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(costMeterReader)))
	})
	return costMeterReader
}

// llmCost is a snapshot of the cumulative LLM usage counters, broken down by
// operation (chat_completion, embeddings, ...).
type llmCost struct {
	calls    map[string]int64   // operation → calls
	tokens   map[string]int64   // operation + "/" + kind → tokens
	durSum   map[string]float64 // operation → total seconds
	durCount map[string]uint64  // operation → recorded calls
}

// collectLLMCost reads the current cumulative values from the reader.
func collectLLMCost(t *testing.T, reader *sdkmetric.ManualReader) llmCost {
	t.Helper()

	c := llmCost{
		calls:    map[string]int64{},
		tokens:   map[string]int64{},
		durSum:   map[string]float64{},
		durCount: map[string]uint64{},
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	op := func(set attribute.Set) string {
		if v, ok := set.Value(attribute.Key(telemetry.AttrOperation)); ok {
			return v.AsString()
		}
		return "unknown"
	}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch m.Name {
			case "amoxtli.llm.calls":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						c.calls[op(dp.Attributes)] += dp.Value
					}
				}
			case "amoxtli.llm.tokens":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						kind := "unknown"
						if v, ok := dp.Attributes.Value(attribute.Key(telemetry.AttrTokenKind)); ok {
							kind = v.AsString()
						}
						c.tokens[op(dp.Attributes)+"/"+kind] += dp.Value
					}
				}
			case "amoxtli.llm.duration":
				if hist, ok := m.Data.(metricdata.Histogram[float64]); ok {
					for _, dp := range hist.DataPoints {
						c.durSum[op(dp.Attributes)] += dp.Sum
						c.durCount[op(dp.Attributes)] += dp.Count
					}
				}
			}
		}
	}

	return c
}

// sub returns the delta cur − prev, i.e. what one phase consumed.
func (c llmCost) sub(prev llmCost) llmCost {
	d := llmCost{
		calls:    map[string]int64{},
		tokens:   map[string]int64{},
		durSum:   map[string]float64{},
		durCount: map[string]uint64{},
	}
	for k, v := range c.calls {
		if v -= prev.calls[k]; v != 0 {
			d.calls[k] = v
		}
	}
	for k, v := range c.tokens {
		if v -= prev.tokens[k]; v != 0 {
			d.tokens[k] = v
		}
	}
	for k, v := range c.durSum {
		d.durSum[k] = v - prev.durSum[k]
	}
	for k, v := range c.durCount {
		d.durCount[k] = v - prev.durCount[k]
	}
	return d
}

// logLLMCost logs one phase's LLM usage per operation: calls (normalised per
// query when queries > 0), tokens by kind and average call latency.
func logLLMCost(t *testing.T, phase string, cost llmCost, queries int) {
	t.Helper()

	if len(cost.calls) == 0 {
		t.Logf("LLM cost [%s]: none", phase)
		return
	}

	ops := make([]string, 0, len(cost.calls))
	for op := range cost.calls {
		ops = append(ops, op)
	}
	sort.Strings(ops)

	lines := make([]string, 0, len(ops))
	for _, op := range ops {
		line := fmt.Sprintf("  %-22s %6d calls", op, cost.calls[op])
		if queries > 0 {
			line += fmt.Sprintf(" (%.1f/query)", float64(cost.calls[op])/float64(queries))
		}
		for _, kind := range []string{"prompt", "completion"} {
			if tok := cost.tokens[op+"/"+kind]; tok > 0 {
				line += fmt.Sprintf(", %d %s tokens", tok, kind)
			}
		}
		if n := cost.durCount[op]; n > 0 {
			line += fmt.Sprintf(", avg %.2fs/call", cost.durSum[op]/float64(n))
		}
		lines = append(lines, line)
	}

	t.Logf("LLM cost [%s]:\n%s", phase, strings.Join(lines, "\n"))
}
