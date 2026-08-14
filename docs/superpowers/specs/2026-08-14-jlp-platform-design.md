# JLP — Japanese Learning Platform: Technical Design Spec

**Date:** 2026-08-14
**Source PRD:** `docs/japanese_learning_platform_jlp_prd.md`
**Status:** Approved for implementation planning

This spec resolves the concrete technical decisions the PRD leaves open, and defines the
build/operate/deploy workflow. The PRD remains authoritative for product behavior; this
document is authoritative for implementation choices.

---

## 1. Delivery constraints (from project owner)

1. **Makefile-driven everything.** Build, operate, and deploy are simple `make` targets.
   No developer (human or subagent) should need to remember raw `docker compose` or `go`
   invocations.
2. **All development runs locally via Docker Compose.** The Go toolchain runs inside
   containers; nothing assumes host-installed Go.
3. **Subagents verify their own work in the Chrome browser.** Every implementation task
   that touches user-visible behavior ends with the implementing subagent driving Chrome
   (claude-in-chrome tools) against the local compose stack and confirming the feature
   works. Backend-only tasks verify via tests plus, where a UI surface exists, a browser
   smoke check.

## 2. Approach

**Phased vertical slices inside a modular monolith**, following the PRD's own phasing
(MVP → Phase 2 → Phase 3 → Phase 4). Each slice lands end-to-end (domain → application →
adapter → UI) and finishes in a state a browser can verify. Hexagonal boundaries from
day one (PRD §34/§75), but adapters are only built when a phase needs them (Rule 15,
YAGNI).

Alternatives rejected:

- *Infrastructure-first* (all ports/adapters scaffolded up front): long stretch with
  nothing verifiable, high speculative-design risk.
- *UI-first with mocked AI*: delays the learner-model/event core, which is the durable
  asset (PRD §77), and risks retrofitting identity scoping.

## 3. Technology decisions

| Concern | Decision | Notes |
|---|---|---|
| Language | Go (latest stable, ≥1.25) | PRD-mandated |
| Module path | `github.com/mikeyaustin/jlp` | Cosmetic; rename is a one-line change + goimports |
| HTTP router | Chi v5 | PRD §37 |
| UI | Go `html/template` + HTMX 2 + Alpine.js 3, vendored locally (no CDN) | PRD §38; vendoring required for offline PWA + privacy |
| CSS | Hand-rolled vanilla CSS with design tokens | 3-pane layout is custom; avoids framework churn |
| Editor (MVP) | `<textarea>`-based with autosave, rune-accurate selection offsets | Native IME support; `selectionStart/End` gives clean offsets. CodeMirror 6 is a later opt-in if plain textarea proves limiting |
| Database | PostgreSQL 17 | PRD §43 |
| DB access | pgx/v5 + sqlc (type-safe queries), repositories live in `internal/adapters/postgres` | Keeps SQL out of domain |
| Migrations | goose, SQL migrations embedded in the binary, `make migrate` | |
| Config | spf13/viper → typed structs → validation at boot, `APP_*` env scheme | PRD §36 |
| Logging | stdlib `log/slog`, JSON in containers | |
| First AI provider | Anthropic API via `anthropic-sdk-go` | Structured output via forced tool-use; PRD lists Anthropic as an adapter |
| AI structured output | JSON Schema contracts validated with `santhosh-tekuri/jsonschema/v6`; validate → constrained repair → retry → fail safely | PRD §62 |
| Deterministic diff | `sergi/go-diff` (diff-match-patch) at rune level, semantic cleanup, mapped back to correction spans | PRD Rule 5 |
| Event bus (in-proc) | Small in-process pub/sub behind `ports/events.EventBus`; handlers registered at bootstrap | MQTT is a Phase 3 adapter of the same port |
| MQTT broker | Eclipse Mosquitto 2 (Phase 3) | |
| Auth | Caddy `forward_auth` → Authelia (file user backend in dev); app reads `Remote-User`/`Remote-Name` headers via auth adapter, only from trusted proxy | PRD §41 |
| Dev auth escape hatch | `APP_AUTH_MODE=static` adapter that injects a fixed dev identity | Default dev stack still runs full Authelia so the real flow stays testable; static mode exists for fast inner-loop work |
| Hot reload | `air` inside the app container, source bind-mounted | |
| Mail (dev) | Mailpit (modern MailHog successor) | Phase 3 email summaries |
| Local models | Ollama service under a compose `--profile ollama` | Optional per PRD §47 |
| Lint | golangci-lint (run in container) | |
| Test libs | stdlib + testify; integration tests run inside a compose `tools` container against compose services | No testcontainers needed — compose *is* the harness |

