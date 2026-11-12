// Issue #4158: the fake api must refuse the `running` reports the real api's SetRunRunning refuses.
// Differential: fixtures/cross-check-running-guard/cases.json drives both the pure rule and the
// fake's /state route; api/internal/store/plan_cross_check_running_guard_livedb_test.go drives the
// same file against Postgres. Both halves must stay green on the same cases.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import {
  FakeApi,
  crossCheckRunningAdmitted,
  type CrossCheckApproval,
  type CrossCheckLane,
  type CrossCheckModel,
} from "./fake-api.js";
import { makeClaim } from "./helpers.js";

const TOKEN = "tkn-fake-running";
const GENERATION = 1;

interface Case {
  name: string;
  lane: string;
  approval: string;
  status: string;
  payload_mismatch?: boolean;
  expect_applied: boolean;
}

const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/cross-check-running-guard/cases.json", import.meta.url), "utf8"),
) as { cases: Case[] };

const LANES = ["gated", "required_forced_human", "required_auto", "required_auto_parked", "ordinary"];
const APPROVALS = ["applied_bound_current", "unapplied", "stale_revision", "unbound", "superseded", "none"];
const STATUSES = ["claimed", "awaiting_approval", "running"];

describe("fixture shape", () => {
  it("uses only known enum values", () => {
    assert.ok(fixture.cases.length > 0);
    for (const c of fixture.cases) {
      assert.ok(LANES.includes(c.lane), `${c.name}: lane ${c.lane}`);
      assert.ok(APPROVALS.includes(c.approval), `${c.name}: approval ${c.approval}`);
      assert.ok(STATUSES.includes(c.status), `${c.name}: status ${c.status}`);
      assert.equal(typeof c.expect_applied, "boolean", c.name);
    }
    assert.equal(new Set(fixture.cases.map((c) => c.name)).size, fixture.cases.length, "case names are unique");
  });
});

// The pure rule, model built directly from the case.
describe("crossCheckRunningAdmitted", () => {
  // A parked auto run has auto_approve cleared, so it is the human-gated `required` row.
  const lane = (l: string): CrossCheckLane =>
    l === "required_forced_human" || l === "required_auto_parked" ? "required" : (l as CrossCheckLane);
  it("matches every fixture case", () => {
    let ran = 0;
    for (const c of fixture.cases) {
      const current = 2;
      const model: CrossCheckModel = {
        lane: lane(c.lane), plan: "plan B", presentedPlan: c.payload_mismatch ? "other" : "plan B", gateRevision: current,
      };
      const row = (over: Partial<CrossCheckApproval>): CrossCheckApproval =>
        ({ applied: true, superseded: false, bound: true, gateRevision: current, ...over });
      const approvals: CrossCheckApproval[] = {
        applied_bound_current: [row({})],
        unapplied: [row({ applied: false })],
        stale_revision: [row({ gateRevision: current - 1 })],
        unbound: [row({ bound: false, gateRevision: undefined })],
        superseded: [row({ superseded: true })],
        none: [],
      }[c.approval]!;
      assert.equal(crossCheckRunningAdmitted(model, approvals, c.status), c.expect_applied, c.name);
      ran++;
    }
    assert.equal(ran, fixture.cases.length);
  });
});

