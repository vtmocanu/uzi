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

/** The native execution surfaces that must be pinned off in every production/loopback config. */
const NATIVE_FEATURES = [
  "shell_tool",
  "unified_exec",
  "code_mode",
  "code_mode_only",
  "code_mode_host",
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

describe("codex U failure closure (a): production hooks are DISABLED (config.ts)", () => {
  it(CODEX_U_FAILURE_HOOKS_DISABLED_TITLE, () => {
    const opts = { model: "gpt-6-astra", projectPath: "/work/repo" } as const;
    const prodApi = config.buildCodexProductionConfigToml({ ...opts, authMode: "api_key" });
    const prodSub = config.buildCodexProductionConfigToml({ ...opts, authMode: "subscription" });
    const loopback = config.buildCodexLoopbackTestConfigToml({ ...opts, authMode: "subscription" }, "http://127.0.0.1:5599/v1");

    // NEGATIVE oracle: hooks are OFF and every native execution feature is OFF, in EVERY builder.
    // An upstream hook failure therefore has nothing to become — production ships no hook surface.
    for (const [label, toml] of [["prod api_key", prodApi], ["prod subscription", prodSub], ["loopback", loopback]] as const) {
      assert.match(toml, /hooks = false/, `${label}: hooks are disabled`);
      assert.doesNotMatch(toml, /hooks = true/, `${label}: hooks are NEVER enabled`);
      assert.ok(!toml.includes("bypass_hook_trust"), `${label}: no per-thread hook-trust bypass`);
      assert.ok(!toml.includes("dangerously-bypass-hook-trust"), `${label}: no CLI hook-trust bypass`);
      for (const feature of NATIVE_FEATURES) {
        assert.match(toml, new RegExp(`${feature} = false`), `${label}: native feature "${feature}" is disabled`);
      }
      assert.match(toml, /project_doc_max_bytes = 0/, `${label}: repo docs are not ingested`);
      assert.match(toml, /trust_level = "untrusted"/, `${label}: the canonical project is untrusted`);
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
