import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type Readable } from "node:stream";
import { type Executor } from "../src/executor.js";
import { LimitReachedError } from "../src/limit.js";
import { PauseNowSignal } from "../src/steering.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import type { BoundaryRequest, CodexExecutionSafety } from "../src/harness.js";
import { resolveBoundaryExecutable } from "../src/git.js";
import { recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";
import {
  commitInTree,
  drain,
  spyPublishLands,
  type CodexRig,
  manualDeadline,
  delay,
  codexRig,
  FakeCodexExecutor,
  statuses,
  SHUTDOWN_LOAD_SPAWN_DELAY_MS,
  FINALIZE_LOAD_SPAWN_DELAY_MS,
  finalizeDeadlineScenario,
  shutdownDeadlineScenario,
  installRecoveryRootCleanup,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

describe("Codex durability executable resolution", () => {
  it("pins Git and gitleaks to absolute image paths and rejects other relative names", () => {
    assert.equal(resolveBoundaryExecutable("git"), "/usr/bin/git");
    assert.equal(resolveBoundaryExecutable("gitleaks"), "/usr/local/bin/gitleaks");
    assert.throws(() => resolveBoundaryExecutable("relative-tool"), /trusted absolute path/);
  });
});

describe("RunRunner m4 — Codex durability sinks route through withBoundary", () => {
  it("stops a transient completion-permit retry when the finalize deadline fires (finding 4daa8d10)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const deadline = manualDeadline("finalize");
    const rig = codexRig({ armDeadline: deadline.armDeadline });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "PERMIT-DEADLINE.txt", "permit retry must stop\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1916, {
      config: { completion_contract_version: 1, contract_revision: 1 },
    });
    api.setCompletionPermitResponse(true, { httpStatus: 503 });

    let retryStarted!: () => void;
    const retrying = new Promise<void>((resolve) => { retryStarted = resolve; });
    const permitClient = client as unknown as { sleep: (ms: number) => Promise<void> };
    const originalSleep = permitClient.sleep;
    permitClient.sleep = async () => {
      retryStarted();
      await delay(1_000);
    };
    const originalPermit = client.requestCompletionPermit.bind(client);
    let permitSignal: AbortSignal | undefined;
    client.requestCompletionPermit = async (runId, args, signal) => {
      permitSignal = signal;
      return originalPermit(runId, args, signal);
    };
    try {
      const run = runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
      await retrying;
      assert.equal(api.completionPermitRequests.length, 1, "the first transient permit response entered retry backoff");
      assert.equal(permitSignal?.aborted, false, "the permit signal is live before the finalize deadline");

      deadline.fire();
      // An unfixed permit can resume after backoff. Let the fake API recover so the red
      // regression settles promptly instead of waiting for the real 10-minute retry budget.
      api.setCompletionPermitResponse(true);
      await run;

      assert.equal(permitSignal?.aborted, true, "the finalize deadline aborts the permit retry signal");
      assert.equal(api.completionPermitRequests.length, 1, "no permit retry starts after the deadline");
      assert.equal(calls.length, 0, "no MR is opened after the finalize deadline");
      const failures = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
      assert.equal(failures.length, 1, "the run fails once");
      assert.match(String(failures[0]!.body.failure_reason), /codex boundary failed at action \(finalize\) during completion-permit: codex boundary action deadline exceeded/);
      assert.ok(!statuses(claim.run_id).includes("completed"), "a poisoned boundary never reports completion");
    } finally {
      permitClient.sleep = originalSleep;
      client.requestCompletionPermit = originalPermit;
    }
  });

  it("aborts and settles a stuck finalize forge request before withBoundary/dispose returns", async () => {
    await finalizeDeadlineScenario(1219);
  });

  it("finalize deadline contract holds when permit-held Git is slow (suite load, issue #1513)", async () => {
    const r = await finalizeDeadlineScenario(1223, FINALIZE_LOAD_SPAWN_DELAY_MS);
    assert.ok(
      r.entryToHttpStartMs >= 2_000,
      `the loaded finalize boundary outlasted the old 2000ms wall-clock budget before the forge call (${r.entryToHttpStartMs}ms)`,
    );
  });

  it("(1) phasePublish FINALIZE routes through withBoundary; the trusted push/MR runs inside it (subscription reconcile + reap before publish)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1200);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.ok(rig.boundaries.includes("finalize"), `finalize boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // The trusted action ran INSIDE the boundary: the MR was opened.
    assert.equal(calls.length, 1, "the MR (the finalize action) was opened inside the boundary");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The provider root was reaped as part of the finalize boundary, BEFORE the credentialed push.
    assert.ok(rig.reaps() >= 1, "the provider root was reaped inside the finalize boundary");
    assert.ok(rig.processSpawns() > 0, "permit-held Git launched through registered boundary-action roots");
    assert.ok(rig.processArgv.every((argv) => argv[0]?.startsWith("/")), "every permit-held executable is absolute");
    // The subscription reconcile ran before the boundary (one refresh; zero standalone release).
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile refreshed before the boundary");
    // F1: the runner disposed the registry ONCE after the sinks settled.
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner disposed the registry once, in the finally");
    assert.equal(rig.registry.state(), "disposed", "the registry ends disposed");
    assert.ok(rig.disposes() >= 1, "the provider root was disposed");
  });

  it("reports the committed MR terminal only after the Codex finalize boundary releases", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig();
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    let insideFinalize = false;
    rig.safety.withBoundary = (request, action) =>
      originalWithBoundary(request, async (permit) => {
        if (request.boundary === "finalize") insideFinalize = true;
        try {
          return await action(permit);
        } finally {
          if (request.boundary === "finalize") insideFinalize = false;
        }
      });

    const originalReportState = client.reportState.bind(client);
    let terminalInsideFinalize: boolean | undefined;
    let terminalCarriedBoundarySignal: boolean | undefined;
    client.reportState = async (runId, body, signal) => {
      if (body.status === "completed" && body.mr_iid !== undefined) {
        terminalInsideFinalize = insideFinalize;
        terminalCarriedBoundarySignal = signal !== undefined;
      }
      return originalReportState(runId, body, signal);
    };
    try {
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "TERMINAL.txt", "report after boundary\n");
        return { branch: ctx.branch };
      });
      const claim = gitlabClaim(1221);
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

      assert.equal(calls.length, 1, "the MR was created inside the trusted finalize action");
      assert.equal(terminalInsideFinalize, false, "the committed MR terminal runs after boundary release");
      assert.equal(terminalCarriedBoundarySignal, false, "the terminal retry schedule is not poisoned by the expired boundary signal");
      assert.ok(statuses(claim.run_id).includes("completed"));
    } finally {
      client.reportState = originalReportState;
    }
  });

  it("reports the committed MR instead of failing when the finalize boundary errors after publication", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig();
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      const value = await originalWithBoundary(request, action);
      if (request.boundary === "finalize") {
        const error = new Error("deadline expired after action settlement");
        error.name = "CodexBoundaryError";
        throw error;
      }
      return value;
    };
    const { logger, lines } = recordingLogger();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LATE-BOUNDARY.txt", "published before boundary error\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1222);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    assert.equal(calls.length, 1, "the MR was already created before the boundary error");
    assert.ok(statuses(claim.run_id).includes("completed"), "the committed publish is reported completed");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the committed publish is never misreported failed");
    assert.ok(
      lines.some((line) =>
        (line as { msg?: string }).msg ===
        "Codex finalize boundary failed after committed publish; reporting committed terminal outcome"),
      "the swallowed post-publish boundary error is logged",
    );
    const stepLines = lines.filter((l) => (l as { msg?: string }).msg === "finalize step") as Record<string, unknown>[];
    const last = stepLines.at(-1);
    assert.equal(last?.outcome, "committed", `the last finalize step reports the committed outcome; got ${JSON.stringify(last)}`);
    assert.ok(!stepLines.some((l) => l.outcome === "failed"), "no finalize step is logged failed for a committed publish");
  });

  it("(1) checkpoint reap:true routes through withBoundary (boundary=checkpoint)", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1201);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
    assert.ok(rig.boundaries.includes("checkpoint"), `checkpoint boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // finalize still runs afterwards (a second boundary re-quiesces the closed registry).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary also runs");
  });

  it("(1) park routes through withBoundary (boundary=park)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-park-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "PARK.txt", "committed before the limit\n");
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      });
      const claim = gitlabClaim(1202, { wait_on_limit: true });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
      assert.ok(rig.boundaries.includes("park"), `park boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(statuses(claim.run_id).includes("limit_wait"), "the run parked at limit_wait");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the registry was disposed once even on the park path");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // PRD #1809 D2: the park cache drop is Claude-only. A Codex run's caches live on its own
  // per-run volume, so a Codex park leaves its HOME exactly as it was.
  it("a Codex park does not drop the run caches from its HOME (PRD #1809)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-codex-park-"));
    try {
      const rig = codexRig();
      const home = path.join(homeRoot, "home");
      const caches = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
      const exec = new FakeCodexExecutor(rig.safety, async () => {
        for (const rel of caches) {
          fs.mkdirSync(path.join(home, rel), { recursive: true });
          fs.writeFileSync(path.join(home, rel, "entry"), "cached\n");
        }
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      });
      const claim = gitlabClaim(1809, { wait_on_limit: true });
      await runnerWith(() => ({ executor: exec, homeDir: home }), gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("limit_wait"), "precondition: the run parked at limit_wait");
      for (const rel of caches) {
        assert.ok(fs.existsSync(path.join(home, rel, "entry")), `${rel} must be untouched on a Codex park`);
      }
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(1) graceful shutdown routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      const rig = codexRig();
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const runner = runnerWith(() => ({ executor: new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "SHUT.txt", "committed before shutdown\n");
        started();
        await new Promise<void>((_, reject) => ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }));
        return { branch: ctx.branch };
      }) }), gitlab);
      const p = runner.execute(gitlabClaim(1203, { wait_on_limit: true }));
      await startedP;
      runner.shutdown();
      await p;
      assert.ok(rig.boundaries.includes("shutdown"), `shutdown boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    } finally {
      restore();
    }
  });

  it("shutdown deadline cancels the upload and awaits its settlement before safety.dispose", async () => {
    await shutdownDeadlineScenario(1209);
  });

  it("shutdown deadline contract holds when permit-held Git is slow (suite load, issue #1513)", async () => {
    const r = await shutdownDeadlineScenario(1224, SHUTDOWN_LOAD_SPAWN_DELAY_MS);
    assert.ok(
      r.entryToUploadStartMs >= 500,
      `the loaded shutdown boundary outlasted the old 500ms wall-clock budget before the upload (${r.entryToUploadStartMs}ms)`,
    );
  });

  it("(1) recovery publication routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-rec-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "REC.txt", "recovered work\n");
        throw new TransientRecoveryError();
      });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1204));
      assert.ok(rig.boundaries.includes("shutdown"), `recovery boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "the run parked at recovery_wait",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ================================================================================
describe("RunRunner m4 — per-sink reconcile before the boundary", () => {
  it("(2) refresh-failure BEFORE the action: reconcile blocked → CodexBoundaryError, action never ran, registry poisoned, later publication blocked", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription", blockReconcile: true });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1210);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    // The finalize boundary was attempted, its reconcile blocked, and it threw a
    // CodexBoundaryError("reconcile") — the trusted publish/push never ran (no MR), the run
    // reported failed, and the registry is poisoned (later publication blocked).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary was attempted");
    assert.equal(calls.length, 0, "the trusted push/MR NEVER ran (publication blocked)");
    assert.ok(rig.reaps() === 0, "the provider root was NOT reaped (reconcile blocked before quiesce/reap)");
    assert.ok(statuses(claim.run_id).includes("failed"), "the failed-run report fires for a finalize CodexBoundaryError");
    assert.ok(rig.registry.isPoisoned(), "the registry is poisoned (sink counter zero)");
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile attempted a refresh");
    // F1: the runner STILL disposes the registry in the finally even on the blocked/failed path —
    // the provider root is torn down (disposeTools disposes roots regardless of poison).
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the terminal dispose ran even though the boundary was blocked");
    assert.ok(rig.disposes() >= 1, "the provider root was torn down by the terminal dispose");
  });

  it("(3) api_key reconcile: zero refreshCodex calls, releaseCodex called, the action runs", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "api_key" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1211);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.equal(rig.refreshCalls(), 0, "an api_key run performs ZERO subscription refresh calls");
    assert.ok(rig.releaseCalls() >= 1, "an api_key reconcile freshly releases (re-authorizes)");
    assert.equal(calls.length, 1, "the trusted push/MR ran (reconcile ready)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
  });
});

