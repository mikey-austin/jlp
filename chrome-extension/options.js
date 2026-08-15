// options.js — stores {baseUrl, sessionId} in chrome.storage.sync
// (chrome.storage.sync, not .local: small, textual, and worth carrying
// across a signed-in Chrome profile's devices, same rationale as any
// other extension's settings page). No API calls happen here; popup.js
// is the only file that talks to the JLP API.

const DEFAULT_BASE_URL = "http://localhost:8080";

async function loadOptions() {
  const cfg = await chrome.storage.sync.get({ baseUrl: DEFAULT_BASE_URL, sessionId: "" });
  document.getElementById("base-url").value = cfg.baseUrl;
  document.getElementById("session-id").value = cfg.sessionId;
}

// requestOriginPermission asks Chrome to grant the optional host
// permission (declared in manifest.json's optional_host_permissions) for
// baseUrl's origin, so background.js/popup.js's fetches to it run with
// host permission — which is what makes a REAL extension's request exempt
// from CORS enforcement entirely (see README.md's CORS/extension
// distinction: this is unrelated to, and doesn't need, any change on the
// server). Must be called synchronously from within a user-gesture event
// handler (the form's submit handler below) — chrome.permissions.request
// rejects otherwise. No-ops harmlessly under the shim (chrome.permissions
// doesn't exist there) or a malformed URL.
async function requestOriginPermission(baseUrl) {
  if (!(window.chrome && chrome.permissions && chrome.permissions.request)) return;
  try {
    const origin = new URL(baseUrl).origin + "/*";
    await chrome.permissions.request({ origins: [origin] });
  } catch (err) {
    console.warn("jlp: host permission request failed", err);
  }
}

async function saveOptions(e) {
  e.preventDefault();
  const baseUrl = document.getElementById("base-url").value.trim().replace(/\/+$/, "");
  const sessionId = document.getElementById("session-id").value.trim();
  const statusEl = document.getElementById("options-status");

  await requestOriginPermission(baseUrl);
  await chrome.storage.sync.set({ baseUrl, sessionId });
  statusEl.textContent = "保存しました。";
}

document.addEventListener("DOMContentLoaded", () => {
  loadOptions();
  document.getElementById("options-form").addEventListener("submit", saveOptions);
});
