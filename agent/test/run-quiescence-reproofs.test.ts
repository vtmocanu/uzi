import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { REASON_WORKER_RESIDUE_BLOCKED, type ExecutorFactory, type RunRunner } from "../src/runner.js";
import type { StateRequest } from "../src/protocol.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import type { CheckRunner } from "../src/self-improve.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { makeClaim, nullLogger } from "./helpers.js";
import {
  api,
  client,
  deferred,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

// issue #1783 (reviewer, round 3) — the RE-PROOFS a sink runs after its own runner-clone git or
// agent-authored code, and the mapping of each blocked re-proof onto the sink's outcome, are
// pinned here, one site per test: the limit park (`park`) and the pause park (`pause_park`) after
// their wip marker; the graceful shutdown (`shutdown`) after its wip marker; the restore-point
// captures (`recovery_capture`, `credential_switch`) and the settle transfer (`settle_transfer`)
// after their status read / wip marker; the completion hold and the wall park (`hold_capture`)
// after theirs; and finalize after its base-align (`finalize_align`) and after the self-improve
// install + checks (`finalize_checks`, a full proof that also repeats the Docker teardown). The
// quiescer is SITE-aware: it answers `survivors` only at ONE re-proof site
// (`<site>:after_runner_git`) and `quiescent` everywhere else, so the first proof of each sink
// passes and ONLY the re-proof can stop the credentialed step. Removing the re-proof (or its
// mapping) turns each test red. (run-quiescence-planted-filter.test.ts additionally runs a real
// planted filter through the limit, pause and wall parks and finalize's base-align.)

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };
/** A Docker endpoint the injected quiescer never dials: a re-proof handed it runs the Docker half. */
const FAKE_DOCKER_HOST = "unix:///nonexistent/uzi-test-docker.sock";

/**
 * Answers `survivors` at `blockSite` for its first `times` calls there, `quiescent` otherwise.
 * `lastBlocked()` is true while the most recent answer was the blocked one, so a spy can tell a
 * credentialed step that ran right after a blocked re-proof.
 */
function siteQuiescer(blockSite: string, times = Infinity) {
  const calls: QuiesceRunRequest[] = [];
  let blockedAt = 0;
  let last: "survivors" | "quiescent" = "quiescent";
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    const block = req.site === blockSite && blockedAt < times;
    if (block) blockedAt += 1;
    last = block ? "survivors" : "quiescent";
    return {
      process: {
        state: last,
        processes: block ? [{ pid: 4242, uid: 10002, comm: "sleep", cwd: req.targetPaths[0]!, reason: "own_attempt:kill_unconfirmed" }] : [],
        killed: [],
        detail: `scripted ${last} at ${req.site}`,
      },
      docker: { state: "not_wired", removed: [], detail: "" },
    };
  };
  return {
    calls,
    quiesceRun,
    sites: () => calls.map((c) => c.site),
    blockedCount: () => blockedAt,
    lastBlocked: () => last === "survivors",
  };
}

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}

function trackingSha(iid: number): string | null {
  try {
    return gitIn(git.barePathFor(fx.originPath), ["rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`]);
  } catch {
    return null;
  }
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

function spyPublish(): { calls: () => number } {
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n };
}

/** Count fetch-backs that ran right after a blocked answer (none may). */
function spyFetchBack(lastBlocked: () => boolean): { early: () => number; total: () => number } {
  let early = 0;
  let total = 0;
  const orig = git.fetchAgentBranch.bind(git);
  git.fetchAgentBranch = (async (...a: Parameters<typeof git.fetchAgentBranch>) => {
    total += 1;
    if (lastBlocked()) early += 1;
    return orig(...a);
  }) as typeof git.fetchAgentBranch;
  return { early: () => early, total: () => total };
}

const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));
const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

// ─── limit park / pause park: the re-proof after the wip marker ────────────────────────────

