package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/pkg/errors"
)

// Lock is an exclusive per-workspace lock file. The bleve index directory can
// only be opened by one process at a time; taking an explicit lock first
// turns the cryptic backend error into an actionable message.
type Lock struct {
	path string
}

// acquireLock takes the workspace lock, stealing it when the owning process
// is gone (stale lock left by a crash).
func acquireLock(path string, command string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, errors.WithStack(err)
	}

	for range 3 {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, writeErr := fmt.Fprintf(file, "%d %s", os.Getpid(), command)
			if closeErr := file.Close(); writeErr == nil {
				writeErr = closeErr
			}
			if writeErr != nil {
				_ = os.Remove(path)
				return nil, errors.WithStack(writeErr)
			}

			return &Lock{path: path}, nil
		}

		if !errors.Is(err, os.ErrExist) {
			return nil, errors.WithStack(err)
		}

		pid, owner := readLock(path)
		if pid != 0 && processAlive(pid) {
			return nil, errors.Errorf("workspace is already in use by another amoxtli process (%s, pid %d); only one process can use it at a time", owner, pid)
		}

		// Stale lock: remove it and retry.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.WithStack(err)
		}
	}

	return nil, errors.Errorf("could not acquire workspace lock %s", path)
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.WithStack(err)
	}

	return nil
}

func readLock(path string) (int, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}

	rawPid, owner, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")

	pid, err := strconv.Atoi(rawPid)
	if err != nil {
		return 0, ""
	}

	if owner == "" {
		owner = "unknown"
	}

	return pid, owner
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Windows FindProcess fails for dead processes.
		return false
	}

	// On Unix FindProcess always succeeds; probe with signal 0.
	return proc.Signal(syscall.Signal(0)) == nil
}
