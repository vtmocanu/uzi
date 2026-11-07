import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";
import {
  LIFTED_COMMAND_TIMER_MS, LIFTED_HELPER_DEADLINE_MS, LIFTED_OUTER_TIMEOUT_MS, LIFTED_SCOPE_TIMEOUT_MS, PRODUCTION_OUTER_TIMEOUT_MS,
  emptyPhases, liftPlanningHelperDeadline, phaseEvidenceOf, phaseObserverMismatches, verifyRow,
} from "./planning-deadline-lift.js";

const DEADLINE = "const deadline = Date.now() + 28000;";
const TIMER = "Math.max(0, Math.min(5000, deadline - Date.now()))";
const root = fs.mkdtempSync(path.join(os.tmpdir(), "pbd-lift-"));
after(() => { fs.rmSync(root, { recursive: true, force: true }); });

// The real helper source and outer timeout exactly as capturePlanningDiff hands them to the transport.
async function capturedHelper(): Promise<{ argv: readonly string[]; timeoutMs: number | undefined }> {
  const cache = new GitCache(root, nullLogger());
  let seen: { argv: readonly string[]; timeoutMs: number | undefined } | undefined;
  (cache as unknown as { readBoundedPlanningOutput: (cwd: string, argv: readonly string[], timeoutMs?: number) => Promise<Buffer> })
    .readBoundedPlanningOutput = async (_cwd, argv, timeoutMs) => { seen = { argv, timeoutMs }; return Buffer.alloc(0); };
  await cache.capturePlanningDiff(path.join(root, "runner", "repo", "issue-1"), "a".repeat(40), path.join(root, "repos", "repo.git"));
  assert.ok(seen, "capturePlanningDiff did not reach the transport");
  return seen;
}

describe("liftPlanningHelperDeadline", () => {
  it("lifts exactly the deadline and the per-command timer of the real helper source", async () => {
    const { argv, timeoutMs } = await capturedHelper();
    assert.equal(argv[1], "-e");
    assert.equal(timeoutMs, PRODUCTION_OUTER_TIMEOUT_MS);
    const source = argv[2] as string;
    const lifted = liftPlanningHelperDeadline(source);
    assert.notEqual(lifted, source);
    assert.ok(lifted.includes(`const deadline = Date.now() + ${LIFTED_HELPER_DEADLINE_MS};`));
    assert.ok(lifted.includes(`Math.max(0, Math.min(${LIFTED_COMMAND_TIMER_MS}, deadline - Date.now()))`));
    assert.ok(!lifted.includes(DEADLINE));
    assert.ok(!lifted.includes(TIMER));
    const back = lifted.replace(`+ ${LIFTED_HELPER_DEADLINE_MS};`, "+ 28000;").replace(`Math.min(${LIFTED_COMMAND_TIMER_MS},`, "Math.min(5000,");
    assert.equal(back, source);
  });

  it("keeps every lifted timer inside the 32-bit setTimeout range, ordered outward", () => {
    assert.ok(LIFTED_SCOPE_TIMEOUT_MS > LIFTED_OUTER_TIMEOUT_MS && LIFTED_OUTER_TIMEOUT_MS > LIFTED_HELPER_DEADLINE_MS);
    assert.ok(LIFTED_SCOPE_TIMEOUT_MS < 2 ** 31 - 1);
  });

  it("fails closed on a missing marker, a duplicated marker and a source without the cap token", () => {
    const ok = `${DEADLINE}\n${TIMER}\nbase_discovery_entry_cap`;
    assert.doesNotThrow(() => liftPlanningHelperDeadline(ok));
    assert.throws(() => liftPlanningHelperDeadline(ok.replace(DEADLINE, "")), /deadline marker must occur exactly once, found 0/);
    assert.throws(() => liftPlanningHelperDeadline(ok.replace(TIMER, "")), /per-command timer marker must occur exactly once, found 0/);
    assert.throws(() => liftPlanningHelperDeadline(`${ok}\n${DEADLINE}`), /deadline marker must occur exactly once, found 2/);
    assert.throws(() => liftPlanningHelperDeadline(`${ok}\n${TIMER}`), /per-command timer marker must occur exactly once, found 2/);
    assert.throws(() => liftPlanningHelperDeadline(`${DEADLINE}\n${TIMER}`), /base_discovery_entry_cap/);
  });

  it("refuses a source that already carries the lifted text elsewhere", () => {
    const ok = `${DEADLINE}\n${TIMER}\nbase_discovery_entry_cap\nconst deadline = Date.now() + ${LIFTED_HELPER_DEADLINE_MS};`;
    assert.throws(() => liftPlanningHelperDeadline(ok), /not reversible/);
  });
});

