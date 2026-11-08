// Issue #3644 m4c (N1): an owner Cancel already persisted server-side (the ownership read reports `cancellation_requested`) but not
// yet routed by steering must be reported by a claim that the restore route stops BEFORE the seed writes, exactly as the
// held-predecessor refusal does (runner-held-seam-cancel.test.ts). The cancel is surfaced only through the ownership field, so
// steering's own input poll never routes it. Every cancelled stop writes nothing to P's journal, ledger, slot, clone, HOME.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { Outbox } from "../src/outbox.js";
import { api, installHarness } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import {
  BRANCH, buildRig, cfgSet, cleanupRig, failed, logged, refusals, resetRigRoots, snapshot, armTraps, treeDigest,
  type Rig, type RigOptions,
} from "./restore-route-rig.js";

installHarness();

const outDirs: string[] = [];
afterEach(() => {
  cleanupRig();
  for (const d of outDirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
beforeEach(() => { resetRigRoots(); });

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "restore-cancel-outbox-"));
  outDirs.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  for (let page = 0; page < 8; page++) if ((await outbox.scanTerminalObservationsPage()).passComplete) break;
  assert.equal(outbox.policyInventoryCoverageAvailable(), true, "fixture: the inventory pass completed");
  return outbox;
}

/** Serve `cancellation_requested` from the ownership read; steering's input poll never sees a cancel row. */
function persistCancel(r: Rig, requested: boolean): void {
  api.ownershipWire.set(r.runId, res => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ status: "running", claim_released: false, claim_generation: r.claim.claim_generation ?? 2,
      ...(requested ? { cancellation_requested: true } : {}) }));
  });
}

async function run(r: Rig): Promise<{ before: Awaited<ReturnType<typeof snapshot>>; traps: ReturnType<typeof armTraps> }> {
  const before = await snapshot(r);
  const traps = armTraps(r);
  try { await r.runner.execute(r.claim); } finally { traps.armed = false; }
  return { before, traps };
}

async function assertNothingWritten(r: Rig, before: Awaited<ReturnType<typeof snapshot>>, traps: ReturnType<typeof armTraps>): Promise<void> {
  assert.deepEqual(await snapshot(r), before, "P's journal, ledger, slots, clone, HOME and provision directory are byte-identical");
  assert.deepEqual(traps.hits.slice(0, 5), [], "no fs call named P's path");
  assert.deepEqual(traps.calls, {}, "no reservation, capture, admission, legacy discovery or family match");
  assert.equal(r.counters.ran, 0, "no model turn");
  assert.equal(treeDigest(r.pPath), before.clone, "P's clone untouched");
}

interface Case { name: string; rig?: RigOptions; arrange?: (r: Rig) => void; stop: "refusal" | "composite" | "disabled" }

const cases: Case[] = [
  { name: "a refused restore claim (no pin record)", rig: { pin: false }, stop: "refusal" },
  { name: "a refused restore claim (hosted predecessor)", rig: { pBoundary: "hosted" }, stop: "refusal" },
  { name: "a composite-stop claim (an invalid restore entry)", stop: "composite", arrange: r => { cfgSet(r, `uzi-restore.${BRANCH}.journal`, "not a restore entry"); } },
  { name: "an eligible claim with seeding disabled", rig: { independentRestore: false }, stop: "disabled" },
];

describe("an owner Cancel persisted but not yet routed, at the restore route's pre-seed stops (issue #3644 m4c N1)", () => {
  for (const c of cases) {
    it(`${c.name}: reports cancelled with custody kept and writes nothing`, async () => {
      const r = await buildRig({ ...c.rig, runnerExtra: { outbox: await mkOutbox(), pollMs: 1_000, heldSeamCancelObserveMs: 2_000 } });
      c.arrange?.(r);
      persistCancel(r, true);
      const { before, traps } = await run(r);
      const reports = failed(r);
      assert.equal(reports.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore|retained|cancel/i.test(String((l as { msg: string }).msg)))));
      assert.match(String(reports[0]!.body.failure_reason ?? ""), /cancel/i, "the failed report is the run cancelled one");
      assert.equal(logged(r, "retained predecessor held; owner cancel observed at the refusal, reporting cancellation").length, 1);
      await assertNothingWritten(r, before, traps);
    });

    it(`${c.name}: the uncancelled control still stops nonterminally`, async () => {
      const r = await buildRig({ ...c.rig, runnerExtra: { outbox: await mkOutbox(), pollMs: 1_000, heldSeamCancelObserveMs: 2_000 } });
      c.arrange?.(r);
      persistCancel(r, false);
      const { before, traps } = await run(r);
      assert.equal(failed(r).length, 0, "no failed report");
      assert.equal(logged(r, "retained predecessor held; owner cancel observed at the refusal, reporting cancellation").length, 0);
      if (c.stop === "refusal") assert.equal(refusals(r).length, 1);
      if (c.stop === "composite") assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
      if (c.stop === "disabled") assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1);
      await assertNothingWritten(r, before, traps);
    });
  }
});

