// Built-mode browser harness for runtime web modules (PRD #4686, plan milestone m2).
//
//   npm run test:modules-browser            every scenario, then the interrupt self-test
//   node scripts/modules-browser-test.mjs --selftest-interrupt   only the self-test
//
// The vitest browser target runs components in a dev server. This harness builds the REAL
// host (vite build, in-process) and the example module's fixture variants, serves them from
// in-process HTTP servers that stand in for the web nginx and the module upstreams, and
// drives Chromium through the module scenarios of the PRD (examples ex.1-ex.5, failure
// isolation, the guards, one React, the CSP).
//
// Processes and cleanup. Builds and servers run in THIS process, so there is no build child.
// The only child is the browser: it is started with chromium.launchServer, which spawns it
// detached in its own process group (Playwright 1.64: `detached` on Linux), and the harness
// records that group id. shutdown() runs on normal exit, an uncaught error, SIGINT and
// SIGTERM, is idempotent, and always: (1) asks the browser to close, bounded to 3 s, (2) then
// SIGKILLs the whole group whether or not (1) worked, (3) destroys every tracked socket and
// (4) closes every server. Afterwards it asserts the group is gone (ESRCH, or only zombie
// members left: a container whose PID 1 does not reap leaves killed Chromium children as
// zombies that keep the group alive) and that no port it opened still accepts connections.
// Not claimed: catching a descendant that leaves the group (none of Chromium's processes do).
//
// Exit codes: 0 all scenarios passed, 1 a scenario or cleanup assertion failed, 2 the
// instrument is broken (a build failed, a server never became ready, the manifest shape
// changed, the browser could not start).
//
// Environment: MODULES_HARNESS_OUT (build output root; default a fresh dir under the OS temp
// dir, removed at the end), MODULES_HARNESS_BASE_DIST (optional dist of the base commit, for
// the report-only measurement comparison), UZI_WEB_BROWSER_CHROMIUM (see browser-launch.mjs).

import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import zlib from "node:zlib";
import { chromium } from "playwright";
import { build } from "vite";
import { createModuleConfig } from "../examples/ui-module/vite.config.ts";
import { chromiumLaunchOptions } from "./browser-launch.mjs";
import { federationOnlyFiles, ManifestShapeError, zeroModuleViolations } from "./modules-manifest-traversal.mjs";

const WEB_DIR = fileURLToPath(new URL("..", import.meta.url));
const SELF = fileURLToPath(import.meta.url);
const SCAN_SCRIPT = path.join(WEB_DIR, "scripts", "csp-build-scan.mjs");

// The enforced CSP of the web nginx (scripts/assert-web-nginx.sh `CSP=`). Every response of the
// harness front carries it, so a violation here is a violation under the real server.
const CSP =
  "default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; img-src 'self' data:; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'";

const argv = process.argv.slice(2);
const MODE = argv.includes("--selftest-child") ? "selftest-child" : argv.includes("--selftest-interrupt") ? "selftest" : "full";

class InstrumentError extends Error {}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(...a);

function withTimeout(promise, ms) {
  let timer;
  return Promise.race([
    promise.finally(() => clearTimeout(timer)),
    new Promise((resolve) => {
      timer = setTimeout(() => resolve("timeout"), ms);
    }),
  ]);
}

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

// ---------------------------------------------------------------------------------------
// Tracked servers and sockets (every port and socket the harness opens is accounted for).
// ---------------------------------------------------------------------------------------

const servers = new Set();
const sockets = new Set();
const ports = new Set();

