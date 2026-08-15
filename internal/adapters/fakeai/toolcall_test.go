package fakeai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// TestCallWithToolsFirstTurnInvokesGetLearningPriorities pins the task
// brief's exact script: with no tool result yet in the conversation,
// the first turn must call get_learning_priorities and produce no
// prose — the model "deciding" to look something up before answering.
func TestCallWithToolsFirstTurnInvokesGetLearningPriorities(t *testing.T) {
	gen := New()
	req := ai.ToolRequest{
		System: "You are an agentic Japanese writing teacher.",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
		},
		Tools: []ai.ToolDef{{Name: "get_learning_priorities"}},
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}
	if resp.Turn.Text != "" {
		t.Errorf("Turn.Text = %q, want empty on the first turn", resp.Turn.Text)
	}
	if len(resp.Turn.Invocations) != 1 {
		t.Fatalf("len(Invocations) = %d, want 1", len(resp.Turn.Invocations))
	}
	if resp.Turn.Invocations[0].Name != "get_learning_priorities" {
		t.Errorf("Invocations[0].Name = %q, want get_learning_priorities", resp.Turn.Invocations[0].Name)
	}
	if resp.Provider != provider || resp.Model != model {
		t.Errorf("Provider/Model = %s/%s, want %s/%s", resp.Provider, resp.Model, provider, model)
	}
}

// TestCallWithToolsSecondTurnAnswersWithTheToolResult pins the second
// half of the script: once the conversation carries the tool result
// (a "tool" role message), CallWithTools must answer in prose naming
// the priority the tool returned, with no further tool calls — a
// complete, drivable two-turn loop offline.
func TestCallWithToolsSecondTurnAnswersWithTheToolResult(t *testing.T) {
	gen := New()
	req := ai.ToolRequest{
		System: "You are an agentic Japanese writing teacher.",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "What should I focus on next?"},
			{Role: "assistant", Invocations: []ai.ToolInvocation{
				{ID: "fake-call-1", Name: "get_learning_priorities", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "tool", Results: []ai.ToolResult{
				{ID: "fake-call-1", Content: `[{"subject_type":"concept","subject":"i-adjective-past","score":5,"reason":"recurring"}]`},
			}},
		},
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}
	if len(resp.Turn.Invocations) != 0 {
		t.Fatalf("len(Invocations) = %d, want 0 — the model is done after the second turn", len(resp.Turn.Invocations))
	}
	if !strings.Contains(resp.Turn.Text, "i-adjective-past") {
		t.Errorf("Turn.Text = %q, want it to mention the returned priority %q", resp.Turn.Text, "i-adjective-past")
	}
}

func TestCallWithToolsSecondTurnFallsBackWhenResultIsUnparsable(t *testing.T) {
	gen := New()
	req := ai.ToolRequest{
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "hi"},
			{Role: "tool", Results: []ai.ToolResult{{ID: "x", Content: "not json"}}},
		},
	}

	resp, err := gen.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools returned error: %v", err)
	}
	if resp.Turn.Text == "" {
		t.Fatal("Turn.Text is empty, want a fallback answer even when the tool result can't be parsed")
	}
}
