package filterexpr

import (
	"reflect"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

func TestParseFilters(t *testing.T) {
	testCases := []struct {
		name    string
		exprs   []string
		want    []index.Condition
		wantErr error
	}{
		{
			name:  "comparison operators",
			exprs: []string{"type=code", "type!=code", "year>=2020", "year<=2026", "n>1", "n<9"},
			want: []index.Condition{
				index.Eq("type", "code"),
				index.Ne("type", "code"),
				index.Gte("year", 2020.0),
				index.Lte("year", 2026.0),
				index.Gt("n", 1.0),
				index.Lt("n", 9.0),
			},
		},
		{
			name:  "scalar types",
			exprs: []string{"public=true", "year=2026", "author=william"},
			want: []index.Condition{
				index.Eq("public", true),
				index.Eq("year", 2026.0),
				index.Eq("author", "william"),
			},
		},
		{
			name:  "presence forms",
			exprs: []string{"type?", "!type"},
			want:  []index.Condition{index.Exists("type"), index.NotExists("type")},
		},
		{
			name:    "invalid key",
			exprs:   []string{"a.b=x"},
			wantErr: index.ErrInvalidFilterKey,
		},
		{
			name:    "invalid presence key",
			exprs:   []string{"!a b"},
			wantErr: index.ErrInvalidFilterKey,
		},
		{
			name:  "no operator",
			exprs: []string{"nonsense"},
			// Not an ErrInvalidFilterKey: the expression has no recognizable shape.
			wantErr: errors.New("invalid filter"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFilters(tc.exprs)

			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseFilters(%v) = nil error, want one", tc.exprs)
				}
				if errors.Is(tc.wantErr, index.ErrInvalidFilterKey) && !errors.Is(err, index.ErrInvalidFilterKey) {
					t.Fatalf("ParseFilters(%v) = %+v, want an ErrInvalidFilterKey", tc.exprs, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseFilters(%v) = %+v", tc.exprs, err)
			}

			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseFilters(%v) = %#v, want %#v", tc.exprs, got, tc.want)
			}
		})
	}
}
