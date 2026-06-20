#!/bin/bash
# Run GraphRAG-Bench generation_eval with the LOCAL Qwen3.6-35B server as the
# judge LLM (instead of gpt-4o-mini). The judge LLM call goes to the local
# OpenAI-compatible llama-server on :8080; the BGE embeddings run locally too,
# so nothing leaves the box.
#
# Usage: run_judge.sh <results.json> [out_scores.json]
set -e
REPO=/tmp/grbench/GraphRAG-Benchmark
DATA="${1:-/tmp/grbench/med-validate-grbench.json}"
OUT="${2:-/tmp/grbench/judge-scores.json}"

export LLM_API_KEY=dummy                 # required to be set; llama-server ignores it
export TOKENIZERS_PARALLELISM=false
source /tmp/grbench/venv/bin/activate

cd "$REPO"
python -m Evaluation.generation_eval \
  --mode API \
  --model "local-qwen-35b" \
  --base_url http://localhost:8080/v1 \
  --embedding_model BAAI/bge-small-en-v1.5 \
  --data_file "$DATA" \
  --output_file "$OUT" \
  --detailed_output
