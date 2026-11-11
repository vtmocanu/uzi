import { countPartialCause, mergePartialCauses, readFailureCause, type PartialCauses } from "./pod-epoch-partial-causes.js";
import { constants, statfsSync } from "node:fs";
import { open } from "node:fs/promises";
import { release as osRelease } from "node:os";
import { basename, resolve as resolvePath } from "node:path";
import { dockerSidecarExpected } from "./docker-wiring.js";
import { MaintenanceBudget } from "./maintenance-budget.js";
import type { inspectPodEpochBootstrap } from "./pod-epoch-bootstrap.js";
import type { PodEpochOwnership } from "./pod-epoch-lock.js";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import type { AuthorityRuntime } from "./authority-runtime.js";
import { isRunOwnershipLost } from "./client.js";
import { parsePodEpochEvidence, assertPodEpochEvidenceBinding } from "./pod-epoch-evidence.js";
import { openPodEpochDaemonBarrier, PodEpochDaemonError, type PodEpochDaemonGuard } from "./pod-epoch-daemon.js";
import type { EpochScratchDiscardInput, GitCache, RetainedDiscardResult, RetainedHoldsSnapshot, RetainedLifecycleSnapshot, RetainedSourceSelector } from "./git.js";
import type { RecoveryHoldsResponse, RunOwnershipResponse } from "./protocol.js";
import { censusScratchRoot } from "./rmtree.js";
import {
  KEPT_REASONS, censusEntryUnchanged, classifyBindSources, classifyScratchCensus, parseScratchCensus, pinScratchRoot,
  protectedLeaves, readPodEntryFence, readScratchMounts, repinScratchRoot, scanScratchProcesses, scratchFilesystemOf,
  scratchLayout, type ScratchIo, type ScratchLayout, type ScratchRootPin, type Settlement,
} from "./pod-epoch-scratch.js";

/**
 * Startup activation for the M1b deletion phase. Parsed once from the worker's own process env: the controller
 * renders UZI_EPOCH_RECLAIM_DELETION=1 only for the one selected canary worker. It permits attempting the phase
 * and supplies no evidence: the shadow verdict, attested authority and every other gate in runPodEpochShadow
 * still decide whether anything is reclaimed. Unset, empty or "0" is off; "1" needs an epoch-locked, Docker-wired
 * worker (UZI_EPOCH_LOCK=1 and DOCKER_HOST or UZI_DIND_SOCKET); anything else is a startup error (no trimming).
 *
 * Test-only override: the deletion phase also runs when `deletion.enableForTest` is exactly the sentinel
 * below, which agent/test/pod-epoch-reclaim-support.ts exports as `ENABLE_DELETION_FOR_TEST`. Nothing under
 * agent/src passes `enableForTest` (a test asserts it), so production can only run deletion through the
 * startup activation.
 */
export function readEpochReclaimDeletionActivation(env: NodeJS.ProcessEnv): boolean {
  const raw = env.UZI_EPOCH_RECLAIM_DELETION;
  if (raw === undefined || raw === "" || raw === "0") return false;
  if (raw === "1") {
    if (env.UZI_EPOCH_LOCK === "1" && dockerSidecarExpected(env)) return true;
    throw new Error("UZI_EPOCH_RECLAIM_DELETION=1 requires an epoch-locked, Docker-wired worker (UZI_EPOCH_LOCK=1 and DOCKER_HOST or UZI_DIND_SOCKET)");
  }
  throw new Error("UZI_EPOCH_RECLAIM_DELETION must be unset, empty, 0 or 1");
}
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
  git: Pick<GitCache, "createEpochDiscardProof" | "discardStoppedSource" | "readEpochJournalVector" | "assertNoUnsettledDeletes">;
  /** Startup opt-in; set only by main.ts from {@link readEpochReclaimDeletionActivation}. Permits the phase, proves nothing. */
  activation?: boolean;
  /** Test override sentinel only; production uses `activation`. */
  enableForTest?: symbol;
}
/**
 * Activation gate for the runner-scratch reclaim (issue #4043), separate from the attempt-deletion activation
 * ({@link PodEpochDeletionDeps.activation}, the one-worker canary of issue #4134)
 * and independent of it: the attempt gate being open never starts scratch work and the reverse. While it is false,
 * production startup does nothing for scratch: no census, no new child and no new path read. Flipping it needs a
 * separate owner decision recorded on issue #4043.
 *
 * Test-only override: the phase also runs when `scratch.scratchTestGate` is exactly the sentinel below, which
 * agent/test/pod-epoch-reclaim-support.ts exports as `ENABLE_SCRATCH_FOR_TEST`. Nothing under agent/src passes
 * `scratchTestGate` (a test asserts it), and the seams in {@link PodEpochScratchDeps.seams} are ignored without it.
 */
