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
  # Only sqlite is supported for now; paths are relative to this directory.
  driver: sqlite
  dsn: data/store.sqlite

index:
  # Full-text (BM25) index; works fully offline.
  fulltext:
    enabled: true
    path: data/index.bleve
    weight: 1.0
  # Vector (semantic) index. "auto" enables it when llm.embeddings is
  # configured; results of both indexes are fused by weighted RRF.
  vector:
    enabled: auto
    path: data/vectors.sqlite
    weight: 1.0
    # 0 defers to the library defaults.
    vector_size: 0
    max_words: 0

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

# Retrieval enhancements; all of them require llm.chat.
retrieval:
  reranking: false
  grounding_check: false
  grounding_fail_open: true
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
