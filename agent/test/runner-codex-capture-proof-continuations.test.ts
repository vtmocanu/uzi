import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, commitInTree } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
import { JOIN_TOKEN } from "./runner-held-publication-fixture.js";
installHarness();

it("non-Codex accepted owner pause retains the public legacy continuation", async () => {
  const claim = { ...gitlabClaim(2859), claim_generation: 1, park_run_supported: true, auto_approve: false };
  api.parkRunGeneration = 1;
  api.workerPauseRefused = true;
  api.parkRunOwnership = { status: "paused", claim_generation: 1, hold_reason: null, claim_released: false };
  let result: unknown, head = "", clone = "", turns = 0, retained = "";
  const executor: Executor = { run: async ctx => {
    turns++; clone = ctx.worktreePath;
    commitInTree(clone, "LEGACY.txt", "legacy retained content\n");
    head = await git.branchTip(clone, ctx.branch) ?? "";
    result = await ctx.requestParkRun!({ completedCount: 1, total: 2 });
    retained = fs.readFileSync(path.join(clone, "LEGACY.txt"), "utf8");
    return { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } };
  } };
  const r = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, JOIN_TOKEN,
    undefined, { checkpointIntervalMs: 0 });
  await r.execute(claim);
  assert.deepEqual(result, { kind: "paused" });
  assert.equal(turns, 1);
  assert.equal(retained, "legacy retained content\n");
  const records = await (r as any).recovery.inspect(claim.run_id);
  assert.ok(records.some((record: any) => record.sourceSha === head), "legacy route pins the actual clone head");
});

