package config

import (
	"reflect"
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8080 || cfg.Auth.Mode != "static" || cfg.AI.Provider != "fake" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Auth.Static.ID != "dev" {
		t.Fatalf("static identity default: %+v", cfg.Auth.Static)
	}
	if cfg.AI.Ollama.URL != "http://ollama:11434" {
		t.Fatalf("AI.Ollama.URL default = %q, want http://ollama:11434", cfg.AI.Ollama.URL)
	}
	if cfg.AI.Ollama.Model != "" {
		t.Fatalf("AI.Ollama.Model default = %q, want empty (no default — required only when routed-to)", cfg.AI.Ollama.Model)
	}
	if cfg.AI.Routes != "" {
		t.Fatalf("AI.Routes default = %q, want empty", cfg.AI.Routes)
	}
	if cfg.AI.ClaudeCLI.Bin != "claude" {
		t.Fatalf("AI.ClaudeCLI.Bin default = %q, want claude", cfg.AI.ClaudeCLI.Bin)
	}
	if cfg.AI.CodexCLI.Bin != "codex" {
		t.Fatalf("AI.CodexCLI.Bin default = %q, want codex", cfg.AI.CodexCLI.Bin)
	}
	// Regression: the default must be the loopback address only. A
	// broad default like 172.16.0.0/12 would trust every private-network
	// peer, re-opening the hairpin-NAT spoofing hole a non-compose
	// deployment (no docker-compose.yml override) would otherwise be
	// exposed to.
	want := []string{"127.0.0.1/32"}
	if len(cfg.Auth.TrustedProxies) != len(want) || cfg.Auth.TrustedProxies[0] != want[0] {
		t.Fatalf("TrustedProxies default = %+v, want %+v (127.0.0.1/32 only, no 172.16.0.0/12)", cfg.Auth.TrustedProxies, want)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SERVER_PORT", "9999")
	t.Setenv("APP_AI_PROVIDER", "anthropic")
	t.Setenv("APP_AI_ANTHROPIC_APIKEY", "sk-test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9999 || cfg.AI.Anthropic.APIKey != "sk-test" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestOllamaAndRoutesEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_AI_PROVIDER", "ollama")
	t.Setenv("APP_AI_OLLAMA_URL", "http://localhost:11434")
	t.Setenv("APP_AI_OLLAMA_MODEL", "qwen3:4b")
	t.Setenv("APP_AI_ROUTES", "teacher.feedback=fake,anthropic")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Ollama.URL != "http://localhost:11434" {
		t.Fatalf("AI.Ollama.URL = %q, want override applied", cfg.AI.Ollama.URL)
	}
	if cfg.AI.Ollama.Model != "qwen3:4b" {
		t.Fatalf("AI.Ollama.Model = %q, want override applied", cfg.AI.Ollama.Model)
	}
	if cfg.AI.Routes != "teacher.feedback=fake,anthropic" {
		t.Fatalf("AI.Routes = %q, want override applied", cfg.AI.Routes)
	}
}

func TestCLIBinEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_AI_CLAUDECLI_BIN", "/usr/local/bin/claude")
	t.Setenv("APP_AI_CODEXCLI_BIN", "/usr/local/bin/codex")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.ClaudeCLI.Bin != "/usr/local/bin/claude" {
		t.Fatalf("AI.ClaudeCLI.Bin = %q, want override applied", cfg.AI.ClaudeCLI.Bin)
	}
	if cfg.AI.CodexCLI.Bin != "/usr/local/bin/codex" {
		t.Fatalf("AI.CodexCLI.Bin = %q, want override applied", cfg.AI.CodexCLI.Bin)
	}
}

// TestAnkiConnectURLDefaultsEmptyAndDoesNotRequireValidation pins the
// "feature dormant by default" contract (PRD §19): with no
// APP_ANKI_CONNECT_URL set, Config.Anki.ConnectURL is "" and Load still
// succeeds — TSV export must work with zero Anki-specific configuration.
func TestAnkiConnectURLDefaultsEmptyAndDoesNotRequireValidation(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Anki.ConnectURL != "" {
		t.Fatalf("Anki.ConnectURL default = %q, want empty", cfg.Anki.ConnectURL)
	}
}

// TestAnkiConnectURLEnvOverride pins the exact operator-facing env var
// name APP_ANKI_CONNECT_URL (not the auto-derived APP_ANKI_CONNECTURL).
func TestAnkiConnectURLEnvOverride(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_ANKI_CONNECT_URL", "http://localhost:8765")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Anki.ConnectURL != "http://localhost:8765" {
		t.Fatalf("Anki.ConnectURL = %q, want override applied", cfg.Anki.ConnectURL)
	}
}

