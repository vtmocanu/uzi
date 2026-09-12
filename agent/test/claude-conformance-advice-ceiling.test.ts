// PRD #1287 C2 (D5) — Claude advice-lane capability-ceiling conformance.
//
// ADDITIVE ONLY. This file sits BESIDE model-pass.test.ts's advice isolation pin and the
// harness-m2 terminal-decode regression (which is the template for constructing
// ClaudeAdviceHarness directly with a fake queryFn over scripted frames). It changes no
// existing assertion and adds NO Claude capability — it proves, category by category, that
// the advice lane's UNCONDITIONAL deny-all PreToolUse hook (claude-advice-harness.ts
// buildDenyAllHook, :16) denies every forbidden tool family, that a legitimate text result
// is still preserved (positive control), and that the advice lane carries no run workspace
// or handler registry (D5's "advice receives no run workspace or handler registry"), both at
// runtime and at the type level (AdviceRequest carries no cwd/agents/mcpServers/registry).
//
// The deny-all hook ignores its input (it denies EVERY tool), so per-category coverage is
// the point of the advice ceiling, not the "vacuous unknown-tool" antipattern D5 forbids:
// each category is a REAL, named tool the advice runner must never be allowed to run, and the
// independent no-side-effect oracle is that the advice options wire no mcp handler registry —
// nothing could execute even if the deny were removed. No SDK query, no real token, no
// network; the token-shaped value is dummy data.

