// Package vision turns images into searchable text: a vision LLM describes an
// image, and the resulting title/description/transcription is indexed as
// ordinary markdown by every backend (full-text, vector, metadata filtering).
//
// The package is deliberately independent of the ingestion pipeline: it is
// used by convert/vision to index standalone image files, and by the markdown
// enrichment of embedded images. Callers pass an llm.Client already decorated
// with llmx.NewRetryClient (retry + rate limit), exactly as the HyDE and Judge
// retrieval stages do.
package vision

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bornholm/amoxtli/telemetry"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PromptVersion identifies the default prompt. It participates in the cache
// key (see CachingDescriber): bump it whenever DefaultPrompt changes, so
// descriptions produced by an older prompt are not served from the cache.
const PromptVersion = "1"

// DefaultMaxImageBytes bounds, by default, the size of an image submitted to
// the vision model. Providers reject oversized payloads anyway, and the base64
// encoding inflates the request by a third.
const DefaultMaxImageBytes int64 = 10 << 20 // 10 MiB

// ErrImageTooLarge is returned when an image exceeds the configured size
// limit and could not be shrunk below it (see Shrink). It is returned *before*
// any call to the model.
var ErrImageTooLarge = errors.New("image too large")

// ErrUnsupportedImageFormat is returned when the media type of an image is not
// one the vision providers accept. Like ErrImageTooLarge, it is returned
// *before* any call to the model: the provider client rejects such an image
// while building its request parameters, and reports it as a generic
// "unavailable" error that is indistinguishable from a transient outage — so
// it would otherwise be retried, pointlessly, on every image.
var ErrUnsupportedImageFormat = errors.New("unsupported image format")

// SupportedMimeTypes are the media types accepted by the mainstream vision
// providers (and the only ones the OpenAI-compatible client of genai lets
// through). It mirrors convert/vision.DefaultExtensions.
var SupportedMimeTypes = []string{
	"image/png",
	"image/jpeg",
	"image/webp",
	"image/gif",
}

// IsSupportedMimeType reports whether a vision provider accepts mimeType. Any
// media type parameter (`; charset=...`, as returned by http.DetectContentType)
// is ignored, and the comparison is case-insensitive.
func IsSupportedMimeType(mimeType string) bool {
	return slices.Contains(SupportedMimeTypes, normalizeMimeType(mimeType))
}

// normalizeMimeType strips the parameters of a media type and lowercases it.
// An unparsable value is returned lowercased, and will simply not match any
// supported type.
func normalizeMimeType(mimeType string) string {
	if media, _, err := mime.ParseMediaType(mimeType); err == nil {
		return media
	}

	return strings.ToLower(strings.TrimSpace(mimeType))
}

// DefaultPrompt asks for a dense, factual description. The description is what
// gets indexed and searched, so it must be rich in the vocabulary a user would
// actually type: named entities, object names, chart values, UI labels.
//
// It is deliberately terse, and the detailed field requirements live in
// descriptionSchema instead. Spelling them out here made an OCR-specialized
// model (glm-ocr) — whose instinct is to transcribe whatever text it is handed
// — copy the instructions themselves into the answer, which would then have
// been indexed verbatim for every image. Keep any change to this prompt short
// and free of enumerations, and re-run the vision integration test: it asserts
// that no fragment of the prompt comes back in the answer.
const DefaultPrompt = `Describe this image so that a search engine can find it later.

Name it, say what it shows, and transcribe every word written in it.
Stick to what is visible. Use the language of the image, or English.`

// Description is the structured result of describing an image.
type Description struct {
	// Title is a short, one-line title for the image.
	Title string `json:"title"`
	// Description is the detailed markdown description: content, context,
	// notable elements, data of charts and diagrams.
	Description string `json:"description"`
	// Text is the text visible in the image, transcribed verbatim (implicit
	// OCR). Empty when the image carries no text.
	Text string `json:"text"`
}

// IsEmpty reports whether the description carries no usable text at all.
func (d *Description) IsEmpty() bool {
	return d == nil || (d.Title == "" && d.Description == "" && d.Text == "")
}

// Describer produces a textual description of an image.
type Describer interface {
	Describe(ctx context.Context, mimeType string, data []byte) (*Description, error)
}

// Options configures an LLMDescriber.
type Options struct {
	Prompt string
	// MaxImageBytes is the largest image submitted to the model; a larger one
	// is re-encoded to fit (see Shrink).
	MaxImageBytes int64
	// MaxSourceBytes is the largest image Describe accepts at all: between
	// MaxImageBytes and this limit an image is shrunk, above it Describe fails
	// with ErrImageTooLarge without decoding it.
	MaxSourceBytes int64
	// StructuredOutput requests a JSON response matching Description. Disable
	// it for providers that reject a response schema; the whole reply then
	// lands in Description.
	StructuredOutput bool
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		Prompt:           DefaultPrompt,
		MaxImageBytes:    DefaultMaxImageBytes,
		MaxSourceBytes:   DefaultMaxSourceBytes,
		StructuredOutput: true,
	}

	for _, fn := range funcs {
		fn(opts)
	}

	// A source limit below the model limit would reject images that need no
	// shrinking at all: a configuration raising only MaxImageBytes keeps its
	// meaning.
	opts.MaxSourceBytes = max(opts.MaxSourceBytes, opts.MaxImageBytes)

	return opts
}

