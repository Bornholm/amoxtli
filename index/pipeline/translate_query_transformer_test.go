package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// translateStubLLM returns a canned JSON translation and records its calls.
type translateStubLLM struct {
	llm.Client
	response string
	calls    int
}

func (m *translateStubLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	m.calls++
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, m.response),
		llm.NewChatCompletionUsage(0, 0, 0),
	), nil
}

type stubLanguageLister struct {
	languages []string
	err       error
	gotIDs    []model.CollectionID
}

func (s *stubLanguageLister) ListCollectionLanguages(ctx context.Context, ids []model.CollectionID) ([]string, error) {
	s.gotIDs = ids
	return s.languages, s.err
}

// A French question over an English corpus must reach the lexical index with
// the English terms appended — the whole point of the transformer — while the
// original wording is preserved.
func TestTranslateQueryWidensWithCorpusLanguage(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[{"lang":"en","text":"dog breeds"}]}`}
	lister := &stubLanguageLister{languages: []string{"en"}}

	transformer := NewTranslateQueryTransformer(client, lister)

	got, err := transformer.TransformQuery(context.Background(), "races de chien", index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if !strings.Contains(got, "races de chien") {
		t.Errorf("transformed query %q lost the original wording", got)
	}
	if !strings.Contains(got, "dog breeds") {
		t.Errorf("transformed query %q is missing the translation", got)
	}
	if client.calls != 1 {
		t.Errorf("LLM calls = %d, want 1", client.calls)
	}
}

// A monolingual corpus queried in its own language has nothing to gain, and
// must not pay for an LLM round-trip.
func TestTranslateQuerySkipsWhenQueryMatchesCorpusLanguage(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[{"lang":"fr","text":"never"}]}`}
	lister := &stubLanguageLister{languages: []string{"fr"}}

	query := "Quelles sont les principales races de chiens de berger élevées en France et en Belgique ?"

	got, err := transformer(client, lister).TransformQuery(context.Background(), query, index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if got != query {
		t.Errorf("transformed query = %q, want the original %q", got, query)
	}
	if client.calls != 0 {
		t.Errorf("LLM calls = %d, want 0 for a corpus already in the query's language", client.calls)
	}
}

// A corpus whose documents carry no detected language gives nothing to
// translate into; the query must go through untouched and free.
func TestTranslateQuerySkipsWithoutCorpusLanguages(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[{"lang":"en","text":"never"}]}`}
	lister := &stubLanguageLister{languages: nil}

	got, err := transformer(client, lister).TransformQuery(context.Background(), "races de chien", index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if got != "races de chien" {
		t.Errorf("transformed query = %q, want it untouched", got)
	}
	if client.calls != 0 {
		t.Errorf("LLM calls = %d, want 0 without any corpus language", client.calls)
	}
}

// A translation identical to the query (the model echoing it back for a
// language it is already written in) must not be appended twice: repeating
// terms would skew the frequencies BM25 scores on.
func TestTranslateQueryDoesNotRepeatIdenticalTranslation(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[{"lang":"en","text":"Dog Breeds "},{"lang":"de","text":"Hunderassen"}]}`}
	lister := &stubLanguageLister{languages: []string{"en", "de"}}

	got, err := transformer(client, lister).TransformQuery(context.Background(), "dog breeds", index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if n := strings.Count(strings.ToLower(got), "dog breeds"); n != 1 {
		t.Errorf("transformed query %q repeats the original %d times, want 1", got, n)
	}
	if !strings.Contains(got, "Hunderassen") {
		t.Errorf("transformed query %q dropped the distinct translation", got)
	}
}

// Only the most represented languages are targeted: a long tail would dilute
// the lexical query.
func TestTranslateQueryHonoursMaxTargetLanguages(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[]}`}
	lister := &stubLanguageLister{languages: []string{"en", "de", "es", "it"}}

	tr := NewTranslateQueryTransformer(client, lister, WithMaxTargetLanguages(2))

	targets, err := tr.targetLanguages(context.Background(), "races de chien", index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(targets) != 2 {
		t.Errorf("target languages = %v, want the 2 most represented", targets)
	}
}

// The search's collection scope must reach the language inventory: translating
// into a language absent from the searched collections would be wasted.
func TestTranslateQueryScopesLanguagesToSearchedCollections(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[{"lang":"en","text":"dog breeds"}]}`}
	lister := &stubLanguageLister{languages: []string{"en"}}

	opts := index.SearchOptions{Collections: []model.CollectionID{"coll-1"}}

	if _, err := transformer(client, lister).TransformQuery(context.Background(), "races de chien", opts); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(lister.gotIDs) != 1 || lister.gotIDs[0] != "coll-1" {
		t.Errorf("language inventory queried with %v, want [coll-1]", lister.gotIDs)
	}
}

func transformer(client llm.Client, lister CollectionLanguageLister) *TranslateQueryTransformer {
	return NewTranslateQueryTransformer(client, lister)
}

// The cap must apply to the languages left to translate into, not to the corpus
// inventory: on a corpus dominated by the query's own language, capping first
// would spend the budget on the one language needing no translation.
func TestTranslateQueryCapAppliesAfterExcludingQueryLanguage(t *testing.T) {
	client := &translateStubLLM{response: `{"translations":[]}`}
	// French dominates the corpus, English follows.
	lister := &stubLanguageLister{languages: []string{"fr", "en"}}

	tr := NewTranslateQueryTransformer(client, lister, WithMaxTargetLanguages(1))

	query := "Quelles sont les principales races de chiens de berger élevées en France et en Belgique ?"

	targets, err := tr.targetLanguages(context.Background(), query, index.SearchOptions{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(targets) != 1 || targets[0] != "en" {
		t.Errorf("target languages = %v, want [en] (fr is the query's own language)", targets)
	}
}
