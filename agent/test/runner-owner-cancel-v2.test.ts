import { it, mock } from "node:test";
import { codexRig } from "./runner-codex-sinks-fixture.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { RequestError } from "../src/client.js";
import type { Executor } from "../src/executor.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, command, commit, frozenRecords, heldServer, packObjects,
  recoveryOf, skipCheckpoints, watchFreezes,
} from "./runner-held-publication-fixture.js";

installHarness();

for (const scenario of ["wallpark", "wallpark-healthy", "wallpark-probe", "clarification", "old-generation-foreground", "late-quiescence", "identical", "dirty", "closed", "unknown", "wrong-custodian", "wrong-repo", "wrong-live-run", "malformed", "live", "retained", "registry", "late-writer"] as const) {
  const name = scenario === "identical"
    ? "#2901 before-model cancelled identical-tree inventory with no messages or checkpoint"
    : `V2 owner cancellation preserves frozen inventory: ${scenario}`;
  it(name, { skip: HELD_LINUX_ONLY }, async t => {
    const wallpark = scenario === "wallpark" || scenario === "wallpark-healthy" || scenario === "wallpark-probe";
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
    let foreground = false;
    const fetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
      if (!String(input).endsWith(`/runs/${claim.run_id}/recovery-holds?generation=71`))
        return fetch(input, init);
      const closed = scenario === "closed" && server.failedAcked();
      // Exercise the real client's authentication/binding validation, including refusals.
      return new Response(JSON.stringify({ run_id: claim.run_id,
        exact_hold: (scenario === "unknown" || (scenario === "old-generation-foreground" && !foreground)) ? undefined : {
          run_id: claim.run_id, generation: scenario === "malformed" ? "71" : 71, hold_id: holdId,
          repo_id: scenario === "wrong-repo" ? holdId : claim.repo.id, inventory_guarded: true,
          original_worker_id: scenario === "wrong-custodian" ? holdId : registration.worker_id,
          original_worker_identity: "original",
          ...(closed ? { state: "discarded", release_evidence: "owner_discard" }
            : { state: "open", live_worker_id: registration.worker_id,
              live_run_id: scenario === "wrong-live-run" ? holdId : claim.run_id }),
          run_status: server.failedAcked() && scenario !== "live" ? (scenario === "old-generation-foreground" ? "failed" : "cancelled") : "running",
          current_claim_generation: foreground ? 72 : 71, ...(foreground ? { run_worker_id: holdId } : {}), claim_released: server.failedAcked(),
        },
      }), { status: 200 });
    });
    const send = client.reportState.bind(client);
    mock.method(client, "reportState", async (...args: Parameters<typeof send>) => {
      const ack = await send(...args);
      return args[1].status === "failed" && scenario !== "old-generation-foreground" ? { ...ack, status: "cancelled" } : ack;
    });
    skipCheckpoints();
    const checkpoint = mock.method(client, "publishCheckpoint");
    const freezes = watchFreezes(server);
    let models = 0, mono = 0, factories = 0;
    let budget: MaintenanceBudget | undefined, observedBudget: MaintenanceBudget | undefined;
    let lateTriggered = false;
    const lateReads: string[] = [];
    const status = git.worktreeStatus.bind(git);
    mock.method(git, "worktreeStatus", async (...args: Parameters<typeof status>) => {
      if (scenario === "late-quiescence" && mono >= 600_000) lateReads.push(args[0]);
      return status(...args);
    });
    let clone = "", identicalTree = "";
    let dirtyBlob: string | undefined;
    let oldRoot: string | undefined;
    const rig = codexRig();
    let capturePending = false, boundaryPending = false;
    const cancelAdmissions: Array<{ capturePending: boolean; boundaryPending: boolean }> = [];
    const withBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      boundaryPending = true;
      try { return await withBoundary(request, action); }
      finally { boundaryPending = false; }
    };
    const executor: Executor = {
      ...(scenario === "wallpark-healthy" ? { safety: rig.safety } : {}),
      withCredentialFreeCapture: scenario === "registry"
        ? async () => ({ kind: "incomplete", errors: [] }) : async (ms, action, purpose, signal, attempt) => {
          if (purpose === "cancel") {
            cancelAdmissions.push({ capturePending, boundaryPending });
            assert.equal(capturePending, false, "independent cancel capture follows full capture-scope join");
            assert.equal(boundaryPending, false, "independent cancel capture follows full healthy-boundary join");
          }
          capturePending = true;
          try { return await rig.capture(ms, action, purpose, signal, attempt); }
          finally { capturePending = false; }
        },
      async run(ctx) {
        if (ctx.cancelRequested?.()) throw new Error("run cancelled");
        models++;
        if (wallpark || scenario === "clarification") {
          oldRoot = commit(ctx.worktreePath, "before-cancel.txt");
          fs.writeFileSync(path.join(ctx.worktreePath, "cancel-dirty.txt"), "work before route cancellation\n");
          dirtyBlob = command(ctx.worktreePath, ["hash-object", "cancel-dirty.txt"]);
          if (wallpark) {
            if (scenario === "wallpark-probe") {
              mock.method(client, "reportWallPark", async () => {
                const ownership = client.getRunOwnership.bind(client);
                mock.method(client, "getRunOwnership", async (...args: Parameters<typeof ownership>) =>
                  ({ ...await ownership(...args), status: "cancelled" as const }));
                return { applied: false, status: "running" };
              });
            } else api.setWallParkResponse("cancelled");
            assert.equal(await ctx.parkForWall!({ completedCount: 1 }), "cancelled");
          } else {
            api.onState(claim.run_id, state => {
              if (state.status === "awaiting_input") api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
            });
            assert.equal((await ctx.askUser!([{
              question: "Continue?", header: "Choice", options: [{ label: "Continue", description: "Continue work" }],
              multiSelect: false,
            }])).kind, "cancel");
          }
          assert.equal(ctx.signal?.aborted, true, "route cancellation stops the model before returning");
          throw new Error("run cancelled");
        }
        throw new Error("model must stay stopped");
      },
      killAgentTree() {},
      async settleForCredentialFreeCapture() { return scenario === "registry" ? { kind: "incomplete", errors: [] } : { kind: "observed_empty" }; },
    };
    const r = runner(executor, fakeGitlab().gitlab, JOIN_TOKEN, {
      cancellationBudgetFactory: scenario === "late-quiescence" ? () => {
        factories++; return budget = new MaintenanceBudget(600_000, () => mono);
      } : undefined,
      quiesceRun: async req => {
        if (scenario === "late-quiescence" && req.site === "terminal_retire" && !lateTriggered) {
          lateTriggered = true;
          r["observeCancellation"](flight);
          observedBudget = MaintenanceBudget.current();
          // The signal-ignoring quiescence result arrives after the same attempt expires.
          mono = 600_001;
        }
        return {
          process: { state: "quiescent", processes: [], killed: scenario === "late-writer" && req.site === "owner_cancel_after_inspection" ? [123] : [], detail: "fixture empty process registry" },
          docker: { state: "not_wired", removed: [], detail: "fixture has no Docker" },
        };
      },
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
      if (scenario === "identical") {
        identicalTree = command(clone, ["rev-parse", "HEAD^{tree}"]);
        assert.deepEqual(api.messages(claim.run_id), [], "no messages before cancellation observation");
      }
      if (scenario === "retained") flight.retainedEpisode = true;
      if (scenario !== "old-generation-foreground" && scenario !== "late-quiescence" && !wallpark && scenario !== "clarification") r["observeCancellation"](flight);
      return out;
    };
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    await r.execute(claim);
    if (wallpark) assert.deepEqual(cancelAdmissions, [{ capturePending: false, boundaryPending: false }],
      "exactly one independent cancellation producer starts after the prior scope fully returns");
    assert.equal(models, scenario === "old-generation-foreground" || scenario === "late-quiescence" || wallpark || scenario === "clarification" ? 1 : 0);
    assert.equal(api.states.filter(s => s.body.status === "failed").length, 1);
    if (scenario === "old-generation-foreground") {
      assert.equal(server.heldCalls.length, 0);
      assert.equal(server.finals.length, 0);
      foreground = true;
      flight!.heldDeferredCapture = undefined;
      const reserves = server.archiveReserves();
      const ownershipBody = JSON.stringify({ error: "run not found for this worker" });
      client.getRunOwnership = async () => { throw new RequestError("GET", `/api/worker/runs/${claim.run_id}/ownership`, 404,
        ownershipBody, undefined, { complete: true, bytes: Buffer.byteLength(ownershipBody) }); };
      await r["settleRecoveryGeneration"](claim, flight!, flight!.runLog);
      assert.equal(server.heldCalls.length, 0, "persisted self-contained archive precedes new held production");
      assert.equal(server.finals.length, 1, "original custody settles despite current g72/foreign worker and ownership404");
      assert.equal(server.finals[0]!.generation, 71);
      assert.equal(server.finals[0]!.disposition?.kind, "archive");
      assert.equal(server.archiveReserves(), reserves, "reuse existing archive identity");
      assert.equal(server.acked(), 1);
      return;
    }
    if (scenario === "late-quiescence") {
      assert.equal(lateTriggered, true);
      assert.equal(factories, 1);
      assert.ok(budget);
      assert.equal(observedBudget, budget, "cancellation attaches to the already-awaiting flight continuation");
      assert.deepEqual(lateReads, [], "late quiescence cannot start fresh source inspection");
      assert.equal(server.failedAcked(), true, "honest terminal status transport remains independent of preservation expiry");
    }
    const positive = scenario === "identical" || scenario === "dirty" || wallpark || scenario === "clarification";
    assert.equal(server.heldCalls.length, positive ? 1 : 0);
    assert.equal(capture.mock.callCount(), 0, "no archive before positive exact terminal authority");
    assert.equal(server.archiveReserves(), 0);
    if (positive) {
      if (wallpark) {
        assert.equal(capturePending, false);
        assert.equal(boundaryPending, false);
      }
      const records = await frozenRecords(claim);
      assert.equal(records.length, 1);
      assert.ok(freezes.failedAckedAtFreeze.length > 0);
      assert.ok(freezes.failedAckedAtFreeze.every(acked => !acked));
      assert.equal(records[0]!.finalRequest?.disposition.kind, "held_publication");
      assert.equal(server.heldCalls[0]!.tip, records[0]!.sourceSha);
      const objects = packObjects(server.heldCalls[0]!.pack);
      if (scenario === "identical") {
        assert.equal(checkpoint.mock.callCount(), 0, "no checkpoint preceded held preservation");
        assert.equal(command(records[0]!.coverageContext!.barePath,
          ["rev-parse", `${records[0]!.sourceSha}^{tree}`]), identicalTree, "immutable capture has the identical pre-model tree");
        // The executing-flight guard retains FINAL until the real execution tail closes.
        assert.equal(server.acked(), 0);
        await recoveryOf(r).resumePending();
        assert.equal(server.heldCalls.length, 1, "reconciliation reuses the admitted publication");
        assert.equal(server.archiveReserves(), 0);
        assert.equal(server.acked(), 1, "exact held FINAL settles the cancellation");
        assert.equal(server.finals[0]!.generation, 71);
      }
      if (dirtyBlob) assert.ok(objects.has(dirtyBlob));
      if (oldRoot) assert.ok(objects.has(oldRoot));
    } else {
      assert.ok(fs.existsSync(clone), "refused cancellation keeps the source");
      assert.equal(server.finals.length, 0);
      if (scenario === "closed") {
        const records = await frozenRecords(claim);
        assert.equal(records.length, 1);
        assert.equal(records[0]!.holdSettlement?.state, "discarded");
        assert.notEqual(records[0]!.finalAcknowledged, true, "a known settlement is not a fabricated ACK");
      }
    }
  });
}
