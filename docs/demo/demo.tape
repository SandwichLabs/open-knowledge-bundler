Set Shell bash

Output demo.gif

Set FontSize 16
Set Width 1400
Set Height 900
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Padding 20

Hide
Type `cd /home/zac/projects/sandwich-labs/chicago_business_intelligence/test && clear`
Enter
Sleep 1s
Show

Type `# cbi -- DuckDB Knowledge Graph CLI`
Enter
Type `# Hybrid Search (VSS + FTS + RRF) | SQL/PGQ Graph Queries | Temporal Tracking`
Enter
Sleep 2s

Enter

Type `# 1. Initialize database -- load vss, fts, spatial, duckpgq extensions`
Enter
Sleep 1s

Type `rm -f pokemon.duckdb && ../cli/cbi init --config domain.yaml`
Enter
Sleep 4s

Enter

Type `# 2. Ingest nodes -- 36 nodes (Pokemon, Types, Regions, Trainers) with gemma embeddings`
Enter
Sleep 1s

Type `../cli/cbi ingest --nodes nodes.ndjson --config domain.yaml --batch-size 10`
Enter
Sleep 12s

Enter

Type `# 3. Ingest edges -- 66 relationships (HAS_TYPE, EVOLVES_TO, FOUND_IN, OWNED_BY)`
Enter
Sleep 1s

Type `../cli/cbi ingest --edges edges.ndjson --config domain.yaml`
Enter
Sleep 4s

Enter

Type `# 4. Schema -- LLM-friendly readout of the knowledge graph`
Enter
Sleep 1s

Type `../cli/cbi schema --config domain.yaml | head -30`
Enter
Sleep 4s

Enter

Type `# 5. Hybrid Search -- "fire breathing dragon"`
Enter
Sleep 1s

Type `../cli/cbi query --text "fire breathing dragon" --config domain.yaml --limit 3`
Enter
Sleep 4s

Enter

Type `# 6. Hybrid Search -- "psychic legendary genetic experiment"`
Enter
Sleep 1s

Type `../cli/cbi query --text "psychic legendary genetic experiment" --config domain.yaml --limit 3`
Enter
Sleep 4s

Enter

Type `# 7. Graph Query -- What Pokemon does Ash own?`
Enter
Sleep 1s

Type@80ms `../cli/cbi graph --config domain.yaml --sql "FROM GRAPH_TABLE(domain_graph MATCH (p:\"node\")-[e:\"edge\"]->(t:\"node\") WHERE t.node_id = 'trainer:ash' AND e.relationship_type = 'OWNED_BY' COLUMNS (p.properties->>'name' AS pokemon))"`
Enter
Sleep 4s

Enter

Type `# 8. Multi-hop -- Ash's Pokemon and their types`
Enter
Sleep 1s

Type@80ms `../cli/cbi graph --config domain.yaml --sql "FROM GRAPH_TABLE(domain_graph MATCH (p:\"node\")-[o:\"edge\"]->(t:\"node\"), (p:\"node\")-[h:\"edge\"]->(tp:\"node\") WHERE t.node_id = 'trainer:ash' AND o.relationship_type = 'OWNED_BY' AND h.relationship_type = 'HAS_TYPE' COLUMNS (p.properties->>'name' AS pokemon, tp.properties->>'type_name' AS type)) ORDER BY pokemon"`
Enter
Sleep 4s

Enter

Type `# 9. 3-hop -- Full evolution chains`
Enter
Sleep 1s

Type@80ms `../cli/cbi graph --config domain.yaml --sql "FROM GRAPH_TABLE(domain_graph MATCH (a:\"node\")-[e1:\"edge\"]->(b:\"node\")-[e2:\"edge\"]->(c:\"node\") WHERE e1.relationship_type = 'EVOLVES_TO' AND e2.relationship_type = 'EVOLVES_TO' COLUMNS (a.properties->>'name' AS stage1, b.properties->>'name' AS stage2, c.properties->>'name' AS stage3)) ORDER BY stage1"`
Enter
Sleep 5s

Enter
Type `# Done -- hybrid search + graph queries in a single DuckDB instance`
Enter
Sleep 3s
