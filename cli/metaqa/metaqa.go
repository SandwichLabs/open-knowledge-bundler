// Package metaqa converts the MetaQA dataset (WikiMovies knowledge base + the
// 1/2/3-hop QA sets) into the cbi ingest format: pre-resolved nodes/edges
// NDJSON, a domain.yaml, a vocab.txt for precision scoring, and a
// questions.jsonl answer key the eval driver consumes.
//
// MetaQA's kb.txt is "subject|relation|object" per line; every relation has a
// movie as its subject, so the object type is determined by the relation. The
// data is distributed on Google Drive (see the upstream repo yuyuz/MetaQA); this
// package operates on a local copy.
package metaqa

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/eval"
)

// relSpec maps a kb relation to the object node type and the edge relationship
// type used in the graph. The subject is always a Movie.
type relSpec struct {
	objType string
	relType string
}

// Relations is the fixed MetaQA schema (9 relations).
var Relations = map[string]relSpec{
	"directed_by":     {"Person", "DIRECTED_BY"},
	"written_by":      {"Person", "WRITTEN_BY"},
	"starred_actors":  {"Person", "STARRED"},
	"release_year":    {"Year", "RELEASE_YEAR"},
	"in_language":     {"Language", "IN_LANGUAGE"},
	"has_genre":       {"Genre", "HAS_GENRE"},
	"has_tags":        {"Tag", "HAS_TAG"},
	"has_imdb_rating": {"Rating", "HAS_IMDB_RATING"},
	"has_imdb_votes":  {"Votes", "HAS_IMDB_VOTES"},
}

// typePrefix is the node_id prefix per node type.
var typePrefix = map[string]string{
	"Movie":    "movie",
	"Person":   "person",
	"Year":     "year",
	"Language": "language",
	"Genre":    "genre",
	"Tag":      "tag",
	"Rating":   "rating",
	"Votes":    "votes",
}

// Node is a pre-resolved cbi node record (matches the ingest NDJSON schema).
type Node struct {
	NodeID       string         `json:"node_id"`
	NodeType     string         `json:"node_type"`
	Properties   map[string]any `json:"properties"`
	SemanticText string         `json:"semantic_text"`
}

// Edge is a pre-resolved cbi edge record.
type Edge struct {
	EdgeID           string  `json:"edge_id"`
	SourceID         string  `json:"source_id"`
	TargetID         string  `json:"target_id"`
	RelationshipType string  `json:"relationship_type"`
	Weight           float64 `json:"weight"`
}

// Graph is the parsed knowledge base.
type Graph struct {
	Nodes []Node
	Edges []Edge
	// NameByID lets question gold answers (entity names) be cross-checked.
	NameByID map[string]string
}

// builder accumulates nodes/edges while de-duplicating entities and assigning
// stable, unique node ids.
type builder struct {
	idByKey map[string]string // type|name -> node_id
	usedID  map[string]bool   // node_id -> taken (slug collision guard)
	nodes   []Node
	edges   []Edge
	edgeSet map[string]bool // dedupe identical triples
}

func newBuilder() *builder {
	return &builder{
		idByKey: map[string]string{},
		usedID:  map[string]bool{},
		edgeSet: map[string]bool{},
	}
}

// ensure returns the node_id for (typ, name), creating the node on first sight.
func (b *builder) ensure(typ, name string) string {
	key := typ + "|" + name
	if id, ok := b.idByKey[key]; ok {
		return id
	}
	id := uniqueID(typePrefix[typ], name, b.usedID)
	b.usedID[id] = true
	b.idByKey[key] = id
	b.nodes = append(b.nodes, Node{
		NodeID:       id,
		NodeType:     typ,
		Properties:   map[string]any{"name": name},
		SemanticText: name,
	})
	return id
}

func (b *builder) addEdge(srcID, dstID, relType string) {
	eid := relType + "|" + srcID + "|" + dstID
	if b.edgeSet[eid] {
		return
	}
	b.edgeSet[eid] = true
	b.edges = append(b.edges, Edge{
		EdgeID:           strings.ToLower(eid),
		SourceID:         srcID,
		TargetID:         dstID,
		RelationshipType: relType,
		Weight:           1.0,
	})
}

// ParseKB reads a kb.txt stream (subject|relation|object) into a Graph.
// Unknown relations are skipped with no error so a partial KB still converts.
func ParseKB(r io.Reader) (*Graph, error) {
	b := newBuilder()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		parts := strings.SplitN(raw, "|", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("line %d: expected subject|relation|object, got %q", line, raw)
		}
		subj, rel, obj := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		spec, ok := Relations[rel]
		if !ok {
			continue
		}
		srcID := b.ensure("Movie", subj)
		dstID := b.ensure(spec.objType, obj)
		b.addEdge(srcID, dstID, spec.relType)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	names := make(map[string]string, len(b.nodes))
	for _, n := range b.nodes {
		names[n.NodeID] = n.Properties["name"].(string)
	}
	return &Graph{Nodes: b.nodes, Edges: b.edges, NameByID: names}, nil
}

