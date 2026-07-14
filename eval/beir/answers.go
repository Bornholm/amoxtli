package beir

import (
	"encoding/json"
	"os"

	"github.com/pkg/errors"
)

// LoadHotpotAnswers reads the gold answers from a native HotpotQA distribution
// file (e.g. hotpot_dev_fullwiki_v1.json) — a JSON array of objects with at least
// "_id" and "answer" — and returns them keyed by question id. BEIR's hotpotqa
// query ids are the original HotpotQA question ids, so the result can be joined
// onto a BEIR-loaded dataset to enable the generation (reader) EM/F1 evaluation,
// which the three BEIR files (corpus/queries/qrels) cannot provide on their own.
func LoadHotpotAnswers(path string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()

	var items []struct {
		ID     string `json:"_id"`
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(f).Decode(&items); err != nil {
		return nil, errors.Wrap(err, "beir: decoding HotpotQA answers")
	}

	out := make(map[string][]string, len(items))
	for _, it := range items {
		if it.ID == "" || it.Answer == "" {
			continue
		}
		out[it.ID] = []string{it.Answer}
	}
	if len(out) == 0 {
		return nil, errors.New("beir: no answers found in HotpotQA file")
	}
	return out, nil
}
