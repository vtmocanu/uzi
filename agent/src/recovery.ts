// Durable run recovery: worker capture, authenticated journal, and model-free
// restart-safe upload (PRD #1296 M3, decisions D1/D3/D5/D6/D9).
//
// This module owns the WORKER side of the archive primitive: at the protected
// finalization boundary the runner PINS the original committed head H into an
// AUTHENTICATED local journal (unconditional, credential-free — {@link pin}); on a
// finalization failure it produces an independently-usable Git bundle and streams it to
// the API ({@link captureAndUpload}); on a successful full publication it releases the
// run's custody ({@link release}); and on worker restart it re-uploads any journaled
// bundle BYTE-IDENTICALLY with NO forge PAT ({@link resumePending}).
//
// The journal is authenticated so a plain editable JSON cannot authorize releasing or
// substituting the last original (D1): each record carries a domain-separated per-worker
// HMAC keyed by a key DERIVED from the worker join token. A tampered/invalid/missing
// record is treated as needs_action, NEVER a guessed head or a silent release.
//
// The journal store is a DISTINCT /data subtree (`GitCache.recoveryRoot`) — separate from
// the runner clone (torn down) and from the unrelated issue #1187 git-config
// recovery-capture journal. It survives runner-clone removal and worker restart.

import { createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import { createReadStream } from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";

import type { Logger } from "./log.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
  RunKind,
} from "./protocol.js";
import { RecoveryBundleTooLargeError, type RecoveryBundleResult } from "./git.js";

/** The six forge-finalizing run kinds that reach code publication (PRD #1296 In-scope).
 *  `chat` and `judge` never publish code, so they never open a custody hold and never
 *  capture. A run kind absent from this set is not archived. */
export const CODE_PUBLISHING_KINDS: ReadonlySet<RunKind> = new Set<RunKind>([
  "issue",
  "ci_fix",
  "self_improve",
  "prompt",
  "task",
  "mr_rework",
]);

/** True iff the resolved run kind reaches code publication and so is archive-eligible. */
export function isCodePublishingKind(kind: RunKind): boolean {
  return CODE_PUBLISHING_KINDS.has(kind);
}

/** Local capture lifecycle in the durable journal. Distinct from the SERVER capture
 *  lifecycle (preparing/uploading/available/…): this tracks the worker's own progress so a
 *  restart knows what to re-do.
 *   - `pinned`: H is journaled; no verified bundle yet (the unconditional boundary pin).
 *   - `bundled`: a verified bundle FILE is journaled (D5: journal the file BEFORE binding
 *     its manifest), ready to (re-)upload with no PAT.
 *   - `uploaded`: the bundle bytes were accepted by the API (durable capture).
 *   - `needs_action`: a producer/upload/quota/oversize failure; the source is retained. */
export type RecoveryLocalState = "pinned" | "bundled" | "uploaded" | "needs_action";

/** One authenticated journal record. The MAC (stored alongside, not in this object)
 *  covers a canonical serialization of every field here, so editing any field is refused. */