func TestEmptyEnvVarDoesNotClobberDefault(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	// docker-compose.yml passes APP_AI_ANTHROPIC_MODEL/BASEURL through as
	// ${VAR:-} when the operator's .env doesn't set them, which means the
	// container sees the env var SET to an empty string, not unset. This
	// pins whether viper's automatic-env binding treats "set but empty"
	// the same as "unset" (falls back to the default) or clobbers it.
	t.Setenv("APP_AI_ANTHROPIC_MODEL", "")
	t.Setenv("APP_AI_ANTHROPIC_BASEURL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.Anthropic.Model != "claude-sonnet-5" {
		t.Fatalf("Model = %q, want default claude-sonnet-5 (empty env var should not override)", cfg.AI.Anthropic.Model)
	}
	if cfg.AI.Anthropic.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want default https://api.anthropic.com (empty env var should not override)", cfg.AI.Anthropic.BaseURL)
	}
}

// TestCLIBinEmptyEnvVarDoesNotClobberDefault is
// TestEmptyEnvVarDoesNotClobberDefault's twin for the two Task 12
// fields: .env.example and README both document "defaults to
// claude/codex when unset/empty" for APP_AI_CLAUDECLI_BIN/
// APP_AI_CODEXCLI_BIN, the identical claim already pinned for
// Anthropic's Model/BaseURL above — this closes the matching coverage
// gap for the new keys (docker-compose.yml doesn't currently wire
// these two through as ${VAR:-}, unlike the Anthropic ones, but the
// documented "empty also falls back to default" contract holds
// independent of that, and should stay pinned in case compose ever
// does start passing them through the same way).
func TestCLIBinEmptyEnvVarDoesNotClobberDefault(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_AI_CLAUDECLI_BIN", "")
	t.Setenv("APP_AI_CODEXCLI_BIN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.ClaudeCLI.Bin != "claude" {
		t.Fatalf("AI.ClaudeCLI.Bin = %q, want default claude (empty env var should not override)", cfg.AI.ClaudeCLI.Bin)
	}
	if cfg.AI.CodexCLI.Bin != "codex" {
		t.Fatalf("AI.CodexCLI.Bin = %q, want default codex (empty env var should not override)", cfg.AI.CodexCLI.Bin)
	}
}

func TestTrustedProxiesEnvOverride(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_AUTH_TRUSTEDPROXIES", "127.0.0.1/32,172.30.5.9/32")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1/32", "172.30.5.9/32"}
	if len(cfg.Auth.TrustedProxies) != len(want) || cfg.Auth.TrustedProxies[0] != want[0] || cfg.Auth.TrustedProxies[1] != want[1] {
		t.Fatalf("TrustedProxies = %+v, want %+v", cfg.Auth.TrustedProxies, want)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"missing db url":                    {"APP_DATABASE_URL": ""},
		"bad auth mode":                     {"APP_DATABASE_URL": "postgres://x", "APP_AUTH_MODE": "oauth"},
		"anthropic without key":             {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "anthropic"},
		"unknown ai provider":               {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "hal9000"},
		"ollama without model":              {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "ollama"},
		"malformed ai routes":               {"APP_DATABASE_URL": "postgres://x", "APP_AI_ROUTES": "teacher.feedback"},
		"ai routes bad provider":            {"APP_DATABASE_URL": "postgres://x", "APP_AI_ROUTES": "teacher.feedback=hal9000"},
		"summary enabled without recipient": {"APP_DATABASE_URL": "postgres://x", "APP_SUMMARY_ENABLED": "true"},
		"summary enabled with bad cron": {"APP_DATABASE_URL": "postgres://x", "APP_SUMMARY_ENABLED": "true",
			"APP_SUMMARY_TO": "learner@jlp.local", "APP_SUMMARY_CRON": "not a cron spec"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestValidationAutheliaRequiresNonEmptyTrustedProxies exercises
// Config.validate directly rather than through Load: Viper's env
// handling treats a *set-but-empty* APP_AUTH_TRUSTEDPROXIES the same as
// unset (viper's allowEmptyEnv defaults to false, so an empty env var
// falls back to the non-empty default rather than producing an empty
// slice — see TestEmptyEnvVarDoesNotClobberDefault for the same
// behavior on string fields). validate's guard is therefore defense in
// depth against any other path that could construct a Config with an
// empty list (a future default change, a non-env config source, etc.),
// and this test proves that guard fires.
// TestSummaryDefaults pins the weekly-summary scheduler's "dormant by
// default" contract (PRD §21, §65): with no APP_SUMMARY_*/APP_SMTP_*
// set, Load still succeeds, Enabled is false, Cron carries the brief's
// exact Sunday-18:00 default, and To/From are empty — mirrors
// TestAnkiConnectURLDefaultsEmptyAndDoesNotRequireValidation above.
func TestSummaryDefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Summary.Enabled {
		t.Fatal("Summary.Enabled default = true, want false (dormant by default)")
	}
	if cfg.Summary.Cron != "0 18 * * 0" {
		t.Fatalf("Summary.Cron default = %q, want \"0 18 * * 0\"", cfg.Summary.Cron)
	}
	if cfg.Summary.To != "" || cfg.Summary.From != "" {
		t.Fatalf("Summary.To/From defaults = %q/%q, want both empty", cfg.Summary.To, cfg.Summary.From)
	}
	if cfg.SMTP.Addr != "mailpit:1025" {
		t.Fatalf("SMTP.Addr default = %q, want mailpit:1025", cfg.SMTP.Addr)
	}
}

// TestSummaryEnvOverrides pins every APP_SUMMARY_*/APP_SMTP_* env var
// name exactly as documented (.env.example, README).
func TestSummaryEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SUMMARY_ENABLED", "true")
	t.Setenv("APP_SUMMARY_CRON", "0 9 * * 1")
	t.Setenv("APP_SUMMARY_TO", "learner@jlp.local")
	t.Setenv("APP_SUMMARY_FROM", "jlp@example.com")
	t.Setenv("APP_SMTP_ADDR", "smtp.example.com:587")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Summary.Enabled {
		t.Fatal("Summary.Enabled = false, want true")
	}
	if cfg.Summary.Cron != "0 9 * * 1" {
		t.Fatalf("Summary.Cron = %q, want override applied", cfg.Summary.Cron)
	}
	if cfg.Summary.To != "learner@jlp.local" {
		t.Fatalf("Summary.To = %q, want override applied", cfg.Summary.To)
	}
	if cfg.Summary.From != "jlp@example.com" {
		t.Fatalf("Summary.From = %q, want override applied", cfg.Summary.From)
	}
	if cfg.SMTP.Addr != "smtp.example.com:587" {
		t.Fatalf("SMTP.Addr = %q, want override applied", cfg.SMTP.Addr)
	}
}

