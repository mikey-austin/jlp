# 読解 from the Phone — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create Kindle study editions from Chrome on Android — full articles with images through a bookmarklet, text through the PWA share target.

**Architecture:** The server embeds the extension's `article.js` and a newly extracted `imaging.js` (package `extension` inside `chrome-extension/`), generates a bookmarklet from `article.js`, and serves both files to two new JLP pages: `/reading/capture` (receives the bookmarklet's capture by `postMessage`, confirms, uploads) and `/reading/share` (receives Android shares that the service worker intercepted). Both submit through the existing `/api/v1/reading/articles` with the session cookie and `deliver: true`.

**Tech Stack:** Go (chi, html/template, go:embed), vanilla JS (no build step), Web App Manifest `share_target`, Service Worker + Cache Storage, `window.postMessage`.

**Spec:** `docs/superpowers/specs/2026-09-30-reading-from-phone-design.md`

## Global Constraints

- The server never fetches an article's page. Image bytes are fetched by JLP pages in the browser, never by the server.
- One capture implementation: the bookmarklet is generated from `chrome-extension/article.js`; images use `chrome-extension/imaging.js` in both the extension and JLP pages. No copies.
- Image limits (unchanged from the extension): long side ≤ 1200 px; ≤ 2 MB each; ≤ 12; ≤ 14 MiB total; 10 s per fetch; decoded images under 200 px either side dropped; PNG kept when source PNG and < 500 KB, else JPEG 0.82 → 0.72 → 0.62.
- `/reading/capture` never submits without the learner tapping 「作成してKindleに送る」. `/reading/share` submits at once.
- postMessage: capture page accepts `jlp-capture-article` only from `window.opener`; bookmarklet posts the article only to the window it opened, with targetOrigin = JLP origin, after a `jlp-capture-ready` from that window and origin. Ready is posted every 1 s for 20 s; bookmarklet listens 5 min.
- All submissions: same-origin `fetch` to `/api/v1/reading/articles`, `credentials: "same-origin"`, `deliver: true`; multipart failure other than 401/403 retries once as text-only JSON.
- New routes sit behind the session login: `GET /settings/phone`, `GET /reading/capture`, `GET`+`POST /reading/share`. `GET /static/ext/{article.js,imaging.js}` is a static asset route (public, like /static).
- Share URL-only message, verbatim: 「URLだけでは本文を読めません。本文を選択して共有するか、ブックマークレットを使ってください」. No-article message: 「記事が届きませんでした。記事のタブでもう一度ブックマークレットを実行してください」.
- The service worker's activate step must keep the `jlp-share` cache.
- Dev: app http://localhost:28080, Mailpit http://localhost:8025 (Kindle delivery pointed at Mailpit), everything through make/compose; repo `tmp/` and `dist/` are root-owned (write scratch files under /tmp).

## Review Focus

1. **A hostile page opens `/reading/capture` and posts an article:** nothing is submitted without the learner's tap, and a message not from `window.opener` is ignored. Pinned in Task 3's browser verification.
2. **The bookmarklet on a page whose CSP blocks inline/remote scripts:** a bookmarklet runs regardless of the page's CSP in Chrome, but `window.open` may be popup-blocked — the fallback navigation must work. Pinned in Task 3.
3. **Service worker update:** an existing installed worker (old version, no share handler) must be replaced; the share cache must survive activate. Pinned in Task 4's test and verification.
4. **Shared text with a trailing URL, or only a URL:** split correctly / the URL-only message. Pinned in Task 4.
5. **The extension popup after `imaging.js` extraction:** still captures, downscales and uploads. Pinned in Task 1.

---

### Task 1: Extract `imaging.js`; embed extension files

**Files:**
- Create: `chrome-extension/imaging.js`, `chrome-extension/extension.go`, `chrome-extension/extension_test.go`
- Modify: `chrome-extension/popup.js`, `chrome-extension/popup.html`, `chrome-extension/README.md`, `Makefile` (ext-build excludes `*.go`)

