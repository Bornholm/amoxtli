package amoxtli

import (
	"context"
	"testing"

	"github.com/pkg/errors"
)

func TestCodexAnswerRequiresAModel(t *testing.T) {
	codex, _ := newBleveCodex(t)

	if codex.CanAnswer() {
		t.Error("a codex without llm client must not answer")
	}
	if _, _, err := codex.Answer(context.Background(), "q", nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %+v", err)
	}
}
