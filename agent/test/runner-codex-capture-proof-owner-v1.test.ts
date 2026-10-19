import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { JOIN_TOKEN, command, recoveryOf } from "./runner-held-publication-fixture.js";

installHarness();

for (const missing of [false, true]) {
  it(`A owner V1 ${missing ? "MISSING_SCOPE" : "accepted"}`, async t => {
    const claim = gitlabClaim(30301, { claim_generation: 7 });
    claim.repo.default_branch = "main";
    claim.secrets.codex = { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" };
    command(fx.originPath, ["branch", "agent/issue-30301", "main"]);
    client.protocolFeatures = ["recovery_release_exact_echo"];
    const sourceFilter: { sourcePath?: string } = {};
    const proof = observe(sourceFilter);
    t.after(() => proof.restore());

    let budget: MaintenanceBudget | undefined;
    let originalDeadline = 0;
    let modelSignal: AbortSignal;
    let cloneIdentity: fs.Stats, sourceBytes: Buffer, sourcePath = "";
    let requestMs = 0, requestAt = 0, contextDeadline = 0, originalRemaining = 0;
    let capturePurpose: string | undefined, captureSignal: AbortSignal | undefined;
    let heldContext: Parameters<Parameters<typeof proof.capture>[1]>[0] | undefined;
    const budgetRows: Array<{
      budget: unknown; deadline: number | undefined; signal: AbortSignal | undefined;
      aborted: boolean | undefined; modelAborted: boolean; contextDeadline: number | undefined;
    }> = [];
    const recordBudget = () => budgetRows.push({
      budget: MaintenanceBudget.current(), deadline: MaintenanceBudget.current()?.deadline,
      signal: MaintenanceBudget.signal(), aborted: MaintenanceBudget.signal()?.aborted,
      modelAborted: modelSignal?.aborted, contextDeadline: heldContext?.deadlineAt,
    });
    const events: Array<{ name: string; state: unknown; value?: unknown }> = [];
    const snapshot = () => {
      recordBudget();
      proof.recordSource(sourceFilter.sourcePath);
      return proof.observations().source.at(-1);
    };
    const capture: typeof proof.capture = (ms, action, purpose, signal) => {
      requestMs = ms;
      requestAt = Date.now();
      originalRemaining = budget?.remainingMs() ?? 0;
      capturePurpose = purpose;
      captureSignal = signal;
      return proof.capture(ms, async context => {
        heldContext = context;
        contextDeadline = context.deadlineAt;
        recordBudget();
        return action(context);
      }, purpose, signal);
    };

    const clean = git.credentialFreeCancelCleanHead.bind(git);
    git.credentialFreeCancelCleanHead = async (...args) => {
      proof.recordSource(args[0]);
      events.push({ name: "clean-entry", state: snapshot() });
      const out = await clean(...args);
      events.push({ name: "clean-return", state: snapshot(), value: out });
      return out;
    };
    const fetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
      if (String(input).endsWith(`/runs/${claim.run_id}/archives/release`)) {
        events.push({ name: "release-http", state: snapshot(), value: JSON.parse(String(init?.body)) });
        return Response.json({ run_id: claim.run_id, generation: 7, released: true, holds_released: 1, retained: false });
      }
      return fetch(input, init);
    });
    const release = client.releaseRecoveryCustody.bind(client);
    client.releaseRecoveryCustody = async (...args) => {
      const out = await release(...args);
      events.push({ name: "release-ack", state: snapshot(), value: out });
      return out;
    };
    const pin = git.deleteRecoveryPin.bind(git);
    git.deleteRecoveryPin = async (...args) => {
      await pin(...args);
      events.push({ name: "pin-delete", state: snapshot() });
    };
    const executor: Executor = {
      withCredentialFreeCapture: missing ? undefined : capture,
      settleForCredentialFreeCapture: proof.rig.settle,
      async run(ctx) {
        sourceFilter.sourcePath = sourcePath = ctx.worktreePath;
        modelSignal = ctx.signal!;
        cloneIdentity = fs.statSync(sourcePath);
        sourceBytes = fs.readFileSync(path.join(sourcePath, "README.md"));
        proof.arm();
        api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
        await new Promise<void>(resolve => modelSignal.addEventListener("abort", () => resolve(), { once: true }));
        throw new Error("run cancelled");
      },
      killAgentTree() {},
    };
    const r = runner(executor, fakeGitlab().gitlab, JOIN_TOKEN, {
      cancellationBudgetFactory: () => {
        budget = new MaintenanceBudget(600000);
        originalDeadline = budget.deadline;
        return budget;
      },
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "fixture" },
        docker: { state: "not_wired", removed: [], detail: "fixture" },
      }),
    });
    let flight: ReturnType<typeof r["buildFlight"]>;
    const build = r["buildFlight"].bind(r);
    r["buildFlight"] = (...args) => flight = build(...args);
    const recovery = recoveryOf(r);
    const forget = recovery.forgetGeneration.bind(recovery);
    recovery.forgetGeneration = async (...args) => {
      await forget(...args);
      events.push({ name: "forget", state: snapshot() });
    };

    await proof.execute(r.execute(claim));

    if (missing) {
      const retained = fs.statSync(sourcePath);
      assert.equal(retained.ino, cloneIdentity!.ino, "exact source clone inode retained");
      assert.equal(retained.dev, cloneIdentity!.dev, "exact source clone device retained");
      assert.deepEqual(fs.readFileSync(path.join(sourcePath, "README.md")), sourceBytes!, "source bytes retained");
      assert.equal(events.filter(x => ["clean-entry", "release-http", "pin-delete"].includes(x.name)).length, 0,
        "missing scope forbids source/release/pin operations");
      return;
    }
    proof.assertProof(false);
    assert.equal(budget, flight!.cancellationBudget, "factory budget is the original flight budget");
    assert.equal(capturePurpose, "cancel");
    assert.equal(captureSignal, budget?.signal, "capture inherits original cancellation budget signal");
    assert.ok(heldContext);
    assert.notEqual(heldContext.signal, modelSignal!);
    assert.ok(budgetRows.length > 0);
    for (const row of budgetRows) {
      assert.equal(row.budget, budget, "original flight budget retained through reader/release ACK/pin");
      assert.equal(row.deadline, originalDeadline, "original monotonic deadline never renewed");
      assert.ok(row.signal);
      assert.notEqual(row.signal, modelSignal!);
      assert.equal(row.aborted, false, "maintenance lineage remains live after model cancellation");
      assert.equal(row.modelAborted, true);
      assert.equal(row.contextDeadline, contextDeadline, "capture wall-clock deadline stable at operations");
    }
    assert.ok(requestMs > 0 && requestMs <= 600000);
    assert.ok(requestMs <= originalRemaining + 5, "capture consumes original remaining budget");
    assert.ok(contextDeadline <= requestAt + requestMs + 5, "capture request does not renew its wall-clock cap");
    assert.equal(modelSignal!.aborted, true);
    for (const name of ["clean-entry", "clean-return", "release-http", "release-ack", "forget", "pin-delete"]) {
      const row = events.find(x => x.name === name);
      assert.ok(row, `actual ${name} occurred`);
      assert.equal((row.state as { active: boolean }).active, true, `${name} remains held`);
    }
    assert.ok(events.find(x => x.name === "clean-return")?.value, "clean helper accepted real source");
  });
}
