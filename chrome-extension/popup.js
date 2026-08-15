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

// loadConfig reads the options page's saved {baseUrl, sessionId} from
// chrome.storage.sync (or the shim's in-memory stand-in — see
// shim/chrome-shim.js). sessionId is optional: empty means "use the
// caller's first session", resolved below.
async function loadConfig() {
  return chrome.storage.sync.get({ baseUrl: DEFAULT_BASE_URL, sessionId: "" });
}

// --- session / document resolution -------------------------------------

// resolveSessionId returns the configured default session id, or (when
// none is configured) the caller's first session per GET
// /api/v1/sessions, which returns them newest-first — the most recently
// created/used session is the reasonable default for a "right-click and
// correct" flow.
async function resolveSessionId(baseUrl, configuredSessionId) {
  if (configuredSessionId) return configuredSessionId;
  const res = await fetch(`${baseUrl}/api/v1/sessions`, { credentials: "include" });
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
async function resolveDocumentId(baseUrl, sessionId) {
  const res = await fetch(`${baseUrl}/sessions/${encodeURIComponent(sessionId)}`, { credentials: "include" });
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
    const { baseUrl, sessionId: configuredSessionId } = await loadConfig();
    const sessionId = await resolveSessionId(baseUrl, configuredSessionId);
    const documentId = await resolveDocumentId(baseUrl, sessionId);
    const body = { document_id: documentId, start: 0, end: runeLength(text), text };
    const res = await fetch(`${baseUrl}/api/v1/sessions/${encodeURIComponent(sessionId)}/feedback`, {
      method: "POST",
      credentials: "include",
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
  article.dataset.status = c.status || "";

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
    const { baseUrl } = await loadConfig();
    const body = {
      type: "vocabulary.lookup",
      expression,
      reading: "",
      definition: "",
      example: "",
      source: { type: "web", title: sourceTitle || "" },
      client_event_id: crypto.randomUUID(),
    };
    const res = await fetch(`${baseUrl}/api/v1/vocabulary/events`, {
      method: "POST",
      credentials: "include",
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

// --- bootstrap -------------------------------------------------------------

function showView(id) {
  for (const el of document.querySelectorAll("main > section, #empty-state")) {
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
async function init() {
  wireVocabForm();
  const stored = await chrome.storage.session.get({ selection: "", title: "", mode: "" });
  const params = new URLSearchParams(location.search);
  const text = stored.selection || params.get("text") || "";
  const title = stored.title || params.get("title") || "";
  const mode = stored.mode || params.get("mode") || "";

  if (mode === "vocab" && text) {
    await saveVocabulary(text, title);
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
};
