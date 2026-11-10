import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { acquireWorkerLifetime, type WorkerLifetime } from "../src/authority-lock.js";
import { openAuthorityStore, type AuthorityFsOps, type SlotSnapshot } from "../src/completion-authority.js";
import { runPodEpochShadow, PodEpochSettlementError, type PodEpochDeletionDeps } from "../src/pod-epoch-reclaim.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";
import { daemon, ENABLE_DELETION_FOR_TEST } from "./pod-epoch-reclaim-support.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { RunRunner } from "../src/runner.js";
import { buildPodEpochDeletionDeps, buildWorkerGitCache } from "../src/main.js";
import { testLockOptions } from "./authority-test-support.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-epoch-discard";
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
function setJournal(value: string): void { gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`, value]); }

function freshScope(): PodEpochReclaimScope {
  let ownPending = 0, ownStopped = false;
  return {
    assertExclusive() { if (ownStopped) throw new Error("stopped"); },
    get pendingMutations() { return ownPending; },
    beginMutation() {
      ownPending++; pending = ownPending;
      let settled = false;
      return { settle() { if (!settled) { settled = true; ownPending--; pending = ownPending; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { nativeLaunches++; return [...stdio]; },
    stop() { ownStopped = true; stopped = true; },
    stopped() { return ownStopped; },
    assertSettled() { if (ownStopped || ownPending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
}

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
  assert.equal(inspection.sources[0]?.epochSettlement?.disposition.kind, "local_discard");
  pending = 0; stopped = false; live = true; nativeLaunches = 0;
  // Contract double counts real Git participation; descriptor ownership is tested separately.
  scope = freshScope();
  options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => true };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => fx?.cleanup());

async function refresh() {
  const inspection = await git.inspectRetainedSources({ selector: options.selector, barePath: bare,
    holds: options.holds, lifecycle: options.lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  assert.equal(inspection.sources.length, 1, "fresh epoch must rediscover the exact source");
  options.confirm = inspection.sources[0]!.descriptor;
  input = { ...input, confirm: options.confirm };
}
function ledger(): string[] { return gitIn(["config", "--get-all", `uzi-attempts.${BRANCH}.entry`]).split("\n"); }
async function discard() { return git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) }); }
function fault(step: string, action: () => Promise<void>) {
  (git as unknown as { epochSettlementFault: (name: string) => Promise<void> }).epochSettlementFault = async name => {
    if (name === step) await action();
  };
}
for (const uncertain of [false, true]) it(`caller owns endSourceUse under the same A after barriers: uncertain=${uncertain}`, async () => {
  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "epoch-integration", timeoutMs: 5000 });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: authority.hooks }));
  const lane = { executionTails: new Map(), inventoryReservations: new Map(), activeRuns: new Map() };
  const events: string[] = [];
  fault(uncertain ? "retired:after" : "clear:after", async () => {
    events.push("barrier");
    if (uncertain) throw new Error("persistence uncertain");
  });
  await authority.withTransaction(async tx => {
    await RunRunner.prototype.withSourceReservation.call(lane as unknown as RunRunner, RUN, async eligible => {
      input = { ...input, checkLive: () => { assert.equal(eligible(), true); assert.equal(authority.active(), tx); } };
      const result = await discard();
      assert.equal(result.kind, uncertain ? "unknown" : "done");
      assert.deepEqual(events, ["barrier"]);
      assert.equal(authority.active(), tx);
      if (result.kind === "done") {
        scope.assertSettled();
        const ended = await authority.endSourceUse([clonePath], { skipFlightOwned: true });
        assert.deepEqual(ended.ended, [clonePath]);
        events.push("ended");
      }
    });
    assert.equal(authority.active(), tx);
  });
  assert.deepEqual(events, uncertain ? ["barrier"] : ["barrier", "ended"]);
  assert.equal(scope.stopped(), uncertain);
});
it("physically deletes a clean published clone and skills then durably retires and clears", async () => {
  const skills = path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
  fs.mkdirSync(skills); fs.writeFileSync(path.join(skills, "generated.md"), "generated");
  const result = await discard();
  assert.equal(result.kind, "done");
  assert.equal(fs.existsSync(clonePath), false); assert.equal(fs.existsSync(skills), false);
  assert.equal(journal(), "");
  assert.equal(ledger().length, 2); assert.equal(JSON.parse(ledger()[1]!).state, "retired");
  assert.equal(stopped, false); assert.equal(pending, 0); assert.ok(nativeLaunches > 0);
});
it("capture observation denies effects and rechecks reservation after awaiting", async () => {
  options.quiesce = async () => false;
  assert.equal((await discard()).kind, "refused");
  assert.equal(journal(), raw); assert.equal(ledger().length, 1); assert.equal(stopped, false);
  options.quiesce = async () => { live = false; return true; };
  assert.equal((await discard()).kind, "unknown");
  assert.ok(fs.existsSync(clonePath)); assert.equal(journal(), raw);
});
for (const effect of [1, 3]) it(`preserves successor at trusted effect boundary ${effect}`, async () => {
  let calls = 0;
  const successor = JSON.stringify({ ...JSON.parse(raw), runId: "successor" });
  input = { ...input, beforeEffect: async () => { if (++calls === effect) setJournal(successor); } };
  assert.equal((await discard()).kind, "unknown");
  assert.equal(journal(), successor); assert.equal(stopped, true);
});
for (const replacement of [false, true]) it(`preserves journal on parent loss after retired barrier: replacement=${replacement}`, async () => {
  fault("retired:after", async () => {
    const parent = path.dirname(clonePath);
    fs.renameSync(parent, parent + "-old");
    if (replacement) fs.mkdirSync(parent);
  });
  assert.equal((await discard()).kind, "unknown");
  assert.equal(journal(), raw); assert.equal(stopped, true);
});
it("settled EACCES keeps records and permits later handover", async () => {
  const original = fs.promises.rm;
  fs.promises.rm = async () => { throw Object.assign(new Error("denied"), { code: "EACCES" }); };
  try {
    assert.equal((await discard()).kind, "incomplete");
    assert.equal(stopped, false); assert.equal(pending, 0);
    assert.equal(journal(), raw); assert.equal(ledger().length, 1);
    scope.assertSettled();
  } finally { fs.promises.rm = original; }
});
it("settles missing-leaf metadata", async () => {
  fs.rmSync(clonePath, { recursive: true }); await refresh();
  assert.equal((await discard()).kind, "done"); assert.equal(journal(), "");
});
it("rediscovers retired strict journal with absent footprint and repeats barriers without duplicate append", async () => {
  fault("retired:after", async () => { throw new Error("interrupted"); });
  assert.equal((await discard()).kind, "unknown"); assert.equal(journal(), raw);
  assert.equal(stopped, true); assert.ok(pending > 0); assert.equal(ledger().length, 2);
  const oldScope = scope;
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  scope = freshScope(); input = { ...input, scope };
  fault("never", async () => {}); await refresh();
  assert.equal(oldScope.stopped(), true); assert.ok(oldScope.pendingMutations > 0);
  assert.equal((await discard()).kind, "done"); assert.equal(ledger().length, 2); assert.equal(journal(), "");
});
// Only ledger live (strict journal), retired with a missing footprint (strict journal) and retired with the
// explicit empty-journal repair are admitted. A reclaimed or abandoned attempt is refused before any effect,
// whichever journal shape the proof carries.
for (const state of ["reclaimed", "abandoned"]) for (const shape of ["empty", "strict"]) {
  it(`refuses a ledger ${state} attempt with the ${shape} journal: tree kept, nothing appended, journal untouched`, async () => {
    const entry = { ...JSON.parse(ledger()[0]!), state };
    gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
    if (shape === "empty") setJournal("");
    await refresh();
    // An absent journal is no longer admissible to the epoch proof constructor.
    assert.throws(() => git.createEpochDiscardProof({ ...input, expectedRawJournal: [] }), /invalid epoch discard proof/);
    input = { ...input, expectedRawJournal: shape === "empty" ? [""] : raw };
    const before = fs.readFileSync(path.join(bare, "config"));
    const result = await discard();
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(clonePath));
    assert.equal(ledger().length, 2); assert.equal(JSON.parse(ledger().at(-1)!).state, state);
    assert.equal(journal(), shape === "empty" ? "" : raw);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
    assert.equal(stopped, false); assert.equal(pending, 0);
  });
}
it("refuses the empty-journal repair while the retired attempt's footprint is still present", async () => {
  const entry = { ...JSON.parse(ledger()[0]!), state: "retired" };
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
  setJournal("");
  input = { ...input, expectedRawJournal: [""] };
  const before = fs.readFileSync(path.join(bare, "config"));
  // The exact repair pair cannot be rediscovered, so the discard is uncertain (UNKNOWN), never done.
  assert.equal((await discard()).kind, "unknown"); assert.equal(stopped, true);
  assert.ok(fs.existsSync(clonePath));
  assert.equal(ledger().length, 2); assert.equal(journal(), "");
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
});
for (const boundary of ["retired", "clear"]) {
  const points = ["before-rawverify", "after-rawverify", ...["config", "directory"].flatMap(kind =>
    ["before-open", "after-open", "before-sync", "after-sync", "before-close", "after-close"].map(point => `${kind}:${point}`))];
  for (const point of points) it(`owns and stops on ${boundary}:${point} uncertainty`, async () => {
    fault(`${boundary}:${point}`, async () => { throw new Error("injected storage uncertainty"); });
    assert.equal((await discard()).kind, "unknown"); assert.equal(stopped, true); assert.ok(pending > 0);
    assert.equal(journal(), boundary === "retired" ? raw : ""); assert.equal(ledger().length, 2);
  });
}
for (const point of ["before-retiredappend", "after-retiredappend", "after-clear"]) it(`stops on ${point}`, async () => {
  fault(point, async () => { throw new Error("injected"); });
  assert.equal((await discard()).kind, "unknown"); assert.equal(stopped, true); assert.ok(pending > 0);
  assert.equal(journal(), point === "after-clear" ? "" : raw);
  assert.equal(ledger().length, point === "before-retiredappend" ? 1 : 2);
});
it("refuses older retired evidence when the latest entry differs before deleting", async () => {
  const entry = JSON.parse(ledger()[0]!);
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify({ ...entry, state: "retired" })]);
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
  await refresh(); assert.equal((await discard()).kind, "refused"); assert.ok(fs.existsSync(clonePath)); assert.equal(journal(), raw);
});
it("keeps a missing ancestor instead of treating it as a missing leaf", async () => {
  fs.rmSync(path.dirname(clonePath), { recursive: true }); await refresh();
  assert.equal((await discard()).kind, "refused"); assert.equal(journal(), raw);
});
it("preserves a successor installed at before-clear", async () => {
  const successor = JSON.stringify({ ...JSON.parse(raw), runId: "successor" });
  fault("before-clear", async () => setJournal(successor));
  assert.equal((await discard()).kind, "unknown");
  assert.equal(journal(), successor); assert.equal(stopped, true);
});
it("rechecks exact journal after retired barrier and never overwrites a successor", async () => {
  const successor = JSON.stringify({ ...JSON.parse(raw), runId: "successor" });
  fault("retired:after", async () => setJournal(successor));
  assert.equal((await discard()).kind, "unknown"); assert.equal(journal(), successor); assert.equal(stopped, true);
});

for (const shape of ["noncanonical", "duplicate"] as const) it(`refuses ${shape} retired evidence`, async () => {
  fs.rmSync(clonePath, { recursive: true });
  const entry = { ...JSON.parse(ledger()[0]!), state: "retired" };
  const value = JSON.stringify(entry);
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, shape === "noncanonical" ? value + " " : value]);
  if (shape === "duplicate") gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, value]);
  await refresh(); assert.equal((await discard()).kind, "refused"); assert.equal(journal(), raw);
});
it("preserves the complete ordered ledger across other branches", async () => {
  const entry = JSON.parse(ledger()[0]!);
  const other = JSON.stringify({ ...entry, attemptId: formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000002"),
    clonePath: clonePath.replace("0000000000000001", "0000000000000002"), runId: "other", state: "retired" });
  gitIn(["config", "--local", "--add", "uzi-attempts.other.entry", other]);
  assert.equal((await discard()).kind, "done");
  assert.equal(gitIn(["config", "--local", "--get-all", "uzi-attempts.other.entry"]), other);
});
for (const shape of ["absent", "duplicate", "valueless", "different"]) it(`refuses ${shape} clear readback`, async () => {
  fault("after-clear", async () => {
    if (shape === "absent" || shape === "valueless") gitIn(["config", "--local", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
    if (shape === "valueless") fs.appendFileSync(path.join(bare, "config"), `\n[uzi-recovery "${BRANCH}"]\nclone\n`);
    if (shape === "duplicate") gitIn(["config", "--local", "--add", `uzi-recovery.${BRANCH}.clone`, ""]);
    if (shape === "different") setJournal(raw);
  });
  assert.equal((await discard()).kind, "unknown"); assert.equal(stopped, true); assert.ok(pending > 0);
  assert.equal(JSON.parse(ledger().at(-1)!).state, "retired");
});
it("keeps source admission running-pending refusal ahead of all physical and metadata effects", async () => {
  (git as unknown as { authority: unknown }).authority = {
    admitSourceUse: async () => { throw new Error("running persistent pending"); },
  };
  assert.equal((await discard()).kind, "refused"); assert.ok(fs.existsSync(clonePath));
  assert.equal(journal(), raw); assert.equal(ledger().length, 1);
});

async function emptyPair(): Promise<void> {
  fault("clear:config:before-sync", async () => { throw new Error("clear sync uncertain"); });
  assert.equal((await discard()).kind, "unknown");
  assert.equal(scope.stopped(), true);
  assert.ok(scope.pendingMutations > 0);
  assert.equal(journal(), "");
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  scope = freshScope();
  input = { ...input, scope, expectedRawJournal: [""] };
}
async function inspectPair(over: Partial<Parameters<GitCache["inspectRetainedSources"]>[0]> = {}) {
  return git.inspectRetainedSources({ selector: options.selector, barePath: bare, holds: options.holds,
    lifecycle: options.lifecycle, epoch: true, isLivePath: () => false, ...over });
}
it("rediscovers the empty pair after step 6 with fresh ownership and repeats both barriers without writes", async t => {
  const oldScope = scope;
  await emptyPair();
  assert.equal(oldScope.stopped(), true);
  assert.ok(oldScope.pendingMutations > 0);
  const operator = await inspectPair({ epoch: false });
  assert.equal(operator.sources.length, 0);
  await refresh();
  const before = fs.readFileSync(path.join(bare, "config"));
  const events: string[] = [];
  (git as unknown as { epochSettlementFault: (step: string) => Promise<void> }).epochSettlementFault = async step => { events.push(step); };
  t.mock.method(fs.promises, "rm", async () => { assert.fail("metadata pair must never delete"); });
  for (let i = 0; i < 2; i++) {
    assert.equal((await discard()).kind, "done");
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
    assert.equal(ledger().length, 2);
    scope.assertSettled();
    if (i === 0) await refresh();
  }
  assert.equal(events.filter(x => x === "retired:config:before-sync").length, 2);
  assert.equal(events.filter(x => x === "clear:config:before-sync").length, 2);
  assert.equal(events.filter(x => x === "retired:directory:before-sync").length, 2);
  assert.equal(events.filter(x => x === "clear:directory:before-sync").length, 2);
  assert.ok(!events.includes("before-retiredappend"));
  assert.ok(!events.includes("before-clear"));
  assert.equal(oldScope.stopped(), true);
  assert.ok(oldScope.pendingMutations > 0);
});
for (const boundary of ["retired:config:before-sync", "clear:directory:after-sync"]) {
  it(`empty pair keeps its ticket pending on ${boundary}`, async () => {
    await emptyPair(); await refresh();
    fault(boundary, async () => { throw new Error("barrier uncertain"); });
    assert.equal((await discard()).kind, "unknown");
    assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
    assert.equal(journal(), ""); assert.equal(ledger().length, 2);
  });
}
for (const shape of ["missing", "valueless", "duplicate", "malformed", "rebound"] as const) {
  it(`does not rediscover an empty pair with ${shape} journal`, async () => {
    await emptyPair();
    if (shape === "missing" || shape === "valueless") gitIn(["config", "--local", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
    if (shape === "valueless") fs.appendFileSync(path.join(bare, "config"), `\n[uzi-recovery "${BRANCH}"]\nclone\n`);
    if (shape === "duplicate") gitIn(["config", "--local", "--add", `uzi-recovery.${BRANCH}.clone`, ""]);
    if (shape === "malformed") setJournal("{");
    if (shape === "rebound") setJournal(JSON.stringify({ ...JSON.parse(raw), runId: "successor" }));
    const result = await inspectPair();
    assert.equal(result.sources.length, 0);
  });
}
for (const shape of ["noncanonical", "extra-field", "contradictory", "shared", "successor"] as const) {
  it(`does not rediscover ambiguous ${shape} ledger evidence`, async () => {
    await emptyPair();
    const entries = ledger(); const entry = JSON.parse(entries[0]!);
    if (shape === "noncanonical" || shape === "extra-field" || shape === "contradictory") {
      gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
      const first = shape === "noncanonical" ? entries[0]! + " " : JSON.stringify({ ...entry,
        ...(shape === "extra-field" ? { extra: true } : { runId: "other" }) });
      gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, first]);
      gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, entries[1]!]);
    } else {
      const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000002");
      gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify({ ...entry, attemptId,
        clonePath: shape === "shared" ? clonePath : clonePath.replace("0000000000000001", "0000000000000002"),
        state: shape === "shared" ? "retired" : "abandoned" })]);
    }
    assert.equal((await inspectPair()).sources.length, 0);
  });
}
it("requires fresh lifecycle, zero holds and no pins for the empty pair", async () => {
  await emptyPair(); await refresh();
  for (const over of [
    { lifecycle: { state: "unknown" as const } },
    { holds: { state: "unknown" as const } },
    { holds: { state: "read" as const, holds: [{ id: "h", generation: 1 }] } },
  ]) {
    const inspection = await inspectPair(over);
    assert.equal(inspection.sources[0]?.epochSettlement, undefined);
    assert.equal(inspection.sources[0]?.disposition, undefined);
  }
  const tip = gitIn(["rev-parse", "HEAD"]);
  gitIn(["update-ref", `refs/uzi-owed/${RUN}/${tip}`, tip]);
  assert.equal((await inspectPair()).sources[0]?.epochSettlement, undefined);
  assert.equal((await discard()).kind, "refused");
  assert.equal(journal(), "");
});
for (const leaf of ["clone", "skills"]) it(`never deletes a ${leaf} reappearing during empty-pair repair`, async t => {
  await emptyPair(); await refresh();
  const target = leaf === "clone" ? clonePath : path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
  fault("retired:after", async () => { fs.mkdirSync(target); fs.writeFileSync(path.join(target, "keep"), "retained"); });
  t.mock.method(fs.promises, "rm", async () => { assert.fail("repair cannot delete reappearing data"); });
  assert.equal((await discard()).kind, "unknown");
  assert.equal(fs.readFileSync(path.join(target, "keep"), "utf8"), "retained");
  assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
});
it("keeps an empty-pair successor journal installed during repair", async () => {
  await emptyPair(); await refresh();
  fault("retired:after", async () => setJournal(raw));
  assert.equal((await discard()).kind, "unknown");
  assert.equal(journal(), raw); assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
});
it("refuses an unreadable empty-pair config without repairing it", async () => {
  await emptyPair();
  fs.appendFileSync(path.join(bare, "config"), "\n[broken\n");
  const before = fs.readFileSync(path.join(bare, "config"));
  const inspection = await inspectPair();
  assert.equal(inspection.sources.length, 0);
  assert.ok(inspection.unreadable > 0);
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
});
for (const change of ["ancestor", "journal-namer"] as const) it(`keeps ambiguous empty-pair ${change} attribution`, async () => {
  await emptyPair();
  if (change === "ancestor") fs.renameSync(path.dirname(clonePath), path.dirname(clonePath) + "-old");
  else gitIn(["config", "--local", "uzi-recovery.other.clone", raw]);
  assert.equal((await inspectPair()).sources.length, 0);
});
it("stops when a new journal namer appears during pair barriers", async () => {
  await emptyPair(); await refresh();
  fault("retired:after", async () => { gitIn(["config", "--local", "uzi-recovery.other.clone", raw]); });
  assert.equal((await discard()).kind, "unknown");
  assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
  assert.equal(gitIn(["config", "--local", "uzi-recovery.other.clone"]), raw);
});

for (const change of ["missing-journal", "rebound-journal", "clone", "skills", "ledger"] as const) {
  it(`keeps a selected empty-pair ${change} mismatch UNKNOWN before B`, async t => {
    await emptyPair(); await refresh();
    const proof = git.createEpochDiscardProof(input);
    if (change === "missing-journal") gitIn(["config", "--local", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
    if (change === "rebound-journal") setJournal(raw);
    if (change === "clone" || change === "skills") {
      const target = change === "clone" ? clonePath : path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
      fs.mkdirSync(target); fs.writeFileSync(path.join(target, "keep"), "retained");
    }
    if (change === "ledger") {
      const entry = { ...JSON.parse(ledger().at(-1)!), state: "abandoned" };
      gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
    }
    const before = fs.readFileSync(path.join(bare, "config"));
    t.mock.method(fs.promises, "rm", async () => { assert.fail("selected repair must not delete"); });
    assert.equal((await git.discardStoppedSource({ ...options, epochProof: proof })).kind, "unknown");
    assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  });
}

for (const retry of [false, true]) {
  it(`joins actual config and directory sync/close before each raw witness: repair=${retry}`, async t => {
    if (retry) { await emptyPair(); await refresh(); }
    const events: string[] = [];
    let next: { boundary: string; kind: string } | undefined;
    (git as unknown as { epochSettlementFault: (name: string) => Promise<void> }).epochSettlementFault = async name => {
      const match = /^(retired|clear):(config|directory):before-open$/.exec(name);
      if (match) next = { boundary: match[1]!, kind: match[2]! };
      if (name === "retired:before-rawverify" || name === "clear:before-rawverify") events.push(name);
      if (name === "before-clear") assert.deepEqual(events, ["retired:sync:config", "retired:close:config",
        "retired:sync:directory", "retired:close:directory", "retired:before-rawverify"]);
    };
    const open = fs.promises.open.bind(fs.promises);
    t.mock.method(fs.promises, "open", async (...args: Parameters<typeof fs.promises.open>) => {
      const handle = await open(...args);
      const owned = next;
      if (owned && args[0] === (owned.kind === "config" ? path.join(bare, "config") : bare)) {
        next = undefined;
        const sync = handle.sync.bind(handle), close = handle.close.bind(handle);
        t.mock.method(handle, "sync", async () => {
          await sync(); events.push(`${owned.boundary}:sync:${owned.kind}`);
        });
        t.mock.method(handle, "close", async () => {
          // An unawaited close must not let raw verification overtake actual closure.
          await new Promise<void>(resolve => setImmediate(resolve));
          await close(); events.push(`${owned.boundary}:close:${owned.kind}`);
        });
      }
      return handle;
    });
    assert.equal((await discard()).kind, "done");
    assert.deepEqual(events, ["retired", "clear"].flatMap(boundary => [
      `${boundary}:sync:config`, `${boundary}:close:config`, `${boundary}:sync:directory`,
      `${boundary}:close:directory`, `${boundary}:before-rawverify`,
    ]));
  });
}

for (const boundary of ["retired", "clear"] as const) {
  it(`refuses an actual ordered ledger mismatch at the ${boundary} raw barrier`, async () => {
    fault(`${boundary}:before-rawverify`, async () => {
      const entry = { ...JSON.parse(ledger()[0]!), runId: "other", state: "retired",
        attemptId: formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000002"),
        clonePath: clonePath.replace("0000000000000001", "0000000000000002") };
      gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
    });
    assert.equal((await discard()).kind, "unknown");
    assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
    assert.equal(journal(), boundary === "retired" ? raw : "");
  });
}

for (const repair of [false, true]) for (const point of ["clear:before-rawverify", "clear:after-rawverify", "clear:after"]) {
  for (const leaf of ["clone", "skills"]) it(`keeps reappeared ${leaf} UNKNOWN at ${point}: repair=${repair}`, async () => {
    if (repair) { await emptyPair(); await refresh(); }
    const target = leaf === "clone" ? clonePath : path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
    fault(point, async () => { fs.mkdirSync(target); fs.writeFileSync(path.join(target, "keep"), "retained"); });
    assert.equal((await discard()).kind, "unknown");
    assert.equal(fs.readFileSync(path.join(target, "keep"), "utf8"), "retained");
    assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
    assert.equal(journal(), ""); assert.equal(ledger().length, 2);
  });
}
for (const point of ["clear:before-rawverify", "clear:after"]) it(`keeps replaced parent UNKNOWN at ${point}`, async () => {
  const parent = path.dirname(clonePath);
  fault(point, async () => { fs.renameSync(parent, parent + "-old"); fs.mkdirSync(parent); });
  assert.equal((await discard()).kind, "unknown");
  assert.equal(scope.stopped(), true); assert.ok(scope.pendingMutations > 0);
  assert.equal(journal(), "");
});


// The worker lifetime lock is per process and per data dir, so only this block acquires it (the caller-owns-end
// test above acquires its own).
describe("M1b epoch reclaim over real Git and authority", () => {
// ---- M1b: the coordinator over a REAL GitCache and AuthorityRuntime, crash recovery through fresh instances ----
const evidenceBytes = fs.readFileSync(new URL("../../fixtures/pod-epoch/v2/positive.json", import.meta.url));
const evidenceDoc = JSON.parse(evidenceBytes.toString());
const TOKEN = "epoch-reclaim-" + randomBytes(12).toString("hex");
let lifetime: WorkerLifetime;
interface Stack {
  authority: AuthorityRuntime; git: GitCache; scope: PodEpochReclaimScope;
  state: { pending: number; stopped: boolean; finished: number };
  /** Built through main's production wiring (buildWorkerGitCache and buildPodEpochDeletionDeps). */
  production: boolean;
}
/** A FRESH GitCache plus a FRESH AuthorityRuntime (new incarnation, same worker lifetime) and a fresh native scope. */
function stack(ops?: AuthorityFsOps, production = false): Stack {
  const authority = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000,
    ...(ops ? { ops } : {}) });
  const g = production
    ? buildWorkerGitCache({ dataDir: fx.dataDir, recoveryDecodedLimitBytes: 8 * 1024 * 1024 }, nullLogger(), async () => false, authority)
    : new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: authority.hooks }));
  const state = { pending: 0, stopped: false, finished: 0 };
  const scope: PodEpochReclaimScope = {
    assertExclusive() { if (state.stopped) throw new Error("stopped"); },
    get pendingMutations() { return state.pending; },
    beginMutation() {
      state.pending++; let settled = false;
      return { settle() { if (!settled) { settled = true; state.pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
    stop() { state.stopped = true; }, stopped: () => state.stopped,
    assertSettled() { if (state.stopped || state.pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); state.finished++; },
  };
  return { authority, git: g, scope, state, production };
}
async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}
function faultAt(s: Stack, step: string, action: () => Promise<void>) {
  (s.git as unknown as { epochSettlementFault: (name: string) => Promise<void> }).epochSettlementFault = async name => {
    if (name === step) await action();
  };
}
type Outcome = { result?: NonNullable<Awaited<ReturnType<typeof runPodEpochShadow>>>; error?: unknown; handovers: number };
async function epoch(s: Stack, tune?: (o: Parameters<typeof runPodEpochShadow>[0], deps: PodEpochDeletionDeps) => void): Promise<Outcome> {
  const d = await daemon();
  const timers: Array<() => void> = [];
  const lane = { executionTails: new Map(), inventoryReservations: new Map(), activeRuns: new Map() };
  let handovers = 0;
  const ownership: PodEpochOwnership = {
    mode: "exclusive", beginReclaim: () => s.scope,
    consume: async () => { throw new Error("consume already performed"); },
    matchesEntry: () => true, reacquireShared: async () => { handovers++; }, close: () => {},
  };
  const reserve = ((runId: string, action: (eligible: () => boolean) => Promise<unknown>) =>
    RunRunner.prototype.withSourceReservation.call(lane as unknown as RunRunner, runId, action)) as PodEpochDeletionDeps["reserve"];
  // A production stack uses main's own deps builder (no enableForTest); the test injects the sentinel through `tune`.
  const deps: PodEpochDeletionDeps = s.production
    ? buildPodEpochDeletionDeps({ authority: s.authority, runner: { withSourceReservation: reserve as never }, git: s.git, dockerHost: d.host })
    : { enableForTest: ENABLE_DELETION_FOR_TEST, authority: s.authority, reserve, quiesce: async () => true, git: s.git };
  const options: Parameters<typeof runPodEpochShadow>[0] = {
    bootstrap: { podUid: evidenceDoc.successor, runtime: evidenceDoc.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
    ownership, consumption: { result: "fresh", mayEvaluate: true }, signal: new AbortController().signal,
    dockerHost: d.host, readEvidence: async () => evidenceBytes, kernelRelease: () => "6.1.0-test",
    client: {
      listRecoveryHolds: async run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: 0 } }),
      getRunOwnership: async () => ({ status: "completed" }),
    },
    git: s.git, deletion: deps,
    schedule: (callback, ms) => { const t = setTimeout(callback, ms); const c = () => clearTimeout(t); timers.push(c); return c; },
  };
  tune?.(options, deps);
  try { return { result: (await runPodEpochShadow(options))!, handovers }; }
  catch (error) { return { error, handovers }; }
  finally { for (const cancel of timers) cancel(); await d.close(); }
}
beforeEach(async () => { lifetime = await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions()); });

it("M1b: an eligible attested pass physically reclaims a clean epoch source through the real coordinator", async () => {
  const s = stack();
  const out = await epoch(s);
  assert.equal(out.error, undefined);
  assert.equal(out.result!.would_delete, 1);
  assert.equal(out.result!.deleted, 1); assert.equal(out.result!.verdict, "eligible"); assert.equal(out.result!.reason, "reclaim_complete");
  assert.equal(fs.existsSync(clonePath), false); assert.equal(journal(), "");
  assert.equal(JSON.parse(ledger().at(-1)!).state, "retired");
  assert.equal(s.state.finished, 1); assert.equal(out.handovers, 1);
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
});

for (const fault of ["chmod", "entry cap"] as const) {
  it(`#3851 a source whose deletion re-inspection turns tree-locally short (${fault}) is refused before any proof or discard`, async t => {
    if (fault === "chmod" && process.getuid?.() === 0) { t.skip("root bypasses directory permissions"); return; }
    const locked = path.join(clonePath, "locked-subdir");
    fs.mkdirSync(locked);
    const journalBefore = journal(), ledgerBefore = ledger();
    const s = stack();
    const calls = { inspections: 0, proofs: 0, discards: 0 };
    const reports: Array<{ partial: boolean; shortfall: string | undefined; bytesState: string }> = [];
    try {
      const out = await epoch(s, (o, deps) => {
        const real = s.git;
        o.git = {
          discoverEpochRetainedSources: opts => real.discoverEpochRetainedSources(opts),
          inspectRetainedSources: async opts => {
            const second = ++calls.inspections === 2;
            if (second && fault === "chmod") fs.chmodSync(locked, 0o000);
            const result = await real.inspectRetainedSources(second && fault === "entry cap" ? { ...opts, measureCaps: { maxEntries: 1 } } : opts);
            if (second) {
              const source = result.sources[0]!;
              reports.push({ partial: result.partial, shortfall: source.sources[0]!.measurementShortfall, bytesState: source.bytesState });
            }
            return result;
          },
        };
        deps.git = {
          assertNoUnsettledDeletes: () => real.assertNoUnsettledDeletes(),
          readEpochJournalVector: (...args) => real.readEpochJournalVector(...args),
          createEpochDiscardProof: (...args) => { calls.proofs++; return real.createEpochDiscardProof(...args); },
          discardStoppedSource: (...args) => { calls.discards++; return real.discardStoppedSource(...args); },
        };
      });
      assert.equal(out.error, undefined);
      assert.equal(calls.inspections, 2, "one shadow inspection and one deletion re-inspection");
      assert.deepEqual(reports, [{ partial: false, shortfall: fault === "chmod" ? "eacces" : "entry_cap", bytesState: "unknown" }]);
      assert.equal(calls.proofs, 0); assert.equal(calls.discards, 0);
      assert.equal(out.result!.would_delete, 1);
      assert.equal(out.result!.deleted, 0);
      assert.equal(out.result!.kept_after_refusal, 1);
      assert.equal(out.handovers, 1);
      assert.ok(fs.existsSync(clonePath), "the source is kept");
      assert.equal(journal(), journalBefore); assert.deepEqual(ledger(), ledgerBefore);
    } finally { fs.chmodSync(locked, 0o700); }
  });
}

