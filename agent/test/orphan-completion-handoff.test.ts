import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { AuthorityLockBusy, AuthorityRefusal } from "../src/authority-lock.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { parseCompletionSourceV2, type CompletionSourceFence, type CompletionSourceV2 } from "../src/completion-source.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { nullLogger } from "./helpers.js";
import { legacyCompletionCore, type HandoffRetirement, type HandoffRetirementInput } from "../src/recovery.js";
import { api, git, gitlabClaim, installHarness } from "./runner-harness.js";
import {
  ATTEMPT_ROWS, ROWS, STOPPED, OWNER_FILE, assertUnchanged, diagnostics, durable, failure, holdCycle, reported, scene,
  type Scene,
} from "./orphan-handoff-scene.js";
import {
  BRANCH, ENV, GENERATION, fenceOf, journalRaw, readRecords, recordDir, recordNames, rewriteSources, rig, slotVia, snapshotFiles, sourceOf,
  type PrivateRunner, type Rig,
} from "./completion-retirement-fixture.js";

installHarness();

// issue #3645 M2 - the coordinator half of the terminal-owner handoff: RecoveryCoordinator.retireCompletedSourceForHandoff
// retires (or classifies) a terminal owner's completed source on behalf of a successor claimant. Real authority store,
// real recovery coordinator, real Git; no runner involved. The claimant routing is M3's.

const linux = { skip: process.platform !== "linux" };
const cloneAlive = (r: Rig): boolean => fs.existsSync(path.join(r.clone, ".git"));

/** Once the retirement's preparation has built its redundancy claims (past the durable verification), every lstat of a
 *  bundle file fails with an EIO whose message carries the owner path. */
function failBundleLstat(t: TestContext, r: Rig, recovery: object): void {
  let armed = false;
  const internals = recovery as { buildRedundancyClaims(...a: unknown[]): Promise<unknown> };
  const build = internals.buildRedundancyClaims.bind(recovery);
  t.mock.method(internals, "buildRedundancyClaims", (async (...a: unknown[]) => { armed = true; return build(...a); }) as never);
  const real = fs.promises.lstat.bind(fs.promises) as (p: string, ...rest: unknown[]) => Promise<unknown>;
  t.mock.method(fs.promises, "lstat", (async (p: string, ...rest: unknown[]) => {
    if (armed && String(p).endsWith(".bundle")) throw Object.assign(new Error(`EIO: i/o error, lstat '${r.clone}'`), { code: "EIO" });
    return real(p, ...rest);
  }) as never);
}

async function published(t: TestContext, options: { codex?: boolean; cohort?: number } = {}): Promise<Rig> {
  const r = await rig(t, { codex: options.codex ?? false, cohort: options.cohort ?? 2 });
  await r.end();
  await r.receipt();
  return r;
}

/** A restarted incarnation: it owns no in-memory binding, exactly like a successor claimant's process view. */
function claimant(r: Rig): { p: PrivateRunner; rt: Rig["rt"]; handoff(over?: Partial<HandoffRetirementInput>): Promise<HandoffRetirement> } {
  const second = r.restart();
  // Captured now: the journal and the records are gone once the retirement has run.
  const attemptId = sourceOf(readRecords(r)[0]!).attemptId;
  const fence = fenceOf(r) as CompletionSourceFence;
  return {
    p: second.p, rt: second.rt,
    handoff: over => second.p.recovery.retireCompletedSourceForHandoff({
      runId: r.runId, clonePath: r.clone, attemptId, fence, ownerCompleted: true, wait: { timeoutMs: 5000 }, ...over,
    }),
  };
}

/** Nothing the owner holds moved: clone, skills dir, records and bundles, the journal and its fence, the slot. */
async function assertUntouched(r: Rig, files: Record<string, string>, journal: string): Promise<void> {
  assert.ok(cloneAlive(r), "the owner clone stays");
  assert.ok(fs.existsSync(r.skills), "the skills dir stays");
  assert.deepEqual(snapshotFiles(r), files, "no record or bundle changed");
  assert.equal(journalRaw(r), journal, "the journal is byte-identical");
  assert.ok(fenceOf(r), "the journal fence stays");
}

