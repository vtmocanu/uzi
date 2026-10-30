import { it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { client, git, fx, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

for (const warm of [false, true]) {
  it(`ordinary phaseClone reports actual ${warm ? "warm fetch" : "cold clone"} without awaiting telemetry`, async () => {
    if (warm) await git.ensureClone(fx.originPath);
    let sends = 0;
    let modelCalls = 0;
    let reportSignal: AbortSignal | undefined;
    const telemetry = new WorkerClient("https://api.example", "fixture-token", "test", nullLogger(), {
      fetch: async (_url, init) => {
        sends++;
        reportSignal = init?.signal as AbortSignal;
        return new Promise<Response>((_resolve, reject) => {
          reportSignal!.addEventListener("abort", () => reject(new Error("cancelled")), { once: true });
        });
      },
    });
    telemetry.protocolFeatures = ["forge_clone_success_v1"];
    client.reportForgeCloneSuccess = (c, signal) => telemetry.reportForgeCloneSuccess(c, signal);
    const claim = gitlabClaim(42, {
      claim_generation: 9,
      forge_origin_reservation: { incarnation: "22222222-2222-4222-8222-222222222222", version: 7, observation_version: 3 },
    });
    const runner = runnerWith(() => ({ executor: {
      run: async () => {
        modelCalls++;
        assert.equal(sends, 1);
        assert.equal(reportSignal?.aborted, false);
        throw new Error("end fixture model");
      },
    } }), fakeGitlab().gitlab);
    await runner.execute(claim);
    assert.equal(modelCalls, 1, "model ran while telemetry was unresolved");
    assert.equal(sends, 1, "one report for the flight");
    assert.equal(reportSignal?.aborted, true, "flight exit cancels telemetry");
  });
}
