// popup.js — the extension's ONLY code that talks to the JLP JSON API
// (PRD §40: no learning-domain logic here, just decode/encode plumbing
// against the frozen DTOs, same spirit as internal/adapters/http/api.go
// on the server side). Every exported function below is self-contained
// (resolves its own config/session/document, one function call = one
// full flow) so the mandatory browser-verification protocol
// (README.md) can drive it directly via javascript_tool injection,
// without a service worker or a real extension install in the loop.

const DEFAULT_BASE_URL = "http://localhost:8080";

// --- config -----------------------------------------------------------

// jlpFetch is the ONE place this extension decides how it authenticates.
//
// Bearer token, not cookies: under APP_AUTH_MODE=oidc the session cookie
// is SameSite=Lax and never travels from an extension origin, so
// credentials: "include" silently produced a 401 on every call. Mint a
// token in JLP under 設定 → APIトークン and paste it into the options
// page.
//
// A missing token is reported as such rather than being sent as an empty
// Authorization header, which the server would (correctly) refuse with a
// 401 that says nothing about what is actually wrong.
async function jlpFetch(cfg, path, init = {}) {
  if (!cfg.apiToken) {
    throw new Error("APIトークンが設定されていません。オプションページで設定してください。");
  }
  const res = await fetch(`${cfg.baseUrl}${path}`, {
    ...init,
    headers: {
      ...(init.headers || {}),
      Authorization: `Bearer ${cfg.apiToken}`,
    },
  });
  if (res.status === 401) {
    throw new Error("APIトークンが無効か、取り消されています。");
  }
  if (res.status === 403) {
    throw new Error("このトークンにはこの操作の権限がありません（スコープ不足）。");
  }
  return res;
}

// loadConfig reads the options page's saved {baseUrl, sessionId} from
// chrome.storage.sync (or the shim's in-memory stand-in — see
// shim/chrome-shim.js). sessionId is optional: empty means "use the
// caller's first session", resolved below.
async function loadConfig() {
  const [synced, local] = await Promise.all([
    chrome.storage.sync.get({ baseUrl: DEFAULT_BASE_URL, sessionId: "", chatUrl: "", autoDeliver: false }),
    // The token lives in storage.local, NOT sync: a credential that
    // replicates to every device signed into this Chrome profile is a
    // different security posture than a base URL, and keeping them in
    // one store would make that difference invisible.
    chrome.storage.local.get({ apiToken: "" }),
  ]);
  return { ...synced, ...local };
}

// --- session / document resolution -------------------------------------

// resolveSessionId returns the configured default session id, or (when
// none is configured) the caller's first session per GET
// /api/v1/sessions, which returns them newest-first — the most recently
// created/used session is the reasonable default for a "right-click and
// correct" flow.
async function resolveSessionId(cfg, configuredSessionId) {
  if (configuredSessionId) return configuredSessionId;
  const res = await jlpFetch(cfg, `/api/v1/sessions`);
  if (!res.ok) throw new Error(`sessions request failed: ${res.status}`);
  const sessions = await res.json();
  if (!sessions.length) throw new Error("no JLP sessions exist yet — create one in the app first");
  return sessions[0].id;
}

// docIdPattern matches the exact attribute order
// web/templates/workspace.html.tmpl renders on the editor textarea:
// <textarea id="editor" lang="ja" spellcheck="false" data-doc-id="...">
const docIdPattern = /<textarea[^>]*\bid="editor"[^>]*\bdata-doc-id="([^"]+)"/;

// resolveDocumentId is the deliberate, minimal-coupling workaround
// documented in README.md: there is no GET /api/v1/sessions/{id}/document
// endpoint, so this fetches the session's own HTML workspace page (same
// origin, cookies included — sessionsWorkspace's Writing.Open call also
// creates the document on first view, so this is safe to call before
// any editing has happened) and regex-extracts data-doc-id from the
// editor textarea it renders.
async function resolveDocumentId(cfg, sessionId) {
  const res = await jlpFetch(cfg, `/sessions/${encodeURIComponent(sessionId)}`);
  if (!res.ok) throw new Error(`workspace page request failed: ${res.status}`);
  const html = await res.text();
  const match = html.match(docIdPattern);
  if (!match) throw new Error("could not find the document id in the workspace page");
  return match[1];
}

