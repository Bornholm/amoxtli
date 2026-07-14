#!/usr/bin/env bash
# Map the .env GENAI_* provider credentials onto the AMOXTLI_EVAL_* variables the
# evaluation harness reads, then exec the given command. This keeps the API key
# out of the command line and the logs (it is read from .env at run time, never
# echoed). Only sets the embeddings/chat endpoints — model-specific knobs
# (AMOXTLI_EVAL_EMBED_DIM, _WORKDIR, _ITERATIVE, ...) stay the caller's job.
#
# Usage:
#   scripts/eval_env.sh make eval-beir EVAL_BEIR=hotpotqa ...
#   ENV_FILE=.env.other scripts/eval_env.sh make eval ...
set -euo pipefail

env_file="${ENV_FILE:-.env}"
if [ -f "$env_file" ]; then
	set -a
	# shellcheck disable=SC1090
	. "$env_file"
	set +a
fi

# Embeddings endpoint (enables the vector backend / hybrid fusion).
if [ -n "${GENAI_EMBEDDINGS_OPENROUTER_MODEL:-}" ]; then
	export AMOXTLI_EVAL_EMBED_BASE_URL="${GENAI_EMBEDDINGS_OPENROUTER_BASE_URL%/}/"
	export AMOXTLI_EVAL_EMBED_MODEL="$GENAI_EMBEDDINGS_OPENROUTER_MODEL"
	export AMOXTLI_EVAL_EMBED_API_KEY="${GENAI_EMBEDDINGS_OPENROUTER_API_KEY:-}"
fi

# Chat endpoint (HyDE, reranking, grounding evaluator, query reformulation).
if [ -n "${GENAI_CHAT_COMPLETION_OPENROUTER_MODEL:-}" ]; then
	export AMOXTLI_EVAL_CHAT_BASE_URL="${GENAI_CHAT_COMPLETION_OPENROUTER_BASE_URL%/}/"
	export AMOXTLI_EVAL_CHAT_MODEL="$GENAI_CHAT_COMPLETION_OPENROUTER_MODEL"
	export AMOXTLI_EVAL_CHAT_API_KEY="${GENAI_CHAT_COMPLETION_OPENROUTER_API_KEY:-}"
fi

exec "$@"
