import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import { RequestError, type WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RecoveryBundleResult } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type {
  ChatClaimResponse,
  ClaimResponse,
  RecoveryCaptureStatusResponse,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";
import {
  classifyUploadFailure,
  PERMANENT_UPLOAD_REASONS,
  RecoveryCoordinator,
  type RecoveryArchiveClient,
  type RecoveryBundleProducer,
  type RecoveryOutcome,
  type RecoveryRecord,
} from "../src/recovery.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { nullLogger } from "./helpers.js";

// issue #1995 M2 — a recovery bundle whose upload fails while the worker is ALIVE is re-driven by
// the heartbeat (no restart), and the capture/sweep/live/pin paths are race-safe: one producer, one
// stream, immutable bytes, no resurrection of a removed record, no overwrite of a newer state, and a
// typed (api reason, else status) disposition for every upload failure.

const TOKEN = "worker-join-token-abcdef0123456789";
const H = "1111111111111111111111111111111111111111";
const H_PRIME = "2222222222222222222222222222222222222222";
const RUN = "run-1";
const FIN = { bareDir: "forge.example+org+repo.git", defaultBranch: "main", finalizationPin: true } as const;
const BYTES = Buffer.from("deterministic-fake-bundle-bytes-\x00ÿ");
const sha256 = (b: Buffer): string => createHash("sha256").update(b).digest("hex");

// ── tiny async primitives ────────────────────────────────────────────────────────

interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
}
function deferred(): Deferred {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** A monotonic event counter a test can await ("the producer was entered"), never a sleep. */
class Counter {
  count = 0;
  private waiters: Array<{ n: number; resolve: () => void }> = [];
  inc(): void {
    this.count++;
    for (const w of this.waiters.filter((x) => this.count >= x.n)) w.resolve();
    this.waiters = this.waiters.filter((x) => this.count < x.n);
  }
  waitFor(n: number): Promise<void> {
    if (this.count >= n) return Promise.resolve();
    return new Promise<void>((resolve) => this.waiters.push({ n, resolve }));
  }
}

// ── fakes ────────────────────────────────────────────────────────────────────────

interface UploadCall {
  captureId: string;
  manifest: RecoveryUploadManifest;
  bytes: Buffer;
}

class FakeClient implements RecoveryArchiveClient {
  reserveCalls: RecoveryReserveRequest[] = [];
  uploadCalls: UploadCall[] = [];
  releaseCalls = 0;
  reserveEntered = new Counter();
  uploadEntered = new Counter();
  holdReserve: Promise<void> | undefined;
  reserveError: unknown;
  holdUpload: Promise<void> | undefined;
  /** When set, `holdUpload` only holds uploads of this run. */
  holdUploadRun: string | undefined;
  /** The run id of every upload attempt, in the order the streams opened. */
  uploadRuns: string[] = [];
  /** One entry is consumed per upload attempt; an absent entry means the upload is accepted. */
  uploadFailures: Array<{ error: unknown; before?: () => void }> = [];
  active = 0;
  maxActive = 0;
  serverCaptureId = "server-capture-1";

  async reserveRecoveryCapture(_runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push(req);
    this.reserveEntered.inc();
    if (this.holdReserve) await this.holdReserve;
    if (this.reserveError) throw this.reserveError;
    return { capture_id: this.serverCaptureId, state: "preparing" };
  }
  async getRecoveryCaptureStatus(_runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    this.active++;
    this.maxActive = Math.max(this.maxActive, this.active);
    this.uploadRuns.push(runId);
    this.uploadEntered.inc();
    try {
      if (this.holdUpload && (!this.holdUploadRun || this.holdUploadRun === runId)) await this.holdUpload;
      const chunks: Buffer[] = [];
      for await (const c of bundle) chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c as string));
      this.uploadCalls.push({ captureId, manifest, bytes: Buffer.concat(chunks) });
      const failure = this.uploadFailures.shift();
      if (failure) {
        failure.before?.();
        throw failure.error;
      }
      return { capture_id: captureId, state: "available", manifest_bound: true };
    } finally {
      this.active--;
    }
  }
  async releaseRecoveryCustody(runId: string): Promise<RecoveryReleaseResponse> {
    this.releaseCalls++;
    return { run_id: runId, released: true, holds_released: 1 };
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    return { run_id: runId, holds: [] };
  }
}

class FakeGit implements RecoveryBundleProducer {
  produceCalls = 0;
  entered = new Counter();
  hold: Promise<void> | undefined;

  async fetchDefaultTip(): Promise<string> {
    return "3333333333333333333333333333333333333333";
  }
  async produceRecoveryBundle(
    _barePath: string,
    opts: { sourceSha: string; outPath: string },
  ): Promise<RecoveryBundleResult> {
    this.produceCalls++;
    await fsp.writeFile(opts.outPath, BYTES);
    this.entered.inc();
    if (this.hold) await this.hold;
    return {
      bundlePath: opts.outPath,
      byteSize: BYTES.length,
      checksum: sha256(BYTES),
      chunkCount: 1,
      prerequisiteShas: [],
      sourceSha: opts.sourceSha,
      selfContained: true,
      alreadyPublished: false,
    };
  }
  async resolveRestartSource(): Promise<{ status: "unpublished"; barePath: string }> {
    return { status: "unpublished", barePath: "/bare" };
  }
}

const reqErr = (status: number, body = ""): RequestError => new RequestError("POST", "/archives/x/upload", status, body);
const typed = (status: number, reason: string): RequestError =>
  reqErr(status, JSON.stringify({ error: "msg", reason }));

