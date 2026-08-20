package gorm

import (
	"context"
	"reflect"

	"github.com/bornholm/amoxtli/crypto"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Encryption at rest for document content.
//
// The integration point is a pair of gorm callbacks rather than explicit
// calls at every read and write site: documents are loaded from many places
// (by id, by source, preloaded behind sections and collections), and a
// single forgotten site would leak content in clear without anything
// noticing. The callbacks see every query against this *gorm.DB.
//
// Only Document.Content is sealed. Sections carry no content of their own —
// they address ranges *of the clear content*, which is also why decryption
// happens on load: once the row is in memory, every Chunk(start, end) keeps
// meaning what it always meant. Metadata stays clear on purpose: it is the
// queryable envelope (identifiers, scopes, kinds), and stores filter on it.

// clearContentKey stashes the clear content between the before- and
// after-create callbacks, so the caller's struct is restored once the row
// is written: SaveDocuments keeps working on clear content (blob reference
// scanning), and no caller ever observes its own document sealed.
const clearContentKey = "amoxtli:clear_content"

// registerEncryption arms the callbacks on db.
func registerEncryption(db *gorm.DB, cipher *crypto.Cipher) error {
	if err := db.Callback().Create().Before("gorm:create").Register("amoxtli:seal_content", sealOnCreate(cipher)); err != nil {
		return errors.WithStack(err)
	}
	if err := db.Callback().Create().After("gorm:create").Register("amoxtli:restore_content", restoreAfterCreate()); err != nil {
		return errors.WithStack(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register("amoxtli:open_content", openAfterQuery(cipher)); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func sealOnCreate(cipher *crypto.Cipher) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		if db.Error != nil || db.Statement == nil {
			return
		}

		documents := collectDocuments(db.Statement.ReflectValue)
		if len(documents) == 0 {
			return
		}

		stash := make(map[*Document][]byte, len(documents))
		for _, doc := range documents {
			if len(doc.Content) == 0 || crypto.IsSealed(doc.Content) {
				continue
			}

			sealed, err := cipher.Seal(doc.Content)
			if err != nil {
				_ = db.AddError(errors.WithStack(err))
				return
			}

			stash[doc] = doc.Content
			doc.Content = sealed
		}

		db.InstanceSet(clearContentKey, stash)
	}
}

func restoreAfterCreate() func(db *gorm.DB) {
	return func(db *gorm.DB) {
		value, ok := db.InstanceGet(clearContentKey)
		if !ok {
			return
		}

		stash, ok := value.(map[*Document][]byte)
		if !ok {
			return
		}

		for doc, clear := range stash {
			doc.Content = clear
		}
	}
}

func openAfterQuery(cipher *crypto.Cipher) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		if db.Error != nil || db.Statement == nil {
			return
		}

		for _, doc := range collectDocuments(db.Statement.ReflectValue) {
			clear, err := cipher.Open(doc.Content)
			if err != nil {
				_ = db.AddError(errors.WithStack(err))
				return
			}
			doc.Content = clear
		}
	}
}

// collectDocuments gathers every *Document reachable from v: top level
// results, but also documents preloaded behind sections and collections.
// The visited set guards against the cycles of the model graph
// (Section.Parent, Section.Document.Sections, …).
func collectDocuments(v reflect.Value) []*Document {
	var documents []*Document
	visited := map[uintptr]struct{}{}

	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Ptr:
			if v.IsNil() {
				return
			}
			ptr := v.Pointer()
			if _, seen := visited[ptr]; seen {
				return
			}
			visited[ptr] = struct{}{}
			walk(v.Elem())
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Struct:
			if !v.CanAddr() {
				return
			}
			switch target := v.Addr().Interface().(type) {
			case *Document:
				documents = append(documents, target)
				walk(v.FieldByName("Sections"))
				walk(v.FieldByName("Collections"))
			case *Section:
				walk(v.FieldByName("Document"))
				walk(v.FieldByName("Parent"))
				walk(v.FieldByName("Sections"))
			case *Collection:
				walk(v.FieldByName("Documents"))
			}
		}
	}

	walk(v)

	return documents
}

// EncryptExistingDocuments seals the content of documents still stored in
// clear: enabling encryption only protects what is written afterwards, and
// a store usually carries a whole history.
//
// The operation is resumable — sealed rows are left untouched — and reads
// stay transparent meanwhile: a half migrated store keeps working, clear
// and sealed values living side by side.
func (s *Store) EncryptExistingDocuments(ctx context.Context) (sealed, skipped int, err error) {
	if s.cipher == nil {
		return 0, 0, errors.New("store has no encryption configured")
	}

	db, err := s.getDatabase(ctx)
	if err != nil {
		return 0, 0, errors.WithStack(err)
	}

	// Raw scans and raw updates on purpose: the model callbacks would
	// decrypt on read and re-seal on write, hiding which rows are clear.
	var rows []struct {
		ID      string
		Content []byte
	}
	if err := db.WithContext(ctx).Raw(`SELECT id, content FROM documents`).Scan(&rows).Error; err != nil {
		return 0, 0, errors.WithStack(err)
	}

	for _, row := range rows {
		if len(row.Content) == 0 || crypto.IsSealed(row.Content) {
			skipped++
			continue
		}

		value, err := s.cipher.Seal(row.Content)
		if err != nil {
			return sealed, skipped, errors.WithStack(err)
		}

		if err := db.WithContext(ctx).Exec(`UPDATE documents SET content = ? WHERE id = ?`, value, row.ID).Error; err != nil {
			return sealed, skipped, errors.WithStack(err)
		}
		sealed++
	}

	return sealed, skipped, nil
}
