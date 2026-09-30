// reading-share.js — lands an Android share (see sw.js: the worker kept
// the shared fields and redirected here) and shows a card. Nothing is
// submitted until the learner taps: the worker parks any cross-site
// POST to /reading/share too, so arriving here proves nothing. Text
// only; images need the bookmarklet.
(async () => {
  const status = document.getElementById("share-status");
  const card = document.getElementById("share-card");
  const URL_ONLY = "URLだけでは本文を読めません。本文を選択して共有するか、ブックマークレットを使ってください";
  const id = new URLSearchParams(location.search).get("pending");
  const key = `/reading/share/pending/${id}`;
  let entry;
  try {
    const cache = id && (await caches.open("jlp-share"));
    const hit = cache && (await cache.match(key));
    if (!hit) { status.textContent = "共有された内容が見つかりません。もう一度共有してください。"; return; }
    entry = await hit.json();
    await cache.delete(key);
  } catch (err) {
    status.textContent = `共有された内容を読み込めませんでした。もう一度共有してください。(${err.message})`;
    return;
  }

  // Some apps append the page URL as the last line of the shared text.
  let text = (entry.text || "").trim();
  let url = (entry.url || "").trim();
  const lines = text.split(/\r?\n/);
  const last = lines[lines.length - 1].trim();
  if (/^https?:\/\/\S+$/.test(last)) { url = url || last; lines.pop(); text = lines.join("\n").trim(); }
  if (!text) { status.textContent = URL_ONLY; return; }
  const title = (entry.title || "").trim() || Array.from(text.split("\n")[0]).slice(0, 80).join("");

  document.getElementById("share-title").textContent = title;
  document.getElementById("share-meta").textContent =
    `${url}${url ? "　" : ""}${Array.from(text).length}字　テキストのみ（画像なし）`;
  status.textContent = "";
  card.hidden = false;

  document.getElementById("share-send").addEventListener("click", async (e) => {
    // currentTarget is null once dispatch ends, i.e. after the first await.
    const btn = e.currentTarget;
    btn.disabled = true;
    status.textContent = "送信しています…";
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
      btn.disabled = false;
    }
  });
})();
