// Ancestry settlement of an OLDER-generation custody hold (issue #1582 M2, worker side).
//
// A same-worker successor generation that ADOPTED a predecessor generation's work (a reseed off
// the worker tracking ref, or off this run's own mirrored checkpoint) and then completed has, in
// all likelihood, published that predecessor's work too. The worker cannot prove it: it only
// records CANDIDATE evidence here and asks the api (`POST /runs/{id}/recovery-holds/{holdID}/settle`)
// to prove, through the forge alone, that each candidate is contained in the completed branch head.
// Only a `released` answer permits local cleanup; the worker never sends or computes a verdict.
//
// The journal is authenticated exactly like the recovery journal (recovery.ts): each record carries
// an HMAC over its canonical JSON, keyed by a key DERIVED from the worker join token with its own
// domain-separation label, so a hand-edited record can never redirect a settle or trigger a cleanup.
// It lives in a SIBLING of `recovery/` (`GitCache.recoverySettlementRoot`), so it survives both the
// successor's own exact-generation release (which drops recovery/<runId> once empty) and the
// recovery restart sweep (which treats every entry under recovery/ as a runId). No credential of any
// kind is ever stored: the record holds ids, generations, SHAs, a branch name and a bare path.

import { createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";

import { RequestError } from "./client.js";
import type { Logger } from "./log.js";
import type { RecoverySettleRequest, RecoverySettleResponse } from "./protocol.js";
import { canonicalJson } from "./recovery.js";

/** Local settlement lifecycle.
 *   - `adopted`: the successor adopted the predecessor's work; no pushed head yet (never sent).
 *   - `pushed`: the successor's pushed head is persisted WRITE-AHEAD of its completed report, but
 *     no completion ACK has been observed yet. NEVER sent (neither by the in-run path nor by the
 *     sweep): the api would answer `not_eligible` (terminal) before it has applied the completion.
 *   - `pending_settle`: a completion ACK for the successor generation was observed (status
 *     `completed`, or applied with no status from an older server); the settle may be (re)sent.
 *   - `terminal`: automatic retries stopped (a terminal reason, the attempt cap, or a successor
 *     that can never publish). The record, its pins and the predecessor's journal are KEPT for owner
 *     attention (the owner discard path handles the hold). */
export type SettlementState = "adopted" | "pushed" | "pending_settle" | "terminal";

export interface SettlementRecord {
  version: 1;
  runId: string;
  holdId: string;
  predecessorGeneration: number;
  successorGeneration: number;
  /** The predecessor generation's journaled source (from its MAC-authenticated recovery record). */
  sourceSha: string;
  /** The predecessor recovery record's captureId (provenance). */
  sourceCaptureId: string;
  /** The tip the successor adopted (RunnerClone.baseCommit). */
  adoptedSha: string;
  seededFrom: "tracking" | "checkpoint";
  branch: string;
  barePath: string;
  createdAt: number;
  state: SettlementState;
  /** The successor generation's pushed head, persisted (state `pushed`) BEFORE its completed
   *  report is sent. */
  pushedSha?: string;
  disposition?: "publication";
  lastReason?: string;
  attempts: number;
  nextAttemptAt?: number;
}

/** The single settle RPC — WorkerClient satisfies it structurally; a test supplies a fake. */
export interface RecoverySettleClient {
  settleRecoveryHold(
    runId: string,
    holdId: string,
    req: RecoverySettleRequest,
    signal?: AbortSignal,
  ): Promise<RecoverySettleResponse>;
}

/** The slice of a terminal state report (and of its ACK) the settlement lifecycle reads. */
export interface SettlementTerminalReport {
  status?: string;
}
export interface SettlementTerminalAck {
  applied: boolean;
  status?: string;
  staleClaim?: boolean;
}

/** The local cleanup a `released` settle performs — GitCache + RecoveryCoordinator satisfy it. */
export interface SettlementCleanup {
  deleteSettlementRefs(barePath: string, runId: string, holdId: string): Promise<void>;
  deleteRecoveryPin(barePath: string, runId: string, generation: number): Promise<void>;
  forgetGeneration(runId: string, generation: number): Promise<void>;
}

/** Retained reasons that are transient: retry later with backoff. */
const RETRY_REASONS: ReadonlySet<string> = new Set(["ancestry_unknown", "state_changed"]);
/** Retained reasons that can never succeed on a retry: stop automatic retries, keep everything. */
const TERMINAL_REASONS: ReadonlySet<string> = new Set([
  "not_ancestor",
  "not_eligible",
  "candidate_mismatch",
  "branch_missing",
]);

const SETTLEMENT_KEY_LABEL = "uzi-recovery-settlement-v1";
const SETTLEMENT_VERSION = 1 as const;
const SAFE_ID = /^[A-Za-z0-9_-]{1,128}$/;
const RUN_TERMINAL_STATUSES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

/** Whether `id` is a single safe path component the journal accepts. The runner checks both ids
 *  BEFORE pinning `refs/uzi-settle/...`, so a refused record never leaves an orphan pin (and two
 *  ids that would sanitize to the same ref name never share one). */
export function isSafeSettlementId(id: string): boolean {
  return SAFE_ID.test(id);
}
const SHA_RE = /^[0-9a-f]{40}$/;

/** Terminal reason for a record whose successor completed WITH a branch but whose pushed head the
 *  worker could not record (no bare, unreadable tracking tip, or the `.../pushed` pin failed). */
export const PUSHED_HEAD_UNRECORDED = "pushed_head_unrecorded";

/** Retry backoff base (1 min), cap (6 h) and the attempt cap after which a record goes terminal. */
export const SETTLE_BACKOFF_BASE_MS = 60_000;
const SETTLE_BACKOFF_CAP_MS = 6 * 60 * 60_000;
export const SETTLE_MAX_ATTEMPTS = 24;

/** Exponential backoff for the `attempts`-th failed attempt (1-based): 1m, 2m, 4m, … capped at 6h. */
export function settleBackoffMs(attempts: number): number {
  const exp = Math.max(0, Math.min(attempts - 1, 30));
  return Math.min(SETTLE_BACKOFF_CAP_MS, SETTLE_BACKOFF_BASE_MS * 2 ** exp);
}

export interface SettlementJournalOptions {
  /** `GitCache.recoverySettlementRoot` = join(dataDir, "recovery-settlement"). */
  root: string;
  /** The worker join token — the MAC key is DERIVED from it (never stored). Absent ⇒ disabled. */
  workerToken?: string;
  log: Logger;
  now?: () => number;
}

/** The authenticated, restart-safe settlement journal: one record per (runId, holdId). */
export class SettlementJournal {
  private readonly root: string;
  private readonly key: Buffer | undefined;
  private readonly log: Logger;
  readonly now: () => number;
  readonly enabled: boolean;

  constructor(opts: SettlementJournalOptions) {
    this.root = opts.root;
    this.log = opts.log;
    this.now = opts.now ?? (() => Date.now());
    this.key = opts.workerToken
      ? createHmac("sha256", opts.workerToken).update(SETTLEMENT_KEY_LABEL).digest()
      : undefined;
    this.enabled = this.key !== undefined;
  }

  /** Atomically write (create or replace) a record with a fresh MAC. Returns false (nothing
   *  written) when disabled or when an id is not a single safe path component. */
  async put(record: SettlementRecord): Promise<boolean> {
    if (!this.key) return false;
    if (!SAFE_ID.test(record.runId) || !SAFE_ID.test(record.holdId)) return false;
    const dir = path.join(this.root, record.runId);
    await fs.mkdir(this.root, { recursive: true, mode: 0o700 });
    await fs.mkdir(dir, { recursive: true, mode: 0o700 });
    const dst = path.join(dir, `${record.holdId}.json`);
    const tmp = `${dst}.${randomUUID()}.tmp`;
    await fs.writeFile(tmp, JSON.stringify({ ...record, mac: this.mac(record) }), { mode: 0o600 });
    await fs.rename(tmp, dst);
    return true;
  }

  /** Every AUTHENTICATED record of one run (tampered/unparseable files are refused and logged). */
  async listRun(runId: string): Promise<SettlementRecord[]> {
    if (!this.key || !SAFE_ID.test(runId)) return [];
    let names: string[];
    try {
      names = await fs.readdir(path.join(this.root, runId));
    } catch {
      return [];
    }
    const out: SettlementRecord[] = [];
    for (const name of names.sort()) {
      if (!name.endsWith(".json")) continue;
      const rec = await this.read(runId, name.slice(0, -".json".length));
      if (rec) out.push(rec);
    }
    return out;
  }

  /** Every authenticated record across all runs (the restart/retry sweep's input). */
  async listAll(): Promise<SettlementRecord[]> {
    if (!this.key) return [];
    let runs: string[];
    try {
      runs = await fs.readdir(this.root);
    } catch {
      return [];
    }
    const out: SettlementRecord[] = [];
    for (const runId of runs.sort()) out.push(...(await this.listRun(runId)));
    return out;
  }

  /** The current authenticated record for (runId, holdId), or null when absent/refused. */
  async get(runId: string, holdId: string): Promise<SettlementRecord | null> {
    if (!this.key || !SAFE_ID.test(runId)) return null;
    return this.read(runId, holdId);
  }

  /** Remove one record (and the run dir once empty). Best-effort. */
  async remove(runId: string, holdId: string): Promise<void> {
    if (!SAFE_ID.test(runId) || !SAFE_ID.test(holdId)) return;
    const dir = path.join(this.root, runId);
    await fs.rm(path.join(dir, `${holdId}.json`), { force: true }).catch(() => undefined);
    const left = await fs.readdir(dir).catch(() => [] as string[]);
    if (left.length === 0) await fs.rmdir(dir).catch(() => undefined);
  }

  private async read(runId: string, holdId: string): Promise<SettlementRecord | null> {
    if (!SAFE_ID.test(holdId)) return null;
    const file = path.join(this.root, runId, `${holdId}.json`);
    let raw: string;
    try {
      raw = await fs.readFile(file, "utf8");
    } catch {
      return null;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      this.log.warn("recovery settlement: unparseable journal record; refusing", { file });
      return null;
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      this.log.warn("recovery settlement: malformed journal record; refusing", { file });
      return null;
    }
    const obj = { ...(parsed as Record<string, unknown>) };
    const mac = obj.mac;
    delete obj.mac;
    const record = coerceSettlementRecord(obj);
    if (typeof mac !== "string" || !record) {
      this.log.warn("recovery settlement: malformed journal record; refusing", { file });
      return null;
    }
    if (!macEqual(mac, this.mac(record))) {
      this.log.warn("recovery settlement: journal MAC mismatch; refusing tampered record", { file });
      return null;
    }
    // The MAC binds the content, not the path: refuse a valid record moved under another name.
    if (record.runId !== runId || record.holdId !== holdId) {
      this.log.warn("recovery settlement: journal record path does not match its identity; refusing", { file });
      return null;
    }
    return record;
  }

  private mac(record: SettlementRecord): string {
    if (!this.key) throw new Error("recovery settlement: MAC key unavailable");
    return createHmac("sha256", this.key).update(canonicalJson(record)).digest("hex");
  }
}

export type SettleResult = "released" | "retry" | "terminal" | "skipped";

export interface PredecessorSettlerOptions {
  journal: SettlementJournal;
  client: RecoverySettleClient;
  cleanup: SettlementCleanup;
  log: Logger;
}

/** Whether the journal's current copy still matches the snapshot an attempt was started from. */
function sameSnapshot(a: SettlementRecord, b: SettlementRecord): boolean {
  return (
    a.state === b.state &&
    a.attempts === b.attempts &&
    a.nextAttemptAt === b.nextAttemptAt &&
    a.pushedSha === b.pushedSha &&
    a.predecessorGeneration === b.predecessorGeneration &&
    a.successorGeneration === b.successorGeneration
  );
}

/**
 * Drives the settle RPC for `pending_settle` records and applies the outcome rules. Every method
 * is best-effort: an error is logged and swallowed, and a record is removed ONLY after a `released`
 * answer for exactly its (run, hold) and the local cleanup that answer authorizes.
 */
export class PredecessorSettler {
  private readonly journal: SettlementJournal;
  private readonly client: RecoverySettleClient;
  private readonly cleanup: SettlementCleanup;
  private readonly log: Logger;
  /** Single-flight per (runId, holdId): the live post-ACK settle and the sweep never overlap. */
  private readonly inflight = new Set<string>();

  constructor(opts: PredecessorSettlerOptions) {
    this.journal = opts.journal;
    this.client = opts.client;
    this.cleanup = opts.cleanup;
    this.log = opts.log;
  }

  /**
   * Apply an OBSERVED terminal-report ACK for `successorGeneration` of `runId` to that generation's
   * `adopted`/`pushed` records. Called from every terminal send (the live run's reportState choke
   * point AND the outbox replay sends at boot / after a drain / in the queued-duplicate gate), always
   * BEFORE the caller retires the outbox journal, so a crash between the ACK and this promotion
   * replays the journal and promotes then.
   *   - a stale-claim ACK, a non-terminal report, or an ACK whose outcome is unknown/non-terminal:
   *     nothing changes (the terminal is still pending, or a newer generation owns the run);
   *   - outcome `completed` (ack status, or applied with no status from an older server): `pushed`
   *     → `pending_settle` (settle-eligible); `adopted` (no pushed head was persisted, e.g. a
   *     report_only / not_code completion) → `terminal` / `successor_not_published`;
   *   - outcome `failed` / `cancelled`: both → `terminal` / `successor_not_completed`.
   * Pins and the predecessor's journal are always kept. Never throws.
   */
  async observeTerminalAck(
    runId: string,
    successorGeneration: number,
    report: SettlementTerminalReport,
    ack: SettlementTerminalAck,
  ): Promise<void> {
    if (!this.journal.enabled) return;
    if (report.status === undefined || !RUN_TERMINAL_STATUSES.has(report.status)) return;
    if (ack.staleClaim) return;
    const outcome = ack.status ?? (ack.applied ? report.status : undefined);
    if (outcome === undefined || !RUN_TERMINAL_STATUSES.has(outcome)) return;
    try {
      for (const rec of await this.journal.listRun(runId)) {
        if (rec.successorGeneration !== successorGeneration) continue;
        if (rec.state !== "adopted" && rec.state !== "pushed") continue;
        if (outcome === "completed" && rec.state === "pushed" && rec.pushedSha) {
          // Compare-and-write against the journal's CURRENT copy: the listRun snapshot may be stale
          // (a concurrent release + remove, or another transition), and a stale put would resurrect it.
          if (!(await this.replaceUnlocked(rec, { ...rec, state: "pending_settle", nextAttemptAt: undefined }))) continue;
          this.log.info("recovery settlement: completion ACK observed; predecessor hold settle-eligible", {
            run_id: runId,
            hold_id: rec.holdId,
            successor_generation: successorGeneration,
          });
          continue;
        }
        await this.markTerminal(
          rec,
          outcome === "completed" ? "successor_not_published" : "successor_not_completed",
        );
      }
    } catch (err) {
      this.log.warn("recovery settlement: terminal ACK not applied to the settlement journal", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /**
   * A newer claim generation of `runId` started: every record whose successor generation is OLDER
   * and still `adopted`/`pushed` can never be settled by that successor (it never completed), so it
   * goes `terminal` / `superseded` (pins kept; the newer generation re-evidences the hold itself if
   * it adopts it). Never throws.
   */
  async supersedeOlderGenerations(runId: string, currentGeneration: number): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      for (const rec of await this.journal.listRun(runId)) {
        if (rec.successorGeneration >= currentGeneration) continue;
        if (rec.state !== "adopted" && rec.state !== "pushed") continue;
        await this.markTerminal(rec, "superseded");
      }
    } catch (err) {
      this.log.warn("recovery settlement: supersede of older-generation records failed", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /** Settle every `pending_settle` record of ONE run now (the post-completion path, run AFTER the
   *  terminal outcome resolved). */
  async settleRun(runId: string, signal?: AbortSignal): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      for (const rec of await this.journal.listRun(runId)) {
        if (signal?.aborted) return;
        if (rec.state === "pending_settle") await this.settleOne(rec, signal);
      }
    } catch (err) {
      this.log.warn("recovery settlement: settle of a completed run failed (custody unchanged)", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /** The restart/retry sweep: settle every DUE `pending_settle` record. `adopted`/`pushed` records
   *  are never sent: a record only becomes `pending_settle` once its completion ACK was observed. */
  async sweep(signal?: AbortSignal): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      const now = this.journal.now();
      for (const rec of await this.journal.listAll()) {
        if (signal?.aborted) return;
        if (rec.state !== "pending_settle" || !rec.pushedSha) continue;
        if (rec.nextAttemptAt !== undefined && rec.nextAttemptAt > now) continue;
        await this.settleOne(rec, signal);
      }
    } catch (err) {
      this.log.warn("recovery settlement: sweep failed (custody unchanged)", { error: errText(err) });
    }
  }

  async settleOne(rec: SettlementRecord, signal?: AbortSignal): Promise<SettleResult> {
    if (rec.state !== "pending_settle" || !rec.pushedSha) return "skipped";
    const key = `${rec.runId}/${rec.holdId}`;
    if (this.inflight.has(key)) return "skipped";
    this.inflight.add(key);
    try {
      // Re-read under the single-flight key: a caller's snapshot may be stale (released and
      // removed, retried, or promoted/terminal since it was listed). Only the journal's current
      // copy, unchanged from that snapshot, may be sent.
      const current = await this.journal.get(rec.runId, rec.holdId);
      if (!current || !current.pushedSha || !sameSnapshot(current, rec)) return "skipped";
      return await this.settleLocked(current, current.pushedSha, signal);
    } catch (err) {
      this.log.warn("recovery settlement: settle attempt failed locally (custody unchanged)", {
        run_id: rec.runId,
        hold_id: rec.holdId,
        error: errText(err),
      });
      return "skipped";
    } finally {
      this.inflight.delete(key);
    }
  }

  private async settleLocked(rec: SettlementRecord, pushedSha: string, signal?: AbortSignal): Promise<SettleResult> {
    const req: RecoverySettleRequest = {
      predecessor_generation: rec.predecessorGeneration,
      successor_generation: rec.successorGeneration,
      pushed_sha: pushedSha,
      source_sha: rec.sourceSha,
      adopted_sha: rec.adoptedSha,
    };
    let res: RecoverySettleResponse;
    try {
      res = await this.client.settleRecoveryHold(rec.runId, rec.holdId, req, signal);
    } catch (err) {
      // An aborted attempt (worker shutdown) consumes no attempt: the next life re-sends it.
      if (signal?.aborted) return "skipped";
      // 429 (per-worker limiter), 404 (an older api without the route), 408, 401/403 (a rotated
      // join token is transient for the worker), 5xx and any transport error are transient. Any
      // other 4xx is a request the api will never accept.
      if (err instanceof RequestError) {
        const s = err.status;
        if (s === 401 || s === 403 || s === 404 || s === 408 || s === 429 || s >= 500) {
          return this.retry(rec, `http_${s}`);
        }
        return this.terminal(rec, `http_${s}`);
      }
      return this.retry(rec, "transport_error");
    }
    const sameHold = res?.run_id === rec.runId && res?.hold_id === rec.holdId;
    if (res?.outcome === "released" && sameHold) {
      await this.cleanup.deleteSettlementRefs(rec.barePath, rec.runId, rec.holdId);
      await this.cleanup.deleteRecoveryPin(rec.barePath, rec.runId, rec.predecessorGeneration);
      await this.cleanup.forgetGeneration(rec.runId, rec.predecessorGeneration);
      // Removed LAST: a crash mid-cleanup re-sends the identical settle, which the api answers
      // `released` again, and the cleanup re-runs idempotently.
      await this.journal.remove(rec.runId, rec.holdId);
      this.log.info("recovery settlement: predecessor hold released by server-proven ancestry", {
        run_id: rec.runId,
        hold_id: rec.holdId,
        predecessor_generation: rec.predecessorGeneration,
        successor_generation: rec.successorGeneration,
        final_head_sha: res.final_head_sha,
      });
      return "released";
    }
    if (res?.outcome === "released") return this.retry(rec, "response_mismatch");
    const reason = typeof res?.reason === "string" && res.reason !== "" ? res.reason : "unknown";
    if (TERMINAL_REASONS.has(reason)) return this.terminal(rec, reason);
    // ancestry_unknown / state_changed, and any unrecognised answer, retry (bounded by the cap).
    if (!RETRY_REASONS.has(reason)) {
      this.log.warn("recovery settlement: unrecognised settle answer; retrying later", {
        run_id: rec.runId,
        hold_id: rec.holdId,
        outcome: res?.outcome,
        reason,
      });
    }
    return this.retry(rec, reason);
  }

  /** Write `next` ONLY if the journal still holds `orig` unchanged: an attempt never resurrects a
   *  record removed meanwhile, nor overwrites a newer state. */
  private async replaceIfUnchanged(orig: SettlementRecord, next: SettlementRecord): Promise<boolean> {
    const current = await this.journal.get(orig.runId, orig.holdId);
    if (!current || !sameSnapshot(current, orig)) return false;
    return this.journal.put(next);
  }

  private async retry(rec: SettlementRecord, reason: string): Promise<SettleResult> {
    const attempts = rec.attempts + 1;
    if (attempts >= SETTLE_MAX_ATTEMPTS) return this.terminal(rec, reason, attempts);
    const next = { ...rec, attempts, lastReason: reason, nextAttemptAt: this.journal.now() + settleBackoffMs(attempts) };
    if (!(await this.replaceIfUnchanged(rec, next))) return "skipped";
    this.log.info("recovery settlement: predecessor hold retained; retrying later", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      reason,
      attempts,
    });
    return "retry";
  }

  private async terminal(rec: SettlementRecord, reason: string, attempts = rec.attempts): Promise<SettleResult> {
    const next: SettlementRecord = { ...rec, attempts, state: "terminal", lastReason: reason, nextAttemptAt: undefined };
    if (!(await this.replaceIfUnchanged(rec, next))) return "skipped";
    this.logTerminal(next, reason);
    return "terminal";
  }

  /**
   * Move an `adopted`/`pushed` record terminal (no settle was ever sent for it). Written only if the
   * journal still holds `rec` unchanged and no settle holds its single-flight key, so a caller's
   * stale snapshot never resurrects a record released + removed meanwhile. Returns whether it wrote.
   */
  async markTerminal(rec: SettlementRecord, reason: string): Promise<boolean> {
    const next: SettlementRecord = { ...rec, state: "terminal", lastReason: reason, nextAttemptAt: undefined };
    if (!(await this.replaceUnlocked(rec, next))) return false;
    this.logTerminal(next, reason);
    return true;
  }

  /** {@link replaceIfUnchanged} for a caller that does NOT hold the (runId, holdId) single-flight
   *  key: takes it for the read-compare-write, and skips when a settle attempt already holds it. */
  private async replaceUnlocked(orig: SettlementRecord, next: SettlementRecord): Promise<boolean> {
    const key = `${orig.runId}/${orig.holdId}`;
    if (this.inflight.has(key)) return false;
    this.inflight.add(key);
    try {
      return await this.replaceIfUnchanged(orig, next);
    } finally {
      this.inflight.delete(key);
    }
  }

  private logTerminal(rec: SettlementRecord, reason: string): void {
    this.log.warn("recovery settlement: predecessor hold retained; automatic settle stopped (journal + pins kept)", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      reason,
      attempts: rec.attempts,
    });
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function macEqual(a: string, b: string): boolean {
  if (a.length !== b.length || !/^[0-9a-f]+$/.test(a)) return false;
  const ba = Buffer.from(a, "hex");
  const bb = Buffer.from(b, "hex");
  if (ba.length !== bb.length || ba.length === 0) return false;
  return timingSafeEqual(ba, bb);
}

/** Validate the untrusted parsed object into a SettlementRecord (shape only; the MAC is the
 *  integrity gate). Returns null on any mismatch. */
function coerceSettlementRecord(o: Record<string, unknown>): SettlementRecord | null {
  const str = (v: unknown): v is string => typeof v === "string";
  const int = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v);
  if (o.version !== SETTLEMENT_VERSION) return null;
  if (!str(o.runId) || !str(o.holdId) || !str(o.sourceCaptureId) || !str(o.branch) || !str(o.barePath)) return null;
  if (!str(o.sourceSha) || !SHA_RE.test(o.sourceSha) || !str(o.adoptedSha) || !SHA_RE.test(o.adoptedSha)) return null;
  if (!int(o.predecessorGeneration) || !int(o.successorGeneration) || !int(o.createdAt) || !int(o.attempts)) return null;
  if (o.seededFrom !== "tracking" && o.seededFrom !== "checkpoint") return null;
  if (o.state !== "adopted" && o.state !== "pushed" && o.state !== "pending_settle" && o.state !== "terminal") {
    return null;
  }
  const rec: SettlementRecord = {
    version: SETTLEMENT_VERSION,
    runId: o.runId,
    holdId: o.holdId,
    predecessorGeneration: o.predecessorGeneration,
    successorGeneration: o.successorGeneration,
    sourceSha: o.sourceSha,
    sourceCaptureId: o.sourceCaptureId,
    adoptedSha: o.adoptedSha,
    seededFrom: o.seededFrom,
    branch: o.branch,
    barePath: o.barePath,
    createdAt: o.createdAt,
    state: o.state,
    attempts: o.attempts,
  };
  if (o.pushedSha !== undefined) {
    if (!str(o.pushedSha) || !SHA_RE.test(o.pushedSha)) return null;
    rec.pushedSha = o.pushedSha;
  }
  if (o.disposition !== undefined) {
    if (o.disposition !== "publication") return null;
    rec.disposition = o.disposition;
  }
  if (o.lastReason !== undefined) {
    if (!str(o.lastReason)) return null;
    rec.lastReason = o.lastReason;
  }
  if (o.nextAttemptAt !== undefined) {
    if (!int(o.nextAttemptAt)) return null;
    rec.nextAttemptAt = o.nextAttemptAt;
  }
  return rec;
}
