package retrieval

// Names of the optional retrieval stages a request may skip (see
// internal/stagectx). They match the amoxtli.Stage values.
const (
	StageGrounding   = "grounding"
	StageReformulate = "reformulate"
	StageDecompose   = "decompose"
	StageRerank      = "rerank"
)
