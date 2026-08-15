# Accélération locale des embeddings — résumé exécutif

Mesures réalisées le 2026-08-14 sur la machine de développement. Ce document sert de brief
d'implémentation : il fixe la décision, les chiffres de référence et les pièges connus.

## Décision

Passer les embeddings d'Ollama à **llama.cpp compilé Vulkan, avec le modèle quantifié en Q8_0**,
via [yzma](https://github.com/hybridgroup/yzma) (bindings purego sur les `.so` de llama.cpp).

**Gain mesuré end-to-end : ×5,9** (0,30 → 1,78 documents/s sur des documents de ~488 tokens).

Deux pistes ont été évaluées puis écartées, voir « Ce qui ne marche pas ».

## Matériel cible

| | |
|---|---|
| CPU | Intel Core Ultra 7 265U (Arrow Lake-U), 12 cœurs / 14 threads, AVX2 + AVX-VNNI |
| iGPU | Intel Graphics (ARL), Vulkan via Mesa ANV 26.1.5, `uma: 1`, **`matrix cores: none`** |
| NPU | Intel AI Boost, `/dev/accel/accel0`, driver `intel_vpu`, `libze_intel_npu.so` 1.32.1 |
| OpenVINO | 2026.0 système |

L'absence d'unités matricielles (XMX) sur l'iGPU explique que le gain GPU soit modeste : les GEMM
tournent sur les EU génériques, et la mémoire est unifiée avec le CPU.

## Chiffres de référence

Modèle : `mxbai-embed-large` (BERT 335M, dim 1024, contexte 512). Métrique `pp512` en tokens/s,
mesurée avec `llama-bench` (build b10434).

| Quantification | Taille | CPU (12 threads) | Vulkan / iGPU |
|---|---:|---:|---:|
| F16 (ce qu'utilise Ollama) | 638 Mio | 388 | 530 |
| **Q8_0** | 341 Mio | 424 | **920** |
| Q4_0 | 189 Mio | 701 | 661 |

Débit end-to-end, 64 documents / 31 230 tokens, via `llama-server --embeddings` :

| Configuration | docs/s | t/s |
|---|---:|---:|
| Ollama (situation actuelle) | 0,30 | 146 |
| mxbai F16 / Vulkan | 1,00 | 499 |
| **mxbai Q8_0 / Vulkan** | **1,78** | **867** |

Deux observations qui orientent les choix futurs :

- **Sur GPU, Q8_0 est l'optimum et Q4_0 régresse** (920 → 661 t/s). La charge est compute-bound :
  déquantifier l'int4 coûte plus que la bande passante économisée. Ne pas descendre sous Q8_0 sur GPU.
- **Sur CPU, l'ordre s'inverse** (424 → 701 t/s), grâce à AVX-VNNI. Un fallback CPU doit donc
  utiliser Q4_0, pas Q8_0 — ce n'est pas le même artefact selon la cible.

L'essentiel du ×5,9 vient de la quantification et du **batching** : Ollama ne batche pas les
requêtes `/v1/embeddings`, d'où ses 146 t/s. Le changement de backend seul ne vaut que +37 %.

## Ce qui ne marche pas

**Backend OpenVINO de llama.cpp — ne supporte pas les encodeurs.** Le build prébuilt officiel
(`llama-b10434-bin-ubuntu-openvino-2026.2.1-x64`) embarque son propre runtime, donc pas de conflit
avec l'OpenVINO système. Il fonctionne sur un décodeur (Llama-3.2-1B Q4_0, device CPU : 367 t/s en
pp128). Mais `mxbai-embed-large` échoue identiquement sur les trois devices :

```
CPU / GPU / NPU → failed to decode prompt batch, res = -3
```

Le modèle charge, le device est sélectionné, la rupture est au premier batch. Ce n'est ni le device
ni `n_ubatch` (testé explicitement). C'est la limite documentée du backend sur les modèles
d'embedding et de reranking.

**NPU — cassé indépendamment, et sans intérêt ici.** Même sur le décodeur validé par Intel :

```
[ NOT_FOUND ] Option 'NPU_COMPILER_DYNAMIC_QUANTIZATION' is not supported for current configuration
```

Décalage de version entre le compilateur NPU du runtime bundlé et le driver système. Réparable en
mettant à jour le driver, mais sans effet sur le besoin : les encodeurs resteraient non supportés.

Si le NPU redevient une priorité, la voie est **OpenVINO Model Server** (endpoints `/v3/embeddings`
et `/v3/rerank`, `--device /dev/accel`, `target_device: NPU`, `--pooling LAST`), pas llama.cpp.

## Notes d'implémentation

Point d'intégration : `amoxtli` consomme `llm.Client` de `github.com/bornholm/genai/llm` (v0.31.0),
décoré dans `llmx/` par `RetryClient` et `ObservableClient`. La méthode à couvrir est :

```go
Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error)
```

Deux stratégies possibles, par ordre d'invasivité croissante :

1. **`llama-server` en sidecar** — expose `/v1/embeddings` compatible OpenAI. Aucun code Go à
   écrire, on repointe la configuration. C'est ce qui a produit les chiffres ci-dessus.
2. **yzma en process** — supprime le saut HTTP et la sérialisation JSON. Implémenter un
   `llm.Client` adossé à yzma, derrière les décorateurs existants.

Pour la voie yzma, en suivant `examples/embeddings` :

- `ctxParams.Embeddings = 1`
- **`NUbatch >= ` la longueur max de chunk.** Contrainte dure côté llama.cpp :
  `GGML_ASSERT(cparams.n_ubatch >= n_tokens && "encoder requires n_ubatch >= n_tokens")`.
  Ce n'est pas une dégradation gracieuse, c'est un abort. Avec mxbai (contexte 512), fixer
  `NCtx = NUbatch = 512` suffit.
- `GetEmbeddingsSeq()` puis normalisation L2 manuelle — l'exemple ne la fait pas pour vous.
- yzma charge les `.so` au runtime : lui fournir le build **Vulkan**, pas le build CPU par défaut.
- Batcher les entrées. C'est la moitié du gain, et l'interface `Embeddings` reçoit déjà un `[]string`.

Le GGUF Q8_0 se régénère depuis le blob Ollama en une seconde :

```sh
llama-quantize <blob-mxbai-f16> mxbai-q8_0.gguf Q8_0 12
```

Le blob source est donné par `ollama show mxbai-embed-large --modelfile`. Attention : les blobs sous
`/usr/share/ollama/` ne sont pas lisibles par l'utilisateur, ceux sous `~/.ollama/` le sont.

## Reste à trancher

- **bge-m3 Q8_0 contre mxbai Q8_0** sur le harness SciFact. C'est le levier qualité principal, et
  la mécanique de mesure est maintenant en place. À faire avant de figer le choix du modèle.
- **Reranking** : non mesuré ici. Un cross-encoder est aussi un encodeur, donc mêmes contraintes
  (`n_ubatch`, pas d'OpenVINO) mais des séquences plus longues — donc un `NUbatch` plus grand et un
  profil de performance à établir séparément.
- Le contexte de 512 tokens de mxbai est une contrainte de découpage à garder en tête si le modèle change.
