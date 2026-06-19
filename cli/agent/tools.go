package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
)

// toolset bundles the dependencies the agent's tools operate over.
type toolset struct {
	db       *store.DB
	bundle   *Bundle
	embedder *Embedder // may be nil when embeddings are unavailable
	vectorOK bool      // whether the semantic channel is usable
}

const (
	maxSQLRows  = 50    // row cap for sql_query output
	maxCellLen  = 200   // per-cell character cap
	maxDocList  = 200   // entries returned by list_docs
	maxDocHits  = 40    // results returned by search_docs
	maxDocBytes = 60000 // read_doc size cap
)

// Tools returns the fantasy tools the agent can call against the bundle.
func (t *toolset) Tools() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		t.sqlQueryTool(),
		t.hybridSearchTool(),
		t.schemaTool(),
		t.readDocTool(),
		t.listDocsTool(),
		t.searchDocsTool(),
	}
}

// ---------------------------------------------------------------------------
// sql_query

type sqlQueryInput struct {
	SQL string `json:"sql" description:"A single read-only DuckDB SQL or SQL/PGQ query. Tables: Nodes_Base, Edges_Base (filter is_current = TRUE for the current state). Graph: FROM GRAPH_TABLE(domain_graph MATCH ... ) labelling nodes \"node\" and edges \"edge\"."`
}

func (t *toolset) sqlQueryTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"sql_query",
		"Run a read-only SQL or SQL/PGQ query against the bundle's DuckDB graph and get the result rows as a markdown table. Use this for precise facts, counts, filters, and graph traversals. Call schema first if unsure of the structure.",
		func(ctx context.Context, in sqlQueryInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if err := ensureReadOnly(in.SQL); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			table, err := t.runQueryTable(in.SQL)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("query failed: %v", err)), nil
			}
			return fantasy.NewTextResponse(table), nil
		},
	)
}

// ---------------------------------------------------------------------------
// hybrid_search

type hybridSearchInput struct {
	Text  string `json:"text" description:"Natural-language search query."`
	Limit int    `json:"limit,omitempty" description:"Optional maximum number of results (default 10)."`
	Date  string `json:"date,omitempty" description:"Optional temporal filter as YYYY-MM-DD; returns the graph state as of that date."`
}

func (t *toolset) hybridSearchTool() fantasy.AgentTool {
	desc := "Find nodes by meaning using hybrid vector + lexical search (RRF fusion). Use this for fuzzy, conceptual, or exploratory lookups when you don't have an exact id or field value."
	if !t.vectorOK {
		desc += " NOTE: vector embeddings are unavailable in this session, so this runs lexical (keyword/BM25) search only."
	}
	return fantasy.NewAgentTool(
		"hybrid_search",
		desc,
		func(ctx context.Context, in hybridSearchInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			limit := in.Limit
			if limit <= 0 {
				limit = 10
			}

			var dateFilter *time.Time
			if in.Date != "" {
				ts, err := time.Parse("2006-01-02", in.Date)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid date %q (want YYYY-MM-DD)", in.Date)), nil
				}
				dateFilter = &ts
			}

			var results []store.SearchResult
			var err error
			if t.vectorOK && t.embedder != nil {
				var vec []float32
				vec, err = t.embedder.Embed(ctx, in.Text)
				if err == nil {
					results, err = t.db.HybridSearch(in.Text, vec, dateFilter, limit)
				}
			} else {
				results, err = t.lexicalSearch(in.Text, dateFilter, limit)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("search failed: %v", err)), nil
			}
			return fantasy.NewTextResponse(formatSearchResults(results)), nil
		},
	)
}

// ---------------------------------------------------------------------------
// schema

func (t *toolset) schemaTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"schema",
		"Describe the knowledge graph: node types and counts, relationship types, and how node types connect. Call this before writing sql_query if you are unsure of the available types or structure.",
		func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			out, err := t.schemaText()
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("schema failed: %v", err)), nil
			}
			return fantasy.NewTextResponse(out), nil
		},
	)
}

// ---------------------------------------------------------------------------
// read_doc / list_docs / search_docs

type readDocInput struct {
	Path string `json:"path" description:"Bundle-relative path to a markdown concept file, e.g. catalog/index.md or Pokemon/pikachu.md (as returned by list_docs/search_docs)."`
}

func (t *toolset) readDocTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"read_doc",
		"Read one markdown concept document from the bundle (catalog or per-node). Use after list_docs/search_docs to read prose context, fields, and cross-links.",
		func(ctx context.Context, in readDocInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			content, err := t.bundle.ReadDoc(in.Path)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(content) > maxDocBytes {
				content = content[:maxDocBytes] + "\n\n…(truncated)"
			}
			return fantasy.NewTextResponse(content), nil
		},
	)
}

type listDocsInput struct {
	Prefix string `json:"prefix,omitempty" description:"Optional path prefix filter, e.g. \"catalog/\" or \"Pokemon/\". Empty lists everything."`
}

