import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { installHarness, deferred, git } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";

installHarness();
for (const boundary of ["J", "B", "O"] as const) {
  it(`integrated retireRecoveryReport cancellation propagates through queued ${boundary}`, async t => {
    const f = await reportRetirementFixture(t);
    const retirement = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
    // The runner owns a separate coordinator over the same process-wide journal queue.
    const entered = deferred(), release = deferred(), queued = deferred();
    const abort = new AbortController();
    const journal = f.coordinator as unknown as Lock;
    const outbox = f.outbox as unknown as OutboxLock;
    const hold = async () => { entered.resolve(); await release.promise; };
    const predecessor = boundary === "J" ? journal.withJournalLock(f.claim.run_id, hold)
      : boundary === "B" ? git.withBareLock(f.bare, hold)
      : outbox.withRunLock(f.claim.run_id, hold);
    t.after(async () => { release.resolve(); await predecessor; });
    await entered.promise;
    let proofs = 0, unlinks = 0;
    const clean = git.credentialFreeCancelCleanHead.bind(git);
    t.mock.method(git, "credentialFreeCancelCleanHead", async (...args: Parameters<typeof clean>) => {
      proofs++; return clean(...args);
    });
    const unlink = fs.unlink.bind(fs);
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof unlink>) => {
      unlinks++; return unlink(...args);
    });
    if (boundary === "J") {
      const actual = (f.r as unknown as { recovery: Lock }).recovery;
      const lock = actual.withJournalLock.bind(actual);
      t.mock.method(actual, "withJournalLock", (...args: Parameters<typeof lock>) => {
        queued.resolve(); return lock(...args);
      });
    } else if (boundary === "B") {
      const lock = git.withBareLock.bind(git);
      t.mock.method(git, "withBareLock", (...args: Parameters<typeof lock>) => {
        queued.resolve(); return lock(...args);
      });
    } else {
      const lock = outbox.withRunLock.bind(outbox);
      t.mock.method(outbox, "withRunLock", (...args: Parameters<typeof lock>) => {
        queued.resolve(); return lock(...args);
      });
    }
    const pending = f.r.retireRecoveryReport(f.claim.run_id, 2, {
      ...retirement.context, signal: abort.signal,
      eligible: () => true,
    });
    await queued.promise;
    for (let i = 0; i < 20 && !f.r.isExecuting(f.claim.run_id); i++)
      await new Promise<void>(r => setImmediate(r));
    assert.equal(f.r.isExecuting(f.claim.run_id), true);
    // O is reached only after the real two-pass content proof.
    if (boundary === "O") {
      for (let i = 0; i < 100 && proofs < 2; i++)
        await new Promise<void>(r => setTimeout(r, 5));
      assert.equal(proofs, 2);
    }
    abort.abort(new Error("integrated cancellation"));
    await pending;
    const proofsAtCancel = proofs;
    release.resolve(); await predecessor;
    if (boundary === "J") await journal.withJournalLock(f.claim.run_id, async () => {});
    if (boundary === "B") await git.withBareLock(f.bare, async () => {});
    if (boundary === "O") await outbox.withRunLock(f.claim.run_id, async () => {});
    assert.equal(proofs, proofsAtCancel, "no proof after cancelled queue drains");
    assert.equal(unlinks, 0);
    await f.assertPending();
  });
}

it("C skip refuses a busy capture immediately without waiting", async t => {
  const f = await reportRetirementFixture(t);
  const entered = deferred(), release = deferred();
  const capture = f.coordinator as unknown as { captureCycle(): Promise<{ state: string }> };
  t.mock.method(capture, "captureCycle", async () => {
    entered.resolve(); await release.promise; return { state: "uploaded" };
  });
  const busy = f.coordinator.captureAndUpload({ record: f.archive, barePath: f.bare, defaultBranch: "main" });
  t.after(async () => { release.resolve(); await busy; });
  await entered.promise;
  const retirement = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  const start = Date.now();
  await f.r.retireRecoveryReport(f.claim.run_id, 2, retirement.context);
  assert.ok(Date.now() - start < 250, "busy C skips before the one-second deadline");
  assert.equal(f.r.isExecuting(f.claim.run_id), false);
  release.resolve(); await busy;
  await f.assertPending();
});

type Lock = { withJournalLock(run: string, fn: () => Promise<void>, signal?: AbortSignal): Promise<void> };
type OutboxLock = { withRunLock(run: string, fn: () => Promise<void>, signal?: AbortSignal): Promise<void> };

for (const boundary of ["J", "B", "O"] as const) {
  it(`${boundary}: cancelled queued proof never starts after predecessor release`, async t => {
    const f = await reportRetirementFixture(t);
    const retirement = await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation);
    assert.ok(retirement);
    const entered = deferred(), release = deferred();
    const abort = new AbortController();
    const hold = async () => { entered.resolve(); await release.promise; };
    const journal = f.coordinator as unknown as Lock;
    const outbox = f.outbox as unknown as OutboxLock;
    const predecessor = boundary === "J" ? journal.withJournalLock(f.claim.run_id, hold)
      : boundary === "B" ? git.withBareLock(f.bare, hold)
      : outbox.withRunLock(f.claim.run_id, hold);
    t.after(async () => { release.resolve(); await predecessor; });
    await entered.promise;
    let lateProof = 0, unlinks = 0;
    t.mock.method(fs, "unlink", async () => { unlinks++; throw new Error("unexpected unlink"); });
    const proof = async () => { lateProof++; };
    const pending = boundary === "J" ? journal.withJournalLock(f.claim.run_id, proof, abort.signal)
      : boundary === "B" ? git.withReportProofBudget({ signal: abort.signal, deadline: Date.now() + 5000 },
        () => git.withBareLock(f.bare, proof))
      : f.outbox.retireTerminalIfEligible(f.claim.run_id, f.generation, {
        ...retirement.context, signal: abort.signal, deadline: Date.now() + 5000,
        eligible: () => { lateProof++; return true; },
      });
    const rejected = assert.rejects(pending);
    abort.abort(new Error("fixture cancel"));
    await rejected;
    release.resolve();
    await predecessor;
    // Drain the same actual queue, rather than merely waiting one event-loop turn.
    if (boundary === "J") await journal.withJournalLock(f.claim.run_id, async () => {});
    if (boundary === "B") await git.withBareLock(f.bare, async () => {});
    if (boundary === "O") await outbox.withRunLock(f.claim.run_id, async () => {});
    assert.equal(lateProof, 0);
    assert.equal(unlinks, 0);
    await f.assertPending();
  });
}