// ── harness ──────────────────────────────────────────────────────────────────────

let root: string;
let clock: { t: number };
beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1995-live-"));
  clock = { t: 1_700_000_000_000 };
});
afterEach(async () => {
  await fsp.rm(root, { recursive: true, force: true }).catch(() => undefined);
});

function makeCoord(
  client: FakeClient,
  git: FakeGit,
  extra: Partial<ConstructorParameters<typeof RecoveryCoordinator>[0]> = {},
): RecoveryCoordinator {
  return new RecoveryCoordinator({
    client,
    git,
    log: nullLogger(),
    recoveryRoot: path.join(root, "recovery"),
    workerToken: TOKEN,
    now: () => clock.t,
    liveBackoffBaseMs: 1,
    liveBackoffCapMs: 5,
    ...extra,
  });
}

const pinRec = async (coord: RecoveryCoordinator, over: Partial<Parameters<RecoveryCoordinator["pin"]>[0]> = {}): Promise<RecoveryRecord> => {
  const rec = await coord.pin({ runId: RUN, sourceSha: H, kind: "issue", branch: "agent/issue-1", generation: 1, ...over });
  assert.ok(rec);
  return rec;
};
const cap = (coord: RecoveryCoordinator, record: RecoveryRecord, extra: { attemptedHeadSha?: string } = {}): Promise<RecoveryOutcome> =>
  coord.captureAndUpload({ record, barePath: "/bare", defaultBranch: "main", ...extra });
const current = async (coord: RecoveryCoordinator): Promise<RecoveryRecord> => {
  const all = await coord.inspect(RUN);
  assert.equal(all.length, 1);
  return all[0]!;
};
const live = (coord: RecoveryCoordinator, over: { authenticatedAtMs?: number; executing?: boolean } = {}): Promise<void> =>
  coord.resumeLive({
    isExecuting: () => over.executing === true,
    authenticatedAtMs: over.authenticatedAtMs ?? clock.t + 1_000_000,
  });
const runFiles = (): string[] => {
  try {
    return fs.readdirSync(path.join(root, "recovery", RUN));
  } catch {
    return [];
  }
};
const priv = (coord: RecoveryCoordinator) =>
  coord as unknown as { markFailure(r: RecoveryRecord, reason: string): Promise<RecoveryOutcome> };

/** A pinned record whose first upload failed transiently: the journaled bundle is on disk. */
async function failedFirstUpload(client: FakeClient, coord: RecoveryCoordinator, error: unknown = typed(503, "busy")) {
  client.uploadFailures.push({ error });
  const pinned = await pinRec(coord);
  const out = await cap(coord, pinned);
  assert.equal(out.state, "needs_action");
  const rec = await current(coord);
  assert.ok(rec.bundlePath && fs.existsSync(rec.bundlePath));
  return { pinned, rec, out };
}

// ── 1. the regression: a live worker re-drives a failed upload ───────────────────

describe("live recovery re-drive through the Worker heartbeat (issue #1995)", () => {
  it("uploads a failed bundle without a restart once the api accepts again", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord);
    assert.equal(rec.state, "needs_action");
    assert.equal(rec.reason, "capture_error");

    const outbox = new Outbox({
      root: path.join(root, "outbox"),
      log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024,
      retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    // Resolved by the worker's own heartbeat-driven pass once a pass finds the record journaled uploaded:
    // an observed event, so the test is not bound to a polling interval.
    const settled = deferred();
    const runner = {
      resumePendingRecoveries: async () => {},
      resumeLiveRecoveries: async (authenticatedAtMs: number, signal?: AbortSignal) => {
        await coord.resumeLive({ isExecuting: () => false, authenticatedAtMs, signal });
        if ((await current(coord)).state === "uploaded") settled.resolve();
      },
      execute: async () => {},
    } as unknown as RunRunner;
    const wclient = {
      register: async () => ({}),
      heartbeat: async () => {},
      reportState: async () => ({ applied: true, status: "completed" }),
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
      hasFeature: () => false,
      getMessageGaps: async () => ({ gaps: [] }),
      postMessages: async () => {},
    } as unknown as WorkerClient;
    const noop = { execute: async () => {} };
    // The coordinator's own clock is the wall clock here: the heartbeat's send time is Date.now().
    clock = { t: Date.now() };
    const worker = new Worker(
      {
        workerName: "w1",
        workerTemplate: "base",
        pollIntervalMs: 1,
        heartbeatIntervalMs: 5,
        chatPollMs: 1,
        chatSessions: 1,
        maxConcurrentRuns: 1,
        dockerWiring: {},
        dataDir: root,
        gapFillMax: 100,
        outboxTerminalMaxBytes: 1 << 20,
      } as unknown as Config,
      wclient,
      runner,
      noop as unknown as ChatRunner,
      noop as unknown as JudgeRunner,
      noop as unknown as ReviewRunner,
      nullLogger(),
      () => ({ ok: true, missing: [] }),
      outbox,
      new Map(),
      new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32),
    );
    const controller = new AbortController();
    const done = worker.run(controller.signal);
    try {
      let timer: NodeJS.Timeout | undefined;
      const timeout = new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error("no live re-upload settled within 60s")), 60_000);
      });
      try {
        await Promise.race([settled.promise, timeout]);
      } finally {
        clearTimeout(timer);
      }
      assert.equal((await current(coord)).state, "uploaded", "the bundle settled uploaded with no restart");
      assert.equal(client.uploadCalls.length, 2, "the failed attempt, then one accepted re-upload");
      assert.equal(client.reserveCalls.length, 1, "the server capture reserved by the first attempt is reused");
      assert.equal(git.produceCalls, 1, "the journaled bytes are re-uploaded, never re-produced");
      assert.ok(client.uploadCalls[1]!.bytes.equals(BYTES));
    } finally {
      controller.abort();
      await done.catch(() => undefined);
    }
  });
});