describe("phase evidence", () => {
  const seen = { ...emptyPhases(), sizingSeenMs: 10, sizingGoneMs: 40, sizingPid: 7 };
  it("accepts a completed capture and a refusal observed after the sizing batch", () => {
    assert.equal(phaseEvidenceOf("completed", null, null), "completed");
    assert.equal(phaseEvidenceOf("refused", { ...seen, indexOpenSeenMs: 50 }, 100), "post_sizing_observed");
    assert.equal(phaseEvidenceOf("refused", { ...seen, postSizingSpawnSeenMs: 60 }, 100), "post_sizing_observed");
  });
  it("rejects a refusal with no post-sizing observation or one later than the outcome", () => {
    assert.equal(phaseEvidenceOf("refused", seen, 100), null);
    assert.equal(phaseEvidenceOf("refused", { ...seen, sizingGoneMs: null, indexOpenSeenMs: 50 }, 100), null);
    assert.equal(phaseEvidenceOf("refused", { ...seen, indexOpenSeenMs: 150 }, 100), null);
    assert.equal(phaseEvidenceOf("oom", { ...seen, indexOpenSeenMs: 50 }, 100), null);
  });
  it("flags only contradictions with the preamble, never a missed observation", () => {
    const helper = { t0: 1000, sizingBytes: 5, sizingCloseMs: 40, boundaryMs: 60 };
    assert.deepEqual(phaseObserverMismatches(emptyPhases(), 1000, helper), []);
    assert.deepEqual(phaseObserverMismatches({ ...seen, indexOpenSeenMs: 62 }, 1000, helper), []);
    const late = { t0: 1000, sizingBytes: 5, sizingCloseMs: 200, boundaryMs: 220 };
    assert.match(phaseObserverMismatches({ ...seen, indexOpenSeenMs: 10 }, 1000, late)[0] ?? "", /^PHASE_OBSERVER_MISMATCH/);
    assert.match(phaseObserverMismatches({ ...seen, sizingGoneMs: 200 }, 1000, helper)[0] ?? "", /^PHASE_OBSERVER_MISMATCH/);
    assert.match(phaseObserverMismatches(seen, 1000, { t0: 1000, sizingBytes: 0, sizingCloseMs: null, boundaryMs: 60 })[0] ?? "", /^PHASE_OBSERVER_MISMATCH/);
  });
});

