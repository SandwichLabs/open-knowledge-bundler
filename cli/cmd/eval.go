package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/agent"
	"github.com/sandwich-labs/open-knowledge-bundler/cli/eval"
	"github.com/spf13/cobra"
)

var (
	evalBundle    string
	evalDB        string
	evalQuestions string
	evalTiers     []string
	evalModel     string
	evalProcessor string
	evalVocab     string
	evalLimit     int
	evalBy        string
	evalOut       string
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Benchmark the local agent against a known-answer question set",
	Long: `Runs the local agent (the same one behind 'okb agent') over a
questions.jsonl answer key and scores its answers deterministically: recall
(gold coverage), exact-match (Hits@all), and — when a vocabulary is available —
precision/F1 to catch over-generation (the hallucination mode). Honest "not
found" misses are counted separately from confident wrong answers.

The model loads once per tier and answers every question in-process (far faster
than spawning 'okb agent --ask' per question). Pass --tier more than once to
sweep model sizes and compare. A per-question results JSONL can be written with
--out; the leaderboard is printed to stdout.

questions.jsonl: one object per line, e.g.
  {"id":"q1","question":"How many Pokemon are there?","gold":["20"],"tags":{"hop":"1"}}

Examples:
  okb eval --bundle ./okf-bundle --questions q.jsonl
  okb eval --bundle ./okf-bundle --questions q.jsonl --tier small --tier medium --by hop
  okb eval --bundle ./okf-bundle --questions q.jsonl --vocab vocab.txt --out results.jsonl`,
	RunE: runEval,
}

func runEval(cmd *cobra.Command, args []string) error {
	// Questions.
	qf, err := os.Open(evalQuestions)
	if err != nil {
		return fmt.Errorf("opening questions file: %w", err)
	}
	questions, err := eval.ReadQuestions(qf)
	qf.Close()
	if err != nil {
		return fmt.Errorf("parsing questions: %w", err)
	}
	if evalLimit > 0 && evalLimit < len(questions) {
		questions = questions[:evalLimit]
	}
	if len(questions) == 0 {
		return fmt.Errorf("no questions to evaluate")
	}

	// Optional vocabulary for precision scoring: explicit --vocab, else a
	// vocab.txt sitting next to the bundle.
	vocab, vocabSrc := loadVocab(evalVocab, evalBundle)
	if vocab != nil {
		fmt.Fprintf(os.Stderr, "precision scoring on (%d entities from %s)\n", len(vocab), vocabSrc)
	} else {
		fmt.Fprintln(os.Stderr, "no vocabulary found — scoring recall/exact only (pass --vocab for precision/F1)")
	}

	// Force the llama.cpp backend before any kronk call.
	cfg, err := agent.LoadConfig(false, false)
	if err != nil {
		return err
	}
	if evalProcessor != "" {
		cfg.Processor = evalProcessor
	}
	if cfg.Processor != "" {
		if err := os.Setenv("KRONK_PROCESSOR", cfg.Processor); err != nil {
			return fmt.Errorf("setting KRONK_PROCESSOR: %w", err)
		}
	}

	bundle, err := agent.LoadBundle(evalBundle, evalDB)
	if err != nil {
		return err
	}

	tiers := evalTiers
	if len(tiers) == 0 {
		tiers = []string{cfg.Tier} // current configured tier
	}

	ctx := context.Background()
	var all []eval.Result
	for _, tier := range tiers {
		llmSource := evalModel
		if llmSource == "" {
			if err := cfg.SetTier(tier); err != nil {
				return err
			}
			llmSource = cfg.LLMSource()
		}

		fmt.Fprintf(os.Stderr, "\n=== tier %s (%s) — %d questions ===\n", tier, llmSource, len(questions))
		results, err := evalTier(ctx, bundle, llmSource, cfg.EmbedSource, tier, questions, vocab)
		if err != nil {
			return fmt.Errorf("tier %s: %w", tier, err)
		}
		all = append(all, results...)
	}

	if evalOut != "" {
		if err := writeResults(evalOut, all); err != nil {
			return fmt.Errorf("writing results: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d results to %s\n", len(all), evalOut)
	}

	// Leaderboard to stdout.
	fmt.Println()
	fmt.Print(eval.RenderTable(eval.Aggregate(all, evalBy)))
	return nil
}

// evalTier loads one model tier and answers every question with a fresh history.
func evalTier(ctx context.Context, bundle *agent.Bundle, llmSource, embedSource, tier string, questions []eval.Question, vocab map[string]struct{}) ([]eval.Result, error) {
	sess, err := agent.NewSession(ctx, bundle, llmSource, embedSource, true, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	})
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	results := make([]eval.Result, 0, len(questions))
	for i, q := range questions {
		sess.ResetHistory()
		ans, _ := sess.Answer(ctx, q.Question, llmSource)
		score := eval.Grade(ans.Answer, q, vocab)

		toolNames := make([]string, 0, len(ans.ToolCalls))
		for _, tc := range ans.ToolCalls {
			toolNames = append(toolNames, tc.Name)
		}
		r := eval.Result{
			Tier: tier, Model: llmSource, ID: q.ID, Question: q.Question, Gold: q.Gold,
			Answer: ans.Answer, Score: score, DurationMS: ans.DurationMS, Steps: ans.Steps,
			ToolCalls: toolNames, Tags: q.Tags, Error: ans.Error,
		}
		results = append(results, r)

		mark := outcomeMark(r.Outcome())
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s %s  (%d/%d gold, %.1fs)\n",
			i+1, len(questions), mark, trunc(q.Question, 56), score.GoldFound, score.GoldTotal,
			float64(ans.DurationMS)/1000)
	}
	return results, nil
}

