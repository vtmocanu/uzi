// PRD #1287 C3 (D7 layer P) — native-execution-bypass + isolation on the REAL app-server.
//
// Representative top-level native dispatch probes on the real protocol. Supported config
// suppression, explicit environments: [] and the replaced launcher environment deny effects;
// false feature keys alone do not establish runtime suppression at 0.167.0 (pin 0.169.0, not re-measured).
// Forced shell/exec/write_stdin/local_shell and direct native patch calls produce no marker,
// worker callback, command spawn or fileop; the dynamic uzi_bash control reaches the broker.
//
// The legacy inventory oracle below checks representative TOP-LEVEL identities only. It does
// not inspect nested inventories or prove nested apply_patch absent: native apply_patch remains
// advertised under the code-mode host. See native-patch-inert.test.ts for the exact per-model
// inventory matrix and nested/direct patch refusal across fresh and truly cold roots. #1566
// native async questions and read-only UTC remain; registration grants no worker effects.
// P-layer injected OS seams do not prove packaged OS isolation or exhaustive native OS denial.
//
// The isolation leg asserts the real app-server was spawned under the sparse REPLACED env
// (buildReplacedEnv) with no provider credential or worker-token shape (command/provider
// credential separation).

import { before, test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import {
  advertisedToolIdentities,
  bashArgCanary,
  CODEX_NATIVE_TOOL_NAMES,
  dummyCredential,
  nativeApplyPatchStep,
  nativeToolsAdvertised,
  scriptedStepsResponder,
} from "./fake-provider.js";
import { loadProtocolModules, runProtocolTurn, P_SUITE_DEADLINE_MS, type ProtocolModules } from "./harness-p.js";
import { recordEvidence } from "./evidence.js";
import { P_LAYER_SKIP } from "./p-platform.js";
import { CODEX_P_NATIVE_ABSENT_TITLES, NATIVE_BYPASS_MODELS, CODEX_P_ISOLATION_ENV_TITLE } from "./titles-c3.js";

import type { RunGrants, SpawnCommandSeam, FileopClient, FileopRequest } from "../../agent/src/codex/broker.js";

let mods: ProtocolModules;
before(async () => {
  // node:test runs before() even when all tests skip; on a non-Linux host route to the container
  // (P_LAYER_SKIP) and do NO load/resolve work here.
  if (P_LAYER_SKIP !== false) return;
  mods = await loadProtocolModules();
});

/** The lead root grants: the effect tools whose native equivalents would be the bypass surface. */
function leadEffectGrants(): RunGrants {
  return {
    role: "lead",
    phase: "implement",
    allowedTools: new Set(["Bash", "Read", "apply_patch"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
  };
}

/** One contract model per catalog `shell_type` spelling. The 0.160.0 catalog gives gpt-6-astra (and
 *  gpt-5.6-sol) `shell_type: "unified_exec"` but gpt-6-sol and gpt-6.1-sol `shell_type: "shell_command"` (a serde
 *  alias of the same internal type, gated by `shell_tool = false`), so these representative probes
 *  cover both catalog shapes, not only the default model. */
for (const model of NATIVE_BYPASS_MODELS) {
  const title = CODEX_P_NATIVE_ABSENT_TITLES[model]!;
  test(title, { skip: P_LAYER_SKIP }, async (t) => {
    await nativeBypassFor(t, model);
    recordEvidence(title, "pass");
  });
}

async function nativeBypassFor(t: { diagnostic: (message: string) => void }, model: string): Promise<void> {
  const credential = dummyCredential();
  const bashArg = bashArgCanary();

  // Markers OUTSIDE the harness's disposable cwd (which is removed in its finally): each is an
  // ABSOLUTE path a native execution would create. Their absence after the run is the negative
  // oracle for "no native side effect".
  const markerDir = mkdtempSync(path.join(tmpdir(), "codex-m4-native-marker-"));
  const marker = (name: string): string => path.join(markerDir, name);

  const spawnCalls: { argv: readonly string[] }[] = [];
  const spawnCommand: SpawnCommandSeam = async (argv) => {
    spawnCalls.push({ argv });
    return { code: 0, stdout: `${bashArg}\n`, stderr: "" };
  };
  const fileopOps: FileopRequest[] = [];
  const fileop: FileopClient = {
    op: async (request) => {
      fileopOps.push(request);
      return { ok: true };
    },
  };

  try {
    const obs = await runProtocolTurn(mods, {
      grants: leadEffectGrants(),
      credential,
      model,
      effort: "medium",
      // Force the representative native surfaces, then the intended-model worker exec (positive control), then done.
      respond: scriptedStepsResponder([
        { kind: "call", callId: "n-shell", name: "shell", args: { command: ["/bin/sh", "-c", `touch ${marker("shell")}`] } },
        { kind: "call", callId: "n-shell-command", name: "shell_command", args: { command: `touch ${marker("shellcmd")}` } },
        { kind: "call", callId: "n-exec", name: "exec_command", args: { command: `touch ${marker("exec")}` } },
        { kind: "call", callId: "n-unified", name: "unified_exec", args: { input: `touch ${marker("unified")}` } },
        { kind: "call", callId: "n-write-stdin", name: "write_stdin", args: { data: "x" } },
        { kind: "call", callId: "n-local-shell", name: "local_shell", args: { command: ["/bin/sh", "-c", `touch ${marker("local")}`] } },
        nativeApplyPatchStep("n-patch", marker("patch")),
        { kind: "call", callId: "c-allowed-exec", name: "uzi_bash", args: { command: `echo ${bashArg}` } },
        { kind: "finish" },
      ]),
      spawnCommand,
      fileop,
      turnDeadlineMs: 60_000,
    });

    t.diagnostic(`[${model}] callbacks=${JSON.stringify(obs.callbacks.map((c) => c.tool))} (source=${obs.binSource}, ${obs.elapsedMs}ms)`);

    // The iteration really ran THIS model: every observed provider request names it, so a silent
    // fallback to the default model cannot make a later iteration pass on the default's shape.
    assert.ok(obs.providerRequests.length > 0, `[${model}] the fake provider observed real requests`);
    for (const request of obs.providerRequests) {
      assert.equal((request as { reasoning?: { effort?: unknown } }).reasoning?.effort, "medium",
        `[${model}] the real runtime forwards medium reasoning effort to the provider`);
    }
    assert.deepEqual(
      [...new Set(obs.providerRequests.map((body) => (body as { model?: unknown }).model))],
      [model],
      `[${model}] every provider request uses the iterated model`,
    );

    // NEGATIVE-EFFECT ORACLE: no native execution touched the filesystem.
    for (const name of ["shell", "shellcmd", "exec", "unified", "local", "patch"]) {
      assert.equal(existsSync(marker(name)), false, `native execution must not create ${name} marker`);
    }
    // No native call reached the worker command/file surfaces (native is NOT routed as a worker
    // callback, and never executed natively). The ONLY spawn is the intended worker exec.
    assert.equal(spawnCalls.length, 1, "only the intended worker exec reached the command spawn seam");
    assert.deepEqual(fileopOps, [], "no native apply_patch reached the fileop client");

    // The ONLY model-selected callback the broker saw is the dynamic worker exec — every native
    // name produced NO worker callback (so an allowed callback is never mistaken for native).
    const brokerTools = obs.callbacks.map((c) => c.tool);
    assert.deepEqual(brokerTools, ["uzi_bash"], "only the dynamic worker exec produced a worker callback; native names produced none");

    // Representative TOP-LEVEL inventory oracle across observed provider requests.
    // nativeToolsAdvertised does not traverse nested code-mode tools; this assertion does not
    // prove nested apply_patch absence. The exact inventory matrix is native-patch-inert.test.ts.
    const advertised = [
      ...obs.advertisedTools,
      ...obs.providerRequests.flatMap((body) => {
        const tools = (body as { tools?: unknown }).tools;
        return Array.isArray(tools)
          ? tools.filter((tool): tool is Record<string, unknown> => tool !== null && typeof tool === "object")
          : [];
      }),
    ];
    t.diagnostic(
      `advertised tool identities across ${obs.providerRequests.length} provider request(s): `
      + JSON.stringify(advertisedToolIdentities(advertised)),
    );
    // Require observed traffic so the representative top-level check cannot pass on no requests.
    assert.ok(
      obs.providerRequests.length > 0,
      "the fake provider observed real requests (the advertised-schema absence reflects real traffic)",
    );
    assert.deepEqual(
      nativeToolsAdvertised(advertised),
      [],
      `no native tool schema (${CODEX_NATIVE_TOOL_NAMES.slice(0, 3).join("/")}/…) is advertised to the model on the production native-disabled path`,
    );

    // POSITIVE CONTROL: the intended worker exec reached its effect (the native absence is a real
    // deny, not a broken fixture / dead turn).
    const exec = obs.callbacks.find((c) => c.callId === "c-allowed-exec");
    assert.ok(exec?.result.ok, "the intended uzi_bash callback succeeded");
    assert.equal(obs.turnStatus, "completed", "the turn completed through the allowed callback");
    assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");
    assert.ok(obs.elapsedMs < P_SUITE_DEADLINE_MS, `startup+turn ${obs.elapsedMs}ms under the ${P_SUITE_DEADLINE_MS}ms bound`);
  } finally {
    rmSync(markerDir, { recursive: true, force: true });
  }
}

test(CODEX_P_ISOLATION_ENV_TITLE, { skip: P_LAYER_SKIP }, async (t) => {
  const credential = dummyCredential();
  const inheritedCanary = "codex-m4-parent-env-must-not-reach-the-child";
  const spawnCommand: SpawnCommandSeam = async () => ({ code: 0, stdout: "", stderr: "" });
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants: leadEffectGrants(),
    credential,
    respond: scriptedStepsResponder([{ kind: "finish" }]),
    spawnCommand,
    fileop,
    launcherEnv: {
      UZI_UID_SPLIT: "1",
      UZI_M4_ENV_CANARY: inheritedCanary,
    },
    turnDeadlineMs: 60_000,
  });

  const env = obs.spawnEnv;
  t.diagnostic(`app-server spawn env keys: ${JSON.stringify(Object.keys(env).sort())}`);
  const envBlob = JSON.stringify(env);

  // A deterministic parent-env canary proves this is a full replacement, not a merge that happens
  // to omit the credential-shaped names below.
  assert.equal(env.UZI_M4_ENV_CANARY, undefined, "the replaced child env drops a parent-env canary");
  assert.ok(!envBlob.includes(inheritedCanary), "the parent-env canary value is absent");

  // The credential rides the app-server login RPC, NEVER the launcher/app-server env. The sparse
  // REPLACED env must not carry the provider credential (command/provider credential separation).
  assert.doesNotMatch(
    envBlob,
    new RegExp(credential.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
    "the provider credential never rides the app-server spawn env",
  );
  // No inherited worker-token / OAuth shapes (a full replacement, not a merge of process.env).
  assert.doesNotMatch(
    envBlob,
    /CLAUDE_CODE_OAUTH_TOKEN|UZI_WORKER_TOKEN|GITHUB_TOKEN|GITLAB_TOKEN|OPENAI_API_KEY/,
    "no inherited credential-shaped env var",
  );
  // EXACT allowlist (not a merged process.env): buildReplacedEnv emits precisely these keys
  // for an app-server-auth root — HOME/CODEX_HOME/the four XDG_*/TMPDIR plus a fixed
  // PATH/SHELL/LANG/TERM, and NO provider credential var (the credential rides the login RPC).
  const REPLACED_ENV_ALLOWLIST = [
    "HOME", "CODEX_HOME", "CODEX_EXEC_SERVER_URL",
    "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
    "TMPDIR", "PATH", "SHELL", "LANG", "TERM",
  ].sort();
  assert.deepEqual(
    Object.keys(env).sort(),
    REPLACED_ENV_ALLOWLIST,
    "the app-server env is EXACTLY the sparse replaced allowlist, not a merged process.env",
  );
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");

  recordEvidence(CODEX_P_ISOLATION_ENV_TITLE, "pass");
});