describe("issue #1783: the limit park re-proves after its wip marker", () => {
  it("blocked at park:after_runner_git: the park stands (limit_wait), nothing fetched back or published", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1878;
    const q = siteQuiescer("park:after_runner_git");
    const pub = spyPublish();
    const fetch = spyFetchBack(q.lastBlocked);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in progress\n");
          const { LimitReachedError } = await import("../src/limit.js");
          throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
        },
      },
    });
    const claim = gitlabClaim(iid, { wait_on_limit: true });
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.ok(q.sites().includes("park"), "the first proof ran (and passed)");
    assert.equal(q.blockedCount(), 1, "the re-proof after the wip marker ran and blocked");
    assert.ok(statuses(claim.run_id).includes("limit_wait"), "the park stands");
    assert.equal(fetch.total(), 0, "no fetch-back");
    assert.equal(pub.calls(), 0, "no checkpoint published");
    assert.ok(
      statusTexts(claim.run_id).some((t) => t.startsWith("park checkpoint NOT published")),
      JSON.stringify(statusTexts(claim.run_id)),
    );
    assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
  });
});

describe("issue #1783: the pause park re-proves after its wip marker", () => {
  it("blocked at pause_park:after_runner_git: pause_failed (never paused), the marker undone, nothing fetched back or published", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1879;
    const q = siteQuiescer("pause_park:after_runner_git");
    const pub = spyPublish();
    const fetch = spyFetchBack(q.lastBlocked);
    let undos = 0;
    const origUndo = git.undoWipMarker.bind(git);
    git.undoWipMarker = (async (...a: Parameters<typeof git.undoWipMarker>) => {
      undos += 1;
      return origUndo(...a);
    }) as typeof git.undoWipMarker;
    const parked: Array<boolean | undefined> = [];
    const after: { head?: string; status?: string; fetches?: number; publishes?: number; tracking?: string | null } = {};
    let trackingBefore: string | null | undefined;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in progress\n");
          trackingBefore = trackingSha(iid);
          const p = await ctx.parkForPause?.({ completedCount: 1, total: 2 });
          parked.push(p);
          // Observed right after the declined park, before the run goes on to finalize.
          after.head = gitIn(ctx.worktreePath, ["log", "-1", "--format=%s"]);
          after.status = gitIn(ctx.worktreePath, ["status", "--porcelain"]);
          after.fetches = fetch.total();
          after.publishes = pub.calls();
          after.tracking = trackingSha(iid);
          return { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.deepEqual(parked, [false], "the pause did not park");
    assert.ok(q.sites().includes("pause_park"), "the first proof ran (and passed)");
    assert.equal(q.blockedCount(), 1, "the re-proof after the wip marker ran and blocked");
    const st = statuses(claim.run_id);
    assert.ok(st.includes("pause_failed"), `pause_failed reported: ${st.join(",")}`);
    assert.ok(!st.includes("paused"), "never paused");
    assert.equal(undos, 1, "the wip marker was undone");
    assert.equal(after.head, "work", "HEAD is the agent's own commit, not a wip(park): marker");
    assert.match(after.status ?? "", /^\?\? UNCOMMITTED\.txt$/m, "the marker's content is back in the uncommitted tree");
    assert.equal(after.fetches, 0, "no fetch-back");
    assert.equal(fetch.early(), 0, "no fetch-back right after the blocked re-proof");
    assert.equal(after.publishes, 0, "no checkpoint published");
    assert.equal(after.tracking, trackingBefore, "the tracking ref is unchanged");
    const reproof = q.calls.find((c) => c.site === "pause_park:after_runner_git");
    assert.equal(reproof?.dockerHost, undefined, "a re-proof after a plain runner git is process-only");
  });
});

// ─── graceful shutdown: the re-proof after the wip marker ──────────────────────────────────

describe("issue #1783: the shutdown sink re-proves after its wip marker", () => {
  it("blocked at shutdown:after_runner_git: nothing published (boundary_blocked), no fetch-back, the clone kept", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1880;
    const q = siteQuiescer("shutdown:after_runner_git");
    const pub = spyPublish();
    const started = deferred();
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in progress\n");
          started.resolve();
          await waitAbort(ctx.signal!);
          throw new Error("aborted mid-run");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun });
    const claim = gitlabClaim(iid);
    const p = runner.execute(claim);
    await started.promise;
    runner.shutdown();
    await p;
    assert.ok(q.sites().includes("shutdown"), "the first proof ran (and passed)");
    assert.equal(q.blockedCount(), 1, "the re-proof after the wip marker ran and blocked");
    assert.equal(pub.calls(), 0, "no checkpoint published");
    assert.ok(
      statusTexts(claim.run_id).some((t) => t.startsWith("shutdown checkpoint NOT published (reason: boundary_blocked)")),
      JSON.stringify(statusTexts(claim.run_id)),
    );
    assert.equal(trackingSha(iid), null, "no fetch-back into the tracking ref");
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
  });
});

