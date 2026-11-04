import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { RunRunner } from "../src/runner.js";
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
for (const snapshot of [[], [""]] as string[][]) it(`preserves authorized inactive raw journal shape ${JSON.stringify(snapshot)}`, async () => {
  const entry = { ...JSON.parse(ledger()[0]!), state: "abandoned" };
  gitIn(["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify(entry)]);
  if (snapshot.length) setJournal(""); else gitIn(["config", "--local", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
  await refresh(); input = { ...input, expectedRawJournal: snapshot };
  assert.equal((await discard()).kind, "done");
  const list = execFileSync("git", ["-C", bare, "config", "--local", "--null", "--list"], { env: ENV, encoding: "utf8" });
  assert.equal(list.includes(`uzi-recovery.${BRANCH}.clone\n\0`), snapshot.length === 1);
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
