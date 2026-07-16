// Package config defines the amoxtli workspace configuration file
// (.amoxtli/config.yaml): parsing, environment variable interpolation and
// validation. The mapping of each field to the library API lives in
// internal/cli/runtime.
package config

import (
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Version   int             `yaml:"version"`
	Store     StoreConfig     `yaml:"store"`
	Index     IndexConfig     `yaml:"index"`
	LLM       LLMConfig       `yaml:"llm"`
	Retrieval RetrievalConfig `yaml:"retrieval"`
	Converter ConverterConfig `yaml:"converter"`
	Indexing  IndexingConfig  `yaml:"indexing"`
}

type StoreConfig struct {
	// Driver selects the document store backend; only "sqlite" is supported
	// for now ("postgres" is planned).
	Driver string `yaml:"driver"`
	// DSN is the store location, relative to the .amoxtli directory.
	DSN string `yaml:"dsn"`
}

type IndexConfig struct {
	Fulltext FulltextIndexConfig `yaml:"fulltext"`
	Vector   VectorIndexConfig   `yaml:"vector"`
}

type FulltextIndexConfig struct {
	Enabled bool    `yaml:"enabled"`
	Path    string  `yaml:"path"`
	Weight  float64 `yaml:"weight"`
}

type VectorIndexConfig struct {
	// Enabled accepts true, false or "auto"; auto enables the vector index
	// when an embeddings client is configured.
	Enabled    Toggle  `yaml:"enabled"`
	Path       string  `yaml:"path"`
	Weight     float64 `yaml:"weight"`
	VectorSize int     `yaml:"vector_size"`
	MaxWords   int     `yaml:"max_words"`
}

type LLMConfig struct {
	Chat       *ClientConfig `yaml:"chat"`
	Embeddings *ClientConfig `yaml:"embeddings"`
}

type ClientConfig struct {
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
}

// SupportedProviders lists the llm.chat / llm.embeddings providers the CLI
// can wire. All share provider.CommonOptions (model, base_url, api_key).
var SupportedProviders = []string{"openai", "openrouter", "mistral"}

func isSupportedProvider(name string) bool {
	return slices.Contains(SupportedProviders, name)
}

type RetrievalConfig struct {
	Reranking         bool `yaml:"reranking"`
	GroundingCheck    bool `yaml:"grounding_check"`
	GroundingFailOpen bool `yaml:"grounding_fail_open"`
	// MaxTotalWords bounds the prompt size (in words) of the LLM retrieval
	// stages (reranker, judge, evidence evaluator). Keep it low enough that the
	// resulting prompt fits your chat endpoint's context window: words are only
	// a coarse proxy for tokens (~1.8 tokens/word on mixed prose and code), so
	// 18000 words already overflow a 32k-token limit. Zero defers to the
	// library default (12000).
	MaxTotalWords int                 `yaml:"max_total_words"`
	Iterative     IterativeConfig     `yaml:"iterative"`
	Decomposition DecompositionConfig `yaml:"decomposition"`
}

type IterativeConfig struct {
	Enabled   bool `yaml:"enabled"`
	MaxRounds int  `yaml:"max_rounds"`
}

type DecompositionConfig struct {
	Enabled       bool `yaml:"enabled"`
	MaxSubQueries int  `yaml:"max_sub_queries"`
}

type ConverterConfig struct {
	Pandoc      PandocConfig         `yaml:"pandoc"`
	LibreOffice LibreOfficeConfig    `yaml:"libreoffice"`
	GenAI       GenAIConverterConfig `yaml:"genai"`
}

type PandocConfig struct {
	// Enabled accepts true, false or "auto"; auto enables the pandoc
	// converter when the binary is found in the PATH.
	Enabled Toggle `yaml:"enabled"`
}

type LibreOfficeConfig struct {
	// Enabled accepts true, false or "auto"; auto enables the LibreOffice
	// converter when both the libreoffice and pandoc binaries are found. It
	// supersedes the standalone pandoc converter, adding .doc support.
	Enabled Toggle `yaml:"enabled"`
}

type GenAIConverterConfig struct {
	// Enabled turns on the GenAI (OCR/LLM) converter. It is opt-in: a DSN and
	// at least one extension are required.
	Enabled bool `yaml:"enabled"`
	// DSN selects the extraction backend, e.g. mistral://?apiKey=${MISTRAL_API_KEY}
	// or marker://host:port.
	DSN string `yaml:"dsn"`
	// Extensions are routed to this converter (e.g. .pdf, .png, .jpg).
	Extensions []string `yaml:"extensions"`
}

type IndexingConfig struct {
	MaxWordsPerSection int                `yaml:"max_words_per_section"`
	TaskParallelism    int                `yaml:"task_parallelism"`
	PersistentTasks    bool               `yaml:"persistent_tasks"`
	Code               CodeIndexingConfig `yaml:"code"`
}

type CodeIndexingConfig struct {
	// Enabled accepts true, false or "auto"; auto (the default) enables
	// source-code indexing (it is pure Go and needs no external tool). Source
	// files are split into declaration-level sections and tagged with
	// `type=code` and `language=<name>` metadata, filterable at search time.
	Enabled Toggle `yaml:"enabled"`
	// Extensions extends or overrides the built-in extension→language mapping,
	// e.g. {".phtml": "php"}. Languages: go, javascript, typescript, tsx,
	// python, php.
	Extensions map[string]string `yaml:"extensions"`
}

