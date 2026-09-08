// Pre-paint appearance stamp (PRD #1167 "Lights on"). Runs before the app bundle
// and before first paint, applying the last resolved appearance from the cache so
// there is no flash of the wrong theme/typeface. The server-resolved value wins
// once me() returns (web/src/lib/theme.ts applyAppearance). Shipped as an EXTERNAL
// same-origin classic script, loaded blocking in <head>, so the CSP stays
// script-src 'self' with NO 'unsafe-inline' — an inline <script> would be blocked
// by the nginx CSP and the pre-paint would silently never run (see web/nginx.conf).
//
// Dependency-free. Reads the "uzi.appearance" localStorage key written by
// applyAppearance: a JSON object { mode, light, dark, typeface }. The LITERAL
// polarity map below (LIGHT/DARK) must track THEME_POLARITY in web/src/lib/theme.ts
// — a new theme adds one entry here and one there. It stamps BOTH data-theme (the
// painted polarity's theme id) and data-font (the typeface), and NEVER leaves
// data-theme unset: with no cache, "system" mode follows the OS (hall on OS-light,
// ember on OS-dark). All storage/matchMedia access is guarded so a private-mode or
// locked-storage browser still paints the compiled fallback.
(function () {
  var LIGHT = { dawn: 1, hall: 1, shadow: 1 };
  var DARK = { ember: 1, mission: 1 };
  var el = document.documentElement;

  // Own-property membership only: a cached (or tampered) light/dark id of
  // "__proto__" / "constructor" / "toString" must NOT read as a valid theme via
  // the object's prototype chain, which would stamp an unknown data-theme. Guards
  // every LIGHT/DARK lookup below.
  function known(map, id) {
    return typeof id === "string" && Object.prototype.hasOwnProperty.call(map, id);
  }

  // paint stamps data-theme/data-font for a resolved appearance. matchMedia is
  // guarded here so a "system" mode without matchMedia falls to the light polarity
  // rather than throwing (leaving data-theme unset).
  function paint(mode, light, dark, typeface) {
    var osDark = false;
    if (mode === "system") {
      try {
        osDark = !!(
          window.matchMedia &&
          window.matchMedia("(prefers-color-scheme: dark)").matches
        );
      } catch (e) {
        osDark = false;
      }
    }
    var polarityDark = mode === "dark" || (mode === "system" && osDark);
    el.setAttribute("data-theme", polarityDark ? dark : light);
    el.setAttribute("data-font", typeface);
  }

  try {
    // Pre-paint defaults: neutral "system" mode (there is no server-resolved
    // default this early), hall/ember for the polarity pair, system typeface.
    var mode = "system";
    var light = "hall";
    var dark = "ember";
    var typeface = "system";

    var raw = localStorage.getItem("uzi.appearance");
    if (raw) {
      var a = JSON.parse(raw);
      if (a && typeof a === "object") {
        if (a.mode === "system" || a.mode === "light" || a.mode === "dark") mode = a.mode;
        if (known(LIGHT, a.light)) light = a.light;
        if (known(DARK, a.dark)) dark = a.dark;
        if (a.typeface === "system" || a.typeface === "plex") typeface = a.typeface;
      }
    } else {
      // No uzi.appearance yet: migrate a stale pre-m5 "uzi.theme" cache. A dark id
      // becomes the dark slot pinned by mode="dark"; anything else keeps defaults.
      var stale = localStorage.getItem("uzi.theme");
      if (known(DARK, stale)) {
        dark = stale;
        mode = "dark";
      }
    }

    paint(mode, light, dark, typeface);
  } catch (e) {
    // Storage / JSON failure: stamp the compiled fallback (system → matchMedia →
    // hall/ember). paint guards matchMedia itself, so if that ALSO throws the
    // nested catch stamps the safe light default (light/hall).
    try {
      paint("system", "hall", "ember", "system");
    } catch (e2) {
      el.setAttribute("data-theme", "hall");
      el.setAttribute("data-font", "system");
    }
  }
})();
