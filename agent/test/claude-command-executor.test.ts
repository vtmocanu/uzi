import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { CredentialSwitchSignal, PauseNowSignal, type PauseMode } from "../src/steering.js";
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
    gatePlan: async () => approve, pullFollowUps: () => [], reportIteration: () => {},
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
const agentsOf = (options: SdkOptions): Record<string, { prompt: string; tools?: string[]; disallowedTools?: string[] }> =>
  (options.agents ?? {}) as Record<string, { prompt: string; tools?: string[]; disallowedTools?: string[] }>;

describe("default-off: no ctx.capturedMemory", () => {
  it("keeps the options, hooks, prompts and roster of an unguarded run", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const roster = [template("coder", ["Bash", "Read"]), template("scoped", ["Bash(git status:*)", "Read"]), template("reader", null)];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ agents: roster }));
    assert.equal(seen.length, 2, "a plan turn and an implement turn");
    for (const options of seen) {
      assert.equal("toolAliases" in options, false);
      assert.deepEqual(options.disallowedTools, [...ASYNC_DEFERRAL_TOOLS, "AskUserQuestion"]);
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
      assert.deepEqual(options.disallowedTools, [...ASYNC_DEFERRAL_TOOLS, "AskUserQuestion", "Bash", "Monitor"]);
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

  it("PRD #3914 M3: a repo-sourced plan roster is shaped to the guarded command lane", async () => {
    const { queryFn, seen } = fakeTurns(planAndDone());
    const { capture } = fakeCapture();
    const statuses: string[] = [];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      capturedMemory: capture,
      emit: (m) => { const text = (m.payload as { text?: unknown } | undefined)?.text; if (typeof text === "string") statuses.push(text); },
      agents: [template("coder", ["Bash", "Read"])],
      repoAgents: [template("repo-coder", ["Bash", "Read", "Edit"]), template("repo-scoped", ["Bash(git status:*)", "Read"])],
      requestedAgentSource: "repo",
    }));
    const plan = agentsOf(seen[0]!);
    assert.deepEqual(Object.keys(plan), ["repo-coder"]);
    assert.equal(plan["repo-coder"]!.tools?.[0], GUARDED_COMMAND_TOOL);
    assert.equal(plan["repo-coder"]!.tools?.includes("Bash"), false);
    assert.equal(plan["repo-coder"]!.tools?.includes("Edit"), false);
    assert.ok(plan["repo-coder"]!.disallowedTools?.includes("Write"));
    assert.ok(promptOf(seen[0]!).includes(`- repo-scoped: ${UNAVAILABLE_TEXT}`));
    const guard = seen[0]!.hooks!.PreToolUse!.find((m) => m.matcher === NESTED_AGENT_TOOL)!.hooks[0]!;
    const call = (sub: string) => guard({ hook_event_name: "PreToolUse", tool_name: NESTED_AGENT_TOOL,
      tool_input: { subagent_type: sub, prompt: "do it" }, tool_use_id: "u1", session_id: "s", transcript_path: "", cwd: "" } as never,
      "u1", { signal: new AbortController().signal });
    const scoped = await call("repo-scoped") as { hookSpecificOutput?: { permissionDecision?: string; permissionDecisionReason?: string } };
    assert.equal(scoped.hookSpecificOutput?.permissionDecision, "deny");
    assert.ok(scoped.hookSpecificOutput?.permissionDecisionReason?.includes(UNAVAILABLE_TEXT));
    const own = await call("coder") as { hookSpecificOutput?: { permissionDecision?: string } };
    assert.equal(own.hookSpecificOutput?.permissionDecision, "deny");
    assert.ok(statuses.some((t) => t.includes("not planning with repo-scoped") && t.includes(UNAVAILABLE_TEXT)));
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

  it("an invalidation between turns signals no CLI pid (the last turn's pid may be recycled)", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    const kills: Array<number | undefined> = [];
    let killsAtFire = -1;
    const spawn = (): never => Object.assign(new EventEmitter(), { pid: 41000 }) as never;
    const { queryFn } = fakeTurns([
      (_signal, options) => (async function* () {
        options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        yield submitPlan("plan");
        yield resultSuccess();
      })(),
      [signalDone(), resultSuccess()],
    ]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn, rootStartTime: () => 1, killCliGroup: () => true, kill: (pid) => { kills.push(pid); return true; } }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
        // The plan turn's CLI has exited by now; the invalidation arrives at the gate.
        gatePlan: async () => { fire(stop); killsAtFire = kills.length; return { kind: "approve", selection: { status: "absent" } }; },
      })),
      (err: unknown) => err === stop,
    );
    assert.equal(killsAtFire, 0, "no kill is issued for a trip that lands with no live turn");
  });

  it("a pause that trips at the plan gate (between turns) never signals the finished turn's CLI pid", async () => {
    // Regression for the between-turns pid reset: driveTurn's finally forgets the CLI pid, so the
    // generic trip (pause, cancel, wall) that lands at the gate has no pid left to group-kill.
    let pause: () => void = () => {};
    const kills: Array<number | undefined> = [];
    let killedAtFire: Array<number | undefined> = [];
    let spawned: number | undefined;
    const spawn = (): never => { spawned = 41002; return Object.assign(new EventEmitter(), { pid: 41002 }) as never; };
    const { queryFn } = fakeTurns([
      (_signal, options) => (async function* () {
        options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        yield submitPlan("plan");
        yield resultSuccess();
      })(),
      [signalDone(), resultSuccess()],
    ]);
    const { capture } = fakeCapture();
    let parks = 0;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn, rootStartTime: () => 1, killCliGroup: () => true, kill: (pid) => { kills.push(pid); return true; } }).run(makeCtx({
      capturedMemory: capture,
      onPauseNow: (cb) => { pause = cb; },
      parkForPause: async () => { parks++; return true; },
      // The plan turn's CLI (pid 41002) has exited by now; the pause lands at the gate.
      gatePlan: async () => { pause(); killedAtFire = [...kills]; return { kind: "approve", selection: { status: "absent" } }; },
    }));
    assert.equal(spawned, 41002, "the plan turn did spawn a CLI with the pid under test");
    assert.equal(killedAtFire.includes(41002), false, "the pause at the gate never names the finished turn's pid");
    assert.ok(result.pausedAt, "the pause still parks the run");
    assert.equal(parks, 1);
  });

  it("an invalidation during a live turn group-kills that turn's CLI", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    const kills: Array<number | undefined> = [];
    let killedAtFire: Array<number | undefined> = [];
    const spawn = (): never => Object.assign(new EventEmitter(), { pid: 41001 }) as never;
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      // eslint-disable-next-line require-yield
      (signal, options) => (async function* () {
        options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        fire(stop);
        killedAtFire = [...kills];
        await hang(signal);
      })(),
    ]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn, rootStartTime: () => 1, killCliGroup: () => true, kill: (pid) => { kills.push(pid); return true; } }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
      })),
      (err: unknown) => err === stop,
    );
    assert.deepEqual(killedAtFire, [41001]);
  });

  it("a credential switch that gives up while a carried stop is pending rethrows the stop and does not continue", async () => {
    const stop = new Stop();
    let fire: (stop: Error) => void = () => {};
    let gates = 0;
    let switches = 0;
    const { queryFn, seen } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const { capture } = fakeCapture();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        capturedMemory: capture,
        onMemoryAuthorityLost: (listener) => { fire = listener; return () => {}; },
        gatePlan: async () => {
          gates++;
          // The invalidation lands first (the carrier), then the credential switch interrupts the gate.
          if (gates > 1) return { kind: "approve", selection: { status: "absent" } };
          fire(stop);
          throw new CredentialSwitchSignal();
        },
        attemptCredentialSwitch: async () => { switches++; return "gave_up"; },
      })),
      (err: unknown) => err === stop,
    );
    assert.equal(switches, 1);
    assert.equal(gates, 1, "the clear-and-continue path did not re-present the gate");
    assert.equal(seen.length, 1, "no implement turn starts");
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
    // Read inside the script, right after the exit and before the run ends: the run-end sweep
    // would otherwise satisfy the exit expectation on its own.
    let unprotectsAtExit = -1;
    const { queryFn } = fakeTurns([
      (_signal, options) => (async function* () {
        const proc = options.spawnClaudeCodeProcess!({ command: "claude", args: [], cwd: "/", env: {}, signal: new AbortController().signal });
        (proc as unknown as EventEmitter).emit("exit", 0, null);
        unprotectsAtExit = registry.unprotects.length;
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
    assert.equal(unprotectsAtExit, 1, "the exit hook unprotects the exited root before the run ends");
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

const TOOL_INPUT = { command: "echo hi" };

function liveCapture(): { capture: () => CapturedMemoryContext } {
  const { capture } = fakeCapture();
  return {
    capture: () => {
      const captured = capture() as unknown as { commands: Record<string, unknown> };
      Object.assign(captured.commands, {
        isCurrent: () => true,
        admissionReason: () => undefined,
        admissionMessage: () => "x",
        phaseRevision: () => 1,
      });
      return captured as unknown as CapturedMemoryContext;
    },
  };
}

/** Attribute and run one guarded command on the executor's lane; launch is stubbed and counted. */
function guardedRunner(executor: SdkExecutor): { run: (id: string) => Promise<string>; launches: { n: number } } {
  const launches = { n: 0 };
  let stubbed: ClaudeCommandLane | undefined;
  return {
    launches,
    run: async (id) => {
      const lane = (executor as unknown as { guardLane: ClaudeCommandLane }).guardLane;
      if (stubbed !== lane) {
        (lane as unknown as { launch: () => Promise<never> }).launch = async () => { launches.n++; throw new Error("sentinel-launch"); };
        stubbed = lane;
      }
      await (lane.hook as (input: unknown) => Promise<unknown>)({
        hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: TOOL_INPUT, tool_use_id: id,
      });
      const result = await lane.handle(TOOL_INPUT, { _meta: { "claudecode/toolUseId": id } });
      return result.content.map((c) => c.text ?? "").join("");
    },
  };
}

/** A turn that runs one guarded command, then yields `messages`. */
function commandTurn(run: (id: string) => Promise<string>, id: string, texts: string[], messages: SDKMessage[]): (signal: AbortSignal) => AsyncIterable<SDKMessage> {
  return () => (async function* () {
    texts.push(await run(id));
    yield* messages;
  })();
}

const LAUNCHED = "Command could not be started: sentinel-launch";
const REFUSED = "cancelled before it started";

// Issue #4076: a credential switch that gives up leaves the shared cancel signal aborted with a
// CredentialSwitchSignal. The retried turn's guarded commands must still run.
describe("guarded commands after a given-up credential switch (#4076)", () => {
  it("a planning turn retried after a given-up switch runs its guarded command", async () => {
    const cancel = new AbortController();
    const texts: string[] = [];
    let switches = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn, seen } = fakeTurns([
      actThenHang(() => cancel.abort(new CredentialSwitchSignal())),
      (signal) => commandTurn((id) => runner.run(id), "toolu_1", texts, [submitPlan("plan"), resultSuccess()])(signal),
      [signalDone(), resultSuccess()],
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      capturedMemory: liveCapture().capture,
      signal: cancel.signal,
      attemptCredentialSwitch: async () => { switches++; return "gave_up"; },
    }));
    assert.equal(switches, 1);
    assert.equal(seen.length, 3, "aborted plan turn, retried plan turn, implement turn");
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes("Command could not be started: sentinel-launch"), String(texts[0]));
    assert.equal(runner.launches.n, 1);
  });

  it("an implement turn retried after a given-up switch runs its guarded command", async () => {
    const cancel = new AbortController();
    const texts: string[] = [];
    let switches = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn, seen } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      actThenHang(() => cancel.abort(new CredentialSwitchSignal())),
      (signal) => commandTurn((id) => runner.run(id), "toolu_2", texts, [signalDone(), resultSuccess()])(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      capturedMemory: liveCapture().capture,
      signal: cancel.signal,
      attemptCredentialSwitch: async () => { switches++; return "gave_up"; },
    }));
    assert.equal(switches, 1);
    assert.equal(seen.length, 3, "plan turn, aborted implement turn, retried implement turn");
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes("Command could not be started: sentinel-launch"), String(texts[0]));
    assert.equal(runner.launches.n, 1);
  });

  it("behaviour check: a command in the turn of a pending switch is refused as cancelled", async () => {
    const cancel = new AbortController();
    const texts: string[] = [];
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        cancel.abort(new CredentialSwitchSignal());
        texts.push(await runner.run("toolu_3"));
        await hang(signal);
      })(),
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      capturedMemory: liveCapture().capture,
      signal: cancel.signal,
      attemptCredentialSwitch: async () => "gave_up",
    }));
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes("cancelled before it started"), String(texts[0]));
    assert.equal(runner.launches.n, 0);
  });

  it("behaviour check: a terminal lifecycle stop after the give-up still refuses guarded commands", async () => {
    const cancel = new AbortController();
    const terminal = new AbortController();
    const texts: string[] = [];
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      actThenHang(() => cancel.abort(new CredentialSwitchSignal())),
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        terminal.abort(new Stop());
        texts.push(await runner.run("toolu_4"));
        await hang(signal);
      })(),
      [signalDone(), resultSuccess()],
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    const outcome = await executor.run(makeCtx({
      capturedMemory: liveCapture().capture,
      signal: cancel.signal,
      terminalLifecycleSignal: terminal.signal,
      attemptCredentialSwitch: async () => "gave_up",
    })).then(() => undefined, (err: unknown) => err);
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes("cancelled before it started"), String(texts[0]));
    assert.equal(runner.launches.n, 0);
    assert.ok(outcome instanceof Error && /run cancelled/.test(outcome.message), "the run ends as cancelled");
  });
});

