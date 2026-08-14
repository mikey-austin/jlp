# JLP Complete-PRD Implementation Roadmap

> **For agentic workers:** This is the master plan for the complete PRD. Phase 1 is fully
> detailed in `2026-08-14-jlp-phase1-mvp.md` — execute that with
> superpowers:subagent-driven-development. Phases 2–4 are specified here at task level;
> **when a phase starts, generate its detailed bite-sized plan with the
> superpowers:writing-plans skill from this roadmap + the spec**, then execute it the same
> way. Do not write Phase N+1's detailed plan before Phase N ships — later details depend
> on earlier reality.

**Goal:** Implement the entire Japanese Learning Platform PRD (`docs/japanese_learning_platform_jlp_prd.md`) — a personal adaptive language-learning system with the learner model and learning events as the stable core.

**Architecture:** Modular monolith (one Go binary), hexagonal (PRD §34/§75). Every phase adds modules/adapters/agents/event-consumers — never cross-cutting rewrites (Rule 15). Delivery is Makefile-driven on Docker Compose; deployment is a LAN home server via `make deploy`.

**Tech Stack:** see design spec `docs/superpowers/specs/2026-08-14-jlp-platform-design.md` §3 (authoritative decision table).

## Global Constraints (all phases)

- All build/operate/deploy actions are `make` targets; dev runs entirely in Docker Compose; the Go toolchain is containerized.
- **Implementing subagents verify their own work in Chrome** via claude-in-chrome against the local stack before a task counts as done (protocol: spec §10).
- PRD §75 Rules 1–15 enforced by `make arch-check` (depguard); new packages get depguard rules in the same task that creates them.
- All learner state identity-scoped (Rule 6); AI output schema-validated (Rule 4); prompts versioned files in `internal/prompts/templates/` (Rule 10); every AI call goes through the observability decorator (Rule 11); external side effects (email, Anki publish, messaging) require explicit config/approval (Rule 12, PRD §65).
- Rune-based text handling everywhere; tests include Japanese strings.
- `make test` never needs a live AI key or network.

---

## Phase map

| Phase | Theme | Detailed plan |
|---|---|---|
| 1 (MVP) | The core loop: write → feedback → learn → record → adapt | `2026-08-14-jlp-phase1-mvp.md` (Tasks 1–19, ready to execute) |
| 2 | Adaptive teaching: planner, grammar/vocab tracking, drills, multi-provider AI | this doc §Phase 2 → detailed plan at phase start |
| 3 | Reach: Chrome extension, Anki, tutors, email, MQTT/reading ingestion | this doc §Phase 3 → detailed plan at phase start |
| 4 | Ecosystem: A2A, multi-agent, messaging channels, conversation, speech | this doc §Phase 4 → detailed plan at phase start |

Dependency rule between phases: strictly sequential. Within a phase, tasks list their intra-phase dependencies; independent tasks may be parallelized with worktree isolation.

---

## Phase 2 — Adaptive Teaching Engine

### P2.1 Grammar concept tracking (PRD §10)
- **Modules:** `internal/domain/grammar` (`Concept{ID slug, Name, JLPTLevel, Description, Examples []string, Related, Prerequisites []string}`), migration `grammar_concepts` + `correction_concepts` join table, curated seed data `data/grammar/jlpt-n5-n1.yaml` (start ~150 concepts, N5→N2), loader in `jlp seed`.
- **Behavior:** the teacher prompt (bump to `teacher.feedback.v2`) instructs the model to tag corrections with concept slugs from a provided candidate list; backend resolves slugs against the table (unknown slugs recorded as `unresolved` for later curation); `GrammarConceptEncountered` events emitted per tagged correction.
- **UI:** `/grammar` page — concepts with per-concept encounter/error counts; concept detail with linked corrections.
- **Accept:** correction cards show concept chips linking to concept pages; events visible in activity; browser-verified.

