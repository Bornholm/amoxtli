# Synthèse — Recherche itérative (HyDE + grounding) sur SciFact

Ce document synthétise l'évaluation de la **recherche itérative** d'amoxtli
(`Codex.SearchIterative`) — la couche « agentique » — sur le benchmark
**SciFact**, et compare ses deux modes de grounding (**filter** et **demote**) à
la **recherche simple** hybride.

> **Résultat principal** : le comportement de l'itératif dépend entièrement du
> **mode de grounding**.
> - En mode **filter** (défaut historique), il **sacrifie le rappel** pour
>   produire un petit ensemble d'évidence **très pur** (précision ≈ 0.72).
> - En mode **demote** (nouveau), il **domine toutes les métriques de classement
>   et de rappel** — au-dessus même de la recherche simple — tout en préservant
>   l'objectif de grounding.
>
> Autrement dit : le « −15 pts de rappel » constaté auparavant n'était pas une
> fatalité de l'itératif, mais un **artefact du filtrage**. Le mode demote le
> corrige.

## 1. Ce qu'est la recherche itérative ici

`SearchIterative` passe par l'**orchestrateur**, qui empile trois mécanismes
au-dessus de la fusion hybride :

1. **HyDE** — le LLM rédige un document hypothétique à partir de la requête ;
   c'est ce texte qui est embarqué pour la recherche vectorielle (comble l'écart
   de vocabulaire).
2. **Grounding / évaluateur d'évidence** — à chaque tour, le LLM juge quels
   résultats sont pertinents, puis rend un **verdict de fondation** (score de
   confiance). Ce que l'on **fait** du signal de pertinence dépend du mode :
   - **filter** (`GroundingFilter`, défaut) — les résultats jugés non pertinents
     sont **retirés** ;
   - **demote** (`GroundingDemote`) — ils sont **conservés mais reclassés en fin
     de liste** (les pertinents passent en tête, rien n'est perdu).
3. **Re-récupération gardée** — si le verdict n'est pas « confiant »
   (`score >= grounding_min_score`), le LLM **reformule** la requête et un
   nouveau tour de récupération est fusionné (jusqu'à `rounds` tours).

> Précision d'API : `grounding_min_score` ne pilote **que** la décision de
> relancer un tour de re-récupération ; il n'agit **pas** sur le filtrage/
> reclassement de l'évidence (c'est `WithGroundingMode`).

### Configuration évaluée

| Paramètre | Valeur |
|---|---|
| Corpus / requêtes | sous-échantillon **1000 docs / 50 requêtes** SciFact (déterministe) |
| Embeddings | `bge-m3` (1024 dim, OpenRouter, contexte 8k — requis pour les textes HyDE longs, sans throttle) |
| LLM (HyDE, grounding, reformulation) | `mistral-small` (Xolo, throttlé) |
| Fusion | RRF bleve 0.50 / vecteur 0.50 |
| Tours de re-récupération | 2 |

## 2. Résultats A/B (mêmes 50 requêtes, même index, même embedder)

`P@k` = précision @k (part des résultats renvoyés qui sont pertinents) — l'axe
que l'itératif optimise, ajouté au harnais pour cette étude.

| Métrique | Simple hybride | Itératif **filter** | Itératif **demote** |
|---|---|---|---|
| MRR | 0.763 | 0.747 | **0.874** |
| Recall@1 | 0.687 | 0.697 | **0.797** |
| nDCG@1 | 0.700 | 0.720 | **0.820** |
| Recall@3 | 0.803 | 0.760 | **0.910** |
| nDCG@3 | 0.765 | 0.750 | **0.878** |
| Recall@5 | 0.840 | 0.760 | **0.920** |
| nDCG@5 | 0.781 | 0.750 | **0.883** |
| Recall@10 | 0.860 | 0.760 | **0.920** |
| nDCG@10 | 0.788 | 0.750 | **0.883** |
| P@3 | 0.287 | **0.717** | 0.327 |
| P@5 | 0.184 | **0.715** | 0.200 |
| P@10 | 0.094 | **0.715** | 0.100 |

## 3. Analyse

### Le mode demote domine sur le rappel ET le classement

Sur **toutes** les métriques de rappel et de classement, `demote` **bat la
recherche simple** : Recall@10 0.920 (vs 0.860), nDCG@10 0.883 (vs 0.788), MRR
0.874 (vs 0.763). Deux mécanismes se combinent :

- **HyDE + re-récupération apportent du rappel** : les tours supplémentaires
  ramènent de vrais positifs que la fusion d'un seul passage manquait. Comme
  demote ne jette rien, ce rappel est **conservé**.
- **Le signal de grounding classe mieux** : les documents jugés pertinents
  passent en tête, ce qui améliore nDCG / MRR par rapport à l'ordre de fusion
  brut.

C'est un renversement : avec demote, la couche agentique n'est plus un compromis
« autre objectif » — elle est **la meilleure configuration de récupération pure**
mesurée à ce jour, pour peu qu'on accepte son coût LLM.

### Le mode filter détient l'axe précision

`filter` fait exactement ce pour quoi il est conçu : un **petit ensemble
d'évidence très pur**. Sa précision est **stable à ≈ 0.72 de P@3 à P@10** (contre
0.09-0.29 pour les autres) — signe qu'il ne renvoie que quelques documents,
presque tous pertinents. En contrepartie il **coupe la queue** : Recall@10 tombe
à 0.760. C'est le bon choix quand on veut **alimenter un générateur** avec une
poignée de passages fiables et éviter d'injecter du hors-sujet, **pas** quand on
veut maximiser le rappel.

