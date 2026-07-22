package index_test

import (
	"context"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/filtertest"
	"github.com/pkg/errors"
)

// TestFilterConformance runs the shared semantics suite against the Go
// evaluator. Every filterable backend must run the same suite against its SQL
// translation: that pair of runs is what keeps push-down and fallback returning
// the same documents.
func TestFilterConformance(t *testing.T) {
	filtertest.Run(t, func(_ context.Context, filter index.Filter, metadata map[string]any) (bool, error) {
		return filter.Matches(metadata), nil
	})
}

func TestFilterValidate(t *testing.T) {
	testCases := []struct {
		name    string
		filter  index.Filter
		wantErr bool
	}{
		{"empty", index.Filter{}, false},
		{"simple key", index.Filter{index.Eq("author", "x")}, false},
		{"digits, dash and underscore", index.Filter{index.Eq("created_at-2", "x")}, false},
		{"empty key", index.Filter{index.Eq("", "x")}, true},
		{"dotted key breaks sqlite json paths", index.Filter{index.Eq("a.b", "x")}, true},
		{"quoted key", index.Filter{index.Eq(`a"b`, "x")}, true},
		{"sql injection attempt", index.Filter{index.Eq("'; DROP TABLE documents; --", "x")}, true},
		{"json path key", index.Filter{index.Eq(`$."a"[0]`, "x")}, true},
		{"space in key", index.Filter{index.Eq("a b", "x")}, true},
		{"unicode key", index.Filter{index.Eq("auteur_éric", "x")}, true},
		{"second condition invalid", index.Filter{index.Eq("a", "x"), index.Eq("a b", "y")}, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Validate()

			if tc.wantErr {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				if !errors.Is(err, index.ErrInvalidFilterKey) {
					t.Fatalf("Validate() = %+v, want an ErrInvalidFilterKey", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Validate() = %+v, want nil", err)
			}
		})
	}
}

// A rejected key must never reach a backend, but an unvalidated filter must
// still evaluate safely: a hostile key is a key that no document carries.
func TestFilterMatchesIgnoresInvalidKeys(t *testing.T) {
	filter := index.Filter{index.Eq("'; DROP TABLE documents; --", "x")}

	if filter.Matches(map[string]any{"a": "x"}) {
		t.Fatal("expected a hostile key to match nothing")
	}
}