### P2.2 Learner observations, priorities, heuristic teaching planner (PRD §13, §16, §44)
- **Modules:** migrations `learner_observations` (`{id, identity, kind, subject, confidence, evidence jsonb, updated_at}`), `learner_priorities` (`{identity, subject_type, subject, score, reason, updated_at}`); event consumers (subscribed at bootstrap) deriving observations: recurring-error detector (≥3 presented corrections sharing concept/type in 30 days ⇒ weakness), improvement detector (weakness with no recurrence in 14 days ⇒ emerging strength); planner service computing `score = persistent_error × recent_frequency × learning_value` (learning_value from JLPT level proximity, default 1.0); `jlp rebuild-model` command replaying `learning_events` from scratch (PRD §14 rebuildability) + `make rebuild-model`.
- **Behavior:** `teacher.ReviewWriting`'s `RecentErrors` (empty since MVP) now filled from top-5 priorities — closing the adapt loop.
- **Accept:** after seeding repeated conjugation errors, priorities list ranks the concept; feedback prompt (visible in ai_requests debug view) contains it; `make rebuild-model` reproduces identical observations from events alone. Browser-verified via a `/learner` priorities page.
- **Depends:** P2.1.

### P2.3 Vocabulary system + ingestion API (PRD §11, §12)
- **Modules:** `internal/domain/vocabulary` (`Item{ID, Identity, Expression, Reading, Meaning, PartOfSpeech, JLPTLevel, Kind word|expression|collocation|pattern, Source, Counts{Lookups, Productions, SuccessfulProductions}, FirstSeen, LastReviewed, Confidence}`); migrations `vocabulary_items`, `vocabulary_events`; ingestion endpoint `POST /api/v1/vocabulary/events` accepting the PRD §12 event shape (type `vocabulary.lookup` etc.), idempotency via client event IDs; production detection: on each feedback round, match the identity's vocabulary expressions against the reviewed text → `VocabularyProduced` / `VocabularyProducedCorrectly` (no correction touched it) events updating counts.
- **UI:** `/vocabulary` — filterable list (seen/looked-up/reviewed/produced ladders per PRD §11.2).
- **Accept:** curl-style ingestion via `make demo-ingest` (sample payload) shows the item in the UI; writing a sentence using an ingested word flips it to produced. Browser-verified.

