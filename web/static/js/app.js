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

// New-session modal (sessions.html.tmpl): opened from #new-session-trigger,
// a native <dialog> so showModal() supplies the focus trap and Esc-to-close
// for free — nothing here reimplements either. What IS implemented:
// - backdrop click: a click landing on #session-modal itself (not a
//   descendant) is by construction a click outside .modal__panel, since
//   the dialog carries no padding of its own (components.css) — the
//   well-known idiom for "click outside a <dialog> closes it".
// - focus returning to the trigger: the dialog's native "close" event
//   fires for Esc, dialog.close(), AND a backdrop click alike, so one
//   listener covers all three closing paths.
// - reopening with the learner's input intact: POST /sessions
//   (sessionsCreate, internal/adapters/http/sessions.go) re-renders this
//   same page on a validation failure with the dialog carrying
//   data-reopen and the submitted values preserved — showModal() here
//   just needs to notice that attribute on load and open, so the error
//   is never a silently-closed modal with the learner's input gone.
(function () {
  "use strict";
  var dialog = document.getElementById("session-modal");
  var trigger = document.getElementById("new-session-trigger");
  if (!dialog || !trigger) return;

  trigger.addEventListener("click", function () {
    dialog.showModal();
  });

  var closeBtn = dialog.querySelector("[data-modal-close]");
  if (closeBtn) {
    closeBtn.addEventListener("click", function () {
      dialog.close();
    });
  }

  dialog.addEventListener("click", function (evt) {
    if (evt.target === dialog) dialog.close();
  });

  dialog.addEventListener("close", function () {
    trigger.focus();
  });

  if (dialog.hasAttribute("data-reopen")) {
    dialog.showModal();
  }
})();

// Delete-confirmation modal (partials/confirm_delete.html.tmpl): the
// one <dialog> on a page, shared by every delete control on it. A
// delete that fires on a single click, in an app someone uses daily,
// eventually destroys work — so nothing deletes without passing
// through here.
//
// It reuses the same native <dialog> the new-session modal above uses,
// for the same reasons (showModal() supplies the focus trap and
// Esc-to-close; a click landing on the dialog itself is a backdrop
// click; the "close" event covers all three closing paths). What
// differs is the trigger arrangement: there is one dialog and MANY
// triggers — one per row — so instead of a hardcoded id pair, each
// trigger carries the POST target and the human-readable name of the
// thing it deletes:
//
//   <button data-confirm-delete="/sessions/{id}/delete"
//           data-confirm-delete-label="日記の練習">削除</button>
//
// and this handler copies those onto the dialog's form before opening
// it. The form is a plain method="post" — CSRFProtect (internal/
// adapters/http/csrf.go) covers it by origin, and with JS off the
// triggers simply do nothing rather than deleting unconfirmed.
(function () {
  "use strict";
  var dialog = document.getElementById("confirm-delete-modal");
  if (!dialog) return;

  var form = dialog.querySelector("[data-confirm-delete-form]");
  var label = dialog.querySelector("[data-confirm-delete-label]");
  if (!form) return;

  var opener = null;

  document.addEventListener("click", function (evt) {
    var trigger = evt.target.closest("[data-confirm-delete]");
    if (!trigger) return;
    evt.preventDefault();
    // A trigger with an empty target opens nothing rather than opening a
    // dialog whose form would fall back to posting at the CURRENT url.
    // Every trigger the templates emit carries one; this is here so that
    // if one ever doesn't, the failure is "the button does nothing"
    // rather than "the button posts somewhere unintended".
    var action = trigger.getAttribute("data-confirm-delete");
    if (!action) return;
    opener = trigger;
    form.setAttribute("action", action);
    if (label) label.textContent = trigger.getAttribute("data-confirm-delete-label") || "";
    dialog.showModal();
  });

  dialog.querySelectorAll("[data-modal-close]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      dialog.close();
    });
  });

  dialog.addEventListener("click", function (evt) {
    if (evt.target === dialog) dialog.close();
  });

  // Focus goes back to the row's own delete button, not to a fixed
  // element: with one dialog serving many rows, "the trigger" is
  // whichever one opened it.
  dialog.addEventListener("close", function () {
    if (opener) opener.focus();
  });
})();

// Hamburger nav (layout.html.tmpl): #nav-toggle shows/hides #topnav
// below the 900px breakpoint (components.css) where the inline nav no
// longer fits the header in one row. #topnav has no native modal
// semantics (it's a <nav>, not a <dialog> — the header behind it must
// stay reachable/visible, unlike the session modal above), so open/close
// state, Esc, and click-outside are all handled by hand here.
(function () {
  "use strict";
  var toggle = document.getElementById("nav-toggle");
  var menu = document.getElementById("topnav");
  if (!toggle || !menu) return;

  function isOpen() {
    return menu.classList.contains("is-open");
  }

  function open() {
    menu.classList.add("is-open");
    toggle.setAttribute("aria-expanded", "true");
    document.addEventListener("keydown", onKeydown);
    document.addEventListener("click", onDocClick, true);
    var first = menu.querySelector("a");
    if (first) first.focus();
  }

  function close(returnFocus) {
    menu.classList.remove("is-open");
    toggle.setAttribute("aria-expanded", "false");
    document.removeEventListener("keydown", onKeydown);
    document.removeEventListener("click", onDocClick, true);
    if (returnFocus) toggle.focus();
  }

  function onKeydown(evt) {
    if (evt.key === "Escape") close(true);
  }

  // Capture-phase so this sees the click before it might otherwise be
  // stopped, and so a click on the toggle itself (which has its own
  // handler below) doesn't immediately re-close what that handler just
  // opened — toggle.contains(evt.target) is excluded here for exactly
  // that reason.
  function onDocClick(evt) {
    if (menu.contains(evt.target) || toggle.contains(evt.target)) return;
    close(true);
  }

  toggle.addEventListener("click", function () {
    if (isOpen()) close(true);
    else open();
  });
})();