describe("retireCompletedSourceForHandoff: retire", linux, () => {
  for (const codex of [false, true]) {
    it(`a valid published source with a receipt is retired through the certified chain (${codex ? "Codex" : "Claude"})`, async t => {
      const r = await published(t, { codex, cohort: 3 });
      const c = claimant(r);
      assert.deepEqual(await c.handoff(), { kind: "retired" });
      assert.equal(fs.existsSync(r.clone), false, "the owner clone is gone");
      assert.equal(fs.existsSync(r.skills), false, "its skills dir is gone");
      assert.equal(journalRaw(r), "", "the fence-carrying journal entry is cleared");
      assert.equal(fenceOf(r), undefined);
      for (const id of r.captureIds) {
        assert.equal(fs.existsSync(path.join(recordDir(r), `${id}.json`)), false);
        assert.equal(fs.existsSync(path.join(recordDir(r), `${id}.bundle`)), false);
      }
      assert.deepEqual(recordNames(r), []);
      assert.equal((await slotVia(c.rt, r.clone))?.state, "retired");
    });
  }

  it("a second call after the retirement finds nothing and is not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    assert.deepEqual(await c.handoff(), { kind: "retired" });
    assert.deepEqual(await c.handoff(), { kind: "not_completion" });
  });

  it("a busy capture cycle is pending/cycle_busy and nothing changes", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    // pin() takes the generation's cycle lock C (and no A), so a held pin is a busy C under a free A.
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let entered!: () => void;
    const inside = new Promise<void>(resolve => { entered = resolve; });
    t.mock.method(c.p.recovery as unknown as { pinCycle(): Promise<undefined> }, "pinCycle", async () => { entered(); await gate; return undefined; });
    const head = (await c.p.recovery.inspect(r.runId))[0]!.sourceSha;
    const holder = c.p.recovery.pin({ runId: r.runId, generation: GENERATION, kind: "issue", branch: BRANCH, sourceSha: head, inventoryGuarded: true });
    await inside;
    try {
      assert.deepEqual(await c.handoff(), { kind: "pending", why: "cycle_busy" });
      await assertUntouched(r, files, journal);
    } finally {
      release();
      await holder;
    }
    assert.deepEqual(await c.handoff(), { kind: "retired" }, "the free cycle retires");
  });

  it("A held in another transaction past the wait is pending/authority_busy and nothing changes", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let held!: () => void;
    const started = new Promise<void>(resolve => { held = resolve; });
    const holder = c.rt.withTransaction(async () => { held(); await gate; });
    await started;
    try {
      assert.deepEqual(await c.handoff({ wait: { timeoutMs: 100 } }), { kind: "pending", why: "authority_busy" });
      await assertUntouched(r, files, journal);
    } finally {
      release();
      await holder;
    }
  });

  it("an aborted wait for A propagates and is not pending", async t => {
    const r = await published(t);
    const c = claimant(r);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let held!: () => void;
    const started = new Promise<void>(resolve => { held = resolve; });
    const holder = c.rt.withTransaction(async () => { held(); await gate; });
    await started;
    const controller = new AbortController();
    try {
      const attempt = c.handoff({ wait: { signal: controller.signal, timeoutMs: 5000 } });
      controller.abort(new Error("cancelled"));
      await assert.rejects(attempt, /cancelled/);
    } finally {
      release();
      await holder;
    }
  });

  it("no receipt: a completed owner is pending, any other owner is invalid", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff({ ownerCompleted: true }), { kind: "pending", why: "no_receipt" });
    assert.deepEqual(await c.handoff({ ownerCompleted: false }), { kind: "invalid", why: "no_receipt" });
    await assertUntouched(r, files, journal);
  });

  it("a receipt alone never retires: with no published source the fence selects nothing", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.receipt();
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    const second = r.restart();
    const result = await second.p.recovery.retireCompletedSourceForHandoff({
      runId: r.runId, clonePath: r.clone, fence: { version: 1, sourceUseEpoch: "99999999-9999-4999-8999-999999999999", sourceCoreDigest: "a".repeat(64) },
      ownerCompleted: true, wait: { timeoutMs: 5000 },
    });
    assert.deepEqual(result, { kind: "not_completion" });
    assert.ok(cloneAlive(r));
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal(journalRaw(r), journal);
  });

  it("a broken certificate is invalid and nothing is retired", async t => {
    const r = await published(t);
    const flip = (hex: string): string => (hex.startsWith("0") ? "1" : "0") + hex.slice(1);
    await rewriteSources(r, s => ({ ...s, certificate: flip(s.certificate as string) }));
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "invalid", why: "unverified" });
    await assertUntouched(r, files, journal);
  });

  it("a fence of another epoch selects nothing: not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    const fence = { ...(fenceOf(r) as CompletionSourceFence), sourceUseEpoch: "99999999-9999-4999-8999-999999999999" };
    assert.deepEqual(await c.handoff({ fence }), { kind: "not_completion" });
    await assertUntouched(r, files, journal);
  });

  it("the fence matches but the clone path or attempt differs: invalid", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff({ clonePath: `${r.clone}-other` }), { kind: "invalid", why: "path_mismatch" });
    assert.deepEqual(await c.handoff({ attemptId: "20261011T000000Z-g29-bbbbbbbbbbbbbbbb" }), { kind: "invalid", why: "path_mismatch" });
    await assertUntouched(r, files, journal);
  });

  it("an unauthenticated record of the run is uncertain, never invalid or not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    fs.writeFileSync(path.join(recordDir(r), "00000000-0000-4000-8000-0000000000ee.json"), "{\"not\":\"authenticated\"}");
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "uncertain", why: "unreadable_record" });
    assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "uncertain", why: "unreadable_record" });
    assert.ok(cloneAlive(r));
    assert.equal(journalRaw(r), journal);
  });

  it("a coordinator without an authority scope answers not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    Object.assign(c.p.recovery, { authority: undefined });
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "not_completion" });
    await assertUntouched(r, files, journal);
  });

  it("a coordinator without a completion boundary is unsupported", async t => {
    const r = await published(t);
    const c = claimant(r);
    Object.assign(c.p.recovery, { withCompletionSourceBoundary: undefined });
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "unsupported", why: "no_boundary" });
    await assertUntouched(r, files, journal);
  });

  it("a live owner execution tail is pending/executing and nothing changes", async t => {
    const r = await published(t);
    const c = claimant(r);
    (c.p as unknown as { executionTails: Map<string, Promise<void>> }).executionTails.set(r.runId, new Promise(() => {}));
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "executing" });
    await assertUntouched(r, files, journal);
  });

  it("a live attempt of the clone is a memory veto: pending/vetoed", async t => {
    const r = await published(t);
    const c = claimant(r);
    t.mock.method(c.p.liveAttempts, "isLivePath", () => true);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "vetoed" });
    await assertUntouched(r, files, journal);
  });

  it("a process that is not quiescent is pending/not_quiescent", async t => {
    const r = await published(t);
    const c = claimant(r);
    (c.p as unknown as { quiesceImpl: unknown }).quiesceImpl = async () => ({ process: { state: "not_quiescent" }, docker: { state: "docker_unconfirmed" } });
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "not_quiescent" });
    await assertUntouched(r, files, journal);
  });

  it("a HEAD that moved since the proof is invalid/identity_changed", async t => {
    const r = await published(t);
    execFileSync("git", ["-C", r.clone, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "advance"],
      { env: ENV, stdio: "pipe" });
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff(), { kind: "invalid", why: "identity_changed" });
    await assertUntouched(r, files, journal);
  });

  it("an I/O failure while reading the journal fence is uncertain, never invalid", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    t.mock.method(git, "readCompletionSourceFence", async () => { throw new Error("EIO: injected"); });
    assert.deepEqual(await c.handoff(), { kind: "uncertain", why: "verify_error" });
    await assertUntouched(r, files, journal);
  });

  it("a preparation I/O error (a bundle lstat) is uncertain/preparation_error, never a raw throw", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    failBundleLstat(t, r, c.p.recovery);
    assert.deepEqual(await c.handoff(), { kind: "uncertain", why: "preparation_error" });
    await assertUntouched(r, files, journal);
  });

  it("a lifecycle abort inside the preparation still propagates", async t => {
    const r = await published(t);
    const c = claimant(r);
    const controller = new AbortController();
    Object.assign(c.p.recovery, { testHooks: { afterHandoffSelect: async () => { controller.abort(new Error("cancelled")); throw new Error("EIO: after abort"); } } });
    await assert.rejects(c.handoff({ wait: { signal: controller.signal, timeoutMs: 5000 } }), /EIO: after abort/);
    const named = new Error("aborted");
    named.name = "AbortError";
    Object.assign(c.p.recovery, { testHooks: { afterHandoffSelect: async () => { throw named; } } });
    await assert.rejects(c.handoff(), err => err === named);
  });

  it("a spent maintenance budget inside the preparation propagates instead of reading as preparation_error", async t => {
    const r = await published(t);
    const c = claimant(r);
    // The budget's abort reason is a plain Error, not an AbortError, and the wait carries no signal: only the
    // budget check in the preparation catch can tell this throw from an ordinary I/O error.
    // The budget is live on entry and runs out (injected clock) while the preparation is failing.
    let now = 0;
    const budget = new MaintenanceBudget(10_000, () => now);
    Object.assign(c.p.recovery, { testHooks: { afterHandoffSelect: async () => { now += 20_000; throw new Error("EIO: during the spent budget"); } } });
    await assert.rejects(MaintenanceBudget.run(budget, () => c.handoff()), /maintenance deadline exhausted/);
  });
});

describe("retireCompletedSourceForHandoff: classify", linux, () => {
  it("a disabled-authority coordinator classifies as uncertain/classify_unavailable, not not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    Object.assign(c.p.recovery, { authority: undefined });
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "uncertain", why: "classify_unavailable" });
    assert.deepEqual(await c.handoff({ mode: "retire" }), { kind: "not_completion" }, "retire mode keeps the outward refusal");
    await assertUntouched(r, files, journal);
  });

  it("a valid published source is pending/episode_bearing and nothing is deleted", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    const slot = await slotVia(c.rt, r.clone);
    const retire = t.mock.method(git, "retireRunnerClone");
    const preflight = t.mock.method(git, "cleanupRecoveryGeneration");
    assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "pending", why: "episode_bearing" });
    assert.equal(retire.mock.callCount(), 0, "no boundary, no deletion");
    assert.equal(preflight.mock.callCount(), 0);
    await assertUntouched(r, files, journal);
    assert.deepEqual(await slotVia(c.rt, r.clone), slot, "the slot is untouched");
  });

  it("a broken certificate is invalid and an I/O failure is uncertain", async t => {
    const r = await published(t);
    const c = claimant(r);
    t.mock.method(git, "readCompletionSourceFence", async () => { throw new Error("EIO: injected"); });
    assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "uncertain", why: "verify_error" });
    const flip = (hex: string): string => (hex.startsWith("0") ? "1" : "0") + hex.slice(1);
    await rewriteSources(r, s => ({ ...s, certificate: flip(s.certificate as string) }));
    assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "invalid", why: "unverified" });
  });

  it("no record names the fence: a slot at the path is uncertain, no slot is not_completion", async t => {
    const r = await published(t);
    const c = claimant(r);
    const fence = { ...(fenceOf(r) as CompletionSourceFence), sourceUseEpoch: "99999999-9999-4999-8999-999999999999" };
    assert.deepEqual(await c.handoff({ mode: "classify", fence }), { kind: "uncertain", why: "slot_present" });
    assert.deepEqual(await c.handoff({ mode: "classify", fence, clonePath: `${r.clone}-nowhere` }), { kind: "not_completion" });
    assert.deepEqual(await c.handoff({ mode: "retire", fence }), { kind: "not_completion" }, "retire mode never consults the slot");
  });

  it("the fence matches but the path differs: invalid; no receipt: pending or invalid by the owner status", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    const c = claimant(r);
    assert.deepEqual(await c.handoff({ mode: "classify", clonePath: `${r.clone}-other` }), { kind: "invalid", why: "path_mismatch" });
    assert.deepEqual(await c.handoff({ mode: "classify", ownerCompleted: true }), { kind: "pending", why: "no_receipt" });
    assert.deepEqual(await c.handoff({ mode: "classify", ownerCompleted: false }), { kind: "invalid", why: "no_receipt" });
  });

  it("a busy capture cycle is pending/cycle_busy", async t => {
    const r = await published(t);
    const c = claimant(r);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let entered!: () => void;
    const inside = new Promise<void>(resolve => { entered = resolve; });
    t.mock.method(c.p.recovery as unknown as { pinCycle(): Promise<undefined> }, "pinCycle", async () => { entered(); await gate; return undefined; });
    const head = (await c.p.recovery.inspect(r.runId))[0]!.sourceSha;
    const holder = c.p.recovery.pin({ runId: r.runId, generation: GENERATION, kind: "issue", branch: BRANCH, sourceSha: head, inventoryGuarded: true });
    await inside;
    try {
      assert.deepEqual(await c.handoff({ mode: "classify" }), { kind: "pending", why: "cycle_busy" });
    } finally {
      release();
      await holder;
    }
  });
});

