# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A domain-agnostic, local-first GraphRAG CLI (`cbi`) that builds temporal knowledge graphs in DuckDB with hybrid search (vector + lexical + graph). The first domain is Chicago business licensing data (58,108+ records), but the system is designed to work with any domain defined via YAML configuration.

## Essential Commands

```bash
task build                    # Compile the Go CLI → cli/cbi
task tidy                     # go mod tidy
task pipeline                 # Full end-to-end: convert → init → ingest
task clean                    # Remove binary and out/ directory
task clean:db                 # Delete the knowledge graph database (prompts)
```

### CLI (after `task build`, run from directory containing domain.yaml)

```bash
cbi init --config domain.yaml                          # Create DB, load extensions, schema, indexes, property graph
cbi ingest --nodes n.ndjson --edges e.ndjson            # NDJSON mode (batched, one record per line)
cbi ingest --file data.json                             # Single JSON mode ({nodes: [...], edges: [...]})
cbi query --text "search" --limit 10 [--date 2025-01-01]  # Hybrid search with optional temporal filter
cbi graph --sql "FROM GRAPH_TABLE(...)"                 # Raw SQL/PGQ queries
cbi schema                                             # LLM-friendly schema readout with query examples
```

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
../cli/cbi init --config domain.yaml
../cli/cbi ingest --nodes nodes.ndjson --config domain.yaml --batch-size 10
../cli/cbi ingest --edges edges.ndjson --config domain.yaml
../cli/cbi query --text "fire breathing dragon" --config domain.yaml --limit 3
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
