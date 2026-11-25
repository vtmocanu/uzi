import { before, test } from "node:test";
import { recordEvidence } from "./evidence.js";
import { P_LAYER_SKIP } from "./p-platform.js";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import path from "node:path";
import { ASYNC_MESSAGE_TITLES, ASYNC_ARGS, ASYNC_QUESTIONS, EXCEPTIONS_TITLE, runtimeInventory } from "./native-exceptions.js";
import { codeModeExecStep, dummyCredential, nestedWorkerCallScript, scriptedStepsResponder, toolCallbackTexts } from "./fake-provider.js";
import { loadProtocolModules, runProtocolTurn, type ProtocolModules } from "./harness-p.js";

let mods: ProtocolModules;
before(async () => {
  if (P_LAYER_SKIP !== false) return;
  mods = await loadProtocolModules();
});

// One bounded local-provider turn per new catalog model, with no worker grants.
for (const [model, title] of Object.entries(ASYNC_MESSAGE_TITLES)) {
  test(title, { skip: P_LAYER_SKIP, timeout: 119_000 }, async (t) => {
    let spawns = 0, fileops = 0;
    const message = "Controlled loopback async message";
    const obs = await runProtocolTurn(mods, {
      model, lifecycle: "start", origin: "root", codeModeHost: true,
      credential: dummyCredential(),
      grants: { role: "coder", phase: "implement", allowedTools: new Set(), allowedSkills: new Set(), isRoot: true },
      respond: scriptedStepsResponder([
        { kind: "call", callId: "native-message", name: "send_message_to_user_async", args: { message } },
        codeModeExecStep("nested-message", 'await tools.send_message_to_user_async({ message: "nested must fail" });'),
        { kind: "finish", text: "controlled turn finished" },
      ]),
      spawnCommand: async () => { spawns++; throw new Error("unexpected worker spawn"); },
      fileop: { op: async () => { fileops++; throw new Error("unexpected worker file effect"); } },
    });
    t.diagnostic(JSON.stringify({ binary: obs.binaryEvidence, elapsedMs: obs.elapsedMs }));
    assert.equal(obs.turnStatus, "completed");
    assert.deepEqual(obs.providerErrors, []);
    assert.ok(obs.providerRequests.length >= 3, "all scripted steps reached the loopback provider");
    for (const request of obs.providerRequests) {
      assert.equal(request.model, model, "actual provider model on every request");
      const inventory = runtimeInventory([request]);
      assert.equal(inventory.observed, true);
      assert.deepEqual(inventory.unparsed, []);
      assert.ok(inventory.structuredNames.includes("functions.send_message_to_user_async"));
      assert.equal(inventory.nestedNames.includes("send_message_to_user_async"), false);
    }
    assert.deepEqual([...new Set(toolCallbackTexts(obs.providerRequests, "native-message"))], ['{"accepted":true}']);
    assert.ok(toolCallbackTexts(obs.providerRequests, "nested-message").some(text =>
      /TypeError/.test(text) && /send_message_to_user_async/.test(text)), "DirectModelOnly helper cannot run inside code mode");
    const messages = obs.notes.filter(note => note.kind === "activity" && note.method === "item/completed")
      .map(note => (note.params as { item?: Record<string, unknown> }).item)
      .filter(item => item?.delivery === "async");
    assert.equal(messages.length, 1);
    assert.equal(messages[0]?.id, "native-message");
    assert.equal(messages[0]?.type, "agentMessage");
    assert.equal(messages[0]?.text, message);
    assert.equal(messages[0]?.phase, "final_answer");
    assert.equal(messages[0]?.questions == null, true);
    assert.deepEqual(obs.callbacks, [], "native message never reaches the worker broker");
    assert.equal(spawns, 0);
    assert.equal(fileops, 0);
    recordEvidence(title, "pass");
  });
}