// Vocab returns the de-duplicated set of entity names (for precision scoring).
func (g *Graph) Vocab() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range g.Nodes {
		name := n.Properties["name"].(string)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// ParseQuestions reads a MetaQA QA file ("question\tans1|ans2", topic entity in
// [brackets]) into eval.Questions. hop tags each question with its hop count.
// The bracketed topic entity is unwrapped so the question reads naturally.
func ParseQuestions(r io.Reader, hop int) ([]eval.Question, error) {
	var out []eval.Question
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	idx := 0
	for sc.Scan() {
		raw := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		qPart, ansPart, ok := strings.Cut(raw, "\t")
		if !ok {
			return nil, fmt.Errorf("question %d: missing tab separator: %q", idx, raw)
		}
		q := unwrapBrackets(strings.TrimSpace(qPart))
		ansField := strings.TrimSpace(ansPart)
		var gold []string
		for _, a := range strings.Split(ansField, "|") {
			if a = strings.TrimSpace(a); a != "" {
				gold = append(gold, a)
			}
		}
		out = append(out, eval.Question{
			ID:       fmt.Sprintf("%dhop-%d", hop, idx),
			Question: q,
			Gold:     gold,
			Tags:     map[string]string{"hop": fmt.Sprintf("%d", hop)},
		})
		idx++
	}
	return out, sc.Err()
}

// Sample returns up to n questions chosen deterministically (by seed) from the
// input. n <= 0 returns all questions unchanged. The selection is sorted back
// into original order for stable output.
func Sample(questions []eval.Question, n int, seed int64) []eval.Question {
	if n <= 0 || n >= len(questions) {
		return questions
	}
	idx := make([]int, len(questions))
	for i := range idx {
		idx[i] = i
	}
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	pick := idx[:n]
	sort.Ints(pick)
	out := make([]eval.Question, 0, n)
	for _, i := range pick {
		out = append(out, questions[i])
	}
	return out
}

// DomainYAML returns a cbi domain config for the converted movie knowledge
// graph. The NDJSON this package emits is pre-resolved, so the mappings are
// nominal; the file mainly declares the node/edge types and embedding settings
// that `cbi init` needs.
func DomainYAML(dbFile string) string {
	var b strings.Builder
	b.WriteString("domain_name: metaqa_movies\n")
	b.WriteString("embedding_dim: 768\n")
	b.WriteString("embedding_model: gemma\n")
	b.WriteString("endpoint_url: \"http://localhost:8080\"\n")
	fmt.Fprintf(&b, "database_path: %s\n\n", dbFile)

	b.WriteString("node_definitions:\n")
	nodeTypes := []string{"Movie", "Person", "Year", "Language", "Genre", "Tag", "Rating", "Votes"}
	for _, nt := range nodeTypes {
		fmt.Fprintf(&b, "  %s:\n", nt)
		b.WriteString("    semantic_fields:\n      - name\n")
		b.WriteString("    mappings:\n")
		b.WriteString("      - { source_field: \"node_id\", target_field: node_id, is_key: true }\n")
		b.WriteString("      - { source_field: \"name\",    target_field: name }\n")
	}

	b.WriteString("\nedge_definitions:\n")
	// Stable order over the relation set.
	rels := make([]string, 0, len(Relations))
	for _, spec := range Relations {
		rels = append(rels, spec.relType)
	}
	sort.Strings(rels)
	for _, rt := range rels {
		fmt.Fprintf(&b, "  %s:\n", rt)
		b.WriteString("    mappings:\n")
		b.WriteString("      - { source_field: \"source_id\", target_field: source_id, is_key: true }\n")
		b.WriteString("      - { source_field: \"target_id\", target_field: target_id, is_key: true }\n")
	}
	return b.String()
}

// unwrapBrackets removes the [ ] that MetaQA wraps around the topic entity.
func unwrapBrackets(s string) string {
	return strings.NewReplacer("[", "", "]", "").Replace(s)
}

// uniqueID builds prefix:slug(name), disambiguating slug collisions between
// distinct names with a numeric suffix so node ids stay unique.
func uniqueID(prefix, name string, used map[string]bool) string {
	base := prefix + ":" + slug(name)
	id := base
	for i := 2; used[id]; i++ {
		id = fmt.Sprintf("%s_%d", base, i)
	}
	return id
}

// slug lower-cases and replaces runs of non-alphanumeric characters with a
// single underscore.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
