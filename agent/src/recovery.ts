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

import { createHash, createHmac, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import { createReadStream } from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";

import { RequestError } from "./client.js";
import type { Logger } from "./log.js";
import { residueQuarantine } from "./residue-quarantine.js";
import { RunDiskLocks } from "./run-disk-locks.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
  RunKind,
} from "./protocol.js";
import { RECOVERY_CHUNK_BYTES, RecoveryBundleTooLargeError, type RecoveryBundleResult } from "./git.js";

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
  /** The exact claim generation this record's custody hold was taken at (PRD #1349 M1/M2,
   *  D1). Populated from `claim.claim_generation` at every pin call site — the early
   *  generation-evidence pin (after clone), the finalization pin, and the park / early-terminal
   *  disposition — so a release or capture targets the ONE hold this generation owns and never
   *  a sibling's. Optional only for a pre-#1296 payload from an older server that omits
   *  `claim_generation`; a v1 record then settles by run+worker rather than by exact generation. */
  generation?: number;
  /** Epoch ms of the pin. */
  createdAt: number;
  state: RecoveryLocalState;
  /** Absolute path of the verified bundle FILE (set at `bundled`). */
  bundlePath?: string;
  /** MAC-covered basename for the transient recovery pin; installed only with fresh bundle bytes. */
  recoveryPinBareDir?: string;
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
  /** issue #1742 D4(a): the BASENAME of the private bare (`bareDirName`) the finalization pin's
   *  `sourceSha` lives in, so the restart sweep can re-resolve it through the git cache. Set only
   *  by the finalization pin. MAC-covered; absent on older records. Never a path. */
  bareDir?: string;
  /** issue #1742 D4(a): the repo default branch the finalization pin was taken against, so the
   *  restart sweep can prove "already published" without a forge fetch. */
  defaultBranch?: string;
  /** issue #1742 D4(a): true ONLY while `sourceSha` is the finalization pin's committed head H.
   *  Every unflagged `pinned` record (the early start-tip pin, and the shutdown / pause /
   *  restore-point pins of `pinRecoveryGeneration`) is never read by a restart as proof of "no
   *  unpublished work" and is never bundled by it. A later non-finalization pin that re-points
   *  `sourceSha` in the same generation CLEARS this flag and `bareDir`/`defaultBranch`, since the
   *  label is only true for the finalization head. */
  finalizationPin?: boolean;
}

/** issue #2213: the outcome of a capture/upload refused because the worker is latched. A
 *  non-uploaded `needs_action` that writes nothing, so every caller keeps the clone and the pin. */
function quarantinedOutcome(captureId: string): RecoveryOutcome {
  return { state: "needs_action", captureId, reason: "worker_quarantined" };
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
  hasFeature?(feature: string): boolean;
  reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse>;
  getRecoveryCaptureStatus(runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse>;
  uploadRecoveryBundle(
    runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
    signal?: AbortSignal,
  ): Promise<RecoveryCaptureStatusResponse>;
  /** Release the run's custody. A v2 caller names the EXACT generation so the server settles
   *  only the hold taken at that claim generation (PRD #1349 M1/M2, D1/D2); an omitted
   *  generation is the v1 settle-by-run+worker fallback. `releaseEvidence` stamps the release's
   *  evidence class (PRD #1392 M1/M2, fact 9), allowlisted server-side; omitted stores NULL. */
  releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse>;
  /** The worker's own open custody holds on a run — the post-clone generation-exact inventory
   *  (PRD #1349 M1/M2, D3). */
  listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse>;
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
  /** Optional best-effort cleanup of the transient pin after durable upload. */
  resolveRecoveryBareDir?(bareDir: string): Promise<string | undefined>;
  deleteRecoveryPin?(barePath: string, runId: string, generation: number): Promise<void>;
  /** issue #1742 D4(a): resolve a journaled bare-dir basename under the private repos root and
   *  classify `sourceSha` against it (see `GitCache.resolveRestartSource`). Optional: a producer
   *  without it makes every finalization-pinned record `source_not_verifiable_after_restart`. */
  resolveRestartSource?(
    bareDir: string,
    sourceSha: string,
    defaultBranch: string,
  ): Promise<{ status: "missing_bare" | "missing_sha" | "on_default" | "unpublished"; barePath?: string }>;
}

export interface PinInput {
  runId: string;
  sourceSha: string;
  kind: RunKind;
  branch: string;
  attemptedHeadSha?: string;
  generation?: number;
  /** issue #1742 D4(a): set ONLY by the finalization pin (see RecoveryRecord). */
  bareDir?: string;
  defaultBranch?: string;
  finalizationPin?: boolean;
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
  terminalRecordProtection?: (runId: string) => Promise<boolean>;
  onAuthoritativeGenerationReleased?: (runId: string, generation: number) => void;
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
  /** issue #1995: the live re-drive's per-pass record cap (default {@link LIVE_MAX_PER_PASS}). */
  liveMaxPerPass?: number;
  /** issue #1995: the live re-drive's pass spacing, and the backoff base after a transient pass. */
  liveBackoffBaseMs?: number;
  /** issue #1995: the ceiling of the live re-drive's exponential backoff. */
  liveBackoffCapMs?: number;
  /** Test seams (never set in production). */
  testHooks?: {
    /** Awaited by {@link RecoveryCoordinator.pin} between its unlocked record observation and
     *  its locked re-read, so a test can interleave a capture there. */
    afterPinObserve?: () => Promise<void>;
  };
}

/** issue #1995: what the worker hands the live re-drive. */
export interface ResumeLiveOptions {
  /** True while the runner is executing `runId` (its flight owns the run's records). */
  isExecuting: (runId: string) => boolean;
  /** The SEND time of the successful heartbeat that triggered this pass. */
  authenticatedAtMs: number;
  signal?: AbortSignal;
}

const LIVE_MAX_PER_PASS = 4;
const LIVE_BACKOFF_BASE_MS = 30_000;
const LIVE_BACKOFF_CAP_MS = 15 * 60_000;

/** Dispositions of a journaled bundle's upload that no retry can change: the api refused the
 *  capture for good, or the local bytes no longer match what was journaled. Written into the
 *  existing `reason` string (never a new record field: an older worker recomputes the MAC from the
 *  fields it knows). Checked at every entry point; a record carrying one is never uploaded again. */
export const PERMANENT_UPLOAD_REASONS: ReadonlySet<string> = new Set([
  "stale_ownership",
  "upload_rejected",
  "archive_constraint",
  "local_bundle_mismatch",
]);

/** The `needs_action` reasons a live pass may retry (all need a journaled bundle). */
const LIVE_RETRY_REASONS: ReadonlySet<string> = new Set([
  "capture_error",
  "restart_upload_failed",
  "upload_transient",
  "credential_rejected",
]);

/** Which entry point is uploading: picks the transient retry reason and the failure log. */
type UploadMode = "capture" | "sweep" | "live";
const TRANSIENT_UPLOAD_REASON: Readonly<Record<UploadMode, string>> = {
  capture: "capture_error",
  sweep: "restart_upload_failed",
  live: "upload_transient",
};

/** A record that journaled a complete verified bundle (file path plus every manifest fact). */
type JournaledBundleRecord = RecoveryRecord & {
  bundlePath: string;
  byteSize: number;
  checksum: string;
  chunkCount: number;
};

function hasJournaledBundle(record: RecoveryRecord): record is JournaledBundleRecord {
  return (
    !!record.bundlePath && typeof record.byteSize === "number" && !!record.checksum && typeof record.chunkCount === "number"
  );
}

/** A record the live pass may re-drive: a journaled bundle that is `bundled`, or `needs_action`
 *  for a transient reason or `credential_rejected` (gated by the caller). Never pinned,
 *  bundle-less, uploaded, or permanent. */
function isLiveCandidate(record: RecoveryRecord): boolean {
  if (!hasJournaledBundle(record)) return false;
  if (record.state === "bundled") return true;
  return record.state === "needs_action" && !!record.reason && LIVE_RETRY_REASONS.has(record.reason);
}

/** True when `current`'s pinned source differs from the one `snapshot`'s bytes were produced from. */
function pinFactsDiffer(snapshot: RecoveryRecord, current: RecoveryRecord): boolean {
  return (
    snapshot.sourceSha !== current.sourceSha ||
    snapshot.finalizationPin !== current.finalizationPin ||
    snapshot.bareDir !== current.bareDir ||
    snapshot.defaultBranch !== current.defaultBranch ||
    snapshot.generation !== current.generation
  );
}

/** Apply a pin to the CURRENT record under the journal lock; returns `current` itself when nothing
 *  changes. Only a `pinned` record may re-point its source or take the finalization facts; any other
 *  state may only fill an absent attempted head or generation (issue #1995). */
function applyPin(current: RecoveryRecord, input: PinInput): RecoveryRecord {
  const next: RecoveryRecord = { ...current };
  let changed = false;
  // Advance the pinned source to the newest restore point for THIS generation (base →
  // committed head), but NEVER re-point a record that already produced or uploaded a
  // bundle: its source is bound to journaled bytes, so moving it would strand them.
  if (current.state === "pinned" && current.sourceSha !== input.sourceSha) {
    next.sourceSha = input.sourceSha;
    changed = true;
    // issue #1742: the finalization label describes the finalization head ONLY. A later
    // non-finalization pin (shutdown / pause / restore-point transfer) that moves the source
    // to a different head must not leave the old label (and its bare/default-branch facts) on it.
    if (input.finalizationPin !== true) {
      delete next.finalizationPin;
      delete next.bareDir;
      delete next.defaultBranch;
    }
  }
  // issue #1742 D4(a): the finalization pin persists the restart-sweep facts on the
  // generation's record. Only while `pinned` (a bundled/uploaded record is bound to
  // journaled bytes and is never altered), and only when it is the finalization pin, so the
  // early pin can never set the flag.
  if (current.state === "pinned" && input.finalizationPin === true && input.bareDir && input.defaultBranch) {
    if (
      next.bareDir !== input.bareDir ||
      next.defaultBranch !== input.defaultBranch ||
      next.finalizationPin !== true
    ) {
      next.bareDir = input.bareDir;
      next.defaultBranch = input.defaultBranch;
      next.finalizationPin = true;
      changed = true;
    }
  }
  // Record the provenance H' once it is known and was not yet recorded.
  if (input.attemptedHeadSha && !next.attemptedHeadSha) {
    next.attemptedHeadSha = input.attemptedHeadSha;
    changed = true;
  }
  // Fill the generation in if an earlier v1-shaped pin created the record without one.
  if (next.generation === undefined && input.generation !== undefined) {
    next.generation = input.generation;
    changed = true;
  }
  return changed ? next : current;
}

// issue #1995: the per-capture cycle lock. A capture cycle (authenticated re-read → short-circuits
// → production + install → reserve + upload → failure marking) is one critical section per record,
// so two entry points can never produce twice or stream concurrently. It is OUTER to the per-run
// journal lock (withJournalLock), which every inner helper takes only briefly. Process memory on
// purpose, like recoveryJournalLocks: one worker process owns each data dir.
const captureCycleTails = new Map<string, Promise<void>>();

/** Run `fn` as the capture cycle of `key`. `wait` queues behind the current holder and returns
 *  `fn`'s value; `skip` returns `{ ran: false }` at once when the cycle is busy. */
function runCaptureCycle<T>(key: string, mode: "wait", fn: () => Promise<T>): Promise<T>;
function runCaptureCycle<T>(
  key: string,
  mode: "skip",
  fn: () => Promise<T>,
): Promise<{ ran: true; value: T } | { ran: false }>;
async function runCaptureCycle<T>(
  key: string,
  mode: "wait" | "skip",
  fn: () => Promise<T>,
): Promise<T | { ran: true; value: T } | { ran: false }> {
  const previous = captureCycleTails.get(key);
  if (mode === "skip" && previous) return { ran: false };
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  // Installed synchronously, so a later waiter queues behind this holder.
  const tail = (previous ?? Promise.resolve()).then(() => held);
  captureCycleTails.set(key, tail);
  await previous;
  try {
    const value = await fn();
    return mode === "skip" ? { ran: true, value } : value;
  } finally {
    release();
    if (captureCycleTails.get(key) === tail) captureCycleTails.delete(key);
  }
}

/** The typed disposition of a failed reserve/upload RPC (issue #1995). */
export type UploadFailureClass =
  | { kind: "transient" }
  | { kind: "permanent"; reason: string }
  | { kind: "credential"; reason: "credential_rejected" };

/** Recompute the size, SHA-256 and chunk count of the journaled bundle file and compare them with
 *  the journaled facts. False on a mismatch or an unreadable file. */
async function verifyJournaledBytes(record: RecoveryRecord): Promise<boolean> {
  if (!hasJournaledBundle(record)) return false;
  try {
    const hash = createHash("sha256");
    let size = 0;
    for await (const chunk of createReadStream(record.bundlePath)) {
      const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string);
      size += buf.length;
      hash.update(buf);
    }
    return (
      size === record.byteSize &&
      hash.digest("hex") === record.checksum &&
      Math.max(1, Math.ceil(size / RECOVERY_CHUNK_BYTES)) === record.chunkCount
    );
  } catch {
    return false;
  }
}

