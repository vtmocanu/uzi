import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fsSync from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import { afterEach, mock, test } from "node:test";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { type DescribeLocalRecordsResult, type LocalRecordGeneration, type OperatorSourceEvidence, type RecoveryRecord, type ResolveLocalRecordResult } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";
import { binding, closed, environment, fixture, git, inventory, patch, type Variant } from "./fixtures/recovery-bindingless-compat.js";
import { fakeSourceEvidence } from "./fixtures/recovery-case-b-evidence.js";
import { RUN, allRefs, createWorld, gitIn, treeDigest, type World as E2EWorld } from "./fixtures/recovery-local-record-e2e-world.js";

// issue #3885 M1: the operator's Case B disposition. A generation whose archive was never settled (its hold is
// discarded or closed without the saved FINAL's receipt, or the FINAL was never saved) is described as
// `case_b_owner_discard` / `case_b_released_mismatch` and, only with the explicit `case_b` action, has its LOCAL
// RECORDS and bundles removed under the source boundary. Real coordinator and Git; the API client, the physical
// source evidence and (in most tests) the runner's source boundary are the controlled seams. The last test runs the
// real readers and the real RunRunner boundary over a real retained source.

const roots: string[] = [];
const e2eWorlds: E2EWorld[] = [];
afterEach(async () => {
  mock.restoreAll();
  resetResidueQuarantineForTests();
  for (const w of e2eWorlds.splice(0)) await w.stop();
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});

const id = (n: number) => `31050000-0000-4000-8000-${String(n).padStart(12, "0")}`;
type Track = "discarded-final" | "settled-final" | "no-final";
const TRACKS: Track[] = ["discarded-final", "settled-final", "no-final"];
interface Step { kind: string; path: string; phase: string }

/** A real coordinator over a real Git fixture. `track` is the record shape: a saved FINAL with no ACK (whose hold the
 *  server discarded), the same with a persisted non-matching settlement, or a frozen aggregate with no FINAL at all. */
