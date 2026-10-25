import { it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";
import { RunRunner } from "../src/runner.js";
import { GitCache, ScratchPublicationError } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { makeTextRedactor } from "../src/redact.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40);
const secret = "glpat-" + "1234567890abcdefghij";
const redact = makeTextRedactor([secret]);
const label = (phase: string, cause: string) =>
  `checkpoint publish failed: error (phase=${phase}, cause=${cause})`;

// Exercise the actual catch, dedupe, confirmation and producer settlement paths.
function harness(redactor: ((s: string) => string) | undefined = redact) {
  const logs: Array<{ msg: string; fields: Record<string, unknown> }> = [];
  const feed: Array<{ payload: { text: string } }> = [];
  const git = new GitCache("/unused", nullLogger());
  const client = new WorkerClient("http://unused", "test", "test", nullLogger());
  const runner = new RunRunner(client, git, () => ({ executor: { run: async () => ({ branch: "branch" }) } }), nullLogger(), 1);
  const seam = runner as unknown as {
    requireTrackingOwned: () => Promise<string>;
    checkpointRemediationHold: () => Promise<boolean>;
    publishCheckpointOutcome: (flight: unknown, bare: string, branch: string, overlay?: undefined, signal?: AbortSignal) => Promise<unknown>;
  };
  seam.requireTrackingOwned = async () => TIP;
  seam.checkpointRemediationHold = async () => false;
  let error: unknown;
  let packing = false, success = false, requests = 0, cancels = 0, exits = 0;
  git.checkpointPack = async () => {
    if (packing) throw error;
    return { tipOid: TIP, pack: Readable.from(["pack"]),
      exited: Promise.resolve(0).then(code => { exits++; return code; }),
      cancel: () => { cancels++; }, replay: async () => { throw new Error("unexpected retry"); } };
  };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    requests++;
    for await (const _ of pack) { /* drain */ }
    if (success) return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/branch" } };
    throw error;
  };
  const flight = { runId: "diagnostics", runKind: "issue", lastCheckpointRefTip: "OLD",
    lastAttemptedCheckpointRefTip: undefined as string | undefined,
    reportedPublishOutcomes: new Set<string>(), redactText: redactor,
    runLog: { ...nullLogger(), warn: (msg: string, fields: Record<string, unknown> = {}) => logs.push({ msg, fields }) },
    batcher: { emit: (event: unknown) => feed.push(event as typeof feed[number]) } };
  return { logs, feed, flight, client,
    get requests() { return requests; }, get cancels() { return cancels; }, get exits() { return exits; },
    fail: (e: unknown, pack = false, signal?: AbortSignal) => {
      error = e; packing = pack; success = false;
      return seam.publishCheckpointOutcome(flight, "bare", "branch", undefined, signal);
    },
    confirm: () => {
      packing = false; success = true;
      return seam.publishCheckpointOutcome(flight, "bare", "branch");
    },
  };
}

for (const [code, expected] of [
  ["ENOTFOUND", "dns"], ["EAI_AGAIN", "dns"], ["ECONNREFUSED", "connection_refused"],
  ["ECONNRESET", "connection_reset"], ["ETIMEDOUT", "timeout"],
  ["UND_ERR_CONNECT_TIMEOUT", "timeout"], ["UND_ERR_HEADERS_TIMEOUT", "timeout"],
  ["UND_ERR_BODY_TIMEOUT", "timeout"],
]) {
  it(`nested fetch ${code} yields only the fixed label and key, without retries`, async () => {
    const h = harness();
    const remote = "https://user:password@example.invalid/\u001b[31mremote";
    assert.deepEqual(await h.fail(new TypeError(remote, { cause: Object.assign(new Error(remote), { code }) })),
      { published: false, reason: "error" });
    assert.deepEqual(h.feed.map(e => e.payload.text), [label("publish_request", expected!)]);
    assert.deepEqual([...h.flight.reportedPublishOutcomes], [`error:publish_request:${expected}`]);
    assert.equal(h.requests, 1);
    assert.equal(h.cancels, 1);
    assert.equal(h.exits, 1);
    assert.equal(h.flight.lastCheckpointRefTip, "OLD");
    assert.equal(h.flight.lastAttemptedCheckpointRefTip, TIP);
  });
}

