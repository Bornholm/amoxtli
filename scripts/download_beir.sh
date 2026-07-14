#!/usr/bin/env bash
# Download and unpack a BEIR dataset (https://github.com/beir-cellar/beir) into
# <out-dir>/<dataset>/ — the corpus.jsonl / queries.jsonl / qrels/test.tsv
# layout consumed by TestEvaluateBEIR. Skips the download when the dataset is
# already unpacked.
set -euo pipefail

dataset="${1:?usage: download_beir.sh <dataset> [out-dir]}"
out="${2:-.eval-data}"
url="https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/${dataset}.zip"

if [ -f "${out}/${dataset}/corpus.jsonl" ]; then
    echo "BEIR ${dataset}: already present in ${out}/${dataset}" >&2
    exit 0
fi

mkdir -p "${out}"
echo "BEIR ${dataset}: downloading ${url}" >&2
curl -fL --retry 3 -o "${out}/${dataset}.zip" "${url}"
unzip -q -o "${out}/${dataset}.zip" -d "${out}"

test -f "${out}/${dataset}/corpus.jsonl" || {
    echo "BEIR ${dataset}: corpus.jsonl missing after unzip" >&2
    exit 1
}
