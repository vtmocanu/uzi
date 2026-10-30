import { describe, it, type TestContext } from "node:test";
import { MessageBatcher } from "../src/batcher.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { StateReportUnappliedError, WorkerClient } from "../src/client.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { acceptsHeldCaptureCleanup, CodexBoundaryError } from "../src/codex/safety.js";
import type { BoundaryPermit } from "../src/harness.js";
import { quiesceRunAttempt } from "../src/run-quiescence.js";
import { SdkExecutor, TransientRecoveryError } from "../src/sdk-executor.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { nullLogger } from "./helpers.js";
import { api, baseUrl, client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith, TOKEN, worktreeDirFor } from "./runner-harness.js";
import {
  FakeCodexExecutor,
  codexRig,
  commitInTree,
  enabledRecovery,
  feedTexts,
  installRecoveryRootCleanup,
  spyCustodySettle,
  virtualBoundaryClock,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

// PRD #1733: a provider-origin TransientRecoveryError parks the run recovery_wait typed
// `provider_outage` (with the productive-turn count) whenever the api advertised the cause and the
// report is claim-fenced. Whether the provider was reaped before the report is PROOF for the later
// settlement and cleanup only, never an input to the typing. Both harnesses are driven: a Claude
// stub executor and a Codex executor over the real execution-safety facade.

const FEATURES = ["claim_generation_fence", "recovery_cause_provider_outage"];
const PROGRESS = "provider_progress_on_recovery_park_v1";
const HOLD = "provider_unavailable";

type Harness = "claude" | "codex";
const HARNESSES: Harness[] = ["claude", "codex"];

interface Scenario {
  /** Distinguish committed work on a fresh flight of the same run. */
  work?: string;
  /** Model turns that settle (reach onModelTurnSettled) before the executor throws. */
  turns?: number;
  /** The error the executor throws; default a provider-origin TransientRecoveryError. */
  error?: () => Error;
  /** Make the pre-report provider reap fail. */
  failReap?: boolean;
  clock?: ReturnType<typeof virtualBoundaryClock>;
  afterIdleReport?: () => Promise<void>;
  wrapIdleRefusal?: boolean;
}

interface Rig {
  runner: RunRunner;
  custody: () => number;
  homePath: () => string;
  assertRefusal: () => void;
  archive: ReturnType<typeof enabledRecovery>["archive"];
}

function rigFor(harness: Harness, scenario: Scenario, homeRoot: string): Rig {
  const { gitlab } = fakeGitlab();
  const { coord, archive } = enabledRecovery();
  const error = scenario.error ?? (() => new TransientRecoveryError("provider transient error persisted", { providerOrigin: true }));
  let runHome = "";
  let thrown = false;
  const refusalPhases: string[] = [];
  let heldPermit: BoundaryPermit | undefined;
  let codexOwner: ReturnType<typeof codexRig> | undefined;
  let verifiedCaptures = 0;
  const initialCleanup: boolean[] = [];
  const reportCleanup: boolean[] = [];
  const refusalProofs: { held: boolean; signalLive: boolean; rootsDrained: boolean; reaped: boolean; pending: number; callbacks: number; verifiedCaptures: number; state: string }[] = [];
  const behavior = async (ctx: RunContext): Promise<ExecutorResult> => {
    fs.mkdirSync(runHome, { recursive: true });
    fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
    commitInTree(ctx.worktreePath, "WORK.txt", scenario.work ?? "work before the outage\n");
    for (let i = 0; i < (scenario.turns ?? 0); i++) await ctx.onModelTurnSettled?.(1);
    thrown = true;
    throw error();
  };
  let factory: ExecutorFactory;
  if (harness === "codex") {
    factory = (runId) => {
      runHome = path.join(homeRoot, runId);
      // Replacement executors own fresh provider roots, safety and capture authority.
      const rig = codexRig({ now: scenario.clock?.now, armDeadline: scenario.clock?.armDeadline });
      if (scenario.afterIdleReport) {
        const boundary = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = (req, action) => boundary(req, async permit => {
          try { return await action(permit); }
          catch (error) {
            if (scenario.wrapIdleRefusal && modelThrown) throw new CodexBoundaryError("action", [], error);
            throw error;
          }
          finally { if (modelThrown) await scenario.afterIdleReport!(); }
        });
      }
      let modelThrown = false;
      codexOwner = rig;
      const instanceBehavior = async (ctx: RunContext): Promise<ExecutorResult> => {
        try {
          return await behavior(ctx);
        } catch (err) {
          modelThrown = true;
          throw err;
        }
      };
      if (scenario.failReap) {
        const real = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = async (req, action) => {
          const postThrowShutdown = modelThrown && req.boundary === "shutdown";
          return real(req, async permit => {
            if (!postThrowShutdown) return action(permit);
            heldPermit = permit;
            refusalPhases.push("held");
            try {
              return await action(permit);
            } finally {
              heldPermit = undefined;
              refusalPhases.push("joined");
            }
          });
        };
      }
      return { homeDir: runHome, executor: new FakeCodexExecutor(rig.safety, instanceBehavior, undefined, rig.capture) };
    };
  } else {
    factory = (runId) => {
      runHome = path.join(homeRoot, runId);
      const executor: Executor = { run: behavior };
      return { homeDir: runHome, executor };
    };
  }
  const runner = runnerWith(factory, gitlab, undefined, nullLogger(), {
    recovery: coord, recoveryRetryMs: 5,
    quiesceRun: async req => {
      if (harness !== "codex" || !scenario.failReap || req.site !== "recovery_provider_settle")
        return quiesceRunAttempt(req);
      // Refuse clone quiescence at the actual pre-report site, after local capture.
      // The real safety owner still proves its registered roots drained in this callback.
      refusalPhases.push("pre_report_refused");
      refusalProofs.push({
        held: heldPermit !== undefined,
        signalLive: heldPermit !== undefined && !heldPermit.signal.aborted,
        rootsDrained: Boolean(codexOwner && !codexOwner.registry.hasLiveCommandRoot() && !codexOwner.registry.hasUnreapedBoundaryRoot()),
        reaped: (codexOwner?.reaps() ?? 0) > 0,
        pending: codexOwner?.registry.pendingLaunchCount() ?? -1,
        callbacks: codexOwner?.registry.inFlightCallbackCount() ?? -1,
        verifiedCaptures, state: codexOwner?.registry.state() ?? "absent",
      });
      return {
        process: { state: "unverified", processes: [], killed: [], detail: "pre-settlement clone quiescence refused" },
        docker: { state: "not_wired", removed: [], detail: "fixture has no Docker" },
      };
    },
  });
  if (harness === "codex" && scenario.failReap) {
    const settle = runner["settleCaptureRequest"].bind(runner);
    runner["settleCaptureRequest"] = async (...args) => {
      await settle(...args);
      const held = args[1];
      if (held) {
        const checkpoints = refusalProofs.length ? reportCleanup : initialCleanup;
        checkpoints.push(held.safety === codexOwner?.safety && held.permit === heldPermit &&
          acceptsHeldCaptureCleanup(held.safety, held.permit));
      }
    };
    const capture = runner["captureRecoveryRestorePoint"].bind(runner);
    runner["captureRecoveryRestorePoint"] = async (...args) => {
      const result = await capture(...args);
      if (result.verified) {
        verifiedCaptures++;
        refusalPhases.push("capture_verified");
      }
      return result;
    };
  }
  if (harness === "claude" && scenario.failReap) {
    const r = runner as unknown as { reapForSink: (...a: unknown[]) => Promise<void> };
    const real = r.reapForSink.bind(runner);
    r.reapForSink = async (...a: unknown[]) => {
      if (thrown && (a[1] as { boundary: string }).boundary === "shutdown") throw new Error("provider not reaped");
      return real(...a);
    };
  }
  return {
    runner, archive, custody: spyCustodySettle(runner), homePath: () => runHome,
    assertRefusal: () => {
      if (harness !== "codex") return;
      assert.ok(initialCleanup.length > 0 && initialCleanup.every(Boolean), "the runner used genuine full-root cleanup proof before capture");
      assert.deepEqual(refusalProofs, [{ held: true, signalLive: true, rootsDrained: true, reaped: true, pending: 0, callbacks: 0, verifiedCaptures: 1, state: "closed" }], "one refusal after verified capture inside the live callback");
      assert.deepEqual(reportCleanup, [true], "the real report checkpoint refreshed cleanup after the refused clone proof");
      assert.deepEqual(refusalPhases, ["held", "capture_verified", "pre_report_refused", "joined"]);
    },
  };
}

async function withHome(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-provider-park-"));
  try {
    await fn(homeRoot);
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}

const parks = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "recovery_wait").map((s) => s.body);

describe("RunRunner recovery parks consume this flight's provider progress", () => {
  for (const harness of HARNESSES) {
    for (const kind of ["ordinary", "disk counted", "disk preventive"] as const) {
      for (const turns of [0, 2]) {
        it(`${harness}: ${kind}, ${turns} successful turns, then a fresh zero-progress provider flight`, async () => {
          await withHome(async (homeRoot) => {
            client.protocolFeatures = [...FEATURES, PROGRESS, "recovery_cause_data_volume_full"];
            const first = rigFor(harness, { turns, error: () => kind === "ordinary"
              ? new TransientRecoveryError() : new DiskParkSignal(kind === "disk preventive") }, homeRoot);
            const claim = gitlabClaim(3320, { claim_generation: 3 });
            await first.runner.execute(claim);
            const [park] = parks(claim.run_id);
            assert.ok(park);
            assert.equal(parks(claim.run_id).length, 1);
            assert.equal(park.provider_progress_turns, turns || undefined);
            assert.equal(park.claim_generation, 3);
            assert.equal(park.recovery_cause, kind === "ordinary" ? undefined : "data_volume_full");
            assert.equal(park.disk_park_preventive, kind === "disk preventive" ? true : undefined);
            assert.equal(park.productive_turns, undefined);

            api.setOwnershipStatus(claim.run_id, "running", 4);
            const next = rigFor(harness, { work: "fresh flight before failure\n" }, homeRoot);
            await next.runner.execute({ ...claim, claim_generation: 4 });
            const sent = parks(claim.run_id);
            assert.equal(sent.length, 2);
            assert.equal(sent[1]!.recovery_cause, "provider_outage");
            assert.equal(sent[1]!.productive_turns, 0, "prior flight success never leaks into a fresh flight");
            assert.equal(sent[1]!.provider_progress_turns, undefined);
            assert.equal(sent[1]!.claim_generation, 4);
          });
        });
      }
    }
    for (const scenario of [
      { name: "feature absent", features: FEATURES, generation: 3 },
      { name: "zero generation", features: [...FEATURES, PROGRESS], generation: 0 },
      { name: "unfenced generation", features: [PROGRESS], generation: 3 },
    ]) {
      it(`${harness}: ordinary progress omitted with ${scenario.name}`, async () => {
        await withHome(async (homeRoot) => {
          client.protocolFeatures = scenario.features;
          const rig = rigFor(harness, { turns: 1, error: () => new TransientRecoveryError() }, homeRoot);
          const claim = gitlabClaim(3321, { claim_generation: scenario.generation });
          await rig.runner.execute(claim);
          assert.equal(parks(claim.run_id).length, 1);
          assert.equal(parks(claim.run_id)[0]!.provider_progress_turns, undefined);
        });
      });
    }
  }
  it("credential deferral without its cause feature stays untyped and carries no progress", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = [...FEATURES, PROGRESS];
      const rig = rigFor("codex", { turns: 2, error: () =>
        new CodexBoundaryError("reconcile", [], undefined, "vault_locked") }, homeRoot);
      const claim = gitlabClaim(3322, { claim_generation: 3 });
      await rig.runner.execute(claim);
      assert.equal(parks(claim.run_id).length, 1);
      assert.equal(parks(claim.run_id)[0]!.recovery_cause, undefined);
      assert.equal(parks(claim.run_id)[0]!.provider_progress_turns, undefined);
    });
  });
});

