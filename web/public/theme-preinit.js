// Pre-paint theme stamp (PRD #21): runs before the app bundle and before first
// paint, applying the last resolved theme from the cache so there is no flash of
// the wrong theme. The server-resolved value wins once me() returns
// (web/src/lib/theme.ts). Shipped as an EXTERNAL same-origin classic script,
// loaded blocking in <head>, so the CSP stays script-src 'self' with NO
// 'unsafe-inline' — an inline <script> would be blocked by the nginx CSP and the
// pre-paint would silently never run. Dependency-free and kept in sync with that
// module's THEMES list and "uzi.theme" storage key. No cached value (or blocked
// storage) leaves data-theme unset, which renders ember.
(function () {
  try {
    var t = localStorage.getItem("uzi.theme");
    if (t === "ember" || t === "mission") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    /* storage unavailable: fall through to the default (ember) */
  }
})();
