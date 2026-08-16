package settings_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/settings"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeRepo is a minimal in-memory storage.SettingsRepository — a test
// double for the postgres adapter, injected the same way every other
// application-layer test in this codebase fakes its repository port.
// Safe for concurrent use (TestModelIsSafeForConcurrentReadsAndWrites
// below relies on that, exercising Service's OWN locking, not this
// fake's — see that test's doc comment).
type fakeRepo struct {
	mu   sync.Mutex
	rows map[string]string
}

func newFakeRepo() *fakeRepo { return &fakeRepo{rows: map[string]string{}} }

func (r *fakeRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[key] = value
	return nil
}

func (r *fakeRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, key)
	return nil
}

func (r *fakeRepo) List(_ context.Context) ([]storage.AppSetting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]storage.AppSetting, 0, len(r.rows))
	for k, v := range r.rows {
		out = append(out, storage.AppSetting{Key: k, Value: v})
	}
	return out, nil
}

func baseAICfg() config.AI {
	return config.AI{
		Ollama:    config.Ollama{Model: "gemma4:12b"},
		Anthropic: config.Anthropic{Model: "claude-sonnet-5"},
		ClaudeCLI: config.ClaudeCLI{Model: "opus", Effort: "medium"},
		CodexCLI:  config.CodexCLI{Model: "gpt-5.5-codex", Effort: "low"},
	}
}

func newTestService(t *testing.T, cfg config.AI) (*settings.Service, *fakeRepo) {
	t.Helper()
	return newTestServiceWithListers(t, cfg, nil)
}

func newTestServiceWithListers(t *testing.T, cfg config.AI, listers map[string]ai.ModelLister) (*settings.Service, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	svc, err := settings.NewService(context.Background(), repo, cfg, listers)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, repo
}

// fakeLister is a minimal, call-counting ports/ai.ModelLister test
// double: Models/Err control what ListModels returns, and Calls lets a
// test assert HOW MANY TIMES the underlying "expensive" enumeration
// actually ran — the caching contract's whole point (task brief:
// "agy models costs a subprocess").
type fakeLister struct {
	mu     sync.Mutex
	Models []string
	Err    error
	Calls  int
}

func (f *fakeLister) ListModels(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Models, nil
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}

