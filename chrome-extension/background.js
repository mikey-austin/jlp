// background.js — MV3 service worker. Owns exactly two things: the two
// context-menu items, and turning a click on either into a stashed
// selection (chrome.storage.session) plus a popup.html window. Every
// actual API call — resolving which session/document to use, POSTing
// feedback, POSTing a vocabulary event — happens in popup.js, not here:
// popup.js has to be able to do all of that on its own anyway (the
// documented browser-verification protocol drives popup.js directly,
// without a service worker in the loop — see README.md), so duplicating
// session/document resolution here would just be a second copy of logic
// that can drift from the one popup.js actually runs.
const MENU_FEEDBACK = "jlp-feedback";
const MENU_VOCAB = "jlp-vocab";

chrome.runtime.onInstalled.addListener(() => {
  // removeAll first: contextMenus items persist across service-worker
  // restarts (they're tied to the extension install, not the worker's
  // lifetime), so a bare create() on an already-installed extension would
  // throw "duplicate id".
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: MENU_FEEDBACK, title: "JLPで添削", contexts: ["selection"] });
    chrome.contextMenus.create({ id: MENU_VOCAB, title: "JLPに語彙を保存", contexts: ["selection"] });
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId !== MENU_FEEDBACK && info.menuItemId !== MENU_VOCAB) return;
  if (!tab || tab.id == null) return;

  let text = info.selectionText || "";
  let title = tab.title || "";
  try {
    // See content.js's doc comment for why this (not info.selectionText)
    // is the primary source — this can still fail (e.g. a chrome:// page
    // scripting can't reach), in which case the selectionText/tab.title
    // fallback above stands.
    const [{ result } = {}] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      files: ["content.js"],
    });
    if (result && result.text) {
      text = result.text;
      title = result.title || title;
    }
  } catch (err) {
    console.warn("jlp: content script injection failed, falling back to selectionText", err);
  }
  if (!text) return;

  const mode = info.menuItemId === MENU_FEEDBACK ? "feedback" : "vocab";
  await chrome.storage.session.set({ selection: text, title, mode });
  await chrome.windows.create({
    url: chrome.runtime.getURL(`popup.html?mode=${encodeURIComponent(mode)}`),
    type: "popup",
    width: 420,
    height: 620,
  });
});
