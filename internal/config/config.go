package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
)

type Config struct {
	Server   Server
	Database Database
	Auth     Auth
	AI       AI
	Anki     Anki
	Summary  Summary
	SMTP     SMTP
	MQTT     MQTT
	A2A      A2A
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
	Ollama    Ollama
	ClaudeCLI ClaudeCLI
	CodexCLI  CodexCLI
	// Routes is the raw APP_AI_ROUTES string — "prompt.name=prov1,prov2;
	// other.name=prov" — parsed by ParseRoutes. Kept as a string here
	// (validate below only checks it parses, fail-fast, same as every
	// other field) rather than pre-parsed into a map: main.go calls
	// ParseRoutes again itself to get the map it actually wires up,
	// since that's also where a route naming a provider that isn't
	// constructible (e.g. a Task 12 CLI adapter, or ollama without a
	// model) becomes a boot error.
	Routes string
	// AgenticTeacher opts into Phase 4 Task 2's agentic teacher path
	// (PRD §27/§50, APP_AI_AGENTICTEACHER): when true,
	// application/feedback.Service.RequestFeedback drives a
	// tool-calling investigation (application/agentrun.Runner over
	// internal/tools.Registry) before asking for corrections, instead
	// of going straight to the single-shot structured-generation call.
	// Defaults false — Go's bool zero value already is the "keep Phase
	// 1's behaviour and cost unchanged" contract, same as
	// Summary.Enabled's own doc comment explains for the same pattern.
	AgenticTeacher bool
}

type Anthropic struct {
	APIKey  string
	Model   string
	BaseURL string
}

// Ollama configures the local-model adapter (internal/adapters/ollama).
// Model has no default: it's only required when Ollama is actually
// used, either as APP_AI_PROVIDER or named in an APP_AI_ROUTES chain
// (see validate and cmd/jlp/main.go's provider construction).
type Ollama struct {
	URL   string
	Model string
}

// ClaudeCLI configures internal/adapters/clicmd.NewClaude, the
// host-mode Claude Code CLI fallback (Task 12, PRD §23). Bin defaults
// to "claude" (see Load's viper default) — the bare command name,
// resolved via the process's PATH at call time, not construction
// time: NewClaude never errors just because the binary isn't
// installed (see the clicmd package doc comment), so this struct
// carries no other fields to validate.
type ClaudeCLI struct {
	Bin string
}

// CodexCLI configures internal/adapters/clicmd.NewCodex, the host-mode
// OpenAI Codex CLI fallback (Task 12, PRD §23). Bin defaults to
// "codex" (see Load's viper default); same construction-never-fails
// contract as ClaudeCLI above.
type CodexCLI struct {
	Bin string
}

// Anki configures the optional AnkiConnect push (PRD §19,
// internal/adapters/ankiconnect). ConnectURL empty (the default) means
// the feature is dormant: main.go never constructs an ankiconnect.Client,
// application/anki.Service.PushToAnkiConnect always returns
// ErrAnkiConnectNotConfigured, and the /anki page's 「Ankiへ送信」 button
// is never rendered — validate below deliberately does NOT require this
// field, since TSV export (the other export path) works with no
// AnkiConnect configuration at all.
type Anki struct {
	ConnectURL string
}

// Summary configures the weekly email summary (Phase 3 Task 5, PRD
// §21, §65). Enabled false — the default — keeps the feature entirely
// dormant: main.go never constructs a robfig/cron scheduler and no
// outbound email is ever possible, matching Anki.ConnectURL's own
// "dormant by default" contract above. This is JLP's one piece of
// automatic external communication, so it is opt-in, explicitly, per
// PRD §65 — an operator must set APP_SUMMARY_ENABLED=true themselves.
//
// The manual `jlp send-summary` CLI command (cmd/jlp/summary.go)
// bypasses Enabled on purpose: a human running that command IS the
// opt-in act, the same way clicking a "send now" button would be — it
// only requires To to be set (validate below does not require it; see
// runSendSummary's own fail-fast check).
//
// From is currently INFORMATIONAL ONLY — setting it has no effect on
// outbound mail. internal/adapters/smtp — the deliberately minimal,
// no-auth, LAN/Mailpit-oriented adapter this task ships — always sends
// with a fixed envelope/header sender, since notifications.Notification
// (the port every Notifier implements) carries no From field by
// design: a future channel (Slack, SMS) has no use for an email-shaped
// sender address, so it isn't part of the transport-agnostic port.
// From is reserved here, unused, for wiring into a configurable-sender
// transport later — cmd/jlp/summary.go deliberately does NOT log it
// alongside cron/to, so as not to imply it currently does anything.
type Summary struct {
	Enabled bool
	Cron    string
	To      string
	From    string
}

// SMTP configures the outbound-email adapter (internal/adapters/smtp).
// Addr is host:port — "mailpit:1025" by default, the docker-compose
// "mail" profile's in-network address (see docker-compose.yml and
// `make up-mail`); a production deployment would point this at a real
// relay. Not validated as required below: Summary.Enabled false means
// this is never dialed at all, exactly like Anki.ConnectURL.
type SMTP struct {
	Addr string
}

