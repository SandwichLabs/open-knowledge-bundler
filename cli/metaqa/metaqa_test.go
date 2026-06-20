package metaqa

import (
	"strings"
	"testing"
)

const sampleKB = `Kismet|directed_by|William Dieterle
Kismet|starred_actors|Ronald Colman
Kismet|starred_actors|Marlene Dietrich
Kismet|release_year|1944
Kismet|has_genre|Drama
Vamp|directed_by|Richard Wenk
Vamp|starred_actors|Grace Jones
`

func TestParseKBTypesAndEdges(t *testing.T) {
	g, err := ParseKB(strings.NewReader(sampleKB))
	if err != nil {
		t.Fatal(err)
	}

	typeOf := map[string]string{}
	for _, n := range g.Nodes {
		typeOf[n.Properties["name"].(string)] = n.NodeType
	}
	checks := map[string]string{
		"Kismet":          "Movie",
		"William Dieterle": "Person",
		"Ronald Colman":   "Person",
		"1944":            "Year",
		"Drama":           "Genre",
	}
	for name, want := range checks {
		if got := typeOf[name]; got != want {
			t.Errorf("%s: type = %q, want %q", name, got, want)
		}
	}

	// Two movies, distinct actors/directors → unique nodes; no dupes.
	if len(g.Nodes) != 9 {
		t.Errorf("expected 9 distinct nodes, got %d", len(g.Nodes))
	}
	// 7 triples with known relations → 7 edges.
	if len(g.Edges) != 7 {
		t.Errorf("expected 7 edges, got %d", len(g.Edges))
	}

	// Movie is always the edge source.
	for _, e := range g.Edges {
		if !strings.HasPrefix(e.SourceID, "movie:") {
			t.Errorf("edge %s source %q is not a movie", e.RelationshipType, e.SourceID)
		}
	}
}

func TestParseKBUnknownRelationSkipped(t *testing.T) {
	g, err := ParseKB(strings.NewReader("A|sequel_of|B\nA|directed_by|C\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 1 || g.Edges[0].RelationshipType != "DIRECTED_BY" {
		t.Fatalf("unknown relation should be skipped; got %+v", g.Edges)
	}
}

func TestParseKBDedup(t *testing.T) {
	// Same person directs two movies → one Person node, two edges.
	kb := "M1|directed_by|Same Director\nM2|directed_by|Same Director\n"
	g, _ := ParseKB(strings.NewReader(kb))
	people := 0
	for _, n := range g.Nodes {
		if n.NodeType == "Person" {
			people++
		}
	}
	if people != 1 {
		t.Errorf("expected 1 deduped Person node, got %d", people)
	}
	if len(g.Edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(g.Edges))
	}
}

func TestVocab(t *testing.T) {
	g, _ := ParseKB(strings.NewReader(sampleKB))
	v := g.Vocab()
	if len(v) != 9 {
		t.Errorf("expected 9 vocab names, got %d: %v", len(v), v)
	}
}

func TestParseQuestions(t *testing.T) {
	qa := "what movies did [Grace Jones] appear in\tVamp|Boomerang\nwho directed [Vamp]\tRichard Wenk\n"
	qs, err := ParseQuestions(strings.NewReader(qa), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(qs))
	}
	if strings.ContainsAny(qs[0].Question, "[]") {
		t.Errorf("brackets not stripped: %q", qs[0].Question)
	}
	if qs[0].Question != "what movies did Grace Jones appear in" {
		t.Errorf("unexpected question text: %q", qs[0].Question)
	}
	if len(qs[0].Gold) != 2 || qs[0].Gold[0] != "Vamp" {
		t.Errorf("unexpected gold: %v", qs[0].Gold)
	}
	if qs[0].Tags["hop"] != "2" || qs[0].ID != "2hop-0" {
		t.Errorf("unexpected tags/id: %v %s", qs[0].Tags, qs[0].ID)
	}
}

func TestSlugUniqueness(t *testing.T) {
	used := map[string]bool{}
	a := uniqueID("movie", "Spider-Man", used)
	used[a] = true
	b := uniqueID("movie", "Spider Man", used) // same slug, different name
	if a == b {
		t.Fatalf("slug collision not disambiguated: %s == %s", a, b)
	}
}
