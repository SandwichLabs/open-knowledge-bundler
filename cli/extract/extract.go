package extract

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
)

// extractResult is the per-chunk model output (grammar-constrained).
type extractResult struct {
	Entities []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Aliases []string `json:"aliases"`
	} `json:"entities"`
	Relations []struct {
		Source   string `json:"source"`
		Relation string `json:"relation"`
		Target   string `json:"target"`
	} `json:"relations"`
}

// buildExtractionSchema returns the JSON schema that structurally constrains
// per-chunk extraction.
//
// Note on enums: ideally `type`/`relation` would be enum-bound to the ontology
// so off-vocabulary values were impossible at the token level. But kronk
// v1.28.0's JSON-schema→GBNF generator emits a grammar that llama.cpp REJECTS
// when an enum is present (SamplerInitGrammar returns 0), and kronk then
// silently falls back to *unconstrained* sampling — which produced malformed
// JSON in testing. A structure-only schema compiles to a grammar llama.cpp
// accepts, so it is reliably enforced. The ontology is therefore enforced in
// code instead: unknown entity types are coerced to "Other" (see Extract), and
// off-vocabulary relations are mapped/bucketed by the normalization stage.
func buildExtractionSchema(ont *domain.Ontology) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entities": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    map[string]any{"type": "string"},
						"type":    map[string]any{"type": "string"},
						"aliases": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"name", "type", "aliases"},
				},
			},
			"relations": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source":   map[string]any{"type": "string"},
						"relation": map[string]any{"type": "string"},
						"target":   map[string]any{"type": "string"},
					},
					"required": []string{"source", "relation", "target"},
				},
			},
		},
		"required": []string{"entities", "relations"},
	}
}

// extractionSystem builds the system prompt describing the closed ontology so
// the model knows what each type/relation means and which direction edges run.
func extractionSystem(ont *domain.Ontology) string {
	var b strings.Builder
	b.WriteString("You extract a knowledge graph from text. Return ONLY entities and relations that are explicitly supported by the TEXT — never add facts from your own knowledge.\n\n")
	b.WriteString("Use canonical, low-noise entity names (singular, no qualifiers). Put alternate surface forms / abbreviations seen in the text into `aliases`. Every relation's source and target MUST be names that also appear in `entities`.\n\n")
	b.WriteString("ENTITY TYPES (choose exactly one per entity):\n")
	for _, t := range ont.EntityTypes {
		if t.Description != "" {
			fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		} else {
			fmt.Fprintf(&b, "- %s\n", t.Name)
		}
	}
	b.WriteString("\nRELATIONS (use the exact name; edges run source -> target in the direction shown):\n")
	for _, r := range ont.Relations {
		fmt.Fprintf(&b, "- %s (%s -> %s)", r.Name, r.SourceType, r.TargetType)
		if r.Description != "" {
			fmt.Fprintf(&b, ": %s", r.Description)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ProgressFunc reports stage progress (printf-style).
type ProgressFunc func(format string, a ...any)

// Extract runs stage 1 (per-chunk extraction) and stage 2 (gleaning) over all
// chunks, accumulating into a Graph. glean is the number of extra recall passes
// per chunk (0 = off); a pass that adds nothing ends gleaning early for that
// chunk.
func Extract(ctx context.Context, g *Generator, ont *domain.Ontology, chunks []Chunk, glean int, progress ProgressFunc) (*Graph, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	schema := buildExtractionSchema(ont)
	system := extractionSystem(ont)
	validType := map[string]bool{}
	for _, t := range ont.EntityTypes {
		validType[t.Name] = true
	}
	graph := NewGraph()

	for _, ch := range chunks {
		before := len(graph.Entities)
		if err := extractChunk(ctx, g, system, schema, validType, graph, ch, ""); err != nil {
			progress("  ! chunk %d extract error: %v", ch.ID, err)
			continue // a single bad chunk (timeout / unrecoverable JSON) shouldn't kill the run
		}

		// Stage 2 — gleaning: ask for what was missed, stop when a round is dry.
		for round := 0; round < glean; round++ {
			seen := graph.entitiesInChunk(ch.ID)
			addBefore := len(graph.Entities) + len(graph.Relations)
			hint := "Entities already extracted from this text: " + strings.Join(seen, ", ") +
				".\nExtract ONLY entities and relations that were MISSED above and are still supported by the TEXT. If nothing was missed, return empty arrays."
			if err := extractChunk(ctx, g, system, schema, validType, graph, ch, hint); err != nil {
				progress("  ! chunk %d glean error: %v", ch.ID, err)
				break
			}
			if len(graph.Entities)+len(graph.Relations) == addBefore {
				break // dry round
			}
		}

		if (ch.ID+1)%5 == 0 || ch.ID+1 == len(chunks) {
			progress("  chunk %d/%d | %d entities %d relations (+%d this chunk)",
				ch.ID+1, len(chunks), len(graph.Entities), len(graph.Relations), len(graph.Entities)-before)
		}
	}
	return graph, nil
}

// extractChunk runs one extraction (or glean) pass and folds the result into g.
// Unknown entity types (the schema no longer enum-constrains them) are coerced
// to "Other" so the ontology stays closed.
func extractChunk(ctx context.Context, gen *Generator, system string, schema map[string]any, validType map[string]bool, g *Graph, ch Chunk, hint string) error {
	user := "TEXT:\n" + ch.Text
	if hint != "" {
		user = hint + "\n\n" + user
	}
	var res extractResult
	if _, err := gen.GenerateJSON(ctx, system, user, schema, &res); err != nil {
		return err
	}
	for _, e := range res.Entities {
		typ := e.Type
		if !validType[typ] {
			typ = "Other"
		}
		ent := g.AddEntity(e.Name, typ, ch.ID)
		if ent != nil {
			for _, a := range e.Aliases {
				if a = strings.TrimSpace(a); a != "" && !strings.EqualFold(a, ent.Name) {
					ent.Aliases[strings.ToLower(a)] = true
				}
			}
		}
	}
	for _, r := range res.Relations {
		g.AddRelation(r.Source, r.Relation, r.Target, ch.ID)
	}
	return nil
}

// entitiesInChunk lists the display names of entities seen in a chunk (for the
// gleaning hint), sorted for determinism.
func (g *Graph) entitiesInChunk(chunkID int) []string {
	var names []string
	for _, e := range g.Entities {
		if e.Chunks[chunkID] {
			names = append(names, e.Name)
		}
	}
	sort.Strings(names)
	return names
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