// MQTT configures the optional MQTT event bridge
// (internal/adapters/mqtt, Phase 3 Task 6, PRD §31-33/§12/§59). URL
// empty — the default — keeps the bridge entirely dormant: main.go
// never constructs a mqtt.Bridge, no broker connection is ever
// attempted, and no learning event is ever published outside the
// process. This mirrors Anki.ConnectURL's and Summary's own
// "dormant unless explicitly configured" contract above — not
// validated as required by validate below for the same reason.
//
// URL is a paho broker URI, e.g. "tcp://mosquitto:1883" — see
// docker-compose.yml's "mqtt" profile and `make up-mqtt`, which set
// it to exactly that for the in-network mosquitto service.
type MQTT struct {
	URL string
}

// A2A configures Phase 4 Task 3's protocol adapter
// (internal/adapters/a2a, PRD §29/§30, Rule 13): exposes selected
// agents as A2A "skills" over HTTP+JSON, each running through the
// exact same application/agentrun.Runner + internal/tools.Registry
// permissions every local agent-run already goes through — a remote
// caller gets no privilege a local agent lacks. Enabled false (the
// default) keeps the feature entirely dormant: main.go never
// constructs an a2a.Server and the routes it would otherwise expose
// are absent (404), not merely guarded — the same "dormant unless
// explicitly configured" contract Anki.ConnectURL/Summary.Enabled/
// MQTT.URL above already establish.
type A2A struct {
	Enabled bool
	// Path is where the adapter's routes are mounted, inside the
	// authenticated group internal/adapters/http/server.go already
	// establishes (so Authelia/CSRF posture is unchanged) — see
	// a2a.Server.Routes' own doc comment for the routes themselves,
	// relative to this prefix. Defaults to "/a2a". validate() rejects a
	// Path whose top-level segment collides with one of that package's
	// own routes (a2aReservedPathPrefixes below) — see validate's own
	// comment on why that check exists.
	Path string
}

// a2aReservedPathPrefixes is every top-level path segment
// internal/adapters/http/server.go already routes, as of this
// writing: "" is the home page ("/" itself), the rest are each
// route's first path segment (e.g. "/ai" covers both "/ai" and
// "/ai/agents/{id}"; "/api" covers the whole "/api/v1/..." subtree).
// validate() rejects APP_A2A_PATH when Enabled and its own first
// segment is in this set — see that call site's doc comment for why
// (chi's r.Mount panics on the collision instead of failing
// gracefully). Keep in sync with server.go's routes() if its top-level
// route list ever changes.
var a2aReservedPathPrefixes = map[string]bool{
	"":            true, // "/" itself
	"healthz":     true,
	"offline":     true,
	"static":      true,
	"sessions":    true,
	"corrections": true,
	"documents":   true,
	"ai":          true,
	"ratings":     true,
	"grammar":     true,
	"learner":     true,
	"vocabulary":  true,
	"practice":    true,
	"anki":        true,
	"lessons":     true,
	"api":         true,
}

