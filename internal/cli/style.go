package cli

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// palette holds the ANSI escapes used to render human output. When colours
// are disabled every field is the empty string, so the same format strings
// produce plain text.
type palette struct {
	bold  string
	dim   string
	cyan  string
	green string
	reset string
}

// newPalette returns a colourful palette when w is a terminal and colours are
// not disabled (NO_COLOR), or a no-op palette otherwise.
func newPalette(w io.Writer) palette {
	if !colorsEnabled(w) {
		return palette{}
	}

	return palette{
		bold:  "\x1b[1m",
		dim:   "\x1b[2m",
		cyan:  "\x1b[36m",
		green: "\x1b[32m",
		reset: "\x1b[0m",
	}
}

func colorsEnabled(w io.Writer) bool {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}

	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	return term.IsTerminal(int(file.Fd()))
}

// formatSource turns a result source into a short title and, for local files,
// a readable path. file:// sources yield the basename as title and a
// home-abbreviated path; other sources are shown verbatim with no path line.
func formatSource(raw string) (title, path string) {
	if raw == "" {
		return "(unknown source)", ""
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return raw, ""
	}

	return filepath.Base(u.Path), abbreviateHome(u.Path)
}

// abbreviateHome replaces the user's home directory prefix with ~.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}

	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}

	return path
}
