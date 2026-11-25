// Browser checks of the live compose stack with runtime web modules (PRD #4686, plan m5).
//
//   node scripts/modules-compose-check.mjs baseline <base-url>
//   node scripts/modules-compose-check.mjs enabled  <base-url>
//
// Run by scripts/web-modules-compose-acceptance.sh inside the pinned Playwright container
// (`docker exec`), against the throwaway compose stack. Credentials come from the environment
// (MC_EMAIL, MC_PASSWORD: the dummy seed admin of that stack).
//
//   baseline  the stack was started with an empty module list. Signs in, opens the dashboard as
//             a fresh document and asserts: no module entry in the sidebar, exactly one
//             /modules.json request in that document, and no /modules/ request at all.
//   enabled   the stack was started with the `example` module. Asserts: the sidebar entry and
//             the command-menu entry; /m/example renders the module with `sdk.theme.mode` text
//             and the host theme (the module's surface follows the host's --surface variable
//             across a live theme switch); the module's own link and the "back" link move
//             through the host router without a document reload; the remote entry was served
//             200 by the web nginx.
//   Both modes also assert zero CSP violations (a securitypolicyviolation listener is installed
//   before any page script runs, and a deliberate violation first proves it reports).
//
// Exit codes: 0 every check passed, 1 a check failed, 2 the instrument is broken (bad
// arguments, the sign-in form never appeared, the browser could not start). A wait that
// times out on something the stack should show counts as a failed check (exit 1).

import { chromium } from "playwright";
import { chromiumLaunchOptions } from "./browser-launch.mjs";

const [mode, base] = process.argv.slice(2);
const email = process.env.MC_EMAIL;
const password = process.env.MC_PASSWORD;

class Broken extends Error {}
class Failed extends Error {}

function check(cond, msg) {
  if (!cond) throw new Failed(msg);
}

function usage() {
  throw new Broken("usage: modules-compose-check.mjs baseline|enabled <base-url> (MC_EMAIL and MC_PASSWORD in the environment)");
}

