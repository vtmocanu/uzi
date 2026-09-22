// PRD #1287 C4 (D7 layer U) — Codex failure-closure conformance, the explicit two-part split.
//
// (a) Upstream hook characterization is DISTINCT from production policy and is test-only:
//     production Codex hooks are DISABLED. This drives the REAL production + loopback config
//     builders (config.ts) and asserts hooks + every native execution feature are off — so an
//     upstream hook serialization/spawn/timeout/malformed-output failure can NEVER become
//     production permission. The upstream *characterization* itself lives in the M0 opt-in suite
//     (e2e/codex-m0/hooks-stdin.test.mjs, harness-errors.test.mjs); it is REFERENCED here (its
//     files exist), NOT re-run in this gate.
//
// (b) Production policy/handler/transport failure closure: drives the REAL CodexCallbackBroker
//     with a THROWING and an UNWIRED (malformed) worker-tool handler and a REJECTING command
//     spawn (a callback/transport failure), and asserts fail-closed — no effect, no fabricated
//     successful completion, no authorized action (D4: spawn/fileop counters 0, deny codes, the
//     callback settled ERROR).

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import path from "node:path";

import { loadPackagedConfig, type ConfigModule } from "./packaged-modules.js";
import { loadUModules, makeUBroker, leadGrants, type UModules } from "./harness-u.js";
import { recordEvidence } from "./evidence.js";
import type { ToolHandler } from "../../agent/src/codex/broker.js";
import {
  CODEX_U_FAILURE_HOOKS_DISABLED_TITLE,
  CODEX_U_FAILURE_BROKER_CLOSURE_TITLE,
} from "./titles-c4.js";

let config: ConfigModule;
let umods: UModules;
before(async () => {
  config = await loadPackagedConfig();
  umods = await loadUModules();
});

/** The native EXECUTION surfaces that must be pinned off in every production/loopback config, in
 *  BOTH the default/advice and the run-lane posture. `code_mode_host` is deliberately NOT in this
 *  list: it is the authority-free callback-routing execution HOST (off on the advice/stock lane,
 *  enabled on the run lane), asserted posture-by-posture below — not an always-off native-execution
 *  grant. */
const NATIVE_FEATURES = [
  "shell_tool",
  "unified_exec",
  "code_mode",
  "code_mode_only",
  "code_mode_prewarm",
  "apply_patch_freeform",
  "shell_snapshot",
  "shell_snapshot_v2",
  "multi_agent",
  "multi_agent_v2",
  "plugins",
  "apps",
  "remote_models",
];

/** Split a config.toml TEXT into sections keyed by table header ("" = the root table,
 *  before the first [header]). Only bare `key = value` lines are collected per section —
 *  enough to assert a key lands in its EFFECTIVE table, without a full TOML parser. */
function tomlSections(toml: string): Map<string, string[]> {
  const sections = new Map<string, string[]>([["", []]]);
  let current = "";
  for (const raw of toml.split("\n")) {
    const line = raw.trim();
    if (line.startsWith("[") && line.endsWith("]")) {
      current = line.slice(1, -1);
      if (!sections.has(current)) sections.set(current, []);
      continue;
    }
    if (line.length > 0) sections.get(current)!.push(line);
  }
  return sections;
}

