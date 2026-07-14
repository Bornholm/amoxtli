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
| Lexical (BM25) | 1000 docs, 300 req. | 0.797 | 0.894 | 0.774 |
| Hybride (bge-m3) | 1000 docs, 300 req. | 0.826 | 0.919 | 0.800 |
| **Hybride (mistral-embed)** | 1000 docs, 300 req. | **0.875** | **0.975** | **0.848** |
| Hybride + reranking (mxbai + mistral-small) | 1000 docs | 0.819 | 0.883 | 0.806 |
| Itératif HyDE + grounding **filter** (bge-m3) | 1000 docs, 50 req. | 0.750 | 0.760 | 0.747 |
| **Itératif HyDE + grounding demote** (bge-m3) | 1000 docs, 50 req. | **0.883** | **0.920** | **0.874** |

> Les lignes lexical / bge-m3 / mistral-embed ci-dessus sont mesurées sur le
> **même sous-échantillon déterministe** (1000 docs, 300 requêtes) et sont donc
> directement comparables entre elles. Voir le comparatif embedders détaillé plus
> bas.

Trois enseignements transverses :

1. **L'embedder est le levier n°1.** À configuration identique, `mistral-embed`
   gagne **+4,9 pts de nDCG@10** et **+5,6 pts de Recall@10** sur `bge-m3` — plus
   que n'importe quel autre étage. Le premier réflexe d'optimisation doit porter
   sur le modèle d'embeddings.
2. **Le reranking n'aide que s'il dépasse le premier étage.** Deux conditions le
   rendent utile : un index **grand** (sur le corpus complet il rapporte +4,2 pts
   de nDCG@10 avec bge-m3, contre ~0 à 1000 docs) **et** un premier étage
   **améliorable**. Sur un embedder faible (bge-m3) le reranker `mistral-small`
   rattrape la précision ; sur un embedder déjà fort (mistral-embed, nDCG@10 0.763)
   il n'ajoute plus que +0,3 pt et **retire du rappel**. Un reranker généraliste
   plafonne : il tire vers le haut un classement faible, pas un classement déjà bon.
3. **L'itératif dépend de son mode de grounding.** En **demote** (reclasse au lieu
   de supprimer), il **domine toutes les métriques** — rappel, nDCG et MRR au-dessus
   même de l'hybride, grâce à HyDE + re-récupération qui ajoutent du rappel et au
   grounding qui classe mieux. En **filter** (supprime l'évidence non pertinente),
   il vise l'inverse : un jeu minuscule et **très pur** (précision ≈ 0.72) pour
   fonder une réponse, au prix du rappel. Les deux coûtent plusieurs appels LLM par
   requête.

### Calibrage sur corpus complet (comparable au SoTA)

Mesuré sur le **corpus SciFact complet** (5183 docs, 300 requêtes) — donc
directement comparable aux classements BEIR publiés. Les deux embedders sont
servis via OpenRouter ; le cache d'embeddings a rendu l'opération faisable
(~5500 appels par embedder).

| Configuration | nDCG@10 | Recall@10 | MRR |
|---|---|---|---|
| Lexical (BM25) | 0.679 | 0.801 | 0.650 |
| Hybride (bge-m3) | 0.705 | 0.846 | 0.669 |
| Hybride (bge-m3) + reranking | 0.747 | 0.828 | 0.729 |
| **Hybride (mistral-embed)** | 0.763 | **0.923** | 0.721 |
| **Hybride (mistral-embed) + reranking** | **0.766** | 0.886 | **0.737** |

Positionnement — **amoxtli est au niveau SoTA** :

- Le lexical (0.679) est **aligné sur la baseline BM25 publiée** (~0.665) : socle
  correctement calibré.
- L'**hybride mistral-embed atteint nDCG@10 = 0.763 sans reranking**, dans la
  **fourchette du SoTA SciFact publié (~0.75-0.77)**, avec un **Recall@10 de 0.923**
  particulièrement élevé. Le reranking le porte à **0.766** (au sommet de la
  fourchette), mais marginalement et **au prix du rappel** — voir ci-dessous.
- L'embedder reste le levier décisif : mistral-embed gagne **+5,8 pts de nDCG@10**
  et **+7,7 pts de Recall@10** sur bge-m3, à configuration identique.

**Le reranking n'aide que lorsqu'il dépasse le classement de premier étage.** Sur
un premier étage faible (bge-m3, nDCG@10 0.705) il rapporte +4,2 pts ; sur un
premier étage fort (mistral-embed, 0.763) le reranker généraliste `mistral-small`
n'ajoute que +0,3 pt et **retire du rappel** (0.923 → 0.886). Autrement dit,
`mistral-small` en reranker plafonne autour de 0.76-0.77 : il tire un embedder
faible vers le haut mais ne fait plus progresser un embedder déjà au niveau. Pour
dépasser ce plafond il faudrait un **reranker cross-encoder dédié**.

### Comparatif embedders (à isopérimètre)

Deux embedders, **même index, même sous-échantillon déterministe** (SciFact 1000
docs / 300 requêtes), fusion hybride RRF 0.50/0.50, sans reranking. C'est la
comparaison à privilégier : tout est identique sauf le modèle d'embeddings.

| Embedder | Dim | nDCG@10 | Recall@10 | MRR | Fourniture | Débit observé |
|---|---|---|---|---|---|---|
| `bge-m3` | 1024 | 0.826 | 0.919 | 0.800 | OpenRouter (~0,01 $/M tok) ; **poids ouverts, auto-hébergeable** | rapide, aucun 429 |
| **`mistral-embed`** | 1024 | **0.875** | **0.975** | **0.848** | API (ici Cadoles Xolo) | **fortement rate-limité** (mur de 429) |

Lecture :

- **Qualité** : `mistral-embed` domine nettement (+4,9 pts nDCG@10, +5,6 pts
  Recall@10). C'est le meilleur choix de récupération pure mesuré à ce jour.
- **Exploitabilité** : sur notre infrastructure, `mistral-embed` est **difficile
  à passer à l'échelle** (le débit du fournisseur oblige à throttler à ~2 req/s
  et à s'appuyer sur le cache d'embeddings pour terminer). `bge-m3` via OpenRouter
  n'a subi **aucun 429**, coûte une fraction de centime pour le corpus, et — poids
  ouverts — peut être **auto-hébergé** (aucune donnée envoyée à un tiers).
