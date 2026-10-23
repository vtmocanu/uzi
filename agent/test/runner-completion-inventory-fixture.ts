import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer } from "../src/recovery.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import type { RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { fx, git } from "./runner-harness.js";

// Real MAC-authenticated inventory journal semantics, with synthesized Git inputs as in
// recovery-inventory.test.ts. The runner's Git operations, outbox and Codex safety stay real.
export function acknowledgedCompletionInventory(runId: string, generation: number) {
  let context!: PositiveOwedCandidateContext;
  let sourceHeads: string[] = [];
  let aggregateSha = "";
  let open = true;
  let manifest: { checksum: string; byte_size: number } | undefined;
  let releases = 0;
  let physicalCleanups = 0;
  const captureId = "m2-archive";
  const status = () => ({
    capture_id: captureId, state: manifest ? "available" as const : "preparing" as const,
    manifest_bound: !!manifest, checksum: manifest?.checksum, byte_size: manifest?.byte_size,
    expires_at: "2099-01-01T00:00:00Z",
  });
  const archive: RecoveryArchiveClient = {
    hasFeature: feature => feature === "recovery_inventory_v1",
    getRunOwnership: async id => {
      assert.equal(id, runId);
      return { status: "paused", claim_generation: generation, inventory_guarded: true };
    },
    listRecoveryHolds: async id => {
      assert.equal(id, runId);
      return { run_id: runId, holds: open ? [{
        hold_id: "m2-hold", generation, inventory_guarded: true, has_available_capture: false,
      }] : [] };
    },
    reserveRecoveryCapture: async (id, request) => {
      assert.equal(id, runId);
      assert.equal(request.generation, generation);
      assert.equal(request.source_sha, aggregateSha);
      return { capture_id: captureId, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (id, capture) => {
      assert.equal(id, runId);
      assert.equal(capture, captureId);
      return status();
    },
    uploadRecoveryBundle: async (id, capture, uploaded, stream) => {
      assert.equal(id, runId);
      assert.equal(capture, captureId);
      const chunks: Buffer[] = [];
      for await (const chunk of stream) chunks.push(Buffer.from(chunk));
      const bytes = Buffer.concat(chunks);
      assert.equal(uploaded.checksum, createHash("sha256").update(bytes).digest("hex"));
      assert.equal(uploaded.byte_size, bytes.length);
      manifest = { checksum: uploaded.checksum, byte_size: uploaded.byte_size };
      return status();
    },
    releaseRecoveryCustody: async (id, gen, evidence, disposition) => {
      assert.equal(id, runId);
      assert.equal(gen, generation);
      assert.equal(evidence, undefined, "an incomplete run has no publication settlement evidence");
      assert.equal(disposition?.kind, "archive");
      assert.ok(manifest);
      releases++;
      open = false;
      return { run_id: runId, generation, released: true, holds_released: 1 };
    },
  };
  const producer: RecoveryBundleProducer = {
    enumerateOwedCandidates: async () => sourceHeads.map(sha => ({
      sha, pinRef: `refs/owed/${sha}`, contexts: [structuredClone(context)],
    })),
    readInventoryCloneHeads: async () => ({ kind: "verified", heads: sourceHeads, clones: [], foreignOwners: [] }),
    buildRecoveryCoverage: async (_bare, ctx, roots, currentSha) => {
      assert.deepEqual(ctx, context);
      assert.ok(roots.includes(currentSha));
      assert.deepEqual([...roots].sort(), [...sourceHeads].sort());
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots, currentSha })).digest("hex");
      aggregateSha = fingerprint.slice(0, 40);
      return { sha: aggregateSha, fingerprint,
        coverageRef: `refs/uzi-recovery/m2-${generation}`, originalShas: [...roots], currentSha };
    },
    resolveRecoveryBareDir: async bare => bare === path.basename(context.barePath) ? context.barePath : undefined,
    ancestry: async (_bare, source, target) => sourceHeads.includes(source) && target === aggregateSha ? "ancestor" : "unknown",
    fetchDefaultTip: async () => { throw new Error("guarded archive must be credential free"); },
    produceRecoveryBundle: async (_bare, opts) => {
      assert.equal(opts.sourceSha, aggregateSha);
      assert.equal(opts.forgeTip, undefined);
      assert.equal(opts.guardedDefaultBranch, "main");
      // Self-contained empty pack at the synthesized Git seam; no prerequisites.
      const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
      const bytes = Buffer.concat([Buffer.from(`# v2 git bundle\n${opts.sourceSha} refs/heads/recovered-source\n\n`),
        pack, createHash("sha1").update(pack).digest()]);
      fs.writeFileSync(opts.outPath, bytes);
      return { bundlePath: opts.outPath, sourceSha: opts.sourceSha, byteSize: bytes.length,
        checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
    cleanupRecoveryGeneration: async () => {
      physicalCleanups++;
      return "retained"; // Keep the acknowledged journal available to the runner's real guard.
    },
  };
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: path.join(fx.dataDir, "m2-inventory"), workerToken: "local-worker-fixture",
    log: nullLogger(), client: archive, git: producer,
  });
  return {
    coordinator,
    acknowledge: async (ctx: RunContext, head: string) => {
      assert.ok(ctx.defaultBranchCommit);
      context = { runId, generation, kind: "issue", branch: ctx.branch,
        barePath: git.barePathFor(fx.originPath),
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: ctx.defaultBranchCommit } };
      sourceHeads = [...new Set([head, ...(await coordinator.inspect(runId)).map(record => record.sourceSha)])];
      await coordinator.pin({ runId, generation, kind: "issue", branch: ctx.branch, sourceSha: head, inventoryGuarded: true });
      const record = await coordinator.freezeInventory({ context, currentSha: head, defaultBranch: "main" });
      assert.ok(record);
      assert.ok(record.originalRoots?.some(root => root.sha === head), "the actual committed source is covered");
      await coordinator.captureAndUpload({ record, barePath: context.barePath, defaultBranch: "main" });
      const acknowledged = (await coordinator.inspect(runId)).find(r => r.captureId === record.captureId);
      assert.equal(acknowledged?.finalAcknowledged, true);
      assert.equal(acknowledged?.finalRequest?.disposition.kind, "archive");
      assert.equal(acknowledged?.inventoryCurrentSha, head);
      assert.equal(releases, 1);
      assert.equal(physicalCleanups, 1);
      assert.deepEqual(await archive.getRunOwnership!(runId), {
        status: "paused", claim_generation: generation, inventory_guarded: true,
      }, "archive authority belongs to the exact inactive guarded generation");
      assert.equal(await coordinator.inventoryCleanupState(runId, generation), "acknowledged", "real journal authority exists before the completion hold");
    },
  };
}
