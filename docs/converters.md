# Convertisseurs de fichiers

Binaires externes requis selon le convertisseur : `pandoc` (`convert/pandoc`), `libreoffice` (`convert/libreoffice`). `convert/genai` utilise une API d'extraction LLM/OCR (Mistral OCR, Marker).

Le convertisseur se branche via `amoxtli.WithFileConverter(...)` ; l'ingestion route alors automatiquement tout fichier non-markdown à travers lui avant parsing et indexation. `convert.NewRouted(...)` combine plusieurs convertisseurs par extension.

L'ingestion étant asynchrone, `IndexFile` renvoie un `task.ID` : on suit la progression et les messages d'étape (« converting document », « parsing document », « indexing document »…) via `codex.TaskState(ctx, id)`.

Voir [`example/convert`](../example/convert/main.go) (implémente aussi un `convert.Converter` minimal, sans binaire externe).

## Images

`convert/vision` décrit les fichiers image autonomes (`.png`, `.jpg`, `.jpeg`, `.webp`, `.gif`) via un LLM vision et produit un markdown (titre, description, texte visible) porteur d'un frontmatter `type: image`. Il doit être placé **avant** `convert/genai` dans `convert.NewRouted(...)` : une extension image explicitement routée vers un modèle vision prime sur la même extension listée côté OCR.

Les images *embarquées* dans les documents sont traitées en amont du parsing par `markdown/imagetext.Enrich` (option `amoxtli.WithImageEnrichment`), indépendamment du convertisseur qui a produit le markdown. Ce que chaque convertisseur transmet :

| Convertisseur | Images embarquées | Remarque |
|---|---|---|
| markdown natif | oui | chemins relatifs résolus sous le répertoire du fichier indexé, data-URI décodés |
| `convert/pandoc` | oui, sur option | `WithInlineMedia(maxBytes)` ajoute `--extract-media` et réécrit les médias extraits en data-URI ; sans l'option, les médias sont perdus (comportement historique) |
| `convert/libreoffice` | idem pandoc | il délègue la conversion à pandoc |
| `convert/genai` (OCR) | dépend du provider | Mistral OCR et Marker renvoient du markdown pouvant contenir des images en data-URI selon la configuration du provider ; aucun traitement spécifique n'est nécessaire côté amoxtli, la phase d'enrichissement les couvre telles quelles. **Non vérifié sur un échantillon réel** : le comportement exact par provider reste à confirmer |
| `convert/vision` | sans objet | le fichier *est* l'image |

L'inlining pandoc est opt-in car il gonfle la source indexée avec du base64 : la CLI ne l'active que lorsque `converter.vision.embedded.enabled` est vrai, c'est-à-dire quand quelque chose en aval lit réellement les images.