it("uses the first recognized code and does not infer from text or aggregate collections", async () => {
  const h = harness();
  await h.fail({ code: "ENOSPC", message: "ENOTFOUND timeout out of memory",
    errors: [{ code: "ECONNRESET" }], cause: { code: "ECONNREFUSED", cause: { code: "ETIMEDOUT" } } });
  assert.equal(h.feed[0]!.payload.text, label("publish_request", "connection_refused"));
  await h.fail(new AggregateError([{ code: "ENOTFOUND" }], "ECONNRESET"));
  assert.equal(h.feed[1]!.payload.text, label("publish_request", "unknown"));
});

it("bounds traversal at four nodes including root, detects cycles, and accepts the fourth code", async () => {
  const h = harness();
  await h.fail({ cause: { cause: { cause: { cause: { code: "ENOTFOUND" } } } } });
  const cycle: { cause?: unknown; message: string } = { message: "cycle" };
  cycle.cause = cycle;
  await h.fail(cycle);
  assert.equal(h.logs.at(-1)!.fields.cause, undefined);
  await h.fail({ cause: { cause: { cause: { code: "ECONNRESET" } } } });
  assert.deepEqual(h.feed.map(e => e.payload.text), [label("publish_request", "unknown"), label("publish_request", "connection_reset")]);
});

it("ignores malformed fields and hostile diagnostic getters without throwing", async () => {
  const h = harness();
  for (const value of [undefined, null, 7, Symbol("bad"), "ETIMEDOUT", { code: ["ENOTFOUND"], message: {} }]) {
    assert.deepEqual(await h.fail(value), { published: false, reason: "error" });
  }
  const broken = { get code() { throw new Error("code"); }, get message() { throw new Error("message"); },
    cause: { code: "EAI_AGAIN", get message() { throw new Error("nested"); }, get cause() { throw new Error("cause"); } } };
  await h.fail(broken);
  assert.equal(h.feed.at(-1)!.payload.text, label("publish_request", "dns"));
  assert.equal(h.logs.at(-1)!.fields.error, undefined);
  assert.equal(h.logs.at(-1)!.fields.cause, undefined);
  await h.fail({ code: { toString() { throw new Error("never stringify"); } },
    message: { toString() { throw new Error("never stringify"); } }, get cause() { throw new Error("cause"); } });
});

it("dedupes by phase and class, logs every attempt, and rearms only on confirmation", async () => {
  const h = harness();
  const dns = { code: "ENOTFOUND" };
  await h.fail(dns, true);
  assert.equal(h.requests, 0);
  assert.equal(h.flight.lastAttemptedCheckpointRefTip, undefined);
  await h.fail(dns);
  await h.fail(dns);
  await h.fail({ code: "ETIMEDOUT" });
  assert.deepEqual([...h.flight.reportedPublishOutcomes],
    ["error:packing:dns", "error:publish_request:dns", "error:publish_request:timeout"]);
  assert.equal(h.feed.length, 3);
  assert.equal(h.logs.length, 4);
  assert.deepEqual(await h.confirm(), { published: true });
  assert.equal(h.flight.reportedPublishOutcomes.size, 0);
  assert.match(h.feed.at(-1)!.payload.text, /publishing recovered/);
  await h.fail(dns);
  assert.equal(h.feed.length, 5);
});

it("redacts whole messages before sanitizing and bounds each part and the joined causes in UTF-16", async () => {
  const h = harness();
  const split = secret.slice(0, 10) + "\u2028\u2029\n\u001b[31m" + secret.slice(10);
  const message = `remote ${split} ${"😀".repeat(140)}`;
  await h.fail(new Error(message, { cause: { message, cause: { message, cause: { message, cause: { message: "fifth" } } } } }));
  const fields = h.logs[0]!.fields;
  assert.equal(typeof fields.error, "string");
  assert.ok(String(fields.error).length <= 200);
  assert.ok(String(fields.cause).length <= 600);
  assert.equal(String(fields.cause).split(" <- ").length, 3);
  assert.match(String(fields.error), /REDACTED/);
  assert.doesNotMatch(JSON.stringify(fields), /1234567890|abcdefghij|fifth/);
  assert.ok([...String(fields.error)].every(c => c.codePointAt(0)! >= 32 && c !== "\u2028" && c !== "\u2029"));
  assert.deepEqual(h.feed.map(e => e.payload.text), [label("publish_request", "unknown")]);
});

