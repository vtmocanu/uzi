import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { PublishResponse } from "../src/protocol.js";

// The TS half of the PRD #122 M8 publish wire contract. The api side pins the Go struct
// separately (no shared golden file), so this only asserts the TS type compiles for each
// documented shape and round-trips through JSON.parse typed as PublishResponse.
describe("PublishResponse wire contract (PRD #122 M8)", () => {
  it("accepts the published shape and round-trips it", () => {
    const published: PublishResponse = {
      published: true,
      ref: "refs/uzi-checkpoints/agent/issue-1",
    };
    const parsed = JSON.parse(JSON.stringify(published)) as PublishResponse;
    assert.strictEqual(parsed.published, true);
    assert.strictEqual(parsed.ref, "refs/uzi-checkpoints/agent/issue-1");
    assert.strictEqual(parsed.skipped, undefined);
  });

  it("accepts each best-effort skip reason and round-trips it", () => {
    const reasons: Array<PublishResponse["skipped"]> = [
      "no_ref",
      "not_descendant",
      "unsupported",
    ];
    for (const skipped of reasons) {
      const skip: PublishResponse = {
        published: false,
        ref: "refs/uzi-checkpoints/agent/issue-1",
        skipped,
      };
      const parsed = JSON.parse(JSON.stringify(skip)) as PublishResponse;
      assert.strictEqual(parsed.published, false);
      assert.strictEqual(parsed.skipped, skipped);
    }
  });
});
