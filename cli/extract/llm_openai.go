package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

// OpenAIGenerator is an LLM backend that calls an external OpenAI-compatible
// chat-completions endpoint (a llama.cpp llama-server, vLLM, Ollama, etc.)
// instead of running a model in-process. It mirrors *Generator: a non-nil schema
// is sent as response_format=json_schema so a server that honours it constrains
// the output to valid JSON (llama-server compiles the schema to a GBNF grammar);
// servers that ignore it still work because generateJSON cleans/repairs/retries.
type OpenAIGenerator struct {
	url       string // full chat-completions URL (base + /chat/completions)
	model     string
	apiKey    string
	maxTokens int
	http      *http.Client
	calls     int
}

// NewOpenAIGenerator builds a generator for baseURL (already including the API
// version path, e.g. http://localhost:8080/v1). modelID is the served model name
// (a local llama-server ignores it; any non-empty string works). apiKey may be
// empty for a local server.
func NewOpenAIGenerator(baseURL, modelID, apiKey string, maxTokens int) *OpenAIGenerator {
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if strings.TrimSpace(modelID) == "" {
		modelID = "local-model"
	}
	return &OpenAIGenerator{
		url:       strings.TrimRight(baseURL, "/") + "/chat/completions",
		model:     modelID,
		apiKey:    apiKey,
		maxTokens: maxTokens,
		// No client timeout: extraction calls can be slow on a busy server; the
		// per-call context deadline (attached below) bounds them instead.
		http: &http.Client{},
	}
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

type oaiRequest struct {
	Model          string             `json:"model"`
	Messages       []oaiMessage       `json:"messages"`
	Temperature    float64            `json:"temperature"`
	MaxTokens      int                `json:"max_tokens"`
	ResponseFormat *oaiResponseFormat `json:"response_format,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Generate runs one chat completion against the endpoint. kronk requires the
// context to carry a deadline; one is attached when absent (matching *Generator).
func (h *OpenAIGenerator) Generate(ctx context.Context, system, user string, schema map[string]any) (string, *model.Usage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}

	reqBody := oaiRequest{
		Model: h.model,
		Messages: []oaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.1,
		MaxTokens:   h.maxTokens,
	}
	if schema != nil {
		// OpenAI-standard structured-output shape. The schema is kept
		// structure-only (no enum) by callers, matching the kronk path — see the
		// note in cli/extract/ontology.go.
		reqBody.ResponseFormat = &oaiResponseFormat{
			Type: "json_schema",
			JSONSchema: map[string]any{
				"name":   "okb_extract",
				"schema": schema,
			},
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(h.apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}

	h.calls++
	resp, err := h.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("calling %s: %w", h.url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("endpoint %s returned %s: %.300s", h.url, resp.Status, raw)
	}

	var out oaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("decoding response: %w (body: %.300s)", err, raw)
	}
	if out.Error != nil {
		return "", nil, fmt.Errorf("endpoint error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("endpoint returned no choices")
	}

	usage := &model.Usage{
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		OutputTokens:     out.Usage.CompletionTokens,
		TotalTokens:      out.Usage.TotalTokens,
	}
	return out.Choices[0].Message.Content, usage, nil
}

// GenerateJSON generates and unmarshals into v with the shared retry/repair loop.
func (h *OpenAIGenerator) GenerateJSON(ctx context.Context, system, user string, schema map[string]any, v any) (*model.Usage, error) {
	return generateJSON(ctx, h, system, user, schema, v)
}

// Calls returns the number of completion calls made so far.
func (h *OpenAIGenerator) Calls() int { return h.calls }

// Close is a no-op: there is no local model or persistent connection to release.
func (h *OpenAIGenerator) Close() error { return nil }
