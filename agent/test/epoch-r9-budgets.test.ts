import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync, spawn, type SpawnOptions } from "node:child_process";
import type { TestContext } from "node:test";
import { createDeflate, deflateSync } from "node:zlib";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { finished } from "node:stream/promises";
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

// Each finite CONTENT-stage (and worktree/shape-stage) limit, lowered through the test seam, must end the proof as a
// settled keep. An overflow while the object-store topology is read is UNKNOWN instead (see the topology describes).
// `viaDiscard: false` marks a limit so low that the discard core's own retained inspection, which runs before the
// content proof under the same limits, is UNKNOWN first; the content proof is still pinned through `observation`.
const overflows: { name: string; limits: Limits; via: "stores" | "reason"; match: RegExp; prepare?: () => void;
  viaDiscard?: boolean }[] = [
  // Content stage only: the topology fits the budget, then 600 valid loose objects overflow it.
  { name: "maxObjectStoreBytes (object-store snapshot bytes in the content stage)", limits: { maxObjectStoreBytes: 64 * 1024 },
    via: "stores", match: /object metadata byte limit/,
    prepare: () => {
      const dir = path.join(fx.dataDir, "loose-inputs");
      fs.mkdirSync(dir, { recursive: true });
      const files = Array.from({ length: 600 }, (_, i) => {
        const file = path.join(dir, `f${i}`);
        fs.writeFileSync(file, `loose ${i}\n`);
        return file;
      });
      execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin-paths"], { env: ENV, input: files.join("\n") + "\n" });
    } },
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
  { name: "maxDirectoryEntries (a loose fan-out directory past the default limit)", limits: {}, via: "stores",
    match: /object directory entry limit/,
    prepare: () => {
      const dir = path.join(objectDir(clonePath), "00");
      fs.mkdirSync(dir, { recursive: true });
      for (let i = 0; i < 16_400; i++) fs.writeFileSync(path.join(dir, i.toString(16).padStart(38, "0")), "");
    } },
  { name: "maxMetadataBytes (config, index and every Git output)", limits: { maxMetadataBytes: 64 }, via: "reason",
    match: /metadata byte limit/, viaDiscard: false },
  // The limit itself ("index: index entry limit"), not any other "index: ..." refusal such as a checksum mismatch.
  { name: "maxMetadataRecords (index entries)", limits: { maxMetadataRecords: 64 }, via: "reason",
    match: /^index: index entry limit$/, viaDiscard: false,
    prepare: () => {
      // Every other record counter stays far below 64, so only the index count (100 more entries) can overflow.
      for (let i = 0; i < 100; i++) fs.writeFileSync(path.join(clonePath, `staged-${i}`), `staged ${i}\n`);
      command(clonePath, "add", "--", ...Array.from({ length: 100 }, (_, i) => `staged-${i}`));
    } },
  { name: "maxMetadataBytes (an over-budget .git/objects/info entry in the clone shape stage)",
    limits: { maxMetadataBytes: 4096 }, via: "reason", match: /^\.git\/objects\/info\/packs: metadata byte limit$/,
    prepare: () => { fs.writeFileSync(path.join(objectDir(clonePath), "info/packs"), "P ".repeat(4096)); }, viaDiscard: false },
  { name: "EPOCH_UNTRACKED_EXPANSION_DEPTH (an untracked tree deeper than 32 levels)", limits: {}, via: "reason",
    match: /untracked expansion depth limit/,
    prepare: () => {
      const deep = path.join(clonePath, ...Array.from({ length: 36 }, (_, i) => `d${i}`));
      fs.mkdirSync(deep, { recursive: true });
      fs.writeFileSync(path.join(deep, "leaf"), "x");
    } },
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
  for (const [label, limits] of [["the default budget", {}], ["a lowered budget", { maxObjectStoreBytes: 256 * 1024 }]] as const) {
    it("a truncated loose object under " + label, async t => {
      looseFile(deflateSync(Buffer.from("blob 4\0data")).subarray(0, 6));
      await assertUnknown(t, limits, /unexpected end of file|Z_BUF_ERROR/i);
    });
  }
  it("a loose object whose header declares a size that fits but whose stream overruns it", async t => {
    // Declares 5 bytes, then inflates 1 MiB: contradicting its own header, so corrupt, not over budget.
    looseFile(deflateSync(Buffer.concat([Buffer.from("blob 5\0"), Buffer.alloc(1024 * 1024, 66)])));
    await assertUnknown(t, { maxObjectStoreBytes: 256 * 1024 }, /invalid loose object/);
  });
  it("a loose object with no readable header past the budget", async t => {
    looseFile(deflateSync(Buffer.alloc(1024 * 1024, 67)));
    await assertUnknown(t, { maxObjectStoreBytes: 256 * 1024 }, /invalid loose object/);
  });
  it("a loose object whose header declares past the budget but whose stream ends short of it", async t => {
    // Declares 1 MiB (past the 256 KiB budget) and produces 5 bytes: a lie, so UNKNOWN rather than a keep.
    looseFile(deflateSync(Buffer.concat([Buffer.from("blob 1048576\0"), Buffer.from("short")])));
    await assertUnknown(t, { maxObjectStoreBytes: 256 * 1024 }, /invalid loose object/);
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
  // Lead decision: ANY overflow of a topology observation is UNKNOWN, because its indirection cannot be observed.
  it("a joined Git stdout overrun of the topology config listing is UNKNOWN", async t => {
    configFails(t, { outputExceeded: true, outputStream: "stdout", code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", epochProcessJoined: true });
    await assertUnknown(t, {}, /exceeds the observation limit/);
  });
  /** The topology snapshot alone, under lowered limits (a lowered limit may end an earlier stage first in the full proof). */
  async function assertUnknownUnder(limits: Limits): Promise<void> {
    const error = await failure(observeStores(limits));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /exceeds the observation limit/);
  }
  it("a config listing past the record limit is UNKNOWN", async () => {
    await assertUnknownUnder({ maxMetadataRecords: 3 });
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
  it("a real Git stdout overrun of the metadata cap is UNKNOWN, even behind partialclone", async () => {
    // A bare config FILE that fits the 8192 byte cap (a file past it is UNKNOWN too) whose Git listing is larger than
    // the cap: a long section name makes each short line list as a long `section.key` row. The listing is never read
    // past the cap, so the partialclone it holds is unobservable: UNKNOWN, never a keep.
    fs.appendFileSync(path.join(bare, "config"),
      "[extensions]\n\tpartialclone = origin\n[averyveryverylongsectionnamefortheoverrun]\n" + "\tk = v\n".repeat(600));
    assert.ok(fs.statSync(path.join(bare, "config")).size < 8192);
    await assertUnknownUnder({ maxMetadataBytes: 8192 });
  });
  it("the same listing without partialclone is UNKNOWN too: the overflow itself is the unknown", async () => {
    fs.appendFileSync(path.join(bare, "config"),
      "[averyveryverylongsectionnamefortheoverrun]\n" + "\tk = v\n".repeat(600));
    assert.equal(await observation(), true, "control: the same store is provable under the default cap");
    await assertUnknownUnder({ maxMetadataBytes: 8192 });
  });
  it("a runner-side local config listing past the record limit is UNKNOWN, while the topology read fits", async t => {
    const original = git["execScoped"].bind(git);
    t.mock.method(git as unknown as { execScoped: typeof original }, "execScoped", async (...args: Parameters<typeof original>) => {
      if (args[1].includes("config") && args[1].includes("--local"))
        return { stdout: "x.k\nv\0".repeat(131_073), stderr: "" };
      return original(...args);
    });
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.equal(error.message, "object config listing exceeds the observation limit");
  });
  it("a 630 KB clone config with partialclone and 90,000 rows under 250-character sections is UNKNOWN at the default caps", async t => {
    const section = "s".repeat(250);
    fs.appendFileSync(path.join(clonePath, ".git", "config"),
      "[extensions]\n\tpartialclone = origin\n[" + section + "]\n" + "\tk = v\n".repeat(90_000));
    assert.ok(fs.statSync(path.join(clonePath, ".git", "config")).size < 1024 * 1024);
    await assertUnknown(t);
  });
});

// ---------------------------------------------------------------------------------------------------------------
// Memory: the observation streams, so peak memory is bounded by its windows, never by the 4 GiB logical budget.

/** Growth of the process high-water RSS (and of a sampled peak) while `run` executes. */
async function rssGrowth<T>(run: () => Promise<T>): Promise<{ result: T; growth: number }> {
  const high = () => process.resourceUsage().maxRSS * 1024;
  const base = process.memoryUsage().rss, highBase = high();
  let peak = base;
  const timer = setInterval(() => { peak = Math.max(peak, process.memoryUsage().rss); }, 10);
  try {
    const result = await run();
    peak = Math.max(peak, process.memoryUsage().rss);
    return { result, growth: Math.max(peak - base, high() - highBase) };
  } finally { clearInterval(timer); }
}
/** zlib bytes of `size` zero bytes (after `prefix`), produced and hashed in 16 MiB steps so the test itself stays small. */
async function zeroStream(prefix: Buffer, size: number): Promise<{ compressed: Buffer; sha1: string }> {
  const deflate = createDeflate();
  const parts: Buffer[] = [];
  deflate.on("data", (part: Buffer) => parts.push(part));
  const hash = createHash("sha1");
  const step = Buffer.alloc(16 * 1024 * 1024);
  const send = async (piece: Buffer) => { hash.update(piece); if (!deflate.write(piece)) await once(deflate, "drain"); };
  await send(prefix);
  for (let left = size; left > 0; left -= step.length) await send(step.subarray(0, Math.min(step.length, left)));
  deflate.end();
  await finished(deflate.resume());
  return { compressed: Buffer.concat(parts), sha1: hash.digest("hex") };
}
describe("a large valid object costs windows of memory, not its size", () => {
  const GIB = 1024 * 1024 * 1024;
  // Inflating a gigabyte takes seconds per snapshot; the proof's own 30 second test deadline is not what is under test.
  beforeEach(() => { input = { ...input, deadline: Date.now() + 10 * 60_000 }; });
  it("a loose blob that inflates to 1 GiB of zeros, at the default limits", async t => {
    const { compressed, sha1 } = await zeroStream(Buffer.from(`blob ${GIB}\0`), GIB);
    assert.ok(compressed.length < 4 * 1024 * 1024, "zeros compress about a thousand to one");
    const dir = path.join(objectDir(bare), sha1.slice(0, 2));
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, sha1.slice(2)), compressed, { mode: 0o444 });
    const { result, growth } = await rssGrowth(() => observation());
    // 1 GiB is under the 4 GiB budget, and only the bare holds it, so the clone is covered: the proof completes.
    assert.equal(result, true);
    t.diagnostic(`RSS grew by ${Math.round(growth / 1048576)} MiB`);
    assert.ok(growth < 512 * 1024 * 1024, `RSS grew by ${Math.round(growth / 1048576)} MiB`);
  });
  it("a pack record that inflates to 1 GiB of zeros, at the default limits", async t => {
    const size = GIB;
    const { compressed } = await zeroStream(Buffer.alloc(0), size);
    // Pack entry header: type 3 (blob), then the size as a little-endian base-128 varint.
    const header: number[] = [(3 << 4) | (size & 15)];
    for (let n = Math.floor(size / 16); n > 0; n = Math.floor(n / 128)) { header[header.length - 1]! |= 128; header.push(n % 128); }
    const record = Buffer.concat([Buffer.from(header), compressed]);
    const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 1]), record]);
    const trailer = createHash("sha1").update(pack).digest();
    const packDir = path.join(objectDir(bare), "pack");
    fs.mkdirSync(packDir, { recursive: true });
    const name = path.join(packDir, "pack-" + trailer.toString("hex"));
    fs.writeFileSync(name + ".pack", Buffer.concat([pack, trailer]));
    // Git writes the valid index (and so computes the real object id) for the hand-built pack.
    command(bare, "index-pack", "-o", name + ".idx", name + ".pack");
    // The pair is only observed by the Node-side store snapshot (Git's verify-pack is not run here).
    const { growth } = await rssGrowth(() => observeStores());
    t.diagnostic(`RSS grew by ${Math.round(growth / 1048576)} MiB`);
    assert.ok(growth < 512 * 1024 * 1024, `RSS grew by ${Math.round(growth / 1048576)} MiB`);
  });
});

