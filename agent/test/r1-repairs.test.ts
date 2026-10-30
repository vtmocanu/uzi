import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const policy: PolicyHoldRequest = {
  outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
  source_generation: 1, claim_generation: 1, detail: "refused",
  continuation_mode: "same_conversation", published: false,
  tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1",
};
async function fixture(t: { after(fn: () => Promise<void>): void }, bytes = 1e6) {
  const dir = makeScratchRoot("r1-repairs-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const make = () => new Outbox({ root, log: nullLogger(), maxBytes: bytes, runMaxBytes: bytes, retentionMs: 1 });
  const outbox = make();
  await outbox.init();
  return { root, make, outbox };
}
function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
// Instrument I/O seams, exercising only public lifecycle operations/assertions.
type IOSeams = {
  fsyncDir(dir: string, strict?: boolean): Promise<void>;
  hasPhysicalTerminalProtection(runId: string): Promise<boolean>;
};

it("R1 A1: first spill publication retains a selection made after root fsync", async t => {
  const { outbox, root } = await fixture(t);
  const token = outbox.reservePolicyFlight(run, 1)!;
  const entered = latch(), resume = latch();
  const seam = outbox as unknown as IOSeams;
  const original = seam.fsyncDir.bind(outbox);
  let once = true;
  seam.fsyncDir = async (dir, strict) => {
    await original(dir, strict);
    if (dir === root && once) { once = false; entered.resolve(); await resume.promise; }
  };
  const spill = outbox.markSpillUnclean(run);
  try {
    await entered.promise;
    assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  } finally { resume.resolve(); await spill; }
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.deepEqual(await outbox.journalTerminal(run, 2, "running", 0, { status: "failed" }),
    { journaled: false, reason: "winner_unavailable" });
  assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
});

it("R1 A2: reservation prevents deletion and selected memory survives missing files", async t => {
  const { outbox, root } = await fixture(t);
  await outbox.markSpillUnclean(run);
  const token = outbox.reservePolicyFlight(run, 1)!;
  await outbox.retireRun(run);
  assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  await fs.rm(path.join(root, run), { recursive: true, force: true });
  await outbox.retireRun(run);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
});

it("R1 A2: deletion proof fences new reservations through actual rm settlement and permits late reuse", async t => {
  const { outbox, root } = await fixture(t);
  await outbox.markSpillUnclean(run);
  const proofEntered = latch(), proofResume = latch();
  const rmEntered = latch(), rmResume = latch();
  const seam = outbox as unknown as IOSeams;
  const originalProof = seam.hasPhysicalTerminalProtection.bind(outbox);
  seam.hasPhysicalTerminalProtection = async id => {
    const protectedSource = await originalProof(id);
    assert.equal(protectedSource, false);
    proofEntered.resolve();
    await proofResume.promise;
    return protectedSource;
  };
  const originalRm = fs.rm.bind(fs);
  const rmMock = t.mock.method(fs, "rm", async (...args: Parameters<typeof fs.rm>) => {
    await originalRm(...args);
    if (args[0] === path.join(root, run)) { rmEntered.resolve(); await rmResume.promise; }
  });
  const retiring = outbox.retireRun(run);
  let initializing: Promise<void> | undefined;
  try {
    await proofEntered.promise;
    assert.equal(outbox.reservePolicyFlight(run, 1), undefined);
    assert.equal(outbox.reservePolicyFlight(run.toUpperCase(), 1), undefined);
    proofResume.resolve();
    await rmEntered.promise;
    assert.equal(outbox.reservePolicyFlight(run, 1), undefined);
    // Initialization joins the existing run lock, while physical deletion is pending.
    initializing = outbox.markSpillUnclean(run);
    assert.equal(outbox.reservePolicyFlight(run, 1), undefined);
  } finally {
    proofResume.resolve(); rmResume.resolve();
    await retiring; await initializing;
    rmMock.mock.restore();
  }
  const replacement = outbox.reservePolicyFlight(run, 1);
  assert.ok(replacement);
  assert.equal(outbox.selectPolicyFlight(replacement, policy.outcome_id), true);
  assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
  assert.equal((await outbox.readTerminalJournal(run, 1))?.body.detail, policy.detail);
});

it("R1 selected capture gap never exposes a competing journal before its payload is owned", async t => {
  const { outbox, make } = await fixture(t);
  const writer = make();
  await writer.init();
  const token = outbox.reservePolicyFlight(run, 1)!;
  assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  assert.equal((await writer.journalPolicyHold(run, policy)).journaled, true);
  assert.equal(await outbox.readTerminalJournal(run, 1), undefined);
  assert.equal(await outbox.readTerminalJournalForRetirement(run, 1), undefined);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
  assert.deepEqual((await outbox.readTerminalJournalForRetirement(run, 1))?.journal.body, policy);
});

it("R1 B2: authenticated same-ID different-body collision cannot replace selected original", async t => {
  const { outbox, make, root } = await fixture(t);
  const competing = make();
  await competing.init();
  const token = outbox.reservePolicyFlight(run, 1)!;
  assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  assert.equal((await competing.journalPolicyHold(run, { ...policy, tag: "misalignmentPolicyViolation" })).journaled, true);
  assert.deepEqual(await outbox.journalPolicyHold(run, policy), { journaled: false, reason: "reserve_exhausted" });
  assert.equal(await outbox.readTerminalJournal(run, 1), undefined, "competing policy must never be sendable");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  // File disappearance grants no retirement. Retrying after removal preserves the original body/id.
  await fs.unlink(path.join(root, run, "terminal-1.json"));
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "policy_hold");
  assert.equal((await outbox.journalTerminal(run, 1, "planning", 17,
    { ...policy, tag: "misalignmentPolicyViolation", phase: "planning" }, "policy_hold")).journaled, true);
  const retained = await outbox.readTerminalJournal(run, 1);
  assert.deepEqual(retained?.body, policy);
  assert.equal(retained?.phase, policy.phase);
  assert.equal(retained?.messagesThroughSeq, 0);
});

