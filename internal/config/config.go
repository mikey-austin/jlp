package config

import (
	"fmt"
	"slices"
	"strings"
	"time"

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
	Slack    Slack
	Signal   Signal
	Channels Channels
	Speech   Speech
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
	Gemini    Gemini
	ClaudeCLI ClaudeCLI
	CodexCLI  CodexCLI
	AgyCLI    AgyCLI
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
// Timeout bounds a single /api/chat round trip. A local model on cold
// weights can legitimately take a minute or more before its first
// token, so this is generous — but it must exist: the zero value of
// http.Client has no timeout at all, so a wedged Ollama server would
// hang a request (and its handler goroutine) indefinitely.
type Ollama struct {
	URL     string
	Model   string
	Timeout time.Duration
}

// ClaudeCLI configures internal/adapters/clicmd.NewClaude, the
// host-mode Claude Code CLI fallback (Task 12, PRD §23). Bin defaults
// to "claude" (see Load's viper default) — the bare command name,
// resolved via the process's PATH at call time, not construction
// time: NewClaude never errors just because the binary isn't
// installed (see the clicmd package doc comment).
//
// Model and Effort are both optional: empty means "don't pass the flag
// at all", leaving the CLI's own configured default in charge, which
// is the right default for a tool the operator has already set up for
// themselves. When set they map to `--model` and `--effort`.
type ClaudeCLI struct {
	Bin    string
	Model  string
	Effort string
}

// CodexCLI configures internal/adapters/clicmd.NewCodex, the host-mode
// OpenAI Codex CLI fallback (Task 12, PRD §23). Bin defaults to
// "codex" (see Load's viper default); same construction-never-fails
// and empty-means-CLI-default contract as ClaudeCLI above.
//
// Codex spells effort differently from Claude Code: it has no
// `--effort` flag, taking it as a config override
// (`-c model_reasoning_effort="high"`) instead, and its vocabulary is
// minimal/low/medium/high with no xhigh or max. The two are validated
// against their own CLI's real vocabulary rather than a shared
// invented one — a value only one tool accepts must not silently pass
// for the other (see claudeEfforts/codexEfforts in validate).
type CodexCLI struct {
	Bin    string
	Model  string
	Effort string
}

// AgyCLI configures internal/adapters/agycli, the host-mode Antigravity
// CLI fallback. Bin defaults to "agy"; same construction-never-fails and
// empty-means-CLI-default contract as ClaudeCLI/CodexCLI above.
//
// Timeout bounds one invocation and is also passed to the CLI as
// --print-timeout, so both sides agree on when to give up.
type AgyCLI struct {
	Bin     string
	Model   string
	Effort  string
	Timeout time.Duration
}

// Gemini configures the Google Gemini adapter
// (internal/adapters/gemini), the hosted-API provider for a deployment
// with no local model worth using. APIKey is the ONLY gate: with it
// empty the provider is never constructed and the app boots clean, the
// same dormant-unless-configured contract as Anthropic above.
//
// Model has a default (see Load) because unlike Ollama there is a
// single obvious answer that is already in production elsewhere on this
// network. Timeout bounds one generateContent round trip; the adapter
// backstops a zero value of its own, since the zero value of
// http.Client has no timeout at all.
type Gemini struct {
	APIKey  string
	Model   string
	BaseURL string
	Timeout time.Duration
}

// claudeEfforts/codexEfforts are each CLI's own accepted effort levels.
// Claude Code's come from `claude --help` ("low, medium, high, xhigh,
// max"); Codex's come from the Codex backend itself, which enumerated
// them in an error while rejecting an unknown one ("expected one of
// `none`, `minimal`, `low`, `medium`, `high`, `xhigh`") — an
// authoritative list, not a guess from documentation.
//
// They deliberately differ: "max" is meaningful to Claude Code and
// unknown to Codex, "none"/"minimal" the other way round. Validating
// each against its own tool is the point — a value only one CLI
// accepts must not silently pass for the other and fail later at
// exec time.
var (
	claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}
	codexEfforts  = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	// agy's ladder genuinely stops at high — it has no xhigh or max tier,
	// and "none" is not one either. Accepting only what the CLI takes
	// means a bad value fails at startup rather than making every routed
	// request die at exec time.
	agyEfforts = []string{"low", "medium", "high"}
)

