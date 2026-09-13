// PRD #1287 — Claude adapter clause contributions (C2, D5).
//
// These rows replace the C1 placeholder seeds with the REAL per-clause Claude preservation
// coverage. Every row is layer "U" (unit/adapter), executed by an additive test under
// agent/test/*.test.ts that runs in the ordinary `npm test`. Each `tests` entry MUST match a
// `test()` title in those files EXACTLY — that is the completeness checker's zero-test-match
// contract (the evidence is wired from the npm test run at C5; agent/test files do NOT import
// this tree's evidence.ts).
//
// Family names match the PRD "Required clause inventory": Advice ceiling / Isolation and
// compatibility / Roles and phases / Workflow signals. Each row names the real production seam
// it exercises, a reachable positive control, and the independent negative-effect oracle
// (denial text alone is insufficient per D4). No existing Claude behavior is changed — these
// ADD discriminating coverage (D5).

import type { ClauseRow } from "./clause.js";

export const CLAUDE_CLAUSES: ClauseRow[] = [
  // --- Advice ceiling (D5: advice denies shell/files/network/delegation/run-signal/credential;
  //     legitimate text results preserved; no run workspace or handler registry) -------------
  {
    id: "claude-c2-advice-ceiling-categories",
    adapter: "claude",
    layer: "U",
    family: "Advice ceiling",
    seam: "agent/src/claude-advice-harness.ts:buildDenyAllHook + ClaudeAdviceHarness.run",
    positiveControl:
      "a legitimate advice text/analysis turn returns its accumulated text and a success terminal",
    negativeOracle:
      "the deny-all PreToolUse hook returns permissionDecision:'deny' for EACH forbidden family "
      + "(shell/Bash, file read+write, network/WebFetch, delegation/Agent, run-signal submit_plan+"
      + "signal_done, credential access), and the advice options wire NO mcp handler registry, so no "
      + "forbidden effect can execute",
    intendedOutcome:
      "advice stays tool-less behind its deny-all hook; forbidden advice effects never occur while "
      + "legitimate text results preserve current semantics",
    tests: [
      "claude C2 advice ceiling: deny-all hook denies a shell/Bash tool with no side effect",
      "claude C2 advice ceiling: deny-all hook denies filesystem read and write tools",
      "claude C2 advice ceiling: deny-all hook denies a network/WebFetch tool",
      "claude C2 advice ceiling: deny-all hook denies a delegation/Agent tool",
      "claude C2 advice ceiling: deny-all hook denies run/worker signal tools submit_plan and signal_done",
      "claude C2 advice ceiling: deny-all hook denies credential access",
      "claude C2 advice ceiling: a legitimate text result is preserved as a positive control",
    ],
  },
  {
    id: "claude-c2-advice-ceiling-no-registry",
    adapter: "claude",
    layer: "U",
    family: "Advice ceiling",
    seam: "agent/src/claude-advice-harness.ts:ClaudeAdviceHarness options + agent/src/harness.ts:AdviceRequest",
    positiveControl:
      "the run lane wires the signal/findings mcp servers + subagent roster; the advice lane is the "
      + "contrast — those are absent",
    negativeOracle:
      "the assembled advice SdkOptions carry mcpServers===undefined and agents===undefined (no run "
      + "workspace or handler registry), and AdviceRequest exposes no cwd/agents/mcpServers/registry "
      + "(runtime shape assertion + a compile-time type guard checked by tsc)",
    intendedOutcome:
      "advice receives no run workspace or handler registry, at both the value and type level",
    tests: [
      "claude C2 advice ceiling: advice options carry no mcpServers handler registry",
      "claude C2 advice ceiling: AdviceRequest exposes no cwd/agents/mcpServers/registry",
    ],
  },

  // --- Isolation and compatibility (equivalent-invariant mappings for Codex-only wire ops,
  //     plus unchanged Claude reducer/projection) --------------------------------------------
  {
    id: "claude-c2-map-shell-boundary",
    adapter: "claude",
    layer: "U",
    family: "Isolation and compatibility",
    seam: "agent/src/guardrails.ts:screenBashCommand+buildPreToolUseHook; agent/src/claude-advice-harness.ts:buildDenyAllHook",
    positiveControl:
      "a harmless run-lane Bash command (`ls -la src`, `echo hello`) reaches its effect (no deny)",
    negativeOracle:
      "MAPPING Codex exec_command/unified_exec/write_stdin -> Claude has no native shell and no "
      + "retained interpreter: buildPreToolUseHook denies a hostile `git push`, screenBashCommand "
      + "re-screens EVERY command statelessly so a later `/proc` read is still denied, and the advice "
      + "deny-all hook denies any shell tool (no invented write_stdin hook)",
    intendedOutcome:
      "no native shell or retained writable terminal exists on the Claude path; the boundary is the "
      + "stateless Bash screen (run lane) plus the deny-all hook (advice lane)",
    tests: [
      "claude C2 mapping shell-boundary: run-lane buildPreToolUseHook denies a hostile Bash command and allows a harmless one",
      "claude C2 mapping shell-boundary: each Bash command is re-screened (no retained interpreter) so a later /proc read is denied",
      "claude C2 mapping shell-boundary: the advice deny-all hook denies a Bash shell tool",
    ],
  },
  {
    id: "claude-c2-map-repo-trust",
    adapter: "claude",
    layer: "U",
    family: "Isolation and compatibility",
    seam: "agent/src/claude-harness.ts:buildSdkOptions (settingSources:[]); agent/src/claude-advice-harness.ts (settingSources:[])",
    positiveControl:
      "the assembled options are otherwise usable (permissionMode bypassPermissions), proving the "
      + "empty settingSources is a deliberate isolation literal, not an unconstructed options object",
    negativeOracle:
      "MAPPING Codex repo-trust (AGENTS.md/.codex config) -> Claude settingSources: []: both the run "
      + "lane (buildSdkOptions) and the advice lane (ClaudeAdviceHarness) emit the literal [] , so a "
      + "cloned repo's .claude/ can grant NO permissions",
    intendedOutcome:
      "untrusted repo content cannot install instructions, callbacks or execution authority on either "
      + "Claude lane; the settingSources literal is unchanged",
    tests: [
      "claude C2 mapping repo-trust: run-lane buildSdkOptions emits literal settingSources: []",
      "claude C2 mapping repo-trust: advice-lane ClaudeAdviceHarness emits literal settingSources: []",
    ],
  },
  {
    id: "claude-c2-reducer-projection",
    adapter: "claude",
    layer: "U",
    family: "Isolation and compatibility",
    seam: "agent/src/harness-reducer.ts:RunTurnReducerImpl (accept/foldSignals/finish)",
    positiveControl:
      "a main-origin frame's lead text accumulates into finalText and a real terminal projects a "
      + "result status message (the reducer still folds and projects normal turns)",
    negativeOracle:
      "an empty turn projects only init+terminal and leaves done=false/finalText=undefined; an "
      + "activity-only/garbage turn adds no messages; and the origin gate holds — a subagent-origin "
      + "frame's signals are NOT folded while a main-origin frame's are (done/plan unchanged vs latched)",
    intendedOutcome:
      "Claude reducer/projection and fallback behavior are unchanged; the main-thread signal gate and "
      + "lead/subagent attribution discriminate exactly as today",
    tests: [
      "claude C2 reducer: an empty turn projects only its terminal and leaves done=false, finalText undefined",
      "claude C2 reducer: a garbage/activity-only turn adds no messages and preserves the fallback result",
      "claude C2 reducer: signals from a subagent-origin frame are not folded",
      "claude C2 reducer: signals from a main-origin frame are folded once",
      "claude C2 reducer: lead text becomes finalText while subagent text only sets subagentActivity",
    ],
  },

  // --- Roles and phases (immutable registry decides; unknown/unregistered identity fails closed) --
  {
    id: "claude-c2-map-unknown-identity",
    adapter: "claude",
    layer: "U",
    family: "Roles and phases",
    seam: "agent/src/guardrails.ts:buildAgentGuardHook",
    positiveControl:
      "an ASSEMBLED subagent (`coder`) is allowed and, when backgrounded, rewritten to run "
      + "synchronously in-turn (run_in_background:false); an already-synchronous one passes through",
    negativeOracle:
      "MAPPING Codex unknown/unregistered identity -> Claude buildAgentGuardHook: a subagent_type not "
      + "in the assembled map (the SDK built-in `general-purpose`, or a spoofed `attacker-role`) is "
      + "denied with permissionDecision:'deny' — NOT a vacuous unknown-tool test, this is a real "
      + "production allow-list seam",
    intendedOutcome:
      "the immutable assembled-subagent registry decides; unknown/spoofed identities fail closed while "
      + "valid allocated ones still run synchronously",
    tests: [
      "claude C2 mapping unknown-identity: buildAgentGuardHook denies an unassembled subagent_type",
      "claude C2 mapping unknown-identity: buildAgentGuardHook denies a spoofed/unregistered role name",
      "claude C2 mapping unknown-identity: buildAgentGuardHook allows an assembled subagent and forces synchronous execution",
      "claude C2 mapping unknown-identity: buildAgentGuardHook leaves an already-synchronous assembled subagent unchanged",
    ],
  },

  // --- Workflow signals (main-thread-only gating of submit_plan/signal_done) ------------------
  {
    id: "claude-c2-map-workflow-signals",
    adapter: "claude",
    layer: "U",
    family: "Workflow signals",
    seam: "agent/src/signals.ts:scanSignals + isSubagentFrame (main-thread-only gating)",
    positiveControl:
      "a MAIN-thread submit_plan scans to exactly {plan} and a main-thread signal_done scans to "
      + "exactly {done:true} (valid root signals change the intended state)",
    negativeOracle:
      "MAPPING Codex child/stale-origin workflow signals -> Claude signals.ts main-thread-only gating: "
      + "a submit_plan carried on a subagent frame (parent_tool_use_id set) and a signal_done carried "
      + "on a subagent frame (subagent_type set) each scan to {} — no plan/done latched",
    intendedOutcome:
      "no unauthorized handler invocation or reducer transition from a child/unknown-origin signal; a "
      + "valid root signal changes the intended state exactly once",
    tests: [
      "claude C2 mapping workflow-signals: a submit_plan from a subagent frame is ignored",
      "claude C2 mapping workflow-signals: a signal_done from a subagent frame is ignored",
      "claude C2 mapping workflow-signals: a main-thread submit_plan is honored exactly once",
      "claude C2 mapping workflow-signals: a main-thread signal_done is honored",
    ],
  },
];