it("R1 B1: actual oversized source remains unknown and cannot become ordinary", async t => {
  const { outbox, root, make } = await fixture(t, 1024);
  assert.equal((await outbox.journalTerminal(run, 1, "running", 0, { status: "failed" })).journaled, true);
  await fs.writeFile(path.join(root, run, "terminal-1.json"), " ".repeat(1025));
  const restarted = make();
  await restarted.init();
  const page = await restarted.scanTerminalObservationsPage();
  assert.ok(page.observations.some(o => o.kind === "oversized"));
  assert.equal(await restarted.readTerminalJournal(run, 1), undefined);
  assert.equal(restarted.listPendingTerminals()[0]?.outcome_kind, "unknown");
  assert.equal(await restarted.hasPendingPolicyOutcome(run), true);
  await restarted.closeTerminalObservationScan();
});

for (const kind of ["policy", "unknown"] as const) for (const policyListedFirst of [false, true]) {
  it(`R1 T1: real case aliases preserve ${kind} evidence, policy listed first=${policyListedFirst}`, async t => {
    const { outbox, root } = await fixture(t);
    const ordinary = () => outbox.journalTerminal(run.toUpperCase(), 1, "running", 0, { status: "failed" });
    const protect = async () => {
      assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
      if (kind === "unknown") await fs.writeFile(path.join(root, run, "terminal-1.json"), "invalid");
    };
    assert.equal((await ordinary()).journaled, true);
    await protect();
    // Restart rederives unknown evidence from the physical source.
    const restarted = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
    await restarted.init();
    assert.ok(restarted.listPendingTerminals().some(p => p.outcome_kind === (kind === "policy" ? "policy_hold" : "unknown")));
    const sources = restarted.listPendingTerminals().sort((a, b) =>
      (a.outcome_kind === "terminal" ? 0 : 1) - (b.outcome_kind === "terminal" ? 0 : 1));
    assert.equal(sources.length, 2);
    if (policyListedFirst) sources.reverse();
    const registry = new ActiveRunRegistry(() => sources, () => 1);
    assert.equal(registry.claimsPausedByPendingOverflow(), true);
    assert.equal(registry.build().pending_overflow, true);
    assert.deepEqual(registry.build().pending_all, [{ run_id: run, claim_generation: 1 }]);
    assert.equal(registry.claimsPausedByPendingOverflow(true), false);
    assert.equal(registry.notePolicyOwnership(run, 3), true);
    const scoped = registry.build(true);
    assert.deepEqual(scoped.pending_policy_protection, [{ run_id: run, owner_generation: 3 }]);
    assert.deepEqual(scoped.pending_all, [{ run_id: run, claim_generation: 1 }]);
    assert.deepEqual(scoped.active, kind === "policy" ? [
      { run_id: run, claim_generation: 3, phase: "running", terminal_pending: true },
    ] : []);
  });
}

