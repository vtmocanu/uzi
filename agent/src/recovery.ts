import { AsyncLocalStorage } from "node:async_hooks";
import { MaintenanceBudget } from "./maintenance-budget.js";
import { MaintenanceResources as M1MaintenanceResources, RecoveryMaintenance, type MaintenanceAdmission } from "./recovery-maintenance.js";
import { classifyRecoveryOrigin } from "./recovery-origin.js";
import { prepareFrozenHeld, validFrozenHeldPolicy, type FrozenHeldPolicy, type HeldGit } from "./frozen-held.js";
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
import { setTimeout as retryDelay } from "node:timers/promises";

import { RUN_KINDS, HELD_FINAL_FALLBACK_REASONS, HELD_FINAL_RETAINED_REASONS, type HeldFinalRetainedReason } from "./protocol.js";
import { RequestError, isRunOwnershipLost, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import { residueQuarantine } from "./residue-quarantine.js";
import { RunDiskLocks } from "./run-disk-locks.js";
import type {
  CompletedPublicationReceipt,
  RecoveryArchiveRedundancyRequest,
  RecoveryArchiveRedundancyResponse,
  RecoveryReconcileRequest,
  RecoveryReconcileResponse,
  RecoveryFinalDisposition,
  RunOwnershipResponse,
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryExactHold,
  RecoveryHoldsResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
  RunKind,
} from "./protocol.js";
import { readRecoveryBundleHeader, RECOVERY_CHUNK_BYTES, RecoveryBundleTooLargeError, type RecoveryBundleResult, type PositiveOwedCandidateContext, type OwedCandidate, type RecoveryCoverage, type RecoveryCoverageClaim, type FetchAgentBranchOptions, type TrackingUpdateResult, type GitCache } from "./git.js";

/** Ephemeral execution metadata only; never custody or production admission authority. */
export type ClosedSourceObservation = Readonly<{
  genesis: Readonly<{ runId: string; generation: number; clonePath: string; attemptId: string }>;
  originalContext?: Readonly<{
    barePath: string; branch: string; key: string; kind: RunKind; defaultBranch: string;
    cloneUrl: string; repoId: string; baseCommit: string; context: PositiveOwedCandidateContext;
  }>;
  terminal: "normal_park" | "normal_failed" | "normal_blocked";
  family?: Readonly<{
    originals: ReadonlyArray<ClosedSourceObservation["genesis"] & { identity: string }>;
    observedExecution: Readonly<{ runId: string; generation: number }>;
    metadata: string;
  }>;
}>;

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
  /** Original hold identity authenticated at freeze; absent on legacy records. */
  exactHoldBinding?: { holdId: string; repoId: string };
  heldPolicy?: FrozenHeldPolicy;
  heldCreateUncertain?: boolean;
  /** Validated closed-hold evidence stops publication without granting cleanup authority. */
  holdSettlement?: RecoveryExactHold;
  /** Typed archive refusal, never inferred from status or error prose. */
  archiveRefusal?: "oversize" | "quota";
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
  /** Issue #2545: set only by a held_publication FINAL attempt. `acked` = the api released the hold
   *  against the forge ref; `kept` = the journaled held FINAL stays (non-definitive); `fallback` = the
   *  held FINAL was cleared (definitive refusal or a downgrade 400) and the caller must capture. */
  held?: "acked" | "kept" | "fallback" | "settled";
}

/** The archive RPC subset {@link RecoveryCoordinator} needs — WorkerClient satisfies it
 *  structurally; a test supplies a fake. */
export interface RecoveryArchiveClient {
  withAcknowledgedFinalReportCustody?: WorkerClient["withAcknowledgedFinalReportCustody"];
  hasFeature?(feature: string): boolean;
  hasHeldPublicationV2?: WorkerClient["hasHeldPublicationV2"];
  publishHeld?: WorkerClient["publishHeld"];
  getExactRecoveryHold?: WorkerClient["getExactRecoveryHold"];
  observeTerminalMaintenanceHold?: WorkerClient["observeTerminalMaintenanceHold"];
  isCurrentTerminalMaintenanceHold?: WorkerClient["isCurrentTerminalMaintenanceHold"];
  reconcileRecoveryCapture?(runId: string, captureId: string, req: RecoveryReconcileRequest): Promise<RecoveryReconcileResponse>;
  reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse>;
  getRecoveryCaptureStatus(runId: string, captureId: string, options?: { signal?: AbortSignal; deadline: number }): Promise<RecoveryCaptureStatusResponse>;
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
  releaseRecoveryCustody: WorkerClient["releaseRecoveryCustody"];
  /** Issue #2625: claim a completed run's archive is redundant with its published branch. Optional
   *  and gated on `recovery_archive_redundancy_v1`; the answer is only ever logged. */
  proveRecoveryArchiveRedundancy?(runId: string, captureId: string, req: RecoveryArchiveRedundancyRequest, signal?: AbortSignal): Promise<RecoveryArchiveRedundancyResponse>;
  getRunOwnership?(runId: string, options?: { signal?: AbortSignal; deadline: number }): Promise<RunOwnershipResponse>;
  /** The worker's own open custody holds on a run — the post-clone generation-exact inventory
   *  (PRD #1349 M1/M2, D3). */
  listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse>;
}

