import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs, { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { PassThrough } from "node:stream";
import { createInterface } from "node:readline";
import { CodexExecutor, type CodexExecutorDeps } from "../src/codex/codex-executor.js";
import { createCodexTransport } from "../src/codex/transport.js";
import { selectCodexBinding } from "../src/codex/select.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { ExecutorResult } from "../src/executor.js";
import { type SDKMessage, type SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached } from "../src/sdk-spawn.js";
import { nullLogger } from "./helpers.js";
import { forceIncompleteHomeHelper } from "./forced-home-helper.js";
import { scanRunProcesses, reapRunProcesses } from "../src/run-procs.js";
import { StubExecutor, PlanRejectedError, STUB_FAIL_SENTINEL, STUB_ASK_SENTINEL, type Executor } from "../src/executor.js";
import { AUTOPILOT_SENTINEL_ANSWER } from "../src/runner.js";
import { CI_CONFIG_MARKER } from "../src/prompt.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { PlanSummaryResult, SummaryRunner } from "../src/summary-runner.js";
import type { Logger } from "../src/log.js";
import type { StateRequest } from "../src/protocol.js";
import { SteeringChannel } from "../src/steering.js";
import {
  api,
  assistant,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  isAlive,
  planThenDoneQuery,
  planWithMilestonesThenDoneQuery,
  resultOk,
  runner,
  runnerWith,
  simulateCommittedWork,
  waitDead,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

function approvalWaiter(
  stateApi: Pick<typeof api, "onState">,
  runId: string,
  signal: AbortSignal,
  start: () => Promise<void>,
): { approval: Promise<void>; execution: Promise<void> } {
  let lastFailureReason: string | undefined;
  const gate = new Promise<void>((resolve) => {
    stateApi.onState(runId, (body) => {
      if (body.failure_reason) lastFailureReason = body.failure_reason;
      if (body.status === "awaiting_approval") resolve();
    });
  });
  let onAbort = () => {};
  const cancelled = new Promise<never>((_, reject) => {
    onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted) onAbort();
  });
  const execution = Promise.resolve().then(() => {
    signal.throwIfAborted();
    return start();
  });
  const ended = execution.then(() => {
    throw new Error(
      `execution completed before awaiting_approval${lastFailureReason ? `: ${lastFailureReason}` : ""}`,
    );
  });
  const approval = Promise.race([gate, ended, cancelled]).finally(() => {
    signal.removeEventListener("abort", onAbort);
    stateApi.onState(runId, () => {});
  });
  // The clone hold may delay awaiting approval; observe rejection immediately.
  void approval.catch(() => {});
  return { approval, execution };
}

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((res) => { resolve = res; });
  return { promise, resolve };
}

async function flushMicrotasks(): Promise<void> {
  // Eight turns drain the waiter's race, finally, and observation handlers.
  for (let turn = 0; turn < 8; turn++) await Promise.resolve();
}

function controlledStates() {
  let hook = (_body: StateRequest) => {};
  return {
    onState(_runId: string, fn: (body: StateRequest) => void) { hook = fn; },
    emit(body: StateRequest) { hook(body); },
  };
}

describe("approval waiter", () => {
  it("ignores running then resolves on approval", async (t) => {
    const states = controlledStates();
    const held = deferred();
    const wait = approvalWaiter(states, "run", t.signal, () => {
      states.emit({ status: "running" });
      return held.promise;
    });
    let settled = false;
    void wait.approval.then(() => { settled = true; }, () => { settled = true; });
    await flushMicrotasks();
    assert.equal(settled, false);
    states.emit({ status: "awaiting_approval" });
    await wait.approval;
    held.resolve();
    await wait.execution;
    // The hook was replaced after settlement.
    states.emit({ status: "failed", failure_reason: "late report" });
  });

  it("subscribes before the start callback emits approval", async (t) => {
    const states = controlledStates();
    const wait = approvalWaiter(states, "run", t.signal, async () => {
      states.emit({ status: "awaiting_approval" });
    });
    await wait.approval;
    await wait.execution;
  });

  it("preserves execution rejection", async (t) => {
    const sentinel = new Error("execution sentinel");
    const wait = approvalWaiter(controlledStates(), "run", t.signal, () => Promise.reject(sentinel));
    await assert.rejects(wait.approval, (err) => err === sentinel);
    await assert.rejects(wait.execution, (err) => err === sentinel);
  });

  it("rejects premature completion", async (t) => {
    const wait = approvalWaiter(controlledStates(), "run", t.signal, async () => {});
    await assert.rejects(wait.approval, /execution completed before awaiting_approval/);
    await wait.execution;
  });

  it("includes the last recorded failure reason on premature completion", async (t) => {
    const states = controlledStates();
    const wait = approvalWaiter(states, "run", t.signal, async () => {
      states.emit({ status: "failed", failure_reason: "first reason" });
      states.emit({ status: "failed", failure_reason: "last reason" });
      states.emit({ status: "running" });
    });
    await assert.rejects(wait.approval, /execution completed before awaiting_approval: last reason/);
    await wait.execution;
  });

  it("aborts a pending wait", async () => {
    const controller = new AbortController();
    const held = deferred();
    const wait = approvalWaiter(controlledStates(), "run", controller.signal, () => held.promise);
    await flushMicrotasks();
    const sentinel = new Error("abort sentinel");
    controller.abort(sentinel);
    await assert.rejects(wait.approval, (err) => err === sentinel);
    held.resolve();
    await wait.execution;
  });

  it("rejects a pre-aborted signal without starting execution", async () => {
    const controller = new AbortController();
    const sentinel = new Error("pre-abort sentinel");
    controller.abort(sentinel);
    let started = false;
    const wait = approvalWaiter(controlledStates(), "run", controller.signal, async () => { started = true; });
    await assert.rejects(wait.approval, (err) => err === sentinel);
    await assert.rejects(wait.execution, (err) => err === sentinel);
    assert.equal(started, false);
  });

  it("reaches approval after the former polling deadline with execution held", async (t) => {
    const states = controlledStates();
    const held = deferred();
    t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: Date.now() });
    const wait = approvalWaiter(states, "run", t.signal, () => held.promise);
    let settled = false;
    void wait.approval.then(() => { settled = true; }, () => { settled = true; });
    try {
      await flushMicrotasks();
      t.mock.timers.tick(3000);
      await flushMicrotasks();
      if (settled) await wait.approval;
      assert.equal(settled, false, "approval remains pending while execution is held");
      states.emit({ status: "awaiting_approval" });
      await wait.approval;
    } finally {
      t.mock.timers.reset();
      held.resolve();
      await wait.execution;
    }
  });
});

