#!/usr/bin/env python3
"""Translate a BEIR queries.jsonl into another language, offline.

The output is a queries.jsonl with the SAME _id values and translated text,
consumed by TestEvaluateBEIR through AMOXTLI_EVAL_BEIR_QUERIES_TRANSLATED. The
corpus and qrels are left untouched, so a run against the translated queries
measures the query<->document language gap and nothing else.

Only the queries that appear in the qrels file are translated (the BEIR
queries.jsonl usually holds train+dev+test, while an evaluation run scores the
test split alone).

The run is resumable: an existing output file is read back and its queries are
skipped, so a rate-limited or interrupted run can simply be relaunched.

Credentials come from the environment (or a .env file, see scripts/eval_env.sh):
OPENROUTER_API_KEY, or GENAI_CHAT_COMPLETION_OPENROUTER_API_KEY.

Usage:
    python scripts/translate_beir_queries.py \
        --queries .eval-data/scifact/queries.jsonl \
        --qrels .eval-data/scifact/qrels/test.tsv \
        --out .eval-data/scifact/queries.fr.jsonl \
        --lang French
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

SYSTEM_PROMPT = (
    "You are a professional translator. Translate each input into {lang}, "
    "faithfully and idiomatically, preserving the register and every technical "
    "term of the scientific domain. Keep named entities, gene and protein names, "
    "acronyms and numbers as they are. Do not answer, explain or comment on the "
    "queries: translate them.\n\n"
    "Input is a JSON object mapping ids to texts. Reply with a JSON object "
    "mapping the SAME ids to the translated texts, and nothing else."
)


def load_qrel_ids(path):
    """Return the query ids having at least one positive judgement."""
    ids = set()
    with open(path, encoding="utf-8") as f:
        for i, line in enumerate(f):
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 3:
                continue
            if i == 0 and parts[0] == "query-id":  # header
                continue
            try:
                score = float(parts[2])
            except ValueError:
                continue
            if score > 0:
                ids.add(parts[0])
    return ids


def load_queries(path, keep):
    out = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            row = json.loads(line)
            qid, text = row.get("_id"), row.get("text")
            if qid and text and (not keep or qid in keep):
                out[qid] = text
    return out


def load_done(path):
    if not os.path.exists(path):
        return {}
    return load_queries(path, None)


def chat(url, key, model, messages, retries, timeout):
    payload = {
        "model": model,
        "messages": messages,
        "temperature": 0,
        "response_format": {"type": "json_object"},
    }
    body = json.dumps(payload).encode("utf-8")

    for attempt in range(retries):
        req = urllib.request.Request(
            url,
            data=body,
            headers={
                "Authorization": f"Bearer {key}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                data = json.load(resp)
            return data["choices"][0]["message"]["content"]
        except urllib.error.HTTPError as err:
            # 429/5xx are worth retrying, a 4xx is not.
            if err.code != 429 and err.code < 500:
                raise
            wait = min(60, 2**attempt)
            print(f"  HTTP {err.code}, retrying in {wait}s", file=sys.stderr)
            time.sleep(wait)
        except (urllib.error.URLError, TimeoutError) as err:
            wait = min(60, 2**attempt)
            print(f"  {err}, retrying in {wait}s", file=sys.stderr)
            time.sleep(wait)

    raise RuntimeError(f"giving up after {retries} attempts")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--queries", required=True)
    ap.add_argument("--qrels", help="restrict to the queries judged in this qrels file")
    ap.add_argument("--out", required=True)
    ap.add_argument("--lang", default="French", help="target language, in English")
    ap.add_argument("--batch", type=int, default=10)
    ap.add_argument("--retries", type=int, default=8)
    ap.add_argument("--timeout", type=float, default=120)
    ap.add_argument(
        "--base-url",
        default=os.environ.get(
            "GENAI_CHAT_COMPLETION_OPENROUTER_BASE_URL",
            "https://openrouter.ai/api/v1",
        ),
    )
    ap.add_argument(
        "--model",
        default=os.environ.get(
            "GENAI_CHAT_COMPLETION_OPENROUTER_MODEL", "mistralai/mistral-medium-3-5"
        ),
    )
    args = ap.parse_args()

    key = os.environ.get("OPENROUTER_API_KEY") or os.environ.get(
        "GENAI_CHAT_COMPLETION_OPENROUTER_API_KEY"
    )
    if not key:
        sys.exit("no API key: set OPENROUTER_API_KEY (or source .env)")

    keep = load_qrel_ids(args.qrels) if args.qrels else set()
    queries = load_queries(args.queries, keep)
    done = load_done(args.out)

    todo = {qid: text for qid, text in queries.items() if qid not in done}
    print(
        f"{len(queries)} queries to translate into {args.lang}, "
        f"{len(done)} already done, {len(todo)} to go",
        file=sys.stderr,
    )

    url = args.base_url.rstrip("/") + "/chat/completions"
    system = SYSTEM_PROMPT.format(lang=args.lang)

    # Sorted ids keep the batching — hence the output file — reproducible.
    ids = sorted(todo)
    for start in range(0, len(ids), args.batch):
        batch = {qid: todo[qid] for qid in ids[start : start + args.batch]}

        content = chat(
            url,
            key,
            args.model,
            [
                {"role": "system", "content": system},
                {"role": "user", "content": json.dumps(batch, ensure_ascii=False)},
            ],
            args.retries,
            args.timeout,
        )

        try:
            translated = json.loads(content)
        except json.JSONDecodeError:
            print(f"  unparseable reply, skipping batch: {content[:200]}", file=sys.stderr)
            continue

        # Append as we go, so an interrupted run keeps what it has earned.
        with open(args.out, "a", encoding="utf-8") as f:
            for qid in batch:
                text = translated.get(qid)
                if not isinstance(text, str) or not text.strip():
                    print(f"  missing translation for {qid}", file=sys.stderr)
                    continue
                f.write(
                    json.dumps({"_id": qid, "text": text.strip()}, ensure_ascii=False)
                    + "\n"
                )
                done[qid] = text

        print(f"  {len(done)}/{len(queries)}", file=sys.stderr)

    missing = [qid for qid in queries if qid not in done]
    if missing:
        sys.exit(
            f"{len(missing)} queries left untranslated (e.g. {missing[:5]}); "
            "rerun to resume"
        )

    print(f"wrote {len(done)} translated queries to {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
