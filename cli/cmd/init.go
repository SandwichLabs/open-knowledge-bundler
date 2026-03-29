package cmd

import (
	"fmt"
	"log"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize the DuckDB database and load extensions",
	Long:  "Creates the local .duckdb file, loads vss/fts/duckpgq extensions, and creates base tables from the domain config.",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		embDim := viper.GetInt("embedding_dim")
		if embDim == 0 {
			embDim = 768
		}

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
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

		log.Printf("Initialized database at %s (embedding_dim=%d)", dbPath, embDim)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
