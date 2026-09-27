// PRD #1795 M3: the client half of gate-revision reports. The awaiting_approval ACK's top-level
// gate_revision is read like contractRevision, a refusal 409's reason reaches the caller, and the
// presentation fields never ride a report that does not also carry claim_generation (the api
// refuses an id-bearing report without it, and a rolled-back api strict-decodes both away).
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";

const TOKEN = "worker-join-token-0123456789";
const ID = "8b0c1a2e-3f4d-4e5f-8a6b-7c8d9e0f1a2b";

interface Post {
  body: Record<string, unknown>;
}

async function stateServer(
  features: string[],
  respond: (body: Record<string, unknown>, attempt: number) => { status: number; body: unknown },
): Promise<{ url: string; posts: Post[]; close: () => Promise<void> }> {
  const posts: Post[] = [];
  const server = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += String(c)));
    req.on("end", () => {
      const body = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};
      const url = new URL(req.url ?? "/", "http://fake");
      if (url.pathname.endsWith("/register")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ worker_id: "w", protocol_features: features }));
        return;
      }
      const answer = respond(body, posts.length);
      posts.push({ body });
      res.writeHead(answer.status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(answer.body));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  server.unref();
  const { port } = server.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, posts, close: () => new Promise<void>((r) => server.close(() => r())) };
}

const servers: { close: () => Promise<void> }[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

async function client(features: string[], respond: Parameters<typeof stateServer>[1]) {
  const srv = await stateServer(features, respond);
  servers.push(srv);
  const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  await c.register("w");
  return { c, posts: srv.posts };
}

const GATE = { status: "awaiting_approval" as const, plan_md: "# P", presentation_id: ID, claim_generation: 3 };

describe("gate-revision reports (PRD #1795)", () => {
  it("reads the ACK's top-level gate_revision (a positive integer only)", async () => {
    for (const [value, expected] of [[4, 4], [0, undefined], [-1, undefined], [2.5, undefined], ["4", undefined], [null, undefined]] as const) {
      const { c } = await client(["claim_generation_fence", "gate_revision_v1"], () => ({
        status: 200,
        body: { run: { status: "awaiting_approval" }, gate_revision: value },
      }));
      const ack = await c.reportState("r1", GATE);
      assert.equal(ack.gateRevision, expected, `gate_revision ${JSON.stringify(value)}`);
    }
  });

  it("surfaces a refusal 409's reason with applied:false", async () => {
    const { c } = await client(["claim_generation_fence", "gate_revision_v1"], () => ({
      status: 409,
      body: { run: { status: "running" }, reason: "gate_presentation_historical" },
    }));
    const ack = await c.reportState("r1", GATE);
    assert.deepStrictEqual([ack.applied, ack.status, ack.reason, ack.gateRevision], [false, "running", "gate_presentation_historical", undefined]);
  });

  it("sends the presentation fields beside claim_generation", async () => {
    const { c, posts } = await client(["claim_generation_fence", "gate_revision_v1"], () => ({ status: 200, body: { run: { status: "awaiting_approval" }, gate_revision: 1 } }));
    await c.reportState("r1", { ...GATE, adopt_gate_revision: 2 });
    assert.deepStrictEqual(posts.map((p) => [p.body.presentation_id, p.body.adopt_gate_revision, p.body.claim_generation]), [[ID, 2, 3]]);
  });

  it("strips them with claim_generation when the generation is not sent", async () => {
    // No fence feature and no credential-switch capability: claim_generation is not stamped.
    const { c, posts } = await client(["gate_revision_v1"], () => ({ status: 200, body: { run: { status: "awaiting_approval" } } }));
    await c.reportState("r1", { ...GATE, adopt_gate_revision: 2 });
    assert.ok(!("presentation_id" in posts[0]!.body) && !("adopt_gate_revision" in posts[0]!.body) && !("claim_generation" in posts[0]!.body));
  });

  it("strips them on the strict-decode fallback of a rolled-back api", async () => {
    const { c, posts } = await client(["claim_generation_fence", "gate_revision_v1"], (_b, attempt) =>
      attempt === 0 ? { status: 400, body: { error: "invalid request body" } } : { status: 200, body: { run: { status: "awaiting_approval" } } },
    );
    const ack = await c.reportState("r1", GATE);
    assert.equal(ack.applied, true);
    assert.equal(posts[0]!.body.presentation_id, ID);
    assert.ok(!("presentation_id" in posts[1]!.body) && !("claim_generation" in posts[1]!.body), "the retry is an id-less report");
  });

  it("a transient failure retries the SAME body, so the presentation id is reused", async () => {
    const { c, posts } = await client(["claim_generation_fence", "gate_revision_v1"], (_b, attempt) =>
      attempt === 0 ? { status: 503, body: { error: "try again" } } : { status: 200, body: { run: { status: "awaiting_approval" }, gate_revision: 5 } },
    );
    const ack = await c.reportState("r1", GATE);
    assert.equal(ack.gateRevision, 5);
    assert.deepStrictEqual(posts.map((p) => p.body.presentation_id), [ID, ID]);
  });
});
