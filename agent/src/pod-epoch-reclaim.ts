import { countPartialCause, mergePartialCauses, readFailureCause, type PartialCauses } from "./pod-epoch-partial-causes.js";
import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { release as osRelease } from "node:os";
import { basename, resolve as resolvePath } from "node:path";
import { MaintenanceBudget } from "./maintenance-budget.js";
import type { inspectPodEpochBootstrap } from "./pod-epoch-bootstrap.js";
import type { PodEpochOwnership } from "./pod-epoch-lock.js";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import type { AuthorityRuntime } from "./authority-runtime.js";
import { isRunOwnershipLost } from "./client.js";
import { parsePodEpochEvidence, assertPodEpochEvidenceBinding } from "./pod-epoch-evidence.js";
import { openPodEpochDaemonBarrier, PodEpochDaemonError, type PodEpochDaemonGuard } from "./pod-epoch-daemon.js";
import type { GitCache, RetainedDiscardResult, RetainedHoldsSnapshot, RetainedLifecycleSnapshot, RetainedSourceSelector } from "./git.js";
import type { RecoveryHoldsResponse, RunOwnershipResponse } from "./protocol.js";

/**
 * Activation gate for the M1b deletion phase. While it is false, production startup is shadow-only: no proof is
 * created and nothing is mutated, whatever deletion dependencies the caller supplies. Flipping it needs the R9
 * owner co-sign recorded in PRD #3431.
 *
 * Test-only override: the deletion phase also runs when `deletion.enableForTest` is exactly the sentinel
 * below, which agent/test/pod-epoch-reclaim-support.ts exports as `ENABLE_DELETION_FOR_TEST`. Nothing under
 * agent/src passes `enableForTest` (a test asserts it), so production can only run deletion through this flag.
 */
export const EPOCH_RECLAIM_DELETION_ENABLED: boolean = false;
const TEST_ENABLE_DELETION = Symbol.for("uzi.test.pod-epoch-reclaim.enable-deletion");

