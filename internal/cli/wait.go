package cli

import (
	"context"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

// waitTask polls an indexing task until it reaches a terminal status, the
// timeout elapses (0 = no timeout) or the context is cancelled. onProgress,
// when non-nil, is invoked after each poll.
func waitTask(ctx context.Context, codex *amoxtli.Codex, id task.ID, timeout time.Duration, onProgress func(*task.State)) (*task.State, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		state, err := codex.TaskState(ctx, id)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if onProgress != nil {
			onProgress(state)
		}

		switch state.Status {
		case task.StatusSucceeded, task.StatusFailed:
			return state, nil
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			return state, errors.Errorf("task %s did not finish within %s (status: %s)", id, timeout, state.Status)
		}

		select {
		case <-ctx.Done():
			return state, errors.WithStack(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
