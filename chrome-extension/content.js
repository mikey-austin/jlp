// content.js — selection capture. Not a manifest content_script (it isn't
// injected into every page on load); background.js injects it on demand,
// via chrome.scripting.executeScript, only when a context-menu item fires
// on a text selection (the context-menu click is the user gesture
// "activeTab" needs to authorize the injection).
//
// window.getSelection().toString() is deliberately used instead of the
// contextMenus API's own info.selectionText: selectionText collapses
// internal whitespace/newlines, which would throw off the rune-offset
// math (start=0, end=Array.from(text).length) popup.js sends the
// feedback endpoint — see app.js's window.jlp.runeOffset for the same
// UTF-16-vs-rune trap this avoids on the main app side.
//
// chrome.scripting.executeScript's result for a files-based injection is
// the completion value of the injected script — the last expression
// evaluated — so this file is intentionally just one expression, not a
// function declaration.
(function () {
  return {
    text: window.getSelection().toString(),
    title: document.title,
  };
})();
