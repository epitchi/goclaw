package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
)

// EmbeddingProvider generates vector embeddings for text.
type EmbeddingProvider interface {
	// Name returns the provider identifier (e.g., "openai", "voyage").
	Name() string

	// Model returns the model used for embeddings.
	Model() string

	// Embed generates embeddings for a batch of texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenAIEmbeddingProvider uses the OpenAI-compatible embedding API.
// Works with OpenAI, OpenRouter, and any compatible endpoint.
type OpenAIEmbeddingProvider struct {
	name       string
	model      string
	apiKey     string
	apiURL     string
	dimensions int // optional: truncate output to this many dimensions (0 = use model default)
}

// NewOpenAIEmbeddingProvider creates a provider for OpenAI-compatible embedding APIs.
func NewOpenAIEmbeddingProvider(name, apiKey, apiURL, model string) *OpenAIEmbeddingProvider {
	if apiURL == "" {
		apiURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}

	return &OpenAIEmbeddingProvider{
		name:   name,
		model:  model,
		apiKey: apiKey,
		apiURL: apiURL,
	}
}

// WithDimensions sets the output dimensions for models that support dimension truncation.
func (p *OpenAIEmbeddingProvider) WithDimensions(d int) *OpenAIEmbeddingProvider {
	p.dimensions = d
	return p
}

func (p *OpenAIEmbeddingProvider) Name() string  { return p.name }
func (p *OpenAIEmbeddingProvider) Model() string { return p.model }

func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := map[string]interface{}{
		"input": texts,
		"model": p.model,
	}
	if p.dimensions > 0 {
		reqBody["dimensions"] = p.dimensions
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/embeddings", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	embeddings := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		embeddings[i] = d.Embedding
	}

	return embeddings, nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value between -1 and 1 (1 = identical).
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}

	return dot / denom
}
