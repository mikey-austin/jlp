# Chrome extension: token auth and selection actions — design

**Goal:** the extension works again under OIDC, and selecting text
anywhere on the web offers four things to do with it.

**Status:** approved 2026-08-18. Second of two projects; the first
(rich A2A content) is deployed.

## The blocker

Every call in `popup.js` uses `credentials: "include"`. Under
`APP_AUTH_MODE=oidc` the session cookie is `SameSite=Lax`, so it never
travels from an extension origin: **the extension has been dead since
the OIDC cutover.** It moves to `Authorization: Bearer <token>`, the
mechanism built for precisely this.

Nothing else in the extension changes shape. It remains a pure client of
the JSON API (PRD §40), holding no learning-domain logic.

## Token storage

The token lives in `chrome.storage.local`.

Deliberately not `chrome.storage.sync`, where `baseUrl` and `sessionId`
already live: a credential replicated to every device signed into the
Chrome profile is a different security posture than a URL, and putting
both in one store makes that difference invisible. The options page
writes each to its own store.

## Scopes

The extension does more than write vocabulary, so two scopes are added
to `internal/application/apitoken`:

| Scope | Grants | Used by |
|---|---|---|
| `vocabulary:write` (exists) | save words | 語彙を保存 |
| `sessions:write` (new) | create a session, optionally with text | 新しいセッション |
| `feedback:request` (new) | ask for corrections on a piece of text | 添削 |
| `a2a:use` (exists) | drive the A2A adapter | the chat server (not the extension) |

Separate rather than one combined `writing:review`, because "can park
text in my account" and "can spend money on AI corrections" are
different grants — the second costs real tokens on every call.

`requiredScope` in `internal/adapters/http/apiauth.go` maps the routes:

- `POST /api/v1/sessions` → `sessions:write`
- `POST /api/v1/sessions/{id}/feedback` → `feedback:request`
- `POST /api/v1/words`, `POST /api/v1/vocabulary/events` → `vocabulary:write`

Default-deny is unchanged: a route no scope covers stays refused.

## Server addition: text on session creation

`POST /api/v1/sessions` gains an optional `text` field. When present,
the session is created with that text as its document.

This is what makes "park this for later" expressible. Today the only way
text reaches JLP through the API is riding on a feedback request, which
means the "new session" action could not differ from the "feedback"
action — it would be the same call with an extra session.

Empty or absent `text` behaves exactly as today, so existing callers are
unaffected.

## Chat addition: pre-filled composer

`clients/a2a-chat` accepts `?q=<text>`, which **pre-fills the composer
and does not send**.

Not auto-sending is the point: a right-click that silently spends money
on a model call is a spend the reader did not confirm, and the text is
often worth editing first (a selection picks up navigation chrome and
stray whitespace).

## The four actions

All on a selection, in `background.js`'s context menu:

```
JLPに語彙を保存       → POST /api/v1/vocabulary/events   (exists; auth fixed)
JLPで添削             → POST /sessions/{id}/feedback     (exists; auth fixed)
JLPで新しいセッション  → POST /api/v1/sessions {text}     (new)
JLPでチャット         → open <chat>/?q=<text>            (new)
```

The first three continue through the existing popup window, which is a
real `chrome.windows.create` window rather than an action popup — it
survives losing focus, which an action popup does not.

The fourth opens a tab and needs no popup at all: the extension contains
no A2A code. One chat implementation, the deployed one.

### Known failure mode

The chat action depends on the a2a-chat service running AND an Authelia
session in that tab. Signed out, a right-click lands on a login page
with the text in the URL. Accepted deliberately as the cost of not
maintaining a second chat client; documented in the extension README so
it reads as a known behaviour rather than a bug.

## Configuration

The options page gains:

- **API token** (`chrome.storage.local`) — with a link to
  `<baseUrl>/settings/tokens` to mint one.
- **Chat URL** (`chrome.storage.sync`) — where the deployed chat lives.
  Empty hides the chat menu item entirely rather than offering an action
  that cannot work, the same rule `A2AChatURL` follows in the app's nav.

## Testing

Go, where the risk is:

- Each new route requires its scope; a token without it gets 403.
- A session created with `text` has that text as its document; without
  `text`, behaviour is byte-identical to today.
- The existing default-deny still holds for uncovered routes.

Extension: the repo has a `shim/chrome-shim.js` for driving `popup.js`
without a browser. Auth-header construction and the menu-to-action
mapping are covered there; the end-to-end path is browser-verified
against the running stack, per the standing rule.

## Out of scope

- Embedding a chat in the extension. Rejected in favour of one chat
  implementation.
- Any learning-domain logic in the extension (PRD §40).
- Rendering widgets in the extension popup: the popup shows corrections
  through its own existing markup, and the chat is where widgets live.
