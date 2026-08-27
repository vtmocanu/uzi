import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import {
  api,
  deferred,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
  secretRecordingLogger,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

/** Resolves once `signal` aborts (or immediately if already aborted). */
function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

// ── PRD #556 M1 — the worker-shutdown session-preservation carve-out ──────────
//
// A park (usage limit) preserves EXACTLY two filesystem dirs a same-worker resume
// needs: the sibling skills plugin dir and the per-run HOME that holds the resumable
// SDK transcript. Before M1 a WORKER-SHUTDOWN interrupt — the `active?.shuttingDown`
// catch branch — did NOT set the preserve flag, so the finally deleted both dirs and a
// same-worker re-claim could no longer resume the SDK session. M1 gives the shutdown
// path its own `preserveSession` flag that skips those same two removals. The carve-out
// is deliberately scoped to ONLY those two: the runner clone is still removed, and the
// run stays NON-terminal (no failed/completed report) so the sweeper requeues it.
describe("RunRunner — worker-shutdown session preservation (PRD #556 M1)", () => {
  interface Paths {
    pluginDir: string;
    runHome: string;
    worktree: string;
  }

  /** An executor that materializes everything a resume needs (worktree-sibling plugin
   *  dir + a per-run HOME holding the SDK transcript), signals it is mid-run, waits for
   *  its controller to abort, then throws — the shape a run takes when the worker's
   *  shutdown() aborts it mid-flight. */
  function shutdownFactory(
    homeRoot: string,
    iid: number,
  ): { factory: ExecutorFactory; started: Promise<void>; paths: () => Paths } {
    const pluginDir = skillsPluginDir(worktreeDirFor(iid));
    const start = deferred();
    let runHome = "";
    const factory: ExecutorFactory = (runId) => {
      runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(pluginDir, { recursive: true });
            fs.writeFileSync(path.join(pluginDir, "marker"), "x");
            fs.mkdirSync(runHome, { recursive: true });
            fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
            start.resolve();
            await waitAbort(ctx.signal!);
            throw new Error("aborted mid-run");
          },
        },
      };
    };
    return {
      factory,
      started: start.promise,
      paths: () => ({ pluginDir, runHome, worktree: worktreeDirFor(iid) }),
    };
  }

  it("preserves the plugin dir and the run HOME (but removes the clone) when the worker shuts down mid-run", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-556-shut-"));
    try {
      const iid = 556;
      const { factory, started, paths } = shutdownFactory(homeRoot, iid);
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      const p = runner.execute(claim);
      await started;
      // shutdown() marks the active run shuttingDown and aborts its controller, driving
      // execute() into the `active?.shuttingDown` catch branch.
      runner.shutdown();
      await p;

      const paths_ = paths();
      // The two dirs a same-worker resume needs must SURVIVE the shutdown.
      assert.strictEqual(
        fs.existsSync(paths_.pluginDir),
        true,
        "skills plugin dir must survive a worker-shutdown interrupt (PRD #556 M1)",
      );
      assert.strictEqual(
        fs.existsSync(paths_.runHome),
        true,
        "run HOME must survive a worker-shutdown interrupt (PRD #556 M1)",
      );
      // The carve-out is scoped to exactly those two: the runner clone is still removed.
      assert.strictEqual(
        fs.existsSync(paths_.worktree),
        false,
        "the runner clone is still removed on a shutdown — the carve-out is only the two dirs",
      );
      // The run reported NO terminal state — it stays requeuable.
      assert.strictEqual(
        api.states.some(
          (s) =>
            s.runId === claim.run_id &&
            (s.body.status === "failed" || s.body.status === "completed"),
        ),
        false,
        "a shutdown interruption must requeue, never report a terminal state",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // 🔴 THE R1 REGRESSION GUARD — the shutdown sibling of the park's secret-eviction
  // test ("still evicts the run-scoped secrets from the logger when the run parks",
  // runner-usage-limit-park.test.ts). The finally's `for (const s of runScopedSecrets)
  // this.log.removeSecret(s)` is structurally UPSTREAM of the `preserveSession`
  // carve-out: M1's flag skips exactly the two filesystem removals (plugin dir + HOME),
  // and MUST NOT be widened to cover the secret eviction. If it ever were — the exact
  // mistake the finally's "HOW EACH ONE FAILS IF YOU WIDEN" warning names — the eviction
  // fails SILENTLY: typecheck and every existing test still pass with exit 0, leaving a
  // shutdown-interrupted run's decrypted forge PAT and Anthropic token registered in the
  // logger on a worker that goes on to run other runs. The eviction must fire on a
  // shutdown exactly as it does on a park; this test is what catches that silent widening.
  it("still evicts the run-scoped secrets from the logger when the worker shuts down mid-run", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(
      path.join(os.tmpdir(), "uzi-556-shut-secrets-"),
    );
    try {
      const iid = 557;
      const { factory, started, paths } = shutdownFactory(homeRoot, iid);
      const { logger, added, removed } = secretRecordingLogger();
      const runner = runnerWith(factory, gitlab, undefined, logger);
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      const p = runner.execute(claim);
      await started;
      // shutdown() marks the active run shuttingDown and aborts its controller, driving
      // execute() into the `active?.shuttingDown` catch branch (the M1 carve-out path).
      runner.shutdown();
      await p;

      // Precondition: the shutdown carve-out path was actually taken (the HOME
      // survived), or the eviction assertion below could be passing for the wrong
      // reason (e.g. a plain failure cleanup rather than the shutdown branch).
      assert.strictEqual(
        fs.existsSync(paths().runHome),
        true,
        "the run HOME must survive — proves the shutdown carve-out path was taken (else this test is vacuous)",
      );
      // Precondition: the run actually registered secrets, or the loop below asserts
      // nothing. The claim delivers the forge PAT + Anthropic token, addSecret'd early
      // in execute() — well before the shutdown abort point.
      assert.ok(added.length > 0, "the run registered secrets");
      for (const s of added) {
        assert.ok(
          removed.includes(s),
          `secret registered at claim must be evicted on a shutdown interrupt: ${s}`,
        );
      }
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
