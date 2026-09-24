// Package proc runs the external programs of the converters under a
// deadline: a crafted document must not hold a worker forever.
package proc

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/pkg/errors"
)

// DefaultTimeout bounds a conversion when the caller sets no deadline of
// its own.
const DefaultTimeout = 5 * time.Minute

// waitDelay is how long the pipes of a killed process may linger.
const waitDelay = 5 * time.Second

// Run runs the program in dir until it exits, ctx ends or timeout elapses;
// timeout <= 0 selects DefaultTimeout. The whole process group is killed on
// expiry: converters such as LibreOffice spawn children.
func Run(ctx context.Context, timeout time.Duration, dir, name string, args ...string) error {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = waitDelay
	isolate(cmd)

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.Wrapf(ctx.Err(), "%s did not complete", name)
		}
		return errors.WithStack(err)
	}
	return nil
}
