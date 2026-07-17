# CLI `amoxtli`

Le binaire `amoxtli` expose la bibliothèque sous forme d'outil en ligne de
commande : il indexe des fichiers locaux dans un espace de travail par projet
(un répertoire `.amoxtli/`), effectue des recherches et sert un serveur MCP
pour les agents LLM.

## Installation

```bash
go install github.com/bornholm/amoxtli/cmd/amoxtli@latest
# ou, depuis le dépôt :
make build   # produit dist/amoxtli
```

## Espace de travail

`amoxtli init` crée dans le répertoire courant :

```
.amoxtli/
  config.yaml    # configuration (index, LLM, retrieval, converters)
  .gitignore     # ignore data/ et .env
  data/          # magasin SQLite + index bleve/vectoriel + tâches persistantes
```

Les autres commandes découvrent l'espace de travail en remontant l'arborescence
depuis le répertoire courant (comme `git`). On peut forcer le point de départ
avec `-C <dir>`, pointer une configuration précise avec `--config <fichier>`, ou
désigner directement un répertoire `.amoxtli/` via la variable d'environnement
`AMOXTLI_DIR` (utile pour `amoxtli mcp`, dont le répertoire de travail est
imposé par le client MCP).

## Configuration

`config.yaml` interpole les variables d'environnement : `${VAR}` échoue si la
variable est absente, `${VAR:-défaut}` fournit une valeur de repli, `$$` insère
un `$` littéral. Les secrets peuvent aussi être placés dans un fichier
`.amoxtli/.env` (`CLE=valeur`, ignoré par git) chargé avant l'environnement du
processus.

L'index plein-texte (bleve) est actif par défaut. L'index vectoriel
(sqlite-vec) est activé automatiquement (`enabled: auto`) dès qu'un modèle
d'embeddings est configuré sous `llm.embeddings`. Les fonctions pilotées par LLM
(`reranking`, `grounding_check`, `iterative`, `decomposition`, recherche
`--deep`) exigent une section `llm.chat`. Fournisseurs supportés : `openai`,
`openrouter` et `mistral`. Le fournisseur `openai` couvre en outre tout endpoint
compatible OpenAI (Ollama, vLLM…) via `base_url`.

### Convertisseurs de fichiers

Sans convertisseur, seul le `.md` est indexable. La section `converter` en
active trois, tous routés par extension :

| Convertisseur | Formats | Activation |
|---|---|---|
| `pandoc` | `.docx .rtf .odt .md .rst .epub .html .tex .txt` | `auto` = binaire `pandoc` présent |
| `libreoffice` | pandoc **+ `.doc`** | `auto` = binaires `libreoffice` **et** `pandoc` présents ; remplace le pandoc autonome |
| `genai` | extensions configurées (OCR/LLM : PDF, images…) | opt-in explicite : `enabled: true` + `dsn` + `extensions` |

Le convertisseur `genai` utilise un DSN d'extraction distinct du client chat :
`mistral://?apiKey=${MISTRAL_API_KEY}` ou `marker://host:port`. Ses extensions
sont prioritaires sur pandoc/libreoffice en cas de recouvrement.

### Code source