async function listen(server) {
  servers.add(server);
  server.on("connection", (s) => {
    sockets.add(s);
    s.on("close", () => sockets.delete(s));
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const { port } = server.address();
  ports.add(port);
  return port;
}

function acceptsConnection(port) {
  return new Promise((resolve) => {
    const s = net.connect({ port, host: "127.0.0.1" });
    const done = (v) => {
      s.destroy();
      resolve(v);
    };
    s.setTimeout(500, () => done(false));
    s.once("connect", () => done(true));
    s.once("error", () => done(false));
  });
}

// ---------------------------------------------------------------------------------------
// Process group bookkeeping.
// ---------------------------------------------------------------------------------------

function groupMembers(pgid) {
  const r = spawnSync("ps", ["-o", "pid=,pgid=,stat=", "-e"], { encoding: "utf8" });
  if (r.status !== 0) throw new InstrumentError(`ps failed: ${r.stderr}`);
  return r.stdout
    .split("\n")
    .map((l) => l.trim().split(/\s+/))
    .filter((c) => c.length >= 3 && Number(c[1]) === pgid)
    .map((c) => ({ pid: Number(c[0]), stat: c[2] }));
}

/** "Gone" is ESRCH for the group, or every remaining member being an unreaped zombie. */
async function waitGroupGone(pgid, deadlineMs = 5000) {
  const end = Date.now() + deadlineMs;
  let last = "";
  for (;;) {
    try {
      process.kill(-pgid, 0);
    } catch (e) {
      if (e.code === "ESRCH") return null;
    }
    const alive = groupMembers(pgid).filter((m) => !m.stat.startsWith("Z"));
    if (alive.length === 0) return null;
    last = alive.map((m) => `${m.pid}(${m.stat})`).join(",");
    if (Date.now() > end) return `process group ${pgid} still has live members: ${last}`;
    await sleep(100);
  }
}

// ---------------------------------------------------------------------------------------
// Browser lifecycle and the idempotent shutdown.
// ---------------------------------------------------------------------------------------

let browserServer = null;
let browser = null;
let browserPgid = 0;
let shutdownPromise = null;

async function startBrowser() {
  try {
    browserServer = await chromium.launchServer({
      ...chromiumLaunchOptions(),
      headless: true,
      handleSIGINT: false,
      handleSIGTERM: false,
      handleSIGHUP: false,
    });
    browserPgid = browserServer.process().pid;
    // The browser must lead its own group (Playwright spawns it detached); a harness that
    // cannot prove that must not claim cleanup.
    const own = spawnSync("ps", ["-o", "pgid=", "-p", String(browserPgid)], { encoding: "utf8" }).stdout.trim();
    if (Number(own) !== browserPgid) throw new InstrumentError(`browser pid ${browserPgid} is not its own group leader (pgid ${own})`);
    browser = await chromium.connect(browserServer.wsEndpoint());
  } catch (e) {
    throw e instanceof InstrumentError ? e : new InstrumentError(`could not start the browser: ${e.message}`);
  }
}

function shutdown() {
  shutdownPromise ??= (async () => {
    // 1. graceful close, bounded. A connect()ed browser's close() only disconnects; the
    //    server's close() is what closes Chromium.
    await withTimeout(Promise.allSettled([browser?.close(), browserServer?.close()]), 3000);
    // 2. unconditionally, even if 1 failed or timed out.
    if (browserPgid) {
      try {
        process.kill(-browserPgid, "SIGKILL");
      } catch (e) {
        if (e.code !== "ESRCH") console.error(`shutdown: kill(-${browserPgid}) failed: ${e.message}`);
      }
    }
    // 3. sockets, 4. servers.
    for (const s of sockets) s.destroy();
    await withTimeout(
      Promise.allSettled([...servers].map((s) => new Promise((r) => s.close(() => r())))),
      3000,
    );
  })();
  return shutdownPromise;
}

/** Problems left after shutdown(): live group members, or a tracked port still accepting. */
async function cleanupProblems() {
  const problems = [];
  if (browserPgid) {
    const g = await waitGroupGone(browserPgid);
    if (g) problems.push(g);
  }
  for (const port of ports) {
    if (await acceptsConnection(port)) problems.push(`port ${port} still accepts connections`);
  }
  return problems;
}

function installSignalHandlers() {
  for (const [sig, code] of [
    ["SIGINT", 130],
    ["SIGTERM", 143],
  ]) {
    process.on(sig, () => {
      void shutdown().finally(() => process.exit(code));
    });
  }
  const crash = (e) => {
    console.error("harness: unhandled error", e);
    void shutdown().finally(() => process.exit(1));
  };
  process.on("uncaughtException", crash);
  process.on("unhandledRejection", crash);
}

// ---------------------------------------------------------------------------------------
// The interrupt self-test (child + wrapper). A stalled upstream socket is held open while the
// browser runs; the wrapper SIGTERMs the child and then, independently of the child's own
// shutdown, asserts the browser's group is gone and no listening port remains.
// ---------------------------------------------------------------------------------------

async function selftestChild() {
  installSignalHandlers();
  let stalled = 0;
  const server = http.createServer((req, res) => {
    if (req.url === "/stall") {
      stalled++; // never answered: the socket stays open
      return;
    }
    res.setHeader("content-type", req.url === "/s.js" ? "text/javascript" : "text/html");
    res.end(req.url === "/s.js" ? "fetch('/stall').catch(function(){})" : '<!doctype html><script src="/s.js"></script>');
  });
  const port = await listen(server);
  await startBrowser();
  const page = await (await browser.newContext()).newPage();
  await page.goto(`http://127.0.0.1:${port}/`);
  const end = Date.now() + 10000;
  while (stalled === 0 && Date.now() < end) await sleep(50);
  if (stalled === 0) throw new InstrumentError("the stalled request never reached the server");
  console.log(`READY pgid=${browserPgid} ports=${[...ports].join(",")}`);
  setInterval(() => {}, 1 << 30);
}

async function selftestInterrupt() {
  const child = spawn(process.execPath, [SELF, "--selftest-child"], { stdio: ["ignore", "pipe", "inherit"] });
  let out = "";
  const ready = new Promise((resolve) => {
    child.stdout.on("data", (d) => {
      out += d;
      const m = /READY pgid=(\d+) ports=([\d,]+)/.exec(out);
      if (m) resolve({ pgid: Number(m[1]), ports: m[2].split(",").map(Number) });
    });
    child.once("exit", () => resolve(null));
  });
  const exited = new Promise((resolve) => child.once("exit", (code, signal) => resolve({ code, signal })));
  const info = await withTimeout(ready, 30000);
  if (info === "timeout" || info === null) {
    child.kill("SIGKILL");
    throw new InstrumentError(`interrupt self-test child never became ready: ${out.trim()}`);
  }
  child.kill("SIGTERM");
  const result = await withTimeout(exited, 15000);
  const problems = [];
  if (result === "timeout") {
    child.kill("SIGKILL");
    problems.push("the child did not exit within 15 s of SIGTERM");
  }
  const g = await waitGroupGone(info.pgid);
  if (g) problems.push(g);
  for (const p of info.ports) {
    if (await acceptsConnection(p)) problems.push(`port ${p} still accepts connections after SIGTERM`);
  }
  if (problems.length > 0) throw new Error(`interrupt self-test: ${problems.join("; ")}`);
  return `child ${result.signal ?? `exit ${result.code}`}, group ${info.pgid} gone, ports ${info.ports.join(",")} closed`;
}

// ---------------------------------------------------------------------------------------
// Builds.
// ---------------------------------------------------------------------------------------

const MODULE_BUILDS = [
  { dir: "example-v1", id: "example", buildTag: "v1" },
  { dir: "example-v2", id: "example", buildTag: "v2" },
  { dir: "example2", id: "example2", buildTag: "v1" },
  { dir: "incompat", id: "incompat", buildTag: "v1" },
  { dir: "contract2", id: "contract2", buildTag: "v1" },
  { dir: "noexpose", id: "noexpose", buildTag: "v1" },
];

async function buildAll(outRoot) {
  const t0 = Date.now();
  const dirs = { host: path.join(outRoot, "host"), hostOff: path.join(outRoot, "host-off") };
  const hostConfig = path.join(WEB_DIR, "vite.config.ts");
  try {
    // The harness host: test hooks on, and a build manifest (set HERE only: the shipped
    // vite.config.ts has no build.manifest).
    await build({
      root: WEB_DIR,
      configFile: hostConfig,
      logLevel: "error",
      define: { __UZI_TEST_HOOKS__: "true" },
      build: { outDir: dirs.host, emptyOutDir: true, manifest: true },
    });
    // The shipped shape (hooks off, as vite.config.ts defines it): the measurement subject.
    await build({
      root: WEB_DIR,
      configFile: hostConfig,
      logLevel: "error",
      build: { outDir: dirs.hostOff, emptyOutDir: true },
    });
    for (const m of MODULE_BUILDS) {
      dirs[m.dir] = path.join(outRoot, m.dir);
      await build({ ...createModuleConfig({ id: m.id, buildTag: m.buildTag, outDir: dirs[m.dir] }), logLevel: "error" });
    }
  } catch (e) {
    throw new InstrumentError(`build failed: ${e.message}`);
  }
  log(`builds: host, host (hooks off) and ${MODULE_BUILDS.length} module variants in ${((Date.now() - t0) / 1000).toFixed(1)} s`);
  return dirs;
}

// ---------------------------------------------------------------------------------------
// The front server (stands in for the web nginx), the module upstreams and the API stub.
// ---------------------------------------------------------------------------------------

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
  ".woff2": "font/woff2",
  ".woff": "font/woff",
  ".map": "application/json",
  ".txt": "text/plain",
};
const COMPRESSIBLE = /\.(html|js|mjs|css|json|svg|txt)$/;

const SECURITY_HEADERS = {
  "Content-Security-Policy": CSP,
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "Referrer-Policy": "same-origin",
};

function userFor(kind) {
  const admin = kind === "admin";
  return {
    id: admin ? "u-admin" : "u-member",
    email: admin ? "admin@example.test" : "member@example.test",
    display_name: admin ? "Admin" : "Member",
    is_admin: admin,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: null,
    attribution_enabled: true,
    ephemeral_workers_enabled: false,
  };
}

function sessionBody(kind) {
  return {
    user: userFor(kind),
    uzi_label: "uzi",
    autopilot_label: "autopilot",
    appearance: {
      mode: "dark",
      light_theme: "dawn",
      dark_theme: "ember",
      typeface: "system",
      sidebar_motion: false,
      overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null, sidebar_motion: null },
      defaults: { mode: "dark", light_theme: "dawn", dark_theme: "ember", typeface: "system", sidebar_motion: false },
    },
    theme: "ember",
    theme_override: null,
    default_theme: "ember",
    vault: { unlocked: true, exists: true, mode: "unprotected", policy: "optional" },
    has_password: true,
  };
}

