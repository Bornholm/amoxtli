# amoxtli — Makefile
#
# Evaluation benchmark targets: download a real Hugging Face QA dataset, load it
# and run the retrieval-quality evaluation (Recall@k / MRR / nDCG@k) on it.

.DEFAULT_GOAL := help

# ---- Evaluation configuration (override on the command line) ----------------
EVAL_DATA_DIR    ?= .eval-data
EVAL_VENV        ?= .eval-venv
EVAL_DATASET     ?= rajpurkar/squad
EVAL_CONFIG      ?= plain_text
EVAL_SPLIT       ?= validation
EVAL_LANG        ?= en
EVAL_MAX_ROWS    ?= 3000
EVAL_MAX_DOCS    ?= 500
EVAL_MAX_QUERIES ?= 200
EVAL_TOPK        ?= 10

EVAL_FILE       := $(EVAL_DATA_DIR)/squad-$(EVAL_LANG).json
EVAL_LANG_UPPER := $(shell echo $(EVAL_LANG) | tr '[:lower:]' '[:upper:]')

.PHONY: help
help: ## Show this help
	@echo "amoxtli — targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Evaluation knobs (make eval VAR=value): EVAL_DATASET, EVAL_CONFIG,"
	@echo "EVAL_SPLIT, EVAL_LANG, EVAL_MAX_ROWS, EVAL_MAX_DOCS, EVAL_MAX_QUERIES, EVAL_TOPK."

.PHONY: test
test: ## Run the short unit test suite
	go test -short ./...

# Local virtualenv holding the `datasets` package (the official HF loader).
$(EVAL_VENV)/.installed:
	python3 -m venv $(EVAL_VENV)
	$(EVAL_VENV)/bin/pip install --quiet --upgrade pip
	$(EVAL_VENV)/bin/pip install --quiet datasets
	touch $@

# Download + convert the dataset to SQuAD JSON (only when missing).
$(EVAL_FILE): $(EVAL_VENV)/.installed scripts/hf_to_squad.py
	mkdir -p $(EVAL_DATA_DIR)
	$(EVAL_VENV)/bin/python scripts/hf_to_squad.py \
		--dataset "$(EVAL_DATASET)" --config "$(EVAL_CONFIG)" --split "$(EVAL_SPLIT)" \
		--max-rows $(EVAL_MAX_ROWS) --out "$(EVAL_FILE)"

.PHONY: eval-download
eval-download: $(EVAL_FILE) ## Download + convert the HF dataset to SQuAD JSON

.PHONY: eval
eval: $(EVAL_FILE) ## Download (if needed), load and run the evaluation benchmark
	AMOXTLI_EVAL=1 \
	AMOXTLI_EVAL_SQUAD_$(EVAL_LANG_UPPER)="$(abspath $(EVAL_FILE))" \
	AMOXTLI_EVAL_MAX_DOCS=$(EVAL_MAX_DOCS) \
	AMOXTLI_EVAL_MAX_QUERIES=$(EVAL_MAX_QUERIES) \
	AMOXTLI_EVAL_TOPK=$(EVAL_TOPK) \
	go test ./eval/ -run TestEvaluateRealWorld -v -timeout 30m

# ---- Per-language convenience targets ---------------------------------------
# Best-effort dataset coordinates; override EVAL_* if a config/split differs.

.PHONY: eval-en
eval-en: ## Evaluate on English SQuAD (rajpurkar/squad)
	$(MAKE) eval EVAL_DATASET=rajpurkar/squad EVAL_CONFIG=plain_text EVAL_SPLIT=validation EVAL_LANG=en

.PHONY: eval-es
eval-es: ## Evaluate on Spanish squad_es
	$(MAKE) eval EVAL_DATASET=ccasimiro/squad_es EVAL_CONFIG=v1.1.0 EVAL_SPLIT=validation EVAL_LANG=es

.PHONY: eval-fr
eval-fr: ## Evaluate on French PIAF (etalab-ia/piaf)
	$(MAKE) eval EVAL_DATASET=etalab-ia/piaf EVAL_CONFIG= EVAL_SPLIT=train EVAL_LANG=fr

# ---- BEIR evaluation ---------------------------------------------------------
# Runs TestEvaluateBEIR on a BEIR dataset (gold-aware subsample). Lexical-only
# by default; export AMOXTLI_EVAL_EMBED_* for hybrid, AMOXTLI_EVAL_RERANK=1 for
# reranking, AMOXTLI_EVAL_EMBED_CACHE_DIR for the persistent embeddings cache.

EVAL_BEIR           ?= scifact
EVAL_BEIR_SETS      ?= scifact nfcorpus
EVAL_SAMPLE_DOCS    ?= 1000
EVAL_SAMPLE_QUERIES ?= 300
EVAL_BEIR_STREAM    ?=
EVAL_TIMEOUT        ?= 60m
EVAL_SUMMARY        ?= $(EVAL_DATA_DIR)/summary.tsv
EVAL_GOLDEN_NDCG10  ?= 0.78

