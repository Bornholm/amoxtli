#!/usr/bin/env python3
"""Download a Hugging Face extractive-QA dataset and export it as SQuAD JSON.

The output format is the SQuAD v1 JSON schema consumed by the amoxtli evaluation
loader (eval/hfqa). Any dataset sharing the SQuAD column schema works — SQuAD
(English), squad_es (Spanish), PIAF/FQuAD (French), MLQA, XQuAD — so this single
script turns them into a passage-retrieval benchmark.

Requires the `datasets` package (installed by the Makefile's eval target into a
local virtualenv).

Example:
    python hf_to_squad.py --dataset rajpurkar/squad --config plain_text \\
        --split validation --max-rows 3000 --out .eval-data/squad-en.json
"""

import argparse
import collections
import json
import sys


def convert(rows, dataset_name):
    """Group flat SQuAD rows into the nested {data:[{title,paragraphs:[...]}]} schema."""
    articles = collections.OrderedDict()
    for i, row in enumerate(rows):
        context = row.get("context")
        question = row.get("question")
        if not context or not question:
            continue
        title = row.get("title") or "untitled"
        qid = row.get("id") or f"{dataset_name}-{i}"
        answers = row.get("answers") or {}
        texts = answers.get("text", []) if isinstance(answers, dict) else []
        qa = {"question": question, "id": str(qid), "answers": [{"text": t} for t in texts]}
        articles.setdefault(title, collections.OrderedDict()).setdefault(context, []).append(qa)

    data = [
        {"title": title, "paragraphs": [{"context": ctx, "qas": qas} for ctx, qas in paras.items()]}
        for title, paras in articles.items()
    ]
    return {"version": "hf-export", "data": data}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dataset", required=True, help="Hugging Face dataset id (e.g. rajpurkar/squad)")
    ap.add_argument("--config", default="", help="dataset config/subset (empty for the default)")
    ap.add_argument("--split", default="validation", help="split to export (e.g. validation, train)")
    ap.add_argument("--out", required=True, help="output SQuAD JSON path")
    ap.add_argument("--max-rows", type=int, default=0, help="cap the number of rows (0 = all)")
    args = ap.parse_args()

    try:
        from datasets import load_dataset
    except ImportError:
        sys.exit("error: the 'datasets' package is required (pip install datasets)")

    config = args.config or None
    print(f"downloading {args.dataset} (config={config or 'default'}, split={args.split}) from Hugging Face...")
    ds = load_dataset(args.dataset, config, split=args.split)

    if args.max_rows and args.max_rows < len(ds):
        ds = ds.select(range(args.max_rows))

    squad = convert(ds, args.dataset)

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(squad, f, ensure_ascii=False)

    n_titles = len(squad["data"])
    n_para = sum(len(a["paragraphs"]) for a in squad["data"])
    n_q = sum(len(p["qas"]) for a in squad["data"] for p in a["paragraphs"])
    print(f"wrote {args.out}: {n_titles} titles, {n_para} passages, {n_q} questions")


if __name__ == "__main__":
    main()
