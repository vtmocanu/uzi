import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import type { Readable } from "node:stream";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { BoundaryPermit, BoundaryProcessRequest, CodexExecutionSafety } from "../src/harness.js";
import { ReviewRunner } from "../src/review-runner.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { RunResidueBlockedError, latchResidueQuarantine, residueQuarantine } from "../src/residue-quarantine.js";
import type { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import type { ClaimResponse, StateRequest, TaskReviewRequest } from "../src/protocol.js";
import { makeFakeProcRoot, plantUnreadableUnattributed, withQuiescenceView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  assistant,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  resultOk,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

// issue #2213 — enforcement while latched, driven through the runner and the real executors.
// Layer: the git funnel (finalize push, milestone checkpoint) through RunRunner + the real git
// layer on both harness shapes, the Claude SdkExecutor's boundary and call-site gates, the Claude
// advice lane, and the review runner's pre-fetch check. The Codex transport/harness/launcher gates
// are in residue-quarantine-codex.test.ts and codex-executor.test.ts.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const TIMEOUT = { timeout: 60_000 };

const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));

function latch(): void {
  latchResidueQuarantine({ cause: 'runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)', runId: "r", site: "pre_clone" }, nullLogger());
}

function commitWork(tree: string, file = "WORK.txt"): void {
  fs.writeFileSync(path.join(tree, file), "work\n");
  execFileSync("git", ["-C", tree, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

/** Does the fake forge origin carry `agent/issue-<iid>`? (A push that ran would have put it there.) */
function originHasBranch(iid: number): boolean {
  try {
    execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "-q", `refs/heads/agent/issue-${iid}`], { env: GIT_ENV, stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

function spyPublish(): { calls: () => number } {
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n };
}

/** A fake Codex safety whose boundary spawns real children and records every request. */
function recordingSafety(): { safety: CodexExecutionSafety; spawns: BoundaryProcessRequest[] } {
  const spawns: BoundaryProcessRequest[] = [];
  const safety: CodexExecutionSafety = {
    kind: "codex",
    withBoundary: async (req, action) =>
      action({ epoch: 1, boundary: req.boundary, signal: new AbortController().signal } as unknown as BoundaryPermit),
    spawnBoundaryProcess: async (_permit, request) => {
      spawns.push(request);
      const [command, ...args] = request.argv;
      const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const completed = new Promise<{ code: number }>((resolve) => {
        child.once("error", () => resolve({ code: -1 }));
        child.once("close", (code) => resolve({ code: code ?? 1 }));
      });
      const cancel = async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; };
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel, completed };
    },
    dispose: async () => ({ kind: "disposed" }),
  };
  return { safety, spawns };
}

function factoryFor(executor: Executor): ExecutorFactory {
  return (runId) => ({ homeDir: path.join(homeDir, runId), executor });
}

describe("credentialed git while latched, through the real git layer (issue #2213)", () => {
  for (const shape of ["claude", "codex"] as const) {
    it(`${shape} run: a latch set mid-run refuses the finalize push as worker_residue_blocked; no push is spawned; the clone is kept`, TIMEOUT, async () => {
      const iid = shape === "claude" ? 22200 : 22201;
      const rec = recordingSafety();
      const executor: Executor = {
        ...(shape === "codex" ? { safety: rec.safety } : {}),
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          latch(); // detected elsewhere: no process is planted here, so no own-mode scan could notice it
          return { branch: ctx.branch, summary: "done" };
        },
      };
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const claim = gitlabClaim(iid);
      await runnerWith(factoryFor(executor), gitlab).execute(claim);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
      assert.match(String(failed?.failure_reason), /this worker is quarantined/);
      assert.equal(originHasBranch(iid), false, "the push never reached the forge");
      assert.equal(mrCalls.length, 0, "no MR was opened");
      assert.equal(rec.spawns.some((s) => s.argv.includes("push")), false, "no push child was spawned through the boundary");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      assert.ok(!statuses(claim.run_id).includes("completed"));
    });
  }

  it("the milestone checkpoint's overlay PAT fetch is refused while latched, so its publish is skipped; the run continues and then fails typed at finalize", TIMEOUT, async () => {
    // The overlay (and so the checkpoint's only PAT-bearing git) exists for a GitHub repo.
    const iid = 22202;
    const pub = spyPublish();
    const seen: number[] = [];
    const executor: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        latch();
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        seen.push(pub.calls());
        return { branch: ctx.branch, summary: "done" };
      },
    };
    const claim = gitlabClaim(iid, { repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github", default_branch: "main" } });
    await runnerWith(factoryFor(executor), fakeGitlab().gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      github: fakeGitHub().github,
    }).execute(claim);
    assert.deepEqual(seen, [0], "ctx.checkpoint returned with nothing published");
    assert.equal(pub.calls(), 0);
    assert.equal(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
    assert.equal(originHasBranch(iid), false);
  });

  it("control: the same GitHub milestone checkpoint publishes when unlatched", TIMEOUT, async () => {
    const iid = 22204;
    const pub = spyPublish();
    const seen: number[] = [];
    const executor: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        seen.push(pub.calls());
        return { branch: ctx.branch, summary: "done" };
      },
    };
    const claim = gitlabClaim(iid, { repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github", default_branch: "main" } });
    await runnerWith(factoryFor(executor), fakeGitlab().gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      github: fakeGitHub().github,
    }).execute(claim);
    assert.ok((seen[0] ?? 0) >= 1, `the checkpoint published: ${JSON.stringify(seen)}`);
  });

  it("control (unlatched): the same run completes and pushes", TIMEOUT, async () => {
    const iid = 22203;
    const executor: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        return { branch: ctx.branch, summary: "done" };
      },
    };
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const claim = gitlabClaim(iid);
    await runnerWith(factoryFor(executor), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), statuses(claim.run_id).join(","));
    assert.equal(originHasBranch(iid), true);
    assert.equal(mrCalls.length, 1);
  });
});