### Pourquoi l'ancienne conclusion (« l'itératif dégrade le rappel ») était incomplète

Elle mesurait le **seul** mode filter et l'attribuait à l'itératif en général.
Le vrai coupable était l'élagage de `FilterRelevant` à chaque tour. `demote`
applique le même signal de pertinence **sans rien retirer** : le rappel remonte
au-dessus du baseline, et la précision de tête reste excellente (P@1 = 0.820,
meilleur des trois).

## 4. Conclusions — quel mode pour quel objectif

| Objectif | Mode | Pourquoi |
|---|---|---|
| **Meilleure récupération pure** (trouver + bien classer) | **Itératif demote** | Domine rappel, nDCG et MRR — au prix des appels LLM |
| **Réponse générée fondée**, jeu d'évidence minimal et pur | **Itératif filter** | Précision ≈ 0.72, filtre le hors-sujet |
| **Coût / latence serrés** | **Simple hybride** | Aucun appel LLM par requête ; reste très correct |

- **La précision de tête est préservée dans les deux modes itératifs** (P@1 ≥
  0.72), et **demote la porte à 0.82** : l'agentique ne dégrade jamais le premier
  résultat.
- **Coût** : les deux modes itératifs coûtent **plusieurs appels LLM par requête**
  (HyDE + grounding + reformulation). Sur ces 50 requêtes, chaque run itératif a
  pris ~6 min contre ~3 s pour l'hybride. À réserver aux cas où la qualité (ou le
  grounding) justifie ce coût.

## 5. Confirmation à grande échelle : multi-hop (HotpotQA)

