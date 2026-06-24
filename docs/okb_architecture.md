# okb Architecture

A Domain-Driven Design (DDD) and Information Architecture reference for the
**Open Knowledge Bundler** (`okb`) — a domain-agnostic, local-first GraphRAG CLI
that builds temporal knowledge graphs in DuckDB and packs them into portable,
queryable knowledge bundles.

This document describes the system's bounded contexts, data model, and the
build → inspect → consume command surface. It is implementation-oriented: every
section maps to code under `cli/`.

-----

## 1. Architectural Overview

`okb` is a local-first, CLI-driven application that constructs, analyzes, and
queries domain-agnostic knowledge graphs. It relies on a "Chimera" architecture,
unifying relational data, vector embeddings, full-text search, geometry, and
property graphs within a single DuckDB instance.

  * **Core engine:** DuckDB (pinned to 1.5.0 — `duckpgq` is not yet built for 1.5.1).
  * **Graph traversal:** the `duckpgq` extension, executing SQL/PGQ over the relational tables.
  * **Semantic search:** the `vss` extension for ANN indexing (HNSW) and `array_cosine_distance`.
  * **Lexical search:** the `fts` extension via the `match_bm25` macro.
  * **Geometry:** the `spatial` extension (`ST_Point`, `ST_Distance_Spheroid`).
  * **Embeddings:** two paths, both producing `FLOAT[N]` arrays stored in DuckDB —
    * *fully local* (kronk / llama.cpp, EmbeddingGemma 768-dim) for `okb extract` and `okb agent`; no server, no API keys.
    * *external endpoint* (any OpenAI-compatible `/v1/embeddings`, e.g. Ollama / llama.cpp / vLLM) for `okb ingest`.

The portable output is an **Open Knowledge Format (OKF)** bundle: browsable
markdown + the DuckDB database + an optional agent `SKILL.md`.

-----

## 2. Domain-Driven Design (DDD) Core Contexts

The framework is domain-agnostic: every bounded context is abstracted into generic
primitives defined entirely by user configuration (`domain.yaml`).

### 2.1 Core Entities (Nodes)

Entities represent the primary nouns in your configured domain (e.g. `Business`,
`Resource`, `Person`, `Document`).

  * **Identity:** strictly formatted unique string — `type_prefix:key_parts` (e.g. `biz:12345:1`, `neighborhood:Loop`).
  * **Properties:** dynamic JSON payload of scalar attributes.
  * **Semantic anchor:** a designated text field (or concatenated `semantic_fields`) that is vectorized to create the `embedding`.

### 2.2 Relationships (Edges)

Edges represent the verbs connecting entities (e.g. `LOCATED_IN`, `REPORTS_TO`,
`REFERENCES`).

  * **Directionality:** strictly directed (source → target).
  * **Properties:** edges carry weights, confidence scores, or qualitative metadata.

### 2.3 Temporal Tracking Context

All entities and relationships implement a temporal interface (SCD Type 2 —
Slowly Changing Dimensions) for chronological tracking.

  * **valid_from / valid_to:** the window an entity state or relationship was active.
  * **is_current:** a boolean flag optimizing queries for the present state.

### 2.4 Ontology Context (extraction only)

When a graph is built from prose via `okb extract`, the domain is governed by an
**ontology**: a compact, closed vocabulary of entity types plus a directional
relation vocabulary, declared in the `domain.yaml` `ontology:` block. It is
bootstrapped by the model, then human-editable, and enforced in code during
extraction (the `type`/`relation` fields are constrained to the ontology).

-----

## 3. Information Architecture & Database Schema

The data layer uses DuckDB's zero-copy interoperability, defining logical property
graphs directly over relational tables.

### 3.1 Relational Substrate

The Go application generates these tables from the domain configuration.

  * **`Nodes_Base`** — entities with embedding + optional geometry + temporal tracking:

      * `node_id` (VARCHAR, PK)
      * `node_type` (VARCHAR) — e.g. "Business", "Resource"
      * `properties` (JSON)
      * `semantic_text` (VARCHAR) — target for the FTS index
      * `embedding` (FLOAT[N]) — target for the VSS HNSW index
      * `latitude`, `longitude` (DOUBLE), `geom` (GEOMETRY)
      * `valid_from`, `valid_to` (TIMESTAMP), `is_current` (BOOLEAN)

  * **`Edges_Base`** — directed weighted relationships:

      * `edge_id` (VARCHAR, PK)
      * `source_id` (VARCHAR, FK → `Nodes_Base.node_id`)
      * `target_id` (VARCHAR, FK → `Nodes_Base.node_id`)
      * `relationship_type` (VARCHAR)
      * `weight` (FLOAT)
      * `valid_from`, `valid_to` (TIMESTAMP), `is_current` (BOOLEAN)

### 3.2 Graph Instantiation (duckpgq)

Rather than duplicating data, the system executes a generated `CREATE PROPERTY
GRAPH` statement. The graph is named `domain_graph`; nodes are labeled `"node"`
and edges `"edge"` — every SQL/PGQ `MATCH` pattern must use those labels and
filter on `node_type` / `relationship_type`.

```sql
CREATE PROPERTY GRAPH domain_graph
VERTEX TABLES (
    Nodes_Base LABEL "node"
)
EDGE TABLES (
    Edges_Base
        SOURCE KEY (source_id) REFERENCES Nodes_Base (node_id)
        DESTINATION KEY (target_id) REFERENCES Nodes_Base (node_id)
        LABEL "edge"
);
```

-----

## 4. Go Implementation & Module Outline

