package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	queryText  string
	queryDate  string
	queryLimit int
)

var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "Search the knowledge graph using hybrid search",
	Long:  "Embeds the query text, runs lexical + semantic search with RRF fusion, then traverses the property graph filtered by the given date.",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		endpointURL := viper.GetString("endpoint_url")
		model := viper.GetString("embedding_model")

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}

		emb, cleanup, err := newEmbedder(viper.GetInt("embedding_dim"), endpointURL, model)
		if err != nil {
			return err
		}
		defer cleanup()
		queryVec, err := emb.Embed(cmd.Context(), queryText)
		if err != nil {
			return fmt.Errorf("embedding query: %w", err)
		}

		var dateFilter *time.Time
		if queryDate != "" {
			t, err := time.Parse("2006-01-02", queryDate)
			if err != nil {
				return fmt.Errorf("parsing --date: %w", err)
			}
			dateFilter = &t
		}

		results, err := db.HybridSearch(queryText, queryVec, dateFilter, queryLimit)
		if err != nil {
			return fmt.Errorf("hybrid search: %w", err)
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	},
}

func init() {
	queryCmd.Flags().StringVar(&queryText, "text", "", "search query text (required)")
	queryCmd.Flags().StringVar(&queryDate, "date", "", "temporal filter date (YYYY-MM-DD)")
	queryCmd.Flags().IntVar(&queryLimit, "limit", 10, "max results to return")
	queryCmd.Flags().StringVar(&embedMode, "embed", "local", "embedding backend: local (in-process kronk) or endpoint (OpenAI-compatible endpoint_url)")
	_ = queryCmd.MarkFlagRequired("text")
	rootCmd.AddCommand(queryCmd)
}