export const EPOCH_SCRATCH_DELETION_ENABLED: boolean = false;
const TEST_SCRATCH_GATE = Symbol.for("uzi.test.pod-epoch-reclaim.scratch-gate");

/** Test seams of the scratch phase; honored only under the test gate sentinel, never passed by main.ts. */
export interface PodEpochScratchTestSeams {
  /** Environment for the exact-layout gate (defaults to process.env). */
  env?: NodeJS.ProcessEnv;
  /** Replaces the layout the gate returned, so a fixture can use a temp root. */
  layout?: ScratchLayout;
  io?: ScratchIo;
  podEntryFile?: string;
  readFence?: (podUid: string) => ReturnType<typeof readPodEntryFence>;
  readMounts?: (leafNames: readonly string[]) => ReturnType<typeof readScratchMounts>;
  statfs?: () => "ok" | "fs_unsupported";
  freeBytes?: () => bigint | null;
  scanProcesses?: (leafNames: readonly string[]) => ReturnType<typeof scanScratchProcesses>;
  bindLstat?: Parameters<typeof classifyBindSources>[0]["lstat"];
  bindShared?: string;
  census?: typeof censusScratchRoot;
  discardSeam?: EpochScratchDiscardInput["testSeam"];
}
/** What the scratch phase needs; absent means no scratch work whatever the gate says. */
export interface PodEpochScratchDeps {
  git: Pick<GitCache, "collectEpochScratchProtectedPaths" | "discardEpochScratchLeaf" | "assertNoUnsettledDeletes">;
  /** Worker-wide exclusive-idle proof (bound to terminalMaintenanceProof). */
  quiesce: () => Promise<boolean>;
  /** Test override sentinel only; see {@link EPOCH_SCRATCH_DELETION_ENABLED}. */
  scratchTestGate?: symbol;
  seams?: PodEpochScratchTestSeams;
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
  /** Runner-scratch reclaim dependencies; used only while the scratch gate is open. */
  scratch?: PodEpochScratchDeps;
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
  protectedDiscard: boolean; parts: number; groupSize: number; ledgerState: string;
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
  /** Runner-scratch reclaim (issue #4043); always present, `not_run` while the gate is closed. Only fixed tokens
   *  and counts, never controller text or path names. */
  scratch: ScratchVerdict;
}
interface ScratchVerdict {
  state: "not_run" | "refused" | "ran" | "stopped";
  reason: string;
  layout: "split" | "single" | null;
  examined: number;
  /** Kept leaves by fixed reason (the classifier's reasons plus `changed`, a leaf that moved before its delete). */
  kept: Record<string, number>;
  /** Leaves admitted at census time. `kept.mount`, `kept.changed` and `kept.held_open` may also count an
   *  admitted leaf demoted before its delete, so those counts overlap `eligible`. */
  eligible: number;
  eligible_allocated_bytes_estimate: number;
  shared_bytes: number;
  deleted: number;
  deleted_allocated_bytes_estimate: number;
  residue: number;
  helper_failed: number;
  estimate_partial: boolean;
  /** Pool available-bytes change across the delete loop (all writers, so unattributed); null when unreadable. */
  statfs_free_delta_unattributed: number | null;
  /** Estimated from allocated blocks, never proof that storage was freed. */
  reclaim_bytes_state: "estimated" | "unknown";
  /** The root listing hit its entry cap, so some top-level names were never seen. */
  list_overflow: boolean;
  /** The idle proof failed after the last delete joined (everything deleted was already settled); the phase then
   *  reports `stopped`/`not_idle` with the settled counts kept. */
  not_idle_after: boolean;
}
const emptyScratch = (): ScratchVerdict => ({
  state: "not_run", reason: "not_run", layout: null, examined: 0,
  kept: Object.fromEntries([...KEPT_REASONS, "changed"].map(reason => [reason, 0])), eligible: 0,
  eligible_allocated_bytes_estimate: 0, shared_bytes: 0, deleted: 0, deleted_allocated_bytes_estimate: 0, residue: 0,
  helper_failed: 0, estimate_partial: false, statfs_free_delta_unattributed: null, reclaim_bytes_state: "unknown",
  list_overflow: false, not_idle_after: false,
});
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