// runeLength mirrors app.js's window.jlp.runeOffset rule exactly: the
// feedback endpoint's start/end are RUNE offsets, and Array.from splits
// on code points the way plain string indexing/`.length` (UTF-16 code
// units) does not — see app.js's own comment for the same trap.
function runeLength(text) { return Array.from(text).length; }

// --- feedback flow -------------------------------------------------------

// requestFeedback runs the whole "JLPで添削" flow end to end: resolve
// config -> resolve session -> resolve document -> POST feedback ->
// render. Called both by the popup's own bootstrap (real extension
// usage, driven by background.js's stashed selection) and directly by
// the browser-verification protocol.
async function requestFeedback(text) {
  showView("feedback-view");
  const statusEl = document.getElementById("feedback-status");
  const resultsEl = document.getElementById("feedback-results");
  resultsEl.textContent = "";
  statusEl.textContent = "読み込み中…";
  statusEl.classList.remove("error-banner");
  try {
    const cfg = await loadConfig();
    const sessionId = await resolveSessionId(cfg, cfg.sessionId);
    const documentId = await resolveDocumentId(cfg, sessionId);
    const body = { document_id: documentId, start: 0, end: runeLength(text), text };
    const res = await jlpFetch(cfg, `/api/v1/sessions/${encodeURIComponent(sessionId)}/feedback`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`feedback request failed: ${res.status}`);
    const dto = await res.json();
    statusEl.textContent = "";
    renderFeedback(dto);
    return dto;
  } catch (err) {
    statusEl.textContent = `エラー: ${err.message}`;
    statusEl.classList.add("error-banner");
    throw err;
  }
}

// renderFeedback is a pure render step: given a feedbackDTO
// (internal/adapters/http/api.go's frozen shape), build the same
// correction-card structure correction_card.html.tmpl renders
// server-side, minus the accept/reject/retry affordances (the
// extension is read-only feedback, not a workspace). Built via DOM
// calls rather than innerHTML string concatenation so no
// escaping/XSS bookkeeping is needed for AI-provided text.
function renderFeedback(dto) {
  const resultsEl = document.getElementById("feedback-results");
  resultsEl.textContent = "";

  if (!dto.corrections || dto.corrections.length === 0) {
    const p = document.createElement("p");
    p.className = "all-good";
    p.textContent = "訂正はありませんでした。";
    resultsEl.appendChild(p);
    return;
  }

  for (const c of dto.corrections) {
    resultsEl.appendChild(renderCorrectionCard(c));
  }
}

function renderCorrectionCard(c) {
  const article = document.createElement("article");
  article.className = "correction";

  const header = document.createElement("header");
  header.appendChild(badge("badge", c.type));
  header.appendChild(badge(`badge sev sev-${c.severity}`, c.severity));
  article.appendChild(header);

  if (c.gated) {
    // Socratic pre-reveal gate (PRD §9/§53): correctionDTO.gated is true
    // exactly when correction_card.html.tmpl's own gate would hide the
    // answer — replacement/explanations are EMPTY on the wire in this
    // case (see api.go's toCorrectionDTO), so there is nothing to
    // accidentally leak here even if this branch were skipped by
    // mistake; it exists to render the hint instead of an empty card.
    const pair = document.createElement("p");
    pair.className = "pair";
    const del = document.createElement("del");
    del.textContent = c.original;
    pair.appendChild(del);
    article.appendChild(pair);

    if (c.hint_ja) {
      const hintJa = document.createElement("p");
      hintJa.className = "hint ja";
      hintJa.textContent = `💡 ${c.hint_ja}`;
      article.appendChild(hintJa);
    }
    if (c.hint_en) {
      const hintEn = document.createElement("p");
      hintEn.className = "hint en";
      hintEn.textContent = c.hint_en;
      article.appendChild(hintEn);
    }
  } else {
    const pair = document.createElement("p");
    pair.className = "pair";
    const del = document.createElement("del");
    del.textContent = c.original;
    pair.appendChild(del);
    pair.appendChild(document.createTextNode(" → "));
    const ins = document.createElement("ins");
    ins.textContent = c.replacement;
    pair.appendChild(ins);
    article.appendChild(pair);

    const explJa = document.createElement("p");
    explJa.className = "expl ja";
    explJa.textContent = c.explanation_ja;
    article.appendChild(explJa);

    const explEn = document.createElement("p");
    explEn.className = "expl en";
    explEn.textContent = c.explanation_en;
    article.appendChild(explEn);
  }

  return article;
}

