// Package ollama implements ai.StructuredGenerator against a local
// Ollama server's /api/chat endpoint (stream disabled), using Ollama's
// "format" field — a raw JSON Schema object — to constrain the model's
// output the way adapters/anthropic uses forced tool use. Unlike
// Anthropic's tool-call arguments (already structured), Ollama returns
// the constrained JSON as a plain *string* inside message.content, so
// this adapter does not itself validate that string against the
// schema: a model can still ignore "format" (smaller local models
// sometimes do), and passing the raw bytes through lets the caller's
// own schema validation — the agent layer's job, not the adapter's —
// surface that failure with full context.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const provider = "ollama"

type generator struct {
	url    string
	model  string
	client *http.Client
}

// New returns an ai.StructuredGenerator backed by a local Ollama
// server at cfg.URL, always requesting cfg.Model.
func New(cfg config.Ollama) ai.StructuredGenerator {
	return &generator{
		url:    cfg.URL,
		model:  cfg.Model,
		client: &http.Client{},
	}
}

// chatMessage/chatRequest mirror Ollama's /api/chat request body.
// Format carries req.Schema verbatim — Ollama decodes it as a JSON
// Schema object constraining the model's output, the same raw-bytes
// pass-through adapters/anthropic uses for its tool's input_schema.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []chatMessage   `json:"messages"`
	Format   json.RawMessage `json:"format,omitempty"`
}

// chatResponse mirrors Ollama's /api/chat response body. Model is the
// model that actually served the request — reported back rather than
// assumed to always equal the generator's configured model, so a
// server-side alias or default-model fallback is still recorded
// accurately.
type chatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Model           string `json:"model"`
}

func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	body := chatRequest{
		Model:  g.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Format: req.Schema,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := g.client.Do(httpReq)
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: chat: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: chat: status %d: %s", httpResp.StatusCode, raw)
	}

	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: unmarshal response: %w", err)
	}

	model := resp.Model
	if model == "" {
		model = g.model
	}

	return ai.StructuredResponse{
		JSON:         json.RawMessage(resp.Message.Content),
		Provider:     provider,
		Model:        model,
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
		Latency:      time.Since(start),
	}, nil
}
