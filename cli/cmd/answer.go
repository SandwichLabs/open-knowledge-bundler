package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/agent"
	"github.com/sandwich-labs/open-knowledge-bundler/cli/eval"
	"github.com/spf13/cobra"
)

var (
	ansBundle    string
	ansDB        string
	ansQuestions string
	ansTier      string
	ansModel     string
	ansProcessor string
	ansOut       string
	ansLimit     int
	ansCtxChars  int
)

// answerRecord is the neutral per-question output: the agent's answer plus the
// retrieved context (concatenated tool outputs) and run metadata. It passes the
// question's gold/tags through so an external harness (e.g. GraphRAG-Bench) can
// map ground_truth / question_type without re-reading the source file.
type answerRecord struct {
	ID         string            `json:"id"`
	Question   string            `json:"question"`
	Answer     string            `json:"generated_answer"`
	Context    string            `json:"context"`
	Gold       []string          `json:"gold,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	ToolCalls  []agent.AskToolCall `json:"tool_calls,omitempty"`
	Steps      int               `json:"steps"`
	DurationMS int64             `json:"duration_ms"`
	Error      string            `json:"error,omitempty"`
}

var answerCmd = &cobra.Command{
	Use:   "answer",
	Short: "Batch-answer a question set with the local agent (no scoring)",
	Long: `Runs the local agent over a questions.jsonl and emits, per question, the
generated answer plus the retrieved context (the concatenated tool outputs) and
run metadata — as a JSON array. The model loads once and answers in-process.

Unlike 'okb eval' this does not score; it produces raw answers + context for an
external evaluator (e.g. GraphRAG-Bench, which judges separately). Question gold
and tags are passed through for downstream mapping.

Examples:
  okb answer --bundle ./med-bundle --questions q.jsonl --out results.json
  okb answer --bundle ./med-bundle --questions q.jsonl --limit 30 --tier medium`,
	RunE: runAnswer,
}

func runAnswer(cmd *cobra.Command, args []string) error {
	qf, err := os.Open(ansQuestions)
	if err != nil {
		return fmt.Errorf("opening questions: %w", err)
	}
	questions, err := eval.ReadQuestions(qf)
	qf.Close()
	if err != nil {
		return fmt.Errorf("parsing questions: %w", err)
	}
	if ansLimit > 0 && ansLimit < len(questions) {
		questions = questions[:ansLimit]
	}
	if len(questions) == 0 {
		return fmt.Errorf("no questions to answer")
	}

	cfg, err := agent.LoadConfig(false, false)
	if err != nil {
		return err
	}
	if ansTier != "" {
		if err := cfg.SetTier(ansTier); err != nil {
			return err
		}
	}
	if ansProcessor != "" {
		cfg.Processor = ansProcessor
	}
	if cfg.Processor != "" {
		if err := os.Setenv("KRONK_PROCESSOR", cfg.Processor); err != nil {
			return fmt.Errorf("setting KRONK_PROCESSOR: %w", err)
		}
	}

	bundle, err := agent.LoadBundle(ansBundle, ansDB)
	if err != nil {
		return err
	}
	llmSource := cfg.LLMSource()
	if ansModel != "" {
		llmSource = ansModel
	}

	ctx := context.Background()
	sess, err := agent.NewSession(ctx, bundle, llmSource, cfg.EmbedSource, true, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	records := make([]answerRecord, 0, len(questions))
	for i, q := range questions {
		sess.ResetHistory()
		ans, _ := sess.Answer(ctx, q.Question, llmSource)
		records = append(records, answerRecord{
			ID: q.ID, Question: q.Question, Answer: ans.Answer,
			Context: buildContext(ans.ToolResults, ansCtxChars),
			Gold: q.Gold, Tags: q.Tags, ToolCalls: ans.ToolCalls,
			Steps: ans.Steps, DurationMS: ans.DurationMS, Error: ans.Error,
		})
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s (%.1fs, %d ctx-tools)\n",
			i+1, len(questions), trunc(q.Question, 56), float64(ans.DurationMS)/1000, len(ans.ToolResults))
	}

	out := os.Stdout
	if ansOut != "" {
		f, err := os.Create(ansOut)
		if err != nil {
			return fmt.Errorf("creating output: %w", err)
		}
		defer f.Close()
		out = f
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(records); err != nil {
		return err
	}
	if ansOut != "" {
		fmt.Fprintf(os.Stderr, "wrote %d answers to %s\n", len(records), ansOut)
	}
	return nil
}

// buildContext concatenates the agent's tool outputs into a single retrieved-
// context string, truncated to maxChars (0 = no cap). This is what an external
// faithfulness/recall evaluator scores the answer against.
func buildContext(results []agent.AskToolResult, maxChars int) string {
	var b strings.Builder
	for _, r := range results {
		out := strings.TrimSpace(r.Output)
		if out == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n---\n\n")
		}
		fmt.Fprintf(&b, "[%s]\n%s", r.Name, out)
	}
	s := b.String()
	if maxChars > 0 && len(s) > maxChars {
		s = s[:maxChars] + "\n…[truncated]"
	}
	return s
}

func init() {
	answerCmd.Flags().StringVar(&ansBundle, "bundle", "", "path to an OKF bundle directory (required)")
	answerCmd.Flags().StringVar(&ansDB, "db", "", "override the bundle's DuckDB path")
	answerCmd.Flags().StringVar(&ansQuestions, "questions", "", "questions.jsonl (id/question/gold/tags) (required)")
	answerCmd.Flags().StringVar(&ansTier, "tier", "", "model size tier (small|medium|large|xl|moe)")
	answerCmd.Flags().StringVar(&ansModel, "model", "", "override the LLM with an explicit kronk model source")
	answerCmd.Flags().StringVar(&ansProcessor, "gpu", "", "llama.cpp backend (cpu|cuda|rocm|vulkan)")
	answerCmd.Flags().StringVar(&ansOut, "out", "", "write the JSON array here (default: stdout)")
	answerCmd.Flags().IntVar(&ansLimit, "limit", 0, "answer only the first N questions")
	answerCmd.Flags().IntVar(&ansCtxChars, "context-chars", 12000, "cap retrieved-context length per question (0 = no cap)")
	_ = answerCmd.MarkFlagRequired("bundle")
	_ = answerCmd.MarkFlagRequired("questions")
	benchCmd.AddCommand(answerCmd)
}
