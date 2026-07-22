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
)
