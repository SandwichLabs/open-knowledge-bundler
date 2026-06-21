package cmd

import (
	"fmt"
	"log"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// initializeDB creates the base schema, vector index, and property graph on an
// already-opened, extensions-loaded database. All three operations are idempotent
// (CREATE ... IF NOT EXISTS / CREATE OR REPLACE), so it is safe to call on an
// already-initialized database — which is why ingest (and, later, extract) run it
// automatically rather than requiring a separate `init` step.
func initializeDB(db *store.DB, embDim int) error {
	if embDim == 0 {
		embDim = 768
	}
	if err := db.CreateSchema(embDim); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	if err := db.CreateIndexes(embDim); err != nil {
		return fmt.Errorf("creating indexes: %w", err)
	}
	if err := db.CreatePropertyGraph(); err != nil {
		return fmt.Errorf("creating property graph: %w", err)
	}
	return nil
}

// initCmd is retained (hidden) as an escape hatch / back-compat alias. The build
// pipeline (ingest, extract) initializes the database automatically, so this is no
// longer part of the visible surface.
var initCmd = &cobra.Command{
	Use:    "init",
	Short:  "Initialize the DuckDB database and load extensions (usually automatic)",
	Long:   "Creates the local .duckdb file, loads vss/fts/duckpgq extensions, and creates base tables from the domain config. ingest/extract do this automatically; you rarely need to run it directly.",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		embDim := viper.GetInt("embedding_dim")

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}
		if err := initializeDB(db, embDim); err != nil {
			return err
		}

		log.Printf("Initialized database at %s", dbPath)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
