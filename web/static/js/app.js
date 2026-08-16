window.jlp = window.jlp || {};
// Convert a UTF-16 index (textarea selectionStart/End) to a rune offset.
window.jlp.runeOffset = (text, utf16Index) => Array.from(text.slice(0, utf16Index)).length;

window.jlp.selectionPayload = () => {
  const ta = document.getElementById("editor");
  return {
    document_id: ta.dataset.docId,
    start: window.jlp.runeOffset(ta.value, ta.selectionStart),
    end: window.jlp.runeOffset(ta.value, ta.selectionEnd),
    text: ta.value.slice(ta.selectionStart, ta.selectionEnd),
  };
};

// PWA shell: register the service worker on every page (not just the
// editor), scoped to the whole app so it can intercept navigations and
// serve the offline fallback.
if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/static/sw.js", { scope: "/" });
  });
}

(function () {
  const ta = document.getElementById("editor");
  if (!ta) return;
  const state = document.getElementById("save-state");
  const runes = document.getElementById("rune-count");
  let timer = null, composing = false;
  ta.addEventListener("compositionstart", () => { composing = true; });
  ta.addEventListener("compositionend", () => { composing = false; queue(); });
  ta.addEventListener("input", () => { if (!composing) queue(); });
  function queue() {
    state.dataset.state = "dirty"; state.textContent = "未保存";
    clearTimeout(timer); timer = setTimeout(save, 1500);
  }
  async function save() {
    const body = new URLSearchParams({ content: ta.value });
    const res = await fetch(`/documents/${ta.dataset.docId}`, { method: "POST", body });
    if (!res.ok) { state.dataset.state = "error"; state.textContent = "保存失敗"; return; }
    const j = await res.json();
    state.dataset.state = "saved"; state.textContent = "保存済み";
    runes.textContent = j.runes;
  }
})();

// Phase 4 Task W item 1: フィードバックを取得's progress state — a
// spinner, the requested adapter's name, and a live elapsed-seconds
// timer, written directly into #feedback-results the instant the
// request starts (htmx:beforeRequest) so the pane is IMMEDIATELY
// replaced rather than looking frozen for the 5-30s a local model or a
// CLI adapter can take. The eventual htmx response swap (on success)
// overwrites this with the real result; on failure (htmx does not swap
// non-2xx responses by default) htmx:responseError below shows the
// error text instead — "an explicit override does not fall back" means
// a failure here IS the answer the operator needs to see, not a silent
// blank.
(function () {
  "use strict";
  const btn = document.getElementById("feedback-btn");
  const results = document.getElementById("feedback-results");
  const select = document.getElementById("provider-override");
  if (!btn || !results) return;

  let timerId = null;
  function stopTimer() {
    if (timerId) { clearInterval(timerId); timerId = null; }
  }

  btn.addEventListener("htmx:beforeRequest", function () {
    const label = select && select.selectedOptions.length ? select.selectedOptions[0].text : "AI";
    results.innerHTML =
      '<div class="feedback-progress btn--busy" role="status" aria-live="polite">' +
      '<span class="btn__dot"></span>' +
      "<span>" + label + " に問い合わせ中… <span id=\"feedback-timer\">0</span>秒</span>" +
      "</div>";
    const start = Date.now();
    const timerEl = document.getElementById("feedback-timer");
    stopTimer();
    timerId = setInterval(function () {
      if (timerEl) timerEl.textContent = String(Math.floor((Date.now() - start) / 1000));
    }, 1000);
  });
  btn.addEventListener("htmx:afterRequest", stopTimer);
  btn.addEventListener("htmx:responseError", function (evt) {
    stopTimer();
    const status = evt.detail && evt.detail.xhr ? evt.detail.xhr.status : "";
    const text = evt.detail && evt.detail.xhr ? evt.detail.xhr.responseText : "";
    results.innerHTML = '<p class="error">フィードバックの取得に失敗しました（' + status + "）: " +
      (text || "").replace(/[<>&]/g, function (c) { return { "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c]; }) +
      "</p>";
  });
  btn.addEventListener("htmx:sendError", function () {
    stopTimer();
    results.innerHTML = '<p class="error">フィードバックの取得に失敗しました（ネットワークエラー）。</p>';
  });
})();

// Phase 4 Task W item 2: mark whichever feedback-history row was just
// clicked .is-active. Delegated on document (not a direct listener on
// #feedback-history) because that element gets wholesale replaced by
// an htmx out-of-band swap after every new フィードバックを取得 —see
// feedback_history.html.tmpl — so a reference captured once would go
// stale.
document.addEventListener("click", function (evt) {
  const row = evt.target.closest(".history-row");
  if (!row) return;
  document.querySelectorAll(".history-row.is-active").forEach(function (r) {
    r.classList.remove("is-active");
  });
  row.classList.add("is-active");
});

// Phase 4 Task W item 4: 添削/会話 workspace tabs, persisted per session
// in localStorage so returning to a session later shows the same tab.
(function () {
  "use strict";
  const root = document.querySelector(".workspace-tabs");
  if (!root) return;
  const key = "jlp-workspace-tab-" + root.dataset.sessionId;
  const buttons = root.querySelectorAll("[data-tab]");
  const panels = root.querySelectorAll("[data-tab-panel]");

  function show(name) {
    buttons.forEach(function (b) {
      const active = b.dataset.tab === name;
      b.classList.toggle("is-active", active);
      b.setAttribute("aria-selected", String(active));
    });
    panels.forEach(function (p) {
      p.hidden = p.dataset.tabPanel !== name;
    });
  }

  buttons.forEach(function (b) {
    b.addEventListener("click", function () {
      show(b.dataset.tab);
      try { window.localStorage.setItem(key, b.dataset.tab); } catch (e) {
        // localStorage unavailable (private mode / disabled) — the tab
        // still switches for this page view, it just won't persist.
      }
    });
  });

  let saved = null;
  try { saved = window.localStorage.getItem(key); } catch (e) {
    // See above.
  }
  show(saved === "conversation" ? "conversation" : "feedback");
})();