// Drive only public executor callbacks. Wrappers below delay real journal/settlement
// operations; none supplies authority, implements exclusion, or changes their result.
for (const route of ["uncertain-ACK", "guarded-question", "outer-owner-question"] as const) {
  for (const refusal of [false, true]) it(`owner pause continuation ${route} fresh-proof-${refusal ? "refused" : "accepted"}`, async t => {
    const o = observe();
    t.after(() => o.restore());
    const claim = { ...gitlabClaim(2858), claim_generation: 1, park_run_supported: true, auto_approve: false };
    api.parkRunGeneration = 1;
    api.parkRunOwnership = { status: route === "uncertain-ACK" ? "paused" : "running",
      claim_generation: 1, hold_reason: null, claim_released: false };
    if (route === "guarded-question") api.guardedQuestionStatus = "paused";
    let continuing = false, captures = 0, continuationCaptures = 0, turns = 0, attempts = 0;
    let clone = "", head = "", result: unknown, pausedAcks = 0, questions = 0;
    let beforeAuth = { refresh: 0, release: 0 };
    const sources: string[] = [], pins: unknown[] = [], inventories: unknown[] = [];
    const questionIds: string[] = [];
    const budgets: { at: number; ms: number; continuation: boolean; deadline?: number }[] = [];
    let beforeJournal: unknown, cloneInode = 0;
    const mark = async () => {
      if (continuing) return;
      continuing = true; o.arm();
      beforeAuth = { refresh: o.rig.refreshCalls(), release: o.rig.releaseCalls() };
      cloneInode = fs.statSync(clone).ino;
      beforeJournal = await recovery.inspect(claim.run_id);
    };
    const branchTip = git.branchTip.bind(git);
    t.mock.method(git, "branchTip", async (...args: Parameters<typeof branchTip>) => {
      if (continuing && args[0] === clone) {
        sources.push(args[0]);
        o.recordSource(clone);
      }
      return branchTip(...args);
    });
    const request = client.requestWorkerPause.bind(client);
    t.mock.method(client, "requestWorkerPause", async (...args: Parameters<typeof request>) => {
      attempts++;
      return request(...args);
    });
    const ownership = client.getRunOwnership.bind(client);
    t.mock.method(client, "getRunOwnership", async (...args: Parameters<typeof ownership>) => {
      const value = await ownership(...args);
      if (route === "uncertain-ACK" && args[1] && "deadline" in args[1]) await mark();
      return value;
    });
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      const body = args[1];
      const ack = await report(...args);
      if (body.status === "paused") {
        pausedAcks++;
        if (route === "uncertain-ACK") throw new Error("test lost accepted pause ACK");
      }
      if (body.status === "awaiting_input") {
        questions++;
        questionIds.push(body.open_question_id!);
        if (route === "guarded-question") await mark();
      }
      if (route === "outer-owner-question" && body.status === "running" && questions) {
        await mark();
        return { ...ack, status: "paused", holdReason: null };
      }
      return ack;
    });
    if (route === "outer-owner-question") api.onState(claim.run_id, state => {
      if (state.status === "awaiting_input") api.setInputs(claim.run_id, [{
        id: 1, kind: "answer", body: JSON.stringify({ question_id: state.open_question_id, answers: ["continue"] }),
      }]);
    });
    const capture: typeof o.capture = async (ms, action, purpose, signal) => {
      captures++;
      const row = { at: Date.now(), ms, continuation: continuing, deadline: undefined as number | undefined };
      budgets.push(row);
      if (continuing) continuationCaptures++;
      if ((continuing && refusal) || (!continuing && route === "guarded-question")) return { kind: "incomplete", errors: [{ category: "tool", message: "test initial continuation proof refused" }] };
      return o.capture(ms, async held => { row.deadline = held.deadlineAt; return action(held); }, purpose, signal);
    };
    const behavior: Executor["run"] = async ctx => {
      turns++; clone = ctx.worktreePath;
      commitInTree(clone, "CONTINUATION.txt", "exact owner pause content\n");
      head = await branchTip(clone, ctx.branch) ?? "";
      fs.writeFileSync(path.join(homeDir, "owner-session"), "original session");
      // Startup inventory is unrelated to the continuation under test.
      claim.inventory_guarded = true;
      if (route === "outer-owner-question") {
        await ctx.askUser!([{ header: "Owner", question: "Continue?" }]);
        throw new Error("owner-paused question must stop the model");
      }
      result = await ctx.requestParkRun!({ completedCount: 1, total: 2 });
      return { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } };
    };
    const executor = new FakeCodexExecutor(o.rig.safety, behavior, o.rig.settle, capture);
    const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, JOIN_TOKEN,
      undefined, { checkpointIntervalMs: 0 });
    const internal = runner as any;
    const recovery = internal.recovery;
    const pin = recovery.pin.bind(recovery);
    t.mock.method(recovery, "pin", async (...args: any[]) => {
      if (!continuing) return pin(...args);
      const value = await pin(...args);
      pins.push(value);
      await o.hold("last-pin");
      assert.equal(o.isActive(), true, "fresh capture remains held through awaited journal pin");
      return value;
    });
    const settle = internal.settleGuardedInventory.bind(runner);
    t.mock.method(internal, "settleGuardedInventory", async (...args: any[]) => {
      const value = await settle(...args);
      if (continuing) {
        inventories.push({ active: o.isActive(), signal: args[4]?.aborted, locallyQuiescent: args[5] });
        await o.hold("guarded-settlement-tail");
        assert.equal(o.isActive(), true, "fresh capture remains held through final guarded settlement await");
      }
      return value;
    });
    await o.execute(runner.execute(claim));
    console.log("CONTINUATION", JSON.stringify({ route, refusal, captures, continuationCaptures, attempts,
      turns, pausedAcks, questions, questionIds, sourceReads: sources.length, pins, inventories, budgets }));
    assert.equal(continuing, true, "the public continuation was reached");
    assert.equal(turns, 1, "no new provider turn");
    assert.equal(pausedAcks, route === "uncertain-ACK" ? 1 : 0, "continuation fabricates no new pause ACK");
    assert.equal(questions, route === "uncertain-ACK" ? 0 : 1, "no replacement owner question");
    assert.ok(questionIds.every(id => /^[0-9a-f-]{36}$/.test(id)), "question retains a real API identity");
    assert.equal(attempts, route === "outer-owner-question" ? 0 : 1, "one attempt per consumed park");
    if (route !== "outer-owner-question") assert.deepEqual(result, { kind: "paused" });
    assert.equal(o.rig.refreshCalls(), beforeAuth.refresh, "continuation makes no auth refresh");
    assert.equal(o.rig.releaseCalls(), beforeAuth.release, "continuation makes no auth release");
    assert.equal(fs.statSync(clone).ino, cloneInode, "exact clone retained");
    assert.equal(fs.readFileSync(path.join(clone, "CONTINUATION.txt"), "utf8"), "exact owner pause content\n");
    assert.equal(fs.readFileSync(path.join(homeDir, "owner-session"), "utf8"), "original session");
    assert.equal(await branchTip(clone, "agent/issue-2858"), head, "no new WIP or fabricated capture");
    assert.equal(continuationCaptures, 1, "a fresh proof is required even after an earlier capture");
    if (refusal) {
      assert.deepEqual(sources, [], "initial refusal performs no source read");
      assert.deepEqual(pins, [], "initial refusal performs no journal pin");
      assert.deepEqual(inventories, [], "initial refusal performs no guarded inventory settlement");
      o.assertRefusal();
      assert.deepEqual(await recovery.inspect(claim.run_id), beforeJournal,
        "initial refusal preserves the exact journal records and capture identities");
    } else {
      assert.ok(sources.length > 0, "actual clone branchTip executed");
      assert.equal(pins.length, 1);
      assert.equal((pins[0] as any)?.sourceSha, head, "real journal pins the actual clone head");
      assert.equal(inventories.length, 1, "guarded settlement awaited");
      assert.equal((inventories[0] as any).active, true);
      assert.equal((inventories[0] as any).locallyQuiescent, true);
      o.assertProof(false);
    }
    assert.ok(Array.isArray(beforeJournal));
    if (route === "uncertain-ACK" && !refusal) {
      assert.ok(budgets[1]!.deadline! <= budgets[0]!.deadline!,
        "uncertain ACK continuation must preserve the original absolute capture deadline");
    }
  });
}
