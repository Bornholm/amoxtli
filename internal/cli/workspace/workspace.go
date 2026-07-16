// Package workspace locates and describes an amoxtli workspace: a project
// directory holding a .amoxtli/ configuration directory, discovered by
// walking up the filesystem from a starting point (like git does).
package workspace

import (
	"path/filepath"
)

// DirName is the name of the workspace configuration directory.
const DirName = ".amoxtli"

// EnvDir is the environment variable pointing directly to a workspace
// configuration directory, bypassing discovery. Useful for processes whose
// working directory is unpredictable (e.g. MCP servers spawned by a client).
const EnvDir = "AMOXTLI_DIR"

// Workspace describes a located workspace. Dir is the .amoxtli directory
// itself; Root is its parent (the project root).
type Workspace struct {
	Root string
	Dir  string
}

// New builds a Workspace from the path of its configuration directory.
func New(dir string) *Workspace {
	return &Workspace{
		Root: filepath.Dir(dir),
		Dir:  dir,
	}
}

// ConfigPath returns the path of the workspace configuration file.
func (w *Workspace) ConfigPath() string {
	return filepath.Join(w.Dir, "config.yaml")
}

// DataDir returns the directory holding the indexed data.
func (w *Workspace) DataDir() string {
	return filepath.Join(w.Dir, "data")
}

// StagingDir returns the stable staging directory used by persistent tasks.
func (w *Workspace) StagingDir() string {
	return filepath.Join(w.DataDir(), "staging")
}

// LockPath returns the path of the workspace lock file.
func (w *Workspace) LockPath() string {
	return filepath.Join(w.DataDir(), "lock")
}

// Resolve resolves a possibly-relative path from the configuration file
// against the .amoxtli directory.
func (w *Workspace) Resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(w.Dir, path)
}