// ---------------------------------------------------------------------------------------------------------------
// Topology first: an UNKNOWN-class indirection is decided before any content overflow can end the proof as a keep.

describe("the topology is decided before content is charged", () => {
  const junk = (n: number) => {
    const dir = path.join(objectDir(clonePath), "00");
    fs.mkdirSync(dir, { recursive: true });
    for (let i = 0; i < n; i++) fs.writeFileSync(path.join(dir, i.toString(16).padStart(38, "0")), "");
  };
  const outside = () => {
    const alt = path.join(fx.dataDir, "outside.git");
    command(bare, "init", "--bare", alt);
    fs.appendFileSync(path.join(objectDir(clonePath), "info/alternates"), path.join(alt, "objects") + "\n");
  };
  it("control: junk past the directory entry limit alone is a settled keep", async () => {
    junk(16_400);
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticBoundExceeded");
    assert.match(error.message, /object directory entry limit/);
    assert.equal(await observation(), false);
  });
  it("an alternate outside the managed repositories alone is UNKNOWN", async () => {
    outside();
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /alternate writer exclusion unavailable/);
  });
  it("the same alternate stays UNKNOWN behind 16,400 junk entries, however the proof is entered", async () => {
    outside();
    junk(16_400);
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /alternate writer exclusion unavailable/);
    await assert.rejects(observation(), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
  });
  it("an outside alternate reached through a managed alternate stays UNKNOWN behind a content overflow", async () => {
    const alt = path.join(fx.dataDir, "outside.git");
    command(bare, "init", "--bare", alt);
    fs.appendFileSync(path.join(objectDir(bare), "info/alternates"), path.join(alt, "objects") + "\n");
    junk(16_400);
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /alternate writer exclusion unavailable/);
  });
  it("a topology directory past the entry limit is UNKNOWN, whatever it holds", async () => {
    fs.writeFileSync(path.join(objectDir(clonePath), "info/zz-not-legal"), "");
    const error = await failure(observeStores({ maxDirectoryEntries: 1 }));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /unsupported object info entry|object directory entry limit/);
  });
  it("an oversized packed-refs does not hide an outside alternate in the clone shape stage", async () => {
    outside();
    fs.writeFileSync(path.join(clonePath, ".git", "packed-refs"), "# pack-refs with: peeled fully-peeled sorted\n".repeat(400));
    await assert.rejects(observation({ maxMetadataBytes: 4096 }), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
  });
  it("control: the same oversized packed-refs alone is a settled keep from the shape stage", async t => {
    fs.writeFileSync(path.join(clonePath, ".git", "packed-refs"), "# pack-refs with: peeled fully-peeled sorted\n".repeat(400));
    const reasons = reasonsOf(t);
    assert.equal(await observation({ maxMetadataBytes: 4096 }), false);
    assert.ok(reasons.some(reason => /packed-refs: metadata byte limit/.test(reason)), reasons.join("|"));
  });
  it("an illegal objects/info name in the clone is UNKNOWN in the shape stage even behind an oversized packed-refs", async () => {
    fs.writeFileSync(path.join(objectDir(clonePath), "info/http-alternates"), "http://127.0.0.1:1/objects\n");
    fs.writeFileSync(path.join(clonePath, ".git", "packed-refs"), "# pack-refs with: peeled fully-peeled sorted\n".repeat(400));
    await assert.rejects(observation({ maxMetadataBytes: 4096 }), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
  });
});