export interface RecoveryRecord {
  version: 1;
  /** The run this capture belongs to. Uniqueness by run_id (a per-run UUID) prevents a
   *  later run on the same ISSUE from overwriting or adopting this capture (D1). */
  runId: string;
  /** The worker's durable source-journal identity, minted once at pin and persisted, so a
   *  lost reserve ACK re-reserves the SAME server capture (it is the reserve
   *  idempotency_key). */
  captureId: string;
  /** The original committed head H (40-hex). NEVER the post-align H', a checkpoint tip, or
   *  the private origin/<default> (D1). */
  sourceSha: string;
  /** The attempted publication head H' (provenance only; omitted when no publish was
   *  attempted). Recording it is provenance, never permission to substitute it. */
  attemptedHeadSha?: string;
  /** Run kind (provenance). */
  kind: RunKind;
  /** The run's branch, used to name the bundle's source ref. */
  branch: string;
  /** claim_generation when the server provides it. See the M3 report: the frozen
   *  ClaimResponse does not carry claim_generation, so this is currently always absent. */
  generation?: number;
  /** Epoch ms of the pin. */
  createdAt: number;
  state: RecoveryLocalState;
  /** Absolute path of the verified bundle FILE (set at `bundled`). */
  bundlePath?: string;
  /** Complete-bundle byte size (set at `bundled`). */
  byteSize?: number;
  /** Lowercase hex SHA-256 of the bundle bytes (set at `bundled`). */
  checksum?: string;
  /** Expected ordered-chunk inventory (set at `bundled`). */
  chunkCount?: number;
  /** Verified public prerequisite closure the bundle imports against (set at `bundled`). */
  prerequisiteShas?: string[];
  /** True when the bundle is self-contained (no forge-reachable prerequisite). */
  selfContained?: boolean;
  /** The server-minted capture id, bound after the reserve ACK. */
  serverCaptureId?: string;
  /** A bounded reason on `needs_action`. */
  reason?: string;
}

/** The outcome of a capture attempt, surfaced to the runner for logging. */
export interface RecoveryOutcome {
  state: RecoveryLocalState;
  captureId: string;
  reason?: string;
}

/** The archive RPC subset {@link RecoveryCoordinator} needs — WorkerClient satisfies it
 *  structurally; a test supplies a fake. */
export interface RecoveryArchiveClient {
  reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse>;
  getRecoveryCaptureStatus(runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse>;
  uploadRecoveryBundle(
    runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
    signal?: AbortSignal,
  ): Promise<RecoveryCaptureStatusResponse>;
  releaseRecoveryCustody(runId: string): Promise<RecoveryReleaseResponse>;
}

/** The bundle-producer subset {@link RecoveryCoordinator} needs — GitCache satisfies it. */
export interface RecoveryBundleProducer {
  produceRecoveryBundle(
    barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult>;
  fetchDefaultTip(
    barePath: string,
    defaultBranch: string,
    pat?: string,
    cloneUrl?: string,
    username?: string,
  ): Promise<string>;
}

export interface PinInput {
  runId: string;
  sourceSha: string;
  kind: RunKind;
  branch: string;
  attemptedHeadSha?: string;
  generation?: number;
}

export interface CaptureInput {
  record: RecoveryRecord;
  barePath: string;
  /** The fresh-forge-tip inputs (D5). The PAT is used ONLY here, on the failure path
   *  inside the reap-before-credentialed-git boundary, to fetch the verified forge tip. */
  defaultBranch: string;
  forgePat?: string;
  cloneUrl?: string;
  forgeUsername?: string;
  attemptedHeadSha?: string;
  signal?: AbortSignal;
}

export interface RecoveryCoordinatorOptions {
  client: RecoveryArchiveClient;
  git: RecoveryBundleProducer;
  log: Logger;
  /** `GitCache.recoveryRoot` = join(dataDir, "recovery"). */
  recoveryRoot: string;
  /** The worker join token — the MAC key is DERIVED from it (never stored). Absent ⇒
   *  recovery is DISABLED (every method is a no-op), so a token-less test harness is
   *  unaffected. */
  workerToken?: string;
  now?: () => number;
}

// Domain-separation label for the journal-key derivation (D1). Binding a distinct label
// into the derived key keeps this MAC unusable for any other worker-token-keyed purpose.
const JOURNAL_KEY_LABEL = "uzi-recovery-journal-v1";
const JOURNAL_VERSION = 1 as const;

/** Stable, key-sorted JSON so the MAC is deterministic regardless of insertion order. */
function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map((v) => canonicalJson(v)).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).sort();
  const parts: string[] = [];
  for (const k of keys) {
    if (obj[k] === undefined) continue; // omit undefined so an absent optional is stable
    parts.push(`${JSON.stringify(k)}:${canonicalJson(obj[k])}`);
  }
  return `{${parts.join(",")}}`;
}

