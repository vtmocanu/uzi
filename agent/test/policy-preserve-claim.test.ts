import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { api, client, gitlabClaim, installHarness } from "./runner-harness.js";
import {
  decodePolicyRecoveryVerifiedReply, validatePolicyRecoveryVerifiedRequest, type ClaimResponse,
} from "../src/protocol.js";

const HEAD40 = "0123456789abcdef0123456789abcdef01234567";
const HEAD64 = "0123456789abcdef".repeat(4);

describe("preserve-only claim fields on the production claim path", () => {
  installHarness();
  async function claimWith(extra: Record<string, unknown>): Promise<ClaimResponse> {
    api.enqueueClaim(gitlabClaim(4101, { claim_generation: 3, ...extra }));
    const claim = await client.claimRun();
    assert.ok(claim, "the fake API delivered the claim");
    return claim;
  }

  it("keeps a boolean flag and a lowercase 40- or 64-hex head", async () => {
    for (const head of [HEAD40, HEAD64]) {
      const claim = await claimWith({ policy_preserve_only: true, policy_expected_head: head });
      assert.equal(claim.policy_preserve_only, true);
      assert.equal(claim.policy_expected_head, head);
    }
  });

  it("an ordinary claim carries neither field", async () => {
    const claim = await claimWith({});
    assert.equal(claim.policy_preserve_only, undefined);
    assert.equal(claim.policy_expected_head, undefined);
  });

  it("a preserve-only claim without a head stays distinguishable from one with a malformed head", async () => {
    const degraded = await claimWith({ policy_preserve_only: true });
    assert.equal(degraded.policy_preserve_only, true);
    assert.equal(degraded.policy_expected_head, undefined, "absent means a degraded capture");
    for (const bad of ["", "ABCDEF" + "0".repeat(34), "a".repeat(39), "a".repeat(41), "g".repeat(40), 7, null, {}]) {
      const claim = await claimWith({ policy_preserve_only: true, policy_expected_head: bad });
      assert.equal(claim.policy_preserve_only, true);
      assert.equal(claim.policy_expected_head, "", `malformed head ${JSON.stringify(bad)} reads as present but invalid`);
    }
  });

  it("policy_saved_work_head: valid 40/64-hex kept, absent stays absent, present-but-invalid reads as empty", async () => {
    for (const head of [HEAD40, HEAD64]) {
      assert.equal((await claimWith({ policy_saved_work_head: head })).policy_saved_work_head, head);
    }
    assert.equal((await claimWith({})).policy_saved_work_head, undefined);
    for (const bad of ["", "ABCDEF" + "0".repeat(34), "a".repeat(39), "g".repeat(40), 7, null, {}]) {
      const claim = await claimWith({ policy_saved_work_head: bad });
      assert.equal(claim.policy_preserve_only, undefined);
      assert.equal(claim.policy_saved_work_head, "", JSON.stringify(bad));
    }
  });

  it("a non-boolean flag is dropped as absent", async () => {
    for (const bad of ["true", 1, null, {}]) {
      const claim = await claimWith({ policy_preserve_only: bad });
      assert.equal(claim.policy_preserve_only, undefined, JSON.stringify(bad));
    }
  });

  it("the shared fixture parses to the same claim fields", async () => {
    const raw = JSON.parse(await fs.readFile(new URL("../../fixtures/claims/policy-preserve-claim.json", import.meta.url), "utf8")) as
      Record<string, unknown>;
    const { _comment: _ignored, ...fields } = raw;
    assert.equal(fields.policy_preserve_only, true);
    const claim = await claimWith(fields);
    assert.equal(claim.policy_preserve_only, true);
    assert.equal(claim.policy_expected_head, fields.policy_expected_head);
    assert.match(String(claim.policy_expected_head), /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/);
  });
});

describe("policy-recovery-verified wire shape", () => {
  it("requires a positive generation and a lowercase 40/64-hex head and nothing else", () => {
    assert.deepEqual(validatePolicyRecoveryVerifiedRequest({ claim_generation: 3, required_head: HEAD40 }),
      { claim_generation: 3, required_head: HEAD40 });
    for (const bad of [
      { claim_generation: 0, required_head: HEAD40 }, { claim_generation: 1.5, required_head: HEAD40 },
      { claim_generation: 3, required_head: "" }, { claim_generation: 3, required_head: HEAD40.toUpperCase() },
      { claim_generation: 3 }, { claim_generation: 3, required_head: HEAD40, extra: 1 }, null, [],
    ]) assert.throws(() => validatePolicyRecoveryVerifiedRequest(bad), JSON.stringify(bad));
  });

  it("decodes only the closed dispositions with a bounded reason", () => {
    for (const disposition of ["recorded", "pending", "refused"]) {
      assert.deepEqual(decodePolicyRecoveryVerifiedReply({ disposition }), { disposition });
    }
    assert.deepEqual(decodePolicyRecoveryVerifiedReply({ disposition: "refused", reason: "stale" }),
      { disposition: "refused", reason: "stale" });
    for (const bad of [{ disposition: "applied" }, {}, { disposition: "recorded", reason: "x".repeat(257) },
      { disposition: "recorded", extra: true }, null]) assert.throws(() => decodePolicyRecoveryVerifiedReply(bad));
  });
});
