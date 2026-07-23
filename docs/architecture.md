# Architecture

| Package | Rôle |
|---|---|
| `amoxtli` (racine) | Façade `Codex` : composition explicite (store + indexeurs), ingestion, recherche, backup |
| `model` | Modèle de domaine : Document, Section, Collection |
| `index` | Contrat `index.Index` + options de recherche |
| `index/bleve` | Backend plein-texte (bleve) |
| `index/sqlitevec` | Backend vectoriel (sqlite-vec + embeddings) : vec0 partitionné par collection, quantization binaire optionnelle |
| `index/postgres` | Backend hybride PostgreSQL (FTS `tsvector` + pgvector, fusion RRF) |
| `index/pipeline` | Index composite : fusion RRF pondérée par index + transformers (HyDE, Judge, dédup) |
| `retrieval` | Orchestration de récupération pilotée par le grounding (γ) : vérification, re-retrieval itératif, décomposition de requête, reranker LLM |
| `llmx` | Décorateurs `llm.Client` : `RetryClient` (retries à backoff + rate-limit optionnel) |
| `index/testsuite` | Suite de conformité pour les implémentations de `index.Index` |
| `markdown` | Parsing/chunking markdown en sections |
| `sourcecode` | Parsing/chunking de code source en sections par déclaration (tree-sitter pur Go, métadonnées `type=code` / `language=<nom>`) |
| `convert` | Conversion de fichiers vers markdown (`pandoc`, `libreoffice`, `genai`) |
| `task` / `task/memory` / `task/gorm` | Exécution de tâches asynchrones : contrat `task.Runner`, runner en mémoire, runner persistant gorm (reprise au démarrage) |
| `ingest` / `ingest/gorm` | Pipeline d'ingestion + magasin de documents (SQLite ou PostgreSQL) |
| `backup` | Abstraction snapshot/restore |
| `eval` / `eval/hfqa` | Harnais d'évaluation de la pertinence (Recall@k / MRR / nDCG@k, segmentation par langue) + loader de jeux QA Hugging Face (format SQuAD) |
| `telemetry` | Intégration OpenTelemetry : tracer/meter partagés + instruments (latence recherche, coût LLM) |

## Indexeurs personnalisés

Tout type implémentant `index.Index` peut être branché dans le pipeline, avec son poids relatif dans la fusion des scores :

```go
codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.4},
        amoxtli.Indexer{ID: "custom", Index: myIndex, Weight: 0.6},
    ),
)
```

La conformité se vérifie avec `index/testsuite.TestIndex`.

### Fusion des résultats et scores

Le pipeline fusionne les listes de résultats des différents indexeurs par **Reciprocal Rank Fusion** (RRF) : chaque indexeur contribue à une source (et à ses sections) une valeur inversement proportionnelle au rang où elle apparaît, pondérée par le poids de l'indexeur. La fusion est donc sensible au rang (un résultat classé 1er pèse plus qu'un 10e) et récompense le consensus entre indexeurs. Les scores fusionnés sont exposés sur `index.SearchResult` (`Score` au niveau source, `SectionScores` par section) : un consommateur peut seuiller, ré-ordonner ou brancher un reranker.

### Transformation de requête par-index (`index.Semantic`)

La transformation de requête est appliquée **par-index** : les transformers marqués sémantiques (interface `pipeline.SemanticQueryTransformer`, implémentée par HyDE) ne sont appliqués qu'aux indexeurs déclarant l'interface de capacité `index.Semantic` (`Semantic() bool`), car l'expansion de requête aide la recherche vectorielle mais dégrade souvent le plein-texte. `index/sqlitevec` déclare cette capacité ; `index/bleve` (lexical) et `index/postgres` (hybride, gérant sa propre fusion lexical/vectoriel en interne) ne la déclarent pas et reçoivent la requête brute. Si aucun indexeur n'est sémantique, HyDE (et son appel LLM) est purement et simplement ignoré.

### Métadonnées de documents et filtrage (`index.Filter`)

