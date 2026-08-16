# A2A protocol adapter

JLP speaks the [A2A protocol](https://a2a-protocol.org/) — **version
1.0**, over the **JSON-RPC 2.0** binding — at `APP_A2A_PATH` (default
`/a2a`), dormant unless `APP_A2A_ENABLED=true`. It exposes four of
JLP's own agents as A2A skills a remote agent can discover and invoke
(PRD §29/§30).

The official client, [`@a2a-js/sdk`](https://github.com/a2aproject/a2a-js),
connects to it with no adapter, shim, or compatibility flag:

```js
import { ClientFactory } from '@a2a-js/sdk/client';

const client = await new ClientFactory().createFromUrl('http://localhost:28080/a2a/');
const task = await client.sendMessage({
  message: {
    messageId: crypto.randomUUID(),
    role: 'ROLE_USER',
    parts: [{ text: '「は」と「が」の違いを教えてください。' }],
  },
});
console.log(task.status.state, task.artifacts[0].parts[0].content.value);
```

> ### Changed: the bespoke shape is retired
>
> Until this change, this adapter served a hand-rolled REST shape —
> `POST {path}/tasks` with `{skill, input, session_id}`, `GET
> {path}/tasks/{task_id}`, and a snake_case card with
> `tasks_endpoint`/`input_schema`/`output_schema` — that was **not the
> A2A protocol**. No A2A client could speak it. Those routes are gone,
> not deprecated-in-place: there was no external consumer, and keeping
> two contracts would mean keeping two honest.
>
> | Retired | Now |
> |---|---|
> | `POST {path}/tasks` `{skill, input}` | `POST {path}/v1` → JSON-RPC `SendMessage` |
> | `GET {path}/tasks/{task_id}` | `POST {path}/v1` → JSON-RPC `GetTask` |
> | card: `tasks_endpoint` | card: `supportedInterfaces[].url` |
> | card: `skills[].input_schema`/`output_schema` | gone from the spec; skills carry `tags`/`examples`/`inputModes`/`outputModes` |
> | card: `skills[].tools` | `tags: ["tool:<name>", …]` (see "Rule 13" below) |
> | card: `capabilities.push_notifications` | `capabilities.pushNotifications` |
> | request field `session_id` | `message.metadata.session_id` |
> | request field `skill` | `message.metadata.skill` (optional — see "Skills") |

## What we implement, and against what

| | |
|---|---|
| Spec | <https://a2a-protocol.org/latest/specification/> — "the latest released version is **1.0.0**" (checked 2026-08-16) |
| Cross-checked against | `@a2a-js/sdk@1.0.1`, whose `A2A_PROTOCOL_VERSION` is `"1.0"` |
| Binding | JSON-RPC 2.0 (`protocolBinding: "JSONRPC"`) |
| Methods served | `SendMessage`, `GetTask`, `CancelTask` |
| Methods **not** served | `SendStreamingMessage`, `SubscribeToTask`, `ListTasks`, `GetExtendedAgentCard`, and the four `*TaskPushNotificationConfig` methods — all answer `-32601` |
| Capabilities | `streaming: false`, `pushNotifications: false` |

A2A v1.0's data model is derived from protobuf, so its JSON is
**protobuf JSON**. Three details differ from A2A v0.3 and from what
most people assume:

1. **A `Part` is a `oneof`, spelled by field name.** `{"text": "…"}` —
   there is **no** `kind` or `type` discriminator. (v0.3 used
   `{"kind":"text","text":"…"}`.)
2. **Enums use their full protobuf value name.** `"ROLE_USER"`,
   `"TASK_STATE_COMPLETED"` — not `"user"`, not `"completed"`.
3. **The card declares endpoints as a list**, `supportedInterfaces[]`,
   each with its own `url`/`protocolBinding`/`protocolVersion` — not a
   top-level `url` + `preferredTransport`.

One asymmetry worth knowing, because it's easy to get backwards:
`SendMessage`'s `result` is **enveloped** (`{"task": {…}}`, a
`task`|`message` oneof), while `GetTask`'s and `CancelTask`'s results
are a **bare** `Task`.

## Rule 13, and how it's enforced here

> Agents own reasoning; application services own state. A remote agent
> should not become a second source of truth for learner state.
> — PRD §30

Every task this adapter runs goes through the **exact same**
`application/agentrun.Runner` + `internal/tools.Registry` allowlists the
local agent-run path (the agentic teacher) already goes through. A
remote A2A caller gets **no privilege a local agent lacks**.

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

Every agent-card response makes this visible, not just documented: each
skill carries a `tool:<name>` **tag** per tool its agent may call, read
live from `tools.Registry.DefsFor(agent)` on every request — never a
static claim in this codebase that could drift from what the agent's
local path is actually permitted. (The retired card had a dedicated
`tools` field; v1.0's `AgentSkill` has no free-form field except
`tags`, so the disclosure moved there rather than being dropped.)

Today, `analyse_learner` and `plan_lesson` carry **no** `tool:` tags,
because `cmd/jlp/main.go` has not yet `Allow()`ed the `summary`/`lesson`
agents to call anything (only `teacher` is, for the agentic reviewer)
— those two skills still run and still return a model response, they
just can't currently investigate anything through a tool first. That's
the correct, current permission boundary, not a bug in this adapter;
widening it is a separate decision to make at the one place allowlists
are declared. Their system prompts agree with the card: a skill whose
agent has no tools is not told to "investigate using your available
tools".

## Identity

Single-learner LAN posture: this adapter runs as **the identity already
authenticated for the request** — the same identity every other route
in JLP resolves via `RequireIdentity` + `APP_AUTH_MODE`'s configured
`auth.Authenticator` (static dev identity, or Authelia forward-auth
headers from a trusted proxy). The adapter's routes are mounted
**inside** the existing authenticated route group in
`internal/adapters/http/server.go`, so Authelia/CSRF posture is
completely unchanged for A2A traffic.

The request `params` have **no identity field at all**, at any nesting
level: the JSON is decoded into structs with nowhere to put one, so a
forged `"identity"` key in the payload is simply dropped by
`encoding/json`, never read, never honoured. There is no code path from
the request body to which identity a task runs as.

Task reads are identity-scoped, and **"not found" and "not yours" are
indistinguishable** — both answer `-32001` with the same message, so
the error can never be used as an oracle to confirm that a task id
belongs to someone else. That applies to `CancelTask` too: cancelling
another identity's task reports "not found", never "not cancelable".

## Discovery: the agent card

`GET {path}/.well-known/agent-card.json`

```sh
curl -s http://localhost:28080/a2a/.well-known/agent-card.json | jq .
```

⚠️ **The trailing slash matters.** The official client resolves the card
with `new URL(".well-known/agent-card.json", baseUrl)` — a *relative*
path. Given `http://host/a2a` (no trailing slash), URL resolution drops
the last segment and fetches `http://host/.well-known/agent-card.json`,
which is not where this card lives. Use either:

```js
await factory.createFromUrl('http://localhost:28080/a2a/');                      // trailing slash
await factory.createFromUrl('http://localhost:28080', '/a2a/.well-known/agent-card.json'); // explicit path
```

The card's `supportedInterfaces[0].url` is built from the requesting
`Host` (and `X-Forwarded-Proto`, so a TLS-terminating proxy doesn't
yield an `http://` card for an `https://` deployment) — the adapter has
no configured notion of JLP's public origin. Since the card is a
self-description returned only to the caller that asked for it, nothing
server-side is keyed on that value.

## Invocation: the JSON-RPC endpoint

`POST {path}/v1`

All three methods are ordinary JSON-RPC 2.0 calls. The `id` is echoed
back verbatim. JSON-RPC-level errors come back with HTTP `200` and an
`error` member (the JSON-RPC-over-HTTP convention); the sole exception
is a missing authenticated identity, which is a transport-level
authentication failure and answers `401`.

### `SendMessage`

Runs the selected skill, **synchronously**, and returns the finished
`Task`.

```sh
curl -s -X POST http://localhost:28080/a2a/v1 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{
        "message":{"messageId":"m1","role":"ROLE_USER",
                   "parts":[{"text":"友達と映画を見ました。とても面白いでした。"}],
                   "metadata":{"skill":"review_writing"}}}}' | jq .
```

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "task": {
      "id": "…",
      "contextId": "…",
      "status": { "state": "TASK_STATE_COMPLETED", "timestamp": "2026-08-16T…Z" },
      "artifacts": [
        { "artifactId": "…", "name": "review_writing", "parts": [{ "text": "…" }] }
      ],
      "history": [
        { "messageId": "m1", "role": "ROLE_USER",  "parts": [{ "text": "…" }], "taskId": "…", "contextId": "…" },
        { "messageId": "…",  "role": "ROLE_AGENT", "parts": [{ "text": "…" }], "taskId": "…", "contextId": "…" }
      ],
      "metadata": { "skill": "review_writing" }
    }
  }
}
```

- `state` is always `TASK_STATE_COMPLETED` or `TASK_STATE_FAILED` —
  never `SUBMITTED` or `WORKING`. See "Why synchronous".
- A **failed run is not an RPC error**. It returns a `Task` in
  `TASK_STATE_FAILED` whose `status.message` carries the reason, so the
  caller still gets a task id — and with it the `/ai/agents` trace row.
- `contextId` is echoed if the client supplied one (that's it threading
  its own conversation), otherwise the server mints one.
- `-32602` for a message with no non-empty text part; `-32600` for a
  body over 1 MiB (the same cap every sibling JSON handler in JLP
  applies).

### `GetTask`

```sh
curl -s -X POST http://localhost:28080/a2a/v1 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":"<task id>"}}' | jq .
```

Result is a **bare** `Task`. `-32001` if the id is unknown *or* belongs
to another identity — deliberately the same answer.

### `CancelTask`

Always `-32002 TASK_NOT_CANCELABLE` for a task you own: every task this
adapter creates is already terminal before you could learn its id, so
there is nothing to cancel and saying otherwise would be a lie. `-32001`
for a task you don't own or that doesn't exist.

## Skills

A2A v1.0 has **no skill selector on the wire**. A client sends a
`Message`; what the agent does with it is the agent's business. Skills
are a *discovery* aid on the card, not a dispatch key.

So a plain chat message — all the official SDK's `sendMessage` produces
— routes to the conversational default, `chat`. A client that has read
the card and wants a specific skill selects it through the protocol's
own metadata map (checked on the message first, then the request):

```json
{"message": {"…": "…", "metadata": {"skill": "plan_lesson"}}}
```

An unrecognised skill id is **not** an error — it falls back to `chat`.
An A2A client is entitled to know nothing about JLP's skill ids, and
refusing its message would be refusing the protocol's normal case.

| ID | Local agent | Allowlist source |
|----|-------------|-------------------|
| `chat` (default) | `teacher` (Japanese Tutor Chat) | `cmd/jlp/main.go`'s `toolRegistry.Allow("teacher", …)` |
| `review_writing` | `teacher` (Writing Reviewer) | same |
| `analyse_learner` | `summary` (Learner Analyst) | none configured yet — runs with no tool access |
| `plan_lesson` | `lesson` (Lesson Planner) | none configured yet — runs with no tool access |

Each skill frames the task with its own system prompt (recorded as
`agent_runs.prompt_name` `a2a.chat`/`a2a.review_writing`/
`a2a.analyse_learner`/`a2a.plan_lesson`, version `v1`). This is
intentionally a different prompt from the one `internal/agent/teacher`'s
own `ReviewWritingAgentic` renders for the local agentic-teacher path —
that prompt and its template are private to that package (Rule 3); this
adapter hands `agentrun.Runner` a system string directly rather than
importing prompt-rendering machinery of its own.

### `contextId` is not a JLP session id

Tempting analogy, wrong mapping. `agent_runs.session_id` is `uuid
REFERENCES sessions(id)` — a real foreign key into JLP's own learning
sessions — whereas an A2A `contextId` is an identifier the *client*
invents for its own conversation threading. Feeding one to the other
would turn every ordinary chat into a failed insert. To scope a run's
trace to a real JLP session, pass it explicitly:

```json
{"message": {"…": "…", "metadata": {"session_id": "<a real sessions.id>"}}}
```

## Why synchronous

`SendMessage` runs the whole agent-run loop before responding — the
`state` in that response is already final. This is not a shortcut around
the protocol: v1.0's `SendMessageConfiguration.returnImmediately`
defaults to **false**, meaning "the operation MUST wait until the task
reaches a terminal or interrupted state before returning". It is also
the honest shape for what's underneath — JLP's agent-run loop is already
one bounded, `MaxTurns`-capped, synchronous call, so there is no
genuinely long-running work that would justify a queue-and-poll design.
`GetTask` exists so a client that always polls after sending still gets
the shape it expects; it will simply always find the task already done.

The task `id` is the underlying `agent_runs.id` (the same identifier the
`/ai/agents` trace viewer uses) when the run got far enough to be
assigned one. That lookup is served from a small **in-process cache**
this adapter keeps of tasks it has itself already computed — not a
second source of truth for the run (that remains the
`agent_runs`/`tool_calls` rows `agentrun.Runner` already persisted,
independently of this adapter, and which appear at `/ai/agents`
regardless of whether anyone ever calls `GetTask` at all). The cache
holds the most recent 512 tasks, evicting the oldest — every entry
holds a full model response, and the conversational default means a
chat client can create them indefinitely. A process restart empties it
entirely. Neither touches the underlying trace: a `GetTask` for an
evicted id answers "task not found", and its `/ai/agents` row is still
there.

## Worked example: the official client, end to end

```sh
npm i @a2a-js/sdk
```

```js
import { ClientFactory } from '@a2a-js/sdk/client';
import { randomUUID } from 'node:crypto';

const client = await new ClientFactory().createFromUrl('http://localhost:28080/a2a/');

const task = await client.sendMessage({
  message: { messageId: randomUUID(), role: 'ROLE_USER',
             parts: [{ text: '友達と映画を見ました。とても面白いでした。' }] },
});

console.log(task.id, task.status.state);
console.log(task.artifacts[0].parts[0].content.value);

// And read it back:
const again = await client.getTask({ id: task.id });
console.log(again.status.state);
```

The resulting run appears in `/ai/agents` (agent `teacher`, prompt
`a2a.chat`/`v1`) with its full tool-call trace, exactly like any local
agentic-teacher run.

## A ready-made client

`clients/a2a-chat/` is a small, polished chat UI built on this same
official SDK — `make a2a-chat` (http://localhost:8090). It's generic
(everything it shows comes from whatever agent card it's pointed at,
not from JLP specifically), handles the non-streaming progress state
this adapter's `streaming: false` implies, and renders
`TASK_STATE_FAILED` as an actual failure rather than an empty reply.
See `clients/a2a-chat/README.md`.
