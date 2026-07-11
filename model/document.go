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
