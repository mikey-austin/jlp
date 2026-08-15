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

// New returns a value implementing BOTH ai.StructuredGenerator and
// ai.ToolCaller against a local Ollama server at cfg.URL, always
// requesting cfg.Model — same "concrete type, not either interface
// alone" reasoning as adapters/anthropic.New's own doc comment.
func New(cfg config.Ollama) *generator {
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
// ToolCalls (assistant turns) and Content-as-a-result (tool turns) are
// only ever populated by CallWithTools — GenerateStructured never sets
// them.
type chatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type chatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []chatMessage   `json:"messages"`
	Format   json.RawMessage `json:"format,omitempty"`
	// Tools is only set by CallWithTools — GenerateStructured constrains
	// output via Format instead, never both on the same request.
	Tools []ollamaTool `json:"tools,omitempty"`
}

// ollamaTool/ollamaFunction mirror one entry of /api/chat's "tools"
// array: Ollama's tool-calling contract is function-call shaped
// (type "function", a name/description/parameters trio), the same
// convention OpenAI-compatible chat APIs use — Parameters carries an
// ai.ToolDef.Schema's JSON Schema verbatim.
type ollamaTool struct {
	Type     string         `json:"type"`
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ollamaToolCall mirrors one entry of a response message's tool_calls
// array. Unlike Anthropic's tool_use blocks, Ollama's tool_calls carry
// no per-call ID for the model to correlate a later tool_result
// against — the local server has no cross-request state to correlate
// against anyway, since the whole conversation is replayed on every
// call. CallWithTools invents its own IDs (index-based, "call_N") to
// satisfy ai.ToolInvocation.ID/ai.ToolResult.ID's correlation contract
// for the CALLER's bookkeeping (internal/tools.Registry.Invoke, the
// agent-run loop); those IDs are never sent back to Ollama, since its
// tool-result messages need no id field to line up — only message
// order matters (see toOllamaMessages' "tool" case below).
type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// chatResponse mirrors Ollama's /api/chat response body. Model is the
// model that actually served the request — reported back rather than
// assumed to always equal the generator's configured model, so a
// server-side alias or default-model fallback is still recorded
// accurately.
type chatResponse struct {
	Message struct {
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
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

// CallWithTools implements ai.ToolCaller against the same /api/chat
// endpoint GenerateStructured uses, but with req.Tools offered as real
// tools (the "tools" field) instead of Format constraining a single
// JSON object — the model decides, turn by turn, whether to call one.
func (g *generator) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	start := time.Now()

	messages := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, chatMessage{Role: "system", Content: req.System})
	}
	messages = append(messages, toOllamaMessages(req.Messages)...)

	body := chatRequest{
		Model:    g.model,
		Stream:   false,
		Messages: messages,
		Tools:    toOllamaTools(req.Tools),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := g.client.Do(httpReq)
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: chat: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: read response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: chat: status %d: %s", httpResp.StatusCode, raw)
	}

	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ai.ToolResponse{Provider: provider, Model: g.model},
			fmt.Errorf("ollama: unmarshal response: %w", err)
	}

	model := resp.Model
	if model == "" {
		model = g.model
	}

	turn := ai.ToolTurn{Text: resp.Message.Content}
	for i, tc := range resp.Message.ToolCalls {
		turn.Invocations = append(turn.Invocations, ai.ToolInvocation{
			ID:        fmt.Sprintf("call_%d", i),
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	return ai.ToolResponse{
		Turn:         turn,
		Provider:     provider,
		Model:        model,
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
		Latency:      time.Since(start),
	}, nil
}

// toOllamaTools translates every ai.ToolDef into an Ollama function
// tool, Parameters carrying the def's Schema byte-for-byte.
func toOllamaTools(defs []ai.ToolDef) []ollamaTool {
	tools := make([]ollamaTool, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, ollamaTool{
			Type: "function",
			Function: ollamaFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Schema,
			},
		})
	}
	return tools
}

// toOllamaMessages replays a ToolRequest's conversation into Ollama's
// own message shape: a "user" ToolMessage becomes a plain user turn;
// an "assistant" ToolMessage becomes an assistant turn carrying its
// Text plus one tool_calls entry per Invocation (Ollama's tool_calls
// need no per-call id — see ollamaToolCall's doc comment); a "tool"
// ToolMessage becomes one "tool"-role message per Result, content
// being that Result's own output — Ollama correlates a tool result to
// the immediately preceding tool call by message order, not by id.
func toOllamaMessages(msgs []ai.ToolMessage) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			msg := chatMessage{Role: "assistant", Content: m.Text}
			for _, inv := range m.Invocations {
				var tc ollamaToolCall
				tc.Function.Name = inv.Name
				tc.Function.Arguments = inv.Arguments
				msg.ToolCalls = append(msg.ToolCalls, tc)
			}
			out = append(out, msg)
		case "tool":
			for _, r := range m.Results {
				out = append(out, chatMessage{Role: "tool", Content: r.Content})
			}
		default: // "user"
			out = append(out, chatMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}
