package pipeline

import (
	"context"
	"io"

	"github.com/bornholm/amoxtli/backup"
	"github.com/pkg/errors"
)

const snapshotBoundary = "corpus-pipeline-index"

// GenerateSnapshot implements backup.Snapshotable.
func (i *Index) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	snapshotable := i.getCompositeSnapshotable()
	snapshot, err := snapshotable.GenerateSnapshot(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return snapshot, nil
}

// RestoreSnapshot implements backup.Snapshotable.
func (i *Index) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	snapshotable := i.getCompositeSnapshotable()
	if err := snapshotable.RestoreSnapshot(ctx, r); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (i *Index) getCompositeSnapshotable() *backup.Composite {
	snapshotables := make([]backup.IdentifiedSnapshotable, 0)

	for identified := range i.indexes {
		if s, ok := identified.Index().(backup.Snapshotable); ok {
			snapshotables = append(snapshotables, backup.WithSnapshotID(identified.ID(), s))
		}
	}

	return backup.ComposeSnapshots(snapshotBoundary, snapshotables...)
}

var _ backup.Snapshotable = &Index{}
