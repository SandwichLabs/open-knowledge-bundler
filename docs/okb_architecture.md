Here is a comprehensive Domain-Driven Design (DDD) and Information Architecture document for building your general-purpose, local-first GraphRAG and hybrid search framework.

This guide strips away specific industry constraints, providing your engineering team with a fully configurable, temporally aware foundation using Go and DuckDB.

-----

## **1. Architectural Overview**

This system is a local-first, CLI-driven application designed to construct, analyze, and query domain-agnostic knowledge graphs. It relies on a "Chimera" architecture, unifying relational data, vector embeddings, and property graphs within a single DuckDB instance[cite: 1].

  * **Core Engine:** DuckDB.
  * **Graph Traversal:** Handled by the `duckpgq` extension, executing SQL/PGQ syntax over relational tables[cite: 1].
  * **Semantic Search:** Powered by the `vss` extension for Approximate Nearest Neighbor (ANN) indexing (HNSW) and `array_cosine_distance` calculations[cite: 1, 2].
  * **Lexical Search:** Managed by DuckDB's `fts` extension using the `match_bm25` macro[cite: 2].
  * **Embeddings:** Generated via an external OpenAI-compatible endpoint (e.g., Ollama, vLLM) before being stored in DuckDB as `FLOAT[N]` arrays[cite: 1, 2].

-----

## **2. Domain-Driven Design (DDD) Core Contexts**

To make this framework domain-agnostic, the bounded contexts are abstracted into generic primitives defined entirely by user configuration.

### **2.1 Core Entities (Nodes)**

Entities represent the primary nouns in your configured domain (e.g., `Business`, `Resource`, `Person`, `Document`).

  * **Identity:** UUID or strictly formatted unique string.
  * **Properties:** Dynamic JSON payload containing scalar attributes.
  * **Semantic Anchor:** A designated text field (or concatenated fields) that is vectorized to create the `Embedding`[cite: 1].

### **2.2 Relationships (Edges)**

Edges represent the verbs connecting entities (e.g., `LOCATED_IN`, `REPORTS_TO`, `REFERENCES`).

  * **Directionality:** Strictly directed (Source -\> Destination).
  * **Properties:** Edges can carry weights, confidence scores, or qualitative metadata.

### **2.3 Temporal Tracking Context**

To support chronological tracking of changes over time, all Entities and Relationships implement a temporal interface (SCD Type 2 - Slowly Changing Dimensions).

  * **ValidFrom / ValidTo:** Timestamps dictating the exact window an entity state or relationship was active.
  * **IsCurrent:** A boolean flag optimizing queries for the present state.

-----

## **3. Information Architecture & Database Schema**

The data layer uses DuckDB's zero-copy interoperability, allowing it to define logical property graphs directly over relational tables[cite: 1].

### **3.1 Relational Substrate**

Your Go application will dynamically generate these tables based on the user's domain configuration.

  * **`Nodes_Base` Table**

      * `node_id` (VARCHAR, Primary Key)
      * `node_type` (VARCHAR) - e.g., "Organization", "Resource"
      * `properties` (JSON)
      * `semantic_text` (VARCHAR) - Target for the FTS index[cite: 2].
      * `embedding` (FLOAT[N]) - Target for the VSS HNSW index[cite: 1, 2].
      * `valid_from` (TIMESTAMP)
      * `valid_to` (TIMESTAMP)
      * `is_current` (BOOLEAN)

  * **`Edges_Base` Table**

      * `edge_id` (VARCHAR, Primary Key)
      * `source_id` (VARCHAR, Foreign Key to Nodes)
      * `target_id` (VARCHAR, Foreign Key to Nodes)
      * `relationship_type` (VARCHAR)
      * `weight` (FLOAT)
      * `valid_from` (TIMESTAMP)
      * `valid_to` (TIMESTAMP)
      * `is_current` (BOOLEAN)

### **3.2 Graph Instantiation (duckpgq)**

Instead of duplicating data, the system will execute a dynamically generated `CREATE PROPERTY GRAPH` statement[cite: 1].