// ================================================================================
describe("RunRunner #1864 — a Codex boundary failure names its stage, checkpoint and unsettled work", () => {
  const UNSETTLED = "quiesceChildren: 1 callback/child-turn reservation(s) unsettled";

  /** Runs a Codex run whose milestone checkpoint (reap:true, sink milestone_checkpoint) boundary
   *  throws `makeError(request)` BEFORE its action runs. Oracles for "nothing published": the
   *  checkpoint upload spy (`client.publishCheckpoint` call count) and the forge MR calls. */
  async function failMilestoneCheckpoint(
    issue: number,
    makeError: (request: BoundaryRequest) => Error,
  ): Promise<{ reasons: string[]; lines: unknown[]; uploads: number; mrCalls: number; boundaries: string[]; sinks: unknown[] }> {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig();
    const sinks: unknown[] = [];
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      if (request.boundary === "checkpoint") {
        sinks.push(request.sink);
        throw makeError(request);
      }
      return originalWithBoundary(request, action);
    };
    const originalPublish = client.publishCheckpoint.bind(client);
    let uploads = 0;
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string,
      _tipOid: string,
      pack: Readable,
    ) => {
      uploads += 1;
      await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
    };
    const { logger, lines } = recordingLogger();
    try {
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
        return { branch: ctx.branch };
      });
      const claim = gitlabClaim(issue);
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);
      const reasons = api.states
        .filter((st) => st.runId === claim.run_id && st.body.status === "failed")
        .map((st) => String(st.body.failure_reason));
      return { reasons, lines, uploads, mrCalls: calls.length, boundaries: rig.boundaries, sinks };
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
  }

  it("a milestone checkpoint quiesce failure reports the full diagnostic, logs it, and publishes nothing", async () => {
    const r = await failMilestoneCheckpoint(1864, (request) =>
      new CodexBoundaryError("quiesce", [{ category: "protocol", message: UNSETTLED }], undefined, undefined, {
        boundary: request.boundary,
        ...(request.sink !== undefined ? { sink: request.sink } : {}),
      }));
    const expected = `codex boundary failed at quiesce (milestone checkpoint): ${UNSETTLED}`;
    assert.deepEqual(r.sinks, ["milestone_checkpoint"], "the runner forwarded the sink into the boundary request");
    assert.deepEqual(r.reasons, [expected], "the failed report names the stage, checkpoint and unsettled work");
    const runFailed = r.lines.find((l) => (l as { msg?: string }).msg === "run failed") as { error?: unknown } | undefined;
    assert.equal(runFailed?.error, expected, "the run-failed log carries the diagnostic");
    const detail = r.lines.find((l) => (l as { msg?: string }).msg === "codex boundary failed") as Record<string, unknown> | undefined;
    assert.ok(detail, "the structured boundary line is logged");
    assert.equal(detail.stage, "quiesce");
    assert.equal(detail.boundary, "checkpoint");
    assert.equal(detail.sink, "milestone_checkpoint");
    assert.equal(detail.detail, expected);
    assert.equal(r.uploads, 0, "no checkpoint was uploaded");
    assert.equal(r.mrCalls, 0, "no MR was opened");
    assert.ok(!r.boundaries.includes("finalize"), "finalize never ran");
  });

  for (const [label, diagnostic] of [
    ["a non-string", 42],
    ["an oversize", `codex boundary failed at quiesce: ${"x".repeat(600)}`],
    ["a control-character", "codex boundary failed at quiesce: forged\nline"],
    ["a bidi-override", `codex boundary failed at quiesce: forged${String.fromCharCode(0x202e)}enil`],
    // One code point per Unicode category (Cc, Cf, Zl, Zp) and per range the old hand-listed
    // predicate missed; codex-safety.test.ts folds the same list on the write side.
    ...[
      0x0007, 0x001b, 0x007f, 0x0085, 0x00ad, 0x061c, 0x180e, 0x200b, 0x200f, 0x2028,
      0x2029, 0x202a, 0x202e, 0x2060, 0x2064, 0x2066, 0x2069, 0x206a, 0x206f, 0xfeff,
    ].map((cp) => [
      `a U+${cp.toString(16).toUpperCase().padStart(4, "0")}`,
      `codex boundary failed at quiesce: forged${String.fromCodePoint(cp)}line`,
    ] as const),
  ] as const) {
    it(`a forged CodexBoundaryError with ${label} diagnostic falls back to the bare message`, async () => {
      const r = await failMilestoneCheckpoint(1865, () => {
        const forged = new Error("codex boundary failed at quiesce");
        forged.name = "CodexBoundaryError";
        (forged as unknown as { diagnostic: unknown }).diagnostic = diagnostic;
        return forged;
      });
      assert.deepEqual(r.reasons, ["codex boundary failed at quiesce"]);
      assert.ok(!r.lines.some((l) => (l as { msg?: string }).msg === "codex boundary failed"), "no structured line for a forged diagnostic");
      assert.equal(r.uploads, 0);
      assert.equal(r.mrCalls, 0);
    });
  }
});

