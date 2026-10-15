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

installHarness();

for (const scenario of ["identical", "dirty", "unknown", "wrong-custodian", "wrong-repo", "wrong-live-run", "malformed", "live", "retained", "registry", "late-writer"] as const) {
  it(`V2 owner cancellation preserves frozen inventory: ${scenario}`, { skip: HELD_LINUX_ONLY }, async t => {
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
      // Exercise the real client's authentication/binding validation, including refusals.
      return new Response(JSON.stringify({ run_id: claim.run_id,
        exact_hold: scenario === "unknown" ? undefined : {
          run_id: claim.run_id, generation: scenario === "malformed" ? "71" : 71, hold_id: holdId,
          repo_id: scenario === "wrong-repo" ? holdId : claim.repo.id, inventory_guarded: true,
          original_worker_id: scenario === "wrong-custodian" ? holdId : registration.worker_id,
          original_worker_identity: "original",
          live_worker_id: registration.worker_id, live_run_id: scenario === "wrong-live-run" ? holdId : claim.run_id,
          state: "open", run_status: server.failedAcked() && scenario !== "live" ? "cancelled" : "running",
          current_claim_generation: 71, claim_released: server.failedAcked(),
        },
      }), { status: 200 });
    });
    const send = client.reportState.bind(client);
    mock.method(client, "reportState", async (...args: Parameters<typeof send>) => {
      const ack = await send(...args);
      return args[1].status === "failed" ? { ...ack, status: "cancelled" } : ack;
    });
    skipCheckpoints();
    const freezes = watchFreezes(server);
    let models = 0;
    let clone = "";
    let dirtyBlob: string | undefined;
    let oldRoot: string | undefined;
    const executor: Executor = {
      async run(ctx) {
        if (ctx.cancelRequested?.()) throw new Error("run cancelled");
        models++;
        throw new Error("model must stay stopped");
      },
      killAgentTree() {},
      async settleForCredentialFreeCapture() { return scenario === "registry" ? { kind: "incomplete", errors: [] } : { kind: "observed_empty" }; },
    };
    const r = runner(executor, fakeGitlab().gitlab, JOIN_TOKEN, {
      quiesceRun: async req => ({
        process: { state: "quiescent", processes: [], killed: scenario === "late-writer" && req.site === "owner_cancel_after_inspection" ? [123] : [], detail: "fixture empty process registry" },
        docker: { state: "not_wired", removed: [], detail: "fixture has no Docker" },
      }),
    });
    // Cancel after clone creation, before executor/model admission. This also exercises
    // API-only cancellation, with no fabricated steering input receipt.
    let flight: ReturnType<typeof r["buildFlight"]>;
    const build = r["buildFlight"].bind(r);
    r["buildFlight"] = (...args) => flight = build(...args);
    const ensure = r["runnerCloneForClaim"].bind(r);
    r["runnerCloneForClaim"] = async (...args) => {
      const out = await ensure(...args);
      clone = out.path;
      if (scenario === "dirty") {
        oldRoot = commit(clone, "earlier-root.txt");
        fs.appendFileSync(path.join(clone, "README.md"), "tracked change\n");
        fs.writeFileSync(path.join(clone, "untracked.txt"), "untracked change\n");
        dirtyBlob = command(clone, ["hash-object", "untracked.txt"]);
      }
      if (scenario === "retained") flight.retainedEpisode = true;
      r["observeCancellation"](flight);
      return out;
    };
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    await r.execute(claim);
    assert.equal(models, 0);
    assert.equal(api.states.filter(s => s.body.status === "failed").length, 1);
    const positive = scenario === "identical" || scenario === "dirty";
    assert.equal(server.heldCalls.length, positive ? 1 : 0);
    assert.equal(capture.mock.callCount(), 0, "no archive before positive exact terminal authority");
    assert.equal(server.archiveReserves(), 0);
    if (positive) {
      const records = await frozenRecords(claim);
      assert.equal(records.length, 1);
      assert.ok(freezes.failedAckedAtFreeze.length > 0);
      assert.ok(freezes.failedAckedAtFreeze.every(acked => !acked));
      assert.equal(records[0]!.finalRequest?.disposition.kind, "held_publication");
      assert.equal(server.heldCalls[0]!.tip, records[0]!.sourceSha);
      const objects = packObjects(server.heldCalls[0]!.pack);
      if (dirtyBlob) assert.ok(objects.has(dirtyBlob));
      if (oldRoot) assert.ok(objects.has(oldRoot));
    } else {
      assert.ok(fs.existsSync(clone), "refused cancellation keeps the source");
      assert.equal(server.finals.length, 0);
    }
  });
}