func (t *toolset) listDocsTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"list_docs",
		"List the markdown concept documents available in the bundle, optionally filtered by a path prefix. Use this to discover what can be read with read_doc.",
		func(ctx context.Context, in listDocsInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var matches []string
			for _, d := range t.bundle.Docs {
				if in.Prefix == "" || strings.HasPrefix(d, in.Prefix) {
					matches = append(matches, d)
				}
			}
			total := len(matches)
			truncated := false
			if total > maxDocList {
				matches = matches[:maxDocList]
				truncated = true
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d document(s)", total)
			if truncated {
				fmt.Fprintf(&b, " (showing first %d)", maxDocList)
			}
			b.WriteString(":\n")
			for _, m := range matches {
				b.WriteString("- ")
				b.WriteString(m)
				b.WriteByte('\n')
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}

type searchDocsInput struct {
	Query string `json:"query" description:"Case-insensitive substring to find in document contents."`
}

func (t *toolset) searchDocsTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"search_docs",
		"Full-text (substring) search across the bundle's markdown documents. Returns matching document paths with a snippet. Use this to locate concepts by keyword before reading them.",
		func(ctx context.Context, in searchDocsInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			q := strings.TrimSpace(in.Query)
			if q == "" {
				return fantasy.NewTextErrorResponse("query is empty"), nil
			}
			needle := strings.ToLower(q)

			var b strings.Builder
			hits := 0
			for _, d := range t.bundle.Docs {
				content, err := t.bundle.ReadDoc(d)
				if err != nil {
					continue
				}
				lower := strings.ToLower(content)
				idx := strings.Index(lower, needle)
				if idx < 0 {
					continue
				}
				hits++
				fmt.Fprintf(&b, "- %s — %s\n", d, snippet(content, idx, len(q)))
				if hits >= maxDocHits {
					b.WriteString("…(more matches omitted)\n")
					break
				}
			}
			if hits == 0 {
				return fantasy.NewTextResponse(fmt.Sprintf("No documents match %q.", q)), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("%d match(es):\n%s", hits, b.String())), nil
		},
	)
}

// ---------------------------------------------------------------------------
// helpers

// ensureReadOnly rejects anything that is not a single read-only statement.
func ensureReadOnly(sql string) error {
	trimmed := strings.TrimSpace(stripSQLComments(sql))
	if trimmed == "" {
		return fmt.Errorf("empty query")
	}
	// Disallow statement stacking (a trailing semicolon is fine).
	if i := strings.Index(strings.TrimRight(trimmed, "; \n\t"), ";"); i >= 0 {
		return fmt.Errorf("only a single statement is allowed")
	}

	lower := strings.ToLower(trimmed)
	allowedPrefixes := []string{"select", "with", "from", "describe", "show", "summarize", "explain", "pragma", "table", "values"}
	ok := false
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(lower, p+" ") || lower == p {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("only read-only queries are allowed (must start with SELECT, WITH, FROM, DESCRIBE, SHOW, SUMMARIZE, EXPLAIN, PRAGMA, TABLE, or VALUES)")
	}

	// Defense in depth: reject mutating/side-effecting keywords as whole words.
	forbidden := []string{"insert", "update", "delete", "drop", "alter", "create", "attach", "detach", "copy", "install", "load", "export", "vacuum", "call", "set"}
	for _, w := range forbidden {
		if containsWord(lower, w) {
			return fmt.Errorf("disallowed keyword %q in query", w)
		}
	}
	return nil
}

func containsWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); {
		idx := strings.Index(s[i:], word)
		if idx < 0 {
			return false
		}
		abs := i + idx
		before := abs == 0 || !isWordChar(s[abs-1])
		afterIdx := abs + len(word)
		after := afterIdx >= len(s) || !isWordChar(s[afterIdx])
		if before && after {
			return true
		}
		i = abs + len(word)
	}
	return false
}

func isWordChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// stripSQLComments removes -- line comments so keyword checks aren't fooled.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// runQueryTable executes a query and renders the rows as a markdown table.
func (t *toolset) runQueryTable(query string) (string, error) {
	rows, err := t.db.RawQuery(query)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}

	var data [][]string
	truncated := false
	for rows.Next() {
		if len(data) >= maxSQLRows {
			truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		row := make([]string, len(cols))
		for i := range vals {
			row[i] = mdCell(vals[i])
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if len(data) == 0 {
		return "(0 rows)", nil
	}
	return markdownTable(cols, data, truncated), nil
}

func markdownTable(cols []string, data [][]string, truncated bool) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(cols)) + "\n")
	for _, row := range data {
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	fmt.Fprintf(&b, "\n(%d row(s)", len(data))
	if truncated {
		fmt.Fprintf(&b, "; capped at %d — add LIMIT/filters for more", maxSQLRows)
	}
	b.WriteString(")")
	return b.String()
}