## 4. Repository layout

```
/cmd/jlp/                 main: config load, DI wiring, tool registry, serve
/internal/domain/...      pure domain (learner, session, writing, correction,
                          vocabulary, grammar, learning, lesson, anki, event)
/internal/application/... use cases (writing, feedback, learning, lesson,
                          analytics, anki)
/internal/ports/...       ai, auth, events, storage, messaging, notifications
/internal/adapters/...    http, postgres, authelia, staticauth, anthropic,
                          (later: ollama, claudecli, mqtt, email, anki, ...)
/internal/agent/...       teacher, reviewer, planner, drill, vocabulary, lesson, anki
/internal/config/         viper loading + typed structs + validation
/internal/observability/  slog setup, AI request recording
/web/templates/           Go templates (layouts, pages, partials for HTMX)
/web/static/              CSS, vendored htmx/alpine, PWA manifest, service worker, icons
/db/migrations/           goose SQL migrations
/db/queries/              sqlc query files
/prompts/                 versioned prompt templates (teacher.feedback.v1.md, ...)
/schemas/                 JSON Schemas for AI contracts (CorrectionResult, ...)
/eval/corpus/             Japanese correction evaluation corpus (golden data)
/deploy/                  compose.prod.yml, Caddyfile, authelia/, mosquitto/
/docker-compose.yml       dev stack
/Dockerfile               multi-stage: dev (air) and prod (distroless) targets
/Makefile
```

Architectural rules 1–15 from PRD §75 are enforced by a `make arch-check` target
(depguard rules in golangci-lint: domain may not import adapters/ports impls, agents may
not import postgres, etc.).

## 5. Docker Compose topology (dev)

| Service | Image | Purpose | Port (host) |
|---|---|---|---|
| `app` | Dockerfile `dev` target (golang + air) | The Go monolith, hot reload | 8080 |
| `postgres` | postgres:17-alpine | Primary store | 5432 |
| `caddy` | caddy:2 | Reverse proxy + forward_auth | 8443 → app |
| `authelia` | authelia/authelia:4 | Auth (file backend, dev users) | 9091 |
| `mailpit` | axllent/mailpit | Email testing (Phase 3) | 8025 UI |
| `mosquitto` | eclipse-mosquitto:2 | MQTT (Phase 3, profile `mqtt`) | 1883 |
| `ollama` | ollama/ollama | Local models (profile `ollama`) | 11434 |
| `tools` | Dockerfile `dev` target | One-shot runner: tests, lint, migrations, sqlc, seed | — |

Two browser entry points, both always valid:

- `http://localhost:8080` — app directly (static identity when `APP_AUTH_MODE=static`)
- `https://jlp.localhost:8443` — through Caddy + Authelia (real login flow; dev user
  `mikey` / password in `.env`, self-signed cert)

`.env` (gitignored, `.env.example` committed) carries secrets: `ANTHROPIC_API_KEY`,
Authelia secrets, dev user password, `DEPLOY_HOST`.

## 6. Makefile command surface

```
make help                # self-documenting target list (default)
make init                # one-time: copy .env.example, generate authelia secrets
make build               # build all images + compile the binary
make up / down / restart # operate the dev stack
make logs [s=app]        # follow logs
make ps                  # stack status
make migrate             # apply goose migrations
make migrate-new n=name  # create a migration
make sqlc                # regenerate sqlc code
make seed                # load dev fixtures (identity, session, JLPT grammar concepts)
make test                # domain + application tests (fast, no services)
make test-integration    # adapter tests against compose postgres/mqtt
make lint / fmt / arch-check
make eval                # run AI eval corpus against configured provider
make db-shell            # psql into dev database
make deploy              # build prod images, deploy via DOCKER_HOST=ssh://$DEPLOY_HOST
make clean               # down -v + remove build artifacts
```

