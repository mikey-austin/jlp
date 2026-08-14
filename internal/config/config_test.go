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
