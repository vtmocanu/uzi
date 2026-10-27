import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient, type PlanCrossCheckStateRequest } from "../src/client.js";
import { CAPABILITY_INFERENCE_EVIDENCE_FEATURE } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const servers: http.Server[] = [];
afterEach(async () => { await Promise.all(servers.splice(0).map(s => new Promise<void>((resolve,reject) => s.close(e => e ? reject(e) : resolve())))); });

describe("capability evidence negotiation", () => {
  for (const lane of ["ordinary", "checked", "semantic"] as const) {
    for (const enabled of [false, true]) it(`${lane}: advisory field negotiated=${enabled}`, async () => {
      const posts: Record<string,unknown>[] = [];
      const server = http.createServer((req,res) => {
        let raw="";req.on("data",c => raw+=String(c));req.on("end",() => {
          const body = JSON.parse(raw) as Record<string,unknown>; posts.push(body);
          const known = new Set(["status","plan_md","claim_generation","candidate_digest",...(enabled?["capability_inference_evidence"]:[])]);
          if (Object.keys(body).some(key=>!known.has(key))) {
            res.writeHead(400,{"Content-Type":"application/json"});res.end(JSON.stringify({error:"unknown field"}));return;
          }
          res.writeHead(200,{"Content-Type":"application/json"});
          res.end(JSON.stringify({run:{id:"r1",status:"running"}}));
        });
      });
      servers.push(server);
      await new Promise<void>(resolve => server.listen(0,"127.0.0.1",resolve));server.unref();
      const c = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`,"test-worker-token","test",nullLogger(),{sleep:async()=>{}});
      c.protocolFeatures=["claim_generation_fence","gate_revision_v1",...(enabled?[CAPABILITY_INFERENCE_EVIDENCE_FEATURE]:[])];
      const body: PlanCrossCheckStateRequest={status:"running",plan_md:"plan",claim_generation:1,candidate_digest:"a".repeat(64),capability_inference_evidence:{jvm:{triggers:[{path:"app/android/build.gradle",reason:"jvm_manifest"}],truncated:false}}};
      if(lane==="ordinary") await c.reportState("r1",body);
      else if(lane==="checked") await c.reportPlanCrossCheckGateState("r1",body);
      else await c.reportSemanticPlanGateState("r1",body);
      assert.equal(posts.length,1);
      const posted = posts[0]; assert.ok(posted);
      assert.equal("capability_inference_evidence" in posted,enabled);
      assert.equal(posted.candidate_digest,body.candidate_digest);
      assert.equal(posted.claim_generation,1);
      assert.equal(posted.plan_md,"plan");
      assert.ok(body.capability_inference_evidence,"negotiation must not mutate caller snapshot");
    });
  }
});

type Lane = "ordinary" | "checked" | "semantic";
type Reply = "strict" | "ok" | "lost" | { status: number; body: string | ReadableStream<Uint8Array> };
const negotiated = ["claim_generation_fence", "gate_revision_v1", "plan_revision_retry_v1", "heartbeat_outbox", CAPABILITY_INFERENCE_EVIDENCE_FEATURE];
const success = JSON.stringify({ run: { id: "r1", status: "running" } });
function plan(): PlanCrossCheckStateRequest {
  const body: PlanCrossCheckStateRequest = { status: "running", plan_md: "immutable plan", claim_generation: 7,
    candidate_digest: "a".repeat(64), session_id: "session", presentation_id: "11111111-1111-4111-8111-111111111111",
    required_capabilities: ["jvm"], required_tools: ["go"], size_class: "s",
    capability_inference_evidence: { jvm: { triggers: [{ path: "app/pom.xml", reason: "jvm_manifest" }], truncated: false } },
  };
  return Object.freeze(body);
}
async function rollbackClient(replies: Reply[], sleep?: (c: WorkerClient, n: number) => void,
  onPost?: (c: WorkerClient, n: number) => void) {
  const wires: string[] = [];
  let c: WorkerClient;
  let sleeps = 0;
  const transport: typeof fetch = async (url, init) => {
    if (String(url).endsWith("/register")) return new Response(JSON.stringify({
      worker_id: "11111111-1111-4111-8111-111111111111", protocol_features: negotiated,
    }));
    wires.push(String(init?.body));
    onPost?.(c, wires.length);
    const reply = replies.shift();
    assert.ok(reply, "unexpected additional request variant");
    if (reply === "lost") throw new TypeError("lost ACK");
    if (reply === "ok") return new Response(success);
    if (reply === "strict") return new Response('{"error":"invalid request body"}', { status: 400 });
    return new Response(reply.body, { status: reply.status });
  };
  c = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
    fetch: transport, sleep: async () => { sleep?.(c, ++sleeps); }, terminalRetrySchedule: [0],
  });
  await c.register("fixture", "base", 1, undefined, ["credential_switch_v1"], undefined, 0);
  return { c, wires };
}
function report(lane: Lane, c: WorkerClient, body: PlanCrossCheckStateRequest, signal?: AbortSignal) {
  if (lane === "ordinary") return c.reportState("r1", body, signal);
  if (lane === "checked") return c.reportPlanCrossCheckGateState("r1", body, signal);
  return c.reportSemanticPlanGateState("r1", body, signal);
}
function withoutEvidence(body: PlanCrossCheckStateRequest) {
  const copy = structuredClone(body);
  delete copy.capability_inference_evidence;
  return copy;
}

describe("capability evidence API rollback", () => {
  for (const lane of ["ordinary", "checked", "semantic"] as const) {
    it(`${lane}: exact refusal retries only advisory metadata and drops only its feature after ACK`, async () => {
      const body = plan(), original = JSON.stringify(body);
      const { c, wires } = await rollbackClient(["strict", "ok"], undefined, (client, n) => {
        if (n === 2) assert.equal(client.hasFeature(CAPABILITY_INFERENCE_EVIDENCE_FEATURE), true, "wait for a valid ACK before dropping");
      });
      await report(lane, c, body);
      assert.deepEqual(wires.map(w => JSON.parse(w)), [body, withoutEvidence(body)]);
      assert.equal(JSON.stringify(body), original, "caller snapshot remains unchanged");
      assert.equal(c.hasFeature(CAPABILITY_INFERENCE_EVIDENCE_FEATURE), false);
      for (const feature of negotiated.filter(f => f !== CAPABILITY_INFERENCE_EVIDENCE_FEATURE))
        assert.equal(c.hasFeature(feature), true, `keep ${feature}`);
    });

    it(`${lane}: lost ACK retries stay identical within each frozen variant despite feature refresh`, async () => {
      const body = plan();
      const { c, wires } = await rollbackClient(["lost", "strict", "lost", "ok"], (client, n) => {
        client.protocolFeatures = n === 1 ? negotiated.filter(f => f !== CAPABILITY_INFERENCE_EVIDENCE_FEATURE) : negotiated;
      });
      await report(lane, c, body);
      assert.equal(wires.length, 4);
      assert.equal(wires[0], wires[1], "primary transient retry is byte-identical");
      assert.equal(wires[2], wires[3], "compatibility transient retry is byte-identical");
      assert.deepEqual(JSON.parse(wires[2]!), withoutEvidence(body));
      assert.equal(c.hasFeature(CAPABILITY_INFERENCE_EVIDENCE_FEATURE), false);
    });

    it(`${lane}: a second strict refusal propagates without dropping generation or sibling features`, async () => {
      const body = plan();
      const { c, wires } = await rollbackClient(["strict", "strict"]);
      await assert.rejects(report(lane, c, body));
      assert.deepEqual(wires.map(w => JSON.parse(w)), [body, withoutEvidence(body)]);
      assert.deepEqual(c.protocolFeatures, negotiated, "no successful negotiation means no feature is dropped");
    });

    it(`${lane}: compatibility preserves revision-source authority fields`, async () => {
      const body = Object.freeze({ ...plan(), expected_gate_revision: 3, revision_input_id: 9,
        superseded_revision_input_ids: [4, 5] });
      const { c, wires } = await rollbackClient(["strict", "ok"]);
      await report(lane, c, body);
      assert.deepEqual(wires.map(w => JSON.parse(w)), [body, withoutEvidence(body)]);
    });

    it(`${lane}: caller mutation during backoff cannot alter the owned wire snapshot`, async () => {
      const body = structuredClone(plan()), original = structuredClone(body);
      const { c, wires } = await rollbackClient(["lost", "ok"], () => {
        body.required_capabilities?.push("docker");
        body.capability_inference_evidence!.jvm!.triggers[0]!.path = "changed/pom.xml";
      });
      await report(lane, c, body);
      assert.equal(wires[0], wires[1]);
      assert.deepEqual(JSON.parse(wires[1]!), original);
    });

    it(`${lane}: a 5xx with decoder-like prose retries the same primary variant`, async () => {
      const { c, wires } = await rollbackClient([{ status: 503, body: '{"error":"invalid request body"}' }, "ok"]);
      await report(lane, c, plan());
      assert.equal(wires.length, 2);
      assert.equal(wires[0], wires[1]);
      assert.equal(c.hasFeature(CAPABILITY_INFERENCE_EVIDENCE_FEATURE), true);
    });

    it(`${lane}: abort after a strict refusal prevents the compatibility POST`, async () => {
      const abort = new AbortController();
      const { c, wires } = await rollbackClient(["strict"], undefined, () => { abort.abort(new Error("cancelled")); });
      await assert.rejects(report(lane, c, plan(), abort.signal));
      assert.equal(wires.length, 1);
      assert.deepEqual(c.protocolFeatures, negotiated);
    });

    for (const [name, diagnostic] of [
      ["business", '{"error":"invalid plan"}'],
      ["extra key", '{"error":"invalid request body","reason":"invalid plan"}'],
      ["similar prose", '{"error":"invalid request body for this plan"}'],
      ["malformed", '{"error":"invalid request body"'],
      ["oversized", '{"error":"invalid request body"}' + " ".repeat(2 * 1024 * 1024)],
    ] as const) {
      it(`${lane}: ${name} 400 is not an evidence downgrade`, async () => {
        const body = plan();
        const { c, wires } = await rollbackClient([{ status: 400, body: diagnostic }]);
        await assert.rejects(report(lane, c, body));
        assert.deepEqual(wires.map(w => JSON.parse(w)), [body]);
        assert.deepEqual(c.protocolFeatures, negotiated);
      });
    }

    it(`${lane}: incomplete decoder diagnostic is not a downgrade`, async () => {
      const stream = new ReadableStream<Uint8Array>({ start(controller) {
        controller.enqueue(new TextEncoder().encode('{"error":"invalid request body"}'));
        controller.error(new Error("incomplete response"));
      } });
      const { c, wires } = await rollbackClient([{ status: 400, body: stream }]);
      await assert.rejects(report(lane, c, plan()));
      assert.equal(wires.length, 1);
      assert.deepEqual(c.protocolFeatures, negotiated);
    });

    it(`${lane}: transport exhaustion does not select a compatibility variant`, async () => {
      const { c, wires } = await rollbackClient(["lost", "lost"]);
      await assert.rejects(report(lane, c, plan()));
      assert.equal(wires.length, 2);
      assert.equal(wires[0], wires[1]);
      assert.ok(JSON.parse(wires[1]!).capability_inference_evidence);
      assert.deepEqual(c.protocolFeatures, negotiated);
    });

    it(`${lane}: malformed success does not close the evidence feature`, async () => {
      const { c, wires } = await rollbackClient(["strict", { status: 200, body: "{}" }]);
      await report(lane, c, plan()).catch(() => {});
      assert.equal(wires.length, 2);
      assert.deepEqual(c.protocolFeatures, negotiated);
    });
  }
});
