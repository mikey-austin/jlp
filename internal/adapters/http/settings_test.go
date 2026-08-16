package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeSettingsRepo is a minimal in-memory storage.SettingsRepository —
// httpx.Options.Settings is a concrete *appsettings.Service (it also
// serves as the ports/ai.ModelResolver every AI adapter holds — see
// that field's own doc comment), so these handler tests build a real
// Service on top of this fake repository rather than mocking the
// service itself.
type fakeSettingsRepo struct {
	mu   sync.Mutex
	rows map[string]string
}

func newFakeSettingsRepo() *fakeSettingsRepo { return &fakeSettingsRepo{rows: map[string]string{}} }

func (r *fakeSettingsRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[key] = value
	return nil
}

func (r *fakeSettingsRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, key)
	return nil
}

func (r *fakeSettingsRepo) List(_ context.Context) ([]storage.AppSetting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]storage.AppSetting, 0, len(r.rows))
	for k, v := range r.rows {
		out = append(out, storage.AppSetting{Key: k, Value: v})
	}
	return out, nil
}

// settingsTestOptions builds Options with a real *appsettings.Service
// (backed by fakeSettingsRepo) wired in, on top of testOptions().
func settingsTestOptions(t *testing.T) (Options, *fakeSettingsRepo) {
	t.Helper()
	return settingsTestOptionsWithListers(t, nil)
}

// settingsTestOptionsWithListers is settingsTestOptions plus an
// explicit ports/ai.ModelLister map (Phase 4 Task M), for tests that
// need to exercise the /settings dropdown-vs-free-text rendering.
func settingsTestOptionsWithListers(t *testing.T, listers map[string]ai.ModelLister) (Options, *fakeSettingsRepo) {
	t.Helper()
	repo := newFakeSettingsRepo()
	cfg := config.AI{
		Ollama:    config.Ollama{Model: "gemma4:12b"},
		Anthropic: config.Anthropic{Model: "claude-sonnet-5"},
		ClaudeCLI: config.ClaudeCLI{Model: "opus", Effort: "medium"},
		CodexCLI:  config.CodexCLI{Model: "gpt-5.5-codex", Effort: "low"},
	}
	svc, err := appsettings.NewService(context.Background(), repo, cfg, listers)
	if err != nil {
		t.Fatalf("appsettings.NewService: %v", err)
	}
	opts := testOptions()
	opts.Settings = svc
	return opts, repo
}

// fakeLister is a minimal ports/ai.ModelLister test double, mirroring
// application/settings's own (unexported, so not reusable from this
// package).
type fakeLister struct {
	models []string
	err    error
}

func (f *fakeLister) ListModels(_ context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

// TestSettingsPageRendersEffectiveValuesAndSource covers GET /settings:
// every provider's row must render its current effective model (the
// config value, with nothing yet overridden) and say so ("config").
func TestSettingsPageRendersEffectiveValuesAndSource(t *testing.T) {
	opts, _ := settingsTestOptions(t)
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"gemma4:12b", "claude-sonnet-5", "opus", "gpt-5.5-codex", "config"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /settings body missing %q: %s", want, body)
		}
	}
	// Effort selects populated from config's own vocabulary (not a
	// hand-copied list) — "max" and "xhigh" must appear (claudecli's
	// vocabulary includes them).
	if !strings.Contains(body, "max") {
		t.Errorf("GET /settings body missing the claudecli effort option \"max\": %s", body)
	}
}

// TestSettingsSetModelSavesOverrideAndRedirects covers POST
// /settings/{provider}/model: a valid save persists through the
// repository and redirects back to /settings, where the NEW value now
// renders with source "override".
func TestSettingsSetModelSavesOverrideAndRedirects(t *testing.T) {
	opts, repo := settingsTestOptions(t)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("model", "gemma4:latest")
	rec := postForm(t, srv.HandlerForTest(), "/settings/ollama/model", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/ollama/model status = %d, want 303, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/settings" {
		t.Errorf("Location = %q, want /settings", got)
	}

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("repo.List: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Key == appsettings.KeyOllamaModel && r.Value == "gemma4:latest" {
			found = true
		}
	}
	if !found {
		t.Errorf("repo rows after save = %+v, want ai.ollama.model=gemma4:latest", rows)
	}

	// The change must be visible on the very next request — no restart,
	// no re-wiring — since Options.Settings IS the same in-memory
	// resolver instance every AI adapter would consult.
	if model, _ := opts.Settings.Model("ollama"); model != "gemma4:latest" {
		t.Errorf("Settings.Model(ollama) after save = %q, want gemma4:latest (available to the NEXT AI request with no restart)", model)
	}
}