describe("retireCompletedSourceForHandoff: seams", linux, () => {
  it("afterHandoffSelect then beforeHandoffCycle run after the selection and before the cycle; a sweep between them is observed", async t => {
    const r = await published(t);
    const c = claimant(r);
    const events: string[] = [];
    Object.assign(c.p.recovery, { testHooks: {
      afterHandoffSelect: async () => { events.push("select"); },
      beforeHandoffCycle: async () => { events.push("cycle"); },
    } });
    const real = (git.retireRunnerClone as (...a: unknown[]) => Promise<unknown>).bind(git);
    t.mock.method(git, "retireRunnerClone", (async (...args: unknown[]) => { events.push("retire"); return real(...args); }) as never);
    assert.deepEqual(await c.handoff(), { kind: "retired" });
    assert.deepEqual(events, ["select", "cycle", "retire"]);
  });

  it("a sweep that retires the generation between selection and the cycle leaves nothing for the entry to retire", async t => {
    const r = await published(t);
    const c = claimant(r);
    Object.assign(c.p.recovery, { testHooks: {
      beforeHandoffCycle: async () => {
        const snapshot = (await c.p.recovery.inspect(r.runId)).filter(x => x.completionReceipt);
        await c.p.recovery.resumePending(undefined, snapshot);
      },
    } });
    const result = await c.handoff();
    // The sweep (nested in the entry's A) retired the generation; the entry's own cycle then finds no authority record.
    assert.equal(fs.existsSync(r.clone), false);
    assert.deepEqual(result, { kind: "invalid", why: "no_authority_record" });
    assert.equal(journalRaw(r), "");
  });

  it("an AuthorityLockBusy raised once A is held propagates: it is not the acquisition's authority_busy", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    Object.assign(c.p.recovery, { testHooks: { afterHandoffSelect: async () => { throw new AuthorityLockBusy(); } } });
    await assert.rejects(c.handoff(), err => err instanceof AuthorityLockBusy);
    await assertUntouched(r, files, journal);
  });
});

describe("retireCompletedSourceForHandoff: refusal and guard classification", linux, () => {
  const refusals: Array<[string, () => Error, HandoffRetirement]> = [
    ["a source that is not the published source", () => new AuthorityRefusal("completion source is not the published source", "source_mismatch"),
      { kind: "invalid", why: "unverified" }],
    ["a malformed source", () => new AuthorityRefusal("malformed completion source", "source_mismatch"), { kind: "invalid", why: "unverified" }],
    ["no authority slot", () => new AuthorityRefusal("no authority slot covers the source", "no_slot"), { kind: "invalid", why: "unverified" }],
    ["an unresolved operation on the retiring source", () => new AuthorityRefusal("an operation on the retiring source is unresolved", "operation_unresolved"),
      { kind: "pending", why: "unresolved_operation" }],
    ["an untyped failure", () => new Error("EIO: injected"), { kind: "uncertain", why: "verify_error" }],
  ];
  for (const mode of ["retire", "classify"] as const) {
    for (const [name, make, expected] of refusals) {
      it(`${mode}: ${name} is ${expected.kind} and nothing changes`, async t => {
        const r = await published(t);
        const c = claimant(r);
        const files = snapshotFiles(r);
        const journal = journalRaw(r);
        const slot = await slotVia(c.rt, r.clone);
        (c.rt.retirement as { inspect: unknown }).inspect = async () => { throw make(); };
        assert.deepEqual(await c.handoff({ mode }), expected);
        await assertUntouched(r, files, journal);
        assert.deepEqual(await slotVia(c.rt, r.clone), slot, "the slot is untouched");
      });
    }
  }

  it("the typed kind is carried by the real parser for a malformed source", () => {
    assert.throws(() => parseCompletionSourceV2({}, { dataDir: "/x", storageId: "s", layout: "l" } as never),
      err => err instanceof AuthorityRefusal && err.kind === "source_mismatch");
  });

  it("a preflight that does not verify is pending/preflight and nothing changes", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    const real = (git.cleanupRecoveryGeneration as (...a: unknown[]) => Promise<unknown>).bind(git);
    t.mock.method(git, "cleanupRecoveryGeneration", (async (...args: unknown[]) => args[7] === "completion-check" ? "retained" : real(...args)) as never);
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "preflight" });
    await assertUntouched(r, files, journal);
  });

  it("the worker residue quarantine is pending/quarantine and nothing changes", async t => {
    const r = await published(t);
    const c = claimant(r);
    const files = snapshotFiles(r);
    const journal = journalRaw(r);
    t.after(() => resetResidueQuarantineForTests());
    latchResidueQuarantine({ cause: "handoff fixture", site: "terminal_drive", runId: r.runId }, nullLogger());
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "quarantine" });
    await assertUntouched(r, files, journal);
  });

  for (const mode of ["retire", "classify"] as const) {
    it(`${mode}: a coordinator that cannot read the journal fence is unsupported/no_retirement_scope`, async t => {
      const r = await published(t);
      const c = claimant(r);
      const files = snapshotFiles(r);
      const journal = journalRaw(r);
      const original = git.readCompletionSourceFence;
      t.after(() => { Object.assign(git, { readCompletionSourceFence: original }); });
      Object.assign(git, { readCompletionSourceFence: undefined });
      assert.deepEqual(await c.handoff({ mode }), { kind: "unsupported", why: "no_retirement_scope" });
      await assertUntouched(r, files, journal);
    });
  }

  it("a throw inside the boundary action is pending/action_failed, not uncertain/boundary_error", async t => {
    const r = await published(t);
    const c = claimant(r);
    // The binder check runs inside the action after the records are gone; its throw is the action's own.
    Object.assign(c.p.recovery, { completionBinderPending: () => true });
    assert.deepEqual(await c.handoff(), { kind: "pending", why: "action_failed" });
    assert.equal((await slotVia(c.rt, r.clone))?.state, "retiring", "the retirement stays resumable");
  });
});


// ---------------------------------------------------------------------------------------------------------------
// issue #3645 M3 - the claimant half: a REAL RunRunner admits a successor on a just-completed owner's branch. Real
// authority store, real recovery coordinator, real Git, the owner-classification fake. See orphan-handoff-scene.ts.

const subreasons = (s: Scene): unknown[] => diagnostics(s).map(d => d["retirement_subreason"]);
const diagnostic = (s: Scene, event: string, reason: string): Record<string, unknown> | undefined =>
  diagnostics(s).find(d => d["event"] === event && d["reason"] === reason);

describe("a successor claim on a completed owner: held, then retired by the certified path", linux, () => {
  for (const row of [...ROWS, ...ATTEMPT_ROWS]) {
    it(`${row.name}: a busy cycle stops the claimant retained; the re-claim retires the owner and seeds`, async t => {
      const s = await scene(t, row);
      assert.ok(fenceOf(s.r), "the owner's journal still carries its completion fence");
      const before = durable(s);
      const hold = await holdCycle(t, s, row.owner.kind);
      await s.runner.execute(s.claim);

      // The retained stop: no terminal report, no model, the owner's source byte-identical.
      assert.deepEqual(reported(s).filter(x => ["failed", "completed", "cancelled"].includes(x)), [], "no terminal report");
      assert.equal(failure(s), undefined);
      await assertUnchanged(s, before, assert);
      const pending = diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending");
      assert.equal(pending?.["stage"], "completion_handoff");
      assert.equal(pending?.["retirement_subreason"], "completion_pending");
      assert.equal(s.registry.has(s.claim.run_id), false, "the stopped claimant left the active-run snapshot");
      assert.equal(s.settle.mock.callCount(), 0, "the claim's custody hold was not settled");

      // Released: the next claim of the run (a new claim generation) retires the owner by the certified chain.
      await hold.release();
      const second = { ...s.claim, claim_generation: GENERATION + 2 };
      api.setOwnershipStatus(second.run_id, "running", second.claim_generation);
      await s.runner.execute(second);
      assert.equal(String(failure(s)), STOPPED, "the claimant was admitted and reached its executor");
      const retiredAt = s.handoffs.at(-1)!;
      assert.deepEqual(retiredAt.outcome, { kind: "retired" });
      assert.equal(retiredAt.slot?.state, "retired", "the owner's slot was retired before the claimant was admitted");
      assert.equal(retiredAt.fence, undefined, "the owner's fence was cleared before the claimant was admitted");
      assert.equal(retiredAt.cloneExists, false, "the owner's clone was retired");
      assert.equal(fenceOf(s.r), undefined, "the owner's fence is gone");
      assert.deepEqual(recordNames(s.r), [], "the owner's records and bundles are gone");
      assert.ok(s.started, "the claimant's executor started");
      assert.equal(s.started.slot?.state, "active", "the claimant holds an active epoch on its path");
      assert.notEqual(s.started.slot?.epoch, s.ownerSlot?.epoch, "a fresh epoch, not the owner's");
      assert.equal(fs.existsSync(path.join(s.started.worktree, OWNER_FILE)), false, "no owner-only file in the claimant's clone");
      const retired = diagnostic(s, "orphan_reclaim_succeeded", "completion-retired");
      assert.equal(retired?.["stage"], "complete");
      if (row.attemptPaths) {
        assert.match(path.basename(s.started.worktree), /\.attempt-/, "the claimant selected its own attempt path");
        assert.notEqual(s.started.worktree, s.r.clone);
        assert.match(path.basename(s.r.clone), /\.attempt-/, "the owner was at an attempt path");
      } else if (row.samePath) {
        assert.equal(path.basename(s.r.clone), "agent-issue-3404", "the owner is at the canonical path");
        assert.equal(s.started.worktree, s.r.clone, "the claimant seeds at the owner's former path");
        assert.equal(s.foreign[0]?.name, "ForeignCaptureBlockedError");
      } else {
        assert.notEqual(s.started.worktree, s.r.clone);
        assert.equal(fs.existsSync(s.r.clone), false, "the owner's clone was retired");
        if (row.ownerCanonical) {
          assert.equal(path.basename(s.r.clone), "issue-3404", "the owner was at its canonical path");
          assert.equal(path.basename(s.started.worktree), "agent-issue-3404", "the claimant reseeds at its own canonical path");
        }
      }
    });
  }
});

