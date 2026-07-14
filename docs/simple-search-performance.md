# Synthèse — Performance de la recherche simple (SciFact)

Ce document synthétise les résultats de l'évaluation de la **recherche simple**
d'amoxtli (`Codex.Search`) sur le benchmark **SciFact** (BEIR). Il compare trois
configurations — **lexical**, **hybride**, **hybride + reranking** — et en tire
les régimes d'usage où chacune est préférable.

> Périmètre : ces mesures portent sur `Search` (un seul tour de récupération :
> fusion RRF + reranking optionnel). Elles **n'incluent pas** la boucle
> agentique `SearchIterative` (HyDE, grounding, re-récupération reformulée), qui
> fait l'objet d'une évaluation distincte.

## 1. Méthodologie

- **Jeu de données** : [SciFact](https://github.com/allenai/scifact) au format
  BEIR (`corpus.jsonl` / `queries.jsonl` / `qrels/test.tsv`), chargé par
  [`eval/beir`](../eval/beir). SciFact est un cas où l'**écart de vocabulaire
  requête↔document** est réel — les affirmations scientifiques ne partagent pas
  le lexique des abstracts — donc là où la récupération sémantique est censée
  aider par rapport à BM25.
- **Test** : `eval.TestEvaluateBEIR`, piloté par variables d'environnement (voir
  [docs/evaluation.md](evaluation.md)).
- **Métriques** : Recall@k, nDCG@k et MRR, aux coupures k ∈ {1, 3, 5, 10}.
- **Corpus évalué** : sous-échantillon **gold-aware** de **1000 documents /
  300 requêtes** (toutes les requêtes conservées restent répondables — leurs
  documents pertinents sont toujours dans le corpus). Le sous-échantillonnage
  est **déterministe**, donc les trois configurations partagent exactement le
  même corpus et le même jeu de requêtes → comparaison à isopérimètre.

### Configuration des composants

| Composant | Choix |
|---|---|
| Index lexical | bleve (BM25) |
| Index vectoriel | sqlite-vec + `mxbai-embed-large` (1024 dim, Ollama local), chunks de 256 mots |
| Fusion | RRF, poids bleve 0.50 / vecteur 0.50 |
| Reranker | `mistral-small` (endpoint distant OpenAI-compatible), pool de 100 candidats, budget 3000 mots |
| Désactivé | HyDE, Judge, grounding, re-récupération (mode `Search` pur) |

## 2. Résultats (1000 docs / 300 requêtes)

| Métrique | Lexical | Hybride | Hybride + Rerank |
|---|---|---|---|
| **MRR** | 0.774 | 0.776 | **0.806** |
| Recall@1 | 0.676 | 0.638 | **0.718** |
| nDCG@1 | 0.710 | 0.673 | **0.760** |
| Recall@3 | 0.792 | **0.845** | 0.824 |
| nDCG@3 | 0.743 | 0.775 | **0.799** |
| Recall@5 | 0.845 | **0.908** | 0.852 |
| nDCG@5 | 0.762 | 0.793 | **0.809** |
| Recall@10 | 0.894 | **0.958** | 0.883 |
| nDCG@10 | 0.797 | 0.817 | **0.819** |

*(En gras : meilleure valeur par ligne.)*

### Référence : lexical sur le corpus complet

Sur le **corpus SciFact complet** (5183 docs / 300 requêtes), le lexical seul
obtient **nDCG@10 = 0.679** (MRR 0.650, Recall@10 0.801). C'est cohérent avec la
baseline BM25 publiée du papier BEIR (**nDCG@10 ≈ 0.665**) : le pipeline lexical
d'amoxtli est correctement calibré. Les valeurs du tableau ci-dessus sont plus
élevées uniquement parce que le sous-échantillon à 1000 docs contient moins de
distracteurs ; **les écarts relatifs entre configurations restent valides**.

## 3. Analyse

### Hybride vs lexical — gain de rappel profond

L'ajout du vecteur fait nettement progresser le **rappel aux coupures
profondes** : **Recall@10 passe de 0.894 à 0.958 (+6,4 pts)**, Recall@5 de 0.845
à 0.908 (+6,3 pts). La recherche sémantique comble l'écart de vocabulaire propre
à SciFact et ramène davantage de bons documents dans le top-10.