it("omits oversized raw messages whole before redaction", async () => {
  const inputs: string[] = [];
  const h = harness(s => { inputs.push(s); return redact(s); });
  await h.fail({ message: secret + "x".repeat(4097), cause: { message: "prefix" + "x".repeat(4097) } });
  assert.equal(inputs.length, 2);
  assert.ok(inputs.every(s => s.length < 100 && s.includes("omitted")));
  assert.doesNotMatch(JSON.stringify(h.logs), /glpat|prefix/);
});

it("omits free text without a usable redactor or when it throws or returns malformed data", async () => {
  for (const redactor of [undefined, (() => { throw new Error("redactor failed"); }), (() => ({ unsafe: secret }))]) {
    const h = harness();
    h.flight.redactText = redactor as unknown as typeof h.flight.redactText;
    await h.fail({ message: secret, cause: { code: "ENOTFOUND", message: secret } });
    assert.equal(h.logs[0]!.fields.error, undefined);
    assert.equal(h.logs[0]!.fields.cause, undefined);
    assert.equal(h.feed[0]!.payload.text, label("publish_request", "dns"));
  }
});

it("retains fail-closed boundary/name getters, typed refusal and abort precedence, settling producers", async () => {
  const h = harness();
  for (const field of ["checkpointBoundary", "name"]) {
    const sentinel = new Error("getter refused");
    const error = new Error("original");
    Object.defineProperty(error, field, { get() { throw sentinel; } });
    await assert.rejects(h.fail(error), e => e === sentinel);
  }
  const boundary = new GitBoundaryAbortError("hard");
  await assert.rejects(h.fail(boundary), e => e === boundary);
  const refusal = new ScratchPublicationError("refused");
  assert.deepEqual(await h.fail(refusal), { published: false, reason: "scratch_publication_refused" });
  const abort = new Error("abort", { cause: { code: "ENOTFOUND" } });
  abort.name = "AbortError";
  assert.deepEqual(await h.fail(abort), { published: false, reason: "aborted" });
  assert.equal(h.cancels, 5);
  assert.equal(h.exits, 5);
  assert.equal(h.feed.some(e => e.payload.text.includes("cause=dns")), false);
});

it("real-client malformed ACK keeps its untrusted remote excerpt only in the bounded redacted log", async () => {
  const ackSecret = "ack-" + "key!";
  const h = harness(makeTextRedactor([secret, ackSecret]));
  const realClient = new WorkerClient("http://unused", "test", "test", nullLogger());
  h.client.publishCheckpoint = realClient.publishCheckpoint.bind(realClient);
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (_input, init) => {
    for await (const _ of init!.body as Readable) { /* drain actual upload */ }
    return new Response("R:" + ackSecret, { status: 200 });
  };
  try {
    assert.deepEqual(await h.fail(new Error("unused")), { published: false, reason: "error" });
  } finally { globalThis.fetch = originalFetch; }
  const fields = h.logs.at(-1)!.fields;
  assert.match(String(fields.error), /R:/);
  assert.match(String(fields.error), /REDACTED/);
  assert.doesNotMatch(String(fields.error), new RegExp(ackSecret));
  assert.doesNotMatch(String(fields.error), new RegExp(secret));
  assert.ok(String(fields.error).length <= 200);
  assert.equal(h.feed[0]!.payload.text, label("publish_request", "unknown"));
  assert.equal(h.flight.lastAttemptedCheckpointRefTip, TIP);
  assert.equal(h.flight.lastCheckpointRefTip, "OLD");
  assert.equal(h.cancels, 1);
});