describe("the alternate edge limit", () => {
  const lowered = { maxMetadataRecords: 40 };
  const own = () => path.join(bare, "objects");
  const writeAlternates = (lines: string[]) =>
    fs.writeFileSync(path.join(objectDir(clonePath), "info/alternates"), lines.join("\n") + "\n");
  const valid = (n: number) => Array.from({ length: n }, () => own());
  it("control: lines within the cap are observed", async () => {
    writeAlternates(valid(3));
    assert.equal(await observation(lowered), true);
  });
  // Lines past the cap are never traversed, so crossing it is a topology overflow: UNKNOWN, never a keep.
  for (const [label, lines, message] of [
    ["more valid lines than the cap", () => valid(45), /alternate edge limit/],
    ["an invalid line before the cap", () => ["not a valid line", ...valid(44)], /unsupported alternate syntax/],
    ["an invalid line after the cap", () => [...valid(45), "not a valid line"], /alternate edge limit/],
    ["an outside path after the cap", () => [...valid(45), "/tmp/outside/objects"], /alternate edge limit/],
  ] as const) {
    it(label + " is UNKNOWN", async () => {
      writeAlternates(lines());
      const error = await failure(observeStores(lowered));
      assert.equal(error.name, "AutomaticCompletenessUnknown");
      assert.match(error.message, message);
      await assert.rejects(observation(lowered), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
    });
  }
});

// ---------------------------------------------------------------------------------------------------------------
// Re-reads and Git inventories: the mocks sit under the real runGitAsRunner/runGitWithEnv wrappers (at execScoped), which
// wrap every child failure in a plain Error, so a Git output overrun never reaches the proof as a limit overflow. Both
// the first read and its repeat are UNKNOWN.

describe("an overflow of a Git inventory read is UNKNOWN, not a settled keep", () => {
  const overrun = () => Object.assign(new Error("git subprocess maxBuffer exceeded"), {
    outputExceeded: true, outputStream: "stdout", code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", epochProcessJoined: true });
  const sites: Record<string, (args: string[]) => boolean> = {
    "the source inventory": args => args.includes("--batch-all-objects") && args.includes(clonePath),
    "the bare inventory": args => args.includes("--batch-all-objects") && args.includes(bare),
    "the clone refs": args => args.includes("for-each-ref") && args.includes("--format=%(objectname) %(refname) %(symref)"),
    "the trusted-root closure": args => args.includes("rev-list"),
  };
  for (const [label, matches] of Object.entries(sites)) {
    it(label, async t => {
      const original = git["execScoped"].bind(git);
      let seen = 0, failAt = 1;
      t.mock.method(git as unknown as { execScoped: typeof original }, "execScoped", async (...args: Parameters<typeof original>) => {
        if (matches(args[1]) && ++seen === failAt) throw overrun();
        return original(...args);
      });
      // First instance: the real wrapper turns the overrun into an unreadable inventory, which is UNKNOWN.
      await assert.rejects(observation(), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown" &&
        /object coverage unavailable/.test((e as Error).message));
      assert.equal(seen, 1);
      // The repeat of the same read, after the first fit: UNKNOWN.
      seen = 0; failAt = 2;
      await assert.rejects(observation(), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
      assert.equal(seen, 2);
    });
  }
  it("the final topology snapshot, when the store grows past the directory entry limit between the reads", async t => {
    let limit = 0;
    for (let n = 5; n < 400 && !limit; n++) {
      try { await observeStores({ maxDirectoryEntries: n }); limit = n; } catch { /* below the store's own size */ }
    }
    assert.ok(limit > 0);
    while (!await observation({ maxDirectoryEntries: limit })) limit++;
    const original = git["runGitAsRunner"].bind(git);
    let inventories = 0;
    t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
      // An empty fan-out directory is invisible to Git and counts as two entries in the next snapshot.
      if (args[1].includes("--batch-all-objects") && ++inventories === 2) fs.mkdirSync(path.join(objectDir(bare), "ee"));
      return original(...args);
    });
    await assert.rejects(observation({ maxDirectoryEntries: limit }), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown" &&
      /object observations changed/.test((e as Error).message));
  });
});

// ---------------------------------------------------------------------------------------------------------------
// No pending overflow may absorb an UNKNOWN-class observation, and nothing is charged before it was inspected.

describe("a topology overflow is UNKNOWN and never hides an UNKNOWN-class observation", () => {
  const packJunk = (n: number) => {
    const dir = path.join(objectDir(clonePath), "pack");
    fs.mkdirSync(dir, { recursive: true });
    for (let i = 0; i < n; i++) fs.writeFileSync(path.join(dir, `pack-${i.toString(16).padStart(40, "0")}.keep`), "");
  };
  const cloneConfig = (...args: string[]) => command(clonePath, "config", "--local", ...args);
  /** The snapshot's config launches (`git config --local ...`) made to fail the way an unresolved Git execution fails. */
  function localConfigFails(t: TestContext, make: () => Error): void {
    const original = git["runGitAsRunner"].bind(git);
    t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
      if (args[1].includes("config") && args[1].includes("--local")) throw make();
      return original(...args);
    });
  }
  async function assertUnknownStores(limits: Limits, message?: RegExp): Promise<void> {
    const error = await failure(observeStores(limits));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    if (message) assert.match(`${error.message} ${error.cause?.code ?? ""}`, message);
  }
  it("control: 16,400 pack junk entries alone are UNKNOWN, a topology overflow", async () => {
    packJunk(16_400);
    await assertUnknownStores({}, /object directory entry limit/);
  });
  it("a symlinked .idx behind 16,400 pack junk entries is UNKNOWN", async () => {
    packJunk(16_400);
    const name = "pack-" + "a".repeat(40);
    const dir = path.join(objectDir(clonePath), "pack");
    fs.writeFileSync(path.join(dir, name + ".pack"), "");
    fs.writeFileSync(path.join(fx.dataDir, "elsewhere.idx"), "");
    fs.symlinkSync(path.join(fx.dataDir, "elsewhere.idx"), path.join(dir, name + ".idx"));
    await assertUnknownStores({});
    await assert.rejects(observation(), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
  });
  it("an orphan .pack behind 16,400 pack junk entries is UNKNOWN", async () => {
    packJunk(16_400);
    fs.writeFileSync(path.join(objectDir(clonePath), "pack", "pack-" + "b".repeat(40) + ".pack"), "");
    await assertUnknownStores({});
    await assert.rejects(observation(), e => (e as Error).constructor.name === "AutomaticCompletenessUnknown");
  });
  it("partialclone plus an unknown extension, behind 16,400 pack junk entries, is UNKNOWN", async () => {
    cloneConfig("core.repositoryformatversion", "1");
    cloneConfig("extensions.partialclone", "origin");
    cloneConfig("extensions.zzunknown", "true");
    packJunk(16_400);
    await assertUnknownStores({});
  });
  it("an unknown extension alone, behind 16,400 pack junk entries, is UNKNOWN", async () => {
    cloneConfig("core.repositoryformatversion", "1");
    cloneConfig("extensions.zzunknown", "true");
    packJunk(16_400);
    await assertUnknownStores({});
  });
  it("a timeout of the config launch is UNKNOWN", async t => {
    localConfigFails(t, () => Object.assign(new Error("git subprocess timed out"), { killed: true }));
    await assertUnknownStores({});
  });
  it("a revocation during the config launch is UNKNOWN", async t => {
    const original = git["runGitAsRunner"].bind(git);
    t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
      if (args[1].includes("config") && args[1].includes("--local")) live = false;
      return original(...args);
    });
    await assertUnknownStores({}, /reservation expired|revoked|stopped|unavailable/i);
  });
  // The content stage must not run before the topology (and its config checks) decided: with a 4096 byte object-store
  // budget the content stage would otherwise end the proof as a keep first.
  it("partialclone in the clone config stays UNKNOWN under a 4096 byte object-store budget", async () => {
    cloneConfig("extensions.partialclone", "origin");
    await assertUnknownStores({ maxObjectStoreBytes: 4096 });
  });
  it("a failing config launch stays UNKNOWN under a 4096 byte object-store budget", async t => {
    localConfigFails(t, () => Object.assign(new Error("git subprocess timed out"), { killed: true }));
    await assertUnknownStores({ maxObjectStoreBytes: 4096 });
  });
  it("an illegal loose object name is UNKNOWN even when counting it would overflow the entry limit", async () => {
    // The empty fan-out directory is part of the topology and is the LAST directory the content stage lists (the
    // bare is read after the clone, and `ff` sorts after its other fan-outs); the limit is the least one that still
    // lets the store through, so the one name added below passes the limit in the content stage only.
    const dir = path.join(objectDir(bare), "ff");
    fs.mkdirSync(dir, { recursive: true });
    let low = 1, high = 512;
    while (low < high) {
      const mid = Math.floor((low + high) / 2);
      try { await observeStores({ maxDirectoryEntries: mid }); high = mid; } catch { low = mid + 1; }
    }
    await observeStores({ maxDirectoryEntries: low });
    fs.writeFileSync(path.join(dir, "zz"), "");
    await assertUnknownStores({ maxDirectoryEntries: low }, /unsupported loose object/);
  });
});

