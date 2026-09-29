// options.js — stores {baseUrl, sessionId, chatUrl, autoDeliver} in chrome.storage.sync
// (chrome.storage.sync, not .local: small, textual, and worth carrying
// across a signed-in Chrome profile's devices, same rationale as any
// other extension's settings page). No API calls happen here; popup.js
// is the only file that talks to the JLP API.

const DEFAULT_BASE_URL = "http://localhost:8080";

async function loadOptions() {
  const cfg = await chrome.storage.sync.get({ baseUrl: DEFAULT_BASE_URL, sessionId: "", chatUrl: "", autoDeliver: false });
  // The token is read from storage.local, where saveOptions puts it —
  // see there for why it does not live in sync with the rest.
  const { apiToken } = await chrome.storage.local.get({ apiToken: "" });
  document.getElementById("base-url").value = cfg.baseUrl;
  document.getElementById("session-id").value = cfg.sessionId;
  document.getElementById("chat-url").value = cfg.chatUrl;
  document.getElementById("auto-deliver").checked = !!cfg.autoDeliver;
  document.getElementById("api-token").value = apiToken;
  updateTokensLink(cfg.baseUrl);
}

// updateTokensLink points the "mint one here" link at whichever JLP this
// extension is configured against, so it works for a dev instance and
// the deployed one without editing anything.
function updateTokensLink(baseUrl) {
  const link = document.getElementById("tokens-link");
  if (link) link.href = `${String(baseUrl).replace(/\/+$/, "")}/settings/tokens`;
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
  const chatUrl = document.getElementById("chat-url").value.trim().replace(/\/+$/, "");
  const apiToken = document.getElementById("api-token").value.trim();
  const autoDeliver = document.getElementById("auto-deliver").checked;
  const statusEl = document.getElementById("options-status");

  await requestOriginPermission(baseUrl);
  await chrome.storage.sync.set({ baseUrl, sessionId, chatUrl, autoDeliver });
  // The token goes to storage.local, NOT sync. sync replicates to every
  // device signed into this Chrome profile, which is a reasonable place
  // for a base URL and an unreasonable one for a credential — and
  // keeping both in one store would hide that difference.
  await chrome.storage.local.set({ apiToken });
  updateTokensLink(baseUrl);
  statusEl.textContent = apiToken
    ? "保存しました。"
    : "保存しました（APIトークンが未設定のため、JLPへの操作は失敗します）。";
}

document.addEventListener("DOMContentLoaded", () => {
  loadOptions();
  document.getElementById("options-form").addEventListener("submit", saveOptions);
});
