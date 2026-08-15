import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { failOriginForReason } from "../src/runner.js";
import { REASON_PROVISION_FAILED } from "../src/provision-run.js";
import { REASON_NO_TOKEN } from "../src/sdk-executor.js";

// PRD #69 M7a: the worker authors the fail_origin from the reason CONSTANT the throw
// site used, never by parsing free text. This pins the two fatal pre-start mappings
// and the "everything else is an ordinary agent failure" default.
describe("failOriginForReason", () => {
  it("maps a provisioning failure (with appended detail) to provisioning_failed", () => {
    assert.strictEqual(
      failOriginForReason(REASON_PROVISION_FAILED),
      "provisioning_failed",
    );
    // provision-run appends `: <detail>` after the constant — the prefix match holds.
    assert.strictEqual(
      failOriginForReason(`${REASON_PROVISION_FAILED}: invalid run id`),
      "provisioning_failed",
    );
  });

  it("maps a missing Anthropic token to credential_unavailable", () => {
    assert.strictEqual(
      failOriginForReason(REASON_NO_TOKEN),
      "credential_unavailable",
    );
  });

  it("returns undefined for an ordinary agent failure (server defaults to agent_failure)", () => {
    assert.strictEqual(failOriginForReason("clone failed: exit 128"), undefined);
    assert.strictEqual(failOriginForReason(""), undefined);
    assert.strictEqual(
      failOriginForReason("the agent ended the planning turn without submitting a plan"),
      undefined,
    );
  });
});
