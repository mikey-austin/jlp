package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server   Server
	Database Database
	Auth     Auth
	AI       AI
}

type Server struct {
	Port    int
	BaseURL string
}

type Database struct {
	URL string
}

type Auth struct {
	Mode           string
	TrustedProxies []string
	Static         StaticIdentity
}

type StaticIdentity struct {
	ID          string
	DisplayName string
}

type AI struct {
	Provider  string
	Anthropic Anthropic
}

type Anthropic struct {
	APIKey  string
	Model   string
	BaseURL string
}

func Load() (Config, error) {
	v := viper.New()
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.baseurl", "http://localhost:8080")
	v.SetDefault("auth.mode", "static")
	// 127.0.0.1/32 only: a broad default like 172.16.0.0/12 would trust
	// every private-network peer out of the box, re-opening the
	// hairpin-NAT spoofing hole (see docker-compose.yml /
	// deploy/compose.prod.yml, both of which override this with the
	// exact reverse-proxy address for their stack). Anyone running
	// authelia mode outside compose must set APP_AUTH_TRUSTEDPROXIES
	// explicitly to their real proxy's address.
	v.SetDefault("auth.trustedproxies", []string{"127.0.0.1/32"})
	v.SetDefault("auth.static.id", "dev")
	v.SetDefault("auth.static.displayname", "Dev Learner")
	v.SetDefault("ai.provider", "fake")
	v.SetDefault("ai.anthropic.model", "claude-sonnet-5")
	v.SetDefault("ai.anthropic.baseurl", "https://api.anthropic.com")
	v.SetDefault("database.url", "")

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv+Unmarshal quirk: bind each key explicitly so env vars land in structs.
	for _, key := range []string{"server.port", "server.baseurl", "database.url",
		"auth.mode", "auth.static.id", "auth.static.displayname",
		"ai.provider", "ai.anthropic.apikey", "ai.anthropic.model", "ai.anthropic.baseurl"} {
		if err := v.BindEnv(key); err != nil {
			return Config{}, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("config: APP_DATABASE_URL is required")
	}
	if !slices.Contains([]string{"static", "authelia"}, c.Auth.Mode) {
		return fmt.Errorf("config: APP_AUTH_MODE must be static|authelia, got %q", c.Auth.Mode)
	}
	// authelia mode authenticates purely by trusting Remote-User/Remote-Name
	// headers from a peer in this list; an empty list would mean no peer is
	// trusted (every request 401s) at best, or — if a future change ever
	// mishandled that — an unintentionally wide-open trust decision at
	// worst. Guard explicitly rather than relying on the default alone,
	// since an operator's env can still override it to empty.
	if c.Auth.Mode == "authelia" && len(c.Auth.TrustedProxies) == 0 {
		return fmt.Errorf("config: APP_AUTH_TRUSTEDPROXIES must be non-empty when APP_AUTH_MODE=authelia")
	}
	if !slices.Contains([]string{"fake", "anthropic"}, c.AI.Provider) {
		return fmt.Errorf("config: APP_AI_PROVIDER must be fake|anthropic, got %q", c.AI.Provider)
	}
	if c.AI.Provider == "anthropic" && c.AI.Anthropic.APIKey == "" {
		return fmt.Errorf("config: APP_AI_ANTHROPIC_APIKEY required when provider=anthropic")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: invalid port %d", c.Server.Port)
	}
	return nil
}
