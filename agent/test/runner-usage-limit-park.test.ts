import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { LimitReachedError } from "../src/limit.js";
import {
  api,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
  secretRecordingLogger,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// ── PRD #35: the usage-limit park and its cleanup carve-out ───────────────────
//
// Three cases, deliberately THREE SEPARATELY NAMED TESTS rather than one
// table-driven list. Only the third fails against an implementation that keys the
// carve-out on the acknowledgement's `applied` flag instead of on its `status`, so
// it is the artifact that shows at review time whether the contract was understood
// rather than merely satisfied — and a table row is much harder to find later.
describe("RunRunner — usage-limit park (PRD #35)", () => {
  // An executor that materializes everything a resume needs (worktree-sibling
  // plugin dir + a per-run HOME holding the SDK transcript) and then dies on a
  // usage limit, the way SdkExecutor does when a turn ends in a limit-classified
  // error result.
  function limitFactory(
    homeRoot: string,
    iid: number,
  ): { factory: ExecutorFactory; paths: () => Paths } {
    const pluginDir = skillsPluginDir(worktreeDirFor(iid));
    let runHome = "";
    const factory: ExecutorFactory = (runId) => {
      runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          run: async (): Promise<ExecutorResult> => {
            fs.mkdirSync(pluginDir, { recursive: true });
            fs.writeFileSync(path.join(pluginDir, "marker"), "x");
            fs.mkdirSync(runHome, { recursive: true });
            fs.writeFileSync(
              path.join(runHome, "session"),
              "transcript",
              "utf8",
            );
            throw new LimitReachedError({
              resetsAtMs: Date.now() + 5 * 3600_000,
              rateLimitType: "five_hour",
            });
          },
        },
      };
    };
    return {
      factory,
      paths: () => ({ pluginDir, runHome, worktree: worktreeDirFor(iid) }),
    };
  }
  interface Paths {
    pluginDir: string;
    runHome: string;
    worktree: string;
  }
  const assertAllGone = (p: Paths) => {
    assert.strictEqual(
      fs.existsSync(p.worktree),
      false,
      "runner clone must be removed",
    );
    assert.strictEqual(
      fs.existsSync(p.pluginDir),
      false,
      "skills plugin dir must be removed",
    );
    assert.strictEqual(
      fs.existsSync(p.runHome),
      false,
      "run HOME must be removed",
    );
  };

  it("preserves the clone, the plugin dir and the run HOME when the server parks the run", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-ok-"));
    try {
      const { factory, paths } = limitFactory(homeRoot, 91);
      await runnerWith(factory, gitlab).execute(
        gitlabClaim(91, { wait_on_limit: true }),
      );
      const p = paths();
      // All three survive: a resume needs the transcript, the worktree AND the
      // plugin dir, and preserving only some of them resumes into a broken session.
      assert.strictEqual(
        fs.existsSync(p.worktree),
        true,
        "runner clone must survive a park",
      );
      assert.strictEqual(
        fs.existsSync(p.pluginDir),
        true,
        "skills plugin dir must survive a park",
      );
      assert.strictEqual(
        fs.existsSync(p.runHome),
        true,
        "run HOME must survive a park",
      );
      const parked = api.states.find((s) => s.body.status === "limit_wait");
      assert.ok(parked, "the worker must have reported limit_wait");
      assert.strictEqual(parked.body.rate_limit_type, "five_hour");
      assert.strictEqual(typeof parked.body.limit_resets_at, "number");
      // Decision 10's structured feed kind, not a status line with the facts baked
      // into prose: M4 maps rate_limit_type through a known-value lookup, which is
      // impossible against interpolated text.
      const feed = api
        .messages(parked.runId)
        .find((m) => m.kind === "limit_wait");
      assert.ok(feed, "a structured limit_wait feed message must be emitted");
      assert.strictEqual(feed.payload?.rate_limit_type, "five_hour");
      assert.strictEqual(
        typeof feed.payload?.resets_at,
        "string",
        "resets_at is an ISO string",
      );
      assert.ok(
        !("attempt" in (feed.payload ?? {})),
        "attempt is server-owned and must not be guessed here",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // 🔴 THE SECURITY HALF OF THE CARVE-OUT, and until this test it had no artifact
  // behind it. Measured by the M1 reviewer: extending `!parked` to also guard the
  // secret eviction and the two gate-map deletes passed typecheck AND all 41 runner
  // tests with exit 0. So an implementation leaving a parked run's decrypted forge
  // PAT and Anthropic token registered in the logger — for the days a seven-day
  // window lasts, on a worker that goes on to run other runs — was green everywhere.
  //
  // The carve-out is three FILESYSTEM removals. The eviction is not one of them: the
  // secrets are re-delivered on the next claim, so holding them buys nothing and
  // costs a widened exposure window on a machine that keeps working.
  it("still evicts the run-scoped secrets from the logger when the run parks", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(
      path.join(os.tmpdir(), "uzi-park-secrets-"),
    );
    try {
      const { factory, paths } = limitFactory(homeRoot, 95);
      const { logger, added, removed } = secretRecordingLogger();
      const claim = gitlabClaim(95, { wait_on_limit: true });
      await runnerWith(factory, gitlab, undefined, logger).execute(claim);
      // Precondition: the park actually happened, or this asserts nothing.
      assert.strictEqual(
        fs.existsSync(paths().runHome),
        true,
        "must have parked (else this test is vacuous)",
      );
      assert.ok(added.length > 0, "the run registered secrets");
      for (const s of added) {
        assert.ok(
          removed.includes(s),
          `secret registered at claim must be evicted on park: ${s}`,
        );
      }
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("cleans up fully when the park is refused with a 409 (the run was cancelled under it)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-409-"));
    try {
      const { factory, paths } = limitFactory(homeRoot, 92);
      const claim = gitlabClaim(92, { wait_on_limit: true });
      api.refuseStateWith409(claim.run_id);
      await runnerWith(factory, gitlab).execute(claim);
      assertAllGone(paths());
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // 🔴 THE DISCRIMINATING CASE — the one an `applied`-keyed carve-out fails.
  //
  // The server DECLINED the park and failed the run instead. That is a DESIGNED
  // outcome, not an error: it is what happens when RUN_LIMIT_MAX_WAITS is
  // exhausted, when the computed retry_not_before exceeds RUN_LIMIT_MAX_PARK, or
  // when wait_on_limit is false and the report is coerced. The server answers 200,
  // because a transition WAS applied — just not the requested one — so `applied` is
  // **true** while the run is emphatically not parked.
  //
  // Budget exhaustion is the ordinary end of a run that keeps hitting limits, i.e.
  // the exact population this feature serves, so an `applied`-keyed branch would
  // leak the clone, the plugin dir and up to ~170 MB of run HOME on the MOST COMMON
  // cause of all, forever, for a run nothing will ever claim again.
  it("cleans up fully when the server answers 200 but failed the run instead of parking it", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(
      path.join(os.tmpdir(), "uzi-park-declined-"),
    );
    try {
      const { factory, paths } = limitFactory(homeRoot, 93);
      const claim = gitlabClaim(93, { wait_on_limit: true });
      // 200 + "failed": applied is true, status is not limit_wait.
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(factory, gitlab).execute(claim);
      assertAllGone(paths());
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("fails with the structured limit fields, and does not park, when the run is opted out", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-optout-"));
    try {
      const { factory, paths } = limitFactory(homeRoot, 94);
      // wait_on_limit absent entirely — an older server, or a run created before the
      // opt-in existed. Must behave as opted out.
      await runnerWith(factory, gitlab).execute(gitlabClaim(94));
      assertAllGone(paths());
      const failed = api.states.find((s) => s.body.status === "failed");
      assert.ok(failed, "an opted-out run must still report failed");
      assert.strictEqual(
        api.states.some((s) => s.body.status === "limit_wait"),
        false,
        "must not park",
      );
      // The worker reports the STRUCTURED facts and lets the server compose the
      // sentence (Decision 8) — composing it here would carry an unvalidated
      // rateLimitType into the run row as free text.
      assert.strictEqual(failed.body.rate_limit_type, "five_hour");
      assert.strictEqual(typeof failed.body.limit_resets_at, "number");
      assert.strictEqual(
        failed.body.failure_reason,
        undefined,
        "the SERVER composes the reason, not the worker",
      );
      // The opted-out counterpart kind. Distinct from limit_wait because the two mean
      // opposite things to a reader: one resumes, one is over.
      const hit = api
        .messages(failed.runId)
        .find((m) => m.kind === "limit_hit");
      assert.ok(hit, "a structured limit_hit feed message must be emitted");
      assert.strictEqual(hit.payload?.rate_limit_type, "five_hour");
      assert.strictEqual(
        api.messages(failed.runId).some((m) => m.kind === "limit_wait"),
        false,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
