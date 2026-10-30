import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { AuthorityLockBusy, AuthorityRefusal } from "../src/authority-lock.js";
import { parseCompletionSourceV2, type CompletionSourceFence } from "../src/completion-source.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { nullLogger } from "./helpers.js";
import type { HandoffRetirement, HandoffRetirementInput } from "../src/recovery.js";
import { git, installHarness } from "./runner-harness.js";
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
});

describe("retireCompletedSourceForHandoff: classify", linux, () => {
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
