// chrome-shim.js — a minimal in-memory stub of chrome.storage.{sync,session}
// plus no-op chrome.runtime, used ONLY when this page is opened outside a
// real extension context (window.chrome.storage absent — e.g. popup.html
// or options.html loaded directly via file:// for automated verification;
// see chrome-extension/README.md's "Testing without load-unpacked"
// section). popup.html/options.html only ever request this file through a
// conditional document.write that never fires once a real chrome.storage
// exists, so a packed extension (which excludes shim/ from the build —
// see `make ext-build`) never even attempts to fetch it.
//
// Deliberately tiny: get/set/remove on two independent in-memory areas,
// both callback- and Promise-style (mirroring the real API, which
// supports both), no persistence across reloads, no onChanged. That's
// the full surface popup.js/options.js touch.
(function () {
  if (window.chrome && window.chrome.storage) return;

  function makeArea() {
    const data = {};
    function resolveGet(keys) {
      if (keys == null) return { ...data };
      if (typeof keys === "string") return keys in data ? { [keys]: data[keys] } : {};
      if (Array.isArray(keys)) {
        const out = {};
        for (const k of keys) if (k in data) out[k] = data[k];
        return out;
      }
      // Object form: keys are defaults, overridden by any stored value.
      return { ...keys, ...Object.fromEntries(Object.entries(data).filter(([k]) => k in keys)) };
    }
    return {
      get(keys, cb) {
        const p = Promise.resolve(resolveGet(keys));
        if (cb) { p.then(cb); return; }
        return p;
      },
      set(items, cb) {
        Object.assign(data, items);
        const p = Promise.resolve();
        if (cb) { p.then(cb); return; }
        return p;
      },
      remove(keys, cb) {
        for (const k of Array.isArray(keys) ? keys : [keys]) delete data[k];
        const p = Promise.resolve();
        if (cb) { p.then(cb); return; }
        return p;
      },
    };
  }

  window.chrome = window.chrome || {};
  // local joins sync/session because the API token lives there — see
  // options.js's saveOptions for why a credential does not belong in
  // sync. Without it here, every popup.js call throws before it can even
  // build a request, and the documented verification protocol (which
  // drives popup.js through this shim) could not exercise the extension
  // at all.
  window.chrome.storage = { sync: makeArea(), session: makeArea(), local: makeArea() };
  window.chrome.runtime = {
    getURL: (path) => path,
    sendMessage: () => {},
  };
})();
