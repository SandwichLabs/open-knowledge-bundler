# Open Knowledge Bundler 

A declarative toolkit for building [open knowledge format](https://cloud.google.com/blog/products/data-analytics/how-the-open-knowledge-format-can-improve-data-sharing) bundles packaged as agent skills. Define your domain in YAML, ingest data, and get vector + lexical + graph search for your local or remote agent(via mcp) over your knowledge. 

```
domain.yaml + data  -->  okb ingest  -->  DuckDB knowledge graph
                                              |
                                              +--> okb query "semantic search"
                                              +--> okb graph "SQL/PGQ graph traversal"
                                              +--> okb bundle --> portable bundle (.duckdb + OKF + Skill)
                                              +--> okb agent  --> fully-local chat over the bundle
```

## What it does

- **Declarative schema** — Define node types, edge types, and field mappings in a single YAML file. No code required to model a new domain.
- **Hybrid search** — BM25 lexical + cosine vector similarity, fused with Reciprocal Rank Fusion (RRF). Same algorithm in CLI and browser.
- **Property graph queries** — SQL/PGQ pattern matching via DuckDB's `duckpgq` extension. Multi-hop traversals, shortest path, PageRank.
- **Temporal tracking** — SCD Type 2 versioning. Query current state or any historical snapshot.
- **Browser compilation** — Export your graph to a self-contained HTML file with in-browser semantic search (Transformers.js) and interactive graph visualization (sigma.js).
- **Local-first** — Everything runs on your machine. DuckDB is the only database. Any OpenAI-compatible embedding endpoint works (Ollama, llama.cpp, vLLM).

## Quick start

### Prerequisites

- **Go** 1.24+
- **[Task](https://taskfile.dev)** v3 (task runner)
- **Node.js** 18+ (for browser compilation only)
- **Embedding server** — any OpenAI-compatible endpoint (e.g. `ollama serve` with an embedding model)

### Build and run with the Pokemon test dataset

```bash
# Build the CLI
task build

# Ingest and query the test dataset (ingest auto-initializes the DB)
cd test
../cli/okb ingest --nodes nodes.ndjson --config domain.yaml --batch-size 50
../cli/okb ingest --edges edges.ndjson --config domain.yaml
../cli/okb query --text "fire breathing dragon" --config domain.yaml --limit 5
```

### Compile to a browser app

```bash
cd browser
npm install
node compile.mjs --config ../test/domain.yaml --output dist/
npx serve dist/
```

Open `http://localhost:3000` — full semantic search + interactive graph visualization, runs entirely in the browser after initial model download.

## Defining a domain

Everything is driven by `domain.yaml`. Here's a minimal example:

```yaml
domain_name: my_domain
embedding_dim: 768
embedding_model: gemma
endpoint_url: "http://localhost:11434"
database_path: my_domain.duckdb

node_definitions:
  Person:
    semantic_fields:
      - name
      - bio
    mappings:
      - { source_field: "id", target_field: node_id, is_key: true }
      - { source_field: "name", target_field: name }
      - { source_field: "bio", target_field: bio }

  Organization:
    semantic_fields:
      - name
    mappings:
      - { source_field: "org_id", target_field: node_id, is_key: true }
      - { source_field: "org_name", target_field: name }

edge_definitions:
  WORKS_AT:
    mappings:
      - { source_field: "person_id", target_field: source_id, is_key: true }
      - { source_field: "org_id", target_field: target_id, is_key: true }
```

Key concepts:

- **`node_definitions`** — Each key is a node type. `semantic_fields` lists which properties are concatenated for embedding. `mappings` maps source data fields to the graph schema.
- **`edge_definitions`** — Each key is a relationship type. Edges connect `source_id` to `target_id`.
- **`embedding_dim`** — Dimension of your embedding model (768 for most small models).
- **`endpoint_url`** — Any OpenAI-compatible `/v1/embeddings` endpoint.

## Data format

Ingest data as NDJSON (one JSON object per line):

**nodes.ndjson:**
```json
{"node_id":"person:1","node_type":"Person","properties":{"name":"Alice","bio":"Software engineer"},"semantic_text":"Alice | Software engineer who builds distributed systems"}
{"node_id":"org:1","node_type":"Organization","properties":{"name":"Acme Corp"},"semantic_text":"Acme Corp | Technology company"}
```

**edges.ndjson:**
```json
{"edge_id":"works_at:1:1","source_id":"person:1","target_id":"org:1","relationship_type":"WORKS_AT","weight":1.0}
```

The `semantic_text` field is what gets embedded and searched. You can craft it however you want — the richer the text, the better the semantic search.

## CLI reference

```bash
# BUILD — input → graph → portable bundle (ingest/extract auto-initialize the DB)
okb extract --corpus docs/ -o out/ [--bootstrap --glean 1 --resolve --ingest]
                                                     # Prose corpus → graph with a local LLM (fully in-process)
okb ingest  --nodes n.ndjson --edges e.ndjson        # Ingest pre-structured data (batched); also --file data.json
okb bundle  -o bundle/ [--skill] [--no-db]           # Pack a portable bundle (.duckdb + OKF + Skill)

# INSPECT — validate the graph
okb query   --text "search" --limit 10               # Hybrid search (BM25 + vector + RRF); --date for temporal
okb graph   --sql "FROM GRAPH_TABLE(...)"            # Raw SQL/PGQ queries
okb schema                                           # Schema readout with query examples

# CONSUME — fully-local agent over a bundle
okb agent   --bundle ./bundle [--ask "q" --json]     # Chat TUI / one-shot answer

# bench (benchmark scaffolding) · site (hosted viewer)
okb bench eval|answer|convert ...                    # Evaluate the local agent
okb site generate -o dist/ | okb site serve          # Static site / live HTTP API + D3 viewer
```

### Graph extraction from a corpus (`okb extract`)

`okb extract` builds a knowledge graph from a prose corpus using a **local LLM**
(via kronk/llama.cpp) — no external server, no API keys. It is the front door for
domains that start as documents rather than structured records.

```bash
# First run: propose an ontology, stop for review, then re-run to extract.
okb extract --corpus medical.json -o med-graph/          # auto-bootstrap → review domain.yaml
okb extract --corpus medical.json -o med-graph/ --glean 1 --resolve --ingest

# Or bootstrap-and-run in one shot:
okb extract --corpus docs/ -o out/ --bootstrap --glean 1 --resolve --ingest --tier large
```

Five in-process stages:

1. **Ontology bootstrap** — the model proposes a compact, closed vocabulary
   (entity types + directional relations) from a corpus sample into the
   `domain.yaml` `ontology:` block. *Bootstrap, then editable*: an automatic
   bootstrap stops so you can edit it; `--bootstrap`/`--yes` continues immediately.
2. **Extraction** — per-chunk entity/relation extraction with `type`/`relation`
   **enum-constrained to the ontology** (kronk `response_format` → GBNF grammar), so
   the relation vocabulary can't sprawl and the JSON is always valid.
3. **Gleaning** (`--glean K`) — extra recall passes per chunk for missed facts.
4. **Entity resolution** (`--resolve`) — exact-merge + per-type embedding clustering
   (local EmbeddingGemma) with LLM adjudication of borderline pairs; unions aliases
   and remaps edges to canonical nodes.
5. **Normalization** — canonicalizes relation direction (rewriting inverses) and
   validates endpoint types, then emits `nodes.ndjson`/`edges.ndjson`/`domain.yaml`/
   `vocab.txt`. `--ingest` loads straight into DuckDB in-process.

`--tier small|medium|large|xl|moe` (default `large` = Gemma-4-12B) or `--model <src>`
picks the generator; `--gpu` picks the llama.cpp backend.

### OKF bundle export

`okb bundle` packs the graph into a portable bundle: an [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
v0.1 directory tree of markdown (readable by humans and agents with no tooling,
diffable in git) plus — by default — the DuckDB database and domain config, so the
bundle is self-contained and queryable.

```bash
okb bundle -o bundle/ --mode both        # catalog + one doc per node (default)
okb bundle -o bundle/ --mode catalog     # per-type / per-relationship schema only
okb bundle -o bundle/ --mode full        # one concept document per node
okb bundle --node-types Business,Ward    # restrict to specific node types
okb bundle --max-per-type 50             # cap per-node docs written per type
okb bundle --skill                       # add SKILL.md → self-contained agent skill (DB included by default)
okb bundle --no-db                       # OKF markdown only (omit DB/config)
```

Layout: `index.md` (root listing, carries `okf_version`), `log.md`, `catalog/`
(node-type and relationship-type concepts), and `<NodeType>/` directories of
per-node concept documents with edges rendered as bundle-relative cross-links.

**Agent skill bundle.** Add `--skill` to emit a `SKILL.md` so the bundle doubles
as a self-describing [agent skill](https://docs.claude.com/en/docs/claude-code/skills).
The DuckDB database and domain config are copied alongside it by default (pass
`--no-db` to omit them), so the result is fully self-contained: browsable OKF
markdown for orientation plus a queryable database for precise hybrid retrieval.
The generated
`SKILL.md` documents the entity types, the `Nodes_Base`/`Edges_Base` schema, and
ready-to-run DuckDB SQL + `okb` query examples.

### Local agent (`okb agent`)

Chat with a bundle using a **fully local, self-contained agent** — no API keys,
no embedding server. Inference and embeddings run on-device via
[kronk](https://github.com/ardanlabs/kronk) (llama.cpp); the agent loop, tools,
and streaming are handled by [fantasy](https://github.com/charmbracelet/fantasy).

```bash
okb agent --bundle ./okf-bundle                 # Bubble Tea chat TUI
okb agent --bundle ./okf-bundle --ask "how many incidents per year?"   # one-shot
okb agent --bundle ./okf-bundle --tier large --gpu vulkan              # bigger model / backend
okb agent --bundle ./okf-bundle --reconfigure   # re-pick the model size
```

The agent answers by calling tools over the bundle: `schema`, `sql_query`
(read-only), `hybrid_search` (vector + lexical), and `list_docs` / `search_docs`
/ `read_doc` for the markdown concepts. On first run you pick a model size
(Gemma 4 family); the choice and all settings persist in
`~/.config/okb/config.yaml`. Models download once from Hugging Face. The default
llama.cpp backend is **Vulkan** (override with `--gpu` or the `processor` config /
`KRONK_PROCESSOR`). Embeddings (EmbeddingGemma, 768-dim) are pinned to the
bundle's index; if they can't be loaded, `hybrid_search` degrades to lexical-only.

Generate the bundle with the database included (the default; don't pass `--no-db`)
and, for document exploration, `--mode both` so per-node concept docs are written.

## Architecture

### Four DuckDB extensions, one database

| Extension | Purpose | Key capability |
|-----------|---------|----------------|
| **vss** | Vector similarity | HNSW index, `array_cosine_distance` |
| **fts** | Full-text search | BM25 scoring via `match_bm25` |
| **spatial** | Geometry | `ST_Point`, `ST_Distance_Spheroid` |
| **duckpgq** | Property graph | `CREATE PROPERTY GRAPH`, `GRAPH_TABLE`, `MATCH` |

### Hybrid search pipeline

```
Query text
  |
  +--> BM25 lexical scoring (fts)    --> rank_lex
  |                                        |
  +--> Embed query --> cosine sim (vss) --> rank_sem
                                           |
                                     RRF fusion
                              1/(60+rank_lex) + 1/(60+rank_sem)
                                           |
                                     top-N results
```

### Data model

**Nodes_Base** — entities with embedding + optional geometry + temporal tracking:
```
node_id PK, node_type, properties JSON, semantic_text,
embedding FLOAT[768], latitude, longitude, geom GEOMETRY,
valid_from, valid_to, is_current
```

**Edges_Base** — directed weighted relationships:
```
edge_id PK, source_id FK, target_id FK, relationship_type,
weight, valid_from, valid_to, is_current
```

### Graph queries (SQL/PGQ)

All graph pattern matching uses the `domain_graph` property graph. Nodes are labeled `"node"`, edges are labeled `"edge"`. Filter by `node_type` and `relationship_type`:

```sql
-- Find all Fire-type Pokemon
FROM GRAPH_TABLE(domain_graph
  MATCH (p:"node")-[e:"edge"]->(t:"node")
  WHERE p.node_type = 'Pokemon' AND t.node_type = 'Type'
    AND e.relationship_type = 'HAS_TYPE'
    AND t.node_id = 'type:fire'
  COLUMNS (p.properties->>'name' AS pokemon, t.node_id AS type)
)

-- Multi-hop: Pokemon that share a type with Charizard
FROM GRAPH_TABLE(domain_graph
  MATCH (a:"node")-[e1:"edge"]->(t:"node")<-[e2:"edge"]-(b:"node")
  WHERE a.node_id = 'pokemon:006'
    AND e1.relationship_type = 'HAS_TYPE'
    AND e2.relationship_type = 'HAS_TYPE'
    AND b.node_id != a.node_id
  COLUMNS (b.properties->>'name' AS pokemon, t.node_id AS shared_type)
)
```

## Browser app

The `browser/compile.mjs` script reads your DuckDB database and produces a self-contained search experience:

```bash
node compile.mjs --config domain.yaml --output dist/
```

**Output:**
- `graph.json` — nodes, edges, and UI configuration derived from your domain.yaml
- `embeddings.bin` — pre-computed embeddings as a flat Float32Array
- `index.html` — self-contained app (~30 KB)

**What it includes:**
- **Semantic search** via Transformers.js (EmbeddingGemma-300M, runs in a Web Worker)
- **BM25 lexical search** with an in-memory inverted index
- **RRF fusion** matching the CLI's algorithm exactly
- **Interactive graph** via Graphology + sigma.js (force-directed layout, WebGL rendering)
- **Config-driven UI** — filter checkboxes, property tables, and colors all generated from your domain.yaml
- **Offline support** — model weights cached via Cache API after first load

No server required. Open `index.html` from any static file host.

## Temporal queries

The system uses SCD Type 2 tracking. Each ingestion timestamps records. Query current state or historical snapshots:

```bash
# Current state (default)
okb query --text "search"

# State as of a specific date
okb query --text "search" --date 2025-06-15
```

Historical queries use: `WHERE valid_from <= ts AND (valid_to IS NULL OR valid_to > ts)`

## Example domains

### Pokemon (test fixture)

801 Pokemon, 18 types, 7 regions. Node types: `Pokemon`, `Type`, `Region`. Edge types: `HAS_TYPE`, `FOUND_IN`.

```bash
cd test && ../cli/okb query --text "legendary dragon" --config domain.yaml --limit 3
```

### Chicago business licenses

58,108 businesses with neighborhoods, wards, activities, license types. Spatial proximity edges (200m threshold). 6 node types, 7 edge types.

```yaml
# domain.yaml excerpt
spatial:
  near_threshold_meters: 200

node_definitions:
  Business:
    semantic_fields: [legal_name, doing_business_as, activity, license_description, address, neighborhood]
```

## Task commands

```bash
task build              # Compile Go CLI
task compile            # Build browser app (Pokemon test data)
task compile:serve      # Serve browser app locally
task pipeline           # Full end-to-end: convert -> init -> ingest
task query -- "text"    # Hybrid search
task graph:stats        # Node/edge counts
task clean              # Remove build artifacts
```

## Dependencies

**CLI (Go):**
- DuckDB 1.5.0 via `duckdb-go/v2` (pinned — duckpgq requires 1.5.0)
- Cobra + Viper for CLI framework

**Browser compilation (Node.js):**
- `@duckdb/node-api` for reading the database
- `yaml` for config parsing

**Browser runtime (CDN, loaded on first use):**
- Transformers.js v3.7.2 (EmbeddingGemma-300M Q8)
- Graphology v0.26.0
- sigma.js v2.4.0

## License

MIT
