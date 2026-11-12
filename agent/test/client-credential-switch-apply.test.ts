import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient, type PlanCrossCheckStateRequest } from "../src/client.js";

// Issue #4201 m2a: the worker-side client of POST /runs/{id}/credential-switch/apply, the op_id on
// the credential_switch signal, and the op-fenced credential switch state reports.

const TOKEN = "worker-join-token-0123456789";
const SECRET = "sk-ant-oat01-" + "ABCDEFGH12345678"; // a token-shaped fixture assembled at runtime
const FEATURES = ["claim_generation_fence", "credential_switch_hold", "credential_switch_apply_v1"];
const REQ = { claimGeneration: 7, registerNonce: "nonce-1", opId: "op-1" };

interface Reply { status: number; headers?: Record<string, string>; body: string }
interface Post { path: string; body: Record<string, unknown> }

async function server(
  respond: (path: string, body: Record<string, unknown>) => Reply,
  registerBody: Record<string, unknown> = { worker_id: "w", register_nonce: "nonce-1", protocol_features: FEATURES },
): Promise<{ url: string; posts: Post[]; close: () => Promise<void> }> {
  const posts: Post[] = [];
  const srv = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += String(c)));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", "http://fake");
      const body = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};
      if (url.pathname === "/api/worker/register") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(registerBody));
        return;
      }
      posts.push({ path: url.pathname, body });
      const r = respond(url.pathname, body);
      res.writeHead(r.status, r.headers ?? { "Content-Type": "application/json" });
      res.end(r.body);
    });
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", () => r()));
  srv.unref();
  const { port } = srv.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, posts, close: () => new Promise<void>((r) => srv.close(() => r())) };
}

function clientFor(url: string): WorkerClient {
  return new WorkerClient(url, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1, 1] });
}

async function registered(url: string, caps: string[] = ["credential_switch_v1", "credential_switch_in_place_v1"]): Promise<WorkerClient> {
  const c = clientFor(url);
  await c.register("t", undefined, undefined, undefined, caps);
  return c;
}

const json = (status: number, body: unknown): Reply => ({ status, body: JSON.stringify(body) });