for (const phase of ["reserve-count", "upload"] as const) {
  it(`U1 legacy ${phase} quota survives capture live and restart until success`, async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    let coord = makeCoord(client, git);
    const refuse = () => {
      if (phase === "reserve-count") client.reserveError = typed(507, "quota");
      else client.uploadFailures.push({ error: typed(507, "quota") });
    };
    refuse();
    const pinned = await pinRec(coord, FIN);
    const outcome = await cap(coord, pinned);
    assert.equal(outcome.reason, "storage_quota_exceeded");
    const before = await current(coord);
    assert.equal(before.state, "needs_action");
    assert.equal(before.reason, "storage_quota_exceeded");
    assert.equal(before.serverCaptureId, phase === "reserve-count" ? undefined : client.serverCaptureId);
    const bytes = fs.readFileSync(before.bundlePath!);
    const assertRetained = async () => {
      assert.deepEqual(await current(coord), before, "refusals preserve every journal fact and server identity");
      assert.deepEqual(fs.readFileSync(before.bundlePath!), bytes);
      assert.equal(git.produceCalls, 1);
      assert.equal(client.releaseCalls, 0);
    };
    refuse();
    await live(coord);
    await assertRetained();
    const attempts = () => phase === "reserve-count" ? client.reserveCalls.length : client.uploadCalls.length;
    assert.equal(attempts(), 2, "quota is eligible for live retry");
    await live(coord);
    assert.equal(attempts(), 2, "an immediate retry is spaced");
    clock.t += 2;
    refuse();
    await live(coord);
    assert.equal(attempts(), 3);
    await assertRetained();
    clock.t += 3;
    await live(coord);
    assert.equal(attempts(), 3, "a second refusal doubles the delay");

    coord = makeCoord(client, git);
    refuse();
    await coord.resumePending();
    assert.equal(attempts(), 4, "restart retries the authenticated quota journal");
    await assertRetained();
    client.reserveError = undefined;
    await live(coord);
    const after = await current(coord);
    assert.equal(after.state, "uploaded");
    assert.equal(after.reason, undefined, "success clears the quota diagnostic");
    for (const key of ["captureId", "sourceSha", "bundlePath", "checksum", "byteSize", "chunkCount", "generation"] as const) {
      assert.equal(after[key], before[key], key);
    }
    assert.equal(after.serverCaptureId, client.serverCaptureId);
    assert.equal(fs.existsSync(after.bundlePath!), false, "successful legacy upload retires the local bytes");
    assert.equal(git.produceCalls, 1);
    assert.equal(client.releaseCalls, 0, "legacy upload does not release custody");
    assert.ok(client.reserveCalls.every(request => request.idempotency_key === before.captureId));
    assert.ok(client.uploadCalls.every(call => call.captureId === client.serverCaptureId && call.bytes.equals(BYTES)));
  });
}

// ── 2/3. one producer, one stream, immutable bytes ───────────────────────────────

describe("per-capture cycle lock (issue #1995)", () => {
  it("a foreground capture and a live pass on a bundled needs_action record: no producer, one stream, same bytes", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { pinned, rec } = await failedFirstUpload(client, coord);
    const gate = deferred();
    client.holdUpload = gate.promise;
    // The foreground carries the STALE pinned record the runner still holds.
    const fg = cap(coord, pinned);
    await client.uploadEntered.waitFor(2);
    await live(coord); // the cycle is busy: the live pass skips the record and returns at once
    assert.equal(client.uploadEntered.count, 2, "the live pass did not open a second stream");
    gate.resolve();
    assert.equal((await fg).state, "uploaded");
    assert.equal(git.produceCalls, 1, "no producer call after the first capture");
    assert.equal(client.maxActive, 1);
    const final = await current(coord);
    assert.equal(final.state, "uploaded");
    assert.equal(final.checksum, rec.checksum, "the original checksum is still bound");
    assert.equal(sha256(client.uploadCalls[1]!.bytes), rec.checksum);
    // A late failure never downgrades an uploaded record.
    const late = await priv(coord).markFailure(rec, "upload_transient");
    assert.equal(late.state, "uploaded");
    assert.equal((await current(coord)).state, "uploaded");
  });

  it("two foreground captures and a boot-sweep step on a pinned record produce once and stream once", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord, FIN);
    const snapshot = await coord.snapshotBootRecords();
    const produceGate = deferred();
    const uploadGate = deferred();
    git.hold = produceGate.promise;
    client.holdUpload = uploadGate.promise;
    const a = cap(coord, pinned);
    await git.entered.waitFor(1);
    const b = cap(coord, pinned);
    const sweep = coord.resumePending(undefined, snapshot);
    produceGate.resolve();
    await client.uploadEntered.waitFor(1);
    const rec = await current(coord);
    assert.equal(rec.state, "bundled");
    assert.equal(rec.bundlePath, path.join(root, "recovery", RUN, `${rec.captureId}.bundle`));
    assert.equal(sha256(fs.readFileSync(rec.bundlePath!)), rec.checksum, "the installed bytes are the journaled bytes");
    uploadGate.resolve();
    assert.equal((await a).state, "uploaded");
    assert.equal((await b).state, "uploaded");
    await sweep;
    assert.equal(git.produceCalls, 1);
    assert.equal(client.uploadEntered.count, 1);
    assert.equal(client.maxActive, 1);
    assert.equal(sha256(client.uploadCalls[0]!.bytes), rec.checksum, "the streamed bytes are the installed bytes");
  });
});

