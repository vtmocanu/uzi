import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query, type Options as SdkOptions, type SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, TransientRecoveryError, type SdkQueryFn, type SdkExecutorOptions } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { defaultQueryFn } from "../src/sdk-messages.js";
import { nullLogger } from "./helpers.js";

// issue #1656: a FOREIGN SIGTERM/SIGKILL of the SDK CLI child (no result frame, no uzi trip)
// resumes the same session in-process, at most twice per turn, instead of failing the run.
// A uzi-initiated kill always sets state.tripReason first and keeps its trip outcome; crash
// signals and other exit codes still fail fast; with no known session id nothing resumes.
//
// Same harness shape as sdk-executor.test.ts: `queryFn` is faked per turn, so every path runs
// with dummy credentials. The SDK-shape pin at the bottom drives the INSTALLED SDK's real
// process-exit path with a child this test spawns and that ends ITSELF (exit 143 or a signal
// to its own pid); nothing here signals a process it did not spawn.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

// Unique per process and per call, never created (see sdk-executor.test.ts: the executor
// materializes a sibling skills-plugin dir, and node --test runs files concurrently).
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-signal-death-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}
function resultApiError(status: number, sessionId = "sess-1"): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: true,
    api_error_status: status,
    result: `API Error: ${status}`,
    terminal_reason: "api_error",
    session_id: sessionId,
  } as unknown as SDKMessage;
}

// The SDK's tagged exit errors, as getProcessExitError builds them (own properties on a
// plain Error). The real shape is pinned against the installed SDK at the bottom.
function sdkExit(code: number): Error {
  return Object.assign(new Error(`Claude Code process exited with code ${code}`), {
    errorClass: "process_exited_nonzero",
    exitCode: code,
  });
}
function sdkSignal(signal: string): Error {
  return Object.assign(new Error(`Claude Code process terminated by signal ${signal}`), {
    errorClass: "process_killed_by_signal",
    signal,
  });
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

interface Turn {
  options: SdkOptions;
  startedAt: number;
}

function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    turns.push({ options: params.options, startedAt: Date.now() });
    return (async function* () {
      for await (const _p of params.prompt) { /* drain the SDK prompt */ }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

/** A turn that yields `frames` (optionally after `delayMs`), then its CLI dies with `err`. */
function dies(err: Error, frames: SDKMessage[] = [], delayMs = 0): Script {
  return () =>
    (async function* () {
      for (const f of frames) yield f;
      if (delayMs > 0) await new Promise((r) => setTimeout(r, delayMs));
      throw err;
    })();
}

/** Waits until the turn is aborted (a trip), then surfaces `err` as the CLI's death, the way
 *  a group-killed CLI's exit can reach the iterator before the abort is observed. */
function hangThenDie(err: Error): Script {
  return (signal) =>
    (async function* () {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener("abort", () => { clearInterval(keepAlive); resolve(); }, { once: true });
      });
      yield* [];
      throw err;
    })();
}

let homeDir: string;
let saved: Record<string, string | undefined>;

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emits: EmittedMessage[]; gated: string[] } {
  const emits: EmittedMessage[] = [];
  const gated: string[] = [];
  const verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    gatePlan: async (planMd) => {
      gated.push(planMd);
      return verdict;
    },
    pullFollowUp: () => undefined,
    reportIteration: () => {},
    ...overrides,
  };
  return { ctx, emits, gated };
}

const opts = (queryFn: SdkQueryFn, extra: Partial<SdkExecutorOptions> = {}): SdkExecutorOptions => ({
  queryFn,
  emptyTurnBackoffBaseMs: 0,
  emptyTurnMaxRetries: 2,
  // No test here may group-kill anything: the fakes spawn nothing, and the real-SDK pin's
  // children end themselves.
  kill: () => true,
  ...extra,
});

async function rejection(p: Promise<unknown>): Promise<Error> {
  return p.then(
    () => { throw new Error("expected the run to reject"); },
    (e: unknown) => {
      assert.ok(e instanceof Error, "the run rejects with an Error");
      return e;
    },
  );
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sighome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});

afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor foreign CLI signal death (issue #1656)", () => {
  for (const [label, err] of [
    ["process_killed_by_signal SIGTERM", () => sdkSignal("SIGTERM")],
    ["process_killed_by_signal SIGKILL", () => sdkSignal("SIGKILL")],
    ["process_exited_nonzero 143", () => sdkExit(143)],
    ["process_exited_nonzero 137", () => sdkExit(137)],
  ] as const) {
    it(`first turn: a ${label} death after the session frame resumes the SAME session`, async () => {
      const { queryFn, turns } = fakeTurns([
        dies(err(), [assistantText("working", "sess-1")]),
        [submitPlan("# Plan", "sess-1"), resultSuccess("sess-1")],
        [signalDone("sess-1"), resultSuccess("sess-1")],
      ]);
      const probe = makeCtx();
      const result = await new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(probe.ctx);
      assert.equal(result.branch, "agent/issue-5");
      assert.deepEqual(probe.gated, ["# Plan"]);
      assert.deepEqual(
        turns.map((t) => t.options.resume),
        [undefined, "sess-1", "sess-1"],
        "the retry resumes the observed session; no fresh session is started",
      );
    });
  }

  for (const [label, err] of [
    ["SIGTERM", () => sdkSignal("SIGTERM")],
    ["exit 143", () => sdkExit(143)],
  ] as const) {
    it(`later turn: a ${label} death with NO frames resumes the caller's resumeId`, async () => {
      const { queryFn, turns } = fakeTurns([
        [submitPlan("# Plan", "sess-A"), resultSuccess("sess-A")],
        dies(err()), // implement turn: dies before any frame (the run's session latch already fired)
        [signalDone("sess-A"), resultSuccess("sess-A")],
      ]);
      const result = await new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx);
      assert.equal(result.branch, "agent/issue-5");
      assert.deepEqual(turns.map((t) => t.options.resume), [undefined, "sess-A", "sess-A"]);
    });
  }

  it("emits a truthful status notice for each resume", async () => {
    const { queryFn } = fakeTurns([
      dies(sdkSignal("SIGTERM"), [assistantText("working")]),
      [submitPlan("# Plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(probe.ctx);
    const notices = probe.emits
      .filter((m) => m.kind === "status")
      .map((m) => String(m.payload["text"]))
      .filter((t) => /SIGTERM/.test(t) && /resum/i.test(t));
    assert.equal(notices.length, 1, `one resume notice, got ${JSON.stringify(notices)}`);
  });

  it("after 2 retries the run fails with the SDK's own error, never a recovery park", async () => {
    const { queryFn, turns } = fakeTurns([dies(sdkSignal("SIGTERM"), [assistantText("working")])]);
    const err = await rejection(new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx));
    assert.equal(err.message, "Claude Code process terminated by signal SIGTERM");
    assert.ok(!(err instanceof TransientRecoveryError), "exhaustion is not routed to recovery_wait");
    assert.equal(turns.length, 3, "the first death plus at most two resumes");
    assert.deepEqual(turns.map((t) => t.options.resume), [undefined, "sess-1", "sess-1"]);
  });

  it("the bound is per turn: a later turn gets its own two retries", async () => {
    const { queryFn, turns } = fakeTurns([
      dies(sdkExit(143), [assistantText("working", "sess-A")]),
      dies(sdkExit(143)),
      [submitPlan("# Plan", "sess-A"), resultSuccess("sess-A")],
      dies(sdkExit(143)),
      dies(sdkExit(143)),
      [signalDone("sess-A"), resultSuccess("sess-A")],
    ]);
    const result = await new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx);
    assert.equal(result.branch, "agent/issue-5");
    assert.equal(turns.length, 6);
  });

  it("provider-transient retries in the same turn do not extend the signal-death bound", async () => {
    // death → resume 1 → transient 529 (provider retry) → death → resume 2 → death → exhausted.
    // A counter reset or shared by the provider retry would allow a fifth drive.
    const { queryFn, turns } = fakeTurns([
      dies(sdkSignal("SIGTERM"), [assistantText("working")]),
      [resultApiError(529)],
      dies(sdkSignal("SIGTERM")),
    ]);
    const err = await rejection(new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx));
    assert.equal(err.message, "Claude Code process terminated by signal SIGTERM");
    assert.equal(turns.length, 4, "two signal-death resumes in total, however the provider retries fall");
  });

  it("a resume does not reset the wall budget", async () => {
    // Wall 1.2s. The first drive spends ~0.8s then dies; the resume hangs until the wall
    // trips. With the remaining budget carried over the resume lasts ~0.4s; a reset would
    // give it the full 1.2s.
    const { queryFn, turns } = fakeTurns([
      dies(sdkSignal("SIGTERM"), [assistantText("working")], 800),
      hangThenDie(sdkExit(143)),
    ]);
    const err = await rejection(
      new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx({ config: { run_timeout_seconds: 1.2 } }).ctx),
    );
    const resumedFor = Date.now() - turns[1]!.startedAt;
    assert.match(err.message, /wall-clock timeout/);
    assert.equal(turns.length, 2);
    assert.ok(resumedFor < 900, `the resume ran on the remaining budget, lasted ${resumedFor}ms`);
  });

  it("a trip during a resume wins (idle), and nothing further is driven", async () => {
    const { queryFn, turns } = fakeTurns([
      dies(sdkSignal("SIGTERM"), [assistantText("working")]),
      hangThenDie(sdkExit(143)),
    ]);
    const err = await rejection(
      new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx({ config: { idle_timeout_seconds: 0.1 } }).ctx),
    );
    assert.match(err.message, /idle timeout/);
    assert.equal(turns.length, 2);
  });

  it("a signal death with no known session id fails as today and starts no fresh session", async () => {
    const { queryFn, turns } = fakeTurns([dies(sdkSignal("SIGTERM")), [submitPlan("# Plan"), resultSuccess()]]);
    const err = await rejection(new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx));
    assert.equal(err.message, "Claude Code process terminated by signal SIGTERM");
    assert.equal(turns.length, 1, "no second query: a fresh session is never labelled a resume");
  });

  // ---- negative controls: unchanged behaviour ------------------------------------------

  for (const [label, err] of [
    ["process_killed_by_signal SIGSEGV", () => sdkSignal("SIGSEGV")],
    ["process_killed_by_signal SIGABRT", () => sdkSignal("SIGABRT")],
    ["process_exited_nonzero 1", () => sdkExit(1)],
    ["process_exited_nonzero 139", () => sdkExit(139)],
    ["an untagged Error", () => new Error("Claude Code process exited with code 143")],
  ] as const) {
    it(`control: ${label} still fails the run with no resume`, async () => {
      const thrown = err();
      const { queryFn, turns } = fakeTurns([dies(thrown, [assistantText("working")]), [submitPlan("# Plan"), resultSuccess()]]);
      const got = await rejection(new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx().ctx));
      assert.equal(got.message, thrown.message);
      assert.equal(turns.length, 1);
    });
  }

  for (const [label, ctxOverrides, reason] of [
    ["idle", { config: { idle_timeout_seconds: 0.1 } }, /idle timeout/],
    ["wall", { config: { run_timeout_seconds: 0.1 } }, /wall-clock timeout/],
  ] as const) {
    it(`control: a ${label} trip whose CLI death surfaces as exit 143 keeps its trip outcome`, async () => {
      const { queryFn, turns } = fakeTurns([hangThenDie(sdkExit(143)), [submitPlan("# Plan"), resultSuccess()]]);
      const got = await rejection(
        new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx({ sessionId: "prev", ...ctxOverrides }).ctx),
      );
      assert.match(got.message, reason);
      assert.equal(turns.length, 1, "a uzi trip is never reclassified as a foreign death");
    });
  }

  it("control: a cancel whose CLI death surfaces as exit 143 stays cancelled", async () => {
    const ac = new AbortController();
    const { queryFn, turns } = fakeTurns([hangThenDie(sdkExit(143)), [submitPlan("# Plan"), resultSuccess()]]);
    const t = setTimeout(() => ac.abort(), 50);
    try {
      const got = await rejection(
        new SdkExecutor(nullLogger(), homeDir, opts(queryFn)).run(makeCtx({ sessionId: "prev", signal: ac.signal }).ctx),
      );
      assert.match(got.message, /run cancelled/);
      assert.equal(turns.length, 1);
    } finally {
      clearTimeout(t);
    }
  });
});

