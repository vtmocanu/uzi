import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Readable } from "node:stream";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";
import { createCodexExecutionSafety, type SpawnBoundaryProcessSeam } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { assertClosedReaperRequest, isClosedReaperRequest } from "../src/closed-reaper.js";
import { launchCodexReaperRoot } from "../src/codex/launcher.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { runnerTmpdir } from "../src/runner-uid.js";

function latch() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

// Fake root outcomes exercise ownership bookkeeping, never kernel ECHILD.
function rig(options: {
  gitStartup?: Promise<void>; gitReap?: Promise<void>; entered?: () => void; gitReapEntered?: () => void;
  scanReap?: Promise<void>; scanEntered?: () => void;
  scanCode?: number; output?: string; disposeFailure?: boolean;
  beforeScanExit?: () => void;
  inspect?: (request: Parameters<SpawnBoundaryProcessSeam>[0]) => Promise<void>;
} = {}) {
  const attempt = newRunAttempt("h-scope", 1, path.join(os.tmpdir(), "capture-h-scope"), () => []);
  const live = new LiveAttemptRegistry(); live.add(attempt);
  const request: QuiesceRunRequest = { mode: "capture", attempt, cloneKey: attempt.cloneKey,
    targetPaths: [attempt.clonePath], processes: true, dockerHost: undefined, registry: live };
  const operations: string[] = [];
  const temp = path.join(runnerTmpdir() ?? os.tmpdir(), "uzi-quiesce-hscope");
  const spawn: SpawnBoundaryProcessSeam = async r => {
    const closed = isClosedReaperRequest(r);
    if (closed) assertClosedReaperRequest(r);
    const scan = closed && r.argv[0] === process.execPath;
    operations.push(closed ? r.argv[0]! : "git");
    await options.inspect?.(r);
    if (!closed) { options.entered?.(); await options.gitStartup; }
    return {
      root: { kind: "boundary_action", dispose: async () => {
        if (scan && options.disposeFailure) throw new Error("modeled cancellation disposal failure");
      }, reap: async () => {
        if (scan) { options.scanEntered?.(); await options.scanReap; }
        if (!closed) { options.gitReapEntered?.(); await options.gitReap; }
        return { ok: true };
      } },
      stdin: null,
      stdout: Readable.from([scan ? options.output ??
        '{"state":"quiescent","processes":[],"killed":[],"detail":"inert scope seam"}'
        : closed && r.argv[0] === "/bin/mktemp" ? temp + "\n" : closed ? "" : " M sentinel.txt\n"]),
      stderr: Readable.from([]),
      waitChild: async () => {
        await new Promise<void>(resolve => setImmediate(resolve));
        if (scan) options.beforeScanExit?.();
        return { code: scan ? options.scanCode ?? 0 : 0 };
      },
    };
  };
  const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
    async () => { throw new Error("legacy spawn forbidden"); }, undefined, undefined,
    spawn, undefined, undefined, { reaperAttempt: () => attempt });
  const git = new GitCache(os.tmpdir(), nullLogger());
  return { safety, git, request, operations };
}

