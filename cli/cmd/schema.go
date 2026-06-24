package cmd

import (
	"fmt"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Display the knowledge graph schema, metrics, and query guide",
	Long:  "Returns an LLM-friendly readout of node types, edge types, cardinalities, sample properties, and example queries.",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}

		info, err := buildSchemaOutput(db)
		if err != nil {
			return err
		}

		fmt.Print(info)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(schemaCmd)
}

func buildSchemaOutput(db *store.DB) (string, error) {
	var b strings.Builder

	b.WriteString("# Knowledge Graph Schema\n\n")

	// Node types
	nodeTypes, err := queryStringRows(db, `
		SELECT node_type, COUNT(*) as count,
		       COUNT(*) FILTER (WHERE embedding IS NOT NULL) as with_embeddings,
		       COUNT(*) FILTER (WHERE latitude IS NOT NULL) as with_coordinates
		FROM Nodes_Base WHERE is_current = TRUE
		GROUP BY node_type ORDER BY count DESC
	`)
	if err != nil {
		return "", fmt.Errorf("querying node types: %w", err)
	}

	b.WriteString("## Node Types\n\n")
	b.WriteString("| Type | Count | With Embeddings | With Coordinates |\n")
	b.WriteString("|------|------:|----------------:|-----------------:|\n")
	for _, row := range nodeTypes {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
			row["node_type"], row["count"], row["with_embeddings"], row["with_coordinates"]))
	}

	// Sample properties per node type
	b.WriteString("\n## Node Properties (sample per type)\n\n")
	for _, row := range nodeTypes {
		nt := row["node_type"]
		props, err := queryStringRows(db, fmt.Sprintf(`
			SELECT json_keys(properties) as keys
			FROM Nodes_Base WHERE node_type = '%s' AND is_current = TRUE LIMIT 1
		`, nt))
		if err == nil && len(props) > 0 {
			b.WriteString(fmt.Sprintf("- **%s**: `%v`\n", nt, props[0]["keys"]))
		}
	}

	// Edge types
	edgeTypes, err := queryStringRows(db, `
		SELECT relationship_type, COUNT(*) as count,
		       ROUND(AVG(weight), 3) as avg_weight,
		       COUNT(DISTINCT source_id) as distinct_sources,
		       COUNT(DISTINCT target_id) as distinct_targets
		FROM Edges_Base WHERE is_current = TRUE
		GROUP BY relationship_type ORDER BY count DESC
	`)
	if err != nil {
		return "", fmt.Errorf("querying edge types: %w", err)
	}

	b.WriteString("\n## Edge Types\n\n")
	b.WriteString("| Relationship | Count | Avg Weight | Distinct Sources | Distinct Targets |\n")
	b.WriteString("|-------------|------:|-----------:|-----------------:|-----------------:|\n")
	for _, row := range edgeTypes {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
			row["relationship_type"], row["count"], row["avg_weight"],
			row["distinct_sources"], row["distinct_targets"]))
	}

	// Edge connectivity patterns
	b.WriteString("\n## Edge Connectivity (source_type → target_type)\n\n")
	connectivity, err := queryStringRows(db, `
		SELECT e.relationship_type,
		       src.node_type as source_type,
		       tgt.node_type as target_type,
		       COUNT(*) as count
		FROM Edges_Base e
		JOIN Nodes_Base src ON e.source_id = src.node_id AND src.is_current
		JOIN Nodes_Base tgt ON e.target_id = tgt.node_id AND tgt.is_current
		WHERE e.is_current = TRUE
		GROUP BY e.relationship_type, src.node_type, tgt.node_type
		ORDER BY count DESC
	`)
	if err == nil {
		b.WriteString("| Edge | Source Type | Target Type | Count |\n")
		b.WriteString("|------|-----------|------------|------:|\n")
		for _, row := range connectivity {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				row["relationship_type"], row["source_type"], row["target_type"], row["count"]))
		}
	}

	// Totals
	totals, err := queryStringRows(db, `
		SELECT
		  (SELECT COUNT(*) FROM Nodes_Base WHERE is_current) as total_nodes,
		  (SELECT COUNT(*) FROM Edges_Base WHERE is_current) as total_edges
	`)
	if err == nil && len(totals) > 0 {
		b.WriteString(fmt.Sprintf("\n## Totals\n\n- **Nodes**: %s\n- **Edges**: %s\n",
			totals[0]["total_nodes"], totals[0]["total_edges"]))
	}

	// Query guide
	b.WriteString(`
## Query Guide

### Hybrid Search (semantic + lexical with RRF fusion)
`)
	b.WriteString("```\n")
	b.WriteString("okb query --text \"your natural language query\" --limit 10\n")
	b.WriteString("okb query --text \"your query\" --date 2025-01-01  # temporal filter\n")
	b.WriteString("```\n")

	b.WriteString(`
### Graph Queries (SQL/PGQ via duckpgq)

Single hop — find connected nodes:
`)
	b.WriteString("```sql\n")
	b.WriteString(`okb graph --sql "
FROM GRAPH_TABLE(domain_graph
  MATCH (a:\"node\")-[e:\"edge\"]->(b:\"node\")
  WHERE a.node_id = 'some:id' AND e.relationship_type = 'EDGE_TYPE'
  COLUMNS (b.node_id, b.node_type, b.properties->>'name' AS name)
)"
`)
	b.WriteString("```\n")

	b.WriteString(`
Multi-hop — traverse paths:
`)
	b.WriteString("```sql\n")
	b.WriteString(`okb graph --sql "
FROM GRAPH_TABLE(domain_graph
  MATCH (a:\"node\")-[e1:\"edge\"]->(b:\"node\")-[e2:\"edge\"]->(c:\"node\")
  WHERE a.node_id = 'some:id'
    AND e1.relationship_type = 'TYPE1'
    AND e2.relationship_type = 'TYPE2'
  COLUMNS (a.properties->>'name' AS src, b.properties->>'name' AS mid, c.properties->>'name' AS dst)
)"
`)
	b.WriteString("```\n")

	b.WriteString(`
Fan-out — multiple edge types from same node:
`)
	b.WriteString("```sql\n")
	b.WriteString(`okb graph --sql "
FROM GRAPH_TABLE(domain_graph
  MATCH (p:\"node\")-[e1:\"edge\"]->(t1:\"node\"), (p:\"node\")-[e2:\"edge\"]->(t2:\"node\")
  WHERE p.node_id = 'some:id'
    AND e1.relationship_type = 'TYPE1'
    AND e2.relationship_type = 'TYPE2'
  COLUMNS (t1.properties->>'name' AS result1, t2.properties->>'name' AS result2)
)"
`)
	b.WriteString("```\n")

	b.WriteString(`
### Plain SQL (direct table access)
`)
	b.WriteString("```sql\n")
	b.WriteString("okb graph --sql \"SELECT * FROM Nodes_Base WHERE node_type = 'Pokemon' AND is_current LIMIT 5\"\n")
	b.WriteString("okb graph --sql \"SELECT * FROM Edges_Base WHERE relationship_type = 'EVOLVES_TO' AND is_current\"\n")
	b.WriteString("```\n")

	b.WriteString(`
### Important Notes
- All MATCH patterns must label every node as ` + "`\"node\"`" + ` and every edge as ` + "`\"edge\"`" + `
- Use ` + "`properties->>'key'`" + ` to extract JSON fields as text
- Use ` + "`properties->'key'`" + ` to extract JSON fields preserving type (arrays, objects)
- Temporal filter: ` + "`is_current = TRUE`" + ` or ` + "`valid_from <= ts AND (valid_to IS NULL OR valid_to > ts)`" + `
`)

	return b.String(), nil
}

// queryStringRows runs a query and returns results as string maps for easy formatting.
func queryStringRows(db *store.DB, query string) ([]map[string]string, error) {
	rows, err := db.RawQuery(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]string
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]string, len(cols))
		for i, col := range cols {
			row[col] = fmt.Sprintf("%v", values[i])
		}
		results = append(results, row)
	}
	return results, rows.Err()
}
