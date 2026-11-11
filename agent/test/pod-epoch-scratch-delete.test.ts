// Issue #4043 M2: the strict scratch delete (PINNED_SUBTREE_SCRIPT, oneFs mode), the shared epoch
// spawn/join core and GitCache.discardEpochScratchLeaf. Small temp fixtures; the real helper runs as
// the current uid (runnerCommand is the identity outside the uid split).
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { GitCache, type EpochScratchDiscardInput, type JoinedEpochChild } from "../src/git.js";
import { PINNED_SUBTREE_SCRIPT, restoreTreeWritability } from "../src/rmtree.js";
import { runnerCommand } from "../src/runner-uid.js";
import { nullLogger } from "./helpers.js";

const asRoot = process.getuid?.() === 0;
const NO_PROC_FD = "no /proc/self/fd on this host: the pinned walk refuses here by design";
const FS_EXT4 = 0xef53;
const FS_XFS = 0x58465342;

async function mktmp(): Promise<string> {
  return fsp.mkdtemp(path.join(os.tmpdir(), "uzi-scratch-delete-"));
}
async function cleanup(dir: string): Promise<void> {
  await restoreTreeWritability(dir).catch(() => undefined);
  await fsp.rm(dir, { recursive: true, force: true }).catch(() => undefined);
}
const exists = (p: string): Promise<boolean> => fsp.lstat(p).then(() => true, () => false);
function devIno(p: string): string {
  const st = fs.lstatSync(p, { bigint: true });
  return `${st.dev}:${st.ino}`;
}

interface Run { code: number | null; stdout: string }
function strictArgs(root: string, name: string, o: { expect?: string; rootExpect?: string; depth?: number; sync?: string; phase?: string; mode?: string; oneFs?: string } = {}): string[] {
  return [root, name, "2000000", "60000", o.mode ?? "remove", o.expect ?? devIno(path.join(root, name)), o.sync ?? "", o.phase ?? "pinned",
    o.rootExpect ?? devIno(root), o.oneFs ?? "1", String(o.depth ?? 256)];
}
function startHelper(args: string[], cwd = "/"): { done: Promise<Run> } {
  const child = spawn(process.execPath, ["-e", PINNED_SUBTREE_SCRIPT, ...args], { cwd, stdio: ["ignore", "pipe", "ignore"] });
  let stdout = "";
  child.stdout.on("data", (b: Buffer) => { stdout += b.toString(); });
  return { done: new Promise(resolve => child.on("close", code => resolve({ code, stdout }))) };
}
const runHelper = (args: string[]): Promise<Run> => startHelper(args).done;

/** Runs the helper to the test-only seam `phase`, hands control to `act`, then lets it continue. */
async function withSeam(root: string, name: string, phase: string, act: () => Promise<void> | void,
  o: { expect?: string; rootExpect?: string } = {}): Promise<Run> {
  const syncDir = await mktmp();
  try {
    const sync = path.join(syncDir, "sync");
    const run = startHelper(strictArgs(root, name, { ...o, sync, phase }));
    for (let i = 0; i < 500 && !(await exists(`${sync}.${phase}`)); i++) await new Promise(r => setTimeout(r, 20));
    assert.ok(await exists(`${sync}.${phase}`), `helper reached seam ${phase}`);
    await act();
    await fsp.writeFile(`${sync}.go`, "");
    return await run.done;
  } finally { await cleanup(syncDir); }
}

async function fixtureTree(): Promise<{ dir: string; root: string; leaf: string }> {
  const dir = await mktmp();
  const root = path.join(dir, "root");
  await fsp.mkdir(path.join(root, "leaf", "sub", "deeper"), { recursive: true });
  await fsp.writeFile(path.join(root, "leaf", "a.txt"), "a");
  await fsp.writeFile(path.join(root, "leaf", "sub", "b.txt"), "b");
  await fsp.writeFile(path.join(root, "leaf", "sub", "deeper", "c.txt"), "c");
  return { dir, root, leaf: path.join(root, "leaf") };
}

