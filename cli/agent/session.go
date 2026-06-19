package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"charm.land/fantasy"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
)

// Session is a fully wired agent over a bundle: database, models, tools, and
// the chat runner. Close releases all of them.
type Session struct {
	bundle   *Bundle
	db       *store.DB
	embedder *Embedder
	provider fantasy.Provider
	runner   *Runner

	Info string // status-bar label (models in use)
	Warn string // optional warning banner (e.g. lexical-only)
}

// Logf is an optional progress sink for setup (model downloads, etc.).
type Logf func(format string, args ...any)

// NewSession opens the bundle database, loads the LLM and embedding models via
// kronk, wires the tools, and builds the chat runner. Heavy work (downloads,
// model loads) happens here and reports progress through log; run it before
// starting the TUI.
func NewSession(ctx context.Context, bundle *Bundle, llmSource, embedSource string, log Logf) (*Session, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	s := &Session{bundle: bundle}

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

	// Language model (downloads on first use).
	log("Loading language model %s … (first run downloads llama.cpp + the model)", llmSource)
	provider, err := NewProvider()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("creating kronk provider: %w", err)
	}
	s.provider = provider
	model, err := provider.LanguageModel(ctx, llmSource)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("loading language model %q: %w", llmSource, err)
	}

	// Embedding model (best-effort; degrade to lexical-only on failure).
	vectorOK := false
	dim := bundle.Config.EmbeddingDim
	log("Loading embedding model %s …", embedSource)
	embedder, eerr := NewEmbedder(ctx, embedSource, dim, applog.FmtLogger)
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
		OnToolResult: func(n string) { fmt.Fprintf(os.Stderr, "[tool: %s done]\n", n) },
		OnDone:       func(err error) { runErr = err },
	})
	fmt.Println()
	return runErr
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
