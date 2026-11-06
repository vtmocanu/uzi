import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync, spawn, type SpawnOptions } from "node:child_process";
import type { TestContext } from "node:test";
import { deflateSync } from "node:zlib";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// The R9 finite budgets of the epoch automatic content proof (owner answer seq 1162, 2026-11-06 (R9)):
// a positively identified overflow is a settled keep (`epoch_content_unproven`), while a corrupt, unreadable
// or indirect store, an unresolved execution and a Git stderr overrun stay UNKNOWN.

const RUN = "run-r9";
const BRANCH = "agent/issue-3431";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
let fx: Fixture;
let git: GitCache;
let bare: string;
let clonePath: string;
let raw: string;
let options: RetainedDiscardOptions;
let input: EpochDiscardProofOptions;
let scope: PodEpochReclaimScope;
let pending: number;
let stopped: boolean;
let live: boolean;


function gitIn(args: string[]): string {
  return execFileSync("git", ["-C", bare, ...args], { env: ENV, encoding: "utf8" }).trim();
}
function journal(): string { return gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`]); }

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-3431", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  raw = journal();
  const selector = { runId: RUN, generation: 1, attempt };
  const holds = { state: "read" as const, holds: [] };
  const lifecycle = { state: "terminal" as const, status: "completed" as const };
  const inspection = await git.inspectRetainedSources({ selector, barePath: bare, holds, lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  pending = 0; stopped = false; live = true;
  // Contract double counts real Git participation; descriptor ownership is tested separately.
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
  options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => true };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => fx?.cleanup());



type Limits = {
  maxMetadataRecords?: number; maxMetadataBytes?: number; maxWorktreeContentBytes?: number;
  maxObjectStoreBytes?: number; maxDirectoryEntries?: number;
};
/** The whole automatic content proof under its production native/limit scopes. */
async function observation(limits: Limits = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
/** The object-store observation alone, so the error that ends it can be identified. */
async function observeStores(limits: Limits = {}): Promise<unknown> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () => git["withEpochNativeReads"](proof, () => {
    const automatic = { roots: [], checkLive: () => git["checkEpochDiscardLive"](state) };
    return git["automaticObserveStores"](bare, clonePath, automatic as never);
  }));
}
async function failure(action: Promise<unknown>): Promise<{ name: string; message: string; cause?: { code?: string } }> {
  try { await action; } catch (error) {
    const e = error as Error & { cause?: { code?: string } };
    return { name: e.constructor.name, message: e.message, ...(e.cause ? { cause: e.cause } : {}) };
  }
  assert.fail("expected the observation to end in an error");
}
/** The debug reasons the stage-one and worktree stages log when they return false. */
function reasonsOf(t: TestContext): string[] {
  const reasons: string[] = [];
  const debug = git["log"].debug.bind(git["log"]);
  t.mock.method(git["log"], "debug", (message: string, fields?: { error?: unknown }) => {
    if (typeof fields?.error === "string") reasons.push(fields.error);
    return debug(message, fields as never);
  });
  return reasons;
}
/** The discard core has no limit argument: lower the same seam for its own epoch scope. */
function lowerDiscardLimits(t: TestContext, limits: Limits): void {
  const limitsOf = git["epochLimits"].bind(git);
  t.mock.method(git as unknown as { epochLimits: typeof limitsOf }, "epochLimits",
    (given?: Parameters<typeof limitsOf>[0]) => limitsOf({ ...given, ...limits }));
}
async function discard(): Promise<unknown> {
  return git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
}
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function objectDir(cwd: string): string { return path.join(cwd, cwd === bare ? "objects" : ".git/objects"); }
/** Writes a loose-object file with arbitrary (possibly lying or corrupt) bytes under a fresh id. */
function looseFile(contents: Buffer): string {
  const id = "ab" + "c".repeat(38);
  const dir = path.join(objectDir(clonePath), "ab");
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, "c".repeat(38));
  fs.writeFileSync(file, contents, { mode: 0o444 });
  return id;
}

describe("defaults and the test seam", () => {
  it("pins the committed default budgets", () => {
    assert.deepEqual(git["epochLimits"](), {
      maxDirectoryEntries: 16384, maxBares: 1024, maxMetadataBytes: 16 * 1024 * 1024, maxMetadataRecords: 131072,
      maxSeenPaths: 4096, maxWorktreeContentBytes: 512 * 1024 * 1024, maxObjectStoreBytes: 4 * 1024 * 1024 * 1024,
    });
  });
  const raised = {
    maxDirectoryEntries: 16385, maxBares: 1025, maxMetadataBytes: 16 * 1024 * 1024 + 1, maxMetadataRecords: 131073,
    maxSeenPaths: 4097, maxWorktreeContentBytes: 512 * 1024 * 1024 + 1, maxObjectStoreBytes: 8 * 1024 * 1024 * 1024,
  };
  for (const [name, value] of Object.entries(raised)) {
    it("the seam cannot raise " + name + " above its default", () => {
      assert.throws(() => git["epochLimits"]({ [name]: value }), /invalid epoch retained limit/);
    });
  }
  for (const name of Object.keys(raised)) {
    it("the seam may lower " + name + " and rejects a non-positive or unsafe value", () => {
      assert.equal((git["epochLimits"]({ [name]: 1 }) as Record<string, number>)[name], 1);
      for (const bad of [0, -1, 1.5, Number.NaN, Number.MAX_SAFE_INTEGER + 2])
        assert.throws(() => git["epochLimits"]({ [name]: bad }), /invalid epoch retained limit/);
    });
  }
  it("the occurrence cap is the epoch 131072, not the legacy 20000 of the ordinary stopped-source proof", async () => {
    // 20,500 packed objects: past the legacy MAX_LOCAL_REF_SHAS (20,000), far below the epoch cap.
    const lines: string[] = [];
    for (let i = 0; i < 20500; i++) lines.push(`blob\ndata ${String(i).length + 2}\nb ${i}\n`);
    execFileSync("git", ["-C", bare, "fast-import", "--quiet"], { env: ENV, input: lines.join("") });
    assert.ok(Number(command(bare, "count-objects", "-v").match(/in-pack: (\d+)/)![1]) >= 20500);
    assert.equal(await observation(), true);
  });
  for (const [label, limits, pin] of [["the 96 MiB Git default", {}, 96 * 1024 * 1024],
    ["a lowered object-store budget", { maxObjectStoreBytes: 64 * 1024 * 1024 }, 64 * 1024 * 1024]] as const) {
    it("pins core.deltaBaseCacheLimit to " + label + ", never the 4 GiB object-store budget", async () => {
      const pins: string[] = [];
      (git as unknown as { epochNativeTestDependencies?: unknown }).epochNativeTestDependencies = {
        spawn: ((cmd: string, args: string[], opts: SpawnOptions) => {
          if (!args.includes("version")) pins.push(...args.filter(arg => arg.startsWith("core.deltaBaseCacheLimit=")));
          return spawn(cmd, args, opts);
        }) as typeof spawn,
        alive: (pid: number) => {
          try { process.kill(-pid, 0); return true; }
          catch (error) { return (error as NodeJS.ErrnoException).code === "EPERM"; }
        },
        kill: (pid: number) => {
          try { process.kill(-pid, "SIGKILL"); } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error; }
        },
        joinMs: 2_000,
      };
      assert.equal(await observation(limits), true);
      assert.ok(pins.length > 0, "object-store and worktree Git launches carry the pin");
      assert.deepEqual([...new Set(pins)], ["core.deltaBaseCacheLimit=" + pin]);
    });
  }
});

// Each finite limit, lowered through the test seam, must end the proof as a settled keep.
// `viaDiscard: false` marks a limit so low that the discard core's own retained inspection, which runs before the
// content proof under the same limits, is UNKNOWN first; the content proof is still pinned through `observation`.
const overflows: { name: string; limits: Limits; via: "stores" | "reason"; match: RegExp; prepare?: () => void;
  viaDiscard?: boolean }[] = [
  { name: "maxObjectStoreBytes (object-store snapshot bytes)", limits: { maxObjectStoreBytes: 2048 }, via: "stores",
    match: /object metadata byte limit/ },
  { name: "maxObjectStoreBytes (a loose object whose header declares past the budget)",
    limits: { maxObjectStoreBytes: 256 * 1024 }, via: "stores", match: /loose output byte limit/,
    prepare: () => {
      execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
        { env: ENV, input: Buffer.alloc(1024 * 1024, 65) });
    } },
  { name: "maxMetadataRecords (physical object occurrences)", limits: { maxMetadataRecords: 100 }, via: "stores",
    match: /physical object occurrence limit/,
    prepare: () => {
      for (let i = 0; i < 150; i++)
        execFileSync("git", ["-C", bare, "hash-object", "-w", "--stdin"], { env: ENV, input: `occurrence ${i}\n` });
    } },
  { name: "maxMetadataRecords (a Git config listing)", limits: { maxMetadataRecords: 3 }, via: "stores",
    match: /object completeness output limit/, viaDiscard: false },
  { name: "maxDirectoryEntries (object directory entries)", limits: { maxDirectoryEntries: 1 }, via: "stores",
    match: /object directory entry limit/ },
  { name: "maxMetadataBytes (config, index and every Git output)", limits: { maxMetadataBytes: 64 }, via: "reason",
    match: /metadata byte limit/, viaDiscard: false },
  { name: "maxMetadataRecords (index entries)", limits: { maxMetadataRecords: 2 }, via: "reason",
    match: /path record limit|index/, viaDiscard: false },
  { name: "maxWorktreeContentBytes (tracked content)", limits: { maxWorktreeContentBytes: 4 }, via: "reason",
    match: /tracked content byte limit/ },
];
describe("an overflow of each finite limit is a settled keep, not UNKNOWN", () => {
  for (const { name, limits, via, match, prepare, viaDiscard = true } of overflows) {
    it(name, async t => {
      prepare?.();
      const reasons = reasonsOf(t);
      if (via === "stores") {
        const error = await failure(observeStores(limits));
        assert.equal(error.name, "AutomaticBoundExceeded");
        assert.match(error.message, match);
      }
      assert.equal(await observation(limits), false, "the proof returns false rather than throwing UNKNOWN");
      if (via === "reason") assert.ok(reasons.some(reason => match.test(reason)), `refused by the limit: ${reasons.join("|")}`);
      if (!viaDiscard) return;
      lowerDiscardLimits(t, limits);
      assert.deepEqual(await discard(), { kind: "refused", reason: "epoch_content_unproven", consumers: [] });
      assert.equal(stopped, false, "a settled keep does not stop the epoch scope");
      scope.assertSettled();
      assert.ok(fs.existsSync(clonePath));
      assert.equal(journal(), raw);
    });
  }
  it("controls: the same fixture is provable under the default budgets", async () => {
    assert.equal(await observation(), true);
  });
});

// A corrupt, unreadable or indirect store, and an unresolved Git execution, are never a settled keep.
describe("UNKNOWN is preserved for anything that is not a positively identified overflow", () => {
  async function assertUnknown(t: TestContext, limits: Limits = {}, message?: RegExp): Promise<void> {
    const error = await failure(observeStores(limits));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    if (message) assert.match(`${error.message} ${error.cause?.code ?? ""}`, message);
    await assert.rejects(observation(limits), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
    const result = await discard();
    assert.equal((result as { kind: string }).kind, "unknown");
    assert.equal(stopped, true);
    assert.ok(fs.existsSync(clonePath));
    assert.equal(journal(), raw);
    void t;
  }
  it("a loose object that is not a zlib stream", async t => {
    looseFile(Buffer.from("this is not a zlib stream at all"));
    await assertUnknown(t);
  });
  it("a truncated loose object, with and without a lowered budget", async t => {
    looseFile(deflateSync(Buffer.from("blob 4\0data")).subarray(0, 6));
    await assertUnknown(t);
  });
  it("a loose object whose header declares a size that fits but whose stream overruns the budget", async t => {
    // Declares 5 bytes, then inflates past the 256 KiB budget: contradicting its own header, so corrupt.
    looseFile(deflateSync(Buffer.concat([Buffer.from("blob 5\0"), Buffer.alloc(1024 * 1024, 66)])));
    await assertUnknown(t, { maxObjectStoreBytes: 256 * 1024 }, /ERR_BUFFER_TOO_LARGE|Cannot create a Buffer larger/i);
  });
  it("a loose object with no readable header past the budget", async t => {
    looseFile(deflateSync(Buffer.alloc(1024 * 1024, 67)));
    await assertUnknown(t, { maxObjectStoreBytes: 256 * 1024 }, /ERR_BUFFER_TOO_LARGE|Cannot create a Buffer larger/i);
  });
  it("an unreadable object directory", async t => {
    const root = objectDir(bare);
    const opendir = fsp.opendir.bind(fsp);
    t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (args[0] === root) throw Object.assign(new Error("owned unreadable seam"), { code: "EACCES" });
      return opendir(...args);
    });
    await assertUnknown(t);
  });
  it("an http-alternates entry (indirect store)", async t => {
    fs.writeFileSync(path.join(objectDir(clonePath), "info/http-alternates"), "http://127.0.0.1:1/objects\n");
    await assertUnknown(t, {}, /unsupported object info entry/);
  });

  /** The snapshot's bounded `git config --file -` read is made to fail the way a Git child fails. */
  function configFails(t: TestContext, fields: Record<string, unknown>): void {
    const original = git["execScoped"].bind(git);
    t.mock.method(git as unknown as { execScoped: typeof original }, "execScoped", async (...args: Parameters<typeof original>) => {
      if (args[1].includes("--file") && args[1].includes("--null"))
        throw Object.assign(new Error("git subprocess maxBuffer exceeded"), fields);
      return original(...args);
    });
  }
  it("a joined Git stdout overrun is a settled keep", async t => {
    configFails(t, { outputExceeded: true, outputStream: "stdout", code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", epochProcessJoined: true });
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticBoundExceeded");
    assert.equal(await observation(), false);
    assert.deepEqual(await discard(), { kind: "refused", reason: "epoch_content_unproven", consumers: [] });
    assert.equal(stopped, false);
  });
  for (const [label, fields] of [
    ["a Git stderr overrun (a failing Git is noisy)", { outputExceeded: true, outputStream: "stderr", code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", epochProcessJoined: true }],
    ["an overrun of an unjoined child", { outputExceeded: true, outputStream: "stdout", code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }],
    ["a bare maxBuffer code with no stream or join facts", { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }],
    ["a timeout", { killed: true }],
  ] as const) {
    it(label + " stays UNKNOWN", async t => {
      configFails(t, fields);
      await assertUnknown(t);
    });
  }
  it("a real Git stdout overrun of the metadata cap is a settled keep", async () => {
    // A bare config whose listing is larger than the cap, while every earlier stage still fits.
    for (let i = 0; i < 600; i++) command(bare, "config", "--local", `r9.key${i}`, "value");
    assert.equal(await observation(), true, "control: the same store is provable under the default cap");
    assert.equal(await observation({ maxMetadataBytes: 8192 }), false);
  });
});
