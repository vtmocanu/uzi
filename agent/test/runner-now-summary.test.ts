// PRD #2603 M1: the RunRunner wiring of the Now summary controller, end to end through a real
// claim against FakeApi: the setting rides the /inputs poll, the run's own emit/reportProgress
// feed the controller, the note lands on the run's message stream, and a run without the setting,
// the collaborators or the Claude harness makes no model call at all.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import type { Executor, RunContext } from "../src/executor.js";
import { GitLabClient, type FetchFn } from "../src/forge.js";
import { RunRunner } from "../src/runner.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { CodexExecutor, CODEX_PRODUCTION_PROVIDER, createCodexEpochAuthority, type CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import { selectCodexExecutionAuthority } from "../src/codex/select.js";
import { ownedSummaryFactories, summaryOwnership } from "../src/codex/summary-ownership.js";
import type { AdviceRequest, AdviceResult, AdviceResultPolicy } from "../src/harness.js";

const TOKEN = "tkn-nowsummary-123456";
const HAIKU = "claude-haiku-4-5-20251001";

let api: FakeApi;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
});

afterEach(async () => {
  try {
    await api.close();
  } finally {
    fx.cleanup();
  }
});

function fakeGitlab(): GitLabClient {
  const fetchFn: FetchFn = async () => ({
    status: 201,
    text: async () => JSON.stringify({ iid: 42, web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42" }),
  });
  return new GitLabClient({ fetchFn });
}

function summaryQuery(calls: { options: unknown; prompt: unknown }[]): SdkQueryFn {
  return ((params: { options: unknown; prompt: unknown }) => {
    calls.push(params);
    return (async function* () {
      yield { type: "assistant", message: { id: "msg1", model: HAIKU, role: "assistant", content: [{ type: "text", text: "Running the api gate for the docs milestone" }], usage: { input_tokens: 21, output_tokens: 8 } } };
      yield { type: "result", subtype: "success", is_error: false, modelUsage: { [HAIKU]: { inputTokens: 21, outputTokens: 8, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0003 } } };
    })();
  }) as unknown as SdkQueryFn;
}

/** summaryQuery that also drains the streamed prompt into `texts` (the model pass sends an async iterable). */
function promptCapturingQuery(texts: string[]): SdkQueryFn {
  const inner = summaryQuery([]);
  return ((params: { options: unknown; prompt: unknown }) => {
    return (async function* () {
      let text = "";
      if (typeof params.prompt === "string") text = params.prompt;
      else for await (const m of params.prompt as AsyncIterable<unknown>) text += JSON.stringify(m);
      texts.push(text);
      yield* (inner as unknown as (p: unknown) => AsyncIterable<unknown>)(params);
    })();
  }) as unknown as SdkQueryFn;
}

function runnerFor(executor: Executor, nowSummary?: { homeRoot: string; queryFn: SdkQueryFn; codexAdviceHarnessFactory?: CodexAdviceHarnessFactory }): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    gitlab: fakeGitlab(),
    ...(nowSummary ? { nowSummary } : {}),
  });
}

const claimFor = (iid: number, overrides = {}) =>
  makeClaim({
    issue_iid: iid,
    issue_title: `Fix thing ${iid}`,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "oauth-fixture-token" },
    ...overrides,
  });

function commitMarker(worktreePath: string): void {
  fs.writeFileSync(path.join(worktreePath, "UZI_RUN.md"), "# now summary\n");
  execFileSync("git", ["add", "UZI_RUN.md"], { cwd: worktreePath });
  execFileSync("git", ["-c", "user.name=uzi-agent", "-c", "user.email=uzi-agent@uzi.local", "-c", "commit.gpgsign=false", "commit", "-m", "uzi test"], { cwd: worktreePath });
}

const notesOf = (runId: string) => api.messages(runId).filter((m) => m.kind === "progress_note");

/** An executor that works milestone m1: it emits tool frames and reports progress until `done()`
 *  or `maxMs`, the way a live lead does, then commits. */
function workingExecutor(done: () => boolean, maxMs = 4000): Executor {
  return {
    async run(ctx: RunContext) {
      const frozen = [{ id: "m1", title: "Write the docs" }] as never;
      await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, frozen);
      const until = Date.now() + maxMs;
      while (!done() && Date.now() < until) {
        ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "task gate:api", description: "Run the api gate" } } });
        await new Promise((r) => setTimeout(r, 25));
      }
      commitMarker(ctx.worktreePath);
      return { branch: ctx.branch };
    },
  };
}

