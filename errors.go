package amoxtli

import (
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/task"
)

var (
	ErrNotFound = ingest.ErrNotFound
	ErrCanceled = task.ErrCanceled
	// ErrCursorFilterMismatch signals a cursor replayed with a different search
	// filter than the one it was issued for: restart from the first page.
	ErrCursorFilterMismatch = ingest.ErrCursorFilterMismatch
)
