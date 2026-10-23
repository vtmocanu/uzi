import { it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import type { Executor } from "../src/executor.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, command, commit, frozenRecords, heldServer, packObjects,
  recoveryOf, skipCheckpoints, watchFreezes,
} from "./runner-held-publication-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";

installHarness();

for (const variant of ["accepted", "MISSING_SCOPE", "root refusal"]) {
  const missing = variant === "MISSING_SCOPE";
  const refusal = variant === "root refusal";
  it(`A owner V2 ${variant}`, { skip: HELD_LINUX_ONLY }, async t => {
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
    const sourceFilter: { sourcePath?: string; refusal?: boolean } = { refusal };
    const proof = observe(sourceFilter);
    t.after(() => proof.restore());
    const freezesHeld: unknown[] = [];
    let budget: MaintenanceBudget | undefined, originalDeadline = 0;
    let cloneIdentity: fs.Stats, sourceBytes: Buffer;
    let requestMs = 0, requestAt = 0, contextDeadline = 0, originalRemaining = 0;
    let captureSignal: AbortSignal | undefined;
    let capturePurpose: string | undefined;
    let heldContext: Parameters<Parameters<typeof proof.capture>[1]>[0] | undefined;
    const releases = mock.method(client, "releaseRecoveryCustody");
    let ownerPhase = false;
    const operations: Array<{
      name: string; active: boolean; budget: unknown; deadline: number | undefined;
      signal: AbortSignal | undefined; aborted: boolean | undefined; modelAborted: boolean;
      contextDeadline: number | undefined;
    }> = [];
    const record = (name: string) => operations.push({
      name, active: proof.isActive(), budget: MaintenanceBudget.current(),
      deadline: MaintenanceBudget.current()?.deadline, signal: MaintenanceBudget.signal(),
      aborted: MaintenanceBudget.signal()?.aborted, modelAborted: flight.cancel.signal.aborted,
      contextDeadline: heldContext?.deadlineAt,
    });
    const scoped: typeof proof.capture = (ms, action, purpose, signal) => {
      requestMs = ms; requestAt = Date.now(); captureSignal = signal; capturePurpose = purpose;
      originalRemaining = budget?.remainingMs() ?? 0;
      return proof.capture(ms, async context => {
        contextDeadline = context.deadlineAt;
        heldContext = context;
        record("callback-entry");
        return action(context);
      }, purpose, signal);
    };
    const executor: Executor = {
      withCredentialFreeCapture: missing ? undefined : scoped,
      async run(ctx) {
        if (ctx.cancelRequested?.()) throw new Error("run cancelled");
        models++;
        throw new Error("model must stay stopped");
      },
      killAgentTree() {},
      settleForCredentialFreeCapture: proof.rig.settle,
    };
    const r = runner(executor, fakeGitlab().gitlab, JOIN_TOKEN, {
      cancellationBudgetFactory: () => { budget = new MaintenanceBudget(600000); originalDeadline = budget.deadline; return budget; },
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
      cloneIdentity = fs.statSync(clone);
      sourceBytes = fs.readFileSync(path.join(clone, "README.md"));
      r["observeCancellation"](flight);
      sourceFilter.sourcePath = clone;
      proof.arm();
      return out;
    };
    const disposeOwner = r["disposeCredentialFreeOwnerCancel"].bind(r);
    r["disposeCredentialFreeOwnerCancel"] = async (...args) => {
      ownerPhase = true;
      try { return await disposeOwner(...args); }
      finally { ownerPhase = false; }
    };
    const fetchTracking = r["fetchTracking"].bind(r);
    r["fetchTracking"] = async (...args) => {
      if (args[2] !== clone || !ownerPhase) return fetchTracking(...args);
      record("fetchTracking-entry");
      const out = await fetchTracking(...args);
      record("fetchTracking-return");
      return out;
    };
    for (const name of ["verifyRunnerTrackingCovers", "worktreeHead", "anchorRecoveryHead"] as const) {
      const original = git[name].bind(git) as (...args:any[]) => Promise<any>;
      mock.method(git, name, async (...args:any[]) => {
        // Only the actual held owner transfer; later terminal/bare reads are outside A.
        const source = name === "anchorRecoveryHead" ? args[0] === flight?.barePath : args[name === "verifyRunnerTrackingCovers" ? 1 : 0] === clone;
        if (!source || !ownerPhase) return original(...args);
        record(name + "-entry");
        const out = await original(...args);
        record(name + "-return");
        return out;
      });
    }
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    const coordinator = recoveryOf(r);
    const freeze = coordinator.freezeInventory.bind(coordinator);
    coordinator.freezeInventory = async (...args) => {
      if (ownerPhase) record("freeze-entry");
      const out = await freeze(...args);
      if (ownerPhase) {
        record("freeze-return");
        proof.recordSource(clone);
        freezesHeld.push(proof.observations().source.at(-1));
      }
      return out;
    };
    await proof.execute(r.execute(claim));

    if (missing || refusal) {
      if (refusal) proof.assertRefusal();
      assert.equal(operations.length, 0, "refused capture performs no held transfer");
      assert.equal(fs.statSync(clone).ino, cloneIdentity!.ino);
      assert.equal(fs.statSync(clone).dev, cloneIdentity!.dev);
      assert.deepEqual(fs.readFileSync(path.join(clone, "README.md")), sourceBytes!);
      assert.equal(fs.readFileSync(path.join(clone, "untracked.txt"), "utf8"), "untracked change\n");
      assert.equal((await frozenRecords(claim)).length, 0, "refusal creates no frozen inventory");
      assert.equal(server.heldCalls.length, 0);
      assert.equal(capture.mock.callCount(), 0);
      assert.equal(releases.mock.callCount(), 0, "refusal never releases custody");
      assert.equal(command(clone, ["rev-parse", "HEAD"]), oldRoot, "exact original source head retained");
      assert.equal(proof.observations().source.length, 0, "missing scope forbids owner source reads");
      assert.equal(freezesHeld.length, 0, "missing scope forbids freeze");
      assert.ok(fs.existsSync(clone), "missing scope retains the source clone");
      return;
    }
    proof.assertProof(false);
    for (const name of ["fetchTracking", "verifyRunnerTrackingCovers", "worktreeHead", "anchorRecoveryHead", "freeze"]) {
      for (const phase of ["entry", "return"]) assert.ok(operations.some(x => x.name === name + "-" + phase), name + " actual " + phase);
    }
    for (const row of operations) {
      assert.equal(row.active, true, row.name + " held");
      assert.equal(row.budget, budget, row.name + " original flight budget");
      assert.equal(row.deadline, originalDeadline, row.name + " stable monotonic deadline");
      assert.ok(row.signal);
      assert.notEqual(row.signal, flight!.cancel.signal, "maintenance does not use model-aborted signal");
      assert.equal(row.aborted, false, "maintenance cancellation lineage remains live");
      assert.equal(row.modelAborted, true);
      assert.equal(row.contextDeadline, contextDeadline, "capture wall-clock deadline stable");
    }
    assert.equal(budget, flight!.cancellationBudget, "factory budget is the original flight budget");
    assert.equal(capturePurpose, "cancel");
    assert.equal(captureSignal, budget?.signal);
    assert.ok(heldContext);
    assert.notEqual(heldContext.signal, flight!.cancel.signal);
    assert.equal(flight!.cancel.signal.aborted, true);
    assert.ok(requestMs > 0 && requestMs <= 600000);
    assert.ok(requestMs <= originalRemaining + 5, "capture consumes original remaining budget");
    assert.ok(contextDeadline <= requestAt + requestMs + 5, "capture wall-clock cap not renewed");
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
