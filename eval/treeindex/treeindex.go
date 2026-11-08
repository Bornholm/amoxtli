// Package treeindex evaluates tree-aware retrieval inside a single document,
// in the spirit of PageIndex: instead of ranking a document's sections as a
// flat list, the section tree (headings, subsections) is used as a signal.
//
// A document is parsed with amoxtli's Markdown splitter. Each section's own
// text (its heading and the text before its first subsection or split) is a
// unit: the units partition the document and are what gets ranked. A question
// is answered by the units that contain its evidence paragraphs.
package treeindex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// Unit is one section of a document's tree.
type Unit struct {
	// Index is the unit's position in Paper.Units (document order).
	Index int
	// Title is the section heading, empty for a split continuing its parent.
	Title string
	// Text is the section's own text: its heading line, then everything up
	// to its first subsection.
	Text string
	// Body is Text without the heading line.
	Body string
	// Path lists the headings from the top-level section down to this unit
	// (the document title excluded).
	Path     []string
	Parent   *Unit
	Children []*Unit
	Depth    int
	// Rank is the unit's position in Paper.Ranked, -1 when the unit is too
	// short to be ranked (a heading opening subsections, the title).
	Rank int
}

// Paper is a parsed document.
type Paper struct {
	ID     string
	Title  string
	Root   *Unit
	Units  []*Unit
	Ranked []*Unit
}

// Question is a question about one paper and the units holding its evidence.
type Question struct {
	Paper    string   `json:"paper"`
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Evidence []string `json:"evidence"`
	// Gold holds the Rank of the units containing the evidence.
	Gold map[int]bool `json:"-"`
}

// MinBodyWords is the number of words below which a unit is not ranked.
const MinBodyWords = 5

// Split is an evaluation split: papers and their questions.
type Split struct {
	Papers    map[string]*Paper
	Questions []*Question
	// Unmatched counts the questions dropped because none of their evidence
	// was found in the parsed document.
	Unmatched int
}

// LoadSplit reads <dir>/papers/*.md and <dir>/questions.jsonl, as written by
// scripts/qasper_markdown.py.
func LoadSplit(dir string) (*Split, error) {
	files, err := filepath.Glob(filepath.Join(dir, "papers", "*.md"))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	split := &Split{Papers: make(map[string]*Paper, len(files))}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		id := strings.TrimSuffix(filepath.Base(f), ".md")
		paper, err := ParsePaper(id, data)
		if err != nil {
			return nil, errors.Wrapf(err, "paper %s", id)
		}
		split.Papers[id] = paper
	}

	f, err := os.Open(filepath.Join(dir, "questions.jsonl"))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var q Question
		if err := json.Unmarshal(scanner.Bytes(), &q); err != nil {
			return nil, errors.WithStack(err)
		}
		paper, ok := split.Papers[q.Paper]
		if !ok {
			return nil, errors.Errorf("question %s: unknown paper %s", q.ID, q.Paper)
		}
		q.Gold = Locate(paper, q.Evidence)
		if len(q.Gold) == 0 {
			split.Unmatched++
			continue
		}
		split.Questions = append(split.Questions, &q)
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	return split, nil
}

// ParsePaper splits a Markdown document into its unit tree.
func ParsePaper(id string, data []byte) (*Paper, error) {
	doc, err := markdown.Parse(data)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	paper := &Paper{ID: id}

	var build func(s model.Section, parent *Unit, depth int) *Unit
	build = func(s model.Section, parent *Unit, depth int) *Unit {
		end := s.End()
		if children := s.Sections(); len(children) > 0 {
			end = children[0].Start()
		}
		text := strings.TrimSpace(string(data[s.Start():end]))

		u := &Unit{Index: len(paper.Units), Text: text, Body: text, Parent: parent, Depth: depth, Rank: -1}
		if strings.HasPrefix(text, "#") {
			heading, body, _ := strings.Cut(text, "\n")
			u.Title = strings.TrimSpace(strings.TrimLeft(heading, "#"))
			u.Body = strings.TrimSpace(body)
		}
		paper.Units = append(paper.Units, u)

		if parent != nil {
			u.Path = append(append([]string{}, parent.Path...), u.Title)
			if u.Title == "" {
				u.Path = parent.Path
			}
		}

		for _, c := range s.Sections() {
			u.Children = append(u.Children, build(c, u, depth+1))
		}
		return u
	}

	roots := doc.Sections()
	if len(roots) != 1 {
		return nil, errors.Errorf("expected one root section, got %d", len(roots))
	}
	paper.Root = build(roots[0], nil, 0)
	paper.Title = paper.Root.Title
	paper.Root.Path = nil

	for _, u := range paper.Units {
		if u != paper.Root && len(strings.Fields(u.Body)) >= MinBodyWords {
			u.Rank = len(paper.Ranked)
			paper.Ranked = append(paper.Ranked, u)
		}
	}

	return paper, nil
}

// Locate returns the ranked units containing the evidence paragraphs. A
// paragraph is found in a unit that contains it, in a unit it contains (a
// paragraph cut by a split), or in a unit containing its first or last words.
func Locate(paper *Paper, evidence []string) map[int]bool {
	const shingle = 12

	gold := make(map[int]bool)
	for _, e := range evidence {
		words := Words(e)
		if len(words) == 0 {
			continue
		}
		whole := pad(words)
		var ends []string
		if len(words) > shingle {
			ends = []string{pad(words[:shingle]), pad(words[len(words)-shingle:])}
		}
		for _, u := range paper.Ranked {
			text := pad(Words(u.Text))
			body := Words(u.Body)
			switch {
			case strings.Contains(text, whole):
			case len(body) >= shingle && strings.Contains(whole, pad(body)):
			case len(ends) > 0 && (strings.Contains(text, ends[0]) || strings.Contains(text, ends[1])):
			default:
				continue
			}
			gold[u.Rank] = true
		}
	}
	return gold
}

// Words lowercases a text and splits it into letter and digit runs.
func Words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func pad(words []string) string {
	return " " + strings.Join(words, " ") + " "
}

// Truncate keeps the first n words of s.
func Truncate(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) <= n {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:n], " ")
}