// WithPrompt replaces the default description prompt. A custom prompt must be
// reflected in the cache namespace (see Namespace).
func WithPrompt(prompt string) OptionFunc {
	return func(opts *Options) {
		if prompt != "" {
			opts.Prompt = prompt
		}
	}
}

// WithMaxImageBytes bounds the size of an accepted image; <= 0 keeps the
// default (DefaultMaxImageBytes).
func WithMaxImageBytes(maxBytes int64) OptionFunc {
	return func(opts *Options) {
		if maxBytes > 0 {
			opts.MaxImageBytes = maxBytes
		}
	}
}

// WithMaxSourceBytes bounds the size of an image accepted for shrinking; <= 0
// keeps the default (DefaultMaxSourceBytes). It is always raised to at least
// MaxImageBytes.
func WithMaxSourceBytes(maxBytes int64) OptionFunc {
	return func(opts *Options) {
		if maxBytes > 0 {
			opts.MaxSourceBytes = maxBytes
		}
	}
}

// WithStructuredOutput toggles the JSON response schema.
func WithStructuredOutput(enabled bool) OptionFunc {
	return func(opts *Options) {
		opts.StructuredOutput = enabled
	}
}

// LLMDescriber describes images with a vision-capable chat model.
type LLMDescriber struct {
	client llm.Client
	opts   *Options
}

// NewLLMDescriber builds a Describer on top of client, which must be a
// vision-capable chat model and is expected to be already decorated with
// llmx.NewRetryClient by the caller.
func NewLLMDescriber(client llm.Client, funcs ...OptionFunc) *LLMDescriber {
	return &LLMDescriber{
		client: client,
		opts:   NewOptions(funcs...),
	}
}

// MaxImageBytes reports the configured image size limit, so callers can bound
// their reads before handing over the bytes.
func (d *LLMDescriber) MaxImageBytes() int64 {
	return d.opts.MaxImageBytes
}

// MaxSourceBytes reports the largest image Describe accepts before shrinking,
// so callers can bound their reads accordingly.
func (d *LLMDescriber) MaxSourceBytes() int64 {
	return d.opts.MaxSourceBytes
}

// Prompt reports the effective description prompt (used to derive the cache
// namespace).
func (d *LLMDescriber) Prompt() string {
	return d.opts.Prompt
}

// descriptionSchema is the JSON schema mirroring Description. It carries the
// detailed field requirements that DefaultPrompt deliberately leaves out: a
// schema is a contract the provider applies, not text the model is tempted to
// transcribe back (see the comment on DefaultPrompt).
var descriptionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title": map[string]any{
			"type":        "string",
			"description": "Short specific title naming the image, on a single line, without trailing punctuation",
		},
		"description": map[string]any{
			"type":        "string",
			"description": "Dense factual description of what the image contains: objects, people, places, brands, technologies, UI elements; for a chart, diagram, table or screenshot, its structure and its values",
		},
		"text": map[string]any{
			"type":        "string",
			"description": "Text visible in the image, transcribed verbatim and in reading order (empty if none)",
		},
	},
	"required":             []string{"title", "description", "text"},
	"additionalProperties": false,
}