describe("RunRunner #1733 — a provider outage parks typed provider_outage", () => {
  for (const harness of HARNESSES) {
    it(`${harness}: sends recovery_cause provider_outage with productive_turns and the fence, then settles on positive proof`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 2 }, homeRoot);
        const claim = gitlabClaim(3301, { claim_generation: 3 });
        await rig.runner.execute(claim);
        const sent = parks(claim.run_id);
        assert.equal(sent.length, 1);
        assert.equal(sent[0]!.recovery_cause, "provider_outage");
        assert.equal(sent[0]!.productive_turns, 2, "only the two settled turns count");
        assert.equal(sent[0]!.claim_generation, 3);
        assert.equal(rig.custody(), 1, "a reaped provider settles the generation once");
        assert.equal(fs.existsSync(rig.homePath()), true, "HOME survives the park");
        assert.ok(feedTexts(claim.run_id).some((t) => /provider outage/.test(t)));
      });
    });

    // R1 regression: the reap result must not decide the typing, or the park is untyped and
    // uncounted exactly when the provider could not be reaped.
    it(`${harness}: R1 a failed reap still sends the typed counted park, keeps clone and HOME, and settles nothing`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 1, failReap: true }, homeRoot);
        const claim = gitlabClaim(3302, { claim_generation: 3 });
        await rig.runner.execute(claim);
        const sent = parks(claim.run_id);
        assert.equal(sent.length, 1);
        assert.equal(sent[0]!.recovery_cause, "provider_outage");
        assert.equal(sent[0]!.productive_turns, 1);
        rig.assertRefusal();
        assert.equal(rig.custody(), 0, "no settlement without positive proof");
        assert.equal(fs.existsSync(worktreeDirFor(3302)), true, "the clone is kept");
        assert.equal(fs.existsSync(rig.homePath()), true, "HOME is kept");
        assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false);
      });
    });

    it(`${harness}: R1 a failed reap and a paused/provider_unavailable ack is still an accepted park that keeps everything`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 3, failReap: true }, homeRoot);
        const claim = gitlabClaim(3303, { claim_generation: 3, inventory_guarded: true });
        api.ackTypedParkAs(claim.run_id, "paused", HOLD);
        await rig.runner.execute(claim);
        const sent = parks(claim.run_id);
        assert.equal(sent.length, 1);
        assert.equal(sent[0]!.recovery_cause, "provider_outage");
        assert.equal(sent[0]!.productive_turns, 3);
        rig.assertRefusal();
        assert.equal(rig.custody(), 0);
        assert.equal(fs.existsSync(worktreeDirFor(3303)), true);
        assert.equal(fs.existsSync(rig.homePath()), true);
        assert.ok(feedTexts(claim.run_id).some((t) => /waits for its owner/.test(t)), "the hold is said on the feed");
      });
    });

    it(`${harness}: a paused/provider_unavailable ack keeps clone and HOME and settles only a guarded claim`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const unguarded = rigFor(harness, { turns: 1 }, homeRoot);
        const a = gitlabClaim(3304, { claim_generation: 3 });
        api.ackTypedParkAs(a.run_id, "paused", HOLD);
        await unguarded.runner.execute(a);
        assert.equal(unguarded.custody(), 0, "an unguarded claim keeps custody without settling");
        assert.equal(fs.existsSync(worktreeDirFor(3304)), true, "a held run keeps its clone for the owner's resume");
        assert.equal(fs.existsSync(unguarded.homePath()), true);

        const guarded = rigFor(harness, { turns: 1 }, homeRoot);
        const b = gitlabClaim(3305, { claim_generation: 3, inventory_guarded: true });
        api.ackTypedParkAs(b.run_id, "paused", HOLD);
        await guarded.runner.execute(b);
        assert.equal(guarded.custody(), 1, "the credential-free guarded path settles on positive proof");
        assert.equal(fs.existsSync(worktreeDirFor(3305)), true);
      });
    });

    // The ownership read must happen AFTER the typed report is sent and its ack lost, so the
    // handler reaches the lost-ack branch. The flip happens inside the drop matcher.
    it(`${harness}: a lost ack followed by a paused/provider_unavailable ownership read is the accepted park`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 1 }, homeRoot);
        const claim = gitlabClaim(3330, { claim_generation: 3 });
        api.setOwnershipStatus(claim.run_id, "running", 3);
        const stop = api.dropStatesWhen(claim.run_id, (b) => {
          if (b.recovery_cause !== "provider_outage") return false;
          api.setOwnershipStatus(claim.run_id, "paused", 3, undefined, HOLD);
          return true;
        });
        try {
          await rig.runner.execute(claim);
        } finally {
          stop();
        }
        assert.equal(parks(claim.run_id).length, 0, "the typed report was dropped before it was recorded");
        assert.equal(fs.existsSync(worktreeDirFor(3330)), true, "a held run keeps its clone");
        assert.equal(fs.existsSync(rig.homePath()), true, "HOME is kept");
        assert.equal(rig.custody(), 0);
        assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false);
      });
    });

    it(`${harness}: a lost ack, an unproven reap and a recovery_wait ownership read keeps the clone`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 1, failReap: true }, homeRoot);
        const claim = gitlabClaim(3331, { claim_generation: 3 });
        api.setOwnershipStatus(claim.run_id, "running", 3);
        const stop = api.dropStatesWhen(claim.run_id, (b) => {
          if (b.recovery_cause !== "provider_outage") return false;
          api.setOwnershipStatus(claim.run_id, "recovery_wait", 3);
          return true;
        });
        try {
          await rig.runner.execute(claim);
        } finally {
          stop();
        }
        rig.assertRefusal();
        assert.equal(fs.existsSync(worktreeDirFor(3331)), true, "clone kept: the reap was never proven");
        assert.equal(fs.existsSync(rig.homePath()), true, "HOME is kept");
        assert.equal(rig.custody(), 0, "no settlement without positive proof");
        assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false);
      });
    });
  }

  it("provider-hold continuation at the owner's scope ceiling still requests a permit and publishes a non-closing partial", async () => {
    await withHome(async homeRoot => {
      api.setRegisterProtocolFeatures([...FEATURES, "completion_non_closing_v1"]);
      await client.register("provider-scope-continuation");
      const claim = gitlabClaim(3310, {
        claim_generation: 3,
        plan_source: "seeded",
        plan_approved: true,
        plan_md: "# Approved work",
        milestones: [{ id: "m1", title: "Delivered" }, { id: "m2", title: "Deferred" }],
        config: { completion_contract_version: 1, contract_revision: 1 },
      });
      api.setPublicationState(claim.run_id, [], 1);
      const parked = rigFor("claude", { turns: 1 }, homeRoot);
      api.ackTypedParkAs(claim.run_id, "paused", HOLD);
      await parked.runner.execute(claim);
      assert.equal(parks(claim.run_id).length, 1);
      assert.equal(api.completionPermitRequests.length, 0, "a provider hold does not finalize work");
      assert.equal(fs.existsSync(worktreeDirFor(3310)), true);

      // Model the server's next claim after Stop caps scope and Resume re-queues the same run.
      // The real HTTP Stop/Resume transitions are covered in provider_hold_stop_livedb_test.go.
      const resumed = { ...claim, claim_generation: 4, resume_phase: "implementing" };
      api.setOwnershipStatus(claim.run_id, "running", 4);
      api.setStateAckScope(claim.run_id, { scope_ceiling: 1, milestones_completed: ["m1"] });
      api.setCompletionPermitResponse(true);
      const forge = fakeGitlab();
      const pushedHead = followSuccessfulPush(git, forge.pr);
      let modelQueries = 0;
      const continuation = runnerWith(() => ({
        homeDir: parked.homePath(),
        executor: new SdkExecutor(nullLogger(), parked.homePath(), {
          queryFn: () => { modelQueries++; throw new Error("scope ceiling must stop before querying the provider"); },
        }),
      }), forge.gitlab);
      await continuation.execute(resumed);
      assert.equal(modelQueries, 0, "the completed scope ceiling is honored before further model work");
      assert.equal(api.completionPermitRequests.length, 1, "continuation keeps the completion interlock");
      const permit = api.completionPermitRequests[0]!.body;
      assert.equal(permit.claim_generation, 4);
      assert.equal(permit.scope_capped, true);
      assert.equal(permit.head, pushedHead());
      const terminal = api.states.filter(s => s.runId === claim.run_id && s.body.status === "completed").at(-1)?.body;
      assert.ok(terminal, "the resumed partial completed after permit and head verification");
      assert.equal(terminal.scope_capped, true);
      assert.equal(terminal.head, pushedHead());
      const post = forge.calls.find(c => c.method === "POST");
      assert.ok(post, "the retained committed work was published");
      const body = JSON.parse(post.body ?? "{}") as { title: string; description: string };
      assert.match(body.title, /^\[partial\] /);
      assert.doesNotMatch(body.description, /Closes #3310/);
      assert.doesNotMatch(forge.pr.description, /Closes #3310/, "head verification keeps the final description non-closing");
      assert.equal(api.completionHoldRequests.length, 0);
      assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.status === "failed"), false);
    });
  });

  it("a paused ack without the provider hold reason is NOT an accepted park", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = FEATURES;
      const rig = rigFor("claude", { turns: 1 }, homeRoot);
      const claim = gitlabClaim(3307, { claim_generation: 3 });
      api.ackTypedParkAs(claim.run_id, "paused");
      await rig.runner.execute(claim);
      assert.equal(rig.custody(), 0);
      assert.equal(feedTexts(claim.run_id).some((t) => /provider outage/.test(t)), false);
    });
  });

  it("a lost ack followed by a paused ownership read WITHOUT the provider hold is not the accepted park", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = FEATURES;
      const rig = rigFor("claude", { turns: 1 }, homeRoot);
      const claim = gitlabClaim(3308, { claim_generation: 3, inventory_guarded: true });
      api.setOwnershipStatus(claim.run_id, "running", 3, true);
      let matchedTypedReport = false;
      // Flip ownership after the typed report so the lost-ack path runs; a guarded claim
      // would enter settlement if paused without the provider hold were wrongly accepted.
      const stop = api.dropStatesWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "provider_outage") return false;
        matchedTypedReport = true;
        api.setOwnershipStatus(claim.run_id, "paused", 3, true);
        return true;
      });
      try {
        await rig.runner.execute(claim);
      } finally {
        stop();
      }
      assert.equal(matchedTypedReport, true, "the typed provider-outage report reached the drop matcher");
      assert.equal(parks(claim.run_id).length, 0, "the typed report was dropped before it was recorded");
      assert.equal(rig.custody(), 0, "nothing settled: the ownership read is not the provider hold");
      assert.equal(feedTexts(claim.run_id).some((t) => /provider outage/.test(t)), false);
    });
  });

  describe("what stays untyped", () => {
    const untyped = (runId: string) => {
      const sent = parks(runId);
      assert.equal(sent.length, 1, "the park itself still happens");
      assert.equal(sent[0]!.recovery_cause, undefined);
      assert.equal(sent[0]!.productive_turns, undefined);
    };

    it("an empty-turn exhaustion", async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor("claude", { error: () => new TransientRecoveryError() }, homeRoot);
        const claim = gitlabClaim(3310, { claim_generation: 3 });
        await rig.runner.execute(claim);
        untyped(claim.run_id);
      });
    });

    for (const reason of ["could not read plan-gate inputs after the resume", "the plan gate could not be re-presented"]) {
      it(`the gate-recovery reason "${reason}"`, async () => {
        await withHome(async (homeRoot) => {
          client.protocolFeatures = FEATURES;
          const rig = rigFor("claude", { error: () => new TransientRecoveryError(reason) }, homeRoot);
          const claim = gitlabClaim(3311, { claim_generation: 3 });
          await rig.runner.execute(claim);
          untyped(claim.run_id);
        });
      });
    }

    it("a provider outage when the api did not advertise the cause", async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = ["claim_generation_fence"];
        const rig = rigFor("claude", { turns: 1 }, homeRoot);
        const claim = gitlabClaim(3312, { claim_generation: 3 });
        await rig.runner.execute(claim);
        untyped(claim.run_id);
      });
    });

    it("a provider outage on a claim without a positive generation", async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor("claude", { turns: 1 }, homeRoot);
        const claim = gitlabClaim(3313, { claim_generation: 0 });
        await rig.runner.execute(claim);
        untyped(claim.run_id);
      });
    });
  });

  it("an old api that strict-decodes the typed body gets one untyped re-send with the fence, and only this feature is dropped", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = [...FEATURES, "recovery_park_cause"];
      const rig = rigFor("claude", { turns: 1 }, homeRoot);
      const claim = gitlabClaim(3314, { claim_generation: 3 });
      api.refuseTypedParkWith(claim.run_id, "invalid request body");
      await rig.runner.execute(claim);
      assert.equal(api.refusedTypedParks.length, 1, "the typed body is tried once");
      const sent = parks(claim.run_id);
      assert.equal(sent.length, 1, "the untyped re-send lands the park");
      assert.equal(sent[0]!.recovery_cause, undefined);
      assert.equal(sent[0]!.productive_turns, undefined);
      assert.equal(sent[0]!.claim_generation, 3, "the fence is kept on the re-send");
      assert.equal(client.hasFeature("recovery_cause_provider_outage"), false);
      assert.equal(client.hasFeature("recovery_park_cause"), true, "every other negotiated feature stays");
      assert.equal(client.hasFeature("claim_generation_fence"), true);
    });
  });

  it("a current api's validation 400 is not a rollback: no untyped re-send, nothing dropped, the typed retry lands", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = FEATURES;
      const rig = rigFor("claude", { turns: 1 }, homeRoot);
      const claim = gitlabClaim(3315, { claim_generation: 3 });
      api.refuseTypedParkWith(claim.run_id, "state must be one of: running, recovery_wait", 1);
      await rig.runner.execute(claim);
      assert.equal(api.refusedTypedParks.length, 1);
      const sent = parks(claim.run_id);
      assert.equal(sent.length, 1, "the park loop's own retry is the only further report");
      assert.equal(sent[0]!.recovery_cause, "provider_outage", "still typed: the refusal was not read as a rollback");
      assert.equal(client.hasFeature("recovery_cause_provider_outage"), true);
    });
  });
});


