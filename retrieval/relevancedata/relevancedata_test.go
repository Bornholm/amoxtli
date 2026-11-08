package relevancedata

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
)

// stubLLM writes the first word of the passage as the query, judges relevant
// the documents containing the query, and complete those starting with it.
type stubLLM struct {
	llm.Client
}

func (m *stubLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)
	user := opts.Messages[len(opts.Messages)-1].Content()

	var response any
	if strings.HasPrefix(user, "## Query") {
		query := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(user, "## Query"), "## Documents", 2)[0])
		relevant, complete := []string{}, []string{}
		for _, doc := range strings.Split(user, "### Document ")[1:] {
			id, body, _ := strings.Cut(doc, "\n")
			if strings.Contains(body, query) {
				relevant = append(relevant, id)
			}
			if strings.HasPrefix(strings.TrimSpace(body), query) {
				complete = append(complete, id)
			}
		}
		response = map[string]any{"relevant": relevant, "complete": complete}
	} else {
		response = map[string]any{"query": strings.Fields(user)[0]}
	}

	data, _ := json.Marshal(response)
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, string(data)), llm.NewChatCompletionUsage(0, 0, 0)), nil
}

type stubSection struct {
	model.Section
	id      model.SectionID
	content string
}

func (s *stubSection) ID() model.SectionID       { return s.id }
func (s *stubSection) Content() ([]byte, error)  { return []byte(s.content), nil }
func (s *stubSection) Sections() []model.Section { return nil }

type stubStore map[model.SectionID]model.Section

func (s stubStore) GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error) {
	out := map[model.SectionID]model.Section{}
	for _, id := range ids {
		if sec, ok := s[id]; ok {
			out[id] = sec
		}
	}
	return out, nil
}

func TestBuild(t *testing.T) {
	store := stubStore{
		"cats":  &stubSection{id: "cats", content: "felines purr and chase mice"},
		"dogs":  &stubSection{id: "dogs", content: "canines bark at the mailman"},
		"tiger": &stubSection{id: "tiger", content: "tigers are large felines"},
		"zebra": &stubSection{id: "zebra", content: "zebras graze on the savanna"},
	}

	// The search always returns dogs then tiger: it misses the cats section,
	// which the builder must add to the candidates.
	search := func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
		return []*index.SearchResult{
			{Source: &url.URL{Scheme: "test", Host: "a"}, Sections: []model.SectionID{"dogs"}},
			{Source: &url.URL{Scheme: "test", Host: "b"}, Sections: []model.SectionID{"tiger"}},
		}, nil
	}

	b := NewBuilder(&stubLLM{}, store, search, Options{Candidates: 2})
	pairs, stats, err := b.Build(context.Background(), []model.Section{store["cats"]})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	// Query "felines": dogs is not relevant, tiger and cats are.
	got := map[model.SectionID]bool{}
	sufficiency := map[model.SectionID]any{}
	for _, p := range pairs {
		if p.Query != "felines" || p.Family != "cats" {
			t.Fatalf("pair = %+v, want query felines from cats", p)
		}
		got[p.Section] = p.Relevant()
		sufficiency[p.Section] = p.Labels[QuestionSufficient]
	}
	// cats starts with the query: complete on its own.
	wantSufficiency := map[model.SectionID]any{"dogs": SufficiencyNone, "tiger": SufficiencyPartial, "cats": SufficiencyComplete}
	for id, s := range wantSufficiency {
		if sufficiency[id] != s {
			t.Fatalf("sufficiency = %v, want %v", sufficiency, wantSufficiency)
		}
	}
	want := map[model.SectionID]bool{"dogs": false, "tiger": true, "cats": true}
	if len(got) != len(want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	for id, rel := range want {
		if got[id] != rel {
			t.Fatalf("pairs = %v, want %v", got, want)
		}
	}
	if stats.Positives != 2 || stats.Negatives != 1 || stats.Dropped != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBuildDropsInconsistentQuery(t *testing.T) {
	// The store cannot return the section, so the labeler never sees it and
	// cannot judge it relevant: the query is dropped.
	section := &stubSection{id: "missing", content: "felines purr"}
	search := func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
		return nil, nil
	}

	b := NewBuilder(&stubLLM{}, stubStore{}, search, Options{})
	pairs, stats, err := b.Build(context.Background(), []model.Section{section})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if len(pairs) != 0 || stats.Dropped != 1 {
		t.Fatalf("pairs = %v, stats = %+v, want the query dropped", pairs, stats)
	}
}

func TestSample(t *testing.T) {
	var sections []model.Section
	for i := range 100 {
		content := "short"
		if i%2 == 0 {
			content = "a section long enough to keep"
		}
		sections = append(sections, &stubSection{id: model.SectionID(string(rune('a' + i%26))), content: content})
	}
	walk := func(fn func(model.Section) error) error {
		for _, s := range sections {
			if err := fn(s); err != nil {
				return err
			}
		}
		return nil
	}

	sample, err := Sample(walk, 10, All(Leaves(), MinWords(3)), 1)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if len(sample) != 10 {
		t.Fatalf("sample size = %d, want 10", len(sample))
	}
	for _, s := range sample {
		if c, _ := s.Content(); string(c) == "short" {
			t.Fatalf("sample kept a filtered section")
		}
	}
}
