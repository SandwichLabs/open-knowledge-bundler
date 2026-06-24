package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	graphSQL string
)

var graphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Execute SQL/PGQ graph queries against the knowledge graph",
	Long: `Runs arbitrary SQL (including GRAPH_TABLE / MATCH patterns) against the
knowledge graph database with all extensions pre-loaded.

Examples:
  okb graph --sql "FROM GRAPH_TABLE(domain_graph MATCH (a)-[e]->(b) WHERE a.node_id = 'pokemon:006' COLUMNS (b.node_id, b.node_type, e.relationship_type)) ORDER BY e.relationship_type"

  okb graph --sql "SELECT node_type, COUNT(*) FROM Nodes_Base GROUP BY 1"`,
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

		// If --sql not provided, check for positional args joined as query.
		query := graphSQL
		if query == "" && len(args) > 0 {
			query = strings.Join(args, " ")
		}
		if query == "" {
			return fmt.Errorf("provide a query via --sql or as arguments")
		}

		rows, err := db.RawQuery(query)
		if err != nil {
			return fmt.Errorf("executing query: %w", err)
		}
		defer rows.Close()

		results, err := rowsToMaps(rows)
		if err != nil {
			return err
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	},
}

// rowsToMaps converts sql.Rows into a slice of maps using column names.
func rowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("getting columns: %w", err)
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = values[i]
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func init() {
	graphCmd.Flags().StringVar(&graphSQL, "sql", "", "SQL/PGQ query to execute")
	rootCmd.AddCommand(graphCmd)
}
