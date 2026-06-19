package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
)

func TestEnsureReadOnly(t *testing.T) {
	ok := []string{
		"SELECT * FROM Nodes_Base LIMIT 5",
		"  select node_id from Nodes_Base where is_current",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"FROM GRAPH_TABLE(domain_graph MATCH (a:\"node\")-[e:\"edge\"]->(b:\"node\") COLUMNS (a.node_id))",
		"SELECT 1;", // trailing semicolon allowed
		"PRAGMA table_info('Nodes_Base')",
		"-- a comment\nSELECT count(*) FROM Edges_Base",
	}
	for _, q := range ok {
		if err := ensureReadOnly(q); err != nil {
			t.Errorf("expected allowed, got error for %q: %v", q, err)
		}
	}

	bad := []string{
		"",
		"INSERT INTO Nodes_Base VALUES (1)",
		"DROP TABLE Nodes_Base",
		"update Nodes_Base set node_type = 'x'",
		"SELECT 1; DROP TABLE Nodes_Base", // statement stacking
		"ATTACH 'evil.db'",
		"COPY Nodes_Base TO 'out.csv'",
		"create table t as select 1",
		"SELECT 1 -- ; harmless\n; DELETE FROM Edges_Base", // stacked after comment strip
	}
	for _, q := range bad {
		if err := ensureReadOnly(q); err == nil {
			t.Errorf("expected rejection for %q, got nil", q)
		}
	}
}

func TestContainsWord(t *testing.T) {
	if !containsWord("select drop from x", "drop") {
		t.Error("should match whole word drop")
	}
	if containsWord("select dropped from x", "drop") {
		t.Error("should not match substring inside dropped")
	}
	if containsWord("a_drop_b", "drop") {
		t.Error("underscore-bounded should not match")
	}
}

func TestMdCellTruncation(t *testing.T) {
	long := strings.Repeat("x", maxCellLen+50)
	got := mdCell(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > maxCellLen+1 {
		t.Errorf("cell not truncated: len=%d", len([]rune(got)))
	}
	if mdCell("a|b\nc") != "a\\|b c" {
		t.Errorf("pipe/newline escaping wrong: %q", mdCell("a|b\nc"))
	}
	if mdCell(nil) != "" {
		t.Error("nil should render empty")
	}
}

func TestMarkdownTable(t *testing.T) {
	out := markdownTable([]string{"a", "b"}, [][]string{{"1", "2"}}, true)
	if !strings.Contains(out, "| a | b |") || !strings.Contains(out, "| 1 | 2 |") {
		t.Errorf("missing rows: %s", out)
	}
	if !strings.Contains(out, "capped") {
		t.Errorf("truncation note missing: %s", out)
	}
}

// TestToolsetWithRealDB exercises the DB-facing tools against the local ufo
// graph when present. It is skipped in environments without that database.
func TestToolsetWithRealDB(t *testing.T) {
	const dbPath = "../ufo.duckdb"
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("no %s; skipping DB-backed test", dbPath)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.LoadExtensions(); err != nil {
		t.Skipf("extensions unavailable (offline?): %v", err)
	}

	ts := &toolset{db: db, vectorOK: false}

	schema, err := ts.schemaText()
	if err != nil {
		t.Fatalf("schemaText: %v", err)
	}
	if !strings.Contains(schema, "Node Types") {
		t.Errorf("schema missing node types:\n%s", schema)
	}

	// sql_query via the tool function.
	tool := ts.sqlQueryTool()
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Input: `{"sql":"SELECT node_type, COUNT(*) c FROM Nodes_Base WHERE is_current GROUP BY 1 ORDER BY c DESC LIMIT 3"}`,
	})
	if err != nil {
		t.Fatalf("sql_query run: %v", err)
	}
	if txt := resp.Content; !strings.Contains(txt, "| node_type | c |") {
		t.Errorf("sql_query output unexpected:\n%s", txt)
	}

	// lexical search path.
	results, err := ts.lexicalSearch("triangle lights", nil, 3)
	if err != nil {
		t.Fatalf("lexicalSearch: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected lexical results")
	}
}
