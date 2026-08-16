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
	router := New(map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}}, []ai.StructuredGenerator{fallback}, nil)

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
	router := New(map[string][]ai.StructuredGenerator{"teacher.feedback": {first, second}}, nil, nil)

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
	router := New(map[string][]ai.StructuredGenerator{"other.prompt": {routed}}, []ai.StructuredGenerator{fallback}, nil)

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
	router := New(nil, []ai.StructuredGenerator{first, second}, nil)

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
	router := New(nil, nil, nil)
	_, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err == nil {
		t.Fatal("expected error when neither a route nor a fallback chain is configured")
	}
}

// TestRouterExplicitOverrideDispatchesToNamedProviderOnly pins Phase 4
// Task W item 5's core plumbing requirement: req.ProviderOverride
// bypasses PromptName-based routing entirely and calls exactly the
// named provider, even when a routed chain for that PromptName exists
// and would otherwise have picked a different provider first.
func TestRouterExplicitOverrideDispatchesToNamedProviderOnly(t *testing.T) {
	routed := &stubGenerator{name: "routed"}
	overridden := &stubGenerator{name: "agycli"}
	router := New(
		map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}},
		nil,
		map[string]ai.StructuredGenerator{"routed": routed, "agycli": overridden},
	)

	resp, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback", ProviderOverride: "agycli"})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if resp.Provider != "agycli" {
		t.Errorf("Provider = %q, want agycli", resp.Provider)
	}
	if overridden.calls != 1 {
		t.Errorf("overridden.calls = %d, want 1", overridden.calls)
	}
	if routed.calls != 0 {
		t.Errorf("routed.calls = %d, want 0 (an explicit override must never consult the routed chain)", routed.calls)
	}
}

// TestRouterExplicitOverrideDoesNotFallBackOnFailure pins the brief's
// "an explicit override does not fall back" rule: when the overridden
// provider itself errors, GenerateStructured must surface that error
// directly rather than trying any other configured provider — even one
// sitting right there in the routed chain or fallback list.
func TestRouterExplicitOverrideDoesNotFallBackOnFailure(t *testing.T) {
	failing := &stubGenerator{name: "agycli", err: errors.New("agycli: binary not found")}
	otherRouted := &stubGenerator{name: "routed"}
	otherFallback := &stubGenerator{name: "fallback"}
	router := New(
		map[string][]ai.StructuredGenerator{"teacher.feedback": {otherRouted}},
		[]ai.StructuredGenerator{otherFallback},
		map[string]ai.StructuredGenerator{"agycli": failing, "routed": otherRouted},
	)

	_, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback", ProviderOverride: "agycli"})
	if err == nil {
		t.Fatal("expected the overridden provider's own error, got nil")
	}
	if !strings.Contains(err.Error(), "agycli") || !strings.Contains(err.Error(), "binary not found") {
		t.Errorf("error = %q, want it to name agycli and its underlying error", err.Error())
	}
	if failing.calls != 1 {
		t.Errorf("failing.calls = %d, want 1", failing.calls)
	}
	if otherRouted.calls != 0 || otherFallback.calls != 0 {
		t.Errorf("otherRouted.calls=%d otherFallback.calls=%d, want 0/0 (an explicit override must never fall back)", otherRouted.calls, otherFallback.calls)
	}
}

// TestRouterExplicitOverrideUnknownProviderReturnsError pins "reject
// an unknown/unconfigured provider" at the router's own last-resort
// level (the HTTP handler validates first — see feedback.go — but the
// router must never silently ignore an override it can't satisfy).
func TestRouterExplicitOverrideUnknownProviderReturnsError(t *testing.T) {
	fallback := &stubGenerator{name: "fallback"}
	router := New(nil, []ai.StructuredGenerator{fallback}, map[string]ai.StructuredGenerator{"ollama": fallback})

	_, err := router.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback", ProviderOverride: "not-a-real-provider"})
	if err == nil {
		t.Fatal("expected an error for an unknown provider override")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("error = %v, want it to wrap ErrUnknownProvider", err)
	}
	if fallback.calls != 0 {
		t.Errorf("fallback.calls = %d, want 0 (an unknown override must not fall back to the default chain)", fallback.calls)
	}
}
