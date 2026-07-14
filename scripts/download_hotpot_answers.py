#!/usr/bin/env python3
"""Dump HotpotQA gold answers as a JSON array of {"_id", "answer"} objects.

The canonical CMU host (curtis.ml.cmu.edu) that the HotpotQA README links to is
frequently offline, so this pulls the same dev/validation answers from the
Hugging Face `hotpot_qa` dataset instead. The output is the shape consumed by
beir.LoadHotpotAnswers, joined onto a BEIR hotpotqa run (matching question ids)
to score the generation (reader) EM/F1.

Usage:
    python scripts/download_hotpot_answers.py --config fullwiki --split validation \
        --out .eval-data/hotpot_answers.json
"""
import argparse
import json

from datasets import load_dataset


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", default="hotpotqa/hotpot_qa")
    ap.add_argument("--config", default="fullwiki", choices=["fullwiki", "distractor"])
    ap.add_argument("--split", default="validation")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    try:
        ds = load_dataset(args.dataset, args.config, split=args.split)
    except Exception:
        # Older HF datasets ship a loading script and require opting in.
        ds = load_dataset(
            args.dataset, args.config, split=args.split, trust_remote_code=True
        )

    items = []
    for row in ds:
        qid = row.get("id") or row.get("_id")
        answer = row.get("answer")
        if qid and answer:
            items.append({"_id": qid, "answer": answer})

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(items, f, ensure_ascii=False)
    print(f"wrote {len(items)} answers to {args.out}")


if __name__ == "__main__":
    main()
