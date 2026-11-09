// Issue #3644 Gate B (m4b part 2, review rework of part 1): ownership confirmation around the seed, the executor the teardown
// judges after a post-swap refusal, and the preserve flags once the seed is durable. Production RunRunner + real Git + the
// durable authority through the shared restore rig.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import type { Executor } from "../src/executor.js";
import { StubExecutor } from "../src/executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { parseRestoreValue } from "../src/restore-journal.js";
import { nullLogger } from "./helpers.js";
import { api, git, installHarness } from "./runner-harness.js";
import {
  BRANCH, buildRig, cfgAll, cfgGet, cleanupRig, failed, logged, refusals, resetRigRoots, rigDefaults, snapshot, treeDigest, type Rig,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

afterEach(() => { cleanupRig(); });
beforeEach(() => { resetRigRoots(); });

const restoreKey = `uzi-restore.${BRANCH}.journal`;
const markerFile = (r: Rig): string => path.join(r.dataDir, "retained-homes", `${r.runId}.json`);
const configText = (r: Rig): string => fs.readFileSync(path.join(r.bare, "config"), "utf8");

/** The ownership read answers a different generation from the moment `trigger` fires. */
function loseOwnershipAt(r: Rig, target: object, method: string): void {
  const original = (target as Record<string, (...a: unknown[]) => unknown>)[method]!.bind(target);
  mock.method(target as Record<string, (...a: unknown[]) => unknown>, method, async (...args: unknown[]) => {
    api.setOwnershipStatus(r.runId, "running", (r.claim.claim_generation ?? 2) + 1);
    return original(...args);
  });
}

/** An executor distinct from the rig's initial one: with a shared executor the seed's own quiesce sweep is refused as
 *  touching the predecessor and the seed never becomes durable. */
function distinctSuccessor(): Executor {
  return Object.create(new StubExecutor(nullLogger())) as Executor;
}

function captureFlight(r: Rig): { flight?: Record<string, unknown> } {
  const holder: { flight?: Record<string, unknown> } = {};
  const runner = r.runner as unknown as { registerEvidenceExecutor(f: Record<string, unknown>): void };
  const original = runner.registerEvidenceExecutor.bind(runner);
  mock.method(runner, "registerEvidenceExecutor", (f: Record<string, unknown>) => { holder.flight ??= f; return original(f); });
  return holder;
}

describe("ownership is confirmed around the independent restore seed (issue #3644 m4b review N1)", () => {
  it("a claim the server moved before the seed stops nonterminally with no marker, no S ledger entry and no restore entry", async () => {
    const r = await buildRig({ addrAware: true });
    const before = await snapshot(r);
    const config = configText(r);
    loseOwnershipAt(r, r.runner as unknown as object, "runRestoreRoute");
    await r.runner.execute(r.claim);
    assert.equal(failed(r).length, 0, "no failed report");
    assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0);
    assert.equal(logged(r, "independent restore seeded").length, 0);
    assert.equal(fs.existsSync(markerFile(r)), false, "no marker");
    assert.equal(cfgGet(r, restoreKey), undefined, "no restore entry");
    assert.equal(cfgAll(r, r.ledgerKey).length, 1, "only P's ledger entry");
    assert.equal(configText(r), config, "the bare's config is byte-identical");
    assert.deepEqual(await snapshot(r), before);
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined], "no successor executor was built");
    assert.equal(r.counters.ran, 0);
  });

  it("a claim the server moved after the seed keeps S: the restore entry stays, the stop is nonterminal and the next claim is refused", async () => {
    const r = await buildRig({ addrAware: true, successorExecutor: distinctSuccessor() });
    const original = git.seedIndependentAttempt.bind(git);
    mock.method(git, "seedIndependentAttempt", async (...args: Parameters<typeof original>) => {
      const seeded = await original(...args);
      api.setOwnershipStatus(r.runId, "running", (r.claim.claim_generation ?? 2) + 1);
      return seeded;
    });
    const holder = captureFlight(r);
    await r.runner.execute(r.claim);
    assert.equal(failed(r).length, 0);
    assert.equal(r.counters.ran, 0, "no model turn on S");
    const value = parseRestoreValue(cfgGet(r, restoreKey)!);
    assert.equal(value.kind, "entry", "S's restore entry is preserved");
    assert.equal(value.kind === "entry" ? value.stage : undefined, "ready", "the seed completed: the entry is past seeding");
    assert.equal(refusals(r).length, 0, "the seed was not refused");
    // The "seeded" note is emitted after the post-adoption ownership check, so a lost claim stops before it.
    assert.equal(logged(r, "independent restore seeded").length, 0, "the stop came before the seeded note");
    assert.equal(fs.existsSync(markerFile(r)), true);
    assert.equal(cfgAll(r, r.ledgerKey).length, 2, "P's and S's ledger entries");
    assert.equal(holder.flight!.preserveRecoveryClone, true);
    assert.equal(holder.flight!.preserveSession, true);
    assert.equal(holder.flight!.keepGuardedInventoryOpen, true);
    assert.equal(holder.flight!.retainedLocalCustody, true);
    // The re-claim stops on the composite and writes nothing.
    const config = configText(r);
    api.setOwnershipStatus(r.runId, "running", 3);
    await r.runner.execute({ ...r.claim, claim_generation: 3 });
    assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
    assert.equal(configText(r), config);
  });

  it("a control error once the seed is durable sets every preserve flag before it is rethrown", async () => {
    const r = await buildRig({ addrAware: true, successorExecutor: distinctSuccessor() });
    const runner = r.runner as unknown as { bindFlightSource(...a: unknown[]): Promise<void> };
    mock.method(runner, "bindFlightSource", async () => { throw new CredentialSwitchSignal(); });
    const holder = captureFlight(r);
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 0, "the failure came before the seeded note");
    const value = parseRestoreValue(cfgGet(r, restoreKey)!);
    assert.equal(value.kind, "entry", "the seed is durable");
    assert.equal(value.kind === "entry" ? value.stage : undefined, "ready", "the seed completed: the entry is past seeding");
    assert.equal(refusals(r).length, 0, "the seed was not refused");
    const f = holder.flight!;
    assert.equal(f.preserveRecoveryClone, true);
    assert.equal(f.preserveSession, true);
    assert.equal(f.keepGuardedInventoryOpen, true);
    assert.equal(f.retainedLocalCustody, true);
  });
});