function badge(className, text) {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = text;
  return span;
}

// --- vocabulary save flow -------------------------------------------------

// saveVocabulary runs the whole "JLPに語彙を保存" flow: POST
// /api/v1/vocabulary/events with type vocabulary.lookup — same shape the
// demo-ingest Makefile target and appvocabulary.IngestEvent expect.
// reading/definition/example are left blank: the extension only ever
// captures a bare selection, with no dictionary lookup of its own (PRD
// §40 — that would be learning-domain logic).
async function saveVocabulary(expression, sourceTitle) {
  showView("vocab-view");
  document.getElementById("vocab-expression").value = expression;
  const statusEl = document.getElementById("vocab-status");
  statusEl.textContent = "保存中…";
  statusEl.classList.remove("error-banner");
  try {
    const cfg = await loadConfig();
    const body = {
      type: "vocabulary.lookup",
      expression,
      reading: "",
      definition: "",
      example: "",
      source: { type: "web", title: sourceTitle || "" },
      client_event_id: crypto.randomUUID(),
    };
    const res = await jlpFetch(cfg, `/api/v1/vocabulary/events`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`vocabulary save failed: ${res.status}`);
    const item = await res.json();
    statusEl.textContent = `保存しました: ${item.expression}`;
    return item;
  } catch (err) {
    statusEl.textContent = `エラー: ${err.message}`;
    statusEl.classList.add("error-banner");
    throw err;
  }
}


// --- Kindle edition (読解) flow --------------------------------------------

// Poll cadence and patience for a study edition. Analysis is one model
// call of up to a minute or two; the worker retries with minutes of
// backoff, so past a few minutes the popup stops watching and points at
// the page, which keeps going without it.
const READING_POLL_MS = 3000;
const READING_POLL_LIMIT = 100; // ~5 minutes

// submitArticle runs 「JLPでKindle版を作成」 end to end: POST the captured
// article, then follow the edition until it is finished. The server
// decides everything — whether this text is a duplicate, when the edition
// is ready, whether to mail it — the popup only shows what it says.
async function submitArticle(article) {
  showView("reading-view");
  const statusEl = document.getElementById("reading-status");
  document.getElementById("reading-title").textContent = article.title || "";
  statusEl.classList.remove("error-banner");
  statusEl.textContent = "送信中…";
  try {
    const cfg = await loadConfig();
    const res = await jlpFetch(cfg, `/api/v1/reading/articles`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        url: article.url || "",
        title: article.title || "",
        source: article.source || "",
        author: article.author || "",
        published_at: article.published_at || "",
        content: article.content || "",
        selection: article.selection || "",
        deliver: !!cfg.autoDeliver,
      }),
    });
    if (!res.ok) {
      let msg = `送信に失敗しました (${res.status})`;
      try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
      throw new Error(msg);
    }
    const out = await res.json();
    document.getElementById("reading-title").textContent = out.article.title;
    renderEdition(cfg, out.edition, out.duplicate);
    if (!out.edition.terminal) await followEdition(cfg, out.edition.id);
    return out;
  } catch (err) {
    statusEl.textContent = `エラー: ${err.message}`;
    statusEl.classList.add("error-banner");
    document.getElementById("reading-progress").hidden = true;
    throw err;
  }
}