// ── 4a. cleanup interleaving ─────────────────────────────────────────────────────

describe("cleanup racing a capture never resurrects the record (issue #1995)", () => {
  for (const cleanup of ["release", "forget"] as const) {
    it(`${cleanup} while the bundle is produced: no record, no bundle, no temp, no RPC`, async () => {
      const client = new FakeClient();
      const git = new FakeGit();
      const coord = makeCoord(client, git);
      const pinned = await pinRec(coord);
      const gate = deferred();
      git.hold = gate.promise;
      const run = cap(coord, pinned);
      await git.entered.waitFor(1);
      if (cleanup === "release") await coord.release(RUN, 1);
      else await coord.forgetGeneration(RUN, 1);
      gate.resolve();
      const out = await run;
      assert.deepEqual([out.state, out.reason], ["needs_action", "record_removed"]);
      assert.deepEqual(await coord.inspect(RUN), []);
      assert.deepEqual(runFiles(), [], "neither a bundle nor a temp is left behind");
      assert.equal(client.reserveCalls.length + client.uploadEntered.count, 0);
    });
  }

  it("release while the reserve is in flight: the guarded write refuses, nothing is uploaded or recreated", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord);
    const gate = deferred();
    client.holdReserve = gate.promise;
    const run = cap(coord, pinned);
    await client.reserveEntered.waitFor(1);
    await coord.release(RUN, 1);
    gate.resolve();
    const out = await run;
    assert.equal(out.reason, "record_removed");
    assert.equal(client.uploadEntered.count, 0);
    assert.deepEqual(await coord.inspect(RUN), []);
    assert.deepEqual(runFiles(), []);
  });

  it("release while the stream is in flight: the uploaded write refuses and the bundle is dropped", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord);
    const gate = deferred();
    client.holdUpload = gate.promise;
    const run = cap(coord, pinned);
    await client.uploadEntered.waitFor(1);
    await coord.release(RUN, 1);
    gate.resolve();
    assert.equal((await run).reason, "record_removed");
    assert.deepEqual(await coord.inspect(RUN), []);
    assert.deepEqual(runFiles(), []);
  });

  it("markFailure after removal writes nothing, and a tampered record is refused at entry", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord);
    await coord.release(RUN, 1);
    await assert.rejects(priv(coord).markFailure(pinned, "capture_error"), /was removed/);
    assert.deepEqual(await coord.inspect(RUN), []);
    assert.deepEqual(runFiles(), []);

    const again = await pinRec(coord, { runId: RUN, generation: 2 });
    const file = path.join(root, "recovery", RUN, `${again.captureId}.json`);
    const tampered = JSON.stringify({ ...JSON.parse(fs.readFileSync(file, "utf8")), sourceSha: H_PRIME });
    fs.writeFileSync(file, tampered);
    const out = await cap(coord, again);
    assert.deepEqual([out.state, out.reason], ["needs_action", "record_unauthenticated"]);
    assert.equal(fs.readFileSync(file, "utf8"), tampered, "a tampered record is not rewritten");
    assert.equal(git.produceCalls, 0);
    assert.equal(client.reserveCalls.length, 0);
  });
});

// ── 4b. atomic pin updates ───────────────────────────────────────────────────────

describe("pin() never overwrites a newer state (issue #1995)", () => {
  it("a pin that observed `pinned` while a capture uploaded changes only an absent attempted head", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const observed = deferred();
    const gate = deferred();
    let armed = false;
    const coord = makeCoord(client, git, {
      testHooks: {
        afterPinObserve: async () => {
          if (!armed) return;
          observed.resolve();
          await gate.promise;
        },
      },
    });
    const pinned = await pinRec(coord);
    armed = true;
    const lateAttempted = "5555555555555555555555555555555555555555";
    const latePin = coord.pin({
      runId: RUN,
      sourceSha: H_PRIME,
      kind: "issue",
      branch: "agent/issue-1",
      generation: 1,
      attemptedHeadSha: lateAttempted,
      ...FIN,
    });
    await observed.promise;
    assert.equal((await cap(coord, pinned)).state, "uploaded");
    const before = await current(coord);
    gate.resolve();
    const out = await latePin;
    const after = await current(coord);
    assert.equal(after.state, "uploaded");
    assert.equal(out?.state, "uploaded");
    for (const k of ["bundlePath", "byteSize", "checksum", "chunkCount", "serverCaptureId", "sourceSha"] as const) {
      assert.equal(after[k], before[k], k);
    }
    assert.equal(after.sourceSha, H, "the ORIGINAL source");
    assert.equal(after.finalizationPin, undefined);
    assert.equal(after.attemptedHeadSha, lateAttempted, "an absent attempted head is filled");
  });

  it("a pin that advances the source while the bundle is produced discards the stale bytes", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord);
    const gate = deferred();
    git.hold = gate.promise;
    const run = cap(coord, pinned);
    await git.entered.waitFor(1);
    await coord.pin({ runId: RUN, sourceSha: H_PRIME, kind: "issue", branch: "agent/issue-1", generation: 1, ...FIN });
    gate.resolve();
    const out = await run;
    assert.deepEqual([out.state, out.reason], ["needs_action", "source_advanced"]);
    const rec = await current(coord);
    assert.equal(rec.state, "pinned");
    assert.equal(rec.sourceSha, H_PRIME);
    assert.equal(rec.finalizationPin, true);
    assert.deepEqual(runFiles(), [`${rec.captureId}.json`], "no bundle and no temp");
    assert.equal(client.reserveCalls.length + client.uploadEntered.count, 0);
  });
});

