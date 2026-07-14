// Package beir loads datasets in the BEIR format (the de-facto information
// retrieval benchmark) into an amoxtli evaluation corpus + query set. Unlike the
// SQuAD-style extractive-QA datasets (where questions are written from the
// passage and share its vocabulary, favouring lexical search), several BEIR
// datasets — FiQA, Quora, ArguAna, SciFact — exhibit a real query↔document
// vocabulary gap, so they are where semantic (dense) retrieval is expected to
// help over BM25.
//
// BEIR layout (three files):
//
//	corpus.jsonl   one JSON per line: {"_id","title","text"}
//	queries.jsonl  one JSON per line: {"_id","text"}
//	qrels/*.tsv    header "query-id\tcorpus-id\tscore" then rows
//
// A query's relevant documents are the corpus ids with a positive qrel score.
package beir

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/eval"
	"github.com/pkg/errors"
)

// Source builds the stable source identifier for a BEIR document id. It is
// exported so callers can map ids the same way the loader does.
func Source(dataset, id string) string {
	return "beir://" + dataset + "/" + url.PathEscape(id)
}

type corpusLine struct {
	ID    string `json:"_id"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type queryLine struct {
	ID   string `json:"_id"`
	Text string `json:"text"`
}

// Load reads a BEIR dataset from the three given files, tagging documents and
// queries with lang (may be empty). name labels the corpus/dataset and namespaces
// the source identifiers. Only queries that have at least one relevant document
// present in the corpus are kept.
func Load(corpusPath, queriesPath, qrelsPath, name, lang string) (*eval.Corpus, *eval.Dataset, error) {
	corpus, docText, err := loadCorpus(corpusPath, name, lang)
	if err != nil {
		return nil, nil, errors.Wrap(err, "beir: loading corpus")
	}

	queries, err := loadQueries(queriesPath)
	if err != nil {
		return nil, nil, errors.Wrap(err, "beir: loading queries")
	}

	qrels, err := loadQrels(qrelsPath, name)
	if err != nil {
		return nil, nil, errors.Wrap(err, "beir: loading qrels")
	}

	dataset := &eval.Dataset{Name: name}
	for qid, text := range queries {
		relevant := qrels[qid]
		if len(relevant) == 0 {
			continue
		}
		// Keep only relevant sources that actually exist in the corpus.
		present := relevant[:0]
		for _, src := range relevant {
			if _, ok := docText[src]; ok {
				present = append(present, src)
			}
		}
		if len(present) == 0 {
			continue
		}
		dataset.Queries = append(dataset.Queries, eval.Query{
			ID:              qid,
			Query:           text,
			Lang:            lang,
			RelevantSources: present,
		})
	}

	if len(corpus.Documents) == 0 {
		return nil, nil, errors.New("beir: empty corpus")
	}
	if len(dataset.Queries) == 0 {
		return nil, nil, errors.New("beir: no queries with relevant documents in the corpus")
	}

	return corpus, dataset, nil
}

func loadCorpus(path, name, lang string) (*eval.Corpus, map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	defer f.Close()

	corpus := &eval.Corpus{Name: name}
	present := map[string]struct{}{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cl corpusLine
		if err := json.Unmarshal([]byte(line), &cl); err != nil {
			return nil, nil, errors.Wrapf(err, "beir: corpus line %q", truncate(line))
		}
		if cl.ID == "" || cl.Text == "" {
			continue
		}
		src := Source(name, cl.ID)
		corpus.Documents = append(corpus.Documents, eval.Document{
			Source:  src,
			Title:   cl.Title,
			Content: cl.Text,
			Lang:    lang,
		})
		present[src] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, errors.WithStack(err)
	}
	return corpus, present, nil
}

func loadQueries(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ql queryLine
		if err := json.Unmarshal([]byte(line), &ql); err != nil {
			return nil, errors.Wrapf(err, "beir: query line %q", truncate(line))
		}
		if ql.ID != "" && ql.Text != "" {
			out[ql.ID] = ql.Text
		}
	}
	if err := sc.Err(); err != nil {
		return nil, errors.WithStack(err)
	}
	return out, nil
}

// loadQrels returns, per query id, the source identifiers of its relevant
// documents (positive score).
func loadQrels(path, name string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			fields = strings.Fields(line)
		}
		if len(fields) < 3 {
			continue
		}
		// Skip the header row ("query-id corpus-id score").
		if first {
			first = false
			if _, err := strconv.Atoi(fields[2]); err != nil {
				continue
			}
		}
		score, err := strconv.Atoi(fields[2])
		if err != nil || score <= 0 {
			continue
		}
		qid, cid := fields[0], fields[1]
		out[qid] = append(out[qid], Source(name, cid))
	}
	if err := sc.Err(); err != nil {
		return nil, errors.WithStack(err)
	}
	return out, nil
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
