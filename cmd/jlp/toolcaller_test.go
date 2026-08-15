package main

import (
	"context"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func TestBuildToolCallerDefaultFakeProviderAnswers(t *testing.T) {
	cfg := baseCfg()
	caller, err := buildToolCaller(cfg, &memRepo{})
	if err != nil {
		t.Fatalf("buildToolCaller: %v", err)
	}
	resp, err := caller.CallWithTools(context.Background(), ai.ToolRequest{PromptName: "teacher.agentic", Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Provider != "fake" {
		t.Errorf("Provider = %q, want fake", resp.Provider)
	}
}

func TestBuildToolCallerRoutesNamedPromptToOllamaFallsThroughOthers(t *testing.T) {
	srv := newOllamaTestServer(t)
	defer srv.Close()

	cfg := baseCfg()
	cfg.AI.Routes = "teacher.agentic=ollama"
	cfg.AI.Ollama = config.Ollama{URL: srv.URL, Model: "test-ollama-model"}

	caller, err := buildToolCaller(cfg, &memRepo{})
	if err != nil {
		t.Fatalf("buildToolCaller: %v", err)
	}

	req := ai.ToolRequest{Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}}}
	req.PromptName = "teacher.agentic"
	routed, err := caller.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools(routed): %v", err)
	}
	if routed.Provider != "ollama" {
		t.Errorf("routed Provider = %q, want ollama", routed.Provider)
	}

	req.PromptName = "nonexistent.prompt"
	unrouted, err := caller.CallWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CallWithTools(unrouted): %v", err)
	}
	if unrouted.Provider != "fake" {
		t.Errorf("unrouted Provider = %q, want fake (falls through to APP_AI_PROVIDER default)", unrouted.Provider)
	}
}

// TestBuildToolCallerErrorsWhenRouteNamesNonToolCallingProvider pins
// this task's provider-narrowing rule: claudecli/codexcli answer
// buildAIGenerator (ai.StructuredGenerator) but neither implements
// ai.ToolCaller, so naming either in a route meant for tool calling
// must be a boot error, not a silent construction of something that
// can't actually satisfy the interface.
func TestBuildToolCallerErrorsWhenRouteNamesNonToolCallingProvider(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.agentic=claudecli"

	_, err := buildToolCaller(cfg, &memRepo{})
	if err == nil {
		t.Fatal("buildToolCaller() err = nil, want an error — claudecli doesn't implement ai.ToolCaller")
	}
}

// TestBuildToolCallerIgnoresRoutesForNonToolCallingPromptNames pins a
// regression found in review: APP_AI_ROUTES is ONE shared config
// string covering both buildAIGenerator (structured generation) and
// buildToolCaller (tool calling) routes. A route naming claudecli/
// codexcli for a prompt buildToolCaller has no business validating
// (e.g. "teacher.feedback", never a tool-calling prompt name) must NOT
// fail boot just because claudecli doesn't implement ai.ToolCaller —
// that route is buildAIGenerator's concern, already resolved there.
// Before this fix, ANY deployment routing ANY prompt to claudecli/
// codexcli (a documented, supported Task 12 configuration) would fail
// to boot the moment main.go started calling buildToolCaller
// unconditionally (Task 2), even with the agentic teacher disabled.
func TestBuildToolCallerIgnoresRoutesForNonToolCallingPromptNames(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=claudecli"

	caller, err := buildToolCaller(cfg, &memRepo{})
	if err != nil {
		t.Fatalf("buildToolCaller() err = %v, want nil — \"teacher.feedback\" is not a tool-calling prompt name, this route is irrelevant here", err)
	}

	// The unrelated route must not have displaced the default fake
	// provider for an actual tool-calling prompt name.
	resp, err := caller.CallWithTools(context.Background(), ai.ToolRequest{
		PromptName: "teacher.agentic",
		Messages:   []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Provider != "fake" {
		t.Errorf("Provider = %q, want fake (falls through to APP_AI_PROVIDER default, unaffected by the unrelated teacher.feedback route)", resp.Provider)
	}
}

func TestBuildToolCallerErrorsWhenDefaultProviderNotToolCalling(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Provider = "codexcli"

	_, err := buildToolCaller(cfg, &memRepo{})
	if err == nil {
		t.Fatal("buildToolCaller() err = nil, want an error — codexcli doesn't implement ai.ToolCaller")
	}
}

func TestBuildToolCallerErrorsOnMalformedRoutes(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "not-a-valid-route-string==="

	_, err := buildToolCaller(cfg, &memRepo{})
	if err == nil {
		t.Fatal("buildToolCaller() err = nil, want an error for malformed APP_AI_ROUTES")
	}
}
