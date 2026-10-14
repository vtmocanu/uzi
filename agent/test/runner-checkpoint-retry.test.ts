import { it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";
import { GitCache, ScratchPublicationError } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40);
const ACK = { ok: true, body: { published: true } };
const FAIL = { ok: false, httpStatus: 500 };

/** Hand-driven response body for the real-client path: pulls on demand and records the source cancel hook. */
interface RealAck { stream: ReadableStream<Uint8Array>; cancelled: () => boolean }
function ackStream(bytes: Uint8Array): RealAck {
  let sent = false, cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    pull(c) { if (!sent) { sent = true; c.enqueue(bytes); } else c.close(); },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 });
  return { stream, cancelled: () => cancelled };
}

function start(opts: {
  responses?: unknown[]; abort?: "backoff" | "ownership" | "guard";
  refusal?: "ownership" | "scratch" | "remediation"; exit?: number; throws?: boolean; packThrows?: boolean;
  /** Omit for the production 250 ms then 1000 ms backoff; the default skips real waiting. */
  backoffMs?: readonly [number, number] | "production";
  /** Opt in: keep the real WorkerClient.publishCheckpoint and stub only globalThis.fetch with this ack body. */
  realClient?: { body: Uint8Array };
  /** Opt in: `exited` stays pending until the returned `resolveExit` is called. */
  deferredExit?: boolean;
} = {}) {
  const abort = new AbortController();
  const git = new GitCache("/unused", nullLogger());
  const client = new WorkerClient("http://unused", "test", "test", nullLogger());
  const backoff = opts.backoffMs ?? (opts.abort === "backoff" ? [60_000, 60_000] as const : [0, 0] as const);
  const hooks = backoff === "production" ? undefined : { retryBackoffMs: backoff };
  const runner = new RunRunner(client, git, () => ({ executor: { run: async () => ({ branch: "branch" }) } }), nullLogger(), 1,
    undefined, { checkpointTestHooks: hooks });
  const seam = runner as unknown as {
    requireTrackingOwned: () => Promise<string>;
    checkpointRemediationHold: () => Promise<boolean>;
    publishCheckpointOutcome: (flight: unknown, bare: string, branch: string, overlay: undefined, signal: AbortSignal) => Promise<unknown>;
  };
  let ownership = 0, guards = 0, requests = 0, producers = 0, cancels = 0;
  const streams: Readable[] = [];
  let resolveExit: (code: number) => void = () => undefined;
  const exited = opts.deferredExit
    ? new Promise<number>(resolve => { resolveExit = resolve; })
    : Promise.resolve(opts.exit ?? 0);
  let ack: RealAck | undefined;
  const bytes: string[] = [];
  const events: string[] = [];
  seam.requireTrackingOwned = async () => {
    if (++ownership === 2) {
      if (opts.abort === "ownership") abort.abort();
      if (opts.refusal === "ownership") throw new Error("ownership refused");
    }
    return TIP;
  };
  seam.checkpointRemediationHold = async () => {
    if (++guards === 3 && opts.abort === "guard") abort.abort();
    return guards === 3 && opts.refusal === "remediation";
  };
  git.checkpointPack = async (_bare, _branch, _overlay, _pinned, _step, _source, guard) => {
    const replay = async () => {
      if (opts.packThrows) throw new Error("local pack failure");
      if (producers && opts.refusal === "scratch") throw new ScratchPublicationError("refused");
      await guard?.(TIP);
      producers++;
      events.push("open");
      return { pack: Readable.from(["exact-pack"]), exited,
        cancel: () => { cancels++; events.push("settled"); } };
    };
    return { tipOid: TIP, ...await replay(), replay };
  };
  const realFetch = globalThis.fetch;
  if (opts.realClient) {
    const real = opts.realClient;
    globalThis.fetch = (async (_input: unknown, init?: RequestInit) => {
      requests++;
      const pack = init?.body as Readable;
      streams.push(pack);
      let body = "";
      for await (const chunk of pack) body += chunk;
      bytes.push(body);
      ack = ackStream(real.body);
      return new Response(ack.stream, { status: 200 });
    }) as typeof fetch;
  } else client.publishCheckpoint = async (_run, tip, pack, signal) => {
    assert.equal(tip, TIP);
    assert.equal(signal, abort.signal);
    requests++;
    streams.push(pack);
    let body = "";
    for await (const chunk of pack) body += chunk;
    bytes.push(body);
    if (opts.throws) throw new Error("transport failed");
    if (opts.abort === "backoff") setTimeout(() => abort.abort(), 10);
    return (opts.responses?.[requests - 1] ?? FAIL) as Awaited<ReturnType<typeof client.publishCheckpoint>>;
  };
  const feed: unknown[] = [];
  const retryLogs: Array<{ attempt: unknown; backoff_ms: unknown }> = [];
  const runLog = { ...nullLogger(), warn: (msg: string, fields?: Record<string, unknown>) => {
    if (msg === "checkpoint publish retry") retryLogs.push({ attempt: fields?.attempt, backoff_ms: fields?.backoff_ms });
  } };
  const flight = { runId: "retry", runKind: "issue", lastCheckpointRefTip: "OLD",
    lastAttemptedCheckpointRefTip: undefined as string | undefined,
    reportedPublishOutcomes: new Set<string>(), runLog,
    batcher: { emit: (value: unknown) => feed.push(value) } };
  let settled = false;
  const done = seam.publishCheckpointOutcome(flight, "bare", "branch", undefined, abort.signal)
    .finally(() => { settled = true; globalThis.fetch = realFetch; });
  return { done, flight, feed, streams, bytes, events, retryLogs, resolveExit,
    get settled() { return settled; }, get ackCancelled() { return ack?.cancelled() ?? false; },
    get requests() { return requests; }, get producers() { return producers; }, get cancels() { return cancels; } };
}

