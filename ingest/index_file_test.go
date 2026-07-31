package ingest

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/markdown/imagetext"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

const testSourceFile = `package greeting

// ParseGreetingMessage parses a greeting message.
func ParseGreetingMessage(message string) string {
	return message
}
`

// handlerStore captures the documents saved by IndexFileHandler; the embedded
// nil Store panics on any other call.
type handlerStore struct {
	Store
	saved []model.Document
}

func (s *handlerStore) SaveDocuments(ctx context.Context, documents ...model.Document) error {
	s.saved = append(s.saved, documents...)
	return nil
}

func (s *handlerStore) GetCollectionByID(ctx context.Context, id model.CollectionID, full bool) (model.PersistedCollection, error) {
	return &stubCollection{id: id}, nil
}

type stubCollection struct {
	id model.CollectionID
}

func (c *stubCollection) ID() model.CollectionID { return c.id }
func (c *stubCollection) Label() string          { return string(c.id) }
func (c *stubCollection) Description() string    { return "" }
func (c *stubCollection) CreatedAt() time.Time   { return time.Time{} }
func (c *stubCollection) UpdatedAt() time.Time   { return time.Time{} }

var _ model.PersistedCollection = &stubCollection{}

// runIndexFileTask drives IndexFileHandler.Handle on a staged copy of the
// given content and returns the saved document.
func runIndexFileTask(t *testing.T, originalName string, content string, metadata map[string]any) model.Document {
	t.Helper()

	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	store := &handlerStore{}
	handler := NewIndexFileHandler(store, nil, &stubIndex{}, 250, WithIndexFileHandlerSourceCode(sourcecode.DefaultRegistry()))

	source := &url.URL{Scheme: "file", Path: "/" + originalName}
	tsk := NewIndexFileTask(staged, originalName, "etag", source, []model.CollectionID{"default"}, metadata)

	events := make(chan task.Event, 128)
	go func() {
		for range events {
		}
	}()
	defer close(events)

	if err := handler.Handle(context.Background(), tsk, events); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(store.saved) != 1 {
		t.Fatalf("expected one saved document, got %d", len(store.saved))
	}

	return store.saved[0]
}

func TestIndexFileHandlerSourceCode(t *testing.T) {
	doc := runIndexFileTask(t, "greeting.go", testSourceFile, nil)

	metadata := model.Metadata(doc)

	if e, g := "code", metadata["type"]; e != g {
		t.Errorf("metadata[\"type\"]: expected '%s', got '%v'", e, g)
	}

	if e, g := "go", metadata["language"]; e != g {
		t.Errorf("metadata[\"language\"]: expected '%s', got '%v'", e, g)
	}

	// root section + the function declaration
	if e, g := 2, model.CountSections(doc); e != g {
		t.Errorf("model.CountSections(doc): expected '%d', got '%v'", e, g)
	}
}

func TestIndexFileHandlerSourceCodeMetadataMerge(t *testing.T) {
	doc := runIndexFileTask(t, "greeting.go", testSourceFile, map[string]any{
		"topic":    "greeting",
		"language": "golang", // user-supplied values win over parser-injected ones
	})

	metadata := model.Metadata(doc)

	if e, g := "code", metadata["type"]; e != g {
		t.Errorf("metadata[\"type\"]: expected '%s', got '%v'", e, g)
	}

	if e, g := "golang", metadata["language"]; e != g {
		t.Errorf("metadata[\"language\"]: expected '%s', got '%v'", e, g)
	}

	if e, g := "greeting", metadata["topic"]; e != g {
		t.Errorf("metadata[\"topic\"]: expected '%s', got '%v'", e, g)
	}
}

func TestIndexFileHandlerMarkdownUnchanged(t *testing.T) {
	doc := runIndexFileTask(t, "note.md", "# Title\n\nSome content.\n", map[string]any{"topic": "notes"})

	metadata := model.Metadata(doc)

	if _, exists := metadata["type"]; exists {
		t.Errorf("markdown documents must not be tagged with a type, got %v", metadata["type"])
	}

	if e, g := "notes", metadata["topic"]; e != g {
		t.Errorf("metadata[\"topic\"]: expected '%s', got '%v'", e, g)
	}
}

const testFrenchDocument = `# La recherche hybride

La recherche documentaire hybride combine un canal lexical et un canal
sémantique. Le premier retrouve les termes exacts de la requête, le second
rapproche les formulations différentes d'une même idée.

Leur fusion donne un rappel supérieur à celui de chacun des deux canaux pris
isolément, à condition que les scores soient ramenés sur une échelle commune.
`

func TestIndexFileHandlerDetectsLang(t *testing.T) {
	doc := runIndexFileTask(t, "note.md", testFrenchDocument, nil)

	if e, g := "fr", model.Metadata(doc)[MetadataKeyLang]; e != g {
		t.Errorf("metadata[%q]: expected '%s', got '%v'", MetadataKeyLang, e, g)
	}
}

