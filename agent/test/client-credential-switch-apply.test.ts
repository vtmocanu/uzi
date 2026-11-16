import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { ReportPreconditionError, WorkerClient, type PlanCrossCheckStateRequest } from "../src/client.js";

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
  it("an already aborted apply never sends and remains uncertain", async () => {
    const s = await server(() => json(200, { outcome: "superseded" }));
    try {
      const c = await registered(s.url);
      const got = await c.applyCredentialSwitch("run-1", REQ, AbortSignal.abort());
      assert.equal(got.kind, "uncertain");
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
  });

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
      [{ generation: 4, op_id: "op", target_secret_id: "target" }, { generation: 4, op_id: "op", target_secret_id: "target" }],
      [{ generation: 4, target_secret_id: 12 }, { generation: 4 }],
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
    const s = await server(() => json(200, { run: { status: "running" }, credential_switch: { generation: 4, op_id: "op-9", target_secret_id: "target" } }));
    try {
      const ack = await (await registered(s.url)).reportState("run-1", { status: "running", claim_generation: 4 });
      assert.deepEqual(ack.credentialSwitch, { generation: 4, op_id: "op-9", target_secret_id: "target" });
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
        assert.equal(s.posts[0]!.body.register_nonce, "nonce-1");
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
      assert.equal("register_nonce" in s.posts[0]!.body, false);
    } finally { await s.close(); }
  });

  it("the v1 failed body (no op_id) carries no register_nonce", async () => {
    const s = await server(() => json(200, { run: { status: "running" } }));
    try {
      await (await registered(s.url)).reportState("run-1", report("credential_switch_failed"));
      assert.equal("register_nonce" in s.posts[0]!.body, false);
    } finally { await s.close(); }
  });

  it("refuses an op-carrying failed report client-side when there is no registration nonce", async () => {
    const noNonce = await server(() => json(200, { run: { status: "running" } }), { worker_id: "w", protocol_features: FEATURES });
    try {
      await assert.rejects((await registered(noNonce.url)).reportState("run-1", report("credential_switch_failed", { op_id: "op-1" })),
        (e: unknown) => e instanceof ReportPreconditionError && /nonce/.test(e.message));
      assert.equal(noNonce.posts.length, 0);
    } finally { await noNonce.close(); }
  });

  // Issue #4201: the client's local refusals are a typed ReportPreconditionError, which the runner's in-place report
  // loops treat as non-transient (a plain Error would be retried as a transport failure).
  const isPrecondition = (e: unknown): boolean => e instanceof ReportPreconditionError && /requires/.test(e.message);

  it("refuses an uncertain report client-side without nonce, op_id, generation or feature", async () => {
    const s = await server(() => json(200, { run: { status: "running" } }));
    try {
      const c = await registered(s.url);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain")), isPrecondition);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain", { op_id: "" })), isPrecondition);
      await assert.rejects(c.reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1", claim_generation: 0 })), isPrecondition);
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
    const noNonce = await server(() => json(200, { run: { status: "running" } }), { worker_id: "w", protocol_features: FEATURES });
    try {
      await assert.rejects((await registered(noNonce.url)).reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1" })), isPrecondition);
      assert.equal(noNonce.posts.length, 0);
    } finally { await noNonce.close(); }
    const noFeature = await server(() => json(200, { run: { status: "running" } }), { worker_id: "w", register_nonce: "n", protocol_features: [] });
    try {
      await assert.rejects((await registered(noFeature.url)).reportState("run-1", report("credential_switch_uncertain", { op_id: "op-1" })), isPrecondition);
      // The hold report's own precondition throw site (no credential_switch_hold feature) is typed too.
      await assert.rejects((await registered(noFeature.url)).reportState("run-1", report("credential_switch_held", { op_id: "op-1" })), isPrecondition);
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

describe("Codex switch wire contract", () => {
  const codexRegistered = (url: string) => registered(url, ["credential_switch_in_place_v1", "codex_credential_switch_v1"]);
  const features = [...FEATURES, "codex_credential_switch_apply_v1"];
  const registration = { worker_id: "w", register_nonce: "nonce-1", protocol_features: features };
  const apiKey = { auth_mode: "api_key", access_token: SECRET, capability: "epoch.secret" };
  const subscription = { ...apiKey, auth_mode: "subscription", generation: 0, chatgpt_account_id: "account", chatgpt_plan_type: null };
  const applied = (codex: unknown) => ({ outcome: "applied", codex, capability_epoch: 9, ordinal: 3, message_seq: 42, replay: false });

  for (const auth of [apiKey, subscription]) {
    it(`decodes complete ${auth.auth_mode} auth and current epoch coordinates`, async () => {
      const s = await server(() => json(200, applied(auth)), registration);
      try {
        const got = await (await codexRegistered(s.url)).applyCredentialSwitch("run-1", REQ);
        assert.deepEqual(got, { kind: "codex_applied", codex: auth, capabilityEpoch: 9, ordinal: 3, messageSeq: 42, replay: false });
        assert.equal(s.posts.length, 1);
      } finally { await s.close(); }
    });
  }

  it("rejects partial, mixed, invalid and unnegotiated auth without retaining secrets", async () => {
    const invalid = [
      applied({ ...apiKey, capability: "" }),
      applied({ ...apiKey, access_token: " " }),
      applied({ ...subscription, generation: -1 }),
      applied({ ...subscription, chatgpt_account_id: "" }),
      applied({ ...subscription, chatgpt_plan_type: "pro" }),
      applied({ ...apiKey, generation: 0 }),
      applied({ ...subscription, generation: undefined }),
      { ...applied(apiKey), capability_epoch: 0 },
      { ...applied(apiKey), ordinal: 0 },
      { ...applied(apiKey), message_seq: 0 },
      { ...applied(apiKey), replay: undefined },
      { ...applied(apiKey), token: SECRET },
    ];
    for (const body of invalid) {
      const s = await server(() => json(200, body), registration);
      try {
        const got = await (await codexRegistered(s.url)).applyCredentialSwitch("run-1", REQ);
        assert.equal(got.kind, "uncertain");
        assert.ok(!JSON.stringify(got).includes(SECRET));
        assert.equal(s.posts.length, 1);
      } finally { await s.close(); }
    }
    const s = await server(() => json(200, applied(apiKey)));
    try { assert.equal((await (await codexRegistered(s.url)).applyCredentialSwitch("run-1", REQ)).kind, "uncertain"); }
    finally { await s.close(); }
  });

  it("classifies a truncated auth response as uncertain and does not retry", async () => {
    const s = await server(() => ({ status: 200, body: JSON.stringify(applied(subscription)).slice(0, -2) }), registration);
    try {
      const got = await (await codexRegistered(s.url)).applyCredentialSwitch("run-1", REQ);
      assert.equal(got.kind, "uncertain");
      assert.ok(!JSON.stringify(got).includes(SECRET));
      assert.equal(s.posts.length, 1);
    } finally { await s.close(); }
  });

  it("preflight bounds response-body reading with the transport signal", async () => {
    const s = await server(() => json(200, {}), registration);
    let finish!: () => void;
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const c = await codexRegistered(s.url);
      const body = new ReadableStream<Uint8Array>({
        start(controller) { finish = () => controller.close(); controller.enqueue(new TextEncoder().encode("{")); },
      });
      (c as unknown as { fetchRaw: () => Promise<Response> }).fetchRaw = async () => new Response(body);
      const cancel = new AbortController();
      const pending = c.preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }, cancel.signal);
      await new Promise<void>(resolve => setImmediate(resolve));
      cancel.abort();
      const result = await Promise.race([pending, new Promise<string>(resolve => {
        timer = setTimeout(() => resolve("body did not abort"), 100);
      })]);
      assert.deepEqual(result, { kind: "uncertain", detail: "preflight_unavailable" });
    } finally { clearTimeout(timer); try { finish(); } catch {} await s.close(); }
  });

  it("preflight sends explicit target and returns server current coordinates", async () => {
    const s = await server(() => json(200, { outcome: "ready", expected_epoch: 11, claim_generation: 8, register_nonce: "current", op_id: "current-op" }), registration);
    try {
      const got = await (await codexRegistered(s.url)).preflightCodexCredentialSwitch("run/1", { ...REQ, targetSecretId: "target" });
      assert.deepEqual(got, { kind: "ready", expectedEpoch: 11, claimGeneration: 8, registerNonce: "current", opId: "current-op" });
      assert.deepEqual(s.posts, [{ path: "/api/worker/runs/run%2F1/credential-switch/codex/preflight",
        body: { claim_generation: 7, register_nonce: "nonce-1", op_id: "op-1", target_secret_id: "target" } }]);
    } finally { await s.close(); }
  });

  it("cancels an in-flight preflight without later success or response secrets", async () => {
    let respond!: () => void;
    let arrived!: () => void;
    const received = new Promise<void>((resolve) => { arrived = resolve; });
    const srv = http.createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        if (req.url === "/api/worker/register") {
          res.end(JSON.stringify(registration));
          return;
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.flushHeaders();
        respond = () => res.end(JSON.stringify({ outcome: "ready", claim_generation: 7,
          register_nonce: "n", op_id: "op", token: SECRET }));
        arrived();
      });
    });
    await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
    try {
      const c = await codexRegistered(`http://127.0.0.1:${(srv.address() as AddressInfo).port}`);
      const cancel = new AbortController();
      const pending = c.preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }, cancel.signal);
      await received;
      cancel.abort(new Error(SECRET));
      const result = await pending;
      assert.deepEqual(result, { kind: "uncertain", detail: "preflight_unavailable" });
      respond();
      assert.deepEqual(await pending, result);
      assert.ok(!JSON.stringify(result).includes(SECRET));
    } finally {
      srv.closeAllConnections();
      await new Promise<void>((resolve) => srv.close(() => resolve()));
    }
  });

  it("strict preflight rejects auth and partial or invalid decisions without leaking", async () => {
    const base = { outcome: "ready", claim_generation: 7, register_nonce: "n", op_id: "op" };
    for (const body of [
      ...[undefined, null, 0, -1, 1.5, "11", Number.MAX_SAFE_INTEGER + 1].map(expected_epoch => ({ ...base, expected_epoch })),
      { ...base, expected_epoch: 11, token: SECRET },
      { ...base, codex: subscription }, { ...base, token: SECRET },
      { ...base, claim_generation: -1 }, { ...base, op_id: undefined },
      { ...base, outcome: "applied" }, { ...base, outcome: "not_applied", reason: "" },
    ]) {
      const s = await server(() => json(200, body), registration);
      try {
        const got = await (await codexRegistered(s.url)).preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" });
        assert.equal(got.kind, "uncertain");
        assert.ok(!JSON.stringify(got).includes(SECRET));
      } finally { await s.close(); }
    }
  });

  it("preflight requires the server feature and fails closed on response loss", async () => {
    const s = await server(() => json(200, {}));
    try {
      await assert.rejects((await codexRegistered(s.url)).preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }), /requires/);
      assert.equal(s.posts.length, 0);
    } finally { await s.close(); }
    const lost = await server(() => json(200, {}), registration);
    const c = await codexRegistered(lost.url);
    await lost.close();
    assert.deepEqual(await c.preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }),
      { kind: "uncertain", detail: "preflight_unavailable" });
  });

  it("preflight retains fenced zero coordinates and refusal reasons", async () => {
    for (const outcome of ["fenced", "superseded", "not_applied", "refused_committed"]) {
      const reason = outcome === "not_applied" || outcome === "refused_committed" ? { reason: "target_moved" } : {};
      const s = await server(() => json(200, { outcome, claim_generation: 0, register_nonce: "", op_id: "", ...reason }), registration);
      try {
        assert.deepEqual(await (await codexRegistered(s.url)).preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }),
          { kind: outcome, claimGeneration: 0, registerNonce: "", opId: "", ...reason });
      } finally { await s.close(); }
    }
  });
});

