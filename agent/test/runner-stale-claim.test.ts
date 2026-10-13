// PRD #1247 M5b — the transport-behavior half of the held-state credential switch (worker side),
// as send-gated by the fix round (E).
//
// This unit makes the worker (1) STAMP its claim-lane generation on every mutating /state report
// THROUGH the claim_generation send-gate (fix round E): the field rides the wire only for a
// generation>0 capability/feature worker, and on the exact strict-decode 400 from a rolled-back api
// it is stripped and the report retried ONCE so the run never wedges; and (2) STOP a flight the
// moment a report is answered with the stale_claim disposition (a held-state switch RELEASED the
// claim, or a reclaim SUPERSEDED it) — without a terminal report and without a preserve flag.
//
// The generation stamping is driven end-to-end through the runner (buildFlight wires the batcher
// from claim.claim_generation and the reportState closure stamps it, and client.reportState applies
// the send-gate), so these are RUNNER tests. The gate's omit-vs-stamp + strip-and-retry byte shape
// is pinned at the client level in client-state-send-gate.test.ts (state) and
// client-send-gate.test.ts (messages).

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { SdkExecutor } from "../src/sdk-executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { CredentialSwitchSignal, type SteeringChannel } from "../src/steering.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  planThenDoneQuery,
  runner,
  runnerWith,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

describe("RunRunner — PRD #1247 M5b claim_generation stamping (send-gated, fix round E)", () => {
  it("a CAPABILITY worker stamps the claim's generation on every /state report, and completes normally", async () => {
    const { gitlab } = fakeGitlab();
    // The fake SDK query commits nothing; model committed work so the zero-diff guard does not
    // fail this happy path (issue #279).
    simulateCommittedWork();
    // PRD #1247 fix round E: /state now rides the claim_generation send-gate, so the field is
    // stamped only for a generation>0 capability/feature worker (a production worker always
    // advertises credential_switch_v1). Register the harness client as a capability worker so the
    // reportState closure's stamp actually reaches the wire.
    await client.register("w-1247", undefined, 1, undefined, ["credential_switch_v1"]);
    const GEN = 7;
    const claim = gitlabClaim(1247, { claim_generation: GEN });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.ok(states.length > 0, "the run made at least one /state report");
    // The single choke point stamps EVERY report, including the terminal one.
    for (const s of states) {
      assert.strictEqual(
        s.claim_generation,
        GEN,
        `every /state report carries claim_generation for a capability worker (${s.status})`,
      );
    }
    // The specific reports the spec calls out: a `running` report and the terminal report.
    const running = states.find((s) => s.status === "running");
    assert.ok(running, "a running report was made");
    assert.strictEqual(running!.claim_generation, GEN);
    // Back-compat: with no stale ack, the run proceeds unchanged to completion.
    assert.ok(
      states.some((s) => s.status === "completed"),
      "the flight completed normally (a matching generation is a no-op)",
    );
  });

  it("OMITS claim_generation on /state for a gen-0 claim, even for a capability worker (E: 0 is chat's legacy sentinel)", async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    await client.register("w-1248", undefined, 1, undefined, ["credential_switch_v1"]);
    const claim = gitlabClaim(1248); // no claim_generation on the claim ⇒ 0
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const running = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "running");
    assert.ok(running, "a running report was made");
    // E's send-gate never sends 0 (unlike the old M5b unconditional stamp, which sent 0). This is
    // the same fix that makes a <=v0.82 claim (generation ?? 0) omit the key instead of sending 0.
    assert.strictEqual(running!.claim_generation, undefined, "0 is never sent; the key is omitted");
  });

  it("OMITS claim_generation on /state for a NON-capability worker without the feature (E send-gate)", async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    await client.register("w-bare"); // no capability advertised, FakeApi advertises no feature
    const claim = gitlabClaim(1249, { claim_generation: 5 });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.ok(states.length > 0, "the run made at least one /state report");
    for (const s of states) {
      assert.strictEqual(
        s.claim_generation,
        undefined,
        `a bare worker omits claim_generation on /state (${s.status})`,
      );
    }
  });

  it("survives an rc.5-shaped strict decoder: the first running report strips+retries and the run still completes", async () => {
    // PRD #1247 fix round E, the runner integration: a capability worker's /state stamp meets a
    // rolled-back api that strict-decodes the unknown field with a 400. reportState strips the field
    // and retries ONCE, so the first `running` report and the terminal report both land — the run is
    // NOT wedged. Reverting E (unconditional stamp, no fallback) throws the 400 out of the first
    // report and the run fails instead.
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    await client.register("w-skew", undefined, 1, undefined, ["credential_switch_v1"]);
    api.failStateStrictDecodeNext(1); // ONLY the first /state POST strict-decodes
    const claim = gitlabClaim(1250, { claim_generation: 3 });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    // The first running report survived (via the stripped retry), and the run reached a terminal
    // report — proof the strict decoder did not wedge it.
    assert.ok(states.some((s) => s.status === "running"), "the running report landed after strip+retry");
    assert.ok(states.some((s) => s.status === "completed"), "the run reached its terminal report");
  });
});