Un document peut porter des métadonnées arbitraires (`map[string]any` : auteur, tags, dates, ...) via la capacité optionnelle `model.WithMetadata`, attachées à l'ingestion (`WithIndexFileMetadata`) et persistées par le store gorm (colonne JSON). À la recherche, `WithSearchFilter(...)` restreint les résultats aux documents dont les métadonnées satisfont toutes les conditions du filtre — une conjonction de `index.Condition` construites avec `index.Eq/Ne/Gt/Gte/Lt/Lte/In/Exists/NotExists`. Le filtrage est évalué en Go contre les métadonnées rechargées depuis le store en une requête par lot (capacité `ingest.MetadataProvider`), donc uniformément quel que soit le backend d'index (les backends ignorent les métadonnées). Il s'applique après la fusion et avant la pagination.

#### Sémantique du filtre

La sémantique est **normative** et spécifiée sur le type `index.Filter` : elle
est le contrat que doit respecter toute implémentation, que le filtre soit
évalué en Go ou (à terme) poussé dans la requête d'un backend. La suite de
conformité partagée `index/filtertest` l'encode cas par cas ; c'est le test
différentiel qui empêche deux implémentations de diverger silencieusement.

- **Présence de clé** : une clé est présente dès qu'elle figure dans la map,
  quelle que soit sa valeur (y compris `null`). Tous les opérateurs sauf
  `Exists`/`NotExists` exigent la présence — **`Ne` compris** : `Ne("author",
  "x")` ne matche pas un document sans clé `author`. C'est la lecture « SQL
  NULL-like », choisie parce que c'est celle qu'un backend SQL peut exprimer
  fidèlement ; l'absence s'exprime avec `NotExists`.
- **Nombres** : tous les types numériques Go sont unifiés en `float64` des deux
  côtés et pour tous les opérateurs, donc `Eq("count", 3)` matche le `float64(3)`
  que produit le décodage JSON.
- **Dates** : les `time.Time` et les chaînes RFC 3339 sont canonicalisées en UTC
  à précision nanoseconde fixe, **à l'écriture comme sur l'opérande du filtre**.
  L'égalité devient une comparaison de chaînes exacte et l'ordre une comparaison
  lexicographique — qui, sur des dates canoniques, est l'ordre chronologique.
  Sans cette normalisation, `2024-01-02T10:00:00+02:00` se compare *après*
  `2024-01-02T09:00:00Z` alors qu'il lui est antérieur, et le push-down des
  opérateurs ordonnés sur dates devient impossible de façon fiable.
- **Chaînes** : comparaison exacte, sensible à la casse et aux accents. Le
  `unaccent` du plein-texte ne s'étend pas aux métadonnées.
- **Types mixtes** : comparer des valeurs de natures différentes ne matche
  jamais, sans jamais produire d'erreur (évaluation totale).
- **Conteneurs** : une valeur tableau ou objet est présente mais jamais égale ni
  ordonnable — `Eq("tags", "go")` ne matche pas `tags: ["go", "db"]`.
  L'appartenance à un tableau demanderait un opérateur `Contains` explicite,
  volontairement hors périmètre pour l'instant.
- **`In` vide** : `In(key)` sans valeur ne matche rien, comme `IN ()` en SQL.
- **Clés** : restreintes à `index.KeyPattern` (`^[A-Za-z0-9_-]{1,128}$`) et
  validées avant évaluation (`ErrInvalidFilterKey`). Les clés proviennent
  souvent d'une surface exposée (flags CLI, outil MCP `search`) ; la restriction
  garantit qu'elles restent exprimables sans risque comme chemin JSON dans
  n'importe quel backend.

Les règles de normalisation et de comparaison vivent dans un unique endroit,
`internal/filternorm`, partagé par l'évaluateur Go et le chemin d'écriture des
métadonnées, précisément pour qu'il n'y ait jamais deux implémentations à tenir
d'accord.

#### Sur-échantillonnage et pagination sous filtre

