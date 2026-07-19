package config

// Template is the commented configuration written by "amoxtli init". It must
// stay loadable as-is (comments are not expanded, see ExpandEnv).
const Template = `# amoxtli workspace configuration.
#
# Values support environment variable interpolation: ${VAR} fails if VAR is
# undefined, ${VAR:-default} falls back to a default. Secrets can also be put
# in a .env file next to this one (KEY=VALUE, ignored by git).
version: 1

store:
  # "sqlite" (file-based, dsn relative to this directory) or "postgres"
  # (client-server, dsn is a connection string) for a shared deployment.
  driver: sqlite
  dsn: data/store.sqlite

index:
  # Index backend: "local" (bleve full-text + sqlite-vec vector, configured
  # below) or "postgres" (single hybrid index, see the postgres section). Use
  # postgres together with a postgres store to let several processes query the
  # same database concurrently (e.g. multiple "amoxtli mcp http" instances).
  driver: local
  # Full-text (BM25) index; works fully offline. (local driver)
  fulltext:
    enabled: true
    path: data/index.bleve
    weight: 1.0
  # Vector (semantic) index. "auto" enables it when llm.embeddings is
  # configured; results of both indexes are fused by weighted RRF. (local driver)
  vector:
    enabled: auto
    path: data/vectors.sqlite
    weight: 1.0
    # 0 defers to the library defaults.
    vector_size: 0
    max_words: 0
    # Number of embedding batches computed in parallel per document. Main lever
    # for large-file indexing speed. Raise if your embeddings endpoint tolerates
    # more concurrency; lower to avoid rate limiting (429). 0 = default (8).
    embeddings_concurrency: 0
    # Dedicated read connections for concurrent searches (WAL). 0 = default (4).
    read_pool: 0
  # Hybrid PostgreSQL index (index.driver: postgres). Requires the "vector" and
  # "unaccent" extensions. The vector leg activates when llm.embeddings is set.
  #
  # postgres:
  #   # Defaults to store.dsn when the store is also postgres.
  #   dsn: postgres://user:pass@localhost:5432/kb?sslmode=disable
  #   weight: 1.0
  #   # 0 defers to the library defaults.
  #   vector_size: 0
  #   max_words: 0
  #   text_search_config: simple

# LLM clients (optional). Supported providers: openai, openrouter, mistral.
# "openai" also covers any OpenAI-compatible endpoint (Ollama, vLLM...) via
# base_url.
#
# llm:
#   chat:
#     provider: openrouter
#     model: anthropic/claude-sonnet-4
#     api_key: ${OPENROUTER_API_KEY}
#   embeddings:
#     provider: openai
#     base_url: http://localhost:11434/v1
#     model: bge-m3
#     api_key: ${OLLAMA_API_KEY:-ollama}
#   # Persistent on-disk LLM cache: embedding vectors and deterministic seeded
#   # chat completions (HyDE). Re-indexing unchanged content and repeating a
#   # query stop hitting the endpoints. Enabled by default ("auto"); purge it
#   # with "amoxtli cache purge".
#   cache:
#     enabled: auto
#     path: cache
#   # Dedicated chat client per retrieval stage (hyde, judge, grounding,
#   # rerank, decompose, reformulate), overriding llm.chat for that stage.
#   # Main cost lever: point the high-volume stages at a small fast model.
#   stages:
#     hyde:
#       provider: openrouter
#       model: anthropic/claude-haiku-4.5
#       api_key: ${OPENROUTER_API_KEY}
#     judge:
#       provider: openrouter
#       model: anthropic/claude-haiku-4.5
#       api_key: ${OPENROUTER_API_KEY}

# Retrieval enhancements; all of them require llm.chat.
retrieval:
  reranking: false
  grounding_check: false
  grounding_fail_open: true
  # Prompt budget (in words) for the LLM retrieval stages (reranker, judge,
  # evidence evaluator). Words are a coarse proxy for tokens (~1.8 tokens/word),
  # so keep this well under your chat endpoint's context window. 0 uses the
  # built-in default (8000, ~14k tokens). Lower it for smaller context windows.
  max_total_words: 0
  # Per-section cap (in words) inside those prompts, on top of max_total_words:
  # relevance is almost always judgeable from the beginning of a section. 0 uses
  # the built-in default (200).
  max_section_words: 0
  iterative:
    enabled: false
    max_rounds: 2
  decomposition:
    enabled: false
    max_sub_queries: 3

converter:
  # File conversion to markdown. "auto" enables pandoc when the binary is in
  # the PATH; without it only .md files can be indexed.
  # Supported: .docx .rtf .odt .md .rst .epub .html .tex .txt
  pandoc:
    enabled: auto
  # LibreOffice adds .doc support on top of pandoc. "auto" enables it when
  # both the libreoffice and pandoc binaries are present; it supersedes the
  # standalone pandoc converter above.
  libreoffice:
    enabled: auto
  # GenAI (OCR/LLM) converter for formats pandoc cannot read (PDF, images...).
  # Opt-in: set a DSN and the extensions to route to it.
  #
  # genai:
  #   enabled: true
  #   dsn: mistral://?apiKey=${MISTRAL_API_KEY}   # or marker://host:port
  #   extensions: [.pdf, .png, .jpg, .jpeg]

indexing:
  # 0 defers to the library defaults.
  max_words_per_section: 0
  task_parallelism: 0
  persistent_tasks: true
  # Source-code indexing (tree-sitter, pure Go). Code files are split into
  # declaration-level sections and tagged with type=code and language=<name>
  # metadata, filterable at search time (e.g. --filter language=go, or
  # --filter "type!=code" to search documentation only).
  code:
    # true, false or auto (auto enables it; no external tool required).
    enabled: auto
    # Extend or override the extension→language mapping. Built-in languages:
    # go, javascript, typescript, tsx, python, php.
    # extensions:
    #   .phtml: php
`
