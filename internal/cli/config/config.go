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
	// Driver selects the document store backend: "sqlite" (file-based) or
	// "postgres" (client-server, for a shared/concurrent deployment).
	Driver string `yaml:"driver"`
	// DSN is the store location. For sqlite it is a path relative to the
	// .amoxtli directory; for postgres it is a connection string
	// (postgres://user:pass@host:5432/db?sslmode=disable).
	DSN string `yaml:"dsn"`
}

type IndexConfig struct {
	// Driver selects the index backend. "local" (the default) uses the
	// file-based bleve full-text index and the sqlite-vec vector index,
	// configured through the fulltext/vector sections below. "postgres" uses a
	// single hybrid PostgreSQL index (native full-text + pgvector), suitable
	// for a shared deployment where several processes query the same database
	// concurrently.
	Driver   string              `yaml:"driver"`
	Fulltext FulltextIndexConfig `yaml:"fulltext"`
	Vector   VectorIndexConfig   `yaml:"vector"`
	Postgres PostgresIndexConfig `yaml:"postgres"`
}

// PostgresIndexConfig configures the hybrid PostgreSQL index (index.driver:
// postgres). The vector leg is active only when llm.embeddings is configured;
// otherwise the index degrades to full-text search only.
type PostgresIndexConfig struct {
	// DSN is the PostgreSQL connection string. When empty it defaults to
	// store.dsn (only valid when the store is also postgres), letting the index
	// and the document store share a single database.
	DSN    string  `yaml:"dsn"`
	Weight float64 `yaml:"weight"`
	// VectorSize is the pgvector column dimension; 0 defers to the library
	// default.
	VectorSize int `yaml:"vector_size"`
	// MaxWords bounds the chunk size sent to the embeddings model; 0 defers to
	// the library default.
	MaxWords int `yaml:"max_words"`
	// TextSearchConfig is the regconfig used when language detection is
	// inconclusive; empty defers to the library default ("simple").
	TextSearchConfig string `yaml:"text_search_config"`
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
	// EmbeddingsConcurrency bounds how many embedding batches are computed in
	// parallel for a single document. It is the main lever for large-file
	// indexing latency (one big file is a single task, so cross-file parallelism
	// does not help it). Raise it if your embeddings endpoint tolerates more
	// concurrent requests; lower it to avoid rate limiting (429). 0 defers to the
	// library default.
	EmbeddingsConcurrency int `yaml:"embeddings_concurrency"`
	// ReadPool is the number of dedicated read connections opened for
	// concurrent searches (WAL). 0 defers to the library default (4).
	ReadPool int `yaml:"read_pool"`
	// CoarseQuantization enables the two-stage vector search: a fast KNN on
	// binary-quantized vectors preselects candidates, re-scored with the full
	// float vectors. Worthwhile on large corpora (100k+ chunks); requires a
	// vector size divisible by 8. Off by default.
	CoarseQuantization bool `yaml:"coarse_quantization"`
}

type LLMConfig struct {
	Chat       *ClientConfig `yaml:"chat"`
	Embeddings *ClientConfig `yaml:"embeddings"`
	// Stages assigns a dedicated chat client to individual retrieval stages
	// (hyde, judge, grounding, rerank, decompose, reformulate), overriding
	// llm.chat for that stage. The main cost lever of a search: point the
	// high-volume stages (hyde, judge) at a small fast model while keeping a
	// stronger model as the default. Requires llm.chat.
	Stages map[string]*ClientConfig `yaml:"stages"`
	// Cache is the persistent on-disk LLM cache: embedding vectors (below
	// <path>/embeddings) and deterministic seeded chat completions such as
	// HyDE (below <path>/chat).
	Cache LLMCacheConfig `yaml:"cache"`
}

// StageNames lists the retrieval stages accepted under llm.stages, mirroring
// amoxtli.Stages.
var StageNames = []string{"hyde", "judge", "grounding", "rerank", "decompose", "reformulate"}

type ClientConfig struct {
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
}

