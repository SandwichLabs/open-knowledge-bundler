// Package extract implements okb's fully-local, in-process graph extraction
// pipeline: it turns a prose corpus into a resolved knowledge graph (nodes +
// edges) using a local LLM (via kronk/llama.cpp) for ontology bootstrap, entity
// and relation extraction, gleaning, entity resolution, and relation
// normalization. No external LLM server is required.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	kk "github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
)

// genContextWindow is the context window the extraction model is loaded with.
// kronk's autotune defaults to an 8192 cap; the bootstrap prompt (a multi-chunk
// corpus sample) and large chunks need more, so we set it explicitly (honored
// over the autotune cap). 32k is plenty for extraction without a huge KV cache.
const genContextWindow = 32768

// Generator wraps a kronk generation model for grammar-constrained JSON
// generation. It mirrors agent.Embedder's setup but loads an instruct model and
// calls Chat. The two can co-reside (generation + embedder) on a host with
// enough memory — the agent already runs both together.
type Generator struct {
	krn       *kk.Kronk
	maxTokens int
	calls     int // total Chat calls (for reporting)
}

// NewGenerator installs llama.cpp (if needed), downloads/loads the generation
// model, and verifies it is not an embedding model. The processor backend is
// taken from the KRONK_PROCESSOR environment (set by the caller from config).
func NewGenerator(ctx context.Context, source string, maxTokens int, log applog.Logger) (*Generator, error) {
	if log == nil {
		log = applog.DiscardLogger
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	lbs, err := libs.New()
	if err != nil {
		return nil, fmt.Errorf("libs: %w", err)
	}
	if _, err := lbs.Download(ctx, log); err != nil {
		return nil, fmt.Errorf("installing llama.cpp: %w", err)
	}

	mdls, err := models.New()
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	mp, err := mdls.Download(ctx, log, source)
	if err != nil {
		return nil, fmt.Errorf("downloading model %q: %w", source, err)
	}

	if err := kk.Init(); err != nil {
		return nil, fmt.Errorf("kronk init: %w", err)
	}
	// Set an explicit context window. kronk's autotune otherwise caps context at
	// 8192, which the ontology-bootstrap prompt (a multi-chunk corpus sample)
	// overflows. 32k comfortably fits the bootstrap sample plus per-chunk
	// extraction, without the KV-cache cost of the model's full 128k/256k.
	krn, err := kk.New(
		model.WithModelFiles(mp.ModelFiles),
		model.WithAutoTune(true),
		model.WithContextWindow(genContextWindow),
	)
	if err != nil {
		return nil, fmt.Errorf("loading model: %w", err)
	}
	if krn.ModelInfo().IsEmbedModel {
		_ = krn.Unload(context.Background())
		return nil, fmt.Errorf("model %q is an embedding model, not a generation model", source)
	}

	return &Generator{krn: krn, maxTokens: maxTokens}, nil
}

// Generate runs one chat completion. When schema is non-nil it is supplied as an
// OpenAI-shaped response_format, which kronk compiles to a GBNF grammar so the
// output is guaranteed-valid JSON matching the schema (this is what makes the
// type/relation enums un-violable at the token level). kronk requires the
// context to carry a deadline; one is attached when absent.
func (g *Generator) Generate(ctx context.Context, system, user string, schema map[string]any) (string, *model.Usage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}

	d := model.D{
		"messages": []model.D{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     0.1,
		"max_tokens":      g.maxTokens,
		"enable_thinking": false,
	}
	if schema != nil {
		d["response_format"] = model.D{
			"type":        "json_schema",
			"json_schema": model.D{"schema": schema},
		}
	}

	g.calls++
	resp, err := g.krn.Chat(ctx, d)
	if err != nil {
		return "", nil, err
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return "", resp.Usage, fmt.Errorf("empty chat response")
	}
	return resp.Choices[0].Message.Content, resp.Usage, nil
}

// GenerateJSON runs Generate and unmarshals the JSON into v. The grammar makes
// the output valid in the common case, but kronk's schema→GBNF generation is not
// perfectly reliable on complex schemas, so this also cleans the text (strips
// fences, slices to the outermost object) and retries generation a couple of
// times before failing — extraction must not silently drop a chunk on a single
// malformed response.
func (g *Generator) GenerateJSON(ctx context.Context, system, user string, schema map[string]any, v any) (*model.Usage, error) {
	const attempts = 3
	var lastErr error
	var lastUsage *model.Usage
	for i := 0; i < attempts; i++ {
		out, usage, err := g.Generate(ctx, system, user, schema)
		lastUsage = usage
		if err != nil {
			lastErr = err
			continue
		}
		cleaned := cleanJSON(out)
		if err := json.Unmarshal([]byte(cleaned), v); err == nil {
			return usage, nil
		} else if err2 := json.Unmarshal([]byte(repairJSON(cleaned)), v); err2 == nil {
			return usage, nil // recovered a truncated/under-closed response
		} else {
			lastErr = fmt.Errorf("parsing model JSON: %w (output: %.200q)", err, out)
			continue
		}
	}
	return lastUsage, lastErr
}

// cleanJSON strips markdown code fences and any prose around the JSON value,
// returning the substring from the first opening brace to the last closing brace.
func cleanJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if a, b := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); a != -1 && b > a {
		return s[a : b+1]
	}
	return s
}

// repairJSON attempts to recover a truncated or under-closed JSON object by
// dropping a dangling trailing comma / partial token and appending the closing
// brackets implied by the open-bracket stack (tracking string/escape state so
// braces inside strings are ignored). It cannot fix a structurally wrong bracket,
// but it recovers the common "model stopped early" case so a dense chunk isn't
// lost entirely (weakpoint #5).
func repairJSON(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		}
	}
	out := strings.TrimRight(strings.TrimSpace(s), ",")
	if inStr {
		out += `"`
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out += string(stack[i])
	}
	return out
}

// Calls returns the number of Chat calls made so far.
func (g *Generator) Calls() int { return g.calls }

// Close unloads the generation model.
func (g *Generator) Close() error {
	if g == nil || g.krn == nil {
		return nil
	}
	return g.krn.Unload(context.Background())
}
