# Indexation des images

Les images sont rendues **cherchables** en les décrivant avec un LLM vision : la
description (titre, description détaillée, texte visible transcrit) est ensuite
indexée comme du texte markdown ordinaire. Elle profite donc gratuitement de la
recherche plein-texte, de la recherche vectorielle, du découpage en sections, de
la fusion RRF, du filtrage par métadonnées et du grounding — **sans aucune
modification des index**.

Deux chemins, un seul mécanisme de description :

| Cas | Composant | Résultat |
|---|---|---|
| Fichier image autonome (`.png`, `.jpg`…) | `convert/vision` | un document markdown avec un frontmatter `type: image` |
| Image embarquée dans un document | `markdown/imagetext` | la description insérée en texte, juste après l'image, dans la même section |

Un [blob store](#réafficher-les-images-mcp) optionnel s'ajoute par-dessus pour
rendre les images **réaffichables** par un agent, et non plus seulement
cherchables.

> **Choix d'architecture.** L'alternative « embeddings multimodaux » (CLIP & co)
> a été écartée : elle exigerait un espace vectoriel dédié, un nouvel index et
> une fusion spécifique, et resterait incompatible avec la recherche
> plein-texte. Décrire en texte coûte un appel LLM par image — mis en cache par
> contenu — et ne coûte rien au reste du système.

## Fichiers image autonomes

`convert/vision` est un `convert.Converter` comme les autres : routé par
extension, il transforme l'image en markdown avant parsing.

```markdown
---
type: image
---

# Diagramme d'architecture

Un schéma reliant le convertisseur à l'index…

## Texte visible

ingestion → conversion → index
```

Le frontmatter est remonté dans les métadonnées du document par
`markdown.Parse` — c'est le mécanisme générique de *hoisting* du frontmatter,
symétrique de ce que fait `sourcecode.Parse` avec `type=code`. Toutes les clés
scalaires d'un frontmatter markdown sont concernées, pas seulement `type`.

Extensions par défaut (`convvision.DefaultExtensions`) : `.png`, `.jpg`,
`.jpeg`, `.webp`, `.gif` — le sous-ensemble accepté par les principaux
fournisseurs vision.

## Images embarquées dans les documents

`markdown/imagetext.Enrich` s'applique **avant le parsing**, donc uniformément
au markdown natif et à la sortie des convertisseurs (pandoc, LibreOffice, OCR).
Il réécrit les octets de la source pour insérer, après le bloc de chaque image,
un paragraphe de description :

```markdown
![Schéma](media/schema.png)

> **Image — Diagramme d'architecture** : Un schéma reliant le convertisseur à l'index. ingestion
```

L'insertion vit dans le même bloc que l'image : elle tombe donc dans la même
section au parsing, contextualisée par les titres environnants — exactement ce
qu'il faut pour la recherche et le grounding.

Résolution des destinations :

| Destination | Traitement |
|---|---|
| `data:image/...;base64,…` | décodée sur place |
| chemin relatif (`media/x.png`) | résolu sous le répertoire du fichier indexé, **strictement confiné** |
| chemin absolu, `../` sortant du répertoire | **refusé** |
| `http(s)://` | **ignoré** par défaut (aucun appel réseau depuis l'ingestion) |

Les fichiers de code ne passent jamais par l'enrichissement.

### Documents convertis (docx, odt, epub…)

pandoc perd les médias par défaut. `pandoc.WithInlineMedia(maxBytes)` ajoute
`--extract-media` et réécrit les médias extraits en data-URI, ce qui rend le
markdown produit autonome — le répertoire temporaire d'extraction est supprimé
dès le retour de la conversion. La phase d'enrichissement les traite ensuite
sans rien savoir de pandoc. LibreOffice en hérite (il délègue à pandoc).

C'est **opt-in** : inliner des images gonfle la source indexée de base64, ce qui
ne se justifie que si quelque chose en aval les lit. La CLI l'active
automatiquement quand `converter.vision.embedded.enabled` est vrai.

Voir [docs/converters.md](converters.md) pour le comportement par convertisseur.

## Configuration (CLI)

```yaml
converter:
  vision:
    enabled: true
    # Client dédié pointant un modèle vision. Absent : réutilise llm.chat,
    # qui doit alors accepter les pièces jointes image.
    chat:
      provider: openrouter
      model: qwen/qwen2.5-vl-72b-instruct
      api_key: ${OPENROUTER_API_KEY}
    extensions: [.png, .jpg, .jpeg, .webp, .gif]
    # Taille maximale d'une image envoyée au modèle, en octets.
    # 0 = défaut (10 Mio). Au-delà : refus, jamais de troncature.
    max_image_size: 10485760
    # Prompt de description personnalisé (fait partie de la clé de cache).
    # prompt: |
    #   ...
    embedded:
      # Décrit aussi les images embarquées dans les documents.
      enabled: true
      # Plus petit côté accepté, en px : en dessous, c'est une icône ou une
      # puce, pas du contenu. 0 = défaut (64), négatif = filtre désactivé.
      min_dimensions: 64
      # Principal levier de coût sur un corpus riche en images. 0 = défaut (32).
      max_images_per_document: 32
      # Descriptions menées en parallèle pour un même document. 0 = défaut (2).
      concurrency: 2
```

`embedded.enabled` exige `vision.enabled` (vérifié à la validation de la
configuration). Les extensions de `converter.vision` sont routées **avant**
celles de `converter.genai` : une extension image explicitement confiée à un
modèle vision prime sur la même extension listée côté OCR.

## Configuration (bibliothèque)

```go
import (
    "github.com/bornholm/amoxtli"
    "github.com/bornholm/amoxtli/convert"
    convvision "github.com/bornholm/amoxtli/convert/vision"
    "github.com/bornholm/amoxtli/llmx"
    "github.com/bornholm/amoxtli/markdown/imagetext"
    "github.com/bornholm/amoxtli/vision"
)

// Le client est décoré par l'appelant (retry + rate-limit), comme pour
// HyDE/Judge ; le paquet vision ne redécore rien.
describer := vision.NewLLMDescriber(llmx.NewRetryClient(visionClient))

// Cache de descriptions, indexé par le contenu de l'image.
cached, err := vision.NewCachingDescriber(describer, "/data/kb/cache", vision.Namespace(model, ""))

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(/* … */),

    // Fichiers image autonomes.
    amoxtli.WithFileConverter(convert.NewRouted(convvision.NewConverter(cached))),

    // Images embarquées dans les documents.
    amoxtli.WithImageEnrichment(
        imagetext.WithDescriber(cached),
        imagetext.WithMaxImagesPerDocument(16),
    ),
)
```

Sans `imagetext.WithDescriber`, `WithImageEnrichment` construit le descripteur à
partir du client passé à `WithLLMClient` — qui doit alors être capable de
vision. Partager un même `Describer` entre les deux chemins, comme ci-dessus,
garantit qu'une image rencontrée deux fois (comme fichier puis embarquée dans un
document) n'est décrite qu'une seule fois.

Exemple exécutable : [`example/vision`](../example/vision/main.go) (descripteur
factice par défaut, `-llm` pour un vrai modèle).

## Rechercher les images

Le filtrage repose sur la métadonnée `type=image` :

```bash
amoxtli search "schéma d'architecture"                      # tout
amoxtli search "schéma d'architecture" --filter type=image  # images seules
amoxtli search "schéma d'architecture" --filter '!type'     # documentation seule
```

Les images embarquées, elles, ne sont pas des documents distincts : leur
description appartient au document qui les contient et se cherche comme son
texte.

Les métadonnées de fichier automatiques (`filename`, `extension`, `size`,
`mtime`, `dirname`, `indexed_at`) s'appliquent aux images comme à tout fichier
ajouté par la CLI.

## Coûts et cache

Un premier `sync` sur un corpus riche en images peut générer des centaines
d'appels vision. Quatre garde-fous se cumulent :

1. **Cache par contenu** — clé `sha256(version du prompt, modèle, octets de
   l'image)`, sous `<cache>/vision/`. Indépendant du nom de fichier : une image
   renommée, déplacée ou dupliquée n'est décrite qu'une fois, et une
   réindexation complète ne coûte rien.
2. **ETag d'ingestion** — un fichier inchangé n'est pas réindexé du tout.
3. **Plafond par document** — `max_images_per_document` (32 par défaut) ;
   au-delà, les images restantes gardent leur seul texte alternatif.
4. **Filtres avant appel** — dimensions minimales, taille maximale, type MIME :
   tous appliqués **avant** le moindre appel au modèle.

> Le cache chat de `llmx` refuse délibérément de mettre en cache les messages
> portant une pièce jointe (sérialiser l'image dans la clé JSON serait coûteux) :
> d'où ce cache dédié, qui hache les octets directement.

La concurrence des descriptions d'un même document est bornée (2 par défaut) :
le rate-limit global du `RetryClient` protège le fournisseur, cette borne locale
protège la mémoire et évite d'aggraver la congestion d'ingestion.

## Choix du modèle vision

La recherche ne vaut que ce que vaut la description. Deux familles, aux
comportements très différents, mesurées par le test d'intégration
(`vision/integration_test.go`, voir [docs/testing.md](testing.md)) :

| Famille | Exemple | Transcription | Description |
|---|---|---|---|
| Modèle OCR spécialisé | `glm-ocr:q8_0` (~2,2 Go) | excellente | quasi inexistante : il transcrit, il ne décrit pas |
| Modèle vision généraliste | `qwen2.5vl:3b`, `qwen/qwen2.5-vl-72b-instruct` | bonne | titre et description réels |

Pour de la recherche documentaire, un modèle **généraliste** est le bon défaut :
c'est la description qui porte le vocabulaire qu'un utilisateur tape. Un modèle
OCR ne convient que pour un corpus de scans où seul le texte compte.

⚠ **Tous les modèles chat n'acceptent pas les pièces jointes image.** La
configuration exige un client chat résolu, mais la capacité vision elle-même
n'est constatée qu'au premier appel : vérifier son modèle sur une image avant de
lancer un `sync` complet.

⚠ **Les modèles OCR spécialisés recopient les consignes.** Une formulation
antérieure du prompt par défaut, qui détaillait les champs en prose, revenait
intégralement dans le champ `description` de `glm-ocr` — et aurait donc été
indexée pour chaque image. Le prompt par défaut est volontairement court et sans
énumération ; le vocabulaire de cadrage vit dans les descriptions du schéma JSON
(`descriptionSchema`), qu'un fournisseur applique au lieu de le donner à lire au
modèle. Toute personnalisation via `prompt:` doit respecter la même discipline.

Un « titre » anormalement long (au-delà de `vision.MaxTitleRunes`, 120) est
automatiquement rétrogradé en tête de description plutôt que de devenir un titre
markdown monstrueux.

## Sécurité

- Résolution de chemins **strictement confinée** au répertoire du fichier
  indexé : chemins absolus et traversées `../` refusés, quelle que soit la
  destination écrite dans le document.
- **Aucun appel réseau** depuis l'ingestion : les images distantes sont ignorées
  par défaut (`imagetext.WithHTTPFetcher` existe mais n'est pas câblé côté CLI).
- Taille d'image bornée **avant** lecture complète (`io.LimitReader` à N+1) et,
  pour les data-URI, avant même le décodage base64.
- Les data-URI restent retirés des extraits rendus (`StripDataURL`) : la
  description, elle, est du texte ordinaire et survit.

## Tolérance aux pannes

L'enrichissement est *best-effort* par construction : une image irrésolvable
(fichier absent, hors du répertoire de base) ou une description en échec
(fournisseur indisponible) laisse l'image avec son seul texte alternatif et
**l'indexation du document continue**. Seule une erreur de contexte
(annulation, dépassement du délai de 2 h de la tâche) fait échouer le document.

## Télémétrie

Avec un `MeterProvider` OpenTelemetry installé (voir
[docs/architecture.md](architecture.md#observabilité-telemetry)) :

| Instrument | Attributs | Sens |
|---|---|---|
| `amoxtli.vision.descriptions` | `outcome` = `ok` / `error` / `rejected` | descriptions produites ; `rejected` = refusée avant tout appel (image trop grande, type non supporté) |
| `amoxtli.vision.duration` | — | latence des appels au modèle (les hits de cache n'y figurent pas) |
| `amoxtli.vision.cache.lookups` | `cache_result` = `hit` / `miss` | efficacité du cache de descriptions |

`CachingDescriber.Stats()` donne les mêmes compteurs hits/misses sans OTel.

## Réafficher les images (MCP)

Les sections précédentes rendent les images *cherchables*. Un **blob store** les
rend *réaffichables* : sans lui, la `source` d'une image pointe vers un fichier
que seul un agent co-localisé peut ouvrir (et qui a pu bouger), et les images
embarquées disparaissent des extraits rendus.

### URI interne

Les images stockées sont adressées par `amoxtli://images/<hash>`, où `<hash>`
est le `sha256` hexadécimal de leur contenu. La propriété décisive tient au
schéma : `StripDataURL` ne retire que les destinations `data:`, donc cette
destination-là **survit** au rendu d'un extrait. L'agent voit la référence juste
à côté du texte de la description et sait quoi demander.

Le contenu est adressé par son hash : la même image rencontrée deux fois — comme
fichier puis embarquée dans un document, ou partagée par deux documents — n'est
stockée qu'une fois.

### Backends

| Backend | Paquet | Stockage | Pour |
|---|---|---|---|
| `fs` | `blob/fs` | `<dir>/<2 hexits>/<hash>` + sidecar `<hash>.json` (type média), écriture atomique | espaces de travail SQLite : garde les octets hors d'un fichier que bleve et sqlite-vec côtoient déjà |
| `database` | `blob/gorm` | table `blobs`, **SQLite et PostgreSQL avec le même code** | déploiement « tout PostgreSQL » : une seule base, un seul serveur à sauvegarder |

Les deux passent la même [suite de conformance](../blob/testsuite) — `blob/gorm`
étant exécuté sur SQLite *et* sur PostgreSQL, puisque seul un run sur les deux
prouve que le mapping `[]byte` → `BLOB`/`bytea` est portable.

Côté CLI :

```yaml
images:
  # auto (défaut) : "database" si le magasin est postgres, "fs" sinon.
  # "none" désactive. Avec auto, le stockage suit converter.vision.enabled ;
  # nommer un backend explicitement l'active à lui seul.
  store: auto
  path: blobs        # backend fs, relatif au répertoire .amoxtli
  max_size: 0        # 0 = défaut (10 Mio)
```

Côté bibliothèque, `amoxtli.WithBlobStore(store)` suffit : il alimente
l'enrichissement des images embarquées, permet au nettoyage de ramasser les
blobs orphelins et fait voyager les images avec les documents dans les
sauvegardes. Les convertisseurs reçoivent le leur explicitement
(`convvision.WithBlobStore`, `pandoc.WithBlobStore`) — le même, en pratique.

### En ligne de commande

```bash
amoxtli image list                          # hash, type média, taille, total
amoxtli image get <hash> -o schema.png      # hash nu ou URI amoxtli://images/<hash>
amoxtli image get <hash> > schema.png       # redirections et pipes fonctionnent
amoxtli --json image get <hash>             # métadonnées seules, pas les octets
```

`image get` refuse d'écrire du binaire dans un terminal (convention `curl`) :
utiliser `-o <fichier>`, rediriger, ou forcer avec `-o -`.

### Outil `fetch_image` et resources

Le serveur MCP expose les images de deux façons, **uniquement lorsqu'un blob
store est configuré** (même logique conditionnelle que `iterative` ou le
grounding) :

- **`fetch_image`** — voie principale, supportée par tous les clients MCP. Prend
  l'URI trouvée dans le contenu d'une section (ou le hash nu) et renvoie un bloc
  image (base64 + type média). La description de l'outil `search` est complétée
  pour signaler ces références à l'agent.
- **Resources** — voie idiomatique : un template `amoxtli://images/{hash}` que
  les clients sachant déréférencer les URI utilisent directement.

Garde-fou commun : le serveur refuse de servir un blob dont le type média n'est
pas `image/*`. Le store est adressé par hash de contenu, il ne doit pas pouvoir
se transformer en point d'exfiltration de fichiers arbitraires.

### Cycle de vie

- **Écriture** — pendant la conversion ou l'enrichissement, donc *avant*
  l'enregistrement du document. Un échec en aval peut laisser des blobs
  orphelins : c'est accepté, le ramasse-miettes les récupère. Le stockage est
  best-effort — un store indisponible coûte la référence, jamais la description
  qu'on vient de payer.
- **Suppression** — jamais synchrone : une même image peut être partagée par
  plusieurs documents via son hash.
- **Ramasse-miettes** — la tâche de nettoyage (`amoxtli cleanup`) calcule
  l'ensemble des blobs vivants, puis supprime ceux qui n'en font pas partie.
  Cet ensemble vient d'un **index de références** (`ingest.BlobReferenceLister`)
  maintenu par le magasin : une requête indexée au lieu d'un parcours complet du
  corpus (voir ci-dessous). Un magasin qui n'expose pas cette capacité retombe
  automatiquement sur le scan des contenus.

### Index de références (`document_blobs`)

Le magasin gorm tient une table `document_blobs (document_id, hash)` : une ligne
par couple document/image. L'ensemble vivant du ramasse-miettes devient un
`SELECT DISTINCT hash` indexé, au lieu de lire le contenu de chaque document.

C'est un **état dérivé**, donc exposé au risque classique de divergence — et une
divergence ici ne se voit pas : elle supprime des images vivantes. Trois choix le
contiennent :

1. **Écriture dans la même transaction que le document.** `SaveDocuments` est le
   point d'étranglement unique de toutes les écritures (ingestion, réindexation,
   restauration de snapshot) ; l'index ne peut donc ni survivre à une écriture
   annulée, ni manquer une écriture validée.
2. **Suppression explicite** à la réindexation et à la suppression d'un document,
   doublée d'une clé étrangère `ON DELETE CASCADE` — un document retiré par un
   chemin imprévu ne laisse pas de références fantômes.
3. **Test différentiel** : `TestBlobReferencesMatchContentScan` compare, hash pour
   hash, ce que renvoie l'index et ce que donne `blob.ScanHashes` sur les
   contenus stockés. C'est ce test qui empêche les deux implémentations de
   diverger silencieusement, et le scan reste le comportement de référence.

Attention à un piège si vous écrivez une variante en SQL : `Document.Content` est
un `[]byte`, donc un `BLOB` en SQLite. Un `content LIKE '%…%'` y renvoie **zéro
ligne sans erreur** (il faut `CAST(content AS TEXT)` ou `instr`), alors que la
même requête fonctionne sur le `bytea` de PostgreSQL. Un ramasse-miettes écrit
ainsi et validé sur PostgreSQL effacerait tout le stockage d'images en SQLite.
- **Sauvegarde** — `amoxtli backup` inclut les blobs (partie `blobs-v1`) à côté
  des documents et de l'index : restaurer les documents sans eux laisserait des
  références mortes. Le snapshot est écrit contre l'interface, donc portable
  entre backends (`fs` ↔ `database`). En PostgreSQL, la sauvegarde SQL native du
  serveur couvre déjà la table `blobs` (voir [postgres.md](postgres.md)).

## Hors périmètre

- Embeddings multimodaux (CLIP…) et recherche image-par-image ;
- description des images distantes (`http(s)`) pendant l'ingestion ;
- vidéo et audio.