// ---- SDK-shape pin: the INSTALLED SDK's real process-exit error ----------------------------
//
// The classifier reads getProcessExitError's own-property tags, which are not in the SDK's
// public types. These tests run the real `query()` with a CLI child that ends ITSELF once the
// SDK has written its first stdin line, so an SDK bump that renames or drops the tags turns
// them red instead of silently degrading the classifier to "fail the run".

/** A child that ends itself after its first stdin line, recorded so the test can await it. */
function selfEndingSpawn(body: string, children: ChildProcess[]): NonNullable<SdkExecutorOptions["spawn"]> {
  return () => {
    const child = spawn(process.execPath, ["-e", `process.stdin.once("data", () => { ${body} });`], {
      stdio: ["pipe", "pipe", "pipe"],
    });
    children.push(child);
    return child;
  };
}

async function reaped(children: ChildProcess[]): Promise<void> {
  await Promise.all(
    children.map((c) =>
      c.exitCode !== null || c.signalCode !== null ? undefined : new Promise((r) => c.once("exit", r)),
    ),
  );
}

async function realSdkError(body: string): Promise<Error & Record<string, unknown>> {
  const children: ChildProcess[] = [];
  const spawnFn = selfEndingSpawn(body, children);
  try {
    const q = query({
      prompt: "hi",
      options: {
        env: { PATH: process.env.PATH ?? "" },
        spawnClaudeCodeProcess: (o) => spawnFn(o) as unknown as ReturnType<NonNullable<SdkOptions["spawnClaudeCodeProcess"]>>,
      },
    });
    for await (const _m of q) { /* the child emits nothing */ }
  } catch (err) {
    return err as Error & Record<string, unknown>;
  } finally {
    await reaped(children);
  }
  throw new Error("the SDK did not throw on the child's exit");
}

