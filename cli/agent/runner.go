package agent

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	kronkprov "charm.land/fantasy/providers/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

// contextWindow is the token context the LLM is loaded with. kronk defaults to
// 8192, which a multi-step tool conversation (system prompt + accumulating tool
// results) overflows on complex questions — producing "context window is full"
// errors and blank answers. Gemma 4 supports far more; 32k gives ample room for
// the agent loop while staying modest on KV-cache memory.
const contextWindow = 32768

// NewProvider creates the kronk LLM provider. Download/install progress is
// printed via the provider logger (so do this before the TUI starts). Pass nil
// to use the default stdout FmtLogger.
func NewProvider(logger kronkprov.Logger) (fantasy.Provider, error) {
	if logger == nil {
		logger = kronkprov.FmtLogger
	}
	// Enlarge the context window; other Config fields stay nil so kronk applies
	// its defaults for them.
	var cfg model.Config
	model.WithContextWindow(contextWindow)(&cfg)
	return kronkprov.New(
		kronkprov.WithName("cbi"),
		kronkprov.WithLogger(logger),
		kronkprov.WithModelConfig(cfg),
	)
}

// Runner owns the fantasy agent and the running conversation history.
type Runner struct {
	agent   fantasy.Agent
	history []fantasy.Message
}

// NewRunner assembles the agent from a language model, system prompt, and tools.
func NewRunner(model fantasy.LanguageModel, systemPrompt string, tools []fantasy.AgentTool) *Runner {
	a := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(tools...),
		fantasy.WithTemperature(0.3),
		fantasy.WithMaxOutputTokens(2048),
		// Bound multi-step tool loops so a confused model can't spin forever,
		// but allow enough retries to recover from a bad query.
		fantasy.WithStopConditions(fantasy.StepCountIs(20)),
	)
	return &Runner{agent: a}
}

// StreamHandler receives streaming events for one turn.
type StreamHandler struct {
	OnText       func(text string)
	OnReasoning  func(text string)
	OnToolCall   func(name, input string)
	OnToolResult func(name, output string)
	OnDone       func(err error)
}

// Stream runs one user turn, invoking the handler callbacks as events arrive,
// and appends the exchange to the conversation history on success. It also
// returns the underlying agent result (final response, steps, token usage) so
// non-interactive callers can capture structured metrics.
func (r *Runner) Stream(ctx context.Context, prompt string, h StreamHandler) (*fantasy.AgentResult, error) {
	call := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: r.history,
		OnTextDelta: func(_, text string) error {
			if h.OnText != nil {
				h.OnText(text)
			}
			return nil
		},
		OnReasoningDelta: func(_, text string) error {
			if h.OnReasoning != nil {
				h.OnReasoning(text)
			}
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			if h.OnToolCall != nil {
				h.OnToolCall(tc.ToolName, tc.Input)
			}
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			if h.OnToolResult != nil {
				out := ""
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					out = txt.Text
				}
				h.OnToolResult(tr.ToolName, out)
			}
			return nil
		},
	}

	res, err := r.agent.Stream(ctx, call)
	if err == nil && res != nil {
		r.history = append(r.history, fantasy.NewUserMessage(prompt))
		for _, s := range res.Steps {
			r.history = append(r.history, s.Messages...)
		}
	}
	if h.OnDone != nil {
		h.OnDone(err)
	}
	return res, err
}

// BuildSystemPrompt composes the agent's system prompt from the bundle's skill
// notes, a live schema snapshot, and operating instructions for the in-process
// tools.
func BuildSystemPrompt(b *Bundle, schema string, vectorOK bool) string {
	var p strings.Builder

	fmt.Fprintf(&p, "You are cbi-agent, a data-retrieval assistant answering questions about the \"%s\" knowledge graph. ", b.Name())
	p.WriteString("It is an OKF (Open Knowledge Format) bundle: browsable markdown concept documents paired with a DuckDB graph database.\n\n")

	p.WriteString("You answer by calling tools, never by guessing. Available tools:\n")
	p.WriteString("- schema: list node/relationship types and how they connect. Call this first when unsure.\n")
	p.WriteString("- sql_query: run read-only DuckDB SQL or SQL/PGQ for precise facts, counts, filters, and graph traversals.\n")
	if vectorOK {
		p.WriteString("- hybrid_search: vector + lexical search for fuzzy/conceptual lookups when you lack an exact id.\n")
	} else {
		p.WriteString("- hybrid_search: lexical (keyword) search for fuzzy lookups (vector embeddings are unavailable this session).\n")
	}
	p.WriteString("- list_docs / search_docs / read_doc: discover and read the markdown concept documents for narrative context and cross-links.\n\n")

	p.WriteString("Guidance: prefer sql_query for anything precise; use hybrid_search or search_docs to find ids/concepts first when needed. ")
	p.WriteString("Always ground answers in tool output and cite node ids. If a query errors, call schema, fix it, and retry. Keep answers concise and factual.\n")
	p.WriteString("Writing SQL: use the exact property keys and edge directions from the schema below (do not guess field names like 'name'). ")
	p.WriteString("ALWAYS parenthesise JSON comparisons: `(properties->>'key') = 'value'` — the unparenthesised form raises a cast error. ")
	p.WriteString("Prefer plain SQL joins on Edges_Base/Nodes_Base for relationships and multi-hop traversals (self-join Edges_Base for each hop). ")
	p.WriteString("Do NOT use recursive CTEs or inline `{property: value}` filters with GRAPH_TABLE — duckpgq does not support them. ")
	p.WriteString("After at most 2 failed query attempts, simplify to a basic Edges_Base/Nodes_Base join rather than retrying variations.\n")
	p.WriteString("CRITICAL — never use your own background knowledge about the subject matter. Every fact in your answer must come from a tool result in THIS conversation. ")
	p.WriteString("If the tools do not return the information, say you could not find it in the graph. Do not fill in, complete, or correct lists from memory. Always end with a plain-language answer grounded only in tool output.\n\n")

	p.WriteString("## Current schema\n\n")
	p.WriteString(schema)

	if strings.TrimSpace(b.Skill) != "" {
		p.WriteString("\n\n## Bundle notes (from SKILL.md)\n")
		p.WriteString("These notes describe the bundle. Where they mention `cbi query`/`cbi graph` CLI commands, use your sql_query/hybrid_search tools instead.\n\n")
		p.WriteString(b.Skill)
	}

	return p.String()
}
