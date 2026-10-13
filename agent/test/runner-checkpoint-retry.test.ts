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

async function exercise(opts: {
  responses?: unknown[]; abort?: "backoff" | "ownership" | "guard";
  refusal?: "ownership" | "scratch" | "remediation"; exit?: number; throws?: boolean; packThrows?: boolean;
  /** Omit for the production 250 ms then 1000 ms backoff; the default skips real waiting. */
  backoffMs?: readonly [number, number] | "production";
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
      return { pack: Readable.from(["exact-pack"]), exited: Promise.resolve(opts.exit ?? 0),
        cancel: () => { cancels++; events.push("settled"); } };
    };
    return { tipOid: TIP, ...await replay(), replay };
  };
  client.publishCheckpoint = async (_run, tip, pack, signal) => {
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
  const outcome = await seam.publishCheckpointOutcome(flight, "bare", "branch", undefined, abort.signal);
  return { outcome, flight, feed, requests, producers, cancels, streams, bytes, events, retryLogs };
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