/** A process-death emulation for the authority store: the Nth slot or head rename fails and every later one too. */
function crashOps() {
  const control = { dead: false, failSlot: 0, failHead: 0, slot: 0, head: 0 };
  const ops: AuthorityFsOps = {
    syncFile: h => h.sync(), syncDir: h => h.sync(),
    rename: async (a, b) => {
      if (control.dead) throw new Error("process dead");
      const kind = path.basename(path.dirname(b)) === "heads" ? "head" : "slot";
      const n = ++control[kind];
      if (n === (kind === "slot" ? control.failSlot : control.failHead)) { control.dead = true; throw new Error("injected crash"); }
      fs.renameSync(a, b);
    },
  };
  return { control, ops };
}
async function crashed(arm: (control: ReturnType<typeof crashOps>["control"], s: Stack) => void) {
  const { control, ops } = crashOps();
  const s = stack(ops);
  await slotOf(s.authority, clonePath); // open the store first, so only the discard's saves are counted
  control.slot = 0; control.head = 0;
  arm(control, s);
  const out = await epoch(s);
  return { out, s, control };
}
/** What a restarted worker sees: a FRESH GitCache, a FRESH AuthorityRuntime and a fresh native scope. */
async function afterRestart() {
  const fresh = stack();
  return { fresh, slot: await slotOf(fresh.authority, clonePath) };
}
const crashRows: Array<{
  name: string; arm: (control: ReturnType<typeof crashOps>["control"], s: Stack) => void;
  first: "refused" | "fatal"; slot: (slot: SlotSnapshot | null) => void; next: "retaken" | "settled" | "kept";
}> = [
  { name: "before the takeover save", arm: c => { c.failSlot = 1; }, first: "refused",
    slot: slot => assert.equal(slot, null), next: "retaken" },
  { name: "inside the takeover save (slot renamed, head stale)", arm: c => { c.failHead = 1; }, first: "refused",
    slot: slot => { assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending ?? null, null); }, next: "retaken" },
  { name: "after the takeover, before beginDestructive", arm: c => { c.failSlot = 2; }, first: "fatal",
    slot: slot => { assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending ?? null, null); }, next: "retaken" },
  { name: "inside the beginDestructive save", arm: c => { c.failHead = 2; }, first: "fatal",
    slot: slot => { assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending?.status, "running"); }, next: "kept" },
  { name: "after the beginDestructive save, before its settle", arm: c => { c.failSlot = 3; }, first: "fatal",
    slot: slot => { assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending?.status, "running"); }, next: "kept" },
];
for (const row of crashRows) it(`crash table: ${row.name}`, async () => {
  const { out, s } = await crashed(row.arm);
  if (row.first === "fatal") assert.ok(out.error instanceof PodEpochSettlementError, String(out.error));
  else { assert.equal(out.error, undefined); assert.equal(out.result!.kept_after_refusal, 1); assert.equal(out.result!.verdict, "partial"); }
  assert.equal(out.handovers, row.first === "refused" ? 1 : 0, "a fatal row never reaches the shared handover");
  assert.equal(s.state.stopped || s.state.pending > 0, row.first === "fatal");
  const { fresh, slot } = await afterRestart();
  row.slot(slot);
  const next = await epoch(fresh);
  assert.equal(next.error, undefined);
  if (row.next === "kept") {
    assert.equal(next.result!.would_delete + next.result!.would_settle, 1, "still discoverable");
    assert.equal(next.result!.kept_after_refusal, 1); assert.equal(next.result!.deleted + next.result!.settled, 0);
    assert.equal(next.result!.verdict, "partial"); assert.equal(next.result!.reason, "reclaim_partial");
    assert.equal((await slotOf(stack().authority, clonePath))?.pending?.status, "running");
  } else {
    assert.equal(next.result!.reason, "reclaim_complete");
    assert.equal(next.result!.deleted + next.result!.settled, 1);
    assert.equal(fs.existsSync(clonePath), false); assert.equal(journal(), "");
    assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
  }
});

