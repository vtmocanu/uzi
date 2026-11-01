import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import path from "node:path";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import { canonicalizeTerminalBody, type Outbox } from "../src/outbox.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { nullLogger } from "./helpers.js";
import { FINALIZE_DOMAIN, RUN, RUN_B, cleanupRoots, forgeRecord, intent, makeOutbox, tmpRoot, stored } from "./publication-intent-fixture.js";

// Issue #1324 D6: the register snapshot's publication_resume list, its exclusion from finalize_resume,
// and the publication_resume_v1 / forge_api_success_v1 capability advertisement.
afterEach(cleanupRoots);

interface Registered { protocolCapabilities: string[] | undefined; snapshot: ActiveSnapshot | undefined }

async function register(outbox: Outbox | undefined): Promise<Registered> {
  const seen: Registered = { protocolCapabilities: undefined, snapshot: undefined };
  const client = {
    getRunOwnership: async () => ({ status: "completed", claim_generation: Number.MAX_SAFE_INTEGER }),
    register: async (_n: string, _t?: string, _m?: number, _c?: string[], protocol?: string[], snapshot?: ActiveSnapshot) => {
      seen.protocolCapabilities = protocol;
      seen.snapshot = snapshot;
      return {};
    },
  } as unknown as WorkerClient;
  const worker = new Worker(
    { workerName: "publication-probe", workerTemplate: "base", maxConcurrentRuns: 1, pollIntervalMs: 1 } as Config,
    client,
    { recoveryInventoryPending: async () => false } as unknown as RunRunner,
    {} as ChatRunner,
    {} as JudgeRunner,
    {} as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] }),
    outbox,
    new Map(),
    outbox ? new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0) : undefined,
  );
  await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> }).registerWithRetry(new AbortController().signal);
  return seen;
}

async function ackedRecord(outbox: Outbox, over: Parameters<typeof intent>[0] = {}, seqs: [number, number] = [2, 1]) {
  const body = stored(over);
  assert.equal((await outbox.savePublicationRecord(body)).ok, true);
  assert.equal((await outbox.replacePublicationRecord({ ...body, enrollment_seq: seqs[0], candidate_seq: seqs[1] }, {})).ok, true);
}

describe("register snapshot: publication_resume", () => {
  it("lists acknowledged records and never also lists them under finalize_resume", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await ackedRecord(outbox);
    await outbox.journalFinalize(RUN, 4);
    await outbox.journalFinalize(RUN_B, 6);
    const { snapshot } = await register(outbox);
    assert.deepEqual(snapshot?.publication_resume, [{ run_id: RUN, claim_generation: 4, enrollment_seq: 2, candidate_seq: 1 }]);
    assert.deepEqual(snapshot?.finalize_resume, [{ run_id: RUN_B, claim_generation: 6 }], "the publication run is withheld from finalize_resume");
  });

  it("sends a snapshot for a publication record alone, with no finalize_resume key", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await ackedRecord(outbox);
    const { snapshot } = await register(outbox);
    assert.equal(snapshot?.pending_overflow, false);
    assert.deepEqual(snapshot?.active, []);
    assert.equal("finalize_resume" in (snapshot ?? {}), false);
    assert.equal(snapshot?.publication_resume?.length, 1);
  });

  it("withholds a run from finalize_resume even when its record is not yet acknowledged (D6), and attests nothing for it", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    assert.equal((await outbox.savePublicationRecord(stored())).ok, true);
    await outbox.journalFinalize(RUN, 4);
    const { snapshot } = await register(outbox);
    assert.equal(snapshot, undefined, "no finalize_resume (an enrolled run must not use the #1742 allowance), no publication_resume");
  });

  it("withholds a run whose record failed authentication from finalize_resume, as the protection still holds", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN, 4);
    await outbox.journalFinalize(RUN_B, 6);
    await fsp.writeFile(path.join(root, RUN, "publication-4.json"), await forgeRecord(root, FINALIZE_DOMAIN, intent()));
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.equal(restarted.hasPublicationRecords(RUN), false, "not tracked in memory");
    const { snapshot } = await register(restarted);
    assert.deepEqual(snapshot?.finalize_resume, [{ run_id: RUN_B, claim_generation: 6 }]);
    assert.equal(snapshot?.publication_resume, undefined);
  });

  it("lists the highest ACKNOWLEDGED generation, so a later sequence-less record cannot hide it", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await ackedRecord(outbox, { claim_generation: 4 }, [9, 3]);
    assert.equal((await outbox.savePublicationRecord(stored({ claim_generation: 6 }))).ok, true, "a later record that was saved but never ACKed");
    assert.deepEqual(outbox.listPublicationRegisterEntries(), [{ run_id: RUN, claim_generation: 4, enrollment_seq: 9, candidate_seq: 3 }]);
    await ackedRecord(outbox, { claim_generation: 5 }, [10, 1]);
    assert.deepEqual(outbox.listPublicationRegisterEntries(), [{ run_id: RUN, claim_generation: 5, enrollment_seq: 10, candidate_seq: 1 }], "the highest acknowledged one wins");
  });

  it("offers nothing for a run with a pending terminal journal, even with an acknowledged record", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await ackedRecord(outbox);
    assert.equal(outbox.listPublicationRegisterEntries().length, 1, "control: offered before the terminal journal");
    assert.equal((await outbox.journalTerminal(RUN, 4, "reviewing", 0, canonicalizeTerminalBody({ status: "completed", report_md: "done" }, 1 << 20))).journaled, true);
    assert.deepEqual(outbox.listPublicationRegisterEntries(), []);
  });

  it("omits the key for an ordinary worker and survives an outbox restart", async () => {
    const root = tmpRoot();
    const first = makeOutbox(root);
    await first.init();
    assert.equal((await register(first)).snapshot, undefined);
    await ackedRecord(first, {}, [9, 3]);
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual((await register(restarted)).snapshot?.publication_resume, [{ run_id: RUN, claim_generation: 4, enrollment_seq: 9, candidate_seq: 3 }]);
    await fsp.rm(path.join(root, RUN, "publication-4.json"));
    const emptied = makeOutbox(root);
    await emptied.init();
    assert.equal((await register(emptied)).snapshot, undefined, "losing the record attests nothing and retires nothing");
  });
});

