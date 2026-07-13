package model

import (
	"net/url"

	"github.com/rs/xid"
)

type DocumentID string

func NewDocumentID() DocumentID {
	return DocumentID(xid.New().String())
}

type Document interface {
	WithID[DocumentID]

	Source() *url.URL
	ETag() string
	Collections() []Collection
	Sections() []Section
	Content() ([]byte, error)
	Chunk(start, end int) ([]byte, error)
}

type PersistedDocument interface {
	Document
	WithLifecycle
}

// WithMetadata is an optional capability a Document may implement to carry
// arbitrary key/value metadata (author, tags, dates, ...). It is used for
// metadata filtering at search time. Implementations that don't need metadata
// can simply not implement it.
type WithMetadata interface {
	Metadata() map[string]any
}

// Metadata returns the document's metadata when it implements WithMetadata, or
// nil otherwise.
func Metadata(d Document) map[string]any {
	if m, ok := d.(WithMetadata); ok {
		return m.Metadata()
	}
	return nil
}
