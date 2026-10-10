import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Executor, RunContext } from "../src/executor.js";
import { checkCode } from "../src/code-cross-check-gate.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, fx, homeDir, runner, fakeGitlab, gitlabClaim, installHarness, planThenDoneQuery, input } from "./runner-harness.js";

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

function codeSafety() {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  let reaps = 0;
  const reservation = registry.reserveLaunch("provider");
  assert.equal(reservation.kind, "reserved");
  if (reservation.kind === "reserved") registry.registerRoot(reservation.reservation, {
    kind: "provider", reap: async () => { reaps++; return { ok: true }; }, dispose: async () => {},
  });
  const safety = createCodexExecutionSafety(registry,
    async () => { throw new Error("unused root spawn"); }, undefined, undefined,
    async request => {
      const [command, ...args] = request.argv;
      assert.ok(command);
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => { await terminal; return { ok: true }; },
          dispose: async () => {
            if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
            await terminal.catch(() => undefined);
          },
        },
        stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
        waitChild: async () => terminal,
      };
    });
  return { safety, reaps: () => reaps };
}

for (const harness of ["claude", "codex"] as const) {
  for (const interlocked of [false, true]) {
    it(`${harness} ${interlocked ? "interlocked" : "legacy"} repair preserves original H across the repaired tip and MR`, async (t) => {
      let persisted: import("../src/code-cross-check-contract.js").CodeCrossCheckStatus = { stage: "code", result: "no_row" };
      t.mock.method(client, "codeCrossCheckStatus", async () => persisted);
      let original = "", repaired = "";
      const submit = t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
        assert.ok("head_commit" in snapshot);
        persisted = { ...record(snapshot.head_commit, snapshot.base_commit), checker_run_id: "child", finalized_at: null, dispositions: null,
          findings: [{ id: "F1", severity: "major", path: "implementation.txt", line: 1, title: "Verify work", detail: "one repair" }] };
        return persisted;
      });
      t.mock.method(client, "reportCodeCrossCheckDispositions", async (_id: string, _gen: number, batch: Parameters<WorkerClient["reportCodeCrossCheckDispositions"]>[2]) => {
        assert.ok(!("result" in persisted));
        persisted = { ...persisted, finalized_at: new Date().toISOString(), dispositions: batch };
        return persisted;
      });
      const pin = t.mock.method(git, "pinCodeSnapshot", git.pinCodeSnapshot.bind(git));
      const safetyRig = harness === "codex" ? codeSafety() : undefined;
      const exec: Executor = {
        ...(safetyRig ? { safety: safetyRig.safety } : { killAgentTree: () => {} }),
        run: async ctx => {
          await assert.rejects(ctx.reportCrossCheckDispositions!([]), /not active/);
          original = commit(ctx);
          if (interlocked) await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
          assert.equal((await ctx.codeCrossCheckGate!({ interlocked })).action, "repair");
          assert.equal((await ctx.codeCrossCheckRepair!())?.head_commit, original);
          execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "repair"]);
          repaired = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
          await ctx.reportCrossCheckDispositions!([{ finding_id: "F1", disposition: "addressed", reason: "verified work and reran checks" }]);
          if (interlocked) await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
          assert.deepEqual(await ctx.codeCrossCheckGate!({ interlocked }), { action: "proceed" });
          assert.equal(await ctx.codeCrossCheckRepair!(), undefined);
          assert.equal(pin.mock.callCount(), 2, "only original import capture and original fresh pin");
          return { branch: ctx.branch };
        },
      };
      const forge = fakeGitlab();
      await runner(exec, forge.gitlab).execute(gitlabClaim(2180, { code_cross_check_required: true, claim_generation: 1,
        ...(harness === "codex" ? { secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", capability: "fixture-cap", access_token: "fixture-access" } } } : {}) }));
      assert.equal(submit.mock.callCount(), 1);
      if (safetyRig) assert.ok(safetyRig.reaps() > 0, "production safety facade reaped the registered provider root");
      assert.notEqual(original, repaired);
      assert.ok(JSON.stringify(forge.calls).includes(`checked ${original.slice(0, 12)}`));
      assert.ok(!JSON.stringify(forge.calls).includes(`checked ${repaired.slice(0, 12)}`));
      assert.ok(JSON.stringify(forge.calls).includes("1 findings: 1 addressed, 0 declined"));
    });
  }
}

for (const movement of ["bridge", "concurrent clone tip"] as const) {
  it(`interlocked code gate preserves original H after successful import and ${movement}`, async (t) => {
    const status = t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
    const snapshots: Parameters<WorkerClient["submitCodeCrossCheck"]>[2][] = [];
    const submit = t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
      snapshots.push(snapshot);
      return "head_commit" in snapshot ? record(snapshot.head_commit, snapshot.base_commit) : record();
    });
    let base = "", originalH = "", movedTip = "", clone = "", branch = "", bare = "";
    const publishedBranch = "feature/code-snapshot";
    if (movement === "bridge") {
      execFileSync("git", ["-C", fx.originPath, "checkout", "-b", publishedBranch]);
      fs.writeFileSync(path.join(fx.originPath, "published.txt"), "published floor\n");
      execFileSync("git", ["-C", fx.originPath, "add", "published.txt"]);
      execFileSync("git", ["-C", fx.originPath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "published floor"]);
      execFileSync("git", ["-C", fx.originPath, "checkout", "main"]);
    }
    let moveOnPublish = false;
    let reaps = 0;
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<WorkerClient["publishCheckpoint"]>) => {
      const pack = args[2];
      for await (const _chunk of pack) { /* drain the real local Git pack */ }
      if (moveOnPublish && movement === "concurrent clone tip") {
        execFileSync("git", ["-C", clone, "-c", "user.name=test", "-c", "user.email=test@example.test",
          "commit", "--allow-empty", "-qm", "concurrent tip"]);
        movedTip = execFileSync("git", ["-C", clone, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      }
      return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
    });
    const exec: Executor = {
      killAgentTree: () => { reaps++; },
      run: async (ctx) => {
        clone = ctx.worktreePath;
        branch = ctx.branch;
        bare = git.barePathFor(fx.originPath);
        base = execFileSync("git", ["-C", fx.originPath, "rev-parse", "main"], { encoding: "utf8" }).trim();
        if (movement === "concurrent clone tip") commit(ctx);
        if (movement === "bridge") {
          execFileSync("git", ["-C", clone, "reset", "--hard", base]);
          fs.writeFileSync(path.join(clone, "rewritten.txt"), "rewritten implementation\n");
          execFileSync("git", ["-C", clone, "add", "rewritten.txt"]);
          execFileSync("git", ["-C", clone, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "rewritten"]);
        }
        originalH = execFileSync("git", ["-C", clone, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
        moveOnPublish = true;
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
        if (movement === "bridge") movedTip = await git.trackingTip(bare, branch) ?? "";
        await ctx.codeCrossCheckGate!({ interlocked: true });
        return { branch };
      },
    };
    const forge = fakeGitlab();
    await runner(exec, forge.gitlab).execute(gitlabClaim(2175, { code_cross_check_required: true, claim_generation: 1,
      ...(movement === "bridge" ? { kind: "task", branch: publishedBranch, open_mr: false } : {}) }));
    assert.ok(reaps > 0, "checkpoint reaped the executor");
    assert.equal(status.mock.callCount(), movement === "bridge" ? 2 : 3, "publication refresh occurs only when this run publishes an MR");
    assert.equal(submit.mock.callCount(), 1);
    assert.deepEqual(snapshots, [{ head_commit: originalH, base_commit: base }]);
    assert.match(originalH, /^[0-9a-f]{40}$/);
    assert.match(movedTip, /^[0-9a-f]{40}$/);
    assert.notEqual(movedTip, originalH, "the post-import movement actually happened");
    if (movement === "bridge") {
      assert.equal(await git.ancestry(bare, originalH, movedTip), "ancestor");
      assert.equal(await git.revParse(bare, `${movedTip}^{tree}`), await git.revParse(bare, `${originalH}^{tree}`));
    }
  });
}

for (const failure of ["refused import", "failed import", "unavailable reap", "unverified quiescence"] as const) {
  it(`interlocked code gate clears a cached snapshot after ${failure}`, async (t) => {
    t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
    const snapshots: Parameters<WorkerClient["submitCodeCrossCheck"]>[2][] = [];
    t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
      snapshots.push(snapshot);
      const reply = { ...record(), head_commit: null, base_commit: null, outcome: "failed" as const, reason_class: "snapshot_failed" };
      return reply;
    });
    const importBranch = git.fetchAgentBranch.bind(git);
    let fail = false, imported = 0, refused = 0, reapFailures = 0, blockedProofs = 0, failedPhaseImports = 0;
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      if (fail) failedPhaseImports++;
      if (fail && (failure === "refused import" || failure === "failed import")) {
        refused++;
        if (failure === "failed import") throw new Error("fixture import failed");
        return { kind: "not_updated", reason: "ownership_unknown" };
      }
      const result = await importBranch(...args);
      if (result.kind === "updated") imported++;
      return result;
    });
    let pinsBeforeFailure = 0, pinsAfterGate = 0;
    const pin = t.mock.method(git, "pinCodeSnapshot", git.pinCodeSnapshot.bind(git));
    let checkpointError: unknown, gateResult: unknown;
    const exec: Executor = {
      killAgentTree: () => {
        if (fail && failure === "unavailable reap") { reapFailures++; throw new Error("fixture reap unavailable"); }
      },
      run: async (ctx) => {
        commit(ctx);
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
        pinsBeforeFailure = pin.mock.callCount();
        fail = true;
        try { await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" }); }
        catch (err) { checkpointError = err; }
        gateResult = await ctx.codeCrossCheckGate!({ interlocked: true });
        pinsAfterGate = pin.mock.callCount();
        fail = false;
        return { branch: ctx.branch };
      },
    };
    const forge = fakeGitlab();
    await runner(exec, forge.gitlab, undefined, failure === "unverified quiescence" ? {
      quiesceRun: async () => {
        if (fail) blockedProofs++;
        return {
          process: { state: fail ? "unverified" : "quiescent", processes: [], killed: [], detail: fail ? "fixture proof unavailable" : "" },
          docker: { state: "not_wired", removed: [], detail: "" },
        };
      },
    } : {}).execute(gitlabClaim(2176, { code_cross_check_required: true, claim_generation: 1 }));
    assert.ok(imported > 0, "first checkpoint successfully imported real Git work");
    assert.equal(pinsBeforeFailure, 1, "first checkpoint captured a snapshot to invalidate");
    assert.equal(pinsAfterGate, pinsBeforeFailure, "failed checkpoint cannot repin a cached tip");
    assert.deepEqual(snapshots, [{ reason_class: "snapshot_failed" }], "failure submission contains neither SHA");
    assert.deepEqual(gateResult, { action: "proceed" });
    if (failure === "unavailable reap") {
      assert.equal(reapFailures, 1);
      assert.ok(checkpointError instanceof Error);
    } else if (failure === "unverified quiescence") {
      assert.equal(blockedProofs, 1);
      assert.equal(failedPhaseImports, 0, "unverified quiescence prevented import");
    } else {
      assert.equal(refused, 1, "second checkpoint attempted import despite the unchanged tip");
    }
  });
}

