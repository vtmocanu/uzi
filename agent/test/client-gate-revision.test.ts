// PRD #1795 M3: the client half of gate-revision reports. The awaiting_approval ACK's top-level
// gate_revision is read like contractRevision, a refusal 409's reason reaches the caller, and the
// presentation fields never ride a report that does not also carry claim_generation (the api
// refuses an id-bearing report without it, and a rolled-back api strict-decodes both away).
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RevisionSourceFenceUnavailableError } from "../src/client.js";

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

const REVISION = { ...GATE, expected_gate_revision: 2, revision_input_id: 10, superseded_revision_input_ids: [8, 9] };

describe("revision retry wire contract", () => {
  it("requires negotiated source support and never weakens a source fence after rollback", async () => {
    for (const features of [
      ["claim_generation_fence", "gate_revision_v1"],
      ["gate_revision_v1", "plan_revision_retry_v1"],
      ["claim_generation_fence", "gate_revision_v1", "plan_revision_retry_v1"],
    ]) {
      const negotiated = features.includes("plan_revision_retry_v1") && features.includes("claim_generation_fence");
      const { c, posts } = await client(features, (_b, attempt) =>
        negotiated && attempt === 0 ? { status: 400, body: { error: "invalid request body" } }
          : { status: 200, body: { run: { status: "awaiting_approval" } } });
      if (negotiated) await assert.rejects(c.reportState("r1", REVISION), /invalid request body/);
      else await assert.rejects(c.reportState("r1", REVISION), RevisionSourceFenceUnavailableError);
      for (const post of posts) {
        for (const key of ["expected_gate_revision", "revision_input_id", "superseded_revision_input_ids"]) {
          assert.equal(key in post.body, negotiated, key);
        }
      }
      assert.equal(posts.length, negotiated ? 1 : 0, "a source write cannot fall back to an unfenced report");
    }
  });

  it("heartbeat rollback cannot strip an established source fence", async () => {
    const { c, posts } = await client(["claim_generation_fence", "gate_revision_v1", "plan_revision_retry_v1", "heartbeat_outbox"], body => {
      if (body.version !== undefined) return body.outbox !== undefined
        ? { status: 400, body: { error: "invalid request body" } }
        : { status: 200, body: {} };
      return { status: 200, body: { run: { status: "awaiting_approval" }, gate_revision: 3,
        revision_input_id: 10, superseded_revision_input_ids: [8, 9] } };
    });
    await c.reportState("r1", REVISION);
    await c.heartbeat(undefined, [{ run_id: "r1", pending_messages: 1, pending_terminal: 0, stale_retired: 0, since: 1 }]);
    assert.deepEqual(c.protocolFeatures, []);
    const before = posts.length;
    await assert.rejects(c.reportState("r1", REVISION), RevisionSourceFenceUnavailableError);
    assert.equal(posts.length, before, "no unfenced successor POST after heartbeat fallback");
    assert.equal(posts.filter(p => p.body.status !== undefined).length, 1);
    assert.equal(posts[0]!.body.expected_gate_revision, 2);
  });

  it("a source request without positive included generation sends nothing", async () => {
    const { c, posts } = await client(["claim_generation_fence", "plan_revision_retry_v1"], () => ({
      status: 200, body: { run: { status: "awaiting_approval" } },
    }));
    for (const generation of [undefined, 0, -1, 1.5]) {
      await assert.rejects(c.reportState("r1", { ...REVISION, claim_generation: generation }), RevisionSourceFenceUnavailableError);
    }
    assert.equal(posts.length, 0);
  });

  it("decodes the receipt atomically with strict positive safe IDs", async () => {
    const { c } = await client(["claim_generation_fence", "plan_revision_retry_v1"], (body) => ({
      status: 200, body: { run: { status: "awaiting_approval" },
        revision_input_id: body.revision_input_id, superseded_revision_input_ids: body.superseded_revision_input_ids },
    }));
    for (const [id, ids, valid] of [
      [10, [], true], [10, [8, 9], true], [0, [8], false], [-1, [], false],
      [1.5, [], false], ["10", [], false], [Number.MAX_SAFE_INTEGER + 1, [], false],
      [10, [8, 0], false], [10, [8, "9"], false], [10, [8, 8], false],
      [10, [10], false], [10, null, false], [10, undefined, false],
      [undefined, [8], false], [10, [Number.MAX_SAFE_INTEGER + 1], false],
    ] as const) {
      const ack = await c.reportState("r1", { ...REVISION,
        revision_input_id: id as number, superseded_revision_input_ids: ids as unknown as number[] });
      assert.equal(ack.revisionInputId, valid ? id : undefined);
      assert.deepEqual(ack.supersededRevisionInputIds, valid ? ids : undefined);
    }
  });

  it("surfaces stale source distinctly without retrying a presentation refusal", async () => {
    const { c, posts } = await client(["claim_generation_fence", "plan_revision_retry_v1"], () => ({
      status: 409, body: { run: { status: "awaiting_approval" }, reason: "stale_revision_source" },
    }));
    const ack = await c.reportState("r1", REVISION);
    assert.equal(ack.staleRevisionSource, true);
    assert.equal(ack.applied, false);
    assert.equal(posts.length, 1);
  });
});

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