// TestModelReturnsConfigValueWithNoOverride pins the fallback contract
// (task brief Step 1, bullet 1): with nothing ever saved through
// Service, Model must report exactly what config.AI said, for every
// provider — an absent override means "use APP_AI_*", never a zero
// value.
func TestModelReturnsConfigValueWithNoOverride(t *testing.T) {
	cfg := baseAICfg()
	svc, _ := newTestService(t, cfg)

	for _, tc := range []struct {
		provider   string
		wantModel  string
		wantEffort string
	}{
		{"ollama", cfg.Ollama.Model, ""},
		{"anthropic", cfg.Anthropic.Model, ""},
		{"claudecli", cfg.ClaudeCLI.Model, cfg.ClaudeCLI.Effort},
		{"codexcli", cfg.CodexCLI.Model, cfg.CodexCLI.Effort},
	} {
		gotModel, gotEffort := svc.Model(tc.provider)
		if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
			t.Errorf("Model(%q) = (%q, %q), want (%q, %q)", tc.provider, gotModel, gotEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

// TestModelReturnsOverrideWhenSet pins the override contract (Step 1,
// bullet 2): once SetModel/SetEffort has saved a value, Model must
// report THAT, not the config fallback — for both a model-only
// provider (ollama) and a provider with both knobs (claudecli).
func TestModelReturnsOverrideWhenSet(t *testing.T) {
	cfg := baseAICfg()
	svc, _ := newTestService(t, cfg)
	ctx := context.Background()

	if err := svc.SetModel(ctx, "ollama", "gemma4:latest"); err != nil {
		t.Fatalf("SetModel(ollama): %v", err)
	}
	if model, effort := svc.Model("ollama"); model != "gemma4:latest" || effort != "" {
		t.Errorf("Model(ollama) after override = (%q, %q), want (gemma4:latest, \"\")", model, effort)
	}
	// anthropic was never overridden — must still read the config value.
	if model, _ := svc.Model("anthropic"); model != cfg.Anthropic.Model {
		t.Errorf("Model(anthropic) = %q, want unaffected config value %q", model, cfg.Anthropic.Model)
	}

	if err := svc.SetModel(ctx, "claudecli", "sonnet"); err != nil {
		t.Fatalf("SetModel(claudecli): %v", err)
	}
	if err := svc.SetEffort(ctx, "claudecli", "xhigh"); err != nil {
		t.Fatalf("SetEffort(claudecli): %v", err)
	}
	if model, effort := svc.Model("claudecli"); model != "sonnet" || effort != "xhigh" {
		t.Errorf("Model(claudecli) after override = (%q, %q), want (sonnet, xhigh)", model, effort)
	}
}

// TestSetEffortRejectsInvalidPerTool pins Step 1's exact example: "max"
// is a valid Claude Code effort but not a Codex one, and "minimal" the
// reverse — a value only one CLI accepts must be rejected for the
// other, using config's own single vocabulary definition (never a
// second, hand-copied list here).
func TestSetEffortRejectsInvalidPerTool(t *testing.T) {
	svc, repo := newTestService(t, baseAICfg())
	ctx := context.Background()

	if err := svc.SetEffort(ctx, "claudecli", "max"); err != nil {
		t.Errorf("SetEffort(claudecli, max) = %v, want nil (max is valid for Claude Code)", err)
	}
	if err := svc.SetEffort(ctx, "codexcli", "max"); err == nil {
		t.Error("SetEffort(codexcli, max) = nil, want an error (max is not a valid Codex effort)")
	}

	if err := svc.SetEffort(ctx, "codexcli", "minimal"); err != nil {
		t.Errorf("SetEffort(codexcli, minimal) = %v, want nil (minimal is valid for Codex)", err)
	}
	if err := svc.SetEffort(ctx, "claudecli", "minimal"); err == nil {
		t.Error("SetEffort(claudecli, minimal) = nil, want an error (minimal is not a valid Claude Code effort)")
	}

	// The two REJECTED calls above must never have reached the
	// repository — "codexcli"="max" and "claudecli"="minimal" must be
	// absent from what was actually persisted.
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Key] = r.Value
	}
	if v, ok := got[settings.KeyCodexCLIEffort]; ok && v == "max" {
		t.Errorf("rejected codexcli effort %q was written to the repository", v)
	}
	if v, ok := got[settings.KeyClaudeCLIEffort]; ok && v == "minimal" {
		t.Errorf("rejected claudecli effort %q was written to the repository", v)
	}
}

// TestSetEffortInvalidValueNeverWritesToRepo is the direct, minimal
// version of the brief's "never written to the DB" requirement: a
// single invalid call against an otherwise-empty repo must leave it
// completely empty.
func TestSetEffortInvalidValueNeverWritesToRepo(t *testing.T) {
	svc, repo := newTestService(t, baseAICfg())
	ctx := context.Background()

	if err := svc.SetEffort(ctx, "codexcli", "not-a-real-effort"); err == nil {
		t.Fatal("SetEffort(codexcli, not-a-real-effort) = nil, want an error")
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("repo rows after a rejected SetEffort = %v, want empty", rows)
	}
}

// TestSetEffortRejectsForProvidersWithoutOne covers ollama/anthropic —
// neither has an effort concept, so SetEffort against either must
// fail with ErrNoEffort regardless of the value, and never write.
func TestSetEffortRejectsForProvidersWithoutOne(t *testing.T) {
	svc, repo := newTestService(t, baseAICfg())
	ctx := context.Background()

	for _, provider := range []string{"ollama", "anthropic"} {
		err := svc.SetEffort(ctx, provider, "high")
		if !errors.Is(err, settings.ErrNoEffort) {
			t.Errorf("SetEffort(%q, high) = %v, want ErrNoEffort", provider, err)
		}
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("repo rows = %v, want empty", rows)
	}
}

// TestResetDeletesOverrideAndFallsBackToConfig pins the "reset to
// config" action end to end: after overriding both model and effort,
// Reset must remove both, and Model must go back to reporting the
// config values — not zero values.
func TestResetDeletesOverrideAndFallsBackToConfig(t *testing.T) {
	cfg := baseAICfg()
	svc, _ := newTestService(t, cfg)
	ctx := context.Background()

	if err := svc.SetModel(ctx, "codexcli", "gpt-6"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := svc.SetEffort(ctx, "codexcli", "high"); err != nil {
		t.Fatalf("SetEffort: %v", err)
	}
	if model, effort := svc.Model("codexcli"); model != "gpt-6" || effort != "high" {
		t.Fatalf("Model(codexcli) before reset = (%q, %q), want (gpt-6, high)", model, effort)
	}

	if err := svc.Reset(ctx, "codexcli"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if model, effort := svc.Model("codexcli"); model != cfg.CodexCLI.Model || effort != cfg.CodexCLI.Effort {
		t.Errorf("Model(codexcli) after reset = (%q, %q), want config values (%q, %q)", model, effort, cfg.CodexCLI.Model, cfg.CodexCLI.Effort)
	}
}

// TestRowsReportsSourceAndEffortOptions exercises the /settings page's
// own read path: ModelSource/EffortSource must say "config" until
// overridden, then "override"; EffortOptions must come from config's
// own per-tool vocabulary (never a hand-copied list — see
// config.ClaudeEfforts/CodexEfforts), and stay empty for a provider
// with no effort concept.
func TestRowsReportsSourceAndEffortOptions(t *testing.T) {
	cfg := baseAICfg()
	svc, _ := newTestService(t, cfg)
	ctx := context.Background()

	rows := svc.Rows(context.Background())
	byProvider := map[string]settings.Row{}
	for _, r := range rows {
		byProvider[r.Provider] = r
	}

	if r := byProvider["ollama"]; r.ModelSource != "config" || r.HasEffort {
		t.Errorf("ollama row = %+v, want ModelSource=config, HasEffort=false", r)
	}
	claude := byProvider["claudecli"]
	if !claude.HasEffort || claude.EffortSource != "config" {
		t.Errorf("claudecli row = %+v, want HasEffort=true, EffortSource=config", claude)
	}
	if len(claude.EffortOptions) == 0 {
		t.Error("claudecli row EffortOptions is empty, want config.ClaudeEfforts()")
	}
	for _, opt := range claude.EffortOptions {
		found := false
		for _, want := range config.ClaudeEfforts() {
			if opt == want {
				found = true
			}
		}
		if !found {
			t.Errorf("claudecli EffortOptions contains %q, not in config.ClaudeEfforts()", opt)
		}
	}

	if err := svc.SetModel(ctx, "ollama", "gemma4:latest"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	rows = svc.Rows(context.Background())
	for _, r := range rows {
		if r.Provider == "ollama" && r.ModelSource != "override" {
			t.Errorf("ollama row ModelSource after SetModel = %q, want override", r.ModelSource)
		}
	}
}

// TestModelIsSafeForConcurrentReadsAndWrites is the concurrency
// requirement from the task brief and PRD context notes: the resolver
// is read from many HTTP goroutines (Model) while the settings handler
// writes (SetModel/Reset) — run under `go test -race` (make
// test-race), this must never trip the race detector. The assertion
// itself is minimal (no panic, no data race) since the interesting
// property here IS the absence of a race, not any particular
// intermediate value.
func TestModelIsSafeForConcurrentReadsAndWrites(t *testing.T) {
	svc, _ := newTestService(t, baseAICfg())
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					svc.Model("ollama")
					svc.Model("claudecli")
					svc.Rows(context.Background())
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			if err := svc.SetModel(ctx, "ollama", "gemma4:latest"); err != nil {
				t.Errorf("SetModel: %v", err)
			}
		} else {
			if err := svc.Reset(ctx, "ollama"); err != nil {
				t.Errorf("Reset: %v", err)
			}
		}
	}
	close(stop)
	wg.Wait()
}

// TestRowsPopulatesModelOptionsFromLister pins Task M's happy path: a
// provider with a working ModelLister gets ModelOptions from it and no
// ModelNote (nothing to explain — enumeration just worked).
func TestRowsPopulatesModelOptionsFromLister(t *testing.T) {
	cfg := baseAICfg()
	lister := &fakeLister{Models: []string{"gemma4:12b", "gemma4:latest"}}
	svc, _ := newTestServiceWithListers(t, cfg, map[string]ai.ModelLister{"ollama": lister})

	rows := svc.Rows(context.Background())
	var ollama settings.Row
	for _, r := range rows {
		if r.Provider == "ollama" {
			ollama = r
		}
	}

	if len(ollama.ModelOptions) != 2 {
		t.Fatalf("ollama.ModelOptions = %v, want the lister's 2 models", ollama.ModelOptions)
	}
	if ollama.ModelNote != "" {
		t.Errorf("ollama.ModelNote = %q, want empty on a successful list", ollama.ModelNote)
	}
}

// TestRowsAlwaysIncludesCurrentValueEvenIfListerOmitsIt pins the "the
// operator's current value must never be silently dropped" rule: a
// model that WAS overridden but is no longer in the lister's result
// (removed, or hand-typed) must still appear in ModelOptions.
func TestRowsAlwaysIncludesCurrentValueEvenIfListerOmitsIt(t *testing.T) {
	cfg := baseAICfg()
	lister := &fakeLister{Models: []string{"gemma4:12b"}}
	svc, _ := newTestServiceWithListers(t, cfg, map[string]ai.ModelLister{"ollama": lister})

	if err := svc.SetModel(context.Background(), "ollama", "some-removed-model:9b"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	rows := svc.Rows(context.Background())
	var ollama settings.Row
	for _, r := range rows {
		if r.Provider == "ollama" {
			ollama = r
		}
	}

	found := false
	for _, m := range ollama.ModelOptions {
		if m == "some-removed-model:9b" {
			found = true
		}
	}
	if !found {
		t.Errorf("ollama.ModelOptions = %v, want the overridden value \"some-removed-model:9b\" included even though the lister no longer reports it", ollama.ModelOptions)
	}
	if ollama.Model != "some-removed-model:9b" {
		t.Errorf("ollama.Model = %q, want the override, unchanged", ollama.Model)
	}
}

// TestRowsDegradesToFreeTextOnListerError pins the "degrade, don't
// block/500" rule: a failing ModelLister (e.g. Ollama unreachable)
// leaves ModelOptions empty and explains why in ModelNote, rather than
// Rows itself failing/panicking.
func TestRowsDegradesToFreeTextOnListerError(t *testing.T) {
	cfg := baseAICfg()
	lister := &fakeLister{Err: errors.New("dial tcp: connection refused")}
	svc, _ := newTestServiceWithListers(t, cfg, map[string]ai.ModelLister{"ollama": lister})

	rows := svc.Rows(context.Background())
	var ollama settings.Row
	for _, r := range rows {
		if r.Provider == "ollama" {
			ollama = r
		}
	}

	if len(ollama.ModelOptions) != 0 {
		t.Errorf("ollama.ModelOptions = %v, want empty when the lister errors", ollama.ModelOptions)
	}
	if ollama.ModelNote == "" {
		t.Error("ollama.ModelNote is empty, want a reason shown when enumeration failed")
	}
	// The effective model itself must be completely unaffected by the
	// listing failure — still the config fallback, still usable.
	if ollama.Model != cfg.Ollama.Model {
		t.Errorf("ollama.Model = %q, want unaffected config value %q", ollama.Model, cfg.Ollama.Model)
	}
}

// TestRowsCachesSuccessfulEnumeration pins the caching rule: a second
// Rows() call soon after a SUCCESSFUL one must not hit the lister
// again — the task brief specifically flags agy models as costing a
// real subprocess, so re-listing on every /settings render would be
// wasteful.
func TestRowsCachesSuccessfulEnumeration(t *testing.T) {
	cfg := baseAICfg()
	lister := &fakeLister{Models: []string{"gemma4:12b"}}
	svc, _ := newTestServiceWithListers(t, cfg, map[string]ai.ModelLister{"ollama": lister})

	svc.Rows(context.Background())
	svc.Rows(context.Background())
	svc.Rows(context.Background())

	if got := lister.callCount(); got != 1 {
		t.Errorf("lister.Calls = %d, want 1 (second/third Rows() should have served the cache)", got)
	}
}

// TestRowsNeverCachesAFailedEnumeration pins the other half of the
// caching rule: a FAILED attempt must never be remembered as "no
// models" — every subsequent Rows() call must retry, so an operator who
// fixes Ollama sees it recover on their very next /settings visit.
func TestRowsNeverCachesAFailedEnumeration(t *testing.T) {
	cfg := baseAICfg()
	lister := &fakeLister{Err: errors.New("connection refused")}
	svc, _ := newTestServiceWithListers(t, cfg, map[string]ai.ModelLister{"ollama": lister})

	svc.Rows(context.Background())
	svc.Rows(context.Background())

	if got := lister.callCount(); got != 2 {
		t.Errorf("lister.Calls = %d, want 2 (a failed attempt must never be cached)", got)
	}
}

// TestRowsClaudeCLIOffersAliasesNotEnumeration and
// TestRowsCodexCLIHasNoteButNoOptionsOrAliases pin the two providers
// that structurally cannot enumerate (task brief): claudecli gets its
// documented aliases as a clearly-separate, labelled convenience;
// codexcli gets neither options nor aliases, just the explanatory note.
// Neither ever consults a ModelLister (none is even offered here).
func TestRowsClaudeCLIOffersAliasesNotEnumeration(t *testing.T) {
	svc, _ := newTestService(t, baseAICfg())
	rows := svc.Rows(context.Background())

	var claude settings.Row
	for _, r := range rows {
		if r.Provider == "claudecli" {
			claude = r
		}
	}
	if len(claude.ModelOptions) != 0 {
		t.Errorf("claudecli.ModelOptions = %v, want empty (never a real enumeration)", claude.ModelOptions)
	}
	if len(claude.ModelAliases) == 0 {
		t.Error("claudecli.ModelAliases is empty, want the documented sonnet/opus/haiku aliases")
	}
	if claude.ModelNote == "" {
		t.Error("claudecli.ModelNote is empty, want an explanation that this isn't an enumeration")
	}
}

func TestRowsCodexCLIHasNoteButNoOptionsOrAliases(t *testing.T) {
	svc, _ := newTestService(t, baseAICfg())
	rows := svc.Rows(context.Background())

	var codex settings.Row
	for _, r := range rows {
		if r.Provider == "codexcli" {
			codex = r
		}
	}
	if len(codex.ModelOptions) != 0 {
		t.Errorf("codexcli.ModelOptions = %v, want empty", codex.ModelOptions)
	}
	if len(codex.ModelAliases) != 0 {
		t.Errorf("codexcli.ModelAliases = %v, want empty (no documented aliases offered for Codex)", codex.ModelAliases)
	}
	if codex.ModelNote == "" {
		t.Error("codexcli.ModelNote is empty, want an explanation that the list isn't available")
	}
}