// Default returns the configuration used when a field is absent from the
// file; zero numeric values defer to the library defaults.
func Default() *Config {
	return &Config{
		Version: 1,
		Store: StoreConfig{
			Driver: "sqlite",
			DSN:    "data/store.sqlite",
		},
		Index: IndexConfig{
			Fulltext: FulltextIndexConfig{
				Enabled: true,
				Path:    "data/index.bleve",
				Weight:  1.0,
			},
			Vector: VectorIndexConfig{
				Enabled: ToggleAuto,
				Path:    "data/vectors.sqlite",
				Weight:  1.0,
			},
		},
		Retrieval: RetrievalConfig{
			GroundingFailOpen: true,
			Iterative: IterativeConfig{
				MaxRounds: 2,
			},
			Decomposition: DecompositionConfig{
				MaxSubQueries: 3,
			},
		},
		Converter: ConverterConfig{
			Pandoc: PandocConfig{
				Enabled: ToggleAuto,
			},
			LibreOffice: LibreOfficeConfig{
				Enabled: ToggleAuto,
			},
		},
		Indexing: IndexingConfig{
			PersistentTasks: true,
			Code: CodeIndexingConfig{
				Enabled: ToggleAuto,
			},
		},
	}
}

// HasChat reports whether a chat completion client is configured.
func (c *Config) HasChat() bool {
	return c.LLM.Chat != nil && c.LLM.Chat.Model != ""
}

// HasEmbeddings reports whether an embeddings client is configured.
func (c *Config) HasEmbeddings() bool {
	return c.LLM.Embeddings != nil && c.LLM.Embeddings.Model != ""
}

// VectorEnabled resolves the vector index toggle against the presence of an
// embeddings client.
func (c *Config) VectorEnabled() bool {
	return c.Index.Vector.Enabled.Resolve(c.HasEmbeddings())
}

// CodeEnabled resolves the source-code indexing toggle; it defaults to
// enabled (pure Go, no external dependency).
func (c *Config) CodeEnabled() bool {
	return c.Indexing.Code.Enabled.Resolve(true)
}

// Validate checks cross-field constraints and rejects combinations that
// would fail later with an obscure error.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return errors.Errorf("unsupported config version %d (expected 1)", c.Version)
	}

	switch c.Store.Driver {
	case "sqlite":
	case "postgres":
		return errors.New("store driver \"postgres\" is not supported yet")
	default:
		return errors.Errorf("unknown store driver %q", c.Store.Driver)
	}

	if !c.Index.Fulltext.Enabled && !c.VectorEnabled() {
		return errors.New("no index enabled: enable index.fulltext or configure llm.embeddings")
	}

	if c.Index.Vector.Enabled == ToggleTrue && !c.HasEmbeddings() {
		return errors.New("index.vector.enabled is true but llm.embeddings is not configured")
	}

	if !c.HasChat() {
		var needsChat []string
		if c.Retrieval.Reranking {
			needsChat = append(needsChat, "retrieval.reranking")
		}
		if c.Retrieval.GroundingCheck {
			needsChat = append(needsChat, "retrieval.grounding_check")
		}
		if c.Retrieval.Iterative.Enabled {
			needsChat = append(needsChat, "retrieval.iterative")
		}
		if c.Retrieval.Decomposition.Enabled {
			needsChat = append(needsChat, "retrieval.decomposition")
		}
		if len(needsChat) > 0 {
			return errors.Errorf("%s requires llm.chat to be configured", strings.Join(needsChat, ", "))
		}
	}

	for ext, name := range c.Indexing.Code.Extensions {
		if !strings.HasPrefix(ext, ".") {
			return errors.Errorf("indexing.code.extensions: extension %q must start with a dot", ext)
		}
		if _, exists := sourcecode.ByName(name); !exists {
			return errors.Errorf("indexing.code.extensions: unknown language %q for extension %q (supported: %v)", name, ext, sourcecode.Names())
		}
	}

	if c.Converter.GenAI.Enabled {
		if c.Converter.GenAI.DSN == "" {
			return errors.New("converter.genai.enabled is true but converter.genai.dsn is empty")
		}
		if len(c.Converter.GenAI.Extensions) == 0 {
			return errors.New("converter.genai.enabled is true but converter.genai.extensions is empty")
		}
	}

	for _, client := range []struct {
		name string
		cfg  *ClientConfig
	}{
		{"llm.chat", c.LLM.Chat},
		{"llm.embeddings", c.LLM.Embeddings},
	} {
		if client.cfg == nil {
			continue
		}
		if !isSupportedProvider(client.cfg.Provider) {
			return errors.Errorf("%s.provider: unsupported provider %q (supported: %v)", client.name, client.cfg.Provider, SupportedProviders)
		}
		if client.cfg.Model == "" {
			return errors.Errorf("%s.model is required", client.name)
		}
	}

	return nil
}

// Toggle is a tri-state flag accepting true, false or "auto" in YAML.
type Toggle string

const (
	ToggleAuto  Toggle = "auto"
	ToggleTrue  Toggle = "true"
	ToggleFalse Toggle = "false"
)

// Resolve returns the boolean value of the toggle, falling back to auto when
// the toggle is "auto" (or unset).
func (t Toggle) Resolve(auto bool) bool {
	switch t {
	case ToggleTrue:
		return true
	case ToggleFalse:
		return false
	default:
		return auto
	}
}

func (t *Toggle) UnmarshalYAML(node *yaml.Node) error {
	var b bool
	if err := node.Decode(&b); err == nil {
		if b {
			*t = ToggleTrue
		} else {
			*t = ToggleFalse
		}

		return nil
	}

	var s string
	if err := node.Decode(&s); err != nil {
		return errors.WithStack(err)
	}

	switch strings.ToLower(s) {
	case string(ToggleAuto):
		*t = ToggleAuto
	case string(ToggleTrue):
		*t = ToggleTrue
	case string(ToggleFalse):
		*t = ToggleFalse
	default:
		return errors.Errorf("invalid toggle value %q (expected true, false or auto)", s)
	}

	return nil
}