.PHONY: eval-beir-download
eval-beir-download: ## Download the BEIR dataset (EVAL_BEIR=scifact|nfcorpus|fiqa|...)
	scripts/download_beir.sh $(EVAL_BEIR) $(EVAL_DATA_DIR)

.PHONY: eval-beir
eval-beir: eval-beir-download ## Run the BEIR evaluation benchmark (EVAL_BEIR=...)
	AMOXTLI_EVAL=1 \
	AMOXTLI_EVAL_BEIR_CORPUS=$(abspath $(EVAL_DATA_DIR)/$(EVAL_BEIR)/corpus.jsonl) \
	AMOXTLI_EVAL_BEIR_QUERIES=$(abspath $(EVAL_DATA_DIR)/$(EVAL_BEIR)/queries.jsonl) \
	AMOXTLI_EVAL_BEIR_QRELS=$(abspath $(EVAL_DATA_DIR)/$(EVAL_BEIR)/qrels/test.tsv) \
	AMOXTLI_EVAL_BEIR_NAME=$(EVAL_BEIR) \
	AMOXTLI_EVAL_SAMPLE_DOCS=$(EVAL_SAMPLE_DOCS) \
	AMOXTLI_EVAL_SAMPLE_QUERIES=$(EVAL_SAMPLE_QUERIES) \
	AMOXTLI_EVAL_BEIR_STREAM=$(EVAL_BEIR_STREAM) \
	AMOXTLI_EVAL_TOPK=$(EVAL_TOPK) \
	AMOXTLI_EVAL_SUMMARY_FILE=$(abspath $(EVAL_SUMMARY)) \
	go test ./eval/ -run TestEvaluateBEIR -v -timeout $(EVAL_TIMEOUT)

.PHONY: eval-fever
eval-fever: ## Fact-checking benchmark FEVER (streamed: ~5.4M-doc corpus, gold-aware subsample)
	$(MAKE) eval-beir EVAL_BEIR=fever EVAL_BEIR_STREAM=1 \
		EVAL_SAMPLE_DOCS=$(or $(FEVER_DOCS),5000) \
		EVAL_SAMPLE_QUERIES=$(or $(FEVER_QUERIES),300) \
		EVAL_TIMEOUT=$(or $(FEVER_TIMEOUT),90m)

HOTPOT_ANSWERS := $(EVAL_DATA_DIR)/hotpot_answers.json

# Gold answers for the HotpotQA generation (reader) evaluation, pulled from
# Hugging Face (the CMU host in the HotpotQA README is often offline).
$(HOTPOT_ANSWERS): $(EVAL_VENV)/.installed scripts/download_hotpot_answers.py
	mkdir -p $(EVAL_DATA_DIR)
	$(EVAL_VENV)/bin/python scripts/download_hotpot_answers.py \
		--config fullwiki --split validation --out "$(HOTPOT_ANSWERS)"

.PHONY: eval-hotpotqa-gen
eval-hotpotqa-gen: $(HOTPOT_ANSWERS) ## HotpotQA end-to-end: retrieval + answer EM/F1 (reader). Export AMOXTLI_EVAL_EMBED_*/CHAT_* (or use scripts/eval_env.sh) and AMOXTLI_EVAL_WORKDIR to reuse a built index.
	AMOXTLI_EVAL_GENERATE=1 \
	AMOXTLI_EVAL_BEIR_ANSWERS=$(abspath $(HOTPOT_ANSWERS)) \
	$(MAKE) --no-print-directory eval-beir EVAL_BEIR=hotpotqa EVAL_BEIR_STREAM=1

.PHONY: eval-matrix
eval-matrix: ## Run eval-beir over EVAL_BEIR_SETS, then print the consolidated table
	@for ds in $(EVAL_BEIR_SETS); do \
		$(MAKE) eval-beir EVAL_BEIR=$$ds || exit 1; \
	done
	@$(MAKE) --no-print-directory eval-summary

.PHONY: eval-summary
eval-summary: ## Print the consolidated results table (dataset x config x metrics)
	@{ printf 'dataset\tmode\tqueries\tMRR\tRecall@k\tnDCG@k\n'; cat $(EVAL_SUMMARY); } | column -t -s "$$(printf '\t')"

.PHONY: eval-summary-reset
eval-summary-reset: ## Reset the consolidated results table
	rm -f $(EVAL_SUMMARY)

.PHONY: eval-golden
eval-golden: ## Non-regression golden set: SciFact 1000 docs, lexical only, nDCG@10 floor
	AMOXTLI_EVAL_EMBED_BASE_URL= AMOXTLI_EVAL_EMBED_MODEL= \
	AMOXTLI_EVAL_RERANK= AMOXTLI_EVAL_ITERATIVE= AMOXTLI_EVAL_HYDE= \
	AMOXTLI_EVAL_WORKDIR= AMOXTLI_EVAL_MIN_NDCG10=$(EVAL_GOLDEN_NDCG10) \
	$(MAKE) --no-print-directory eval-beir EVAL_BEIR=scifact EVAL_TIMEOUT=20m

.PHONY: eval-clean
eval-clean: ## Remove the downloaded datasets and the eval virtualenv
	rm -rf $(EVAL_DATA_DIR) $(EVAL_VENV)
