package eval

import (
	"encoding/json"
	"io"
	"os"

	"github.com/pkg/errors"
)

// Document is one indexable unit of an evaluation corpus: the passage/document
// that queries are expected to retrieve. Source is the identifier that a
// Query's RelevantSources refer to (and that a Retriever returns), so the two
// must agree.
type Document struct {
	Source  string `json:"source"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
	Lang    string `json:"lang,omitempty"`
}

// Corpus is the set of documents an evaluation indexes before scoring queries.
type Corpus struct {
	Name      string     `json:"name,omitempty"`
	Documents []Document `json:"documents"`
}

// LoadCorpus reads a JSON corpus from the given file path.
func LoadCorpus(path string) (*Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	return ReadCorpus(f)
}

// ReadCorpus decodes a JSON corpus from r.
func ReadCorpus(r io.Reader) (*Corpus, error) {
	var c Corpus
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, errors.WithStack(err)
	}
	if len(c.Documents) == 0 {
		return nil, errors.New("eval: corpus has no documents")
	}
	return &c, nil
}

// Sources returns the set of document source identifiers in the corpus.
func (c *Corpus) Sources() map[string]struct{} {
	set := make(map[string]struct{}, len(c.Documents))
	for _, d := range c.Documents {
		set[d.Source] = struct{}{}
	}
	return set
}

// Truncate keeps at most the first n documents (insertion order). A
// non-positive n leaves the corpus unchanged. Pair it with
// Dataset.KeepAnswerable to drop queries whose passage was removed.
func (c *Corpus) Truncate(n int) {
	if n > 0 && n < len(c.Documents) {
		c.Documents = c.Documents[:n]
	}
}

// Subsample builds a smaller, self-consistent corpus + query set for cheap
// evaluation on a machine that cannot embed a full BEIR corpus: it keeps the
// first maxQueries queries, ALL of their relevant documents (so every kept query
// stays answerable), then fills the corpus with further documents as distractors
// up to maxDocs. Unlike Corpus.Truncate (which keeps the first N documents and
// would drop the scattered gold documents of a BEIR dataset), it is gold-aware.
// A non-positive bound disables that limit; gold documents are always included
// even if they exceed maxDocs. Selection order is deterministic.
func Subsample(corpus *Corpus, dataset *Dataset, maxQueries, maxDocs int) (*Corpus, *Dataset) {
	queries := dataset.Queries
	if maxQueries > 0 && maxQueries < len(queries) {
		queries = queries[:maxQueries]
	}

	gold := map[string]struct{}{}
	for _, q := range queries {
		for _, s := range q.RelevantSources {
			gold[s] = struct{}{}
		}
	}

	bySource := make(map[string]Document, len(corpus.Documents))
	order := make([]string, 0, len(corpus.Documents))
	for _, d := range corpus.Documents {
		if _, ok := bySource[d.Source]; !ok {
			bySource[d.Source] = d
			order = append(order, d.Source)
		}
	}

	newCorpus := &Corpus{Name: corpus.Name}
	included := map[string]struct{}{}
	// Gold documents first (always kept).
	for _, src := range order {
		if _, isGold := gold[src]; isGold {
			newCorpus.Documents = append(newCorpus.Documents, bySource[src])
			included[src] = struct{}{}
		}
	}
	// Then distractors up to the budget.
	for _, src := range order {
		if maxDocs > 0 && len(newCorpus.Documents) >= maxDocs {
			break
		}
		if _, in := included[src]; in {
			continue
		}
		newCorpus.Documents = append(newCorpus.Documents, bySource[src])
		included[src] = struct{}{}
	}

	newDataset := &Dataset{Name: dataset.Name}
	for _, q := range queries {
		answerable := true
		for _, s := range q.RelevantSources {
			if _, ok := included[s]; !ok {
				answerable = false
				break
			}
		}
		if answerable {
			newDataset.Queries = append(newDataset.Queries, q)
		}
	}

	return newCorpus, newDataset
}

// Merge appends the documents of others into c, de-duplicating by Source (the
// first occurrence wins). It is used to combine per-language corpora into a
// single index.
func (c *Corpus) Merge(others ...*Corpus) {
	seen := make(map[string]struct{}, len(c.Documents))
	for _, d := range c.Documents {
		seen[d.Source] = struct{}{}
	}
	for _, o := range others {
		if o == nil {
			continue
		}
		for _, d := range o.Documents {
			if _, ok := seen[d.Source]; ok {
				continue
			}
			seen[d.Source] = struct{}{}
			c.Documents = append(c.Documents, d)
		}
	}
}
