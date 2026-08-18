# JLP Chrome extension

A pure client of JLP's existing JSON API (PRD §40): the extension holds
no learning-domain logic of its own — no correction rules, no scoring,
nothing that could drift from the server. It reads the same
`/api/v1/sessions`, `/api/v1/sessions/{id}/feedback`, and
`/api/v1/vocabulary/events` DTOs the app itself uses (see
`internal/adapters/http/api.go`), right-clicks Japanese text anywhere on
the web, and shows the result in a small popup styled to match the app
(`popup.css` hand-mirrors `web/static/css/app.css`'s tokens — accent
`#b7282e`, insertion/deletion colors, severity badges — there's no build
step or shared stylesheet to import it from).

No build tooling: plain ES modules/scripts, loaded straight off disk by
Chrome. `make ext-build` just zips the directory (see below) — there is
nothing to compile.

## Authentication: an API token, not cookies

Every call sends `Authorization: Bearer <token>`. There is no cookie
anywhere in this extension, and adding one back would not work.

This is not a preference. Under `APP_AUTH_MODE=oidc` the app's session
cookie is `SameSite=Lax`, so it never travels from an extension origin —
`credentials: "include"` produced a silent 401 on every call, and the
extension was dead from the OIDC cutover until this change. No amount of
host permissions fixes that; it was never a CORS problem.

**Setup:** mint a token in JLP under 設定 → APIトークン and paste it into
this extension's options page. It needs three scopes:

| Scope | Used by |
|---|---|
| `vocabulary:write` | JLPに語彙を保存 |
| `sessions:write` | JLPで新しいセッション |
| `feedback:request` | JLPで添削 |

`sessions:write` and `feedback:request` are separate on purpose: parking
text costs nothing, while every feedback request spends real money on a
model call. A token that only files things away cannot run up a bill.

The token is stored in `chrome.storage.local`, deliberately not `.sync`
where the base URL and default session live — a credential replicated to
every device signed into the Chrome profile is a different posture than
a URL.

## The four actions

Select text anywhere, right-click:

| Menu item | What it does |
|---|---|
| JLPで添削 | Asks for corrections on the selection, in your default (or most recent) session. Costs a model call. |
| JLPで新しいセッション | Creates a new session with the selection as its text. No AI call, nothing to wait for — the "work on this later" action. |
| JLPに語彙を保存 | Saves the selection as a vocabulary lookup. |
| JLPでチャット | Opens the deployed A2A chat with the selection pre-filled. |

「JLPでチャット」 appears **only when a chat URL is configured** in the
options page — a menu item that cannot work is worse than an absent one.

### Known failure mode for the chat action

It opens the deployed chat, so it needs that service running AND an
Authelia session in that tab. Signed out, you land on a login page with
your text in the URL. That is the accepted cost of having one chat
implementation rather than a second one embedded here; the extension
contains no A2A code at all.

The chat pre-fills the composer and does **not** send. A right-click
that silently spends money on a model call is a spend you never
confirmed, and a web selection usually needs a trim first.

## Install (manual, `chrome://extensions`)

1. `make up && make migrate && make seed` (see the root README) so
   there's a running app with at least one session to point at.
