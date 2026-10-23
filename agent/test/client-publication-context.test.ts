import assert from "node:assert/strict";
import { afterEach, beforeEach, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { PublicationContextLatch } from "../src/publication-context.js";
import type { PublicationContextRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const FEATURE = "refresh_publication_context_v1";
const RUN = "11111111-1111-4111-8111-111111111111";
const SOURCE = "22222222-2222-4222-8222-222222222222";
const HEAD = "a".repeat(40);
const request = (): PublicationContextRequest => ({ claim_generation: 3, branch: "agent/issue-1", candidate_head: HEAD });
const resolved = (intent: "closing" | "non_closing" = "non_closing") => ({
  version: 1, status: "resolved",
  binding: { run_id: RUN, ...request(), mr_iid: 7, source_run_id: SOURCE, issue_iid: 42, intent,
    source_publication: { version: 1, claim_generation: 1, branch: request().branch, mr_iid: 7,
      published_head: "b".repeat(40), intent: "closing", reported_non_closing: false } },
});
let client: WorkerClient;
let features: string[];
let sent: { url: string; body: unknown }[];
let respond: () => Response;
beforeEach(() => {
  features = [FEATURE];
  sent = [];
  respond = () => Response.json(resolved());
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    if (String(input).endsWith("/register")) return Response.json({ worker_id: "worker", protocol_features: features });
    sent.push({ url: String(input), body: JSON.parse(String(init?.body)) });
    return respond();
  });
  client = new WorkerClient("http://publication.test", "fixture-worker", "test", nullLogger());
});
afterEach(() => mock.restoreAll());
async function query() {
  await client.register("worker");
  return client.publicationContext(RUN, request());
}
it("sends the exact worker request without generation capability and accepts a distinct historical SHA", async () => {
  assert.deepEqual(await query(), resolved());
  assert.deepEqual(sent, [{ url: "http://publication.test/api/worker/runs/" + RUN + "/publication-context", body: request() }]);
});
it("accepts a historical null stamp only as affirmative non-closing evidence", async () => {
  const raw = resolved() as any;
  raw.binding.source_publication = null;
  respond = () => Response.json(raw);
  assert.deepEqual(await query(), raw);
  raw.binding.intent = "closing";
  await assert.rejects(client.publicationContext(RUN, request()), /publication context/);
});
it("accepts positive not-applicable with null issue and null stamp", async () => {
  const raw = resolved() as any;
  raw.status = "not_applicable";
  raw.binding.intent = "not_applicable";
  raw.binding.issue_iid = null;
  raw.binding.source_publication = null;
  respond = () => Response.json(raw);
  assert.deepEqual(await query(), raw);
});
for (const reason of ["claim_changed", "ancestry_unknown", "association_unknown", "ambiguous_source", "source_unknown",
  "authority_unknown", "publication_unproven", "invalid_request", "forge_unknown", "forge_mismatch", "authority_changed"]) {
  it("accepts only the static unknown reason " + reason, async () => {
    const raw = { version: 1, status: "unknown", reason };
    respond = () => Response.json(raw);
    assert.deepEqual(await query(), raw);
  });
}
const invalidResponses: [string, (v: any) => void][] = [
  ["version", v => { v.version = 2; }],
  ["unknown status", v => { v.status = "permitted"; }],
  ["extra envelope key", v => { v.extra = true; }],
  ["extra binding key", v => { v.binding.extra = true; }],
  ["missing stamp", v => { delete v.binding.source_publication; }],
  ["missing issue", v => { delete v.binding.issue_iid; }],
  ["run mismatch", v => { v.binding.run_id = SOURCE; }],
  ["generation mismatch", v => { v.binding.claim_generation = 4; }],
  ["branch mismatch", v => { v.binding.branch = "other"; }],
  ["candidate mismatch", v => { v.binding.candidate_head = "c".repeat(40); }],
  ["unsafe MR", v => { v.binding.mr_iid = Number.MAX_SAFE_INTEGER + 1; }],
  ["fractional MR", v => { v.binding.mr_iid = 1.5; }],
  ["zero issue", v => { v.binding.issue_iid = 0; }],
  ["string issue", v => { v.binding.issue_iid = "42"; }],
  ["bad source UUID", v => { v.binding.source_run_id = "source"; }],
  ["nil source UUID", v => { v.binding.source_run_id = "00000000-0000-0000-0000-000000000000"; }],
  ["self source", v => { v.binding.source_run_id = RUN; }],
  ["source version", v => { v.binding.source_publication.version = 2; }],
  ["source generation", v => { v.binding.source_publication.claim_generation = 0; }],
  ["source extra key", v => { v.binding.source_publication.extra = true; }],
  ["source branch mismatch", v => { v.binding.source_publication.branch = "other"; }],
  ["source MR mismatch", v => { v.binding.source_publication.mr_iid = 8; }],
  ["source SHA", v => { v.binding.source_publication.published_head = "b".repeat(39); }],
  ["source intent", v => { v.binding.source_publication.intent = "not_applicable"; }],
  ["reported type", v => { v.binding.source_publication.reported_non_closing = 1; }],
  ["reported coherence", v => { v.binding.source_publication.reported_non_closing = true; }],
  ["closing coherence", v => { v.binding.intent = "closing"; v.binding.source_publication.intent = "non_closing"; }],
  ["resolved not-applicable", v => { v.binding.intent = "not_applicable"; }],
  ["not-applicable issue", v => { v.status = "not_applicable"; v.binding.intent = "not_applicable"; v.binding.source_publication = null; }],
  ["not-applicable stamp", v => { v.status = "not_applicable"; v.binding.intent = "not_applicable"; v.binding.issue_iid = null; }],
];
for (const [name, mutate] of invalidResponses) {
  it("rejects invalid response: " + name, async () => {
    const raw = resolved();
    mutate(raw);
    respond = () => Response.json(raw);
    await assert.rejects(query(), /publication context/);
    assert.equal(sent.length, 1);
  });
}
for (const raw of [
  null, [], {}, { version: 1, status: "unknown", reason: "server prose" },
  { version: 1, status: "unknown", reason: "claim_changed", binding: {} },
  { version: 1, status: "unknown" },
]) {
  it("rejects malformed envelope " + JSON.stringify(raw), async () => {
    respond = () => Response.json(raw);
    await assert.rejects(query(), /publication context/);
  });
}
for (const patch of [
  { claim_generation: 0 }, { claim_generation: 1.5 }, { claim_generation: Number.MAX_SAFE_INTEGER + 1 },
  { branch: "" }, { branch: "x".repeat(1025) }, { branch: "é".repeat(513) },
  { branch: "unsafe\u202ebranch" }, { branch: "unsafe\nbranch" },
  { candidate_head: "a".repeat(39) }, { candidate_head: "z".repeat(40) }, { extra: true },
]) {
  it("rejects invalid request locally " + JSON.stringify(patch), async () => {
    await client.register("worker");
    await assert.rejects(client.publicationContext(RUN, { ...request(), ...patch }), /publication context/);
    assert.equal(sent.length, 0);
  });
}
it("accepts the maximum safe integer and 1024-byte safe branch", async () => {
  await client.register("worker");
  const req = { ...request(), claim_generation: Number.MAX_SAFE_INTEGER, branch: "é".repeat(512) };
  const raw = resolved() as any;
  Object.assign(raw.binding, req);
  raw.binding.source_publication.branch = req.branch;
  raw.binding.mr_iid = Number.MAX_SAFE_INTEGER;
  raw.binding.source_publication.mr_iid = Number.MAX_SAFE_INTEGER;
  respond = () => Response.json(raw);
  assert.deepEqual(await client.publicationContext(RUN, req), raw);
  assert.ok(Buffer.byteLength(JSON.stringify(sent[0]!.body)) <= 4096);
});
it("fails closed before send when feature disappears", async () => {
  await client.register("worker");
  features = [];
  await client.register("worker");
  await assert.rejects(client.publicationContext(RUN, request()), /publication context/);
  assert.equal(sent.length, 0);
});
it("does not retry network failure or strip request fields", async () => {
  respond = () => { throw new Error("untrusted transport prose"); };
  await assert.rejects(query(), { message: "publication context unavailable" });
  assert.deepEqual(sent.map(s => s.body), [request()]);
});
it("rejects malformed JSON without including its prose", async () => {
  respond = () => new Response("untrusted response prose");
  await assert.rejects(query(), { message: "publication context unavailable" });
});
function streamed(text: string, chunkSize: number, headers?: Record<string, string>): Response {
  const bytes = new TextEncoder().encode(text);
  let offset = 0;
  return new Response(new ReadableStream({
    pull(controller) {
      if (offset >= bytes.length) { controller.close(); return; }
      controller.enqueue(bytes.slice(offset, offset += chunkSize));
    },
  }), { headers });
}
for (const chunkSize of [257, 20000]) {
  it("enforces actual 16KiB streamed cap with chunk size " + chunkSize, async () => {
    const json = JSON.stringify(resolved());
    const exact = json + " ".repeat(16384 - Buffer.byteLength(json));
    respond = () => streamed(exact, chunkSize);
    assert.deepEqual(await query(), resolved());
    respond = () => streamed(exact + " ", chunkSize);
    await assert.rejects(client.publicationContext(RUN, request()), /publication context/);
    // A false Content-Length must not bypass the actual-byte bound.
    respond = () => streamed(exact + " ", chunkSize, { "content-length": "1" });
    await assert.rejects(client.publicationContext(RUN, request()), /publication context/);
  });
}
it("latches restrictions while binding each newly requested candidate independently", async () => {
  await client.register("worker");
  const latch = new PublicationContextLatch();
  respond = () => Response.json(resolved("closing"));
  assert.equal(latch.observe(await client.publicationContext(RUN, request())), undefined);
  respond = () => Response.json(resolved());
  assert.equal(latch.observe(await client.publicationContext(RUN, request())), 42);
  const next = { ...request(), candidate_head: "c".repeat(40) };
  const raw = resolved("closing");
  raw.binding.candidate_head = next.candidate_head;
  respond = () => Response.json(raw);
  assert.equal(latch.observe(await client.publicationContext(RUN, next)), 42);
  raw.binding.source_run_id = "33333333-3333-4333-8333-333333333333";
  assert.throws(() => latch.observe({ ...raw, version: 1, status: "resolved" } as any), /identity changed/);
  assert.throws(() => latch.observe({ version: 1, status: "unknown", reason: "claim_changed" }), /unavailable/);
});
