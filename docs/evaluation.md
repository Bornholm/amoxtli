# Évaluation de la pertinence

Le package [`eval`](../eval) mesure objectivement la qualité de récupération —
**Recall@k**, **MRR** et **nDCG@k** — d'un `Retriever` quelconque contre un jeu
de requêtes « golden » (requête → sources pertinentes attendues). Les fonctions
de métriques sont pures et couvertes par les tests courts ; ce document décrit
comment lancer une évaluation **en conditions réelles** sur un vrai jeu de
données QA multilingue.

## Principe

Un jeu QA extractif (SQuAD-like) se transforme naturellement en benchmark de
**récupération de passages** :

- chaque paragraphe unique du dataset devient un **document** du corpus,
  identifié par une source stable ;
- chaque question devient une **requête** dont la source pertinente est le
  paragraphe d'où elle a été rédigée.

On indexe tout le corpus, puis pour chaque question on vérifie à quel rang la
librairie retrouve le paragraphe d'origine. C'est exactement ce que mesurent
Recall@k (le bon passage est-il dans le top-k ?) et MRR / nDCG (à quel rang ?).

## Format et loader

Le loader [`eval/hfqa`](../eval/hfqa) lit le **format JSON SQuAD**, partagé par
plusieurs jeux de données multilingues construits sur de vrais documents
(Wikipedia). Un même parser couvre donc les trois langues visées :

