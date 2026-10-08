import { it } from "node:test";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { WorkerClient } from "../src/client.js";
import { nullLogger, makeClaim, noProofReseed } from "./helpers.js";
import { Outbox } from "../src/outbox.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { resolvePendingTerminal, type TerminalOutboxDeps } from "../src/terminal-resolve.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

function enableGuardedJournalCreation(runId: string, generation: number): void {
  client.hasFeature = name => name === "recovery_inventory_v1";
  client.listRecoveryHolds = async () => ({ run_id: runId, holds: [
    { hold_id: "33333333-3333-4333-8333-333333333333", generation,
      inventory_guarded: true, has_available_capture: false },
  ] });
}

for (const damage of ["mac", "unreadable", "malformed-guard", "dangling-dir", "legacy", "empty", "known-missing", "no-key"] as const) {
  it(`inventory cleanup authority rollback ${damage}`, async () => {
    const { gitlab } = fakeGitlab();
    const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, damage === "no-key" ? undefined : "journal-key");
    const internals = r as unknown as {
      recovery: RecoveryCoordinator;
      outbox: TerminalOutboxDeps["outbox"];
      terminalDeps(): TerminalOutboxDeps;
      retireFinalizeRecord(flight: { runId: string; claimGeneration: number }, site: string): Promise<void>;
    };
    const runId = "cleanup-run", generation = 7;
    enableGuardedJournalCreation(runId, generation);
    const record = damage === "no-key" ? undefined : await internals.recovery.pin({
      runId, generation, kind: "issue", branch: "task", sourceSha: "a".repeat(40),
      inventoryGuarded: damage !== "legacy" && damage !== "empty",
    });
    if (damage !== "no-key") assert.ok(record, "the damage fixture needs an authenticated journal");
    const dir = path.join(git.recoveryRoot, runId);
    if (record) {
      const file = path.join(dir, record.captureId + ".json");
      if (damage === "mac") {
        const raw = JSON.parse(fs.readFileSync(file, "utf8"));
        raw.mac = "0".repeat(64);
        fs.writeFileSync(file, JSON.stringify(raw));
      }
      if (damage === "malformed-guard") {
        const raw = JSON.parse(fs.readFileSync(file, "utf8"));
        raw.inventoryGuarded = { truthy: true };
        fs.writeFileSync(file, JSON.stringify(raw));
      }
      if (damage === "unreadable") { fs.unlinkSync(file); fs.mkdirSync(file); }
      if (damage === "dangling-dir") {
        fs.rmSync(dir, { recursive: true });
        fs.symlinkSync(path.join(git.recoveryRoot, "missing-run-directory"), dir, "dir");
      }
      if (damage === "empty" || damage === "known-missing") fs.rmSync(dir, { recursive: true });
    } else {
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, "unknown.json"), "{}");
    }
    if (damage === "known-missing") {
      client.hasFeature = name => name === "recovery_inventory_v1";
      api.enqueueClaim(gitlabClaim(1999, { run_id: runId, claim_generation: generation, inventory_guarded: true }));
      assert.equal((await client.claimRun())?.inventory_guarded, true);
      // Restore the real feature method so clearFeatures exercises runtime rollback.
      delete (client as unknown as { hasFeature?: unknown }).hasFeature;
    }
    // Reconstruct the coordinator from persisted files, as a feature-loss restart does.
    internals.recovery = (runner({ run: async () => ({ branch: "task" }) }, gitlab,
      damage === "no-key" ? undefined : "journal-key") as unknown as { recovery: RecoveryCoordinator }).recovery;
    delete (client as unknown as { hasFeature?: unknown }).hasFeature;
    client.clearFeatures();
    let holdReads = 0, terminalRetires = 0, finalizeRetires = 0;
    client.listRecoveryHolds = async () => { holdReads++; throw new Error("unsupported"); };
    internals.outbox = {
      isDisabled: () => false,
      retireTerminal: async () => { terminalRetires++; },
      retireFinalize: async () => { finalizeRetires++; },
    } as unknown as TerminalOutboxDeps["outbox"];
    await internals.terminalDeps().outbox.retireTerminal(runId, generation);
    await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "regression");
    const expected = damage === "legacy" ? 1 : 0;
    assert.equal(terminalRetires, expected, "terminal cleanup needs checked authority");
    assert.equal(finalizeRetires, expected, "finalize cleanup needs checked authority");
    assert.equal(holdReads, 0, "feature loss never requires unsupported hold reads");
  });
}