// ---- races: the owner's journal moves under the claimant -------------------------------------------------------------

const sweep = async (s: Scene): Promise<void> => {
  const snapshot = (await s.recovery.inspect(s.r.runId)).filter(x => x.completionReceipt);
  await s.recovery.resumePending(undefined, snapshot);
};
const caseA = ROWS[0]!;
const caseB = ROWS[1]!;

describe("a successor claim on a completed owner: races", linux, () => {
  for (const row of [caseA, caseB]) {
    it(`${row.name}: a sweep that retires the owner after the fence read re-admits the claimant`, async t => {
      const s = await scene(t, row);
      const real = git.readOrphanJournalFence.bind(git);
      let reads = 0;
      t.mock.method(git, "readOrphanJournalFence", (async (...args: Parameters<typeof real>) => {
        const out = await real(...args);
        if (++reads === 1) { assert.equal(out.state, "fenced"); await sweep(s); }
        return out;
      }) as typeof real);
      await s.runner.execute(s.claim);
      assert.equal(String(failure(s)), STOPPED, "re-admitted, then seeded");
      assert.equal(s.handoffs.length, 1);
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" }, "the sweep left nothing to retire");
      assert.equal(fenceOf(s.r), undefined);
      assert.equal(subreasons(s).includes("pair_changed"), true);
      assert.equal(subreasons(s).includes("fenced_source"), false, "not a terminal refusal");
      assert.equal(s.started?.slot?.state, "active");
    });

    it(`${row.name}: a sweep inside the selection window re-admits instead of refusing`, async t => {
      const s = await scene(t, row);
      Object.assign(s.recovery, { testHooks: { afterHandoffSelect: async () => { await sweep(s); } } });
      await s.runner.execute(s.claim);
      assert.equal(String(failure(s)), STOPPED);
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "invalid", why: "no_authority_record" }, "a just-retired owner reads as no authority record");
      assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_refused")?.["retirement_subreason"], "pair_changed");
      assert.equal(subreasons(s).includes("completion_unverified"), false, "the invalid disposition never fired");
      assert.equal(fenceOf(s.r), undefined);
    });

    it(`${row.name}: a retirement whose final cleanup is not removed re-admits once the journal moved`, async t => {
      const s = await scene(t, row);
      const real = (git.cleanupRecoveryGeneration as (...a: unknown[]) => Promise<unknown>).bind(git);
      const cleanup = t.mock.method(git, "cleanupRecoveryGeneration", (async (...args: unknown[]) => args[7] === "completion" ? "retained" : real(...args)) as never);
      await s.runner.execute(s.claim);
      cleanup.mock.restore();
      assert.equal(s.handoffs[0]!.outcome?.kind, "pending");
      assert.equal(subreasons(s).includes("pair_changed"), true, "the journal and clone had moved: the claimant was re-admitted");
      if (row.samePath) {
        // The re-admitted seed lands on the path whose slot is still `retiring`: a not-ready retirement, so a retained
        // stop (no terminal report), not a failure. A sweep that finishes the retirement lets the re-claim through.
        assert.equal(failure(s), undefined);
        assert.deepEqual(reported(s).filter(x => TERMINAL.includes(x)), [], "no terminal report");
        const stop = diagnostics(s).filter(d => d["reason"] === "completion_retirement_pending");
        assert.equal(stop.at(-1)?.["retirement_subreason"], "completion_pending");
        assert.equal(s.registry.has(s.claim.run_id), false);
        await sweep(s);
        assert.equal((await slotVia(s.rt, s.r.clone))?.state, "retired");
        await s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 });
      }
      assert.equal(String(failure(s)), STOPPED);
    });
  }

  it("a fresh claim (no handoff re-admission) that meets a retiring slot is a retained stop; after the finishing sweep it seeds", async t => {
    const s = await scene(t, caseB);
    const real = (git.cleanupRecoveryGeneration as (...a: unknown[]) => Promise<unknown>).bind(git);
    const cleanup = t.mock.method(git, "cleanupRecoveryGeneration", (async (...args: unknown[]) => args[7] === "completion" ? "retained" : real(...args)) as never);
    // The retirement is interrupted outside any claimant: the journal and clone moved, the slot stays `retiring`.
    const fence = fenceOf(s.r) as CompletionSourceFence;
    const attemptId = sourceOf(readRecords(s.r)[0]!).attemptId;
    const direct = await s.recovery.retireCompletedSourceForHandoff({
      runId: s.r.runId, clonePath: s.r.clone, attemptId, fence, ownerCompleted: true, wait: { timeoutMs: 5000 },
    });
    cleanup.mock.restore();
    assert.equal(direct.kind, "pending");
    // Before the finishing sweep: the seed meets the retiring slot of a partially complete retirement.
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs.length, 1, "only the interrupted retirement above: the claimant never entered a handoff");
    assert.equal(failure(s), undefined, "not a terminal failure");
    assert.deepEqual(reported(s).filter(x => TERMINAL.includes(x)), [], "no terminal report");
    assert.equal(s.started, undefined, "the model never starts");
    const stop = diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending");
    assert.equal(stop?.["stage"], "completion_handoff");
    assert.equal(stop?.["retirement_subreason"], "completion_pending");
    assert.equal(stop?.["owner_id"], "invalid", "a fresh claim knows no owner: the fixed placeholder");
    assert.equal(JSON.stringify(s.lines).includes(s.r.clone), false, "no path leaves in the log");
    assert.equal(s.registry.has(s.claim.run_id), false);
    assert.equal(s.settle.mock.callCount(), 0, "the claim hold is not settled");
    // After the sweep finished the retirement, the re-claim seeds.
    await sweep(s);
    assert.equal((await slotVia(s.rt, s.r.clone))?.state, "retired");
    await s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 });
    assert.equal(String(failure(s)), STOPPED);
  });


  it("a replacement journal for another run installed in the window is left byte-identical and validated afresh", async t => {
    const s = await scene(t, caseB);
    const other = randomUUID();
    let planted = "";
    Object.assign(s.recovery, { testHooks: { afterHandoffSelect: async () => {
      await sweep(s);
      fs.mkdirSync(s.r.clone, { recursive: true });
      fs.writeFileSync(path.join(s.r.clone, "OTHER.txt"), "other run bytes\n");
      await git.markRecoveryCapture(s.r.bare, s.r.clone, BRANCH, other);
      planted = journalRaw(s.r);
    } } });
    api.setOrphanClassification(other, { status: "running", repo_id: "r1", kind: "mr_rework", issue_iid: 3404, branch: null, pipeline_ref: BRANCH, pipeline_id: null });
    await s.runner.execute(s.claim);
    assert.equal(s.started, undefined, "the model never starts");
    assert.match(String(failure(s)), /refusing to replace a retained clone owned by another run/, "the replacement was validated from scratch");
    assert.equal(journalRaw(s.r), planted, "the replacement journal is byte-identical");
    assert.ok(planted.includes(other));
    assert.equal(fs.readFileSync(path.join(s.r.clone, "OTHER.txt"), "utf8"), "other run bytes\n");
    assert.equal(diagnostic(s, "orphan_reclaim_refused", "nonterminal_owner") !== undefined, true);
  });

  it("neither the journal lock J nor the bare lock B is held when the claimant enters the coordinator", async t => {
    const s = await scene(t, caseB);
    const probe = async (what: string, hold: () => Promise<unknown>): Promise<void> => {
      const timer = new Promise<string>(resolve => setTimeout(() => resolve("held"), 3000));
      assert.equal(await Promise.race([hold().then(() => "free"), timer]), "free", `${what} is free on entry`);
    };
    s.beforeHandoff = async () => {
      await probe("B", () => (git as unknown as { withLock<T>(bare: string, fn: () => Promise<T>): Promise<T> }).withLock(s.r.bare, async () => true));
      await probe("J", () => (s.recovery as unknown as { withJournalLock<T>(run: string, fn: () => Promise<T>): Promise<T> }).withJournalLock(s.r.runId, async () => true));
    };
    await s.runner.execute(s.claim);
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(s.handoffs.length, 1);
  });

  it("a claimant admitted next to the coordinator's own passes settles within a bounded time, with one lawful end", async t => {
    const s = await scene(t, caseA);
    const runner = s.runner as unknown as { isExecuting(id: string): boolean };
    const passes = (async () => {
      await sweep(s);
      await s.recovery.resumeLive({ isExecuting: id => runner.isExecuting(id), authenticatedAtMs: Date.now() });
    })();
    const bounded = <T>(work: Promise<T>): Promise<T> => Promise.race([work, new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error("the handoff and the coordinator did not settle")), 60_000))]);
    await bounded(Promise.all([s.runner.execute(s.claim), passes]));
    assert.equal(fenceOf(s.r), undefined, "the owner's fence is gone whichever side retired it");
    assert.deepEqual(recordNames(s.r), []);
    if (failure(s) === undefined) {
      // The claimant lost the cycle to the coordinator: a retained stop, then the re-claim seeds.
      assert.deepEqual(reported(s).filter(x => ["failed", "completed", "cancelled"].includes(x)), []);
      await bounded(s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 }));
    }
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(fs.existsSync(path.join(s.started!.worktree, OWNER_FILE)), false);
  });

  it("a third re-admission is a retained stop, not a loop", async t => {
    const s = await scene(t, caseB);
    const before = durable(s);
    let reads = 0;
    t.mock.method(git, "readOrphanJournalFence", async () => {
      reads++;
      return { state: "changed" as const };
    });
    await s.runner.execute(s.claim);
    assert.equal(reads, 3, "two re-admissions, then the stop");
    assert.deepEqual(reported(s).filter(x => ["failed", "completed", "cancelled"].includes(x)), [], "no terminal report");
    assert.equal(s.handoffs.length, 0, "the owner was never touched");
    await assertUnchanged(s, before, assert);
    const stop = diagnostics(s).filter(d => d["reason"] === "completion_retirement_pending");
    assert.equal(stop.length, 1);
    assert.equal(stop[0]!["retirement_subreason"], "pair_changed");
    assert.equal(s.registry.has(s.claim.run_id), false);
    assert.equal(s.settle.mock.callCount(), 0);
  });
});

