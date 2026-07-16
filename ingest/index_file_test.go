package ingest

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
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