for (const pendingTracking of [false, true]) {
it(`${pendingTracking ? "rework-2464: pending tracking consumer " : "M2 "}normal retirement survives actual cleanup in process and with a genuinely fresh WorkerClient`, async t => {
  const { gitlab } = fakeGitlab();
  const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key");
  const internals = r as unknown as {
    client: WorkerClient; recovery: RecoveryCoordinator; outbox: TerminalOutboxDeps["outbox"];
    terminalDeps(): TerminalOutboxDeps;
    retireFinalizeRecord(flight: { runId: string; claimGeneration: number }, site: string): Promise<void>;
  };
  const runId = "11111111-1111-4111-8111-111111111111", generation = 7;
  const bare = await git.ensureClone(fx.originPath);
  const sha = execFileSync("git", ["-C", bare, "rev-parse", "refs/remotes/origin/main"],
    { encoding: "utf8" }).trim();
  const context = { runId, generation, kind: "issue" as const, branch: "task", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha } };
  let consumers: { config: string; refs: string; files: Record<string, string> } | undefined;
  const snapshot = () => ({
    config: fs.readFileSync(path.join(bare, "config"), "utf8"),
    refs: execFileSync("git", ["-C", bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-runner/"],
      { encoding: "utf8" }),
    files: Object.fromEntries(fs.readdirSync(path.join(bare, "uzi-owed")).filter(n =>
      /^(context|receipt|governed)-/.test(n)).sort().map(n => [n,
        fs.readFileSync(path.join(bare, "uzi-owed", n), "utf8")])),
  });
  if (pendingTracking) {
    const clone = (await git.runnerCloneForBranch(bare, "task", "cleanup", noProofReseed, runId)).path;
    assert.equal((await git.fetchAgentBranch(bare, clone, "task", runId, { context })).kind, "updated");
    const receiptPath = path.join(bare, "uzi-owed",
      `receipt-${createHash("sha256").update("task").digest("hex")}.json`);
    const receipt = JSON.parse(fs.readFileSync(receiptPath, "utf8"));
    fs.writeFileSync(receiptPath, JSON.stringify({ ...receipt, phase: "pending" }));
    assert.equal((await git.committedTrackingOwnership(bare, "task", runId, sha, generation)).kind, "not_owned");
    consumers = snapshot();
  }
  let open = true;
  client.hasFeature = name => name === "recovery_inventory_v1";
  client.getRunOwnership = async () => ({ status: "completed", claim_generation: generation, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: runId, holds: open ? [
    { hold_id: "hold", generation, inventory_guarded: true, has_available_capture: false },
  ] : [] });
  let uploadedBytes = 0;
  let manifest: Parameters<WorkerClient["uploadRecoveryBundle"]>[2] | undefined;
  if (pendingTracking) {
    client.reserveRecoveryCapture = async () => ({ capture_id: "archive-cleanup", state: "preparing" });
    client.getRecoveryCaptureStatus = async () => ({
      capture_id: "archive-cleanup", state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
      checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: "2099-01-01T00:00:00Z",
    });
    client.uploadRecoveryBundle = async (_runId, _captureId, supplied, stream) => {
      for await (const chunk of stream) uploadedBytes += chunk.length;
      assert.equal(uploadedBytes, supplied.byte_size);
      manifest = supplied;
      return client.getRecoveryCaptureStatus(runId, "archive-cleanup");
    };
  }
  client.releaseRecoveryCustody = async () => {
    if (pendingTracking) {
      assert.ok(uploadedBytes > 0, "real Git bundle was streamed before FINAL ACK");
      assert.ok(fs.readdirSync(path.join(git.recoveryRoot, runId)).some(n => n.endsWith(".bundle")));
    }
    open = false;
    return { run_id: runId, generation, released: true, holds_released: 1 };
  };
  const source = await internals.recovery.pin({ runId, generation, kind: "issue", branch: "task",
    sourceSha: sha, inventoryGuarded: true });
  assert.ok(source);
  const record = await internals.recovery.freezeInventory({
    context,
    currentSha: sha, defaultBranch: "main", settledEvidence: "publication",
  });
  assert.ok(record);
  await internals.recovery.resumePending(undefined, [record]);
  assert.deepEqual(await internals.recovery.inspect(runId), []);
  if (pendingTracking) {
    assert.deepEqual(snapshot(), consumers);
    assert.deepEqual(await git.enumerateOwedCandidates(bare, runId), []);
    assert.equal(execFileSync("git", ["-C", bare, "for-each-ref", "--format=%(refname)",
      "refs/uzi-coverage/", "refs/uzi-recovery-pin/"], { encoding: "utf8" }).trim(), "");
    assert.equal((await git.committedTrackingOwnership(bare, "task", runId, sha, generation)).kind, "not_owned");
  }
  assert.equal(fs.existsSync(path.join(git.recoveryRoot, runId)), false);
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/register")) return Response.json({
      worker_id: "22222222-2222-4222-8222-222222222222", protocol_features: ["terminal_rejection_report"],
    });
    if (url.endsWith("/ownership")) return Response.json({
      status: "completed", claim_generation: generation, inventory_guarded: true,
    });
    return Response.json({
      run_id: runId, worker_id: "22222222-2222-4222-8222-222222222222", generation,
      exact_holds: [{ id: "33333333-3333-4333-8333-333333333333", state: "released" }],
      sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true,
      complete: true, outcome: "settled",
    });
  });
  // The in-process transport needs the same real registered identity and negotiated
  // retirement capability as the reconstructed transport below.
  delete (client as unknown as { hasFeature?: unknown }).hasFeature;
  await client.register("test");
  let terminalRetires = 0, finalizeRetires = 0;
  internals.outbox = {
    isDisabled: () => false, retireTerminal: async () => { terminalRetires++; },
    retireFinalize: async () => { finalizeRetires++; },
  } as unknown as TerminalOutboxDeps["outbox"];
  await internals.terminalDeps().outbox.retireTerminal(runId, generation);
  await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "M2 same process");
  assert.equal(terminalRetires, 1);
  assert.equal(finalizeRetires, 1);
  internals.client = new WorkerClient("http://retirement.test", "join-token", "test", nullLogger());
  await internals.client.register("test");
  await internals.terminalDeps().outbox.retireTerminal(runId, generation);
  await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "M2 fresh transport");
  assert.equal(terminalRetires, 2);
  assert.equal(finalizeRetires, 2);
});
}

