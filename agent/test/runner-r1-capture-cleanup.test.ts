import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { recordingLogger } from "./helpers.js";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { api, client, deferred, fakeGitlab, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

for (const origin of ["owner", "lead_park"] as const) {
  it(`R1 ${origin} pause gate waiting consumes the original absolute deadline`, async t => {
    const entered = deferred(), release = deferred();
    let start = 0, receivedDeadline: number | undefined, parked: boolean | undefined;
    let runner: ReturnType<typeof runnerWith>;
    runner = runnerWith(() => ({ executor: {
      run: async ctx => {
        const owner = runner as any;
        start = Date.now();
        let now = start;
        const clock = t.mock.method(Date, "now", () => now);
        t.mock.method(owner, "runGatedSink", async (_flight: unknown, action: () => Promise<unknown>) => {
          entered.resolve();
          await release.promise;
          now = start + owner.codexBoundaryDeadlineMs + 1;
          return action();
        });
        t.mock.method(owner, "runFreeCapture", async (_claim: unknown, _flight: unknown, deadline: number) => {
          receivedDeadline = deadline;
          return { kind: "incomplete" };
        });
        try {
          parked = origin === "owner"
            ? await ctx.parkForPause!({ completedCount: 0 })
            : (await ctx.requestParkRun!({ completedCount: 0 })).kind === "paused";
        } finally { clock.mock.restore(); }
        throw new Error("fixture model stopped after pause refusal");
      },
    } }), fakeGitlab().gitlab);
    // Lead refusal reconciles to an owner cancel without asking a new question.
    t.mock.method(client, "requestWorkerPause", async () => ({ applied: true, status: "running" as const }));
    let reconcile = false;
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      const ack = await report(...args);
      if (args[1].status === "pause_failed") reconcile = true;
      return ack;
    });
    const ownership = client.getRunOwnership.bind(client);
    t.mock.method(client, "getRunOwnership", async (...args: Parameters<typeof ownership>) => {
      const current = await ownership(...args);
      return reconcile ? { ...current, status: "cancelled" as const } : current;
    });
    const claim = gitlabClaim(2852, { claim_generation: 1, park_run_supported: true });
    const operation = runner.execute(claim);
    try {
      await Promise.race([entered.promise, operation.then(() => { throw new Error("pause gate never reached"); })]);
    } finally { release.resolve(); }
    await operation;
    assert.equal(parked, false);
    assert.equal(receivedDeadline, start + (runner as any).codexBoundaryDeadlineMs,
      "sink gate admission cannot renew a pause capture deadline");
    assert.equal(api.messages(claim.run_id).some(message => message.kind === "paused"), false);
  });
}

for (const completion of ["incomplete", "throws"] as const) {
  it(`R1 predecessor ${completion} outer capture cleanup cannot qualify a normal blocked source`, async t => {
    const claim = gitlabClaim(2852, { claim_generation: 1, inventory_guarded: true });
    const feature = client.hasFeature.bind(client);
    t.mock.method(client, "hasFeature", (name: Parameters<typeof feature>[0]) =>
      name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
    let generation = 1;
    t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{
      hold_id: "fixture-hold", generation, inventory_guarded: true, has_available_capture: false,
    }] }));
    t.mock.method(client, "reserveRecoveryCapture", async () => ({
      capture_id: "00000000-0000-4000-8000-000000002852", state: "preparing" as const,
    }));
    t.mock.method(client, "uploadRecoveryBundle", async (...[_run, _capture, _manifest, stream]: Parameters<typeof client.uploadRecoveryBundle>) => {
      for await (const chunk of stream) void chunk;
      throw new Error("fixture archive transport unavailable");
    });
    t.mock.method(client, "getRecoveryCaptureStatus", async () => { throw new Error("fixture archive transport unavailable"); });
    t.mock.method(client, "publishCheckpoint", async (...[_run, _tip, stream]: Parameters<typeof client.publishCheckpoint>) => {
      for await (const chunk of stream) void chunk;
      return { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
    });
    const cleaning = deferred(), finishCleanup = deferred();
    let models = 0, parked = false, captureEntries = 0, captureFlight: any;
    const log = recordingLogger();
    const runner = runnerWith(() => ({
      homeDir: path.join(homeDir, claim.run_id),
      executor: {
        run: async ctx => {
          models++;
          await fs.writeFile(path.join(ctx.worktreePath, "retained.txt"), "R1 predecessor local work\n");
          const command = (...args: string[]) => execFileSync("git", ["-C", ctx.worktreePath, ...args], { stdio: "pipe" });
          command("add", "retained.txt");
          command("-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "local predecessor work");
          await fs.writeFile(path.join(ctx.worktreePath, "dirty.txt"), "uncommitted predecessor work\n");
          const pausedAt = { completedCount: 0 };
          parked = await ctx.parkForPause!(pausedAt);
          return { branch: ctx.branch, pausedAt };
        },
        reapAttributedProcesses: async () => {
          if (parked) { cleaning.resolve(); await finishCleanup.promise; }
          return { complete: true, killed: [], left: [] };
        },
      },
    }), fakeGitlab().gitlab, "fixture-journal-key", log.logger, {
      dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      }),
    });
    const owner = runner as any;
    const capture = owner.runFreeCapture.bind(runner);
    t.mock.method(owner, "runFreeCapture", async (...args: any[]) => {
      const result = await capture(...args);
      const flight = args[1];
      if (flight.claimGeneration === 2 && flight.predecessorCapture) {
        captureEntries++;
        captureFlight = flight;
        assert.equal(result.kind, "observed_empty", "inner capture and quiescence completed");
        if (completion === "throws") throw new RecoveryClosureLimitError(4096);
        return { kind: "incomplete" };
      }
      return result;
    });
    const first = runner.execute(claim);
    const firstTail = owner.executionTails.get(claim.run_id);
    let second = Promise.resolve();
    try {
      await Promise.race([cleaning.promise, first.then(() => { throw new Error("first park never reached cleanup"); })]);
      generation = 2;
      api.setOwnershipStatus(claim.run_id, "running", 2);
      second = runner.execute({ ...claim, claim_generation: 2, session_id: "predecessor-session" });
      const secondTail = owner.executionTails.get(claim.run_id);
      finishCleanup.resolve();
      await first;
      assert.equal(parked, true);
      const original = await firstTail;
      assert.ok(original?.family, "real predecessor has a positively observed source family: " + JSON.stringify(log.lines.slice(-6)));
      await second;
      assert.equal(captureEntries, 1, "failed outer cleanup ends this recovery iteration");
      assert.equal(captureFlight.sourceFamilyMatched, true, "family match alone is insufficient");
      assert.equal(captureFlight.sourceBlockingNormal, false);
      assert.equal(captureFlight.sourceNormalBlocked, false);
      assert.equal(await secondTail, undefined, "outer cleanup failure cannot acquire terminal source authority");
      assert.equal(models, 1, "predecessor refusal never admits a successor model");
      assert.ok(api.states.some(state => state.body.status === "failed"), "failure report alone grants no source authority");
    } finally {
      finishCleanup.resolve();
      await Promise.all([first, second]);
      runner.shutdown();
      await runner.shutdownRecoveries();
    }
  });
}