describe("a generic failure handing the seeded attempt to the flight keeps S and refuses the next claim (issue #3644 m4)", () => {
  /** Drives a claim whose seed is durable and whose hand-off to the flight fails with a plain (non-control) Error, then a later
   *  claim; asserts the first is a nonterminal stop with S kept and the second writes nothing. */
  async function failAfterSeed(inject: (r: Rig, boom: Error) => void): Promise<void> {
    // A successor executor distinct from the initial one: with a shared executor the seed's own quiesce sweep is refused as
    // touching the predecessor and the seed never happens.
    const successorExecutor = Object.create(new StubExecutor(nullLogger())) as Executor;
    const r = await buildRig({ addrAware: true, successorExecutor });
    const boom = new Error("injected generic failure after the seed");
    inject(r, boom);
    const holder = captureFlight(r);
    await r.runner.execute(r.claim);
    assert.equal(failed(r).length, 0, "no failed report");
    assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0);
    assert.equal(r.logs.lines.filter(l => /could not be handed to the flight/.test(String((l as { msg: string }).msg))).length, 1, "the injected failure fired");
    assert.equal(r.counters.ran, 0, "no model turn on S");
    assert.equal(logged(r, "independent restore seeded").length, 0, "the failure came before the seeded note");
    const f = holder.flight!;
    assert.equal(f.preserveRecoveryClone, true);
    assert.equal(f.preserveSession, true);
    assert.equal(f.keepGuardedInventoryOpen, true);
    assert.equal(f.retainedLocalCustody, true);
    const value = parseRestoreValue(cfgGet(r, restoreKey)!);
    assert.equal(value.kind, "entry", "S's restore entry is kept");
    assert.equal(value.kind === "entry" ? value.stage : undefined, "ready", "the seed completed: the entry is past seeding");
    assert.equal(refusals(r).length, 0, "the seed was not refused");
    const ledger = cfgAll(r, r.ledgerKey);
    assert.equal(ledger.length, 2, "P's and S's ledger entries");
    const s = JSON.parse(ledger[1]!) as { clonePath: string; state: string };
    assert.notEqual(s.clonePath, r.pPath);
    assert.equal(s.state, "live", "S's ledger entry is not retired");
    assert.equal(fs.existsSync(s.clonePath), true, "S's attempt clone directory exists");
    assert.equal(fs.existsSync(markerFile(r)), true);

    const config = configText(r);
    const entry = cfgGet(r, restoreKey);
    const sDigest = treeDigest(s.clonePath);
    const before = await snapshot(r);
    const logsBefore = r.logs.lines.length;
    mock.restoreAll();
    api.setOwnershipStatus(r.runId, "running", 3);
    await r.runner.execute({ ...r.claim, claim_generation: 3 });
    assert.equal(logged(r, "restore entry present; claim stopped").length, 1, "the next claim stops on S's live restore entry");
    assert.ok(r.logs.lines.length > logsBefore);
    assert.equal(failed(r).length, 0);
    assert.equal(configText(r), config, "the bare's config is byte-identical");
    assert.equal(cfgGet(r, restoreKey), entry);
    assert.deepEqual(cfgAll(r, r.ledgerKey), ledger);
    assert.equal(treeDigest(s.clonePath), sDigest, "S's clone is byte-identical");
    assert.deepEqual(await snapshot(r), before, "P's key, slot, clone, HOME and provision are unchanged");
    assert.equal(logged(r, "independent restore seeded").length, 0);
    assert.equal(r.factoryCalls.filter(c => c.generation === 3).length, 0, "no successor executor was built for the next claim");
  }

  it("bindFlightSource failing with a plain Error stops nonterminally with S kept; the next claim stops on the live entry and writes nothing", async () => {
    await failAfterSeed((r, boom) => {
      const runner = r.runner as unknown as { bindFlightSource(...a: unknown[]): Promise<void> };
      const original = runner.bindFlightSource.bind(runner);
      mock.method(runner, "bindFlightSource", async (...args: unknown[]) => {
        if (args[1] !== r.pPath) throw boom;
        return original(...args);
      });
    });
  });

  it("markRecoveryCapture failing with a plain Error for S's path stops nonterminally with S kept; the next claim stops on the live entry and writes nothing", async () => {
    await failAfterSeed((r, boom) => {
      const original = git.markRecoveryCapture.bind(git);
      mock.method(git, "markRecoveryCapture", async (...args: Parameters<typeof original>) => {
        if (args[1] !== r.pPath) throw boom;
        return original(...args);
      });
    });
  });
});

