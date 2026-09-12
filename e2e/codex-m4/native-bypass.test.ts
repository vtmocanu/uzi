// PRD #1287 C3 (D7 layer P) — native-execution-bypass + isolation on the REAL app-server.
//
// Under the SHIPPED production native-disabled template (config.ts, `shell_tool=false`,
// `unified_exec=false`, `code_mode*=false`, `apply_patch_freeform=false`, `hooks=false`, …) and
// NO `/etc/codex`, the model must hold NO native execution authority. The oracle is BEHAVIORAL and
// discriminating (D4): a fake provider that FORCES a native `shell`/`exec_command`/`unified_exec`/
// `write_stdin`/`local_shell` function-call and a native freeform `apply_patch` custom-tool-call
// must produce NO worker callback and NO native side effect (no marker file, no command spawn, no
// fileop), while the intended-model exec — the dynamic `uzi_bash` callback — DOES reach its
// command-identity effect. A reachable native authority here is a BLOCKING defect (D6).
//
// This separates the DYNAMIC WORKER tool name (`uzi_bash`) from every NATIVE name so an allowed
// callback is never mistaken for native execution. Per the PRD "Native execution bypass" clause,
// BOTH halves are checked: (1) the ADVERTISED-SCHEMA absence — the real app-server offers the
// model no native tool schema on any observed provider request (empty is a PASS; see the oracle
// below), and (2) the forced-dispatch RESULT with no forbidden effect. The U layer
// (broker-policy.test.ts) additionally proves the real broker denies a native name `unknown_tool`
// if one ever reached it.
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
import { CODEX_P_NATIVE_ABSENT_TITLE, CODEX_P_ISOLATION_ENV_TITLE } from "./titles-c3.js";

import type { RunGrants, SpawnCommandSeam, FileopClient, FileopRequest } from "../../agent/src/codex/broker.js";

let mods: ProtocolModules;
before(async () => {
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

test(CODEX_P_NATIVE_ABSENT_TITLE, async (t) => {
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
      // Force every native surface, then the intended-model worker exec (positive control), then done.
      respond: scriptedStepsResponder([
        { kind: "call", callId: "n-shell", name: "shell", args: { command: ["/bin/sh", "-c", `touch ${marker("shell")}`] } },
        { kind: "call", callId: "n-exec", name: "exec_command", args: { command: `touch ${marker("exec")}` } },
        { kind: "call", callId: "n-unified", name: "unified_exec", args: { input: `touch ${marker("unified")}` } },
        { kind: "call", callId: "n-write-stdin", name: "write_stdin", args: { data: "x" } },
        { kind: "call", callId: "n-local-shell", name: "local_shell", args: { command: ["/bin/sh", "-c", `touch ${marker("local")}`] } },
        nativeApplyPatchStep("n-patch", "native-marker"),
        { kind: "call", callId: "c-allowed-exec", name: "uzi_bash", args: { command: `echo ${bashArg}` } },
        { kind: "finish" },
      ]),
      spawnCommand,
      fileop,
      turnDeadlineMs: 60_000,
    });

    t.diagnostic(`callbacks=${JSON.stringify(obs.callbacks.map((c) => c.tool))} (source=${obs.binSource}, ${obs.elapsedMs}ms)`);

    // NEGATIVE-EFFECT ORACLE: no native execution touched the filesystem.
    for (const name of ["shell", "exec", "unified", "local"]) {
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

    // ── ADVERTISED-SCHEMA ABSENCE ORACLE ──────────────────────────────────────────
    // The PRD "Native execution bypass" clause requires checking BOTH the advertised schemas
    // (including nested/additional tool lists) AND the failed/unsupported dispatch result. The
    // asserts above are the DISPATCH half (no native call is dispatchable / has any effect); this
    // is the ADVERTISEMENT half — the real app-server must offer the model NO native execution
    // schema. Collect the model-visible `tools` schema the app-server advertised on EVERY observed
    // provider request (the harness's dedicated `advertisedTools` observation is the first
    // request's; scanning all requests catches any nested/additional list on a later turn) and
    // assert none of CODEX_NATIVE_TOOL_NAMES appears.
    //
    // On this production native-disabled path the app-server forwards no `tools` array to the
    // provider (C3 finding: the native-disabled template advertises no native tool to the model,
    // and the dynamic worker tools ride the app-server↔worker channel, not the provider Responses
    // `tools` field). An absent/empty native-tool advertisement SATISFIES the absence property —
    // the model is never offered a native execution schema — and the assertion is scoped over the
    // real observed requests, so it is a truthful absence check, not a vacuous one.
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
    // Anchor the absence to REAL traffic: the app-server issued provider requests, so an empty
    // native-tool advertisement is "the model was never offered a native schema", not "nothing
    // was observed" — this keeps the absence check from silently going vacuous if the harness ever
    // stopped capturing requests.
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

    recordEvidence(CODEX_P_NATIVE_ABSENT_TITLE, "pass");
  } finally {
    rmSync(markerDir, { recursive: true, force: true });
  }
});

test(CODEX_P_ISOLATION_ENV_TITLE, async (t) => {
  const credential = dummyCredential();
  const spawnCommand: SpawnCommandSeam = async () => ({ code: 0, stdout: "", stderr: "" });
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants: leadEffectGrants(),
    credential,
    respond: scriptedStepsResponder([{ kind: "finish" }]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  const env = obs.spawnEnv;
  t.diagnostic(`app-server spawn env keys: ${JSON.stringify(Object.keys(env).sort())}`);
  const envBlob = JSON.stringify(env);

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
  // A bounded allowlist, not the worker's whole environment.
  assert.ok(Object.keys(env).length > 0, "the app-server received a (replaced) env");
  assert.ok(Object.keys(env).length < 40, "the app-server env is a bounded sparse allowlist, not a merged process.env");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");

  recordEvidence(CODEX_P_ISOLATION_ENV_TITLE, "pass");
});
