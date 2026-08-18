package agentrun_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// insatiableCaller asks for a tool on every turn it is offered one and
// answers only when offered none — the behaviour of an agent with a lot
// to read, which is what the A2A specialists became the moment they were
// granted tools.
type insatiableCaller struct {
	toolsSeen []int // how many tools were offered on each turn
}

func (c *insatiableCaller) CallWithTools(_ context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	c.toolsSeen = append(c.toolsSeen, len(req.Tools))
	if len(req.Tools) == 0 {
		return ai.ToolResponse{Turn: ai.ToolTurn{Text: "ここまでで分かったことをまとめます。"}}, nil
	}
	return ai.ToolResponse{Turn: ai.ToolTurn{Invocations: []ai.ToolInvocation{
		{ID: "1", Name: "probe_tool", Arguments: json.RawMessage(`{}`)},
	}}}, nil
}

// A run that uses every turn must still produce an answer.
//
// Before this, it failed: the caller got "Task failed" and nothing at
// all, having already paid for every tool call along the way. Granting
// the A2A specialists their tools surfaced it immediately —
// analyse_learner genuinely needs several turns to read a learner's
// history, and hitting the cap threw that work away.
func TestRunningOutOfTurnsAnswersInsteadOfFailing(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("probe_tool", false, "some data"))
	reg.Allow("teacher", "probe_tool")

	caller := &insatiableCaller{}
	runner := agentrun.NewRunner(caller, reg, &fakeRepo{}, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		MaxTurns: 4,
		Messages: []ai.ToolMessage{{Role: "user", Text: "分析して"}},
	})
	if err != nil {
		t.Fatalf("a run that used all its turns failed instead of answering: %v", err)
	}
	if !strings.Contains(out.Text, "まとめます") {
		t.Errorf("no answer text: %q", out.Text)
	}

	// The last turn is offered nothing — that is what forces an answer —
	// and every earlier turn keeps its tools.
	if len(caller.toolsSeen) != 4 {
		t.Fatalf("made %d turns, want 4", len(caller.toolsSeen))
	}
	if last := caller.toolsSeen[3]; last != 0 {
		t.Errorf("the final turn was offered %d tools, want 0 — nothing would force an answer", last)
	}
	for i, n := range caller.toolsSeen[:3] {
		if n == 0 {
			t.Errorf("turn %d was offered no tools; only the last one should be", i+1)
		}
	}

	// What it gathered on the way is still reported, so the widgets built
	// from those results survive a run that went the distance.
	if len(out.ToolCalls) == 0 {
		t.Error("no tool calls reported, so nothing gathered during the run could be rendered")
	}
}
