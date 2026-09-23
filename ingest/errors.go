package ingest

import "errors"

var (
	ErrNotFound = errors.New("not found")

	// ErrCursorFilterMismatch is returned when a search cursor issued for one
	// metadata filter is replayed with a different one. The cursor anchors a
	// position inside a filtered ordering, so honouring it under another filter
	// would silently return duplicated or skipped results. Clients receiving it
	// must restart from the first page.
	ErrCursorFilterMismatch = errors.New("search cursor was issued for a different filter")

	// ErrStagedFileMissing is returned when an indexing task cannot find the
	// file staged by IndexFile. When several processes share a persistent task
	// runner, any of them may run the task: the staging directory must then be
	// shared between them (see WithManagerStagingDir).
	ErrStagedFileMissing = errors.New("staged file not found: the staging directory must be shared by the processes sharing the task runner")
)