```sql
CREATE PROPERTY GRAPH dynamic_domain_graph
VERTEX TABLES (
    Nodes_Base LABEL dynamic_node_label
)
EDGE TABLES (
    Edges_Base 
        SOURCE KEY (source_id) REFERENCES Nodes_Base (node_id)
        DESTINATION KEY (target_id) REFERENCES Nodes_Base (node_id)
        LABEL dynamic_edge_label
);
```

-----

## **4. Go Implementation & Module Outline**

Here is what your engineer needs to scaffold the local-first Go application.

### **4.1 Required Go Modules**

  * **Database Driver:** `[github.com/duckdb/duckdb-go/v2](github.com/duckdb/duckdb-go/v2)` - Required to execute SQL and pragmas for the `fts` and `vss` extensions[cite: 2].
  * **CLI Framework:** `[github.com/spf13/cobra](https://github.com/spf13/cobra)` - For routing `init`, `ingest`, and `query` CLI commands.
  * **Configuration:** \`[github.com/spf13/viper](https://github.com/spf13/viper)` - To parse dynamic domain schemas from YAML/JSON.
*   **HTTP Client:** standard `net/http` - For calling Ollama/vLLM endpoints.

### **4.2 Core Struct Mapping**

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
    Embedding    []float32              `json:"embedding"` // Populated via external LLM[cite: 2]
    TemporalRecord
}

// Edge represents a chronological relationship
type Edge struct {
    EdgeID           string                 `json:"edge_id"`
    SourceID         string                 `json:"source_id"`
    TargetID         string                 `json:"target_id"`
    RelationshipType string                 `json:"relationship_type"`
    Weight           float64                `json:"weight"`
    TemporalRecord
}

// DomainConfig dictates how the CLI maps raw data to the graph
type DomainConfig struct {
    DomainName       string            `yaml:"domain_name"`
    EmbeddingDim     int               `yaml:"embedding_dim"` // e.g., 768 or 1536
    EndpointURL      string            `yaml:"endpoint_url"`  // Ollama/vLLM URL
    NodeDefinitions  map[string]Config `yaml:"node_definitions"`
    EdgeDefinitions  map[string]Config `yaml:"edge_definitions"`
}
```

---

## **5. Hybrid Search Pipeline**

To support ambiguous or exploratory queries across the generic domain, the system will use a parallel execution strategy combining Lexical and Semantic search, unified via Reciprocal Rank Fusion (RRF)[cite: 2].

1.  **Lexical CTE:** Uses DuckDB's `fts` extension and `match_bm25` macro to score exact text matches[cite: 1, 2].
2.  **Semantic CTE:** Uses the `vss` extension's `array_cosine_distance` to find conceptually similar records by comparing the user's embedded query against the `embedding` column[cite: 1, 2].
3.  **RRF Fusion:** Normalizes the disparate scoring mechanisms by ranking. The standard RRF formula applied in the fusion CTE is `1 / (k + rank)`[cite: 2].

*Note: DuckDB FTS indexes do not automatically update on data mutation; the CLI must execute `PRAGMA drop_fts_index` and `PRAGMA create_fts_index` after batch ingestions[cite: 2].*

---

## **6. CLI Application Flow**

Your engineer should implement the following primary CLI commands:

*   **`okb init --config domain.yaml`**: Initializes the local `domain.duckdb` file, loads the `vss`, `fts`, and `duckpgq` extensions[cite: 1, 2], and creates the base tables mapped to the YAML definitions.
*   **`okb ingest --file data.json --time "2026-03-29"`**: Reads incoming JSON data. Uses `net/http` to send text to the Ollama/vLLM endpoint, receives `[]float32` arrays[cite: 2], and batch-inserts the Nodes and Edges. Updates `valid_to` on existing records to maintain chronological history.
*   **`okb query --text "search query" --date "2025-01-01"`**: 
    1. Embeds the query via the external endpoint.
    2. Runs the Hybrid Search CTE to find entry nodes.
    3. Executes a SQL/PGQ traversal (e.g., `MATCH (a)->(b)`) restricted by the provided `--date` constraint against the `valid_from`/`valid_to` fields[cite: 1].
