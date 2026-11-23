// Issue #4627 (U3, B): the WHOLE claim exit of a held bare storage identity change, for the Claude and the Codex executor, with a
// confirmed owner hold and an uncertain one (a statusless or lost ACK). A claim that holds before any source action must leave
// everything of the predecessor as it found it: its live attributed process, the clone, HOME (a `.npm/_cacache` blob and a Go work
// leaf), the cache and provision directories, the authority slots and the bare's whole config and refs. Real Git, the production
// RunRunner.execute() and a real planted process; see runner-bare-identity-hold-fixture.ts.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { afterEach, describe, it } from "node:test";
import { reapRunProcesses } from "../src/run-procs.js";
import { installHarness, api, git } from "./runner-harness.js";
import {
  GUIDANCE, alive, buildHold, cleanupHold, plantChild, procfs, type Ack, type Harness, type HoldRig,
} from "./runner-bare-identity-hold-fixture.js";
import { setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { scopedRealView } from "./fake-proc.js";

installHarness();
afterEach(() => cleanupHold());

const feed = (r: HoldRig): string[] => api.messages(r.runId).map(m => String((m.payload as { text?: string }).text ?? ""));
const guidance = (r: HoldRig): string[] => feed(r).filter(t => t.includes(GUIDANCE));
const corrective = (r: HoldRig): string[] => feed(r).filter(t => t.startsWith("The owner hold for the changed storage identity was not confirmed"));
const logged = (r: HoldRig, msg: string) => r.logs.lines.filter(l => (l as { msg: string }).msg === msg) as Array<Record<string, unknown>>;
const failures = (r: HoldRig) => api.states.filter(s => s.runId === r.runId && s.body.status === "failed");
const holds = (r: HoldRig) => r.parks.filter(p => p.recovery_cause === "retained_recovery_adoption_failed");

describe("instrument control", procfs, () => {
  it("the reaping executor kills a planted HOME-attributed child, so a surviving child proves no reap ran", async t => {
    const r = await buildHold(t, { harness: "claude", ack: "confirmed" });
    const second = await plantChild(r.home, r.clonePath);
    setQuiescenceViewForTests(scopedRealView({ pids: [r.child!, second] }));
    assert.equal(alive(second), true);
    const reap = await reapRunProcesses(r.home, undefined, [], {});
    assert.ok(reap.killed.includes(second) || !alive(second), JSON.stringify(reap));
    const end = Date.now() + 5000;
    while (alive(second) && Date.now() < end) await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(alive(second), false, "the real scan reaps a HOME-attributed child");
  });
});

for (const harness of ["claude", "codex"] as Harness[]) {
  for (const ack of ["confirmed", "statusless", "lost"] as Ack[]) {
    describe(`whole claim exit of a held bare identity change: ${harness}, ${ack === "confirmed" ? "confirmed park" : `uncertain park (${ack} ACK)`}`, procfs, () => {
      it("leaves the predecessor's process, clone, HOME, caches, provision dir, slots, journal, owner record and ledger untouched", async t => {
        const r = await buildHold(t, { harness, ack });
        const before = await r.snapshot();
        assert.equal(before.slot, null, "no source use is admitted before the claim");
        await r.executeClaim();
        const after = await r.snapshot();

        assert.equal(r.counts.holds, 1, "the hold ran once");
        assert.equal(r.flights[0]!.bareIdentityHold, true);
        // parkRetainedRecoveryExhausted sends the park once on a confirming ACK and retries it once on an uncertain one.
        assert.equal(holds(r).length, ack === "confirmed" ? 1 : 2, JSON.stringify(r.parks));
        assert.equal(r.flights[0]!.parked === true, ack === "confirmed", "parked only on a confirming ACK");
        assert.equal(r.flights[0]!.preserveRecoveryClone, true);
        assert.equal(r.flights[0]!.retainedLocalCustody, true);
        assert.equal(r.flights[0]!.preserveSession, true);
        assert.equal(guidance(r).length, 1, "one guidance line");
        assert.equal(corrective(r).length, ack === "confirmed" ? 0 : 1,
          "exactly one corrective 'not confirmed' line on an uncertain park, none on a confirmed one");
        assert.equal(r.registry.has(r.runId), false, "the flight left the active snapshot");

        assert.equal(alive(r.child!), true, "the predecessor-attributed child is alive");
        assert.equal(r.counts.reaps, 0, "no HOME-attributed reap was requested");
        assert.equal(r.counts.models, 0, "no model turn");
        assert.equal(r.counts.ledger, 0, "no attempt ledger append");
        assert.equal(r.counts.captures, 0, "no capture");
        assert.equal(r.counts.blocks, 0, "no blocker recorded");
        assert.equal(r.counts.freeCaptures, 0, "no credential-free capture");
        assert.equal(r.counts.reclaims, 0, "no cache reclaim");
        for (const p of holds(r)) assert.equal(p.claim_generation, r.claim.claim_generation, "every park (retries included) carries the claim's generation");
        assert.equal(failures(r).length, 0, "no failed report");
        assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0);
        assert.deepEqual(r.quiesce.filter(q => q.targetPaths.some(p => p === r.clonePath || p.startsWith(r.clonePath + path.sep))), [],
          "no quiescence request named the clone");

        assert.equal(after.clone, before.clone, "the clone is byte-identical");
        assert.equal(after.home, before.home, "HOME is byte-identical");
        assert.equal(after.caches, before.caches, "the caches and the Go work leaf are byte-identical");
        assert.equal(after.provision, before.provision, "the provision dir is byte-identical");
        assert.equal(after.skills, before.skills, "the skills plugin dir is byte-identical");
        assert.deepEqual(after.slot, before.slot, "the clone's authority slot did not move");
        assert.deepEqual(after.bareSlot, before.bareSlot, "the bare's authority slot did not move");
        assert.equal(after.config, before.config, "the bare's whole config is byte-identical");
        assert.equal(after.journal, before.journal, "the recovery journal is byte-identical");
        assert.equal(after.owner, before.owner, "the owner record is byte-identical");
        assert.equal(after.attemptMode, before.attemptMode, "the attempt mode is byte-identical");
        assert.equal(after.attempts, before.attempts, "uzi-attempts.* is byte-identical");
        assert.equal(after.refs, before.refs, "the ref dump is byte-identical");
        assert.equal(after.owed, before.owed, "the uzi-owed files are byte-identical");
        assert.equal(after.refusals, before.refusals, "the refusal files are byte-identical");
        assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
        assert.equal(fs.existsSync(path.join(r.home, ".npm", "_cacache", "content-v2", "sha512", "ab", "blob")), true);
        assert.equal(fs.existsSync(path.join(r.home, ".uzi-go-work", "g3", "work")), true);

        if (ack === "confirmed") {
          assert.equal(logged(r, "retained recovery bare identity change held").length, 1);
          assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 0);
        } else {
          assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
          assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 1);
        }
        if (harness === "codex") {
          assert.equal(r.codex!.processSpawns(), 0, "no Codex boundary process was started");
          assert.equal(r.codex!.refreshCalls() + r.codex!.releaseCalls(), 0, "no credential reconcile");
        }
        void git;
      });
    });
  }
}

