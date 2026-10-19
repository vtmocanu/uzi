import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Readable } from "node:stream";
import os from "node:os";
import path from "node:path";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { launchCodexReaperRoot } from "../src/codex/launcher.js";
import { assertClosedReaperRequest, isClosedReaperRequest } from "../src/closed-reaper.js";
import * as closed from "../src/closed-reaper.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { runnerTmpdir } from "../src/runner-uid.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";

function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

// These are inert process seams, not evidence about the real scanner or ECHILD.
describe("fixed capture reaper: portable closed admission", () => {
  it("exports no generic branding factory and rejects arbitrary/noop requests at actual launcher admission", async () => {
    assert.equal("runClosedReaper" in closed, false);
    let actual = 0;
    for (const change of [
      {}, { argv: ["/arbitrary/inert", "script"] }, { argv: [process.execPath, "--eval", "inert"] },
      { env: { PATH: "/foreign" } }, { cwd: "/foreign" }, { identity: "worker_pat" },
      { timeoutMs: 0 }, { timeoutMs: Infinity }, { stdin: "forged" }, { check: () => {} },
    ]) {
      const request = Object.freeze({ argv: Object.freeze(["/bin/mktemp", "-d", "/tmp/uzi-quiesce-XXXXXXXXXX"]),
        env: Object.freeze({ PATH: "/usr/local/bin:/usr/bin:/bin" }), cwd: "/", identity: "command",
        timeoutMs: 1000, ...change }) as BoundaryProcessRequest;
      assert.equal(isClosedReaperRequest(request), false);
      await assert.rejects(launchCodexReaperRoot(request, { spawnSupervisor: () => {
        actual++; throw new Error("must never launch");
      } }), /unbound reaper operation/);
    }
    assert.equal(actual, 0);
  });

  it("joins ignored initiated pipelines through cleanup, but rejects a new inherited pipeline after callback revocation", async () => {
    const oldSplit = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    setQuiescenceViewForTests(undefined);
    const entered = latch(), release = latch(), late = latch();
    const attempt = newRunAttempt("portable", 1, path.join(os.tmpdir(), "capture-portable"), () => []);
    const live = new LiveAttemptRegistry(); live.add(attempt);
    const request: QuiesceRunRequest = { mode: "capture", attempt, cloneKey: attempt.cloneKey,
      targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live,
      otherClaimInFlight: false };
    const operations: string[] = [];
    const temp = path.join(runnerTmpdir() ?? os.tmpdir(), "uzi-quiesce-portable");
    let seamFailure: unknown;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
      async r => {
        try {
        const input = assertClosedReaperRequest(r);
        operations.push(r.argv[0]!);
        const scan = r.argv[0] === process.execPath;
        if (scan) {
          const parsed = JSON.parse(input);
          assert.equal(parsed.ownMarker, attempt.marker);
          assert.deepEqual(parsed.targetPaths, [attempt.clonePath]);
          assert.equal(parsed.targetUid, 10002);
          assert.ok(parsed.reportDeadline <= Date.now() + 10000);
        } else assert.equal(input, "");
        return { root: { kind: "boundary_action", dispose: async () => {},
          reap: async () => { if (scan) { entered.resolve(); await release.promise; } return { ok: true }; } },
          stdin: null, stdout: Readable.from([scan ? '{"state":"quiescent","processes":[],"killed":[],"detail":"inert seam"}' : r.argv[0] === "/bin/mktemp" ? temp + "\n" : ""]),
          stderr: Readable.from([]), waitChild: async () => {
            await new Promise<void>(resolve => setImmediate(resolve));
            return { code: 0 };
          } };
        } catch (error) {
          seamFailure = error;
          throw error;
        }
      }, undefined, { reaperAttempt: () => attempt });
    let pending!: ReturnType<typeof quiesceRunAttempt>;
    let continuation!: ReturnType<typeof quiesceRunAttempt>;
    let done = false;
    let ready!: Promise<void>;
    try {
      const capture = safety.withCredentialFreeCapture(10000, async () => {
        pending = quiesceRunAttempt(request); // Deliberately ignored by the callback.
        continuation = late.promise.then(() => quiesceRunAttempt(request));
        ready = Promise.race([entered.promise, pending.then(verdict => {
          throw new Error("pipeline completed before scanner reap: " + JSON.stringify(verdict), { cause: seamFailure });
        })]);
        await ready;
      });
      void capture.then(() => { done = true; }, () => {});
      // Observe both callback failure and early pipeline completion while awaiting readiness.
      await Promise.race([entered.promise, capture.then(() => {
        throw new Error("capture completed before scanner reap");
      })]);
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(done, false, "primary exit cannot release a held process root");
      late.resolve();
      assert.equal((await continuation).process?.state, "unverified");
      release.resolve();
      const result = await capture;
      assert.equal(result.kind, "observed_empty");
      assert.equal((await pending).process?.state, "quiescent");
      assert.deepEqual(operations, ["/bin/mktemp", process.execPath, "/bin/rm"]);
    } finally {
      release.resolve(); late.resolve();
      if (oldSplit === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = oldSplit;
    }
  });

  it("floors the shortest fractional maintenance budget once without renewing it across three operations", async t => {
    const oldSplit = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    setQuiescenceViewForTests(undefined);
    const start = 1_800_000_000_000;
    let elapsed = 0;
    t.mock.method(Date, "now", () => start + elapsed);
    const budget = new MaintenanceBudget(6000.75, () => elapsed);
    const originalDeadline = budget.deadline;
    const attempt = newRunAttempt("fractional", 1, path.join(os.tmpdir(), "capture-fractional"), () => []);
    const live = new LiveAttemptRegistry(); live.add(attempt);
    const request: QuiesceRunRequest = { mode: "capture", attempt, cloneKey: attempt.cloneKey,
      targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live,
      reportBudget: { deadline: start + 20000, signal: new AbortController().signal } };
    const operations: string[] = [];
    const deadlines: number[] = [];
    const temp = path.join(runnerTmpdir() ?? os.tmpdir(), "uzi-quiesce-fractional");
    let seamFailure: unknown;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
      async r => {
        try {
          const input = assertClosedReaperRequest(r);
          assert.equal(MaintenanceBudget.current(), budget, "original budget object at every operation");
          assert.equal(budget.deadline, originalDeadline, "original maintenance deadline retained");
          assert.equal(budget.remainingMs(), 6000.75 - elapsed, "fractional budget remains fractional");
          const deadline = Date.now() + r.timeoutMs!;
          assert.ok(Number.isSafeInteger(deadline), "actual admitted deadline is a safe integer");
          assert.equal(deadline, start + 6000, "deadline is the floored original bound, never renewed");
          deadlines.push(deadline);
          operations.push(r.argv[0]!);
          const scan = r.argv[0] === process.execPath;
          if (scan) {
            console.log("FRACTIONAL_CANONICAL_JSON", input);
            const parsed = JSON.parse(input);
            assert.equal(parsed.reportDeadline, start + 6000);
            assert.ok(Number.isSafeInteger(parsed.reportDeadline));
            assert.equal(parsed.ownMarker, attempt.marker);
            assert.deepEqual(parsed.targetPaths, [attempt.clonePath]);
          } else assert.equal(input, "");
          return { root: { kind: "boundary_action", dispose: async () => {},
            reap: async () => { return { ok: true }; } },
            stdin: null, stdout: Readable.from([scan
              ? '{"state":"quiescent","processes":[],"killed":[],"detail":"inert fractional seam"}'
              : r.argv[0] === "/bin/mktemp" ? temp + "\n" : ""]),
            stderr: Readable.from([]), waitChild: async () => {
              await new Promise<void>(resolve => setImmediate(resolve));
              elapsed += 101;
              return { code: 0 };
            } };
        } catch (error) { seamFailure = error; throw error; }
      }, undefined, { reaperAttempt: () => attempt });
    try {
      const result = await MaintenanceBudget.run(budget, () =>
        safety.withCredentialFreeCapture(9000, async () => quiesceRunAttempt(request)))
        .catch(error => { throw seamFailure ?? error; });
      if (seamFailure) throw seamFailure;
      assert.equal(result.kind, "observed_empty");
      assert.ok("value" in result);
      assert.equal(result.value.process?.state, "quiescent");
      assert.deepEqual(operations, ["/bin/mktemp", process.execPath, "/bin/rm"]);
      assert.deepEqual(deadlines, [start + 6000, start + 6000, start + 6000]);
      assert.equal(budget.deadline, originalDeadline);
    } finally {
      budget.dispose();
      if (oldSplit === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = oldSplit;
    }
  });

  it("refuses missing, forged, foreign, broadened and dependency-overridden attempts before any process seam", async () => {
    const oldSplit = process.env.UZI_UID_SPLIT; process.env.UZI_UID_SPLIT = "1";
    setQuiescenceViewForTests(undefined);
    const attempt = newRunAttempt("scope", 1, path.join(os.tmpdir(), "capture-scope"), () => []);
    const foreign = newRunAttempt("foreign", 1, path.join(os.tmpdir(), "capture-foreign"), () => []);
    const live = new LiveAttemptRegistry(); live.add(attempt);
    let launches = 0;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
      async () => { launches++; throw new Error("process seam forbidden"); }, undefined, { reaperAttempt: () => attempt });
    const request: QuiesceRunRequest = { mode: "capture", attempt, cloneKey: attempt.cloneKey,
      targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live };
    try {
      const result = await safety.withCredentialFreeCapture(10000, async () => {
        for (const patch of [{ attempt: undefined }, { attempt: { ...attempt } }, { attempt: foreign, cloneKey: foreign.cloneKey, targetPaths: [foreign.clonePath] },
          { workerWide: true }, { targetPaths: [] }, { targetPaths: ["/foreign"] }, { cloneKey: "foreign" }]) {
          assert.equal((await quiesceRunAttempt({ ...request, ...patch })).process?.state, "unverified");
        }
        assert.equal((await quiesceRunAttempt(request, { targetUid: 10002 })).process?.state, "unverified");
        setQuiescenceViewForTests({ procRoot: "/inert-view" });
        assert.equal((await quiesceRunAttempt(request)).process?.state, "unverified");
      });
      assert.equal(result.kind, "observed_empty");
      assert.equal(launches, 0);
    } finally {
      setQuiescenceViewForTests(undefined);
      if (oldSplit === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = oldSplit;
    }
  });
});
