import assert from "node:assert/strict";
import test from "node:test";
import { Probe, callOutput, message, poll, tool } from "./harness.mjs";

const childPrompt = "M0 owned child delayed action fixture";
const parentPrompt = "M0 parent completes while owned child waits";

for (const interrupt of [false, true]) {
  test(`child action after parent completion; interrupt=${interrupt}`, async (t) => {
    let releaseChild;
    const childRelease = new Promise((resolve) => { releaseChild = resolve; });
    // Resolve even on assertion failure so a fixture handler cannot hang cleanup.
    t.after(() => releaseChild());
    let childRequest;
    let childRequestCount = 0;
    const p = await Probe.create(t, {
      multiAgent: true,
      respond: async (body, probe) => {
        if (callOutput(body, "control")) return [message("M0 root remains usable")];
        if (JSON.stringify(body.input).includes("M0 after drain control")) {
          return [tool("control", "exec_command", { cmd: "pwd", shell: "/bin/sh", login: false })];
        }
        if (callOutput(body, "spawn")) return [message("M0 parent finished")];
        if (JSON.stringify(body.input).includes(childPrompt)) {
          childRequestCount++;
          if (callOutput(body, "child-marker")) return [message("M0 child finished")];
          childRequest = body;
          probe.events.emit("change");
          await childRelease;
          return [tool("child-marker", "exec_command", {
            cmd: "printf 'M0 child marker\\n' > child-marker", shell: "/bin/sh", login: false,
          })];
        }
        assert.ok(JSON.stringify(body.input).includes(parentPrompt), "fixture request is parent or child");
        return [tool("spawn", "spawn_agent", {
          task_name: "m0_child", message: childPrompt, fork_turns: "none",
        }, "collaboration")];
      },
    });
    const parentId = await p.thread("never");
    const parentTurn = await p.turn(parentId, parentPrompt);
    const parentTerminal = await p.completed(parentId, parentTurn);
    assert.equal(parentTerminal.params.turn.status, "completed");
    await p.until(() => childRequest, "child actually requested a response");
    const childStarted = await p.until(() => p.messages.find((value) =>
      value.method === "turn/started" && value.params.threadId !== parentId), "attributed child turn start");
    const childId = childStarted.params.threadId;
    const childTurn = childStarted.params.turn.id;
    assert.equal(await p.marker("child-marker"), null);
    if (interrupt) {
      await p.rpc("turn/interrupt", { threadId: childId, turnId: childTurn });
      const childTerminal = await p.completed(childId, childTurn);
      assert.equal(childTerminal.params.turn.status, "interrupted");
      await p.until(() => p.requestStates.find((state) => state.body === childRequest)?.closed,
        "interrupted child HTTP request closed");
      releaseChild();
      const controlTurn = await p.turn(parentId, "M0 after drain control");
      const controlTerminal = await p.completed(parentId, controlTurn);
      assert.equal(controlTerminal.params.turn.status, "completed");
      const control = p.requests.map((body) => callOutput(body, "control")).find(Boolean);
      assert.ok(control?.output.includes(p.workspace), "post-drain root command actually executed");
      assert.equal(childRequestCount, 1);
      assert.equal(await p.marker("child-marker"), null);
    } else {
      releaseChild();
      const childTerminal = await p.completed(childId, childTurn);
      assert.equal(childTerminal.params.turn.status, "completed");
      assert.equal(childRequestCount, 2, "child requests again with actual tool output");
      assert.equal(await p.marker("child-marker"), "M0 child marker\n");
      const output = p.requests.map((body) => callOutput(body, "child-marker")).find(Boolean);
      assert.match(output?.output, /Process exited with code 0/);
      assert.ok(p.messages.indexOf(childTerminal) > p.messages.indexOf(parentTerminal));
    }
    t.diagnostic(JSON.stringify({ interrupt, childRequestCount,
      marker: await p.marker("child-marker"), childTurnTerminalObserved: true,
      interpretation: interrupt ? "child turn interruption drained this blocked action" : "actual child action after parent completion" }));
  });
}

