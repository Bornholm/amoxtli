package config

import (
	"os"
	"strings"

	"github.com/pkg/errors"
)

// ExpandEnv expands $VAR and ${VAR} references in s using lookup, supporting
// the ${VAR:-default} fallback syntax and $$ as a literal dollar escape. A
// reference to an undefined variable without a default is an error (fail
// fast rather than silently injecting an empty secret).
//
// Full-line comments (lines whose first non-blank character is '#') are left
// untouched, so configuration examples in comments are not expanded.
func ExpandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var missing []string

	expand := func(name string) string {
		if name == "$" {
			return "$"
		}

		key, def, hasDefault := strings.Cut(name, ":-")

		if value, ok := lookup(key); ok {
			return value
		}

		if hasDefault {
			return def
		}

		missing = append(missing, key)

		return ""
	}

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		lines[i] = os.Expand(line, expand)
	}

	if len(missing) > 0 {
		return "", errors.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}

	return strings.Join(lines, "\n"), nil
}
