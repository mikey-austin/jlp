package airouter

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// stubToolCaller mirrors stubGenerator (router_test.go) for
// ai.ToolCaller: records call count, either succeeds naming itself as
// Provider or returns err.
type stubToolCaller struct {
	name  string
	err   error
	calls int
}

func (s *stubToolCaller) CallWithTools(_ context.Context, _ ai.ToolRequest) (ai.ToolResponse, error) {
	s.calls++
	if s.err != nil {
		return ai.ToolResponse{Provider: s.name}, s.err
	}
	return ai.ToolResponse{Provider: s.name, Turn: ai.ToolTurn{Text: "ok"}}, nil
}

func TestToolRouterUsesRoutedChainOnExactPromptNameMatch(t *testing.T) {
	routed := &stubToolCaller{name: "routed"}
	fallback := &stubToolCaller{name: "fallback"}
	router := NewToolCaller(map[string][]ai.ToolCaller{"teacher.agentic": {routed}}, []ai.ToolCaller{fallback})

	resp, err := router.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic"})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Provider != "routed" {
		t.Errorf("Provider = %q, want routed", resp.Provider)
	}
	if routed.calls != 1 {
		t.Errorf("routed.calls = %d, want 1", routed.calls)
	}
	if fallback.calls != 0 {
		t.Errorf("fallback.calls = %d, want 0 (routed chain matched, fallback must not run)", fallback.calls)
	}
}

func TestToolRouterFallsBackWhenNoRouteMatches(t *testing.T) {
	fallback := &stubToolCaller{name: "fallback"}
	router := NewToolCaller(map[string][]ai.ToolCaller{"other.prompt": {&stubToolCaller{name: "unused"}}}, []ai.ToolCaller{fallback})

	resp, err := router.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic"})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Provider != "fallback" {
		t.Errorf("Provider = %q, want fallback", resp.Provider)
	}
}

func TestToolRouterFallsThroughToSecondProviderOnFirstError(t *testing.T) {
	first := &stubToolCaller{name: "first", err: errors.New("boom")}
	second := &stubToolCaller{name: "second"}
	router := NewToolCaller(map[string][]ai.ToolCaller{"teacher.agentic": {first, second}}, nil)

	resp, err := router.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic"})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Provider != "second" {
		t.Errorf("Provider = %q, want second", resp.Provider)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("calls: first=%d second=%d, want both 1 (both attempts recorded)", first.calls, second.calls)
	}
}

func TestToolRouterAllProvidersFailReturnsJoinedError(t *testing.T) {
	first := &stubToolCaller{name: "first", err: errors.New("boom1")}
	second := &stubToolCaller{name: "second", err: errors.New("boom2")}
	router := NewToolCaller(map[string][]ai.ToolCaller{"teacher.agentic": {first, second}}, nil)

	_, err := router.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic"})
	if err == nil {
		t.Fatal("CallWithTools() err = nil, want an error when every provider fails")
	}
}

func TestToolRouterEmptyChainReturnsClearError(t *testing.T) {
	router := NewToolCaller(nil, nil)
	_, err := router.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic"})
	if err == nil {
		t.Fatal("CallWithTools() err = nil, want an error for an empty chain")
	}
}
