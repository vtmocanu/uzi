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
import { constants, createReadStream } from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";

import { RUN_KINDS } from "./protocol.js";
import { RequestError, isRunOwnershipLost } from "./client.js";
import type { Logger } from "./log.js";
import { residueQuarantine } from "./residue-quarantine.js";
import { RunDiskLocks } from "./run-disk-locks.js";
import type {
  CompletedPublicationReceipt,
  RecoveryReconcileRequest,
  RecoveryReconcileResponse,
  RecoveryFinalDisposition,
  RunOwnershipResponse,
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
  RunKind,
} from "./protocol.js";
import { readRecoveryBundleHeader, RECOVERY_CHUNK_BYTES, RecoveryBundleTooLargeError, type RecoveryBundleResult, type PositiveOwedCandidateContext, type OwedCandidate, type RecoveryCoverage, type FetchAgentBranchOptions, type TrackingUpdateResult, type GitCache } from "./git.js";

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

/** Completing attempt provenance stored within an authenticated generation record. */
export interface CompletionSource {
  context: PositiveOwedCandidateContext;
  clonePath: string;
  attemptId?: string;
  expectedHead: string;
  roots: Array<{ sha: string; contexts: OwedCandidate["contexts"] }>;
}

/** One authenticated journal record; its MAC covers every field. */
export interface RecoveryRecord {
  version: 1;
  inventoryGuarded?: boolean;
  /** Original disposition source stays separate from the synthetic archive source. */
  originalSourceSha?: string;
  /** Exact tree source used to construct the aggregate, distinct from its synthetic tip. */
  inventoryCurrentSha?: string;
  originalRoots?: Array<{ sha: string; contexts: OwedCandidate["contexts"] }>;
  coverageDigest?: string;
  coverageContext?: PositiveOwedCandidateContext;
  finalRequest?: { disposition: RecoveryFinalDisposition; evidence?: string };
  finalAcknowledged?: boolean;
  /** Completion proof is independent of archive coverage and never covers another generation. */
  completionReceipt?: CompletedPublicationReceipt;
  /** Exact completing attempt provenance, not a pending terminal intent. */
  completionSource?: CompletionSource;
  /** The run this capture belongs to. Uniqueness by run_id (a per-run UUID) prevents a
   *  later run on the same ISSUE from overwriting or adopting this capture (D1). */
  runId: string;
  /** Stable source-journal identity, also the legacy reservation key. Replacement
   *  reservations use reserveIdempotencyKey without changing journal paths or pins. */
  captureId: string;
  /** Authenticated reservation identity; absent on legacy records. */
  reserveIdempotencyKey?: string;
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

/** Status evidence must agree with the authenticated bundle manifest. */
function validInventoryStatus(status: RecoveryCaptureStatusResponse, record: RecoveryRecord): boolean {
  return !!status && status.capture_id === record.serverCaptureId &&
    ["preparing", "uploading", "available", "expired", "discarded", "needs_action"].includes(status.state) &&
    typeof status.manifest_bound === "boolean" &&
    (status.checksum === undefined || (typeof status.checksum === "string" && /^[0-9a-f]{64}$/.test(status.checksum) && status.checksum === record.checksum)) &&
    (status.byte_size === undefined || (Number.isSafeInteger(status.byte_size) && status.byte_size! >= 0 && status.byte_size === record.byteSize)) &&
    (!status.manifest_bound || (status.checksum !== undefined && status.byte_size !== undefined)) &&
    (status.reason === undefined || typeof status.reason === "string") &&
    (status.expires_at === undefined || (typeof status.expires_at === "string" && Number.isFinite(Date.parse(status.expires_at))));
}

function healthyInventoryStatus(status: RecoveryCaptureStatusResponse, record: RecoveryRecord, now: number): boolean {
  return validInventoryStatus(status, record) && status.state === "available" && status.manifest_bound &&
    !status.reason && Date.parse(status.expires_at ?? "") > now;
}

/** Contradictory observed integrity evidence cannot authorize a replacement. */
function replaceableInventoryStatus(status: RecoveryCaptureStatusResponse, record: RecoveryRecord, now: number): boolean {
  if (!validInventoryStatus(status, record) || status.reason === "archive integrity check failed") return false;
  switch (status.state) {
    case "available": return status.manifest_bound && !status.reason && Date.parse(status.expires_at ?? "") <= now;
    case "needs_action": return ["upload_retry_window_exhausted", "storage quota exceeded", "upload failed; retry available"].includes(status.reason ?? "");
    case "preparing":
    case "uploading":
    case "discarded": return !status.reason;
    case "expired": return true;
    default: return false;
  }
}

/** issue #2213: a latched capture writes nothing and retains the clone and pin. */
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
  reconcileRecoveryCapture?(runId: string, captureId: string, req: RecoveryReconcileRequest): Promise<RecoveryReconcileResponse>;
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
    finalDisposition?: RecoveryFinalDisposition,
  ): Promise<RecoveryReleaseResponse>;
  getRunOwnership?(runId: string): Promise<RunOwnershipResponse>;
  /** The worker's own open custody holds on a run — the post-clone generation-exact inventory
   *  (PRD #1349 M1/M2, D3). */
  listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse>;
}