describe("strict scratch delete script", { skip: fs.existsSync("/proc/self/fd") ? false : NO_PROC_FD }, () => {
  it("receipt witness on the real host filesystem; a symlink inside is unlinked, never followed", async t => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const type = fs.statfsSync(dir).type;
      const victim = path.join(dir, "victim");
      await fsp.mkdir(victim);
      await fsp.writeFile(path.join(victim, "keep.txt"), "keep");
      await fsp.symlink(victim, path.join(leaf, "link"));
      const expect = devIno(leaf);
      const out = await runHelper(strictArgs(root, "leaf"));
      assert.equal(out.code, 0);
      assert.equal(out.stdout, `UZI-SCRATCH-REMOVED ${expect}\n`);
      assert.equal(await exists(leaf), false);
      assert.equal(await fsp.readFile(path.join(victim, "keep.txt"), "utf8"), "keep");
      if (type !== FS_EXT4 && type !== FS_XFS) {
        // The receipt condition (nlink 0 on the retained pin) is only proven here on ext4/XFS.
        t.todo(`UNVERIFIED: host filesystem 0x${type.toString(16)} is not ext4/XFS`);
      }
    } finally { await cleanup(dir); }
  });

  it("a rootExpect mismatch exits 3 and touches nothing", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await runHelper(strictArgs(root, "leaf", { rootExpect: "1:1" }));
      assert.equal(out.code, 3);
      assert.equal(out.stdout, "");
      assert.equal(await exists(path.join(leaf, "a.txt")), true);
    } finally { await cleanup(dir); }
  });

  it("a leaf that pins something else than expect exits 3", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      assert.equal((await runHelper(strictArgs(root, "leaf", { expect: "1:1" }))).code, 3);
      assert.equal(await exists(path.join(leaf, "a.txt")), true);
    } finally { await cleanup(dir); }
  });

  it("descending beyond maxDepth keeps the leaf and exits 8", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await runHelper(strictArgs(root, "leaf", { depth: 1 }));
      assert.equal(out.code, 8);
      assert.equal(out.stdout, "");
      assert.equal(await exists(path.join(leaf, "sub", "deeper", "c.txt")), true);
      // A depth that fits the tree removes it (leaf 0, sub 1, deeper 2).
      assert.equal((await runHelper(strictArgs(root, "leaf", { depth: 2 }))).code, 0);
    } finally { await cleanup(dir); }
  });

  it("the entry cap keeps the leaf and exits 8", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const args = strictArgs(root, "leaf");
      args[2] = "2";
      const out = await runHelper(args);
      assert.equal(out.code, 8);
      assert.equal(out.stdout, "");
      assert.equal(await exists(leaf), true);
    } finally { await cleanup(dir); }
  });

  it("refuses bad strict arguments with exit 6", async () => {
    const { dir, root } = await fixtureTree();
    try {
      for (const args of [
        strictArgs(root, "leaf", { mode: "empty" }),
        strictArgs(root, "leaf", { oneFs: "2" }),
        strictArgs(root, "leaf", { depth: -1 }),
        strictArgs(root, "leaf", { expect: "-" }),
        strictArgs(root, "leaf", { rootExpect: "" }),
        strictArgs(root, "leaf/sub"),
        // "." and ".." are refused by the script itself, not only by its caller's validator.
        strictArgs(root, ".", { expect: devIno(root) }),
        strictArgs(root, "..", { expect: devIno(path.dirname(root)) }),
      ]) assert.equal((await runHelper(args)).code, 6, args.join(" "));
      assert.equal(await exists(path.join(root, "leaf", "a.txt")), true);
    } finally { await cleanup(dir); }
  });

  it("chmods nothing: a read-only directory that cannot be emptied stays (exit 4)", { skip: asRoot ? "root bypasses directory modes" : false }, async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const ro = path.join(leaf, "sub");
      await fsp.chmod(ro, 0o555);
      const out = await runHelper(strictArgs(root, "leaf"));
      assert.equal(out.code, 4);
      assert.equal(out.stdout, "");
      assert.equal((await fsp.stat(ro)).mode & 0o7777, 0o555);
      assert.equal(await exists(path.join(ro, "b.txt")), true);
    } finally { await cleanup(dir); }
  });

  it("emptied seam: a directory swapped for another one is kept (exit 3)", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await withSeam(root, "leaf", "emptied", async () => {
        await fsp.rename(path.join(leaf, "sub"), path.join(leaf, "moved"));
        await fsp.mkdir(path.join(leaf, "sub"));
      });
      assert.equal(out.code, 3);
      assert.equal(out.stdout, "");
      assert.equal(await exists(path.join(leaf, "sub")), true);
    } finally { await cleanup(dir); }
  });

  it("rechecked seam: a non-empty plant is kept, an empty one is removed (the stated boundary)", async () => {
    const kept = await fixtureTree();
    try {
      // One descendant directory only, so the first "rechecked" pause is the one being planted over.
      await fsp.rm(path.join(kept.leaf, "sub", "deeper"), { recursive: true });
      // The helper only stops at "rechecked" once a descendant passed the comparison.
      const out = await withSeam(kept.root, "leaf", "rechecked", async () => {
        const d = path.join(kept.leaf, "sub");
        await fsp.rm(d, { recursive: true, force: true });
        await fsp.mkdir(d);
        await fsp.writeFile(path.join(d, "planted.txt"), "x");
      });
      assert.equal(out.code, 4);
      assert.equal(out.stdout, "");
      assert.equal(await exists(path.join(kept.leaf, "sub", "planted.txt")), true);
    } finally { await cleanup(kept.dir); }
    const empty = await fixtureTree();
    try {
      await fsp.rm(path.join(empty.leaf, "sub", "deeper"), { recursive: true });
      const out = await withSeam(empty.root, "leaf", "rechecked", async () => {
        const d = path.join(empty.leaf, "sub");
        await fsp.rm(d, { recursive: true, force: true });
        await fsp.mkdir(d);
      });
      assert.equal(out.code, 0);
      assert.match(out.stdout, /^UZI-SCRATCH-REMOVED \d+:\d+\n$/);
      assert.equal(await exists(empty.leaf), false);
    } finally { await cleanup(empty.dir); }
  });

  it("final seam: the original moved away and an empty replacement planted gives no receipt (exit 3)", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await withSeam(root, "leaf", "final", async () => {
        await fsp.rename(leaf, path.join(root, "moved"));
        await fsp.mkdir(leaf);
      });
      assert.equal(out.code, 3);
      assert.equal(out.stdout, "");
      // The helper's own rmdir took the empty replacement; the original is still linked elsewhere.
      assert.equal(await exists(leaf), false);
      assert.equal(await exists(path.join(root, "moved")), true);
    } finally { await cleanup(dir); }
  });

  it("final seam: another actor removing the emptied original gives no receipt (rmdir ENOENT, exit 3)", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await withSeam(root, "leaf", "final", async () => { await fsp.rmdir(leaf); });
      assert.equal(out.code, 3);
      assert.equal(out.stdout, "");
    } finally { await cleanup(dir); }
  });

  it("existing callers' argv is unchanged: the script still runs without the trailing arguments", async () => {
    const { dir, root, leaf } = await fixtureTree();
    try {
      const out = await runHelper([root, "leaf", "2000000", "60000", "remove", devIno(leaf)]);
      assert.equal(out.code, 0);
      assert.equal(out.stdout, "");
      assert.equal(await exists(leaf), false);
    } finally { await cleanup(dir); }
  });
  // A real mount cannot be built unprivileged, so the test-only `<sync>.otherdev` seam (live only with a
  // non-empty sync) makes one named descendant read as another st_dev.
  it("a descendant on another device is skipped, never entered or removed: exit 9, no receipt", async () => {
    const { dir, root, leaf } = await fixtureTree();
    const syncDir = await mktmp();
    try {
      const sync = path.join(syncDir, "sync");
      await fsp.writeFile(`${sync}.otherdev`, "sub");
      await fsp.writeFile(`${sync}.go`, "");
      const out = await runHelper(strictArgs(root, "leaf", { sync }));
      assert.equal(out.code, 9);
      assert.equal(out.stdout, "");
      assert.equal(await exists(path.join(leaf, "sub", "b.txt")), true);
      assert.equal(await exists(path.join(leaf, "sub", "deeper", "c.txt")), true);
      assert.equal(await exists(path.join(leaf, "a.txt")), false, "same-device content is still removed");
      assert.equal(await exists(leaf), true);
    } finally { await cleanup(syncDir); await cleanup(dir); }
  });

  it("the other-device seam is inert with an empty sync: a relative .otherdev in the cwd changes nothing", async () => {
    const { dir, root, leaf } = await fixtureTree();
    const cwdDir = await mktmp();
    try {
      // With sync "" the seam path would be the relative ".otherdev", resolved against the helper's cwd.
      await fsp.writeFile(path.join(cwdDir, ".otherdev"), "sub");
      const expect = devIno(leaf);
      const out = await startHelper(strictArgs(root, "leaf", { sync: "" }), cwdDir).done;
      assert.equal(out.code, 0);
      assert.equal(out.stdout, `UZI-SCRATCH-REMOVED ${expect}\n`);
      assert.equal(await exists(leaf), false);
    } finally { await cleanup(cwdDir); await cleanup(dir); }
  });
});