interface ScratchContext {
  deps: PodEpochScratchDeps;
  seams: PodEpochScratchTestSeams;
  podUid: string;
  guard: PodEpochDaemonGuard;
  controller: AbortController;
  owned<T>(work: () => Promise<T>, hard: number, cleanup?: boolean): Promise<T>;
  check(): void;
  entry(): void;
  now(): number;
  hardPass: number;
  deadlineAt(): number;
  out: ScratchVerdict;
  openScope(): PodEpochReclaimScope;
  /** True once the pass is fatal or aborted: an error is then the pass's own and propagates. */
  interrupted(): boolean;
  failFatal(): never;
  /** Invalidates the pass as a daemon veto (the real guard has already done so itself). */
  veto(): void;
}
const SCRATCH_CENSUS_MARGIN_MS = 10_000;
const SCRATCH_RECENSUS_MAX_MS = 30_000;
const SCRATCH_RETAINED_STOP = 3;
const MAX_SAFE_BIGINT = BigInt(Number.MAX_SAFE_INTEGER);

function poolFreeBytes(fd: number): bigint | null {
  try {
    const stat = statfsSync(`/proc/self/fd/${fd}`, { bigint: true });
    return stat.bavail * stat.bsize;
  } catch { return null; }
}

/**
 * The runner-scratch phase (issue #4043 plan 1d). Every refusal is recorded as `refused`/`stopped` with a fixed
 * token on `ctx.out` and returns; the caller's attempt phase then continues unless the pass was invalidated.
 * Throws only for the pass's own fatal/abort/veto outcomes. Deletes serially, each leaf to a settled
 * classification before the next begins.
 */
async function runScratchPhase(ctx: ScratchContext): Promise<void> {
  const { deps, seams, out } = ctx;
  const refuse = (reason: string): void => { out.state = "refused"; out.reason = reason; };
  const stopPhase = (reason: string): void => { out.state = "stopped"; out.reason = reason; };
  try { deps.git.assertNoUnsettledDeletes(); } catch { ctx.failFatal(); }
  const gate = scratchLayout(seams.env ?? process.env);
  if ("refused" in gate) return refuse(gate.refused);
  out.layout = gate.mode;
  const layout: ScratchLayout = seams.layout ?? gate;
  const pin = pinScratchRoot(layout, seams.io);
  if ("refused" in pin) return refuse(pin.refused);
  try { await scratchBody(ctx, layout, pin, refuse, stopPhase); }
  catch (error) {
    // The pass's own abort or veto ended the phase mid-way: record it before propagating.
    if (out.state === "ran" || out.state === "not_run") stopPhase(error instanceof Refusal || error instanceof PodEpochDaemonError ? error.reason : "interrupted");
    throw error;
  } finally { pin.close(); }
}

