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

Quand le grounding est activé, cet évaluateur **remplace le `Judge`** dans le pipeline : `Search` s'en sert pour le filtrage de pertinence, `CheckGrounding` pour le verdict, et `SearchIterative` pour les deux. Conséquences :

- `Search` fait le même nombre d'appels qu'avec le `Judge` (filtrage), à granularité identique → **pertinence inchangée**.
- `SearchIterative` économise un appel LLM par round (le grounding n'est plus une passe séparée).
- En mode décomposé, le filtrage devient **global** (une passe sur l'évidence fusionnée) au lieu de par-sous-question — neutre-à-positif (plus de contexte, déduplication naturelle), et le verdict porte sur l'évidence déjà filtrée (plus cohérent).

Quand le grounding est **désactivé**, le `Judge` reste dans le pipeline : comportement inchangé.

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