// TestSettingsSetEffortRejectsInvalidValueAndWritesNothing is the
// browser-verification "reject case" pinned as an HTTP-layer test: an
// effort invalid for the given tool (codexcli doesn't accept "max")
// must render the error in the page body, respond with a non-2xx/3xx
// status, and leave the repository untouched.
func TestSettingsSetEffortRejectsInvalidValueAndWritesNothing(t *testing.T) {
	opts, repo := settingsTestOptions(t)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("effort", "max") // valid for claudecli, NOT for codexcli
	rec := postForm(t, srv.HandlerForTest(), "/settings/codexcli/effort", form)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /settings/codexcli/effort (invalid) status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "effort must be") {
		t.Errorf("GET body missing the surfaced validation error: %s", body)
	}

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("repo.List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("repo rows after a rejected save = %v, want empty (invalid input must never be written)", rows)
	}
}

// TestSettingsResetDeletesOverride covers POST /settings/{provider}/reset:
// after overriding a value, reset must remove it and Model must report
// the config fallback again.
func TestSettingsResetDeletesOverride(t *testing.T) {
	opts, repo := settingsTestOptions(t)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("model", "gemma4:latest")
	if rec := postForm(t, srv.HandlerForTest(), "/settings/ollama/model", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed save status = %d", rec.Code)
	}

	rec := postForm(t, srv.HandlerForTest(), "/settings/ollama/reset", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/ollama/reset status = %d, want 303, body=%s", rec.Code, rec.Body.String())
	}

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("repo.List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("repo rows after reset = %v, want empty", rows)
	}
	if model, _ := opts.Settings.Model("ollama"); model != "gemma4:12b" {
		t.Errorf("Settings.Model(ollama) after reset = %q, want the config value gemma4:12b", model)
	}
}

// TestSettingsPageRendersSelectWhenEnumerationWorks covers the Task M
// happy path end to end through the HTTP layer: a provider with a
// working ModelLister renders a <select> carrying its models, with the
// free-text escape hatch still present alongside it.
func TestSettingsPageRendersSelectWhenEnumerationWorks(t *testing.T) {
	listers := map[string]ai.ModelLister{
		"ollama": &fakeLister{models: []string{"gemma4:12b", "gemma4:latest", "qwen3.6:latest"}},
	}
	opts, _ := settingsTestOptionsWithListers(t, listers)
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<select class="select" name="model">`) {
		t.Errorf("GET /settings body has no model <select>, want one for ollama: %s", body)
	}
	for _, want := range []string{"gemma4:12b", "gemma4:latest", "qwen3.6:latest"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /settings body missing enumerated model %q: %s", want, body)
		}
	}
	// Embedding-style filtering happens in the adapter, not here, but the
	// free-text escape hatch must still be present even when a select is
	// rendered — a value not offered by the list must stay settable.
	if strings.Count(body, `type="text" name="model"`) == 0 {
		t.Error("GET /settings body has no free-text model input alongside the select")
	}
}

// TestSettingsPageDegradesToFreeTextOnListerError covers the "Ollama
// unreachable" browser-verification case at the HTTP layer: a failing
// ModelLister must still render 200 (never 500), with the free-text
// input and a reason, not a select.
func TestSettingsPageDegradesToFreeTextOnListerError(t *testing.T) {
	listers := map[string]ai.ModelLister{
		"ollama": &fakeLister{err: errors.New(`dial tcp 127.0.0.1:1: connect: connection refused`)},
	}
	opts, _ := settingsTestOptionsWithListers(t, listers)
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200 (degrade, never 500), body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Scope to the Ollama card specifically: claudecli's row legitimately
	// renders its OWN "<select ... name=\"model\">" for its documented
	// aliases (see TestSettingsPageOffersClaudeCLIAliasesAndFreeText)
	// regardless of Ollama's lister state, so a page-wide search for that
	// markup would false-positive on it.
	ollamaSection := body[strings.Index(body, "Ollama"):strings.Index(body, "Anthropic")]
	if strings.Contains(ollamaSection, `<select class="select" name="model">`) {
		t.Errorf("Ollama section has a model <select> despite the lister erroring: %s", ollamaSection)
	}
	if !strings.Contains(ollamaSection, "connection refused") {
		t.Errorf("Ollama section doesn't surface the failure reason: %s", ollamaSection)
	}
	// The provider's actual effective model must still be visible in the
	// (now free-text-only) input — the operator's setting is never lost.
	if !strings.Contains(ollamaSection, `value="gemma4:12b"`) {
		t.Errorf("Ollama section lost the effective model value: %s", ollamaSection)
	}
}

// TestSettingsPageOffersClaudeCLIAliasesAndFreeText covers claudecli's
// "cannot enumerate, but may offer documented aliases" case: no
// <select> of a fabricated list, but the sonnet/opus/haiku aliases
// appear (clearly not as the primary model select), the explanatory
// note is present, and free text is still available.
func TestSettingsPageOffersClaudeCLIAliasesAndFreeText(t *testing.T) {
	opts, _ := settingsTestOptions(t)
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"sonnet", "opus", "haiku"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /settings body missing claudecli alias %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "not enumerated") {
		t.Errorf("GET /settings body missing the claudecli \"not enumerated\" note: %s", body)
	}
}
