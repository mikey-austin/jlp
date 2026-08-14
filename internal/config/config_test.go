package config

import "testing"

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
		"missing db url":        {"APP_DATABASE_URL": ""},
		"bad auth mode":         {"APP_DATABASE_URL": "postgres://x", "APP_AUTH_MODE": "oauth"},
		"anthropic without key": {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "anthropic"},
		"unknown ai provider":   {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "hal9000"},
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
