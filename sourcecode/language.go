package sourcecode

import (
	"slices"
	"strings"
	"sync"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
	"github.com/pkg/errors"
)

// Language describes how to parse and chunk one programming language.
type Language struct {
	// Name identifies the language in document metadata (e.g. "go", "python").
	Name string

	// Grammar lazily returns the tree-sitter grammar.
	Grammar func() *ts.Language

	// TokenSource lazily returns a tree-sitter token source factory for
	// grammars whose lexer cannot be expressed as a pure DFA (e.g. JSON,
	// which requires escaping-aware tokenization). When nil, the parser
	// falls back to the grammar's built-in DFA lexer.
	TokenSource func(data []byte, lang *ts.Language) ts.TokenSource

	// Query is a tree-sitter S-expression pattern capturing the declarations
	// to expose as sections, each tagged @def.
	Query string

	once     sync.Once
	grammar  *ts.Language
	compiled *ts.Query
	loadErr  error
}

// parseBackend reports which parse path the language needs at runtime.
type parseBackend int

const (
	// parseBackendDFA uses tree-sitter's built-in DFA lexer (parser.Parse).
	parseBackendDFA parseBackend = iota
	// parseBackendTokenSource routes the parse through a registered
	// TokenSource + parser.ParseWithTokenSource.
	parseBackendTokenSource
)

// backend inspects the language's grammar and optional token source to
// pick the right parse path: token-source grammars (JSON, ...) fall back
// to DFA when their external lexer is unavailable so a missing
// grammar_subset_<lang> build tag does not break ingestion.
func (l *Language) backend() parseBackend {
	if l.TokenSource == nil {
		return parseBackendDFA
	}

	if l.grammar == nil {
		return parseBackendDFA
	}

	if len(l.grammar.LexStates) > 0 {
		return parseBackendDFA
	}

	return parseBackendTokenSource
}

// load compiles the grammar and query once and caches them. It recovers
// from the fatal panic that gotreesitter raises when a grammar blob was
// not embedded in the current build (e.g. grammar_subset without
// grammar_subset_typescript), so a missing build tag degrades to a normal
// loadErr rather than crashing the whole ingestion.
func (l *Language) load() (lang *ts.Language, query *ts.Query, err error) {
	loadOnce(l)

	return l.grammar, l.compiled, l.loadErr
}

// loadOnce applies safeLoadGrammar exactly once per language. Subsequent
// calls reuse the cached loadErr so the parser never re-panics in
// gotreesitter when it tries to load a missing grammar blob.
func loadOnce(l *Language) {
	l.once.Do(func() {
		l.grammar, l.compiled, l.loadErr = safeLoadGrammar(l)
	})
}

// safeLoadGrammar loads the language grammar and compiled query, swallowing
// gotreesitter's "not embedded in this grammar_subset build" panic into a
// regular error so the parser can fall back to whole-file indexing.
func safeLoadGrammar(l *Language) (lang *ts.Language, query *ts.Query, err error) {
	defer func() {
		if r := recover(); r != nil {
			lang = nil
			query = nil
			err = errors.Errorf("grammar for language '%s' is not available: %v", l.Name, r)
		}
	}()

	loaded := l.Grammar()
	if loaded == nil {
		return nil, nil, errors.Errorf("grammar for language '%s' is not available", l.Name)
	}

	compiled, err := ts.NewQuery(l.Query, loaded)
	if err != nil {
		return loaded, nil, errors.Wrapf(err, "could not compile query for language '%s'", l.Name)
	}

	return loaded, compiled, nil
}

// Registry maps file extensions (lowercase, with leading dot) to languages.
type Registry struct {
	byExt map[string]*Language
}

func NewRegistry() *Registry {
	return &Registry{byExt: map[string]*Language{}}
}

func (r *Registry) Register(ext string, lang *Language) {
	r.byExt[strings.ToLower(ext)] = lang
}

func (r *Registry) Lookup(ext string) (*Language, bool) {
	lang, exists := r.byExt[strings.ToLower(ext)]
	return lang, exists
}

// SupportedExtensions returns the registered extensions, sorted.
func (r *Registry) SupportedExtensions() []string {
	exts := make([]string, 0, len(r.byExt))
	for ext := range r.byExt {
		exts = append(exts, ext)
	}

	slices.Sort(exts)

	return exts
}

