package config

import (
	"reflect"
	"strings"
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
	if cfg.AI.AgenticTeacher {
		t.Fatalf("AI.AgenticTeacher default = true, want false (Phase 1's single-shot path stays default)")
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
	t.Setenv("APP_AI_AGENTICTEACHER", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9999 || cfg.AI.Anthropic.APIKey != "sk-test" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if !cfg.AI.AgenticTeacher {
		t.Fatal("AI.AgenticTeacher = false, want true after APP_AI_AGENTICTEACHER=true")
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

// TestMQTTURLDefaultsEmptyAndDoesNotRequireValidation pins the MQTT
// bridge's "dormant by default" contract (PRD §31-33/§12/§59, Phase 3
// Task 6): with no APP_MQTT_URL set, Config.MQTT.URL is "" and Load
// still succeeds — mirrors TestAnkiConnectURLDefaultsEmptyAndDoesNotRequireValidation
// above for the analogous "empty means main.go never constructs the
// adapter" contract.
func TestMQTTURLDefaultsEmptyAndDoesNotRequireValidation(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.URL != "" {
		t.Fatalf("MQTT.URL default = %q, want empty", cfg.MQTT.URL)
	}
}

func TestMQTTURLEnvOverride(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_MQTT_URL", "tcp://mosquitto:1883")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.URL != "tcp://mosquitto:1883" {
		t.Fatalf("MQTT.URL = %q, want override applied", cfg.MQTT.URL)
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
	got, err := ParseRoutes("teacher.feedback=claudecli,codexcli,agycli")
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	want := []string{"claudecli", "codexcli", "agycli"}
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

// TestA2ADefaults pins Phase 4 Task 3's "dormant unless explicitly
// configured" contract: Enabled false, but Path still defaults to
// "/a2a" regardless — see A2A's doc comment on why Path always has a
// usable default even though Enabled doesn't.
func TestA2ADefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.A2A.Enabled {
		t.Fatal("A2A.Enabled default = true, want false (dormant by default)")
	}
	if cfg.A2A.Path != "/a2a" {
		t.Fatalf("A2A.Path default = %q, want \"/a2a\"", cfg.A2A.Path)
	}
}

// TestA2AEnvOverrides pins the APP_A2A_ENABLED/APP_A2A_PATH env var
// names exactly as documented (.env.example, README, docker-compose.yml).
func TestA2AEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_A2A_ENABLED", "true")
	t.Setenv("APP_A2A_PATH", "/agents")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.A2A.Enabled {
		t.Fatal("A2A.Enabled = false, want true")
	}
	if cfg.A2A.Path != "/agents" {
		t.Fatalf("A2A.Path = %q, want \"/agents\"", cfg.A2A.Path)
	}
}

// TestA2APathEmptyEnvVarDoesNotClobberDefault mirrors
// TestEmptyEnvVarDoesNotClobberDefault: docker-compose.yml passes
// APP_A2A_PATH through as ${APP_A2A_PATH:-}, so an operator who never
// sets it must still get the "/a2a" default, not an empty string.
func TestA2APathEmptyEnvVarDoesNotClobberDefault(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_A2A_ENABLED", "true")
	t.Setenv("APP_A2A_PATH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.A2A.Path != "/a2a" {
		t.Fatalf("A2A.Path = %q, want default \"/a2a\" (empty env var should not override)", cfg.A2A.Path)
	}
}

// TestA2AEnabledRequiresPathWithLeadingSlash pins validate's guard: a
// misconfigured Path only matters once the adapter is actually live —
// the same "a feature's config only needs to make sense once the
// feature is live" pattern TestSummaryEnabledRequiresRecipientAndValidCron
// pins for Summary.
func TestA2AEnabledRequiresPathWithLeadingSlash(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	badPath := base
	badPath.A2A = A2A{Enabled: true, Path: "a2a"}
	if err := badPath.validate(); err == nil {
		t.Fatal("expected validation error for A2A.Enabled with a Path missing its leading slash")
	}

	disabledBadPath := base
	disabledBadPath.A2A = A2A{Enabled: false, Path: "not-even-a-path"}
	if err := disabledBadPath.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil (A2A.Path is only checked once Enabled=true)", err)
	}

	ok := base
	ok.A2A = A2A{Enabled: true, Path: "/a2a"}
	if err := ok.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for a well-formed enabled A2A", err)
	}
}

// TestA2AEnabledRejectsPathCollidingWithExistingRoute is the Task 3
// code review's Minor 6 pin: APP_A2A_PATH must not collide with a
// route already registered under the same top-level segment (e.g.
// "/api", which r.Route("/api/v1", ...) already owns) — confirmed
// empirically (fix round 2, against the pinned chi v5.3.1) that this
// does NOT panic internal/adapters/http/server.go's r.Mount the way
// the original finding assumed; a literal route and a Mount can
// coexist at the same prefix. What it actually does is silently
// capture every deeper request under that prefix into A2A's own
// router (e.g. mounting at "/ai" would route GET /ai/tasks into A2A
// instead of a 404) — a correctness bug just as worth rejecting
// fail-fast as a panic would be. Only checked once Enabled=true, same
// as the shape guard below.
func TestA2AEnabledRejectsPathCollidingWithExistingRoute(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	for _, collision := range []string{"/api", "/api/v1", "/ai", "/ai/agents", "/", "/static", "/static/js", "/sessions"} {
		cfg := base
		cfg.A2A = A2A{Enabled: true, Path: collision}
		if err := cfg.validate(); err == nil {
			t.Errorf("validate() = nil for A2A.Path %q, want an error (collides with an existing route)", collision)
		}
	}

	disabled := base
	disabled.A2A = A2A{Enabled: false, Path: "/api"}
	if err := disabled.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil (the collision check only applies once Enabled=true)", err)
	}

	noCollision := base
	noCollision.A2A = A2A{Enabled: true, Path: "/a2a"}
	if err := noCollision.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for the non-colliding default path", err)
	}
}

