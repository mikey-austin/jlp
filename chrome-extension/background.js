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
const MENU_SESSION = "jlp-session";
const MENU_CHAT = "jlp-chat";
const MENU_READING = "jlp-reading";

// The three that go through the popup. Chat is not one of them: it opens
// a tab straight at the deployed chat and needs no UI of ours.
const POPUP_MENUS = { [MENU_FEEDBACK]: "feedback", [MENU_VOCAB]: "vocab", [MENU_SESSION]: "session" };

chrome.runtime.onInstalled.addListener(() => {
  // removeAll first: contextMenus items persist across service-worker
  // restarts (they're tied to the extension install, not the worker's
  // lifetime), so a bare create() on an already-installed extension would
  // throw "duplicate id".
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: MENU_FEEDBACK, title: "JLPで添削", contexts: ["selection"] });
    chrome.contextMenus.create({ id: MENU_SESSION, title: "JLPで新しいセッション", contexts: ["selection"] });
    chrome.contextMenus.create({ id: MENU_VOCAB, title: "JLPに語彙を保存", contexts: ["selection"] });
    // The one item offered on the PAGE as well as on a selection: a
    // Kindle edition is usually of the whole article. With a selection,
    // the selection is what gets studied.
    chrome.contextMenus.create({ id: MENU_READING, title: "JLPでKindle版を作成", contexts: ["page", "selection"] });
    // Only offered when a chat URL is configured. An item that cannot
    // work is worse than an absent one — the same rule the app's own nav
    // follows for its chat link.
    chrome.storage.sync.get({ chatUrl: "" }).then(({ chatUrl }) => {
      if (chatUrl) {
        chrome.contextMenus.create({ id: MENU_CHAT, title: "JLPでチャット", contexts: ["selection"] });
      }
    });
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (!tab || tab.id == null) return;

  if (info.menuItemId === MENU_READING) {
    // article.js reads the article body and metadata out of the tab the
    // learner is looking at (see its doc comment); the popup submits it.
    // Stashed like a selection, and consumed one-shot the same way.
    let article = null;
    try {
      const [{ result } = {}] = await chrome.scripting.executeScript({
        target: { tabId: tab.id },
        files: ["article.js"],
      });
      article = result || null;
    } catch (err) {
      console.warn("jlp: article capture failed, falling back to the selection", err);
    }
    if (!article) {
      article = { url: tab.url || "", title: tab.title || "", selection: info.selectionText || "", content: "" };
    }
    await chrome.storage.session.set({ article, mode: "reading" });
    await chrome.windows.create({
      url: chrome.runtime.getURL("popup.html?mode=reading"),
      type: "popup",
      width: 420,
      height: 620,
    });
    return;
  }

  if (!POPUP_MENUS[info.menuItemId] && info.menuItemId !== MENU_CHAT) return;

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

  if (info.menuItemId === MENU_CHAT) {
    // Pre-fills the deployed chat's composer; it deliberately does not
    // send. See clients/a2a-chat/public/app.js's prefillFromQuery.
    const { chatUrl } = await chrome.storage.sync.get({ chatUrl: "" });
    if (!chatUrl) return;
    await chrome.tabs.create({ url: `${chatUrl.replace(/\/$/, "")}/?q=${encodeURIComponent(text)}` });
    return;
  }

  const mode = POPUP_MENUS[info.menuItemId];
  await chrome.storage.session.set({ selection: text, title, mode });
  await chrome.windows.create({
    url: chrome.runtime.getURL(`popup.html?mode=${encodeURIComponent(mode)}`),
    type: "popup",
    width: 420,
    height: 620,
  });
});