// TestSummaryEnabledRequiresRecipientAndValidCron exercises
// Config.validate directly (same reasoning as
// TestValidationAutheliaRequiresNonEmptyTrustedProxies above): once
// Summary.Enabled is true, an empty To or an unparseable Cron must both
// fail validation — a live scheduler with nowhere to send, or a spec
// robfig/cron can't parse, would otherwise only fail much later (or
// never, for an inert cron.AddFunc) instead of at boot.
func TestSummaryEnabledRequiresRecipientAndValidCron(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	noTo := base
	noTo.Summary = Summary{Enabled: true, Cron: "0 18 * * 0"}
	if err := noTo.validate(); err == nil {
		t.Fatal("expected validation error for Summary.Enabled with empty To")
	}

	badCron := base
	badCron.Summary = Summary{Enabled: true, Cron: "not a cron spec", To: "learner@jlp.local"}
	if err := badCron.validate(); err == nil {
		t.Fatal("expected validation error for Summary.Enabled with an invalid Cron")
	}

	ok := base
	ok.Summary = Summary{Enabled: true, Cron: "0 18 * * 0", To: "learner@jlp.local"}
	if err := ok.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for a well-formed enabled Summary", err)
	}
}

func TestValidationAutheliaRequiresNonEmptyTrustedProxies(t *testing.T) {
	cfg := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "authelia", TrustedProxies: []string{}},
		AI:       AI{Provider: "fake"},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error for authelia mode with empty TrustedProxies")
	}
}

func TestParseRoutesValid(t *testing.T) {
	got, err := ParseRoutes("teacher.feedback=fake,anthropic;drill.exercise=ollama")
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	want := map[string][]string{
		"teacher.feedback": {"fake", "anthropic"},
		"drill.exercise":   {"ollama"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseRoutes = %+v, want %+v", got, want)
	}
}

func TestParseRoutesEmptyStringIsNoRoutes(t *testing.T) {
	got, err := ParseRoutes("")
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParseRoutes(\"\") = %+v, want empty map", got)
	}
}

// TestParseRoutesAcceptsCLIProviderNames pins the brief's split:
// ParseRoutes only validates the provider NAME is one of the five
// known names — whether cmd/jlp/ai.go's buildAIGenerator can actually
// construct an instance for it (claudecli/codexcli always can, as of
// Task 12; see internal/adapters/clicmd) is a separate, later check,
// not ParseRoutes's job.
func TestParseRoutesAcceptsCLIProviderNames(t *testing.T) {
	got, err := ParseRoutes("teacher.feedback=claudecli,codexcli")
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	want := []string{"claudecli", "codexcli"}
	if !reflect.DeepEqual(got["teacher.feedback"], want) {
		t.Fatalf("ParseRoutes = %+v, want teacher.feedback: %v", got, want)
	}
}

func TestParseRoutesRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown provider name": "teacher.feedback=hal9000",
		"missing =":             "teacher.feedback",
		"empty prompt name":     "=fake",
		"empty provider list":   "teacher.feedback=",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRoutes(s); err == nil {
				t.Fatalf("ParseRoutes(%q): expected error, got nil", s)
			}
		})
	}
}

// TestParseRoutesTrimsWhitespaceAndSkipsBlankEntries pins the
// tolerant side of parsing: stray whitespace around names/providers,
// and a doubled comma inside one entry's provider list, must not be
// treated as an error — only an entry with NO providers at all (the
// "empty provider list" case above) is rejected.
func TestParseRoutesTrimsWhitespaceAndSkipsBlankEntries(t *testing.T) {
	got, err := ParseRoutes(" teacher.feedback = fake, ,anthropic ; drill.exercise=ollama ")
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	want := map[string][]string{
		"teacher.feedback": {"fake", "anthropic"},
		"drill.exercise":   {"ollama"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseRoutes = %+v, want %+v", got, want)
	}
}
