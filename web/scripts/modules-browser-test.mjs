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
// SIGTERM, is idempotent, and always: (0) if the launch is still pending (a signal during
// chromium.launchServer, before the group id is known), waits for it, bounded to 10 s, and a launch
// that resolves later kills its own group; (1) asks the browser to close, bounded to 3 s, (2) then
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
import { childGroupRows, createTerminationGate, flushThenExit, launchPhaseProblem, waitForLeader } from "./modules-browser-groups.mjs";
import { federationOnlyFiles, ManifestShapeError, zeroModuleRequestViolations, zeroModuleViolations } from "./modules-manifest-traversal.mjs";

const WEB_DIR = fileURLToPath(new URL("..", import.meta.url));
const SELF = fileURLToPath(import.meta.url);
const SCAN_SCRIPT = path.join(WEB_DIR, "scripts", "csp-build-scan.mjs");

// The enforced CSP of the web nginx. It is read from web/nginx.conf (the server-level
// `add_header Content-Security-Policy`) at harness start by loadCsp(), never copied here, and
// cross-checked against scripts/assert-web-nginx.sh `CSP=` when that file is present. Every
// response of the harness front carries it, so a violation here is a violation under the real
// server.
let CSP = "";

function loadCsp() {
  const conf = path.join(WEB_DIR, "nginx.conf");
  let text;
  try {
    text = fs.readFileSync(conf, "utf8");
  } catch (e) {
    throw new InstrumentError(`cannot read ${conf}: ${e.message}`);
  }
  const m = /^ {4}add_header Content-Security-Policy "([^"]+)" always;/m.exec(text);
  if (!m) throw new InstrumentError(`no server-level add_header Content-Security-Policy found in ${conf}`);
  CSP = m[1];
  SECURITY_HEADERS["Content-Security-Policy"] = CSP;
  const assertScript = path.join(WEB_DIR, "..", "scripts", "assert-web-nginx.sh");
  if (fs.existsSync(assertScript)) {
    const a = /^CSP="(.*)"$/m.exec(fs.readFileSync(assertScript, "utf8"));
    if (!a) throw new InstrumentError(`no CSP="..." line found in ${assertScript}`);
    if (a[1] !== CSP) throw new InstrumentError(`CSP drift: web/nginx.conf has\n  ${CSP}\nbut scripts/assert-web-nginx.sh has\n  ${a[1]}`);
  }
}

const argv = process.argv.slice(2);
const SELFTEST_LAUNCH = argv.includes("--selftest-launch"); // child mode: be signalled DURING the browser launch
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
  const r = spawnSync("ps", ["-o", "pid=,pgid=,stat=,comm=", "-e"], { encoding: "utf8" });
  if (r.status !== 0) throw new InstrumentError(`ps failed: ${r.stderr}`);
  return r.stdout
    .split("\n")
    .map((l) => l.trim().split(/\s+/))
    .filter((c) => c.length >= 3 && Number(c[1]) === pgid)
    .map((c) => ({ pid: Number(c[0]), stat: c[2], comm: c.slice(3).join(" ") }));
}

/** childGroupRows with a ps failure reported as an instrument error. */
function childGroupRowsOrBroken(ppid) {
  try {
    return childGroupRows(ppid);
  } catch (e) {
    throw new InstrumentError(e.message);
  }
}

/**
 * "Gone" is ESRCH for the group, or every remaining member being an unreaped zombie. Returns
 * { problem: string|null, branch } where branch names which of those satisfied it.
 */
async function waitGroupGone(pgid, deadlineMs = 5000) {
  const end = Date.now() + deadlineMs;
  let last = "";
  for (;;) {
    try {
      process.kill(-pgid, 0);
    } catch (e) {
      if (e.code === "ESRCH") return { problem: null, branch: "ESRCH (group fully reaped)" };
    }
    const members = groupMembers(pgid);
    const alive = members.filter((m) => !m.stat.startsWith("Z"));
    if (alive.length === 0) {
      return { problem: null, branch: members.length > 0 ? `zombies-only (${members.length} unreaped: ${members.map((m) => m.pid).join(",")})` : "no members listed" };
    }
    last = alive.map((m) => `${m.pid}(${m.stat} ${m.comm})`).join(",");
    if (Date.now() > end) return { problem: `process group ${pgid} still has live members: ${last}`, branch: "none" };
    await sleep(100);
  }
}

