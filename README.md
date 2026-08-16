# JLP — Japanese Learning Platform

Write Japanese, get AI-backed corrections with deterministic diffs, accept
or reject them, and watch your learner statistics build up over time.
Phase 1 MVP: sessions, a writing editor with autosave, AI structured
feedback, learning events, basic statistics, AI observability, and a
JSON API — all run locally via `make` on Docker Compose.

## Quickstart

```sh
make init      # copy .env.example -> .env, generate Authelia dev users
make build     # build the app image
make up        # start postgres + app (hot reload via air)
make migrate   # apply database migrations
make seed      # create a demo identity, session, and document
```

Then open **http://localhost:8080**.

If port 8080 is already taken on your machine, set `APP_HOST_PORT` in
`.env` (e.g. `APP_HOST_PORT=28080`) before `make up` and open
`http://localhost:$APP_HOST_PORT` instead — the container always listens
on 8080 internally, only the published host port changes.

## Everyday commands (`make help`)

```
help               Show available commands
init               One-time setup: create .env from example, generate Authelia dev users
build              Build all images
up                 Start the dev stack (app + postgres)
up-auth            Start dev stack including Caddy + Authelia (https://jlp.localhost:8443)
up-mail            Start the dev stack plus Mailpit (SMTP capture UI at http://localhost:8025) for the weekly summary
up-mqtt            Start the dev stack plus mosquitto (MQTT event bridge, PRD §31-33/§12/§59), app pointed at it
up-signal          Start the dev stack plus the signal-cli JSON-RPC sidecar (Signal channel adapter, PRD §20/§20.1); set APP_SIGNAL_RPCURL + APP_SIGNAL_NUMBER together in .env once a device is linked (see deploy/signal/README.md), then `make restart`
signal-register    One-time Signal device link: prints a QR/URI to scan with the Signal app on the account's phone (needs `make up-signal` first; see deploy/signal/README.md)
ollama-pull        Pull a local model into the ollama service (m=qwen3:4b), starting it if needed
a2a-chat           Start the dev stack plus the generic A2A chat client (http://localhost:8090), app's A2A adapter enabled; set A2A_AGENT_URL to point it at a different agent instead (see clients/a2a-chat/README.md)
down               Stop the stack (including profile-gated services like Caddy/Authelia)
restart            Restart the app service
logs               Follow logs (s=<service>, default app)
ps                 Show stack status
test               Run unit + application tests (no services needed)
test-race          Run unit + application tests with the race detector (no services needed; catches goroutine data races `make test` alone misses)
lint               Run all linters (govet, staticcheck, errcheck, ineffassign, unused, depguard)
fmt                gofmt the whole tree
arch-check         Enforce PRD §75 dependency-direction rules only (depguard)
tidy               go mod tidy inside the container
clean              Stop stack and remove volumes + build artifacts
migrate            Apply database migrations
seed               Populate a dev-friendly identity, session, and document (idempotent)
rebuild-model      Recompute every identity's learner_observations from learning_events (safe to rerun)
eval               Run the Japanese-correction eval corpus against the configured AI provider; regression-flags vs the previous report (PRD §48/§49)
migrate-new        Create a migration (n=short_name)
sqlc               Regenerate sqlc query code
db-shell           psql into the dev database
test-integration   Adapter tests against compose services
vendor-js          Vendor pinned htmx + alpine into web/static/js
vendor-fonts       Vendor pinned Instrument Sans + JetBrains Mono woff2 into web/static/fonts (design system, PRD §39/§45: no CDN fonts at runtime)
ext-build          Zip chrome-extension/ (excluding shim/ and README) into dist/jlp-extension.zip
send-summary       Trigger one weekly summary send immediately (needs APP_SUMMARY_TO set; brings up Mailpit + postgres first)
mqtt-tap           Tail every learner/# MQTT topic (needs `make up-mqtt` first)
mqtt-demo          Publish a sample vocabulary.lookup ingest event over MQTT (needs `make up-mqtt` first); appears on /vocabulary for the "dev" identity
slack-smoke        Post one test message via the Slack bot token (needs APP_SLACK_BOTTOKEN + APP_SLACK_SMOKECHANNEL set; no live Slack test runs in `make test`)
deploy-local       Run the production stack locally (https://<JLP_DOMAIN>:8444, see deploy/.env.prod)
deploy             Deploy to $(DEPLOY_HOST) over SSH (set in .env)
deploy-logs        Tail remote app logs
```

Every command goes through `make` and runs inside containers — there is
no host-installed Go, golangci-lint, or sqlc requirement. If you need a
one-off Go/psql/sqlc command that isn't already a target, add one rather
than reaching for `docker compose run` directly.

## Auth modes

Two `auth.Authenticator` adapters are wired behind `APP_AUTH_MODE`:

- **`static` (default)** — every request is the same dev identity
  (`dev` / `Dev Learner`, configurable via `APP_AUTH_STATIC_ID` /
  `APP_AUTH_STATIC_DISPLAYNAME`). No login flow, no certificates —
  just `make up` and go straight to `http://localhost:8080`.
- **`authelia`** — real forward-auth login via Caddy + Authelia. Run
  `make up-auth` and open **https://jlp.localhost:8443**. The dev TLS
  cert is self-signed, so the browser will show a certificate warning on
  first visit — click through it (or add the cert to your trust store)
  to reach the login page. Dev credentials come from
  `AUTHELIA_DEV_PASSWORD` in `.env` (default `devpassword`); `make init`
  generates the corresponding Authelia users file.

## AI providers

`APP_AI_PROVIDER` selects the default `ai.StructuredGenerator` behind
every AI capability (Teacher feedback, drill exercises); every call —
whichever provider ends up serving it — is wrapped by an observability
decorator that records latency, cost, and success to `ai_requests` (see
the `/ai` page).

- **`fake` (default)** — a deterministic in-process provider with no
  network calls and no API key. This is what `make test` and a clean
  `make up` use out of the box, and what the seeded session's feedback
  flow exercises end to end.
- **`anthropic`** — live calls to Claude. Set in `.env`:

  ```
  APP_AI_PROVIDER=anthropic
  ANTHROPIC_API_KEY=sk-ant-...
  ```

  Optional overrides (leave blank to use the built-in defaults —
  `claude-sonnet-5` / `https://api.anthropic.com`):

  ```
  APP_AI_ANTHROPIC_MODEL=
  APP_AI_ANTHROPIC_BASEURL=
  ```

