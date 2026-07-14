# Synthèse générale — Performances et configurations d'amoxtli

Ce document donne une vue d'ensemble des **performances** d'amoxtli en
récupération, et propose une correspondance **scénario d'usage → configuration**.
Il s'appuie sur deux études détaillées :

- [simple-search-performance.md](simple-search-performance.md) — lexical /
  hybride / hybride + reranking (`Codex.Search`).
- [iterative-search-performance.md](iterative-search-performance.md) — le mode
  agentique HyDE + grounding (`Codex.SearchIterative`).

> Mesures réalisées sur **SciFact** (BEIR), un jeu à fort écart de vocabulaire
> requête↔document. Sauf mention contraire, sous-échantillon de 1000 documents.
> Les valeurs absolues sont optimistes (moins de distracteurs) ; **les écarts
> relatifs entre configurations sont robustes**.

## 1. La librairie en une phrase

amoxtli est une **récupération composable** : on empile des étages
indépendants — index lexical, index vectoriel, fusion, reranking, HyDE,
grounding, re-récupération itérative — et **chaque étage est un compromis**
précision ↔ rappel ↔ coût. Il n'y a pas de « meilleure » configuration dans
l'absolu : il y a la configuration adaptée à **votre** objectif.

## 2. Panorama des performances

Récupération pure sur SciFact (nDCG@10 = qualité du classement, Recall@10 =
capacité à ne pas manquer les bons documents dans le top-10).

| Configuration | Conditions | nDCG@10 | Recall@10 | MRR |
|---|---|---|---|---|
| Lexical (BM25) | corpus complet 5183 docs | 0.679 | 0.801 | 0.650 |
| Lexical (BM25) | 1000 docs | 0.797 | 0.894 | 0.774 |
| Hybride (mxbai-embed) | 1000 docs | 0.817 | 0.958 | 0.776 |
| **Hybride (mistral-embed)** | 1000 docs | **0.874** | **0.973** | 0.847 |
| Hybride + reranking (mxbai + mistral-small) | 1000 docs | 0.819 | 0.883 | **0.806** |
| Itératif HyDE + grounding (mistral) | 1000 docs, 50 requêtes | 0.824 | 0.830 | 0.820 |

Trois enseignements transverses :

1. **L'embedder est le levier n°1.** Passer de `mxbai-embed-large` à
   `mistral-embed` gagne ~+6 pts de nDCG@10 — plus que n'importe quel autre
   étage. Le premier réflexe d'optimisation doit porter sur le modèle
   d'embeddings.
2. **Le reranking déplace le compromis vers la tête.** Il améliore MRR /
   précision top-1 mais **réduit le rappel profond** (il ne renvoie qu'un top-k
   réordonné).
3. **L'itératif optimise un autre objectif que le rappel.** Le grounding
   **filtre** l'évidence : excellent pour fonder une réponse, mais il coupe la
   queue de résultats — d'où un Recall@10 en fort retrait sur ces métriques IR.

### Calibrage