describe("RunRunner Now summary wiring (PRD #2603)", () => {
  for (const kind of ["self_improve", "prompt"] as const) {
  it(`M1 Now runner ${kind} eligibility`, async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2603, { kind });
    api.nowSummary.set(claim.run_id, true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      // Publication ends the executor early; retain the helper's bounded allowance under CPU stalls.
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1, "one call per 5 minute window");
    const p = notes[0]!.payload as Record<string, unknown>;
    assert.equal(p.text, "Running the api gate for the docs milestone");
    assert.equal(p.milestone_id, "m1");
    assert.deepEqual(Object.keys(p).sort(), ["milestone_id", "model_usage", "text"]);
    assert.deepEqual((p.model_usage as Record<string, unknown>)[HAIKU], { inputTokens: 21, outputTokens: 8, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0003 });
    assert.equal(notes[0]!.agent, "worker");
    assert.equal(calls.length, 1);
    const opts = calls[0]!.options as { model?: string; env?: Record<string, string>; thinking?: unknown };
    assert.equal(opts.model, "haiku");
    assert.equal(opts.env?.CLAUDE_CODE_MAX_OUTPUT_TOKENS, "256");
    assert.deepEqual(opts.thinking, { type: "disabled" });
    assert.equal(opts.env?.CLAUDE_CODE_OAUTH_TOKEN, "oauth-fixture-token", "the run's own credential");
  });

  }
  it("posts one progress_note with the usage on the run's stream when the poll says the setting is on", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2603);
    api.nowSummary.set(claim.run_id, true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1, "one call per 5 minute window");
    const p = notes[0]!.payload as Record<string, unknown>;
    assert.equal(p.text, "Running the api gate for the docs milestone");
    assert.equal(p.milestone_id, "m1");
    assert.deepEqual(Object.keys(p).sort(), ["milestone_id", "model_usage", "text"]);
    assert.deepEqual((p.model_usage as Record<string, unknown>)[HAIKU], { inputTokens: 21, outputTokens: 8, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0003 });
    assert.equal(notes[0]!.agent, "worker");
    assert.equal(calls.length, 1);
    const opts = calls[0]!.options as { model?: string; env?: Record<string, string>; thinking?: unknown };
    assert.equal(opts.model, "haiku");
    assert.equal(opts.env?.CLAUDE_CODE_MAX_OUTPUT_TOKENS, "256");
    assert.deepEqual(opts.thinking, { type: "disabled" });
    assert.equal(opts.env?.CLAUDE_CODE_OAUTH_TOKEN, "oauth-fixture-token", "the run's own credential");
  });

  it("a poll that turns the setting off clears the shown note at once", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2603);
    api.nowSummary.set(claim.run_id, true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    const executor: Executor = {
      async run(ctx: RunContext) {
        const frozen = [{ id: "m1", title: "Write the docs" }] as never;
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, frozen);
        const emitFrame = () => ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
        const until = Date.now() + 4000;
        while (!notesOf(claim.run_id).some((m) => (m.payload as { text?: string }).text) && Date.now() < until) {
          emitFrame();
          await new Promise((r) => setTimeout(r, 25));
        }
        api.nowSummary.set(claim.run_id, false);
        // Quiet: no frames, so only the poll can clear the note.
        const quietUntil = Date.now() + 3000;
        while (!notesOf(claim.run_id).some((m) => (m.payload as { text?: string }).text === "" && !(m.payload as { model_usage?: unknown }).model_usage) && Date.now() < quietUntil) {
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    try {
      await runnerFor(executor, { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id).map((m) => m.payload as Record<string, unknown>);
    assert.equal(notes[0]!.text, "Running the api gate for the docs milestone");
    const clears = notes.filter((p) => p.text === "" && p.model_usage === undefined);
    assert.equal(clears.length, 1);
    assert.equal(clears[0]!.milestone_id, "m1");
  });

  it("makes no call when the poll never says on (an absent now_summary is off)", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2604);
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(calls.length, 0);
    assert.equal(notesOf(claim.run_id).length, 0);
  });

  it("makes no call when the setting is on but the runner has no collaborators (stub executor, tests)", async () => {
    const claim = claimFor(2605);
    api.nowSummary.set(claim.run_id, true);
    let frames = 0;
    await runnerFor(workingExecutor(() => ++frames > 40, 3000)).execute(claim);
    assert.equal(notesOf(claim.run_id).length, 0);
  });

  const CODEX_SECRETS = {
    forge_pat: "fixture-forge-pat-000000",
    // Carries an Anthropic token too, so ONLY the Codex term of the claim gate decides.
    anthropic_oauth_token: "oauth-fixture-token",
    codex: { auth_mode: "subscription" as const, access_token: "claim-key", capability: "cap", generation: 3, chatgpt_account_id: "verified-account", chatgpt_plan_type: null },
  };

  for (const stop of ["deadline", "lifecycle", "cancel"] as const) {
    it(`M2 Codex gate wait ${stop} before admission retains A`, async (t) => {
      const claim = claimFor(2631, { secrets: CODEX_SECRETS, claim_generation: 1 });
      let bounded = false;
      let aborted = false;
      let heldAfterStop = false;
      let worktree = "";
      let continued = false;
      let deadlineFire: (() => void) | undefined;
      const runner = runnerFor({
        async run(ctx) {
          worktree = ctx.worktreePath;
          fs.writeFileSync(path.join(worktree, "retained.txt"), "waiting A work");
          const internal = runner as unknown as {
            runGatedSink(flight: { sinkGate: import("../src/sink-gate.js").SinkGate; steering: SteeringChannel }, fn: () => Promise<unknown>,
              bound?: { stop: { signal: AbortSignal }; deadlineAt: number }): Promise<unknown>;
          };
          const original = internal.runGatedSink.bind(runner);
          const timer = globalThis.setTimeout;
          const mocked = t.mock.method(globalThis, "setTimeout", ((fn: () => void, ms: number, ...args: unknown[]) => {
            if (ms === 10_000) {
              deadlineFire = fn;
              return { unref() { return this; } } as unknown as ReturnType<typeof setTimeout>;
            }
            return Reflect.apply(timer, globalThis, [fn, ms, ...args]) as ReturnType<typeof setTimeout>;
          }) as typeof setTimeout);
          internal.runGatedSink = async (flight, fn, bound) => {
            bounded = !!bound && bound.deadlineAt <= Date.now() + 10_000;
            const release = flight.sinkGate.tryAcquire(() => {
              if (stop === "deadline") deadlineFire?.();
              else if (stop === "cancel") (flight.steering as unknown as { commitCancel(): void }).commitCancel();
            else (runner as unknown as { shutdownSignal: AbortController }).shutdownSignal.abort();
              aborted = bound?.stop.signal.aborted === true;
              if (!bound) release!();
            });
            assert.ok(release);
            try { return await original(flight, fn, bound); }
            finally { heldAfterStop = flight.sinkGate.isHeld(); release(); }
          };
          try {
            await ctx.attemptCodexCredentialSwitch!({ site: "implement_turn", laneSettled: true });
            continued = true;
          } finally {
            internal.runGatedSink = original;
            mocked.mock.restore();
          }
          throw new Error("switch unexpectedly continued past gate stop");
        },
      });
      await runner.execute(claim);
      assert.equal(bounded, true);
      assert.equal(aborted, true);
      assert.equal(heldAfterStop, true, "waiting switch must not release tick custody");
      assert.equal(continued, false);
      assert.equal(fs.readFileSync(path.join(worktree, "retained.txt"), "utf8"), "waiting A work");
      const failures = api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed");
      assert.deepEqual(failures.map(s => s.body.failure_reason), stop === "cancel" ? ["run cancelled"] : []);
    });

  }

  for (const stop of ["deadline", "cancel", "fence"] as const) {
    it(`M2 raw teardown remains owned through ${stop}`, async (t) => {
      const claim = claimFor(2632, { secrets: CODEX_SECRETS, claim_generation: 1 });
      let release!: () => void;
      let entered!: () => void;
      const barrier = new Promise<void>(resolve => { release = resolve; });
      const started = new Promise<void>(resolve => { entered = resolve; });
      let pending = false;
      let rawDone = false;
      let runDone = false;
      let hookDone = false;
      let gateDone = false;
      let worktree = "";
      let interrupt!: () => void;
      let deadlineFire!: () => void;
      let steering!: SteeringChannel;
      let sink!: { isHeld(): boolean };
      const timer = globalThis.setTimeout;
      t.mock.method(globalThis, "setTimeout", ((fn: () => void, ms: number, ...args: unknown[]) => {
        if (ms === 30_000) {
          deadlineFire = fn;
          return { unref() { return this; } } as unknown as ReturnType<typeof setTimeout>;
        }
        return Reflect.apply(timer, globalThis, [fn, ms, ...args]);
      }) as typeof setTimeout);
      client.registrationNonce = () => "original-nonce";
      client.supportsInPlaceCodexCredentialSwitch = () => true;
      client.supportsCodexSwitchStopped = () => true;
      client.getRunOwnership = async () => ({ status: "running", claim_generation: 1, claim_released: false, hold_reason: null });
      const operation = { generation: 1, op_id: "current-op", target_secret_id: "target" };
      client.getInputs = async () => ({ inputs: [], ...(pending ? { credentialSwitch: operation } : {}) });
      client.preflightCodexCredentialSwitch = async () => ({
        kind: "ready", expectedEpoch: 11, claimGeneration: 1, registerNonce: "original-nonce", opId: "current-op",
      });
      client.applyCredentialSwitch = async () => { assert.fail("apply after interruption"); };
      const report = client.reportState.bind(client);
      client.reportState = (async (id, body, ...args) => {
        if (body.status === "credential_switch_hold_settled") assert.equal(rawDone, true);
        assert.ok(!["credential_switch_failed", "credential_switch_uncertain"].includes(body.status),
          "interruption must not mutate the server operation");
        return report(id, body, ...args);
      }) as typeof client.reportState;
      const runner = runnerFor({
        // Deliberately no optional joinOwnedOperations: the lease owns this promise.
        suspendCodexEpoch: async () => ({
          reconcileA: async () => ({ kind: "ready" }),
          persistRollout: async () => ({ kind: "usable", sessionId: "saved-A" }),
          reopenA: async () => { assert.fail("A reopened"); },
          teardown: async () => { entered(); await barrier; rawDone = true; return true; },
          publishB: async () => { assert.fail("B published"); },
          restartAndResume: async () => { assert.fail("B resumed"); },
          startFreshLineage: async () => { assert.fail("B started"); },
          finishB: async () => { assert.fail("B finished"); },
        }),
        async run(ctx) {
          worktree = ctx.worktreePath;
          fs.writeFileSync(path.join(worktree, "retained.txt"), "A work");
          pending = true;
          const internal = runner as unknown as {
            runGatedSink(flight: { steering: SteeringChannel; sinkGate: typeof sink }, fn: () => Promise<unknown>, bound?: unknown): Promise<unknown>;
          };
          const original = internal.runGatedSink.bind(runner);
          internal.runGatedSink = async (flight, fn, bound) => {
            steering = flight.steering;
            sink = flight.sinkGate;
            try { return await original(flight, fn, bound); }
            finally { gateDone = true; }
          };
          interrupt = () => {
            if (stop === "cancel") (steering as unknown as { commitCancel(): void }).commitCancel();
            else if (stop === "fence") client.registrationNonce = () => "successor-nonce";
            deadlineFire();
          };
          try { await ctx.attemptCodexCredentialSwitch!({ site: "implement_turn", laneSettled: true }); }
          finally { hookDone = true; internal.runGatedSink = original; }
          assert.fail("A continued");
        },
      });
      (runner as unknown as { credentialSwitchLedger: unknown }).credentialSwitchLedger = {
        stateOf: async () => undefined,
        markSending: async () => { assert.fail("ledger apply after interruption"); },
      };
      const running = runner.execute(claim).finally(() => { runDone = true; });
      await started;
      interrupt();
      // Event-loop barrier lets the abort race and finalizers run without a timed sleep.
      await new Promise<void>(resolve => setImmediate(resolve));
      const beforeRelease = { hookDone, gateDone, runDone, held: sink.isHeld() };
      release();
      await running;
      assert.deepEqual(beforeRelease, { hookDone: false, gateDone: false, runDone: false, held: true });
      assert.equal(rawDone, true);
      assert.deepEqual(operation, { generation: 1, op_id: "current-op", target_secret_id: "target" });
      assert.equal(fs.readFileSync(path.join(worktree, "retained.txt"), "utf8"), "A work");
    });
  }

  for (const phase of ["publish", "resume", "fresh", "finish"] as const) {
    for (const rejectRaw of [false, true]) {
      for (const stop of ["deadline", "cancel", "fence"] as const) {
        it(`M2 raw B ${phase} ${rejectRaw ? "reject" : "resolve"} remains owned through ${stop}`, async (t) => {
          const claim = claimFor(2632, { secrets: CODEX_SECRETS, claim_generation: 1 });
          let release!: () => void;
          let entered!: () => void;
          const barrier = new Promise<void>(resolve => { release = resolve; });
          const started = new Promise<void>(resolve => { entered = resolve; });
          let pending = false;
          let rawDone = false;
          let runDone = false;
          let hookDone = false;
          let gateDone = false;
          let worktree = "";
          let interrupt!: () => void;
          let deadlineFire!: () => void;
          let steering!: SteeringChannel;
          let sink!: { isHeld(): boolean };
          const timer = globalThis.setTimeout;
          t.mock.method(globalThis, "setTimeout", ((fn: () => void, ms: number, ...args: unknown[]) => {
            if (ms === 30_000) {
              deadlineFire = fn;
              return { unref() { return this; } } as unknown as ReturnType<typeof setTimeout>;
            }
            return Reflect.apply(timer, globalThis, [fn, ms, ...args]);
          }) as typeof setTimeout);
          client.registrationNonce = () => "original-nonce";
          client.supportsInPlaceCodexCredentialSwitch = () => true;
          client.supportsCodexSwitchStopped = () => true;
          client.getRunOwnership = async () => ({ status: "running", claim_generation: 1, claim_released: false, hold_reason: null });
          const operation = { generation: 1, op_id: "current-op", target_secret_id: "target" };
          client.getInputs = async () => ({ inputs: [], ...(pending ? { credentialSwitch: operation } : {}) });
          client.preflightCodexCredentialSwitch = async () => ({
            kind: "ready", expectedEpoch: 11, claimGeneration: 1, registerNonce: "original-nonce", opId: "current-op",
          });
          client.applyCredentialSwitch = async () => {
            pending = false;
            return { kind: "codex_applied", codex: { ...CODEX_SECRETS.codex!, access_token: "B-access", capability: "B-capability" },
              ordinal: 1, capabilityEpoch: 42, messageSeq: 7, replay: false };
          };
          const waitRaw = async () => {
            entered(); await barrier; rawDone = true;
            if (rejectRaw) throw new Error("raw B failed");
          };
          const report = client.reportState.bind(client);
          client.reportState = (async (id, body, ...args) => {
            if (body.status === "credential_switch_hold_settled") assert.equal(rawDone, true);
            assert.ok(!["credential_switch_failed", "credential_switch_uncertain"].includes(body.status),
              "interruption must not mutate the server operation");
            return report(id, body, ...args);
          }) as typeof client.reportState;
          const runner = runnerFor({
            // Deliberately no optional joinOwnedOperations: the lease owns this promise.
            suspendCodexEpoch: async () => ({
              reconcileA: async () => ({ kind: "ready" }),
              persistRollout: async () => ({ kind: "usable", sessionId: "saved-A" }),
              reopenA: async () => { assert.fail("A reopened"); },
              teardown: async () => true,
              publishB: async () => { if (phase === "publish") await waitRaw(); },
              restartAndResume: async () => {
                if (phase === "publish") assert.fail("resume after interruption");
                if (phase === "resume") await waitRaw();
                return { kind: phase === "fresh" ? "confirmed_resume_refusal" : "resumed" };
              },
              startFreshLineage: async () => { await waitRaw(); },
              finishB: async () => {
                if (phase !== "finish") assert.fail("finish after interruption");
                await waitRaw();
              },
            }),
            async run(ctx) {
              worktree = ctx.worktreePath;
              fs.writeFileSync(path.join(worktree, "retained.txt"), "A work");
              pending = true;
              const internal = runner as unknown as {
                runGatedSink(flight: { steering: SteeringChannel; sinkGate: typeof sink }, fn: () => Promise<unknown>, bound?: unknown): Promise<unknown>;
              };
              const original = internal.runGatedSink.bind(runner);
              internal.runGatedSink = async (flight, fn, bound) => {
                steering = flight.steering;
                sink = flight.sinkGate;
                try { return await original(flight, fn, bound); }
                finally { gateDone = true; }
              };
              interrupt = () => {
                if (stop === "cancel") (steering as unknown as { commitCancel(): void }).commitCancel();
                else if (stop === "fence") client.registrationNonce = () => "successor-nonce";
                deadlineFire();
              };
              try { await ctx.attemptCodexCredentialSwitch!({ site: "implement_turn", laneSettled: true }); }
              finally { hookDone = true; internal.runGatedSink = original; }
              assert.fail("A continued");
            },
          });
          (runner as unknown as { credentialSwitchLedger: unknown }).credentialSwitchLedger = {
            stateOf: async () => undefined,
            markSending: async () => ({ wasAmbiguous: false }),
            record: async () => "settled",
          };
          const running = runner.execute(claim).finally(() => { runDone = true; });
          await started;
          interrupt();
          // Event-loop barrier lets the abort race and finalizers run without a timed sleep.
          await new Promise<void>(resolve => setImmediate(resolve));
          const beforeRelease = { hookDone, gateDone, runDone, held: sink.isHeld() };
          release();
          await running;
          assert.deepEqual(beforeRelease, { hookDone: false, gateDone: false, runDone: false, held: true });
          assert.equal(rawDone, true);
          assert.deepEqual(operation, { generation: 1, op_id: "current-op", target_secret_id: "target" });
          assert.equal(fs.readFileSync(path.join(worktree, "retained.txt"), "utf8"), "A work");
        });
      }

    }
  }


  for (const outcome of ["policy_refusal", "baseline_missing", "failed_clear", "ready", "missing_negotiation", "persist_failed", "persist_threw", "released_after_clear", "unproven", "busy_after", "vault_after", "route_after", "refused_after", "lost_after", "fenced_after", "superseded_after", "applied_after", "key_to_sub_after", "applied_key_after", "init_after", "publish_after", "fresh_after", "invalid_after", "replay_after", "ledger_after", "ack_after", "budget_after", "newop_after", "successor_refusal_after", "successor_fence_after", "successor_route_after", "successor_ledger_after", "successor_ledger_follow_after", "successor_ledger_unreadable_after", "successor_noop_after", "successor_teardown_after", "successor_cycle_after", "no_op_after"] as const) {
    it(`M2 firsthalf Codex Runner ${outcome}`, async (t) => {
      const claim = claimFor(2630, { secrets: outcome === "key_to_sub_after"
        ? { ...CODEX_SECRETS, codex: { auth_mode: "api_key", access_token: "A-key", capability: "A-capability" } }
        : CODEX_SECRETS, claim_generation: 1 });
      let bDispatch: Parameters<Executor["run"]>[0]["prepareCodexRootDispatch"];
      let pending = false;
      let pendingId = "current-op";
      const ledgerFollow = outcome === "successor_ledger_follow_after" || outcome === "successor_ledger_unreadable_after";
      const successor = outcome === "newop_after" || outcome.startsWith("successor_");
      let pendingTarget = "target";
      let failedClears = 0;
      const probed: { opId: string; target: string }[] = [];
      const appliedOps: string[] = [];
      const holdReasons: string[] = [];
      const heldOps: string[] = [];
      let ordinaryPause = 0;
      let heldReports = 0;
      let settledReports = 0;
      const order: string[] = [];
      const post = outcome === "ready" || outcome.endsWith("_after") || outcome === "unproven";
      let uncertainReports = 0;
      let elapsed = 0;
      let cleanupBudget = 0;
      let reopened = 0;
      let suspended = 0;
      let preflights = 0;
      let apply = 0;
      let teardown = 0;
      let continued = false;
      let runEntries = 0;
      let probeBudget: number | undefined;
      let drainBound = false;
      const feature = client.hasFeature.bind(client);
      client.hasFeature = name => name === "credential_switch_hold" || feature(name);
      client.registrationNonce = () => "original-nonce";
      client.supportsInPlaceCodexCredentialSwitch = () => outcome !== "missing_negotiation" && !(outcome === "successor_route_after" && pendingId === "successor-op");
      client.supportsCodexSwitchStopped = () => client.supportsInPlaceCodexCredentialSwitch();
      client.getRunOwnership = async () => ({ status: "running", claim_generation: 1,
        claim_released: outcome === "released_after_clear" && !pending, hold_reason: null });
      client.getInputs = async () => ({ inputs: [], ...(pending ? { credentialSwitch: {
        generation: 1, op_id: pendingId, target_secret_id: pendingTarget,
      } } : {}) });
      client.settleCodexSwitchStopped = async () => { ordinaryPause++; return { kind: "unconfirmed" }; };
      client.preflightCodexCredentialSwitch = async (_id, request) => {
        probed.push({ opId: request.opId, target: request.targetSecretId });
        assert.equal(request.opId, pendingId);
        assert.equal(request.targetSecretId, pendingTarget);
        preflights++; order.push("preflight");
        if (outcome === "budget_after") elapsed = 9000;
        const coords = { claimGeneration: 1, registerNonce: "original-nonce", opId: pendingId };
        if (pendingId === "successor-op") {
          if (outcome === "successor_refusal_after") return { kind: "not_applied", reason: "policy_refusal", ...coords };
          if (outcome === "successor_fence_after") return { kind: "fenced", ...coords };
          if (outcome === "successor_noop_after") { pending = false; return { kind: "superseded", ...coords }; }
          if (outcome === "successor_cycle_after") { pendingId = "current-op"; pendingTarget = "target"; return { kind: "superseded", ...coords }; }
        }
        return post ? { kind: "ready", expectedEpoch: 11, ...coords } :
          { kind: "not_applied", reason: outcome === "baseline_missing" ? outcome : "policy_refusal", ...coords };
      };
      client.applyCredentialSwitch = async (_id, req, signal) => {
        apply++; order.push("apply"); appliedOps.push(req.opId);
        assert.equal(req.opId, ledgerFollow && apply > 1 ? "latest-op" : successor && (apply > 1 || outcome === "successor_teardown_after") ? "successor-op" : "current-op");
        assert.equal(req.registerNonce, "original-nonce");
        assert.equal(req.claimGeneration, 1);
        assert.ok(signal && !signal.aborted);
        if (outcome === "lost_after") return { kind: "uncertain", detail: "response_lost" };
        if (outcome === "route_after") return { kind: "route_absent" };
        if (outcome === "fenced_after") return { kind: "fenced" };
        if (successor && apply === 1 && outcome !== "successor_teardown_after") { pendingId = "successor-op"; pendingTarget = "new-target"; return { kind: "superseded" }; }
        if (outcome === "no_op_after") { pending = false; return { kind: "superseded" }; }
        if (outcome === "superseded_after") return { kind: "superseded" };
        if (outcome === "refused_after") return { kind: "refused_committed", reason: "committed" };
        if (["newop_after", "successor_teardown_after", "applied_after", "key_to_sub_after", "applied_key_after", "init_after", "publish_after", "fresh_after", "invalid_after", "replay_after"].includes(outcome)) {
          pending = false;
          return { kind: "codex_applied", codex: outcome === "applied_key_after"
            ? { auth_mode: "api_key", access_token: "B-access", capability: "B-capability" }
            : { ...CODEX_SECRETS.codex!, access_token: "B-access", capability: "B-capability" }, ordinal: outcome === "invalid_after" ? 2 : 1,
            capabilityEpoch: 42, messageSeq: 7, replay: outcome === "replay_after" };
        }
        return { kind: "not_applied", reason: outcome === "busy_after" ? "busy" : "vault_locked" };
      };
      const report = client.reportState.bind(client);
      client.reportState = (async (id, body, ...args) => {
        if (body.status === "credential_switch_uncertain") {
          uncertainReports++; order.push("hold"); holdReasons.push(body.failure_reason!);
          heldOps.push(body.op_id ?? "<missing>");
          if (outcome === "ack_after" && uncertainReports === 1) throw new Error("lost reply");
          if (outcome === "ack_after" && uncertainReports === 2) return { applied: true, status: "paused" };
          return { applied: true, credentialSwitchUncertain: true };
        }
        if (body.status === "credential_switch_held") {
          heldReports++; order.push("held");
          assert.equal(body.op_id, "current-op");
          return { applied: true, credentialSwitchHeld: true };
        }
        if (body.status === "credential_switch_hold_settled") {
          settledReports++;
          assert.ok(order.includes("join"));
          return { applied: true };
        }
        if (body.status === "credential_switch_failed") {
          failedClears++;
          assert.equal(body.op_id, "current-op");
          if (outcome === "failed_clear") return { applied: false };
          pending = false;
          return { applied: true };
        }
        return report(id, body, ...args);
      }) as typeof client.reportState;
      let worktree = "";
      const executor: Executor = {
        joinOwnedOperations: async () => { order.push("join"); },
        suspendCodexEpoch: async () => {
          suspended++;
          return {
            reconcileA: async (_bounds, remainingMs) => { probeBudget = remainingMs; return { kind: "ready" }; },
            persistRollout: async () => {
              order.push("persist");
              if (outcome === "persist_threw") throw new Error("save failed");
              return outcome === "persist_failed" ? { kind: "failed" } : { kind: "usable", sessionId: "saved-A" };
            },
            reopenA: async () => { reopened++; },
            teardown: async bounds => {
              teardown++; order.push("reap");
              if (outcome === "successor_teardown_after") { pendingId = "successor-op"; pendingTarget = "new-target"; }
              if (outcome === "budget_after") {
                cleanupBudget = bounds.deadline - performance.now();
                elapsed = 11000; // Past the original admission deadline, without sleeping.
                bounds.guard();
                assert.equal(bounds.signal.aborted, false);
              }
              return outcome !== "unproven";
            },
            publishB: async bundle => {
              order.push("publish"); assert.equal(bundle.ordinal, 1); assert.equal(bundle.capabilityEpoch, 42);
              if (outcome === "publish_after") throw new Error("publication failed");
            },
            restartAndResume: async session => {
              order.push("login-resume"); assert.equal(session, "saved-A");
              if (outcome === "init_after") throw new Error("login transport failed");
              return { kind: outcome === "fresh_after" ? "confirmed_resume_refusal" : "resumed" };
            },
            startFreshLineage: async () => { order.push("fresh"); assert.equal(outcome, "fresh_after"); },
            finishB: async () => { order.push("finish"); },
          };
        },
        async run(ctx) {
          runEntries++;
          worktree = ctx.worktreePath;
          fs.writeFileSync(path.join(worktree, "retained.txt"), "A work");
          pending = true;
          // Simulate six seconds spent draining under the gate, without waiting in real time.
          elapsed = 0;
          const clock = outcome === "policy_refusal" || outcome === "budget_after" ? t.mock.method(performance, "now", () => elapsed) : undefined;
          const internal = runner as unknown as {
            awaitTickSurvivorsGone(flight: unknown, deadlineAt?: number): Promise<"gone">;
          };
          const drain = internal.awaitTickSurvivorsGone.bind(runner);
          if (outcome === "policy_refusal") internal.awaitTickSurvivorsGone = async (_flight, deadlineAt) => {
            drainBound = deadlineAt !== undefined && deadlineAt <= Date.now() + 10_000;
            elapsed = 6000;
            return "gone";
          };
          const oldDispatch = ctx.prepareCodexRootDispatch;
          const oldEmit = ctx.emit;
          let result;
          try {
            result = await ctx.attemptCodexCredentialSwitch!({ site: "implement_turn", laneSettled: true });
          } finally {
            internal.awaitTickSurvivorsGone = drain;
            clock?.mock.restore();
          }
          assert.equal(result, outcome === "fresh_after" ? "fresh_B" :
            outcome === "newop_after" || outcome === "successor_teardown_after" || outcome === "applied_after" || outcome === "key_to_sub_after" || outcome === "applied_key_after" ? "resumed_B" : "continued_A");
          if (result !== "continued_A") {
            if (outcome !== "key_to_sub_after") {
              assert.ok(oldDispatch);
              await assert.rejects(() => oldDispatch(new AbortController().signal, () => {}), /closed/);
            }
            bDispatch = ctx.prepareCodexRootDispatch;
            assert.equal(ctx.prepareCodexRootDispatch === undefined, outcome === "applied_key_after");
            assert.throws(() => oldEmit({ kind: "status", agent: "worker", payload: { text: "old epoch frame" } }), /epoch callback retired/);
            ctx.emit({ kind: "status", agent: "worker", payload: { text: "new epoch B-access B-capability" } });
          }
          continued = true;
          throw new CredentialSwitchSignal();
        },
      };
      const runner = runnerFor(executor);
      (runner as unknown as { waitRecoveryRetry: () => Promise<void> }).waitRecoveryRetry = async () => {};
      (runner as unknown as { credentialSwitchLedger: unknown }).credentialSwitchLedger = {
        stateOf: async (_runId: string, opId: string) => {
          if (ledgerFollow && opId === "successor-op") {
            pendingId = "latest-op"; pendingTarget = "latest-target";
            if (outcome === "successor_ledger_unreadable_after") throw new Error("ledger unreadable");
            return "ambiguous";
          }
          return outcome === "successor_ledger_after" && opId === "successor-op" ? "ambiguous" : undefined;
        },
        markSending: async () => { order.push("ledger"); if (outcome === "ledger_after") throw new Error("ledger unavailable"); return { wasAmbiguous: false }; },
        record: async () => "settled",
      };
      await runner.execute(claim);
      if (outcome === "key_to_sub_after") {
        assert.ok(bDispatch);
        await assert.rejects(() => bDispatch!(new AbortController().signal, () => {}), /closed/);
      }
      const refusal = outcome === "policy_refusal" || outcome === "baseline_missing" || outcome === "persist_failed" || outcome === "persist_threw";
      const notices = api.messages(claim.run_id).filter(m => m.kind === "status" && (m.payload as { event?: string }).event === "credential_switch_failed");
      assert.equal(notices.length, refusal ? 1 : 0);
      if (refusal) {
        assert.equal((notices[0]!.payload as { reason: string }).reason,
          outcome === "policy_refusal" || outcome === "baseline_missing" ? outcome : "rollout_persist_failed");
      }
      if (outcome === "policy_refusal") {
        assert.equal(drainBound, true);
        assert.equal(probeBudget, 4000, "drain and epoch probes share the original budget");
      }
      const success = outcome === "newop_after" || outcome === "successor_teardown_after" || outcome === "applied_after" || outcome === "key_to_sub_after" || outcome === "applied_key_after" || outcome === "fresh_after";
      assert.equal(continued, refusal || success);
      if (success) {
        assert.deepEqual(order.slice(order.indexOf("publish")), outcome === "fresh_after"
          ? ["publish", "login-resume", "fresh", "finish"] : ["publish", "login-resume", "finish"]);
        const frames = api.messages(claim.run_id);
        const old = frames.find(m => (m.payload as { text?: string }).text === "old epoch frame");
        const next = frames.find(m => (m.payload as { text?: string }).text?.startsWith("new epoch"));
        assert.equal(old, undefined, "retired A callback cannot publish"); assert.ok(next);
        assert.equal(next.credential_epoch_ordinal, 1);
        assert.ok(!JSON.stringify(next).includes("B-access"));
        assert.ok(!JSON.stringify(next).includes("B-capability"));
      }
      if (outcome === "invalid_after" || outcome === "replay_after") assert.ok(!order.includes("publish"));
      if (outcome === "init_after") assert.ok(order.includes("login-resume"));
      if (outcome === "publish_after") assert.ok(!order.includes("login-resume"));
      assert.equal(runEntries, 1, "successors stay inside the same stopped A boundary");
      assert.equal(reopened, refusal ? 1 : 0);
      assert.equal(suspended, outcome === "missing_negotiation" ? 0 : 1);
      if (successor) {
        assert.equal(failedClears, 0, "never clear a successor after teardown");
        assert.deepEqual(appliedOps, outcome === "newop_after" ? ["current-op", "successor-op"] :
          outcome === "successor_teardown_after" ? ["successor-op"] : ["current-op"]);
        assert.deepEqual(probed, (ledgerFollow || ["successor_route_after", "successor_ledger_after"].includes(outcome))
          ? [{ opId: "current-op", target: "target" }]
          : [{ opId: "current-op", target: "target" }, { opId: "successor-op", target: "new-target" }]);
        assert.ok(holdReasons.every(reason => reason === (ledgerFollow || outcome === "successor_ledger_after" ? "apply_uncertain" : "not_applied_after_teardown")));
        if (!success) assert.ok(!order.includes("publish"), "refused successor cannot publish");
        if (ledgerFollow) assert.ok(!order.includes("login-resume"), "uncertain ledger cannot authenticate B");
      }
      assert.equal(preflights, successor && !(ledgerFollow || ["successor_route_after", "successor_ledger_after"].includes(outcome)) ? 2 : ["missing_negotiation", "persist_failed", "persist_threw"].includes(outcome) ? 0 : 1);
      assert.equal(apply, outcome === "newop_after" ? 2 : post && !["unproven", "ledger_after"].includes(outcome) ? 1 : 0);
      assert.equal(teardown, post ? 1 : 0);
      assert.equal(uncertainReports, outcome === "ack_after" ? 3 : post && !["unproven", "fenced_after", "successor_fence_after", "successor_noop_after", "newop_after", "successor_teardown_after", "no_op_after", "applied_after", "key_to_sub_after", "applied_key_after", "fresh_after"].includes(outcome) ? 1 : 0);
      assert.deepEqual(heldOps, Array(uncertainReports).fill(ledgerFollow ? "latest-op" : successor && outcome !== "successor_cycle_after" ? "successor-op" : "current-op"));
      assert.equal(ordinaryPause, outcome === "no_op_after" || outcome === "successor_noop_after" ? 3 : 0);
      assert.equal(heldReports, outcome === "unproven" ? 1 : 0);
      assert.equal(settledReports, outcome === "unproven" ? 1 : 0);
      if (outcome === "budget_after") assert.equal(cleanupBudget, 30_000);
      if (apply && outcome !== "successor_teardown_after") assert.deepEqual(order.slice(0, 5), ["persist", "preflight", "reap", "ledger", "apply"]);
      assert.equal(fs.readFileSync(path.join(worktree, "retained.txt"), "utf8"), "A work");
      assert.ok(!api.states.some(s => s.runId === claim.run_id && s.body.status === "credential_switch"));
    });
  }

  for (const entry of ["escaped", "claude_hook"] as const) {
    it(`M2 Codex ${entry} retains work without legacy release`, async () => {
      const claim = claimFor(2625, { secrets: CODEX_SECRETS });
      let worktree = "";
      let entered = false;
      const runner = runnerFor({
        async run(ctx) {
          entered = true;
          worktree = ctx.worktreePath;
          fs.writeFileSync(path.join(worktree, "retained.txt"), "pending switch work");
          if (entry === "claude_hook") await ctx.attemptCredentialSwitch!({ site: "implement_turn", laneSettled: true });
          throw new CredentialSwitchSignal();
        },
      });
      let legacyCalls = 0;
      const legacy = runner as unknown as { enterCredentialSwitch(...args: unknown[]): Promise<never> };
      legacy.enterCredentialSwitch = async () => { legacyCalls++; throw new Error("legacy release entered"); };
      await runner.execute(claim);
      assert.equal(legacyCalls, 0);
      assert.equal(entered, true);
      assert.equal(fs.readFileSync(path.join(worktree, "retained.txt"), "utf8"), "pending switch work");
      const statuses = api.states.filter(s => s.runId === claim.run_id).map(s => s.body.status);
      assert.ok(!statuses.includes("credential_switch"), JSON.stringify(statuses));
      assert.ok(!statuses.includes("credential_switch_failed"), JSON.stringify(statuses));
      assert.ok(!statuses.includes("failed"), JSON.stringify(statuses));
      assert.ok(!statuses.includes("completed"), JSON.stringify(statuses));
    });
  }

  it("M2 Codex claude_hook honours committed withheld cancel without legacy release", async () => {
    const claim = claimFor(2626, { secrets: CODEX_SECRETS, claim_generation: 1 });
    api.setInputClaimGeneration(claim.run_id, 1);
    api.deriveCancellationRequested = true;
    const channels: SteeringChannel[] = [];
    const begin = SteeringChannel.prototype.beginCredentialSwitchDefer;
    SteeringChannel.prototype.beginCredentialSwitchDefer = function () {
      channels.push(this);
      return begin.call(this);
    };
    let hookEntered = false;
    let legacyCalls = 0;
    const runner = runnerFor({
      async run(ctx) {
        await ctx.deferCredentialSwitch!(async () => {
          api.requestCredentialSwitch(claim.run_id, 1);
          api.setInputs(claim.run_id, [{ id: 1, kind: "cancel", body: null }]);
          // Cancellation sets its committed marker before aborting terminal lifecycle.
          const terminal = ctx.terminalLifecycleSignal!;
          await new Promise<void>(resolve => {
            if (terminal.aborted) resolve();
            else terminal.addEventListener("abort", () => resolve(), { once: true });
          });
          assert.ok(channels.some(ch => ch.cancelCommittedUnderWithheldSwitch()), "cancel committed under the deferred switch");
          hookEntered = true;
          await ctx.attemptCredentialSwitch!({ site: "implement_turn", laneSettled: true });
        });
        return { branch: ctx.branch };
      },
    });
    const legacy = runner as unknown as { enterCredentialSwitch(...args: unknown[]): Promise<never> };
    legacy.enterCredentialSwitch = async () => { legacyCalls++; throw new Error("legacy release entered"); };
    try {
      await runner.execute(claim);
    } finally {
      SteeringChannel.prototype.beginCredentialSwitchDefer = begin;
    }
    assert.equal(hookEntered, true);
    assert.equal(legacyCalls, 0);
    const states = api.states.filter(s => s.runId === claim.run_id).map(s => s.body);
    assert.deepEqual(states.filter(s => s.status === "failed").map(s => s.failure_reason), ["run cancelled"]);
    assert.ok(!states.some(s => s.status === "credential_switch" || s.status === "credential_switch_failed" || s.status === "completed"));
  });

  /** A fake production factory: records the build params and each request, answers like luna. */
  function lunaFactory(rec: { builds: unknown[]; requests: AdviceRequest[] }, text = "Running the api gate for the docs milestone"): CodexAdviceHarnessFactory {
    return (async (params: unknown) => {
      rec.builds.push(params);
      return {
        kind: "codex",
        async run(request: AdviceRequest, _policy: AdviceResultPolicy): Promise<AdviceResult> {
          rec.requests.push(request);
          request.usageObserver?.({ "gpt-6-luna": { inputTokens: 90, outputTokens: 12, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "metered", costUSD: 0.000015 } });
          return { text, end: { kind: "terminal", terminal: { outcome: "success" } } } as unknown as AdviceResult;
        },
      };
    }) as unknown as CodexAdviceHarnessFactory;
  }

  for (const providerMode of [false, true]) it(`M2 Runner composes the Codex Now factory through its executor provider=${providerMode}`, async () => {
    const secrets = providerMode ? { forge_pat: "fixture-forge-pat-000000", provider: {
      provider_id: "now-provider", revision: 2, engine: "codex" as const, protocol: "openai_responses" as const,
      base_url: "https://gateway.example/v1", auth_style: "bearer" as const,
      credential: "now-provider-fixture-key", model_map: { default: { model: "gateway-model" } },
    } } : CODEX_SECRETS;
    const claim = claimFor(2619, { secrets });
    api.nowSummary.set(claim.run_id, true);
    const rec = { builds: [] as unknown[], requests: [] as AdviceRequest[] };
    const raw = lunaFactory(rec);
    let bindings = 0, calls = 0;
    const selection = selectCodexExecutionAuthority(secrets);
    assert.equal(selection.kind, "codex");
    if (selection.kind !== "codex") throw new Error("Codex fixture binding missing");
    const executor = new CodexExecutor(nullLogger(), fx.dataDir, { binding: selection.binding, client, provider: CODEX_PRODUCTION_PROVIDER });
    const compose = executor.bindCodexSummaryFactory.bind(executor);
    executor.bindCodexSummaryFactory = build => {
      assert.equal(build, raw);
      bindings++;
      const owned = compose(async params => {
        assert.ok(summaryOwnership.has(params), "actual owner is attached before raw construction");
        calls++;
        return build(params);
      });
      assert.ok(ownedSummaryFactories.has(owned));
      return owned;
    };
    const working = workingExecutor(() => notesOf(claim.run_id).length > 0);
    executor.run = ctx => {
      // Model the run-entry publication while retaining the production wrapper.
      (executor as unknown as { currentCodexAuthority: ReturnType<typeof createCodexEpochAuthority> })
        .currentCodexAuthority = createCodexEpochAuthority(selection.binding);
      return working.run(ctx);
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: (() => { assert.fail("Claude route"); }) as SdkQueryFn, codexAdviceHarnessFactory: raw }).execute(claim);
    } finally { fs.rmSync(homeRoot, { recursive: true, force: true }); }
    assert.equal(bindings, 1);
    assert.equal(calls, 1);
    const built = rec.builds[0] as { binding: unknown; refresh?: string };
    assert.deepEqual(built.binding, selection.binding, "Now retains the executor-bound authority");
    assert.equal(built.refresh, "deny");
    assert.equal(rec.requests.length, 1);
    assert.equal(notesOf(claim.run_id).length, 1);
  });

  for (const kind of ["issue", "self_improve", "prompt"] as const) {
  it(`M1 Now runner Codex ${kind} eligibility`, async () => {
    const claim = claimFor(2606, { kind, secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
    const rec = { builds: [] as unknown[], requests: [] as AdviceRequest[] };
    const sdkCalls: { options: unknown; prompt: unknown }[] = [];
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery(sdkCalls), codexAdviceHarnessFactory: lunaFactory(rec) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(sdkCalls.length, 0, "a Codex run never reaches the Claude SDK");
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1);
    const p = notes[0]!.payload as Record<string, unknown>;
    assert.equal(p.text, "Running the api gate for the docs milestone");
    assert.deepEqual((p.model_usage as Record<string, unknown>)["gpt-6-luna"], { inputTokens: 90, outputTokens: 12, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "metered", costUSD: 0.000015 });
    assert.equal(rec.requests.length, 1);
    assert.equal(rec.requests[0]!.model, "gpt-6-luna");
    assert.equal(rec.requests[0]!.effort, "low");
    assert.equal(rec.requests[0]!.timeoutMs, 30_000);
    assert.equal(rec.requests[0]!.label, "now");
    assert.match(rec.requests[0]!.prompt, /Run the api gate/);
    const built = rec.builds[0] as { runId: string; refresh?: string; binding: { authMode: string; capability: string } };
    assert.equal(built.runId, claim.run_id);
    assert.equal(built.refresh, "deny");
    assert.equal(built.binding.authMode, "subscription");
    assert.equal(built.binding.capability, CODEX_SECRETS.codex.capability, "original claim binding");
  });

  }

  it("M1 Now runner Codex with the production claim shape (empty anthropic_oauth_token) still posts the Now note", async () => {
    // A real Codex claim carries "" for the Anthropic token (api claim_assembly), so no credential holder exists.
    const claim = claimFor(2607, { kind: "issue", secrets: { ...CODEX_SECRETS, anthropic_oauth_token: "" } });
    api.nowSummary.set(claim.run_id, true);
    const rec = { builds: [] as unknown[], requests: [] as AdviceRequest[] };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery([]), codexAdviceHarnessFactory: lunaFactory(rec) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1, "the Codex Now note posts without an Anthropic credential holder");
    assert.equal((notes[0]!.payload as Record<string, unknown>).text, "Running the api gate for the docs milestone");
    assert.equal(rec.requests.length, 1);
  });

  it("M1 Now runner judge is excluded by the shared eligibility predicate", async () => {
    const { isRunSummaryKind } = await import("../src/run-kind.js");
    assert.equal(isRunSummaryKind("judge"), false);
  });

  for (const kind of ["issue", "self_improve", "prompt"] as const) {
    it(`M1 Now runner ${kind} without milestones emits no note`, async () => {
      const claim = claimFor(2610, { kind, secrets: CODEX_SECRETS });
      api.nowSummary.set(claim.run_id, true);
      const rec = { builds: [] as unknown[], requests: [] as AdviceRequest[] };
      const calls: { options: unknown; prompt: unknown }[] = [];
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
      const executor: Executor = { run: async ctx => {
        // No reportProgress: tool activity cannot invent a milestone.
        for (let frame = 0; frame < 8; frame++) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          await new Promise(resolve => setTimeout(resolve, 10));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      } };
      try {
        await runnerFor(executor, { homeRoot, queryFn: summaryQuery(calls), codexAdviceHarnessFactory: lunaFactory(rec) }).execute(claim);
      } finally {
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
      assert.equal(rec.requests.length, 0);
      assert.equal(calls.length, 0);
      assert.equal(notesOf(claim.run_id).length, 0);
    });
  }

  it("a Codex account that refuses the model leaves the run unaffected and shows no summary", async () => {
    const claim = claimFor(2608, { secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
    const statuses: string[] = [];
    api.onState(claim.run_id, (body) => void statuses.push(body.status));
    let builds = 0;
    const refusing = (async () => {
      builds++;
      throw new Error("model gpt-6-luna is not available to this account");
    }) as unknown as CodexAdviceHarnessFactory;
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery([]), codexAdviceHarnessFactory: refusing }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(builds, 1, "tried once; the failed call holds the 5 minute window");
    assert.equal(notesOf(claim.run_id).length, 0);
    assert.ok(statuses.includes("completed") && !statuses.includes("failed"), `the run is unaffected: ${statuses.join(",")}`);
  });

  it("makes no call for a Codex-harness claim when no advice factory is wired", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2609, { secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(calls.length, 0);
    assert.equal(notesOf(claim.run_id).length, 0);
  });

  it("makes no call while the executor is parked on askUser, and calls once the answer ends the hold", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2607);
    api.nowSummary.set(claim.run_id, true);
    let questionId = "";
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) questionId = body.open_question_id;
    });
    let callsDuringHold = -1;
    let heldFrames = 0;
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
        const asked = ctx.askUser!([{ question: "which docs?", header: "Docs" }]);
        const parkBy = Date.now() + 3000;
        while (questionId === "" && Date.now() < parkBy) await new Promise((r) => setTimeout(r, 10));
        // Frames arrive while the run is parked; a controller that is not held would call at once.
        const holdUntil = Date.now() + 300;
        while (Date.now() < holdUntil) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          heldFrames++;
          await new Promise((r) => setTimeout(r, 25));
        }
        callsDuringHold = calls.length;
        api.setInputs(claim.run_id, [{ id: 1, kind: "answer", body: JSON.stringify({ question_id: questionId, answers: ["the api docs"] }) }]);
        await asked;
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(questionId !== "", "the run really parked on the question");
    assert.ok(heldFrames > 3, "frames arrived during the hold");
    assert.equal(callsDuringHold, 0, "no model call while the run is parked on askUser");
    assert.ok(calls.length >= 1, "the call is made once the hold ends (positive control)");
  });

  it("redacts the run's claim secrets in the model prompt: a secret straddling the 200 rune cut leaves no prefix", async () => {
    const texts: string[] = [];
    // Assembled at runtime so no secret-shaped literal sits in source.
    const secret = ["fixture", "forge", "pat", "Zq9Xw8Vu7Ts6"].join("-");
    const claim = claimFor(2608, { secrets: { forge_pat: secret, anthropic_oauth_token: "oauth-fixture-token" } });
    api.nowSummary.set(claim.run_id, true);
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "y".repeat(192) + secret } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: promptCapturingQuery(texts) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(texts.length >= 1, "a call was made");
    const prompt = texts[0]!;
    assert.ok(prompt.includes("yyyyyyyy"), "the frame reached the prompt (positive control)");
    assert.ok(!prompt.includes(secret.slice(0, 6)), "no prefix of the claim secret reaches the model");
  });

  it("the turn-boundary claim milestone list is only a fallback: it never replaces the list reportProgress gave", async () => {
    const texts: string[] = [];
    const claim = claimFor(2609, { milestones: [{ id: "m1", title: "CLAIM_LIST_TITLE" }] });
    api.nowSummary.set(claim.run_id, true);
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "LIVE_LIST_TITLE" }] as never);
        await ctx.reportIteration!(1, { completed: [], in_progress: ["m1"] });
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: promptCapturingQuery(texts) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(texts.length >= 1);
    const prompt = texts[0]!;
    assert.ok(prompt.includes("LIVE_LIST_TITLE"));
    assert.ok(!prompt.includes("CLAIM_LIST_TITLE"));
  });
});
