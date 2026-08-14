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
