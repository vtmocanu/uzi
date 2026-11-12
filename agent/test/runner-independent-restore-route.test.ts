// Issue #3644 Gate B (m4a): the independent-restore route of a retained claim whose predecessor's authority slot is still
// active under a dead worker incarnation. Production RunRunner + real Git + the durable authority, driven through
// RunRunner.execute(). The predecessor P is a real attempt clone with its journal, ledger entry, pin record and an ACTIVE slot
// owned by another incarnation; the successor S is the runner's own AuthorityRuntime with a different incarnation. The rig is
// shared with the m4b seed tests (restore-route-rig.ts).
//
// This file runs with `independentRestore` OFF: every refusal, and the eligible verdict, ends in a nonterminal stop that writes
// nothing to P (journal, ledger, slot, clone, HOME, provision directory) and never reaches the reservation, the capture, or the
// admission. runner-independent-restore-seed.test.ts covers the same eligible verdict with the restore ON.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { randomUUID } from "node:crypto";
import { CacheTargetBinding } from "../src/cache-target.js";
import type { GovernedRun } from "../src/cache-cap.js";
import type { Executor } from "../src/executor.js";
import type { RecoveryRecord } from "../src/recovery.js";
import type { RunCacheDropResult } from "../src/run-caches.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { api, git, installHarness } from "./runner-harness.js";
import {
  BRANCH, IID, armTraps, assertStoppedUntouched, buildRig, cfgAdd, cfgGet, cfgSet, cleanupRig, drive, failed, gitIn, journalOf,
  logged, olderLedgerEntry, ownLedger, peekOnceFrom, refusals, resetRigRoots, rigDefaults, setJournal, slotOf, snapshot, treeDigest,
  type RigOptions, type Rig,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = false;

afterEach(() => { cleanupRig(); });

// ---- tests ------------------------------------------------------------------------------------------------------------------

describe("the independent restore route (issue #3644 m4a)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("an eligible foreign-active predecessor stops nonterminally, writes nothing, and says seeding is not enabled", async () => {
    const r = await buildRig();
    const { before, traps } = await drive(r);
    const eligible = logged(r, "independent restore eligible; seeding disabled for this runner instance");
    assert.equal(eligible.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore/i.test(String((l as { msg: string }).msg)))));
    const verdict = eligible[0]!.verdict as { predecessorIncarnation: string; boot: string };
    assert.equal(verdict.predecessorIncarnation, r.pInc);
    assert.equal(verdict.boot, "same_boot");
    assert.equal(eligible[0]!.pin_generation, 1);
    assert.equal(eligible[0]!.source_sha, r.pinSha);
    assert.equal(refusals(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("scope control: a foreign-active slot of an unrelated bare does not block", async () => {
    const r = await buildRig();
    const other = path.join(r.dataDir, "runner", "some-other-repo", "issue-5");
    fs.mkdirSync(other, { recursive: true });
    await r.pRt.withTransaction(() => r.pRt.hooks.admitSourceUse(other, "seed-canonical"));
    const { before, traps } = await drive(r);
    assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1);
    assert.equal(refusals(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("without active_run_snapshot the legacy path is unchanged: the admission refuses and the run fails preservation_failed", async () => {
    const r = await buildRig({ features: ["claim_generation_fence"] });
    const spies = { reserve: 0, discover: 0 };
    const reserve = git.reserveRecoveryIteration.bind(git);
    git.reserveRecoveryIteration = ((...args: Parameters<typeof reserve>) => { spies.reserve++; return reserve(...args); }) as typeof git.reserveRecoveryIteration;
    const discover = git.discoverRetainedRecovery.bind(git);
    git.discoverRetainedRecovery = ((...args: Parameters<typeof discover>) => { spies.discover++; return discover(...args); }) as typeof git.discoverRetainedRecovery;
    await r.runner.execute(r.claim);
    assert.ok(spies.discover >= 1, "legacy discovery ran");
    assert.ok(spies.reserve >= 1, "the legacy path reserved an iteration");
    assert.equal(logged(r, "retained predecessor refused by source-use admission; capture not started").length, 1);
    assert.equal(failed(r).length, 1, "today's outcome: the run is failed");
    assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 0);
    assert.equal(refusals(r).length, 0);
  });

  it("a claim generation of 1 never takes the restore route", async () => {
    const r = await buildRig({ claim: { claim_generation: 1 } });
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 0);
    assert.equal(refusals(r).length, 0);
    assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
  });

  describe("every refusal stops nonterminally with no effects and logs its reason", () => {
    const cases: Array<{ name: string; reason: string; rig?: RigOptions; arrange?: (r: Rig) => Promise<void> | void }> = [
      { name: "no pin record", reason: "no_eligible_pin", rig: { pin: false } },
      { name: "a pin of another generation", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => { await r.pin({ generation: 2 }); } },
      { name: "a pin of another branch", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => { await r.pin({ branch: "agent/issue-9999" }); } },
      { name: "a coverage-only record", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, coverageDigest: "a".repeat(64) });
      } },
      { name: "a record of another version", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, version: 2 as never });
      } },
      { name: "a pin that already carries a final request", reason: "no_eligible_pin", rig: { pin: false }, arrange: async r => {
        const rec = (await r.pin())!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, finalRequest: { disposition: "published" } as never });
      } },
      { name: "two ordinary pin records of the generation", reason: "pin_ambiguous", arrange: async r => {
        const rec = (await r.recovery().inspect(r.runId))[0]!;
        const rc = r.recovery() as unknown as { writeRecordUnlocked(rec: RecoveryRecord): Promise<void> };
        await rc.writeRecordUnlocked({ ...rec, captureId: randomUUID() });
      } },
      { name: "a finalization pin taken in another bare", reason: "pin_bare_mismatch", rig: { pin: false }, arrange: async r => {
        await r.pin({ finalizationPin: true, bareDir: "another-bare.git", defaultBranch: "main" });
      } },
      { name: "a pin that is not bound in the bare", reason: "pin_unbound", rig: { pin: false }, arrange: async r => {
        const tree = gitIn(r.bare, ["rev-parse", `${r.pinSha}^{tree}`]);
        const orphan = gitIn(r.bare, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", "-p", r.pinSha, "-m", "unbound", tree]);
        await r.pin({ sourceSha: orphan });
      } },
      { name: "a checkpoint tip that is not an ancestor of the pin", reason: "checkpoint_not_ancestor", arrange: r => {
        const tree = gitIn(r.bare, ["rev-parse", `${r.pinSha}^{tree}`]);
        const side = gitIn(r.bare, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", "-p", r.pinSha, "-m", "side", tree]);
        (r.claim as { checkpoint_tip?: string }).checkpoint_tip = side;
      } },
      { name: "a pending footprint on the predecessor's slot", reason: "predecessor_pending", arrange: async r => {
        await r.pRt.withTransaction(() => r.pRt.hooks.beginDestructive([r.pPath], "unsettled-destruction"));
      } },
      { name: "a completion source fence on the journal", reason: "uncertain_publication", arrange: r => {
        setJournal(r, { completionSourceFence: { version: 1, sourceUseEpoch: randomUUID(), sourceCoreDigest: "b".repeat(64) } });
      } },
      { name: "an episode on the journal", reason: "predecessor_not_plain", arrange: r => {
        setJournal(r, { recovery: { version: 1, source: { runId: r.runId, clonePath: r.pPath, attemptId: r.pAttempt }, attempts: 1,
          startedAt: 1000, deadline: 301_000, claimGeneration: 1, backoffMs: 0, stage: "capturing" } });
      } },
      { name: "a restore tip on the journal", reason: "predecessor_not_plain", arrange: r => { setJournal(r, { restoreTip: "c".repeat(40) }); } },
      { name: "a protected descriptor of the run", reason: "conflicting_attribution", arrange: r => {
        cfgSet(r, `uzi-retained.${r.runId}.journal`, JSON.stringify({ version: 1, branch: BRANCH, key: `issue-${IID}`, journal: journalOf(r) }));
      } },
      { name: "a stopped marker of the run", reason: "conflicting_attribution", arrange: r => { cfgAdd(r, `uzi-stopped.${r.runId}.entry`, "{}"); } },
      { name: "another live ledger entry of the run", reason: "conflicting_attribution", arrange: r => {
        const id = mintAttemptId(2);
        cfgAdd(r, r.ledgerKey, JSON.stringify({ attemptId: id, runId: r.runId, clonePath: `${r.pPath.replace(/\.attempt-.*$/, "")}.attempt-${id}`, state: "live" }));
      } },
      { name: "another key naming the predecessor's path", reason: "conflicting_attribution", arrange: r => {
        cfgSet(r, "uzi-recovery.agent/issue-9998.clone", JSON.stringify({ runId: randomUUID(), clonePath: r.pPath }));
      } },
      { name: "a running pending record on the bare", reason: "bare_busy", arrange: async r => {
        await r.pRt.withTransaction(() => r.pRt.hooks.beginBareMutation(r.bare, "unsettled-ref-deletion", ["refs/uzi-test/x"]));
      } },
      { name: "another run's foreign-active source in the same bare", reason: "census_foreign_active", arrange: async r => {
        const otherRun = randomUUID();
        const id = mintAttemptId(1);
        const other = path.join(path.dirname(r.pPath), `issue-77.attempt-${id}`);
        fs.mkdirSync(other, { recursive: true });
        await r.pRt.withTransaction(() => r.pRt.hooks.admitSourceUse(other, "seed-attempt"));
        cfgAdd(r, "uzi-attempts.agent/issue-77.entry", JSON.stringify({ attemptId: id, runId: otherRun, clonePath: other, state: "live" }));
      } },
      { name: "P's own ledger entry retired", reason: "predecessor_ledger", arrange: r => { cfgSet(r, r.ledgerKey, JSON.stringify({ ...ownLedger(r), state: "retired" })); } },
      { name: "P's own ledger entry reclaimed", reason: "predecessor_ledger", arrange: r => { cfgSet(r, r.ledgerKey, JSON.stringify({ ...ownLedger(r), state: "reclaimed" })); } },
      { name: "an older-generation live ledger entry of the run", reason: "conflicting_attribution", arrange: r => { cfgAdd(r, r.ledgerKey, olderLedgerEntry(r, "live")); } },
      { name: "an incomplete attribution config read", reason: "attribution_unverifiable", arrange: () => {
        mock.method(git, "readRestoreAttribution", async () => ({ entries: [], complete: false }));
      } },
      { name: "a slot that changed owner between classification and evaluation", reason: "predecessor_changed", arrange: r => {
        peekOnceFrom(r, "evaluateRestoreRoute", () => null);
      } },
      { name: "a slot peek that throws during classification (a slot one write ahead of its head)", reason: "slot_unreadable", arrange: r => {
        peekOnceFrom(r, "opaquePreDiscovery", () => { throw new Error("slot ahead of its head record"); });
      } },
      { name: "a slot peek that throws during evaluation", reason: "slot_unreadable", arrange: r => {
        peekOnceFrom(r, "evaluateRestoreRoute", () => { throw new Error("unreadable"); });
      } },
      { name: "a hosted predecessor", reason: "hosted_restore_not_delivered", rig: { pBoundary: "hosted" } },
      { name: "a predecessor with a failed cross-check", reason: "cross_check_failed", rig: { pBoundary: "failed" } },
      { name: "a predecessor of another slot", reason: "slot_mismatch", rig: { pBoundary: "slot_mismatch" } },
      { name: "a missing predecessor boundary record", reason: "predecessor_record_missing", rig: { pBoundary: "missing" } },
      { name: "an invalid predecessor boundary record", reason: "predecessor_record_invalid", rig: { pBoundary: "invalid" } },
      { name: "the same execution", reason: "same_execution", rig: { pBoundary: "same_execution" } },
      { name: "a hidden procfs", reason: "procfs_hidden", rig: { sLive: "hidden" } },
      { name: "no process boundary", reason: "self_record_invalid", rig: { sBoundary: "none" } },
      { name: "this process's record missing on disk", reason: "self_record_invalid", rig: { sBoundary: "missing_file" } },
      { name: "certified completion not negotiated", reason: "certified_completion_unavailable", rig: { features: ["active_run_snapshot", "claim_generation_fence"] } },
      { name: "an unguarded inventory", reason: "certified_completion_unavailable", rig: { claim: { inventory_guarded: false } } },
    ];
    for (const c of cases) {
      it(`${c.name}: ${c.reason}`, async () => {
        const r = await buildRig(c.rig);
        await c.arrange?.(r);
        const { before, traps } = await drive(r);
        const logs = refusals(r);
        assert.equal(logs.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore|retained/i.test(String((l as { msg: string }).msg)))));
        assert.equal(logs[0]!.reason, c.reason);
        assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 0);
        const feed = api.messages(r.runId).map(m => JSON.stringify(m.payload)).filter(t => t.includes("independent restore refused"));
        assert.equal(feed.length, 1, "one bounded feed line");
        assert.ok(feed[0]!.includes(c.reason) && feed[0]!.length < 400);
        await assertStoppedUntouched(r, before, traps);
      });
    }
  });

  describe("positive controls", () => {
    it("an older retired ledger entry of the run is history, not attribution", async () => {
      const r = await buildRig();
      cfgAdd(r, r.ledgerKey, olderLedgerEntry(r, "retired"));
      const { before, traps } = await drive(r);
      assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1);
      assert.equal(refusals(r).length, 0);
      await assertStoppedUntouched(r, before, traps);
    });

    it("a journal with another key order is classified by shape, from the exact raw value discovery read", async () => {
      const r = await buildRig();
      const j = journalOf(r);
      const raw = JSON.stringify({ runId: j.runId, attemptId: j.attemptId, clonePath: j.clonePath });
      assert.notEqual(raw, cfgGet(r, r.journalKey), "the reordered value differs byte-wise");
      cfgSet(r, r.journalKey, raw);
      const { before, traps } = await drive(r);
      assert.equal(refusals(r).length, 0, JSON.stringify(refusals(r)));
      assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1);
      await assertStoppedUntouched(r, before, traps);
    });
  });

  describe("a blocked or memory-parked foreign-owned predecessor keeps its legacy outcome", () => {
    it("a blocked recovery stage is the unchanged blocked-claim failure", async () => {
      const r = await buildRig();
      setJournal(r, { recovery: { version: 1, source: { runId: r.runId, clonePath: r.pPath, attemptId: r.pAttempt }, attempts: 3,
        startedAt: 1000, deadline: 301_000, claimGeneration: 1, backoffMs: 0, stage: "blocked", blocker: "capture_failed" } });
      await r.runner.execute(r.claim);
      assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 0);
      assert.equal(refusals(r).length, 0);
      assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
      assert.equal(failed(r).length, 1, "the legacy blocked path fails the run");
    });
  });

  describe("a memory-parked foreign-owned predecessor keeps its legacy outcome", () => {
    it("a journal with a memoryPark is not routed to the restore route", async () => {
      const r = await buildRig();
      const slot = (await slotOf(r, r.pPath))!;
      setJournal(r, { memoryPark: { version: 1, binding: { runId: r.runId, claimGeneration: 1, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 0 },
        ack: "not_sent", origin: { flightId: randomUUID(), attemptId: r.pAttempt, executorIncarnation: null, authorityIncarnation: r.pInc, sourceEpoch: slot.epoch } } });
      let discovered = 0;
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = ((...args: Parameters<typeof discover>) => { discovered++; return discover(...args); }) as typeof git.discoverRetainedRecovery;
      await r.runner.execute(r.claim);
      assert.ok(discovered >= 1, "legacy discovery ran");
      assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 0);
      assert.equal(refusals(r).length, 0);
      assert.equal(logged(r, "retained predecessor is foreign-owned; taking the independent restore route").length, 0);
    });
  });

  describe("composite and invalid restore entries", () => {
    it("a restore entry of the branch that is not a valid composite stops the claim, not a generic failure", async () => {
      const r = await buildRig();
      cfgSet(r, `uzi-restore.${BRANCH}.journal`, "not a restore entry");
      const before = await snapshot(r);
      const traps = armTraps(r);
      try { await r.runner.execute(r.claim); } finally { traps.armed = false; }
      assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
      assert.equal(failed(r).length, 0, "no generic failure report");
      assert.equal(refusals(r).length, 0);
      assert.deepEqual(traps.hits, []);
      assert.deepEqual(traps.calls, {});
      assert.equal(fs.readFileSync(path.join(r.bare, "config"), "utf8"), before.config);
      assert.equal(treeDigest(r.pPath), before.clone);
    });
  });

  describe("the disk governor's cache target", () => {
    it("a retained re-claim starts unbound: a forced reclaim before classification and after a refusal touches nothing of the predecessor's", async () => {
      const r = await buildRig({ governor: true, pBoundary: "hosted" });
      const homeBefore = treeDigest(r.runHome);
      let midRun: Promise<unknown> | undefined;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        // Classification has not run yet: the registered callbacks must find no target.
        midRun = Promise.all([r.governor.run!.reclaimInPlace(), r.governor.run!.withHome!(async h => h), Promise.resolve(r.governor.run!.home)]);
        return found;
      }) as typeof git.discoverOpaque;
      await r.runner.execute(r.claim);
      assert.equal(refusals(r)[0]?.reason, "hosted_restore_not_delivered");
      const [dropped, held] = await (midRun as Promise<[unknown, unknown]>);
      assert.equal(dropped, undefined);
      assert.equal(held, undefined);
      const run = r.governor.run!;
      assert.equal(typeof run.home === "function" ? (run.home as () => string | undefined)() : run.home, undefined, "still unbound after the refusal");
      assert.equal(await run.reclaimInPlace(), undefined, "a reclaim after the refusal drops nothing");
      assert.equal(await run.withHome!(async h => h), undefined);
      assert.equal(r.counters.reclaims, 0, "the predecessor's executor was never asked to reclaim");
      assert.deepEqual(r.governor.measures, []);
      assert.deepEqual(r.governor.trims, []);
      assert.equal(treeDigest(r.runHome), homeBefore, "the HOME is byte-identical");
    });

    const homeOf = (run: GovernedRun): string | undefined => typeof run.home === "function" ? run.home() : run.home;

    it("a generation-1 claim binds at registration: the registered callbacks reach its executor and HOME", async () => {
      const r = await buildRig({ governor: true, claim: { claim_generation: 1 } });
      let homeAtDiscovery: string | undefined;
      let reclaimed: RunCacheDropResult | undefined;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        homeAtDiscovery = homeOf(r.governor.run!);
        reclaimed = await r.governor.run!.reclaimInPlace();
        return found;
      }) as typeof git.discoverOpaque;
      await r.runner.execute(r.claim);
      assert.equal(homeAtDiscovery, r.runHome, "bound at registration, before any discovery");
      assert.ok(reclaimed, "the reclaim ran");
      assert.equal(r.counters.reclaims, 1, "through the claim's executor");
    });

    it("the legacy route binds the initial executor and HOME before any HOME use", async () => {
      // No active_run_snapshot: the same retained predecessor takes today's route.
      const r = await buildRig({ governor: true, features: ["claim_generation_fence"] });
      let before: string | undefined | null = null;
      let after: string | undefined | null = null;
      const opaque = git.discoverOpaque.bind(git);
      git.discoverOpaque = (async (...args: Parameters<typeof opaque>) => {
        const found = await opaque(...args);
        before = homeOf(r.governor.run!);
        return found;
      }) as typeof git.discoverOpaque;
      const reserve = git.reserveRecoveryIteration.bind(git);
      git.reserveRecoveryIteration = (async (...args: Parameters<typeof reserve>) => {
        after = homeOf(r.governor.run!);
        await r.governor.run!.reclaimInPlace();
        return reserve(...args);
      }) as typeof git.reserveRecoveryIteration;
      await r.runner.execute(r.claim);
      assert.equal(before, undefined, "a retained re-claim starts unbound");
      assert.equal(after, r.runHome, "legacy routing bound it before the recovery reserved an iteration");
      assert.equal(r.counters.reclaims, 1);
    });
  });
});