// ---- the shared spawn/join core -------------------------------------------------------------------

type Identity = "command" | "worker_pat";
interface Internal {
  spawnJoinedEpochChild(scope: PodEpochReclaimScope, command: string, args: string[], options: {
    env: NodeJS.ProcessEnv; identity: Identity; signals: readonly AbortSignal[]; deadline: number; timeout?: number; cwd?: string;
  }): Promise<JoinedEpochChild>;
  epochNativeTestDependencies?: {
    spawn: typeof spawn; alive: (pid: number, identity: Identity) => boolean;
    kill: (pid: number, identity: Identity) => void; joinMs: number;
  };
  unsettledDeletes: Map<string, number>;
  validateEpochScratchLaunch(command: string, args: readonly string[], env: NodeJS.ProcessEnv, cwd: string | undefined,
    identity: Identity, seamRoot?: string): void;
}

function fakeScope(opts: { realStdio?: boolean } = {}) {
  let pending = 0, stopped = false, inherited = 0;
  const scope: PodEpochReclaimScope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let done = false;
      return { settle() { if (!done) { done = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) {
      assert.ok(pending > 0, "ticket before inheritance");
      inherited++;
      return opts.realStdio ? [...stdio] : [...stdio, 42];
    },
    stop() { stopped = true; }, stopped() { return stopped; },
    assertSettled() { assert.equal(pending, 0); assert.equal(stopped, false); },
    finish() { this.assertSettled(); },
  };
  return { scope, pending: () => pending, stopped: () => stopped, inherited: () => inherited };
}

function fakeChildDeps(internal: Internal, o: {
  onSpawn?: (command: string, args: string[], options: { detached: boolean; stdio: unknown; env: NodeJS.ProcessEnv; cwd?: string }) => void;
  outcome: "receipt" | "exit1" | "hang" | { stdout: string; code: number } | "spawn-throws"; joinMs?: number;
}) {
  let exited = false;
  const child = Object.assign(new EventEmitter(), {
    pid: 4242, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), exitCode: null, signalCode: null,
  });
  internal.epochNativeTestDependencies = {
    spawn: ((command: string, args: string[], options: { detached: boolean; stdio: unknown; env: NodeJS.ProcessEnv; cwd?: string }) => {
      o.onSpawn?.(command, args, options);
      if (o.outcome === "spawn-throws") throw new Error("spawn failed");
      if (o.outcome !== "hang") {
        setImmediate(() => {
          const result = typeof o.outcome === "object" ? o.outcome : o.outcome === "exit1" ? { stdout: "", code: 1 } : { stdout: "", code: 0 };
          child.stdout.end(result.stdout); child.stderr.end();
          exited = true;
          child.emit("exit", result.code, null);
          child.emit("close", result.code, null);
        });
      }
      return child as unknown as ChildProcess;
    }) as unknown as typeof spawn,
    alive: () => !exited,
    kill: () => undefined,
    joinMs: o.joinMs ?? 40,
  };
  return child;
}

