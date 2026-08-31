import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { LimitReachedError } from "../src/limit.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  barrier,
  client,
  deferred,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  input,
  runnerWith,
  secretRecordingLogger,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// ── PRD #218 shared helpers ───────────────────────────────────────────────────
// Real git so the fetch-back and reseed are exercised end to end, not mocked.
const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

/** Commit a file in a git working tree (the runner clone, or the fixture origin) and
 *  return the new HEAD sha. */
function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
  return execFileSync("git", ["-C", treePath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

/** The sha a ref resolves to in the worker bare, or null when it does not exist. */
function shaInBare(bare: string, ref: string): string | null {
  try {
    return execFileSync("git", ["-C", bare, "rev-parse", "--verify", ref], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: ["pipe", "pipe", "pipe"],
    }).trim();
  } catch {
    return null;
  }
}

function gitUpdateRef(bare: string, ref: string, sha: string): void {
  execFileSync("git", ["-C", bare, "update-ref", ref, sha], { env: GIT_ENV, stdio: "pipe" });
}

function originHead(): string {
  return execFileSync("git", ["-C", fx.originPath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

/** Resolves once `signal` aborts (or immediately if already aborted). */
function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const trackingRef = (iid: number) => `refs/uzi-runner/agent/issue-${iid}`;

/** A minimal real Worker over the harness client/api, with the chat lane disabled
 *  (chatSessions: 0 makes chatClaimLoop never claim) and a passing toolchain
 *  preflight (this is not an image host). Used only for the within-grace proof that
 *  the fetch-back completes before `worker.run` resolves. */
function buildWorker(runner: RunRunner): Worker {
  const config = {
    workerName: "w1",
    workerTemplate: "base",
    maxConcurrentRuns: 1,
    pollIntervalMs: 1,
    heartbeatIntervalMs: 10_000,
    chatPollMs: 10_000,
    chatSessions: 0,
    dockerWiring: {},
  } as unknown as Config;
  const noChat = { execute: async () => {} } as unknown as ChatRunner;
  const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
  const noReview = { execute: async () => {} } as unknown as ReviewRunner;
  return new Worker(config, client, runner, noChat, noJudge, noReview, nullLogger(), () => ({
    ok: true,
    missing: [],
  }));
}

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

  it("removes the clone but preserves the plugin dir and the run HOME when the server parks the run", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-ok-"));
    try {
      const { factory, paths } = limitFactory(homeRoot, 91);
      await runnerWith(factory, gitlab).execute(
        gitlabClaim(91, { wait_on_limit: true }),
      );
      const p = paths();
      // PRD #218 M6: the clone leg of the carve-out was dropped. M1/M2 moved a
      // parked run's committed work off the clone and into the worker's bare
      // (fetch-back into the tracking ref, validated live on dev-cluster
      // 2026-08-04), so preserving the clone directory buys nothing and it is now
      // removed on a park too. The plugin dir and the run HOME are still preserved:
      // a resume needs the transcript and the plugins, and the reseed recreates the
      // clone at the same path.
      assert.strictEqual(
        fs.existsSync(p.worktree),
        false,
        "runner clone must be removed on a park (PRD #218 M6)",
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
  // The finally holds three FILESYSTEM removals (clone, plugin dir, run HOME); the
  // park carve-out skips two of them (plugin dir + HOME — the clone leg was dropped
  // in PRD #218 M6). The secret eviction is none of the three: the secrets are
  // re-delivered on the next claim, so holding them buys nothing and costs a widened
  // exposure window on a machine that keeps working.
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
      // PRD #69 M7a: a rate-limit opt-out failure carries the trusted origin so the
      // judge can key on the class instead of parsing the server-composed sentence.
      assert.strictEqual(failed.body.fail_origin, "rate_limited");
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

// ── PRD #218 M1/M2/M3 — the park is durable (fetch-back + tracking-ref reseed) ──
//
// Before this, a park preserved the runner clone but the very NEXT claim's reseed
// `fs.rm`d it and re-seeded off origin/default, destroying every commit the parked
// attempt made (it had pushed nothing). M1 fetches the committed work back into the
// worker bare's tracking ref; M2 teaches the reseed to read it on a resume.
describe("RunRunner — durable park (PRD #218 M1/M2/M3)", () => {
  /** A factory whose executor commits `file` in the runner clone, then parks on a
   *  usage limit — the shape the bug needs (work committed, nothing pushed). Captures
   *  the committed sha. */
  function commitThenParkFactory(homeRoot: string, file: string): {
    factory: ExecutorFactory;
    sha: () => string;
  } {
    let sha = "";
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          sha = commitInTree(ctx.worktreePath, file, "work before the park\n");
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          throw new LimitReachedError({
            resetsAtMs: Date.now() + 5 * 3600_000,
            rateLimitType: "five_hour",
          });
        },
      },
    });
    return { factory, sha: () => sha };
  }

  it("M1: fetches the committed work back into the worker bare on park", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-park-"));
    try {
      const iid = 201;
      const { factory, sha } = commitThenParkFactory(homeRoot, "WORK.txt");
      await runnerWith(factory, gitlab).execute(
        gitlabClaim(iid, { wait_on_limit: true }),
      );
      const bare = git.barePathFor(fx.originPath);
      assert.strictEqual(
        shaInBare(bare, trackingRef(iid)),
        sha(),
        "the tracking ref must carry the committed work after a park",
      );
      // And the park itself still happened (the fetch-back must not have undone it).
      assert.ok(
        api.states.some((s) => s.body.status === "limit_wait"),
        "the run still parks",
      );
      // PRD #218 M6: the clone leg of the carve-out was dropped once the fetch-back
      // above proved it makes the committed work durable, so the clone is now
      // removed on a park (the tracking ref, asserted above, is what survives).
      assert.strictEqual(
        fs.existsSync(worktreeDirFor(iid)),
        false,
        "the clone is removed on a park (PRD #218 M6)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("M1: a failed fetch-back still parks the run (best-effort, D4)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-park-boom-"));
    try {
      const iid = 205;
      // A real GitCache whose ONLY broken method is the fetch-back — ensureClone and
      // the reseed must still work so the run gets far enough to park.
      const brokenGit = new GitCache(fx.dataDir, nullLogger());
      brokenGit.fetchAgentBranch = async () => {
        throw new Error("fetch-back boom");
      };
      const { factory } = commitThenParkFactory(homeRoot, "WORK.txt");
      const runner = new RunRunner(
        client,
        brokenGit,
        factory,
        nullLogger(),
        20,
        undefined,
        { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab },
      );
      await runner.execute(gitlabClaim(iid, { wait_on_limit: true }));
      assert.ok(
        api.states.some((s) => s.body.status === "limit_wait"),
        "a park that could not fetch back must still park — a lost park is worse",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("M2/regression: a park with local commits RESUMES with those commits present", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-resume-"));
    try {
      const iid = 202;
      // A resume keeps the SAME run_id (a requeue re-claims the same run row), so both
      // claims carry it — that identity is what the tracking-ref owner stamp matches on.
      const runId = "20200000-0000-4000-8000-000000000202";
      // 1) First attempt: commit + park (writes the tracking ref, stamped with runId).
      const { factory: parkFactory, sha } = commitThenParkFactory(
        homeRoot,
        "WORK.txt",
      );
      await runnerWith(parkFactory, gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true }),
      );

      // 2) Resume (a claim with a session id): the reseed must read the tracking ref
      //    and hand the executor a tree that still carries WORK.txt.
      let sawFile = false;
      let priorWork: unknown;
      let baseCommit: string | undefined;
      const resumeFactory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            sawFile = fs.existsSync(path.join(ctx.worktreePath, "WORK.txt"));
            priorWork = ctx.priorWork;
            baseCommit = ctx.baseCommit;
            return { branch: ctx.branch };
          },
        },
      });
      const resumeClaim = gitlabClaim(iid, {
        run_id: runId,
        wait_on_limit: true,
        session_id: SID,
        // A real resume carries the last seq the run reached before parking; without it
        // the resume's feed seqs would collide with the park run's (same run_id) and the
        // server dedups them, hiding the recovered-count status this test asserts on.
        last_seq: 1000,
      });
      await runnerWith(resumeFactory, gitlab).execute(resumeClaim);

      // The control the bug failed: the file the agent created before the park is
      // still there after it (Success Criterion 1).
      assert.strictEqual(
        sawFile,
        true,
        "the committed file must survive the resume — this is the exact bug",
      );
      assert.strictEqual(
        baseCommit,
        sha(),
        "the resume seeds off the recovered tracking-ref tip",
      );
      // M3: the lead is told the work was recovered (a commit, not a lead noticing).
      assert.deepStrictEqual(
        priorWork,
        { commits: 1 },
        "the lead is told a recovered commit exists on the branch",
      );
      const recovered = api
        .messages(resumeClaim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text))
        .find((t) => /recovered 1 commit/.test(t));
      assert.ok(
        recovered,
        "the feed states the recovered commit count (Success Criterion 3)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("M3: a resume that recovers nothing admits the loss in the feed", async () => {
    // A resume (session id present) on a branch with neither an origin branch nor a
    // tracking ref — the cross-worker case (R1). The reseed falls back to the default
    // branch, and the feed must SAY the tree could not be recovered rather than let
    // the lead discover it.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-loss-"));
    try {
      const iid = 203;
      const seen: RunContext[] = [];
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            seen.push(ctx);
            return { branch: ctx.branch };
          },
        },
      });
      const claim = gitlabClaim(iid, { wait_on_limit: true, session_id: SID });
      await runnerWith(factory, gitlab).execute(claim);
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      assert.ok(
        texts.some((t) => /no earlier work could be recovered/.test(t)),
        `expected an honest tree-loss notice, got ${JSON.stringify(texts)}`,
      );
      assert.strictEqual(
        seen[0]?.priorWork,
        undefined,
        "nothing recovered ⇒ no prior-work note",
      );
      // issue #222: this is the measured harmful case — a resume (session id present) whose
      // reseed fell to the default branch, so priorWork is empty and the feed status is the
      // ONLY reseed signal. The runner must ALSO set ctx.resumed so the reseed warning can
      // ride the implement prompt, the one thing the lead reads.
      assert.strictEqual(
        seen[0]?.resumed,
        true,
        "a resume claim (session id present) is flagged so the reseed warning renders",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("issue #222: a FRESH claim (no session id) is not flagged resumed — no prior tree was lost", async () => {
    // A run that never executed before had no working tree to destroy, so the reseed warning
    // must NOT fire. The discriminator is the raw claim session id.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-222-fresh-"));
    try {
      const iid = 222;
      const seen: RunContext[] = [];
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            seen.push(ctx);
            return { branch: ctx.branch };
          },
        },
      });
      // No session_id ⇒ a fresh run.
      await runnerWith(factory, gitlab).execute(gitlabClaim(iid, {}));
      // Guard against a vacuous pass: the executor must actually have run, else
      // `seen[0]?.resumed !== true` is trivially true on an undefined seen[0].
      assert.strictEqual(seen.length, 1, "the executor must have been invoked exactly once");
      assert.ok(
        seen[0]?.resumed !== true,
        `a fresh claim must not be flagged resumed, got ${String(seen[0]?.resumed)}`,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("M2/anchor: a DIFFERENT run on the same issue does NOT inherit a dead run's orphan ref (auditor counterexample)", async () => {
    // Run A parks and dies, leaving refs/uzi-runner/agent/issue-<iid> stamped with A.
    // Run B is a fresh run on the SAME issue that was killed mid-turn before it could
    // fetch its own work back, then requeued and resumed. Without the run-identity
    // anchor B would seed off A's commits and falsely claim to have recovered its own
    // work. With it, B (a different run_id) does not own A's ref and seeds off default.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-anchor-"));
    try {
      const iid = 250;
      const runA = "25000000-0000-4000-8000-0000000250aa";
      const runB = "25000000-0000-4000-8000-0000000250bb";
      // A: commit + park → tracking ref owned by A.
      const { factory: parkA, sha: shaA } = commitThenParkFactory(homeRoot, "A.txt");
      await runnerWith(parkA, gitlab).execute(
        gitlabClaim(iid, { run_id: runA, wait_on_limit: true }),
      );
      assert.strictEqual(
        shaInBare(git.barePathFor(fx.originPath), trackingRef(iid)),
        shaA(),
        "precondition: A's work is in the tracking ref",
      );

      // B: a resume claim on the same issue, DIFFERENT run_id, which never fetched its
      // own work back.
      let sawA = true;
      let priorWork: unknown;
      const bFactory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            sawA = fs.existsSync(path.join(ctx.worktreePath, "A.txt"));
            priorWork = ctx.priorWork;
            return { branch: ctx.branch };
          },
        },
      });
      const claimB = gitlabClaim(iid, {
        run_id: runB,
        wait_on_limit: true,
        session_id: SID,
      });
      await runnerWith(bFactory, gitlab).execute(claimB);
      assert.strictEqual(
        sawA,
        false,
        "B must NOT inherit A's commits — the ref is anchored to A",
      );
      assert.strictEqual(priorWork, undefined, "and B is told of no recovery");
      const texts = api
        .messages(claimB.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      assert.strictEqual(
        texts.some((t) => /recovered \d+ commit/.test(t)),
        false,
        "B must NOT falsely claim to have recovered A's work",
      );
      assert.ok(
        texts.some((t) => /no earlier work could be recovered/.test(t)),
        "B honestly reports its own tree could not be recovered",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ── PRD #759 M5 — the recovery feed distinguishes WIP from committed ──────────
//
// M5 makes an uncommitted-WIP recovery legible in the run feed, distinct from a
// committed-milestone recovery, and — the ordering crux — keeps the #218 M3 loss
// notice from FALSELY firing on a cross-worker DIVERGED WIP recovery (which leaves
// seededFrom==='default' but wipRecovered===true). Driven on the REAL runner emission:
//   (a)/(c) via a genuinely-dirty park + same-worker resume — the runner's own M1
//     commitWipMarker plants the wip(park): marker, the reseed resets it back to
//     uncommitted, and the emission reads the resulting RunnerClone end to end.
//   (b) via a plain cross-worker resume (no recoverable ref → seededFrom 'default')
//     with wipRecovered forced true on the reseed's return — exactly the shape the
//     git-level diverged leg produces (proved end to end in
//     git-cross-worker-recovery.test.ts). It is the direct A/B of the existing loss
//     test above: same claim, plus wipRecovered, must flip the loss notice OFF and the
//     WIP notice ON. That is the whole ordering fix.
describe("RunRunner — WIP-vs-committed recovery feed (PRD #759 M5)", () => {
  const WIP_MSG =
    /uncommitted work-in-progress from this run's interrupted attempt — a partial snapshot/;
  const LOSS_MSG = /no earlier work could be recovered/;

  const feedTexts = (runId: string): string[] =>
    api
      .messages(runId)
      .filter((m) => m.kind === "status")
      .map((m) => String(m.payload.text));

  /** A factory that leaves a genuinely UNCOMMITTED edit in the clone (no `git add` /
   *  commit — the single deviation from commitThenParkFactory) then parks. The runner's
   *  M1 commitWipMarker turns it into a wip(park): marker, fetched back to the tracking
   *  ref; the same-worker resume's reseed resets it back to uncommitted (wipRecovered). */
  function dirtyThenParkFactory(homeRoot: string, dirty: string): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, dirty), "in-progress, uncommitted\n");
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          throw new LimitReachedError({
            resetsAtMs: Date.now() + 5 * 3600_000,
            rateLimitType: "five_hour",
          });
        },
      },
    });
  }

  /** Like above but ALSO commits `committed` first, so the resume recovers a committed
   *  milestone AND the uncommitted WIP (the "plus your uncommitted work-in-progress"
   *  wording, priorCommits > 0 && wipRecovered). */
  function commitAndDirtyThenParkFactory(
    homeRoot: string,
    committed: string,
    dirty: string,
  ): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitInTree(ctx.worktreePath, committed, "committed milestone\n");
          fs.writeFileSync(path.join(ctx.worktreePath, dirty), "in-progress, uncommitted\n");
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          throw new LimitReachedError({
            resetsAtMs: Date.now() + 5 * 3600_000,
            rateLimitType: "five_hour",
          });
        },
      },
    });
  }

  /** A no-op resume executor that records whether `probe` was restored to the tree. */
  function resumeProbeFactory(
    homeRoot: string,
    probe: string,
    onSeen: (present: boolean) => void,
  ): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          onSeen(fs.existsSync(path.join(ctx.worktreePath, probe)));
          return { branch: ctx.branch };
        },
      },
    });
  }

  it("(a) a PURE uncommitted-WIP recovery says 'partial snapshot' and NOT the loss notice", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-wip-pure-"));
    try {
      const iid = 759;
      const runId = "75900000-0000-4000-8000-0000000759aa";
      // Park with a genuinely dirty (never-committed) tree → wip(park): marker on the
      // tracking ref (no committed work: priorCommits stays 0).
      await runnerWith(dirtyThenParkFactory(homeRoot, "WIP.txt"), gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true }),
      );
      // Same-worker resume: the reseed reset --soft's the marker back to uncommitted.
      // PRD #759 M6: this drives the FULL park→resume flow via LimitReachedError through the
      // runner's park path (not the git.ts reseed in isolation) and asserts the two
      // discriminating properties together, in ONE end-to-end test — the M6 requirement the
      // split coverage (runner (a) had content-present; git-cross-worker-recovery had the
      // marker-free branch) did not satisfy on a single LimitReachedError-driven path:
      //   1. the WIP file CONTENT is present in the resumed clone (pre-fix it is ABSENT — the
      //      reseed fs.rm's the dirty tree and seedsFrom 'default'), and
      //   2. the resumed branch history carries NO wip(park): subject (the marker was
      //      reset --soft'd to uncommitted at adopt time, so it never reaches finalize / the MR).
      let sawWip = false;
      let wipContent: string | null = null;
      let logSubjects: string[] = [];
      await runnerWith(
        (rid) => ({
          homeDir: path.join(homeRoot, rid),
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              const p = path.join(ctx.worktreePath, "WIP.txt");
              sawWip = fs.existsSync(p);
              wipContent = sawWip ? fs.readFileSync(p, "utf8") : null;
              logSubjects = execFileSync(
                "git",
                ["-C", ctx.worktreePath, "log", "--format=%s"],
                { env: GIT_ENV, encoding: "utf8" },
              )
                .trim()
                .split("\n");
              return { branch: ctx.branch };
            },
          },
        }),
        gitlab,
      ).execute(
        gitlabClaim(iid, {
          run_id: runId,
          wait_on_limit: true,
          session_id: SID,
          last_seq: 1000,
        }),
      );
      assert.strictEqual(sawWip, true, "the uncommitted WIP file is restored to the resumed tree");
      // The discriminating assertion (PRD #759 M6): the file's CONTENT is present, not merely
      // "a checkpoint ref appeared". Pre-fix the file is absent, so this reads null and fails.
      assert.strictEqual(
        wipContent,
        "in-progress, uncommitted\n",
        "the pre-park WIP content is restored verbatim to the resumed clone",
      );
      // The finalized branch shows NO wip(park): subject — the marker never enters the history
      // the agent builds on (R4). Non-vacuity: logSubjects is populated (the log ran), so an
      // empty-history false pass cannot slip through.
      assert.ok(logSubjects.length > 0 && logSubjects[0] !== "", "the resumed branch has a real history to inspect");
      assert.ok(
        logSubjects.every((s) => !s.startsWith(WIP_PARK_COMMIT_PREFIX)),
        `no wip(park): commit in the resumed branch history, got ${JSON.stringify(logSubjects)}`,
      );
      const texts = feedTexts(runId);
      assert.ok(
        texts.some((t) => WIP_MSG.test(t)),
        `expected the WIP-recovery notice, got ${JSON.stringify(texts)}`,
      );
      assert.strictEqual(
        texts.some((t) => LOSS_MSG.test(t)),
        false,
        "the loss notice must NOT fire when uncommitted WIP was recovered",
      );
      assert.strictEqual(
        texts.some((t) => /recovered \d+ commit/.test(t)),
        false,
        "a pure-WIP recovery must not claim any committed recovery",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(b) a DIVERGED WIP recovery (seededFrom 'default' + wipRecovered) fires the WIP notice, not the loss notice", async () => {
    // The ordering crux. A cross-worker diverged recovery leaves seededFrom==='default'
    // (the base is the advanced floor, not a checkpoint) with wipRecovered===true. This is
    // the EXACT claim of the existing loss test ("a resume that recovers nothing"), so the
    // ONLY delta is wipRecovered — and it must flip the loss notice OFF and the WIP notice
    // ON. Force wipRecovered on the reseed's return, the git-level diverged leg being proved
    // end to end in git-cross-worker-recovery.test.ts.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-wip-diverged-"));
    const origReseed = git.runnerCloneForBranch.bind(git);
    (git as unknown as { runnerCloneForBranch: unknown }).runnerCloneForBranch = async (
      ...args: unknown[]
    ) => {
      const rc = await (origReseed as unknown as (
        ...a: unknown[]
      ) => Promise<Record<string, unknown>>)(...args);
      // A plain cross-worker resume already yields seededFrom 'default' / priorCommits 0;
      // add the diverged leg's wipRecovered signal.
      assert.strictEqual(rc.seededFrom, "default", "precondition: nothing recoverable → default floor");
      return { ...rc, wipRecovered: true };
    };
    try {
      const iid = 763;
      const claim = gitlabClaim(iid, { wait_on_limit: true, session_id: SID });
      await runnerWith(
        (runId) => ({
          homeDir: path.join(homeRoot, runId),
          executor: { run: async (ctx: RunContext) => ({ branch: ctx.branch }) },
        }),
        gitlab,
      ).execute(claim);
      const texts = feedTexts(claim.run_id);
      assert.ok(
        texts.some((t) => WIP_MSG.test(t)),
        `expected the WIP-recovery notice on the diverged recovery, got ${JSON.stringify(texts)}`,
      );
      assert.strictEqual(
        texts.some((t) => LOSS_MSG.test(t)),
        false,
        "the loss notice must NOT fire on a diverged WIP recovery — the crux of the ordering fix",
      );
    } finally {
      (git as unknown as { runnerCloneForBranch: unknown }).runnerCloneForBranch = origReseed;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(c) a committed + WIP recovery mentions BOTH the commit count and the uncommitted work", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-wip-both-"));
    try {
      const iid = 764;
      const runId = "76400000-0000-4000-8000-0000000764aa";
      // Commit one milestone AND leave an uncommitted edit, then park: the marker sits on
      // top of the committed work, so the resume recovers 1 commit + the WIP.
      await runnerWith(
        commitAndDirtyThenParkFactory(homeRoot, "M1.txt", "WIP.txt"),
        gitlab,
      ).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true }));
      let sawWip = false;
      await runnerWith(
        resumeProbeFactory(homeRoot, "WIP.txt", (p) => {
          sawWip = p;
        }),
        gitlab,
      ).execute(
        gitlabClaim(iid, {
          run_id: runId,
          wait_on_limit: true,
          session_id: SID,
          last_seq: 1000,
        }),
      );
      assert.strictEqual(sawWip, true, "the uncommitted WIP file is restored alongside the committed work");
      const texts = feedTexts(runId);
      assert.ok(
        texts.some((t) =>
          /recovered 1 commit\(s\) plus your uncommitted work-in-progress from this run's interrupted attempt/.test(
            t,
          )),
        `expected the combined committed+WIP notice, got ${JSON.stringify(texts)}`,
      );
      assert.strictEqual(
        texts.some((t) => LOSS_MSG.test(t)),
        false,
        "the loss notice must not fire when work was recovered",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(d) regression: a committed-only recovery keeps the byte-identical pre-M5 wording", async () => {
    // The !wipRecovered branch must be untouched: a park that COMMITS its work (no dirty
    // tree, so no marker) recovers exactly as before. (The pure-loss byte-identical case is
    // covered by "a resume that recovers nothing admits the loss in the feed" above.)
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-committed-only-"));
    try {
      const iid = 765;
      const runId = "76500000-0000-4000-8000-0000000765aa";
      const { factory } = commitThenParkFactoryLocal(homeRoot, "WORK.txt");
      await runnerWith(factory, gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true }),
      );
      await runnerWith(
        resumeProbeFactory(homeRoot, "WORK.txt", () => {}),
        gitlab,
      ).execute(
        gitlabClaim(iid, {
          run_id: runId,
          wait_on_limit: true,
          session_id: SID,
          last_seq: 1000,
        }),
      );
      const texts = feedTexts(runId);
      assert.ok(
        texts.some(
          (t) => t === "recovered 1 commit(s) of work from this run's interrupted attempt",
        ),
        `expected the byte-identical committed-only wording, got ${JSON.stringify(texts)}`,
      );
      assert.strictEqual(
        texts.some((t) => WIP_MSG.test(t)),
        false,
        "a committed-only recovery must not mention uncommitted work-in-progress",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  /** A committing park factory local to this block (a duplicate of the durable-park
   *  block's private helper), so (d) can commit real work without a dirty tree. */
  function commitThenParkFactoryLocal(homeRoot: string, file: string): {
    factory: ExecutorFactory;
  } {
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitInTree(ctx.worktreePath, file, "work before the park\n");
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          throw new LimitReachedError({
            resetsAtMs: Date.now() + 5 * 3600_000,
            rateLimitType: "five_hour",
          });
        },
      },
    });
    return { factory };
  }
});

