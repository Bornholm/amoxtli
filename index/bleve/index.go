package bleve

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"slices"

	"github.com/blevesearch/bleve/v2"
	bleveQuery "github.com/blevesearch/bleve/v2/search/query"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

type Index struct {
	index bleve.Index
	// recreated reports that OpenOrCreate rebuilt the index from scratch —
	// because it did not exist, or because the mapping changed. A recreated
	// index is empty: whoever owns the corpus must reindex it, and without
	// this signal the loss is silent — searches simply return nothing.
	recreated bool
}

// DeleteByID implements index.Index.
func (i *Index) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	batch := i.index.NewBatch()
	for _, id := range ids {
		batch.Delete(string(id))
	}

	if err := i.index.Batch(batch); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// DeleteBySource implements index.Index.
func (i *Index) DeleteBySource(ctx context.Context, source *url.URL) error {
	query := bleve.NewTermQuery(source.String())
	query.SetField("source")
	req := &bleve.SearchRequest{
		Query: query,
		Size:  100,
	}

	for {
		result, err := i.index.Search(req)
		if err != nil {
			return errors.WithStack(err)
		}

		// No hit left: the source is gone from the index. Also guards against
		// looping forever should Total disagree with the returned hits.
		if len(result.Hits) == 0 {
			break
		}

		batch := i.index.NewBatch()
		for _, r := range result.Hits {
			slog.DebugContext(ctx, "deleting resource", slog.String("source", source.String()), slog.String("id", r.ID))

			batch.Delete(r.ID)
		}

		if err := i.index.Batch(batch); err != nil {
			return errors.WithStack(err)
		}

		if result.Total <= uint64(req.Size) {
			break
		}
	}

	return nil
}

// batchFlushSize bounds how many section updates accumulate before the batch
// is applied. Indexing section by section would create one scorch segment per
// section: the segment count — and the background merges it triggers — would
// then grow with the corpus, making each new document slower to index than the
// previous one. Batching keeps the per-document cost flat.
const batchFlushSize = 500

// Index implements index.Index.
func (i *Index) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	opts := index.NewOptions(funcs...)
	source := document.Source()
	if source == nil {
		return errors.New("source missing")
	}

	if err := i.DeleteBySource(ctx, source); err != nil {
		return errors.WithStack(err)
	}

	// The sections skipped as blank never report their progress, so the count
	// below can stop short of 1.
	defer func() {
		if opts.OnProgress != nil {
			opts.OnProgress(1.0)
		}
	}()

	totalSections := model.CountSections(document)

	totalIndexed := 0
	onSectionIndexed := func() {
		if opts.OnProgress == nil {
			return
		}

		totalIndexed++
		progress := float32(totalIndexed) / float32(totalSections)
		opts.OnProgress(progress)
	}

	batch := i.index.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}

		if err := i.index.Batch(batch); err != nil {
			return errors.WithStack(err)
		}

		batch.Reset()

		return nil
	}

	for _, s := range document.Sections() {
		if err := i.indexSection(ctx, batch, flush, s, onSectionIndexed); err != nil {
			return errors.WithStack(err)
		}
	}

	return flush()
}

// indexSection adds the section and its children to batch, flushing it
// whenever it reaches batchFlushSize.
func (i *Index) indexSection(ctx context.Context, batch *bleve.Batch, flush func() error, section model.Section, onSectionIndexed func()) error {
	for _, s := range section.Sections() {
		if err := i.indexSection(ctx, batch, flush, s, onSectionIndexed); err != nil {
			return errors.WithStack(err)
		}
	}

	id, resource, err := i.getIndexableResource(ctx, section)
	if err != nil {
		return errors.WithStack(err)
	}

	if resource == nil {
		slog.DebugContext(ctx, "ignoring empty section", slog.String("sectionID", string(section.ID())))
		return nil
	}

	slog.DebugContext(ctx, "indexing section", slog.String("sectionID", string(section.ID())))

	if err := batch.Index(id, resource); err != nil {
		return errors.WithStack(err)
	}

	onSectionIndexed()

	if batch.Size() >= batchFlushSize {
		if err := flush(); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (i *Index) getIndexableResource(ctx context.Context, section model.Section) (string, map[string]any, error) {
	source := section.Document().Source()

	collections := slices.Collect(func(yield func(s string) bool) {
		for _, c := range section.Document().Collections() {
			if !yield(string(c.ID())) {
				return
			}
		}
	})

	content, err := section.Content()
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	if len(bytes.TrimSpace(content)) == 0 {
		return "", nil, nil
	}

	return string(section.ID()), map[string]any{
		"_type":       "resource",
		"content":     string(content),
		"source":      source.String(),
		"collections": collections,
	}, nil
}

// Search implements index.Index.
func (i *Index) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	queries := []bleveQuery.Query{}

	matchQuery := bleve.NewMatchQuery(query)
	queries = append(queries, matchQuery)

	if len(opts.Collections) > 0 {
		collectionQueries := make([]bleveQuery.Query, 0)
		for _, c := range opts.Collections {
			termQuery := bleve.NewTermQuery(string(c))
			termQuery.SetField("collections")
			collectionQueries = append(collectionQueries, termQuery)
		}
		queries = append(queries, bleve.NewDisjunctionQuery(collectionQueries...))
	}

	// A hit is a section, but MaxResults counts sources: page through the
	// hits until that many sources are reached (a long document can fill a
	// page on its own), within a bound on the hits read.
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	pageSize := maxResults * 4
	maxHits := maxResults * maxHitsPerResult

	var hits []index.Hit
	for from := 0; from < maxHits; from += pageSize {
		req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(queries...))
		req.From = from
		req.Size = min(pageSize, maxHits-from)
		req.Fields = []string{"source"}

		result, err := i.index.SearchInContext(ctx, req)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		for _, r := range result.Hits {
			rawSource, ok := r.Fields["source"].(string)
			if !ok {
				continue
			}

			source, err := url.Parse(rawSource)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			source.Fragment = ""

			hits = append(hits, index.Hit{Source: source.String(), Section: model.SectionID(r.ID), Score: r.Score})
		}

		if len(result.Hits) < req.Size || index.CountSources(hits) >= maxResults {
			break
		}
	}

	return index.AggregateHits(hits, maxResults)
}

// maxHitsPerResult bounds the section hits read per requested source.
const maxHitsPerResult = 64

// Close releases the resources held by the underlying bleve index.
func (i *Index) Close() error {
	return i.index.Close()
}

func NewIndex(index bleve.Index) *Index {
	return &Index{
		index: index,
	}
}

// Recreated reports whether OpenOrCreate rebuilt this index empty. Callers
// should trigger a reindex of their corpus when it returns true.
func (i *Index) Recreated() bool {
	return i.recreated
}

// DocCount returns the number of indexed entries. Combined with a count of
// the corpus, it detects an index left empty by an interrupted reindex —
// a state Recreated cannot see, since the rebuild happened on an earlier
// start.
func (i *Index) DocCount() (uint64, error) {
	count, err := i.index.DocCount()
	if err != nil {
		return 0, errors.WithStack(err)
	}

	return count, nil
}

var _ index.Index = &Index{}
