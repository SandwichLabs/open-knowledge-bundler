package cmd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	_ "embed"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/domain"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

//go:embed generate_template.html
var generateTemplate []byte

var (
	generateOutDir string
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Build a self-contained static site bundle (index.html + graph.json + embeddings.bin)",
	Long: `Compiles the knowledge graph into a static bundle that can be hosted on
any object storage (S3, GCS, etc.) and serves the full hybrid search + graph
explorer entirely in the browser via transformers.js + in-worker BM25.

Output:
  <out>/index.html       embedded UI (template baked into the binary)
  <out>/graph.json       nodes, edges, and domain config
  <out>/embeddings.bin   flat Float32 array of node embeddings (row-major)
  <out>/manifest.json    {domain, model, dim, dtype, generated_at, counts}`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := viper.GetString("database_path")
		if dbPath == "" {
			dbPath = "domain.duckdb"
		}
		domainName := viper.GetString("domain_name")
		embDim := viper.GetInt("embedding_dim")
		if embDim == 0 {
			embDim = 768
		}
		embModel := viper.GetString("embedding_model")

		// Read the YAML directly so node/edge type keys keep their original case
		// (viper lowercases all keys, which would break node_type lookups in the UI).
		nodeDefs, edgeDefs, err := readDefinitions(viper.ConfigFileUsed())
		if err != nil {
			return fmt.Errorf("reading definitions: %w", err)
		}

		if err := os.MkdirAll(generateOutDir, 0o755); err != nil {
			return fmt.Errorf("creating output dir: %w", err)
		}

		db, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()
		if err := db.LoadExtensions(); err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}

		log.Printf("Domain:    %s", domainName)
		log.Printf("Database:  %s", dbPath)
		log.Printf("Dim:       %d", embDim)

		nodes, embeddings, err := readNodes(db, embDim)
		if err != nil {
			return fmt.Errorf("reading nodes: %w", err)
		}
		edges, err := readEdges(db)
		if err != nil {
			return fmt.Errorf("reading edges: %w", err)
		}
		log.Printf("Nodes:     %d", len(nodes))
		log.Printf("Edges:     %d", len(edges))
		log.Printf("Embeddings:%d floats (%.1f MB)", len(embeddings), float64(len(embeddings)*4)/1024.0/1024.0)

		// graph.json
		graph := map[string]any{
			"meta": map[string]any{
				"domain_name":   domainName,
				"embedding_dim": embDim,
				"node_count":    len(nodes),
				"edge_count":    len(edges),
			},
			"config": map[string]any{
				"node_definitions": deriveNodeConfig(nodeDefs),
				"edge_definitions": deriveEdgeConfig(edgeDefs),
			},
			"nodes": nodes,
			"edges": edges,
		}
		graphPath := filepath.Join(generateOutDir, "graph.json")
		if err := writeJSONFile(graphPath, graph); err != nil {
			return fmt.Errorf("writing graph.json: %w", err)
		}
		log.Printf("Wrote %s", graphPath)

		// embeddings.bin (little-endian Float32)
		embPath := filepath.Join(generateOutDir, "embeddings.bin")
		if err := writeFloat32Bin(embPath, embeddings); err != nil {
			return fmt.Errorf("writing embeddings.bin: %w", err)
		}
		log.Printf("Wrote %s", embPath)

		// manifest.json
		manifest := map[string]any{
			"domain_name":   domainName,
			"embedding_dim": embDim,
			"embedding_model": embModel,
			"dtype":         "float32",
			"node_count":    len(nodes),
			"edge_count":    len(edges),
			"format_version": 1,
		}
		manifestPath := filepath.Join(generateOutDir, "manifest.json")
		if err := writeJSONFile(manifestPath, manifest); err != nil {
			return fmt.Errorf("writing manifest.json: %w", err)
		}
		log.Printf("Wrote %s", manifestPath)

		// index.html
		htmlPath := filepath.Join(generateOutDir, "index.html")
		if err := os.WriteFile(htmlPath, generateTemplate, 0o644); err != nil {
			return fmt.Errorf("writing index.html: %w", err)
		}
		log.Printf("Wrote %s", htmlPath)

		log.Printf("\nDone. Serve %s/ with any static file host (S3, npx serve, etc.)", generateOutDir)
		return nil
	},
}

