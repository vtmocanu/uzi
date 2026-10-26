import { constants } from "node:fs";
import { open } from "node:fs/promises";
import { basename } from "node:path";
import { MaintenanceBudget } from "./maintenance-budget.js";
import type { inspectPodEpochBootstrap } from "./pod-epoch-bootstrap.js";
import type { PodEpochOwnership } from "./pod-epoch-lock.js";
import { parsePodEpochEvidence, assertPodEpochEvidenceBinding } from "./pod-epoch-evidence.js";
import { openPodEpochDaemonBarrier, PodEpochDaemonError, type PodEpochDaemonGuard } from "./pod-epoch-daemon.js";
import type { GitCache, RetainedHoldsSnapshot, RetainedLifecycleSnapshot } from "./git.js";
import type { RecoveryHoldsResponse, RunOwnershipResponse } from "./protocol.js";

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
}
interface Verdict {
  verdict: "eligible" | "deferred" | "partial";
  reason: string;
  examined: number;
  would_delete: number;
  kept: number;
  list: Array<{ run_id: string; path: string; bytes: number }>;
  list_overflow: number;
  inventory_overflow: number;
  inventory_overflow_exact: boolean;
  inventory_complete: boolean;
  bytes_state: "measured" | "partial" | "unknown";
  evidence_wait_ms: number;
  pass_ms: number;
  settlement_reserve_ms: number;
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
    verdict: "deferred", reason: "not_evaluated", examined: 0, would_delete: 0, kept: 0,
    list: [], list_overflow: 0, inventory_overflow: 0, inventory_overflow_exact: false,
    inventory_complete: false, bytes_state: "unknown", evidence_wait_ms: 0, pass_ms: 0,
    settlement_reserve_ms: 5000,
  };
  const selected = new Map<string, { run_id: string; path: string; bytes: number }>();
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
    if (boot.firstMode !== "exclusive" || !boot.runtime || !boot.evidenceFile)
      throw new Refusal(boot.reason ?? "bootstrap_uncertain");
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
    try { assertPodEpochEvidenceBinding(document, boot.podUid, boot.runtime); }
    catch { throw new Refusal("evidence_binding"); }
    const evidence = document;
    const entry = () => {
      try {
        if (!ownership.matchesEntry(evidence.successor, evidence.predecessor)) throw new Error();
      } catch { invalid = true; stop("entry_changed"); throw new Refusal("entry_changed"); }
    };
    entry();
    guard = await owned(async () => {
      const opened = await openPodEpochDaemonBarrier({ dockerHost: options.dockerHost ?? "", signal: controller.signal,
        deadline: hardPass - 5000, now, schedule });
      // If opening outlives fatal refusal, only cleanup may continue, with EX retained.
      if (fatal) { await opened.close(); throw new PodEpochSettlementError(); }
      guard = opened;
      return opened;
    }, hardPass);
    const veto = () => { invalid = true; stop("daemon_veto"); };
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
    const censusPaths = new Set<string>();
    const groups = [];
    for (const group of census.groups) {
      const fresh = [...new Set(group.paths)].filter(p => !censusPaths.has(p));
      if (groups.length >= 200 || censusPaths.size + fresh.length > 200) { partial = true; break; }
      for (const p of fresh) censusPaths.add(p);
      groups.push(group);
    }
    for (const group of groups) {
      check();
      for (const path of group.paths) examined.add(path);
      // Refuse before either API call: retained metadata is not an HTTP path authority.
      // Keep every path in this group; continue bounded sibling work without retrying it.
      if (typeof group.selector.runId !== "string" || !RUN_ID_RE.test(group.selector.runId)) {
        partial = true;
        continue;
      }
      let holds: RetainedHoldsSnapshot = { state: "unknown" };
      let lifecycle: RetainedLifecycleSnapshot = { state: "unknown" };
      try {
        holds = holdsSnapshot(await owned(() => MaintenanceBudget.runOwned(controller.signal,
          () => options.client.listRecoveryHolds(group.selector.runId, controller.signal)), hardPass), group.selector.runId);
        const row = await owned(() => MaintenanceBudget.runOwned(controller.signal,
          () => options.client.getRunOwnership(group.selector.runId, {
            signal: controller.signal, deadline: hardPass - 5000,
          })), hardPass);
        if (row && (row.status === "completed" || row.status === "failed" || row.status === "cancelled"))
          lifecycle = { state: "terminal", status: row.status };
      } catch (error) { if (fatal || controller.signal.aborted) throw error; partial = true; }
      const expected = new Set(group.paths);
      const inspected = await owned(() => options.git.inspectRetainedSources({
        epoch: true, maxDescriptors: 200, maxPhysicalPaths: expected.size,
        barePath: group.barePath, selector: group.selector, holds, lifecycle,
        isLivePath: () => false, signal: controller.signal, deadline: hardPass - 5000,
        budgetMs: Math.max(0, hardPass - 5000 - now()), now,
        beforeTree: path => { check(); entry(); if (!expected.has(path)) { invalid = true; stop("path_set_changed"); throw new Refusal("path_set_changed"); } },
      }), hardPass);
      check(); entry();
      const actual = new Set(inspected.sources.flatMap(s => s.sources.map(p => p.clonePath)));
      const drift = actual.size !== expected.size || [...actual].some(p => !expected.has(p));
      if (drift) { invalid = true; throw new Refusal("path_set_changed"); }
      const complete = !inspected.partial && inspected.unreadable === 0;
      partial ||= !complete;
      for (const source of inspected.sources) {
        check(); entry();
        const allowed = complete && lifecycle.state === "terminal" && holds.state === "read" && holds.holds.length === 0 &&
          source.runId === group.selector.runId && source.barePath === group.barePath &&
          source.presence === "present" && source.descriptor.length > 0 && source.localRecords.length === 0 &&
          source.holds.state === "read" && source.holds.ids.length === 0 &&
          (source.disposition?.kind === "local_discard" || source.disposition?.kind === "local_protected_discard");
        for (const part of source.sources) {
          examined.add(part.clonePath);
          if (part.presence !== "unsafe" && part.allocatedBytes !== null &&
              Number.isSafeInteger(part.allocatedBytes) && part.allocatedBytes >= 0) measured.add(part.clonePath);
          if (allowed && part.presence === "present" && !part.live && source.bytesState === "measured" &&
              part.allocatedBytes !== null && Number.isSafeInteger(part.allocatedBytes) && part.allocatedBytes >= 0)
            selected.set(part.clonePath, { run_id: safe(source.runId), path: safe(basename(part.clonePath)), bytes: part.allocatedBytes });
        }
      }
    }
    check(); entry();
    if (partial) result.inventory_complete = false;
    result.verdict = partial ? "partial" : "eligible";
    result.reason = partial ? "inventory_partial" : "shadow_complete";
    result.bytes_state = measured.size === examined.size && !partial ? "measured" :
      measured.size ? "partial" : "unknown";
  } catch (error) {
    if (error instanceof PodEpochSettlementError) { fatal = true; throw error; }
    result.verdict = controller.signal.aborted ? "partial" : "deferred";
    const cause = controller.signal.reason;
    result.reason = error instanceof PodEpochDaemonError ? error.reason :
      cause instanceof Refusal ? cause.reason : error instanceof Refusal ? error.reason : "read_failed";
    result.inventory_complete = false;
    result.inventory_overflow_exact = false;
    result.bytes_state = examined.size ? "partial" : "unknown";
  } finally {
    try {
      if (!fatal && guard) {
        // Assert once more before detaching intentional daemon_closed notification.
        try { guard.assertHealthy(); }
        catch {
          invalid = true;
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
  if (invalid) selected.clear();
  result.examined = examined.size;
  result.would_delete = selected.size;
  result.kept = Math.max(0, examined.size - selected.size);
  result.list = [...selected.values()].slice(0, 32);
  result.list_overflow = Math.max(0, selected.size - result.list.length);
  return result;
}