// ClaudeEfforts and CodexEfforts return copies of each CLI's own
// accepted effort vocabulary (see claudeEfforts/codexEfforts above) —
// exported so a caller outside this package (application/settings, for
// the /settings page's effort selects — Phase 4 Task S) can populate
// its UI from the exact same list ValidateEffort below checks against,
// with no risk of a second, hand-copied list drifting out of sync.
// Copies, not the package-level slices themselves, so a caller can
// never mutate the vocabulary this package validates against.
func ClaudeEfforts() []string { return slices.Clone(claudeEfforts) }
func CodexEfforts() []string  { return slices.Clone(codexEfforts) }
func AgyEfforts() []string    { return slices.Clone(agyEfforts) }

// ValidateEffort checks value against tool's own accepted effort
// vocabulary ("claudecli" or "codexcli" — the same provider names
// config.AI's route/resolver machinery already uses). An empty value
// is always valid — it means "no effort override, let the CLI (or its
// own configured default) decide" — mirroring ClaudeCLI.Effort/
// CodexCLI.Effort's own empty-means-unset contract.
//
// This is the ONE definition of each vocabulary: validate() below and
// application/settings.Service (Phase 4 Task S's runtime override path)
// both call this rather than each keeping its own copy of
// claudeEfforts/codexEfforts — a second, hand-maintained list is
// exactly the kind of thing that quietly drifts (see claudeEfforts'
// own doc comment on why "max" and "none"/"minimal" must never be
// assumed interchangeable between the two tools).
func ValidateEffort(tool, value string) error {
	if value == "" {
		return nil
	}
	switch tool {
	case "claudecli":
		if !slices.Contains(claudeEfforts, value) {
			return fmt.Errorf("effort must be %s, got %q", strings.Join(claudeEfforts, "|"), value)
		}
	case "codexcli":
		if !slices.Contains(codexEfforts, value) {
			return fmt.Errorf("effort must be %s, got %q", strings.Join(codexEfforts, "|"), value)
		}
	case "agycli":
		if !slices.Contains(agyEfforts, value) {
			return fmt.Errorf("effort must be %s, got %q", strings.Join(agyEfforts, "|"), value)
		}
	default:
		return fmt.Errorf("unknown effort tool %q, want claudecli|codexcli|agycli", tool)
	}
	return nil
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
	// relative to this prefix. Defaults to "/a2a".
	//
	// validate() rejects two distinct problems with this value, once
	// Enabled is true (Task 3 code review, Minor 6 — see both helpers'
	// own doc comments for the full reasoning):
	//   - a2aPathShapeError: Path isn't a plain literal path at all
	//     (missing leading slash, a trailing slash, an empty segment,
	//     or a chi route-pattern metacharacter) — chi's own r.Mount
	//     panics on this shape of input.
	//   - a2aReservedPathPrefixes: Path's top-level segment collides
	//     with a route internal/adapters/http/server.go already owns —
	//     chi doesn't panic on this (confirmed against the pinned chi
	//     v5.3.1: a literal route and a Mount can coexist at the same
	//     prefix), but the mounted A2A router would then silently
	//     capture every deeper request under that prefix the literal
	//     route doesn't itself handle — e.g. mounting at "/ai" would
	//     make GET /ai/tasks resolve to A2A's own router, not a 404,
	//     without anyone intending that. server.go's own recover around
	//     its r.Mount call (mountA2A) is the last-resort net for a
	//     collision NEITHER of these two checks anticipated (e.g. a
	//     future route this list wasn't updated for) — belt and
	//     suspenders, not a substitute for either check here.
	Path string
}

// Slack configures Phase 4 Task 4's channel adapter
// (internal/adapters/slack, PRD §20/§20.1): a Socket Mode connection —
// dials OUT to Slack, so no public ingress is needed on a LAN
// deployment. AppToken and BotToken both empty (the default) keep the
// adapter entirely dormant: main.go never constructs a slack.Adapter and
// no Socket Mode connection is ever dialed, matching MQTT.URL's/
// A2A.Enabled's own "dormant unless explicitly configured" contract.
// validate() below requires both to be set together (a lone token is
// almost certainly a misconfiguration, not a valid dormant state) and,
// once both are set, that each carries Slack's own documented token
// prefix — catching a pasted-the-wrong-token mistake at boot rather than
// at the adapter's first (silently failing) Socket Mode dial.
type Slack struct {
	AppToken string
	BotToken string
	// SmokeChannel is a Slack channel or user ID `make slack-smoke`
	// (cmd/jlp/slack.go) posts one test message to, confirming BotToken
	// actually works without going through the full Socket Mode event
	// loop. Only required by that command, not by validate() here — an
	// operator who never runs `make slack-smoke` doesn't need to set it.
	SmokeChannel string
}

