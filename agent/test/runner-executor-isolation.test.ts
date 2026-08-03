import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import {
  FakeReapExecutor,
  api,
  barrier,
  deferred,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
  secretRecordingLogger,
  type Deferred,
} from "./runner-harness.js";

installHarness();

describe("RunRunner — per-run executor isolation (PRD #42 Decision 4)", () => {
  it("two concurrent runs each reap ONLY their own subprocess set; a sibling is untouched", async () => {
    const { gitlab } = fakeGitlab();
    const claimA = gitlabClaim(51);
    const claimB = gitlabClaim(52);
    const pidsByRun: Record<string, number[]> = {
      [claimA.run_id]: [7001, 7002],
      [claimB.run_id]: [8001, 8002],
    };
    const killLog: Array<{ runId: string; pid: number }> = [];
    const bothSpawned = barrier(2);
    const gates: Record<string, Deferred> = {
      [claimA.run_id]: deferred(),
      [claimB.run_id]: deferred(),
    };
    const execs = new Map<string, FakeReapExecutor>();
    const factoryCalls: string[] = [];
    const factory: ExecutorFactory = (runId) => {
      factoryCalls.push(runId);
      const e = new FakeReapExecutor(
        runId,
        pidsByRun[runId]!,
        killLog,
        bothSpawned.arrive,
        gates[runId]!.promise,
      );
      execs.set(runId, e);
      return { executor: e };
    };
    const rnr = runnerWith(factory, gitlab);

    const errs: unknown[] = [];
    // If an execute rejects before reaching the executor, still trip the barrier so
    // the test asserts (and fails clearly) rather than hanging on bothSpawned.ready.
    const pA = rnr.execute(claimA).catch((e) => {
      errs.push(e);
      bothSpawned.arrive();
    });
    const pB = rnr.execute(claimB).catch((e) => {
      errs.push(e);
      bothSpawned.arrive();
    });
    try {
      await bothSpawned.ready;
      assert.deepStrictEqual(
        errs,
        [],
        "both runs reached the executor without erroring",
      );
      // The factory built a DISTINCT executor per run (per-run construction).
      assert.deepStrictEqual(
        [...factoryCalls].sort(),
        [claimA.run_id, claimB.run_id].sort(),
      );
      assert.notStrictEqual(execs.get(claimA.run_id), execs.get(claimB.run_id));
      // Both runs are mid-run with their own sets populated.
      assert.deepStrictEqual(
        execs.get(claimA.run_id)!.livePids(),
        [7001, 7002],
      );
      assert.deepStrictEqual(
        execs.get(claimB.run_id)!.livePids(),
        [8001, 8002],
      );

      // Release run A → its pre-push reap kills EXACTLY A's tree and clears A's set.
      gates[claimA.run_id]!.resolve();
      await pA;
      assert.deepStrictEqual(
        killLog.map((k) => k.pid).sort((a, b) => a - b),
        [7001, 7002],
      );
      // The SIBLING (B) is untouched: its set is intact, none of its pids reaped.
      assert.deepStrictEqual(
        execs.get(claimB.run_id)!.livePids(),
        [8001, 8002],
      );
      assert.deepStrictEqual(execs.get(claimA.run_id)!.livePids(), []);

      // Release run B → it now reaps exactly its own set.
      gates[claimB.run_id]!.resolve();
      await pB;
    } finally {
      gates[claimA.run_id]!.resolve();
      gates[claimB.run_id]!.resolve();
      await Promise.allSettled([pA, pB]);
    }

    // End state: every reap targeted only its own run's pids — nothing crossed over.
    for (const k of killLog) {
      assert.ok(
        pidsByRun[k.runId]!.includes(k.pid),
        `run ${k.runId} reaped foreign pid ${k.pid}`,
      );
    }
    const grouped = (id: string) =>
      killLog
        .filter((k) => k.runId === id)
        .map((k) => k.pid)
        .sort((a, b) => a - b);
    assert.deepStrictEqual(grouped(claimA.run_id), [7001, 7002]);
    assert.deepStrictEqual(grouped(claimB.run_id), [8001, 8002]);
    // Both runs completed with an MR (the reaps did not disturb the happy path).
    for (const c of [claimA, claimB]) {
      assert.ok(
        api.states.some(
          (s) => s.runId === c.run_id && s.body.status === "completed",
        ),
        `run ${c.issue_iid} completed`,
      );
    }
  });

  it("gives each run its own HOME (agent-home/<runId>) and removes it on terminal (Decision 5)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runhome-"));
    const created: string[] = [];
    const factory: ExecutorFactory = (runId) => {
      const runHome = path.join(homeRoot, runId);
      created.push(runHome);
      const executor: Executor = {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          // Mirror SdkExecutor: create the HOME and write a run-private file in it.
          await fs.promises.mkdir(runHome, { recursive: true });
          await fs.promises.writeFile(
            path.join(runHome, "session"),
            ctx.runId,
            "utf8",
          );
          return { branch: ctx.branch };
        },
      };
      return { executor, homeDir: runHome };
    };
    const claimA = gitlabClaim(55);
    const claimB = gitlabClaim(56);
    try {
      await Promise.all([
        runnerWith(factory, gitlab).execute(claimA),
        runnerWith(factory, gitlab).execute(claimB),
      ]);
      // Two distinct, run-id-scoped HOMEs were built (no shared dir).
      assert.strictEqual(created.length, 2);
      assert.deepStrictEqual(
        created.map((p) => path.basename(p)).sort(),
        [claimA.run_id, claimB.run_id].sort(),
      );
      // Each was removed on terminal (the runner's finally cleaned every run's HOME).
      for (const h of created) {
        assert.strictEqual(
          fs.existsSync(h),
          false,
          `HOME ${h} must be removed on terminal`,
        );
      }
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // PRD #108 M6. The Go module cache writes its package directories mode 0555, and
  // `fs.rm(force: true)` suppresses ENOENT, not EACCES — so before this fix the
  // runner's terminal cleanup rejected on the first unlink inside such a directory
  // and stranded the module cache (measured: 167.3 MB for one run,
  // "EACCES: permission denied, unlink '<home>/go/pkg/mod/…/benchmark_test.go'").
  //
  // RED against the unfixed runner: with `fs.rm` restored at runner.ts's cleanup
  // this test fails on `HOME … must be removed` while the run itself still
  // completes — which is the other half of the contract, below.
  it("removes a run HOME containing read-only (0555) directories, and cleanup failure never fails the run", async (t) => {
    // Root ignores the permission bits, so the fixture would not be hostile and
    // the assertion would hold against the UNFIXED runner too. Say so; do not
    // pass quietly.
    if (process.getuid?.() === 0) {
      t.skip(
        "running as uid 0 — root bypasses the 0555 fixture, so it proves nothing here",
      );
      return;
    }
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runhome-ro-"));
    let runHome = "";
    let modDir = "";
    // The mode as the FILESYSTEM reported it, read back after the chmod and
    // immediately before the run returns — so the fixture is proven hostile at
    // the moment the runner's cleanup sees it, not merely requested to be.
    let observedMode = "";
    const factory: ExecutorFactory = (runId) => {
      runHome = path.join(homeRoot, runId);
      modDir = path.join(
        runHome,
        "go",
        "pkg",
        "mod",
        "gopkg.in",
        "inf.v0@v0.9.1",
      );
      const executor: Executor = {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          // Mirror what a `go build` inside the run leaves behind.
          fs.mkdirSync(modDir, { recursive: true });
          fs.writeFileSync(
            path.join(modDir, "benchmark_test.go"),
            "package inf\n",
          );
          fs.chmodSync(modDir, 0o555);
          observedMode = (fs.lstatSync(modDir).mode & 0o777).toString(8);
          return { branch: ctx.branch };
        },
      };
      return { executor, homeDir: runHome };
    };
    const claim = gitlabClaim(57);
    try {
      await runnerWith(factory, gitlab).execute(claim);
      assert.strictEqual(
        observedMode,
        "555",
        "fixture directory was not actually read-only when the run ended",
      );
      assert.strictEqual(
        fs.existsSync(runHome),
        false,
        `HOME ${runHome} must be removed even with a 0555 dir inside`,
      );
      // Cleanup is best-effort and lives in a `finally`; it must never turn a
      // completed run into a failed one.
      const state = api.states.filter((s) => s.runId === claim.run_id).at(-1);
      assert.strictEqual(state?.body.status, "completed");
    } finally {
      // Only reachable when the fix regressed and the tree survived.
      if (fs.existsSync(modDir)) fs.chmodSync(modDir, 0o755);
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("registers the run's secrets and evicts them all on terminal (Decision 7)", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added, removed } = secretRecordingLogger();
    const PAT = "fixture-forge-pat-evict-001";
    const OAUTH = "dummy-oauth-evict-000000";
    const claim = gitlabClaim(60, {
      secrets: {
        forge_pat: PAT,
        anthropic_oauth_token: OAUTH,
        forge_username: "bot",
      },
    });
    await runnerWith(
      () => ({ executor: new StubExecutor(nullLogger()) }),
      gitlab,
      undefined,
      logger,
    ).execute(claim);

    assert.ok(added.includes(PAT), "PAT registered");
    assert.ok(added.includes(OAUTH), "OAuth token registered");
    // Every registered run-secret (PAT, OAuth token, and the derived git Basic
    // credential) is evicted on terminal — nothing lingers in the process set.
    for (const s of added) {
      assert.ok(
        removed.includes(s),
        `run secret ${JSON.stringify(s)} must be evicted on terminal`,
      );
    }
  });

  it("still evicts the run's secrets when the run FAILS (Decision 7 — finally path)", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added, removed } = secretRecordingLogger();
    const boom: Executor = {
      run: async () => {
        throw new Error("kaboom");
      },
    };
    const claim = gitlabClaim(61, {
      secrets: {
        forge_pat: "fixture-forge-pat-evict-002",
        anthropic_oauth_token: "dummy-oauth-evict-111111",
      },
    });
    await runnerWith(
      () => ({ executor: boom }),
      gitlab,
      undefined,
      logger,
    ).execute(claim);

    assert.ok(
      api.states.some(
        (s) => s.runId === claim.run_id && s.body.status === "failed",
      ),
      "run failed",
    );
    assert.ok(added.length > 0, "secrets were registered");
    for (const s of added) {
      assert.ok(
        removed.includes(s),
        `failed run's secret ${JSON.stringify(s)} must still be evicted`,
      );
    }
  });

  it("rejects a run whose id is not a UUID BEFORE it becomes a path (defense in depth)", async () => {
    const { gitlab } = fakeGitlab();
    let factoryReached = false;
    const factory: ExecutorFactory = () => {
      factoryReached = true;
      return { executor: new StubExecutor(nullLogger()) };
    };
    // Empty (would collapse the per-run HOME to the shared root), path separators,
    // and traversal (would escape it) — all must be refused before makeExecutor.
    for (const badId of ["", "../../etc", "a/b", "not-a-uuid"]) {
      await assert.rejects(
        runnerWith(factory, gitlab).execute(gitlabClaim(70, { run_id: badId })),
        /invalid run id/,
      );
    }
    assert.strictEqual(
      factoryReached,
      false,
      "the executor factory must not be reached for an invalid run id",
    );
  });
});
