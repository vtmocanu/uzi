import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, type PositiveOwedCandidateContext } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();
function command(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
function commit(dir: string, name: string): string {
  fs.writeFileSync(path.join(dir, name), name + "\n");
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}

// Guarded server fake: one open exact-generation hold, a reservable/uploadable capture, and a
// final release that closes the hold.
function guardedServer(claim: ReturnType<typeof gitlabClaim>, status: () => string) {
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({
    status: status(), claim_generation: claim.claim_generation, inventory_guarded: true,
  });
  const serverId = "00000000-0000-4000-8000-000000001945";
  let manifest: { checksum: string; byte_size: number } | undefined;
  let finals = 0, open = true;
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: "inventory-hold", generation: claim.claim_generation!, inventory_guarded: true,
    has_available_capture: !!manifest,
  }] : [] });
  client.reserveRecoveryCapture = async () => ({ capture_id: serverId, state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
    for await (const chunk of stream) assert.ok(chunk);
    manifest = m;
    return { capture_id: serverId, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async () => ({
    capture_id: serverId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
    checksum: manifest?.checksum, byte_size: manifest?.byte_size,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
  });
  client.releaseRecoveryCustody = async () => {
    finals++;
    open = false;
    return { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
  };
  return { finals: () => finals, isOpen: () => open };
}

// The pre-report terminal snapshot proves the clone quiescent and uploads the archive. The
// post-report terminal drive must reuse that proof: it runs while execute() still holds the run's
// execution lane, so without it the freeze is refused, the clone is marked preserved and the
// finalize cleanup (quiescence, settlement, clone retirement) is skipped for every finished run.
it(`guarded completed run whose inventory was released retires its clone`, async () => {
  const claim = gitlabClaim(1962, { claim_generation: 29, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  guardedServer(claim, () => api.states.some(s => s.body.status === "completed") ? "completed" : "running");
  let clone = "";
  const r = runner({ run: async ctx => {
    clone = ctx.worktreePath;
    commit(clone, "released.txt");
    return { branch: ctx.branch };
  } }, gitlab, "journal-key");
  await r.execute(claim);
  assert.equal(fs.existsSync(clone), false, "the released run's clone is retired");
});

// Construct the post-completion stores directly: execute() may retire an uploaded clone.
// The callback-free coordinator uploads but retains FINAL while an attributed clone exists.
it("completed guarded run settles its persisted terminal after loss of attributed clone parents", async () => {
  const claim = gitlabClaim(2433, { claim_generation: 29, inventory_guarded: true });
  const generation = claim.claim_generation!;
  const branch = "agent/issue-2433", token = "inventory-restart-journal-key";
  const server = guardedServer(claim, () => "completed");
  const bare = await git.ensureClone(fx.originPath);
  const attemptId = mintAttemptId(generation);
  const clone = await git.runnerCloneForBranch(bare, branch, "issue-2433", {
    beforeFree: async () => { throw new Error("attempt seed must not free a canonical clone"); },
  }, claim.run_id, false, undefined, {
    attemptId, isLive: () => false,
    beforeSeed: async paths => assert.deepEqual(paths, []),
    quiescent: async () => true,
  });
  const head = commit(clone.path, "durable-completed.txt");
  const context: PositiveOwedCandidateContext = {
    barePath: bare, runId: claim.run_id, generation, kind: "issue", branch,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: command(bare, ["rev-parse", "refs/remotes/origin/main"]) },
  };
  await git.fetchAgentBranch(bare, clone.path, branch, claim.run_id, { context });
  await git.markRecoveryCapture(bare, clone.path, branch, claim.run_id, attemptId);
  const coordinator = new RecoveryCoordinator({
    client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: token,
  });
  assert.ok(await coordinator.pin({
    runId: claim.run_id, generation, inventoryGuarded: true, kind: "issue", branch, sourceSha: head,
  }));
  const inventory = await coordinator.freezeInventory({ context, currentSha: head, defaultBranch: "main" });
  assert.ok(inventory);
  assert.ok(inventory.originalRoots?.some(root => root.sha === head));
  assert.equal((await coordinator.captureAndUpload({
    record: inventory, barePath: bare, defaultBranch: "main",
  })).state, "uploaded");
  const uploaded = (await coordinator.inspect(claim.run_id)).find(r => r.captureId === inventory.captureId)!;
  assert.equal(uploaded.state, "uploaded");
  assert.notEqual(uploaded.finalAcknowledged, true);
  assert.equal(server.finals(), 0, "physical custody retains FINAL before loss");
  assert.equal(server.isOpen(), true);
  const holds = await client.listRecoveryHolds(claim.run_id);
  assert.equal(holds.holds.length, 1);
  assert.equal(holds.holds[0]?.generation, generation);
  assert.equal(holds.holds[0]?.has_available_capture, true);
  assert.ok(fs.statSync(clone.path).isDirectory());
  assert.equal(command(clone.path, ["rev-parse", "HEAD"]), head);
  const attribution = await git.readInventoryCloneHeads(bare, claim.run_id);
  assert.deepEqual(attribution, {
    kind: "verified", heads: [head],
    clones: [{ clonePath: clone.path, branch, runId: claim.run_id }], foreignOwners: [],
  });
  const journal = command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]);
  assert.deepEqual(JSON.parse(journal), { runId: claim.run_id, clonePath: clone.path, attemptId });
  const ledger = command(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]);
  assert.ok(ledger.split("\n").map(line => JSON.parse(line)).some(entry =>
    entry.attemptId === attemptId && entry.runId === claim.run_id &&
    entry.clonePath === clone.path && entry.state === "live"));
  assert.ok(uploaded.bundlePath);
  const archive = fs.readFileSync(uploaded.bundlePath);
  assert.equal(archive.length, uploaded.byteSize);
  assert.equal(createHash("sha256").update(archive).digest("hex"), uploaded.checksum);
  command(bare, ["bundle", "verify", uploaded.bundlePath]);
  const refs = command(bare, ["show-ref"]);
  const outboxRoot = path.join(fx.dataDir, "outbox");
  const makeOutbox = async () => {
    const outbox = new Outbox({
      root: outboxRoot, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    assert.equal(outbox.isDisabled(), false);
    return outbox;
  };
  const outbox = await makeOutbox();
  const body = { status: "completed", claim_generation: generation };
  assert.equal((await outbox.journalTerminal(claim.run_id, generation, "finalize", 0, body)).journaled, true);
  assert.ok(await outbox.readTerminalJournal(claim.run_id, generation));

  // Only the emptyDir disappears; the bare, archive, recovery MACs and outbox key survive.
  const runnerRoot = path.join(fx.dataDir, "runner");
  fs.rmSync(runnerRoot, { recursive: true });
  fs.mkdirSync(runnerRoot);
  assert.deepEqual(fs.readdirSync(runnerRoot), []);
  assert.equal(fs.existsSync(path.dirname(clone.path)), false);
  assert.equal(command(bare, ["show-ref"]), refs);
  assert.equal(command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]), journal);
  assert.equal(command(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]), ledger);
  assert.deepEqual(fs.readFileSync(uploaded.bundlePath), archive);
  const freshGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const freshOutbox = await makeOutbox();
  const freshRunner = new RunRunner(client, freshGit, () => {
    throw new Error("boot recovery must not execute a run");
  }, nullLogger(), 20, token, { outbox: freshOutbox });
  const deps = makeTerminalOutboxDeps(freshOutbox, client, {
    gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger(),
  });
  assert.ok(deps);
  const protectedDeps = freshRunner.protectRecoveryTerminalDeps(deps);
  const replay = () => resolvePendingTerminal(protectedDeps, {
    runId: claim.run_id, claimGeneration: generation,
    send: async replayBody => {
      assert.equal(replayBody.status, "completed");
      return { applied: false, status: "completed" };
    },
  });
  assert.equal(freshOutbox.listPendingTerminals().length, 1);
  assert.equal(freshOutbox.depthFor(claim.run_id)?.pendingTerminal, 1);
  assert.equal(await freshRunner.recoveryInventoryPending(claim.run_id, generation), true);
  await replay();
  assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation), "pre-FINAL replay retains the journal");
  assert.equal(server.finals(), 0);
  const release = client.releaseRecoveryCustody.bind(client);
  client.releaseRecoveryCustody = async (runId, gen, evidence, disposition) => {
    assert.equal(runId, claim.run_id);
    assert.equal(gen, generation);
    assert.deepEqual(disposition, {
      kind: "archive", capture_id: uploaded.serverCaptureId,
      source_sha: uploaded.sourceSha, coverage_digest: uploaded.coverageDigest,
    });
    return release(runId, gen, evidence, disposition);
  };
  const boot = await freshRunner.snapshotBootRecoveries();
  assert.ok(boot.some(record => record.captureId === uploaded.captureId && record.state === "uploaded"));
  await freshRunner.resumePendingRecoveries(undefined, boot);
  const persisted = await new RecoveryCoordinator({
    client, git: freshGit, log: nullLogger(), recoveryRoot: freshGit.recoveryRoot, workerToken: token,
  }).inspect(claim.run_id);
  assert.equal(persisted.find(record => record.captureId === uploaded.captureId)?.finalAcknowledged, true);
  assert.equal(server.finals(), 1, "exactly one successful custody release");
  assert.equal(server.isOpen(), false);
  assert.deepEqual((await client.listRecoveryHolds(claim.run_id)).holds, []);
  assert.equal(await freshRunner.recoveryInventoryPending(claim.run_id, generation), false);
  assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation), "FINAL alone does not retire the outbox");
  await replay();
  assert.equal(await freshOutbox.readTerminalJournal(claim.run_id, generation), undefined);
  assert.deepEqual(freshOutbox.listPendingTerminals(), []);
  assert.equal(freshOutbox.depthFor(claim.run_id)?.pendingTerminal ?? 0, 0);
  assert.equal(freshOutbox.depthFor(claim.run_id)?.pendingMessages ?? 0, 0);
  await freshRunner.resumePendingRecoveries();
  assert.equal(server.finals(), 1, "a later recovery pass does not release twice");
});
