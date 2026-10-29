import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient } from "../src/client.js";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";

describe("compact hold negotiated capability", () => {
  for (const server of [false, true]) for (const worker of [false, true]) {
    it(`requires both server=${server} and registered worker=${worker}`, async () => {
      const api = new FakeApi("policy-negotiation");
      const url = await api.listen();
      const client = new WorkerClient(url, "policy-negotiation", "test", nullLogger());
      try {
        api.setRegisterProtocolFeatures(server ? ["provider_policy_hold_v1"] : []);
        await client.register("policy-test", "base", 1, [],
          worker ? ["provider_policy_hold_v1"] : [], undefined, 0);
        assert.equal(client.supportsPolicyHold, server && worker);
        // A heartbeat feature alone cannot invent an image's registered capability.
        client.protocolFeatures = ["provider_policy_hold_v1"];
        assert.equal(client.supportsPolicyHold, worker);
        await client.register("policy-test", "base", 1, [], [], undefined, 0);
        assert.equal(client.supportsPolicyHold, false, "re-registration replaces capabilities");
      } finally {
        await api.close();
      }
    });
  }
});
