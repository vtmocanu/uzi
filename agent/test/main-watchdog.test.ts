import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { MAIN_WATCHDOG_DEFAULTS } from "../src/main-watchdog.js";

const agent = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const fixture = (name: string) => path.join(agent, "test/fixtures", name);

interface Child {
  proc: ChildProcess;
  lines: string[];
  stdout: string;
  exited: Promise<number | null>;
  waitFor(pred: (line: string) => boolean, what: string, ms?: number): Promise<string>;
}

function run(file: string, args: string[] = []): Child {
  const proc = spawn(process.execPath, ["--import", "tsx", fixture(file), ...args], {
    cwd: agent, stdio: ["pipe", "pipe", "pipe"],
  });
  const lines: string[] = [];
  const waiters: Array<{ pred: (l: string) => boolean; resolve: (l: string) => void }> = [];
  let carry = "";
  const child: Child = {
    proc, lines, stdout: "",
    exited: new Promise((resolve) => proc.on("exit", (code) => resolve(code))),
    waitFor(pred, what, ms = 20_000) {
      const seen = lines.find(pred);
      if (seen !== undefined) return Promise.resolve(seen);
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error(`timed out waiting for ${what}; got ${JSON.stringify(lines)}`)), ms);
        waiters.push({ pred, resolve: (l) => { clearTimeout(timer); resolve(l); } });
      });
    },
  };
  proc.stderr!.on("data", (chunk: Buffer) => {
    carry += chunk.toString("utf8");
    let nl: number;
    while ((nl = carry.indexOf("\n")) >= 0) {
      const line = carry.slice(0, nl);
      carry = carry.slice(nl + 1);
      lines.push(line);
      for (let i = waiters.length - 1; i >= 0; i--) {
        const w = waiters[i];
        if (w !== undefined && w.pred(line)) { waiters.splice(i, 1); w.resolve(line); }
      }
    }
  });
  proc.stdout!.on("data", (chunk: Buffer) => { child.stdout += chunk.toString("utf8"); });
  return child;
}

const msgIs = (msg: string) => (line: string): boolean => {
  try { return (JSON.parse(line) as { msg?: string }).msg === msg; } catch { return false; }
};

async function withTimeout<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error(`timed out: ${what}`)), ms); }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

test("defaults and thread isolation settings", () => {
  assert.equal(MAIN_WATCHDOG_DEFAULTS.reportIntervalMs, 15_000);
  assert.equal(MAIN_WATCHDOG_DEFAULTS.stallMs, 30_000);
  const src = fs.readFileSync(path.join(agent, "src/main-watchdog.ts"), "utf8");
  assert.match(src, /execArgv: \[\]/);
  assert.match(src, /main-watchdog-thread\.mjs/);
});

test("a blocked main thread is reported as stalled before the block ends, then resumed", async (t) => {
  const child = run("main-watchdog-block.ts");
  t.after(() => { child.proc.kill("SIGKILL"); });
  await child.waitFor((l) => l === "ready", "ready");
  const stalledLine = await child.waitFor(msgIs("worker main thread stalled"), "stalled line");
  assert.ok(!child.lines.includes("unblocked"), "stall must be reported while main is still blocked");
  const stalled = JSON.parse(stalledLine) as Record<string, unknown>;
  assert.equal(stalled.level, "warn");
  assert.equal(typeof stalled.rss_bytes, "number");
  assert.ok((stalled.rss_bytes as number) > 0);
  assert.equal(typeof stalled.heartbeat_age_ms, "number");
  child.proc.stdin!.write("x");
  await child.waitFor((l) => l === "unblocked", "unblocked marker");
  const resumed = JSON.parse(await child.waitFor(msgIs("worker main thread resumed"), "resumed line")) as Record<string, unknown>;
  assert.equal(resumed.level, "warn");
  assert.deepEqual(Object.keys(resumed).sort(), ["level", "msg", "rss_bytes", "stalled_ms", "ts"]);
  assert.equal(typeof resumed.stalled_ms, "number");
  assert.equal(await withTimeout(child.exited, 10_000, "block fixture exit"), 0);
  assert.equal(child.lines.filter(msgIs("worker main thread stalled")).length, 1);
});

test("periodic lines carry exactly ts, level, msg, rss_bytes, heartbeat_age_ms", async (t) => {
  const child = run("main-watchdog-block.ts");
  t.after(() => { child.proc.kill("SIGKILL"); });
  const line = await child.waitFor(msgIs("worker main watchdog"), "periodic line");
  const parsed = JSON.parse(line) as Record<string, unknown>;
  assert.deepEqual(Object.keys(parsed).sort(), ["heartbeat_age_ms", "level", "msg", "rss_bytes", "ts"]);
  assert.equal(parsed.level, "info");
  assert.ok(Number.isInteger(parsed.heartbeat_age_ms));
  child.proc.stdin!.write("x");
  await withTimeout(child.exited, 10_000, "block fixture exit");
});

test("the watchdog does not keep the process alive without stop()", async (t) => {
  const child = run("main-watchdog-unref.ts");
  t.after(() => { child.proc.kill("SIGKILL"); });
  assert.equal(await withTimeout(child.exited, 15_000, "unref fixture exit"), 0);
});

for (const [mode, label] of [["throw", "thread throws at load"], ["missing", "thread file is missing"], ["sync", "constructor throws synchronously"]] as const) {
  test(`failure isolation: ${label}`, async (t) => {
    const child = run("main-watchdog-failing.ts", [mode]);
    t.after(() => { child.proc.kill("SIGKILL"); });
    assert.equal(await withTimeout(child.exited, 20_000, "failing fixture exit"), 0);
    assert.match(child.stdout, /^done$/m);
    const unavailable = child.lines.filter(msgIs("worker main watchdog unavailable"));
    assert.equal(unavailable.length, 1, JSON.stringify(child.lines));
    const parsed = JSON.parse(unavailable[0] ?? "{}") as Record<string, unknown>;
    assert.equal(parsed.level, "warn");
    const allowed = new Set(["ts", "level", "msg", "reason", ...(parsed.reason === "exit" ? ["exit_code"] : [])]);
    for (const key of Object.keys(parsed)) assert.ok(allowed.has(key), `unexpected key ${key}`);
    assert.ok(["start_failed", "error", "exit"].includes(parsed.reason as string));
    if (mode === "sync") assert.equal(parsed.reason, "start_failed");
    assert.ok(!child.lines.join("\n").includes("SECRET-MARKER"));
  });
}