/** What the deletion phase needs; absent means the pass stays shadow-only whatever the gate says. */
export interface PodEpochDeletionDeps {
  /** The worker's authority runtime: one transaction A per source, ended with endSourceUse after a takeover. */
  authority: Pick<AuthorityRuntime, "withTransaction" | "active" | "endSourceUse">;
  /** Bound to RunRunner.withSourceReservation; resolves undefined when the reservation is refused. */
  reserve<T>(runId: string, action: (eligible: () => boolean) => Promise<T>): Promise<T | undefined>;
  /**
   * Worker-wide exclusive-idle proof (bound to terminalMaintenanceProof). The paths argument is accepted for
   * the discard API's shape but is not used to scope the proof.
   */
  quiesce: (paths: string[]) => Promise<boolean>;
  git: Pick<GitCache, "createEpochDiscardProof" | "discardStoppedSource" | "readEpochJournalVector">;
  /** Test override sentinel only; see {@link EPOCH_RECLAIM_DELETION_ENABLED}. */
  enableForTest?: symbol;
}
type Schedule = (callback: () => void, milliseconds: number) => () => void;
interface Options {
  bootstrap: ReturnType<typeof inspectPodEpochBootstrap>;
  ownership: PodEpochOwnership;
  consumption: { result: "fresh" | "existing" | "full"; mayEvaluate: boolean };
  dockerHost?: string;
  git: Pick<GitCache, "discoverEpochRetainedSources" | "inspectRetainedSources">;
  client: {
    listRecoveryHolds(runId: string, signal?: AbortSignal): Promise<RecoveryHoldsResponse>;
    getRunOwnership(runId: string, options: { signal: AbortSignal; deadline: number }): Promise<RunOwnershipResponse>;
  };
  signal: AbortSignal;
  now?: () => number;
  schedule?: Schedule;
  sleep?: (milliseconds: number, signal: AbortSignal) => Promise<unknown>;
  readEvidence?: (path: string, signal: AbortSignal) => Promise<Uint8Array | string>;
  /** Kernel release observation; defaults to the OS release. Read once, on the v2 path only. */
  kernelRelease?: () => string;
  /** M1b deletion dependencies; used only while the activation gate is open. */
  deletion?: PodEpochDeletionDeps;
  /** Daemon barrier opener; defaults to the real one. A seam for tests only, never passed by main.ts. */
  openDaemonGuard?: typeof openPodEpochDaemonBarrier;
  /** Operator log for the deletion phase; every field is a fixed token or a count, never controller text. */
  log?: { warn(message: string, fields?: Record<string, unknown>): void };
}
type Basis = "discard" | "strict_plain";
interface Candidate { run_id: string; path: string; bytes: number; presence: "present" | "missing"; basis: Basis }
/** Private per-candidate evidence the deletion phase re-verifies; never logged. */
interface Admit {
  clonePath: string; barePath: string; selector: RetainedSourceSelector; branch: string;
  kind: "attempt" | "canonical" | "protected"; descriptor: string;
  protectedDiscard: boolean; parts: number; groupSize: number;
}
// Exact crio-crun kernel releases covered by the PRD #3430 audit. Any other release defers
// with kernel_unaudited; refreshing this list needs a reviewed PRD #3430 audit addendum.
const AUDITED_CRIO_KERNELS: readonly string[] = ["6.12.0-243.el10.x86_64", "6.12.0-243.el10.aarch64"];
const RUNTIME_PREFIX = { "containerd-runc": "containerd://", "crio-crun": "cri-o://" } as const;
interface Verdict {
  partial_causes: PartialCauses;
  verdict: "eligible" | "deferred" | "partial";
  reason: string;
  examined: number;
  would_delete: number;
  would_delete_bytes: number;
  would_settle: number;
  by_basis: { discard: number; strict_plain: number };
  kept: number;
  /** Final discard candidates whose marker-less canonical plain journal carried fields beyond
   *  runId and clonePath (for example restoreTip). Report-only, so m6 can review them before M1b. */
  journal_extra_fields: number;
  list: Candidate[];
  list_overflow: number;
  inventory_overflow: number;
  inventory_overflow_exact: boolean;
  inventory_complete: boolean;
  bytes_state: "measured" | "partial" | "unknown";
  /** What the evidence proved; reasons are fixed tokens, never controller text. Deletion
   * authority (M1b) requires BOTH verdict "eligible" AND authority "attested". */
  authority: { result: "undeclared" | "legacy_evidence" | "contradicted" | "unproven" | "attested"; reason: string };
  evidence_version: 1 | 2 | null;
  observations?: { kernel: string };
  evidence_wait_ms: number;
  pass_ms: number;
  settlement_reserve_ms: number;
  /** M1b deletion counters (all zero while the phase does not run). Sources positively removed, and their
   *  shadow-measured allocated bytes. */
  deleted: number;
  deleted_bytes: number;
  /** Missing sources settled by metadata only (no bytes). */
  settled: number;
  /** Takeovers whose endSourceUse did not report the slot ended (kept or active). The delete itself still counts. */
  source_use_not_ended: number;
  /** A source kept before any delete: a coordinator refusal, a failed fresh recheck or a refused reservation. */
  kept_after_refusal: number;
  /** A settled delete failure (residue): the slot stays retiring + settled_failed for the next epoch. */
  kept_residue: number;
  /** Structural keeps, which never downgrade the verdict. */
  kept_protected: number;
  kept_unrecoverable_shape: number;
  /** "measured" once the phase ran (deleted_bytes sums measured shadow bytes of sources positively removed);
   *  "unknown" when it did not run. */
  reclaim_bytes_state: "measured" | "unknown";
}
/** Fatal startup refusal. The caller must exit without admitting a worker or releasing EX. */
export class PodEpochSettlementError extends Error {
  constructor() { super("pod_epoch_settlement_uncertain"); this.name = "PodEpochSettlementError"; }
}
class Refusal extends Error {
  constructor(readonly reason: string) { super(reason); }
}
const timer: Schedule = (callback, milliseconds) => {
  const handle = setTimeout(callback, milliseconds);
  return () => clearTimeout(handle);
};
export async function readPodEpochProjection(path: string, _signal: AbortSignal): Promise<Uint8Array> {
  // Projection symlinks are intentional. O_NONBLOCK prevents waiting for a FIFO writer;
  // it cannot bound OS open/stat/read/close latency (for example on a stalled mount).
  // AbortSignal cannot cancel those operations. Join every actual operation, including
  // close; runPodEpochShadow retains EX on a fatal settlement timeout instead of admitting.
  const handle = await open(path, constants.O_RDONLY | constants.O_NONBLOCK);
  try {
    if (!(await handle.stat()).isFile()) throw new Refusal("evidence_invalid");
    const bytes = Buffer.alloc(8193);
    let length = 0;
    while (length < bytes.length) {
      const read = await handle.read(bytes, length, bytes.length - length, null);
      if (!read.bytesRead) break;
      length += read.bytesRead;
    }
    return bytes.subarray(0, length);
  } finally { await handle.close(); }
}
function holdsSnapshot(response: RecoveryHoldsResponse, runId: string): RetainedHoldsSnapshot {
  if (!response || response.run_id !== runId || !Array.isArray(response.holds) || response.holds.length > 256)
    return { state: "unknown" };
  const holds: Array<{ id: string; generation: number }> = [];
  for (const h of response.holds) {
    if (!h || typeof h.hold_id !== "string" || !h.hold_id.length || h.hold_id.length > 160 ||
        !Number.isSafeInteger(h.generation) || h.generation < 0) return { state: "unknown" };
    holds.push({ id: h.hold_id, generation: h.generation });
  }
  return { state: "read", holds };
}
// Use the operator channel's run-ID alphabet and length bound, with an explicit
// end-of-input assertion before metadata can become an HTTP path component.
const RUN_ID_RE = /^[A-Za-z0-9_-]{1,160}(?![\s\S])/;
/** Deletion-side zero-hold rule: an empty `holds` list AND a matching run-wide count of exactly zero. An absent,
 *  mismatched or non-zero `run_open_holds` is unknown for deletion. The shadow keeps {@link holdsSnapshot}.
 *  Consequence: the api reads the PREDECESSOR pod's stored worker capabilities and this pass runs before the
 *  successor registers, so the first roll onto an image that advertises recovery_run_open_holds_v1 gets no
 *  count and keeps every source. */
