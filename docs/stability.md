# Stabilité de l'API

Amoxtli suit le [versionnage sémantique](https://semver.org/lang/fr/). Ce
document décrit ce qui est couvert par cette garantie et la politique de
compatibilité pendant la série `0.x`.

## Série `0.x` (actuelle)

Tant que la version majeure est `0`, l'API n'est **pas figée** : une version
mineure (`0.1 → 0.2`) peut introduire des changements incompatibles. Ils seront
toujours :

- annoncés dans le [CHANGELOG](../CHANGELOG.md) sous une section **Modifié** ou
  **Supprimé** ;
- accompagnés, quand c'est raisonnable, d'une période de dépréciation (l'ancienne
  API reste, marquée `Deprecated:`, pendant au moins une version mineure).

Les versions **correctives** (`0.1.0 → 0.1.1`) ne contiennent que des
corrections rétro-compatibles.

## Surface publique couverte

La garantie de compatibilité, une fois en `1.x`, portera sur :

- le package racine `amoxtli` : `Codex`, `New`, les `Option`, les types d'options
  de recherche et d'ingestion ;
- les contrats inter-couches : `index.Index` (+ `index.SearchResult`,
  `index.Filter`, `index.Semantic`), `ingest.Store`, `task.Runner` /
  `task.PersistentRunner`, `model.*`, `retrieval.EvidenceEvaluator`,
  `backup.Snapshotable` ;
- les constructeurs des backends fournis (`index/bleve`, `index/sqlitevec`,
  `index/postgres`, `ingest/gorm`, `task/memory`, `task/gorm`) ;
- les décorateurs `llmx`, le harnais `eval` et le scope d'instrumentation
  `telemetry`.

## Hors garantie

Ne font **pas** partie de la surface stable et peuvent changer à tout moment :

- tout ce qui est sous `internal/` (non importable) ;
- les détails d'implémentation exposés « pour usage avancé » (`Codex.Manager()`,
  `Codex.Index()`) ;
- le format de sérialisation interne des tâches persistées (`task/gorm`) — la
  reprise est garantie au sein d'une même version, pas entre versions ;
- le format des snapshots `backup` au-delà de la frontière de version encodée
  (`snapshot-v1`) ;
- le comportement exact des composants LLM (prompts, seuils), qui relève de la
  qualité et non du contrat de type.

## Contraintes de dépendances

Le backend `index/sqlitevec` impose deux versions précises
(`ncruces/go-sqlite3` v0.23.0 et `wazero` ≥ v1.9.0) documentées dans le
[README](../README.md). Elles font partie du contrat d'installation de ce
backend et ne changeront pas sans note explicite dans le CHANGELOG.