describe("a config listing past the record limit is a topology overflow, hence UNKNOWN", () => {
  it("partialclone behind 131,100 padding rows in the clone config is UNKNOWN", async () => {
    fs.appendFileSync(path.join(clonePath, ".git", "config"),
      "[extensions]\n\tpartialclone = origin\n[pad]\n" + "\tk = v\n".repeat(131_100));
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /exceeds the observation limit|config indirection unsupported/);
  });
  it("the same padding alone is UNKNOWN as well, not a settled keep", async () => {
    fs.appendFileSync(path.join(clonePath, ".git", "config"), "[pad]\n" + "\tk = v\n".repeat(131_100));
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
  });
});

describe("a topology file past the metadata byte limit is UNKNOWN, never a keep", () => {
  it("a bare config padded past the limit", async () => {
    fs.appendFileSync(path.join(bare, "config"), "# padding\n".repeat(1000));
    const error = await failure(observeStores({ maxMetadataBytes: 4096 }));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /object metadata read byte limit/);
  });
  it("an alternates file past the limit that names an outside store", async () => {
    const alt = path.join(fx.dataDir, "outside.git");
    command(bare, "init", "--bare", alt);
    const own = path.join(bare, "objects");
    fs.appendFileSync(path.join(objectDir(clonePath), "info/alternates"),
      (own + "\n").repeat(60) + path.join(alt, "objects") + "\n");
    const error = await failure(observeStores({ maxMetadataBytes: 2048 }));
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /object metadata read byte limit/);
  });
});

