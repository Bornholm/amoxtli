//go:build grammar_subset && !grammar_subset_json

package sourcecode

import ts "github.com/odvcencio/gotreesitter"

// This file is the build-tag-gated companion to json_lexer_lookup.go.
// With only the grammar_subset tag enabled (no grammar_subset_json), the
// underlying JSON external lexer symbol is not exported by gotreesitter,
// so the lookup must return ok=false and the JSON parser falls back to
// the DFA path exposed by parser.Parse.

func lookupJSONTokenSourceFactory() (func(data []byte, lang *ts.Language) ts.TokenSource, bool) {
	return nil, false
}