2. Open `chrome://extensions` in Chrome.
3. Enable **Developer mode** (top-right toggle).
4. Click **Load unpacked**, and select this `chrome-extension/` directory
   (not a zip — `make ext-build`'s zip is for distribution, not for
   `chrome://extensions` itself, which wants an unpacked directory).
   Chrome shows the generic gray puzzle-piece icon for it in the
   toolbar/extensions list — that's expected, not a missing asset:
   `manifest.json` deliberately omits `icons`/`action.default_icon`
   rather than rasterizing `web/static/icons/icon.svg` to the PNGs MV3
   requires (SVG isn't a valid extension icon format), which would mean
   adding an image-conversion step to a project that otherwise has no
   build tooling at all for this extension.
5. Click the extension's **Details → Extension options** (or the
   toolbar icon's right-click menu → Options) and set:
   - **Base URL** — where your JLP app is running, e.g.
     `http://localhost:8080` (or whatever `APP_HOST_PORT` you set — see
     the root README's Quickstart). Saving requests the
     `optional_host_permissions` grant for that origin (a Chrome
     permission prompt) — accept it, or extension fetches to that origin
     will be blocked like any other cross-origin page fetch.
   - **Default session ID** — optional; leave blank to always use your
     most recently created session (`GET /api/v1/sessions` returns
     newest-first).
6. On any page, select some Japanese text, right-click, and use either:
   - **JLPで添削** — opens a popup showing corrections for the
     selection (rune-accurate offsets, socratic hints rendered instead
     of the answer when the session is gated — see below).
   - **JLPに語彙を保存** — saves the selection as a vocabulary lookup
     event, visible afterward at `/vocabulary` in the app.

## The `document_id` workaround

`POST /api/v1/sessions/{id}/feedback` requires a `document_id`, and a
session's document is normally created lazily, the first time its
workspace page (`GET /sessions/{id}`) is opened — there is no
`GET /api/v1/sessions/{id}/document` API to fetch (or create) it
directly today.

Rather than add a server endpoint for this one caller, `popup.js`'s
`resolveDocumentId` does the minimal-coupling thing: it fetches the
session's own HTML workspace page (same origin, cookies included — that
GET is what lazily creates the document if it doesn't exist yet) and
regex-extracts `data-doc-id="..."` off the editor `<textarea>`
(`web/templates/workspace.html.tmpl` renders that attribute; the regex
pins the exact attribute order that template uses). It's deliberately
fragile in one specific way: if that template's markup changes, this
breaks loudly (a regex miss throws), not silently.

**The clean fix**, if this extension gains more users of the workaround
or the template's markup starts moving around: add a small, read-only
`GET /api/v1/sessions/{id}/document` endpoint (handler + a
`{id, session_id, content, version}` DTO + a test) to
`internal/adapters/http/api.go`, mirroring the shape of every other
handler already there. Left for a later phase since one screen-scrape in
one extension file is a smaller footprint than a new API surface for a
single caller.

## CORS vs. extension host permissions — two different mechanisms

These are easy to conflate; they are not the same thing, and only one of
them is real extension behavior:

- **A real installed extension** (loaded via `chrome://extensions`,
  running at a `chrome-extension://…` origin) making a `fetch()` from
  its background service worker or a popup, to an origin it holds a
  **host permission** for (granted via `optional_host_permissions` +
  `chrome.permissions.request`, see `options.js`), is **exempt from CORS
  enforcement entirely**. The browser doesn't consult
  `Access-Control-Allow-Origin` at all for that request — this is a
  deliberate part of the extension platform, not a workaround, and it's
  why the app needs zero CORS middleware for the extension to work.
  `internal/adapters/http/csrf.go`'s Task 1 CSRF allowances (extension-
  scheme `Origin` always passes) cover the write side of this; there is
  nothing else the server needs to do.
- **A `file://`-hosted page** (e.g. `popup.html` opened directly in a
  browser tab for the verification protocol below, *not* as an
  installed extension) is an ordinary cross-origin page as far as the
  browser is concerned. Its `fetch()` calls to `http://localhost:PORT`
  ARE subject to CORS, and the app has no CORS middleware — so a
  `file://`-hosted popup's fetches to the real API will fail to read the
  response (the request may even succeed server-side; the browser just
  won't let the script read it back). This is expected, not a bug: the
  fix is never "add CORS to the app" (that would open the JSON API to
  arbitrary third-party origins, which the app should not do), it's to
  drive the same popup functions from an **app-origin tab** instead —
  see below.

## Testing without `load-unpacked` (the mandatory browser-verification protocol)

`popup.html`/`options.html` can also run as plain pages, outside any
extension: a small conditional loader at the top of each
(`<script>...document.write('<script src="shim/chrome-shim.js">')...`)
requests `shim/chrome-shim.js` — a ~40-line in-memory stub of
`chrome.storage.{sync,session}` and `chrome.runtime` — **only when
`window.chrome.storage` is absent**. Inside a real extension that
condition is never true, so the shim is never fetched there; `shim/` is
excluded from `make ext-build`'s zip for the same reason (it would 404
in a packed extension that never asks for it, but there's no reason to
ship dead weight either).

The verification protocol this task's brief mandates:

1. `make up && make migrate && make seed`.
2. Open `chrome-extension/options.html` via `file://` in a browser tab.
   The shim activates (no real `chrome.storage`). Set **Base URL** to
   your running app's URL (e.g. `http://localhost:28080` if
   `APP_HOST_PORT=28080`), save, and confirm `chrome.storage.sync` (via
   the shim) now holds it.
3. Open `chrome-extension/popup.html` via `file://`. Because this is a
   `file://` page (not an installed extension — see the CORS section
   above), its own `fetch()` calls would be blocked reading the app's
   response. So: instead of driving `popup.html`'s fetches directly,
   inject the test selection into the shim
   (`chrome.storage.session.set({selection:"…"})`) and then call
   `window.jlpPopup.requestFeedback(text)` / `.saveVocabulary(...)`
   **from an app-origin tab** (`http://localhost:PORT`, navigated to
   directly) with `popup.js`'s functions injected into that tab via
   `javascript_tool` — a same-origin fetch from that tab needs no CORS
   at all, and it's exercising the exact same `popup.js` code a real
   extension's popup would run. This substitutes for `load-unpacked`
   automation, which isn't scriptable headlessly.
4. Assert a correction card renders containing 面白かったです for the
   seeded (non-socratic) session, and separately, that a socratic-mode
   session's response renders its hint (`hint_ja`/`hint_en`) with no
   `replacement`/explanation text present — `correctionDTO.gated` (see
   `api.go`) is `false`/`true` respectively; `renderFeedback` branches on
   it exactly the way `correction_card.html.tmpl` does server-side.
5. Assert a vocabulary save shows up at `http://localhost:PORT/vocabulary`.
6. Assert zero browser console errors throughout.

## `make ext-build`

```sh
make ext-build   # -> dist/jlp-extension.zip (manifest.json at the zip root)
```

Zips this directory for distribution, excluding `shim/` (test-only, see
above) and this `README.md`. `dist/` is gitignored.
