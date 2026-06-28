package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"charm.land/fantasy"
	kronkprov "charm.land/fantasy/providers/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/sandwich-labs/open-knowledge-bundler/cli/store"
)

// Session is a fully wired agent over a bundle: database, models, tools, and
// the chat runner. Close releases all of them.
type Session struct {
	bundle   *Bundle
	db       *store.DB
	embedder *Embedder
	provider fantasy.Provider
	runner   *Runner

	Info     string // status-bar label (models in use)
	Warn     string // optional warning banner (e.g. lexical-only)
	VectorOK bool   // whether the vector channel is live (vs lexical-only)
}

// Logf is an optional progress sink for setup (model downloads, etc.).
type Logf func(format string, args ...any)

// NewSession opens the bundle database, loads the LLM and embedding models via
// kronk, wires the tools, and builds the chat runner. Heavy work (downloads,
// model loads) happens here and reports progress through log; run it before
// starting the TUI.
// quietStdout, when true, routes kronk's own progress logging (model/library
// downloads) to stderr so stdout stays clean for machine-readable output
// (--json). The setup progress sink (log) is the caller's to direct.
func NewSession(ctx context.Context, bundle *Bundle, llmSource, embedSource string, inf Inference, quietStdout bool, log Logf) (*Session, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	s := &Session{bundle: bundle}

	// kronk's bundled FmtLogger writes to stdout; redirect to stderr when the
	// caller needs a clean stdout.
	var provLogger kronkprov.Logger
	embedLogger := applog.FmtLogger
	if quietStdout {
		provLogger = stderrKronkLogger
		embedLogger = stderrKronkLogger
	}

	// Database.
	db, err := store.Open(bundle.DBPath)
	if err != nil {
		return nil, fmt.Errorf("opening bundle database: %w", err)
	}
	s.db = db
	if err := db.LoadExtensions(); err != nil {
		s.Close()
		return nil, fmt.Errorf("loading DuckDB extensions: %w", err)
	}

	// Language model. For kronk this downloads/loads on first use; for an
	// external provider modelID is the name sent to the endpoint (the model is
	// already loaded server-side).
	modelID := llmSource
	if inf.IsExternal() {
		modelID = inf.ModelID
		if modelID == "" {
			modelID = "local-model"
		}
		log("Using external LLM (%s) model %q at %s", inf.Provider, modelID, inf.BaseURL())
	} else {
		log("Loading language model %s … (first run downloads llama.cpp + the model)", llmSource)
	}
	provider, err := NewProvider(provLogger, inf)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("creating LLM provider: %w", err)
	}
	s.provider = provider
	model, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("loading language model %q: %w", modelID, err)
	}

	// Embedding model (best-effort; degrade to lexical-only on failure).
	vectorOK := false
	dim := bundle.Config.EmbeddingDim
	log("Loading embedding model %s …", embedSource)
	embedder, eerr := NewEmbedder(ctx, embedSource, dim, embedLogger)
	switch {
	case eerr != nil:
		s.Warn = fmt.Sprintf("embeddings unavailable (%v) — hybrid_search runs lexical-only", eerr)
		log("WARNING: %s", s.Warn)
	default:
		s.embedder = embedder
		// Probe: confirm the produced dimension matches the bundle's index.
		pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		vec, perr := embedder.Embed(pctx, "probe")
		cancel()
		switch {
		case perr != nil:
			s.Warn = fmt.Sprintf("embedding probe failed (%v) — hybrid_search runs lexical-only", perr)
			log("WARNING: %s", s.Warn)
		case dim > 0 && len(vec) != dim:
			s.Warn = fmt.Sprintf("embedding dim %d != bundle index dim %d — hybrid_search runs lexical-only", len(vec), dim)
			log("WARNING: %s", s.Warn)
		default:
			vectorOK = true
		}
	}

	s.VectorOK = vectorOK

	// Tools.
	ts := &toolset{db: db, bundle: bundle, embedder: s.embedder, vectorOK: vectorOK}
	schema, err := ts.schemaText()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("reading schema: %w", err)
	}

	systemPrompt := BuildSystemPrompt(bundle, schema, vectorOK)
	s.runner = NewRunner(model, systemPrompt, ts.Tools())

	embedLabel := "lexical-only"
	if vectorOK {
		embedLabel = embedSource
	}
	s.Info = fmt.Sprintf("%s · model: %s · embed: %s", bundle.Name(), llmSource, embedLabel)

	return s, nil
}

