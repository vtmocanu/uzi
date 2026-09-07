import assert from "node:assert/strict";
import { appendFile, readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { callOutput, message, poll } from "./harness.mjs";
import { SupervisorProbe as Probe } from "./supervisor-probe.mjs";

assert.equal(process.platform, "linux");
assert.equal(await readFile("/etc/codex/requirements.toml", "utf8"), await readFile(new URL("./requirements.toml", import.meta.url), "utf8"));
const delayMs = 500;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const success = (text) => ({ result: { success: true, contentItems: [{ type: "inputText", text }] } });
const failure = (text) => ({ result: { success: false, contentItems: [{ type: "inputText", text }] } });
const dynamicTools = ["hold", "mark"].map((name) => ({ name: `m0_${name}`, description: `M0 harmless ${name} fixture`, inputSchema: { type: "object", properties: {}, additionalProperties: false } }));

async function running(t, model, yielded) {
  let holdStarted = false;
  let settle;
  let rootId;
  const gate = Promise.withResolvers();
  const responseGate = Promise.withResolvers();
  const p = await Probe.create(t, {
    model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
    agentsEnabled: false, environments: [],
    dynamicTool: async (request, probe) => {
      assert.equal(request.params.threadId, rootId);
      assert.equal(request.params.turnId, probe.fixtureTurnId);
      assert.equal(request.params.namespace, null);
      if (request.params.tool === "m0_hold") {
        holdStarted = true;
        await gate.promise;
        // Settlement is a worker-owned completion; revoke wins before release.
        const reply = probe.admitted ? success("M0 hold released") : failure("M0 revoked");
        settle();
        return reply;
      }
      assert.equal(request.params.tool, "m0_mark");
      if (!probe.admitted) return failure("M0 revoked");
      await appendFile(path.join(probe.workspace, "dynamic-marker"), "M0 delayed dynamic marker\n");
      return success("M0 marker recorded");
    },
    respond: async (body) => {
      assert.equal(body.model, model);
      if (callOutput(body, "cell")) {
        if (yielded) await responseGate.promise;
        return [message()];
      }
      return [{ type: "custom_tool_call", name: "exec", call_id: "cell", input:
        `${yielded ? '// @exec: {"yield_time_ms": 1}\n' : ''}await tools.m0_hold({});\nawait new Promise((resolve) => setTimeout(resolve, ${delayMs}));\ntext(await tools.m0_mark({}));` }];
    },
  });
  p.admitted = true;
  p.releaseResponse = responseGate.resolve;
  p.releaseCallback = gate.resolve;
  p.callbackSettled = new Promise((resolve) => { settle = resolve; });
  rootId = await p.thread("never", dynamicTools);
  p.fixtureTurnId = await p.turn(rootId);
  await poll(() => holdStarted, "actual cell entered worker callback");
  const started = p.supervisorMessages.find((value) => value.event === "started");
  assert.ok(started?.subreaper);
  const snap = await p.supervisorCommand("snapshot");
  const app = snap.processes.find((value) => value.pid === started.appServerPid);
  const host = snap.processes.find((value) => value.command.includes("codex-code-mode-host"));
  assert.ok(app, "actual app-server exists below supervisor");
  assert.ok(host, "actual remote code-mode host exists below supervisor");
  assert.notEqual(app.pgid, host.pgid, "actual code host escapes app-server process group");
  if (yielded) {
    await p.until(() => p.requests.some((body) => callOutput(body, "cell")), "yielded cell result reached model");
    assert.match(JSON.stringify(callOutput(p.requests[1], "cell")), /Script running with cell ID/);
  } else {
    assert.equal(p.requests.length, 1, "cell remains active before model receives output");
  }
  t.diagnostic(JSON.stringify({ event: "actual ownership", model, yielded, started, app, host }));
  return { p, rootId, app, host };
}

for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
  for (const yielded of [false, true]) {
    test(`${model} ${yielded ? "yielded" : "active"} uninterrupted delayed dynamic positive control`, async (t) => {
      const { p } = await running(t, model, yielded);
      p.releaseCallback();
      await poll(async () => (await p.marker("dynamic-marker")) !== null, "positive delayed dynamic marker");
      assert.equal(await p.marker("dynamic-marker"), "M0 delayed dynamic marker\n");
      assert.equal(p.dynamicCalls.filter((v) => v.params.tool === "m0_mark").length, 1);
      p.releaseResponse();
      const result = await p.dispose();
      assert.equal(result.state, "drained");
      t.diagnostic(JSON.stringify({ positiveMarker: true, disposal: result }));
    });

    test(`${model} ${yielded ? "yielded" : "active"} revoked settled interrupt dispose suppresses delayed dynamic marker`, async (t) => {
      const { p, rootId, app, host } = await running(t, model, yielded);
      p.admitted = false;
      p.releaseCallback();
      await p.callbackSettled;
      await p.rpc("turn/interrupt", { threadId: rootId, turnId: p.fixtureTurnId });
      const result = await p.dispose();
      assert.equal(result.state, "drained");
      assert.equal(result.authority, "ECHILD+__WALL");
      assert.ok(result.reaped.includes(app.pid), "app-server reaped by own supervisor");
      assert.ok(result.reaped.includes(host.pid), "escaped host adopted and reaped by own supervisor");
      await sleep(delayMs + 200);
      assert.equal(await p.marker("dynamic-marker"), null, "late marker absent beyond actual positive-control delay");
      assert.equal(p.dynamicCalls.filter((v) => v.params.tool === "m0_mark").length, 0,
        "late dynamic callback itself absent, not merely rejected by revoked admission");
      t.diagnostic(JSON.stringify({ markerAbsentAfterMs: delayMs + 200, lateCalls: 0, disposal: result }));
    });
  }
}

test("actual yielded host no-signal disposal is bounded unconfirmed then recovered", async (t) => {
  const { p } = await running(t, "gpt-6-astra", true);
  const start = performance.now();
  const result = await p.dispose(true);
  const elapsedMs = performance.now() - start;
  assert.equal(result.state, "unconfirmed");
  assert.equal(result.reason, "deadline");
  assert.ok(result.children.length > 0);
  assert.ok(elapsedMs >= 40 && elapsedMs < 1000);
  assert.notEqual(p.drained, true, "unconfirmed never promotes to clean");
  const cleanup = await p.dispose();
  assert.equal(cleanup.state, "drained");
  await sleep(delayMs + 200);
  assert.equal(await p.marker("dynamic-marker"), null);
  t.diagnostic(JSON.stringify({ injected: "no signal", elapsedMs, result, cleanup }));
});
