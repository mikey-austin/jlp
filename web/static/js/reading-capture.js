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
    help.hidden = true; // a late article can arrive after the give-up message
    card.hidden = false;
  });

  document.getElementById("capture-send").addEventListener("click", async (e) => {
    // currentTarget is null once dispatch ends, i.e. after the first await.
    const btn = e.currentTarget;
    btn.disabled = true;
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
      btn.disabled = false;
    }
  });
})();