async function exercise(opts: Parameters<typeof start>[0] = {}) {
  const h = start(opts);
  const outcome = await h.done;
  return Object.assign(h, { outcome });
}

async function until(cond: () => boolean): Promise<void> {
  for (let i = 0; i < 200 && !cond(); i++) await new Promise(r => setImmediate(r));
  assert.ok(cond(), "condition was not reached");
}

it("500 then ACK uses fresh exact streams, settles the old producer, and recovers an empty ledger", async () => {
  const r = await exercise({ responses: [FAIL, ACK] });
  assert.deepEqual(r.outcome, { published: true });
  assert.equal(r.requests, 2);
  assert.notEqual(r.streams[0], r.streams[1]);
  assert.deepEqual(r.bytes, ["exact-pack", "exact-pack"]);
  assert.deepEqual(r.events, ["open", "settled", "open", "settled"]);
  assert.equal(r.flight.lastCheckpointRefTip, TIP);
  assert.equal(r.flight.lastAttemptedCheckpointRefTip, undefined);
  assert.match(JSON.stringify(r.feed), /publishing recovered/);
  assert.doesNotMatch(JSON.stringify(r.feed), /publish failed/);
});

it("persistent 500 is exactly three requests and one exhausted diagnostic", async () => {
  const r = await exercise();
  assert.equal(r.requests, 3);
  assert.equal(r.cancels, 3);
  assert.deepEqual(r.outcome, { published: false, reason: "rejected", httpStatus: 500 });
  assert.equal(r.feed.length, 1);
  assert.equal(r.flight.lastCheckpointRefTip, "OLD");
  assert.equal(r.flight.lastAttemptedCheckpointRefTip, TIP);
});

it("production backoff is 250 ms then 1000 ms when no test hook is set", async () => {
  const started = Date.now();
  const r = await exercise({ backoffMs: "production" });
  assert.equal(r.requests, 3);
  assert.deepEqual(r.retryLogs, [{ attempt: 1, backoff_ms: 250 }, { attempt: 2, backoff_ms: 1000 }]);
  assert.ok(Date.now() - started >= 1200, "the production delays were actually waited");
});
it("the test hook replaces the backoff the retry log reports", async () => {
  const r = await exercise({ backoffMs: [3, 5] });
  assert.deepEqual(r.retryLogs, [{ attempt: 1, backoff_ms: 3 }, { attempt: 2, backoff_ms: 5 }]);
});