Le filtre étant évalué *après* la recherche, demander `k` candidats peut n'en
laisser aucun. Le Manager sur-échantillonne donc, puis élargit la fenêtre tant
qu'il n'a pas assez de survivants (`candidateFetchFactor = 3`, croissance ×3,
borne dure `maxCandidateFetch = 500`).

Deux propriétés gouvernent ce dimensionnement :

- **Déterminisme.** La fenêtre ne dépend que de la cible — `offset + taille de
  page + 1` — et jamais d'un état conservé entre les appels. Rejouer une page
  refait donc exactement la même requête et rend le même ordre, ce qui est la
  seule façon de garder une pagination stable en déploiement multi-instances
  (aucun état serveur à partager). L'offset est transporté par le curseur ; le
  « +1 » est le résultat de lookahead sans lequel une dernière page pleine
  serait indiscernable d'une page suivie d'autres.
- **Coût borné.** Une seule requête store par tour, pour les seuls documents pas
  encore jugés : les verdicts du filtre sont mémoïsés par source pour la durée
  de la recherche, si bien qu'élargir la fenêtre ne recharge jamais les
  métadonnées déjà lues. Le cache est local à la recherche — pas de cache global
  à invalider.

Le curseur transporte aussi une **empreinte du filtre** (`index.Filter` →
`CanonicalBytes` → SHA-256 tronqué). Un curseur repère une position dans *un*
ordre filtré : le rejouer sous un autre filtre rendrait des doublons ou des
trous, silencieusement. La reprise est donc refusée avec
`ingest.ErrCursorFilterMismatch` (réexporté `amoxtli.ErrCursorFilterMismatch`),
que le client traduit par « repartir de la première page ».

L'empreinte identifie ce qu'un filtre *sélectionne*, pas la façon dont il a été
écrit : conditions réordonnées, `3` au lieu de `3.0`, date exprimée dans un autre
fuseau ou valeurs de `In` permutées donnent la même empreinte — même
normalisation que l'évaluateur (`internal/filternorm`). Un filtre vide n'a pas
d'empreinte, si bien que les curseurs des recherches non filtrées restent
interchangeables.

⚠️ **Compromis assumé** : au-delà de `maxCandidateFetch`, le rappel sous filtre
très sélectif est **tronqué**. Un filtre ne retenant qu'un document sur mille
peut rendre une page courte alors que d'autres documents correspondent. C'est le
prix du filtrage hors de l'index : pour des filtres sélectifs sur de gros
corpus, privilégier un backend qui saura pousser le filtre dans sa requête
(`index.FilterableIndex`, ci-dessous). `WithSearchCandidatePoolSize` reste
disponible pour forcer une fenêtre fixe, sans adaptation.

#### Push-down du filtre (`index.FilterableIndex`, ⚠ expérimental)

Filtrer après coup coûte la sémantique top-k, d'où le sur-échantillonnage
ci-dessus. Les backends capables d'appliquer le filtre **dans** leur requête
déclarent la capacité optionnelle
`index.FilterableIndex` (méthode `SearchFiltered`), et rendent alors `k`
résultats déjà filtrés.

La capacité est **une méthode dédiée, pas un champ `Filter` sur
`SearchOptions`** : un backend qui ignorerait silencieusement un tel champ
retournerait des résultats non filtrés que l'appelant croirait filtrés. Le
typage doit rendre cette corruption silencieuse impossible.

Deux règles encadrent la capacité :

- **Conformité obligatoire.** Un backend ne déclare `FilterableIndex` qu'après
  avoir passé `index/filtertest` contre sa propre traduction. C'est le test
  différentiel qui garantit que push-down et évaluation Go rendent les mêmes
  documents ; sans lui, un désaccord sur une clé absente ou un décalage horaire
  produirait des résultats dépendants du backend, sans rien faire échouer.