// ---- authority contention: the claimant's wait for A ---------------------------------------------------------------------

/** Take A in another transaction (the way a coordinator cycle holds it) until `release()`. */
function holdAuthority(s: Scene): { held: Promise<void>; release(): Promise<void> } {
  let open!: () => void;
  const gate = new Promise<void>(resolve => { open = resolve; });
  let entered!: () => void;
  const held = new Promise<void>(resolve => { entered = resolve; });
  const holder = s.rt.withTransaction(async () => { entered(); await gate; });
  return { held, release: async () => { open(); await holder; } };
}

describe("a successor claim on a completed owner: contention for the authority transaction A", linux, () => {
  /** A is taken only once the claimant is about to enter the coordinator, so its seed and other waits are unaffected. */
  function contend(s: Scene, shorten: boolean, during?: () => void): void {
    let holder: ReturnType<typeof holdAuthority> | undefined;
    s.beforeHandoff = async input => {
      holder = holdAuthority(s);
      await holder.held;
      during?.();
      // The production wait (300 s) is not shortened anywhere; the test hands the entry a short one.
      return shorten ? { ...input, wait: { ...input.wait, timeoutMs: 150 } } : input;
    };
    s.afterHandoff = async () => { await holder?.release(); };
  }

  it("A held past the wait: a retained stop with authority_busy, then the re-claim retires and seeds", async t => {
    const s = await scene(t, caseA);
    const before = durable(s);
    contend(s, true);
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "authority_busy" });
    assert.deepEqual(reported(s).filter(x => ["failed", "completed", "cancelled"].includes(x)), [], "no terminal report");
    await assertUnchanged(s, before, assert);
    const stop = diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending");
    assert.equal(stop?.["stage"], "completion_handoff");
    assert.equal(stop?.["retirement_subreason"], "authority_busy");
    assert.equal(s.registry.has(s.claim.run_id), false);
    s.beforeHandoff = undefined;
    s.afterHandoff = undefined;
    await s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 });
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(fenceOf(s.r), undefined);
  });

  it("a cancel during the wait for A is the cancelled stop: a `run cancelled` report with custody kept", async t => {
    const s = await scene(t, caseA);
    const before = durable(s);
    contend(s, false, () => {
      const flight = s.flights.at(-1)!;
      setTimeout(() => { flight.cancel.abort(); flight.steering.markCancelled(); }, 50);
    });
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs[0]!.outcome, undefined, "the wait ended by the abort, not by an answer");
    assert.equal(String(failure(s)), "run cancelled");
    assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending"), undefined, "a cancel is not a pending retirement");
    await assertUnchanged(s, before, assert);
  });

  it("a stale claim fence during the wait is the quiet stop: no terminal report", async t => {
    const s = await scene(t, caseA);
    const before = durable(s);
    contend(s, true, () => { s.flights.at(-1)!.steering.endFencedFlight("stale"); });
    await s.runner.execute(s.claim);
    assert.deepEqual(reported(s).filter(x => ["failed", "completed", "cancelled"].includes(x)), [], "a fenced claim reports nothing terminal");
    await assertUnchanged(s, before, assert);
  });

  it("an AuthorityRefusal that is not the acquisition's busy signal propagates: a terminal failure, never a retained stop", async t => {
    const s = await scene(t, caseA);
    const before = durable(s);
    Object.assign(s.recovery, { testHooks: { afterHandoffSelect: async () => { throw new AuthorityRefusal("authority lock inode changed"); } } });
    await s.runner.execute(s.claim);
    assert.notEqual(failure(s), undefined, "reported as a failure");
    assert.notEqual(String(failure(s)), STOPPED);
    assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending"), undefined);
    assert.equal(s.started, undefined);
    assert.deepEqual(durable(s), before);
  });
});

// ---- the quiescence exclusion of the handoff claimant ------------------------------------------------------------------------

type QuiesceRequest = { otherClaimInFlight: boolean; site: string; [k: string]: unknown };
const quiescent = { process: { state: "quiescent", processes: [], killed: [] }, docker: { state: "docker_unconfirmed", removed: [] } };
interface Internals {
  executionTails: Map<string, Promise<void>>;
  handoffClaimants: Map<string, unknown>;
  quiesceImpl: unknown;
  shuttingDownGlobal: boolean;
  buildFlight(...args: unknown[]): Record<string, unknown>;
  registerHandoffClaimant(owner: string, flight: Record<string, unknown>): (() => void) | undefined;
}

