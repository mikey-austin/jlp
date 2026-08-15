package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// memRepo is a minimal in-memory storage.AIRequestRepository — enough
// for observability.NewAIObserver (which buildAIGenerator wraps every
// provider in) to have somewhere to Insert into. What was actually
// recorded isn't these tests' concern (internal/observability already
// covers that); only List is unused but required to satisfy the
// interface.
type memRepo struct {
	mu      sync.Mutex
	records []storage.AIRequestRecord
}

func (r *memRepo) Insert(_ context.Context, rec storage.AIRequestRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

func (r *memRepo) List(_ context.Context, _ learner.IdentityID, _ int) ([]storage.AIRequestRecord, error) {
	return nil, nil
}

// baseCfg is the smallest valid config.Config for buildAIGenerator:
// only the AI section matters to it, but a zero-value Config is a
// legitimate config.Config value to build tests on top of (no
// database/auth wiring needed — buildAIGenerator never touches them).
func baseCfg() config.Config {
	return config.Config{AI: config.AI{Provider: "fake"}}
}

// newOllamaTestServer starts an httptest server implementing enough of
// Ollama's /api/chat to answer buildAIGenerator's real ollama.New
// client — model name comes back distinct from any other provider so
// tests can tell, from resp.Provider/resp.Model alone, that a request
// actually reached THIS provider and not the fake fallback.
func newOllamaTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"{}"},"prompt_eval_count":1,"eval_count":1,"model":"test-ollama-model"}`))
	}))
}

func TestBuildAIGeneratorDefaultFakeProviderAnswers(t *testing.T) {
	cfg := baseCfg()
	gen, err := buildAIGenerator(cfg, &memRepo{})
	if err != nil {
		t.Fatalf("buildAIGenerator: %v", err)
	}
	resp, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if resp.Provider != "fake" {
		t.Errorf("Provider = %q, want fake", resp.Provider)
	}
}

// TestBuildAIGeneratorRoutesNamedPromptToOllamaFallsThroughOthers is
// the glue-logic test the task brief calls for at the cmd/jlp layer:
// APP_AI_ROUTES sends ONE prompt name to ollama (a real ollama.New
// instance hitting an httptest server, not a stub), while every other
// prompt still falls through to the APP_AI_PROVIDER default (fake).
// This exercises buildAIGenerator's full path — the `needed` set
// computation, ollama's routed-to-AND-model-set gate, the
// resolvedRoutes loop, and the defaultGen fallback — in one round
// trip per branch, not just the already well-covered building blocks
// (config.ParseRoutes, airouter, ollama) in isolation.
func TestBuildAIGeneratorRoutesNamedPromptToOllamaFallsThroughOthers(t *testing.T) {
	srv := newOllamaTestServer(t)
	defer srv.Close()

	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=ollama"
	cfg.AI.Ollama = config.Ollama{URL: srv.URL, Model: "test-ollama-model"}

	gen, err := buildAIGenerator(cfg, &memRepo{})
	if err != nil {
		t.Fatalf("buildAIGenerator: %v", err)
	}

	routed, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "teacher.feedback"})
	if err != nil {
		t.Fatalf("GenerateStructured(routed): %v", err)
	}
	if routed.Provider != "ollama" {
		t.Errorf("routed Provider = %q, want ollama (APP_AI_ROUTES=teacher.feedback=ollama)", routed.Provider)
	}

	unrouted, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "drill.exercise"})
	if err != nil {
		t.Fatalf("GenerateStructured(unrouted): %v", err)
	}
	if unrouted.Provider != "fake" {
		t.Errorf("unrouted Provider = %q, want fake (falls through to APP_AI_PROVIDER default, unaffected by the ollama route)", unrouted.Provider)
	}
}

// TestBuildAIGeneratorErrorsWhenRouteNamesOllamaWithoutModel pins the
// brief's "model required only when routed-to" rule: a route naming
// ollama with APP_AI_OLLAMA_MODEL unset must be a boot error, not a
// silently-skipped route (config.validate only catches this when
// ollama is the DEFAULT provider — see internal/config/config_test.go
// — so this branch, reached only through routing, needs its own
// coverage here).
func TestBuildAIGeneratorErrorsWhenRouteNamesOllamaWithoutModel(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=ollama"
	// cfg.AI.Ollama.Model left "" deliberately.

	_, err := buildAIGenerator(cfg, &memRepo{})
	if err == nil {
		t.Fatal("expected a boot error, got nil")
	}
	if !strings.Contains(err.Error(), "ollama") || !strings.Contains(err.Error(), "teacher.feedback") {
		t.Errorf("error = %q, want it to name both the route (teacher.feedback) and the unconstructible provider (ollama)", err.Error())
	}
}

// TestBuildAIGeneratorErrorsWhenRouteNamesCLIProviderNotYetConstructible
// covers the Task 12 seam explicitly: config.ParseRoutes accepts
// claudecli/codexcli as spellable names (see config_test.go), but
// buildAIGenerator has no way to construct one yet — that gap must
// surface as a boot error naming the route, not a panic or a silent
// no-op provider.
func TestBuildAIGeneratorErrorsWhenRouteNamesCLIProviderNotYetConstructible(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=claudecli"

	_, err := buildAIGenerator(cfg, &memRepo{})
	if err == nil {
		t.Fatal("expected a boot error, got nil")
	}
	if !strings.Contains(err.Error(), "claudecli") || !strings.Contains(err.Error(), "teacher.feedback") {
		t.Errorf("error = %q, want it to name both the route (teacher.feedback) and the unconstructible provider (claudecli)", err.Error())
	}
}

// TestBuildAIGeneratorErrorsWhenDefaultProviderNotConstructible covers
// the defaultGen lookup's own boot-error path (distinct from the
// resolvedRoutes loop's): APP_AI_PROVIDER itself naming a provider
// that isn't buildable — here, anthropic with no API key — must fail
// the same way a bad route does. config.validate() already prevents
// this exact case at Load() time (see config_test.go's "anthropic
// without key"), but buildAIGenerator doesn't call validate() itself,
// so this is defense in depth for any other caller/config path.
func TestBuildAIGeneratorErrorsWhenDefaultProviderNotConstructible(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Provider = "anthropic" // no APIKey set

	_, err := buildAIGenerator(cfg, &memRepo{})
	if err == nil {
		t.Fatal("expected a boot error, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error = %q, want it to name anthropic", err.Error())
	}
}

func TestBuildAIGeneratorErrorsOnMalformedRoutes(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "not-a-valid-route-entry-no-equals"

	_, err := buildAIGenerator(cfg, &memRepo{})
	if err == nil {
		t.Fatal("expected an error for a malformed APP_AI_ROUTES value, got nil")
	}
}

func TestWarnIfUnpricedSkipsKnownAndEmptyModels(t *testing.T) {
	pricing := aiPricing()
	// None of these should panic; the interesting behavior (the actual
	// slog.Warn call) is exercised implicitly by every buildAIGenerator
	// test above that constructs anthropic/ollama with models present in
	// aiPricing() (no warning) or absent from it (warning, but slog
	// output isn't asserted here — this only pins that the guard itself
	// doesn't panic or error on either edge).
	warnIfUnpriced(pricing, "anthropic", "")
	warnIfUnpriced(pricing, "anthropic", "claude-sonnet-5")
	warnIfUnpriced(pricing, "ollama", "some-model-nobody-priced")
}