/**
 * The persistent, authenticated durable-recovery coordinator (PRD #1296 M3).
 *
 * Every method is best-effort from the runner's perspective and MUST NOT disturb the run's
 * honest terminal reporting: the caller wraps them, and they also guard internally. The
 * source is protected by the unconditional local pin plus the server custody hold, so an
 * upload can safely happen after (or across a restart from) the terminal report.
 */
export class RecoveryCoordinator {
  private readonly client: RecoveryArchiveClient;
  private readonly git: RecoveryBundleProducer;
  private readonly log: Logger;
  private readonly recoveryRoot: string;
  private readonly now: () => number;
  private readonly key: Buffer | undefined;
  readonly enabled: boolean;

  constructor(opts: RecoveryCoordinatorOptions) {
    this.client = opts.client;
    this.git = opts.git;
    this.log = opts.log;
    this.recoveryRoot = opts.recoveryRoot;
    this.now = opts.now ?? (() => Date.now());
    // The MAC key is DERIVED from the (stable) worker join token, so it re-derives
    // identically on restart. A token-less coordinator is disabled.
    this.key = opts.workerToken
      ? createHmac("sha256", opts.workerToken).update(JOURNAL_KEY_LABEL).digest()
      : undefined;
    this.enabled = this.key !== undefined;
  }

  // ── D1: unconditional local pin + authenticated journal ────────────────────────

  /**
   * Pin H into the durable authenticated journal. UNCONDITIONAL, local and credential-free
   * — this never waits for a server response and never uses a forge PAT. Idempotent within
   * a run: an existing verified record for the same (runId, sourceSha) is reused, so a
   * re-execution of the same committed head does not duplicate the pin.
   *
   * Returns the record, or undefined when recovery is disabled or the pin failed (the
   * caller treats undefined as "no capture to drive", never as a release authority).
   */
  async pin(input: PinInput): Promise<RecoveryRecord | undefined> {
    if (!this.enabled) return undefined;
    try {
      const existing = await this.findRecord(input.runId, (r) => r.sourceSha === input.sourceSha);
      if (existing) {
        // Update the provenance H' if it is now known and was not recorded.
        if (input.attemptedHeadSha && !existing.attemptedHeadSha) {
          existing.attemptedHeadSha = input.attemptedHeadSha;
          await this.writeRecord(existing);
        }
        return existing;
      }
      const record: RecoveryRecord = {
        version: JOURNAL_VERSION,
        runId: input.runId,
        captureId: randomUUID(),
        sourceSha: input.sourceSha,
        attemptedHeadSha: input.attemptedHeadSha,
        kind: input.kind,
        branch: input.branch,
        generation: input.generation,
        createdAt: this.now(),
        state: "pinned",
      };
      await this.writeRecord(record);
      this.log.info("recovery: pinned source head at finalization boundary", {
        run_id: input.runId,
        capture_id: record.captureId,
      });
      return record;
    } catch (err) {
      this.log.warn("recovery: failed to pin source head (source stays protected by the server hold)", {
        run_id: input.runId,
        error: errText(err),
      });
      return undefined;
    }
  }

  // ── D3/D5: produce + verify + journal-before-manifest + upload ──────────────────