// ── 5. no regeneration ───────────────────────────────────────────────────────────

describe("journaled bytes are never regenerated (issue #1995)", () => {
  it("captureAndUpload re-uploads a journaled bundle without calling the producer", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { pinned } = await failedFirstUpload(client, coord);
    assert.equal((await cap(coord, pinned)).state, "uploaded");
    assert.equal(git.produceCalls, 1);
  });

  it("a boot-sweep step whose snapshot says pinned but whose latest record has a bundle only uploads", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const pinned = await pinRec(coord, FIN);
    const snapshot = await coord.snapshotBootRecords();
    assert.equal(snapshot[0]!.state, "pinned");
    client.uploadFailures.push({ error: typed(503, "busy") });
    await cap(coord, pinned);
    await coord.resumePending(undefined, snapshot);
    assert.equal(git.produceCalls, 1);
    assert.equal((await current(coord)).state, "uploaded");
  });
});

// ── 6. permanent dispositions ────────────────────────────────────────────────────

describe("permanent upload dispositions are final (issue #1995)", () => {
  const cases: Array<[string, unknown, string]> = [
    ["typed 403 not_authorized", typed(403, "not_authorized"), "stale_ownership"],
    ["reason-less 403", reqErr(403, "forbidden"), "upload_rejected"],
    ["unknown-reason 403", typed(403, "mystery"), "upload_rejected"],
    ["413 oversize", typed(413, "oversize"), "archive_constraint"],
  ];
  for (const [name, error, reason] of cases) {
    it(`${name} is journaled as ${reason} and never retried by any path`, async () => {
      const client = new FakeClient();
      const git = new FakeGit();
      const coord = makeCoord(client, git);
      const { pinned, rec } = await failedFirstUpload(client, coord, error);
      assert.equal(rec.reason, reason);
      assert.ok(PERMANENT_UPLOAD_REASONS.has(reason));
      const attempts = client.uploadEntered.count;
      await live(coord);
      await coord.resumePending();
      const out = await cap(coord, pinned);
      assert.deepEqual([out.state, out.reason], ["needs_action", reason]);
      assert.equal(client.uploadEntered.count, attempts, "no further upload attempt");
      assert.equal(git.produceCalls, 1);
      assert.ok(fs.existsSync(rec.bundlePath!), "the bundle and custody are kept");
    });
  }
});

// ── 7. integrity ─────────────────────────────────────────────────────────────────

describe("an integrity rejection is judged by the local bytes (issue #1995)", () => {
  it("intact bytes: transient, retried and uploaded", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord, typed(422, "integrity"));
    assert.equal(rec.reason, "capture_error");
    await live(coord);
    assert.equal((await current(coord)).state, "uploaded");
    assert.equal(git.produceCalls, 1);
  });

  it("altered bytes: local_bundle_mismatch, the file is kept, nothing is retried or re-bundled", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    client.uploadFailures.push({
      error: typed(422, "integrity"),
      before: () => {
        const bp = path.join(root, "recovery", RUN, fs.readdirSync(path.join(root, "recovery", RUN)).find((n) => n.endsWith(".bundle"))!);
        fs.appendFileSync(bp, "x");
      },
    });
    const pinned = await pinRec(coord);
    await cap(coord, pinned);
    const rec = await current(coord);
    assert.equal(rec.reason, "local_bundle_mismatch");
    assert.ok(fs.existsSync(rec.bundlePath!));
    const attempts = client.uploadEntered.count;
    await live(coord);
    await coord.resumePending();
    await cap(coord, pinned);
    assert.equal(client.uploadEntered.count, attempts);
    assert.equal(git.produceCalls, 1);
    assert.ok(fs.existsSync(rec.bundlePath!));
  });
});

// ── 8. rate limit / backoff ──────────────────────────────────────────────────────

describe("live pass backoff (issue #1995)", () => {
  it("a proxy 429 is transient: no retry before nextPassAt, and the spacing doubles up to the cap", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git, { liveBackoffBaseMs: 500, liveBackoffCapMs: 4000 });
    const { rec } = await failedFirstUpload(client, coord, reqErr(429, "slow down"));
    assert.equal(rec.reason, "capture_error");
    for (let i = 0; i < 4; i++) client.uploadFailures.push({ error: reqErr(429, "slow down") });
    const t0 = clock.t;
    let attempts = client.uploadEntered.count;
    let at = t0;
    for (const gap of [1000, 2000, 4000, 4000]) {
      clock.t = at;
      await live(coord);
      assert.equal(client.uploadEntered.count, ++attempts, `attempt at +${at - t0}`);
      assert.equal((await current(coord)).reason, "upload_transient");
      clock.t = at + gap - 1;
      await live(coord);
      assert.equal(client.uploadEntered.count, attempts, `no retry ${gap - 1}ms later`);
      at += gap;
    }
    clock.t = at;
    await live(coord);
    assert.equal((await current(coord)).state, "uploaded");
  });
});

