// PRD #1287 C2 (D5) — Codex-specific wire operations mapped to the EQUIVALENT Claude
// construction/hook invariant, each with a written mapping and an executed assertion.
//
// ADDITIVE ONLY. Sits beside guardrails.test.ts, signals.test.ts and the sdk-executor guardrail
// options pin; changes no existing assertion and invents NO Claude protocol field. D5 is explicit:
// "Codex-specific wire concepts map to the equivalent invariant on Claude, not invented Claude
// protocol fields" and "Do not invent a Claude write_stdin hook or claim a denial because the
// fixture passed an unknown tool name." Each mapping below names the Codex concept, the Claude
// equivalent, and the real production seam it asserts against. Pure, credential-free, no network.

import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import type { Options as SdkOptions, HookInput, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";

import { ClaudeHarness, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { ClaudeAdviceHarness } from "../src/claude-advice-harness.js";
import { screenBashCommand, buildPreToolUseHook, buildAgentGuardHook } from "../src/guardrails.js";
import { scanSignals } from "../src/signals.js";
import type { RunTurnRequest } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

function decisionOf(out: HookJSONOutput): string | undefined {
  return (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput
    ?.permissionDecision;
}
function updatedInputOf(out: HookJSONOutput): Record<string, unknown> | undefined {
  return (out as { hookSpecificOutput?: { updatedInput?: Record<string, unknown> } })
    .hookSpecificOutput?.updatedInput;
}

// ---------------------------------------------------------------------------
// MAPPING 1 — Codex exec_command / unified_exec / write_stdin → Claude has NO native shell
// and NO retained/reusable interpreter. The equivalent invariant is (a) the advice lane's
// deny-all hook denies any tool, and (b) the run lane re-screens EVERY Bash command through
// guardrails.screenBashCommand / buildPreToolUseHook — there is no persistent terminal a
// second `write_stdin`-style write could reach, so each command is judged independently.
// (No invented write_stdin hook — the invariant is the absence of a retained interpreter.)
// ---------------------------------------------------------------------------

test("claude C2 mapping shell-boundary: run-lane buildPreToolUseHook denies a hostile Bash command and allows a harmless one", async () => {
  const hook = buildPreToolUseHook(nullLogger());
  const denied = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Bash",
    tool_input: { command: "git push origin main" },
  } as unknown as HookInput);
  assert.equal(decisionOf(denied), "deny", "a hostile git push is denied at the run-lane hook");

  const allowed = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Bash",
    tool_input: { command: "ls -la src" },
  } as unknown as HookInput);
  // A passing command returns no decision (the tool proceeds under bypassPermissions).
  assert.deepEqual(allowed, {}, "a harmless command reaches its effect (no deny)");
});

test("claude C2 mapping shell-boundary: each Bash command is re-screened (no retained interpreter) so a later /proc read is denied", () => {
  // Codex's reusable interpreter + write_stdin has no Claude analog: the screener is stateless,
  // so a first harmless command does not open a session a second hostile write could ride.
  assert.equal(screenBashCommand("echo hello").denied, false, "first harmless command allowed");
  // The very next, independently-screened command that tries to disclose the environment is
  // still denied — there is no retained writable terminal to smuggle it through.
  assert.equal(
    screenBashCommand("cat /proc/1/environ").denied,
    true,
    "a subsequent /proc environment read is denied on its own screening",
  );
});

test("claude C2 mapping shell-boundary: the advice deny-all hook denies a Bash shell tool", async () => {
  // The advice lane has no shell at all; the deny-all hook is the boundary (mirrors the
  // Codex advice lane's isolated calculation — no exec_command equivalent is added to Claude).
  const { options } = await captureAdviceOptions();
  const hook = options.hooks?.PreToolUse?.[0]?.hooks?.[0] as unknown as (
    i: unknown,
  ) => Promise<HookJSONOutput>;
  const out = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Bash",
    tool_input: { command: "sh -c 'id'" },
  });
  assert.equal(decisionOf(out), "deny", "advice lane denies any shell tool");
});