// ================================================================================
describe("RunRunner m4 — credential-free sinks mint NO permit", () => {
  it("(4) the pause sink mints NO permit — withBoundary is not called for the credential-free pause publish", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-pause-"));
    try {
      const rig = codexRig();
      let boundariesAtPause: string[] = [];
      let parkedResult: boolean | undefined;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        await ctx.checkpoint?.({ reap: false }); // fetch-back (also credential-free, no permit)
        const at = { completedCount: 2, total: 3 };
        parkedResult = await ctx.parkForPause?.(at);
        boundariesAtPause = [...rig.boundaries]; // capture the moment the pause sink returns
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      });
      const claim = gitlabClaim(1220);
      await runnerWith(() => ({ executor: exec, homeDir: path.join(homeRoot, "h") }), gitlab).execute(claim);
      assert.equal(parkedResult, true, "the pause parked (its credential-free publish landed)");
      assert.deepEqual(
        boundariesAtPause,
        [],
        `neither the reap:false checkpoint NOR the pause sink minted a permit; got ${JSON.stringify(boundariesAtPause)}`,
      );
      assert.ok(statuses(claim.run_id).includes("paused"), "the run reported paused");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Issue #1764: a Codex run that PARKED on an owner pause returns pausedAt. phasePublish finalizes
  // nothing for it, and it must run OUTSIDE the finalize boundary: that boundary's per-sink
  // credential reconcile (refreshCodex/releaseCodex) is refused by the server once the run is
  // `paused`, which would fail a durably parked run. The terminal registry dispose still runs.
  it("(T11) a Codex run parked on an owner pause bypasses the finalize boundary: no permit, no reconcile, no push/MR, no terminal report; terminal dispose still runs", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-pause-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      let parkedResult: boolean | undefined;
      const runHome = path.join(homeRoot, "h");
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        // The per-run HOME holds the resumable session; a parked run must keep it.
        fs.mkdirSync(runHome, { recursive: true });
        fs.writeFileSync(path.join(runHome, "session.marker"), "resume me\n");
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        const at = { completedCount: 1, total: 2 };
        parkedResult = await ctx.parkForPause?.(at);
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      });
      const claim = gitlabClaim(1222);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      assert.equal(parkedResult, true, "the pause parked");
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the parked run's HOME (its resumable session) was retained");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile for a paused run");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile for a paused run");
      assert.equal(calls.length, 0, "no push/MR for a paused run");
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), "the run reported paused");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // T12 (issue #1764): a plan-phase `now` pause makes the Codex executor reject with a
  // PauseNowSignal (the plan turn has no in-loop park seam). The runner's PauseNowSignal catch owns
  // the park: handlePausePark publishes the checkpoint credential-free and parks on a `paused` ACK,
  // or reports pause_failed and preserves the session for a requeue. Either way no finalize runs,
  // so no permit is minted and no Codex credential reconcile fires, and the finally still disposes
  // the Codex registry exactly once.
  const planPhasePauseNow = (safety: CodexExecutionSafety, runHome: string): FakeCodexExecutor =>
    new FakeCodexExecutor(safety, async (ctx) => {
      fs.mkdirSync(runHome, { recursive: true });
      fs.writeFileSync(path.join(runHome, "session.marker"), "plan session\n");
      commitInTree(ctx.worktreePath, "PLAN-WORK.txt", "committed before the plan-phase pause\n");
      throw new PauseNowSignal();
    });

  it("(T12 parked) a Codex plan-phase PauseNowSignal parks through the runner catch: paused, HOME kept, no terminal, no permit/reconcile, registry disposed", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-plan-now-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      const runHome = path.join(homeRoot, "h");
      const exec = planPhasePauseNow(rig.safety, runHome);
      const claim = gitlabClaim(1225);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), `the run parked (paused ACK); got ${JSON.stringify(st)}`);
      assert.ok(!st.includes("pause_failed"), "the checkpoint landed, so no pause_failed");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the parked run's HOME was retained for the resume");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile");
      assert.equal(calls.length, 0, "no push/MR");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner disposed the Codex registry once");
      assert.equal(rig.registry.state(), "disposed", "the registry ends disposed");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(T12 declined) a Codex plan-phase PauseNowSignal whose checkpoint fails reports pause_failed, preserves the session for requeue, never fails, and disposes the registry", async () => {
    const { gitlab, calls } = fakeGitlab();
    const orig = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string,
      _tipOid: string,
      pack: Readable,
    ) => {
      await drain(pack);
      return { ok: false, httpStatus: 500 };
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1764-plan-now-fail-"));
    try {
      const rig = codexRig({ authMode: "subscription" });
      const runHome = path.join(homeRoot, "h");
      const exec = planPhasePauseNow(rig.safety, runHome);
      const claim = gitlabClaim(1226);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("pause_failed"), `pause_failed was reported; got ${JSON.stringify(st)}`);
      assert.ok(!st.includes("paused"), "the run did not park");
      assert.ok(!st.includes("failed") && !st.includes("completed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "the session HOME was preserved for the requeue");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex reconcile");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex reconcile");
      assert.equal(calls.length, 0, "no push/MR");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(5) a reap:false checkpoint mints NO permit — no withBoundary for the iteration-boundary publish", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    let boundariesAfterCheckpoint: string[] = [];
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "ITER.txt", "iteration work\n");
      await ctx.checkpoint!({ reap: false });
      boundariesAfterCheckpoint = [...rig.boundaries];
      return { branch: ctx.branch };
    });
    await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1221));
    assert.deepEqual(
      boundariesAfterCheckpoint,
      [],
      `a reap:false checkpoint never minted a permit; got ${JSON.stringify(boundariesAfterCheckpoint)}`,
    );
    // ...and finalize still runs afterwards (the ONLY boundary of this run).
    assert.deepEqual(rig.boundaries, ["finalize"], "the only permit this run minted was the finalize sink");
  });
});

