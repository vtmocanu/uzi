// Issue #4263 M3: the retire-scratch `empty-fd` helper mode, its launch validator and
// GitCache.discardRetireScratchParent, all through the real helper script. The uid split is simulated:
// the fake spawn strips the setpriv wrapper and runs the helper as the test's own uid (single-uid), so the
// cross-uid proof (runner-owned clone under a worker-owned 0700 parent) belongs to the e2e milestone.
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { spawn, type ChildProcess, type StdioOptions } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { GitCache, type JoinedEpochChild, type RetireScratchDiscardInput } from "../src/git.js";
import { PINNED_SUBTREE_SCRIPT, restoreTreeWritability } from "../src/rmtree.js";
import { COMMAND_UID, commandRootCommand, runnerCommand, setprivArgsForUid } from "../src/runner-uid.js";
import { nullLogger } from "./helpers.js";

const asRoot = process.getuid?.() === 0;
const PIN = 0o10000000 | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
const UUID = "0b9f4a52-3c1d-4e8a-9b7e-1f2a3b4c5d6e";
const PARENT = `.retire-${UUID}`;

const tmpDirs: string[] = [];
const openFds: number[] = [];
afterEach(async () => {
  for (const fd of openFds.splice(0)) { try { fs.closeSync(fd); } catch { /* already closed */ } }
  delete process.env.UZI_UID_SPLIT;
  for (const d of tmpDirs.splice(0)) {
    await restoreTreeWritability(d).catch(() => undefined);
    await fsp.rm(d, { recursive: true, force: true }).catch(() => undefined);
  }
});
async function mktmp(): Promise<string> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-retire-discard-"));
  tmpDirs.push(d);
  return d;
}
const exists = (p: string): boolean => { try { fs.lstatSync(p); return true; } catch { return false; } };
function devIno(p: string): string { const st = fs.lstatSync(p, { bigint: true }); return `${st.dev}:${st.ino}`; }
function pin(p: string): number { const fd = fs.openSync(p, PIN); openFds.push(fd); return fd; }

// ---- the helper, spawned directly with the directory as fd 3 ---------------------------------------

interface Run { code: number | null; stdout: string }
function emptyFdArgs(expect: string, o: { home?: string; rel?: string; max?: string; budget?: string; mode?: string; sync?: string;
  phase?: string; rootExpect?: string; oneFs?: string; depth?: string } = {}): string[] {
  return [o.home ?? "fd:3", o.rel ?? "-", o.max ?? "2000000", o.budget ?? "60000", o.mode ?? "empty-fd", expect, o.sync ?? "",
    o.phase ?? "pinned", o.rootExpect ?? "-", o.oneFs ?? "1", o.depth ?? "256"];
}
function runHelper(args: string[], fd3: number | "ignore"): Promise<Run> {
  const child = spawn(process.execPath, ["-e", PINNED_SUBTREE_SCRIPT, ...args], { cwd: "/", stdio: ["ignore", "pipe", "ignore", fd3] });
  let stdout = "";
  child.stdout!.on("data", (b: Buffer) => { stdout += b.toString(); });
  return new Promise(resolve => child.on("close", code => resolve({ code, stdout })));
}
async function tree(): Promise<{ dir: string; home: string; outside: string }> {
  const dir = await mktmp();
  const home = path.join(dir, "home");
  const outside = path.join(dir, "outside");
  await fsp.mkdir(path.join(home, "sub", "deeper"), { recursive: true });
  await fsp.mkdir(outside);
  await fsp.writeFile(path.join(outside, "keep.txt"), "keep");
  await fsp.writeFile(path.join(home, "a.txt"), "a");
  await fsp.writeFile(path.join(home, "sub", "b.txt"), "b");
  await fsp.writeFile(path.join(home, "sub", "deeper", "c.txt"), "c");
  return { dir, home, outside };
}

