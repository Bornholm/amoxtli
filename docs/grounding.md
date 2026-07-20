# Grounding (récupération vérifiée)

Porté des mécanismes dérivés de MothRAG du projet [Corpus](https://github.com/Bornholm/corpus), le grounding est **découplé de la recherche** : `Search` reste une récupération pure, et deux opérations explicites viennent par-dessus, **sans génération de réponse**.

- **`CheckGrounding(query, results)`** — étape autonome : un LLM juge si des résultats déjà obtenus par `Search` permettent une réponse fiable, et retourne un verdict (`valid` / `partial` / `invalid` + score + explication). L'appelant décide de s'appuyer dessus ou de s'abstenir.
- **`SearchIterative(query, …)`** — orchestration explicite : décomposition de requête optionnelle et, quand le grounding le juge insuffisant, re-retrieval itératif (reformulation + relance). Retourne l'évidence fusionnée **et** le verdict final (`retrieval.Result`).

## Mécanismes

Trois mécanismes, activables indépendamment (client LLM requis) :

- **Vérification (γ)** — `WithGroundingCheck()` : rend `CheckGrounding` disponible et pilote la boucle de `SearchIterative`.
- **Re-retrieval itératif** — `WithIterativeRetrieval(rounds)` : dans `SearchIterative`, tant que le verdict n'est pas confiant, la requête est reformulée et relancée (nécessite `WithGroundingCheck`).
- **Décomposition de requête** — `WithQueryDecomposition(maxSubQueries)` : dans `SearchIterative`, une question complexe est scindée en sous-questions, chacune recherchée et leurs évidences fusionnées.

## Évaluateur d'évidence fusionné

Le filtrage de pertinence (auparavant le `Judge` du pipeline) et la vérification de grounding lisent tous deux la même évidence via le LLM. Pour éviter cet appel redondant, `WithGroundingCheck()` active un **évaluateur d'évidence unique** (`retrieval.EvidenceEvaluator`) qui, en **un seul appel LLM**, retourne à la fois les documents pertinents à conserver **et** le verdict de suffisance (calculé sur les documents retenus).

Quand le grounding est activé, cet évaluateur **remplace le `Judge`** dans le pipeline : `Search` s'en sert pour appliquer le signal de pertinence, `CheckGrounding` pour le verdict, et `SearchIterative` pour les deux. Conséquences :

- `Search` fait un seul appel d'évaluation par recherche (comme le `Judge`), mais applique le résultat selon le **mode** ci-dessous (par défaut : ré-ordonnancement, pas suppression).
- `SearchIterative` économise un appel LLM par round (le grounding n'est plus une passe séparée).
- En mode décomposé, l'évaluation devient **globale** (une passe sur l'évidence fusionnée) au lieu de par-sous-question — neutre-à-positif (plus de contexte, déduplication naturelle), et le verdict porte sur l'évidence déjà traitée (plus cohérent).

Quand le grounding est **désactivé**, le `Judge` reste dans le pipeline : comportement inchangé.

## Mode d'application : `demote` (défaut) / `filter`

`WithGroundingMode` (CLI : `retrieval.grounding_mode`) décide de ce que le signal de pertinence fait à l'évidence :

- **`GroundingDemote` (défaut)** — garde **tous** les documents récupérés mais relègue les non pertinents en fin de liste (les pertinents remontent). Préserve le rappel@k **et** améliore le classement.
- **`GroundingFilter`** — **supprime** les documents jugés non pertinents. Précision de liste maximale mais rappel tronqué : adapté au RAG qui n'alimente le générateur qu'avec quelques passages très pertinents.

Le défaut est `demote` sur la foi d'une évaluation SciFact (corpus complet 5183 documents, 300 requêtes, embeddings bge-m3 + chat mistral-small-24b) :

| Configuration | Recall@10 | nDCG@10 | MRR | P@10 |
|---|---|---|---|---|
| HyDE seul (pas de grounding) | 0,871 | 0,723 | 0,684 | 0,097 |
| HyDE + grounding **demote** | 0,867 | **0,752** | **0,723** | 0,097 |
| HyDE + grounding **filter** | 0,649 | 0,618 | 0,612 | **0,549** |

`demote` est le meilleur profil sur le classement (nDCG@10, MRR) sans sacrifier le rappel — il exploite le verdict pour trier, pas pour jeter. `filter` échange l'essentiel du rappel contre une précision de liste élevée : à ne choisir que pour un usage RAG à listes courtes.

> ⚠️ **Le grounding dépend de la qualité du modèle chat.** Avec un modèle faible, `filter` peut **effondrer le rappel** (l'évaluateur juge à tort la quasi-totalité de l'évidence non pertinente ; sur un run CPU avec un modèle 3B, Recall@10 est tombé à 0,05). Le fail-open ne protège pas de ce cas — l'évaluateur ne renvoie pas d'erreur, il sur-filtre — d'où l'intérêt de `demote` par défaut. Réservez `filter` (et le grounding en général) à un modèle chat capable, ou pointez l'étage grounding sur un bon modèle via `WithStageLLMClient`.

## Exemple

```go
codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(/* ... */),
    amoxtli.WithLLMClient(llmClient),
    amoxtli.WithGroundingCheck(),
    amoxtli.WithGroundingMinScore(0.4),
    amoxtli.WithIterativeRetrieval(1),
    amoxtli.WithQueryDecomposition(3),
)

// Recherche pure, puis grounding comme étape séparée.
results, _ := codex.Search(ctx, "…", amoxtli.WithSearchMaxResults(5))
verdict, _ := codex.CheckGrounding(ctx, "…", results)
if verdict.Status != retrieval.GroundingValid {
    // s'abstenir plutôt que de s'appuyer sur une évidence insuffisante
}

// Ou orchestration explicite (décomposition + re-retrieval piloté par grounding).
res, _ := codex.SearchIterative(ctx, "…", amoxtli.WithSearchMaxResults(5))
_ = res.Results  // évidence fusionnée
_ = res.Grounding // verdict final
```

Sans aucune option d'orchestration, `SearchIterative` équivaut à `Search`. Les composants (`EvidenceEvaluator`, `QueryReformulator`, `QueryDecomposer`) sont des interfaces : toute implémentation personnalisée peut être branchée via `retrieval.NewOrchestrator`.