function deletionZeroHolds(response: RecoveryHoldsResponse, runId: string): boolean {
  const open = response?.run_open_holds;
  return holdsSnapshot(response, runId).state === "read" && response.holds.length === 0 &&
    !!open && typeof open === "object" && open.run_id === runId && open.count === 0;
}
/** The exact strict journal the epoch proof accepts, or the explicit empty-journal repair; anything else
 *  (no key, a valueless key, duplicates, extra fields) is an unrecoverable shape. */
function journalShape(vector: ReadonlyArray<string | null>, selector: RetainedSourceSelector, clonePath: string):
    string | readonly string[] | undefined {
  if (vector.length !== 1) return undefined;
  const raw = vector[0];
  if (raw === "") return [""];
  if (typeof raw !== "string") return undefined;
  try {
    const journal = JSON.parse(raw) as Record<string, unknown> | null;
    if (!journal || typeof journal !== "object" || Array.isArray(journal)) return undefined;
    if (JSON.stringify(Object.keys(journal).sort()) !== JSON.stringify(["attemptId", "clonePath", "runId"]) ||
        journal.runId !== selector.runId || journal.clonePath !== clonePath ||
        journal.attemptId !== selector.attempt || JSON.stringify(journal) !== raw) return undefined;
    return raw;
  } catch { return undefined; }
}
const safe = (value: string) => value.replace(/[^a-zA-Z0-9._-]/g, "_").slice(0, 160);

/** Read-only, first-process shadow pass. Durable consumption is performed by main.
 * Evidence soft-stops at 115s (hard 120s); the entire pass soft-stops at 595s
 * (hard 600s). Both include a 5s settlement reserve, not an extra grace period.
 * A fatal settlement timeout deliberately leaves EX held; no worker may be admitted.
 * After cleanup, native reacquireShared closes EX and owns its separate 120s SH wait.
 * Admission must await that success; native handover errors propagate unchanged.
 */