describe("PINNED_SUBTREE_SCRIPT empty-fd mode", { skip: !fs.existsSync("/proc/self/fd") }, () => {
  it("empties a nested tree through fd 3, keeps the directory itself, and prints the receipt", async () => {
    const t = await tree();
    const expect = devIno(t.home);
    const run = await runHelper(emptyFdArgs(expect), pin(t.home));
    assert.equal(run.code, 0);
    assert.equal(run.stdout, `UZI-SCRATCH-EMPTIED ${expect}\n`);
    assert.deepEqual(fs.readdirSync(t.home), []);
    assert.equal(devIno(t.home), expect);
  });

  it("an identity mismatch exits 3 with nothing touched and no receipt", async () => {
    const t = await tree();
    const run = await runHelper(emptyFdArgs("1:2"), pin(t.home));
    assert.equal(run.code, 3);
    assert.equal(run.stdout, "");
    assert.ok(exists(path.join(t.home, "sub", "deeper", "c.txt")));
  });

  it("a non-directory fd 3 exits 3", async () => {
    const t = await tree();
    const file = path.join(t.dir, "file");
    fs.writeFileSync(file, "x");
    const fd = fs.openSync(file, fs.constants.O_RDONLY); openFds.push(fd);
    const run = await runHelper(emptyFdArgs(devIno(file)), fd);
    assert.equal(run.code, 3);
  });

  it("a symlink inside is unlinked and its target outside is never touched", async () => {
    const t = await tree();
    fs.symlinkSync(t.outside, path.join(t.home, "sub", "link-dir"));
    fs.symlinkSync(path.join(t.outside, "keep.txt"), path.join(t.home, "link-file"));
    const run = await runHelper(emptyFdArgs(devIno(t.home)), pin(t.home));
    assert.equal(run.code, 0);
    assert.deepEqual(fs.readdirSync(t.home), []);
    assert.equal(fs.readFileSync(path.join(t.outside, "keep.txt"), "utf8"), "keep");
  });

  it("the other-device seam skips that descendant and exits 9 without a receipt", async () => {
    const t = await tree();
    const sync = path.join(t.dir, "sync");
    fs.writeFileSync(`${sync}.go`, "");
    fs.writeFileSync(`${sync}.otherdev`, "sub");
    const run = await runHelper(emptyFdArgs(devIno(t.home), { sync, phase: "never" }), pin(t.home));
    assert.equal(run.code, 9);
    assert.equal(run.stdout, "");
    assert.ok(exists(path.join(t.home, "sub", "b.txt")));
    assert.ok(!exists(path.join(t.home, "a.txt")));
  });

  it("the depth and entry caps exit 8 and keep the rest", async () => {
    const t = await tree();
    const shallow = await runHelper(emptyFdArgs(devIno(t.home), { depth: "1" }), pin(t.home));
    assert.equal(shallow.code, 8);
    assert.equal(shallow.stdout, "");
    assert.ok(exists(path.join(t.home, "sub")));
    const flood = await mktmp();
    for (let i = 0; i < 40; i++) fs.writeFileSync(path.join(flood, `f${i}`), "");
    const capped = await runHelper(emptyFdArgs(devIno(flood), { max: "5" }), pin(flood));
    assert.equal(capped.code, 8);
    assert.ok(fs.readdirSync(flood).length > 0);
  });

  it("the time budget exits 8", async () => {
    const flood = await mktmp();
    for (let i = 0; i < 4000; i++) fs.writeFileSync(path.join(flood, `f${i}`), "");
    const run = await runHelper(emptyFdArgs(devIno(flood), { budget: "0" }), pin(flood));
    assert.equal(run.code, 8);
    assert.equal(run.stdout, "");
  });

  it("a 0500 subdirectory is never chmodded: it stays 0500, its content stays, exit 4", { skip: asRoot }, async () => {
    const t = await tree();
    const locked = path.join(t.home, "locked");
    fs.mkdirSync(locked);
    fs.writeFileSync(path.join(locked, "inside.txt"), "x");
    fs.chmodSync(locked, 0o500);
    const run = await runHelper(emptyFdArgs(devIno(t.home)), pin(t.home));
    assert.equal(run.code, 4);
    assert.equal(run.stdout, "");
    assert.equal(fs.statSync(locked).mode & 0o7777, 0o500);
    assert.ok(exists(path.join(locked, "inside.txt")));
  });

  it("every malformed empty-fd shape exits 6 and touches nothing", async () => {
    const t = await tree();
    const expect = devIno(t.home);
    const bad: Array<[string, string[]]> = [
      ["home is a path", emptyFdArgs(expect, { home: t.home })],
      ["home is fd:4", emptyFdArgs(expect, { home: "fd:4" })],
      ["rel is a name", emptyFdArgs(expect, { rel: "home" })],
      ["rootExpect given", emptyFdArgs(expect, { rootExpect: expect })],
      ["not strict", emptyFdArgs(expect, { oneFs: "" })],
      ["expect is -", emptyFdArgs("-")],
      ["expect is malformed", emptyFdArgs("7")],
      ["depth is not an integer", emptyFdArgs(expect, { depth: "x" })],
      ["fd:3 with mode remove", emptyFdArgs(expect, { mode: "remove" })],
      ["fd:3 with mode empty", emptyFdArgs(expect, { mode: "empty" })],
      ["unknown mode", emptyFdArgs(expect, { mode: "empty-fd2" })],
    ];
    for (const [label, args] of bad) {
      const run = await runHelper(args, pin(t.home));
      assert.equal(run.code, 6, label);
      assert.equal(run.stdout, "", label);
    }
    assert.ok(exists(path.join(t.home, "sub", "deeper", "c.txt")));
  });

  it("the earlier strict remove grammar is unchanged: a path home still removes the leaf", async () => {
    const t = await tree();
    const root = path.join(t.dir, "root");
    fs.mkdirSync(path.join(root, "leaf", "x"), { recursive: true });
    fs.writeFileSync(path.join(root, "leaf", "x", "y"), "y");
    const leafId = devIno(path.join(root, "leaf"));
    const child = spawn(process.execPath, ["-e", PINNED_SUBTREE_SCRIPT, root, "leaf", "2000000", "60000", "remove", leafId, "",
      "pinned", devIno(root), "1", "256"], { cwd: "/", stdio: ["ignore", "pipe", "ignore"] });
    let stdout = "";
    child.stdout!.on("data", (b: Buffer) => { stdout += b.toString(); });
    const code = await new Promise(resolve => child.on("close", resolve));
    assert.equal(code, 0);
    assert.equal(stdout, `UZI-SCRATCH-REMOVED ${leafId}\n`);
    assert.ok(!exists(path.join(root, "leaf")));
  });
});

