import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { AgentTemplate } from "../src/protocol.js";
import type { CapturedMemoryContext } from "../src/codex/broker.js";
import { ClaudeCommandLane, GuardedCommandUnconfirmedError } from "../src/claude-command/lane.js";
import { GUARDED_COMMAND_DESCRIPTION } from "../src/claude-command/server.js";
import { GUARDED_COMMAND_SERVER, GUARDED_COMMAND_TOOL } from "../src/claude-command/grants.js";
import { ASYNC_DEFERRAL_TOOLS, NESTED_AGENT_TOOL } from "../src/guardrails.js";
import { CLAUDE_GUARDED_COMMAND_APPEND, CLAUDE_LONG_COMMAND_APPEND } from "../src/prompt.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

// Issue #2471 M3: the SDK executor's guard wiring, driven through a fake SDK query.

const BACKGROUND_RECIPE = "run_in_background: true";
const UNAVAILABLE_TEXT = "unavailable on this run: scoped Bash grants are not supported under the memory guard";

function submitPlan(plan: string): SDKMessage {
  return { type: "assistant", session_id: "sess-1", message: { content: [
    { type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] } } as unknown as SDKMessage;
}
function signalDone(): SDKMessage {
  return { type: "assistant", session_id: "sess-1", message: { content: [
    { type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] } } as unknown as SDKMessage;
}
function resultSuccess(): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "sess-1" } as unknown as SDKMessage;
}

/** Hang until `signal` aborts. Never yielding is the fixture. */
async function hang(signal: AbortSignal): Promise<void> {
  await new Promise<void>((resolve) => {
    if (signal.aborted) return resolve();
    const keepAlive = setInterval(() => {}, 1_000);
    signal.addEventListener("abort", () => { clearInterval(keepAlive); resolve(); }, { once: true });
  });
}

type Script = SDKMessage[] | ((signal: AbortSignal, options: SdkOptions) => AsyncIterable<SDKMessage>);

function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; seen: SdkOptions[] } {
  const seen: SdkOptions[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    seen.push(params.options as SdkOptions);
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      if (typeof script === "function") yield* script(params.options.abortController!.signal, params.options as SdkOptions);
      else for (const m of script) yield m;
    })();
  };
  return { queryFn, seen };
}

/** A turn that runs `act`, then hangs until the turn is aborted. */
function actThenHang(act: () => void): Script {
  // Never yielding is the fixture.
  // eslint-disable-next-line require-yield
  return (signal) => (async function* () {
    act();
    await hang(signal);
  })();
}

const template = (name: string, tools: string[] | null): AgentTemplate =>
  ({ name, description: `${name} role`, prompt_body: `You are ${name}.`, tools, model: null }) as AgentTemplate;

let homeDir: string;
let saved: Record<string, string | undefined>;
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-cmd-exec");

interface Registry { protects: Array<{ kind: string; root: { pid: number; startTime: string } }>; unprotects: unknown[] }

function fakeCapture(): { capture: () => CapturedMemoryContext; calls: { n: number }; registry: Registry } {
  const registry: Registry = { protects: [], unprotects: [] };
  const commands = {
    protect: (kind: string, root: { pid: number; startTime: string }) => {
      const token = { kind, root };
      registry.protects.push(token);
      return token;
    },
    unprotect: (token: unknown) => { registry.unprotects.push(token); return true; },
  };
  const calls = { n: 0 };
  return {
    calls, registry,
    capture: () => { calls.n++; return { commands, context: {}, flight: {}, lifecycleRevision: 1 } as unknown as CapturedMemoryContext; },
  };
}

class Stop extends Error { constructor() { super("fixture retained stop"); } }

function makeCtx(overrides: Partial<RunContext> = {}): RunContext {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  return {
    runId: "r1", issueIid: 5, issueTitle: "Fix login", issueDescription: "please implement",
    worktreePath: nonexistentWorktree(), branch: "agent/issue-5", emit: () => {},
    oauthToken: "dummy-oauth-token-do-not-scan-0000",
    agents: [], config: null, sessionId: null, onSessionId: () => {},
    gatePlan: async () => approve, pullFollowUp: () => undefined, reportIteration: () => {},
    checkpoint: async () => {}, worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
    ...overrides,
  };
}

const planAndDone = (): Script[] => [[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]];

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-cmdexec-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-2222";
  process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-1111";
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k]; else process.env[k] = v;
  }
});

const matchers = (options: SdkOptions): string[] => (options.hooks?.PreToolUse ?? []).map((m) => m.matcher ?? "");
const promptOf = (options: SdkOptions): string => {
  const sp = options.systemPrompt;
  return typeof sp === "object" && sp !== null && "append" in sp ? String(sp.append) : String(sp);
};
const agentsOf = (options: SdkOptions): Record<string, { prompt: string; tools?: string[] }> =>
  (options.agents ?? {}) as Record<string, { prompt: string; tools?: string[] }>;

