package eval

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"

	"github.com/pkg/errors"
)

// Query is one golden evaluation case: a natural-language query and the set of
// document source identifiers that are considered relevant answers to it. The
// identifiers must match whatever the Retriever returns (for a Codex-backed
// retriever, the document Source URL as a string).
type Query struct {
	// ID is a stable identifier for the query, used to label per-query results
	// and to segment metrics. Optional but recommended.
	ID string `json:"id,omitempty"`
	// Query is the search string submitted to the retriever.
	Query string `json:"query"`
	// Lang is the ISO language code of the query (e.g. "fr", "en", "es"), used
	// to segment metrics by language.
	Lang string `json:"lang,omitempty"`
	// RelevantSources lists the source identifiers expected in the results.
	RelevantSources []string `json:"relevant_sources"`
	// Tags optionally categorise the query (intent, complexity, domain) for
	// segmented reporting.
	Tags []string `json:"tags,omitempty"`
}

// Dataset is a named collection of golden queries.
type Dataset struct {
	Name    string  `json:"name,omitempty"`
	Queries []Query `json:"queries"`
}

// KeepAnswerable returns a copy of the dataset keeping only the queries whose
// every relevant source is present in the given set — i.e. questions that are
// actually answerable against the (possibly truncated) corpus. Queries with no
// relevant sources are dropped.
func (ds *Dataset) KeepAnswerable(sources map[string]struct{}) *Dataset {
	out := &Dataset{Name: ds.Name}
	for _, q := range ds.Queries {
		if len(q.RelevantSources) == 0 {
			continue
		}
		answerable := true
		for _, s := range q.RelevantSources {
			if _, ok := sources[s]; !ok {
				answerable = false
				break
			}
		}
		if answerable {
			out.Queries = append(out.Queries, q)
		}
	}
	return out
}

// Truncate keeps at most the first n queries. A non-positive n leaves the
// dataset unchanged.
func (ds *Dataset) Truncate(n int) {
	if n > 0 && n < len(ds.Queries) {
		ds.Queries = ds.Queries[:n]
	}
}

// LoadDataset reads a JSON dataset from the given file path.
func LoadDataset(path string) (*Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	return ReadDataset(f)
}

// LoadDatasetFS reads a JSON dataset from a file system (e.g. an embed.FS), so
// fixtures can be embedded into a binary or test.
func LoadDatasetFS(fsys fs.FS, path string) (*Dataset, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	return ReadDataset(f)
}

// ReadDataset decodes a JSON dataset from r.
func ReadDataset(r io.Reader) (*Dataset, error) {
	var ds Dataset
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ds); err != nil {
		return nil, errors.WithStack(err)
	}
	if len(ds.Queries) == 0 {
		return nil, errors.New("eval: dataset has no queries")
	}
	return &ds, nil
}
