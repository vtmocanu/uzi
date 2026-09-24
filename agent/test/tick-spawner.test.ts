import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { TickSpawner, argvClass } from "../src/tick-spawner.js";

// issue #1597 M2 — the mid-turn checkpoint tick's spawner: own process group per child, SIGTERM
// then SIGKILL after a grace, `completed` only after exit, settled(), and PROVEN-ownership lock
// custody (pre-spawn snapshot + /proc fd evidence + dev/ino re-check). Real child processes.

const NODE = process.execPath;
let dir: string;
let bare: string;

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-tick-spawner-"));
  bare = path.join(dir, "bare.git");
  fs.mkdirSync(path.join(bare, "refs", "uzi-runner", "agent"), { recursive: true });
  fs.mkdirSync(path.join(bare, "objects", "info"), { recursive: true });
});

afterEach(() => {
  fs.rmSync(dir, { recursive: true, force: true });
});

function script(name: string, body: string): string {
  const p = path.join(dir, `${name}.cjs`);
  fs.writeFileSync(p, body);
  return p;
}

function req(argv: string[], identity: BoundaryProcessRequest["identity"] = "worker_pat"): BoundaryProcessRequest {
  return { argv, cwd: dir, env: { PATH: process.env.PATH }, identity };
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    assert.equal((e as NodeJS.ErrnoException).code, "ESRCH");
    return false;
  }
}

/** Resolve once the child printed `ready` on stdout. */
function ready(h: BoundaryProcessHandle): Promise<void> {
  return new Promise((resolve) => {
    let buf = "";
    h.stdout!.on("data", (c: Buffer) => {
      buf += String(c);
      if (buf.includes("ready")) resolve();
    });
  });
}

/** A child that ignores SIGTERM, optionally creates+holds `lock` (O_EXCL), then idles. */
function stubborn(lock?: string): string {
  return script(
    `stubborn-${Math.random().toString(36).slice(2)}`,
    `const fs = require("fs");
process.on("SIGTERM", () => {});
${lock ? `fs.openSync(${JSON.stringify(lock)}, "wx");` : ""}
process.stdout.write("ready\\n");
setInterval(() => {}, 1000);
`,
  );
}

describe("TickSpawner (issue #1597 M2)", () => {
  it("runs a child to completion; completed carries the exit code and settled() resolves", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal });
    const h = await sp.spawn(req([NODE, "-e", "process.exit(3)"]));
    assert.deepEqual(await h.completed, { code: 3 });
    await sp.settled();
    assert.equal(sp.cancelledAny(), false);
  });

  it("identity `command` runs through runnerCommand (a passthrough single-uid) in the given cwd", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal });
    const h = await sp.spawn(req([NODE, "-e", "process.stdout.write(process.cwd())"], "command"));
    let out = "";
    h.stdout!.on("data", (c: Buffer) => (out += String(c)));
    assert.deepEqual(await h.completed, { code: 0 });
    assert.equal(fs.realpathSync(out), fs.realpathSync(dir));
  });

  it("rejects a relative executable and refuses to spawn after the signal aborted", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal });
    await assert.rejects(sp.spawn(req(["node", "-e", "1"])), /absolute/);
    ac.abort();
    await assert.rejects(sp.spawn(req([NODE, "-e", "1"])), (e: Error) => e.name === "AbortError");
  });

  it("SIGTERM ends a cooperative child; its grandchild in the same group dies too", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, killGraceMs: 5_000 });
    const parent = script(
      "parent",
      `const { spawn } = require("child_process");
const c = spawn(${JSON.stringify(NODE)}, ["-e", "setInterval(() => {}, 1000)"], { stdio: "ignore" });
process.stdout.write("ready " + c.pid + "\\n");
setInterval(() => {}, 1000);
`,
    );
    const h = await sp.spawn(req([NODE, parent]));
    let out = "";
    h.stdout!.on("data", (c: Buffer) => (out += String(c)));
    await ready(h);
    const grandchild = Number(/ready (\d+)/.exec(out)![1]);
    const [pid] = sp.pids();
    ac.abort();
    await h.completed;
    await sp.settled();
    assert.equal(alive(pid!), false, "the child is gone");
    // The group SIGTERM reached the grandchild too (it does not trap it).
    const deadline = Date.now() + 3_000;
    while (alive(grandchild) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
    assert.equal(alive(grandchild), false, "the grandchild in the child's process group is gone");
    assert.equal(sp.cancelledAny(), true);
  });

  it("escalates to SIGKILL after the grace for a SIGTERM-ignoring child; completed only after exit", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, killGraceMs: 200 });
    const h = await sp.spawn(req([NODE, stubborn()]));
    await ready(h);
    const [pid] = sp.pids();
    const t0 = Date.now();
    ac.abort();
    const { code } = await h.completed;
    assert.ok(Date.now() - t0 >= 150, "it survived SIGTERM for the grace");
    assert.equal(code, 128, "killed by a signal");
    assert.equal(alive(pid!), false);
    await sp.settled();
  });

  it("a scoped sub-signal kills only via that deadline, and settled() covers scoped children", async () => {
    const ac = new AbortController();
    const sub = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, killGraceMs: 200 });
    const h = await sp.scoped(sub.signal)(req([NODE, stubborn()]));
    await ready(h);
    sub.abort();
    await h.completed;
    await sp.settled();
    assert.equal(ac.signal.aborted, false);
    assert.equal(alive(sp.pids()[0]!), false);
  });

  it("argvClass names the git subcommand without paths or config values", () => {
    assert.equal(argvClass(["/usr/bin/git", "-C", "/b", "-c", "protocol.file.allow=user", "fetch", "--no-tags", "x"]), "git fetch");
    assert.equal(argvClass(["/usr/bin/git", "-C", "/b", "pack-objects", "--revs"]), "git pack-objects");
    assert.equal(argvClass(["/usr/local/bin/gitleaks", "git", "/b"]), "gitleaks");
  });
});