  /**
   * The model-free capture on a finalization failure: (1) produce and verify an
   * independently-usable bundle (resolving the prerequisite against a FRESH forge tip),
   * (2) journal the verified bundle FILE before binding its manifest (D5), then
   * (3) reserve → bind manifest → stream upload. On any failure the source stays pinned and
   * the record becomes needs_action; the caller reports the run's execution honestly.
   */
  async captureAndUpload(input: CaptureInput): Promise<RecoveryOutcome> {
    if (!this.enabled) return { state: "pinned", captureId: input.record.captureId };
    let record = input.record;
    try {
      if (input.attemptedHeadSha && !record.attemptedHeadSha) {
        record = { ...record, attemptedHeadSha: input.attemptedHeadSha };
        await this.writeRecord(record);
      }
      if (record.state === "uploaded") return { state: "uploaded", captureId: record.captureId };
      if (record.state !== "bundled") {
        const produced = await this.produceBundle(record, input);
        if (produced.kind === "already_published") {
          // H is already on the fresh forge tip — verified no-unpublished-output (D3).
          // Release custody and drop the local journal; nothing to archive.
          await this.release(record.runId);
          return { state: "uploaded", captureId: record.captureId, reason: "already_published" };
        }
        if (produced.kind !== "bundled") {
          return { state: "needs_action", captureId: record.captureId, reason: produced.reason };
        }
        record = produced.record;
      }
      return await this.uploadJournaledBundle(record, input.signal);
    } catch (err) {
      this.log.warn("recovery: capture/upload failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      return this.markNeedsAction(record, "capture_error");
    }
  }

  /** Produce + verify the bundle and journal its FILE before any manifest binding (D5).
   *  Returns the bundled record, an already-published signal, or a needs_action reason. */
  private async produceBundle(
    record: RecoveryRecord,
    input: CaptureInput,
  ): Promise<
    | { kind: "bundled"; record: RecoveryRecord }
    | { kind: "already_published" }
    | { kind: "needs_action"; reason: string }
  > {
    // Resolve a FRESH, verified forge tip (D5) inside the reap-before-credentialed-git
    // boundary (the agent is already reaped at finalization). Best-effort: a fetch failure
    // falls back to a self-contained bundle rather than blocking capture. The runner clone's
    // private origin/main and any checkpoint wrapper are NEVER consulted here.
    let forgeTip: string | undefined;
    try {
      forgeTip = await this.git.fetchDefaultTip(
        input.barePath,
        input.defaultBranch,
        input.forgePat,
        input.cloneUrl,
        input.forgeUsername,
      );
    } catch (err) {
      this.log.warn("recovery: could not fetch a fresh forge tip; producing a self-contained bundle", {
        run_id: record.runId,
        error: errText(err),
      });
    }
    const outPath = this.bundlePath(record);
    await fs.mkdir(path.dirname(outPath), { recursive: true, mode: 0o700 });
    let result: RecoveryBundleResult;
    try {
      result = await this.git.produceRecoveryBundle(input.barePath, {
        sourceSha: record.sourceSha,
        outPath,
        forgeTip,
      });
    } catch (err) {
      if (err instanceof RecoveryBundleTooLargeError) {
        this.log.warn("recovery: bundle exceeds the size limit; retaining source (needs_action)", {
          run_id: record.runId,
          byte_size: err.byteSize,
          max_bytes: err.maxBytes,
        });
        await this.markNeedsAction(record, "oversized");
        return { kind: "needs_action", reason: "oversized" };
      }
      this.log.warn("recovery: bundle production failed; retaining source (needs_action)", {
        run_id: record.runId,
        error: errText(err),
      });
      await this.markNeedsAction(record, "bundle_failed");
      return { kind: "needs_action", reason: "bundle_failed" };
    }
    if (result.alreadyPublished) return { kind: "already_published" };
    // Journal the verified bundle FILE before binding its manifest (D5), so a
    // terminal/restart retry re-uploads exactly these bytes with no forge PAT.
    const bundled: RecoveryRecord = {
      ...record,
      state: "bundled",
      bundlePath: result.bundlePath,
      byteSize: result.byteSize,
      checksum: result.checksum,
      chunkCount: result.chunkCount,
      prerequisiteShas: result.prerequisiteShas,
      selfContained: result.selfContained,
      reason: undefined,
    };
    await this.writeRecord(bundled);
    this.log.info("recovery: journaled verified bundle before upload", {
      run_id: record.runId,
      capture_id: record.captureId,
      byte_size: result.byteSize,
      self_contained: result.selfContained,
    });
    return { kind: "bundled", record: bundled };
  }

  /**
   * Reserve → bind manifest → stream the JOURNALED bundle bytes. Restart-safe: reads the
   * bundle from disk (never re-derives it, never touches a forge PAT). An incomplete local
   * input (missing bundle file / manifest facts) → needs_action, never a fresh forge fetch.
   * Never silently swaps a rebuilt bundle's checksum under a bound capture id — the journaled
   * checksum is the one bound and streamed.
   */
  private async uploadJournaledBundle(
    record: RecoveryRecord,
    signal?: AbortSignal,
  ): Promise<RecoveryOutcome> {
    if (
      !record.bundlePath ||
      typeof record.byteSize !== "number" ||
      !record.checksum ||
      typeof record.chunkCount !== "number"
    ) {
      return this.markNeedsAction(record, "incomplete_local_inputs");
    }
    if (!(await fileExists(record.bundlePath))) {
      // The verified bundle is gone and we may not reproduce it without a forge PAT (D5).
      return this.markNeedsAction(record, "bundle_file_missing");
    }
    // Reserve once; the local captureId is the idempotency_key so a lost ACK re-reserves the
    // SAME server capture rather than duplicating it.
    let current = record;
    if (!current.serverCaptureId) {
      const reserved = await this.client.reserveRecoveryCapture(current.runId, {
        run_id: current.runId,
        idempotency_key: current.captureId,
        source_sha: current.sourceSha,
        ...(current.attemptedHeadSha ? { attempted_head_sha: current.attemptedHeadSha } : {}),
      });
      current = { ...current, serverCaptureId: reserved.capture_id };
      await this.writeRecord(current);
    }
    const manifest: RecoveryUploadManifest = {
      byte_size: current.byteSize!,
      checksum: current.checksum!,
      chunk_count: current.chunkCount!,
      ...(current.prerequisiteShas && current.prerequisiteShas.length > 0
        ? { prerequisite_shas: current.prerequisiteShas }
        : {}),
    };
    const status = await this.client.uploadRecoveryBundle(
      current.runId,
      current.serverCaptureId!,
      manifest,
      createReadStream(current.bundlePath!),
      signal,
    );
    const uploaded: RecoveryRecord = { ...current, state: "uploaded", reason: undefined };
    await this.writeRecord(uploaded);
    // Bytes are durable on the server; free the local copy. The small journal record stays
    // (state=uploaded) so a restart sweep skips it.
    await fs.rm(current.bundlePath!, { force: true }).catch(() => undefined);
    this.log.info("recovery: bundle uploaded (durable capture)", {
      run_id: current.runId,
      capture_id: current.captureId,
      server_capture_id: current.serverCaptureId,
      server_state: status.state,
    });
    return { state: "uploaded", captureId: current.captureId };
  }

  // ── D3: release custody on successful full publication ──────────────────────────

  /**
   * Release the run's custody after a successful full publication (nothing to archive).
   * Best-effort and idempotent: a failed release is NOT a precondition for reporting
   * completion (a reconciler settles it later, D3), so this never throws to the caller. On a
   * clean release, the run's local journal is removed.
   */
  async release(runId: string): Promise<void> {
    if (!this.enabled) return;
    try {
      const res = await this.client.releaseRecoveryCustody(runId);
      this.log.info("recovery: released custody after successful publication", {
        run_id: runId,
        released: res.released,
        holds_released: res.holds_released,
      });
      await this.removeRunDir(runId);
    } catch (err) {
      this.log.warn("recovery: custody release failed (a reconciler will retry; completion is unaffected)", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  // ── D3/D5: restart-safe sweep ───────────────────────────────────────────────────

  /**
   * On worker restart, re-upload any journaled bundle BYTE-IDENTICALLY with NO forge PAT
   * (D5). Verifies each record's MAC first: a tampered/invalid record is refused (left for
   * needs_action), never trusted. A `pinned` record with no bundle, or one whose bundle file
   * is gone, is incomplete and cannot be reproduced without a PAT → needs_action.
   */
  async resumePending(signal?: AbortSignal): Promise<void> {
    if (!this.enabled) return;
    let runDirs: string[];
    try {
      runDirs = await fs.readdir(this.recoveryRoot);
    } catch {
      return; // no journal dir yet — nothing to resume
    }
    for (const runId of runDirs) {
      if (signal?.aborted) return;
      const records = await this.listRecords(runId);
      for (const record of records) {
        if (signal?.aborted) return;
        if (record.state === "uploaded") continue;
        if (record.state !== "bundled" && record.state !== "needs_action") {
          // `pinned` with no verified bundle: cannot reproduce without a forge PAT (D5).
          await this.markNeedsAction(record, "no_local_bundle_after_restart");
          continue;
        }
        try {
          await this.uploadJournaledBundle(record, signal);
        } catch (err) {
          this.log.warn("recovery: restart re-upload failed; retaining source (needs_action)", {
            run_id: record.runId,
            capture_id: record.captureId,
            error: errText(err),
          });
          await this.markNeedsAction(record, "restart_upload_failed");
        }
      }
    }
  }

  /** All AUTHENTICATED records for a run (a tampered/unreadable record is omitted, never
   *  returned as trusted). Used by the restart sweep and by owner/status surfaces. */
  async inspect(runId: string): Promise<RecoveryRecord[]> {
    if (!this.enabled) return [];
    return this.listRecords(runId);
  }

  // ── journal IO + authentication ─────────────────────────────────────────────────

  private runDir(runId: string): string {
    return path.join(this.recoveryRoot, runId);
  }

  private recordPath(record: Pick<RecoveryRecord, "runId" | "captureId">): string {
    return path.join(this.runDir(record.runId), `${record.captureId}.json`);
  }

  private bundlePath(record: Pick<RecoveryRecord, "runId" | "captureId">): string {
    return path.join(this.runDir(record.runId), `${record.captureId}.bundle`);
  }

  /** Atomically write the record with a fresh MAC (0600 file, 0700 dir). */
  private async writeRecord(record: RecoveryRecord): Promise<void> {
    if (!this.key) return;
    const mac = this.computeMac(record);
    const dir = this.runDir(record.runId);
    await fs.mkdir(dir, { recursive: true, mode: 0o700 });
    const dst = this.recordPath(record);
    const tmp = `${dst}.${randomUUID()}.tmp`;
    await fs.writeFile(tmp, JSON.stringify({ ...record, mac }), { mode: 0o600 });
    await fs.rename(tmp, dst);
  }

  /** Read + AUTHENTICATE one record; null on a missing/tampered/unparseable file. */
  private async readRecord(filePath: string): Promise<RecoveryRecord | null> {
    if (!this.key) return null;
    let raw: string;
    try {
      raw = await fs.readFile(filePath, "utf8");
    } catch {
      return null;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      this.log.warn("recovery: unparseable journal record; refusing (needs_action)", { file: filePath });
      return null;
    }
    if (typeof parsed !== "object" || parsed === null) return null;
    const obj = parsed as Record<string, unknown>;
    const mac = obj.mac;
    if (typeof mac !== "string") return null;
    const rest = { ...obj };
    delete rest.mac;
    const record = coerceRecord(rest);
    if (!record) {
      this.log.warn("recovery: malformed journal record; refusing (needs_action)", { file: filePath });
      return null;
    }
    const expected = this.computeMac(record);
    if (!macEqual(mac, expected)) {
      this.log.warn("recovery: journal MAC mismatch; refusing tampered record (needs_action)", {
        file: filePath,
        run_id: record.runId,
      });
      return null;
    }
    return record;
  }

  private computeMac(record: RecoveryRecord): string {
    // key is guaranteed non-undefined by the enabled/callers guard, but re-check for TS.
    if (!this.key) throw new Error("recovery: MAC key unavailable");
    return createHmac("sha256", this.key).update(canonicalJson(record)).digest("hex");
  }

  /** All authenticated records for a run (skips tampered/unreadable files). */
  private async listRecords(runId: string): Promise<RecoveryRecord[]> {
    let names: string[];
    try {
      names = await fs.readdir(this.runDir(runId));
    } catch {
      return [];
    }
    const out: RecoveryRecord[] = [];
    for (const name of names) {
      if (!name.endsWith(".json")) continue;
      const record = await this.readRecord(path.join(this.runDir(runId), name));
      if (record) out.push(record);
    }
    return out;
  }

  private async findRecord(
    runId: string,
    predicate: (r: RecoveryRecord) => boolean,
  ): Promise<RecoveryRecord | undefined> {
    const records = await this.listRecords(runId);
    return records.find(predicate);
  }

  private async markNeedsAction(record: RecoveryRecord, reason: string): Promise<RecoveryOutcome> {
    const updated: RecoveryRecord = { ...record, state: "needs_action", reason };
    await this.writeRecord(updated).catch(() => undefined);
    return { state: "needs_action", captureId: record.captureId, reason };
  }

  private async removeRunDir(runId: string): Promise<void> {
    await fs.rm(this.runDir(runId), { recursive: true, force: true }).catch(() => undefined);
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

async function fileExists(p: string): Promise<boolean> {
  try {
    await fs.access(p);
    return true;
  } catch {
    return false;
  }
}

/** Constant-time compare of two hex MACs of equal length. */
function macEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  const ba = Buffer.from(a, "hex");
  const bb = Buffer.from(b, "hex");
  if (ba.length !== bb.length || ba.length === 0) return false;
  return timingSafeEqual(ba, bb);
}

/** Validate the untrusted parsed object into a RecoveryRecord (shape only; the MAC is the
 *  integrity gate). Returns null on any type mismatch. */
function coerceRecord(obj: Record<string, unknown>): RecoveryRecord | null {
  const str = (v: unknown): v is string => typeof v === "string";
  const num = (v: unknown): v is number => typeof v === "number";
  if (obj.version !== JOURNAL_VERSION) return null;
  if (!str(obj.runId) || !str(obj.captureId) || !str(obj.sourceSha) || !str(obj.kind) || !str(obj.branch)) {
    return null;
  }
  if (!num(obj.createdAt) || !str(obj.state)) return null;
  const state = obj.state;
  if (state !== "pinned" && state !== "bundled" && state !== "uploaded" && state !== "needs_action") {
    return null;
  }
  const prereq = obj.prerequisiteShas;
  if (prereq !== undefined && (!Array.isArray(prereq) || !prereq.every(str))) return null;
  const record: RecoveryRecord = {
    version: JOURNAL_VERSION,
    runId: obj.runId,
    captureId: obj.captureId,
    sourceSha: obj.sourceSha,
    kind: obj.kind as RunKind,
    branch: obj.branch,
    createdAt: obj.createdAt,
    state,
  };
  if (str(obj.attemptedHeadSha)) record.attemptedHeadSha = obj.attemptedHeadSha;
  if (num(obj.generation)) record.generation = obj.generation;
  if (str(obj.bundlePath)) record.bundlePath = obj.bundlePath;
  if (num(obj.byteSize)) record.byteSize = obj.byteSize;
  if (str(obj.checksum)) record.checksum = obj.checksum;
  if (num(obj.chunkCount)) record.chunkCount = obj.chunkCount;
  if (prereq !== undefined) record.prerequisiteShas = prereq as string[];
  if (typeof obj.selfContained === "boolean") record.selfContained = obj.selfContained;
  if (str(obj.serverCaptureId)) record.serverCaptureId = obj.serverCaptureId;
  if (str(obj.reason)) record.reason = obj.reason;
  return record;
}
