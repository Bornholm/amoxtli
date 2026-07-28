# Tests

Les tests unitaires tournent sans dépendance externe. Les tests d'intégration démarrent des conteneurs Docker (via [testcontainers](https://golang.testcontainers.org/)) et sont gardés derrière des variables d'environnement dédiées ; `-short` les saute tous.

```bash
go test -short ./...                                             # sans Docker
AMOXTLI_TEST_OLLAMA=1 go test ./index/sqlitevec/ -timeout 20m    # Docker + Ollama
AMOXTLI_TEST_OLLAMA=1 go test ./retrieval/ -timeout 20m          # Docker + Ollama (grounding/décomposition/re-retrieval)
AMOXTLI_TEST_POSTGRES=1 go test ./index/postgres/ -timeout 10m   # Docker + PostgreSQL (FTS seul)
AMOXTLI_TEST_POSTGRES=1 go test ./ingest/gorm/ -timeout 10m      # Docker + PostgreSQL (magasin de documents)
AMOXTLI_TEST_OLLAMA=1 go test ./vision/ -timeout 40m             # Docker + Ollama (OCR / description d'images)
AMOXTLI_TEST_POSTGRES=1 AMOXTLI_TEST_OLLAMA=1 \
  go test ./index/postgres/ -timeout 20m                         # Docker + PostgreSQL + Ollama (hybride)
```

Les conteneurs Ollama réutilisent un volume nommé `ollama-data` comme cache de modèles : le premier run télécharge les modèles (~2 Go), les suivants les réutilisent.

## Test vision / OCR

`vision/integration_test.go` dessine un mot dans une image (police bitmap intégrée, agrandie — aucune fixture binaire dans le dépôt) et vérifie que le modèle le retranscrit, à travers le describer, le convertisseur `convert/vision` et le cache de descriptions.

Deux variables permettent de changer de modèle sans toucher au code :

| Variable | Défaut | Rôle |
|---|---|---|
| `AMOXTLI_TEST_VISION_MODEL` | `glm-ocr:q8_0` | modèle vision/OCR à tirer et interroger (~2,2 Go) |
| `AMOXTLI_TEST_OLLAMA_IMAGE` | `ollama/ollama:latest` | image du conteneur ; les modèles OCR récents exigent un runtime récent, d'où le `latest` plutôt que la version épinglée des autres tests |

```bash
AMOXTLI_TEST_OLLAMA=1 AMOXTLI_TEST_VISION_MODEL=granite3.2-vision \
  go test ./vision/ -run TestIntegrationDescribe -v -timeout 40m
```

Sur CPU seul, une description prend plusieurs dizaines de secondes : d'où le `-timeout` généreux. L'assertion est volontairement tolérante sur la forme (le mot peut arriver dans `Text` si le modèle respecte le schéma JSON, ou dans `Description` via le repli texte brut) mais stricte sur le fond : le mot doit être lu.

Le test vérifie aussi qu'**aucun fragment de `vision.DefaultPrompt` ne revient dans la réponse**. Ce n'est pas théorique : une formulation antérieure du prompt, qui détaillait les champs en prose, était intégralement recopiée par `glm-ocr` dans le champ `description` — et aurait donc été indexée pour chaque image. D'où deux règles pour toute évolution du prompt : rester court et sans énumération (le vocabulaire de cadrage vit dans les descriptions du schéma JSON, `descriptionSchema`), puis rejouer ce test.

Deux modèles ont été validés sur cette base :

| Modèle | OCR | Description |
|---|---|---|
| `glm-ocr:q8_0` | exacte | se limite à la transcription — c'est un modèle OCR, pas un descripteur |
| `qwen2.5vl:3b` | exacte | titre et description réels, ~100 s sur CPU |

La fixture est rendue avec la police vectorielle Go (`x/image/font/gofont`) rastérisée à 96 px avec antialiasing. Une police bitmap agrandie donnait des glyphes crénelés que `qwen2.5vl:3b` lisait `HMXTLI` en les qualifiant de « distorted letters » : si le test échoue sur la seule transcription, suspecter la lisibilité de l'image avant le modèle.