describe("TickSpawner lock custody (issue #1597 M2)", () => {
  const branch = "agent/issue-7";
  const trackingLock = (): string => path.join(bare, "refs", "uzi-runner", "agent", "issue-7.lock");

  it("proven-owned: a SIGKILLed child's held lock (absent pre-spawn, same inode) is removed", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch, killGraceMs: 200 });
    const h = await sp.spawn(req([NODE, stubborn(trackingLock())]));
    await ready(h);
    ac.abort();
    await h.completed;
    await sp.settled();
    assert.ok(fs.existsSync(trackingLock()), "SIGKILL left the lock behind");
    const res = await sp.reconcileLocks();
    assert.deepEqual(res.retained, []);
    assert.deepEqual(res.removed, [fs.realpathSync(path.dirname(trackingLock())) + "/issue-7.lock"]);
    assert.equal(fs.existsSync(trackingLock()), false);
    // Idempotent.
    assert.deepEqual(await sp.reconcileLocks(), { removed: [], retained: [] });
  });

  it("foreign: a lock another process created during the cancellation is RETAINED with evidence", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch, killGraceMs: 300 });
    const h = await sp.spawn(req([NODE, stubborn()]));
    await ready(h);
    ac.abort();
    // A SEPARATE process creates packed-refs.lock while the child is being cancelled.
    const foreign: ChildProcess = spawn(NODE, [
      "-e",
      `require("fs").writeFileSync(${JSON.stringify(path.join(bare, "packed-refs.lock"))}, "x", { flag: "wx" })`,
    ]);
    await new Promise((r) => foreign.once("exit", r));
    await h.completed;
    await sp.settled();
    const res = await sp.reconcileLocks();
    assert.deepEqual(res.removed, []);
    assert.equal(res.retained.length, 1);
    assert.equal(res.retained[0]!.reason, "foreign");
    assert.equal(res.retained[0]!.argvClass, "node");
    assert.deepEqual(res.retained[0]!.preSpawn, { present: false });
    assert.equal(typeof res.retained[0]!.ino, "number");
    assert.ok(fs.existsSync(path.join(bare, "packed-refs.lock")), "never deleted");
  });

  it("replaced: the child held the lock but the path now has a different inode — RETAINED", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch, killGraceMs: 200 });
    const h = await sp.spawn(req([NODE, stubborn(trackingLock())]));
    await ready(h);
    ac.abort();
    await h.completed;
    await sp.settled();
    // Replace the file at the same path (new inode) before reconcile.
    const tmp = trackingLock() + ".new";
    fs.writeFileSync(tmp, "other");
    fs.renameSync(tmp, trackingLock());
    const res = await sp.reconcileLocks();
    assert.deepEqual(res.removed, []);
    assert.equal(res.retained[0]?.reason, "inode_mismatch");
    assert.ok(fs.existsSync(trackingLock()));
  });

  it("preexisting: a lock present before the spawn is never removed", async () => {
    fs.writeFileSync(path.join(bare, "config.lock"), "old");
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch, killGraceMs: 200 });
    const h = await sp.spawn(req([NODE, stubborn()]));
    await ready(h);
    ac.abort();
    await h.completed;
    await sp.settled();
    const res = await sp.reconcileLocks();
    assert.deepEqual(res.removed, []);
    assert.equal(res.retained[0]?.reason, "preexisting");
    assert.ok(fs.existsSync(path.join(bare, "config.lock")));
  });

  it("no cancellation: reconcile is a no-op even when a lock appeared", async () => {
    const ac = new AbortController();
    const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch });
    const h = await sp.spawn(req([NODE, "-e", `require("fs").writeFileSync(${JSON.stringify(path.join(bare, "shallow.lock"))}, "")`]));
    await h.completed;
    assert.deepEqual(await sp.reconcileLocks(), { removed: [], retained: [] });
    assert.ok(fs.existsSync(path.join(bare, "shallow.lock")));
  });
});
