package workspace

import (
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

// ErrNotFound is returned by Discover when no .amoxtli directory exists
// between the starting directory and the filesystem root.
var ErrNotFound = errors.Errorf("no %s directory found; run \"amoxtli init\" first", DirName)

// Discover walks up from startDir and returns the first directory containing
// a .amoxtli subdirectory. If the AMOXTLI_DIR environment variable is set, it
// is used directly as the configuration directory instead.
func Discover(startDir string) (*Workspace, error) {
	if env := os.Getenv(EnvDir); env != "" {
		abs, err := filepath.Abs(env)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return nil, errors.Errorf("%s does not point to a directory: %q", EnvDir, env)
		}

		return New(abs), nil
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for {
		candidate := filepath.Join(dir, DirName)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return New(candidate), nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errors.WithStack(ErrNotFound)
		}

		dir = parent
	}
}
