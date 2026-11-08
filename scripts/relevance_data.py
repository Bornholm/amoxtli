#!/usr/bin/env python3
"""Build training and test sets for an indecis relevance model.

The model reads (query, passage) pairs and answers two questions:

- pertinent (noul): the passage answers the query or brings context needed to
  answer it. Labelled on single passages only.
- suffisance (score: aucune, partielle, complete): the passage, or several
  passages joined, are enough to answer the query without outside knowledge.
  Labelled on single passages and on joined passages.

Sources, all read from the Hugging Face cache:

- SQuAD v1.1 train (English): the paragraph of a question is pertinent and
  complete; other paragraphs of the same article that do not contain the answer
  are hard negatives.
- PIAF (French), minus the articles of the amoxtli evaluation file
  (.eval-data/squad-fr.json): same as SQuAD.
- HotpotQA fullwiki train (English, two-hop): each of the two supporting
  paragraphs is pertinent but partial, both together are complete; the eight
  distractors retrieved by TF-IDF are hard negatives.

Test sets come from data the training never reads: SQuAD validation, HotpotQA
validation (the source of the BEIR HotpotQA test queries) and the PIAF articles
of the evaluation file. SciFact and NFCorpus stay out entirely: they are the
benchmarks of the end-to-end comparison.

Usage:
    uv run --with pyarrow python scripts/relevance_data.py --out ~/.cache/indecis/relevance
"""

import argparse
import glob
import json
import os
import random

import pyarrow.parquet as pq

HUB = os.path.expanduser("~/.cache/huggingface/hub")


def parquet(pattern):
    files = sorted(glob.glob(os.path.join(HUB, pattern)))
    if not files:
        raise SystemExit(f"missing dataset files: {pattern}")
    rows = []
    for f in files:
        rows.extend(pq.read_table(f).to_pylist())
    return rows


def write(path, examples):
    with open(path, "w", encoding="utf-8") as f:
        for e in examples:
            f.write(json.dumps(e, ensure_ascii=False) + "\n")
    print(f"{path}: {len(examples)}")


def example(query, text, family, pertinent=None, suffisance=None):
    labels = {}
    if pertinent is not None:
        labels["pertinent"] = pertinent
    if suffisance is not None:
        labels["suffisance"] = suffisance
    return {"context": query, "text": text, "labels": labels, "family": family}


def squad_examples(rows, rng, n_questions, family, n_hard=1, n_random=1):
    """One positive, n_hard negatives from the same article and n_random from
    another article per question."""
    by_title = {}
    for r in rows:
        by_title.setdefault(r["title"], set()).add(r["context"])
    titles = sorted(by_title)
    paragraphs = {t: sorted(ps) for t, ps in by_title.items()}

    rng.shuffle(rows)
    out = []
    for r in rows[:n_questions]:
        q, para = r["question"].strip(), r["context"]
        answers = [a.lower() for a in r["answers"]["text"] if a.strip()]
        out.append(example(q, para, family, True, "complete"))

        def negative(p):
            low = p.lower()
            return p != para and not any(a in low for a in answers)

        hard = [p for p in paragraphs[r["title"]] if negative(p)]
        for p in rng.sample(hard, min(n_hard, len(hard))):
            out.append(example(q, p, family, False, "aucune"))
        for _ in range(n_random):
            t = rng.choice(titles)
            if t == r["title"]:
                continue
            p = rng.choice(paragraphs[t])
            if negative(p):
                out.append(example(q, p, family, False, "aucune"))
    return out


def hotpot_paragraph(title, sentences):
    return title + "\n" + "".join(sentences).strip()


