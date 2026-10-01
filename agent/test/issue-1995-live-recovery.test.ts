import { afterEach, beforeEach, describe, it } from "node:test";
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
import { sleep } from "../src/util.js";

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
  holdUpload: Promise<void> | undefined;
  /** One entry is consumed per upload attempt; an absent entry means the upload is accepted. */
  uploadFailures: Array<{ error: unknown; before?: () => void }> = [];
  active = 0;
  maxActive = 0;
  serverCaptureId = "server-capture-1";

  async reserveRecoveryCapture(_runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push(req);
    this.reserveEntered.inc();
    if (this.holdReserve) await this.holdReserve;
    return { capture_id: this.serverCaptureId, state: "preparing" };
  }
  async getRecoveryCaptureStatus(_runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    _runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    this.active++;
    this.maxActive = Math.max(this.maxActive, this.active);
    this.uploadEntered.inc();
    try {
      if (this.holdUpload) await this.holdUpload;
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
    const runner = {
      resumePendingRecoveries: async () => {},
      resumeLiveRecoveries: (authenticatedAtMs: number, signal?: AbortSignal) =>
        coord.resumeLive({ isExecuting: () => false, authenticatedAtMs, signal }),
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
      const deadline = Date.now() + 5000;
      while ((await current(coord)).state !== "uploaded" && Date.now() < deadline) await sleep(5);
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
      ["507 quota", typed(507, "quota"), transient, undefined],
      ["507 untyped", reqErr(507), transient, undefined],
    ];
    for (const [name, err, want, record] of table) {
      assert.deepEqual(await classifyUploadFailure(err, record), want, name);
    }
  });
});