// ---- the launch validator ---------------------------------------------------------------------------

type Identity = "command" | "worker_pat";
interface Internal {
  validateEpochRetireEmptyLaunch(command: string, args: readonly string[], env: NodeJS.ProcessEnv, cwd: string | undefined, identity: Identity): void;
  spawnJoinedEpochChild(scope: PodEpochReclaimScope, command: string, args: string[], options: {
    env: NodeJS.ProcessEnv; identity: Identity; signals: readonly AbortSignal[]; deadline: number; timeout?: number; extraFd?: number;
  }): Promise<JoinedEpochChild>;
  epochNativeTestDependencies?: {
    spawn: typeof spawn; alive: (pid: number, identity: Identity) => boolean; kill: (pid: number, identity: Identity) => void; joinMs: number;
  };
  unsettledDeletes: Map<string, number>;
}
const newGit = () => new GitCache(path.join(os.tmpdir(), "uzi-retire-cache"), nullLogger());

const goodTail = (over: Record<number, string> = {}): string[] => {
  const tail = ["-e", PINNED_SUBTREE_SCRIPT, "fd:3", "-", "2000000", "60000", "empty-fd", "7:5", "", "pinned", "-", "1", "256"];
  for (const [i, v] of Object.entries(over)) tail[Number(i)] = v;
  return tail;
};
const ENV = { PATH: "/usr/bin:/bin" };

