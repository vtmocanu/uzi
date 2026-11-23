// Issue #4627 (U3, A control): the restore route is untouched. A retained claim whose predecessor's authority slot is ACTIVE under
// a dead worker incarnation goes through opaque pre-discovery and the independent-restore route, which never calls
// reserveRecoveryIteration, so a typed bare-identity mismatch on the predecessor's owner record neither holds the run nor reaches
// the reservation. Production RunRunner + real Git + the durable authority through the shared rig of restore-route-rig.ts.
import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { api, git, installHarness } from "./runner-harness.js";
import {
  BRANCH, IID, assertStoppedUntouched, buildRig, cfgSet, cleanupRig, drive, journalOf, logged, refusals, resetRigRoots, rigDefaults, type Rig,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = false;

afterEach(() => { cleanupRig(); });

const GUIDANCE = "storage identity changed";
const guidance = (r: Rig): string[] => api.messages(r.runId).map(m => String((m.payload as { text?: string }).text ?? "")).filter(t => t.includes(GUIDANCE));

/** Reads of the shifted identity after the mismatch is armed; the restore route is expected to make none (asserted below). */
const identityReads = { count: 0 };

/** The predecessor P has an owner record (one charged reservation of its own) and the bare's device number then moves. */
async function withTypedMismatch(r: Rig, plain: boolean): Promise<string[]> {
  identityReads.count = 0;
  const source = { runId: r.runId, clonePath: r.pPath, attemptId: r.pAttempt };
  await git.reserveRecoveryIteration(r.bare, BRANCH, `issue-${IID}`, source, 1, 2, 7, 0);
  if (plain) {
    // Instrument arrangement: the owner record stays, the journal's reservation is dropped, so P is again the plain
    // predecessor the restore route accepts. The route reads neither; this only keeps the typed mismatch armed beside it.
    const { recovery: _recovery, ...rest } = journalOf(r);
    cfgSet(r, r.journalKey, JSON.stringify(rest));
  }
  const seam = git as unknown as Record<string, (...a: unknown[]) => Promise<string>>;
  const identity = JSON.parse(await seam.recoveryOwnerBareIdentity!(r.bare)) as string[];
  const real = seam.recoveryOwnerBareIdentity!.bind(git);
  let armed = false;
  mock.method(seam, "recoveryOwnerBareIdentity", async (...args: unknown[]) => {
    if (!armed) return real(...args);
    identityReads.count++;
    const shifted = [...identity];
    shifted[1] = String(BigInt(identity[1]!) + 1n);
    return JSON.stringify(shifted);
  });
  const calls: string[] = [];
  const reserve = git.reserveRecoveryIteration.bind(git);
  mock.method(git, "reserveRecoveryIteration", async (...args: Parameters<typeof reserve>) => { calls.push("reserveRecoveryIteration"); return reserve(...args); });
  armed = true;
  return calls;
}

describe("a foreign-active predecessor slot with a typed bare-identity mismatch takes the restore route, never the hold", () => {
  beforeEach(() => { resetRigRoots(); });

  for (const plain of [false, true]) {
    it(plain ? "an eligible (plain) predecessor: the route stops at its eligible verdict; no hold, no guidance, no reservation"
      : "a predecessor with its own reservation: the route refuses it as not plain; no hold, no guidance, no reservation", async () => {
      const r = await buildRig();
      const calls = await withTypedMismatch(r, plain);
      const runner = r.runner as unknown as Record<string, (...a: unknown[]) => unknown>;
      let holds = 0;
      if (typeof runner.holdForBareIdentityChange === "function") {
        const hold = runner.holdForBareIdentityChange.bind(r.runner);
        mock.method(runner, "holdForBareIdentityChange", (...args: unknown[]) => { holds++; return hold(...args); });
      }
      const { before, traps } = await drive(r);
      assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 1, "the restore route was taken");
      if (plain) {
        assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1, "P is eligible");
        assert.equal(refusals(r).length, 0);
      } else {
        assert.equal(refusals(r).length, 1);
        assert.equal(refusals(r)[0]!.reason, "predecessor_not_plain");
      }
      assert.equal(holds, 0, "no bare-identity hold");
      assert.equal(logged(r, "retained recovery bare storage identity changed; execution refused").length, 0);
      assert.equal(guidance(r).length, 0, "no guidance line");
      assert.deepEqual(calls, [], "reserveRecoveryIteration was never called");
      // The restore route classifies from the bare config and authority evidence alone, so the owner record's bare identity is
      // never read: the armed (shifted) mismatch is never even observed.
      assert.equal(identityReads.count, 0, "recoveryOwnerBareIdentity was never read while armed");
      assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "recovery_wait").length, 0, "no park request");
      // P's journal, owner record, ledger, slots, clone, HOME and provision directory are byte-identical.
      await assertStoppedUntouched(r, before, traps);
    });
  }
});
