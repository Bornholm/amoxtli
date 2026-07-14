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

.PHONY: eval-clean
eval-clean: ## Remove the downloaded datasets and the eval virtualenv
	rm -rf $(EVAL_DATA_DIR) $(EVAL_VENV)
