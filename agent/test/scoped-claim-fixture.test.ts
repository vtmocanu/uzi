import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const source = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const ordinary = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const nonce = "r3-scoped-claim-fixture";
const capability = "pending_outcome_scoped_claim_v1";

it("R3 shared scoped claim bytes use real outbox projection and negotiated client serialization", async t => {
  const dir = makeScratchRoot("r3-wire-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const box = new Outbox({ root: dir, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  await box.init();
  t.after(() => box.closeTerminalObservationScan());
  for (const generation of [1, 3, 7]) {
    await box.journalTerminal(source, generation, "running", 0, { status: "failed" });
    // Physical malformed evidence carries source identity, never a sendable journal.
    await fs.writeFile(path.join(dir, source, `terminal-${generation}.json`), "{");
    assert.equal((await box.observeTerminalAuthentication(source, generation)).kind, "malformed");
  }
  await box.journalTerminal(ordinary, 2, "running", 0, { status: "completed" });
  let complete = false;
  for (let page = 0; page < 20; page++) {
    if ((await box.scanTerminalObservationsPage()).passComplete) { complete = true; break; }
  }
  assert.equal(complete, true, "bounded physical inventory pass must finish");

  let scoped = true;
  const claims: string[] = [];
  const client = new WorkerClient("https://api.example", "fixture-join-token", "test", nullLogger(), {
    fetch: async (input, init) => {
      if (String(input).endsWith("/register")) {
        return Response.json({
          worker_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", register_nonce: nonce,
          protocol_features: ["active_run_snapshot", ...(scoped ? [capability] : [])],
          worker_outbox_max_pending: 2,
        });
      }
      assert.ok(String(input).endsWith("/runs/claim"));
      claims.push(String(init?.body));
      return new Response(null, { status: 204 });
    },
  });
  const register = () => client.register("fixture", "base", 8, [], [capability], undefined, 0);
  await register();
  assert.equal(client.supportsScopedPendingClaim, true);
  assert.equal(client.hasFeature("provider_policy_hold_v1"), false);
  const registry = new ActiveRunRegistry(
    () => box.listPendingOutcomeSources().sort((a, b) =>
      a.run_id.localeCompare(b.run_id) || a.claim_generation - b.claim_generation),
    () => client.workerOutboxMaxPending, () => box.policyInventoryCoverageAvailable(),
  );
  assert.equal(registry.beginPolicyOwnershipObservation(source)(9), true);
  assert.equal(registry.claimsPausedByPendingOverflow(client.supportsScopedPendingClaim), false);
  const snapshot = registry.build(client.supportsScopedPendingClaim);
  assert.equal(snapshot.pending_overflow, true);
  assert.deepEqual(snapshot.active, [
    { run_id: ordinary, claim_generation: 2, phase: "running", terminal_pending: true },
  ], "unknown historical sources grant no generation-9 lease");
  assert.equal(await client.claimRun(snapshot), null);
  const fixture = await fs.readFile(new URL("../../fixtures/worker-register-snapshot/scoped-claim.json", import.meta.url), "utf8");
  assert.equal(claims[0], fixture.trimEnd(), "exact compact JSON bytes; fixture has a trailing newline");

  scoped = false;
  await register();
  assert.equal(client.supportsScopedPendingClaim, false);
  assert.equal(registry.claimsPausedByPendingOverflow(client.supportsScopedPendingClaim), true);
  // Exercise the serializer's own omission guard even with a previously built snapshot.
  assert.equal(await client.claimRun(snapshot), null);
  const lostFeatureClaim = claims[1];
  assert.ok(lostFeatureClaim);
  const withoutFeature = JSON.parse(lostFeatureClaim).active_snapshot;
  assert.equal(Object.hasOwn(withoutFeature, "pending_policy_protection"), false);
  assert.equal(withoutFeature.pending_overflow, true);
  assert.deepEqual(withoutFeature.pending_all, snapshot.pending_all,
    "feature loss cannot discard historical source protection");
});
