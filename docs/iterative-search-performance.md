# Synthèse — Recherche itérative (HyDE + grounding) sur SciFact

Ce document synthétise l'évaluation de la **recherche itérative** d'amoxtli
(`Codex.SearchIterative`) — la couche « agentique » — sur le benchmark
**SciFact**, et la compare à la **recherche simple** hybride mesurée dans
[simple-search-performance.md](simple-search-performance.md).

> **Résultat principal** : sur les métriques de récupération (Recall@k /
> nDCG@k), le mode itératif **est en retrait** de la recherche simple, surtout
> sur le **rappel**. Ce n'est pas un bug : le pipeline itératif optimise un
> objectif différent (évidence fondée pour une réponse générée), que ces
> métriques ne mesurent pas.

## 1. Ce qu'est la recherche itérative ici

`SearchIterative` passe par l'**orchestrateur**, qui empile trois mécanismes
au-dessus de la fusion hybride :

1. **HyDE** — le LLM rédige un document hypothétique à partir de la requête ;
   c'est ce texte qui est embarqué pour la recherche vectorielle (comble l'écart
   de vocabulaire).
2. **Grounding / évaluateur d'évidence** — à chaque tour, le LLM **filtre** les
   résultats pour ne garder que l'évidence jugée pertinente, puis rend un
   **verdict de fondation** (score de confiance).
3. **Re-récupération gardée** — si le verdict n'est pas « confiant »
   (`score >= grounding_min_score`), le LLM **reformule** la requête et un
   nouveau tour de récupération est fusionné (jusqu'à `rounds` tours).

### Configuration évaluée

| Paramètre | Valeur |
|---|---|
| Corpus / requêtes | sous-échantillon 1000 docs / **50 requêtes** SciFact |
| Embeddings | `mistral-embed` (1024 dim, distant, contexte 8k — requis pour les textes HyDE longs) |
| LLM (HyDE, grounding, reformulation) | `mistral-small` (distant) |
| Fusion | RRF bleve 0.50 / vecteur 0.50 |
| Tours de re-récupération | 2 |
| `grounding_min_score` | 0.4 (défaut) et 0.1 (variante) |

## 2. Résultats (mêmes 50 requêtes, même index)

| Métrique | Simple hybride | Itératif (seuil 0.4) | Itératif (seuil 0.1) |
|---|---|---|---|
| MRR | **0.874** | 0.820 | 0.810 |
| Recall@1 | 0.780 | 0.765 | **0.787** |
| nDCG@1 | 0.800 | 0.800 | 0.800 |
| Recall@3 | **0.940** | 0.825 | 0.807 |
| nDCG@3 | **0.887** | 0.824 | 0.802 |
| Recall@5 | **0.960** | 0.830 | 0.820 |
| nDCG@5 | **0.895** | 0.824 | 0.810 |
| Recall@10 | **0.980** | 0.830 | 0.820 |
| nDCG@10 | **0.901** | 0.824 | 0.810 |

## 3. Analyse

### L'itératif effondre le rappel

Recall@10 chute de **0.980 → 0.83** (−15 pts). La signature est parlante : en
itératif, **le rappel est quasi plat** (Recall@1 0.765 ≈ Recall@10 0.830). Le
pipeline **renvoie très peu de documents par requête**.

### Cause : le filtre de pertinence, pas le seuil

L'orchestrateur applique `FilterRelevant` à **chaque tour**
([`retrieval/orchestrator.go`](../retrieval/orchestrator.go)) : il ne conserve
que les résultats que l'évaluateur LLM marque « pertinents ». Sur SciFact,
`mistral-small` **sur-filtre** — il jette de vrais positifs. C'est là que le
rappel se perd.

### Le `grounding_min_score` ne corrige pas ce point — confirmé

Abaisser le seuil de **0.4 à 0.1** ne restaure **pas** le rappel : Recall@10
passe même de 0.830 à 0.820. C'est cohérent avec le code — le seuil ne pilote
**que** la décision de relancer un tour de re-récupération, **pas** le filtre de
pertinence. Un seuil plus bas rend le verdict « confiant » plus tôt, donc
**stoppe** la re-récupération plus vite (moins d'évidence fusionnée). Pour
*ajouter* du rappel, il faudrait au contraire **plus** de re-récupération — mais
elle reste bornée par le filtre qui élague à chaque tour.

### Un désaccord d'objectif, pas un échec

Recall@k / nDCG@k récompensent le fait de **ramener un maximum de documents
pertinents**. L'itératif optimise l'inverse : un **petit ensemble d'évidence
vérifiée et de haute confiance**, destiné à **fonder une réponse générée** (RAG
avec garantie de grounding, filtrage du hors-sujet). Le benchmark le pénalise
donc en partie **injustement** : il ne mesure pas la qualité du grounding ni la
fidélité de la réponse.

## 4. Conclusions

- **Pour la récupération pure** (trouver les bons passages) → la **recherche
  simple hybride gagne largement** ; l'itératif est contre-productif.
- **Pour une réponse générée fondée** (éviter d'injecter du hors-sujet, garantir
  que la réponse s'appuie sur de l'évidence vérifiée) → l'itératif a une valeur
  **non capturée par Recall@k**. Il faut l'évaluer avec des métriques de type
  *faithfulness* / answer-grounding.
- **La précision de tête est préservée** (nDCG@1 = 0.800 dans les trois
  configurations) : l'itératif ne dégrade pas le premier résultat, il **coupe la
  queue**.

### Pistes pour rendre l'itératif compétitif en rappel

1. **Juge de grounding plus fort** (ou cross-encoder dédié) : sur-filtre moins.
2. **Rétrograder au lieu d'éliminer** : classer les résultats jugés peu
   pertinents en fin de liste plutôt que les retirer (préserverait le rappel@k).
3. **Plus de tours de re-récupération** (seuil *plus haut*, pas plus bas) pour
   réinjecter de l'évidence — dans la limite du filtre.

## 5. Limites

- **50 requêtes** seulement (mode itératif coûteux : plusieurs appels LLM par
  requête) — signal indicatif, pas définitif.
- `mistral-small` comme juge de grounding ; sa tendance à sur-filtrer domine les
  résultats. Un modèle plus capable changerait probablement les conclusions.
- Sous-échantillon 1000 docs ; valeurs absolues optimistes, écarts relatifs
  robustes.
- Métriques IR uniquement : elles ne mesurent pas ce pour quoi l'itératif est
  conçu (grounding de la réponse).
