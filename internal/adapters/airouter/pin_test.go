package airouter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// namedGen answers with its own name so a test can assert WHICH provider
// handled a request, not merely that one did.
type namedGen struct {
	name string
	err  error
}

func (g namedGen) GenerateStructured(context.Context, ai.StructuredRequest) (ai.StructuredResponse, error) {
	if g.err != nil {
		return ai.StructuredResponse{Provider: g.name}, g.err
	}
	return ai.StructuredResponse{Provider: g.name, JSON: []byte(`{"from":"` + g.name + `"}`)}, nil
}

func pinTo(name string) PinnedProvider {
	return func(string) string { return name }
}

func TestPinBeatsTheConfiguredRoute(t *testing.T) {
	routed, pinned := namedGen{name: "routed"}, namedGen{name: "pinned"}
	r := New(
		map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}},
		[]ai.StructuredGenerator{routed},
		map[string]ai.StructuredGenerator{"routed": routed, "pinned": pinned},
		WithPinnedProvider(pinTo("pinned")),
	)

	resp, err := r.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.Provider != "pinned" {
		t.Errorf("answered by %q, want the pinned provider — a pin that loses to the configured route is not a choice", resp.Provider)
	}
}

// A per-request override is more specific than a stored pin and must
// win, or the workspace dropdown would silently stop working the moment
// anything was pinned.
func TestPerRequestOverrideBeatsThePin(t *testing.T) {
	pinned, asked := namedGen{name: "pinned"}, namedGen{name: "asked"}
	r := New(nil, nil,
		map[string]ai.StructuredGenerator{"pinned": pinned, "asked": asked},
		WithPinnedProvider(pinTo("pinned")),
	)

	resp, err := r.GenerateStructured(context.Background(), ai.StructuredRequest{
		PromptName: "teacher.feedback", ProviderOverride: "asked",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.Provider != "asked" {
		t.Errorf("answered by %q, want the explicitly requested provider", resp.Provider)
	}
}

// The whole point of pinning is defeated if a failure quietly produces
// an answer from somewhere else.
func TestPinnedFailureDoesNotFallBack(t *testing.T) {
	boom := namedGen{name: "pinned", err: errors.New("boom")}
	other := namedGen{name: "other"}
	r := New(
		map[string][]ai.StructuredGenerator{"teacher.feedback": {other}},
		[]ai.StructuredGenerator{other},
		map[string]ai.StructuredGenerator{"pinned": boom, "other": other},
		WithPinnedProvider(pinTo("pinned")),
	)

	_, err := r.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err == nil {
		t.Fatal("a failing pinned provider was silently answered by another provider")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("error %q does not name the pinned provider", err)
	}
}

// A pin naming a provider this deployment no longer builds must not
// break the prompt: it falls through to the configured route, which is
// what the learner had before pinning.
func TestPinToAnUnbuiltProviderFallsThroughToTheRoute(t *testing.T) {
	routed := namedGen{name: "routed"}
	r := New(
		map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}},
		[]ai.StructuredGenerator{routed},
		map[string]ai.StructuredGenerator{"routed": routed},
		WithPinnedProvider(pinTo("a-provider-that-was-removed")),
	)

	resp, err := r.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("a stale pin broke the prompt entirely: %v", err)
	}
	if resp.Provider != "routed" {
		t.Errorf("answered by %q, want the configured route", resp.Provider)
	}
}

// No pin at all must leave routing exactly as it was.
func TestNoPinLeavesRoutingUnchanged(t *testing.T) {
	routed := namedGen{name: "routed"}
	for _, pin := range []PinnedProvider{nil, func(string) string { return "" }} {
		r := New(
			map[string][]ai.StructuredGenerator{"teacher.feedback": {routed}},
			nil,
			map[string]ai.StructuredGenerator{"routed": routed},
			WithPinnedProvider(pin),
		)
		resp, err := r.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
		if err != nil || resp.Provider != "routed" {
			t.Errorf("resp=%q err=%v, want the configured route", resp.Provider, err)
		}
	}
}
