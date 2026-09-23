// Package ignore implements .amoxtlignore support: gitignore-style exclusion
// rules that decide whether a file should be skipped by "amoxtli add" and
// "amoxtli sync", or by any application walking a tree for amoxtli.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/pkg/errors"
)

// FileName is the per-directory ignore file amoxtli looks for, mirroring how
// git reads .gitignore.
const FileName = ".amoxtlignore"

// Matcher evaluates .amoxtlignore rules cascaded from the workspace root down
// to each candidate file's directory. Compiled matchers are cached per
// directory so a batch of files under the same tree only parses each ignore
// file once. A Matcher is not safe for concurrent use.
type Matcher struct {
	root      string
	fileNames []string
	cache     map[string][]fileMatcher // dir -> matchers of its ignore files
}

// fileMatcher is the compiled rules of one ignore file.
type fileMatcher struct {
	path    string
	matcher *patternmatcher.PatternMatcher
}

// New returns a Matcher anchored at root (the workspace root, i.e. the
// directory whose subtree the .amoxtlignore files govern).
func New(root string) *Matcher {
	return NewWithFileNames(root, FileName)
}

// NewWithFileNames returns a Matcher reading, in each directory, the ignore
// files of the given names — an application may accept its own name besides
// .amoxtlignore. The rules of all of them apply.
func NewWithFileNames(root string, fileNames ...string) *Matcher {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}

	if len(fileNames) == 0 {
		fileNames = []string{FileName}
	}

	return &Matcher{
		root:      filepath.Clean(abs),
		fileNames: fileNames,
		cache:     make(map[string][]fileMatcher),
	}
}

// Ignored reports whether absPath is excluded by any .amoxtlignore located
// between the root and the file's own directory. When several ignore files
// apply, a match in any of them wins (logical OR); source is the path of the
// .amoxtlignore that matched. Negations (!pattern) are honoured within a single
// ignore file but cannot re-include, from a child directory, a file already
// excluded by a parent.
func (m *Matcher) Ignored(absPath string) (ignored bool, source string, err error) {
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return false, "", errors.WithStack(err)
	}
	abs = filepath.Clean(abs)

	for _, dir := range m.cascade(filepath.Dir(abs)) {
		matchers, err := m.matchersFor(dir)
		if err != nil {
			return false, "", err
		}

		rel, err := filepath.Rel(dir, abs)
		if err != nil {
			continue
		}

		for _, fm := range matchers {
			matched, err := fm.matcher.MatchesOrParentMatches(rel)
			if err != nil {
				return false, "", errors.Wrapf(err, "matching %q against %s", rel, fm.path)
			}
			if matched {
				ignored = true
				source = fm.path
			}
		}
	}

	return ignored, source, nil
}

// cascade returns the directories whose .amoxtlignore may apply to a file in
// dir, ordered from the shallowest (workspace root) to dir itself. Only
// directories within the root are considered; if dir is outside the workspace,
// only dir itself is returned.
func (m *Matcher) cascade(dir string) []string {
	dir = filepath.Clean(dir)

	rel, err := filepath.Rel(m.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// dir is not under the workspace root: fall back to its own directory.
		return []string{dir}
	}

	dirs := []string{dir}
	for dir != m.root {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		dirs = append(dirs, dir)
	}

	// Reverse so the workspace root comes first and dir last.
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}

	return dirs
}

// matchersFor lazily loads and compiles the ignore files of dir, caching the
// result (including their absence) so each is read once.
func (m *Matcher) matchersFor(dir string) ([]fileMatcher, error) {
	if matchers, ok := m.cache[dir]; ok {
		return matchers, nil
	}

	var matchers []fileMatcher
	for _, name := range m.fileNames {
		path := filepath.Join(dir, name)

		patterns, err := readPatterns(path)
		if err != nil {
			return nil, err
		}
		if len(patterns) == 0 {
			continue
		}

		pm, err := patternmatcher.New(patterns)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid patterns in %s", path)
		}
		matchers = append(matchers, fileMatcher{path: path, matcher: pm})
	}

	m.cache[dir] = matchers

	return matchers, nil
}

// readPatterns reads an ignore file, dropping blank lines and # comments like
// git does. A missing file yields no patterns and no error.
func readPatterns(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.WithStack(err)
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, normalizePattern(line))
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	return patterns, nil
}

// normalizePattern bridges gitignore semantics onto patternmatcher's
// dockerignore-style matching. In gitignore a pattern with no embedded slash
// (e.g. "*.log", "build/") matches at any depth, whereas patternmatcher anchors
// it to the ignore file's directory. Prefixing "**/" restores the recursive
// behaviour. Patterns that are anchored — a leading "/" or an embedded "/" —
// keep their gitignore meaning (relative to the ignore file's directory), so a
// leading "/" is simply stripped since patternmatcher is already anchored.
func normalizePattern(pattern string) string {
	negated := strings.HasPrefix(pattern, "!")
	if negated {
		pattern = pattern[1:]
	}

	anchored := strings.HasPrefix(pattern, "/")
	if anchored {
		pattern = strings.TrimPrefix(pattern, "/")
	}
	// A single trailing slash marks a directory but does not anchor the pattern.
	if strings.Contains(strings.TrimSuffix(pattern, "/"), "/") {
		anchored = true
	}

	if !anchored {
		pattern = "**/" + pattern
	}
	if negated {
		pattern = "!" + pattern
	}

	return pattern
}
