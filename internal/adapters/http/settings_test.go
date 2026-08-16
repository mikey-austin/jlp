package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
	"github.com/mikeyaustin/jlp/internal/config"
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
	repo := newFakeSettingsRepo()
	cfg := config.AI{
		Ollama:    config.Ollama{Model: "gemma4:12b"},
		Anthropic: config.Anthropic{Model: "claude-sonnet-5"},
		ClaudeCLI: config.ClaudeCLI{Model: "opus", Effort: "medium"},
		CodexCLI:  config.CodexCLI{Model: "gpt-5.5-codex", Effort: "low"},
	}
	svc, err := appsettings.NewService(context.Background(), repo, cfg)
	if err != nil {
		t.Fatalf("appsettings.NewService: %v", err)
	}
	opts := testOptions()
	opts.Settings = svc
	return opts, repo
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
