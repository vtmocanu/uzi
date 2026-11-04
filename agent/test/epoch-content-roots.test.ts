import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-content-roots";
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
let nativeLaunches: number;

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
  pending = 0; stopped = false; live = true; nativeLaunches = 0;
  // Contract double counts real Git participation; descriptor ownership is tested separately.
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { nativeLaunches++; return [...stdio]; },
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


// Invoke the registered automatic proof under its production native/limit scopes.
// The discard core intentionally refuses owed consumers before reaching this proof.
async function covered(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function side(): string {
  const sha = command(bare, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "owed side");
  command(clonePath, "update-ref", "refs/private/side", sha);
  return sha;
}
async function pin(sha: string, runId = RUN): Promise<void> {
  await git["pinOwedUnderLock"]({
    runId, barePath: bare, branch: BRANCH, generation: 1, kind: "issue",
    defaultIdentity: { ref: "refs/heads/main", sha: command(bare, "rev-parse", "HEAD") },
  }, sha);
}
function snapshot(): string {
  return fs.readdirSync(path.join(bare, "uzi-owed")).sort().map(name =>
    name + ":" + fs.readFileSync(path.join(bare, "uzi-owed", name), "utf8")).join("\n") +
    command(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-owed/");
}

describe("automatic approved content roots", () => {
  it("accepts a validated own-run owed side commit without changing custody or waiving consumers", async () => {
    await pin(side());
    const before = snapshot();
    assert.equal(await covered(), true);
    assert.ok(nativeLaunches > 0);
    assert.equal(snapshot(), before);
    const inspected = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    assert.equal(inspected.partial, false);
    assert.ok(inspected.sources[0]!.consumers.includes("owed_pins:1"));
    assert.equal(inspected.sources[0]!.epochSettlement, undefined, "owed consumers prevent strict-plain authority");
    const confirm = inspected.sources[0]!.descriptor;
    const result = await git.discardStoppedSource({ ...options, confirm,
      epochProof: git.createEpochDiscardProof({ ...input, confirm }) });
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(clonePath));
    assert.equal(snapshot(), before);
  });
  it("does not approve another run's owed root or an arbitrary bare ref", async () => {
    const sha = side();
    command(bare, "update-ref", "refs/private/arbitrary", sha);
    assert.equal(await covered(), false);
    await pin(sha, "foreign-run");
    assert.equal(await covered(), false);
  });
  for (const damage of ["missing", "invalid", "unreadable"] as const) {
    it("refuses " + damage + " owed discovery context", async t => {
      await pin(side());
      const dir = path.join(bare, "uzi-owed");
      const name = path.join(dir, fs.readdirSync(dir).find(n => n.startsWith("context-"))!);
      if (damage === "missing") fs.unlinkSync(name);
      if (damage === "invalid") fs.writeFileSync(name, "{}");
      if (damage === "unreadable") {
        const open = fsp.open.bind(fsp);
        t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
          if (args[0] === name) throw Object.assign(new Error("unreadable"), { code: "EACCES" });
          return open(...args);
        });
      }
      assert.equal(await covered(), false);
    });
  }
  it("caps automatic directory reads while preserving legacy enumeration defaults", async t => {
    await pin(side());
    const dir = path.join(bare, "uzi-owed");
    for (let i = 0; i < 40; i++) fs.writeFileSync(path.join(dir,
      ".tmp-" + i.toString(16).padStart(8, "0") + "-0000-4000-8000-000000000000"), "");
    let reads = 0, closes = 0;
    const opendir = fsp.opendir.bind(fsp);
    t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      const handle = await opendir(...args);
      if (args[0] === dir) {
        assert.equal((args[1] as { bufferSize: number }).bufferSize, 1);
        const read = handle.read.bind(handle), close = handle.close.bind(handle);
        handle.read = (async () => { reads++; return read(); }) as typeof handle.read;
        handle.close = (async () => { closes++; await close(); }) as typeof handle.close;
      }
      return handle;
    });
    assert.equal(await covered({ maxMetadataRecords: 3 }), false);
    assert.equal(reads, 4);
    assert.equal(closes, 1);
    t.mock.restoreAll();
    assert.equal((await git.enumerateOwedCandidates(bare, RUN)).length, 1);
    assert.equal(await covered({ maxMetadataBytes: 128 }), false);
  });
  for (const phase of ["open", "read", "opendir"] as const) {
    it("joins the opened handle after cancellation during actual owed " + phase, async t => {
      await pin(side());
      const dir = path.join(bare, "uzi-owed");
      let closes = 0, reads = 0;
      const open = fsp.open.bind(fsp), opendir = fsp.opendir.bind(fsp);
      if (phase === "opendir") {
        t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
          const handle = await opendir(...args);
          if (args[0] === dir) {
            const close = handle.close.bind(handle);
            handle.close = (async () => { closes++; await close(); }) as typeof handle.close;
            live = false;
          }
          return handle;
        });
      } else {
        t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
          const handle = await open(...args);
          if (String(args[0]).startsWith(dir + path.sep)) {
            const close = handle.close.bind(handle), read = handle.read.bind(handle);
            handle.close = async () => { closes++; await close(); };
            handle.read = (async (...readArgs: Parameters<typeof handle.read>) => {
              reads++;
              const value = await read(...readArgs);
              live = false;
              return value;
            }) as typeof handle.read;
            if (phase === "open") live = false;
          }
          return handle;
        });
      }
      assert.equal(await covered(), false);
      assert.equal(closes, 1);
      assert.equal(reads, phase === "read" ? 1 : 0);
    });
  }
});

it("refuses unpublished local refs and objects hidden by a common-directory redirect", async () => {
  const sha = command(clonePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unpublished source");
  command(clonePath, "update-ref", "refs/private/side", sha);
  assert.equal(await covered(), false);
  const gitdir = path.join(clonePath, ".git");
  const common = path.join(fx.dataDir, "alternate-common");
  fs.cpSync(gitdir, common, { recursive: true });
  command(common, "--git-dir=.", "update-ref", "-d", "refs/private/side");
  const objectPath = path.join("objects", sha.slice(0, 2), sha.slice(2));
  fs.unlinkSync(path.join(common, objectPath));
  fs.writeFileSync(path.join(gitdir, "commondir"), common + "\n");
  assert.equal(fs.readFileSync(path.join(gitdir, "refs/private/side"), "utf8").trim(), sha);
  assert.equal(fs.existsSync(path.join(gitdir, objectPath)), true);
  const before = fs.readFileSync(path.join(bare, "config"));
  assert.equal(await covered(), false, "automatic coverage must refuse hidden local metadata");
  assert.equal((await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) })).kind, "refused");
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(fs.existsSync(path.join(gitdir, objectPath)), true);
});
