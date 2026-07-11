package backup

import (
	"context"
	"io"
)

type IdentifiedSnapshotable interface {
	Snapshotable
	SnapshotID() string
}

type Snapshotable interface {
	GenerateSnapshot(ctx context.Context) (io.ReadCloser, error)
	RestoreSnapshot(ctx context.Context, r io.Reader) error
}

type identifiedSnapshotable struct {
	id string
	Snapshotable
}

// SnapshotID implements IdentifiedSnapshotable.
func (i *identifiedSnapshotable) SnapshotID() string {
	return i.id
}

var _ IdentifiedSnapshotable = &identifiedSnapshotable{}

func WithSnapshotID(id string, snapshotable Snapshotable) IdentifiedSnapshotable {
	return &identifiedSnapshotable{
		id:           id,
		Snapshotable: snapshotable,
	}
}
