package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
)

// UpsertNodes expires previous versions of matching nodes and inserts new ones.
// If a node has latitude and longitude, a GEOMETRY point is computed via ST_Point.
func (db *DB) UpsertNodes(nodes []domain.Node, ts time.Time) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Expire previous current versions.
	expireStmt, err := tx.Prepare(`
		UPDATE Nodes_Base
		SET is_current = FALSE, valid_to = ?
		WHERE node_id = ? AND is_current = TRUE
	`)
	if err != nil {
		return fmt.Errorf("preparing expire stmt: %w", err)
	}
	defer expireStmt.Close()

	// Insert with geometry computed from lat/lng when available.
	insertStmt, err := tx.Prepare(`
		INSERT INTO Nodes_Base (node_id, node_type, properties, semantic_text, embedding, latitude, longitude, geom, valid_from, is_current)
		VALUES (?, ?, ?, ?, ?, ?, ?, ST_Point(?, ?), ?, TRUE)
	`)
	if err != nil {
		return fmt.Errorf("preparing insert stmt: %w", err)
	}
	defer insertStmt.Close()

	// Fallback for nodes without coordinates.
	insertNoGeomStmt, err := tx.Prepare(`
		INSERT INTO Nodes_Base (node_id, node_type, properties, semantic_text, embedding, valid_from, is_current)
		VALUES (?, ?, ?, ?, ?, ?, TRUE)
	`)
	if err != nil {
		return fmt.Errorf("preparing insert-no-geom stmt: %w", err)
	}
	defer insertNoGeomStmt.Close()

	for _, n := range nodes {
		if _, err := expireStmt.Exec(ts, n.NodeID); err != nil {
			return fmt.Errorf("expiring node %s: %w", n.NodeID, err)
		}

		props, err := json.Marshal(n.Properties)
		if err != nil {
			return fmt.Errorf("marshaling properties for %s: %w", n.NodeID, err)
		}

		if n.Latitude != nil && n.Longitude != nil {
			if _, err := insertStmt.Exec(
				n.NodeID, n.NodeType, string(props), n.SemanticText, n.Embedding,
				*n.Latitude, *n.Longitude, *n.Longitude, *n.Latitude,
				n.ValidFrom,
			); err != nil {
				return fmt.Errorf("inserting node %s: %w", n.NodeID, err)
			}
		} else {
			if _, err := insertNoGeomStmt.Exec(
				n.NodeID, n.NodeType, string(props), n.SemanticText, n.Embedding,
				n.ValidFrom,
			); err != nil {
				return fmt.Errorf("inserting node %s: %w", n.NodeID, err)
			}
		}
	}

	return tx.Commit()
}