Rule: every target is a thin wrapper over `docker compose` — CI, subagents, and the
human all use identical entry points.

## 7. Deployment

Target: single home-server host on the LAN (PRD §46).

- `deploy/compose.prod.yml`: app (distroless prod image), postgres (volume-backed),
  caddy (TLS via internal CA or LAN cert), authelia, mosquitto (when Phase 3 lands).
- `make deploy` = `docker compose -f deploy/compose.prod.yml build` +
  `DOCKER_HOST=ssh://$(DEPLOY_HOST) docker compose -f deploy/compose.prod.yml up -d`,
  followed by a scripted health check (`/healthz`).
- No registry required; images build on the target via the SSH docker context.
  (A registry can be introduced later without changing the make interface.)

## 8. Core flows (MVP)

**Correction flow** (PRD §57): selection offsets (rune-based) + surrounding context +
session profile + learner priorities → Teacher agent → Writing Reviewer capability call
(StructuredGeneration against `CorrectionResult` schema) → backend validates → backend
computes deterministic rune-level diff per correction span → HTMX partial renders
original/corrected/inline-diff + per-correction explanation cards (ja/en) → learner
accepts/rejects/retries → `learning_events` appended → learner model updated → AI
request row recorded with latency/tokens/cost → optional 1–5 rating.

**Event flow:** application services append immutable `learning_events` rows and publish
on the in-process bus; consumers (learner model updater, statistics) subscribe at
bootstrap. Derived state (observations, priorities) is rebuildable from events
(`make rebuild-model` maintenance target, Phase 2).

**Identity scoping:** every repository method takes an `IdentityID`; HTTP middleware
resolves the authenticated identity or rejects. There is no unscoped query path.

## 9. Phase map (what ships when)

- **MVP (Phase 1):** compose stack, Makefile, schema/migrations, identity+auth
  (Authelia + static), sessions CRUD, writing editor (autosave, versions, selection),
  feedback pipeline with Anthropic adapter, deterministic diff UI, learning events,
  basic learner model + statistics page, AI observability + ratings, PWA shell.
- **Phase 2:** teaching planner (heuristic scoring), grammar concept tracking, vocabulary
  ingestion API + vocabulary/expression bank, active recall, drill engine, confidence
  tracking, AI quality dashboard, Ollama + CLI adapters, AI routing policies, eval corpus.
- **Phase 3:** Chrome extension (MV3), Anki (AnkiConnect + file export), tutor lesson
  guides, weekly email summaries, MQTT adapter + reading-app ingestion, richer analytics.
- **Phase 4:** A2A protocol adapter, multi-agent collaboration, messaging channels
  (Slack first, then Signal/WhatsApp), conversation tutor, advanced spaced retrieval,
  speech capabilities, learning-outcome analytics.

## 10. Verification protocol (binding for all implementation tasks)

1. **Tests first** per superpowers TDD: domain/application tests with mocked ports;
   adapter changes get integration tests (`make test-integration`).
2. **Browser verification by the implementing subagent:** after `make up`, the subagent
   uses claude-in-chrome to exercise the feature at `http://localhost:8080` (or the
   Caddy/Authelia entry point when the task touches auth), confirms expected DOM/behavior,
   and checks the browser console for errors. A task is not complete until this passes.
3. **AI-dependent tasks** must also pass schema validation tests with recorded/stubbed
   provider responses so `make test` never needs a live API key; live-provider checks are
   manual/eval-corpus concerns.
4. `make lint`, `make arch-check`, and full `make test` must pass before any task is
   considered done.

## 11. Open items deferred (not blocking)

- CodeMirror upgrade decision — revisit after MVP editor feedback.
- Registry-based deploys, Postgres backups automation — post-MVP operational hardening.
- Signal/WhatsApp adapter library choice — decide in Phase 4 planning (ecosystem moves).