async function world(track: Track = "discarded-final", opts: { bind?: boolean; variant?: Variant } = {}) {
  await fs.mkdir(path.resolve("../.uzi/scratch"), { recursive: true });
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/case-b-"));
  roots.push(root);
  const f = await fixture(root, opts.variant ?? (track === "no-final" ? "uploaded" : "saved-final"));
  const bare = f.context.barePath;
  await fs.chmod(path.dirname(bare), 0o700);
  await fs.chmod(bare, 0o700);
  await fs.chmod(path.join(bare, "config"), 0o600);
  const e = environment(f);
  const { coordinator } = e;
  const producer = coordinator["git"], client = coordinator["client"];
  const cache = new GitCache(path.join(root, "data"), nullLogger());
  producer.recoveryRefSnapshot = cache.recoveryRefSnapshot.bind(cache);
  producer.assertRecoveryBareClear = cache.assertRecoveryBareClear.bind(cache);
  producer.verifyRecoveryCoverage = cache.verifyRecoveryCoverage.bind(cache);
  e.state.open = false;
  if (opts.bind !== false) f.record = await patch(coordinator, f.record, { exactHoldBinding: binding });
  const runId = f.context.runId;
  const request = new AbortController(), shutdown = new AbortController();
  const counts = { forgotten: 0, released: 0, redundancy: 0, cleanup: 0, forget: 0 };
  const steps: Step[] = [];
  const contexts: Array<{ recordDisposition?: { expectedClones: string } }> = [];
  const w = {
    f, e, coordinator, producer, client, root, bare, runId, request, shutdown, counts, steps, contexts,
    exact: closed(f) as RecoveryExactHold | undefined | "throw",
    others: new Map<number, RecoveryExactHold>(),
    evidence: (): OperatorSourceEvidence => fakeSourceEvidence(runId, bare),
    evidenceCalls: 0,
    stepHook: undefined as undefined | ((s: Step) => void | Promise<void>),
    /** "retain": the boundary answers retained before the action; "prove-false": the in-action proof fails. */
    boundary: "pass" as "pass" | "retain" | "prove-false" | "throw",
    proves: 0,
    sig: (over: Partial<{ deadlineAt: number }> = {}): { requestSignal: AbortSignal; shutdownSignal: AbortSignal; deadlineAt: number } => ({ requestSignal: request.signal, shutdownSignal: shutdown.signal,
      deadlineAt: Date.now() + 60_000, ...over }),
    describe: (): Promise<DescribeLocalRecordsResult> => coordinator.describeLocalRecords(runId, w.sig()),
    resolve: (confirm: string, over: Partial<{ generation: number; deadlineAt: number; noAction: boolean; action: "case_b" }> = {}): Promise<ResolveLocalRecordResult> => {
      const { noAction, ...rest } = over;
      return coordinator.resolveLocalRecord({ runId, generation: 7, confirm, ...(noAction ? {} : { action: "case_b" as const }), ...w.sig(), ...rest });
    },
    async view(generation = 7): Promise<LocalRecordGeneration | undefined> {
      const d = await w.describe();
      assert.equal(d.kind, "described", JSON.stringify(d));
      return d.kind === "described" ? d.generations.find(g => g.generation === generation) : undefined;
    },
    async classification(): Promise<string> {
      const d = await w.describe();
      assert.equal(d.kind, "described", JSON.stringify(d));
      return d.kind === "described" ? (d.generations.find(g => g.generation === 7)?.classification ?? d.blockers.join(",")) : "";
    },
    async digest(): Promise<string> {
      const g: LocalRecordGeneration | undefined = await w.view();
      assert.ok(g?.digest, JSON.stringify(g));
      return g.digest;
    },
    refs: (): string => git(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    latest: async (): Promise<RecoveryRecord> => (await coordinator.inspect(runId)).find(r => r.generation === 7 && r.captureId === f.record.captureId)!,
    async state(): Promise<{ bytes: Awaited<ReturnType<typeof inventory>>; refs: string }> { return { bytes: await inventory(root), refs: w.refs() }; },
    /** Nothing was touched and no forbidden path ran. */
    async assertUntouched(before: { bytes: Awaited<ReturnType<typeof inventory>>; refs: string }): Promise<void> {
      assert.deepEqual(await inventory(root), before.bytes, "every byte unchanged (records, bundle, source tree)");
      assert.equal(w.refs(), before.refs, "every ref unchanged");
      w.assertNoForbidden();
    },
    assertNoForbidden(): void {
      assert.deepEqual(counts, { forgotten: 0, released: 0, redundancy: 0, cleanup: 0, forget: 0 },
        "no cleanup, redundancy, release or completion-witness path ran");
      assert.equal(e.calls.final + e.calls.reserve + e.calls.upload + e.calls.produce + e.calls.held, 0, "no api write");
    },
  };
  w.exact = closed(f);
  client.getExactRecoveryHold = async (_run, generation) => {
    if (generation !== 7) return w.others.get(generation);
    if (w.exact === "throw") throw new Error("fixture exact read failed");
    return w.exact;
  };
  client.proveRecoveryArchiveRedundancy = async () => { counts.redundancy++; throw new Error("redundancy must not run"); };
  producer.cleanupRecoveryGeneration = async () => { counts.cleanup++; throw new Error("cleanup must not run"); };
  const forget = coordinator["forgetGenerationWithinCycle"].bind(coordinator);
  Object.assign(coordinator, {
    forgetGenerationWithinCycle: (...args: Parameters<typeof forget>) => { counts.forget++; return forget(...args); },
    onCompletionGenerationForgotten: () => { counts.forgotten++; },
    onAuthoritativeGenerationReleased: () => { counts.released++; },
    operatorSourceEvidence: async () => { w.evidenceCalls++; return w.evidence(); },
    onOperatorDisposeStepForTest: async (s: Step) => { steps.push(s); await w.stepHook?.(s); },
    withInventorySourceBoundary: async (context: { recordDisposition?: { expectedClones: string }; reportDiagnostic?: (d: unknown) => unknown },
      action: (prove: () => Promise<boolean>) => Promise<void>) => {
      contexts.push(context);
      if (w.boundary === "retain") { context.reportDiagnostic?.({ check: "process_not_quiescent_before_git" }); return "retained"; }
      const prove = async (): Promise<boolean> => {
        w.proves++;
        if (w.boundary === "prove-false" && w.proves > 1) { context.reportDiagnostic?.({ check: "source_identity_changed" }); return false; }
        return true;
      };
      if (!await prove()) return "retained";
      // The production boundary swallows an error from the action and answers "retained".
      try { await action(prove); return "passed"; } catch { return "retained"; }
    },
  });
  if (track === "settled-final") f.record = await patch(coordinator, f.record, { holdSettlement: closed(f) });
  return w;
}
type World = Awaited<ReturnType<typeof world>>;

/** A released hold whose receipt is NOT the saved FINAL's (the "released mismatch" evidence). */
const mismatched = (w: World, over: Partial<RecoveryExactHold> = {}): RecoveryExactHold => ({ ...closed(w.f), state: "released",
  release_evidence: "publication", final_receipt: { kind: "settled", coverage_digest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" },
  ...over });

/** Write an extra record of the run through the real journal writer. */
async function addRecord(w: World, over: Partial<RecoveryRecord> = {}, from: "bare" | "anchor" = "bare"): Promise<RecoveryRecord> {
  const base: RecoveryRecord = from === "anchor" ? { ...(await w.latest()) } : { version: 1, runId: w.runId, captureId: "", sourceSha: w.f.head,
    kind: "issue", branch: w.f.context.branch, generation: 7, createdAt: 1, state: "pinned" };
  const captureId = randomUUID();
  // A copy owns its own canonical bundle path (the journal refuses a record that names another capture's bundle).
  const record: RecoveryRecord = { ...base, captureId, ...(base.bundlePath ? { bundlePath: path.join(w.coordinator["runDir"](w.runId), `${captureId}.bundle`) } : {}),
    ...over };
  await w.coordinator["withJournalLock"](w.runId, () => w.coordinator["writeRecordUnlocked"](record));
  return record;
}

/** The refusal: `resolve` with the digest taken BEFORE `setup` (when one exists) is refused by name and nothing changed. */
async function refusedBy(w: World, reason: string, setup: () => void | Promise<void>, over: Parameters<World["resolve"]>[1] = {}) {
  const view = await w.view();
  const confirm = view?.digest ?? "c".repeat(64);
  await setup();
  const before = await w.state();
  assert.deepEqual(await w.resolve(confirm, over), { kind: "refused", reason });
  await w.assertUntouched(before);
}

// ── positives ────────────────────────────────────────────────────────────────────────────────────────────

for (const track of TRACKS) {
  test(`${track}: described as case_b_owner_discard, confirmed with the action, only the local records go`, async () => {
    const w = await world(track);
    const view = await w.view();
    assert.equal(view?.classification, "case_b_owner_discard");
    assert.equal(view?.action, "case_b");
    assert.deepEqual(view?.laterCleanup, []);
    assert.match(view?.digest ?? "", /^[0-9a-f]{64}$/);
    assert.equal(await w.digest(), view?.digest, "describe is stable over an unchanged fixture");
    // The genuine #4002 shape: discarded by the owner, no final receipt, no live pointers.
    const exact = w.exact as RecoveryExactHold;
    assert.deepEqual([exact.state, exact.release_evidence, exact.final_receipt, exact.live_worker_id, exact.live_run_id],
      ["discarded", "owner_discard", undefined, undefined, undefined]);
    const refs = w.refs();
    const bundle = w.f.record.bundlePath!;
    await fs.stat(bundle);
    assert.deepEqual(await w.resolve(view!.digest!), { kind: "retired", generation: 7, remaining: 0 });
    assert.deepEqual(await w.coordinator.inspect(w.runId), []);
    await assert.rejects(fs.stat(bundle), { code: "ENOENT" });
    await assert.rejects(fs.stat(w.coordinator["runDir"](w.runId)), { code: "ENOENT" });
    assert.equal(w.refs(), refs, "no ref or pin is touched");
    assert.equal(w.contexts.length, 1);
    assert.deepEqual(w.contexts[0]?.recordDisposition, { expectedClones: JSON.stringify([[
      path.join(w.root, "data", "runner", "fixture", "issue-3105"), "agent/issue-3105", "a".repeat(40), "1:42"]]) });
    assert.equal(w.proves, 2, "the boundary proved before the action and again under J");
    assert.deepEqual(w.steps.map(s => `${s.kind}:${s.phase}`),
      ["bundle:before", "bundle:after", "anchor:before", "anchor:after", "rmdir:before", "rmdir:after"]);
    w.assertNoForbidden();
  });
}

test("a closed hold whose receipt is not the saved FINAL's is case_b_released_mismatch, and retires the same way", async () => {
  for (const track of TRACKS) {
    const w = await world(track);
    w.exact = mismatched(w);
    if (track === "settled-final") await patch(w.coordinator, await w.latest(), { holdSettlement: w.exact });
    assert.equal(await w.classification(), "case_b_released_mismatch", track);
    assert.deepEqual(await w.resolve(await w.digest()), { kind: "retired", generation: 7, remaining: 0 }, track);
    w.assertNoForbidden();
  }
});

test("a released hold with the saved FINAL's own receipt is still the Case A replay, never Case B", async () => {
  const w = await world("discarded-final");
  const request = w.f.record.finalRequest!;
  w.exact = { ...closed(w.f), state: "released", release_evidence: request.evidence, final_receipt: request.disposition };
  const view = await w.view();
  assert.equal(view?.classification, "case_a_replay");
  assert.equal(view?.action, undefined);
  assert.equal(view?.laterCleanup, undefined);
});

test("a multi-record generation where only the anchor carries the binding: bundles first, unbound records, binding carriers, the anchor last", async () => {
  const w = await world("discarded-final");
  const anchor = await w.latest();
  const loose = await addRecord(w);
  const looseBundle = w.coordinator["bundlePath"](loose) as string;
  await fs.writeFile(looseBundle, "sibling bundle bytes");
  const carrier = await addRecord(w, { inventoryGuarded: true, exactHoldBinding: binding });
  assert.equal(anchor.exactHoldBinding !== undefined && loose.exactHoldBinding === undefined, true);
  assert.equal(await w.classification(), "case_b_owner_discard");
  const digest = await w.digest();
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  const unlinked = w.steps.filter(s => s.phase === "after").map(s => [s.kind, path.basename(s.path)]);
  const kinds = unlinked.map(([kind]) => kind);
  // Every record's canonical bundle is a step (an absent one is a tolerated ENOENT, as in the A1 cleanup).
  assert.deepEqual(kinds, ["bundle", "bundle", "bundle", "record", "record", "anchor", "rmdir"]);
  assert.deepEqual(unlinked.slice(0, 3).map(([, name]) => name).sort(),
    [`${anchor.captureId}.bundle`, `${loose.captureId}.bundle`, `${carrier.captureId}.bundle`].sort());
  assert.deepEqual(unlinked.slice(3, 5).map(([, name]) => name), [`${loose.captureId}.json`, `${carrier.captureId}.json`], "no binding is removed before the unbound record");
  assert.equal(unlinked[5]?.[1], `${anchor.captureId}.json`);
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
  w.assertNoForbidden();
});

// ── binding_missing, cleanup_pending ─────────────────────────────────────────────────────────────────────

test("binding_missing: no persisted binding on the anchor, or an archive FINAL whose capture id is missing or different", async () => {
  // (A persisted settlement requires a binding, so the settled shape cannot be unbound.)
  for (const track of ["discarded-final", "no-final"] as const) {
    const unbound = await world(track, { bind: false });
    assert.equal(await unbound.classification(), "binding_missing", track + " without a binding (and no finalRequest dereference)");
    assert.deepEqual(await unbound.resolve("c".repeat(64)), { kind: "refused", reason: "binding_missing" });
  }
  // An archive FINAL that names no capture at all (neither the request nor the record) is readable but has no association.
  for (const track of ["discarded-final", "settled-final"] as const) {
    const w = await world(track);
    const anchor = await w.latest();
    await patch(w.coordinator, anchor, { serverCaptureId: undefined,
      finalRequest: { ...anchor.finalRequest!, disposition: { ...anchor.finalRequest!.disposition, capture_id: undefined } } });
    assert.equal(await w.classification(), "binding_missing", track + " with no archive capture identity");
    // A capture id that disagrees with the FINAL is not even an authenticated record.
    const wrong = await world(track);
    await patch(wrong.coordinator, await wrong.latest(), { serverCaptureId: id(0xa2) });
    assert.equal(await wrong.classification(), "record_unverified", track + " with a different capture id");
  }
  // No saved FINAL: no archive association to check, so the same record is still Case B.
  const none = await world("no-final");
  await patch(none.coordinator, await none.latest(), { serverCaptureId: undefined });
  assert.equal(await none.classification(), "case_b_owner_discard");
});

test("binding_inconsistent: another record of the generation carries a different binding", async () => {
  const w = await world("no-final");
  await addRecord(w, { inventoryGuarded: true, exactHoldBinding: { holdId: id(77), repoId: binding.repoId } });
  assert.equal(await w.classification(), "binding_inconsistent");
});

test("cleanup_pending: two saved FINALs, an ACK elsewhere in the generation, and no or several frozen aggregates", async () => {
  const two = await world("discarded-final");
  await addRecord(two, {}, "anchor");
  assert.equal(await two.classification(), "cleanup_pending");
  const ack = await world("discarded-final");
  await addRecord(ack, { finalAcknowledged: true }, "anchor");
  assert.equal(await ack.classification(), "cleanup_pending");
  const none = await world("no-final");
  await patch(none.coordinator, await none.latest(), { coverageDigest: undefined, coverageContext: undefined, originalRoots: undefined,
    originalSourceSha: undefined, inventoryCurrentSha: undefined });
  assert.equal(await none.classification(), "cleanup_pending");
  const several = await world("no-final");
  await addRecord(several, {}, "anchor");
  assert.equal(await several.classification(), "cleanup_pending");
});

test("a settlement difference that is not on the record leaves a refused anchor refused unless a settlement is persisted", async () => {
  const plain = await world("discarded-final");
  await patch(plain.coordinator, await plain.latest(), { reason: "final_retry_refused" });
  assert.equal(await plain.classification(), "replay_refused_persisted", "no persisted settlement: unchanged");
  const settled = await world("settled-final");
  await patch(settled.coordinator, await settled.latest(), { reason: "credential_rejected" });
  assert.equal(await settled.classification(), "case_b_owner_discard", "with a non-matching settlement it qualifies");
});

// ── shared refusals, reused over the Case B action ───────────────────────────────────────────────────────

test("shared refusals hold for Case B: executing, quarantine, unknown holds, missing exact evidence, report, protection, settlement root", async () => {
  for (const track of TRACKS) {
    const executing = await world(track);
    await refusedBy(executing, "executing", () => { Object.assign(executing.coordinator, { isExecuting: () => true }); });
    const quarantined = await world(track);
    await refusedBy(quarantined, "worker_quarantined", () => { latchResidueQuarantine({ cause: "fixture", site: "test" }, nullLogger()); });
    resetResidueQuarantineForTests();
    const unknown = await world(track);
    await refusedBy(unknown, "holds_unknown", () => { unknown.client.listRecoveryHolds = async () => { throw new Error("down"); }; });
    const unavailable = await world(track);
    await refusedBy(unavailable, "exact_evidence_unavailable", () => { unavailable.exact = undefined; });
    const thrown = await world(track);
    await refusedBy(thrown, "exact_evidence_unavailable", () => { thrown.exact = "throw"; });
    const running = await world(track);
    await refusedBy(running, "run_state_not_terminal", () => { running.exact = { ...closed(running.f), run_status: "running" }; });
    const report = await world(track);
    await refusedBy(report, "pending_generation_report", () => { Object.assign(report.coordinator, { pendingGenerationReport: () => true }); });
    const terminal = await world(track);
    await refusedBy(terminal, "terminal_protection", () => { Object.assign(terminal.coordinator, { terminalRecordProtection: async () => true }); });
    const open = await world(track);
    await refusedBy(open, "hold_open", () => { open.exact = { ...closed(open.f), state: "open", live_worker_id: "w", live_run_id: open.runId,
      release_evidence: undefined }; });
  }
  const bare = await world();
  await refusedBy(bare, "bare_mismatch", () => { bare.producer.resolveRecoveryBareDir = async () => path.join(bare.root, "data", "repos", "elsewhere.git"); });
  const settlement = await world();
  await refusedBy(settlement, "settlement_record_unsupported", async () => {
    const dir = path.join(settlement.root, "data", "recovery-settlement", settlement.runId);
    await fs.mkdir(dir, { recursive: true });
    await fs.writeFile(path.join(dir, "x.json"), "{}");
  });
  const unbound = await world();
  await refusedBy(unbound, "unbound_artifact:stray.tmp", async () => {
    await fs.writeFile(path.join(path.dirname(unbound.coordinator["recordPath"](unbound.f.record)), "stray.tmp"), "stray");
  });
  const origin = await world();
  await refusedBy(origin, "non_ordinary_origin", async () => { await addRecord(origin, { captureId: "maintenance-v1-" + randomUUID() }); });
});

test("an invalid MAC refuses the whole run", async () => {
  const w = await world();
  const digest = await w.digest();
  const file = w.coordinator["recordPath"](w.f.record) as string;
  const raw = JSON.parse(await fs.readFile(file, "utf8"));
  await fs.writeFile(file, JSON.stringify({ ...raw, reason: "tampered" }));
  const before = await w.state();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "record_unverified" });
  await w.assertUntouched(before);
});

