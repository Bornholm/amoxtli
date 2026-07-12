package bleve

import (
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

		for _, r := range result.Hits {
			slog.DebugContext(ctx, "deleting resource", slog.String("source", source.String()), slog.String("id", r.ID))

			if err := i.index.Delete(r.ID); err != nil {
				return errors.WithStack(err)
			}
		}

		if result.Total <= uint64(req.Size) {
			break
		}
	}

	return nil
}

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

	for _, s := range document.Sections() {
		if err := i.indexSection(ctx, s, onSectionIndexed); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (i *Index) indexSection(ctx context.Context, section model.Section, onSectionIndexed func()) error {
	for _, s := range section.Sections() {
		if err := i.indexSection(ctx, s, onSectionIndexed); err != nil {
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

	if err := i.index.Index(id, resource); err != nil {
		return errors.WithStack(err)
	}

	onSectionIndexed()

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

	if len(content) == 0 {
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

	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(queries...))

	req.From = 0
	req.Fields = []string{"source"}

	if opts.MaxResults > 0 {
		req.Size = opts.MaxResults
	}

	result, err := i.index.SearchInContext(ctx, req)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	mappedScores := map[string]float64{}
	mappedSections := map[string][]model.SectionID{}
	mappedSectionScores := map[string]map[model.SectionID]float64{}

	for _, r := range result.Hits {
		rawSource, ok := r.Fields["source"].(string)
		if !ok {
			continue
		}

		source, err := url.Parse(rawSource)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		sectionID := model.SectionID(r.ID)

		source.Fragment = ""
		key := source.String()

		sectionIDs, exists := mappedSections[key]
		if !exists {
			sectionIDs = make([]model.SectionID, 0)
			mappedSectionScores[key] = map[model.SectionID]float64{}
		}

		sectionIDs = append(sectionIDs, sectionID)

		mappedSections[key] = sectionIDs
		mappedScores[key] += r.Score
		mappedSectionScores[key][sectionID] += r.Score
	}

	searchResults := make([]*index.SearchResult, 0)

	for rawSource, sectionIDs := range mappedSections {
		source, err := url.Parse(rawSource)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		searchResults = append(searchResults, &index.SearchResult{
			Source:        source,
			Sections:      sectionIDs,
			Score:         mappedScores[rawSource],
			SectionScores: mappedSectionScores[rawSource],
		})
	}

	slices.SortFunc(searchResults, func(r1 *index.SearchResult, r2 *index.SearchResult) int {
		score1 := mappedScores[r1.Source.String()]
		score2 := mappedScores[r2.Source.String()]
		if score1 > score2 {
			return -1
		}
		if score1 < score2 {
			return 1
		}
		return 0
	})

	return searchResults, nil
}

// Close releases the resources held by the underlying bleve index.
func (i *Index) Close() error {
	return i.index.Close()
}

func NewIndex(index bleve.Index) *Index {
	return &Index{
		index: index,
	}
}

var _ index.Index = &Index{}
