// ToolCaller is a SECOND ai capability, alongside StructuredGenerator in
// ai.go — deliberately not merged into it (PRD §22 wants capability-
// specific interfaces: one call shape per kind of thing an agent asks a
// model to do). Where StructuredGenerator is a single forced-tool-use
// round trip that always returns schema-valid JSON, ToolCaller is a
// multi-turn conversation in which the model may call zero or more
// REAL tools (internal/tools.Registry's tools) before answering in
// prose — the shape Phase 4's agentic teacher (Task 2) and everything
// built on internal/tools needs.
package ai

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// ToolDef describes one tool a model may call: Name/Description are
// shown to the model, Schema is the JSON Schema (draft 2020-12, same
// convention as StructuredRequest.Schema) constraining the shape of a
// ToolInvocation.Arguments the model may produce for it.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolInvocation is one call the model made: ID is the provider's own
// identifier for this specific call (Anthropic's tool_use.id, Ollama's
// equivalent), used to correlate the eventual ToolResult back to it.
// Arguments is the model's raw JSON input for the tool — untrusted,
// schema-shaped but not schema-validated by this package.
type ToolInvocation struct {
	ID        string
	Name      string
	Arguments json.RawMessage
	// ProviderState is opaque data the provider attached to THIS call
	// and requires back verbatim when the call is replayed in a later
	// turn's history. It is never inspected, logged as meaningful, or
	// constructed by anything but the adapter that produced it.
	//
	// Gemini 3 is why it exists: it signs every functionCall part with a
	// thoughtSignature and rejects the next request outright if the
	// signature does not come back ("Function call is missing a
	// thought_signature"). Providers with no such notion leave it empty.
	ProviderState string
}

// ToolResult is the outcome of running one ToolInvocation: ID echoes
// the invocation's ID so the provider can correlate it back to the
// call that produced it. Content is the tool's own compact,
// model-readable output; IsError marks a refusal or failure so the
// model can see what went wrong and try something else, rather than
// silently receiving no information.
type ToolResult struct {
	ID      string
	Content string
	IsError bool
}

// ToolTurn is one assistant turn of a tool-calling conversation: Text
// is whatever prose the model produced alongside (or instead of) tool
// calls — often empty on a turn that's purely tool calls. An empty
// Invocations slice means the model considers the conversation
// finished; the caller should treat Text as the final answer.
type ToolTurn struct {
	Text        string
	Invocations []ToolInvocation
}

// ToolMessage is one turn of the conversation so far, in the shape
// ToolCaller.CallWithTools needs to replay the whole exchange back to
// the provider on every call (providers are stateless per-request):
// Role is "user" (the human's turn), "assistant" (a prior model turn —
// Text and/or Invocations), or "tool" (the Results of running that
// turn's Invocations, fed back for the model's next turn).
type ToolMessage struct {
	Role        string
	Text        string
	Invocations []ToolInvocation
	Results     []ToolResult
}

// ToolRequest asks a ToolCaller for the next turn of a tool-calling
// conversation: Messages is the conversation so far (see ToolMessage),
// Tools is the full set of tool definitions the model may call this
// turn — internal/tools.Registry.DefsFor(Agent) is the intended
// source, already narrowed to what Agent is allowed to see.
// IdentityID/SessionID/Agent are carried for observability exactly
// like StructuredRequest's — they never reach the model itself.
type ToolRequest struct {
	PromptName, PromptVersion string
	System                    string
	Messages                  []ToolMessage
	Tools                     []ToolDef
	MaxTokens                 int
	IdentityID                learner.IdentityID
	SessionID                 *session.ID
	Agent                     string
}

// ToolResponse is a ToolCaller's answer: the next ToolTurn, plus the
// same provenance/cost/latency metadata StructuredResponse carries.
// RequestID is set by the observability decorator, same convention as
// StructuredResponse.RequestID.
type ToolResponse struct {
	RequestID                 string
	Turn                      ToolTurn
	Provider, Model           string
	InputTokens, OutputTokens int
	Latency                   time.Duration
}

// ToolCaller produces the next turn of a tool-calling conversation.
// Implementations: adapters/anthropic (the Messages API's tools +
// tool_use/tool_result blocks), adapters/ollama (/api/chat's tools
// field), adapters/fakeai (a deterministic scripted two-turn
// sequence), observability's decorator (wraps either, recording every
// turn as an ai_requests row).
type ToolCaller interface {
	CallWithTools(ctx context.Context, req ToolRequest) (ToolResponse, error)
}