// ─── restore-point captures: recovery_capture and credential_switch ────────────────────────

describe("issue #1783: the restore-point capture re-proves after its status read / wip marker", () => {
  it("recovery_capture: a blocked re-proof is the capture-failure branch (no fetch-back), retried until it passes", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1881;
    const q = siteQuiescer("recovery_capture:after_runner_git", 2);
    const pub = spyPublish();
    const fetch = spyFetchBack(q.lastBlocked);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          throw new TransientRecoveryError();
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.equal(q.blockedCount(), 2, "both blocked re-proofs ran");
    assert.equal(fetch.early(), 0, "no fetch-back right after a blocked re-proof");
    assert.ok(
      statusTexts(claim.run_id).some((t) => t.startsWith("Recovery checkpoint could not be verified")),
      "the blocked re-proof took the existing capture-failure branch",
    );
    assert.ok(statuses(claim.run_id).includes("recovery_wait"), "parked once the re-proof passed");
    assert.ok(pub.calls() >= 1, "the capture published once verified");
  });

  it("credential_switch: a blocked re-proof never releases the switch (credential_switch_failed), nothing fetched or published", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1882;
    const q = siteQuiescer("credential_switch:after_runner_git");
    const pub = spyPublish();
    const fetch = spyFetchBack(q.lastBlocked);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          throw new CredentialSwitchSignal();
        },
      },
    });
    const claim = gitlabClaim(iid);
    api.overrideStateStatus(claim.run_id, "queued");
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    const st = statuses(claim.run_id);
    assert.ok(q.blockedCount() >= 1, "the re-proof ran and blocked");
    assert.ok(st.includes("credential_switch_failed"), `the capture-failure give-up: ${st.join(",")}`);
    assert.ok(!st.includes("credential_switch"), "never released without a verified capture");
    assert.equal(fetch.early(), 0, "no fetch-back right after a blocked re-proof");
    assert.equal(pub.calls(), 0, "no publish");
    assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
  });
});

// ─── settle transfer ──────────────────────────────────────────────────────────────────────

/** An enabled recovery coordinator stand-in that counts the credentialed capture. */
function fakeRecovery(): { recovery: RecoveryCoordinator; captures: () => number } {
  let captures = 0;
  const recovery = {
    enabled: true,
    pin: async (input: { runId: string; sourceSha: string; branch: string; generation?: number }) => ({
      version: 1,
      runId: input.runId,
      captureId: "cap-1",
      sourceSha: input.sourceSha,
      kind: "issue",
      branch: input.branch,
      generation: input.generation,
    }),
    captureAndUpload: async () => {
      captures += 1;
      return { state: "needs_action", captureId: "cap-1", reason: "test" };
    },
    release: async () => undefined,
    inventoryHolds: async () => [],
    inspect: async () => [],
    resumePending: async () => undefined,
  };
  return { recovery: recovery as unknown as RecoveryCoordinator, captures: () => captures };
}

describe("issue #1783: the settle transfer re-proves after its status read / wip marker", () => {
  it("blocked at settle_transfer:after_runner_git: no fetch-back, no credentialed capture, the clone kept", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1883;
    const q = siteQuiescer("settle_transfer:after_runner_git");
    const fetch = spyFetchBack(q.lastBlocked);
    const { recovery, captures } = fakeRecovery();
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          throw new Error("agent crashed");
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun, recovery }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.equal(q.blockedCount(), 1, "the settle transfer's re-proof ran and blocked");
    assert.equal(fetch.early(), 0, "no transfer fetch-back after the blocked re-proof");
    assert.equal(captures(), 0, "no credentialed capture ran");
    assert.equal(trackingSha(iid), null, "nothing transferred into the tracking ref");
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
  });
});

// ─── completion hold / wall park: the hold_capture mapping ─────────────────────────────────