- **`ollama`** — a local model server, no API key, no per-token cost.
  Set in `.env`:

  ```
  APP_AI_PROVIDER=ollama
  APP_AI_OLLAMA_MODEL=qwen3:4b
  ```

  `APP_AI_OLLAMA_URL` defaults to `http://ollama:11434`, the compose
  `ollama` service's in-network address — leave it blank unless you're
  pointing at an Ollama instance running somewhere else. That service
  isn't part of a plain `make up` (it's gated behind the `ollama`
  compose profile, and ships no model of its own); pull one first:

  ```
  make ollama-pull m=qwen3:4b   # starts the ollama service, then pulls the model
  ```

  To use an Ollama you already run natively on the host (typically the
  one with GPU access) instead of the compose service, point the URL at
  the host gateway — the app service maps `host.docker.internal` for
  exactly this:

  ```
  APP_AI_OLLAMA_URL=http://host.docker.internal:11434
  APP_AI_OLLAMA_MODEL=gemma4:12b
  APP_AI_OLLAMA_TIMEOUT=5m       # optional; bounds one /api/chat round trip
  ```

  **Thinking is disabled on every request** (`"think": false`), because
  most local models default to emitting a full reasoning pass that
  dominates wall-clock time and buys nothing here — one call path wants
  a schema-shaped JSON object, the other wants a tool call.

  **Model choice matters more than size**, because disabling thinking
  interacts with structured-output support. Measured on this repo's
  real `teacher.feedback` prompt and `correction_result.v1` schema
  (Ollama 0.30.6):

  | model | valid JSON with `think:false` | latency |
  |---|---|---|
  | `gemma4:12b` | 6/6 | 3.1–3.9 s |
  | `gemma4:latest` (8B) | 3/3 | 2.0–12.6 s |
  | `gemma4:31b` | 3/3 | 7.1–24.7 s |
  | `qwen3.6:35b` | **0/3** | 2.8–4.7 s |

  `qwen3.6:35b` ignores the `format` schema entirely once thinking is
  off — it returns Markdown prose instead of JSON, fast and useless.
  With thinking on it obeys the schema but takes 14–20 s. Prefer a
  `gemma4` model; if you switch to a model not listed here, check that
  it still honours `format` with thinking disabled before trusting it.

  `make test` always uses the fake provider regardless of `.env` — a
  live key or a running Ollama server is never required to run the
  test suite.