// An undecidable document must carry no lang at all: a wrong code silently
// excludes it from every lang filter, an absent one does not.
func TestIndexFileHandlerOmitsUndetectableLang(t *testing.T) {
	doc := runIndexFileTask(t, "note.md", "# 42\n\n1234 5678\n", nil)

	if code, exists := model.Metadata(doc)[MetadataKeyLang]; exists {
		t.Errorf("metadata[%q]: expected no language, got '%v'", MetadataKeyLang, code)
	}
}

func TestIndexFileHandlerLangMetadataOverride(t *testing.T) {
	doc := runIndexFileTask(t, "note.md", testFrenchDocument, map[string]any{MetadataKeyLang: "oc"})

	if e, g := "oc", model.Metadata(doc)[MetadataKeyLang]; e != g {
		t.Errorf("metadata[%q]: expected '%s', got '%v'", MetadataKeyLang, e, g)
	}
}

// The natural language of a source file must not clobber the programming
// language the parser records under "language".
func TestIndexFileHandlerLangDoesNotShadowSourceCodeLanguage(t *testing.T) {
	doc := runIndexFileTask(t, "greeting.go", testSourceFile, nil)

	if e, g := "go", model.Metadata(doc)["language"]; e != g {
		t.Errorf("metadata[\"language\"]: expected '%s', got '%v'", e, g)
	}
}

// TestIndexFileHandlerImageEnrichment checks the enrichment step of the
// pipeline: images embedded in a markdown source are described before parsing,
// relative paths resolving against the directory of the *original* file (the
// handler itself works on a staged copy).
func TestIndexFileHandlerImageEnrichment(t *testing.T) {
	corpus := t.TempDir()
	writeTestImage(t, filepath.Join(corpus, "media", "schema.png"))

	const document = `# Architecture

![Schéma](media/schema.png)
`

	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}

	describer := &countingDescriber{
		desc: &vision.Description{Description: "Un schéma reliant le convertisseur à l'index."},
	}

	store := &handlerStore{}
	handler := NewIndexFileHandler(store, nil, &stubIndex{}, 250,
		WithIndexFileHandlerSourceCode(sourcecode.DefaultRegistry()),
		WithIndexFileHandlerImageEnrichment(imagetext.New(imagetext.WithDescriber(describer))),
	)

	source := &url.URL{Scheme: "file", Path: filepath.Join(corpus, "note.md")}
	tsk := NewIndexFileTask(staged, "note.md", "etag", source, []model.CollectionID{"default"}, nil)

	events := make(chan task.Event, 128)

	var messages []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range events {
			if event.Message != nil {
				messages = append(messages, *event.Message)
			}
		}
	}()

	if err := handler.Handle(context.Background(), tsk, events); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	close(events)
	<-done

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	if len(store.saved) != 1 {
		t.Fatalf("expected one saved document, got %d", len(store.saved))
	}

	content, err := store.saved[0].Content()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if !strings.Contains(string(content), "Un schéma reliant le convertisseur à l'index.") {
		t.Errorf("document content: expected the image description, got:\n%s", content)
	}

	if !slices.Contains(messages, "describing images (1/1)") {
		t.Errorf("task events: expected a description progress message, got %v", messages)
	}
}

// TestIndexFileHandlerImageEnrichmentRelativeSource covers the case the
// source cannot serve as a base directory: indexed with --base-dir, it names a
// location relative to a repository root, which does not exist on this
// filesystem. Deriving the directory from it finds no image and, enrichment
// being best-effort, indexes the document stripped of its illustrations
// without any error — hence the explicit base directory.
func TestIndexFileHandlerImageEnrichmentRelativeSource(t *testing.T) {
	corpus := t.TempDir()
	writeTestImage(t, filepath.Join(corpus, "img", "ecran.png"))

	const document = `# Commandes

![L'écran des commandes](img/ecran.png)
`

	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}

	describer := &countingDescriber{
		desc: &vision.Description{Description: "La liste des commandes de la clinique."},
	}

	store := &handlerStore{}
	handler := NewIndexFileHandler(store, nil, &stubIndex{}, 250,
		WithIndexFileHandlerImageEnrichment(imagetext.New(imagetext.WithDescriber(describer))),
	)

	// What the CLI stores under --base-dir: absolute-looking, yet nowhere on
	// this filesystem.
	source := &url.URL{Scheme: "file", Path: "/proj/docs/tutoriel/commandes.md"}
	tsk := NewIndexFileTask(staged, "commandes.md", "etag", source, []model.CollectionID{"default"}, nil,
		WithIndexFileTaskImageBaseDir(corpus),
	)

	events := make(chan task.Event, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events {
		}
	}()

	if err := handler.Handle(context.Background(), tsk, events); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	close(events)
	<-done

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	if len(store.saved) != 1 {
		t.Fatalf("expected one saved document, got %d", len(store.saved))
	}

	content, err := store.saved[0].Content()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if !strings.Contains(string(content), "La liste des commandes de la clinique.") {
		t.Errorf("document content: expected the image description, got:\n%s", content)
	}
}

