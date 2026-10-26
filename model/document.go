package model

import (
	"net/url"
	"strings"

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
	// Chunk returns the bytes of the content in [start:end]. It clamps a start
	// below 0 and an end past the content, and returns an error when start
	// still lies past end. model/testsuite.TestChunk checks this contract.
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

// ReservedMetadataPrefix marks the metadata keys reserved to the application
// embedding the library — access control lists, typically. They are stored,
// indexed and filterable like any other key, but never shown: PublicMetadata
// leaves them out.
const ReservedMetadataPrefix = "_"

// PublicMetadata returns the metadata of d without its reserved keys (see
// ReservedMetadataPrefix), or nil when nothing remains.
func PublicMetadata(d Document) map[string]any {
	metadata := Metadata(d)
	if len(metadata) == 0 {
		return nil
	}

	public := make(map[string]any, len(metadata))
	for k, v := range metadata {
		if !strings.HasPrefix(k, ReservedMetadataPrefix) {
			public[k] = v
		}
	}

	if len(public) == 0 {
		return nil
	}

	return public
}