describe("issue #1783: a blocked hold_capture re-proof maps onto each park's degraded outcome", () => {
  it("completion hold: held with an EMPTY head after ONE blocked re-proof (no retry, no publish)", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1884;
    api.setCompletionHoldResponse("paused", 200);
    const q = siteQuiescer("hold_capture:after_runner_git");
    const pub = spyPublish();
    const held: Array<boolean | undefined> = [];
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          const h = await ctx.enterCompletionHold!("test hold");
          held.push(h);
          if (h) return { branch: ctx.branch, completionHeld: { reason: "test hold" } };
          throw new Error("legacy terminal (could not hold)");
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.deepEqual(held, [true], "the hold stands");
    assert.equal(q.blockedCount(), 1, "one blocked re-proof, then no capture retry");
    const head = (api.completionHoldRequests[0]!.body as { head: string }).head;
    assert.equal(head, "", "an empty head: nothing was captured");
    assert.equal(pub.calls(), 0, "no checkpoint published");
    assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("wall park: parked DEGRADED after ONE blocked re-proof (no retry, no publish)", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1885;
    const q = siteQuiescer("hold_capture:after_runner_git");
    const pub = spyPublish();
    const outcomes: unknown[] = [];
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          commitWork(ctx.worktreePath);
          const o = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
          outcomes.push(o);
          return o === "parked" || o === "undeliverable"
            ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
            : { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.deepEqual(outcomes, ["parked"], "the wall park stands");
    assert.equal(q.blockedCount(), 1, "one blocked re-proof, then no capture retry");
    const body = api.wallParkRequests[0]!.body as { head: string; published: boolean };
    assert.equal(body.head, "", "degraded: an empty head");
    assert.equal(body.published, false);
    assert.equal(pub.calls(), 0, "no checkpoint published");
    assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
  });
});

// ─── finalize: the re-proofs after the base-align and the self-improve checks ─────────────

const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

const githubClaim = (iid: number) =>
  gitlabClaim(iid, {
    repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
  });

function originHasBranch(branch: string): boolean {
  try {
    gitIn(fx.originPath, ["rev-parse", "--verify", `refs/heads/${branch}`]);
    return true;
  } catch {
    return false;
  }
}

describe("issue #1783: finalize re-proves after its runner-clone steps, before the refetch and the PAT push", () => {
  it("base-align: a blocked finalize_align re-proof fails the run typed worker_residue_blocked, nothing pushed, the clone kept", async () => {
    const iid = 1886;
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const { github, calls: prCalls } = fakeGitHub();
    const q = siteQuiescer("finalize_align:after_runner_git");
    const fetch = spyFetchBack(q.lastBlocked);
    let pushes = 0;
    const origPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...a: Parameters<typeof git.pushBranch>) => {
      pushes += 1;
      return origPush(...a);
    }) as typeof git.pushBranch;
    // A Docker-wired worker (dockerHost set) seeds a per-attempt clone path (issue #1783 M2), so
    // the kept clone is the one the executor actually ran in, not the canonical path.
    let clonePath = "";
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          clonePath = ctx.worktreePath;
          commitWork(ctx.worktreePath);
          // main moves ahead on .github/workflows, so finalize aligns the branch before its push.
          commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances");
          return { branch: ctx.branch };
        },
      },
    });
    const claim = githubClaim(iid);
    await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, {
      ...RUNNER_OPTS,
      github,
      quiesceRun: q.quiesceRun,
      dockerHost: FAKE_DOCKER_HOST,
    }).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed").at(-1)?.body;
    assert.ok(q.sites().includes("finalize"), "the finalize gate ran (and passed)");
    assert.equal(q.blockedCount(), 1, "the re-proof after the align ran and blocked");
    assert.equal(failed?.fail_origin, "worker_residue_blocked", JSON.stringify(failed));
    assert.ok(String(failed?.failure_reason).startsWith(`${REASON_WORKER_RESIDUE_BLOCKED}: `));
    assert.equal(fetch.early(), 0, "no refetch after the blocked re-proof");
    assert.equal(pushes, 0, "no PAT push");
    assert.equal(prCalls.length, 0, "no PR");
    assert.equal(originHasBranch(`agent/issue-${iid}`), false, "nothing reached origin");
    assert.notEqual(clonePath, worktreeDirFor(iid), "the wired worker ran in an attempt path");
    assert.equal(fs.existsSync(path.join(clonePath, "WORK.txt")), true, "the clone is kept");
    const reproof = q.calls.find((c) => c.site === "finalize_align:after_runner_git");
    assert.equal(reproof?.dockerHost, undefined, "the re-proof after the align's plain runner git is process-only");
  });

  it("self-improve checks: a blocked finalize_checks re-proof fails the run typed worker_residue_blocked, no MR", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const q = siteQuiescer("finalize_checks:after_runner_git");
    let checksRan = 0;
    const checkRunner: CheckRunner = async (check) => {
      checksRan += 1;
      return { name: check.name, status: "skipped", detail: "test: not run" };
    };
    const claim = makeClaim({
      kind: "self_improve",
      issue_iid: 77,
      issue_title: "Self-improvement cycle",
      issue_description: "Pick one top improvement and land it.",
      base_branch: "main",
      self_improve_dogfood: true,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
    });
    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      ...RUNNER_OPTS,
      checkRunner,
      quiesceRun: q.quiesceRun,
      dockerHost: FAKE_DOCKER_HOST,
    }).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed").at(-1)?.body;
    assert.ok(checksRan >= 1, "the self-improve checks ran");
    assert.equal(q.blockedCount(), 1, "the re-proof after the checks ran and blocked");
    assert.equal(failed?.fail_origin, "worker_residue_blocked", JSON.stringify(failed));
    assert.equal(mrCalls.length, 0, "no MR opened");
    // The checks ran agent-authored code that can start containers after finalize's teardown: the
    // re-proof repeats the Docker teardown (it is handed the daemon), not just the process half.
    const reproof = q.calls.find((c) => c.site === "finalize_checks:after_runner_git");
    assert.equal(reproof?.dockerHost, FAKE_DOCKER_HOST, "the finalize_checks re-proof runs the Docker teardown");
  });

  it("self-improve checks: a quiescent finalize_checks re-proof tears Docker down before the MR is opened", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const q = siteQuiescer("none");
    let dockerTeardownAtChecks = 0;
    let mrsWhenTornDown = -1;
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      if (req.site === "finalize_checks:after_runner_git" && req.dockerHost !== undefined) {
        dockerTeardownAtChecks += 1;
        mrsWhenTornDown = mrCalls.length;
      }
      return q.quiesceRun(req);
    };
    const claim = makeClaim({
      kind: "self_improve",
      issue_iid: 78,
      issue_title: "Self-improvement cycle",
      issue_description: "Pick one top improvement and land it.",
      base_branch: "main",
      self_improve_dogfood: true,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
    });
    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      ...RUNNER_OPTS,
      checkRunner: async (check) => ({ name: check.name, status: "skipped", detail: "test: not run" }),
      quiesceRun,
      dockerHost: FAKE_DOCKER_HOST,
    }).execute(claim);
    assert.equal(dockerTeardownAtChecks, 1, "one Docker teardown at finalize_checks");
    assert.equal(mrsWhenTornDown, 0, "it ran before the MR was opened");
    assert.ok(mrCalls.length >= 1, "the run went on to open its MR");
  });
});

