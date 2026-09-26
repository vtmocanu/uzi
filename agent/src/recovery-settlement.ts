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
//
// issue #1751 M2 adds the LIVE leg: while the successor generation is still running, a confirmed
// publication of its work (a checkpoint publish, or the finalize branch push) lets the worker ask
// `POST .../settle-live` to prove the same candidates against that published tip, so the older hold
// can be released before the run completes. The leg rides the same MAC-covered record and the same
// per-hold lock as the completed path; either path's `released` answer performs the one cleanup.

import { createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";

import { RequestError } from "./client.js";
import type { Logger } from "./log.js";
import type { RecoveryLiveSettleRequest, RecoverySettleRequest, RecoverySettleResponse } from "./protocol.js";
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

/** What a live settle proves the published tip against (issue #1751 M2): the run's checkpoint ref
 *  (`checkpoint`, issue/self_improve runs) or its creation-time branch (`branch`). */
export type LiveSettleTarget = "checkpoint" | "branch";

/** The live-settle leg of a record (issue #1751 M2). `sent` is persisted BEFORE the first request
 *  leaves, so a leg that may have reached the api is never dropped by the record's lifecycle: it
 *  is retried (whatever the record state) until an answer resolves it. */
export interface LiveSettleLeg {
  target: LiveSettleTarget;
  /** The successor generation's published tip, pinned under `refs/uzi-settle/.../published`. */
  publishedSha: string;
  sent: boolean;
  attempts: number;
  nextAttemptAt?: number;
  lastReason?: string;
}

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
  /** issue #1751 M2: the live-settle leg, absent when none (an older record parses unchanged). */
  live?: LiveSettleLeg;
}

/** The settle RPCs — WorkerClient satisfies it structurally; a test supplies a fake. The live
 *  RPC is optional: a client without it records and sends no live leg. */