// Script only the external app-server peer; the real transport, Codex hooks and runner
// gate own framing, revision, persistence and callback ordering.
function summaryPlanProvider(
  plans: string[],
  // Awaited when the provider is asked to start turn N (0-based): plans first, then the implementation turn.
  onTurn?: (index: number) => Promise<void>,
): NonNullable<CodexExecutorDeps["launchProviderRoot"]> {
  let turns = 0;
  return async () => {
    const inbound = new PassThrough();
    const outbound = new PassThrough();
    const lines = createInterface({ input: outbound });
    const frame = (value: unknown) => inbound.write(JSON.stringify(value) + "\n");
    lines.on("line", async line => {
      const request = JSON.parse(line);
      if (!request.method || request.id === undefined) return;
      if (request.method === "turn/start") await onTurn?.(turns);
      let result: unknown = {};
      if (request.method === "initialize")
        result = { userAgent: "codex/test", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" };
      else if (request.method === "account/login/start") result = { type: request.params.type };
      else if (request.method === "thread/start" || request.method === "thread/resume")
        result = { thread: { id: "summary-thread" } };
      else if (request.method === "turn/start") {
        const index = turns++;
        const turnId = `summary-turn-${index}`;
        result = { turn: { id: turnId } };
        frame({ id: 100 + index, method: "item/tool/call", params: {
          threadId: "summary-thread", turnId, callId: `summary-call-${index}`,
          tool: index < plans.length ? "submit_plan" : "signal_done",
          arguments: index < plans.length ? { plan_md: plans[index] } : { report_only: true, summary: "Done" },
        } });
        frame({ method: "turn/completed", params: {
          threadId: "summary-thread", turn: { id: turnId, status: "completed" },
        } });
      }
      frame({ id: request.id, result });
    });
    return {
      root: { kind: "provider", reap: async () => ({ ok: true }), dispose: async () => {
        lines.close(); outbound.destroy(); inbound.destroy();
      } },
      transport: createCodexTransport({ inbound, outbound }), supervisorPid: 1234,
    };
  };
}

function codexSummaryDeps(plans: string[], onTurn?: (index: number) => Promise<void>): CodexExecutorDeps {
  return {
    launchProviderRoot: summaryPlanProvider(plans, onTurn),
    launchEffectRoot: async () => ({
      started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 200,
      transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
      waitChild: async () => ({ event: "child_exit", code: 0 }),
      dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
      failed: undefined, whenFailed: new Promise<Error>(() => {}),
    }),
    wireFileop: () => ({ client: { op: async () => ({ ok: true }) }, dispose: async () => {} }),
    sessionStore: {
      adopt: async () => ({ files: 0 }), inspect: async () => "absent",
      remove: async () => {}, persist: async () => ({ files: 0, bytes: 0 }),
    },
    spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
    provisionRunTools: async () => ({ toolEnv: {} }),
    installDeps: async () => ({ results: [], truncated: false }),
    idleMs: 5000, wallMs: 5000, childTurnDeadlineMs: 5000,
    };
}

describe("2833 M3 runner Codex summary persistence", () => {
  for (const mode of ["initial", "revised", "replacement-failed", "replacement-409", "autopilot"] as const) {
    it(`2833 M3 ${mode}: summaries follow durable human gates and only accepted replacement reaches result`, async () => {
      const revised = mode.startsWith("replacement") || mode === "revised";
      const plans = revised ? ["Initial\u0000 plan", "Revised\u0000 plan"] : ["Initial\u0000 plan"];
      const { gitlab } = fakeGitlab();
      const codex = { auth_mode: "api_key", access_token: "fixture-codex-access", capability: "fixture-codex-capability" };
      const claim = gitlabClaim(2833, {
        auto_approve: mode === "autopilot", summary_intent: "Existing intent",
        secrets: { forge_pat: "fixture-forge-pat-000000", codex },
      });
      const selected = selectCodexBinding({ codex });
      assert.equal(selected.kind, "codex");
      if (selected.kind !== "codex") throw new Error("invalid fixture binding");
      api.codexAuthModes.set(claim.run_id, "api_key");
      let presentations = 0;
      // The runner no longer awaits the summary hook (issue #2901), so each verdict is appended only
      // after that presentation's summary attempt has settled; the assertions below then keep
      // meaning "the attempt for presentation N ran against plan N".
      const attemptSettled = [deferred(), deferred()];
      api.onState(claim.run_id, body => {
        if (body.status !== "awaiting_approval") return;
        const index = presentations++;
        void attemptSettled[index]!.promise.then(() => {
          api.appendInputs(claim.run_id, [input(revised && index === 0 ? "revise_plan" : "approve_plan",
            revised && index === 0 ? "Revise the plan" : undefined)]);
        });
      });
      const observations: { generated: string; persisted: string | undefined; status: string | undefined }[] = [];
      const attempts: { guard: string; persisted: string | undefined; status: string | undefined }[] = [];
      const accepted: string[] = [];
      const callbacks: string[] = [];
      let result: ExecutorResult | undefined;
      // This public client seam models the summary endpoint's exact stale-write guard.
      // Record successes separately; a swallowed mismatch/HTTP error cannot look green.
      client.postPlanSummary = async (_runId, body: Parameters<WorkerClient["postPlanSummary"]>[1]) => {
        const state = api.states.filter(s => s.runId === claim.run_id && s.body.plan_md !== undefined).at(-1)?.body;
        attempts.push({ guard: body.plan_md, persisted: state?.plan_md, status: state?.status });
        if (state?.plan_md !== body.plan_md || mode === "replacement-409" && attempts.length === 2)
          throw new RequestError("POST", "/summary/plan", 409, "superseded plan");
        if (mode === "replacement-failed" && attempts.length === 2)
          throw new RequestError("POST", "/summary/plan", 500, "summary persistence failed");
        accepted.push(body.summary);
      };
      const deps = codexSummaryDeps(plans);
      await runnerWith(() => {
        const executor = new CodexExecutor(nullLogger(), homeDir, {
          binding: selected.binding, client,
          provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
          summaryRunner: {
            generateIntentSummary: async () => null,
            generatePlanSummary: async ({ planMd }) => {
              const state = api.states.filter(s => s.runId === claim.run_id && s.body.plan_md !== undefined).at(-1)?.body;
              observations.push({ generated: planMd, persisted: state?.plan_md, status: state?.status });
              return { summary: `Summary ${observations.length}`, deltas: [] };
            },
          },
        }, deps);
        return { executor: { run: async ctx => {
          const gate = ctx.gatePlan!;
          result = await executor.run({ ...ctx, gatePlan: (md, milestones, callback, settles) =>
            gate(md, milestones, callback && (async persisted => {
              callbacks.push(persisted);
              try {
                await callback(persisted);
              } finally {
                attemptSettled[callbacks.length - 1]!.resolve();
              }
            }), settles),
          });
          return result;
        } } };
      }, gitlab).execute(claim);
      // All assertions are outside the runner/executor's swallowed advisory callbacks.
      assert.ok(result, JSON.stringify(api.states));
      assert.equal(api.states.at(-1)?.body.status, "completed");
      const durable = plans.map(plan => plan.replaceAll("\u0000", ""));
      if (mode === "autopilot") {
        assert.deepEqual(callbacks, [plans[0]]);
        assert.deepEqual(observations, [{ generated: plans[0], persisted: durable[0], status: "running" }]);
        assert.deepEqual(attempts, [{ guard: durable[0], persisted: durable[0], status: "running" }]);
        assert.deepEqual(accepted, ["Summary 1"]);
        assert.ok(api.states.some(s => s.body.status === "running" && s.body.plan_md === durable[0]));
        assert.equal(presentations, 0);
      } else {
        assert.deepEqual(callbacks, plans);
        assert.deepEqual(observations, plans.map((generated, i) => ({
          generated, persisted: durable[i], status: "awaiting_approval",
        })));
        assert.deepEqual(attempts, durable.map(guard => ({ guard, persisted: guard, status: "awaiting_approval" })));
        const failed = mode.startsWith("replacement");
        assert.deepEqual(accepted, revised && !failed ? ["Summary 1", "Summary 2"] : ["Summary 1"]);
        assert.equal(result.summaryPlan, failed ? undefined : revised ? "Summary 2" : "Summary 1");
        assert.equal(presentations, plans.length);
      }
    });
  }
});

it("M1 destructive checkpoint waits for summary settlement before reaping", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(28385, { auto_approve: true });
  const settling = deferred(), drained = deferred();
  let active = false, settled = false, prematureReap = false, reaps = 0;
  const executor: Executor = {
    async closeAndSettlePlanSummary() {
      if (!active) return;
      settling.resolve();
      await drained.promise;
      settled = true;
    },
    killAgentTree() {
      if (!active) return;
      if (!settled) prematureReap = true;
      assert.equal(settled, true, "summary drain precedes the destructive action");
      reaps++;
    },
    async run(ctx) {
      await ctx.gatePlan!("# Plan");
      active = true;
      await ctx.checkpoint!({ reap: true });
      active = false;
      return { branch: ctx.branch };
    },
  };
  const execution = runner(executor, gitlab).execute(claim);
  try {
    const reachedSettlement = await Promise.race([
      settling.promise.then(() => true),
      execution.then(() => false),
    ]);
    assert.equal(reachedSettlement, true, "checkpoint must reach settlement before execution ends");
    await flushMicrotasks();
    assert.equal(prematureReap, false, "a blocked drain cannot permit a reap attempt");
    assert.equal(reaps, 0, "a blocked drain cannot permit a reap");
  } finally {
    drained.resolve();
    await execution;
  }
  assert.equal(prematureReap, false, "summary drain precedes every destructive action");
  assert.ok(reaps > 0, "the checkpoint reaps after summary settlement");
});

describe("M1 persisted ACK boundary", () => {
  for (const human of [false, true]) {
    it(`M1 persisted ACK boundary ${human ? "human enters verdict wait and implementation while callback held" : "autopilot enters implementation while callback held"}`, { timeout: 120000 }, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(28382, { auto_approve: !human });
      const ack = deferred(), stored = deferred(), callback = deferred(), started = deferred(), entered = deferred();
      const implementation = deferred();
      const plans: string[] = [];
      let verdictWaits = 0;
      const wait = SteeringChannel.prototype.awaitGateEvent;
      SteeringChannel.prototype.awaitGateEvent = function (...args) {
        verdictWaits++;
        return wait.apply(this, args);
      };
      let execution: Promise<unknown> = Promise.resolve();
      api.afterPersistState(claim.run_id, body => {
        if (body.plan_md !== "# Accepted plan") return false;
        stored.resolve();
        return true;
      }, ack.promise);
      api.setInputs(claim.run_id, [input("approve_plan")]);
      execution = runner({ run: async ctx => {
        const verdict = await ctx.gatePlan!("# Accepted plan", undefined, async md => {
          plans.push(md);
          started.resolve();
          await callback.promise;
        });
        assert.equal(verdict.kind, "approve");
        entered.resolve();
        await implementation.promise;
        return { branch: ctx.branch };
      } }, gitlab).execute(claim);
      try {
        await stored.promise;
        assert.deepEqual(plans, [], "server storage alone cannot admit summary generation");
        ack.resolve();
        let startedTimer: NodeJS.Timeout | undefined;
        try {
          await Promise.race([started.promise, new Promise<never>((_, reject) => {
            startedTimer = setTimeout(() => reject(new Error("accepted ACK did not start persisted callback")), 30000);
          })]);
        } finally { clearTimeout(startedTimer); }
        await flushMicrotasks();
        let timer: NodeJS.Timeout | undefined;
        try {
          await Promise.race([entered.promise, new Promise<never>((_, reject) => {
            timer = setTimeout(() => reject(new Error("implementation blocked by persisted callback")), 30000);
          })]);
        } finally { clearTimeout(timer); }
        assert.deepEqual(plans, ["# Accepted plan"]);
        assert.equal(verdictWaits, human ? 1 : 0);
      } finally {
        ack.resolve();
        callback.resolve();
        implementation.resolve();
        try { await execution; } finally { SteeringChannel.prototype.awaitGateEvent = wait; }
      }
    });
  }

  it("M1 persisted ACK boundary advisory throw still auto-approves", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(28383, { auto_approve: true });
    let generated = 0, entered = false;
    await runner({ run: async ctx => {
      const verdict = await ctx.gatePlan!("# Plan", undefined, async () => {
        generated++;
        throw new Error("advisory failure");
      });
      entered = verdict.kind === "approve";
      return { branch: ctx.branch };
    } }, gitlab).execute(claim);
    assert.equal(generated, 1);
    assert.equal(entered, true);
  });

  for (const failure of ["transport", "unapplied"] as const) {
    it(`M1 persisted ACK boundary ${failure} refuses generation`, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(28384, { auto_approve: true });
      let generated = 0, entered = false;
      const report = client.reportState.bind(client);
      client.reportState = async (...args) => {
        if (args[1].plan_md !== "# Refused plan") return report(...args);
        if (failure === "transport") throw new RequestError("POST", "/state", 400, "refused");
        return { applied: false, status: "cancelled" };
      };
      await runner({ run: async ctx => {
        await ctx.gatePlan!("# Refused plan", undefined, async () => { generated++; });
        entered = true;
        return { branch: ctx.branch };
      } }, gitlab).execute(claim);
      assert.equal(generated, 0);
      assert.equal(entered, false);
    });
  }
});