def hotpot_examples(rows, rng, n_questions, family, pointwise=True, joined=True):
    rng.shuffle(rows)
    out = []
    for r in rows:
        if n_questions <= 0:
            break
        ctx = dict(zip(r["context"]["title"], r["context"]["sentences"]))
        gold_titles = list(dict.fromkeys(r["supporting_facts"]["title"]))
        if len(gold_titles) != 2 or any(t not in ctx for t in gold_titles):
            continue
        gold = [hotpot_paragraph(t, ctx[t]) for t in gold_titles]
        distractors = [hotpot_paragraph(t, s) for t, s in ctx.items() if t not in gold_titles and "".join(s).strip()]
        if len(distractors) < 3:
            continue
        n_questions -= 1
        q = r["question"].strip()

        if pointwise:
            for g in gold:
                out.append(example(q, g, family, True, "partielle"))
            for d in rng.sample(distractors, 2):
                out.append(example(q, d, family, False, "aucune"))

        if joined:
            # Sets of passages, as the evidence evaluator sees them: both
            # supporting paragraphs (complete), one of them (partial), none.
            d1, d2 = rng.sample(distractors, 2)
            both = gold + ([d1] if rng.random() < 0.5 else [])
            rng.shuffle(both)
            one = [rng.choice(gold), d1]
            rng.shuffle(one)
            out.append(example(q, "\n\n".join(both), family + "-set", None, "complete"))
            out.append(example(q, "\n\n".join(one), family + "-set", None, "partielle"))
            out.append(example(q, "\n\n".join([d1, d2]), family + "-set", None, "aucune"))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--amoxtli-eval", default=os.path.join(os.path.dirname(__file__), "..", ".eval-data"))
    ap.add_argument("--squad", type=int, default=5000, help="SQuAD training questions")
    ap.add_argument("--hotpot", type=int, default=3000, help="HotpotQA training questions")
    ap.add_argument("--test", type=int, default=500, help="questions per test set")
    ap.add_argument("--seed", type=int, default=1)
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    rng = random.Random(args.seed)

    # PIAF: the evaluation file holds the first rows of the train split; keep
    # its articles for testing and train on the others.
    with open(os.path.join(args.amoxtli_eval, "squad-fr.json"), encoding="utf-8") as f:
        eval_titles = {a["title"] for a in json.load(f)["data"]}
    piaf = parquet("datasets--etalab-ia--piaf/snapshots/*/plain_text/train-*.parquet")
    piaf_train = [r for r in piaf if r["title"] not in eval_titles]
    piaf_test = [r for r in piaf if r["title"] in eval_titles]
    print(f"PIAF: {len(piaf_train)} training questions, {len(piaf_test)} held out")

    squad_train = parquet("datasets--rajpurkar--squad/snapshots/*/plain_text/train-*.parquet")
    squad_val = parquet("datasets--rajpurkar--squad/snapshots/*/plain_text/validation-*.parquet")
    hotpot_train = parquet("datasets--hotpotqa--hotpot_qa/snapshots/*/fullwiki/train-*.parquet")
    hotpot_val = parquet("datasets--hotpotqa--hotpot_qa/snapshots/*/fullwiki/validation-*.parquet")

    # The BEIR HotpotQA queries must never be trained on.
    beir_ids = set()
    for split in ("test", "dev"):
        path = os.path.join(args.amoxtli_eval, "hotpotqa", "qrels", split + ".tsv")
        if os.path.exists(path):
            with open(path) as f:
                beir_ids.update(line.split("\t")[0] for line in f)
    before = len(hotpot_train)
    hotpot_train = [r for r in hotpot_train if r["id"] not in beir_ids]
    print(f"HotpotQA: {before - len(hotpot_train)} training questions dropped (in BEIR test/dev)")

    train = []
    train += squad_examples(squad_train, rng, args.squad, "squad")
    train += squad_examples(piaf_train, rng, len(piaf_train), "piaf", n_hard=2)
    train += hotpot_examples(hotpot_train, rng, args.hotpot, "hotpot")
    rng.shuffle(train)
    write(os.path.join(args.out, "train.jsonl"), train)

    write(os.path.join(args.out, "test-squad.jsonl"), squad_examples(squad_val, rng, args.test, "squad"))
    write(os.path.join(args.out, "test-piaf.jsonl"), squad_examples(piaf_test, rng, args.test, "piaf"))
    write(os.path.join(args.out, "test-hotpot.jsonl"), hotpot_examples(hotpot_val, rng, args.test, "hotpot", joined=False))
    write(os.path.join(args.out, "test-hotpot-set.jsonl"), hotpot_examples(hotpot_val, rng, args.test, "hotpot", pointwise=False))


if __name__ == "__main__":
    main()
