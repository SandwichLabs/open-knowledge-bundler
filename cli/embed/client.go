package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Client calls an OpenAI-compatible embedding endpoint (Ollama, vLLM, etc.).
type Client struct {
	endpoint string
	model    string
	http     *http.Client
}

func NewClient(endpoint, model string) *Client {
	return &Client{
		endpoint: endpoint,
		model:    model,
		http:     &http.Client{},
	}
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// maxEmbedChars is a defensive ceiling on input length. EmbeddingGemma caps
// input at 512 tokens (≈ 2000 chars); a hub node's semantic_text can exceed
// that, and not every endpoint auto-truncates, so we bound it well under the
// limit (~450 tokens) to avoid a hard error at the cost of dropping trailing
// alias text that adds little embedding signal.
const maxEmbedChars = 1800

// Embed sends text to the configured endpoint and returns the embedding vector.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if len(text) > maxEmbedChars {
		text = text[:maxEmbedChars]
	}
	body, err := json.Marshal(embeddingRequest{Model: c.model, Input: text})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding endpoint returned %d", resp.StatusCode)
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding response contained no data")
	}
	return result.Data[0].Embedding, nil
}
