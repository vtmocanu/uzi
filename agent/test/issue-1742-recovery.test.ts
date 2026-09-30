import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { StubExecutor } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { TOKEN, fakeGitlab, git, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

// Issue #1742 M3 D4(b) — the EARLY CUT: a crash at finalize fetch-back, before the finalization
// pin, with committed work only in the owner's runner clone. G's record is still the early base
// pin (the start tip). After a restart the boot sweep must map it to early_pin_only_after_restart:
// never bundled, never reserved, never read as "no unpublished work". No new capture path exists
// for G; the work is captured under G+1 only if the run is re-claimed (see the note at the end).
installHarness();

const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fsp.rm(root, { recursive: true, force: true });
});

describe("issue #1742 D4(b): crash before fetch-back and the finalization pin", () => {
  it("offers the finalize record, and the boot sweep maps G's early pin to early_pin_only_after_restart without a bundle or reserve", async () => {
    const outboxRoot = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-recovery-"));
    roots.push(outboxRoot);
    const makeOutbox = () =>
      new Outbox({
        root: path.join(outboxRoot, "outbox"),
        log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024,
        maxBytes: 512 * 1024 * 1024,
        retentionMs: 7 * 86_400_000,
      });
    const outbox = makeOutbox();
    await outbox.init();

    const GEN = 5;
    const claim = gitlabClaim(1742, { claim_generation: GEN });
    simulateCommittedWork();
    let reached!: () => void;
    const at = new Promise<void>((resolve) => { reached = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    git.fetchAgentBranch = (async () => {
      reached();
      await gate;
      throw new Error("cut: process died at finalize fetch-back");
    }) as typeof git.fetchAgentBranch;
    const { gitlab } = fakeGitlab();
    const execution = runner(new StubExecutor(nullLogger()), gitlab, TOKEN, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const makeCoordinator = (client: RecoveryArchiveClient) =>
      new RecoveryCoordinator({ client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: TOKEN });
    try {
      await Promise.race([at, new Promise<void>((_, rej) => setTimeout(() => rej(new Error("fetch-back cut not reached")), 10_000))]);

      // M2 behaviour: the finalize record is on disk and a restarted outbox offers it.
      const restarted = makeOutbox();
      await restarted.init();
      assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: claim.run_id, claim_generation: GEN }]);

      // G's recovery record at the cut is the EARLY pin only.
      const client = new FakeRecoveryClient();
      const coord = makeCoordinator(client);
      const before = await coord.inspect(claim.run_id);
      assert.equal(before.length, 1, "exactly the generation's early pin exists");
      assert.equal(before[0]!.generation, GEN);
      assert.equal(before[0]!.state, "pinned");
      assert.equal(before[0]!.finalizationPin, undefined, "the early pin never carries the finalization flag");
      assert.equal(before[0]!.bareDir, undefined);

      // The boot sweep of the restarted worker.
      const snapshot = await coord.snapshotBootRecords();
      await coord.resumePending(undefined, snapshot);
      const after = await coord.inspect(claim.run_id);
      assert.equal(after.length, 1);
      assert.equal(after[0]!.state, "needs_action");
      assert.equal(after[0]!.reason, "early_pin_only_after_restart");
      assert.notEqual(after[0]!.reason, "no_unpublished_work_after_restart");
      assert.equal(after[0]!.bundlePath, undefined, "no bundle is produced for G");
      assert.equal(client.reserveCalls.length, 0, "no reserve at generation G");
    } finally {
      release();
      await execution;
    }
  });
});

describe("issue #1742 D4(a): the runner's finalization pin", () => {
  it("records bareDir, defaultBranch and finalizationPin on the SAME generation record, and the boot sweep captures it at exact G", async () => {
    const GEN = 4;
    const claim = gitlabClaim(1743, { claim_generation: GEN });
    simulateCommittedWork();
    // Hold the run right after the finalization pin (the first changedFiles call after the
    // fetch-back succeeded) so the pinned record is still on disk.
    let fetched = false;
    const realFetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = (async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      const out = await realFetch(...args);
      fetched = true;
      return out;
    }) as typeof git.fetchAgentBranch;
    let reached!: () => void;
    const at = new Promise<void>((resolve) => { reached = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    git.changedFiles = (async () => {
      if (fetched) {
        reached();
        await gate;
        throw new Error("cut: process died after the finalization pin");
      }
      return ["src/impl.ts"];
    }) as typeof git.changedFiles;
    const { gitlab } = fakeGitlab();
    const execution = runner(new StubExecutor(nullLogger()), gitlab, TOKEN).execute(claim);
    try {
      await Promise.race([at, new Promise<void>((_, rej) => setTimeout(() => rej(new Error("post-pin cut not reached")), 10_000))]);
      const client = new FakeRecoveryClient();
      const coord = new RecoveryCoordinator({ client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: TOKEN });
      const records = await coord.inspect(claim.run_id);
      assert.equal(records.length, 1, "the finalization pin reused the early record");
      const rec = records[0]!;
      assert.equal(rec.generation, GEN);
      assert.equal(rec.finalizationPin, true);
      assert.equal(rec.bareDir, path.basename(git.barePathFor(claim.repo.clone_url)));
      assert.ok(rec.defaultBranch && rec.defaultBranch.length > 0);
      // Restart: unpublished work in the bare -> bundled and reserved at exactly G.
      const snapshot = await coord.snapshotBootRecords();
      await coord.resumePending(undefined, snapshot);
      assert.equal(client.reserveCalls.length, 1, "reserved once");
      assert.equal(client.reserveCalls[0]!.generation, GEN);
      assert.equal(client.uploadCalls.length, 1);
      assert.equal((await coord.inspect(claim.run_id))[0]!.state, "uploaded");
    } finally {
      release();
      await execution;
    }
  });
});

// NOT COVERED HERE: the re-claim leg (the same run re-claimed at G+1 on the same worker entering
// the PendingRecoveryCaptureError path, parking recovery_wait with the work in the tracking ref,
// and reserving at G+1 only). That path runs only inside phaseClone of a docker-lane attempt-path
// flight over a journaled predecessor clone; the fixtures that build it (seedPredecessor, the
// fake docker daemon, restartedWorker) are private to clone-attempt-paths.test.ts, which this
// change does not own. The existing "P-resume-unverified (C')" case there covers that path;
// this file covers the G side (early pin, no bundle, no reserve at G).