func mdCell(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	if b, ok := v.([]byte); ok {
		s = string(b)
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	if len(s) > maxCellLen {
		s = s[:maxCellLen] + "…"
	}
	return s
}

// lexicalSearch runs BM25-only search, used when embeddings are unavailable.
func (t *toolset) lexicalSearch(text string, dateFilter *time.Time, limit int) ([]store.SearchResult, error) {
	temporal := "is_current = TRUE"
	if dateFilter != nil {
		ts := dateFilter.Format("2006-01-02 15:04:05")
		temporal = fmt.Sprintf("valid_from <= '%s' AND (valid_to IS NULL OR valid_to > '%s')", ts, ts)
	}
	query := fmt.Sprintf(`
		SELECT node_id, node_type, properties, semantic_text, score AS rrf_score
		FROM (
			SELECT *, fts_main_Nodes_Base.match_bm25(node_id, ?) AS score
			FROM Nodes_Base
			WHERE %s
		)
		WHERE score IS NOT NULL
		ORDER BY score DESC
		LIMIT %d
	`, temporal, limit)

	rows, err := t.db.RawQueryArgs(query, text)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return store.ScanSearchRows(rows)
}

func formatSearchResults(results []store.SearchResult) string {
	if len(results) == 0 {
		return "No results."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s):\n\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "%d. [%s] %s (score %.4f)\n", i+1, r.NodeType, r.NodeID, r.RRFScore)
		if r.SemanticText != "" {
			fmt.Fprintf(&b, "   %s\n", oneLine(r.SemanticText, 240))
		}
	}
	return b.String()
}

// schemaText returns a compact node/edge type readout for the agent.
func (t *toolset) schemaText() (string, error) {
	var b strings.Builder
	b.WriteString("# Knowledge Graph Schema\n\n")

	nodeTypes, err := t.runQueryTable(`
		SELECT node_type, COUNT(*) AS count
		FROM Nodes_Base WHERE is_current = TRUE
		GROUP BY node_type ORDER BY count DESC`)
	if err != nil {
		return "", err
	}
	b.WriteString("## Node Types\n\n")
	b.WriteString(nodeTypes)

	// Property keys per node type — the model otherwise guesses field names.
	propKeys, err := t.runQueryTable(`
		SELECT node_type, json_keys(ANY_VALUE(properties)) AS property_keys
		FROM Nodes_Base WHERE is_current = TRUE
		GROUP BY node_type ORDER BY node_type`)
	if err == nil {
		b.WriteString("\n\n## Node Property Keys (use these exact keys in properties->>'...')\n\n")
		b.WriteString(propKeys)
	}

	edgeTypes, err := t.runQueryTable(`
		SELECT relationship_type, COUNT(*) AS count
		FROM Edges_Base WHERE is_current = TRUE
		GROUP BY relationship_type ORDER BY count DESC`)
	if err == nil {
		b.WriteString("\n\n## Relationship Types\n\n")
		b.WriteString(edgeTypes)
	}

	conn, err := t.runQueryTable(`
		SELECT e.relationship_type, src.node_type AS source_type, tgt.node_type AS target_type, COUNT(*) AS count
		FROM Edges_Base e
		JOIN Nodes_Base src ON e.source_id = src.node_id AND src.is_current
		JOIN Nodes_Base tgt ON e.target_id = tgt.node_id AND tgt.is_current
		WHERE e.is_current = TRUE
		GROUP BY e.relationship_type, src.node_type, tgt.node_type
		ORDER BY count DESC`)
	if err == nil {
		b.WriteString("\n\n## Connectivity (source_type → target_type)\n\n")
		b.WriteString(conn)
	}

	b.WriteString("\n\n## Querying notes\n")
	b.WriteString("- Filter `is_current = TRUE` for the current state.\n")
	b.WriteString("- Extract JSON with the exact property keys above. ALWAYS wrap the extraction in parentheses when comparing: `(properties->>'key') = 'value'`. Without parentheses `->>` binds to the comparison and raises a cast error.\n")
	b.WriteString("- Edges are directed: `Edges_Base.source_id` → `Edges_Base.target_id`, with the direction shown in Connectivity above (source_type → target_type). Match that direction.\n")
	b.WriteString("- PREFER plain SQL joins on `Edges_Base`/`Nodes_Base` for relationships and traversals — it is the most reliable. Example, neighbours of a node by relationship:\n")
	b.WriteString("  ```sql\n")
	b.WriteString("  SELECT tgt.node_id, (tgt.properties->>'name') AS name\n")
	b.WriteString("  FROM Edges_Base e\n")
	b.WriteString("  JOIN Nodes_Base src ON e.source_id = src.node_id\n")
	b.WriteString("  JOIN Nodes_Base tgt ON e.target_id = tgt.node_id\n")
	b.WriteString("  WHERE e.is_current AND e.relationship_type = 'REL'\n")
	b.WriteString("    AND (src.properties->>'KEY') = 'VALUE';\n")
	b.WriteString("  ```\n")
	b.WriteString("- Multi-hop: self-join `Edges_Base` again on the previous `target_id`. duckpgq `GRAPH_TABLE` does NOT support recursive CTEs or inline `{property: value}` match filters — do not use those; put all filters in a WHERE clause.\n")
	return b.String(), nil
}

func snippet(content string, idx, qlen int) string {
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := idx + qlen + 40
	if end > len(content) {
		end = len(content)
	}
	return oneLine(content[start:end], 120)
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