describe("completion boundary: the handoff claimant is the only tail left out of otherClaimInFlight", linux, () => {
  const claimantId = randomUUID();

  /** A restarted incarnation with a recording quiesceImpl and a registered, pre-seed claimant flight. */
  async function boundary(t: TestContext, options: { register?: boolean; tail?: boolean } = {}) {
    const r = await published(t, { cohort: 2 });
    const second = r.restart();
    const p = second.p as unknown as Internals;
    const requests: QuiesceRequest[] = [];
    let onQuiesce: ((req: QuiesceRequest) => void) | undefined;
    p.quiesceImpl = async (req: QuiesceRequest) => { requests.push(req); onQuiesce?.(req); return quiescent; };
    const claim = gitlabClaim(3404, { run_id: claimantId, claim_generation: GENERATION + 1, inventory_guarded: true });
    const flight = p.buildFlight(claim, claimantId, r.executor, undefined, "", []) as Record<string, any> & {
      steering: { endFencedFlight(r: string): void; markCancelled(): void } };
    const tail = new Promise<void>(() => {});
    p.executionTails.set(claimantId, tail);
    if (options.tail !== false) flight.executionTail = tail;
    const deregister = options.register === false ? undefined : p.registerHandoffClaimant(r.runId, flight);
    t.after(() => { deregister?.(); p.executionTails.delete(claimantId); });
    const attemptId = sourceOf(readRecords(r)[0]!).attemptId;
    const fence = fenceOf(r) as CompletionSourceFence;
    return {
      r, p, flight, requests, tail, deregister, onQuiesce: (fn: (req: QuiesceRequest) => void) => { onQuiesce = fn; },
      handoff: () => second.p.recovery.retireCompletedSourceForHandoff({
        runId: r.runId, clonePath: r.clone, attemptId, fence, ownerCompleted: true, wait: { timeoutMs: 5000 },
      }),
    };
  }
  const flags = (requests: QuiesceRequest[]): boolean[] => requests.map(q => q.otherClaimInFlight);

  it("the unseeded, registered claimant alone: false at every request", async t => {
    const b = await boundary(t);
    assert.deepEqual(await b.handoff(), { kind: "retired" });
    assert.ok(b.requests.length >= 2);
    assert.deepEqual(flags(b.requests), b.requests.map(() => false));
    assert.ok(b.requests.every(q => ["completion_source", "completion_source:after_git", "completion_source:skills"].includes(q.site)));
  });

  const counted: Array<[string, (b: Awaited<ReturnType<typeof boundary>>) => void]> = [
    ["a seeded claimant (worktreePath set)", b => { b.flight["worktreePath"] = "/seeded"; }],
    ["a claimant with a clone bound", b => { b.flight["runnerClone"] = { path: "/seeded" }; }],
    ["a claimant with a registered live attempt", b => { b.flight["attempt"] = { marker: "m", clonePath: "/x" }; }],
    ["another active execution tail", b => { b.p.executionTails.set(randomUUID(), new Promise<void>(() => {})); }],
    ["a claimant whose tail was replaced by a queued duplicate", b => { b.p.executionTails.set(claimantId, new Promise<void>(() => {})); }],
    ["a claimant fenced by a stale claim", b => { (b.flight["steering"] as { endFencedFlight(r: string): void }).endFencedFlight("stale"); }],
    ["a cancelled claimant", b => { (b.flight["steering"] as { markCancelled(): void }).markCancelled(); }],
    ["a claimant that is shutting down", b => { b.flight["active"] = { shuttingDown: true }; }],
    ["a worker that is shutting down", b => { b.p.shuttingDownGlobal = true; }],
  ];
  for (const [name, change] of counted) {
    it(`${name} counts as another claim`, async t => {
      const b = await boundary(t);
      change(b);
      await b.handoff();
      assert.ok(b.requests.length >= 1);
      assert.equal(flags(b.requests)[0], true);
    });
  }

  it("an unregistered claimant tail counts as another claim, and the request is the baseline's", async t => {
    const b = await boundary(t, { register: false });
    assert.deepEqual(await b.handoff(), { kind: "retired" });
    const first = b.requests[0]!;
    assert.deepEqual(Object.keys(first).sort(), ["attempt", "cloneKey", "dockerHost", "mode", "otherClaimInFlight", "processes", "registry", "site", "targetPaths"]);
    assert.equal(first.otherClaimInFlight, true);
    assert.equal(first.mode, "capture");
    assert.equal(first.processes, true);
    assert.equal(first.attempt, undefined);
    assert.deepEqual(first.targetPaths, [b.r.clone]);
    assert.equal(first.site, "completion_source");
  });

  it("a claimant whose flight recorded no tail is never excluded", async t => {
    const b = await boundary(t, { tail: false });
    await b.handoff();
    assert.equal(flags(b.requests)[0], true, "the claimant's own tail is then an unregistered tail");
  });

  it("the exclusion flipping during the awaited quiescence retains the source", async t => {
    const b = await boundary(t);
    const files = snapshotFiles(b.r);
    const journal = journalRaw(b.r);
    b.onQuiesce(() => { b.flight["worktreePath"] = "/seeded"; });
    assert.deepEqual(await b.handoff(), { kind: "pending", why: "not_quiescent" });
    assert.equal(flags(b.requests)[0], false);
    await assertUntouched(b.r, files, journal);
  });

  it("the exclusion flipping during the after_git quiescence retains the source", async t => {
    const b = await boundary(t);
    const files = snapshotFiles(b.r);
    const journal = journalRaw(b.r);
    b.onQuiesce(req => { if (req.site === "completion_source:after_git") b.flight["worktreePath"] = "/seeded"; });
    assert.deepEqual(await b.handoff(), { kind: "pending", why: "not_quiescent" });
    assert.deepEqual(b.requests.map(q => q.site), ["completion_source", "completion_source:after_git"]);
    await assertUntouched(b.r, files, journal);
  });

  it("the exclusion flipping during the skills quiescence leaves the skills dir", async t => {
    const b = await boundary(t);
    b.onQuiesce(req => { if (req.site === "completion_source:skills") b.flight["worktreePath"] = "/seeded"; });
    await b.handoff();
    assert.ok(b.requests.some(q => q.site === "completion_source:skills"), "the skills quiescence ran");
    assert.ok(fs.existsSync(b.r.skills), "the skills dir is not removed");
  });

  it("a live owner execution tail still refuses, with the claimant registered", async t => {
    const b = await boundary(t);
    const files = snapshotFiles(b.r);
    const journal = journalRaw(b.r);
    b.p.executionTails.set(b.r.runId, new Promise<void>(() => {}));
    assert.deepEqual(await b.handoff(), { kind: "pending", why: "executing" });
    await assertUntouched(b.r, files, journal);
    b.p.executionTails.delete(b.r.runId);
  });

  it("the registration is per owner and removed by the returned function", async t => {
    const b = await boundary(t);
    assert.equal(b.p.handoffClaimants.has(b.r.runId), true);
    assert.equal(b.p.registerHandoffClaimant(b.r.runId, b.flight), undefined, "a second claimant for the same owner is not registered");
    b.deregister!();
    assert.equal(b.p.handoffClaimants.has(b.r.runId), false, "the returned function removes the entry");
    const newer = b.p.registerHandoffClaimant(b.r.runId, b.flight);
    assert.equal(typeof newer, "function");
    b.deregister!();
    assert.equal(b.p.handoffClaimants.has(b.r.runId), true, "a stale deregister never deletes the newer entry");
    newer!();
    assert.equal(b.p.handoffClaimants.has(b.r.runId), false);
  });
});

// ---- the fail-closed matrix, through the runner ------------------------------------------------------------------------------

const TERMINAL = ["failed", "completed", "cancelled"];
const flip = (hex: string): string => (hex.startsWith("0") ? "1" : "0") + hex.slice(1);
const OUTWARD = /refusing to replace a retained clone owned by another run|recovery journal points at a different clone path/;

/** Nonterminal stop: no terminal report, no model, nothing of the owner's moved, the diagnostic names `subreason`. */
async function expectStop(s: Scene, before: ReturnType<typeof durable>, subreason: string): Promise<void> {
  assert.deepEqual(reported(s).filter(x => TERMINAL.includes(x)), [], "no terminal report");
  await assertUnchanged(s, before, assert);
  const stop = diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending");
  assert.equal(stop?.["stage"], "completion_handoff");
  assert.equal(stop?.["retirement_subreason"], subreason);
  assert.equal(s.registry.has(s.claim.run_id), false, "absent from the active-run snapshot");
  assert.equal(s.settle.mock.callCount(), 0, "the claim hold is not settled");
}

/** Terminal refusal with the ORIGINAL error the base raised: same class, same message, nothing moved. */
async function expectRefusal(s: Scene, before: ReturnType<typeof durable>, subreason: string): Promise<void> {
  await assertUnchanged(s, before, assert);
  assert.match(String(failure(s)), OUTWARD);
  assert.ok(s.foreign.length > 0 || String(failure(s)).includes("different clone path"));
  assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_refused")?.["retirement_subreason"], subreason);
  assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending"), undefined);
}

