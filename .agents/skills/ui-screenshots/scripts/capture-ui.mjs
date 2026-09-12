// capture-ui.mjs — capture clean uzi web UI screenshots in BOTH light (Dawn) and
// dark (Ember) themes, straight from a logged-in Chrome over CDP. Reads shots.json
// (the manifest) to know what to shoot, at which viewport, and where it lands.
//
// Captures the app VIEWPORT only (no browser chrome, no crop) at deviceScaleFactor
// 2 (desktop) / 3 (mobile), so light and dark both come out crisp with no
// luminance cropping. One engine does it all over CDP: ensures demo mode on (masks
// identifying data), flips instance branding to vanilla (built-in uzi mark, no
// "powered by") then restores it, forces each theme via PUT /me/settings, scans the
// page for leaked sensitive strings, then screenshots into a staging dir for review.
// publish-shots.mjs copies the reviewed pairs to their manifest targets.
//
// The instance URL is always an argument (or read from `uzi auth status`), never
// hardcoded — this repo is public.
//
// Usage (normally via run.sh, which provisions playwright-core and resolves --url):
//   node capture-ui.mjs --url <url> --manifest <shots.json> --stage <dir> [opts]
//     --only <slugs>   comma list of slugs (default: every auto shot, plus any
//                      repo-board/run-detail whose id is supplied)
//     --repo <id>      repo id for the board shot (/repos/<id>/board)
//     --run <id>       run id for run-detail shots (/runs/<id>)
//     --themes <list>  which themes to shoot (default light,dark)
//     --deny <list>    extra comma-separated leak-scan substrings
//     --port <n>       CDP port (default 9222)
//     --no-branding    skip the branding vanilla-flip/restore
//     --keep-open      leave the CDP connection open at the end

import { writeFileSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { createRequire } from "node:module";

// playwright-core is provisioned by run.sh into a cache dir (kept OUT of the skill
// dir so it stays small); load it from there rather than the skill's own tree.
const deps = process.env.UZI_SHOTS_DEPS;
if (!deps) { console.error("UZI_SHOTS_DEPS unset — run via run.sh (it provisions playwright-core)."); process.exit(4); }
const { chromium } = createRequire(join(deps, "noop.cjs"))("playwright-core");

const args = process.argv.slice(2);
const opt = (n, d) => { const i = args.indexOf(`--${n}`); return i === -1 ? d : args[i + 1]; };
const has = (n) => args.includes(`--${n}`);

const URL = opt("url");
const MANIFEST = opt("manifest");
const STAGE = opt("stage");
if (!URL || !MANIFEST || !STAGE) {
  console.error("usage: node capture-ui.mjs --url <url> --manifest <shots.json> --stage <dir> [opts]");
  process.exit(2);
}
const PORT = opt("port", "9222");
const REPO = opt("repo");
const RUN = opt("run");
const ONLY = opt("only") ? opt("only").split(",").map((s) => s.trim()) : null;
const DO_BRANDING = !has("no-branding");
const FORCE_THIN = has("force-thin"); // capture even when the downgrade guard says a shot is thin
mkdirSync(STAGE, { recursive: true });

const M = JSON.parse(readFileSync(MANIFEST, "utf8"));
const THEMES = (opt("themes") ? opt("themes").split(",") : Object.keys(M.themes)).map((t) => t.trim());
const host = new global.URL(URL).host; // e.g. uzi.example.com
const DENY = [...(M.denylist || []), host, ...(opt("deny") ? opt("deny").split(",") : [])]
  .map((s) => s.trim().toLowerCase()).filter(Boolean);

// ── page-context helpers (same-origin fetch with the app's CSRF convention) ────
const API = `
  window.__csrf = () => { const m = document.cookie.match(/(?:^|; )uzi_csrf=([^;]*)/); return m ? decodeURIComponent(m[1]) : ""; };
  window.__get = async (p) => { const r = await fetch('/api'+p, { credentials:'same-origin' }); if(!r.ok) throw new Error('GET '+p+' -> '+r.status); return r.json(); };
  window.__put = async (p, body) => { const r = await fetch('/api'+p, { method:'PUT', credentials:'same-origin', headers:{'Content-Type':'application/json','X-CSRF-Token':window.__csrf()}, body: JSON.stringify(body) }); if(!r.ok) throw new Error('PUT '+p+' -> '+r.status+' '+(await r.text())); return r.json(); };
`;

const browser = await chromium.connectOverCDP(`http://localhost:${PORT}`);
const ctx = browser.contexts()[0];
if (!ctx) { console.error("no CDP context — is the app-mode Chrome up (launch-chrome.sh)?"); process.exit(4); }
const page = ctx.pages()[0] || (await ctx.newPage());
const cdp = await ctx.newCDPSession(page);
const base = URL.replace(/\/$/, "");

await page.addInitScript(API);
await page.goto(base + "/dashboard", { waitUntil: "domcontentloaded" });
if (/\/(login|auth|realms)\b/.test(page.url()) || /keycloak/.test(page.url())) {
  console.error(`Not logged in (at ${page.url()}). Log in once in the Chrome window, then re-run.`);
  process.exit(3);
}
await page.evaluate(API);

// 1) demo mode ON (per-device, persists in this profile).
const demoWas = await page.evaluate(() => { try { return localStorage.getItem("uzi_demo_mode"); } catch { return null; } });
if (demoWas !== "1") { await page.evaluate(() => { try { localStorage.setItem("uzi_demo_mode", "1"); } catch {} }); console.log("demo mode: turned ON"); }
else console.log("demo mode: already on");

// 2) save appearance + branding to restore later; flip branding to vanilla.
const origAppearance = await page.evaluate(async () => (await window.__get("/me/settings")).settings);
let origBranding = null;
if (DO_BRANDING) {
  const s = await page.evaluate(async () => (await window.__get("/admin/settings")).settings).catch((e) => { console.error("branding read failed (not admin?): " + e.message); return null; });
  if (s) {
    origBranding = { app_logo_mode: s.app_logo_mode, app_logo_preset: s.app_logo_preset, app_logo_keep_name: s.app_logo_keep_name, brand_mode: s.brand_mode, brand_company: s.brand_company, brand_placement: s.brand_placement, brand_plaque: s.brand_plaque };
    const vanilla = { ...origBranding, app_logo_mode: "default", app_logo_preset: "", app_logo_keep_name: "true", brand_mode: "none" };
    await page.evaluate(async (v) => window.__put("/admin/settings", { settings: v }), vanilla);
    console.log("branding: flipped to vanilla (built-in uzi mark, no powered-by)");
  }
}

// run status (for the downgrade guard on run-detail shots).
let runStatusVal = null;
if (RUN) {
  runStatusVal = await page.evaluate(async (id) => { try { const j = await window.__get("/runs/" + id); return (j.run || j).status || null; } catch { return null; } }, RUN);
  console.log(`run ${RUN} status: ${runStatusVal}`);
}

async function applyTheme(label) {
  const mode = label === "light" ? "light" : "dark";
  const themeId = M.themes[label];
  const patch = mode === "light" ? { appearance_mode: "light", light_theme: themeId } : { appearance_mode: "dark", dark_theme: themeId };
  await page.evaluate(API);
  await page.evaluate(async (p) => window.__put("/me/settings", p), patch);
  await page.evaluate(({ mode, themes }) => {
    try {
      const a = JSON.parse(localStorage.getItem("uzi.appearance") || "{}");
      localStorage.setItem("uzi.appearance", JSON.stringify({ mode, light: themes.light, dark: themes.dark, typeface: a.typeface || "system" }));
    } catch {}
  }, { mode, themes: M.themes });
}

// Scan only the text that will actually be IN the shot (the captured element for a
// panel shot; the whole page for a viewport shot) so an off-panel link/path elsewhere
// on the run page is not a false positive.
function leakScan(slug, theme, text) {
  const t = (text || "").toLowerCase();
  const hits = DENY.filter((d) => t.includes(d));
  if (hits.length) console.log(`  ⚠ leak-scan ${slug}-${theme}: found ${JSON.stringify(hits)} in the captured content — REVIEW before publishing`);
  return hits;
}

const flags = [];
async function shoot(shot, theme) {
  const vp = M.viewports[shot.viewport];
  let route = shot.route || "";
  if (shot.needs === "repo") { if (!REPO) { console.log(`skip ${shot.slug}: needs --repo (${shot.state || ""})`); return; } route = route.replace("{repo}", REPO); }
  if (shot.needs === "run") { if (!RUN) { console.log(`skip ${shot.slug}: needs --run (state: ${shot.state || ""})`); return; } route = route.replace("{run}", RUN); }

  // downgrade guard 1: run must be in an expected state, else the shot shows less than the current image.
  // Fail closed: skip when the status is unknown (lookup failed) or does not match,
  // so a null read never lets a possibly-wrong page through. FORCE_THIN still bypasses.
  if (shot.requireStatus && !FORCE_THIN && !(runStatusVal && shot.requireStatus.includes(runStatusVal))) {
    console.log(`  ⚠ SKIP ${shot.slug}-${theme}: run status '${runStatusVal ?? "unknown"}' not in [${shot.requireStatus.join(", ")}] — would be a downgrade, keep the current image`);
    flags.push(`${shot.slug}: wrong run state (${runStatusVal ?? "unknown"}); want ${shot.requireStatus.join("/")}`);
    return;
  }

  await cdp.send("Emulation.setDeviceMetricsOverride", { width: vp.width, height: vp.height, deviceScaleFactor: vp.dsf, mobile: vp.mobile });
  await page.goto(base + route, { waitUntil: "networkidle" });
  await page.waitForLoadState("networkidle").catch(() => {});
  await page.waitForTimeout(1600); // settle animations + streamed summaries (avoids raw-path flash)

  if (shot.action === "openNav") {
    await page.getByRole("button", { name: /menu|navigation|open/i }).first().click({ timeout: 4000 }).catch(() => {});
    await page.waitForTimeout(500);
  }

  // downgrade guard 2: DOM richness (e.g. board must have enough cards).
  if (shot.richness && !FORCE_THIN) {
    const n = await page.evaluate((sel) => document.querySelectorAll(sel).length, shot.richness.domSelector);
    if (n < shot.richness.min) {
      console.log(`  ⚠ SKIP ${shot.slug}-${theme}: ${shot.richness.label} ${n} < ${shot.richness.min} — would be a downgrade, keep the current image`);
      flags.push(`${shot.slug}: thin (${shot.richness.label} ${n} < ${shot.richness.min})`);
      return;
    }
  }

  const file = join(STAGE, `${shot.slug}-${theme}.png`);

  if (shot.panel) {
    // element capture of one run-detail panel (the nearest border-edge Card around the anchor text).
    if (shot.expand === "lead") {
      // Expand the lead's agent GROUP header (a button with aria-expanded) — NOT the
      // summary "jump" chip (which carries aria-label="Jump to lead …"). Assert it
      // actually opened; if not, fall back to the "Expand all" button so the shot is
      // never silently a collapsed near-duplicate of the activity feed.
      const expandAll = async () => { await page.getByRole("button", { name: /expand all/i }).first().click({ timeout: 3000 }).catch(() => {}); await page.waitForTimeout(700); };
      const leadHeader = page.locator("button[aria-expanded]").filter({ hasText: /lead/i }).first();
      if (await leadHeader.count()) {
        await leadHeader.click({ timeout: 4000 }).catch(() => {});
        await page.waitForTimeout(700);
        if ((await leadHeader.getAttribute("aria-expanded").catch(() => null)) !== "true") await expandAll();
      } else {
        await expandAll();
      }
    }
    // Anchor on the panel HEADING (so "Milestones" doesn't match the word inside the
    // Run summary paragraph); fall back to any text for non-heading anchors (e.g. the
    // cost card's "Per-phase breakdown" toggle, which is not a heading).
    const rx = new RegExp(shot.panel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i");
    let anchor = page.getByRole("heading", { name: rx }).first();
    if (!(await anchor.count())) anchor = page.getByText(shot.panel, { exact: false }).first();
    // Panel cards are rounded-xl with a semantic border: border-edge for normal Cards,
    // border-warn for the amber plan-approval / wait panels. Take the nearest such card.
    const card = anchor.locator('xpath=ancestor-or-self::div[contains(@class,"rounded-xl") and (contains(@class,"border-edge") or contains(@class,"border-warn"))][1]');
    await card.scrollIntoViewIfNeeded({ timeout: 4000 }).catch(() => {});
    await page.waitForTimeout(400);
    const cardText = await card.innerText().catch(() => "");
    const hits = leakScan(shot.slug, theme, cardText);
    const buf = await card.screenshot({ type: "png" }).catch(() => null);
    if (!buf) {
      console.log(`  ⚠ ${shot.slug}-${theme}: panel '${shot.panel}' not found — skip`);
      flags.push(`${shot.slug}: panel '${shot.panel}' not found`);
      return;
    }
    writeFileSync(file, buf);
    console.log(`  captured ${shot.slug}-${theme}.png (panel)${hits.length ? "  ⚠" : ""}`);
    if (hits.length) flags.push(`${shot.slug}-${theme}: ${hits.join(", ")}`);
    return;
  }

  const pageText = await page.evaluate(() => document.body.innerText || "");
  const hits = leakScan(shot.slug, theme, pageText);
  const shotData = await cdp.send("Page.captureScreenshot", { format: "png", clip: { x: 0, y: 0, width: vp.width, height: vp.height, scale: 1 }, captureBeyondViewport: false });
  writeFileSync(file, Buffer.from(shotData.data, "base64"));
  console.log(`  captured ${shot.slug}-${theme}.png${hits.length ? "  ⚠" : ""}`);
  if (hits.length) flags.push(`${shot.slug}-${theme}: ${hits.join(", ")}`);
}

// select shots: --only wins; else every auto shot + any repo-board/run-detail with an id supplied.
const wanted = M.shots.filter((s) => {
  if (s.kind === "terminal") return false; // uzi tui: captured by hand
  if (ONLY) return ONLY.includes(s.slug);
  if (s.kind === "auto") return true;
  if (s.needs === "repo") return !!REPO;
  if (s.needs === "run") return !!RUN;
  return false;
});

// The restore MUST run even if a capture throws (a networkidle timeout, a CDP error,
// a failed PUT), or the shared instance is left flipped to vanilla branding and the
// operator's theme is left changed. Hence try/finally around the whole capture loop.
try {
  for (const theme of THEMES) {
    await applyTheme(theme);
    for (const shot of wanted) await shoot(shot, theme);
  }
} finally {
  await page.evaluate(API).catch(() => {});
  await page.evaluate(async (s) => window.__put("/me/settings", { appearance_mode: s.appearance_mode, light_theme: s.light_theme, dark_theme: s.dark_theme, typeface: s.typeface }), origAppearance).then(() => console.log("appearance: restored")).catch((e) => console.error("appearance restore FAILED: " + e.message));
  if (origBranding) await page.evaluate(async (b) => window.__put("/admin/settings", { settings: b }), origBranding).then(() => console.log("branding: restored")).catch((e) => console.error("branding restore FAILED (instance may be left vanilla): " + e.message));
  await cdp.send("Emulation.clearDeviceMetricsOverride").catch(() => {});
}
if (!has("keep-open")) await browser.close();

console.log(`\nstaged in ${STAGE}. Review each PNG (Read it) for layout + leaks, then publish-shots.mjs.`);
if (flags.length) { console.log("LEAK-SCAN FLAGS (review these):"); for (const f of flags) console.log("  ⚠ " + f); }
else console.log("leak-scan: clean");