const BRANDING = {
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_present: false,
  app_logo_keep_name: false,
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: false,
  brand_logo_present: false,
};

// Scenario-controlled state of the whole fixture world.
const world = {
  hostDir: "",
  session: null, // null | "admin" | "member"
  modules: [], // the /modules.json body
  upstream: new Map(), // id -> { dir, mode: "up" | "stopped" | "stall", port }
  log: [], // { path, status }
  unknownApi: new Set(),
};

function resetWorld() {
  world.session = null;
  world.modules = [];
  for (const u of world.upstream.values()) u.mode = "up";
}

function sendJson(res, status, body, extra = {}) {
  const data = Buffer.from(JSON.stringify(body));
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", "Content-Length": data.length, ...extra });
  res.end(data);
}

const gzipCache = new Map();
function sendFile(req, res, file, cache) {
  let st;
  try {
    st = fs.statSync(file);
  } catch {
    return false;
  }
  if (!st.isFile()) return false;
  const ext = path.extname(file);
  const headers = { "Content-Type": MIME[ext] ?? "application/octet-stream", "Cache-Control": cache };
  let body = fs.readFileSync(file);
  if (COMPRESSIBLE.test(file) && /\bgzip\b/.test(req.headers["accept-encoding"] ?? "")) {
    let z = gzipCache.get(file);
    if (!z || z.mtimeMs !== st.mtimeMs) {
      z = { mtimeMs: st.mtimeMs, data: zlib.gzipSync(body) };
      gzipCache.set(file, z);
    }
    body = z.data;
    headers["Content-Encoding"] = "gzip";
  }
  headers["Content-Length"] = body.length;
  res.writeHead(200, headers);
  res.end(req.method === "HEAD" ? undefined : body);
  return true;
}

function safeJoin(root, rel) {
  const p = path.join(root, rel);
  return p.startsWith(root + path.sep) || p === root ? p : null;
}

const BARE_LISTS = new Set(["/api/me/schedules"]);
const EMPTY_COLLECTIONS = Object.fromEntries(
  ["tokens", "accounts", "users", "templates", "connections", "secrets", "runs", "repos", "workers", "items", "holds", "schedules", "findings", "agents", "skills", "settings", "checks", "rows", "entries", "labels", "needs_decision"].map((k) => [
    k,
    k === "settings" ? {} : [],
  ]),
);
EMPTY_COLLECTIONS.aggregate = { open_holds: 0, custody_hold_limit: 0, decision_needed: 0, blocked_runs: 0 };

const RUN_USAGE = { input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 0, cost_status: "" };
const RUN_OUTCOMES = {
  last_failed_at: null,
  last_failed_run_id: null,
  last_failed_origin: null,
  last_failed_user_id: null,
  completed_since_last_failure: null,
  finished: 0,
  completed: 0,
  cancelled: 0,
  plan_rejected: 0,
  failed: 0,
  needs_landing: 0,
  fail_origins: {},
};
const SELF_USAGE = {
  lifetime: RUN_USAGE,
  last_7_days: RUN_USAGE,
  run_count: 0,
  outcomes: { lifetime: RUN_OUTCOMES, last_7_days: RUN_OUTCOMES },
  lifetime_subscription_run_count: 0,
  lifetime_unreported_run_count: 0,
  last7_subscription_run_count: 0,
  last7_unreported_run_count: 0,
};

function handleApi(req, res, p) {
  if (p === "/api/auth/me") {
    if (!world.session) return sendJson(res, 401, { error: "unauthorized" });
    return sendJson(res, 200, sessionBody(world.session));
  }
  if (p === "/api/branding") return sendJson(res, 200, BRANDING);
  if (p === "/api/auth/config") return sendJson(res, 200, { registration_enabled: false, allowed_domains: [] });
  if (!world.session) return sendJson(res, 401, { error: "unauthorized" });
  if (p === "/api/admin/health") {
    return sendJson(res, 200, {
      blocking: false,
      status: "ok",
      checked_at: new Date().toISOString(),
      counts: { ok: 0, warn: 0, danger: 0, unknown: 0, na: 0 },
      snoozed_until: null,
      episode_id: null,
      checks: [],
    });
  }
  if (p === "/api/usage") return sendJson(res, 200, SELF_USAGE);
  if (p === "/api/admin/usage") return sendJson(res, 200, { factory: SELF_USAGE, users: [], earliest_run: null });
  world.unknownApi.add(`${req.method} ${p}`);
  // Benign data for everything else the shell asks at startup. Bare-array endpoints get [];
  // every other GET gets an object that carries an empty list under each wrapper key the
  // client reads, which is what a fresh, empty instance returns.
  if (req.method !== "GET") return sendJson(res, 200, {});
  if (BARE_LISTS.has(p)) return sendJson(res, 200, []);
  return sendJson(res, 200, EMPTY_COLLECTIONS);
}

