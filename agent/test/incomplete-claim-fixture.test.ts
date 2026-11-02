import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const capability = "pending_outcome_scoped_claim_v1";
it("incomplete claim shared bytes preserve unknown scope, negotiate and remove the marker after coverage", async t => {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "incomplete-claim-"));
  const writer = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await writer.init();
  assert.equal((await writer.journalTerminal(run, 7, "running", 0, { status: "failed", error: "protected first writer" })).journaled, true);
  await fs.rename(path.join(root, run), path.join(root, ".saved"));
  await fs.symlink(path.join(root, ".saved"), path.join(root, run));
  const retainedBytes = await fs.readFile(path.join(root, ".saved", "terminal-7.json"));
  const box = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  t.after(async () => { await box.closeTerminalObservationScan(); await writer.closeTerminalObservationScan(); await fs.rm(root, { recursive: true, force: true }); });
  await box.init();
  for (let page = 0; page < 8; page++) if ((await box.scanTerminalObservationsPage()).passComplete) break;
  assert.equal(box.policyInventoryCoverageAvailable(), false);
  assert.deepEqual(box.listPendingOutcomeSources(), []);
  let negotiated = true;
  const bodies: string[] = [];
  const client = new WorkerClient("https://api.example", "fixture-token", "test", nullLogger(), {
    fetch: async (input, init) => {
      if (String(input).endsWith("/register")) return Response.json({
        register_nonce: "incomplete-inventory-fixture", protocol_features: ["active_run_snapshot", ...(negotiated ? [capability] : [])],
        worker_outbox_max_pending: 4,
      });
      bodies.push(String(init?.body));
      return new Response(null, { status: 204 });
    },
  });
  await client.register("fixture", "base", 8, [], [capability], undefined, 0);
  const registry = new ActiveRunRegistry(() => box.listPendingOutcomeSources(), () => 4, () => box.policyInventoryCoverageAvailable());
  assert.equal(registry.claimsPausedByPendingOverflow(true), false);
  const snapshot = registry.build(true);
  await client.claimRun(snapshot);
  assert.equal(bodies[0], (await fs.readFile(new URL("../../fixtures/worker-register-snapshot/incomplete-claim.json", import.meta.url), "utf8")).trimEnd());
  negotiated = false;
  await client.register("fixture", "base", 8, [], [capability], undefined, 0);
  await client.claimRun(snapshot); // Even a cached capable snapshot must strip on downgrade.
  const stripped = JSON.parse(bodies[1]!).active_snapshot;
  assert.equal(Object.hasOwn(stripped, "pending_inventory_incomplete"), false);
  assert.equal(Object.hasOwn(stripped, "pending_all"), false);
  assert.equal(stripped.pending_overflow, true);
  assert.deepEqual(await fs.readFile(path.join(root, ".saved", "terminal-7.json")), retainedBytes);
  await fs.unlink(path.join(root, run));
  for (let page = 0; page < 8; page++) if ((await box.scanTerminalObservationsPage()).passComplete) break;
  assert.equal(box.policyInventoryCoverageAvailable(), true);
  const complete = registry.build(true);
  assert.equal(complete.pending_inventory_incomplete, undefined);
  assert.deepEqual(complete.pending_all, []);
});

it("incomplete capacity-exhausted catalog keeps the existing local claim gate closed", async t => {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "incomplete-capacity-"));
  const box = new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  t.after(async () => { await box.closeTerminalObservationScan(); await fs.rm(root, { recursive: true, force: true }); });
  await box.init();
  for (let generation = 1; generation <= 1024; generation++) {
    const token = box.reservePolicyFlight(run, generation);
    assert.ok(token);
    assert.equal(box.selectPolicyFlight(token, run), true);
  }
  assert.equal(box.reservePolicyFlight(run, 1025), undefined, "actual producer capacity is exhausted");
  await fs.mkdir(path.join(root, run));
  await fs.writeFile(path.join(root, run, "terminal-1025.json"), "{");
  for (let page = 0; page < 8; page++) if ((await box.scanTerminalObservationsPage()).passComplete) break;
  assert.equal(box.policyInventoryCoverageAvailable(), false);
  const registry = new ActiveRunRegistry(() => box.listPendingOutcomeSources(), () => 4, () => box.policyInventoryCoverageAvailable());
  assert.equal(registry.claimsPausedByPendingOverflow(true), true, "the marker cannot override the real local protection ceiling");
  assert.equal(registry.claimsPausedByPendingOverflow(false), true);
  assert.equal(registry.build(true).pending_all, undefined);
});