// ─── limit park: a blocked sink never claims durability and never settles custody ─────────

/** The api feature that lets a park report carry `checkpoint_contains_latest` (PRD #1809 D8). */
const CHECKPOINT_DURABILITY_FEATURE = "run_checkpoint_durability";

const limitWaitBodies = (runId: string): StateRequest[] =>
  api.states.filter((s) => s.runId === runId && s.body.status === "limit_wait").map((s) => s.body);

/** Commit work, leave some uncommitted (so the park's wip marker runs), then hit the usage limit. */
const limitFactory: ExecutorFactory = (runId) => ({
  homeDir: path.join(homeDir, runId),
  executor: {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      commitWork(ctx.worktreePath);
      fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in progress\n");
      const { LimitReachedError } = await import("../src/limit.js");
      throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
    },
  },
});

/** Count the credentialed settles and record every pre-report reap verdict (`limitReaped`). */
function spySettle(runner: RunRunner): { settles: () => number; reaps: boolean[] } {
  const r = runner as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  let settles = 0;
  const reaps: boolean[] = [];
  const settle = r.settleRecoveryGeneration!.bind(runner);
  r.settleRecoveryGeneration = async (...a) => {
    settles += 1;
    return await settle(...a);
  };
  const reap = r.reapRecoveryProviderForSettle!.bind(runner);
  r.reapRecoveryProviderForSettle = async (...a) => {
    const ok = (await reap(...a)) as boolean;
    reaps.push(ok);
    return ok;
  };
  return { settles: () => settles, reaps };
}