for (const response of [{ ok: false, httpStatus: 400 }, { ok: false, httpStatus: 200 },
  { ok: false, httpStatus: "500" }, { ok: true, body: { published: false, skipped: "not_descendant" } }]) {
  it("permanent or nonnumeric response never retries: " + JSON.stringify(response), async () => {
    assert.equal((await exercise({ responses: [response] })).requests, 1);
  });
}
for (const abort of ["backoff", "ownership", "guard"] as const) {
  it("abort during " + abort + " creates no next producer or upload", async () => {
    const r = await exercise({ abort });
    assert.equal(r.requests, 1);
    assert.equal(r.producers, 1);
    assert.equal(r.cancels, 1);
    assert.deepEqual(r.outcome, { published: false, reason: "aborted" });
    assert.equal(r.flight.lastCheckpointRefTip, "OLD");
  });
}
for (const refusal of ["ownership", "scratch", "remediation"] as const) {
  it(refusal + " refusal prevents replay upload", async () => {
    const r = await exercise({ refusal });
    assert.equal(r.requests, 1);
    assert.equal(r.producers, 1);
  });
}
for (const response of [FAIL, ACK]) {
  it("known failed producer cannot retry or confirm: " + JSON.stringify(response), async () => {
    const r = await exercise({ exit: 47, responses: [response] });
    assert.equal(r.requests, 1);
    assert.deepEqual(r.outcome, { published: false, reason: "error" });
    assert.equal(r.flight.lastCheckpointRefTip, "OLD");
    assert.equal(r.flight.lastAttemptedCheckpointRefTip, TIP);
  });
}
it("local pack throw never uploads or retries", async () => {
  const r = await exercise({ packThrows: true });
  assert.equal(r.requests, 0);
  assert.equal(r.producers, 0);
  assert.equal(r.flight.lastAttemptedCheckpointRefTip, undefined);
});
it("transport throw is ambiguous and never retried", async () => {
  const r = await exercise({ throws: true });
  assert.equal(r.requests, 1);
  assert.equal(r.flight.lastAttemptedCheckpointRefTip, TIP);
});

const ACK_CAP = 64 * 1024;
const enc = new TextEncoder();
const ackBytes = (n: number, extra = "") => {
  const head = `{"published":true${extra}}`;
  return enc.encode(head + " ".repeat(n - enc.encode(head).length));
};

it("over-cap ack with the real client stays unsettled until the producer exits, then is an ambiguous error", async () => {
  const marker = "ACK-BODY-MARKER";
  const h = start({ realClient: { body: ackBytes(ACK_CAP + 1, `,"m":"${marker}"`) }, deferredExit: true });
  await until(() => h.ackCancelled);
  await new Promise(r => setImmediate(r));
  assert.equal(h.settled, false);
  h.resolveExit(0);
  const outcome = await h.done;
  assert.deepEqual(outcome, { published: false, reason: "error" });
  assert.equal(h.flight.lastCheckpointRefTip, "OLD");
  assert.equal(h.flight.lastAttemptedCheckpointRefTip, TIP);
  assert.equal(h.requests, 1);
  const feed = JSON.stringify(h.feed);
  assert.doesNotMatch(feed, new RegExp(marker));
  assert.match(feed, /checkpoint publish failed: error/);
});

it("normal ack with the real client confirms only after the producer exits 0", async () => {
  const h = start({ realClient: { body: ackBytes(100) }, deferredExit: true });
  await until(() => h.requests === 1);
  await new Promise(r => setTimeout(r, 20));
  assert.equal(h.settled, false);
  assert.equal(h.flight.lastCheckpointRefTip, "OLD");
  h.resolveExit(0);
  assert.deepEqual(await h.done, { published: true });
  assert.equal(h.flight.lastCheckpointRefTip, TIP);
});

it("normal ack with the real client but a failing producer is not confirmed", async () => {
  const h = start({ realClient: { body: ackBytes(100) }, deferredExit: true });
  await until(() => h.requests === 1);
  h.resolveExit(1);
  assert.deepEqual(await h.done, { published: false, reason: "error" });
  assert.equal(h.flight.lastCheckpointRefTip, "OLD");
  assert.equal(h.flight.lastAttemptedCheckpointRefTip, TIP);
});