// ---------------------------------------------------------------------------
// MAPPING 2 — Codex repo-trust (AGENTS.md / .codex config / project_doc) → Claude
// `settingSources: []`. The mapping: a cloned repo's `.claude/` can grant NO permissions
// because setting sources are empty. Asserted in BOTH lanes: the run lane (buildSdkOptions,
// via ClaudeHarness) and the advice lane (ClaudeAdviceHarness construction).
// ---------------------------------------------------------------------------

test("claude C2 mapping repo-trust: run-lane buildSdkOptions emits literal settingSources: []", async () => {
  const options = await captureRunLaneOptions();
  assert.deepEqual(
    options.settingSources,
    [],
    "run lane: a cloned repo's .claude cannot grant permissions (settingSources empty)",
  );
  // The run lane is allow-by-default-deny-specific: bypassPermissions + the deny hooks.
  assert.equal(options.permissionMode, "bypassPermissions");
});

test("claude C2 mapping repo-trust: advice-lane ClaudeAdviceHarness emits literal settingSources: []", async () => {
  const { options } = await captureAdviceOptions();
  assert.deepEqual(
    options.settingSources,
    [],
    "advice lane: repo-borne .claude cannot grant permissions (settingSources empty)",
  );
});

// ---------------------------------------------------------------------------
// MAPPING 3 — Codex unknown/unregistered identity → Claude buildAgentGuardHook. This is NOT a
// vacuous unknown-tool test: buildAgentGuardHook is a real production seam that (a) denies a
// subagent_type not in the run's ASSEMBLED map, and (b) forces an assembled one to run
// SYNCHRONOUSLY (run_in_background:false). An unassembled/spoofed identity fails closed;
// a valid allocated one still runs.
// ---------------------------------------------------------------------------

test("claude C2 mapping unknown-identity: buildAgentGuardHook denies an unassembled subagent_type", async () => {
  const hook = buildAgentGuardHook(["coder", "reviewer"], nullLogger());
  const out = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Agent",
    tool_input: { subagent_type: "general-purpose" },
  } as unknown as HookInput);
  assert.equal(decisionOf(out), "deny", "the SDK's built-in general-purpose agent is denied");
});

test("claude C2 mapping unknown-identity: buildAgentGuardHook denies a spoofed/unregistered role name", async () => {
  const hook = buildAgentGuardHook(["coder", "reviewer"], nullLogger());
  const out = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Agent",
    tool_input: { subagent_type: "attacker-role" },
  } as unknown as HookInput);
  assert.equal(decisionOf(out), "deny", "an unregistered role name fails closed");
});

test("claude C2 mapping unknown-identity: buildAgentGuardHook allows an assembled subagent and forces synchronous execution", async () => {
  const hook = buildAgentGuardHook(["coder", "reviewer"], nullLogger());
  const out = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Agent",
    tool_input: { subagent_type: "coder", run_in_background: true },
  } as unknown as HookInput);
  assert.equal(decisionOf(out), undefined, "an assembled subagent is not denied");
  assert.equal(
    updatedInputOf(out)?.["run_in_background"],
    false,
    "an assembled subagent is forced to run synchronously in-turn",
  );
});

test("claude C2 mapping unknown-identity: buildAgentGuardHook leaves an already-synchronous assembled subagent unchanged", async () => {
  const hook = buildAgentGuardHook(["coder", "reviewer"], nullLogger());
  const out = await hook({
    hook_event_name: "PreToolUse",
    tool_name: "Agent",
    tool_input: { subagent_type: "coder", run_in_background: false },
  } as unknown as HookInput);
  assert.deepEqual(out, {}, "an already-synchronous assembled subagent passes through untouched");
});

// ---------------------------------------------------------------------------
// MAPPING 4 — Codex child/stale-origin workflow signals → Claude signals.ts main-thread-only
// gating. A submit_plan/signal_done that arrives on a SUBAGENT frame (child or stale origin)
// is ignored by scanSignals; a main-thread one is honored. This is the load-bearing guarantee
// independent of SDK tool gating.
// ---------------------------------------------------------------------------