describe("a successor claim on a completed owner: fail closed", linux, () => {
  for (const row of [caseA, caseB]) {
    const outwardName = row.samePath ? "ForeignCaptureBlockedError" : "CapturePathMismatchError";
    describe(row.name, () => {
      it("a broken certificate is invalid: the outward error is unchanged and nothing moved", async t => {
        const s = await scene(t, row);
        await rewriteSources(s.r, src => ({ ...src, certificate: flip(src.certificate as string) }));
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "invalid", why: "unverified" });
        await expectRefusal(s, before, "completion_unverified");
        assert.equal(String(failure(s)), s.foreign[0]!.message, "the base's own error reaches the report");
        assert.equal(s.foreign[0]!.name, outwardName);
      });

      it("a failed owner with no receipt is invalid", async t => {
        const s = await scene(t, row, { receipt: false, ownerStatus: "failed" });
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "invalid", why: "no_receipt" });
        await expectRefusal(s, before, "completion_unverified");
      });

      it("a coordinator without a completion boundary is unsupported", async t => {
        const s = await scene(t, row);
        Object.assign(s.recovery, { withCompletionSourceBoundary: undefined });
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "unsupported", why: "no_boundary" });
        await expectRefusal(s, before, "completion_unsupported");
      });

      it("a coordinator with no authority scope answers not_completion: the fenced source is refused as before", async t => {
        const s = await scene(t, row);
        Object.assign(s.recovery, { authority: undefined });
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" });
        await expectRefusal(s, before, "fenced_source");
      });

      it("a completed owner with no receipt yet is pending: a retained stop, then the receipt lets the re-claim through", async t => {
        const s = await scene(t, row, { receipt: false });
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "no_receipt" });
        await expectStop(s, before, "completion_pending");
        await s.r.receipt();
        await s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 });
        assert.equal(String(failure(s)), STOPPED);
        assert.equal(fenceOf(s.r), undefined);
      });

      it("a live owner execution tail is pending: a retained stop", async t => {
        const s = await scene(t, row);
        const before = durable(s);
        const tails = (s.runner as unknown as { executionTails: Map<string, Promise<void>> }).executionTails;
        tails.set(s.r.runId, new Promise(() => {}));
        await s.runner.execute(s.claim);
        tails.delete(s.r.runId);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "executing" });
        await expectStop(s, before, "completion_pending");
      });

      it("a preflight that does not verify is pending/preflight: a retained stop", async t => {
        const s = await scene(t, row);
        const before = durable(s);
        const real = (git.cleanupRecoveryGeneration as (...a: unknown[]) => Promise<unknown>).bind(git);
        t.mock.method(git, "cleanupRecoveryGeneration", (async (...args: unknown[]) => args[7] === "completion-check" ? "retained" : real(...args)) as never);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "preflight" });
        await expectStop(s, before, "completion_preflight_retained");
      });

      it("an unreadable record of the owner is uncertain: a retained stop", async t => {
        const s = await scene(t, row);
        fs.writeFileSync(path.join(recordDir(s.r), "00000000-0000-4000-8000-0000000000ee.json"), "{\"not\":\"authenticated\"}");
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "uncertain", why: "unreadable_record" });
        await expectStop(s, before, "completion_uncertain");
      });

      it("an I/O failure while verifying is uncertain: a retained stop", async t => {
        const s = await scene(t, row);
        const before = durable(s);
        t.mock.method(git, "readCompletionSourceFence", async () => { throw new Error("EIO: injected"); });
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "uncertain", why: "verify_error" });
        await expectStop(s, before, "completion_uncertain");
      });

      it("an interrupted retirement stays resumable and the next claim is admitted", async t => {
        const s = await scene(t, row);
        Object.assign(s.recovery, { completionBinderPending: () => true });
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "action_failed" });
        assert.equal(s.handoffs[0]!.slot?.state, "retiring", "resumable");
        Object.assign(s.recovery, { completionBinderPending: () => false });
        await sweep(s);
        assert.deepEqual(recordNames(s.r), []);
        await s.runner.execute({ ...s.claim, claim_generation: GENERATION + 2 });
        assert.equal(String(failure(s)), STOPPED);
      });
    });
  }

  it("a worker without authority takes the baseline path: no coordinator entry, the claimant is seeded", async t => {
    const s = await scene(t, caseA);
    Object.assign(git, { authorityRequired: false });
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs.length, 0, "the unfenced baseline never reaches the coordinator");
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(fs.existsSync(path.join(s.started!.worktree, OWNER_FILE)), false);
    assert.equal(diagnostic(s, "orphan_reclaim_succeeded", "quarantined") !== undefined || diagnostics(s).some(d => d["event"] === "orphan_reclaim_succeeded"), true);
  });

  it("a Codex owner with a proven seal is retired like a Claude one", async t => {
    const s = await scene(t, { ...caseA, name: "codex", codex: true });
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs.map(h => [h.outcome, h.slot?.state, h.fence]), [[{ kind: "retired" }, "retired", undefined]]);
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(s.started?.slot?.state, "active");
  });

  it("a Codex owner whose seal is unproven publishes no fence: the baseline path runs, not the handoff", async t => {
    const s = await scene(t, { ...caseA, name: "codex-unproven", codex: true, unproven: true });
    assert.equal(fenceOf(s.r), undefined, "an unproven seal never published a completion fence");
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs.length, 0);
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(fs.existsSync(path.join(s.started!.worktree, OWNER_FILE)), false);
  });
});

// ---- a fence-carrying journal that also retains sources: classified, never detached unless positively not a completion ----

/** Rewrite the owner's bare journal in place (the strict reader parses any key order). */
function rewriteJournal(bare: string, change: (journal: Record<string, unknown>) => Record<string, unknown>): string {
  const key = `uzi-recovery.${BRANCH}.clone`;
  const raw = execFileSync("git", ["-C", bare, "config", "--get", key], { encoding: "utf8", env: ENV }).trim();
  const next = JSON.stringify(change(JSON.parse(raw) as Record<string, unknown>));
  execFileSync("git", ["-C", bare, "config", "--local", key, next], { env: ENV, stdio: "pipe" });
  return next;
}
const withRetainedSource = (j: Record<string, unknown>): Record<string, unknown> => {
  const { completionSourceFence, ...rest } = j;
  return { ...rest, retainedSources: [{ runId: j["runId"], clonePath: j["clonePath"], ...(j["attemptId"] === undefined ? {} : { attemptId: j["attemptId"] }) }],
    completionSourceFence };
};