describe("validateEpochRetireEmptyLaunch", () => {
  const validate = (command: string, args: readonly string[], env: NodeJS.ProcessEnv = ENV, cwd: string | undefined = "/",
    identity: Identity = "command") =>
    (newGit() as unknown as Internal).validateEpochRetireEmptyLaunch(command, args, env, cwd, identity);

  it("accepts exactly the runnerCommand-wrapped empty-fd argv under the split", () => {
    process.env.UZI_UID_SPLIT = "1";
    const ok = runnerCommand(process.execPath, goodTail());
    assert.doesNotThrow(() => validate(ok.command, ok.args));
    for (const [i, v] of [[4, "1"], [4, "2000000"], [5, "1000"], [5, "300000"], [12, "1"]] as const) {
      const w = runnerCommand(process.execPath, goodTail({ [i]: v }));
      assert.doesNotThrow(() => validate(w.command, w.args), `${i}=${v}`);
    }
  });

  it("refuses when the uid split is inactive (no unwrapped worker-run empty-fd)", () => {
    delete process.env.UZI_UID_SPLIT;
    const unwrapped = runnerCommand(process.execPath, goodTail());
    assert.equal(unwrapped.command, process.execPath);
    assert.throws(() => validate(unwrapped.command, unwrapped.args), /unaudited/);
    const wrapped = runnerCommand(process.execPath, goodTail(), true);
    assert.throws(() => validate(wrapped.command, wrapped.args), /unaudited/);
  });

  it("mutation pin: no wrapper other than runnerCommand is accepted (command-root, worker, double, none)", () => {
    process.env.UZI_UID_SPLIT = "1";
    const cmdRoot = commandRootCommand(process.execPath, goodTail());
    assert.ok(cmdRoot.args.includes(String(COMMAND_UID)), "the command-root wrapper really targets runner-cmd");
    const refusals: Array<[string, { command: string; args: string[] }]> = [
      ["command-root wrapper", cmdRoot],
      ["command-root setpriv argv", { command: runnerCommand("x", [], true).command, args: [...setprivArgsForUid(COMMAND_UID), process.execPath, ...goodTail()] }],
      ["no wrapper (worker identity)", { command: process.execPath, args: goodTail() }],
      ["double runner pass", runnerCommand(runnerCommand(process.execPath, goodTail()).command, runnerCommand(process.execPath, goodTail()).args)],
    ];
    for (const [label, w] of refusals) assert.throws(() => validate(w.command, w.args), /unaudited/, label);
  });

  it("refuses every other deviation", () => {
    process.env.UZI_UID_SPLIT = "1";
    const w = (over: Record<number, string>) => runnerCommand(process.execPath, goodTail(over));
    const cases: Array<[string, { command: string; args: string[] }]> = [
      ["other script", w({ 1: "process.exit(0)" })],
      ["home is a path", w({ 2: "/data/runner" })],
      ["rel is a name", w({ 3: "clone" })],
      ["max zero", w({ 4: "0" })],
      ["max over the cap", w({ 4: "2000001" })],
      ["max not canonical", w({ 4: "02000" })],
      ["budget under the minimum", w({ 5: "999" })],
      ["budget over the cap", w({ 5: "300001" })],
      ["mode remove", w({ 6: "remove" })],
      ["mode empty", w({ 6: "empty" })],
      ["expect malformed", w({ 7: "7:5:1" })],
      ["sync seam", w({ 8: "/tmp/x" })],
      ["phase", w({ 9: "listed" })],
      ["rootExpect", w({ 10: "7:2" })],
      ["not strict", w({ 11: "" })],
      ["depth over the cap", w({ 12: "257" })],
      ["depth zero", w({ 12: "0" })],
    ];
    for (const [label, c] of cases) assert.throws(() => validate(c.command, c.args), /unaudited/, label);
    const ok = w({});
    assert.throws(() => validate(ok.command, [...ok.args, "extra"]), /unaudited/, "extra arg");
    assert.throws(() => validate(ok.command, ok.args.slice(0, -1)), /unaudited/, "short argv");
    assert.throws(() => validate(ok.command, ok.args, { PATH: "/usr/bin:/bin", X: "1" }), /unaudited/, "extra env");
    assert.throws(() => validate(ok.command, ok.args, { PATH: "/bin" }), /unaudited/, "other PATH");
    assert.throws(() => validate(ok.command, ok.args, ENV, "/tmp"), /unaudited/, "cwd");
    assert.throws(() => validate(ok.command, ok.args, ENV, "/", "worker_pat"), /unaudited/, "identity");
  });

  it("mutation pin: the empty-fd route contains no chmod, chown or alternate wrapper", () => {
    const source = fs.readFileSync(new URL("../src/git.ts", import.meta.url), "utf8");
    const start = source.indexOf("private validateEpochRetireEmptyLaunch(");
    const end = source.indexOf("Registers a copied, immutable binding");
    assert.ok(start > 0 && end > start, "route located");
    const route = source.slice(start, end);
    for (const banned of [/chmod/i, /chown/i, /commandRootCommand/, /workerBoundaryCommand/, /asWorker/, /fchmod/i, /RETENTION_RM_BIN/])
      assert.doesNotMatch(route.replace(/\/\*[\s\S]*?\*\/|\/\/[^\n]*/g, ""), banned);
    assert.equal((route.match(/runnerCommand\(/g) ?? []).length, 2, "exactly the validator reference and the one launch pass");
  });
});

// ---- discardRetireScratchParent ----------------------------------------------------------------------

/** A scope whose EX lock is a real descriptor, so the child fd table can be inspected. */
function realScope() {
  let pending = 0, stopped = false;
  const exFile = path.join(os.tmpdir(), `uzi-retire-ex-${process.pid}-${Math.random().toString(36).slice(2)}`);
  fs.writeFileSync(exFile, "");
  const exFd = fs.openSync(exFile, fs.constants.O_RDONLY);
  openFds.push(exFd);
  const scope: PodEpochReclaimScope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() { pending++; let done = false; return { settle() { if (!done) { done = true; pending--; } } }; },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { assert.ok(pending > 0); return [...stdio, exFd] as StdioOptions as never; },
    stop() { stopped = true; }, stopped() { return stopped; },
    assertSettled() { assert.equal(pending, 0); },
    finish() { this.assertSettled(); },
  };
  return { scope, pending: () => pending, stopped: () => stopped, exId: devIno(exFile) };
}