// The interrupt self-test's child (see selftestInterrupt). Tracked here so shutdown and the
// signal handlers can kill and await it: the child is the one process the harness spawns
// besides the browser.
let selfChild = null;
let selfChildPgid = 0; // the browser group the child reported, killed too as a last resort
let selfChildExited = null;
let killChildPromise = null;

function resetSelfChild(child, exited) {
  selfChild = child;
  selfChildExited = exited;
  selfChildPgid = 0;
  killChildPromise = null;
}

function killSelfChild() {
  killChildPromise ??= (async () => {
    if (!selfChild || !selfChildExited) return;
    if (selfChild.exitCode === null && selfChild.signalCode === null) {
      selfChild.kill("SIGTERM");
      if ((await withTimeout(selfChildExited, 5000)) === "timeout") {
        selfChild.kill("SIGKILL");
        await withTimeout(selfChildExited, 2000);
      }
    }
    if (selfChildPgid) {
      try {
        process.kill(-selfChildPgid, "SIGKILL");
      } catch (e) {
        if (e.code !== "ESRCH") console.error(`killSelfChild: kill(-${selfChildPgid}) failed: ${e.message}`);
      }
    }
  })();
  return killChildPromise;
}

// ---------------------------------------------------------------------------------------
// Browser lifecycle and the idempotent shutdown.
// ---------------------------------------------------------------------------------------

let browserServer = null;
let browser = null;
let browserPgid = 0;
let launchPromise = null; // the pending (or settled) chromium.launchServer, kept so shutdown can await it
let launchSettled = false; // set when launchPromise's continuation ran or it rejected
let shuttingDown = false;
let shutdownPromise = null;
const LAUNCH_WAIT_MS = 10000;

function killBrowserGroup() {
  if (!browserPgid) return;
  try {
    process.kill(-browserPgid, "SIGKILL");
  } catch (e) {
    if (e.code !== "ESRCH") console.error(`shutdown: kill(-${browserPgid}) failed: ${e.message}`);
  }
}

