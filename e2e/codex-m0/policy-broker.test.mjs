import assert from "node:assert/strict";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { Probe, callOutput, message, deadline } from "./harness.mjs";
import { FixtureBroker, fixtureTools } from "./policy-broker.mjs";

assert.equal(process.env.M0_MANAGED_ONESHOT, "1");
assert.equal(process.platform, "linux");
assert.equal(await readFile("/etc/codex/requirements.toml", "utf8"),
  await readFile(new URL("./requirements.toml", import.meta.url), "utf8"));
const cell = (id, code) => ({ type: "custom_tool_call", call_id: id, name: "exec", input: code });
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};
const outputText = (body, id) => {
  const output = callOutput(body, id)?.output;
  assert.ok(Array.isArray(output), `actual ${id} cell output`);
  return output.filter((item) => item.type === "input_text").map((item) => item.text).join("\n");
};
async function setup(t, model, respond, options = {}) {
  let broker;
  // Broker drains before Probe removes its workspace. Even failure teardown
  // revokes admission; held fixtures register a release hook before this hook.
  t.after(async () => { if (broker) await deadline(broker.dispose(), "broker callback drain"); });
  const p = await Probe.create(t, {
    model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
    environments: [], agentsEnabled: false, multiAgent: false,
    respond: (body, probe) => { assert.equal(body.model, model); return respond(body, probe); },
    dynamicTool: (request) => broker.handle(request),
  });
  broker = new FixtureBroker(p, options);
  await writeFile(path.join(p.workspace, "policy-source"), "M0 readable source\n");
  return { p, broker };
}
async function run(p, threadId, prompt) {
  const turnId = await p.turn(threadId, prompt);
  assert.equal((await p.completed(threadId, turnId)).params.turn.status, "completed");
  return turnId;
}
function failedItems(p, threadId) {
  return p.messages.filter((value) => value.method === "item/completed"
    && value.params.threadId === threadId && value.params.item.type === "dynamicToolCall"
    && value.params.item.status === "failed");
}