// TestA2AEnabledRejectsMalformedPathShape is the Task 3 code review's
// fix-round-2 pin (Minor 6, round 2): closes the GENERAL case, not
// just the three examples the reviewer's own chi repro found. Each of
// these — confirmed empirically (fix round 2) to genuinely panic
// chi's r.Mount, not merely look suspicious — must fail config
// validation instead, with a clear "config:" error naming
// APP_A2A_PATH, before the process ever reaches r.Mount:
//   - "/a2a/{unclosed" — unclosed route-param brace
//   - "/a2a*/foo"       — bare wildcard not at the end
//   - "/a2a/{id}/{id}"  — duplicate named param
//
// Plus the shape rules those three examples are instances of: no
// leading slash, empty (""), and a trailing slash.
func TestA2AEnabledRejectsMalformedPathShape(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	malformed := []string{
		"/a2a/{unclosed",
		"/a2a*/foo",
		"/a2a/{id}/{id}",
		"",
		"a2a",
		"/a2a/",
	}
	for _, path := range malformed {
		cfg := base
		cfg.A2A = A2A{Enabled: true, Path: path}
		if err := cfg.validate(); err == nil {
			t.Errorf("validate() = nil for A2A.Path %q, want an error (malformed path shape)", path)
		}
	}

	// The root path "/" is a valid SHAPE (no metacharacters, no
	// trailing-slash violation since it IS just "/") — it's still
	// rejected, but by the collision check (it's the home page), not
	// this one. Pinning that here guards against a2aPathShapeError
	// ever accidentally rejecting "/" on shape grounds alone.
	rootShapeOK := base
	rootShapeOK.A2A = A2A{Enabled: true, Path: "/"}
	if err := rootShapeOK.validate(); err == nil {
		t.Fatal(`validate() = nil for A2A.Path "/", want an error (still rejected — but by the collision check, not the shape check; see TestA2AEnabledRejectsPathCollidingWithExistingRoute)`)
	} else if strings.Contains(err.Error(), "is invalid") {
		t.Fatalf(`validate() = %v, want the COLLISION error for "/", not the shape error`, err)
	}

	disabled := base
	disabled.A2A = A2A{Enabled: false, Path: "/a2a/{unclosed"}
	if err := disabled.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil (the shape check only applies once Enabled=true)", err)
	}
}

// TestSlackDefaults pins Slack's dormant-by-default contract: both
// tokens empty must boot clean, mirroring TestA2ADefaults/
// TestMQTTURLDefaultsEmptyAndDoesNotRequireValidation for the other
// opt-in adapters.
func TestSlackDefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.AppToken != "" || cfg.Slack.BotToken != "" {
		t.Fatalf("Slack defaults = %+v, want both tokens empty", cfg.Slack)
	}
	if cfg.Channels.AllowFrom != "" {
		t.Fatalf("Channels.AllowFrom default = %q, want empty", cfg.Channels.AllowFrom)
	}
}

// TestSlackEnvOverrides pins the APP_SLACK_APPTOKEN/APP_SLACK_BOTTOKEN/
// APP_CHANNELS_ALLOWFROM env var names exactly as documented.
func TestSlackEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SLACK_APPTOKEN", "xapp-1-test")
	t.Setenv("APP_SLACK_BOTTOKEN", "xoxb-test")
	t.Setenv("APP_CHANNELS_ALLOWFROM", "slack:U123=dev")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.AppToken != "xapp-1-test" || cfg.Slack.BotToken != "xoxb-test" {
		t.Fatalf("Slack tokens = %+v, want overrides applied", cfg.Slack)
	}
	if cfg.Channels.AllowFrom != "slack:U123=dev" {
		t.Fatalf("Channels.AllowFrom = %q, want override applied", cfg.Channels.AllowFrom)
	}
}