it("R1 pending_all wire ceiling is 256 distinct tuples, independent of lease cap and run count", () => {
  for (const count of [255, 256, 257]) for (const cap of [0, 1000]) {
    const entries = Array.from({ length: count }, (_, i) => ({
      run_id: run, claim_generation: i + 1, phase: "running", since: 1, blocked: false, outcome_kind: "unknown" as const,
    }));
    const registry = new ActiveRunRegistry(() => [...entries, ...entries.map(p => ({ ...p, run_id: run.toUpperCase() }))], () => cap);
    for (const snapshot of [registry.buildRegisterSnapshot(), registry.build(), registry.build(true)]) {
      assert.equal(snapshot.pending_all?.length, count <= 256 ? count : undefined);
      assert.equal(snapshot.pending_overflow, true);
    }
    assert.equal(registry.claimsPausedByPendingOverflow(true), cap === 0 || count > 256,
      "scoped claims require complete coverage as well as real capacity");
  }
  const incomplete = new ActiveRunRegistry(() => [], () => 1000, () => false);
  assert.equal(incomplete.build().pending_all, undefined);
  assert.equal(incomplete.build().pending_overflow, true);
  assert.equal(incomplete.claimsPausedByPendingOverflow(), true);
  assert.deepEqual(new ActiveRunRegistry(() => [], () => 0).build().pending_all, []);
});

it("R1 full catalog retains frontier, recovers on release, and rejects stale retirement", async t => {
  const { outbox, make } = await fixture(t);
  const writer = make();
  await writer.init();
  const tokens: symbol[] = [];
  for (let i = 1; i <= 1024; i++) { const token = outbox.reservePolicyFlight(run, i); assert.ok(token); tokens.push(token); }
  assert.equal((await writer.journalPolicyHold(run, policy)).journaled, true);
  const first = outbox.scanTerminalObservationsPage();
  assert.equal(outbox.scanTerminalObservationsPage(), first);
  assert.equal((await first).passComplete, false);
  assert.equal(outbox.policyInventoryCoverageAvailable(), false);
  assert.equal(outbox.reservePolicyFlight(run, 2000), undefined);
  outbox.releasePolicyFlight(tokens.shift()!);
  // At most six bounded pages; no sleeps or unlimited scan-until-complete loop.
  for (let i = 0; i < 6 && !outbox.policyInventoryCoverageAvailable(); i++) await outbox.scanTerminalObservationsPage();
  assert.equal(outbox.policyInventoryCoverageAvailable(), true);
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "policy_hold");
  assert.equal(outbox.reservePolicyFlight(run, 2000), undefined);
  const sent = await outbox.readTerminalJournalForRetirement(run, 1);
  assert.ok(sent);
  assert.equal(await outbox.retireTerminalIfEligible(run, 1, sent.context), true);
  const replacement = outbox.reservePolicyFlight(run, 1);
  assert.ok(replacement);
  assert.equal(outbox.selectPolicyFlight(replacement, policy.outcome_id), true);
  assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
  await outbox.markTerminalServerSettled(run, 1, sent.context);
  assert.equal(outbox.listPendingTerminals()[0]?.server_settled, undefined);
  assert.equal(await outbox.retireTerminalIfEligible(run, 1, sent.context), false);
  for (const token of tokens) outbox.releasePolicyFlight(token);
  await outbox.closeTerminalObservationScan();
});

