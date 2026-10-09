import { describe, it } from "node:test";
import { AsyncResource } from "node:async_hooks";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import { type ArmBoundaryDeadline } from "../src/codex/safety.js";

import type { BoundaryRequest } from "../src/harness.js";
import { SupervisedChildExitTimeoutError } from "../src/codex/launcher.js";
import type { SummaryRunner } from "../src/summary-runner.js";

import { recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
} from "./runner-harness.js";
import {
  GIT_ENV,
  commitInTree,
  drain,
  manualDeadline,
  codexRig,
  FakeCodexExecutor,
  statuses,
  VAULT_FEATURE,
  parkReports,
  installRecoveryRootCleanup,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

describe("RunRunner #1900 — the Codex finalize boundary deadline and its named steps", () => {
  const FINALIZE_DEFAULT_MS = 50 * 60_000;

  /** A virtual clock for the finalize boundary deadline: records every `ms` the finalize arm
   *  receives and fires only when `advance` moves the virtual clock to the armed instant. Every
   *  other boundary keeps a real unref'd timer, as in production. */
  function virtualFinalizeDeadline(): {
    armDeadline: ArmBoundaryDeadline;
    armedMs: number[];
    fired: () => number;
    advance: (ms: number) => void;
  } {
    let now = 0;
    let fired = 0;
    const armedMs: number[] = [];
    let pending: { at: number; fire: () => void } | undefined;
    const armDeadline: ArmBoundaryDeadline = (request, ms, fireDeadline) => {
      if (request.boundary !== "finalize") {
        const timer = setTimeout(fireDeadline, ms);
        timer.unref?.();
        return () => clearTimeout(timer);
      }
      armedMs.push(ms);
      const entry = { at: now + ms, fire: fireDeadline };
      pending = entry;
      return () => {
        if (pending === entry) pending = undefined;
      };
    };
    return {
      armDeadline,
      armedMs,
      fired: () => fired,
      advance: (ms) => {
        now += ms;
        if (pending && now >= pending.at) {
          const { fire } = pending;
          pending = undefined;
          fired += 1;
          fire();
        }
      },
    };
  }

  /** A fake editor pass: a deadline in the future (so the publisher calls it) and a
   *  `generateDeliverySummary` that runs `during` and falls back to the lead's claims. */
  function fakeSummaryRunner(during: () => void): { runner: SummaryRunner; calls: () => number } {
    let calls = 0;
    const runner = {
      deliverySummaryDeadline: () => Date.now() + 60_000,
      generateDeliverySummary: async () => {
        calls += 1;
        during();
        return null;
      },
    } as unknown as SummaryRunner;
    return { runner, calls: () => calls };
  }

  function originBranchTip(iid: number): string | null {
    try {
      return execFileSync(
        "git",
        ["-C", fx.originPath, "rev-parse", "--verify", `refs/heads/agent/issue-${iid}`],
        { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] },
      ).trim();
    } catch {
      return null;
    }
  }

  function failedReasons(runId: string): string[] {
    return api.states
      .filter((st) => st.runId === runId && st.body.status === "failed")
      .map((st) => String(st.body.failure_reason));
  }

  it("a 31 s PR-description editor pass completes under the default finalize deadline (regression)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const clock = virtualFinalizeDeadline();
    const pass = fakeSummaryRunner(() => clock.advance(31_000));
    const rig = codexRig({ armDeadline: clock.armDeadline });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "SLOW-PASS.txt", "a slow editor pass\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1900);
    // Default deadlines: neither codexBoundaryDeadlineMs nor the finalize override is passed.
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
      summaryRunner: pass.runner,
    }).execute(claim);

    assert.equal(pass.calls(), 1, "the editor pass ran (and advanced the virtual clock 31 s)");
    assert.deepEqual(failedReasons(claim.run_id), [], "no failure was reported");
    assert.equal(clock.armedMs.length, 1, "the finalize deadline was armed once");
    assert.equal(clock.fired(), 0, "the finalize deadline did not fire during a 31 s editor pass");
    assert.equal(calls.length, 1, "the MR was created");
    assert.ok(originBranchTip(1900), "the branch was pushed");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
  });

  it("a finalize deadline failure names the step running when it fired, not the step the action moved on to", async () => {
    const { gitlab, calls } = fakeGitlab();
    const deadline = manualDeadline("finalize");
    const pass = fakeSummaryRunner(() => deadline.fire());
    const rig = codexRig({ armDeadline: deadline.armDeadline });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "NAMED-STEP.txt", "deadline during prepare\n");
      return { branch: ctx.branch };
    });
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(1901);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, {
      summaryRunner: pass.runner,
    }).execute(claim);

    assert.equal(pass.calls(), 1, "the editor pass ran and fired the deadline");
    assert.equal(deadline.fired(), 1, "the finalize deadline fired once");
    assert.equal(calls.length, 0, "the aborted createMergeRequest opened no MR");
    const stepLines = lines.filter((l) => (l as { msg?: string }).msg === "finalize step") as Record<string, unknown>[];
    assert.ok(stepLines.some((l) => l.step === "mr_create"), "the action moved on into mr_create after the fire");
    const reasons = failedReasons(claim.run_id);
    assert.equal(reasons.length, 1, `the run failed once; got ${JSON.stringify(reasons)}`);
    assert.match(reasons[0]!, /codex boundary failed at action \(finalize\)/);
    assert.ok(reasons[0]!.includes("during pr-description-prepare"), `names the step; got ${reasons[0]}`);
    assert.ok(!reasons[0]!.includes("during mr-create"), `does not name the later step; got ${reasons[0]}`);
    const detail = lines.find((l) => (l as { msg?: string }).msg === "codex boundary failed") as Record<string, unknown> | undefined;
    assert.equal(detail?.step, "pr_description_prepare", "the structured boundary line carries the step");
    assert.equal(detail?.boundary, "finalize");
    assert.equal(detail?.stage, "action");
  });

  for (const override of [undefined, 123_456] as const) {
    it(`routes ${override === undefined ? "the default finalize deadline" : "the finalize override"} to finalize and codexBoundaryDeadlineMs to checkpoint`, async () => {
      assert.notEqual(FINALIZE_DEFAULT_MS, 30_000, "the finalize deadline differs from the 30 s default");
      const armed: { boundary: string; ms: number }[] = [];
      const armDeadline: ArmBoundaryDeadline = (request, ms, fireDeadline) => {
        armed.push({ boundary: request.boundary, ms });
        const timer = setTimeout(fireDeadline, ms);
        timer.unref?.();
        return () => clearTimeout(timer);
      };
      const { gitlab } = fakeGitlab();
      const rig = codexRig({ armDeadline });
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "ROUTE.txt", "milestone 1\n");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      });
      const claim = gitlabClaim(override === undefined ? 1902 : 1903);
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined,
        override === undefined ? {} : { codexFinalizeBoundaryDeadlineMs: override }).execute(claim);

      const finalize = armed.filter((a) => a.boundary === "finalize");
      const checkpoint = armed.filter((a) => a.boundary === "checkpoint");
      assert.equal(finalize.length, 1, `finalize armed once; got ${JSON.stringify(armed)}`);
      assert.ok(checkpoint.length >= 1, `checkpoint armed; got ${JSON.stringify(armed)}`);
      const expected = override ?? FINALIZE_DEFAULT_MS;
      // The arm receives the remaining budget at action entry: at most the deadline, and within a
      // generous slack of it (the acquire runs on real time).
      assert.ok(finalize[0]!.ms <= expected && finalize[0]!.ms > expected - 20_000,
        `finalize armed with ~${expected} ms; got ${finalize[0]!.ms}`);
      for (const c of checkpoint) {
        assert.ok(c.ms <= 30_000 && c.ms > 10_000, `checkpoint armed with ~30000 ms; got ${c.ms}`);
      }
      assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    });
  }

  it("a successful Codex finalize logs each finalize step with its duration", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "STEPS.txt", "step logging\n");
      return { branch: ctx.branch };
    });
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(1904);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    const stepLines = lines.filter((l) => (l as { msg?: string }).msg === "finalize step") as Record<string, unknown>[];
    const steps = stepLines.map((l) => l.step);
    for (const step of ["run_quiescence", "fetch_back", "push", "pr_description_prepare", "mr_create", "post_mr"]) {
      assert.ok(steps.includes(step), `logged ${step}; got ${JSON.stringify(steps)}`);
    }
    assert.equal(new Set(steps).size, steps.length, "each step is logged once");
    for (const l of stepLines) {
      assert.equal(l.level, "info");
      assert.equal(typeof l.durationMs, "number");
      assert.ok((l.durationMs as number) >= 0);
    }
    const last = stepLines.at(-1)!;
    assert.equal(last.step, "post_mr", "the last step is closed at the end of finalize");
    assert.equal(last.outcome, "ok", "the last step carries the finalize outcome");
    assert.ok(stepLines.slice(0, -1).every((l) => l.outcome === undefined), "only the last step carries an outcome");
  });
});

