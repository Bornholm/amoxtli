package fs

import (
	"testing"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/blob/testsuite"
	"github.com/pkg/errors"
)

func TestStore(t *testing.T) {
	testsuite.TestStore(t, func(t *testing.T) blob.Store {
		store, err := NewStore(t.TempDir(), WithMaxBytes(testsuite.MaxBytes))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}

		return store
	})
}
