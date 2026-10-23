import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
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
}

function rigFor(harness: Harness, scenario: Scenario, homeRoot: string): Rig {
  const { gitlab } = fakeGitlab();
  const { coord } = enabledRecovery();
  const error = scenario.error ?? (() => new TransientRecoveryError("provider transient error persisted", { providerOrigin: true }));
  let runHome = "";
  let thrown = false;
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
      let drainedAfterThrow = false;
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
          // Let the real strict drain finish before verified local capture. Inject the
          // late publication/pre-report reap fault only after that successful return.
          if (postThrowShutdown && drainedAfterThrow) throw new CodexBoundaryError("quiesce", [{ category: "protocol", message: "provider not reaped" }]);
          const result = await real(req, action);
          if (postThrowShutdown) drainedAfterThrow = true;
          return result;
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
  const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord, recoveryRetryMs: 5 });
  if (harness === "claude" && scenario.failReap) {
    const r = runner as unknown as { reapForSink: (...a: unknown[]) => Promise<void> };
    const real = r.reapForSink.bind(runner);
    r.reapForSink = async (...a: unknown[]) => {
      if (thrown && (a[1] as { boundary: string }).boundary === "shutdown") throw new Error("provider not reaped");
      return real(...a);
    };
  }
  return { runner, custody: spyCustodySettle(runner), homePath: () => runHome };
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
