package indecisrel

import (
	"testing"

	"github.com/bornholm/indecis"
)

func TestCheckSchema(t *testing.T) {
	if err := checkSchema(Schema()); err != nil {
		t.Fatalf("the model's own schema is refused: %v", err)
	}
	for name, schema := range map[string]indecis.Schema{
		"no sufficiency": {indecis.NewNoul(QuestionRelevant, "")},
		"renamed level": {
			indecis.NewNoul(QuestionRelevant, ""),
			indecis.NewScore(QuestionSufficient, "", "aucune", "partielle", "totale"),
		},
		"relevance as a score": {
			indecis.NewScore(QuestionRelevant, "", "non", "oui"),
			indecis.NewScore(QuestionSufficient, "", "aucune", "partielle", "complete"),
		},
	} {
		if err := checkSchema(schema); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