type FakeOutcome = "real" | "receipt-only" | "hang";
/** Replaces the spawn: strips the setpriv wrapper and runs the helper as this uid, or fakes a result. */
function installSpawn(git: GitCache, outcome: FakeOutcome, calls: Array<{ command: string; args: string[]; stdio: unknown }>) {
  const internal = git as unknown as Internal;
  const wrapLen = runnerCommand(process.execPath, [], true).args.length;
  internal.epochNativeTestDependencies = {
    spawn: ((command: string, args: string[], options: { stdio: StdioOptions; env: NodeJS.ProcessEnv; cwd?: string; detached: boolean }) => {
      calls.push({ command, args, stdio: options.stdio });
      if (outcome === "real") return spawn(process.execPath, args.slice(wrapLen), options);
      const child = Object.assign(new EventEmitter(), {
        pid: 4242, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), exitCode: null, signalCode: null,
      });
      if (outcome === "receipt-only") {
        const expect = args[wrapLen + 7];
        setImmediate(() => {
          child.stdout.end(`UZI-SCRATCH-EMPTIED ${expect}\n`); child.stderr.end("secret-looking stderr text");
          child.emit("exit", 0, null); child.emit("close", 0, null);
        });
      }
      return child as unknown as ChildProcess;
    }) as unknown as typeof spawn,
    alive: (pid: number) => { if (outcome !== "real") return outcome === "hang"; try { process.kill(-pid, 0); return true; } catch { return false; } },
    kill: (pid: number) => { if (outcome === "real") { try { process.kill(-pid, "SIGKILL"); } catch { /* gone */ } } },
    joinMs: 60,
  };
}

