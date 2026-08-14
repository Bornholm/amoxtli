// Package telemetry wires amoxtli to OpenTelemetry. It exposes a shared tracer
// and meter under a single instrumentation scope, plus lazily-created metric
// instruments for the things worth watching in a RAG library: search latency,
// LLM cost (call count, latency, token usage) and image description volume
// (descriptions produced, cache hits, failures).
//
// Instrumentation is always on but cheap: when the process has not installed a
// TracerProvider/MeterProvider, OpenTelemetry's global no-op providers are used
// and every span/measurement is discarded at near-zero cost. A consumer opts
// into real telemetry simply by installing the OTel SDK providers in their
// program; nothing in amoxtli needs to change.
package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope reported on every span and metric.
const ScopeName = "github.com/bornholm/amoxtli"

// Attribute keys used across amoxtli spans and metrics.
const (
	// AttrOperation labels an LLM operation (chat_completion, embeddings, ...).
	AttrOperation = "amoxtli.llm.operation"
	// AttrTokenKind distinguishes prompt vs. completion tokens on the token
	// counter.
	AttrTokenKind = "amoxtli.llm.token_kind"
	// AttrQueryLength is the character length of a search query (queries
	// themselves are not recorded, to avoid leaking user content).
	AttrQueryLength = "amoxtli.search.query_length"
	// AttrResultCount is the number of results returned by a search.
	AttrResultCount = "amoxtli.search.result_count"
	// AttrCandidateCount is the number of candidates a search stage produced,
	// recorded on each stage span so the funnel from retrieval to the returned
	// page can be read directly off a trace.
	AttrCandidateCount = "amoxtli.search.candidate_count"
	// AttrIndexType is the Go type of the index backing a retrieval leg.
	AttrIndexType = "amoxtli.index.type"
	// AttrIndexSemantic reports whether a retrieval leg is a vector index.
	AttrIndexSemantic = "amoxtli.index.semantic"
	// AttrTransformerType is the Go type of a query or results transformer.
	AttrTransformerType = "amoxtli.pipeline.transformer"
	// AttrSectionCount is the number of sections handled by a stage (hydrated,
	// judged, reranked).
	AttrSectionCount = "amoxtli.search.section_count"
	// AttrVisionOutcome labels how an image description ended: "ok",
	// "error" or "rejected" (refused before any call, e.g. an oversized
	// image).
	AttrVisionOutcome = "amoxtli.vision.outcome"
	// AttrVisionCacheResult labels a description cache lookup: "hit" or
	// "miss".
	AttrVisionCacheResult = "amoxtli.vision.cache_result"
)

// Values of AttrVisionOutcome and AttrVisionCacheResult.
const (
	VisionOutcomeOK       = "ok"
	VisionOutcomeError    = "error"
	VisionOutcomeRejected = "rejected"

	VisionCacheHit  = "hit"
	VisionCacheMiss = "miss"
)

// Tracer returns amoxtli's shared tracer from the global provider.
func Tracer() trace.Tracer {
	return otel.Tracer(ScopeName)
}

// Meter returns amoxtli's shared meter from the global provider.
func Meter() metric.Meter {
	return otel.Meter(ScopeName)
}
