package cli

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
)

// sourceMapper turns filesystem paths into the source URLs stored alongside
// indexed documents, and back. Without a base directory a source carries the
// absolute path, which discloses the host's filesystem layout to whoever reads
// a document header — typically an MCP client, which never needs it. With one,
// the base prefix is stripped and only the relative path is stored:
// "/srv/kb/docs/cli.md" under base "/srv/kb" becomes "file:///docs/cli.md".
//
// The relative form keeps its leading slash so the result stays a well-formed
// file URL: a source URL whose path is relative would be re-parsed with its
// first segment as the host ("file://docs/cli.md" → host "docs"). The path is
// therefore relative to the base directory, not to the filesystem root, and is
// resolved back through Path when the file must be reached on disk again.
type sourceMapper struct {
	// base is the absolute, cleaned base directory. Empty disables rewriting:
	// sources keep their absolute path.
	base string
}

// newSourceMapper builds a mapper stripping baseDir from every source it
// produces. An empty baseDir yields a pass-through mapper emitting absolute
// sources, the historical behaviour.
func newSourceMapper(baseDir string) (*sourceMapper, error) {
	if baseDir == "" {
		return &sourceMapper{}, nil
	}

	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, errors.Wrapf(err, "could not resolve base directory %q", baseDir)
	}
	if !info.IsDir() {
		return nil, errors.Errorf("base directory %q is not a directory", baseDir)
	}

	return &sourceMapper{base: abs}, nil
}

// Source returns the source URL of an absolute filesystem path. It fails when
// the path lies outside the base directory: indexing it would either leak an
// absolute path or store a source that cannot be resolved back.
func (m *sourceMapper) Source(abs string) (*url.URL, error) {
	path, err := m.sourcePath(abs)
	if err != nil {
		return nil, err
	}

	return &url.URL{Scheme: "file", Path: path}, nil
}

// Path resolves a source URL back to a filesystem path, re-anchoring a
// relative source on the base directory.
func (m *sourceMapper) Path(u *url.URL) string {
	path := filepath.FromSlash(u.Path)
	if m.base == "" {
		return path
	}

	return filepath.Join(m.base, path)
}

// DirPrefix returns the source URL prefix covering every document indexed from
// dir, trailing slash included so sibling directories (e.g. "/x/proj2" for
// "/x/proj") stay out of the match.
func (m *sourceMapper) DirPrefix(dir string) (string, error) {
	path, err := m.sourcePath(dir)
	if err != nil {
		return "", err
	}

	prefix := (&url.URL{Scheme: "file", Path: path}).String()

	// The base directory itself maps to "/", whose URL already ends with a
	// slash; trimming first keeps the prefix free of an empty path segment.
	return strings.TrimSuffix(prefix, "/") + "/", nil
}

// sourcePath maps an absolute filesystem path to the path component of its
// source URL: the path itself when no base directory is set, otherwise its
// slash-separated location below the base directory.
func (m *sourceMapper) sourcePath(abs string) (string, error) {
	if m.base == "" {
		return abs, nil
	}

	rel, err := filepath.Rel(m.base, abs)
	if err != nil {
		return "", errors.Wrapf(err, "could not relativize %q against base directory %q", abs, m.base)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.Errorf("%q is outside the base directory %q", abs, m.base)
	}
	if rel == "." {
		return "/", nil
	}

	return "/" + filepath.ToSlash(rel), nil
}
