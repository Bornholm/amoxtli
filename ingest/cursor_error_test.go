package ingest

import (
	"errors"
	"testing"
)

func TestDecodeCursorReportsInvalidCursor(t *testing.T) {
	for _, cursor := range []string{"!!!", "bm90LWpzb24"} {
		if _, err := decodeCursor(cursor); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%q: expected ErrInvalidCursor, got %+v", cursor, err)
		}
	}
}