// ── 9. credential gate ───────────────────────────────────────────────────────────

describe("credential rejection (issue #1995)", () => {
  it("a 401 is journaled and a live pass waits for a heartbeat sent after it", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord, reqErr(401, "unauthorized"));
    assert.equal(rec.reason, "credential_rejected");
    const attempts = client.uploadEntered.count;
    await live(coord, { authenticatedAtMs: clock.t });
    await live(coord, { authenticatedAtMs: clock.t - 5 });
    assert.equal(client.uploadEntered.count, attempts, "a heartbeat not newer than the rejection does not retry");
    await live(coord, { authenticatedAtMs: clock.t + 1 });
    assert.equal((await current(coord)).state, "uploaded");
  });
});

// ── 10/11. pass single-flight, executing runs ────────────────────────────────────

describe("live pass scoping (issue #1995)", () => {
  it("is a no-op while the boot sweep is in flight", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    for (const runId of ["run-a", "run-b"]) {
      client.uploadFailures.push({ error: typed(503, "busy") });
      const pinned = await pinRec(coord, { runId });
      await cap(coord, pinned);
    }
    const attempts = client.uploadEntered.count;
    const gate = deferred();
    client.holdUpload = gate.promise;
    const sweep = coord.resumePending();
    await client.uploadEntered.waitFor(attempts + 1);
    await live(coord);
    assert.equal(client.uploadEntered.count, attempts + 1, "the live pass did not start a second upload");
    gate.resolve();
    await sweep;
    assert.equal(client.uploadEntered.count, attempts + 2, "each record is uploaded exactly once by the sweep");
    for (const runId of ["run-a", "run-b"]) assert.equal((await coord.inspect(runId))[0]!.state, "uploaded");
  });

  it("skips a run the runner is executing", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord);
    const attempts = client.uploadEntered.count;
    await live(coord, { executing: true });
    assert.equal(client.uploadEntered.count, attempts);
    assert.deepEqual(await current(coord), rec);
  });
});

// ── B1: an unreadable record never costs its bundle bytes ────────────────────────

/** Make every read of a record `.json` fail with `code` from the `failFrom`-th read on. */
function failJsonReads(failFrom: number, code = "EMFILE"): { restore(): void; reads(): number } {
  const real = fsp.readFile.bind(fsp);
  let n = 0;
  const m = mock.method(fsp, "readFile", (async (...args: Parameters<typeof fsp.readFile>) => {
    if (String(args[0]).endsWith(".json") && ++n >= failFrom) {
      throw Object.assign(new Error(`${code}: simulated`), { code });
    }
    return real(...args);
  }) as typeof fsp.readFile);
  return { restore: () => m.mock.restore(), reads: () => n };
}
const jsonPath = (rec: RecoveryRecord): string => path.join(root, "recovery", RUN, `${rec.captureId}.json`);

describe("a read error is not a removal (issue #1995)", () => {
  afterEach(() => mock.restoreAll());

  it("captureAndUpload: EMFILE on the mid-cycle re-read keeps the bundle and the record", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { pinned, rec } = await failedFirstUpload(client, coord);
    const before = fs.readFileSync(jsonPath(rec), "utf8");
    const attempts = client.uploadEntered.count;
    // read 1 is the entry re-read; read 2 is the requirePresent before the reserve/stream.
    const fault = failJsonReads(2);
    let out: RecoveryOutcome;
    try {
      out = await cap(coord, pinned);
    } finally {
      fault.restore();
    }
    assert.deepEqual([out.state, out.reason], ["needs_action", "record_unauthenticated"]);
    assert.ok(fs.existsSync(rec.bundlePath!), "the bundle bytes survive a read error");
    assert.equal(fs.readFileSync(jsonPath(rec), "utf8"), before, "nothing is written");
    assert.equal(client.uploadEntered.count, attempts, "nothing is uploaded on a record that cannot be read");
    await live(coord);
    assert.equal((await current(coord)).state, "uploaded", "a later pass still has the bytes to upload");
  });

  it("a live pass: EMFILE on the mid-step re-read keeps the bundle and leaves the record eligible", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord);
    const before = fs.readFileSync(jsonPath(rec), "utf8");
    const attempts = client.uploadEntered.count;
    // read 1 is the pass's listing, 2 the step's re-read, 3 the requirePresent before the stream.
    const fault = failJsonReads(3);
    try {
      await live(coord);
    } finally {
      fault.restore();
    }
    assert.ok(fs.existsSync(rec.bundlePath!), "the bundle bytes survive a read error");
    assert.equal(fs.readFileSync(jsonPath(rec), "utf8"), before, "the record is unchanged");
    assert.equal(client.uploadEntered.count, attempts);
    clock.t += 1_000;
    await live(coord);
    assert.equal((await current(coord)).state, "uploaded", "the next pass uploads it");
  });

  it("a boot-sweep step: EMFILE on the mid-step re-read keeps the bundle", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const { rec } = await failedFirstUpload(client, coord);
    const before = fs.readFileSync(jsonPath(rec), "utf8");
    // 1 is the boot listing, 2 the step's re-read, 3 the requirePresent before the reserve/stream.
    const fault = failJsonReads(3);
    try {
      await coord.resumePending();
    } finally {
      fault.restore();
    }
    assert.ok(fs.existsSync(rec.bundlePath!));
    assert.equal(fs.readFileSync(jsonPath(rec), "utf8"), before);
  });

  it("a confirmed removal still cleans up: only the record file deleted mid-stream drops the bundle", async () => {
    for (const via of ["capture", "live"] as const) {
      const client = new FakeClient();
      const git = new FakeGit();
      const coord = makeCoord(client, git);
      const { pinned, rec } = await failedFirstUpload(client, coord);
      const gate = deferred();
      client.holdUpload = gate.promise;
      const attempts = client.uploadEntered.count;
      const run = via === "capture" ? cap(coord, pinned) : live(coord);
      await client.uploadEntered.waitFor(attempts + 1);
      await fsp.rm(jsonPath(rec)); // the record alone: the bundle is still on disk
      assert.ok(fs.existsSync(rec.bundlePath!));
      gate.resolve();
      const out = await run;
      if (via === "capture") assert.equal((out as RecoveryOutcome).reason, "record_removed");
      assert.deepEqual(runFiles(), [], `${via}: the removed record's bundle is dropped, nothing recreated`);
      await fsp.rm(path.join(root, "recovery"), { recursive: true, force: true });
    }
  });
});