test("client, shutdown and deadline before the boundary are named refusals and touch nothing", async () => {
  const gone = await world();
  const digest = await gone.digest();
  gone.request.abort();
  const before = await gone.state();
  assert.deepEqual(await gone.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  await gone.assertUntouched(before);
  const down = await world();
  const downDigest = await down.digest();
  down.shutdown.abort();
  assert.deepEqual(await down.resolve(downDigest), { kind: "refused", reason: "shutting_down" });
  const late = await world();
  assert.deepEqual(await late.resolve(await late.digest(), { deadlineAt: Date.now() - 1 }), { kind: "refused", reason: "deadline_exhausted" });
  assert.equal(late.contexts.length, 0);
});

test("the client leaving after the evidence was read but before the first unlink ends the request with nothing touched", async () => {
  const w = await world();
  const digest = await w.digest();
  // Reads: the pre-check outside J (1), then the re-assessment under J inside the source boundary (2): the client leaves there.
  let reads = 0;
  w.evidence = () => { if (++reads === 2) w.request.abort(); return fakeSourceEvidence(w.runId, w.bare); };
  const before = await w.state();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  await w.assertUntouched(before);
  assert.deepEqual(w.steps, []);
});

test("the client leaving at the first unlink does not stop the disposition (the boundary is crossed)", async () => {
  const w = await world();
  const digest = await w.digest();
  w.stepHook = s => { if (s.phase === "before") w.request.abort(); };
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
});

// ── the action ───────────────────────────────────────────────────────────────────────────────────────────

test("the action is mandatory for Case B, forbidden for Case A and validated", async () => {
  for (const track of TRACKS) {
    const w = await world(track);
    const digest = await w.digest();
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest, { noAction: true }), { kind: "refused", reason: "case_b_disposition_required" }, track);
    assert.deepEqual(await w.resolve(digest, { action: "bogus" as never }), { kind: "refused", reason: "invalid_request" }, track);
    assert.deepEqual(await w.resolve("a".repeat(64)), { kind: "refused", reason: "descriptor_changed" }, track);
    await w.assertUntouched(before);
    assert.equal(w.contexts.length, 0, "no source boundary was entered");
  }
  const a1 = await world("discarded-final", { variant: "acknowledged" });
  a1.exact = { ...closed(a1.f), state: "released", release_evidence: a1.f.record.finalRequest!.evidence, final_receipt: a1.f.record.finalRequest!.disposition };
  const view = await a1.view();
  assert.equal(view?.classification, "case_a_ack");
  assert.deepEqual([view?.action, view?.laterCleanup], [undefined, undefined]);
  const before = await a1.state();
  assert.deepEqual(await a1.resolve(view!.digest!), { kind: "refused", reason: "case_b_action_mismatch" });
  await a1.assertUntouched(before);
});

