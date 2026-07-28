package vision_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"strings"
	"testing"
	"unicode"

	convvision "github.com/bornholm/amoxtli/convert/vision"
	"github.com/bornholm/amoxtli/internal/ollamatest"
	"github.com/bornholm/amoxtli/vision"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcollama "github.com/testcontainers/testcontainers-go/modules/ollama"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// defaultVisionModel is a small OCR-oriented vision model: the description
// pipeline is only worth integration-testing against a model that actually
// reads the pixels. Override it with AMOXTLI_TEST_VISION_MODEL.
const defaultVisionModel = "glm-ocr:q8_0"

// defaultOllamaImage is deliberately not the pinned 0.5.7 of the other
// integration tests: the OCR models this test drives need a recent runtime.
// Override it with AMOXTLI_TEST_OLLAMA_IMAGE to pin a known-good version.
const defaultOllamaImage = "ollama/ollama:latest"

// ocrWord is the text drawn into the test image. Uppercase and unambiguous:
// an OCR model must transcribe it verbatim, whatever its language.
const ocrWord = "AMOXTLI"

// promptMarkers are fragments of vision.DefaultPrompt that carry no meaning
// outside of it: seeing one in an answer means the model transcribed the
// instructions instead of the image.
var promptMarkers = []string{
	"say what it shows",
	"transcribe every word",
	"stick to what is visible",
	"a search engine can find it",
}

// TestIntegrationDescribe exercises the describer — and the image converter on
// top of it — against a real vision model served by Ollama. The assertion is
// the OCR one: the word drawn in the image must come back in the description.
func TestIntegrationDescribe(t *testing.T) {
	requireOllama(t)

	ctx := context.Background()

	model := envOr("AMOXTLI_TEST_VISION_MODEL", defaultVisionModel)
	client := newOllamaVisionClient(t, model)

	image := drawTextImage(t, ocrWord)

	t.Run("Describer", func(t *testing.T) {
		describer := vision.NewLLMDescriber(client)

		description, err := describer.Describe(ctx, "image/png", image)
		if err != nil {
			t.Fatalf("could not describe image: %+v", errors.WithStack(err))
		}

		t.Logf("title: %q", description.Title)
		t.Logf("description: %q", description.Description)
		t.Logf("visible text: %q", description.Text)

		if description.IsEmpty() {
			t.Fatal("description: expected a non-empty description")
		}

		// The word may land in Text (structured output honoured) or in
		// Description (the plain-text fallback): both are acceptable outcomes,
		// what matters is that it was read at all.
		transcribed := description.Title + " " + description.Description + " " + description.Text

		if !containsWord(transcribed, ocrWord) {
			t.Errorf("description: expected the transcription of %q, got %q", ocrWord, transcribed)
		}

		requireNoPromptEcho(t, transcribed)
	})

	t.Run("Converter", func(t *testing.T) {
		converter := convvision.NewConverter(vision.NewLLMDescriber(client))

		readCloser, err := converter.Convert(ctx, "schema.png", bytes.NewReader(image))
		if err != nil {
			t.Fatalf("could not convert image: %+v", errors.WithStack(err))
		}
		defer readCloser.Close()

		converted, err := io.ReadAll(readCloser)
		if err != nil {
			t.Fatalf("could not read converted markdown: %+v", errors.WithStack(err))
		}

		t.Logf("markdown:\n%s", converted)

		if !strings.HasPrefix(string(converted), "---\ntype: image\n---\n") {
			t.Errorf("markdown: expected the type=image frontmatter, got:\n%s", converted)
		}

		if !containsWord(string(converted), ocrWord) {
			t.Errorf("markdown: expected the transcription of %q, got:\n%s", ocrWord, converted)
		}

		requireNoPromptEcho(t, string(converted))
	})

	t.Run("CachingDescriber", func(t *testing.T) {
		// A real call is slow and billable-by-analogy: the cache must spare the
		// second one entirely, keyed by the image content.
		cached, err := vision.NewCachingDescriber(vision.NewLLMDescriber(client), t.TempDir(), vision.Namespace(model, ""))
		if err != nil {
			t.Fatalf("could not create cache: %+v", errors.WithStack(err))
		}

		if _, err := cached.Describe(ctx, "image/png", image); err != nil {
			t.Fatalf("could not describe image: %+v", errors.WithStack(err))
		}

		if _, err := cached.Describe(ctx, "image/png", image); err != nil {
			t.Fatalf("could not describe image: %+v", errors.WithStack(err))
		}

		hits, misses := cached.Stats()
		if hits != 1 || misses != 1 {
			t.Errorf("Stats(): expected 1 hit / 1 miss, got %d / %d", hits, misses)
		}
	})
}

