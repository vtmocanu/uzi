import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { acknowledgedCompletionInventory } from "./runner-completion-inventory-fixture.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, GIT_ENV, manualDeadline, commitInTree, statuses } from "./runner-codex-sinks-fixture.js";

installHarness();

for (const branch of ["permit", "prepush"] as const) {
  for (const failure of ["healthy", "direct", "poison", "healthy-expiry", "parent-expiry"] as const) {
    it(`finalize ${branch} hold ${failure}: one disposition after owned unwind`, async t => {
      const deadline = manualDeadline("finalize");
      const rig = codexRig({ armDeadline: deadline.armDeadline });
      let inBoundary = false, joined = false, inCapture = false, injected = false, inFree = false;
      let freeCalls = 0, captures = 0, parentBudget = 0, pushes = 0, clone = "";
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) => {
        parentBudget = request.deadlineMs;
        try {
          return await boundary(request, async permit => {
            inBoundary = true;
            try { return await action(permit); } finally { inBoundary = false; }
          });
        } finally { joined = true; }
      });
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        clone = ctx.worktreePath;
        commitInTree(clone, "FINALIZE.txt", "finalize capture bytes\n");
        return { branch: ctx.branch, ...(branch === "prepush" ? { nonClosing: { reason: "remaining work" } } : {}) };
      }, undefined, (ms, action, purpose, signal, attempt) => {
        freeCalls++;
        assert.equal(joined, true, "fallback waits for original boundary settlement");
        assert.equal(inBoundary, false);
        return rig.capture(ms, async held => {
          inFree = true;
          try { return await action(held); } finally { inFree = false; }
        }, purpose, signal, attempt);
      });
      const { gitlab, all } = fakeGitlab();
      const run = runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      const privateRun = run as unknown as { captureHoldContext: (...args: unknown[]) => Promise<unknown> };
      const capture = privateRun.captureHoldContext.bind(run);
      t.mock.method(privateRun, "captureHoldContext", async (...args: unknown[]) => {
        captures++;
        inCapture = true;
        try {
          if (!injected && (failure === "parent-expiry" || failure === "healthy-expiry")) {
            injected = true;
            const expired = Date.now() + (failure === "parent-expiry" ? parentBudget : 120_000) + 1;
            t.mock.method(Date, "now", () => expired);
            if (failure === "parent-expiry") deadline.fire();
            else assert.equal(deadline.fired(), 0, "only the capture phase expires; its parent remains live");
          }
          return await capture(...args);
        } finally { inCapture = false; }
      });
      const status = git.worktreeStatus.bind(git);
      t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof status>) => {
        if (inCapture) {
          if (!injected && (failure === "direct" || failure === "poison")) {
            injected = true;
            if (failure === "poison") rig.registry.poison({ category: "tool", message: "capture root incomplete" });
            throw new TrustedExecutionRefusal("capture phase refused before local proof");
          }
          assert.ok(inBoundary || inFree, "capture source has live parent or free proof");
        }
        return status(...args);
      });
      const push = git.pushBranch.bind(git);
      t.mock.method(git, "pushBranch", async (...args: Parameters<typeof push>) => { pushes++; return push(...args); });
      if (branch === "prepush") t.mock.method(git, "publishedCommitMessages", async () => null);
      api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
      await client.register("finalize-capture");
      const iid = 285340 + ["permit", "prepush"].indexOf(branch) * 10 + ["healthy", "direct", "poison", "healthy-expiry", "parent-expiry"].indexOf(failure);
      const claim = gitlabClaim(iid, { claim_generation: 7, config: { completion_contract_version: 1, contract_revision: 1 } });
      api.setPublicationState(claim.run_id, [], 1);
      api.setCompletionPermitResponse(false, { denyReason: "incomplete" });
      api.setCompletionHoldResponse("paused");
      await run.execute(claim);
      assert.deepEqual(rig.boundaries, ["finalize"], "no nested/repeated authenticated boundary");
      assert.deepEqual(all, [], "a held delivery opens no MR");
      assert.equal(pushes, branch === "permit" ? 1 : 0, "continuation does not repeat publication");
      assert.ok(fs.existsSync(clone));
      assert.equal(fs.readFileSync(path.join(clone, "FINALIZE.txt"), "utf8"), "finalize capture bytes\n");
      if (failure === "parent-expiry") {
        assert.equal(freeCalls, 0, "parent expiry cannot renew the capture budget");
        assert.equal(api.completionHoldRequests.length, 0);
      } else {
        assert.equal(freeCalls, failure === "healthy" ? 0 : 1);
        assert.equal(captures, failure === "healthy" ? 1 : 2, "failed primary plus exactly one fallback");
        assert.equal(api.completionHoldRequests.length, 1);
        assert.ok(api.completionHoldRequests[0]!.body.head);
        assert.ok(!statuses(claim.run_id).some(s => ["failed", "completed"].includes(s)));
        const bare = git.barePathFor(fx.originPath);
        assert.equal(execFileSync("git", ["-C", bare, "show", `refs/uzi-runner/agent/issue-${iid}:FINALIZE.txt`], { env: GIT_ENV, encoding: "utf8" }), "finalize capture bytes\n");
      }
    });
  }
}

