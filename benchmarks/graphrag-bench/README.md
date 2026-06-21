# GraphRAG-Bench × cbi

Run the local `cbi` agent against the
[GraphRAG-Bench](https://github.com/GraphRAG-Bench/GraphRAG-Benchmark) generation
benchmark, with **graph construction and judging both done by a local LLM** — no
API keys, nothing leaves the box.

The benchmark targets systems that build a knowledge graph *from a document
corpus*. `cbi` ingests pre-structured graphs, so step 1 below adds an LLM
entity/relation-extraction pass to turn the corpus into nodes/edges; the rest is
the normal `cbi` pipeline plus an adapter to GraphRAG-Bench's results schema.

## Pieces

| Script | Role |
|--------|------|
| `extract_graph.py` | Chunk a corpus and LLM-extract entities+relations → cbi `nodes.ndjson`/`edges.ndjson`/`domain.yaml`/`vocab.txt`. Uses an OpenAI-compatible endpoint (a strong local model, e.g. Qwen3.6-35B). |
| `prep_questions.py` | GraphRAG-Bench questions → cbi `questions.jsonl`, scoped to questions whose entities exist in the extracted graph, stratified by `question_type`. |
| `to_grbench.py` | `cbi answer` output → GraphRAG-Bench results schema (`id, question, generated_answer, ground_truth, context, question_type`). |
| `run_judge.sh` | Run GraphRAG-Bench `generation_eval` with a **local** judge LLM (OpenAI-compatible endpoint) + local BGE embeddings. |
| `judge-requirements.txt` | Python deps for the judge (generation eval only — no `ragas`). |

## Prerequisites

- **Two local servers** (llama.cpp `llama-server`):
  - a capable chat model on `:8080` (extractor + judge) — e.g. the repo's
    `sketchpad/ai/llm-server/start.sh` (Qwen3.6-35B). Thinking models must run
    with thinking disabled (the scripts pass `chat_template_kwargs.enable_thinking=false`).
  - a 768-dim embedding model on `:8181` for `cbi ingest` (EmbeddingGemma).
- The GraphRAG-Bench repo cloned next to this dir (for `Evaluation/`), and its
  `generation_eval.py` patched for a local judge: `timeout` raised from 30s and
  `extra_body={"chat_template_kwargs": {"enable_thinking": false}}` added to the
  `ChatOpenAI(...)` construction in API mode.

## Pipeline

```bash
# 0. Get the corpus + questions from GraphRAG-Bench/Datasets/{Corpus,Questions}/medical.*

# 1. Build a knowledge graph from the corpus (LLM extraction). ~24s/chunk.
python3 extract_graph.py --corpus medical.json --out med-graph \
    --endpoint http://localhost:8080 --chunk-chars 3000 --max-chars 250000

# 2. Ingest -> bundle (needs the :8181 embedding server; ingest auto-initializes).
cd med-graph
cbi ingest --nodes nodes.ndjson --edges edges.ndjson --config domain.yaml
cbi bundle --skill --config domain.yaml -o okf-bundle
cp vocab.txt okf-bundle/vocab.txt && cd ..

# 3. Pick questions the graph can answer, stratified by type.
python3 prep_questions.py --graph-vocab med-graph/vocab.txt --per-type 8 --min-hits 2 --out med-q.jsonl

# 4. Answer them with the local agent (model loads once), capturing context.
cbi bench answer --bundle med-graph/okf-bundle --questions med-q.jsonl --out med-answers.json

# 5. Map to GraphRAG-Bench schema, then judge with the LOCAL model.
python3 to_grbench.py --answers med-answers.json --out grbench_results.json
bash run_judge.sh grbench_results.json judge-scores.json
```

`generation_eval` scores per `question_type`: ROUGE-L + LLM-judged
answer-correctness (Fact Retrieval / Complex Reasoning), plus coverage
(Contextual Summarize) and faithfulness (Creative Generation).

## Notes

- **Volume.** Extraction is ~24s/chunk; the full ~1MB medical corpus is ~350
  chunks (~2.3 h). `--max-chars` bounds it; `prep_questions.py` then only keeps
  questions the (partial) graph actually covers, so scores stay honest.
- **Extractor vs answerer.** The graph is *built* with the strong server model;
  the questions are *answered* by the small local agent model (`cbi agent`) — the
  system under test. Index-time and query-time models are intentionally different.
- The judge LLM call is the only network-shaped hop, and it points at localhost.