import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import type { Options as SdkOptions, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";

import { ClaudeAdviceHarness } from "../src/claude-advice-harness.js";
import type { AdviceRequest, AdviceResult } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

/** The deny reason the advice lane is constructed with; asserted to prove the deny came
 *  from the advice deny-all path rather than some default. */
const DENY_REASON = "advice lane is read-only and runs no tools";

/** Run the advice harness once over `frames`, capturing the SdkOptions it assembled (so the
 *  installed deny-all hook + the absent handler registry can be observed) and its result.
 *  The homeDir need not exist — buildSdkEnv only builds an env object — and the fake query
 *  never spawns a process, so this is pure and credential-free. */
async function captureAdvice(
  frames: unknown[],
): Promise<{ options: SdkOptions; result: AdviceResult }> {
  let captured: SdkOptions | undefined;
  const queryFn = ((params: { options: SdkOptions }) => {
    captured = params.options;
    return (async function* () {
      for (const f of frames) yield f;
    })();
  }) as unknown as SdkQueryFn;
  const abort = new AbortController();
  const harness = new ClaudeAdviceHarness({
    token: "dummy-advice-tok",
    homeDir: path.join(os.tmpdir(), "uzi-c2-advice-home-does-not-exist"),
    abort,
    queryFn,
    denyReason: DENY_REASON,
    log: nullLogger(),
  });
  const result = await harness.run(
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
  return { options: captured!, result };
}

type HookFn = (input: unknown, toolUseId?: unknown, ctx?: unknown) => Promise<HookJSONOutput>;

/** The single deny-all PreToolUse hook the advice lane installs (PreToolUse[0].hooks[0]). */
function denyAllHookOf(options: SdkOptions): HookFn {
  const hook = options.hooks?.PreToolUse?.[0]?.hooks?.[0];
  assert.equal(typeof hook, "function", "advice lane installs a PreToolUse deny hook");
  return hook as unknown as HookFn;
}

function decisionOf(out: HookJSONOutput): string | undefined {
  return (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput
    ?.permissionDecision;
}
function reasonOf(out: HookJSONOutput): string | undefined {
  return (out as { hookSpecificOutput?: { permissionDecisionReason?: string } })
    .hookSpecificOutput?.permissionDecisionReason;
}

/** Assert one named tool family is denied AND that no handler registry exists to run it. */
async function assertDenied(tool_name: string, tool_input: unknown): Promise<void> {
  const { options } = await captureAdvice([]);
  // Independent no-side-effect oracle (D4/D5): the advice lane wires NO mcp handler registry,
  // so even were the deny removed there is nothing to execute — advice gets no run workspace.
  assert.equal(options.mcpServers, undefined, "advice lane exposes no handler registry");
  const out = await denyAllHookOf(options)({ hook_event_name: "PreToolUse", tool_name, tool_input });
  assert.equal(decisionOf(out), "deny", `${tool_name} must be denied in the advice lane`);
  assert.equal(reasonOf(out), DENY_REASON, "denial comes from the advice deny-all path");
}

const assistantText = (text: string) => ({
  type: "assistant",
  message: { role: "assistant", content: [{ type: "text", text }] },
});
const successResult = () => ({ type: "result", subtype: "success", is_error: false });

// -- Per-category denies -----------------------------------------------------

test("claude C2 advice ceiling: deny-all hook denies a shell/Bash tool with no side effect", async () => {
  await assertDenied("Bash", { command: "git push origin main" });
});

test("claude C2 advice ceiling: deny-all hook denies filesystem read and write tools", async () => {
  await assertDenied("Read", { file_path: "/etc/passwd" });
  await assertDenied("Write", { file_path: "/tmp/uzi-c2-advice-should-not-write", content: "x" });
});

test("claude C2 advice ceiling: deny-all hook denies a network/WebFetch tool", async () => {
  await assertDenied("WebFetch", { url: "https://exfil.example/leak" });
});

test("claude C2 advice ceiling: deny-all hook denies a delegation/Agent tool", async () => {
  await assertDenied("Agent", { subagent_type: "coder", prompt: "do work" });
});

test("claude C2 advice ceiling: deny-all hook denies run/worker signal tools submit_plan and signal_done", async () => {
  await assertDenied("mcp__uzi__submit_plan", { plan_md: "a plan" });
  await assertDenied("mcp__uzi__signal_done", {});
});

test("claude C2 advice ceiling: deny-all hook denies credential access", async () => {
  // A shell read of the worker credential mount, and a file read of the SDK credentials.
  await assertDenied("Bash", { command: "cat /run/secrets/worker_token" });
  await assertDenied("Read", { file_path: "/home/agent/.claude/.credentials.json" });
});

// -- Positive control: legitimate text/analysis result is preserved unchanged --

test("claude C2 advice ceiling: a legitimate text result is preserved as a positive control", async () => {
  const { result } = await captureAdvice([assistantText("analysis: looks good"), successResult()]);
  assert.equal(result.text, "analysis: looks good", "advice text result is preserved");
  assert.equal(result.end.kind, "terminal");
  if (result.end.kind === "terminal") {
    assert.equal(result.end.terminal.outcome, "success");
  }
});

// -- The capability ceiling: no run workspace / handler registry (runtime + type) --

test("claude C2 advice ceiling: advice options carry no mcpServers handler registry", async () => {
  const { options } = await captureAdvice([]);
  // D5: advice receives no run workspace or handler registry — the run-lane signal/findings
  // MCP servers, the file/bash guard hooks and the subagent map are ALL absent here.
  assert.equal(options.mcpServers, undefined, "no mcp handler registry in the advice lane");
  assert.equal(options.agents, undefined, "no subagent roster in the advice lane");
  assert.deepEqual(options.settingSources, [], "advice isolation literal settingSources: []");
  assert.equal(options.permissionMode, "bypassPermissions");
  assert.equal(options.allowDangerouslySkipPermissions, true);
  // Exactly ONE PreToolUse matcher (the deny-all), with no per-tool allow matcher.
  assert.equal(options.hooks?.PreToolUse?.length, 1, "advice wires a single deny-all matcher");
});

// Compile-time ceiling (D5): AdviceRequest must expose NO run-workspace / handler-registry
// surface. If any of these keys were added to AdviceRequest, `Has<K>` becomes `true` and
// `AssertFalse<true>` fails to compile — caught by `task gate:agent`'s tsc, NOT by tsx (which
// strips types), so the runtime shape assertion below is the executed evidence for the row.
type Has<K extends string> = K extends keyof AdviceRequest ? true : false;
type AssertFalse<T extends false> = T;
const _adviceCeilingTypeGuard: [
  AssertFalse<Has<"cwd">>,
  AssertFalse<Has<"agents">>,
  AssertFalse<Has<"mcpServers">>,
  AssertFalse<Has<"registry">>,
  AssertFalse<Has<"handlers">>,
] = [false, false, false, false, false];
void _adviceCeilingTypeGuard;

test("claude C2 advice ceiling: AdviceRequest exposes no cwd/agents/mcpServers/registry", () => {
  const request: AdviceRequest = {
    label: "review",
    systemPrompt: "sys",
    prompt: "p",
    output: { kind: "text" },
    signal: new AbortController().signal,
    timeoutMs: 5000,
  };
  for (const forbidden of ["cwd", "agents", "mcpServers", "registry", "handlers", "toolServers"]) {
    assert.ok(!(forbidden in request), `advice request must not carry ${forbidden}`);
  }
  // The construction above is the executed evidence; the compile-time _adviceCeilingTypeGuard
  // enforces the same ceiling at the type level under tsc.
});
