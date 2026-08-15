// JLP design system — theme toggle.
//
// Resolution order for the *current* theme (mirrors the pre-paint
// inline script in layout.html.tmpl, which already set data-theme on
// <html> before this file loads, so there's no flash of unstyled/
// wrong-theme content to fix here): localStorage("jlp-theme") ->
// prefers-color-scheme -> light.
//
// window.jlp.toggleTheme() flips data-theme and persists the explicit
// choice to localStorage; the topbar's <button> calls it and updates
// its own aria-pressed/label.
(function () {
  "use strict";

  var STORAGE_KEY = "jlp-theme";

  function currentTheme() {
    return document.documentElement.getAttribute("data-theme") || "light";
  }

  function applyTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
  }

  function toggleTheme() {
    var next = currentTheme() === "dark" ? "light" : "dark";
    applyTheme(next);
    try {
      window.localStorage.setItem(STORAGE_KEY, next);
    } catch (e) {
      // localStorage unavailable (private mode / disabled) — the
      // toggle still works for this page load, it just won't persist.
    }
    return next;
  }

  window.jlp = window.jlp || {};
  window.jlp.toggleTheme = toggleTheme;
  window.jlp.currentTheme = currentTheme;

  // Wire up the topbar toggle button: keyboard-reachable <button>,
  // aria-pressed reflects state (both on load and after each toggle).
  function syncButton(btn) {
    var pressed = currentTheme() === "dark";
    btn.setAttribute("aria-pressed", String(pressed));
  }

  document.addEventListener("DOMContentLoaded", function () {
    var buttons = document.querySelectorAll("[data-theme-toggle]");
    buttons.forEach(function (btn) {
      syncButton(btn);
      btn.addEventListener("click", function () {
        toggleTheme();
        syncButton(btn);
      });
    });
  });
})();
