package llmx

import (
	"context"
	"time"

	"github.com/bornholm/amoxtli/telemetry"
	"github.com/bornholm/genai/llm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ObservableClient decorates an llm.Client with OpenTelemetry spans and metrics
// (call count, latency, token usage). It is a thin wrapper: when no OTel
// provider is installed the instrumentation is a no-op. Compose it with
// RetryClient as needed (e.g. observe the retried client to count every
// attempt's latency, or wrap the outside to measure the logical call).
type ObservableClient struct {
	inner llm.Client
}

// NewObservableClient wraps client so its calls emit spans and metrics under the
// amoxtli instrumentation scope.
func NewObservableClient(client llm.Client) *ObservableClient {
	return &ObservableClient{inner: client}
}

// ChatCompletion implements llm.ChatCompletionClient with instrumentation.
func (c *ObservableClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	ctx, end := c.startCall(ctx, "chat_completion")

	res, err := c.inner.ChatCompletion(ctx, funcs...)

	var usage tokenRecorder
	if err == nil && res != nil {
		u := res.Usage()
		usage = tokenUsage{prompt: u.PromptTokens(), total: u.TotalTokens()}
	}
	end(ctx, "chat_completion", err, usage)

	return res, err
}

// ChatCompletionStream implements llm.ChatCompletionStreamingClient. Only the
// call opening the stream is instrumented (span + latency); token usage is not
// available until the stream completes and is therefore not recorded here.
func (c *ObservableClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	ctx, end := c.startCall(ctx, "chat_completion_stream")
	ch, err := c.inner.ChatCompletionStream(ctx, funcs...)
	end(ctx, "chat_completion_stream", err, nil)
	return ch, err
}

// Embeddings implements llm.EmbeddingsClient with instrumentation.
func (c *ObservableClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	ctx, end := c.startCall(ctx, "embeddings")

	res, err := c.inner.Embeddings(ctx, inputs, funcs...)

	// EmbeddingsUsage exposes prompt/total tokens; adapt it to the shared
	// token-recording helper via embeddingsUsage.
	var usage tokenRecorder
	if err == nil && res != nil {
		u := res.Usage()
		usage = tokenUsage{prompt: u.PromptTokens(), total: u.TotalTokens()}
	}
	end(ctx, "embeddings", err, usage)

	return res, err
}

// startCall opens a span for op and returns the context carrying it together
// with an end function to be deferred/called once the call returns.
func (c *ObservableClient) startCall(ctx context.Context, op string) (context.Context, func(context.Context, string, error, tokenRecorder)) {
	start := time.Now()
	ctx, span := telemetry.Tracer().Start(ctx, "llm."+op,
		trace.WithAttributes(attribute.String(telemetry.AttrOperation, op)),
	)

	return ctx, func(ctx context.Context, op string, err error, usage tokenRecorder) {
		elapsed := time.Since(start).Seconds()
		in := telemetry.Metrics()
		attrs := metric.WithAttributes(attribute.String(telemetry.AttrOperation, op))

		if in.LLMCalls != nil {
			in.LLMCalls.Add(ctx, 1, attrs)
		}
		if in.LLMDuration != nil {
			in.LLMDuration.Record(ctx, elapsed, attrs)
		}
		if usage != nil {
			usage.record(ctx, op, in)
		}

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// tokenRecorder abstracts the two usage shapes (chat vs. embeddings) so the end
// helper can record their token counts uniformly.
type tokenRecorder interface {
	record(ctx context.Context, op string, in *telemetry.Instruments)
}

// tokenUsage records prompt/total tokens (embeddings have no completion split).
type tokenUsage struct {
	prompt, total int64
}

func (u tokenUsage) record(ctx context.Context, op string, in *telemetry.Instruments) {
	if in.LLMTokens == nil {
		return
	}
	if u.prompt > 0 {
		in.LLMTokens.Add(ctx, u.prompt, metric.WithAttributes(
			attribute.String(telemetry.AttrOperation, op),
			attribute.String(telemetry.AttrTokenKind, "prompt"),
		))
	}
	// Derive completion tokens when total exceeds prompt (chat responses).
	if completion := u.total - u.prompt; completion > 0 {
		in.LLMTokens.Add(ctx, completion, metric.WithAttributes(
			attribute.String(telemetry.AttrOperation, op),
			attribute.String(telemetry.AttrTokenKind, "completion"),
		))
	}
}

var _ tokenRecorder = tokenUsage{}
var _ llm.Client = &ObservableClient{}