Sur le corpus **complet**, le lexical obtient nDCG@10 = 0.679, aligné sur la
baseline BM25 publiée (~0.665) : le socle est **correctement calibré**. Les
configurations hybride/rerank n'ont pas encore été mesurées sur corpus complet
(contrainte de débit du fournisseur d'embeddings distant) ; les chiffres à 1000
docs ne sont donc **pas** directement comparables aux classements publiés.

## 3. Les leviers de configuration

| Levier | Ce qu'il achète | Ce qu'il coûte | Option |
|---|---|---|---|
| Index vectoriel (hybride) | Rappel, robustesse à l'écart de vocabulaire | Un service d'embeddings ; ingestion plus lourde | `WithIndexers(... vector ...)` |
| Embedder plus fort / grand contexte | Le plus gros gain de qualité | Coût/latence du modèle | choix du modèle d'embeddings |
| Reranking | Précision de tête (MRR, nDCG@1) | 1 appel LLM/requête ; perte de rappel profond | `WithReranking()` + `WithLLMClient` |
| HyDE | Rappel sur requêtes courtes / vocabulaire éloigné | 1 appel LLM/requête ; embedder à grand contexte requis | LLM + ne pas `WithDisableHyDE()` |
| Judge | Précision (retire le hors-sujet) | Appels LLM ; risque de sur-filtrer | ne pas `WithDisableJudge()` |
| Grounding + itératif | Évidence fondée, vérifiée ; réponse ancrée | Plusieurs appels LLM/requête ; effondre le rappel@k | `WithGroundingCheck()`, `WithIterativeRetrieval(n)` |

## 4. Scénarios d'usage → configuration

### A. Assistant / chatbot RAG (réponse à partir de 1 à 5 passages)
Ce qui compte : que les **tout premiers** résultats soient les bons.
→ **Hybride + reranking.** Meilleurs MRR / nDCG@1..5. Le reranking concentre la
pertinence en tête ; la perte de rappel profond est sans importance puisqu'on
n'exploite que le haut de liste.

### B. Exploration / revue documentaire (ne rien manquer)
Ce qui compte : **maximiser le rappel** dans le top-10.
→ **Hybride sans reranking**, avec le **meilleur embedder disponible**. C'est la
configuration au plus haut Recall@10 (0.973 avec `mistral-embed`). Le reranking
serait ici contre-productif.

### C. Recherche exacte, hors-ligne, ou sensible
Mots-clés / identifiants / codes, ou contrainte de coût, latence,
confidentialité (aucune donnée envoyée à un service tiers).
→ **Lexical seul.** Aucun modèle requis, résultats corrects, et les mots-clés
exacts sont son point fort. Sert aussi de repli fiable.

### D. Réponse générée **fondée**, anti-hors-sujet
Ce qui compte : que la réponse s'appuie sur de l'**évidence vérifiée**, sans se
laisser polluer par des passages hors-sujet.
→ **Itératif (HyDE + grounding).** Attention : à évaluer avec des métriques de
*faithfulness* / answer-grounding, **pas** avec Recall@k (qui le pénalise
injustement — il filtre volontairement). Pour préserver un peu de rappel, viser
un **juge de grounding fort** et éventuellement rétrograder plutôt qu'éliminer.

### E. Budget coût / latence serré, mais besoin sémantique
→ **Hybride sans étage LLM** (pas de rerank, pas de HyDE, pas de grounding). On
garde le gain de rappel du vecteur sans le coût des appels LLM par requête.

## 5. Guide de décision rapide

| Votre priorité | Configuration |
|---|---|
| Le 1ᵉʳ résultat doit être le bon (RAG) | Hybride + **reranking** |
| Ne manquer aucun document pertinent | **Hybride** (meilleur embedder) |
| Aucun service d'IA / confidentialité / mots-clés exacts | **Lexical** |
| Réponse générée fondée et vérifiée | **Itératif** (HyDE + grounding) |
| Sémantique à coût/latence minimal | Hybride **sans étage LLM** |

**Règle générale** : partez de l'**hybride avec un bon embedder** (le meilleur
rapport gain/effort), puis ajoutez **le reranking si vous n'exploitez que la
tête**, ou **le grounding si vous générez une réponse à fonder**. N'ajoutez un
étage LLM que si votre objectif le justifie — chacun coûte un appel par requête.

## 6. Limites

- Un seul jeu (**SciFact**, anglais, scientifique). Les jeux à faible écart de
  vocabulaire (type SQuAD) favorisent davantage le lexical ; l'apport sémantique
  n'y est pas aussi marqué.
- Mesures principalement sur **sous-échantillon** (1000 docs) ; hybride/rerank
  jamais mesurés sur corpus complet.
- Modèles utilisés (`mxbai-embed-large`, `mistral-embed`, `mistral-small`) : ni
  l'embedder ni le reranker ne sont les plus forts du marché ; des modèles
  dédiés (embedder SoTA, reranker cross-encoder) amélioreraient encore les
  résultats.
- L'itératif n'est mesuré qu'en métriques IR, qui ne capturent pas son objectif
  réel (grounding de la réponse).