// Signal configures Phase 4 Task 5's channel adapter
// (internal/adapters/signal, PRD §20/§20.1): a `signal-cli` sidecar run
// in JSON-RPC daemon mode over a plain TCP socket (docker-compose.yml's
// "signal" profile — not started by default; see `make up-signal`),
// with NO account baked into the sidecar's own startup command — every
// JSON-RPC call the adapter makes names Number as the request's
// "account" param instead, so the same sidecar could, in principle,
// serve more than one linked number. RPCURL and Number both empty (the
// default) keep the adapter entirely dormant: main.go never constructs
// a signal.Adapter and no TCP connection is ever dialed, matching
// Slack's own "dormant unless explicitly configured" contract above.
// validate() below requires both to be set together (a lone value is
// almost certainly a misconfiguration, same as Slack's paired-token
// check) and, once Number is set, that it looks like an E.164 phone
// number (leading "+") — catching a pasted-without-the-plus mistake at
// boot rather than at the adapter's first, silently failing "send"
// call.
//
// Linking a device (`signal-cli link`) requires scanning a QR/URI with
// the user's own phone — see deploy/signal/README.md and `make
// signal-register` — so this repo can build and unit-test the adapter,
// but cannot itself complete registration or verify a live send/
// receive; see that README's own "what wasn't verified" section.
type Signal struct {
	// RPCURL is the signal-cli daemon's JSON-RPC TCP address, host:port
	// (e.g. "signal-cli:6006", docker-compose.yml's "signal" profile
	// default) — no scheme, no path: internal/adapters/signal dials it
	// directly with net.Dialer, the same way MQTT.URL is a bare broker
	// URI for a different transport (see that field's own doc comment).
	RPCURL string
	// Number is the linked Signal account's own E.164 phone number
	// (e.g. "+15555550100"), sent as the "account" param on every
	// JSON-RPC call — required because the sidecar's daemon is started
	// WITHOUT a `-a` account of its own (see this struct's doc comment).
	Number string
}

// Speech configures Phase 4 Task 8's STT/TTS capability (PRD §66):
// STTURL and TTSURL are each independently dormant-unless-configured,
// the same "empty ⇒ absent" contract as MQTT.URL/Anki.ConnectURL/
// Signal.RPCURL above — an operator who sets neither gets zero HTTP
// calls to either sidecar, ever, and POST /speech/transcribe answers
// 503 rather than panicking or silently no-opping (see
// application/speech.Service's ErrNotConfigured).
//
// STTURL points at a local whisper.cpp `whisper-server` instance
// (docker-compose.yml's "speech" profile pins the exact image —
// ghcr.io/ggml-org/whisper.cpp — and the `--convert` flag that lets it
// accept a browser MediaRecorder's webm/opus blobs directly). TTSURL
// points at a local VOICEVOX Engine instance — a real, working
// Japanese speech synthesizer, not a stub (see internal/adapters/tts's
// package doc comment for why this task did NOT give TTS the
// dormant-shell treatment WhatsApp got in Task 5: a working local
// engine turned out to be available, so building a non-functional
// stub instead would have been the dishonest choice). Task 8's own
// HTTP route only ever calls STTURL's recognizer — TTSURL exists,
// tested, and wireable, but nothing in this task's UI calls Speak yet.
type Speech struct {
	STTURL string
	TTSURL string
}

// Channels configures the transport-agnostic channel port (Phase 4 Task
// 4, PRD §20/§20.1) application/channel.Service composes on top of the
// existing sessions/feedback/practice application services.
type Channels struct {
	// AllowFrom is APP_CHANNELS_ALLOWFROM: a comma-separated list of
	// "<channel>:<external id>=<identity>" entries (e.g.
	// "slack:U012ABCDEF=dev,slack:U099XYZAB=alice"), each mapping one
	// channel-specific sender to a JLP learner.IdentityID. This is the
	// UNTRUSTED EDGE of the system (PRD §20.1): an inbound sender whose
	// "<channel>:<external id>" isn't a key in this map gets a polite
	// refusal from application/channel.Service.Handle and NOTHING is
	// recorded — no session, no events, no learner-model updates. The
	// mapping is an explicit allow-list, never inferred or
	// auto-provisioned from the message itself.
	AllowFrom string
}