async function retireFixture(o: { outcome?: FakeOutcome; populate?: (clone: string) => void } = {}) {
  process.env.UZI_UID_SPLIT = "1";
  const root = await mktmp();
  const parent = path.join(root, PARENT);
  const clone = path.join(parent, "clone");
  fs.mkdirSync(path.join(clone, "src", "deep"), { recursive: true });
  fs.writeFileSync(path.join(clone, "src", "deep", "f.txt"), "f");
  fs.writeFileSync(path.join(clone, "top.txt"), "t");
  o.populate?.(clone);
  fs.chmodSync(parent, 0o700);
  const rootPin = pin(root);
  const f = realScope();
  const git = newGit();
  const calls: Array<{ command: string; args: string[]; stdio: unknown }> = [];
  installSpawn(git, o.outcome ?? "real", calls);
  const input: RetireScratchDiscardInput = {
    scope: f.scope, root, rootPin, rootDev: String(fs.lstatSync(root, { bigint: true }).dev), parentName: PARENT,
    parentIno: String(fs.lstatSync(parent, { bigint: true }).ino), cloneIno: String(fs.lstatSync(clone, { bigint: true }).ino),
    deadline: Date.now() + 120_000, maxEntries: 2_000_000, maxDepth: 256, signal: new AbortController().signal,
    live: () => undefined, beforeEffect: async () => undefined,
  };
  return { root, parent, clone, f, git, internal: git as unknown as Internal, calls, input };
}