for (const interrupt of [false, true]) {
  test(`already-running child shell; interrupt and terminal cleanup=${interrupt}`, async (t) => {
    let releaseChild;
    const childRelease = new Promise((resolve) => { releaseChild = resolve; });
    t.after(() => releaseChild());
    let startupOutput;
    const p = await Probe.create(t, {
      multiAgent: true,
      respond: async (body, probe) => {
        if (callOutput(body, "spawn")) return [message("M0 parent finished")];
        const startup = callOutput(body, "child-delayed");
        if (startup) {
          startupOutput = startup;
          probe.events.emit("change");
          await childRelease;
          return [message("M0 child finished")];
        }
        if (JSON.stringify(body.input).includes(childPrompt)) {
          return [tool("child-delayed", "exec_command", {
            cmd: "printf 'M0 ready\\n' > child-ready; sleep 3; printf 'M0 late marker\\n' > child-late-marker",
            shell: "/bin/sh", login: false, yield_time_ms: 100,
          })];
        }
        assert.ok(JSON.stringify(body.input).includes(parentPrompt));
        return [tool("spawn", "spawn_agent", {
          task_name: "m0_child", message: childPrompt, fork_turns: "none",
        }, "collaboration")];
      },
    });
    const parentId = await p.thread("never");
    const parentTurn = await p.turn(parentId, parentPrompt);
    await p.completed(parentId, parentTurn);
    await p.until(() => startupOutput, "child returned actual running process output");
    const session = /Process running with session ID (\d+)/.exec(startupOutput.output);
    assert.ok(session);
    assert.equal(await p.marker("child-ready"), "M0 ready\n", "child shell actually executed before interruption");
    const childStarted = await p.until(() => p.messages.find((value) =>
      value.method === "turn/started" && value.params.threadId !== parentId), "child turn start");
    const childId = childStarted.params.threadId;
    const childTurn = childStarted.params.turn.id;
    const before = await p.rpc("thread/backgroundTerminals/list", { threadId: childId });
    assert.deepEqual(before.data.map((item) => item.processId), [session[1]]);
    let terminalsAfterInterrupt;
    if (interrupt) {
      await p.rpc("turn/interrupt", { threadId: childId, turnId: childTurn });
      const terminal = await p.completed(childId, childTurn);
      assert.equal(terminal.params.turn.status, "interrupted");
      const after = await p.rpc("thread/backgroundTerminals/list", { threadId: childId });
      assert.deepEqual(after.data.map((item) => item.processId), [session[1]],
        "the original child shell session survives turn interruption before explicit cleanup");
      terminalsAfterInterrupt = await p.terminateTerminals(childId);
      assert.equal(terminalsAfterInterrupt, 1, "explicit cleanup terminates the surviving child shell");
      releaseChild();
      // The command itself waited three seconds; observe beyond that deadline.
      await new Promise((resolve) => setTimeout(resolve, 3500));
      assert.equal(await p.marker("child-late-marker"), null);
    } else {
      releaseChild();
      const terminal = await p.completed(childId, childTurn);
      assert.equal(terminal.params.turn.status, "completed");
      await poll(() => p.marker("child-late-marker"), "allowed delayed child marker", 5000);
      assert.equal(await p.marker("child-late-marker"), "M0 late marker\n");
      await poll(async () => (await p.rpc("thread/backgroundTerminals/list", { threadId: childId })).data.length === 0,
        "allowed child process exited", 5000);
    }
    t.diagnostic(JSON.stringify({ interrupt, terminalsAfterInterrupt,
      readyMarker: await p.marker("child-ready"), lateMarker: await p.marker("child-late-marker"),
      interpretation: "running-process drain includes explicit terminal termination and a post-delay effect check" }));
  });
}