test("Case A is unchanged by a later generation's open hold, while Case B refuses it as run_hold_open", async () => {
  const a1 = await world("discarded-final", { variant: "acknowledged" });
  a1.exact = { ...closed(a1.f), state: "released", release_evidence: a1.f.record.finalRequest!.evidence, final_receipt: a1.f.record.finalRequest!.disposition };
  const later = { run_id: a1.runId, holds: [{ hold_id: id(8), generation: 8, inventory_guarded: true, has_available_capture: false }] };
  a1.client.listRecoveryHolds = async () => later;
  assert.equal(await a1.classification(), "case_a_ack");
  for (const track of TRACKS) {
    const w = await world(track);
    const digest = await w.digest();
    w.client.listRecoveryHolds = async () => later;
    assert.equal(await w.classification(), "run_hold_open", track);
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "run_hold_open" });
    await w.assertUntouched(before);
    // The run-wide count, when the api reports it, is a hold too.
    const counted = await world(track);
    counted.client.listRecoveryHolds = async () => ({ run_id: counted.runId, holds: [], run_open_holds: { run_id: counted.runId, count: 1 } });
    assert.equal(await counted.classification(), "run_hold_open", track + " run-wide");
  }
  // A saved FINAL whose fresh read is discarded is Case B only after the exact read: it still refuses a later hold.
  const fresh = await world("discarded-final");
  fresh.client.listRecoveryHolds = async () => later;
  assert.equal(await fresh.classification(), "run_hold_open");
});

test("an absent run-wide open-hold count is unknown, never zero: Case B refuses it, Case A does not change", async () => {
  for (const track of TRACKS) {
    const w = await world(track);
    const digest = await w.digest();
    w.client.listRecoveryHolds = async () => ({ run_id: w.runId, holds: [] });
    const view = await w.view();
    assert.equal(view?.classification, "holds_unknown", track);
    assert.equal(view?.digest, undefined, track + ": no Case B digest is offered");
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "holds_unknown" }, track);
    await w.assertUntouched(before);
  }
  const a1 = await world("discarded-final", { variant: "acknowledged" });
  a1.exact = { ...closed(a1.f), state: "released", release_evidence: a1.f.record.finalRequest!.evidence, final_receipt: a1.f.record.finalRequest!.disposition };
  a1.client.listRecoveryHolds = async () => ({ run_id: a1.runId, holds: [] });
  assert.equal(await a1.classification(), "case_a_ack");
  // A saved FINAL whose fresh read turns out discarded is Case B only after the exact read, and refuses the same way.
  const fresh = await world("discarded-final");
  fresh.client.listRecoveryHolds = async () => ({ run_id: fresh.runId, holds: [] });
  assert.equal(await fresh.classification(), "holds_unknown");
});

test("a malformed run-wide open-hold count is unknown on every Case B track; a well-formed one is read exactly", async () => {
  const malformed: Array<[string, (runId: string) => unknown]> = [
    ["mismatched run_id", () => ({ run_id: id(55), count: 0 })],
    ["missing run_id", () => ({ count: 0 })],
    ["count 0.5", runId => ({ run_id: runId, count: 0.5 })],
    ["count \"0\"", runId => ({ run_id: runId, count: "0" })],
    ["count NaN", runId => ({ run_id: runId, count: Number.NaN })],
    ["count -1", runId => ({ run_id: runId, count: -1 })],
    ["null", () => null],
  ];
  for (const track of TRACKS) {
    for (const [name, make] of malformed) {
      const w = await world(track);
      const digest = await w.digest();
      w.client.listRecoveryHolds = async () => ({ run_id: w.runId, holds: [], run_open_holds: make(w.runId) as never });
      const view = await w.view();
      assert.equal(view?.classification, "holds_unknown", `${track}: ${name}`);
      assert.equal(view?.digest, undefined, `${track}: ${name}: no digest`);
      const before = await w.state();
      assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "holds_unknown" }, `${track}: ${name}`);
      await w.assertUntouched(before);
    }
    const zero = await world(track);
    zero.client.listRecoveryHolds = async () => ({ run_id: zero.runId, holds: [], run_open_holds: { run_id: zero.runId, count: 0 } });
    assert.equal(await zero.classification(), "case_b_owner_discard", track + ": count 0");
    const two = await world(track);
    two.client.listRecoveryHolds = async () => ({ run_id: two.runId, holds: [], run_open_holds: { run_id: two.runId, count: 2 } });
    assert.equal(await two.classification(), "run_hold_open", track + ": count 2");
  }
});

test("a discarded merged-predecessor policy outcome is retained even when confirmed, and unknown discard evidence is not Case B", async () => {
  for (const final_receipt of [undefined, { kind: "settled", coverage_digest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" }] as const) {
    const w = await world();
    const digest = await w.digest();
    w.exact = { ...closed(w.f), release_evidence: "merged_predecessor_discard", ...(final_receipt ? { final_receipt } : {}) };
    assert.equal(await w.classification(), "policy_discard_retained");
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "policy_discard_retained" });
    await w.assertUntouched(before);
  }
  const unknown = await world();
  unknown.exact = { ...closed(unknown.f), release_evidence: "something_else" };
  assert.equal(await unknown.classification(), "case_b_discard_evidence_unsupported");
});

test("exact identity: a different hold, repo, generation or run in the observation is exact_evidence_unavailable", async () => {
  for (const mutate of [{ hold_id: id(9) }, { repo_id: id(9) }, { generation: 8 }, { run_id: id(9) }, { inventory_guarded: false }] as const) {
    const w = await world();
    w.exact = { ...closed(w.f), ...mutate };
    assert.equal(await w.classification(), "exact_evidence_unavailable", JSON.stringify(mutate));
  }
});

test("a persisted settlement must agree with the fresh read on immutable fields only", async () => {
  // Mutable fields may differ: the run moved on since the settlement was persisted.
  const ok = await world("settled-final");
  ok.exact = { ...closed(ok.f), run_status: "cancelled", current_claim_generation: 9, claim_released: false, run_worker_id: id(9),
    recovery_wait_cause: "worker_requeue_exhausted" };
  assert.equal(await ok.classification(), "case_b_owner_discard");
  const settlement = closed((await world("settled-final")).f);
  const differences: Array<[string, Partial<RecoveryExactHold>]> = [
    ["original_worker_id", { original_worker_id: id(99) }],
    ["original_worker_identity", { original_worker_identity: "another-worker" }],
    ["release_evidence", { release_evidence: "merged_predecessor_discard" }],
    ["final_receipt", { final_receipt: { kind: "settled", coverage_digest: "b".repeat(64) } }],
    ["completed_publication_receipt", { completed_publication_receipt: {} as never }],
    ["state", { state: "released", release_evidence: "publication", final_receipt: settlement.final_receipt }],
  ];
  for (const [name, change] of differences) {
    const w = await world("settled-final");
    w.exact = { ...closed(w.f), ...change };
    assert.equal(await w.classification(), "settlement_evidence_inconsistent", name);
  }
  // Mixed: a second record of the generation carries a settlement of another worker.
  const mixed = await world("settled-final");
  await addRecord(mixed, { inventoryGuarded: true, exactHoldBinding: binding, holdSettlement: { ...closed(mixed.f), original_worker_id: id(98) } });
  assert.equal(await mixed.classification(), "settlement_evidence_inconsistent");
});

