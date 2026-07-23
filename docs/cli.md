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

### Profils de récupération

`retrieval.profile` choisit un préréglage des étages LLM, du moins cher au plus
poussé — les clés explicites (`reranking`, `grounding_check`, …) s'ajoutent
par-dessus :

| Profil | Étages | Appels chat / recherche |
|---|---|---|
| `fast` | embeddings + fusion RRF + dédup | 0 |
| `balanced` | + HyDE (seedé, mis en cache) | 1 (0 si requête répétée) |
| `precision` | + évaluateur de grounding | 2 |
| *(vide)* | défaut historique : HyDE + Judge | 2 |

`fast` fonctionne sans `llm.chat` ; `balanced` et `precision` l'exigent.

Le grounding (`grounding_check`, ou le profil `precision`) applique son verdict
selon `retrieval.grounding_mode` : **`demote` (défaut)** garde tous les
documents mais relègue les non pertinents en fin de liste — il préserve le
rappel *et* améliore le classement ; **`filter`** supprime les documents non
pertinents — forte précision de liste mais rappel tronqué, pour du RAG à listes
courtes. Sur SciFact (corpus complet, 300 requêtes, mistral-small-24b) : demote
nDCG@10 0,752 / Recall@10 0,867 vs filter 0,618 / 0,649. Attention : avec un
petit modèle chat, `filter` peut effondrer le rappel (sur-filtrage) — une raison
de plus de garder demote.

### Cache LLM

Dès que `llm.embeddings` ou `llm.chat` est configuré, un cache persistant sur
disque (`llm.cache`, activé par défaut, répertoire `cache/` dans `.amoxtli/`)
mémorise les vecteurs d'embeddings (`cache/embeddings`) et les complétions de
chat déterministes — HyDE est seedé par requête (`cache/chat`) —, clés par
modèle : réindexer un contenu inchangé ou répéter une requête identique ne
touche plus les endpoints (ni leur facturation, ni leur rate-limit). Les
clients par étage (`llm.stages`) bénéficient du même cache de chat. `amoxtli
cache purge` vide le cache ; les statistiques hits/misses sont journalisées en
mode `--verbose`.

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
amoxtli search "parse configuration" --filter '!type'       # documentation seule
```

La table `indexing.code.extensions` étend ou remplace le routage
extension→langage (ex. `.phtml: php`). Langages intégrés : `go`, `javascript`,
`typescript`, `tsx`, `python`, `php`.

## Commandes

| Commande | Rôle |
|----------|------|
| `init [--force]` | Initialise l'espace de travail |
| `add <fichier>... [-c collection] [--meta k=v] [--base-dir d] [--no-wait] [--no-ignore] [--no-file-metadata] [--timeout d]` | Indexe des fichiers |
| `sync <dir> [-c collection] [--filter glob] [--base-dir d] [--dry-run] [--no-wait] [--no-ignore] [--no-file-metadata] [--timeout d]` | Synchronise l'index avec une arborescence (indexe, ignore l'inchangé, supprime les disparus) |
| `search <requête> [-n N] [-c coll] [--filter k=v] [--cursor c] [--deep] [--no-content]` | Recherche (—`--deep` = itérative LLM) |
| `doc list\|show\|delete` | Inspecte et supprime des documents (suppression par lot via filtres) |
| `collection create\|list\|show\|rename\|describe\|stats\|delete` | Gère les collections |
| `task list\|show\|cancel` | Suit les tâches d'indexation |
| `reindex [-c coll]` / `cleanup [-c coll]` | Maintenance de l'index |
| `cache purge` | Vide le cache LLM sur disque (embeddings + chat) |
| `backup [-o fichier]` / `restore <fichier>` | Sauvegarde/restauration |
| `mcp` | Serveur MCP sur stdio |

Options globales : `--json` (sortie machine), `-C <dir>`, `--config <fichier>`,
`-v/--verbose` (journalisation debug sur stderr).

Les filtres de métadonnées de `search --filter` et `doc` acceptent les
opérateurs `=`, `!=`, `>`, `>=`, `<`, `<=` (ex. `--filter year>=2020`), plus
deux formes de présence : `clé?` (le document porte la clé) et `!clé` (il ne la
porte pas). Les valeurs sont typées automatiquement (`true`/`false` en booléen,
nombre, sinon chaîne).

Un `--cursor` est lié au `--filter` avec lequel il a été émis : le rejouer sous
un filtre différent est refusé (il désignerait une position dans un autre
classement, d'où doublons et trous). Reprendre sans `--cursor` repart de la
première page. Un filtre équivalent mais écrit autrement (conditions
réordonnées, `2020` au lieu de `2020.0`) reste accepté.

⚠️ Tous les opérateurs **exigent que la clé soit présente**, `!=` compris :
`--filter "type!=code"` ne remonte pas les documents dépourvus de `type`. C'est
la sémantique « SQL NULL-like » (voir
[docs/architecture.md](architecture.md#sémantique-du-filtre)) ; pour cibler
l'absence, utiliser `--filter '!type'`. Les clés sont limitées à
`[A-Za-z0-9_-]` (128 caractères max) ; une clé hors de ce jeu est rejetée.

### Métadonnées de fichier

`add` et `sync` attachent par défaut à chaque document les attributs du fichier
indexé, directement utilisables comme clés de `--filter` :

| Clé | Valeur | Exemple de filtre |
|-----|--------|-------------------|
| `filename` | Nom du fichier, extension comprise | `--filter filename=cli.md` |
| `extension` | Extension en minuscules, sans le point | `--filter extension=md` |
| `size` | Taille en octets | `--filter size>100000` |
| `mtime` | Date de dernière modification sur disque | `--filter mtime>=2026-01-01T00:00:00Z` |
| `dirname` | Répertoire du fichier, exprimé comme la source (relatif à `--base-dir` s'il est posé, absolu sinon) | `--filter dirname=/docs` |
| `indexed_at` | Date de passage à l'indexation | `--filter indexed_at<2026-01-01T00:00:00Z` |

```bash
amoxtli sync --base-dir . ./docs
amoxtli search "authentification" --filter dirname=/docs/guides --filter extension=md
amoxtli doc list --filter indexed_at'<'2026-01-01T00:00:00Z   # documents jamais rafraîchis depuis
```

- Les dates sont stockées au format canonique (RFC 3339, UTC, précision
  nanoseconde) : leur comparaison lexicographique est chronologique, quel que
  soit le backend. Un opérande de filtre écrit en RFC 3339 est canonicalisé de
  la même façon.
- `--meta k=v` l'emporte sur la valeur dérivée de la même clé : c'est la
  soupape quand l'attribut déduit ne convient pas.
- Ces clés s'ajoutent à celles injectées par les analyseurs (`type=code`,
  `language=<nom>`) sans les remplacer.
- `--no-file-metadata` les désactive ; seul ce qui est passé par `--meta` est
  alors enregistré.
- Elles ne sont posées qu'au moment où le fichier est lu sur le disque : les
  documents déjà présents dans l'index ne les portent pas. Comme `sync` ignore
  les fichiers inchangés (ETag mtime+taille) et que `reindex` reconstruit
  l'index à partir des documents **stockés**, rattraper un fonds existant
  demande de le réingérer depuis le disque — `amoxtli doc delete
  --source-like 'file:///docs/%'` puis `amoxtli sync`, ou un `add` sur les
  fichiers concernés (une source déjà connue est remplacée).

### Chemins sources (`--base-dir`)

Par défaut, `add` et `sync` enregistrent chaque document avec le chemin **absolu**
du fichier indexé (`file:///home/alice/projets/kb/docs/cli.md`). Ce chemin
ressort tel quel dans `doc list`, dans les résultats de recherche et dans les
outils MCP `search` / `list_documents` : il révèle la topologie du système de
fichiers de la machine qui indexe.

