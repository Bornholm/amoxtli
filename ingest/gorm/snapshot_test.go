package gorm

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// A snapshot restored into an empty store brings the documents back with
// their metadata — and with the destination key, when it has one.
func TestSnapshotRoundTripKeepsMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source, err := NewSQLiteStore(filepath.Join(dir, "source.sqlite"), WithEncryptionKey(encryptionTestKey))
	if err != nil {
		t.Fatalf("could not create store: %+v", errors.WithStack(err))
	}
	defer source.Close()

	doc, err := markdown.Parse([]byte(encryptedTestDocument))
	if err != nil {
		t.Fatalf("could not parse document: %+v", errors.WithStack(err))
	}
	u, _ := url.Parse("https://example.net/secret.md")
	doc.SetSource(u)
	doc.SetMetadata(map[string]any{"client": "acme", "year": float64(2024), "tags": []any{"bail", "loyer"}})
	if err := source.SaveDocuments(ctx, doc); err != nil {
		t.Fatalf("could not save document: %+v", errors.WithStack(err))
	}

	snapshot, err := source.GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %+v", errors.WithStack(err))
	}
	defer snapshot.Close()

	target, err := NewSQLiteStore(filepath.Join(dir, "target.sqlite"), WithEncryptionKey("another-key-with-at-least-32-bytes!!"))
	if err != nil {
		t.Fatalf("could not create store: %+v", errors.WithStack(err))
	}
	defer target.Close()
	if err := target.RestoreSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("RestoreSnapshot: %+v", errors.WithStack(err))
	}

	restored, err := target.GetDocumentByID(ctx, doc.ID())
	if err != nil {
		t.Fatalf("could not retrieve document: %+v", errors.WithStack(err))
	}
	metadata := model.Metadata(restored)
	if metadata["client"] != "acme" || metadata["year"] != float64(2024) {
		t.Errorf("metadata lost in the round trip: %v", metadata)
	}
	if tags, _ := metadata["tags"].([]any); len(tags) != 2 {
		t.Errorf("array metadata lost in the round trip: %v", metadata["tags"])
	}
	content, _ := restored.Content()
	if string(content) != encryptedTestDocument {
		t.Errorf("content lost in the round trip: %q", content)
	}
}
