package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
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
	gen, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
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

	gen, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
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

	// "nonexistent.prompt" deliberately isn't a real prompt name (see
	// knownPromptNames in ai.go) — this only needs to be a PromptName
	// with no matching APP_AI_ROUTES entry, and using an actual prompt
	// name here (drill.exercise was tried previously, but that was
	// never a real prompt name either — see the boot-time warning this
	// finding adds) would risk being mistaken for one.
	unrouted, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: "nonexistent.prompt"})
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

	_, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
	if err == nil {
		t.Fatal("expected a boot error, got nil")
	}
	if !strings.Contains(err.Error(), "ollama") || !strings.Contains(err.Error(), "teacher.feedback") {
		t.Errorf("error = %q, want it to name both the route (teacher.feedback) and the unconstructible provider (ollama)", err.Error())
	}
}

// TestBuildAIGeneratorRoutesNamedPromptToCLIProvidersConstructWithoutError
// covers the Task 12 seam explicitly: config.ParseRoutes accepts
// claudecli/codexcli as spellable names (see config_test.go), and as
// of this task buildAIGenerator can now construct both unconditionally
// (internal/adapters/clicmd.NewClaude/NewCodex never error at
// construction — see that package's doc comment) — routing to either
// must succeed at boot even with no Bin configured (cfg.AI.ClaudeCLI/
// CodexCLI left zero-value here). A per-call failure is still expected
// (and asserted below) since Bin="" can never resolve to a real
// executable, but that must surface from GenerateStructured, not
// buildAIGenerator.
func TestBuildAIGeneratorRoutesNamedPromptToCLIProvidersConstructWithoutError(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=claudecli;drill.exercise=codexcli"

	gen, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
	if err != nil {
		t.Fatalf("buildAIGenerator: %v (claudecli/codexcli must be constructible without a binary configured, Task 12)", err)
	}

	for _, promptName := range []string{"teacher.feedback", "drill.exercise"} {
		_, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{PromptName: promptName})
		if err == nil {
			t.Errorf("GenerateStructured(%q): expected a per-call error with no CLI binary configured, got nil", promptName)
		}
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

	_, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
	if err == nil {
		t.Fatal("expected a boot error, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Errorf("error = %q, want it to name anthropic", err.Error())
	}
}

// TestGeminiIsDormantWithoutAKey: the provider must not exist at all
// until APP_AI_GEMINI_APIKEY is set, so every deployment that doesn't
// use it boots exactly as it did before this adapter existed. A route
// naming it is then a boot error, which is the loud failure an operator
// who set the route but forgot the key deserves.
func TestGeminiIsDormantWithoutAKey(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "teacher.feedback=gemini"

	_, available, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
	if err == nil {
		t.Fatal("expected a boot error for a gemini route with no API key")
	}
	if slices.Contains(available, "gemini") {
		t.Error("gemini was offered as an available provider with no API key configured")
	}
}

// TestGeminiIsConstructedWithAKeyForBothCapabilities is the one that
// would have caught a structured-generation-only adapter: cmd/jlp
// builds a ToolCaller from APP_AI_PROVIDER too and exits(1) when it
// can't, so APP_AI_PROVIDER=gemini has to satisfy BOTH builders or the
// deployment this provider exists for never starts.
func TestGeminiIsConstructedWithAKeyForBothCapabilities(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Provider = "gemini"
	cfg.AI.Gemini = config.Gemini{APIKey: "test-key", Model: "gemini-3-flash-preview"}

	_, available, defaultProvider, err := buildAIGenerator(cfg, &memRepo{}, nil)
	if err != nil {
		t.Fatalf("buildAIGenerator: %v", err)
	}
	if !slices.Contains(available, "gemini") {
		t.Errorf("available = %v, want it to include gemini", available)
	}
	if defaultProvider != "gemini" {
		t.Errorf("defaultProvider = %q, want gemini", defaultProvider)
	}

	if _, err := buildToolCaller(cfg, &memRepo{}, nil); err != nil {
		t.Fatalf("buildToolCaller: %v — APP_AI_PROVIDER=gemini would exit(1) at boot", err)
	}
}

// TestGeminiModelsArePriced: an unpriced model records every call at $0
// on /ai. Gemini is the first provider in this table whose cost is
// neither zero nor negligible, so a missing entry would be a silently
// wrong bill rather than a cosmetic gap.
func TestGeminiModelsArePriced(t *testing.T) {
	p, ok := aiPricing()["gemini-3-flash-preview"]
	if !ok {
		t.Fatal("gemini-3-flash-preview has no pricing entry — every call would record $0")
	}
	if p.InPerMTok <= 0 || p.OutPerMTok <= 0 {
		t.Errorf("pricing = %+v, want the published paid-tier rates", p)
	}
}

func TestBuildAIGeneratorErrorsOnMalformedRoutes(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "not-a-valid-route-entry-no-equals"

	_, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil)
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

// TestUnknownPromptNamesFlagsNamesOutsideTheKnownList pins Finding 2's
// boot-time guard: a route naming a prompt not in knownPromptNames
// (teacher.feedback/drill.generate/drill.evaluate) must be flagged so
// a typo — like the README's now-fixed "drill.exercise", which was
// never a real prompt name (see internal/agent/drill/drill.go's
// generatePromptName/evaluatePromptName) — doesn't silently route
// nowhere. unknownPromptNames is asserted directly (rather than
// swapping slog's handler to capture warnForUnknownPromptNames'
// output) precisely so this test doesn't need to know anything about
// log formatting — see that function's doc comment for why the check
// is split out.
func TestUnknownPromptNamesFlagsNamesOutsideTheKnownList(t *testing.T) {
	allKnown := map[string][]string{
		"teacher.feedback": {"fake"},
		"drill.generate":   {"fake"},
		"drill.evaluate":   {"fake"},
	}
	if got := unknownPromptNames(allKnown); len(got) != 0 {
		t.Errorf("unknownPromptNames(all known) = %v, want empty", got)
	}

	withTypo := map[string][]string{
		"teacher.feedback": {"fake"},
		"drill.exercise":   {"fake"}, // the wrong name Finding 2 is about
	}
	got := unknownPromptNames(withTypo)
	if len(got) != 1 || got[0] != "drill.exercise" {
		t.Errorf("unknownPromptNames(with typo) = %v, want exactly [drill.exercise]", got)
	}
}

// TestUnknownPromptNamesDoesNotFlagTeacherAgentic pins a minor found in
// review: "teacher.agentic" — the one legitimate tool-calling route
// name (buildToolCaller's knownToolPromptNames) — must not trigger a
// spurious "unknown prompt name" boot warning just because it's not a
// GenerateStructured prompt. It's listed in knownPromptNames purely to
// suppress that false positive (see that slice's own doc comment) —
// it's never actually sent to GenerateStructured.
func TestUnknownPromptNamesDoesNotFlagTeacherAgentic(t *testing.T) {
	routed := map[string][]string{"teacher.agentic": {"anthropic"}}
	if got := unknownPromptNames(routed); len(got) != 0 {
		t.Errorf("unknownPromptNames(teacher.agentic routed) = %v, want empty — this is a real tool-calling route, not a typo", got)
	}
}

// TestA2APromptNamesAreNeitherWarnedAboutNorSilentlyDropped pins
// whole-branch review I-4, in both of its halves. Every A2A skill runs
// through agentrun.Runner with PromptName = "a2a.<skill>" and airouter's
// tool router routes on exactly that field, so those are legitimate
// APP_AI_ROUTES keys. Until this fix they appeared in NEITHER
// knownPromptNames (so warnForUnknownPromptNames called a correct
// config a typo at boot) NOR knownToolPromptNames (so buildToolCaller
// skipped the route entirely and every A2A request went to
// APP_AI_PROVIDER regardless). That is the failure Task 2's bb3878d fix
// closed, re-created from the other side.
//
// The second half is asserted BEHAVIOURALLY rather than by reading the
// slice: routing an a2a prompt name at a provider that is not an
// ai.ToolCaller must now be a boot ERROR. A skipped route produces no
// error at all, so this half fails against the old code for the right
// reason — the route is being resolved, not ignored.
//
// Driven off a2a.PromptNames() rather than a literal list so a skill
// added later is covered here automatically.
func TestA2APromptNamesAreNeitherWarnedAboutNorSilentlyDropped(t *testing.T) {
	names := a2a.PromptNames()
	if len(names) == 0 {
		t.Fatal("a2a.PromptNames() is empty — this test would be vacuous")
	}

	for _, name := range names {
		routed := map[string][]string{name: {"anthropic"}}
		if got := unknownPromptNames(routed); len(got) != 0 {
			t.Errorf("unknownPromptNames(%s routed) = %v, want empty — this is a real A2A route, not a typo", name, got)
		}

		cfg := baseCfg()
		// claudecli is a valid buildAIGenerator provider but is NOT an
		// ai.ToolCaller, so a tool-calling route naming it can only be
		// resolved (→ error) or skipped (→ nil). Nil means the route was
		// dropped.
		cfg.AI.Routes = name + "=claudecli"
		if _, err := buildToolCaller(cfg, &memRepo{}, nil); err == nil {
			t.Errorf("buildToolCaller(%s=claudecli) = nil error — the route was silently ignored instead of resolved", name)
		}
	}
}

// TestBuildAIGeneratorSucceedsDespiteUnknownRoutedPromptName confirms
// the boot-time guard is warning-only, symmetric with warnIfUnpriced
// above: an APP_AI_ROUTES entry naming a prompt outside
// knownPromptNames must NOT fail buildAIGenerator — the provider it
// names (fake, here) is still perfectly constructible, so the route is
// wired exactly as configured; the operator just never gets a request
// for it since nothing in the app sends that PromptName.
func TestBuildAIGeneratorSucceedsDespiteUnknownRoutedPromptName(t *testing.T) {
	cfg := baseCfg()
	cfg.AI.Routes = "nonexistent.prompt=fake"

	if _, _, _, err := buildAIGenerator(cfg, &memRepo{}, nil); err != nil {
		t.Fatalf("buildAIGenerator: %v, want success (unknown prompt name is a warning, not a boot error)", err)
	}
}