function submitPlanFrame(planMd: string, opts: { subagent?: boolean } = {}) {
  const msg: Record<string, unknown> = {
    type: "assistant",
    message: {
      role: "assistant",
      content: [{ type: "tool_use", name: "mcp__uzi__submit_plan", input: { plan_md: planMd } }],
    },
  };
  // parent_tool_use_id marks a child/stale origin (isSubagentFrame).
  if (opts.subagent) msg["parent_tool_use_id"] = "toolu_parent_origin";
  return msg;
}
function signalDoneFrame(opts: { subagent?: boolean } = {}) {
  const msg: Record<string, unknown> = {
    type: "assistant",
    message: {
      role: "assistant",
      content: [{ type: "tool_use", name: "mcp__uzi__signal_done", input: {} }],
    },
  };
  // subagent_type marks a child origin (isSubagentFrame) — the other of the two markers.
  if (opts.subagent) msg["subagent_type"] = "coder";
  return msg;
}

test("claude C2 mapping workflow-signals: a submit_plan from a subagent frame is ignored", () => {
  const scanned = scanSignals(submitPlanFrame("child plan", { subagent: true }));
  assert.deepEqual(scanned, {}, "a child/stale-origin submit_plan latches no plan");
});

test("claude C2 mapping workflow-signals: a signal_done from a subagent frame is ignored", () => {
  const scanned = scanSignals(signalDoneFrame({ subagent: true }));
  assert.deepEqual(scanned, {}, "a child/stale-origin signal_done latches no done");
});

test("claude C2 mapping workflow-signals: a main-thread submit_plan is honored exactly once", () => {
  const scanned = scanSignals(submitPlanFrame("root plan"));
  assert.deepEqual(scanned, { plan: "root plan" }, "a main-thread submit_plan is honored, once");
});

test("claude C2 mapping workflow-signals: a main-thread signal_done is honored", () => {
  const scanned = scanSignals(signalDoneFrame());
  assert.deepEqual(scanned, { done: true }, "a main-thread signal_done latches done");
});

// -- shared capture helpers --------------------------------------------------

/** A minimal ClaudeTurnConfig; only the isolation shape (settingSources) is asserted. */
const runLaneConfig: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-c2-worktree",
  env: {},
  skillsPluginPath: "/tmp/uzi-c2-plugin",
  skills: [],
  systemPrompt: "sys",
  agents: {},
  mcpServers: {},
  preToolUse: [],
};

/** Drive one ClaudeHarness turn with a capturing fake query to observe the SdkOptions that
 *  buildSdkOptions assembled for the RUN lane. The fake query yields no frames, so draining
 *  the turn's events generator just triggers the query construction and returns. */
async function captureRunLaneOptions(): Promise<SdkOptions> {
  let captured: SdkOptions | undefined;
  const queryFn = ((params: { options: SdkOptions }) => {
    captured = params.options;
    return (async function* () {
      /* no frames */
    })();
  }) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn,
    spawn: () => ({ pid: undefined }),
    kill: () => true,
    log: nullLogger(),
    contextUsageTimeoutMs: 2000,
    spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-c2-run-home-does-not-exist"),
  });
  harness.prepareTurn(runLaneConfig, { pid: undefined });
  const request: RunTurnRequest = {
    prompt: "p",
    systemPrompt: "sys",
    signal: new AbortController().signal,
    phase: "plan",
    agents: {},
    leadSkills: [],
  };
  const turn = harness.startTurn(request);
  for await (const _ of turn.events) {
    /* drain (empty) to trigger the capturing query */
  }
  return captured!;
}

/** Capture the ADVICE-lane SdkOptions ClaudeAdviceHarness assembled. */
async function captureAdviceOptions(): Promise<{ options: SdkOptions }> {
  let captured: SdkOptions | undefined;
  const queryFn = ((params: { options: SdkOptions }) => {
    captured = params.options;
    return (async function* () {
      /* no frames */
    })();
  }) as unknown as SdkQueryFn;
  const abort = new AbortController();
  await new ClaudeAdviceHarness({
    token: "dummy-advice-tok",
    homeDir: path.join(os.tmpdir(), "uzi-c2-advice-home-does-not-exist"),
    abort,
    queryFn,
    denyReason: "advice lane is read-only and runs no tools",
    log: nullLogger(),
  }).run(
    {
      label: "review",
      systemPrompt: "sys",
      prompt: "p",
      output: { kind: "text" },
      signal: abort.signal,
      timeoutMs: 5000,
    },
    { onTerminal() {} },
  );
  return { options: captured! };
}
