import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
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
const HOLD = "provider_unavailable";

type Harness = "claude" | "codex";
const HARNESSES: Harness[] = ["claude", "codex"];

interface Scenario {
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
    commitInTree(ctx.worktreePath, "WORK.txt", "work before the outage\n");
    for (let i = 0; i < (scenario.turns ?? 0); i++) await ctx.onModelTurnSettled?.(1);
    thrown = true;
    throw error();
  };
  let factory: ExecutorFactory;
  if (harness === "codex") {
    const rig = codexRig();
    if (scenario.failReap) {
      const real = rig.safety.withBoundary.bind(rig.safety);
      rig.safety.withBoundary = async (req, action) => {
        if (thrown && req.boundary === "shutdown") throw new CodexBoundaryError("quiesce", [{ category: "protocol", message: "provider not reaped" }]);
        return real(req, action);
      };
    }
    factory = (runId) => {
      runHome = path.join(homeRoot, runId);
      return { homeDir: runHome, executor: new FakeCodexExecutor(rig.safety, behavior, rig.settle) };
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

    it(`${harness}: a lost ack followed by a paused/provider_unavailable ownership read is the accepted park`, async () => {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = FEATURES;
        const rig = rigFor(harness, { turns: 1 }, homeRoot);
        const claim = gitlabClaim(3306, { claim_generation: 3 });
        const stop = api.dropStatesWhen(claim.run_id, (b) => b.recovery_cause === "provider_outage");
        api.setOwnershipStatus(claim.run_id, "paused", 3, undefined, HOLD);
        try {
          await rig.runner.execute(claim);
        } finally {
          stop();
        }
        assert.equal(parks(claim.run_id).length, 0, "the typed report was dropped before it was recorded");
        assert.equal(fs.existsSync(worktreeDirFor(3306)), true, "a held run keeps its clone");
        assert.equal(fs.existsSync(rig.homePath()), true);
        assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false);
      });
    });
  }

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
      const claim = gitlabClaim(3308, { claim_generation: 3 });
      const stop = api.dropStatesWhen(claim.run_id, (b) => b.recovery_cause === "provider_outage");
      api.setOwnershipStatus(claim.run_id, "paused", 3);
      try {
        await rig.runner.execute(claim);
      } finally {
        stop();
      }
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
