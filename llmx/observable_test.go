package llmx

import (
	"context"
	"testing"

	"github.com/bornholm/genai/llm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// stubClient is a minimal llm.Client returning canned usage.
type stubClient struct {
	err error
}

func (s *stubClient) ChatCompletion(_ context.Context, _ ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, "hi"),
		llm.NewChatCompletionUsage(10, 4, 14),
	), nil
}

func (s *stubClient) ChatCompletionStream(_ context.Context, _ ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk)
	close(ch)
	return ch, s.err
}

func (s *stubClient) Embeddings(_ context.Context, inputs []string, _ ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return stubEmbeddings{prompt: 7, total: 7}, nil
}

type stubEmbeddings struct {
	prompt, total int64
}

func (e stubEmbeddings) Embeddings() [][]float64 { return [][]float64{{0.1, 0.2}} }
func (e stubEmbeddings) Usage() llm.EmbeddingsUsage {
	return llm.NewEmbeddingsUsage(e.prompt, e.total)
}

func TestObservableClientRecordsSpansAndMetrics(t *testing.T) {
	// In-memory SDK providers registered globally; the observable client reads
	// the global tracer/meter, so ordering is tolerant thanks to OTel's
	// delegating globals.
	reader := metric.NewManualReader()
	otel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))

	spanRecorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))

	client := NewObservableClient(&stubClient{})
	ctx := context.Background()

	if _, err := client.ChatCompletion(ctx); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if _, err := client.Embeddings(ctx, []string{"hello"}); err != nil {
		t.Fatalf("Embeddings: %v", err)
	}

	// Spans.
	spans := spanRecorder.Ended()
	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name()] = true
	}
	if !names["llm.chat_completion"] || !names["llm.embeddings"] {
		t.Errorf("missing expected spans, got %v", names)
	}

	// Metrics: at least the call counter and token counter must be present.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = true
		}
	}
	for _, want := range []string{"amoxtli.llm.calls", "amoxtli.llm.duration", "amoxtli.llm.tokens"} {
		if !found[want] {
			t.Errorf("missing metric %q (got %v)", want, found)
		}
	}
}

var _ llm.Client = &stubClient{}