// ── N1/N2: live pass selection ───────────────────────────────────────────────────

/** Two runs each holding a journaled bundle whose first upload failed; returns run ids in the
 *  order the pass will list them. */
async function twoFailed(client: FakeClient, coord: RecoveryCoordinator): Promise<[string, string]> {
  for (const runId of ["run-a", "run-b"]) {
    client.uploadFailures.push({ error: typed(503, "busy") });
    const pinned = await pinRec(coord, { runId });
    assert.equal((await cap(coord, pinned)).state, "needs_action");
  }
  const order = fs.readdirSync(path.join(root, "recovery"));
  assert.equal(order.length, 2);
  return [order[0]!, order[1]!];
}
const stateOf = async (coord: RecoveryCoordinator, runId: string): Promise<string> =>
  (await coord.inspect(runId))[0]!.state;

describe("live pass selection (issue #1995)", () => {
  it("liveMaxPerPass=1: an executing run does not hold the slot, the idle record uploads", async () => {
    for (const executingIdx of [0, 1]) {
      const client = new FakeClient();
      const git = new FakeGit();
      const coord = makeCoord(client, git, { liveMaxPerPass: 1 });
      const order = await twoFailed(client, coord);
      const executing = order[executingIdx]!;
      const idle = order[1 - executingIdx]!;
      await coord.resumeLive({
        isExecuting: (runId) => runId === executing,
        authenticatedAtMs: clock.t + 1_000_000,
      });
      assert.equal(await stateOf(coord, idle), "uploaded", `idle ${idle} uploads though ${executing} is listed first/last`);
      assert.equal(await stateOf(coord, executing), "needs_action");
      await fsp.rm(path.join(root, "recovery"), { recursive: true, force: true });
    }
  });

  it("liveMaxPerPass=1: a record whose cycle is busy rotates behind its peers instead of holding the slot", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git, { liveMaxPerPass: 1 });
    const [first, second] = await twoFailed(client, coord);
    const gate = deferred();
    client.holdUpload = gate.promise;
    client.holdUploadRun = first;
    const attempts = client.uploadEntered.count;
    const fg = (async () => {
      const rec = (await coord.inspect(first))[0]!;
      return cap(coord, rec);
    })();
    await client.uploadEntered.waitFor(attempts + 1);
    // first is listed first and its cycle is busy: pass 1 skips it (consuming the only slot)...
    await live(coord);
    clock.t += 1_000;
    // ...but it is now the most recently attempted, so pass 2 reaches the other record.
    await live(coord);
    assert.equal(await stateOf(coord, second), "uploaded");
    gate.resolve();
    await fg;
  });

  it("the per-pass cap bounds the records a single pass attempts", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git, { liveMaxPerPass: 1 });
    await twoFailed(client, coord);
    const attempts = client.uploadEntered.count;
    await live(coord);
    assert.equal(client.uploadEntered.count, attempts + 1);
  });

  it("the least recently attempted record goes first, so a failing record cannot starve its peer", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git, { liveMaxPerPass: 1 });
    await twoFailed(client, coord);
    client.uploadFailures.push({ error: typed(503, "busy") }, { error: typed(503, "busy") });
    const from = client.uploadRuns.length;
    await live(coord);
    clock.t += 1_000;
    await live(coord);
    const [a, b] = client.uploadRuns.slice(from);
    assert.ok(a && b && a !== b, `two passes reached two different runs, got ${a} then ${b}`);
  });

  it("a credential rejection stops the pass: the next eligible record is not attempted", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    const [first, second] = await twoFailed(client, coord);
    client.uploadFailures.push({ error: reqErr(401, "unauthorized") });
    const attempts = client.uploadEntered.count;
    await live(coord);
    assert.equal(client.uploadEntered.count, attempts + 1, "only the rejected record was attempted");
    assert.equal(await stateOf(coord, second), "needs_action");
    assert.equal((await coord.inspect(first))[0]!.reason, "credential_rejected");
  });

  it("a run that becomes executing after selection is skipped under the lock", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    await twoFailed(client, coord);
    const gate = deferred();
    client.holdUpload = gate.promise;
    const attempts = client.uploadEntered.count;
    let flipped = false;
    const pass = coord.resumeLive({
      isExecuting: (runId) => flipped && runId !== client.uploadRuns[attempts],
      authenticatedAtMs: clock.t + 1_000_000,
    });
    await client.uploadEntered.waitFor(attempts + 1);
    // Both were selected as idle; the second now starts executing before its step takes the lock.
    flipped = true;
    gate.resolve();
    await pass;
    assert.equal(client.uploadEntered.count, attempts + 1, "the now-executing run was not uploaded");
  });

  it("forgets the attempt stamps of captures that left the journal", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoord(client, git);
    await failedFirstUpload(client, coord);
    client.uploadFailures.push({ error: typed(503, "busy") });
    await live(coord);
    const stamps = (coord as unknown as { lastAttemptAt: Map<string, number> }).lastAttemptAt;
    assert.equal(stamps.size, 1);
    await coord.release(RUN, 1);
    clock.t += 1_000_000;
    await live(coord);
    assert.equal(stamps.size, 0);
  });
});

