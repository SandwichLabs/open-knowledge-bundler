package eval

import (
	"strings"
	"testing"
)

func TestGradeRecallAndExact(t *testing.T) {
	q := Question{Question: "Which Pokemon does Ash own?", Gold: []string{"Squirtle", "Charmander", "Bulbasaur", "Pikachu"}}

	full := Grade("Ash Ketchum owns Squirtle, Charmander, Bulbasaur, and Pikachu.", q, nil)
	if !full.Exact || full.Recall != 1.0 || full.GoldFound != 4 {
		t.Fatalf("expected exact 4/4, got %+v", full)
	}

	partial := Grade("Ash owns Squirtle and Pikachu.", q, nil)
	if partial.Exact || partial.GoldFound != 2 || partial.Recall != 0.5 {
		t.Fatalf("expected 2/4 recall 0.5, got %+v", partial)
	}
}

func TestGradeWholeTokenMatching(t *testing.T) {
	// "Ash" must not match inside "Ashley"; multi-word names must match.
	q := Question{Gold: []string{"Ash", "Mr. Mime"}}
	s := Grade("The trainer Ashley is here with Mr. Mime.", q, nil)
	if s.GoldFound != 1 {
		t.Fatalf("expected only 'Mr. Mime' to match (not Ash in Ashley), got %d (%+v)", s.GoldFound, s)
	}
}

func TestGradePrecisionCatchesOverGeneration(t *testing.T) {
	// The Eevee case: graph holds 3, model lists all 8 real-world Eeveelutions.
	q := Question{Gold: []string{"Vaporeon", "Jolteon", "Flareon"}}
	vocab := NormalizeVocab([]string{
		"Vaporeon", "Jolteon", "Flareon", "Espeon", "Umbreon", "Leafeon", "Glaceon", "Sylveon",
	})
	answer := "Eevee evolves into Vaporeon, Jolteon, Flareon, Espeon, Umbreon, Leafeon, Glaceon, and Sylveon."
	s := Grade(answer, q, vocab)
	if !s.Exact {
		t.Fatalf("recall should be perfect (all 3 gold present): %+v", s)
	}
	if s.VocabMentioned != 8 {
		t.Fatalf("expected 8 entities mentioned, got %d", s.VocabMentioned)
	}
	if s.Precision != 3.0/8.0 {
		t.Fatalf("expected precision 3/8 for over-generation, got %v (%+v)", s.Precision, s)
	}
	// F1 should sit below recall, flagging the over-generation.
	if s.F1 >= 1.0 {
		t.Fatalf("F1 should be penalized by low precision, got %v", s.F1)
	}
}

func TestGradePrecisionPerfect(t *testing.T) {
	q := Question{Gold: []string{"Vaporeon", "Jolteon", "Flareon"}}
	vocab := NormalizeVocab([]string{"Vaporeon", "Jolteon", "Flareon", "Espeon", "Umbreon"})
	s := Grade("Eevee evolves into Vaporeon, Jolteon, and Flareon.", q, vocab)
	if s.Precision != 1.0 || s.Recall != 1.0 || s.F1 != 1.0 {
		t.Fatalf("expected perfect P/R/F1, got %+v", s)
	}
}

func TestGradePrecisionExcludesTopicEntity(t *testing.T) {
	// The model restating the question's subject (Eevee) must not count as a
	// false positive: precision should be perfect when only gold + subject are named.
	q := Question{
		Question: "List all of Eevee's evolutions.",
		Gold:     []string{"Vaporeon", "Jolteon", "Flareon"},
	}
	vocab := NormalizeVocab([]string{"Eevee", "Vaporeon", "Jolteon", "Flareon", "Espeon"})
	s := Grade("Eevee evolves into Vaporeon, Jolteon, and Flareon.", q, vocab)
	if s.Precision != 1.0 {
		t.Fatalf("topic entity (Eevee) should be excluded; expected precision 1.0, got %v (mentioned %d)", s.Precision, s.VocabMentioned)
	}
	// But a real extra answer entity (Espeon) still drops precision.
	s2 := Grade("Eevee evolves into Vaporeon, Jolteon, Flareon, and Espeon.", q, vocab)
	if s2.Precision != 3.0/4.0 {
		t.Fatalf("expected precision 3/4 with one real over-generation, got %v", s2.Precision)
	}
}

func TestGradeHonestMiss(t *testing.T) {
	q := Question{Gold: []string{"Bulbasaur", "Charmander"}}
	miss := Grade("I could not find any records for that in the graph.", q, nil)
	if !miss.HonestMiss || miss.GoldFound != 0 {
		t.Fatalf("expected honest miss, got %+v", miss)
	}
	wrong := Grade("That trainer owns Mewtwo.", q, nil)
	if wrong.HonestMiss {
		t.Fatalf("a confident wrong answer is not an honest miss: %+v", wrong)
	}
}

func TestReadQuestions(t *testing.T) {
	in := `# a comment
{"id":"q1","question":"how many?","gold":["20"],"tags":{"hop":"1"}}

{"question":"who?","gold":["Ash","Misty"]}
`
	qs, err := ReadQuestions(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(qs))
	}
	if qs[0].ID != "q1" || qs[0].Tags["hop"] != "1" || len(qs[1].Gold) != 2 {
		t.Fatalf("unexpected parse: %+v", qs)
	}
}
