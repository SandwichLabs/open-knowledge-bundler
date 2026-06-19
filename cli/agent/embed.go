package agent

import (
	"context"
	"fmt"
	"time"

	kk "github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
)

// Embedder produces embedding vectors locally via kronk/llama.cpp, reduced to
// the bundle's embedding dimension (EmbeddingGemma is Matryoshka, so the
// `dimensions` request guarantees a vector that matches the HNSW index width).
type Embedder struct {
	krn *kk.Kronk
	dim int
}

// NewEmbedder installs llama.cpp (if needed), downloads/loads the embedding
// model, and verifies it is an embedding model. The processor backend is taken
// from the KRONK_PROCESSOR environment (set by the caller from config).
func NewEmbedder(ctx context.Context, source string, dim int, log applog.Logger) (*Embedder, error) {
	if log == nil {
		log = applog.DiscardLogger
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
		return nil, fmt.Errorf("downloading embedding model %q: %w", source, err)
	}

	if err := kk.Init(); err != nil {
		return nil, fmt.Errorf("kronk init: %w", err)
	}
	krn, err := kk.New(model.WithModelFiles(mp.ModelFiles), model.WithAutoTune(true))
	if err != nil {
		return nil, fmt.Errorf("loading embedding model: %w", err)
	}
	if !krn.ModelInfo().IsEmbedModel {
		_ = krn.Unload(context.Background())
		return nil, fmt.Errorf("model %q is not an embedding model", source)
	}

	return &Embedder{krn: krn, dim: dim}, nil
}

// Embed returns an embedding vector for text. kronk requires a context with a
// deadline, so one is attached when the caller's context has none.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	d := model.D{"input": text}
	if e.dim > 0 {
		d["dimensions"] = e.dim // Matryoshka reduction to match the bundle index
	}

	resp, err := e.krn.Embeddings(ctx, d)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	return resp.Data[0].Embedding, nil
}

// Dim returns the configured target dimension.
func (e *Embedder) Dim() int { return e.dim }

// Close unloads the embedding model.
func (e *Embedder) Close() error {
	if e == nil || e.krn == nil {
		return nil
	}
	return e.krn.Unload(context.Background())
}
