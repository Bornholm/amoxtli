package gorm

import (
	"testing"

	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/model/testsuite"
)

func TestWrappedDocumentChunk(t *testing.T) {
	testsuite.TestChunk(t, func(t *testing.T, data []byte) model.Document {
		return &wrappedDocument{d: &Document{Content: data}}
	})
}

func TestSnapshottedDocumentChunk(t *testing.T) {
	testsuite.TestChunk(t, func(t *testing.T, data []byte) model.Document {
		return &snapshottedDocumentWrapper{snapshot: SnapshottedDocument{Content: data}}
	})
}
