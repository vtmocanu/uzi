import assert from "node:assert/strict";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { Probe, callOutput, message, patchTool, tool } from "./harness.mjs";

assert.equal(process.env.M0_MANAGED_ONESHOT, "1", "native isolation tests require the explicit managed-container opt-in");
assert.equal(process.platform, "linux", "managed fixture is container-only; never change host /etc");
assert.equal(await readFile("/etc/codex/requirements.toml", "utf8"),
  await readFile(new URL("./requirements.toml", import.meta.url), "utf8"));

function toolSpecs(body) {
  const specs = [];
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (typeof value.name === "string") specs.push(value);
    for (const child of Object.values(value)) {
      if (Array.isArray(child)) child.forEach(visit);
      else visit(child);
    }
  };
  visit(body.tools);
  for (const item of body.input ?? []) if (item.type === "additional_tools") visit(item.tools);
  return specs;
}

const childPrompt = "M0 native isolation child returns only text";
const parentPrompt = "M0 native isolation parent exercises fixed tools";

for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
  for (const disabled of [false, true]) {
    test(`${model} native environment and agents disabled=${disabled}`, async (t) => {
      let childRequests = 0;
      let rootId;
      const p = await Probe.create(t, {
        model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
        multiAgent: false, ...(disabled ? { environments: [], agentsEnabled: false } : {}),
        respond: (body, probe) => {
          assert.equal(body.model, model);
          if (body.client_metadata.thread_id !== rootId) {
            assert.ok(JSON.stringify(body.input).includes(childPrompt), "child request carries its fixed prompt");
            childRequests++;
            probe.events.emit("change");
            return [message("M0 native child completed")];
          }
          assert.ok(JSON.stringify(body.input).includes(parentPrompt));
          const specs = toolSpecs(body);
          const cell = specs.find((value) => value.name === "exec");
          assert.equal(cell?.type, "custom");
          assert.equal(specs.some((value) => value.name === "spawn_agent"), !disabled);
          for (const name of ["exec_command", "apply_patch", "view_image"]) {
            assert.equal(cell.description.includes(`declare const tools: { ${name}(`), !disabled,
              `${name} nested declaration follows environment presence`);
          }
          if (callOutput(body, "nested")) return [message()];
          if (callOutput(body, "spawn")) return [{ type: "custom_tool_call", call_id: "nested", name: "exec", input: `
for (const [name, args] of [
  ["exec_command", { cmd: "printf 'M0 nested shell\\n' > nested-shell", shell: "/bin/sh", login: false, timeout_ms: 1000 }],
  ["apply_patch", ${JSON.stringify(patchTool("unused", "nested-patch").input)}],
  ["view_image", { path: ${JSON.stringify(path.join(probe.workspace, "pixel.png"))} }],
  ["collaboration__spawn_agent", { task_name: "m0_nested", message: ${JSON.stringify(childPrompt)}, fork_turns: "none" }],
]) {
  try {
    const output = await tools[name](args);
    if (name === "view_image") image(output.image_url);
    text({ name, success: true, output: name === "view_image" ? { image: output.image_url?.startsWith("data:") } : output });
  } catch (error) { text({ name, error: error.message }); }
}` }];
          if (callOutput(body, "image")) return [tool("spawn", "spawn_agent", {
            task_name: "m0_direct", message: childPrompt, fork_turns: "none",
          }, "collaboration")];
          if (callOutput(body, "patch")) return [tool("image", "view_image", { path: path.join(probe.workspace, "pixel.png") })];
          if (callOutput(body, "shell")) return [patchTool("patch", "direct-patch")];
          return [tool("shell", "exec_command", {
            cmd: "printf 'M0 direct shell\\n' > direct-shell", shell: "/bin/sh", login: false, timeout_ms: 1000,
          })];
        },
      });
      // A real raster fixture makes the enabled view_image control reach the
      // successful image path instead of merely producing a path/type error.
      await writeFile(path.join(p.workspace, "pixel.png"), Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNgYGAAAAAEAAH2FzhVAAAAAElFTkSuQmCC", "base64"));
      const threadId = await p.thread("never");
      rootId = threadId;
      const turnId = await p.turn(threadId, parentPrompt);
      const terminal = await p.completed(threadId, turnId);
      assert.equal(terminal.params.turn.status, "completed", JSON.stringify({ terminal, errors: p.errors }));
      if (!disabled) {
        await p.until(() => childRequests > 0, "native child actually requests a response");
        const started = await p.until(() => p.messages.find((value) => value.method === "turn/started"
          && value.params.threadId !== threadId), "native child turn starts");
        assert.equal((await p.completed(started.params.threadId, started.params.turn.id)).params.turn.status, "completed");
      }
      const outputs = {};
      for (const id of ["shell", "patch", "image", "spawn", "nested"]) {
        const item = p.requests.map((body) => callOutput(body, id)).find(Boolean);
        assert.ok(item, `${id} forced call returned to the model`);
        outputs[id] = item.output;
      }
      assert.ok(Array.isArray(outputs.nested), "nested code-mode call returns structured text items");
      const nested = outputs.nested.filter((item) => item.type === "input_text" && item.text.startsWith("{"))
        .map((item) => JSON.parse(item.text));
      assert.equal(nested.length, 4, "every forced nested tool reports its outcome");
      t.diagnostic(JSON.stringify({ model, disabled, childRequests, outputs,
        directShell: await p.marker("direct-shell"), nestedShell: await p.marker("nested-shell"),
        directPatch: await p.marker("direct-patch"), nestedPatch: await p.marker("nested-patch") }));
      if (disabled) {
        assert.equal(outputs.shell, "unsupported call: exec_command");
        assert.equal(outputs.patch, "unsupported custom tool call: apply_patch");
        assert.equal(outputs.image, "unsupported call: view_image");
        assert.equal(outputs.spawn, "unsupported call: collaborationspawn_agent");
        assert.deepEqual(nested.map((item) => item.name), ["exec_command", "apply_patch", "view_image", "collaboration__spawn_agent"]);
        for (const item of nested) assert.equal(item.error, "tools[name] is not a function");
        assert.equal(childRequests, 0);
        assert.equal(await p.marker("direct-shell"), null);
        assert.equal(await p.marker("nested-shell"), null);
        assert.equal(await p.marker("direct-patch"), null);
        assert.equal(await p.marker("nested-patch"), null);
        assert.equal(p.threadIds.size, 1);
      } else {
        assert.equal(await p.marker("direct-shell"), "M0 direct shell\n");
        assert.equal(await p.marker("nested-shell"), "M0 nested shell\n");
        assert.equal(await p.marker("direct-patch"), "M0 marker\n");
        assert.equal(await p.marker("nested-patch"), "M0 marker\n");
        assert.ok(Array.isArray(outputs.image) && outputs.image.some((item) => item.type === "input_image"));
        assert.equal(nested.find((item) => item.name === "view_image").output.image, true);
        assert.ok(outputs.nested.some((item) => item.type === "input_image"), "nested view_image returned a real image to the model");
        assert.equal(nested.find((item) => item.name === "collaboration__spawn_agent").error, "tools[name] is not a function",
          "v2 collaboration is direct-only even in the native enabled control");
        assert.equal(childRequests, 1, "native direct spawn creates one child request despite multiAgent=false");
      }
    });
  }

  test(`${model} no-environment cell calculates and rejects runtime I/O globals`, async (t) => {
    const p = await Probe.create(t, {
      model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
      environments: [], agentsEnabled: false,
      respond: (body) => {
        assert.equal(body.model, model);
        return callOutput(body, "isolate") ? [message()] : [{
          type: "custom_tool_call", call_id: "isolate", name: "exec", input: `
text({ sum: 6 * 7, globals: { process: typeof process, require: typeof require, fetch: typeof fetch,
  Deno: typeof Deno, Bun: typeof Bun, XMLHttpRequest: typeof XMLHttpRequest, WebSocket: typeof WebSocket } });
for (const [name, action] of [
  ["module import", () => import("node:fs")],
  ["file read", () => require("node:fs").readFileSync("/etc/passwd", "utf8")],
  ["process environment", () => process.env],
  ["network fetch", () => fetch("http://127.0.0.1:9/m0-isolation-canary")],
]) {
  try { text({ name, unexpected: await action() }); }
  catch (error) { text({ name, rejected: true, error: String(error) }); }
}`,
        }];
      },
    });
    const threadId = await p.thread("never");
    const turnId = await p.turn(threadId);
    assert.equal((await p.completed(threadId, turnId)).params.turn.status, "completed");
    assert.equal(p.requests.length, 2);
    const item = callOutput(p.requests[1], "isolate");
    assert.equal(item?.type, "custom_tool_call_output");
    assert.ok(Array.isArray(item.output));
    const records = item.output.filter((part) => part.type === "input_text" && part.text.startsWith("{"))
      .map((part) => JSON.parse(part.text));
    assert.equal(records.length, 5);
    assert.equal(records[0].sum, 42, "positive calculation proves the cell really executed");
    assert.deepEqual(records[0].globals, { process: "undefined", require: "undefined", fetch: "undefined",
      Deno: "undefined", Bun: "undefined", XMLHttpRequest: "undefined", WebSocket: "undefined" });
    assert.deepEqual(records.slice(1).map((record) => record.name),
      ["module import", "file read", "process environment", "network fetch"]);
    for (const record of records.slice(1)) {
      assert.equal(record.rejected, true);
      assert.ok(record.error.length > 0);
      assert.equal(record.unexpected, undefined);
    }
    assert.equal(p.threadIds.size, 1);
    assert.equal(p.approvals.length, 0);
    assert.equal(p.dynamicCalls.length, 0);
    t.diagnostic(JSON.stringify({ model, records }));
  });
}
