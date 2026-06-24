package store

import (
	"fmt"
	"time"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
)

// UpsertEdges expires previous versions of matching edges and inserts new ones.
func (db *DB) UpsertEdges(edges []domain.Edge, ts time.Time) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	expireStmt, err := tx.Prepare(`
		UPDATE Edges_Base
		SET is_current = FALSE, valid_to = ?
		WHERE edge_id = ? AND is_current = TRUE
	`)
	if err != nil {
		return fmt.Errorf("preparing expire stmt: %w", err)
	}
	defer expireStmt.Close()

	insertStmt, err := tx.Prepare(`
		INSERT INTO Edges_Base (edge_id, source_id, target_id, relationship_type, weight, valid_from, is_current)
		VALUES (?, ?, ?, ?, ?, ?, TRUE)
	`)
	if err != nil {
		return fmt.Errorf("preparing insert stmt: %w", err)
	}
	defer insertStmt.Close()

	for _, e := range edges {
		if _, err := expireStmt.Exec(ts, e.EdgeID); err != nil {
			return fmt.Errorf("expiring edge %s: %w", e.EdgeID, err)
		}
		if _, err := insertStmt.Exec(e.EdgeID, e.SourceID, e.TargetID, e.RelationshipType, e.Weight, e.ValidFrom); err != nil {
			return fmt.Errorf("inserting edge %s: %w", e.EdgeID, err)
		}
	}

	return tx.Commit()
}
