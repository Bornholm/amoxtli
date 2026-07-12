package pipeline

import (
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
	count := len(i.indexes)
	errs := make(chan error, count)
	defer close(errs)

	var wg sync.WaitGroup

	wg.Add(count)

	aggregatedErr := NewAggregatedError()

	for identified := range i.indexes {
		go func(identified *IdentifiedIndex) {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						aggregatedErr.Add(errors.WithStack(err))
					} else {
						panic(r)
					}
				}
			}()
			defer wg.Done()

			internalIndex := identified.Index()

			slog.DebugContext(ctx, "deleting indexed section", slog.String("indexType", fmt.Sprintf("%T", internalIndex)))

			if err := internalIndex.DeleteByID(ctx, ids...); err != nil {
				errs <- errors.WithStack(err)
				return
			}

			errs <- nil
		}(identified)
	}

	wg.Wait()

	idx := 0

	for e := range errs {
		if e != nil {
			aggregatedErr.Add(e)
		}

		if idx >= count-1 {
			break
		}

		idx++
	}

	if aggregatedErr.Len() > 0 {
		return errors.WithStack(aggregatedErr.OrOnlyOne())
	}

	return nil
}

type indexSearchResults struct {
	Results []*index.SearchResult
	Index   *IdentifiedIndex
}

// DeleteBySource implements index.Index.
func (i *Index) DeleteBySource(ctx context.Context, source *url.URL) error {
	count := len(i.indexes)
	errs := make(chan error, count)
	defer close(errs)

	var wg sync.WaitGroup

	wg.Add(count)

	aggregatedErr := NewAggregatedError()

	for identified := range i.indexes {
		go func(identified *IdentifiedIndex) {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						aggregatedErr.Add(errors.WithStack(err))
					} else {
						panic(r)
					}
				}
			}()
			defer wg.Done()

			if err := identified.Index().DeleteBySource(ctx, source); err != nil {
				errs <- errors.WithStack(err)
				return
			}

			errs <- nil
		}(identified)
	}

	wg.Wait()

	idx := 0

	for e := range errs {
		if e != nil {
			aggregatedErr.Add(e)
		}

		if idx >= count-1 {
			break
		}

		idx++
	}

	if aggregatedErr.Len() > 0 {
		return errors.WithStack(aggregatedErr.OrOnlyOne())
	}

	return nil
}

// Index implements index.Index.
func (i *Index) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	opts := index.NewOptions(funcs...)

	var progress syncx.Map[index.Index, float32]

	count := len(i.indexes)
	errs := make(chan error, count)
	defer close(errs)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(count)

	aggregatedErr := NewAggregatedError()

	ctx = slogx.WithAttrs(ctx, slog.String("documentID", string(document.ID())))

	slog.DebugContext(ctx, "pipeline: indexing document", slog.Int("indexCount", count))

	for identified := range i.indexes {
		go func(identified *IdentifiedIndex) {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						err = errors.WithStack(err)
						aggregatedErr.Add(err)
						errs <- err
					} else {
						panic(r)
					}
				}
			}()
			defer wg.Done()

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
				errs <- err
				cancel()
				return
			}

			errs <- nil
		}(identified)
	}

	wg.Wait()

	idx := 0

	for e := range errs {
		if e != nil {
			aggregatedErr.Add(e)
		}

		if idx >= count-1 {
			break
		}

		idx++
	}

	if aggregatedErr.Len() > 0 {
		return errors.WithStack(aggregatedErr.OrOnlyOne())
	}

	slog.DebugContext(ctx, "document indexed")

	return nil
}