describe("input is charged as zlib consumes it, never as a window is read", () => {
  const lowered = { maxObjectStoreBytes: 512 * 1024 };
  it("a 1 MiB loose file that is not a zlib stream is UNKNOWN at a lowered budget, as at the default", async () => {
    looseFile(Buffer.alloc(1024 * 1024, 0x41));
    for (const limits of [{}, lowered]) {
      const error = await failure(observeStores(limits));
      assert.equal(error.name, "AutomaticCompletenessUnknown");
      assert.match(`${error.message} ${error.cause?.code ?? ""}`, /incorrect header check|Z_DATA_ERROR/i);
    }
  });
  it("a valid stream followed by 1 MiB of trailing garbage is UNKNOWN at a lowered budget, as at the default", async () => {
    looseFile(Buffer.concat([deflateSync(Buffer.from("blob 5\0hello")), Buffer.alloc(1024 * 1024, 0x41)]));
    for (const limits of [{}, lowered]) {
      const error = await failure(observeStores(limits));
      assert.equal(error.name, "AutomaticCompletenessUnknown");
      assert.match(error.message, /trailing loose input/);
    }
  });
  it("a valid stream followed by a 24 MiB tail stops reading at the first window, not at the end of the file", async t => {
    const id = looseFile(Buffer.concat([deflateSync(Buffer.from("blob 5\0hello")), Buffer.alloc(24 * 1024 * 1024, 0x41)]));
    const file = path.join(objectDir(clonePath), id.slice(0, 2), id.slice(2));
    const open = fsp.open.bind(fsp);
    let bytesRead = 0;
    t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
      const handle = await open(...args);
      if (args[0] !== file) return handle;
      const read = handle.read.bind(handle) as (...a: unknown[]) => Promise<{ bytesRead: number }>;
      (handle as unknown as { read: typeof read }).read = async (...a) => {
        const result = await read(...a);
        bytesRead += result.bytesRead;
        return result;
      };
      return handle;
    });
    const error = await failure(observeStores());
    assert.equal(error.name, "AutomaticCompletenessUnknown");
    assert.match(error.message, /trailing loose input/);
    // One 8 MiB read window covers the stream; the stream ended inside its first piece, so the 24 MiB tail is never
    // fetched (without the early stop the loop would pull every following window).
    assert.ok(bytesRead > 0 && bytesRead <= 8 * 1024 * 1024, `bytes read from the loose file: ${bytesRead}`);
  });
});
