// imaging.js — fetch, downscale and budget an article's images. Shared,
// byte for byte, by the extension popup and by JLP's own phone pages
// (/reading/capture), which the server serves from its embedded copy of
// this file (package extension). No learning-domain logic, no chrome.*.
(function (global) {
  const MAX_EDGE = 1200;
  const MIN_EDGE = 200;
  const MAX_IMAGE_BYTES = 2 * 1024 * 1024;
  const PNG_KEEP_BYTES = 500 * 1024;

  async function fetchWithTimeout(url, ms, init = { credentials: "include" }) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), ms);
    try {
      const res = await fetch(url, { ...init, signal: ctl.signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return await res.blob();
    } finally {
      clearTimeout(timer);
    }
  }

  // downscale re-encodes one image: long side <= 1200 px; PNG kept when it
  // stays small (charts), else JPEG stepping down in quality until <= 2 MB.
  async function downscale(blob) {
    const bmp = await createImageBitmap(blob);
    let canvas;
    try {
      // The capture cannot size-check a lazy image before it loads, so the
      // decoded size is the real test: icons and avatars end here.
      if (bmp.width < MIN_EDGE || bmp.height < MIN_EDGE) throw new Error("too small");
      const scale = Math.min(1, MAX_EDGE / Math.max(bmp.width, bmp.height));
      canvas = new OffscreenCanvas(Math.round(bmp.width * scale), Math.round(bmp.height * scale));
      const ctx = canvas.getContext("2d");
      ctx.fillStyle = "#fff"; // JPEG has no alpha
      ctx.fillRect(0, 0, canvas.width, canvas.height);
      ctx.drawImage(bmp, 0, 0, canvas.width, canvas.height);
    } finally {
      bmp.close();
    }
    if (blob.type === "image/png") {
      const png = await canvas.convertToBlob({ type: "image/png" });
      if (png.size < PNG_KEEP_BYTES) return png;
    }
    for (const quality of [0.82, 0.72, 0.62]) {
      const jpg = await canvas.convertToBlob({ type: "image/jpeg", quality });
      if (jpg.size <= MAX_IMAGE_BYTES) return jpg;
    }
    throw new Error("too large");
  }

  // The server caps a multipart request at 15 MiB; keep the images to 14
  // so the metadata part and framing fit.
  const UPLOAD_BUDGET = 14 * 1024 * 1024;

  // withinBudget keeps the prepared images in order while their running
  // total stays under the budget; the rest are dropped (counted by the caller).
  function withinBudget(ok, budget = UPLOAD_BUDGET) {
    let total = 0;
    const kept = [];
    for (const x of ok) {
      if (total + x.blob.size > budget) break;
      total += x.blob.size;
      kept.push(x);
    }
    return kept;
  }

  // prepareImages fetches and downscales the captured figures, all at once
  // (each has its own timeout, so the popup is not held open for the sum).
  // A figure that fails is dropped, never the import; results keep the
  // article's order. opts.fetchInit overrides the fetch options: the
  // extension sends cookies, JLP's own pages must not (credentials: "omit").
  async function prepareImages(figures, opts = {}) {
    const list = figures || [];
    const settled = await Promise.allSettled(
      list.map(async (f) => downscale(await fetchWithTimeout(f.src, 10000, opts.fetchInit))),
    );
    let ok = [];
    let failed = 0;
    settled.forEach((r, i) => {
      if (r.status === "fulfilled") {
        ok.push({ meta: list[i], blob: r.value });
      } else {
        failed++;
        console.warn("[JLP] image skipped", list[i].src, String(r.reason));
      }
    });
    const kept = withinBudget(ok);
    if (kept.length < ok.length) {
      console.warn("[JLP] images over the upload budget dropped", ok.length - kept.length);
      failed += ok.length - kept.length;
      ok = kept;
    }
    return { ok, failed };
  }

  global.JLPImaging = {
    MAX_EDGE, MIN_EDGE, MAX_IMAGE_BYTES, PNG_KEEP_BYTES, UPLOAD_BUDGET,
    fetchWithTimeout, downscale, withinBudget, prepareImages,
  };
})(globalThis);