// a2aPathShapeError reports why p isn't an acceptable APP_A2A_PATH
// shape, or "" if it is. This is a STRUCTURAL check — it has nothing
// to do with what routes already exist (see a2aReservedPathPrefixes
// below for that) — closing the general case the Task 3 code review's
// Minor 6 follow-up asked for: any chi route-pattern metacharacter
// ('{', '}', '*', ':') anywhere in Path reliably panics chi's own
// r.Mount (confirmed empirically against the pinned chi v5.3.1 for
// an unclosed '{', a non-trailing '*', and a duplicate '{id}' — see
// the fix-round commit), not just the three specific examples that
// panic finds; this check rejects the whole character class, not an
// enumerated list of bad strings.
func a2aPathShapeError(p string) string {
	if !strings.HasPrefix(p, "/") {
		return `must start with "/"`
	}
	if strings.ContainsAny(p, "{}*:") {
		return `must not contain a chi route-pattern character ({, }, *, or :)`
	}
	if p == "/" {
		return ""
	}
	if strings.HasSuffix(p, "/") {
		return `must not end with "/"`
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" {
			return "must not contain an empty path segment"
		}
	}
	return ""
}

// a2aReservedPathPrefixes is every top-level path segment
// internal/adapters/http/server.go already routes, as of this
// writing: "" is the home page ("/" itself), the rest are each
// route's first path segment (e.g. "/ai" covers both "/ai" and
// "/ai/agents/{id}"; "/api" covers the whole "/api/v1/..." subtree).
// validate() rejects APP_A2A_PATH when Enabled and its own first
// segment is in this set — see A2A.Path's own doc comment for exactly
// what failure mode this prevents (silent route shadowing, not a
// panic — that distinction is why this check exists ALONGSIDE
// server.go's mountA2A recover rather than instead of it: a recover
// can only catch a panic chi actually raises, and a shadowing
// collision never raises one). Keep in sync with server.go's routes()
// if its top-level route list ever changes; a route added there
// without a matching entry here is exactly the gap mountA2A's recover
// exists to catch instead.
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
	"settings":    true,
}

