package amoxtli

import (
	"errors"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/task"
)

var (
	ErrNotFound = ingest.ErrNotFound
	ErrCanceled = task.ErrCanceled
	// ErrCursorFilterMismatch signals a cursor replayed with a different search
	// filter than the one it was issued for: restart from the first page.
	ErrCursorFilterMismatch = ingest.ErrCursorFilterMismatch
	// ErrInvalidCursor signals a search cursor that cannot be decoded.
	ErrInvalidCursor = ingest.ErrInvalidCursor

	// ErrStagedFileMissing reports that an indexing task could not find its
	// staged file, typically because the staging directory is not shared by
	// the processes sharing a persistent task runner.
	ErrStagedFileMissing = ingest.ErrStagedFileMissing

	// ErrUnsupported is returned when the configured store lacks an optional
	// capability an operation needs.
	ErrUnsupported = errors.New("operation not supported by the configured store")
)