### P2.4 Expression bank + vocabulary activator (PRD §55, §17.5)
- **Modules:** expression seed data (`data/expressions/core.yaml` with PRD's examples et al.); activation-candidate query (encountered ≥3×, produced 0×); planner exposes `VocabularyWorthActivating`; teacher prompt v2 gains an "encourage these expressions when natural" block.
- **Accept:** an ingested-but-never-produced expression appears in `/vocabulary?filter=activate` and in the rendered prompt. Browser-verified.
- **Depends:** P2.3, P2.2.

### P2.5 Active recall mode (PRD §9, §53)
- **Modules:** schema `correction_result.v2` adds optional per-correction `hint` (ja/en); teacher-mode `socratic` renders hint-first cards (answer hidden); UI retry-in-place: learner edits the flagged span in a small input, re-submits just that span for evaluation (reuses feedback pipeline on the span); events `HintRequested`, `HintUsed`, `CorrectionRetried` with attempt counts + independent-solve flag; confidence prompt (1–5 scale widget) after resolution → `ConfidenceRecorded` events.
- **Accept:** in socratic mode the answer is hidden until reveal; solving from hint records `independent: true` evidence. Browser-verified.
- **Depends:** P2.2 (events feed the model).

### P2.6 Drill engine (PRD §17.2, §58)
- **Modules:** migrations `exercises`, `exercise_attempts`; `internal/agent/drill` + prompt `drill.generate.v1` + schema `exercise.v1` (types: fill-in-blank, multiple-choice, transformation, free-production); `/practice` page: planner picks the top-priority concept → drill agent generates a contextual exercise → deterministic evaluation for closed types, AI evaluation (schema `exercise_eval.v1`) for free production → `QuizStarted/QuizAnswered/QuizCompleted` events; fakeai extended with canned exercises so dev/browser verification needs no key.
- **Accept:** full practice round in the browser; attempt + events in DB; wrong answer decreases nothing (encouragement per PRD §56 — copy reviewed against "reinforce competence" principle).
- **Depends:** P2.2.

### P2.7 AI quality dashboard (PRD §26, §15-AI)
- **Modules:** extend `/ai` into sections: provider comparison (requests, avg rating, success rate, p50/p95 latency, cost by provider+model+capability), capability performance, quality-over-time by prompt_version (data already recorded since MVP). SQL views + plain HTML tables/tiles (dataviz charts only if genuinely needed — sparklines via inline SVG at most).
- **Accept:** after mixed fake/anthropic usage, the comparison table separates providers; a prompt bump (v1→v2 from P2.1) shows as distinct rows. Browser-verified.

### P2.8 Ollama adapter + AI routing (PRD §23, §24)
- **Modules:** `internal/adapters/ollama` (native `/api/chat` with `format: <json-schema>` structured output); compose `ollama` profile + `make ollama-pull m=qwen3` style target; `internal/adapters/airouter` implementing `ai.StructuredGenerator`: config-driven policy `APP_AI_ROUTES` (per prompt-name capability: preferred provider + ordered fallbacks, e.g. writing correction → anthropic, fallback ollama), fallback on error/timeout; router sits UNDER the observability decorator so each attempt is recorded with its real provider.
- **Accept:** with Anthropic key removed and Ollama profile up, feedback still works via fallback; ai_requests shows the failover pair. Browser-verified.

### P2.9 CLI adapters (PRD §23: Claude CLI, Codex CLI)
- **Modules:** `internal/adapters/claudecli` (`claude -p <prompt> --output-format json` with schema-in-prompt + validate/repair path), `internal/adapters/codexcli` analogous; binaries + auth are host resources — these adapters are **host-mode only** (documented; compose app container can't see host CLIs; a `cli-bridge` sidecar mounting the host binaries is the pattern if containerized use is needed — decide during phase planning).
- **Accept:** unit tests with a stub executable on PATH; manual smoke documented. (No browser step — no UI surface.)

### P2.10 AI evaluation corpus + `make eval` (PRD §48, §49)
- **Modules:** `eval/corpus/*.yaml` cases (`{selection, context, must_correct: [substr], must_not_correct: [substr], notes}`) — ≥40 cases spanning grammar errors, unnatural phrasing, acceptable alternatives, casual/formal, deliberately-correct text (false-positive traps); `jlp eval` runner scoring correction precision/recall/false-positive rate per provider+prompt-version, report to `eval/reports/<timestamp>.md`; `make eval` (uses configured provider; fake provider gives a smoke-run).
- **Accept:** `make eval` produces a scored report; regression = score drop vs previous report is flagged in output.
- **Depends:** P2.8 (multi-provider comparison is the point).

**Phase 2 Makefile additions:** `rebuild-model`, `ollama-pull`, `demo-ingest`, `eval`.

---

## Phase 3 — Reach: Extension, Anki, Tutors, Email, MQTT

### P3.1 Chrome extension (PRD §40)
- **Modules:** `chrome-extension/` (MV3: manifest, service worker, content script, popup) — a pure API client of `/api/v1` (no learning logic, PRD rule); options page stores base URL; auth rides the Authelia session cookie (user logs in via browser first; `credentials: 'include'`); actions: request correction on selected text (rendered in popup with the same severity semantics), save selection as vocabulary lookup (`/api/v1/vocabulary/events`), open related session; `make ext-build` zips a loadable artifact.
- **Verification note:** subagents can't install extensions; verify by (a) API contract tests, (b) driving the popup/options pages directly as file-served pages in Chrome with a stubbed `chrome.*` shim, (c) a documented manual load-unpacked checklist for the user.
- **Depends:** MVP API (Task 16), works better after P2.3 (vocab save).

### P3.2 Anki integration (PRD §19)
- **Modules:** migration `anki_cards` (`{id, identity, source_type, source_id, front, back, status draft|approved|rejected|exported, created_at}`); `internal/agent/anki` + prompt `anki.generate.v1` + schema `anki_card.v1` (contextual cards in the PRD §19 style, generated from accepted corrections + recurring weaknesses); review queue UI `/anki`: approve/edit/reject/batch-approve; export adapters: AnkiConnect (`internal/adapters/ankiconnect`, HTTP to host `localhost:8765` — reachable via `host.docker.internal`, config-gated) and TSV file export (download); `AnkiCardCreated` events; auto-publish OFF by default (PRD §64/65).
- **Accept:** accepted correction → draft card appears → approve → TSV export downloads; with AnkiConnect configured, card lands in Anki. Browser-verified (TSV path; AnkiConnect behind config flag).

### P3.3 Human tutor lesson guides (PRD §18, §60)
- **Modules:** migrations `lessons`, `lesson_observations`; `internal/agent/lesson` + `lesson.generate.v1` + schema `lesson_plan.v1` (PRD §18 contents: level, strengths, weaknesses, focus, vocab, grammar, prompts, exercises, recent examples, questions); `/lessons` UI: generate guide (rendered, print-friendly CSS for sharing), post-lesson observation form (tutor/learner notes + per-concept ratings) → `TutorLessonStarted/Completed` events + observations feeding the learner model.
- **Accept:** guide generation reflects current priorities; recording an observation shifts the relevant priority. Browser-verified.
- **Depends:** P2.2.

### P3.4 Weekly email summary (PRD §21) + notifications port
- **Modules:** `internal/ports/notifications` (`Notifier interface { Send(ctx, n Notification) error }`); `internal/adapters/smtp` (Mailpit in dev — compose service from spec §5); progress-analyst agent + `summary.generate.v1` + schema `weekly_summary.v1` (accomplishments, improvements, persistent weaknesses, new expressions, activated words, recommended focus, optional challenge — encouraging tone per PRD); in-app scheduler (robfig/cron, config `APP_SUMMARY_CRON`, default Sunday 18:00) gated by `APP_SUMMARY_ENABLED` (explicit opt-in = the PRD §65 approval boundary for external communication).
- **Accept:** `make send-summary` (manual trigger target) → email visible in Mailpit UI at `http://localhost:8025`. Browser-verified in Mailpit.
- **Depends:** P2.2 (content), P2.3 (vocab sections).

### P3.5 MQTT adapter + external reading ingestion (PRD §31, §32, §12, §59)
- **Modules:** compose `mosquitto` service (profile `mqtt`, authenticated listener); `internal/adapters/mqtt` bridging the in-proc bus: outbound — learning events published to `learner/{identity}/{category}/…` topics (transport-independent envelope, Rule 7/8); inbound — subscribes `learner/{identity}/vocabulary/ingest` and feeds the same application service as the HTTP ingestion endpoint; reconnect/backoff; integration tests against compose mosquitto.
- **Accept:** `make mqtt-tap` (mosquitto_sub helper target) shows events flowing during a feedback round; publishing a lookup event via `make mqtt-demo` appears in `/vocabulary`. Browser-verified for the UI side.
- **Depends:** P2.3.

### P3.6 Richer analytics (PRD §15, §51 full)
- **Modules:** vocab funnel (looked-up → reviewed → produced → retained), weakness trend lines (recurrence decay over weeks), confidence-vs-correctness calibration view, agents/system sections (agent runs, event throughput); SQL views + server-rendered sections; inline-SVG sparklines where a trend genuinely needs a line (follow dataviz skill when implementing).
- **Accept:** dashboard sections match PRD §51 headings; numbers reconcile with raw SQL spot-checks. Browser-verified.

**Phase 3 Makefile additions:** `ext-build`, `send-summary`, `mqtt-tap`, `mqtt-demo`, `backup` (pg_dump to `backups/`, added here as prod data becomes precious).

---

## Phase 4 — Ecosystem: A2A, Multi-Agent, Channels, Conversation, Speech

### P4.1 Tool registry + agentic teacher (PRD §27, §28, §29, §50, §64)
- **Modules:** `internal/tools` registry: typed tools (`get_active_session, get_session_context, get_learner_profile, get_learning_priorities, get_recent_errors, get_grammar_history, get_vocabulary_history, search_vocabulary, get_recent_writing, get_correction_history, record_learning_event, create_exercise, create_lesson_plan, create_anki_card`) each with JSON Schema + per-agent allowlist (PRD §64 permission matrix); registered at bootstrap grouped per PRD §29; teacher becomes multi-turn tool-calling (extend `ports/ai` with `ToolCalling` capability; Anthropic + Ollama adapters implement it); `agent_runs` + `tool_calls` tables tracing every run (PRD §50); trace viewer under `/ai/agents`.
- **Accept:** a feedback round's trace shows tool calls (`get_learner_priorities` etc.) in the viewer; an agent calling a non-allowlisted tool is rejected and logged. Browser-verified.

### P4.2 A2A protocol adapter (PRD §30)
- **Modules:** `internal/adapters/a2a` exposing selected agents (reviewer, analyst, lesson planner) via the A2A protocol (agent cards, task lifecycle) as an integration boundary — remote agents consume/serve structured contracts; identity context propagated; remote agents never write learner state directly (Rule 13) — they go through the same tool registry with their own permissions.
- **Accept:** a2a-client integration test completes a review task end-to-end; trace recorded like local runs.
- **Depends:** P4.1.

### P4.3 Slack channel (PRD §20)
- **Modules:** `internal/ports/channels` (`Channel` port: inbound message → application command, outbound rendering per channel style); `internal/adapters/slack` (Socket Mode; no public ingress needed on a LAN): short exercises, quick corrections, daily nudge — channel-appropriate brevity (PRD §20.1); outbound messages gated by approval config (PRD §65).
- **Accept:** DM a sentence to the bot → correction reply threads back; events recorded identically to PWA-originated ones (domain never sees "slack").
- **Depends:** P4.1 (agents), P2.6 (exercises).

### P4.4 Signal + WhatsApp channels (PRD §20)
- **Modules:** same `channels` port; Signal via `signal-cli` (JSON-RPC mode in a sidecar container), WhatsApp via the Business Cloud API webhook (needs tunnel/ingress — config-gated, may stay dormant on pure-LAN installs). Library/approach re-validated at phase planning (ecosystem churn).
- **Accept:** per-channel echo of the P4.3 acceptance flow.
- **Depends:** P4.3 (port shape proven).

### P4.5 Conversation tutor (PRD §17.4)
- **Modules:** conversation mode in the workspace (chat pane variant) + conversation agent + `conversation.turn.v1` prompt/schema; feedback timing modes: immediate / delayed / end-of-conversation summary (session profile option); conversation events feed vocabulary production + grammar encounters like writing does.
- **Accept:** a 6-turn conversation with end-summary produces a digest of issues + events. Browser-verified.
- **Depends:** P4.1.

### P4.6 Advanced spaced retrieval (PRD §54)
- **Modules:** retrieval scheduler: per learning-point (concept/expression) next-review time from success/failure history, elapsed time, production frequency, confidence, importance; surfaces as (a) drill selection bias, (b) "use it again" nudges inside writing sessions ("先週この表現を正しく使いました。この文でも使ってみましょうか"), (c) resurfacing queue on the dashboard.
- **Accept:** a mastered item's resurfacing interval visibly grows; a failed retrieval shortens it (inspectable `/learner` schedule view). Browser-verified.
- **Depends:** P2.5, P2.6.

### P4.7 Speech module (PRD §66)
- **Modules:** `ports/ai` capabilities `SpeechRecognition`/`SpeechSynthesis`; adapters: local whisper (via Ollama-adjacent runner or whisper.cpp sidecar) + a cloud STT/TTS provider behind config; PWA audio capture (MediaRecorder) in conversation mode; spoken answers transcribed → the SAME feedback pipeline (the learner-model hypothesis: written formulation transfers to speech — now measurable); pronunciation evaluation deferred to its own follow-up task.
- **Accept:** record a spoken sentence → transcription appears in the conversation → correction round works. Browser-verified (mic permission granted to the tab).
- **Depends:** P4.5.

### P4.8 Learning-outcome analytics (PRD §52, §72, §73)
- **Modules:** outcome linker: for each accepted correction/concept, detect later independent correct production (no correction on the same concept in subsequent writing) → `outcome` measures: recurrence decay per concept, corrected→retained conversion, confidence-calibration trend, "assistance fading" report (PRD's paradoxical success criterion); dashboard "learning outcomes" section.
- **Accept:** synthetic event replay produces the expected decay curves; dashboard renders the outcome section. Browser-verified.
- **Depends:** everything — this is the capstone.

**Phase 4 Makefile additions:** `signal-register`, `channels-status`, plus per-adapter compose profiles.

---

## PRD traceability

| PRD § | Covered by |
|---|---|
| 1–5 (vision, goals, principles, UX) | MVP Tasks 6–7, 13–14 (3-pane UX, frictionless feedback); principles enforced throughout |
| 6 Sessions / 7 Teacher profiles | MVP Task 6 (profile incl. teacher mode); modes deepen in P2.5 (socratic), P4.5 (conversation) |
| 8 Correction workflow | MVP Tasks 9–13 |
| 9 Active recall / 53 Confidence | P2.5 |
| 10 Grammar knowledge | P2.1 |
| 11 Vocabulary / 12 Reading integration / 55 Expression bank | P2.3, P2.4, P3.5 |
| 13 Learner model / 16 Teaching engine / 44 Event-vs-derived | MVP Task 8 + 14 (basic), P2.2 (full) |
| 14 Learning events | MVP Task 8, extended every phase |
| 15 Analytics / 51 Dashboard | MVP Task 14, P2.7, P3.6, P4.8 |
| 17 Learning modules | 17.1 MVP; 17.2 P2.6; 17.3 P2.6 (explain-type exercise); 17.4 P4.5; 17.5 P2.4; 17.6 P3.3 |
| 18 Tutors / 60 flow | P3.3 |
| 19 Anki | P3.2 |
| 20 Channels / 21 Email | P3.4 (email), P4.3–P4.4 (messaging); PWA+API from MVP |
| 22–26 AI architecture, adapters, routing, observability, quality | MVP Tasks 9, 11, 15; P2.7–P2.9 |
| 27–29 Agents, tools, registry | MVP single-shot agents; P4.1 full registry |
| 30 A2A | P4.2 |
| 31–33 Bus, MQTT, event arch | MVP Task 8 (bus port); P3.5 (MQTT) |
| 34–38 Architecture, packages, config, HTTP, HTMX | MVP Tasks 1–3, 16, 18 |
| 39 PWA / 40 Chrome extension | MVP Task 17; P3.1 |
| 41–42 AuthN/AuthZ | MVP Tasks 4–5; identity scoping every task |
| 43 Storage | MVP Task 3 onward, one migration per aggregate |
| 45 Privacy | provider config + ai_requests audit (MVP 9/15), local models P2.8, retention config P3 hardening |
| 46–47 Deployment, Compose | MVP Tasks 1, 19 |
| 48–49 Testing, AI eval | test pyramid every task; P2.10 corpus |
| 50 Agent observability | MVP Task 9 (requests); P4.1 (runs/tool traces) |
| 52 Feedback quality / 72–73 Success metrics | MVP Task 15 (ratings); P4.8 (outcomes) |
| 54 Spaced retrieval | P4.6 |
| 56 Motivation | copy principles in MVP Task 14 + P2.6; no streaks/gamification anywhere |
| 57–59 Example flows | MVP Task 13 (§57), P2.6 (§58), P3.5 (§59) |
| 61–63 API, contracts, prompts | MVP Tasks 9, 16 |
| 64–65 Agent security, approval | config gates P3.2/P3.4/P4.3; tool allowlists P4.1 |
| 66 Speech | P4.7 |
| 67–70 Phases | this roadmap |
| 71, 74–78 Evolution, rules, core model | modular monolith + arch-check (MVP Task 18); learner-model-as-core informs every boundary |

## Operating model recap

- `make help` is always the entry point; every phase keeps it current.
- Execution per phase: writing-plans → subagent-driven-development → each subagent: TDD → `make test`/`lint`/`arch-check` → **own Chrome verification** → commit. Code review via superpowers:requesting-code-review at epic boundaries.
- Risks to watch: Authelia config schema drift (pin image tag once working); Anthropic SDK type churn (wire contract is the invariant); textarea editor limits (CodeMirror decision deferred per spec §11); WhatsApp/Signal ecosystem churn (re-validate at P4 planning).