describe("default-off: no ctx.capturedMemory", () => {
  it("keeps the options, hooks, prompts and roster of an unguarded run", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const roster = [template("coder", ["Bash", "Read"]), template("scoped", ["Bash(git status:*)", "Read"]), template("reader", null)];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ agents: roster }));
    assert.equal(seen.length, 2, "a plan turn and an implement turn");
    for (const options of seen) {
      assert.equal("toolAliases" in options, false);
      assert.deepEqual(options.disallowedTools, [...ASYNC_DEFERRAL_TOOLS]);
      assert.equal(GUARDED_COMMAND_SERVER in (options.mcpServers ?? {}), false);
      assert.equal(matchers(options).includes(GUARDED_COMMAND_TOOL), false);
      assert.equal(matchers(options).filter((m) => m === "Bash").length, 1, "only the original Bash hook");
      assert.ok(promptOf(options).includes(CLAUDE_LONG_COMMAND_APPEND));
      assert.equal(promptOf(options).includes(CLAUDE_GUARDED_COMMAND_APPEND), false);
      assert.equal(promptOf(options).includes("Subagent roles withheld"), false);
      const agents = agentsOf(options);
      assert.deepEqual(Object.keys(agents).sort(), ["coder", "reader", "scoped"], "no role is withheld");
      assert.deepEqual(agents["scoped"]!.tools?.slice(0, 2), ["Bash(git status:*)", "Read"]);
      assert.deepEqual(agents["coder"]!.tools?.slice(0, 2), ["Bash", "Read"]);
      for (const def of Object.values(agents)) {
        assert.ok(def.prompt.includes(CLAUDE_LONG_COMMAND_APPEND));
        assert.equal(def.prompt.includes(CLAUDE_GUARDED_COMMAND_APPEND), false);
      }
    }
  });
});