// ── PRD #759 M4 — the runner-level resume-reviewed-plan predicate ─────────────
//
// embedSeededPlan (sdk-executor.test.ts) pins the plan-BODY gate as a pure function;
// these pin the RUNNER's m4ResumeReviewedPlan → { planApproved, reviewedPlanResume }
// resolution — the layer that owns the facts (dropped session, recovery success, the
// plan_source allowlist, and the human-approved-recovery-failed re-gate). Each captures
// the RunContext the executor receives, exactly as runner-seeded-plan.test.ts does for
// the D4 rows; the question here is only what the runner RESOLVES, not what the run then
// does (the capturing executor never calls gatePlan, so ctx.planApproved is read raw).
//
// Calibrated against the safety clauses the PRD names (R2, D4):
//   (a) reddens if `m4ResumeReviewedPlan` is removed entirely (planApproved → false).
//   (b) reddens if the `!(humanApproved && recoveryFailed)` re-gate clause is removed
//       (planApproved would flip to true, re-implementing onto a lost human-approved tree).
//   (c) reddens if the guard wrongly re-gated an autopilot recovery-failed resume.
//   (d) reddens if the `plan_source === "agent"` allowlist is loosened to `!== "seeded"`
//       (a future unreviewed provenance would then fail OPEN — R2's exact warning).
describe("RunRunner — M4 resume-reviewed-plan predicate (PRD #759 M4)", () => {
  const M4_SID = "aaaaaaaa-bbbb-cccc-dddd-ffffffffffff";

  /** Park factory: commit `file` in the clone then park — leaves a tracking ref owned by
   *  the run, which the same-worker resume recovers (seededFrom 'tracking' ⇒ recovery
   *  SUCCEEDED, recoveryFailed=false). */
  function commitThenParkM4(homeRoot: string, file: string): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitInTree(ctx.worktreePath, file, "committed before the park\n");
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          throw new LimitReachedError({
            resetsAtMs: Date.now() + 5 * 3600_000,
            rateLimitType: "five_hour",
          });
        },
      },
    });
  }

  /** A capturing resume factory. No transcript is planted under its per-run HOME, so the
   *  issue #105 preflight DROPS the session — the dropped-session (cross-worker) shape M4
   *  keys on. It records the RunContext and returns without calling the gate. */
  function capturingM4(homeRoot: string, seen: RunContext[]): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen.push(ctx);
          return { branch: ctx.branch };
        },
      },
    });
  }

  it("(a) recovery-succeeded human-approved dropped-session agent run is approved without a re-gate", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-m4-ok-"));
    try {
      const iid = 770;
      const runId = "77000000-0000-4000-8000-0000000770aa";
      // Park with a committed milestone → a tracking ref owned by runId.
      await runnerWith(commitThenParkM4(homeRoot, "WORK.txt"), gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true }),
      );
      // Resume: SAME run_id, a dropped session (session_id present, no transcript), an
      // agent-provenance approved plan, NOT autopilot (a human saw the gate). Recovery
      // succeeds off the tracking ref, so the re-gate fallback does not fire.
      const seen: RunContext[] = [];
      await runnerWith(capturingM4(homeRoot, seen), gitlab).execute(
        gitlabClaim(iid, {
          run_id: runId,
          wait_on_limit: true,
          session_id: M4_SID,
          last_seq: 1000,
          plan_approved: true,
          plan_source: "agent",
          plan_md: "# reviewed plan\n- ship it",
          auto_approve: false,
        }),
      );
      assert.strictEqual(seen[0]?.sessionId, undefined, "the cross-worker transcript was dropped by the preflight");
      // Precondition — recovery genuinely SUCCEEDED (else planApproved would be false via the
      // re-gate fallback and the positive assertions below would be vacuous).
      assert.deepStrictEqual(seen[0]?.priorWork, { commits: 1 }, "the parked commit was recovered off the tracking ref");
      assert.strictEqual(seen[0]?.planApproved, true, "a provably-reviewed recovered resume is approved (no re-plan)");
      assert.strictEqual(seen[0]?.reviewedPlanResume, true, "reviewedPlanResume drives the plan-body embed + gate skip");
      assert.strictEqual(seen[0]?.seeded, false, "an agent-provenance plan is not seeded");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(b) human-approved but recovery-FAILED dropped-session run RE-GATES (the loss-detection gate)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-m4-regate-"));
    try {
      // A fresh iid ⇒ no tracking ref, so the dropped-session reseed falls to the default
      // branch (seededFrom 'default' ⇒ recovery FAILED, empty tree).
      const iid = 771;
      const seen: RunContext[] = [];
      await runnerWith(capturingM4(homeRoot, seen), gitlab).execute(
        gitlabClaim(iid, {
          wait_on_limit: true,
          session_id: M4_SID,
          plan_approved: true,
          plan_source: "agent",
          plan_md: "# reviewed plan\n- ship it",
          auto_approve: false, // a human saw the gate
        }),
      );
      assert.strictEqual(seen[0]?.sessionId, undefined, "dropped session");
      assert.strictEqual(seen[0]?.priorWork, undefined, "precondition: recovery FAILED (empty tree)");
      assert.strictEqual(
        seen[0]?.planApproved,
        false,
        "a human-approved run that lost its tree RE-GATES — removing !(humanApproved && recoveryFailed) reddens this",
      );
      assert.strictEqual(seen[0]?.reviewedPlanResume, false);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(b2) human-approved DIVERGED-leg WIP recovery (seededFrom 'default' + wipRecovered) RESUMES, not re-gates", async () => {
    // The exact claim of (b) — human-approved, dropped session, seededFrom 'default' — but the
    // diverged cross-worker cherry-pick leg recovered the WIP onto the advanced floor, so
    // wipRecovered is true. ADR-0759 and the reseed-feed path both call that a successful
    // recovery, so the #209 loss-detection re-gate must NOT fire: the human's work came back.
    // The ONLY delta from (b) is wipRecovered, and it must flip planApproved/reviewedPlanResume
    // from false to true. This pins the recoveryFailed predicate — dropping its
    // `&& wipRecovered !== true` clause (so a recovered-WIP run is treated as a lost tree)
    // reddens this. Force wipRecovered on the reseed's return exactly as the feed test does; the
    // git-level diverged leg is proved end to end in git-cross-worker-recovery.test.ts.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-m4-wip-resume-"));
    const origReseed = git.runnerCloneForBranch.bind(git);
    (git as unknown as { runnerCloneForBranch: unknown }).runnerCloneForBranch = async (
      ...args: unknown[]
    ) => {
      const rc = await (origReseed as unknown as (
        ...a: unknown[]
      ) => Promise<Record<string, unknown>>)(...args);
      assert.strictEqual(rc.seededFrom, "default", "precondition: nothing committed recoverable → default floor");
      return { ...rc, wipRecovered: true };
    };
    try {
      const iid = 774;
      const seen: RunContext[] = [];
      await runnerWith(capturingM4(homeRoot, seen), gitlab).execute(
        gitlabClaim(iid, {
          wait_on_limit: true,
          session_id: M4_SID,
          plan_approved: true,
          plan_source: "agent",
          plan_md: "# reviewed plan\n- ship it",
          auto_approve: false, // a human saw the gate
        }),
      );
      assert.strictEqual(seen[0]?.sessionId, undefined, "dropped session");
      assert.strictEqual(seen[0]?.wipRecovered, true, "precondition: the diverged leg recovered the WIP");
      assert.strictEqual(
        seen[0]?.planApproved,
        true,
        "a human-approved run whose WIP recovered on the diverged leg RESUMES — dropping `&& wipRecovered !== true` from recoveryFailed reddens this",
      );
      assert.strictEqual(seen[0]?.reviewedPlanResume, true);
    } finally {
      (git as unknown as { runnerCloneForBranch: unknown }).runnerCloneForBranch = origReseed;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(c) autopilot recovery-FAILED dropped-session resume still RESUMES (no human gate to protect)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-m4-auto-"));
    try {
      const iid = 772;
      const seen: RunContext[] = [];
      await runnerWith(capturingM4(homeRoot, seen), gitlab).execute(
        gitlabClaim(iid, {
          wait_on_limit: true,
          session_id: M4_SID,
          plan_approved: true,
          plan_source: "agent",
          plan_md: "# reviewed plan\n- ship it",
          auto_approve: true, // autopilot: no human ever saw the gate
        }),
      );
      assert.strictEqual(seen[0]?.sessionId, undefined, "dropped session");
      assert.strictEqual(seen[0]?.priorWork, undefined, "recovery FAILED, but autopilot has no human gate to protect");
      assert.strictEqual(
        seen[0]?.planApproved,
        true,
        "an autopilot recovery-failed resume implements from its plan (the re-gate fallback needs a HUMAN)",
      );
      assert.strictEqual(seen[0]?.reviewedPlanResume, true);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(d) a future non-agent, non-seeded provenance FAILS CLOSED even when recovery would resume (R2 allowlist)", async () => {
    // Models a server sending a plan_source the worker's enum does not yet know — a future
    // unreviewed provenance. Same shape as (c) (autopilot, recovery-failed, so the re-gate
    // clause is satisfied and cannot be the thing that keeps it out); the ONLY discriminator
    // is the provenance. The POSITIVE allowlist `=== "agent"` rejects it, so planApproved is
    // false; loosening it to `!== "seeded"` would fail OPEN and flip this to true.
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-759-m4-fail-closed-"));
    try {
      const iid = 773;
      const seen: RunContext[] = [];
      await runnerWith(capturingM4(homeRoot, seen), gitlab).execute(
        gitlabClaim(iid, {
          wait_on_limit: true,
          session_id: M4_SID,
          plan_approved: true,
          // A provenance value outside the {agent, seeded} enum the worker knows today.
          plan_source: "future_unreviewed_provenance",
          plan_md: "# an UNREVIEWED worker plan\n- do it",
          auto_approve: true,
        }),
      );
      assert.strictEqual(seen[0]?.sessionId, undefined, "dropped session");
      assert.strictEqual(seen[0]?.seeded, false, "not seeded");
      assert.strictEqual(
        seen[0]?.planApproved,
        false,
        "an unknown provenance is NOT on the agent allowlist ⇒ re-gate; loosening to !== 'seeded' reddens this",
      );
      assert.strictEqual(seen[0]?.reviewedPlanResume, false);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ── PRD #218 M2 — the reseed's per-leg base resolution (git level) ────────────
//
// The two legs a UNIFORM ancestor test gets wrong are the point: a first park with a
// MOVED default (tracking must win) and a pushed branch that DIVERGED (origin must
// win). Driven at the git layer, where the resolution lives, with `runId` as the
// run-identity anchor the runner threads from `claim.run_id`. `fetchAgentBranch` stamps
// the writing run's id, so passing the SAME id to the reseed makes the ref "owned here".
describe("GitCache.runnerCloneForBranch — tracking-ref reseed (PRD #218 M2)", () => {
  const RUN = "run-owner-aaaa-bbbb-cccc-dddddddddddd";

  it("first park + MOVED default branch: the tracking ref wins, with no ancestry test", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Commit locally off the fork point and fetch it back (a first park — never pushed).
    const rc = await git.createOrAttachRunnerClone(bare, 210, RUN);
    const local = commitInTree(rc.path, "LOCAL.txt", "local\n");
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-210", RUN);
    await git.removeRunnerClone(rc.path);

    // The default branch moves PAST the fork point, then the bare is refreshed.
    const moved = commitInTree(fx.originPath, "MOVED.txt", "moved\n");
    await git.ensureClone(fx.originPath);

    const resumed = await git.runnerCloneForBranch(
      bare,
      "agent/issue-210",
      "issue-210",
      RUN,
    );
    assert.strictEqual(resumed.seededFrom, "tracking");
    assert.strictEqual(
      resumed.baseCommit,
      local,
      "the tracking ref wins even though the default moved past the fork",
    );
    assert.notStrictEqual(resumed.baseCommit, moved);
    assert.strictEqual(
      fs.existsSync(path.join(resumed.path, "LOCAL.txt")),
      true,
    );
    // The control: a uniform `--is-ancestor moved local` would FAIL (moved is not an
    // ancestor of local), so a single uniform ancestor test would have discarded the
    // recovered work — exactly the case D2 excludes the first-park leg from.
    assert.strictEqual(
      shaInBare(bare, trackingRef(210)),
      local,
      "the tracking ref still points at the recovered work",
    );
  });

  it("resume with a pushed branch that DIVERGED: origin wins (never drop a published commit)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Push O1 to origin's agent/issue-211 (this fetch-back stamps the owner = RUN).
    const first = await git.createOrAttachRunnerClone(bare, 211, RUN);
    const o1 = commitInTree(first.path, "ORIGIN.txt", "origin\n");
    await git.fetchAgentBranch(bare, first.path, "agent/issue-211", RUN);
    await git.pushBranch(bare, "agent/issue-211", "", fx.originPath);
    await git.removeRunnerClone(first.path);
    await git.ensureClone(fx.originPath); // learn origin/agent/issue-211 = O1

    // A DIVERGED tracking ref: a commit off main that is NOT a descendant of O1. Bring
    // l1 into the bare via a throwaway branch, then repoint issue-211's ref at it; the
    // owner stamp for issue-211 stays RUN (a different branch's fetch-back does not touch
    // it), so RUN still owns the (now diverged) ref.
    const tmp = await git.createOrAttachRunnerClone(bare, 2110, "run-tmp");
    const l1 = commitInTree(tmp.path, "LOCAL.txt", "local\n");
    await git.fetchAgentBranch(bare, tmp.path, "agent/issue-2110", "run-tmp");
    await git.removeRunnerClone(tmp.path);
    gitUpdateRef(bare, trackingRef(211), l1);

    const resumed = await git.runnerCloneForBranch(
      bare,
      "agent/issue-211",
      "issue-211",
      RUN,
    );
    assert.strictEqual(resumed.seededFrom, "origin");
    assert.strictEqual(
      resumed.baseCommit,
      o1,
      "on divergence origin wins — a published commit is never dropped",
    );
  });

  it("resume where the tracking ref DESCENDS from origin: tracking wins and the base moves past origin (Success Criterion 4)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 212, RUN);
    const o1 = commitInTree(rc.path, "O.txt", "o\n");
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-212", RUN);
    await git.pushBranch(bare, "agent/issue-212", "", fx.originPath);
    // Build ON TOP of the pushed tip (descends), fetch that back — do NOT push it.
    const l1 = commitInTree(rc.path, "L.txt", "l\n");
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-212", RUN);
    await git.removeRunnerClone(rc.path);
    await git.ensureClone(fx.originPath); // origin/agent/issue-212 stays at O1

    const resumed = await git.runnerCloneForBranch(
      bare,
      "agent/issue-212",
      "issue-212",
      RUN,
    );
    assert.strictEqual(resumed.seededFrom, "tracking");
    assert.strictEqual(
      resumed.baseCommit,
      l1,
      "the tracking ref wins when it strictly descends from origin",
    );
    assert.notStrictEqual(
      resumed.baseCommit,
      o1,
      "the base legitimately MOVES past origin/<branch> — asserting base==origin here would pin the bug",
    );
    assert.strictEqual(fs.existsSync(path.join(resumed.path, "L.txt")), true);
  });

  it("a DIFFERENT run does NOT own a stale tracking ref, so the reseed ignores it (issue #105 reintroduction guard)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // A tracking ref a permanently-dead run (run-dead) left behind, no origin branch.
    const tmp = await git.createOrAttachRunnerClone(bare, 213, "run-dead");
    commitInTree(tmp.path, "STALE.txt", "stale\n");
    await git.fetchAgentBranch(bare, tmp.path, "agent/issue-213", "run-dead");
    await git.removeRunnerClone(tmp.path);
    assert.ok(shaInBare(bare, trackingRef(213)), "precondition: the stale ref exists");

    // A different run (run-fresh) reseeds the same branch — it does not own the ref.
    const fresh = await git.runnerCloneForBranch(
      bare,
      "agent/issue-213",
      "issue-213",
      "run-fresh",
    );
    assert.strictEqual(
      fresh.seededFrom,
      "default",
      "a run that did not write the ref must never inherit it",
    );
    assert.strictEqual(fresh.baseCommit, originHead());
    assert.strictEqual(fs.existsSync(path.join(fresh.path, "STALE.txt")), false);

    // And a no-runId call (the pre-#218 createOrAttachRunnerClone test call sites) also
    // ignores the ref — undefined never owns.
    const noOwner = await git.runnerCloneForBranch(
      bare,
      "agent/issue-213",
      "issue-213",
    );
    assert.strictEqual(noOwner.seededFrom, "default");
  });
});

