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
    # Two-stage vector search: binary-quantized (Hamming) preselection then
    # float re-scoring. ~30x faster scans on large corpora (100k+ chunks) for a
    # marginal quality cost. Requires vector_size divisible by 8. Off by default.
    coarse_quantization: false
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
#   # rerank, decompose, reformulate, translate), overriding llm.chat for that
#   # stage.
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
  # Stage preset, from cheapest to most thorough (requires llm.chat except
  # "fast"). Explicit keys below still add stages on top of the profile.
  #   fast      = no per-search chat call (embeddings + RRF fusion + dedup);
  #   balanced  = + HyDE query expansion (one cached, seeded chat call);
  #   precision = + grounding evaluator (relevance filtering + verdict).
  # Empty keeps the historical default (HyDE + Judge when llm.chat is set).
  profile: ""
  reranking: false
  grounding_check: false
  grounding_fail_open: true
  # How the grounding verdict is applied: "demote" (default) keeps every
  # document but ranks irrelevant ones last (preserves recall, improves
  # ranking); "filter" drops irrelevant documents (high list precision, lower
  # recall — for short-list RAG).
  grounding_mode: demote
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
  # Widens the query sent to the FULL-TEXT index with its translation into the
  # languages of the corpus (metadata "lang", detected at ingestion) — the
  # vector index keeps the original wording, a multilingual embedding model
  # already crossing the language barrier on its own. Useful when questions and
  # documents are in different languages: measured on SciFact, a French query
  # over an English corpus costs the lexical leg 40% of its nDCG@10. Costs one
  # cached, seeded chat call per distinct query, and is skipped entirely when
  # the corpus is already in the query's language.
  translation:
    enabled: false
    max_languages: 2

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
  #
  # Vision converter: describes image files with a vision LLM (title,
  # description, visible text) and indexes the description as markdown tagged
  # with type=image (searchable, and filterable with --filter type=image).
  # Its extensions are routed before converter.genai's.
  #
  # vision:
  #   enabled: true
  #   # Dedicated vision model; omit to reuse llm.chat (which must then
  #   # support image attachments).
  #   chat:
  #     provider: openrouter
  #     model: qwen/qwen2.5-vl-72b-instruct
  #     api_key: ${OPENROUTER_API_KEY}
  #   extensions: [.png, .jpg, .jpeg, .webp, .gif]
  #   # Largest image sent to the model, in bytes. 0 = default (10 MiB).
  #   # A larger image is downscaled to fit, not rejected.
  #   max_image_size: 10485760
  #   # Largest image accepted for that downscaling. 0 = default (64 MiB).
  #   max_source_size: 67108864
  #   # Custom description prompt (part of the description cache key).
  #   # prompt: |
  #   #   ...
  #   # Also describe the images embedded in documents (native .md as well as
  #   # pandoc/LibreOffice/OCR output). Relative image paths are resolved
  #   # against the directory of the indexed file and confined to it; remote
  #   # images are never fetched.
  #   embedded:
  #     enabled: true
  #     # Smallest accepted side in px — below it an image is an icon, not
  #     # content. 0 = default (64), negative disables the filter.
  #     min_dimensions: 64
  #     # Main cost lever on image-rich corpora. 0 = default (32).
  #     max_images_per_document: 32
  #     # Parallel descriptions per document. 0 = default (2).
  #     concurrency: 2

# Storage of the images referenced by the indexed documents: it is what makes
# them displayable again (MCP fetch_image / resources), not merely searchable.
# Documents point at them with amoxtli://images/<hash>, a destination that
# survives the rendering of a chunk — unlike a data URI.
images:
  # "auto" (default) follows the document store: "database" when it is
  # postgres (one server to back up), "fs" when it is sqlite (keeping the bytes
  # out of a file bleve and sqlite-vec already sit next to). "none" disables
  # storage. With "auto", storage follows converter.vision.enabled; naming a
  # backend explicitly turns it on by itself.
  store: auto
  # Directory of the "fs" backend, relative to this one.
  path: blobs
  # Largest stored image, in bytes. 0 = default (10 MiB).
  max_size: 0

indexing:
  # 0 defers to the library defaults.
  max_words_per_section: 0
  task_parallelism: 0
  persistent_tasks: true
  # Source-code indexing (tree-sitter, pure Go). Code files are split into
  # declaration-level sections and tagged with type=code and language=<name>
  # metadata, filterable at search time (e.g. --filter language=go, or
  # --filter '!type' to search documents carrying no type, i.e. documentation
  # only — every other operator requires the key to be present).
  code:
    # true, false or auto (auto enables it; no external tool required).
    enabled: auto
    # Extend or override the extension→language mapping. Built-in languages:
    # go, javascript, typescript, tsx, python, php.
    # extensions:
    #   .phtml: php
`
