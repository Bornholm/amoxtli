package backup

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
)

func TestCompose(t *testing.T) {
	expectedRestoration := 4
	restored := 0

	d1 := &dummySnapshot{data: "d1", onRestore: func(d []byte) {
		defer func() {
			restored++
		}()
		if e, g := "d1", string(d); e != g {
			t.Errorf("d1: expected '%v', got '%v'", e, g)
		}
	}}

	d2 := &dummySnapshot{data: "d2", onRestore: func(d []byte) {
		defer func() {
			restored++
		}()
		if e, g := "d2", string(d); e != g {
			t.Errorf("d2: expected '%v', got '%v'", e, g)
		}
	}}

	d3 := ComposeSnapshots("sub-boundary",
		WithSnapshotID("d3.1", &dummySnapshot{data: "d3.1", onRestore: func(d []byte) {
			defer func() {
				restored++
			}()

			if e, g := "d3.1", string(d); e != g {
				t.Errorf("d3.1: expected '%v', got '%v'", e, g)
			}
		}}),
		WithSnapshotID("d3.2", &dummySnapshot{data: "d3.2", onRestore: func(d []byte) {
			defer func() {
				restored++
			}()

			if e, g := "d3.2", string(d); e != g {
				t.Errorf("d3.2: expected '%v', got '%v'", e, g)
			}
		}}),
	)

	composite := ComposeSnapshots("main-boundary",
		WithSnapshotID("d1", d1),
		WithSnapshotID("d2", d2),
		WithSnapshotID("d3", d3),
	)

	ctx := context.TODO()

	snapshot, err := composite.GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Errorf("%+v", errors.WithStack(err))
		}
	}()

	data, err := io.ReadAll(snapshot)
	if err != nil {
		t.Errorf("%+v", errors.WithStack(err))
	}

	t.Logf("Snapshot: %s", spew.Sdump(data))

	if err := composite.RestoreSnapshot(ctx, bytes.NewBuffer(data)); err != nil {
		t.Errorf("%+v", errors.WithStack(err))
	}

	if e, g := expectedRestoration, restored; e != g {
		t.Errorf("restored: expected '%d', got '%d'", e, g)
	}
}

type dummySnapshot struct {
	data      string
	onRestore func(d []byte)
}

// GenerateSnapshot implements port.Snapshotable.
func (d *dummySnapshot) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(d.data)), nil
}

// RestoreSnapshot implements port.Snapshotable.
func (d *dummySnapshot) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.WithStack(err)
	}

	if d.onRestore != nil {
		d.onRestore(data)
	}

	return nil
}

var _ Snapshotable = &dummySnapshot{}