- **Recommandation par défaut** : viser `mistral-embed` **quand la qualité prime
  et que le débit suit** ; sinon `bge-m3` est un excellent repli (−4,9 pts
  nDCG@10) sans contrainte de débit et auto-hébergeable. Dans les deux cas, activer
  le **cache d'embeddings** (`AMOXTLI_EVAL_EMBED_CACHE_DIR` côté éval, décorateur
  `llmx.CachingClient` côté applicatif) : il rend les ré-ingestions gratuites et
  permet de terminer un embedding throttlé en plusieurs passes.

> Méthodologie : le sous-échantillon est désormais **reproductible** (les requêtes
> BEIR sont triées par identifiant avant échantillonnage), condition nécessaire
> pour que deux runs d'embedders portent sur exactement le même jeu et que le
> cache converge. Reste à confirmer la hiérarchie sur **corpus complet** (prochain
> incrément).

## 3. Les leviers de configuration

| Levier | Ce qu'il achète | Ce qu'il coûte | Option |
|---|---|---|---|
| Index vectoriel (hybride) | Rappel, robustesse à l'écart de vocabulaire | Un service d'embeddings ; ingestion plus lourde | `WithIndexers(... vector ...)` |
| Embedder plus fort / grand contexte | Le plus gros gain de qualité | Coût/latence du modèle | choix du modèle d'embeddings |
| Reranking | Précision de tête (MRR, nDCG@1) | 1 appel LLM/requête ; perte de rappel profond | `WithReranking()` + `WithLLMClient` |
| HyDE | Rappel sur requêtes courtes / vocabulaire éloigné | 1 appel LLM/requête ; embedder à grand contexte requis | LLM + ne pas `WithDisableHyDE()` |
| Judge | Précision (retire le hors-sujet) | Appels LLM ; risque de sur-filtrer | ne pas `WithDisableJudge()` |
| Grounding + itératif (**demote**) | Meilleure récupération pure (rappel + classement) | Plusieurs appels LLM/requête | `WithGroundingCheck()`, `WithGroundingMode(GroundingDemote)`, `WithIterativeRetrieval(n)` |
| Grounding + itératif (**filter**) | Évidence minimale et très pure pour un générateur | Plusieurs appels LLM/requête ; coupe le rappel | `WithGroundingCheck()`, `WithIterativeRetrieval(n)` |