describe("the review runner (issue #2213)", () => {
  function reviewRig() {
    const posted: TaskReviewRequest[] = [];
    const states: StateRequest[] = [];
    const calls = { ensureClone: 0 };
    const fakeClient = {
      reportState: async (_id: string, body: StateRequest) => {
        states.push(body);
        return { applied: true, status: body.status } as never;
      },
      postTaskReview: async (_id: string, review: TaskReviewRequest) => {
        posted.push(review);
      },
    } as unknown as WorkerClient;
    const fakeGit = {
      ensureClone: async () => {
        calls.ensureClone++;
        return "/bare/repo.git";
      },
      reviewDiff: async () => "diff --git a/x b/x\n",
      defaultBranchName: async () => "main",
    } as unknown as GitCache;
    const claim = {
      run_id: "22210000-0000-4000-8000-000000000000",
      kind: "task",
      review_target_run_id: "target-1",
      issue_iid: null,
      issue_title: "Review",
      issue_description: "",
      branch: "uzi/task/target-1",
      base_branch: "main",
      repo: { id: "r1", url: "u", clone_url: "/origin", default_branch: "main" },
      secrets: { forge_pat: "pat", anthropic_oauth_token: "tok-abc" },
      last_seq: 0,
      agents: [],
    } as unknown as ClaimResponse;
    return { fakeClient, fakeGit, claim, posted, states, calls };
  }
  let root: string;
  beforeEach(() => {
    root = makeFakeProcRoot("uzi-2213-review-");
  });
  afterEach(() => {
    restoreHermeticView();
    fs.rmSync(root, { recursive: true, force: true });
  });

  it("an unreadable unattributed process refuses the PAT-bearing fetch: no ensureClone, a failed review is posted, and the worker latches", {
    ...TIMEOUT, skip: process.platform !== "linux" && "worker-wide pre-fetch process scan is Linux-only",
  }, async () => {
    plantUnreadableUnattributed(root, 4242);
    const rig = reviewRig();
    const rr = runnerWith(factoryFor({ run: async () => ({ branch: "x" }) }), fakeGitlab().gitlab);
    const reviewer = new ReviewRunner(rig.fakeClient, rig.fakeGit, nullLogger(), {
      queryFn: (() => {
        throw new Error("model must not be called");
      }) as unknown as SdkQueryFn,
      preFetchCheck: (runId, site) => rr.checkWorkerResidueBeforeFetch(runId, site),
    });
    await withQuiescenceView({ procRoot: root }, () => reviewer.execute(rig.claim));
    assert.equal(rig.calls.ensureClone, 0, "the fetch never ran");
    assert.equal(rig.posted.at(-1)?.status, "failed");
    assert.match(String(rig.posted.at(-1)?.summary), /worker_residue_blocked/);
    assert.equal(residueQuarantine()?.site, "review_pre_fetch");
  });

  it("a worker that is already latched refuses the review's fetch through the same check", TIMEOUT, async () => {
    latch();
    const rig = reviewRig();
    const reviewer = new ReviewRunner(rig.fakeClient, rig.fakeGit, nullLogger(), {
      queryFn: (() => {
        throw new Error("model must not be called");
      }) as unknown as SdkQueryFn,
      preFetchCheck: async () => {
        throw new RunResidueBlockedError("injected");
      },
    });
    await reviewer.execute(rig.claim);
    assert.equal(rig.calls.ensureClone, 0);
    assert.equal(rig.posted.at(-1)?.status, "failed");
  });
});

