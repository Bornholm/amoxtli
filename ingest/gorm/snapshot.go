package gorm

import (
	"context"
	"encoding/gob"
	"io"
	"log/slog"
	"net/url"
	"slices"

	"github.com/bornholm/amoxtli/backup"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func init() {
	gob.Register(SnapshottedDocument{})
	gob.Register(SnapshottedCollection{})
	gob.Register(SnapshottedSection{})
}

// GenerateSnapshot implements backup.Snapshotable.
func (s *Store) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	r, w := io.Pipe()

	go func() {
		defer w.Close()

		encoder := gob.NewEncoder(w)

		page := 0
		limit := 100
		for {
			documents, _, err := s.QueryDocuments(ctx, ingest.QueryDocumentsOptions{
				Page:       &page,
				Limit:      &limit,
				HeaderOnly: true,
			})
			if err != nil {
				w.CloseWithError(errors.WithStack(err))
				return
			}

			if len(documents) == 0 {
				break
			}

			for _, d := range documents {
				d, err := s.GetDocumentByID(ctx, d.ID())
				if err != nil {
					w.CloseWithError(errors.WithStack(err))
					return
				}

				content, err := d.Content()
				if err != nil {
					w.CloseWithError(errors.WithStack(err))
					return
				}

				err = encoder.Encode(SnapshottedDocument{
					ID:          string(d.ID()),
					Source:      d.Source().String(),
					ETag:        d.ETag(),
					Content:     content,
					Collections: toSnapshottedCollections(d.Collections()),
					Sections:    toSnapshottedSections(d.Sections()),
				})
				if err != nil {
					w.CloseWithError(errors.WithStack(err))
					return
				}
			}

			page++
		}

	}()

	return io.NopCloser(r), nil
}

// RestoreSnapshot implements backup.Snapshotable.
func (s *Store) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	decoder := gob.NewDecoder(r)

	slog.DebugContext(ctx, "restoring snapshotted documents")
	defer slog.DebugContext(ctx, "snapshotted documents restored")

	batchSize := 100
	batch := make([]model.Document, 0, batchSize)

	for {
		var doc SnapshottedDocument
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				if len(batch) > 0 {
					if err := s.SaveDocuments(ctx, batch...); err != nil {
						return errors.WithStack(err)
					}
				}

				return nil
			}

			return errors.WithStack(err)
		}

		batch = append(batch, &snapshottedDocumentWrapper{doc})
		if len(batch) >= batchSize {
			if err := s.SaveDocuments(ctx, batch...); err != nil {
				return errors.WithStack(err)
			}

			batch = nil
		}
	}
}

var _ backup.Snapshotable = &Store{}

type SnapshottedDocument struct {
	ID          string
	Source      string
	ETag        string
	Content     []byte
	Collections []SnapshottedCollection
	Sections    []SnapshottedSection
}

type snapshottedDocumentWrapper struct {
	snapshot SnapshottedDocument
}

// ETag implements model.Document.
func (w *snapshottedDocumentWrapper) ETag() string {
	return w.snapshot.ETag
}

// Chunk implements model.Document.
func (w *snapshottedDocumentWrapper) Chunk(start int, end int) ([]byte, error) {
	if start < 0 {
		start = 0
	}

	if end > len(w.snapshot.Content) {
		end = len(w.snapshot.Content)
	}

	return w.snapshot.Content[start:end], nil
}

// Collections implements model.Document.
func (w *snapshottedDocumentWrapper) Collections() []model.Collection {
	var collections []model.Collection
	for _, c := range w.snapshot.Collections {
		collections = append(collections, &snapshottedCollectionWrapper{c})
	}
	return collections
}

// Content implements model.Document.
func (w *snapshottedDocumentWrapper) Content() ([]byte, error) {
	return w.snapshot.Content, nil
}

// ID implements model.Document.
func (w *snapshottedDocumentWrapper) ID() model.DocumentID {
	return model.DocumentID(w.snapshot.ID)
}

