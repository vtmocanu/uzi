// PRD #1533 (D7 layer P) — RUNTIME real-host validation of the run-path code-mode execution host.
//
// M1 (commit 901bfbda) enabled the Codex code-mode execution host ONLY on the run provider roots
// via the trusted `CodexLaunchSpec.codeModeHost` threaded into config.ts's
// `buildCodexLoopbackTestConfigToml`/`buildCodexProductionConfigToml`. THIS suite proves that
// switch end-to-end against the REAL pinned Codex app-server + REAL config.ts loopback builder,
// through the SAME production launch path the C3 P cases use (harness-p `runProtocolTurn`).
//
// The oracle is BEHAVIORAL and REPRESENTATIVE (D4). For the intended model `gpt-6-astra` the
// app-server advertises ONLY a code-mode `exec` custom tool (each worker dynamic tool folded into
// a NESTED `declare const tools: {...}` inventory inside `exec`'s description), so a real
// schema-compliant model can ONLY reach a worker tool through an `exec` cell whose script body is
// `text(await tools.<wire>({...}))` — never a direct `function_call` (that transport-level shape
// is a bypass a real model never emits, so it would not be representative here).
//
// The sharp #1533 signature, measured live against the real binary:
//   * code_mode_host=true  → the exec cell RUNS, the nested worker callback reaches the broker
//     over `item/tool/call`, the exec output is `"Script completed\n…\nOutput:\n<callback text>"`.
//   * code_mode_host=false → the SAME exec cell fails immediately, the exec output is the literal
//     `"code-mode host is disabled"`, the callback is NEVER reached, and the turn still completes.
// This is exactly the production bug #1533; the paired case below is its fail-on-unfixed regression.
//
// MEASURED startup+turn wall time on this worker: ~1-3s per case (image-baked binary). The suite
// bound is P_SUITE_DEADLINE_MS (90s) and each case is one turn.

import { before, test } from "node:test";
import assert from "node:assert/strict";

import {
  bashArgCanary,
  CODE_MODE_HOST_DISABLED_OUTPUT,
  codeModeExecStep,
  countToolCallbacks,
  dummyCredential,
  nestedWorkerCallScript,
  scriptedStepsResponder,
  toolCallbackTexts,
} from "./fake-provider.js";
import { loadProtocolModules, runProtocolTurn, P_SUITE_DEADLINE_MS, type ProtocolModules } from "./harness-p.js";
import { loadPackagedReducer } from "./packaged-modules.js";
import { P_LAYER_SKIP } from "./p-platform.js";

import type { RunGrants, SpawnCommandSeam, FileopClient } from "../../agent/src/codex/broker.js";
import type { HarnessContext, HarnessContextHook, TurnSignals } from "../../agent/src/harness.js";

/** A no-op context hook so the REAL run-lane reducer can fold a signal in isolation; mirrors the
 *  neutral reducer characterization in e2e/harness-m2/harness-reducer.test.ts. */
class FakeContextHook implements HarnessContextHook {
  request(): void {
    /* the plan fold needs no lead-context read */
  }
  async get(): Promise<HarnessContext | undefined> {
    return undefined;
  }
}

/** The REAL `RunTurnReducerImpl` constructor, loaded from the packaged src (container-safe). */
type ReducerCtor = (typeof import("../../agent/src/harness-reducer.js"))["RunTurnReducerImpl"];

let mods: ProtocolModules;
let Reducer: ReducerCtor;
before(async () => {
  // node:test runs before() even when all tests skip; on a non-Linux host route to the container
  // (P_LAYER_SKIP) and do NO load/resolve work here.
  if (P_LAYER_SKIP !== false) return;
  mods = await loadProtocolModules();
  Reducer = (await loadPackagedReducer()).RunTurnReducerImpl;
});

/** The intended (code-mode-advertising) model whose exec cell folds the worker tools into `tools`. */
const INTENDED_MODEL = "gpt-6-astra";
/** The exec cell's own custom_tool_call id (the exec output comes back under it). */
const EXEC_CALL_ID = "cm-exec";

/** A root coder grant with just the one shell tool (advertised as `uzi_bash` and folded into the
 *  exec cell's nested `tools` inventory). */