async function main() {
  if ((mode !== "baseline" && mode !== "enabled") || !base || !email || !password) usage();
  const browser = await chromium.launch(chromiumLaunchOptions()).catch((e) => {
    throw new Broken(`the browser could not start: ${e.message}`);
  });
  const ctx = await browser.newContext();
  // Violations are reported to this process, so they survive full document navigations.
  const violations = [];
  await ctx.exposeFunction("__cspReport", (v) => {
    violations.push(v);
  });
  await ctx.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (e) => {
      window.__cspReport?.(`${e.violatedDirective} ${e.blockedURI}`);
    });
  });
  const page = await ctx.newPage();
  const requests = [];
  page.on("request", (r) => requests.push(new URL(r.url()).pathname));
  const responses = new Map();
  page.on("response", (r) => responses.set(new URL(r.url()).pathname, r.status()));

  try {
    // CSP control: a deliberate violation must be reported, or the zero-violation result below
    // would prove nothing (the listener could be dead). connect-src 'self' blocks this fetch.
    await page.goto(`${base}/login`);
    await page.evaluate(() => {
      fetch("http://127.0.0.2:1/x").catch(() => {});
    });
    for (let i = 0; i < 50 && violations.length === 0; i++) await page.waitForTimeout(100);
    check(violations.length >= 1, "CSP control: a deliberate cross-origin fetch produced no securitypolicyviolation event, so the listener proves nothing");
    console.log(`OK    CSP control: a deliberate violation was reported (${violations[0]})`);
    violations.length = 0;

    // Sign in as the seeded admin.
    await page.goto(`${base}/login`);
    const emailInput = page.locator("input[type=email]").first();
    await emailInput.waitFor({ state: "visible", timeout: 30000 }).catch(() => {
      throw new Broken("the sign-in form never appeared");
    });
    await emailInput.fill(email);
    await page.locator("input[type=password]").first().fill(password);
    await page.getByRole("button", { name: "Log in" }).click();
    await page.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 30000 });

    // A fresh document, so the counters below describe exactly one page load.
    requests.length = 0;
    await page.goto(`${base}/dashboard`);
    await page.locator("nav").first().waitFor({ state: "visible", timeout: 30000 });
    await page.waitForLoadState("networkidle");
    const manifestRequests = requests.filter((p) => p === "/modules.json").length;
    const moduleRequests = requests.filter((p) => p.startsWith("/modules/"));
    check(manifestRequests === 1, `expected exactly one /modules.json request in the document, saw ${manifestRequests}`);
    console.log(`OK    exactly one /modules.json request in the signed-in document`);

    if (mode === "baseline") {
      check(moduleRequests.length === 0, `baseline made /modules/ requests: ${moduleRequests.join(", ")}`);
      check((await page.locator('nav a[href^="/m/"]').count()) === 0, "baseline shows a module entry in the sidebar");
      console.log("OK    no module sidebar entry and no /modules/ request");
    } else {
      const entry = page.locator('nav a[href="/m/example"]');
      await entry.first().waitFor({ state: "visible", timeout: 15000 });
      console.log("OK    sidebar entry for the example module");
      await page.keyboard.press("Control+K");
      // The option's name also carries its hint, so match the exact label span inside the option.
      await page
        .getByRole("option")
        .filter({ has: page.locator("span.truncate", { hasText: /^Example$/ }) })
        .first()
        .waitFor({ state: "visible", timeout: 10000 });
      await page.keyboard.press("Escape");
      console.log("OK    command-menu entry for the example module");

      await entry.first().click();
      await page.locator(".uzi-mod-example").first().waitFor({ state: "visible", timeout: 30000 });
      const mode0 = await page.locator("[data-testid=example-theme]").first().innerText();
      check(/^theme: (light|dark)$/.test(mode0.trim()), `unexpected sdk.theme.mode text: ${mode0}`);
      const css = () =>
        page.evaluate(() => {
          const el = document.querySelector(".uzi-mod-example");
          return {
            border: getComputedStyle(el).borderLeftWidth,
            bg: getComputedStyle(el).backgroundColor,
            surface: getComputedStyle(document.documentElement).getPropertyValue("--surface").trim(),
          };
        });
      const before = await css();
      check(before.border === "4px", `the module stylesheet was not applied: ${JSON.stringify(before)}`);
      const want = (m) => new RegExp(`theme: ${m}`);
      for (const [theme, modeName] of [
        ["dawn", "light"],
        ["ember", "dark"],
      ]) {
        await page.evaluate((t) => {
          document.documentElement.dataset.theme = t;
        }, theme);
        await page.waitForFunction(
          (re) => new RegExp(re).test(document.querySelector("[data-testid=example-theme]")?.textContent ?? ""),
          want(modeName).source,
          { timeout: 10000 },
        );
      }
      // dawn then ember again: the module's surface moved with the host's --surface.
      await page.evaluate(() => {
        document.documentElement.dataset.theme = "dawn";
      });
      await page.waitForFunction(() => /theme: light/.test(document.querySelector("[data-testid=example-theme]")?.textContent ?? ""));
      const light = await css();
      await page.evaluate(() => {
        document.documentElement.dataset.theme = "ember";
      });
      await page.waitForFunction(() => /theme: dark/.test(document.querySelector("[data-testid=example-theme]")?.textContent ?? ""));
      const dark = await css();
      check(light.surface !== dark.surface && light.bg !== dark.bg, `the module did not follow the host theme: ${JSON.stringify({ light, dark })}`);
      console.log("OK    /m/example renders with sdk.theme.mode text and follows the host theme");

      await page.evaluate(() => {
        window.__noReload = performance.timeOrigin;
      });
      await page.getByRole("link", { name: "Sub view" }).click();
      await page.waitForFunction(() => /Sub view, build /.test(document.querySelector("[data-testid=example-subview]")?.textContent ?? ""), null, { timeout: 15000 });
      check(new URL(page.url()).pathname === "/m/example/sub", `unexpected url after the module link: ${page.url()}`);
      await page.goBack();
      await page.locator("[data-testid=example-title]").first().waitFor({ state: "visible", timeout: 15000 });
      await page.getByRole("link", { name: "Back to the dashboard" }).click();
      await page.waitForURL((u) => u.pathname === "/dashboard", { timeout: 15000 });
      check((await page.evaluate(() => window.__noReload === performance.timeOrigin)) === true, "the document reloaded during host-router navigation");
      console.log("OK    module link and the way back navigate the host router without a reload");

      check(responses.get("/modules/example/remoteEntry.js") === 200, `remoteEntry.js status: ${responses.get("/modules/example/remoteEntry.js")}`);
      console.log("OK    /modules/example/remoteEntry.js served 200 through the web nginx");
    }

    check(violations.length === 0, `CSP violations: ${JSON.stringify(violations.slice(0, 3))}`);
    console.log("OK    zero CSP violations");
  } finally {
    await browser.close().catch(() => {});
  }
}

main().then(
  () => {
    console.log(`PASS  modules-compose-check ${mode}`);
  },
  (e) => {
    if (e instanceof Broken) {
      console.error(`BROKEN  ${e.message}`);
      process.exitCode = 2;
    } else {
      // A failed assertion, or a wait that timed out on something the stack should show.
      console.error(`FAIL  ${e instanceof Failed ? e.message : (e?.message ?? e)}`);
      process.exitCode = 1;
    }
  },
);