function proxyToModule(req, res, id) {
  const up = world.upstream.get(id);
  const noStore = { "Cache-Control": "no-store", ...SECURITY_HEADERS };
  if (!up) {
    res.writeHead(404, { "Content-Type": "text/plain", ...noStore });
    return res.end("not found");
  }
  if (req.method !== "GET" && req.method !== "HEAD") {
    res.writeHead(405, { "Content-Type": "text/plain", ...noStore });
    return res.end("method not allowed");
  }
  if (up.mode === "stopped") {
    res.writeHead(502, { "Content-Type": "text/plain", ...noStore });
    return res.end("bad gateway");
  }
  if (up.mode === "stall") return; // hold the request open: the socket is tracked and destroyed at shutdown
  const out = http.request(
    { host: "127.0.0.1", port: up.port, method: req.method, path: req.url, headers: { host: "upstream", "accept-encoding": req.headers["accept-encoding"] ?? "" } },
    (r) => {
      const h = { ...r.headers, ...SECURITY_HEADERS, "Cache-Control": "no-store" };
      res.writeHead(r.statusCode ?? 502, h);
      r.pipe(res);
    },
  );
  out.on("error", () => {
    if (!res.headersSent) res.writeHead(502, { "Content-Type": "text/plain", ...noStore });
    res.end("bad gateway");
  });
  out.end();
}

function createFront() {
  return http.createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://front");
    const p = decodeURIComponent(url.pathname);
    const entry = { path: p, search: url.search, status: 0 };
    world.log.push(entry);
    const origWriteHead = res.writeHead.bind(res);
    res.writeHead = (status, ...rest) => {
      entry.status = status;
      return origWriteHead(status, ...rest);
    };
    for (const [k, v] of Object.entries(SECURITY_HEADERS)) res.setHeader(k, v);

    if (p.startsWith("/api/")) return handleApi(req, res, p);
    if (p === "/modules.json") return sendJson(res, 200, world.modules, { "Cache-Control": "no-cache" });
    if (p === "/__csp-control") {
      // A deliberate violation: an inline script. The listener must report it.
      res.writeHead(200, { "Content-Type": "text/html", "Cache-Control": "no-store" });
      return res.end("<!doctype html><title>csp control</title><script>window.__inlineRan = true</script><p>control</p>");
    }
    const m = /^\/modules\/([a-z][a-z0-9-]*)\//.exec(p);
    if (m) return proxyToModule(req, res, m[1]);
    if (p.startsWith("/modules/")) {
      res.writeHead(404, { "Content-Type": "text/plain", "Cache-Control": "no-store" });
      return res.end("not found");
    }
    // The host: a file when it exists, else (no extension) the SPA index, like try_files.
    const file = safeJoin(world.hostDir, p);
    const cache = p.startsWith("/assets/") ? "public, max-age=31536000, immutable" : "no-cache";
    if (file && sendFile(req, res, file, cache)) return;
    if (path.extname(p) === "" && sendFile(req, res, path.join(world.hostDir, "index.html"), "no-cache")) return;
    res.writeHead(404, { "Content-Type": "text/plain", "Cache-Control": "no-store" });
    res.end("not found");
  });
}

function createModuleServer(id, up) {
  return http.createServer((req, res) => {
    const p = decodeURIComponent(new URL(req.url ?? "/", "http://up").pathname);
    const prefix = `/modules/${id}/`;
    const rel = p.startsWith(prefix) ? p.slice(prefix.length) : null;
    const file = rel === null ? null : safeJoin(up.dir, rel === "" ? "index.html" : rel);
    if (file && sendFile(req, res, file, "no-store")) return;
    res.writeHead(404, { "Content-Type": "text/plain", "Cache-Control": "no-store" });
    res.end("not found");
  });
}

async function startWorld(dirs) {
  world.hostDir = dirs.host;
  for (const [id, dir] of [
    ["example", dirs["example-v1"]],
    ["example2", dirs.example2],
    ["incompat", dirs.incompat],
    ["contract2", dirs.contract2],
    ["noexpose", dirs.noexpose],
  ]) {
    const up = { dir, mode: "up", port: 0 };
    up.port = await listen(createModuleServer(id, up));
    world.upstream.set(id, up);
  }
  return listen(createFront());
}

async function waitReady(base) {
  const urls = [`${base}/`, ...[...world.upstream.keys()].map((id) => `${base}/modules/${id}/remoteEntry.js`)];
  const end = Date.now() + 15000;
  for (const url of urls) {
    for (;;) {
      try {
        const r = await fetch(url);
        await r.arrayBuffer();
        if (r.ok) break;
      } catch {
        // not up yet
      }
      if (Date.now() > end) throw new InstrumentError(`not ready within 15 s: ${url}`);
      await sleep(100);
    }
  }
  world.log.length = 0;
}

// ---------------------------------------------------------------------------------------
// Scenario plumbing.
// ---------------------------------------------------------------------------------------

const manifestEntry = (id, over = {}) => ({
  id,
  version: "1.2.3",
  page: "./Page",
  label: `Mod ${id}`,
  icon: "package",
  navGroup: "modules",
  adminOnly: false,
  ...over,
});

let front = ""; // http://127.0.0.1:<port>
const results = [];
const observed = { missingExposeText: "", incompatFetched: [] };

async function newContext({ loadTimeoutMs } = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  ctx.setDefaultTimeout(10000);
  ctx.violations = [];
  ctx.consoleErrors = [];
  await ctx.exposeFunction("__uziHarnessCsp", (v) => ctx.violations.push(v));
  await ctx.addInitScript(
    ({ timeout }) => {
      document.addEventListener("securitypolicyviolation", (e) => {
        // @ts-ignore injected by exposeFunction
        window.__uziHarnessCsp({ directive: e.violatedDirective, blocked: e.blockedURI, source: e.sourceFile });
      });
      if (timeout) window.__UZI_MODULE_LOAD_TIMEOUT_MS__ = timeout;
    },
    { timeout: loadTimeoutMs ?? 0 },
  );
  ctx.on("page", (page) => {
    page.on("console", (m) => {
      if (m.type() === "error") ctx.consoleErrors.push(m.text().slice(0, 300));
    });
    page.on("pageerror", (e) => ctx.consoleErrors.push(`pageerror: ${e.message.slice(0, 300)}`));
  });
  return ctx;
}