describe("M2 Codex idle typed ACK and custody", () => {
  const feature = "recovery_cause_codex_idle_stall_v1";
  for (const kind of ["zero_evidence", "productive"] as const) for (const turns of [0, 2]) {
    it(`M2 ${kind} sends settled progress=${turns} and uses ordinary settlement`, async t => {
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature, "run_checkpoint_durability"];
        t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof client.publishCheckpoint>) => {
          for await (const chunk of args[2]) void chunk;
          return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/idle-fixture" } };
        });
        api.stateAckWire = (body, run) => body.recovery_cause === "codex_idle_stall"
          ? { status: 200, body: { run: { ...run, recovery_wait_cause: "codex_idle_stall",
              recovery_retry_not_before: "2026-10-28T00:00:00Z" } } } : undefined;
        const rig = rigFor("codex", { turns, error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
        const claim = gitlabClaim(3509, { claim_generation: 3 });
        const providerDisposition = rig.runner["dispositionProviderPark"];
        rig.runner["dispositionProviderPark"] = async () => { throw new Error("idle must not use provider disposition"); };
        try { await rig.runner.execute(claim); }
        finally { rig.runner["dispositionProviderPark"] = providerDisposition; t.mock.restoreAll(); }
        assert.equal(parks(claim.run_id).length, 1);
        const [park] = parks(claim.run_id);
        assert.equal(park?.recovery_cause, "codex_idle_stall");
        assert.equal(park?.provider_progress_turns, turns);
        assert.equal(park?.productive_turns, undefined);
        assert.equal(park?.claim_generation, 3);
        assert.equal(rig.custody(), 1, "ordinary exact-generation settlement");
        const opening = kind === "zero_evidence" ? "provider sent no model output" : "model output stalled";
        assert.ok(feedTexts(claim.run_id).includes(
          `paused after ${opening}; the recovery checkpoint is published and it resumes automatically`,
        ));
      });
    });
  }

  for (const kind of ["zero_evidence", "productive"] as const) for (const held of [false, true]) {
    it(`M2 ${kind} ${held ? "held" : "timed"} ordinary checkpoint HTTP507 still accepts typed idle and retains source and custody`, async t => {
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature, "run_checkpoint_durability"];
        api.checkpointPublishHttpStatus = 507;
        api.stateAckWire = (body, run) => body.recovery_cause === "codex_idle_stall"
          ? { status: 200, body: { run: { ...run, recovery_wait_cause: "codex_idle_stall",
              ...(held ? {} : { recovery_retry_not_before: "2026-10-28T00:00:00Z" }) } } } : undefined;
        const rig = rigFor("codex", { error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
        const publish = client.publishCheckpoint.bind(client);
        const outcomes: unknown[] = [];
        t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
          const outcome = await publish(...args);
          outcomes.push(outcome);
          return outcome;
        });
        const claim = gitlabClaim(3511, { claim_generation: 3 });
        try { await rig.runner.execute(claim); }
        finally { t.mock.restoreAll(); }
        assert.deepEqual(outcomes, Array.from({ length: 3 }, () => ({ ok: false, httpStatus: 507 })),
          "ordinary checkpoint broker exhausts its bounded retries, not archive upload");
        assert.equal(parks(claim.run_id).length, 1);
        assert.equal(parks(claim.run_id)[0]?.recovery_cause, "codex_idle_stall");
        assert.ok(fs.existsSync(worktreeDirFor(3511)));
        assert.ok(fs.existsSync(rig.homePath()));
        assert.deepEqual(rig.archive.releaseCalls, [], "checkpoint refusal never releases custody");
        const texts = feedTexts(claim.run_id);
        const opening = kind === "zero_evidence" ? "provider sent no model output" : "model output stalled";
        assert.ok(texts.includes(
          `paused after ${opening}; the recovery checkpoint is saved on this worker and ${held ? "the run waits for its owner to resume it" : "it resumes automatically"}`,
        ));
        assert.ok(!texts.some(text => /checkpoint is published/.test(text)), "local capture is never described as published");
        if (held) assert.ok(!texts.some(text => /resumes automatically/.test(text)));
      });
    });
  }

  for (const { ack, kind } of [
    ...(["held", "held null", "missing cause", "wrong cause", "statusless", "strict refusal", "validation refusal"] as const)
      .map(ack => ({ ack, kind: "productive" as const })),
    ...(["held", "held null", "strict refusal", "validation refusal"] as const)
      .map(ack => ({ ack, kind: "zero_evidence" as const })),
  ]) {
    it(`M2 ${kind} ${ack} keeps source with no false automatic-resume promise`, async t => {
      const held = ack === "held" || ack === "held null";
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature, "run_checkpoint_durability"];
        t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof client.publishCheckpoint>) => {
          for await (const chunk of args[2]) void chunk;
          return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/idle-fixture" } };
        });
        api.stateAckWire = (body, run) => {
          if (body.recovery_cause !== "codex_idle_stall") return undefined;
          return { status: 200, body: { run: {
            ...run, ...(ack === "statusless" ? { status: undefined } : {}),
            ...(ack === "held null" ? { recovery_retry_not_before: null } : {}),
            recovery_wait_cause: held ? "codex_idle_stall" : ack === "wrong cause" ? "provider_outage" : undefined,
          } } };
        };
        const refusal = ack.endsWith("refusal");
        const rig = rigFor("codex", { turns: refusal ? 2 : 0,
          error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
        const claim = gitlabClaim(3510, { claim_generation: 3 });
        const refusedBodies: Parameters<typeof client.reportState>[1][] = [];
        const refuseNext = () => api.failStateWhen(claim.run_id, body => {
          if (body.status !== "recovery_wait") return false;
          refusedBodies.push({ ...body });
          return true;
        }, { httpStatus: 400, error: ack === "strict refusal" ? "invalid request body" : "invalid progress" });
        if (refusal) refuseNext();
        let reports = 0;
        const reportBodies: Parameters<typeof client.reportState>[1][] = [];
        const report = client.reportState.bind(client);
        t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
          if (args[1].status === "recovery_wait") {
            reports++;
            reportBodies.push({ ...args[1] });
            assert.equal(args[1].recovery_cause, "codex_idle_stall", "no untyped resend");
          }
          return report(...args);
        });
        const wait = rig.runner["waitRecoveryRetry"].bind(rig.runner);
        let waits = 0;
        t.mock.method(rig.runner as unknown as { waitRecoveryRetry: typeof wait }, "waitRecoveryRetry", async (...args: Parameters<typeof wait>) => {
          waits++;
          assert.equal(reports, waits);
          if (refusal && waits === 1) {
            assert.equal(refusedBodies.length, 1, "first actual HTTP request refused");
            refuseNext();
            return;
          }
          // Refusals get two actual requests without backoff; shutdown bounds all other retries at one.
          rig.runner.shutdown();
          return wait(...args);
        });
        try {
          const executing = rig.runner.execute(claim);
          if (held || ack === "strict refusal") await executing;
          else await assert.rejects(executing, { name: "AbortError" });
        } finally { rig.runner.shutdown(); t.mock.restoreAll(); }
        assert.equal(reports, ack === "strict refusal" ? 1 : refusal ? 2 : 1);
        assert.equal(waits, held || ack === "strict refusal" ? 0 : refusal ? 2 : 1);
        if (refusal) {
          assert.equal(refusedBodies.length, ack === "strict refusal" ? 1 : 2, "actual HTTP refusal count");
          for (const body of [...reportBodies, ...refusedBodies]) {
            assert.equal(body.recovery_cause, "codex_idle_stall", "no untyped fallback, including internal client resends");
            assert.equal(body.claim_generation, 3);
            assert.equal(body.provider_progress_turns, 2, "settled progress is independent of idle kind");
            assert.equal(body.productive_turns, undefined);
            assert.deepEqual(body, refusedBodies[0], "the complete typed request stays frozen across retry");
          }
        }
        assert.equal(parks(claim.run_id).length, refusal ? 0 : 1, "refusal never persists");
        assert.equal(rig.custody(), held ? 1 : 0, "only exact typed ACK authorizes settlement");
        assert.ok(fs.existsSync(worktreeDirFor(3510)), "source retained");
        assert.ok(fs.existsSync(rig.homePath()), "HOME retained");
        assert.ok(!feedTexts(claim.run_id).some(t => /resumes automatically/.test(t)));
        if (held) {
          const opening = kind === "zero_evidence" ? "provider sent no model output" : "model output stalled";
          assert.ok(feedTexts(claim.run_id).includes(
            `paused after ${opening}; the recovery checkpoint is published and the run waits for its owner to resume it`,
          ));
        }
      });
    });
  }
});