describe("applyCredentialSwitch classification", () => {
  const cases: Array<[string, Reply, Record<string, unknown>]> = [
    ["exact chi 404", { status: 404, headers: { "Content-Type": "text/plain; charset=utf-8" }, body: "404 page not found\n" }, { kind: "route_absent" }],
    ["exact chi 404 without newline", { status: 404, headers: { "Content-Type": "text/plain" }, body: "404 page not found" }, { kind: "route_absent" }],
    ["405", { status: 405, headers: { "Content-Type": "text/plain" }, body: "" }, { kind: "uncertain" }],
    ["405 with chi body", { status: 405, headers: { "Content-Type": "text/plain" }, body: "404 page not found\n" }, { kind: "uncertain" }],
    ["html 404", { status: 404, headers: { "Content-Type": "text/html" }, body: "<html>404 page not found</html>" }, { kind: "uncertain" }],
    ["json content-type with chi text", { status: 404, headers: { "Content-Type": "application/json" }, body: "404 page not found\n" }, { kind: "uncertain" }],
    ["text/plain with extra text", { status: 404, headers: { "Content-Type": "text/plain" }, body: "404 page not found\n\n" }, { kind: "uncertain" }],
    ["empty 404", { status: 404, headers: { "Content-Type": "text/plain" }, body: "" }, { kind: "uncertain" }],
    ["other json 404", json(404, { error: "something else" }), { kind: "uncertain" }],
    ["json 404 with extra key", json(404, { error: "run not found for this worker", x: 1 }), { kind: "uncertain" }],
    ["run not owned 404", json(404, { error: "run not found for this worker" }), { kind: "fenced" }],
    ["500", json(500, { error: "boom" }), { kind: "uncertain" }],
    ["502 html", { status: 502, headers: { "Content-Type": "text/html" }, body: "bad gateway" }, { kind: "uncertain" }],
    ["200 non-json", { status: 200, headers: { "Content-Type": "text/plain" }, body: "ok" }, { kind: "uncertain" }],
    ["200 unknown outcome", json(200, { outcome: "maybe" }), { kind: "uncertain" }],
    ["200 applied", json(200, { outcome: "applied", token: SECRET, ordinal: 1, message_seq: 5, replay: false }),
      { kind: "applied", token: SECRET, ordinal: 1, messageSeq: 5, replay: false }],
    ["200 applied replay", json(200, { outcome: "applied", token: SECRET, ordinal: 2, message_seq: 9, replay: true }),
      { kind: "applied", token: SECRET, ordinal: 2, messageSeq: 9, replay: true }],
    ["200 applied short token", json(200, { outcome: "applied", token: "short", ordinal: 1, message_seq: 5, replay: false }), { kind: "uncertain" }],
    ["200 applied missing token", json(200, { outcome: "applied", ordinal: 1, message_seq: 5 }), { kind: "uncertain" }],
    ["200 superseded", json(200, { outcome: "superseded" }), { kind: "superseded" }],
    ["200 fenced", json(200, { outcome: "fenced" }), { kind: "fenced" }],
    ["200 not_applied", json(200, { outcome: "not_applied", reason: "pool_empty" }), { kind: "not_applied", reason: "pool_empty" }],
    ["200 refused_committed", json(200, { outcome: "refused_committed", reason: "credential_gone" }), { kind: "refused_committed", reason: "credential_gone" }],
    ["200 not_applied without reason", json(200, { outcome: "not_applied" }), { kind: "uncertain" }],
  ];
  for (const [name, reply, want] of cases) {
    it(name, async () => {
      const s = await server(() => reply);
      try {
        const got = await (await registered(s.url)).applyCredentialSwitch("run-1", REQ);
        assert.deepEqual({ ...got, ...(got.kind === "uncertain" ? { detail: undefined } : {}) },
          { ...want, ...(want.kind === "uncertain" ? { detail: undefined } : {}) });
      } finally { await s.close(); }
    });
  }

  it("trims the applied token once, so every sink sees the value the env gets, and checks the length after trimming", async () => {
    const padded = json(200, { outcome: "applied", token: `  ${SECRET}\n`, ordinal: 1, message_seq: 5, replay: false });
    const shortPadded = json(200, { outcome: "applied", token: "  short  ", ordinal: 1, message_seq: 5, replay: false });
    for (const [reply, want] of [[padded, "applied"], [shortPadded, "uncertain"]] as const) {
      const s = await server(() => reply);
      try {
        const got = await (await registered(s.url)).applyCredentialSwitch("run-1", REQ);
        assert.equal(got.kind, want);
        if (got.kind === "applied") assert.equal(got.token, SECRET);
      } finally { await s.close(); }
    }
  });

  it("sends the documented request body to the documented path", async () => {
    const s = await server(() => json(200, { outcome: "superseded" }));
    try {
      await (await registered(s.url)).applyCredentialSwitch("run-1", { ...REQ, oldCumulative: { turns: 3 } });
      assert.equal(s.posts[0]!.path, "/api/worker/runs/run-1/credential-switch/apply");
      assert.deepEqual(s.posts[0]!.body, { claim_generation: 7, register_nonce: "nonce-1", op_id: "op-1", old_cumulative: { turns: 3 } });
    } finally { await s.close(); }
  });

  it("transport error is uncertain and does not echo anything", async () => {
    const s = await server(() => json(200, {}));
    const c = await registered(s.url);
    await s.close();
    const got = await c.applyCredentialSwitch("run-1", REQ);
    assert.equal(got.kind, "uncertain");
    assert.ok(!JSON.stringify(got).includes(SECRET));
  });

  it("never leaks the token or body through detail or thrown errors", async () => {
    for (const body of [
      JSON.stringify({ outcome: "applied", token: SECRET, ordinal: 0, message_seq: 5 }),
      `garbage ${SECRET}`,
    ]) {
      const s = await server(() => ({ status: 200, body }));
      try {
        const got = await (await registered(s.url)).applyCredentialSwitch("run-1", REQ);
        assert.equal(got.kind, "uncertain");
        assert.ok(!JSON.stringify(got).includes(SECRET));
      } finally { await s.close(); }
    }
    const s = await server(() => json(200, {}));
    try {
      const c = await registered(s.url, []);
      await assert.rejects(c.applyCredentialSwitch("run-1", REQ), (e: Error) => !e.message.includes(SECRET));
    } finally { await s.close(); }
  });

  it("is a programming error without the capability or without the feature", async () => {
    const s = await server(() => json(200, { outcome: "superseded" }));
    try {
      const noCap = await registered(s.url, ["credential_switch_v1"]);
      assert.equal(noCap.supportsInPlaceCredentialSwitch(), false);
      await assert.rejects(noCap.applyCredentialSwitch("run-1", REQ), /supportsInPlaceCredentialSwitch/);
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
    const s2 = await server(() => json(200, {}), { worker_id: "w", register_nonce: "n", protocol_features: ["credential_switch_hold"] });
    try {
      const noFeature = await registered(s2.url);
      assert.equal(noFeature.supportsInPlaceCredentialSwitch(), false);
      await assert.rejects(noFeature.applyCredentialSwitch("run-1", REQ), /requires/);
    } finally { await s2.close(); }
    const s3 = await server(() => json(200, {}));
    try { assert.equal((await registered(s3.url)).supportsInPlaceCredentialSwitch(), true); } finally { await s3.close(); }
  });
});

describe("credential switch signal op_id", () => {
  it("parses op_id from /inputs and keeps the v1 shape when absent", async () => {
    for (const [cs, want] of [
      [{ generation: 4, op_id: "op-9" }, { generation: 4, op_id: "op-9" }],
      [{ generation: 4 }, { generation: 4 }],
      [{ generation: 4, op_id: 12 }, { generation: 4 }],
    ] as const) {
      const s = await server(() => json(200, { inputs: [], credential_switch: cs }));
      try {
        const got = await (await registered(s.url)).getInputs("run-1", 4);
        assert.deepEqual(got.credentialSwitch, want);
      } finally { await s.close(); }
    }
  });

  it("parses op_id from the /state ack", async () => {
    const s = await server(() => json(200, { run: { status: "running" }, credential_switch: { generation: 4, op_id: "op-9" } }));
    try {
      const ack = await (await registered(s.url)).reportState("run-1", { status: "running", claim_generation: 4 });
      assert.deepEqual(ack.credentialSwitch, { generation: 4, op_id: "op-9" });
    } finally { await s.close(); }
  });
});

describe("credential switch state reports carry op_id", () => {
  const report = (status: string, extra: Record<string, unknown> = {}): PlanCrossCheckStateRequest =>
    ({ status, claim_generation: 7, ...extra }) as unknown as PlanCrossCheckStateRequest;

  for (const status of ["credential_switch_failed", "credential_switch_held", "credential_switch_uncertain"]) {
    it(`${status} sends op_id`, async () => {
      const s = await server(() => json(200, { run: { status: "running" }, disposition: status === "credential_switch_held" ? "held" : undefined }));
      try {
        await (await registered(s.url)).reportState("run-1", report(status, { op_id: "op-1" }));
        assert.equal(s.posts.length, 1);
        assert.equal(s.posts[0]!.body.op_id, "op-1");
        assert.equal(s.posts[0]!.body.status, status);
        if (status !== "credential_switch_failed") assert.equal(s.posts[0]!.body.register_nonce, "nonce-1");
      } finally { await s.close(); }
    });
  }

  it("failed and held without op_id keep the v1 body", async () => {
    for (const status of ["credential_switch_failed", "credential_switch_held"]) {
      const s = await server(() => json(200, { run: { status: "running" } }));
      try {
        await (await registered(s.url)).reportState("run-1", report(status));
        assert.equal("op_id" in s.posts[0]!.body, false);
      } finally { await s.close(); }
    }
  });

  it("strips op_id from failed when the apply feature is not negotiated", async () => {
    const s = await server(() => json(200, { run: { status: "running" } }),
      { worker_id: "w", register_nonce: "n", protocol_features: ["claim_generation_fence"] });
    try {
      await (await registered(s.url)).reportState("run-1", report("credential_switch_failed", { op_id: "op-1" }));
      assert.equal("op_id" in s.posts[0]!.body, false);
    } finally { await s.close(); }
  });

  it("refuses an uncertain report client-side without nonce, op_id, generation or feature", async () => {
    const s = await server(() => json(200, { run: { status: "running" } }));
    try {
      const c = await registered(s.url);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain")), /requires/);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain", { op_id: "" })), /requires/);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1", claim_generation: 0 })), /requires/);
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
    const noNonce = await server(() => json(200, { run: { status: "running" } }), { worker_id: "w", protocol_features: FEATURES });
    try {
      await assert.rejects((await registered(noNonce.url)).reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1" })), /requires/);
      assert.equal(noNonce.posts.length, 0);
    } finally { await noNonce.close(); }
    const noFeature = await server(() => json(200, { run: { status: "running" } }), { worker_id: "w", register_nonce: "n", protocol_features: [] });
    try {
      await assert.rejects((await registered(noFeature.url)).reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1" })), /requires/);
      assert.equal(noFeature.posts.length, 0);
    } finally { await noFeature.close(); }
  });

  it("refuses op_id on an unrelated state", async () => {
    const s = await server(() => json(200, { run: { status: "running" } }));
    try {
      await assert.rejects((await registered(s.url)).reportState("run-1", report("running", { op_id: "op-1" })), /op_id/);
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
  });
});