// firstPathSegment returns p's first "/"-delimited segment (no leading
// or trailing slash) — "" for "/" itself, "api" for both "/api" and
// "/api/v1/words". Assumes p already starts with "/" (validate only
// calls this after a2aPathShapeError has already confirmed that).
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
	v.SetDefault("ai.ollama.timeout", 5*time.Minute)
	// ai.ollama.model has no default (see the Ollama struct's doc
	// comment) — it's zero-value "" unless the operator sets it.
	v.SetDefault("ai.ollama.url", "http://ollama:11434")
	// gemini-3-flash-preview is the model already in production against
	// this same key elsewhere on the network (nihongo-daily) and the one
	// every measurement behind internal/adapters/gemini was taken
	// against. ai.gemini.apikey has no default — its emptiness IS the
	// "provider is dormant" contract.
	v.SetDefault("ai.gemini.model", "gemini-3-flash-preview")
	v.SetDefault("ai.gemini.baseurl", "https://generativelanguage.googleapis.com")
	v.SetDefault("ai.gemini.timeout", 2*time.Minute)
	v.SetDefault("ai.claudecli.bin", "claude")
	v.SetDefault("ai.codexcli.bin", "codex")
	v.SetDefault("ai.agycli.bin", "agy")
	v.SetDefault("ai.agycli.timeout", 3*time.Minute)
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
	// slack.* and channels.allowfrom have no explicit defaults (Go's
	// zero-value empty string IS the "dormant"/"nobody allowed" contract
	// — see Slack's and Channels.AllowFrom's own doc comments).

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv+Unmarshal quirk: bind each key explicitly so env vars land in structs.
	for _, key := range []string{"server.port", "server.baseurl", "database.url",
		"auth.mode", "auth.static.id", "auth.static.displayname",
		"ai.provider", "ai.anthropic.apikey", "ai.anthropic.model", "ai.anthropic.baseurl",
		"ai.ollama.url", "ai.ollama.model", "ai.ollama.timeout",
		"ai.gemini.apikey", "ai.gemini.model", "ai.gemini.baseurl", "ai.gemini.timeout",
		"ai.claudecli.bin", "ai.claudecli.model", "ai.claudecli.effort",
		"ai.codexcli.bin", "ai.codexcli.model", "ai.codexcli.effort",
		"ai.agycli.bin", "ai.agycli.model", "ai.agycli.effort", "ai.agycli.timeout",
		"ai.routes", "ai.agenticteacher",
		"summary.enabled", "summary.cron", "summary.to", "summary.from", "smtp.addr", "mqtt.url",
		"a2a.enabled", "a2a.path",
		"slack.apptoken", "slack.bottoken", "slack.smokechannel",
		"signal.rpcurl", "signal.number", "channels.allowfrom",
		"speech.stturl", "speech.ttsurl"} {
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
	// The APP_AI_PROVIDER set is narrower than routeProviders below on
	// purpose: a DEFAULT provider must implement ai.ToolCaller as well as
	// ai.StructuredGenerator, because cmd/jlp builds both from this one
	// value and treats a default it cannot build a ToolCaller for as a
	// boot error. The three CLI providers are structured-generation only,
	// so they can be named in a route chain but never as the default.
	if !slices.Contains([]string{"fake", "anthropic", "ollama", "gemini"}, c.AI.Provider) {
		return fmt.Errorf("config: APP_AI_PROVIDER must be fake|anthropic|ollama|gemini, got %q", c.AI.Provider)
	}
	if c.AI.Provider == "anthropic" && c.AI.Anthropic.APIKey == "" {
		return fmt.Errorf("config: APP_AI_ANTHROPIC_APIKEY required when provider=anthropic")
	}
	if c.AI.Provider == "ollama" && c.AI.Ollama.Model == "" {
		return fmt.Errorf("config: APP_AI_OLLAMA_MODEL required when provider=ollama")
	}
	if c.AI.Provider == "gemini" && c.AI.Gemini.APIKey == "" {
		return fmt.Errorf("config: APP_AI_GEMINI_APIKEY required when provider=gemini")
	}
	if _, err := ParseRoutes(c.AI.Routes); err != nil {
		return err
	}
	// Effort vocabularies differ per CLI and are validated against each
	// tool's own set: passing an unsupported level would otherwise only
	// surface as a per-call exec failure, long after boot, on whichever
	// prompt happened to route there first.
	if err := ValidateEffort("claudecli", c.AI.ClaudeCLI.Effort); err != nil {
		return fmt.Errorf("config: APP_AI_CLAUDECLI_EFFORT: %w", err)
	}
	if err := ValidateEffort("codexcli", c.AI.CodexCLI.Effort); err != nil {
		return fmt.Errorf("config: APP_AI_CODEXCLI_EFFORT: %w", err)
	}
	if err := ValidateEffort("agycli", c.AI.AgyCLI.Effort); err != nil {
		return fmt.Errorf("config: APP_AI_AGYCLI_EFFORT: %w", err)
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
	// See A2A.Path's own doc comment for what each of these two checks
	// closes and why both are needed alongside server.go's mountA2A
	// recover, not instead of it.
	if c.A2A.Enabled {
		if msg := a2aPathShapeError(c.A2A.Path); msg != "" {
			return fmt.Errorf("config: APP_A2A_PATH %q is invalid: %s", c.A2A.Path, msg)
		}
		if seg := firstPathSegment(c.A2A.Path); a2aReservedPathPrefixes[seg] {
			return fmt.Errorf("config: APP_A2A_PATH %q collides with an existing route (\"/%s\") — choose a different mount path", c.A2A.Path, seg)
		}
	}
	// Slack: both tokens empty is the valid "dormant" state (see Slack's
	// doc comment) — only a LONE token, or a token that doesn't carry
	// Slack's own documented prefix, is rejected.
	if (c.Slack.AppToken == "") != (c.Slack.BotToken == "") {
		return fmt.Errorf("config: APP_SLACK_APPTOKEN and APP_SLACK_BOTTOKEN must both be set, or both left empty to keep the Slack adapter dormant")
	}
	if c.Slack.AppToken != "" {
		if !strings.HasPrefix(c.Slack.AppToken, "xapp-") {
			return fmt.Errorf("config: APP_SLACK_APPTOKEN must be a Socket Mode app-level token (starts with \"xapp-\")")
		}
		if !strings.HasPrefix(c.Slack.BotToken, "xoxb-") {
			return fmt.Errorf("config: APP_SLACK_BOTTOKEN must be a bot token (starts with \"xoxb-\")")
		}
	}
	// Signal: both empty is the valid "dormant" state (see Signal's doc
	// comment) — only a LONE value, or a Number that doesn't look like
	// an E.164 number, is rejected. Mirrors the Slack pairing check
	// immediately above.
	if (c.Signal.RPCURL == "") != (c.Signal.Number == "") {
		return fmt.Errorf("config: APP_SIGNAL_RPCURL and APP_SIGNAL_NUMBER must both be set, or both left empty to keep the Signal adapter dormant")
	}
	if c.Signal.Number != "" && !strings.HasPrefix(c.Signal.Number, "+") {
		return fmt.Errorf("config: APP_SIGNAL_NUMBER must be an E.164 phone number (starts with \"+\")")
	}
	// APP_CHANNELS_ALLOWFROM is validated unconditionally (not gated
	// behind Slack or any other channel being enabled): it's cheap to
	// parse, channel-agnostic, and a malformed entry here is exactly the
	// kind of thing that should fail fast at boot rather than silently
	// deny every sender once a channel adapter is later turned on — see
	// application/channel.NewService, which re-parses this same string.
	if _, err := ParseAllowFrom(c.Channels.AllowFrom); err != nil {
		return err
	}
	return nil
}

// ParseAllowFrom parses APP_CHANNELS_ALLOWFROM (see Channels.AllowFrom's
// doc comment): comma-separated "<channel>:<external id>=<identity>"
// entries into a map keyed "<channel>:<external id>" -> identity. An
// empty string parses to an empty (non-nil) map — PRD §20.1's
// default-deny: no channel sender is allowed until an operator
// explicitly lists them.
//
// A "<channel>:<external id>" key listed more than once is a fail-fast
// error, even when every occurrence names the same identity: this is
// the untrusted edge of the system (see Channels.AllowFrom's doc
// comment), so an ambiguous mapping — independent review, Minor 1 —
// must never resolve silently (previously: last-wins), which could bind
// a sender to the wrong learner's identity with no boot-time signal at
// all. Every other malformed shape in this function already fails fast
// the same way; this closes the one case that didn't.
func ParseAllowFrom(s string) (map[string]string, error) {
	out := map[string]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, identity, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		identity = strings.TrimSpace(identity)
		if !ok || key == "" || identity == "" {
			return nil, fmt.Errorf("config: invalid APP_CHANNELS_ALLOWFROM entry %q, want <channel>:<external id>=<identity>", entry)
		}
		channel, externalID, ok := strings.Cut(key, ":")
		channel = strings.TrimSpace(channel)
		externalID = strings.TrimSpace(externalID)
		if !ok || channel == "" || externalID == "" {
			return nil, fmt.Errorf("config: invalid APP_CHANNELS_ALLOWFROM entry %q, want <channel>:<external id>=<identity>", entry)
		}
		mapKey := channel + ":" + externalID
		if _, dup := out[mapKey]; dup {
			return nil, fmt.Errorf("config: APP_CHANNELS_ALLOWFROM lists %q more than once — an ambiguous identity mapping is not allowed", mapKey)
		}
		out[mapKey] = identity
	}
	return out, nil
}

// routeProviders is the set of provider names ParseRoutes accepts —
// wider than APP_AI_PROVIDER's own fake|anthropic|ollama|gemini enum
// (see validate above) because a route may name a CLI adapter
// (claudecli, codexcli) that Task 12 makes constructible. ParseRoutes
// only checks the NAME is spellable; whether cmd/jlp/main.go can
// actually build an instance for it is a separate, later check (a boot
// error there, not a config-parse error here) — see the AI.Routes field
// comment.
var routeProviders = []string{"fake", "anthropic", "ollama", "gemini", "claudecli", "codexcli", "agycli"}

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
