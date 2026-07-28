# Backend PostgreSQL

`index/postgres` combine le FTS natif (`tsvector` + `unaccent`, configuration de langue détectée automatiquement) et pgvector (KNN cosinus, index HNSW), fusionnés par Reciprocal Rank Fusion. Sans client LLM, il fonctionne en plein-texte seul. La base doit disposer des extensions `vector` et `unaccent` (images Docker [`pgvector/pgvector`](https://hub.docker.com/r/pgvector/pgvector)).

Le magasin de documents et l'index peuvent tous deux vivre dans la même base, pour un déploiement **entièrement PostgreSQL** sans aucun stockage local :

```go
dsn := "postgres://user:pass@localhost:5432/kb?sslmode=disable"

store, err := gorm.NewPostgresStore(ctx, dsn) // ingest/gorm
defer store.Close()

pool, err := pgxpool.New(ctx, dsn) // possédé par l'appelant
defer pool.Close()
pg := postgres.NewIndex(pool, llmClient) // client LLM nil = plein-texte seul

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(amoxtli.Indexer{ID: "postgres", Index: pg, Weight: 1.0}),
)
```

Voir [`example/postgres`](../example/postgres/main.go) pour un exemple complet et exécutable.

## Table `blobs` (images)

Quand le stockage des images est actif (`images.store: database`, ou `auto` avec un magasin PostgreSQL), `blob/gorm` crée une table `blobs` dans la **même base** — une seule connexion, un seul serveur à sauvegarder :

| Colonne | Type | Rôle |
|---|---|---|
| `hash` | `text` (clé primaire, 64 car.) | `hex(sha256(contenu))` : le contenu *est* la clé |
| `mime_type` | `text` | type média servi tel quel par MCP |
| `size` | `bigint` | taille en octets |
| `data` | `bytea` | le contenu |
| `created_at` | `bigint` | horodatage d'insertion |

Le mapping `[]byte` → `bytea` est celui de gorm par défaut, sans tag spécifique au dialecte : la même définition donne un `BLOB` en SQLite, et la [suite de conformance](../blob/testsuite) est exécutée sur les deux moteurs pour le vérifier.

L'insertion est un `ON CONFLICT DO NOTHING` sur le hash : deux ingestions simultanées de la même image ne se marchent pas dessus.

**Sauvegarde.** La table est incluse dans les sauvegardes SQL natives du serveur (`pg_dump`) comme n'importe quelle autre — c'est l'intérêt du choix « tout PostgreSQL ». Le snapshot applicatif (`amoxtli backup`) l'inclut également, sous l'identifiant de partie `blobs-v1`, ce qui permet en plus de migrer entre backends (`fs` ↔ `database`).

**Table `document_blobs`.** Le magasin de documents tient en parallèle un index
`document_blobs (document_id, hash)`, maintenu dans la même transaction que
`SaveDocuments` et supprimé en cascade avec le document. Il donne l'ensemble des
blobs vivants au ramasse-miettes en un `SELECT DISTINCT hash` indexé, sans lire
le contenu des documents. Il est entièrement dérivé : le reconstruire revient à
réindexer, et sa cohérence est verrouillée par un test différentiel contre
`blob.ScanHashes`.

**Volumétrie.** Chaque image décrite occupe une ligne `bytea` : à surveiller sur un corpus riche en images. Les parades sont `images.max_size` (10 Mio par défaut), la déduplication par hash (une image partagée n'est stockée qu'une fois) et le ramasse-miettes de la tâche de nettoyage, qui supprime les blobs qu'aucun document ne référence plus.
