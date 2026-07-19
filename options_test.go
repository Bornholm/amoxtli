package amoxtli

import (
	"testing"

	"github.com/bornholm/genai/llm"
)

// stageStubClient is a distinguishable llm.Client stub (methods are inherited
// from the embedded nil interface and must not be called).
type stageStubClient struct {
	llm.Client
	name string
}

// TestStageClientResolution checks the per-stage client resolution: a stage
// with a dedicated client uses it, every other stage falls back to the default
// WithLLMClient one, and a nil client removes the override.
func TestStageClientResolution(t *testing.T) {
	def := &stageStubClient{name: "default"}
	hyde := &stageStubClient{name: "hyde"}

	opts := defaultOptions()
	for _, fn := range []Option{
		WithLLMClient(def),
		WithStageLLMClient(StageHyDE, hyde),
	} {
		fn(opts)
	}

	if got := opts.clientFor(StageHyDE); got != llm.Client(hyde) {
		t.Errorf("clientFor(StageHyDE) = %v, want the dedicated hyde client", got)
	}
	for _, stage := range []Stage{StageJudge, StageGrounding, StageRerank, StageDecompose, StageReformulate} {
		if got := opts.clientFor(stage); got != llm.Client(def) {
			t.Errorf("clientFor(%s) = %v, want the default client", stage, got)
		}
	}

	WithStageLLMClient(StageHyDE, nil)(opts)
	if got := opts.clientFor(StageHyDE); got != llm.Client(def) {
		t.Errorf("clientFor(StageHyDE) after nil override = %v, want the default client", got)
	}
}

// TestStageClientWithoutDefault checks that a stage client alone (no default
// client) is resolved for its stage while the others stay nil.
func TestStageClientWithoutDefault(t *testing.T) {
	judge := &stageStubClient{name: "judge"}

	opts := defaultOptions()
	WithStageLLMClient(StageJudge, judge)(opts)

	if got := opts.clientFor(StageJudge); got != llm.Client(judge) {
		t.Errorf("clientFor(StageJudge) = %v, want the dedicated judge client", got)
	}
	if got := opts.clientFor(StageHyDE); got != nil {
		t.Errorf("clientFor(StageHyDE) = %v, want nil (no default client)", got)
	}
}
