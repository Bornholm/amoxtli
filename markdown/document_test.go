package markdown

import (
	"testing"

	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/model/testsuite"
)

func TestDocumentChunk(t *testing.T) {
	testsuite.TestChunk(t, func(t *testing.T, data []byte) model.Document {
		return &Document{data: data}
	})
}
