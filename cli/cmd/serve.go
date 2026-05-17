package cmd

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/embed"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//go:embed web.html
var webHTML []byte

var (
	serveAddr string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run an HTTP server exposing /api/query, /api/node, /api/sql, /api/stats and a UI at /",
	Long: `Wraps the existing query, graph, and store code behind a thin HTTP layer so a
browser can drive the knowledge graph without loading embeddings client-side.

Endpoints:
  GET /                  - search UI
  GET /api/stats         - counts by node_type + relationship_type
  GET /api/query         - ?text=...&limit=N&date=YYYY-MM-DD (hybrid search)
  GET /api/node          - ?id=<node_id> (node + one-hop neighborhood)
  GET /api/sql           - ?q=<sql> (raw SQL/PGQ; read-only enforced)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		endpointURL := viper.GetString("endpoint_url")
		model := viper.GetString("embedding_model")
		domainName := viper.GetString("domain_name")

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()
		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}

		client := embed.NewClient(endpointURL, model)

		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(webHTML)
		})

		mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
			handleStats(w, r, db, domainName)
		})
		mux.HandleFunc("/api/query", func(w http.ResponseWriter, r *http.Request) {
			handleQuery(w, r, db, client)
		})
		mux.HandleFunc("/api/node", func(w http.ResponseWriter, r *http.Request) {
			handleNode(w, r, db)
		})
		mux.HandleFunc("/api/sql", func(w http.ResponseWriter, r *http.Request) {
			handleSQL(w, r, db)
		})
		mux.HandleFunc("/api/preset/list", func(w http.ResponseWriter, r *http.Request) {
			handlePresetList(w, r)
		})
		mux.HandleFunc("/api/preset", func(w http.ResponseWriter, r *http.Request) {
			handlePreset(w, r, db)
		})

		srv := &http.Server{
			Addr:              serveAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("cbi serve listening on http://%s (domain=%s db=%s)", serveAddr, domainName, dbPath)
		return srv.ListenAndServe()
	},
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func handleStats(w http.ResponseWriter, _ *http.Request, db *store.DB, domainName string) {
	stats := map[string]any{"domain": domainName}
	type kv struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	}

	getKV := func(query string) ([]kv, int, error) {
		rows, err := db.RawQuery(query)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		var out []kv
		total := 0
		for rows.Next() {
			var k string
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				return nil, 0, err
			}
			out = append(out, kv{k, c})
			total += c
		}
		return out, total, rows.Err()
	}

	byType, nodeTotal, err := getKV("SELECT node_type, COUNT(*) FROM Nodes_Base WHERE is_current GROUP BY 1 ORDER BY 2 DESC")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	byRel, edgeTotal, err := getKV("SELECT relationship_type, COUNT(*) FROM Edges_Base WHERE is_current GROUP BY 1 ORDER BY 2 DESC")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	stats["nodes"] = nodeTotal
	stats["edges"] = edgeTotal
	stats["by_node_type"] = byType
	stats["by_relationship_type"] = byRel
	writeJSON(w, 200, stats)
}

func handleQuery(w http.ResponseWriter, r *http.Request, db *store.DB, client *embed.Client) {
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if text == "" {
		writeErr(w, 400, fmt.Errorf("missing ?text"))
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	var dateFilter *time.Time
	if v := r.URL.Query().Get("date"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("invalid ?date: %w", err))
			return
		}
		dateFilter = &t
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	queryVec, err := client.Embed(ctx, text)
	if err != nil {
		writeErr(w, 502, fmt.Errorf("embedding: %w", err))
		return
	}
	results, err := db.HybridSearch(text, queryVec, dateFilter, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	// HybridSearch returns SearchResult with properties as JSON string; decode for the UI.
	type out struct {
		NodeID       string         `json:"node_id"`
		NodeType     string         `json:"node_type"`
		Properties   map[string]any `json:"properties"`
		SemanticText string         `json:"semantic_text"`
		RrfScore     float64        `json:"rrf_score"`
	}
	resp := make([]out, 0, len(results))
	for _, r := range results {
		resp = append(resp, out{r.NodeID, r.NodeType, r.Properties, r.SemanticText, r.RRFScore})
	}
	writeJSON(w, 200, resp)
}

func handleNode(w http.ResponseWriter, r *http.Request, db *store.DB) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, 400, fmt.Errorf("missing ?id"))
		return
	}
	// Node itself.
	nodeRows, err := db.RawQuery(fmt.Sprintf(
		"SELECT node_id, node_type, properties::VARCHAR, semantic_text FROM Nodes_Base WHERE is_current AND node_id = '%s' LIMIT 1",
		escapeSQL(id),
	))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	defer nodeRows.Close()
	var node map[string]any
	if nodeRows.Next() {
		var nid, ntype, props, sem string
		if err := nodeRows.Scan(&nid, &ntype, &props, &sem); err != nil {
			writeErr(w, 500, err)
			return
		}
		var pmap map[string]any
		_ = json.Unmarshal([]byte(props), &pmap)
		node = map[string]any{
			"node_id":       nid,
			"node_type":     ntype,
			"properties":    pmap,
			"semantic_text": sem,
		}
	}
	nodeRows.Close()
	if node == nil {
		writeErr(w, 404, fmt.Errorf("not found"))
		return
	}

	// One-hop neighborhood (both directions).
	edgeQuery := fmt.Sprintf(`
		SELECT 'out' AS direction, e.relationship_type, n.node_id, n.node_type, n.properties::VARCHAR
		FROM Edges_Base e
		JOIN Nodes_Base n ON e.target_id = n.node_id AND n.is_current
		WHERE e.is_current AND e.source_id = '%[1]s'
		UNION ALL
		SELECT 'in' AS direction, e.relationship_type, n.node_id, n.node_type, n.properties::VARCHAR
		FROM Edges_Base e
		JOIN Nodes_Base n ON e.source_id = n.node_id AND n.is_current
		WHERE e.is_current AND e.target_id = '%[1]s'
		LIMIT 200`, escapeSQL(id))
	rows, err := db.RawQuery(edgeQuery)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	defer rows.Close()
	edges := []map[string]any{}
	for rows.Next() {
		var dir, rel, oid, otype, oprops string
		if err := rows.Scan(&dir, &rel, &oid, &otype, &oprops); err != nil {
			writeErr(w, 500, err)
			return
		}
		var op map[string]any
		_ = json.Unmarshal([]byte(oprops), &op)
		edges = append(edges, map[string]any{
			"direction":         dir,
			"relationship_type": rel,
			"other_id":          oid,
			"other_type":        otype,
			"other_props":       op,
		})
	}
	writeJSON(w, 200, map[string]any{"node": node, "edges": edges})
}

func handleSQL(w http.ResponseWriter, r *http.Request, db *store.DB) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, 400, fmt.Errorf("missing ?q"))
		return
	}
	// Read-only guardrail.
	upper := strings.ToUpper(strings.TrimSpace(q))
	for _, banned := range []string{"INSERT ", "UPDATE ", "DELETE ", "DROP ", "ALTER ", "CREATE ", "ATTACH ", "COPY ", "PRAGMA "} {
		if strings.Contains(upper, banned) {
			writeErr(w, 400, fmt.Errorf("only read-only queries allowed (blocked %s)", strings.TrimSpace(banned)))
			return
		}
	}
	rows, err := db.RawQuery(q)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	results := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeErr(w, 500, err)
			return
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = values[i]
		}
		results = append(results, row)
		if len(results) >= 1000 {
			break
		}
	}
	writeJSON(w, 200, results)
}

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:8765", "listen address")
	rootCmd.AddCommand(serveCmd)
}