### 4.1 Required Go Modules

  * **Database driver:** `github.com/duckdb/duckdb-go/v2` (v2.10500.0 = DuckDB 1.5.0) — executes SQL and the pragmas for `fts`/`vss`/`spatial`/`duckpgq`.
  * **CLI framework:** `github.com/spf13/cobra` — routes the `extract`/`ingest`/`bundle`/`query`/`graph`/`schema`/`agent` commands.
  * **Configuration:** `github.com/spf13/viper` — parses dynamic domain schemas from YAML.
  * **HTTP client:** standard `net/http` — for the external embedding endpoint used by `ingest`.
  * **Local inference + embeddings:** kronk (llama.cpp) for `extract`/`agent`; the agent loop and streaming use fantasy.

### 4.2 Core Struct Mapping

```go
package domain

import "time"

// TemporalRecord handles the chronological tracking requirement
type TemporalRecord struct {
    ValidFrom time.Time `json:"valid_from"`
    ValidTo   time.Time `json:"valid_to,omitempty"`
    IsCurrent bool      `json:"is_current"`
}

// Node represents a generic entity in the graph
type Node struct {
    NodeID       string                 `json:"node_id"`
    NodeType     string                 `json:"node_type"`
    Properties   map[string]interface{} `json:"properties"`
    SemanticText string                 `json:"semantic_text"`
    Embedding    []float32              `json:"embedding"` // local (kronk) or external endpoint
    TemporalRecord
}

// Edge represents a chronological relationship
type Edge struct {
    EdgeID           string  `json:"edge_id"`
    SourceID         string  `json:"source_id"`
    TargetID         string  `json:"target_id"`
    RelationshipType string  `json:"relationship_type"`
    Weight           float64 `json:"weight"`
    TemporalRecord
}

// DomainConfig dictates how the CLI maps raw data to the graph
type DomainConfig struct {
    DomainName      string            `yaml:"domain_name"`
    EmbeddingDim    int               `yaml:"embedding_dim"`  // e.g. 768
    EmbeddingModel  string            `yaml:"embedding_model"`
    EndpointURL     string            `yaml:"endpoint_url"`   // ingest embedding endpoint
    DatabasePath    string            `yaml:"database_path"`
    NodeDefinitions map[string]Config `yaml:"node_definitions"`
    EdgeDefinitions map[string]Config `yaml:"edge_definitions"`
    Ontology        Ontology          `yaml:"ontology"` // extract: entity types + relation vocabulary
}
```

-----

## 5. Hybrid Search Pipeline

To support ambiguous or exploratory queries, the system runs lexical and semantic
search in parallel and fuses them with Reciprocal Rank Fusion (RRF). See
`store/search.go`.

1.  **Lexical CTE** — `fts_main_Nodes_Base.match_bm25()` scores exact text matches against `semantic_text`.
2.  **Semantic CTE** — `array_cosine_distance` over the HNSW-indexed `embedding` column against the embedded query.
3.  **RRF fusion** — `1/(60 + rank_lex) + 1/(60 + rank_sem)`, FULL OUTER JOIN, ordered DESC.

The same RRF algorithm is implemented in the browser app, so CLI and in-browser
results match.

> **Note:** DuckDB FTS indexes do not auto-update on mutation. After batch
> ingestion the CLI rebuilds the index (`PRAGMA drop_fts_index` +
> `PRAGMA create_fts_index`). HNSW persistence on disk requires
> `SET hnsw_enable_experimental_persistence = true` (handled in `CreateIndexes`).

-----

## 6. CLI Application Flow

The surface is organized around building portable knowledge bundles. `okb init`
is hidden — `extract` and `ingest` initialize the DuckDB database automatically.

### BUILD — input → graph → portable bundle

  * **`okb extract --corpus docs/ -o out/`** — turns a prose corpus into a resolved
    graph with a local LLM. Five in-process stages: ontology bootstrap →
    grammar-constrained extraction → gleaning (recall) → entity resolution →
    relation normalization. Emits `nodes.ndjson`/`edges.ndjson`/`domain.yaml`/
    `vocab.txt`; `--ingest` loads straight into DuckDB. Code: `cli/extract/`.
  * **`okb ingest --nodes n.ndjson --edges e.ndjson`** — loads pre-structured
    NDJSON/JSON. Embeds `semantic_text` via the external endpoint, batch-inserts
    nodes/edges with SCD Type 2 timestamps, and rebuilds the FTS index.
  * **`okb bundle -o bundle/ [--skill] [--no-db]`** — packs the graph into an OKF
    bundle: `index.md` + `log.md` + `catalog/` (per-type/relationship concepts) +
    `<NodeType>/` per-node concept docs with edges as bundle-relative cross-links.
    The DuckDB database and config are included by default (`--no-db` for markdown
    only); `--skill` adds a self-describing agent `SKILL.md`. Code: `cli/cmd/okf.go`.

### INSPECT — validate the graph

  * **`okb query --text "search" [--date 2025-01-01]`** — hybrid search with an
    optional temporal filter.
  * **`okb graph --sql "FROM GRAPH_TABLE(...)"`** — raw SQL/PGQ traversals.
  * **`okb schema`** — LLM-friendly schema readout with query examples.

### CONSUME — fully-local agent

  * **`okb agent --bundle ./bundle [--ask "q" --json]`** — chat TUI or one-shot
    answer over a bundle. Inference + embeddings run on-device via kronk; the agent
    calls `schema`, `sql_query` (read-only), `hybrid_search`, and
    `list_docs`/`search_docs`/`read_doc`. Settings persist in
    `~/.config/okb/config.yaml`. Code: `cli/agent/`.

### Quarantined namespaces

  * **`okb bench answer|eval|convert`** — research/benchmark scaffolding that
    evaluates the local agent; not part of the build pipeline. Code: `cli/eval/`,
    `cli/metaqa/`.
  * **`okb site generate|serve`** — a hosted graph viewer (static site or live HTTP
    API/UI), distinct from the portable bundle.
