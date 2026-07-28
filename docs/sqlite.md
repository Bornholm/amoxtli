# Backend SQLite

Le déploiement local d'amoxtli repose sur des fichiers SQLite : le magasin de
documents (`ingest/gorm`), l'index vectoriel (`index/sqlitevec`, extension
[sqlite-vec](https://github.com/asg017/sqlite-vec)) et, en option, le stockage
des images. Aucun serveur à déployer, aucune dépendance système : c'est la
configuration par défaut de la CLI (`.amoxtli/`) et le pendant local du
[backend PostgreSQL](postgres.md).

```go
dir := "/data/kb"

store, err := gorm.NewSQLiteStore(filepath.Join(dir, "data.sqlite")) // ingest/gorm
defer store.Close()

bleveIdx, err := bleve.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve")) // index/bleve
defer bleveIdx.Close()

vecIdx, err := sqlitevec.NewIndexAtPath(filepath.Join(dir, "index.sqlite"), llmClient) // index/sqlitevec
defer vecIdx.Close()

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.5},
        amoxtli.Indexer{ID: "vector", Index: vecIdx, Weight: 0.5},
    ),
)
```

Voir [`example/sqlite`](../example/sqlite/main.go) pour un exemple complet et
exécutable (SQLite + bleve, sans client LLM).

`NewIndexAtPath` possède ses connexions : une connexion d'écriture et un pool de
connexions de lecture (`WithReadPoolSize`), de sorte que les recherches
concurrentes s'exécutent chacune sur sa connexion sous isolation snapshot WAL au
lieu de se sérialiser. Les migrations de schéma et le contrôle d'identité
modèle/dimension sont effectués à l'ouverture. Sur le fonctionnement interne de
l'index (partition vec0 par collection, quantization binaire, poussée des
filtres), voir [architecture.md](architecture.md).

## Build WASM embarqué

Aucune directive `replace` n'est nécessaire côté consommateur : `index/sqlitevec`
embarque son propre build WASM de SQLite incluant l'extension sqlite-vec (voir
`index/sqlitevec/internal/vec`). Il n'y a donc ni cgo, ni bibliothèque native à
installer.

## Contraintes de versions (`ncruces/go-sqlite3` et `wazero`)

Ce build WASM impose deux contraintes, déclarées dans le `go.mod` d'amoxtli et
**à préserver côté consommateur** :

```
require github.com/ncruces/go-sqlite3 v0.23.0   // ABI hôte du WASM
require github.com/tetratelabs/wazero v1.11.0   // >= v1.9.0
```

- `ncruces/go-sqlite3` **v0.23.0** : le WASM est couplé à cette ABI
  (`sqlite3.Binary` / `sqlite3.RuntimeConfig`, retirées dans les versions
  ultérieures ; les versions ≥ v0.30.5 attendent un contrat guest incompatible).
- `tetratelabs/wazero` **≥ v1.9.0** : le compilateur de wazero v1.8.2 (version
  épinglée par défaut par ncruces v0.23.0) mis-compile `vec0Filter` de sqlite-vec
  et provoque un crash (`out of bounds memory access`) sur **toute** requête KNN.
  Corrigé depuis wazero v1.9.0.

Ces versions font partie du contrat d'installation du backend et ne changeront
pas sans note explicite dans le [CHANGELOG](../CHANGELOG.md) (voir
[stability.md](stability.md)).

Les autres backends (bleve, postgres) et le magasin SQLite (`ingest/gorm`) ne
sont pas concernés : ils n'utilisent pas ce build WASM.

## Stockage des images

Sur un espace de travail SQLite, le stockage des blobs par défaut est `fs`
(`blob/fs` : `<dir>/<2 hexits>/<hash>` plus un sidecar JSON pour le type média),
ce qui garde les octets des images hors des fichiers que bleve et sqlite-vec
côtoient déjà. Le stockage `database` reste possible — la définition gorm des
blobs donne un `BLOB` en SQLite comme un `bytea` en PostgreSQL, et la
[suite de conformance](../blob/testsuite) est exécutée sur les deux moteurs.
Détails et arbitrages dans [images.md](images.md).