async function scratchBody(ctx: ScratchContext, layout: ScratchLayout, pin: ScratchRootPin,
    refuse: (reason: string) => void, stopPhase: (reason: string) => void): Promise<void> {
  const { deps, seams, out, controller, guard } = ctx;
  const fence = (seams.readFence ?? ((uid: string) => readPodEntryFence(uid, seams.io, seams.podEntryFile)))(ctx.podUid);
  if ("refused" in fence) return refuse(fence.refused);
  const readMounts = (names: readonly string[]) =>
    (seams.readMounts ?? ((n: readonly string[]) => readScratchMounts(pin, n, layout.root, seams.io)))(names);
  // Before the census the leaf names are unknown, so only the topology refusal is decided here; a mount inside
  // the root is judged against the listed names below.
  const early = readMounts([]);
  if ("refused" in early && early.refused === "mount_unexpected_ancestor") return refuse(early.refused);
  if ((seams.statfs ?? (() => scratchFilesystemOf(pin, seams.io)))() !== "ok") return refuse("fs_unsupported");

  // Fail closed: a guard without bindSources, or one that cannot read it, is unknown, never "no binds".
  let binds: ReturnType<NonNullable<PodEpochDaemonGuard["bindSources"]>> = { state: "unknown" };
  try { if (typeof guard.bindSources === "function") binds = guard.bindSources(); } catch { /* unknown */ }
  if (binds.state !== "read") return refuse("bind_unknown");
  const bindInput: Parameters<typeof classifyBindSources>[0] = { sources: binds.sources, root: layout.root };
  if (seams.bindLstat) bindInput.lstat = seams.bindLstat;
  if (seams.bindShared) bindInput.shared = seams.bindShared;
  const bound = classifyBindSources(bindInput);
  if ("refused" in bound) return refuse(bound.refused);

  let storedPaths: string[];
  try {
    const stored = await ctx.owned(() => deps.git.collectEpochScratchProtectedPaths({
      signal: controller.signal, deadline: ctx.deadlineAt() }), ctx.hardPass);
    if (stored.state !== "read") return refuse(stored.state === "malformed" ? "protected_malformed" : "protected_unknown");
    storedPaths = stored.paths;
  } catch (error) {
    if (error instanceof PodEpochSettlementError || ctx.interrupted()) throw error;
    return refuse("protected_unknown");
  }

  const scan = (names: readonly string[]) =>
    (seams.scanProcesses ?? ((n: readonly string[]) => scanScratchProcesses(layout.root, n, seams.io)))(names);
  // The exclusive-idle proof; a throw or false is "not idle". `cleanup` joins it even after an abort.
  const idle = async (cleanup = false): Promise<boolean> => {
    try { return await ctx.owned(() => deps.quiesce(), ctx.hardPass, cleanup); }
    catch (error) {
      if (error instanceof PodEpochSettlementError || (!cleanup && ctx.interrupted())) throw error;
      return false;
    }
  };
  if (!(await idle())) return refuse("not_idle");
  const first = scan([]);
  if ("refused" in first) return refuse(first.refused);

  // ---- census (read-only, as the runner uid) ----
  const budget = ctx.hardPass - 5000 - ctx.now() - SCRATCH_CENSUS_MARGIN_MS;
  if (budget <= 0) return refuse("census_budget");
  const runCensus = seams.census ?? censusScratchRoot;
  // censusScratchRoot joins its child before it rejects (joinOnTimeout), so a rejection here is a joined
  // census failure; an unjoined one surfaces as the owner's PodEpochSettlementError and stays fatal.
  let raw: Awaited<ReturnType<typeof runCensus>>;
  try { raw = await ctx.owned(() => runCensus(layout.root, pin.devIno, undefined, budget), ctx.hardPass); }
  catch (error) {
    if (error instanceof PodEpochSettlementError || ctx.interrupted()) throw error;
    return refuse("census_failed");
  }
  const census = parseScratchCensus(raw, pin.devIno);
  if ("refused" in census) return refuse(census.refused);
  const names = census.entries.map(e => e.name);
  const mounts = readMounts(names);
  if ("refused" in mounts) return refuse(mounts.refused);
  const held = scan(names);
  if ("refused" in held) return refuse(held.refused);
  const classified = classifyScratchCensus({
    census, t0Ms: fence.t0Ms, mountedLeaves: mounts.mountedLeaves, boundLeaves: bound.referenced,
    protectedLeaves: protectedLeaves(storedPaths, layout.root, names), heldLeaves: held.held,
  });
  out.examined = classified.examined;
  Object.assign(out.kept, classified.kept);
  out.eligible = classified.admitted.length;
  out.eligible_allocated_bytes_estimate = classified.eligibleBytes;
  out.shared_bytes = classified.sharedBytes;
  out.list_overflow = census.overflow;
  out.reclaim_bytes_state = "estimated";
  out.state = "ran"; out.reason = "complete";

  const freeBytes = seams.freeBytes ?? (() => poolFreeBytes(pin.fd));
  const freeBefore = freeBytes();
  let retained = 0, launched = 0;
  for (const leaf of classified.admitted) {
    // Soft stop: no new leaf; whatever is in flight was already joined by the previous iteration.
    if (controller.signal.aborted) {
      stopPhase(controller.signal.reason instanceof Refusal ? controller.signal.reason.reason : "pass_aborted");
      break;
    }
    ctx.check(); ctx.entry();
    const fresh = readMounts(names);
    if ("refused" in fresh) { stopPhase(fresh.refused); break; }
    if (fresh.mountedLeaves.has(leaf.name)) { out.kept.mount = (out.kept.mount ?? 0) + 1; continue; }
    if (typeof guard.refreshInventory !== "function") { stopPhase("daemon_changed"); break; }
    try { await ctx.owned(() => guard.refreshInventory!(), ctx.hardPass); }
    catch (error) {
      if (error instanceof PodEpochSettlementError || error instanceof Refusal) throw error;
      // The inventory moved (or could not be read): the whole pass is a daemon veto.
      stopPhase("daemon_changed");
      ctx.veto();
      throw error instanceof PodEpochDaemonError ? error : new PodEpochDaemonError("daemon_changed");
    }
    const remaining = ctx.hardPass - 5000 - ctx.now();
    if (remaining <= 0) { stopPhase("census_budget"); break; }
    let rawAgain: Awaited<ReturnType<typeof runCensus>>;
    try {
      rawAgain = await ctx.owned(() => runCensus(layout.root, pin.devIno, leaf.name, Math.min(SCRATCH_RECENSUS_MAX_MS, remaining)), ctx.hardPass);
    } catch (error) {
      if (error instanceof PodEpochSettlementError || ctx.interrupted()) throw error;
      stopPhase("census_failed"); break;
    }
    const again = parseScratchCensus(rawAgain, pin.devIno);
    if ("refused" in again) { stopPhase(again.refused); break; }
    if (!censusEntryUnchanged(leaf, again.entries.find(e => e.name === leaf.name), again.rootUid)) {
      out.kept.changed = (out.kept.changed ?? 0) + 1; continue;
    }
    if (!(await idle())) { stopPhase("not_idle"); break; }
    const holding = scan([leaf.name]);
    if ("refused" in holding) { stopPhase(holding.refused); break; }
    if (holding.held.has(leaf.name)) { out.kept.held_open = (out.kept.held_open ?? 0) + 1; continue; }
    ctx.check(); ctx.entry();

    let scope: PodEpochReclaimScope;
    try { scope = ctx.openScope(); } catch { stopPhase("scope_refused"); break; }
    if (scope.stopped()) ctx.failFatal();
    // A refusal raised by the pre-launch observation itself happens before any child, ticket or unsettled
    // entry exists: it is the pass's own invalidation (as in the attempt phase), not an unresolved delete.
    let preLaunch: unknown;
    let settlement: Settlement;
    try {
      settlement = await ctx.owned(() => deps.git.discardEpochScratchLeaf({
        scope, root: layout.root as EpochScratchDiscardInput["root"], name: leaf.name, expect: leaf.devIno,
        rootExpect: pin.devIno, bytes: leaf.bytes, signal: controller.signal, deadline: ctx.deadlineAt(),
        beforeLaunch: async () => {
          try {
            ctx.check(); ctx.entry();
            // A slow pre-launch step finishing after the pass went fatal or aborted must not start a delete.
            if (ctx.interrupted()) throw new Error("scratch pass interrupted before launch");
          } catch (error) { preLaunch = error; throw error; }
        },
        rootRepin: () => repinScratchRoot(layout, pin.devIno, seams.io),
        mountsOk: () => {
          const current = readMounts(names);
          return !("refused" in current) && !current.mountedLeaves.has(leaf.name);
        },
        ...(seams.discardSeam ? { testSeam: seams.discardSeam } : {}),
      }), ctx.hardPass, true);
    } catch (error) {
      if (error instanceof PodEpochSettlementError) throw error;
      if (preLaunch !== undefined && error === preLaunch) { stopPhase("observation_refused"); throw error; }
      // discardEpochScratchLeaf tags an error thrown before any unsettled entry or child exists; that is the
      // pass's own invalidation, not an unresolved delete.
      if ((error as { epochScratchNotLaunched?: unknown } | null)?.epochScratchNotLaunched === true) {
        stopPhase("launch_refused"); throw error;
      }
      // An unresolved delete, or any untagged throw (a launch may have happened): EX stays held.
      return ctx.failFatal();
    }
    launched++;
    if (settlement.outcome === "removed") {
      out.deleted++; out.deleted_allocated_bytes_estimate += settlement.bytes;
    } else if (settlement.outcome === "residue") {
      out.residue++; out.deleted_allocated_bytes_estimate += settlement.bytesLowerBound; out.estimate_partial = true;
      if (settlement.kind === "helper_failed") out.helper_failed++;
      if (settlement.stopPhase) { stopPhase(settlement.kind); break; }
      if (++retained >= SCRATCH_RETAINED_STOP) { stopPhase("retained_cap"); break; }
    } else ctx.failFatal();
  }
  // The proof once more after the last join. Everything deleted is already settled, so a failure here only
  // records that the idle interval cannot be vouched for to its end (never fatal). An earlier stop reason is kept.
  if (launched > 0 && !(await idle(true))) { out.not_idle_after = true; if (out.state === "ran") stopPhase("not_idle"); }
  const freeAfter = freeBytes();
  if (freeBefore !== null && freeAfter !== null) {
    const delta = freeAfter - freeBefore;
    out.statfs_free_delta_unattributed = delta >= -MAX_SAFE_BIGINT && delta <= MAX_SAFE_BIGINT ? Number(delta) : null;
  }
}

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
    kept_protected: 0, kept_unrecoverable_shape: 0, reclaim_bytes_state: "unknown", scratch: emptyScratch(),
  };
  // The ONE reclaim scope shared by the scratch and attempt phases, opened lazily by whichever deletes first;
  // settled and finished before native handover.
  let reclaimScope: PodEpochReclaimScope | undefined;
  const openScope = (): PodEpochReclaimScope => reclaimScope ??= ownership.beginReclaim!();
  // The git handles whose process-wide unsettled-delete maps the finish must consult (the same GitCache in production).
  const settleGits = new Set<{ assertNoUnsettledDeletes(): void }>();
  const finishReclaimScope = () => {
    const scope = reclaimScope;
    if (!scope) return;
    try {
      for (const git of settleGits) git.assertNoUnsettledDeletes();
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
            // A descriptor with any byte-incomplete part (a locally short present sibling) yields no
            // candidate for any of its parts, a missing one included.
            else if (part.presence === "missing" && source.bytesState === "measured")
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
              parts: source.sources.length, groupSize: group.paths.length, ledgerState: source.ledgerState });
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

    const deadlineAt = () => Date.now() + Math.max(0, hardPass - 5000 - now());
    // ---- Runner-scratch phase (issue #4043). Runs before the attempt phase, under the same shadow verdict, with
    // the guard open. It never reads bytes_state or partial_causes: the #3851 attempt shortfalls are not its
    // authority. Gate closed (production): nothing below this condition executes. ----
    const scratchDeps = options.scratch;
    if (scratchDeps && (EPOCH_SCRATCH_DELETION_ENABLED || scratchDeps.scratchTestGate === TEST_SCRATCH_GATE) &&
        result.verdict === "eligible" && result.authority.result === "attested" &&
        !invalid && !controller.signal.aborted && typeof ownership.beginReclaim === "function") {
      settleGits.add(scratchDeps.git);
      await runScratchPhase({
        deps: scratchDeps, seams: scratchDeps.scratchTestGate === TEST_SCRATCH_GATE ? scratchDeps.seams ?? {} : {},
        podUid: boot.podUid, guard: guard!, controller, owned, check, entry, now, hardPass, deadlineAt, out: result.scratch,
        openScope,
        interrupted: () => fatal || controller.signal.aborted,
        failFatal: () => { fatal = true; throw new PodEpochSettlementError(); },
        veto: () => { invalidate("daemon_veto"); stop("daemon_veto"); },
      });
    }

    // ---- M1b deletion phase. Shadow counters and the verdict above are final; this only reclaims. ----
    // Runs only when ALL hold: the activation gate (or the test override), a shadow verdict of "eligible" with
    // attested authority, an intact pass (not invalidated, aborted or soft-stopped), a native reclaim scope on
    // the ownership, and the deletion dependencies. Anything less stays shadow-only and mutates nothing.
    const deps = options.deletion;
    if (deps && (deps.activation === true || deps.enableForTest === TEST_ENABLE_DELETION) &&
        result.verdict === "eligible" && result.authority.result === "attested" &&
        !invalid && !controller.signal.aborted && typeof ownership.beginReclaim === "function") {
      // With no admitted candidate nothing can be mutated here, so this phase opens no native scope of its own (the
      // scratch phase may already have opened the shared one); the pass still reports reclaim_complete. Every loop
      // body below runs only when admits is non-empty.
      // An unsettled runner delete already recorded in this process is fatal before this phase opens a scope
      // (defence in depth: no startup step before the pass deletes today except the scratch phase above, which
      // settles each of its deletes before it returns, but any delete recorded before or during the pass counts):
      // EX is kept and no handover happens.
      try { deps.git.assertNoUnsettledDeletes(); } catch { fatal = true; throw new PodEpochSettlementError(); }
      const scope = admits.size > 0 ? openScope() : undefined;
      settleGits.add(deps.git);
      // Pending children or a stop latch (the shared scope's, scratch phase included) are never settled by a
      // later step: fail closed with EX kept.
      const assertSettled = () => {
        const shared = reclaimScope;
        if (shared && (shared.stopped() || shared.pendingMutations > 0)) { fatal = true; throw new PodEpochSettlementError(); }
      };
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
        // Structural keeps are refused before any deletion-phase authority read or proof, and never downgrade the verdict.
        if (admit.protectedDiscard) { result.kept_protected++; continue; }
        if (admit.kind !== "attempt" || admit.parts !== 1 || admit.groupSize !== 1) { result.kept_unrecoverable_shape++; continue; }
        const runId = admit.selector.runId;
        // Journal shape and ledger state are structural keeps too: classified BEFORE any authority read or
        // proof, counted without an authority read, and never downgrading the verdict. Admitted shapes only:
        // (a) ledger live + strict journal, (c) ledger retired + strict journal + missing footprint,
        // (b) ledger retired + `[""]` journal + missing footprint. Anything else (reclaimed, abandoned,
        // a no-key journal) is an unrecoverable shape.
        let raw: string | readonly string[] | undefined;
        try {
          raw = journalShape(await owned(() => deps.git.readEpochJournalVector(admit.barePath, admit.branch),
            hardPass, true), admit.selector, clonePath);
        } catch (error) {
          if (error instanceof PodEpochSettlementError) throw error;
          if (error instanceof PodEpochDaemonError || invalid) throw error;
          refusal(); if (controller.signal.aborted) { skipped = true; break; } continue;
        }
        const missing = candidate.presence === "missing";
        const emptyRepair = Array.isArray(raw) && raw.length === 1 && raw[0] === "";
        const shapeAdmitted = raw !== undefined && (emptyRepair
          ? admit.ledgerState === "retired" && missing
          : admit.ledgerState === "live" || admit.ledgerState === "retired" && missing);
        if (!shapeAdmitted || raw === undefined) { result.kept_unrecoverable_shape++; continue; }
        const admittedJournal: string | readonly string[] = raw;
        let step: { result: RetainedDiscardResult } | "keep" | undefined;
        let effects = false, notEnded = false;
        try {
          // 2. fresh lifecycle and holds with the deletion-side zero rule.
          const first = await owned(() => readAuthority(runId), hardPass, true);
          if (!first) { assertSettled(); refusal(); if (controller.signal.aborted) { skipped = true; break; } continue; }
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
              only.localRecords.length !== 0 || only.holds.state !== "read" || only.holds.ids.length !== 0 || !plain ||
              // A tree-local measurement shortfall no longer makes the re-inspection partial, so a fresh
              // present source that is not byte-measured must be refused here.
              (only.presence === "present" && only.bytesState !== "measured")) {
            assertSettled(); refusal(); continue;
          }
          // 4. the raw journal must still be exactly what the structural pre-check admitted; a change is a
          //    refusal (not a shape keep).
          const again = journalShape(await owned(() => deps.git.readEpochJournalVector(admit.barePath, admit.branch),
            hardPass, true), admit.selector, clonePath);
          if (JSON.stringify(again) !== JSON.stringify(admittedJournal)) { assertSettled(); refusal(); continue; }
          {
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
                paths: [clonePath], confirm: only.descriptor, expectedRawJournal: admittedJournal, checkLive: live,
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
