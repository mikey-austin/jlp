# A2A chat client

A generic chat UI for **any A2A agent**, built on the official
[`@a2a-js/sdk`](https://github.com/a2aproject/a2a-js). It defaults to
talking to JLP's own A2A adapter (`docs/api/a2a.md`), but nothing about
it is JLP-specific: everything the UI shows — the agent's name,
description, version, protocol version, transport, capabilities, and
skills (with descriptions) — comes from the agent's `agent-card.json`
at runtime, not from anything hard-coded here. Point it at a different
A2A agent's base URL and it works the same way.

## Architecture

```
 browser  <--- plain fetch (JSON) --->  this Node server  <--- @a2a-js/sdk (JSON-RPC) --->  A2A agent
 (public/*.html/css/js)                 (server.js)
```

`@a2a-js/sdk` is a Node package. Running it in the browser would drag
in Node-only dependencies and require CORS support on the target
agent (JLP's A2A routes don't have any — see "Auth" below). So the SDK
client lives **server-side**: `server.js` holds one shared
`ClientFactory`-created client and exposes a small JSON API
(`/api/state`, `/api/connect`, `/api/message`, `/api/cancel`,
`/api/task`) that the static UI in `public/` talks to over ordinary
`fetch`. No build step, no framework — a few hundred lines of vanilla
JS and one dependency (the SDK itself).

## Running it

### Via Docker Compose (the committed workflow)

From the repo root:

```sh
make a2a-chat
```

This starts `postgres` + `app` (with `APP_A2A_ENABLED=true`, so JLP's
A2A routes actually exist) + this service, all under the `a2a-chat`
compose profile — it is **not** started by a plain `make up`. Open
**http://localhost:8090**.

Override the host port with `A2A_CHAT_HOST_PORT` in `.env` (default
`8090`). Stop everything with `make down` (or `docker compose --profile
a2a-chat down` if you only want this profile).

### Standalone (iterating on the UI/server without a container rebuild)

Node 20+ and npm are enough — no Docker required:

```sh
cd clients/a2a-chat
npm install
A2A_AGENT_URL=http://localhost:28080/a2a/ PORT=3000 node server.js
```

(`http://localhost:28080/a2a/` is JLP's default host-port address per
the root README — adjust if you changed `APP_HOST_PORT`, and make sure
`APP_A2A_ENABLED=true` is set for that `app` container, e.g. via
`.env` or `APP_A2A_ENABLED=true docker compose up -d app`.)

`npm run start` is equivalent to `node server.js`.

## Pointing it at a different agent

Two ways, same underlying mechanism (`ClientFactory().createFromUrl`):

1. **At startup**: set `A2A_AGENT_URL` to the target agent's base URL
   (compose: in `.env` or inline before `make a2a-chat`; standalone: as
   an env var before `node server.js`). Defaults to JLP's in-network
   address, `http://app:8080/a2a/`.
2. **At runtime**: open "Agent info" in the UI, type a different base
   URL into "Agent base URL", and click **Connect**. No restart needed.
   A trailing slash is added automatically if you omit one — see the
   gotcha below — but the target still needs to actually be an A2A
   agent serving a `.well-known/agent-card.json`.

A failed connect attempt (wrong URL, agent down, not an A2A server at
all) reports an error **without** dropping a previously-working
connection — see `connect()`'s doc comment in `server.js`.

### The trailing-slash gotcha

The SDK resolves the agent card with `new URL('.well-known/agent-card.json',
baseUrl)` — a *relative* resolution. Given `http://host/a2a` (no
trailing slash), that drops the last path segment and fetches
`http://host/.well-known/agent-card.json` instead of
`http://host/a2a/.well-known/agent-card.json`. `server.js`'s
`normalizeAgentUrl()` appends a trailing slash before ever calling
`createFromUrl`, so this client is safe against it either way — but if
you ever copy the `ClientFactory` snippet elsewhere, keep the slash.

## What to expect when the agent is slow

JLP's card advertises `capabilities: {streaming: false}`, and means
it — this client does not pretend otherwise. A reply behind a local
Ollama model takes **5–30 seconds**, and a `coordinate` run that hands
off to specialists takes **minutes**. There's no partial output to show
in the meantime, because the agent has none to give.

So this client sends `configuration: {returnImmediately: true}` and
**polls**. `SendMessage` comes back in milliseconds with a
`TASK_STATE_WORKING` task; the browser then asks `/api/task` every
second, easing off to every five, until the state is terminal — at
which point the working bubble is replaced in place by the answer.
Nothing holds an HTTP connection open for the length of a run, which is
the whole point: waiting inline used to die at the agent's write
timeout, and then at undici's 300s header timeout.

**Cancel** is therefore real. The task id exists while the work is still
going, so the **Cancel task** button next to a working task calls
`CancelTask`, which stops the run server-side and leaves the task in
`TASK_STATE_CANCELED`. (The header **Cancel** button, shown during the
send itself, still only aborts that outbound request — a window now
milliseconds wide.)

## Debugging: where to see the request/response

Every JSON-RPC call this process makes (`SendMessage`, `GetTask`,
`CancelTask`, and the initial card fetch on `connect`) is logged as one
line to stdout — method, a short summary, and the outcome (task id +
state, or the mapped error). Compose: `make logs s=a2a-chat` (or `docker
compose logs -f a2a-chat`). Standalone: it's just the terminal you ran
`node server.js` in.

That line doesn't include the raw wire JSON — for that, either open
your browser's DevTools Network tab (shows this server's own
`/api/message` etc. calls, not the agent's raw JSON-RPC), or talk to
the agent's endpoint directly with `curl`, exactly as shown in
`docs/api/a2a.md`'s worked examples, e.g.:

```sh
curl -s http://localhost:28080/a2a/.well-known/agent-card.json | jq .
curl -s -X POST http://localhost:28080/a2a/v1 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"..."}]}}}' | jq .
```

## Errors

`server.js`'s `describeError()` maps whatever the SDK throws to a
short `{kind, message}` the UI renders as an error bubble (never a
silent empty reply):

- Typed A2A errors (`JsonRpcTaskNotFoundError`,
  `JsonRpcTaskNotCancelableError`, their REST-transport twins, ...) —
  `kind` is the SDK's own class name, `message` is its message.
- A `TASK_STATE_FAILED`/`TASK_STATE_REJECTED` task is **not** an RPC
  error (JLP returns these as ordinary successful `Task`s, per the
  A2A spec) — the UI renders these as a failed-looking bubble with the
  agent's own failure reason (`task.status.message`), not an empty one.
- Network failures (DNS, connection refused, non-A2A server at the URL)
  surface as `NetworkError` with the underlying cause.
- A cancelled in-flight request surfaces as `Aborted` server-side /
  a distinct "cancelled" bubble client-side (see "Cancel" above).

## Auth

JLP's A2A routes sit inside its authenticated route group. In the dev
default **`static`** mode (`APP_AUTH_MODE=static`) an in-network request
from this container is identified automatically and needs no
credentials — that is the round trip `make a2a-chat` exercises.

In every other mode a bare request is refused. Verified, from inside
this very container:

```sh
$ wget -qO- --server-response http://app:8080/a2a/.well-known/agent-card.json
  HTTP/1.1 401 Unauthorized
```

**Set `A2A_AUTH_TOKEN` to a JLP API token with the `a2a:use` scope** and
the same call succeeds. Mint one in JLP under 設定 → APIトークン; it is
shown once. The token is attached by the SDK's `AuthenticationHandler`
to *both* the agent-card fetch and the transport — wiring only one of
them produces the confusing failure where the client connects and then
401s on the first message.

This is what an earlier version of this README described as one of the
two honest ways forward ("authenticating this Node process itself"). The
token belongs to this service, is scoped to A2A alone, and can be
revoked on its own without touching anything else.

Because the token grants only `a2a:use`, it cannot be used against the
rest of JLP's API: a request to `/api/v1/words` or `/api/v1/sessions`
carrying it gets a 403.

## Theme

Follows `prefers-color-scheme` automatically (light/dark, `public/styles.css`).
`?theme=light` or `?theme=dark` on the URL overrides it manually — handy
if you want a specific look regardless of OS setting, or for
screenshotting both palettes.

## History and sessions

Conversation history and the A2A `contextId` used to thread turns
together live **only in the browser tab's memory** — reloading the
page starts a fresh conversation (a fresh `contextId`), same as
closing a plain single-page chat tab would. Click **New conversation**
to do the same thing deliberately (and to abort anything in flight
first).

## Files

- `server.js` — the Node server: holds the SDK client, exposes the
  JSON API, serves `public/`.
- `public/index.html`, `public/styles.css`, `public/app.js` — the UI.
  Self-contained styling (system font stack, its own CSS custom
  properties) — deliberately **not** importing JLP's `web/` design
  tokens or CSS, so this stays a separate, simple app rather than an
  uncontrolled second copy of JLP's design system.
- `package.json` / `package-lock.json` — `@a2a-js/sdk` is pinned to an
  exact version (not floated); the lockfile is committed.
- `Dockerfile` — `node:20-alpine`, `npm ci --omit=dev`, listens on
  `3000` inside the container (mapped to `A2A_CHAT_HOST_PORT` by
  compose).