// The same cases through the fake's HTTP state route.
describe("FakeApi /state running rule", () => {
  let api: FakeApi;
  let base: string;
  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    api.gateRevisions = true;
    base = await api.listen();
  });
  afterEach(async () => {
    await api.close();
  });

  async function post(path: string, body: unknown): Promise<{ status: number; json: any }> {
    const res = await fetch(`${base}/api/worker/runs/${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${TOKEN}` },
      body: JSON.stringify(body),
    });
    return { status: res.status, json: await res.json().catch(() => ({})) };
  }
  const gate = (runId: string, plan: string) =>
    post(`${runId}/state`, { status: "awaiting_approval", plan_md: plan, claim_generation: GENERATION });
  const receipt = (runId: string, kind: "ack" | "applied" | "discarded", id: number) =>
    post(`${runId}/inputs/${kind}`, { ids: [id], claim_generation: GENERATION });

  let nextInput = 70_000;

  it("agrees with every fixture case", async () => {
    let ran = 0;
    const ordinaryAwaitingRan: string[] = [];
    // The fake keeps an ordinary run's accept-everything behaviour by default; this test opts in so
    // the route is held to the api's awaiting_approval arm for ordinary runs as well.
    api.modelOrdinaryRunningRule = true;
    for (const c of fixture.cases) {
      if (c.lane === "ordinary" && c.status === "awaiting_approval") ordinaryAwaitingRan.push(c.name);
      const autoStart = c.lane === "required_auto" || c.lane === "required_auto_parked";
      const claim = makeClaim({
        claim_generation: GENERATION,
        auto_approve: autoStart,
        plan_cross_check_gated: c.lane === "gated",
        plan_cross_check_required: c.lane === "required_forced_human" || autoStart,
        // An auto-approved required run stores its plan without a park (SetRunAutopilotPlan).
        ...(c.lane === "required_auto" ? { plan_md: "plan A" } : {}),
      });
      const runId = claim.run_id;
      api.enqueueClaim(claim);
      api.setInputClaimGeneration(runId, GENERATION);

      let rev1 = 0;
      if (c.lane === "required_auto") {
        assert.equal(c.approval, "none", `${c.name}: required_auto has no gate to approve`);
      } else {
        assert.equal((await gate(runId, "plan A")).status, 200, `${c.name}: gate A`);
        rev1 = api.gateOf(runId).revision;
      }
      const approve = (revision: number | undefined, binding: "bound" | "unbound" | undefined): number => {
        const id = nextInput++;
        api.appendInputs(runId, [{
          id, kind: "approve_plan", body: "{}",
          ...(binding !== undefined ? { gate_binding: binding } : {}),
          ...(revision !== undefined ? { gate_revision: revision } : {}),
        }]);
        return id;
      };
      const apply = async (id: number): Promise<void> => {
        assert.equal((await receipt(runId, "ack", id)).status, 200, `${c.name}: ack`);
        assert.equal((await receipt(runId, "applied", id)).status, 200, `${c.name}: applied`);
      };
      switch (c.approval) {
        case "applied_bound_current":
          await apply(approve(rev1, "bound"));
          break;
        case "unapplied":
          approve(rev1, "bound");
          break;
        case "stale_revision":
          await apply(approve(rev1, "bound"));
          assert.equal((await gate(runId, "plan B")).status, 200, `${c.name}: gate B`);
          assert.ok(api.gateOf(runId).revision > rev1, `${c.name}: revision advanced`);
          break;
        case "unbound":
          await apply(approve(undefined, "unbound"));
          break;
        case "superseded": {
          const id = approve(rev1, "bound");
          assert.equal((await receipt(runId, "ack", id)).status, 200);
          assert.equal((await receipt(runId, "discarded", id)).status, 200);
          break;
        }
        case "none":
          break;
      }
      if (c.payload_mismatch) api.setStoredPlan(runId, "a plan the gate never presented");
      api.setOwnershipStatus(runId, c.status, GENERATION);

      const before = api.states.length;
      const refusalsBefore = api.crossCheckRunningRefusals.length;
      const res = await post(`${runId}/state`, { status: "running", claim_generation: GENERATION });
      if (c.expect_applied) {
        assert.equal(res.status, 200, c.name);
        assert.equal(res.json.run.status, "running", c.name);
        assert.equal(api.states.length, before + 1, `${c.name}: recorded`);
        assert.equal(api.crossCheckRunningRefusals.length, refusalsBefore, `${c.name}: no refusal recorded`);
      } else {
        assert.equal(api.crossCheckRunningRefusals.length, refusalsBefore + 1, `${c.name}: refusal recorded`);
        assert.equal(res.status, 409, c.name);
        assert.equal(res.json.run.status, c.status, `${c.name}: acks the current status`);
        assert.equal(api.states.length, before, `${c.name}: a refusal records nothing`);
      }
      ran++;
    }
    api.modelOrdinaryRunningRule = false;
    // Every case, the ordinary awaiting_approval ones included, ran through the HTTP route.
    assert.ok(ordinaryAwaitingRan.length > 0, "ordinary awaiting_approval cases ran through the route");
    assert.equal(ran, fixture.cases.length);
  });

  it("accepts the pre-plan running report of a gated run", async () => {
    const claim = makeClaim({ claim_generation: GENERATION, auto_approve: false, plan_cross_check_gated: true });
    api.enqueueClaim(claim);
    const res = await post(`${claim.run_id}/state`, { status: "running", claim_generation: GENERATION });
    assert.equal(res.status, 200);
  });

  it("enqueueClaim keeps a row status the test set for the same generation", async () => {
    const claim = makeClaim({ claim_generation: GENERATION, auto_approve: false, plan_cross_check_gated: true });
    api.enqueueClaim(claim);
    await gate(claim.run_id, "plan A");
    api.setOwnershipStatus(claim.run_id, "running", GENERATION);
    api.enqueueClaim(claim);
    const res = await post(`${claim.run_id}/state`, { status: "running", claim_generation: GENERATION });
    assert.equal(res.status, 409);
    assert.equal(res.json.run.status, "running");
  });

  it("a gate-resume claim seeds the presented plan so an approved human-gated run is admitted", async () => {
    const claim = makeClaim({ claim_generation: GENERATION, auto_approve: false, plan_cross_check_gated: true,
      plan_md: "plan A", resume_gate_revision: 3 });
    api.enqueueClaim(claim);
    api.setInputClaimGeneration(claim.run_id, GENERATION);
    const id = nextInput++;
    api.appendInputs(claim.run_id, [{ id, kind: "approve_plan", body: "{}", gate_binding: "bound", gate_revision: 3 }]);
    assert.equal((await receipt(claim.run_id, "ack", id)).status, 200);
    assert.equal((await receipt(claim.run_id, "applied", id)).status, 200);
    const res = await post(`${claim.run_id}/state`, { status: "running", claim_generation: GENERATION });
    assert.equal(res.status, 200);
  });

  it("requeueRun re-offers the run as claimed and a refused running report leaves it claimed", async () => {
    const claim = makeClaim({ claim_generation: GENERATION, auto_approve: false, plan_cross_check_gated: true });
    api.enqueueClaim(claim);
    await gate(claim.run_id, "plan A");
    const next = api.requeueRun(claim, GENERATION + 1);
    // The ownership probe reports `claimed` at the next generation (not the stale `running`).
    const own = await fetch(`${base}/api/worker/runs/${next.run_id}/ownership`, { headers: { Authorization: `Bearer ${TOKEN}` } });
    const ownBody = await own.json() as { status?: string; claim_generation?: number };
    assert.equal(ownBody.status, "claimed");
    assert.equal(ownBody.claim_generation, GENERATION + 1);
    const res = await post(`${next.run_id}/state`, { status: "running", claim_generation: GENERATION + 1 });
    assert.equal(res.status, 409);
    assert.equal(res.json.run.status, "claimed");
  });
});