// ── 12. classifier table ─────────────────────────────────────────────────────────

describe("classifyUploadFailure (issue #1995)", () => {
  async function journaled(): Promise<{ intact: RecoveryRecord; altered: RecoveryRecord; unreadable: RecoveryRecord }> {
    const mk = (name: string, content: Buffer | undefined): RecoveryRecord => {
      const file = path.join(root, name);
      if (content) fs.writeFileSync(file, content);
      return {
        version: 1,
        runId: RUN,
        captureId: name,
        sourceSha: H,
        kind: "issue",
        branch: "b",
        createdAt: 0,
        state: "needs_action",
        bundlePath: file,
        byteSize: BYTES.length,
        checksum: sha256(BYTES),
        chunkCount: 1,
      };
    };
    return {
      intact: mk("intact.bundle", BYTES),
      altered: mk("altered.bundle", Buffer.concat([BYTES, Buffer.from("x")])),
      unreadable: mk("missing.bundle", undefined),
    };
  }

  it("maps every status and typed reason to its disposition", async () => {
    const { intact, altered, unreadable } = await journaled();
    const permanent = (reason: string) => ({ kind: "permanent", reason });
    const transient = { kind: "transient" };
    const table: Array<[string, unknown, unknown, RecoveryRecord | undefined]> = [
      ["fetch TypeError", new TypeError("fetch failed"), transient, undefined],
      ["timeout", new DOMException("timed out", "TimeoutError"), transient, undefined],
      ["fs error", Object.assign(new Error("EIO"), { code: "EIO" }), transient, undefined],
      ["401", reqErr(401), { kind: "credential", reason: "credential_rejected" }, undefined],
      ["403 not_authorized", typed(403, "not_authorized"), permanent("stale_ownership"), undefined],
      ["403 untyped", reqErr(403, "no"), permanent("upload_rejected"), undefined],
      ["403 unknown reason", typed(403, "mystery"), permanent("upload_rejected"), undefined],
      ["400 bad_request", typed(400, "bad_request"), permanent("upload_rejected"), undefined],
      ["400 untyped", reqErr(400), permanent("upload_rejected"), undefined],
      ["409 ambiguous_generation", typed(409, "ambiguous_generation"), permanent("upload_rejected"), undefined],
      ["404 capture_not_found", typed(404, "capture_not_found"), permanent("upload_rejected"), undefined],
      ["404 untyped", reqErr(404), permanent("upload_rejected"), undefined],
      ["409 not_available", typed(409, "not_available"), permanent("upload_rejected"), undefined],
      ["409 manifest_conflict", typed(409, "manifest_conflict"), permanent("upload_rejected"), undefined],
      ["409 untyped", reqErr(409), permanent("upload_rejected"), undefined],
      ["413 oversize", typed(413, "oversize"), permanent("archive_constraint"), undefined],
      ["413 untyped", reqErr(413), permanent("archive_constraint"), undefined],
      ["422 integrity, intact bytes", typed(422, "integrity"), transient, intact],
      ["422 untyped, intact bytes", reqErr(422), transient, intact],
      ["422 integrity, altered bytes", typed(422, "integrity"), permanent("local_bundle_mismatch"), altered],
      ["422 integrity, unreadable file", typed(422, "integrity"), permanent("local_bundle_mismatch"), unreadable],
      ["429", reqErr(429, "slow down"), transient, undefined],
      ["408", reqErr(408), transient, undefined],
      ["503 busy", typed(503, "busy"), transient, undefined],
      ["500 internal", typed(500, "internal"), transient, undefined],
      ["502 untyped", reqErr(502), transient, undefined],
      ["507 quota", typed(507, "quota"), { kind: "transient", reason: "storage_quota_exceeded" }, undefined],
      ["401 quota", typed(401, "quota"), { kind: "credential", reason: "credential_rejected" }, undefined],
      ["507 unknown reason", typed(507, "mystery"), transient, undefined],
      ["507 non-string reason", reqErr(507, JSON.stringify({ reason: 507 })), transient, undefined],
      ["507 malformed body", reqErr(507, "{"), transient, undefined],
      ["507 oversize", typed(507, "oversize"), permanent("archive_constraint"), undefined],
      ["507 not_authorized", typed(507, "not_authorized"), permanent("stale_ownership"), undefined],
      ["507 integrity, altered bytes", typed(507, "integrity"), permanent("local_bundle_mismatch"), altered],
      ["507 untyped", reqErr(507), transient, undefined],
    ];
    for (const [name, err, want, record] of table) {
      assert.deepEqual(await classifyUploadFailure(err, record), want, name);
    }
  });
});