test("a stale digest is descriptor_changed whatever changed: a record, a ref, the exact observation, the evidence, the journal", async () => {
  const changes: Array<[string, (w: World) => void | Promise<void>]> = [
    ["a record", async w => { await patch(w.coordinator, await w.latest(), { reason: "changed" }); }],
    ["a ref", w => { git(w.bare, "update-ref", `refs/uzi-owed/${w.runId}/${w.f.head}`, w.f.head); }],
    ["the exact observation", w => { w.exact = { ...closed(w.f), run_status: "cancelled" }; }],
    // Changes between describe and resolve; both resolve reads then agree, so only the digest can notice it.
    ["only the completed publication receipt", w => { w.exact = { ...closed(w.f), completed_publication_receipt: { observed_branch_head: "d".repeat(40) } as never }; }],
    ["another run record", async w => { await addRecord(w, { generation: 3 }); }],
    ["a clone head", w => { w.evidence = () => fakeSourceEvidence(w.runId, w.bare, { head: "b".repeat(40) }); }],
    ["a clone replaced at the same path and head (new dev:ino)", w => { w.evidence = () => fakeSourceEvidence(w.runId, w.bare, { identity: "1:43" }); }],
    ["metadata of the retained source only", w => {
      w.evidence = () => { const ev = fakeSourceEvidence(w.runId, w.bare); (ev.e2.sources[0] as { descriptor: string }).descriptor = "e".repeat(64); return ev; };
    }],
    ["the journal state of the retained source", w => {
      w.evidence = () => { const ev = fakeSourceEvidence(w.runId, w.bare); (ev.e2.sources[0] as { journalState: string }).journalState = "other"; return ev; };
    }],
    ["a consumer of the retained source", w => {
      w.evidence = () => { const ev = fakeSourceEvidence(w.runId, w.bare); ev.e2.sources[0]!.consumers.push("episode_pins:2"); return ev; };
    }],
  ];
  for (const [name, change] of changes) {
    const w = await world();
    const digest = await w.digest();
    await change(w);
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "descriptor_changed" }, name);
    await w.assertUntouched(before);
    assert.equal(w.contexts.length, 0, name + ": refused before the source boundary");
  }
});

test("the exact observation changing inside J (after the pre-check) is descriptor_changed with nothing deleted", async () => {
  const w = await world();
  const digest = await w.digest();
  const before = await w.state();
  let n = 0;
  // The pre-check's evidence read comes after its exact read: the next exact read (under J) sees the change.
  w.evidence = () => { if (++n === 1) w.exact = { ...closed(w.f), run_status: "cancelled" }; return fakeSourceEvidence(w.runId, w.bare); };
  // The mutable run_status is part of the confirmed digest.
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "descriptor_changed" });
  await w.assertUntouched(before);
  assert.equal(w.proves, 1, "refused before the in-action proof");

  // Only a field the digest omits changes inside J: the in-J comparison of the immutable observation must catch it.
  const receipt = await world();
  const receiptDigest = await receipt.digest();
  const receiptBefore = await receipt.state();
  let reads = 0;
  receipt.evidence = () => {
    if (++reads === 1) receipt.exact = { ...closed(receipt.f), completed_publication_receipt: { hold_id: binding.holdId } as never };
    return fakeSourceEvidence(receipt.runId, receipt.bare);
  };
  assert.deepEqual(await receipt.resolve(receiptDigest), { kind: "refused", reason: "descriptor_changed" });
  await receipt.assertUntouched(receiptBefore);
  assert.equal(receipt.proves, 1, "refused before the in-action proof");
});

// ── the source evidence ──────────────────────────────────────────────────────────────────────────────────

type Mutator = (ev: OperatorSourceEvidence) => void;
const e2 = (ev: OperatorSourceEvidence): Record<string, any> => ev.e2 as unknown as Record<string, any>;
const refusals: Array<[string, string, Mutator]> = [
  ["E2 partial", "source_evidence_incomplete", ev => { (e2(ev)).partial = true; }],
  ["E1 unknown", "source_evidence_incomplete", ev => { ev.e1 = { kind: "unknown", cause: "other" } as never; }],
  ["E1 clone without identity", "source_evidence_incomplete", ev => { delete (ev.e1 as any).clones[0].identity; }],
  ["unreadable only, causes empty", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = 1; (e2(ev)).partialCauses = {}; }],
  ["unreadable only, causes stripped", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = 1; delete (e2(ev)).partialCauses; }],
  ["unreadable missing", "source_evidence_incomplete", ev => { delete (e2(ev)).unreadable; }],
  ["unreadable fractional", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = 0.5; }],
  ["unreadable a string", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = "0"; }],
  ["unreadable negative", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = -1; }],
  ["unreadable NaN", "source_evidence_incomplete", ev => { (e2(ev)).unreadable = Number.NaN; }],
  ["partial missing", "source_evidence_incomplete", ev => { delete (e2(ev)).partial; }],
  ["partial a string", "source_evidence_incomplete", ev => { (e2(ev)).partial = "false"; }],
  ["partial zero", "source_evidence_incomplete", ev => { (e2(ev)).partial = 0; }],
  ["sources not a list", "source_evidence_incomplete", ev => { (e2(ev)).sources = {}; }],
  ["a malformed source entry", "source_evidence_incomplete", ev => { (e2(ev)).sources.push(null); }],
  ["a malformed own source", "source_evidence_incomplete", ev => { delete e2(ev).sources[0]!.consumers; }],
  ["both empty", "source_evidence_empty", ev => { (ev.e1 as any).clones = []; (e2(ev)).sources = []; }],
  ["a missing source", "source_missing", ev => { e2(ev).sources[0]!.presence = "missing"; }],
  ["an unsafe part", "source_missing", ev => { e2(ev).sources[0]!.sources[0].presence = "unsafe"; }],
  ["E1 path not in E2", "source_enumeration_mismatch", ev => { (ev.e1 as any).clones[0].clonePath = "/elsewhere/issue-1"; }],
  ["an extra E1 clone", "source_enumeration_mismatch", ev => { (ev.e1 as any).clones.push({ ...(ev.e1 as any).clones[0], clonePath: "/extra/issue-2" }); }],
  ["E1 empty with E2 present", "source_evidence_empty", ev => { (ev.e1 as any).clones = []; }],
  ["E2 has no own physical paths", "source_evidence_empty", ev => { e2(ev).sources[0]!.runId = "other-run"; }],
  ["a named-by-other-journal blocker", "source_not_exclusive:named_by_other_journal:other", ev => { e2(ev).sources[0]!.consumers.push("blocker:named_by_other_journal:other"); }],
  ["a live blocker", "source_not_exclusive:live:issue-3105", ev => { e2(ev).sources[0]!.consumers.push("blocker:live:issue-3105"); }],
];

test("source evidence refusals: each shape is named, and every record, bundle and ref stays byte-identical", async () => {
  for (const [name, reason, mutate] of refusals) {
    const w = await world();
    const digest = await w.digest();
    w.evidence = () => { const ev = fakeSourceEvidence(w.runId, w.bare); mutate(ev); return ev; };
    assert.equal(await w.classification(), reason, name);
    const before = await w.state();
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason }, name);
    await w.assertUntouched(before);
    assert.equal(w.contexts.length, 0, name + ": no boundary was entered");
  }
  const absent = await world();
  Object.assign(absent.coordinator, { operatorSourceEvidence: undefined });
  assert.equal(await absent.classification(), "source_evidence_unavailable");
  const failing = await world();
  failing.evidence = () => { throw new Error("fixture inspection failure"); };
  assert.equal(await failing.classification(), "source_evidence_unavailable");
});