/** The api's typed error reason (`{"error","reason"}`, issue #1995 M1), or undefined. */
function errorReason(err: RequestError): string | undefined {
  try {
    const parsed: unknown = JSON.parse(err.body);
    if (typeof parsed === "object" && parsed !== null) {
      const reason = (parsed as Record<string, unknown>).reason;
      if (typeof reason === "string") return reason;
    }
  } catch {
    // an untyped (or proxy) body: the status is the signal
  }
  return undefined;
}

/**
 * Classify a failed reserve/upload RPC (issue #1995). The body's typed `reason` wins; the status is
 * the fallback for an api without the typed bodies. Permanent: the api refused this capture for good
 * (`stale_ownership`, `upload_rejected`, `archive_constraint`) or the local bytes no longer match the
 * journaled facts (`local_bundle_mismatch`, an integrity rejection the bytes confirm). Credential: a
 * 401. Everything else is transient, including any non-HTTP failure (network, timeout, fs). A 422
 * integrity rejection is transient only when `record`'s journaled bytes still verify.
 */
export async function classifyUploadFailure(err: unknown, record?: RecoveryRecord): Promise<UploadFailureClass> {
  if (!(err instanceof RequestError)) return { kind: "transient" };
  const permanent = (reason: string): UploadFailureClass => ({ kind: "permanent", reason });
  const integrity = async (): Promise<UploadFailureClass> =>
    record && (await verifyJournaledBytes(record)) ? { kind: "transient" } : permanent("local_bundle_mismatch");
  if (err.status === 401) return { kind: "credential", reason: "credential_rejected" };
  switch (errorReason(err)) {
    case "not_authorized":
      return permanent("stale_ownership");
    case "ambiguous_generation":
    case "capture_not_found":
    case "not_available":
    case "manifest_conflict":
    case "bad_request":
      return permanent("upload_rejected");
    case "oversize":
      return permanent("archive_constraint");
    case "integrity":
      return integrity();
    case "quota":
    case "busy":
    case "internal":
      return { kind: "transient" };
    default:
      break;
  }
  switch (err.status) {
    case 400:
    case 403:
    case 404:
    case 409:
      return permanent("upload_rejected");
    case 413:
      return permanent("archive_constraint");
    case 422:
      return integrity();
    default:
      return { kind: "transient" }; // 408, 429, 5xx (incl. 507), anything else
  }
}

// Domain-separation label for the journal-key derivation (D1). Binding a distinct label
// into the derived key keeps this MAC unusable for any other worker-token-keyed purpose.
const JOURNAL_KEY_LABEL = "uzi-recovery-journal-v1";
const JOURNAL_VERSION = 1 as const;

/** Stable, key-sorted JSON so the MAC is deterministic regardless of insertion order. Shared
 *  with the issue #1582 settlement journal (recovery-settlement.ts). */
