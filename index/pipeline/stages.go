package pipeline

// Names of the optional pipeline stages a request may skip (see
// internal/stagectx). They match the amoxtli.Stage values.
const (
	StageHyDE      = "hyde"
	StageTranslate = "translate"
	StageJudge     = "judge"
)
