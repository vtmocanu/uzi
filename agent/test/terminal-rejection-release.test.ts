import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { RecoveryCoordinator } from "../src/recovery.js";
import { PredecessorSettler, SettlementJournal, type SettlementRecord } from "../src/recovery-settlement.js";
import { Outbox } from "../src/outbox.js";
import type { RecoveryReleaseResponse } from "../src/protocol.js";
import { FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";
import { nullLogger } from "./helpers.js";
import os from "node:os";
import { realpathSync } from "node:fs";

const scratch = realpathSync(os.tmpdir());
const run = "11111111-1111-4111-8111-111111111111";
const hold = "22222222-2222-4222-8222-222222222222";
test("exact release hints follow metadata locks; bad terminals and unknown sibling files remain", async () => {
  const root = await fs.mkdtemp(path.join(scratch, "terminal-release-"));
  const log = nullLogger();
  const box = new Outbox({ root: path.join(root, "outbox"), log, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  try {
    await box.init();
    await box.journalTerminal(run, 3, "implement", 0, { status: "failed" });
    const terminal = path.join(root, "outbox", run, "terminal-3.json");
    const bad = JSON.parse(await fs.readFile(terminal, "utf8"));
    bad.body = { status: "completed" };
    await fs.writeFile(terminal, JSON.stringify(bad));
    const client = new FakeRecoveryClient();
    let feature = true;
    let response: RecoveryReleaseResponse = { run_id: run, released: true, holds_released: 1, generation: 3 };
    const exactClient = Object.assign(client, {
      hasFeature: () => feature,
      releaseRecoveryCustody: async () => response,
    });
    const hints: number[] = [];
    const followups: Promise<void>[] = [];
    const coord = new RecoveryCoordinator({
      client: exactClient, git: new FakeRecoveryGit(), log, recoveryRoot: path.join(root, "recovery"), workerToken: "unit-key",
      terminalRecordProtection: (id) => box.hasPhysicalTerminalProtection(id),
      onAuthoritativeGenerationReleased: (id, gen) => {
        hints.push(gen);
        followups.push((async () => { await coord.inspect(id); await coord.forgetGeneration(id, 99); })());
      },
    });
    const pin = (generation: number) => coord.pin({ runId: run, sourceSha: "1".repeat(40), kind: "issue", branch: "b", generation });
    await pin(3); await pin(4);
    const unknown = path.join(root, "recovery", run, "unverifiable.json");
    await fs.writeFile(unknown, "{");
    const orphan = path.join(root, "recovery", run, "unknown.bundle");
    await fs.writeFile(orphan, "unpublished");
    await coord.release(run, 3);
    await Promise.all(followups);
    assert.deepEqual(hints, [3]);
    assert.deepEqual((await coord.inspect(run)).map((r) => r.generation), [4]);
    await coord.release(run, 4);
    assert.ok(await fs.stat(unknown)); assert.ok(await fs.stat(orphan)); assert.ok(await fs.stat(terminal));
    for (const answer of [
      { ...response, released: false },
      { ...response, retained: true },
      { ...response, run_id: "other" },
      { ...response, generation: 4 },
      { ...response, generation: undefined },
    ]) {
      response = answer;
      await coord.release(run, 3);
    }
    response = { run_id: run, released: true, holds_released: 1, generation: 3 };
    feature = false;
    await coord.release(run, 3);
    await coord.release(run);
    assert.deepEqual(hints, [3], "no hints for ambiguous, retained, noop, or feature-absent releases");
    await coord.forgetGeneration(run, 3);
    assert.deepEqual(hints, [3], "metadata forgetting is not release authority");
    const failClosed = new RecoveryCoordinator({ client, git: new FakeRecoveryGit(), log, recoveryRoot: path.join(root, "recovery"), workerToken: "unit-key",
      terminalRecordProtection: async () => { throw new Error("inventory unavailable"); } });
    await failClosed.release(run);
    assert.ok(await fs.stat(unknown));
  } finally { await box.closeTerminalObservationScan(); await fs.rm(root, { recursive: true, force: true }); }
});

for (const via of ["completed", "live"] as const) {
  test(`${via} ancestry hint follows hold release; owner discard manufactures no hint`, async () => {
    const root = await fs.mkdtemp(path.join(scratch, "terminal-settle-"));
    try {
      const log = nullLogger();
      const journal = new SettlementJournal({ root, workerToken: "unit-key", log });
      const rec: SettlementRecord = {
        version: 1, runId: run, holdId: hold, predecessorGeneration: 3, successorGeneration: 4,
        sourceCaptureId: "cap-3", sourceSha: "1".repeat(40), adoptedSha: "2".repeat(40), seededFrom: "tracking",
        branch: "b", barePath: "/trusted/bare", createdAt: Date.now(),
        state: via === "live" ? "adopted" : "pending_settle", pushedSha: "3".repeat(40), disposition: "publication", attempts: 0,
      };
      await journal.put(rec);
      let reentry: Promise<unknown> | undefined;
      const hints: number[] = [];
      let settler: PredecessorSettler;
      settler = new PredecessorSettler({
        journal, log,
        client: {
          settleRecoveryHold: async () => ({ run_id: run, hold_id: hold, outcome: "released" }),
          settleRecoveryHoldLive: async () => ({ run_id: run, hold_id: hold, outcome: "released" }),
        },
        cleanup: {
          deleteSettlementRefs: async () => {}, deleteRecoveryPin: async () => {}, forgetGeneration: async () => {},
          onAuthoritativeGenerationReleased: (id, gen) => {
            hints.push(gen);
            reentry = (async () => {
              assert.equal(await journal.get(id, hold), null);
              await settler.markTerminal(rec, "owner_discard");
            })();
          },
        },
        liveGit: { isAncestor: async () => true, pinPublished: async () => true, unpinPublished: async () => {} },
      });
      if (via === "live") {
        assert.equal(await settler.observeLivePublication(run, 4, "3".repeat(40), "branch"), 1);
        await settler.settleLive(run);
      } else {
        assert.equal(await settler.settleOne(rec), "released");
      }
      await reentry;
      assert.deepEqual(hints, [3]);
      await journal.put({ ...rec, state: "adopted" });
      await settler.markTerminal({ ...rec, state: "adopted" }, "owner_discard");
      assert.deepEqual(hints, [3]);
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
}