/** The bundle-producer subset {@link RecoveryCoordinator} needs — GitCache satisfies it. */
export interface RecoveryBundleProducer {
  matchMaintenanceSourceFamily?: GitCache["matchMaintenanceSourceFamily"];
  maintenanceOriginDigest?: GitCache["maintenanceOriginDigest"];
  worktreeHead?: GitCache["worktreeHead"];
  readonly recoveryDecodedLimitBytes?: number;
  resolveCheckpointRange?: HeldGit["resolveCheckpointRange"];
  secretScanCheckpointRange?: HeldGit["secretScanCheckpointRange"];
  rangeTouchesWorkflows?: HeldGit["rangeTouchesWorkflows"];
  changedFilesNoRenames?: HeldGit["changedFilesNoRenames"];
  checkpointPack?: HeldGit["checkpointPack"];
  withReportProofBudget?: GitCache["withReportProofBudget"];
  withBareLock?: GitCache["withBareLock"];
  readAcknowledgedReportSourcesUnderLock?: GitCache["readAcknowledgedReportSourcesUnderLock"];
  readInventoryCloneHeadsUnderLock?: GitCache["readInventoryCloneHeadsUnderLock"];
  enumerateOwedUnderLock?: GitCache["enumerateOwedUnderLock"];
  cleanupRecoveryGeneration?: GitCache["cleanupRecoveryGeneration"];
  readInventoryCloneHeads?: GitCache["readInventoryCloneHeads"];
  committedTrackingOwnership?: GitCache["committedTrackingOwnership"];
  ancestry?: GitCache["ancestry"];
  enumerateOwedCandidates?(barePath: string, runId: string): Promise<OwedCandidate[]>;
  discoverOwedCandidates?(): ReturnType<GitCache["discoverOwedCandidates"]>;
  retainCurrentOwedCandidate?(barePath: string, options: FetchAgentBranchOptions): Promise<TrackingUpdateResult>;
  buildRecoveryCoverage?(barePath: string, context: PositiveOwedCandidateContext, roots: string[], currentSha: string): Promise<RecoveryCoverage>;
  /** Issue #2625: read-only redundancy claim for a previously built coverage aggregate. */
  readRecoveryCoverageClaim?(barePath: string, opts: {
    sourceSha: string; fingerprint: string; roots: string[]; currentSha: string; witnessHead?: string;
  }): Promise<RecoveryCoverageClaim | undefined>;
  /** Runs `action` under a local recovery clock: every git child and queued repo lock inside it
   *  is clipped to `deadline` and aborted with `signal` ({@link GitCache.withRecoveryOperation}). */
  withRecoveryOperation?<T>(signal: AbortSignal, deadline: number, action: (signal: AbortSignal) => Promise<T>): Promise<T>;
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
  resolveRecoveryBareDir?(bareDir: string): Promise<string | undefined>;
  /** Optional: delete the transient recovery pin of an archived (uploaded) record. A producer
   *  without it never cleans a pin (see {@link RecoveryCoordinator}'s archived-pin cleanup). */
  cleanupArchivedRecoveryPin?: GitCache["cleanupArchivedRecoveryPin"];
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

export interface FrozenOperationOptions {
  /** The runner has reaped its own source before its terminal ACK. Live callers omit this. */
  foreground?: boolean;
  terminal?: { status: "failed" | "cancelled" | "completed"; fail_origin?: string };
  prepare?: (record: RecoveryRecord) => Promise<{ pack: Buffer } | { reason: string }>;
}

export interface CaptureInput {
  /** Foreground attempts hand their first observed cancellation back to the flight. Never serialized. */
  onCancellationObserved?: () => MaintenanceBudget;
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
      } catch { MaintenanceBudget.current()?.check(); /* Diagnostics must never change the proof's decision. */ }
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
  withTerminalSourceBoundary?: <T>(runId: string, paths: readonly string[], action: (ctx: {
    open: () => boolean; quiesce: (paths: string[]) => Promise<boolean>;
    isLivePath: (clonePath: string) => boolean;
  }) => Promise<T>) => Promise<{ entered: false } | { entered: true; value: T }>;
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
  /** Exact reports still need their authenticated generation evidence after FINAL.
   * Read synchronously so cleanup rechecks it at every destructive boundary. */
  pendingGenerationReport?: (runId: string, generation: number) => boolean;
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
  /** In-process delay before a settled (`deleted` / `already_absent`) archived-pin cleanup is attempted
   *  again for the same capture (default {@link LIVE_PIN_SETTLE_MS}). A restart re-checks every record once. */
  livePinSettleMs?: number;
  /** Delays between the bounded retries of {@link RecoveryCoordinator.settleUnadoptedGuardedGeneration}. */
  unadoptedSettleRetryDelaysMs?: readonly number[];
  /** Test seams (never set in production). */
  testHooks?: {
    /** Awaited by {@link RecoveryCoordinator.pin} between its unlocked record observation and
     *  its locked re-read, so a test can interleave a capture there. */
    afterPinObserve?: () => Promise<void>;
    /** Schedules the redundancy abort; tests fire it at an observed boundary and return a disposer. */
    redundancyTimer?: (fire: () => void, ms: number) => () => void;
    maintenanceBudget?: () => MaintenanceBudget;
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

/** Issue #2625: one deadline bounds every redundancy claim a single cleanup submits. */
const REDUNDANCY_DEADLINE_MS = 30_000;

/** Runs `start` and resolves with its result, or with undefined once `signal` aborts, whichever is
 *  first. The abort wait is armed BEFORE `start` runs: a callee that aborts synchronously would
 *  otherwise fire the event unheard, and an aborted signal never fires again. A rejection of
 *  `start` propagates; a late one after abort is swallowed. */
async function withinBudget<T>(signal: AbortSignal, start: () => Promise<T>): Promise<T | undefined> {
  if (signal.aborted) return undefined;
  let onAbort: (() => void) | undefined;
  const expired = new Promise<undefined>(resolve => {
    onAbort = () => resolve(undefined);
    signal.addEventListener("abort", onAbort, { once: true });
  });
  try {
    const pending = start();
    pending.catch(() => undefined);
    return await Promise.race([pending, expired]);
  } finally {
    if (onAbort) signal.removeEventListener("abort", onAbort);
  }
}
/** Retained reasons that say nothing final about the archive: a later cleanup may claim again. */
const TRANSIENT_REDUNDANCY_REASONS: ReadonlySet<string> = new Set([
  "ancestry_unknown", "forge_timeout", "cooling_down", "invalid_response",
]);

/** A claim built under the journal lock and submitted after it is released. */
interface PendingRedundancyClaim { runId: string; generation: number; captureId: string; request: RecoveryArchiveRedundancyRequest }
/** Issue #2625: the ONE deadline for a cleanup's redundancy work, started before claims are built
 *  so building (under the journal lock) and submitting (after it) share it. */
interface RedundancyBudget { signal: AbortSignal; deadline: number; dispose(): void }

/** Archived-pin cleanup: per-record cap, and the shared boot-sweep budget for all of them. */
const PIN_CLEANUP_RECORD_MS = 10_000;
const PIN_CLEANUP_BOOT_MS = 60_000;
export type UploadedPinCleanupMode = "capture" | "sweep" | "live" | "boot";
export type UploadedPinCleanupResult =
  | { state: "deleted" | "already_absent" }
  | { state: "retained"; reason: string };

/** The archive proof for deleting a pin: an authenticated, plain (non-guarded) `uploaded` record
 *  whose bundle was self-contained with no prerequisites, so the archive alone holds the head. */
function archiveProvesPin(r: RecoveryRecord): boolean {
  return r.state === "uploaded" && !r.inventoryGuarded && r.coverageDigest === undefined &&
    r.finalRequest === undefined && r.completionReceipt === undefined &&
    typeof r.serverCaptureId === "string" && r.serverCaptureId !== "" &&
    typeof r.checksum === "string" && /^[0-9a-f]{64}$/.test(r.checksum) &&
    typeof r.byteSize === "number" && Number.isSafeInteger(r.byteSize) && r.byteSize >= 0 &&
    r.selfContained === true && Array.isArray(r.prerequisiteShas) && r.prerequisiteShas.length === 0;
}

const LIVE_MAX_PER_PASS = 4;
const LIVE_BACKOFF_BASE_MS = 30_000;
const LIVE_BACKOFF_CAP_MS = 15 * 60_000;
const LIVE_PIN_SETTLE_MS = 6 * 60 * 60_000;

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

/** Bounded server reconciliation reasons that retain custody for owner attention. */
const INVENTORY_RECONCILE_ATTENTION_REASONS: ReadonlySet<string> = new Set([
  "hold_released_without_final_receipt", "hold_not_open", "hold_not_original_worker",
  "capture_not_replaceable", "capture_manifest_integrity_mismatch", "available_capture_manifest_unbound",
  "capture_manifest_invalid", "capture_failure_requires_attention", "available_capture_expiry_invalid",
  "capture_state_unknown",
]);

/** The `needs_action` reasons a live pass may retry (all need a journaled bundle). */
const LIVE_RETRY_REASONS: ReadonlySet<string> = new Set([
  "capture_error",
  "restart_upload_failed",
  "upload_transient",
  "storage_quota_exceeded",
  "credential_rejected",
  "final_retry_refused",
  "final_retry_unknown",
  "inventory_hold_settled",
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
// journal lock (withJournalLock). Most inner helpers take it only briefly; a few hold it across
// bounded Git work (the archived-pin cleanup, cleanupUploadedRecoveryPin, holds it from the journal
// re-read through the destructive Git call). Process memory on
// purpose, like recoveryJournalLocks: one worker process owns each data dir.
const captureCycleTails = new Map<string, Promise<void>>();

function maintenanceSignal(signal?: AbortSignal): AbortSignal | undefined {
  MaintenanceBudget.current()?.check();
  const pending = MaintenanceBudget.signal();
  return pending ? (signal ? AbortSignal.any([signal, pending]) : pending) : signal;
}

function maintenanceSignalArgs(): [] | [AbortSignal] {
  const signal = maintenanceSignal();
  return signal ? [signal] : [];
}

/** Owned operations settle before their lock or child ownership can be released. */
async function ownedRecoveryIO<T>(action: () => T | Promise<T>): Promise<T> {
  MaintenanceBudget.current()?.check();
  const value = await action();
  MaintenanceBudget.current()?.check();
  return value;
}

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
  MaintenanceBudget.current()?.check();
  const previous = captureCycleTails.get(key);
  if (mode === "skip" && previous) return { ran: false };
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  // Installed synchronously, so a later waiter queues behind this holder.
  const tail = (previous ?? Promise.resolve()).then(() => held);
  captureCycleTails.set(key, tail);
  try {
    // An expired waiter releases only its placeholder, behind the earlier holder.
    await MaintenanceBudget.waitFor(previous ?? Promise.resolve());
    MaintenanceBudget.current()?.check();
    const value = await fn();
    MaintenanceBudget.current()?.check();
    return mode === "skip" ? { ran: true, value } : value;
  } finally {
    release();
    void tail.then(() => {
      if (captureCycleTails.get(key) === tail) captureCycleTails.delete(key);
    });
  }
}

/** The typed disposition of a failed reserve/upload RPC (issue #1995). */
export type UploadFailureClass =
  | { kind: "transient"; reason?: "storage_quota_exceeded"; archiveRefusal?: "quota" }
  | { kind: "permanent"; reason: string; archiveRefusal?: "oversize" }
  | { kind: "credential"; reason: "credential_rejected" };

/** Recompute the size, SHA-256 and chunk count of the journaled bundle file and compare them with
 *  the journaled facts. False on a mismatch or an unreadable file. */
async function verifyJournaledBytes(record: RecoveryRecord, signal?: AbortSignal): Promise<boolean> {
  signal = maintenanceSignal(signal);
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
  } catch { MaintenanceBudget.current()?.check();
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
  } catch { MaintenanceBudget.current()?.check();
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
      return { kind: "permanent", reason: "archive_constraint", archiveRefusal: "oversize" };
    case "integrity":
      return integrity();
    case "quota":
      return { kind: "transient", reason: "storage_quota_exceeded", archiveRefusal: "quota" };
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

/** A held publication id is the api's UUID (lowercase hex, hyphenated). */
const HELD_PUBLICATION_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

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
  private readonly pendingGenerationReport: RecoveryCoordinatorOptions["pendingGenerationReport"];
  private readonly onAuthoritativeGenerationReleased: RecoveryCoordinatorOptions["onAuthoritativeGenerationReleased"];
  private readonly withTerminalSourceBoundary: RecoveryCoordinatorOptions["withTerminalSourceBoundary"];
  private readonly maintenance: RecoveryMaintenance | undefined;
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
  private readonly livePinSettleMs: number;
  private readonly testHooks: RecoveryCoordinatorOptions["testHooks"];
  private readonly unadoptedSettleRetryDelaysMs: readonly number[];
  /** In-flight passes (the boot sweep, a live pass): {@link resumeLive} is a no-op while non-empty. */
  private readonly passes = new Set<Promise<void>>();
  private readonly passShutdown = new AbortController();
  /** The earliest time the next live pass may run (the coordinator's `now()` clock). */
  private nextPassAt = 0;
  /** issue #2213: the quarantine skip of a live pass is logged once. */
  private loggedQuarantineSkip = false;
  /** Consecutive live passes that ended with a transient failure (drives the backoff). */
  private transientPasses = 0;
  /** Set by any 401: a live pass runs only for a heartbeat sent after it. */
  private credentialBlockedAt: number | undefined;
  private readonly lastAttemptAt = new Map<string, number>();
  /** Automatic saved-FINAL retries remain eligible indefinitely, paced independently of peers.
   * Process-local only: restart permits one fresh attempt. Counts saturate at the delay cap. */
  private readonly finalRetry = new Map<string, { identity: string; failures: number; nextAt: number }>();
  /** Latest transition per fixed retainFinal predicate; bounded to four entries per capture. */
  private readonly finalRefusalLogged = new Map<string, { identity: string; transitions: Map<string, string> }>();

  private finalRetryIdentity(record: RecoveryRecord): string | undefined {
    return record.inventoryGuarded && record.finalRequest && !record.finalAcknowledged && !record.completionReceipt
      ? canonicalJson([record.runId, record.generation, record.captureId, record.serverCaptureId, record.exactHoldBinding, record.finalRequest])
      : undefined;
  }

  private finalRetryReady(record: RecoveryRecord): boolean {
    const identity = this.finalRetryIdentity(record);
    const retry = this.finalRetry.get(record.captureId);
    if (retry && retry.identity !== identity) this.finalRetry.delete(record.captureId);
    if (this.finalRefusalLogged.get(record.captureId)?.identity !== identity) this.finalRefusalLogged.delete(record.captureId);
    return !identity || retry?.identity !== identity || retry.nextAt <= this.now();
  }

  private pruneFinalRetries(records: RecoveryRecord[]): void {
    const seen = new Set(records.map(record => record.captureId));
    for (const record of records) this.finalRetryReady(record);
    for (const id of this.finalRetry.keys()) if (!seen.has(id)) this.finalRetry.delete(id);
    for (const id of this.finalRefusalLogged.keys()) if (!seen.has(id)) this.finalRefusalLogged.delete(id);
  }

  private async retainFinal(record: RecoveryRecord, reason: "final_retry_refused" | "final_retry_unknown" | "inventory_hold_settled" | "credential_rejected", err?: unknown, persist = true): Promise<void> {
    const identity = this.finalRetryIdentity(record);
    if (!identity || residueQuarantine() !== undefined) return;
    const current = persist ? await this.writeExistingRecord(record, cur =>
      this.finalRetryIdentity(cur) === identity && cur.reason !== reason && !["inventory_snapshot_changed", "inventory_quiescence_breach"].includes(cur.reason ?? "")
        && !INVENTORY_RECONCILE_ATTENTION_REASONS.has(cur.reason ?? "")
        ? { ...cur, reason } : undefined) : await this.requireRecord(record);
    if (this.finalRetryIdentity(current) !== identity) return;
    // Only known protocol reasons can enter diagnostics; RequestError text/body is untrusted.
    const known = new Set(["not_authorized", "ambiguous_generation", "capture_not_found", "not_available",
      "manifest_conflict", "bad_request", "oversize", "integrity", "quota", "busy", "internal"]);
    const parsed = err instanceof RequestError ? errorReason(err) : undefined;
    const code = parsed && known.has(parsed) ? parsed : "unknown";
    const status = err instanceof RequestError && Number.isInteger(err.status) && err.status >= 100 && err.status <= 599
      ? err.status : undefined;
    const transition = canonicalJson([reason, code, status]);
    const prior = this.finalRefusalLogged.get(record.captureId);
    const transitions = prior?.identity === identity ? prior.transitions : new Map<string, string>();
    if (transitions.get(reason) === transition || (err === undefined && transitions.has(reason))) return;
    transitions.set(reason, transition);
    this.finalRefusalLogged.set(record.captureId, { identity, transitions });
    this.log.warn("recovery: FINAL retry retained", {
      run_id: current.runId.slice(0, 160), generation: current.generation,
      capture_id: current.captureId.slice(0, 160),
      ...(current.exactHoldBinding ? { hold_id: current.exactHoldBinding.holdId } : {}),
      reason: code, refusal_code: status ?? "unknown", refusing_predicate: reason,
      disposition: current.holdSettlement?.state ?? "unknown",
    });
  }

  private async paceFinalRetry(record: RecoveryRecord, outcome: RecoveryOutcome | void): Promise<void> {
    if (residueQuarantine() !== undefined || !outcome?.reason ||
        (!["upload_transient", "storage_quota_exceeded", "final_retry_refused", "final_retry_unknown",
          "stale_ownership", "upload_rejected"].includes(outcome.reason) &&
          !INVENTORY_RECONCILE_ATTENTION_REASONS.has(outcome.reason))) return;
    const latest = await this.readLatest(record);
    if (latest.kind !== "ok" || this.finalRetryIdentity(latest.record) !== this.finalRetryIdentity(record) ||
        !this.finalRetryIdentity(latest.record)) {
      this.finalRetry.delete(record.captureId);
      this.finalRefusalLogged.delete(record.captureId);
      return;
    }
    if (["inventory_snapshot_changed", "inventory_quiescence_breach"].includes(latest.record.reason ?? "")) return;
    if (latest.record.reason === "credential_rejected") return;
    if (latest.record.holdSettlement && !await this.finalHoldAllowsReplay(latest.record, "settled")) return;
    const identity = this.finalRetryIdentity(latest.record)!;
    const previous = this.finalRetry.get(record.captureId);
    const maxFailures = Math.max(1, Math.ceil(Math.log2(this.liveBackoffCapMs / this.liveBackoffBaseMs)));
    const failures = Math.min((previous?.identity === identity ? previous.failures : 0) + 1, maxFailures);
    this.finalRetry.set(record.captureId, { identity, failures,
      nextAt: this.now() + Math.min(this.liveBackoffBaseMs * 2 ** failures, this.liveBackoffCapMs) });
    if (outcome.reason === "final_retry_unknown" && latest.record.reason !== "final_retry_refused" && latest.record.reason !== "final_retry_unknown") {
      await this.retainFinal(latest.record, "final_retry_unknown");
    }
  }
  /** Per-capture exponential backoff of the live archived-pin cleanup (pruned like lastAttemptAt). */
  private readonly pinCleanupBackoff = new Map<string, { failures: number; nextAt: number }>();
  /** One diagnostic per retained ACK; pruned with the live journal snapshot. */
  private readonly pendingReportLogged = new Set<string>();
  /** serverCaptureIds whose redundancy claim got a final answer (expired, or a non-transient retained). */
  private readonly redundancySettled = new Set<string>();

  constructor(opts: RecoveryCoordinatorOptions) {
    this.withTerminalSourceBoundary = opts.withTerminalSourceBoundary;
    this.maintenance = opts.workerToken ? new RecoveryMaintenance({
      root: path.join(opts.recoveryRoot, "..", "recovery-maintenance"), workerToken: opts.workerToken,
    }) : undefined;
    this.withInventorySourceBoundary = opts.withInventorySourceBoundary;
    this.withCompletionSourceBoundary = opts.withCompletionSourceBoundary;
    this.isExecuting = opts.isExecuting ?? (() => false);
    this.terminalRecordProtection = opts.terminalRecordProtection;
    this.pendingGenerationReport = opts.pendingGenerationReport;
    this.onAuthoritativeGenerationReleased = opts.onAuthoritativeGenerationReleased;
    this.client = opts.client;
    this.git = opts.git;
    this.log = opts.log;
    this.recoveryRoot = opts.recoveryRoot;
    this.now = opts.now ?? (() => Date.now());
    this.liveMaxPerPass = opts.liveMaxPerPass ?? LIVE_MAX_PER_PASS;
    this.liveBackoffBaseMs = opts.liveBackoffBaseMs ?? LIVE_BACKOFF_BASE_MS;
    this.liveBackoffCapMs = opts.liveBackoffCapMs ?? LIVE_BACKOFF_CAP_MS;
    this.livePinSettleMs = opts.livePinSettleMs ?? LIVE_PIN_SETTLE_MS;
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
          ? (r: RecoveryRecord): boolean => r.generation === input.generation && !r.coverageDigest && classifyRecoveryOrigin(r.captureId) === "ordinary"
          : (r: RecoveryRecord): boolean => r.sourceSha === input.sourceSha && !r.coverageDigest && classifyRecoveryOrigin(r.captureId) === "ordinary";
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
          if (!this.newProductionAllowed(current)) return undefined;
          const next = applyPin(current, input);
          if (next === current) return current;
          await this.writeRecordUnlocked(next);
          return next;
        }
        if (!await this.newReconstructionRecords(input.runId, input.generation)) return undefined;
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
    } catch (err) { MaintenanceBudget.current()?.check();
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
    /** Trusted repo identity from the original claim, never reconstructed from Git. */
    repoId?: string;
    heldPolicy?: FrozenHeldPolicy;
    /** Caller proved the local process reaped; permits freezing before the terminal RPC. */
    locallyQuiescent?: boolean;
    /** Foreground cancellation budget belongs to the flight, including later FINAL work. */
    onCancellationObserved?: () => MaintenanceBudget;
  }): Promise<RecoveryRecord | undefined> {
    if (!this.enabled) return undefined;
    const { context } = input;
    return this.withCancellationAttempt({ runId: context.runId, generation: context.generation, inventoryGuarded: true } as RecoveryRecord, async () =>
      runCaptureCycle(await this.cycleKey({ runId: context.runId, generation: context.generation, captureId: "", inventoryGuarded: true }), "wait", async () => {
      if (!await this.newReconstructionRecords(context.runId, context.generation)) return undefined;
      if (!this.git.enumerateOwedCandidates || !this.git.buildRecoveryCoverage) throw new Error("inventory Git support unavailable");
      const candidates = await ownedRecoveryIO(() => this.git.enumerateOwedCandidates!(context.barePath, context.runId));
      const probe = { runId: context.runId, generation: context.generation } as RecoveryRecord;
      if (!input.locallyQuiescent && !(await this.inactiveInventory(probe))) return undefined;
      if (!(await this.openInventoryHold(probe))) return undefined;
      let exactHoldBinding: RecoveryRecord["exactHoldBinding"];
      if (input.repoId && this.client.getExactRecoveryHold) {
        const response = await this.recoveryRPC(() => this.client.listRecoveryHolds(context.runId));
        const holds = response.run_id === context.runId && Array.isArray(response.holds)
          ? response.holds.filter(h => h.generation === context.generation && h.inventory_guarded === true) : [];
        if (holds.length === 1 && HELD_PUBLICATION_ID_RE.test(holds[0]!.hold_id) && HELD_PUBLICATION_ID_RE.test(input.repoId)) {
          const binding = { holdId: holds[0]!.hold_id, repoId: input.repoId };
          const exact = await this.recoveryRPC(() => this.client.getExactRecoveryHold!(context.runId, context.generation, binding, ...maintenanceSignalArgs()));
          if (exact && exact.run_id === context.runId && exact.generation === context.generation &&
              exact.hold_id === binding.holdId && exact.repo_id === binding.repoId && exact.inventory_guarded === true) {
            if (exact.run_status === "cancelled" && Number.isSafeInteger(exact.current_claim_generation) &&
                exact.current_claim_generation >= context.generation) MaintenanceBudget.observeCancellation();
            if (exact.state === "released" || exact.state === "discarded") return undefined;
            if (exact.state === "open") exactHoldBinding = binding;
          }
        }
      }
      const freezeRemaining = async (): Promise<RecoveryRecord | undefined> => {
        MaintenanceBudget.current()?.check();
        if (input.settledEvidence === "publication") {
          try {
            const own = await this.recoveryRPC(() => this.client.getRunOwnership!(context.runId));
            if (own.status === "cancelled" && own.claim_generation === context.generation && own.inventory_guarded === true) MaintenanceBudget.observeCancellation();
            if (own.status !== "completed" || own.claim_generation !== context.generation || own.inventory_guarded !== true) return undefined;
          } catch { MaintenanceBudget.current()?.check(); return undefined; }
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
          const coverage = await ownedRecoveryIO(() => this.git.buildRecoveryCoverage!(context.barePath, context, aggregateRoots, input.currentSha));
          sourceSha = coverage.sha;
          digest = coverage.fingerprint;
        }
        return this.withJournalLock(context.runId, async () => {
          const records = await this.newReconstructionRecords(context.runId, context.generation);
          if (!records) return undefined;
          const matching = records.find(r => r.generation === context.generation && r.inventoryGuarded &&
            r.coverageDigest === digest && r.sourceSha === sourceSha &&
            r.inventoryCurrentSha === input.currentSha && r.originalSourceSha === originalSourceSha &&
            canonicalJson(r.coverageContext) === canonicalJson(context) &&
            canonicalJson(r.originalRoots) === canonicalJson(roots));
          if (matching) {
            if (exactHoldBinding && !matching.exactHoldBinding) {
              const bound = { ...matching, exactHoldBinding };
              await this.writeRecordUnlocked(bound);
              return bound;
            }
            return matching;
          }
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
            ...(exactHoldBinding ? { exactHoldBinding } : {}),
            ...(input.heldPolicy && validFrozenHeldPolicy(input.heldPolicy) ? { heldPolicy: input.heldPolicy } : {}),
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
      };
      return freezeRemaining();
    }), undefined, input.onCancellationObserved);
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
      catch (err) { MaintenanceBudget.current()?.check();
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") return "retained";
        bareExists = false;
      }
      if (bareExists) {
        if (!this.git.resolveRecoveryBareDir || !this.git.enumerateOwedCandidates || !this.git.readInventoryCloneHeads ||
            await ownedRecoveryIO(() => this.git.resolveRecoveryBareDir!(path.basename(barePath))) !== barePath) return "retained";
        const owed = await ownedRecoveryIO(() => this.git.enumerateOwedCandidates!(barePath, runId));
        if (owed.some(c => c.contexts.some(x => x.runId === runId))) return "retained";
        const clones = await ownedRecoveryIO(() => this.git.readInventoryCloneHeads!(barePath, runId));
        if (clones.kind !== "verified" || clones.clones.length > 0 || clones.heads.length > 0) return "retained";
      }
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.warn("recovery: unadopted-generation proof unavailable; custody retained", { run_id: runId, generation, error: errText(err) });
      return "retained";
    }
    for (let attempt = 0; ; attempt++) {
      try {
        const open = (await this.guardedHolds(runId)).filter(h => h.generation === generation);
        if (open.length === 0) return "settled"; // already closed (a replayed or concurrent settle)
        if (open.length !== 1) return "retained";
        const ack = await this.recoveryRPC(() => (MaintenanceBudget.current() ? this.client.releaseRecoveryCustody(runId, generation, "forge_no_output",
          { kind: "settled", coverage_digest: EMPTY_INVENTORY_DIGEST }, MaintenanceBudget.current()!.signal) : this.client.releaseRecoveryCustody(runId, generation, "forge_no_output",
          { kind: "settled", coverage_digest: EMPTY_INVENTORY_DIGEST })));
        if (ack.run_id === runId && ack.generation === generation && ack.released === true &&
            ack.holds_released === 1 && !ack.retained) return "settled";
        this.log.warn("recovery: unadopted-generation release not confirmed; custody retained", { run_id: runId, generation });
        return "retained";
      } catch (err) { MaintenanceBudget.current()?.check();
        const delay = this.unadoptedSettleRetryDelaysMs[attempt];
        if (delay === undefined || input.signal?.aborted) {
          this.log.warn("recovery: unadopted-generation release failed; custody retained", { run_id: runId, generation, error: errText(err) });
          return "retained";
        }
        const budget = MaintenanceBudget.current();
        budget?.check();
        const signals = [MaintenanceBudget.signal(), input.signal].filter((s): s is AbortSignal => s !== undefined);
        const signal = signals.length ? AbortSignal.any(signals) : undefined;
        await retryDelay(Math.min(delay, budget?.remainingMs() ?? delay), undefined, { signal, ref: false });
        MaintenanceBudget.current()?.check();
      }
    }
  }

  private async inactiveInventory(record: RecoveryRecord): Promise<boolean> {
    const generation = record.generation;
    if (this.isExecuting(record.runId) || !this.client.hasFeature?.("recovery_inventory_v1") ||
        !this.client.getRunOwnership || typeof generation !== "number" || !Number.isSafeInteger(generation) || generation <= 0) return false;
    let own;
    try { own = await this.recoveryRPC(() => this.client.getRunOwnership!(record.runId)); }
    catch (err) { MaintenanceBudget.current()?.check();
      if (!isRunOwnershipLost(err, record.runId)) return false;
      // A MAC-authenticated immutable FINAL may only replay; new work needs the exact open hold.
      return !!(record.inventoryGuarded && record.finalRequest) || await this.openInventoryHold(record);
    }
    if (!Number.isSafeInteger(own.claim_generation) || own.claim_generation! < generation) return false;
    if (own.status === "cancelled" && own.inventory_guarded === true) {
      MaintenanceBudget.observeCancellation();
    }
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
    const response = await this.recoveryRPC(() => this.client.listRecoveryHolds(runId));
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

  /** One read per boundary, no retries. Unknown evidence retains all sources and blocks writes.
   * A validated closed hold is settled, never a release ACK or local cleanup permission. */
  async heldOperationState(record: RecoveryRecord, signal?: AbortSignal): Promise<"open" | "settled" | "unknown"> {
    if (!MaintenanceBudget.isAttempt(`${record.runId}:${record.generation}`)) {
      return this.withCancellationAttempt(record, () => this.heldOperationState(record, signal));
    }
    try {
      const current = await this.requireRecord(record);
      if (current.holdSettlement) return "settled";
      // Legacy immutable FINALs retain their byte-identical replay authority after a lost ACK.
      if (!current.exactHoldBinding) return await this.openInventoryHold(current) ? "open" : "unknown";
      const exact = await this.recoveryRPC(() => this.client.getExactRecoveryHold?.(current.runId, current.generation!,
        current.exactHoldBinding!, maintenanceSignal(signal)));
      if (!exact || exact.run_id !== current.runId || exact.generation !== current.generation ||
          exact.hold_id !== current.exactHoldBinding.holdId || exact.repo_id !== current.exactHoldBinding.repoId ||
          exact.inventory_guarded !== true) return "unknown";
      if (exact.run_status === "cancelled" && Number.isSafeInteger(exact.current_claim_generation) &&
          exact.current_claim_generation >= current.generation!) {
        MaintenanceBudget.observeCancellation();
      }
      if (exact.state === "open") return "open";
      if (exact.state !== "released" && exact.state !== "discarded") return "unknown";
      await this.writeExistingRecord(current, cur => ({ ...cur, holdSettlement: exact }));
      return "settled";
    } catch { MaintenanceBudget.current()?.check(); return "unknown"; }
  }

  /** Only an existing immutable request can use replay authority. Closed observations
   * never authorize capture, upload, clearing a request, or a replacement FINAL. */
  private async finalHoldAllowsReplay(record: RecoveryRecord, state: "open" | "settled" | "unknown"): Promise<boolean> {
    if (state === "open") return true;
    const current = await this.requireRecord(record);
    if (!current.finalRequest) return false;
    // A saved, MAC-authenticated request may recover its original ACK even after
    // an API rollback removes exact observations. This grants no new FINAL or fallback.
    if (state === "unknown") return true;
    const settled = current.holdSettlement;
    return !!settled?.final_receipt &&
      canonicalJson(settled.final_receipt) === canonicalJson(current.finalRequest.disposition) &&
      (current.finalRequest.evidence === undefined || current.finalRequest.evidence === settled.release_evidence);
  }

  private async heldStopped(record: RecoveryRecord, state: "settled" | "unknown"): Promise<RecoveryOutcome> {
    if (record.finalRequest && !record.finalAcknowledged) {
      await this.retainFinal(record, state === "settled" ? "inventory_hold_settled" : "final_retry_unknown");
    }
    return { state: record.state, captureId: record.captureId,
      reason: state === "settled" ? "inventory_hold_settled" : "inventory_hold_unknown",
      held: state === "settled" ? "settled" : "kept" };
  }

  private async openInventoryHold(record: Pick<RecoveryRecord, "runId" | "generation">): Promise<boolean> {
    if (!this.client.hasFeature?.("recovery_inventory_v1") || !Number.isSafeInteger(record.generation) || record.generation! <= 0) return false;
    const response = await this.recoveryRPC(() => this.client.listRecoveryHolds(record.runId));
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
      } catch { MaintenanceBudget.current()?.check(); /* Logging cannot affect custody. */ }
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
      const inventory = await ownedRecoveryIO(() => this.git.readInventoryCloneHeads?.(context.barePath, context.runId));
      if (inventory?.kind !== "verified" || !Array.isArray(inventory.clones))
        return refuse({ check: "inventory_read_not_verified", cause: inventoryReadCause(inventory) });
      if (inventory.clones.length !== 0) return refuse({ check: "physical_sources_without_boundary" });
      return true;
    };
    try {
      if (!await prove()) return "retained";
      await action(prove);
      return "passed";
    } catch (err) { MaintenanceBudget.current()?.check();
      refuse({ check: "boundary_exception" });
      throw err;
    }
  }

  private async finalizeInventory(snapshot: RecoveryRecord, reconcile = true): Promise<RecoveryOutcome | void> {
    return this.withCancellationAttempt(snapshot, () => this.finalizeInventoryInAttempt(snapshot, reconcile));
  }

  private async finalizeInventoryInAttempt(snapshot: RecoveryRecord, reconcile = true): Promise<RecoveryOutcome | void> {
    if (!snapshot.coverageContext || typeof snapshot.generation !== "number") return;
    // Exact closure can refuse work before mutable health/source probes. It grants no ACK or cleanup.
    const current = await this.requireRecord(snapshot);
    if (current.finalRequest && !current.finalAcknowledged && current.exactHoldBinding) {
      const hold = await this.heldOperationState(current);
      if (hold === "settled" && !await this.finalHoldAllowsReplay(current, hold)) return this.heldStopped(current, hold);
    }
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

  /** ACK-only report retirement never settles or rewrites recovery evidence. */
  async retireAcknowledgedFinalReport(
    runId: string, generation: number,
    budget: { signal: AbortSignal; deadline: number },
    reserve: (action: (ownTailEligible: () => boolean) => Promise<void>) => Promise<void>,
    inspectSources: (bare: string, covered: (head: string) => Promise<boolean>,
      laterHead: (head: string) => boolean) => Promise<boolean>,
    remove: (ownTailEligible: () => boolean) => Promise<void>,
  ): Promise<void> {
    return this.withCancellationAttempt({ runId, generation, inventoryGuarded: true } as RecoveryRecord,
      () => this.retireAcknowledgedFinalReportWithinAttempt(runId, generation, budget, reserve, inspectSources, remove));
  }

  private async retireAcknowledgedFinalReportWithinAttempt(
    runId: string, generation: number,
    budget: { signal: AbortSignal; deadline: number },
    reserve: (action: (ownTailEligible: () => boolean) => Promise<void>) => Promise<void>,
    inspectSources: (bare: string, covered: (head: string) => Promise<boolean>,
      laterHead: (head: string) => boolean) => Promise<boolean>,
    remove: (ownTailEligible: () => boolean) => Promise<void>,
  ): Promise<void> {
    if (!this.enabled || !Number.isSafeInteger(generation) || generation <= 0 ||
        !this.git.withBareLock || !this.git.withReportProofBudget ||
        !this.git.enumerateOwedUnderLock || !this.git.readInventoryCloneHeads ||
        !this.git.ancestry || !this.client.getRunOwnership || !this.client.withAcknowledgedFinalReportCustody) return;
    const check = () => {
      budget.signal.throwIfAborted();
      if (Date.now() >= budget.deadline || residueQuarantine() !== undefined) throw new Error("report proof unavailable");
    };
    await runCaptureCycle(path.resolve(this.runDir(runId), `generation-${generation}`), "skip",
      () => reserve(ownTailEligible => this.withJournalLock(runId, async () => {
        check();
        const sources = await this.checkedRecords(runId, budget);
        const ackRecords = sources.filter(r => r.generation === generation && r.inventoryGuarded === true &&
          r.finalAcknowledged === true && r.finalRequest?.disposition.kind === "archive");
        if (ackRecords.length !== 1) return;
        const records = ackRecords.filter(r => r.generation === generation && r.inventoryGuarded === true &&
          r.state === "uploaded" && r.serverCaptureId && r.coverageDigest && r.coverageContext &&
          r.originalSourceSha && r.inventoryCurrentSha && Array.isArray(r.originalRoots) &&
          /^[0-9a-f]{40}$/.test(r.sourceSha) && /^[0-9a-f]{64}$/.test(r.checksum ?? "") &&
          Number.isSafeInteger(r.byteSize) && r.byteSize! > 0 &&
          r.reason !== "inventory_snapshot_changed" && r.reason !== "inventory_quiescence_breach");
        if (records.length !== 1) return;
        const archive = records[0]!;
        const unresolved = sources.filter(r => r.finalRequest && r.finalAcknowledged !== true);
        if (unresolved.length !== 1 || unresolved.some(r => r.generation !== generation || r.inventoryGuarded !== true ||
            r.finalRequest?.disposition.kind !== "settled" || r.finalRequest.evidence !== "publication" ||
            r.coverageDigest !== EMPTY_INVENTORY_DIGEST || r.originalRoots?.length !== 0 ||
            canonicalJson(r.coverageContext) !== canonicalJson(archive.coverageContext) ||
            !r.originalSourceSha || !r.inventoryCurrentSha ||
            r.reason === "inventory_snapshot_changed" || r.reason === "inventory_quiescence_breach")) return;
        for (const record of records) {
          check();
          const bare = record.bareDir && await ownedRecoveryIO(() => this.git.withReportProofBudget!(budget,
            () => this.git.resolveRecoveryBareDir?.(record.bareDir!) ?? Promise.resolve(undefined)));
          if (!bare || bare !== record.coverageContext!.barePath) continue;
          const removed = await ownedRecoveryIO(() => this.git.withReportProofBudget!(budget, () => this.git.withBareLock!(bare, async () => {
            check();
            const ownership = await this.recoveryRPC(() => this.client.getRunOwnership!(runId, budget));
            if (ownership?.status === "cancelled" && ownership.claim_generation === generation && ownership.inventory_guarded === true) MaintenanceBudget.observeCancellation();
            if (!ownership || ownership.status !== "completed" ||
                !Number.isSafeInteger(ownership.claim_generation) || ownership.claim_generation !== generation ||
                ownership.inventory_guarded !== true) return false;
            const status = await this.recoveryRPC(() => this.client.getRecoveryCaptureStatus(runId, record.serverCaptureId!, budget));
            if (!healthyInventoryStatus(status, record, this.now())) return false;
            check();
            const owed = await ownedRecoveryIO(() => this.git.enumerateOwedUnderLock!(bare, runId));
            const settledEarlier = await this.settledEarlierGenerations(sources, generation);
            const laterContext = (c: OwedCandidate["contexts"][number]) =>
              Number.isSafeInteger(c.generation) && c.generation! > generation;
            const laterHead = (sha: string) => {
              const journals = sources.filter(r => !r.coverageDigest && r.sourceSha === sha);
              const producers = owed.filter(c => c.sha === sha).flatMap(c => c.contexts);
              return journals.length + producers.length > 0 &&
                journals.every(r => Number.isSafeInteger(r.generation) && r.generation! > generation) &&
                producers.every(laterContext);
            };
            const covered = async (head: string) => {
              check();
              return /^[0-9a-f]{40}$/.test(head) &&
                await ownedRecoveryIO(() => this.git.ancestry!(bare, head, record.sourceSha)) === "ancestor";
            };
            for (const source of sources) {
              if (source.captureId === record.captureId ||
                  (Number.isSafeInteger(source.generation) &&
                    (source.generation! > generation || settledEarlier.has(source.generation!)))) continue;
              if (source.coverageDigest && (!source.originalSourceSha || !source.inventoryCurrentSha ||
                  !Array.isArray(source.originalRoots))) return false;
              if (source.coverageDigest && canonicalJson(source.coverageContext) !== canonicalJson(record.coverageContext)) return false;
              // Earlier aggregates' synthetic tips need not be archive ancestors; only a competing
              // unacknowledged FINAL's own source must be covered (its frozen heads alone are not).
              const heads = source.coverageDigest
                ? [...(source.finalRequest && !source.finalAcknowledged ? [source.sourceSha] : []), source.originalSourceSha!, source.inventoryCurrentSha!, ...source.originalRoots!.map(r => r.sha)]
                : [source.sourceSha];
              for (const head of heads) if (!await covered(head)) return false;
            }
            for (const root of owed) {
              const contexts = root.contexts.filter(c => !laterContext(c) &&
                !(Number.isSafeInteger(c.generation) && settledEarlier.has(c.generation!)));
              if (!contexts.length) continue;
              if (!record.originalRoots!.some(frozen => root.sha === frozen.sha &&
                  contexts.every(c => frozen.contexts.some(p => canonicalJson(p) === canonicalJson(c)))) ||
                  !await covered(root.sha)) return false;
            }
            // Frozen original/current heads remain requirements even if their pins disappeared.
            for (const head of [record.originalSourceSha!, record.inventoryCurrentSha!,
                ...record.originalRoots!.map(r => r.sha)]) if (!await covered(head)) return false;
            if (!await inspectSources(bare, covered, laterHead)) return false;
            check();
            if (canonicalJson(await this.checkedRecords(runId, budget)) !== canonicalJson(sources) ||
                canonicalJson(await ownedRecoveryIO(() => this.git.enumerateOwedUnderLock!(bare, runId))) !== canonicalJson(owed)) return false;
            if (!ownTailEligible() || !healthyInventoryStatus(status, record, this.now())) return false;
            await this.client.withAcknowledgedFinalReportCustody!(runId, generation, budget, async custodyEligible => {
              MaintenanceBudget.current()?.check();
              const eligible = () => ownTailEligible() && custodyEligible() && !budget.signal.aborted &&
                Date.now() < budget.deadline && residueQuarantine() === undefined &&
                healthyInventoryStatus(status, record, this.now());
              if (eligible()) await remove(eligible);
              MaintenanceBudget.current()?.check();
            });
            return true;
          })));
          if (removed) return;
        }
      }, budget.signal)));
  }


  /** C -> S -> J -> B -> O. This operation consumes archive observations only;
   * the callback performs the conditional exact unlink before any boundary releases.
   * At most 256 journals and their captures are considered; a bad candidate retains
   * its report, without granting FINAL, capture or cleanup authority. */
  async retireArchivedReport(
    runId: string, generation: number,
    budget: { signal: AbortSignal; deadline: number },
    reserve: (action: (ownTailEligible: () => boolean) => Promise<void>) => Promise<void>,
    inspectSources: (bare: string, covered: (head: string) => Promise<boolean>,
      laterHead: (head: string) => boolean) => Promise<boolean>,
    remove: (ownTailEligible: () => boolean) => Promise<void>,
  ): Promise<void> {
    return this.withCancellationAttempt({ runId, generation, inventoryGuarded: true } as RecoveryRecord,
      () => this.retireArchivedReportWithinAttempt(runId, generation, budget, reserve, inspectSources, remove));
  }

  private async retireArchivedReportWithinAttempt(
    runId: string, generation: number,
    budget: { signal: AbortSignal; deadline: number },
    reserve: (action: (ownTailEligible: () => boolean) => Promise<void>) => Promise<void>,
    inspectSources: (bare: string, covered: (head: string) => Promise<boolean>,
      laterHead: (head: string) => boolean) => Promise<boolean>,
    remove: (ownTailEligible: () => boolean) => Promise<void>,
  ): Promise<void> {
    if (!this.enabled || !Number.isSafeInteger(generation) || generation <= 0 ||
        !this.git.withBareLock || !this.git.withReportProofBudget ||
        !this.git.enumerateOwedUnderLock || !this.git.readInventoryCloneHeads ||
        !this.git.ancestry || !this.client.getRunOwnership) return;
    const check = () => {
      budget.signal.throwIfAborted();
      if (Date.now() >= budget.deadline || residueQuarantine() !== undefined) throw new Error("report proof unavailable");
    };
    await runCaptureCycle(path.resolve(this.runDir(runId), `generation-${generation}`), "skip",
      () => reserve(ownTailEligible => this.withJournalLock(runId, async () => {
        check();
        const sources = await this.checkedRecords(runId, budget);
        const records = sources.filter(r => r.generation === generation && r.inventoryGuarded === true &&
          r.state === "uploaded" && r.serverCaptureId && r.coverageDigest && r.coverageContext &&
          r.originalSourceSha && r.inventoryCurrentSha && Array.isArray(r.originalRoots) &&
          /^[0-9a-f]{40}$/.test(r.sourceSha) && /^[0-9a-f]{64}$/.test(r.checksum ?? "") &&
          Number.isSafeInteger(r.byteSize) && r.byteSize! > 0 &&
          r.reason !== "inventory_snapshot_changed" && r.reason !== "inventory_quiescence_breach");
        if (records.length !== 1) return;
        for (const record of records) {
          check();
          const bare = record.bareDir && await ownedRecoveryIO(() => this.git.withReportProofBudget!(budget,
            () => this.git.resolveRecoveryBareDir?.(record.bareDir!) ?? Promise.resolve(undefined)));
          if (!bare || bare !== record.coverageContext!.barePath) continue;
          const removed = await ownedRecoveryIO(() => this.git.withReportProofBudget!(budget, () => this.git.withBareLock!(bare, async () => {
            check();
            const ownership = await this.recoveryRPC(() => this.client.getRunOwnership!(runId, budget));
            if (ownership?.status === "cancelled" && ownership.claim_generation === generation && ownership.inventory_guarded === true) MaintenanceBudget.observeCancellation();
            if (!ownership || !["completed", "failed", "cancelled"].includes(ownership.status) ||
                !Number.isSafeInteger(ownership.claim_generation) || ownership.claim_generation !== generation ||
                (ownership.inventory_guarded !== undefined && typeof ownership.inventory_guarded !== "boolean")) return false;
            const status = await this.recoveryRPC(() => this.client.getRecoveryCaptureStatus(runId, record.serverCaptureId!, budget));
            if (!healthyInventoryStatus(status, record, this.now())) return false;
            check();
            const owed = await ownedRecoveryIO(() => this.git.enumerateOwedUnderLock!(bare, runId));
            const settledEarlier = await this.settledEarlierGenerations(sources, generation);
            const laterContext = (c: OwedCandidate["contexts"][number]) =>
              Number.isSafeInteger(c.generation) && c.generation! > generation;
            const laterHead = (sha: string) => {
              const journals = sources.filter(r => !r.coverageDigest && r.sourceSha === sha);
              const producers = owed.filter(c => c.sha === sha).flatMap(c => c.contexts);
              return journals.length + producers.length > 0 &&
                journals.every(r => Number.isSafeInteger(r.generation) && r.generation! > generation) &&
                producers.every(laterContext);
            };
            const covered = async (head: string) => {
              check();
              return /^[0-9a-f]{40}$/.test(head) &&
                await ownedRecoveryIO(() => this.git.ancestry!(bare, head, record.sourceSha)) === "ancestor";
            };
            for (const source of sources) {
              if (source.captureId === record.captureId ||
                  (Number.isSafeInteger(source.generation) &&
                    (source.generation! > generation || settledEarlier.has(source.generation!)))) continue;
              if (source.coverageDigest && (!source.originalSourceSha || !source.inventoryCurrentSha ||
                  !Array.isArray(source.originalRoots))) return false;
              const heads = source.coverageDigest
                ? [...(source.finalRequest && !source.finalAcknowledged ? [source.sourceSha] : []), source.originalSourceSha!, source.inventoryCurrentSha!, ...source.originalRoots!.map(r => r.sha)]
                : [source.sourceSha];
              for (const head of heads) if (!await covered(head)) return false;
            }
            for (const root of owed) {
              const contexts = root.contexts.filter(c => !laterContext(c) &&
                !(Number.isSafeInteger(c.generation) && settledEarlier.has(c.generation!)));
              if (!contexts.length) continue;
              if (!record.originalRoots!.some(frozen => root.sha === frozen.sha &&
                  contexts.every(c => frozen.contexts.some(p => canonicalJson(p) === canonicalJson(c)))) ||
                  !await covered(root.sha)) return false;
            }
            // Frozen original/current heads remain requirements even if their pins disappeared.
            for (const head of [record.originalSourceSha!, record.inventoryCurrentSha!,
                ...record.originalRoots!.map(r => r.sha)]) if (!await covered(head)) return false;
            if (!await inspectSources(bare, covered, laterHead)) return false;
            check();
            if (canonicalJson(await this.checkedRecords(runId, budget)) !== canonicalJson(sources) ||
                canonicalJson(await ownedRecoveryIO(() => this.git.enumerateOwedUnderLock!(bare, runId))) !== canonicalJson(owed)) return false;
            if (!ownTailEligible()) return false;
            await remove(ownTailEligible);
            return true;
          })));
          if (removed) return;
        }
      }, budget.signal)));
  }

  private async frozenCoverageProven(record: RecoveryRecord): Promise<boolean> {
    // Every still-unresolved root must be covered; an earlier archive cannot close a later inventory.
    if (!record.coverageContext || !this.git.enumerateOwedCandidates) return false;
    const unresolved = await ownedRecoveryIO(() => this.git.enumerateOwedCandidates!(record.coverageContext!.barePath, record.runId));
    const frozenRoots = record.originalRoots ?? [];
    // Authenticate every physical source record: listRecords deliberately skips unreadable
    // files for inspection, which cannot prove that FINAL covers the run's sources.
    if (!this.git.readInventoryCloneHeads || !this.git.ancestry) return false;
    let sources: RecoveryRecord[];
    try { sources = await this.checkedRecords(record.runId); } catch { MaintenanceBudget.current()?.check(); return false; }
    const clones = await ownedRecoveryIO(() => this.git.readInventoryCloneHeads!(record.coverageContext!.barePath, record.runId));
    if (clones.kind !== "verified") return false;
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
      if (await ownedRecoveryIO(() => this.git.ancestry!(record.coverageContext!.barePath, head, record.sourceSha)) !== "ancestor") {
        await this.writeExistingRecord(record, cur => ({ ...cur, reason: cur.finalRequest
          ? "inventory_quiescence_breach" : "inventory_snapshot_changed" }));
        return false;
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
      return false;
    }
    if (!record.inventoryCurrentSha || !record.originalSourceSha || !this.git.buildRecoveryCoverage) return false;
    if (record.originalRoots?.length || record.finalRequest?.disposition.kind !== "settled") {
      const roots = [...new Set([...frozenRoots.map(c => c.sha), record.originalSourceSha])].sort();
      // Use the producer's full proof. Optional archive-redundancy claims impose
      // smaller head/body/encoding limits that cannot become FINAL eligibility limits.
      const coverage = await ownedRecoveryIO(() => this.git.buildRecoveryCoverage!(record.coverageContext!.barePath, record.coverageContext!,
        roots, record.inventoryCurrentSha!));
      if (coverage.sha !== record.sourceSha || coverage.fingerprint !== record.coverageDigest) return false;
    }
    return true;
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
    // Issue #2545: a held_publication FINAL rests on the forge ref the api verified, not on retained
    // archive bytes, so it needs no bundle; every other check below still applies to it.
    const heldFinal = record.finalRequest?.disposition.kind === "held_publication";
    if (record.finalRequest?.disposition.kind !== "settled" && !heldFinal) {
      if (record.selfContained !== true || !Array.isArray(record.prerequisiteShas) ||
          record.prerequisiteShas.length !== 0 || !record.bundlePath) return;
      try {
        const header = await readRecoveryBundleHeader(record.bundlePath, record.sourceSha);
        if (!header.selfContained || header.prerequisiteShas.length !== 0) return;
      } catch { MaintenanceBudget.current()?.check(); return; }
    }
    if (!await this.frozenCoverageProven(record)) return;
    // Validate the archive before both minting and replaying FINAL. A committed receipt
    // remains authoritative when its capture has expired or its status is unusable.
    let observedStatus: RecoveryCaptureStatusResponse | undefined;
    if (record.finalRequest?.disposition.kind !== "settled" && !heldFinal) {
      if (record.state !== "uploaded" || !record.serverCaptureId!) return;
      try { observedStatus = await this.recoveryRPC(() => this.client.getRecoveryCaptureStatus(record.runId, record.serverCaptureId!)); }
      catch { MaintenanceBudget.current()?.check(); if (reconcile) return this.reconcileInventory(record, prove); return; }
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      if (!healthyInventoryStatus(observedStatus, record, this.now())) {
        if (reconcile) return this.reconcileInventory(record, prove, observedStatus);
        return;
      }
    }
    if (!record.finalRequest) {
      const hold = await this.heldOperationState(record);
      if (hold !== "open") return this.heldStopped(record, hold);
      if (!(await this.openInventoryHold(record)) || !await prove() || residueQuarantine() !== undefined) return;
      record = await this.writeExistingRecord(record, cur => ({
        ...cur, finalRequest: { disposition: { kind: "archive", capture_id: cur.serverCaptureId!,
          source_sha: cur.sourceSha, coverage_digest: cur.coverageDigest! } },
      }));
    }
    const request = record.finalRequest!;
    if (request.disposition.kind === "held_publication") return this.releaseHeldFinal(record, request, prove);
    // The journaled identity was frozen under the exact open guarded hold.
    // Publication proof was checked at minting; replay grants no reserve/upload authority.
    try {
      if (!this.client.hasFeature?.("recovery_inventory_v1") || !(await this.inactiveInventory(record))) return;
      if (!await prove() || !await this.inactiveInventory(record)) return;
      // issue #2213: synchronous check immediately before the release (nothing awaited in between).
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      const hold = await this.heldOperationState(record);
      if (hold !== "open" && !await this.finalHoldAllowsReplay(record, hold)) return this.heldStopped(record, hold);
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      const ack = await this.recoveryRPC(() => (MaintenanceBudget.current() ? this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition, MaintenanceBudget.current()!.signal) : this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition)));
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
      MaintenanceBudget.current()?.check();
      this.onAuthoritativeGenerationReleased?.(record.runId, record.generation!);
      await this.forgetGenerationWithinCycle(record.runId, record.generation!);
      return { state: "uploaded", captureId: record.captureId };
    } catch (err) { MaintenanceBudget.current()?.check();
      const cls = await classifyUploadFailure(err, record);
      await this.retainFinal(record, cls.kind === "credential" ? "credential_rejected" :
        err instanceof RequestError && [403, 409].includes(err.status) ? "final_retry_refused" : "final_retry_unknown", err,
        cls.kind === "credential" || (err instanceof RequestError && [403, 409].includes(err.status)));
      if (cls.kind === "credential") this.credentialBlockedAt = this.now();
      // retainFinal changed the authenticated journal. Reconciliation's guarded reason
      // write must compare against that snapshot, while keeping the same saved FINAL.
      const latest = await this.readLatest(record);
      const reconciled = latest.kind === "ok" && this.finalRetryIdentity(latest.record) === this.finalRetryIdentity(record)
        ? await this.reconcileInventory(latest.record, prove, observedStatus) : undefined;
      if (reconciled && reconciled.reason !== "final_retry_unknown") return reconciled;
      return { state: record.state, captureId: record.captureId,
        reason: cls.kind === "transient" ? cls.reason ?? "upload_transient" : cls.reason };
    }
  }