**Interfaces — Produces:**
- `imaging.js` defines on `globalThis` (classic script, no modules): `JLPImaging = { MAX_EDGE, MIN_EDGE, MAX_IMAGE_BYTES, PNG_KEEP_BYTES, UPLOAD_BUDGET, fetchWithTimeout(url, ms, init), downscale(blob), withinBudget(ok, budget), prepareImages(figures, opts) }`. `prepareImages(figures, {fetchInit})` returns `{ok: [{meta, blob}], failed}` — `fetchInit` defaults to `{credentials: "include"}` (extension); JLP pages pass `{credentials: "omit", mode: "cors"}`.
- Go package `extension` (import path `github.com/mikeyaustin/jlp/chrome-extension`): `func File(name string) ([]byte, bool)` for `"article.js"` and `"imaging.js"`; `func ArticleCapture() string` — article.js source with whole-line `//` comments removed and trailing whitespace trimmed, ending in the IIFE expression without a trailing semicolon.

- [ ] **Step 1: Move the code.** Cut from `popup.js` the constants `MAX_EDGE, MIN_EDGE, MAX_IMAGE_BYTES, PNG_KEEP_BYTES, UPLOAD_BUDGET` and functions `fetchWithTimeout, downscale, withinBudget, prepareImages` into `imaging.js`, wrapped:

```js
// imaging.js — fetch, downscale and budget an article's images. Shared,
// byte for byte, by the extension popup and by JLP's own phone pages
// (/reading/capture), which the server serves from its embedded copy of
// this file (package extension). No learning-domain logic, no chrome.*.
(function (global) {
  // ...constants and functions moved verbatim from popup.js...
  // prepareImages gains an opts.fetchInit passed through to fetchWithTimeout.
  global.JLPImaging = { MAX_EDGE, MIN_EDGE, MAX_IMAGE_BYTES, PNG_KEEP_BYTES, UPLOAD_BUDGET,
    fetchWithTimeout, downscale, withinBudget, prepareImages };
})(globalThis);
```

`fetchWithTimeout(url, ms, init = { credentials: "include" })` spreads `init` into the fetch options (keeping `signal`). `prepareImages(figures, opts = {})` calls `fetchWithTimeout(f.src, 10000, opts.fetchInit)`. In `popup.js`, keep `IMAGE_ORIGINS` and `imagesAllowed` (they use `chrome.*`) and replace uses with `JLPImaging.prepareImages(...)`; keep `window.jlpPopup.prepareImages`/`downscale` exported as aliases to the JLPImaging functions so the README protocol still works. In `popup.html`, add `<script src="imaging.js"></script>` before `popup.js`.

- [ ] **Step 2: Failing Go test** — `chrome-extension/extension_test.go`:

```go
package extension

import (
	"strings"
	"testing"
)

func TestFilesAreEmbedded(t *testing.T) {
	for _, name := range []string{"article.js", "imaging.js"} {
		b, ok := File(name)
		if !ok || len(b) == 0 {
			t.Fatalf("%s not embedded", name)
		}
	}
	if _, ok := File("popup.js"); ok {
		t.Fatal("only the shared capture files are served")
	}
}

func TestArticleCaptureIsAnExpression(t *testing.T) {
	src := ArticleCapture()
	if !strings.HasPrefix(src, "(function") || strings.HasSuffix(src, ";") || !strings.HasSuffix(src, ")") {
		t.Fatalf("capture must be the bare IIFE expression; got %q…%q", src[:20], src[len(src)-20:])
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			t.Fatalf("whole-line comment survived: %q", line)
		}
	}
	if !strings.Contains(src, "figures") {
		t.Fatal("not the article capture")
	}
}
```

Run: `go test ./chrome-extension/` → FAIL (undefined).

- [ ] **Step 3: Implement** — `chrome-extension/extension.go`:

