package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/domain"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/embed"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// ingestPayload is the expected shape when using a single JSON file.
type ingestPayload struct {
	Nodes []domain.Node `json:"nodes"`
	Edges []domain.Edge `json:"edges"`
}

var (
	nodesFile  string
	edgesFile  string
	ingestFile string
	ingestTime string
	batchSize  int
)

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Ingest data into the knowledge graph",
	Long: `Reads node and edge data, generates embeddings via the configured endpoint,
and batch-inserts into DuckDB with temporal tracking.

Supports two modes:
  --file data.json          Single JSON file with {"nodes": [...], "edges": [...]}
  --nodes n.ndjson --edges e.ndjson   Separate NDJSON files (one JSON object per line)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		endpointURL := viper.GetString("endpoint_url")
		model := viper.GetString("embedding_model")

		ts, err := time.Parse("2006-01-02", ingestTime)
		if err != nil {
			return fmt.Errorf("parsing --time: %w", err)
		}

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}

		client := embed.NewClient(endpointURL, model)

		// Determine ingestion mode.
		if ingestFile != "" {
			return ingestSingleJSON(db, client, ts)
		}
		if nodesFile != "" || edgesFile != "" {
			return ingestNDJSON(db, client, ts)
		}
		return fmt.Errorf("provide either --file or --nodes/--edges")
	},
}

// ingestSingleJSON handles the original single-JSON-file mode.
func ingestSingleJSON(db *store.DB, client *embed.Client, ts time.Time) error {
	raw, err := os.ReadFile(ingestFile)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}
	var payload ingestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	if err := embedNodes(payload.Nodes, client); err != nil {
		return err
	}
	stampNodes(payload.Nodes, ts)
	stampEdges(payload.Edges, ts)

	if err := db.UpsertNodes(payload.Nodes, ts); err != nil {
		return fmt.Errorf("upserting nodes: %w", err)
	}
	if err := db.UpsertEdges(payload.Edges, ts); err != nil {
		return fmt.Errorf("upserting edges: %w", err)
	}
	if err := db.RebuildFTSIndex(); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}

	log.Printf("Ingested %d nodes and %d edges at %s", len(payload.Nodes), len(payload.Edges), ts.Format("2006-01-02"))
	return nil
}

// ingestNDJSON reads NDJSON files and ingests in batches.
func ingestNDJSON(db *store.DB, client *embed.Client, ts time.Time) error {
	var totalNodes, totalEdges int

	// Ingest nodes first (edges reference them).
	if nodesFile != "" {
		log.Printf("Ingesting nodes from %s (batch_size=%d)...", nodesFile, batchSize)
		count, err := processNDJSONFile(nodesFile, batchSize, func(batch []json.RawMessage) error {
			nodes := make([]domain.Node, 0, len(batch))
			for _, raw := range batch {
				var n domain.Node
				if err := json.Unmarshal(raw, &n); err != nil {
					return fmt.Errorf("parsing node: %w", err)
				}
				nodes = append(nodes, n)
			}

			if err := embedNodes(nodes, client); err != nil {
				return err
			}
			stampNodes(nodes, ts)

			return db.UpsertNodes(nodes, ts)
		})
		if err != nil {
			return fmt.Errorf("ingesting nodes: %w", err)
		}
		totalNodes = count
		log.Printf("  %d nodes ingested", totalNodes)
	}

	// Ingest edges.
	if edgesFile != "" {
		log.Printf("Ingesting edges from %s (batch_size=%d)...", edgesFile, batchSize)
		count, err := processNDJSONFile(edgesFile, batchSize, func(batch []json.RawMessage) error {
			edges := make([]domain.Edge, 0, len(batch))
			for _, raw := range batch {
				var e domain.Edge
				if err := json.Unmarshal(raw, &e); err != nil {
					return fmt.Errorf("parsing edge: %w", err)
				}
				edges = append(edges, e)
			}
			stampEdges(edges, ts)
			return db.UpsertEdges(edges, ts)
		})
		if err != nil {
			return fmt.Errorf("ingesting edges: %w", err)
		}
		totalEdges = count
		log.Printf("  %d edges ingested", totalEdges)
	}

	// Rebuild FTS after all data is loaded.
	if err := db.RebuildFTSIndex(); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}

	log.Printf("Ingested %d nodes and %d edges at %s", totalNodes, totalEdges, ts.Format("2006-01-02"))
	return nil
}

// processNDJSONFile reads a file line-by-line and calls fn for each batch.
// Returns total line count processed.
func processNDJSONFile(path string, batch int, fn func([]json.RawMessage) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow large lines (up to 10MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var buf []json.RawMessage
	total := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		buf = append(buf, json.RawMessage(line))
		total++

		if len(buf) >= batch {
			if err := fn(buf); err != nil {
				return total, err
			}
			buf = buf[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return total, err
	}
	if len(buf) > 0 {
		if err := fn(buf); err != nil {
			return total, err
		}
	}
	return total, nil
}

func embedNodes(nodes []domain.Node, client *embed.Client) error {
	ctx := context.Background()
	for i := range nodes {
		n := &nodes[i]
		if n.SemanticText != "" && len(n.Embedding) == 0 {
			vec, err := client.Embed(ctx, n.SemanticText)
			if err != nil {
				return fmt.Errorf("embedding node %s: %w", n.NodeID, err)
			}
			n.Embedding = vec
		}
	}
	return nil
}

func stampNodes(nodes []domain.Node, ts time.Time) {
	for i := range nodes {
		nodes[i].ValidFrom = ts
		nodes[i].IsCurrent = true
	}
}

func stampEdges(edges []domain.Edge, ts time.Time) {
	for i := range edges {
		edges[i].ValidFrom = ts
		edges[i].IsCurrent = true
	}
}

func init() {
	ingestCmd.Flags().StringVar(&ingestFile, "file", "", "single JSON file with {nodes, edges}")
	ingestCmd.Flags().StringVar(&nodesFile, "nodes", "", "NDJSON file of nodes (one per line)")
	ingestCmd.Flags().StringVar(&edgesFile, "edges", "", "NDJSON file of edges (one per line)")
	ingestCmd.Flags().StringVar(&ingestTime, "time", time.Now().Format("2006-01-02"), "ingestion timestamp (YYYY-MM-DD)")
	ingestCmd.Flags().IntVar(&batchSize, "batch-size", 500, "records per batch for NDJSON ingestion")
	rootCmd.AddCommand(ingestCmd)
}
