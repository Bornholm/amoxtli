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

	// Query is a tree-sitter S-expression pattern capturing the declarations
	// to expose as sections, each tagged @def.
	Query string

	once     sync.Once
	grammar  *ts.Language
	compiled *ts.Query
	loadErr  error
}

// load compiles the grammar and query once and caches them.
func (l *Language) load() (*ts.Language, *ts.Query, error) {
	l.once.Do(func() {
		l.grammar = l.Grammar()
		if l.grammar == nil {
			l.loadErr = errors.Errorf("grammar for language '%s' is not available", l.Name)
			return
		}

		compiled, err := ts.NewQuery(l.Query, l.grammar)
		if err != nil {
			l.loadErr = errors.Wrapf(err, "could not compile query for language '%s'", l.Name)
			return
		}

		l.compiled = compiled
	})

	return l.grammar, l.compiled, l.loadErr
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
}

var defaultExtensions = map[string]string{
	".go":  "go",
	".js":  "javascript",
	".mjs": "javascript",
	".cjs": "javascript",
	".jsx": "javascript",
	".ts":  "typescript",
	".mts": "typescript",
	".cts": "typescript",
	".tsx": "tsx",
	".py":  "python",
	".pyi": "python",
	".php": "php",
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