async function startBrowser() {
  try {
    // Self-test hook (child only): hold the launch promise open after Chromium exists, so the
    // wrapper's SIGTERM lands while launchServer has not resolved and the group id is unknown.
    const hookDelay = MODE === "selftest-child" ? Number(process.env.MODULES_SELFTEST_LAUNCH_DELAY_MS ?? 0) : 0;
    // The group id is recorded in the continuation attached HERE, so whoever awaits
    // launchPromise (startBrowser, or shutdown during an interrupt) sees it already set.
    launchPromise = chromium
      .launchServer({
        ...chromiumLaunchOptions(),
        headless: true,
        handleSIGINT: false,
        handleSIGTERM: false,
        handleSIGHUP: false,
      })
      .then(
        async (server) => {
          if (hookDelay > 0) await sleep(hookDelay);
          browserServer = server;
          browserPgid = server.process().pid;
          launchSettled = true;
          if (MODE === "selftest-child") console.log(`BROWSER pgid=${browserPgid}`);
          // A launch that resolved after shutdown gave up waiting for it: kill it now.
          if (shuttingDown) killBrowserGroup();
          return server;
        },
        (e) => {
          launchSettled = true;
          throw e;
        },
      );
    await launchPromise;
    // Shutdown began while launching: its signal handler (or the crash handler) ends the
    // process after cleanup, so park here instead of reporting a spurious instrument error.
    if (shuttingDown) await new Promise(() => {});
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
    shuttingDown = true;
    // 0. A signal during chromium.launchServer: the group id is unknown until it resolves.
    //    Wait for it (bounded); a launch that outlives the bound kills its own group when it
    //    resolves (see startBrowser).
    if (launchPromise && !browserPgid) await withTimeout(launchPromise.catch(() => {}), LAUNCH_WAIT_MS);
    // 1. graceful close, bounded. A connect()ed browser's close() only disconnects; the
    //    server's close() is what closes Chromium.
    await withTimeout(Promise.allSettled([browser?.close(), browserServer?.close()]), 3000);
    // 2. unconditionally, even if 1 failed or timed out.
    killBrowserGroup();
    // 2b. Prove it, then say so BEFORE the caller's process.exit: Playwright's own exit
    //     handler also SIGKILLs launched browsers' groups, so the interrupt self-test cannot
    //     tell the harness's cleanup from Playwright's by looking at the group alone. It
    //     requires this line, with the pgid it learned, instead. Only printed for a known
    //     group (a shutdown that never learned the pgid prints nothing).
    if (browserPgid) {
      const g = await waitGroupGone(browserPgid);
      if (!g.problem) console.log(`SHUTDOWN group ${browserPgid} gone`);
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
    log(`cleanup: group ${browserPgid} gone via: ${g.branch}`);
    if (g.problem) problems.push(g.problem);
  }
  for (const port of ports) {
    if (await acceptsConnection(port)) problems.push(`port ${port} still accepts connections`);
  }
  return problems;
}

const shutdownAll = () => Promise.allSettled([shutdown(), killSelfChild()]);

// Whether the first signal arrived while the launch was still pending (null: no signal yet).
let signalLaunchPending = null;
// Closed by a signal or crash before its cleanup starts; selftestInterrupt refuses to spawn after it.
const termination = createTerminationGate();

function installSignalHandlers() {
  for (const [sig, code] of [
    ["SIGINT", 130],
    ["SIGTERM", 143],
  ]) {
    process.on(sig, () => {
      const first = signalLaunchPending === null;
      if (first) signalLaunchPending = launchPromise !== null && !launchSettled;
      termination.close();
      if (first && MODE === "selftest-child") console.log(`SIGNAL launch-pending=${signalLaunchPending}`);
      void shutdownAll().finally(() => flushThenExit(code));
    });
  }
  const crash = (e) => {
    termination.close();
    console.error("harness: unhandled error", e);
    void shutdownAll().finally(() => flushThenExit(1));
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
  if (SELFTEST_LAUNCH) {
    // Phase "launch": start the launch, wait until Chromium exists (it is our child) but
    // launchServer has not resolved, announce it, and keep waiting. The wrapper's SIGTERM
    // arrives in this window.
    const launching = startBrowser();
    // Observe the launch from the start: a rejection while the poll sleeps must end the poll
    // and surface through `await launching` (the BROKEN path), not as an unhandled rejection.
    let launchFailed = false;
    launching.catch(() => {
      launchFailed = true;
    });
    const seen = await waitForLeader({
      hasLeader: () => childGroupRowsOrBroken(process.pid).length > 0,
      launchFailed: () => launchFailed,
      deadlineMs: 10000,
    });
    if (seen === "failed") await launching;
    // Announce only once a browser group leader was actually seen.
    if (seen !== "leader") throw new InstrumentError("the browser never appeared as a group-leading child within 10 s of the launch");
    console.log(`LAUNCHING ports=${[...ports].join(",")}`);
    await launching;
    // Not signalled: the wrapper reports it; stay alive so it can.
    setInterval(() => {}, 1 << 30);
    return;
  }
  await startBrowser();
  const page = await (await browser.newContext()).newPage();
  await page.goto(`http://127.0.0.1:${port}/`);
  const end = Date.now() + 10000;
  while (stalled === 0 && Date.now() < end) await sleep(50);
  if (stalled === 0) throw new InstrumentError("the stalled request never reached the server");
  console.log(`READY pgid=${browserPgid} ports=${[...ports].join(",")}`);
  setInterval(() => {}, 1 << 30);
}

/**
 * phase "ready": SIGTERM a child whose browser runs and holds a stalled request.
 * phase "launch": SIGTERM a child while chromium.launchServer is still pending (the group id
 * is not yet known to the child). The wrapper learns the group two independent ways, both
 * collected until the child exits: the `BROWSER pgid=` line the child prints once it knows,
 * and a `ps` poll for the child's own children (the browser). A child that dies without
 * the harness ever recording the group would otherwise leave nothing to assert on, so
 * "no group identified" is a failure of the self-test, not a pass.
 */
async function selftestInterrupt(phase) {
  // A signal's (or crash's) cleanup (shutdownAll -> killSelfChild) may already have run while
  // its exit waits on the stdout flush: never spawn a child it can no longer cover. Park; that
  // handler ends the process. (Not `shuttingDown`: the full run calls shutdown() before the
  // self-tests on purpose.)
  await termination.beforeSpawn();
  const env = { ...process.env, ...(phase === "launch" ? { MODULES_SELFTEST_LAUNCH_DELAY_MS: "1500" } : {}) };
  const args = [SELF, "--selftest-child", ...(phase === "launch" ? ["--selftest-launch"] : [])];
  const child = spawn(process.execPath, args, { stdio: ["ignore", "pipe", "inherit"], env });
  const exited = new Promise((resolve) => child.once("exit", (code, signal) => resolve({ code, signal })));
  resetSelfChild(child, exited);
  let out = "";
  const marker = phase === "launch" ? /LAUNCHING ports=([\d,]*)/ : /READY pgid=(\d+) ports=([\d,]+)/;
  const ready = new Promise((resolve) => {
    child.stdout.on("data", (d) => {
      out += d;
      const b = /BROWSER pgid=(\d+)/.exec(out);
      const r = /READY pgid=(\d+)/.exec(out);
      // Recorded as soon as it is printed, so every cleanup path below can kill the group.
      if (b || r) selfChildPgid = Number((b ?? r)[1]);
      const m = marker.exec(out);
      if (m) resolve({ ports: (phase === "launch" ? m[1] : m[2]).split(",").filter(Boolean).map(Number) });
    });
    child.once("exit", () => resolve(null));
  });
  const info = await withTimeout(ready, 30000);
  if (info === "timeout" || info === null) {
    // Never SIGKILL the child here: it may own a browser group it has not told us about.
    // SIGTERM runs its shutdown, which awaits the launch and kills the group; the helper
    // escalates only after that, and kills the reported group as a last resort.
    await killSelfChild();
    throw new InstrumentError(`interrupt self-test (${phase}) child never became ready: ${out.trim()}`);
  }
  const seen = new Map(); // pgid -> first sampled row
  let polling = true;
  const poll = (async () => {
    while (polling) {
      try {
        for (const row of childGroupRows(child.pid)) if (!seen.has(row.pgid)) seen.set(row.pgid, row);
      } catch {
        // ps unavailable: the BROWSER line is then the only source
      }
      await sleep(20);
    }
  })();
  child.kill("SIGTERM");
  const result = await withTimeout(exited, 15000);
  polling = false;
  await poll;
  // The exit event can precede the last stdout data: let the pipe drain before reading `out`.
  if (!child.stdout.readableEnded) await withTimeout(new Promise((r) => child.stdout.once("end", r)), 2000);
  const problems = [];
  if (result === "timeout") {
    await killSelfChild();
    problems.push("the child did not exit within 15 s of SIGTERM");
  }
  if (phase === "launch") {
    const lp = launchPhaseProblem(out);
    if (lp) problems.push(lp);
  }
  const groups = new Set(seen.keys());
  const reported = /(?:BROWSER|READY) pgid=(\d+)/.exec(out);
  if (reported) groups.add(Number(reported[1]));
  if (groups.size === 0) throw new InstrumentError(`interrupt self-test (${phase}): no browser group was identified, so cleanup cannot be asserted: ${out.trim()}`);
  // The harness's own shutdown must have killed and verified the group (see shutdown, 2b):
  // Playwright's exit handler would otherwise make this pass without it.
  for (const pgid of groups) {
    if (!out.includes(`SHUTDOWN group ${pgid} gone`)) {
      const r = seen.get(pgid);
      const origin = r
        ? ` (sampled as pid ${r.pid} ppid ${r.ppid} pgid ${r.pgid}; the child reported pgid ${reported ? reported[1] : "none"})`
        : "";
      problems.push(`the harness's shutdown never reported group ${pgid} gone (cleanup relied on something else)${origin}`);
    }
  }
  const via = [];
  for (const pgid of groups) {
    const g = await waitGroupGone(pgid);
    if (g.problem) problems.push(g.problem);
    via.push(`${pgid} via ${g.branch}`);
  }
  for (const p of info.ports) {
    if (await acceptsConnection(p)) problems.push(`port ${p} still accepts connections after SIGTERM`);
  }
  if (problems.length > 0) throw new Error(`interrupt self-test (${phase}): ${problems.join("; ")}`);
  return `${phase}: child ${result.signal ?? `exit ${result.code}`}, group ${via.join("; ")}, ports ${info.ports.join(",")} closed`;
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

let pageChunkPath = ""; // set by buildAll: example-v1's Page chunk, as a request path

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
      const cfg = createModuleConfig({ id: m.id, buildTag: m.buildTag, outDir: dirs[m.dir] });
      // Only example-v1 gets a build manifest (set HERE; the example's vite.config.ts is
      // untouched): the chunk-failure scenarios need the hashed file name of the Page chunk.
      if (m.dir === "example-v1") cfg.build = { ...cfg.build, manifest: true };
      await build({ ...cfg, logLevel: "error" });
    }
    // The request path of example-v1's Page chunk, for the chunk-failure scenario.
    const manifestFile = path.join(dirs["example-v1"], ".vite", "manifest.json");
    let pageChunk;
    try {
      const manifest = JSON.parse(fs.readFileSync(manifestFile, "utf8"));
      const found = manifest["src/Page.tsx"] ?? Object.values(manifest).find((e) => e && e.src === "src/Page.tsx");
      if (!found || typeof found.file !== "string") throw new Error("no entry for src/Page.tsx");
      pageChunk = `/modules/example/${found.file}`;
    } catch (e) {
      throw new InstrumentError(`example-v1 build manifest (${manifestFile}): ${e.message}`);
    }
    pageChunkPath = pageChunk;
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
  "Content-Security-Policy": "", // set by loadCsp() before the first server starts
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
  for (const u of world.upstream.values()) {
    u.mode = "up";
    u.failPaths.clear();
    u.holdRetryEntry = false;
    for (const p of u.parked.splice(0)) p.res.destroy();
  }
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
  const reqUrl = new URL(req.url ?? "/", "http://front");
  if (up.failPaths.has(reqUrl.pathname)) {
    res.writeHead(502, { "Content-Type": "text/plain", ...noStore });
    return res.end("bad gateway");
  }
  if (up.holdRetryEntry && reqUrl.pathname === `/modules/${id}/remoteEntry.js` && reqUrl.search.startsWith("?retry=")) {
    // Parked, not answered, until releaseParked(); the socket is tracked and destroyed at
    // shutdown (and by resetWorld).
    up.parked.push({ req, res });
    return;
  }
  forwardToModule(up, req, res, noStore);
}

// Releases every parked retry-entry request: each is proxied normally.
function releaseParked(up) {
  for (const p of up.parked.splice(0))
    forwardToModule(up, p.req, p.res, {
      "Cache-Control": "no-store",
      ...SECURITY_HEADERS,
    });
}

function forwardToModule(up, req, res, noStore) {
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
    const up = {
      dir,
      mode: "up",
      port: 0,
      failPaths: new Set(),
      holdRetryEntry: false,
      parked: [],
    };
    up.port = await listen(createModuleServer(id, up));
    world.upstream.set(id, up);
  }
  return listen(createFront());
}

