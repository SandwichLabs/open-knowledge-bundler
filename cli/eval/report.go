package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Result is one fully-graded question: the agent's answer plus its score and
// the run metadata the leaderboard aggregates over. Results are written to the
// --out JSONL verbatim.
type Result struct {
	Tier       string            `json:"tier"`
	Model      string            `json:"model"`
	ID         string            `json:"id,omitempty"`
	Question   string            `json:"question"`
	Gold       []string          `json:"gold"`
	Answer     string            `json:"answer"`
	Score      Score             `json:"score"`
	DurationMS int64             `json:"duration_ms"`
	Steps      int               `json:"steps"`
	ToolCalls  []string          `json:"tool_calls,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// Outcome classifies a result for the leaderboard: a confident wrong answer is
// worse than an honest miss, so they are counted separately.
func (r Result) Outcome() string {
	switch {
	case r.Error != "":
		return "error"
	case r.Score.Exact:
		return "exact"
	case r.Score.GoldFound > 0:
		return "partial"
	case r.Score.HonestMiss:
		return "honest_miss"
	default:
		return "wrong"
	}
}

// Summary is the aggregate scoreboard for one group of results.
type Summary struct {
	Group          string  `json:"group"`
	N              int     `json:"n"`
	ExactRate      float64 `json:"exact_rate"`
	MeanRecall     float64 `json:"mean_recall"`
	VocabAware     bool    `json:"vocab_aware"`
	MeanPrecision  float64 `json:"mean_precision,omitempty"`
	MeanF1         float64 `json:"mean_f1,omitempty"`
	Partial        int     `json:"partial"`
	HonestMiss     int     `json:"honest_miss"`
	Wrong          int     `json:"wrong"`
	Errors         int     `json:"errors"`
	MeanDurationMS float64 `json:"mean_duration_ms"`
	MeanSteps      float64 `json:"mean_steps"`
}

// Aggregate groups results by tier (and, when byTag is non-empty, by that tag's
// value within each tier) and computes a Summary per group. Groups are returned
// sorted by name for stable output.
func Aggregate(results []Result, byTag string) []Summary {
	groups := map[string][]Result{}
	for _, r := range results {
		key := r.Tier
		if byTag != "" {
			key = fmt.Sprintf("%s / %s=%s", r.Tier, byTag, r.Tags[byTag])
		}
		groups[key] = append(groups[key], r)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Summary, 0, len(keys))
	for _, k := range keys {
		rs := groups[k]
		s := Summary{Group: k, N: len(rs), VocabAware: true}
		var recall, precision, f1, dur, steps float64
		var vocabN int
		for _, r := range rs {
			recall += r.Score.Recall
			dur += float64(r.DurationMS)
			steps += float64(r.Steps)
			if r.Score.VocabAware {
				precision += r.Score.Precision
				f1 += r.Score.F1
				vocabN++
			}
			switch r.Outcome() {
			case "exact":
				s.ExactRate++ // accumulate count, divide below
			case "partial":
				s.Partial++
			case "honest_miss":
				s.HonestMiss++
			case "wrong":
				s.Wrong++
			case "error":
				s.Errors++
			}
		}
		n := float64(len(rs))
		s.ExactRate /= n
		s.MeanRecall = recall / n
		s.MeanDurationMS = dur / n
		s.MeanSteps = steps / n
		if vocabN == len(rs) && vocabN > 0 {
			s.MeanPrecision = precision / float64(vocabN)
			s.MeanF1 = f1 / float64(vocabN)
		} else {
			s.VocabAware = false
		}
		out = append(out, s)
	}
	return out
}

// RenderTable formats summaries as a fixed-width leaderboard. Precision/F1
// columns are shown only when every group was scored with a vocabulary.
func RenderTable(summaries []Summary) string {
	vocab := len(summaries) > 0
	for _, s := range summaries {
		if !s.VocabAware {
			vocab = false
		}
	}

	var b strings.Builder
	if vocab {
		fmt.Fprintf(&b, "%-28s %4s  %7s  %7s  %7s  %5s  %7s  %5s  %5s  %7s  %5s\n",
			"group", "n", "exact", "recall", "prec", "f1", "partial", "miss", "wrong", "ms/q", "steps")
		for _, s := range summaries {
			fmt.Fprintf(&b, "%-28s %4d  %6.1f%%  %7.3f  %7.3f  %5.3f  %7d  %5d  %5d  %7.0f  %5.1f\n",
				trunc(s.Group, 28), s.N, s.ExactRate*100, s.MeanRecall, s.MeanPrecision, s.MeanF1,
				s.Partial, s.HonestMiss, s.Wrong, s.MeanDurationMS, s.MeanSteps)
		}
	} else {
		fmt.Fprintf(&b, "%-28s %4s  %7s  %7s  %7s  %5s  %5s  %7s  %5s\n",
			"group", "n", "exact", "recall", "partial", "miss", "wrong", "ms/q", "steps")
		for _, s := range summaries {
			fmt.Fprintf(&b, "%-28s %4d  %6.1f%%  %7.3f  %7d  %5d  %5d  %7.0f  %5.1f\n",
				trunc(s.Group, 28), s.N, s.ExactRate*100, s.MeanRecall,
				s.Partial, s.HonestMiss, s.Wrong, s.MeanDurationMS, s.MeanSteps)
		}
	}
	return b.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
