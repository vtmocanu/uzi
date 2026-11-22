import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { isRunEnvLeadLane, leadLaneRunEnv } from "../src/run-env-lane.js";
import { RUN_KINDS, type ClaimRunEnvVar } from "../src/protocol.js";

// PRD 4416 M2: the worker mirror of leadcfg.LeadLane decides whether a run's variables reach its executor.

const VARS: ClaimRunEnvVar[] = [
  { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "FOO", value: "dummy-value", secret: false, description: "" },
];
const readFixture = <T>(rel: string): T => JSON.parse(fs.readFileSync(new URL(`../../fixtures/${rel}`, import.meta.url), "utf8")) as T;
const REGISTRY_KINDS = readFixture<{ kinds: string[] }>("run-kinds/registry.json").kinds;
const LEAD_KINDS = readFixture<{ vocabulary: { lead_lane_kinds: string[] } }>("lead-config/contract.json").vocabulary.lead_lane_kinds;

describe("the lead lane agrees with the shared fixtures", () => {
  it("every registry kind is a lead kind exactly when the contract lists it", () => {
    for (const kind of REGISTRY_KINDS) assert.equal(isRunEnvLeadLane({ kind }), LEAD_KINDS.includes(kind), kind);
  });

  it("every lead kind of the contract is a registry kind", () => {
    for (const kind of LEAD_KINDS) assert.ok(REGISTRY_KINDS.includes(kind), kind);
  });
});

describe("leadLaneRunEnv", () => {
  for (const kind of RUN_KINDS) {
    const lead = LEAD_KINDS.includes(kind);
    it(`${kind}: ${lead ? "receives" : "never receives"} the variables`, () => {
      assert.equal(isRunEnvLeadLane({ kind }), lead);
      assert.deepEqual(leadLaneRunEnv({ kind }, VARS), lead ? VARS : undefined);
    });
  }

  it("a review target withholds the variables from a task run", () => {
    assert.equal(leadLaneRunEnv({ kind: "task", review_target_run_id: "r-1" }, VARS), undefined);
  });

  it("an egress-bound (isolated_fetch) run withholds the variables", () => {
    assert.equal(leadLaneRunEnv({ kind: "issue", isolated_fetch: { profile: "p" } }, VARS), undefined);
  });

  it("null review target and null grant do not withhold", () => {
    assert.deepEqual(leadLaneRunEnv({ kind: "issue", review_target_run_id: null, isolated_fetch: null }, VARS), VARS);
  });

  it("an absent kind is an issue run", () => {
    assert.deepEqual(leadLaneRunEnv({}, VARS), VARS);
  });

  it("no variables yields undefined", () => {
    assert.equal(leadLaneRunEnv({ kind: "issue" }, undefined), undefined);
    assert.equal(leadLaneRunEnv({ kind: "issue" }, []), undefined);
  });
});