// Sections implements model.Document.
func (w *snapshottedDocumentWrapper) Sections() []model.Section {
	var sections []model.Section
	for _, s := range w.snapshot.Sections {
		sections = append(sections, &snapshottedSectionWrapper{
			document: w,
			snapshot: s,
		})
	}
	return sections
}

// Source implements model.Document.
func (w *snapshottedDocumentWrapper) Source() *url.URL {
	source, err := url.Parse(w.snapshot.Source)
	if err != nil {
		panic(errors.WithStack(err))
	}

	return source
}

var _ model.Document = &snapshottedDocumentWrapper{}

type SnapshottedCollection struct {
	ID          string
	Label       string
	Description string
}

type snapshottedCollectionWrapper struct {
	snapshot SnapshottedCollection
}

// Description implements model.Collection.
func (s *snapshottedCollectionWrapper) Description() string {
	return s.snapshot.Description
}

// ID implements model.Collection.
func (s *snapshottedCollectionWrapper) ID() model.CollectionID {
	return model.CollectionID(s.snapshot.ID)
}

// Label implements model.Collection.
func (s *snapshottedCollectionWrapper) Label() string {
	return s.snapshot.Label
}

var _ model.Collection = &snapshottedCollectionWrapper{}

func toSnapshottedCollections(collections []model.Collection) []SnapshottedCollection {
	snapshots := make([]SnapshottedCollection, 0, len(collections))
	for _, c := range collections {
		snapshots = append(snapshots, SnapshottedCollection{
			ID:          string(c.ID()),
			Label:       c.Label(),
			Description: c.Description(),
		})
	}
	return snapshots
}

type SnapshottedSection struct {
	ID       string
	Branch   []string
	Start    int
	End      int
	Level    int
	Sections []SnapshottedSection
}

type snapshottedSectionWrapper struct {
	document model.Document
	parent   model.Section
	snapshot SnapshottedSection
}

// Branch implements model.Section.
func (w *snapshottedSectionWrapper) Branch() []model.SectionID {
	return slices.Collect(func(yield func(model.SectionID) bool) {
		for _, id := range w.snapshot.Branch {
			if !yield(model.SectionID(id)) {
				return
			}
		}
	})
}

// Content implements model.Section.
func (w *snapshottedSectionWrapper) Content() ([]byte, error) {
	return w.Document().Chunk(w.snapshot.Start, w.snapshot.End)
}

// Document implements model.Section.
func (w *snapshottedSectionWrapper) Document() model.Document {
	return w.document
}

// End implements model.Section.
func (w *snapshottedSectionWrapper) End() int {
	return w.snapshot.End
}

// ID implements model.Section.
func (w *snapshottedSectionWrapper) ID() model.SectionID {
	return model.SectionID(w.snapshot.ID)
}

// Level implements model.Section.
func (w *snapshottedSectionWrapper) Level() uint {
	return uint(w.snapshot.Level)
}

// Parent implements model.Section.
func (w *snapshottedSectionWrapper) Parent() model.Section {
	return w.parent
}

// Sections implements model.Section.
func (w *snapshottedSectionWrapper) Sections() []model.Section {
	var sections []model.Section
	for _, s := range w.snapshot.Sections {
		sections = append(sections, &snapshottedSectionWrapper{
			document: w.document,
			parent:   w,
			snapshot: s,
		})
	}
	return sections
}

// Start implements model.Section.
func (w *snapshottedSectionWrapper) Start() int {
	return w.snapshot.Start
}

var _ model.Section = &snapshottedSectionWrapper{}

func toSnapshottedSections(sections []model.Section) []SnapshottedSection {
	snapshots := make([]SnapshottedSection, 0, len(sections))
	for _, s := range sections {
		snapshots = append(snapshots, SnapshottedSection{
			ID: string(s.ID()),
			Branch: slices.Collect(func(yield func(string) bool) {
				for _, id := range s.Branch() {
					if !yield(string(id)) {
						return
					}
				}
			}),
			Start:    s.Start(),
			End:      s.End(),
			Level:    int(s.Level()),
			Sections: toSnapshottedSections(s.Sections()),
		})
	}
	return snapshots
}