describe("a refusal after the executor swap tears down S's executor, never the disposed initial one (issue #3644 m4b review N2)", () => {
  it("pin_changed after the swap disposes S's safety, disposes the initial one once, and no reap or cache drop reaches P's HOME", async () => {
    const disposals = { initial: 0, successor: 0 };
    const reaps = { initial: 0, successor: 0 };
    const mk = (key: "initial" | "successor"): Executor => {
      const safety = { dispose: async () => { disposals[key]++; return { kind: "disposed" }; } };
      const stub = Object.create(new StubExecutor(nullLogger())) as Executor;
      return Object.assign(stub, {
        safety: safety as never,
        reapAttributedProcesses: async () => { reaps[key]++; return { complete: true, killed: [], left: [] }; },
      });
    };
    const initial = mk("initial");
    const successor = mk("successor");
    const r = await buildRig({ executor: initial, successorExecutor: successor, addrAware: true,
      factory: (rig, id, _codex, addr) => addr?.generation !== undefined
        ? { executor: successor, homeDir: path.join(rig.homeRoot, `${id}.g${addr.generation}`) }
        : { executor: initial, homeDir: rig.runHome },
    });
    const homeBefore = treeDigest(r.runHome);
    const recovery = r.recovery();
    const original = recovery.withRestoreJournalLock.bind(recovery);
    mock.method(recovery, "withRestoreJournalLock", async (...args: Parameters<typeof original>) => {
      const [rec] = await recovery.inspect(r.runId);
      await (recovery as unknown as { writeRecordUnlocked(x: unknown): Promise<void> }).writeRecordUnlocked({ ...rec, finalAcknowledged: true });
      return original(...args);
    });
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "pin_changed", JSON.stringify(refusals(r)));
    assert.equal(disposals.initial, 1, "the initial executor was disposed once, by the swap");
    assert.ok(disposals.successor >= 1, "S's executor is the one terminally disposed");
    assert.equal(reaps.initial, 0, "no reap acts on the disposed initial executor");
    assert.equal(r.counters.reclaims, 0, "no cache drop on the initial executor");
    assert.equal(treeDigest(r.runHome), homeBefore, "agent-home/<runId> is untouched");
  });

  it("an executor built for the swap but never installed is disposed when the swap fails", async () => {
    let builtDisposals = 0;
    let priorDisposals = 0;
    const prior = Object.assign(Object.create(new StubExecutor(nullLogger())) as Executor,
      { safety: { dispose: async () => { priorDisposals++; return { kind: "incomplete" }; } } as never });
    const built = Object.assign(Object.create(new StubExecutor(nullLogger())) as Executor,
      { safety: { dispose: async () => { builtDisposals++; return { kind: "disposed" }; } } as never });
    const r = await buildRig({ executor: prior, successorExecutor: built, addrAware: true,
      factory: (rig, id, _codex, addr) => addr?.generation !== undefined
        ? { executor: built, homeDir: path.join(rig.homeRoot, `${id}.g${addr.generation}`) }
        : { executor: prior, homeDir: rig.runHome } });
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "executor_unavailable", JSON.stringify(refusals(r)));
    assert.ok(priorDisposals >= 1);
    assert.equal(builtDisposals, 1, "the built-but-not-installed executor was disposed");
    assert.equal(cfgGet(r, restoreKey), undefined, "nothing was written");
  });
});
