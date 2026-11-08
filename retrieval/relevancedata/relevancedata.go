// Package relevancedata builds training pairs for a relevance model from a
// corpus already indexed by amoxtli, with an LLM as the labeler.
//
// For each sampled section, the LLM writes a search query the section answers,
// the query is searched like a user query would be, and the LLM judges, in a
// single call, which of the retrieved sections are relevant. The judged
// sections become (query, passage) pairs: the relevant ones are positives, the
// others hard negatives, since the search ranked them high. A query whose own
// section is not judged relevant is dropped: the LLM wrote a query that does
// not match its passage.
//
// The LLM is paid once, when the pairs are built; the relevance model trained
// on them then replaces it at search time.
package relevancedata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"

	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// Question names of the relevance model, shared with indecisrel.
const (
	QuestionRelevant = "pertinent"
)

// Pair is a labeled (query, passage) pair, written as an indecis example: the
// query is the context, the passage the text.
type Pair struct {
	Query   string          `json:"context"`
	Passage string          `json:"text"`
	Labels  map[string]any  `json:"labels"`
	Family  string          `json:"family"`
	Section model.SectionID `json:"section"`
}

// Relevant reports whether the pair is labeled relevant.
func (p Pair) Relevant() bool {
	v, _ := p.Labels[QuestionRelevant].(bool)
	return v
}

// WriteJSONL writes pairs as JSON lines.
func WriteJSONL(w io.Writer, pairs []Pair) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

// Options configures a Builder.
type Options struct {
	// Candidates is how many retrieved sections the LLM judges per query
	// (default 10).
	Candidates int
	// MaxSectionWords bounds the words of each section shown to the LLM and
	// written in the pairs (default retrieval.DefaultMaxSectionWords).
	MaxSectionWords int
	// Concurrency bounds the queries processed in parallel (default 4).
	Concurrency int
	// Seed makes the choice of query styles reproducible.
	Seed uint64
}

