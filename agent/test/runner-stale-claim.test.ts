// PRD #1247 M5b — the transport-behavior half of the held-state credential switch (worker side).
//
// This unit makes the worker (1) STAMP its claim-lane generation on every mutating /state report,
// and (2) STOP a flight the moment a report is answered with the
// stale_claim disposition (a held-state switch RELEASED the claim, or a reclaim SUPERSEDED it) —
// without a terminal report and without a preserve flag. It advertises NO capability and does NOT
// implement the release itself, so it is transparent in normal operation: stamping a generation
// the server's fence already matches changes no outcome (the back-compat test below).
//
// The generation stamping is driven end-to-end through the runner (buildFlight wires the batcher
// from claim.claim_generation and the reportState closure stamps it), so these are RUNNER tests.
// Message-batch generation stamping is the shared #1391 wire, feature-gated on
// `claim_generation_fence`; its omit-vs-stamp byte shape is pinned in client-negotiation.test.ts.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { SdkExecutor } from "../src/sdk-executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  fakeGitlab,
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

describe("RunRunner — PRD #1247 M5b claim_generation stamping", () => {
  it("stamps the claim's generation on every /state report, and completes normally", async () => {
    const { gitlab } = fakeGitlab();
    // The fake SDK query commits nothing; model committed work so the zero-diff guard does not
    // fail this happy path (issue #279).
    simulateCommittedWork();
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
        `every /state report carries claim_generation (${s.status})`,
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
    // Message-batch generation stamping is the shared #1391 wire (feature-gated on
    // `claim_generation_fence`), pinned by byte shape in client-negotiation.test.ts; the batcher is
    // wired from claim.claim_generation at construction. This runner test owns the #1247-specific
    // half: the /state reportState closure stamps every report unconditionally.
  });

  it("stamps a generation of 0 when the claim omits it (server-side NOT NULL DEFAULT 0)", async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    const claim = gitlabClaim(1248); // no claim_generation on the claim
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
    assert.strictEqual(running!.claim_generation, 0, "the missing generation defaults to 0");
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

  it("MINOR-7: a credential_switch signal on a /state ACK triggers the switch (the secondary transport, not just /inputs)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1247-ack-switch-"));
    try {
      const claim = gitlabClaim(1280, { claim_generation: 5 });
      // Arm the switch signal ONLY on the /state ACK — /inputs (setInputs) is never called — so a
      // trip PROVES the state-ack transport works on its own. Before MINOR-7 the ack's credential_switch
      // was parsed but never forwarded, so ONLY a /inputs poll could trigger a switch; here the run
      // would just complete normally.
      api.armStateAckCredentialSwitch(claim.run_id, 5);
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: new SdkExecutor(nullLogger(), path.join(homeRoot, runId), { queryFn: planThenDoneQuery() }),
      });
      await runnerWith(factory, gitlab, undefined, nullLogger()).execute(claim);

      const s = api.states.filter((x) => x.runId === claim.run_id).map((x) => x.body.status);
      // The state-ack signal tripped the switch: the flight entered enterCredentialSwitch and
      // reported the release (credential_switch) or the give-up (credential_switch_failed) — NOT a
      // plain `completed`, which is what a run whose switch signal was ignored would report.
      assert.ok(
        s.includes("credential_switch") || s.includes("credential_switch_failed"),
        `the state-ack credential_switch tripped the switch; reports were: ${s.join(", ")}`,
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
