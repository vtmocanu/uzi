import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { RecoveryCoordinator } from "../src/recovery.js";
import type { TerminalOutboxDeps } from "../src/terminal-resolve.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

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
    const record = damage === "no-key" ? undefined : await internals.recovery.pin({
      runId, generation, kind: "issue", branch: "task", sourceSha: "a".repeat(40),
      inventoryGuarded: damage !== "legacy" && damage !== "empty",
    });
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
    const expected = damage === "legacy" || damage === "empty" ? 1 : 0;
    assert.equal(terminalRetires, expected, "terminal cleanup needs checked authority");
    assert.equal(finalizeRetires, expected, "finalize cleanup needs checked authority");
    assert.equal(holdReads, 0, "feature loss never requires unsupported hold reads");
  });
}

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