En contrepartie, l'hybride **dégrade légèrement la tête de classement** :
Recall@1 −3,8 pts, nDCG@1 −3,7 pts. La fusion élargit le vivier de candidats
pertinents mais ne le **trie pas finement** : le meilleur document n'est pas
toujours propulsé en première position.

### Reranking vs hybride — gain de précision de tête

Le reranker LLM inverse ce compromis en **réordonnant** le vivier :

- **MRR : +3,0 pts** (0.776 → 0.806)
- **Recall@1 : +8,0 pts** (0.638 → 0.718)
- **nDCG@1 : +8,7 pts** (0.673 → 0.760)

Il restaure — et dépasse — la précision de tête que l'hybride avait sacrifiée.

**Mais** le reranking **réduit le rappel profond** : Recall@10 retombe à 0.883,
**sous l'hybride (0.958) et même sous le lexical (0.894)**. Mécanisme : le
reranker réordonne ~100 candidats et n'en renvoie que 10 ; imparfait, il
rétrograde parfois un document réellement pertinent qui était en position 6–10
par fusion, l'éjectant du top-10. Il **concentre la pertinence en tête au prix
de la queue**.

Le `nDCG@10` est quasi identique entre hybride et rerank (0.817 vs 0.819) : le
gain en tête compense exactement la perte en profondeur.

## 4. Conclusions

