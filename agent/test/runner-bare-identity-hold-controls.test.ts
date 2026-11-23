// Issue #4627 (U3, A control): a retained claim whose journal is NOT blocked but would need the legacy attribution repair (an
// `adopting` successor journaled before its ledger attribution existed), combined with a typed bare-identity mismatch, is neither
// held nor repaired. The read-only discovery refuses it through the existing discovery-failure path with the operator text
// "corrupt journal or unsafe local recovery storage", local work and custody kept, no ledger append, no reservation and no hold
// (the plan declares this edge as not held; it fails today too). Real Git, the production RunRunner.execute().
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { afterEach, describe, it } from "node:test";
import { api, git, installHarness } from "./runner-harness.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { alive, buildHold, cleanupHold, gitIn, GUIDANCE, procfs, type HoldRig } from "./runner-bare-identity-hold-fixture.js";

installHarness();
afterEach(() => cleanupHold());

const aid = (n: number): string => `20261008T171401Z-g${n}-0123456789abcdef`;
const OPERATOR_TEXT = "Retained recovery blocked: corrupt journal or unsafe local recovery storage; persistence unavailable; local work and custody retained";
const ledger = (r: HoldRig): string => { try { return gitIn(r.bare, "config", "--get-all", `uzi-attempts.${r.branch}.entry`); } catch { return ""; } };

/** A legacy adopting journal: the successor was journaled before its ledger attribution existed (as git-discovery-bare-identity.test.ts). */
async function legacyAdopting(r: HoldRig): Promise<void> {
  const source = { runId: r.runId, clonePath: r.clonePath };
  await git.reserveRecoveryIteration(r.bare, r.branch, r.key, source, 1000, 1, 7, 0);
  fs.writeFileSync(path.join(r.clonePath, "work.txt"), "retained work\n");
  gitIn(r.clonePath, "add", "work.txt");
  gitIn(r.clonePath, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "milestone");
  await fixtureFetchTracking(git, r.bare, r.clonePath, r.branch, r.runId);
  const tip = gitIn(r.clonePath, "rev-parse", "HEAD");
  await git.recordRecoveryCapture(r.bare, r.branch, r.key, source, 1, tip);
  const journal = JSON.parse(gitIn(r.bare, "config", `uzi-recovery.${r.branch}.clone`)) as { recovery: Record<string, unknown> };
  const successor = { runId: r.runId, clonePath: r.clonePath + ".attempt-" + aid(2), attemptId: aid(2), restoreTip: tip };
  gitIn(r.bare, "config", `uzi-recovery.${r.branch}.clone`,
    JSON.stringify({ ...journal, recovery: { ...journal.recovery, successor, stage: "adopting" } }));
}

describe("a not-blocked adopting journal that needs the legacy repair, with a typed bare-identity mismatch", procfs, () => {
  it("is refused read-only by discovery: the existing operator text, custody kept, no ledger append, no reservation, no hold", async t => {
    const r = await buildHold(t, { harness: "claude", ack: "confirmed", skipReserve: true, tracked: legacyAdopting });
    const journal = JSON.parse(gitIn(r.bare, "config", `uzi-recovery.${r.branch}.clone`)) as { recovery: { stage: string } };
    assert.equal(journal.recovery.stage, "adopting", "the fixture's journal is the not-blocked adopting one");
    const before = await r.snapshot();
    const ledgerBefore = ledger(r);
    await r.executeClaim();
    const after = await r.snapshot();

    assert.equal(r.counts.holds, 0, "no bare-identity hold");
    assert.equal(r.counts.reserves, 0, "the claim never reached a reservation");
    assert.equal(r.parks.length, 0, "no park was requested");
    assert.equal(api.messages(r.runId).filter(m => String((m.payload as { text?: string }).text ?? "").includes(GUIDANCE)).length, 0, "no guidance line");
    assert.equal(r.counts.ledger, 0, "no attempt ledger append through the runner");
    assert.equal(ledger(r), ledgerBefore, "uzi-attempts.* is byte-identical");
    assert.equal(after.config, before.config, "the bare's whole config is byte-identical");
    assert.equal(after.journal, before.journal, "the journal is byte-identical");
    assert.equal(after.owner, before.owner, "the owner record is byte-identical");
    assert.equal(after.refs, before.refs);

    const failed = api.states.filter(s => s.runId === r.runId && s.body.status === "failed");
    assert.equal(failed.length, 1, "the existing discovery-failure path reports one failure");
    assert.equal(String((failed[0]!.body as { reason?: string }).reason ?? (failed[0]!.body as { failure_reason?: string }).failure_reason), OPERATOR_TEXT);

    assert.equal(fs.existsSync(r.clonePath), true, "the predecessor clone is kept");
    assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
    assert.equal(after.clone, before.clone, "the clone is byte-identical");
    assert.equal(after.home, before.home, "HOME is byte-identical");
    assert.equal(after.provision, before.provision);
    assert.equal(alive(r.child!), true, "the predecessor-attributed child is alive");
    assert.equal(r.counts.reaps, 0);
  });
});
