package testsuite

import (
	"context"
	"embed"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
)

//go:embed testdata/**/*
var testdata embed.FS

func TestIndex(t *testing.T, factory func(t *testing.T) (index.Index, error)) {
	type testCase struct {
		Name string
		Run  func(t *testing.T, ctx context.Context, idx index.Index) error
	}

	var testCases []testCase = []testCase{
		{
			// A freshly created index must be queryable and iterable without
			// error, and must yield nothing.
			Name: "EmptyIndex",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				sections, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				if e, g := 0, len(sections); e != g {
					t.Errorf("len(All()): expected %d, got %d", e, g)
				}

				results, err := idx.Search(ctx, "une requête quelconque", index.SearchOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				if e, g := 0, len(results); e != g {
					t.Errorf("len(results): expected %d, got %d", e, g)
				}

				return nil
			},
		},
		{
			Name: "SimpleQuery",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				if _, _, err := loadTestDocuments(t, idx); err != nil {
					return errors.WithStack(err)
				}

				query := "De quand date la recette du boeuf bourguignon ?"

				t.Logf("executing query '%s'", query)

				results, err := idx.Search(ctx, query, index.SearchOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				t.Logf("results: %s", spew.Sdump(results))

				if len(results) == 0 {
					t.Fatalf("len(results): no results")
				}

				if e, g := "https://fr.wikipedia.org/wiki/B%C5%93uf_bourguignon", results[0].Source.String(); e != g {
					t.Errorf("results[0].Source.String(): expected %s, got %s", e, g)
				}

				return nil
			},
		},
		{
			Name: "FilterByCollection",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				_, collections, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				programmingCollection, exists := collections["programming"]
				if !exists {
					return errors.New("could not find 'programming' collection")
				}

				query := "Par qui a été créé le langage Go ?"

				t.Logf("executing query '%s'", query)

				results, err := idx.Search(ctx, query, index.SearchOptions{
					Collections: []model.CollectionID{programmingCollection.ID()},
				})
				if err != nil {
					return errors.WithStack(err)
				}

				t.Logf("results: %s", spew.Sdump(results))

				if len(results) == 0 {
					t.Fatalf("len(results): no results")
				}

				if e, g := 2, len(results); e != g {
					t.Errorf("len(results): expected %d, got %d", e, g)
				}

				if e, g := "https://fr.wikipedia.org/wiki/Go_(langage)", results[0].Source.String(); e != g {
					t.Errorf("results[0].Source.String(): expected %s, got %s", e, g)
				}

				return nil
			},
		},
		{
			// Filtering by a collection no document belongs to must exclude
			// every result, whatever the query would match otherwise.
			Name: "FilterByUnknownCollection",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				if _, _, err := loadTestDocuments(t, idx); err != nil {
					return errors.WithStack(err)
				}

				results, err := idx.Search(ctx, "langage de programmation", index.SearchOptions{
					Collections: []model.CollectionID{model.NewCollectionID()},
				})
				if err != nil {
					return errors.WithStack(err)
				}

				if e, g := 0, len(results); e != g {
					t.Errorf("len(results): expected %d, got %d — collection filter leaked", e, g)
				}

				return nil
			},
		},
		{
			// A collection filter must exclude documents that are not members,
			// even when the query strongly matches them.
			Name: "FilterExcludesOtherCollection",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				_, collections, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				programmingCollection, exists := collections["programming"]
				if !exists {
					return errors.New("could not find 'programming' collection")
				}

				const cookingSource = "https://fr.wikipedia.org/wiki/B%C5%93uf_bourguignon"

				results, err := idx.Search(ctx, "recette du boeuf bourguignon", index.SearchOptions{
					Collections: []model.CollectionID{programmingCollection.ID()},
				})
				if err != nil {
					return errors.WithStack(err)
				}

				t.Logf("results: %s", spew.Sdump(results))

				if containsSource(results, cookingSource) {
					t.Errorf("results contain %s although it is not part of the 'programming' collection", cookingSource)
				}

				return nil
			},
		},
		{
			// All must yield the indexed sections, and never an identifier that
			// does not correspond to a section of the loaded documents.
			Name: "AllIteratesIndexedSections",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				docs, _, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				known := map[model.SectionID]struct{}{}
				for _, doc := range docs {
					for _, id := range documentSectionIDs(doc) {
						known[id] = struct{}{}
					}
				}

				indexed, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				if len(indexed) == 0 {
					t.Fatalf("All() yielded no section")
				}

				for id := range indexed {
					if _, ok := known[id]; !ok {
						t.Errorf("All() yielded unknown section id %q", id)
					}
				}

				return nil
			},
		},
		{
			// Returning false from the yield callback must stop the iteration
			// immediately.
			Name: "AllHonorsEarlyStop",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				if _, _, err := loadTestDocuments(t, idx); err != nil {
					return errors.WithStack(err)
				}

				all, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				if len(all) < 2 {
					t.Fatalf("test precondition: expected at least 2 indexed sections, got %d", len(all))
				}

				count := 0
				if err := idx.All(ctx, func(_ model.SectionID) bool {
					count++
					return false
				}); err != nil {
					return errors.WithStack(err)
				}

				if e, g := 1, count; e != g {
					t.Errorf("yield calls after early stop: expected %d, got %d", e, g)
				}

				return nil
			},
		},
		{
			// Re-indexing the same document must replace it in place: no
			// duplicated nor orphaned section, and it stays searchable.
			Name: "ReindexReplacesDocument",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				docs, _, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				before, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				goDoc := findDocument(t, docs, "Go_(langage)")

				if err := idx.Index(ctx, goDoc); err != nil {
					return errors.WithStack(err)
				}

				after, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				if e, g := len(before), len(after); e != g {
					t.Errorf("len(All()) after reindex: expected %d, got %d — reindex is not idempotent", e, g)
				}

				results, err := idx.Search(ctx, "Par qui a été créé le langage Go ?", index.SearchOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				if !containsSource(results, goDoc.Source().String()) {
					t.Errorf("document %s is no longer searchable after reindex", goDoc.Source())
				}

				return nil
			},
		},
		{
			// DeleteBySource must remove every section of the targeted document
			// and leave the others untouched.
			Name: "DeleteBySourceRemovesDocument",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				docs, _, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				before, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				rustDoc := findDocument(t, docs, "Rust")
				rustIDs := documentSectionIDs(rustDoc)

				if !anyPresent(before, rustIDs) {
					t.Fatalf("test precondition: no section of %s was indexed", rustDoc.Source())
				}

				if err := idx.DeleteBySource(ctx, rustDoc.Source()); err != nil {
					return errors.WithStack(err)
				}

				after, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				for _, id := range rustIDs {
					if _, ok := after[id]; ok {
						t.Errorf("section %q still present after DeleteBySource", id)
					}
				}

				if len(after) == 0 {
					t.Errorf("DeleteBySource removed every section, expected only %s to be deleted", rustDoc.Source())
				}

				results, err := idx.Search(ctx, "sécurité mémoire du langage Rust", index.SearchOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				if containsSource(results, rustDoc.Source().String()) {
					t.Errorf("deleted document %s still appears in search results", rustDoc.Source())
				}

				return nil
			},
		},
		{
			// DeleteByID must remove exactly the targeted sections.
			Name: "DeleteByIDRemovesSections",
			Run: func(t *testing.T, ctx context.Context, idx index.Index) error {
				docs, _, err := loadTestDocuments(t, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				goDoc := findDocument(t, docs, "Go_(langage)")
				goIDs := documentSectionIDs(goDoc)

				before, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				if !anyPresent(before, goIDs) {
					t.Fatalf("test precondition: no section of %s was indexed", goDoc.Source())
				}

				if err := idx.DeleteByID(ctx, goIDs...); err != nil {
					return errors.WithStack(err)
				}

				after, err := collectAllSections(ctx, idx)
				if err != nil {
					return errors.WithStack(err)
				}

				for _, id := range goIDs {
					if _, ok := after[id]; ok {
						t.Errorf("section %q still present after DeleteByID", id)
					}
				}

				if len(after) == 0 {
					t.Errorf("DeleteByID removed every section, expected only %s to be deleted", goDoc.Source())
				}

				results, err := idx.Search(ctx, "Par qui a été créé le langage Go ?", index.SearchOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				if containsSource(results, goDoc.Source().String()) {
					t.Errorf("document %s still appears in search results after its sections were deleted", goDoc.Source())
				}

				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()

			idx, err := factory(t)
			if err != nil {
				t.Fatalf("could not create index: %+v", errors.WithStack(err))
			}

			if err := tc.Run(t, ctx, idx); err != nil {
				t.Fatalf("could not run test: %+v", errors.WithStack(err))
			}
		})
	}
}

