# Convertisseurs de fichiers

Binaires externes requis selon le convertisseur : `pandoc` (`convert/pandoc`), `libreoffice` (`convert/libreoffice`). `convert/genai` utilise une API d'extraction LLM/OCR (Mistral OCR, Marker).

Le convertisseur se branche via `amoxtli.WithFileConverter(...)` ; l'ingestion route alors automatiquement tout fichier non-markdown à travers lui avant parsing et indexation. `convert.NewRouted(...)` combine plusieurs convertisseurs par extension.

L'ingestion étant asynchrone, `IndexFile` renvoie un `task.ID` : on suit la progression et les messages d'étape (« converting document », « parsing document », « indexing document »…) via `codex.TaskState(ctx, id)`.

Voir [`example/convert`](../example/convert/main.go) (implémente aussi un `convert.Converter` minimal, sans binaire externe).