describe("guard-bound run", () => {
  it("routes Bash to the command tool in the plan and the implement turn", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      agents: [template("coder", ["Bash", "Read"]), template("scoped", ["Bash(git status:*)", "Read"]), template("reader", null)],
    }));
    assert.equal(seen.length, 2);
    for (const options of seen) {
      assert.deepEqual(options.toolAliases, { Bash: GUARDED_COMMAND_TOOL });
      assert.deepEqual(options.disallowedTools, [...ASYNC_DEFERRAL_TOOLS, "Bash", "Monitor"]);
      assert.ok(GUARDED_COMMAND_SERVER in (options.mcpServers ?? {}));
      const names = matchers(options);
      assert.deepEqual(names.slice(0, 3), ["Bash", "Bash", GUARDED_COMMAND_TOOL], "the original Bash hook stays, the lane hook rides both names");
      const entries = options.hooks!.PreToolUse!;
      assert.equal(entries[1]!.hooks[0], entries[2]!.hooks[0], "one lane hook under both matchers");
      assert.notEqual(entries[0]!.hooks[0], entries[1]!.hooks[0]);
    }
  });

  it("states the guarded recipe in the lead and subagent prompts and never the native background one", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      agents: [template("coder", ["Bash", "Read"]), template("scoped", ["Bash(git status:*)", "Read"]), template("reader", null)],
    }));
    for (const options of seen) {
      const lead = promptOf(options);
      assert.ok(lead.includes(CLAUDE_GUARDED_COMMAND_APPEND));
      assert.equal(lead.includes(CLAUDE_LONG_COMMAND_APPEND), false);
      assert.equal(lead.includes(BACKGROUND_RECIPE), false);
      for (const def of Object.values(agentsOf(options))) {
        assert.ok(def.prompt.includes(CLAUDE_GUARDED_COMMAND_APPEND));
        assert.equal(def.prompt.includes(BACKGROUND_RECIPE), false);
        assert.equal(def.prompt.includes(CLAUDE_LONG_COMMAND_APPEND), false);
      }
    }
    for (const text of [CLAUDE_GUARDED_COMMAND_APPEND, GUARDED_COMMAND_DESCRIPTION]) {
      assert.match(text, /120000/);
      assert.match(text, /3600000/);
      assert.match(text, /explicit `?timeout/);
      assert.match(text, /60 minutes/);
      assert.match(text, /64 MiB/);
      assert.match(text, /truncated|bounded/);
      assert.match(text, /8 commands/);
    }
    assert.match(CLAUDE_GUARDED_COMMAND_APPEND, /blocker/);
  });

  it("withholds a scoped-only role from the roster, lists it, and denies it by name", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    const statuses: string[] = [];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      emit: (m) => { const text = (m.payload as { text?: unknown } | undefined)?.text; if (typeof text === "string") statuses.push(text); },
      agents: [template("coder", ["Bash", "Read"]), template("scoped", ["Bash(git status:*)", "Read"]), template("reader", null)],
    }));
    for (const options of seen) {
      const agents = agentsOf(options);
      assert.deepEqual(Object.keys(agents).sort(), ["coder", "reader"]);
      assert.deepEqual(agents["coder"]!.tools?.slice(0, 2), [GUARDED_COMMAND_TOOL, "Read"]);
      assert.equal(agents["coder"]!.tools?.includes("Bash"), false);
      assert.ok(promptOf(options).includes(`- scoped: ${UNAVAILABLE_TEXT}`));
      const guard = options.hooks!.PreToolUse!.find((m) => m.matcher === NESTED_AGENT_TOOL)!.hooks[0]!;
      const call = (sub: string) => guard({ hook_event_name: "PreToolUse", tool_name: NESTED_AGENT_TOOL,
        tool_input: { subagent_type: sub, prompt: "do it" }, tool_use_id: "u1", session_id: "s", transcript_path: "", cwd: "" } as never,
        "u1", { signal: new AbortController().signal });
      const denied = await call("scoped") as { hookSpecificOutput?: { permissionDecision?: string; permissionDecisionReason?: string } };
      assert.equal(denied.hookSpecificOutput?.permissionDecision, "deny");
      assert.ok(denied.hookSpecificOutput?.permissionDecisionReason?.includes(UNAVAILABLE_TEXT));
      const unknown = await call("nobody") as { hookSpecificOutput?: { permissionDecisionReason?: string } };
      assert.equal(unknown.hookSpecificOutput?.permissionDecisionReason?.includes("scoped Bash"), false, "an unknown name keeps the generic reason");
      const allowed = await call("coder") as { hookSpecificOutput?: { permissionDecision?: string } };
      assert.notEqual(allowed.hookSpecificOutput?.permissionDecision, "deny");
    }
    assert.ok(statuses.some((t) => t.includes("not planning with scoped") && t.includes(UNAVAILABLE_TEXT)));
    assert.ok(statuses.some((t) => t.includes("not implementing with scoped") && t.includes(UNAVAILABLE_TEXT)));
  });

  it("captures the memory context once per turn and a renewal never aborts the approval handoff", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture, calls } = fakeCapture();
    let subscribed = 0;
    let unsubscribed = 0;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      onMemoryAuthorityLost: () => { subscribed++; return () => { unsubscribed++; }; },
    }));
    assert.equal(seen.length, 2, "plan approval then the implement turn proceeds");
    assert.equal(calls.n, 2, "one capture per turn");
    assert.equal(result.walled, undefined);
    assert.equal(subscribed, 1);
    assert.equal(unsubscribed, 1, "the listener is dropped at run end");
  });
});

describe("typed trip carrier", () => {
  it("a mid-turn invalidation aborts the turn and the original stop reaches the caller with no resume", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    const { queryFn, seen } = fakeTurns([[submitPlan("plan"), resultSuccess()], actThenHang(() => fire(stop))]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
      })),
      (err: unknown) => err === stop,
    );
    assert.equal(seen.length, 2, "the aborted turn is not resumed or re-driven");
  });

  it("a stop landing during an idle gap surfaces at the next turn instead of being cleared", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    const { queryFn, seen } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
        // The invalidation arrives while the run waits at the plan gate.
        gatePlan: async () => { fire(stop); return { kind: "approve", selection: { status: "absent" } }; },
      })),
      (err: unknown) => err === stop,
    );
    assert.equal(seen.length, 1, "no implement turn starts after the memory authority is lost");
  });

  it("pause first, then invalidation: the pause parks and reports normally", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    let pause: () => void = () => {};
    const { queryFn, seen } = fakeTurns([[submitPlan("plan"), resultSuccess()], actThenHang(() => { pause(); fire(stop); })]);
    const { capture } = fakeCapture();
    let parks = 0;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
      onPauseNow: (cb) => { pause = cb; },
      parkForPause: async () => { parks++; return true; },
    }));
    assert.ok(result.pausedAt, "the run parked on the owner's pause");
    assert.equal(parks, 1);
    assert.equal(seen.length, 2);
  });

  it("wall first, then invalidation: the stop wins over the wall park", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    let wallParks = 0;
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        await hang(signal); // the wall timer aborts the turn first
        fire(stop);
      })(),
    ]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        capturedMemory: capture,
        config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.05 },
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
        parkForWall: async () => { wallParks++; return "parked"; },
      })),
      (err: unknown) => err === stop,
    );
    assert.equal(wallParks, 0, "the wall park must not swallow the lost memory authority");
  });

  it("an unconfirmed lane join on a clean turn fails the run through the carrier", async (t) => {
    let joins = 0;
    t.mock.method(ClaudeCommandLane.prototype, "quiesce", async () => { joins++; throw new GuardedCommandUnconfirmedError("fixture unconfirmed"); });
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ capturedMemory: capture })),
      (err: unknown) => err instanceof GuardedCommandUnconfirmedError,
    );
    assert.equal(seen.length, 1, "no further turn after an unconfirmed join");
    assert.ok(joins >= 1);
  });

  it("an in-flight stop beats a later join failure (join mocked after the stop)", async (t) => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    let mocked = false;
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], actThenHang(() => {
      // From the implement turn on, every join fails.
      t.mock.method(ClaudeCommandLane.prototype, "quiesce", async () => { throw new GuardedCommandUnconfirmedError("fixture unconfirmed"); });
      mocked = true;
      fire(stop);
    })]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
      })),
      (err: unknown) => err === stop,
    );
    assert.ok(mocked);
  });
});

describe("protected CLI roots and executor seams", () => {
  it("protects each guarded CLI root at spawn, unprotects it at exit and sweeps the rest at run end", async () => {
    const { capture, registry } = fakeCapture();
    const procs: EventEmitter[] = [];
    const spawn = (): never => {
      const proc = Object.assign(new EventEmitter(), { pid: 40000 + procs.length });
      procs.push(proc);
      return proc as never;
    };
    const { queryFn } = fakeTurns([
      (_signal, options) => (async function* () {
        const proc = options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        (proc as unknown as EventEmitter).emit("exit", 0, null);
        yield submitPlan("plan");
        yield resultSuccess();
      })(),
      (_signal, options) => (async function* () {
        options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        yield signalDone();
        yield resultSuccess();
      })(),
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn, rootStartTime: (pid) => pid * 10, killCliGroup: () => true, kill: () => true })
      .run(makeCtx({ capturedMemory: capture }));
    assert.deepEqual(registry.protects.map((p) => [p.kind, p.root]), [
      ["provider", { pid: 40000, startTime: "400000" }],
      ["provider", { pid: 40001, startTime: "400010" }],
    ]);
    assert.equal(registry.unprotects.length, 2, "the exited root at its exit and the other by the run-end sweep");
    assert.equal(registry.unprotects[0], registry.protects[0], "the exit unprotects exactly its own root");
  });

  it("protects nothing on an unguarded run", async () => {
    const { registry } = fakeCapture();
    const spawn = (): never => Object.assign(new EventEmitter(), { pid: 40100 }) as never;
    const { queryFn } = fakeTurns([
      (_signal, options) => (async function* () {
        options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        yield submitPlan("plan");
        yield resultSuccess();
      })(),
      [signalDone(), resultSuccess()],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn, rootStartTime: () => 1, killCliGroup: () => true, kill: () => true }).run(makeCtx());
    assert.equal(registry.protects.length, 0);
  });

  it("unions the lane's recorded roots, settles through the lane and starts its disposal at killAgentTree", async () => {
    const { queryFn } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn, killCliGroup: () => true, kill: () => true,
      runProcesses: { reap: async () => ({ killed: [], left: [], complete: true }), scan: async () => ({ procs: [], complete: true }) } as never });
    assert.deepEqual(await executor.settleWorkerCommands(10), { confirmed: true }, "no lane: nothing to settle");
    await executor.run(makeCtx({ capturedMemory: capture }));
    const lane = (executor as unknown as { guardLane: ClaudeCommandLane }).guardLane;
    assert.ok(lane, "a guard-bound run owns a lane");
    // The lane's own state is fixture-driven here: its disposal behaviour is covered by the lane tests.
    let roots = [{ pid: 777, startTime: 123 }];
    let quiesces = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let failing = false;
    (lane as unknown as { recordedRoots: () => typeof roots }).recordedRoots = () => roots;
    (lane as unknown as { quiesce: () => Promise<void> }).quiesce = async () => { quiesces++; await gate; if (failing) throw new GuardedCommandUnconfirmedError("x"); };
    assert.deepEqual(executor.recordedRootPids(), roots, "lane roots are reported beside the CLI roots");
    executor.killAgentTree();
    assert.equal(quiesces, 1, "killAgentTree starts the disposal synchronously");
    let reaped = false;
    const reap = executor.reapAttributedProcesses().then(() => { reaped = true; });
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(reaped, false, "the attributed reap waits for the lane disposal");
    release();
    await reap;
    assert.equal(reaped, true);
    roots = [];
    assert.deepEqual(executor.recordedRootPids(), []);
    failing = true;
    assert.deepEqual(await executor.settleWorkerCommands(10), { confirmed: false }, "an unconfirmed lane never settles");
  });
});
