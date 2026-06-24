# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A domain-agnostic, local-first GraphRAG CLI (`okb`) that builds temporal knowledge graphs in DuckDB with hybrid search (vector + lexical + graph). The first domain is Chicago business licensing data (58,108+ records), but the system is designed to work with any domain defined via YAML configuration.

## Essential Commands

```bash
task build                    # Compile the Go CLI → cli/okb
task tidy                     # go mod tidy
task pipeline                 # Full end-to-end: convert → init → ingest
task clean                    # Remove binary and out/ directory
task clean:db                 # Delete the knowledge graph database (prompts)
```

### CLI (after `task build`, run from directory containing domain.yaml)

The surface is focused on building portable knowledge bundles. Top-level groups:
**BUILD** (extract · ingest · bundle), **INSPECT** (query · graph · schema),
**CONSUME** (agent), plus quarantined **bench** and **site** namespaces.
(`extract` is the fully-local in-process LLM extractor — corpus → graph; see the
design record in `benchmarks/graphrag-bench/EXTRACTOR_HANDOFF.md`.)

```bash
# BUILD — input → graph → portable bundle (ingest/extract auto-initialize the DB)
okb extract --corpus docs/ -o out/ [--bootstrap --glean 1 --resolve --ingest]
                                                        # Prose corpus → resolved graph with a local LLM (no server)
okb ingest --nodes n.ndjson --edges e.ndjson            # NDJSON mode (batched); also --file data.json
okb bundle -o bundle/ [--skill] [--no-db] [--mode both|catalog|full] [--node-types ...] [--max-per-type N]
                                                        # Pack a portable bundle (.duckdb + OKF + Skill). --include-db is the DEFAULT; --no-db = markdown only.

# INSPECT — validate the graph
okb query --text "search" --limit 10 [--date 2025-01-01]  # Hybrid search with optional temporal filter
okb graph --sql "FROM GRAPH_TABLE(...)"                 # Raw SQL/PGQ queries
okb schema                                             # LLM-friendly schema readout with query examples

# CONSUME — fully-local agent over a bundle
okb agent --bundle ./bundle                            # Chat TUI (kronk LLM + embeddings)
okb agent --bundle ./bundle --ask "question" [--json]  # One-shot answer; --json adds tool trace + tokens + timing
okb agent --bundle ./bundle --tier large --gpu vulkan  # Pick model size / llama.cpp backend

# bench — benchmark scaffolding (not part of the build pipeline)
okb bench eval --bundle ./bundle --questions q.jsonl --vocab vocab.txt --by hop
okb bench answer --bundle ./bundle --questions q.jsonl --out answers.json
okb bench convert metaqa --src ./MetaQA --out ./metaqa-okb --sample 100

# site — hosted graph viewer (separate from the portable bundle)
okb site generate -o dist/                             # Self-contained static site
okb site serve --addr 127.0.0.1:8765                   # Live HTTP API + UI

# okb init is now hidden — ingest/extract initialize the DB automatically.
```

### Graph Extraction (`okb extract`)

`okb extract` builds a resolved knowledge graph from a prose corpus using a
**local LLM** (kronk/llama.cpp, no external server, no API keys) — the front door
for domains that start as documents rather than structured records. Five
in-process stages: ontology bootstrap → grammar-constrained extraction → gleaning
(recall) → entity resolution → relation normalization, then it emits the standard
okb ingest shape (`nodes.ndjson`, `edges.ndjson`, `domain.yaml`, `vocab.txt`) and
can `--ingest` straight into DuckDB.

```bash
okb extract --corpus medical.json -o med-graph/                       # auto-bootstrap → stops for ontology review
okb extract --corpus medical.json -o med-graph/ --glean 1 --resolve --ingest
okb extract --corpus docs/ -o out/ --bootstrap --glean 1 --resolve --ingest --tier large
```

- **Ontology** (entity types + a closed, directional relation vocabulary) lives in
  the `domain.yaml` `ontology:` block. *Bootstrap, then editable*: an automatic
  bootstrap stops so you can review/edit; `--bootstrap`/`--yes` continues now.