// ── PRD #218 M1 — the same fetch-back on the WORKER-SHUTDOWN path ──────────────
//
// The larger loss: a requeue re-claims and the reseed's unconditional `fs.rm`
// destroys committed work with no usage limit involved. A graceful SIGTERM aborts each
// run; its catch fetches the work back and leaves the run NON-terminal so the sweeper
// requeues it onto the recovered tree. The discriminator is the shutdown FLAG, never
// the error (a user cancel raises the same one).
describe("RunRunner — worker-shutdown fetch-back (PRD #218 M1)", () => {
  /** An executor that commits `file` in the clone, signals it started, waits for its
   *  signal to abort, then throws. `commitOnKill` defers the commit into killAgentTree
   *  so a test can prove the reap ran before the fetch-back (R5). */
  function shutdownExecutorFactory(
    homeRoot: string,
    file: string,
    opts: { commitOnKill?: boolean } = {},
  ): {
    factory: ExecutorFactory;
    started: Promise<void>;
    sha: () => string;
    killed: () => boolean;
  } {
    const start = deferred();
    let sha = "";
    let killed = false;
    let clonePath = "";
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          clonePath = ctx.worktreePath;
          if (!opts.commitOnKill) sha = commitInTree(ctx.worktreePath, file, "wip\n");
          start.resolve();
          await waitAbort(ctx.signal!);
          throw new Error("aborted mid-run");
        },
        killAgentTree: () => {
          killed = true;
          if (opts.commitOnKill && !sha) sha = commitInTree(clonePath, file, "wip\n");
        },
      },
    });
    return { factory, started: start.promise, sha: () => sha, killed: () => killed };
  }

  it("(a)+(b): a SIGTERM run fetches its work back WITHIN the grace and is NOT failed", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-sig-"));
    try {
      const iid = 220;
      const { factory, started, sha } = shutdownExecutorFactory(homeRoot, "SHUT.txt");
      const runner = runnerWith(factory, gitlab);
      const worker = buildWorker(runner);
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      api.enqueueClaim(claim);
      const controller = new AbortController();
      const done = worker.run(controller.signal);
      await started;
      // The signal handler's order (main.ts): shutdown() first (no git), then abort.
      runner.shutdown();
      controller.abort();
      await done; // process exit is gated on this; the drain awaits the fetch-back.
      const bare = git.barePathFor(fx.originPath);
      assert.strictEqual(
        shaInBare(bare, trackingRef(iid)),
        sha(),
        "the committed work is in the tracking ref the MOMENT worker.run resolves",
      );
      assert.strictEqual(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "failed",
        ),
        false,
        "a shutdown interruption must requeue, never fail",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(c): reaps the agent tree BEFORE fetching back (R5 ordering)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-r5-"));
    try {
      const iid = 221;
      // The commit is made INSIDE killAgentTree, so it can only reach the tracking ref
      // if the reap ran before the fetch-back.
      const { factory, started, sha, killed } = shutdownExecutorFactory(
        homeRoot,
        "SHUT.txt",
        { commitOnKill: true },
      );
      const runner = runnerWith(factory, gitlab);
      const p = runner.execute(gitlabClaim(iid, { wait_on_limit: true }));
      await started;
      runner.shutdown();
      await p;
      assert.strictEqual(killed(), true, "killAgentTree must run on the shutdown path");
      assert.strictEqual(
        shaInBare(git.barePathFor(fx.originPath), trackingRef(iid)),
        sha(),
        "the kill-time commit reached the tracking ref ⇒ reap ran before fetch",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(d): a throwing fetch-back still requeues (best-effort)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-d-"));
    try {
      const iid = 222;
      const brokenGit = new GitCache(fx.dataDir, nullLogger());
      brokenGit.fetchAgentBranch = async () => {
        throw new Error("fetch-back boom");
      };
      const { factory, started } = shutdownExecutorFactory(homeRoot, "SHUT.txt");
      const runner = new RunRunner(
        client,
        brokenGit,
        factory,
        nullLogger(),
        20,
        undefined,
        { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab },
      );
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p; // resolves, does not throw
      assert.strictEqual(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "failed",
        ),
        false,
        "a failed fetch-back must not turn the requeue into a failure",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(e): a user steering-cancel is UNCHANGED — it fails, it does not requeue", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-e-"));
    try {
      const iid = 223;
      // Same abort, same error, but shuttingDown is false — the flag is the whole
      // discriminator, so this must land on today's generic failure path.
      const { factory, started } = shutdownExecutorFactory(homeRoot, "SHUT.txt");
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      const p = runner.execute(claim);
      await started;
      api.setInputs(claim.run_id, [input("cancel")]);
      await p;
      assert.strictEqual(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "failed",
        ),
        true,
        "a user cancel still fails the run (unchanged behaviour)",
      );
      assert.strictEqual(
        shaInBare(git.barePathFor(fx.originPath), trackingRef(iid)),
        null,
        "no fetch-back on a plain cancel — the tracking ref must not appear",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(f): a run that COMPLETES naturally during shutdown reports completed, not requeued", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-f-"));
    try {
      const iid = 224;
      const started = deferred();
      const proceed = deferred();
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            commitInTree(ctx.worktreePath, "DONE.txt", "done\n");
            started.resolve();
            await proceed.promise; // ignores the abort — finishes normally
            return { branch: ctx.branch };
          },
        },
      });
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      const p = runner.execute(claim);
      await started.promise;
      runner.shutdown(); // marks + aborts, but the executor ignores it and returns
      proceed.resolve();
      await p;
      assert.strictEqual(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "completed",
        ),
        true,
        "a natural completion during shutdown still completes (flag+throw is the discriminator)",
      );
      assert.strictEqual(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "failed",
        ),
        false,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(g): two in-flight runs each snapshot on ONE shutdown", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-g-"));
    try {
      const both = barrier(2);
      const shas = new Map<string, string>();
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            shas.set(ctx.branch, commitInTree(ctx.worktreePath, "SHUT.txt", ctx.branch));
            both.arrive();
            await waitAbort(ctx.signal!);
            throw new Error("aborted mid-run");
          },
        },
      });
      const runner = runnerWith(factory, gitlab);
      const p1 = runner.execute(gitlabClaim(230, { wait_on_limit: true }));
      const p2 = runner.execute(gitlabClaim(231, { wait_on_limit: true }));
      await both.ready; // both committed and registered
      runner.shutdown(); // one shutdown aborts both
      await Promise.all([p1, p2]);
      const bare = git.barePathFor(fx.originPath);
      assert.strictEqual(
        shaInBare(bare, trackingRef(230)),
        shas.get("agent/issue-230"),
        "run 230 fetched back",
      );
      assert.strictEqual(
        shaInBare(bare, trackingRef(231)),
        shas.get("agent/issue-231"),
        "run 231 fetched back on the same shutdown",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(h): deregisters on terminal, and a run registering mid-shutdown aborts at once (late-register guard)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-218-h-"));
    try {
      // Part 1 — no registry leak after an ordinary completed run.
      const doneFactory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => ({
            branch: ctx.branch,
          }),
        },
      });
      const doneRunner = runnerWith(doneFactory, gitlab);
      await doneRunner.execute(gitlabClaim(240, { wait_on_limit: true }));
      const doneActive = (
        doneRunner as unknown as { activeRuns: Map<string, unknown> }
      ).activeRuns;
      assert.strictEqual(doneActive.size, 0, "no registry entry leaks after a terminal run");

      // Part 2 — a run that registers AFTER shutdown() has fired: the late-register
      // guard must abort it at once so it snapshots its committed work rather than
      // running to completion past the grace window.
      const iid = 241;
      const { factory, sha } = shutdownExecutorFactory(homeRoot, "LATE.txt");
      const lateRunner = runnerWith(factory, gitlab);
      lateRunner.shutdown(); // global flag set before any run registers
      await lateRunner.execute(gitlabClaim(iid, { wait_on_limit: true }));
      assert.strictEqual(
        shaInBare(git.barePathFor(fx.originPath), trackingRef(iid)),
        sha(),
        "a run that starts during shutdown still snapshots its work",
      );
      const lateActive = (
        lateRunner as unknown as { activeRuns: Map<string, unknown> }
      ).activeRuns;
      assert.strictEqual(lateActive.size, 0, "and deregisters afterwards");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