describe("GitCache.discardRetireScratchParent", { skip: !fs.existsSync("/proc/self/fd") }, () => {
  it("removes a populated clone and its parent through the real helper, then settles", async () => {
    const t = await retireFixture();
    const result = await t.git.discardRetireScratchParent(t.input);
    assert.deepEqual(result, { kind: "removed" });
    assert.ok(!exists(t.parent));
    assert.equal(t.calls.length, 1);
    assert.equal(t.f.pending(), 0);
    assert.equal(t.f.stopped(), false);
    assert.doesNotThrow(() => t.git.assertNoUnsettledDeletes());
    // the single launch is the runner wrapper around the audited empty-fd argv
    assert.equal(t.calls[0]!.command, runnerCommand(process.execPath, [], true).command);
    assert.ok(t.calls[0]!.args.includes("empty-fd"));
  });

  it("every preflight refusal leaves the tree intact and launches nothing", async () => {
    type Mut = (t: Awaited<ReturnType<typeof retireFixture>>) => Partial<RetireScratchDiscardInput> | void;
    const outside = await mktmp();
    fs.writeFileSync(path.join(outside, "keep"), "k");
    const cases: Array<[string, string, Mut]> = [
      ["wrong parent inode", "identity_changed", t => ({ parentIno: String(BigInt(t.input.parentIno) + 1n) })],
      ["wrong root dev", "identity_changed", t => ({ rootDev: String(BigInt(t.input.rootDev) + 1n) })],
      ["wrong clone inode", "clone_identity_changed", t => ({ cloneIno: String(BigInt(t.input.cloneIno) + 1n) })],
      ["parent mode 0750", "wrong_mode", t => { fs.chmodSync(t.parent, 0o750); }],
      ["parent mode 0700 plus sticky", "wrong_mode", t => { fs.chmodSync(t.parent, 0o1700); }],
      ["extra entry beside clone", "unexpected_entries", t => { fs.writeFileSync(path.join(t.parent, "other"), ""); }],
      ["clone missing", "unexpected_entries", t => { fs.renameSync(t.clone, path.join(t.root, "moved")); }],
      ["clone replaced by a symlink", "not_directory", t => {
        fs.renameSync(t.clone, path.join(t.root, "moved")); fs.symlinkSync(outside, t.clone);
      }],
      ["parent replaced by a symlink", "not_directory", t => {
        fs.renameSync(t.parent, path.join(t.root, "moved")); fs.symlinkSync(outside, t.parent);
      }],
      ["parent missing", "parent_missing", t => { fs.renameSync(t.parent, path.join(t.root, "moved")); }],
      ["parent is a file", "not_directory", t => { fs.renameSync(t.parent, path.join(t.root, "moved")); fs.writeFileSync(t.parent, ""); }],
      ["name outside the key grammar", "invalid_input", () => ({ parentName: ".retire-not-a-uuid" })],
      ["name with a separator", "invalid_input", () => ({ parentName: `../${PARENT}` })],
      ["not live", "not_live", () => ({ live: () => { throw new Error("gone"); } })],
      ["beforeEffect denies", "not_live", () => ({ beforeEffect: async () => { throw new Error("no"); } })],
      ["aborted", "not_live", () => { const c = new AbortController(); c.abort(); return { signal: c.signal }; }],
      ["deadline spent", "deadline", () => ({ deadline: Date.now() - 1 })],
      ["deadline too close", "budget_exhausted", () => ({ deadline: Date.now() + 3000 })],
      ["bad limits", "invalid_input", () => ({ maxDepth: 0 })],
    ];
    for (const [label, reason, mutate] of cases) {
      if (label.startsWith("parent mode") && asRoot) continue;
      const t = await retireFixture();
      const patch = mutate(t) ?? {};
      const before = fs.readdirSync(t.root).sort();
      const result = await t.git.discardRetireScratchParent({ ...t.input, ...patch });
      assert.deepEqual(result, { kind: "refused", reason }, label);
      assert.equal(t.calls.length, 0, `${label}: no launch`);
      assert.equal(t.f.pending(), 0, label);
      assert.deepEqual(fs.readdirSync(t.root).sort(), before, label);
      assert.ok(t.internal.unsettledDeletes.size === 0, label);
    }
    assert.equal(fs.readFileSync(path.join(outside, "keep"), "utf8"), "k");
  });

  it("refuses with nothing touched when the uid split is inactive", async () => {
    const t = await retireFixture();
    delete process.env.UZI_UID_SPLIT;
    const result = await t.git.discardRetireScratchParent(t.input);
    assert.deepEqual(result, { kind: "refused", reason: "split_inactive" });
    assert.equal(t.calls.length, 0);
    assert.ok(exists(path.join(t.clone, "top.txt")));
  });

  it("a helper that leaves content behind is residue and the parent is kept", { skip: asRoot }, async () => {
    const t = await retireFixture({ populate: clone => {
      fs.mkdirSync(path.join(clone, "locked"));
      fs.writeFileSync(path.join(clone, "locked", "inside"), "x");
      fs.chmodSync(path.join(clone, "locked"), 0o500);
    } });
    const result = await t.git.discardRetireScratchParent(t.input);
    assert.equal(result.kind, "residue");
    if (result.kind !== "residue") return;
    assert.equal(result.reason, "helper_remaining");
    assert.equal(result.exit, 4);
    assert.ok(exists(path.join(t.clone, "locked", "inside")));
    assert.equal(fs.statSync(path.join(t.clone, "locked")).mode & 0o7777, 0o500);
    assert.ok(!exists(path.join(t.clone, "top.txt")), "the removable part was emptied");
    assert.equal(t.f.pending(), 0);
    assert.doesNotThrow(() => t.git.assertNoUnsettledDeletes());
  });

  it("a receipt is not emptiness proof: the worker's rmdir fails on a non-empty clone and the bytes survive", async () => {
    const t = await retireFixture({ outcome: "receipt-only" });
    const result = await t.git.discardRetireScratchParent(t.input);
    assert.equal(result.kind, "residue");
    if (result.kind !== "residue") return;
    assert.equal(result.reason, "rmdir_clone_failed");
    assert.equal(result.exit, 0);
    // only a bounded length of the helper's stderr is reported, never its text
    assert.equal(result.stderrBytes, "secret-looking stderr text".length);
    assert.ok(!JSON.stringify(result).includes("secret-looking"));
    assert.ok(exists(path.join(t.clone, "top.txt")));
    assert.ok(exists(t.parent));
  });

  it("a swapped clone name after the helper keeps everything (residue)", async () => {
    const t = await retireFixture({ outcome: "receipt-only" });
    const input = { ...t.input, live: (() => {
      let calls = 0;
      return () => { if (++calls === 3) { fs.renameSync(t.clone, path.join(t.root, "moved")); fs.mkdirSync(t.clone); } };
    })() };
    const result = await t.git.discardRetireScratchParent(input);
    assert.equal(result.kind, "residue");
    if (result.kind === "residue") assert.equal(result.reason, "clone_identity_changed");
    assert.ok(exists(path.join(t.root, "moved", "top.txt")));
    assert.ok(exists(t.parent));
  });

  it("an unjoined child stops the scope, leaves the unsettled entry and throws epochScratchUnresolved", async () => {
    const t = await retireFixture({ outcome: "hang" });
    await assert.rejects(t.git.discardRetireScratchParent({ ...t.input, deadline: Date.now() + 6500 }),
      (e: Error & { epochScratchUnresolved?: boolean }) => e.epochScratchUnresolved === true);
    assert.equal(t.f.stopped(), true);
    assert.equal(t.f.pending(), 1);
    assert.throws(() => t.git.assertNoUnsettledDeletes(), /unsettled runner delete/);
    assert.ok(exists(path.join(t.clone, "top.txt")));
  });

  it("the child fd table is stdio 0..2, the clone pin at fd 3 and the EX lock at fd 4, nothing else", async () => {
    const t = await retireFixture();
    const probe = [
      'const fs=require("node:fs");const out={};',
      'for (const fd of [3,4]) { const s=fs.fstatSync(fd,{bigint:true}); out[fd]=s.dev+":"+s.ino+(s.isDirectory()?":dir":":file"); }',
      'out.extra=[];for (const name of fs.readdirSync("/proc/self/fd")) { const n=Number(name); if (n<5) continue;',
      ' let target="";try{target=fs.readlinkSync("/proc/self/fd/"+name);}catch{continue;}',
      ' if (target.startsWith("/") && !target.startsWith("/dev/") && !target.startsWith("/proc/")) out.extra.push(target); }',
      'process.stdout.write(JSON.stringify(out));',
    ].join("\n");
    const clonePin = pin(t.clone);
    const calls: unknown[] = [];
    const internal = t.internal;
    internal.epochNativeTestDependencies = {
      spawn: ((_c: string, _a: string[], options: unknown) => { calls.push(options); return spawn(process.execPath, ["-e", probe], options as never); }) as typeof spawn,
      alive: (pid: number) => { try { process.kill(-pid, 0); return true; } catch { return false; } },
      kill: () => undefined, joinMs: 60,
    };
    const joined = await internal.spawnJoinedEpochChild(t.f.scope, "x", [], {
      env: ENV, identity: "command", signals: [], deadline: Date.now() + 20_000, extraFd: clonePin,
    });
    assert.ok(joined.spawned);
    if (!joined.spawned) return;
    joined.ticket.settle();
    const seen = JSON.parse(joined.stdout) as Record<string, unknown>;
    assert.equal(seen["3"], `${devIno(t.clone)}:dir`);
    assert.equal(seen["4"], `${t.f.exId}:file`);
    assert.deepEqual(seen.extra, []);
    assert.deepEqual((calls[0] as { stdio: unknown[] }).stdio, ["pipe", "pipe", "pipe", clonePin, (calls[0] as { stdio: unknown[] }).stdio[4]]);
    assert.equal((calls[0] as { stdio: unknown[] }).stdio.length, 5);
  });

  it("existing callers keep the four-descriptor stdio (no extra fd without the option)", async () => {
    const t = await retireFixture();
    const seen: unknown[][] = [];
    t.internal.epochNativeTestDependencies = {
      spawn: ((_c: string, _a: string[], options: { stdio: unknown[] }) => { seen.push(options.stdio); return spawn(process.execPath, ["-e", "0"], options as never); }) as typeof spawn,
      alive: (pid: number) => { try { process.kill(-pid, 0); return true; } catch { return false; } },
      kill: () => undefined, joinMs: 60,
    };
    const joined = await t.internal.spawnJoinedEpochChild(t.f.scope, "x", [], { env: ENV, identity: "command", signals: [], deadline: Date.now() + 20_000 });
    if (joined.spawned || joined.joined) joined.ticket.settle();
    assert.equal(seen[0]!.length, 4);
  });
});
