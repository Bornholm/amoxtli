package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/syncx"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

type WeightedIndexes map[*IdentifiedIndex]float64

type Index struct {
	queryTransformers   []QueryTransformer
	resultsTransformers []ResultsTransformer
	indexes             WeightedIndexes
}

// DeleteByID implements index.Index.
func (i *Index) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	_, err := fanOut(i.indexes, func(identified *IdentifiedIndex) (struct{}, error) {
		internalIndex := identified.Index()

		slog.DebugContext(ctx, "deleting indexed section", slog.String("indexType", fmt.Sprintf("%T", internalIndex)))

		return struct{}{}, errors.WithStack(internalIndex.DeleteByID(ctx, ids...))
	})

	return err
}

type indexSearchResults struct {
	Results []*index.SearchResult
	Index   *IdentifiedIndex
}

// DeleteBySource implements index.Index.
func (i *Index) DeleteBySource(ctx context.Context, source *url.URL) error {
	_, err := fanOut(i.indexes, func(identified *IdentifiedIndex) (struct{}, error) {
		return struct{}{}, errors.WithStack(identified.Index().DeleteBySource(ctx, source))
	})

	return err
}

// Index implements index.Index.
func (i *Index) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	opts := index.NewOptions(funcs...)

	var progress syncx.Map[index.Index, float32]

	count := len(i.indexes)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ctx = slogx.WithAttrs(ctx, slog.String("documentID", string(document.ID())))

	slog.DebugContext(ctx, "pipeline: indexing document", slog.Int("indexCount", count))

	_, err := fanOut(i.indexes, func(identified *IdentifiedIndex) (struct{}, error) {
		indexCtx := slogx.WithAttrs(ctx, slog.String("indexType", fmt.Sprintf("%T", identified.Index())))

		indexOptions := []index.OptionFunc{}

		if opts.OnProgress != nil {
			indexOptions = append(indexOptions, index.WithOnProgress(func(p float32) {
				progress.Store(identified.Index(), p)
				var globalProgress float32
				progress.Range(func(_ index.Index, p float32) bool {
					globalProgress += p
					return true
				})
				globalProgress /= float32(count)
				opts.OnProgress(globalProgress)
			}))

			defer opts.OnProgress(1)
		}

		slog.DebugContext(indexCtx, "pipeline: calling Index() on underlying index")
		if err := identified.Index().Index(indexCtx, document, indexOptions...); err != nil {
			err = errors.WithStack(err)
			slog.ErrorContext(indexCtx, "could not index document", slog.Any("error", err))
			// Indexing is all-or-nothing across the legs: once one has failed the
			// document will not be considered indexed, so the others stop early
			// rather than finish work that is about to be discarded.
			cancel()
			return struct{}{}, err
		}

		return struct{}{}, nil
	})
	if err != nil {
		return err
	}

	slog.DebugContext(ctx, "document indexed")

	return nil
}

// Search implements index.Index.
func (i *Index) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return i.search(ctx, query, nil, opts)
}

// SearchFiltered implements index.FilterableIndex by pushing the filter into
// every leg. Callers must detect the capability with index.AsFilterable, which
// consults Filterable below: a pipeline can only honour the contract when all
// of its legs can.
func (i *Index) SearchFiltered(ctx context.Context, query string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	if len(filter) > 0 && !i.Filterable() {
		return nil, errors.New("pipeline: cannot push a metadata filter down, some indexes do not support it")
	}

	return i.search(ctx, query, filter, opts)
}

// Filterable implements index.ConditionallyFilterable: the pipeline merges the
// legs' results, so it can only guarantee that everything it returns satisfies
// the filter when every leg applies it. With a single non-filterable leg the
// merged list would carry unfiltered results that the caller believes filtered
// — the silent corruption the capability exists to prevent — so the pipeline
// declines the capability and the caller falls back to filtering in Go.
func (i *Index) Filterable() bool {
	if len(i.indexes) == 0 {
		return false
	}

	for identified := range i.indexes {
		if _, ok := index.AsFilterable(identified.Index()); !ok {
			return false
		}
	}

	return true
}

