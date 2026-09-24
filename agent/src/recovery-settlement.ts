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
 *   - `pending_settle`: the successor's pushed head is persisted; the settle may be (re)sent.
 *   - `terminal`: automatic retries stopped (a terminal reason, or the attempt cap). The record,
 *     its pins and the predecessor's journal are KEPT for owner attention. */
export type SettlementState = "adopted" | "pending_settle" | "terminal";

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
  /** The successor generation's pushed head, persisted BEFORE its completed report is sent. */
  pushedSha?: string;
  disposition?: "publication";
  lastReason?: string;
  attempts: number;
  nextAttemptAt?: number;
}

/** The single settle RPC — WorkerClient satisfies it structurally; a test supplies a fake. */
export interface RecoverySettleClient {
  settleRecoveryHold(runId: string, holdId: string, req: RecoverySettleRequest): Promise<RecoverySettleResponse>;
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
const SHA_RE = /^[0-9a-f]{40}$/;

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

  /** Settle every `pending_settle` record of ONE run now (the post-completion-ACK path). */
  async settleRun(runId: string): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      for (const rec of await this.journal.listRun(runId)) {
        if (rec.state === "pending_settle") await this.settleOne(rec);
      }
    } catch (err) {
      this.log.warn("recovery settlement: settle of a completed run failed (custody unchanged)", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  /** The restart/retry sweep: settle every DUE `pending_settle` record, skipping runs `skipRun`
   *  names (a run whose completion is still an unresolved pending terminal). */
  async sweep(signal?: AbortSignal, skipRun?: (runId: string) => boolean): Promise<void> {
    if (!this.journal.enabled) return;
    try {
      const now = this.journal.now();
      for (const rec of await this.journal.listAll()) {
        if (signal?.aborted) return;
        if (rec.state !== "pending_settle" || !rec.pushedSha) continue;
        if (rec.nextAttemptAt !== undefined && rec.nextAttemptAt > now) continue;
        if (skipRun?.(rec.runId)) continue;
        await this.settleOne(rec);
      }
    } catch (err) {
      this.log.warn("recovery settlement: sweep failed (custody unchanged)", { error: errText(err) });
    }
  }

  async settleOne(rec: SettlementRecord): Promise<SettleResult> {
    if (rec.state !== "pending_settle" || !rec.pushedSha) return "skipped";
    const key = `${rec.runId}/${rec.holdId}`;
    if (this.inflight.has(key)) return "skipped";
    this.inflight.add(key);
    try {
      return await this.settleLocked(rec, rec.pushedSha);
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

  private async settleLocked(rec: SettlementRecord, pushedSha: string): Promise<SettleResult> {
    const req: RecoverySettleRequest = {
      predecessor_generation: rec.predecessorGeneration,
      successor_generation: rec.successorGeneration,
      pushed_sha: pushedSha,
      source_sha: rec.sourceSha,
      adopted_sha: rec.adoptedSha,
    };
    let res: RecoverySettleResponse;
    try {
      res = await this.client.settleRecoveryHold(rec.runId, rec.holdId, req);
    } catch (err) {
      // 429 (per-worker limiter), 404 (an older api without the route), 408, 5xx and any
      // transport error are transient. Any other 4xx is a request the api will never accept.
      if (err instanceof RequestError) {
        const s = err.status;
        if (s === 404 || s === 408 || s === 429 || s >= 500) return this.retry(rec, `http_${s}`);
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

  private async retry(rec: SettlementRecord, reason: string): Promise<SettleResult> {
    const attempts = rec.attempts + 1;
    if (attempts >= SETTLE_MAX_ATTEMPTS) return this.terminal({ ...rec, attempts }, reason);
    await this.journal.put({
      ...rec,
      attempts,
      lastReason: reason,
      nextAttemptAt: this.journal.now() + settleBackoffMs(attempts),
    });
    this.log.info("recovery settlement: predecessor hold retained; retrying later", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      reason,
      attempts,
    });
    return "retry";
  }

  private async terminal(rec: SettlementRecord, reason: string): Promise<SettleResult> {
    await this.journal.put({ ...rec, state: "terminal", lastReason: reason, nextAttemptAt: undefined });
    this.log.warn("recovery settlement: predecessor hold retained; automatic settle stopped (journal + pins kept)", {
      run_id: rec.runId,
      hold_id: rec.holdId,
      reason,
      attempts: rec.attempts,
    });
    return "terminal";
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
  if (o.state !== "adopted" && o.state !== "pending_settle" && o.state !== "terminal") return null;
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