// Rows driven by a Git-side settlement fault: the fault IS the crash, so the authority store dies with it.
for (const row of [
  { name: "after settle ok, before the retired append", step: "before-retiredappend", ledgerAfter: 1 },
  { name: "after the retired append, before the clear (shape c)", step: "before-clear", ledgerAfter: 2 },
]) it(`crash table: ${row.name}`, async () => {
  const { control, ops } = crashOps();
  const s = stack(ops);
  await slotOf(s.authority, clonePath);
  faultAt(s, row.step, async () => { control.dead = true; throw new Error("process died"); });
  const out = await epoch(s);
  assert.ok(out.error instanceof PodEpochSettlementError); assert.equal(out.handovers, 0);
  assert.equal(fs.existsSync(clonePath), false); assert.equal(ledger().length, row.ledgerAfter); assert.equal(journal(), raw);
  const { fresh, slot } = await afterRestart();
  assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending ?? null, null);
  const next = await epoch(fresh);
  assert.equal(next.error, undefined);
  // The footprint is already gone: metadata settlement only, and the retired append is never duplicated.
  assert.equal(next.result!.settled, 1); assert.equal(next.result!.deleted, 0);
  assert.equal(next.result!.verdict, "eligible"); assert.equal(next.result!.reason, "reclaim_complete");
  assert.equal(ledger().length, 2); assert.equal(JSON.parse(ledger()[1]!).state, "retired"); assert.equal(journal(), "");
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
});
it("crash table: after settle failed the slot stays retiring + settled_failed, is ordinarily refused, then retaken and completed", async () => {
  const original = fs.promises.rm;
  fs.promises.rm = async () => { throw Object.assign(new Error("denied"), { code: "EACCES" }); };
  let first: Outcome;
  const s = stack();
  try { first = await epoch(s); } finally { fs.promises.rm = original; }
  assert.equal(first.error, undefined);
  assert.equal(first.result!.kept_residue, 1); assert.equal(first.result!.deleted, 0);
  assert.equal(first.result!.verdict, "partial"); assert.equal(first.result!.reason, "reclaim_partial");
  assert.equal(first.handovers, 1); assert.equal(s.state.finished, 1);
  assert.ok(fs.existsSync(clonePath)); assert.equal(journal(), raw); assert.equal(ledger().length, 1);
  const { fresh, slot } = await afterRestart();
  assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending?.status, "settled_failed");
  // Ordinary admission is refused while the retirement is unfinished.
  await assert.rejects(fresh.authority.withTransaction(() => fresh.authority.hooks.admitSourceUse(clonePath, "seed-attempt")),
    /unresolved pending footprint/);
  const next = await epoch(fresh);
  assert.equal(next.error, undefined);
  assert.equal(next.result!.deleted, 1); assert.equal(next.result!.reason, "reclaim_complete");
  assert.equal(fs.existsSync(clonePath), false); assert.equal(journal(), "");
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
});
it("crash table: after the clear but before the end save (shape b) the next epoch runs the barriers only and tombstones", async t => {
  const { control, ops } = crashOps();
  const s = stack(ops);
  await slotOf(s.authority, clonePath);
  control.failSlot = 4; // the end-of-use save; saves 1-3 are the takeover, beginDestructive and its settle
  const out = await epoch(s);
  assert.equal(out.error, undefined);
  assert.equal(fs.existsSync(clonePath), false); assert.equal(journal(), ""); assert.equal(ledger().length, 2);
  const { fresh, slot } = await afterRestart();
  assert.equal(slot?.state, "retiring");
  t.mock.method(fs.promises, "rm", async () => { assert.fail("shape b is metadata barriers only"); });
  const before = ledger();
  const next = await epoch(fresh);
  assert.equal(next.error, undefined);
  assert.equal(next.result!.settled, 1); assert.equal(next.result!.deleted, 0); assert.equal(next.result!.reason, "reclaim_complete");
  assert.deepEqual(ledger(), before, "no duplicate append");
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
});
it("crash table: after the end save the tombstone is idempotent, unlike a settled keep", async t => {
  const first = await epoch(stack());
  assert.equal(first.result!.deleted, 1);
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
  t.mock.method(fs.promises, "rm", async () => { assert.fail("nothing is left to delete"); });
  const again = await epoch(stack());
  assert.equal(again.error, undefined); assert.equal(again.result!.deleted, 0);
  assert.equal(again.result!.verdict, "eligible");
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired");
  assert.equal(journal(), ""); assert.equal(ledger().length, 2);
});
it("a head-write failure after the takeover slot rename is completed by the next epoch; a read-only classification refuses it", async () => {
  const { control, ops } = crashOps();
  const s = stack(ops);
  await slotOf(s.authority, clonePath);
  control.failHead = 1;
  const out = await epoch(s);
  assert.equal(out.error, undefined);
  assert.equal(out.result!.kept_after_refusal, 1); assert.equal(out.result!.verdict, "partial");
  assert.ok(fs.existsSync(clonePath), "the source is kept");
  // A read-only classification never completes the slot that is one ahead of its head: it answers `other`.
  const fresh = stack();
  assert.equal((await fresh.authority.classifySourceAdmission(clonePath, undefined)).kind, "other");
  const next = await epoch(fresh);
  assert.equal(next.result!.deleted, 1); assert.equal(fs.existsSync(clonePath), false);
});
it("a successor journal installed during the pass is never cleared, even by the next epoch", async () => {
  const successor = JSON.stringify({ ...JSON.parse(raw), runId: "successor" });
  const { control, ops } = crashOps();
  const s = stack(ops);
  await slotOf(s.authority, clonePath);
  faultAt(s, "before-clear", async () => { setJournal(successor); control.dead = true; });
  const out = await epoch(s);
  assert.ok(out.error instanceof PodEpochSettlementError);
  assert.equal(journal(), successor);
  const next = await epoch(stack());
  assert.equal(journal(), successor);
  assert.equal(next.result?.deleted ?? 0, 0);
});