describe("the Claude lanes while latched (issue #2213)", () => {
  /** A queryFn whose first call yields a plan, then latches the worker; counts every call. */
  function planThenLatch(counter: { calls: number }, firstFrames: unknown[]): SdkQueryFn {
    return (params) => {
      counter.calls++;
      const first = counter.calls === 1;
      return (async function* () {
        for await (const _ of params.prompt) {
          /* drain */
        }
        if (!first) throw new Error("a second query must not be started");
        // The worker is quarantined while this turn is in flight: before its terminal frame (the
        // consumer stops iterating at the result, so nothing after it would run).
        for (const [i, m] of firstFrames.entries()) {
          if (i === firstFrames.length - 1) latch();
          yield m as never;
        }
      })();
    };
  }
  const planFrames = [
    assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# PLAN\n- do it" } }]),
    resultOk(),
  ];

  it("the implementation loop top stops the run before another turn: typed worker_residue_blocked, one query total", TIMEOUT, async () => {
    const counter = { calls: 0 };
    const iid = 22220;
    const claim = gitlabClaim(iid);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    const factory: ExecutorFactory = (id) => ({
      homeDir: path.join(homeDir, id),
      executor: new SdkExecutor(nullLogger(), path.join(homeDir, id), { queryFn: planThenLatch(counter, planFrames) }),
    });
    await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
    assert.equal(counter.calls, 1, "no query after the latch");
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
    assert.match(String(failed?.failure_reason), /this worker is quarantined/);
  });

  it("the queryFn call site alone: a retry that bypasses every loop boundary (the plan nudge) is refused; no second query", TIMEOUT, async () => {
    const counter = { calls: 0 };
    const iid = 22221;
    const claim = gitlabClaim(iid);
    const prose = [assistant([{ type: "text", text: "I will think about a plan, no tool call" }]), resultOk()];
    const factory: ExecutorFactory = (id) => ({
      homeDir: path.join(homeDir, id),
      executor: new SdkExecutor(nullLogger(), path.join(homeDir, id), { queryFn: planThenLatch(counter, prose) }),
    });
    await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
    assert.equal(counter.calls, 1, "the nudge turn was never started");
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
  });

  it("an advice pass (summary) makes zero queryFn calls and takes its fallback (null)", async () => {
    latch();
    let calls = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-advice-"));
    try {
      const runner = new SummaryRunner(nullLogger(), {
        queryFn: ((): never => {
          calls++;
          throw new Error("must not be called");
        }) as unknown as SdkQueryFn,
        homeRoot,
        modelTimeoutMs: 200,
      });
      const out = await runner.generateIntentSummary({ token: "tok-abc", model: "haiku", issueTitle: "t", issueBody: "b", prdText: "" });
      assert.equal(out, null, "the deterministic fallback");
      assert.equal(calls, 0, "no query was started");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("control: unlatched, the same advice pass queries", async () => {
    let calls = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-advice-"));
    try {
      const runner = new SummaryRunner(nullLogger(), {
        queryFn: (async function* () {
          calls++;
          yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "ok" }] } };
          yield { type: "result", subtype: "success", is_error: false };
        }) as unknown as SdkQueryFn,
        homeRoot,
        modelTimeoutMs: 2000,
      });
      assert.equal(await runner.generateIntentSummary({ token: "tok-abc", model: "haiku", issueTitle: "t", issueBody: "b", prdText: "" }), "ok");
      assert.equal(calls, 1);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