var defaultLanguages = map[string]*Language{
	"go": {
		Name:    "go",
		Grammar: grammars.GoLanguage,
		Query: `[
			(function_declaration)
			(method_declaration)
			(type_declaration)
		] @def`,
	},
	"javascript": {
		Name:    "javascript",
		Grammar: grammars.JavascriptLanguage,
		Query: `[
			(function_declaration)
			(class_declaration)
			(method_definition)
		] @def
		(program (lexical_declaration) @def)
		(program (export_statement (lexical_declaration) @def))`,
	},
	"typescript": {
		Name:    "typescript",
		Grammar: grammars.TypescriptLanguage,
		Query: `[
			(function_declaration)
			(class_declaration)
			(method_definition)
			(interface_declaration)
			(type_alias_declaration)
			(enum_declaration)
		] @def
		(program (lexical_declaration) @def)
		(program (export_statement (lexical_declaration) @def))`,
	},
	"tsx": {
		Name:    "tsx",
		Grammar: grammars.TsxLanguage,
		Query: `[
			(function_declaration)
			(class_declaration)
			(method_definition)
			(interface_declaration)
			(type_alias_declaration)
			(enum_declaration)
		] @def
		(program (lexical_declaration) @def)
		(program (export_statement (lexical_declaration) @def))`,
	},
	"python": {
		Name:    "python",
		Grammar: grammars.PythonLanguage,
		Query: `[
			(function_definition)
			(class_definition)
		] @def`,
	},
	"php": {
		Name:    "php",
		Grammar: grammars.PhpLanguage,
		Query: `[
			(function_definition)
			(method_declaration)
			(class_declaration)
			(interface_declaration)
			(trait_declaration)
			(enum_declaration)
		] @def`,
	},
	"json": {
		Name:    "json",
		Grammar: grammars.JsonLanguage,
		// JSON's grammar ships a custom external lexer (NewJSONTokenSource)
		// because string contents and escape sequences must be split at the
		// token level. The lexer's build tag makes it disappear from
		// `go build -tags 'grammar_subset'` builds: in that case the
		// TokenSource field stays nil and the parser gracefully falls back
		// to the DFA path.
		TokenSource: newJSONTokenSourceIfAvailable,
		Query: `[
			(pair)
			(object)
			(array)
		] @def`,
	},
	"yaml": {
		Name:    "yaml",
		Grammar: grammars.YamlLanguage,
		Query: `[
			(block_mapping_pair)
			(block_sequence_item)
			(document)
		] @def`,
	},
}

// newJSONTokenSourceIfAvailable bridges the optional JSON external lexer,
// which is only compiled in when gotreesitter's grammar_subset_json build
// tag is enabled (or when no grammar_subset tag is used at all). Returning
// nil tells the parser to fall back to its built-in DFA path.
func newJSONTokenSourceIfAvailable(data []byte, lang *ts.Language) ts.TokenSource {
	if jsonTokenSourceFactory == nil {
		return nil
	}

	return jsonTokenSourceFactory(data, lang)
}

// jsonTokenSourceFactory stores the resolved JSON external lexer. It is
// populated by init() through lookupJSONTokenSourceFactory (defined in the
// build-tag-gated json_lexer_lookup*.go files) and stays nil in plain
// grammar_subset builds so the parser can degrade gracefully.
var jsonTokenSourceFactory func(data []byte, lang *ts.Language) ts.TokenSource

// registerJSONTokenSourceFactory stores the optional JSON token source
// factory; it is invoked from init() only when the gotreesitter symbol is
// available at compile time.
func registerJSONTokenSourceFactory(factory func(data []byte, lang *ts.Language) ts.TokenSource) {
	jsonTokenSourceFactory = factory
}

// init wires the optional JSON token source factory only when gotreesitter
// ships its JSON external lexer. The lookup is delegated to a build-tag
// helper (json_lexer_lookup.go / json_lexer_lookup_stub.go), so plain
// grammar_subset builds stay free of any compile-time dependency on the
// gated grammars.NewJSONTokenSourceOrEOF symbol.
func init() {
	if factory, ok := lookupJSONTokenSourceFactory(); ok {
		registerJSONTokenSourceFactory(factory)
	}
}

var defaultExtensions = map[string]string{
	".go":   "go",
	".js":   "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".jsx":  "javascript",
	".ts":   "typescript",
	".mts":  "typescript",
	".cts":  "typescript",
	".tsx":  "tsx",
	".py":   "python",
	".pyi":  "python",
	".php":  "php",
	".json": "json",
	".yaml": "yaml",
	".yml":  "yaml",
}

// ByName returns the built-in language definition with the given name.
func ByName(name string) (*Language, bool) {
	lang, exists := defaultLanguages[strings.ToLower(name)]
	return lang, exists
}

// Names returns the built-in language names, sorted.
func Names() []string {
	names := make([]string, 0, len(defaultLanguages))
	for name := range defaultLanguages {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// DefaultRegistry returns a registry with the built-in extension mapping.
func DefaultRegistry() *Registry {
	registry := NewRegistry()
	for ext, name := range defaultExtensions {
		lang, _ := ByName(name)
		registry.Register(ext, lang)
	}

	return registry
}