async function plantSecond() {
  const run = "run-epoch-discard-2", branch = "agent/issue-3432";
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000002");
  const clone = await git.runnerCloneForBranch(bare, branch, "issue-3432", noProofReseed, run, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, branch, run, attempt);
  return { path: clone.path, run, branch };
}
it("mixed pass: a deleted source and a pre-effect refusal; the refused slot is restored and usable", async () => {
  const second = await plantSecond();
  const s = stack();
  const out = await epoch(s, (_o, deps) => { deps.quiesce = async paths => !paths.includes(second.path); });
  assert.equal(out.error, undefined);
  assert.equal(out.result!.would_delete, 2);
  assert.equal(out.result!.deleted, 1); assert.equal(out.result!.kept_after_refusal, 1);
  assert.equal(out.result!.verdict, "partial"); assert.equal(out.result!.reason, "reclaim_partial");
  assert.equal(fs.existsSync(clonePath), false); assert.ok(fs.existsSync(second.path));
  assert.equal(out.handovers, 1); assert.equal(s.state.finished, 1);
  const fresh = stack();
  assert.equal((await slotOf(fresh.authority, clonePath))?.state, "retired");
  assert.equal(await slotOf(fresh.authority, second.path), null, "the refused source was never taken over");
  // The refused slot is usable: an ordinary admission succeeds and a retry of the epoch reclaims it.
  await fresh.authority.withTransaction(() => fresh.authority.hooks.admitSourceUse(second.path, "seed-attempt"));
  assert.equal((await slotOf(fresh.authority, second.path))?.state, "active");
  const next = await epoch(fresh);
  assert.equal(next.error, undefined); assert.equal(next.result!.deleted, 1);
  assert.equal(fs.existsSync(second.path), false);
});
it("mixed pass: a deleted source and a residue failure; the residue slot is retaken and completed by the next epoch", async () => {
  const second = await plantSecond();
  const original = fs.promises.rm;
  const guarded = ((target: string, ...rest: unknown[]) => String(target).startsWith(second.path) ?
    Promise.reject(Object.assign(new Error("denied"), { code: "EACCES" })) :
    (original as (...a: unknown[]) => Promise<void>)(target, ...rest)) as typeof fs.promises.rm;
  fs.promises.rm = guarded;
  let out: Outcome;
  try { out = await epoch(stack()); } finally { fs.promises.rm = original; }
  assert.equal(out.error, undefined);
  assert.equal(out.result!.deleted, 1); assert.equal(out.result!.kept_residue, 1);
  assert.equal(out.result!.verdict, "partial"); assert.equal(out.result!.reason, "reclaim_partial");
  assert.equal(fs.existsSync(clonePath), false); assert.ok(fs.existsSync(second.path));
  const fresh = stack();
  assert.equal((await slotOf(fresh.authority, clonePath))?.state, "retired");
  const slot = await slotOf(fresh.authority, second.path);
  assert.equal(slot?.state, "retiring"); assert.equal(slot?.pending?.status, "settled_failed");
  await assert.rejects(fresh.authority.withTransaction(() => fresh.authority.hooks.admitSourceUse(second.path, "seed-attempt")),
    /unresolved pending footprint/);
  const next = await epoch(fresh);
  assert.equal(next.error, undefined); assert.equal(next.result!.reason, "reclaim_complete");
  assert.equal(fs.existsSync(second.path), false);
  assert.equal((await slotOf(stack().authority, second.path))?.state, "retired");
});
it("with the production flag value a real eligible attested pass mutates nothing", async () => {
  const s = stack();
  const before = fs.readFileSync(path.join(bare, "config"));
  const out = await epoch(s, (o) => { delete o.deletion!.enableForTest; });
  assert.equal(out.error, undefined);
  assert.equal(out.result!.would_delete, 1); assert.equal(out.result!.deleted, 0);
  assert.equal(out.result!.verdict, "eligible"); assert.equal(out.result!.reason, "shadow_complete");
  assert.ok(fs.existsSync(clonePath)); assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(await slotOf(stack().authority, clonePath), null);
  assert.equal(s.state.finished, 0);
});
it("main's production wiring: with the production flag an eligible attested pass mutates nothing", async () => {
  const s = stack(undefined, true);
  const before = fs.readFileSync(path.join(bare, "config"));
  const out = await epoch(s, o => { assert.equal("enableForTest" in o.deletion!, false); });
  assert.equal(out.error, undefined);
  assert.equal(out.result!.would_delete, 1); assert.equal(out.result!.deleted, 0);
  assert.equal(out.result!.verdict, "eligible"); assert.equal(out.result!.reason, "shadow_complete");
  assert.equal(out.result!.reclaim_bytes_state, "unknown");
  assert.ok(fs.existsSync(clonePath)); assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(await slotOf(stack().authority, clonePath), null);
  assert.equal(s.state.finished, 0); assert.equal(out.handovers, 1);
});
it("main's production wiring: with the test sentinel injected in the test only the same wiring deletes end to end", async () => {
  const s = stack(undefined, true);
  // Only the activation sentinel and the deny-only process proof (which needs a recorded maintenance bootstrap and a
  // process scan) are replaced; authority, reservation and Git are main's production bindings.
  const out = await epoch(s, (_o, deps) => { deps.enableForTest = ENABLE_DELETION_FOR_TEST; deps.quiesce = async () => true; });
  assert.equal(out.error, undefined);
  assert.equal(out.result!.deleted, 1); assert.equal(out.result!.verdict, "eligible"); assert.equal(out.result!.reason, "reclaim_complete");
  assert.equal(fs.existsSync(clonePath), false); assert.equal(journal(), "");
  assert.equal(out.result!.source_use_not_ended, 0);
  assert.equal(s.state.finished, 1); assert.equal(out.handovers, 1);
  assert.equal((await slotOf(stack().authority, clonePath))?.state, "retired", "read through a fresh AuthorityRuntime");
});
it("an unsettled runner delete recorded by Git is fatal: EX stays held and no handover happens", async t => {
  const s = stack(undefined, true);
  // Git's unsettled-delete record (a runner-uid rm that may still be running) turns a failed delete into an
  // unproven outcome instead of a settled residue.
  (s.git as unknown as { unsettledDeletes: Map<string, number> }).unsettledDeletes.set(path.resolve(clonePath), 1);
  t.mock.method(fs.promises, "rm", async () => { throw Object.assign(new Error("denied"), { code: "EACCES" }); });
  const out = await epoch(s, (_o, deps) => { deps.enableForTest = ENABLE_DELETION_FOR_TEST; deps.quiesce = async () => true; });
  assert.ok(out.error instanceof PodEpochSettlementError, String(out.error));
  assert.equal(out.handovers, 0); assert.equal(s.state.finished, 0);
  assert.ok(fs.existsSync(clonePath));
});
it("a no-key attempt is an unrecoverable shape: the shadow counts it, the coordinator refuses it before any proof", async () => {
  const entry = { ...JSON.parse(ledger()[0]!), state: "abandoned" };
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
  gitIn(["config", "--local", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
  const before = fs.readFileSync(path.join(bare, "config"));
  let proofs = 0;
  const s = stack();
  const create = s.git.createEpochDiscardProof.bind(s.git);
  s.git.createEpochDiscardProof = input => { proofs++; return create(input); };
  const out = await epoch(s);
  assert.equal(out.error, undefined);
  assert.equal(out.result!.would_delete, 1);
  assert.equal(out.result!.kept_unrecoverable_shape, 1); assert.equal(out.result!.deleted, 0);
  assert.equal(out.result!.verdict, "eligible"); assert.equal(out.result!.reason, "reclaim_complete");
  assert.equal(proofs, 0); assert.ok(fs.existsSync(clonePath));
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(await slotOf(stack().authority, clonePath), null);
});
});