type nodeOut struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Properties   map[string]any `json:"properties"`
	SemanticText string         `json:"semantic_text"`
	Lat          *float64       `json:"lat,omitempty"`
	Lng          *float64       `json:"lng,omitempty"`
}

type edgeOut struct {
	ID     string  `json:"id"`
	Source string  `json:"source"`
	Target string  `json:"target"`
	Type   string  `json:"type"`
	Weight float64 `json:"weight"`
}

func readNodes(db *store.DB, dim int) ([]nodeOut, []float32, error) {
	rows, err := db.RawQuery(`
		SELECT node_id, node_type, properties::VARCHAR, COALESCE(semantic_text, ''),
		       embedding::VARCHAR, latitude, longitude
		FROM Nodes_Base
		WHERE is_current = TRUE
		ORDER BY node_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	nodes := make([]nodeOut, 0, 1024)
	embeddings := make([]float32, 0, 1024*dim)
	zero := make([]float32, dim)

	for rows.Next() {
		var nid, ntype, props, sem string
		var embStr *string
		var lat, lng *float64
		if err := rows.Scan(&nid, &ntype, &props, &sem, &embStr, &lat, &lng); err != nil {
			return nil, nil, err
		}
		var pmap map[string]any
		if props != "" {
			if err := json.Unmarshal([]byte(props), &pmap); err != nil {
				pmap = map[string]any{}
			}
		}
		n := nodeOut{ID: nid, Type: ntype, Properties: pmap, SemanticText: sem}
		if lat != nil && lng != nil {
			n.Lat = lat
			n.Lng = lng
		}
		nodes = append(nodes, n)

		if embStr != nil && *embStr != "" {
			vec := parseDuckDBFloatArray(*embStr, dim)
			embeddings = append(embeddings, vec...)
		} else {
			embeddings = append(embeddings, zero...)
		}
	}
	return nodes, embeddings, rows.Err()
}

func readEdges(db *store.DB) ([]edgeOut, error) {
	rows, err := db.RawQuery(`
		SELECT edge_id, source_id, target_id, relationship_type, COALESCE(weight, 1.0)
		FROM Edges_Base
		WHERE is_current = TRUE
		ORDER BY edge_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	edges := make([]edgeOut, 0, 1024)
	for rows.Next() {
		var e edgeOut
		if err := rows.Scan(&e.ID, &e.Source, &e.Target, &e.Type, &e.Weight); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// parseDuckDBFloatArray parses DuckDB's array-as-string format, e.g. "[0.1, 0.2, ...]".
// Returns exactly dim floats (zero-padded or truncated).
func parseDuckDBFloatArray(s string, dim int) []float32 {
	out := make([]float32, dim)
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return out
	}
	i := 0
	for _, part := range strings.Split(s, ",") {
		if i >= dim {
			break
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f, err := strconv.ParseFloat(part, 32)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			out[i] = 0
		} else {
			out[i] = float32(f)
		}
		i++
	}
	return out
}

// readDefinitions parses node_definitions and edge_definitions directly from the
// YAML file, preserving the original key case (viper would lowercase them).
func readDefinitions(path string) (map[string]domain.EntityDef, map[string]domain.EntityDef, error) {
	if path == "" {
		path = "domain.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		NodeDefinitions map[string]domain.EntityDef `yaml:"node_definitions"`
		EdgeDefinitions map[string]domain.EntityDef `yaml:"edge_definitions"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	return doc.NodeDefinitions, doc.EdgeDefinitions, nil
}

func deriveNodeConfig(defs map[string]domain.EntityDef) map[string]any {
	result := make(map[string]any, len(defs))
	for typeName, def := range defs {
		display := make([]string, 0, len(def.Mappings))
		for _, m := range def.Mappings {
			if !m.IsKey {
				display = append(display, m.TargetField)
			}
		}
		result[typeName] = map[string]any{
			"semantic_fields": def.SemanticFields,
			"display_fields":  display,
		}
	}
	return result
}

func deriveEdgeConfig(defs map[string]domain.EntityDef) map[string]any {
	result := make(map[string]any, len(defs))
	for typeName := range defs {
		result[typeName] = map[string]any{}
	}
	return result
}

func writeJSONFile(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func writeFloat32Bin(path string, vec []float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	buf := make([]byte, 4)
	for _, v := range vec {
		binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	generateCmd.Flags().StringVarP(&generateOutDir, "output", "o", "dist", "output directory for the static bundle")
	rootCmd.AddCommand(generateCmd)
}