// Run starts the chat TUI and blocks until the user quits.
func (s *Session) Run(ctx context.Context) error {
	return RunTUI(ctx, s.runner, s.Info, s.Warn)
}

// RunOnce answers a single prompt non-interactively, streaming the answer to
// stdout and tool activity to stderr. Useful for scripting and smoke tests.
func (s *Session) RunOnce(ctx context.Context, prompt string) error {
	if s.Warn != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", s.Warn)
	}
	tctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	var runErr error
	s.runner.Stream(tctx, prompt, StreamHandler{
		OnText:       func(t string) { fmt.Print(t) },
		OnToolCall:   func(n, in string) { fmt.Fprintf(os.Stderr, "\n[tool: %s %s]\n", n, oneLine(in, 200)) },
		OnToolResult: func(n, _ string) { fmt.Fprintf(os.Stderr, "[tool: %s done]\n", n) },
		OnDone:       func(err error) { runErr = err },
	})
	fmt.Println()
	return runErr
}

// AskResult is the machine-readable result of a single non-interactive turn,
// emitted by RunOnceJSON. It carries the final answer, the tool-call trace, and
// token/timing metrics so an external eval harness can grade without scraping
// streamed prose.
type AskResult struct {
	Question    string          `json:"question"`
	Answer      string          `json:"answer"`
	ToolCalls   []AskToolCall   `json:"tool_calls"`
	ToolResults []AskToolResult `json:"tool_results,omitempty"`
	Steps       int             `json:"steps"`
	Usage       AskUsage        `json:"usage"`
	DurationMS  int64           `json:"duration_ms"`
	VectorOK    bool            `json:"vector_ok"`
	Model       string          `json:"model"`
	Bundle      string          `json:"bundle"`
	Warning     string          `json:"warning,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// AskToolCall records one tool invocation (name + raw JSON input) in call order.
type AskToolCall struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// AskToolResult records one tool's output text, in result order. This is the
// retrieved context an external eval (e.g. GraphRAG-Bench) needs to score
// retrieval/faithfulness. Only emitted when result capture is enabled.
type AskToolResult struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

// AskUsage is the token accounting for a turn.
type AskUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// Answer runs a single prompt in-process and returns the structured result
// (final answer, tool-call trace, steps, token usage, timing). It does not
// print anything, so an eval harness can load the session once and loop over
// many questions without reloading the model. Any agent error is captured in
// AskResult.Error and also returned.
func (s *Session) Answer(ctx context.Context, prompt, model string) (AskResult, error) {
	tctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	out := AskResult{
		Question: prompt,
		VectorOK: s.VectorOK,
		Model:    model,
		Bundle:   s.bundle.Name(),
		Warning:  s.Warn,
	}

	start := time.Now()
	res, err := s.runner.Stream(tctx, prompt, StreamHandler{
		OnToolCall: func(n, in string) {
			out.ToolCalls = append(out.ToolCalls, AskToolCall{Name: n, Input: in})
		},
		OnToolResult: func(n, output string) {
			out.ToolResults = append(out.ToolResults, AskToolResult{Name: n, Output: output})
		},
	})
	out.DurationMS = time.Since(start).Milliseconds()

	if err != nil {
		out.Error = err.Error()
	}
	if res != nil {
		out.Answer = res.Response.Content.Text()
		out.Steps = len(res.Steps)
		out.Usage = AskUsage{
			InputTokens:  res.TotalUsage.InputTokens,
			OutputTokens: res.TotalUsage.OutputTokens,
			TotalTokens:  res.TotalUsage.TotalTokens,
		}
	}
	return out, err
}

// ResetHistory clears the conversation history so the next Answer/Stream starts
// a fresh turn. The eval driver calls this between independent questions.
func (s *Session) ResetHistory() { s.runner.history = nil }

// RunOnceJSON answers a single prompt and writes one JSON object (an AskResult)
// to stdout. Nothing else goes to stdout, so the output is safe to pipe into a
// grader. Errors are reported inside the JSON (and returned) rather than printed
// loose. The model/llmSource label is passed in by the caller.
func (s *Session) RunOnceJSON(ctx context.Context, prompt, model string) error {
	out, err := s.Answer(ctx, prompt, model)

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(out); encErr != nil {
		return encErr
	}
	return err
}

// Close releases the database, embedding model, and LLM provider.
func (s *Session) Close() {
	if s.embedder != nil {
		_ = s.embedder.Close()
	}
	if s.provider != nil {
		if closer, ok := s.provider.(interface{ Close(context.Context) error }); ok {
			_ = closer.Close(context.Background())
		}
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}
