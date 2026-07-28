package telemetry

import (
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/metric"
)

// Instruments holds the metric instruments amoxtli records into. They are
// created once against the global meter; if instrument creation fails (which it
// should not with the no-op or a well-formed provider) the field is left nil
// and recording against it is silently skipped by the helpers below.
type Instruments struct {
	// LLMCalls counts LLM calls, tagged by operation.
	LLMCalls metric.Int64Counter
	// LLMDuration is the LLM call latency in seconds, tagged by operation.
	LLMDuration metric.Float64Histogram
	// LLMTokens counts tokens consumed, tagged by operation and token kind.
	LLMTokens metric.Int64Counter
	// SearchDuration is the search latency in seconds.
	SearchDuration metric.Float64Histogram
	// SearchResults counts results returned across searches.
	SearchResults metric.Int64Counter
	// VisionDescriptions counts image descriptions produced, tagged by
	// outcome.
	VisionDescriptions metric.Int64Counter
	// VisionDescriptionDuration is the image description latency in seconds.
	VisionDescriptionDuration metric.Float64Histogram
	// VisionCacheLookups counts description cache lookups, tagged by result
	// (hit or miss).
	VisionCacheLookups metric.Int64Counter
}

var (
	instrumentsOnce sync.Once
	instruments     *Instruments
)

// Metrics returns the shared, lazily-initialised metric instruments. The first
// call builds them against the global meter; subsequent calls return the same
// set. It never returns nil.
func Metrics() *Instruments {
	instrumentsOnce.Do(func() {
		instruments = newInstruments()
	})
	return instruments
}

func newInstruments() *Instruments {
	meter := Meter()
	in := &Instruments{}

	var err error
	if in.LLMCalls, err = meter.Int64Counter(
		"amoxtli.llm.calls",
		metric.WithDescription("Number of LLM calls issued by amoxtli"),
		metric.WithUnit("{call}"),
	); err != nil {
		logInstrumentError("amoxtli.llm.calls", err)
	}
	if in.LLMDuration, err = meter.Float64Histogram(
		"amoxtli.llm.duration",
		metric.WithDescription("LLM call latency"),
		metric.WithUnit("s"),
	); err != nil {
		logInstrumentError("amoxtli.llm.duration", err)
	}
	if in.LLMTokens, err = meter.Int64Counter(
		"amoxtli.llm.tokens",
		metric.WithDescription("Tokens consumed by LLM calls"),
		metric.WithUnit("{token}"),
	); err != nil {
		logInstrumentError("amoxtli.llm.tokens", err)
	}
	if in.SearchDuration, err = meter.Float64Histogram(
		"amoxtli.search.duration",
		metric.WithDescription("Search latency"),
		metric.WithUnit("s"),
	); err != nil {
		logInstrumentError("amoxtli.search.duration", err)
	}
	if in.SearchResults, err = meter.Int64Counter(
		"amoxtli.search.results",
		metric.WithDescription("Number of search results returned"),
		metric.WithUnit("{result}"),
	); err != nil {
		logInstrumentError("amoxtli.search.results", err)
	}

	if in.VisionDescriptions, err = meter.Int64Counter(
		"amoxtli.vision.descriptions",
		metric.WithDescription("Number of image descriptions produced"),
		metric.WithUnit("{description}"),
	); err != nil {
		logInstrumentError("amoxtli.vision.descriptions", err)
	}
	if in.VisionDescriptionDuration, err = meter.Float64Histogram(
		"amoxtli.vision.duration",
		metric.WithDescription("Image description latency"),
		metric.WithUnit("s"),
	); err != nil {
		logInstrumentError("amoxtli.vision.duration", err)
	}
	if in.VisionCacheLookups, err = meter.Int64Counter(
		"amoxtli.vision.cache.lookups",
		metric.WithDescription("Image description cache lookups"),
		metric.WithUnit("{lookup}"),
	); err != nil {
		logInstrumentError("amoxtli.vision.cache.lookups", err)
	}

	return in
}

func logInstrumentError(name string, err error) {
	slog.Warn("telemetry: could not create metric instrument", slog.String("name", name), slog.Any("error", err))
}