describe("independent Codex switch negotiation", () => {
  const cap = "codex_credential_switch_v1";
  const feature = "codex_credential_switch_apply_v1";
  for (const caps of [[], [cap], ["credential_switch_in_place_v1"], [cap, "credential_switch_in_place_v1"]]) {
    for (const features of [[], [feature], ["credential_switch_apply_v1"], [feature, "credential_switch_apply_v1"]]) {
      it(`negotiates independently caps=${caps} features=${features}`, async () => {
        const s = await server(() => json(200, { outcome: "superseded" }),
          { worker_id: "w", register_nonce: "nonce-1", protocol_features: features });
        try {
          const c = await registered(s.url, caps);
          const codex = caps.includes(cap) && features.includes(feature);
          const claude = caps.includes("credential_switch_in_place_v1") && features.includes("credential_switch_apply_v1");
          assert.equal(c.supportsInPlaceCodexCredentialSwitch(), codex);
          assert.equal(c.supportsInPlaceCredentialSwitch(), claude);
          if (codex) await c.preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" });
          else await assert.rejects(c.preflightCodexCredentialSwitch("r", { ...REQ, targetSecretId: "target" }), /requires/);
          if (codex || claude) assert.deepEqual(await c.applyCredentialSwitch("r", REQ), { kind: "superseded" });
          else await assert.rejects(c.applyCredentialSwitch("r", REQ), /requires/);
          assert.equal(s.posts.length, Number(codex) + Number(codex || claude));
          await c.register("t", undefined, undefined, undefined, []);
          assert.equal(c.supportsInPlaceCodexCredentialSwitch(), false);
          assert.equal(c.supportsInPlaceCredentialSwitch(), false);
        } finally { await s.close(); }
      });
    }
  }

  it("keeps the operation on a Codex hold report", async () => {
    const s = await server(() => json(200, { run: { status: "running" }, disposition: "held" }),
      { worker_id: "w", register_nonce: "nonce-1", protocol_features: [feature, "credential_switch_hold"] });
    try {
      await (await registered(s.url, [cap])).reportState("r", { status: "credential_switch_held", claim_generation: 7, op_id: "op-1" });
      assert.deepEqual(s.posts.map(p => p.body), [{ status: "credential_switch_held", claim_generation: 7, op_id: "op-1", register_nonce: "nonce-1" }]);
    } finally { await s.close(); }
  });

  for (const status of ["credential_switch_failed", "credential_switch_uncertain"]) {
    it(`${status} preserves exact Codex operation and nonce without Claude feature`, async () => {
      const s = await server(() => json(200, { run: { status: "running" } }),
        { worker_id: "w", register_nonce: "nonce-1", protocol_features: [feature] });
      try {
        const c = await registered(s.url, [cap]);
        await c.reportState("r", { status, claim_generation: 7, op_id: "op-1" } as PlanCrossCheckStateRequest);
        assert.deepEqual(s.posts.map(p => p.body), [{ status, claim_generation: 7, op_id: "op-1", register_nonce: "nonce-1" }]);
      } finally { await s.close(); }
    });
    it(`${status} never downgrades after strict-decode refusal`, async () => {
      const s = await server(() => json(400, { error: 'json: unknown field "op_id"' }),
        { worker_id: "w", register_nonce: "nonce-1", protocol_features: [feature] });
      try {
        const c = await registered(s.url, [cap]);
        await assert.rejects(c.reportState("r", { status, claim_generation: 7, op_id: "op-1" } as PlanCrossCheckStateRequest));
        assert.deepEqual(s.posts.map(p => p.body), [{ status, claim_generation: 7, op_id: "op-1", register_nonce: "nonce-1" }]);
      } finally { await s.close(); }
    });
    it(`${status} refuses invalid operation coordinates`, async () => {
      const s = await server(() => json(200, { run: { status: "running" } }),
        { worker_id: "w", register_nonce: "nonce-1", protocol_features: [feature] });
      try {
        const c = await registered(s.url, [cap]);
        for (const coords of [{ claim_generation: 0, op_id: "op-1" }, { claim_generation: 7, op_id: "" }]) {
          await assert.rejects(c.reportState("r", { status, ...coords } as PlanCrossCheckStateRequest), ReportPreconditionError);
        }
        assert.equal(s.posts.length, 0);
      } finally { await s.close(); }
    });
    it(`${status} refuses missing Codex registration nonce`, async () => {
      const s = await server(() => json(200, { run: { status: "running" } }),
        { worker_id: "w", protocol_features: [feature] });
      try {
        const c = await registered(s.url, [cap]);
        await assert.rejects(c.reportState("r", { status, claim_generation: 7, op_id: "op-1" } as PlanCrossCheckStateRequest), ReportPreconditionError);
        assert.equal(s.posts.length, 0);
      } finally { await s.close(); }
    });
  }
});

