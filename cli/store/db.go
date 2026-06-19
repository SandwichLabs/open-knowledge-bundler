package store

import (
	"database/sql"
	"fmt"

	_ "github.com/duckdb/duckdb-go/v2"
)

// DB wraps a DuckDB connection.
type DB struct {
	conn *sql.DB
}

// Open creates or opens a DuckDB database at the given path.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("opening duckdb at %s: %w", path, err)
	}
	return &DB{conn: conn}, nil
}

// Close closes the underlying connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// LoadExtensions installs and loads vss, fts, spatial, and duckpgq extensions.
func (db *DB) LoadExtensions() error {
	// Core extensions from the default repository.
	for _, ext := range []string{"vss", "fts", "spatial"} {
		if _, err := db.conn.Exec(fmt.Sprintf("INSTALL %s; LOAD %s;", ext, ext)); err != nil {
			return fmt.Errorf("loading extension %s: %w", ext, err)
		}
	}
	// duckpgq comes from the community repository.
	if _, err := db.conn.Exec("INSTALL duckpgq FROM community; LOAD duckpgq;"); err != nil {
		return fmt.Errorf("loading extension duckpgq: %w", err)
	}
	return nil
}

// CreateSchema creates the Nodes_Base and Edges_Base tables with spatial support.
func (db *DB) CreateSchema(embeddingDim int) error {
	nodesSQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS Nodes_Base (
			node_id       VARCHAR PRIMARY KEY,
			node_type     VARCHAR NOT NULL,
			properties    JSON,
			semantic_text VARCHAR,
			embedding     FLOAT[%d],
			latitude      DOUBLE,
			longitude     DOUBLE,
			geom          GEOMETRY,
			valid_from    TIMESTAMP,
			valid_to      TIMESTAMP,
			is_current    BOOLEAN DEFAULT TRUE
		);
	`, embeddingDim)

	edgesSQL := `
		CREATE TABLE IF NOT EXISTS Edges_Base (
			edge_id           VARCHAR PRIMARY KEY,
			source_id         VARCHAR NOT NULL REFERENCES Nodes_Base(node_id),
			target_id         VARCHAR NOT NULL REFERENCES Nodes_Base(node_id),
			relationship_type VARCHAR NOT NULL,
			weight            FLOAT DEFAULT 1.0,
			valid_from        TIMESTAMP,
			valid_to          TIMESTAMP,
			is_current        BOOLEAN DEFAULT TRUE
		);
	`

	if _, err := db.conn.Exec(nodesSQL); err != nil {
		return fmt.Errorf("creating Nodes_Base: %w", err)
	}
	if _, err := db.conn.Exec(edgesSQL); err != nil {
		return fmt.Errorf("creating Edges_Base: %w", err)
	}
	return nil
}

// RawQuery executes an arbitrary SQL query and returns the result rows.
func (db *DB) RawQuery(query string) (*sql.Rows, error) {
	return db.conn.Query(query)
}

// RawQueryArgs executes a parameterized SQL query and returns the result rows.
func (db *DB) RawQueryArgs(query string, args ...any) (*sql.Rows, error) {
	return db.conn.Query(query, args...)
}

// CreateIndexes creates the HNSW vector similarity index and spatial index.
func (db *DB) CreateIndexes(embeddingDim int) error {
	// Enable persistent HNSW indexes for on-disk databases.
	if _, err := db.conn.Exec("SET hnsw_enable_experimental_persistence = true;"); err != nil {
		return fmt.Errorf("enabling HNSW persistence: %w", err)
	}

	// HNSW index for vector similarity search.
	hnswSQL := fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_nodes_embedding
		ON Nodes_Base
		USING HNSW (embedding)
		WITH (metric = 'cosine');
	`)
	if _, err := db.conn.Exec(hnswSQL); err != nil {
		return fmt.Errorf("creating HNSW index: %w", err)
	}
	return nil
}

// CreatePropertyGraph creates the SQL/PGQ property graph over the base tables.
func (db *DB) CreatePropertyGraph() error {
	_, err := db.conn.Exec(`
		CREATE OR REPLACE PROPERTY GRAPH domain_graph
		VERTEX TABLES (
			Nodes_Base LABEL "node"
		)
		EDGE TABLES (
			Edges_Base
				SOURCE KEY (source_id) REFERENCES Nodes_Base (node_id)
				DESTINATION KEY (target_id) REFERENCES Nodes_Base (node_id)
				LABEL "edge"
		);
	`)
	if err != nil {
		return fmt.Errorf("creating property graph: %w", err)
	}
	return nil
}