describe("issue #1783 / PRD #1809 D8: a blocked limit-park sink reports no checkpoint_contains_latest", () => {
  for (const [label, blockSite] of [
    ["the first proof (park)", "park"],
    ["the re-proof after the wip marker (park:after_runner_git)", "park:after_runner_git"],
  ] as const) {
    it(`blocked at ${label}: the limit_wait body carries no checkpoint_contains_latest key, even with the feature`, async () => {
      client.protocolFeatures = [CHECKPOINT_DURABILITY_FEATURE];
      const { gitlab } = fakeGitlab();
      const q = siteQuiescer(blockSite);
      const pub = spyPublish();
      const claim = gitlabClaim(blockSite === "park" ? 1890 : 1891, { wait_on_limit: true });
      await runnerWith(limitFactory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
      assert.equal(q.blockedCount(), 1, `the ${blockSite} proof ran and blocked`);
      assert.equal(pub.calls(), 0, "nothing published");
      const [park, ...more] = limitWaitBodies(claim.run_id);
      assert.ok(park, "the park stands (limit_wait reported)");
      assert.deepEqual(more, []);
      assert.ok(!("checkpoint_contains_latest" in park), `no durability claim, got ${JSON.stringify(park)}`);
    });
  }

  it("positive control: a quiescent sink with the feature DOES carry checkpoint_contains_latest", async () => {
    client.protocolFeatures = [CHECKPOINT_DURABILITY_FEATURE];
    const { gitlab } = fakeGitlab();
    const q = siteQuiescer("none");
    const pub = spyPublish();
    const claim = gitlabClaim(1892, { wait_on_limit: true });
    await runnerWith(limitFactory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.equal(q.blockedCount(), 0);
    assert.ok(pub.calls() >= 1, "the park published a checkpoint");
    const [park] = limitWaitBodies(claim.run_id);
    assert.ok(park, "the park stands (limit_wait reported)");
    assert.equal(park.checkpoint_contains_latest, true, JSON.stringify(park));
  });
});

describe("issue #1783 / PRD #1809 D8: a REFUSED limit park whose sink blocked never runs the credentialed settle", () => {
  for (const [label, blockSite] of [
    ["the first proof (park)", "park"],
    ["the re-proof after the wip marker (park:after_runner_git)", "park:after_runner_git"],
  ] as const) {
    it(`blocked at ${label}, park coerced to failed: limitReaped was true, yet no settle`, async () => {
      const { gitlab } = fakeGitlab();
      const q = siteQuiescer(blockSite);
      const { recovery, captures } = fakeRecovery();
      const claim = gitlabClaim(blockSite === "park" ? 1893 : 1894, { wait_on_limit: true });
      // The server declines the park (a designed refusal path) and fails the run instead, so
      // handleLimitReached returns parked=false and the non-parked branch decides the settle.
      api.overrideStateStatus(claim.run_id, "failed");
      const runner = runnerWith(limitFactory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun, recovery });
      const spy = spySettle(runner);
      await runner.execute(claim);
      assert.deepEqual(spy.reaps, [true], "the pre-report reap passed (limitReaped === true)");
      assert.equal(q.blockedCount(), 1, `the ${blockSite} proof ran and blocked`);
      assert.ok(limitWaitBodies(claim.run_id).length >= 1, "the park was asked for");
      assert.equal(spy.settles(), 0, "settleRecoveryGeneration never ran");
      assert.equal(captures(), 0, "no credentialed capture");
    });
  }

  it("positive control: the same refused park with a quiescent sink DOES settle", async () => {
    const { gitlab } = fakeGitlab();
    const q = siteQuiescer("none");
    const { recovery } = fakeRecovery();
    const claim = gitlabClaim(1895, { wait_on_limit: true });
    api.overrideStateStatus(claim.run_id, "failed");
    const runner = runnerWith(limitFactory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun, recovery });
    const spy = spySettle(runner);
    await runner.execute(claim);
    assert.deepEqual(spy.reaps, [true], "the pre-report reap passed (limitReaped === true)");
    assert.equal(q.blockedCount(), 0);
    assert.ok(limitWaitBodies(claim.run_id).length >= 1, "the park was asked for");
    assert.equal(spy.settles(), 1, "the non-parked branch settled once");
  });
});
