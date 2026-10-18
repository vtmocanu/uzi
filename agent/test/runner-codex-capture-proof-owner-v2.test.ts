import { it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, command, commit, frozenRecords, heldServer, packObjects,
  recoveryOf, skipCheckpoints, watchFreezes,
} from "./runner-held-publication-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";

installHarness();

for (const missing of [false, true]) {
  it(`A owner V2 ${missing ? "MISSING_SCOPE" : "accepted"}`, { skip: HELD_LINUX_ONLY }, async t => {
    const claim = gitlabClaim(30310, { claim_generation: 71, inventory_guarded: true });
    claim.secrets.codex = { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" };
    const features = ["recovery_inventory_v1", "recovery_held_publication_v1", "recovery_held_publication_v2"];
    api.setRegisterProtocolFeatures(features);
    const registration = await client.register("v2-cancel", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
    const server = heldServer(claim, features);
    const holdId = "00000000-0000-4000-8000-000000000071";
    claim.repo.id = "00000000-0000-4000-8000-000000000072";
    client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
      hold_id: holdId, generation: 71, inventory_guarded: true, has_available_capture: false,
    }] });
    const fetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
      if (!String(input).endsWith(`/runs/${claim.run_id}/recovery-holds?generation=71`))
        return fetch(input, init);
      // Keep the real client's exact-generation authentication and custody validation.
      return Response.json({ run_id: claim.run_id, exact_hold: {
        run_id: claim.run_id, generation: 71, hold_id: holdId,
        repo_id: claim.repo.id, inventory_guarded: true,
        original_worker_id: registration.worker_id, original_worker_identity: "original",
        state: "open", live_worker_id: registration.worker_id, live_run_id: claim.run_id,
        run_status: server.failedAcked() ? "cancelled" : "running",
        current_claim_generation: 71, claim_released: server.failedAcked(),
      } });
    });
    const send = client.reportState.bind(client);
    mock.method(client, "reportState", async (...args: Parameters<typeof send>) => {
      const ack = await send(...args);
      return args[1].status === "failed" ? { ...ack, status: "cancelled" } : ack;
    });
    skipCheckpoints();
    const freezes = watchFreezes(server);
    let models = 0;
    let clone = "", dirtyBlob = "", oldRoot = "";
    const sourceFilter: { sourcePath?: string } = {};
    const proof = observe(sourceFilter);
    t.after(() => proof.restore());
    const freezesHeld: unknown[] = [];
    const executor: Executor = {
      withCredentialFreeCapture: missing ? undefined : proof.capture,
      async run(ctx) {
        if (ctx.cancelRequested?.()) throw new Error("run cancelled");
        models++;
        throw new Error("model must stay stopped");
      },
      killAgentTree() {},
      settleForCredentialFreeCapture: proof.rig.settle,
    };
    const r = runner(executor, fakeGitlab().gitlab, JOIN_TOKEN, {
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "fixture empty process registry" },
        docker: { state: "not_wired", removed: [], detail: "fixture has no Docker" },
      }),
    });
    // Cancel after dirty clone creation, before model admission.
    let flight: ReturnType<typeof r["buildFlight"]>;
    const build = r["buildFlight"].bind(r);
    r["buildFlight"] = (...args) => flight = build(...args);
    const ensure = r["runnerCloneForClaim"].bind(r);
    r["runnerCloneForClaim"] = async (...args) => {
      const out = await ensure(...args);
      clone = out.path;
      oldRoot = commit(clone, "earlier-root.txt");
      fs.appendFileSync(path.join(clone, "README.md"), "tracked change\n");
      fs.writeFileSync(path.join(clone, "untracked.txt"), "untracked change\n");
      dirtyBlob = command(clone, ["hash-object", "untracked.txt"]);
      r["observeCancellation"](flight);
      sourceFilter.sourcePath = clone;
      proof.arm();
      return out;
    };
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    const coordinator = recoveryOf(r);
    const freeze = coordinator.freezeInventory.bind(coordinator);
    coordinator.freezeInventory = async (...args) => {
      const out = await freeze(...args);
      proof.recordSource(clone);
      freezesHeld.push(proof.observations().source.at(-1));
      return out;
    };
    await proof.execute(r.execute(claim));

    if (missing) {
      assert.equal(proof.observations().source.length, 0, "missing scope forbids owner source reads");
      assert.equal(freezesHeld.length, 0, "missing scope forbids freeze");
      assert.ok(fs.existsSync(clone), "missing scope retains the source clone");
      return;
    }
    proof.assertProof(false);
    assert.ok(freezesHeld.length > 0, "actual inventory freeze executed");
    assert.ok(freezesHeld.every((x: any) => x.active && x.fullReap), "inventory freeze remains inside capture");
    assert.equal(models, 0);
    assert.equal(api.states.filter(s => s.body.status === "failed").length, 1);
    assert.equal(server.heldCalls.length, 1);
    assert.equal(capture.mock.callCount(), 0, "no archive before positive exact terminal authority");
    assert.equal(server.archiveReserves(), 0);
    const records = await frozenRecords(claim);
    assert.equal(records.length, 1);
    assert.ok(freezes.failedAckedAtFreeze.length > 0);
    assert.ok(freezes.failedAckedAtFreeze.every(acked => !acked));
    assert.equal(records[0]!.finalRequest?.disposition.kind, "held_publication");
    assert.equal(server.heldCalls[0]!.tip, records[0]!.sourceSha);
    const objects = packObjects(server.heldCalls[0]!.pack);
    assert.ok(dirtyBlob);
    assert.ok(oldRoot);
    assert.ok(objects.has(dirtyBlob));
    assert.ok(objects.has(oldRoot));
  });
}