it("authenticated pre-generation legacy journal preserves legacy retirement", async () => {
  const { gitlab } = fakeGitlab();
  const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key");
  const internals = r as unknown as { recovery: RecoveryCoordinator };
  await internals.recovery.pin({ runId: "legacy-run", kind: "issue", branch: "task", sourceSha: "a".repeat(40) });
  client.clearFeatures();
  assert.equal(await r.recoveryInventoryPending("legacy-run", 7), false);
});

for (const scenario of ["discarded", "released", "absent-released", "absent-legacy", "absent-unknown",
  "absent-unavailable", "mac-discarded", "unreadable-discarded"] as const) {
  it(`fresh WorkerClient normal retirement authority ${scenario}`, async t => {
    const { gitlab } = fakeGitlab();
    const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key");
    const internals = r as unknown as {
      client: WorkerClient; recovery: RecoveryCoordinator; outbox: TerminalOutboxDeps["outbox"];
      terminalDeps(): TerminalOutboxDeps;
      retireFinalizeRecord(flight: { runId: string; claimGeneration: number }, site: string): Promise<void>;
    };
    const runId = "11111111-1111-4111-8111-111111111111", generation = 7;
    const workerId = "22222222-2222-4222-8222-222222222222";
    const absent = scenario.startsWith("absent-");
    let journal: string | undefined;
    let original: string | undefined;
    if (!absent) {
      enableGuardedJournalCreation(runId, generation);
      const record = await internals.recovery.pin({
        runId, generation, kind: "issue", branch: "task", sourceSha: "a".repeat(40), inventoryGuarded: true,
      });
      assert.ok(record);
      journal = path.join(git.recoveryRoot, runId, record.captureId + ".json");
      if (scenario === "mac-discarded") {
        const raw = JSON.parse(fs.readFileSync(journal, "utf8"));
        raw.mac = "0".repeat(64);
        fs.writeFileSync(journal, JSON.stringify(raw));
      }
      if (scenario === "unreadable-discarded") { fs.unlinkSync(journal); fs.mkdirSync(journal); }
      else original = fs.readFileSync(journal, "utf8");
    }
    let custodyReads = 0;
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
      const url = String(input);
      if (url.endsWith("/register")) return Response.json({
        worker_id: workerId, protocol_features: scenario === "absent-legacy" ? [] : ["terminal_rejection_report"],
      });
      if (url.endsWith("/ownership")) {
        if (scenario === "absent-unavailable") throw new Error("unavailable");
        return Response.json({ status: "completed", claim_generation: generation,
          ...(scenario === "absent-legacy" ? { inventory_guarded: false } :
            scenario === "absent-unknown" ? {} : { inventory_guarded: true }) });
      }
      custodyReads++;
      return Response.json({
        run_id: runId, worker_id: workerId, generation,
        exact_holds: [{ id: "33333333-3333-4333-8333-333333333333", state: scenario.includes("released") ? "released" : "discarded" }],
        sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true,
        complete: true, outcome: "settled",
      });
    });
    // Replace the transport itself, not just the coordinator: no process claim memory survives.
    internals.client = new WorkerClient("http://retirement.test", "join-token", "test", nullLogger());
    await internals.client.register("test");
    let terminalRetires = 0, finalizeRetires = 0;
    internals.outbox = {
      isDisabled: () => false, retireTerminal: async () => { terminalRetires++; },
      retireFinalize: async () => { finalizeRetires++; },
    } as unknown as TerminalOutboxDeps["outbox"];
    await internals.terminalDeps().outbox.retireTerminal(runId, generation);
    await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "M1 regression");
    const expected = ["discarded", "absent-released", "absent-legacy"].includes(scenario) ? 1 : 0;
    assert.equal(terminalRetires, expected);
    assert.equal(finalizeRetires, expected);
    if (scenario === "mac-discarded" || scenario === "unreadable-discarded" || scenario === "absent-legacy") {
      assert.equal(custodyReads, 0);
    }
    if (journal && original) assert.equal(fs.readFileSync(journal, "utf8"), original, "observation cannot mutate recovery journal");
  });
}

