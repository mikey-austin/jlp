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

// Progress state for any htmx trigger that asks for it: a spinner, the
// requested adapter's name, and a live elapsed-seconds timer, written
// into the target the instant the request starts (htmx:beforeRequest) so
// the pane is IMMEDIATELY replaced rather than looking frozen for the
// 5-30s a local model or a CLI adapter can take. The eventual response
// swap overwrites this; on failure — htmx does NOT swap a non-2xx
// response — the error text is shown instead, because a failure here is
// the answer the operator needs to see, not a silent blank.
//
// Opt in per element rather than by id. This was bound to #feedback-btn
// alone, which is exactly why 練習する shipped with none of it: a five
// second model call behind a button that never changed, and a failed one
// that left the page byte-identical. An element declares:
//
//   data-progress-into="#exercise-area"        (required) where it goes
//   data-progress-label-from="#provider-override"  (optional) adapter name
//   data-progress-verb="問題を作成中"           (optional) copy
//
// Delegated on document, so elements swapped in by htmx (the result
// partial's 次の問題へ) are covered without re-binding.
(function () {
  "use strict";
  const timers = new WeakMap();

  function targetOf(el) {
    const sel = el && el.dataset && el.dataset.progressInto;
    return sel ? document.querySelector(sel) : null;
  }

  function stopTimer(el) {
    const id = timers.get(el);
    if (id) { clearInterval(id); timers.delete(el); }
  }

  function escapeText(s) {
    return String(s || "").replace(/[<>&]/g, function (c) {
      return { "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c];
    });
  }

  document.addEventListener("htmx:beforeRequest", function (evt) {
    const el = evt.target;
    const into = targetOf(el);
    if (!into) return;

    const sel = el.dataset.progressLabelFrom
      ? document.querySelector(el.dataset.progressLabelFrom) : null;
    const label = sel && sel.selectedOptions && sel.selectedOptions.length
      ? sel.selectedOptions[0].text : "AI";
    const verb = el.dataset.progressVerb || "に問い合わせ中";

    into.innerHTML =
      '<div class="feedback-progress btn--busy" role="status" aria-live="polite">' +
      '<span class="btn__dot"></span>' +
      "<span>" + escapeText(label) + " " + escapeText(verb) +
      "… <span data-progress-timer>0</span>秒</span>" +
      "</div>";

    const start = Date.now();
    const timerEl = into.querySelector("[data-progress-timer]");
    stopTimer(el);
    timers.set(el, setInterval(function () {
      if (timerEl) timerEl.textContent = String(Math.floor((Date.now() - start) / 1000));
    }, 1000));
  });

  document.addEventListener("htmx:afterRequest", function (evt) {
    if (targetOf(evt.target)) stopTimer(evt.target);
  });

  document.addEventListener("htmx:responseError", function (evt) {
    const el = evt.target;
    const into = targetOf(el);
    if (!into) return;
    stopTimer(el);
    const xhr = evt.detail && evt.detail.xhr;
    into.innerHTML = '<p class="error">失敗しました（' + (xhr ? xhr.status : "") + "）: " +
      escapeText(xhr ? xhr.responseText : "") + "</p>";
  });

  document.addEventListener("htmx:sendError", function (evt) {
    const el = evt.target;
    const into = targetOf(el);
    if (!into) return;
    stopTimer(el);
    into.innerHTML = '<p class="error">失敗しました（ネットワークエラー）。</p>';
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

  // The dialog's default copy describes SOFT delete, which is true for
  // sessions/words/lessons and false for anything else. A trigger can
  // override the title, the sentence after the label, and the confirm
  // button; these captured defaults are what everything else keeps.
  var title = dialog.querySelector("#confirm-delete-title");
  var note = dialog.querySelector("[data-confirm-delete-note]");
  var confirm = dialog.querySelector("[data-confirm-delete-confirm]");
  var defaults = {
    title: title ? title.textContent : "",
    note: note ? note.textContent : "",
    confirm: confirm ? confirm.textContent : "",
  };

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
    if (title) title.textContent = trigger.getAttribute("data-confirm-delete-title") || defaults.title;
    if (note) note.textContent = trigger.getAttribute("data-confirm-delete-note") || defaults.note;
    if (confirm) confirm.textContent = trigger.getAttribute("data-confirm-delete-confirm") || defaults.confirm;
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

// As-you-type feedback for cloze drills: the input turns green once what
// has been typed matches the expected answer so far, and red once it
// cannot. Nothing is submitted and nothing is graded here — the real
// answer still goes through the same POST as every other drill; this
// only saves the learner from finishing a word they already got wrong.
//
// Opt-in via data-live-check, which the server sets ONLY for a cloze
// over the learner's own vocabulary (see exerciseView.LiveCheck). It is
// never present for a generated exercise, so this cannot leak an answer
// the learner was meant to work out.
//
// Delegated on document so inputs htmx swaps in are covered.
(function () {
  "use strict";
  // Compare by code point, not by UTF-16 unit: a kanji outside the BMP
  // is one character to the learner and two to string indexing.
  function startsWith(expected, typed) {
    const e = Array.from(expected);
    const p = Array.from(typed);
    if (p.length > e.length) return false;
    return p.every((c, i) => c === e[i]);
  }

  document.addEventListener("input", function (evt) {
    const input = evt.target;
    if (!input || !input.dataset || !input.dataset.liveCheck) return;

    const expected = input.dataset.liveCheck;
    const typed = input.value;
    const hint = document.getElementById("live-check-hint");

    let state = "empty";
    if (typed.length > 0) {
      if (typed === expected) state = "complete";
      else if (startsWith(expected, typed)) state = "partial";
      else state = "wrong";
    }
    input.dataset.checkState = state;
    if (hint) {
      hint.dataset.state = state;
      hint.textContent = { complete: "✓", partial: "…", wrong: "✗" }[state] || "";
    }
  });
})();

// Speak-this-aloud buttons.
//
// Any element carrying data-speak is clickable to hear its text: the
// word on a flip card, the example sentence beneath it, the explanation
// on a result. One delegated listener rather than a listener per button,
// because these arrive by htmx swap and re-binding after every swap is
// how one gets missed.
//
// TWO voices, in preference order:
//
//   1. POST /speech/say — the VOICEVOX engine, when one is configured.
//      A purpose-built Japanese synthesizer; pitch accent and rhythm are
//      the things a learner is trying to acquire, and it gets them right.
//   2. window.speechSynthesis — the browser's own voice. No server, no
//      sidecar, no network. Lower quality, and worth having anyway:
//      without it the whole feature is dark wherever the engine is not
//      deployed, which today is production.
//
// The server is tried first and its absence is remembered, so a
// deployment without TTS pays one 503 per page load rather than one per
// click. Only when BOTH are unavailable does a button say so.
(function () {
  "use strict";
  let current = null;          // the <audio> element currently playing
  let serverMute = false;      // /speech/say answered 503 — stop asking

  function stopAll() {
    if (current) { current.pause(); current = null; }
    // Cancels a browser utterance too: tapping a word and then its
    // sentence should replace the first, whichever voice said it.
    if (window.speechSynthesis) window.speechSynthesis.cancel();
  }

  // A Japanese voice, or null. Reading Japanese aloud with an English
  // voice is worse than silence — it teaches the wrong pronunciation —
  // so a browser with no ja voice counts as having no browser TTS.
  function japaneseVoice() {
    if (!window.speechSynthesis) return null;
    const voices = window.speechSynthesis.getVoices() || [];
    return voices.find((v) => (v.lang || "").toLowerCase().startsWith("ja")) || null;
  }

  // getVoices() is empty until the list loads on some browsers; asking
  // again after voiceschanged is the documented way round it.
  if (window.speechSynthesis) {
    window.speechSynthesis.addEventListener?.("voiceschanged", function () {});
  }

  async function speakViaServer(text, btn) {
    if (serverMute) return false;
    const res = await fetch("/speech/say", {
      method: "POST",
      body: new URLSearchParams({ text }),
    });
    if (res.status === 503) {
      // Not configured. Remembered for the life of the page.
      serverMute = true;
      return false;
    }
    if (!res.ok) return false;

    const url = URL.createObjectURL(await res.blob());
    const audio = new Audio(url);
    current = audio;
    audio.addEventListener("ended", function () {
      URL.revokeObjectURL(url);
      btn.dataset.speaking = "";
      if (current === audio) current = null;
    });
    btn.dataset.speaking = "playing";
    await audio.play();
    return true;
  }

  function speakViaBrowser(text, btn) {
    const voice = japaneseVoice();
    if (!voice) return false;

    const utter = new SpeechSynthesisUtterance(text);
    utter.voice = voice;
    utter.lang = voice.lang;
    // Slightly under natural pace: this is being replayed to be copied,
    // not listened to.
    utter.rate = 0.9;
    utter.addEventListener("end", function () { btn.dataset.speaking = ""; });
    btn.dataset.speaking = "playing";
    window.speechSynthesis.speak(utter);
    return true;
  }

  document.addEventListener("click", async function (evt) {
    const btn = evt.target.closest("[data-speak]");
    if (!btn) return;
    evt.preventDefault();

    const text = btn.dataset.speak;
    if (!text) return;

    stopAll();
    btn.dataset.speaking = "loading";

    try {
      if (await speakViaServer(text, btn)) return;
    } catch {
      // Network failure reaching our own server; the browser voice may
      // still work, so fall through rather than giving up.
    }
    if (speakViaBrowser(text, btn)) return;

    btn.dataset.speaking = "unavailable";
  });
})();

// The 練習 controls that must travel with every request.
//
// The adapter select and the drill-type checkboxes live OUTSIDE
// #exercise-area so a choice made once survives every swap — which means
// each request has to go and read them. Collected here rather than
// spelled out in each hx-vals, because there are four call sites and a
// fifth that forgets one is how a setting silently stops applying.
(function () {
  "use strict";
  window.jlp = window.jlp || {};

  window.jlp.practiceVals = function () {
    const provider = document.getElementById("provider-override");
    const boxes = document.querySelectorAll('#drill-kinds input[name="kind"]:checked');
    // The run's covered words, read from the DOM rather than baked into
    // each hx-vals: a bare id interpolated into a JS expression is a
    // syntax error, and hx-vals failing to parse means the request never
    // goes out at all.
    const drilled = document.getElementById("run-drilled");
    return {
      provider_override: provider ? provider.value : "",
      // htmx serialises an array into repeated parameters, which is what
      // r.Form["kind"] reads on the other side.
      kind: Array.from(boxes, (b) => b.value),
      drilled: drilled ? drilled.dataset.drilled || "" : "",
    };
  };
})();

// Hide the start controls once a round is underway.
//
// The adapter select and the drill-type checkboxes have to STAY in the
// DOM — every request reads them (see practiceVals) — so they are
// hidden, never removed. They come back at the end of a set, which is
// the moment changing them is useful again.
//
// Driven by what is actually on screen rather than by a counter: the
// page is mid-round when it is showing a question or an answer, and back
// at the start when it is showing the summary or nothing.
(function () {
  "use strict";
  const practice = document.querySelector(".practice");
  if (!practice) return;

  function sync() {
    const area = document.getElementById("exercise-area");
    if (!area) return;
    const running =
      (area.querySelector(".exercise") || area.querySelector(".exercise-result")) &&
      !area.querySelector(".run-summary");
    practice.classList.toggle("is-running", Boolean(running));
  }

  document.addEventListener("htmx:afterSwap", sync);
  sync();
})();