describe("SDK process-exit error shape (issue #1656 pin)", () => {
  it("exit 143 carries own errorClass=process_exited_nonzero and exitCode=143", async () => {
    const err = await realSdkError("process.exit(143);");
    assert.ok(Object.hasOwn(err, "errorClass") && Object.hasOwn(err, "exitCode"), "own-property tags");
    assert.equal(err["errorClass"], "process_exited_nonzero");
    assert.equal(err["exitCode"], 143);
  });

  it("a self-SIGTERM carries own errorClass=process_killed_by_signal and signal=SIGTERM", async () => {
    const err = await realSdkError('process.kill(process.pid, "SIGTERM");');
    assert.ok(Object.hasOwn(err, "errorClass") && Object.hasOwn(err, "signal"), "own-property tags");
    assert.equal(err["errorClass"], "process_killed_by_signal");
    assert.equal(err["signal"], "SIGTERM");
  });

  for (const [label, body, expectResume] of [
    ["exit 143", "process.exit(143);", true],
    ["self-SIGTERM", 'process.kill(process.pid, "SIGTERM");', true],
    ["self-SIGSEGV", 'process.kill(process.pid, "SIGSEGV");', false],
  ] as const) {
    it(`through the executor, the real SDK's ${label} ${expectResume ? "resumes" : "fails the run"}`, async () => {
      const children: ChildProcess[] = [];
      const fake = fakeTurns([
        [submitPlan("# Plan", "prev"), resultSuccess("prev")],
        [signalDone("prev"), resultSuccess("prev")],
      ]);
      const resumes: Array<string | undefined> = [];
      let call = 0;
      // Turn 0 runs the REAL SDK through the production query seam (its CLI is the
      // self-ending child); later turns are faked.
      const queryFn: SdkQueryFn = (params) => {
        resumes.push(params.options.resume);
        return call++ === 0 ? defaultQueryFn(params) : fake.queryFn(params);
      };
      const run = new SdkExecutor(
        nullLogger(),
        homeDir,
        opts(queryFn, { spawn: selfEndingSpawn(body, children) }),
      ).run(makeCtx({ sessionId: "prev" }).ctx);
      try {
        if (expectResume) {
          const result = await run;
          assert.equal(result.branch, "agent/issue-5");
          assert.deepEqual(resumes, ["prev", "prev", "prev"]);
        } else {
          const err = await rejection(run);
          assert.match(err.message, /terminated by signal SIGSEGV/);
          assert.deepEqual(resumes, ["prev"]);
        }
      } finally {
        await reaped(children);
      }
    });
  }
});
