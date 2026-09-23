package amoxtli

import (
	"context"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func TestCodexFacets(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	otherID, err := codex.CreateCollection(ctx, "other")
	if err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}

	indexSync(t, codex, collID, "https://example.net/a.md", map[string]any{"type": "contrat", "year": 2024, "tags": []any{"social", "cadre"}, "_acl": []any{"u:alice"}})
	indexSync(t, codex, collID, "https://example.net/b.md", map[string]any{"type": "contrat", "year": 2025.0, "tags": []any{"social", "social"}, "_acl": []any{"u:bob"}})
	indexSync(t, codex, collID, "https://example.net/c.md", map[string]any{"type": "avenant", "year": "2024", "_acl": []any{"u:alice"}})
	indexSync(t, codex, otherID, "https://example.net/d.md", map[string]any{"type": "note"})

	values := func(key string, opts FacetOptions) map[string]int {
		t.Helper()
		facets, err := codex.Facets(ctx, key, opts)
		if err != nil {
			t.Fatalf("Facets(%s): %+v", key, errors.WithStack(err))
		}
		out := map[string]int{}
		for _, f := range facets {
			out[facetID(f.Value)] = f.Count
		}
		return out
	}

	types := values("type", FacetOptions{Collections: []model.CollectionID{collID}})
	if types["string:contrat"] != 2 || types["string:avenant"] != 1 || types["string:note"] != 0 {
		t.Errorf("unexpected type facets %v", types)
	}

	years := values("year", FacetOptions{Collections: []model.CollectionID{collID}})
	if years["n:2024"] != 1 || years["n:2025"] != 1 || years["string:2024"] != 1 {
		t.Errorf("numbers and strings must stay distinct facets: %v", years)
	}

	tags := values("tags", FacetOptions{Collections: []model.CollectionID{collID}})
	if tags["string:social"] != 2 || tags["string:cadre"] != 1 {
		t.Errorf("array elements must count once per document: %v", tags)
	}

	restricted := values("type", FacetOptions{Collections: []model.CollectionID{collID}, Filter: index.Filter{index.Any("_acl", "u:alice")}})
	if restricted["string:contrat"] != 1 || restricted["string:avenant"] != 1 {
		t.Errorf("the filter must restrict the counts: %v", restricted)
	}

	all := values("type", FacetOptions{})
	if all["string:note"] != 1 || all["string:contrat"] != 2 {
		t.Errorf("unexpected facets over the whole corpus: %v", all)
	}

	if reserved := values("_acl", FacetOptions{}); len(reserved) != 0 {
		t.Errorf("reserved keys must have no facets: %v", reserved)
	}

	keys, err := codex.MetadataKeys(ctx, FacetOptions{Collections: []model.CollectionID{collID}})
	if err != nil {
		t.Fatalf("MetadataKeys: %+v", errors.WithStack(err))
	}
	counts := map[string]int{}
	for _, k := range keys {
		counts[k.Key] = k.Count
	}
	if counts["type"] != 3 || counts["tags"] != 2 {
		t.Errorf("unexpected key counts %v", counts)
	}
	if _, found := counts["_acl"]; found {
		t.Errorf("reserved key listed: %v", counts)
	}
}
