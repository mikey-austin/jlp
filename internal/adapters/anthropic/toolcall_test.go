package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// cannedToolCallTurn is a Messages API response naming ONE real tool
// call (not the forced emit_result tool GenerateStructured uses) —
// the shape CallWithTools must map to a ToolTurn with one Invocation
// and no Text (a pure tool-call turn).
const cannedToolCallTurn = `{"id":"msg_tc1","model":"claude-sonnet-5","role":"assistant","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"toolu_1","name":"get_learning_priorities","input":{"limit":5}}],
 "usage":{"input_tokens":120,"output_tokens":30}}`

// cannedFinalProseTurn is the model's follow-up turn after receiving a
// tool_result — plain text, no further tool calls, which CallWithTools
// must map to an empty Invocations slice (the conversation is done).
const cannedFinalProseTurn = `{"id":"msg_tc2","model":"claude-sonnet-5","role":"assistant","stop_reason":"end_turn",
 "content":[{"type":"text","text":"Your top priority is i-adjective-past."}],
 "usage":{"input_tokens":150,"output_tokens":12}}`

func TestCallWithToolsSendsToolDefsAndMapsToolUseResponse(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolCallTurn)
	defer srv.Close()

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL})
	req := ai.ToolRequest{
		PromptName: "teacher.agentic",
		System:     "You are an agentic Japanese writing teacher.",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
		},
		Tools: []ai.ToolDef{
			{
				Name:        "get_learning_priorities",
				Description: "Returns the learner's top priorities.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}`),
			},
		},
		Agent: "teacher",
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}

	// --- Wire-level: the tool defs must be on the request, and
	// tool_choice must NOT be forced (unlike GenerateStructured). ---
	if _, forced := captured.Body["tool_choice"]; forced {
		t.Errorf("tool_choice = %#v, want absent — CallWithTools lets the model decide", captured.Body["tool_choice"])
	}
	tools, _ := captured.Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one tool", captured.Body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "get_learning_priorities" {
		t.Errorf("tools[0].name = %v, want get_learning_priorities", tool["name"])
	}
	if tool["description"] != "Returns the learner's top priorities." {
		t.Errorf("tools[0].description = %v, want the tool def's description", tool["description"])
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("tools[0].input_schema = %#v, want the ai.ToolDef's schema verbatim", tool["input_schema"])
	}

	messages, _ := captured.Body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message (the single user turn)", captured.Body["messages"])
	}

	// --- Response mapping: one tool_use block -> one Invocation, no text. ---
	if resp.Turn.Text != "" {
		t.Errorf("Turn.Text = %q, want empty for a pure tool-call turn", resp.Turn.Text)
	}
	if len(resp.Turn.Invocations) != 1 {
		t.Fatalf("len(Invocations) = %d, want 1", len(resp.Turn.Invocations))
	}
	inv := resp.Turn.Invocations[0]
	if inv.ID != "toolu_1" || inv.Name != "get_learning_priorities" {
		t.Errorf("Invocations[0] = %+v, want ID=toolu_1 Name=get_learning_priorities", inv)
	}
	var args map[string]any
	if err := json.Unmarshal(inv.Arguments, &args); err != nil {
		t.Fatalf("Invocations[0].Arguments is not valid JSON: %v", err)
	}
	if args["limit"] != float64(5) {
		t.Errorf("Invocations[0].Arguments = %v, want limit=5", args)
	}
	if resp.Provider != provider || resp.Model != "claude-sonnet-5" {
		t.Errorf("Provider/Model = %s/%s, want %s/claude-sonnet-5", resp.Provider, resp.Model, provider)
	}
	if resp.InputTokens != 120 || resp.OutputTokens != 30 {
		t.Errorf("tokens = %d/%d, want 120/30", resp.InputTokens, resp.OutputTokens)
	}
}

func TestCallWithToolsSendsToolResultsAndMapsFinalProse(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedFinalProseTurn)
	defer srv.Close()

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL})
	req := ai.ToolRequest{
		System: "You are an agentic Japanese writing teacher.",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
			{Role: "assistant", Invocations: []ai.ToolInvocation{
				{ID: "toolu_1", Name: "get_learning_priorities", Arguments: json.RawMessage(`{"limit":5}`)},
			}},
			{Role: "tool", Results: []ai.ToolResult{
				{ID: "toolu_1", Content: `[{"subject":"i-adjective-past","score":5}]`},
			}},
		},
		Agent: "teacher",
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}

	messages, _ := captured.Body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want 3 (user, assistant tool_use, user tool_result)", captured.Body["messages"])
	}
	assistantMsg, _ := messages[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", assistantMsg["role"])
	}
	assistantContent, _ := assistantMsg["content"].([]any)
	assistantBlock, _ := assistantContent[0].(map[string]any)
	if assistantBlock["type"] != "tool_use" || assistantBlock["id"] != "toolu_1" {
		t.Errorf("messages[1].content[0] = %#v, want a tool_use block echoing toolu_1", assistantBlock)
	}

	toolResultMsg, _ := messages[2].(map[string]any)
	if toolResultMsg["role"] != "user" {
		t.Fatalf("messages[2].role = %v, want user (Anthropic carries tool_result in a user turn)", toolResultMsg["role"])
	}
	toolResultContent, _ := toolResultMsg["content"].([]any)
	toolResultBlock, _ := toolResultContent[0].(map[string]any)
	if toolResultBlock["type"] != "tool_result" || toolResultBlock["tool_use_id"] != "toolu_1" {
		t.Errorf("messages[2].content[0] = %#v, want a tool_result block for toolu_1", toolResultBlock)
	}

	if resp.Turn.Text != "Your top priority is i-adjective-past." {
		t.Errorf("Turn.Text = %q, want the final prose", resp.Turn.Text)
	}
	if len(resp.Turn.Invocations) != 0 {
		t.Errorf("len(Invocations) = %d, want 0 — an empty slice means the model is done", len(resp.Turn.Invocations))
	}
}

func TestCallWithToolsPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(cannedAPIErrorResponse))
	}))
	defer srv.Close()

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL})
	_, err := gen.CallWithTools(context.Background(), ai.ToolRequest{System: "s", Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("CallWithTools() err = nil, want an error for a non-2xx response")
	}
}
