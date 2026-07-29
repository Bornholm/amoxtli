package pipeline

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/lang"
	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/prompt"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

// defaultMaxTargetLanguages bounds how many languages a query is translated
// into. A long tail of minor languages would multiply the terms of the lexical
// query — diluting BM25's term statistics — for corpora too small to answer
// anything.
const defaultMaxTargetLanguages = 2

// TranslateQueryTransformer widens a lexical query with the translations of the
// user's question into the languages the corpus is actually written in.
//
// It exists because the lexical leg cannot cross a language barrier on its own:
// no analyzer will match "chien" against "dog", so a French question over an
// English corpus retrieves on the few terms the two languages happen to share.
// Measured on SciFact (see docs/performance-overview.md), asking in French
// costs the lexical leg 40% of its nDCG@10 — while a multilingual embedding
// model only loses 3%, which is why this is a LexicalQueryTransformer and never
// touches the semantic query.
//
// The original query is kept alongside the translations rather than replaced.
// For BM25 the concatenation acts as a disjunction of terms, so a document
// matching either wording is retrieved, and the transformation cannot lose the
// recall it is meant to add.
type TranslateQueryTransformer struct {
	llm            llm.Client
	languages      CollectionLanguageLister
	maxTargetLangs int
}

const defaultTranslatePromptTemplate = `
You are a translation engine for a document retrieval system. Translate the
user's search query into each of the following languages, given as ISO 639-1
codes: {{ .Languages }}.

Translate faithfully and idiomatically. Keep named entities, acronyms, numbers
and domain-specific technical terms as they are when they are not normally
translated. Do not answer the query, do not explain, do not add terms that are
not in it.

If the query is already written in one of the target languages, return it
unchanged for that language.

## Output Format (strict JSON, no markdown fencing)
{"translations": [{"lang": "en", "text": "translated query"}]}
`

// TransformQuery implements QueryTransformer.
func (t *TranslateQueryTransformer) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	targets, err := t.targetLanguages(ctx, query, opts)
	if err != nil {
		return "", errors.WithStack(err)
	}

	// Nothing to translate into: a monolingual corpus already queried in its
	// own language, an undetectable corpus, or no indexed language at all. In
	// every case the query goes through untouched and no LLM call is made.
	if len(targets) == 0 {
		return query, nil
	}

	systemPrompt, err := prompt.Template(defaultTranslatePromptTemplate, struct {
		Languages string
	}{
		Languages: strings.Join(targets, ", "),
	})
	if err != nil {
		return "", errors.WithStack(err)
	}

	// Seeding on the prompt and the query makes the completion deterministic,
	// hence cacheable by llmx.CachingClient: the same question asked twice
	// costs one LLM call, unlike HyDE whose value lies in its variety.
	seed, err := text.IntHash(systemPrompt + query)
	if err != nil {
		return "", errors.WithStack(err)
	}

	ctx = slogx.WithAttrs(ctx, slog.Int("seed", seed), slog.Any("target_languages", targets))

	completion, err := t.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(
			llm.NewResponseSchema(
				"Translations",
				"The search query translated into each requested language",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"translations": map[string]any{
							"type":        "array",
							"description": "One entry per requested language",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"lang": map[string]any{"type": "string", "description": "ISO 639-1 code"},
									"text": map[string]any{"type": "string", "description": "Translated query"},
								},
								"required":             []string{"lang", "text"},
								"additionalProperties": false,
							},
						},
					},
					"required":             []string{"translations"},
					"additionalProperties": false,
				},
			),
		),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, systemPrompt),
			llm.NewMessage(llm.RoleUser, query),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return "", errors.WithStack(err)
	}

	type translation struct {
		Lang string `json:"lang"`
		Text string `json:"text"`
	}

	type llmResponse struct {
		Translations []translation `json:"translations"`
	}

	responses, err := llm.ParseJSON[llmResponse](completion.Message())
	if err != nil {
		return "", errors.WithStack(err)
	}

	var sb strings.Builder
	sb.WriteString(query)

	// Deduplicate on the text, not on the language: a query already written in
	// one of the target languages comes back unchanged, and repeating it would
	// skew the term frequencies BM25 scores on.
	seen := map[string]struct{}{normalizeQuery(query): {}}

	for _, r := range responses {
		for _, tr := range r.Translations {
			translated := strings.TrimSpace(tr.Text)
			if translated == "" {
				continue
			}

			key := normalizeQuery(translated)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			sb.WriteString(" ")
			sb.WriteString(translated)
		}
	}

	widened := sb.String()

	slog.DebugContext(ctx, "translated query", slog.String("query", query), slog.String("widened", widened))

	return widened, nil
}

// targetLanguages returns the corpus languages the query should be translated
// into: the most represented ones, minus the language the query is already
// written in.
func (t *TranslateQueryTransformer) targetLanguages(ctx context.Context, query string, opts index.SearchOptions) ([]string, error) {
	corpus, err := t.languages.ListCollectionLanguages(ctx, opts.Collections)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Skipping the query's own language saves a round-trip on the common case
	// of a monolingual corpus queried in its language. The detection is only
	// trusted when it says so: a short query is often undecidable, and there
	// the cost of translating needlessly beats that of not translating at all.
	queryLang, reliable := lang.Detect(query)

	targets := make([]string, 0, len(corpus))
	for _, code := range corpus {
		if reliable && code == queryLang {
			continue
		}

		targets = append(targets, code)
	}

	// The cap applies to what is left to translate into, not to the corpus
	// inventory: on a corpus dominated by the query's own language, capping
	// first would spend the whole budget on the one language that needs no
	// translation and leave nothing to do.
	max := t.maxTargetLangs
	if max <= 0 {
		max = defaultMaxTargetLanguages
	}

	if len(targets) > max {
		targets = targets[:max]
	}

	return targets, nil
}

// normalizeQuery is the comparison form used to tell a translation apart from
// the original: case and surrounding space carry no meaning for a lexical
// index.
func normalizeQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

// LexicalOnly implements LexicalQueryTransformer: only a full-text index needs
// the translated terms. A multilingual embedding model already places both
// wordings in the same region of its space, so feeding it the translation would
// pay an LLM call to replace the user's question with a paraphrase.
func (t *TranslateQueryTransformer) LexicalOnly() bool { return true }

// TranslateQueryTransformerOptionFunc configures a TranslateQueryTransformer.
type TranslateQueryTransformerOptionFunc func(*TranslateQueryTransformer)

// WithMaxTargetLanguages bounds how many corpus languages a query is translated
// into (defaults to defaultMaxTargetLanguages when <= 0). Languages are taken
// in decreasing order of document count.
func WithMaxTargetLanguages(max int) TranslateQueryTransformerOptionFunc {
	return func(t *TranslateQueryTransformer) {
		t.maxTargetLangs = max
	}
}

func NewTranslateQueryTransformer(client llm.Client, languages CollectionLanguageLister, funcs ...TranslateQueryTransformerOptionFunc) *TranslateQueryTransformer {
	transformer := &TranslateQueryTransformer{
		llm:       client,
		languages: languages,
	}

	for _, fn := range funcs {
		fn(transformer)
	}

	return transformer
}

var _ LexicalQueryTransformer = &TranslateQueryTransformer{}
