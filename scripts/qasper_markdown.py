#!/usr/bin/env python3
"""Turns QASPER papers into Markdown documents and evidence-labeled questions.

Each paper becomes <out>/<split>/papers/<paper_id>.md: the title as "#", the
abstract and every section as headings whose depth follows QASPER's
"Parent ::: Child" names. Each answerable question with text evidence becomes
one line of <out>/<split>/questions.jsonl, with the union of the evidence
paragraphs of all its annotators. Evidence pointing to figures and tables
("FLOAT SELECTED") is dropped, and so are questions left without evidence.

Usage: qasper_markdown.py -in qasper-test-v0.3.json -split test -out DIR
"""

import argparse
import json
import os
import re


def clean(text):
    # A paragraph must stay one Markdown paragraph: no heading or list marker
    # at its start, no blank line inside.
    text = re.sub(r"\s+", " ", text).strip()
    return re.sub(r"^([#>*+-]|\d+[.)])\s", r"\\\1 ", text)


def paper_markdown(paper):
    lines = ["# " + clean(paper["title"]), "", "## Abstract", "", clean(paper["abstract"]), ""]
    opened = []
    for section in paper["full_text"]:
        name = section["section_name"] or ""
        parts = [p.strip() for p in name.split(":::") if p.strip()]
        if not parts:
            parts = opened[:1] or ["Body"]
        # Open the missing ancestors once, so that "Model ::: Background"
        # nests under "Model" even when the paper has no text of its own
        # directly under "Model".
        common = 0
        while common < min(len(parts), len(opened)) and parts[common] == opened[common]:
            common += 1
        if common == len(parts):
            common -= 1
        for depth in range(common, len(parts)):
            lines += ["#" * (depth + 2) + " " + clean(parts[depth]), ""]
        opened = parts
        for p in section["paragraphs"]:
            p = clean(p)
            if p:
                lines += [p, ""]
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-in", dest="input", required=True)
    ap.add_argument("-split", required=True)
    ap.add_argument("-out", required=True)
    args = ap.parse_args()

    with open(args.input) as f:
        papers = json.load(f)

    root = os.path.join(args.out, args.split)
    os.makedirs(os.path.join(root, "papers"), exist_ok=True)

    kept = dropped = 0
    with open(os.path.join(root, "questions.jsonl"), "w") as out:
        for pid, paper in papers.items():
            with open(os.path.join(root, "papers", pid + ".md"), "w") as f:
                f.write(paper_markdown(paper))
            for qa in paper["qas"]:
                evidence = []
                for a in qa["answers"]:
                    ans = a["answer"]
                    if ans.get("unanswerable"):
                        continue
                    for e in ans.get("evidence", []):
                        e = re.sub(r"\s+", " ", e).strip()
                        if e and not e.startswith("FLOAT SELECTED") and e not in evidence:
                            evidence.append(e)
                if not evidence:
                    dropped += 1
                    continue
                kept += 1
                out.write(json.dumps({
                    "paper": pid,
                    "id": qa["question_id"],
                    "question": qa["question"].strip(),
                    "evidence": evidence,
                }, ensure_ascii=False) + "\n")

    print(f"{len(papers)} papers, {kept} questions kept, {dropped} without text evidence")


if __name__ == "__main__":
    main()
