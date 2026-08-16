package ollama

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// cannedToolCallResponse is an Ollama /api/chat response naming one
// tool call — the shape CallWithTools must map to a ToolTurn with one
// Invocation.
const cannedToolCallResponse = `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_learning_priorities","arguments":{"limit":5}}}]},
 "prompt_eval_count":80,"eval_count":20,"model":"qwen3:4b"}`

// cannedToolFinalResponse is the model's follow-up turn after
// receiving a tool result — plain content, no tool_calls.
const cannedToolFinalResponse = `{"message":{"role":"assistant","content":"Focus on i-adjective-past."},
 "prompt_eval_count":100,"eval_count":10,"model":"qwen3:4b"}`

func TestCallWithToolsSendsToolDefsAndMapsToolCallResponse(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolCallResponse)
	defer srv.Close()

	gen := New(config.Ollama{URL: srv.URL, Model: "qwen3:4b"}, nil)
	req := ai.ToolRequest{
		System: "You are an agentic Japanese writing teacher.",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
		},
		Tools: []ai.ToolDef{
			{Name: "get_learning_priorities", Description: "top priorities", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}

	if captured.Path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", captured.Path)
	}
	tools, _ := captured.Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one tool", captured.Body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tools[0].type = %v, want function", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "get_learning_priorities" {
		t.Errorf("tools[0].function.name = %v, want get_learning_priorities", fn["name"])
	}
	if _, hasFormat := captured.Body["format"]; hasFormat {
		t.Errorf("format = %#v, want absent — CallWithTools uses tools, not Format", captured.Body["format"])
	}

	if len(resp.Turn.Invocations) != 1 {
		t.Fatalf("len(Invocations) = %d, want 1", len(resp.Turn.Invocations))
	}
	if resp.Turn.Invocations[0].Name != "get_learning_priorities" {
		t.Errorf("Invocations[0].Name = %q, want get_learning_priorities", resp.Turn.Invocations[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.Turn.Invocations[0].Arguments, &args); err != nil {
		t.Fatalf("Invocations[0].Arguments not valid JSON: %v", err)
	}
	if args["limit"] != float64(5) {
		t.Errorf("Invocations[0].Arguments = %v, want limit=5", args)
	}
	if resp.InputTokens != 80 || resp.OutputTokens != 20 {
		t.Errorf("tokens = %d/%d, want 80/20", resp.InputTokens, resp.OutputTokens)
	}
}

func TestCallWithToolsSendsToolResultsAsToolRoleMessages(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolFinalResponse)
	defer srv.Close()

	gen := New(config.Ollama{URL: srv.URL, Model: "qwen3:4b"}, nil)
	req := ai.ToolRequest{
		System: "sys",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
			{Role: "assistant", Invocations: []ai.ToolInvocation{
				{ID: "call_0", Name: "get_learning_priorities", Arguments: json.RawMessage(`{"limit":5}`)},
			}},
			{Role: "tool", Results: []ai.ToolResult{
				{ID: "call_0", Content: `[{"subject":"i-adjective-past"}]`},
			}},
		},
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}

	messages, _ := captured.Body["messages"].([]any)
	// system + user + assistant(tool_calls) + tool = 4
	if len(messages) != 4 {
		t.Fatalf("messages = %#v, want 4", captured.Body["messages"])
	}
	assistantMsg, _ := messages[2].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("messages[2].role = %v, want assistant", assistantMsg["role"])
	}
	toolCalls, _ := assistantMsg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("messages[2].tool_calls = %#v, want one entry", assistantMsg["tool_calls"])
	}
	toolMsg, _ := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["content"] != `[{"subject":"i-adjective-past"}]` {
		t.Errorf("messages[3] = %#v, want role=tool with the result content", toolMsg)
	}

	if resp.Turn.Text != "Focus on i-adjective-past." {
		t.Errorf("Turn.Text = %q, want the final prose", resp.Turn.Text)
	}
	if len(resp.Turn.Invocations) != 0 {
		t.Errorf("len(Invocations) = %d, want 0", len(resp.Turn.Invocations))
	}
}