function shellGrants(): RunGrants {
  return {
    role: "coder",
    phase: "implement",
    allowedTools: new Set(["Bash"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
  };
}

// ── P0 — run-path callback round trip through the REAL host (code_mode_host=true) ───────────────
test("codex/P run path: a nested exec worker callback reaches the broker through the REAL code-mode host (code_mode_host=true)", { skip: P_LAYER_SKIP }, async (t) => {
  const credential = dummyCredential();
  const bashArg = bashArgCanary();
  const spawnCalls: { argv: readonly string[]; cwd?: string }[] = [];
  const spawnCommand: SpawnCommandSeam = async (argv, opts) => {
    spawnCalls.push({ argv, cwd: opts.cwd });
    return { code: 0, stdout: `${bashArg}\n`, stderr: "" };
  };
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants: shellGrants(),
    credential,
    model: INTENDED_MODEL,
    codeModeHost: true,
    // The representative call: a code-mode exec cell whose body invokes the nested worker tool,
    // then a finish message once the exec output comes back.
    respond: scriptedStepsResponder([
      codeModeExecStep(EXEC_CALL_ID, nestedWorkerCallScript("uzi_bash", { command: `echo ${bashArg}` })),
      { kind: "finish" },
    ]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  const brokerCalls = obs.callbacks.map((c) => c.tool);
  const execOutput = toolCallbackTexts(obs.providerRequests, EXEC_CALL_ID).join("\n");
  t.diagnostic(`brokerCalls=${JSON.stringify(brokerCalls)} execOutput=${JSON.stringify(execOutput)} (${obs.binSource}, ${obs.elapsedMs}ms)`);

  // The nested worker callback reached the broker over item/tool/call THROUGH the real host.
  assert.ok(brokerCalls.includes("uzi_bash"), "the nested exec worker callback reached the broker as uzi_bash");
  const shell = obs.callbacks.find((c) => c.tool === "uzi_bash");
  assert.ok(shell?.result.ok, "the uzi_bash callback authorized + executed (result.ok)");
  // The callback synthesizes an `exec-<uuid>` callId (nested), distinct from the exec cell's own id.
  assert.notEqual(shell?.callId, EXEC_CALL_ID, "the nested callback carries its own synthesized exec callId");

  // Reachability through the host to the command-identity seam.
  assert.equal(spawnCalls.length, 1, "the worker command seam ran exactly once through the host");
  assert.deepEqual(spawnCalls[0]?.argv, ["/bin/sh", "-c", `echo ${bashArg}`], "the broker spawned the exact allowed argv");

  // The exec cell RAN (host enabled) — its output is the code-mode "Script completed" wrapper, and
  // it is emphatically NOT the disabled signature.
  assert.match(execOutput, /Script completed/, "the exec cell ran under the enabled host");
  assert.doesNotMatch(execOutput, new RegExp(CODE_MODE_HOST_DISABLED_OUTPUT), "the enabled host does not report disabled");

  // The tool output flowed back to the model: the exec cell's custom_tool_call_output was fed into
  // a later provider request (so the round trip closed, not merely that a callback fired).
  assert.ok(countToolCallbacks(obs.providerRequests) >= 1, "the exec tool result was delivered back to the model");

  assert.equal(obs.turnStatus, "completed", "the turn completed through the host callback");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");
  assert.ok(obs.elapsedMs < P_SUITE_DEADLINE_MS, `startup+turn ${obs.elapsedMs}ms under the ${P_SUITE_DEADLINE_MS}ms bound`);
});

// ── P0 — the on/off signature regression (code_mode_host=false), pinned to the exact bug ─────────
test("codex/P on-off signature: the SAME exec cell fails 'code-mode host is disabled' with NO worker callback (code_mode_host=false)", { skip: P_LAYER_SKIP }, async (t) => {
  const credential = dummyCredential();
  const bashArg = bashArgCanary();
  const spawnCalls: { argv: readonly string[] }[] = [];
  const spawnCommand: SpawnCommandSeam = async (argv) => {
    spawnCalls.push({ argv });
    return { code: 0, stdout: `${bashArg}\n`, stderr: "" };
  };
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants: shellGrants(),
    credential,
    model: INTENDED_MODEL,
    codeModeHost: false,
    // The IDENTICAL exec cell as the enabled case — only the code_mode_host flag differs.
    respond: scriptedStepsResponder([
      codeModeExecStep(EXEC_CALL_ID, nestedWorkerCallScript("uzi_bash", { command: `echo ${bashArg}` })),
      { kind: "finish" },
    ]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  const brokerCalls = obs.callbacks.map((c) => c.tool);
  const execOutput = toolCallbackTexts(obs.providerRequests, EXEC_CALL_ID).join("\n");
  t.diagnostic(`brokerCalls=${JSON.stringify(brokerCalls)} execOutput=${JSON.stringify(execOutput)} (${obs.binSource}, ${obs.elapsedMs}ms)`);

  // The sharp off-signature: the exec cell fails with the literal disabled string.
  assert.equal(execOutput, CODE_MODE_HOST_DISABLED_OUTPUT, "the disabled host fails the exec cell with the exact literal");

  // The worker callback was NEVER reached — the broker saw zero calls and the command seam never ran.
  assert.deepEqual(brokerCalls, [], "no worker callback reached the broker when the host is disabled");
  assert.equal(spawnCalls.length, 0, "the command seam never ran when the host is disabled");

  // The turn still completes cleanly (the disabled host is a per-cell failure, not a turn failure).
  assert.equal(obs.turnStatus, "completed", "the turn completes cleanly even with the host disabled");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");
});

// ── P1 — a nested exec submit_plan callback is admitted to the broker signal path through the host ─
test("codex/P run path: a nested exec submit_plan callback is admitted to the broker signal path through the host (code_mode_host=true)", { skip: P_LAYER_SKIP }, async (t) => {
  const credential = dummyCredential();
  const planMd = "# Plan\n\n1. Do the thing\n2. Prove it\n";
  // submit_plan is a ROOT-ONLY workflow signal; a plan-phase root coder grant advertises it.
  const grants: RunGrants = {
    role: "coder",
    phase: "plan",
    allowedTools: new Set(["submit_plan"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
  };
  const spawnCommand: SpawnCommandSeam = async () => ({ code: 0, stdout: "", stderr: "" });
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants,
    credential,
    model: INTENDED_MODEL,
    codeModeHost: true,
    respond: scriptedStepsResponder([
      codeModeExecStep(EXEC_CALL_ID, nestedWorkerCallScript("submit_plan", { plan_md: planMd })),
      { kind: "finish" },
    ]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  const brokerCalls = obs.callbacks.map((c) => c.tool);
  const execOutput = toolCallbackTexts(obs.providerRequests, EXEC_CALL_ID).join("\n");
  t.diagnostic(`brokerCalls=${JSON.stringify(brokerCalls)} execOutput=${JSON.stringify(execOutput)} (${obs.binSource}, ${obs.elapsedMs}ms)`);

  // The nested submit_plan callback reached the broker through the host and was admitted…
  assert.ok(brokerCalls.includes("submit_plan"), "the nested exec submit_plan callback reached the broker");
  const submit = obs.callbacks.find((c) => c.tool === "submit_plan");
  assert.ok(submit?.result.ok, "submit_plan was admitted (root-origin signal authorized, not lost)");

  // …and — the point of this case — the admitted signal actually MOVES the run-lane reducer's
  // submitted-plan state, not merely the broker's raw output. Production wraps an accepted root
  // signal as a main-origin frame so `RunTurnReducerImpl.foldSignals` fires
  // (agent/src/codex/codex-harness.ts:876-891); drive that EXACT frame through the REAL reducer
  // here and assert its reduced `result.plan` (agent/src/harness-reducer.ts:154-164,224-237).
  const signals: Readonly<Partial<TurnSignals>> =
    submit && submit.result.ok ? (submit.result.output as Readonly<Partial<TurnSignals>>) : {};
  const reducer = new Reducer(new FakeContextHook());
  reducer.beginTurn();
  await reducer.accept({
    kind: "frame",
    origin: { kind: "main" },
    attribution: {},
    items: [],
    signals,
    model: INTENDED_MODEL,
  });
  const reduced = reducer.finish({ kind: "exhausted" }).result;
  assert.equal(reduced.plan, planMd, "the submitted plan reached the run-lane reducer's submitted-plan state");

  // The reduced signal also flowed back to the model as the exec cell's output.
  assert.match(execOutput, /Script completed/, "the exec cell ran under the enabled host");
  assert.match(execOutput, /"plan"/, "the reduced submit_plan signal flowed back to the model");

  assert.equal(obs.turnStatus, "completed", "the turn completed through the host callback");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");
});
