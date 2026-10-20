import { it } from "node:test";
import assert from "node:assert/strict";
import type { WorkerClient, PlanCrossCheckCandidate } from "../src/client.js";
import { SemanticPlanningGate } from "../src/semantic-planning-gate.js";
import { boundedTransport, proposal } from "./semantic-coverage-fixture.js";
import { semanticServer } from "./semantic-server-fixture.js";
const structural: PlanCrossCheckCandidate = { plan_md: "Implement A", milestones: [{ id: "m1", title: "A" }],
  required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40), planning_diff: "" };
const sources = [{ id: "issue:1", kind: "issue", provenance: "server:issue:1", completeness: "complete",
  bytes: Buffer.from("Implement A").toString("base64") }];

for (const harness of ["claude", "codex"] as const) {
  it("M2 " + harness + " server admission settles before publish and same-candidate replay uses persisted report", async () => {
    const client = {} as WorkerClient;
    const events: string[] = [];
    const server = semanticServer(client, events);
    const transport = await boundedTransport(harness, server.report, events);
    const gate = new SemanticPlanningGate(client, "run", 1, new AbortController().signal, transport);
    const c = await gate.compile(structural, proposal(), sources);
    assert.equal((await gate.review(c, structural.plan_md, sources)).outcome, "complete");
    assert.deepEqual(events, ["compile", "status", "reserve", "invoke", "settle:success", "publish"]);
    events.length = 0;
    assert.equal((await gate.review(c, structural.plan_md, sources)).outcome, "complete");
    assert.deepEqual(events, ["status"]);
    // A changed plan cannot inherit a previous report or call authorization.
    const changed = await gate.compile({ ...structural, plan_md: "Implement A differently" }, proposal(), sources);
    assert.notEqual(changed.binding.candidate_digest, c.binding.candidate_digest);
    server.deny(); events.length = 0;
    assert.equal((await gate.review(changed, "Implement A differently", sources)).reason, "coverage_exhausted");
    assert.deepEqual(events, ["status", "reserve"]);
  });
}
it("M2 schema retry preserves review ID, uses new request UUID, and publishes only the parsed report", async () => {
  const client = {} as WorkerClient;
  const events: string[] = [];
  const server = semanticServer(client, events);
  let outputs = 0;
  const transport = await boundedTransport("claude", () => ++outputs === 1 ? {} : server.report(), events);
  const gate = new SemanticPlanningGate(client, "run", 1, new AbortController().signal, transport);
  const c = await gate.compile(structural, proposal(), sources);
  assert.equal((await gate.review(c, structural.plan_md, sources)).outcome, "complete");
  assert.deepEqual(events, ["compile", "status", "reserve", "invoke", "settle:schema_error", "reserve", "invoke", "settle:success", "publish"]);
  const [first, second] = server.requests as { request_id: string; review_id?: string }[];
  assert.notEqual(first!.request_id, second!.request_id);
  assert.equal(first!.review_id, undefined);
  assert.ok(second!.review_id);
});
it("M2 replay never authorizes a call and live defaults make zero calls or reservations", async () => {
  const client = {} as WorkerClient;
  const events: string[] = [];
  const server = semanticServer(client, events);
  const transport = await boundedTransport("claude", server.report, events);
  const gate = new SemanticPlanningGate(client, "run", 1, new AbortController().signal, transport);
  const c = await gate.compile(structural, proposal(), sources);
  server.replay(); events.length = 0;
  assert.equal((await gate.review(c, structural.plan_md, sources)).reason, "coverage_exhausted");
  assert.deepEqual(events, ["status", "reserve"]);
  const live = new SemanticPlanningGate(client, "run", 1, new AbortController().signal);
  events.length = 0;
  assert.equal((await live.review(c, structural.plan_md, sources)).reason, "coverage_unavailable");
  assert.deepEqual(events, ["status"]);
});
it("M2 settlement failure never publishes even a valid report", async () => {
  const client = {} as WorkerClient;
  const events: string[] = [];
  const server = semanticServer(client, events);
  client.settleSemanticCoverage = async () => { events.push("settle-failed"); throw new Error("lost"); };
  const transport = await boundedTransport("claude", server.report, events);
  const gate = new SemanticPlanningGate(client, "run", 1, new AbortController().signal, transport);
  const c = await gate.compile(structural, proposal(), sources);
  assert.equal((await gate.review(c, structural.plan_md, sources)).reason, "coverage_settlement_failed");
  assert.equal(events.includes("publish"), false);
});

it("M4 an owner abort during coverage is never classified as an unavailable-coverage park", async () => {
  const client = {} as WorkerClient;
  const events: string[] = [];
  const server = semanticServer(client, events);
  const transport = await boundedTransport("claude", server.report, events);
  const owner = new AbortController();
  const gate = new SemanticPlanningGate(client, "run", 1, owner.signal, transport);
  const c = await gate.compile(structural, proposal(), sources);
  const abort = Object.assign(new Error("aborted"), { name: "AbortError" });
  server.failStatus(abort);
  // Not aborted by the owner: an AbortError is a timeout-class transport failure.
  assert.equal((await gate.reviewForGate(c, structural.plan_md, sources)).outcome, "unavailable");
  owner.abort(new Error("owner cancelled"));
  await assert.rejects(gate.reviewForGate(c, structural.plan_md, sources), error => error === abort || /owner cancelled|aborted/.test(String(error)));
});