func (i *Index) search(ctx context.Context, query string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	// Query transformation is per-index: each leg starts from the universally
	// transformed query, then gets the variant its own retrieval model calls
	// for — the semantic-only transformers (e.g. HyDE) for vector indexes, the
	// lexical-only ones (e.g. query translation) for full-text indexes. What
	// helps one leg routinely hurts the other, which is why neither variant
	// leaks into the other's query.
	baseQuery, err := i.transformBaseQuery(ctx, query, opts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Both variants are computed lazily and at most once, from inside the legs
	// that need them: each leg waits for its own LLM round-trip and not for the
	// other's, so the total latency is the max of the two branches rather than
	// their sum. A pipeline without any semantic index never invokes the
	// semantic variant — preserving the no-HyDE-without-semantic-index property
	// — and symmetrically for a pipeline without any lexical index.
	semanticQueryOnce := sync.OnceValues(func() (string, error) {
		return i.transformSemanticQuery(ctx, baseQuery, opts)
	})

	lexicalQueryOnce := sync.OnceValues(func() (string, error) {
		return i.transformLexicalQuery(ctx, baseQuery, opts)
	})

	maxResults := 3
	if opts.MaxResults != 0 {
		maxResults = opts.MaxResults
	}

	collections := make([]model.CollectionID, 0)
	if opts.Collections != nil {
		collections = opts.Collections
	}

	results, err := fanOut(i.indexes, func(identified *IdentifiedIndex) (*indexSearchResults, error) {
		indexCtx := slogx.WithAttrs(ctx, slog.String("index_type", fmt.Sprintf("%T", identified.Index())))

		variant := lexicalQueryOnce
		if index.IsSemantic(identified.Index()) {
			variant = semanticQueryOnce
		}

		indexQuery, err := variant()
		if err != nil {
			err = errors.WithStack(err)
			slog.ErrorContext(indexCtx, "could not transform query", slog.Any("error", err))
			return nil, err
		}

		legOpts := index.SearchOptions{
			MaxResults:  maxResults * 2,
			Collections: collections,
		}

		// Push the filter into the leg when it can apply it inside its own
		// query: its top-k is then k *matching* results, instead of a top-k
		// the filter may empty afterwards. Filterable above guarantees every
		// leg can, so there is no unfiltered leg to reconcile here.
		var legResults []*index.SearchResult
		if filterable, ok := index.AsFilterable(identified.Index()); ok && len(filter) > 0 {
			legResults, err = filterable.SearchFiltered(indexCtx, indexQuery, filter, legOpts)
		} else {
			legResults, err = identified.Index().Search(indexCtx, indexQuery, legOpts)
		}
		if err != nil {
			err = errors.WithStack(err)
			slog.ErrorContext(indexCtx, "could not search documents", slog.Any("error", err))
			return nil, err
		}

		slog.DebugContext(indexCtx, "found documents", slog.Int("total", len(legResults)))

		return &indexSearchResults{
			Results: legResults,
			Index:   identified,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	merged, err := i.mergeResults(results, maxResults)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	transformed, err := i.transformResults(ctx, query, merged, opts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return transformed, nil
}

// fanOut runs fn concurrently over every leg of the pipeline and returns the
// per-leg values, or the aggregation of the errors when at least one leg failed.
//
// It also converts a panic raised inside a leg into an error. A panic in a
// goroutine is otherwise unrecoverable by the caller and takes the whole process
// down, and — with the previous per-leg result channel — could leave the
// collecting loop waiting forever for a message the panicking goroutine never
// sent.
func fanOut[T any](indexes WeightedIndexes, fn func(identified *IdentifiedIndex) (T, error)) ([]T, error) {
	type outcome struct {
		value T
		err   error
	}

	outcomes := make([]outcome, len(indexes))

	var wg sync.WaitGroup
	wg.Add(len(indexes))

	slot := 0
	for identified := range indexes {
		go func(slot int, identified *IdentifiedIndex) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					outcomes[slot].err = errors.Errorf("pipeline: panic in index %T: %v", identified.Index(), r)
				}
			}()

			outcomes[slot].value, outcomes[slot].err = fn(identified)
		}(slot, identified)
		slot++
	}

	wg.Wait()

	// Aggregation happens after Wait, on this goroutine only: no lock is needed
	// on the accumulator, unlike the concurrent Add it replaces.
	aggregatedErr := NewAggregatedError()
	values := make([]T, 0, len(outcomes))

	for _, o := range outcomes {
		if o.err != nil {
			aggregatedErr.Add(o.err)
			continue
		}

		values = append(values, o.value)
	}

	if aggregatedErr.Len() > 0 {
		return nil, errors.WithStack(aggregatedErr.OrOnlyOne())
	}

	return values, nil
}

// transformBaseQuery applies the universal query transformers — those opting
// into neither marker. The result is the common input of both the lexical and
// the semantic variants.
func (i *Index) transformBaseQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	baseQuery := query
	var err error
	for _, t := range i.queryTransformers {
		if scopeOf(t) != scopeUniversal {
			continue
		}
		baseQuery, err = t.TransformQuery(ctx, baseQuery, opts)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}
	return baseQuery, nil
}

// transformLexicalQuery applies the lexical-only transformers (e.g. query
// translation) on top of the already base-transformed query. Like its semantic
// counterpart it is only invoked — lazily, from Search — when at least one
// lexical index needs it, so a purely semantic pipeline never pays for it.
func (i *Index) transformLexicalQuery(ctx context.Context, baseQuery string, opts index.SearchOptions) (string, error) {
	lexicalQuery := baseQuery
	var err error
	for _, t := range i.queryTransformers {
		if scopeOf(t) != scopeLexical {
			continue
		}
		lexicalQuery, err = t.TransformQuery(ctx, lexicalQuery, opts)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}
	return lexicalQuery, nil
}

// transformSemanticQuery applies the semantic-only transformers (e.g. HyDE, an
// LLM call) on top of the already base-transformed query. It is only invoked —
// lazily, from Search — when at least one semantic index needs it.
func (i *Index) transformSemanticQuery(ctx context.Context, baseQuery string, opts index.SearchOptions) (string, error) {
	semanticQuery := baseQuery
	var err error
	for _, t := range i.queryTransformers {
		if scopeOf(t) != scopeSemantic {
			continue
		}
		semanticQuery, err = t.TransformQuery(ctx, semanticQuery, opts)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}
	return semanticQuery, nil
}

func (i *Index) transformResults(ctx context.Context, query string, results []*index.SearchResult, opts index.SearchOptions) ([]*index.SearchResult, error) {
	var err error
	for _, t := range i.resultsTransformers {
		results, err = t.TransformResults(ctx, query, results, opts)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if emptyResults(results) {
			return []*index.SearchResult{}, nil
		}
	}

	return results, nil
}

// rrfK is the standard Reciprocal Rank Fusion smoothing constant. A larger k
// flattens the contribution of the top ranks; 60 is the value from the original
// RRF paper and the de-facto default.
const rrfK = 60

// mergeResults fuses the per-index result lists with Reciprocal Rank Fusion.
// Each index leg contributes to a source (and to its sections) an amount
// inversely proportional to the rank at which it appears in that leg, scaled by
// the leg's configured weight. Unlike the previous occurrence-count scoring,
// this is rank-sensitive: a result ranked first weighs more than one ranked
// tenth, and results corroborated by several indexes accumulate. The fused
// scores are exposed on the returned SearchResult (Score / SectionScores).
func (i *Index) mergeResults(indexResults []*indexSearchResults, maxResults int) ([]*index.SearchResult, error) {
	sourceScores := map[string]float64{}
	sectionScores := map[string]map[model.SectionID]float64{}

	for _, r := range indexResults {
		weight := i.indexes[r.Index]

		for rank, rr := range r.Results {
			source := rr.Source.String()
			sourceScores[source] += weight / float64(rrfK+rank+1)

			sections, exists := sectionScores[source]
			if !exists {
				sections = map[model.SectionID]float64{}
				sectionScores[source] = sections
			}

			for sectionRank, s := range rr.Sections {
				sections[s] += weight / float64(rrfK+sectionRank+1)
			}
		}
	}

	sources := make([]string, 0, len(sourceScores))
	for s := range sourceScores {
		sources = append(sources, s)
	}

	slices.SortFunc(sources, func(s1, s2 string) int {
		if c := cmp.Compare(sourceScores[s2], sourceScores[s1]); c != 0 {
			return c
		}
		return strings.Compare(s1, s2)
	})

	merged := make([]*index.SearchResult, 0, len(sources))

	for _, rawSource := range sources {
		source, err := url.Parse(rawSource)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		sections := sectionScores[rawSource]

		sectionIDs := make([]model.SectionID, 0, len(sections))
		for id := range sections {
			sectionIDs = append(sectionIDs, id)
		}

		slices.SortFunc(sectionIDs, func(id1, id2 model.SectionID) int {
			if c := cmp.Compare(sections[id2], sections[id1]); c != 0 {
				return c
			}
			return strings.Compare(string(id1), string(id2))
		})

		merged = append(merged, &index.SearchResult{
			Source:        source,
			Sections:      sectionIDs,
			Score:         sourceScores[rawSource],
			SectionScores: sections,
		})
	}

	if maxResults > 0 && len(merged) > maxResults {
		merged = merged[:maxResults]
	}

	return merged, nil
}

func NewIndex(indexes WeightedIndexes, funcs ...OptionFunc) *Index {
	opts := NewOptions(funcs...)

	// "Only lexical" and "only semantic" intersect to nothing, so a transformer
	// declaring both is silently applied nowhere. Say so once, at wiring time,
	// rather than leaving the author to wonder why their transformation never
	// runs.
	for _, t := range opts.QueryTransformers {
		if scopeOf(t) == scopeNone {
			slog.Warn("query transformer declares itself both semantic-only and lexical-only, it will never be applied",
				slog.String("transformer", fmt.Sprintf("%T", t)),
			)
		}
	}

	return &Index{
		queryTransformers:   opts.QueryTransformers,
		resultsTransformers: opts.ResultsTransformers,
		indexes:             indexes,
	}
}

var _ index.Index = &Index{}

func emptyResults(results []*index.SearchResult) bool {
	if len(results) == 0 {
		return true
	}

	for _, r := range results {
		if len(r.Sections) > 0 {
			return false
		}
	}

	return true
}
