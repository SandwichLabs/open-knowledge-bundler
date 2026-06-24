package extract

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
)

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Hodgkin Lymphoma": "hodgkin_lymphoma",
		"hodgkin lymphoma": "hodgkin_lymphoma",
		"Adrenal Tumors":   "adrenal_tumor", // consonant+s singularized
		"adrenal tumor":    "adrenal_tumor", // collides
		"Pheochromocytoma": "pheochromocytoma",
		"  spaced  out  ":  "spaced_out",
		"diabetes":         "diabetes", // vowel+s kept
		"viruses":          "viruses",  // vowel+s kept (no false merge with "virus")
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRelation(t *testing.T) {
	cases := map[string]string{
		"treated by":  "TREATED_BY",
		"TREATED_BY":  "TREATED_BY",
		"has-symptom": "HAS_SYMPTOM",
		"  is_a  ":    "IS_A",
	}
	for in, want := range cases {
		if got := normalizeRelation(in); got != want {
			t.Errorf("normalizeRelation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanAndRepairJSON(t *testing.T) {
	// Fenced + prose around the object.
	if got := cleanJSON("```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("cleanJSON fenced = %q", got)
	}
	// Truncated/under-closed object recovers via repair.
	var v map[string]any
	repaired := repairJSON(`{"entities": [{"name": "x", "type": "Disease", "aliases": ["a"]}`)
	if err := json.Unmarshal([]byte(repaired), &v); err != nil {
		t.Errorf("repairJSON did not produce valid JSON: %q (%v)", repaired, err)
	}
	// Dangling trailing comma is dropped.
	repaired2 := repairJSON(`{"a": 1,`)
	if err := json.Unmarshal([]byte(repaired2), &v); err != nil {
		t.Errorf("repairJSON trailing comma: %q (%v)", repaired2, err)
	}
}

func TestChunkTextOverlap(t *testing.T) {
	text := "Sentence one. Sentence two. Sentence three. Sentence four. Sentence five. Sentence six."
	chunks := ChunkText(text, 40, 10)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.ID != i {
			t.Errorf("chunk %d has ID %d", i, c.ID)
		}
		if c.Text == "" {
			t.Errorf("chunk %d empty", i)
		}
	}
}

// TestResolveExactMerge verifies that with no embedder, entities collapse only
// by normalized name (the exact-merge path) and relations remap to node ids.
func TestResolveExactMerge(t *testing.T) {
	g := NewGraph()
	// "Tumor" and "Tumors" must collapse by normalized name (consonant+s), with
	// the plural surface form kept as an alias.
	g.AddEntity("Tumor", "Disease", 0)
	g.AddEntity("Tumors", "Disease", 1)
	g.AddEntity("Fever", "Symptom", 0)
	g.AddRelation("Tumors", "HAS_SYMPTOM", "Fever", 0)

	res, err := Resolve(context.Background(), nil, nil, g, 0.86, 0.80, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("expected 2 canonical nodes (disease+symptom), got %d: %+v", len(res.Nodes), res.Nodes)
	}
	if len(res.Relations) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(res.Relations))
	}
	// The relation must remap onto the canonical disease node id.
	var disease *ResolvedNode
	for i := range res.Nodes {
		if res.Nodes[i].Type == "Disease" {
			disease = &res.Nodes[i]
		}
	}
	if disease == nil {
		t.Fatal("no disease node")
	}
	if res.Relations[0].SourceID != disease.ID {
		t.Errorf("relation source %q did not remap to canonical disease id %q", res.Relations[0].SourceID, disease.ID)
	}
	// The plural duplicate should be recorded as an alias.
	if len(disease.Aliases) == 0 {
		t.Errorf("expected the merged plural to be recorded as an alias, got none")
	}
}

// fakeEmbedder returns a fixed vector per entity name (for resolution tests).
type fakeEmbedder struct{ vecs map[string][]float32 }

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