describe("RunRunner #1914 — Codex milestone checkpoint diagnostics", () => {
  it("reports the checkpoint step active when the hard deadline fires", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const deadline = manualDeadline("checkpoint");
    const rig = codexRig({ armDeadline: deadline.armDeadline });
    let inCheckpoint = false;
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      if (request.boundary === "checkpoint") inCheckpoint = true;
      try {
        return await originalWithBoundary(request, action);
      } finally {
        if (request.boundary === "checkpoint") inCheckpoint = false;
      }
    };
    const originalFetch = git.fetchDefaultTip.bind(git);
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      if (inCheckpoint) {
        deadline.fire();
        throw new Error("the test fired the checkpoint boundary deadline");
      }
      return originalFetch(...args);
    }) as typeof git.fetchDefaultTip;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "deadline step\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1916, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, { github }).execute(claim);
    } finally {
      git.fetchDefaultTip = originalFetch;
    }

    assert.equal(deadline.fired(), 1, "the checkpoint deadline fired once");
    const reasons = api.states
      .filter((state) => state.runId === claim.run_id && state.body.status === "failed")
      .map((state) => String(state.body.failure_reason));
    assert.equal(reasons.length, 1, "the hard boundary still fails closed");
    assert.match(reasons[0]!, /deadline exceeded during checkpoint-lock-wait/,
      "the failure names the step at fire time, not the later settled state");
  });

  it("exposes the default-fetch phase to the checkpoint boundary while it runs", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const rig = codexRig();
    let activeStep: BoundaryRequest["activeStep"];
    let inCheckpoint = false;
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      if (request.boundary === "checkpoint") {
        activeStep = request.activeStep;
        inCheckpoint = true;
      }
      try {
        return await originalWithBoundary(request, action);
      } finally {
        if (request.boundary === "checkpoint") {
          activeStep = undefined;
          inCheckpoint = false;
        }
      }
    };
    const originalFetch = git.fetchDefaultTip.bind(git);
    const phasesDuringFetch: Array<string | undefined> = [];
    const phasesWhileQueued: Array<string | undefined> = [];
    const scoped = git as unknown as { execScoped: (...args: unknown[]) => Promise<{ stdout: string; stderr: string }> };
    const originalExec = scoped.execScoped.bind(git);
    const phasesDuringGitFetch: Array<string | undefined> = [];
    const overlay = git as unknown as { workflowTreeDiffers: (...args: unknown[]) => Promise<boolean> };
    const originalTreeDiffers = overlay.workflowTreeDiffers.bind(git);
    const phasesDuringOverlayGate: Array<string | undefined> = [];
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      if (!inCheckpoint) return originalFetch(...args);
      phasesDuringFetch.push(activeStep?.());
      let releaseHeld!: () => void;
      const held = git.withBareLock(args[0], () => new Promise<void>((resolve) => { releaseHeld = resolve; }));
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      const pending = originalFetch(...args);
      try {
        await new Promise<void>((resolve) => setTimeout(resolve, 5));
        phasesWhileQueued.push(activeStep?.());
      } finally {
        releaseHeld();
        await held;
      }
      return pending;
    }) as typeof git.fetchDefaultTip;
    scoped.execScoped = async (...args) => {
      const argv = args[1];
      if (inCheckpoint && Array.isArray(argv) && argv.includes("fetch") && argv.includes("origin")) {
        phasesDuringGitFetch.push(activeStep?.());
      }
      return originalExec(...args);
    };
    overlay.workflowTreeDiffers = async (...args) => {
      if (inCheckpoint) phasesDuringOverlayGate.push(activeStep?.());
      return originalTreeDiffers(...args);
    };
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "checkpoint diagnostics\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1914, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, { github }).execute(claim);
    } finally {
      git.fetchDefaultTip = originalFetch;
      scoped.execScoped = originalExec;
      overlay.workflowTreeDiffers = originalTreeDiffers;
    }

    assert.deepEqual(phasesDuringFetch, ["checkpoint_lock_wait"],
      "the boundary names the lock wait before the authenticated fetch starts");
    assert.deepEqual(phasesWhileQueued, ["checkpoint_lock_wait"],
      "the boundary keeps the lock-wait label while a real bare lock blocks the fetch");
    assert.deepEqual(phasesDuringGitFetch, ["default_fetch"],
      "the boundary's fire-time probe names the authenticated child after acquiring the lock");
    assert.deepEqual(phasesDuringOverlayGate, ["checkpoint_overlay"],
      "the workflow comparison has its own label after the fetch finishes");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed after a normal fetch");
  });

  it("logs bounded, secret-free phase durations for a successful milestone checkpoint", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "checkpoint step timings\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      return { branch: ctx.branch };
    });
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(1915);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    const phases = lines.filter((line) => (line as { msg?: string }).msg === "checkpoint step") as Record<string, unknown>[];
    assert.ok(phases.length > 0, "the checkpoint emitted phase timing records");
    for (const phase of phases) {
      assert.equal(phase.level, "info");
      assert.ok(typeof phase.step === "string" && /^[a-z_]+$/.test(phase.step), "phase is a fixed label");
      assert.equal(typeof phase.durationMs, "number");
      assert.ok(Number.isFinite(phase.durationMs) && (phase.durationMs as number) >= 0, "duration is finite and nonnegative");
      assert.ok(!JSON.stringify(phase).includes(claim.secrets.forge_pat), "phase log contains no forge PAT");
    }
  });
});