it("stale terminal supersession preserves guarded recovery and finalize custody", async t => {
  const { gitlab } = fakeGitlab();
  const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key");
  const internals = r as unknown as {
    client: WorkerClient; recovery: RecoveryCoordinator; outbox: Outbox; terminalDeps(): TerminalOutboxDeps;
    retireFinalizeRecord(flight: { runId: string; claimGeneration: number }, site: string): Promise<void>;
  };
  const runId = "11111111-1111-4111-8111-111111111111", generation = 7;
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    if (String(input).endsWith("/register")) return Response.json({
      worker_id: "22222222-2222-4222-8222-222222222222", protocol_features: ["recovery_inventory_v1"],
    });
    if (String(input).endsWith("/runs/claim")) return Response.json({
      ...makeClaim(), run_id: runId, claim_generation: generation, inventory_guarded: true,
    });
    return Response.json({ status: "completed", claim_generation: generation });
  });
  internals.client = new WorkerClient("http://retirement.test", "join-token", "test", nullLogger());
  await internals.client.register("test");
  await internals.client.claimRun();
  enableGuardedJournalCreation(runId, generation);
  const record = await internals.recovery.pin({
    runId, generation, kind: "issue", branch: "task", sourceSha: "a".repeat(40), inventoryGuarded: true,
  });
  assert.ok(record);
  const journal = path.join(git.recoveryRoot, runId, record.captureId + ".json");
  const before = fs.readFileSync(journal, "utf8");
  internals.outbox = new Outbox({ root: path.join(fx.dataDir, "stale-outbox"), log: nullLogger(),
    runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  await internals.outbox.init();
  await internals.outbox.journalTerminal(runId, generation, "running", 0, { status: "completed" });
  await internals.outbox.journalFinalize(runId, generation);
  await resolvePendingTerminal(internals.terminalDeps(), { runId, claimGeneration: generation,
    send: async () => ({ applied: false, staleClaim: true, status: "completed" }), signal: new AbortController().signal });
  assert.equal(internals.outbox.hasPendingTerminal(runId, generation), false);
  assert.equal(internals.client.knowsInventoryGuardedClaim(runId, generation), true);
  await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "stale regression");
  assert.deepEqual(internals.outbox.listPendingFinalizes(), [{ run_id: runId, claim_generation: generation }]);
  assert.equal(fs.readFileSync(journal, "utf8"), before);
});