it("R1 capacity counts ordinary source tuples outside policy groups and preserves ordinary duplicate guard", () => {
  const other = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
  const source = (run_id: string, claim_generation: number, outcome_kind: "terminal" | "unknown") =>
    ({ run_id, claim_generation, outcome_kind, phase: "running", since: 1, blocked: false });
  const pending = [source(run, 1, "unknown"), source(run.toUpperCase(), 1, "terminal"),
    source(other, 1, "terminal"), source(other.toUpperCase(), 1, "terminal"), source(other, 2, "terminal")];
  const registry = new ActiveRunRegistry(() => pending, () => 2);
  assert.equal(registry.claimsPausedByPendingOverflow(true), true, "one policy group plus two ordinary tuples");
  const room = new ActiveRunRegistry(() => pending, () => 3);
  assert.equal(room.claimsPausedByPendingOverflow(true), false);
  assert.equal(room.build().pending_all?.length, 3);
  const ordinary = new ActiveRunRegistry(() => pending.filter(p => p.run_id.toLowerCase() === other), () => 3);
  assert.deepEqual(ordinary.build().active.map(p => p.claim_generation), [1, 2],
    "distinct ordinary generations must retain the legacy duplicate-run guard");
});

it("R1 protection admission bounds fields and compact encoded slots independently of body quotas", async t => {
  const { outbox } = await fixture(t);
  assert.equal(outbox.reservePolicyFlight("r".repeat(129), 1), undefined);
  const boundedRun = "r".repeat(128);
  const token = outbox.reservePolicyFlight(boundedRun, 1);
  assert.ok(token);
  assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  // Both fields individually fit, but JSON escaping pushes the compact slot over 1024 bytes.
  const escapedPhase = String.fromCharCode(1).repeat(128);
  assert.deepEqual(await outbox.journalTerminal(boundedRun, 1, escapedPhase, 0,
    policy as unknown as Record<string, unknown>, "policy_hold"), { journaled: false, reason: "reserve_exhausted" });
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
  assert.deepEqual(await outbox.journalTerminal(boundedRun, 1, "p".repeat(129), 0,
    policy as unknown as Record<string, unknown>, "policy_hold"), { journaled: false, reason: "reserve_exhausted" });
  assert.equal((await outbox.journalTerminal(boundedRun, 1, "p".repeat(128), 0,
    policy as unknown as Record<string, unknown>, "policy_hold")).journaled, true);
  assert.deepEqual((await outbox.readTerminalJournal(boundedRun, 1))?.body, policy);
});

it("R1 ordinary overflow snapshots keep the absolute wire bound when real capacity is larger", () => {
  const entries = Array.from({ length: 270 }, (_, i) => ({
    run_id: (i + 1).toString(16).padStart(8, "0") + "-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    claim_generation: 1, phase: "running", since: i, blocked: false,
  }));
  const registry = new ActiveRunRegistry(() => entries, () => 1000);
  registry.add("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", 1);
  registry.add("ffffffff-ffff-4fff-8fff-ffffffffffff", 1);
  const snapshot = registry.build(true);
  assert.equal(snapshot.active.length, 256);
  assert.equal(snapshot.active.filter(p => p.terminal_pending).length, 254);
  assert.equal(snapshot.pending_all, undefined);
  assert.equal(snapshot.pending_policy_protection, undefined);
  assert.equal(snapshot.pending_overflow, true);
  assert.equal(registry.claimsPausedByPendingOverflow(true), true);
});

it("R1 positive non-ownership preserves the cap-zero floor for retained policy evidence", () => {
  const registry = new ActiveRunRegistry(() => [{
    run_id: run, claim_generation: 1, phase: "running", since: 1, blocked: false,
    outcome_kind: "unknown",
  }], () => 0);
  registry.notePolicyOwnership(run, null);
  assert.equal(registry.claimsPausedByPendingOverflow(), false);
  assert.equal(registry.claimsPausedByPendingOverflow(true), false);
  assert.equal(registry.build().pending_overflow, false);
});

it("R1 catalog reuses LIVE selected slots after more than 1024 authenticated lifetimes", async t => {
  const { outbox } = await fixture(t);
  // A single rejected lifetime fails immediately; siblings are not retried.
  for (let generation = 1; generation <= 1025; generation++) {
    const token = outbox.reservePolicyFlight(run, generation);
    assert.ok(token, `reservation ${generation}`);
    assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
    const body = { ...policy, source_generation: generation, claim_generation: generation };
    assert.equal((await outbox.journalPolicyHold(run, body)).journaled, true);
    const sent = await outbox.readTerminalJournalForRetirement(run, generation);
    assert.ok(sent);
    assert.equal(await outbox.retireTerminalIfEligible(run, generation, sent.context), true);
    assert.equal(outbox.listPendingTerminals().length, 0);
  }
});