- **`claudecli` / `codexcli`** — host-mode fallbacks that shell out to
  a locally-installed AI CLI (the Claude Code CLI or the OpenAI Codex
  CLI) instead of calling an API directly (PRD §23: "CLI adapters are
  legitimate first-class adapters"). **Host mode only:** neither binary is
  installed inside the app's container image, so these providers only
  do anything useful when the app itself is running directly on a host
  that has `claude`/`codex` on its `PATH` (`go run ./cmd/jlp` outside
  `docker compose`), or via a future bridge that proxies into one —
  under a plain `make up`/`make up-auth`, wiring a prompt to either is
  safe (see below) but every call to it fails with a per-call
  `exec: ... executable file not found in $PATH` error, by design.

  Neither is a valid `APP_AI_PROVIDER` value on its own (only
  `fake`/`anthropic`/`ollama` are) — reach them only through
  `APP_AI_ROUTES`, typically as a fallback link after a real provider:

  ```
  APP_AI_ROUTES=teacher.feedback=anthropic,claudecli
  APP_AI_CLAUDECLI_BIN=        # optional override; defaults to "claude"
  APP_AI_CODEXCLI_BIN=         # optional override; defaults to "codex"
  ```

  Model and effort are configurable per adapter. Both are optional:
  unset means "pass no flag", leaving whatever the operator already
  configured the CLI to do:

  ```
  APP_AI_CLAUDECLI_MODEL=sonnet          # --model
  APP_AI_CLAUDECLI_EFFORT=low            # --effort: low|medium|high|xhigh|max
  APP_AI_CODEXCLI_MODEL=                 # --model
  APP_AI_CODEXCLI_EFFORT=low             # none|minimal|low|medium|high|xhigh
  ```

  The two tools spell effort differently and accept different values,
  so each is validated against its own CLI's vocabulary at startup
  rather than a shared invented one — `max` is meaningful to Claude
  Code and unknown to Codex, `none`/`minimal` the other way round.
  Claude Code takes `--effort` directly; Codex has no such flag, so
  the adapter passes `-c model_reasoning_effort="<level>"`.

  Both adapters report the token counts the CLI itself reports, and
  Claude Code additionally names the model that actually answered
  (from its `modelUsage` map), so `/ai` shows a real model and a real
  cost rather than a `cli` placeholder at $0. Codex does not name a
  model, so its rows read `cli` unless you pin one.

  Measured live (Claude Code CLI 2.1.232, Codex CLI 0.135.0) on this
  repo's `teacher.feedback` prompt: `claudecli` with
  `--model sonnet --effort low` answered in ~11 s; `codexcli` with
  `model_reasoning_effort=low` in ~8 s. Note that Codex rejects some
  model names depending on your account type — `gpt-5.5-codex` returns
  "not supported when using Codex with a ChatGPT account", so leaving
  `APP_AI_CODEXCLI_MODEL` empty is the safer default.

- **`agycli`** — the Antigravity CLI (`agy`), a third host-mode CLI
  adapter with the same host-mode-only caveat as the two above. Reach
  it through `APP_AI_ROUTES`:

  ```
  APP_AI_ROUTES=teacher.feedback=agycli
  APP_AI_AGYCLI_BIN=            # optional override; defaults to "agy"
  APP_AI_AGYCLI_MODEL=          # optional; see `agy models`
  APP_AI_AGYCLI_EFFORT=         # optional; low|medium|high
  APP_AI_AGYCLI_TIMEOUT=        # optional; bounds one run (default 3m)
  ```

  Note the editor binary (`antigravity`) is NOT this — that one is the
  VS Code–derived launcher whose `chat` subcommand hands a prompt to
  the GUI and returns nothing on stdout. `agy` is the headless CLI.

  Three details of the invocation are load-bearing, each established by
  live experiment rather than assumption, and all three are why the
  adapter is its own package rather than a third file in `clicmd`:

  1. **The prompt rides `-p`, not stdin.** Print mode ignores stdin
     entirely.
  2. **The process runs in a fresh temp directory.** Pointed at a real
     project, `agy` behaves like the coding agent it is and reaches for
     shell tools to explore it.
  3. **`--mode plan`** keeps the agent read-only.

  Get any of those wrong and every run ends with the CLI attempting a
  tool that needs the `command` permission — which headless mode cannot
  prompt for — and emitting **zero bytes** on stdout. Measured: 0 of 6
  identical runs produced output before the invocation was corrected,
  3 of 3 after. No change to your Antigravity `settings.json` is needed.
  The adapter treats empty output as a hard error, so a route falls
  through to the next provider instead of returning nothing.

  Unlike the other two CLIs, `agy` enforces the JSON Schema itself via
  `--json-schema`, so the answer arrives already parsed in the
  envelope's `structured_output` field, and its `usage` block gives the
  dashboard real token counts. Measured live (agy 1.1.9,
  `gemini-3.6-flash-low`, effort `low`): ~8 s per feedback round.

  Effort maps onto agy's own ladder, which stops at `high` — there is
  no `xhigh` or `max` tier, and `none` is not one either.

  Both send the prompt (system + user text, plus a trailing instruction
  naming the JSON schema to answer with) on the CLI's stdin —
  `claude -p --output-format json` / `codex exec --json` — under a
  120s timeout, and report `Model: "cli"` with 0 tokens (neither CLI's
  non-interactive JSON output exposes a real token count, and there's
  no per-token cost to track for a locally-installed tool anyway).

### Per-prompt routing (`APP_AI_ROUTES`)

`APP_AI_PROVIDER` picks the default, but individual prompts can be
routed to a different provider — or an ordered fallback chain of
several — via `APP_AI_ROUTES`:

```
APP_AI_ROUTES=teacher.feedback=ollama,anthropic;drill.generate=ollama
```

Format: semicolon-separated `prompt.name=prov1,prov2` entries. A
request whose `PromptName` matches an entry tries that entry's
providers in order, falling through to the next on error; a prompt
with no matching entry uses the `APP_AI_PROVIDER` chain instead. Every
attempt — not just the one that finally answers — is recorded as its
own row in `ai_requests`, so a failover from `ollama` to `anthropic`
shows up as two attempts, correctly attributed.

Only providers the app can actually construct may appear in a route: a
route naming a provider with missing configuration (e.g. `ollama`
without `APP_AI_OLLAMA_MODEL` set) fails the app at startup, not on the
first request that needs it.

### Changing model/effort at runtime (`/settings`)

`APP_AI_*` (above) is the seed and the fallback — but an operator can
override which model each provider uses, and the effort level for the
two CLI providers, from **`/settings`** (linked from `/ai`) without
restarting anything: the change applies to the *next* AI request,
whichever request is already in flight when you save keeps running
with the old value. Each row shows the provider's current effective
value and whether it came from config or an override, plus a "reset to
config" action that deletes the override. Effort is validated against
the exact same per-CLI vocabulary `APP_AI_CLAUDECLI_EFFORT`/
`APP_AI_CODEXCLI_EFFORT` are (`config.ValidateEffort` — one shared
definition, not a second hand-copied list) — an invalid value is
rejected in the form and never written to the database.

Overrides live in the `app_settings` table (global, not per-learner)
and are loaded once at boot into an in-memory snapshot every AI
adapter re-consults on every call; a save updates that same snapshot,
which is what makes the change visible immediately, with no
reconstruction of any adapter. `APP_AI_*` itself is never copied into
the database — an absent override row always means "use config",
exactly as if `/settings` had never been touched.

Where a provider can genuinely enumerate what it's able to serve,
`/settings` shows a dropdown instead of a bare text field: Ollama via
`GET /api/tags` (filtered to completion-capable models — an
embedding-only model like `mxbai-embed-large` is never offered),
`agycli` via `agy models`, and Anthropic via `GET /v1/models` when an
API key is configured. Every list is fetched lazily when `/settings`
renders (never at boot — a slow or absent Ollama must not delay or
fail startup) and cached for an hour so a page load doesn't cost a
fresh subprocess/network round trip every time; a failed attempt is
never cached, so the very next render retries. If a list can't be
fetched (no key, unreachable, etc.) the row falls back to free text
with the reason shown, and a free-text field is always available
alongside a dropdown too, so a model missing from the list can still be
set. The current value is always included in its dropdown even if the
provider no longer reports it — an operator's setting is never silently
dropped. Claude Code and Codex have no way to enumerate at all (`claude
models`/`codex models` aren't real subcommands); their rows stay free
text, with Claude Code additionally offering its documented `sonnet`/
`opus`/`haiku` aliases, clearly labelled as aliases and not a list.

### Agentic teacher (`APP_AI_AGENTICTEACHER`)

`APP_AI_AGENTICTEACHER=true` (default `false`) opts every feedback
request into an agentic path (PRD §27/§50): before producing
corrections, the teacher drives a tool-calling investigation over a
read-only subset of `internal/tools` (the learner's recent priorities,
correction history, grammar progress, vocabulary, recent writing —
never a mutating tool) instead of the single-shot path's precomputed
context. The investigation's own conversation — every model turn and
tool call — is recorded and viewable at `/ai/agents`, linked from the
`/ai` dashboard. The corrections themselves always come from the same
schema-validated structured-generation call either way (Rule 4); the
flag only changes where the surrounding context comes from. Leave it
unset for Phase 1's original single-shot behaviour and cost,
unchanged.

### A2A protocol adapter (`APP_A2A_ENABLED`)

`APP_A2A_ENABLED=true` (default `false`) mounts an [A2A
protocol](https://a2a-protocol.org/) (agent-to-agent) adapter at
`APP_A2A_PATH` (default `/a2a`, inside the same authenticated route
group as every other route — Authelia/CSRF posture is unchanged). It
speaks **A2A v1.0 over the JSON-RPC 2.0 binding**, so the official
`@a2a-js/sdk` client talks to it directly:

```js
const client = await new ClientFactory().createFromUrl('http://localhost:28080/a2a/');
const task = await client.sendMessage({ message: { messageId: crypto.randomUUID(), role: 'ROLE_USER', parts: [{ text: '「は」と「が」の違いは？' }] } });
```

Two routes: `GET {path}/.well-known/agent-card.json` (the agent card)
and `POST {path}/v1` (the JSON-RPC endpoint, serving `SendMessage`,
`GetTask`, and `CancelTask`). It exposes four of JLP's own agents as
A2A skills — a conversational tutor (the default for a plain chat
message), Writing Reviewer, Learner Analyst, Lesson Planner (PRD
§29/§30). Streaming and push notifications are **not** served, and the
card says so.

Every task runs through the exact same `application/agentrun.Runner` +
`internal/tools.Registry` permissions the local agentic-teacher path
uses — a remote caller gets no privilege a local agent lacks (Rule 13)
— and shows up at `/ai/agents` like any other agent-run. See
`docs/api/a2a.md` for the full contract, wire shapes, and a worked
example against the fake provider. Leave it unset to keep the adapter's
routes entirely absent (404).

> **Changed:** this adapter previously served a bespoke REST shape
> (`POST {path}/tasks` with `{skill, input, session_id}`) that was not
> the A2A protocol and that no A2A client could speak. That shape is
> retired — see `docs/api/a2a.md` for the migration.

**A generic chat client for it** (and for any other A2A agent) lives at
`clients/a2a-chat/` — `make a2a-chat` starts it at
**http://localhost:8090**, built on the official `@a2a-js/sdk`. See
`clients/a2a-chat/README.md`.

### Startup pricing warning

`ai_requests.cost_usd` is computed from a fixed rate card in
`cmd/jlp/ai.go`'s `aiPricing()` (USD per million tokens, keyed by exact
model name). A configured or routed model absent from that map still
works — its cost is simply recorded as `$0`, the same as any
genuinely-free model — but the app logs a `slog.Warn` at boot naming
the provider and model, so an operator who typo'd
`APP_AI_ANTHROPIC_MODEL` (or is running a live-billed model under a
name `aiPricing()` doesn't know) finds out immediately rather than
noticing months of untracked spend later.

## Evaluation corpus (`make eval`)

`make eval` runs `eval/corpus/*.yaml` — a hand-authored set of Japanese
sentences with expected corrections (grammar/particle/tense errors,
unnatural-but-grammatical phrasing, casual/formal mismatches) plus
false-positive traps (fully natural sentences that should draw zero
corrections) — through the Teacher agent using the CONFIGURED provider
chain (`APP_AI_PROVIDER`/`APP_AI_ROUTES`, same as `jlp serve`). It's the
corpus-level regression baseline for comparing providers/prompt
versions (PRD §48/§49), separate from the live, per-response `/ai`
quality dashboard.

Each run writes a per-case Markdown report to `eval/reports/<RFC3339
timestamp>.md` (gitignored) and prints a precision/recall/false-positive-rate
summary. If a previous report exists, it also prints the score deltas
and **exits 1** if precision or recall dropped by more than 0.05 versus
that previous run — the same gate a CI regression check would apply.

```sh
make eval   # $(TOOLS) go run ./cmd/jlp eval
```

Runs fully offline against the default `fake` provider (no API key, no
running services needed beyond the tools container) — the fake
provider only recognizes 3 of the corpus's patterns by design, so a
fake-provider run's recall is intentionally low; it's there to prove
the scoring/report/regression mechanics work, not to grade the fake
provider's Japanese. Point `APP_AI_PROVIDER` (or `APP_AI_ROUTES` for
`teacher.feedback`) at `anthropic`/`ollama` to get a real quality
signal. `jlp eval` is a batch CLI command with no browser step.

## Chrome extension

`chrome-extension/` is a pure client of the `/api/v1` JSON API (PRD
§40 — no learning-domain logic lives there): right-click Japanese text
on any page to get JLP corrections or save it to your vocabulary list,
styled to match the app. No build tooling — plain ES modules loaded
straight off disk by Chrome.

```sh
make ext-build   # -> dist/jlp-extension.zip (dist/ gitignored)
```

See **[`chrome-extension/README.md`](chrome-extension/README.md)** for
the manual `chrome://extensions` install checklist, the
`document_id`-resolution workaround (there's no
`GET /api/v1/sessions/{id}/document` endpoint yet), and the CORS-vs-
extension-host-permissions distinction that governs how its API calls
work (and how they're verified without a real install).

## Design system

`web/static/css/tokens.css` (colors, shadows, `--ring`), `components.css`
(buttons, forms, badges, chips, stat tiles, cards/tables, the correction
card, topbar/nav/theme-toggle) and `web/static/fonts/` implement the
JLP Design System — an emerald `oklch()` light/dark palette, Instrument
Sans + JetBrains Mono, exported from a Claude Design mockup as the
project's design source of truth. `app.css` is page-layout only
(workspace grid, editor sizing); load order in every template is
`fonts.css` -> `tokens.css` -> `components.css` -> `app.css`.

Theming: `data-theme` on `<html>` is set by a tiny inline script in
`<head>` before first paint (no FOUC) — `localStorage("jlp-theme")` ->
`prefers-color-scheme` -> `light` — then `web/static/js/theme.js` wires
the topbar's toggle `<button>` (`window.jlp.toggleTheme()`,
`aria-pressed` reflects state, persists the explicit choice back to
`localStorage`).

Fonts are vendored, not loaded from a CDN — PRD §39/§45 (offline PWA +
privacy) forbid external assets at runtime:

```sh
make vendor-fonts   # -> web/static/fonts/*.woff2 + fonts.css (committed)
```

**Deliberate tradeoff:** Noto Sans JP (the mockup's Japanese font) is
*not* vendored — full CJK coverage runs several MB per weight, which
is a bad fit for an offline-first PWA. `font-family` keeps it first in
the fallback stack after Instrument Sans, so a device that already has
it (or another JP font) renders Japanese normally; the vendored files
only cover Latin (UI chrome, numbers, code):
`'Instrument Sans','Noto Sans JP','Hiragino Sans','Yu Gothic',system-ui,sans-serif`.

## External reader-app integration (`POST /api/v1/words`)

`POST /api/v1/words` bulk-ingests vocabulary from an external reader
app — built for Nihongo Daily, implementing its `/api/v1/words`
contract verbatim (field names, the `200` status, the
`{"imported":N}`/`{"error":"..."}` shapes). Words are upserted by
(identity, kanji) in one transaction; a sync never touches
lookups/productions/successful_productions (that's what
`vocabulary.lookup`/`/api/v1/vocabulary/events` are for), and a sparse
re-sync never blanks out richer data a prior sync already recorded.

See **[`docs/api/words-ingestion.md`](docs/api/words-ingestion.md)**
for the full request/response contract, the upsert/counter semantics,
the 1000-word batch limit, a working `curl` example, and how
authentication behaves under `APP_AUTH_MODE=static` vs. Authelia (this
endpoint has no separate service-token scheme — see that doc's
Authentication section before relying on it from a non-LAN
deployment).

## Weekly email summary (PRD §21, §65)

JLP can send one learner an encouraging weekly digest email —
accomplishments, biggest improvements, persistent weaknesses, useful new
expressions, recommended focus, and an optional low-pressure challenge
for next week — composed from `internal/application/analytics.Service`
statistics, the heuristic planner's top priorities, and recently active
vocabulary. This is JLP's only piece of *automatic* outbound external
communication, so it is opt-in, explicitly (PRD §65): the cron scheduler
is never constructed at all unless `APP_SUMMARY_ENABLED=true`. Leaving
it unset (the default) means the app boots with zero email-sending
capability, full stop.

```sh
make up-mail                          # starts postgres + app + Mailpit (SMTP capture UI)
# set APP_SUMMARY_TO=you@example.com in .env, then either:
make send-summary                     # trigger one send immediately, right now
# or, for the automatic weekly schedule, also set in .env:
#   APP_SUMMARY_ENABLED=true
#   APP_SUMMARY_CRON="0 18 * * 0"     # optional; this is already the default (Sunday 18:00)
```

Open **http://localhost:8025** to see delivered mail — Mailpit is a
local SMTP capture inbox, nothing leaves your machine. `APP_SMTP_ADDR`
defaults to `mailpit:1025` (the `up-mail` profile's in-network
address); point it at a real relay for anything beyond local dev.

`jlp send-summary` (what `make send-summary` runs) always sends
immediately regardless of `APP_SUMMARY_ENABLED` — running it IS the
opt-in act, the same as clicking a "send now" button would be. Enabled
only gates the **automatic** cron scheduler that `make up`'s app
container would otherwise start on `APP_SUMMARY_CRON`'s schedule. Both
paths target the single static identity (`APP_AUTH_STATIC_ID`) —
per-learner scheduling across multiple real accounts is a Phase 4
concern.

## MQTT event bridge (PRD §31-33, §12, §59)

`internal/adapters/mqtt.Bridge` mirrors every learning event onto MQTT
and accepts external vocabulary ingestion back — dormant by default,
like the weekly summary and AnkiConnect push above: with no
`APP_MQTT_URL` set, `cmd/jlp/main.go` never constructs it and zero
broker connections are ever attempted.

**Outbound**: every one of `event.AllTypes()` (writing, feedback,
correction, grammar, vocabulary, hint, answer, confidence, quiz, anki,
tutor — the full learning-event catalog) is republished, QoS1, to

```text
learner/{identity}/{category}/{type}
```

where `{category}` is the type's first dot-segment (e.g.
`vocabulary.looked-up` → `vocabulary`). The payload is a
transport-independent JSON envelope —
`{id, identity_id, session_id, type, subject, evidence, occurred_at}`
— built entirely inside the adapter; the domain `event.LearningEvent`
type itself carries no MQTT/JSON-tag knowledge.

**Inbound**: the bridge subscribes `learner/+/vocabulary/ingest` and
routes whatever arrives there through the *same*
`application/vocabulary.Service.Ingest` write path (and the same wire
shape) as `POST /api/v1/vocabulary/events` — this is what lets an
external reading app on your LAN publish a lookup directly to the
broker instead of making an HTTP call. `{identity}` comes from the
topic segment and must already exist (`identities.Get`); an unknown
identity, or a payload that isn't valid JSON, is logged and dropped,
never surfaced as an error to the publisher.

**Trust model**: `deploy/mosquitto/mosquitto.conf` runs
`allow_anonymous true` with no host port published — anyone who can
reach the broker (any container on the compose network today) can
publish as any identity string. This is appropriate for a private LAN
dev broker, not for one exposed further; a production deployment that
opens MQTT beyond the LAN needs its own broker-level authentication/
ACLs, which this bridge does not provide.

```sh
make up-mqtt   # starts postgres + app + mosquitto, app pointed at tcp://mosquitto:1883
make mqtt-tap  # tails every learner/# topic (mosquitto_sub -t 'learner/#' -v)
make mqtt-demo # publishes a sample vocabulary.lookup ingest event; appears on /vocabulary
```

Reconnection after the initial connect is handled by paho's
`AutoReconnect` — a mosquitto restart while the app is running self-
heals without restarting the app.

## Slack channel adapter (PRD §20, §20.1)

`internal/ports/channels` is a transport-agnostic channel port — the
domain never learns which messaging surface an interaction arrived
over — and `internal/application/channel.Service` is the one place that
turns a channel message into calls against the *same*
sessions/feedback/practice application services every other JLP surface
(the web UI, the agentic teacher) already uses: a Slack message gets no
privilege, and no different socratic-gating behaviour, an HTTP request
wouldn't also get. `internal/adapters/slack` is the first concrete
`Channel` implementation, over Slack's **Socket Mode** — the connection
dials *out* to Slack, so no public ingress, reverse-proxy route, or new
`docker-compose.yml` service is needed even for a LAN-only deployment.

**Dormant by default**: both `APP_SLACK_APPTOKEN` and
`APP_SLACK_BOTTOKEN` empty (the default) keeps the adapter entirely
unconstructed — `cmd/jlp/main.go` never dials Slack. Setting only one of
the two is rejected at boot (`config.validate()`), as is a token that
doesn't carry Slack's own documented prefix (`xapp-...` /
`xoxb-...`) — a pasted-the-wrong-token mistake fails fast at startup
rather than silently at the first Socket Mode handshake.

**Routing** (a message's trimmed text decides the reply):

| Message | Reply |
|---|---|
| Japanese text | A correction of that text in the sender's channel session, rendered **compact** — pairs plus a one-line reason, not the full HTML card. A gated (socratic, unrevealed) correction shows only its hint — **never** the answer; `correction.IsGated` (the single project-wide pre-reveal predicate) decides this, not a re-implementation. |
| `practice` / 練習 | Starts a drill exercise and renders it as text; the sender's *next* message is scored as the answer. |
| `help` / ヘルプ | A short command list. |

A channel session is a real `session.Session` (title `"Slack — <external
id>"`), created on first contact so every usual event/learner-model
update flows exactly as it would from the web UI; each inbound message
becomes that session's single document's newest version.

**Untrusted edge — `APP_CHANNELS_ALLOWFROM`**: a comma list of
`<channel>:<external id>=<identity>` entries, e.g.
`slack:U0123ABCDEF=dev,slack:U0456DEFGH=alice`, mapping a Slack user ID
to a JLP `learner.IdentityID`. A sender **not** in this list gets a
polite refusal, and — this is the part that's actually enforced by a
test, not just documented — **nothing is recorded**: no session, no
document, no learning event, no identity row. Config-driven, explicit
allow-list only; nothing is auto-provisioned from the message itself.

```sh
make up                 # Socket Mode dials out — no extra compose service or profile needed
# set APP_SLACK_APPTOKEN, APP_SLACK_BOTTOKEN, APP_CHANNELS_ALLOWFROM in .env, then:
make restart
make slack-smoke        # needs APP_SLACK_BOTTOKEN + APP_SLACK_SMOKECHANNEL; posts one test message
```

Reconnection after the initial connect is handled by
`socketmode.Client.RunContext`'s own retry loop (the slack-go analogue
of paho's `AutoReconnect` above); every other failure along the path —
a bad inbound event, a failed reply post, a fatal Start error — is
logged, never fatal: one channel going down can't take the rest of the
app with it.

**Not exercised by `go test`/`make test`/`make test-integration`**: this
adapter is tested entirely over a fake, in-process transport (no
network, no real Slack connection) — see
`internal/adapters/slack/slack_test.go`. `make slack-smoke` is the only
way to confirm real Slack credentials actually work, and requires the
person running it to supply their own bot token and app-level token.

## Signal channel adapter (PRD §20, §20.1)

`internal/adapters/signal` (Phase 4 Task 5) is the second concrete
`Channel` implementation over the same `ports/channels`/
`application/channel.Service` pipeline the Slack section above
describes — a Signal message gets no privilege, and no different
socratic-gating behaviour, a Slack message or an HTTP request wouldn't
also get. It talks to a
[`signal-cli`](https://github.com/AsamK/signal-cli) sidecar running in
JSON-RPC daemon mode over a plain TCP socket, newline-delimited JSON-RPC
2.0 in both directions on one connection (this adapter's own `send`/
`subscribeReceive` calls interleaved with the daemon's unsolicited
`receive` notifications for inbound messages) — see that package's own
doc comment for the exact wire protocol.

**Dormant by default**: both `APP_SIGNAL_RPCURL` and `APP_SIGNAL_NUMBER`
empty (the default) keeps the adapter entirely unconstructed —
`cmd/jlp/main.go` never dials the sidecar. Setting only one of the two
is rejected at boot (`config.validate()`), as is an `APP_SIGNAL_NUMBER`
that isn't E.164 (missing its leading `+`).

**Sidecar, not baked-in account**: the `signal-cli` compose service (the
`signal` profile — not started by `make up`; join it with `make
up-signal`) starts with no `-a <number>` of its own. `APP_SIGNAL_NUMBER`
is instead sent as the `"account"` param on every JSON-RPC call this
adapter makes, so the sidecar's own command line never needs to change
just to point at a different linked number.

**Routing** is identical to Slack's own table above (Japanese text →
compact correction, `practice`/練習 → drill, `help`/ヘルプ → command
list) — `application/channel.Service` doesn't distinguish which channel
sent the message. A Signal channel session's title is `"Signal —
<sender's E.164 number>"`; `APP_CHANNELS_ALLOWFROM` entries for Signal
look like `signal:+15555550100=dev`, keyed on the sender's own number
(the untrusted edge, same contract as Slack's — see that section above).

```sh
make up-signal          # starts postgres + app + signal-cli (app stays dormant — Signal config is still unset)
make signal-register    # one-time device link — prints a QR/URI to scan with the account's phone (see deploy/signal/README.md)
# then set APP_SIGNAL_RPCURL=signal-cli:6006, APP_SIGNAL_NUMBER, and APP_CHANNELS_ALLOWFROM TOGETHER in .env and:
make restart
```

**Honesty**: linking a device requires scanning that QR/URI with a
phone that owns the number — there's no credential-free way around
that, by Signal's own design. This repo built and unit-tested the
adapter fully offline (a fake transport, plus a real in-memory TCP
listener standing in for signal-cli's JSON-RPC protocol — see
`internal/adapters/signal/signal_test.go`) and confirmed the sidecar's
real JSON-RPC endpoint responds correctly to a dry run against an
unlinked account (a `"not registered"` JSON-RPC error — the expected,
documented evidence the wiring is correct). **Live Signal send/receive
was never exercised** — see `deploy/signal/README.md` for the full
device-link procedure and the exact dry-run transcript.

## Speech recognition (STT/TTS, PRD §66)

Phase 4 Task 8 adds speech recognition that feeds the SAME conversation
pipeline typed text already goes through — PRD §66's hypothesis is that
written formulation transfers to speech, and the only way to measure
that (Task 9) is if a spoken turn produces the exact same corrections,
events, and gating a typed one would. `POST /speech/transcribe` returns
`{"text":..., "duration_ms":...}`; `web/static/js/record.js` drops that
text into the conversation pane's own input, and the learner submits it
through the **unchanged** `POST /sessions/{id}/conversation` route —
literally the same handler, the same `application/conversation.Service.
Say` call, the same corrections and `conversation.turn`/`correction.*`
events a typed message gets. Nothing about speech input has a second,
parallel path into the conversation pipeline.

**Dormant by default**: `APP_SPEECH_STTURL` and `APP_SPEECH_TTSURL` are
each independently empty by default — unlike Slack/Signal's paired
tokens, either can be set without the other. Empty `STTURL` means
`cmd/jlp/main.go` never constructs a recognizer and every `POST
/speech/transcribe` answers `503 speech recognition is not configured`,
never a panic or a silent no-op.

**STT**: `internal/adapters/whisper` targets a local
[whisper.cpp](https://github.com/ggml-org/whisper.cpp) `whisper-server`
instance — `POST {url}/inference`, multipart field `file`, `response_
format=verbose_json` (whisper-server's plain `json` format omits the
clip's own `duration`, which `ai.Transcript.DurationMS` — and the
`speech.transcribed` event's evidence — need). The compose `whisper`
service (the `speech` profile) runs `ghcr.io/ggml-org/whisper.cpp:main`
with `--convert`, so it accepts a browser `MediaRecorder`'s webm/opus
blobs directly (ffmpeg, bundled in the image, transcodes internally) —
no raw-WAV requirement on the browser side. No model ships in the
image; the service's own startup command downloads `ggml-tiny.bin`
(~75MB, multilingual) into a named volume on first start only.

**TTS**: `internal/adapters/tts` targets a local
[VOICEVOX Engine](https://github.com/VOICEVOX/voicevox_engine) instance
— `POST {url}/audio_query` then `POST {url}/synthesis`, VOICEVOX's own
two-step contract (the first call's JSON response is forwarded
verbatim as the second call's body). **This is deliberately NOT the
dormant-shell treatment WhatsApp got below** — a genuine, working local
Japanese TTS engine turned out to be available in this environment
(`voicevox/voicevox_engine:cpu-ubuntu20.04-latest`), so this adapter is
real and tested against it, not a stub standing in for an engine that
doesn't exist. It's still dormant unless `APP_SPEECH_TTSURL` is set,
and nothing in this task's HTTP surface calls `Speak` yet — no route
wires a "hear this spoken" button into the conversation pane; that's
left to a future task, the same way `internal/adapters/ankiconnect.
Client` sits unused until `appanki.Service.SetConnector` is called.

```sh
make up-speech    # starts postgres + app + whisper + voicevox (app stays dormant — Speech config is still unset)
# first start downloads the whisper model (~75MB) — give it a minute, then:
# set APP_SPEECH_STTURL=http://whisper:8080 and/or APP_SPEECH_TTSURL=http://voicevox:50021 in .env, then:
make restart
```

**Honesty — what was actually verified**: both adapters have full
offline `httptest` suites (`internal/adapters/whisper/whisper_test.go`,
`internal/adapters/tts/tts_test.go` — canned responses, no network, no
sidecar; `make test` never depends on either running). Beyond that,
this task ran `make up-speech` for real — pulled
`ghcr.io/ggml-org/whisper.cpp:main` and `voicevox/voicevox_engine:
cpu-ubuntu20.04-latest`, downloaded a real `ggml-tiny.bin` model into
the `whispermodels` volume, and confirmed the whole stack end to end
against the running dev app (not a standalone container):

```
$ curl -X POST http://localhost:28080/speech/transcribe -F "audio=@ja-clear.wav;type=audio/wav"
{"text":"Hi.","duration_ms":771}
```

That request went through the ACTUAL route — identity middleware,
`application/speech.Service.Transcribe`, the real
`internal/adapters/whisper.Recognizer` dialing the real `whisper`
sidecar over the compose network — and this environment has no
microphone, so the audio was a short `espeak-ng`-synthesized Japanese
clip ("はい"), per this task's honesty clause. `ggml-tiny` misheard the
robotic synthesis as "Hi." — reported exactly as it happened, not
cherry-picked. What this DOES prove, honestly: the full round trip is
real (real container, real ffmpeg conversion via `--convert`, real
model inference, a real non-zero `duration_ms`), not a canned response.

The same fixture was then POSTed through the browser via
`javascript_tool` (mirroring `record.js`'s own fetch call exactly:
`FormData` with an `audio` field, `POST /speech/transcribe`), and the
returned text ("Hi.") was confirmed landing in the conversation pane's
own input field. Submitting it through the **unchanged** 送信 button
produced a real conversation turn — a tutor reply rendered in the
transcript — proving the transcript reuses the SAME
`application/conversation.Service.Say` pipeline typed text does; no
second code path was ever written for it. See this task's report for
the full transcript and screenshots (both themes, 390px width, no
horizontal overflow).

VOICEVOX Engine reachability was also confirmed from INSIDE the app
container, over the real compose network: `wget -qO-
http://voicevox:50021/version` returned `"latest"`. No end-to-end
`Speak` call was exercised beyond the adapter's own `httptest` suite,
since nothing consumes it yet (see above).

## Learning outcomes — `/outcomes` (PRD §52, §72, §73)

This is the page the whole product exists to justify: **did being
corrected lead to better later production?** Everything else in JLP
produces evidence; this reads it back.

**How a concept is judged.** For every grammar concept the learner has
actually been corrected on, `internal/adapters/postgres/outcomes.go`
returns raw counts only — corrections in the 30 days after the FIRST
correction (the baseline), corrections in the most recent 30 days,
independent solves, correct later productions, totals and dates.
`internal/application/outcomes` then classifies, in this order:

| Group | Rule |
| --- | --- |
| **収束 (retired)** | ≥3 corrections, then 60 days with none |
| **判定外 (excluded)** | first corrected <60 days ago — the two 30-day windows still overlap, so comparing them measures nothing |
| **判定外 (excluded)** | <3 corrections in the baseline window — too thin a rate to compare against |
| **改善 (improving)** | strictly fewer corrections in the recent window than the baseline |
| **継続中 (persistent)** | the same number or more |

Retired is checked first because it is self-contained: "three
corrections then sixty silent days" never needed the before/after
comparison, so a thin baseline must not demote it. Equal counts are
*persistent*, not improving — no change is no evidence of change.

**Honesty is the feature, not a caveat.** A concept with too little
data is neither improving nor persistent; it is excluded, and the page
states how many were excluded **and the specific reason for each one**
("only 12 days of history…", "only 2 corrections in the first 30
days…") rather than quietly shrinking the denominator. The headline is
one sentence of plain fact — counts, and the direction the
corrections-per-1,000-characters rate moved — and there is no score, no
percentage-improved, no streak, and no encouragement anywhere on the
page (PRD §56, enforced by tests in both
`internal/application/outcomes` and `internal/adapters/http`).

The zero-data case gets the most care, because it is what every new
learner sees:

> Not enough data yet to say whether corrections are improving later
> production.

Not "0% improvement", not an empty chart implying decline. A trend is
drawn only when at least **two** weeks actually carried data — one
point plotted across a full-width sparkline reads as a flat line, which
is a claim nothing supports — and a week with nothing measured renders
as `—`, never `0.0` or `0%`.

**What each number measures.** `修正/1000字` divides by characters
*submitted for review* (`feedback_requests.selection_text`), not every
character ever typed, so it answers "does the same amount of reviewed
writing now need less help?". That deliberately differs from the home
dashboard's lifetime `Statistics.CorrectionsPer1000`, which uses whole
documents; the two are different measures and `/outcomes` never shows
them side by side. `語彙の正用` counts `vocabulary.produced-correctly`
events for expressions that literally appear in the replacement text of
a correction tagged with that concept — a conservative link (it can
miss, but it cannot invent one), and the only link the schema offers.

Every threshold above lives in Go, in `application/outcomes`, never in
SQL — see that package's doc comment and `analyser_test.go`, which
walks every boundary (exactly-60-days, exactly-3-corrections,
exactly-equal counts) explicitly.

**Honesty — what the dev database can actually show**: this repo's dev
database has real accumulated history, but all of it was written
*today*, so every corrected concept legitimately falls under the
"<60 days of history" exclusion. `/outcomes` therefore renders
"No concept has enough history to judge yet; 1 concept excluded for
insufficient data." against it — which is the correct answer, not a
gap in the feature. No backdated demo data was inserted to make the
page look busier; the improving/persistent/retired groups are proven by
the analyser's boundary tests and the handler tests instead.

## WhatsApp: deferred, documented, not stubbed

WhatsApp shares the same `ports/channels.Channel` port Slack and Signal
implement, but **no `internal/adapters/whatsapp` code exists** — not
even a stub. It requires a Meta Business account, an approved WhatsApp
Business phone number, and public HTTPS ingress for inbound webhooks,
none of which exist for this project and none of which this task
attempted to obtain. `docs/api/whatsapp-notes.md` documents exactly what
the adapter would need — the webhook route and verify-token handshake,
the inbound payload shape, the Cloud API send call, config shape — and
why building against credentials nobody has would be an elaborate stub
wearing an adapter's shape, not working code.

## Deploying

The production stack (`deploy/compose.prod.yml`) is a separate compose
project (`jlp-prod`) from the dev stack above — a distroless app image,
Caddy terminating TLS, and Authelia forward-auth, all fronting Postgres.
It runs the same way whether you're verifying it locally or deploying to
a real host:

```sh
sh scripts/gen-prod-secrets.sh          # Authelia JWT/session/storage secrets + one login user
cp deploy/.env.prod.example deploy/.env.prod   # fill in JLP_DOMAIN, POSTGRES_PASSWORD, etc.
make deploy-local                       # builds, starts, migrates, waits for /healthz
```

`make deploy-local` verifies the full production stack on your own
machine at **https://localhost:8444** (self-signed cert — click
through the browser warning). It runs alongside the dev stack without
colliding: distinct compose project, network subnet, and volumes.

To deploy to a real host instead, set `DEPLOY_HOST` (a docker-context-style
`user@host`) in `.env`, then:

```sh
make deploy         # builds, starts, and migrates over DOCKER_HOST=ssh://$DEPLOY_HOST
make deploy-logs    # tail the remote app service's logs
```

`gen-prod-secrets.sh` is idempotent — rerunning it (e.g. as part of a
later `make deploy-local`) never rotates secrets out from under a
running stack unless `FORCE=1` is set. `deploy/.env.prod` is gitignored
and must never be committed.

## Architecture

Modular monolith in Go with strict hexagonal boundaries:

```
domain  ←  application  ←  ports  ←  adapters
```

- **`internal/domain`** — pure business types and logic (sessions,
  writing documents, corrections, rune-level diffing, learning events).
  No imports of adapters, application, ports, or infrastructure packages
  (`net/http`, database drivers, config).
- **`internal/application`** — use-case services (`sessions.Service`,
  `writing.Service`, `feedback.Service`, `analytics.Service`,
  `learning.Recorder`) that depend on `internal/ports` interfaces only,
  never on concrete adapters.
- **`internal/ports`** — the interfaces the application layer depends on:
  `storage` (repositories), `ai` (`StructuredGenerator`), `auth`
  (`Authenticator`), `events` (`EventBus`).
- **`internal/adapters`** — concrete implementations: `postgres`
  (pgx + sqlc), `http` (chi server, HTML + JSON API), `staticauth` /
  `authelia`, `inprocbus`, `fakeai` / `anthropic` / `ollama` / `clicmd`
  (AI providers — `clicmd` is the host-mode Claude/Codex CLI fallback,
  see AI providers above) and `airouter` (routes among them).
- **`internal/agent/teacher`** — the AI agent that reviews writing; it
  reaches the AI port and domain types only, never a repository.

These boundaries are enforced by `.golangci.yml`'s `depguard` rules
(encoding PRD §75 Rules 1–3) and checked in isolation by `make
arch-check`, separately from the full `make lint`.

**Reference docs:**
- PRD: [`docs/japanese_learning_platform_jlp_prd.md`](docs/japanese_learning_platform_jlp_prd.md)
- Design spec: [`docs/superpowers/specs/2026-08-14-jlp-platform-design.md`](docs/superpowers/specs/2026-08-14-jlp-platform-design.md)
- Phase 1 implementation plan: [`docs/superpowers/plans/2026-08-14-jlp-phase1-mvp.md`](docs/superpowers/plans/2026-08-14-jlp-phase1-mvp.md)
- Roadmap: [`docs/superpowers/plans/2026-08-14-jlp-roadmap.md`](docs/superpowers/plans/2026-08-14-jlp-roadmap.md)

## Testing and quality gates

```sh
make test              # unit + application tests, fake AI provider, no services needed
make test-integration  # adapter tests against a real postgres (docker compose)
make lint              # govet, staticcheck, errcheck, ineffassign, unused, depguard
make arch-check         # depguard only, PRD §75 dependency-direction rules
make test-race          # go test -race ./... — same package set as `make test`, with the race detector on
```

All five are expected to pass before every commit; `test-integration`
and `arch-check` are the ones easiest to forget locally since they need
services running or a narrower lint pass, respectively. `test-race`
exists because `make test` alone does not catch data races (Phase 4
Task 4's own adapter tests shipped one that `make test` stayed green
on) — every goroutine-spawning adapter (`slack`, `mqtt`, the channel
port's dispatch loop) is exactly the code this gate is for.

`internal/adapters/mqtt/mqtt_test.go`'s integration tests (build tag
`integration`, same as every other adapter integration test) are
included in `make test-integration`'s package list but skip themselves
individually unless `APP_MQTT_URL` is set — so a plain
`make test-integration` stays green with zero mosquitto dependency.
To actually run them: `make up-mqtt` first (starts mosquitto), then

```sh
docker compose run --rm -e APP_MQTT_URL=tcp://mosquitto:1883 tools \
    go test -tags integration ./internal/adapters/mqtt/...
```

## Project layout

```
cmd/jlp/                     entrypoint: serve | migrate | seed
internal/config/             viper -> typed Config, validation
internal/domain/             learner, session, writing, correction, diff, event
internal/application/        sessions, writing, feedback, learning, analytics
internal/ports/               auth, ai, events, storage, notifications interfaces
internal/adapters/           http, postgres, staticauth, authelia, inprocbus, fakeai, anthropic, ollama, clicmd, airouter, smtp
internal/agent/teacher/      the Teacher AI agent (ReviewWriting)
internal/observability/      AI request/cost/latency recording decorator
internal/prompts/            embedded, versioned prompt templates
internal/schemas/            embedded JSON Schemas + validator
web/templates/                layout, pages, HTMX partials
web/static/                   CSS, vendored htmx/alpine, PWA assets
chrome-extension/             MV3 extension: pure /api/v1 JSON client, no build step
db/queries/                   sqlc query files
internal/adapters/postgres/migrationsfs/  goose SQL migrations (embedded)
deploy/                        production compose, Caddyfile, Authelia config
eval/corpus/                  eval corpus (grammar-basics, naturalness, false-positive-traps)
eval/reports/                 `make eval` output, gitignored
```