test("an own E2 descriptor with zero parts cannot prove an empty physical inventory", async () => {
  const w = await world();
  const digest = await w.digest();
  w.evidence = () => {
    const ev = fakeSourceEvidence(w.runId, w.bare);
    (ev.e1 as any).clones = [];
    e2(ev).sources[0]!.sources = [];
    return ev;
  };
  const view = await w.view();
  assert.equal(view?.classification, "source_evidence_incomplete");
  assert.equal(view?.digest, undefined, "a malformed own descriptor offers no confirmation digest");
  const before = await w.state();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "source_evidence_incomplete" });
  await w.assertUntouched(before);
  assert.equal(w.contexts.length, 0, "no source boundary was entered");
});

test("diagnostic-only causes and unrelated sources never change eligibility or the digest", async () => {
  const w = await world();
  const baseline = await w.digest();
  const variants: Mutator[] = [
    ev => { delete (e2(ev)).partialCauses; },
    ev => { (e2(ev)).partialCauses = {}; },
    ev => { (e2(ev)).partialCauses = { inspection_budget: 4, tree_binding_failed: 1 }; },
    ev => { (e2(ev)).sampledAt = "2030-01-01T00:00:00.000Z"; },
    ev => { e2(ev).sources[0]!.allocatedBytes = 1; e2(ev).sources[0]!.bytesState = "partial"; },
    // Another run, and the same run in another bare, with blockers of their own: filtered away.
    ev => { e2(ev).sources.push({ ...e2(ev).sources[0]!, runId: id(44), consumers: ["blocker:live:other"] }); },
    ev => { e2(ev).sources.push({ ...e2(ev).sources[0]!, barePath: "/other/bare.git", consumers: ["blocker:live:other"] }); },
    // Pins are consumers, never blockers.
    ev => { e2(ev).sources[0]!.consumers.push("episode_pins:1"); e2(ev).sources[0]!.consumers.splice(1, 1, "owed_pins:1"); },
  ];
  for (const [index, mutate] of variants.entries()) {
    w.evidence = () => { const ev = fakeSourceEvidence(w.runId, w.bare); mutate(ev); return ev; };
    assert.equal(await w.classification(), "case_b_owner_discard", `variant ${index}`);
    const digest = await w.digest();
    // Consumers are part of the descriptor on purpose (the pin count is a fact about the source); the rest is not.
    if (index === variants.length - 1) assert.notEqual(digest, baseline); else assert.equal(digest, baseline, `variant ${index}`);
  }
});

// ── the boundary, the unlinks and their failures ─────────────────────────────────────────────────────────

test("a source boundary that retains before the action refuses with the named diagnostic, before anything is touched", async () => {
  const w = await world();
  const digest = await w.digest();
  w.boundary = "retain";
  const before = await w.state();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "inventory_source_not_quiescent:process_not_quiescent_before_git" });
  await w.assertUntouched(before);
});

test("a proof that fails under J refuses before the first unlink; an action error the boundary swallows keeps its cause", async () => {
  const proof = await world();
  const digest = await proof.digest();
  proof.boundary = "prove-false";
  const before = await proof.state();
  assert.deepEqual(await proof.resolve(digest), { kind: "refused", reason: "inventory_source_not_quiescent:source_identity_changed" });
  await proof.assertUntouched(before);
  assert.deepEqual(proof.steps, []);

  const swallowed = await world();
  const swallowedDigest = await swallowed.digest();
  // An error before the first unlink (the journal listing under J), answered by the boundary as "retained".
  const original = swallowed.coordinator["operatorJournal"].bind(swallowed.coordinator);
  let reads = 0;
  Object.assign(swallowed.coordinator, { operatorJournal: (...args: Parameters<typeof original>) => {
    if (++reads === 2) throw new Error("fixture journal failure");
    return original(...args);
  } });
  const state = await swallowed.state();
  assert.deepEqual(await swallowed.resolve(swallowedDigest), { kind: "refused", reason: "dispose_failed_before_mutation" });
  await swallowed.assertUntouched(state);
});

test("a protection change between unlinks leaves a named partial; the retry needs a fresh confirmation and then completes", async () => {
  const w = await world();
  await addRecord(w, { inventoryGuarded: true, exactHoldBinding: binding });
  const digest = await w.digest();
  let executing = false;
  Object.assign(w.coordinator, { isExecuting: () => executing });
  w.stepHook = s => { if (s.path === w.f.record.bundlePath && s.phase === "after") executing = true; };
  assert.deepEqual(await w.resolve(digest), { kind: "partial", reason: "executing" });
  const left = await w.coordinator.inspect(w.runId);
  assert.equal(left.length, 2, "the anchor's bundle went, both records remain");
  await assert.rejects(fs.stat(w.f.record.bundlePath!), { code: "ENOENT" });
  w.assertNoForbidden();

  executing = false;
  w.stepHook = undefined;
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "descriptor_changed" }, "the old digest is stale after a partial");
  const fresh = await w.view();
  assert.equal(fresh?.classification, "case_b_owner_discard");
  assert.notEqual(fresh?.digest, digest);
  assert.deepEqual(await w.resolve(fresh!.digest!), { kind: "retired", generation: 7, remaining: 0 });
});

test("a pending generation report or terminal protection that appears between unlinks is a partial", async () => {
  for (const [reason, install] of [["pending_generation_report", (w: World) => Object.assign(w.coordinator, { pendingGenerationReport: () => true })],
    ["terminal_protection", (w: World) => Object.assign(w.coordinator, { terminalRecordProtection: async () => true })],
    ["worker_quarantined", () => latchResidueQuarantine({ cause: "fixture", site: "test" }, nullLogger())]] as const) {
    const w = await world();
    const digest = await w.digest();
    w.stepHook = s => { if (s.kind === "bundle" && s.phase === "after") install(w); };
    assert.deepEqual(await w.resolve(digest), { kind: "partial", reason }, reason);
    assert.equal((await w.coordinator.inspect(w.runId)).length, 1, "the anchor remains");
    resetResidueQuarantineForTests();
  }
});

test("an unlink error after the boundary is a partial, never a refusal", async () => {
  const w = await world();
  await addRecord(w);
  const digest = await w.digest();
  const unlink = fs.unlink.bind(fs);
  mock.method(fs, "unlink", async (file: string) => {
    if (String(file).endsWith(`${w.f.record.captureId}.json`)) throw Object.assign(new Error("fixture EIO"), { code: "EIO" });
    return unlink(file);
  });
  const result = await w.resolve(digest);
  assert.deepEqual(result, { kind: "partial", reason: "dispose_failed" });
  const left = await w.coordinator.inspect(w.runId);
  assert.deepEqual(left.map(r => r.captureId), [w.f.record.captureId], "the anchor is the last to go and the one that stayed");
  w.assertNoForbidden();
});

