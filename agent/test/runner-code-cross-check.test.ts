import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { WorkerClient } from "../src/client.js";
import type { Executor, RunContext } from "../src/executor.js";
import { checkCode } from "../src/code-cross-check-gate.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, runner, fakeGitlab, gitlabClaim, installHarness, planThenDoneQuery, input } from "./runner-harness.js";

installHarness();
const record = (head = "a".repeat(40), base = "b".repeat(40)) => ({
  stage: "code" as const, round: 1 as const, candidate_generation: 1, head_commit: head, base_commit: base,
  candidate_digest: "c".repeat(64), checker_run_id: null, checker_harness: "codex", checker_model: "test",
  checker_effort: null, outcome: "completed" as const, reason_class: null, findings: [],
  interrupted_at: null, finalized_at: new Date().toISOString(), deadline_at: new Date(Date.now() + 1000).toISOString(),
});
function commit(ctx: RunContext): string {
  fs.writeFileSync(path.join(ctx.worktreePath, "implementation.txt"), "committed work\n");
  execFileSync("git", ["-C", ctx.worktreePath, "add", "implementation.txt"]);
  execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "work"]);
  return execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
}

it("absent code consent omits the code gate from the runner context", async (t) => {
  const status = t.mock.method(client, "codeCrossCheckStatus", async () => { throw new Error("unexpected code call"); });
  const submit = t.mock.method(client, "submitCodeCrossCheck", async () => { throw new Error("unexpected code call"); });
  const pin = t.mock.method(git, "pinCodeSnapshot", async () => { throw new Error("unexpected code ref"); });
  let wired: unknown;
  const exec: Executor = { run: async (ctx) => { wired = ctx.codeCrossCheckGate; commit(ctx); return { branch: ctx.branch }; } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab).execute(gitlabClaim(2170));
  assert.equal(wired, undefined);
  assert.equal(status.mock.callCount(), 0);
  assert.equal(submit.mock.callCount(), 0);
  assert.equal(pin.mock.callCount(), 0);
  assert.ok(!JSON.stringify(api.messages(gitlabClaim(2170).run_id)).includes("code_cross_check_summary"));
});

it("new worker treats an old API claim with absent code consent as disabled", async (t) => {
  const status = t.mock.method(client, "codeCrossCheckStatus", async () => { assert.fail("old API code status"); });
  const submit = t.mock.method(client, "submitCodeCrossCheck", async () => { assert.fail("old API code submit"); });
  const pin = t.mock.method(git, "pinCodeSnapshot", async () => { assert.fail("old API pin"); });
  const inner = planThenDoneQuery();
  let turns = 0;
  let ctxForCommit: RunContext;
  const queryFn: SdkQueryFn = (params) => {
    if (++turns === 2) commit(ctxForCommit);
    return inner(params);
  };
  const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
  const executor: Executor = {
    run: async (ctx) => { ctxForCommit = ctx; assert.equal(ctx.codeCrossCheckGate, undefined); return sdk.run(ctx); },
  };
  const claim = gitlabClaim(2174);
  api.setInputs(claim.run_id, [input("approve_plan")]);
  const forge = fakeGitlab();
  await runner(executor, forge.gitlab).execute(claim);
  assert.equal(turns, 2);
  assert.equal(api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")?.body.status, "completed");
  assert.equal(status.mock.callCount(), 0);
  assert.equal(submit.mock.callCount(), 0);
  assert.equal(pin.mock.callCount(), 0);
  assert.equal(forge.calls.length, 1, "ordinary MR behavior is preserved");
  assert.ok(!JSON.stringify(forge.calls).includes("Code cross-check"));
  assert.ok(!JSON.stringify(api.messages(claim.run_id)).includes("code_cross_check_summary"));
});

it("legacy code gate snapshots committed work locally, ignoring dirty scratch and making no checkpoint publication", async (t) => {
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
  let expected = "";
  let expectedBase = "";
  let gateResult: unknown;
  let submitted: Parameters<WorkerClient["submitCodeCrossCheck"]>[2] | undefined;
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
    submitted = snapshot;
    return "head_commit" in snapshot ? record(snapshot.head_commit, snapshot.base_commit) : record();
  });
  const publish = t.mock.method(client, "publishCheckpoint", async () => { throw new Error("unexpected remote checkpoint"); });
  const exec: Executor = { run: async (ctx) => {
    expectedBase = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    expected = commit(ctx);
    fs.mkdirSync(path.join(ctx.worktreePath, ".uzi/scratch"), { recursive: true });
    fs.writeFileSync(path.join(ctx.worktreePath, ".uzi/scratch/dirty"), "uncommitted");
    gateResult = await ctx.codeCrossCheckGate!({ interlocked: false });
    return { branch: ctx.branch };
  } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab, undefined, { checkpointIntervalMs: 0 }).execute(gitlabClaim(2171, { code_cross_check_required: true, claim_generation: 1 }));
  assert.ok(submitted && "head_commit" in submitted);
  assert.equal(submitted.head_commit, expected, "submitted SHA must equal original committed H");
  assert.equal(submitted.base_commit, expectedBase, "submitted base must equal original default commit");
  assert.notEqual(expected, expectedBase);
  assert.deepEqual(gateResult, { action: "proceed" });
  assert.equal(publish.mock.callCount(), 0);
});

