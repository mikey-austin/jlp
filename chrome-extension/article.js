// article.js — article capture for 「JLPでKindle版を作成」. Like
// content.js it is not a manifest content_script: background.js (on a
// context-menu click) or popup.js (on the toolbar button) injects it on
// demand with chrome.scripting.executeScript, under the user gesture
// activeTab needs.
//
// It reads what the learner can SEE in their own logged-in tab — the
// server never fetches the page itself. That is deliberate: for a
// subscription site (WSJ 日本版) there is no scraper to authenticate and
// nothing that could be mistaken for getting around a paywall; the text
// comes from the learner's own session, exactly as if they had selected
// and pasted it.
//
// No learning-domain logic here either (PRD §40): this only finds text
// and metadata. The server normalises, validates and hashes it.
//
// Like content.js, the file is ONE expression — the injected script's
// completion value is what executeScript returns.
(function () {
  const meta = (sel) => {
    const el = document.querySelector(sel);
    return el ? (el.getAttribute("content") || "").trim() : "";
  };
  const text = (el) => (el ? (el.innerText || el.textContent || "").trim() : "");

  // Anything inside these is page furniture, not article.
  const SKIP = "nav, aside, footer, header, figure, figcaption, form, button, [aria-hidden='true'], [hidden], [role='navigation'], [role='complementary'], script, style, noscript";

  // Link density is the other half of "page furniture": sites that don't
  // mark up <nav>/<aside> (NHK's 「あわせて読みたい」 cards sit in plain divs
  // inside <main>) still give themselves away by being mostly link text.
  // Prose is not.
  const linkDensity = (el) => {
    const all = text(el).length;
    if (!all) return 0;
    const linked = [...el.querySelectorAll("a")].reduce((n, a) => n + text(a).length, 0);
    return linked / all;
  };
  const isFurniture = (el) => !!el.closest(SKIP) || !!el.closest("a") || linkDensity(el) > 0.5;

  // pickRoot finds the element holding the article body: an <article>
  // (or [itemprop=articleBody]) when the page marks one up — news sites
  // mostly do — else the container whose direct <p> children carry the
  // most text, a small Readability-style heuristic.
  function pickRoot() {
    // Most specific markup first: an articleBody beats an <article>
    // (which may hold the headline, share bar and related links too),
    // which beats <main>.
    let marked = [];
    for (const sel of ["[itemprop='articleBody']", "article", "main"]) {
      marked = [...document.querySelectorAll(sel)];
      if (marked.length) break;
    }
    let best = null;
    let bestLen = 0;
    const scoreOf = (el) => [...el.querySelectorAll("p")]
      .filter((p) => !isFurniture(p))
      .reduce((n, p) => n + text(p).length, 0);
    for (const el of marked) {
      const n = scoreOf(el);
      if (n > bestLen) { best = el; bestLen = n; }
    }
    if (best && bestLen > 200) return best;

    const byParent = new Map();
    for (const p of document.querySelectorAll("p")) {
      if (isFurniture(p)) continue;
      const parent = p.parentElement;
      if (!parent) continue;
      byParent.set(parent, (byParent.get(parent) || 0) + text(p).length);
    }
    for (const [el, n] of byParent) {
      if (n > bestLen) { best = el; bestLen = n; }
    }
    return best || document.body;
  }

  // paragraphs keeps headings and paragraphs in document order, one per
  // block, so the server sees the article's own paragraph breaks.
  function paragraphs(root) {
    const blocks = [];
    for (const el of root.querySelectorAll("h2, h3, p, blockquote, li")) {
      if (isFurniture(el)) continue;
      // A <p> inside an <li> or <blockquote> is counted once, via the <p>.
      if ((el.tagName === "LI" || el.tagName === "BLOCKQUOTE") && el.querySelector("p")) continue;
      const t = text(el);
      if (t) blocks.push({ t, heading: el.tagName === "H2" || el.tagName === "H3" });
    }
    // A heading with no prose before the next heading titled a block the
    // filters above dropped (「あわせて読みたい」 over a row of link cards),
    // so it goes too.
    const out = blocks
      .filter((b, i) => !b.heading || (blocks[i + 1] && !blocks[i + 1].heading))
      .map((b) => b.t);
    return out.length ? out : [text(root)];
  }

  const canonical = document.querySelector("link[rel='canonical']");
  const timeEl = document.querySelector("time[datetime]");
  const root = pickRoot();

  return {
    url: (canonical && canonical.href) || location.href,
    title: meta("meta[property='og:title']") || text(document.querySelector("h1")) || document.title,
    source: meta("meta[property='og:site_name']") || location.hostname,
    author: meta("meta[name='author']") || meta("meta[property='article:author']"),
    published_at: meta("meta[property='article:published_time']") || meta("meta[itemprop='datePublished']") || (timeEl ? timeEl.getAttribute("datetime") : ""),
    selection: window.getSelection().toString(),
    content: paragraphs(root).join("\n\n"),
  };
})();