test("the run directory removal tolerates only ENOENT/ENOTEMPTY/EEXIST; the remaining count tolerates only ENOENT", async () => {
  // An unexpected rmdir error is a named partial, with every record already gone.
  const rm = await world();
  const rmDigest = await rm.digest();
  const rmdir = fs.rmdir.bind(fs);
  const runDir = rm.coordinator["runDir"](rm.runId) as string;
  mock.method(fs, "rmdir", async (dir: string) => {
    if (path.resolve(String(dir)) === path.resolve(runDir)) throw Object.assign(new Error("fixture EPERM"), { code: "EPERM" });
    return rmdir(dir);
  });
  assert.deepEqual(await rm.resolve(rmDigest), { kind: "partial", reason: "rmdir_failed" });
  assert.deepEqual(await rm.coordinator.inspect(rm.runId), []);
  mock.restoreAll();

  // A non-empty run directory (ENOTEMPTY) is tolerated and counts only record files.
  const stray = await world();
  const strayDigest = await stray.digest();
  stray.stepHook = async s => {
    if (s.kind === "anchor" && s.phase === "after") await fs.writeFile(path.join(stray.coordinator["runDir"](stray.runId), "stray.txt"), "not a record");
  };
  assert.deepEqual(await stray.resolve(strayDigest), { kind: "retired", generation: 7, remaining: 0 });

  // An unexpected readdir error while counting what remains is a named partial.
  const rd = await world();
  const rdDigest = await rd.digest();
  const readdir = fs.readdir.bind(fs);
  const rdRunDir = rd.coordinator["runDir"](rd.runId) as string;
  rd.stepHook = s => {
    if (s.kind !== "rmdir" || s.phase !== "after") return;
    mock.method(fs, "readdir", async (dir: string, ...rest: unknown[]) => {
      if (path.resolve(String(dir)) === path.resolve(rdRunDir)) throw Object.assign(new Error("fixture EACCES"), { code: "EACCES" });
      return (readdir as (...a: unknown[]) => Promise<unknown>)(dir, ...rest);
    });
  };
  assert.deepEqual(await rd.resolve(rdDigest), { kind: "partial", reason: "remaining_unreadable" });
});

test("shutdown after the boundary is a partial; the client cannot interrupt past it", async () => {
  const w = await world();
  await addRecord(w);
  const digest = await w.digest();
  w.stepHook = s => { if (s.kind === "bundle" && s.phase === "after") w.shutdown.abort(); };
  assert.deepEqual(await w.resolve(digest), { kind: "partial", reason: "shutting_down" });
  assert.equal((await w.coordinator.inspect(w.runId)).length, 2, "only the anchor bundle went; both records remain");
});

test("a deadline that passes after the boundary is a partial", async () => {
  const w = await world();
  await addRecord(w);
  const digest = await w.digest();
  const deadlineAt = Date.now() + 6_000;
  w.stepHook = async s => {
    if (s.kind === "bundle" && s.phase === "after") await new Promise(resolve => setTimeout(resolve, Math.max(0, deadlineAt - Date.now()) + 100));
  };
  assert.deepEqual(await w.resolve(digest, { deadlineAt }), { kind: "partial", reason: "deadline_exhausted" });
  assert.equal((await w.coordinator.inspect(w.runId)).length, 2, "only the anchor bundle went; both records remain");
});

test("an unsafe bundle path is refused before the first unlink (bundle_path_unknown)", async () => {
  const w = await world();
  await patch(w.coordinator, await w.latest(), { bundlePath: path.join(w.root, "elsewhere.bundle") });
  const view = await w.view();
  assert.equal(view?.classification, "case_b_owner_discard");
  const before = await w.state();
  assert.deepEqual(await w.resolve(view!.digest!), { kind: "refused", reason: "bundle_path_unknown" });
  await w.assertUntouched(before);
  assert.deepEqual(w.steps, []);

  const symlink = await world();
  const bundle = symlink.f.record.bundlePath!;
  await fs.rm(bundle);
  await fs.writeFile(path.join(symlink.root, "outside.bundle"), "outside");
  await fs.symlink(path.join(symlink.root, "outside.bundle"), bundle);
  const linkView = await symlink.view();
  assert.equal(linkView?.classification, "case_b_owner_discard");
  assert.deepEqual(await symlink.resolve(linkView!.digest!), { kind: "refused", reason: "bundle_path_unknown" });
  assert.equal((await fs.lstat(bundle)).isSymbolicLink(), true, "the link is left alone");
  assert.equal((await symlink.coordinator.inspect(symlink.runId)).length, 1, "the record is left alone");
  assert.deepEqual(symlink.steps, []);
  symlink.assertNoForbidden();
});

// ── later generations ────────────────────────────────────────────────────────────────────────────────────

test("laterCleanup names what retiring this generation would change for a later one, which stays byte-identical until its own ACK retires it", async () => {
  const w = await world();
  const anchor = await w.latest();
  const later = await addRecord(w, { generation: 8, finalAcknowledged: true, coverageContext: { ...anchor.coverageContext!, generation: 8 } }, "anchor");
  const laterFile = w.coordinator["recordPath"](later) as string;
  const laterBytes = await fs.readFile(laterFile);
  const view = await w.view();
  assert.equal(view?.classification, "case_b_owner_discard");
  assert.deepEqual(view?.laterCleanup, [{ generation: 8, before: "pending", after: "acknowledged" }]);
  // The failing control: while generation 7's unacknowledged FINAL exists, generation 8 is not retirable.
  w.others.set(8, { ...closed(w.f), generation: 8, state: "released", release_evidence: later.finalRequest!.evidence ?? "archive",
    final_receipt: later.finalRequest!.disposition, hold_id: binding.holdId });
  const g8 = (await w.describe()) as { generations: Array<{ generation: number; classification: string }> };
  assert.equal(g8.generations.find(g => g.generation === 8)?.classification, "cleanup_pending");

  assert.deepEqual(await w.resolve(view!.digest!), { kind: "retired", generation: 7, remaining: 1 });
  assert.deepEqual(await fs.readFile(laterFile), laterBytes, "the later generation's record is byte-identical at the end of the request");
  assert.equal(w.counts.forgotten + w.counts.released, 0);

  // A later phase: its own, unchanged ACK predicate now retires it through the ordinary Case A path.
  const after = (await w.describe()) as { generations: Array<{ generation: number; classification: string; digest?: string }> };
  const g8After = after.generations.find(g => g.generation === 8);
  assert.equal(g8After?.classification, "case_a_ack");
  w.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    (args[8] as (() => void) | undefined)?.();
    for (const ref of w.e.refs) git(w.bare, "update-ref", "-d", ref);
    return "removed";
  };
  const result = await w.coordinator.resolveLocalRecord({ runId: w.runId, generation: 8, confirm: g8After!.digest!, ...w.sig() });
  assert.deepEqual(result, { kind: "retired", generation: 8, remaining: 0 });
});

// ── protocol controls: the REAL WorkerClient over a fake transport ───────────────────────────────────────

async function realClient(w: World, caps = ["recovery_held_publication_v2"]) {
  mock.restoreAll();
  const api = { worker: id(5), reads: 0, headers: [] as Array<Headers | undefined>,
    envelope: (): unknown => ({ run_id: w.runId, holds: [], exact_hold: w.exact }),
    inFlight: undefined as undefined | (() => Promise<void>) };
  mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    const u = new URL(String(url));
    if (u.pathname.endsWith("/register")) return Response.json({ worker_id: api.worker, protocol_features: [] });
    if (!u.pathname.includes("/recovery-holds")) throw new Error("unexpected request " + u.pathname);
    api.reads++;
    api.headers.push(init?.headers as Headers | undefined);
    const hook = api.inFlight;
    api.inFlight = undefined;
    await hook?.();
    return Response.json(api.envelope());
  });
  const real = new WorkerClient("http://exact.test", "worker-token", "test", nullLogger());
  const register = (c: string[] = caps) => real.register("worker", undefined, 1, undefined, c);
  await register();
  w.client.getExactRecoveryHold = (runId: string, generation: number, expected: { holdId: string; repoId: string }, signal?: AbortSignal) =>
    real.getExactRecoveryHold(runId, generation, expected, signal);
  return { api, register };
}

test("protocol: the bound exact read works through the real client with the publication advertisement off and no bindingless header", async () => {
  const w = await world();
  const { api } = await realClient(w);
  assert.equal(await w.classification(), "case_b_owner_discard");
  assert.deepEqual(await w.resolve(await w.digest()), { kind: "retired", generation: 7, remaining: 0 });
  assert.ok(api.reads >= 3, "describe, pre-check and the re-assessment under J each read the hold");
  assert.equal(JSON.stringify(api.headers).includes("recovery_bindingless_closed_v1"), false, "no bindingless observation header is requested");
});

