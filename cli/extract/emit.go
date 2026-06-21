package extract

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/domain"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
)

// EmitOptions configures the domain.yaml the emitter regenerates.
type EmitOptions struct {
	DomainName     string
	EmbeddingDim   int
	EmbeddingModel string
	EndpointURL    string
	DatabasePath   string
}

// Emit writes the resolved graph in cbi's ingest format: nodes.ndjson,
// edges.ndjson, vocab.txt, and a regenerated domain.yaml (carrying the ontology
// plus node/edge definitions derived from the resolved types). Node properties
// carry aliases and provenance (source chunk ids) for traceability.
func Emit(dir string, ont *domain.Ontology, res *Resolved, opts EmitOptions) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := writeNDJSON(filepath.Join(dir, "nodes.ndjson"), len(res.Nodes), func(i int) any {
		return ToNode(res.Nodes[i])
	}); err != nil {
		return fmt.Errorf("writing nodes: %w", err)
	}
	if err := writeNDJSON(filepath.Join(dir, "edges.ndjson"), len(res.Relations), func(i int) any {
		return ToEdge(res.Relations[i])
	}); err != nil {
		return fmt.Errorf("writing edges: %w", err)
	}

	// vocab.txt: canonical names + aliases (used by bench prep_questions / eval).
	if err := writeVocab(filepath.Join(dir, "vocab.txt"), res.Nodes); err != nil {
		return fmt.Errorf("writing vocab: %w", err)
	}

	cfg := buildDomainConfig(ont, res, opts)
	if err := SaveConfig(filepath.Join(dir, "domain.yaml"), cfg); err != nil {
		return err
	}
	return nil
}

// ToNode converts a resolved node to the ingest domain.Node shape.
func ToNode(n ResolvedNode) domain.Node {
	props := map[string]any{"name": n.Name}
	if len(n.Aliases) > 0 {
		props["aliases"] = n.Aliases
	}
	if len(n.Chunks) > 0 {
		props["provenance"] = n.Chunks
	}
	semantic := n.Name
	if len(n.Aliases) > 0 {
		semantic = n.Name + " (" + strings.Join(n.Aliases, ", ") + ")"
	}
	return domain.Node{
		NodeID:       n.ID,
		NodeType:     n.Type,
		Properties:   props,
		SemanticText: semantic,
	}
}

// ToEdge converts a resolved relation to the ingest domain.Edge shape.
func ToEdge(r ResolvedRelation) domain.Edge {
	return domain.Edge{
		EdgeID:           strings.ToLower(r.Relation) + "|" + r.SourceID + "|" + r.TargetID,
		SourceID:         r.SourceID,
		TargetID:         r.TargetID,
		RelationshipType: r.Relation,
		Weight:           1.0,
	}
}

func writeNDJSON(path string, n int, get func(i int) any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := 0; i < n; i++ {
		if err := enc.Encode(get(i)); err != nil {
			return err
		}
	}
	return w.Flush()
}

func writeVocab(path string, nodes []ResolvedNode) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	seen := map[string]bool{}
	for _, n := range nodes {
		for _, name := range append([]string{n.Name}, n.Aliases...) {
			if name != "" && !seen[name] {
				seen[name] = true
				fmt.Fprintln(w, name)
			}
		}
	}
	return w.Flush()
}

// buildDomainConfig regenerates node/edge definitions from the resolved types
// and relation vocabulary, preserving the ontology block and embedding settings.
func buildDomainConfig(ont *domain.Ontology, res *Resolved, opts EmitOptions) *domain.DomainConfig {
	if opts.DomainName == "" {
		opts.DomainName = "extracted_kg"
	}
	if opts.EmbeddingDim == 0 {
		opts.EmbeddingDim = 768
	}
	if opts.DatabasePath == "" {
		opts.DatabasePath = opts.DomainName + ".duckdb"
	}

	typesSeen := map[string]bool{}
	for _, n := range res.Nodes {
		typesSeen[n.Type] = true
	}
	relsSeen := map[string]bool{}
	for _, r := range res.Relations {
		relsSeen[r.Relation] = true
	}

	nodeDefs := map[string]domain.EntityDef{}
	for t := range typesSeen {
		nodeDefs[t] = domain.EntityDef{
			SemanticFields: []string{"name"},
			Mappings: []domain.FieldMapping{
				{SourceField: "node_id", TargetField: "node_id", IsKey: true},
				{SourceField: "name", TargetField: "name"},
			},
		}
	}
	edgeDefs := map[string]domain.EntityDef{}
	for r := range relsSeen {
		edgeDefs[r] = domain.EntityDef{
			Mappings: []domain.FieldMapping{
				{SourceField: "source_id", TargetField: "source_id", IsKey: true},
				{SourceField: "target_id", TargetField: "target_id", IsKey: true},
			},
		}
	}

	return &domain.DomainConfig{
		DomainName:      opts.DomainName,
		EmbeddingDim:    opts.EmbeddingDim,
		EmbeddingModel:  opts.EmbeddingModel,
		EndpointURL:     opts.EndpointURL,
		DatabasePath:    opts.DatabasePath,
		Ontology:        ont,
		NodeDefinitions: nodeDefs,
		EdgeDefinitions: edgeDefs,
	}
}

// Ingest loads the resolved graph straight into DuckDB, fully in-process:
// embeddings come from the passed Embedder (no HTTP), and the schema/indexes/
// property-graph are created idempotently. ts stamps the temporal records.
func Ingest(ctx context.Context, dbPath string, res *Resolved, emb Embedder, embDim int, ts time.Time, progress ProgressFunc) error {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()
	if err := db.LoadExtensions(); err != nil {
		return fmt.Errorf("loading extensions: %w", err)
	}
	if err := db.CreateSchema(embDim); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	if err := db.CreateIndexes(embDim); err != nil {
		return fmt.Errorf("creating indexes: %w", err)
	}
	if err := db.CreatePropertyGraph(); err != nil {
		return fmt.Errorf("creating property graph: %w", err)
	}

	nodes := make([]domain.Node, len(res.Nodes))
	for i, rn := range res.Nodes {
		n := ToNode(rn)
		if emb != nil && n.SemanticText != "" {
			vec, err := emb.Embed(ctx, n.SemanticText)
			if err != nil {
				return fmt.Errorf("embedding %s: %w", n.NodeID, err)
			}
			n.Embedding = vec
		}
		n.ValidFrom = ts
		n.IsCurrent = true
		nodes[i] = n
		if (i+1)%200 == 0 {
			progress("  embedded %d/%d nodes", i+1, len(res.Nodes))
		}
	}
	if err := db.UpsertNodes(nodes, ts); err != nil {
		return fmt.Errorf("upserting nodes: %w", err)
	}

	edges := make([]domain.Edge, len(res.Relations))
	for i, rr := range res.Relations {
		e := ToEdge(rr)
		e.ValidFrom = ts
		e.IsCurrent = true
		edges[i] = e
	}
	if err := db.UpsertEdges(edges, ts); err != nil {
		return fmt.Errorf("upserting edges: %w", err)
	}
	if err := db.RebuildFTSIndex(); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}
	progress("  ingested %d nodes, %d edges into %s", len(nodes), len(edges), dbPath)
	return nil
}

// SortedTypeNames returns the entity types present in the resolved nodes (sorted).
func SortedTypeNames(res *Resolved) []string {
	set := map[string]bool{}
	for _, n := range res.Nodes {
		set[n.Type] = true
	}
	out := sortedKeys(set)
	sort.Strings(out)
	return out
}