async function scenario(name, fn, { allowViolations = false, loadTimeoutMs } = {}) {
  // `--only <text>` (development): run just the scenarios whose name contains the text.
  const only = argv.includes("--only") ? argv[argv.indexOf("--only") + 1] : "";
  if (only && !name.includes(only)) return;
  const t0 = Date.now();
  resetWorld();
  const ctxs = [];
  const ctxFactory = async (o) => {
    const c = await newContext({ loadTimeoutMs, ...o });
    ctxs.push(c);
    return c;
  };
  let error = null;
  try {
    await fn(ctxFactory);
    if (!allowViolations) {
      for (const c of ctxs) assert(c.violations.length === 0, `CSP violations: ${JSON.stringify(c.violations.slice(0, 3))}`);
    }
  } catch (e) {
    error = e;
    for (const c of ctxs) if (c.consoleErrors.length) log(`  [${name}] console errors: ${c.consoleErrors.slice(0, 4).join(" | ")}`);
  } finally {
    await Promise.allSettled(ctxs.map((c) => c.close()));
  }
  const ms = Date.now() - t0;
  results.push({ name, ok: error === null, ms, error: error?.message });
  log(`${error === null ? "PASS" : "FAIL"} ${name} (${ms} ms)${error ? `\n     ${error.message.split("\n")[0]}` : ""}`);
}

const reqs = (since, pred) => world.log.slice(since).filter(pred);
const modulePaths = (since) => reqs(since, (r) => r.path.startsWith("/modules/"));

async function openAs(ctx, kind, modules, pathname) {
  world.session = kind;
  world.modules = modules;
  const page = await ctx.newPage();
  const resp = await page.goto(front + pathname);
  return { page, resp };
}

const vis = (page, sel, timeout) => page.locator(sel).first().waitFor({ state: "visible", timeout });
const textOf = (page, sel) => page.locator(sel).first().innerText();
const navLink = (id) => `nav a[href="/m/${id}"]`;

// ---------------------------------------------------------------------------------------
// Scenarios.
// ---------------------------------------------------------------------------------------