```go
// Package extension embeds the Chrome extension's shared capture files so
// the server can serve them to JLP's phone pages and build the
// bookmarklet from them: one capture implementation for desktop and
// phone. Only article.js and imaging.js are exposed.
package extension

import (
	"embed"
	"strings"
)

//go:embed article.js imaging.js
var files embed.FS

// File returns one embedded shared file.
func File(name string) ([]byte, bool) {
	if name != "article.js" && name != "imaging.js" {
		return nil, false
	}
	b, err := files.ReadFile(name)
	return b, err == nil
}

// ArticleCapture returns article.js as a bare expression for inlining
// into the bookmarklet: whole-line // comments removed (they would be
// noise in a URL), and the trailing semicolon dropped so it can sit on
// the right of an assignment.
func ArticleCapture() string {
	b, _ := files.ReadFile("article.js")
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	src := strings.Join(out, "\n")
	return strings.TrimSuffix(strings.TrimSpace(src), ";")
}
```

(article.js's leading block of `//` header comments sits above `(function`, so after stripping, the source starts with `(function`.) In `Makefile` ext-build add `-x '*.go'` to the zip excludes. Run: `go test ./chrome-extension/` → PASS; `make test` and `make lint` green.

- [ ] **Step 4: Browser-verify the popup still works** — per chrome-extension/README.md's shim protocol (app-origin tab at localhost:28080, CORS static server under /tmp for the extension dir and test images, token minted at /settings/tokens): submit an article with 2 synthetic images; expect 「画像 2/2枚」, figure_count 2, no console errors.

- [ ] **Step 5: Commit** — `refactor(extension): share imaging.js; embed the capture files for the server`.

---

### Task 2: Public URL, `/static/ext`, bookmarklet, 「スマホで読解」 settings page

**Files:**
- Create: `internal/adapters/http/phone.go`, `internal/adapters/http/phone_test.go`, `web/templates/settings_phone.html.tmpl`
- Modify: `internal/adapters/http/server.go` (Options.PublicURL, routes), `web/templates/settings.html.tmpl` (index card), `cmd/jlp/main.go` (wire `PublicURL: cfg.Server.BaseURL` — check the config field name), `web/static/css/app.css` if needed

**Interfaces — Produces:**
- `Options.PublicURL string`
- `func (s *Server) publicOrigin(r *http.Request) string` — `strings.TrimRight(PublicURL, "/")`, else `scheme://host` from `X-Forwarded-Proto` (else `https` if `r.TLS != nil`, else `http`) and `r.Host`.
- `func bookmarklet(origin string) string` — the full `javascript:` URL.
- `func extAssetURL(name string) string` — `/static/ext/<name>?v=<first 12 hex of sha256>`; registered as template func `extAsset`.
- Routes: `GET /static/ext/{name}` (public), `GET /settings/phone` (session).

- [ ] **Step 1: Failing tests** — `internal/adapters/http/phone_test.go` (use the package's existing test-server helpers for page GETs as the logged-in dev identity; adapt names):

```go
func TestBookmarkletCarriesCaptureAndOrigin(t *testing.T) {
	b := bookmarklet("https://jlp.example.test")
	if !strings.HasPrefix(b, "javascript:") {
		t.Fatalf("not a bookmarklet: %.30s", b)
	}
	js, err := url.PathUnescape(strings.TrimPrefix(b, "javascript:"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{extension.ArticleCapture(), `"https://jlp.example.test"`, "jlp-capture-ready", "jlp-capture-article", "/reading/capture"} {
		if !strings.Contains(js, want[:min(len(want), 200)]) {
			t.Fatalf("bookmarklet missing %.60q", want)
		}
	}
	if len(b) > 64<<10 {
		t.Fatalf("bookmarklet is %d bytes", len(b))
	}
}

func TestBookmarkletOriginIsAJSStringLiteral(t *testing.T) {
	js, _ := url.PathUnescape(strings.TrimPrefix(bookmarklet(`https://x.test/"+alert(1)+"`), "javascript:"))
	if strings.Contains(js, `"https://x.test/"+alert(1)+""`) {
		t.Fatal("origin must be JSON-encoded, not concatenated raw")
	}
}

func TestExtAssetServesEmbeddedFilesImmutably(t *testing.T) {
	srv := /* existing test server helper */
	res := /* GET extAssetURL("imaging.js") without auth */
	if res.Code != 200 || !strings.Contains(res.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(res.Header().Get("Cache-Control"), "immutable") || !strings.Contains(res.Body.String(), "JLPImaging") {
		t.Fatalf("%d %v", res.Code, res.Header())
	}
	if r := /* GET /static/ext/popup.js */; r.Code != 404 {
		t.Fatalf("popup.js must not be served: %d", r.Code)
	}
}

func TestSettingsPhonePageShowsBookmarklet(t *testing.T) {
	page := /* GET /settings/phone as the dev identity */
	if page.Code != 200 || !strings.Contains(page.Body.String(), `href="javascript:`) || !strings.Contains(page.Body.String(), "スマホで読解") {
		t.Fatalf("settings/phone: %d", page.Code)
	}
}

func TestPublicOriginPrefersConfiguredURL(t *testing.T) {
	s := &Server{opts: Options{PublicURL: "https://jlp.lan.example/"}}
	r := httptest.NewRequest("GET", "http://other/settings/phone", nil)
	if got := s.publicOrigin(r); got != "https://jlp.lan.example" {
		t.Fatalf("got %q", got)
	}
	s.opts.PublicURL = ""
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Host = "jlp.fromhost.example"
	if got := s.publicOrigin(r); got != "https://jlp.fromhost.example" {
		t.Fatalf("got %q", got)
	}
}
```

Run: `make test` → FAIL.

- [ ] **Step 2: Implement** — `internal/adapters/http/phone.go`:

```go
// The phone paths into 読解 (spec: 読解 from the phone). The bookmarklet
// runs the extension's own capture (package extension) inside the
// article's page — where the learner is signed in — and hands the result
// to /reading/capture by postMessage; the server never fetches the page.
package http

// bookmarkletTemplate is the handoff around the inlined capture. %s are,
// in order: the JSON-encoded JLP origin, and the capture expression.
const bookmarkletTemplate = `(()=>{const JLP=%s;let a;try{a=%s;}catch(e){a=null}
if(!a){location.href=JLP+"/reading/capture#failed";return}
const w=window.open(JLP+"/reading/capture","_blank");
if(!w){location.href=JLP+"/reading/capture#blocked";return}
const on=(e)=>{if(e.source!==w||e.origin!==JLP||!e.data||e.data.type!=="jlp-capture-ready")return;
w.postMessage({type:"jlp-capture-article",v:1,article:a},JLP);removeEventListener("message",on)};
addEventListener("message",on);setTimeout(()=>removeEventListener("message",on),300000)})()`

func bookmarklet(origin string) string {
	o, _ := json.Marshal(origin)
	js := fmt.Sprintf(bookmarkletTemplate, o, extension.ArticleCapture())
	// PathEscape keeps the URL a single bookmark-safe string; Chrome
	// decodes it before running.
	return "javascript:" + url.PathEscape(js)
}

func (s *Server) publicOrigin(r *http.Request) string {
	if u := strings.TrimRight(s.opts.PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

var extHashes = map[string]string{} // filled at init from extension.File

func extAssetURL(name string) string { return "/static/ext/" + name + "?v=" + extHashes[name] }

func (s *Server) extAsset(w http.ResponseWriter, r *http.Request) {
	b, ok := extension.File(chi.URLParam(r, "name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

func (s *Server) settingsPhonePage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "settings_phone", map[string]any{
		"Title":       "スマホで読解",
		"Identity":    ident,
		"Bookmarklet": template.URL(bookmarklet(s.publicOrigin(r))),
	})
}
```

(Add `init()` computing `extHashes` with sha256 → hex[:12] for both files; add imports; register the `extAsset` template func wherever the package registers `asset`; register routes: `r.Get("/static/ext/{name}", s.extAsset)` as an exact route **ahead of the `/static/*` wildcard** (as `/static/sw.js` already is — see server.go ~281), public, `r.Get("/settings/phone", s.settingsPhonePage)` with the other settings routes.) `template.URL` is required: html/template otherwise rewrites `javascript:` hrefs to `#ZgotmplZ`.

`web/templates/settings_phone.html.tmpl` (follow settings.html.tmpl's structure and classes):

```
{{define "content"}}<section class="settings">
  <h1>スマホで読解</h1>
  <p>スマホのChromeで読んでいる記事（ログイン中のWSJなども）から、Kindle用の学習版を作ります。記事の本文はそのページの中で読み取るので、JLPのサーバーが記事のサイトにアクセスすることはありません。</p>

  <div class="card">
    <h2>1. ブックマークレット（画像つき・おすすめ）</h2>
    <p><a class="btn" href="{{.Bookmarklet}}">JLP 読解</a> ← このリンクをブックマークに保存します（パソコンではバーにドラッグ、スマホでは下のコードをコピーして新しいブックマークのURLに貼り付け）。名前は「jlp」など短くしておくと便利です。</p>
    <details><summary>コードを表示（コピー用）</summary><textarea readonly rows="6" class="mono">{{.Bookmarklet}}</textarea></details>
    <p>使い方：記事を開いてアドレスバーに「jlp」と入力し、候補のブックマークをタップ → JLPが開くので「作成してKindleに送る」をタップ。</p>
  </div>

  <div class="card">
    <h2>2. 共有（テキストのみ・設定不要）</h2>
    <p>JLPをホーム画面に追加（インストール）すると、共有メニューに「JLP」が表示されます。記事の本文を選択して「共有」→「JLP」で、そのままKindle版が作られます。画像は含まれません。</p>
  </div>
</section>{{end}}
```

Add a card to settings.html.tmpl's index: `<a class="card settings-index__item" href="/settings/phone"><h2>スマホで読解</h2><p>スマホで読んでいる記事から、Kindle用の学習版を作ります（ブックマークレットと共有）。</p></a>`. Wire `PublicURL` in main.go from the server base URL config.

- [ ] **Step 3: Verify** — `make test`, `make lint`; `make restart`; open http://localhost:28080/settings/phone in Chrome and check the link's href starts with `javascript:` (not `#ZgotmplZ`) and the page has no console errors.

- [ ] **Step 4: Commit** — `feat(reading): a bookmarklet for the phone, built from the extension's own capture`.

---

### Task 3: `/reading/capture`

**Files:**
- Create: `web/templates/reading_capture.html.tmpl`, `web/static/js/reading-capture.js`
- Modify: `internal/adapters/http/phone.go` (handler), `internal/adapters/http/server.go` (route), `internal/adapters/http/phone_test.go`

**Interfaces — Consumes:** `JLPImaging` (Task 1, via `{{extAsset "imaging.js"}}`); `POST /api/v1/reading/articles` (multipart or JSON; response `{article, edition:{id,…}, figures, figures_rejected}`; 400 carries `{error}`).

- [ ] **Step 1: Failing Go test** (phone_test.go): `GET /reading/capture` as the dev identity → 200, body contains `reading-capture.js` and `imaging.js`; unauthenticated → the same redirect/401 other /reading pages give (follow the existing test for /reading).

- [ ] **Step 2: Implement.** Handler `readingCapturePage` renders `reading_capture` with `Title: "読解に送る"`. Route `r.Get("/reading/capture", s.readingCapturePage)` beside the other /reading page routes — **registered before `/reading/{id}`** or chi will match `{id}="capture"`; check route order and add a test that `/reading/capture` does not 404 as an edition.

Template (layout's content block; ids used by the JS):

```
{{define "content"}}<section class="reading-capture">
  <h1>読解に送る</h1>
  <p id="capture-status" class="hint">記事を待っています…</p>
  <div id="capture-card" class="card" hidden>
    <p class="reading-title" id="capture-title"></p>
    <p class="hint" id="capture-meta"></p>
    <button type="button" class="btn" id="capture-send">作成してKindleに送る</button>
  </div>
  <div id="capture-help" class="card" hidden></div>
</section>
<script src="{{extAsset "imaging.js"}}"></script>
<script src="{{asset "/static/js/reading-capture.js"}}"></script>{{end}}
```

`web/static/js/reading-capture.js`:

```js
// reading-capture.js — the phone bookmarklet's landing page (spec: 読解
// from the phone). The bookmarklet captured the article inside its own
// page and opened this one; the article arrives by postMessage. Any page
// could open us and post a message, so nothing is submitted until the
// learner taps the button.
(() => {
  const status = document.getElementById("capture-status");
  const card = document.getElementById("capture-card");
  const help = document.getElementById("capture-help");
  const NO_ARTICLE = "記事が届きませんでした。記事のタブでもう一度ブックマークレットを実行してください";

  function showHelp(text) {
    status.textContent = "";
    help.textContent = text;
    help.hidden = false;
  }
  if (location.hash === "#blocked") {
    showHelp("ポップアップがブロックされました。Chromeの設定で記事のサイトのポップアップを許可してから、もう一度実行してください。");
    return;
  }
  if (location.hash === "#failed" || !window.opener) {
    showHelp(NO_ARTICLE);
    return;
  }

  let article = null;
  const ping = setInterval(() => window.opener && window.opener.postMessage({ type: "jlp-capture-ready" }, "*"), 1000);
  const giveUp = setTimeout(() => { clearInterval(ping); if (!article) showHelp(NO_ARTICLE); }, 20000);
  window.opener.postMessage({ type: "jlp-capture-ready" }, "*");

  function valid(a) {
    return a && typeof a.content === "string" && a.content.trim() && typeof a.title === "string" &&
      (!a.figures || Array.isArray(a.figures));
  }

  window.addEventListener("message", (e) => {
    if (article || e.source !== window.opener || !e.data || e.data.type !== "jlp-capture-article" || !valid(e.data.article)) return;
    article = e.data.article;
    clearInterval(ping);
    clearTimeout(giveUp);
    const figs = (article.figures || []).slice(0, 12);
    document.getElementById("capture-title").textContent = article.title;
    document.getElementById("capture-meta").textContent =
      `${article.source || ""}　${Array.from(article.content).length}字　画像 ${figs.length}枚`;
    status.textContent = "";
    card.hidden = false;
  });

  document.getElementById("capture-send").addEventListener("click", async (e) => {
    e.currentTarget.disabled = true;
    try {
      status.textContent = "画像を準備中…";
      const fields = {
        url: article.url || "", title: article.title || "", source: article.source || "",
        author: article.author || "", published_at: article.published_at || "",
        content: article.content || "", selection: article.selection || "", deliver: true,
      };
      const images = await JLPImaging.prepareImages(article.figures || [], { fetchInit: { credentials: "omit", mode: "cors" } });
      status.textContent = "送信中…";
      const post = (init) => fetch("/api/v1/reading/articles", { method: "POST", credentials: "same-origin", ...init });
      const postJSON = () => post({ headers: { "Content-Type": "application/json" }, body: JSON.stringify(fields) });
      let res;
      if (images.ok.length) {
        const form = new FormData();
        form.append("metadata", JSON.stringify({ ...fields, figures: images.ok.map((x) => x.meta) }));
        images.ok.forEach((x, i) => form.append(`image-${i}`, x.blob, `image-${i}`));
        try {
          res = await post({ body: form });
        } catch (_) {
          res = null;
        }
        if (!res || (!res.ok && res.status !== 401 && res.status !== 403)) res = await postJSON();
      } else {
        res = await postJSON();
      }
      if (!res.ok) {
        let msg = `送信に失敗しました (${res.status})`;
        try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
        throw new Error(msg);
      }
      const out = await res.json();
      location.href = `/reading/${encodeURIComponent(out.edition.id)}`;
    } catch (err) {
      status.textContent = `エラー: ${err.message}`;
      e.currentTarget.disabled = false;
    }
  });
})();
```

- [ ] **Step 3: Browser verification** (desktop Chrome, dev stack with Mailpit):
  1. Open http://localhost:28080/settings/phone, read the bookmarklet href (`document.querySelector('a[href^="javascript:"]').href`), and decode it.
  2. In a new tab on a live NHK article, execute the decoded bookmarklet body with javascript_tool **but with JLP replaced by http://localhost:28080** if the page renders the production origin (dev renders localhost already). A new tab opens at /reading/capture; the card shows title, 字数, 画像 n枚. Take a screenshot.
  3. Click 「作成してKindleに送る」: the page goes to /reading/{id}; the edition becomes ready; figure_count > 0 (NHK/WSJ CDNs allow CORS); Mailpit receives the email.
  4. Repeat on the learner's logged-in WSJ article (the MCP browser uses the learner's Chrome profile).
  5. Hostile-page check: from a third tab on another origin, `window.open("http://localhost:28080/reading/capture")` then post a fake `jlp-capture-article` from a *different* window (not the opener) → ignored; post from the opener → card shows but nothing is submitted without the click.
  6. Open /reading/capture directly → the no-article message after 20 s; `#blocked` → the pop-up help.
  Close every tab you opened.

- [ ] **Step 4: Commit** — `feat(reading): /reading/capture receives the bookmarklet's article and sends it on a tap`.

---

### Task 4: Share target

**Files:**
- Modify: `web/static/manifest.webmanifest`, `web/templates/sw.js.tmpl`, `internal/adapters/http/phone.go`, `internal/adapters/http/server.go`, `internal/adapters/http/phone_test.go` (and the existing sw/manifest tests if any)
- Create: `web/templates/reading_share.html.tmpl`, `web/static/js/reading-share.js`

- [ ] **Step 1: Failing tests** (phone_test.go):

```go
func TestManifestDeclaresShareTarget(t *testing.T) {
	b, err := os.ReadFile(repoPath(t, "web/static/manifest.webmanifest")) // use the package's repoPath helper (runtime.Caller-based)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ShareTarget struct {
			Action, Method, Enctype string
			Params                  map[string]string
		} `json:"share_target"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	st := m.ShareTarget
	if st.Action != "/reading/share" || st.Method != "POST" || st.Enctype != "application/x-www-form-urlencoded" ||
		st.Params["title"] != "title" || st.Params["text"] != "text" || st.Params["url"] != "url" {
		t.Fatalf("share_target = %+v", st)
	}
}

func TestServiceWorkerHandlesSharesAndKeepsTheirCache(t *testing.T) {
	sw := /* GET /static/sw.js via the test server */
	body := sw.Body.String()
	for _, want := range []string{`"/reading/share"`, `"jlp-share"`, "303"} {
		if !strings.Contains(body, want) {
			t.Fatalf("sw.js missing %s", want)
		}
	}
}

func TestReadingSharePages(t *testing.T) {
	// GET /reading/share as dev → 200, contains reading-share.js.
	// POST /reading/share (form title/text/url) as dev → 200 page containing
	// "もう一度共有" (the worker-missing message), and creates NOTHING.
}
```

- [ ] **Step 2: Implement.**

Manifest — add:

```json
"share_target": {"action": "/reading/share", "method": "POST", "enctype": "application/x-www-form-urlencoded",
                 "params": {"title": "title", "text": "text", "url": "url"}}
```

sw.js.tmpl — in `activate`, filter `name !== CACHE_NAME && name !== SHARE_CACHE`; add `const SHARE_CACHE = "jlp-share";` and, at the top of the fetch handler:

```js
  // Android share target (manifest share_target). A share arrives as a
  // POST navigation launched by the OS; whether that carries the
  // SameSite=Lax session cookie or passes the CSRF check is not ours to
  // rely on. So the worker keeps the fields and redirects to a GET page,
  // which submits them from JLP's own origin with the session.
  const shareURL = new URL(request.url);
  if (request.method === "POST" && shareURL.pathname === "/reading/share") {
    event.respondWith((async () => {
      const form = await request.formData();
      const id = crypto.randomUUID();
      const entry = { title: form.get("title") || "", text: form.get("text") || "", url: form.get("url") || "" };
      const cache = await caches.open(SHARE_CACHE);
      await cache.put(`/reading/share/pending/${id}`, new Response(JSON.stringify(entry), { headers: { "Content-Type": "application/json" } }));
      return Response.redirect(`/reading/share?pending=${id}`, 303);
    })());
    return;
  }
```

Server: `GET /reading/share` renders `reading_share`; `POST /reading/share` renders the same template with `WorkerMissing: true` (reached only when no worker intercepted) — no side effects. Register both before `/reading/{id}`.

Template:

```
{{define "content"}}<section class="reading-capture">
  <h1>読解に送る</h1>
  {{if .WorkerMissing}}<p class="card">共有を受け取れませんでした。JLPアプリを一度開いてから、もう一度共有してください。</p>
  {{else}}<p id="share-status" class="hint">共有された記事を送信しています…</p>{{end}}
</section>
{{if not .WorkerMissing}}<script src="{{asset "/static/js/reading-share.js"}}"></script>{{end}}{{end}}
```

`web/static/js/reading-share.js`:

```js
// reading-share.js — lands an Android share (see sw.js: the worker kept
// the shared fields and redirected here) and submits it at once: a share
// is already the learner's deliberate act. Text only; images need the
// bookmarklet.
(async () => {
  const status = document.getElementById("share-status");
  const URL_ONLY = "URLだけでは本文を読めません。本文を選択して共有するか、ブックマークレットを使ってください";
  const id = new URLSearchParams(location.search).get("pending");
  const key = `/reading/share/pending/${id}`;
  const cache = id && (await caches.open("jlp-share"));
  const hit = cache && (await cache.match(key));
  if (!hit) { status.textContent = "共有された内容が見つかりません。もう一度共有してください。"; return; }
  const entry = await hit.json();
  await cache.delete(key);

  // Some apps append the page URL as the last line of the shared text.
  let text = (entry.text || "").trim();
  let url = (entry.url || "").trim();
  const lines = text.split("\n");
  const last = lines[lines.length - 1].trim();
  if (/^https?:\/\/\S+$/.test(last)) { url = url || last; lines.pop(); text = lines.join("\n").trim(); }
  if (!text) { status.textContent = URL_ONLY; return; }
  const title = (entry.title || "").trim() || Array.from(text.split("\n")[0]).slice(0, 80).join("");

  try {
    const res = await fetch("/api/v1/reading/articles", {
      method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title, url, content: text, deliver: true }),
    });
    if (!res.ok) {
      let msg = `送信に失敗しました (${res.status})`;
      try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
      throw new Error(msg);
    }
    const out = await res.json();
    location.replace(`/reading/${encodeURIComponent(out.edition.id)}`);
  } catch (err) {
    status.textContent = `エラー: ${err.message}`;
  }
})();
```

- [ ] **Step 3: Browser verification** — on http://localhost:28080 (the worker registers on localhost): reload once so the new sw.js installs (check `navigator.serviceWorker.controller.scriptURL` / wait for activation); then from a JLP page submit a real form POST to /reading/share (create a `<form method=post action=/reading/share>` with title/text/url and submit it) → the worker redirects to /reading/share?pending=…, which submits and lands on /reading/{id}; the edition becomes ready and Mailpit gets it. Repeat with text ending in a URL line (the URL becomes the source) and with only a URL (the URL-only message; no article created). Confirm `caches.keys()` still contains `jlp-share` after a worker update (edit nothing — just verify activate logic by reading the served sw.js).

- [ ] **Step 4: Commit** — `feat(reading): JLP in Android's share sheet — shared text becomes a Kindle edition`.

---

### Task 5: Docs and ship

- [ ] **Step 1: Docs** — README.md's 読解 section: a 「スマホから」 paragraph (bookmarklet via 設定 → スマホで読解, share target after installing the PWA, text vs images). chrome-extension/README.md: note that article.js and imaging.js are also served to JLP's phone pages and inlined in the bookmarklet — changes there affect both.
- [ ] **Step 2: Full suite** — `make test && make test-race && make test-integration && make lint` (only the 3 known assets.go findings).
- [ ] **Step 3: Merge and deploy** (after the final review): fast-forward main, push; `make image-push IMAGE_TAG=20260930-reading-phone`; bump `jlp_image` in ../platform-v2/home/ansible/jlp-playbook.yaml; run the playbook; commit and push platform-v2.
- [ ] **Step 4: Phone check by the learner** — open 設定 → スマホで読解 on the phone, save the bookmarklet, try it on a WSJ article; install/refresh the PWA and share selected text to JLP. Read production logs for `reading: delivered` and stored figures.
