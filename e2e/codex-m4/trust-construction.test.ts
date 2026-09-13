// PRD #1287 C3 (D7 layer U) — repository-trust construction: untrusted repo content can install NO
// instructions, callbacks or execution authority, at START, RESUME and a subsequent TURN.
//
// Two production surfaces decide trust, and NEITHER takes repo input:
//   1. the config.toml builders (config.ts) — a FIXED native-disabled template pinning
//      `project_doc_max_bytes = 0` + an explicit `trust_level = "untrusted"` and every native
//      feature (agents/plugins/apps/shell_tool/code_mode*/unified_exec/hooks/multi_agent) off. They
//      accept only model/projectPath/authMode and REJECT any unknown key, so an AGENTS.md /
//      .codex/agents / .codex/rules / hook / MCP / plugin / app value cannot be smuggled in.
//   2. the CodexHarness thread construction — re-asserting the untrusted config on thread/start AND
//      thread/resume, and an empty `environments: []` on every turn/start, so a resumed root cannot
//      inherit a native execution environment.
//
// The executor leg drives the REAL CodexExecutor over an in-memory transport and inspects the
// thread/start (start), thread/resume (resume) and turn/start (subsequent turn) requests — proving
// the re-assertion happens beyond the first construction, and that no repo canary rides any of them.

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";

import { loadUModules, type UModules } from "./harness-u.js";
import {
  loadExecModules,
  makeExecRig,
  buildExecutor,
  makeCtx,
  withTimeout,
  threadStarted,
  signalDone,
  turnCompleted,
  EXEC_WORKSPACE,
  type ExecModules,
} from "./harness-exec.js";
import { recordEvidence } from "./evidence.js";
import { CODEX_U_TRUST_CONSTRUCTION_TITLE } from "./titles-c3.js";

let mods: UModules;
let execMods: ExecModules;
before(async () => {
  mods = await loadUModules();
  execMods = await loadExecModules();
});

function rec(v: unknown): Record<string, unknown> {
  return (v ?? {}) as Record<string, unknown>;
}

describe("codex U repository trust construction (real config builders + real executor)", () => {
  it(CODEX_U_TRUST_CONSTRUCTION_TITLE, async () => {
    // ── 1. the config.toml builders: a fixed native-disabled untrusted template, no repo input ──
    const projectPath = "/work/repo";
    const production = mods.config.buildCodexProductionConfigToml({ model: "gpt-6-astra", projectPath, authMode: "subscription" });
    const loopback = mods.config.buildCodexLoopbackTestConfigToml({ model: "gpt-6-astra", projectPath, authMode: "api_key" }, "http://127.0.0.1:44444/v1");

    for (const [label, toml] of [["production", production], ["loopback", loopback]] as const) {
      assert.match(toml, /project_doc_max_bytes = 0/, `${label} pins project_doc_max_bytes = 0`);
      assert.match(toml, /\[projects\."\/work\/repo"\]\ntrust_level = "untrusted"/, `${label} pins the project explicitly untrusted`);
      for (const off of ["hooks = false", "code_mode = false", "code_mode_host = false", "unified_exec = false", "shell_tool = false", "apply_patch_freeform = false", "apps = false", "plugins = false"]) {
        assert.ok(toml.includes(off), `${label} disables native feature: ${off}`);
      }
      assert.match(toml, /\[agents\]\nenabled = false/, `${label} disables native agents`);
      // No repo-derived trust promotion or repo-config surface can appear in the fixed template.
      assert.doesNotMatch(toml, /trust_level = "trusted"/, `${label} never promotes trust`);
      assert.doesNotMatch(toml, /AGENTS\.md|\.codex|bypass_hook_trust/, `${label} carries no repo AGENTS.md/.codex/hook-bypass surface`);
    }

    // The builders REJECT any unknown key, so a repo/claim/test cannot smuggle a hook/agent/endpoint
    // field into production construction.
    assert.throws(
      () => mods.config.buildCodexProductionConfigToml({ model: "gpt-6-astra", projectPath, authMode: "subscription", agentsConfig: "x" } as never),
      /unsupported option/,
      "an unknown construction key (a smuggled repo/hook field) is rejected",
    );

    // ── 2. the real executor: untrusted re-asserted on START, RESUME and a subsequent TURN ──
    const untrustedConfig = { project_doc_max_bytes: 0, projects: { [EXEC_WORKSPACE]: { trust_level: "untrusted" } } };

    // START: a fresh run (no sessionId) constructs via thread/start.
    const startRig = makeExecRig(execMods, (c) => {
      if (c.method === "thread/start") return { thread: { id: "th-1" } };
      if (c.method === "turn/start") {
        c.transport.push(threadStarted("th-1")).push(signalDone("th-1", "tn-1")).push(turnCompleted("completed", "th-1", "tn-1"));
        return { turn: { id: "tn-1" } };
      }
      return {};
    });
    await withTimeout(buildExecutor(execMods, startRig).run(makeCtx().ctx), 5000, "start run");
    const start = startRig.transport.requests.find((r) => r.method === "thread/start");
    assert.ok(start, "the fresh run constructed via thread/start");
    assert.deepEqual(rec(start.params).config, untrustedConfig, "thread/start pins the project untrusted with project_doc_max_bytes=0");
    const startTurn = startRig.transport.requests.find((r) => r.method === "turn/start");
    assert.ok(startTurn, "the fresh run issued a turn/start");
    assert.deepEqual(rec(startTurn.params).environments, [], "turn/start reasserts an empty environment selection (no native env inherited)");

    // RESUME + subsequent TURN: a resumed run (sessionId) re-asserts untrusted on thread/resume.
    const resumeRig = makeExecRig(execMods, (c) => {
      if (c.method === "thread/resume") return { thread: { id: "resumed-1" } };
      if (c.method === "thread/start") return { thread: { id: "th-1" } };
      if (c.method === "turn/start") {
        c.transport.push(threadStarted("resumed-1")).push(signalDone("resumed-1", "tn-1")).push(turnCompleted("completed", "resumed-1", "tn-1"));
        return { turn: { id: "tn-1" } };
      }
      return {};
    });
    await withTimeout(buildExecutor(execMods, resumeRig).run(makeCtx({ sessionId: "prior-session" }).ctx), 5000, "resume run");
    const resume = resumeRig.transport.requests.find((r) => r.method === "thread/resume");
    assert.ok(resume, "the resumed run constructed via thread/resume (a subsequent entry, not first construction)");
    assert.deepEqual(rec(resume.params).config, untrustedConfig, "thread/resume RE-asserts the project untrusted with project_doc_max_bytes=0");
    const resumeTurn = resumeRig.transport.requests.find((r) => r.method === "turn/start");
    assert.ok(resumeTurn, "the resumed run issued a subsequent turn/start");
    assert.deepEqual(rec(resumeTurn.params).environments, [], "the subsequent turn/start reasserts an empty environment selection");

    // NEGATIVE ORACLE: no repo AGENTS.md/.codex content or trust promotion rode ANY construction
    // request across both runs (the construction takes no repo input).
    const allRequests = JSON.stringify([...startRig.transport.requests, ...resumeRig.transport.requests]);
    assert.doesNotMatch(allRequests, /AGENTS\.md|\.codex\/|trust_level":"trusted|bypass_hook_trust/, "no repo trust surface rode any construction request");

    recordEvidence(CODEX_U_TRUST_CONSTRUCTION_TITLE, "pass");
  });
});