async function runScenarios(dirs) {
  const hostManifest = JSON.parse(fs.readFileSync(path.join(dirs.host, ".vite", "manifest.json"), "utf8"));
  let fed;
  try {
    fed = federationOnlyFiles(hostManifest);
  } catch (e) {
    if (e instanceof ManifestShapeError) throw new InstrumentError(`host build manifest: ${e.message}`);
    throw e;
  }
  const hostIndexHtml = fs.readFileSync(path.join(dirs.host, "index.html"), "utf8");

  await scenario("typecheck: the example module compiles against the host sdk types", async () => {
    const tsc = path.join(WEB_DIR, "node_modules", "typescript", "bin", "tsc");
    const r = spawnSync(process.execPath, [tsc, "-p", path.join(WEB_DIR, "examples", "ui-module", "tsconfig.json")], { encoding: "utf8" });
    assert(r.status === 0, `tsc exit ${r.status}:\n${(r.stdout + r.stderr).slice(0, 1500)}`);
  });

  await scenario(
    "csp: exact header on the host document; the control page's inline script is reported",
    async (ctxf) => {
      const ctx = await ctxf();
      const { page, resp } = await openAs(ctx, "admin", [], "/dashboard");
      assert(resp.headers()["content-security-policy"] === CSP, `unexpected host CSP header: ${resp.headers()["content-security-policy"]}`);
      const control = await ctx.newPage();
      await control.goto(`${front}/__csp-control`);
      const end = Date.now() + 5000;
      while (ctx.violations.length === 0 && Date.now() < end) await sleep(50);
      assert(ctx.violations.some((v) => /script-src/.test(v.directive)), `the deliberate violation was not reported: ${JSON.stringify(ctx.violations)}`);
      assert((await control.evaluate(() => window.__inlineRan)) !== true, "the inline script ran despite the CSP");
      await page.close();
    },
    { allowViolations: true },
  );

  await scenario("ex.1: nav and command-menu entries, render, theme follows a live switch, host-router navigation, nested and lazy routes", async (ctxf) => {
    const ctx = await ctxf();
    const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" })], "/dashboard");
    await vis(page, navLink("example"));
    await page.keyboard.press("Control+K");
    await page.getByRole("option", { name: /Example/ }).first().waitFor({ state: "visible" });
    await page.keyboard.press("Escape");
    await page.locator(navLink("example")).click();
    await vis(page, ".uzi-mod-example");
    assert(/Example module v1/.test(await textOf(page, "[data-testid=example-title]")), "v1 title missing");
    await page.waitForFunction(() => /api ok: admin@example\.test/.test(document.querySelector("[data-testid=example-api]")?.textContent ?? ""));
    assert(/theme: dark/.test(await textOf(page, "[data-testid=example-theme]")), "sdk.theme.mode should start dark");
    // The module's own stylesheet loaded (not blocked by the CSP) and reads the host variables.
    const css = () =>
      page.evaluate(() => {
        const el = document.querySelector(".uzi-mod-example");
        return { border: getComputedStyle(el).borderLeftWidth, bg: getComputedStyle(el).backgroundColor, surface: getComputedStyle(document.documentElement).getPropertyValue("--surface").trim() };
      });
    const before = await css();
    assert(before.border === "4px", `module stylesheet not applied: ${JSON.stringify(before)}`);
    await page.evaluate(() => {
      document.documentElement.dataset.theme = "dawn";
    });
    await page.waitForFunction(() => /theme: light/.test(document.querySelector("[data-testid=example-theme]")?.textContent ?? ""));
    const after = await css();
    assert(after.bg !== before.bg && after.surface !== before.surface, `the theme variable did not follow the switch: ${JSON.stringify({ before, after })}`);
    await page.evaluate(() => {
      document.documentElement.dataset.theme = "ember";
    });
    await page.waitForFunction(() => /theme: dark/.test(document.querySelector("[data-testid=example-theme]")?.textContent ?? ""));
    // Nested + lazy route, then the host router (no reload) back to the host's own page.
    await page.evaluate(() => {
      window.__noReload = performance.timeOrigin;
    });
    await page.getByRole("link", { name: "Sub view" }).click();
    await page.waitForFunction(() => /Sub view, build v1/.test(document.querySelector("[data-testid=example-subview]")?.textContent ?? ""));
    assert(new URL(page.url()).pathname === "/m/example/sub", `unexpected url ${page.url()}`);
    await page.goBack();
    await vis(page, "[data-testid=example-title]");
    await page.getByRole("link", { name: "Back to the dashboard" }).click();
    await page.waitForURL(`${front}/dashboard`);
    assert((await page.evaluate(() => window.__noReload === performance.timeOrigin)) === true, "the page reloaded during host-router navigation");
  });

  for (const mode of ["stopped", "stall"]) {
    await scenario(
      `ex.2 (${mode}): Retry card, nav entry stays, dashboard still works, Retry recovers without a reload`,
      async (ctxf) => {
        const ctx = await ctxf({});
        world.upstream.get("example").mode = mode;
        const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" })], "/m/example");
        world.upstream.get("example").mode = mode; // openAs reset nothing, but be explicit
        await page.getByRole("button", { name: "Retry" }).waitFor({ state: "visible", timeout: 8000 });
        observed[`firstLoadFailure.${mode}`] = await textOf(page, "[data-testid=module-error-reason]");
        await vis(page, navLink("example"));
        await page.evaluate(() => {
          window.__noReload = performance.timeOrigin;
        });
        // The rest of the app still works: client-side to the dashboard and back.
        await page.locator('nav a[href="/dashboard"]').first().click();
        await page.waitForURL(`${front}/dashboard`);
        await page.getByText("Module unavailable").waitFor({ state: "detached" });
        await vis(page, navLink("example"));
        await page.locator(navLink("example")).click();
        await page.getByRole("button", { name: "Retry" }).waitFor({ state: "visible", timeout: 8000 });
        world.upstream.get("example").mode = "up";
        await page.getByRole("button", { name: "Retry" }).click();
        await vis(page, ".uzi-mod-example");
        assert((await page.evaluate(() => window.__noReload === performance.timeOrigin)) === true, "Retry reloaded the document");
      },
      { loadTimeoutMs: 1500 },
    );
  }

  const incompatReasons = {
    incompat: /does not satisfy the requirement of/,
    contract2: /host contract 2/,
    noexpose: null, // pinned from observation below
  };
  for (const id of ["incompat", "contract2", "noexpose"]) {
    for (const role of ["admin", "member"]) {
      await scenario(`ex.3: ${id} as ${role}: incompatible card, no Retry${id === "incompat" ? ", no remote React fetched" : ""}`, async (ctxf) => {
        const ctx = await ctxf();
        const mark = world.log.length;
        const { page } = await openAs(ctx, role, [manifestEntry(id, { page: id === "noexpose" ? "Missing" : "./Page" })], `/m/${id}`);
        await page.getByText("This module is not compatible with this version of uzi.").waitFor({ state: "visible" });
        assert((await page.getByRole("button", { name: "Retry" }).count()) === 0, "an incompatible module must not offer Retry");
        assert((await page.locator(".uzi-mod-example").count()) === 0, "an incompatible module rendered");
        const body = await page.locator("body").innerText();
        if (role === "admin") {
          assert(body.includes(id) && body.includes("1.2.3"), "admin should see the module id and version");
          const reason = await textOf(page, "[data-testid=module-error-reason]");
          if (id === "noexpose") observed.missingExposeText = reason;
          else assert(incompatReasons[id].test(reason), `unexpected reason for ${id}: ${reason}`);
        } else {
          assert(!body.includes("1.2.3") && (await page.locator("[data-testid=module-error-reason]").count()) === 0, "non-admins must see generic text only");
        }
        if (id === "incompat") {
          const fetched = modulePaths(mark).map((r) => r.path);
          const reactFetched = fetched.filter((p) => /\/assets\/.*(react|jsx)/i.test(p) && !/loadShare|localSharedImportMap/i.test(p));
          observed.incompatFetched = fetched;
          assert(fetched.some((p) => p.endsWith("/remoteEntry.js")), "control: the refused module's entry was never requested");
          assert(reactFetched.length === 0, `the refused module still fetched React: ${reactFetched.join(", ")}`);
        }
      });
    }
  }

  await scenario("guard: signed-out and non-admin direct visits make zero /modules/ requests", async (ctxf) => {
    const ctx = await ctxf();
    const mark = world.log.length;
    const { page } = await openAs(ctx, null, [manifestEntry("example")], "/m/example");
    await page.waitForURL(/\/login/);
    await sleep(500);
    assert(modulePaths(mark).length === 0, `signed-out visit requested ${modulePaths(mark).map((r) => r.path).join(", ")}`);
    const mark2 = world.log.length;
    const ctx2 = await ctxf();
    const { page: page2 } = await openAs(ctx2, "member", [manifestEntry("example", { adminOnly: true })], "/m/example");
    await page2.waitForURL(`${front}/dashboard`);
    await sleep(500);
    assert(modulePaths(mark2).length === 0, `non-admin visit requested ${modulePaths(mark2).map((r) => r.path).join(", ")}`);
  });

  await scenario("react identity: the host's React and router are the remotes' (example + example2 in one document)", async (ctxf) => {
    const ctx = await ctxf();
    const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" }), manifestEntry("example2", { label: "Example Two" })], "/m/example");
    await vis(page, ".uzi-mod-example");
    await page.locator(navLink("example2")).click();
    await page.waitForFunction(() => /Example module v1/.test(document.querySelector("[data-testid=example-title]")?.textContent ?? ""));
    const same = await page.evaluate(async () => {
      const host = window.__uziHostReactRefs;
      const out = {};
      for (const id of ["example", "example2"]) {
        // The remote container is already initialised by the runtime; get() returns the same exposed module.
        const container = await import(`/modules/${id}/remoteEntry.js`);
        const factory = await container.get("./Page");
        const refs = (await factory()).reactRefs;
        out[id] = Object.fromEntries(Object.keys(host).map((k) => [k, refs[k] === host[k]]));
      }
      return out;
    });
    for (const id of ["example", "example2"]) {
      for (const [k, v] of Object.entries(same[id])) assert(v === true, `${id}.reactRefs.${k} is not the host's`);
    }
  });

  await scenario("ex.4: zero modules fetch no federation code (manifest traversal a-d); with modules, no remoteEntry before a module route", async (ctxf) => {
    assert(fed.only.size > 0, "the host build has no federation-only files; the check would be vacuous");
    const ctx = await ctxf();
    const mark = world.log.length;
    const { page } = await openAs(ctx, "admin", [], "/dashboard");
    await vis(page, "nav");
    await page.waitForLoadState("networkidle");
    await sleep(300);
    const mj = reqs(mark, (r) => r.path === "/modules.json");
    assert(mj.length === 1, `(a) expected exactly one /modules.json request, saw ${mj.length}`);
    assert(modulePaths(mark).length === 0, `(b) /modules/ requested: ${modulePaths(mark).map((r) => r.path).join(", ")}`);
    const v = zeroModuleViolations({ manifest: hostManifest, requestedPaths: world.log.slice(mark).map((r) => r.path), indexHtml: hostIndexHtml });
    assert(v.length === 0, `(c/d) ${v.join("; ")}`);

    // With modules configured: registration happens, the federation chunks load, but no remote entry yet.
    const ctx2 = await ctxf();
    const mark2 = world.log.length;
    const { page: page2 } = await openAs(ctx2, "admin", [manifestEntry("example"), manifestEntry("example2")], "/dashboard");
    await vis(page2, "nav");
    await page2.waitForLoadState("networkidle");
    await sleep(300);
    assert(reqs(mark2, (r) => /remoteEntry/.test(r.path)).length === 0, "a remoteEntry was requested before any module route");
    const onlyRequested = world.log.slice(mark2).filter((r) => fed.only.has(r.path.replace(/^\//, "")));
    assert(onlyRequested.length > 0, "with modules configured the federation chunk was never requested (the zero-module check has no positive control)");
    const stats = await page2.evaluate(() => window.__uziModuleStats);
    assert(stats.registerRemotes === 1 && stats.loadRemote === 0, `unexpected stats ${JSON.stringify(stats)}`);
  });

  await scenario("ex.5: a rollout replaces the files: the lazy chunk 404s, Reload page, the reload renders v2", async (ctxf) => {
    const ctx = await ctxf();
    const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" })], "/m/example");
    await page.waitForFunction(() => /Example module v1/.test(document.querySelector("[data-testid=example-title]")?.textContent ?? ""));
    const up = world.upstream.get("example");
    try {
      up.dir = dirs["example-v2"];
      await page.getByRole("link", { name: "Sub view" }).click();
      await page.getByRole("button", { name: "Reload page" }).waitFor({ state: "visible" });
      assert((await page.getByRole("button", { name: "Retry" }).count()) === 0, "a stale module must not offer Retry");
      await page.getByRole("button", { name: "Reload page" }).click();
      await page.waitForFunction(() => /Sub view, build v2/.test(document.querySelector("[data-testid=example-subview]")?.textContent ?? ""));
    } finally {
      up.dir = dirs["example-v1"];
    }
  });

  await scenario("render-error Retry: same document, no re-registration, no new loadRemote", async (ctxf) => {
    const ctx = await ctxf();
    const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" })], "/m/example/fail-until-armed");
    await page.getByRole("button", { name: "Retry" }).waitFor({ state: "visible" });
    await page.getByText("This module hit an error and stopped.").waitFor({ state: "visible" });
    await sleep(1500); // well past React's automatic re-render attempts
    assert((await page.getByRole("button", { name: "Retry" }).count()) === 1, "the error card disappeared on its own");
    const snap = () => page.evaluate(() => ({ origin: performance.timeOrigin, stats: { ...window.__uziModuleStats } }));
    const before = await snap();
    await page.evaluate(() => {
      window.__uziFixtureArmSuccess = true;
    });
    await page.getByRole("button", { name: "Retry" }).click();
    await page.getByText("recovered", { exact: true }).waitFor({ state: "visible" });
    const after = await snap();
    assert(after.origin === before.origin, "Retry reloaded the document");
    assert(JSON.stringify(after.stats) === JSON.stringify(before.stats), `Retry changed the module counters: ${JSON.stringify(before.stats)} -> ${JSON.stringify(after.stats)}`);
  });

  await scenario(
    "two remotes: one stopped example2 in a fresh document does not affect example",
    async (ctxf) => {
      const ctx = await ctxf();
      world.upstream.get("example2").mode = "stopped";
      const mark = world.log.length;
      const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" }), manifestEntry("example2", { label: "Example Two" })], "/m/example");
      world.upstream.get("example2").mode = "stopped";
      await vis(page, ".uzi-mod-example");
      await page.locator(navLink("example2")).click();
      await page.getByRole("button", { name: "Retry" }).waitFor({ state: "visible" });
      const entry = reqs(mark, (r) => r.path === "/modules/example2/remoteEntry.js");
      assert(entry.length >= 1 && entry.every((r) => r.status === 502), `example2's entry request should have failed: ${JSON.stringify(entry)}`);
      await page.locator(navLink("example")).click();
      await vis(page, ".uzi-mod-example");
    },
    { loadTimeoutMs: 3000 },
  );

  await scenario("literal scan: no eval(, new Function, Function( or inline script in any built artifact; hooks only in the harness host", async () => {
    const scanDirs = Object.values(dirs);
    const r = spawnSync(process.execPath, [SCAN_SCRIPT, ...scanDirs], { encoding: "utf8" });
    assert(r.status === 0, `csp-build-scan exit ${r.status}:\n${(r.stdout + r.stderr).slice(0, 1500)}`);
    const grep = (dir, needle) => {
      for (const f of fs.readdirSync(path.join(dir, "assets"))) {
        if (f.endsWith(".js") && fs.readFileSync(path.join(dir, "assets", f), "utf8").includes(needle)) return true;
      }
      return false;
    };
    assert(grep(dirs.host, "__uziModuleStats"), "control: the harness host should carry __uziModuleStats");
    assert(!grep(dirs.hostOff, "__uziModuleStats") && !grep(dirs.hostOff, "__uziHostReactRefs"), "the shipped host build carries a test hook");
  });
}

// ---------------------------------------------------------------------------------------
// Pinned-from-observation output and the report-only measurement.
// ---------------------------------------------------------------------------------------

function median(a) {
  const s = [...a].sort((x, y) => x - y);
  return s[Math.floor(s.length / 2)];
}

async function coldLoad(pathname) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  try {
    const page = await ctx.newPage();
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("Network.enable");
    const types = new Map();
    const finished = [];
    cdp.on("Network.responseReceived", (e) => types.set(e.requestId, e.type));
    cdp.on("Network.loadingFinished", (e) => finished.push({ id: e.requestId, bytes: e.encodedDataLength }));
    await page.addInitScript(() => {
      window.__lcp = 0;
      new PerformanceObserver((l) => {
        for (const e of l.getEntries()) window.__lcp = e.startTime;
      }).observe({ type: "largest-contentful-paint", buffered: true });
    });
    await page.goto(front + pathname, { waitUntil: "load" });
    await page.waitForLoadState("networkidle");
    await sleep(300);
    const nav = await page.evaluate(() => ({ loadEventEnd: performance.getEntriesByType("navigation")[0].loadEventEnd, lcp: window.__lcp }));
    const total = finished.reduce((s, f) => s + f.bytes, 0);
    const js = finished.filter((f) => types.get(f.id) === "Script").reduce((s, f) => s + f.bytes, 0);
    return { bytes: total, jsBytes: js, requests: finished.length, loadEventEnd: nav.loadEventEnd, lcp: nav.lcp };
  } finally {
    await ctx.close();
  }
}

function gzipBundle(dir) {
  let js = 0;
  let all = 0;
  const walk = (d) => {
    for (const n of fs.readdirSync(d)) {
      const p = path.join(d, n);
      if (fs.statSync(p).isDirectory()) walk(p);
      else if (/\.(js|css|html)$/.test(n)) {
        const z = zlib.gzipSync(fs.readFileSync(p)).length;
        all += z;
        if (n.endsWith(".js")) js += z;
      }
    }
  };
  walk(dir);
  return { js, all };
}

async function measure(dirs) {
  const subjects = [["candidate (hooks off)", dirs.hostOff]];
  if (process.env.MODULES_HARNESS_BASE_DIST) subjects.unshift(["base", path.resolve(process.env.MODULES_HARNESS_BASE_DIST)]);
  log("\nmeasurement (report only): zero modules, 5 cold /dashboard loads, medians");
  for (const [label, dir] of subjects) {
    world.hostDir = dir;
    resetWorld();
    world.session = "admin";
    world.modules = [];
    const runs = [];
    for (let i = 0; i < 5; i++) runs.push(await coldLoad("/dashboard"));
    const m = (k) => median(runs.map((r) => r[k]));
    const g = gzipBundle(dir);
    log(
      `  ${label}: transferred ${m("bytes")} B, js ${m("jsBytes")} B, requests ${m("requests")}, loadEventEnd ${m("loadEventEnd").toFixed(0)} ms, LCP ${m("lcp").toFixed(0)} ms; gzip bundle js ${g.js} B, all ${g.all} B`,
    );
  }
  world.hostDir = dirs.hostOff;
  resetWorld();
  world.session = "admin";
  world.modules = [manifestEntry("example", { label: "Example" })];
  const ex = await coldLoad("/m/example");
  log(`  /m/example cold load (candidate): transferred ${ex.bytes} B, js ${ex.jsBytes} B, requests ${ex.requests}, loadEventEnd ${ex.loadEventEnd.toFixed(0)} ms`);
  world.hostDir = dirs.host;
}

// ---------------------------------------------------------------------------------------
// Main.
// ---------------------------------------------------------------------------------------

async function main() {
  if (MODE === "selftest-child") return selftestChild();

  if (MODE === "selftest") {
    const note = await selftestInterrupt();
    log(`PASS interrupt self-test: ${note}`);
    return 0;
  }

  const wall0 = Date.now();
  const tempOut = !process.env.MODULES_HARNESS_OUT;
  const outRoot = process.env.MODULES_HARNESS_OUT
    ? path.resolve(process.env.MODULES_HARNESS_OUT)
    : fs.mkdtempSync(path.join(os.tmpdir(), "uzi-modules-harness-"));
  fs.mkdirSync(outRoot, { recursive: true });
  installSignalHandlers();
  let exitCode = 0;
  try {
    const dirs = await buildAll(outRoot);
    const port = await startWorld(dirs);
    front = `http://127.0.0.1:${port}`;
    await waitReady(front);
    await startBrowser();
    await runScenarios(dirs);
    if (!argv.includes("--no-measure") && !argv.includes("--only")) await measure(dirs);
    if (observed.missingExposeText) log(`\nobserved missing-expose error text (admin card reason): ${observed.missingExposeText}`);
    for (const [k, v] of Object.entries(observed)) if (k.startsWith("firstLoadFailure")) log(`observed ${k} (admin card reason): ${v}`);
    log(`observed requests of the refused incompat module: ${observed.incompatFetched.join(", ")}`);
    if (world.unknownApi.size > 0) log(`stub API paths answered generically: ${[...world.unknownApi].sort().join(", ")}`);
  } catch (e) {
    console.error(e instanceof InstrumentError ? `BROKEN: ${e.message}` : e);
    exitCode = e instanceof InstrumentError ? 2 : 1;
  }
  await shutdown();
  const problems = await cleanupProblems();
  results.push({ name: "cleanup: browser group gone, no tracked port accepts", ok: problems.length === 0, ms: 0, error: problems.join("; ") });
  log(`${problems.length === 0 ? "PASS" : "FAIL"} cleanup: browser group gone, no tracked port accepts${problems.length ? `\n     ${problems.join("; ")}` : ""}`);

  if (exitCode === 0 && !argv.includes("--only")) {
    const t0 = Date.now();
    try {
      const note = await selftestInterrupt();
      results.push({ name: "interrupt self-test", ok: true, ms: Date.now() - t0 });
      log(`PASS interrupt self-test (${Date.now() - t0} ms): ${note}`);
    } catch (e) {
      if (e instanceof InstrumentError) {
        console.error(`BROKEN: ${e.message}`);
        exitCode = 2;
      }
      results.push({ name: "interrupt self-test", ok: false, ms: Date.now() - t0, error: e.message });
      log(`FAIL interrupt self-test\n     ${e.message}`);
    }
  }
  if (tempOut) fs.rmSync(outRoot, { recursive: true, force: true });
  const failed = results.filter((r) => !r.ok);
  log(`\n${results.length - failed.length}/${results.length} passed; harness wall time ${((Date.now() - wall0) / 1000).toFixed(1)} s`);
  if (exitCode === 0 && failed.length > 0) exitCode = 1;
  return exitCode;
}

main().then(
  (code) => {
    if (MODE !== "selftest-child") process.exit(code ?? 0);
  },
  (e) => {
    console.error(e instanceof InstrumentError ? `BROKEN: ${e.message}` : e);
    void shutdown().finally(() => process.exit(e instanceof InstrumentError ? 2 : 1));
  },
);