export function canonicalJson(value: unknown): string {
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

// All coordinators in this worker process share the configured recovery directory. Reuse the
// keyed FIFO mutex for local journal IO only; one worker process owns each data directory.
const recoveryJournalLocks = new RunDiskLocks();

/**
 * The persistent, authenticated durable-recovery coordinator (PRD #1296 M3).
 *
 * Every method is best-effort from the runner's perspective and MUST NOT disturb the run's
 * honest terminal reporting: the caller wraps them, and they also guard internally. The
 * source is protected by the unconditional local pin plus the server custody hold, so an
 * upload can safely happen after (or across a restart from) the terminal report.
 */
export class RecoveryCoordinator {
  private readonly terminalRecordProtection: RecoveryCoordinatorOptions["terminalRecordProtection"];
  private readonly onAuthoritativeGenerationReleased: RecoveryCoordinatorOptions["onAuthoritativeGenerationReleased"];
  private readonly client: RecoveryArchiveClient;
  private readonly git: RecoveryBundleProducer;
  private readonly log: Logger;
  private readonly recoveryRoot: string;
  private readonly now: () => number;
  private readonly key: Buffer | undefined;
  readonly enabled: boolean;
  private readonly liveMaxPerPass: number;
  private readonly liveBackoffBaseMs: number;
  private readonly liveBackoffCapMs: number;
  private readonly testHooks: RecoveryCoordinatorOptions["testHooks"];
  /** In-flight passes (the boot sweep, a live pass): {@link resumeLive} is a no-op while non-empty. */
  private readonly passes = new Set<Promise<void>>();
  /** The earliest time the next live pass may run (the coordinator's `now()` clock). */
  private nextPassAt = 0;
  /** issue #2213: the quarantine skip of a live pass is logged once. */
  private loggedQuarantineSkip = false;
  /** Consecutive live passes that ended with a transient failure (drives the backoff). */
  private transientPasses = 0;
  /** Set by any 401: a live pass runs only for a heartbeat sent after it. */
  private credentialBlockedAt: number | undefined;
  private readonly lastAttemptAt = new Map<string, number>();

  constructor(opts: RecoveryCoordinatorOptions) {
    this.terminalRecordProtection = opts.terminalRecordProtection;
    this.onAuthoritativeGenerationReleased = opts.onAuthoritativeGenerationReleased;
    this.client = opts.client;
    this.git = opts.git;
    this.log = opts.log;
    this.recoveryRoot = opts.recoveryRoot;
    this.now = opts.now ?? (() => Date.now());
    this.liveMaxPerPass = opts.liveMaxPerPass ?? LIVE_MAX_PER_PASS;
    this.liveBackoffBaseMs = opts.liveBackoffBaseMs ?? LIVE_BACKOFF_BASE_MS;
    this.liveBackoffCapMs = opts.liveBackoffCapMs ?? LIVE_BACKOFF_CAP_MS;
    this.testHooks = opts.testHooks;
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
      // Idempotency key (PRD #1349 M2, D1): a v2 worker always carries claim_generation, so ONE
      // record represents the ONE custody hold this run took at that exact generation. The EARLY
      // generation-evidence pin (the restore point the run starts from) and the later
      // finalization / park-terminal disposition pin then UPDATE the same record — its source
      // advances from the start tip to the committed head as the run progresses, so no orphan
      // base record is left behind for the restart sweep to flag needs_action. A v1 worker (no
      // generation) falls back to (runId, sourceSha) matching, exactly as before.
      const matches =
        input.generation !== undefined
          ? (r: RecoveryRecord): boolean => r.generation === input.generation
          : (r: RecoveryRecord): boolean => r.sourceSha === input.sourceSha;
      // The unlocked observation only identifies WHICH record this pin updates; every decision and
      // write below is made on a re-read under the journal lock (issue #1995), so a concurrent
      // capture that bundled or uploaded the record meanwhile is never overwritten with a stale view.
      const observed = await this.findRecord(input.runId, matches);
      await this.testHooks?.afterPinObserve?.();
      return await this.withJournalLock(input.runId, async () => {
        let current: RecoveryRecord | null | undefined;
        if (observed) {
          current = await this.readRecord(this.recordPath(observed));
          // Removed (or no longer authenticating) since the observation: never recreate it.
          if (!current) return undefined;
        } else {
          // Creation re-checks under the lock: a concurrent pin may have created the record already.
          current = (await this.listRecords(input.runId)).find(matches);
        }
        if (current) {
          const next = applyPin(current, input);
          if (next === current) return current;
          await this.writeRecordUnlocked(next);
          return next;
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
          ...(input.finalizationPin === true && input.bareDir && input.defaultBranch
            ? { bareDir: input.bareDir, defaultBranch: input.defaultBranch, finalizationPin: true }
            : {}),
        };
        await this.writeRecordUnlocked(record);
        this.log.info("recovery: pinned source head", {
          run_id: input.runId,
          capture_id: record.captureId,
          finalization_pin: record.finalizationPin === true,
        });
        return record;
      });
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
   *
   * issue #1995: the whole cycle runs under the per-capture lock (a concurrent foreground
   * capture, boot-sweep step or live pass on the same record waits or skips), starts from the
   * authenticated LATEST journaled record rather than the caller's copy, and never re-produces
   * bytes a record already journaled.
   */
  async captureAndUpload(input: CaptureInput): Promise<RecoveryOutcome> {
    if (!this.enabled) return { state: "pinned", captureId: input.record.captureId };
    // issue #2213: a latched worker captures and uploads nothing; the record and pin stay as they are.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(input.record.captureId);
    return runCaptureCycle(this.cycleKey(input.record), "wait", () => this.captureCycle(input));
  }

  /** The capture cycle; the caller holds the record's capture-cycle lock. */
  private async captureCycle(input: CaptureInput): Promise<RecoveryOutcome> {
    let record = input.record;
    // The bundle bytes are dropped only when the record's file is CONFIRMED absent; a record that is
    // present but unreadable keeps them (they may be the only copy) and nothing is written.
    const removed = async (err: RecordGoneError): Promise<RecoveryOutcome> => {
      if (!err.absent) {
        return { state: "needs_action", captureId: record.captureId, reason: "record_unauthenticated" };
      }
      await this.dropBundleFiles(record);
      return { state: "needs_action", captureId: record.captureId, reason: "record_removed" };
    };
    try {
      // The authoritative state is the journal's, never the caller's (possibly stale) copy.
      const latest = await this.readLatest(record);
      if (latest.kind === "gone") {
        return { state: "needs_action", captureId: record.captureId, reason: "record_removed" };
      }
      if (latest.kind === "unauthenticated") {
        return { state: "needs_action", captureId: record.captureId, reason: "record_unauthenticated" };
      }
      record = latest.record;
      if (record.state === "uploaded") return { state: "uploaded", captureId: record.captureId };
      if (record.state === "needs_action" && record.reason && PERMANENT_UPLOAD_REASONS.has(record.reason)) {
        return { state: "needs_action", captureId: record.captureId, reason: record.reason };
      }
      if (input.attemptedHeadSha && !record.attemptedHeadSha) {
        const attempted = input.attemptedHeadSha;
        record = await this.writeExistingRecord(record, (cur) =>
          cur.attemptedHeadSha ? undefined : { ...cur, attemptedHeadSha: attempted },
        );
      }
      if (!hasJournaledBundle(record)) {
        const produced = await this.produceBundle(record, input);
        if (produced.kind === "already_published") {
          // H is already on the fresh forge tip — verified no-unpublished-output (D2/D3).
          // Release THIS generation's exact hold and drop the local journal; nothing to archive.
          // PRD #1392 M1/M2 (fact 9): a proven fresh-forge no-output release stamps forge_no_output.
          // issue #2213: a latch that landed during the fetch must not report "uploaded" (the caller
          // then deletes the pin) while release() is a no-op.
          if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
          await this.release(record.runId, record.generation, "forge_no_output");
          return { state: "uploaded", captureId: record.captureId, reason: "already_published" };
        }
        if (produced.kind !== "bundled") {
          return { state: "needs_action", captureId: record.captureId, reason: produced.reason };
        }
        record = produced.record;
      }
      return await this.uploadJournaledBundle(record, input.signal, "capture");
    } catch (err) {
      if (err instanceof RecordGoneError) return removed(err);
      this.log.warn("recovery: capture/upload failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      try {
        return await this.markFailure(record, "capture_error", true);
      } catch (markErr) {
        if (markErr instanceof RecordGoneError) return removed(markErr);
        throw markErr;
      }
    }
  }

  /** Produce + verify the bundle into a private temp and install it atomically (issue #1995),
   *  journaling the FILE before any manifest binding (D5). Returns the bundled record, an
   *  already-published signal, a needs_action reason, or `source_advanced` when the record's
   *  pinned source moved while the bytes were produced (the bytes are discarded, nothing is
   *  written). The caller holds the record's capture-cycle lock and has just read `record`. */
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
    const tmpPath = this.bundleTmpPath(record);
    await fs.mkdir(path.dirname(tmpPath), { recursive: true, mode: 0o700 });
    let result: RecoveryBundleResult;
    try {
      result = await this.git.produceRecoveryBundle(input.barePath, {
        sourceSha: record.sourceSha,
        outPath: tmpPath,
        forgeTip,
      });
    } catch (err) {
      await fs.rm(tmpPath, { force: true }).catch(() => undefined);
      if (err instanceof RecoveryBundleTooLargeError) {
        this.log.warn("recovery: bundle exceeds the size limit; retaining source (needs_action)", {
          run_id: record.runId,
          byte_size: err.byteSize,
          max_bytes: err.maxBytes,
        });
        await this.markFailure(record, "oversized", true);
        return { kind: "needs_action", reason: "oversized" };
      }
      this.log.warn("recovery: bundle production failed; retaining source (needs_action)", {
        run_id: record.runId,
        error: errText(err),
      });
      await this.markFailure(record, "bundle_failed", true);
      return { kind: "needs_action", reason: "bundle_failed" };
    }
    if (result.alreadyPublished) {
      await fs.rm(tmpPath, { force: true }).catch(() => undefined);
      return { kind: "already_published" };
    }
    const installed = await this.installBundle(record, result, tmpPath, path.basename(input.barePath));
    if (installed.kind === "source_advanced") {
      this.log.warn("recovery: pinned source advanced while the bundle was produced; bytes discarded", {
        run_id: record.runId,
        capture_id: record.captureId,
      });
      return { kind: "needs_action", reason: "source_advanced" };
    }
    this.log.info("recovery: journaled verified bundle before upload", {
      run_id: record.runId,
      capture_id: record.captureId,
      byte_size: installed.record.byteSize,
      self_contained: installed.record.selfContained,
    });
    return { kind: "bundled", record: installed.record };
  }

  /**
   * Install freshly produced bytes (issue #1995): under the per-run journal lock re-read the
   * record, and only if it still names the source `snapshot` was produced from rename the private
   * temp to the final `<captureId>.bundle` and journal the `bundled` record, built from the
   * CURRENT record. A record that is gone or unauthenticated throws {@link RecordGoneError}; a
   * pin that advanced the source discards the bytes (`source_advanced`, nothing written); a bundle
   * journaled meanwhile wins and its facts are returned. The temp never outlives this call.
   */
  private async installBundle(
    snapshot: RecoveryRecord,
    result: RecoveryBundleResult,
    tmpPath: string,
    recoveryPinBareDir?: string,
  ): Promise<{ kind: "bundled"; record: RecoveryRecord } | { kind: "source_advanced" }> {
    const finalPath = this.bundlePath(snapshot);
    try {
      return await this.withJournalLock(snapshot.runId, async () => {
        const cur = await this.requireRecord(snapshot);
        if (pinFactsDiffer(snapshot, cur)) return { kind: "source_advanced" as const };
        if (hasJournaledBundle(cur)) return { kind: "bundled" as const, record: cur };
        await fs.rename(tmpPath, finalPath);
        const bundled: RecoveryRecord = {
          ...cur,
          state: "bundled",
          bundlePath: finalPath,
          ...(recoveryPinBareDir !== undefined ? { recoveryPinBareDir } : {}),
          byteSize: result.byteSize,
          checksum: result.checksum,
          chunkCount: result.chunkCount,
          prerequisiteShas: result.prerequisiteShas,
          selfContained: result.selfContained,
          reason: undefined,
        };
        try {
          await this.writeRecordUnlocked(bundled);
        } catch (err) {
          await fs.rm(finalPath, { force: true }).catch(() => undefined);
          throw err;
        }
        return { kind: "bundled" as const, record: bundled };
      });
    } finally {
      await fs.rm(tmpPath, { force: true }).catch(() => undefined);
    }
  }

  /**
   * Reserve → bind manifest → stream the JOURNALED bundle bytes. Restart-safe: reads the
   * bundle from disk (never re-derives it, never touches a forge PAT). An incomplete local
   * input (missing bundle file / manifest facts) → needs_action, never a fresh forge fetch.
   * Never silently swaps a rebuilt bundle's checksum under a bound capture id — the journaled
   * checksum is the one bound and streamed.
   *
   * issue #1995: every journal write is guarded (a removed record is never recreated, a newer
   * state is never overwritten), the record's existence is re-checked before the reserve and
   * right before the stream opens, and an RPC failure is classified ({@link classifyUploadFailure}):
   * permanent and credential dispositions are journaled as such, a transient one under the mode's
   * retry reason (`capture_error`, `restart_upload_failed`, `upload_transient`). The caller holds
   * the record's capture-cycle lock.
   */
  private async uploadJournaledBundle(
    record: RecoveryRecord,
    signal: AbortSignal | undefined,
    mode: UploadMode,
  ): Promise<RecoveryOutcome> {
    const swallow = mode === "capture";
    // issue #2213: refuse before any journal write too (markFailure below), not just before the client call.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
    if (!hasJournaledBundle(record)) {
      return this.markFailure(record, "incomplete_local_inputs", swallow);
    }
    if (!(await fileExists(record.bundlePath))) {
      // The verified bundle is gone and we may not reproduce it without a forge PAT (D5).
      return this.markFailure(record, "bundle_file_missing", swallow);
    }
    let current: RecoveryRecord = record;
    await this.requirePresent(current);
    // issue #2213: synchronous check immediately before the first client call (the awaits above have
    // returned). Nothing is journaled: the record, its bundle and the pin stay exactly as they are.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(current.captureId);
    if (!current.serverCaptureId) {
      // Reserve once; the local captureId is the idempotency_key so a lost ACK re-reserves the
      // SAME server capture rather than duplicating it.
      let reserved: RecoveryReserveResponse;
      try {
        reserved = await this.client.reserveRecoveryCapture(current.runId, {
          run_id: current.runId,
          idempotency_key: current.captureId,
          source_sha: current.sourceSha,
          ...(current.attemptedHeadSha ? { attempted_head_sha: current.attemptedHeadSha } : {}),
          // Bind the reserve to the EXACT generation's hold (PRD #1349 M2, D1); a v1 record with
          // no generation omits it and the server falls back to the newest-hold reserve.
          ...(current.generation !== undefined ? { generation: current.generation } : {}),
        });
      } catch (err) {
        return this.failUpload(current, err, signal, mode);
      }
      current = await this.writeExistingRecord(current, (cur) => ({ ...cur, serverCaptureId: reserved.capture_id }));
    }
    const manifest: RecoveryUploadManifest = {
      byte_size: current.byteSize!,
      checksum: current.checksum!,
      chunk_count: current.chunkCount!,
      ...(current.prerequisiteShas && current.prerequisiteShas.length > 0
        ? { prerequisite_shas: current.prerequisiteShas }
        : {}),
    };
    // Last custody check before the stream opens: a record removed while the reserve ran is not uploaded.
    await this.requirePresent(current);
    // issue #2213: a latch that landed during the reserve or the check above stops the stream.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(current.captureId);
    let status: RecoveryCaptureStatusResponse;
    try {
      status = await this.client.uploadRecoveryBundle(
        current.runId,
        current.serverCaptureId!,
        manifest,
        createReadStream(current.bundlePath!),
        signal,
      );
    } catch (err) {
      return this.failUpload(current, err, signal, mode);
    }
    // A stream already in flight when cleanup ran cannot be recalled; this guarded write then
    // throws RecordGoneError instead of recreating the removed record.
    const uploaded = await this.writeExistingRecord(current, (cur) => ({ ...cur, state: "uploaded", reason: undefined }));
    await this.cleanupUploadedRecoveryPin(uploaded);
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

  /** One best-effort attempt after the authenticated uploaded write. A crash here may retain
   * the transient ref indefinitely; settlement evidence and journals have their own lifecycle. */
  private async cleanupUploadedRecoveryPin(record: RecoveryRecord): Promise<void> {
    if (!record.recoveryPinBareDir || !this.git.resolveRecoveryBareDir || !this.git.deleteRecoveryPin) return;
    try {
      const barePath = await this.git.resolveRecoveryBareDir(record.recoveryPinBareDir);
      if (!barePath) return;
      await this.git.deleteRecoveryPin(barePath, record.runId, record.generation ?? 0);
    } catch (err) {
      this.log.warn("recovery: uploaded bundle pin cleanup failed", {
        run_id: record.runId,
        error: errText(err).slice(0, 512),
      });
    }
  }

  /** Classify a reserve/upload RPC failure and journal its disposition on the CURRENT record. */
  private async failUpload(
    record: RecoveryRecord,
    err: unknown,
    signal: AbortSignal | undefined,
    mode: UploadMode,
  ): Promise<RecoveryOutcome> {
    // The worker's own abort stops a live pass; it says nothing about the capture or the api.
    if (mode === "live" && signal?.aborted) {
      return { state: record.state, captureId: record.captureId, reason: "aborted" };
    }
    const cls = await classifyUploadFailure(err, record);
    this.log.warn(
      mode === "sweep"
        ? "recovery: restart re-upload failed; retaining source (needs_action)"
        : "recovery: capture/upload failed; retaining source (needs_action)",
      { run_id: record.runId, capture_id: record.captureId, disposition: cls.kind, error: errText(err) },
    );
    if (cls.kind === "credential") this.credentialBlockedAt = this.now();
    const reason = cls.kind === "transient" ? TRANSIENT_UPLOAD_REASON[mode] : cls.reason;
    return this.markFailure(record, reason, mode === "capture");
  }

  /** Throw {@link RecordGoneError} unless the record still exists and authenticates. */
  private async requirePresent(record: RecoveryRecord): Promise<void> {
    await this.requireRecord(record);
  }

  /** The authenticated latest journaled version of `record`, telling a removed record from one
   *  that fails authentication (both read as null). Never falls back to the caller's copy. */
  private async readLatest(
    record: RecoveryRecord,
  ): Promise<{ kind: "ok"; record: RecoveryRecord } | { kind: "gone" } | { kind: "unauthenticated" }> {
    const res = await this.readRecordResult(this.recordPath(record));
    if (res.kind === "ok") return res;
    return res.kind === "absent" ? { kind: "gone" } : { kind: "unauthenticated" };
  }

  /** Drop the bundle file(s) a removed record named (and its canonical path). */
  private async dropBundleFiles(record: RecoveryRecord): Promise<void> {
    await fs.rm(this.bundlePath(record), { force: true }).catch(() => undefined);
    if (record.bundlePath && record.bundlePath !== this.bundlePath(record)) {
      await fs.rm(record.bundlePath, { force: true }).catch(() => undefined);
    }
  }

  // ── D3: release custody on successful full publication ──────────────────────────

  /**
   * Release the run's custody after verified no-unpublished-output for the EXACT generation
   * (a full publication, or a fresh-forge already-published proof). A v2 caller passes the
   * generation so the server settles only the hold taken at that claim generation (PRD #1349
   * M2, D1/D2); an omitted generation is the v1 settle-by-run+worker fallback.
   *
   * Best-effort and idempotent: a failed release is NOT a precondition for reporting completion
   * (a reconciler settles it later, D3), so this never throws to the caller. The local journal
   * is removed ONLY when the server actually released the hold — a server that RETAINED it
   * (v1/ambiguous, `retained`) leaves the source protected, so the record stays for owner
   * attention rather than deleting the only local pointer to it.
   *
   * PRD #1349 M2 (D1) — the local cleanup is GENERATION-SCOPED. M2 stores ONE record per
   * generation (each with its own `<captureId>.json` + `<captureId>.bundle`), and on a same-worker
   * affinity resume a retained sibling generation's record + bundle (possibly the last local copy of
   * that generation's unpublished committed work) coexists with this generation's record in the
   * SAME run dir. So a real release that names an exact generation removes ONLY that generation's
   * record(s) + their bundle files while an authenticated sibling generation remains — wiping the
   * dir would take a sibling generation's work with it. When NO authenticated sibling remains this
   * is the completed-run boundary, so the whole run dir is removed RECURSIVELY, which also sweeps
   * MAC-mismatch/tampered `.json` records, orphan `<captureId>.bundle` files and `*.tmp` leftovers
   * (they would otherwise leak worker disk forever). The v1 fallback (generation omitted, at most
   * one record) removes the whole run dir. Both recursive sweeps are suppressed when physical
   * terminal protection is present or its callback fails; only an empty-dir rmdir is attempted.
   *
   * issue #2213: a latched worker releases no custody (the hold, record and pin stay), with ONE
   * exception: `opts.completedRun`, passed only by the runner's completed-run arm
   * (driveRecoveryTerminal) for every completed code-publishing run, including no-code completions
   * (report_only, not_code, scope-capped-empty) where nothing was published. It is the completed
   * run's custody release, the same one the unlatched flow sends at completion; it is an in-process
   * api call, not a credentialed child, and refusing it would leave the server hold open forever
   * and turn into a false early_pin_only_after_restart needs_action after the restart. The run's
   * clone is kept by executeClaim's finally while latched. Every other caller stays gated.
   */
  async release(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
    opts: { completedRun?: boolean } = {},
  ): Promise<void> {
    if (!this.enabled) return;
    if (residueQuarantine() !== undefined && opts.completedRun !== true) return;
    try {
      const res = await this.client.releaseRecoveryCustody(runId, generation, releaseEvidence);
      this.log.info("recovery: released custody after verified no-unpublished-output", {
        run_id: runId,
        generation,
        release_evidence: releaseEvidence,
        released: res.released,
        holds_released: res.holds_released,
        ...(res.retained ? { retained: res.retained, reason: res.reason } : {}),
      });
      if (!res.retained) {
        if (generation !== undefined) {
          await this.removeGenerationRecords(runId, generation, "sweep_if_last");
        } else {
          await this.withJournalLock(runId, () => this.removeRunDir(runId));
        }
      }
      // Metadata locks have unwound. This is only a hint; the scheduler must GET authority.
      if (res.run_id === runId && res.released === true && !res.retained &&
          generation !== undefined && Number.isSafeInteger(generation) && generation >= 0 &&
          res.generation === generation && this.client.hasFeature?.("recovery_release_exact_echo") === true) {
        this.onAuthoritativeGenerationReleased?.(runId, generation);
      }
    } catch (err) {
      this.log.warn("recovery: custody release failed (a reconciler will retry; completion is unaffected)", {
        run_id: runId,
        error: errText(err),
      });
    }
  }

  // ── D3: post-clone generation-exact prior-hold inventory ─────────────────────────

  /**
   * PRD #1349 M2 (D3): the post-clone, pre-model inventory of THIS run's own open custody
   * holds — exact hold ids + generations, plus each hold's capture state. Purely
   * observational: it NEVER computes `current - 1`, and NEVER releases or transfers a hold by
   * itself. Ancestry settlement of any predecessor generation is deferred to disposition time
   * against the final durable head; same-worker identity is never proof of source continuity.
   * Best-effort: returns [] when recovery is disabled or on any transport failure, so it can
   * never disturb the run's start.
   */
  async inventoryHolds(runId: string): Promise<RecoveryHold[]> {
    if (!this.enabled) return [];
    try {
      const res = await this.client.listRecoveryHolds(runId);
      return res.holds ?? [];
    } catch (err) {
      this.log.warn("recovery: prior-hold inventory failed (custody unchanged)", {
        run_id: runId,
        error: errText(err),
      });
      return [];
    }
  }

  // ── D3/D5: restart-safe sweep ───────────────────────────────────────────────────

  /**
   * issue #1742 D4(a) — snapshot the records the PREVIOUS process left behind. Taken by the worker
   * BEFORE register, so it provably excludes any record a live flight of this process writes
   * later; {@link resumePending} then processes only this snapshot. Authenticated records only
   * (a tampered record is never trusted), `uploaded` ones omitted (nothing to do).
   */
  async snapshotBootRecords(): Promise<RecoveryRecord[]> {
    if (!this.enabled) return [];
    let runDirs: string[];
    try {
      runDirs = await fs.readdir(this.recoveryRoot);
    } catch {
      return [];
    }
    const out: RecoveryRecord[] = [];
    for (const runId of runDirs) {
      for (const record of await this.listRecords(runId)) {
        if (record.state !== "uploaded") out.push(record);
      }
    }
    return out;
  }

  /**
   * On worker restart, dispose of the previous process's journaled records (`snapshot`, from
   * {@link snapshotBootRecords}; taken now when omitted). A record created after the snapshot is
   * never touched. Each record's MAC is verified again on re-read: a tampered/invalid record is
   * refused (left alone), never trusted. Every journal write here first re-checks the record still
   * exists ({@link writeExistingRecord}); a record the live flight's cleanup removed meanwhile is
   * dropped from the sweep, along with any bundle file this sweep produced for it.
   *   - `bundled`, or `needs_action` WITH a journaled bundle: re-upload the bundle BYTE-IDENTICALLY
   *     with NO forge PAT (D5), exactly as before.
   *   - `needs_action` WITHOUT a bundle (a reason a previous sweep or capture recorded, e.g.
   *     `early_pin_only_after_restart`, `source_not_verifiable_after_restart`, `oversized`): never
   *     sent to the upload path (that would rewrite the reason to `incomplete_local_inputs`). Only a
   *     finalization-pinned `source_not_verifiable_after_restart` (transient) is re-evaluated like
   *     `pinned`; any other keeps its reason unchanged (no retry on every boot).
   *   - `pinned` + `finalizationPin` (the committed head H, issue #1742 D4a): resolve the bare from
   *     the journaled basename; a missing bare/commit is `source_not_verifiable_after_restart`; H
   *     already on the default branch is `no_unpublished_work_after_restart` (held untouched, no
   *     bundle); otherwise produce a PAT-less, self-contained bundle of H (no forge tip fetch,
   *     size-capped while it is written), journal it, and upload it at the record's exact
   *     generation.
   *   - `pinned` without the flag: the early start-tip pin, a shutdown / pause / restore-point pin
   *     that moved the source off the finalization head, or an older record that cannot say. All map
   *     to `early_pin_only_after_restart` (the reason name is part of the ADR/spec vocabulary): never
   *     bundled and never read as "no unpublished work".
   * One `recovery restart sweep` log line is written per processed record (ids and outcome only).
   */
  resumePending(signal?: AbortSignal, snapshot?: RecoveryRecord[]): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    // issue #1995: installed SYNCHRONOUSLY (before the first await) so a heartbeat-driven
    // resumeLive can never overlap the boot sweep: it returns at once while any pass is in flight.
    return this.trackPass(() => this.resumePendingPass(signal, snapshot));
  }

  /** Run `fn` as a coordinator pass: {@link resumeLive} is a no-op while any pass is in flight.
   *  `fn` is invoked synchronously, so its in-flight marker exists before any caller can await. */
  private trackPass(fn: () => Promise<void>): Promise<void> {
    const pass = fn();
    this.passes.add(pass);
    return pass.finally(() => {
      this.passes.delete(pass);
    });
  }

  private async resumePendingPass(signal?: AbortSignal, snapshot?: RecoveryRecord[]): Promise<void> {
    const records = snapshot ?? (await this.snapshotBootRecords());
    for (const snap of records) {
      if (signal?.aborted) return;
      // The whole record step runs under the per-capture cycle lock (issue #1995), so a foreground
      // capture or a live pass on the same record cannot interleave with it.
      await runCaptureCycle(this.cycleKey(snap), "wait", () => this.pendingStep(snap, signal));
    }
  }

  /** One boot-sweep record step; the caller holds the record's capture-cycle lock. */
  private async pendingStep(snap: RecoveryRecord, signal?: AbortSignal): Promise<void> {
    // Re-read the current, authenticated state of exactly this snapshotted record.
    const record = await this.readRecord(this.recordPath(snap));
    if (!record || record.state === "uploaded") return;
    const done = (outcome: string, reason?: string): void => {
      this.log.info("recovery restart sweep", {
        run_id: record.runId,
        generation: record.generation,
        outcome,
        ...(reason ? { reason } : {}),
      });
    };
    try {
      if (record.state === "needs_action" && record.reason && PERMANENT_UPLOAD_REASONS.has(record.reason)) {
        // A disposition the api or the local bytes made final: never retried (issue #1995).
        done("needs_action", record.reason);
        return;
      }
      await this.sweepRecord(record, done, signal);
    } catch (err) {
      if (!(err instanceof RecordGoneError)) {
        // One record's failure (e.g. a journal write error) must not abort the rest of the sweep.
        this.log.warn("recovery: restart sweep failed for a record; continuing", {
          run_id: record.runId,
          generation: record.generation,
          error: errText(err),
        });
        return;
      }
      // The record vanished or became unreadable while the sweep worked on it: stop. Only a
      // confirmed-absent record (the live flight's cleanup) drops the bundle bytes; an unreadable
      // one keeps them, since they may be the only copy.
      if (!err.absent) {
        done("needs_action", "record_unauthenticated");
        return;
      }
      await fs.rm(this.bundlePath(record), { force: true }).catch(() => undefined);
      done("record_removed");
    }
  }

  private async sweepRecord(
    record: RecoveryRecord,
    done: (outcome: string, reason?: string) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    if (
      !hasJournaledBundle(record) &&
      (record.state === "pinned" || (record.state === "needs_action" && !record.bundlePath))
    ) {
      // Only the transient `source_not_verifiable_after_restart` is re-evaluated on a later boot;
      // every other bundle-less needs_action reason (oversized, bundle_failed, capture_error,
      // no_unpublished_work_after_restart, ...) is final, so a boot does not retry it forever.
      if (
        record.state === "needs_action" &&
        !(record.finalizationPin && record.reason === "source_not_verifiable_after_restart")
      ) {
        done("needs_action", record.reason); // keep the recorded reason; no rewrite
        return;
      }
      await this.resumePinned(record, done, signal);
      return;
    }
    // A journaled bundle in ANY state is uploaded as journaled, never re-produced (issue #1995).
    try {
      const out = await this.uploadJournaledBundle(record, signal, "sweep");
      done(out.state, out.reason);
    } catch (err) {
      if (err instanceof RecordGoneError) throw err;
      this.log.warn("recovery: restart re-upload failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      const out = await this.markFailure(record, "restart_upload_failed");
      done(out.state, out.reason);
    }
  }

  /** Mark a needs_action reason in the sweep; a record that already carries it is left untouched. */
  private async sweepMark(
    record: RecoveryRecord,
    reason: string,
    done: (outcome: string, reason?: string) => void,
  ): Promise<void> {
    if (record.state === "needs_action" && record.reason === reason && !(await this.recordGone(record))) {
      done("needs_action", reason);
      return;
    }
    const out = await this.markFailure(record, reason);
    done(out.state, out.state === "uploaded" ? undefined : reason);
  }

  private async recordGone(record: RecoveryRecord): Promise<boolean> {
    return (await this.readRecord(this.recordPath(record))) === null;
  }

  /** The `pinned` (and bundle-less finalization `needs_action`) arm of {@link resumePending}. */
  private async resumePinned(
    record: RecoveryRecord,
    done: (outcome: string, reason?: string) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    if (!record.finalizationPin) {
      await this.sweepMark(record, "early_pin_only_after_restart", done);
      return;
    }
    let resolved: Awaited<ReturnType<NonNullable<RecoveryBundleProducer["resolveRestartSource"]>>> | undefined;
    if (record.bareDir && record.defaultBranch && this.git.resolveRestartSource) {
      try {
        resolved = await this.git.resolveRestartSource(record.bareDir, record.sourceSha, record.defaultBranch);
      } catch (err) {
        this.log.warn("recovery: restart source resolution failed", { run_id: record.runId, error: errText(err) });
      }
    }
    if (!resolved || resolved.status === "missing_bare" || resolved.status === "missing_sha" || !resolved.barePath) {
      await this.sweepMark(record, "source_not_verifiable_after_restart", done);
      return;
    }
    if (resolved.status === "on_default") {
      await this.sweepMark(record, "no_unpublished_work_after_restart", done);
      return;
    }
    // Never write a bundle for a record the live flight's cleanup already removed.
    await this.requirePresent(record);
    try {
      // PAT-less and forge-free by design: no fetchDefaultTip, so the bundle is self-contained.
      // Written to a private temp and installed atomically only if the record still names the same
      // source (issue #1995).
      const tmpPath = this.bundleTmpPath(record);
      await fs.mkdir(path.dirname(tmpPath), { recursive: true, mode: 0o700 });
      let result: RecoveryBundleResult;
      try {
        result = await this.git.produceRecoveryBundle(resolved.barePath, {
          sourceSha: record.sourceSha,
          outPath: tmpPath,
          forgeTip: undefined,
        });
      } catch (err) {
        await fs.rm(tmpPath, { force: true }).catch(() => undefined);
        const reason = err instanceof RecoveryBundleTooLargeError ? "oversized" : "bundle_failed";
        await this.sweepMark(record, reason, done);
        return;
      }
      const installed = await this.installBundle(record, result, tmpPath, record.bareDir);
      if (installed.kind === "source_advanced") {
        done("needs_action", "source_advanced");
        return;
      }
      const out = await this.uploadJournaledBundle(installed.record, signal, "sweep");
      done(out.state, out.reason);
    } catch (err) {
      if (err instanceof RecordGoneError) throw err;
      this.log.warn("recovery: restart capture failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      // markFailure marks the CURRENT journaled record (the bundle facts and any reserved server
      // capture id included), so the next boot re-uploads the bundle instead of finding a
      // bundle-less record.
      const out = await this.markFailure(record, "restart_upload_failed");
      done(out.state, out.reason);
    }
  }

  // ── issue #1995: live re-drive of a failed upload (no restart) ──────────────────

  /**
   * Re-drive journaled bundles whose upload failed while the worker stayed alive. Called by the
   * worker after each successful heartbeat (`authenticatedAtMs` is that heartbeat's SEND time).
   * Bounded and cheap to call: it returns at once while any pass (this or the boot sweep) is in
   * flight, before `nextPassAt`, or while a credential rejection is newer than the heartbeat; a
   * pass touches at most `liveMaxPerPass` records, least recently attempted first, and backs off
   * exponentially (capped) after a pass with a transient failure. Only records with a journaled
   * bundle that are `bundled`, or `needs_action` for a transient reason or `credential_rejected`,
   * are candidates; the upload is the journaled-bytes upload (no forge PAT; a journaled bundle is
   * never re-produced). A run that is executing is skipped, and each record step takes the
   * capture-cycle lock in skip mode, so it never waits behind a foreground capture or sweep step
   * on the same record.
   */
  async resumeLive(opts: ResumeLiveOptions): Promise<void> {
    if (!this.enabled || this.passes.size > 0) return;
    // issue #2213: a quarantined worker re-uploads and releases nothing (an unattributable runner-uid
    // process may read credentials); journaled bundles stay put until the worker restarts.
    if (residueQuarantine() !== undefined) {
      if (!this.loggedQuarantineSkip) {
        this.loggedQuarantineSkip = true;
        this.log.warn("recovery: live re-drive skipped; the worker is quarantined");
      }
      return;
    }
    if (this.now() < this.nextPassAt) return;
    if (this.credentialBlockedAt !== undefined && opts.authenticatedAtMs <= this.credentialBlockedAt) return;
    await this.trackPass(() => this.livePass(opts));
  }

  private async livePass(opts: ResumeLiveOptions): Promise<void> {
    let transient = false;
    try {
      const all = await this.snapshotBootRecords();
      // Forget the attempt stamps of captures no longer in the journal listing (bounded map).
      const seen = new Set(all.map((r) => r.captureId));
      for (const id of this.lastAttemptAt.keys()) if (!seen.has(id)) this.lastAttemptAt.delete(id);
      // Executing runs are filtered BEFORE the per-pass cap so they cannot hold its slots.
      const candidates = all
        .filter((r) => isLiveCandidate(r) && !opts.isExecuting(r.runId))
        .sort((a, b) => (this.lastAttemptAt.get(a.captureId) ?? 0) - (this.lastAttemptAt.get(b.captureId) ?? 0))
        .slice(0, this.liveMaxPerPass);
      for (const snap of candidates) {
        if (opts.signal?.aborted) break;
        const res = await runCaptureCycle(this.cycleKey(snap), "skip", () => this.liveStep(snap, opts));
        if (!res.ran) {
          // Cycle busy (a foreground capture or sweep step owns it): rotate it behind its peers so it
          // does not hold a per-pass slot forever.
          this.lastAttemptAt.set(snap.captureId, this.now());
          continue;
        }
        if (res.value === "transient") transient = true;
        if (res.value === "credential") break;
      }
    } catch (err) {
      this.log.warn("recovery: live re-drive pass failed", { error: errText(err) });
      transient = true;
    }
    if (transient) {
      this.transientPasses++;
      this.nextPassAt = this.now() + Math.min(this.liveBackoffBaseMs * 2 ** this.transientPasses, this.liveBackoffCapMs);
    } else {
      this.transientPasses = 0;
      this.nextPassAt = this.now() + this.liveBackoffBaseMs;
    }
  }

  /** One live record step; the caller holds the record's capture-cycle lock. */
  private async liveStep(snap: RecoveryRecord, opts: ResumeLiveOptions): Promise<"ok" | "transient" | "credential"> {
    const log = (outcome: string, reason?: string): void => {
      this.log.info("recovery live re-drive", {
        run_id: snap.runId,
        generation: snap.generation,
        outcome,
        ...(reason ? { reason } : {}),
      });
    };
    const res = await this.readRecordResult(this.recordPath(snap));
    if (res.kind !== "ok") {
      // Nothing is deleted here: a removed record's bytes are already gone with its cleanup, and an
      // unreadable one is retried by a later pass (rotated behind its peers).
      this.lastAttemptAt.set(snap.captureId, this.now());
      log(res.kind === "absent" ? "record_removed" : "record_unauthenticated");
      return "ok";
    }
    const record = res.record;
    // Re-checked inside the lock: the record may have moved on, or the run started executing.
    if (!isLiveCandidate(record) || opts.isExecuting(record.runId)) return "ok";
    this.lastAttemptAt.set(record.captureId, this.now());
    try {
      const out = await this.uploadJournaledBundle(record, opts.signal, "live");
      log(out.state, out.reason);
      if (out.state === "needs_action" && out.reason === "upload_transient") return "transient";
      if (out.state === "needs_action" && out.reason === "credential_rejected") return "credential";
      return "ok";
    } catch (err) {
      if (err instanceof RecordGoneError) {
        // Drop the bytes only for a confirmed-absent record; an unreadable one stays eligible.
        if (err.absent) await this.dropBundleFiles(record);
        log(err.absent ? "record_removed" : "record_unauthenticated");
        return "ok";
      }
      this.log.warn("recovery: live re-upload failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      await this.markFailure(record, "upload_transient").catch(() => undefined);
      log("needs_action", "upload_transient");
      return "transient";
    }
  }

  /**
   * issue #1582 M2 — drop the local journal record(s) + bundle files of ONE predecessor
   * generation whose custody hold the api RELEASED by ancestry settlement. Generation-scoped
   * like a real exact-generation {@link release}: every sibling generation's record stays. Unlike
   * release() it NEVER removes the run dir recursively (issue #1751 M2): it can run while the
   * successor generation is live and writing its own record/bundle/`*.tmp` into the same dir, so it
   * removes only the named generation's files and then attempts a non-recursive rmdir, which leaves
   * any non-empty dir in place. Unauthenticated/orphan leftovers are swept later by the
   * completed-run release(). Local-only (no RPC); the caller invokes it only after a `released`
   * settle outcome.
   */
  async forgetGeneration(runId: string, generation: number): Promise<void> {
    if (!this.enabled) return;
    await this.removeGenerationRecords(runId, generation, "rmdir_if_empty");
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

  /** A private, unique production target (never the final path): bytes are installed over the
   *  final `<captureId>.bundle` only by {@link installBundle}, after a source compare. */
  private bundleTmpPath(record: Pick<RecoveryRecord, "runId" | "captureId">): string {
    return path.join(this.runDir(record.runId), `${record.captureId}.${randomUUID()}.bundle.tmp`);
  }

  /** The per-capture cycle lock key. */
  private cycleKey(record: Pick<RecoveryRecord, "runId" | "captureId">): string {
    return path.resolve(this.recordPath(record));
  }

  private withJournalLock<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    return recoveryJournalLocks.withLock(path.resolve(this.runDir(runId)), fn);
  }

  /** Atomic rename with a fresh MAC (0600 file, 0700 dir). The caller holds this run's journal lock. */
  private async writeRecordUnlocked(record: RecoveryRecord): Promise<void> {
    if (!this.key) return;
    const mac = this.computeMac(record);
    const dir = this.runDir(record.runId);
    await fs.mkdir(dir, { recursive: true, mode: 0o700 });
    const dst = this.recordPath(record);
    const tmp = `${dst}.${randomUUID()}.tmp`;
    await fs.writeFile(tmp, JSON.stringify({ ...record, mac }), { mode: 0o600 });
    await fs.rename(tmp, dst);
  }

  /** Read + AUTHENTICATE one record; null on a missing/tampered/unparseable/unreadable file. */
  private async readRecord(filePath: string): Promise<RecoveryRecord | null> {
    const res = await this.readRecordResult(filePath);
    return res.kind === "ok" ? res.record : null;
  }

  /**
   * Read + authenticate one record, telling a record that is ABSENT (ENOENT: the only outcome that
   * proves it was removed) from one that is present but could not be read or authenticated (EMFILE,
   * EIO, a MAC mismatch, a malformed file). Only `absent` may drive deletion of the bundle bytes the
   * record named: an unreadable record may still be the only pointer to the sole copy.
   */
  private async readRecordResult(
    filePath: string,
  ): Promise<{ kind: "ok"; record: RecoveryRecord } | { kind: "absent" } | { kind: "unreadable" }> {
    if (!this.key) return { kind: "unreadable" };
    let raw: string;
    try {
      raw = await fs.readFile(filePath, "utf8");
    } catch (err) {
      return (err as NodeJS.ErrnoException)?.code === "ENOENT" ? { kind: "absent" } : { kind: "unreadable" };
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      this.log.warn("recovery: unparseable journal record; refusing (needs_action)", { file: filePath });
      return { kind: "unreadable" };
    }
    if (typeof parsed !== "object" || parsed === null) return { kind: "unreadable" };
    const obj = parsed as Record<string, unknown>;
    const mac = obj.mac;
    if (typeof mac !== "string") return { kind: "unreadable" };
    const rest = { ...obj };
    delete rest.mac;
    const record = coerceRecord(rest);
    if (!record) {
      this.log.warn("recovery: malformed journal record; refusing (needs_action)", { file: filePath });
      return { kind: "unreadable" };
    }
    const expected = this.computeMac(record);
    if (!macEqual(mac, expected)) {
      this.log.warn("recovery: journal MAC mismatch; refusing tampered record (needs_action)", {
        file: filePath,
        run_id: record.runId,
      });
      return { kind: "unreadable" };
    }
    return { kind: "ok", record };
  }

  /** The authenticated record at `record`'s path, or a {@link RecordGoneError} carrying whether the
   *  file is confirmed absent. */
  private async requireRecord(record: Pick<RecoveryRecord, "runId" | "captureId">): Promise<RecoveryRecord> {
    const res = await this.readRecordResult(this.recordPath(record));
    if (res.kind !== "ok") throw new RecordGoneError(record, res.kind === "absent");
    return res.record;
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

  /**
   * The guarded post-pin journal write (issue #1995): under the per-run journal lock, re-read
   * the record by path, require it to still authenticate (else {@link RecordGoneError}), apply
   * `mutate` to the CURRENT record (never to an older in-memory copy), and write the result.
   * `mutate` returning undefined writes nothing. Check and rename share the in-process lock
   * cleanup ({@link forgetGeneration} / {@link release}) takes, so a write either lands before
   * the cleanup or refuses the missing record: a removed record is never resurrected, and a
   * newer state (a journaled bundle, `uploaded`) is never overwritten with a stale snapshot.
   * Generalises the restart sweep's #1742 write. Bundling and RPCs stay outside the lock.
   */
  private async writeExistingRecord(
    record: Pick<RecoveryRecord, "runId" | "captureId">,
    mutate: (current: RecoveryRecord) => RecoveryRecord | undefined,
  ): Promise<RecoveryRecord> {
    return this.withJournalLock(record.runId, async () => {
      const current = await this.requireRecord(record);
      const next = mutate(current);
      if (!next) return current;
      await this.writeRecordUnlocked(next);
      return next;
    });
  }

  /**
   * Mark `reason` on the CURRENT journaled record (keeping its bundle facts and server capture
   * id). A record that is gone or fails authentication writes nothing and throws
   * {@link RecordGoneError}; a record the latest journal read shows `uploaded` is never
   * downgraded and surfaces as `uploaded`. `swallow` (the foreground capture) tolerates a
   * journal IO error, as it always has; the record's removal still propagates.
   */
  private async markFailure(
    record: Pick<RecoveryRecord, "runId" | "captureId">,
    reason: string,
    swallow = false,
  ): Promise<RecoveryOutcome> {
    try {
      let alreadyUploaded = false;
      await this.writeExistingRecord(record, (cur) => {
        if (cur.state === "uploaded") {
          alreadyUploaded = true;
          return undefined;
        }
        return { ...cur, state: "needs_action", reason };
      });
      if (alreadyUploaded) return { state: "uploaded", captureId: record.captureId };
      return { state: "needs_action", captureId: record.captureId, reason };
    } catch (err) {
      if (err instanceof RecordGoneError || !swallow) throw err;
      return { state: "needs_action", captureId: record.captureId, reason };
    }
  }

  /** The caller holds this run's journal lock, including for legacy whole-run release. */
  private async removeRunDir(runId: string): Promise<void> {
    let protectedNow = true;
    try {
      protectedNow = await this.terminalRecordProtection?.(runId) ?? false;
    } catch {
      // An unavailable protection inventory retains unknown files.
    }
    if (protectedNow) {
      await fs.rmdir(this.runDir(runId)).catch(() => undefined);
      return;
    }
    await fs.rm(this.runDir(runId), { recursive: true, force: true }).catch(() => undefined);
  }

  /**
   * PRD #1349 M2 (D1) — remove ONLY the released generation's record(s) + their bundle files,
   * leaving every sibling generation's record (and its possibly-last-local-copy bundle) intact.
   * What happens to the run dir afterwards depends on the caller:
   *
   * - `sweep_if_last` (exact-generation {@link release}, the completed-run boundary): when no
   *   authenticated sibling generation remains, the whole run dir is removed RECURSIVELY, sweeping
   *   tampered `.json` records, orphan bundles and `*.tmp` leftovers along with it, unless physical
   *   terminal protection (including an inventory error) requires a non-recursive rmdir instead.
   * - `rmdir_if_empty` ({@link forgetGeneration}, issue #1751 M2 rework N5): runs while the
   *   successor generation is live and may be writing into the same dir concurrently, so the dir
   *   is never removed recursively from a count taken earlier; only a non-recursive rmdir is
   *   attempted, and a non-empty (or already gone) dir is left as is.
   */
  private async removeGenerationRecords(
    runId: string,
    generation: number,
    dirMode: "sweep_if_last" | "rmdir_if_empty",
  ): Promise<void> {
    await this.withJournalLock(runId, () => this.removeGenerationRecordsUnlocked(runId, generation, dirMode));
  }

  /** Generation selection and cleanup run under the same lock as every journal write. */
  private async removeGenerationRecordsUnlocked(
    runId: string,
    generation: number,
    dirMode: "sweep_if_last" | "rmdir_if_empty",
  ): Promise<void> {
    const records = await this.listRecords(runId);
    let remaining = 0;
    for (const record of records) {
      if (record.generation !== generation) {
        remaining++;
        continue;
      }
      await fs.rm(this.recordPath(record), { force: true }).catch(() => undefined);
      // The bundle lives at the canonical <captureId>.bundle path; remove any distinct
      // journaled bundlePath too, so a released generation never leaks its bytes.
      await fs.rm(this.bundlePath(record), { force: true }).catch(() => undefined);
      if (record.bundlePath && record.bundlePath !== this.bundlePath(record)) {
        await fs.rm(record.bundlePath, { force: true }).catch(() => undefined);
      }
    }
    if (dirMode === "sweep_if_last") {
      if (remaining === 0) await this.removeRunDir(runId);
      return;
    }
    // ENOTEMPTY / ENOENT (and any other failure) leave the dir: never a recursive removal here.
    await fs.rmdir(this.runDir(runId)).catch(() => undefined);
  }
}

/** A capture, sweep or live step found its record gone (issue #1742) or unreadable (issue #1995).
 *  `absent` is true only when the record file is CONFIRMED missing; when false the file exists but
 *  could not be read or authenticated, and the bundle bytes it names must be kept. */
class RecordGoneError extends Error {
  constructor(
    record: Pick<RecoveryRecord, "runId" | "captureId">,
    readonly absent: boolean = true,
  ) {
    super(
      absent
        ? `recovery record ${record.captureId} of run ${record.runId} was removed`
        : `recovery record ${record.captureId} of run ${record.runId} could not be read or authenticated`,
    );
    this.name = "RecordGoneError";
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

/** A bare-dir name is a plain basename: non-empty, no path separator, not `.`/`..`, no NUL. The
 *  git cache re-checks this before joining it under its private repos root. */
function isSafeBareDirName(name: string): boolean {
  return (
    name.length > 0 &&
    name !== "." &&
    name !== ".." &&
    !name.includes("/") &&
    !name.includes("\\") &&
    !name.includes("\0")
  );
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
  // issue #1742 D4(a): the restart-sweep facts. A wrongly-typed value refuses the record (like
  // any other shape error); an unsafe bareDir is refused HERE, before it can reach a path join.
  if (obj.recoveryPinBareDir !== undefined) {
    if (!str(obj.recoveryPinBareDir) || !isSafeBareDirName(obj.recoveryPinBareDir)) return null;
    record.recoveryPinBareDir = obj.recoveryPinBareDir;
  }
  if (obj.bareDir !== undefined) {
    if (!str(obj.bareDir) || !isSafeBareDirName(obj.bareDir)) return null;
    record.bareDir = obj.bareDir;
  }
  if (obj.defaultBranch !== undefined) {
    if (!str(obj.defaultBranch)) return null;
    record.defaultBranch = obj.defaultBranch;
  }
  if (obj.finalizationPin !== undefined) {
    if (typeof obj.finalizationPin !== "boolean") return null;
    record.finalizationPin = obj.finalizationPin;
  }
  return record;
}