// Issue #4090: a `now`/`wall` pause aborts the shared cancel signal with a PauseNowSignal, which
// cannot be reset. When the park is declined or refused the run continues in place, and the
// restarted turn's guarded commands must run, unless an interrupt was dropped by the first-wins
// trip this run (then the stale abort keeps fencing in front of the lost stop).
describe("guarded commands after a declined or refused pause (#4090)", () => {
  /** Mirrors steering: abort the shared controller, then fire the paired re-armable interrupt. */
  function steering(): {
    cancel: AbortController;
    ctx: Partial<RunContext>;
    pause: (mode: "now" | "wall") => void;
    switchCredential: () => void;
    pauseInterrupt: () => void;
    setMode: (mode: PauseMode) => void;
  } {
    const cancel = new AbortController();
    let mode: PauseMode = null;
    let onPause: (() => void) | undefined;
    let onSwitch: (() => void) | undefined;
    return {
      cancel,
      ctx: {
        signal: cancel.signal,
        pauseModeRequested: () => mode,
        clearWallMode: () => { if (mode === "wall") mode = null; },
        onPauseNow: (cb) => { onPause = cb; },
        onCredentialSwitch: (cb) => { onSwitch = cb; },
      },
      pause: (next) => {
        mode = next;
        if (!cancel.signal.aborted) cancel.abort(new PauseNowSignal());
        onPause?.();
      },
      switchCredential: () => {
        if (!cancel.signal.aborted) cancel.abort(new CredentialSwitchSignal());
        onSwitch?.();
      },
      pauseInterrupt: () => onPause?.(),
      setMode: (next) => { mode = next; },
    };
  }

  const planTurn = (): Script => [submitPlan("plan"), resultSuccess()];
  const doneMessages = (): SDKMessage[] => [signalDone(), resultSuccess()];
  const settle = (p: Promise<unknown>): Promise<unknown> => p.then(() => undefined, (err: unknown) => err);

  it("(a) a declined `now` pause: the restarted turn's guarded command launches, after one park request", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn, seen } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      (signal) => commandTurn((id) => runner.run(id), "toolu_a", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => { parks++; s.setMode(null); return false; },
    }));
    assert.equal(parks, 1, "the abort and its paired interrupt are one stop");
    assert.equal(seen.length, 3);
    assert.deepEqual(texts.map((t) => t.includes(LAUNCHED)), [true], String(texts));
    assert.equal(runner.launches.n, 1);
  });

  it("(b) a refused `wall` pause: the restarted turn's guarded command launches", async () => {
    const s = steering();
    const texts: string[] = [];
    let walls = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("wall")),
      (signal) => commandTurn((id) => runner.run(id), "toolu_b", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForWall: async () => { walls++; return "refused"; },
    }));
    assert.equal(walls, 1);
    assert.deepEqual(texts.map((t) => t.includes(LAUNCHED)), [true], String(texts));
    assert.equal(runner.launches.n, 1);
  });

  it("(c) a refused REASON_WALL park with the shared signal never aborted: the guarded command launches", async () => {
    const s = steering();
    const texts: string[] = [];
    let walls = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => {}), // the local wall timer trips the turn
      (signal) => commandTurn((id) => runner.run(id), "toolu_c", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.05 },
      reportIteration: async () => (walls > 0 ? { totalWallSeconds: 60 } : undefined),
      parkForWall: async () => { walls++; return "refused"; },
    }));
    assert.equal(walls, 1);
    assert.equal(s.cancel.signal.aborted, false);
    assert.deepEqual(texts.map((t) => t.includes(LAUNCHED)), [true], String(texts));
    assert.equal(runner.launches.n, 1);
  });

  it("(c2) a pause landing while the wall trip is held stays fenced after the refused park", async () => {
    const s = steering();
    const texts: string[] = [];
    let walls = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => {}),
      (signal) => commandTurn((id) => runner.run(id), "toolu_c2", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.05 },
      reportIteration: async () => (walls > 0 ? { totalWallSeconds: 60 } : undefined),
      parkForWall: async () => { walls++; s.pause("wall"); return "refused"; },
    })));
    assert.equal(walls, 1);
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes(REFUSED), String(texts[0]));
    assert.equal(runner.launches.n, 0);
  });

  it("(d) a newer `now` interrupt inside an awaited declined park keeps the next command fenced", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      (signal) => commandTurn((id) => runner.run(id), "toolu_d", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => {
        if (++parks === 1) { s.pause("now"); s.setMode(null); return false; }
        return true;
      },
    })));
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes(REFUSED), String(texts[0]));
    assert.equal(runner.launches.n, 0);
  });

  it("(e) a consumed pause is re-fenced once a later interrupt drops", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        texts.push(await runner.run("toolu_e1"));
        s.pause("now");
        await hang(signal);
      })(),
      (signal) => commandTurn((id) => runner.run(id), "toolu_e2", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => {
        parks++;
        if (parks === 2) s.pauseInterrupt(); // dropped: the second pause's trip is still held
        if (parks <= 2) { s.setMode(null); return false; }
        return true;
      },
    })));
    assert.equal(texts.length, 2, String(texts));
    assert.ok(texts[0]!.includes(LAUNCHED), String(texts[0]));
    assert.ok(texts[1]!.includes(REFUSED), String(texts[1]));
    assert.equal(runner.launches.n, 1);
  });

  it("(f) a credential-switch interrupt inside an awaited declined park keeps the next command fenced", async () => {
    const s = steering();
    const texts: string[] = [];
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      (signal) => commandTurn((id) => runner.run(id), "toolu_f", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => { s.switchCredential(); s.setMode(null); return false; },
    })));
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes(REFUSED), String(texts[0]));
    assert.equal(runner.launches.n, 0);
  });

  it("(g) a credential switch after the pause trip but before the park keeps the next command fenced", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        s.pause("now");
        await hang(signal);
        s.switchCredential(); // lands while the aborted turn drains, before parkForPause
      })(),
      (signal) => commandTurn((id) => runner.run(id), "toolu_g", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => { parks++; s.setMode(null); return false; },
    })));
    assert.equal(parks, 1);
    assert.equal(texts.length, 1);
    assert.ok(texts[0]!.includes(REFUSED), String(texts[0]));
    assert.equal(runner.launches.n, 0);
  });

  it("a pause dropped during a switch attempt re-fences a consumed pause before the give-up continue", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let switches = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      // eslint-disable-next-line require-yield
      (signal) => (async function* () {
        texts.push(await runner.run("toolu_r1"));
        s.switchCredential(); // the controller is spent: only the re-armable interrupt trips
        await hang(signal);
      })(),
      (signal) => commandTurn((id) => runner.run(id), "toolu_r2", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await settle(executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      parkForPause: async () => { parks++; s.setMode(null); return false; },
      // A pause interrupt dropped during the attempt (the switch trip is held); no pause mode is
      // set, so no pause handler (and no settle site) runs before the restarted turn.
      attemptCredentialSwitch: async () => { switches++; s.pauseInterrupt(); return "gave_up"; },
    })));
    assert.equal(parks, 1, "only the first pause reached a pause handler");
    assert.equal(switches, 1);
    assert.equal(texts.length, 2, String(texts));
    assert.ok(texts[0]!.includes(LAUNCHED), String(texts[0]));
    assert.ok(texts[1]!.includes(REFUSED), String(texts[1]));
    assert.equal(runner.launches.n, 1);
  });

  it("a signal-only context (no re-armable interrupt wired): a declined pause launches the next command", async () => {
    const s = steering();
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn } = fakeTurns([
      planTurn(),
      actThenHang(() => s.pause("now")),
      (signal) => commandTurn((id) => runner.run(id), "toolu_s", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    const { onPauseNow: _p, onCredentialSwitch: _c, ...signalOnly } = s.ctx;
    await executor.run(makeCtx({
      ...signalOnly, capturedMemory: liveCapture().capture,
      parkForPause: async () => { parks++; s.setMode(null); return false; },
    }));
    assert.equal(parks, 1);
    assert.deepEqual(texts.map((t) => t.includes(LAUNCHED)), [true], String(texts));
    assert.equal(runner.launches.n, 1);
  });

  it("an initially-aborted signal on a pre-approved resume: a declined pause launches the next command", async () => {
    const s = steering();
    // Before run(): the executor sees an already-aborted signal at setup. No pause mode is seeded,
    // so the resume boundary's seeded park stays out of it and the setup trip alone parks.
    s.pause("now");
    s.setMode(null);
    const texts: string[] = [];
    let parks = 0;
    let runner!: ReturnType<typeof guardedRunner>;
    const { queryFn, seen } = fakeTurns([
      (signal) => commandTurn((id) => runner.run(id), "toolu_i", texts, doneMessages())(signal),
    ]);
    const executor = new SdkExecutor(nullLogger(), homeDir, { queryFn });
    runner = guardedRunner(executor);
    await executor.run(makeCtx({
      ...s.ctx, capturedMemory: liveCapture().capture,
      planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      parkForPause: async () => { parks++; s.setMode(null); return false; },
    }));
    assert.equal(parks, 1);
    assert.equal(seen.length, 1, "the tripped turn never reached the provider");
    assert.deepEqual(texts.map((t) => t.includes(LAUNCHED)), [true], String(texts));
    assert.equal(runner.launches.n, 1);
  });
});
