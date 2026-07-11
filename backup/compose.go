package backup

import (
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/textproto"

	"github.com/pkg/errors"
)

const (
	headerSnapshotID = "snapshot-id"
)

func ComposeSnapshots(boundary string, snapshotables ...IdentifiedSnapshotable) *Composite {
	return &Composite{boundary: boundary, snapshotables: snapshotables}
}

type Composite struct {
	boundary      string
	snapshotables []IdentifiedSnapshotable
}

// GenerateSnapshot implements port.Snapshotable.
func (c *Composite) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	r, w := io.Pipe()

	multi := multipart.NewWriter(w)

	if err := multi.SetBoundary(c.boundary); err != nil {
		return nil, errors.WithStack(err)
	}

	go func() {
		defer w.Close()
		defer multi.Close()

		for _, snapshotable := range c.snapshotables {
			if err := c.writeSnapshotablePart(ctx, multi, snapshotable); err != nil {
				w.CloseWithError(errors.Wrap(err, "could not write snapshotable part"))
				return
			}
		}
	}()

	return io.NopCloser(r), nil
}

func (c *Composite) writeSnapshotablePart(ctx context.Context, multi *multipart.Writer, snapshotable IdentifiedSnapshotable) error {
	partHeader := textproto.MIMEHeader{}
	partHeader.Set(headerSnapshotID, snapshotable.SnapshotID())

	partWriter, err := multi.CreatePart(partHeader)
	if err != nil {
		return errors.WithStack(err)
	}

	snapshot, err := snapshotable.GenerateSnapshot(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	defer func() {
		if err := snapshot.Close(); err != nil {
			slog.ErrorContext(ctx, "could not close snapshot", slog.Any("error", errors.WithStack(err)))
		}
	}()

	if _, err := io.Copy(partWriter, snapshot); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// RestoreSnapshot implements port.Snapshotable.
func (c *Composite) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	reader := multipart.NewReader(r, c.boundary)

	for {
		part, err := reader.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return errors.WithStack(err)
		}

		if err := c.restoreSnapshotPart(ctx, part); err != nil {
			return errors.WithStack(err)
		}
	}
}

func (c *Composite) restoreSnapshotPart(ctx context.Context, part *multipart.Part) error {
	defer part.Close()

	snapshotID := part.Header.Get(headerSnapshotID)

	var snapshotable IdentifiedSnapshotable
	for _, s := range c.snapshotables {
		if s.SnapshotID() != snapshotID {
			continue
		}

		snapshotable = s
		break
	}

	if snapshotable == nil {
		slog.WarnContext(ctx, "ignoring unknown snapshot id", slog.String("snapshotID", snapshotID))
		return nil
	}

	slog.DebugContext(ctx, "restoring snapshot part", slog.String("snapshotID", snapshotID))

	if err := snapshotable.RestoreSnapshot(ctx, part); err != nil {
		return errors.Wrapf(err, "could not restore snapshot '%s'", snapshotID)
	}

	return nil
}

var _ Snapshotable = &Composite{}
