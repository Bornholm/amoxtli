package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) (string, bool) { return "", false }

func TestParseTemplate(t *testing.T) {
	cfg, err := Parse(Template, noEnv)
	if err != nil {
		t.Fatalf("the generated template must load as-is: %+v", err)
	}

	if !cfg.Index.Fulltext.Enabled {
		t.Error("expected fulltext index enabled by default")
	}
	if cfg.VectorEnabled() {
		t.Error("expected vector index disabled without embeddings")
	}
	if cfg.HasChat() {
		t.Error("expected no chat client by default")
	}
}

func TestParseEmpty(t *testing.T) {
	cfg, err := Parse("", noEnv)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	expected := Default()
	if cfg.Store.DSN != expected.Store.DSN || cfg.Index.Fulltext.Path != expected.Index.Fulltext.Path {
		t.Errorf("expected defaults, got %+v", cfg)
	}
}

func TestParseFull(t *testing.T) {
	raw := `
version: 1
index:
  vector:
    enabled: auto
    weight: 2.5
llm:
  chat:
    provider: openai
    base_url: https://example.com/v1
    model: some-model
    api_key: ${API_KEY}
  embeddings:
    provider: openai
    base_url: https://example.com/v1
    model: some-embedder
    api_key: ${API_KEY}
retrieval:
  reranking: true
  iterative:
    enabled: true
    max_rounds: 4
`
	lookup := func(name string) (string, bool) {
		if name == "API_KEY" {
			return "sk-secret", true
		}
		return "", false
	}

	cfg, err := Parse(raw, lookup)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if cfg.LLM.Chat.APIKey != "sk-secret" {
		t.Errorf("expected interpolated api key, got %q", cfg.LLM.Chat.APIKey)
	}
	if !cfg.VectorEnabled() {
		t.Error("expected vector index enabled (auto + embeddings)")
	}
	if cfg.Index.Vector.Weight != 2.5 {
		t.Errorf("expected overridden weight 2.5, got %v", cfg.Index.Vector.Weight)
	}
	if cfg.Retrieval.Iterative.MaxRounds != 4 {
		t.Errorf("expected max_rounds 4, got %d", cfg.Retrieval.Iterative.MaxRounds)
	}
	// Defaults must survive a partial file.
	if cfg.Store.DSN != "data/store.sqlite" {
		t.Errorf("expected default store DSN, got %q", cfg.Store.DSN)
	}
}

func TestParsePostgres(t *testing.T) {
	raw := `
version: 1
store:
  driver: postgres
  dsn: postgres://user:pass@localhost:5432/kb?sslmode=disable
index:
  driver: postgres
`
	cfg, err := Parse(raw, noEnv)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if cfg.IndexDriver() != "postgres" {
		t.Errorf("expected postgres index driver, got %q", cfg.IndexDriver())
	}
	// The index DSN falls back to the postgres store DSN.
	if cfg.PostgresIndexDSN() != cfg.Store.DSN {
		t.Errorf("expected index DSN to fall back to store DSN, got %q", cfg.PostgresIndexDSN())
	}
	// A fully client-server workspace holds no exclusive on-disk state.
	if cfg.HasLocalState() {
		t.Error("expected HasLocalState to be false for a full postgres workspace")
	}
	// The local sqlite-vec index must stay off under the postgres driver.
	if cfg.VectorEnabled() {
		t.Error("expected VectorEnabled to be false under the postgres index driver")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "unknown field",
			raw:     "stoore:\n  driver: sqlite",
			wantErr: "not found",
		},
		{
			name:    "unsupported version",
			raw:     "version: 2",
			wantErr: "unsupported config version",
		},
		{
			name:    "postgres store with non-postgres dsn",
			raw:     "store:\n  driver: postgres",
			wantErr: "must be a postgres connection string",
		},
		{
			name:    "postgres index without dsn",
			raw:     "index:\n  driver: postgres",
			wantErr: "no DSN is available",
		},
		{
			name:    "unknown index driver",
			raw:     "index:\n  driver: elastic",
			wantErr: "unknown index driver",
		},
		{
			name:    "vector true without embeddings",
			raw:     "index:\n  vector:\n    enabled: true",
			wantErr: "llm.embeddings",
		},
		{
			name:    "reranking without chat",
			raw:     "retrieval:\n  reranking: true",
			wantErr: "requires llm.chat",
		},
		{
			name:    "iterative without chat",
			raw:     "retrieval:\n  iterative:\n    enabled: true",
			wantErr: "requires llm.chat",
		},
		{
			name:    "no index at all",
			raw:     "index:\n  fulltext:\n    enabled: false",
			wantErr: "no index enabled",
		},
		{
			name:    "unknown provider",
			raw:     "llm:\n  chat:\n    provider: bedrock\n    model: m",
			wantErr: "unsupported provider",
		},
		{
			name:    "chat without model",
			raw:     "llm:\n  chat:\n    provider: openai",
			wantErr: "llm.chat.model is required",
		},
		{
			name:    "invalid toggle",
			raw:     "index:\n  vector:\n    enabled: maybe",
			wantErr: "invalid toggle",
		},
		{
			name:    "genai without dsn",
			raw:     "converter:\n  genai:\n    enabled: true\n    extensions: [.pdf]",
			wantErr: "converter.genai.dsn",
		},
		{
			name:    "stages without chat",
			raw:     "llm:\n  stages:\n    hyde:\n      provider: openai\n      model: m",
			wantErr: "llm.stages requires llm.chat",
		},
		{
			name:    "unknown retrieval profile",
			raw:     "retrieval:\n  profile: turbo",
			wantErr: "unknown profile",
		},
		{
			name:    "unknown grounding mode",
			raw:     "retrieval:\n  grounding_mode: rerank",
			wantErr: "unknown mode",
		},
		{
			name:    "balanced profile without chat",
			raw:     "retrieval:\n  profile: balanced",
			wantErr: "requires llm.chat",
		},
		{
			name:    "unknown stage",
			raw:     "llm:\n  chat:\n    provider: openai\n    model: m\n  stages:\n    hype:\n      provider: openai\n      model: m",
			wantErr: "unknown stage",
		},
		{
			name:    "stage without model",
			raw:     "llm:\n  chat:\n    provider: openai\n    model: m\n  stages:\n    judge:\n      provider: openai",
			wantErr: "llm.stages.judge.model is required",
		},
		{
			name:    "genai without extensions",
			raw:     "converter:\n  genai:\n    enabled: true\n    dsn: mistral://?apiKey=x",
			wantErr: "converter.genai.extensions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.raw, noEnv)
			if err == nil {
				t.Fatalf("expected error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestLoadWithDotEnv(t *testing.T) {
	dir := t.TempDir()

	configPath := filepath.Join(dir, "config.yaml")
	raw := "llm:\n  chat:\n    provider: openai\n    model: some-model\n    api_key: ${DOTENV_TEST_KEY}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nexport DOTENV_TEST_KEY=\"from-dotenv\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if cfg.LLM.Chat.APIKey != "from-dotenv" {
		t.Errorf("expected api key from .env, got %q", cfg.LLM.Chat.APIKey)
	}

	// The process environment takes precedence over the .env file.
	t.Setenv("DOTENV_TEST_KEY", "from-env")

	cfg, err = Load(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if cfg.LLM.Chat.APIKey != "from-env" {
		t.Errorf("expected api key from environment, got %q", cfg.LLM.Chat.APIKey)
	}
}
