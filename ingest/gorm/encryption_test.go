package gorm

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

const encryptionTestKey = "a-test-key-with-at-least-32-bytes!"

const sentence = "THE TREASURE IS BURIED UNDER THE OLD OAK"

const encryptedTestDocument = `# Secret note

` + sentence + `

## Details

The map is in the drawer.
`

func saveTestDocument(t *testing.T, ctx context.Context, store *Store, rawURL string) model.Document {
	t.Helper()

	doc, err := markdown.Parse([]byte(encryptedTestDocument))
	if err != nil {
		t.Fatalf("could not parse document: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse(rawURL)
	doc.SetSource(source)

	if err := store.SaveDocuments(ctx, doc); err != nil {
		t.Fatalf("could not save document: %+v", errors.WithStack(err))
	}

	return doc
}

// The whole point of encryption at rest: the database file must not carry
// the content in clear, while the store keeps serving it as before.
func TestEncryption_ContentUnreadableOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "amoxtli.sqlite")

	store, err := NewSQLiteStore(path, WithEncryptionKey(encryptionTestKey))
	if err != nil {
		t.Fatalf("could not create store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	doc := saveTestDocument(t, ctx, store, "https://example.net/secret.md")

	// Full read back, document and sections alike.
	persisted, err := store.GetDocumentByID(ctx, doc.ID())
	if err != nil {
		t.Fatalf("could not retrieve document: %+v", errors.WithStack(err))
	}
	content, err := persisted.Content()
	if err != nil {
		t.Fatalf("could not read content: %+v", errors.WithStack(err))
	}
	if !strings.Contains(string(content), sentence) {
		t.Errorf("document content lost after round trip: %q", content)
	}

	sections := persisted.Sections()
	if len(sections) == 0 {
		t.Fatal("no sections persisted")
	}
	sectionContent, err := sections[0].Content()
	if err != nil {
		t.Fatalf("could not read section content: %+v", errors.WithStack(err))
	}
	if len(sectionContent) == 0 {
		t.Error("section content empty after round trip")
	}

	// The caller's document must not have been sealed in place.
	callerContent, err := doc.Content()
	if err != nil {
		t.Fatalf("could not read caller content: %+v", errors.WithStack(err))
	}
	if !strings.Contains(string(callerContent), sentence) {
		t.Error("the caller's document was left sealed after SaveDocuments")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("could not close store: %+v", errors.WithStack(err))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read database file: %+v", errors.WithStack(err))
	}
	if bytes.Contains(raw, []byte("TREASURE")) {
		t.Error("the database file carries the content in clear")
	}
}

// A store written before encryption keeps reading, and the migration seals
// what was left in clear — once.
func TestEncryption_MigratesExistingClearContent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "amoxtli.sqlite")

	clearStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("could not create store: %+v", errors.WithStack(err))
	}
	doc := saveTestDocument(t, ctx, clearStore, "https://example.net/legacy.md")
	if err := clearStore.Close(); err != nil {
		t.Fatalf("could not close store: %+v", errors.WithStack(err))
	}

	store, err := NewSQLiteStore(path, WithEncryptionKey(encryptionTestKey))
	if err != nil {
		t.Fatalf("could not reopen store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	// Legacy clear content reads transparently.
	persisted, err := store.GetDocumentByID(ctx, doc.ID())
	if err != nil {
		t.Fatalf("could not retrieve legacy document: %+v", errors.WithStack(err))
	}
	content, err := persisted.Content()
	if err != nil {
		t.Fatalf("could not read legacy content: %+v", errors.WithStack(err))
	}
	if !strings.Contains(string(content), sentence) {
		t.Error("legacy clear content unreadable after enabling encryption")
	}

	sealed, skipped, err := store.EncryptExistingDocuments(ctx)
	if err != nil {
		t.Fatalf("migration failed: %+v", errors.WithStack(err))
	}
	if sealed != 1 {
		t.Errorf("sealed %d documents, want 1", sealed)
	}

	sealed, skipped, err = store.EncryptExistingDocuments(ctx)
	if err != nil {
		t.Fatalf("second migration failed: %+v", errors.WithStack(err))
	}
	if sealed != 0 || skipped != 1 {
		t.Errorf("second migration sealed=%d skipped=%d, want 0 and 1", sealed, skipped)
	}

	// Still readable after migration, and gone from the file.
	persisted, err = store.GetDocumentByID(ctx, doc.ID())
	if err != nil {
		t.Fatalf("could not retrieve migrated document: %+v", errors.WithStack(err))
	}
	content, err = persisted.Content()
	if err != nil {
		t.Fatalf("could not read migrated content: %+v", errors.WithStack(err))
	}
	if !strings.Contains(string(content), sentence) {
		t.Error("content unreadable after migration")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("could not close store: %+v", errors.WithStack(err))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read database file: %+v", errors.WithStack(err))
	}
	if bytes.Contains(raw, []byte("TREASURE")) {
		t.Error("the database file still carries clear content after migration")
	}
}
