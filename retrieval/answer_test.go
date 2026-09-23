package retrieval

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
)

// streamingLLM streams a canned answer in pieces and keeps the prompt.
type streamingLLM struct {
	llm.Client
	pieces []string
	prompt string
}

func (m *streamingLLM) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	opts := llm.NewChatCompletionOptions(funcs...)
	for _, message := range opts.Messages {
		m.prompt += message.Content() + "\n"
	}

	chunks := make(chan llm.StreamChunk, len(m.pieces)+1)
	for _, piece := range m.pieces {
		chunks <- llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, piece))
	}
	chunks <- llm.NewCompleteStreamChunk(llm.NewChatCompletionUsage(0, 0, 0))
	close(chunks)
	return chunks, nil
}

func TestAnswererNumbersSourcesAndStreams(t *testing.T) {
	store := &stubStore{sections: map[model.SectionID]model.Section{
		"a": &stubSection{id: "a", content: "Le loyer est payable par trimestre."},
		"b": &stubSection{id: "b", content: "Le bail dure neuf ans."},
		"c": &stubSection{id: "c", content: "Hors sujet."},
	}}
	client := &streamingLLM{pieces: []string{"Le loyer est trimestriel ", "[1]", " et le bail dure neuf ans [2]."}}

	results := []*index.SearchResult{resultForSource("bail", "a", "b"), resultForSource("autre", "c")}
	sources, deltas, err := NewAnswerer(client, store).Answer(context.Background(), "Quel loyer ?", results, WithAnswerMaxSources(2))
	if err != nil {
		t.Fatalf("Answer: %+v", err)
	}

	if len(sources) != 2 || sources[0].SectionID != "a" || sources[1].Number != 2 {
		t.Fatalf("unexpected sources %+v", sources)
	}
	if !strings.Contains(client.prompt, "### [1] test://bail") || strings.Contains(client.prompt, "Hors sujet") {
		t.Errorf("unexpected prompt:\n%s", client.prompt)
	}

	var answer strings.Builder
	for delta := range deltas {
		if delta.Err != nil {
			t.Fatalf("delta error: %+v", delta.Err)
		}
		answer.WriteString(delta.Text)
	}
	if answer.String() != "Le loyer est trimestriel [1] et le bail dure neuf ans [2]." {
		t.Errorf("unexpected answer %q", answer.String())
	}
}

func TestAnswererWithoutSources(t *testing.T) {
	_, _, err := NewAnswerer(&streamingLLM{}, &stubStore{}).Answer(context.Background(), "q", nil)
	if !errors.Is(err, ErrNoSources) {
		t.Errorf("expected ErrNoSources, got %+v", err)
	}
}

func TestCitations(t *testing.T) {
	got := Citations("Oui [2][1], non [7] ; encore [2] et [0].", 3)
	if !slices.Equal(got, []int{2, 1}) {
		t.Errorf("unexpected citations %v", got)
	}
}
