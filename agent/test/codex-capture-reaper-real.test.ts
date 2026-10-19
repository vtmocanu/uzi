import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { assertClosedReaperRequest } from "../src/closed-reaper.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { runnerCommand, commandRootCommand, RUNNER_UID, WORKER_UID, uidSplitActive } from "../src/runner-uid.js";

const GROUP_B_SKIP = process.platform === "linux" && process.getuid?.() === WORKER_UID && uidSplitActive()
  ? false : "requires the fresh worker UID-split image and real Linux procfs";
function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
function identity(pid: number) {
  const stat = fs.readFileSync("/proc/" + pid + "/stat", "utf8");
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19];
}
async function plant(uid: "runner" | "command", marker: string, key: string) {
  // Literal inert fixture: readiness over stdout; EOF terminates only this owned child.
  const args = ["-e", 'process.stdout.write("ready\\n");process.stdin.resume();process.stdin.on("end",()=>process.exit(0));'];
  const wrapped = (uid === "runner" ? runnerCommand : commandRootCommand)(process.execPath, args);
  const child = spawn(wrapped.command, wrapped.args, { cwd: "/", env: {
    PATH: "/usr/local/bin:/usr/bin:/bin", UZI_RUN_ATTEMPT: marker, UZI_RUN_CLONE_KEY: key,
  }, stdio: ["pipe", "pipe", "pipe"] });
  const closed = once(child, "close");
  child.stderr!.resume();
  await once(child.stdout!, "data");
  assert.ok(child.pid);
  const startTime = identity(child.pid);
  const status = fs.readFileSync("/proc/" + child.pid + "/status", "utf8");
  assert.match(status, uid === "runner" ? /^Uid:\s+10002\s/m : /^Uid:\s+10003\s/m);
  return { child, closed, pid: child.pid, startTime };
}
async function proof(hold: boolean, mode: "capture" | "own" = "capture", report = false) {
  setQuiescenceViewForTests(undefined); // Actual fixed scanner refuses every test view.
  const attempt = newRunAttempt("real-capture", 1, path.join(os.tmpdir(), "capture-real"), () => []);
  const foreign = newRunAttempt("real-foreign", 1, path.join(os.tmpdir(), "capture-foreign"), () => []);
  const live = new LiveAttemptRegistry(); live.add(attempt); live.add(foreign);
  const plants: Array<Awaited<ReturnType<typeof plant>>> = [];
  const reaped = latch(), release = latch(), late = latch();
  const operations: string[] = [], cleanRoots: string[] = [];
  const adapter = boundaryProcessSpawnerForTest("required", async () => {
    throw new Error("EffectLaunch is unreachable for a closed reaper");
  });
  const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
    async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
    async (request, ms, cleanup) => {
      const input = assertClosedReaperRequest(request);
      const command = request.argv[0]!;
      operations.push(command);
      if (command === process.execPath) {
        assert.ok(request.argv.includes("--import"));
        assert.ok(request.argv.some(arg => arg.endsWith("run-quiescence.ts")));
        const scan = JSON.parse(input);
        assert.equal(scan.targetUid, RUNNER_UID);
        assert.equal(scan.mode, mode);
        assert.equal(Object.hasOwn(scan, "reportDeadline"), report, "only genuine reportBudget changes legacy JSON");
        if (report) assert.ok(Number.isSafeInteger(scan.reportDeadline));
        assert.equal(scan.ownMarker, attempt.marker);
        assert.deepEqual(scan.targetPaths, [attempt.clonePath]);
        assert.ok(scan.liveMarkers.includes(foreign.marker));
      } else assert.equal(input, "");
      const root = await adapter(request, ms, cleanup); // Real same-RUNNER_UID supervisor.
      const reap = root.root.reap.bind(root.root);
      root.root.reap = async budget => {
        const result = await reap(budget);
        assert.equal(result.ok, true, "real supervisor ECHILD+__WALL disposal must succeed");
        cleanRoots.push(command);
        if (hold && command === process.execPath) {
          reaped.resolve(); await release.promise; // Inert settlement latch AFTER actual reap.
        }
        return result;
      };
      return root;
    }, undefined, { reaperAttempt: () => attempt });
  const request = { mode, attempt, cloneKey: attempt.cloneKey,
    targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live,
    otherClaimInFlight: true,
    ...(report ? { reportBudget: { deadline: Date.now() + 30000, signal: new AbortController().signal } } : {}) };
  try {
    plants.push(await plant("runner", attempt.marker, attempt.cloneKey));
    plants.push(await plant("runner", foreign.marker, foreign.cloneKey));
    plants.push(await plant("command", attempt.marker, attempt.cloneKey));
    const own = plants[0]!, other = plants[1]!, otherUid = plants[2]!;
    let pending!: ReturnType<typeof quiesceRunAttempt>;
    let continuation!: ReturnType<typeof quiesceRunAttempt>;
    let settled = false;
    const capture = safety.withCredentialFreeCapture(30000, async () => {
      pending = quiesceRunAttempt(request);
      if (hold) {
        continuation = late.promise.then(() => quiesceRunAttempt(request));
        // Early scanner failure must surface instead of hiding behind the settlement latch.
        await Promise.race([reaped.promise, pending.then(verdict => {
          throw new Error("pipeline completed before real scanner reap: " + JSON.stringify(verdict));
        })]); // Ignore pending on success: safety must still join the whole pipeline.
      } else await pending;
    });
    void capture.then(() => { settled = true; }, () => {});
    if (hold) {
      await Promise.race([reaped.promise, capture.then(() => {
        throw new Error("capture completed before real scanner reap");
      })]);
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(settled, false);
      late.resolve();
      assert.equal((await continuation).process?.state, "unverified", "revocation blocks a NEW pipeline");
      release.resolve();
    }
    const result = await capture;
    assert.equal(result.kind, "observed_empty");
    const verdict = (await pending).process!;
    if (report) {
      assert.notEqual(verdict.state, "quiescent", "owned writer blocks report protection");
      assert.deepEqual(verdict.killed, [], "report retirement must kill NOTHING");
      assert.ok(verdict.processes.some(p => p.pid === own.pid), "actual owned writer was observed");
      assert.equal(identity(own.pid), own.startTime, "report preserves the exact owned writer");
    } else {
      assert.equal(verdict.state, "quiescent", JSON.stringify(verdict));
      assert.ok(verdict.killed.includes(own.pid), "same-own PID selected by actual fixed scanner");
      await own.closed;
      assert.equal(own.child.signalCode, "SIGKILL");
      assert.throws(() => identity(own.pid), /ENOENT/, "same PID/starttime is gone before verdict");
    }
    assert.equal(identity(other.pid), other.startTime, "foreign attempt survives with same identity");
    assert.equal(identity(otherUid.pid), otherUid.startTime, "foreign UID survives with same identity");
    assert.deepEqual(operations, ["/bin/mktemp", process.execPath, "/bin/rm"]);
    assert.deepEqual(cleanRoots, operations, "each real root cleanup precedes the capture verdict");
  } finally {
    release.resolve(); late.resolve();
    for (const p of plants) {
      if ((p.child as ChildProcess).exitCode === null && p.child.signalCode === null) p.child.stdin!.end();
      await p.closed;
    }
  }
}

describe("fixed capture reaper: real worker profile", { skip: GROUP_B_SKIP }, () => {
  it("fixed capture reaper: real runner procfs and signal compatibility", async () => { await proof(false); });
  it("fixed capture reaper: descendant settlement precedes verdict", async () => { await proof(true); });
  it("fixed capture reaper: own mode kills owned residue before capture proceeds", async () => { await proof(false, "own"); });
  it("fixed capture reaper: genuine report preserves the same owned writer", async () => { await proof(false, "capture", true); });
});