// Search implements index.Index.
func (i *Index) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	query, err := i.transformQuery(ctx, query, opts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	count := len(i.indexes)

	type Message struct {
		Results *indexSearchResults
		Err     error
	}

	messages := make(chan *Message, count)
	defer close(messages)

	var wg sync.WaitGroup

	wg.Add(count)

	maxResults := 3
	if opts.MaxResults != 0 {
		maxResults = opts.MaxResults
	}

	collections := make([]model.CollectionID, 0)
	if opts.Collections != nil {
		collections = opts.Collections
	}

	aggregatedErr := NewAggregatedError()

	for identified := range i.indexes {
		go func(identified *IdentifiedIndex) {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						aggregatedErr.Add(errors.WithStack(err))
					} else {
						panic(r)
					}
				}
			}()
			defer wg.Done()

			indexCtx := slogx.WithAttrs(ctx, slog.String("index_type", fmt.Sprintf("%T", identified.Index())))

			results, err := identified.Index().Search(indexCtx, query, index.SearchOptions{
				MaxResults:  maxResults * 2,
				Collections: collections,
			})
			if err != nil {
				err = errors.WithStack(err)
				slog.ErrorContext(indexCtx, "could not search documents", slog.Any("error", err))
				messages <- &Message{
					Err: err,
				}
				return
			}

			slog.DebugContext(indexCtx, "found documents", slog.Int("total", len(results)))

			messages <- &Message{
				Results: &indexSearchResults{
					Results: results,
					Index:   identified,
				},
			}
		}(identified)
	}

	wg.Wait()

	results := make([]*indexSearchResults, 0)

	idx := 0

	for m := range messages {
		if m.Err != nil {
			aggregatedErr.Add(m.Err)
		}

		if m.Results != nil {
			results = append(results, m.Results)
		}

		if idx >= count-1 {
			break
		}

		idx++
	}

	if aggregatedErr.Len() > 0 {
		return nil, errors.WithStack(aggregatedErr.OrOnlyOne())
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

func (i *Index) transformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	var err error
	for _, t := range i.queryTransformers {
		query, err = t.TransformQuery(ctx, query, opts)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}

	return query, nil
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

func (i *Index) mergeResults(indexResults []*indexSearchResults, maxResults int) ([]*index.SearchResult, error) {
	type scoreItem struct {
		Score    float64
		Sections map[model.SectionID]float64
	}

	scores := map[string]scoreItem{}

	for _, r := range indexResults {
		for _, rr := range r.Results {
			source := rr.Source.String()

			var resultScore scoreItem
			if _, exists := scores[source]; !exists {
				scores[source] = scoreItem{
					Score:    0,
					Sections: make(map[model.SectionID]float64),
				}
			}

			resultScore = scores[source]
			resultScore.Score += 1.5 * i.indexes[r.Index]

			for _, s := range rr.Sections {
				if _, exists := resultScore.Sections[s]; !exists {
					resultScore.Sections[s] = 0
				}

				resultScore.Sections[s] += 1 * i.indexes[r.Index]
			}

			scores[source] = resultScore
		}
	}

	sources := []string{}
	for s := range scores {
		sources = append(sources, s)
	}

	slices.SortFunc(sources, func(s1, s2 string) int {
		score1 := scores[s1].Score
		for _, v := range scores[s1].Sections {
			score1 += v
		}

		score2 := scores[s2].Score
		for _, v := range scores[s2].Sections {
			score2 += v
		}

		if score1 < score2 {
			return 1
		}
		if score1 > score2 {
			return -1
		}
		return strings.Compare(s1, s2)
	})

	merged := make([]*index.SearchResult, 0)

	for _, rawSource := range sources {
		source, err := url.Parse(rawSource)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		sectionScores := scores[rawSource].Sections

		sectionIDs := []model.SectionID{}
		for id := range sectionScores {
			sectionIDs = append(sectionIDs, id)
		}

		slices.SortFunc(sectionIDs, func(id1, id2 model.SectionID) int {
			score1 := sectionScores[id1]
			score2 := sectionScores[id2]
			if score1 < score2 {
				return 1
			}
			if score1 > score2 {
				return -1
			}
			return strings.Compare(string(id1), string(id2))
		})

		merged = append(merged, &index.SearchResult{
			Source:   source,
			Sections: sectionIDs,
		})
	}

	if maxResults > 0 && len(merged) > maxResults {
		merged = merged[:maxResults]
	}

	return merged, nil
}

func NewIndex(indexes WeightedIndexes, funcs ...OptionFunc) *Index {
	opts := NewOptions(funcs...)
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