describe("RunRunner #1914 — a slow checkpoint overlay remains owed", () => {
  it("skips a queued default-tip lock without publishing, then retries the same committed tip under a fresh checkpoint", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const rig = codexRig();
    const bare = git.barePathFor(fx.originPath);
    const originalFetch = git.fetchDefaultTip.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let releaseHolder!: () => void;
    const held = new Promise<void>((resolve) => { releaseHolder = resolve; });
    let markAcquired!: () => void;
    const acquired = new Promise<void>((resolve) => { markAcquired = resolve; });
    let fetches = 0;
    let softDeadlines = 0;
    let softExpiries = 0;
    let fireSoftDeadline: (() => void) | undefined;
    const uploadedTips: string[] = [];
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      fetches += 1;
      if (fetches !== 1) return originalFetch(...args);
      // Occupy only the default-tip fetch's lock, after the checkpoint's fetch-back and
      // bridge work. Its queued PAT operation must settle inside the permit when the soft
      // deadline expires; releasing the holder afterward cannot cause a late fetch.
      const holder = git.withBareLock(bare, async () => {
        markAcquired();
        await held;
      });
      await acquired;
      try {
        // fetchDefaultTip installs its lock waiter synchronously. Expire only AFTER it
        // is queued, so CPU load cannot spend the budget before reaching this seam.
        const pending = originalFetch(...args);
        assert.ok(fireSoftDeadline, "the checkpoint soft deadline is armed before the queued fetch");
        softExpiries += 1;
        fireSoftDeadline();
        return await pending;
      } finally {
        releaseHolder();
        await holder;
      }
    }) as typeof git.fetchDefaultTip;
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, tip: string, pack: Readable,
    ) => {
      uploadedTips.push(tip);
      await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1918" } };
    };

    let firstReturned = false;
    let secondReturned = false;
    let committedTip = "";
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "the committed milestone\n");
      committedTip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"],
        { env: GIT_ENV, encoding: "utf8" }).trim();
      // Fetch back before entering the Codex permit so the test spends its short
      // checkpoint deadline specifically at the overlay's queued default-tip lock.
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      firstReturned = true;
      assert.equal(await git.trackingTip(bare, ctx.branch), committedTip,
        "the committed milestone remains in the worker bare after the skipped upload");
      assert.deepEqual(uploadedTips, [], "the timed-out overlay neither uploaded nor claimed remote durability");
      assert.ok(api.states.some((state) => state.runId === ctx.runId && state.body.status === "running" &&
        state.body.milestones_completed?.includes("m1")), "the locally completed milestone progress was reported");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      secondReturned = true;
      assert.equal(uploadedTips.length, 1, "the owed milestone was retried once without a new commit");
      assert.notEqual(uploadedTips[0], committedTip, "retry rebuilt the workflow overlay, rather than sending the raw tip");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1918, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
        github,
        codexBoundaryDeadlineMs: 8_000,
        checkpointIntervalMs: 0,
        checkpointTickIntervalMs: 0,
        checkpointTestHooks: {
          armSoftDeadline: (fire) => {
            softDeadlines += 1;
            fireSoftDeadline = fire;
            // Signal-driven soft expiry leaves child timing to the real hard Codex
            // boundary, so a slow successful retry cannot spend a test-only soft budget.
            return { cancel: () => { fireSoftDeadline = undefined; } };
          },
        },
      }).execute(claim);
    } finally {
      releaseHolder();
      git.fetchDefaultTip = originalFetch;
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }

    assert.equal(softDeadlines, 2, "each checkpoint arms its own soft deadline");
    assert.equal(softExpiries, 1, "only the queued first checkpoint expires");
    assert.equal(fireSoftDeadline, undefined, "the retry cancels its soft deadline on settlement");
    assert.ok(fetches >= 2, `the retry performed a fresh default-tip fetch; saw ${fetches}`);
    assert.equal(firstReturned, true, "the soft-skipped milestone returned to the executor");
    assert.equal(secondReturned, true, "the next milestone settled the owed publication");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed after the guarded retry");
    assert.ok(!statuses(claim.run_id).includes("failed"), "a best-effort checkpoint timeout did not fail the run");
  });

  it("lets the ordinary time-gated tick publish a locally fetched milestone after a soft skip", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const rig = codexRig();
    const bare = git.barePathFor(fx.originPath);
    const originalFetch = git.fetchDefaultTip.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let releaseHolder!: () => void;
    const held = new Promise<void>((resolve) => { releaseHolder = resolve; });
    let markAcquired!: () => void;
    const acquired = new Promise<void>((resolve) => { markAcquired = resolve; });
    let blockedFetches = 0;
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      if (++blockedFetches !== 1) return originalFetch(...args);
      const holder = git.withBareLock(bare, async () => { markAcquired(); await held; });
      await acquired;
      try { return await originalFetch(...args); }
      finally { releaseHolder(); await holder; }
    }) as typeof git.fetchDefaultTip;
    const tips: string[] = [];
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, tip: string, pack: Readable,
    ) => {
      tips.push(tip);
      await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1922" } };
    };
    let tick: (() => void) | undefined;
    let resolveOutcome!: (outcome: string) => void;
    const tickOutcome = new Promise<string>((resolve) => { resolveOutcome = resolve; });
    let now = 0;
    let milestoneTip = "";
    let tickResult = "";
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "tick retry after soft skip\n");
      milestoneTip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"],
        { env: GIT_ENV, encoding: "utf8" }).trim();
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      assert.deepEqual(tips, [], "the soft-skipped milestone sent no raw or overlay pack");
      assert.equal(await git.trackingTip(bare, ctx.branch), milestoneTip, "the tick can read the local committed candidate");
      now = 1_001; // open the ordinary time gate, which was closed for the initial fetch-back
      assert.ok(tick, "the mid-turn timer was armed");
      tick();
      tickResult = await tickOutcome;
      assert.equal(tickResult, "published", "the ordinary tick settled the owed candidate");
      assert.deepEqual(tips, [milestoneTip], "the overlay-less, scratch-scanned tick packed the committed tip");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1922, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
        github, codexBoundaryDeadlineMs: 8_000, checkpointIntervalMs: 1_000, checkpointTickIntervalMs: 424_242,
        now: () => now,
        setTickTimer: (callback) => {
          const bound = AsyncResource.bind(callback);
          tick = bound;
          return () => { if (tick === bound) tick = undefined; };
        },
        checkpointTestHooks: { softDeadlineMs: 2_500, onTickOutcome: resolveOutcome },
      }).execute(claim);
    } finally {
      releaseHolder();
      git.fetchDefaultTip = originalFetch;
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
    assert.equal(tickResult, "published", "the tick completed before the executor returned");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("reclaims the locally fetched candidate on the same worker after a soft skip", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    client.protocolFeatures = [VAULT_FEATURE];
    let vaultLocked = false;
    const rig1 = codexRig({ vaultLocked: () => vaultLocked });
    const bare = git.barePathFor(fx.originPath);
    const originalFetch = git.fetchDefaultTip.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let releaseHolder!: () => void;
    const held = new Promise<void>((resolve) => { releaseHolder = resolve; });
    let markAcquired!: () => void;
    const acquired = new Promise<void>((resolve) => { markAcquired = resolve; });
    let defaultFetches = 0;
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      if (++defaultFetches !== 1) return originalFetch(...args);
      const holder = git.withBareLock(bare, async () => { markAcquired(); await held; });
      await acquired;
      try { return await originalFetch(...args); }
      finally { releaseHolder(); await holder; }
    }) as typeof git.fetchDefaultTip;
    let firstFlight = true;
    const confirmedTips: string[] = [];
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, tip: string, pack: Readable,
    ) => {
      await drain(pack);
      if (firstFlight) {
        return { ok: true, body: { published: false, skipped: "workflow_scope" } };
      }
      confirmedTips.push(tip);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1923" } };
    };
    let committedTip = "";
    let softSkipped = false;
    const claim = gitlabClaim(1923, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    const exec1 = new FakeCodexExecutor(rig1.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "saved in the worker bare\n");
      committedTip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"],
        { env: GIT_ENV, encoding: "utf8" }).trim();
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      softSkipped = true;
      assert.equal(await git.trackingTip(bare, ctx.branch), committedTip, "the candidate survived locally");
      assert.deepEqual(confirmedTips, [], "the first flight reported no remote success");
      vaultLocked = true; // finalize parks for recovery without a credentialed push
      return { branch: ctx.branch };
    }, rig1.settle);
    let reclaimedTip = "";
    let secondCheckpointReturned = false;
    try {
      await runnerWith(() => ({ executor: exec1 }), gitlab, undefined, undefined, {
        github, codexBoundaryDeadlineMs: 8_000, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
        checkpointTestHooks: { softDeadlineMs: 2_500 }, recoveryRetryMs: 5,
      }).execute(claim);
      assert.equal(softSkipped, true, "the first flight returned from its soft-skipped checkpoint");
      assert.equal(parkReports(claim.run_id).length, 1, "the first flight parked for same-worker reclaim");
      assert.deepEqual(confirmedTips, [], "neither the skip nor recovery park claimed remote durability");
      assert.equal(await git.trackingTip(bare, "agent/issue-1923"), committedTip,
        "the same worker bare retains the candidate across the claim boundary");

      firstFlight = false;
      vaultLocked = false;
      const rig2 = codexRig({ vaultLocked: () => vaultLocked });
      const exec2 = new FakeCodexExecutor(rig2.safety, async (ctx) => {
        reclaimedTip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"],
          { env: GIT_ENV, encoding: "utf8" }).trim();
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "M1.txt"), "utf8"), "saved in the worker bare\n");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
        secondCheckpointReturned = true;
        assert.equal(confirmedTips.length, 1, "the reclaim's guarded checkpoint received the first published ACK");
        assert.notEqual(confirmedTips[0], committedTip, "the reclaimed candidate was wrapped for the advanced workflow");
        return { branch: ctx.branch };
      }, rig2.settle);
      await runnerWith(() => ({ executor: exec2 }), gitlab, undefined, undefined, {
        github, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
      }).execute({ ...claim, claim_generation: 2 });
    } finally {
      releaseHolder();
      git.fetchDefaultTip = originalFetch;
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
    assert.equal(reclaimedTip, committedTip, "the second claim recovered the exact committed milestone");
    assert.equal(secondCheckpointReturned, true, "the reclaimed flight retried publication");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("settles an aborted broker upload without a success report, then retries the unacknowledged tip", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const rig = codexRig();
    const originalPublish = client.publishCheckpoint.bind(client);
    let uploads = 0;
    let softDeadlines = 0;
    let softExpiries = 0;
    let fireSoftDeadline: (() => void) | undefined;
    let abortSettled = false;
    let confirmed = 0;
    const publishedStates: Array<{ lastPublishedTip?: string; checkpointFloor?: string }> = [];
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, _tip: string, pack: Readable, signal?: AbortSignal,
    ) => {
      uploads += 1;
      await drain(pack);
      if (uploads === 1) {
        assert.ok(signal, "the milestone passes its soft and hard abort signals to the broker request");
        assert.equal(signal.aborted, false, "the broker is reached before the test fires soft expiry");
        await new Promise<void>((resolve) => {
          if (signal.aborted) resolve();
          else signal.addEventListener("abort", () => resolve(), { once: true });
          assert.ok(fireSoftDeadline, "the checkpoint soft deadline is armed before the broker upload");
          softExpiries += 1;
          fireSoftDeadline();
        });
        abortSettled = true;
        throw new DOMException("checkpoint upload aborted", "AbortError");
      }
      confirmed += 1;
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1919" } };
    };

    let firstReturned = false;
    let secondReturned = false;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "upload must settle before permit release\n");
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      firstReturned = true;
      assert.equal(abortSettled, true, "the in-flight upload settled before the checkpoint returned");
      assert.equal(uploads, 1, "the first checkpoint attempted one upload");
      assert.equal(confirmed, 0, "no upload received a published ACK");
      assert.deepEqual(publishedStates, [], "the aborted attempt did not advance published-tip or floor state");
      assert.ok(api.states.some((state) => state.runId === ctx.runId && state.body.status === "running" &&
        state.body.milestones_completed?.includes("m1")), "the locally completed milestone progress was reported");
      assert.ok(!api.messages(ctx.runId).some((m) => m.kind === "status" &&
        /checkpoint publishing recovered|published to origin/.test(String(m.payload.text))),
      "the feed contains no false published-success line");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      secondReturned = true;
      assert.equal(uploads, 2, "the same committed work was retried without another commit");
      assert.equal(confirmed, 1, "the retry received the only published ACK");
      assert.equal(publishedStates.length, 1, "only the confirmed retry advanced published state");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1919, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
        github,
        codexBoundaryDeadlineMs: 8_000,
        checkpointIntervalMs: 0,
        checkpointTickIntervalMs: 0,
        checkpointTestHooks: {
          armSoftDeadline: (fire) => {
            softDeadlines += 1;
            fireSoftDeadline = fire;
            // Expire only once the first broker request can observe abort. The retry
            // keeps its real hard boundary without racing a test-only soft clock.
            return { cancel: () => { fireSoftDeadline = undefined; } };
          },
          afterUnpinnedPublish: (state) => publishedStates.push(state),
        },
      }).execute(claim);
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
    assert.equal(softDeadlines, 2, "each upload checkpoint arms its own soft deadline");
    assert.equal(softExpiries, 1, "only the unacknowledged upload expires");
    assert.equal(fireSoftDeadline, undefined, "the acknowledged retry cancels its soft deadline");
    assert.equal(firstReturned, true, "the soft-skipped upload returned to the executor");
    assert.equal(secondReturned, true, "the next guarded checkpoint retried publication");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("holds the bare lock through clean reap of a timed-out authenticated Git child", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const events: string[] = [];
    let childLive = true;
    let timedOutFetches = 0;
    let published = 0;
    const rig = codexRig({
      boundaryProcess: (request) => {
        if (timedOutFetches > 0 || request.identity !== "worker_pat" ||
            !request.argv.includes("fetch") || !request.argv.includes("origin")) return undefined;
        timedOutFetches += 1;
        assert.equal(request.recoverableTimeout, true, "only this checkpoint child opted into a recoverable timeout");
        const stdout = new PassThrough();
        const stderr = new PassThrough();
        return {
          root: {
            kind: "boundary_action",
            dispose: async () => {
              events.push("dispose");
              childLive = false;
            },
            reap: async () => {
              events.push("reap");
              return childLive
                ? { ok: false as const, error: { category: "timeout" as const, message: "PAT child still live" } }
                : { ok: true as const };
            },
          },
          stdin: new PassThrough(), stdout, stderr,
          waitChild: async () => {
            events.push("child_timeout");
            throw new SupervisedChildExitTimeoutError(1);
          },
        };
      },
    });
    const originalScope = git.withBoundaryProcessSpawner.bind(git);
    git.withBoundaryProcessSpawner = ((spawner, signal, action, hooks) => originalScope(spawner, signal, action, {
      ...hooks,
      beforeLockRelease: async (key) => {
        await hooks?.beforeLockRelease?.(key);
        if (timedOutFetches > 0 && !events.includes("lock_release")) {
          events.push("lock_release");
          assert.equal(childLive, false, "the PAT child was disposed before the bare lock was released");
          assert.ok(events.includes("reap"), "the registered root fully reaped before bare-lock release");
        }
      },
    })) as typeof git.withBoundaryProcessSpawner;
    const originalPublish = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, _tip: string, pack: Readable,
    ) => {
      published += 1;
      await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1920" } };
    };
    let firstReturned = false;
    let secondReturned = false;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "supervised fetch timeout\n");
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      firstReturned = true;
      assert.equal(published, 0, "a timed-out authenticated child cannot authorize a raw-tip upload");
      assert.ok(api.states.some((state) => state.runId === ctx.runId && state.body.status === "running" &&
        state.body.milestones_completed?.includes("m1")), "the locally completed milestone progress was reported");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      secondReturned = true;
      assert.equal(published, 1, "a later guarded checkpoint retries and lands the committed tip once");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1920, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
        github, codexBoundaryDeadlineMs: 8_000, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
        checkpointTestHooks: { softDeadlineMs: 3_000 },
      }).execute(claim);
    } finally {
      git.withBoundaryProcessSpawner = originalScope;
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
    assert.equal(firstReturned, true, "the soft timeout returned to the executor");
    assert.equal(secondReturned, true, "the guarded retry settled");
    assert.equal(timedOutFetches, 1);
    assert.deepEqual(events.slice(0, 4), ["child_timeout", "dispose", "reap", "lock_release"]);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("settles a timed-out supervised pack child before returning, then retries the pack", async () => {
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const events: string[] = [];
    let timedOutPacks = 0;
    const rig = codexRig({
      boundaryProcess: (request) => {
        if (timedOutPacks > 0 || !request.argv.includes("pack-objects")) return undefined;
        timedOutPacks += 1;
        assert.equal(request.recoverableTimeout, true, "a checkpoint pack child uses the soft deadline");
        let childLive = true;
        const stdout = new PassThrough();
        return {
          root: {
            kind: "boundary_action",
            dispose: async () => { events.push("dispose"); childLive = false; },
            reap: async () => {
              events.push("reap");
              return childLive
                ? { ok: false as const, error: { category: "timeout" as const, message: "pack child still live" } }
                : { ok: true as const };
            },
          },
          stdin: new PassThrough(), stdout, stderr: new PassThrough(),
          waitChild: async () => {
            stdout.destroy();
            await new Promise<void>((resolve) => setImmediate(resolve));
            events.push("pack_timeout");
            throw new SupervisedChildExitTimeoutError(1);
          },
        };
      },
    });
    const originalPublish = client.publishCheckpoint.bind(client);
    let attempts = 0;
    let streamFailures = 0;
    let confirmed = 0;
    const publishedStates: Array<{ lastPublishedTip?: string }> = [];
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, _tip: string, pack: Readable,
    ) => {
      attempts += 1;
      try {
        await drain(pack);
      } catch (error) {
        streamFailures += 1;
        throw error;
      }
      confirmed += 1;
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-1921" } };
    };
    let firstReturned = false;
    let secondReturned = false;
    let turnFailure: unknown;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "pack must fully settle\n");
      await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
      fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
      commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      firstReturned = true;
      assert.equal(timedOutPacks, 1, "the first checkpoint reached the supervised pack child");
      assert.deepEqual(events.slice(0, 3), ["pack_timeout", "dispose", "reap"],
        "the pack child was disposed and reaped before the checkpoint returned");
      assert.equal(confirmed, 0, "a failed pack never received a broker ACK");
      assert.deepEqual(publishedStates, [], "the failed pack did not advance published-tip state");
      assert.ok(api.states.some((state) => state.runId === ctx.runId && state.body.status === "running" &&
        state.body.milestones_completed?.includes("m1")), "the locally completed milestone progress was reported");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: "milestone_checkpoint" });
      secondReturned = true;
      assert.equal(confirmed, 1, "the retried pack was the only confirmed publication");
      assert.equal(publishedStates.length, 1, "only the ACK advanced published state");
      return { branch: ctx.branch };
    });
    const originalRun = exec.run.bind(exec);
    exec.run = async (ctx) => {
      try { return await originalRun(ctx); }
      catch (error) { turnFailure = error; throw error; }
    };
    const claim = gitlabClaim(1921, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    try {
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, {
        github, codexBoundaryDeadlineMs: 8_000, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
        checkpointTestHooks: {
          softDeadlineMs: 3_000,
          afterUnpinnedPublish: (state) => publishedStates.push(state),
        },
      }).execute(claim);
    } finally {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = originalPublish;
    }
    assert.equal(firstReturned, true, "the soft-skipped pack returned to the executor");
    assert.equal(secondReturned, true, `the next checkpoint retried the committed tip; turn error: ${String(turnFailure)}`);
    assert.equal(attempts, 2, "the broker saw the timed-out stream and the guarded retry");
    assert.equal(streamFailures, 1, "only the timed-out pack stream failed");
    const statusTexts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
    assert.ok(statusTexts.some((line) => line.includes("checkpoint publish skipped: soft deadline")),
      `a source close before typed completion remains a soft skip: ${JSON.stringify(statusTexts)}`);
    assert.ok(!statusTexts.some((line) => line.includes("checkpoint publish failed:")),
      "a premature source-close hint is not misreported as a generic publish failure");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });
});


// M2 acceptance through HTTP -> WorkerClient -> buildRunLaneReconcile -> safety -> runner.
// Each server handles at most two credential requests; the first destroys the reply after
// recording the request, the second returns a fixed API classification. No provider exchange.