const IDLE_UNCONFIRMED = "idle recovery park outcome unconfirmed; the source and session are retained; the accepted cause and whether recovery is held or timed are unknown";
const IDLE_REFUSED = "idle recovery park report refused without application; the source and session are retained";

function rawIdleNotices(t: TestContext): string[] {
  const raw: string[] = [];
  const emit = MessageBatcher.prototype.emit;
  t.mock.method(MessageBatcher.prototype, "emit", function(this: MessageBatcher, msg: Parameters<typeof emit>[0]) {
    if (msg.kind === "status" && typeof msg.payload.text === "string") raw.push(msg.payload.text);
    return emit.call(this, msg);
  });
  return raw;
}

describe("3617 idle park delivery evidence", () => {
  const feature = "recovery_cause_codex_idle_stall_v1";
  for (const kind of ["zero_evidence", "productive"] as const) {
    for (const generation of [3, 4, undefined]) for (const held of [false, true]) {
      it(`3617 ${kind} lost ACK ownership generation=${generation} held=${held}`, async t => {
        await withHome(async home => {
          client.protocolFeatures = ["claim_generation_fence", feature];
          const clock = virtualBoundaryClock();
          t.mock.method(Date, "now", clock.now);
          const rig = rigFor("codex", { clock, turns: 2,
            error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
          const claim = gitlabClaim(36171, { claim_generation: 3 });
          const raw = rawIdleNotices(t);
          let posts = 0;
          const stop = api.dropStatesWhen(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return false;
            return ++posts > 1;
          });
          api.onState(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return;
            api.ownershipWire.set(claim.run_id, response => {
              response.writeHead(200, { "Content-Type": "application/json" });
              response.end(JSON.stringify({
                status: "recovery_wait", claim_generation: generation, inventory_guarded: true,
                ...(held ? {} : { recovery_retry_not_before: "2026-10-28T00:00:00Z" }),
              }));
            });
          });
          api.afterPersistState(claim.run_id, body => body.recovery_cause === "codex_idle_stall", "drop");
          let waits = 0;
          t.mock.method(rig.runner as unknown as { waitRecoveryRetry: () => Promise<void> }, "waitRecoveryRetry",
            async () => { waits++; clock.advance(5); });
          try { await rig.runner.execute(claim); } finally { stop(); }
          assert.equal(posts, 3, "all bounded client attempts retain the typed body");
          assert.equal(waits, 1, "the runner's bounded retry remains in use");
          assert.equal(parks(claim.run_id).length, 1, "first report applied, later requests dropped before application");
          assert.equal(parks(claim.run_id)[0]?.claim_generation, 3);
          assert.equal(feedTexts(claim.run_id).filter(text => text === IDLE_UNCONFIRMED).length, generation === 3 ? 1 : 0);
          assert.equal(raw.filter(text => text === IDLE_UNCONFIRMED).length, generation === 3 ? 1 : 0);
          assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED));
          assert.ok(!feedTexts(claim.run_id).some(text => /resumes automatically|waits for its owner/.test(text)));
          assert.equal(rig.custody(), 0);
          assert.deepEqual(rig.archive.releaseCalls, []);
          assert.equal(fs.readFileSync(path.join(worktreeDirFor(36171), "WORK.txt"), "utf8"), "work before the outage\n");
          assert.equal(fs.readFileSync(path.join(rig.homePath(), "session"), "utf8"), "transcript");
          assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed"].includes(s.body.status ?? "")));
        });
      });
    }

    it(`3617 ${kind} lost ACK exact-cause replay remains accepted`, async () => {
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature];
        const rig = rigFor("codex", {
          error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
        const claim = gitlabClaim(36175, { claim_generation: 3 });
        api.afterPersistState(claim.run_id, body => body.recovery_cause === "codex_idle_stall", "drop");
        api.stateAckWire = (body, run) => body.recovery_cause === "codex_idle_stall"
          ? { status: 200, body: { run: { ...run, recovery_wait_cause: "codex_idle_stall",
              recovery_retry_not_before: "2026-10-28T00:00:00Z" } } } : undefined;
        await rig.runner.execute(claim);
        assert.equal(parks(claim.run_id).length, 2, "the actual applied POST is replayed after losing its ACK");
        assert.equal(rig.custody(), 1);
        assert.ok(!feedTexts(claim.run_id).includes(IDLE_UNCONFIRMED));
        assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED));
        assert.ok(feedTexts(claim.run_id).some(text => /resumes automatically/.test(text)));
      });
    });

    it(`3617 ${kind} complete refusal joins before notice and stops promptly`, async t => {
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature];
        const clock = virtualBoundaryClock();
        t.mock.method(Date, "now", clock.now);
        let release!: () => void, entered!: () => void;
        const held = new Promise<void>(resolve => { release = resolve; });
        const joining = new Promise<void>(resolve => { entered = resolve; });
        let refused = false, done = false, posts = 0, publicErrors = 0, retryWaits = 0;
        const rig = rigFor("codex", { clock, turns: 2,
          error: () => new TransientRecoveryError("idle", { idleStallKind: kind }),
          afterIdleReport: async () => { if (refused) { entered(); await held; } },
          wrapIdleRefusal: true,
        }, home);
        const claim = gitlabClaim(36172, { claim_generation: 3 });
        const raw = rawIdleNotices(t);
        api.failStateWhen(claim.run_id, body => {
          if (body.recovery_cause !== "codex_idle_stall") return false;
          posts++;
          refused = true;
          return true;
        }, { httpStatus: 400, error: "invalid request body" });
        t.mock.method(rig.runner as unknown as { waitRecoveryRetry: () => Promise<void> }, "waitRecoveryRetry",
          async () => { retryWaits++; throw new Error("definitive refusal must not enter the recovery retry wait"); });
        const report = client.reportState.bind(client);
        t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
          try { return await report(...args); }
          catch (error) {
            if (error instanceof StateReportUnappliedError) {
              assert.equal(error.runId, claim.run_id);
              publicErrors++; refused = true;
            }
            throw error;
          }
        });
        const started = clock.now();
        const operation = rig.runner.execute(claim).finally(() => { done = true; });
        try {
          await Promise.race([joining, operation.then(() => { throw new Error("execute returned before join barrier"); })]);
          assert.equal(done, false);
          assert.equal(publicErrors, 1, "public error comes from the actual authenticated client");
          assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED), "no joined notice inside held cleanup");
          assert.ok(!raw.includes(IDLE_REFUSED), "no notice emitted before join");
          clock.advance(1000);
        } finally { release(); await operation; }
        assert.ok(clock.now() - started < 120_000, "retained stop precedes both original deadlines");
        assert.equal(retryWaits, 0, "a definitive refusal never enters the retry wait");
        assert.equal(posts, 1);
        assert.equal(parks(claim.run_id).length, 0);
        assert.equal(feedTexts(claim.run_id).filter(text => text === IDLE_REFUSED).length, 1);
        assert.equal(raw.filter(text => text === IDLE_REFUSED).length, 1);
        assert.ok(!feedTexts(claim.run_id).includes(IDLE_UNCONFIRMED));
        const baseline = { states: api.states.length, custody: rig.custody(), releases: rig.archive.releaseCalls.length,
          source: fs.readFileSync(path.join(worktreeDirFor(36172), "WORK.txt"), "utf8"),
          session: fs.readFileSync(path.join(rig.homePath(), "session"), "utf8") };
        clock.advance(240_000);
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.deepEqual({ states: api.states.length, custody: rig.custody(), releases: rig.archive.releaseCalls.length,
          source: fs.readFileSync(path.join(worktreeDirFor(36172), "WORK.txt"), "utf8"),
          session: fs.readFileSync(path.join(rig.homePath(), "session"), "utf8") }, baseline);
        assert.equal(posts, 1, "no later refused POST after the retained stop");
        assert.equal(baseline.source, "work before the outage\n");
        assert.equal(baseline.session, "transcript");
        assert.equal(baseline.custody, 0);
        assert.equal(baseline.releases, 0);
        assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed"].includes(s.body.status ?? "")));
      });
    });

    for (const control of ["shutdown", "shutdown fractional budget", "cancel", "fence"] as const) {
      it(`3617 ${kind} refusal join closes its batcher after ${control}`, async t => {
        await withHome(async home => {
          client.protocolFeatures = ["claim_generation_fence", feature];
          api.strictReceiptGenerations = true;
          const clock = virtualBoundaryClock();
          t.mock.method(Date, "now", clock.now);
          let release!: () => void, entered!: () => void;
          const held = new Promise<void>(resolve => { release = resolve; });
          const joining = new Promise<void>(resolve => { entered = resolve; });
          let refused = false, joined = false, done = false, posts = 0, closes = 0, closedBeforeJoin = false;
          const rig = rigFor("codex", { clock, turns: 2,
            error: () => new TransientRecoveryError("idle", { idleStallKind: kind }),
            afterIdleReport: async () => { if (refused) { entered(); await held; joined = true; } },
            wrapIdleRefusal: true,
          }, home);
          const claim = gitlabClaim(36178, { claim_generation: 3 });
          api.setInputClaimGeneration(claim.run_id, 3);
          const recovery = rig.runner["handleRecoveryExhausted"].bind(rig.runner);
          let flight: Parameters<typeof recovery>[2] | undefined;
          let ownerBatcher: MessageBatcher | undefined;
          rig.runner["handleRecoveryExhausted"] = async (...args: Parameters<typeof recovery>) => {
            flight = args[2]; ownerBatcher = args[4];
            return recovery(...args);
          };
          const closeSignals: Array<AbortSignal | undefined> = [];
          const close = MessageBatcher.prototype.close;
          t.mock.method(MessageBatcher.prototype, "close", async function(this: MessageBatcher, ...args: Parameters<typeof close>) {
            if (this === ownerBatcher) {
              closedBeforeJoin ||= !joined;
              closes++; closeSignals.push(args[0]);
            }
            return close.apply(this, args);
          });
          api.failStateWhen(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return false;
            posts++; refused = true; return true;
          }, { httpStatus: 400, error: "invalid request body" });
          let budgetNow = 0.25;
          const budget = control === "shutdown fractional budget" ? new MaintenanceBudget(600_000, () => budgetNow) : undefined;
          const operation = (budget ? MaintenanceBudget.run(budget, () => rig.runner.execute(claim)) : rig.runner.execute(claim))
            .finally(() => { done = true; budget?.dispose(); });
          // Attach the rejection observer before delivering shutdown.
          const outcome = operation.then(() => undefined, error => error);
          try {
            await Promise.race([joining, operation.then(() => { throw new Error("execute returned before join barrier"); })]);
            assert.equal(done, false);
            assert.equal(closes, 0);
            assert.equal(posts, 1);
            assert.ok(flight);
            if (control.startsWith("shutdown")) rig.runner.shutdown();
            else {
              const delivered = flight.steering.terminalLifecycleSignal();
              const observed = new Promise<void>((resolve, reject) => {
                const timer = setTimeout(() => {
                  delivered.removeEventListener("abort", finish);
                  reject(new Error("control was not delivered"));
                }, 2000);
                const finish = (): void => { clearTimeout(timer); resolve(); };
                if (delivered.aborted) finish();
                else delivered.addEventListener("abort", finish, { once: true });
              });
              if (control === "fence") {
                api.setInputClaimGeneration(claim.run_id, 4);
                api.setInputFenceReason(claim.run_id, "released");
              }
              api.setInputs(claim.run_id, [{ id: 1, kind: control === "cancel" ? "cancel" : "follow_up" }]);
              await observed;
              assert.equal(control === "cancel" ? flight.steering.isCancelled() : flight.steering.claimFence() !== undefined, true);
            }
            // Exhaust the old capture and shutdown grace while cleanup is held.
            if (budget) budgetNow += 599_999.5;
            else clock.advance(240_000);
            assert.equal(done, false);
            assert.equal(closes, 0);
          } finally { release(); await outcome; }
          const error: unknown = await outcome;
          if (control.startsWith("shutdown")) assert.ok(error instanceof Error && error.name === "AbortError", String(error));
          else assert.equal(error, undefined);
          assert.equal(closedBeforeJoin, false, "never close inside held cleanup before join");
          assert.equal(closes, 1, "one post-join close, including control exits bypassing the refusal notice");
          if (control.startsWith("shutdown")) {
            assert.ok(closeSignals[0], "shutdown cleanup has an explicit bounded close signal");
            assert.equal(closeSignals[0].aborted, true, "expired grace or less than one remaining millisecond grants no fresh flush allowance");
          }
          assert.equal(posts, 1);
          assert.equal(parks(claim.run_id).length, 0);
          assert.equal(rig.custody(), 0);
          assert.deepEqual(rig.archive.releaseCalls, []);
          assert.equal(fs.readFileSync(path.join(worktreeDirFor(36178), "WORK.txt"), "utf8"), "work before the outage\n");
          assert.equal(fs.readFileSync(path.join(rig.homePath(), "session"), "utf8"), "transcript");
          if (control === "cancel") assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.failure_reason === "run cancelled"));
          else assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed"].includes(s.body.status ?? "")));
        });
      });
    }

    it(`3617 ${kind} disposition-only free fallback refuses after capture join and closes its batcher`, async t => {
      await withHome(async home => {
        client.protocolFeatures = ["claim_generation_fence", feature];
        const clock = virtualBoundaryClock();
        t.mock.method(Date, "now", clock.now);
        let release!: () => void, entered!: () => void;
        const held = new Promise<void>(resolve => { release = resolve; });
        const joining = new Promise<void>(resolve => { entered = resolve; });
        let captures = 0, joined = false, posts = 0, publicErrors = 0, closes = 0, waits = 0;
        const rig = rigFor("codex", { clock, turns: 2,
          error: () => new TransientRecoveryError("idle", { idleStallKind: kind }),
          afterIdleReport: async () => {
            if (captures === 1 && !joined) { entered(); await held; joined = true; }
          },
        }, home);
        const claim = gitlabClaim(36177, { claim_generation: 3 });
        const raw = rawIdleNotices(t);
        const capture = rig.runner["captureRecoveryRestorePoint"].bind(rig.runner);
        rig.runner["captureRecoveryRestorePoint"] = async (...args: Parameters<typeof capture>) => {
          const result = await capture(...args);
          assert.equal(result.verified, true, "real capture supplies the joined local evidence");
          captures++;
          // Refuse the authenticated continuation AFTER local capture, BEFORE any POST.
          // This enters the actual joined disposition, rather than wrapping a refusal.
          throw new CodexBoundaryError("action", [], new Error("post-capture action unavailable"));
        };
        const recovery = rig.runner["handleRecoveryExhausted"].bind(rig.runner);
        let dispositionBatcher: MessageBatcher | undefined;
        let dispositions = 0;
        rig.runner["handleRecoveryExhausted"] = async (...args: Parameters<typeof recovery>) => {
          const options = args[8];
          if (options?.dispositionOnly) {
            dispositions++;
            assert.equal(joined, true);
            assert.equal(options.freeFallback, true);
            assert.equal(options.heldAuthenticated, undefined);
            assert.equal(options.heldCapture, undefined);
            dispositionBatcher = args[4];
          }
          return recovery(...args);
        };
        const close = MessageBatcher.prototype.close;
        t.mock.method(MessageBatcher.prototype, "close", async function (this: MessageBatcher, ...args: Parameters<typeof close>) {
          if (this === dispositionBatcher) closes++;
          return close.apply(this, args);
        });
        rig.runner["waitRecoveryRetry"] = async () => {
          waits++;
          throw new Error("definitive refusal must not retry");
        };
        api.failStateWhen(claim.run_id, body => {
          if (body.recovery_cause !== "codex_idle_stall") return false;
          posts++;
          assert.equal(joined, true, "no report before authenticated capture joins");
          assert.equal(dispositions, 1, "real HTTP refusal occurs in the disposition-only continuation");
          assert.equal(body.provider_progress_turns, 2);
          assert.equal(body.claim_generation, 3);
          return true;
        }, { httpStatus: 400, error: "invalid request body" });
        const report = client.reportState.bind(client);
        t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
          try { return await report(...args); }
          catch (error) {
            if (error instanceof StateReportUnappliedError) {
              assert.equal(error.runId, claim.run_id);
              publicErrors++;
            }
            throw error;
          }
        });
        const started = clock.now();
        const operation = rig.runner.execute(claim);
        try {
          await Promise.race([joining, operation.then(() => { throw new Error("execute returned before capture join"); })]);
          assert.equal(posts, 0);
          assert.ok(!raw.includes(IDLE_REFUSED));
          clock.advance(1000);
        } finally { release(); await operation; }
        assert.equal(captures, 1, "joined disposition consumes local evidence without recapture");
        assert.equal(dispositions, 1);
        assert.equal(publicErrors, 1, "actual client proves the report was unapplied");
        assert.equal(closes, 1, "existing retained stop closes the disposition batcher");
        assert.equal(waits, 0);
        assert.equal(posts, 1);
        assert.ok(clock.now() - started < 120_000);
        assert.equal(parks(claim.run_id).length, 0);
        assert.equal(raw.filter(text => text === IDLE_REFUSED).length, 1);
        assert.equal(feedTexts(claim.run_id).filter(text => text === IDLE_REFUSED).length, 1);
        assert.ok(!feedTexts(claim.run_id).includes(IDLE_UNCONFIRMED));
        assert.equal(rig.custody(), 0);
        assert.deepEqual(rig.archive.releaseCalls, []);
        assert.equal(fs.readFileSync(path.join(worktreeDirFor(36177), "WORK.txt"), "utf8"), "work before the outage\n");
        assert.equal(fs.readFileSync(path.join(rig.homePath(), "session"), "utf8"), "transcript");
        clock.advance(240_000);
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.equal(posts, 1);
        assert.equal(closes, 1);
        assert.equal(captures, 1);
        assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed"].includes(s.body.status ?? "")));
      });
    });

    for (const initial of ["client retry", "statusless", "wrong cause", "runner retry"] as const) {
      it(`3617 ${kind} ambiguity before refusal across ${initial}`, async t => {
        await withHome(async home => {
          client.protocolFeatures = ["claim_generation_fence", feature];
          const clock = virtualBoundaryClock();
          t.mock.method(Date, "now", clock.now);
          const rig = rigFor("codex", { clock,
            error: () => new TransientRecoveryError("idle", { idleStallKind: kind }) }, home);
          const claim = gitlabClaim(36173, { claim_generation: 3 });
          let posts = 0, waits = 0, publicErrors = 0;
          const refusedBodies: unknown[] = [];
          const refuse = () => api.failStateWhen(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return false;
            refusedBodies.push(structuredClone(body));
            return true;
          }, { httpStatus: 400, error: "invalid request body" });
          const stop = api.dropStatesWhen(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return false;
            posts++;
            if (initial === "client retry" && posts === 1) { refuse(); return true; }
            return initial === "runner retry" && waits === 0;
          });
          api.stateAckWire = (body, run) => body.recovery_cause === "codex_idle_stall"
            ? { status: 200, body: { run: { ...run, status: initial === "statusless" ? undefined : "recovery_wait",
                recovery_wait_cause: "provider_outage" } } } : undefined;
          // Keep ownership running after the uncertain ACK so the next runner call is exercised.
          api.onState(claim.run_id, body => {
            if (body.recovery_cause === "codex_idle_stall") api.setOwnershipStatus(claim.run_id, "running", 3);
          });
          const report = client.reportState.bind(client);
          t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
            try { return await report(...args); }
            catch (error) { if (error instanceof StateReportUnappliedError) publicErrors++; throw error; }
          });
          t.mock.method(rig.runner as unknown as { waitRecoveryRetry: () => Promise<void> }, "waitRecoveryRetry", async () => {
            waits++; clock.advance(5);
            assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED));
            if (waits === 1) { refuse(); return; }
            api.setOwnershipStatus(claim.run_id, "recovery_wait", 3);
          });
          try { await rig.runner.execute(claim); } finally { stop(); }
          assert.equal(waits, 2, "refusal after ambiguity still uses bounded typed retry");
          assert.equal(publicErrors, 1, "fresh runner call may be definitely unapplied by itself");
          assert.equal(refusedBodies.length, initial === "client retry" ? 2 : 1);
          for (const body of refusedBodies) {
            assert.equal((body as { recovery_cause: string }).recovery_cause, "codex_idle_stall");
            assert.equal((body as { claim_generation: number }).claim_generation, 3);
          }
          assert.equal(feedTexts(claim.run_id).filter(text => text === IDLE_UNCONFIRMED).length, 1);
          assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED));
          assert.equal(rig.custody(), 0);
          assert.deepEqual(rig.archive.releaseCalls, []);
          assert.ok(fs.existsSync(worktreeDirFor(36173)));
          assert.ok(fs.existsSync(path.join(rig.homePath(), "session")));
        });
      });
    }

    for (const control of ["cancel", "fence", "shutdown"] as const) {
      it(`3617 ${kind} ambiguous retry wait is interrupted by ${control}`, async t => {
        await withHome(async home => {
          client.protocolFeatures = ["claim_generation_fence", feature];
          const rig = rigFor("codex", {
            error: () => new TransientRecoveryError("idle", { idleStallKind: kind }),
          }, home);
          const claim = gitlabClaim(36176, { claim_generation: 3 });
          api.strictReceiptGenerations = true;
          api.setInputClaimGeneration(claim.run_id, 3);
          let posts = 0, waits = 0, interrupted = false, waitExpired = false;
          const stop = api.dropStatesWhen(claim.run_id, body => {
            if (body.recovery_cause !== "codex_idle_stall") return false;
            posts++;
            return true;
          });
          const wait = rig.runner["waitRecoveryRetry"].bind(rig.runner);
          t.mock.method(rig.runner as unknown as { waitRecoveryRetry: typeof wait }, "waitRecoveryRetry",
            async (...args: Parameters<typeof wait>) => {
              waits++;
              // Keep the real interruptible wait. A separate fail-fast signal prevents
              // a broken control path from spending its full ten seconds in this test.
              const watchdog = new AbortController();
              const timer = setTimeout(() => {
                waitExpired = true;
                watchdog.abort(new Error("control did not interrupt recovery wait"));
              }, 2000);
              const signal = args[3] ? AbortSignal.any([args[3], watchdog.signal]) : watchdog.signal;
              const pending = wait(args[0], args[1], 10_000, signal);
              if (control === "shutdown") rig.runner.shutdown();
              else {
                if (control === "fence") {
                  api.setInputClaimGeneration(claim.run_id, 4);
                  api.setInputFenceReason(claim.run_id, "released");
                }
                api.setInputs(claim.run_id, [{ id: 1, kind: control === "cancel" ? "cancel" : "follow_up" }]);
              }
              try { await pending; }
              finally {
                clearTimeout(timer);
                interrupted = control === "cancel" ? args[0].steering.isCancelled()
                  : control === "fence" ? args[0].steering.claimFence() !== undefined
                  : args[3]?.aborted === true;
              }
            });
          try {
            const operation = rig.runner.execute(claim);
            if (control === "shutdown") await assert.rejects(operation, { name: "AbortError" });
            else await operation;
          } finally { stop(); rig.runner.shutdown(); }
          assert.equal(waits, 1);
          assert.equal(waitExpired, false, "the control interrupts before the fail-fast deadline");
          assert.equal(interrupted, true, "the wait observes the delivered control");
          assert.equal(posts, 3, "no idle resend after the control interrupts the client-exhausted attempt");
          assert.ok(!feedTexts(claim.run_id).includes(IDLE_REFUSED));
          if (control === "cancel")
            assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.failure_reason === "run cancelled"));
          else assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed"].includes(s.body.status ?? "")));
        });
      });
    }

    for (const wire of ["timeout", "unreadable", "truncated", "oversized", "malformed", "unknown validation"] as const) {
      it(`3617 ${kind} ${wire} is ambiguous and stays frozen`, async () => {
        const claim = gitlabClaim(36174, { claim_generation: 3 });
        const requests: string[] = [];
        let truncatedPrefixRead = false;
        const transport: typeof fetch = async (url, init) => {
          requests.push(String(init?.body));
          api.failStateWhen(claim.run_id, () => true, { httpStatus: 400, error: "invalid request body" });
          const response = await fetch(url, init);
          assert.equal(response.status, 400);
          await response.text();
          if (wire === "timeout") throw new DOMException("request timed out", "TimeoutError");
          if (wire === "unreadable") return new Response(new ReadableStream({
            start(controller) { controller.error(new Error("unreadable response")); },
          }), { status: 400 });
          if (wire === "truncated") {
            let sent = false;
            return new Response(new ReadableStream({
              pull(controller) {
                if (!sent) { sent = true; controller.enqueue(new TextEncoder().encode('{"error":"invalid request body"}')); }
                else { truncatedPrefixRead = true; controller.error(new Error("stream failed before EOF")); }
              },
            }), { status: 400 });
          }
          return new Response(wire === "oversized" ? JSON.stringify({ error: "invalid request body" }) + " ".repeat(8192)
            : wire === "malformed" ? '{"error":"invalid request body"'
            : JSON.stringify({ error: "invalid progress" }), { status: 400 });
        };
        const real = new WorkerClient(baseUrl, TOKEN, "test", nullLogger(), {
          fetch: transport, sleep: async () => {}, terminalRetrySchedule: [1, 1],
        });
        real.protocolFeatures = ["claim_generation_fence", feature];
        const body = { status: "recovery_wait" as const, claim_generation: 3,
          recovery_cause: "codex_idle_stall" as const, provider_progress_turns: 2 };
        await assert.rejects(real.reportState(claim.run_id, body), error => !(error instanceof StateReportUnappliedError));
        assert.ok(requests.length > 0 && requests.length <= 3);
        if (wire === "truncated") assert.equal(truncatedPrefixRead, true, "complete-looking JSON was read before EOF failed");
        for (const request of requests) assert.deepEqual(JSON.parse(request), body);
        assert.equal(parks(claim.run_id).length, 0);
        assert.equal(real.hasFeature(feature), true);
      });
    }
  }
});

