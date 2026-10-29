import { it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";

const reservation = { incarnation: "22222222-2222-4222-8222-222222222222", version: 7, observation_version: 3 };
const claim = () => makeClaim({ run_id: "11111111-1111-4111-8111-111111111111",
  claim_generation: 9, forge_origin_reservation: reservation });

it("actual cold clone and warm fetch report only after success; absent callback and failures emit nothing", async () => {
  const fx = makeFixture();
  try {
    const git = new GitCache(fx.dataDir, nullLogger(), { schedule: [] });
    let calls = 0;
    const report = () => { calls++; };
    await git.ensureClone(fx.originPath, undefined, undefined, undefined, report);
    assert.equal(calls, 1);
    await git.ensureClone(fx.originPath, undefined, undefined, undefined, report);
    assert.equal(calls, 2);
    await git.ensureClone(fx.originPath);
    assert.equal(calls, 2);
    await assert.rejects(git.ensureClone(fx.originPath + "/missing", undefined, undefined, undefined, report));
    assert.equal(calls, 2);
    await git.ensureClone(fx.originPath, undefined, undefined, undefined, () => { throw new Error("telemetry unavailable"); });
  } finally { fx.cleanup(); }
});

for (const mode of ["flight", "drop", "clear", "refresh", "timeout"] as const) {
  it(`telemetry cancels on ${mode}, never retries or gates the caller`, async t => {
    if (mode === "timeout") t.mock.timers.enable({ apis: ["setTimeout"] });
    let sends = 0;
    let observed: AbortSignal | undefined;
    let body: unknown;
    const client = new WorkerClient("https://api.example", "fixture-worker-token", "test", nullLogger(), {
      fetch: async (_url, init) => {
        sends++;
        observed = init?.signal as AbortSignal;
        body = JSON.parse(String(init?.body));
        return new Promise<Response>((_resolve, reject) => {
          observed!.addEventListener("abort", () => reject(new Error("cancelled")), { once: true });
        });
      },
    });
    client.protocolFeatures = ["forge_clone_success_v1"];
    const flight = new AbortController();
    assert.equal(client.reportForgeCloneSuccess(claim(), flight.signal), undefined);
    assert.equal(sends, 1);
    assert.deepEqual(body, { claim_generation: 9, ...reservation });
    assert.equal(observed?.aborted, false);
    switch (mode) {
      case "flight": flight.abort(); break;
      case "drop": client.dropFeature("forge_clone_success_v1"); break;
      case "clear": client.clearFeatures(); break;
      case "refresh": client.protocolFeatures = []; break;
      case "timeout": t.mock.timers.tick(4_999);
        assert.equal(observed?.aborted, false, "full five-second budget");
        t.mock.timers.tick(1); break;
    }
    assert.equal(observed?.aborted, true);
    await Promise.resolve();
    assert.equal(sends, 1);
  });
}

for (const mode of ["absent", "token_absent", "malformed", "cancelled", "network", "500", "400", "404"] as const) {
  it(`optional telemetry handles ${mode} without retry or execution failure`, async () => {
    let sends = 0;
    const client = new WorkerClient("https://api.example", "fixture-worker-token", "test", nullLogger(), {
      fetch: async () => {
        sends++;
        if (mode === "network") throw new Error("offline");
        return new Response(null, { status: mode === "500" ? 500 : mode === "400" ? 400 : mode === "404" ? 404 : 200 });
      },
    });
    client.protocolFeatures = mode === "absent" ? [] : ["forge_clone_success_v1"];
    const c = claim();
    if (mode === "token_absent") delete c.forge_origin_reservation;
    if (mode === "malformed") c.forge_origin_reservation = { ...reservation, observation_version: 0 };
    const flight = new AbortController();
    if (mode === "cancelled") flight.abort();
    client.reportForgeCloneSuccess(c, flight.signal);
    await Promise.resolve();
    assert.equal(sends, ["absent", "token_absent", "malformed", "cancelled"].includes(mode) ? 0 : 1);
    if (mode === "400" || mode === "404") assert.equal(client.hasFeature("forge_clone_success_v1"), false);
  });
}
