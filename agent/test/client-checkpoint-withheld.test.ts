// Issue #4257 m2: the client side of the checkpoint-publication diagnostic.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { WorkerClient } from "../src/client.js";
import { CHECKPOINT_PUBLICATION_WITHHELD_FEATURE, RUN_REGISTRATION_EVIDENCE_FEATURE } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const fixture = JSON.parse(readFileSync(new URL("../../fixtures/worker-protocol/checkpoint-publication-withheld-running.json", import.meta.url), "utf8")) as {
  status: string; claim_generation: number;
  checkpoint_publication: { state: "withheld"; since: string; claim_generation: number; register_nonce: string; report_seq: number };
};
const PUB = fixture.checkpoint_publication;
const NONCE = PUB.register_nonce;
const GEN = fixture.claim_generation;
const FEATURES = ["claim_generation_fence", CHECKPOINT_PUBLICATION_WITHHELD_FEATURE];
const input = { state: "withheld" as const, since: PUB.since, report_seq: PUB.report_seq };
const ok = JSON.stringify({ run: { id: "r1", status: "running" } });

type Reply = "strict" | "ok" | { status: number; body: string };

async function make(replies: Reply[], opts: { features?: string[]; nonce?: string | null } = {}) {
  const wires: string[] = [];
  let registrations = 0;
  const transport: typeof fetch = async (url, init) => {
    if (String(url).endsWith("/register")) {
      registrations++;
      return new Response(JSON.stringify({
        worker_id: "11111111-1111-4111-8111-111111111111",
        ...(opts.nonce === null ? {} : { register_nonce: opts.nonce ?? NONCE }),
        protocol_features: opts.features ?? FEATURES,
      }));
    }
    wires.push(String(init?.body));
    const reply = replies.shift();
    assert.ok(reply, "unexpected additional request");
    if (reply === "ok") return new Response(ok);
    if (reply === "strict") return new Response('{"error":"invalid request body"}', { status: 400 });
    return new Response(reply.body, { status: reply.status });
  };
  const c = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
    fetch: transport, sleep: async () => {}, terminalRetrySchedule: [0],
  });
  await c.register("fixture", "base", 1, undefined, ["credential_switch_v1"], undefined, 0);
  return { c, wires, registrations: () => registrations };
}
const parsed = (wires: string[]) => wires.map(w => JSON.parse(w) as Record<string, unknown>);

