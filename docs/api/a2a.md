# A2A protocol adapter

Phase 4 Task 3 (PRD §29/§30) exposes three of JLP's own agents to a
remote agent-to-agent (A2A) caller, over plain HTTP+JSON, at
`APP_A2A_PATH` (default `/a2a`) — dormant unless `APP_A2A_ENABLED=true`.

## Rule 13, and how it's enforced here

> Agents own reasoning; application services own state. A remote agent
> should not become a second source of truth for learner state.
> — PRD §30

Every task this adapter runs goes through the **exact same**
`application/agentrun.Runner` + `internal/tools.Registry` allowlists the
local agent-run path (Phase 4 Task 2's agentic teacher) already goes
through. A remote A2A caller gets **no privilege a local agent lacks**.

This isn't a policy this package merely tries to follow — it's enforced
by construction: `internal/adapters/a2a.Server` is built from exactly
two collaborators, `*agentrun.Runner` and `*tools.Registry`, and nothing
else. It has no `storage.XxxRepository`, no application-layer service,
no direct AI generator — there is no field a future change could
quietly wire a repository into without it standing out as an obviously
out-of-place addition. If a skill ever seemed to need a tool its
underlying agent isn't already `Allow()`ed (in `cmd/jlp/main.go`) to
call, the correct fix is to widen that ONE allowlist — never to give
this adapter its own side door around it.

Every `GET /a2a/.well-known/agent-card.json` response makes this
visible, not just documented: each skill's `tools` field is read live
from `tools.Registry.DefsFor(agent)` on every request — never a static
claim in this codebase that could drift from what the agent's local
path is actually permitted. Today, `analyse_learner` and `plan_lesson`
report an **empty** tools list, because `cmd/jlp/main.go` has not yet
`Allow()`ed the `summary`/`lesson` agents to call anything (only
`teacher` is, for the agentic reviewer) — those two skills still run
(and still return a model response), they just can't currently
investigate anything through a tool first. That's the correct, current
permission boundary, not a bug in this adapter — widening it is a
separate decision to make at the one place allowlists are declared.

## Identity

Single-learner LAN posture: this adapter runs as **the identity
already authenticated for the request** — the same identity every
other route in JLP resolves via `RequireIdentity` +
`APP_AUTH_MODE`'s configured `auth.Authenticator` (static dev identity,
or Authelia forward-auth headers from a trusted proxy). The adapter's
routes are mounted **inside** the existing authenticated route group
in `internal/adapters/http/server.go`, so Authelia/CSRF posture is
completely unchanged for A2A traffic.

The task request body has **no identity field at all** — `POST
/a2a/tasks`'s JSON is decoded into a struct with no place to put one,
so a forged `"identity"` key in the payload is simply dropped by
`encoding/json`, never read, never honoured. There is no code path from
the request body to which identity a task runs as.

## Endpoints

### `GET {path}/.well-known/agent-card.json`

The A2A agent card: JLP's name/description/version, its `capabilities`
(`streaming: false`, `push_notifications: false` — see below), and one
entry per skill with its `id`, `name`, `description`, `input_schema`,
`output_schema`, and live `tools` allowlist.

```sh
curl -s http://localhost:28080/a2a/.well-known/agent-card.json | jq .
```

### `POST {path}/tasks`

Runs one skill, synchronously.

```json
{
  "skill": "review_writing",
  "input": "友達と映画を見ました。とても面白いでした。",
  "session_id": "optional-existing-session-id"
}
```

| Field        | Required | Notes                                                          |
|--------------|----------|------------------------------------------------------------------|
| `skill`      | yes      | One of `review_writing`, `analyse_learner`, `plan_lesson`.       |
| `input`      | yes      | Free text: the writing to review, or context for the analysis/plan. |
| `session_id` | no       | Scopes the underlying agent-run's trace to an existing session.  |

Response (`200 OK`, always — a failed *run* is still a successful task
creation; see below):

```json
{ "task_id": "…", "status": "completed", "output": "…model's final turn…" }
```

`status` is always `"completed"` or `"failed"` — **never** `"running"`
or `"pending"`. See "Why synchronous" below.

Errors:

- `400` — unknown `skill`, or missing/blank `input`.
- `400` — malformed JSON body.
- `401` — no authenticated identity on the request (should not happen
  in practice: every route this adapter is reachable through already
  requires one).

### `GET {path}/tasks/{task_id}`

Reads back a task this same process already computed — for protocol
compatibility with an A2A client that always polls after creating a
task, not because there's ever anything left to wait for (see below).
`404` if `task_id` is unknown (including: the process restarted since
that task ran).

```sh
curl -s http://localhost:28080/a2a/tasks/<task_id>
```

## Why synchronous

`POST /a2a/tasks` runs the whole agent-run loop before responding —
`status` in that response is already final. This is a deliberate
simplification, not an oversight: JLP's agent-run loop
(`application/agentrun.Runner`) is already one bounded,
`MaxTurns`-capped, synchronous call — there is no genuinely long-running
work here that would justify a queue-and-poll design. `GET
/a2a/tasks/{task_id}` exists purely so a generic A2A client that always
polls after creating a task still gets the shape it expects; it will
simply always find the task already done. The `task_id` it looks up is
the underlying `agent_runs.id` (the same identifier the `/ai/agents`
trace viewer uses) when the run got far enough to be assigned one.

That lookup is served from a small **in-process cache** this adapter
keeps of tasks it has itself already computed — not a second source of
truth for the run (that remains the `agent_runs`/`tool_calls` rows
`agentrun.Runner` already persisted, independently of this adapter, and
which appear at `/ai/agents` regardless of whether anyone ever calls
`GET /a2a/tasks/{id}` at all). A process restart empties this cache; it
does not touch the underlying trace.

## Skills

| ID | Local agent | Allowlist source |
|----|-------------|-------------------|
| `review_writing` | `teacher` (Writing Reviewer) | `cmd/jlp/main.go`'s `toolRegistry.Allow("teacher", ...)` |
| `analyse_learner` | `summary` (Learner Analyst) | none configured yet — runs with no tool access |
| `plan_lesson` | `lesson` (Lesson Planner) | none configured yet — runs with no tool access |

Each skill frames the task with its own system prompt (recorded as
`agent_runs.prompt_name` `a2a.review_writing`/`a2a.analyse_learner`/
`a2a.plan_lesson`, version `v1`) asking the underlying agent to
investigate the learner's own history through whatever tools it's
allowed first, then respond in prose. This is intentionally a
different prompt from the one `internal/agent/teacher`'s own
`ReviewWritingAgentic` renders for the local agentic-teacher path
(Task 2) — that prompt and its template are private to that package
(Rule 3); this adapter hands `agentrun.Runner` a system string
directly rather than importing prompt-rendering machinery of its own.

## Example: `review_writing` end to end (fake provider)

```sh
curl -s http://localhost:28080/a2a/.well-known/agent-card.json | jq .

curl -s -X POST http://localhost:28080/a2a/tasks \
  -H "Content-Type: application/json" \
  -d '{"skill":"review_writing","input":"友達と映画を見ました。とても面白いでした。"}'
# => {"task_id":"…","status":"completed","output":"Based on your learning priorities, you should focus on … next."}

curl -s http://localhost:28080/a2a/tasks/<task_id>
```

The resulting run appears in `/ai/agents` (agent `teacher`, prompt
`a2a.review_writing`/`v1`) with its full tool-call trace, exactly like
any local agentic-teacher run.
