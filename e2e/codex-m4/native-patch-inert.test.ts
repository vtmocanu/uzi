import { before, test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { CODEX_ROLE_MODELS } from "../../agent/src/codex/render.js";
import { runtimeInventory } from "./native-exceptions.js";
import { codeModeExecStep, dummyCredential, nativeApplyPatchStep, nestedWorkerCallScript, scriptedStepsResponder, toolCallbackTexts, type ResponsesBody } from "./fake-provider.js";
import { loadProtocolModules, runProtocolTurn, type ProtocolModules } from "./harness-p.js";
import { P_LAYER_SKIP } from "./p-platform.js";
import { recordEvidence } from "./evidence.js";
import { NATIVE_PATCH_INERT_TITLES } from "./titles-c3.js";

let mods: ProtocolModules;
before(async () => { if (P_LAYER_SKIP === false) mods = await loadProtocolModules(); });
const models = (JSON.parse(readFileSync(path.resolve(__dirname, "../../fixtures/codex-models/models.json"), "utf8")) as { curated: string[] }).curated;
const structured = ["functions", "functions.exec", "functions.wait", "functions.request_user_input", "functions.request_user_input_async"];
const nested = ["apply_patch", "list_mcp_resource_templates", "list_mcp_resources", "read_mcp_resource", "uzi_bash", "uzi_apply_patch", "clock__curr_time"];

// The real pinned gpt-5.6-sol catalog omits the DirectModelOnly async helper and nested UTC helper.
function expectedInventory(model: string) {
  return {
    structured: model === "gpt-5.6-sol" ? structured.filter(name => name !== "functions.request_user_input_async")
      : model === "gpt-6.2-sol" || model === "gpt-6.2-astra" ? [...structured, "functions.send_message_to_user_async"] : structured,
    nested: model === "gpt-5.6-sol" ? nested.filter(name => name !== "clock__curr_time") : nested,
  };
}

// Accepted inventory is exact, including missing observations and parser failures.
function assertAcceptedInventory(request: ResponsesBody, model: string): void {
  const inventory = runtimeInventory([request]);
  const expected = expectedInventory(model);
  assert.equal(inventory.observed, true);
  assert.deepEqual(inventory.unparsed, []);
  assert.deepEqual(inventory.structuredNames.sort(), [...expected.structured].sort());
  assert.deepEqual(inventory.nestedNames.sort(), [...expected.nested].sort());
}

for (const model of models) {
  test(`accepted native inventory rejects unknown provider tools: ${model}`, () => {
    const expected = expectedInventory(model);
    const fixture = (unknownStructured = false, unknownNested = false): ResponsesBody => ({
      model,
      tools: [{ type: "namespace", name: "functions", tools: [
        ...expected.structured.filter(name => name !== "functions").map(name => ({
          type: "function", name: name.slice("functions.".length),
          ...(name === "functions.exec" ? { description: `declare const tools: {
${[
            ...expected.nested, ...(unknownNested ? ["unknown_native"] : []),
          ].map(name => `${name}: () => Promise<unknown>;`).join("\n")}
};` } : {}),
        })),
        ...(unknownStructured ? [{ type: "function", name: "unknown_native" }] : []),
      ] }],
    });
    assertAcceptedInventory(fixture(), model);
    assert.throws(() => assertAcceptedInventory(fixture(true), model), /unknown_native/);
    assert.throws(() => assertAcceptedInventory(fixture(false, true), model), /unknown_native/);
    if (model !== "gpt-6.2-sol" && model !== "gpt-6.2-astra") {
      const extraMessage = fixture();
      (extraMessage.tools as { tools: unknown[] }[])[0]!.tools.push({ type: "function", name: "send_message_to_user_async" });
      assert.throws(() => assertAcceptedInventory(extraMessage, model), /send_message_to_user_async/);
    }
  });
}

// Each call-linked outcome is checked before worker controls, with no native MCP servers.
const goals = ["create_goal", "get_goal", "update_goal"] as const;
const utilities = [
  ...goals.map(name => ({ name, args: name === "create_goal" ? { objective: "offline fixture" }
    : name === "update_goal" ? { status: "complete" } : {} })),
  { name: "list_mcp_resources", args: {} },
  { name: "list_mcp_resource_templates", args: {} },
  { name: "read_mcp_resource", args: { server: "offline-fixture", uri: "fixture://missing" } },
];
const directGoalIds = goals.map(name => `direct-${name}`);
const scenarios = ["fresh", "cold", "cold-no-explicit-selection", "fresh-no-launcher-guard"] as const;
assert.deepEqual(Object.keys(NATIVE_PATCH_INERT_TITLES).sort(),
  models.flatMap(model => scenarios.map(scenario => `${model}/${scenario}`)).sort(),
  "native patch evidence titles must exactly match fixture models × scenarios");

for (const model of models) {
  for (const scenario of scenarios) {
    const title = NATIVE_PATCH_INERT_TITLES[`${model}/${scenario}`]!;
    test(title, { skip: P_LAYER_SKIP, timeout: 119_000 }, async (t) => {
      assert.ok(CODEX_ROLE_MODELS.has(model), "curated model accepted by production role renderer");
      const scratch = path.resolve(__dirname, "../../.uzi/scratch");
      mkdirSync(scratch, { recursive: true });
      const artifacts = mkdtempSync(path.join(scratch, "native-patch-evidence-"));
      const marker = path.join(artifacts, "native-marker");
      const patch = `*** Begin Patch\n*** Add File: ${marker}\n+native marker\n*** End Patch`;
      let spawns = 0, fileops = 0;
      const boundaries = new Set<string>();
      const responder = scriptedStepsResponder([
        codeModeExecStep("native-nested", `text(await tools.apply_patch(${JSON.stringify(patch)}));`),
        nativeApplyPatchStep("native-direct", marker),
        ...utilities.map(({ name, args }) => codeModeExecStep(name, `text(await tools.${name}(${JSON.stringify(args)}));`)),
        ...utilities.filter(u => goals.includes(u.name as typeof goals[number])).map(({ name, args }) =>
          ({ kind: "call" as const, callId: `direct-${name}`, name, args })),
        codeModeExecStep("worker-file", nestedWorkerCallScript("uzi_apply_patch", { path: "native-control.txt", content: "worker control" })),
        codeModeExecStep("worker-exec", nestedWorkerCallScript("uzi_bash", { command: "echo native-control" })),
        { kind: "finish" },
      ]);
      const obs = await runProtocolTurn(mods, {
        model, lifecycle: scenario.startsWith("cold") ? "cold" : "start", codeModeHost: true,
        omitLauncherGuardForTest: scenario === "fresh-no-launcher-guard",
        omitAllEnvironmentsForTest: scenario === "cold-no-explicit-selection",
        credential: dummyCredential(),
        grants: { role: "coder", phase: "implement", allowedTools: new Set(["Bash", "apply_patch"]), allowedSkills: new Set(), isRoot: true },
        respond: responder,
        onProviderRequestForTest: (body, callbacks) => {
          for (const id of ["native-nested", "native-direct", ...utilities.map(u => u.name), ...directGoalIds]) {
            const outputs = toolCallbackTexts([body], id);
            if (!outputs.length || boundaries.has(id)) continue;
            assert.equal(existsSync(marker), false, `${id}: no native marker`);
            assert.equal(spawns, 0, `${id}: no spawn before positive controls`);
            assert.equal(fileops, 0, `${id}: no fileop before positive controls`);
            assert.deepEqual(callbacks, [], `${id}: no worker callback`);
            const output = outputs.join("\n");
            if (id.startsWith("native-")) {
              assert.match(output, /No usable execution environment is available/, `${id}: native route refused`);
            } else if (id.startsWith("direct-")) {
              assert.ok(outputs.includes(`unsupported call: ${id.slice("direct-".length)}`), output);
            } else if (goals.includes(id as typeof goals[number])) {
              assert.ok(output.includes(`TypeError: tools.${id} is not a function`), output);
            } else if (id === "read_mcp_resource") {
              assert.ok(outputs.includes("Script error:\nunknown MCP server 'offline-fixture' for the selected environment"), output);
            } else {
              const expected = id === "list_mcp_resources" ? { resources: [] } : { resourceTemplates: [] };
              assert.ok(outputs.includes(JSON.stringify(expected)), `${id}: exact empty JSON result: ${output}`);
              assert.ok(outputs.every(text => !text.includes("Script error:")), output);
            }
            boundaries.add(id);
          }
        },
        spawnCommand: async () => { spawns++; return { code: 0, stdout: "native-control", stderr: "" }; },
        fileop: { op: async request => {
          fileops++;
          assert.equal(request.op, "write");
          assert.equal(request.path, "native-control.txt");
          assert.equal(typeof request.data, "string");
          assert.equal(Buffer.from(request.data!, "base64").toString("utf8"), "worker control");
          return { ok: true };
        } },
      });
      writeFileSync(path.join(artifacts, "observation.json"), JSON.stringify(obs, null, 2));
      t.diagnostic(`evidence=${artifacts} cold=${JSON.stringify(obs.coldRestart)}`);
      assert.deepEqual([...boundaries].sort(), ["native-direct", "native-nested", ...utilities.map(u => u.name), ...directGoalIds].sort());
      assert.equal(existsSync(marker), false);
      assert.equal(obs.turnStatus, "completed");
      assert.deepEqual(obs.providerErrors, []);
      assert.ok(obs.providerRequests.length > 0);
      for (const request of obs.providerRequests) {
        assert.equal(request.model, model);
        assertAcceptedInventory(request, model);
      }
      assert.equal(obs.spawnEnv.CODEX_EXEC_SERVER_URL, scenario === "fresh-no-launcher-guard" ? undefined : "none");
      const requests = [obs.threadParams, obs.turnParams];
      if (scenario.startsWith("cold")) {
        assert.ok(obs.primingTurnParams);
        requests.push(obs.primingTurnParams);
      }
      for (const params of requests) {
        if (scenario === "cold-no-explicit-selection") assert.equal(Object.hasOwn(params, "environments"), false);
        else assert.deepEqual(params.environments, []);
      }
      if (scenario.startsWith("cold")) {
        assert.ok(obs.primingRequests.length > 0);
        assert.ok(obs.coldRestart?.closureConfirmed);
        assert.notEqual(obs.coldRestart.oldPid, obs.coldRestart.newPid);
        assert.equal(obs.coldRestart.transportChanged, true);
        assert.equal(obs.turnParams.threadId, obs.coldRestart.retainedThreadId);
      }
      assert.equal(spawns, 1);
      assert.equal(fileops, 1);
      assert.deepEqual(obs.callbacks.map(c => c.tool), ["uzi_apply_patch", "uzi_bash"]);
      assert.ok(obs.callbacks.every(c => c.result.ok));
      assert.match(toolCallbackTexts(obs.providerRequests, "worker-file").join("\n"), /"written":true/);
      assert.match(toolCallbackTexts(obs.providerRequests, "worker-exec").join("\n"), /native-control/);
      recordEvidence(title, "pass");
    });
  }
}
