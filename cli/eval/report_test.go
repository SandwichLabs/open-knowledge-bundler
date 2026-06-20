package eval

import (
	"strings"
	"testing"
)

func TestAggregateGroupsAndOutcomes(t *testing.T) {
	results := []Result{
		{Tier: "E4B", Tags: map[string]string{"hop": "1"}, DurationMS: 1000, Steps: 2,
			Score: Score{GoldFound: 1, GoldTotal: 1, Recall: 1, Exact: true}},
		{Tier: "E4B", Tags: map[string]string{"hop": "1"}, DurationMS: 3000, Steps: 4,
			Score: Score{GoldFound: 0, GoldTotal: 2, Recall: 0, HonestMiss: true}},
		{Tier: "E4B", Tags: map[string]string{"hop": "2"}, DurationMS: 2000, Steps: 3,
			Score: Score{GoldFound: 1, GoldTotal: 3, Recall: 0.333}},
	}

	// Grouped by tier only.
	byTier := Aggregate(results, "")
	if len(byTier) != 1 || byTier[0].N != 3 {
		t.Fatalf("expected one tier group of 3, got %+v", byTier)
	}
	g := byTier[0]
	if g.HonestMiss != 1 || g.Partial != 1 {
		t.Fatalf("expected 1 honest miss + 1 partial, got %+v", g)
	}
	if g.ExactRate <= 0.33 || g.ExactRate >= 0.34 { // 1 of 3
		t.Fatalf("expected exact rate ~0.333, got %v", g.ExactRate)
	}
	if g.MeanDurationMS != 2000 {
		t.Fatalf("expected mean duration 2000, got %v", g.MeanDurationMS)
	}

	// Broken down by hop.
	byHop := Aggregate(results, "hop")
	if len(byHop) != 2 {
		t.Fatalf("expected 2 hop groups, got %d (%+v)", len(byHop), byHop)
	}
}

func TestRenderTableHasHeaders(t *testing.T) {
	out := RenderTable(Aggregate([]Result{
		{Tier: "E4B", Score: Score{GoldFound: 1, GoldTotal: 1, Recall: 1, Exact: true}},
	}, ""))
	for _, want := range []string{"group", "exact", "recall", "E4B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}