const READY_DEADLINE_MS = 15000;

// Every URL must answer 2xx within ONE overall deadline. The remaining time bounds each fetch
// and its body read (a stalled server cannot hold the harness past it), and a success that
// completes after the deadline is not accepted. Expiry is an instrument error (exit 2) naming
// the pending URL.
async function waitReady(base, deadlineMs = READY_DEADLINE_MS) {
  const urls = [`${base}/`, ...[...world.upstream.keys()].map((id) => `${base}/modules/${id}/remoteEntry.js`)];
  const end = Date.now() + deadlineMs;
  for (const url of urls) {
    for (;;) {
      const remaining = end - Date.now();
      if (remaining <= 0) throw new InstrumentError(`not ready within ${deadlineMs / 1000} s: ${url}`);
      try {
        const signal = AbortSignal.timeout(remaining);
        const r = await fetch(url, { signal });
        await r.arrayBuffer(); // under the same signal: an abort rejects this read too
        if (r.ok && Date.now() <= end) break;
      } catch {
        // not up yet, or aborted at the deadline (caught by the check at the top)
      }
      if (Date.now() >= end) throw new InstrumentError(`not ready within ${deadlineMs / 1000} s: ${url}`);
      await sleep(Math.min(100, Math.max(0, end - Date.now())));
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
  ctx.requestUrls = [];
  // Every request any page of the context makes, for the single-origin assertion in scenario().
  ctx.on("request", (r) => ctx.requestUrls.push(r.url()));
  await ctx.exposeFunction("__uziHarnessCsp", (v) => ctx.violations.push(v));
  await ctx.addInitScript(
    ({ timeout }) => {
      document.addEventListener("securitypolicyviolation", (e) => {
        // @ts-ignore injected by exposeFunction
        window.__uziHarnessCsp({ directive: e.violatedDirective, blocked: e.blockedURI, source: e.sourceFile, page: location.pathname });
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
    // Single origin (PRD section 5): the host, the module files and the API all go through the
    // front proxy, so any request to another origin (or a non-http URL) is a failure.
    const frontOrigin = new URL(front).origin;
    for (const c of ctxs) {
      const stray = c.requestUrls.filter((u) => {
        try {
          return new URL(u).origin !== frontOrigin;
        } catch {
          return true;
        }
      });
      assert(stray.length === 0, `requests outside the front origin ${frontOrigin}: ${stray.slice(0, 3).join(", ")}`);
    }
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
      const elsewhere = ctx.violations.filter((v) => v.page !== "/__csp-control");
      assert(elsewhere.length === 0, `CSP violations from a page other than the control: ${JSON.stringify(elsewhere.slice(0, 3))}`);
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

  // A child chunk that fails on the first load. Retry only changes remoteEntry.js's URL; the
  // chunk keeps its URL. Pinned Chromium re-fetches a failed chunk (S1: Retry recovers in the
  // same document). Engines that keep a failed module file for the document are covered by the
  // card path S2 pins: after a Retry that fails transient the card also offers Reload page.
  const chunkFailure = () => {
    assert(
      pageChunkPath.startsWith("/modules/example/") && pageChunkPath.endsWith(".js"),
      `instrument: unexpected chunk path ${JSON.stringify(pageChunkPath)}`,
    );
    const chunkFile = pageChunkPath.slice(pageChunkPath.lastIndexOf("/") + 1);
    const up = world.upstream.get("example");
    const retryBtn = (page) => page.getByRole("button", { name: "Retry" });
    const reloadBtn = (page) => page.getByRole("button", { name: "Reload page" });
    // The same-document round trip of ex.2: nav entry stays, dashboard works, back to the card.
    const roundTrip = async (page) => {
      await vis(page, navLink("example"));
      await page.locator('nav a[href="/dashboard"]').first().click();
      await page.waitForURL(`${front}/dashboard`);
      await page.getByText("Module unavailable").waitFor({ state: "detached" });
      await page.locator(navLink("example")).click();
      await page.getByRole("button", { name: "Retry" }).waitFor({ state: "visible", timeout: 8000 });
    };
    // Steps 1-3 shared by both scenarios: entry 200, chunk 502, Retry only, same-document round trip.
    const firstFailure = async (ctx, setStep) => {
      setStep("1");
      up.failPaths.add(pageChunkPath);
      const m0 = world.log.length;
      const { page } = await openAs(ctx, "admin", [manifestEntry("example", { label: "Example" })], "/m/example");
      setStep("2");
      await retryBtn(page).waitFor({ state: "visible", timeout: 8000 });
      const firstEntry = reqs(m0, (r) => r.path === "/modules/example/remoteEntry.js" && r.search === "");
      assert(
        firstEntry.length >= 1 && firstEntry.every((r) => r.status === 200),
        `remoteEntry.js should have answered 200: ${JSON.stringify(firstEntry)}`,
      );
      const firstChunk = reqs(m0, (r) => r.path === pageChunkPath);
      assert(
        firstChunk.some((r) => r.status === 502),
        `the chunk should have answered 502: ${JSON.stringify(firstChunk)}`,
      );
      assert((await reloadBtn(page).count()) === 0, "Reload page must not be offered on the first failure");
      setStep("3");
      await page.evaluate(() => {
        window.__noReload = performance.timeOrigin;
      });
      await roundTrip(page);
      return { page, m0 };
    };
    const cleanup = () => {
      up.failPaths.clear();
      up.holdRetryEntry = false;
      releaseParked(up);
    };
    const wrapFail = (e, step, scenarioMark) =>
      new Error(
        `step ${step}: /modules/ requests ${JSON.stringify(modulePaths(scenarioMark).map((r) => `${r.path}${r.search} ${r.status}`))}: ${e instanceof Error ? e.message : String(e)}`,
      );
    return {
      chunkFile,
      up,
      retryBtn,
      reloadBtn,
      roundTrip,
      firstFailure,
      cleanup,
      wrapFail,
    };
  };

  await scenario(
    "child-chunk failure on first load, chunk restored: Retry recovers in the same document (Chromium re-fetches the failed chunk)",
    async (ctxf) => {
      const { up, retryBtn, reloadBtn, firstFailure, cleanup, wrapFail } = chunkFailure();
      const ctx = await ctxf();
      let step = "setup";
      const scenarioMark = world.log.length;
      try {
        const { page } = await firstFailure(ctx, (s) => {
          step = s;
        });
        step = "4";
        // 4. The chunk is healthy again.
        up.failPaths.clear();
        const probe = await page.evaluate(async (p) => (await fetch(p)).status, pageChunkPath);
        assert(probe === 200, `the restored chunk should answer 200 to a plain fetch, got ${probe}`);
        assert((await reloadBtn(page).count()) === 0, "Reload page must not be needed before the Retry");
        step = "5";
        const m1 = world.log.length;
        await retryBtn(page).click();
        await vis(page, ".uzi-mod-example");
        step = "6";
        const retryEntry = reqs(
          m1,
          (r) => r.path === "/modules/example/remoteEntry.js" && r.search.startsWith("?retry="),
        );
        assert(
          retryEntry.length >= 1 && retryEntry.every((r) => r.status === 200),
          `the Retry's remoteEntry.js should have answered 200: ${JSON.stringify(retryEntry)}`,
        );
        const refetched = reqs(m1, (r) => r.path === pageChunkPath);
        assert(
          refetched.some((r) => r.status === 200),
          `Chromium should re-request the failed chunk on Retry and get 200: ${JSON.stringify(refetched)}`,
        );
        assert(
          (await page.evaluate(() => window.__noReload === performance.timeOrigin)) === true,
          "Retry reloaded the document",
        );
        assert((await reloadBtn(page).count()) === 0, "Reload page must not appear when Retry recovers");
      } catch (e) {
        throw wrapFail(e, step, scenarioMark);
      } finally {
        cleanup();
      }
    },
  );

  await scenario(
    "child-chunk failure persists through Retry: Reload page next to Retry; the reload recovers once the chunk is back",
    async (ctxf) => {
      const { chunkFile, up, retryBtn, reloadBtn, roundTrip, firstFailure, cleanup, wrapFail } = chunkFailure();
      const ctx = await ctxf();
      let step = "setup";
      const scenarioMark = world.log.length;
      try {
        const { page } = await firstFailure(ctx, (s) => {
          step = s;
        });

        step = "5";
        // 5. The chunk keeps failing. Retry, with the new remoteEntry.js request parked so the
        // pending state is observable.
        up.holdRetryEntry = true;
        const m1 = world.log.length;
        await retryBtn(page).click();
        step = "5a (loading state after Retry)";
        await page
          .getByRole("status")
          .filter({ hasText: "Loading module" })
          .waitFor({ state: "visible", timeout: 8000 });
        step = "5b (original card gone)";
        await page.getByText("Module unavailable").waitFor({ state: "detached", timeout: 8000 });
        step = "5c (retry entry parked)";
        const parkDeadline = Date.now() + 8000;
        while (up.parked.length === 0) {
          assert(Date.now() < parkDeadline, "Retry never requested remoteEntry.js?retry=N");
          await sleep(25);
        }
        up.holdRetryEntry = false;
        releaseParked(up);
        step = "5d (Retry settles after release)";
        await page.locator("[role=alert], .uzi-mod-example").first().waitFor({ state: "visible", timeout: 8000 });
        assert(
          (await page.locator(".uzi-mod-example").count()) === 0,
          "the module rendered although its chunk still answers 502",
        );
        await page.getByRole("alert").waitFor({ state: "visible", timeout: 8000 });

        step = "6";
        // 6. Before any Reload page check: what the Retry did on the wire, and what the card says.
        const retryEntry = reqs(
          m1,
          (r) => r.path === "/modules/example/remoteEntry.js" && r.search.startsWith("?retry="),
        );
        assert(
          retryEntry.length >= 1 && retryEntry.every((r) => r.status === 200),
          `the Retry's remoteEntry.js should have answered 200: ${JSON.stringify(retryEntry)}`,
        );
        const chunkReqs = reqs(m1, (r) => r.path === pageChunkPath);
        assert(
          chunkReqs.length >= 1 && chunkReqs.every((r) => r.status === 502),
          `the chunk should have been requested and answered 502: ${JSON.stringify(chunkReqs)}`,
        );
        const reason = await textOf(page, "[data-testid=module-error-reason]");
        assert(
          reason.includes(chunkFile) || reason.includes(pageChunkPath),
          `the reason should name the failing chunk ${chunkFile}: ${reason}`,
        );
        assert(
          (await page.evaluate(() => window.__noReload === performance.timeOrigin)) === true,
          "Retry reloaded the document",
        );

        step = "7";
        // 7. Only now: Reload page next to Retry.
        await reloadBtn(page)
          .waitFor({ state: "visible", timeout: 3000 })
          .catch(() => {
            throw new Error("after a Retry that failed transient the card offers no Reload page");
          });
        assert((await retryBtn(page).count()) === 1, "Retry should stay next to Reload page");

        step = "8";
        // 8. The per-document record keeps the offer across a client-side round trip.
        await roundTrip(page);
        await reloadBtn(page).waitFor({ state: "visible", timeout: 3000 });

        step = "9";
        // 9. The chunk is back; Reload page gets a fresh document, which loads the module.
        up.failPaths.clear();
        const probe = await page.evaluate(async (p) => (await fetch(p)).status, pageChunkPath);
        assert(probe === 200, `the restored chunk should answer 200 to a plain fetch, got ${probe}`);
        await reloadBtn(page).click();
        await page.waitForFunction(() => window.__noReload === undefined);
        await vis(page, ".uzi-mod-example");
      } catch (e) {
        throw wrapFail(e, step, scenarioMark);
      } finally {
        cleanup();
      }
    },
  );

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
    // example2 renders the same page source (same title text), so wait for the route AND for
    // the host to have loaded the second remote before comparing refs.
    await page.waitForURL(`${front}/m/example2`);
    await page.waitForFunction(() => window.__uziModuleStats?.loadRemote >= 2);
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

  await scenario("ex.4: zero modules fetch no federation code (manifest traversal a-e); with modules, no remoteEntry before a module route", async (ctxf) => {
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
    // (e) The request budget: nothing beyond the entry chunk, its css and route chunks, and no
    // modulepreload (a bundler runtime chunk split off by the lazy federation import is one).
    const budget = zeroModuleRequestViolations({ manifest: hostManifest, requestedPaths: world.log.slice(mark).map((r) => r.path), indexHtml: hostIndexHtml });
    assert(budget.length === 0, `(e) ${budget.join("; ")}`);

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
      const mark = world.log.length;
      await page.getByRole("link", { name: "Sub view" }).click();
      await page.getByRole("button", { name: "Reload page" }).waitFor({ state: "visible" });
      const stale = reqs(mark, (r) => r.path.startsWith("/modules/example/assets/") && r.status === 404);
      assert(stale.length >= 1, `the proxy log shows no 404 for a v1 asset under /modules/example/assets/: ${JSON.stringify(modulePaths(mark))}`);
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
    assert(before.stats && typeof before.stats.loadRemote === "number" && before.stats.loadRemote >= 1, `the counters are missing or the remote was never loaded, so the comparison would be vacuous: ${JSON.stringify(before.stats)}`);
    await page.evaluate(() => {
      window.__uziFixtureArmSuccess = true;
    });
    await page.getByRole("button", { name: "Retry" }).click();
    await page.getByText("recovered", { exact: true }).waitFor({ state: "visible" });
    // Quiescence: an async re-register or loadRemote regression may land after "recovered"
    // shows, so wait until the counters stay unchanged for 500 ms (bounded at 5 s).
    let last = JSON.stringify((await snap()).stats);
    let stableSince = Date.now();
    const quietEnd = Date.now() + 5000;
    while (Date.now() - stableSince < 500) {
      assert(Date.now() < quietEnd, `the module counters never settled within 5 s: ${last}`);
      await sleep(100);
      const cur = JSON.stringify((await snap()).stats);
      if (cur !== last) {
        last = cur;
        stableSince = Date.now();
      }
    }
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
    installSignalHandlers();
    for (const phase of ["ready", "launch"]) log(`PASS interrupt self-test: ${await selftestInterrupt(phase)}`);
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
    loadCsp();
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
    for (const phase of ["ready", "launch"]) {
      const t0 = Date.now();
      const name = `interrupt self-test (${phase})`;
      try {
        const note = await selftestInterrupt(phase);
        results.push({ name, ok: true, ms: Date.now() - t0 });
        log(`PASS ${name} (${Date.now() - t0} ms): ${note}`);
      } catch (e) {
        if (e instanceof InstrumentError) {
          console.error(`BROKEN: ${e.message}`);
          exitCode = 2;
        }
        results.push({ name, ok: false, ms: Date.now() - t0, error: e.message });
        log(`FAIL ${name}\n     ${e.message}`);
      }
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
    void shutdownAll().finally(() => flushThenExit(e instanceof InstrumentError ? 2 : 1));
  },
);