export interface RecoverySettleClient {
  settleRecoveryHold(
    runId: string,
    holdId: string,
    req: RecoverySettleRequest,
    signal?: AbortSignal,
  ): Promise<RecoverySettleResponse>;
  settleRecoveryHoldLive?(
    runId: string,
    holdId: string,
    req: RecoveryLiveSettleRequest,
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

/** The local git access the live leg needs (issue #1751 M2) — GitCache satisfies it through the
 *  runner's closures; a test supplies a fake. */
export interface SettlementLiveGit {
  /** Whether `ancestor` is an ancestor of (or equal to) `descendant` in the trusted bare. */
  isAncestor(barePath: string, ancestor: string, descendant: string): Promise<boolean>;
  /** Pin `sha` at `refs/uzi-settle/<runId>/<holdId>/published` (false: not pinned). */
  pinPublished(barePath: string, runId: string, holdId: string, sha: string): Promise<boolean>;
  /** Drop ONLY the `published` pin (a live leg cleared without a release). */
  unpinPublished(barePath: string, runId: string, holdId: string): Promise<void>;
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

/** `cleared` (issue #1751 M2): a live leg was dropped on a definitive answer; the record keeps
 *  its state and the completed path is unaffected. */
export type SettleResult = "released" | "retry" | "terminal" | "cleared" | "skipped";

export interface PredecessorSettlerOptions {
  journal: SettlementJournal;
  client: RecoverySettleClient;
  cleanup: SettlementCleanup;
  /** issue #1751 M2: the local git the live leg needs. Absent ⇒ no live leg is ever recorded. */
  liveGit?: SettlementLiveGit;
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

function isDue(nextAttemptAt: number | undefined, now: number): boolean {
  return nextAttemptAt === undefined || nextAttemptAt <= now;
}

/** HTTP statuses a settle (completed or live) treats as transient for the worker: 401/403 (a
 *  rotated join token), 404 (an older api without the route), 408, 429 (the per-worker limiter)
 *  and every 5xx. Any other 4xx is a request the api will never accept. */
function transientStatus(s: number): boolean {
  return s === 401 || s === 403 || s === 404 || s === 408 || s === 429 || s >= 500;
}

/**
 * Drives the settle RPCs and applies the outcome rules. Every method is best-effort: an error is
 * logged and swallowed, and a record is removed ONLY after a `released` answer for exactly its
 * (run, hold) and the local cleanup that answer authorizes.
 *
 * Concurrency (issue #1751 M2): EVERY read-modify-write of a record (the terminal-ACK promotion,
 * supersede, markTerminal, the write-ahead pushed head, adoption evidence, a live publication, and
 * every send, completed or live, with its outcome and released cleanup) runs under that hold's
 * QUEUED lock ({@link withHoldLock}) and re-reads the record under it before writing. A second
 * operation on the same hold WAITS (never skipped), so a promotion is never lost; different holds
 * never block each other.
 */
export class PredecessorSettler {
  private readonly journal: SettlementJournal;
  private readonly client: RecoverySettleClient;
  private readonly cleanup: SettlementCleanup;
  private readonly liveGit: SettlementLiveGit | undefined;
  private readonly log: Logger;
  /** Per-(runId, holdId) queued lock: the tail of that hold's promise chain (deleted when idle). */
  private readonly holdLocks = new Map<string, Promise<void>>();

  constructor(opts: PredecessorSettlerOptions) {
    this.journal = opts.journal;
    this.client = opts.client;
    this.cleanup = opts.cleanup;
    this.liveGit = opts.liveGit;
    this.log = opts.log;
  }

  /**
   * Run `fn` holding the (runId, holdId) lock, keyed exactly like the journal file. An awaiting
   * mutex: a caller queues behind the current holder and runs once it finishes. NOT re-entrant:
   * the `*Locked` helpers run under a held lock and never take it again.
   */
  private async withHoldLock<T>(runId: string, holdId: string, fn: () => Promise<T>): Promise<T> {
    const key = `${runId}/${holdId}`;
    const prev = this.holdLocks.get(key) ?? Promise.resolve();
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const tail = prev.then(() => held);
    this.holdLocks.set(key, tail);
    await prev;
    try {
      return await fn();
    } finally {
      release();
      if (this.holdLocks.get(key) === tail) this.holdLocks.delete(key);
    }
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
   * Pins, the predecessor's journal and any live leg are always kept. Never throws.
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
      for (const snap of await this.journal.listRun(runId)) {
        if (snap.successorGeneration !== successorGeneration) continue;
        await this.withHoldLock(runId, snap.holdId, async () => {
          // Re-read under the lock: the listRun snapshot may be stale (released + removed, or moved
          // on by another transition), and a write from it would resurrect or regress the record.
          const rec = await this.journal.get(runId, snap.holdId);
          if (!rec || rec.successorGeneration !== successorGeneration) return;
          if (rec.state !== "adopted" && rec.state !== "pushed") return;
          if (outcome === "completed" && rec.state === "pushed" && rec.pushedSha) {
            // A live leg rides along unchanged (issue #1751 R1): a sent leg is still resolved.
            if (!(await this.journal.put({ ...rec, state: "pending_settle", nextAttemptAt: undefined }))) return;
            this.log.info("recovery settlement: completion ACK observed; predecessor hold settle-eligible", {
              run_id: runId,
              hold_id: rec.holdId,
              successor_generation: successorGeneration,
            });
            return;
          }
          await this.markTerminalLocked(
            rec,
            outcome === "completed" ? "successor_not_published" : "successor_not_completed",
          );
        });
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
   * goes `terminal` / `superseded` (pins and any live leg kept; the newer generation re-evidences the
   * hold itself if it adopts it). Never throws.
   */
  async supersedeOlderGenerations(runId: string, currentGeneration: number): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      for (const snap of await this.journal.listRun(runId)) {
        if (snap.successorGeneration >= currentGeneration) continue;
        await this.withHoldLock(runId, snap.holdId, async () => {
          const rec = await this.journal.get(runId, snap.holdId);
          if (!rec || rec.successorGeneration >= currentGeneration) return;
          if (rec.state !== "adopted" && rec.state !== "pushed") return;
          await this.markTerminalLocked(rec, "superseded");
        });
      }
    } catch (err) {
      this.log.warn("recovery settlement: supersede of older-generation records failed", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /**
   * Record adoption evidence (a new `adopted` record) under the hold's lock. A record already
   * carrying a SENT live leg is kept as is (its answer may be in flight or lost; the leg resolves
   * it), so false is returned and nothing is written. An unsent leg of a replaced record has its
   * `published` pin dropped. Throws on a journal error (the caller logs it).
   */
  async recordAdoption(record: SettlementRecord): Promise<boolean> {
    if (!this.journal.enabled) return false;
    return this.withHoldLock(record.runId, record.holdId, async () => {
      const existing = await this.journal.get(record.runId, record.holdId);
      if (existing?.live?.sent) {
        this.log.info("recovery settlement: a sent live settle is unresolved for this hold; adoption evidence not replaced", {
          run_id: record.runId,
          hold_id: record.holdId,
        });
        return false;
      }
      if (!(await this.journal.put(record))) return false;
      if (existing?.live) await this.unpinPublished(existing);
      return true;
    });
  }

  /**
   * Persist the successor's pushed head write-ahead of its completed report (`adopted` → `pushed`,
   * disposition `publication`), under the hold's lock and ONLY while the journal still holds an
   * `adopted` record of the same successor generation. Any live leg rides along. Returns whether
   * it wrote. Throws on a journal error (the caller logs it).
   */
  async recordPushedHead(rec: SettlementRecord, pushedSha: string): Promise<boolean> {
    return this.withHoldLock(rec.runId, rec.holdId, async () => {
      const current = await this.journal.get(rec.runId, rec.holdId);
      if (!current || current.state !== "adopted" || current.successorGeneration !== rec.successorGeneration) {
        return false;
      }
      return this.journal.put({ ...current, pushedSha, disposition: "publication", state: "pushed" });
    });
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

  /** The restart/retry sweep: settle every DUE `pending_settle` record, and retry every DUE live
   *  leg whatever its record's state (issue #1751 R1). `adopted`/`pushed` records are never sent
   *  on the completed path: a record only becomes `pending_settle` once its completion ACK was
   *  observed. */
  async sweep(signal?: AbortSignal): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      const now = this.journal.now();
      for (const rec of await this.journal.listAll()) {
        if (signal?.aborted) return;
        if (rec.state === "pending_settle" && rec.pushedSha && isDue(rec.nextAttemptAt, now)) {
          await this.settleOne(rec, signal);
          if (signal?.aborted) return;
        }
        if (rec.live && isDue(rec.live.nextAttemptAt, now)) await this.settleLiveOne(rec.runId, rec.holdId, signal);
      }
    } catch (err) {
      this.log.warn("recovery settlement: sweep failed (custody unchanged)", { error: errText(err) });
    }
  }

  async settleOne(rec: SettlementRecord, signal?: AbortSignal): Promise<SettleResult> {
    if (rec.state !== "pending_settle" || !rec.pushedSha) return "skipped";
    try {
      return await this.withHoldLock(rec.runId, rec.holdId, async () => {
        if (signal?.aborted) return "skipped";
        // Re-read under the lock: a caller's snapshot may be stale (released and removed, retried,
        // or promoted/terminal since it was listed). Only the journal's current copy, unchanged
        // from that snapshot, may be sent.
        const current = await this.journal.get(rec.runId, rec.holdId);
        if (!current || !current.pushedSha || !sameSnapshot(current, rec)) return "skipped";
        return this.settleLocked(current, current.pushedSha, signal);
      });
    } catch (err) {
      this.log.warn("recovery settlement: settle attempt failed locally (custody unchanged)", {
        run_id: rec.runId,
        hold_id: rec.holdId,
        error: errText(err),
      });
      return "skipped";
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
      if (err instanceof RequestError) {
        if (transientStatus(err.status)) return this.retry(rec, `http_${err.status}`);
        return this.terminal(rec, `http_${err.status}`);
      }
      return this.retry(rec, "transport_error");
    }
    const sameHold = res?.run_id === rec.runId && res?.hold_id === rec.holdId;
    if (res?.outcome === "released" && sameHold) {
      await this.releasedCleanupLocked(rec, "completed", res.final_head_sha);
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

  /** The one local cleanup a `released` answer (completed or live) authorizes, run under the
   *  hold's lock: every settlement pin (the live `published` pin included), the predecessor's
   *  recovery pin and journal, then the settlement record LAST. A crash mid-cleanup re-sends the
   *  identical settle, which the api answers `released` again, and the cleanup re-runs. */
  private async releasedCleanupLocked(
    rec: SettlementRecord,
    via: "completed" | "live",
    finalHeadSha: string | undefined,
  ): Promise<void> {
    await this.cleanup.deleteSettlementRefs(rec.barePath, rec.runId, rec.holdId);
    await this.cleanup.deleteRecoveryPin(rec.barePath, rec.runId, rec.predecessorGeneration);
    await this.cleanup.forgetGeneration(rec.runId, rec.predecessorGeneration);
    await this.journal.remove(rec.runId, rec.holdId);
    this.log.info("recovery settlement: predecessor hold released by server-proven ancestry", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      predecessor_generation: rec.predecessorGeneration,
      successor_generation: rec.successorGeneration,
      via,
      final_head_sha: finalHeadSha,
    });
  }

  /** Write `next` ONLY if the journal still holds `orig` unchanged: an attempt never resurrects a
   *  record removed meanwhile, nor overwrites a newer state. The caller holds the hold's lock. */
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
   * Move an `adopted`/`pushed` record terminal (no settle was ever sent for it). Written under the
   * hold's lock, and only if the journal still holds `rec` unchanged, so a caller's stale snapshot
   * never resurrects a record released + removed meanwhile. Returns whether it wrote.
   */
  async markTerminal(rec: SettlementRecord, reason: string): Promise<boolean> {
    return this.withHoldLock(rec.runId, rec.holdId, async () => {
      const current = await this.journal.get(rec.runId, rec.holdId);
      if (!current || !sameSnapshot(current, rec)) return false;
      return this.markTerminalLocked(current, reason);
    });
  }

  /** {@link markTerminal} for a caller that already holds the lock and re-read `rec`. */
  private async markTerminalLocked(rec: SettlementRecord, reason: string): Promise<boolean> {
    const next: SettlementRecord = { ...rec, state: "terminal", lastReason: reason, nextAttemptAt: undefined };
    if (!(await this.journal.put(next))) return false;
    this.logTerminal(next, reason);
    return true;
  }

  /**
   * issue #1751 M2: a confirmed publication of successor generation `successorGeneration`'s work
   * (`publishedSha`, which the api proves against `target`). For each EXISTING record of that run
   * and generation that is still `adopted`, under the hold's lock: check locally that the record's
   * source and adopted tip are ancestors of the published tip in the trusted bare, pin it under
   * `.../published`, and write an unsent live leg. Never creates a record (a released + removed one
   * stays removed) and never touches a `pushed`/`pending_settle`/`terminal` record. An unresolved
   * leg is replaced only by a NEWER publication (one descending its tip). Returns the number of legs
   * written. Never throws.
   */
  async observeLivePublication(
    runId: string,
    successorGeneration: number,
    publishedSha: string,
    target: LiveSettleTarget,
  ): Promise<number> {
    const liveGit = this.liveGit;
    if (!this.journal.enabled || !liveGit || !this.client.settleRecoveryHoldLive) return 0;
    if (!SHA_RE.test(publishedSha) || (target !== "checkpoint" && target !== "branch")) return 0;
    let written = 0;
    try {
      for (const snap of await this.journal.listRun(runId)) {
        if (snap.successorGeneration !== successorGeneration || snap.state !== "adopted") continue;
        const wrote = await this.withHoldLock(runId, snap.holdId, async () => {
          const rec = await this.journal.get(runId, snap.holdId);
          if (!rec || rec.state !== "adopted" || rec.successorGeneration !== successorGeneration) return false;
          const prior = rec.live;
          if (prior) {
            if (prior.publishedSha === publishedSha) return false;
            if (!(await liveGit.isAncestor(rec.barePath, prior.publishedSha, publishedSha))) return false;
          }
          if (!(await liveGit.isAncestor(rec.barePath, rec.sourceSha, publishedSha))) return false;
          if (!(await liveGit.isAncestor(rec.barePath, rec.adoptedSha, publishedSha))) return false;
          if (!(await liveGit.pinPublished(rec.barePath, runId, rec.holdId, publishedSha))) return false;
          if (!(await this.journal.put({ ...rec, live: { target, publishedSha, sent: false, attempts: 0 } }))) {
            return false;
          }
          this.log.info("recovery settlement: live publication recorded for a predecessor hold", {
            run_id: runId,
            hold_id: rec.holdId,
            successor_generation: successorGeneration,
            target,
          });
          return true;
        });
        if (wrote) written += 1;
      }
    } catch (err) {
      this.log.warn("recovery settlement: live publication not recorded (predecessor hold retained)", {
        run_id: runId,
        error: errText(err),
      });
    }
    return written;
  }

  /** issue #1751 M2: send every DUE live leg of ONE run now (the fire-and-forget trigger after a
   *  confirmed publication). Aborts with `signal`; the sweep retries anything left. Never throws. */
  async settleLive(runId: string, signal?: AbortSignal): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      for (const rec of await this.journal.listRun(runId)) {
        if (signal?.aborted) return;
        if (rec.live) await this.settleLiveOne(runId, rec.holdId, signal);
      }
    } catch (err) {
      this.log.warn("recovery settlement: live settle failed (custody unchanged)", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /** One live-leg attempt under the hold's lock: re-read, require a DUE leg, persist `sent`
   *  write-ahead, then send. */
  private async settleLiveOne(runId: string, holdId: string, signal?: AbortSignal): Promise<SettleResult> {
    if (!this.client.settleRecoveryHoldLive) return "skipped";
    try {
      return await this.withHoldLock(runId, holdId, async () => {
        if (signal?.aborted) return "skipped";
        const rec = await this.journal.get(runId, holdId);
        const leg = rec?.live;
        if (!rec || !leg || !isDue(leg.nextAttemptAt, this.journal.now())) return "skipped";
        // Write-ahead: once `sent` is persisted, the leg survives every lifecycle transition of
        // the record until an answer resolves it (issue #1751 R1).
        const sentRec: SettlementRecord = leg.sent ? rec : { ...rec, live: { ...leg, sent: true } };
        if (!leg.sent && !(await this.journal.put(sentRec))) return "skipped";
        return this.settleLiveLocked(sentRec, signal);
      });
    } catch (err) {
      this.log.warn("recovery settlement: live settle attempt failed locally (custody unchanged)", {
        run_id: runId,
        hold_id: holdId,
        error: errText(err),
      });
      return "skipped";
    }
  }

  private async settleLiveLocked(rec: SettlementRecord, signal?: AbortSignal): Promise<SettleResult> {
    const leg = rec.live!;
    const req: RecoveryLiveSettleRequest = {
      predecessor_generation: rec.predecessorGeneration,
      successor_generation: rec.successorGeneration,
      published_sha: leg.publishedSha,
      source_sha: rec.sourceSha,
      adopted_sha: rec.adoptedSha,
      target: leg.target,
    };
    let res: RecoverySettleResponse;
    try {
      res = await this.client.settleRecoveryHoldLive!(rec.runId, rec.holdId, req, signal);
    } catch (err) {
      if (signal?.aborted) return "skipped";
      if (err instanceof RequestError) {
        if (transientStatus(err.status)) return this.retryLive(rec, `http_${err.status}`);
        return this.clearLive(rec, `http_${err.status}`);
      }
      return this.retryLive(rec, "transport_error");
    }
    const sameHold = res?.run_id === rec.runId && res?.hold_id === rec.holdId;
    if (res?.outcome === "released" && sameHold) {
      await this.releasedCleanupLocked(rec, "live", res.final_head_sha);
      return "released";
    }
    if (res?.outcome === "released") return this.retryLive(rec, "response_mismatch");
    const reason = typeof res?.reason === "string" && res.reason !== "" ? res.reason : "unknown";
    if (RETRY_REASONS.has(reason)) return this.retryLive(rec, reason);
    // Any other retained answer is definitive for THIS leg only: the record keeps its state and
    // the completed path still settles it.
    return this.clearLive(rec, reason);
  }

  /** Back the live leg off (attempts++, evidence and pins kept); at the attempt cap the leg is
   *  cleared instead. The caller holds the lock; re-read so a removed record is never resurrected. */
  private async retryLive(rec: SettlementRecord, reason: string): Promise<SettleResult> {
    const leg = rec.live!;
    const attempts = leg.attempts + 1;
    if (attempts >= SETTLE_MAX_ATTEMPTS) return this.clearLive(rec, reason);
    const current = await this.journal.get(rec.runId, rec.holdId);
    if (!current?.live || current.live.publishedSha !== leg.publishedSha) return "skipped";
    const nextAttemptAt = this.journal.now() + settleBackoffMs(attempts);
    const live: LiveSettleLeg = { ...current.live, sent: true, attempts, lastReason: reason, nextAttemptAt };
    if (!(await this.journal.put({ ...current, live }))) return "skipped";
    this.log.info("recovery settlement: live settle retained; retrying later", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      reason,
      attempts,
    });
    return "retry";
  }

  /** Drop ONLY the live leg and its `published` pin; the record keeps its state. The caller holds
   *  the lock; re-read so a removed record is never resurrected. */
  private async clearLive(rec: SettlementRecord, reason: string): Promise<SettleResult> {
    const leg = rec.live!;
    const current = await this.journal.get(rec.runId, rec.holdId);
    if (!current?.live || current.live.publishedSha !== leg.publishedSha) return "skipped";
    if (!(await this.journal.put({ ...current, live: undefined }))) return "skipped";
    await this.unpinPublished(current);
    this.log.warn("recovery settlement: live settle stopped for this publication (record kept)", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      state: current.state,
      reason,
    });
    return "cleared";
  }

  private async unpinPublished(rec: SettlementRecord): Promise<void> {
    await this.liveGit?.unpinPublished(rec.barePath, rec.runId, rec.holdId).catch((err: unknown) => {
      this.log.warn("recovery settlement: published pin not removed", {
        run_id: rec.runId,
        hold_id: rec.holdId,
        error: errText(err),
      });
    });
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
  if (o.live !== undefined) {
    const live = coerceLiveLeg(o.live);
    if (!live) return null;
    rec.live = live;
  }
  return rec;
}

/** Validate an untrusted live leg (issue #1751 M2); null on any mismatch. */
function coerceLiveLeg(v: unknown): LiveSettleLeg | null {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return null;
  const o = v as Record<string, unknown>;
  const int = (x: unknown): x is number => typeof x === "number" && Number.isSafeInteger(x);
  if (o.target !== "checkpoint" && o.target !== "branch") return null;
  if (typeof o.publishedSha !== "string" || !SHA_RE.test(o.publishedSha)) return null;
  if (typeof o.sent !== "boolean" || !int(o.attempts)) return null;
  const leg: LiveSettleLeg = { target: o.target, publishedSha: o.publishedSha, sent: o.sent, attempts: o.attempts };
  if (o.nextAttemptAt !== undefined) {
    if (!int(o.nextAttemptAt)) return null;
    leg.nextAttemptAt = o.nextAttemptAt;
  }
  if (o.lastReason !== undefined) {
    if (typeof o.lastReason !== "string") return null;
    leg.lastReason = o.lastReason;
  }
  return leg;
}