for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
  test(`${model} broker transport root signal and immutable role/phase grants`, async (t) => {
    let rootId;
    const { p, broker } = await setup(t, model, (body) => {
      const lastUser = body.input.filter((item) => item.type === "message" && item.role === "user").at(-1);
      const callId = JSON.stringify(lastUser).includes("M0 implement") ? "policy-implement" : "policy";
      return callOutput(body, callId) ? [message()] : [cell(callId, `
for (const name of ["m0_read", "m0_write", "m0_signal"]) {
  text({ name, output: await tools[name]({ claimedRole: "lead", claimedRoot: true,
    claimedThreadId: ${JSON.stringify(rootId)}, phase: "implement" }) });
}`)];
    });
    rootId = await p.thread("never", fixtureTools);
    broker.register(rootId, { role: "lead", root: true });
    await run(p, rootId);
    assert.equal(await p.marker("policy-signals"), "M0 root signal\n");
    assert.equal(await p.marker("policy-writes"), null);
    const rootOutput = outputText(p.requests.at(-1), "policy");
    assert.match(rootOutput, /M0 readable source/);
    assert.match(rootOutput, /M0 signal recorded/);
    assert.match(rootOutput, /M0 role denied/);
    for (const role of ["coder", "reviewer", "lead"]) {
      const child = await p.thread("never", fixtureTools);
      const identity = { role, root: false, phase: "plan" };
      broker.register(child, identity);
      identity.role = "lead";
      identity.root = true;
      identity.phase = "implement";
      assert.throws(() => broker.register(child, identity));
      await run(p, child);
      const planOutput = outputText(p.requests.at(-1), "policy");
      assert.match(planOutput, /M0 readable source/);
      assert.match(planOutput, role === "coder" ? /M0 plan write denied/ : /M0 role denied/);
      assert.match(planOutput, role === "lead" ? /M0 root required/ : /M0 role denied/);
      assert.equal(await p.marker("policy-writes"), role === "coder" ? null : "M0 coder write\n");
      broker.setPhase(child, "implement");
      // The Responses history retains prior tool outputs, so use a new fixture cell ID.
      await run(p, child, "M0 implement");
      const implementOutput = outputText(p.requests.at(-1), "policy-implement");
      assert.match(implementOutput, role === "coder" ? /M0 write recorded/ : /M0 role denied/);
      assert.ok(failedItems(p, child).length > 0, "denials are actual failed dynamic tool items");
      assert.equal(await p.marker("policy-writes"), "M0 coder write\n");
      assert.equal(await p.marker("policy-signals"), "M0 root signal\n");
    }
    assert.equal(broker.pendingCount, 0);
    t.diagnostic(JSON.stringify({ model, rootSignal: await p.marker("policy-signals"), writes: await p.marker("policy-writes") }));
  });

  test(`${model} broker transport synchronous worker delegation denies unknown and nested roles`, async (t) => {
    const held = deferred();
    t.after(() => held.resolve());
    let rootId;
    const { p, broker } = await setup(t, model, async (body) => {
      const child = body.input.some((item) => item.type === "message" && item.role === "user"
        && JSON.stringify(item.content).includes("M0 child coder"));
      if (child) {
        if (callOutput(body, "child")) { await held.promise; return [message("M0 child completed")]; }
        return [cell("child", `text(await tools.m0_write({})); text(await tools.m0_signal({ claimedRoot: true,
claimedRole: "lead", claimedThreadId: ${JSON.stringify(rootId)} }));
text(await tools.m0_delegate({ role: "coder", claimedRoot: true }));`)];
      }
      return callOutput(body, "delegate") ? [message()] : [cell("delegate", `
text(await tools.m0_delegate({ role: "unknown" }));
text(await tools.m0_delegate({ role: "coder" }));`)];
    });
    rootId = await p.thread("never", fixtureTools);
    broker.register(rootId, { role: "lead", root: true, phase: "implement" });
    const parentTurn = await p.turn(rootId);
    await p.until(() => p.requests.find((body) => callOutput(body, "child")), "child tool results reached model");
    const childId = [...p.threadIds].find((id) => id !== rootId);
    assert.ok(childId);
    const childOutput = outputText(p.requests.find((body) => callOutput(body, "child")), "child");
    assert.match(childOutput, /M0 write recorded/);
    assert.match(childOutput, /M0 role denied/);
    assert.equal(await p.marker("policy-writes"), "M0 coder write\n");
    assert.equal(await p.marker("policy-signals"), null);
    assert.equal(p.threadIds.size, 2, "unknown and nested delegation create no extra thread");
    assert.equal(p.messages.some((event) => event.method === "turn/completed" && event.params.threadId === rootId), false);
    assert.equal(p.requests.some((body) => callOutput(body, "delegate")), false, "parent has no delegation result before child terminal");
    assert.equal(broker.pendingCount, 1, "the parent callback is tracked while awaiting child");
    held.resolve();
    assert.equal((await p.completed(rootId, parentTurn)).params.turn.status, "completed");
    const parentOutput = outputText(p.requests.at(-1), "delegate");
    assert.match(parentOutput, /M0 delegation denied/);
    assert.match(parentOutput, /M0 delegated child completed/);
    const completionIndex = (id) => p.messages.findIndex((event) => event.method === "turn/completed" && event.params.threadId === id);
    assert.ok(completionIndex(childId) >= 0 && completionIndex(childId) < completionIndex(rootId));
    assert.equal(broker.pendingCount, 0);
    t.diagnostic(JSON.stringify({ model, parentOutput, childOutput, ordinaryThreads: p.threadIds.size }));
  });

  for (const mode of ["allow", "revoke", "throw", "invalid"]) {
    test(`${model} broker transport ${mode} held callback settles before disposal`, async (t) => {
      const held = deferred();
      const entered = deferred();
      t.after(() => held.resolve());
      const { p, broker } = await setup(t, model,
        (body) => callOutput(body, "held") ? [message()] : [cell("held", "text(await tools.m0_write({}));")], {
          decide: () => { if (mode === "throw") throw new Error("fixture policy error"); return mode === "invalid" ? "allow" : true; },
          beforeEffect: async () => {
            if (["allow", "revoke"].includes(mode)) { entered.resolve(); await held.promise; }
          },
        });
      const id = await p.thread("never", fixtureTools);
      broker.register(id, { role: "coder", root: true, phase: "implement" });
      const turn = await p.turn(id);
      if (["allow", "revoke"].includes(mode)) {
        await deadline(entered.promise, "callback enters asynchronous hold");
        assert.equal(broker.pendingCount, 1);
        assert.equal(await p.marker("policy-writes"), null);
        let disposed = false;
        const disposing = mode === "revoke" ? broker.dispose().then(() => { disposed = true; }) : null;
        await new Promise((resolve) => setImmediate(resolve));
        assert.equal(disposed, false, "dispose waits for the admitted held callback");
        held.resolve();
        if (disposing) await deadline(disposing, "revoked callback settles");
      }
      assert.equal((await p.completed(id, turn)).params.turn.status, "completed");
      const output = outputText(p.requests.at(-1), "held");
      assert.match(output, { allow: /M0 write recorded/, revoke: /M0 revoked before effect/,
        throw: /M0 policy failed/, invalid: /M0 policy denied/ }[mode]);
      assert.equal(await p.marker("policy-writes"), mode === "allow" ? "M0 coder write\n" : null);
      assert.equal(failedItems(p, id).length, mode === "allow" ? 0 : 1);
      assert.equal(broker.pendingCount, 0);
      t.diagnostic(JSON.stringify({ model, mode, output }));
    });
  }

  test(`${model} broker unit injection rejects unknown/stale/replayed runtime identity`, async (t) => {
    // Real app-server captures a valid live callback; altered callbacks below
    // enter the broker directly. They are NOT fabricated app-server transport evidence.
    const held = deferred();
    const captured = deferred();
    let heldOnce = false;
    t.after(() => held.resolve());
    const { p, broker } = await setup(t, model,
      (body) => callOutput(body, "identity") ? [message()] : [cell("identity", "text(await tools.m0_write({}));")], {
        beforeEffect: async () => {
          if (!heldOnce) { heldOnce = true; captured.resolve(); await held.promise; }
        },
      });
    const unknownId = await p.thread("never", fixtureTools);
    await run(p, unknownId);
    assert.match(outputText(p.requests.at(-1), "identity"), /M0 unknown or stale identity/);
    assert.equal(failedItems(p, unknownId).length, 1, "actual unregistered runtime thread is denied");
    assert.equal(await p.marker("policy-writes"), null);
    const id = await p.thread("never", fixtureTools);
    broker.register(id, { role: "coder", phase: "implement" });
    const turn = await p.turn(id);
    await deadline(captured.promise, "capture real live callback");
    const original = p.dynamicCalls.find((request) => request.params.threadId === id);
    assert.equal(original.params.threadId, id);
    assert.equal(original.params.turnId, turn);
    const inject = (patch, rpcId = "different-rpc-id") => broker.handle({ ...original, id: rpcId,
      params: { ...original.params, ...patch } });
    for (const patch of [{ threadId: "unknown" }, { turnId: "stale" }, { turnId: "" }]) {
      assert.deepEqual((await inject(patch)).result, { success: false, contentItems: [{ type: "inputText", text: "M0 unknown or stale identity" }] });
    }
    assert.match(JSON.stringify(await inject({})), /M0 duplicate callback/);
    assert.match(JSON.stringify(await inject({}, original.id)), /M0 duplicate callback/);
    assert.equal(await p.marker("policy-writes"), null);
    held.resolve();
    assert.equal((await p.completed(id, turn)).params.turn.status, "completed");
    assert.equal(await p.marker("policy-writes"), "M0 coder write\n");
    assert.match(outputText(p.requests.at(-1), "identity"), /M0 write recorded/);
    assert.match(JSON.stringify(await inject({ callId: "new-after-terminal" })), /M0 unknown or stale identity/);
    assert.equal(await p.marker("policy-writes"), "M0 coder write\n");
    assert.equal(broker.pendingCount, 0);
    t.diagnostic(JSON.stringify({ model, layer: "direct broker injection using real captured identity", replayUsesCallId: true }));
  });
}
