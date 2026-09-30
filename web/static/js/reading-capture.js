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
      `${article.source || ""}　${Array.from(article.content).length}字　画像 最大${figs.length}枚`;
    status.textContent = "";
    help.hidden = true; // a late article can arrive after the give-up message
    card.hidden = false;
  });

  // send posts the article to `path` with its images (one-shot submit and
  // draft add share this), resolving the response.
  function send(path, fields) {
    const post = (init) => fetch(path, { method: "POST", credentials: "same-origin", ...init });
    return JLPImaging.sendWithImages(post, fields, article.figures || [], {
      fetchInit: { credentials: "omit", mode: "cors" },
      onStatus: (t) => { status.textContent = t; },
    });
  }
  function articleFields() {
    return {
      url: article.url || "", title: article.title || "", source: article.source || "",
      author: article.author || "", published_at: article.published_at || "",
      content: article.content || "", selection: article.selection || "",
    };
  }

  // The open draft, if any, is known on load (the session cookie rides
  // along); adding to it still takes the tap.
  const draftStatus = document.getElementById("capture-draft-status");
  const draftBtn = document.getElementById("capture-draft");
  const reviewLink = document.getElementById("capture-review");
  function showDraft(d) {
    draftStatus.textContent = `下書き：${d.pages}ページ・段落${d.paragraphs}・画像${d.images}`;
    draftStatus.hidden = false;
    draftBtn.textContent = "下書きに追加";
    reviewLink.href = d.review_url;
    reviewLink.hidden = false;
  }
  fetch("/api/v1/reading/drafts/active", { credentials: "same-origin" })
    .then((r) => (r.ok ? r.json() : null)).then((d) => { if (d) showDraft(d); }).catch(() => {});

  draftBtn.addEventListener("click", async () => {
    draftBtn.disabled = true;
    try {
      const { res } = await send("/api/v1/reading/drafts/active/parts", articleFields());
      if (!res.ok) {
        let msg = `送信に失敗しました (${res.status})`;
        try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
        throw new Error(msg);
      }
      showDraft(await res.json());
      status.textContent = "追加しました。";
    } catch (err) {
      status.textContent = `エラー: ${err.message}`;
    }
    draftBtn.disabled = false;
  });

  document.getElementById("capture-send").addEventListener("click", async (e) => {
    // currentTarget is null once dispatch ends, i.e. after the first await.
    const btn = e.currentTarget;
    btn.disabled = true;
    try {
      const { res } = await send("/api/v1/reading/articles", { ...articleFields(), deliver: true });
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