/** The bundle-producer subset {@link RecoveryCoordinator} needs — GitCache satisfies it. */
export interface RecoveryBundleProducer {
  cleanupRecoveryGeneration?: GitCache["cleanupRecoveryGeneration"];
  readInventoryCloneHeads?: GitCache["readInventoryCloneHeads"];
  committedTrackingOwnership?: GitCache["committedTrackingOwnership"];
  ancestry?: GitCache["ancestry"];
  enumerateOwedCandidates?(barePath: string, runId: string): Promise<OwedCandidate[]>;
  discoverOwedCandidates?(): ReturnType<GitCache["discoverOwedCandidates"]>;
  retainCurrentOwedCandidate?(barePath: string, options: FetchAgentBranchOptions): Promise<TrackingUpdateResult>;
  buildRecoveryCoverage?(barePath: string, context: PositiveOwedCandidateContext, roots: string[], currentSha: string): Promise<RecoveryCoverage>;
  produceRecoveryBundle(
    barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; guardedDefaultBranch?: string; maxBytes?: number },
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
  inventoryGuarded?: boolean;
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

const INVENTORY_READ_CAUSES = [
  "attribution_unreadable", "clone_ancestor_invalid", "clone_path_invalid",
  "clone_head_unreadable", "git_or_filesystem_error", "other",
] as const;
const INVENTORY_SOURCE_CHECKS = [
  "execution_tail_present", "boundary_exception", "inventory_read_not_verified",
  "non_linux_host", "live_attempt_path", "process_not_quiescent_before_git",
  "process_not_quiescent_after_git", "worktree_status_dirty", "worktree_status_unreadable",
  "source_identity_changed", "physical_sources_without_boundary",
] as const;
type InventorySourceCheck = typeof INVENTORY_SOURCE_CHECKS[number];
type InventoryReadCause = typeof INVENTORY_READ_CAUSES[number];
export type InventorySourceDiagnostic =
  | { check: "inventory_read_not_verified"; cause: InventoryReadCause }
  | { check: Exclude<InventorySourceCheck, "inventory_read_not_verified"> };
export interface InventorySourceBoundaryContext {
  runId: string;
  generation: number;
  barePath: string;
  reportDiagnostic?: (diagnostic: InventorySourceDiagnostic) => unknown;
}

export function inventoryReadCause(inventory: unknown): InventoryReadCause {
  const cause = (inventory as { cause?: unknown } | undefined)?.cause;
  return INVENTORY_READ_CAUSES.find(known => known === cause) ?? "other";
}

/** First failure wins locally before observer delivery. Observer failures confer no authority. */
export function inventorySourceReporter(context: InventorySourceBoundaryContext): (diagnostic: InventorySourceDiagnostic) => false {
  let reported = false;
  return diagnostic => {
    if (!reported) {
      reported = true;
      try {
        // Promise resolution guards thenable access/setup and consumes rejection without waiting.
        const result = context.reportDiagnostic?.(diagnostic);
        void Promise.resolve(result).catch(() => {});
      } catch { /* Diagnostics must never change the proof's decision. */ }
    }
    return false;
  };
}

function inventorySourceReason(diagnostic?: InventorySourceDiagnostic): string {
  if (!diagnostic) return "inventory_source_not_quiescent";
  return "inventory_source_not_quiescent:" + diagnostic.check +
    (diagnostic.check === "inventory_read_not_verified" ? ":" + diagnostic.cause : "");
}

export interface RecoveryCoordinatorOptions {
  withCompletionSourceBoundary?: (
    source: CompletionSource,
    action: (canDelete: () => boolean) => Promise<void>,
  ) => Promise<"passed" | "retained">;
  withInventorySourceBoundary?: (
    context: InventorySourceBoundaryContext,
    action: (prove: () => Promise<boolean>) => Promise<void>,
  ) => Promise<"passed" | "retained">;
  isExecuting?: (runId: string) => boolean;
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
  /** Delays between the bounded retries of {@link RecoveryCoordinator.settleUnadoptedGuardedGeneration}. */
  unadoptedSettleRetryDelaysMs?: readonly number[];
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

/** Canonical empty inventory digest from api/internal/recovery/final_inventory.go. */
const EMPTY_INVENTORY_DIGEST = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
/** Delays before each retry of the unadopted-generation settled release (bounded: len + 1 attempts). */
const UNADOPTED_SETTLE_RETRY_DELAYS_MS: readonly number[] = [500, 2_000, 8_000];

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
  "storage_quota_exceeded",
  "credential_rejected",
  "inventory_source_not_quiescent",
  "inventory_source_not_quiescent:inventory_read_not_verified",
  ...INVENTORY_SOURCE_CHECKS.filter(check => check !== "inventory_read_not_verified")
    .map(check => "inventory_source_not_quiescent:" + check),
  ...INVENTORY_READ_CAUSES.map(cause => "inventory_source_not_quiescent:inventory_read_not_verified:" + cause),
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

/** Guarded FINAL retries may include uploaded records.
 *  Legacy records need a journaled bundle that is bundled or needs_action for a
 *  transient/credential reason; pinned, bundle-less and permanent legacy records are skipped. */
function isLiveCandidate(record: RecoveryRecord): boolean {
  if (record.inventoryGuarded && (record.finalAcknowledged || record.completionReceipt)) return true;
  if (record.inventoryGuarded && record.coverageDigest && !record.finalAcknowledged) {
    if (record.reason === "inventory_quiescence_breach" || record.reason === "inventory_snapshot_changed") return false;
    return record.state !== "needs_action" || (!!record.reason && LIVE_RETRY_REASONS.has(record.reason));
  }
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
  if (current.coverageDigest || current.completionReceipt) return current; // acknowledged authority is immutable
  const next: RecoveryRecord = { ...current };
  let changed = false;
  if (input.inventoryGuarded === true && current.inventoryGuarded !== true) {
    next.inventoryGuarded = true;
    changed = true;
  }
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
  | { kind: "transient"; reason?: "storage_quota_exceeded" }
  | { kind: "permanent"; reason: string }
  | { kind: "credential"; reason: "credential_rejected" };

/** Recompute the size, SHA-256 and chunk count of the journaled bundle file and compare them with
 *  the journaled facts. False on a mismatch or an unreadable file. */
async function verifyJournaledBytes(record: RecoveryRecord, signal?: AbortSignal): Promise<boolean> {
  if (!hasJournaledBundle(record)) return false;
  try {
    const hash = createHash("sha256");
    let size = 0;
    for await (const chunk of createReadStream(record.bundlePath, { signal })) {
      const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string);
      size += buf.length;
      if (size > record.byteSize) return false;
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
      return { kind: "transient", reason: "storage_quota_exceeded" };
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
  private readonly withInventorySourceBoundary: RecoveryCoordinatorOptions["withInventorySourceBoundary"];
  private readonly withCompletionSourceBoundary: RecoveryCoordinatorOptions["withCompletionSourceBoundary"];
  private readonly isExecuting: (runId: string) => boolean;
  private bootOwed: Awaited<ReturnType<NonNullable<RecoveryBundleProducer["discoverOwedCandidates"]>>> = [];
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
  private readonly unadoptedSettleRetryDelaysMs: readonly number[];
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
    this.withInventorySourceBoundary = opts.withInventorySourceBoundary;
    this.withCompletionSourceBoundary = opts.withCompletionSourceBoundary;
    this.isExecuting = opts.isExecuting ?? (() => false);
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
    this.unadoptedSettleRetryDelaysMs = opts.unadoptedSettleRetryDelaysMs ?? UNADOPTED_SETTLE_RETRY_DELAYS_MS;
    // The MAC key is DERIVED from the (stable) worker join token, so it re-derives
    // identically on restart. A token-less coordinator is disabled.
    this.key = opts.workerToken
      ? createHmac("sha256", opts.workerToken).update(JOURNAL_KEY_LABEL).digest()
      : undefined;
    this.enabled = this.key !== undefined;
  }

  // ── D1: source pin + authenticated journal ─────────────────────────────────────

  /**
   * Pin H into the authenticated local journal without a forge PAT. Creating a new guarded
   * journal requires a fresh exact open-hold read; unavailable or closed custody refuses
   * creation. Updates to existing journals and legacy pins remain local. Reuses an existing
   * verified source record for the same run and claim generation, or for the same run and
   * source SHA when the legacy input has no generation.
   *
   * Returns the record, or undefined when recovery is disabled or the pin failed (the
   * caller treats undefined as "no capture to drive", never as a release authority).
   */
  async pin(input: PinInput): Promise<RecoveryRecord | undefined> {
    if (!this.enabled) return undefined;
    if (!(await this.guardedGeneration(input))) return this.pinCycle(input);
    return runCaptureCycle(await this.cycleKey({ ...input, captureId: "" }), "wait", () => this.pinCycle(input));
  }

  private async pinCycle(input: PinInput): Promise<RecoveryRecord | undefined> {
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
          ? (r: RecoveryRecord): boolean => r.generation === input.generation && !r.coverageDigest
          : (r: RecoveryRecord): boolean => r.sourceSha === input.sourceSha && !r.coverageDigest;
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
        // No surviving journal is permission to reopen a closed/discarded guarded hold.
        // Inventory-protocol creation requires a fresh exact open hold. An indeterminate
        // read retains the source without manufacturing a new journal.
        if ((await this.guardedGeneration(input)) && !(await this.openInventoryHold(input))) return undefined;
        const record: RecoveryRecord = {
          version: JOURNAL_VERSION,
          runId: input.runId,
          captureId: randomUUID(),
          sourceSha: input.sourceSha,
          attemptedHeadSha: input.attemptedHeadSha,
          kind: input.kind,
          branch: input.branch,
          generation: input.generation,
          inventoryGuarded: input.inventoryGuarded,
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

  /** Snapshot every unresolved root of this run under a generation-wide recovery lock.
   * No retry here: a discovery/proof failure retains pins and stops this snapshot. */
  async freezeInventory(input: {
    context: PositiveOwedCandidateContext; currentSha: string; originalSourceSha?: string;
    defaultBranch: string; settledEvidence?: "publication" | "forge_no_output";
    /** Caller proved the local process reaped; permits freezing before the terminal RPC. */
    locallyQuiescent?: boolean;
  }): Promise<RecoveryRecord | undefined> {
    if (!this.enabled) return undefined;
    const { context } = input;
    return runCaptureCycle(await this.cycleKey({ runId: context.runId, generation: context.generation, captureId: "", inventoryGuarded: true }), "wait", async () => {
      if (!this.git.enumerateOwedCandidates || !this.git.buildRecoveryCoverage) throw new Error("inventory Git support unavailable");
      const candidates = await this.git.enumerateOwedCandidates(context.barePath, context.runId);
      const probe = { runId: context.runId, generation: context.generation } as RecoveryRecord;
      if (!input.locallyQuiescent && !(await this.inactiveInventory(probe))) return undefined;
      if (!(await this.openInventoryHold(probe))) return undefined;
      if (input.settledEvidence === "publication") {
        try {
          const own = await this.client.getRunOwnership!(context.runId);
          if (own.status !== "completed" || own.claim_generation !== context.generation || own.inventory_guarded !== true) return undefined;
        } catch { return undefined; }
      }
      if (!validInventoryContext(context, context.runId) ||
          candidates.some(c => !/^[0-9a-f]{40}$/.test(c.sha) || !c.contexts.length ||
            c.contexts.some(producer => !validInventoryContext(producer, context.runId, context.barePath)))) {
        throw new Error("inventory producer context malformed");
      }
      const roots = snapshotRoots(candidates);
      const originalSourceSha = input.originalSourceSha ?? input.currentSha;
      const aggregateRoots = [...new Set([...roots.map(r => r.sha), originalSourceSha])].sort();
      let sourceSha: string, digest: string;
      if (!roots.length && input.settledEvidence) {
        digest = EMPTY_INVENTORY_DIGEST;
        sourceSha = input.currentSha;
      } else {
        const coverage = await this.git.buildRecoveryCoverage(context.barePath, context, aggregateRoots, input.currentSha);
        sourceSha = coverage.sha;
        digest = coverage.fingerprint;
      }
      return this.withJournalLock(context.runId, async () => {
        const records = await this.listRecords(context.runId);
        const matching = records.find(r => r.generation === context.generation && r.inventoryGuarded &&
          r.coverageDigest === digest && r.sourceSha === sourceSha &&
          r.inventoryCurrentSha === input.currentSha && r.originalSourceSha === originalSourceSha &&
          canonicalJson(r.coverageContext) === canonicalJson(context) &&
          canonicalJson(r.originalRoots) === canonicalJson(roots));
        if (matching) return matching;
        const pending = records.find(r => r.generation === context.generation && r.inventoryGuarded &&
          r.finalRequest && !r.finalAcknowledged);
        if (pending) {
          this.log.warn("recovery: inventory_quiescence_breach; pending identity retained", {
            run_id: context.runId, generation: context.generation,
          });
          return undefined;
        }
        const record: RecoveryRecord = {
          version: JOURNAL_VERSION, runId: context.runId, captureId: randomUUID(),
          generation: context.generation, inventoryGuarded: true, sourceSha,
          originalSourceSha, originalRoots: roots, inventoryCurrentSha: input.currentSha,
          coverageDigest: digest, coverageContext: context,
          branch: context.branch, kind: context.kind, createdAt: this.now(), state: "pinned",
          bareDir: path.basename(context.barePath), defaultBranch: input.defaultBranch,
          finalizationPin: true,
          ...(!roots.length && input.settledEvidence ? {
            finalRequest: { disposition: { kind: "settled" as const, coverage_digest: digest }, evidence: input.settledEvidence },
          } : {}),
        };
        await this.writeRecordUnlocked(record);
        return record;
      });
    });
  }

  /**
   * issue #1924: settle the EMPTY guarded hold of a generation that adopted no source. A guarded
   * claim that parked or failed before its clone has no bare, record or pin, so no inventory
   * freeze ever reaches it, and the api keeps a guarded hold open until a final disposition: it
   * would count toward the owner's hold limit and block worker deletion forever. This sends the
   * `settled` final release (empty-inventory digest, `forge_no_output`) for that EXACT generation.
   *
   * Fail closed: the release goes out only when "nothing adopted" is positively proven locally:
   * no journal record (authenticated read) for the generation; the claim repo's bare is either
   * absent (ENOENT) or verified to hold no owed candidate of this run and no retained clone of
   * it. Any unreadable, unknown or non-empty answer keeps the hold. The caller must have sent the
   * park/failure report first: the api refuses the release until the generation has ended. A
   * refusal or transport failure is retried a bounded number of times (the retry delays), and a
   * replay after a lost ACK is an idempotent echo. Never throws.
   */
  async settleUnadoptedGuardedGeneration(input: {
    runId: string; generation: number; barePath: string; signal?: AbortSignal;
  }): Promise<"settled" | "retained"> {
    // issue #2213: this release is the ADR-2213 "pre-clone park's hold release, where no source
    // exists" exemption: it is sent only after a positive local proof that the generation adopted
    // no source (no record, no owed candidate, no clone), so a latched worker loses nothing by it
    // and refusing it would strand the hold. It stays ungated on purpose.
    const { runId, generation, barePath } = input;
    if (!this.enabled || !Number.isSafeInteger(generation) || generation <= 0 ||
        !this.client.hasFeature?.("recovery_inventory_v1")) return "retained";
    try {
      if ((await this.checkedRecords(runId)).some(r => r.generation === generation)) return "retained";
      let bareExists = true;
      try { await fs.lstat(barePath); }
      catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") return "retained";
        bareExists = false;
      }
      if (bareExists) {
        if (!this.git.resolveRecoveryBareDir || !this.git.enumerateOwedCandidates || !this.git.readInventoryCloneHeads ||
            await this.git.resolveRecoveryBareDir(path.basename(barePath)) !== barePath) return "retained";
        const owed = await this.git.enumerateOwedCandidates(barePath, runId);
        if (owed.some(c => c.contexts.some(x => x.runId === runId))) return "retained";
        const clones = await this.git.readInventoryCloneHeads(barePath, runId);
        if (clones.kind !== "verified" || clones.clones.length > 0 || clones.heads.length > 0) return "retained";
      }
    } catch (err) {
      this.log.warn("recovery: unadopted-generation proof unavailable; custody retained", { run_id: runId, generation, error: errText(err) });
      return "retained";
    }
    for (let attempt = 0; ; attempt++) {
      try {
        const open = (await this.guardedHolds(runId)).filter(h => h.generation === generation);
        if (open.length === 0) return "settled"; // already closed (a replayed or concurrent settle)
        if (open.length !== 1) return "retained";
        const ack = await this.client.releaseRecoveryCustody(runId, generation, "forge_no_output",
          { kind: "settled", coverage_digest: EMPTY_INVENTORY_DIGEST });
        if (ack.run_id === runId && ack.generation === generation && ack.released === true &&
            ack.holds_released === 1 && !ack.retained) return "settled";
        this.log.warn("recovery: unadopted-generation release not confirmed; custody retained", { run_id: runId, generation });
        return "retained";
      } catch (err) {
        const delay = this.unadoptedSettleRetryDelaysMs[attempt];
        if (delay === undefined || input.signal?.aborted) {
          this.log.warn("recovery: unadopted-generation release failed; custody retained", { run_id: runId, generation, error: errText(err) });
          return "retained";
        }
        await new Promise<void>(resolve => { const t = setTimeout(resolve, delay); t.unref?.(); });
      }
    }
  }

  private async inactiveInventory(record: RecoveryRecord): Promise<boolean> {
    const generation = record.generation;
    if (this.isExecuting(record.runId) || !this.client.hasFeature?.("recovery_inventory_v1") ||
        !this.client.getRunOwnership || typeof generation !== "number" || !Number.isSafeInteger(generation) || generation <= 0) return false;
    let own;
    try { own = await this.client.getRunOwnership(record.runId); }
    catch (err) {
      if (!isRunOwnershipLost(err, record.runId)) return false;
      // A MAC-authenticated immutable FINAL may only replay; new work needs the exact open hold.
      return !!(record.inventoryGuarded && record.finalRequest) || await this.openInventoryHold(record);
    }
    if (!Number.isSafeInteger(own.claim_generation) || own.claim_generation! < generation) return false;
    // A later claim ends this exact generation, even while the successor is active or unguarded.
    // The MAC-covered original guard and exact hold checks remain the custody authority.
    return own.claim_generation! > generation ||
      (own.inventory_guarded === true &&
       ["completed", "failed", "cancelled", "paused", "recovery_wait", "limit_wait"].includes(own.status));
  }

  /** The run's open guarded holds, strictly validated: only a boolean false flag may exclude a hold, so a
   *  missing or non-boolean flag, a bad generation or a foreign run throws instead of reading as "no
   *  guarded hold". Callers use it where absence would release or unprotect custody. */
  private async guardedHolds(runId: string): Promise<Array<{ generation: number }>> {
    const response = await this.client.listRecoveryHolds(runId);
    if (!response || response.run_id !== runId || !Array.isArray(response.holds)) throw new Error("malformed recovery holds response");
    const out: Array<{ generation: number }> = [];
    for (const h of response.holds as unknown[]) {
      const hold = h as { inventory_guarded?: unknown; generation?: unknown } | null;
      if (typeof hold !== "object" || hold === null || typeof hold.inventory_guarded !== "boolean") throw new Error("malformed recovery hold flag");
      if (hold.inventory_guarded === false) continue;
      if (typeof hold.generation !== "number" || !Number.isSafeInteger(hold.generation) || hold.generation <= 0) throw new Error("malformed guarded hold generation");
      out.push({ generation: hold.generation });
    }
    return out;
  }

  private async openInventoryHold(record: Pick<RecoveryRecord, "runId" | "generation">): Promise<boolean> {
    if (!this.client.hasFeature?.("recovery_inventory_v1") || !Number.isSafeInteger(record.generation) || record.generation! <= 0) return false;
    const response = await this.client.listRecoveryHolds(record.runId);
    return response.run_id === record.runId && Array.isArray(response.holds) &&
      response.holds.filter(h => h.generation === record.generation && h.inventory_guarded === true &&
        typeof h.hold_id === "string" && h.hold_id.length > 0).length === 1;
  }

  private inventorySourceDiagnostics(context: InventorySourceBoundaryContext, captureId?: string) {
    let first: InventorySourceDiagnostic | undefined;
    let logged = false;
    const logRetention = () => {
      if (logged) return;
      logged = true;
      try {
        this.log.warn("recovery: inventory source boundary retained", {
          run_id: context.runId, generation: context.generation,
          ...(captureId ? { capture_id: captureId } : {}),
          source_boundary_reason: first?.check ?? "inventory_source_not_quiescent",
          ...(first?.check === "inventory_read_not_verified" ? { inventory_read_cause: first.cause } : {}),
        });
      } catch { /* Logging cannot affect custody. */ }
    };
    return {
      context: { ...context, reportDiagnostic: (diagnostic: InventorySourceDiagnostic) => {
        if (first || !INVENTORY_SOURCE_CHECKS.includes(diagnostic.check)) return;
        first = diagnostic.check === "inventory_read_not_verified"
          ? { check: diagnostic.check, cause: inventoryReadCause(diagnostic) }
          : { check: diagnostic.check };
        logRetention();
      } },
      reason: () => inventorySourceReason(first),
      retained: logRetention,
    };
  }

  private async inventorySourceBoundary(
    context: InventorySourceBoundaryContext,
    action: (prove: () => Promise<boolean>) => Promise<void>,
  ): Promise<"passed" | "retained"> {
    if (this.withInventorySourceBoundary) return this.withInventorySourceBoundary(context, action);
    // Only positive discovery of zero physical sources permits a callback-free producer.
    const refuse = inventorySourceReporter(context);
    const prove = async () => {
      const inventory = await this.git.readInventoryCloneHeads?.(context.barePath, context.runId);
      if (inventory?.kind !== "verified" || !Array.isArray(inventory.clones))
        return refuse({ check: "inventory_read_not_verified", cause: inventoryReadCause(inventory) });
      if (inventory.clones.length !== 0) return refuse({ check: "physical_sources_without_boundary" });
      return true;
    };
    try {
      if (!await prove()) return "retained";
      await action(prove);
      return "passed";
    } catch (err) {
      refuse({ check: "boundary_exception" });
      throw err;
    }
  }

  private async finalizeInventory(snapshot: RecoveryRecord, reconcile = true): Promise<RecoveryOutcome | void> {
    if (!snapshot.coverageContext || typeof snapshot.generation !== "number") return;
    let outcome: RecoveryOutcome | void = undefined;
    let sourceVerified = true;
    const diagnostics = this.inventorySourceDiagnostics({ ...snapshot.coverageContext, generation: snapshot.generation }, snapshot.captureId);
    const result = await this.inventorySourceBoundary(diagnostics.context, async prove => {
      outcome = await this.finalizeInventoryWithinBoundary(snapshot, async () => {
        sourceVerified = await prove();
        return sourceVerified;
      }, reconcile);
    });
    if (result === "retained" || !sourceVerified) {
      diagnostics.retained();
      const retained = await this.writeExistingRecord(snapshot, cur => ({ ...cur,
        state: cur.state === "uploaded" ? "uploaded" : "needs_action", reason: diagnostics.reason() }));
      return { state: retained.state, captureId: retained.captureId, reason: retained.reason };
    }
    return outcome;
  }

  private async finalizeInventoryWithinBoundary(snapshot: RecoveryRecord, prove: () => Promise<boolean>, reconcile: boolean): Promise<RecoveryOutcome | void> {
    let record = await this.requireRecord(snapshot);
    // issue #2213 (ADR-2213 "Nothing is uploaded or released while latched"): the guarded FINAL is a
    // custody release of an inactive run's inventory. Neither named exemption (a completed run's
    // release, the pre-clone park's source-less hold release) covers it, so a latched worker holds it
    // and keeps the record, bundle and pins; the next unlatched pass replays the same request.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
    if (!record.inventoryGuarded || record.finalAcknowledged || record.reason === "inventory_quiescence_breach" || !record.coverageDigest ||
        !(await this.inactiveInventory(record))) return;
    // Archive FINAL releases local custody only with positive, retained-byte evidence.
    // Settled FINAL has its own independently verified empty-inventory proof.
    if (record.finalRequest?.disposition.kind !== "settled") {
      if (record.selfContained !== true || !Array.isArray(record.prerequisiteShas) ||
          record.prerequisiteShas.length !== 0 || !record.bundlePath) return;
      try {
        const header = await readRecoveryBundleHeader(record.bundlePath, record.sourceSha);
        if (!header.selfContained || header.prerequisiteShas.length !== 0) return;
      } catch { return; }
    }
    // Every still-unresolved root must be covered; an earlier archive cannot close a later inventory.
    if (!record.coverageContext || !this.git.enumerateOwedCandidates) return;
    const unresolved = await this.git.enumerateOwedCandidates(record.coverageContext.barePath, record.runId);
    const frozenRoots = record.originalRoots ?? [];
    // Authenticate every physical source record: listRecords deliberately skips unreadable
    // files for inspection, which cannot prove that FINAL covers the run's sources.
    if (!this.git.readInventoryCloneHeads || !this.git.ancestry) return;
    let sources: RecoveryRecord[];
    try { sources = await this.checkedRecords(record.runId); } catch { return; }
    const clones = await this.git.readInventoryCloneHeads(record.coverageContext.barePath, record.runId);
    if (clones.kind !== "verified") return;
    const laterContext = (c: OwedCandidate["contexts"][number]): boolean =>
      typeof c.generation === "number" && c.generation > record.generation!;
    const laterHead = (sha: string): boolean => {
      const journals = sources.filter(r => !r.coverageDigest && r.sourceSha === sha);
      const producers = unresolved.filter(c => c.sha === sha).flatMap(c => c.contexts);
      return (journals.length + producers.length > 0) &&
        journals.every(r => typeof r.generation === "number" && r.generation > record.generation!) &&
        producers.every(laterContext);
    };
    const settledEarlier = await this.settledEarlierGenerations(sources, record.generation!);
    const sourceHeads = sources.filter(r => !r.coverageDigest &&
      !(typeof r.generation === "number" && (r.generation > record.generation! || settledEarlier.has(r.generation)))).map(r => r.sourceSha);
    // Later claims have their own holds. Only positive later-generation evidence can
    // exclude a retained head; unknown attribution still needs positive ancestry.
    const heads = [...new Set([...sourceHeads, ...clones.heads.filter(h => !laterHead(h))])];
    for (const head of heads) {
      if (await this.git.ancestry(record.coverageContext.barePath, head, record.sourceSha) !== "ancestor") {
        await this.writeExistingRecord(record, cur => ({ ...cur, reason: cur.finalRequest
          ? "inventory_quiescence_breach" : "inventory_snapshot_changed" }));
        return;
      }
    }
    // Confirmed checkpoints may have removed pins since upload. The immutable archive
    // still covers those roots; only a new or differently attributed root invalidates it.
    if (snapshotRoots(unresolved.map(root => ({ ...root, contexts: root.contexts.filter(c => !laterContext(c)) }))
      .filter(root => root.contexts.length > 0)).some(root => !frozenRoots.some(frozen =>
      root.sha === frozen.sha && root.contexts.every(context =>
        frozen.contexts.some(producer => canonicalJson(producer) === canonicalJson(context)))))) {
      await this.writeExistingRecord(record, cur => ({ ...cur, reason: cur.finalRequest
        ? "inventory_quiescence_breach" : "inventory_snapshot_changed" }));
      return;
    }
    if (!record.inventoryCurrentSha || !record.originalSourceSha || !this.git.buildRecoveryCoverage) return;
    if (record.originalRoots?.length || record.finalRequest?.disposition.kind !== "settled") {
      const coverage = await this.git.buildRecoveryCoverage(record.coverageContext.barePath, record.coverageContext,
        [...new Set([...frozenRoots.map(c => c.sha), record.originalSourceSha])].sort(), record.inventoryCurrentSha);
      if (coverage.sha !== record.sourceSha || coverage.fingerprint !== record.coverageDigest) return;
    }
    // Validate the archive before both minting and replaying FINAL. A committed receipt
    // remains authoritative when its capture has expired or its status is unusable.
    let observedStatus: RecoveryCaptureStatusResponse | undefined;
    if (record.finalRequest?.disposition.kind !== "settled") {
      if (record.state !== "uploaded" || !record.serverCaptureId) return;
      try { observedStatus = await this.client.getRecoveryCaptureStatus(record.runId, record.serverCaptureId); }
      catch { if (reconcile) return this.reconcileInventory(record, prove); return; }
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      if (!healthyInventoryStatus(observedStatus, record, this.now())) {
        if (reconcile) return this.reconcileInventory(record, prove, observedStatus);
        return;
      }
    }
    if (!record.finalRequest) {
      if (!(await this.openInventoryHold(record)) || !await prove() || residueQuarantine() !== undefined) return;
      record = await this.writeExistingRecord(record, cur => ({
        ...cur, finalRequest: { disposition: { kind: "archive", capture_id: cur.serverCaptureId!,
          source_sha: cur.sourceSha, coverage_digest: cur.coverageDigest! } },
      }));
    }
    const request = record.finalRequest!;
    // The journaled identity was frozen under the exact open guarded hold.
    // Publication proof was checked at minting; replay grants no reserve/upload authority.
    try {
      if (!this.client.hasFeature?.("recovery_inventory_v1") || !(await this.inactiveInventory(record))) return;
      if (!await prove() || !await this.inactiveInventory(record)) return;
      // issue #2213: synchronous check immediately before the release (nothing awaited in between).
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      const ack = await this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition);
      if (ack.run_id !== record.runId || ack.generation !== record.generation || ack.released !== true ||
          ack.holds_released !== 1 || ack.retained) {
        return this.reconcileInventory(record, prove, observedStatus);
      }
      // issue #2213: the server release cannot be recalled. A latch that landed while it was in
      // flight keeps the local evidence unacknowledged; the identical request replays once unlatched.
      if (residueQuarantine() !== undefined) {
        this.log.warn("recovery: final inventory release returned while the worker is quarantined; local evidence kept unacknowledged",
          { run_id: record.runId, generation: record.generation });
        return quarantinedOutcome(record.captureId);
      }
      await this.writeExistingRecord(record, cur => ({ ...cur, finalAcknowledged: true }));
      this.onAuthoritativeGenerationReleased?.(record.runId, record.generation!);
      await this.forgetGenerationWithinCycle(record.runId, record.generation!);
      return { state: "uploaded", captureId: record.captureId };
    } catch (err) {
      this.log.warn("recovery: final inventory ACK pending; exact request retained", { run_id: record.runId, generation: record.generation, error: errText(err) });
      const cls = await classifyUploadFailure(err, record);
      if (cls.kind === "credential") this.credentialBlockedAt = this.now();
      const reconciled = await this.reconcileInventory(record, prove, observedStatus);
      if (reconciled) return reconciled;
      return { state: record.state, captureId: record.captureId,
        reason: cls.kind === "transient" ? cls.reason ?? "upload_transient" : cls.reason };
    }
  }

  /** One atomic reconciliation attempt; failures keep custody and every saved identity. */
  private async reconcileInventory(record: RecoveryRecord, prove: () => Promise<boolean>, status?: RecoveryCaptureStatusResponse): Promise<RecoveryOutcome | void> {
    if (!record.serverCaptureId || !record.coverageDigest || !record.generation ||
        !hasJournaledBundle(record) || !this.client.reconcileRecoveryCapture ||
        !this.client.hasFeature?.("recovery_inventory_v1")) return;
    try {
      if (!await prove() || !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
      const response = await this.client.reconcileRecoveryCapture(record.runId, record.serverCaptureId, {
        generation: record.generation, source_sha: record.sourceSha, coverage_digest: record.coverageDigest,
        checksum: record.checksum!, byte_size: record.byteSize!,
      });
      if (!response || typeof response !== "object" || Array.isArray(response) ||
          residueQuarantine() !== undefined || response.run_id !== record.runId ||
          response.generation !== record.generation || response.capture_id !== record.serverCaptureId) return;
      if (response.outcome === "accepted") {
        const receipt = response.final_receipt;
        const saved = record.finalRequest;
        if (!saved || !receipt || response.reason !== undefined ||
            typeof receipt.coverage_digest !== "string" || !/^[0-9a-f]{64}$/.test(receipt.coverage_digest) ||
            (receipt.kind === "archive" && (typeof receipt.capture_id !== "string" ||
              typeof receipt.source_sha !== "string" || !/^[0-9a-f]{40}$/.test(receipt.source_sha))) ||
            (receipt.kind === "settled" && (receipt.capture_id !== undefined || receipt.source_sha !== undefined ||
              !["publication", "forge_no_output"].includes(response.release_evidence ?? ""))) ||
            canonicalJson(receipt) !== canonicalJson(saved.disposition) ||
            response.release_evidence !== saved.evidence ||
            (receipt.kind !== "archive" && receipt.kind !== "settled")) return;
        if (!await prove() || !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
        const current = await this.writeExistingRecord(record, cur =>
          residueQuarantine() === undefined && canonicalJson(cur.finalRequest) === canonicalJson(saved) &&
          cur.generation === record.generation && cur.serverCaptureId === record.serverCaptureId && !cur.finalAcknowledged
            ? { ...cur, finalAcknowledged: true } : undefined);
        if (current.finalAcknowledged) this.onAuthoritativeGenerationReleased?.(record.runId, record.generation);
        return;
      }
      if (response.outcome === "retained") {
        // api/internal/recovery/reconcile.go emits only these bounded protocol reasons.
        const reasons = new Set(["hold_released_without_final_receipt", "hold_not_open",
          "hold_not_original_worker", "generation_not_ended", "capture_not_replaceable",
          "capture_manifest_integrity_mismatch", "available_capture_manifest_unbound",
          "capture_manifest_invalid", "capture_failure_requires_attention",
          "available_capture_expiry_invalid", "capture_available", "capture_state_unknown"]);
        if (response.final_receipt !== undefined || response.release_evidence !== undefined ||
            typeof response.reason !== "string" || !reasons.has(response.reason)) return;
        const reason = response.reason;
        if (reason === "capture_available" || reason === "generation_not_ended") return;
        const current = await this.writeExistingRecord(record, cur =>
          residueQuarantine() === undefined && !cur.finalAcknowledged &&
          canonicalJson(cur) === canonicalJson(record) ? { ...cur, reason } : undefined);
        if (current.reason !== reason) return;
        this.log.warn("recovery: inventory requires owner attention", {
          run_id: current.runId, generation: current.generation, reason,
        });
        return { state: current.state, captureId: current.captureId, reason };
      }
      if (response.final_receipt !== undefined || response.release_evidence !== undefined ||
          response.outcome !== "replaceable" || response.reason !== undefined ||
          !status || !replaceableInventoryStatus(status, record, this.now())) return;
      if (!await this.openInventoryHold(record) || !await prove() ||
          !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
      await this.writeExistingRecord(record, cur => {
        if (residueQuarantine() !== undefined || cur.finalAcknowledged ||
            !cur.inventoryGuarded || cur.generation !== record.generation ||
            canonicalJson(cur.coverageContext) !== canonicalJson(record.coverageContext) ||
            canonicalJson(cur.originalRoots) !== canonicalJson(record.originalRoots) ||
            cur.state !== record.state || cur.serverCaptureId !== record.serverCaptureId ||
            cur.sourceSha !== record.sourceSha || cur.coverageDigest !== record.coverageDigest ||
            cur.checksum !== record.checksum || cur.byteSize !== record.byteSize ||
            cur.reserveIdempotencyKey !== record.reserveIdempotencyKey ||
            canonicalJson(cur.finalRequest) !== canonicalJson(record.finalRequest)) return;
        const next = { ...cur, state: "bundled" as const, reserveIdempotencyKey: randomUUID() };
        delete next.serverCaptureId;
        delete next.finalRequest;
        delete next.finalAcknowledged;
        delete next.reason;
        return next;
      });
    } catch (err) {
      this.log.warn("recovery: capture reconciliation pending; custody retained", { run_id: record.runId, error: errText(err) });
    }
  }

  private async resumeInventory(record: RecoveryRecord, signal?: AbortSignal, isExecuting = this.isExecuting): Promise<RecoveryOutcome | void> {
    if (record.completionReceipt) {
      await this.forgetCompletedGeneration(record.runId, record.generation!, isExecuting);
      return;
    }
    if (record.finalAcknowledged) {
      await this.forgetGenerationWithinCycle(record.runId, record.generation!, isExecuting);
      return;
    }
    if (this.isExecuting(record.runId) || !isLiveCandidate(record) || !record.coverageDigest || record.finalAcknowledged || !(await this.inactiveInventory(record))) return;
    if (record.finalRequest || record.state === "uploaded") {
      const outcome = await this.finalizeInventory(record);
      // An acknowledged FINAL forgets the generation, so its record may already be gone.
      try {
        record = await this.requireRecord(record);
      } catch (err) {
        if (err instanceof RecordGoneError && err.absent) return outcome;
        throw err;
      }
      if (record.state !== "bundled" || record.finalRequest || record.finalAcknowledged) return outcome;
    }
    const barePath = record.bareDir && await this.git.resolveRecoveryBareDir?.(record.bareDir);
    if (!barePath) return;
    return this.captureCycle({ record, barePath, defaultBranch: record.defaultBranch ?? "main", signal });
  }

  /** Credential-free discovery precedes the journal snapshot; authentication is required
   * before materializing anything. A failure blocks discovery, never implies empty work. */
  async snapshotOwedInventory(): Promise<void> {
    if (!this.git.discoverOwedCandidates) throw new Error("inventory discovery unavailable");
    const discovered = await this.git.discoverOwedCandidates();
    const fresh = new Map(discovered.map(entry => [canonicalJson(entry.context), entry]));
    // A successful discovery is authoritative: a context it no longer returns has no pins left,
    // so it leaves the queue instead of costing a hold RPC per pass forever. Survivors keep their
    // queue position (materializeBootInventory rotates the queue to avoid starving later entries).
    const entries = new Map<string, (typeof discovered)[number]>();
    for (const entry of this.bootOwed) {
      const key = canonicalJson(entry.context);
      const current = fresh.get(key);
      if (current) entries.set(key, current);
    }
    for (const [key, entry] of fresh) if (!entries.has(key)) entries.set(key, entry);
    this.bootOwed = [...entries.values()];
  }

  async materializeBootInventory(signal?: AbortSignal, isExecuting = this.isExecuting): Promise<RecoveryRecord[]> {
    const records: RecoveryRecord[] = [];
    // Each discovered context is visited once; a failure keeps its pins and allows siblings.
    const batch = this.bootOwed.splice(0, this.liveMaxPerPass);
    this.bootOwed.push(...batch); // rotate retained discoveries behind siblings; no internal retries
    for (const entry of batch) {
      if (signal?.aborted) break;
      const ctx = entry.context;
      if (ctx.generation === null || !("kind" in ctx) || ctx.kind === null ||
          (this.isExecuting(ctx.runId) || isExecuting(ctx.runId)) || !this.client.hasFeature?.("recovery_inventory_v1")) continue;
      try {
        const hold = (await this.client.listRecoveryHolds(ctx.runId)).holds.find(h =>
          h.generation === ctx.generation && h.inventory_guarded === true);
        if (!hold || !this.client.getRunOwnership) continue; // closed/discarded never reopened
        const probe = { runId: ctx.runId, generation: ctx.generation } as RecoveryRecord;
        if (!(await this.inactiveInventory(probe))) continue;
        const existing = await this.listRecords(ctx.runId);
        const original = existing.find(r => r.generation === ctx.generation && !r.coverageDigest);
        const frozenContext = existing.find(r => r.generation === ctx.generation && r.coverageContext)?.coverageContext;
        // A generation has one stable snapshot authority even when its original roots
        // carry several branch contexts. A changed source still gets a new immutable key.
        const preferred = frozenContext ?? this.bootOwed
          .filter(e => e.context.runId === ctx.runId && e.context.generation === ctx.generation)
          .map(e => e.context)
          .sort((a, b) => Number(b.branch === original?.branch) - Number(a.branch === original?.branch) ||
            canonicalJson(a).localeCompare(canonicalJson(b)))[0];
        if (!preferred || canonicalJson(preferred) !== canonicalJson(ctx)) continue;
        const originalSourceSha = original?.sourceSha;
        let currentSha = originalSourceSha ?? entry.candidates[0]?.sha;
        const owned = await this.git.committedTrackingOwnership?.(ctx.barePath, ctx.branch, ctx.runId, undefined, ctx.generation);
        if (owned?.kind === "owned" && canonicalJson(owned.context) === canonicalJson(ctx)) currentSha = owned.sha;
        if (!currentSha) continue; // No adopted source or positive tracking proof is not an empty snapshot.
        // freezeInventory alone compares the complete source/tree/context fingerprint.
        // Equal pin lists cannot justify reusing an archive of an earlier disposition source.
        const diagnostics = this.inventorySourceDiagnostics({ ...ctx, generation: ctx.generation }, original?.captureId);
        const boundary = await this.inventorySourceBoundary(diagnostics.context, async prove => {
          if (!await this.inactiveInventory(probe) || !await prove()) return;
          const record = await this.freezeInventory({
            context: ctx as PositiveOwedCandidateContext, currentSha, originalSourceSha,
            defaultBranch: ctx.defaultIdentity!.ref.replace(/^refs\/remotes\/origin\//, ""),
          });
          if (record) records.push(record);
        });
        if (boundary === "retained") diagnostics.retained();
        if (boundary === "retained" && original) {
          await this.writeExistingRecord(original, cur => ({ ...cur, state: "needs_action", reason: diagnostics.reason() }));
        }
      } catch (err) {
        this.log.warn("recovery: boot inventory retained; reconstruction failed", { run_id: ctx.runId, generation: ctx.generation, error: errText(err) });
      }
    }
    return records;
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
  /** Positive local capture proof for claim-local adoption, independent of upload/final ACK.
   * This proof grants no cleanup or custody-release authority. */
  async verifiedLocalCapture(record: RecoveryRecord, signal?: AbortSignal): Promise<RecoveryRecord | undefined> {
    const latest = await this.readLatest(record);
    if (latest.kind !== "ok") return undefined;
    const local = latest.record;
    if (!hasJournaledBundle(local) || !await verifyJournaledBytes(local, signal)) return undefined;
    const header = await readRecoveryBundleHeader(local.bundlePath, local.sourceSha);
    if (header.selfContained !== local.selfContained ||
        canonicalJson(header.prerequisiteShas) !== canonicalJson(local.prerequisiteShas)) return undefined;
    return local;
  }

  async captureAndUpload(input: CaptureInput): Promise<RecoveryOutcome> {
    if (!this.enabled) return { state: "pinned", captureId: input.record.captureId };
    // issue #2213: a latched worker captures and uploads nothing; the record and pin stay as they are.
    if (residueQuarantine() !== undefined) return quarantinedOutcome(input.record.captureId);
    return runCaptureCycle(await this.cycleKey(input.record), "wait", () => this.captureCycle(input));
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
      if (record.inventoryGuarded && (!record.coverageDigest || !this.client.hasFeature?.("recovery_inventory_v1"))) {
        return { state: "needs_action", captureId: record.captureId, reason: "inventory_not_frozen_or_feature_unavailable" };
      }
      if (record.inventoryGuarded && !record.finalRequest && !(await this.openInventoryHold(record))) {
        return { state: "needs_action", captureId: record.captureId, reason: "inventory_hold_closed" };
      }
      if (record.finalRequest || record.state === "uploaded") {
        const finalOutcome = record.inventoryGuarded ? await this.finalizeInventory(record) : undefined;
        record = await this.requireRecord(record);
        if (record.state !== "bundled" || record.finalRequest || record.finalAcknowledged) {
          return finalOutcome ?? { state: record.state, captureId: record.captureId };
        }
      }
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
      const outcome = await this.uploadJournaledBundle(record, input.signal, "capture");
      if (record.inventoryGuarded && outcome.state === "uploaded") {
        const finalOutcome = await this.finalizeInventory(await this.requireRecord(record), false);
        if (finalOutcome) return finalOutcome;
      }
      return outcome;
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
      if (!record.inventoryGuarded) forgeTip = await this.git.fetchDefaultTip(
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
        ...(record.inventoryGuarded ? { guardedDefaultBranch: input.defaultBranch } : { forgeTip }),
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
    if (result.alreadyPublished && record.inventoryGuarded) throw new Error("aggregate cannot use already-published shortcut");
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
        const header = cur.inventoryGuarded ? await readRecoveryBundleHeader(tmpPath, cur.sourceSha) : undefined;
        await fs.rename(tmpPath, finalPath);
        const bundled: RecoveryRecord = {
          ...cur,
          state: "bundled",
          bundlePath: finalPath,
          ...(recoveryPinBareDir !== undefined ? { recoveryPinBareDir } : {}),
          byteSize: result.byteSize,
          checksum: result.checksum,
          chunkCount: result.chunkCount,
          prerequisiteShas: header?.prerequisiteShas ?? result.prerequisiteShas,
          selfContained: header?.selfContained ?? result.selfContained,
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
   * permanent and credential dispositions are journaled as such. A transient failure preserves
   * its typed quota reason when present, otherwise using the mode's retry reason
   * (`capture_error`, `restart_upload_failed`, `upload_transient`). The caller holds the record's
   * capture-cycle lock.
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
      // The authenticated reservation key survives lost ACKs; legacy journals use captureId.
      let reserved: RecoveryReserveResponse;
      try {
        if (current.inventoryGuarded) {
          if (!this.client.hasFeature?.("recovery_inventory_v1")) throw new Error("inventory feature unavailable");
          if (!(await this.openInventoryHold(current))) throw new Error("exact guarded open hold unavailable");
          // issue #2213: a latch that landed during the hold read stops the reserve.
          if (residueQuarantine() !== undefined) return quarantinedOutcome(current.captureId);
        }
        reserved = await this.client.reserveRecoveryCapture(current.runId, {
          run_id: current.runId,
          idempotency_key: current.reserveIdempotencyKey ?? current.captureId,
          source_sha: current.sourceSha,
          ...(current.inventoryGuarded ? { coverage_digest: current.coverageDigest } : {}),
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
    if (!uploaded.inventoryGuarded) await this.cleanupUploadedRecoveryPin(uploaded);
    // Bytes are durable on the server; free the local copy. The small journal record stays
    // (state=uploaded) so a restart sweep skips it.
    if (!uploaded.inventoryGuarded) await fs.rm(current.bundlePath!, { force: true }).catch(() => undefined);
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
    const reason = cls.kind === "transient" ? cls.reason ?? TRANSIENT_UPLOAD_REASON[mode] : cls.reason;
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
    if ((await this.listRecords(runId)).some(r => (generation === undefined || r.generation === generation) && r.inventoryGuarded)) {
      this.log.warn("recovery: guarded inventory requires final disposition; custody retained", { run_id: runId, generation });
      return;
    }
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
   * (a tampered record is never trusted), legacy uploads omitted; guarded ACKs retry local cleanup.
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
        if (record.state !== "uploaded" || record.inventoryGuarded) out.push(record);
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
      await runCaptureCycle(await this.cycleKey(snap), "wait", () => this.pendingStep(snap, signal));
    }
  }

  /** One boot-sweep record step; the caller holds the record's capture-cycle lock. */
  private async pendingStep(snap: RecoveryRecord, signal?: AbortSignal): Promise<void> {
    // Re-read the current, authenticated state of exactly this snapshotted record.
    const record = await this.readRecord(this.recordPath(snap));
    if (!record) return;

    const done = (outcome: string, reason?: string): void => {
      this.log.info("recovery restart sweep", {
        run_id: record.runId,
        generation: record.generation,
        outcome,
        ...(reason ? { reason } : {}),
      });
    };
    try {
      if (record.inventoryGuarded) { await this.resumeInventory(record, signal); return; }
      if (record.state === "uploaded") return;
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
   * flight or before `nextPassAt`; rejected credentials exclude uploads but permit local ACK cleanup.
   * A pass touches at most `liveMaxPerPass` records, least recently attempted first, and backs off
   * exponentially (capped) after a pass with a transient failure. Guarded records with a FINAL
   * ACK retry local cleanup, including uploaded records. Other guarded candidates follow
   * inventory retry eligibility; legacy candidates need a journaled bundle in `bundled` or
   * transient/credential-rejected `needs_action`. Uploads use journaled bytes without a forge
   * PAT and never reproduce the bundle. A run that is executing is skipped, and each step takes the
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
    await this.trackPass(() => this.livePass(opts));
  }

  private async livePass(opts: ResumeLiveOptions): Promise<void> {
    let transient = false;
    try {
      const credentialsBlocked = this.credentialBlockedAt !== undefined && opts.authenticatedAtMs <= this.credentialBlockedAt;
      // Discovery may materialize uploads; blocked passes only retry persisted local ACKs.
      // One discovery per spaced pass. Failed discovery retains earlier entries.
      if (!credentialsBlocked && this.git.discoverOwedCandidates && this.client.hasFeature?.("recovery_inventory_v1")) {
        try { await this.snapshotOwedInventory(); }
        catch (err) {
          transient = true;
          this.log.warn("recovery: live inventory discovery failed; retained", { error: errText(err) });
        }
        await this.materializeBootInventory(opts.signal, opts.isExecuting);
      }
      const all = await this.snapshotBootRecords();
      // Forget the attempt stamps of captures no longer in the journal listing (bounded map).
      const seen = new Set(all.map((r) => r.captureId));
      for (const id of this.lastAttemptAt.keys()) if (!seen.has(id)) this.lastAttemptAt.delete(id);
      // Executing runs are filtered BEFORE the per-pass cap so they cannot hold its slots.
      const candidates = all
        .filter((r) => isLiveCandidate(r) && !opts.isExecuting(r.runId) &&
          (!credentialsBlocked || (r.inventoryGuarded && (r.finalAcknowledged || r.completionReceipt))))
        .sort((a, b) => (this.lastAttemptAt.get(a.captureId) ?? 0) - (this.lastAttemptAt.get(b.captureId) ?? 0))
        .slice(0, this.liveMaxPerPass);
      // A blocked pass with no local ACK work must not delay a newer authenticated heartbeat.
      if (credentialsBlocked && candidates.length === 0) return;
      let uploadsStopped = credentialsBlocked;
      for (const snap of candidates) {
        if (uploadsStopped && !(snap.inventoryGuarded && (snap.finalAcknowledged || snap.completionReceipt))) continue;
        if (opts.signal?.aborted) break;
        const res = await runCaptureCycle(await this.cycleKey(snap), "skip", () => this.liveStep(snap, opts));
        if (!res.ran) {
          // Cycle busy (a foreground capture or sweep step owns it): rotate it behind its peers so it
          // does not hold a per-pass slot forever.
          this.lastAttemptAt.set(snap.captureId, this.now());
          continue;
        }
        if (res.value === "transient") transient = true;
        // A credential failure blocks further uploads in this pass even if its heartbeat
        // timestamp is newer than the local clock. Local ACK siblings still run.
        if (res.value === "credential") uploadsStopped = true;
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
    if (!isLiveCandidate(record) || opts.isExecuting(record.runId) ||
        (!(record.inventoryGuarded && (record.finalAcknowledged || record.completionReceipt)) && this.credentialBlockedAt !== undefined &&
         opts.authenticatedAtMs <= this.credentialBlockedAt)) return "ok";
    this.lastAttemptAt.set(record.captureId, this.now());
    try {
      const out = record.inventoryGuarded
        ? await this.resumeInventory(record, opts.signal, opts.isExecuting)
        : await this.uploadJournaledBundle(record, opts.signal, "live");
      if (!out) return "ok";
      log(out.state, out.reason);
      if (out.reason === "credential_rejected") return "credential";
      if (out.reason && LIVE_RETRY_REASONS.has(out.reason)) return "transient";
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
      if (!record.finalAcknowledged && !record.completionReceipt) await this.markFailure(record, "upload_transient").catch(() => undefined);
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
   * completed-run release(). Guarded cleanup additionally requires a covering authenticated FINAL
   * ACK; a server release or owner discard alone grants no source-deletion authority.
   */
  async forgetGeneration(runId: string, generation: number): Promise<void> {
    if (!this.enabled) return;
    await runCaptureCycle(await this.cycleKey({ runId, captureId: "", generation, inventoryGuarded: true }), "wait",
      () => this.forgetGenerationWithinCycle(runId, generation));
  }

  /** Receipt cleanup visits only this authenticated generation. No archive ancestry or capture RPC.
   * One attempt per existing sweep; any failed proof stops this generation and retains authority. */
  private async forgetCompletedGeneration(runId: string, generation: number, isExecuting: (runId: string) => boolean): Promise<void> {
    if (!this.withCompletionSourceBoundary || isExecuting(runId) || this.isExecuting(runId)) return;
    await this.withJournalLock(runId, async () => {
      const records = await this.checkedRecords(runId);
      const selected = records.filter(r => r.generation === generation);
      const authority = selected.find(r => r.completionReceipt && r.completionSource);
      if (!authority || !this.git.cleanupRecoveryGeneration || residueQuarantine() !== undefined) return;
      const source = authority.completionSource!;
      const ctx = source.context;
      if (selected.some(r => !r.inventoryGuarded || r.branch !== ctx.branch || r.kind !== ctx.kind ||
          (r.completionReceipt && canonicalJson(r.completionReceipt) !== canonicalJson(authority.completionReceipt)))) return;
      const bare = await this.git.resolveRecoveryBareDir?.(path.basename(ctx.barePath));
      if (bare !== ctx.barePath) return;
      const bundles = new Set<string>();
      for (const record of selected) {
        if ((record.bareDir && record.bareDir !== path.basename(bare)) ||
            (record.recoveryPinBareDir && record.recoveryPinBareDir !== path.basename(bare)) ||
            (record.coverageContext && record.coverageContext.barePath !== bare) ||
            (record.completionSource && canonicalJson(record.completionSource) !== canonicalJson(source))) return;
        const canonical = path.resolve(this.bundlePath(record));
        if (record.bundlePath && path.resolve(record.bundlePath) !== canonical) return;
        if (path.dirname(canonical) !== path.resolve(this.runDir(runId))) return;
        try {
          const stat = await fs.lstat(canonical);
          if (!stat.isFile() || stat.isSymbolicLink()) return;
        } catch (err) {
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
        }
        bundles.add(canonical);
      }
      const pins = selected.filter(r => r.coverageDigest).map(r => ({ fingerprint: r.coverageDigest!, sha: r.sourceSha }));
      const recoverySources = [...new Set(selected.flatMap(r =>
        [r.sourceSha, r.originalSourceSha, r.inventoryCurrentSha].filter((sha): sha is string => !!sha)))];
      // Refuse malformed/unknown attribution before moving the physical completing source.
      const preflight = await this.git.cleanupRecoveryGeneration!(bare, ctx, source.roots, pins,
        recoverySources, authority.completionReceipt!.final_head,
        () => residueQuarantine() === undefined && !isExecuting(runId) && !this.isExecuting(runId), "completion-check");
      if (preflight !== "verified") return;
      await this.withCompletionSourceBoundary!(source, async canDelete => {
        const result = await this.git.cleanupRecoveryGeneration!(bare, ctx, source.roots, pins,
          recoverySources, authority.completionReceipt!.final_head, canDelete, "completion");
        if (result !== "removed") return;
        const unlink = async (file: string): Promise<void> => {
          if (!canDelete()) throw new Error("completion cleanup protection changed");
          try { await fs.unlink(file); }
          catch (err) { if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
        };
        for (const bundle of bundles) await unlink(bundle);
        for (const record of selected) if (record.captureId !== authority.captureId) await unlink(this.recordPath(record));
        await unlink(this.recordPath(authority));
        await fs.rmdir(this.runDir(runId)).catch(() => undefined);
      });
    });
  }

  /** Caller owns the generation cycle. The journal lock spans selection and every destructive
   * step, excluding delayed journal writers. One pass, no internal retries; failure keeps the
   * covering ACK and the bounded boot/live passes retry it independently of upload eligibility. */
  private async forgetGenerationWithinCycle(runId: string, generation: number, isExecuting = this.isExecuting): Promise<void> {
    await this.withJournalLock(runId, async () => {
      // Keep legacy exact-release cleanup (including opaque sibling retention) unchanged.
      // Classify under the journal lock and generation cycle so a new guarded writer cannot
      // slip between classification and removal. Guarded cleanup checks the complete inventory.
      if (!(await this.guardedGeneration({ runId, generation }))) {
        await this.removeGenerationRecordsUnlocked(runId, generation, "rmdir_if_empty");
        return;
      }
      const records = await this.checkedRecords(runId);
      const selected = records.filter(r => r.generation === generation);
      if (!selected.length) return;
      const state = await this.cleanupStateOf(records, generation);
      if (state === "legacy") {
        await this.removeGenerationRecordsUnlocked(runId, generation, "rmdir_if_empty");
        return;
      }
      if (state !== "acknowledged" || !this.git.cleanupRecoveryGeneration) return;
      const ack = selected.find(r => r.inventoryGuarded && r.finalAcknowledged && r.finalRequest && r.coverageDigest)!;
      const open = (): boolean => residueQuarantine() === undefined && !this.isExecuting(runId) && !isExecuting(runId);
      if (!open() || !ack.coverageContext || !ack.bareDir) return;
      const bare = await this.git.resolveRecoveryBareDir?.(ack.bareDir);
      if (bare !== ack.coverageContext.barePath) return;
      const bundles = new Set<string>();
      // Positive ownership is the canonical capture filename, not an arbitrary MAC-covered path.
      // Validate every path before changing refs; an unknown journal path retains all authority.
      for (const record of selected) {
        if ((record.bareDir && record.bareDir !== ack.bareDir) ||
            (record.recoveryPinBareDir && record.recoveryPinBareDir !== ack.bareDir) ||
            (record.coverageContext && record.coverageContext.barePath !== bare)) return;
        const canonical = path.resolve(this.bundlePath(record));
        if (record.bundlePath && path.resolve(record.bundlePath) !== canonical) return;
        if (path.dirname(canonical) !== path.resolve(this.runDir(runId))) return;
        try {
          const st = await fs.lstat(canonical);
          if (!st.isFile() || st.isSymbolicLink()) return;
        } catch (err) {
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
        }
        bundles.add(canonical);
      }
      if (!open()) return;
      const result = await this.git.cleanupRecoveryGeneration(bare, ack.coverageContext,
        ack.originalRoots ?? [], selected.filter(r => r.coverageDigest).map(r => ({
          fingerprint: r.coverageDigest!, sha: r.sourceSha,
          coveredHeads: [...new Set([r.originalSourceSha!, r.inventoryCurrentSha!, ...(r.originalRoots ?? []).map(root => root.sha)])],
        })), [...new Set(selected.flatMap(r => [r.sourceSha, r.originalSourceSha].filter((s): s is string => !!s)))],
        ack.sourceSha, open);
      if (result !== "removed") return;
      const unlink = async (file: string): Promise<void> => {
        if (!open()) throw new Error("recovery cleanup source protection changed");
        try { await fs.unlink(file); }
        catch (err) { if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
      };
      for (const bundle of bundles) await unlink(bundle);
      // All other journals (including other ACKs) go first. The ACK chosen by cleanupStateOf
      // remains a covering authority at every intermediate state and is removed last.
      for (const record of selected.filter(r => r.captureId !== ack.captureId)
        .sort((a, b) => Number(!!a.finalAcknowledged) - Number(!!b.finalAcknowledged))) await unlink(this.recordPath(record));
      await unlink(this.recordPath(ack));
      await fs.rmdir(this.runDir(runId)).catch(() => undefined);
    });
  }

  /** All AUTHENTICATED records for a run (a tampered/unreadable record is omitted, never
   *  returned as trusted). Used by the restart sweep and by owner/status surfaces. */
  async inspect(runId: string): Promise<RecoveryRecord[]> {
    if (!this.enabled) return [];
    return this.listRecords(runId);
  }

  async bindCompletionSource(source: CompletionSource): Promise<boolean> {
    if (!this.enabled || !this.key || !validCompletionSource(source)) return false;
    const ctx = source.context;
    try {
      return await this.withJournalLock(ctx.runId, async () => {
        const records = await this.checkedRecords(ctx.runId);
        if (records.some(r => r.generation === undefined)) return false;
        const selected = records.filter(r => r.generation === ctx.generation);
        if (!selected.length) return false;
        // Only HEAD and roots may advance for the same completing attempt before a receipt.
        // Validate the entire cohort before writing so foreign attribution is never replaced.
        const bareDir = path.basename(ctx.barePath);
        if (selected.some(r => !r.inventoryGuarded || r.runId !== ctx.runId ||
            r.branch !== ctx.branch || r.kind !== ctx.kind || r.completionReceipt ||
            (r.bareDir && r.bareDir !== bareDir) ||
            (r.recoveryPinBareDir && r.recoveryPinBareDir !== bareDir) ||
            (r.coverageContext && canonicalJson(r.coverageContext) !== canonicalJson(ctx)) ||
            (r.bundlePath && path.resolve(r.bundlePath) !== path.resolve(this.bundlePath(r))) ||
            (r.completionSource && (canonicalJson(r.completionSource.context) !== canonicalJson(ctx) ||
              r.completionSource.clonePath !== source.clonePath ||
              r.completionSource.attemptId !== source.attemptId)))) return false;
        // One pass bounded by the existing cohort; a failed write/readback stops siblings.
        for (const record of selected) {
          await this.writeRecordUnlocked({ ...record, completionSource: source });
        }
        for (const record of selected) {
          const readback = await this.requireRecord(record);
          if (canonicalJson(readback.completionSource) !== canonicalJson(source)) return false;
        }
        return true;
      });
    } catch { return false; }
  }

  /** One bounded authenticated write/readback, only to an existing exact-generation record.
   * Failure retains sources and terminal evidence; no record is created after release. */
  async persistCompletionReceipt(receipt: CompletedPublicationReceipt): Promise<boolean> {
    if (!this.enabled || !this.key || !validCompletionReceipt(receipt)) return false;
    try {
      return await this.withJournalLock(receipt.run_id, async () => {
        const records = await this.checkedRecords(receipt.run_id);
        const eligible = records.filter(r => r.inventoryGuarded && r.generation === receipt.generation &&
          r.branch === receipt.branch && ["issue", "mr_rework", "self_improve"].includes(r.kind));
        const existing = eligible.find(r => r.completionReceipt) ?? eligible.find(r => r.completionSource) ?? eligible[0];
        if (!existing) return false;
        if (existing.completionReceipt && canonicalJson(existing.completionReceipt) !== canonicalJson(receipt)) return false;
        await this.writeRecordUnlocked({ ...existing, completionReceipt: receipt });
        const readback = await this.requireRecord(existing);
        return canonicalJson(readback.completionReceipt) === canonicalJson(receipt);
      });
    } catch (err) {
      this.log.warn("recovery: completion receipt not persisted; source and terminal evidence retained", {
        run_id: receipt.run_id, error: errText(err),
      });
      return false;
    }
  }

  async hasPersistedCompletionReceipt(runId: string, generation: number): Promise<boolean> {
    try {
      return (await this.checkedRecords(runId)).some(r => r.generation === generation && !!r.completionReceipt);
    } catch { return false; }
  }

  /** Cleanup authority requires a complete authenticated physical journal, unlike inspect. */
  async inventoryCleanupState(runId: string, generation: number): Promise<"absent" | "legacy" | "pending" | "acknowledged"> {
    const records = await this.checkedRecords(runId);
    if (!records.some(r => r.generation === generation || r.generation === undefined)) return "absent";
    return this.cleanupStateOf(records, generation);
  }

  /** Earlier generations whose own covering FINAL is acknowledged (cleanupStateOf says so): their
   *  heads are archived durably, so a later generation's archive and cleanup need not cover their
   *  source journals. The one decision shared by finalization and cleanup, so they cannot drift. */
  private async settledEarlierGenerations(records: RecoveryRecord[], generation: number): Promise<Set<number>> {
    const settled = new Set<number>();
    const earlier = new Set(records.filter(r => r.inventoryGuarded === true && typeof r.generation === "number" &&
      r.generation < generation).map(r => r.generation!));
    for (const g of earlier) if (await this.cleanupStateOf(records, g) === "acknowledged") settled.add(g);
    return settled;
  }

  /** The one settlement rule, shared by cleanup authority and owed-context pruning. */
  private cleanupStateOf(records: RecoveryRecord[], generation: number): Promise<"legacy" | "pending" | "acknowledged"> {
    // One evaluation per generation per journal snapshot: the memo is keyed by the snapshot array
    // itself (weakly), so it never outlives or crosses a snapshot. Without it every generation
    // re-evaluates all earlier ones through settledEarlierGenerations, which is exponential.
    let memo = this.cleanupMemo.get(records);
    if (!memo) { memo = new Map(); this.cleanupMemo.set(records, memo); }
    let state = memo.get(generation);
    if (!state) { state = this.evaluateCleanupState(records, generation); memo.set(generation, state); }
    return state;
  }

  private readonly cleanupMemo = new WeakMap<RecoveryRecord[], Map<number, Promise<"legacy" | "pending" | "acknowledged">>>();

  private async evaluateCleanupState(records: RecoveryRecord[], generation: number): Promise<"legacy" | "pending" | "acknowledged"> {
    const relevant = records.filter(r => r.generation === generation);
    const guarded = relevant.filter(r => r.inventoryGuarded === true);
    if (guarded.length === 0) return "legacy";
    const ack = guarded.find(r => r.finalAcknowledged === true && r.finalRequest && r.coverageDigest);
    if (!ack || ack.reason === "inventory_quiescence_breach" || ack.reason === "inventory_snapshot_changed") return "pending";
    // FINAL authenticated its frozen metadata. Later source records still require coverage;
    // no ACK can short-circuit an unreadable sibling or an unattributed source.
    const settledEarlier = await this.settledEarlierGenerations(records, generation);
    for (const source of records) {
      if (source.generation !== undefined && source.generation > generation) continue;
      if (source.generation !== undefined && settledEarlier.has(source.generation)) continue;
      if (source.captureId === ack.captureId) continue;
      if (source.finalRequest && !source.finalAcknowledged) return "pending";
      // Earlier recovery-only aggregates are not adopted original heads. Check their
      // original roots and current source, without requiring synthetic commits as parents.
      if (source.coverageDigest && (!source.originalSourceSha || !source.inventoryCurrentSha)) return "pending";
      const heads = source.coverageDigest
        ? [...new Set([source.originalSourceSha!, source.inventoryCurrentSha!, ...(source.originalRoots ?? []).map(r => r.sha)])]
        : [source.sourceSha];
      for (const head of heads) {
        if (head === ack.sourceSha) continue;
        const bare = ack.bareDir && await this.git.resolveRecoveryBareDir?.(ack.bareDir);
        if (!bare || bare !== ack.coverageContext?.barePath || !this.git.ancestry ||
            await this.git.ancestry(bare, head, ack.sourceSha) !== "ancestor") return "pending";
      }
    }
    return "acknowledged";
  }

  /** One bounded pass over physical JSON files; any failed read blocks authority.
   * Paths must resolve inside this coordinator's root, with no symlink file reads. */
  private async checkedRecords(runId: string): Promise<RecoveryRecord[]> {
    if (!/^[a-zA-Z0-9_-]+$/.test(runId)) throw new Error("unsafe recovery run id");
    const root = path.resolve(this.recoveryRoot);
    const dir = this.runDir(runId);
    let names: string[];
    for (const directory of [root, dir]) {
      let stat;
      try { stat = await fs.lstat(directory); }
      catch (err) {
        if ((err as NodeJS.ErrnoException).code === "ENOENT") return [];
        throw err;
      }
      if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("unsafe recovery directory");
    }
    // The lstat checks above reject a symlinked recovery root or run directory. A symlinked
    // ANCESTOR of the root (a symlinked data dir) is a legitimate layout every other journal
    // read already tolerates, so containment compares against the canonical root, not the lexical one.
    const realRoot = await fs.realpath(root);
    if (await fs.realpath(dir) !== path.join(realRoot, path.basename(dir))) {
      throw new Error("symlink recovery directory");
    }
    // Disappearance after a positive directory check is unknown, not empty proof.
    names = await fs.readdir(dir);
    const records: RecoveryRecord[] = [];
    for (const name of names) {
      if (!name.endsWith(".json")) continue;
      const result = await this.readRecordResult(path.join(dir, name), true);
      if (result.kind !== "ok" || result.record.runId !== runId ||
          name !== result.record.captureId + ".json") throw new Error("unverified recovery journal");
      records.push(result.record);
    }
    return records;
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

  /** Guarded generations serialize original evidence; legacy captures retain pin invalidation. */
  private async cycleKey(record: Pick<RecoveryRecord, "runId" | "captureId" | "generation" | "inventoryGuarded">): Promise<string> {
    return (await this.guardedGeneration(record))
      ? path.resolve(this.runDir(record.runId), `generation-${record.generation ?? "legacy"}`)
      : this.recordPath(record);
  }

  private async guardedGeneration(record: Pick<RecoveryRecord, "runId" | "generation" | "inventoryGuarded">): Promise<boolean> {
    return record.inventoryGuarded === true || (await this.listRecords(record.runId))
      .some(r => r.generation === record.generation && r.inventoryGuarded === true);
  }

  private withJournalLock<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    return recoveryJournalLocks.withLock(path.resolve(this.runDir(runId)), fn);
  }

  /** Atomic rename with a fresh MAC (0600 file, 0700 dir). The caller holds this run's journal lock. */
  private async writeRecordUnlocked(record: RecoveryRecord): Promise<void> {
    if (!this.key) throw new Error("recovery: MAC key unavailable");
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
    noFollow = false,
  ): Promise<{ kind: "ok"; record: RecoveryRecord } | { kind: "absent" } | { kind: "unreadable" }> {
    if (!this.key) return { kind: "unreadable" };
    let raw: string;
    try {
      if (noFollow) {
        const file = await fs.open(filePath, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
        try {
          if (!(await file.stat()).isFile()) return { kind: "unreadable" };
          raw = await file.readFile("utf8");
        } finally { await file.close(); }
      } else {
        raw = await fs.readFile(filePath, "utf8");
      }
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

/** Order-insensitive snapshot identity; contexts describe original producers, never adoption. */
function snapshotRoots(candidates: OwedCandidate[]): NonNullable<RecoveryRecord["originalRoots"]> {
  return candidates.map(c => ({
    sha: c.sha,
    contexts: structuredClone(c.contexts).sort((a, b) => canonicalJson(a).localeCompare(canonicalJson(b))),
  })).sort((a, b) => a.sha.localeCompare(b.sha));
}

function validInventoryContext(value: unknown, runId: string, barePath?: string): boolean {
  if (!value || typeof value !== "object") return false;
  const c = value as Record<string, unknown>;
  if (c.runId !== runId || typeof c.branch !== "string" || !c.branch ||
      typeof c.barePath !== "string" || !path.isAbsolute(c.barePath) ||
      (barePath !== undefined && c.barePath !== barePath)) return false;
  if (c.origin === "historical") {
    return Object.keys(c).sort().join(",") === "barePath,branch,defaultIdentity,generation,kind,origin,producer,runId" &&
      c.generation === null && c.producer === "unknown" && c.kind === null && c.defaultIdentity === null;
  }
  const expected = c.generation === null
    ? "barePath,branch,defaultIdentity,generation,kind,legacy,runId"
    : "barePath,branch,defaultIdentity,generation,kind,runId";
  if (Object.keys(c).sort().join(",") !== expected ||
      (c.generation === null ? c.legacy !== true : !Number.isSafeInteger(c.generation) || (c.generation as number) <= 0) ||
      !RUN_KINDS.includes(c.kind as RunKind)) return false;
  const identity = c.defaultIdentity as Record<string, unknown> | undefined;
  return !!identity && Object.keys(identity).sort().join(",") === "ref,sha" &&
    typeof identity.ref === "string" && identity.ref.startsWith("refs/") &&
    typeof identity.sha === "string" && /^[0-9a-f]{40}$/.test(identity.sha);
}

/** Completion source shape is checked before authenticating the containing record. */
function validCompletionSource(value: unknown): value is CompletionSource {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const s = value as CompletionSource;
  const keys = Object.keys(s).sort().join(",");
  if (keys !== "clonePath,context,expectedHead,roots" && keys !== "attemptId,clonePath,context,expectedHead,roots") return false;
  const ctx = s.context;
  return !!ctx && validInventoryContext(ctx, ctx.runId, ctx.barePath) && !("legacy" in ctx) && !("origin" in ctx) &&
    ["issue", "mr_rework", "self_improve"].includes(ctx.kind) &&
    typeof s.clonePath === "string" && path.isAbsolute(s.clonePath) && path.resolve(s.clonePath) === s.clonePath &&
    typeof s.expectedHead === "string" && /^[0-9a-f]{40}$/.test(s.expectedHead) &&
    (s.attemptId === undefined || (typeof s.attemptId === "string" &&
      new RegExp("^[0-9]{8}T[0-9]{6}Z-g" + ctx.generation + "-[0-9a-f]{16}$").test(s.attemptId))) &&
    Array.isArray(s.roots) && s.roots.every(r => r && /^[0-9a-f]{40}$/.test(r.sha) &&
      Array.isArray(r.contexts) && r.contexts.length > 0 &&
      r.contexts.every(c => validInventoryContext(c, ctx.runId, ctx.barePath) &&
        !("legacy" in c) && !("origin" in c) && c.generation === ctx.generation &&
        c.branch === ctx.branch && c.kind === ctx.kind));
}

/** Strict persisted shape. Live identity validation belongs to WorkerClient; MAC binds it at rest. */
function validCompletionReceipt(value: unknown): value is CompletedPublicationReceipt {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const r = value as Record<string, unknown>;
  const fields = ["hold_id", "run_id", "owner_id", "worker_id", "generation", "final_head", "repo_id",
    "connection_id", "project_id", "forge_type", "base_url", "branch", "mr_iid", "observed_branch_head"];
  if (Object.keys(r).sort().join(",") !== fields.sort().join(",")) return false;
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
  for (const key of ["hold_id", "run_id", "owner_id", "worker_id", "repo_id", "connection_id"])
    if (typeof r[key] !== "string" || !uuid.test(r[key])) return false;
  for (const key of ["final_head", "observed_branch_head"])
    if (typeof r[key] !== "string" || !/^[0-9a-f]{40}$/.test(r[key])) return false;
  for (const key of ["generation", "project_id", "mr_iid"])
    if (!Number.isSafeInteger(r[key]) || (r[key] as number) <= 0) return false;
  for (const key of ["base_url", "branch"])
    if (typeof r[key] !== "string" || !r[key] || Buffer.byteLength(r[key]) > 2048 || /[\p{Cc}\p{Cf}]/u.test(r[key])) return false;
  return ["gitlab", "forgejo", "github"].includes(r.forge_type as string);
}

/** Validate journal shape; the MAC remains the integrity gate. */
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
  if (obj.reserveIdempotencyKey !== undefined) {
    if (!str(obj.reserveIdempotencyKey) || !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(obj.reserveIdempotencyKey)) return null;
    record.reserveIdempotencyKey = obj.reserveIdempotencyKey;
  }
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
  if (obj.inventoryGuarded !== undefined) {
    if (typeof obj.inventoryGuarded !== "boolean" || (obj.inventoryGuarded &&
        (!Number.isSafeInteger(record.generation) || record.generation! <= 0))) return null;
    record.inventoryGuarded = obj.inventoryGuarded;
  }
  if (obj.originalSourceSha !== undefined) {
    if (!str(obj.originalSourceSha) || !/^[0-9a-f]{40}$/.test(obj.originalSourceSha)) return null;
    record.originalSourceSha = obj.originalSourceSha;
  }
  if (obj.inventoryCurrentSha !== undefined) {
    if (!str(obj.inventoryCurrentSha) || !/^[0-9a-f]{40}$/.test(obj.inventoryCurrentSha)) return null;
    record.inventoryCurrentSha = obj.inventoryCurrentSha;
  }
  if (obj.coverageDigest !== undefined) {
    if (!record.inventoryGuarded || !str(obj.coverageDigest) || !/^[0-9a-f]{64}$/.test(obj.coverageDigest)) return null;
    const roots = obj.originalRoots;
    const ctx = obj.coverageContext as PositiveOwedCandidateContext | undefined;
    if (!record.inventoryCurrentSha || !record.originalSourceSha ||
        !Array.isArray(roots) || !roots.every(r => r && typeof r === "object" &&
        typeof r.sha === "string" && /^[0-9a-f]{40}$/.test(r.sha) &&
        Array.isArray(r.contexts) && r.contexts.length > 0 &&
        r.contexts.every((c: unknown) => validInventoryContext(c, record.runId, ctx?.barePath)))) return null;
    if (!ctx || !validInventoryContext(ctx, record.runId) || ctx.runId !== record.runId || ctx.generation !== record.generation ||
        ctx.branch !== record.branch || ctx.kind !== record.kind || !ctx.defaultIdentity ||
        !/^[0-9a-f]{40}$/.test(ctx.defaultIdentity.sha) || typeof ctx.defaultIdentity.ref !== "string" ||
        typeof ctx.barePath !== "string" || !path.isAbsolute(ctx.barePath) ||
        path.basename(ctx.barePath) !== record.bareDir || "legacy" in ctx || "origin" in ctx) return null;
    record.originalRoots = roots;
    record.coverageContext = ctx;
    record.coverageDigest = obj.coverageDigest;

  }
  if (obj.finalRequest !== undefined) {
    const req = obj.finalRequest as RecoveryRecord["finalRequest"];
    if (!record.coverageDigest || !req || !req.disposition ||
        req.disposition.coverage_digest !== record.coverageDigest ||
        !["archive", "settled"].includes(req.disposition.kind)) return null;
    if (req.disposition.kind === "archive" &&
        (req.disposition.capture_id !== record.serverCaptureId || req.disposition.source_sha !== record.sourceSha)) return null;
    if (req.disposition.kind === "settled" && (record.originalRoots?.length !== 0 ||
        record.coverageDigest !== "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" ||
        !["publication", "forge_no_output"].includes(req.evidence ?? "") ||
        req.disposition.capture_id !== undefined || req.disposition.source_sha !== undefined)) return null;
    record.finalRequest = req;
  }
  if (obj.completionSource !== undefined) {
    if (!validCompletionSource(obj.completionSource)) return null;
    const source = obj.completionSource;
    if (!record.inventoryGuarded || source.context.runId !== record.runId ||
        source.context.generation !== record.generation || source.context.branch !== record.branch ||
        source.context.kind !== record.kind) return null;
    record.completionSource = source;
  }
  if (obj.completionReceipt !== undefined) {
    if (!validCompletionReceipt(obj.completionReceipt)) return null;
    const receipt = obj.completionReceipt;
    if (!record.inventoryGuarded || receipt.run_id !== record.runId || receipt.generation !== record.generation ||
        receipt.branch !== record.branch || !["issue", "mr_rework", "self_improve"].includes(record.kind)) return null;
    record.completionReceipt = receipt;
  }
  if (obj.finalAcknowledged !== undefined) {
    if (typeof obj.finalAcknowledged !== "boolean" || !record.finalRequest) return null;
    record.finalAcknowledged = obj.finalAcknowledged;
  }
  return record;
}