for (const harness of ["claude", "codex"] as Harness[]) {
  describe(`a permanent refusal of the hold's guidance flush trips the batcher breaker without failing the run: ${harness}, confirmed park`, procfs, () => {
    it("reports no failed state, reaps nothing and leaves the predecessor untouched", async t => {
      const r = await buildHold(t, { harness, ack: "confirmed", failGuidanceFlush: true });
      const before = await r.snapshot();
      await r.executeClaim();
      const after = await r.snapshot();

      assert.equal(r.counts.holds, 1, "the hold ran once");
      assert.equal(failures(r).length, 0, "no failed report");
      assert.ok(logged(r, "guidance_unconfirmed").length >= 1, "the unconfirmed guidance flush was logged (the breaker tripped)");
      assert.equal(holds(r).length, 1, "the park was requested once");
      assert.equal(r.counts.reaps, 0, "no HOME-attributed reap was requested");
      assert.equal(r.counts.reclaims, 0, "no cache reclaim");
      assert.equal(alive(r.child!), true, "the predecessor-attributed child is alive");
      const flight = r.flights[0]!;
      assert.equal(flight.bareIdentityHold, true);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.retainedLocalCustody, true);
      assert.equal(after.clone, before.clone, "the clone is byte-identical");
      assert.equal(after.home, before.home, "HOME is byte-identical");
      assert.equal(after.caches, before.caches, "the caches and the Go work leaf are byte-identical");
    });
  });
}