describe("a successor claim on a completed owner whose journal retains sources", linux, () => {
  for (const row of [caseA, caseB]) {
    it(`${row.name}: a published source is found: a retained stop, never detached`, async t => {
      const s = await scene(t, row);
      const planted = rewriteJournal(s.r.bare, withRetainedSource);
      const before = durable(s);
      const spies = [t.mock.method(git, "detachTerminalRetained"), t.mock.method(git, "releaseAttemptInPlace"), t.mock.method(git, "retireRunnerClone")];
      await s.runner.execute(s.claim);
      assert.equal(s.handoffs[0]!.input.mode, "classify");
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "episode_bearing" });
      assert.deepEqual(spies.map(x => x.mock.callCount()), [0, 0, 0], "no uncertified clear of any kind");
      await expectStop(s, before, "published_source_retained");
      assert.equal(journalRaw(s.r), planted, "the journal and its fence are byte-identical");
    });

    it(`${row.name}: a source that fails verification is a retained stop (completion_unverified), never terminal`, async t => {
      const s = await scene(t, row);
      await rewriteSources(s.r, src => ({ ...src, certificate: flip(src.certificate as string) }));
      rewriteJournal(s.r.bare, withRetainedSource);
      const before = durable(s);
      const detach = t.mock.method(git, "detachTerminalRetained");
      await s.runner.execute(s.claim);
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "invalid", why: "unverified" });
      assert.equal(detach.mock.callCount(), 0);
      await expectStop(s, before, "completion_unverified");
    });
  }

  /** The second readOrphanJournalFence call (the re-read after the coordinator's outcome) throws. */
  const failSecondRead = (t: TestContext): void => {
    const real = git.readOrphanJournalFence.bind(git);
    let calls = 0;
    t.mock.method(git, "readOrphanJournalFence", (async (...args: Parameters<typeof real>) => {
      if (++calls === 2) throw new Error("EIO: injected");
      return real(...args);
    }) as never);
  };

  it("a re-read that throws after a pending outcome keeps the retained stop", async t => {
    const s = await scene(t, caseA, { receipt: false });
    failSecondRead(t);
    const before = durable(s);
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "no_receipt" });
    await expectStop(s, before, "completion_pending");
  });

  it("a re-read that throws after a found retained source keeps the retained stop", async t => {
    const s = await scene(t, caseA);
    rewriteJournal(s.r.bare, withRetainedSource);
    failSecondRead(t);
    const before = durable(s);
    const detach = t.mock.method(git, "detachTerminalRetained");
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "episode_bearing" });
    assert.equal(detach.mock.callCount(), 0);
    await expectStop(s, before, "published_source_retained");
  });

  it("a re-read that throws after an invalid outcome refuses with the original outward error", async t => {
    const s = await scene(t, caseA);
    await rewriteSources(s.r, src => ({ ...src, certificate: flip(src.certificate as string) }));
    failSecondRead(t);
    const before = durable(s);
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "invalid", why: "unverified" });
    await expectRefusal(s, before, "completion_unverified");
  });

  it("a fence that appears between the classification and the snapshot is never detached", async t => {
    const s = await scene(t, caseA);
    const fence = fenceOf(s.r);
    // Classified unfenced and retaining a source: the baseline detach would apply.
    rewriteJournal(s.r.bare, j => { const { completionSourceFence: _drop, ...rest } = withRetainedSource(j); return rest; });
    const real = git.terminalRetainedSnapshot.bind(git);
    t.mock.method(git, "terminalRetainedSnapshot", (async (...args: Parameters<typeof real>) => {
      rewriteJournal(s.r.bare, j => ({ ...j, completionSourceFence: fence }));
      return real(...args);
    }) as never);
    const detach = t.mock.method(git, "detachTerminalRetained");
    await s.runner.execute(s.claim);
    assert.equal(detach.mock.callCount(), 0);
    assert.deepEqual(reported(s).filter(x => TERMINAL.includes(x)), [], "no terminal report");
    assert.equal(diagnostic(s, "orphan_reclaim_refused", "completion_retirement_pending")?.["retirement_subreason"], "completion_pending");
  });

  it("a worker without authority detaches a fence-carrying retained journal when classification and re-read both say unfenced", async t => {
    const s = await scene(t, caseA);
    rewriteJournal(s.r.bare, withRetainedSource);
    Object.assign(git, { authorityRequired: false });
    const detach = t.mock.method(git, "detachTerminalRetained");
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs.length, 0, "the unfenced baseline never reaches the coordinator");
    assert.equal(detach.mock.callCount(), 1, "the baseline detach ran");
    assert.equal(String(failure(s)), STOPPED);
    assert.notEqual(s.started, undefined, "the claimant is seeded");
    assert.equal(diagnostic(s, "orphan_reclaim_succeeded", "retained-in-place")?.["stage"], "complete");
  });

  it("no record and no slot names the fence, and the re-read throws: a retained stop, never detached", async t => {
    const s = await scene(t, caseA);
    await plantOtherFencedRetained(t, s);
    failSecondRead(t);
    const before = durable(s);
    const detach = t.mock.method(git, "detachTerminalRetained");
    await s.runner.execute(s.claim);
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" });
    assert.equal(detach.mock.callCount(), 0);
    await expectStop(s, before, "published_source_retained");
  });

  /** A fenced, source-retaining journal for another completed run at the claimant's own canonical path, unknown to
   *  the authority store (so the coordinator answers not_completion). */
  const plantOtherFencedRetained = async (_t: TestContext, s: Awaited<ReturnType<typeof scene>>): Promise<string> => {
    const other = randomUUID();
    const fence = fenceOf(s.r);
    execFileSync("git", ["-C", s.r.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`, ""], { env: ENV, stdio: "pipe" });
    // Seeded and journaled without the authority hooks: the authority store has never heard of this clone.
    const wiring = git as unknown as { authority?: unknown; authorityRequired?: boolean };
    const hooks = { authority: wiring.authority, authorityRequired: wiring.authorityRequired };
    Object.assign(wiring, { authority: undefined, authorityRequired: false });
    const clone = await git.runnerCloneForBranch(s.r.bare, BRANCH, "agent-issue-3404", {
      isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
    }, other, false);
    const planted = clone.path;
    fs.writeFileSync(path.join(planted, "OTHER.txt"), "other run bytes\n");
    await git.markRecoveryCapture(s.r.bare, planted, BRANCH, other);
    Object.assign(wiring, hooks);
    rewriteJournal(s.r.bare, j => ({ ...withRetainedSource(j), completionSourceFence: fence }));
    api.setOrphanClassification(other, { status: "completed", repo_id: "r1", kind: "mr_rework", issue_iid: 3404, branch: null, pipeline_ref: BRANCH, pipeline_id: null });
    return planted;
  };

  it("no record and no slot names the fence: not_completion falls through to the unchanged detach", async t => {
    const s = await scene(t, caseA);
    const planted = await plantOtherFencedRetained(t, s);
    const detach = t.mock.method(git, "detachTerminalRetained");
    await s.runner.execute(s.claim);
    assert.equal(s.handoffs[0]!.input.mode, "classify");
    assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" });
    assert.equal(detach.mock.callCount(), 1, "the baseline detach ran");
    assert.equal(String(failure(s)), STOPPED);
    assert.equal(fs.readFileSync(path.join(planted, "OTHER.txt"), "utf8"), "other run bytes\n", "the other run's bytes stay in place");
    assert.equal(diagnostic(s, "orphan_reclaim_succeeded", "retained-in-place")?.["stage"], "complete");
  });
});

// ---- the typed outcome, end to end: unavailable coordinator, preparation errors, the negative matrix --------------------------

describe("a successor claim on a completed owner: classify unavailable and preparation errors", linux, () => {
  for (const row of [caseA, caseB]) {
    it(`${row.name}: a fenced retained journal with an unavailable coordinator is a retained stop, never detached`, async t => {
      const s = await scene(t, row);
      const planted = rewriteJournal(s.r.bare, withRetainedSource);
      Object.assign(s.recovery, { authority: undefined });
      const before = durable(s);
      const spies = [t.mock.method(git, "detachTerminalRetained"), t.mock.method(git, "releaseAttemptInPlace"), t.mock.method(git, "retireRunnerClone")];
      await s.runner.execute(s.claim);
      assert.equal(s.handoffs[0]!.input.mode, "classify");
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "uncertain", why: "classify_unavailable" });
      assert.deepEqual(spies.map(x => x.mock.callCount()), [0, 0, 0], "no uncertified clear of any kind");
      await expectStop(s, before, "published_source_retained");
      assert.equal(journalRaw(s.r), planted, "the journal is byte-identical");
    });

    it(`${row.name}: a preparation I/O error is a retained stop with no failed report and no path in the reason`, async t => {
      const s = await scene(t, row);
      const before = durable(s);
      failBundleLstat(t, s.r, s.recovery);
      await s.runner.execute(s.claim);
      assert.deepEqual(s.handoffs[0]!.outcome, { kind: "uncertain", why: "preparation_error" });
      assert.equal(failure(s), undefined, "no failed report");
      assert.equal(JSON.stringify(api.states.filter(x => x.runId === s.claim.run_id)).includes(s.r.clone), false, "no owner path in any report");
      await expectStop(s, before, "completion_uncertain");
    });
  }
});

describe("a successor claim on a completed owner: the negative matrix through real authority state", linux, () => {
  for (const row of [caseA, caseB]) {
    describe(row.name, () => {
      it("(a) a record whose source epoch differs from the journal fence: not_completion, the outward refusal (fenced_source)", async t => {
        const s = await scene(t, row);
        await rewriteSources(s.r, src => ({ ...src, sourceUseEpoch: "99999999-9999-4999-8999-999999999999" }));
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" });
        await expectRefusal(s, before, "fenced_source");
      });

      it("(b) a V1 (legacy) completion source record: not_completion, the outward refusal (fenced_source)", async t => {
        const s = await scene(t, row);
        await rewriteSources(s.r, src => legacyCompletionCore(src as unknown as CompletionSourceV2) as unknown as Record<string, unknown>);
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "not_completion" });
        await expectRefusal(s, before, "fenced_source");
      });

      it("(c) an extra capture record in the generation (cohort mismatch): invalid, the outward refusal (completion_unverified)", async t => {
        const s = await scene(t, row);
        const [seed] = await s.recovery.inspect(s.r.runId);
        await (s.recovery as unknown as { writeRecordUnlocked(rec: unknown): Promise<void> })
          .writeRecordUnlocked({ ...seed, captureId: "00000000-0000-4000-8000-0000000000dd" });
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.equal(s.handoffs[0]!.outcome?.kind, "invalid");
        await expectRefusal(s, before, "completion_unverified");
      });

      it("(c) a missing capture record in the generation (cohort mismatch): invalid, the outward refusal (completion_unverified)", async t => {
        const s = await scene(t, row, { cohort: 2 });
        // Drop a member that does not carry the receipt, so the selection still finds the authority record.
        const bare = readRecords(s.r).find(rec => !rec["completionReceipt"]);
        assert.ok(bare, "a cohort member without the receipt exists");
        fs.rmSync(path.join(recordDir(s.r), `${String(bare["captureId"])}.json`));
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.equal(s.handoffs[0]!.outcome?.kind, "invalid");
        await expectRefusal(s, before, "completion_unverified");
      });

      it("(d) an unresolved operation on the owner's slot: pending/unresolved_operation, a retained stop", async t => {
        // Not constructible through real state on a PUBLISHED slot: the authority refuses a destructive record over a
        // published source without its retiring lease, and a retiring slot's journal fence is already gone (the claimant
        // would never reach the coordinator). The narrowest seam: the retirement inspection raises the typed refusal the
        // real authority raises for a running record (authority-runtime `operation_unresolved`).
        const s = await scene(t, row);
        (s.rt.retirement as { inspect: unknown }).inspect = async () => {
          throw new AuthorityRefusal("an operation on the retiring source is unresolved", "operation_unresolved");
        };
        const before = durable(s);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "pending", why: "unresolved_operation" });
        await expectStop(s, before, "completion_pending");
      });

      it("(e) a failing completion-check cleanup: uncertain/preparation_error, a retained stop", async t => {
        const s = await scene(t, row);
        const before = durable(s);
        const real = (git.cleanupRecoveryGeneration as (...a: unknown[]) => Promise<unknown>).bind(git);
        t.mock.method(git, "cleanupRecoveryGeneration", (async (...args: unknown[]) => {
          if (args[7] === "completion-check") throw Object.assign(new Error(`EIO: ${s.r.clone}`), { code: "EIO" });
          return real(...args);
        }) as never);
        await s.runner.execute(s.claim);
        assert.deepEqual(s.handoffs[0]!.outcome, { kind: "uncertain", why: "preparation_error" });
        await expectStop(s, before, "completion_uncertain");
      });
    });
  }
});