// LLMCacheConfig configures the persistent LLM cache. Embedding a text is
// deterministic for a given model — and the HyDE completion is seeded per
// query — so both are reused across runs: re-indexing unchanged content and
// repeating a query become cache hits instead of billable, rate-limited calls.
type LLMCacheConfig struct {
	// Enabled accepts true, false or "auto"; auto (the default) enables the
	// cache whenever an embeddings or chat client is configured.
	Enabled Toggle `yaml:"enabled"`
	// Path is the cache root directory, relative to the .amoxtli directory
	// (default "cache"). Entries are keyed by model, so switching model never
	// serves results from the wrong space.
	Path string `yaml:"path"`
}

// DefaultLLMCachePath is the LLM cache root used when llm.cache.path is left
// empty, relative to the .amoxtli directory.
const DefaultLLMCachePath = "cache"

// SupportedProviders lists the llm.chat / llm.embeddings providers the CLI
// can wire. All share provider.CommonOptions (model, base_url, api_key).
var SupportedProviders = []string{"openai", "openrouter", "mistral"}

func isSupportedProvider(name string) bool {
	return slices.Contains(SupportedProviders, name)
}

// looksLikePostgresDSN reports whether dsn is a PostgreSQL connection URL. It
// guards against a postgres backend accidentally inheriting the default sqlite
// path from Default().
func looksLikePostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// Retrieval profiles: presets of the LLM retrieval stages, from cheapest to
// most thorough. The SciFact evaluations showed the embedder dominates
// quality, so the cheap profile is usually competitive.
const (
	// ProfileFast disables every per-search chat call (no HyDE, no Judge):
	// embeddings + weighted RRF fusion + dedup only.
	ProfileFast = "fast"
	// ProfileBalanced keeps HyDE (one cached, seeded chat call) but no Judge.
	ProfileBalanced = "balanced"
	// ProfilePrecision adds the fused grounding evaluator on top of HyDE.
	ProfilePrecision = "precision"
)

// Profiles lists the accepted retrieval.profile values.
var Profiles = []string{ProfileFast, ProfileBalanced, ProfilePrecision}