- The entity `type`/`relation` fields are constrained to the ontology in code,
  not via the schema (kronk v1.28.0 silently ignores `enum` in `response_format`
  — keep extraction schemas structure-only).
- `--tier small|medium|large|xl|moe` (default `large` = Gemma-4-12B) or `--model`
  picks the generator; `--gpu` picks the llama.cpp backend.
- Code: `cli/extract/` + `cli/cmd/extract.go`; design record in
  `benchmarks/graphrag-bench/EXTRACTOR_HANDOFF.md`.

### Benchmarking (`okb bench eval`)

`okb bench eval` runs the local agent over a `questions.jsonl` answer key (one
`{"question","gold":[...],"tags":{...}}` per line) and scores answers
deterministically — `recall`, `exact` (Hits@all), and `precision`/`F1` when an
entity-name `--vocab` is supplied (precision catches over-generation/hallucination
with no LLM judge). Honest "not found" misses are counted separately from
confident wrong answers. The model loads once per tier (in-process via the agent
session's `Answer()`), so it is far faster than `--ask` per question. Sweep model
sizes with repeated `--tier`; break the leaderboard down with `--by <tag>`; write
per-question results with `--out`. Code: `cli/eval/` (scoring) + `cli/cmd/eval.go`.
(`init`, `eval`, `answer`, `convert`, `generate`, `serve` were pruned/regrouped on
2026-06-21; the build surface is now extract/ingest/bundle — see the CLI block above.)

### Dataset conversion (`okb bench convert metaqa`)

Converts a local [MetaQA](https://github.com/yuyuz/MetaQA) checkout (download from
the Google Drive linked in its README) into the okb ingest format + an eval set.
After converting: `okb ingest --nodes nodes.ndjson --edges edges.ndjson` →
`okb bundle --skill` → `okb bench eval`. Code: `cli/metaqa/` +
`cli/cmd/convert_metaqa.go`. Ingest needs a 768-dim embedding endpoint (see the
embedding-server note); `okb bench eval` then runs fully local via kronk.

### Local Agent (`okb agent`)

A self-contained, offline chat agent over an OKF bundle. Inference + embeddings
run locally via [kronk](https://github.com/ardanlabs/kronk) (llama.cpp); the
agent loop, tools, and streaming are handled by
[fantasy](https://github.com/charmbracelet/fantasy). No API keys, no embedding
server.

- **Models** (Gemma 4 family) download once from Hugging Face. Size is chosen on
  first run and persisted, with all settings, in `~/.config/okb/config.yaml`
  (`tier`, `models` map, `embed_source`, `processor`). Override per-invocation
  with `--tier`/`--model`/`--gpu`; re-run the picker with `--reconfigure`.
- **Backend**: defaults to **Vulkan** (`processor: vulkan`). Auto-detect prefers
  ROCm when `rocminfo` is present, which is unreliable on AMD APUs; Vulkan is the
  fast, reliable path on Strix Halo. Set `KRONK_PROCESSOR` or the `processor`
  config / `--gpu` flag to override (cpu|cuda|rocm|vulkan).
- **Embeddings** are pinned to the bundle's index dimension (EmbeddingGemma,
  768-dim, Matryoshka-reduced). If the embedding model can't be loaded/matched,
  hybrid search degrades to lexical-only (shown in the status bar).
- **Tools** the agent can call: `schema`, `sql_query` (read-only guard),
  `hybrid_search` (vector+lexical, or lexical-only fallback), `list_docs`,
  `search_docs`, `read_doc`. Code lives in `cli/agent/`.

### Source Database Exploration (reads chi-city-data.duckdb directly)

```bash
task schema                           # Show all tables
task sql -- "SELECT ..."              # Run SQL
task summarize -- city_businesses     # Column statistics
task graph:stats                      # Node/edge counts in knowledge graph
```

### Test Fixtures (Pokemon domain)

```bash
cd test
rm -f pokemon.duckdb
../cli/okb ingest --nodes nodes.ndjson --config domain.yaml --batch-size 10   # auto-initializes the DB
../cli/okb ingest --edges edges.ndjson --config domain.yaml
../cli/okb query --text "fire breathing dragon" --config domain.yaml --limit 3
```

## Architecture

### Chimera Architecture: Four DuckDB Extensions in One Database

| Extension | Purpose | Key Functions |
|-----------|---------|---------------|
| **vss** | Vector similarity search | HNSW index, `array_cosine_distance` |
| **fts** | Full-text search | `match_bm25` macro, `PRAGMA create_fts_index` |
| **spatial** | Geometry/distance | `ST_Point`, `ST_Distance_Spheroid` |
| **duckpgq** | Property graph queries | `CREATE PROPERTY GRAPH`, `GRAPH_TABLE`, `MATCH` |

### Data Model (store/db.go)

**Nodes_Base** — entities with embedding + geometry + temporal tracking:
`node_id PK, node_type, properties JSON, semantic_text, embedding FLOAT[768], latitude, longitude, geom GEOMETRY, valid_from, valid_to, is_current`

**Edges_Base** — directed weighted relationships with temporal tracking:
`edge_id PK, source_id FK, target_id FK, relationship_type, weight, valid_from, valid_to, is_current`

**Property Graph** — `domain_graph` created over these tables with vertex label `"node"` and edge label `"edge"`.

### Hybrid Search Pipeline (store/search.go)

1. **Lexical CTE** — BM25 scoring via `fts_main_Nodes_Base.match_bm25()`
2. **Semantic CTE** — Cosine distance on HNSW-indexed embeddings
3. **RRF Fusion** — `1/(60+lex_rank) + 1/(60+sem_rank)`, FULL OUTER JOIN, ordered DESC

### Temporal Tracking (SCD Type 2)

Upserts expire the current version (`is_current=FALSE, valid_to=ts`) then insert a new version (`is_current=TRUE, valid_from=ts`). Query current state with `WHERE is_current = TRUE` or historical snapshots with `WHERE valid_from <= ts AND (valid_to IS NULL OR valid_to > ts)`.

### Embedding Client (embed/client.go)

Calls any OpenAI-compatible endpoint at `{endpoint_url}/v1/embeddings`. Default: llama.cpp at `localhost:8080` with `gemma` model (768-dim).

## Key Conventions

### Node ID Format
`type_prefix:key_parts` — e.g., `biz:12345:1`, `neighborhood:Loop`, `activity:775`, `ward:42`

### Graph Query Labels
All SQL/PGQ MATCH patterns must label nodes as `"node"` and edges as `"edge"`. Filter by `node_type` and `relationship_type` in WHERE clauses:
```sql
FROM GRAPH_TABLE(domain_graph
  MATCH (a:"node")-[e:"edge"]->(b:"node")
  WHERE a.node_type = 'Business' AND e.relationship_type = 'LOCATED_IN'
  COLUMNS (a.properties->>'legal_name' AS name, b.node_id AS neighborhood)
)
```

Multi-hop: every node reference in every pattern must include the label, even if the same variable:
```sql
MATCH (p:"node")-[e1:"edge"]->(t1:"node"), (p:"node")-[e2:"edge"]->(t2:"node")
```

### Domain Configuration (domain.yaml)
Defines node/edge types, field mappings from source data, semantic fields for embedding, embedding model/endpoint, and database path. The CLI is domain-agnostic — all domain-specific knowledge lives in this file.

### FTS Index
Must be rebuilt after batch ingestion (`PRAGMA drop_fts_index` + `PRAGMA create_fts_index`). The ingest command handles this automatically.

### HNSW Persistence
Requires `SET hnsw_enable_experimental_persistence = true` for on-disk databases (handled in `CreateIndexes`).

## Dependencies

- **Go**: `github.com/duckdb/duckdb-go/v2` (v2.10500.0 = DuckDB 1.5.0), cobra, viper
- **Pinned to DuckDB 1.5.0** because duckpgq community extension is not yet available for 1.5.1
- **Data conversion**: Node.js + `@duckdb/node-api` (for `data/to_ndjson.ts`)
- **Embedding server**: Any OpenAI-compatible endpoint (Ollama, llama.cpp, vLLM)
- **Task runner**: [Taskfile](https://taskfile.dev) v3
