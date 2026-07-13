// Package ollamatest provides shared helpers for the integration tests that run
// against an Ollama testcontainer.
package ollamatest

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/exec"
)

// EnsureModels makes sure each model is available in the container, pulling it
// only when it is not already cached. The integration tests mount a persistent
// "ollama-data" volume at /root/.ollama, so once a model has been pulled it
// stays cached across runs; skipping the redundant "ollama pull" avoids a slow
// (and sometimes flaky) network round trip to the registry on every run.
func EnsureModels(t *testing.T, ctx context.Context, container testcontainers.Container, models ...string) {
	t.Helper()

	present := cachedModels(ctx, container)

	for _, m := range models {
		if _, ok := present[m]; ok {
			t.Logf("model %q already cached, skipping pull", m)
			continue
		}

		t.Logf("pulling model %q", m)
		code, reader, err := container.Exec(ctx, []string{"ollama", "pull", m}, exec.Multiplexed())
		if err != nil {
			t.Fatalf("failed to pull model %s: %+v", m, errors.WithStack(err))
		}
		if code != 0 {
			out, _ := io.ReadAll(reader)
			t.Fatalf("ollama pull %s exited with code %d: %s", m, code, string(out))
		}
	}
}

// cachedModels returns the set of model names already present in the container,
// read from the offline "ollama list". On any error it returns an empty set so
// the caller falls back to pulling.
func cachedModels(ctx context.Context, container testcontainers.Container) map[string]struct{} {
	present := map[string]struct{}{}

	code, reader, err := container.Exec(ctx, []string{"ollama", "list"}, exec.Multiplexed())
	if err != nil || code != 0 {
		return present
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return present
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "NAME" {
			continue
		}
		present[fields[0]] = struct{}{}
	}

	return present
}
