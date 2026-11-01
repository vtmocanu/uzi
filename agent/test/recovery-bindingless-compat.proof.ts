import { after, before, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { RecoveryCoordinator, type RecoveryRecord } from "../src/recovery.js";
import { binding, closed, environment, equalInventory, fixture, git, inventory, patch, restore,
  snapshot, sourceProof, type Reader } from "./fixtures/recovery-bindingless-compat.js";

// Standalone historical proof: run explicitly with node --import tsx --test --test-timeout=120000
// test/recovery-bindingless-compat.proof.ts from agent/. Both pinned Git commits are required;
// shallow agent CI checkouts cannot supply them. No missing-reader skip or simulated reader.
const root = path.resolve(fileURLToPath(new URL("../..", import.meta.url)));
const scratch = path.join(root, ".uzi", "scratch");
const revisions = [
  "a07f813708ced206cd7b8c3f9d09e259b2fff64b",
  "ee17bf89c5cd84a9c61b8c4c8f799febb6f94d94",
] as const;
let suite: string;
const readers: Array<{ name: string; ReaderClass: Reader }> = [];
before(async () => {
  await fs.mkdir(scratch, { recursive: true });
  suite = await fs.mkdtemp(path.join(scratch, "bindingless-compat-"));
  // Both historical readers are mandatory. Missing commits/imports fail setup, never skip.
  for (const revision of revisions) {
    assert.equal(execFileSync("git", ["rev-parse", revision + "^{commit}"], { cwd: root, encoding: "utf8" }).trim(), revision);
    const destination = path.join(suite, "reader-" + revision);
    await fs.mkdir(destination);
    const archive = execFileSync("git", ["archive", revision, "agent/src", "agent/package.json"], {
      cwd: root, maxBuffer: 32 * 1024 * 1024, timeout: 20_000,
    });
    execFileSync("tar", ["-x", "-C", destination], { input: archive, timeout: 20_000 });
    const modulePath = path.join(destination, "agent", "src", "recovery.ts");
    assert.deepEqual(await fs.readFile(modulePath),
      execFileSync("git", ["show", revision + ":agent/src/recovery.ts"], { cwd: root, maxBuffer: 2 * 1024 * 1024 }),
      "execute untouched archived reader source");
    await fs.symlink(path.join(root, "agent", "node_modules"), path.join(destination, "agent", "node_modules"), "dir");
    readers.push({ name: revision, ReaderClass: (await import(pathToFileURL(modulePath).href)).RecoveryCoordinator });
  }
});
// Keep snapshots, mode/byte inventories and diagnostics for review; exports alone are disposable.
after(async () => {
  if (suite) for (const revision of revisions) await fs.rm(path.join(suite, "reader-" + revision), { recursive: true, force: true });
});

for (const variant of ["pinned", "needs_action", "uploaded", "saved-final", "acknowledged", "completion-receipt"] as const) {
  it(`M1 both released readers authenticate and retain binding-less ${variant} across rollback/roll-forward`, async t => {
    const caseRoot = path.join(suite, variant);
    const live = path.join(caseRoot, "original-paths");
    const f = await fixture(live, variant);
    await sourceProof(f);
    const beforeRecord = structuredClone(f.record);
    assert.equal(beforeRecord.exactHoldBinding, undefined);
    assert.equal(beforeRecord.holdSettlement, undefined);
    const beforeMetadata = await snapshot(live, path.join(caseRoot, "before-metadata"));
    const writer = environment(f).coordinator;
    f.record = await patch(writer, f.record, { exactHoldBinding: binding, holdSettlement: closed(f) });
    assert.deepEqual(f.record.attemptedHeadShas, beforeRecord.attemptedHeadShas);
    const afterRecord = structuredClone(f.record);
    const afterMetadata = await snapshot(live, path.join(caseRoot, "after-metadata"));
    const results: unknown[] = [];
    for (const [phase, saved, expected] of [
      ["before", beforeMetadata, beforeRecord], ["after", afterMetadata, afterRecord],
    ] as const) {
      // Each historical read is followed by current code, exercising both rollback and roll-forward.
      for (const reader of readers) for (const selected of [reader, { name: "current", ReaderClass: RecoveryCoordinator }]) {
        await restore(live, saved); // SAME absolute paths, original signed bytes, no writer/re-sign.
        f.record = expected;
        const env = environment(f, selected.ReaderClass);
        const records = await env.coordinator.inspect(f.context.runId);
        assert.deepEqual(records, [expected], `${selected.name} ${phase}: every current known field must survive MAC/coercion`);
        await equalInventory(live, saved.entries);
        await sourceProof(f);
        assert.equal(await env.coordinator.inventoryCleanupState(f.context.runId, 7),
          variant === "acknowledged" ? "acknowledged" : "pending");
        assert.equal(await env.coordinator.hasPersistedCompletionReceipt(f.context.runId, 7), variant === "completion-receipt");

        if (variant === "acknowledged") {
          // Closed metadata must not freeze independently authorized closeout.
          env.cleanup();
          await env.coordinator.forgetGeneration(f.context.runId, 7);
          assert.equal(env.calls.cleanup, 1);
          assert.deepEqual(await env.coordinator.inspect(f.context.runId), []);
          assert.equal(git(f.context.barePath, "for-each-ref", "--format=%(refname)", "refs/owed", "refs/attempted", "refs/recovery"), "");
        } else if (variant === "completion-receipt") {
          env.cleanup();
          await env.coordinator["forgetCompletedGeneration"](f.context.runId, 7, () => false);
          assert.equal(env.calls.cleanup, 2, "receipt preflight and independently authorized closeout");
          assert.deepEqual(await env.coordinator.inspect(f.context.runId), []);
          await assert.rejects(fs.stat(expected.completionSource!.clonePath), { code: "ENOENT" });
          assert.equal(env.calls.final, 0);
          assert.equal(env.calls.produce + env.calls.reserve + env.calls.upload + env.calls.held, 0);
        } else if (phase === "before") {
          env.cleanup();
          env.state.allowFinal = true;
          await env.coordinator.captureAndUpload({ record: expected, barePath: f.context.barePath, defaultBranch: "main" });
          assert.equal(env.calls.final, 1, "positive control: absent closure allows FINAL/replay");
          assert.equal(env.calls.cleanup, 1);
          assert.deepEqual(await env.coordinator.inspect(f.context.runId), []);
        } else {
          env.cleanup();
          const input = { record: expected, barePath: f.context.barePath, defaultBranch: "main" };
          await env.coordinator.operateFrozenInventory(input);
          await env.coordinator.captureAndUpload(input);
          await env.coordinator.resumePending();
          await env.coordinator.forgetGeneration(f.context.runId, 7);
          const retained = (await env.coordinator.inspect(f.context.runId))[0]!;
          assert.ok(retained);
          assert.deepEqual(retained.finalRequest, expected.finalRequest, "no mint or fallback");
          assert.equal(retained.finalAcknowledged, undefined, "closed observation grants no ACK");
          assert.deepEqual(retained.attemptedHeadShas, expected.attemptedHeadShas);
          assert.deepEqual(env.calls, { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 });
          await sourceProof(f);
          // Only the diagnostic reason may be written by a saved FINAL refusal.
          const authenticated = { ...retained };
          if (expected.reason === undefined) delete authenticated.reason;
          else authenticated.reason = expected.reason;
          assert.deepEqual(authenticated, expected);
          await equalInventory(live, saved.entries,
            path.join("data", "recovery", expected.runId, expected.captureId + ".json"));
        }
        // Roll forward the historical reader's actual resulting bytes, without a restore or MAC writer.
        const afterRead = await inventory(live);
        const historicalResult = await env.coordinator.inspect(f.context.runId);
        assert.deepEqual(await environment(f).coordinator.inspect(f.context.runId), historicalResult);
        await equalInventory(live, afterRead);
        results.push({ reader: selected.name, phase, variant, calls: env.calls });
      }
    }
    // Negative probes mutate only a restored disposable fixture, never a source module or saved snapshot.
    for (const reader of readers) {
      await restore(live, afterMetadata);
      const wrong = environment(f, reader.ReaderClass, "different-fixture-worker");
      assert.deepEqual(await wrong.coordinator.inspect(f.context.runId), [], "wrong worker token refuses");
      assert.ok(wrong.warnings.some(message => message.includes("MAC mismatch")));
      await equalInventory(live, afterMetadata.entries);
      const file = path.join(live, "data", "recovery", f.context.runId, f.record.captureId + ".json");
      const envelope = JSON.parse(await fs.readFile(file, "utf8")) as RecoveryRecord & { mac: string };
      envelope.attemptedHeadShas = [f.attempts[0]!, f.head]; // valid parser shape, changed authenticated provenance
      await fs.writeFile(file, JSON.stringify(envelope)); // deliberately retain the original MAC
      const mutated = await inventory(live);
      const badMac = environment(f, reader.ReaderClass);
      assert.deepEqual(await badMac.coordinator.inspect(f.context.runId), [], "altered known field/MAC refuses");
      assert.ok(badMac.warnings.some(message => message.includes("MAC mismatch")), "refusal must reach MAC authentication");
      await equalInventory(live, mutated);
    }
    await restore(live, afterMetadata);
    await equalInventory(live, afterMetadata.entries);
    await fs.writeFile(path.join(caseRoot, "results.json"), JSON.stringify(results, null, 2));
    t.diagnostic(`snapshots and exact byte/mode inventories: ${caseRoot}`);
  });
}
