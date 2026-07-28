package vision

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// 1x1 transparent PNG.
var pngPixel = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

func TestLLMDescriberStructuredResponse(t *testing.T) {
	client := &stubClient{
		content: `{"title":"Diagramme d'architecture","description":"Un schéma reliant le convertisseur à l'index.","text":"ingestion\nindex"}`,
	}

	describer := NewLLMDescriber(client)

	desc, err := describer.Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "Diagramme d'architecture", desc.Title; e != g {
		t.Errorf("desc.Title: expected %q, got %q", e, g)
	}

	if e, g := "Un schéma reliant le convertisseur à l'index.", desc.Description; e != g {
		t.Errorf("desc.Description: expected %q, got %q", e, g)
	}

	if e, g := "ingestion\nindex", desc.Text; e != g {
		t.Errorf("desc.Text: expected %q, got %q", e, g)
	}

	// The image must travel as a base64 image attachment carrying the MIME type.
	if len(client.opts.Messages) != 1 {
		t.Fatalf("len(messages): expected 1, got %d", len(client.opts.Messages))
	}

	attachments := client.opts.Messages[0].Attachments()
	if len(attachments) != 1 {
		t.Fatalf("len(attachments): expected 1, got %d", len(attachments))
	}

	attachment := attachments[0]

	if e, g := llm.AttachmentTypeImage, attachment.Type(); e != g {
		t.Errorf("attachment.Type(): expected %q, got %q", e, g)
	}

	if e, g := "image/png", attachment.MimeType(); e != g {
		t.Errorf("attachment.MimeType(): expected %q, got %q", e, g)
	}

	if e, g := llm.AttachmentSourceBase64, attachment.Source(); e != g {
		t.Errorf("attachment.Source(): expected %q, got %q", e, g)
	}

	if e, g := base64.StdEncoding.EncodeToString(pngPixel), attachment.Data(); e != g {
		t.Errorf("attachment.Data(): expected %q, got %q", e, g)
	}

	if client.opts.ResponseFormat != llm.ResponseFormatJSON {
		t.Errorf("opts.ResponseFormat: expected %q, got %q", llm.ResponseFormatJSON, client.opts.ResponseFormat)
	}
}

func TestLLMDescriberDetectsMimeType(t *testing.T) {
	client := &stubClient{content: `{"title":"t","description":"d","text":""}`}

	if _, err := NewLLMDescriber(client).Describe(context.Background(), "", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "image/png", client.opts.Messages[0].Attachments()[0].MimeType(); e != g {
		t.Errorf("attachment.MimeType(): expected %q, got %q", e, g)
	}
}

func TestLLMDescriberPlainTextFallback(t *testing.T) {
	// A provider ignoring the response schema answers in prose: the description
	// must survive whole rather than be lost to a parse error.
	client := &stubClient{content: "  Une capture d'écran du tableau de bord.  "}

	desc, err := NewLLMDescriber(client).Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "Une capture d'écran du tableau de bord.", desc.Description; e != g {
		t.Errorf("desc.Description: expected %q, got %q", e, g)
	}

	if desc.Title != "" {
		t.Errorf("desc.Title: expected empty, got %q", desc.Title)
	}
}

func TestLLMDescriberCustomPromptAndNoStructuredOutput(t *testing.T) {
	client := &stubClient{content: "description"}

	describer := NewLLMDescriber(client,
		WithPrompt("décris cette image"),
		WithStructuredOutput(false),
	)

	if _, err := describer.Describe(context.Background(), "image/png", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "décris cette image", client.opts.Messages[0].Content(); e != g {
		t.Errorf("message content: expected %q, got %q", e, g)
	}

	if client.opts.ResponseSchema != nil {
		t.Error("opts.ResponseSchema: expected none when structured output is disabled")
	}
}

func TestLLMDescriberRejectsOversizedImage(t *testing.T) {
	client := &stubClient{content: "description"}

	describer := NewLLMDescriber(client, WithMaxImageBytes(8))

	_, err := describer.Describe(context.Background(), "image/png", pngPixel)
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %+v", err)
	}

	if client.calls != 0 {
		t.Errorf("client.calls: expected 0 (no LLM call for an oversized image), got %d", client.calls)
	}
}

func TestLLMDescriberPropagatesError(t *testing.T) {
	expected := errors.New("provider unavailable")
	client := &stubClient{err: expected}

	_, err := NewLLMDescriber(client).Describe(context.Background(), "image/png", pngPixel)
	if !errors.Is(err, expected) {
		t.Fatalf("expected %+v, got %+v", expected, err)
	}
}

func TestNamespace(t *testing.T) {
	if e, g := "model-a", Namespace("model-a", ""); e != g {
		t.Errorf("Namespace with default prompt: expected %q, got %q", e, g)
	}

	if e, g := "model-a", Namespace("model-a", DefaultPrompt); e != g {
		t.Errorf("Namespace with explicit default prompt: expected %q, got %q", e, g)
	}

	custom := Namespace("model-a", "prompt personnalisé")
	if !strings.HasPrefix(custom, "model-a#") {
		t.Errorf("Namespace with custom prompt: expected a model-a# prefix, got %q", custom)
	}

	if custom == Namespace("model-a", "autre prompt") {
		t.Error("Namespace: two different prompts must not share a namespace")
	}

	if Namespace("model-a", "prompt personnalisé") == Namespace("model-b", "prompt personnalisé") {
		t.Error("Namespace: two different models must not share a namespace")
	}
}

// stubClient captures the last chat completion options and replays a canned
// answer.
type stubClient struct {
	content string
	err     error

	calls int
	opts  *llm.ChatCompletionOptions
}

func (c *stubClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.calls++
	c.opts = llm.NewChatCompletionOptions(funcs...)

	if c.err != nil {
		return nil, c.err
	}

	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, c.content),
		llm.NewChatCompletionUsage(0, 0, 0),
	), nil
}

func (c *stubClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

func (c *stubClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	return nil, errors.New("not implemented")
}

var _ llm.Client = &stubClient{}

func mustDecodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}

	return decoded
}

func TestLLMDescriberDemotesOverlongTitle(t *testing.T) {
	// Some models answer with a whole sentence — sometimes the instructions
	// themselves — in the title field; it would become the markdown heading.
	long := strings.Repeat("mot ", 60)

	client := &stubClient{content: `{"title":"` + long + `","description":"Une description.","text":""}`}

	desc, err := NewLLMDescriber(client).Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if desc.Title != "" {
		t.Errorf("desc.Title: expected it demoted, got %q", desc.Title)
	}

	if !strings.Contains(desc.Description, "mot mot") {
		t.Errorf("desc.Description: expected the demoted title kept, got %q", desc.Description)
	}

	if !strings.Contains(desc.Description, "Une description.") {
		t.Errorf("desc.Description: expected the original description kept, got %q", desc.Description)
	}
}

func TestLLMDescriberKeepsPlausibleTitle(t *testing.T) {
	client := &stubClient{content: `{"title":"Diagramme d'architecture","description":"Une description.","text":""}`}

	desc, err := NewLLMDescriber(client).Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "Diagramme d'architecture", desc.Title; e != g {
		t.Errorf("desc.Title: expected %q, got %q", e, g)
	}
}
