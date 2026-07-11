package gorm

import (
	"database/sql/driver"
	"strings"
	"time"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

type Section struct {
	ID string `gorm:"primaryKey;autoIncrement:false"`

	CreatedAt time.Time
	UpdatedAt time.Time

	Document   *Document
	DocumentID string

	Parent   *Section
	ParentID *string

	// Fixed: Self-referencing relationship should only use ParentID as foreign key
	// ParentID references the ID field of the same Section table
	Sections []*Section `gorm:"foreignKey:ParentID;references:ID;constraint:OnDelete:CASCADE"`

	Branch *Branch
	Level  uint

	Start int
	End   int
}

type wrappedSection struct {
	s *Section
}

// End implements model.Section.
func (w *wrappedSection) End() int {
	return w.s.End
}

// Start implements model.Section.
func (w *wrappedSection) Start() int {
	return w.s.Start
}

// Branch implements model.Section.
func (w *wrappedSection) Branch() []model.SectionID {
	return *w.s.Branch
}

// Level implements model.Section.
func (w *wrappedSection) Level() uint {
	return w.s.Level
}

// Content implements model.Section.
func (w *wrappedSection) Content() ([]byte, error) {
	return w.Document().Chunk(w.s.Start, w.s.End)
}

// Document implements model.Section.
func (w *wrappedSection) Document() model.Document {
	return &wrappedDocument{w.s.Document}
}

// ID implements model.Section.
func (w *wrappedSection) ID() model.SectionID {
	return model.SectionID(w.s.ID)
}

// Parent implements model.Section.
func (w *wrappedSection) Parent() model.Section {
	if w.s.Parent == nil {
		return nil
	}

	return &wrappedSection{s: w.s.Parent}
}

// Sections implements model.Section.
func (w *wrappedSection) Sections() []model.Section {
	sections := make([]model.Section, 0, len(w.s.Sections))
	for _, s := range w.s.Sections {
		s.Document = w.s.Document
		sections = append(sections, &wrappedSection{s})
	}
	return sections
}

var _ model.Section = &wrappedSection{}

func fromSection(d *Document, s model.Section) *Section {
	branch := Branch(s.Branch())
	section := &Section{
		ID:         string(s.ID()),
		Document:   d,
		DocumentID: d.ID,
		Branch:     &branch,
		Sections:   make([]*Section, 0, len(s.Sections())),
		Start:      s.Start(),
		End:        s.End(),
	}

	for _, s := range s.Sections() {
		ss := fromSection(d, s)
		ss.Parent = section
		section.Sections = append(section.Sections, ss)
	}

	return section
}

type Branch []model.SectionID

func (b *Branch) Scan(value any) error {
	// Depending on the SQL driver, a text column may be scanned as a string
	// (SQLite, pgx) or as a []byte (lib/pq); accept both.
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return errors.Errorf("unexpected type '%T'", value)
	}

	parts := strings.Split(text, ".")

	bb := make([]model.SectionID, len(parts))
	for i, p := range parts {
		bb[i] = model.SectionID(p)
	}

	*b = bb

	return nil
}

// Value return json value, implement driver.Valuer interface
func (b *Branch) Value() (driver.Value, error) {
	parts := make([]string, len(*b))
	for i, p := range *b {
		parts[i] = string(p)
	}

	return strings.Join(parts, "."), nil
}