describe("Codex stopped settlement", () => {
  const tuple = Object.freeze({ claimGeneration: 7, registerNonce: "nonce-1", expectedEpoch: 11 });
  const ack = { applied: true, disposition: "codex_switch_stopped",
    run: { id: "run/1", status: "paused", claim_generation: 7, hold_reason: null } };
  const caps = ["codex_credential_switch_v1", "codex_switch_stopped_v1"];
  const features = ["codex_credential_switch_apply_v1", "codex_switch_stopped_v1"];
  const cases: Array<[string, Reply, string]> = [
    ["explicit minimal ack", json(200, ack), "confirmed"],
    ["legacy bounded full run ack", json(200, { ...ack, run: { ...ack.run, worker_id: "w", plan_md: "legacy plan" } }), "confirmed"],
    ["status only", json(200, { run: ack.run }), "unconfirmed"],
    ["not applied", json(200, { ...ack, applied: false }), "unconfirmed"],
    ["wrong disposition", json(200, { ...ack, disposition: "uncertain" }), "unconfirmed"],
    ["wrong id", json(200, { ...ack, run: { ...ack.run, id: "other" } }), "unconfirmed"],
    ["wrong generation", json(200, { ...ack, run: { ...ack.run, claim_generation: 8 } }), "unconfirmed"],
    ["held", json(200, { ...ack, run: { ...ack.run, hold_reason: "wall" } }), "unconfirmed"],
    ["missing hold", json(200, { ...ack, run: { id: "run/1", status: "paused", claim_generation: 7 } }), "unconfirmed"],
    ["ordinary conflict", json(409, ack), "unconfirmed"],
    ["unsupported", json(400, { error: "invalid request body" }), "unconfirmed"],
    ["malformed", { status: 200, body: "{" }, "unconfirmed"],
    ["oversize valid ack", json(200, { ...ack, run: { ...ack.run, plan_md: "p".repeat(16 * 1024) } }), "unconfirmed"],
    ["server error", json(500, ack), "unconfirmed"],
  ];
  for (const [name, reply, kind] of cases) it(name, async () => {
    const s = await server(() => reply);
    try {
      const c = await registered(s.url);
      assert.deepEqual(await c.settleCodexSwitchStopped("run/1", tuple), { kind });
      assert.deepEqual(s.posts, [{ path: "/api/worker/runs/run%2F1/state",
        body: { status: "codex_switch_stopped", claim_generation: 7, register_nonce: "nonce-1", expected_epoch: 11 } }]);
    } finally { await s.close(); }
  });

  for (const stoppedCap of [false, true]) for (const stoppedFeature of [false, true]) {
    it(`admission requires stopped capability=${stoppedCap} feature=${stoppedFeature}`, async () => {
      const s = await server(() => json(200, ack), { worker_id: "w", register_nonce: "nonce-1",
        protocol_features: ["codex_credential_switch_apply_v1", ...(stoppedFeature ? ["codex_switch_stopped_v1"] : [])] });
      try {
        const c = await registered(s.url, ["codex_credential_switch_v1", ...(stoppedCap ? ["codex_switch_stopped_v1"] : [])]);
        assert.equal(c.supportsInPlaceCodexCredentialSwitch(), true);
        assert.equal(c.supportsCodexSwitchStopped(), stoppedCap && stoppedFeature);
      } finally { await s.close(); }
    });
  }

  it("requires stopped negotiation for admission and preserves admitted settlement after withdrawal", async () => {
    const registration = { worker_id: "w", register_nonce: "nonce-1", protocol_features: features };
    const s = await server(() => json(200, ack), registration);
    try {
      const c = await registered(s.url, caps);
      assert.equal(c.supportsCodexSwitchStopped(), true);
      registration.protocol_features = [];
      await c.register("t", undefined, undefined, undefined, caps);
      assert.equal(c.supportsCodexSwitchStopped(), false);
      assert.deepEqual(await c.settleCodexSwitchStopped("run/1", tuple), { kind: "confirmed" });
      registration.register_nonce = "nonce-2";
      await c.register("t", undefined, undefined, undefined, caps);
      assert.deepEqual(await c.settleCodexSwitchStopped("run/1", tuple), { kind: "fenced" });
      assert.equal(s.posts.length, 1);
    } finally { await s.close(); }
  });

  it("fences a nonce change while the response is pending", async () => {
    const registration = { worker_id: "w", register_nonce: "nonce-1", protocol_features: features };
    let finish!: () => void;
    let arrived!: () => void;
    const received = new Promise<void>(resolve => { arrived = resolve; });
    const srv = http.createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        if (req.url === "/api/worker/register") { res.end(JSON.stringify(registration)); return; }
        finish = () => res.end(JSON.stringify(ack));
        arrived();
      });
    });
    await new Promise<void>(resolve => srv.listen(0, "127.0.0.1", resolve));
    try {
      const c = await registered(`http://127.0.0.1:${(srv.address() as AddressInfo).port}`, caps);
      const pending = c.settleCodexSwitchStopped("run/1", tuple);
      await received;
      registration.register_nonce = "nonce-2";
      await c.register("t", undefined, undefined, undefined, caps);
      finish();
      assert.deepEqual(await pending, { kind: "fenced" });
    } finally {
      srv.closeAllConnections();
      await new Promise<void>(resolve => srv.close(() => resolve()));
    }
  });
});
