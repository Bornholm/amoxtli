//go:build unix

package proc

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestRunTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	start := time.Now()
	// The child sleeper must die with its parent's group.
	err := Run(context.Background(), 200*time.Millisecond, t.TempDir(), "sh", "-c", "sleep 30 & sleep 30")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the process group must be killed at once, took %s", elapsed)
	}
}

func TestRunSucceeds(t *testing.T) {
	if err := Run(context.Background(), time.Second, t.TempDir(), "true"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