describe("3506 dual marker provider wire precedence", () => {
  for (const scenario of [
    { name: "timed", features: [...FEATURES, PROGRESS], generation: 3, held: false, turns: 2, typed: true },
    { name: "held", features: [...FEATURES, HOLD, PROGRESS], generation: 3, held: true, turns: 0, typed: true },
    { name: "legacy", features: ["claim_generation_fence", PROGRESS], generation: 3, held: false, turns: 2, typed: false },
    { name: "unfenced", features: ["recovery_cause_provider_outage", PROGRESS], generation: 3, held: false, turns: 0, typed: false },
    { name: "zero generation", features: [...FEATURES, PROGRESS], generation: 0, held: false, turns: 2, typed: false },
  ]) it(`3506 blackbox provider wire report: ${scenario.name}`, async () => {
    await withHome(async home => {
      client.protocolFeatures = [...scenario.features, "recovery_cause_codex_idle_stall_v1"];
      const rig = rigFor("codex", { turns: scenario.turns, error: () => new TransientRecoveryError(
        "silence after a provider-reported rate limit or transport retry",
        { providerOrigin: true, idleStallKind: "zero_evidence", originalIdle: new Error("codex run idle timeout") },
      ) }, home);
      const claim = gitlabClaim(3506, { claim_generation: scenario.generation });
      if (scenario.held) api.ackTypedParkAs(claim.run_id, "paused", HOLD);
      await rig.runner.execute(claim);
      const sent = parks(claim.run_id);
      assert.equal(sent.length, 1);
      assert.equal(sent[0]!.recovery_cause, scenario.typed ? "provider_outage" : undefined);
      assert.equal(sent[0]!.productive_turns, scenario.typed ? scenario.turns : undefined);
      assert.equal(sent[0]!.provider_progress_turns, undefined, "provider-origin fallback keeps existing progress semantics");
      const notices = feedTexts(claim.run_id);
      assert.ok(notices.some(text => text.startsWith("paused following silence after a provider-reported rate limit or transport retry;")));
      assert.ok(notices.some(text => text.includes(scenario.held ? "waits for its owner to resume it" : "resumes automatically")));
      if (scenario.held) assert.ok(!notices.some(text => text.includes("resumes automatically")));
    });
  });
});