for (const metadata of ["reportOnly", "notCode", "confirmedEmptyPrompt"] as const) {
  it(`interlocked code gate skips ${metadata} completion metadata without status, submit or pin`, async (t) => {
    const status = t.mock.method(client, "codeCrossCheckStatus", async () => { throw new Error("unexpected status"); });
    const submit = t.mock.method(client, "submitCodeCrossCheck", async () => { throw new Error("unexpected submit"); });
    const pin = t.mock.method(git, "pinCodeSnapshot", async () => { throw new Error("unexpected pin"); });
    let result: unknown;
    const exec: Executor = { run: async (ctx) => {
      commit(ctx);
      result = await ctx.codeCrossCheckGate!({ interlocked: true, [metadata]: true });
      return { branch: ctx.branch };
    } };
    const forge = fakeGitlab();
    await runner(exec, forge.gitlab).execute(gitlabClaim(2177, { code_cross_check_required: true, claim_generation: 1 }));
    assert.deepEqual(result, { action: "proceed" });
    assert.equal(status.mock.callCount(), 0);
    assert.equal(submit.mock.callCount(), 0);
    assert.equal(pin.mock.callCount(), 0);
  });
}

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

it("Codex lead with unverified reap records snapshot_failed without a checked SHA", async (t) => {
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row" }));
  let submitted: unknown;
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, value: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
    submitted = value;
    return { ...record(), head_commit: null, base_commit: null, outcome: "failed", reason_class: "snapshot_failed" };
  });
  const pin = t.mock.method(git, "pinCodeSnapshot", async () => { throw new Error("unexpected snapshot"); });
  let failingReap = false, reapFailures = 0;
  const exec: Executor = { killAgentTree: () => {
    if (failingReap) { reapFailures++; throw new Error("fixture reap unavailable"); }
  }, run: async (ctx) => {
    commit(ctx);
    failingReap = true;
    try { await ctx.codeCrossCheckGate!({ interlocked: false }); }
    finally { failingReap = false; }
    return { branch: ctx.branch };
  } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab).execute(gitlabClaim(2172, { code_cross_check_required: true, claim_generation: 1,
    secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", capability: "fixture-cap", access_token: "fixture-access" } } }));
  assert.deepEqual(submitted, { reason_class: "snapshot_failed" });
  assert.equal(reapFailures, 1, "snapshot boundary actually exercised the failing reap");
  assert.equal(pin.mock.callCount(), 0);
});