it("M1 ordinary reasonless human refusal does not invoke persisted hook", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(28381);
  let callbacks = 0;
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => args[1].status === "awaiting_approval"
    ? { applied: false, status: "cancelled" }
    : report(...args);
  api.setInputs(claim.run_id, [input("reject_plan")]);
  await runner({ run: async ctx => {
    await ctx.gatePlan!("# Refused", undefined, async () => { callbacks++; });
    return { branch: ctx.branch };
  } }, gitlab).execute(claim);
  assert.equal(callbacks, 0);
});

describe("RunRunner — plan gate + steering end to end", () => {
  async function retainedCloneApproval(t: TestContext, delayedSetup: boolean): Promise<void> {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1719);
    const clone = worktreeDirFor(1719);
    const scratch = path.join(clone, ".uzi", "scratch", "draft.txt");
    let cloneIdentity: { dev: number; ino: number } | undefined;
    const stub = new StubExecutor(nullLogger(), { planGate: true });
    const executor: Executor = {
      async run(ctx) {
        assert.equal(ctx.worktreePath, clone);
        fs.writeFileSync(scratch, "private draft\n");
        const { dev, ino } = fs.statSync(clone);
        cloneIdentity = { dev, ino };
        const result = await stub.run(ctx);
        assert.deepEqual(
          { dev: fs.statSync(clone).dev, ino: fs.statSync(clone).ino },
          cloneIdentity,
          "approval continued in the retained clone",
        );
        assert.equal(fs.readFileSync(scratch, "utf8"), "private draft\n");
        return result;
      },
    };
    const retainedRunner = runner(executor, gitlab);
    const entered = deferred();
    const release = deferred();
    const ensureClone = git.ensureClone.bind(git);
    if (delayedSetup) {
      git.ensureClone = async (...args) => {
        entered.resolve();
        await release.promise;
        return ensureClone(...args);
      };
      t.mock.timers.enable({ apis: ["Date"], now: Date.now() });
    }
    const cancel = () => {
      t.mock.timers.reset();
      release.resolve();
      retainedRunner.shutdown();
    };
    t.signal.addEventListener("abort", cancel, { once: true });
    const { approval, execution } = approvalWaiter(api, claim.run_id, t.signal, () => retainedRunner.execute(claim));
    try {
      if (delayedSetup) {
        await Promise.race([
          entered.promise,
          approval.then(() => { throw new Error("approval reached before clone setup"); }),
        ]);
        t.mock.timers.tick(3000);
        t.mock.timers.reset();
        release.resolve();
      }
      await approval;
      assert.ok(cloneIdentity, "executor created scratch before approval");
      assert.equal(fs.readFileSync(scratch, "utf8"), "private draft\n");
      assert.deepEqual(
        { dev: fs.statSync(clone).dev, ino: fs.statSync(clone).ino },
        cloneIdentity,
      );
      api.setInputs(claim.run_id, [input("approve_plan")]);
      await execution;
      assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    } finally {
      cancel();
      git.ensureClone = ensureClone;
      await Promise.allSettled([approval, execution]);
      t.signal.removeEventListener("abort", cancel);
    }
  }

  it("keeps scratch in the exact retained runner clone across an approval wait", async (t) => {
    await retainedCloneApproval(t, false);
  });

  it("keeps scratch in the retained runner clone after delayed setup", async (t) => {
    await retainedCloneApproval(t, true);
  });

  it("halts at awaiting_approval, resumes on approve, then completes with an MR", async () => {
    const { gitlab, calls } = fakeGitlab();
    // The fake SDK query commits nothing to the clone; model committed work so the
    // issue-run zero-diff guard does not fail this happy path (issue #279).
    simulateCommittedWork();
    const claim = gitlabClaim(21);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const states = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    const statuses = states.map((s) => s.status);
    assert.ok(
      statuses.includes("awaiting_approval"),
      "run halted at the plan gate",
    );
    assert.ok(statuses.includes("completed"), "run completed after approval");
    const gate = states.find((s) => s.status === "awaiting_approval")!;
    assert.match(gate.plan_md ?? "", /# PLAN/);
    // The plan was surfaced to the run stream as a `plan` message, once.
    const planMsgs = api
      .messages(claim.run_id)
      .filter((m) => m.kind === "plan");
    assert.strictEqual(planMsgs.length, 1);
    // Completed with an MR on the agent branch (never main).
    const completed = states.find((s) => s.status === "completed")!;
    assert.strictEqual(completed.branch, "agent/issue-21");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(
      JSON.parse(calls[0]!.body ?? "{}").source_branch,
      "agent/issue-21",
    );
    // An iteration heartbeat was reported.
    assert.ok(
      states.some((s) => s.status === "running" && s.iteration_count === 1),
    );
  });

  it("stub executor with planGate halts at awaiting_approval, then completes on approve", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, [
      "running",
      "running",
      "awaiting_approval",
      "running",
      "completed",
    ]);
    const gate = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_approval",
    )!.body;
    assert.match(gate.plan_md ?? "", /Stub plan for issue #24/);
    const completed = api.states.find(
      (s) => s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-24");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after approval");
    // The plan reached the run stream exactly once.
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });

  it("auto-approves the plan gate for an autopilot claim: never awaiting_approval, plan still recorded", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(26, { auto_approve: true });
    // No inputs are set: an autopilot run must resolve the gate itself. If it
    // parked at awaiting_approval it would hang until the (disabled) timeout.
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "autopilot run runs to completion",
    );
    // The plan is still recorded as an audit message even though no human saw it.
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
    const completed = api.states.find(
      (s) => s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after auto-approval");
  });

  it("throws on the UZI_STUB_FAIL sentinel after the gate (drives the E2E failure path)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(28, {
      auto_approve: true,
      issue_description: `implement prds/x.md then ${STUB_FAIL_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "auto-approved before it fails",
    );
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "sentinel run should fail");
    assert.match(failed!.body.failure_reason ?? "", /UZI_STUB_FAIL/);
    assert.strictEqual(calls.length, 0, "no MR when the run fails");
    // The plan is recorded before the failure (the throw is AFTER the gate).
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });

  // PRD #88 M5 / Decision 8. Autopilot is "no human in the loop", so a park would
  // wedge the run until its deadline with nobody able to answer. The observable
  // property is that awaiting_input never appears in the reported states at all —
  // not merely that the run finishes.
  it("auto-resolves an ask_user for an autopilot claim: never awaiting_input", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(31, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    // No inputs are set: nothing could ever answer this question.
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_input"),
      "an autopilot run must never park on a question",
    );
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "and still never parks at the plan gate",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "it runs to completion unattended",
    );
    assert.strictEqual(calls.length, 1, "one MR opened");
    // The "no `question` message" half of Decision 8 deliberately lives in its OWN
    // test below rather than as a fifth assertion here — see the comment there.
  });

  /**
   * Decision 8's OTHER half, split out because as an assertion inside the test above
   * it could not fail for the reason it exists.
   *
   * `assert` throws on the first failure, so any fold that makes an autopilot run
   * park — the obvious one, disabling the short-circuit — reddens the FIRST assertion
   * up there and every later one never evaluates. A control run using that fold
   * therefore establishes only *the run does not park*, and establishes **nothing**
   * about *no message claims a human was consulted*, even though both properties are
   * "covered" by a test that goes red. That is the assert-shadowing class: a green
   * suite and a red mutation can BOTH be true of a property nothing has measured.
   *
   * Discriminating fold, and it is not one anybody would reach for by accident:
   * keep the short-circuit intact (no park, no `awaiting_input`, sentinel still
   * returned) and change only the emitted message kind from `status` to `question`
   * in `RunRunner.askUser`'s autopilot branch. Every assertion above still passes;
   * this one reddens alone.
   *
   * The property is security-relevant rather than cosmetic: an unattended run that
   * files a `question` message is claiming on the permanent feed — and, with Slack
   * linked, in the owner's DM — that a human was consulted about a decision no human
   * ever saw.
   */
  it("an autopilot run emits NO question message: nothing may claim a human was consulted", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(34, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const kinds = api.messages(claim.run_id).map((m) => m.kind);
    // Positive control FIRST, so this can never pass by the run having done nothing:
    // the stub echoes an `answer` only on a path that actually reached ctx.askUser.
    assert.ok(
      kinds.includes("answer"),
      "fixture is inert: the run never reached the clarification path at all",
    );
    assert.strictEqual(
      kinds.filter((k) => k === "question").length,
      0,
      "an autopilot run was auto-resolved with a sentinel, so no `question` message " +
        "may reach the feed — it would tell the owner, on the record and in Slack, " +
        "that they were asked something they were never asked",
    );
  });

  // The byte-exact sentinel answer (frozen: the lead is told to record the assumption
  // it made, and an unattended run's guesses have to stay auditable).
  it("hands the autopilot lead the frozen sentinel answer, verbatim", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(32, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const answer = api.messages(claim.run_id).find((m) => m.kind === "answer");
    assert.ok(answer, "the stub echoes the answer it received");
    assert.deepStrictEqual(
      (answer!.payload as { answers?: unknown }).answers,
      [AUTOPILOT_SENTINEL_ANSWER],
      "the sentinel wording is frozen — M5's test and the implementation must agree byte for byte",
    );
  });

  // The human path through the SAME sentinel: the run parks, an answer resumes it.
  // This is M4's pre-run park, which before the REASON_NO_PLAN fix would have failed
  // the run outright rather than parking.
  it("parks a NON-autopilot run on a pre-plan question and resumes it on the answer", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(33, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
    });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    // The answer must name the open question, so it is supplied by the fake API's
    // input queue once the park has stamped an id; the runner mints it, so the test
    // reads it back off the state report rather than guessing.
    const r = runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    );
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await r.execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      statuses.includes("awaiting_input"),
      "the run must park on the pre-plan question",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "and resume to completion on the answer",
    );
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "question").length,
      1,
    );
  });

  // B2, WORKER half.
  //
  // ⚠️ THE RATIONALE THIS COMMENT USED TO CARRY WAS FALSIFIED, AND THE TESTS BELOW
  // WERE NEVER WRONG — ONLY INSUFFICIENT. That combination is why nothing went red.
  //
  // It said: clearing open_question_id in SetRunRunning "breaks that chain at its
  // THIRD step, so the worker never receives a resolved id in the first place", and
  // concluded nothing more was needed. Measured false once M4 landed. M4 lets the
  // lead ask BEFORE it plans, and a pre-run park reaches the plan gate with NO
  // intervening `running` report — so SetRunRunning's clear is never reached and the
  // resolved id survives to awaiting_approval (D-AG). The chain was not broken
  // upstream; it had a second entrance that did not exist when this was written.
  //
  // The durable lesson, and the reason this is spelled out rather than quietly
  // rewritten: a test comment asserting "the chain is broken upstream" has a
  // dependency on upstream that NOTHING TRACKS. No gate fires when the premise stops
  // holding, because the tests it justifies are still passing and still correct. The
  // rationale rots while the code stays right.
  //
  // Both clears are now required, and the invariant they jointly carry is stated in
  // runtime.sql: no setter may leave a resolved id behind. Pinned by
  // TestSetRunRunningClearsOpenQuestionLiveDB and
  // TestSetRunAwaitingApprovalClearsOpenQuestionLiveDB — the second being the one
  // these worker-layer tests structurally cannot express, since they cannot reach a
  // plan gate without a running report.
  //
  // Still true and still the reason there is no worker-side guard: the worker cannot
  // tell a resolved id from a live one (a claim reports status "claimed", never
  // "awaiting_input", because ClaimRun is what set it), so any such guard would be a
  // guess. The server-side clears are the only defence.
  //
  // What the worker layer CAN pin, and must, is the property the fix has to leave
  // intact — the two halves of the id lifecycle:
  it("mints a fresh question id when the claim carries none", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(34, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
    });
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const park = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_input",
    );
    assert.ok(
      park?.body.open_question_id,
      "a park must always report a question id — the api rejects one without",
    );
  });

  // The requeue-survival half: a run re-claimed while genuinely still parked re-parks
  // on the SAME id. This is what makes an answer the user submitted before the worker
  // died still valid, and it is the property every clock-based design failed.
  it("re-parks on the SAME question id the claim delivered", async () => {
    const { gitlab } = fakeGitlab();
    const liveID = "question-the-run-is-still-parked-on";
    const claim = gitlabClaim(35, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
      open_question_id: liveID,
    });
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const park = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_input",
    );
    assert.strictEqual(
      park?.body.open_question_id,
      liveID,
      "a resumed worker must re-park on the id the claim delivered — minting a new one silently " +
        "invalidates an answer the user already submitted against the live question",
    );
  });

  it("still halts a NON-autopilot claim at the gate (auto_approve absent)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(27); // no auto_approve
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, [
      "running",
      "running",
      "awaiting_approval",
      "running",
      "completed",
    ]);
  });

  it("stub executor with planGate fails verbatim when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(25);
    api.setInputs(claim.run_id, [input("reject_plan", "not this way")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.strictEqual(failed!.body.failure_reason, "not this way");
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("fails with the rejection reason (verbatim) when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(22);
    api.setInputs(claim.run_id, [
      input("reject_plan", "please rethink the approach"),
    ]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "run should have failed");
    assert.strictEqual(
      failed!.body.failure_reason,
      "please rethink the approach",
    );
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("propagates a PlanRejectedError as a verbatim failure through a custom executor", async () => {
    const { gitlab } = fakeGitlab();
    const rejectExec: Executor = {
      run: async (ctx) => {
        const v = await ctx.gatePlan!("PLAN");
        if (v.kind === "reject") throw new PlanRejectedError(v.reason);
        return { branch: ctx.branch };
      },
    };
    const claim = gitlabClaim(23);
    api.setInputs(claim.run_id, [input("reject_plan", "no")]);
    await runner(rejectExec, gitlab).execute(claim);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.strictEqual(failed!.body.failure_reason, "no");
  });

  it("reaps the agent tree BEFORE the PAT-bearing push (B1 ordering)", async () => {
    const { gitlab } = fakeGitlab();
    // The inline executor commits nothing; model committed work so the run reaches the
    // push (the issue-run zero-diff guard would otherwise fail it — issue #279).
    simulateCommittedWork();
    const events: string[] = [];
    const exec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch }),
      killAgentTree: () => events.push("kill"),
    };
    const claim = gitlabClaim(31);
    const origPush = git.pushBranch.bind(git);
    (git as unknown as { pushBranch: unknown }).pushBranch = async (
      ...args: unknown[]
    ) => {
      events.push("push");
      return (origPush as (...a: unknown[]) => Promise<void>)(...args);
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      (git as unknown as { pushBranch: unknown }).pushBranch = origPush;
    }
    assert.ok(events.includes("kill") && events.includes("push"));
    assert.ok(
      events.indexOf("kill") < events.indexOf("push"),
      "kill must precede push",
    );
  });

  it("kills a real agent-backgrounded survivor before the run completes (B1)", async () => {
    const { gitlab } = fakeGitlab();
    const survivors: number[] = [];
    // Injected spawn stands in for the SDK CLI spawn, launching a real detached
    // `sleep` in its own group — the kind of survivor a `nohup … &` leaves behind.
    const spawn = (_opts: SpawnOptions): { pid?: number } => {
      const p = spawnDetached({
        command: "sleep",
        args: ["30"],
      } as SpawnOptions);
      if (p.pid) survivors.push(p.pid);
      return p;
    };
    // Real plan→done query that also triggers the spawn hook each turn.
    const scripts: SDKMessage[][] = [
      [
        assistant([
          {
            type: "tool_use",
            id: "p",
            name: "mcp__uzi__submit_plan",
            input: { plan_md: "# P" },
          },
        ]),
        resultOk(),
      ],
      [
        assistant([
          {
            type: "tool_use",
            id: "d",
            name: "mcp__uzi__signal_done",
            input: {},
          },
        ]),
        resultOk(),
      ],
    ];
    let turn = 0;
    const queryFn: SdkQueryFn = (params) => {
      const script = scripts[Math.min(turn, scripts.length - 1)]!;
      turn++;
      return (async function* () {
        params.options.spawnClaudeCodeProcess?.({
          command: "x",
          args: [],
        } as never);
        for await (const _ of params.prompt) {
          /* drain */
        }
        for (const m of script) yield m;
      })();
    };
    const claim = gitlabClaim(32);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    // The fake query commits nothing; model committed work so the run completes past the
    // issue-run zero-diff guard (issue #279).
    simulateCommittedWork();
    const exec = new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn });
    try {
      await runner(exec, gitlab).execute(claim);
      assert.ok(survivors.length >= 1, "at least one survivor was spawned");
      for (const pid of survivors) {
        await waitDead(pid);
        assert.strictEqual(
          isAlive(pid),
          false,
          `survivor ${pid} must be dead after the run`,
        );
      }
      // The run still completed with an MR (the reap did not disturb the happy path).
      assert.ok(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "completed",
        ),
      );
    } finally {
      for (const pid of survivors)
        try {
          process.kill(-pid, "SIGKILL");
        } catch {
          /* already gone */
        }
    }
  });

  it("cancels a live run: a cancel input aborts the executor's signal and fails the run", async () => {
    const { gitlab, calls } = fakeGitlab();
    // An executor that runs until cancelled through ctx.signal.
    const waiter: Executor = {
      run: (ctx) =>
        new Promise((_resolve, reject) => {
          const onAbort = (): void => reject(new Error("run cancelled"));
          if (ctx.signal?.aborted) return onAbort();
          ctx.signal?.addEventListener("abort", onAbort, { once: true });
        }),
    };
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("cancel")]);
    await runner(waiter, gitlab).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "cancelled run reports failed");
    assert.match(failed!.body.failure_reason ?? "", /run cancelled/);
    assert.strictEqual(calls.length, 0, "no MR on cancel");
  });
});

describe("RunRunner — required plan cross-check fallback", () => {
  for (const codexLead of [false, true]) {
    it(`parks a required ${codexLead ? "Codex" : "Claude"} autopilot plan and stops after human refusal`, {
      skip: process.platform !== "linux" && "planning capture uses Linux proc-fd paths; non-Linux refuses before the checker",
    }, async () => {
      client.protocolFeatures = ["claim_generation_fence", "gate_revision_v1"];
      api.gateRevisions = true;
      api.checkedTransport = true;
      api.crossCheckHandler = () => ({ status: 409, body: { reason: "checker_unavailable" } });
      const { gitlab, calls } = fakeGitlab();
      const claim = gitlabClaim(codexLead ? 21492 : 21491, {
        auto_approve: true,
        plan_cross_check_required: true,
        ...(codexLead ? { plan_cross_check_codex_lead: true } : {}),
        claim_generation: 1,
        ...(codexLead ? { secrets: {
          forge_pat: "fixture-forge-pat-000000",
          codex: {
            auth_mode: "subscription" as const,
            access_token: "fixture-codex-access-token-abc123",
            capability: "fixture-codex-capability-abc123",
            generation: 1,
            chatgpt_account_id: "verified-account",
            chatgpt_plan_type: null,
          },
        } } : {}),
      });
      api.setInputs(claim.run_id, [input("reject_plan")]);
      await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);
      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      assert.ok(states.some((s) => s.status === "awaiting_approval" && s.plan_md));
      assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no autopilot approval report");
      assert.ok(states.some((s) => s.status === "failed"), "human refusal terminates the run");
      assert.ok(!states.some((s) => s.iteration_count === 1), "refusal never starts implementation");
      assert.ok(!states.some((s) => s.status === "completed"), "refusal never completes");
      assert.equal(calls.length, 0, "refusal never publishes an MR");
      // PRD #2460: a Codex lead takes the same checked path, so the server's refusal reads the same.
      const reason = "plan cross-check: checker unavailable";
      assert.ok(api.messages(claim.run_id).some((m) => m.kind === "status" && m.payload.text === reason));
    });
  }
});

// PRD #71 M5: the CI-config gate override at the gatePlan call site. An auto_approve
// ci_fix run whose plan is CI-config-classified (CI_CONFIG_MARKER first line) must NOT
// take the auto-approve short-circuit — it parks for human review; a code-plan ci_fix
// run keeps the short-circuit. The executor here returns `not_code` after the gate to
// isolate the gate decision from the (separately unit-tested) push guard.
describe("RunRunner — CI-config ci_fix gate override (PRD #71 M5)", () => {
  const ciFixExec = (plan: string): Executor => ({
    run: async (ctx) => {
      await ctx.gatePlan!(plan);
      // Skip the push/MR entirely so this test observes only the gate decision.
      return { branch: ctx.branch, fixVerdict: "not_code" };
    },
  });

  it("PARKS an auto_approve ci_fix run whose plan is CI-config-classified", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(70, { kind: "ci_fix", auto_approve: true });
    // The park needs a human verdict to resume; supply one so the run terminates.
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      ciFixExec(`${CI_CONFIG_MARKER}\nEdit .gitlab-ci.yml to add the missing job`),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      statuses.includes("awaiting_approval"),
      "a CI-config plan must park even on an auto-triggered ci_fix run",
    );
  });

  it("SHORT-CIRCUITS an auto_approve ci_fix run whose plan is code-only (no marker)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(71, { kind: "ci_fix", auto_approve: true });
    // No inputs: a short-circuited run resolves the gate itself. If it parked it
    // would hang until the (disabled) timeout, so a park would surface as a hang.
    await runner(
      ciFixExec("# PLAN\n- fix the failing unit test in src/foo.ts"),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "a code-plan ci_fix run keeps the auto-approve short-circuit",
    );
    assert.equal(statuses.at(-1), "completed", "and runs to completion unattended");
  });
});

// PRD #71 M5 (load-bearing security): the pre-push CI-config guard on a PRE-APPROVED
// RESUME — the execution where the gate does NOT run, so ciFixHumanApproved is decided
// ENTIRELY by its initializer, not the gatePlan closure. The initializer now reads only
// durable claim state: ciFixHumanApproved = auto_approve !== true. The isCIConfigPlan
// disjunct was REMOVED — it is what let an auto-triggered (auto_approve=true) CI-config
// run whose row was restart-requeued bypass the guard with no human in the loop.
//
// The executor here models the resume: it never calls ctx.gatePlan, writes a CI-config
// file, commits it locally (as the real executor does), and returns a real diff to push,
// so the guard's decision is what the assertions observe. plan_md is CI-config-classified
// in BOTH cases so a regression to the old `|| isCIConfigPlan(...)` disjunct would flip
// the auto_approve=true case green — this is the precise guard against that revert.
describe("RunRunner — CI-config push guard on a pre-approved resume (PRD #71 M5)", () => {
  const resumeCiConfigExec = (): Executor => ({
    run: async (ctx) => {
      const p = path.join(ctx.worktreePath, ".gitlab-ci.yml");
      writeFileSync(p, "job:\n  script: echo edited by an auto ci-fix\n", "utf8");
      const g = (args: string[]) =>
        execFileSync("git", args, { cwd: ctx.worktreePath });
      g(["add", ".gitlab-ci.yml"]);
      g([
        "-c", "user.name=uzi-agent",
        "-c", "user.email=agent@uzi.test",
        "-c", "commit.gpgsign=false",
        "commit", "-m", "auto ci-fix: edit .gitlab-ci.yml",
      ]);
      return { branch: ctx.branch };
    },
  });

  it("auto_approve=false (parked + human-approved) resume: guard INACTIVE, CI-config push proceeds", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(72, {
      kind: "ci_fix",
      auto_approve: false,
      // The server CLEARED auto_approve when the run parked for human review; the human
      // then approved. plan_md is CI-config-classified and must NOT matter to the outcome.
      plan_md: `${CI_CONFIG_MARKER}\nEdit .gitlab-ci.yml to fix the pipeline`,
      config: { ci_config_paths: [".gitlab-ci.yml"] },
    });
    await runner(resumeCiConfigExec(), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      statuses.includes("completed"),
      "a human-approved CI-config fix pushes (a human was in the loop)",
    );
    assert.equal(calls.length, 1, "exactly one MR opened for the human-approved fix");
  });

  it("auto_approve=true (never parked, no human) resume: guard ACTIVE, CI-config push REFUSED", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(73, {
      kind: "ci_fix",
      // Still true on the resume ⇒ the run was AUTO-approved and never parked, so no human
      // ever reviewed it. Under the OLD initializer the CI-config plan_md below would set
      // ciFixHumanApproved=true and BYPASS the guard — the exact hole this closes.
      auto_approve: true,
      plan_md: `${CI_CONFIG_MARKER}\nEdit .gitlab-ci.yml to fix the pipeline`,
      config: { ci_config_paths: [".gitlab-ci.yml"] },
    });
    await runner(resumeCiConfigExec(), gitlab).execute(claim);

    const bodies = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    const statuses = bodies.map((s) => s.status);
    assert.ok(
      !statuses.includes("completed"),
      "an auto-approved CI-config push must never complete (no human in the loop)",
    );
    assert.ok(statuses.includes("failed"), "the guard fails the run CLOSED");
    const failed = bodies.find((s) => s.status === "failed")!;
    assert.match(
      failed.failure_reason ?? "",
      /may not edit CI config/,
      "the refusal names the CI-config guard",
    );
    assert.equal(calls.length, 0, "no MR opened for the refused push");
  });
});

describe("RunRunner — auto ci_fix guard keeps the worker floor and matches raw paths", () => {
  const writeExec = (rel: string): Executor => ({
    run: async (ctx) => {
      const p = path.join(ctx.worktreePath, rel);
      mkdirSync(path.dirname(p), { recursive: true });
      writeFileSync(p, "on: push\n", "utf8");
      const g = (args: string[]) => execFileSync("git", args, { cwd: ctx.worktreePath });
      g(["add", "--", rel]);
      g(["-c", "user.name=uzi-agent", "-c", "user.email=agent@uzi.test", "-c", "commit.gpgsign=false", "commit", "-m", "auto ci-fix"]);
      return { branch: ctx.branch };
    },
  });

  for (const [n, rel, name] of [
    [74, ".forgejo/workflows/ci.yml", "a Forgejo workflow the GitLab-only server set omits"],
    [75, ".github/workflows/a\nb.yml", "a newline-bearing workflow path"],
  ] as const) {
    it(`refuses ${name}`, async () => {
      const { gitlab, calls } = fakeGitlab();
      const claim = gitlabClaim(n, {
        kind: "ci_fix",
        auto_approve: true,
        plan_md: "Fix the failing test",
        config: { ci_config_paths: [".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml"] },
      });
      await runner(writeExec(rel), gitlab).execute(claim);
      const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      const failed = bodies.find((s) => s.status === "failed");
      assert.ok(failed, "the guard fails the run closed");
      assert.match(failed.failure_reason ?? "", /may not edit CI config/);
      assert.equal(calls.length, 0, "no MR opened for the refused push");
    });
  }

  it("refuses a rename of a protected workflow out of its directory", async () => {
    const wf = ".github/workflows/ci.yml";
    mkdirSync(path.join(fx.originPath, ".github/workflows"), { recursive: true });
    writeFileSync(path.join(fx.originPath, wf), "on: push\n", "utf8");
    const og = (args: string[]) => execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@uzi.test", "-c", "commit.gpgsign=false", ...args], { cwd: fx.originPath });
    og(["add", "--", wf]);
    og(["commit", "-m", "add workflow"]);
    const renameExec: Executor = {
      run: async (ctx) => {
        const g = (args: string[]) => execFileSync("git", args, { cwd: ctx.worktreePath });
        g(["mv", wf, "moved.yml"]);
        g(["-c", "user.name=uzi-agent", "-c", "user.email=agent@uzi.test", "-c", "commit.gpgsign=false", "commit", "-m", "auto ci-fix"]);
        return { branch: ctx.branch };
      },
    };
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(76, {
      kind: "ci_fix",
      auto_approve: true,
      plan_md: "Fix the failing test",
      config: { ci_config_paths: [".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml"] },
    });
    await runner(renameExec, gitlab).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body).find((s) => s.status === "failed");
    assert.ok(failed, "the guard fails the run closed");
    assert.match(failed.failure_reason ?? "", /may not edit CI config/);
    assert.equal(calls.length, 0, "no MR opened for the refused push");
  });
});

// PRD #122 M1: the CANDIDATE milestone list rides awaiting_approval (human-gated)
// and the FROZEN list rides the autopilot running report (Decision 2). All additive-
// optional: a plan with no milestones is byte-for-byte unchanged from today.
describe("RunRunner — milestones on the plan gate (PRD #122 M1)", () => {
  const MS = [
    { id: "m1", title: "Wire the protocol" },
    { id: "m2", title: "Plumb the executor" },
  ];

  it("carries the candidate milestones on the awaiting_approval report", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(90);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")!;
    assert.ok(gate, "run halted at the plan gate");
    assert.deepStrictEqual(gate.milestones, MS);
  });

  it("BACK-COMPAT: omits the milestones key entirely when the plan carries none", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(91);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(undefined),
      }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")! as unknown as Record<
      string,
      unknown
    >;
    assert.ok(gate, "run halted at the plan gate");
    assert.ok(
      !("milestones" in gate),
      "no milestones key on the wire (not null/[]), so an old api sees today's shape",
    );
  });

  it("AUTOPILOT: the frozen milestones ride the running report and the run never parks", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(26, { auto_approve: true });
    // No inputs: an autopilot run resolves the gate itself and never awaits a human.
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const bodies = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    assert.ok(
      !bodies.some((s) => s.status === "awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    // The frozen list rides the SAME self-contained running report that carries the
    // autopilot agent_selection (Decision 2).
    const autopilotReport = bodies.find(
      (s) => s.status === "running" && s.agent_selection !== undefined,
    )!;
    assert.ok(autopilotReport, "autopilot agent-selection running report present");
    assert.deepStrictEqual(autopilotReport.milestones, MS);
    // RC1 (#1197): the approved plan rides this SAME running report so the server
    // persists it durably via SetRunAutopilotPlan — an autopilot run never reports
    // awaiting_approval, so this is the only channel that carries plan_md.
    assert.strictEqual(
      autopilotReport.plan_md,
      "# PLAN\n- do it",
      "the autopilot running report carries the submitted plan_md",
    );
  });

  // RC1 (#1197): an autopilot run enters implementation ONLY on proof the plan was
  // durably stored (the running-report ack). If the plan-carrying report does NOT
  // succeed — a 409 (the run moved on / was cancelled or parked concurrently) or a
  // thrown transport/4xx — the run must NOT proceed to implementation and must NOT
  // enter awaiting_approval; it ends in failure. Both cases are forced with the
  // FakeApi.failStateWhen hook, which knocks out ONLY the report carrying the
  // submitted plan_md (the autopilot plan report), leaving the ordinary heartbeats so
  // the run still reaches the gate.
  const isAutopilotPlanReport = (s: StateRequest): boolean =>
    s.status === "running" && s.plan_md === "# PLAN\n- do it";

  it("AUTOPILOT: a 409 on the plan report (run moved on) does NOT enter implementation", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(1197, { auto_approve: true });
    // The plan-carrying running report is refused with a 409 whose body reports the
    // run cancelled — the client reads it as { applied: false, status: "cancelled" }.
    api.failStateWhen(claim.run_id, isAutopilotPlanReport, {
      httpStatus: 409,
      runStatus: "cancelled",
    });
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const bodies = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    const statuses = bodies.map((s) => s.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    assert.ok(
      !statuses.includes("completed"),
      "the run must NOT complete when the plan was not durably stored",
    );
    assert.ok(
      statuses.includes("failed"),
      "the run ends in failure when the plan report was refused",
    );
    // NON-VACUITY #1 (direct proof the run never entered implementation): the implement
    // loop's running reports carry iteration_count >= 1 (reportIteration), whereas the
    // pre-gate/heartbeat reports and the autopilot plan report carry 0/absent. Under the
    // swallow-and-proceed regression this branch guards against (a .catch on the plan
    // report instead of gating the approve on the ack), the run DOES enter the loop, so a
    // body with iteration_count: 1 appears and this assertion reddens — even though the
    // run still ends `failed` downstream on the incidental "no changes were committed"
    // guard, which is exactly why the terminal-status assertions above are not enough.
    assert.ok(
      !bodies.some((s) => (s.iteration_count ?? 0) >= 1),
      "no /state report carries an implementation-loop iteration (iteration_count >= 1): the run never entered implementation",
    );
    // NON-VACUITY #2 (failed for the RIGHT reason): the terminal failure names the
    // plan-storage failure, not the harness's incidental "signal_done ... no changes were
    // committed" guard the reverted code trips after wrongly entering implementation. A 409
    // (applied === false) makes the worker throw `autopilot plan not durably stored — ...`.
    const failed = bodies.find((s) => s.status === "failed")!;
    assert.match(
      failed.failure_reason ?? "",
      /durably stored/,
      "the run fails BECAUSE the plan was not durably stored, not on a downstream incidental guard",
    );
    assert.strictEqual(calls.length, 0, "no MR opened — never entered implementation");
    // The plan audit message is still emitted (it is flushed before the report).
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });

  it("AUTOPILOT: a thrown 4xx on the plan report does NOT enter implementation", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(11970, { auto_approve: true });
    // A 400 on the plan-carrying report: not transient and not "already terminal", so
    // the client throws a RequestError that propagates out of gatePlan.
    api.failStateWhen(claim.run_id, isAutopilotPlanReport, { httpStatus: 400 });
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const bodies = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    const statuses = bodies.map((s) => s.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    assert.ok(
      !statuses.includes("completed"),
      "the run must NOT complete when the plan report threw",
    );
    assert.ok(
      statuses.includes("failed"),
      "the run ends in failure when the plan report threw",
    );
    // NON-VACUITY #1 (direct proof the run never entered implementation): identical to the
    // 409 case — the implement loop's running reports carry iteration_count >= 1, so under
    // the swallow-and-proceed regression a body with iteration_count: 1 appears here and
    // this reddens, while the run still ends `failed` on the incidental commit guard.
    assert.ok(
      !bodies.some((s) => (s.iteration_count ?? 0) >= 1),
      "no /state report carries an implementation-loop iteration (iteration_count >= 1): the run never entered implementation",
    );
    // NON-VACUITY #2 (failed for the RIGHT reason): the propagated RequestError names the
    // REFUSED plan-storage report (a 400 on /state), not the incidental "no changes were
    // committed" guard the reverted code trips downstream. In production the server's 400
    // body carries the m1 "autopilot plan_md was not durably stored" message, but the
    // FakeApi's injected 400 body does not, so this asserts the state-report error shape
    // (`returned 400`) rather than the "durably stored" wording — either way it can only
    // hold on the plan-report-throw path, never on the downstream commit guard.
    const failed = bodies.find((s) => s.status === "failed")!;
    assert.match(
      failed.failure_reason ?? "",
      /returned 400/,
      "the run fails BECAUSE the plan-storage report threw a 400, not on a downstream incidental guard",
    );
    assert.strictEqual(calls.length, 0, "no MR opened — never entered implementation");
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });
});

// PRD #212: the plan-turn changed-file list rides EVERY awaiting_approval report
// (Decision 3: always send, `[]` when clean, so each gate round replaces the server's
// list). The list is produced by git.planChangedFiles, which the gate calls with the
// runner clone path; overriding it on the harness GitCache (precedent: the
// simulateCommittedWork changedFiles override) makes the reported value deterministic.
describe("RunRunner — plan_changed_files on the plan gate (PRD #212)", () => {
  it("carries the runner-uid changed-file list on the awaiting_approval report", async () => {
    const files = [" M src/app.ts", "?? notes.md"];
    git.planChangedFiles = (async () => files) as typeof git.planChangedFiles;
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(212);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")!;
    assert.ok(gate, "run halted at the plan gate");
    assert.deepStrictEqual(gate.plan_changed_files, files);
  });

  it("ALWAYS sends plan_changed_files: [] (not omitted) when the plan turn was clean", async () => {
    // Decision 3: unlike milestones (conditionally spread), the field is always present —
    // an empty list is how a revision round CLEARS a prior round's stale list server-side.
    git.planChangedFiles = (async () => []) as typeof git.planChangedFiles;
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(213);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")! as unknown as Record<
      string,
      unknown
    >;
    assert.ok(gate, "run halted at the plan gate");
    assert.ok(
      "plan_changed_files" in gate,
      "the key is ALWAYS present (Decision 3), never omitted like milestones",
    );
    assert.deepStrictEqual(gate.plan_changed_files, []);
  });
});

describe("#2230 shared SDK process default at a plan gate", () => {
  it("a second scripted runner suite completes despite an incomplete live HOME helper", async (t) => {
    const helper = forceIncompleteHomeHelper(t);
    helper.enabled = true;
    const control = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: planThenDoneQuery(),
      runProcesses: { scan: scanRunProcesses, reap: reapRunProcesses },
    });
    assert.equal((await control.reapAttributedProcesses()).complete, false, "explicit real operations still fail closed");
    assert.equal(helper.calls, 1, "this suite's forced-helper matcher is active");
    helper.enabled = false;
    helper.calls = 0;
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(2230);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    api.onState(claim.run_id, (body) => {
      if (body.iteration_count === 1) helper.enabled = true;
    });
    await runner(new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }), gitlab).execute(claim);
    const states = api.states.filter((state) => state.runId === claim.run_id).map((state) => state.body);
    assert.ok(states.some((state) => state.status === "completed"), JSON.stringify(states));
    assert.equal(helper.calls, 0, "the shared preload covers this suite without per-executor injection");
  });
});

// Issue #2901: a human plan gate starts the advisory plan-summary hook without awaiting it, and
// an accepted revise supersedes the summary of the plan it replaced. Each case runs through the
// real runner gate on both harnesses; only the summary model, the summary POST and the
// provider/query peer are scripted. Waits are on events the test controls, bounded by within()
// with a generous ceiling (loaded hosts), so a regression fails with a named message; production
// settle bounds are shortened through settleMs. Timeouts are per test (PER_TEST_MS), never a
// describe-level budget: node:test shares a suite timeout across its tests, so one exhausted
// suite cancels the rest unnamed and skips their cleanup. A failing test spends at most one
// within() wait plus the bounded teardown in cleanup(), which also releases every held summary
// call and turn so nothing is left running after a failure.

/** One test's budget: comfortably above a failing test's first within() wait plus teardown. */
const PER_TEST_MS = 120000;

type SummaryHarness = "sdk" | "codex";
const SUMMARY_HARNESSES: readonly SummaryHarness[] = ["sdk", "codex"];
const PLAN_USAGE = { "claude-haiku-4-5-20251001": { inputTokens: 3, outputTokens: 1 } } as never;

interface SummaryCall {
  readonly planMd: string;
  /** Report one usage snapshot through the executor's onUsage seam. */
  fireUsage(): void;
  /** Settle the scripted summary model's turn. */
  finish(result?: PlanSummaryResult | null): void;
}

interface GateRigConfig {
  plans: string[];
  generate?: "blocked" | "throws";
  post?: "ok" | "rejects";
  /** The executor's bounded settlement (intentSettleBoundMs / summarySettleMs). */
  settleMs?: number;
  /** Turn indexes (plans first, then the implementation turn) held until released. Default: the implementation turn. */
  holdTurns?: number[];
  config?: Record<string, unknown>;
}

/** Bound a wait that must resolve so a regression fails the test instead of hanging it. */
async function within<T>(promise: Promise<T>, what: string, ms = 30000): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error(`timed out waiting for: ${what}`)), ms);
    })]);
  } finally { clearTimeout(timer); }
}

/** Let already-resolved promise chains and one macrotask turn run. */
async function settleTurns(): Promise<void> {
  await flushMicrotasks();
  await new Promise<void>(resolve => setImmediate(resolve));
  await flushMicrotasks();
}

function gatedSdkQuery(plans: string[], onTurn: (index: number) => Promise<void>): SdkQueryFn {
  let next = 0;
  return params => {
    const index = next++;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      await onTurn(index);
      if (index < plans.length) {
        yield assistant([{ type: "tool_use", id: `p${index}`, name: "mcp__uzi__submit_plan", input: { plan_md: plans[index] } }]);
      } else {
        yield assistant([
          { type: "text", text: "done implementing" },
          { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
        ]);
      }
      yield resultOk();
    })();
  };
}

function startGateRig(harness: SummaryHarness, cfg: GateRigConfig) {
  const { gitlab } = fakeGitlab();
  const codex = { auth_mode: "api_key", access_token: "fixture-codex-access", capability: "fixture-codex-capability" };
  const claim = gitlabClaim(2901, {
    summary_intent: "Existing intent",
    ...(cfg.config ? { config: cfg.config } : {}),
    ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat-000000", codex } } : {}),
  });
  const runId = claim.run_id;
  const settleMs = cfg.settleMs ?? 2000;
  const warns: string[] = [];
  const log: Logger = { ...nullLogger(), warn: msg => { warns.push(msg); }, child: () => log };

  const presented = Array.from({ length: 4 }, deferred);
  let presentations = 0;
  api.onState(runId, body => {
    if (body.status === "awaiting_approval") presented[presentations++]?.resolve();
  });

  const callEntered = Array.from({ length: 4 }, deferred);
  const calls: SummaryCall[] = [];
  let released = false;
  const summaryRunner = {
    generateIntentSummary: async () => null,
    generatePlanSummary: async ({ planMd, onUsage }: { planMd: string; onUsage?: (u: never) => void }) => {
      const index = calls.length;
      if (released) { callEntered[index]?.resolve(); return null; }
      let done!: (r: PlanSummaryResult | null) => void;
      const finished = new Promise<PlanSummaryResult | null>(resolve => { done = resolve; });
      calls.push({
        planMd,
        fireUsage: () => onUsage?.(PLAN_USAGE),
        finish: (result = null) => done(result),
      });
      callEntered[index]!.resolve();
      if (cfg.generate === "throws") throw new Error("summary model failed");
      return finished;
    },
  };

  const posts: string[] = [];
  const posted = Array.from({ length: 4 }, deferred);
  client.postPlanSummary = async (_runId, body: Parameters<WorkerClient["postPlanSummary"]>[1]) => {
    posts.push(body.plan_md);
    posted[posts.length - 1]!.resolve();
    if (cfg.post === "rejects") throw new RequestError("POST", "/summary/plan", 500, "summary persistence failed");
  };

  const implementTurn = cfg.plans.length;
  const turnEntered = Array.from({ length: 6 }, deferred);
  const turnHold = new Map<number, ReturnType<typeof deferred>>();
  for (const index of cfg.holdTurns ?? [implementTurn]) turnHold.set(index, deferred());
  const onTurn = async (index: number): Promise<void> => {
    turnEntered[index]?.resolve();
    await turnHold.get(index)?.promise;
  };

  let executor: SdkExecutor | CodexExecutor;
  if (harness === "sdk") {
    executor = new SdkExecutor(log, homeDir, {
      queryFn: gatedSdkQuery(cfg.plans, onTurn), client,
      summaryRunner: summaryRunner as unknown as SummaryRunner, intentSettleBoundMs: settleMs,
    });
  } else {
    const selected = selectCodexBinding({ codex });
    if (selected.kind !== "codex") throw new Error("invalid fixture binding");
    api.codexAuthModes.set(runId, "api_key");
    executor = new CodexExecutor(log, homeDir, {
      binding: selected.binding, client,
      provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
      summaryRunner: summaryRunner as unknown as SummaryRunner, summarySettleMs: settleMs,
    }, codexSummaryDeps(cfg.plans, onTurn));
  }

  let result: ExecutorResult | undefined;
  const run = executor.run.bind(executor);
  executor.run = async ctx => (result = await run(ctx));

  // Hooks around the executor's bounded summary settlement: `during` runs right after admission
  // closes and flights are aborted, before any flight settles or is abandoned; `after` runs once
  // the settlement returned (flights settled or abandoned), before the runner closes its batcher.
  const settle: { during?: () => void; after?: () => void } = {};
  const closeAndSettle = executor.closeAndSettlePlanSummary.bind(executor);
  executor.closeAndSettlePlanSummary = async deadlineMs => {
    const during = settle.during;
    settle.during = undefined;
    const settling = closeAndSettle(deadlineMs);
    during?.();
    await settling;
    const after = settle.after;
    settle.after = undefined;
    after?.();
  };

  const runner = runnerWith(() => ({ executor }), gitlab);
  let finished = false;
  const execution = runner.execute(claim).finally(() => { finished = true; });

  return {
    claim, calls, posts, posted, warns, settle, presented, callEntered, turnEntered, turnHold,
    implementing: turnEntered[implementTurn]!.promise,
    execution,
    result: () => result,
    verdict: (kind: "approve_plan" | "revise_plan" | "reject_plan" | "cancel", body?: string) =>
      api.appendInputs(runId, [input(kind, body)]),
    /** The real claim-release path: the receipt fence ends the flight and aborts its lifecycle. */
    fence: () => (runner as unknown as {
      activeRuns: Map<string, { steering: { endFencedFlight(reason: string): void } }>;
    }).activeRuns.get(runId)!.steering.endFencedFlight("released"),
    planUsage: () => api.messages(runId).filter(
      m => m.kind === "summary_usage" && (m.payload as { pass?: string }).pass === "plan").length,
    async cleanup(): Promise<void> {
      released = true;
      for (const hold of turnHold.values()) hold.resolve();
      for (const call of calls) call.finish(null);
      if (!finished) api.appendInputs(runId, [input("cancel")]);
      try {
        await within(execution.catch(() => {}), "run teardown");
      } catch (err) {
        // Keep the test's own named failure; a stuck teardown is reported on stderr instead.
        console.error(String(err));
      }
    },
  };
}

const S1: PlanSummaryResult = { summary: "Summary one", deltas: [] };
const S2: PlanSummaryResult = { summary: "Summary two", deltas: [] };

for (const harness of SUMMARY_HARNESSES) {
  describe(`issue #2901 (${harness}): human plan gate does not wait for the plan summary`, () => {
    it("approve is processed while the summary hook is still blocked, then the summary latches", { timeout: PER_TEST_MS }, async () => {
      const rig = startGateRig(harness, { plans: ["# Plan one"] });
      try {
        await within(rig.presented[0]!.promise, "first plan presentation");
        await within(rig.callEntered[0]!.promise, "summary hook start");
        rig.verdict("approve_plan");
        await within(rig.implementing, "implementation turn while the summary hook is blocked");
        assert.deepEqual(rig.posts, [], "the blocked hook has posted nothing yet");
        rig.calls[0]!.finish(S1);
        await within(rig.posted[0]!.promise, "summary POST");
        assert.deepEqual(rig.posts, ["# Plan one"]);
        rig.turnHold.get(1)!.resolve();
        await within(rig.execution, "run completion");
        assert.equal(rig.result()?.summaryPlan, "Summary one");
      } finally { await rig.cleanup(); }
    });

    for (const identical of [false, true]) {
      it(`revise (${identical ? "identical" : "different"} revised text): the superseded flight neither POSTs nor latches`, { timeout: PER_TEST_MS }, async () => {
        const plans = ["# Plan one", identical ? "# Plan one" : "# Plan two"];
        // Hold the revision turn so the superseded flight is released between the accepted revise
        // and the revised plan's presentation.
        const rig = startGateRig(harness, { plans, holdTurns: [1, 2] });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "first summary hook start");
          rig.verdict("revise_plan", "Revise the plan");
          await within(rig.turnEntered[1]!.promise, "revision turn after the accepted revise");
          rig.calls[0]!.finish(S1);
          await settleTurns();
          assert.deepEqual(rig.posts, [], "a superseded flight admits no POST");
          rig.turnHold.get(1)!.resolve();
          await within(rig.presented[1]!.promise, "revised plan presentation");
          await within(rig.callEntered[1]!.promise, "revised summary hook start");
          assert.equal(rig.calls[1]!.planMd, plans[1]);
          rig.verdict("approve_plan");
          await within(rig.turnEntered[2]!.promise, "implementation turn while the revised hook is blocked");
          rig.calls[1]!.finish(S2);
          await within(rig.posted[0]!.promise, "revised summary POST");
          assert.deepEqual(rig.posts, [plans[1]]);
          rig.turnHold.get(2)!.resolve();
          await within(rig.execution, "run completion");
          assert.equal(rig.result()?.summaryPlan, "Summary two");
        } finally { await rig.cleanup(); }
      });
    }

    for (const verdict of ["reject", "cancel", "fence"] as const) {
      it(`${verdict}: teardown completes within the settle bound and nothing is posted or latched`, { timeout: PER_TEST_MS }, async () => {
        const rig = startGateRig(harness, { plans: ["# Plan one"], settleMs: 20 });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          if (verdict === "reject") rig.verdict("reject_plan", "no");
          else if (verdict === "cancel") rig.verdict("cancel");
          else rig.fence();
          // The scripted model ignores the abort, so only the bound ends the settlement.
          await within(rig.execution, "bounded teardown");
          assert.ok(rig.warns.includes("plan summary did not settle within the bound"));
          rig.calls[0]!.finish(S1);
          await settleTurns();
          assert.deepEqual(rig.posts, [], "no POST is admitted after close");
          assert.equal(rig.result()?.summaryPlan, undefined);
        } finally { await rig.cleanup(); }
      });
    }

    for (const failure of ["generate", "post"] as const) {
      it(`failing hook (${failure}): the failure is logged and the run proceeds`, { timeout: PER_TEST_MS }, async () => {
        const rig = startGateRig(harness, {
          plans: ["# Plan one"], holdTurns: [],
          generate: failure === "generate" ? "throws" : "blocked",
          post: failure === "post" ? "rejects" : "ok",
        });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          if (failure === "post") {
            rig.calls[0]!.finish(S1);
            await within(rig.posted[0]!.promise, "rejected summary POST");
          }
          rig.verdict("approve_plan");
          await within(rig.execution, "run completion");
          assert.ok(rig.warns.includes("plan summary hook failed"));
          assert.equal(rig.result()?.summaryPlan, undefined);
        } finally { await rig.cleanup(); }
      });
    }

    if (harness === "sdk") {
      it("revision budget exhausted: the re-presented same plan keeps its latched summary", { timeout: PER_TEST_MS }, async () => {
        const rig = startGateRig(harness, { plans: ["# Plan one"], holdTurns: [], config: { plan_max_revisions: 0 } });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          rig.calls[0]!.finish(S1);
          await within(rig.posted[0]!.promise, "summary POST");
          rig.verdict("revise_plan", "Revise the plan");
          await within(rig.presented[1]!.promise, "same plan re-presented");
          rig.verdict("approve_plan");
          await within(rig.execution, "run completion");
          assert.equal(rig.calls.length, 1, "the re-presented same plan is not summarised again");
          assert.deepEqual(rig.posts, ["# Plan one"]);
          assert.equal(rig.result()?.summaryPlan, "Summary one");
        } finally { await rig.cleanup(); }
      });
    }
  });

  describe(`issue #2901 (${harness}): plan-summary usage boundaries`, () => {
    // Each case is a fresh flight firing exactly one usage event, so the once-per-flight
    // `delivered` latch can never mask a missing guard.
    for (const before of [true, false]) {
      it(`supersession (${before ? "+" : "-"}): usage ${before ? "before" : "only after"} the accepted revise is ${before ? "reported" : "not reported"}`, { timeout: PER_TEST_MS }, async () => {
        const plans = ["# Plan one", "# Plan two"];
        const rig = startGateRig(harness, { plans, holdTurns: [1], settleMs: 20 });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          if (before) rig.calls[0]!.fireUsage();
          rig.verdict("revise_plan", "Revise the plan");
          await within(rig.turnEntered[1]!.promise, "revision turn after the accepted revise");
          if (!before) rig.calls[0]!.fireUsage();
          rig.turnHold.get(1)!.resolve();
          await within(rig.presented[1]!.promise, "revised plan presentation");
          rig.verdict("approve_plan");
          rig.turnHold.get(2)?.resolve();
          await within(rig.execution, "run completion");
          assert.equal(rig.planUsage(), before ? 1 : 0);
        } finally { await rig.cleanup(); }
      });
    }

    for (const before of [true, false]) {
      it(`fencing (${before ? "+" : "-"}): usage ${before ? "before" : "after"} the claim is fenced is ${before ? "reported" : "not reported"}`, { timeout: PER_TEST_MS }, async () => {
        const rig = startGateRig(harness, { plans: ["# Plan one"], settleMs: 2000 });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          if (before) rig.calls[0]!.fireUsage();
          // Within the settle bound: the flight is still pending when the fenced teardown starts.
          rig.settle.during = () => {
            if (!before) rig.calls[0]!.fireUsage();
            rig.calls[0]!.finish(null);
          };
          rig.fence();
          await within(rig.execution, "fenced teardown");
          assert.equal(rig.planUsage(), before ? 1 : 0);
        } finally { await rig.cleanup(); }
      });
    }

    it("bounded settlement (+): usage from a flight settling within the bound is reported", { timeout: PER_TEST_MS }, async () => {
      const rig = startGateRig(harness, { plans: ["# Plan one"], settleMs: 2000 });
      try {
        await within(rig.presented[0]!.promise, "first plan presentation");
        await within(rig.callEntered[0]!.promise, "summary hook start");
        rig.settle.during = () => { rig.calls[0]!.fireUsage(); rig.calls[0]!.finish(null); };
        rig.verdict("reject_plan", "no");
        await within(rig.execution, "rejected teardown");
        assert.equal(rig.planUsage(), 1);
      } finally { await rig.cleanup(); }
    });

    it("bounded settlement (-): usage after the bound expired and the flight was abandoned is not reported", { timeout: PER_TEST_MS }, async () => {
      const rig = startGateRig(harness, { plans: ["# Plan one"], settleMs: 20 });
      try {
        await within(rig.presented[0]!.promise, "first plan presentation");
        await within(rig.callEntered[0]!.promise, "summary hook start");
        // The settlement returned with the flight abandoned; the runner has not closed its batcher yet.
        rig.settle.after = () => rig.calls[0]!.fireUsage();
        rig.verdict("reject_plan", "no");
        await within(rig.execution, "rejected teardown");
        assert.ok(rig.warns.includes("plan summary did not settle within the bound"));
        assert.equal(rig.planUsage(), 0);
      } finally { await rig.cleanup(); }
    });

    for (const how of ["superseded", "fenced"] as const) {
      it(`combined: usage during settlement of an already ${how} flight is not reported`, { timeout: PER_TEST_MS }, async () => {
        const plans = how === "superseded" ? ["# Plan one", "# Plan two"] : ["# Plan one"];
        const rig = startGateRig(harness, { plans, holdTurns: [], settleMs: 50 });
        try {
          await within(rig.presented[0]!.promise, "first plan presentation");
          await within(rig.callEntered[0]!.promise, "summary hook start");
          if (how === "superseded") {
            rig.verdict("revise_plan", "Revise the plan");
            await within(rig.presented[1]!.promise, "revised plan presentation");
          }
          rig.settle.during = () => { rig.calls[0]!.fireUsage(); rig.calls[0]!.finish(null); };
          if (how === "superseded") rig.verdict("reject_plan", "no");
          else rig.fence();
          await within(rig.execution, "teardown");
          assert.equal(rig.planUsage(), 0);
        } finally { await rig.cleanup(); }
      });
    }
  });
}
