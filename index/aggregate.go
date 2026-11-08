package index

import (
	"cmp"
	"net/url"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// Hit is one scored match of a backend: a section of a source, or a chunk of
// it (several hits may then share a section). Higher scores are better.
type Hit struct {
	Source  string
	Section model.SectionID
	Score   float64
}

// AggregateHits groups the hits of a backend into per-source results.
//
// A section's score is the best score of its hits, and a source's score is the
// best score of its sections. Summing them instead, as the backends used to,
// ranks a long document above a short one only because it has more sections
// that match a little: on long documents (MLDR, Wikipedia) the max ranks the
// right document first for 16 more questions in 100.
//
// The sections of a result are ordered by decreasing score. The results are
// ordered by decreasing score, ties broken by source, and cut to maxResults
// sources when maxResults is positive.
func AggregateHits(hits []Hit, maxResults int) ([]*SearchResult, error) {
	type agg struct {
		score    float64
		sections map[model.SectionID]float64
	}
	bySource := map[string]*agg{}
	for _, h := range hits {
		a, ok := bySource[h.Source]
		if !ok {
			a = &agg{score: h.Score, sections: map[model.SectionID]float64{}}
			bySource[h.Source] = a
		}
		a.score = max(a.score, h.Score)
		if s, ok := a.sections[h.Section]; !ok || h.Score > s {
			a.sections[h.Section] = h.Score
		}
	}

	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	slices.SortFunc(sources, func(s1, s2 string) int {
		if c := cmp.Compare(bySource[s2].score, bySource[s1].score); c != 0 {
			return c
		}
		return strings.Compare(s1, s2)
	})
	if maxResults > 0 && len(sources) > maxResults {
		sources = sources[:maxResults]
	}

	results := make([]*SearchResult, 0, len(sources))
	for _, raw := range sources {
		source, err := url.Parse(raw)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		a := bySource[raw]
		ids := make([]model.SectionID, 0, len(a.sections))
		for id := range a.sections {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(id1, id2 model.SectionID) int {
			if c := cmp.Compare(a.sections[id2], a.sections[id1]); c != 0 {
				return c
			}
			return strings.Compare(string(id1), string(id2))
		})
		results = append(results, &SearchResult{
			Source:        source,
			Sections:      ids,
			Score:         a.score,
			SectionScores: a.sections,
		})
	}
	return results, nil
}

// CountSources returns the number of distinct sources among hits.
func CountSources(hits []Hit) int {
	seen := map[string]struct{}{}
	for _, h := range hits {
		seen[h.Source] = struct{}{}
	}
	return len(seen)
}
