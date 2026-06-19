package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SearchResult is a single node returned by hybrid search.
type SearchResult struct {
	NodeID       string         `json:"node_id"`
	NodeType     string         `json:"node_type"`
	Properties   map[string]any `json:"properties"`
	SemanticText string         `json:"semantic_text"`
	RRFScore     float64        `json:"rrf_score"`
}

// RebuildFTSIndex drops and recreates the full-text search index on Nodes_Base.
func (db *DB) RebuildFTSIndex() error {
	// Drop existing index (ignore error if it doesn't exist).
	_, _ = db.conn.Exec("PRAGMA drop_fts_index('Nodes_Base')")

	_, err := db.conn.Exec("PRAGMA create_fts_index('Nodes_Base', 'node_id', 'semantic_text')")
	if err != nil {
		return fmt.Errorf("creating FTS index: %w", err)
	}
	return nil
}

// HybridSearch runs parallel lexical (BM25) and semantic (cosine) search,
// fused via Reciprocal Rank Fusion (RRF).
func (db *DB) HybridSearch(text string, queryVec []float32, dateFilter *time.Time, limit int) ([]SearchResult, error) {
	// Build the embedding literal for DuckDB: [0.1, 0.2, ...]::FLOAT[N]
	vecParts := make([]string, len(queryVec))
	for i, v := range queryVec {
		vecParts[i] = fmt.Sprintf("%f", v)
	}
	vecLiteral := fmt.Sprintf("[%s]::FLOAT[%d]", strings.Join(vecParts, ","), len(queryVec))

	temporalClause := "is_current = TRUE"
	if dateFilter != nil {
		ts := dateFilter.Format("2006-01-02 15:04:05")
		temporalClause = fmt.Sprintf("valid_from <= '%s' AND (valid_to IS NULL OR valid_to > '%s')", ts, ts)
	}

	query := fmt.Sprintf(`
		WITH lexical AS (
			SELECT node_id, ROW_NUMBER() OVER (ORDER BY score DESC) AS rank
			FROM (
				SELECT *, fts_main_Nodes_Base.match_bm25(node_id, $1) AS score
				FROM Nodes_Base
				WHERE %s AND score IS NOT NULL
			)
		),
		semantic AS (
			SELECT node_id, ROW_NUMBER() OVER (ORDER BY dist ASC) AS rank
			FROM (
				SELECT node_id, array_cosine_distance(embedding, %s) AS dist
				FROM Nodes_Base
				WHERE %s
			)
		),
		fused AS (
			SELECT
				COALESCE(l.node_id, s.node_id) AS node_id,
				COALESCE(1.0 / (60 + l.rank), 0) + COALESCE(1.0 / (60 + s.rank), 0) AS rrf_score
			FROM lexical l
			FULL OUTER JOIN semantic s ON l.node_id = s.node_id
			ORDER BY rrf_score DESC
			LIMIT %d
		)
		SELECT n.node_id, n.node_type, n.properties, n.semantic_text, f.rrf_score
		FROM fused f
		JOIN Nodes_Base n ON f.node_id = n.node_id AND n.%s
		ORDER BY f.rrf_score DESC;
	`, temporalClause, vecLiteral, temporalClause, limit, temporalClause)

	rows, err := db.conn.Query(query, text)
	if err != nil {
		return nil, fmt.Errorf("executing hybrid search: %w", err)
	}
	defer rows.Close()

	return scanResults(rows)
}

// ScanSearchRows scans rows whose columns are (node_id, node_type, properties,
// semantic_text, rrf_score) into SearchResults. Exposed for callers that build
// their own search queries (e.g. a lexical-only fallback).
func ScanSearchRows(rows *sql.Rows) ([]SearchResult, error) {
	return scanResults(rows)
}

func scanResults(rows *sql.Rows) ([]SearchResult, error) {
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var propsRaw any
		if err := rows.Scan(&r.NodeID, &r.NodeType, &propsRaw, &r.SemanticText, &r.RRFScore); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		switch v := propsRaw.(type) {
		case map[string]any:
			r.Properties = v
		case string:
			if err := json.Unmarshal([]byte(v), &r.Properties); err != nil {
				r.Properties = map[string]any{"_raw": v}
			}
		default:
			raw, _ := json.Marshal(v)
			r.Properties = map[string]any{"_raw": string(raw)}
		}
		results = append(results, r)
	}
	return results, rows.Err()
}
