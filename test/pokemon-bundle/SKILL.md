---
name: pokemon-world-graph
description: Explore and query the pokemon_world knowledge graph (36 entities across 4 types, 66 relationships). Pairs browsable OKF markdown concepts with a DuckDB database for hybrid vector + lexical + graph search. Use when answering questions about pokemon_world, looking up entities and how they relate, or running graph/semantic queries.
type: Skill
---

# pokemon_world knowledge graph

This bundle is an [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md) (OKF v0.1) corpus describing a temporal knowledge graph. Markdown gives you human/agent-readable orientation; the bundled DuckDB database gives you precise, queryable retrieval.

## What's in this bundle

* `index.md` — root listing of everything available (start here).
* `catalog/` — one concept per node type and relationship type: fields, semantic fields, counts, examples.
* `<NodeType>/` — one markdown concept per entity, with its properties and relationships as cross-links.
* `log.md` — generation history.
* `pokemon.duckdb` — the knowledge graph database (DuckDB).
* `domain.yaml` — the domain config (node/edge definitions, embedding model).

## Entity types

* **Pokemon** — 20 (`catalog/Pokemon.md`)
* **Region** — 3 (`catalog/Region.md`)
* **Trainer** — 5 (`catalog/Trainer.md`)
* **Type** — 8 (`catalog/Type.md`)

Relationship types: `EVOLVES_TO`, `FOUND_IN`, `HAS_TYPE`, `OWNED_BY`

## How to use it

1. **Orient** — read `index.md`, then the relevant `catalog/<Type>.md` to learn the shape of the data.
2. **Browse** — open individual `<NodeType>/<id>.md` concepts and follow relationship links between them.
3. **Query** — for precise lookups, joins, or aggregation, query the database directly (below).

## Querying the database

The graph is stored in two DuckDB tables, `Nodes_Base` and `Edges_Base`, with SCD Type 2 temporal tracking (filter `is_current = TRUE` for the present state).

Raw DuckDB SQL (portable — needs only the `duckdb` CLI):

```sql
-- entities of a type
SELECT node_id, properties FROM Nodes_Base
WHERE is_current AND node_type = 'Pokemon' LIMIT 20;

-- a node's relationships
SELECT relationship_type, target_id FROM Edges_Base
WHERE is_current AND source_id = 'pokemon:001';
```

Hybrid search (BM25 + vector + graph) via the `okb` CLI, run from this directory:

```bash
okb query --config domain.yaml --text "your question" --limit 10
okb graph --config domain.yaml --sql "FROM GRAPH_TABLE(domain_graph MATCH (a:\"node\")-[e:\"edge\"]->(b:\"node\") COLUMNS (a.node_id, e.relationship_type, b.node_id)) LIMIT 10"
```

> Vector/semantic search needs the embedding endpoint from the domain config to be reachable; lexical and graph queries work offline against the database alone.