describe("spawnJoinedEpochChild core", () => {
  const baseOptions = () => ({ env: { PATH: "/usr/bin:/bin" }, identity: "command" as const, signals: [] as AbortSignal[], deadline: Date.now() + 10_000 });

  it("a joined non-zero exit returns with the ticket still pending; the caller settles", async () => {
    const f = fakeScope();
    const internal = new GitCache(path.join(os.tmpdir(), "cache"), nullLogger()) as unknown as Internal;
    fakeChildDeps(internal, { outcome: "exit1" });
    const result = await internal.spawnJoinedEpochChild(f.scope, "/bin/x", ["a"], baseOptions());
    assert.equal(result.joined, true);
    assert.equal(result.spawned, true);
    if (!result.spawned) return;
    assert.equal(result.code, 1);
    assert.equal(f.pending(), 1);
    assert.equal(f.stopped(), false);
    result.ticket.settle();
    assert.equal(f.pending(), 0);
  });

  it("a spawn error before a child exists returns spawned:false, ticket pending, scope not stopped", async () => {
    const f = fakeScope();
    const internal = new GitCache(path.join(os.tmpdir(), "cache"), nullLogger()) as unknown as Internal;
    fakeChildDeps(internal, { outcome: "spawn-throws" });
    const result = await internal.spawnJoinedEpochChild(f.scope, "/bin/x", [], baseOptions());
    assert.equal(result.spawned, false);
    assert.equal((result.failure as Error).message, "spawn failed");
    assert.equal(f.pending(), 1);
    assert.equal(f.stopped(), false);
  });

  it("an unconfirmed join stops the scope and throws epochProcessJoined:false with the ticket pending", async () => {
    const f = fakeScope();
    const internal = new GitCache(path.join(os.tmpdir(), "cache"), nullLogger()) as unknown as Internal;
    fakeChildDeps(internal, { outcome: "hang", joinMs: 30 });
    await assert.rejects(internal.spawnJoinedEpochChild(f.scope, "/bin/x", [], { ...baseOptions(), timeout: 5 }),
      (error: Error & { epochProcessJoined?: boolean }) => error.epochProcessJoined === false && /unconfirmed/.test(error.message));
    assert.equal(f.stopped(), true);
    assert.equal(f.pending(), 1);
  });
});

// ---- discardEpochScratchLeaf ----------------------------------------------------------------------

const SEAM = Symbol.for("uzi.test.pod-epoch-scratch.delete-seam");
const ROOT_EXPECT = "7:2";
const EXPECT = "7:5";

function censusText(name: string, kind: "same" | "absent" | "other", bytes = 100): { code: number; stdout: string } {
  const entry = kind === "absent" ? { name, type: "absent" }
    : { name, type: "dir", dev: "7", ino: kind === "same" ? "5" : "99", uid: 10001, fsdiff: false, fuid: false, bytes, shared: 0, ctime: 1, inc: false };
  return { code: 0, stdout: [{ root: ROOT_EXPECT, uid: 10001 }, entry, { end: true, entries: 1, overflow: false }].map(l => JSON.stringify(l)).join("\n") + "\n" };
}