L'indexation de code source est active par défaut (`indexing.code.enabled:
auto`, tree-sitter en pur Go, aucun outil externe). Les fichiers `.go`, `.js
.mjs .cjs .jsx`, `.ts .mts .cts`, `.tsx`, `.py .pyi` et `.php` sont découpés en
sections au niveau des déclarations (fonctions, méthodes, types/classes, avec
leurs commentaires de documentation) et reçoivent automatiquement les
métadonnées `type=code` et `language=<nom>`, filtrables à la recherche :

```bash
amoxtli add -c code $(git ls-files '*.go')     # indexer le code d'un dépôt
amoxtli search "parse configuration" --filter language=go   # code Go seul
amoxtli search "parse configuration" --filter "type!=code"  # documentation seule
```

La table `indexing.code.extensions` étend ou remplace le routage
extension→langage (ex. `.phtml: php`). Langages intégrés : `go`, `javascript`,
`typescript`, `tsx`, `python`, `php`.

## Commandes

| Commande | Rôle |
|----------|------|
| `init [--force]` | Initialise l'espace de travail |
| `add <fichier>... [-c collection] [--meta k=v] [--no-wait] [--no-ignore] [--timeout d]` | Indexe des fichiers |
| `search <requête> [-n N] [-c coll] [--filter k=v] [--cursor c] [--deep] [--no-content]` | Recherche (—`--deep` = itérative LLM) |
| `doc list\|show\|delete` | Inspecte et supprime des documents (suppression par lot via filtres) |
| `collection create\|list\|show\|rename\|describe\|stats\|delete` | Gère les collections |
| `task list\|show\|cancel` | Suit les tâches d'indexation |
| `reindex [-c coll]` / `cleanup [-c coll]` | Maintenance de l'index |
| `backup [-o fichier]` / `restore <fichier>` | Sauvegarde/restauration |
| `mcp` | Serveur MCP sur stdio |

Options globales : `--json` (sortie machine), `-C <dir>`, `--config <fichier>`,
`-v/--verbose` (journalisation debug sur stderr).

Les filtres de métadonnées de `search --filter` et `doc` acceptent les
opérateurs `=`, `!=`, `>`, `>=`, `<`, `<=` (ex. `--filter year>=2020`). Les
valeurs sont typées automatiquement (booléen, nombre, sinon chaîne).

### Ignorer des fichiers (`.amoxtlignore`)

`add` écarte les fichiers correspondant à un fichier `.amoxtlignore`, sur le
modèle de `.gitignore`. Les fichiers écartés apparaissent avec le statut
`ignored` (ce n'est pas une erreur, ils ne comptent pas comme des échecs).

- **Emplacement** : un `.amoxtlignore` par répertoire, appliqués en cascade
  depuis la racine de l'espace de travail (le dossier contenant `.amoxtli`)
  jusqu'au répertoire du fichier considéré.
- **Syntaxe** : façon gitignore — un pattern sans `/` (ex. `*.log`, `build/`)
  s'applique à n'importe quelle profondeur ; un pattern contenant un `/` est
  ancré au répertoire du `.amoxtlignore` ; `#` introduit un commentaire ; `!`
  ré-inclut un fichier.
- **Limitation** : une négation `!` ne peut ré-inclure que dans le même
  `.amoxtlignore` ; elle ne peut pas annuler une règle héritée d'un répertoire
  parent.
- `--no-ignore` indexe les fichiers même s'ils correspondent à une règle.

```gitignore
# .amoxtlignore
*.log
build/
!important.log
```

### Exemple

```bash
amoxtli init
amoxtli add --meta topic=go ./docs/*.md
amoxtli search "modèle de concurrence" -n 3
amoxtli search --deep "comment fonctionne le grounding ?"   # nécessite llm.chat
```

## Serveur MCP

Le serveur MCP expose quatre outils en lecture seule : `search` (contenu des
sections inline, options `deep` et `filters` — mêmes expressions que `--filter`,
ex. `["type=code", "language=go"]`), `fetch_sections`, `list_collections` et
`list_documents`. Deux transports sont disponibles :

- `amoxtli mcp stdio` (ou simplement `amoxtli mcp`) sert le protocole sur
  stdin/stdout ; **tous les diagnostics vont sur stderr**. C'est le mode
  « un processus par client », lancé par le client MCP lui-même.
- `amoxtli mcp http --addr :8080` sert le transport HTTP streamable depuis un
  **processus unique et durable** qui gère plusieurs sessions client
  concurrentes. C'est la brique d'une utilisation mutualisée (front de chat
  multi-utilisateurs).

Exemple d'entrée dans la configuration d'un client MCP (transport stdio) :

```json
{
  "mcpServers": {
    "amoxtli": {
      "command": "amoxtli",
      "args": ["mcp", "stdio"],
      "env": { "AMOXTLI_DIR": "/chemin/vers/mon-projet/.amoxtli" }
    }
  }
}
```

## Concurrence

Les backends **par fichier** (bleve full-text, sqlite-vec, store sqlite) prennent
un verrou exclusif : **un seul processus amoxtli peut utiliser un tel espace de
travail à la fois**. Un fichier de verrou (`.amoxtli/data/lock`) est pris par
toute commande ouvrant ces index et produit un message clair si l'espace est
déjà occupé (par exemple un `amoxtli mcp` en cours). Pour indexer pendant qu'un
serveur MCP tourne, arrêtez-le d'abord.

Pour un déploiement **mutualisé** (plusieurs processus, ou un serveur
`mcp http` partagé par plusieurs utilisateurs), basculez sur un backend
client-serveur PostgreSQL :

```yaml
store:
  driver: postgres
  dsn: postgres://user:pass@host:5432/kb?sslmode=disable
index:
  driver: postgres   # index hybride full-text + pgvector
```

La base PostgreSQL doit disposer des extensions `vector` et `unaccent` (p. ex.
image Docker `pgvector/pgvector`). Dans ce mode, aucun état exclusif n'est posé
sur le disque : le verrou de workspace est ignoré et **plusieurs instances
`amoxtli mcp http` peuvent servir la même base simultanément**. L'index et le
store peuvent partager le même DSN (`index.postgres.dsn` par défaut = `store.dsn`
lorsque le store est postgres).
