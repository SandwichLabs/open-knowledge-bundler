package extract

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Entity is an accumulated entity mention set: one logical entity with the
// surface names seen for it and the chunks it appeared in.
type Entity struct {
	Name    string          // chosen display name (longest/most-frequent surface form)
	Type    string          // ontology entity type
	Aliases map[string]bool // other surface forms (lowercased) for lexical recall
	Chunks  map[int]bool    // provenance: chunk ids the entity was seen in
}

// Relation is an accumulated relation mention between two entity surface names.
// Endpoints are raw surface names during extraction and become canonical node
// ids after resolution.
type Relation struct {
	Source   string
	Relation string
	Target   string
	Chunks   map[int]bool
}

// Graph is the mutable accumulator filled by stages 1–2 and rewritten by
// stages 3–4. Entities are keyed by normalized name; relations by a raw
// source|relation|target triple.
type Graph struct {
	Entities  map[string]*Entity   // normalizeName(name) -> entity
	Relations map[string]*Relation // src|REL|tgt -> relation
}

// NewGraph returns an empty accumulator.
func NewGraph() *Graph {
	return &Graph{
		Entities:  map[string]*Entity{},
		Relations: map[string]*Relation{},
	}
}

// AddEntity records an entity mention, merging by normalized name. A non-Other
// type upgrades a previously-unknown/Other type. The first non-normalized
// surface form becomes the display name; later forms are kept as aliases.
func (g *Graph) AddEntity(name, typ string, chunkID int) *Entity {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	key := normalizeName(name)
	if key == "" {
		return nil
	}
	e, ok := g.Entities[key]
	if !ok {
		e = &Entity{Name: name, Type: orOther(typ), Aliases: map[string]bool{}, Chunks: map[int]bool{}}
		g.Entities[key] = e
	} else {
		// Upgrade Other -> a concrete type; record alternate surface forms.
		if (e.Type == "" || e.Type == "Other") && typ != "" && typ != "Other" {
			e.Type = typ
		}
		if name != e.Name {
			e.Aliases[strings.ToLower(name)] = true
		}
	}
	e.Chunks[chunkID] = true
	return e
}

// AddRelation records a relation mention between two raw surface names.
func (g *Graph) AddRelation(source, relation, target string, chunkID int) {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	relation = normalizeRelation(relation)
	if source == "" || target == "" || relation == "" || strings.EqualFold(source, target) {
		return
	}
	key := normalizeName(source) + "|" + relation + "|" + normalizeName(target)
	r, ok := g.Relations[key]
	if !ok {
		r = &Relation{Source: source, Relation: relation, Target: target, Chunks: map[int]bool{}}
		g.Relations[key] = r
	}
	r.Chunks[chunkID] = true
}

// EntityByName resolves a surface name to its accumulated entity (post-merge).
func (g *Graph) EntityByName(name string) *Entity {
	return g.Entities[normalizeName(name)]
}

// rawGraph is the JSON-serializable form of a pre-resolution Graph (sets are
// flattened to sorted slices). Persisting it lets resolution be re-run/re-tuned
// later without re-extracting from the corpus (the expensive stage).
type rawGraph struct {
	Entities  []rawEntity   `json:"entities"`
	Relations []rawRelation `json:"relations"`
}

type rawEntity struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases,omitempty"`
	Chunks  []int    `json:"chunks,omitempty"`
}

type rawRelation struct {
	Source   string `json:"source"`
	Relation string `json:"relation"`
	Target   string `json:"target"`
	Chunks   []int  `json:"chunks,omitempty"`
}

// Save writes the accumulated (pre-resolution) graph to path as JSON.
func (g *Graph) Save(path string) error {
	var rg rawGraph
	ekeys := make([]string, 0, len(g.Entities))
	for k := range g.Entities {
		ekeys = append(ekeys, k)
	}
	sort.Strings(ekeys)
	for _, k := range ekeys {
		e := g.Entities[k]
		rg.Entities = append(rg.Entities, rawEntity{
			Name: e.Name, Type: e.Type, Aliases: setToSorted(e.Aliases), Chunks: intsToSorted(e.Chunks),
		})
	}
	rkeys := make([]string, 0, len(g.Relations))
	for k := range g.Relations {
		rkeys = append(rkeys, k)
	}
	sort.Strings(rkeys)
	for _, k := range rkeys {
		r := g.Relations[k]
		rg.Relations = append(rg.Relations, rawRelation{
			Source: r.Source, Relation: r.Relation, Target: r.Target, Chunks: intsToSorted(r.Chunks),
		})
	}
	out, err := json.MarshalIndent(rg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// LoadGraph reconstructs a Graph from a file written by Save, preserving the
// same entity/relation keys so resolution behaves identically.
func LoadGraph(path string) (*Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rg rawGraph
	if err := json.Unmarshal(data, &rg); err != nil {
		return nil, err
	}
	g := NewGraph()
	for _, e := range rg.Entities {
		ent := &Entity{Name: e.Name, Type: orOther(e.Type), Aliases: map[string]bool{}, Chunks: map[int]bool{}}
		for _, a := range e.Aliases {
			ent.Aliases[a] = true
		}
		for _, c := range e.Chunks {
			ent.Chunks[c] = true
		}
		g.Entities[normalizeName(e.Name)] = ent
	}
	for _, r := range rg.Relations {
		rr := &Relation{Source: r.Source, Relation: r.Relation, Target: r.Target, Chunks: map[int]bool{}}
		for _, c := range r.Chunks {
			rr.Chunks[c] = true
		}
		g.Relations[normalizeName(r.Source)+"|"+r.Relation+"|"+normalizeName(r.Target)] = rr
	}
	return g, nil
}

func setToSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intsToSorted(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func orOther(typ string) string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return "Other"
	}
	return typ
}

var (
	nonAlnum   = regexp.MustCompile(`[^a-z0-9]+`)
	multiUnder = regexp.MustCompile(`_+`)
)

// normalizeName lowercases, strips punctuation to single underscores, and
// trivially singularizes a trailing plural so "tumors"/"tumor" collide. This is
// the exact-merge key; embedding clustering handles the fuzzier cases.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = multiUnder.ReplaceAllString(nonAlnum.ReplaceAllString(s, "_"), "_")
	s = strings.Trim(s, "_")
	s = singularize(s)
	return s
}

// singularize trims a few common English plural suffixes. Deliberately
// conservative — it only strips a trailing "s" when preceded by a consonant
// (so "tumors"->"tumor", "symptoms"->"symptom") and leaves vowel+s endings
// ("diabetes", "diseases", "virus") alone. False non-merges are recovered by
// embedding clustering; false merges are not, so we err toward not merging.
func singularize(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 4:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "s") && len(s) > 3 && isConsonant(s[len(s)-2]):
		return s[:len(s)-1]
	}
	return s
}

func isConsonant(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u', 's':
		return false
	}
	return b >= 'a' && b <= 'z'
}

// normalizeRelation upper-snake-cases a relation surface form.
func normalizeRelation(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = multiUnder.ReplaceAllString(nonAlnum.ReplaceAllString(strings.ToLower(s), "_"), "_")
	return strings.ToUpper(strings.Trim(s, "_"))
}

// slug produces an id-safe token from a name (lowercase, underscore-separated).
func slug(s string) string {
	s = multiUnder.ReplaceAllString(nonAlnum.ReplaceAllString(strings.ToLower(s), "_"), "_")
	return strings.Trim(s, "_")
}