- **Détection via `index.AsFilterable`, jamais par assertion de type directe.**
  Une assertion `idx.(index.FilterableIndex)` échoue à travers un décorateur
  (logging, métriques, retry) qui ne redéclare pas la méthode. Tout décorateur
  d'`Index` doit donc soit redéclarer les méthodes de capacité qu'il veut
  exposer, soit implémenter `index.Unwrapper` (`Unwrap() Index`) — la même
  convention que `errors.Is`/`As`. `AsFilterable` et `IsSemantic` déballent
  cette chaîne (bornée, pour qu'un décorateur cyclique dégrade en « capacité
  absente » plutôt que de figer la recherche).

**`index/sqlitevec` et `index/postgres` implémentent la capacité.** Les deux
tiennent leur **propre copie des métadonnées** des documents (table dédiée,
clé : la source), écrite à chaque (ré)indexation et supprimée avec le document :
évaluer un filtre dans la requête impose que les valeurs y soient lisibles. Le
store reste la source de vérité, l'index en détient une copie dérivée,
canonicalisée par le **même** `internal/filternorm` — faute de quoi la
comparaison de dates, du texte côté SQL, divergerait. Les documents indexés
avant ces tables n'ont pas de ligne, ce que la traduction lit comme « aucune clé
présente » ; ils redeviennent filtrables à leur prochaine réindexation.

Chaque cast SQL est gardé par un test de type (`json_type` / `jsonb_typeof`) :
sans la garde, comparer une valeur texte comme un nombre lèverait une erreur SQL
là où la sémantique exige une simple absence de correspondance. Les clés sont
toujours des paramètres liés, jamais interpolées.

##### `index/postgres` (JSONB)

Le filtre est injecté dans **les deux jambes** — plein-texte et pgvector — avant
leur `LIMIT`, donc avant la fusion RRF interne. L'appliquer après la fusion
laisserait un filtre sélectif vider le top-k de chaque jambe, ce que la capacité
existe précisément pour éviter.

Deux pièges spécifiques à PostgreSQL, tranchés ici :

- **Pas de cast sur la donnée.** PostgreSQL ne garantit pas l'ordre d'évaluation
  des opérandes d'un `AND` : un `(metadata->>k)::numeric` pourrait s'exécuter
  *avant* la garde `jsonb_typeof` censée le protéger, et lever une erreur. La
  traduction compare donc du `jsonb` à du `jsonb` (`metadata->k > to_jsonb($n::numeric)`),
  ce qui déplace le cast sur le paramètre lié — le nôtre, toujours bien typé.
- **Collation binaire obligatoire.** PostgreSQL compare le texte avec la
  collation de la base ; une collation `en_US.UTF-8` ignore la ponctuation au
  niveau primaire, et une collation ICU non déterministe peut rendre `'a' = 'A'`.
  L'évaluateur Go compare des octets. Toutes les comparaisons de texte portent
  donc un `COLLATE "C"` explicite — c'est aussi ce qui rend l'ordre
  lexicographique des dates canoniques chronologique.

Le `LEFT JOIN` porte sur `dm.metadata` sans `COALESCE` : une ligne absente donne
`NULL`, tout opérateur JSON sur `NULL` donne `NULL`, et un prédicat `NULL` n'est
pas vrai — la sémantique « aucune clé présente » sort gratuitement, et l'index
GIN reste utilisable. Seul `NotExists` a besoin d'une branche `IS NULL`
explicite.

Un index GIN `jsonb_path_ops` couvre l'opérateur de containment ; les opérateurs
ordonnés scannent, ce qui est acceptable puisqu'ils s'appliquent aux lignes déjà
restreintes par la jambe plein-texte ou vectorielle.

##### `index/sqlitevec` (JSON1, schéma v3)

vec0 n'accepte pas de jointure externe dans sa contrainte KNN, mais il honore
`rowid IN (...)` **pendant** le scan. Le filtre est donc traduit en une
sous-requête de rowids éligibles injectée dans la requête KNN : les k plus
proches voisins sont choisis *parmi* les documents éligibles. C'est ce qui en
fait un vrai push-down et non un post-filtrage déguisé. La table
`document_metadata` fait partie du snapshot (`backup`/`restore`), sans quoi une
restauration rendrait l'index non filtrable.

##### Intégration au pipeline : tout ou rien

