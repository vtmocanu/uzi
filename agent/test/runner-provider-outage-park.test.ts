import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { acceptsHeldCaptureCleanup, CodexBoundaryError } from "../src/codex/safety.js";
import type { BoundaryPermit } from "../src/harness.js";
import { quiesceRunAttempt } from "../src/run-quiescence.js";
import { SdkExecutor, TransientRecoveryError } from "../src/sdk-executor.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import {
  FakeCodexExecutor,
  codexRig,
  commitInTree,
  enabledRecovery,
  feedTexts,
  installRecoveryRootCleanup,
  spyCustodySettle,
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
      const rig = codexRig();
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
          if (held) await executing;
          else await assert.rejects(executing, { name: "AbortError" });
        } finally { rig.runner.shutdown(); t.mock.restoreAll(); }
        assert.equal(reports, refusal ? 2 : 1);
        assert.equal(waits, held ? 0 : refusal ? 2 : 1);
        if (refusal) {
          assert.equal(refusedBodies.length, 2, "both retries reached the actual HTTP refusal hook");
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