| Langue | Jeu recommandé | Source Hugging Face |
|---|---|---|
| Français | PIAF | [`etalab-ia/piaf`](https://huggingface.co/datasets/etalab-ia/piaf) |
| Anglais | SQuAD v1.1 (dev) | [`rajpurkar/squad`](https://huggingface.co/datasets/rajpurkar/squad) |
| Espagnol | SQuAD-es | [`ccasimiro/squad_es`](https://huggingface.co/datasets/ccasimiro/squad_es) |

D'autres jeux au même format conviennent (FQuAD, MLQA, XQuAD).

### Récupérer les fichiers

Il faut des fichiers **JSON au format SQuAD** en local. Selon le jeu, ils sont
distribués directement en JSON ou en parquet. Le script fourni
[`scripts/hf_to_squad.py`](../scripts/hf_to_squad.py) télécharge n'importe quel
jeu partageant le schéma SQuAD depuis le Hub Hugging Face (via la bibliothèque
`datasets`) et l'exporte au bon format :

```bash
python scripts/hf_to_squad.py \
    --dataset rajpurkar/squad --config plain_text --split validation \
    --max-rows 3000 --out squad-en.json
```

En pratique il n'est pas nécessaire de l'appeler à la main : la cible `make eval`
ci-dessous s'en charge (installation de `datasets` incluse).

## Lancer le benchmark

### Raccourci : `make eval`

La cible `eval` du `Makefile` fait tout en une commande — télécharge un jeu
Hugging Face, le convertit au format SQuAD JSON puis lance l'évaluation :

```bash
make eval                       # SQuAD anglais (rajpurkar/squad) par défaut
make eval-fr                    # PIAF (français)
make eval-es                    # squad_es (espagnol)
make eval EVAL_DATASET=mlqa EVAL_CONFIG=mlqa.en.en EVAL_SPLIT=validation EVAL_LANG=en
```

Le premier appel crée un virtualenv local (`.eval-venv`) et y installe la
bibliothèque officielle `datasets` (le téléchargement passe donc par le Hub
Hugging Face) ; le fichier converti est mis en cache dans `.eval-data/` et n'est
re-téléchargé que s'il manque. Les bornes de coût se règlent par variables :
`make eval EVAL_MAX_DOCS=1000 EVAL_MAX_QUERIES=500`. `make help` liste les cibles
et `make eval-clean` supprime le cache et le virtualenv. Ajouter le backend
vectoriel se fait en exportant les variables `AMOXTLI_EVAL_EMBED_*` (voir
ci-dessous) avant `make eval`.

### Manuellement

L'évaluation « conditions réelles » est un **test gated**
(`eval.TestEvaluateRealWorld`), piloté par variables d'environnement — dans la
lignée des tests d'intégration Ollama/PostgreSQL. Par défaut il tourne en
**lexical pur** (backend bleve), sans aucun service LLM :

```bash
AMOXTLI_EVAL=1 \
AMOXTLI_EVAL_SQUAD_FR=./piaf-fr.json \
AMOXTLI_EVAL_SQUAD_EN=./squad-en.json \
AMOXTLI_EVAL_SQUAD_ES=./squad-es.json \
AMOXTLI_EVAL_MAX_DOCS=500 \
AMOXTLI_EVAL_MAX_QUERIES=200 \
go test ./eval/ -run TestEvaluateRealWorld -v -timeout 30m
```

Au moins un des trois fichiers de langue est requis. Le rapport est loggé
**globalement puis segmenté par langue** (`Report.ByLang()`).

### Ajouter le backend vectoriel

Pour évaluer la **fusion hybride** (bleve + sqlite-vec) plutôt que le lexical
seul, configurez un endpoint d'embeddings compatible OpenAI (Ollama, vLLM,
OpenAI, ...) :

```bash
AMOXTLI_EVAL=1 \
AMOXTLI_EVAL_SQUAD_EN=./squad-en.json \
AMOXTLI_EVAL_EMBED_BASE_URL=http://localhost:11434/v1/ \
AMOXTLI_EVAL_EMBED_MODEL=mxbai-embed-large:latest \
go test ./eval/ -run TestEvaluateRealWorld -v -timeout 60m
```

Comparer le rapport lexical et le rapport hybride sur le **même** jeu (mêmes
`MAX_DOCS`/`MAX_QUERIES`) mesure l'apport réel de la recherche vectorielle.

### Variables d'environnement

| Variable | Rôle | Défaut |
|---|---|---|
| `AMOXTLI_EVAL` | Active le test (obligatoire) | — |
| `AMOXTLI_EVAL_SQUAD_FR` / `_EN` / `_ES` | Chemins des fichiers SQuAD JSON | — |
| `AMOXTLI_EVAL_MAX_DOCS` | Nombre max de passages indexés par langue | illimité |
| `AMOXTLI_EVAL_MAX_QUERIES` | Nombre max de questions évaluées par langue | illimité |
| `AMOXTLI_EVAL_TOPK` | Plus grand cut-off (Recall@k / nDCG@k) | 10 |
| `AMOXTLI_EVAL_EMBED_BASE_URL` | Endpoint d'embeddings (active le backend vectoriel) | — |
| `AMOXTLI_EVAL_EMBED_MODEL` | Modèle d'embeddings | — |
| `AMOXTLI_EVAL_EMBED_API_KEY` | Clé d'API de l'endpoint | — |

> Bornez toujours `MAX_DOCS`/`MAX_QUERIES` sur les gros jeux : l'ingestion
> vectorielle appelle le service d'embeddings pour chaque passage. Les questions
> dont le passage a été écarté par la troncature sont automatiquement retirées
> (`Dataset.KeepAnswerable`), pour ne pas fausser le recall.

## Benchmarks BEIR (dont FEVER)

En plus des jeux SQuAD-like, le harnais évalue les jeux au **format BEIR** (la
référence en recherche d'information) via `eval.TestEvaluateBEIR` et le loader
[`eval/beir`](../eval/beir). Un jeu BEIR = trois fichiers (`corpus.jsonl`,
`queries.jsonl`, `qrels/test.tsv`). La cible `make eval-beir` télécharge le jeu
et lance l'évaluation :

```bash
make eval-beir EVAL_BEIR=scifact          # lexical pur, sous-échantillon gold-aware
make eval-beir EVAL_BEIR=nfcorpus EVAL_SAMPLE_DOCS=2000
```

Comme pour SQuAD, le backend vectoriel s'active en exportant `AMOXTLI_EVAL_EMBED_*`
et le reranking avec `AMOXTLI_EVAL_RERANK=1`. Le sous-échantillonnage
(`EVAL_SAMPLE_DOCS` / `EVAL_SAMPLE_QUERIES`) est **gold-aware** : il garde tous les
documents pertinents des requêtes retenues, puis complète avec des distracteurs —
chaque requête gardée reste donc répondable.

### FEVER et les très gros corpus (chargement en streaming)

Le corpus BEIR de **FEVER** (fact-checking sur Wikipédia) compte **~5,4 M de
documents** — impossible à charger intégralement en mémoire avant de
sous-échantillonner. Le loader dispose donc d'un mode **streaming, gold-aware**
(`beir.LoadSubsample`, activé par `AMOXTLI_EVAL_BEIR_STREAM=1`) : il lit d'abord
les requêtes/qrels, sélectionne un sous-ensemble déterministe, puis parcourt
`corpus.jsonl` **une seule fois** en ne retenant que les documents pertinents plus
un budget borné de distracteurs. Le pic mémoire est plafonné par
`EVAL_SAMPLE_DOCS`, pas par la taille du corpus. La cible dédiée l'active
d'office :

```bash
make eval-fever                                  # 5000 docs / 300 requêtes, streaming
make eval-fever FEVER_DOCS=10000 FEVER_QUERIES=500
```

Le téléchargement (~1,2 Go compressé, ~5 Go décompressés) est mis en cache dans
`.eval-data/fever/`. Pour un run **hybride** avec cache d'embeddings persistant :

```bash
AMOXTLI_EVAL_EMBED_BASE_URL=https://openrouter.ai/api/v1/ \
AMOXTLI_EVAL_EMBED_MODEL=mistralai/mistral-embed-2312 \
AMOXTLI_EVAL_EMBED_API_KEY=$OPENROUTER_KEY \
AMOXTLI_EVAL_EMBED_DIM=1024 \
AMOXTLI_EVAL_EMBED_CACHE_DIR=.eval-cache \
AMOXTLI_EVAL_LLM_MAX_RETRIES=15 AMOXTLI_EVAL_LLM_RATE=6 \
make eval-fever
```

> Le même mode streaming sert à tous les gros jeux BEIR (HotpotQA, DBPedia,
> Climate-FEVER, NQ) : `make eval-beir EVAL_BEIR=<jeu> EVAL_BEIR_STREAM=1`.

## Évaluation end-to-end : génération (reader)

Amoxtli est une librairie de **récupération** — il ne génère pas de réponse. Mais
pour mesurer une chaîne RAG complète (et se comparer aux benchmarks QA type
HotpotQA/BeerQA), le harnais propose un **reader optionnel** : il branche le
`llm.Client` déjà configuré (celui de HyDE/reranking/grounding) sur les passages
retrouvés, génère une réponse courte, et la note en **EM / F1** (normalisation
SQuAD, token-overlap) contre les réponses gold. Le code de la lib reste inchangé
— la génération vit **dans le harnais d'éval uniquement**.

Activé par `AMOXTLI_EVAL_GENERATE=1`, en plus des variables de récupération. Il
faut des **réponses gold** : les fichiers BEIR n'en portent pas, donc pour
HotpotQA on les joint depuis le jeu natif (par `_id`) via
`AMOXTLI_EVAL_BEIR_ANSWERS`. La cible dédiée récupère les réponses (HuggingFace)
et lance le tout :

```bash
# Reader HotpotQA au-dessus d'un index déjà construit (réutilisé via WORKDIR).
AMOXTLI_EVAL_WORKDIR=.eval-workdir-hotpot100k \
AMOXTLI_EVAL_EMBED_DIM=1024 AMOXTLI_EVAL_EMBED_CACHE_DIR=.eval-cache \
scripts/eval_env.sh make eval-hotpotqa-gen \
    EVAL_SAMPLE_DOCS=100000 EVAL_SAMPLE_QUERIES=100
```

Réglages : `AMOXTLI_EVAL_GEN_CONTEXT_K` (passages fournis au reader, défaut 5),
`AMOXTLI_EVAL_GEN_MAX_WORDS` (troncature par passage, défaut 250),
`AMOXTLI_EVAL_GEN_SUMMARY_FILE` (TSV `dataset / mode+gen / EM / F1`).

> **Deux limites à garder en tête.** (1) L'EM/F1 **mélange** qualité de
> récupération et qualité du reader : pour juger le *stack de récupération*, les
> métriques nDCG@k / Recall@k restent la boussole ; la génération les complète.
> (2) Un LLM génératif paraphrase → l'EM est fragile et **non superposable** aux
> baselines extractives des leaderboards (IRRR & co) ; lire le F1 comme un
> « RAG-reader F1 » légitime, pas comme un score de leaderboard.

## Évaluer par le code

Le harnais s'utilise aussi directement, sur n'importe quelle implémentation de
récupération :

```go
ds, _ := eval.LoadDataset("queries.json")
retriever := eval.FromSearchResults(func(ctx context.Context, q string, k int) ([]*index.SearchResult, error) {
    return codex.Search(ctx, q, amoxtli.WithSearchMaxResults(k))
})
report, _ := eval.Evaluate(ctx, ds, retriever, 1, 3, 5, 10)
fmt.Println(report)                 // global
for lang, sub := range report.ByLang() {
    fmt.Printf("[%s]\n%s", lang, sub)  // par langue
}
```

Les métriques (`RecallAtK`, `ReciprocalRank`, `NDCGAtK`) sont aussi exposées
unitairement pour des usages sur mesure.
