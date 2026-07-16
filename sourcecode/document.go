package sourcecode

import (
	"net/url"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

type Document struct {
	data        []byte
	id          model.DocumentID
	etag        string
	source      *url.URL
	collections []model.Collection
	sections    []*Section
	metadata    map[string]any
}

// ETag implements model.Document.
func (d *Document) ETag() string {
	return d.etag
}

func (d *Document) SetETag(etag string) {
	d.etag = etag
}

// Chunk implements model.Document.
func (d *Document) Chunk(start int, end int) ([]byte, error) {
	if start < 0 {
		start = 0
	}

	if end > len(d.data) {
		end = len(d.data)
	}

	return d.data[start:end], nil
}

// Content implements model.Document.
func (d *Document) Content() ([]byte, error) {
	return d.data, nil
}

func (d *Document) AddCollection(coll model.Collection) {
	d.collections = append(d.collections, coll)
}

// Metadata implements model.WithMetadata.
func (d *Document) Metadata() map[string]any {
	return d.metadata
}

// SetMetadata attaches arbitrary metadata used for filtering at search time.
func (d *Document) SetMetadata(metadata map[string]any) {
	d.metadata = metadata
}

// Collections implements model.Document.
func (d *Document) Collections() []model.Collection {
	return d.collections
}

// ID implements model.Document.
func (d *Document) ID() model.DocumentID {
	return d.id
}

// Sections implements model.Document.
func (d *Document) Sections() []model.Section {
	sections := make([]model.Section, len(d.sections))
	for i, s := range d.sections {
		sections[i] = s
	}
	return sections
}

// Source implements model.Document.
func (d *Document) Source() *url.URL {
	return d.source
}

func (d *Document) SetSource(source *url.URL) {
	d.source = source
}

var (
	_ model.Document     = &Document{}
	_ model.WithMetadata = &Document{}
)

type Section struct {
	id       model.SectionID
	branch   []model.SectionID
	level    uint
	document *Document
	parent   *Section
	sections []*Section
	start    int
	end      int
}

// Content implements model.Section.
//
// Unlike markdown sections, the chunk is returned as-is: source code must not
// be re-rendered or trimmed.
func (s *Section) Content() ([]byte, error) {
	chunk, err := s.Document().Chunk(s.start, s.end)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return chunk, nil
}

// End implements model.Section.
func (s *Section) End() int {
	return s.end
}

// Start implements model.Section.
func (s *Section) Start() int {
	return s.start
}

// Branch implements model.Section.
func (s *Section) Branch() []model.SectionID {
	return s.branch
}

// Level implements model.Section.
func (s *Section) Level() uint {
	return s.level
}

// ID implements model.Section.
func (s *Section) ID() model.SectionID {
	return s.id
}

// Document implements model.Section.
func (s *Section) Document() model.Document {
	return s.document
}

// Parent implements model.Section.
func (s *Section) Parent() model.Section {
	if s.parent == nil {
		return nil
	}

	return s.parent
}

// Sections implements model.Section.
func (s *Section) Sections() []model.Section {
	sections := make([]model.Section, len(s.sections))
	for i, s := range s.sections {
		sections[i] = s
	}
	return sections
}

var _ model.Section = &Section{}