async function followEdition(cfg, id) {
  for (let i = 0; i < READING_POLL_LIMIT; i++) {
    await new Promise((r) => setTimeout(r, READING_POLL_MS));
    const res = await jlpFetch(cfg, `/api/v1/reading/editions/${encodeURIComponent(id)}`);
    if (!res.ok) throw new Error(`状態の取得に失敗しました (${res.status})`);
    const ed = await res.json();
    renderEdition(cfg, ed, false);
    // Stop once the edition is finished AND any delivery it queued has
    // finished too, so an auto-send shows its outcome.
    const sending = (ed.deliveries || []).some((d) => d.status === "pending" || d.status === "sending");
    if (ed.terminal && !sending) return ed;
  }
  document.getElementById("reading-status").textContent =
    "まだ作成中です。JLPのページで続きを確認できます。";
  document.getElementById("reading-progress").hidden = true;
  return null;
}

// renderEdition is a pure render of the server's edition DTO.
function renderEdition(cfg, ed, duplicate) {
  const statusEl = document.getElementById("reading-status");
  const progress = document.getElementById("reading-progress");
  const actions = document.getElementById("reading-actions");
  const base = cfg.baseUrl.replace(/\/+$/, "");

  const latest = (ed.deliveries || [])[0];
  let line = ed.status_label;
  if (ed.status === "pending" || ed.status === "analysing") {
    line = duplicate ? "この記事はすでに作成中です…" : "学習版を作成しています…";
  } else if (ed.status === "ready") {
    line = duplicate ? "この記事の学習版はすでにあります。" : "学習版ができました。";
  } else if (ed.status === "failed") {
    line = `作成できませんでした: ${ed.last_error || ""}`;
  }
  if (latest) line += ` ${latest.status_label}${latest.last_error ? `（${latest.last_error}）` : ""}`;
  statusEl.textContent = line;
  statusEl.classList.toggle("error-banner", ed.status === "failed");
  progress.hidden = ed.terminal;

  actions.hidden = false;
  document.getElementById("reading-page").href = base + ed.page_url;
  const epub = document.getElementById("reading-epub");
  epub.hidden = !ed.epub_url;
  if (ed.epub_url) epub.href = base + ed.epub_url;

  const deliver = document.getElementById("reading-deliver");
  const inFlight = latest && (latest.status === "pending" || latest.status === "sending");
  deliver.hidden = !(ed.delivery_enabled && ed.status === "ready");
  deliver.disabled = !!inFlight;
  deliver.dataset.editionId = ed.id;
}

async function deliverEdition(id) {
  const cfg = await loadConfig();
  const btn = document.getElementById("reading-deliver");
  btn.disabled = true;
  const res = await jlpFetch(cfg, `/api/v1/reading/editions/${encodeURIComponent(id)}/deliver`, { method: "POST" });
  if (!res.ok) {
    let msg = `送信に失敗しました (${res.status})`;
    try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
    document.getElementById("reading-status").textContent = `エラー: ${msg}`;
    btn.disabled = false;
    return;
  }
  await followEdition(cfg, id);
}

// captureActiveTab is the toolbar button's path: the same article.js
// background.js injects for the context menu, run against the tab the
// toolbar was clicked on (activeTab covers it — the click is the gesture).
async function captureActiveTab() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab || tab.id == null) throw new Error("タブが見つかりません");
  const [{ result } = {}] = await chrome.scripting.executeScript({ target: { tabId: tab.id }, files: ["article.js"] });
  if (!result) throw new Error("このページは読み取れません");
  return result;
}

function wireReading() {
  document.getElementById("reading-deliver").addEventListener("click", (e) => {
    const id = e.currentTarget.dataset.editionId;
    if (id) deliverEdition(id);
  });
  document.getElementById("reading-start").addEventListener("click", async () => {
    try {
      submitArticle(await captureActiveTab());
    } catch (err) {
      showView("reading-view");
      const statusEl = document.getElementById("reading-status");
      statusEl.textContent = `エラー: ${err.message}`;
      statusEl.classList.add("error-banner");
    }
  });
}

// --- bootstrap -------------------------------------------------------------

function showView(id) {
  for (const el of document.querySelectorAll("main > section, main > #empty-state")) {
    el.hidden = el.id !== id;
  }
}

function wireVocabForm() {
  document.getElementById("vocab-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const expression = document.getElementById("vocab-expression").value.trim();
    if (expression) saveVocabulary(expression, document.title);
  });
}