test("protocol: a missing exact capability, or a registration that changed in flight, is exact_evidence_unavailable", async () => {
  const noCap = await world();
  await realClient(noCap, []);
  assert.equal(await noCap.classification(), "exact_evidence_unavailable");

  const switched = await world();
  const s = await realClient(switched);
  const digest = await switched.digest();
  s.api.inFlight = async () => { s.api.worker = id(6); await s.register(); };
  const before = await switched.state();
  assert.deepEqual(await switched.resolve(digest), { kind: "refused", reason: "exact_evidence_unavailable" });
  await switched.assertUntouched(before);

  const dropped = await world();
  const d = await realClient(dropped);
  const droppedDigest = await dropped.digest();
  d.api.inFlight = async () => { await d.register([]); };
  assert.deepEqual(await dropped.resolve(droppedDigest), { kind: "refused", reason: "exact_evidence_unavailable" });

  // The client's own validation: an owner discard with a final receipt, or served for another worker, is not a closed proof.
  const forged = await world();
  const f = await realClient(forged);
  f.api.envelope = () => ({ run_id: forged.runId, holds: [], exact_hold: { ...closed(forged.f), final_receipt: { kind: "settled", coverage_digest: "b".repeat(64) } } });
  assert.equal(await forged.classification(), "exact_evidence_unavailable");
});

// ── the real readers and the real RunRunner boundary ─────────────────────────────────────────────────────

test("real readers and the real boundary: forged completeness and a replaced clone refuse; a dirty retained source is kept while its records go", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await createWorld({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: false });
  e2eWorlds.push(w);
  const sig = () => ({ requestSignal: new AbortController().signal, shutdownSignal: new AbortController().signal, deadlineAt: Date.now() + 60_000 });
  const describe = async () => {
    const d = await w.coordinator.describeLocalRecords(RUN, sig());
    assert.equal(d.kind, "described", JSON.stringify(d));
    return d.kind === "described" ? d.generations.find(g => g.generation === 9)! : (undefined as never);
  };
  const resolve = (confirm: string) => w.coordinator.resolveLocalRecord({ runId: RUN, generation: 9, confirm, action: "case_b", ...sig() });
  const footprint = () => ({ source: treeDigest(w.episode.sourcePath), successor: treeDigest(w.episode.successorPath),
    records: treeDigest(path.join(w.dir, "recovery", RUN)), refs: allRefs(w.bare) });

  // The retained predecessor carries unpublished bytes: the record disposition must not demand a clean source.
  fsSync.writeFileSync(path.join(w.episode.sourcePath, "dirty.txt"), "unpublished bytes\n");
  const view = await describe();
  assert.equal(view.classification, "case_b_owner_discard");
  assert.equal(view.action, "case_b");
  const before = footprint();

  // E2 unreadable-only regression: complete-looking (partial false, no causes) but one bare was unreadable.
  const real = w.git.inspectRetainedSources.bind(w.git);
  Object.assign(w.git, { inspectRetainedSources: async (...args: Parameters<typeof real>) => {
    const result = await real(...args);
    const forged = { ...result, unreadable: 1, partial: false } as Record<string, unknown>;
    delete forged.partialCauses;
    return forged;
  } });
  assert.equal((await describe()).classification, "source_evidence_incomplete");
  assert.deepEqual(await resolve(view.digest!), { kind: "refused", reason: "source_evidence_incomplete" });
  assert.deepEqual(footprint(), before, "every record, bundle, source and ref byte unchanged");
  Object.assign(w.git, { inspectRetainedSources: real });
  assert.equal((await describe()).digest, view.digest, "the genuine evidence is eligible again, with the same digest");

  // A clone replaced at the same path and HEAD (a new dev:ino).
  const parked = w.episode.sourcePath + ".parked";
  await fs.cp(w.episode.sourcePath, parked, { recursive: true, preserveTimestamps: true });
  await fs.rm(w.episode.sourcePath, { recursive: true });
  await fs.rename(parked, w.episode.sourcePath);
  const replacedView = await describe();
  assert.equal(replacedView.classification, "case_b_owner_discard");
  assert.notEqual(replacedView.digest, view.digest);
  assert.deepEqual(await resolve(view.digest!), { kind: "refused", reason: "descriptor_changed" });

  // The confirmed, dirty source passes the real boundary, stays byte-identical, and its records go.
  const kept = footprint();
  assert.deepEqual(await resolve(replacedView.digest!), { kind: "retired", generation: 9, remaining: 0 });
  assert.deepEqual(treeDigest(w.episode.sourcePath), kept.source, "the retained source is untouched");
  assert.deepEqual(treeDigest(w.episode.successorPath), kept.successor);
  assert.deepEqual(allRefs(w.bare), kept.refs, "no ref or pin is touched");
  await assert.rejects(fs.stat(path.join(w.dir, "recovery", RUN)), { code: "ENOENT" });
  assert.equal(w.client.posts.length, 0, "nothing was reported to the api");
});

test("real readers: metadata-only drift of the retained source (journal bytes) changes the native descriptor, not the E1 clone tuple, and refuses descriptor_changed", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await createWorld({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: false });
  e2eWorlds.push(w);
  const sig = () => ({ requestSignal: new AbortController().signal, shutdownSignal: new AbortController().signal, deadlineAt: Date.now() + 60_000 });
  const describe = async () => {
    const d = await w.coordinator.describeLocalRecords(RUN, sig());
    assert.equal(d.kind, "described", JSON.stringify(d));
    return d.kind === "described" ? d.generations.find(g => g.generation === 9)! : (undefined as never);
  };
  const footprint = () => ({ source: treeDigest(w.episode.sourcePath), successor: treeDigest(w.episode.successorPath),
    records: treeDigest(path.join(w.dir, "recovery", RUN)), refs: allRefs(w.bare) });
  const e1 = async () => JSON.stringify(await w.git.readInventoryCloneHeads(w.bare, RUN, { cloneIdentity: true }));
  const native = async (): Promise<string[]> => (await w.git.inspectRetainedSources({ isLivePath: () => false })).sources
    .filter(x => x.runId === RUN && x.barePath === w.bare).map(x => x.descriptor).sort();

  const view = await describe();
  assert.equal(view.classification, "case_b_owner_discard");
  const e1Before = await e1();
  const nativeBefore = await native();
  assert.ok(nativeBefore.length >= 1);
  const before = footprint();

  // Same JSON value, different bytes: only the journal text the native descriptor hashes changes.
  const key = `uzi-recovery.${w.episode.branch}.clone`;
  const raw = gitIn(w.bare, ["config", "--get", key]);
  const reformatted = raw.replace(/^\{/, "{ ");
  assert.notEqual(reformatted, raw);
  gitIn(w.bare, ["config", key, reformatted]);

  assert.equal(await e1(), e1Before, "the E1 clone tuple (paths, branches, heads, identities) is unchanged");
  assert.notDeepEqual(await native(), nativeBefore, "the native retained-source descriptor (rec.digest) changed");
  const drifted = await describe();
  assert.equal(drifted.classification, "case_b_owner_discard", "still eligible");
  assert.notEqual(drifted.digest, view.digest, "the native descriptor moved the digest");
  const mid = footprint();
  assert.deepEqual(await w.coordinator.resolveLocalRecord({ runId: RUN, generation: 9, confirm: view.digest!, action: "case_b", ...sig() }),
    { kind: "refused", reason: "descriptor_changed" });
  assert.deepEqual(footprint(), mid, "nothing is deleted");
  assert.deepEqual(mid.source, before.source);
  assert.deepEqual(mid.refs, before.refs);
});