## 4. Scénarios d'usage → configuration

### A. Assistant / chatbot RAG (réponse à partir de 1 à 5 passages)
Ce qui compte : que les **tout premiers** résultats soient les bons.
→ **Hybride + reranking.** Meilleurs MRR / nDCG@1..5. Le reranking concentre la
pertinence en tête ; la perte de rappel profond est sans importance puisqu'on
n'exploite que le haut de liste.

### B. Exploration / revue documentaire (ne rien manquer)
Ce qui compte : **maximiser le rappel** dans le top-10.
→ **Hybride sans reranking**, avec le **meilleur embedder disponible**. C'est la
configuration au plus haut Recall@10 (0.975 avec `mistral-embed`). Le reranking
serait ici contre-productif.

### C. Recherche exacte, hors-ligne, ou sensible
Mots-clés / identifiants / codes, ou contrainte de coût, latence,
confidentialité (aucune donnée envoyée à un service tiers).
→ **Lexical seul.** Aucun modèle requis, résultats corrects, et les mots-clés
exacts sont son point fort. Sert aussi de repli fiable.

### D. Réponse générée **fondée**, anti-hors-sujet
Ce qui compte : que la réponse s'appuie sur de l'**évidence vérifiée**, sans se
laisser polluer par des passages hors-sujet.
→ **Itératif en mode filter.** Il renvoie un ensemble **minimal et très pur**
(précision ≈ 0.72), idéal pour alimenter un générateur sans hors-sujet. Le rappel
n'est pas l'objectif ici.

### D′. Meilleure récupération pure possible (qualité maximale, coût accepté)
Ce qui compte : trouver **et** bien classer un maximum de passages pertinents,
sans contrainte de coût par requête.
→ **Itératif en mode demote.** Il **domine toutes les métriques** (rappel, nDCG,
MRR au-dessus de l'hybride simple), en gardant toute l'évidence et en classant les
passages fondés en tête. Compter plusieurs appels LLM par requête. Voir
[iterative-search-performance.md](iterative-search-performance.md).

### E. Budget coût / latence serré, mais besoin sémantique
→ **Hybride sans étage LLM** (pas de rerank, pas de HyDE, pas de grounding). On
garde le gain de rappel du vecteur sans le coût des appels LLM par requête.

## 5. Guide de décision rapide

| Votre priorité | Configuration |
|---|---|
| Le 1ᵉʳ résultat doit être le bon (RAG) | Hybride + **reranking** |
| Ne manquer aucun document pertinent | **Hybride** (meilleur embedder) |
| Aucun service d'IA / confidentialité / mots-clés exacts | **Lexical** |
| Réponse générée fondée, évidence pure | **Itératif filter** (HyDE + grounding) |
| Qualité de récupération maximale, coût accepté | **Itératif demote** |
| Sémantique à coût/latence minimal | Hybride **sans étage LLM** |

**Règle générale** : partez de l'**hybride avec un bon embedder** (le meilleur
rapport gain/effort), puis ajoutez **le reranking si vous n'exploitez que la
tête**, **l'itératif demote si vous visez la qualité de récupération maximale**,
ou **l'itératif filter si vous générez une réponse à fonder**. N'ajoutez un étage
LLM que si votre objectif le justifie — chacun coûte des appels par requête.

## 6. Limites

- Un seul jeu (**SciFact**, anglais, scientifique). Les jeux à faible écart de
  vocabulaire (type SQuAD) favorisent davantage le lexical ; l'apport sémantique
  n'y est pas aussi marqué.
- Hybride et hybride+rerank sont mesurés sur **corpus complet** (5183 docs) avec
  `bge-m3` **et** `mistral-embed` (tous deux via OpenRouter) : le positionnement
  SoTA est établi, plus seulement extrapolé du sous-échantillon.
- Modèles utilisés (`bge-m3`, `mistral-embed`, `mistral-small`) : ni
  l'embedder ni le reranker ne sont les plus forts du marché ; des modèles
  dédiés (embedder SoTA, reranker cross-encoder) amélioreraient encore les
  résultats.
- L'A/B itératif (filter/demote) porte sur **50 requêtes** ; le mode filter reste
  évalué en proxy (précision d'évidence), pas en *faithfulness* de la réponse
  générée (amoxtli ne génère pas de réponse).