| Régime d'usage | Configuration recommandée | Pourquoi |
|---|---|---|
| **RAG / top-1..3** (on n'injecte que quelques passages) | **Hybride + Rerank** | Meilleurs MRR / nDCG@1..5 ; la pertinence est concentrée en tête |
| **Rappel exhaustif** (récupérer un maximum de pertinents en top-10) | **Hybride** sans rerank | Meilleur Recall@5/@10 ; aucune perte de queue |
| **Lexical seul** | Fallback sans service d'embeddings | Baseline solide, mais dominée dès qu'un backend vectoriel est disponible |

**En une phrase** : sur SciFact, l'hybride achète du **rappel**, le reranker
achète de la **précision de tête** — et pour un usage RAG (le cas courant),
l'empilement hybride + reranker est la meilleure configuration de recherche
simple.

## 5. Guide pratique — quelle configuration choisir ?

*Cette section s'adresse à un public non spécialiste. Les trois configurations
répondent à la même question — « trouver les bons documents pour une
requête » — mais ne font pas le même compromis.*

### Les trois configurations en une image

Imaginez que vous cherchez un livre dans une bibliothèque.

- **Lexical** = chercher par **mots-clés exacts**. Rapide, sans électricité,
  mais si vous demandez « comment démarrer une voiture » et que le livre parle
  de « mettre le contact d'un véhicule », vous risquez de passer à côté : ce
  sont les mêmes idées, pas les mêmes mots.
- **Hybride** = ajouter un bibliothécaire qui comprend le **sens**, pas seulement
  les mots. Il ramène plus de bons livres, même formulés différemment. En
  revanche, dans la pile qu'il vous tend, le meilleur n'est pas toujours sur le
  dessus.
- **Hybride + reranking** = ce même bibliothécaire **relit** ensuite les
  quelques livres retenus et les **classe du plus au moins pertinent**. Le
  meilleur se retrouve en haut — au prix d'un peu de temps, et il peut écarter à
  tort un livre qui était tout en bas de la pile.

### Quelle configuration pour quel besoin ?

| Votre besoin | Configuration | Pourquoi |
|---|---|---|
| **Un assistant qui répond à partir de 1 à 3 documents** (chatbot, RAG) | **Hybride + reranking** | Ce qui compte, c'est que le tout premier résultat soit le bon. C'est exactement ce que le reranking optimise. |
| **Lister un maximum de documents pertinents** (revue documentaire, exploration) | **Hybride** (sans reranking) | On veut n'en manquer aucun dans le top-10 ; le reranking risquerait d'en écarter. |
| **Pas de service d'IA disponible / contrainte de coût ou de confidentialité** | **Lexical** | Aucun modèle requis, aucune donnée envoyée à l'extérieur, résultats corrects. Idéal comme repli. |
| **Recherche par identifiant, code, référence exacte** | **Lexical** | Les mots-clés exacts sont précisément le point fort du lexical. |

> **Règle simple** : si votre application **n'utilise que les tout premiers
> résultats**, prenez **hybride + reranking**. Si elle a besoin d'une **liste
> large et complète**, prenez **hybride seul**. Si vous **ne pouvez pas (ou ne
> voulez pas) faire appel à un modèle d'IA**, restez en **lexical**.

### En code

Les trois configurations diffèrent seulement par les options passées à
`amoxtli.New`. Le reste (indexation, appel `Search`) est identique.

```go
// 1) LEXICAL — mots-clés uniquement, aucun service d'IA.
codex, _ := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1.0},
    ),
    amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge(), // pas de client LLM
)

// 2) HYBRIDE — on ajoute un index vectoriel (compréhension du sens).
//    Les poids 0.5 / 0.5 équilibrent mots-clés et sémantique dans la fusion.
codex, _ := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.5},
        amoxtli.Indexer{ID: "vector", Index: vectorIdx, Weight: 0.5}, // sqlite-vec + embeddings
    ),
    amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge(),
)

// 3) HYBRIDE + RERANKING — on relit et reclasse les candidats avec un LLM.
codex, _ := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.5},
        amoxtli.Indexer{ID: "vector", Index: vectorIdx, Weight: 0.5},
    ),
    amoxtli.WithLLMClient(llmClient), // requis par le reranker
    amoxtli.WithReranking(),
    amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge(),
)

// L'usage est le même dans les trois cas :
results, _ := codex.Search(ctx, "comment démarrer une voiture",
    amoxtli.WithSearchMaxResults(5))
```

### À propos de HyDE et du Judge

Les exemples ci-dessus **désactivent systématiquement HyDE et le Judge**
(`WithDisableHyDE()`, `WithDisableJudge()`). C'est un choix de **périmètre de
mesure — pas une recommandation de les éviter** : ce document isole la couche de
récupération pour attribuer proprement chaque gain (hybride, reranking). HyDE et
le Judge appartiennent à la couche « agentique » et sont mesurés séparément.

Ce sont deux étages LLM **optionnels et utiles** dans les bons cas :

- **HyDE** (transformateur de *requête*) : le LLM rédige un document hypothétique
  à partir de la requête, et c'est *ce texte* qui est embarqué pour la recherche
  vectorielle — utile pour les requêtes courtes ou à fort écart de vocabulaire.
  Il ne s'applique qu'aux index **sémantiques** (vectoriels) et est ignoré en
  lexical pur. Coût : un appel LLM par requête ; un hypothétique halluciné peut
  dégrader la recherche, et l'embedder doit tolérer des textes longs.
- **Judge** (transformateur de *résultats*) : le LLM **filtre** les résultats non
  pertinents (il en retire — contrairement au reranker qui réordonne). Utile pour
  ne pas injecter de passages hors-sujet en aval (RAG). Coût : appels LLM ;
  risque de sur-filtrer. Il est automatiquement retiré du pipeline quand le
  *grounding* est actif.

**En pratique** : sans service LLM, ou pour une recherche purement lexicale,
laissez-les désactivés. Dès que vous disposez d'un LLM et de requêtes en langage
naturel sur un index vectoriel, **HyDE mérite d'être essayé** ; activez le
**Judge** (ou le reranking / grounding) quand la précision des passages transmis
compte. Pour les activer, il suffit de fournir un client LLM
(`WithLLMClient(...)`) et de **ne pas** appeler les options `WithDisable...`
correspondantes.

## 6. Limites

- Mesures sur un **sous-échantillon** de 1000 documents ; les valeurs absolues
  sont optimistes par rapport au corpus complet (les écarts relatifs, eux, sont
  robustes).
- Un seul jeu (**SciFact**, anglais, domaine scientifique). Les jeux à faible
  écart de vocabulaire (SQuAD-like) favorisent davantage le lexical ; les
  conclusions sur l'apport sémantique ne s'y transposent pas mécaniquement.
- Reranker = `mistral-small` ; un modèle plus capable réduirait probablement la
  perte de rappel profond observée.
- Périmètre `Search` uniquement — voir l'évaluation `SearchIterative` pour
  l'apport de la boucle agentique (HyDE + grounding + re-récupération).