func (o Options) withDefaults() Options {
	if o.Candidates <= 0 {
		o.Candidates = 10
	}
	if o.MaxSectionWords <= 0 {
		o.MaxSectionWords = retrieval.DefaultMaxSectionWords
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	return o
}

// Stats counts what a Build did.
type Stats struct {
	Sections  int
	Dropped   int
	Positives int
	Negatives int
}

func (s Stats) String() string {
	return fmt.Sprintf("%d sections, %d queries dropped, %d positive pairs, %d negative pairs",
		s.Sections, s.Dropped, s.Positives, s.Negatives)
}

// Builder builds labeled pairs from sections of an indexed corpus.
type Builder struct {
	llm    llm.Client
	store  retrieval.SectionStore
	search retrieval.SearchFunc
	opts   Options
}

// NewBuilder returns a Builder that writes queries and labels pairs with
// client, and retrieves candidates with search.
func NewBuilder(client llm.Client, store retrieval.SectionStore, search retrieval.SearchFunc, opts Options) *Builder {
	return &Builder{llm: client, store: store, search: search, opts: opts.withDefaults()}
}

// Build processes each section: it writes a query, retrieves and labels the
// candidates. Sections are processed concurrently; the pairs of a section stay
// together, in the order of the candidates. A section whose processing fails
// is logged and skipped.
func (b *Builder) Build(ctx context.Context, sections []model.Section) ([]Pair, Stats, error) {
	results := make([][]Pair, len(sections))
	styles := queryStyles(len(sections), b.opts.Seed)

	var (
		mu    sync.Mutex
		stats = Stats{Sections: len(sections)}
		done  int
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.opts.Concurrency)
	for i, section := range sections {
		g.Go(func() error {
			pairs, err := b.buildOne(gctx, section, styles[i])
			if err != nil {
				if gctx.Err() != nil {
					return gctx.Err()
				}
				slog.WarnContext(gctx, "relevance pairs: section skipped",
					slog.String("section", string(section.ID())), slog.Any("error", err))
			}
			mu.Lock()
			defer mu.Unlock()
			results[i] = pairs
			done++
			if pairs == nil {
				stats.Dropped++
			}
			for _, p := range pairs {
				if p.Relevant() {
					stats.Positives++
				} else {
					stats.Negatives++
				}
			}
			if done%25 == 0 || done == len(sections) {
				slog.InfoContext(gctx, "relevance pairs progress", slog.Int("done", done), slog.Int("total", len(sections)))
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, stats, errors.WithStack(err)
	}

	var pairs []Pair
	for _, r := range results {
		pairs = append(pairs, r...)
	}
	return pairs, stats, nil
}

// buildOne returns the pairs of one section, nil when the query is dropped.
func (b *Builder) buildOne(ctx context.Context, section model.Section, style string) ([]Pair, error) {
	content, err := section.Content()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	passage := truncateWords(string(content), b.opts.MaxSectionWords)

	query, err := b.writeQuery(ctx, passage, style)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if query == "" {
		return nil, nil
	}

	results, err := b.search(ctx, query, b.opts.Candidates, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Candidates in rank order, the query's own section added if the search
	// missed it.
	ids := make([]model.SectionID, 0, b.opts.Candidates+1)
	seen := map[model.SectionID]bool{}
	for _, r := range results {
		for _, id := range r.Sections {
			if !seen[id] && len(ids) < b.opts.Candidates {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if !seen[section.ID()] {
		ids = append(ids, section.ID())
	}

	sections, err := b.store.GetSectionsByIDs(ctx, ids)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	candidates := make([]candidate, 0, len(ids))
	for _, id := range ids {
		s, ok := sections[id]
		if !ok {
			continue
		}
		c, err := s.Content()
		if err != nil {
			return nil, errors.WithStack(err)
		}
		candidates = append(candidates, candidate{id: id, text: truncateWords(string(c), b.opts.MaxSectionWords)})
	}

	relevant, err := b.label(ctx, query, candidates)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if !relevant[section.ID()] {
		return nil, nil
	}

	pairs := make([]Pair, 0, len(candidates))
	for _, c := range candidates {
		pairs = append(pairs, Pair{
			Query:   query,
			Passage: c.text,
			Labels:  map[string]any{QuestionRelevant: relevant[c.id]},
			Family:  string(section.ID()),
			Section: c.id,
		})
	}
	return pairs, nil
}

type candidate struct {
	id   model.SectionID
	text string
}

// Query styles: users search with questions, keywords or statements to check,
// and the model must handle all three.
var styleInstructions = map[string]string{
	"question": "Write a natural question, as a user would ask it, that the passage answers.",
	"keywords": "Write a short keyword query (2 to 6 words), as typed in a search box, that the passage answers.",
	"claim":    "Write a short factual statement that the passage supports or contradicts, as someone checking a claim would type it.",
}

func queryStyles(n int, seed uint64) []string {
	names := []string{"question", "keywords", "claim"}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	styles := make([]string, n)
	for i := range styles {
		styles[i] = names[rng.IntN(len(names))]
	}
	return styles
}

const queryPrompt = `You write search queries to evaluate a search engine.

Read the passage, then: %s

The query must be answerable from the passage, written in the language of the
passage, self-contained, and must not copy a long phrase from it. Do not
mention "the passage" or "the text".

Answer in strict JSON, without markdown fencing: {"query": "..."}`

func (b *Builder) writeQuery(ctx context.Context, passage, style string) (string, error) {
	system := fmt.Sprintf(queryPrompt, styleInstructions[style])
	seed, err := text.IntHash(system + passage)
	if err != nil {
		return "", errors.WithStack(err)
	}

	completion, err := b.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(llm.NewResponseSchema("Query", "A search query the passage answers",
			map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"query": map[string]any{"type": "string"}},
				"required":             []string{"query"},
				"additionalProperties": false,
			})),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, system),
			llm.NewMessage(llm.RoleUser, passage),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return "", errors.WithStack(err)
	}

	responses, err := llm.ParseJSON[struct {
		Query string `json:"query"`
	}](completion.Message())
	if err != nil {
		return "", errors.WithStack(err)
	}
	if len(responses) == 0 {
		return "", nil
	}
	return strings.TrimSpace(responses[0].Query), nil
}

const labelPrompt = `You are a strict relevance judge for a search engine.

## Input
- **Query**: a search query
- **Documents**: retrieved passages, each with an identifier

## Task
Select the documents that are relevant to the query: they answer it, or support
or contradict it when the query is a statement, or bring information needed to
answer it. A document that is only on the same topic, without the information
the query asks for, is NOT relevant.

## Output Format (strict JSON, no markdown fencing)
{"relevant": ["id1", "id2"]}`

func (b *Builder) label(ctx context.Context, query string, candidates []candidate) (map[model.SectionID]bool, error) {
	var sb strings.Builder
	sb.WriteString("## Query\n\n")
	sb.WriteString(query)
	sb.WriteString("\n\n## Documents\n\n")
	for _, c := range candidates {
		fmt.Fprintf(&sb, "### Document %s\n\n%s\n\n", c.id, c.text)
	}

	seed, err := text.IntHash(labelPrompt + sb.String())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	completion, err := b.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(llm.NewResponseSchema("Relevance", "The identifiers of the documents relevant to the query",
			map[string]any{
				"type": "object",
				"properties": map[string]any{"relevant": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
				}},
				"required":             []string{"relevant"},
				"additionalProperties": false,
			})),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, labelPrompt),
			llm.NewMessage(llm.RoleUser, sb.String()),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	responses, err := llm.ParseJSON[struct {
		Relevant []string `json:"relevant"`
	}](completion.Message())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	relevant := map[model.SectionID]bool{}
	for _, r := range responses {
		for _, id := range r.Relevant {
			relevant[model.SectionID(strings.TrimSpace(strings.TrimPrefix(id, "Document ")))] = true
		}
	}
	return relevant, nil
}

func truncateWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// SectionFilter tells whether a section is worth writing a query for.
type SectionFilter func(s model.Section, words int) bool

// MinWords keeps the sections with at least n words.
func MinWords(n int) SectionFilter {
	return func(_ model.Section, words int) bool { return words >= n }
}

// Sample draws n sections uniformly (reservoir sampling) among the sections
// produced by walk that pass the filter. walk calls its argument for every
// candidate section, typically every section of every stored document.
func Sample(walk func(func(model.Section) error) error, n int, keep SectionFilter, seed uint64) ([]model.Section, error) {
	rng := rand.New(rand.NewPCG(seed, seed^0xda942042e4dd58b5))
	sample := make([]model.Section, 0, n)
	seen := 0
	err := walk(func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}
		if keep != nil && !keep(s, len(strings.Fields(string(content)))) {
			return nil
		}
		seen++
		if len(sample) < n {
			sample = append(sample, s)
		} else if j := rng.IntN(seen); j < n {
			sample[j] = s
		}
		return nil
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return sample, nil
}

// Leaves keeps the sections without subsections, whose text is not repeated
// by another section.
func Leaves() SectionFilter {
	return func(s model.Section, _ int) bool { return len(s.Sections()) == 0 }
}

// All keeps the sections that pass every filter.
func All(filters ...SectionFilter) SectionFilter {
	return func(s model.Section, words int) bool {
		for _, f := range filters {
			if !f(s, words) {
				return false
			}
		}
		return true
	}
}