Le pipeline fusionne les listes de ses indexeurs. Il ne peut donc promettre une
liste intégralement filtrée que si **chacune** de ses jambes applique le filtre :
une seule jambe non filterable suffirait à faire remonter des résultats non
filtrés que l'appelant croirait filtrés — exactement la corruption silencieuse
que la capacité existe pour empêcher.

`pipeline.Index` déclare donc `Filterable() bool`
(`index.ConditionallyFilterable`, consultée par `AsFilterable` comme
`Semantic()` l'est pour la transformation de requête) : vrai seulement si toutes
les jambes sont filterables. Concrètement, une configuration **sqlitevec seul**
ou **postgres seul** pousse le filtre ; une configuration **bleve + sqlitevec**
ne le pousse pas et retombe intégralement sur le filtrage Go, puisque bleve ne
déclare pas la capacité (choix documenté : indexer des champs de métadonnées
dans bleve imposerait de déclarer le mapping à la création et de réindexer à
chaque changement de schéma).

Quand le push-down est disponible, le Manager appelle `SearchFiltered` et **ne
recharge aucune métadonnée** : ni sur-échantillonnage, ni second tour, ni requête
store par page. C'est le contrat de `FilterableIndex` — garanti par la suite de
conformité — qui autorise à faire confiance au backend plutôt qu'à revérifier
son travail.

##### Parité de qualité mesurée (`eval`)

Le résultat observable ne doit pas dépendre de l'endroit où le filtre est
appliqué. `TestPushdownEvalParity` (dans `eval/`) le mesure sur le harnais
d'évaluation : un corpus de 120 documents, six filtres (dont clé absente,
inégalité, plage, conjonction), et **le même backend parcouru deux fois** — une
fois tel quel, une fois enveloppé dans un décorateur opaque qui masque la
capacité et force ainsi le repli Go.

Envelopper le même index plutôt que comparer deux backends est ce qui isole la
variable : moteur, classement et jeu de données identiques, seule change la voie
du filtrage. L'exigence est donc l'**égalité stricte** des nDCG et des rappels —
pas seulement leur proximité — agrégés *et* par requête, deux écarts opposés
pouvant s'annuler dans une moyenne.

Le test a été validé par mutation : inverser la traduction SQL de `NotExists`
le fait échouer en désignant les requêtes fautives.

### Reranking (`ingest.Reranker`)

`WithReranking()` insère un reranker LLM (`retrieval.NewLLMReranker`) dans le pipeline de recherche : il réordonne les candidats fusionnés (et filtrés) par pertinence à la requête, réutilisant le budget `WithMaxTotalWords` pour borner le prompt. Contrairement au Judge (qui filtre), le reranker ne fait que réordonner. Il s'exécute après le filtrage métadonnées et avant la pagination, donc l'ordre reranké est celui exposé et encodé dans les curseurs.

### Index vectoriel sqlite-vec : partition et quantization