describe("codex U failure closure (a): production hooks are DISABLED (config.ts)", () => {
  it(CODEX_U_FAILURE_HOOKS_DISABLED_TITLE, () => {
    const opts = { model: "gpt-6-astra", projectPath: "/work/repo" } as const;
    const prodApi = config.buildCodexProductionConfigToml({ ...opts, authMode: "api_key" });
    const prodSub = config.buildCodexProductionConfigToml({ ...opts, authMode: "subscription" });
    const loopback = config.buildCodexLoopbackTestConfigToml({ ...opts, authMode: "subscription" }, "http://127.0.0.1:5599/v1");

    // NEGATIVE oracle: hooks are OFF and every native EXECUTION feature is OFF in EVERY builder, with
    // code_mode_host the single posture-dependent exception — the authority-free callback-routing host
    // is OFF on the DEFAULT/advice lane here (asserted per-builder below) and enabled ONLY on the run
    // lane (the second pass further down). An upstream hook failure therefore has nothing to become —
    // production ships no hook surface, and the run-lane host is not a native-execution grant.
    for (const [label, toml] of [["prod api_key", prodApi], ["prod subscription", prodSub], ["loopback", loopback]] as const) {
      const sections = tomlSections(toml);
      const root = sections.get("") ?? [];
      const features = sections.get("features") ?? [];
      const projectsKey = [...sections.keys()].find((k) => k.startsWith("projects."));
      const projects = projectsKey !== undefined ? sections.get(projectsKey)! : [];

      // Global hook-trust invariants (never a per-thread/CLI bypass anywhere in the text).
      assert.doesNotMatch(toml, /hooks = true/, `${label}: hooks are NEVER enabled`);
      assert.ok(!toml.includes("bypass_hook_trust"), `${label}: no per-thread hook-trust bypass`);
      assert.ok(!toml.includes("dangerously-bypass-hook-trust"), `${label}: no CLI hook-trust bypass`);

      // EFFECTIVE-TABLE oracle (a key moved to an ineffective table now fails):
      // hooks + every native execution feature are disabled UNDER [features].
      assert.ok(features.includes("hooks = false"), `${label}: [features].hooks = false`);
      for (const feature of NATIVE_FEATURES) {
        assert.ok(features.includes(`${feature} = false`), `${label}: [features].${feature} = false (effective table)`);
      }
      // DEFAULT/advice posture: the callback-routing host is OFF on the default (advice/stock) lane.
      assert.match(toml, /^code_mode_host = false$/m, `${label}: default builder pins code_mode_host = false (advice/stock lane)`);
      // repo docs are not ingested — project_doc_max_bytes is a ROOT key, not a nested one.
      assert.ok(root.includes("project_doc_max_bytes = 0"), `${label}: project_doc_max_bytes = 0 is a ROOT key`);
      // the canonical project is untrusted under its own [projects."<path>"] table.
      assert.equal(projectsKey, `projects.${JSON.stringify(opts.projectPath)}`, `${label}: the [projects."<path>"] table exists`);
      assert.ok(projects.includes(`trust_level = "untrusted"`), `${label}: [projects."${opts.projectPath}"].trust_level = "untrusted"`);
    }

    // RUN-LANE posture: the SAME production + loopback builders, driven with the trusted
    // launcher-fixed codeModeHost:true, enable ONLY the authority-free code-mode execution host
    // (code_mode_host = true). hooks stay off and EVERY OTHER native execution feature stays off, so
    // the run-lane host is a callback-routing surface, never a native-execution grant.
    const prodApiRun = config.buildCodexProductionConfigToml({ ...opts, authMode: "api_key", codeModeHost: true });
    const prodSubRun = config.buildCodexProductionConfigToml({ ...opts, authMode: "subscription", codeModeHost: true });
    const loopbackRun = config.buildCodexLoopbackTestConfigToml({ ...opts, authMode: "subscription", codeModeHost: true }, "http://127.0.0.1:5599/v1");
    for (const [label, toml] of [["prod api_key (run lane)", prodApiRun], ["prod subscription (run lane)", prodSubRun], ["loopback (run lane)", loopbackRun]] as const) {
      const features = tomlSections(toml).get("features") ?? [];
      // The run lane flips ONLY code_mode_host on (line-anchored) …
      assert.match(toml, /^code_mode_host = true$/m, `${label}: the run lane enables code_mode_host = true`);
      // … while hooks stay off and no hook-trust bypass appears …
      assert.ok(features.includes("hooks = false"), `${label}: hooks stay off on the run lane`);
      assert.doesNotMatch(toml, /hooks = true/, `${label}: hooks are NEVER enabled on the run lane`);
      assert.ok(!toml.includes("bypass_hook_trust") && !toml.includes("dangerously-bypass-hook-trust"), `${label}: no hook-trust bypass on the run lane`);
      // … and EVERY OTHER native execution feature stays off (code_mode/code_mode_only/prewarm too).
      for (const feature of NATIVE_FEATURES) {
        assert.ok(features.includes(`${feature} = false`), `${label}: [features].${feature} = false stays off on the run lane`);
      }
    }

    // POSITIVE control: the builders are reachable and DO emit their intended authenticated
    // surface — production keeps the built-in openai provider (no override block), while the
    // loopback TEST builder wires a distinct authenticated fake provider. So the disabled hooks
    // are a real deny within a working config, not an empty/broken build.
    assert.ok(!prodApi.includes("[model_providers"), "production keeps the built-in openai provider (no override block)");
    assert.match(loopback, /\[model_providers/, "the loopback builder wires an authenticated fake provider");
    assert.match(loopback, /requires_openai_auth = true/, "the loopback provider preserves account/login auth");

    // The loopback builder is fail-closed against exfil: a non-loopback base URL is rejected.
    assert.throws(
      () => config.buildCodexLoopbackTestConfigToml({ ...opts, authMode: "subscription" }, "https://api.evil.example/v1"),
      "a non-loopback provider URL cannot redirect credentials off host",
    );

    // REFERENCE (not re-run): the upstream hook characterization suite exists and is the M0
    // test-only process; it is opt-in and outside this gate (production hooks being off is the
    // production-side proof asserted above).
    const m0 = path.resolve(process.cwd(), "../e2e/codex-m0");
    assert.ok(existsSync(path.join(m0, "hooks-stdin.test.mjs")), "the M0 hook-stdin characterization exists (referenced, not re-run)");
    assert.ok(existsSync(path.join(m0, "harness-errors.test.mjs")), "the M0 hook-error characterization exists (referenced, not re-run)");

    recordEvidence(CODEX_U_FAILURE_HOOKS_DISABLED_TITLE, "pass");
  });
});

describe("codex U failure closure (b): production broker/handler/transport fail closed", () => {
  it(CODEX_U_FAILURE_BROKER_CLOSURE_TITLE, async () => {
    // (i) a THROWING worker-tool handler fails closed: handler_error, NO effect, NO fabricated
    // success. The callback settles ERROR — a post-settlement replay returns replayed_error.
    const throwing = new Map<string, ToolHandler>([["mcp__memory__store", async () => { throw new Error("policy boom"); }]]);
    const hThrow = makeUBroker(umods, {
      grants: leadGrants({ allowedTools: new Set(["Bash", "mcp__memory__store"]) }),
      toolHandlers: throwing,
    });
    const throwId = { threadId: "th-1", turnId: "tn-1", callId: "mcp-throw" };
    const rThrow = await hThrow.broker.handleToolCall(throwId, "mcp__memory__store", { key: "v" }, "root");
    assert.equal(rThrow.ok, false, "a throwing handler is denied");
    if (!rThrow.ok) assert.equal(rThrow.code, "handler_error");
    assert.equal(hThrow.spawn.calls.length, 0, "a throwing handler reaches no command spawn");
    assert.equal(hThrow.fileop.ops.length, 0, "a throwing handler reaches no fileop");
    const replayThrow = await hThrow.broker.handleToolCall(throwId, "mcp__memory__store", { key: "v" }, "root");
    assert.equal(replayThrow.ok, false, "no fabricated success: the callback settled ERROR");
    if (!replayThrow.ok) assert.equal(replayThrow.code, "replayed_error");
    assert.equal(hThrow.registry.isPoisoned(), false, "an honest handler failure is a bounded deny, not a poison");

    // (ii) an UNWIRED (malformed policy) granted worker-tool handler fails closed: denied_tool,
    // no effect. A granted MCP tool with no concrete handler is a DENY, never a silent success.
    const hUnwired = makeUBroker(umods, {
      grants: leadGrants({ allowedTools: new Set(["mcp__forge__comment"]) }),
      toolHandlers: new Map<string, ToolHandler>(),
    });
    const rUnwired = await hUnwired.broker.handleToolCall(
      { threadId: "th-1", turnId: "tn-1", callId: "mcp-unwired" },
      "mcp__forge__comment",
      {},
      "root",
    );
    assert.equal(rUnwired.ok, false, "an unwired granted tool is denied");
    if (!rUnwired.ok) assert.equal(rUnwired.code, "denied_tool");
    assert.equal(hUnwired.spawn.calls.length, 0);
    assert.equal(hUnwired.fileop.ops.length, 0);

    // (iii) a callback/transport failure: a command-spawn seam that REJECTS (command-root
    // transport down) fails closed with broker_error and NO fabricated successful completion.
    const rejectingSpawn = async (): Promise<never> => { throw new Error("command-root transport down"); };
    const hSpawn = makeUBroker(umods, { grants: leadGrants(), spawnCommand: rejectingSpawn });
    const spawnId = { threadId: "th-1", turnId: "tn-1", callId: "shell-fail" };
    const rSpawn = await hSpawn.broker.handleToolCall(spawnId, "Bash", { command: "echo hi" }, "root");
    assert.equal(rSpawn.ok, false, "a rejecting command spawn is denied");
    if (!rSpawn.ok) assert.equal(rSpawn.code, "broker_error");
    const replaySpawn = await hSpawn.broker.handleToolCall(spawnId, "Bash", { command: "echo hi" }, "root");
    assert.equal(replaySpawn.ok, false, "no fabricated success: the failed callback settled ERROR");
    if (!replaySpawn.ok) assert.equal(replaySpawn.code, "replayed_error");
    assert.equal(hSpawn.registry.isPoisoned(), false, "a transport failure is a bounded deny, not a poison");

    // POSITIVE control: the SAME handler path returns success when the handler behaves — so every
    // fail-closed deny above is a real refusal, not a broken fixture.
    const okHandlers = new Map<string, ToolHandler>([["mcp__memory__store", async () => ({ stored: true })]]);
    const hOk = makeUBroker(umods, {
      grants: leadGrants({ allowedTools: new Set(["mcp__memory__store"]) }),
      toolHandlers: okHandlers,
    });
    const rOk = await hOk.broker.handleToolCall(
      { threadId: "th-1", turnId: "tn-1", callId: "mcp-ok" },
      "mcp__memory__store",
      {},
      "root",
    );
    assert.equal(rOk.ok, true, "a well-behaved handler still succeeds (the fail-closed paths are real denies)");

    recordEvidence(CODEX_U_FAILURE_BROKER_CLOSURE_TITLE, "pass");
  });
});