// TestSlackRequiresBothTokensTogether pins validate's guard: a LONE
// token (the other left at its empty default) is almost certainly a
// misconfiguration, not the valid "dormant" state — only both-empty or
// both-set are accepted.
func TestSlackRequiresBothTokensTogether(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	onlyApp := base
	onlyApp.Slack = Slack{AppToken: "xapp-1-test"}
	if err := onlyApp.validate(); err == nil {
		t.Fatal("expected a validation error for AppToken set without BotToken")
	}

	onlyBot := base
	onlyBot.Slack = Slack{BotToken: "xoxb-test"}
	if err := onlyBot.validate(); err == nil {
		t.Fatal("expected a validation error for BotToken set without AppToken")
	}

	bothEmpty := base
	if err := bothEmpty.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for the dormant both-empty state", err)
	}

	bothSet := base
	bothSet.Slack = Slack{AppToken: "xapp-1-test", BotToken: "xoxb-test"}
	if err := bothSet.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for a well-formed both-set Slack config", err)
	}
}

// TestSlackRejectsWrongTokenPrefix pins validate's second guard: each
// token, once both are set, must carry Slack's own documented prefix —
// catching a pasted-the-wrong-token mistake (e.g. swapping AppToken and
// BotToken) at boot rather than at the adapter's first, silently
// failing Socket Mode dial.
func TestSlackRejectsWrongTokenPrefix(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	swapped := base
	swapped.Slack = Slack{AppToken: "xoxb-test", BotToken: "xapp-1-test"}
	if err := swapped.validate(); err == nil {
		t.Fatal("expected a validation error for swapped Slack token prefixes")
	}

	badApp := base
	badApp.Slack = Slack{AppToken: "not-a-token", BotToken: "xoxb-test"}
	if err := badApp.validate(); err == nil {
		t.Fatal("expected a validation error for a malformed AppToken")
	}
}

// TestSignalDefaults pins Signal's dormant-by-default contract: both
// RPCURL and Number empty must boot clean, mirroring TestSlackDefaults
// for the other opt-in channel adapter.
func TestSignalDefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Signal.RPCURL != "" || cfg.Signal.Number != "" {
		t.Fatalf("Signal defaults = %+v, want both fields empty", cfg.Signal)
	}
}

// TestSignalEnvOverrides pins the APP_SIGNAL_RPCURL/APP_SIGNAL_NUMBER
// env var names exactly as documented.
func TestSignalEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SIGNAL_RPCURL", "signal-cli:6006")
	t.Setenv("APP_SIGNAL_NUMBER", "+15555550100")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Signal.RPCURL != "signal-cli:6006" || cfg.Signal.Number != "+15555550100" {
		t.Fatalf("Signal = %+v, want overrides applied", cfg.Signal)
	}
}

// TestSignalRequiresBothFieldsTogether pins validate's guard: a LONE
// value (the other left at its empty default) is almost certainly a
// misconfiguration, not the valid "dormant" state — only both-empty or
// both-set are accepted. Mirrors TestSlackRequiresBothTokensTogether.
func TestSignalRequiresBothFieldsTogether(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	onlyURL := base
	onlyURL.Signal = Signal{RPCURL: "signal-cli:6006"}
	if err := onlyURL.validate(); err == nil {
		t.Fatal("expected a validation error for RPCURL set without Number")
	}

	onlyNumber := base
	onlyNumber.Signal = Signal{Number: "+15555550100"}
	if err := onlyNumber.validate(); err == nil {
		t.Fatal("expected a validation error for Number set without RPCURL")
	}

	bothEmpty := base
	if err := bothEmpty.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for the dormant both-empty state", err)
	}

	bothSet := base
	bothSet.Signal = Signal{RPCURL: "signal-cli:6006", Number: "+15555550100"}
	if err := bothSet.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for a well-formed both-set Signal config", err)
	}
}

// TestSignalRejectsNonE164Number pins validate's second guard: Number,
// once set, must carry a leading "+" — catching a pasted-without-the-
// plus mistake at boot rather than at the adapter's first, silently
// failing "send" call. Mirrors TestSlackRejectsWrongTokenPrefix.
func TestSignalRejectsNonE164Number(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	bad := base
	bad.Signal = Signal{RPCURL: "signal-cli:6006", Number: "15555550100"}
	if err := bad.validate(); err == nil {
		t.Fatal("expected a validation error for a Number missing its leading +")
	}
}