describe("capability advertisement", () => {
  it("advertises forge_api_success_v1 always and publication_resume_v1 only with a usable outbox", async () => {
    const usable = makeOutbox(tmpRoot());
    await usable.init();
    const withOutbox = (await register(usable)).protocolCapabilities;
    assert.ok(withOutbox?.includes("publication_resume_v1"));
    assert.ok(withOutbox?.includes("forge_api_success_v1"));

    // A store that failed closed (records present, key gone) cannot hold a record.
    const root = tmpRoot();
    const failing = makeOutbox(root);
    await failing.init();
    await ackedRecord(failing);
    await fsp.rm(path.join(root, ".key"));
    const closed = makeOutbox(root);
    await closed.init();
    assert.equal(closed.isDisabled(), true);
    const unusable = (await register(closed)).protocolCapabilities;
    assert.equal(unusable?.includes("publication_resume_v1"), false, "a fail-closed outbox cannot hold a record");
    assert.ok(unusable?.includes("forge_api_success_v1"));

    const none = (await register(undefined)).protocolCapabilities;
    assert.equal(none?.includes("publication_resume_v1"), false);
    assert.ok(none?.includes("forge_api_success_v1"));
  });
});

it("manifest adoption and publication registration retain unresolved policy protection", async () => {
  const outbox = makeOutbox(tmpRoot());
  await outbox.init();
  await outbox.markSpillUnclean(RUN);
  await ackedRecord(outbox);
  await outbox.journalFinalize(RUN, 4);
  assert.equal(outbox.listPublicationRegisterEntries().length, 1);
  const token = outbox.reservePolicyFlight(RUN, 7);
  assert.ok(token);
  assert.equal(outbox.selectPolicyFlight(token, RUN_B), true);
  const pending = outbox.listPendingOutcomeSources();
  assert.equal(pending.length, 1);
  assert.equal(outbox.hasPublicationRecords(RUN), true);
  assert.deepEqual(outbox.listPublicationRegisterEntries(), [], "policy selection withholds publication attestation");
  await (outbox as unknown as { loadRun(runId: string): Promise<void> }).loadRun(RUN);
  assert.deepEqual(outbox.listPendingOutcomeSources(), pending, "manifest adoption cannot overwrite the selected policy state");
  assert.equal(outbox.hasPublicationRecords(RUN), true, "manifest adoption cannot overwrite publication state");
  const { snapshot } = await register(outbox);
  assert.ok(snapshot);
  assert.equal(snapshot.pending_overflow, true, "publication attestation never hides the policy blocker");
  assert.equal(snapshot.publication_resume, undefined, "unresolved policy cannot attest publication-only authority");
  assert.equal(snapshot.finalize_resume, undefined, "publication records remain excluded from the finalize allowance");
  assert.deepEqual(outbox.listPendingOutcomeSources(), pending);
});