// TestResolveLeaderClustering verifies representative-based clustering merges an
// entity that is similar to a cluster's representative, but does NOT merge one
// that is dissimilar to the representative — the anti-chaining fix. The
// higher-frequency entity seeds the cluster and becomes the canonical name. With
// no gray-band pairs here, the generator is never called (nil is safe).
func TestResolveLeaderClustering(t *testing.T) {
	g := NewGraph()
	// "cancer" is the frequent generic (seed); "ca" is a near-identical synonym
	// (auto-merges to the rep); "metastasis" is unrelated (must stay separate).
	for c := 0; c < 3; c++ {
		g.AddEntity("cancer", "Disease", c)
	}
	g.AddEntity("ca", "Disease", 0)
	g.AddEntity("metastasis", "Disease", 1)

	emb := fakeEmbedder{vecs: map[string][]float32{
		"cancer":     {1, 0, 0},
		"ca":         {0.98, 0.2, 0}, // cosine vs cancer ≈ 0.98 (>= 0.93 → auto-merge)
		"metastasis": {0, 1, 0},      // cosine vs cancer = 0 (< gray-lo → separate)
	}}

	res, err := Resolve(context.Background(), nil, emb, g, 0.93, 0.86, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("expected 2 nodes (cancer+ca merged, metastasis separate), got %d: %+v", len(res.Nodes), res.Nodes)
	}
	var cancer *ResolvedNode
	for i := range res.Nodes {
		if res.Nodes[i].Name == "cancer" {
			cancer = &res.Nodes[i]
		}
	}
	if cancer == nil {
		t.Fatal("expected the high-frequency 'cancer' to be the canonical name")
	}
	found := false
	for _, a := range cancer.Aliases {
		if a == "ca" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'ca' merged as an alias of cancer, got aliases %v", cancer.Aliases)
	}
}

// TestNormalizeDirectionAndInverse checks inverse rewriting + endpoint-type
// orientation, which is the relation-vocabulary fix (#2).
func TestNormalizeDirectionAndInverse(t *testing.T) {
	ont := &domain.Ontology{
		EntityTypes: []domain.TypeDef{{Name: "Disease"}, {Name: "Symptom"}, {Name: "Other"}},
		Relations: []domain.RelationDef{
			{Name: "HAS_SYMPTOM", SourceType: "Disease", TargetType: "Symptom", Inverse: "SYMPTOM_OF"},
		},
	}
	res := &Resolved{
		Nodes: []ResolvedNode{
			{ID: "disease:flu", Type: "Disease", Name: "flu"},
			{ID: "symptom:fever", Type: "Symptom", Name: "fever"},
		},
		Relations: []ResolvedRelation{
			// Inverse phrasing: fever SYMPTOM_OF flu -> should become flu HAS_SYMPTOM fever.
			{SourceID: "symptom:fever", Relation: "SYMPTOM_OF", TargetID: "disease:flu"},
			// Reversed canonical: fever HAS_SYMPTOM flu -> endpoints flipped to flu->fever.
			{SourceID: "symptom:fever", Relation: "HAS_SYMPTOM", TargetID: "disease:flu"},
		},
	}
	res.NodeByID = map[string]*ResolvedNode{}
	for i := range res.Nodes {
		res.NodeByID[res.Nodes[i].ID] = &res.Nodes[i]
	}

	rep, err := Normalize(context.Background(), nil, ont, res, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Both inputs should collapse to the single canonical edge flu HAS_SYMPTOM fever.
	if len(res.Relations) != 1 {
		t.Fatalf("expected 1 canonical edge after dedup, got %d: %+v", len(res.Relations), res.Relations)
	}
	got := res.Relations[0]
	if got.SourceID != "disease:flu" || got.Relation != "HAS_SYMPTOM" || got.TargetID != "symptom:fever" {
		t.Errorf("got %+v, want flu HAS_SYMPTOM fever", got)
	}
	if rep.Inverted != 1 {
		t.Errorf("expected 1 inverted, got %d", rep.Inverted)
	}
	if rep.Flipped != 1 {
		t.Errorf("expected 1 flipped, got %d", rep.Flipped)
	}
}