// firstPathSegment returns p's first "/"-delimited segment (no leading
// or trailing slash) — "" for "/" itself, "api" for both "/api" and
// "/api/v1/words". Assumes p already starts with "/" (validate only
// calls this after that's confirmed).
func firstPathSegment(p string) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	return seg
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
	// ai.ollama.model has no default (see the Ollama struct's doc
	// comment) — it's zero-value "" unless the operator sets it.
	v.SetDefault("ai.ollama.url", "http://ollama:11434")
	v.SetDefault("ai.claudecli.bin", "claude")
	v.SetDefault("ai.codexcli.bin", "codex")
	v.SetDefault("database.url", "")
	// summary.enabled has no explicit default (Go's bool zero value,
	// false, IS the "dormant by default" contract — see Summary's doc
	// comment); summary.to/summary.from have no default either (empty
	// unless the operator sets them, same as ai.ollama.model above).
	// summary.cron's default is the brief's exact Sunday 18:00 weekly
	// cadence.
	v.SetDefault("summary.cron", "0 18 * * 0")
	v.SetDefault("smtp.addr", "mailpit:1025")
	// a2a.enabled has no explicit default (Go's bool zero value, false,
	// IS the "dormant by default" contract — see A2A's doc comment);
	// a2a.path defaults to "/a2a" regardless of Enabled, so an operator
	// who only sets APP_A2A_ENABLED=true doesn't also have to pick a
	// mount path.
	v.SetDefault("a2a.path", "/a2a")

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv+Unmarshal quirk: bind each key explicitly so env vars land in structs.
	for _, key := range []string{"server.port", "server.baseurl", "database.url",
		"auth.mode", "auth.static.id", "auth.static.displayname",
		"ai.provider", "ai.anthropic.apikey", "ai.anthropic.model", "ai.anthropic.baseurl",
		"ai.ollama.url", "ai.ollama.model", "ai.claudecli.bin", "ai.codexcli.bin", "ai.routes", "ai.agenticteacher",
		"summary.enabled", "summary.cron", "summary.to", "summary.from", "smtp.addr", "mqtt.url",
		"a2a.enabled", "a2a.path"} {
		if err := v.BindEnv(key); err != nil {
			return Config{}, err
		}
	}
	// anki.connecturl is bound to an explicit env var name (rather than
	// the automatic ANKI_CONNECTURL the loop above's replacer would
	// derive) so the operator-facing variable reads as
	// APP_ANKI_CONNECT_URL — see .env.example and docker-compose.yml,
	// which document/pass it through under that exact name.
	if err := v.BindEnv("anki.connecturl", "APP_ANKI_CONNECT_URL"); err != nil {
		return Config{}, err
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
	if !slices.Contains([]string{"fake", "anthropic", "ollama"}, c.AI.Provider) {
		return fmt.Errorf("config: APP_AI_PROVIDER must be fake|anthropic|ollama, got %q", c.AI.Provider)
	}
	if c.AI.Provider == "anthropic" && c.AI.Anthropic.APIKey == "" {
		return fmt.Errorf("config: APP_AI_ANTHROPIC_APIKEY required when provider=anthropic")
	}
	if c.AI.Provider == "ollama" && c.AI.Ollama.Model == "" {
		return fmt.Errorf("config: APP_AI_OLLAMA_MODEL required when provider=ollama")
	}
	if _, err := ParseRoutes(c.AI.Routes); err != nil {
		return err
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: invalid port %d", c.Server.Port)
	}
	// Summary.To/SMTP.Addr are validated ONLY when the scheduler is
	// actually turned on — mirrors validate's own authelia/trustedproxies
	// guard just above: a feature's config only needs to make sense once
	// the feature is live. Enabled=false (the default) skips this
	// entirely, so an operator who never touches APP_SUMMARY_* still
	// boots clean — see Summary's doc comment and
	// TestSummaryDefaults/TestSummaryEnabledRequiresRecipientAndValidCron
	// in config_test.go.
	if c.Summary.Enabled {
		if c.Summary.To == "" {
			return fmt.Errorf("config: APP_SUMMARY_TO is required when APP_SUMMARY_ENABLED=true")
		}
		if _, err := cron.ParseStandard(c.Summary.Cron); err != nil {
			return fmt.Errorf("config: invalid APP_SUMMARY_CRON %q: %w", c.Summary.Cron, err)
		}
	}
	// A2A.Path is only validated once the adapter is actually live —
	// same "a feature's config only needs to make sense once the
	// feature is live" pattern Summary.Enabled's guard above uses; an
	// operator who never sets APP_A2A_ENABLED never has this checked
	// at all, even if APP_A2A_PATH was somehow set to something odd.
	if c.A2A.Enabled {
		if !strings.HasPrefix(c.A2A.Path, "/") {
			return fmt.Errorf("config: APP_A2A_PATH must start with \"/\" when APP_A2A_ENABLED=true, got %q", c.A2A.Path)
		}
		// A path whose top-level segment collides with an
		// already-mounted route makes internal/adapters/http/server.go's
		// r.Mount PANIC at boot (Task 3 code review, Minor 6) instead of
		// failing here with a clean error — reject it fail-fast instead,
		// the same way every other cross-field A2A check in this
		// function does. a2aReservedPathPrefixes is that package's own
		// top-level routes; keep the two in sync if server.go's route
		// list ever changes.
		if seg := firstPathSegment(c.A2A.Path); a2aReservedPathPrefixes[seg] {
			return fmt.Errorf("config: APP_A2A_PATH %q collides with an existing route (\"/%s\") — choose a different mount path", c.A2A.Path, seg)
		}
	}
	return nil
}

// routeProviders is the set of provider names ParseRoutes accepts —
// wider than APP_AI_PROVIDER's own fake|anthropic|ollama enum (see
// validate above) because a route may name a CLI adapter (claudecli,
// codexcli) that Task 12 makes constructible. ParseRoutes only checks
// the NAME is spellable; whether cmd/jlp/main.go can actually build an
// instance for it is a separate, later check (a boot error there, not
// a config-parse error here) — see the AI.Routes field comment.
var routeProviders = []string{"fake", "anthropic", "ollama", "claudecli", "codexcli"}

// ParseRoutes parses APP_AI_ROUTES: semicolon-separated
// "prompt.name=prov1,prov2" entries, each naming an ordered fallback
// chain of providers for one prompt name. An empty string parses to an
// empty (non-nil) map — no routes configured, every prompt uses the
// default APP_AI_PROVIDER fallback chain main.go builds.
func ParseRoutes(s string) (map[string][]string, error) {
	routes := map[string][]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return routes, nil
	}
	for _, entry := range strings.Split(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, chainStr, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("config: invalid APP_AI_ROUTES entry %q, want prompt.name=prov1,prov2", entry)
		}
		var chain []string
		for _, p := range strings.Split(chainStr, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !slices.Contains(routeProviders, p) {
				return nil, fmt.Errorf("config: APP_AI_ROUTES entry %q: unknown provider %q (want one of %v)", entry, p, routeProviders)
			}
			chain = append(chain, p)
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("config: APP_AI_ROUTES entry %q has no providers", entry)
		}
		routes[name] = chain
	}
	return routes, nil
}