`--base-dir <dir>` retire ce préfixe et ne conserve que le chemin relatif :

```bash
amoxtli add --base-dir . ./docs/cli.md     # source : file:///docs/cli.md
amoxtli sync --base-dir . ./docs           # idem sur toute l'arborescence
```

- Le chemin stocké garde son `/` initial : il reste une URL `file://` bien
  formée (sans quoi le premier segment serait relu comme un hôte). Ce `/`
  désigne le répertoire de base, pas la racine du système de fichiers.
- Un fichier situé **hors** du répertoire de base est refusé (`… is outside the
  base directory …`) plutôt qu'indexé avec un chemin absolu : aucune fuite ne
  peut passer au travers une fois l'option posée. Pour `sync`, c'est
  l'arborescence passée en argument qui doit se trouver sous `--base-dir`.
- L'option est un drapeau par commande, et non un réglage de `config.yaml` :
  plusieurs racines peuvent cohabiter dans le même index (une invocation par
  racine, chacune avec son propre `--base-dir`).
- Le choix engage la durée : `sync` reconnaît les documents déjà indexés par
  leur source. Passer une arborescence de l'absolu au relatif (ou changer de
  `--base-dir`) la fait réindexer sous ses nouvelles sources ; les anciennes
  entrées restent en place jusqu'à un `amoxtli doc delete --source-like`.
- La sortie des commandes (`sync … : 3 indexed`, statut par fichier) continue
  d'afficher les chemins locaux : elle décrit l'exécution en cours, elle n'est
  pas indexée.

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
sections inline, option `filters` — mêmes expressions que `--filter`,
ex. `["type=code", "language=go"]`), `fetch_sections`, `list_collections` et
`list_documents`.

`search` et `list_documents` renvoient les métadonnées de chaque document dans
un champ `metadata`. L'agent découvre ainsi les clés et valeurs réellement
indexées, et peut les réinjecter dans `filters` au tour suivant sans les
deviner.

La profondeur de recherche n'est pas un paramètre d'outil : elle relève de la
configuration de l'espace de travail. Avec `retrieval.iterative.enabled: true`,
chaque appel à `search` emprunte l'orchestration itérative (reformulation pilotée
par le grounding) et remonte le nombre de tours dans `rounds` ; sinon il s'agit
d'une recherche paginée simple. Le verdict `grounding` est joint dès que
l'évaluateur tourne (`retrieval.grounding_check`, `retrieval.iterative` ou
`retrieval.profile: precision`).

Deux transports sont disponibles :

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