// init reads the selection background.js stashed in chrome.storage.session
// (falling back to a ?text=/&mode= URL query, mainly useful for manual
// testing) and, when one is present, kicks off the matching flow
// automatically — this is what makes a real right-click -> popup open
// actually show results without further user action.
//
// The chrome.storage.session read below is consumed ONE-SHOT, cleared
// immediately after reading and BEFORE acting on it: background.js
// stashes {selection,title,mode} on every context-menu click and never
// clears it itself, and popup.html is ALSO the toolbar's
// action.default_popup — without this, opening the toolbar icon later in
// the same browser session (with no fresh context-menu click) would
// silently replay whatever the last context-menu action was. For
// saveVocabulary that's a real duplicate POST to
// /api/v1/vocabulary/events: client_event_id is freshly generated per
// call (see saveVocabulary), so the server's idempotency check never
// catches a client-side replay like this. Clearing first means a stale
// (or already-consumed) storage.session state can only ever produce the
// neutral idle view ("empty-state" below), never a second automatic
// action.
// createSession parks the selection in a NEW session and does nothing
// else — no AI call, no cost, nothing to wait for. It is the "I want to
// work on this later" action, distinct from 添削, which spends money on
// corrections right now.
//
// The page title becomes the session title because it is the only
// meaningful name available at right-click time, and an untitled session
// is unfindable a week later. Purpose is "reading": this text came from
// something the learner was reading, which is exactly the distinction
// /sessions shows.
async function createSession(text, sourceTitle) {
  const statusEl = document.getElementById("session-status");
  showView("session-view");
  statusEl.textContent = "セッションを作成しています…";
  try {
    const cfg = await loadConfig();
    const res = await jlpFetch(cfg, `/api/v1/sessions`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        title: (sourceTitle || "").slice(0, 200) || "ウェブから",
        purpose: "reading",
        text,
      }),
    });
    if (!res.ok) throw new Error(`session create failed: ${res.status}`);
    const sess = await res.json();
    statusEl.textContent = "";
    renderCreatedSession(cfg, sess, text);
    return sess;
  } catch (err) {
    statusEl.textContent = String(err.message || err);
    return null;
  }
}

// renderCreatedSession confirms what was made and links straight into
// it: a "done" with no way through to the thing it made would leave the
// learner hunting for it in /sessions.
function renderCreatedSession(cfg, sess, text) {
  document.getElementById("session-status").textContent =
    `セッションを作成しました: ${sess.title || ""}`;

  // Shows what was actually parked. A selection routinely picks up
  // navigation chrome, and finding that out a week later in the editor
  // is worse than seeing it now.
  const preview = document.getElementById("session-preview");
  preview.textContent = text.length > 200 ? `${text.slice(0, 200)}…` : text;
  preview.hidden = false;

  // A "done" with no way through to the thing it made leaves the learner
  // hunting for it in /sessions.
  const link = document.getElementById("session-link");
  link.href = `${cfg.baseUrl}/sessions/${encodeURIComponent(sess.id)}`;
  link.hidden = false;
}

async function init() {
  wireVocabForm();
  wireReading();
  const stored = await chrome.storage.session.get({ selection: "", title: "", mode: "", article: null });
  await chrome.storage.session.remove(["selection", "title", "mode", "article"]);
  const params = new URLSearchParams(location.search);
  const text = stored.selection || params.get("text") || "";
  const title = stored.title || params.get("title") || "";
  const mode = stored.mode || params.get("mode") || "";

  if (mode === "reading" && stored.article) {
    await submitArticle(stored.article);
  } else if (mode === "vocab" && text) {
    await saveVocabulary(text, title);
  } else if (mode === "session" && text) {
    await createSession(text, title);
  } else if (text) {
    await requestFeedback(text);
  } else {
    showView("empty-state");
  }
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}

// Exposed for the browser-verification protocol (README.md) to drive
// directly via javascript_tool, and for popup.html's own bootstrap above.
window.jlpPopup = {
  loadConfig,
  resolveSessionId,
  resolveDocumentId,
  requestFeedback,
  renderFeedback,
  saveVocabulary,
  createSession,
  submitArticle,
  renderEdition,
};