// TestChannelsAllowFromValidation pins the config.validate() ->
// ParseAllowFrom wiring: a malformed APP_CHANNELS_ALLOWFROM must fail
// fast at boot, unconditionally (not gated behind Slack being enabled)
// — see Channels.AllowFrom's own doc comment.
func TestChannelsAllowFromValidation(t *testing.T) {
	base := Config{
		Server:   Server{Port: 8080},
		Database: Database{URL: "postgres://x"},
		Auth:     Auth{Mode: "static"},
		AI:       AI{Provider: "fake"},
	}

	malformed := base
	malformed.Channels = Channels{AllowFrom: "slack:U123-no-equals-sign"}
	if err := malformed.validate(); err == nil {
		t.Fatal("expected a validation error for a malformed APP_CHANNELS_ALLOWFROM entry")
	}

	wellFormed := base
	wellFormed.Channels = Channels{AllowFrom: "slack:U123=dev,slack:U456=alice"}
	if err := wellFormed.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for a well-formed APP_CHANNELS_ALLOWFROM", err)
	}
}

// TestParseAllowFrom exercises config.ParseAllowFrom directly: multiple
// entries parse into the exact "<channel>:<external id>" -> identity
// map application/channel.Service looks senders up in; an empty string
// parses to an empty, non-nil map (PRD §20.1's default-deny — nobody is
// allowed until explicitly listed).
func TestParseAllowFrom(t *testing.T) {
	got, err := ParseAllowFrom("slack:U123=dev, slack:U456=alice")
	if err != nil {
		t.Fatalf("ParseAllowFrom returned error: %v", err)
	}
	want := map[string]string{"slack:U123": "dev", "slack:U456": "alice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseAllowFrom = %+v, want %+v", got, want)
	}

	empty, err := ParseAllowFrom("")
	if err != nil {
		t.Fatalf("ParseAllowFrom(\"\") returned error: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("ParseAllowFrom(\"\") = %#v, want an empty non-nil map", empty)
	}

	for _, bad := range []string{"no-equals-sign", "slack=dev", "=dev", "slack:U123="} {
		if _, err := ParseAllowFrom(bad); err == nil {
			t.Errorf("ParseAllowFrom(%q) = nil error, want an error", bad)
		}
	}
}

// TestParseAllowFromRejectsDuplicateKey pins the independent review's
// Minor 1: two entries mapping the SAME "<channel>:<external id>" to
// different identities used to resolve silently (last-wins), which
// could bind a sender to the wrong learner's identity with no boot-time
// signal at all. This is the untrusted-edge allow-list — an ambiguous
// mapping must be a fail-fast config error, the same posture every
// other malformed shape here already gets, not a resolved-in-the-dark
// last-wins pick. A duplicate key with the SAME identity repeated is
// also rejected (simplest, most predictable rule: a key must appear at
// most once, full stop) — an operator who genuinely wants to list a
// mapping twice should just not.
func TestParseAllowFromRejectsDuplicateKey(t *testing.T) {
	if _, err := ParseAllowFrom("slack:U1=alice,slack:U1=bob"); err == nil {
		t.Fatal("expected an error for a duplicate \"<channel>:<external id>\" key mapped to different identities")
	}
	if _, err := ParseAllowFrom("slack:U1=alice,slack:U1=alice"); err == nil {
		t.Fatal("expected an error for a duplicate \"<channel>:<external id>\" key, even with the same identity repeated")
	}
}

// TestSpeechDefaultsEmpty pins Speech's "empty ⇒ dormant" contract
// (Phase 4 Task 8, PRD §66): neither field has a default, so an
// operator who sets nothing gets both fields empty, exactly like
// MQTT.URL/Anki.ConnectURL above.
func TestSpeechDefaultsEmpty(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Speech.STTURL != "" || cfg.Speech.TTSURL != "" {
		t.Fatalf("Speech defaults = %+v, want both fields empty", cfg.Speech)
	}
}

// TestSpeechEnvOverridesAreIndependent pins the APP_SPEECH_STTURL/
// APP_SPEECH_TTSURL env var names exactly as documented, and that
// (unlike Signal's RPCURL/Number) either can be set without the
// other — STT and TTS are independently dormant, not a paired
// all-or-nothing config.
func TestSpeechEnvOverridesAreIndependent(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SPEECH_STTURL", "http://whisper:8080")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Speech.STTURL != "http://whisper:8080" {
		t.Fatalf("Speech.STTURL = %q, want http://whisper:8080", cfg.Speech.STTURL)
	}
	if cfg.Speech.TTSURL != "" {
		t.Fatalf("Speech.TTSURL = %q, want empty (unset, independent of STTURL)", cfg.Speech.TTSURL)
	}
}
