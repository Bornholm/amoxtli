//go:build !grammar_subset || grammar_subset_json

package sourcecode

import (
	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// This file is the build-tag-gated bridge between sourcecode and
// grammars.NewJSONTokenSourceOrEOF. It is selected by adding
// `grammar_subset_json` to the build tags (or running without any
// grammar_subset tag at all). All other builds use the stub in
// json_lexer_lookup_stub.go, which keeps the sourcecode package free of
// any compile-time dependency on the gated JSON lexer symbol.

func lookupJSONTokenSourceFactory() (func(data []byte, lang *ts.Language) ts.TokenSource, bool) {
	return grammars.NewJSONTokenSourceOrEOF, true
}