func loadTestDocuments(t *testing.T, idx index.Index) ([]model.Document, map[string]model.Collection, error) {
	ctx := context.TODO()

	files, err := fs.Glob(testdata, "testdata/documents/*.md")
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	t.Logf("loading %d documents", len(files))

	collections := map[string]model.Collection{}
	documents := make([]model.Document, 0, len(files))

	for _, f := range files {
		data, err := testdata.ReadFile(f)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}

		doc, err := markdown.Parse(data)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}

		filename := filepath.Base(f)
		collectionName, _, _ := strings.Cut(filename, "_")

		coll, exists := collections[collectionName]
		if !exists {
			coll = model.NewCollection(
				model.NewCollectionID(),

				"",
				"",
			)
			collections[collectionName] = coll
		}

		doc.AddCollection(coll)

		t.Logf("indexing document %s within collections %v", doc.Source(), slices.Collect(func(yield func(string) bool) {
			for _, c := range doc.Collections() {
				if !yield(c.Label()) {
					return
				}
			}
		}))

		if err := idx.Index(ctx, doc); err != nil {
			return nil, nil, errors.WithStack(err)
		}

		documents = append(documents, doc)
	}

	return documents, collections, nil
}

// collectAllSections drains idx.All into a deduplicated set. Backends that
// split a section into several chunks may yield the same identifier more than
// once, so callers must not rely on the raw yield count.
func collectAllSections(ctx context.Context, idx index.Index) (map[model.SectionID]struct{}, error) {
	sections := map[model.SectionID]struct{}{}

	err := idx.All(ctx, func(id model.SectionID) bool {
		sections[id] = struct{}{}
		return true
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return sections, nil
}

// documentSectionIDs returns the identifiers of every section of doc,
// recursively.
func documentSectionIDs(doc model.Document) []model.SectionID {
	var ids []model.SectionID

	_ = model.WalkSections(doc, func(s model.Section) error {
		ids = append(ids, s.ID())
		return nil
	})

	return ids
}

// findDocument returns the loaded document whose source contains needle.
func findDocument(t *testing.T, docs []model.Document, needle string) model.Document {
	for _, doc := range docs {
		if strings.Contains(doc.Source().String(), needle) {
			return doc
		}
	}

	t.Fatalf("no test document with a source containing %q", needle)

	return nil
}

// containsSource reports whether results reference the given source.
func containsSource(results []*index.SearchResult, source string) bool {
	for _, r := range results {
		if r.Source.String() == source {
			return true
		}
	}

	return false
}

// anyPresent reports whether at least one of ids belongs to set.
func anyPresent(set map[model.SectionID]struct{}, ids []model.SectionID) bool {
	for _, id := range ids {
		if _, ok := set[id]; ok {
			return true
		}
	}

	return false
}