// ================================================================================
// Issue #1784: a Codex run the server CONFIRMED as wall-parked or completion-held (`paused`) must
// bypass the finalize boundary like an owner pause (#1764): the server refuses the boundary's
// credential reconcile for a `paused` run. An unresolved wall park needs same-generation running
// proof before that boundary. captureHoldContext opens its own "shutdown" boundary, so these
// assert no "finalize" entry and unchanged reconcile counts AFTER the park, not zero totals.
describe("RunRunner issue #1784 - Codex finalize boundary on a hold or wall park", () => {
  const HOLD_REASON = "test completion hold reason";
  const WALL_REASON = "run exceeded its wall-clock timeout";
  const HELD_HEAD = "cafef00dcafef00dcafef00dcafef00dcafef00d";

  interface Probe { refresh: number; release: number; outcome?: unknown }

  const wallExecutor = (rig: CodexRig, runHome: string, probe: Probe, refuse: { on: boolean }): FakeCodexExecutor =>
    new FakeCodexExecutor(rig.safety, async (ctx) => {
      fs.mkdirSync(runHome, { recursive: true });
      fs.writeFileSync(path.join(runHome, "session.marker"), "resume me\n");
      commitInTree(ctx.worktreePath, "WORK.txt", "work before the wall\n");
      await ctx.checkpoint?.({ reap: false });
      probe.outcome = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
      if (probe.outcome === "parked") refuse.on = true;
      probe.refresh = rig.refreshCalls();
      probe.release = rig.releaseCalls();
      return { branch: ctx.branch, walled: { reason: WALL_REASON } };
    });

  const assertKept = (iid: number, runId: string, runHome: string, rig: CodexRig): void => {
    const st = statuses(runId);
    assert.ok(!st.includes("failed") && !st.includes("completed"), `non-terminal; got ${JSON.stringify(st)}`);
    assert.ok(fs.existsSync(path.join(runHome, "session.marker")), "HOME retained for the resume");
    assert.ok(fs.existsSync(worktreeDirFor(iid)), "clone retained for the resume");
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the registry was disposed once, at terminal");
  };

  it("(R1) a confirmed wall park bypasses the finalize boundary: no finalize permit, no reconcile after the park, not reported failed", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1784-wall-"));
    try {
      api.setWallParkResponse("paused");
      const refuse = { on: false };
      const rig = codexRig({ authMode: "subscription", refuseReconcile: () => refuse.on });
      const runHome = path.join(homeRoot, "h");
      const probe: Probe = { refresh: -1, release: -1 };
      const claim = gitlabClaim(1784);
      await runnerWith(() => ({ executor: wallExecutor(rig, runHome, probe, refuse), homeDir: runHome }), gitlab).execute(claim);
      assert.equal(probe.outcome, "parked");
      assert.ok(!rig.boundaries.includes("finalize"), `no finalize boundary; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), probe.refresh, "no refresh reconcile after the confirmed park");
      assert.equal(rig.releaseCalls(), probe.release, "no release reconcile after the confirmed park");
      assert.equal(api.wallParkRequests.length, 1, "one wall_park report was sent");
      assert.equal(calls.length, 0, "no push/MR");
      assertKept(1784, claim.run_id, runHome, rig);
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(R2) a confirmed completion hold bypasses the finalize boundary: no finalize permit, no reconcile after the hold, not reported failed", async () => {
    const { gitlab, calls } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1784-hold-"));
    try {
      api.setCompletionHoldResponse("paused");
      const refuse = { on: false };
      const rig = codexRig({ authMode: "subscription", refuseReconcile: () => refuse.on });
      const runHome = path.join(homeRoot, "h");
      const probe: Probe = { refresh: -1, release: -1 };
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        fs.mkdirSync(runHome, { recursive: true });
        fs.writeFileSync(path.join(runHome, "session.marker"), "resume me\n");
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the hold\n");
        // Deterministic capture verdict (the clone is already seeded), as runner-completion-hold does.
        git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
        mockCommittedTracking(git, HELD_HEAD);
        git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
        git.trackingTip = (async () => HELD_HEAD) as typeof git.trackingTip;
        git.checkpointPack = (async () => null) as typeof git.checkpointPack;
        const entered = await ctx.enterCompletionHold?.(HOLD_REASON);
        probe.outcome = entered;
        if (entered) refuse.on = true;
        probe.refresh = rig.refreshCalls();
        probe.release = rig.releaseCalls();
        return { branch: ctx.branch, completionHeld: { reason: HOLD_REASON } };
      });
      const claim = gitlabClaim(1785);
      await runnerWith(() => ({ executor: exec, homeDir: runHome }), gitlab).execute(claim);
      assert.equal(probe.outcome, true, "the hold was entered");
      assert.ok(!rig.boundaries.includes("finalize"), `no finalize boundary; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), probe.refresh, "no refresh reconcile after the confirmed hold");
      assert.equal(rig.releaseCalls(), probe.release, "no release reconcile after the confirmed hold");
      assert.equal(api.completionHoldRequests.length, 1, "one completion-hold request was sent");
      assert.equal(calls.length, 0, "no push/MR");
      assertKept(1785, claim.run_id, runHome, rig);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  for (const [name, httpStatus, iid] of [
    ["(C1) an unresolved wall park (503) bypasses finalize without generation proof", 503, 1786],
    ["(C1b) a reclaimed wall park (404) bypasses finalize without generation proof", 404, 1787],
  ] as const) {
    it(`${name}: non-terminal, work kept`, async () => {
      const { gitlab, calls } = fakeGitlab();
      const restore = spyPublishLands();
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1784-undeliverable-"));
      try {
        api.setWallParkResponse("paused", httpStatus);
        const rig = codexRig({ authMode: "subscription" });
        const runHome = path.join(homeRoot, "h");
        const probe: Probe = { refresh: -1, release: -1 };
        const claim = gitlabClaim(iid);
        await runnerWith(() => ({ executor: wallExecutor(rig, runHome, probe, { on: false }), homeDir: runHome }), gitlab).execute(claim);
        assert.equal(probe.outcome, "undeliverable");
        assert.ok(!rig.boundaries.includes("finalize"), `no finalize without generation proof; got ${JSON.stringify(rig.boundaries)}`);
        assert.equal(api.wallParkRequests.length, 2, "the identical wall report was retried once");
        assert.deepEqual(api.wallParkRequests[0]?.body, api.wallParkRequests[1]?.body);
        assert.deepEqual(api.ownershipRequests, [claim.run_id], "one ownership probe");
        assert.equal(calls.length, 0, "no push/MR");
        assertKept(iid, claim.run_id, runHome, rig);
      } finally {
        restore();
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
    });
  }

// A wall report can commit even when its acknowledgment is lost. The finalize credential reconcile
// requires proof that this exact generation is still running after any unresolved report.
describe("RunRunner uncertain wall park", () => {
  for (const scenario of [
    { name: "committed park lost acknowledgment", iid: 20901, lost: true, status: 200, owner: "paused", finalize: false, requests: 2 },
    { name: "repeated wall report outage", iid: 20902, lost: false, status: 503, owner: "failedProbe", finalize: false, requests: 2 },
    { name: "transient wall report 404", iid: 20903, lost: false, status: 404, owner: "notOwned", finalize: false, requests: 2 },
    { name: "stale same-worker 409 paused", iid: 20904, lost: false, status: 409, owner: "paused", finalize: false, requests: 1 },
    { name: "proven same-generation running after outage", iid: 20905, lost: false, status: 503, owner: "running", finalize: true, requests: 2 },
    { name: "running at a later generation remains uncertain", iid: 20906, lost: false, status: 503, owner: "laterGeneration", finalize: false, requests: 2 },
    { name: "running without a generation remains uncertain", iid: 20907, lost: false, status: 503, owner: "unknownGeneration", finalize: false, requests: 2 },
    { name: "409 running at a later generation remains uncertain", iid: 20908, lost: false, status: 409, owner: "laterGeneration", finalize: false, requests: 1 },
  ] as const) {
    it(scenario.name, async () => {
      const { gitlab, calls } = fakeGitlab();
      const restore = spyPublishLands();
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wall-uncertain-"));
      try {
        const claim = gitlabClaim(scenario.iid, { claim_generation: 7 });
        if (scenario.lost) api.commitWallParkThenLoseFirstReply();
        else api.setWallParkResponse(scenario.name.startsWith("409 running") ? "running" : "paused", scenario.status);
        if (scenario.owner === "paused") api.setOwnershipStatus(claim.run_id, "paused", claim.claim_generation);
        if (scenario.owner === "notOwned") api.setOwnershipNotOwned(claim.run_id);
        if (scenario.owner === "failedProbe") api.failOwnership(claim.run_id);
        if (scenario.owner === "running") api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
        if (scenario.owner === "laterGeneration") api.setOwnershipStatus(claim.run_id, "running", (claim.claim_generation ?? 0) + 1);
        if (scenario.owner === "unknownGeneration") api.setOwnershipStatus(claim.run_id, "running");
        const rig = codexRig({ authMode: "subscription" });
        const runHome = path.join(homeRoot, "h");
        const probe: Probe = { refresh: -1, release: -1 };
        await runnerWith(() => ({ executor: wallExecutor(rig, runHome, probe, { on: false }), homeDir: runHome }), gitlab).execute(claim);
        assert.equal(probe.outcome, scenario.lost ? "parked" : "undeliverable");
        assert.equal(api.wallParkRequests.length, scenario.requests, "wall report has one bounded retry on throw");
        if (scenario.requests === 2) assert.deepEqual(api.wallParkRequests[0]?.body, api.wallParkRequests[1]?.body);
        assert.deepEqual(api.ownershipRequests, scenario.lost ? [] : [claim.run_id], "only unresolved reports need one ownership probe");
        assert.equal(rig.boundaries.includes("finalize"), scenario.finalize, "finalize needs same-generation running proof");
        assert.equal(calls.length, 0, "no push/MR");
        assertKept(scenario.iid, claim.run_id, runHome, rig);
      } finally {
        restore();
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
    });
  }
});
});

// ================================================================================
describe("RunRunner m4 — Claude/stub legacy path: literal killAgentTree reaps, plus the issue #1783 quiescence proofs", () => {
  it("(6) an executor with killAgentTree only (no safety) takes the legacy branch — withBoundary is never referenced", async () => {
    const { gitlab, calls } = fakeGitlab();
    const kills: string[] = [];
    // A LEGACY executor: killAgentTree only, NO `safety`. reapForSink/withCodexBoundaryOnly must
    // take the literal killAgentTree branch (finalize wrapper does NOT reap: the security-boundary
    // killAgentTree already did). No `safety` ⇒ the runner's terminal dispose is skipped entirely.
    const legacy: Executor = {
      run: async (ctx) => {
        commitInTree(ctx.worktreePath, "IMPL.txt", "claude work\n");
        return { branch: ctx.branch };
      },
      killAgentTree: () => kills.push("kill"),
    };
    const claim = gitlabClaim(1230);
    await runnerWith(() => ({ executor: legacy }), gitlab).execute(claim);
    assert.equal(calls.length, 1, "the MR opened exactly as before (finalize wrapper is a plain call)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The security-boundary reap fired once (per the untouched killAgentTree at the boundary),
    // plus exactly one more per issue #1783 quiescence proof, whose first step is the same literal
    // killAgentTree: the finalize boundary and the terminal retire. `=== 3` — not `>= 3` — still
    // guards the double-reap hazard: if withCodexBoundaryOnly's legacy branch erroneously
    // re-reaped, this would be 4.
    assert.equal(kills.length, 3, "security boundary + finalize quiescence + terminal-retire quiescence");
  });
});

// ================================================================================
// Issue #1766 M3b: a Codex refresh/release deferred by a LOCKED owner vault (the api's typed 409
// `vault_locked`) parks the run for recovery, credential-free, instead of failing it.
