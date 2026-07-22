# Indexation de code source

Le package [`sourcecode`](../sourcecode) découpe un fichier de code en sections
au niveau des déclarations (fonctions, méthodes, types/classes, interfaces,
traits…), commentaires de documentation inclus, via [tree-sitter](https://github.com/tree-sitter/tree-sitter)
en **pur Go** ([odvcencio/gotreesitter](https://github.com/odvcencio/gotreesitter)) —
aucun cgo, aucun binaire externe. Chaque document reçoit automatiquement les
métadonnées `type=code` et `language=<nom>`, filtrables à la recherche.

Langages intégrés : `go`, `javascript` (`.js .mjs .cjs .jsx`), `typescript`
(`.ts .mts .cts`), `tsx`, `python` (`.py .pyi`), `php`.

## Intégration en mode bibliothèque

L'indexation de code se branche via `amoxtli.WithSourceCode(...)`, en lui
passant un registre extension→langage. `sourcecode.DefaultRegistry()` fournit le
mapping intégré ; l'ingestion route alors automatiquement tout fichier dont
l'extension est enregistrée vers le parseur de code (avant, et à la place, du
convertisseur de fichiers).

```go
import (
    "github.com/bornholm/amoxtli"
    "github.com/bornholm/amoxtli/sourcecode"
)

registry := sourcecode.DefaultRegistry()

// Étendre ou surcharger le mapping (facultatif) :
if php, ok := sourcecode.ByName("php"); ok {
    registry.Register(".phtml", php)
}

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1.0}),
    amoxtli.WithSourceCode(registry),
)
```

L'indexation reste identique à celle d'un document markdown : `IndexFile`
renvoie un `task.ID` que l'on suit via `codex.TaskState(ctx, id)`. Le parsing est
*best-effort* — un fichier illisible, un dépassement de délai ou un fichier de
plus de 2 Mo dégradent vers une section racine unique couvrant tout le fichier,
sans jamais faire échouer l'ingestion.

## Recherche croisée doc ↔ code

La distinction se fait sur les métadonnées, avec la mécanique de filtres
existante (`amoxtli.WithSearchFilter`, voir `index.Eq/Ne/...`) :

```go
// Code Go uniquement
codex.Search(ctx, "supported file extensions",
    amoxtli.WithSearchFilter(index.Eq("language", "go")))

// Documentation uniquement : les documents markdown ne portent pas de `type`,
// et `index.Ne` exige que la clé soit présente — l'absence s'exprime avec
// `index.NotExists` (voir la sémantique des filtres dans docs/architecture.md)
codex.Search(ctx, "supported file extensions",
    amoxtli.WithSearchFilter(index.NotExists("type")))
```

C'est ce qui permet, par exemple, de confronter une même requête entre la
documentation et l'implémentation pour repérer des contradictions. Côté serveur
MCP, l'outil `search` expose la même capacité via son paramètre `filters` (voir
[docs/cli.md](cli.md)).

Exemple complet et exécutable : [`example/sourcecode`](../example/sourcecode/main.go)
(indexe une doc Markdown et un fichier Go, puis lance la même requête filtrée
sur le code seul, la doc seule, puis les deux).

## Taille du binaire (⚠ build tags)

Le registre `gotreesitter/grammars` embarque par défaut ses ~206 grammaires
(~23 Mo ajoutés au binaire). Pour n'embarquer que les grammaires réellement
routées, compilez avec les build tags `grammar_subset` correspondants :

```bash
go build -tags 'grammar_subset grammar_subset_go grammar_subset_javascript \
  grammar_subset_typescript grammar_subset_tsx grammar_subset_python grammar_subset_php' ./...
```

Le `Makefile` (`make build`) et `.goreleaser.yaml` d'amoxtli appliquent déjà ces
tags. **Gardez la liste synchronisée avec les langages de votre registre** : une
grammaire absente à l'exécution fait basculer le fichier concerné vers
l'indexation fichier-entier (le fallback décrit plus haut).
