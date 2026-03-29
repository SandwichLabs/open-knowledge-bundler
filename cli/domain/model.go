package domain

import "time"

// TemporalRecord handles SCD Type 2 chronological tracking.
type TemporalRecord struct {
	ValidFrom time.Time `json:"valid_from"`
	ValidTo   time.Time `json:"valid_to,omitempty"`
	IsCurrent bool      `json:"is_current"`
}

// Node represents a generic entity in the property graph.
type Node struct {
	NodeID       string         `json:"node_id"`
	NodeType     string         `json:"node_type"`
	Properties   map[string]any `json:"properties"`
	SemanticText string         `json:"semantic_text"`
	Embedding    []float32      `json:"embedding,omitempty"`
	Latitude     *float64       `json:"latitude,omitempty"`
	Longitude    *float64       `json:"longitude,omitempty"`
	TemporalRecord
}

// Edge represents a directed, temporally-tracked relationship.
type Edge struct {
	EdgeID           string  `json:"edge_id"`
	SourceID         string  `json:"source_id"`
	TargetID         string  `json:"target_id"`
	RelationshipType string  `json:"relationship_type"`
	Weight           float64 `json:"weight"`
	TemporalRecord
}