type RetrievalConfig struct {
	// Profile selects a preset of retrieval stages: "fast" (no per-search
	// chat call), "balanced" (HyDE only) or "precision" (HyDE + grounding
	// evaluator). Empty keeps the historical default (HyDE + Judge when
	// llm.chat is configured). Explicit keys below still apply on top of the
	// profile (they can enable more stages, not disable the profile's).
	Profile           string `yaml:"profile"`
	Reranking         bool   `yaml:"reranking"`
	GroundingCheck    bool   `yaml:"grounding_check"`
	GroundingFailOpen bool   `yaml:"grounding_fail_open"`
	// MaxTotalWords bounds the prompt size (in words) of the LLM retrieval
	// stages (reranker, judge, evidence evaluator). Keep it low enough that the
	// resulting prompt fits your chat endpoint's context window: words are only
	// a coarse proxy for tokens (~1.8 tokens/word on mixed prose and code), so
	// 18000 words already overflow a 32k-token limit. Zero defers to the
	// library default (8000).
	MaxTotalWords int `yaml:"max_total_words"`
	// MaxSectionWords bounds how many words of each retrieved section are
	// included in those prompts, on top of MaxTotalWords. Relevance is almost
	// always judgeable from the beginning of a section, so a low cap cuts the
	// per-search prompt cost. Zero defers to the library default (200).
	MaxSectionWords int                 `yaml:"max_section_words"`
	Iterative       IterativeConfig     `yaml:"iterative"`
	Decomposition   DecompositionConfig `yaml:"decomposition"`
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
			Driver: "local",
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

// LLMCacheEnabled resolves the LLM cache toggle: enabled by default as soon as
// an embeddings or chat client is configured.
func (c *Config) LLMCacheEnabled() bool {
	return (c.HasEmbeddings() || c.HasChat()) && c.LLM.Cache.Enabled.Resolve(true)
}

// LLMCachePath returns the configured LLM cache root directory, falling back
// to DefaultLLMCachePath. The path may be relative to the .amoxtli directory
// (resolve it with workspace.Resolve).
func (c *Config) LLMCachePath() string {
	if c.LLM.Cache.Path != "" {
		return c.LLM.Cache.Path
	}
	return DefaultLLMCachePath
}

// VectorEnabled resolves the vector index toggle against the presence of an
// embeddings client. It only governs the local sqlite-vec index; the postgres
// index manages its vector leg internally.
func (c *Config) VectorEnabled() bool {
	return c.IndexDriver() == "local" && c.Index.Vector.Enabled.Resolve(c.HasEmbeddings())
}

// IndexDriver returns the normalized index backend ("local" when unset).
func (c *Config) IndexDriver() string {
	if c.Index.Driver == "" {
		return "local"
	}
	return c.Index.Driver
}

// PostgresIndexDSN returns the DSN the postgres index should connect to,
// falling back to the store DSN when the index DSN is left empty and the store
// is itself postgres.
func (c *Config) PostgresIndexDSN() string {
	if c.Index.Postgres.DSN != "" {
		return c.Index.Postgres.DSN
	}
	if c.Store.Driver == "postgres" {
		return c.Store.DSN
	}
	return ""
}

// HasLocalState reports whether the workspace opens any file-based backend
// that a single process must hold exclusively (bleve, sqlite-vec or the sqlite
// store). When false, the workspace is fully backed by a client-server
// database and several processes may run against it concurrently, so the
// exclusive workspace lock is skipped.
func (c *Config) HasLocalState() bool {
	return c.Store.Driver != "postgres" || c.IndexDriver() != "postgres"
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
		if !looksLikePostgresDSN(c.Store.DSN) {
			return errors.New("store.dsn must be a postgres connection string (postgres://...) when store.driver is \"postgres\"")
		}
	default:
		return errors.Errorf("unknown store driver %q", c.Store.Driver)
	}

	switch c.IndexDriver() {
	case "local":
		if !c.Index.Fulltext.Enabled && !c.VectorEnabled() {
			return errors.New("no index enabled: enable index.fulltext or configure llm.embeddings")
		}

		if c.Index.Vector.Enabled == ToggleTrue && !c.HasEmbeddings() {
			return errors.New("index.vector.enabled is true but llm.embeddings is not configured")
		}
	case "postgres":
		dsn := c.PostgresIndexDSN()
		if dsn == "" {
			return errors.New("index.driver is \"postgres\" but no DSN is available: set index.postgres.dsn or use the postgres store driver")
		}
		if !looksLikePostgresDSN(dsn) {
			return errors.New("index.postgres.dsn must be a postgres connection string (postgres://...)")
		}
	default:
		return errors.Errorf("unknown index driver %q (expected \"local\" or \"postgres\")", c.Index.Driver)
	}

	if p := c.Retrieval.Profile; p != "" && !slices.Contains(Profiles, p) {
		return errors.Errorf("retrieval.profile: unknown profile %q (supported: %v)", p, Profiles)
	}

	if !c.HasChat() {
		var needsChat []string
		if p := c.Retrieval.Profile; p == ProfileBalanced || p == ProfilePrecision {
			needsChat = append(needsChat, "retrieval.profile="+p)
		}
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

	if len(c.LLM.Stages) > 0 && !c.HasChat() {
		return errors.New("llm.stages requires llm.chat to be configured (stages override the default chat client)")
	}

	clients := []struct {
		name string
		cfg  *ClientConfig
	}{
		{"llm.chat", c.LLM.Chat},
		{"llm.embeddings", c.LLM.Embeddings},
	}

	for name, stage := range c.LLM.Stages {
		if !slices.Contains(StageNames, name) {
			return errors.Errorf("llm.stages: unknown stage %q (supported: %v)", name, StageNames)
		}
		clients = append(clients, struct {
			name string
			cfg  *ClientConfig
		}{"llm.stages." + name, stage})
	}

	for _, client := range clients {
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
