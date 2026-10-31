import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import path from "node:path";

import type { RawWriteSeam } from "../src/outbox.js";
import {
  cleanupRoots, forgeRecord, intent, makeOutbox, FINALIZE_DOMAIN, PUBLICATION_DOMAIN, RUN, RUN_B, CAND, CAND2, tmpRoot,
} from "./publication-intent-fixture.js";
import { MAX_PUBLICATION_RECORD_BYTES, type PublicationRecord } from "../src/publication-intent.js";

// Issue #1324: the worker-local publication-intent record (`publication-<G>.json`) in the outbox.
afterEach(cleanupRoots);

const file = (root: string, run: string, gen: number) => path.join(root, run, `publication-${gen}.json`);
const names = async (root: string, run: string) => (await fsp.readdir(path.join(root, run))).sort();

async function savedAndAcked(root: string, over: Partial<PublicationRecord> = {}) {
  const outbox = makeOutbox(root);
  await outbox.init();
  const saved = await outbox.savePublicationRecord({ ...intent(), ...over });
  assert.equal(saved.ok, true);
  const acked = await outbox.replacePublicationRecord({ ...intent(), ...over, enrollment_seq: 1, candidate_seq: 1 }, {});
  assert.equal(acked.ok, true);
  return outbox;
}

describe("publication record: MAC, size and version", () => {
  it("round-trips through save, replace, restart and load", async () => {
    const root = tmpRoot();
    const outbox = await savedAndAcked(root);
    const again = makeOutbox(root);
    await again.init();
    const loaded = await again.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded, { kind: "ok", record: { ...intent(), enrollment_seq: 1, candidate_seq: 1 } });
    assert.deepEqual(again.listPublicationRecords(), [{ run_id: RUN, claim_generation: 4, enrollment_seq: 1, candidate_seq: 1 }]);
    assert.equal(outbox.hasPublicationRecords(RUN), true);
  });

  it("keeps the finalize and publication MAC domains apart in both directions", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN, 4);
    // A finalize record is not a publication record, even when its file is copied under the other name.
    await fsp.copyFile(path.join(root, RUN, "finalize-4.json"), file(root, RUN, 4));
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "unauthenticated");
    // A well-formed publication body sealed under the FINALIZE domain does not verify as a publication record.
    await fsp.writeFile(file(root, RUN, 5), await forgeRecord(root, FINALIZE_DOMAIN, { ...intent({ claim_generation: 5 }) }));
    assert.equal((await outbox.loadPublicationRecord(RUN, 5)).kind, "unauthenticated");
    // Control: the same body under the publication domain verifies, so the forger itself is right.
    await fsp.writeFile(file(root, RUN, 6), await forgeRecord(root, PUBLICATION_DOMAIN, { ...intent({ claim_generation: 6 }) }));
    assert.equal((await outbox.loadPublicationRecord(RUN, 6)).kind, "ok");
    // And a publication record copied to a finalize name is never adopted as a finalize record.
    await outbox.savePublicationRecord(intent({ claim_generation: 7 }));
    await fsp.copyFile(file(root, RUN, 7), path.join(root, RUN, "finalize-7.json"));
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes().filter((f) => f.claim_generation === 7), []);
  });

  it("refuses a tampered record and an over-cap file, and neither implies retirement", async () => {
    const root = tmpRoot();
    const outbox = await savedAndAcked(root);
    const raw = JSON.parse(await fsp.readFile(file(root, RUN, 4), "utf8")) as Record<string, unknown>;
    await fsp.writeFile(file(root, RUN, 4), JSON.stringify({ ...raw, candidate: CAND2 }));
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "unauthenticated");
    await fsp.writeFile(file(root, RUN, 9), "x".repeat(MAX_PUBLICATION_RECORD_BYTES + 1));
    assert.equal((await outbox.loadPublicationRecord(RUN, 9)).kind, "unauthenticated");
    assert.equal((await outbox.loadPublicationRecord(RUN, 77)).kind, "absent");
    // The unauthenticated record is still on disk, still protects the run, and is never discarded blindly.
    assert.equal(await outbox.discardPublicationRecord(RUN, 4, { enrollment_seq: 1 }), false);
    assert.equal(await outbox.hasPhysicalTerminalProtection(RUN), true);
    assert.ok((await names(root, RUN)).includes("publication-4.json"));
  });

  it("refuses an oversize intent at write time without touching the disk", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    const big = intent();
    big.guard.secret_remediation.known = Array.from({ length: 200 }, (_, i) => ({ file: "f".repeat(1000), start_line: i, commit: "a".repeat(40), rule_id: "r" }));
    const result = await outbox.savePublicationRecord(big);
    assert.deepEqual([result.ok, result.ok ? "" : result.reason, result.ok ? "" : result.state], [false, "intent_too_large", "unwritten"]);
    await assert.rejects(fsp.readdir(path.join(root, RUN)), { code: "ENOENT" });
  });

  it("reports a MAC-valid record of another version, or with an unknown guard member, as a typed mismatch", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await fsp.mkdir(path.join(root, RUN), { recursive: true });
    await fsp.writeFile(file(root, RUN, 10), await forgeRecord(root, PUBLICATION_DOMAIN, { ...intent({ claim_generation: 10 }), v: 2 }));
    const future = { ...intent({ claim_generation: 11 }), guard: { ...intent().guard, quarantine_state: { x: 1 } } };
    await fsp.writeFile(file(root, RUN, 11), await forgeRecord(root, PUBLICATION_DOMAIN, future));
    const missing = intent({ claim_generation: 12 }) as unknown as Record<string, unknown>;
    delete (missing.guard as Record<string, unknown>).landed_checkpoint;
    await fsp.writeFile(file(root, RUN, 12), await forgeRecord(root, PUBLICATION_DOMAIN, missing));
    await fsp.writeFile(file(root, RUN, 13), await forgeRecord(root, PUBLICATION_DOMAIN, { ...intent({ claim_generation: 99 }) }));
    for (const gen of [10, 11, 12, 13]) {
      const loaded = await outbox.loadPublicationRecord(RUN, gen);
      assert.equal(loaded.kind, "mismatch", `generation ${gen}`);
    }
  });
});

