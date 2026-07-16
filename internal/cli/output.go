package cli

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/pkg/errors"
)

// printJSON writes v as indented JSON, for the --json output mode.
func printJSON(w io.Writer, v any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return errors.WithStack(encoder.Encode(v))
}

// excerpt collapses whitespace and truncates s to at most max runes, for
// compact human-readable result listings.
func excerpt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")

	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	return string(runes[:max]) + "…"
}