vec0 effectue un **scan exhaustif** (pas d'index ANN). Deux leviers en bornent
le coût :

- **Partition par collection** (schéma v2) : une ligne vec0 par
  (chunk × collection), la collection étant la *partition key* vec0. Une
  recherche filtrée ne scanne que les lignes de sa collection **et garantit k
  résultats issus de la collection** — l'ancien filtre post-KNN pouvait en
  retourner silencieusement moins (les k voisins globaux pouvaient vivre
  ailleurs). Les chunks sans collection vivent dans la partition `''` ; une
  recherche non filtrée scanne toutes les partitions et déduplique les chunks
  multi-collections. La table `embeddings_vec_map` relie les rowids vec0 aux
  chunks (`embeddings_id`) avec un index classique. La migration v1→v2 est
  automatique à l'ouverture : les blobs existants sont **re-liés, pas
  re-calculés**.
- **Quantization binaire** (`coarse_quantization`, opt-in, dimension divisible
  par 8) : recherche en deux temps — KNN Hamming sur la colonne
  `embedding_coarse bit[N]` (~30× plus rapide) présélectionnant k×8 candidats,
  puis re-scoring float et tri. Perte de qualité marginale (littérature < 1 %),
  pertinent au-delà de ~100k chunks. À valider sur le harness d'éval avant de
  l'activer en production.

### Coût LLM par recherche

Chaque étage LLM optionnel ajoute un appel réseau (latence et facturation) à
**chaque** recherche. Ordres de grandeur par configuration, hors cache :

| Configuration | Appels chat | Appels embeddings |
|---|---|---|
| Plein-texte seul (pas de `llm`) | 0 | 0 |
| + embeddings (index vectoriel) | 0 | 1 (requête) |
| + `llm.chat` (défauts : HyDE + Judge) | 2 | 1 |
| + `grounding_check` (remplace le Judge) | 2 | 1 |
| + `reranking` | 3 | 1 |
| Recherche itérative (`--deep`, décomposition + N sous-requêtes + reformulation) | 4 et plus | 1 + N |

Le prompt du Judge / reranker / évaluateur est borné par `WithMaxTotalWords`
(défaut 8000 mots, ≈ 14k tokens) et par section via
`WithMaxWordsPerSectionInPrompt` (défaut 200 mots) — c'est le poste de coût
dominant d'une recherche avec chat configuré. Trois leviers l'amortissent :

- le **cache persistant** (`llmx.CachingClient`, activé par défaut dans la
  CLI) absorbe les embeddings répétés *et* les complétions seedées (HyDE,
  Judge) : répéter une requête identique ne coûte aucun appel ;
- un **client par étage** (`WithStageLLMClient` / `llm.stages`) permet de
  pointer HyDE et le Judge vers un petit modèle rapide ;
- HyDE n'est appliqué qu'aux indexeurs sémantiques (ignoré sans eux) et
  s'exécute **en parallèle** de la branche lexicale, pas avant elle.

Sur la **qualité** apportée par ces étages, voir les résultats de référence dans
[evaluation.md](evaluation.md#résultats-de-référence-profils-de-récupération) :
l'embedder domine, HyDE apporte un gain modeste et dépendant du modèle chat, et
le grounding s'applique par défaut en mode `demote` (préserve le rappel) —
détails dans [grounding.md](grounding.md#mode-dapplication--demote-défaut--filter).

### Runner de tâches persistant (`task/gorm`)

L'ingestion (`IndexFile`, `Reindex`, `CleanupIndex`) est asynchrone : elle
planifie une tâche exécutée par un `task.Runner`. Par défaut le runner est en
mémoire (`task/memory`) — un redémarrage perd les tâches en attente. Le package
`task/gorm` fournit un runner **persistant** adossé à la même base que le store
(SQLite ou PostgreSQL), activé par `amoxtli.WithPersistentTasks(stagingDir)`.

Chaque tâche est sérialisée via son `json.Marshaler` et persistée (table
`amoxtli_tasks`) dès la planification. Au démarrage (`Run`), le runner **reprend**
le travail persisté : les tâches laissées `running` par un process interrompu
sont d'abord remises `pending` en une passe groupée *avant* le démarrage des
workers (à cet instant aucune tâche du process courant n'est encore en cours, donc
seuls de vrais orphelins sont touchés), puis toutes les `pending` sont ré-enfilées
et reconstruites via les `task.Factory` enregistrées (`RegisterFactory`, câblées
par `Manager.RegisterHandlers` pour les trois types d'ingestion). Un **claim
atomique** — transition `pending → running` gardée par `WHERE status = 'pending'`
et vérifiée sur `RowsAffected` — garantit qu'une tâche enfilée deux fois
(planifiée puis reprise, ou reprise par plusieurs runners partageant la base) ne
s'exécute qu'une seule fois. Les transitions d'état et l'état terminal sont
persistés immédiatement ; la progression intermédiaire est écrite de façon
throttlée pour ne pas marteler la base.

La reprise d'une tâche `IndexFile` suppose que son fichier mis en attente survive
au redémarrage : `WithPersistentTasks` épingle donc le répertoire de staging à un
chemin **stable** (`WithManagerStagingDir`), non supprimé à l'arrêt, au lieu du
répertoire temporaire éphémère par-process utilisé par défaut. Les handlers
d'ingestion sont idempotents (suppression par source puis ré-écriture), une tâche
reprise depuis le début est donc sûre.

### Harnais d'évaluation (`eval`)

Le package `eval` mesure objectivement la qualité de récupération. Ses fonctions
de métriques sont pures et testables sans LLM : `RecallAtK` (fraction des
documents pertinents dans le top-k, ensembliste — dégénère en binaire pour une
cible unique), `ReciprocalRank` (moyenné en MRR) et `NDCGAtK` (nDCG à relevance
binaire, sensible au rang exact contrairement au recall). Un jeu golden (`query
→ sources pertinentes`) se charge depuis un JSON (`LoadDataset`) ; `Evaluate`
le déroule à travers un `Retriever` et agrège Recall@k / MRR / nDCG@k par cut-off
et par requête. Le contrat `Retriever` est découplé du `Codex` — n'importe quelle
implémentation de récupération est notable — et `FromSearchResults` adapte
`Codex.Search` (identifiant = `Source` du résultat). Les requêtes portent une
langue (`Lang`) et des tags ; `Report.ByLang()` (et le plus général
`BySegment`) ré-agrège les métriques par segment, pour l'analyse par langue ou
type de requête.

Pour l'évaluation **en conditions réelles**, le sous-package `eval/hfqa` charge
un jeu QA extractif au **format JSON SQuAD** — partagé par des datasets Hugging
Face multilingues sur de vrais documents (PIAF en français, SQuAD en anglais,
squad_es en espagnol, MLQA, XQuAD) — et le transforme en benchmark de
récupération de passages : chaque paragraphe unique devient un document, chaque
question une requête dont la source pertinente est son paragraphe d'origine. Le
test gated `TestEvaluateRealWorld` (activé par `AMOXTLI_EVAL`, piloté par
variables d'environnement) indexe le corpus et logge le rapport global puis par
langue ; il tourne en lexical pur (bleve, sans LLM) par défaut, ou en fusion
hybride si un endpoint d'embeddings est configuré. Voir
[docs/evaluation.md](evaluation.md).

### Observabilité (`telemetry`)

L'instrumentation OpenTelemetry est **toujours active mais gratuite** : sans
provider installé, les providers no-op globaux d'OTel absorbent spans et mesures.
Le package `telemetry` expose un tracer et un meter partagés sous un scope
d'instrumentation unique, plus des instruments créés paresseusement : latence de
recherche, nombre de résultats, et pour les appels LLM le nombre d'appels, la
latence et les tokens (prompt/completion). `Codex.Search` ouvre un span (longueur
de requête, nombre de résultats — jamais le texte de la requête). Le décorateur
`llmx.ObservableClient` instrumente un `llm.Client` (spans + métriques, dérivant
les tokens de complétion depuis `Usage()`) ; l'option `WithObservability()`
l'applique automatiquement au client LLM du `Codex` (HyDE, Judge, grounding,
reranker). Les embeddings émis par un index construit par l'appelant
(sqlitevec/postgres) ne sont pas couverts par cette option — l'appelant peut
envelopper lui-même son client avec `llmx.NewObservableClient`.

### Pagination par curseur

La recherche est paginée par curseur opaque. `Codex.SearchPage(...)` renvoie une page de résultats (`WithSearchMaxResults` = taille de page) et un `NextCursor` ; l'appel suivant reprend via `WithSearchCursor(cursor)`. Le curseur ancre sur la source du dernier résultat renvoyé (unique après fusion). La pagination est stable tant que l'ordre l'est (fusion RRF déterministe, ou reranking déterministe) : un contenu strictement identique produit des scores à égalité que bleve ordonne de façon non déterministe, seul cas où la pagination peut dériver. `Codex.Search(...)` reste l'entrée simple mono-page (le filtrage et le reranking s'y appliquent toujours).
