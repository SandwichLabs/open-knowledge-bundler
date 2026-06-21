package extract

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Embedder produces embedding vectors. agent.Embedder satisfies this; keeping it
// an interface decouples the extract package from the agent's heavy deps.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ResolvedNode is a canonical entity after resolution.
type ResolvedNode struct {
	ID      string
	Type    string
	Name    string
	Aliases []string
	Chunks  []int
}

// ResolvedRelation has endpoints remapped to canonical node ids.
type ResolvedRelation struct {
	SourceID string
	Relation string
	TargetID string
	Chunks   []int
}

// Resolved is the output of stage 3: canonical nodes + relations whose endpoints
// are canonical node ids.
type Resolved struct {
	Nodes      []ResolvedNode
	Relations  []ResolvedRelation
	NodeByID   map[string]*ResolvedNode
	Merged     int // entities collapsed by clustering (beyond exact merge)
	Adjudicate int // LLM adjudication calls made
}

// adjudicateSchema constrains the same/different judgment.
var adjudicateSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"same":      map[string]any{"type": "boolean"},
		"canonical": map[string]any{"type": "string"},
	},
	"required": []string{"same"},
}

const adjudicateSystem = `You decide whether two entity names refer to the SAME real-world thing in this domain. Answer same=true only for genuine synonyms, abbreviations, or spelling/pluralization variants (e.g. "adrenocortical carcinoma" vs "adrenocortical cancer"). Answer same=false for distinct-but-related things (e.g. a disease vs its subtype, a drug vs its class). When same=true, set canonical to the clearer full name.`