describe("fixed capture reaper: public scope behavior", () => {
  const previous = process.env.UZI_UID_SPLIT;
  // Each case restores process state even on assertion failure.
  async function split(action: () => Promise<void>) {
    process.env.UZI_UID_SPLIT = "1"; setQuiescenceViewForTests(undefined);
    try { await action(); } finally {
      if (previous === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = previous;
    }
  }

  for (const hold of ["pending startup", "unreaped root"] as const) {
    it("H4 refuses reaping during canonical Git " + hold + " and admits after full join", () => split(async () => {
      const entered = latch(), reaping = latch(), release = latch();
      const r = rig({ entered: entered.resolve, gitReapEntered: reaping.resolve,
        gitStartup: hold === "pending startup" ? release.promise : undefined,
        gitReap: hold === "unreaped root" ? release.promise : undefined });
      try {
        const result = await r.safety.withCredentialFreeCapture(10000, context =>
          r.git.withCredentialFreeCapture(context, async () => {
            const git = r.git.worktreeStatus("/capture/runner");
            await entered.promise;
            if (hold === "unreaped root") await reaping.promise;
            const verdict = await quiesceRunAttempt(r.request);
            const admitted = [...r.operations];
            release.resolve(); // Release owned latches even when the baseline assertion fails.
            assert.equal(verdict.process?.state, "unverified", "live canonical Git must exclude the reaper");
            assert.deepEqual(admitted, ["git"], "no sensitive reaper operation admitted");
            assert.deepEqual(await git, [" M sentinel.txt"]);
            assert.equal((await quiesceRunAttempt(r.request)).process?.state, "quiescent",
              "full Git join releases admission without self-deadlock");
          }));
        assert.equal(result.kind, "observed_empty");
      } finally { release.resolve(); }
    }));
  }

  it("H4 refuses canonical Git while reaper holds admission and admits Git after pipeline full join", () => split(async () => {
    const entered = latch(), release = latch();
    const r = rig({ scanEntered: entered.resolve, scanReap: release.promise });
    try {
      const result = await r.safety.withCredentialFreeCapture(10000, context =>
        r.git.withCredentialFreeCapture(context, async () => {
          const pending = quiesceRunAttempt(r.request);
          await Promise.race([entered.promise, pending.then(v => { throw new Error("early reaper completion: " + JSON.stringify(v)); })]);
          const status = await r.git.worktreeStatus("/capture/runner");
          const gitAdmitted = r.operations.includes("git");
          release.resolve(); // Ensure a failing baseline still joins its owned root.
          assert.equal(status, null, "public status refuses while exclusive pipeline owns admission");
          assert.equal(gitAdmitted, false, "refused Git never reaches actual spawn");
          assert.equal((await pending).process?.state, "quiescent");
          assert.deepEqual(await r.git.worktreeStatus("/capture/runner"), [" M sentinel.txt"]);
        }));
      assert.equal(result.kind, "observed_empty");
    } finally { release.resolve(); }
  }));

  it("ordinary canonical Git children retain concurrent admission", () => split(async () => {
    const both = latch(), release = latch();
    let entered = 0;
    const r = rig({ entered: () => { if (++entered === 2) both.resolve(); }, gitStartup: release.promise });
    const result = await r.safety.withCredentialFreeCapture(10000, context =>
      r.git.withCredentialFreeCapture(context, async () => {
        const first = r.git.worktreeStatus("/capture/runner");
        const second = r.git.worktreeStatus("/capture/runner");
        await both.promise;
        assert.deepEqual(r.operations, ["git", "git"]);
        release.resolve();
        assert.deepEqual(await first, [" M sentinel.txt"]);
        assert.deepEqual(await second, [" M sentinel.txt"]);
      }));
    assert.equal(result.kind, "observed_empty");
  }));

  for (const cap of ["scope", "report", "helper"] as const) {
    it("H2 preserves the original absolute " + cap + " cap through every operation", t => split(async () => {
      const start = Date.now();
      t.mock.method(Date, "now", () => start);
      const expected = start + (cap === "helper" ? 10000 : 3000);
      const deadlines: number[] = [];
      const reportDeadlines: unknown[] = [];
      const r = rig({ inspect: async request => {
        deadlines.push(Date.now() + request.timeoutMs!);
        if (request.argv[0] === process.execPath) {
          const input = JSON.parse(assertClosedReaperRequest(request));
          reportDeadlines.push(input.reportDeadline);
        }
      } });
      if (cap === "report") r.request.reportBudget = { deadline: expected, signal: new AbortController().signal };
      const result = await r.safety.withCredentialFreeCapture(cap === "scope" ? 3000 : 20000,
        () => quiesceRunAttempt(r.request));
      assert.equal(result.kind, "observed_empty");
      assert.deepEqual(deadlines, [expected, expected, expected]);
      assert.deepEqual(reportDeadlines, [cap === "report" ? expected : undefined], "ordinary JSON has no reportDeadline; genuine report retains its original cap");
    }));
  }

  it("H2 expired parent deadline returns unverified before any sensitive operation", t => split(async () => {
    const r = rig();
    const now = Date.now();
    t.mock.method(Date, "now", () => now);
    await assert.rejects(r.safety.withCredentialFreeCapture(10000, async () => {
      t.mock.method(Date, "now", () => now + 10001);
      assert.equal((await quiesceRunAttempt(r.request)).process?.state, "unverified");
      assert.deepEqual(r.operations, []);
    }), /codex|capture|boundary/i);
    assert.deepEqual(r.operations, []);
  }));

  for (const failure of ["nonzero", "oversize", "late", "cancel failure"] as const) {
    it("H2 " + failure + " cannot produce a verified process verdict", t => split(async () => {
      const now = Date.now();
      const r = rig({
        scanCode: failure === "nonzero" ? 7 : 0,
        output: failure === "oversize" || failure === "cancel failure" ? "x".repeat(1024 * 1024 + 1) : undefined,
        disposeFailure: failure === "cancel failure",
        beforeScanExit: failure === "late" ? () => { t.mock.method(Date, "now", () => now + 20000); } : undefined,
      });
      let state: string | undefined;
      let error: unknown;
      try {
        await r.safety.withCredentialFreeCapture(10000, async () => {
          state = (await quiesceRunAttempt(r.request)).process?.state;
          assert.equal(state, "unverified");
        });
      } catch (e) { error = e; }
      assert.equal(state, "unverified", "failure must expose an unverified process observation");
      assert.ok(r.operations.includes(process.execPath), "scanner actually executed");
      if (failure === "cancel failure") assert.ok(error, "unconfirmed cancellation also refuses capture completion");
    }));
  }

  it("H6 actual private scan rejects substituted flag, argv and env before supervisor spawn", () => split(async () => {
    let rejected = 0, actual = 0;
    const r = rig({ inspect: async request => {
      if (request.argv[0] !== process.execPath) return;
      for (const patch of [
        { argv: Object.freeze([...request.argv, "--inert-substitution"]) },
        { argv: Object.freeze([process.execPath, "--eval", "inert"]) },
        { env: Object.freeze({ ...request.env, PATH: "/foreign" }) },
      ]) {
        await assert.rejects(launchCodexReaperRoot(Object.freeze({ ...request, ...patch }), {
          spawnSupervisor: () => { actual++; throw new Error("must never spawn"); },
        }), /unbound reaper operation/);
        rejected++;
      }
    } });
    const result = await r.safety.withCredentialFreeCapture(10000, () => quiesceRunAttempt(r.request));
    assert.equal(result.kind, "observed_empty");
    assert.equal(rejected, 3); assert.equal(actual, 0);
    assert.ok("value" in result); assert.equal(result.value.process?.state, "quiescent");
  }));
});
