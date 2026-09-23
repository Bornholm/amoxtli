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

	// ErrStagedFileMissing reports that an indexing task could not find its
	// staged file, typically because the staging directory is not shared by
	// the processes sharing a persistent task runner.
	ErrStagedFileMissing = ingest.ErrStagedFileMissing
)