it("an unissued lookalike boundary error cannot resume a finalize hold", async t => {
  const rig = codexRig();
  let freeCalls = 0;
  const executor = new FakeCodexExecutor(rig.safety, async ctx => {
    commitInTree(ctx.worktreePath, "LOOKALIKE.txt", "unissued\n");
    return { branch: ctx.branch };
  }, undefined, (ms, action, purpose, signal, attempt) => {
    freeCalls++;
    return rig.capture(ms, action, purpose, signal, attempt);
  });
  const { gitlab } = fakeGitlab();
  const run = runnerWith(() => ({ executor }), gitlab);
  const privateRun = run as unknown as { phasePublish: (...args: unknown[]) => Promise<void> };
  const lookalike = Object.assign(new Error("unissued boundary wrapper"), {
    name: "CodexBoundaryError", actionError: new Error("capture refused"), cause: new Error("capture refused"),
  });
  t.mock.method(privateRun, "phasePublish", async () => { throw lookalike; });
  await run.execute(gitlabClaim(285359));
  assert.equal(freeCalls, 0);
  assert.equal(api.completionHoldRequests.length, 0);
});

for (const branch of ["permit", "prepush"] as const) for (const confirmed of [true, false]) {
  it(`finalize ${branch} continuation retires ${confirmed ? "confirmed" : "unresolved"} inventory once`, async t => {
    const generation = 8;
    const claim = gitlabClaim(285390 + Number(branch === "prepush") * 2 + Number(confirmed), {
      claim_generation: generation, config: { completion_contract_version: 1, contract_revision: 1 },
    });
    const inventory = acknowledgedCompletionInventory(claim.run_id, generation);
    const root = path.join(fx.dataDir, "finalize-capture-outbox");
    const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const file = path.join(root, claim.run_id, `finalize-${generation}.json`);
    const rig = codexRig();
    let joined = false, free = false, injected = false, captures = 0, retirements = 0;
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (...args: Parameters<typeof boundary>) => {
      try { return await boundary(...args); } finally { joined = true; }
    });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      commitInTree(ctx.worktreePath, "JOURNAL.txt", "journal capture\n");
      const head = await git.worktreeHead(ctx.worktreePath);
      assert.ok(head);
      await inventory.acknowledge(ctx, head);
      assert.equal((await outbox.journalFinalize(claim.run_id, generation)).written, true);
      return { branch: ctx.branch, ...(branch === "prepush" ? { nonClosing: { reason: "remaining work" } } : {}) };
    }, undefined, (ms, action, purpose, signal, attempt) => rig.capture(ms, async held => {
      captures++;
      assert.equal(joined, true);
      free = true;
      try { return await action(held); } finally { free = false; }
    }, purpose, signal, attempt));
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("capture-journal");
    api.setPublicationState(claim.run_id, [], 1);
    api.setCompletionPermitResponse(false, { denyReason: "incomplete" });
    api.setCompletionHoldResponse("paused");
    if (!confirmed) {
      api.completionHoldWire.set(claim.run_id, res => { res.writeHead(200); res.end(""); });
      api.ownershipWire.set(claim.run_id, res => {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ status: "running", claim_generation: generation, claim_released: false, inventory_guarded: true }));
      });
    }
    if (branch === "prepush") t.mock.method(git, "publishedCommitMessages", async () => null);
    const { gitlab } = fakeGitlab();
    const run = runnerWith(() => ({ executor }), gitlab, undefined, undefined,
      { outbox, recovery: inventory.coordinator, recoveryRetryMs: 1 });
    const internal = run as unknown as {
      captureHoldContext: (...args: unknown[]) => Promise<unknown>;
      retireFinalizeRecord: (...args: unknown[]) => Promise<void>;
    };
    const capture = internal.captureHoldContext.bind(run);
    t.mock.method(internal, "captureHoldContext", async (...args: unknown[]) => {
      if (!injected) {
        injected = true;
        assert.equal(free, false);
        rig.registry.poison({ category: "tool", message: "journal capture initial proof refused" });
        throw new TrustedExecutionRefusal("capture proof refused");
      }
      return capture(...args);
    });
    const retire = internal.retireFinalizeRecord.bind(run);
    t.mock.method(internal, "retireFinalizeRecord", async (...args: unknown[]) => {
      retirements++;
      return retire(...args);
    });
    await run.execute(claim);
    assert.equal(injected, true);
    assert.equal(captures, 1);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(retirements, confirmed ? 1 : 0);
    assert.equal(fs.existsSync(file), !confirmed, "unresolved hold keeps authenticated finalize evidence");
    assert.equal(await inventory.coordinator.inventoryCleanupState(claim.run_id, generation), "acknowledged");
  });
}
