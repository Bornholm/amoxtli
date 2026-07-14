// Package hfqa loads Hugging Face extractive-QA datasets in the SQuAD JSON
// format into an amoxtli evaluation corpus + query set. The format is shared by
// several real-document, Wikipedia-based datasets across languages — SQuAD
// (English), squad_es (Spanish), PIAF/FQuAD (French), MLQA, XQuAD — so a single
// loader turns any of them into a passage-retrieval benchmark: each unique
// paragraph becomes a corpus document, and each question becomes a query whose
// relevant source is the paragraph it was written from.
package hfqa

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/bornholm/amoxtli/eval"
	"github.com/pkg/errors"
)

// squadFile is the SQuAD v1/v2 JSON schema (only the fields we need).
type squadFile struct {
	Version string `json:"version"`
	Data    []struct {
		Title      string `json:"title"`
		Paragraphs []struct {
			Context string `json:"context"`
			QAS     []struct {
				Question     string `json:"question"`
				ID           string `json:"id"`
				IsImpossible bool   `json:"is_impossible"`
				Answers      []struct {
					Text string `json:"text"`
				} `json:"answers"`
			} `json:"qas"`
		} `json:"paragraphs"`
	} `json:"data"`
}

// Load reads a SQuAD-format dataset from path, tagging every document and query
// with lang. It returns the corpus (unique paragraphs) and the query set
// (answerable questions). name labels the resulting dataset/corpus.
func Load(path, lang, name string) (*eval.Corpus, *eval.Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	defer f.Close()

	return Read(f, lang, name)
}

// Read decodes a SQuAD-format dataset from r. See Load.
func Read(r io.Reader, lang, name string) (*eval.Corpus, *eval.Dataset, error) {
	var sf squadFile
	if err := json.NewDecoder(r).Decode(&sf); err != nil {
		return nil, nil, errors.Wrap(err, "hfqa: decoding SQuAD JSON")
	}

	corpus := &eval.Corpus{Name: name}
	dataset := &eval.Dataset{Name: name}
	seen := make(map[string]struct{})

	for _, article := range sf.Data {
		for _, p := range article.Paragraphs {
			if p.Context == "" {
				continue
			}
			source := sourceID(lang, p.Context)

			if _, ok := seen[source]; !ok {
				seen[source] = struct{}{}
				corpus.Documents = append(corpus.Documents, eval.Document{
					Source:  source,
					Title:   article.Title,
					Content: p.Context,
					Lang:    lang,
				})
			}

			for _, qa := range p.QAS {
				// Skip unanswerable questions (SQuAD v2): they have no gold
				// passage to retrieve, so they cannot score recall.
				if qa.IsImpossible || qa.Question == "" {
					continue
				}
				id := qa.ID
				if id == "" {
					id = fmt.Sprintf("%s-%d", source, len(dataset.Queries))
				}
				dataset.Queries = append(dataset.Queries, eval.Query{
					ID:              id,
					Query:           qa.Question,
					Lang:            lang,
					RelevantSources: []string{source},
					Tags:            []string{lang},
				})
			}
		}
	}

	if len(corpus.Documents) == 0 {
		return nil, nil, errors.New("hfqa: no paragraphs found (is this a SQuAD-format file?)")
	}
	if len(dataset.Queries) == 0 {
		return nil, nil, errors.New("hfqa: no answerable questions found")
	}

	return corpus, dataset, nil
}

// sourceID derives a stable, collision-resistant identifier for a paragraph
// from its language and content, so the same passage always maps to the same
// source across runs.
func sourceID(lang, context string) string {
	sum := sha1.Sum([]byte(context))
	return fmt.Sprintf("hfqa://%s/%s", lang, hex.EncodeToString(sum[:])[:16])
}
