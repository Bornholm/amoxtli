# Tests

Les tests unitaires tournent sans dépendance externe. Les tests d'intégration démarrent des conteneurs Docker (via [testcontainers](https://golang.testcontainers.org/)) et sont gardés derrière des variables d'environnement dédiées ; `-short` les saute tous.

```bash
go test -short ./...                                             # sans Docker
AMOXTLI_TEST_OLLAMA=1 go test ./index/sqlitevec/ -timeout 20m    # Docker + Ollama
AMOXTLI_TEST_OLLAMA=1 go test ./retrieval/ -timeout 20m          # Docker + Ollama (grounding/décomposition/re-retrieval)
AMOXTLI_TEST_POSTGRES=1 go test ./index/postgres/ -timeout 10m   # Docker + PostgreSQL (FTS seul)
AMOXTLI_TEST_POSTGRES=1 go test ./ingest/gorm/ -timeout 10m      # Docker + PostgreSQL (magasin de documents)
AMOXTLI_TEST_POSTGRES=1 AMOXTLI_TEST_OLLAMA=1 \
  go test ./index/postgres/ -timeout 20m                         # Docker + PostgreSQL + Ollama (hybride)
```

Les conteneurs Ollama réutilisent un volume nommé `ollama-data` comme cache de modèles : le premier run télécharge les modèles (~2 Go), les suivants les réutilisent.
