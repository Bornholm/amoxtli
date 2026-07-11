package pipeline

import (
	"context"
	"net/url"
	"slices"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func TestBranchMergeResultsTransformer(t *testing.T) {
	source, _ := url.Parse("https://example.net")

	tests := []struct {
		name     string
		sections []model.SectionID
		expected []model.SectionID
	}{
		{
			name: "EliminateAncestorsKeepLeaves",
			sections: []model.SectionID{
				"parent1", "child1", "child2",
				"grandchild1", "grandchild2",
				"parent2", "child3",
				"parent3",
			},
			expected: []model.SectionID{
				"child1", "grandchild1", "grandchild2",
				"child3", "parent3",
			},
		},
		{
			name:     "SingleSection",
			sections: []model.SectionID{"parent1"},
			expected: []model.SectionID{"parent1"},
		},
		{
			name:     "EmptySections",
			sections: []model.SectionID{},
			expected: []model.SectionID{},
		},
		{
			name: "NoOverlap",
			sections: []model.SectionID{
				"parent1", "parent2", "parent3",
			},
			expected: []model.SectionID{
				"parent1", "parent2", "parent3",
			},
		},
		{
			name: "DeepChainKeepsOnlyLeaf",
			sections: []model.SectionID{
				"parent1", "child2", "grandchild1",
			},
			expected: []model.SectionID{"grandchild1"},
		},
		{
			name: "SiblingsAllKept",
			sections: []model.SectionID{
				"child1", "child2",
			},
			expected: []model.SectionID{"child1", "child2"},
		},
	}

	store := newDummyStore()

	transformer := &DuplicateContentResultsTransformer{store: store}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := []*index.SearchResult{
				{
					Source:   source,
					Sections: tt.sections,
				},
			}

			transformed, err := transformer.TransformResults(
				context.Background(), "test", results, index.SearchOptions{},
			)
			if err != nil {
				t.Fatalf("%+v", errors.WithStack(err))
			}

			got := transformed[0].Sections

			if e, g := len(tt.expected), len(got); e != g {
				t.Errorf("len(sections): expected %d, got %d", e, g)
			}

			for _, e := range tt.expected {
				if !slices.Contains(got, e) {
					t.Errorf("expected section '%s' not found in %v", e, got)
				}
			}
		})
	}
}

func newDummyStore() *dummyStore {
	return &dummyStore{
		sections: map[model.SectionID]model.Section{
			"parent1": &dummySection{
				id:     "parent1",
				branch: []model.SectionID{"parent1"},
			},
			"child1": &dummySection{
				id:     "child1",
				branch: []model.SectionID{"parent1", "child1"},
			},
			"child2": &dummySection{
				id:     "child2",
				branch: []model.SectionID{"parent1", "child2"},
			},
			"grandchild1": &dummySection{
				id:     "grandchild1",
				branch: []model.SectionID{"parent1", "child2", "grandchild1"},
			},
			"grandchild2": &dummySection{
				id:     "grandchild2",
				branch: []model.SectionID{"parent1", "child2", "grandchild2"},
			},
			"parent2": &dummySection{
				id:     "parent2",
				branch: []model.SectionID{"parent2"},
			},
			"child3": &dummySection{
				id:     "child3",
				branch: []model.SectionID{"parent2", "child3"},
			},
			"parent3": &dummySection{
				id:     "parent3",
				branch: []model.SectionID{"parent3"},
			},
		},
	}
}

type dummyStore struct {
	sections map[model.SectionID]model.Section
}

// GetSectionsByIDs implements SectionStore.
func (d *dummyStore) GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error) {
	result := make(map[model.SectionID]model.Section)
	for _, id := range ids {
		if s, exists := d.sections[id]; exists {
			result[id] = s
		}
	}
	return result, nil
}

var _ SectionStore = &dummyStore{}

type dummySection struct {
	id     model.SectionID
	branch []model.SectionID
	parent model.Section
}

// Branch implements model.Section.
func (d *dummySection) Branch() []model.SectionID {
	return d.branch
}

// Content implements model.Section.
func (d *dummySection) Content() ([]byte, error) {
	return []byte{}, nil
}

// Document implements model.Section.
func (d *dummySection) Document() model.Document {
	panic("unimplemented")
}

// End implements model.Section.
func (d *dummySection) End() int {
	return 1
}

// ID implements model.Section.
func (d *dummySection) ID() model.SectionID {
	return d.id
}

// Level implements model.Section.
func (d *dummySection) Level() uint {
	panic("unimplemented")
}

// Parent implements model.Section.
func (d *dummySection) Parent() model.Section {
	return d.parent
}

// Sections implements model.Section.
func (d *dummySection) Sections() []model.Section {
	return []model.Section{}
}

// Start implements model.Section.
func (d *dummySection) Start() int {
	return 0
}

var _ model.Section = &dummySection{}