// TestIndexFileHandlerImageEnrichmentSkipsSourceCode locks that source files
// never go through the markdown enrichment.
func TestIndexFileHandlerImageEnrichmentSkipsSourceCode(t *testing.T) {
	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(testSourceFile), 0600); err != nil {
		t.Fatal(err)
	}

	// A failing enricher would surface immediately if it were ever called on a
	// source file.
	store := &handlerStore{}
	handler := NewIndexFileHandler(store, nil, &stubIndex{}, 250,
		WithIndexFileHandlerSourceCode(sourcecode.DefaultRegistry()),
		WithIndexFileHandlerImageEnrichment(&failingEnricher{}),
	)

	source := &url.URL{Scheme: "file", Path: "/corpus/greeting.go"}
	tsk := NewIndexFileTask(staged, "greeting.go", "etag", source, []model.CollectionID{"default"}, nil)

	events := make(chan task.Event, 128)
	go func() {
		for range events {
		}
	}()
	defer close(events)

	if err := handler.Handle(context.Background(), tsk, events); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
}

// TestIndexFileHandlerImageEnrichmentFailureIsNotFatal checks that a broken
// enrichment degrades to indexing the document as it is.
func TestIndexFileHandlerImageEnrichmentFailureIsNotFatal(t *testing.T) {
	const document = "# Titre\n\nDu contenu.\n"

	staged := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(staged, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}

	store := &handlerStore{}
	handler := NewIndexFileHandler(store, nil, &stubIndex{}, 250,
		WithIndexFileHandlerImageEnrichment(&failingEnricher{}),
	)

	source := &url.URL{Scheme: "file", Path: "/corpus/note.md"}
	tsk := NewIndexFileTask(staged, "note.md", "etag", source, []model.CollectionID{"default"}, nil)

	events := make(chan task.Event, 128)
	go func() {
		for range events {
		}
	}()
	defer close(events)

	if err := handler.Handle(context.Background(), tsk, events); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(store.saved) != 1 {
		t.Fatalf("expected one saved document, got %d", len(store.saved))
	}

	content, err := store.saved[0].Content()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if e, g := document, string(content); e != g {
		t.Errorf("document content: expected %q, got %q", e, g)
	}
}

func TestImageBaseDir(t *testing.T) {
	testCases := []struct {
		Name     string
		Source   *url.URL
		Explicit string
		Expected string
	}{
		{"derived from the source", &url.URL{Scheme: "file", Path: "/corpus/notes/note.md"}, "", "/corpus/notes"},
		{"non-file source", &url.URL{Scheme: "https", Host: "example.net", Path: "/note.md"}, "", ""},
		{"no source", nil, "", ""},
		// A source made relative to a base directory (CLI --base-dir) names a
		// location that does not exist on this filesystem: the explicit
		// directory is the only one the images can be resolved against.
		{"explicit wins over a relative source", &url.URL{Scheme: "file", Path: "/proj/docs/note.md"}, "/srv/repos/proj/docs", "/srv/repos/proj/docs"},
		{"explicit without a source", nil, "/srv/repos/proj/docs", "/srv/repos/proj/docs"},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			var funcs []IndexFileTaskOption
			if tc.Explicit != "" {
				funcs = append(funcs, WithIndexFileTaskImageBaseDir(tc.Explicit))
			}

			tsk := NewIndexFileTask("", "note.md", "", tc.Source, nil, nil, funcs...)

			if e, g := tc.Expected, tsk.ImageBaseDir(); e != g {
				t.Errorf("ImageBaseDir(): expected %q, got %q", e, g)
			}
		})
	}
}

// TestIndexFileTaskImageBaseDirRoundTrip guards the persisted form: the
// directory must survive a task being resumed from its payload, or a restart
// silently reverts to the broken resolution.
func TestIndexFileTaskImageBaseDirRoundTrip(t *testing.T) {
	source := &url.URL{Scheme: "file", Path: "/proj/docs/note.md"}
	tsk := NewIndexFileTask("/staging/x.md", "note.md", "etag", source, nil, nil,
		WithIndexFileTaskImageBaseDir("/srv/repos/proj/docs"),
	)

	payload, err := tsk.MarshalJSON()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	restored, err := IndexFileTaskFactory(tsk.ID(), payload)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if e, g := "/srv/repos/proj/docs", restored.(*IndexFileTask).ImageBaseDir(); e != g {
		t.Errorf("restored ImageBaseDir(): expected %q, got %q", e, g)
	}
}

// writeTestImage encodes a 128x128 PNG at path.
func writeTestImage(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for x := range 128 {
		for y := range 128 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// countingDescriber replays a canned description and counts calls.
type countingDescriber struct {
	desc  *vision.Description
	calls int
}

func (d *countingDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*vision.Description, error) {
	d.calls++
	return d.desc, nil
}

var _ vision.Describer = &countingDescriber{}

// failingEnricher fails every enrichment; the handler must carry on.
type failingEnricher struct{}

func (e *failingEnricher) Enrich(ctx context.Context, data []byte, baseDir string, progress func(done, total int)) ([]byte, error) {
	return nil, errors.New("enrichment unavailable")
}

var _ ImageEnricher = &failingEnricher{}
