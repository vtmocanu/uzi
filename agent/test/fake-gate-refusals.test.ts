import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import type { StateRequest } from "../src/protocol.js";

const RUN = "gate-refusal-3395";
const PLAN = "# Current plan";
const reportBody = (overrides: Partial<StateRequest> = {}): StateRequest => ({
  status: "awaiting_approval", claim_generation: 1, presentation_id: "current",
  plan_md: PLAN, expected_gate_revision: 2, ...overrides,
});

async function fixture(t: TestContext, maxRefusals = 1) {
  const api = new FakeApi("gate-refusal-fixture");
  api.gateRevisions = true;
  api.planRevisionRetries = true;
  api.gateRefusalMax = maxRefusals;
  api.seedGate(RUN, { revision: 2, presentationId: "current", historical: ["historical"], payload: { plan_md: PLAN } });
  const url = await api.listen();
  t.after(() => api.close());
  const report = async (body: StateRequest) => {
    const res = await fetch(`${url}/api/worker/runs/${RUN}/state`, {
      method: "POST", headers: { Authorization: "Bearer gate-refusal-fixture", "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const reply = await res.json() as {
      reason?: string; run: { status: string }; gate_revision?: number; revision_input_id?: number;
      superseded_revision_input_ids?: number[];
    };
    return { status: res.status, ...reply };
  };
  assert.equal((await report(reportBody())).status, 200);
  return { api, report };
}

describe("FakeApi expected-revision gate refusals (#3395)", () => {
  for (const revision of [1, 2]) {
    it(`historical refusal with expected revision ${revision} counts once per generation and fails past the cap`, async t => {
      const { api, report } = await fixture(t);
      const body = reportBody({ presentation_id: "historical", expected_gate_revision: revision });
      for (let repeat = 0; repeat < 2; repeat++) {
        const reply = await report(body);
        assert.equal(api.gateRefusals.length, repeat + 1, "a stale expected revision cannot bypass refusal classification");
        assert.equal(reply.status, 409);
        assert.equal(reply.reason, "stale_revision_source");
        assert.equal(reply.run.status, "awaiting_approval", "repeating generation 1 does not exhaust the cap");
      }
      const capped = await report({ ...body, claim_generation: 2 });
      assert.equal(capped.status, 409);
      assert.equal(capped.reason, "stale_revision_source");
      assert.equal(capped.run.status, "failed", "the second counted generation passes cap 1");
      assert.deepEqual(api.gateOf(RUN), { revision: 2, presentationId: "current" });
      assert.equal(api.states.length, 1, "refusals publish no gate");
    });
  }

  for (const [name, change] of [
    ["changed payload", { plan_md: "# Changed" }],
    ["stale adoption", { presentation_id: "adoption", adopt_gate_revision: 1 }],
  ] as const) {
    it(`${name} with a matching source returns stale_revision_source`, async t => {
      const { api, report } = await fixture(t);
      const reply = await report(reportBody(change));
      assert.equal(reply.status, 409);
      assert.equal(reply.reason, "stale_revision_source");
      assert.equal(api.gateRefusals.length, 1);
      assert.equal(api.states.length, 1);
    });
  }

  it("an unrefused stale source cannot allocate, change the snapshot or reset refusal accounting", async t => {
    const { api, report } = await fixture(t);
    await report(reportBody({ presentation_id: "historical" }));
    const before = api.gateResumeFields(RUN);
    for (const presentation_id of ["current", "unseen"]) {
      const reply = await report(reportBody({ presentation_id, expected_gate_revision: 1 }));
      assert.equal(reply.status, 409);
      assert.equal(reply.reason, "stale_revision_source");
      assert.deepEqual(api.gateOf(RUN), { revision: 2, presentationId: "current" });
      assert.deepEqual(api.gateResumeFields(RUN), before);
      assert.equal(api.gateRefusals.length, 1, "an unrefused mismatch is uncharged");
    }
    const capped = await report(reportBody({ presentation_id: "historical", claim_generation: 2 }));
    assert.equal(capped.run.status, "failed", "source mismatches did not reset generation 1's charge");
    assert.equal(api.states.length, 1);
  });

  it("a stale-source adoption leaves the id-less gate untouched", async t => {
    const { api, report } = await fixture(t);
    api.seedGate(RUN, { revision: 2, presentationId: null, payload: { plan_md: PLAN } });
    const reply = await report(reportBody({ presentation_id: "adoption", adopt_gate_revision: 2, expected_gate_revision: 1 }));
    assert.equal(reply.status, 409);
    assert.equal(reply.reason, "stale_revision_source");
    assert.deepEqual(api.gateOf(RUN), { revision: 2, presentationId: null });
    assert.deepEqual(api.gateRefusals, []);
    const accepted = await report(reportBody({ presentation_id: "adoption" }));
    assert.equal(accepted.status, 200, "the refused adoption did not leak its id into presentation history");
    assert.equal(accepted.gate_revision, 3);
  });

  it("an unrefused mismatch preserves revision receipts and their lost-ACK replay", async t => {
    const { api, report } = await fixture(t);
    const publication = reportBody({ presentation_id: "revised", plan_md: "# Revised", revision_input_id: 10, superseded_revision_input_ids: [9] });
    const accepted = await report(publication);
    assert.equal(accepted.gate_revision, 3);
    const refused = await report({ ...publication, presentation_id: "unseen", expected_gate_revision: 1, revision_input_id: 11, superseded_revision_input_ids: [10] });
    assert.equal(refused.reason, "stale_revision_source");
    assert.equal(api.isApplied(RUN, 11), false);
    assert.equal(api.isInputSuperseded(RUN, 10), false);
    assert.equal(api.isApplied(RUN, 10), true);
    assert.equal(api.isInputSuperseded(RUN, 9), true);
    assert.deepEqual(await report(publication), accepted, "the original receipt remains replayable after the stale report");
  });

  it("cap 0 remains unlimited across expected-revision refusal generations", async t => {
    const { report } = await fixture(t, 0);
    for (let generation = 1; generation <= 4; generation++) {
      const reply = await report(reportBody({ presentation_id: "historical", claim_generation: generation }));
      assert.equal(reply.status, 409);
      assert.equal(reply.run.status, "awaiting_approval");
    }
  });

  it("a refusal without expected_gate_revision keeps its raw reason", async t => {
    const { report } = await fixture(t);
    const reply = await report(reportBody({ presentation_id: "historical", expected_gate_revision: undefined }));
    assert.equal(reply.status, 409);
    assert.equal(reply.reason, "gate_presentation_historical");
  });
});