describe("publication record: crash-safe candidate update", () => {
  it("never leaves a window without a valid record while the replacement is being written", async () => {
    const root = tmpRoot();
    let observed: Record<string, unknown> | undefined;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "publication" && ctx.path.endsWith(".tmp") && observed === undefined && replacing) {
        // Mid-replace: the live path still holds the whole OLD record.
        observed = JSON.parse(await fsp.readFile(file(root, RUN, 4), "utf8")) as Record<string, unknown>;
      }
      await write();
    };
    let replacing = false;
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    await outbox.savePublicationRecord(intent());
    await outbox.replacePublicationRecord({ ...intent(), enrollment_seq: 1, candidate_seq: 1 }, {});
    replacing = true;
    const next = { ...intent({ candidate: CAND2 }), enrollment_seq: 1, candidate_seq: 2 };
    const result = await outbox.replacePublicationRecord(next, { enrollment_seq: 1, candidate_seq: 1 });
    assert.equal(result.ok, true);
    assert.equal(observed?.candidate_seq, 1, "the old record was intact during the replacement");
    const loaded = await outbox.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded.kind === "ok" ? [loaded.record.candidate, loaded.record.candidate_seq] : [], [CAND2, 2]);
    assert.deepEqual((await names(root, RUN)).filter((n) => n.endsWith(".tmp")), []);
  });

  it("keeps the old record valid when the replacement fails between steps", async () => {
    const root = tmpRoot();
    let failNext = false;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (failNext && ctx.kind === "publication") throw Object.assign(new Error("EIO"), { code: "EIO" });
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    await outbox.savePublicationRecord(intent());
    await outbox.replacePublicationRecord({ ...intent(), enrollment_seq: 1, candidate_seq: 1 }, {});
    failNext = true;
    const result = await outbox.replacePublicationRecord({ ...intent({ candidate: CAND2 }), enrollment_seq: 1, candidate_seq: 2 }, { enrollment_seq: 1, candidate_seq: 1 });
    assert.deepEqual([result.ok, result.ok ? "" : result.reason, result.ok ? "" : result.state], [false, "io_error", "unwritten"]);
    failNext = false;
    const loaded = await outbox.loadPublicationRecord(RUN, 4);
    assert.deepEqual(loaded.kind === "ok" ? [loaded.record.candidate, loaded.record.candidate_seq] : [], [CAND, 1]);
    assert.deepEqual((await names(root, RUN)).filter((n) => n.endsWith(".tmp")), []);
  });

  it("a crash-left temp is removed at the next start and the record survives", async () => {
    const root = tmpRoot();
    await savedAndAcked(root);
    const stray = path.join(root, RUN, "publication-4.json.11111111-1111-4111-8111-111111111111.tmp");
    await fsp.writeFile(stray, "{ half a record");
    const restarted = makeOutbox(root);
    await restarted.init();
    await assert.rejects(fsp.stat(stray), { code: "ENOENT" });
    assert.equal((await restarted.loadPublicationRecord(RUN, 4)).kind, "ok");
  });

  it("swaps only on the expected sequence numbers, one step at a time, for the same identity", async () => {
    const root = tmpRoot();
    const outbox = await savedAndAcked(root);
    const reason = (r: Awaited<ReturnType<typeof outbox.replacePublicationRecord>>) => (r.ok ? (r.adopted ? "adopted" : "ok") : r.reason);
    const at = (candidate_seq: number, over: Partial<PublicationRecord> = {}): PublicationRecord => ({ ...intent(), enrollment_seq: 1, candidate_seq, ...over });
    assert.equal(reason(await outbox.replacePublicationRecord(at(2, { candidate: CAND2 }), { enrollment_seq: 1, candidate_seq: 9 })), "cas_mismatch");
    assert.equal(reason(await outbox.replacePublicationRecord(at(3, { candidate: CAND2 }), { enrollment_seq: 1, candidate_seq: 1 })), "cas_mismatch", "skipping a sequence");
    assert.equal(reason(await outbox.replacePublicationRecord(at(2, { candidate: CAND2, enrollment_seq: 2 }), { enrollment_seq: 1, candidate_seq: 1 })), "cas_mismatch", "another enrollment");
    assert.equal(reason(await outbox.replacePublicationRecord(at(2, { candidate: CAND2, original_head: "9".repeat(40) }), { enrollment_seq: 1, candidate_seq: 1 })), "conflict", "another identity");
    assert.equal(reason(await outbox.replacePublicationRecord(at(2, { candidate: CAND2 }), { enrollment_seq: 1, candidate_seq: 1 })), "ok");
    assert.equal(reason(await outbox.replacePublicationRecord(at(2, { candidate: CAND2 }), { enrollment_seq: 1, candidate_seq: 1 })), "adopted", "re-applying what is on disk is a success");
    assert.equal(reason(await outbox.replacePublicationRecord(at(1), { enrollment_seq: 1, candidate_seq: 2 })), "cas_mismatch", "never backwards");
  });
});