function discardFixture(over: Partial<EpochScratchDiscardInput> = {}, census?: () => { code: number; stdout: string }) {
  const f = fakeScope();
  const git = new GitCache(path.join(os.tmpdir(), "cache"), nullLogger());
  const internal = git as unknown as Internal;
  const root = path.join(os.tmpdir(), "uzi-scratch-fixture-root");
  const events: string[] = [];
  const seen: { pending?: number; entry?: number } = {};
  const input: EpochScratchDiscardInput = {
    scope: f.scope, root: "/data/runner/.tmp", name: "leaf-1", expect: EXPECT, rootExpect: ROOT_EXPECT, bytes: 1000,
    signal: new AbortController().signal, deadline: Date.now() + 60_000,
    beforeLaunch: async () => { events.push("beforeLaunch"); },
    rootRepin: () => true, mountsOk: () => true,
    testSeam: {
      token: SEAM, root,
      census: async () => { events.push("census"); return (census ?? (() => censusText("leaf-1", "absent")))(); },
      onClassify: () => {
        seen.pending = f.pending();
        seen.entry = internal.unsettledDeletes.get(path.resolve(root, "leaf-1"));
      },
    },
    ...over,
  };
  return { f, git, internal, input, root, events, seen, key: path.resolve(root, "leaf-1") };
}