  /**
   * Issue #2545: send the journaled held_publication FINAL and classify the answer.
   *  - released exactly once: record the ACK and run the same local cleanup as an archive FINAL;
   *  - retained with a definitive reason (the forge ref is gone or foreign, the hold is no longer
   *    eligible, the api withdrew the feature) or a downgrade 400 from an api that does not know the
   *    kind: clear the held request and report `fallback`, so the caller captures an archive;
   *  - anything else (`held_publication_unavailable`, a transport error, an unreadable answer, a
   *    client-side feature absence thrown before sending): keep the journaled request and the open
   *    hold. A held FINAL is never cleared for lack of fresh proof.
   * The caller holds the record's capture-cycle lock.
   */
  private async releaseHeldFinal(record: RecoveryRecord, request: NonNullable<RecoveryRecord["finalRequest"]>,
    prove: () => Promise<boolean>): Promise<RecoveryOutcome | void> {
    const kept = (reason?: string): RecoveryOutcome => ({ state: record.state, captureId: record.captureId,
      ...(reason ? { reason } : {}), held: "kept" });
    const clearHeld = async (why: string): Promise<RecoveryOutcome> => {
      const hold = await this.heldOperationState(record);
      if (hold !== "open") return this.heldStopped(record, hold);
      if (!this.newProductionAllowed(record)) return kept();
      const cleared = await this.writeExistingRecord(record, cur =>
        !cur.finalAcknowledged && canonicalJson(cur.finalRequest) === canonicalJson(request)
          ? (() => { const next = { ...cur }; delete next.finalRequest; delete next.reason; return next; })()
          : undefined);
      if (cleared.finalRequest !== undefined) return kept(why);
      this.log.warn("recovery: held publication final cleared; falling back to the archive", {
        run_id: record.runId, generation: record.generation, reason: why });
      return { state: cleared.state, captureId: cleared.captureId, reason: why, held: "fallback" };
    };
    try {
      if (!this.client.hasFeature?.("recovery_inventory_v1") || !(await this.inactiveInventory(record))) return kept();
      if (!await prove() || !await this.inactiveInventory(record)) return kept();
      if (residueQuarantine() !== undefined) return { ...quarantinedOutcome(record.captureId), held: "kept" };
      const hold = await this.heldOperationState(record);
      if (hold !== "open" && !await this.finalHoldAllowsReplay(record, hold)) return this.heldStopped(record, hold);
      if (residueQuarantine() !== undefined) return { ...quarantinedOutcome(record.captureId), held: "kept" };
      if (hold === "settled" && (await this.requireRecord(record)).holdSettlement?.state !== "released") return this.heldStopped(record, "settled");
      let replayProof: RecoveryExactHold | undefined;
      if (hold === "settled" && !this.client.hasFeature?.("recovery_held_publication_v1") &&
          !this.client.hasHeldPublicationV2?.()) {
        // Re-read the MAC-verified journal after observation: only its unchanged saved body
        // and exact binding can recover an ACK. Policy settlements never qualify.
        const current = await this.requireRecord(record);
        const settled = current.holdSettlement;
        if (!current.exactHoldBinding || settled?.state !== "released" ||
            settled.final_receipt?.kind !== "held_publication" ||
            settled.run_id !== current.runId || settled.generation !== current.generation ||
            settled.hold_id !== current.exactHoldBinding.holdId || settled.repo_id !== current.exactHoldBinding.repoId ||
            canonicalJson(current.finalRequest) !== canonicalJson(request) ||
            canonicalJson(settled.final_receipt) !== canonicalJson(request.disposition) ||
            (request.evidence !== undefined && request.evidence !== settled.release_evidence)) return this.heldStopped(record, "settled");
        replayProof = settled;
      }
      const ack = await this.recoveryRPC(() => replayProof
        ? this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition, MaintenanceBudget.current()?.signal, replayProof)
        : (MaintenanceBudget.current() ? this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition, MaintenanceBudget.current()!.signal) : this.client.releaseRecoveryCustody(record.runId, record.generation, request.evidence, request.disposition)));
      if (!ack || typeof ack !== "object") return kept("held_publication_unreadable_answer");
      if (ack.retained === true) {
        const reason: HeldFinalRetainedReason | undefined = typeof ack.reason === "string" &&
          (HELD_FINAL_RETAINED_REASONS as readonly string[]).includes(ack.reason)
          ? ack.reason as HeldFinalRetainedReason : undefined;
        if (ack.run_id !== record.runId || ack.generation !== record.generation || ack.released !== false ||
            ack.holds_released !== 0 || reason === undefined) return kept("held_publication_unreadable_answer");
        if (HELD_FINAL_FALLBACK_REASONS.has(reason)) return await clearHeld(reason);
        this.log.warn("recovery: held publication final retained by the api; exact request kept", {
          run_id: record.runId, generation: record.generation, reason });
        return kept(reason);
      }
      if (ack.run_id !== record.runId || ack.generation !== record.generation || ack.released !== true || ack.holds_released !== 1) {
        return kept("held_publication_unreadable_answer");
      }
      if (residueQuarantine() !== undefined) {
        this.log.warn("recovery: held final release returned while the worker is quarantined; local evidence kept unacknowledged",
          { run_id: record.runId, generation: record.generation });
        return { ...quarantinedOutcome(record.captureId), held: "kept" };
      }
      await this.writeExistingRecord(record, cur => ({ ...cur, finalAcknowledged: true }));
      MaintenanceBudget.current()?.check();
      this.onAuthoritativeGenerationReleased?.(record.runId, record.generation!);
      await this.forgetGenerationWithinCycle(record.runId, record.generation!);
      return { state: record.state, captureId: record.captureId, held: "acked" };
    } catch (err) { MaintenanceBudget.current()?.check();
      // 400 is the strict-decode refusal of an api that rolled back past this kind. Everything else
      // (transport, timeout, 5xx, the typed client-side feature absence) proves nothing: keep.
      if (err instanceof RequestError && err.status === 400) return await clearHeld("held_publication_downgrade_400");
      const cls = await classifyUploadFailure(err, record);
      await this.retainFinal(record, cls.kind === "credential" ? "credential_rejected" :
        err instanceof RequestError && [403, 409].includes(err.status) ? "final_retry_refused" : "final_retry_unknown", err,
        cls.kind === "credential" || (err instanceof RequestError && [403, 409].includes(err.status)));
      if (cls.kind === "credential") this.credentialBlockedAt = this.now();
      return kept(cls.kind === "transient" ? cls.reason ?? "upload_transient" : cls.reason);
    }
  }

  /**
   * Issue #2545: journal and (when the run has stopped executing) send a held_publication FINAL for
   * the frozen record whose `publicationId` the api created on the forge (step A answered `created`).
   *
   * The FINAL is MINTED here, under the exact open guarded hold, on the caller's local quiescence
   * proof (the same proof `freezeInventory` accepts with `locallyQuiescent`): the record then carries
   * the identity for a crash or a lost ACK to replay verbatim. It is SENT here only when this
   * coordinator no longer sees the run executing; in a live flight the runner's source boundary
   * would refuse the proof, so the journaled request is left for the live re-drive after execute().
   *  - `acked`: the api released the hold against the forge ref; local cleanup ran.
   *  - `kept`: the journaled held request and the open hold stay (pending, non-definitive).
   *  - `fallback`: nothing held is journaled (it could not be minted, or the api definitively refused
   *    and the request was cleared); the caller captures an archive on the SAME record.
   * Never throws.
   */
  async releaseHeldPublication(record: RecoveryRecord, publicationId: string): Promise<"acked" | "kept" | "fallback" | "settled"> {
    if (!this.enabled || !HELD_PUBLICATION_ID_RE.test(publicationId)) return "fallback";
    if (residueQuarantine() !== undefined) return "kept";
    try {
      return await this.withCancellationAttempt(record, async () =>
        runCaptureCycle(await this.cycleKey(record), "wait", () => this.releaseHeldWithinCycle(record, publicationId)));
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.warn("recovery: held publication final not completed; custody retained", {
        run_id: record.runId, generation: record.generation, error: errText(err) });
      return "kept";
    }
  }

  private async releaseHeldWithinCycle(record: RecoveryRecord, publicationId: string): Promise<"acked" | "kept" | "fallback" | "settled"> {
    let latest = await this.readLatest(record);
    if (latest.kind !== "ok") return "kept" as const;
    let cur = latest.record;
    if (cur.finalAcknowledged) return "acked" as const;
    if (!cur.finalRequest) {
      const hold = await this.heldOperationState(cur);
      if (hold !== "open") return hold === "settled" ? "settled" as const : "kept" as const;
    }
    if (!cur.inventoryGuarded || !cur.coverageDigest || cur.captureId !== record.captureId ||
        typeof cur.generation !== "number" || !this.client.hasFeature?.("recovery_inventory_v1")) return "fallback" as const;
    if (cur.finalRequest && cur.finalRequest.disposition.kind !== "held_publication") return "kept" as const;
    if (!cur.finalRequest) {
      if (!(await this.openInventoryHold(cur)) || residueQuarantine() !== undefined) return "kept" as const;
      if (!this.newProductionAllowed(cur)) return "kept" as const;
      cur = await this.writeExistingRecord(cur, c => c.finalRequest || c.finalAcknowledged ? undefined : ({
        ...c, finalRequest: { disposition: { kind: "held_publication", publication_id: publicationId,
          source_sha: c.sourceSha, coverage_digest: c.coverageDigest! } },
      }));
      if (cur.finalRequest?.disposition.kind !== "held_publication") return "kept" as const;
    }
    // The replayed identity wins over a differing caller id: it is what the api may already hold.
    if (this.isExecuting(cur.runId)) return "kept" as const;
    const outcome = await this.finalizeInventory(cur, false);
    if (outcome?.held) return outcome.held;
    latest = await this.readLatest(cur);
    if (latest.kind !== "ok") return "kept" as const;
    if (latest.record.finalAcknowledged) return "acked" as const;
    return latest.record.finalRequest?.disposition.kind === "held_publication" ? "kept" as const : "fallback" as const;
  }

  /** One atomic reconciliation attempt; failures keep custody and every saved identity. */
  private async reconcileInventory(record: RecoveryRecord, prove: () => Promise<boolean>, status?: RecoveryCaptureStatusResponse): Promise<RecoveryOutcome | void> {
    if (!record.serverCaptureId || !record.coverageDigest || !record.generation ||
        !hasJournaledBundle(record) || !this.client.hasFeature?.("recovery_inventory_v1")) return;
    const unknown = (): RecoveryOutcome | void => record.finalRequest
      ? { state: record.state, captureId: record.captureId, reason: "final_retry_unknown" } : undefined;
    try {
      if (!await prove() || !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
      if (!this.client.reconcileRecoveryCapture) return unknown();
      const response = await this.recoveryRPC(() => this.client.reconcileRecoveryCapture!(record.runId, record.serverCaptureId!, {
        generation: record.generation!, source_sha: record.sourceSha, coverage_digest: record.coverageDigest!,
        checksum: record.checksum!, byte_size: record.byteSize!,
      }));
      if (residueQuarantine() !== undefined) return quarantinedOutcome(record.captureId);
      if (!response || typeof response !== "object" || Array.isArray(response) ||
          response.run_id !== record.runId || response.generation !== record.generation ||
          response.capture_id !== record.serverCaptureId!) return unknown();
      if (response.outcome === "accepted") {
        const receipt = response.final_receipt;
        const saved = record.finalRequest;
        if (!saved || !receipt || response.reason !== undefined ||
            typeof receipt.coverage_digest !== "string" || !/^[0-9a-f]{64}$/.test(receipt.coverage_digest) ||
            (receipt.kind === "archive" && (typeof receipt.capture_id !== "string" ||
              typeof receipt.source_sha !== "string" || !/^[0-9a-f]{40}$/.test(receipt.source_sha))) ||
            (receipt.kind === "settled" && (receipt.capture_id !== undefined || receipt.source_sha !== undefined ||
              !["publication", "forge_no_output"].includes(response.release_evidence ?? ""))) ||
            // Issue #2545: the api echoes a held receipt with its publication id; it is accepted only
            // when it equals the saved request exactly (the equality below), never on its own.
            (receipt.kind === "held_publication" && (receipt.capture_id !== undefined ||
              typeof receipt.publication_id !== "string" || !HELD_PUBLICATION_ID_RE.test(receipt.publication_id) ||
              typeof receipt.source_sha !== "string" || !/^[0-9a-f]{40}$/.test(receipt.source_sha))) ||
            canonicalJson(receipt) !== canonicalJson(saved.disposition) ||
            response.release_evidence !== saved.evidence ||
            (receipt.kind !== "archive" && receipt.kind !== "settled" && receipt.kind !== "held_publication")) return unknown();
        if (!await prove() || !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
        const current = await this.writeExistingRecord(record, cur =>
          residueQuarantine() === undefined && canonicalJson(cur.finalRequest) === canonicalJson(saved) &&
          cur.generation === record.generation && cur.serverCaptureId === record.serverCaptureId && !cur.finalAcknowledged
            ? { ...cur, finalAcknowledged: true } : undefined);
        MaintenanceBudget.current()?.check();
        if (current.finalAcknowledged) this.onAuthoritativeGenerationReleased?.(record.runId, record.generation);
        return;
      }
      if (response.outcome === "retained") {
        // api/internal/recovery/reconcile.go emits only these bounded protocol reasons.
        if (response.final_receipt !== undefined || response.release_evidence !== undefined ||
            typeof response.reason !== "string" || (!INVENTORY_RECONCILE_ATTENTION_REASONS.has(response.reason) &&
              response.reason !== "capture_available" && response.reason !== "generation_not_ended")) return unknown();
        const reason = response.reason;
        if (reason === "capture_available" || reason === "generation_not_ended") return unknown();
        const current = await this.writeExistingRecord(record, cur =>
          residueQuarantine() === undefined && !cur.finalAcknowledged &&
          cur.reason !== reason && canonicalJson(cur) === canonicalJson(record) ? { ...cur, reason } : undefined);
        if (current.reason !== reason) return;
        if (record.reason !== reason) this.log.warn("recovery: inventory requires owner attention", {
          run_id: current.runId, generation: current.generation, reason,
        });
        return { state: current.state, captureId: current.captureId, reason };
      }
      if (response.final_receipt !== undefined || response.release_evidence !== undefined ||
          response.outcome !== "replaceable" || response.reason !== undefined ||
          !status || !replaceableInventoryStatus(status, record, this.now())) return unknown();
      if (!await this.openInventoryHold(record)) return;
      if (!this.newProductionAllowed(record)) return { state: record.state, captureId: record.captureId, held: "kept" };
      if (!await prove() || !await this.inactiveInventory(record) || residueQuarantine() !== undefined) return;
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
    } catch (err) { MaintenanceBudget.current()?.check();
      if (record.finalRequest) await this.retainFinal(record, "final_retry_unknown", err);
      else this.log.warn("recovery: capture reconciliation pending; custody retained", { run_id: record.runId, error: errText(err) });
      return unknown();
    }
  }

  /** One probe per entry, before queueing. Unknown custody grants no new authority.
   * Nested work keeps even an expired ambient object; only its creator disposes it. */
  private async withCancellationAttempt<T>(record: RecoveryRecord, action: () => Promise<T>, attempts?: Map<string, MaintenanceBudget>, onCancellationObserved?: () => MaintenanceBudget): Promise<T> {
    const key = `${record.runId}:${record.generation}`;
    if (MaintenanceBudget.isAttempt(key)) {
      MaintenanceBudget.current()?.check();
      return action();
    }
    let owned: MaintenanceBudget | undefined;
    const factory = () => {
      const cached = attempts?.get(key);
      if (cached) return cached;
      const budget = onCancellationObserved?.() ?? this.testHooks?.maintenanceBudget?.() ?? new MaintenanceBudget();
      attempts?.set(key, budget);
      if (!attempts && !onCancellationObserved) owned = budget;
      return budget;
    };
    try {
      return await MaintenanceBudget.runAttempt(key, factory, async () => {
        // An expired object from this pass must stop before journal reads or the entry probe.
        if (attempts?.has(key)) MaintenanceBudget.observeCancellation();
        MaintenanceBudget.current()?.check();
        let trusted = record;
        if (record.captureId && !MaintenanceBudget.current()) {
          try { trusted = await this.requireRecord(record); }
          catch (error) { if (error instanceof RecordGoneError) return action(); throw error; }
        }
        if (!MaintenanceBudget.current()) {
          try {
            if (trusted.finalRequest && trusted.holdSettlement &&
                !await this.finalHoldAllowsReplay(trusted, "settled")) {
              // Refusal-only work needs neither a fresh exact probe nor a cancellation budget.
            } else if (trusted.finalRequest && trusted.exactHoldBinding && !trusted.holdSettlement) {
              await this.heldOperationState(trusted);
            } else if (trusted.exactHoldBinding) {
              const binding = trusted.exactHoldBinding;
              const exact = await this.recoveryRPC(() => this.client.getExactRecoveryHold?.(trusted.runId, trusted.generation!, binding));
              if (exact && exact.run_id === trusted.runId && exact.generation === trusted.generation &&
                  exact.hold_id === binding.holdId && exact.repo_id === binding.repoId &&
                  exact.inventory_guarded === true && ["open", "released", "discarded"].includes(exact.state) &&
                  exact.run_status === "cancelled" && Number.isSafeInteger(exact.current_claim_generation) &&
                  exact.current_claim_generation >= trusted.generation!) MaintenanceBudget.observeCancellation();
            } else {
              const own = await this.recoveryRPC(() => this.client.getRunOwnership?.(trusted.runId));
              if (own && own.status === "cancelled" && Number.isSafeInteger(own.claim_generation) &&
                  typeof trusted.generation === "number" && own.claim_generation! >= trusted.generation &&
                  (!trusted.inventoryGuarded || own.inventory_guarded === true)) MaintenanceBudget.observeCancellation();
            }
          } catch { MaintenanceBudget.current()?.check(); /* Missing status grants no maintenance authority. */ }
        }
        const run = async () => {
          MaintenanceBudget.current()?.check();
          const result = await action();
          MaintenanceBudget.current()?.check();
          return result;
        };
        const budget = MaintenanceBudget.current();
        return budget && this.git.withRecoveryOperation
          ? this.git.withRecoveryOperation(budget.signal, Infinity, run) : run();
      });
    } finally { owned?.dispose(); }
  }

  /** Shared foreground/boot/live ordering, serialized once per frozen generation. */
  async operateFrozenInventory(input: CaptureInput, options: FrozenOperationOptions = {}): Promise<RecoveryOutcome> {
    if (!this.enabled || residueQuarantine() !== undefined) return quarantinedOutcome(input.record.captureId);
    return this.withCancellationAttempt(input.record, async () =>
      runCaptureCycle(await this.cycleKey(input.record), "wait",
        () => this.frozenOperationCycle(input, options)), undefined, input.onCancellationObserved);
  }

  private async frozenOperationCycle(input: CaptureInput, options: FrozenOperationOptions = {}): Promise<RecoveryOutcome> {
    let record = await this.requireRecord(input.record);
    const outcome = (held: NonNullable<RecoveryOutcome["held"]>): RecoveryOutcome =>
      ({ state: record.state, captureId: record.captureId, held });
    if (record.finalAcknowledged) return outcome("acked");
    // Immutable FINAL and valid self-contained archives always precede new held work.
    const selfContainedArchive = record.state === "uploaded" && record.selfContained === true &&
      record.prerequisiteShas?.length === 0;
    const replayedHeld = record.finalRequest?.disposition.kind === "held_publication";
    if (record.finalRequest || selfContainedArchive) {
      const final = await this.finalizeInventory(record);
      const latest = await this.readLatest(record);
      if (latest.kind === "gone") return final ?? outcome("acked");
      if (latest.kind !== "ok") return outcome("kept");
      record = latest.record;
      // Reconciliation may have invalidated the old capture and moved this same
      // record back to bundled. Judge the fresh state before deciding to stop.
      const stillArchived = record.state === "uploaded" && record.selfContained === true &&
        record.prerequisiteShas?.length === 0;
      if (record.finalRequest || record.finalAcknowledged || stillArchived) return final ?? outcome("kept");
      if (replayedHeld) {
        const hold = await this.heldOperationState(record, input.signal);
        if (hold !== "open") return this.heldStopped(record, hold);
        return this.captureCycle({ ...input, record });
      }
    }
    let hold = await this.heldOperationState(record, input.signal);
    if (hold !== "open") return this.heldStopped(record, hold);
    if (record.heldCreateUncertain) {
      if (!this.client.publishHeld || input.signal?.aborted || residueQuarantine() !== undefined) return outcome("kept");
      try {
        // HeldPublishGate reconciles an existing invocation before reading the body.
        // Without a marker, the empty invalid pack fails pre-verification before heldInvoke.
        // Known invocations remain read-back-only. An empty-pack refusal can also
        // mean the first request never arrived; a full retry still uses the same server marker.
        const response = await this.recoveryRPC(() => this.client.publishHeld!(record.runId, record.generation!, record.sourceSha,
          record.coverageDigest!, Buffer.alloc(0), maintenanceSignal(input.signal)));
        if (!response.ok && response.reason !== "pack_invalid") return outcome("kept");
        if (response.ok) {
          const body = response.body;
          if (body.tip !== record.sourceSha || body.ref !== `refs/uzi-held/${record.runId}/${record.generation}` ||
              !HELD_PUBLICATION_ID_RE.test(body.publication_id)) return outcome("kept");
          if (body.state === "created") {
            record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: false,
              finalRequest: { disposition: { kind: "held_publication", publication_id: body.publication_id,
                source_sha: cur.sourceSha, coverage_digest: cur.coverageDigest! } } }));
            const released = await this.releaseHeldWithinCycle(record, body.publication_id);
            if (released !== "fallback") return outcome(released);
          } else if (body.state === "refused" && body.reason === "create_refused") {
            record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: false }));
          } else return outcome("kept");
          hold = await this.heldOperationState(record, input.signal);
          if (hold !== "open") return this.heldStopped(record, hold);
          return this.captureCycle({ ...input, record });
        }
        // An empty probe can fail before any invocation exists. It is not proof
        // of absence: retry the same frozen pack through the API's existing
        // locked at-most-once invocation marker, never a second remote create.
      } catch { MaintenanceBudget.current()?.check(); return outcome("kept"); }
    }
    if (!this.newProductionAllowed(record)) {
      return hasJournaledBundle(record) ? this.captureCycle({ ...input, record }) : outcome("kept");
    }
    const uncertainCreate = record.heldCreateUncertain === true;
    const v2 = this.client.hasHeldPublicationV2?.() === true;
    // Missing new held metadata cannot grant publication authority, but it must
    // preserve the pre-existing guarded archive path for boot and legacy journals.
    if (v2 && (!record.exactHoldBinding || !record.heldPolicy)) return uncertainCreate ? outcome("kept") : this.captureCycle({ ...input, record });
    let terminal = options.terminal;
    if (v2) {
      terminal = undefined;
      try {
        const exact = record.exactHoldBinding && await this.recoveryRPC(() => this.client.getExactRecoveryHold?.(
          record.runId, record.generation!, record.exactHoldBinding!, maintenanceSignal(input.signal)));
        if (exact && exact.run_id === record.runId && exact.generation === record.generation &&
            exact.hold_id === record.exactHoldBinding!.holdId && exact.repo_id === record.exactHoldBinding!.repoId &&
            exact.inventory_guarded === true && exact.state === "open" &&
            Number.isSafeInteger(exact.current_claim_generation) && exact.current_claim_generation >= record.generation! &&
            ["failed", "cancelled", "completed"].includes(exact.run_status)) {
          terminal = { status: exact.run_status as "failed" | "cancelled" | "completed",
            fail_origin: options.terminal?.fail_origin };
          if (exact.run_status === "cancelled") MaintenanceBudget.observeCancellation();
        }
      } catch { MaintenanceBudget.current()?.check(); /* Unknown terminal eligibility retains the archive policy. */ }
    } else if (!options.foreground) {
      try {
        const own = await this.recoveryRPC(() => this.client.getRunOwnership?.(record.runId));
        if (own?.status === "cancelled" && own.inventory_guarded === true && Number.isSafeInteger(own.claim_generation) && own.claim_generation! >= record.generation!) MaintenanceBudget.observeCancellation();
        terminal = own && Number.isSafeInteger(own.claim_generation) &&
          own.claim_generation! >= record.generation! &&
          ["failed", "cancelled", "completed"].includes(own.status)
          ? { status: own.status as "failed" | "cancelled" | "completed" } : undefined;
      } catch { MaintenanceBudget.current()?.check(); terminal = undefined; }
    }
    const eligible = isCodePublishingKind(record.kind) && terminal && (v2 || (options.foreground && terminal.status === "failed" &&
      this.client.hasFeature?.("recovery_held_publication_v1"))) &&
      terminal.fail_origin !== "push_secret_blocked" && terminal.fail_origin !== "worker_residue_blocked";
    if (uncertainCreate && !eligible) return outcome("kept");
    // Completed runs attempt the archive first, on precisely this identity.
    let archived: RecoveryOutcome | undefined;
    if (!eligible || (terminal?.status === "completed" && !uncertainCreate)) {
      archived = await this.captureCycle({ ...input, record });
      const latest = await this.readLatest(record);
      if (latest.kind === "gone") return archived;
      if (latest.kind !== "ok") return outcome("kept");
      record = latest.record;
      if (!eligible || record.finalRequest || record.finalAcknowledged) return archived;
      const thin = record.selfContained === false && Array.isArray(record.prerequisiteShas) &&
        record.prerequisiteShas.length > 0 && hasJournaledBundle(record);
      if (!thin && !record.archiveRefusal) return archived;
    }
    if (residueQuarantine() !== undefined) return archived ?? outcome("kept");
    if (!record.heldPolicy || !this.client.publishHeld) return uncertainCreate ? outcome("kept") : archived ?? this.captureCycle({ ...input, record });
    let publicationId: string | undefined;
    let result: "fallback" | "kept" | "settled" = uncertainCreate ? "kept" : "fallback";
    const publish = async (prove: () => Promise<boolean>) => {
      if (!await prove() || !await this.frozenCoverageProven(record)) { result = "kept"; return; }
      const git = this.git;
      const prepare = async () => options.prepare ? options.prepare(record) :
        git.resolveCheckpointRange && git.secretScanCheckpointRange && git.ancestry &&
        git.rangeTouchesWorkflows && git.changedFilesNoRenames && git.checkpointPack
          ? await prepareFrozenHeld({ git: git as HeldGit, barePath: input.barePath,
            branch: record.branch, sourceSha: record.sourceSha, policy: record.heldPolicy!, deadlineMs: 30_000 })
          : { reason: "preparation_unavailable" };
      const prepared = await prepare().catch(() => ({ reason: "preparation_failed" }));
      if (!("pack" in prepared)) {
        // A fresh finding remains a remediation hold after restart even if a later
        // scanner changes its verdict on these same immutable bytes.
        if (prepared.reason === "secret_found") {
          record = await this.writeExistingRecord(record, cur => ({ ...cur,
            heldPolicy: { ...cur.heldPolicy!, blocked: true } }));
        }
        if (!await prove() || !await this.frozenCoverageProven(record)) result = "kept";
        return;
      }
      if (!await prove() || !await this.frozenCoverageProven(record)) { result = "kept"; return; }
      hold = await this.heldOperationState(record, input.signal);
      if (hold !== "open") { result = hold === "settled" ? "settled" : "kept"; return; }
      if (input.signal?.aborted || residueQuarantine() !== undefined) { result = "kept"; return; }
      // V2 reconciles lost create answers before any retry. V1 retains its legacy
      // archive fallback, whose FINAL supplies the generation settlement authority.
      if (v2) record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: true }));
      result = v2 || uncertainCreate ? "kept" : "fallback";
      const response = await this.recoveryRPC(() => this.client.publishHeld!(record.runId, record.generation!, record.sourceSha,
        record.coverageDigest!, prepared.pack, maintenanceSignal(input.signal)));
      if (!response.ok) {
        if (uncertainCreate) return; // Refusal alone cannot erase an earlier invocation's uncertainty.
        if (v2 && (response.reason === "malformed_response" ||
            (response.httpStatus !== undefined && (response.httpStatus < 400 || response.httpStatus >= 500)))) return;
        if (v2) record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: false }));
        result = "fallback";
        return;
      }
      const body = response.body;
      if (body.tip !== record.sourceSha ||
          body.ref !== `refs/uzi-held/${record.runId}/${record.generation}` ||
          !HELD_PUBLICATION_ID_RE.test(body.publication_id)) return;
      if (body.state === "refused" && body.reason === "create_refused") {
        if (v2) record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: false }));
        result = "fallback";
        return;
      }
      if (body.state !== "created") return;
      publicationId = body.publication_id;
      // Save FINAL before leaving the source boundary, without recursively acquiring the cycle.
      record = await this.writeExistingRecord(record, cur => ({ ...cur, heldCreateUncertain: false,
        finalRequest: { disposition: { kind: "held_publication", publication_id: publicationId!,
          source_sha: cur.sourceSha, coverage_digest: cur.coverageDigest! } } }));
    };
    try {
      if (options.foreground) await publish(async () => true);
      else if (record.coverageContext && record.generation) {
        const boundary = await this.inventorySourceBoundary({ ...record.coverageContext!, generation: record.generation }, publish);
        if (boundary !== "passed") result = "kept";
      } else result = "kept";
    } catch { MaintenanceBudget.current()?.check(); result = v2 || uncertainCreate ? "kept" : "fallback"; }
    if (publicationId) {
      const released = await this.releaseHeldWithinCycle(record, publicationId);
      if (released !== "fallback") return outcome(released);
      result = "fallback";
    }
    if (result !== "fallback") return outcome(result);
    // Every fallback observes current authoritative settlement again.
    hold = await this.heldOperationState(record, input.signal);
    if (hold !== "open") return this.heldStopped(record, hold);
    return archived ?? this.captureCycle({ ...input, record: await this.requireRecord(record) });
  }

  private async resumeInventory(record: RecoveryRecord, signal?: AbortSignal, isExecuting = this.isExecuting): Promise<RecoveryOutcome | void> {
    if (record.holdSettlement && !record.finalRequest && !record.finalAcknowledged && !record.completionReceipt) return this.heldStopped(record, "settled");
    if (record.completionReceipt) {
      await this.forgetCompletedGeneration(record.runId, record.generation!, isExecuting, signal);
      return;
    }
    if (record.finalAcknowledged) {
      await this.forgetGenerationWithinCycle(record.runId, record.generation!, isExecuting, signal);
      return;
    }
    if (this.isExecuting(record.runId) || !isLiveCandidate(record) || !record.coverageDigest || record.finalAcknowledged ||
        !this.finalRetryReady(record)) return;
    if (record.finalRequest && record.holdSettlement && !await this.finalHoldAllowsReplay(record, "settled")) {
      return this.heldStopped(record, "settled");
    }
    // A saved FINAL replays before resolving inputs needed only for a new capture.
    // In particular, a verified empty inventory has no bundle or bare to rebuild.
    let archiveFallback = false;
    if (record.finalRequest) {
      const outcome = await this.finalizeInventory(record);
      await this.paceFinalRetry(record, outcome);
      const latest = await this.readLatest(record);
      if (latest.kind !== "ok" || latest.record.finalRequest || latest.record.finalAcknowledged) return outcome;
      archiveFallback = outcome?.held === "fallback";
      record = latest.record;
    }
    if (!await this.inactiveInventory(record)) return;
    const barePath = record.bareDir && await ownedRecoveryIO(() => this.git.resolveRecoveryBareDir?.(record.bareDir!));
    if (!barePath) return;
    const input = { record, barePath, defaultBranch: record.defaultBranch ?? "main", signal };
    if (archiveFallback) {
      const hold = await this.heldOperationState(record, signal);
      if (hold !== "open") return this.heldStopped(record, hold);
      return this.captureCycle(input);
    }
    return this.frozenOperationCycle(input);
  }

  /** Credential-free discovery precedes the journal snapshot; authentication is required
   * before materializing anything. A failure blocks discovery, never implies empty work. */
  async snapshotOwedInventory(): Promise<void> {
    if (!this.git.discoverOwedCandidates) throw new Error("inventory discovery unavailable");
    const discovered = await ownedRecoveryIO(() => this.git.discoverOwedCandidates!());
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

  async materializeBootInventory(signal?: AbortSignal, isExecuting = this.isExecuting, attempts?: Map<string, MaintenanceBudget>): Promise<RecoveryRecord[]> {
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
        const hold = (await this.recoveryRPC(() => this.client.listRecoveryHolds(ctx.runId))).holds.find(h =>
          h.generation === ctx.generation && h.inventory_guarded === true);
        if (!hold || !this.client.getRunOwnership) continue; // closed/discarded never reopened
        const existing = await this.newReconstructionRecords(ctx.runId, ctx.generation);
        if (!existing) continue;
        const probe = existing.find(r => r.generation === ctx.generation && r.exactHoldBinding) ??
          { runId: ctx.runId, generation: ctx.generation, inventoryGuarded: true } as RecoveryRecord;
        const materialize = async (): Promise<void> => {
        if (!(await this.inactiveInventory(probe))) return;
        // Issue #2545: a generation with a journaled held_publication FINAL has its identity fixed by
        // that request. A second snapshot (it would differ in its original source) would only mint a
        // redundant archive beside the held release, so the pending request is replayed instead.
        if (existing.some(r => r.generation === ctx.generation && r.inventoryGuarded && !r.finalAcknowledged &&
            r.finalRequest?.disposition.kind === "held_publication")) return;
        const original = existing.find(r => r.generation === ctx.generation && !r.coverageDigest);
        const frozenContext = existing.find(r => r.generation === ctx.generation && r.coverageContext)?.coverageContext;
        // A generation has one stable snapshot authority even when its original roots
        // carry several branch contexts. A changed source still gets a new immutable key.
        const preferred = frozenContext ?? this.bootOwed
          .filter(e => e.context.runId === ctx.runId && e.context.generation === ctx.generation)
          .map(e => e.context)
          .sort((a, b) => Number(b.branch === original?.branch) - Number(a.branch === original?.branch) ||
            canonicalJson(a).localeCompare(canonicalJson(b)))[0];
        if (!preferred || canonicalJson(preferred) !== canonicalJson(ctx)) return;
        const originalSourceSha = original?.sourceSha;
        let currentSha = originalSourceSha ?? entry.candidates[0]?.sha;
        const owned = await ownedRecoveryIO(() => this.git.committedTrackingOwnership?.(ctx.barePath, ctx.branch, ctx.runId, undefined, ctx.generation));
        if (owned?.kind === "owned" && canonicalJson(owned.context) === canonicalJson(ctx)) currentSha = owned.sha;
        if (!currentSha) return; // No adopted source or positive tracking proof is not an empty snapshot.
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
        };
        await this.withCancellationAttempt(probe, materialize, attempts);
      } catch (err) { MaintenanceBudget.current()?.check();
        this.log.warn("recovery: boot inventory retained; reconstruction failed", { run_id: ctx.runId, generation: ctx.generation, error: errText(err) });
      }
    }
    return records;
  }

  // ── D3/D5: produce + verify + journal-before-manifest + upload ──────────────────

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
    return this.withCancellationAttempt(input.record, async () =>
      runCaptureCycle(await this.cycleKey(input.record), "wait", () => this.captureCycle(input)), undefined, input.onCancellationObserved);
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
      if (record.inventoryGuarded && !record.finalAcknowledged && !record.finalRequest) {
        const hold = await this.heldOperationState(record, input.signal);
        if (hold !== "open") return this.heldStopped(record, hold);
      }
      if (record.inventoryGuarded && (!record.coverageDigest || !this.client.hasFeature?.("recovery_inventory_v1"))) {
        return { state: "needs_action", captureId: record.captureId, reason: "inventory_not_frozen_or_feature_unavailable" };
      }
      if (record.inventoryGuarded && !record.finalRequest && !(await this.openInventoryHold(record))) {
        return { state: "needs_action", captureId: record.captureId, reason: "inventory_hold_closed" };
      }
      if (record.finalRequest || record.state === "uploaded") {
        const wasHeld = record.finalRequest?.disposition.kind === "held_publication";
        const finalOutcome = record.inventoryGuarded ? await this.finalizeInventory(record) : undefined;
        record = await this.requireRecord(record);
        // Issue #2545: a held FINAL the api definitively refused is cleared by the replay above; the
        // record then has no request and no bundle yet, and falls through to the archive capture
        // below on this very pass.
        const heldCleared = wasHeld && !record.finalRequest && !record.finalAcknowledged;
        if (!heldCleared && (record.state !== "bundled" || record.finalRequest || record.finalAcknowledged)) {
          return finalOutcome ?? { state: record.state, captureId: record.captureId };
        }
      }
      if (record.inventoryGuarded && !record.finalRequest && !record.finalAcknowledged) {
        const hold = await this.heldOperationState(record, input.signal);
        if (hold !== "open") return this.heldStopped(record, hold);
      }
      if (record.state === "needs_action" && record.reason && PERMANENT_UPLOAD_REASONS.has(record.reason)) {
        return { state: "needs_action", captureId: record.captureId, reason: record.reason };
      }
      if (!hasJournaledBundle(record) && !this.newProductionAllowed(record)) {
        return { state: record.state, captureId: record.captureId, held: "kept" };
      }
      if (input.attemptedHeadSha && !record.attemptedHeadSha && this.newProductionAllowed(record)) {
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
    } catch (err) { MaintenanceBudget.current()?.check();
      if (err instanceof RecordGoneError) return removed(err);
      this.log.warn("recovery: capture/upload failed; retaining source (needs_action)", {
        run_id: record.runId,
        capture_id: record.captureId,
        error: errText(err),
      });
      try {
        return await this.markFailure(record, "capture_error", true);
      } catch (markErr) { MaintenanceBudget.current()?.check();
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
    record = await this.requireRecord(record);
    if (!this.newProductionAllowed(record)) return { kind: "needs_action", reason: "maintenance_admission_required" };
    // Resolve a FRESH, verified forge tip (D5) inside the reap-before-credentialed-git
    // boundary (the agent is already reaped at finalization). Best-effort: a fetch failure
    // falls back to a self-contained bundle rather than blocking capture. The runner clone's
    // private origin/main and any checkpoint wrapper are NEVER consulted here.
    let forgeTip: string | undefined;
    try {
      if (!record.inventoryGuarded) forgeTip = await ownedRecoveryIO(() => this.git.fetchDefaultTip!(
        input.barePath,
        input.defaultBranch,
        input.forgePat,
        input.cloneUrl,
        input.forgeUsername,
      ));
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.warn("recovery: could not fetch a fresh forge tip; producing a self-contained bundle", {
        run_id: record.runId,
        error: errText(err),
      });
    }
    const tmpPath = this.bundleTmpPath(record);
    await ownedRecoveryIO(() => fs.mkdir(path.dirname(tmpPath), { recursive: true, mode: 0o700 }));
    let result: RecoveryBundleResult;
    try {
      result = await ownedRecoveryIO(() => this.git.produceRecoveryBundle!(input.barePath, {
        sourceSha: record.sourceSha,
        outPath: tmpPath,
        ...(record.inventoryGuarded ? { guardedDefaultBranch: input.defaultBranch } : { forgeTip }),
      }));
    } catch (err) { MaintenanceBudget.current()?.check();
      await ownedRecoveryIO(() => fs.rm(tmpPath, { force: true })).catch(() => undefined);
      if (err instanceof RecoveryBundleTooLargeError) {
        this.log.warn("recovery: bundle exceeds the size limit; retaining source (needs_action)", {
          run_id: record.runId,
          byte_size: err.byteSize,
          max_bytes: err.maxBytes,
        });
        await this.writeExistingRecord(record, cur => ({ ...cur, archiveRefusal: "oversize" }));
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
      await ownedRecoveryIO(() => fs.rm(tmpPath, { force: true })).catch(() => undefined);
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
        if (!this.newProductionAllowed(cur)) return { kind: "source_advanced" as const };
        const header = cur.inventoryGuarded ? await readRecoveryBundleHeader(tmpPath, cur.sourceSha) : undefined;
        await ownedRecoveryIO(() => fs.rename(tmpPath, finalPath));
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
        } catch (err) { MaintenanceBudget.current()?.check();
          await ownedRecoveryIO(() => fs.rm(finalPath, { force: true })).catch(() => undefined);
          throw err;
        }
        return { kind: "bundled" as const, record: bundled };
      });
    } finally {
      await ownedRecoveryIO(() => fs.rm(tmpPath, { force: true })).catch(() => undefined);
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
          const hold = await this.heldOperationState(current, signal);
          if (hold !== "open") return this.heldStopped(current, hold);
          // issue #2213: a latch that landed during the hold read stops the reserve.
          if (residueQuarantine() !== undefined) return quarantinedOutcome(current.captureId);
        }
        reserved = await this.recoveryRPC(() => this.client.reserveRecoveryCapture(current.runId, {
          run_id: current.runId,
          idempotency_key: current.reserveIdempotencyKey ?? current.captureId,
          source_sha: current.sourceSha,
          ...(current.inventoryGuarded ? { coverage_digest: current.coverageDigest } : {}),
          ...(current.attemptedHeadSha ? { attempted_head_sha: current.attemptedHeadSha } : {}),
          // Bind the reserve to the EXACT generation's hold (PRD #1349 M2, D1); a v1 record with
          // no generation omits it and the server falls back to the newest-hold reserve.
          ...(current.generation !== undefined ? { generation: current.generation } : {}),
        }));
      } catch (err) { MaintenanceBudget.current()?.check();
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
    if (current.inventoryGuarded) {
      const hold = await this.heldOperationState(current, signal);
      if (hold !== "open") return this.heldStopped(current, hold);
    }
    if (residueQuarantine() !== undefined) return quarantinedOutcome(current.captureId);
    let status: RecoveryCaptureStatusResponse;
    try {
      status = await this.recoveryRPC(() => this.client.uploadRecoveryBundle(
        current.runId,
        current.serverCaptureId!,
        manifest,
        createReadStream(current.bundlePath!, { signal: maintenanceSignal(signal) }),
        maintenanceSignal(signal),
      ));
    } catch (err) { MaintenanceBudget.current()?.check();
      return this.failUpload(current, err, signal, mode);
    }
    // A stream already in flight when cleanup ran cannot be recalled; this guarded write then
    // throws RecordGoneError instead of recreating the removed record.
    const uploaded = await this.writeExistingRecord(current, (cur) => ({ ...cur, state: "uploaded", reason: undefined }));
    if (!uploaded.inventoryGuarded) {
      // In capture mode a runner-driven settle is still executing, so this usually retains `executing`; the live pass
      // or boot deletes the pin once the run is no longer executing.
      await this.cleanupUploadedRecoveryPin(uploaded, { mode, signal });
    }
    // Bytes are durable on the server; free the local copy. The small journal record stays
    // (state=uploaded) so a restart sweep skips it.
    if (!uploaded.inventoryGuarded) await ownedRecoveryIO(() => fs.rm(current.bundlePath!, { force: true })).catch(() => undefined);
    this.log.info("recovery: bundle uploaded (durable capture)", {
      run_id: current.runId,
      capture_id: current.captureId,
      server_capture_id: current.serverCaptureId,
      server_state: status.state,
    });
    return { state: "uploaded", captureId: current.captureId };
  }

  /**
   * Delete the transient recovery pin of an archived (`uploaded`) record, once. Journal lock first
   * (held across the authenticated re-read and the destructive call), then the primitive's own
   * attribution and bare locks. Writes nothing to the journal: a retry is idempotent
   * (`already_absent`). Never throws; every refusal is a bounded `retained` reason.
   */
  private async cleanupUploadedRecoveryPin(
    record: RecoveryRecord,
    opts: { mode: UploadedPinCleanupMode; signal?: AbortSignal; deadline?: number },
  ): Promise<UploadedPinCleanupResult> {
    const cleanup = this.git.cleanupArchivedRecoveryPin;
    if (!cleanup) return { state: "retained", reason: "unsupported" };
    const { runId, captureId } = record;
    const controller = new AbortController();
    const deadline = Math.min(Date.now() + PIN_CLEANUP_RECORD_MS, opts.deadline ?? Infinity);
    const timer = setTimeout(() => controller.abort(), Math.max(0, deadline - Date.now()));
    timer.unref?.();
    const signal = opts.signal ? AbortSignal.any([opts.signal, controller.signal]) : controller.signal;
    let generation = record.generation ?? 0;
    let result: UploadedPinCleanupResult;
    try {
      result = await this.withJournalLock(runId, async (): Promise<UploadedPinCleanupResult> => {
        const retained = (reason: string): UploadedPinCleanupResult => ({ state: "retained", reason });
        let records: RecoveryRecord[];
        try { records = await this.checkedRecords(runId, { signal, deadline }); }
        catch { MaintenanceBudget.current()?.check(); return retained("unverified_journal"); }
        const cur = records.find((r) => r.captureId === captureId);
        if (!cur) return retained("record_gone");
        generation = cur.generation ?? 0;
        if (!archiveProvesPin(cur)) return retained("archive_unproven");
        if (records.some((r) => r.inventoryGuarded)) return retained("inventory_guarded");
        if (records.some((r) => r.captureId !== captureId && (r.generation ?? 0) === generation && r.state !== "uploaded")) {
          return retained("sibling_pending");
        }
        if (residueQuarantine() !== undefined) return retained("quarantined");
        if (this.isExecuting(runId)) return retained("executing");
        // Forward each authenticated directory independently: the primitive refuses a disagreement.
        MaintenanceBudget.current()?.check();
        const out = await cleanup.call(this.git, {
          runId,
          generation,
          sourceSha: cur.sourceSha,
          ...(cur.recoveryPinBareDir ? { recoveryPinBareDir: cur.recoveryPinBareDir } : {}),
          ...(cur.bareDir ? { bareDir: cur.bareDir } : {}),
          signal,
          deadline,
          isEligible: () => residueQuarantine() === undefined && !this.isExecuting(runId) && !signal.aborted,
        });
        MaintenanceBudget.current()?.check();
        return out.state === "retained" ? retained(out.reason) : out;
      }, signal);
    } catch { MaintenanceBudget.current()?.check();
      // Includes lock-wait aborts and any primitive failure; raw error text never leaves here.
      result = { state: "retained", reason: signal.aborted ? "deadline" : "cleanup_failed" };
    } finally {
      clearTimeout(timer);
    }
    if (result.state === "retained") {
      // `executing` is the expected answer while the run is still running: info, not a warning.
      const level = result.reason === "executing" ? "info" : "warn";
      this.log[level]("recovery: uploaded record pin retained", { run_id: runId, generation, reason: result.reason, mode: opts.mode });
    } else {
      this.log.info("recovery: uploaded record pin cleanup", { run_id: runId, generation, state: result.state, mode: opts.mode });
    }
    return result;
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
    if (cls.kind !== "credential" && cls.archiveRefusal) {
      const archiveRefusal = cls.archiveRefusal;
      record = await this.writeExistingRecord(record, cur => ({ ...cur, archiveRefusal }));
    }
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
    await ownedRecoveryIO(() => fs.rm(this.bundlePath(record), { force: true })).catch(() => undefined);
    if (record.bundlePath && record.bundlePath !== this.bundlePath(record)) {
      await ownedRecoveryIO(() => fs.rm(record.bundlePath!, { force: true })).catch(() => undefined);
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
      const res = await this.recoveryRPC(() => (MaintenanceBudget.current() ? this.client.releaseRecoveryCustody(runId, generation, releaseEvidence, undefined, MaintenanceBudget.current()!.signal) : this.client.releaseRecoveryCustody(runId, generation, releaseEvidence)));
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
        MaintenanceBudget.current()?.check();
        this.onAuthoritativeGenerationReleased?.(runId, generation);
      }
    } catch (err) { MaintenanceBudget.current()?.check();
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
      const res = await this.recoveryRPC(() => this.client.listRecoveryHolds(runId));
      return res.holds ?? [];
    } catch (err) { MaintenanceBudget.current()?.check();
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
   * Non-guarded `uploaded` records are included so {@link resumePending} can retry their archived
   * recovery-pin cleanup (the upload phase still skips them, and {@link isLiveCandidate} is false).
   */
  async snapshotBootRecords(): Promise<RecoveryRecord[]> {
    if (!this.enabled) return [];
    let runDirs: string[];
    try {
      runDirs = await fs.readdir(this.recoveryRoot);
    } catch { MaintenanceBudget.current()?.check();
      return [];
    }
    const out: RecoveryRecord[] = [];
    for (const runId of runDirs) {
      for (const record of await this.listRecords(runId)) {
        out.push(record);
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
  resumePending(signal?: AbortSignal, snapshot?: RecoveryRecord[], discover = false): Promise<void> {
    if (!this.enabled || this.passShutdown.signal.aborted || signal?.aborted) return Promise.resolve();
    signal = signal ? AbortSignal.any([signal, this.passShutdown.signal]) : this.passShutdown.signal;
    // issue #1995: installed SYNCHRONOUSLY (before the first await) so a heartbeat-driven
    // resumeLive can never overlap the boot sweep: it returns at once while any pass is in flight.
    return this.trackPass(() => this.resumePendingPass(signal, snapshot, discover));
  }

  /** Register before the runner releases its exact local tail. The observation selects
   * saved evidence only; every replay retains its own locks and fresh source proofs. */
  resumeTerminalSource(runId: string, tail: Promise<ClosedSourceObservation | void>, generation: number,
    signal?: AbortSignal): Promise<void> {
    if (!this.enabled || this.passShutdown.signal.aborted || signal?.aborted ||
        residueQuarantine() !== undefined) return Promise.resolve();
    const passSignal = signal ? AbortSignal.any([signal, this.passShutdown.signal]) : this.passShutdown.signal;
    return this.trackPass(async () => {
      // Join actual settlement even if shutdown arrives while the execution still owns work.
      const observation = await tail;
      if (passSignal.aborted || residueQuarantine() !== undefined || !observation ||
          !/^[a-zA-Z0-9_-]{1,128}(?![\s\S])/.test(runId) || observation.genesis.runId !== runId ||
          !Number.isSafeInteger(observation.genesis.generation) || observation.genesis.generation <= 0 || observation.genesis.generation > generation ||
          (observation.terminal !== "normal_failed" && observation.terminal !== "normal_blocked")) return;
      const family = observation.family;
      if (!family || family.observedExecution.runId !== runId || family.observedExecution.generation !== generation ||
          !Number.isSafeInteger(family.observedExecution.generation) || family.observedExecution.generation <= 0 ||
          (observation.terminal === "normal_failed" &&
            family.observedExecution.generation !== observation.genesis.generation) ||
          family.originals.length === 0 || family.originals.length > 128 || !family.metadata ||
          !family.originals.every(source => source.runId === runId &&
            Number.isSafeInteger(source.generation) && source.generation > 0 && source.generation <= generation && !!source.identity) ||
          !family.originals.some(source => source.generation === observation.genesis.generation &&
            source.clonePath === observation.genesis.clonePath && source.attemptId === observation.genesis.attemptId)) return;
      const attempts = new Map<string, MaintenanceBudget>();
      try {
        await this.savedEvidenceOnly.run({ signal: passSignal, entries: 0, bytes: 0, exhausted: false }, async () => {
          const records = await this.savedSourceCandidates(runId, passSignal);
          // One bounded candidate snapshot, one attempt per record. Unreadable siblings
          // may be skipped; shared exhaustion denies the rest of this pass.
          // Discovery ends before fresh replay proofs.
          for (const snap of records) {
            if (this.savedEvidenceOnly.getStore()!.exhausted || passSignal.aborted ||
                residueQuarantine() !== undefined || this.isExecuting(runId)) break;
            if (!this.finalRetryReady(snap)) continue;
            try {
              await this.withCancellationAttempt(snap, async () =>
                runCaptureCycle(await this.cycleKey(snap), "wait", async () => {
                  if (passSignal.aborted || residueQuarantine() !== undefined || this.isExecuting(runId)) return;
                  const current = await this.readRecord(this.recordPath(snap));
                  if (!current || !this.savedSourceCandidate(current) || !this.finalRetryReady(current)) return;
                  await this.pendingStep(current, passSignal);
                }), attempts);
            } catch {
              this.log.warn("recovery: terminal saved evidence retained");
            }
          }
        });
      } finally { for (const budget of attempts.values()) budget.dispose(); }
      // Replay gets the first opportunity. A separate COMPLETE view below decides
      // whether a new origin is admissible; the advisory saved snapshot never does.
      if (observation.originalContext) await this.admitTerminalOrigin(observation, passSignal);
    });
  }

  /** Origin only: no capture permit, SOURCE, refs, tracking or owed mutations. */
  private async admitTerminalOrigin(observation: ClosedSourceObservation, signal: AbortSignal): Promise<void> {
    const original = observation.originalContext, family = observation.family;
    const ledger = this.maintenance, boundary = this.withTerminalSourceBoundary;
    const observe = this.client.observeTerminalMaintenanceHold?.bind(this.client);
    const current = this.client.isCurrentTerminalMaintenanceHold?.bind(this.client);
    const scan = this.git.matchMaintenanceSourceFamily?.bind(this.git);
    const originDigest = this.git.maintenanceOriginDigest?.bind(this.git);
    const head = this.git.worktreeHead?.bind(this.git);
    const genesis = observation.genesis;
    type Reason = "invalid_context" | "source_protected_or_ambiguous" | "origin_mismatch" |
      "local_binding_mismatch" | "exact_hold_refused" | "boundary_closed" | "metadata_incomplete" |
      "source_unverified" | "history_refused" | "publication_unverified";
    let stage: Reason = "invalid_context", reported = false;
    let knownHold: string | undefined;
    let chargedSignal: AbortSignal | undefined;
    const safeId = (id: unknown) => typeof id === "string" && /^[A-Za-z0-9_-]{1,128}(?![\s\S])/.test(id) ? id : "redacted";
    const report = (reason: Reason | import("./recovery-maintenance.js").MaintenanceDenial) => {
      if (reported) return;
      reported = true;
      const cause: unknown = (chargedSignal ?? M1MaintenanceResources.signalIfActive())?.reason;
      // A helper can return an unverified proof after a quota abort. Preserve only
      // these fixed counter reasons, including after the live resource scope exits.
      if (cause instanceof Error && (cause.message === "maintenance_resource_exhausted" ||
          cause.message === "maintenance_clock_invalid")) reason = cause.message;
      this.log.warn("recovery: terminal origin refused", { reason, run_id: safeId(genesis.runId),
        generation: Number.isSafeInteger(genesis.generation) && genesis.generation > 0 ? genesis.generation : undefined,
        hold_id: knownHold });
    };
    const refused = (reason: Reason | import("./recovery-maintenance.js").MaintenanceDenial): never => {
      report(reason);
      throw new Error("terminal origin refused");
    };
    if (!original || !family || !ledger || !boundary || !observe || !current || !scan || !originDigest || !head ||
        !validInventoryContext(original.context, genesis.runId, original.barePath) ||
        "legacy" in original.context || "origin" in original.context ||
        original.context.generation !== genesis.generation || original.context.branch !== original.branch ||
        original.context.kind !== original.kind || !isCodePublishingKind(original.kind) ||
        !original.key || !original.defaultBranch ||
        original.context.defaultIdentity.ref !== `refs/remotes/origin/${original.defaultBranch}` ||
        !/^[a-f0-9]{40}$/.test(original.baseCommit) ||
        signal.aborted || this.isExecuting(genesis.runId)) { report("invalid_context"); return; }
    const hash = (value: unknown) => createHash("sha256").update(canonicalJson(value)).digest("hex");
    const records = () => this.checkedRecords(genesis.runId, {
      signal, deadline: Date.now() + Math.min(10_000, M1MaintenanceResources.remainingMsIfActive()),
    }, true);
    const freshFamily = async () => {
      const fresh = await scan(original.barePath, original.branch, original.key, genesis.runId,
        family.originals, { signal });
      return fresh?.metadata === family.metadata;
    };
    const localBindingsMatch = (existing: RecoveryRecord[], binding: { holdId: string; repoId: string }) =>
      existing.every(r => (!r.exactHoldBinding || (r.exactHoldBinding.repoId === binding.repoId &&
        (r.generation !== genesis.generation || r.exactHoldBinding.holdId === binding.holdId))) &&
        (r.generation !== genesis.generation || (
          (!r.bareDir || r.bareDir === path.basename(original.barePath)) &&
          r.branch === original.branch && r.kind === original.kind &&
          (!r.coverageContext || canonicalJson(r.coverageContext) === canonicalJson(original.context)))));
    try {
      stage = "metadata_incomplete";
      const discovered = await M1MaintenanceResources.runDiscovery(async () => {
        const existing = await records();
        // Every pre-existing producer is immutable. This unit does not resume a
        // partial producer, mint another ID, or infer continuity from its name.
        if (existing.some(r => classifyRecoveryOrigin(r.captureId) !== "ordinary" ||
            this.savedSourceCandidate(r))) return refused("source_protected_or_ambiguous");
        stage = "source_protected_or_ambiguous";
        if (!await freshFamily()) return refused(stage);
        stage = "origin_mismatch";
        const origin = await originDigest(original.barePath, original.cloneUrl);
        if (!origin) return refused(stage);
        stage = "exact_hold_refused";
        const held = await observe(genesis.runId, genesis.generation, undefined, signal);
        if (!held.ok || held.hold.run_status !== "failed" || !current(held.token)) return refused(stage);
        if (held.hold.repo_id !== original.repoId) return refused("local_binding_mismatch");
        knownHold = safeId(held.hold.hold_id);
        if (!localBindingsMatch(existing, { holdId: held.hold.hold_id, repoId: held.hold.repo_id }))
          return refused("local_binding_mismatch");
        return { origin, held };
      }, { signal });
      if (!discovered) return refused(stage);
      const { origin, held } = discovered;
      const binding = { holdId: held.hold.hold_id, repoId: held.hold.repo_id };
      // Explicit stable projections: no dev/ino, raw config, later generation,
      // timestamp, changed tip, producer ID or human worker label in ledger identity.
      const originalSource = hash([genesis.runId, genesis.generation, genesis.clonePath, genesis.attemptId,
        original.baseCommit]);
      const admission: MaintenanceAdmission = {
        identity: { run: genesis.runId, generation: genesis.generation, hold: binding.holdId,
          repo: binding.repoId, custodian: held.hold.original_worker_id,
          bareBasename: path.basename(original.barePath), origin, originalSource,
          sourceConfig: hash([original.branch, original.key, original.kind, original.defaultBranch,
            original.context.defaultIdentity]) },
        config: RecoveryMaintenance.configuration(this.git.recoveryDecodedLimitBytes),
        source: originalSource, exactCustody: true, terminalNonStopped: true,
      };
      stage = "boundary_closed";
      const entered = await boundary(genesis.runId, family.originals.map(s => s.clonePath), async ctx => {
        const check = () => {
          signal.throwIfAborted();
          M1MaintenanceResources.checkIfActive();
          MaintenanceBudget.current()?.check();
          if (!ctx.open() || residueQuarantine() !== undefined || !current(held.token) ||
              family.originals.some(s => ctx.isLivePath(s.clonePath))) refused("boundary_closed");
        };
        // Renew exact custody under discovery counters, outside discovery at charge.
        stage = "exact_hold_refused";
        const chargedHold = await M1MaintenanceResources.runDiscovery(async () => {
          check();
          if (!await freshFamily()) return refused("source_protected_or_ambiguous");
          const exact = await observe(genesis.runId, genesis.generation, binding, signal);
          check();
          if (!exact.ok || exact.hold.run_status !== "failed" || !current(exact.token)) return refused("exact_hold_refused");
          stage = "metadata_incomplete";
          const beforeCharge = await records();
          if (!localBindingsMatch(beforeCharge, binding)) return refused("local_binding_mismatch");
          if (beforeCharge.some(r => classifyRecoveryOrigin(r.captureId) !== "ordinary" || this.savedSourceCandidate(r)))
            return refused("source_protected_or_ambiguous");
          check();
          return exact;
        }, { signal });
        if (!chargedHold) return refused("exact_hold_refused");
        check();
        if (!current(chargedHold.token)) return refused("exact_hold_refused");
        stage = "history_refused";
        const admitted = await ledger.admit(admission, { signal });
        if (!admitted.ok) return refused(admitted.reason);
        const slot = admitted.value;
        try {
          await M1MaintenanceResources.run(slot, () => MaintenanceBudget.runProducer(async () => {
            chargedSignal = M1MaintenanceResources.signalIfActive();
            stage = "source_unverified";
            check();
            if (!await freshFamily() || !await ctx.quiesce(family.originals.map(s => s.clonePath))) return refused(stage);
            check();
            if (!await freshFamily() || await originDigest(original.barePath, original.cloneUrl) !== origin) return refused(stage);
            const sourceSha = await head(genesis.clonePath);
            if (!sourceSha || !/^[a-f0-9]{40}$/.test(sourceSha)) return refused(stage);
            check();
            const fresh = async () => {
              check();
              if (!await freshFamily() || await originDigest(original.barePath, original.cloneUrl) !== origin ||
                  await head(genesis.clonePath) !== sourceSha) refused("source_unverified");
              const exact = await observe(genesis.runId, genesis.generation, binding, signal);
              check();
              if (!exact.ok || exact.hold.run_status !== "failed" || !current(exact.token) ||
                  exact.hold.original_worker_id !== admission.identity.custodian ||
                  !current(chargedHold.token)) refused("exact_hold_refused");
            };
            await this.withJournalLock(genesis.runId, async () => {
              // A single bounded second view closes replay/publication races.
              stage = "metadata_incomplete";
              const existing = await records();
              if (!localBindingsMatch(existing, binding)) return refused("local_binding_mismatch");
              if (existing.some(r => classifyRecoveryOrigin(r.captureId) !== "ordinary" || this.savedSourceCandidate(r)))
                return refused("source_protected_or_ambiguous");
              stage = "source_unverified";
              await fresh();
              stage = "history_refused";
              const validation = await ledger.revalidate(slot, { ...admission, quiescent: true, sourceOutputContinuity: true });
              if (!validation.ok) return refused(validation.reason);
              const captureId = "maintenance-v1-" + randomUUID();
              if (captureId.length !== 51 || classifyRecoveryOrigin(captureId) !== "producer") return refused("invalid_context");
              const mutation = await ledger.beforeMutation(slot, [hash(captureId)]);
              if (!mutation.ok) return refused(mutation.reason);
              await fresh();
              stage = "publication_unverified";
              await this.writeProducerOriginUnlocked({
                version: JOURNAL_VERSION, runId: genesis.runId, generation: genesis.generation,
                captureId, sourceSha, kind: original.kind, branch: original.branch,
                createdAt: this.now(), state: "pinned", inventoryGuarded: true,
                bareDir: path.basename(original.barePath), defaultBranch: original.defaultBranch,
                exactHoldBinding: binding,
              }, check, fresh);
            }, signal);
          }, { signal, durationMs: Math.min(slot.deadline - Date.now(), M1MaintenanceResources.remainingMsIfActive()) }));
        } finally {
          // Publication ambiguity keeps the precharged fence even on readback failure.
          // All filesystem, quiescence and RPC work above has actually joined.
          const finished = await ledger.finish(slot, "transient");
          if (!finished.ok) report(finished.reason);
        }
      });
      if (!entered.entered) report("boundary_closed");
    } catch {
      report(stage);
    }
  }

  /** RPCs can finish remotely after cancellation; their late answers grant no local authority.
   * An exhausted saved metadata view cannot dispatch another RPC even through a catch/fallback. */
  private async recoveryRPC<T>(action: () => T | Promise<T>): Promise<T> {
    this.checkSavedMetadata();
    const budget = MaintenanceBudget.current();
    const value = await (budget ? budget.raceIO(async () => action()) : action());
    MaintenanceBudget.current()?.check();
    this.checkSavedMetadata();
    return value;
  }

  private readonly savedEvidenceOnly = new AsyncLocalStorage<{
    signal: AbortSignal; entries: number; bytes: number; exhausted: boolean;
  }>();

  /** Volatile, deny-only accounting shared by discovery and every nested journal reopen.
   * It adds no deadline; existing discovery, caller and cancellation clocks still apply. */
  private checkSavedMetadata(resource?: "entries" | "bytes", amount = 0): void {
    M1MaintenanceResources.checkIfActive();
    const pass = this.savedEvidenceOnly.getStore();
    if (!pass) return;
    MaintenanceBudget.current()?.check();
    pass.signal.throwIfAborted();
    if (resource) {
      const limit = resource === "entries" ? 4096 : 32 * 1024 ** 2;
      if (amount > limit - pass[resource]) pass.exhausted = true;
      else pass[resource] += amount;
    }
    if (pass.exhausted) throw new Error("terminal journal metadata exhausted");
  }

  /** Check existing journal scopes only; no new clock or authority is introduced. */
  private checkJournalMetadata(budget?: { signal: AbortSignal; deadline: number }): void {
    this.checkSavedMetadata();
    if (budget) {
      MaintenanceBudget.current()?.check();
      budget.signal.throwIfAborted();
      if (Date.now() >= budget.deadline) throw new Error("report journal deadline");
    }
  }

  /** Stream raw names before any filtering. Each listing shares the saved pass counters;
   * the existing explicit proof budget and discovery limits may only tighten them.
   * One failed read stops this listing; the caller cannot use a partial lock/proof view. */
  private async *journalNames(dir: string, budget?: { signal: AbortSignal; deadline: number },
    completeOrigin = false): AsyncGenerator<string> {
    this.checkJournalMetadata(budget);
    const directory = await fs.opendir(dir);
    let entries = 0;
    try {
      for (;;) {
        this.checkJournalMetadata(budget);
        if (completeOrigin) M1MaintenanceResources.checkIfActive();
        // Reserve the read before dispatch, including an eventual EOF visit. A
        // completed listing cannot refund work to a later nested listing.
        this.checkSavedMetadata("entries", 1);
        M1MaintenanceResources.consumeIfActive("adminEntries", 1);
        const entry = await directory.read();
        this.checkJournalMetadata(budget);
        if (!entry) break;
        M1MaintenanceResources.consumeIfActive("metadataBytes", Buffer.byteLength(entry.name));
        this.checkSavedMetadata("bytes", Buffer.byteLength(entry.name));
        if (!completeOrigin && budget && ++entries > 256) throw new Error("report journal entry budget");
        yield entry.name;
      }
    } finally { await directory.close(); }
    this.checkJournalMetadata(budget);
  }

  private savedSourceCandidate(record: RecoveryRecord): boolean {
    return !!(record.finalRequest || record.finalAcknowledged || record.completionReceipt ||
      record.heldCreateUncertain || hasJournaledBundle(record));
  }

  /** Advisory per-run metadata enumeration. Partial results never prove completeness,
   * emptiness or permission to bootstrap. Discovery ends before expensive replay proofs. */
  private async savedSourceCandidates(runId: string, signal: AbortSignal): Promise<RecoveryRecord[]> {
    const records: RecoveryRecord[] = [];
    try {
      await M1MaintenanceResources.runDiscovery(async () => {
        const dir = await this.checkedRecordDirectory(runId);
        if (!dir) return;
        const budget = { signal, deadline: Date.now() + Math.min(10_000, M1MaintenanceResources.remainingMsIfActive()) };
        for await (const name of this.journalNames(dir, budget, true)) {
          if (!name.endsWith(".json")) continue;
          const result = await this.readRecordResult(path.join(dir, name), true, budget, true);
          if (result.kind === "ok" && result.record.runId === runId && this.savedSourceCandidate(result.record)) {
            records.push(result.record);
          }
        }
      }, { signal });
    } catch { this.log.warn("recovery: terminal saved evidence discovery incomplete"); }
    // Ordinary saved originals lead; authenticated immutable producer evidence remains eligible.
    return records.sort((a, b) => Number(classifyRecoveryOrigin(a.captureId) !== "ordinary") -
      Number(classifyRecoveryOrigin(b.captureId) !== "ordinary"));
  }

  /** Close admission synchronously, abort owned passes, and join their actual settlement.
   * No new pass can extend this finite snapshot; a failed pass does not skip its siblings. */
  async shutdown(): Promise<void> {
    this.passShutdown.abort();
    await Promise.allSettled(this.passes);
  }

  /** Install ownership synchronously before dispatch, including reentrant shutdown from hooks. */
  private trackPass(fn: () => Promise<void>): Promise<void> {
    const pass = Promise.resolve().then(() => {
      if (this.passShutdown.signal.aborted) return;
      return fn();
    });
    this.passes.add(pass);
    return pass.finally(() => {
      this.passes.delete(pass);
    });
  }

  private async resumePendingPass(signal?: AbortSignal, snapshot?: RecoveryRecord[], discover = false): Promise<void> {
    const attempts = new Map<string, MaintenanceBudget>();
    try {
      const records = [...(snapshot ?? await this.snapshotBootRecords())];
      if (discover) {
        try { records.push(...await this.materializeBootInventory(signal, this.isExecuting, attempts)); }
        catch (err) {
          MaintenanceBudget.current()?.check();
          this.log.warn("recovery: boot materialization failed; independent journals still retried", { error: errText(err) });
        }
      }
      if (!snapshot) this.pruneFinalRetries(records);
      const cleanups: RecoveryRecord[] = [];
      let failure: unknown;
      // Visit each snapshot once; a failed generation retains custody while siblings continue.
      for (const snap of records) {
        if (signal?.aborted) break;
        if (!this.finalRetryReady(snap)) continue;
        if (snap.state === "uploaded" && !snap.inventoryGuarded) { if (archiveProvesPin(snap)) cleanups.push(snap); continue; }
        try {
          await this.withCancellationAttempt(snap, async () =>
            runCaptureCycle(await this.cycleKey(snap), "wait", () => this.pendingStep(snap, signal)), attempts);
        } catch (err) { MaintenanceBudget.current()?.check(); failure ??= err; }
      }
      await this.bootPinCleanups(cleanups, signal);
      if (failure) throw failure;
    } finally { for (const budget of attempts.values()) budget.dispose(); }
  }

  /** Boot-time archived-pin cleanups: least recently attempted first, at most `liveMaxPerPass`,
   *  all inside one shared budget; the rest is left for the live pass. */
  private async bootPinCleanups(cleanups: RecoveryRecord[], signal?: AbortSignal): Promise<void> {
    if (cleanups.length === 0 || !this.git.cleanupArchivedRecoveryPin) return;
    const deadline = Date.now() + PIN_CLEANUP_BOOT_MS;
    // Deterministic at boot, when no attempt stamps exist yet: oldest record first.
    const ordered = [...cleanups].sort((a, b) =>
      (this.lastAttemptAt.get(a.captureId) ?? 0) - (this.lastAttemptAt.get(b.captureId) ?? 0) ||
      a.createdAt - b.createdAt || (a.captureId < b.captureId ? -1 : a.captureId > b.captureId ? 1 : 0));
    let handled = 0;
    for (const snap of ordered.slice(0, this.liveMaxPerPass)) {
      if (signal?.aborted || Date.now() >= deadline) break;
      handled++;
      this.lastAttemptAt.set(snap.captureId, this.now());
      try {
        await runCaptureCycle(await this.cycleKey(snap), "wait", async () => {
          const record = await this.readRecord(this.recordPath(snap));
          if (!record || record.state !== "uploaded" || record.inventoryGuarded) return;
          const res = await this.cleanupUploadedRecoveryPin(record, { mode: "boot", signal, deadline });
          this.log.info("recovery restart sweep", {
            run_id: record.runId,
            generation: record.generation,
            outcome: "pin_cleanup",
            reason: res.state === "retained" ? res.reason : res.state,
          });
        });
      } catch (err) { MaintenanceBudget.current()?.check();
        this.log.warn("recovery: uploaded pin cleanup step failed; continuing", { run_id: snap.runId, error: errText(err).slice(0, 512) });
      }
    }
    const deferred = ordered.length - handled;
    if (deferred > 0 && !signal?.aborted) this.log.info("recovery: uploaded pin cleanup deferred", { count: deferred });
  }

  /** One boot-sweep record step; the caller holds the record's capture-cycle lock. */
  private async pendingStep(snap: RecoveryRecord, signal?: AbortSignal): Promise<void> {
    // Re-read the current, authenticated state of exactly this snapshotted record.
    const record = await this.readRecord(this.recordPath(snap));
    if (!record || !this.finalRetryReady(record)) return;

    const done = (outcome: string, reason?: string): void => {
      this.log.info("recovery restart sweep", {
        run_id: record.runId,
        generation: record.generation,
        outcome,
        ...(reason ? { reason } : {}),
      });
    };
    try {
      if (record.inventoryGuarded) { await this.resumeInventory(record, signal, this.isExecuting); return; }
      if (record.state === "uploaded") return;
      if (record.state === "needs_action" && record.reason && PERMANENT_UPLOAD_REASONS.has(record.reason)) {
        // A disposition the api or the local bytes made final: never retried (issue #1995).
        done("needs_action", record.reason);
        return;
      }
      await this.sweepRecord(record, done, signal);
    } catch (err) { MaintenanceBudget.current()?.check();
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
      await ownedRecoveryIO(() => fs.rm(this.bundlePath(record), { force: true })).catch(() => undefined);
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
    } catch (err) { MaintenanceBudget.current()?.check();
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
    record = await this.requireRecord(record);
    if (!this.newProductionAllowed(record)) { done(record.state); return; }
    if (!record.finalizationPin) {
      await this.sweepMark(record, "early_pin_only_after_restart", done);
      return;
    }
    let resolved: Awaited<ReturnType<NonNullable<RecoveryBundleProducer["resolveRestartSource"]>>> | undefined;
    if (record.bareDir && record.defaultBranch && this.git.resolveRestartSource) {
      try {
        resolved = await ownedRecoveryIO(() => this.git.resolveRestartSource!(record.bareDir!, record.sourceSha, record.defaultBranch!));
      } catch (err) { MaintenanceBudget.current()?.check();
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
      await ownedRecoveryIO(() => fs.mkdir(path.dirname(tmpPath), { recursive: true, mode: 0o700 }));
      let result: RecoveryBundleResult;
      try {
        result = await ownedRecoveryIO(() => this.git.produceRecoveryBundle!(resolved.barePath!, {
          sourceSha: record.sourceSha,
          outPath: tmpPath,
          forgeTip: undefined,
        }));
      } catch (err) { MaintenanceBudget.current()?.check();
        await ownedRecoveryIO(() => fs.rm(tmpPath, { force: true })).catch(() => undefined);
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
    } catch (err) { MaintenanceBudget.current()?.check();
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
    if (!this.enabled || this.passShutdown.signal.aborted || opts.signal?.aborted || this.passes.size > 0) return;
    opts = { ...opts, signal: opts.signal ? AbortSignal.any([opts.signal, this.passShutdown.signal]) : this.passShutdown.signal };
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
    const attempts = new Map<string, MaintenanceBudget>();
    let transient = false;
    // A credential-blocked pass whose only work is archived-pin cleanup must not move the upload schedule.
    let pinOnlyBlocked = false;
    try {
      const credentialsBlocked = this.credentialBlockedAt !== undefined && opts.authenticatedAtMs <= this.credentialBlockedAt;
      // Discovery may materialize uploads; blocked passes only retry persisted local ACKs.
      // One discovery per spaced pass. Failed discovery retains earlier entries.
      if (!credentialsBlocked && this.git.discoverOwedCandidates && this.client.hasFeature?.("recovery_inventory_v1")) {
        try { await this.snapshotOwedInventory(); }
        catch (err) { MaintenanceBudget.current()?.check();
          transient = true;
          this.log.warn("recovery: live inventory discovery failed; retained", { error: errText(err) });
        }
        await this.materializeBootInventory(opts.signal, opts.isExecuting, attempts);
      }
      const all = await this.snapshotBootRecords();
      this.pruneFinalRetries(all);
      // Forget the attempt stamps of captures no longer in the journal listing (bounded map).
      const seen = new Set(all.map((r) => r.captureId));
      for (const id of this.lastAttemptAt.keys()) if (!seen.has(id)) this.lastAttemptAt.delete(id);
      for (const id of this.pinCleanupBackoff.keys()) if (!seen.has(id)) this.pinCleanupBackoff.delete(id);
      for (const id of this.pendingReportLogged) if (!seen.has(id)) this.pendingReportLogged.delete(id);
      // Executing runs and ACKs whose reports still need them are filtered BEFORE
      // the cap. Deferred local cleanup must not displace a fresh upload or FINAL.
      const candidates = all
        .filter((r) => isLiveCandidate(r) && this.finalRetryReady(r) && !opts.isExecuting(r.runId) &&
          !(r.inventoryGuarded && r.finalAcknowledged && typeof r.generation === "number" &&
            this.pendingGenerationReport?.(r.runId, r.generation)) &&
          (!credentialsBlocked || (r.inventoryGuarded && (r.finalAcknowledged || r.completionReceipt))))
        .sort((a, b) => (this.lastAttemptAt.get(a.captureId) ?? 0) - (this.lastAttemptAt.get(b.captureId) ?? 0))
        .slice(0, this.liveMaxPerPass);
      // Archived-pin cleanups are local (no credential), so they run while credentials are blocked.
      // They take only the slots the upload candidates leave, and back off per record.
      const cleanupCandidates = !this.git.cleanupArchivedRecoveryPin ? [] : all
        .filter((r) => r.state === "uploaded" && !r.inventoryGuarded && archiveProvesPin(r) && !opts.isExecuting(r.runId) &&
          (this.pinCleanupBackoff.get(r.captureId)?.nextAt ?? 0) <= this.now())
        .sort((a, b) => (this.lastAttemptAt.get(a.captureId) ?? 0) - (this.lastAttemptAt.get(b.captureId) ?? 0))
        .slice(0, Math.max(0, this.liveMaxPerPass - candidates.length));
      // A blocked pass with no local work must not delay a newer authenticated heartbeat.
      if (credentialsBlocked && candidates.length === 0 && cleanupCandidates.length === 0) return;
      pinOnlyBlocked = credentialsBlocked && candidates.length === 0;
      let uploadsStopped = credentialsBlocked;
      for (const snap of candidates) {
        if (uploadsStopped && !(snap.inventoryGuarded && (snap.finalAcknowledged || snap.completionReceipt))) continue;
        if (opts.signal?.aborted) break;
        let res;
        try {
          res = await this.withCancellationAttempt(snap, async () =>
            runCaptureCycle(await this.cycleKey(snap), "skip", () => this.liveStep(snap, opts)), attempts);
        } catch (err) {
          MaintenanceBudget.current()?.check();
          transient = true;
          this.log.warn("recovery: live generation retained", { run_id: snap.runId, error: errText(err) });
          continue;
        }
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
      for (const snap of cleanupCandidates) {
        if (opts.signal?.aborted) break;
        const res = await runCaptureCycle(await this.cycleKey(snap), "skip", () => this.livePinCleanupStep(snap, opts));
        if (!res.ran) this.lastAttemptAt.set(snap.captureId, this.now());
      }
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.warn("recovery: live re-drive pass failed", { error: errText(err) });
      transient = true;
    } finally { for (const budget of attempts.values()) budget.dispose(); }
    if (pinOnlyBlocked) return;
    if (transient) {
      this.transientPasses++;
      this.nextPassAt = this.now() + Math.min(this.liveBackoffBaseMs * 2 ** this.transientPasses, this.liveBackoffCapMs);
    } else {
      this.transientPasses = 0;
      this.nextPassAt = this.now() + this.liveBackoffBaseMs;
    }
  }

  /** One live archived-pin cleanup; the caller holds the record's capture-cycle lock. */
  private async livePinCleanupStep(snap: RecoveryRecord, opts: ResumeLiveOptions): Promise<void> {
    this.lastAttemptAt.set(snap.captureId, this.now());
    const record = await this.readRecord(this.recordPath(snap));
    if (!record || record.state !== "uploaded" || record.inventoryGuarded || opts.isExecuting(record.runId)) return;
    const res = await this.cleanupUploadedRecoveryPin(record, { mode: "live", signal: opts.signal });
    // A settled record is re-checked in-process only after the long settle interval (no durable marker);
    // a restart still re-checks it once at boot.
    if (res.state === "deleted" || res.state === "already_absent") {
      this.pinCleanupBackoff.set(record.captureId, { failures: 0, nextAt: this.now() + this.livePinSettleMs });
      return;
    }
    const failures = (this.pinCleanupBackoff.get(record.captureId)?.failures ?? 0) + 1;
    this.pinCleanupBackoff.set(record.captureId, {
      failures,
      nextAt: this.now() + Math.min(this.liveBackoffBaseMs * 2 ** failures, this.liveBackoffCapMs),
    });
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
    if (!isLiveCandidate(record) || !this.finalRetryReady(record) || opts.isExecuting(record.runId) ||
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
      if (out.reason && LIVE_RETRY_REASONS.has(out.reason) &&
          !["final_retry_refused", "final_retry_unknown", "inventory_hold_settled"].includes(out.reason)) return "transient";
      return "ok";
    } catch (err) { MaintenanceBudget.current()?.check();
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

  /** Receipt cleanup visits only this authenticated generation. No archive ancestry or capture RPC
   * gates it: the issue #2625 redundancy claims are built under the journal lock from the records
   * read there, but submitted only after the lock is released, bounded by one deadline, and their
   * answers are logged and otherwise ignored, so no refusal, error or timeout changes the cleanup.
   * One attempt per existing sweep; any failed proof stops this generation and retains authority. */
  private async forgetCompletedGeneration(runId: string, generation: number, isExecuting: (runId: string) => boolean,
    signal?: AbortSignal): Promise<void> {
    if (!this.withCompletionSourceBoundary || isExecuting(runId) || this.isExecuting(runId)) return;
    let claims: PendingRedundancyClaim[] = [];
    const budget = this.startRedundancyBudget(signal);
    await this.withJournalLock(runId, async () => {
      const records = await this.checkedRecords(runId);
      const selected = records.filter(r => r.generation === generation);
      const authority = selected.find(r => r.completionReceipt && r.completionSource);
      if (!authority || !this.git.cleanupRecoveryGeneration || residueQuarantine() !== undefined) return;
      const source = authority.completionSource!;
      const ctx = source.context;
      if (selected.some(r => !r.inventoryGuarded || r.branch !== ctx.branch || r.kind !== ctx.kind ||
          (r.completionReceipt && canonicalJson(r.completionReceipt) !== canonicalJson(authority.completionReceipt)))) return;
      const bare = await ownedRecoveryIO(() => this.git.resolveRecoveryBareDir?.(path.basename(ctx.barePath)));
      if (bare !== ctx.barePath) return;
      claims = await this.buildRedundancyClaims(selected, generation, bare, budget, authority.completionReceipt!.final_head);
      const bundles = new Set<string>();
      for (const record of selected) {
        if ((record.bareDir && record.bareDir !== path.basename(bare)) ||
            (record.recoveryPinBareDir && record.recoveryPinBareDir !== path.basename(bare)) ||
            (record.coverageContext && record.coverageContext!.barePath !== bare) ||
            (record.completionSource && canonicalJson(record.completionSource) !== canonicalJson(source))) return;
        const canonical = path.resolve(this.bundlePath(record));
        if (record.bundlePath && path.resolve(record.bundlePath) !== canonical) return;
        if (path.dirname(canonical) !== path.resolve(this.runDir(runId))) return;
        try {
          const stat = await fs.lstat(canonical);
          if (!stat.isFile() || stat.isSymbolicLink()) return;
        } catch (err) { MaintenanceBudget.current()?.check();
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
        }
        bundles.add(canonical);
      }
      const pins = selected.filter(r => r.coverageDigest).map(r => ({ fingerprint: r.coverageDigest!, sha: r.sourceSha }));
      const recoverySources = [...new Set(selected.flatMap(r =>
        [r.sourceSha, r.originalSourceSha, r.inventoryCurrentSha].filter((sha): sha is string => !!sha)))];
      // Refuse malformed/unknown attribution before moving the physical completing source.
      const preflight = await ownedRecoveryIO(() => this.git.cleanupRecoveryGeneration!(bare, ctx, source.roots, pins,
        recoverySources, authority.completionReceipt!.final_head,
        () => residueQuarantine() === undefined && !isExecuting(runId) && !this.isExecuting(runId), "completion-check"));
      if (preflight !== "verified") return;
      await this.withCompletionSourceBoundary!(source, async canDelete => {
        const result = await ownedRecoveryIO(() => this.git.cleanupRecoveryGeneration!(bare, ctx, source.roots, pins,
          recoverySources, authority.completionReceipt!.final_head, canDelete, "completion"));
        if (result !== "removed") return;
        const unlink = async (file: string): Promise<void> => {
          if (!canDelete()) throw new Error("completion cleanup protection changed");
          try { await ownedRecoveryIO(() => fs.unlink(file)); }
          catch (err) { MaintenanceBudget.current()?.check(); if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
        };
        for (const bundle of bundles) await unlink(bundle);
        for (const record of selected) if (record.captureId !== authority.captureId) await unlink(this.recordPath(record));
        await unlink(this.recordPath(authority));
        await ownedRecoveryIO(() => fs.rmdir(this.runDir(runId))).catch(() => undefined);
      });
    });
    await this.submitRedundancyClaims(claims, budget);
  }

  /** Caller owns the generation cycle. The journal lock spans selection and every destructive
   * step, excluding delayed journal writers. One pass, no internal retries; failure keeps the
   * covering ACK and the bounded boot/live passes retry it independently of upload eligibility. */
  private async forgetGenerationWithinCycle(runId: string, generation: number, isExecuting = this.isExecuting,
    signal?: AbortSignal): Promise<void> {
    // Issue #2625: claims are built under the journal lock and submitted after it, as in forgetCompletedGeneration.
    let claims: PendingRedundancyClaim[] = [];
    const budget = this.startRedundancyBudget(signal);
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
      const bare = await ownedRecoveryIO(() => this.git.resolveRecoveryBareDir?.(ack.bareDir!));
      if (bare !== ack.coverageContext.barePath) return;
      const bundles = new Set<string>();
      // Positive ownership is the canonical capture filename, not an arbitrary MAC-covered path.
      // Validate every path before changing refs; an unknown journal path retains all authority.
      for (const record of selected) {
        if ((record.bareDir && record.bareDir !== ack.bareDir) ||
            (record.recoveryPinBareDir && record.recoveryPinBareDir !== ack.bareDir) ||
            (record.coverageContext && record.coverageContext!.barePath !== bare)) return;
        const canonical = path.resolve(this.bundlePath(record));
        if (record.bundlePath && path.resolve(record.bundlePath) !== canonical) return;
        if (path.dirname(canonical) !== path.resolve(this.runDir(runId))) return;
        try {
          const st = await fs.lstat(canonical);
          if (!st.isFile() || st.isSymbolicLink()) return;
        } catch (err) { MaintenanceBudget.current()?.check();
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
        }
        bundles.add(canonical);
      }
      if (!open()) return;
      // The uploaded byte identity is already authenticated. Retirement needs the ACK
      // journal and pinned Git objects for ancestry, but never reads the local bundle.
      // Release those bytes on FINAL's existing schedule even if the report cannot retire.
      if (this.pendingGenerationReport?.(runId, generation)) {
        if (!this.pendingReportLogged.has(ack.captureId)) {
          this.pendingReportLogged.add(ack.captureId);
          this.log.debug("recovery cleanup deferred: pending report", { run_id: runId, generation });
        }
        for (const bundle of bundles) {
          if (!open()) return;
          try { await ownedRecoveryIO(() => fs.unlink(bundle)); }
          catch (err) { MaintenanceBudget.current()?.check(); if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
        }
        return;
      }
      const cleanupOpen = (): boolean => open() && !this.pendingGenerationReport?.(runId, generation);
      this.pendingReportLogged.delete(ack.captureId);
      claims = await this.buildRedundancyClaims(selected, generation, bare, budget);
      const result = await ownedRecoveryIO(() => this.git.cleanupRecoveryGeneration!(bare, ack.coverageContext!,
        ack.originalRoots ?? [], selected.filter(r => r.coverageDigest).map(r => ({
          fingerprint: r.coverageDigest!, sha: r.sourceSha,
          coveredHeads: [...new Set([r.originalSourceSha!, r.inventoryCurrentSha!, ...(r.originalRoots ?? []).map(root => root.sha)])],
        })), [...new Set(selected.flatMap(r => [r.sourceSha, r.originalSourceSha].filter((s): s is string => !!s)))],
        ack.sourceSha, cleanupOpen));
      if (result !== "removed") return;
      const unlink = async (file: string): Promise<void> => {
        if (!cleanupOpen()) throw new Error("recovery cleanup source protection changed");
        try { await ownedRecoveryIO(() => fs.unlink(file)); }
        catch (err) { MaintenanceBudget.current()?.check(); if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
      };
      for (const bundle of bundles) await unlink(bundle);
      // All other journals (including other ACKs) go first. The ACK chosen by cleanupStateOf
      // remains a covering authority at every intermediate state and is removed last.
      for (const record of selected.filter(r => r.captureId !== ack.captureId)
        .sort((a, b) => Number(!!a.finalAcknowledged) - Number(!!b.finalAcknowledged))) await unlink(this.recordPath(record));
      await unlink(this.recordPath(ack));
      await ownedRecoveryIO(() => fs.rmdir(this.runDir(runId))).catch(() => undefined);
    });
    await this.submitRedundancyClaims(claims, budget);
  }

  /**
   * Issue #2625: one claim per uploaded, non-settled guarded record of `generation`, read from the
   * local bare repo. The caller holds the journal lock and passes the records it read there. A
   * failure for one record drops only that claim; nothing here can change a cleanup decision.
   * `witnessHead` is the completed run's delivered head (receipt path only).
   */
  private async buildRedundancyClaims(selected: RecoveryRecord[], generation: number, bare: string,
    budget: RedundancyBudget, witnessHead?: string): Promise<PendingRedundancyClaim[]> {
    const out: PendingRedundancyClaim[] = [];
    try {
      if (!this.client.hasFeature?.("recovery_archive_redundancy_v1") || !this.client.proveRecoveryArchiveRedundancy ||
          !this.git.readRecoveryCoverageClaim) return out;
      for (const r of selected) {
        if (!r.inventoryGuarded || r.generation !== generation || r.state !== "uploaded" || !r.serverCaptureId ||
            !r.coverageDigest || r.coverageDigest === EMPTY_INVENTORY_DIGEST || !r.inventoryCurrentSha ||
            !r.originalSourceSha || !r.coverageContext || r.finalRequest?.disposition.kind === "settled" ||
            this.redundancySettled.has(r.serverCaptureId)) continue;
        try {
          const roots = [...new Set([...(r.originalRoots ?? []).map(x => x.sha), r.originalSourceSha])].sort();
          if (budget.signal.aborted) break;
          // The reads run under the journal lock: bound them (git children and the repo-lock wait)
          // by the same deadline as submission, and race them so even a hung reader gives up.
          const opts = {
            sourceSha: r.sourceSha, fingerprint: r.coverageDigest, roots, currentSha: r.inventoryCurrentSha,
            ...(witnessHead === undefined ? {} : { witnessHead }),
          };
          const read = () => this.git.readRecoveryCoverageClaim!(bare, opts);
          const claim = await withinBudget(budget.signal, () =>
            this.git.withRecoveryOperation ? this.git.withRecoveryOperation!(budget.signal, budget.deadline, read) : read());
          if (!claim) continue;
          out.push({ runId: r.runId, generation, captureId: r.serverCaptureId, request: {
            generation, coverage_digest: claim.coverageDigest, roots: claim.roots, current_sha: claim.currentSha,
            tree: claim.tree, aggregate_objects: claim.aggregateObjects, current_object: claim.currentObject,
            ...(claim.witnessObject === undefined ? {} : { wip: { witness_object: claim.witnessObject } }),
          } });
        } catch (err) { MaintenanceBudget.current()?.check();
          this.log.debug("recovery: archive redundancy claim not built", { run_id: r.runId, generation, error: errText(err) });
        }
      }
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.debug("recovery: archive redundancy claims not built", { generation, error: errText(err) });
    }
    return out;
  }

  /** Starts the one cleanup deadline (30 s, plus any caller abort). The timer is unref'd and
   *  cleared by {@link submitRedundancyClaims}, which every path reaching the lock body calls. */
  private startRedundancyBudget(signal?: AbortSignal): RedundancyBudget {
    const ms = REDUNDANCY_DEADLINE_MS;
    const controller = new AbortController();
    let dispose: () => void;
    if (this.testHooks?.redundancyTimer) {
      dispose = this.testHooks.redundancyTimer(() => controller.abort(), ms);
    } else {
      const timer = setTimeout(() => controller.abort(), ms);
      timer.unref?.();
      dispose = () => clearTimeout(timer);
    }
    return {
      signal: signal ? AbortSignal.any([signal, controller.signal]) : controller.signal,
      deadline: Date.now() + ms,
      dispose,
    };
  }

  /** Sequential, best effort, never throws, and returns by the one deadline even when a request
   *  hangs. Answers are logged; only final ones are remembered. */
  private async submitRedundancyClaims(claims: PendingRedundancyClaim[], budget: RedundancyBudget): Promise<void> {
    try {
      if (!claims.length || !this.client.proveRecoveryArchiveRedundancy) return;
      for (const claim of claims) {
        if (budget.signal.aborted) break;
        try {
          const res = await withinBudget(budget.signal, () =>
            this.recoveryRPC(() => this.client.proveRecoveryArchiveRedundancy!(claim.runId, claim.captureId, claim.request, maintenanceSignal(budget.signal))));
          if (!res) break;
          this.log.info("recovery: archive redundancy claim answered", {
            run_id: claim.runId, generation: claim.generation, capture_id: claim.captureId,
            outcome: res.outcome, ...(res.reason === undefined ? {} : { reason: res.reason }),
          });
          if (res.outcome === "expired" || (res.outcome === "retained" && !TRANSIENT_REDUNDANCY_REASONS.has(res.reason ?? ""))) {
            this.redundancySettled.add(claim.captureId);
          }
        } catch (err) { MaintenanceBudget.current()?.check();
          this.log.info("recovery: archive redundancy claim failed", {
            run_id: claim.runId, generation: claim.generation, capture_id: claim.captureId, error: errText(err),
          });
        }
      }
    } finally {
      budget.dispose();
    }
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
    } catch { MaintenanceBudget.current()?.check(); return false; }
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
    } catch (err) { MaintenanceBudget.current()?.check();
      this.log.warn("recovery: completion receipt not persisted; source and terminal evidence retained", {
        run_id: receipt.run_id, error: errText(err),
      });
      return false;
    }
  }

  async hasPersistedCompletionReceipt(runId: string, generation: number): Promise<boolean> {
    try {
      return (await this.checkedRecords(runId)).some(r => r.generation === generation && !!r.completionReceipt);
    } catch { MaintenanceBudget.current()?.check(); return false; }
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
        const bare = ack.bareDir && await ownedRecoveryIO(() => this.git.resolveRecoveryBareDir?.(ack.bareDir!));
        if (!bare || bare !== ack.coverageContext?.barePath || !this.git.ancestry ||
            await ownedRecoveryIO(() => this.git.ancestry!(bare, head, ack.sourceSha)) !== "ancestor") return "pending";
      }
    }
    return "acknowledged";
  }

  /** Deny-only until a future charged producer entry supplies its own authority.
   * Callers pass only records authenticated by readLatest/requireRecord/listRecords. */
  private newProductionAllowed(record: RecoveryRecord): boolean {
    if (this.savedEvidenceOnly.getStore()) return false;
    const origin = classifyRecoveryOrigin(record.captureId);
    if (origin === "ordinary") return true;
    const display = (id: string | undefined): string =>
      id && id.length <= 80 && /^[a-zA-Z0-9_-]+(?![\s\S])/.test(id) ? id : "[redacted]";
    this.log.warn("recovery: new production denied; source retained", {
      origin, reason: origin === "producer" ? "maintenance_admission_required" : "maintenance_origin_invalid",
      run_id: display(record.runId),
      generation: Number.isSafeInteger(record.generation) && record.generation! > 0 ? record.generation : undefined,
      hold_id: display(record.exactHoldBinding?.holdId),
    });
    return false;
  }

  /** One metadata-only pass, no retries. A fresh discovery scope permits 32 MiB,
   * 4096 entries and ten seconds; existing scopes share counters and the attempt clock.
   * Failure blocks new reconstruction only. Immutable replay and frozenCoverageProven
   * retain their existing proof, including every authenticated source record. */
  private async newReconstructionRecords(runId: string, generation?: number): Promise<RecoveryRecord[] | undefined> {
    if (this.savedEvidenceOnly.getStore()) return undefined;
    const read = async (): Promise<RecoveryRecord[] | undefined> => {
      const records = await this.checkedRecords(runId, {
        signal: MaintenanceBudget.signal() ?? new AbortController().signal,
        deadline: Date.now() + Math.min(10_000, MaintenanceBudget.current()?.remainingMs() ?? Infinity,
          M1MaintenanceResources.remainingMsIfActive()),
      }, true);
      for (const record of records) {
        // Unknown generations cannot exclude a producer from the target source proof.
        if ((generation === undefined || record.generation === undefined || record.generation === generation) &&
            !this.newProductionAllowed(record)) return undefined;
      }
      return records;
    };
    try {
      return await (M1MaintenanceResources.active() ? read() : M1MaintenanceResources.runDiscovery(read));
    } catch {
      MaintenanceBudget.current()?.check();
      this.log.warn("recovery: reconstruction provenance incomplete; source retained");
      return undefined;
    }
  }

  /** Shared directory containment checks; no filename or record grants authority. */
  private async checkedRecordDirectory(runId: string, budget?: { signal: AbortSignal; deadline: number }): Promise<string | undefined> {
    this.checkJournalMetadata(budget);
    if (!/^[a-zA-Z0-9_-]{1,128}(?![\s\S])/.test(runId)) throw new Error("unsafe recovery run id");
    const root = path.resolve(this.recoveryRoot);
    const dir = this.runDir(runId);
    for (const directory of [root, dir]) {
      let stat;
      this.checkJournalMetadata(budget);
      try { stat = await fs.lstat(directory); }
      catch (err) { MaintenanceBudget.current()?.check();
        this.checkJournalMetadata(budget);
        if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
        throw err;
      }
      this.checkJournalMetadata(budget);
      if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("unsafe recovery directory");
    }
    // The lstat checks above reject a symlinked recovery root or run directory. A symlinked
    // ANCESTOR of the root (a symlinked data dir) is a legitimate layout every other journal
    // read already tolerates, so containment compares against the canonical root, not the lexical one.
    this.checkJournalMetadata(budget);
    const realRoot = await fs.realpath(root);
    this.checkJournalMetadata(budget);
    const realDir = await fs.realpath(dir);
    this.checkJournalMetadata(budget);
    if (realDir !== path.join(realRoot, path.basename(dir))) {
      throw new Error("symlink recovery directory");
    }
    this.checkJournalMetadata(budget);
    return dir;
  }

  /** One bounded pass over physical JSON files; any failed read blocks authority.
   * Paths must resolve inside this coordinator's root, with no symlink file reads. */
  private async checkedRecords(runId: string, budget?: { signal: AbortSignal; deadline: number }, completeOrigin = false): Promise<RecoveryRecord[]> {
    this.checkJournalMetadata(budget);
    const dir = await this.checkedRecordDirectory(runId, budget);
    this.checkJournalMetadata(budget);
    if (!dir) return [];
    let names: string[];
    // Disappearance after a positive directory check is unknown, not empty proof.
    const before = await fs.lstat(dir);
    this.checkJournalMetadata(budget);
    if (budget || this.savedEvidenceOnly.getStore() || M1MaintenanceResources.active()) {
      names = [];
      for await (const name of this.journalNames(dir, budget, completeOrigin)) names.push(name);
    } else names = await fs.readdir(dir);
    this.checkJournalMetadata(budget);
    const records: RecoveryRecord[] = [];
    for (const name of names) {
      if (!name.endsWith(".json")) continue;
      this.checkJournalMetadata(budget);
      const result = await this.readRecordResult(path.join(dir, name), true, budget, completeOrigin);
      this.checkJournalMetadata(budget);
      if (result.kind !== "ok" || result.record.runId !== runId ||
          name !== result.record.captureId + ".json") throw new Error("unverified recovery journal");
      records.push(result.record);
    }
    if (completeOrigin) {
      const bundles = new Set(records.map(record => record.captureId + ".bundle"));
      for (const name of names) {
        budget!.signal.throwIfAborted();
        if (Date.now() >= budget!.deadline) throw new Error("origin journal deadline");
        if (name.endsWith(".json")) continue;
        // Unknown entries, including truncated namespace filenames and incomplete writes,
        // cannot prove an empty or ordinary-only inventory. Names grant no origin authority.
        this.checkJournalMetadata(budget);
        if (!bundles.has(name)) throw new Error("unknown recovery journal entry");
        const stat = await fs.lstat(path.join(dir, name));
        this.checkJournalMetadata(budget);
        if (!stat.isFile()) {
          throw new Error("unknown recovery journal entry");
        }
      }
    }
    this.checkJournalMetadata(budget);
    const after = await fs.lstat(dir);
    this.checkJournalMetadata(budget);
    if (completeOrigin && budget) {
      budget.signal.throwIfAborted();
      if (Date.now() >= budget.deadline) throw new Error("origin journal deadline");
    }
    this.checkJournalMetadata(budget);
    if ((budget || this.savedEvidenceOnly.getStore() || M1MaintenanceResources.active()) && (before.dev !== after.dev || before.ino !== after.ino || before.mtimeMs !== after.mtimeMs ||
        before.ctimeMs !== after.ctimeMs)) throw new Error("report journal directory changed");
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

  private withJournalLock<T>(runId: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    MaintenanceBudget.current()?.check();
    return recoveryJournalLocks.withLock(path.resolve(this.runDir(runId)),
      () => ownedRecoveryIO(fn), maintenanceSignal(signal));
  }

  /** Atomic rename with a fresh MAC (0600 file, 0700 dir). The caller holds this run's journal lock. */
  private async writeRecordUnlocked(record: RecoveryRecord): Promise<void> {
    if (!this.key) throw new Error("recovery: MAC key unavailable");
    const mac = this.computeMac(record);
    const dir = this.runDir(record.runId);
    await ownedRecoveryIO(() => fs.mkdir(dir, { recursive: true, mode: 0o700 }));
    const dst = this.recordPath(record);
    const tmp = `${dst}.${randomUUID()}.tmp`;
    await ownedRecoveryIO(() => fs.writeFile(tmp, JSON.stringify({ ...record, mac }), { mode: 0o600 }));
    await ownedRecoveryIO(() => fs.rename(tmp, dst));
  }

  /** Standard journal representation with producer-only durability. Failure preserves evidence.
   * The caller owns the journal lock and has durably indexed this immutable ID. */
  private async writeProducerOriginUnlocked(record: RecoveryRecord, check: () => void,
    fresh: () => Promise<void>): Promise<void> {
    const sync = async (dir: string) => {
      check();
      const handle = await fs.open(dir, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
      try { check(); await handle.sync(); } finally { await handle.close(); }
    };
    const raw = JSON.stringify({ ...record, mac: this.computeMac(record) });
    // Reserve the complete authenticated representation before opening a temp file.
    M1MaintenanceResources.consumeIfActive("metadataBytes", Buffer.byteLength(raw));
    const dir = path.resolve(this.runDir(record.runId));
    let cursor = path.parse(dir).root;
    // One bounded ancestor walk, no recursive mkdir following symlinks. Each new
    // directory and its parent are synced before any journal file is dispatched.
    const components = dir.slice(cursor.length).split(path.sep).filter(Boolean);
    if (components.length > 128) throw new Error("terminal origin path depth");
    for (const part of components) {
      const parent = cursor;
      cursor = path.join(cursor, part);
      check();
      M1MaintenanceResources.consumeIfActive("adminEntries", 1);
      try {
        const stat = await fs.lstat(cursor);
        if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("unsafe terminal origin parent");
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT" ||
            (cursor !== path.resolve(this.recoveryRoot) && cursor !== dir)) throw error;
        await fresh(); check();
        await fs.mkdir(cursor, { mode: 0o700 });
        await sync(cursor); await sync(parent);
      }
    }
    await fresh(); check();
    const dst = this.recordPath(record), tmp = dst + "." + randomUUID() + ".tmp";
    const file = await fs.open(tmp, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
    try {
      check();
      await file.writeFile(raw);
      check(); await file.sync();
    } finally { await file.close(); }
    await fresh(); check();
    await fs.rename(tmp, dst);
    await sync(dir);
    check();
    const result = await this.readRecordResult(dst, true, { signal: MaintenanceBudget.signal()!,
      deadline: Date.now() + M1MaintenanceResources.remainingMsIfActive() }, true);
    if (result.kind !== "ok" || canonicalJson(result.record) !== canonicalJson(record) ||
        this.recordPath(result.record) !== dst) throw new Error("terminal origin readback failed");
    check();
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
    budget?: { signal: AbortSignal; deadline: number },
    quiet = false,
  ): Promise<{ kind: "ok"; record: RecoveryRecord } | { kind: "absent" } | { kind: "unreadable" }> {
    if (!this.key) return { kind: "unreadable" };
    const savedPass = this.savedEvidenceOnly.getStore();
    this.checkSavedMetadata();
    if (savedPass || M1MaintenanceResources.active()) {
      // Every nested journal reopen uses the same byte cap and no-follow proofs.
      // Infinity selects the bounded reader without creating or renewing a deadline.
      const signals = [savedPass?.signal, budget?.signal, MaintenanceBudget.signal(),
        M1MaintenanceResources.signalIfActive()].filter((s): s is AbortSignal => s !== undefined);
      budget = { signal: AbortSignal.any(signals), deadline: budget?.deadline ?? Infinity };
      noFollow = true;
      quiet = true;
    }
    let raw: string;
    try {
      this.checkJournalMetadata(budget);
      if (noFollow) {
        const file = await fs.open(filePath, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
        try {
          this.checkJournalMetadata(budget);
          const before = await file.stat({ bigint: true });
          this.checkJournalMetadata(budget);
          if (!before.isFile()) return { kind: "unreadable" };
          if (budget) {
            if (before.size > BigInt(1024 * 1024)) return { kind: "unreadable" };
            if (savedPass && before.size > BigInt(32 * 1024 ** 2 - savedPass.bytes)) {
              savedPass.exhausted = true;
              this.checkSavedMetadata();
            }
            if (quiet) M1MaintenanceResources.checkIfActive();
            const precharged = M1MaintenanceResources.active();
            // Reserve the entire stat bound before allocating or reading; failed/short reads keep it charged.
            if (precharged) M1MaintenanceResources.consumeIfActive("metadataBytes", Number(before.size));
            const bytes = Buffer.alloc(precharged ? Number(before.size) : Math.min(1024 * 1024 + 1,
              savedPass ? 32 * 1024 ** 2 - savedPass.bytes + 1 : Infinity));
            let length = 0;
            while (length < bytes.length) {
              this.checkJournalMetadata(budget);
              const { bytesRead } = await file.read(bytes, length, bytes.length - length, null);
              this.checkSavedMetadata("bytes", bytesRead);
              this.checkJournalMetadata(budget);
              if (!bytesRead) break;
              length += bytesRead;
            }
            if (quiet) M1MaintenanceResources.checkIfActive();
            if (length > 1024 * 1024 || (precharged && BigInt(length) !== before.size)) return { kind: "unreadable" };
            this.checkJournalMetadata(budget);
            const after = await file.stat({ bigint: true });
            this.checkJournalMetadata(budget);
            const named = await fs.lstat(filePath, { bigint: true });
            this.checkJournalMetadata(budget);
            if (before.dev !== named.dev || before.ino !== named.ino || named.isSymbolicLink() ||
                before.size !== after.size || before.size !== named.size ||
                before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs ||
                before.mtimeNs !== named.mtimeNs || before.ctimeNs !== named.ctimeNs) return { kind: "unreadable" };
            this.checkJournalMetadata(budget);
            raw = bytes.subarray(0, length).toString("utf8");
          } else raw = await file.readFile("utf8");
        } finally { await file.close(); }
      } else {
        raw = await fs.readFile(filePath, "utf8");
      }
      this.checkJournalMetadata(budget);
    } catch (err) { MaintenanceBudget.current()?.check();
      this.checkSavedMetadata();
      return (err as NodeJS.ErrnoException)?.code === "ENOENT" ? { kind: "absent" } : { kind: "unreadable" };
    }
    if (quiet) M1MaintenanceResources.checkIfActive();
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch { MaintenanceBudget.current()?.check();
      if (!quiet) this.log.warn("recovery: unparseable journal record; refusing (needs_action)", { file: filePath });
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
      if (!quiet) this.log.warn("recovery: malformed journal record; refusing (needs_action)", { file: filePath });
      return { kind: "unreadable" };
    }
    const expected = this.computeMac(record);
    if (!macEqual(mac, expected)) {
      if (!quiet) this.log.warn("recovery: journal MAC mismatch; refusing tampered record (needs_action)", {
        file: filePath,
        run_id: record.runId,
      });
      return { kind: "unreadable" };
    }
    // Identity consistency is checked only after the normal MAC authentication.
    if (path.basename(filePath) !== record.captureId + ".json" ||
        path.basename(path.dirname(filePath)) !== record.runId) return { kind: "unreadable" };
    this.checkSavedMetadata();
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
    if (this.savedEvidenceOnly.getStore() || M1MaintenanceResources.active()) {
      this.checkSavedMetadata();
      const dir = await this.checkedRecordDirectory(runId);
      this.checkSavedMetadata();
      if (!dir) return [];
      const before = await fs.lstat(dir);
      this.checkSavedMetadata();
      const out: RecoveryRecord[] = [];
      for await (const name of this.journalNames(dir)) {
        if (!name.endsWith(".json")) continue;
        const record = await this.readRecord(path.join(dir, name));
        if (record) out.push(record);
      }
      this.checkSavedMetadata();
      const after = await fs.lstat(dir);
      this.checkSavedMetadata();
      if (before.dev !== after.dev || before.ino !== after.ino || before.mtimeMs !== after.mtimeMs ||
          before.ctimeMs !== after.ctimeMs) throw new Error("terminal journal directory changed");
      this.checkSavedMetadata();
      return out;
    }
    let names: string[];
    try {
      names = await fs.readdir(this.runDir(runId));
    } catch { MaintenanceBudget.current()?.check();
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
    } catch (err) { MaintenanceBudget.current()?.check();
      if (err instanceof RecordGoneError || !swallow) throw err;
      return { state: "needs_action", captureId: record.captureId, reason };
    }
  }

  /** The caller holds this run's journal lock, including for legacy whole-run release. */
  private async removeRunDir(runId: string): Promise<void> {
    let protectedNow = true;
    try {
      protectedNow = await this.terminalRecordProtection?.(runId) ?? false;
    } catch { MaintenanceBudget.current()?.check();
      // An unavailable protection inventory retains unknown files.
    }
    if (protectedNow) {
      await ownedRecoveryIO(() => fs.rmdir(this.runDir(runId))).catch(() => undefined);
      return;
    }
    await ownedRecoveryIO(() => fs.rm(this.runDir(runId), { recursive: true, force: true })).catch(() => undefined);
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
      await ownedRecoveryIO(() => fs.rm(this.recordPath(record), { force: true })).catch(() => undefined);
      // The bundle lives at the canonical <captureId>.bundle path; remove any distinct
      // journaled bundlePath too, so a released generation never leaks its bytes.
      await ownedRecoveryIO(() => fs.rm(this.bundlePath(record), { force: true })).catch(() => undefined);
      if (record.bundlePath && record.bundlePath !== this.bundlePath(record)) {
        await ownedRecoveryIO(() => fs.rm(record.bundlePath!, { force: true })).catch(() => undefined);
      }
    }
    if (dirMode === "sweep_if_last") {
      if (remaining === 0) await this.removeRunDir(runId);
      return;
    }
    // ENOTEMPTY / ENOENT (and any other failure) leave the dir: never a recursive removal here.
    await ownedRecoveryIO(() => fs.rmdir(this.runDir(runId))).catch(() => undefined);
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
  } catch { MaintenanceBudget.current()?.check();
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
  if (obj.heldCreateUncertain !== undefined) {
    if (typeof obj.heldCreateUncertain !== "boolean") return null;
    record.heldCreateUncertain = obj.heldCreateUncertain;
  }
  if (obj.heldPolicy !== undefined) {
    if (!record.inventoryGuarded || !validFrozenHeldPolicy(obj.heldPolicy)) return null;
    record.heldPolicy = obj.heldPolicy;
  }
  if (obj.exactHoldBinding !== undefined) {
    const binding = obj.exactHoldBinding as RecoveryRecord["exactHoldBinding"];
    if (!record.inventoryGuarded || !binding || !HELD_PUBLICATION_ID_RE.test(binding.holdId) ||
        !HELD_PUBLICATION_ID_RE.test(binding.repoId)) return null;
    record.exactHoldBinding = binding;
  }
  if (obj.holdSettlement !== undefined) {
    const settled = obj.holdSettlement as RecoveryExactHold;
    if (!record.exactHoldBinding || !settled ||
        settled.run_id !== record.runId || settled.generation !== record.generation ||
        settled.hold_id !== record.exactHoldBinding.holdId || settled.repo_id !== record.exactHoldBinding.repoId ||
        settled.inventory_guarded !== true || !["released", "discarded"].includes(settled.state)) return null;
    record.holdSettlement = settled;
  }
  if (obj.archiveRefusal !== undefined) {
    if (obj.archiveRefusal !== "oversize" && obj.archiveRefusal !== "quota") return null;
    record.archiveRefusal = obj.archiveRefusal;
  }
  if (obj.finalRequest !== undefined) {
    const req = obj.finalRequest as RecoveryRecord["finalRequest"];
    if (!record.coverageDigest || !req || !req.disposition ||
        req.disposition.coverage_digest !== record.coverageDigest ||
        !["archive", "settled", "held_publication"].includes(req.disposition.kind)) return null;
    // Issue #2545: a held FINAL names the forge publication and the frozen source; it never carries
    // an archive capture id and needs no bundle, so only these facts authenticate it.
    if (req.disposition.kind === "held_publication" &&
        (typeof req.disposition.publication_id !== "string" || !HELD_PUBLICATION_ID_RE.test(req.disposition.publication_id) ||
         req.disposition.source_sha !== record.sourceSha || req.disposition.capture_id !== undefined ||
         req.evidence !== undefined)) return null;
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