L'A/B SciFact ci-dessus établit que `demote` domine, mais chaque requête SciFact
n'a **qu'une seule preuve** : la re-récupération itérative n'y est pas vraiment
sollicitée. **HotpotQA** est le test décisif — ses questions exigent **deux
documents** (« 2-hop »), exactement ce que la re-récupération gardée est censée
adresser (ramener le 2ᵉ hop qu'un seul passage de fusion manque).

### Configuration

| Paramètre | Valeur |
|---|---|
| Corpus / requêtes | **100 000 docs / 100 requêtes** HotpotQA (streamé, gold-aware) |
| Embeddings / LLM | `bge-m3` / `mistral-small` |
| Fusion / tours | RRF 0.50 / 0.50 · 2 tours · grounding **demote** |

### Récupération

| Config | MRR | Recall@3 | Recall@10 | nDCG@10 |
|---|---|---|---|---|
| Lexical (BM25) | 0.949 | 0.795 | 0.855 | 0.841 |
| Hybride (bge-m3) | 0.957 | 0.820 | 0.930 | 0.889 |
| **Itératif demote** | **0.988** | **0.950** | **0.970** | **0.961** |

### La désaturation révèle le gain

Le même A/B lancé d'abord sur un sous-échantillon de **5 000 docs** montrait toutes
les configs au plafond (nDCG@10 0.93-1.00) — l'écart était masqué. En passant à
**100 000 docs** (20× plus de distracteurs), tout le monde chute, mais **l'écart
hybride → itératif s'élargit** nettement :

| Écart hybride → itératif | 5 000 docs | **100 000 docs** |
|---|---|---|
| nDCG@10 | +1,9 pt | **+7,2 pts** |
| Recall@3 | +4,0 pts | **+13,0 pts** |

Le **+13 pts de Recall@3** est la signature directe de la re-récupération qui
**ramène le 2ᵉ hop dans le top-3**. Conclusion : sur du multi-hop, la valeur de la
couche itérative **croît avec la difficulté du corpus** — c'est là qu'elle paie le
plus, pas sur les jeux mono-preuve.

### Se propage-t-il jusqu'à la réponse ?

Avec le reader optionnel du harnais (`AMOXTLI_EVAL_GENERATE`, cf.
[`evaluation.md`](evaluation.md)) branché sur les 5 premiers passages, réponse
notée EM/F1, pour deux readers (`mistral-small` puis `mistral-medium`) :

| Config | EM (small) | F1 (small) | EM (medium) | F1 (medium) |
|---|---|---|---|---|
| Hybride + reader | 0.510 | 0.665 | 0.540 | 0.718 |
| **Itératif demote + reader** | 0.540 | 0.692 | **0.560** | **0.739** |

Deux enseignements :

1. **Le gain de récupération se propage** à la réponse (itératif > hybride pour les
   deux readers), mais **atténué** : le reader ne voit que le top-5, où l'écart de
   récupération se resserre.
2. **Le reader est le facteur limitant, pas la récupération.** À récupération
   constante (ligne hybride, sans LLM de récup), passer `mistral-small` →
   `mistral-medium` rapporte **+3,0 EM / +5,3 F1** — plus que le gain de
   récupération lui-même. Le goulot du multi-hop end-to-end est le *reader*
   généraliste, pas l'étage de récupération d'amoxtli.

(EM/F1 mélangent récup et lecture ; un reader génératif paraphrase → l'EM est
fragile, le F1 plus juste. Non superposables aux baselines extractives des
leaderboards. Voir les réserves dans [`evaluation.md`](evaluation.md).)

## 6. Activer chaque mode

```go
// Itératif demote — meilleure récupération pure
codex, _ := amoxtli.New(ctx,
    amoxtli.WithStore(store), amoxtli.WithIndexers(indexers...),
    amoxtli.WithLLMClient(llm),
    amoxtli.WithGroundingCheck(),
    amoxtli.WithGroundingMode(retrieval.GroundingDemote),
    amoxtli.WithIterativeRetrieval(2),
)

// Itératif filter (défaut) — évidence minimale et pure pour un générateur
codex, _ := amoxtli.New(ctx,
    amoxtli.WithStore(store), amoxtli.WithIndexers(indexers...),
    amoxtli.WithLLMClient(llm),
    amoxtli.WithGroundingCheck(),                 // filter est le défaut
    amoxtli.WithIterativeRetrieval(2),
)

result, _ := codex.SearchIterative(ctx, query, amoxtli.WithSearchMaxResults(10))
// result.Results : évidence ; result.Grounding : verdict de fondation.
```

## 7. Limites

- L'A/B SciFact porte sur **50 requêtes / 1000 docs** ; le régime multi-hop
  (§5) le confirme à **100 requêtes / 100 000 docs**, mais les deux restent des
  sous-échantillons — valeurs absolues optimistes, **écarts relatifs robustes**.
- Embedder `bge-m3` et juge `mistral-small` : un juge de grounding plus capable
  (ou un cross-encoder dédié) changerait probablement les valeurs absolues,
  vraisemblablement en faveur des deux modes itératifs.
- Sous-échantillon 1000 docs ; valeurs absolues optimistes, **écarts relatifs
  robustes** (comparaison à isopérimètre : seul le mode change entre les runs).
- L'axe « fidélité de la réponse générée » (faithfulness) n'est toujours pas
  mesuré directement : `P@k` en est un proxy côté récupération, pas une mesure de
  la réponse elle-même.
