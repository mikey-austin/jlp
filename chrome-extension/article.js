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
  // Images are judged against SKIP minus figure/figcaption: those are
  // furniture for TEXT (captions are not prose) but the very thing an
  // article's photo lives in.
  const SKIP_FOR_IMAGES = SKIP.replace("figure, figcaption, ", "");
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

  // figureOf reads one <img> as a figure, or null for page furniture:
  // anything inside SKIP, links (a thumbnail to another story), SVG, and
  // images under 200 px either way (pixels, icons, logos, avatars).
  function figureOf(img) {
    if (img.closest(SKIP_FOR_IMAGES) || img.closest("a")) return null;
    // A lazy image still shows its placeholder (1x1, or a broken-image
    // box), so its measured size says nothing: trust only what it declares.
    const lazy = img.hasAttribute("data-src") || img.hasAttribute("data-srcset") ||
      img.hasAttribute("data-original") || img.hasAttribute("data-lazy-src");
    const w = (!lazy && img.naturalWidth) || parseInt(img.getAttribute("width"), 10) || (!lazy && img.getBoundingClientRect().width);
    const h = (!lazy && img.naturalHeight) || parseInt(img.getAttribute("height"), 10) || (!lazy && img.getBoundingClientRect().height);
    if ((w && w < 200) || (h && h < 200)) return null;
    const src = bestSrc(img);
    if (!src || /\.svg(\?|$)/i.test(src) || src.startsWith("data:image/svg")) return null;
    const fig = img.closest("figure");
    const cap = fig && fig.querySelector("figcaption");
    return { src, alt: (img.getAttribute("alt") || "").trim(), caption: cap ? text(cap) : "" };
  }

  // bestSrc picks the largest candidate: the widest srcset entry, then
  // what the browser chose, then src, then the lazy-loading attributes
  // sites keep the real URL in until the image scrolls into view.
  function bestSrc(img) {
    const set = img.getAttribute("srcset") || img.getAttribute("data-srcset") || "";
    let best = "", bestW = 0;
    for (const part of set.split(",")) {
      const [u, d] = part.trim().split(/\s+/);
      const w = parseInt(d, 10) || 1;
      if (u && w >= bestW) { best = u; bestW = w; }
    }
    const raw = best || img.currentSrc || img.getAttribute("data-src") || img.getAttribute("data-original") ||
      img.getAttribute("data-lazy-src") || img.getAttribute("src") || "";
    try { return raw ? new URL(raw, location.href).href : ""; } catch (_) { return ""; }
  }

  // paragraphs keeps headings and paragraphs in document order, one per
  // block, so the server sees the article's own paragraph breaks. Images
  // are collected in the same walk so each figure knows which paragraph it
  // follows.
  function paragraphs(root) {
    const blocks = [];
    const figs = [];
    let lastHeading = -1; // block index of the heading whose section we are in
    for (const el of root.querySelectorAll("h2, h3, p, blockquote, li, img")) {
      if (el.tagName === "IMG") {
        const f = figureOf(el);
        if (f) figs.push({ ...f, blockIndex: blocks.length - 1, heading: lastHeading });
        continue;
      }
      if (isFurniture(el)) continue;
      // A <p> inside an <li> or <blockquote> is counted once, via the <p>.
      if ((el.tagName === "LI" || el.tagName === "BLOCKQUOTE") && el.querySelector("p")) continue;
      const t = text(el);
      if (!t) continue;
      const heading = el.tagName === "H2" || el.tagName === "H3";
      if (heading) lastHeading = blocks.length;
      blocks.push({ t, heading });
    }
    // A heading with no prose before the next heading titled a block the
    // filters above dropped (「あわせて読みたい」 over a row of link cards),
    // so it goes too.
    const keptIdx = []; // block index -> paragraph index in `out`, or -1
    const isKept = [];
    const out = [];
    let n = -1;
    blocks.forEach((b, i) => {
      const kept = !b.heading || (blocks[i + 1] && !blocks[i + 1].heading);
      isKept[i] = kept;
      if (kept) { n++; out.push(b.t); }
      keptIdx[i] = n;
    });
    // An image in the section of a dropped heading belongs to the same
    // furniture (a promo module in plain divs, NHK's 「最新・注目の動画」).
    const figures = figs.filter((f) => f.heading < 0 || isKept[f.heading]).slice(0, 12).map((f, i) => {
      const after = f.blockIndex < 0 ? -1 : keptIdx[f.blockIndex];
      return { src: f.src, caption: f.caption, alt: f.alt, after_paragraph: after,
               after_text: after >= 0 ? out[after].slice(0, 40) : "", lead: i === 0, in_text: true };
    });
    return { paragraphs: out.length ? out : [text(root)], figures };
  }

  const canonical = document.querySelector("link[rel='canonical']");
  const timeEl = document.querySelector("time[datetime]");
  const root = pickRoot();
  const body = paragraphs(root);

  // No photo in the body: fall back to the page's own share image, as a
  // lead only (in_text false) so it is not dropped into the middle of prose.
  let figures = body.figures;
  if (!figures.length) {
    const og = meta("meta[property='og:image']");
    if (og) figures = [{ src: new URL(og, location.href).href, caption: "", alt: "", after_paragraph: -1, after_text: "", lead: true, in_text: false }];
  }

  return {
    url: (canonical && canonical.href) || location.href,
    title: meta("meta[property='og:title']") || text(document.querySelector("h1")) || document.title,
    source: meta("meta[property='og:site_name']") || location.hostname,
    author: meta("meta[name='author']") || meta("meta[property='article:author']"),
    published_at: meta("meta[property='article:published_time']") || meta("meta[itemprop='datePublished']") || (timeEl ? timeEl.getAttribute("datetime") : ""),
    selection: window.getSelection().toString(),
    content: body.paragraphs.join("\n\n"),
    figures,
  };
})();