it("interrupted code check cannot resubmit or replay old findings", async (t) => {
  const previous = { ...record(), candidate_generation: 1, interrupted_at: new Date().toISOString(),
    findings: [{ id: "F1", severity: "major" as const, path: "old", line: 1, title: "old advice", detail: "do not replay" }] };
  const submit = t.mock.method(client, "submitCodeCrossCheck", async () => { throw new Error("resubmit"); });
  t.mock.method(client, "codeCrossCheckStatus", async () => previous);
  let snapshots = 0;
  const result = await checkCode({ client, runId: "lead", generation: 2, signal: new AbortController().signal,
    snapshot: async () => { snapshots++; return { reason_class: "snapshot_failed" }; } });
  assert.ok(result && !("result" in result));
  assert.deepEqual(result.findings, []);
  assert.equal(submit.mock.callCount(), 0);
  assert.equal(snapshots, 0);
});

it("deadline read lets the server settle pending timeout without another snapshot or POST", async () => {
  let reads = 0;
  let snapshots = 0;
  let posts = 0;
  const pending = { ...record(), outcome: "pending" as const, finalized_at: null,
    deadline_at: new Date(Date.now() + 10).toISOString() };
  const result = await checkCode({
    client: {
      codeCrossCheckStatus: async () => ++reads === 1 ? pending
        : { ...pending, outcome: "failed", finalized_at: new Date().toISOString(), reason_class: "timed_out" },
      submitCodeCrossCheck: async () => { posts++; return pending; },
    },
    runId: "lead", generation: 1, signal: new AbortController().signal, pollMs: 100, requestMs: 100,
    snapshot: async () => { snapshots++; return { reason_class: "snapshot_failed" }; },
  });
  assert.equal(reads, 2);
  assert.equal(posts, 0);
  assert.equal(snapshots, 0);
  assert.equal(result && !("result" in result) && result.reason_class, "timed_out");
});

it("unknown persisted status proceeds without snapshot or resubmission", async () => {
  let posts = 0, snapshots = 0;
  const result = await checkCode({
    client: {
      codeCrossCheckStatus: async () => { throw new Error("unknown status"); },
      submitCodeCrossCheck: async () => { posts++; throw new Error("unexpected POST"); },
    },
    runId: "lead", generation: 1, signal: new AbortController().signal,
    snapshot: async () => { snapshots++; throw new Error("unexpected snapshot"); },
  });
  assert.equal(result, undefined);
  assert.equal(posts, 0);
  assert.equal(snapshots, 0);
});

it("terminal no-child refusal releases only the locally captured snapshot", async (t) => {
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
  let head = "";
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
    assert.ok("head_commit" in snapshot);
    head = snapshot.head_commit;
    return { ...record(head, snapshot.base_commit), outcome: "failed", reason_class: "checker_unavailable" };
  });
  const remove = git.deleteCodeSnapshot.bind(git);
  const observations: { tip: string; checked: string; bare: string; id: string }[] = [];
  const deleted = t.mock.method(git, "deleteCodeSnapshot", async (bare: string, id: string, checked: string, proof?: Parameters<typeof git.deleteCodeSnapshot>[3]) => {
    observations.push({ tip: await git.codeSnapshotTip(bare, id), checked, bare, id });
    await remove(bare, id, checked, proof);
  });
  const exec: Executor = { run: async (ctx) => {
    commit(ctx);
    await ctx.codeCrossCheckGate!({ interlocked: false });
    return { branch: ctx.branch };
  } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab).execute(gitlabClaim(2173, { code_cross_check_required: true, claim_generation: 1 }));
  assert.equal(deleted.mock.callCount(), 1);
  assert.equal(observations.length, 1);
  assert.equal(observations[0]!.tip, head);
  assert.equal(observations[0]!.checked, head);
  await assert.rejects(git.codeSnapshotTip(observations[0]!.bare, observations[0]!.id));
});

it("Codex lead records worker_unsupported before snapshot", async (t) => {
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
  let submitted: unknown;
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, value: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
    submitted = value;
    return { ...record(), head_commit: null, base_commit: null, outcome: "failed", reason_class: "worker_unsupported" };
  });
  const pin = t.mock.method(git, "pinCodeSnapshot", async () => { throw new Error("unexpected snapshot"); });
  const exec: Executor = { run: async (ctx) => {
    commit(ctx);
    await ctx.codeCrossCheckGate!({ interlocked: false });
    return { branch: ctx.branch };
  } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab).execute(gitlabClaim(2172, { code_cross_check_required: true, claim_generation: 1,
    secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", capability: "fixture-cap", access_token: "fixture-access" } } }));
  assert.deepEqual(submitted, { reason_class: "worker_unsupported" });
  assert.equal(pin.mock.callCount(), 0);
});
