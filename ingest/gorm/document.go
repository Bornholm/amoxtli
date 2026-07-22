package gorm

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"time"

	"github.com/bornholm/amoxtli/internal/filternorm"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

type Document struct {
	ID        string `gorm:"primaryKey;autoIncrement:false"`
	ETag      string `gorm:"index"`
	CreatedAt time.Time
	UpdatedAt time.Time

	Source      string        `gorm:"unique;not null;index"`
	Sections    []*Section    `gorm:"constraint:OnDelete:CASCADE"`
	Collections []*Collection `gorm:"many2many:documents_collections;"`
	Content     []byte
	// Metadata holds arbitrary document metadata serialized as JSON, used for
	// metadata filtering at search time.
	Metadata []byte
}

type wrappedDocument struct {
	d *Document
}

// CreatedAt implements [model.PersistedDocument].
func (w *wrappedDocument) CreatedAt() time.Time {
	return w.d.CreatedAt
}

// UpdatedAt implements [model.PersistedDocument].
func (w *wrappedDocument) UpdatedAt() time.Time {
	return w.d.UpdatedAt
}

// ETag implements model.Document.
func (w *wrappedDocument) ETag() string {
	return w.d.ETag
}

// Chunk implements model.Document.
func (w *wrappedDocument) Chunk(start int, end int) ([]byte, error) {
	if start < 0 {
		start = 0
	}

	if end > len(w.d.Content) {
		end = len(w.d.Content)
	}

	return w.d.Content[start:end], nil
}

// Content implements model.Document.
func (w *wrappedDocument) Content() ([]byte, error) {
	return w.d.Content, nil
}

// Metadata implements model.WithMetadata.
func (w *wrappedDocument) Metadata() map[string]any {
	if len(w.d.Metadata) == 0 {
		return nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(w.d.Metadata, &metadata); err != nil {
		slog.Warn("could not unmarshal document metadata", slog.String("documentID", w.d.ID), slog.Any("error", errors.WithStack(err)))
		return nil
	}
	return metadata
}

// Collections implements model.Document.
func (w *wrappedDocument) Collections() []model.Collection {
	collections := make([]model.Collection, 0, len(w.d.Collections))
	for _, c := range w.d.Collections {
		collections = append(collections, &wrappedCollection{c})
	}
	return collections
}

// ID implements model.Document.
func (w *wrappedDocument) ID() model.DocumentID {
	return model.DocumentID(w.d.ID)
}

// Sections implements model.Document.
func (w *wrappedDocument) Sections() []model.Section {
	sections := make([]model.Section, 0, len(w.d.Sections))
	for _, s := range w.d.Sections {
		s.Document = w.d
		sections = append(sections, &wrappedSection{s})
	}
	return sections
}

// Source implements model.Document.
func (w *wrappedDocument) Source() *url.URL {
	url, err := url.Parse(w.d.Source)
	if err != nil {
		panic(errors.WithStack(err))
	}

	return url
}

var (
	_ model.PersistedDocument = &wrappedDocument{}
	_ model.WithMetadata      = &wrappedDocument{}
)

func fromDocument(d model.Document) (*Document, error) {
	content, err := d.Content()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Metadata is canonicalized on the way in — notably every date to RFC 3339
	// UTC with fixed precision — so that what is persisted compares identically
	// whether the filter is evaluated in Go or pushed down into SQL, where a
	// date is just text (see index.Filter semantics).
	var metadata []byte
	if m := filternorm.Metadata(model.Metadata(d)); len(m) > 0 {
		metadata, err = json.Marshal(m)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	document := &Document{
		ID:          string(d.ID()),
		ETag:        d.ETag(),
		Source:      d.Source().String(),
		Collections: make([]*Collection, 0, len(d.Collections())),
		Sections:    make([]*Section, 0, len(d.Sections())),
		Content:     content,
		Metadata:    metadata,
	}

	for _, s := range d.Sections() {
		document.Sections = append(document.Sections, fromSection(document, s))
	}

	for _, c := range d.Collections() {
		document.Collections = append(document.Collections, fromCollection(c))
	}

	return document, nil
}