export async function runPodEpochShadow(options: Options): Promise<Verdict | undefined> {
  const { bootstrap: boot, ownership, signal: parent } = options;
  const now = options.now ?? Date.now, schedule = options.schedule ?? timer;
  const started = now(), hardPass = started + 600_000;
  const controller = new AbortController();
  let fatal = false, invalid = false;
  let settlementDeadline = Infinity;
  let guard: PodEpochDaemonGuard | undefined;
  let detachVeto = () => {};
  const result: Verdict = {
    partial_causes: {}, verdict: "deferred", reason: "not_evaluated", examined: 0, would_delete: 0, would_delete_bytes: 0,
    would_settle: 0, by_basis: { discard: 0, strict_plain: 0 }, kept: 0, journal_extra_fields: 0,
    authority: { result: "unproven", reason: "not_evaluated" }, evidence_version: null,
    list: [], list_overflow: 0, inventory_overflow: 0, inventory_overflow_exact: false,
    inventory_complete: false, bytes_state: "unknown", evidence_wait_ms: 0, pass_ms: 0,
    settlement_reserve_ms: 5000, deleted: 0, deleted_bytes: 0, settled: 0, source_use_not_ended: 0, kept_after_refusal: 0, kept_residue: 0,
    kept_protected: 0, kept_unrecoverable_shape: 0, reclaim_bytes_state: "unknown",
  };
  // Set once the deletion phase opened its reclaim scope; settled and finished before native handover.
  let reclaimScope: PodEpochReclaimScope | undefined;
  const finishReclaimScope = () => {
    const scope = reclaimScope;
    if (!scope) return;
    try {
      if (scope.stopped() || scope.pendingMutations > 0) throw new Error("reclaim unsettled");
      scope.finish();
      reclaimScope = undefined;
    } catch { fatal = true; throw new PodEpochSettlementError(); }
  };
  let decided = false;
  const decide = (outcome: Verdict["authority"]["result"], reason: string) => {
    result.authority = { result: outcome, reason }; decided = true;
  };
  // One candidate per clone path. A path observed again with a different presence, run, basis or bytes, by any
  // inspected source in which it is not a candidate, or by a group refused for its run ID, is
  // demoted to kept and cannot be re-added.
  const candidates = new Map<string, Candidate>();
  // The deletion phase's private evidence for each candidate path; kept in lockstep with `candidates`.
  const admits = new Map<string, Admit>();
  const refused = new Set<string>();
  // Paths whose source was a marker-less canonical plain journal carrying extra fields (report-only).
  const extraFieldPaths = new Set<string>();
  let invalidReason = "";
  const invalidate = (reason: string) => { invalid = true; invalidReason ||= reason; };
  const examined = new Set<string>();
  const measured = new Set<string>();
  const stop = (reason: string) => {
    if (!controller.signal.aborted) {
      settlementDeadline = Math.min(hardPass, now() + 5000);
      controller.abort(new Refusal(reason));
    }
  };
  const parentAbort = () => stop("shutdown");
  parent.addEventListener("abort", parentAbort, { once: true });
  if (parent.aborted) parentAbort();
  const cancelPass = schedule(() => stop("pass_deadline"), 595_000);

  // Race only to a fatal EX-preserving refusal. Every nonfatal return joins owned work.
  // At most one stage runs at a time; no retries except the bounded evidence polling loop.
  async function owned<T>(work: () => Promise<T>, hard: number, cleanup = false): Promise<T> {
    let cancel = () => {};
    let abortListener = () => {};
    const timeout = new Promise<never>((_resolve, reject) => {
      const arm = (at: number) => {
        cancel();
        cancel = schedule(() => { fatal = true; reject(new PodEpochSettlementError()); }, Math.max(0, at - now()));
      };
      arm(hard);
      abortListener = () => arm(Math.min(hard, settlementDeadline));
      controller.signal.addEventListener("abort", abortListener, { once: true });
      if (controller.signal.aborted) abortListener();
    });
    try {
      if (!cleanup) controller.signal.throwIfAborted();
      const value = await Promise.race([Promise.resolve().then(work), timeout]);
      if (now() >= hard) { fatal = true; throw new PodEpochSettlementError(); }
      if (!cleanup) controller.signal.throwIfAborted();
      return value;
    } finally {
      cancel();
      controller.signal.removeEventListener("abort", abortListener);
    }
  }
  const check = () => {
    guard?.assertHealthy();
    if (invalid) throw new Refusal("entry_changed");
    controller.signal.throwIfAborted();
  };
  try {
    if (options.consumption.result === "existing") return undefined;
    controller.signal.throwIfAborted();
    if (ownership.mode === "unsupported") throw new Refusal("unsupported_filesystem");
    if (options.consumption.result === "full") throw new Refusal("consumption_full");
    if (boot.firstMode !== "exclusive" || !boot.runtime || !boot.evidenceFile) {
      if (boot.reason === "runtime_not_declared") decide("undeclared", "runtime_not_declared");
      throw new Refusal(boot.reason ?? "bootstrap_uncertain");
    }
    if (ownership.mode !== "exclusive") throw new Refusal("lock_busy");
    if (!options.consumption.mayEvaluate) throw new Refusal("consumption_uncertain");
    const evidenceStart = now(), hardEvidence = Math.min(hardPass, evidenceStart + 120_000);
    const cancelEvidence = schedule(() => stop("evidence_timeout"), Math.max(0, hardEvidence - 5000 - now()));
    let document;
    try {
      for (;;) {
        let raw: Uint8Array | string | undefined;
        try {
          raw = await owned(() => (options.readEvidence ?? readPodEpochProjection)(boot.evidenceFile!, controller.signal), hardEvidence);
        } catch (error) {
          if (fatal || controller.signal.aborted) throw error;
          if ((error as NodeJS.ErrnoException)?.code !== "ENOENT") throw new Refusal("evidence_invalid");
        }
        if (raw !== undefined && raw.length) {
          try { document = parsePodEpochEvidence(raw); }
          catch { throw new Refusal("evidence_invalid"); }
          break;
        }
        await owned(() => options.sleep ? options.sleep(250, controller.signal) : new Promise<void>((resolve, reject) => {
          const abort = () => { cancel(); reject(controller.signal.reason); };
          const cancel = schedule(() => { controller.signal.removeEventListener("abort", abort); resolve(); }, 250);
          controller.signal.addEventListener("abort", abort, { once: true });
          if (controller.signal.aborted) abort();
        }), hardEvidence);
      }
    } finally { cancelEvidence(); result.evidence_wait_ms = Math.max(0, now() - evidenceStart); }
    if ("handoff" in document) throw new Refusal("no_predecessor_evidence");
    result.evidence_version = document.version;
    try { assertPodEpochEvidenceBinding(document, boot.podUid, boot.runtime); }
    catch { throw new Refusal("evidence_binding"); }
    // A controller-declared contradiction defers; its free-text reason is never echoed.
    if (document.version === 2 && document.attestation.result !== "consistent") {
      decide("contradicted", "attestation_contradicted");
      throw new Refusal("attestation_contradicted");
    }
    if (document.version === 1) {
      decide("legacy_evidence", "legacy_evidence");
      throw new Refusal("legacy_evidence");
    }
    const evidence = document;
    // The worker recomputes the runtime prefixes itself; the controller's attestation is
    // never trusted alone.
    const prefix = RUNTIME_PREFIX[boot.runtime];
    const contradict = (reason: string): never => { decide("contradicted", reason); throw new Refusal(reason); };
    if (![...evidence.containers, ...evidence.initContainers].every(c => c.containerID.startsWith(prefix)))
      contradict("predecessor_runtime_prefix");
    if (!evidence.successorContainers.every(c => c.containerID.startsWith(prefix)))
      contradict("successor_runtime_prefix");
    let kernel = "";
    try { kernel = String((options.kernelRelease ?? osRelease)()); } catch { /* unaudited below for crio-crun */ }
    result.observations = { kernel: kernel.replace(/[^A-Za-z0-9._+-]/g, "_").slice(0, 64) };
    if (boot.runtime === "crio-crun" && !AUDITED_CRIO_KERNELS.includes(kernel)) contradict("kernel_unaudited");
    const entry = () => {
      try {
        if (!ownership.matchesEntry(evidence.successor, evidence.predecessor)) throw new Error();
      } catch { invalidate("entry_changed"); stop("entry_changed"); throw new Refusal("entry_changed"); }
    };
    entry();
    decide("attested", "observations_consistent");
    guard = await owned(async () => {
      const opened = await (options.openDaemonGuard ?? openPodEpochDaemonBarrier)({ dockerHost: options.dockerHost ?? "", signal: controller.signal,
        deadline: hardPass - 5000, now, schedule });
      // If opening outlives fatal refusal, only cleanup may continue, with EX retained.
      if (fatal) { await opened.close(); throw new PodEpochSettlementError(); }
      guard = opened;
      return opened;
    }, hardPass);
    const veto = () => { invalidate("daemon_veto"); stop("daemon_veto"); };
    guard.signal.addEventListener("abort", veto, { once: true });
    detachVeto = () => guard?.signal.removeEventListener("abort", veto);
    if (guard.signal.aborted) veto();
    check();
    const census = await owned(() => options.git.discoverEpochRetainedSources({
      signal: controller.signal, deadline: hardPass - 5000, maxPhysicalPaths: 200,
    }), hardPass);
    check();
    result.inventory_complete = census.inventoryComplete && census.unreadable === 0;
    result.inventory_overflow = Math.max(0, census.overflow);
    result.inventory_overflow_exact = census.inventoryComplete && census.overflowExact;
    let partial = !result.inventory_complete || census.overflow > 0;
    const censusDiagnosed = mergePartialCauses(result.partial_causes, census.partialCauses);
    if (partial && !censusDiagnosed) countPartialCause(result.partial_causes, "census_unclassified");
    const censusPaths = new Set<string>();
    const groups = [];
    for (const group of census.groups) {
      const fresh = [...new Set(group.paths)].filter(p => !censusPaths.has(p));
      if (groups.length >= 200 || censusPaths.size + fresh.length > 200) { countPartialCause(result.partial_causes, groups.length >= 200 ? "group_cap" : "group_physical_path_cap"); partial = true; break; }
      for (const p of fresh) censusPaths.add(p);
      groups.push(group);
    }
    for (const group of groups) {
      check();
      for (const path of group.paths) examined.add(path);
      // Refuse before either API call: retained metadata is not an HTTP path authority.
      // Keep every path in this group; continue bounded sibling work without retrying it.
      if (typeof group.selector.runId !== "string" || !RUN_ID_RE.test(group.selector.runId)) {
        countPartialCause(result.partial_causes, "selector_refused");
        partial = true;
        for (const path of group.paths) { candidates.delete(path); admits.delete(path); refused.add(path); }
        continue;
      }
      let holds: RetainedHoldsSnapshot = { state: "unknown" };
      let lifecycle: RetainedLifecycleSnapshot = { state: "unknown" };
      let readStage: "ownership" | "holds" = "ownership";
      try {
        // Lifecycle first, so a terminal run's holds read is at least as current.
        const row = await owned(() => MaintenanceBudget.runOwned(controller.signal,
          () => options.client.getRunOwnership(group.selector.runId, {
            signal: controller.signal, deadline: hardPass - 5000,
          })), hardPass);
        if (row && (row.status === "completed" || row.status === "failed" || row.status === "cancelled"))
          lifecycle = { state: "terminal", status: row.status };
        readStage = "holds";
        holds = holdsSnapshot(await owned(() => MaintenanceBudget.runOwned(controller.signal,
          () => options.client.listRecoveryHolds(group.selector.runId, controller.signal)), hardPass), group.selector.runId);
      } catch (error) {
        if (fatal || controller.signal.aborted) throw error;
        // A DEFINITIVE exact ownership 404 for this run keeps only this group's sources (lifecycle and holds
        // stay unknown, so none is a candidate): no holds read, no authority, not counted as partial. Every
        // other failure, including a 404 with any other body, still makes the whole pass partial.
        if (!(readStage === "ownership" && isRunOwnershipLost(error, group.selector.runId))) {
          countPartialCause(result.partial_causes, readFailureCause(readStage, error)); partial = true;
        }
      }
      const expected = new Set(group.paths);
      const inspected = await owned(() => options.git.inspectRetainedSources({
        epoch: true, maxDescriptors: 200, maxPhysicalPaths: expected.size,
        barePath: group.barePath, selector: group.selector, holds, lifecycle,
        isLivePath: () => false, signal: controller.signal, deadline: hardPass - 5000,
        budgetMs: Math.max(0, hardPass - 5000 - now()), now,
        beforeTree: path => { check(); entry(); if (!expected.has(path)) { invalidate("path_set_changed"); stop("path_set_changed"); throw new Refusal("path_set_changed"); } },
      }), hardPass);
      check(); entry();
      const actual = new Set(inspected.sources.flatMap(s => s.sources.map(p => p.clonePath)));
      const drift = actual.size !== expected.size || [...actual].some(p => !expected.has(p));
      if (drift) { invalidate("path_set_changed"); throw new Refusal("path_set_changed"); }
      const complete = !inspected.partial && inspected.unreadable === 0;
      const inspectionDiagnosed = mergePartialCauses(result.partial_causes, inspected.partialCauses);
      if (!complete && !inspectionDiagnosed) countPartialCause(result.partial_causes, "inspection_unclassified");
      partial ||= !complete;
      for (const source of inspected.sources) {
        check(); entry();
        const eligible = complete && lifecycle.state === "terminal" && holds.state === "read" && holds.holds.length === 0 &&
          source.runId === group.selector.runId && source.barePath === group.barePath &&
          (source.presence === "present" || source.presence === "missing") && source.descriptor.length > 0 && source.localRecords.length === 0 &&
          source.holds.state === "read" && source.holds.ids.length === 0;
        const basis: Basis | undefined =
          source.disposition?.kind === "local_discard" || source.disposition?.kind === "local_protected_discard" ? "discard" :
          source.epochSettlement?.disposition.kind === "local_discard" ? "strict_plain" : undefined;
        for (const part of source.sources) {
          examined.add(part.clonePath);
          if (part.presence !== "unsafe" && part.allocatedBytes !== null &&
              Number.isSafeInteger(part.allocatedBytes) && part.allocatedBytes >= 0) measured.add(part.clonePath);
          let candidate: Candidate | undefined;
          if (eligible && basis !== undefined && !part.live) {
            const entry = { run_id: safe(source.runId), path: safe(basename(part.clonePath)) };
            if (part.presence === "present" && source.bytesState === "measured" &&
                part.allocatedBytes !== null && Number.isSafeInteger(part.allocatedBytes) && part.allocatedBytes >= 0)
              candidate = { ...entry, bytes: part.allocatedBytes, presence: "present", basis };
            else if (part.presence === "missing")
              candidate = { ...entry, bytes: 0, presence: "missing", basis };
          }
          if (candidate && source.epochJournalExtraFields) extraFieldPaths.add(part.clonePath);
          const previous = candidates.get(part.clonePath);
          const priorAdmit = admits.get(part.clonePath);
          if (!candidate || refused.has(part.clonePath) || (priorAdmit && (priorAdmit.barePath !== group.barePath ||
              priorAdmit.selector.runId !== group.selector.runId ||
              priorAdmit.selector.generation !== group.selector.generation || priorAdmit.selector.attempt !== group.selector.attempt)) || (previous && (previous.presence !== candidate.presence || previous.run_id !== candidate.run_id ||
              previous.basis !== candidate.basis || previous.bytes !== candidate.bytes))) {
            candidates.delete(part.clonePath); admits.delete(part.clonePath); refused.add(part.clonePath);
          } else {
            candidates.set(part.clonePath, candidate);
            admits.set(part.clonePath, { clonePath: part.clonePath, barePath: group.barePath, selector: group.selector,
              branch: source.branch, kind: source.kind, descriptor: source.descriptor,
              protectedDiscard: source.disposition?.kind === "local_protected_discard",
              parts: source.sources.length, groupSize: group.paths.length });
          }
        }
      }
    }
    check(); entry();
    if (partial) result.inventory_complete = false;
    result.verdict = partial ? "partial" : "eligible";
    result.reason = partial ? "inventory_partial" : "shadow_complete";
    result.bytes_state = measured.size === examined.size && !partial ? "measured" :
      measured.size ? "partial" : "unknown";

    // ---- M1b deletion phase. Shadow counters and the verdict above are final; this only reclaims. ----
    // Runs only when ALL hold: the activation gate (or the test override), a shadow verdict of "eligible" with
    // attested authority, an intact pass (not invalidated, aborted or soft-stopped), a native reclaim scope on
    // the ownership, and the deletion dependencies. Anything less stays shadow-only and mutates nothing.
    const deps = options.deletion;
    if (deps && (EPOCH_RECLAIM_DELETION_ENABLED || deps.enableForTest === TEST_ENABLE_DELETION) &&
        result.verdict === "eligible" && result.authority.result === "attested" &&
        !invalid && !controller.signal.aborted && typeof ownership.beginReclaim === "function") {
      // With no admitted candidate nothing can be mutated, so no native scope is opened or finished; the pass
      // still reports reclaim_complete. Every loop body below runs only when admits is non-empty.
      const scope = admits.size > 0 ? ownership.beginReclaim() : undefined;
      reclaimScope = scope;
      // Pending children or a stop latch are never settled by a later step: fail closed with EX kept.
      const assertSettled = () => {
        if (scope && (scope.stopped() || scope.pendingMutations > 0)) { fatal = true; throw new PodEpochSettlementError(); }
      };
      const deadlineAt = () => Date.now() + Math.max(0, hardPass - 5000 - now());
      // Fresh lifecycle then holds; undefined keeps the source. Never throws: a failed, aborted or
      // non-definitive read is a keep (bounded by the pass signal and deadline).
      const readAuthority = async (runId: string) => {
        try {
          const row = await MaintenanceBudget.runOwned(controller.signal, () => options.client.getRunOwnership(runId, {
            signal: controller.signal, deadline: hardPass - 5000 }));
          if (!row || !(row.status === "completed" || row.status === "failed" || row.status === "cancelled")) return undefined;
          const response = await MaintenanceBudget.runOwned(controller.signal,
            () => options.client.listRecoveryHolds(runId, controller.signal));
          if (!deletionZeroHolds(response, runId)) return undefined;
          return { lifecycle: { state: "terminal", status: row.status } as RetainedLifecycleSnapshot,
            holds: { state: "read", holds: [] } as RetainedHoldsSnapshot };
        } catch { return undefined; }
      };
      let downgraded = false, skipped = false;
      const refusal = () => { result.kept_after_refusal++; downgraded = true; };
      // Admitted candidates are processed serially, each to a settled outcome before the next starts.
      for (const [clonePath, admit] of admits) {
        const candidate = candidates.get(clonePath);
        if (!candidate) continue;
        // No new source after shutdown or the soft stop.
        if (controller.signal.aborted) { skipped = true; break; }
        // Structural keeps are refused before any authority read or proof, and never downgrade the verdict.
        if (admit.protectedDiscard) { result.kept_protected++; continue; }
        if (admit.kind !== "attempt" || admit.parts !== 1 || admit.groupSize !== 1) { result.kept_unrecoverable_shape++; continue; }
        const runId = admit.selector.runId;
        let step: { result: RetainedDiscardResult } | "keep" | "shape" | undefined;
        let effects = false, notEnded = false;
        try {
          // 2. fresh lifecycle and holds with the deletion-side zero rule.
          const first = await owned(() => readAuthority(runId), hardPass, true);
          if (!first) { refusal(); if (controller.signal.aborted) { skipped = true; break; } continue; }
          // 3. entry and daemon health, then re-inspect this source: it must still be the same candidate.
          check(); entry();
          const inspected = await owned(() => options.git.inspectRetainedSources({
            epoch: true, maxDescriptors: 200, maxPhysicalPaths: admit.groupSize, barePath: admit.barePath,
            selector: admit.selector, holds: first.holds, lifecycle: first.lifecycle, isLivePath: () => false,
            signal: controller.signal, deadline: hardPass - 5000, budgetMs: Math.max(0, hardPass - 5000 - now()), now,
            beforeTree: path => { check(); entry(); if (path !== admit.clonePath) throw new Refusal("path_set_changed"); },
          }), hardPass, true);
          check(); entry();
          const only = inspected.sources.length === 1 ? inspected.sources[0]! : undefined;
          const plain = only?.disposition?.kind === "local_discard" || only?.epochSettlement?.disposition.kind === "local_discard";
          if (!only || inspected.partial || inspected.unreadable !== 0 || only.runId !== runId || only.barePath !== admit.barePath ||
              only.kind !== "attempt" || only.descriptor !== admit.descriptor || only.presence !== candidate.presence ||
              only.sources.length !== 1 || only.sources[0]!.clonePath !== clonePath || only.sources[0]!.live ||
              only.localRecords.length !== 0 || only.holds.state !== "read" || only.holds.ids.length !== 0 || !plain) {
            refusal(); continue;
          }
          // 4. the raw journal must be the exact strict three-key string or the explicit `[""]` repair; a no-key
          //    attempt is an unrecoverable shape, refused before any proof exists.
          const raw = journalShape(await owned(() => deps.git.readEpochJournalVector(admit.barePath, admit.branch),
            hardPass, true), admit.selector, clonePath);
          if (raw === undefined) { step = "shape"; }
          else {
            // 5. one authority transaction A, the run's source reservation S inside it, B only inside the discard.
            step = await owned(() => deps.authority.withTransaction(tx => deps.reserve(runId, async eligible => {
              if (!eligible() || controller.signal.aborted) return "keep" as const;
              // Fresh authority reads inside the reservation: the proof's lifecycle and holds come from here.
              const fresh = await readAuthority(runId);
              if (!fresh) return "keep" as const;
              check(); entry();
              const live = () => {
                if (!eligible() || deps.authority.active() !== tx) throw new Error("reservation or transaction lost");
                check();
              };
              const proof = deps.git.createEpochDiscardProof({
                decision: { eligible: true, attested: true }, scope: scope!, selector: admit.selector, barePath: admit.barePath,
                paths: [clonePath], confirm: only.descriptor, expectedRawJournal: raw, checkLive: live,
                beforeEffect: async () => { check(); entry(); }, signal: controller.signal, deadline: deadlineAt(),
              });
              effects = true;
              const discarded = await deps.git.discardStoppedSource({
                selector: admit.selector, confirm: only.descriptor, holds: fresh.holds, lifecycle: fresh.lifecycle,
                isLivePath: () => false, quiesce: deps.quiesce, barePath: admit.barePath, epochProof: proof,
              });
              if (discarded.tookOver) {
                const end = await deps.authority.endSourceUse([clonePath], { skipFlightOwned: true });
                notEnded = !end?.ended?.includes(resolvePath(clonePath));
              }
              return { result: discarded };
            })), hardPass, true);
          }
        } catch (error) {
          if (error instanceof PodEpochSettlementError) throw error;
          // After a discard began, a thrown error leaves the outcome unproven: EX is kept.
          if (effects) { fatal = true; throw new PodEpochSettlementError(); }
          // A daemon veto or an invalidated pass ends the whole pass with its own reason; it is not a per-source keep.
          if (error instanceof PodEpochDaemonError || invalid) throw error;
          step = "keep";
        }
        if (notEnded) {
          result.source_use_not_ended++;
          options.log?.warn("pod epoch reclaim: source use not ended after a takeover", { cause: "source_use_not_ended" });
        }
        // 6. classify only AFTER the reservation and the transaction are released.
        assertSettled();
        if (step === "shape") { result.kept_unrecoverable_shape++; continue; }
        if (step === undefined || step === "keep") { refusal(); if (controller.signal.aborted) { skipped = true; break; } continue; }
        const outcome = step.result;
        if (outcome.kind === "done") {
          const removed = outcome.sources.length === 1 ? outcome.sources[0]!.outcome : undefined;
          if (removed === "removed") { result.deleted++; result.deleted_bytes += candidate.bytes; }
          else if (removed === "missing") result.settled++;
          else { fatal = true; throw new PodEpochSettlementError(); }
        } else if (outcome.kind === "refused") refusal();
        else if (outcome.kind === "incomplete") { result.kept_residue++; downgraded = true; }
        else { fatal = true; throw new PodEpochSettlementError(); }
      }
      result.reclaim_bytes_state = "measured";
      if (skipped) {
        result.verdict = "partial";
        result.reason = controller.signal.reason instanceof Refusal ? controller.signal.reason.reason : "pass_aborted";
      } else if (downgraded) { result.verdict = "partial"; result.reason = "reclaim_partial"; }
      else result.reason = "reclaim_complete";
    }
  } catch (error) {
    if (error instanceof PodEpochSettlementError) { fatal = true; throw error; }
    result.verdict = controller.signal.aborted ? "partial" : "deferred";
    const cause = controller.signal.reason;
    result.reason = error instanceof PodEpochDaemonError ? error.reason :
      cause instanceof Refusal ? cause.reason : error instanceof Refusal ? error.reason : "read_failed";
    result.inventory_complete = false;
    result.inventory_overflow_exact = false;
    result.bytes_state = examined.size ? "partial" : "unknown";
    if (!decided) decide("unproven", result.reason);
  } finally {
    try {
      if (!fatal && guard) {
        // Assert once more before detaching intentional daemon_closed notification.
        try { guard.assertHealthy(); }
        catch {
          invalidate("daemon_veto");
          if (result.verdict === "eligible") { result.verdict = "deferred"; result.reason = "daemon_veto"; }
          result.inventory_complete = false;
        }
        detachVeto();
        await owned(() => guard!.close(), hardPass, true);
      }
      result.pass_ms = Math.max(0, now() - started);
      cancelPass();
      // EX is closed inside native handover. Its bounded 120s SH wait, not owned(),
      // determines success or failure; the caller must not admit until it succeeds.
      // A reclaim scope must be settled and finished before native handover; a pending child, a stop latch
      // or a finish() failure keeps EX and refuses admission.
      if (!fatal) finishReclaimScope();
      if (!fatal && !parent.aborted && ownership.mode === "exclusive")
        await ownership.reacquireShared();
    } finally {
      cancelPass(); parent.removeEventListener("abort", parentAbort);
    }
  }
  if (controller.signal.aborted && result.verdict === "eligible") {
    result.verdict = "partial";
    result.reason = controller.signal.reason instanceof Refusal ? controller.signal.reason.reason : "pass_aborted";
    result.inventory_complete = false;
    result.inventory_overflow_exact = false;
    result.bytes_state = "partial";
  }
  if (invalid) {
    candidates.clear();
    result.authority = { result: "unproven", reason: invalidReason };
  }
  const all = [...candidates.values()];
  const present = all.filter(c => c.presence === "present"), missing = all.filter(c => c.presence === "missing");
  result.examined = examined.size;
  result.would_delete = present.length;
  result.would_delete_bytes = present.reduce((sum, c) => sum + c.bytes, 0);
  // would_settle counts missing candidate PATHS: a present source may contribute a missing part.
  result.would_settle = missing.length;
  result.by_basis = {
    discard: all.filter(c => c.basis === "discard").length,
    strict_plain: all.filter(c => c.basis === "strict_plain").length,
  };
  // Final candidates only: a demoted or cleared path is never counted.
  result.journal_extra_fields = [...candidates.keys()].filter(p => extraFieldPaths.has(p)).length;
  // Candidates are a subset of examined paths, so the invariant examined = delete + settle + kept
  // holds without help; the clamp is only a defensive floor.
  result.kept = Math.max(0, examined.size - all.length);
  result.list = [...present, ...missing].slice(0, 32);
  result.list_overflow = Math.max(0, all.length - result.list.length);
  return result;
}