describe("CacheTargetBinding", () => {
  const target = (name: string) => ({ executor: { name } as unknown as Executor, home: `/h/${name}` });
  it("is unbound until bound, and refuses to run anything while unbound", async () => {
    const b = new CacheTargetBinding<Executor>();
    assert.equal(b.home(), undefined);
    assert.equal(await b.use(async () => "ran"), undefined);
    b.bind(target("a"));
    assert.equal(b.home(), "/h/a");
    assert.equal(await b.use(async t => t.home), "/h/a");
    b.unbind();
    assert.equal(await b.use(async () => "ran"), undefined);
  });
  it("one snapshot per operation: a rebind during an operation does not change what it sees", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async t => { await gate; return [t.home, t.home]; });
    assert.equal(b.inFlight, 1);
    b.bind(target("b"));
    release();
    assert.deepEqual(await op, ["/h/a", "/h/a"]);
    assert.equal(b.home(), "/h/b");
  });
  it("drainThenBind waits for in-flight operations, refuses new ones meanwhile, then binds", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async t => { await gate; return t.home; });
    let bound = false;
    const swap = b.drainThenBind(target("b")).then(() => { bound = true; });
    await new Promise(r => setImmediate(r));
    assert.equal(bound, false, "still draining");
    assert.equal(b.home(), undefined, "the governor sees no target while draining");
    assert.equal(await b.use(async () => "late"), undefined, "a new operation is refused during the drain");
    release();
    assert.equal(await op, "/h/a");
    await swap;
    assert.equal(b.home(), "/h/b");
    assert.equal(await b.use(async t => t.home), "/h/b");
  });
  it("an abort while draining restores the previous binding and rejects", async () => {
    const b = new CacheTargetBinding<Executor>();
    b.bind(target("a"));
    let release!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const op = b.use(async () => { await gate; });
    const ac = new AbortController();
    const swap = b.drainThenBind(target("b"), ac.signal);
    ac.abort(new Error("stop"));
    await assert.rejects(swap, /stop/);
    assert.equal(b.home(), "/h/a");
    release();
    await op;
    assert.equal(b.inFlight, 0);
  });
});

