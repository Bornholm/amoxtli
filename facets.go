package amoxtli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/internal/filternorm"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// FacetOptions scopes a facet computation.
type FacetOptions struct {
	// Collections restricts the documents counted; empty means all of them.
	Collections []model.CollectionID
	// Filter keeps only the documents it matches — an access control filter
	// on a reserved key, typically, so that counts reveal nothing of the
	// documents a user cannot see.
	Filter index.Filter
	// Limit bounds the number of values or keys returned; 0 means no limit.
	Limit int
}

// FacetValue is one value of a metadata key with the number of documents
// carrying it.
type FacetValue struct {
	Value any
	Count int
}

// FacetKey is a metadata key with the number of documents carrying it.
type FacetKey struct {
	Key   string
	Count int
}

// facetPageSize is the number of document headers read per store query.
const facetPageSize = 500

// Facets returns the distinct values of a metadata key and the number of
// documents carrying each, most frequent first. The elements of an array value
// are counted one by one. Reserved keys (see model.ReservedMetadataPrefix)
// have no facets.
//
// Counts are computed over the document headers, which is fine for corpora of
// a few hundred thousand documents; nothing is precomputed.
func (c *Codex) Facets(ctx context.Context, key string, opts FacetOptions) ([]FacetValue, error) {
	if err := index.ValidateKey(key); err != nil {
		return nil, errors.WithStack(err)
	}
	if strings.HasPrefix(key, model.ReservedMetadataPrefix) {
		return nil, nil
	}

	type bucket struct {
		value any
		count int
	}

	buckets := map[string]*bucket{}

	err := c.eachDocumentMetadata(ctx, opts, func(metadata map[string]any) {
		value, present := metadata[key]
		if !present {
			return
		}

		values := []any{value}
		if elements, ok := filternorm.Elements(value); ok {
			values = elements
		}

		// A value repeated inside one document's array counts once.
		seen := map[string]bool{}
		for _, v := range values {
			v = filternorm.Value(v)
			if _, isContainer := filternorm.Elements(v); isContainer {
				continue
			}
			if _, isMap := v.(map[string]any); isMap {
				continue
			}

			id := facetID(v)
			if seen[id] {
				continue
			}
			seen[id] = true

			b, exists := buckets[id]
			if !exists {
				b = &bucket{value: v}
				buckets[id] = b
			}
			b.count++
		}
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	facets := make([]FacetValue, 0, len(buckets))
	for _, b := range buckets {
		facets = append(facets, FacetValue{Value: b.value, Count: b.count})
	}

	slices.SortFunc(facets, func(a, b FacetValue) int {
		if n := cmp.Compare(b.Count, a.Count); n != 0 {
			return n
		}
		return cmp.Compare(facetID(a.Value), facetID(b.Value))
	})

	if opts.Limit > 0 && len(facets) > opts.Limit {
		facets = facets[:opts.Limit]
	}

	return facets, nil
}

// MetadataKeys returns the metadata keys present on the documents and the
// number of documents carrying each, most frequent first. Reserved keys are
// left out.
func (c *Codex) MetadataKeys(ctx context.Context, opts FacetOptions) ([]FacetKey, error) {
	counts := map[string]int{}

	err := c.eachDocumentMetadata(ctx, opts, func(metadata map[string]any) {
		for k := range metadata {
			if !strings.HasPrefix(k, model.ReservedMetadataPrefix) {
				counts[k]++
			}
		}
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	keys := make([]FacetKey, 0, len(counts))
	for k, n := range counts {
		keys = append(keys, FacetKey{Key: k, Count: n})
	}

	slices.SortFunc(keys, func(a, b FacetKey) int {
		if n := cmp.Compare(b.Count, a.Count); n != 0 {
			return n
		}
		return cmp.Compare(a.Key, b.Key)
	})

	if opts.Limit > 0 && len(keys) > opts.Limit {
		keys = keys[:opts.Limit]
	}

	return keys, nil
}

// eachDocumentMetadata calls fn with the metadata of every document in scope
// matching the filter, each document once even when it belongs to several of
// the collections.
func (c *Codex) eachDocumentMetadata(ctx context.Context, opts FacetOptions, fn func(metadata map[string]any)) error {
	if err := opts.Filter.Validate(); err != nil {
		return errors.WithStack(err)
	}

	seen := map[model.DocumentID]bool{}

	visit := func(query func(page, limit int) ([]model.PersistedDocument, error)) error {
		for page := 0; ; page++ {
			if err := ctx.Err(); err != nil {
				return errors.WithStack(err)
			}

			documents, err := query(page, facetPageSize)
			if err != nil {
				return errors.WithStack(err)
			}

			for _, d := range documents {
				if seen[d.ID()] {
					continue
				}
				seen[d.ID()] = true

				metadata := model.Metadata(d)
				if !opts.Filter.Matches(metadata) {
					continue
				}
				fn(metadata)
			}

			if len(documents) < facetPageSize {
				return nil
			}
		}
	}

	if len(opts.Collections) == 0 {
		return visit(func(page, limit int) ([]model.PersistedDocument, error) {
			documents, _, err := c.store.QueryDocuments(ctx, ingest.QueryDocumentsOptions{Page: &page, Limit: &limit, HeaderOnly: true})
			return documents, errors.WithStack(err)
		})
	}

	for _, collectionID := range opts.Collections {
		err := visit(func(page, limit int) ([]model.PersistedDocument, error) {
			documents, _, err := c.store.QueryDocumentsByCollectionID(ctx, collectionID, ingest.QueryDocumentsOptions{Page: &page, Limit: &limit, HeaderOnly: true})
			return documents, errors.WithStack(err)
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// facetID keys a facet value by kind and content, so that 3 and "3" stay two
// distinct facets while 3 and 3.0 merge.
func facetID(v any) string {
	if number, ok := filternorm.Float(v); ok {
		return fmt.Sprintf("n:%v", number)
	}
	return fmt.Sprintf("%T:%v", v, v)
}