for (const sibling of ["none", "unreadable", "uncovered"] as const) {
  it(`inventory cleanup authority FINAL ACK ${sibling}`, async () => {
    const { gitlab } = fakeGitlab();
    const r = runner({ run: async () => ({ branch: "task" }) }, gitlab, "journal-key");
    const internals = r as unknown as {
      recovery: RecoveryCoordinator;
      outbox: TerminalOutboxDeps["outbox"];
      terminalDeps(): TerminalOutboxDeps;
      retireFinalizeRecord(flight: { runId: string; claimGeneration: number }, site: string): Promise<void>;
    };
    const runId = "cleanup-ack", generation = 7, sha = "a".repeat(40);
    client.hasFeature = name => name === "recovery_inventory_v1";
    client.getRunOwnership = async () => ({ status: "completed", claim_generation: generation, inventory_guarded: true });
    client.listRecoveryHolds = async () => ({ run_id: runId, holds: [
      { hold_id: "hold", generation, inventory_guarded: true, has_available_capture: false },
    ] });
    const requests: unknown[] = [];
    let loseAck = true;
    client.releaseRecoveryCustody = async (...args) => {
      requests.push(structuredClone(args));
      if (loseAck) throw new Error("ACK unavailable");
      return { run_id: runId, generation, released: true, holds_released: 1 };
    };
    git.enumerateOwedCandidates = async () => [];
    git.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] });
    const record = await internals.recovery.freezeInventory({
      context: { runId, generation, kind: "issue", branch: "task", barePath: git.barePathFor(fx.originPath),
        defaultIdentity: { ref: "refs/remotes/origin/main", sha } },
      currentSha: sha, defaultBranch: "main", settledEvidence: "publication",
    });
    assert.ok(record);
    await internals.recovery.resumePending(undefined, [record]);
    assert.equal(requests.length, 1);
    delete (client as unknown as { hasFeature?: unknown }).hasFeature;
    client.clearFeatures();
    await internals.recovery.resumePending(undefined, [record]);
    assert.equal(requests.length, 1, "feature loss retains exact request without wire replay");
    client.hasFeature = name => name === "recovery_inventory_v1";
    loseAck = false;
    await internals.recovery.resumePending(undefined, [record]);
    assert.equal(requests.length, 2);
    assert.deepEqual(requests[1], requests[0], "restored feature replays frozen identity");
    assert.equal((await internals.recovery.inspect(runId))[0]?.finalAcknowledged, true);
    if (sibling === "unreadable") fs.writeFileSync(path.join(git.recoveryRoot, runId, "other.json"), "{}");
    if (sibling === "uncovered") await internals.recovery.pin({
      runId, generation, kind: "issue", branch: "task", sourceSha: "b".repeat(40), inventoryGuarded: true,
    });
    delete (client as unknown as { hasFeature?: unknown }).hasFeature;
    client.clearFeatures();
    let terminalRetires = 0, finalizeRetires = 0;
    internals.outbox = {
      isDisabled: () => false,
      retireTerminal: async () => { terminalRetires++; },
      retireFinalize: async () => { finalizeRetires++; },
    } as unknown as TerminalOutboxDeps["outbox"];
    await internals.terminalDeps().outbox.retireTerminal(runId, generation);
    await internals.retireFinalizeRecord({ runId, claimGeneration: generation }, "regression");
    assert.equal(terminalRetires, sibling === "none" ? 1 : 0);
    assert.equal(finalizeRetires, sibling === "none" ? 1 : 0);
  });
}