// requireNoPromptEcho fails when the answer carries a fragment of the prompt.
// An OCR-specialized model reads everything it is handed, instructions
// included: an earlier phrasing of DefaultPrompt came back verbatim as the
// "description", which would then have been indexed for every image.
func requireNoPromptEcho(t *testing.T, answer string) {
	t.Helper()

	for _, marker := range promptMarkers {
		if containsWord(answer, marker) {
			t.Errorf("answer: the prompt was echoed back (marker %q) in %q", marker, answer)
		}
	}
}

// newOllamaVisionClient starts a disposable Ollama container (reusing the
// "ollama-data" named volume as a model cache across runs), pulls the vision
// model and returns a genai client pointed at its OpenAI-compatible endpoint.
func newOllamaVisionClient(t *testing.T, model string) llm.Client {
	t.Helper()

	ctx := context.Background()

	ollamaImage := envOr("AMOXTLI_TEST_OLLAMA_IMAGE", defaultOllamaImage)

	t.Logf("starting ollama container (%s)", ollamaImage)

	ollamaContainer, err := tcollama.Run(ctx, ollamaImage, testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Mounts: testcontainers.ContainerMounts{
				{
					Source: testcontainers.GenericVolumeMountSource{
						Name: "ollama-data",
					},
					Target: "/root/.ollama",
				},
			},
		},
	}))
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ollamaContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("failed to start container: %+v", errors.WithStack(err))
	}

	ollamatest.EnsureModels(t, ctx, ollamaContainer, model)

	connectionStr, err := ollamaContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	client, err := provider.Create(ctx,
		provider.WithChatCompletion(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   model,
			},
		}),
	)
	if err != nil {
		t.Fatalf("failed to create llm client: %+v", errors.WithStack(err))
	}

	return client
}

// requireOllama gates the integration test behind an explicit opt-in, matching
// the convention of the sqlitevec/postgres/retrieval integration tests.
func requireOllama(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping: requires docker + ollama")
	}

	if os.Getenv("AMOXTLI_TEST_OLLAMA") == "" {
		t.Skip("set AMOXTLI_TEST_OLLAMA=1 to run (requires docker + ollama)")
	}
}

// drawTextImage renders text as large black glyphs on white, using the Go font
// shipped with golang.org/x/image — so the fixture needs no font file on the
// host. It must be rasterized at a real size with antialiasing: an upscaled
// bitmap font produces jagged glyphs that a general-purpose vision model reads
// as "distorted letters" and mistranscribes.
func drawTextImage(t *testing.T, text string) []byte {
	t.Helper()

	const (
		fontSize = 96
		margin   = 48
	)

	parsed, err := opentype.Parse(gobold.TTF)
	if err != nil {
		t.Fatalf("could not parse the test font: %+v", errors.WithStack(err))
	}

	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    fontSize,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		t.Fatalf("could not build the test face: %+v", errors.WithStack(err))
	}
	defer face.Close()

	metrics := face.Metrics()
	width := font.MeasureString(face, text).Ceil() + 2*margin
	height := (metrics.Ascent + metrics.Descent).Ceil() + 2*margin

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	drawer := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(color.Black),
		Face: face,
		Dot:  fixed.P(margin, margin+metrics.Ascent.Ceil()),
	}
	drawer.DrawString(text)

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("could not encode test image: %+v", errors.WithStack(err))
	}

	return buf.Bytes()
}

// containsWord reports whether haystack contains needle, ignoring case and any
// punctuation or spacing a model may have inserted between the letters.
func containsWord(haystack, needle string) bool {
	return strings.Contains(normalize(haystack), normalize(needle))
}

func normalize(value string) string {
	var buf strings.Builder

	for _, r := range strings.ToUpper(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(r)
		}
	}

	return buf.String()
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
