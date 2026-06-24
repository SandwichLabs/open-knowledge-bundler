package extract

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads a domain.yaml into a DomainConfig (or returns a zero config
// if the file is absent, so a fresh domain can be bootstrapped).
func LoadConfig(path string) (*domain.DomainConfig, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &domain.DomainConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg domain.DomainConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// SaveConfig writes a DomainConfig back to path as YAML.
func SaveConfig(path string, cfg *domain.DomainConfig) error {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

const typesSystem = `You are an ontology designer. From sample text, propose a COMPACT, CLOSED set of entity types that later extraction will be constrained to.

Hard rules:
- At most 20 types. Broad, reusable CATEGORIES, never specific instances (e.g. Disease, Treatment, Symptom, Drug, Anatomy, Test, Procedure, RiskFactor — not "lung cancer").
- UpperCamelCase, single word where possible.
- Always include an "Other" catch-all type.`

const relationsSystem = `You are an ontology designer. Given a fixed set of entity types and sample text, propose a COMPACT, CLOSED set of directional relations that later extraction will be constrained to.

Hard rules:
- At most 40 relations. Each is an UPPER_SNAKE verb phrase with ONE canonical direction; you MUST set source_type and target_type to types from the provided list (this is the edge direction: source_type -> target_type).
- Merge synonyms into a single relation — do NOT emit both TREATS and TREATED_BY, or both HAS_SYMPTOM and SYMPTOM_OF. Pick one direction; if the opposite phrasing is natural and DIFFERENT from the chosen name, put it in "inverse" so extraction can normalize it (leave "inverse" empty otherwise — never set it equal to the relation name).
- Avoid near-duplicate relations (DIAGNOSED_BY vs DETECTS vs TESTS_FOR — choose one). Prefer specific, queryable relations.`

var typesSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"entity_types": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"name", "description"},
			},
		},
	},
	"required": []string{"entity_types"},
}

// relationsSchema is structure-only (no enum). kronk v1.28.0's schema→GBNF
// generator emits a grammar llama.cpp rejects when an enum is present, after
// which kronk silently generates unconstrained — so source_type/target_type are
// kept as plain strings and validated against the entity types in code.
var relationsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"relations": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string"},
					"source_type": map[string]any{"type": "string"},
					"target_type": map[string]any{"type": "string"},
					"inverse":     map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"name", "source_type", "target_type", "inverse", "description"},
			},
		},
	},
	"required": []string{"relations"},
}

// Bootstrap proposes a compact ontology from a corpus sample in two grammar-
// constrained passes: first the entity types, then the directional relations
// with source_type/target_type enum-constrained to those types (a single pass
// cannot constrain the endpoints to types it is inventing in the same call).
func Bootstrap(ctx context.Context, g *Generator, chunks []Chunk, sampleN int) (*domain.Ontology, error) {
	sample := SampleChunks(chunks, sampleN)
	var b strings.Builder
	for _, c := range sample {
		b.WriteString(c.Text)
		b.WriteString("\n\n---\n\n")
	}
	sampleText := b.String()

	// Pass 1 — entity types.
	var tres struct {
		EntityTypes []domain.TypeDef `json:"entity_types"`
	}
	typesUser := "Propose the entity types for the domain in this sample text.\n\nSAMPLE TEXT:\n" + sampleText
	if _, err := g.GenerateJSON(ctx, typesSystem, typesUser, typesSchema, &tres); err != nil {
		return nil, fmt.Errorf("ontology bootstrap (types): %w", err)
	}

	ont := &domain.Ontology{}
	seenType := map[string]bool{}
	for _, t := range tres.EntityTypes {
		name := cleanTypeName(t.Name)
		if name == "" || seenType[name] {
			continue
		}
		seenType[name] = true
		ont.EntityTypes = append(ont.EntityTypes, domain.TypeDef{Name: name, Description: t.Description})
	}
	if !seenType["Other"] {
		ont.EntityTypes = append(ont.EntityTypes, domain.TypeDef{Name: "Other", Description: "catch-all"})
	}
	if len(ont.EntityTypes) < 2 {
		return nil, fmt.Errorf("bootstrap produced too few entity types (%d)", len(ont.EntityTypes))
	}

	// Pass 2 — relations, with endpoint types constrained to the entity types.
	var rres struct {
		Relations []domain.RelationDef `json:"relations"`
	}
	relUser := fmt.Sprintf("Entity types: %s.\n\nPropose the directional relations for the domain in this sample text.\n\nSAMPLE TEXT:\n%s",
		strings.Join(ont.EntityTypeNames(), ", "), sampleText)
	if _, err := g.GenerateJSON(ctx, relationsSystem, relUser, relationsSchema, &rres); err != nil {
		return nil, fmt.Errorf("ontology bootstrap (relations): %w", err)
	}

	// Normalized lookup so the model's endpoint-type strings (which aren't
	// enum-enforced) match the entity types despite case/spacing/plural drift.
	typeLookup := map[string]string{}
	for _, t := range ont.EntityTypes {
		typeLookup[normalizeName(t.Name)] = t.Name
	}
	matchType := func(s string) string {
		if t, ok := typeLookup[normalizeName(s)]; ok {
			return t
		}
		return "Other"
	}

	seenRel := map[string]bool{}
	coerced := 0
	for _, r := range rres.Relations {
		name := normalizeRelation(r.Name)
		if name == "" || seenRel[name] {
			continue
		}
		seenRel[name] = true
		inv := normalizeRelation(r.Inverse)
		if inv == name { // a self-inverse is meaningless
			inv = ""
		}
		src := matchType(r.SourceType)
		tgt := matchType(r.TargetType)
		if (src == "Other" && !strings.EqualFold(r.SourceType, "other")) ||
			(tgt == "Other" && !strings.EqualFold(r.TargetType, "other")) {
			coerced++
		}
		ont.Relations = append(ont.Relations, domain.RelationDef{
			Name:        name,
			SourceType:  src,
			TargetType:  tgt,
			Inverse:     inv,
			Description: r.Description,
		})
	}
	if len(ont.Relations) == 0 {
		return nil, fmt.Errorf("bootstrap produced no relations")
	}
	if coerced > 0 {
		fmt.Fprintf(os.Stderr, "  note: %d/%d relations had an unrecognized endpoint type coerced to Other\n", coerced, len(ont.Relations))
	}
	return ont, nil
}

// cleanTypeName normalizes an entity type name to UpperCamel-ish single token.
func cleanTypeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse spaces/underscores; capitalize each part.
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '_' || r == '-' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}