// Resolve performs stage 3. Entities are already exact-merged in g (by
// normalized name). This embeds each surviving entity, clusters within each type
// by cosine >= threshold, LLM-adjudicates gray-band pairs in [grayLo, threshold),
// then canonicalizes clusters and remaps every relation endpoint to the chosen
// canonical node id. maxAdjudicate caps LLM calls (0 = unlimited).
func Resolve(ctx context.Context, gen *Generator, emb Embedder, g *Graph, threshold, grayLo float64, maxAdjudicate int, progress ProgressFunc) (*Resolved, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}

	// Stable entity ordering for determinism.
	keys := make([]string, 0, len(g.Entities))
	for k := range g.Entities {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Embed each entity name (skip if no embedder — falls back to exact-merge only).
	vecs := make(map[string][]float32, len(keys))
	if emb != nil {
		for _, k := range keys {
			v, err := emb.Embed(ctx, g.Entities[k].Name)
			if err != nil {
				return nil, fmt.Errorf("embedding %q: %w", g.Entities[k].Name, err)
			}
			vecs[k] = v
		}
	}

	// Group entity keys by type (clustering only happens within a type).
	byType := map[string][]string{}
	for _, k := range keys {
		t := g.Entities[k].Type
		byType[t] = append(byType[t], k)
	}

	uf := newUnionFind(keys)
	adjCalls := 0
	if emb != nil {
		typeNames := make([]string, 0, len(byType))
		for t := range byType {
			typeNames = append(typeNames, t)
		}
		sort.Strings(typeNames)

		for _, t := range typeNames {
			members := byType[t]
			for i := 0; i < len(members); i++ {
				for j := i + 1; j < len(members); j++ {
					a, b := members[i], members[j]
					if uf.find(a) == uf.find(b) {
						continue
					}
					sim := cosine(vecs[a], vecs[b])
					switch {
					case sim >= threshold:
						uf.union(a, b)
					case sim >= grayLo:
						if maxAdjudicate > 0 && adjCalls >= maxAdjudicate {
							continue // cap reached; leave as separate
						}
						adjCalls++
						if same, err := adjudicate(ctx, gen, g.Entities[a].Name, g.Entities[b].Name); err == nil && same {
							uf.union(a, b)
						}
					}
				}
			}
		}
	}
	if maxAdjudicate > 0 && adjCalls >= maxAdjudicate {
		progress("  ! entity-resolution adjudication hit the cap of %d calls; remaining gray-band pairs left unmerged", maxAdjudicate)
	}

	// Build clusters and canonicalize.
	clusters := map[string][]string{}
	for _, k := range keys {
		root := uf.find(k)
		clusters[root] = append(clusters[root], k)
	}

	res := &Resolved{NodeByID: map[string]*ResolvedNode{}, Adjudicate: adjCalls}
	nameToID := map[string]string{} // normalized name -> canonical node id
	usedID := map[string]bool{}
	merged := 0

	// Deterministic cluster order.
	roots := make([]string, 0, len(clusters))
	for r := range clusters {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	for _, r := range roots {
		members := clusters[r]
		sort.Strings(members)
		rep := pickRepresentative(g, members)
		repEnt := g.Entities[rep]

		// Collect aliases + chunks from all members.
		aliasSet := map[string]bool{}
		chunkSet := map[int]bool{}
		for _, m := range members {
			e := g.Entities[m]
			if m != rep {
				aliasSet[strings.ToLower(e.Name)] = true
				merged++
			}
			for a := range e.Aliases {
				aliasSet[a] = true
			}
			for c := range e.Chunks {
				chunkSet[c] = true
			}
		}
		delete(aliasSet, strings.ToLower(repEnt.Name))

		id := mkNodeID(repEnt.Type, repEnt.Name, usedID)
		usedID[id] = true
		node := ResolvedNode{
			ID:      id,
			Type:    repEnt.Type,
			Name:    repEnt.Name,
			Aliases: sortedKeys(aliasSet),
			Chunks:  sortedInts(chunkSet),
		}
		res.Nodes = append(res.Nodes, node)
		for _, m := range members {
			nameToID[m] = id
		}
	}
	for i := range res.Nodes {
		res.NodeByID[res.Nodes[i].ID] = &res.Nodes[i]
	}
	res.Merged = merged

	// Remap relations to canonical node ids; drop self-loops and danglers.
	seen := map[string]*ResolvedRelation{}
	for _, rel := range g.Relations {
		sID, ok1 := nameToID[normalizeName(rel.Source)]
		tID, ok2 := nameToID[normalizeName(rel.Target)]
		if !ok1 || !ok2 || sID == tID {
			continue
		}
		key := sID + "|" + rel.Relation + "|" + tID
		if existing, ok := seen[key]; ok {
			existing.Chunks = mergeInts(existing.Chunks, sortedInts(rel.Chunks))
			continue
		}
		rr := &ResolvedRelation{SourceID: sID, Relation: rel.Relation, TargetID: tID, Chunks: sortedInts(rel.Chunks)}
		seen[key] = rr
	}
	for _, rr := range seen {
		res.Relations = append(res.Relations, *rr)
	}
	sort.Slice(res.Relations, func(i, j int) bool {
		if res.Relations[i].SourceID != res.Relations[j].SourceID {
			return res.Relations[i].SourceID < res.Relations[j].SourceID
		}
		if res.Relations[i].Relation != res.Relations[j].Relation {
			return res.Relations[i].Relation < res.Relations[j].Relation
		}
		return res.Relations[i].TargetID < res.Relations[j].TargetID
	})
	return res, nil
}

func adjudicate(ctx context.Context, gen *Generator, a, b string) (bool, error) {
	var out struct {
		Same      bool   `json:"same"`
		Canonical string `json:"canonical"`
	}
	user := fmt.Sprintf("Entity A: %q\nEntity B: %q\nDo they refer to the same thing?", a, b)
	if _, err := gen.GenerateJSON(ctx, adjudicateSystem, user, adjudicateSchema, &out); err != nil {
		return false, err
	}
	return out.Same, nil
}

// pickRepresentative chooses the canonical member: most chunk mentions, then the
// shorter name (prefers the more frequently-attested, less-verbose surface form).
func pickRepresentative(g *Graph, members []string) string {
	best := members[0]
	for _, m := range members[1:] {
		em, eb := g.Entities[m], g.Entities[best]
		if len(em.Chunks) > len(eb.Chunks) ||
			(len(em.Chunks) == len(eb.Chunks) && len(em.Name) < len(eb.Name)) ||
			(len(em.Chunks) == len(eb.Chunks) && len(em.Name) == len(eb.Name) && em.Name < eb.Name) {
			best = m
		}
	}
	return best
}

// mkNodeID builds a unique `typeprefix:nameslug` id (matches extract_graph.py).
func mkNodeID(typ, name string, used map[string]bool) string {
	prefix := slug(typ)
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	if prefix == "" {
		prefix = "ent"
	}
	base := prefix + ":" + slug(name)
	id, k := base, 2
	for used[id] {
		id = fmt.Sprintf("%s_%d", base, k)
		k++
	}
	return id
}

// --- small helpers ---

type unionFind struct{ parent map[string]string }

func newUnionFind(keys []string) *unionFind {
	uf := &unionFind{parent: make(map[string]string, len(keys))}
	for _, k := range keys {
		uf.parent[k] = k
	}
	return uf
}

func (uf *unionFind) find(x string) string {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]]
		x = uf.parent[x]
	}
	return x
}

func (uf *unionFind) union(a, b string) {
	ra, rb := uf.find(a), uf.find(b)
	if ra != rb {
		uf.parent[ra] = rb
	}
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func mergeInts(a, b []int) []int {
	seen := map[int]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		seen[x] = true
	}
	return sortedInts(seen)
}
