package bleve

import (
	"path/filepath"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/testsuite"
	_ "github.com/bornholm/genai/llm/provider/openai"
	"github.com/pkg/errors"
)

func TestIndex(t *testing.T) {
	testsuite.TestIndex(t, func(t *testing.T) (index.Index, error) {
		// Each factory call gets its own isolated directory so subtests never
		// share on-disk state; t.TempDir() is removed automatically.
		dataDir := filepath.Join(t.TempDir(), "index.bleve")

		mapping := IndexMapping()

		bleveIndex, err := bleve.New(dataDir, mapping)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		t.Cleanup(func() {
			_ = bleveIndex.Close()
		})

		index := NewIndex(bleveIndex)

		return index, nil
	})
}