// Describe implements Describer.
func (d *LLMDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*Description, error) {
	if len(data) == 0 {
		recordOutcome(ctx, telemetry.VisionOutcomeRejected)
		return nil, errors.New("empty image data")
	}

	if int64(len(data)) > d.opts.MaxSourceBytes {
		recordOutcome(ctx, telemetry.VisionOutcomeRejected)
		return nil, errors.Wrapf(ErrImageTooLarge, "image is %d bytes, limit is %d", len(data), d.opts.MaxSourceBytes)
	}

	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	mimeType = normalizeMimeType(mimeType)

	if !IsSupportedMimeType(mimeType) {
		recordOutcome(ctx, telemetry.VisionOutcomeRejected)
		return nil, errors.Wrapf(ErrUnsupportedImageFormat, "media type '%s' (supported: %s)", mimeType, strings.Join(SupportedMimeTypes, ", "))
	}

	// An image above the model limit is re-encoded rather than refused: a
	// lossless screenshot is heavy because of its format, not because of its
	// content, and its description is exactly what makes it searchable.
	if int64(len(data)) > d.opts.MaxImageBytes {
		shrunkType, shrunk, err := Shrink(mimeType, data, d.opts.MaxImageBytes)
		if err != nil {
			recordOutcome(ctx, telemetry.VisionOutcomeRejected)
			return nil, errors.Wrapf(ErrImageTooLarge, "image is %d bytes, limit is %d, and could not be shrunk: %s", len(data), d.opts.MaxImageBytes, err)
		}

		slog.DebugContext(ctx, "shrunk oversized image before describing it",
			slog.Int("sourceBytes", len(data)),
			slog.Int("shrunkBytes", len(shrunk)),
			slog.String("sourceMimeType", mimeType),
			slog.String("shrunkMimeType", shrunkType),
		)

		mimeType, data = shrunkType, shrunk
	}

	attachment, err := llm.NewImageAttachment(mimeType, base64.StdEncoding.EncodeToString(data), false)
	if err != nil {
		recordOutcome(ctx, telemetry.VisionOutcomeRejected)
		return nil, errors.Wrapf(err, "could not build image attachment (mime type '%s')", mimeType)
	}

	funcs := []llm.ChatCompletionOptionFunc{
		llm.WithMessages(
			llm.NewMessageWithAttachments(llm.RoleUser, d.opts.Prompt, attachment),
		),
		llm.WithTemperature(0),
	}

	if d.opts.StructuredOutput {
		funcs = append(funcs, llm.WithJSONResponse(
			llm.NewResponseSchema(
				"ImageDescription",
				"Title, detailed description and verbatim transcription of an image",
				descriptionSchema,
			),
		))
	}

	start := time.Now()

	completion, err := d.client.ChatCompletion(ctx, funcs...)

	recordDuration(ctx, time.Since(start))

	if err != nil {
		recordOutcome(ctx, telemetry.VisionOutcomeError)
		return nil, errors.WithStack(err)
	}

	message := completion.Message()
	if message == nil {
		recordOutcome(ctx, telemetry.VisionOutcomeError)
		return nil, errors.New("vision model returned no message")
	}

	recordOutcome(ctx, telemetry.VisionOutcomeOK)

	return parseDescription(message), nil
}

// recordOutcome counts one description attempt. Instruments are no-ops unless
// the process installed an OTel MeterProvider, so this stays free by default.
func recordOutcome(ctx context.Context, outcome string) {
	if counter := telemetry.Metrics().VisionDescriptions; counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.AttrVisionOutcome, outcome)))
	}
}

// recordDuration records the latency of one call to the vision model. Only the
// calls that reached the provider are timed: a cache hit or a rejected image
// would otherwise flatten the distribution.
func recordDuration(ctx context.Context, elapsed time.Duration) {
	if histogram := telemetry.Metrics().VisionDescriptionDuration; histogram != nil {
		histogram.Record(ctx, elapsed.Seconds())
	}
}

// parseDescription extracts the structured description from the model reply,
// falling back to the raw text when the provider ignored the schema (or was
// asked for plain text): losing the field split is acceptable, losing the
// description entirely is not.
func parseDescription(message llm.Message) *Description {
	if descriptions, err := llm.ParseJSON[Description](message); err == nil && len(descriptions) > 0 {
		desc := descriptions[0]
		desc.Title = normalizeLine(desc.Title)
		desc.Description = strings.TrimSpace(desc.Description)
		desc.Text = strings.TrimSpace(desc.Text)

		demoteOverlongTitle(&desc)

		if !desc.IsEmpty() {
			return &desc
		}
	}

	return &Description{
		Description: strings.TrimSpace(message.Content()),
	}
}

// MaxTitleRunes is the length beyond which a "title" is not one. The title
// becomes the heading of the emitted markdown, so a model answering with a
// whole paragraph — or with the instructions it was given — must not turn it
// into a monstrous `#` line.
const MaxTitleRunes = 120

// demoteOverlongTitle moves an implausibly long title into the description
// rather than dropping it: the model may have put real content there, but it
// is prose, not a heading.
func demoteOverlongTitle(desc *Description) {
	if len([]rune(desc.Title)) <= MaxTitleRunes {
		return
	}

	if !strings.Contains(desc.Description, desc.Title) {
		desc.Description = strings.TrimSpace(desc.Title + "\n\n" + desc.Description)
	}

	desc.Title = ""
}

// normalizeLine collapses a value meant to sit on a single markdown line.
func normalizeLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// Namespace derives a cache namespace identifying both the vision model and
// the prompt it was given: two descriptions may only share a cache entry when
// both match. Pass it to NewCachingDescriber.
func Namespace(model, prompt string) string {
	if prompt == "" || prompt == DefaultPrompt {
		return model
	}

	sum := sha256.Sum256([]byte(prompt))

	return model + "#" + hex.EncodeToString(sum[:])[:12]
}

var _ Describer = &LLMDescriber{}
