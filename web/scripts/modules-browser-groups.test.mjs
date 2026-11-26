// node --test web/scripts/modules-browser-groups.test.mjs
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { afterEach, beforeEach, describe, it } from "node:test";
import { childGroupRows, createTerminationGate, flushThenExit, launchPhaseProblem, waitForLeader } from "./modules-browser-groups.mjs";

const ps = (...lines) => () => ({ status: 0, stdout: `${lines.join("\n")}\n`, stderr: "" });

describe("childGroupRows", () => {
  it("keeps only direct children that lead their own group", () => {
    const run = ps("  245     1   245", "  300   245   245", "  301   300   245", "  400   300   400", "  401   400   400", "  402   300   245");
    assert.deepEqual(childGroupRows(300, run).map((r) => r.pgid), [400]);
    assert.deepEqual(childGroupRows(300, run), [{ pid: 400, ppid: 300, pgid: 400 }]);
  });

  it("returns nothing for a helper-only listing", () => {
    assert.deepEqual(childGroupRows(300, ps("  301   300   245")), []);
  });

  it("ignores malformed lines", () => {
    assert.deepEqual(childGroupRows(300, ps("", "abc", "1 2", "x 300 400", "  400   300   400")), [{ pid: 400, ppid: 300, pgid: 400 }]);
  });

  it("throws when ps fails", () => {
    assert.throws(() => childGroupRows(300, () => ({ status: 1, stdout: "", stderr: "boom" })), /ps failed: boom/);
    assert.throws(() => childGroupRows(300, () => { throw new Error("spawn"); }));
  });
});

describe("launchPhaseProblem", () => {
  it("accepts a signal while the launch was pending", () => {
    assert.equal(launchPhaseProblem("LAUNCHING ports=1\nSIGNAL launch-pending=true\n"), null);
  });
  it("reports a signal after the launch settled", () => {
    assert.equal(typeof launchPhaseProblem("LAUNCHING ports=1\nSIGNAL launch-pending=false\n"), "string");
  });
  it("reports a missing SIGNAL line", () => {
    assert.equal(typeof launchPhaseProblem("LAUNCHING ports=1\n"), "string");
  });
  it("lets the first SIGNAL line win", () => {
    assert.equal(launchPhaseProblem("SIGNAL launch-pending=true\nSIGNAL launch-pending=false"), null);
  });
});

describe("flushThenExit", () => {
  let savedExitCode;
  beforeEach(() => {
    savedExitCode = process.exitCode;
  });
  afterEach(() => {
    process.exitCode = savedExitCode;
  });

  const fakeStream = (onWrite) => {
    const s = new EventEmitter();
    s.cbs = [];
    s.write = (chunk, cb) => {
      s.cbs.push(cb);
      if (onWrite) onWrite(chunk, cb);
      return true;
    };
    return s;
  };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  it("exits once, only after the write callback", () => {
    const calls = [];
    const stream = fakeStream();
    flushThenExit(143, { stream, deadlineMs: 5000, exit: (c) => calls.push(c) });
    assert.equal(process.exitCode, 143);
    assert.deepEqual(calls, []);
    stream.cbs[0]();
    assert.deepEqual(calls, [143]);
    stream.cbs[0]();
    assert.deepEqual(calls, [143]);
  });

  it("exits after the deadline when the callback never comes, and once even on a late callback", async () => {
    const calls = [];
    const stream = fakeStream();
    flushThenExit(7, { stream, deadlineMs: 20, exit: (c) => calls.push(c) });
    await sleep(80);
    assert.deepEqual(calls, [7]);
    stream.cbs[0]();
    assert.deepEqual(calls, [7]);
  });

  it("exits once when write throws", () => {
    const calls = [];
    const stream = fakeStream(() => {
      throw new Error("closed");
    });
    flushThenExit(1, { stream, deadlineMs: 5000, exit: (c) => calls.push(c) });
    assert.deepEqual(calls, [1]);
  });

  it("swallows a stream error", () => {
    const stream = fakeStream();
    flushThenExit(1, { stream, deadlineMs: 5000, exit: () => {} });
    assert.doesNotThrow(() => stream.emit("error", new Error("EPIPE")));
    stream.cbs[0]();
  });

  it("delivers a large stdout write over a real pipe before exiting", { timeout: 20000 }, async () => {
    const mod = new URL("./modules-browser-groups.mjs", import.meta.url).href;
    const script = `import { flushThenExit } from ${JSON.stringify(mod)};
process.stdout.write("x".repeat(1 << 20));
console.log("MARKER-END");
flushThenExit(143);`;
    const child = spawn(process.execPath, ["--input-type=module", "-e", script], { stdio: ["ignore", "pipe", "inherit"] });
    const chunks = [];
    child.stdout.on("data", (d) => chunks.push(d));
    const code = await new Promise((resolve) => child.once("close", resolve));
    const out = Buffer.concat(chunks).toString();
    assert.equal(code, 143);
    assert.ok(out.length >= 1 << 20);
    assert.ok(out.trimEnd().endsWith("MARKER-END"));
  });
});

describe("waitForLeader", () => {
  // A fake clock: each sleep advances it, so the deadline is reached without real waiting.
  const clock = () => {
    let t = 0;
    return { now: () => t, sleep: async (ms) => void (t += ms) };
  };

  it("returns leader once a group-leading child is seen, not before", async () => {
    const c = clock();
    let polls = 0;
    const r = await waitForLeader({ hasLeader: () => ++polls >= 3, launchFailed: () => false, ...c });
    assert.equal(r, "leader");
    assert.equal(polls, 3);
  });

  it("returns timeout when no leader appears by the deadline (never announces without one)", async () => {
    const c = clock();
    const r = await waitForLeader({ hasLeader: () => false, launchFailed: () => false, deadlineMs: 100, intervalMs: 10, ...c });
    assert.equal(r, "timeout");
  });

  it("reports failed, not leader, when the launch failed although a leader is visible", async () => {
    const r = await waitForLeader({ hasLeader: () => true, launchFailed: () => true, ...clock() });
    assert.equal(r, "failed");
  });

  it("ends the poll as soon as the launch fails mid-poll", async () => {
    const c = clock();
    let polls = 0;
    let failed = false;
    const r = await waitForLeader({
      hasLeader: () => {
        if (++polls === 2) failed = true;
        return false;
      },
      launchFailed: () => failed,
      ...c,
    });
    assert.equal(r, "failed");
    assert.equal(polls, 2);
  });
});

describe("createTerminationGate", () => {
  const settlesWithin = (p, ms) => Promise.race([p.then(() => true), new Promise((r) => setTimeout(() => r(false), ms))]);

  it("lets a spawn through while open", async () => {
    const g = createTerminationGate();
    assert.equal(g.closed, false);
    assert.equal(await settlesWithin(g.beforeSpawn(), 50), true);
  });

  it("parks a spawn after a signal or crash closed it", async () => {
    const g = createTerminationGate();
    g.close();
    assert.equal(g.closed, true);
    assert.equal(await settlesWithin(g.beforeSpawn(), 50), false);
  });
});