describe("checkpoint_publication on the running report", () => {
  it("emits exactly the shared fixture body, nonce and generation stamped by the client", async () => {
    const { c, wires } = await make(["ok"]);
    await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input });
    assert.deepEqual(parsed(wires), [fixture]);
  });

  it("a cleared report carries no since", async () => {
    const { c, wires } = await make(["ok"]);
    await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: { state: "cleared", report_seq: 4 } });
    assert.deepEqual(parsed(wires)[0]!.checkpoint_publication, { state: "cleared", claim_generation: GEN, register_nonce: NONCE, report_seq: 4 });
  });

  const omissions: [string, { features?: string[]; nonce?: string | null }, number | undefined, "running" | "failed", typeof input | undefined][] = [
    ["feature not advertised", { features: ["claim_generation_fence"] }, GEN, "running", input],
    ["no register nonce", { nonce: null }, GEN, "running", input],
    ["generation 0", {}, 0, "running", input],
    ["generation missing", {}, undefined, "running", input],
    ["non-running status", {}, GEN, "failed", input],
    ["no caller input", {}, GEN, "running", undefined],
    ["report_seq 0", {}, GEN, "running", { ...input, report_seq: 0 }],
  ];
  for (const [name, setup, gen, status, inp] of omissions) {
    it(`omits the field: ${name}`, async () => {
      const { c, wires } = await make(["ok"], setup);
      await c.reportState("r1", { status, ...(status === "failed" ? { failure_reason: "x" } : {}), ...(gen === undefined ? {} : { claim_generation: gen }) },
        undefined, inp === undefined ? undefined : { checkpointPublication: inp });
      assert.equal(wires.length, 1);
      assert.equal("checkpoint_publication" in parsed(wires)[0]!, false);
    });
  }

  it("strips a caller-set field even when the client would attach nothing", async () => {
    const { c, wires } = await make(["ok", "ok"]);
    const forged = { ...PUB, report_seq: 99, register_nonce: "forged" };
    await c.reportState("r1", { status: "running", claim_generation: GEN, checkpoint_publication: forged });
    await c.reportState("r1", { status: "running", claim_generation: GEN, checkpoint_publication: forged }, undefined, { checkpointPublication: input });
    const [first, second] = parsed(wires);
    assert.equal("checkpoint_publication" in first!, false);
    assert.deepEqual(second!.checkpoint_publication, PUB);
  });

  it("surfaces the ACK value", async () => {
    for (const value of ["recorded", "cleared", "refused", "unrecorded"] as const) {
      const { c } = await make([{ status: 200, body: JSON.stringify({ run: { id: "r1", status: "running" }, checkpoint_publication: value }) }]);
      const ack = await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input });
      assert.equal(ack.checkpointPublication, value);
    }
  });

  it("surfaces the ACK value from a 409 body too, and ignores an unknown value", async () => {
    const { c } = await make([{ status: 409, body: JSON.stringify({ run: { id: "r1", status: "running" }, checkpoint_publication: "refused" }) },
      { status: 200, body: JSON.stringify({ run: { id: "r1", status: "running" }, checkpoint_publication: "bogus" }) }]);
    const a1 = await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input });
    assert.equal(a1.checkpointPublication, "refused");
    const a2 = await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input });
    assert.equal(a2.checkpointPublication, undefined);
  });

  describe("strict-decode refusal", () => {
    it("strips the field once, resends otherwise identical, omits it until re-register", async () => {
      const { c, wires, registrations } = await make(["strict", "ok", "ok", "ok"]);
      const opts = { checkpointPublication: input };
      await c.reportState("r1", { status: "running", claim_generation: GEN, session_id: "s" }, undefined, opts);
      const [first, second] = parsed(wires);
      assert.ok(first!.checkpoint_publication);
      const expected = structuredClone(first!);
      delete expected.checkpoint_publication;
      assert.deepEqual(second, expected);
      assert.equal(wires.length, 2);
      c.protocolFeatures = FEATURES;
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, opts);
      assert.equal("checkpoint_publication" in parsed(wires)[2]!, false);
      await c.register("fixture", "base", 1, undefined, ["credential_switch_v1"], undefined, 0);
      assert.equal(registrations(), 2);
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, opts);
      assert.deepEqual(parsed(wires)[3], fixture);
    });

    it("is resolved before the registration-evidence fallback: evidence survives", async () => {
      const { c, wires } = await make(["strict", "ok"], { features: [...FEATURES, RUN_REGISTRATION_EVIDENCE_FEATURE] });
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input, registrationEpoch: 2 });
      const [first, second] = parsed(wires);
      assert.ok(first!.checkpoint_publication && first!.registration_evidence);
      assert.equal("checkpoint_publication" in second!, false);
      assert.ok(second!.registration_evidence);
    });

    for (const [name, reply] of [
      ["unrelated 400", { status: 400, body: '{"error":"invalid plan"}' }],
      ["similar-prose 400", { status: 400, body: '{"error":"invalid request body for this plan"}' }],
    ] as const) {
      it(`${name} never strips the diagnostic and keeps the feature`, async () => {
        // The pre-existing loose generation fallback may resend prose-matching 400s; that resend
        // is not the diagnostic strip, so every body still carries the field.
        const { c, wires } = await make([reply, "ok", "ok"]);
        await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input }).catch(() => undefined);
        assert.ok(parsed(wires).every(w => w.checkpoint_publication !== undefined || w.claim_generation === undefined));
        await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { checkpointPublication: input }).catch(() => undefined);
        assert.ok(parsed(wires).at(-1)!.checkpoint_publication);
      });
    }
  });
});
