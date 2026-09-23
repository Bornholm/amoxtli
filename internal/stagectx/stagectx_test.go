package stagectx

import (
	"context"
	"testing"
)

func TestSkipped(t *testing.T) {
	ctx := context.Background()

	if Skipped(ctx, "hyde") {
		t.Error("a context without a stage set must allow every stage")
	}

	restricted := WithAllowed(ctx, "rerank")
	if Skipped(restricted, "rerank") {
		t.Error("an allowed stage must run")
	}
	if !Skipped(restricted, "hyde") {
		t.Error("a stage outside the set must be skipped")
	}

	if !Skipped(WithAllowed(ctx), "rerank") {
		t.Error("an empty set must skip every stage")
	}
}
