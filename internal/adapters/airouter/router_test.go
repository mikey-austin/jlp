package airouter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// stubGenerator is a minimal ai.StructuredGenerator test double that
// records how many times it was called and either succeeds (returning
// a canned response naming itself as Provider) or returns err, so
// router tests can assert both which chain member answered AND that
// every attempt along the way actually ran.
type stubGenerator struct {
	name  string
	err   error
	calls int
}

func (s *stubGenerator) GenerateStructured(_ context.Context, _ ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.calls++
	if s.err != nil {
		return ai.StructuredResponse{Provider: s.name}, s.err
	}
	return ai.StructuredResponse{Provider: s.name, JSON: json.RawMessage(`{"ok":true}`)}, nil
}

func TestRouterUsesRoutedChainOnExactPromptNameMatch(t *testing.T) {
	routed := &stubGenerator{name: "routed"}
	fallback := &stubGenerator{name: "fallback"}
	router := New(map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}}, []ai.StructuredGenerator{fallback})

	resp, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
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

func TestRouterFallsThroughToSecondProviderOnFirstError(t *testing.T) {
	first := &stubGenerator{name: "first", err: errors.New("boom")}
	second := &stubGenerator{name: "second"}
	router := New(map[string][]ai.StructuredGenerator{"teacher.feedback": {first, second}}, nil)

	resp, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if resp.Provider != "second" {
		t.Errorf("Provider = %q, want second", resp.Provider)
	}
	// Both attempts must have actually run — this is the "failover
	// pairs visible in ai_requests" invariant the brief calls out: each
	// provider records its own attempt via its own observer, so the
	// router itself must call every chain member up to (and including)
	// the first success.
	if first.calls != 1 {
		t.Errorf("first.calls = %d, want 1", first.calls)
	}
	if second.calls != 1 {
		t.Errorf("second.calls = %d, want 1", second.calls)
	}
}

func TestRouterUsesFallbackWhenPromptNameUnrouted(t *testing.T) {
	routed := &stubGenerator{name: "routed"}
	fallback := &stubGenerator{name: "fallback"}
	router := New(map[string][]ai.StructuredGenerator{"other.prompt": {routed}}, []ai.StructuredGenerator{fallback})

	resp, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if resp.Provider != "fallback" {
		t.Errorf("Provider = %q, want fallback", resp.Provider)
	}
	if routed.calls != 0 {
		t.Errorf("routed.calls = %d, want 0 (prompt name doesn't match its route)", routed.calls)
	}
	if fallback.calls != 1 {
		t.Errorf("fallback.calls = %d, want 1", fallback.calls)
	}
}

func TestRouterAllProvidersFailReturnsJoinedErrorNamingProviders(t *testing.T) {
	first := &stubGenerator{name: "first", err: errors.New("boom1")}
	second := &stubGenerator{name: "second", err: errors.New("boom2")}
	router := New(nil, []ai.StructuredGenerator{first, second})

	_, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err == nil {
		t.Fatal("expected error when every provider in the chain fails")
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "boom1") {
		t.Errorf("error = %q, want it to name %q and its underlying error", err.Error(), "first")
	}
	if !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "boom2") {
		t.Errorf("error = %q, want it to name %q and its underlying error", err.Error(), "second")
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("calls: first=%d second=%d, want 1/1 (every chain member tried)", first.calls, second.calls)
	}
}

func TestRouterEmptyChainReturnsError(t *testing.T) {
	router := New(nil, nil)
	_, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err == nil {
		t.Fatal("expected error when neither a route nor a fallback chain is configured")
	}
}