describe("verifyRow", () => {
  const liftLabel = "distinct-1170x1024n76__full__4g-2cpu__validation-lift";
  const plainLabel = "chain-d64__full__4g-2cpu";
  const lift = { helperDeadlineMs: LIFTED_HELPER_DEADLINE_MS, commandTimerMs: LIFTED_COMMAND_TIMER_MS,
    outerTimeoutMs: LIFTED_OUTER_TIMEOUT_MS, scopeTimeoutMs: LIFTED_SCOPE_TIMEOUT_MS };
  const term = (label: string, over: Record<string, unknown> = {}): Record<string, unknown> =>
    ({ label, exitCode: "0", oomKilled: false, killedByHarness: false, ...over });
  const result = (label: string, over: Record<string, unknown> = {}): Record<string, unknown> => ({
    label, mode: "full", validation: label.includes("__validation-lift"), failed: false, failures: [],
    deadlineLift: label.includes("__validation-lift") ? lift : null, phaseEvidence: null,
    outcome: { kind: "completed" }, derived: null, treeEntriesFetched: { value: 5 }, ...over,
  });
  const capResult = (label: string, token: string, over: Record<string, unknown> = {}): Record<string, unknown> =>
    result(label, { outcome: { kind: "refused", token }, derived: { charge: { entries: "1", pathBytes: "2" }, reason: null }, ...over });

  it("accepts a clean record of each expectation kind", () => {
    assert.deepEqual(verifyRow(result(liftLabel), term(liftLabel), liftLabel, "completed"), { valid: true, reasons: [] });
    const pe = result(liftLabel, { phaseEvidence: "post_sizing_observed", outcome: { kind: "refused", token: "capture_deadline" } });
    assert.equal(verifyRow(pe, term(liftLabel), liftLabel, "phase-evidence").valid, true);
    const capped = capResult(plainLabel, "base_discovery_entry_cap");
    assert.equal(verifyRow(capped, term(plainLabel), plainLabel, "base_discovery_entry_cap").valid, true);
  });

  it("rejects a mismatch flag, missing phase evidence, a dirty container exit and a missing term", () => {
    const flagged = result(liftLabel, { failed: true, failures: ["PHASE_OBSERVER_MISMATCH: x"] });
    const v = verifyRow(flagged, term(liftLabel), liftLabel, "completed");
    assert.equal(v.valid, false);
    assert.ok(v.reasons.includes("PHASE_OBSERVER_MISMATCH"));
    const none = verifyRow(result(liftLabel), term(liftLabel), liftLabel, "phase-evidence");
    assert.equal(none.valid, false);
    assert.ok(none.reasons.some((r) => r.startsWith("PHASE_UNPROVEN")));
    assert.equal(verifyRow(result(liftLabel), term(liftLabel, { exitCode: "1" }), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel), term(liftLabel, { oomKilled: true }), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel), term(liftLabel, { killedByHarness: true }), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel), undefined, liftLabel, "completed").valid, false);
    assert.equal(verifyRow(undefined, term(liftLabel), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel), term("other"), liftLabel, "completed").valid, false);
  });

  it("rejects a lift row without its deadlineLift record or with other values", () => {
    assert.equal(verifyRow(result(liftLabel, { deadlineLift: null }), term(liftLabel), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel, { deadlineLift: { ...lift, scopeTimeoutMs: 60_000 } }), term(liftLabel), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(liftLabel, { validation: false }), term(liftLabel), liftLabel, "completed").valid, false);
    assert.equal(verifyRow(result(plainLabel, { deadlineLift: lift }), term(plainLabel), plainLabel, "completed").valid, false);
  });

  it("rejects the wrong refusal token and a cap refusal without a derived charge", () => {
    const wrong = result(plainLabel, { outcome: { kind: "refused", token: "capture_deadline" }, derived: { charge: { entries: "1" } } });
    assert.equal(verifyRow(wrong, term(plainLabel), plainLabel, "base_discovery_entry_cap").valid, false);
    const noCharge = capResult(plainLabel, "base_discovery_path_byte_cap", { derived: { charge: null, reason: "x" } });
    assert.equal(verifyRow(noCharge, term(plainLabel), plainLabel, "base_discovery_path_byte_cap").valid, false);
    const noMeter = capResult(plainLabel, "base_discovery_entry_cap", { treeEntriesFetched: { value: null } });
    assert.equal(verifyRow(noMeter, term(plainLabel), plainLabel, "base_discovery_entry_cap").valid, false);
    const control = capResult(plainLabel, "base_discovery_entry_cap", { mode: "control", treeEntriesFetched: null });
    assert.equal(verifyRow(control, term(plainLabel), plainLabel, "base_discovery_entry_cap").valid, true);
  });
});