describe("publication record: protection and discovery", () => {
  it("protects its run from retention, retireRun, physical probes and a missing key", async () => {
    const root = tmpRoot();
    let now = 1_000;
    const outbox = makeOutbox(root, { now: () => now, retentionMs: 10 });
    await outbox.init();
    await outbox.savePublicationRecord(intent());
    await outbox.replacePublicationRecord({ ...intent(), enrollment_seq: 1, candidate_seq: 1 }, {});
    now += 1_000_000;
    await outbox.sweepRetention(now);
    assert.ok((await names(root, RUN)).includes("publication-4.json"), "retention keeps it");
    await outbox.retireRun(RUN);
    assert.ok((await names(root, RUN)).includes("publication-4.json"), "retireRun keeps it");
    assert.equal(await outbox.hasPhysicalTerminalProtection(RUN), true);

    // After a restart, with the sweep seeing nothing but the record.
    const restarted = makeOutbox(root, { now: () => now, retentionMs: 10 });
    await restarted.init();
    await restarted.sweepRetention(now + 1_000_000);
    assert.ok((await names(root, RUN)).includes("publication-4.json"));

    // Only after a confirmed retirement is it discardable, and then the run is reclaimable.
    assert.equal(await restarted.discardPublicationRecord(RUN, 4, { enrollment_seq: 2 }), false, "another enrollment_seq");
    assert.equal(await restarted.discardPublicationRecord(RUN, 4, {}), false, "a missing enrollment_seq");
    assert.equal(await restarted.discardPublicationRecord(RUN, 4, { enrollment_seq: 1 }), true);
    assert.equal(await restarted.hasPhysicalTerminalProtection(RUN), false);
    await restarted.retireRun(RUN);
    await assert.rejects(fsp.readdir(path.join(root, RUN)), { code: "ENOENT" });
  });

  it("an unreadable foreign publication file still protects the run", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await fsp.mkdir(path.join(root, RUN_B), { recursive: true });
    await fsp.writeFile(path.join(root, RUN_B, "publication-3.json"), "not json");
    await outbox.retireRun(RUN_B);
    assert.ok((await names(root, RUN_B)).includes("publication-3.json"));
  });

  it("fails closed rather than minting a new key when only a publication record proves records exist", async () => {
    const root = tmpRoot();
    await savedAndAcked(root);
    await fsp.rm(path.join(root, ".key"));
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.equal(restarted.isDisabled(), true);
    await assert.rejects(fsp.stat(path.join(root, ".key")), { code: "ENOENT" }, "no replacement key was minted");
    const result = await restarted.savePublicationRecord(intent({ claim_generation: 8 }));
    assert.deepEqual([result.ok, result.ok ? "" : result.reason], [false, "outbox_disabled"]);
    assert.equal((await restarted.loadPublicationRecord(RUN, 4)).kind, "unreadable");
  });

  it("never treats a finalize record as an enrollment, nor retires a publication record through it", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN, 4);
    assert.deepEqual(outbox.listPublicationRecords(), []);
    assert.deepEqual(outbox.listPublicationRegisterEntries(), []);
    assert.equal(outbox.hasPublicationRecords(RUN), false);
    await outbox.savePublicationRecord(intent());
    assert.equal(await outbox.retireFinalize(RUN, 4), true);
    assert.deepEqual(await names(root, RUN), ["publication-4.json"]);
    assert.equal(await outbox.retireFinalize(RUN, 4), false);
    assert.equal((await outbox.loadPublicationRecord(RUN, 4)).kind, "ok");
  });

  it("lists register entries for acknowledged records only, one per run (the highest generation)", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    // RUN: two generations, both acknowledged; the highest wins. RUN_B: saved but never acknowledged.
    for (const gen of [3, 4]) {
      await outbox.savePublicationRecord(intent({ claim_generation: gen }));
      await outbox.replacePublicationRecord({ ...intent({ claim_generation: gen }), enrollment_seq: gen - 2, candidate_seq: 1 }, {});
    }
    await outbox.savePublicationRecord(intent({ run_id: RUN_B }));
    assert.deepEqual(outbox.listPublicationRegisterEntries(), [{ run_id: RUN, claim_generation: 4, enrollment_seq: 2, candidate_seq: 1 }]);
    assert.equal(outbox.listPublicationRecords().length, 3);
    assert.equal(outbox.hasPublicationRecords(RUN_B), true);
  });

  it("classifies the local write failure matrix", async () => {
    const outcome = (r: Awaited<ReturnType<ReturnType<typeof makeOutbox>["savePublicationRecord"]>>) => (r.ok ? "ok" : `${r.reason}/${r.state}`);
    // Not initialised (disabled): nothing is written.
    assert.equal(outcome(await makeOutbox(tmpRoot()).savePublicationRecord(intent())), "outbox_disabled/unwritten");
    // ENOSPC on the temp write, absence verified.
    const enospc: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "publication" && ctx.path.endsWith(".tmp")) throw Object.assign(new Error("full"), { code: "ENOSPC" });
      await write();
    };
    const full = makeOutbox(tmpRoot(), { rawWrite: enospc });
    await full.init();
    assert.equal(outcome(await full.savePublicationRecord(intent())), "enospc/unwritten");
    // A generic write failure with the destination verified absent.
    const eio: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "publication" && ctx.path.endsWith(".tmp")) throw Object.assign(new Error("io"), { code: "EIO" });
      await write();
    };
    const broken = makeOutbox(tmpRoot(), { rawWrite: eio });
    await broken.init();
    assert.equal(outcome(await broken.savePublicationRecord(intent())), "io_error/unwritten");
    // Partial: the record was linked but the directory fsync failed, so a file may exist.
    const root = tmpRoot();
    const partial = makeOutbox(root);
    await partial.init();
    const target = partial as unknown as { fsyncDir(dir: string, strict?: boolean): Promise<void> };
    const real = target.fsyncDir.bind(partial);
    target.fsyncDir = async (dir, strict) => {
      if (dir === path.join(root, RUN)) throw new Error("fsync failed");
      await real(dir, strict);
    };
    assert.equal(outcome(await partial.savePublicationRecord(intent())), "io_error/unverified");
    assert.ok((await names(root, RUN)).includes("publication-4.json"), "the linked record is kept, never guessed away");
    // An existing different record is a conflict, not an overwrite.
    const other = makeOutbox(tmpRoot());
    await other.init();
    await other.savePublicationRecord(intent());
    assert.equal(outcome(await other.savePublicationRecord(intent({ original_head: "9".repeat(40) }))), "conflict/unverified");
    // An existing equal-identity record is adopted.
    const adopted = await other.savePublicationRecord(intent({ candidate: CAND2 }));
    assert.deepEqual([adopted.ok, adopted.ok ? adopted.adopted : undefined, adopted.ok ? adopted.record.candidate : undefined], [true, true, CAND]);
  });
});