describe("RunRunner — PRD #1247 M5b stop-on-stale_claim", () => {
  it("stops the flight on a stale_claim ack with NO terminal report and NO preserve flag", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1247-stale-"));
    try {
      const iid = 1249;
      // A factory whose executor must never run: the stale ack lands on the FIRST `running`
      // report (phaseClone), before the clone or the executor — so run() being called would
      // prove the flight did NOT stop.
      let executorRan = false;
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async () => {
            executorRan = true;
            throw new Error("executor must not run after a stale_claim stop");
          },
        },
      });
      const { logger, lines } = recordingLogger();
      const claim = gitlabClaim(iid, { claim_generation: 3 });
      // The server answers the first `running` report as a released/superseded claim: a 409
      // carrying the top-level stale_claim disposition (readRunAck → StateAck.staleClaim).
      api.failStateWhen(claim.run_id, (b) => b.status === "running", {
        httpStatus: 409,
        disposition: "stale_claim",
      });

      // Must resolve, not reject: a stale claim is a clean stop, not a run failure.
      await runnerWith(factory, gitlab, undefined, logger).execute(claim);

      assert.strictEqual(executorRan, false, "the executor never ran — the flight stopped");

      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      // No TERMINAL report — another claim owns the run now, so a `failed`/`completed` here
      // would fight the owning claim.
      assert.strictEqual(
        states.some((s) => s.status === "failed" || s.status === "completed"),
        false,
        "a stale_claim stop must NOT report a terminal state",
      );
      // No PARK/PRESERVE report either — this is normal teardown, not a limit/pause/recovery park.
      assert.strictEqual(
        states.some((s) =>
          ["limit_wait", "recovery_wait", "paused", "pause_failed"].includes(s.status),
        ),
        false,
        "a stale_claim stop must NOT park or preserve",
      );

      // The StaleClaimError arm ran (proves the stop path, not some other exit).
      assert.ok(
        lines.some(
          (l) =>
            (l as { msg?: string }).msg ===
            "run claim superseded server-side (stale_claim); stopping this flight",
        ),
        "the stale_claim stop was logged",
      );
      // The preserve-session flag was NOT set — the finally logs a preserve line only when it is,
      // and it must not appear here (a stale claim's new owner has its own clone).
      assert.strictEqual(
        lines.some(
          (l) =>
            typeof (l as { msg?: string }).msg === "string" &&
            (l as { msg: string }).msg.includes("preserving its plugin dir and HOME"),
        ),
        false,
        "a stale_claim stop must NOT preserve the session (no preserve log line)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  for (const stage of ["ack-empty", "ack-retained", "after-discovery"]) it(`MINOR-7: credential_switch triggers the handshake at ${stage}`, async () => {
    const retained = stage !== "ack-empty";
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1247-ack-switch-"));
    try {
      const claim = gitlabClaim(1280, { claim_generation: 5 });
      api.setOwnershipStatus(claim.run_id, "running", 5);
      await client.register("w-ack-switch", undefined, 1, undefined, ["credential_switch_v1"]);
      let clonePath: string | undefined;
      let journal: string | undefined;
      if (retained) {
        const bare = await git.ensureClone(fx.originPath);
        const clone = await git.createOrAttachRunnerClone(bare, 1280, noProofReseed, claim.run_id);
        await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
        clonePath = clone.path;
        fs.writeFileSync(path.join(clonePath, "retained.txt"), "only local work\n");
        journal = fs.readFileSync(path.join(bare, "config"), "utf8");
      }
      const sessionPath = path.join(homeRoot, claim.run_id, "session.jsonl");
      fs.mkdirSync(path.dirname(sessionPath), { recursive: true });
      fs.writeFileSync(sessionPath, "predecessor session\n");
      let models = 0, fetches = 0, releases = 0;
      git.ensureClone = async () => { fetches++; throw new Error("must not fetch after switch"); };
      client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
      // ACK cases exercise the secondary transport without /inputs. The late-switch
      // case instead trips the same public steering entry after real discovery returns.
      if (stage !== "after-discovery") api.armStateAckCredentialSwitch(claim.run_id, 5);
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: { run: async () => { models++; throw new Error("must not run after switch"); } },
      });
      const r = runnerWith(factory, gitlab, undefined, nullLogger());
      let discoveries = 0;
      if (stage === "after-discovery") {
        const discover = git.discoverRetainedRecovery.bind(git);
        git.discoverRetainedRecovery = async (...args) => {
          const result = await discover(...args);
          assert.ok(result, "actual discovery returned the retained source before switching");
          discoveries++;
          const active = (r as unknown as { activeRuns: Map<string, { steering: SteeringChannel }> }).activeRuns.get(claim.run_id);
          assert.ok(active, "steering is registered before discovery completes");
          // Inject after the initial discovery; the switch handler also reads the existing journal.
          if (discoveries === 1) active.steering.tripCredentialSwitch(5);
          assert.ok(active.steering.lifecycleSignal().reason instanceof CredentialSwitchSignal);
          return result;
        };
      }
      await r.execute(claim);
      assert.equal(discoveries, stage === "after-discovery" ? 2 : 0);

      const s = api.states.filter((x) => x.runId === claim.run_id).map((x) => x.body.status);
      assert.deepEqual(s, ["running", "credential_switch_failed"], "discovery alone cannot authorize a release");
      assert.equal(api.states.find(x => x.runId === claim.run_id && x.body.status === "credential_switch_failed")?.body.claim_generation, 5);
      assert.equal(models, 0);
      assert.equal(fetches, 0);
      assert.equal(releases, 0);
      assert.equal(fs.readFileSync(sessionPath, "utf8"), "predecessor session\n");
      if (clonePath) {
        assert.equal(fs.readFileSync(path.join(clonePath, "retained.txt"), "utf8"), "only local work\n");
        assert.equal(fs.readFileSync(path.join(git.barePathFor(fx.originPath), "config"), "utf8"), journal);
      }
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