func outcomeMark(o string) string {
	switch o {
	case "exact":
		return "✓"
	case "partial":
		return "~"
	case "honest_miss":
		return "·"
	case "error":
		return "!"
	default:
		return "✗"
	}
}

// loadVocab returns the precision vocabulary and a label for where it came
// from, or (nil, "") if none is available.
func loadVocab(explicit, bundleDir string) (map[string]struct{}, string) {
	path := explicit
	if path == "" {
		cand := filepath.Join(bundleDir, "vocab.txt")
		if _, err := os.Stat(cand); err == nil {
			path = cand
		}
	}
	if path == "" {
		return nil, ""
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read vocab %s: %v\n", path, err)
		return nil, ""
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			names = append(names, line)
		}
	}
	v := eval.NormalizeVocab(names)
	if len(v) == 0 {
		return nil, ""
	}
	return v, path
}

func writeResults(path string, results []eval.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func init() {
	evalCmd.Flags().StringVar(&evalBundle, "bundle", "", "path to an OKF bundle directory (required)")
	evalCmd.Flags().StringVar(&evalDB, "db", "", "override the bundle's DuckDB path")
	evalCmd.Flags().StringVar(&evalQuestions, "questions", "", "path to a questions.jsonl answer key (required)")
	evalCmd.Flags().StringSliceVar(&evalTiers, "tier", nil, "model tier(s) to evaluate; repeat to sweep (default: configured tier)")
	evalCmd.Flags().StringVar(&evalModel, "model", "", "override the LLM with an explicit kronk model source (single run)")
	evalCmd.Flags().StringVar(&evalProcessor, "gpu", "", "llama.cpp backend (cpu|cuda|rocm|vulkan); overrides config")
	evalCmd.Flags().StringVar(&evalVocab, "vocab", "", "entity-name vocabulary file for precision/F1 (default: <bundle>/vocab.txt)")
	evalCmd.Flags().IntVar(&evalLimit, "limit", 0, "evaluate only the first N questions")
	evalCmd.Flags().StringVar(&evalBy, "by", "", "break the leaderboard down by this question tag (e.g. hop)")
	evalCmd.Flags().StringVar(&evalOut, "out", "", "write per-question results to this JSONL file")
	_ = evalCmd.MarkFlagRequired("bundle")
	_ = evalCmd.MarkFlagRequired("questions")
	benchCmd.AddCommand(evalCmd)
}