test(EXCEPTIONS_TITLE, { skip: P_LAYER_SKIP }, async (t) => {
  const scratch = path.resolve(__dirname, "../../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const artifacts = mkdtempSync(path.join(scratch, "1566-evidence-"));
  // Fixed provider choices, one serial turn per lifecycle/model. Never a live provider.
  for (const model of ["gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol"]) {
    for (const lifecycle of ["start", "resume", "child", "advice"] as const) {
      let effects = 0;
      const isAdvice = lifecycle === "advice";
      const beforeRead = Date.now();
      const obs = await runProtocolTurn(mods, {
        model, lifecycle, origin: lifecycle === "child" ? "child" : "root",
        codeModeHost: !isAdvice, credential: dummyCredential(),
        grants: { role: "coder", phase: "implement", allowedTools: new Set(["Bash"]), allowedSkills: new Set(), isRoot: lifecycle !== "child" },
        respond: scriptedStepsResponder([
          { kind: "call", callId: "async", name: "request_user_input_async", args: ASYNC_ARGS },
          { kind: "raw", item: { type: "function_call", call_id: "clock-direct", namespace: "clock", name: "curr_time", arguments: "{}" } },
          ...(!isAdvice ? [
            codeModeExecStep("clock-nested", "text(await tools.clock__curr_time({}));"),
            codeModeExecStep("inventory", "text(ALL_TOOLS);"),
            codeModeExecStep("control", nestedWorkerCallScript("uzi_bash", { command: "echo exception-control" })),
          ] : []),
          { kind: "finish", text: "exception turn finished" },
        ]),
        spawnCommand: async () => { effects += 1; return { code: 0, stdout: "exception-control", stderr: "" }; },
        fileop: { op: async () => { throw new Error("unexpected filesystem effect"); } },
      });
      const afterRead = Date.now();
      const label = `${model}-${lifecycle}`;
      writeFileSync(path.join(artifacts, `${label}.json`), JSON.stringify(obs, null, 2));
      t.diagnostic(`${label} binary=${JSON.stringify(obs.binaryEvidence)} evidence=${artifacts}`);
      assert.equal(obs.turnStatus, "completed");
      assert.deepEqual(obs.providerErrors, []);
      assert.ok(obs.providerRequests.length >= (isAdvice ? 3 : 6), "nonempty provider traffic proves observer is live");
      assert.deepEqual([...new Set(toolCallbackTexts(obs.providerRequests, "async"))], ['{"accepted":true}']);
      const asyncNotes = obs.notes.filter(n => n.kind === "activity" && n.method === "item/completed")
        .map(n => (n.params as { item?: Record<string, unknown> })?.item)
        .filter(item => item?.delivery === "async");
      assert.equal(asyncNotes.length, 1);
      assert.deepEqual(asyncNotes[0]?.questions, ASYNC_QUESTIONS);
      assert.equal(asyncNotes[0]?.type, "agentMessage");
      assert.equal(typeof asyncNotes[0]?.text, "string");
      const utc = [...new Set(toolCallbackTexts(obs.providerRequests, "clock-direct"))];
      assert.equal(utc.length, 1);
      assert.match(utc[0]!, /^It is \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} UTC\.$/);
      const readAt = Date.parse(utc[0]!.slice(6, -5).replace(" ", "T") + "Z");
      assert.ok(Number.isFinite(readAt), "the reported UTC time parses");
      assert.ok(readAt >= beforeRead - 1000 && readAt <= afterRead + 1000,
        "UTC read falls inside the observed turn, allowing second precision");
      // Source injection suggested a client time request; runtime succeeds without one.
      // Time-provider provenance remains UNVERIFIED. Never fake a time response here.
      assert.equal(obs.notes.some(n => n.method === "currentTime/read"), false, "future client time request must turn this characterization RED");
      assert.equal(effects, isAdvice ? 0 : 1, "only the later control callback may reach a worker effect; no answer was supplied");
      assert.deepEqual(obs.callbacks.map(c => c.tool), isAdvice ? [] : ["uzi_bash"]);
      const inventory = runtimeInventory(obs.providerRequests);
      assert.equal(inventory.observed, true);
      assert.deepEqual(inventory.unparsed, []);
      assert.ok(inventory.names.includes("request_user_input_async"), "direct async advertisement is visible");
      assert.match(inventory.descriptions, /clock__curr_time/, "UTC appears in the nested exec declarations");
      if (isAdvice) assert.doesNotMatch(inventory.descriptions, /uzi_bash/, "advice advertises no worker callback");
      if (!isAdvice) {
        assert.ok(obs.callbacks[0]?.result.ok);
        const nested = toolCallbackTexts(obs.providerRequests, "clock-nested");
        assert.ok(nested.some(text => /^{"current_time":"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} UTC"}$/.test(text)));
        assert.ok(toolCallbackTexts(obs.providerRequests, "control").some(text => text.includes("exception-control")));
        assert.match(inventory.descriptions, /uzi_bash/);
        const nestedInventories = [...new Set(toolCallbackTexts(obs.providerRequests, "inventory")
          .filter(text => text.startsWith("[")))];
        assert.equal(nestedInventories.length, 1, "the live code host returned one stable inventory");
        const entries = JSON.parse(nestedInventories[0]!) as { name: string }[];
        assert.ok(Array.isArray(entries));
        const names = entries.map(entry => entry.name);
        assert.ok(names.includes("clock__curr_time") && names.includes("uzi_bash"));
        assert.equal(names.some(name => name === "sleep" || name.endsWith("__sleep")), false);
        assert.equal(names.includes("request_user_input_async"), false, "DirectModelOnly async is not a nested capability");
      }
      t.diagnostic(`${label} async=${JSON.stringify(asyncNotes)} UTC=${JSON.stringify(utc)} workerEffects=${effects}`);
    }
  }
  recordEvidence(EXCEPTIONS_TITLE, "pass");
});