describe("GitCache.discardEpochScratchLeaf", () => {
  it("the launch validator admits only the fixed argv with a bounded per-launch time budget", () => {
    const { internal, root } = discardFixture();
    const build = (over: Partial<Record<number, string>> = {}) => {
      const tail = ["-e", PINNED_SUBTREE_SCRIPT, root, "leaf-1", "2000000", "300000", "remove", EXPECT, "", "pinned", ROOT_EXPECT, "1", "256"];
      for (const [i, v] of Object.entries(over)) tail[Number(i)] = v!;
      return runnerCommand(process.execPath, tail);
    };
    const ok = build();
    const env = { PATH: "/usr/bin:/bin" };
    assert.doesNotThrow(() => internal.validateEpochScratchLaunch(ok.command, ok.args, env, "/", "command", root));
    for (const budget of ["1000", "3000", "299999"]) {
      const args = build({ 5: budget });
      assert.doesNotThrow(() => internal.validateEpochScratchLaunch(args.command, args.args, env, "/", "command", root));
    }
    // Without the test seam the temp root is refused, and so is every deviation below.
    assert.throws(() => internal.validateEpochScratchLaunch(ok.command, ok.args, env, "/", "command"), /unaudited/);
    const bad: Array<[string, () => void]> = [
      ["name ..", () => { const a = build({ 3: ".." }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["name with slash", () => { const a = build({ 3: "a/b" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["dot name", () => { const a = build({ 3: ".hidden" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ...["uzi-worker", "uzi-runner", "uzi-quiesce-abc"].map((n): [string, () => void] => [`protected ${n}`,
        () => { const a = build({ 3: n }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }]),
      ["other root", () => { const a = build({ 2: "/data/runner" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["max entries", () => { const a = build({ 4: "1" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ...["0", "999", "300001", "-1000", "1000.5", "01000", "1e3", "NaN"].map((budget): [string, () => void] => ["time budget " + budget,
        () => { const a = build({ 5: budget }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }]),
      ["mode empty", () => { const a = build({ 6: "empty" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["bad expect", () => { const a = build({ 7: "-" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["sync set", () => { const a = build({ 8: "/tmp/s" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["phase", () => { const a = build({ 9: "emptied" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["bad rootExpect", () => { const a = build({ 10: "x" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["oneFs", () => { const a = build({ 11: "0" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["depth", () => { const a = build({ 12: "255" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["script", () => { const a = build({ 1: PINNED_SUBTREE_SCRIPT + "\n" }); internal.validateEpochScratchLaunch(a.command, a.args, env, "/", "command", root); }],
      ["extra arg", () => internal.validateEpochScratchLaunch(ok.command, [...ok.args, "extra"], env, "/", "command", root)],
      ["short argv", () => internal.validateEpochScratchLaunch(ok.command, ok.args.slice(0, -1), env, "/", "command", root)],
      ["extra env", () => internal.validateEpochScratchLaunch(ok.command, ok.args, { ...env, X: "1" }, "/", "command", root)],
      ["other PATH", () => internal.validateEpochScratchLaunch(ok.command, ok.args, { PATH: "/usr/local/bin" }, "/", "command", root)],
      ["empty env", () => internal.validateEpochScratchLaunch(ok.command, ok.args, {}, "/", "command", root)],
      ["cwd", () => internal.validateEpochScratchLaunch(ok.command, ok.args, env, "/tmp", "command", root)],
      ["identity", () => internal.validateEpochScratchLaunch(ok.command, ok.args, env, "/", "worker_pat", root)],
      ["command", () => internal.validateEpochScratchLaunch("/bin/sh", ok.args, env, "/", "command", root)],
    ];
    for (const [label, run] of bad) assert.throws(run, /unaudited/, label);
  });

  it("the public method refuses a bad name, a foreign root and a stray seam before any mutation", async () => {
    for (const over of [
      { name: ".." }, { name: "a/b" }, { expect: "x" }, { rootExpect: "-" },
      { testSeam: undefined, root: "/tmp" as never },
      { testSeam: { token: Symbol("other") } as never },
    ] as Array<Partial<EpochScratchDiscardInput>>) {
      const x = discardFixture(over);
      await assert.rejects(x.git.discardEpochScratchLeaf(x.input));
      assert.deepEqual(x.events, over.testSeam && over.testSeam.token !== SEAM ? [] : ["beforeLaunch"]);
      assert.equal(x.f.pending(), 0);
      assert.equal(x.internal.unsettledDeletes.size, 0);
    }
  });

  it("runs the pre-launch observation before the spawn, then launches with the inherited descriptor", async () => {
    const x = discardFixture({ deadline: Date.now() + 400_000 });
    fakeChildDeps(x.internal, {
      outcome: { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 },
      onSpawn: (command, args, options) => {
        x.events.push("spawn");
        assert.equal(options.detached, true);
        assert.deepEqual(options.stdio, ["pipe", "pipe", "pipe", 42]);
        assert.deepEqual(options.env, { PATH: "/usr/bin:/bin" });
        assert.equal(options.cwd, "/");
        const expected = runnerCommand(process.execPath, ["-e", PINNED_SUBTREE_SCRIPT, x.root, "leaf-1", "2000000", "300000", "remove",
          EXPECT, "", "pinned", ROOT_EXPECT, "1", "256"]);
        assert.equal(command, expected.command);
        assert.deepEqual(args, expected.args);
        assert.equal(x.f.pending(), 1);
        assert.equal(x.internal.unsettledDeletes.get(x.key), 1);
      },
    });
    await x.git.discardEpochScratchLeaf(x.input);
    assert.deepEqual(x.events, ["beforeLaunch", "spawn", "census"]);
    assert.equal(x.f.inherited(), 1);
  });

  it("a failing pre-launch observation or a dead scope launches nothing and leaves no entry", async () => {
    const refused = discardFixture({ beforeLaunch: async () => { throw new Error("proof failed"); } });
    fakeChildDeps(refused.internal, { outcome: "receipt", onSpawn: () => assert.fail("must not spawn") });
    await assert.rejects(refused.git.discardEpochScratchLeaf(refused.input), /proof failed/);
    assert.equal(refused.f.pending(), 0);
    assert.equal(refused.internal.unsettledDeletes.size, 0);
    const stopped = discardFixture();
    fakeChildDeps(stopped.internal, { outcome: "receipt", onSpawn: () => assert.fail("must not spawn") });
    stopped.f.scope.stop();
    await assert.rejects(stopped.git.discardEpochScratchLeaf(stopped.input));
    assert.equal(stopped.internal.unsettledDeletes.size, 0);
    const expired = discardFixture({ deadline: Date.now() - 1 });
    fakeChildDeps(expired.internal, { outcome: "receipt", onSpawn: () => assert.fail("must not spawn") });
    await assert.rejects(expired.git.discardEpochScratchLeaf(expired.input), /deadline/);
  });

  const settled: Array<[string, Parameters<typeof fakeChildDeps>[1]["outcome"], (() => { code: number; stdout: string }) | undefined, string]> = [
    ["a receipt and a vanished name settle removed", { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 }, () => censusText("leaf-1", "absent"), "removed"],
    ["a receipt and a different inode at the name settle removed", { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}`, code: 0 }, () => censusText("leaf-1", "other"), "removed"],
    ["a non-zero exit with the original still linked settles residue", { stdout: "", code: 4 }, () => censusText("leaf-1", "same", 400), "residue"],
    ["a spawn error with the original still linked settles residue", "spawn-throws", () => censusText("leaf-1", "same", 1000), "residue"],
  ];
  for (const [label, outcome, census, expected] of settled) {
    it(`${label}: entry and ticket are held through classification and cleared after`, async () => {
      const x = discardFixture({}, census);
      fakeChildDeps(x.internal, { outcome });
      const settlement = await x.git.discardEpochScratchLeaf(x.input);
      assert.equal(settlement.outcome, expected);
      if (settlement.outcome === "removed") assert.equal(settlement.bytes, 1000);
      if (settlement.outcome === "residue") assert.equal(settlement.bytesLowerBound, outcome === "spawn-throws" ? 0 : 600);
      assert.equal(x.seen.pending, 1, "ticket pending during classification");
      assert.equal(x.seen.entry, 1, "unsettledDeletes entry present during classification");
      assert.equal(x.f.pending(), 0);
      assert.equal(x.internal.unsettledDeletes.size, 0);
      assert.equal(x.f.stopped(), false);
    });
  }

  const unresolved: Array<[string, Parameters<typeof fakeChildDeps>[1]["outcome"], (() => { code: number; stdout: string }) | undefined, Partial<EpochScratchDiscardInput>]> = [
    ["an unreadable post-census", { stdout: "", code: 4 }, () => ({ code: 1, stdout: "" }), {}],
    ["a receipt next to a name that still pins the original", { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 }, () => censusText("leaf-1", "same"), {}],
    ["a root that no longer re-pins", { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 }, () => censusText("leaf-1", "absent"), { rootRepin: () => false }],
    ["a mount change", { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 }, () => censusText("leaf-1", "absent"), { mountsOk: () => false }],
    ["a throwing census", { stdout: "", code: 4 }, () => { throw new Error("census exploded"); }, {}],
  ];
  for (const [label, outcome, census, over] of unresolved) {
    it(`${label} is unresolved: scope stopped, entry and ticket stay pending`, async () => {
      const x = discardFixture(over, census);
      fakeChildDeps(x.internal, { outcome });
      // A post-entry failure is unresolved and is never tagged as not launched.
      await assert.rejects(x.git.discardEpochScratchLeaf(x.input),
        (error: Error & { epochScratchUnresolved?: boolean; epochScratchNotLaunched?: boolean }) =>
          error.epochScratchUnresolved === true && error.epochScratchNotLaunched === undefined);
      assert.equal(x.seen.pending, 1);
      assert.equal(x.f.stopped(), true);
      assert.equal(x.f.pending(), 1);
      assert.equal(x.internal.unsettledDeletes.get(x.key), 1);
    });
  }

  it("a receipt from a child the core reports as failed is never a receipt: unresolved", async () => {
    const x = discardFixture({}, () => censusText("leaf-1", "absent"));
    const child = fakeChildDeps(x.internal, { outcome: { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 } });
    // The receipt and exit 0 are already collected when a late child error marks the launch failed.
    child.stdout.once("end", () => child.emit("error", new Error("late child error")));
    await assert.rejects(x.git.discardEpochScratchLeaf(x.input),
      (error: Error & { epochScratchUnresolved?: boolean }) => error.epochScratchUnresolved === true);
    assert.equal(x.f.stopped(), true);
    assert.equal(x.f.pending(), 1);
    assert.equal(x.internal.unsettledDeletes.get(x.key), 1);
  });

  for (const late of [true, false]) {
    it(`reserves settlement time after the pre-launch observation: ${late ? "late launch refuses without effects" : "a bounded delete leaves the census its full budget"}`, async t => {
      let now = 1_000_000;
      t.mock.method(Date, "now", () => now);
      const x = discardFixture({ deadline: now + 40_000 });
      let launches = 0, censuses = 0;
      x.input.beforeLaunch = async () => {
        x.events.push("beforeLaunch");
        if (late) now += 5_000; // Initially enough time, but the final observation uses the delete allowance.
      };
      t.mock.method(x.internal, "spawnJoinedEpochChild", async (scope: PodEpochReclaimScope, _command: string,
        args: string[], options: Parameters<Internal["spawnJoinedEpochChild"]>[3]) => {
        launches++;
        const budget = Number(args[args.indexOf(PINNED_SUBTREE_SCRIPT) + 4]);
        // Spend the entire child timeout and both 1s join waits, without a real-time wait.
        now += Math.min(options.timeout ?? Infinity, options.deadline - now) + 2_000;
        if (!late) assert.equal(budget, 3_000, "delete leaves 5s kill slack, 2s join allowance and 30s census time");
        return { joined: true as const, spawned: true as const, ticket: scope.beginMutation(),
          code: 0, signal: null, stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, stderr: "", failure: undefined };
      });
      x.input.testSeam!.census = async (_root, _expect, _leaf, timeout) => {
        censuses++;
        // On the unfixed code the child consumes the deadline and this census is starved (1 ms).
        if (timeout < 30_000) return { code: 1, stdout: "" };
        assert.equal(timeout, 30_000);
        return censusText("leaf-1", "absent");
      };
      if (late) {
        await assert.rejects(x.git.discardEpochScratchLeaf(x.input),
          (error: Error & { epochScratchNotLaunched?: boolean }) =>
            error.epochScratchNotLaunched === true && /budget/.test(error.message));
        assert.equal(launches, 0);
        assert.equal(censuses, 0);
        assert.equal(x.f.stopped(), false, "a late refused launch is not a fatal settlement");
      } else {
        assert.deepEqual(await x.git.discardEpochScratchLeaf(x.input), { outcome: "removed", bytes: 1000 });
        assert.equal(launches, 1);
        assert.equal(censuses, 1);
      }
      assert.equal(x.f.pending(), 0);
      assert.equal(x.internal.unsettledDeletes.size, 0);
    });
  }

  it("a settled outcome removes the unsettledDeletes entry before it settles the ticket", async () => {
    const x = discardFixture();
    const begin = x.f.scope.beginMutation.bind(x.f.scope);
    let entryAtSettle: number | undefined = -1;
    x.f.scope.beginMutation = () => {
      const ticket = begin();
      return { settle() { entryAtSettle = x.internal.unsettledDeletes.get(x.key); ticket.settle(); } };
    };
    fakeChildDeps(x.internal, { outcome: { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 } });
    assert.equal((await x.git.discardEpochScratchLeaf(x.input)).outcome, "removed");
    assert.equal(entryAtSettle, undefined, "entry already removed when the ticket settles");
    assert.equal(x.f.pending(), 0);
  });

  it("an unconfirmed join is unresolved without a census: scope stopped, entry and ticket pending", async () => {
    const x = discardFixture();
    fakeChildDeps(x.internal, { outcome: "hang", joinMs: 30 });
    const stop = new AbortController();
    x.input.signal = stop.signal;
    const timer = setTimeout(() => stop.abort(), 5);
    try { await assert.rejects(x.git.discardEpochScratchLeaf(x.input),
      (error: Error & { epochScratchUnresolved?: boolean; epochScratchNotLaunched?: boolean; cause?: { epochProcessJoined?: boolean } }) =>
        error.epochScratchUnresolved === true && error.epochScratchNotLaunched === undefined &&
        error.cause?.epochProcessJoined === false); }
    finally { clearTimeout(timer); }
    assert.deepEqual(x.events, ["beforeLaunch"]);
    assert.equal(x.f.stopped(), true);
    assert.equal(x.f.pending(), 1);
    assert.equal(x.internal.unsettledDeletes.get(x.key), 1);
  });

  it("a stopped scope at classification is unresolved even with a clean receipt", async () => {
    const x = discardFixture();
    fakeChildDeps(x.internal, { outcome: { stdout: `UZI-SCRATCH-REMOVED ${EXPECT}\n`, code: 0 } });
    x.input.testSeam!.onClassify = () => x.f.scope.stop();
    await assert.rejects(x.git.discardEpochScratchLeaf(x.input), /unresolved/);
    assert.equal(x.f.pending(), 1);
    assert.equal(x.internal.unsettledDeletes.get(x.key), 1);
  });

  describe("with the real helper and the real census", { skip: fs.existsSync("/proc/self/fd") ? false : NO_PROC_FD }, () => {
    async function realFixture() {
      const { dir, root, leaf } = await fixtureTree();
      const f = fakeScope({ realStdio: true });
      const git = new GitCache(path.join(os.tmpdir(), "cache"), nullLogger());
      const rootExpect = devIno(root), expect = devIno(leaf);
      const input: EpochScratchDiscardInput = {
        scope: f.scope, root: "/data/runner/.tmp", name: "leaf", expect, rootExpect, bytes: 12345,
        signal: new AbortController().signal, deadline: Date.now() + 60_000,
        beforeLaunch: async () => undefined, rootRepin: () => true, mountsOk: () => true,
        testSeam: { token: SEAM, root },
      };
      return { dir, root, leaf, f, git, input, internal: git as unknown as Internal };
    }

    it("removes a tree and settles removed", async () => {
      const x = await realFixture();
      try {
        const settlement = await x.git.discardEpochScratchLeaf(x.input);
        assert.deepEqual(settlement, { outcome: "removed", bytes: 12345 });
        assert.equal(await exists(x.leaf), false);
        assert.equal(x.f.pending(), 0);
        assert.equal(x.internal.unsettledDeletes.size, 0);
      } finally { await cleanup(x.dir); }
    });

    it("a descendant on another device settles residue retained through discardEpochScratchLeaf; the leaf stays", async () => {
      const x = await realFixture();
      const syncDir = await mktmp();
      try {
        const sync = path.join(syncDir, "sync");
        await fsp.writeFile(`${sync}.otherdev`, "sub");
        await fsp.writeFile(`${sync}.go`, "");
        // The launch validator has already admitted the fixed argv (sync ""); only the spawn is rewritten.
        let closed = false;
        x.internal.epochNativeTestDependencies = {
          spawn: ((command: string, args: string[], options: Parameters<typeof spawn>[2]) => {
            const patched = [...args];
            patched[patched.indexOf(PINNED_SUBTREE_SCRIPT) + 7] = sync;
            const child = spawn(command, patched, options);
            child.once("close", () => { closed = true; });
            return child;
          }) as unknown as typeof spawn,
          alive: () => !closed, kill: () => undefined, joinMs: 2000,
        };
        const settlement = await x.git.discardEpochScratchLeaf(x.input);
        assert.equal(settlement.outcome, "residue");
        if (settlement.outcome === "residue") { assert.equal(settlement.kind, "retained"); assert.equal(settlement.stopPhase, false); }
        assert.equal(await exists(path.join(x.leaf, "sub", "b.txt")), true);
        assert.equal(await exists(x.leaf), true);
        assert.equal(x.f.pending(), 0);
        assert.equal(x.internal.unsettledDeletes.size, 0);
      } finally { await cleanup(syncDir); await cleanup(x.dir); }
    });

    it("a leaf it cannot empty settles residue", { skip: asRoot ? "root bypasses directory modes" : false }, async () => {
      const x = await realFixture();
      try {
        await fsp.chmod(path.join(x.leaf, "sub"), 0o555);
        const settlement = await x.git.discardEpochScratchLeaf(x.input);
        assert.equal(settlement.outcome, "residue");
        if (settlement.outcome === "residue") { assert.equal(settlement.kind, "retained"); assert.equal(settlement.stopPhase, false); }
        assert.equal(await exists(path.join(x.leaf, "sub", "b.txt")), true);
        assert.equal(x.f.pending(), 0);
        assert.equal(x.internal.unsettledDeletes.size, 0);
      } finally { await cleanup(x.dir); }
    });
  });
});
