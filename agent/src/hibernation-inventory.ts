// PRD #4208 M1, worker side: the volume inventory taken while a hosted worker prepares to hibernate.
//
// The api puts a `hibernation_inventory` member in the heartbeat response; this worker quiesces its
// own data-volume writers, upgrades the pod-epoch lock to exclusive, takes a census and reports it
// (with the identity echo the api binds) under writer exclusion held until the pod terminates.
// Only the DinD lane participates (the capability `hibernation_inventory_v1` is advertised nowhere
// else); every other lane has no proven exclusion mechanism and keeps its volumes.
//
// Exclusion = epoch lock EX + writer latch + quiescence:
//   - the claim gate is closed under the `hibernation` owner (no new run/chat/cross-check claim);
//   - `HibernationWriterRegistry.latch()` makes every registered background writer refuse to start,
//     and `settle()` waits for those already inside;
//   - the existing trackers are used as-is: DindPruneGate.inFlightClaims(), Worker.isIdle();
//   - terminalMaintenanceProof(3, dockerHost) proves no other process or running container writes;
//   - PodEpochOwnership.upgradeExclusive() takes EX so a reclaiming peer cannot overlap.
//
// ENUMERATED DATA-VOLUME WRITER CLASSES (registry class name -> registration site, covered by):
//   claim gate + in-flight trackers (no registry wrap: nothing runs once the gate is closed and
//   Worker.isIdle() is true):
//     run lane claim loop        worker.ts Worker.claimLoop (run lane)     gate "hibernation" + runActive
//     cross-check lane           worker.ts Worker.claimLoop (crossCheck)   gate "hibernation" + crossCheckActive
//     chat lane                  worker.ts Worker.chatClaimLoop            gate "hibernation" + chatActive
//     judge / review / job /     claimed through the run lane above        runActive (isIdle)
//       isolated / ci_fix runs
//     checkpoint ticks, publication, recovery capture, fetch-back, run HOME/outbox spill of a live
//       run                      inside runner.execute                     runActive (isIdle)
//     terminal_maintenance       maintenance-exclusion.ts run()            refuses while gate.claimsClosed(); held
//                                                                          owner is checked in the idle probe
//   registry classes (HibernationWriterRegistry.enter at the start of each pass):
//     outbox_drain               worker.ts Worker.drainOutbox (boot drain + every heartbeat)
//     terminal_sweep             worker.ts heartbeatLoop sweepPendingTerminals
//     finalize_sweep             worker.ts heartbeatLoop sweepPendingFinalizes
//     hold_settled_retry         worker.ts heartbeatLoop runner.retryPendingHoldSettled
//     recovery_redrive           worker.ts heartbeatLoop runner.resumeLiveRecoveries
//     recovery_resume            worker.ts runLifecycle runner.resumePendingRecoveries (waits for an unlatch)
//     recovery_pass              recovery.ts Recovery.trackPass: every boot/live pass, resumeTerminalSource and
//                                resumeTerminalEvidence (post-run capture records and bundles; outlive runActive).
//                                Refused while latched; a refused pass is retried by the heartbeat re-drive.
//     live_settle                runner.ts triggerLiveSettle detached observe+settleLive (journal.put; outlives
//                                runActive). Refused while latched; the settlement sweep retries.
//     boot_terminals             worker.ts runLifecycle resolveBootTerminals + cleanupBootCodeSnapshots
//     settlement_sweep           worker.ts Worker.settlementLoop (publication-resume custodian: ancestry settlement)
//     terminal_rejections        terminal-rejections.ts dispose (outbox record deletion)
//     disk_reclaim               disk-reclaim.ts DiskPressureController.requestReclaim
//     dind_prune                 dind-prune.ts DindPruneController.cacheLoop
//     dind_volume_prune          dind-volume-prune.ts DindVolumePruneController.loop
//     dind_maintenance           dind-maintenance.ts DindMaintenanceController.loop
//     retained_ops               ops/retained-source-server.ts resolveSequence + discardSequence
//   startup-only passes that complete before the worker registers, so before any heartbeat can carry
//   an inventory member (no wrap needed): pod-epoch reclaim and scratch (runPodEpochShadow), HOME
//   reclaim, stale job workspaces, retire-scratch inspection, the Codex orphan reaper.
//   read-only samplers (no writes): stats collector, RunDiskSampler, memory guard sampling,
//   retained-source reportRetained.
//   UNCOVERED: none known. A class that cannot be wrapped must call
//   HibernationWriterRegistry.markUncovered(name); the inventory then reports it, this worker
//   posts it with complete=false, and the api also refuses to treat such an inventory as complete.
import type { DindPruneGate } from "./dind-prune.js";
import type { Logger } from "./log.js";
import type { PodEpochOwnership } from "./pod-epoch-lock.js";
import { errMessage } from "./util.js";

// ---- wire types (fixtures/hibernation-inventory/wire.json) ------------------------------------

export type HibernationPvcRole = "data" | "nix" | "dind_data";

export interface HibernationPvc {
  role: HibernationPvcRole;
  name: string;
  uid: string;
}

/** The heartbeat response member `hibernation_inventory`. */
export interface HibernationInventoryMember {
  op_id: string;
  phase: "awaiting" | "accepted";
  register_nonce: string;
  pod_uid: string;
  deployment_resource_version: string;
  pvcs: HibernationPvc[];
}

/** The heartbeat request member `hibernation_exclusion`. */
export interface HibernationExclusionReport {
  op_id: string;
  held: boolean;
}

export interface InventoryEntry {
  kind: string;
  path: string;
  classification: string;
  run_id?: string;
  tip?: string;
  content_state?: string;
  detail?: string;
}

export interface DockerInventory {
  containers: { id: string; state: string; name: string }[];
  volumes: { name: string; anonymous: boolean }[];
  images_count: number;
  build_cache_bytes: number;
}

interface IdentityEcho {
  op_id: string;
  register_nonce: string;
  pod_uid: string;
}

export interface InventoryRequest extends IdentityEcho {
  deployment_resource_version: string;
  pvcs: HibernationPvc[];
  complete: boolean;
  entries: InventoryEntry[];
  fingerprint: string;
  digest: string;
  docker: DockerInventory | null;
  exclusion: { lane: "dind"; mechanism: "epoch_ex+latch+quiescence"; uncovered: string[] };
}

export interface InventoryStatusRequest extends IdentityEcho {
  status: "unsupported" | "exclusion_failed" | "busy";
  reason: string;
}

export type HibernationInventoryBody = InventoryRequest | InventoryStatusRequest;

export interface ExclusionFinalRequest extends IdentityEcho {
  held: true;
  digest: string;
  fingerprint: string;
}

// ---- census interface (U4b implements it) ------------------------------------------------------

export interface CensusResult {
  complete: boolean;
  entries: InventoryEntry[];
  fingerprint: string;
  digest: string;
  docker: DockerInventory | null;
  uncovered: string[];
}
export type HibernationCensus = (opts: { signal: AbortSignal; deadlineMs: number }) => Promise<CensusResult>;
export type HibernationFingerprint = (opts: { signal?: AbortSignal; deadlineMs: number }) =>
  Promise<{ digest: string; fingerprint: string }>;

/** Placeholder census until the volume census lands: it can never report a complete inventory. */
export const notImplementedCensus: HibernationCensus = async () => ({
  complete: false, entries: [], fingerprint: "", digest: "", docker: null, uncovered: ["census_not_implemented"],
});
export const notImplementedFingerprint: HibernationFingerprint = async () => ({ digest: "", fingerprint: "" });

// ---- decoding -----------------------------------------------------------------------------------

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ROLES = new Set<string>(["data", "nix", "dind_data"]);
const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const uuid = (v: unknown): v is string => typeof v === "string" && UUID.test(v);
/** The api's register nonce is a 43-character base64url token, not a UUID; accept any bounded token. */
const token = (v: unknown): v is string => typeof v === "string" && /^[A-Za-z0-9_-]{1,128}$/.test(v);
const bounded = (v: unknown, max = 253): v is string => typeof v === "string" && v.length > 0 && v.length <= max;

/** null = explicitly none (or absent); undefined = malformed, which the caller ignores. */
export function decodeHibernationInventory(value: unknown): HibernationInventoryMember | null | undefined {
  if (value === null || value === undefined) return null;
  if (!isRecord(value)) return undefined;
  if (!uuid(value.op_id) || !token(value.register_nonce) || !uuid(value.pod_uid) ||
      !bounded(value.deployment_resource_version, 64)) return undefined;
  if (value.phase !== "awaiting" && value.phase !== "accepted") return undefined;
  const raw = value.pvcs;
  if (!Array.isArray(raw) || raw.length === 0 || raw.length > ROLES.size) return undefined;
  const pvcs: HibernationPvc[] = [];
  const seen = new Set<string>();
  for (const item of raw) {
    if (!isRecord(item) || typeof item.role !== "string" || !ROLES.has(item.role) || seen.has(item.role) ||
        !bounded(item.name) || !uuid(item.uid)) return undefined;
    seen.add(item.role);
    pvcs.push({ role: item.role as HibernationPvcRole, name: item.name, uid: item.uid });
  }
  return {
    op_id: value.op_id, phase: value.phase, register_nonce: value.register_nonce, pod_uid: value.pod_uid,
    deployment_resource_version: value.deployment_resource_version, pvcs,
  };
}

// ---- writer registry ----------------------------------------------------------------------------

export type WriterRelease = () => void;

export class HibernationBusyError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HibernationBusyError";
  }
}

function pause(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) { resolve(); return; }
    const done = () => { clearTimeout(timer); signal?.removeEventListener("abort", done); resolve(); };
    const timer = setTimeout(done, ms);
    signal?.addEventListener("abort", done, { once: true });
  });
}

/**
 * Process-wide count of background data-volume writers. `enter` refuses (null) while latched, so a
 * refused periodic task simply skips its tick; `settle` waits for the writers already inside.
 */
export class HibernationWriterRegistry {
  private latched = false;
  private readonly counts = new Map<string, number>();
  private readonly uncoveredNames = new Set<string>();

  /** A release function, or null while latched. The release is idempotent. */
  enter(cls: string): WriterRelease | null {
    if (this.latched) return null;
    this.counts.set(cls, (this.counts.get(cls) ?? 0) + 1);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const left = (this.counts.get(cls) ?? 1) - 1;
      if (left <= 0) this.counts.delete(cls);
      else this.counts.set(cls, left);
    };
  }

  latch(): void { this.latched = true; }
  unlatch(): void { this.latched = false; }
  isLatched(): boolean { return this.latched; }
  inFlight(): number {
    let n = 0;
    for (const c of this.counts.values()) n += c;
    return n;
  }
  busyClasses(): string[] { return [...this.counts.keys()].sort(); }

  /** Record a writer class this process cannot wrap; the inventory reports it. */
  markUncovered(name: string): void { this.uncoveredNames.add(name); }
  uncovered(): string[] { return [...this.uncoveredNames].sort(); }

  /**
   * Resolve once every class count is zero and `extraIdle` (the existing trackers) holds; reject
   * with HibernationBusyError after `timeoutMs`. Call after `latch()`, so a writer entering during
   * the wait is refused and the count only falls.
   */
  async settle(timeoutMs: number, signal?: AbortSignal, extraIdle: () => boolean = () => true): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      if (this.inFlight() === 0 && extraIdle()) return;
      if (signal?.aborted) throw new HibernationBusyError("aborted while waiting for writers");
      const left = deadline - Date.now();
      if (left <= 0) {
        const classes = this.busyClasses();
        throw new HibernationBusyError(classes.length > 0 ? `writers still active: ${classes.join(",")}` : "worker is not idle");
      }
      await pause(Math.min(25, left), signal);
    }
  }
}

/** The process-wide registry every wrapped writer consults. */
export const hibernationWriters = new HibernationWriterRegistry();

/** Run `work` as writer class `cls`; `{ ran: false }` when the registry is latched. */
export async function asWriter<T>(
  cls: string, work: () => Promise<T>, registry: HibernationWriterRegistry = hibernationWriters,
): Promise<{ ran: true; value: T } | { ran: false }> {
  const release = registry.enter(cls);
  if (!release) return { ran: false };
  try { return { ran: true, value: await work() }; } finally { release(); }
}

/** Like {@link asWriter} but waits for an unlatch instead of skipping (once-only boot passes). */
export async function asWriterWhenOpen<T>(
  cls: string, work: () => Promise<T>, signal: AbortSignal, registry: HibernationWriterRegistry = hibernationWriters,
): Promise<{ ran: true; value: T } | { ran: false }> {
  for (;;) {
    const result = await asWriter(cls, work, registry);
    if (result.ran || signal.aborted) return result;
    await pause(200, signal);
  }
}

// ---- controller ---------------------------------------------------------------------------------

const MAX_ENTRIES = 10_000;
const MAX_BODY_BYTES = 1024 * 1024;
const SETTLE_MS = 60_000;
const CENSUS_MS = 120_000;
const FINGERPRINT_MS = 10_000;
const FINAL_POST_MS = 5_000;
const RETRY_AFTER_MS = 10_000;
const REASON_MAX = 200;

export interface HibernationClientPort {
  hasFeature(feature: string): boolean;
  /** The current registration's nonce, or undefined before register. */
  currentRegisterNonce(): string | undefined;
  postHibernationInventory(body: HibernationInventoryBody, signal?: AbortSignal): Promise<void>;
  postHibernationExclusionFinal(body: ExclusionFinalRequest, opts?: { timeoutMs?: number; signal?: AbortSignal }): Promise<void>;
}

export interface HibernationDeps {
  registry: HibernationWriterRegistry;
  gate: DindPruneGate;
  client: HibernationClientPort;
  ownership: Pick<PodEpochOwnership, "mode" | "reacquireShared" | "upgradeExclusive"> | undefined;
  /** True only on the DinD lane with epoch participation. */
  laneSupported: boolean;
  podUid: () => string | undefined;
  /** Worker.isIdle. */
  isIdle: () => boolean;
  /** terminalMaintenanceProof(3, dockerHost). */
  proof: () => Promise<boolean>;
  census: HibernationCensus;
  recomputeFingerprint: HibernationFingerprint;
  /** Exit the process non-zero (the main.ts fatal path). Must not return in production. */
  fatalExit: (reason: string) => void;
  log: Logger;
  now?: () => number;
  settleMs?: number;
  retryAfterMs?: number;
}

type Status = InventoryStatusRequest["status"];

class StatusFailure extends Error {
  constructor(readonly status: Status, message: string) {
    super(message);
  }
}

const bound = (text: string): string => (text.length > REASON_MAX ? text.slice(0, REASON_MAX) : text);

export class HibernationInventoryController {
  private readonly now: () => number;
  private readonly abort = new AbortController();
  private latest: HibernationInventoryMember | null | undefined;
  private pump: Promise<void> | undefined;
  /** The op whose latch and gate this controller currently holds (from step (a)). */
  private held?: { opId: string; ex: boolean; accepted: boolean; nonce: string; podUid: string };
  private failure?: { opId: string; at: number };
  private fatal = false;
  /** Shutdown began: the exclusion is held until the process exits and is never released. */
  private stopping = false;
  /** An op whose exclusion was dropped after a post-EX failure: `held:false` is sent once. */
  private pendingRelease?: string;

  constructor(private readonly deps: HibernationDeps) {
    this.now = deps.now ?? Date.now;
  }

  get registry(): HibernationWriterRegistry { return this.deps.registry; }

  /** Feed the heartbeat response member: null = none/absent, undefined = unknown (ignored). */
  observe(member: HibernationInventoryMember | null | undefined): void {
    if (member === undefined || this.fatal) return;
    this.latest = member;
    if (this.pump) return;
    this.pump = this.drain().finally(() => { this.pump = undefined; });
  }

  /** Resolves when no member is being applied. */
  async whenIdle(): Promise<void> {
    while (this.pump) await this.pump;
  }

  /** The `hibernation_exclusion` heartbeat member, or undefined when there is nothing to report. */
  exclusionReport(): HibernationExclusionReport | undefined {
    const h = this.held;
    if (!h?.ex) {
      const opId = this.pendingRelease;
      if (opId === undefined) return undefined;
      this.pendingRelease = undefined;
      return { op_id: opId, held: false };
    }
    return { op_id: h.opId, held: this.intact() };
  }

  private ownershipMode(): string | undefined {
    try { return this.deps.ownership?.mode; } catch { return undefined; }
  }

  private intact(): boolean {
    return this.held !== undefined && this.held.ex && this.ownershipMode() === "exclusive" &&
      this.deps.registry.isLatched() && this.deps.gate.heldBy("hibernation");
  }

  private async drain(): Promise<void> {
    while (this.latest !== undefined && !this.fatal) {
      const member = this.latest;
      this.latest = undefined;
      try {
        await this.apply(member);
      } catch (err) {
        this.deps.log.warn("hibernation inventory: member handling failed", { error: errMessage(err) });
      }
    }
  }

  private async apply(member: HibernationInventoryMember | null): Promise<void> {
    if (this.stopping && this.held && (member === null || member.op_id !== this.held.opId)) return;
    if (this.held && (member === null || member.op_id !== this.held.opId)) await this.release();
    if (this.fatal || member === null) return;
    if (this.held) {
      this.held.accepted = member.phase === "accepted";
      return;
    }
    if (member.phase !== "awaiting") return;
    if (!this.deps.client.hasFeature("hibernation_inventory_v1")) return;
    const podUid = this.deps.podUid();
    if (!podUid || member.pod_uid !== podUid || member.register_nonce !== this.deps.client.currentRegisterNonce()) return;
    if (this.failure && this.failure.opId === member.op_id &&
        this.now() - this.failure.at < (this.deps.retryAfterMs ?? RETRY_AFTER_MS)) return;
    if (this.stopping) return;
    await this.start(member);
  }

  private echo(m: HibernationInventoryMember): IdentityEcho {
    return { op_id: m.op_id, register_nonce: m.register_nonce, pod_uid: m.pod_uid };
  }

  private async postStatus(m: HibernationInventoryMember, status: Status, reason: string): Promise<void> {
    try {
      await this.deps.client.postHibernationInventory({ ...this.echo(m), status, reason: bound(reason) }, this.abort.signal);
    } catch (err) {
      this.deps.log.warn("hibernation inventory: status report failed", { status, error: errMessage(err) });
    }
  }

  private async start(m: HibernationInventoryMember): Promise<void> {
    const d = this.deps;
    if (!d.laneSupported) {
      this.failure = { opId: m.op_id, at: this.now() };
      await this.postStatus(m, "unsupported", "lane has no proven exclusion mechanism");
      return;
    }
    // (a) latch writers and close the claim gate.
    this.held = { opId: m.op_id, ex: false, accepted: false, nonce: m.register_nonce, podUid: m.pod_uid };
    d.registry.latch();
    d.gate.close("hibernation");
    try {
      // (b) settle in-flight writers, bounded.
      const quiet = () => d.isIdle() && d.gate.inFlightClaims() === 0 &&
        !d.gate.heldBy("terminal_maintenance") && !d.gate.heldBy("retained_ops");
      try {
        await d.registry.settle(d.settleMs ?? SETTLE_MS, this.abort.signal, quiet);
      } catch (err) {
        throw new StatusFailure("busy", errMessage(err));
      }
      // (c) the deny-only process and container proof.
      if (!await d.proof()) throw new StatusFailure("busy", "writer exclusion proof failed");
      // (d) exclusive epoch ownership.
      const upgrade = d.ownership?.upgradeExclusive;
      if (!d.ownership || typeof upgrade !== "function") {
        throw new StatusFailure("exclusion_failed", "epoch exclusive ownership is unavailable");
      }
      let got = false;
      try {
        got = await upgrade.call(d.ownership);
      } catch (err) {
        if (this.ownershipMode() === undefined) {
          this.fatal = true;
          d.fatalExit("hibernation_shared_ownership_lost");
        }
        throw new StatusFailure("exclusion_failed", errMessage(err));
      }
      if (!got) throw new StatusFailure("exclusion_failed", "could not acquire the pod epoch lock exclusively");
      this.held.ex = true;
      // (e) census.
      const census = await d.census({ signal: this.abort.signal, deadlineMs: CENSUS_MS });
      // (f) report with the identity echo.
      await d.client.postHibernationInventory(this.buildInventory(m, census), this.abort.signal);
    } catch (err) {
      // Shutdown aborts the census and posts: hold the exclusion until exit, release nothing.
      if (this.fatal || this.stopping) return;
      this.failure = { opId: m.op_id, at: this.now() };
      const status: Status = err instanceof StatusFailure ? err.status : "busy";
      const reason = err instanceof StatusFailure ? err.message : `inventory failed: ${errMessage(err)}`;
      const hadExclusion = this.held?.ex === true;
      await this.release();
      if (hadExclusion && !this.fatal) this.pendingRelease = m.op_id;
      await this.postStatus(m, status, reason);
    }
  }

  private buildInventory(m: HibernationInventoryMember, census: CensusResult): InventoryRequest {
    const uncovered = [...this.deps.registry.uncovered(), ...census.uncovered];
    // An uncovered writer class means the inventory cannot be proven complete (the api enforces it too).
    let complete = census.complete && uncovered.length === 0;
    let entries = census.entries;
    let docker = census.docker;
    if (entries.length > MAX_ENTRIES) { entries = entries.slice(0, MAX_ENTRIES); complete = false; }
    const build = (): InventoryRequest => ({
      ...this.echo(m),
      deployment_resource_version: m.deployment_resource_version,
      pvcs: m.pvcs.map((p) => ({ role: p.role, name: p.name, uid: p.uid })),
      complete, entries, fingerprint: census.fingerprint, digest: census.digest, docker,
      exclusion: {
        lane: "dind", mechanism: "epoch_ex+latch+quiescence",
        uncovered,
      },
    });
    let body = build();
    while (Buffer.byteLength(JSON.stringify(body)) > MAX_BODY_BYTES) {
      complete = false;
      if (entries.length > 1) entries = entries.slice(0, entries.length >> 1);
      else if (entries.length === 1) entries = [];
      else docker = null;
      body = build();
      if (entries.length === 0 && docker === null) break;
    }
    return body;
  }

  /** Reacquire SH, then reopen the gate and unlatch. A failed SH re-take keeps the gate closed and exits. */
  private async release(): Promise<void> {
    const h = this.held;
    if (!h || this.stopping) return;
    const d = this.deps;
    if (h.ex) {
      try {
        await d.ownership!.reacquireShared();
      } catch (err) {
        this.fatal = true;
        d.log.error("hibernation inventory: could not re-take shared epoch ownership; exiting", { error: errMessage(err) });
        d.fatalExit("hibernation_reacquire_shared_failed");
        return;
      }
    }
    d.gate.open("hibernation");
    d.registry.unlatch();
    this.held = undefined;
  }

  /**
   * Shutdown hook, called after every other shutdown writer has finished (main.ts, after
   * finishShutdown): recompute the fingerprint and report exclusion-final. Sends nothing on any
   * failure or timeout, so the api reports the exclusion unproven.
   */
  async finalizeAtShutdown(): Promise<void> {
    const h = this.held;
    if (!h || !h.accepted || !this.intact()) return;
    const d = this.deps;
    try {
      const limit = AbortSignal.timeout(FINGERPRINT_MS + 1000);
      let timer: NodeJS.Timeout | undefined;
      const value = await Promise.race([
        d.recomputeFingerprint({ signal: limit, deadlineMs: FINGERPRINT_MS }),
        new Promise<undefined>((resolve) => { timer = setTimeout(() => resolve(undefined), FINGERPRINT_MS); }),
      ]).finally(() => clearTimeout(timer));
      if (!value || !this.intact()) return;
      await d.client.postHibernationExclusionFinal({
        op_id: h.opId, pod_uid: h.podUid, register_nonce: h.nonce, held: true,
        digest: value.digest, fingerprint: value.fingerprint,
      }, { timeoutMs: FINAL_POST_MS });
    } catch (err) {
      d.log.warn("hibernation inventory: exclusion-final not sent", { error: errMessage(err) });
    }
  }

  /** Cancel in-flight census and posts (shutdown). From here nothing releases the exclusion: it is held until exit. */
  stop(): void { this.stopping = true; this.abort.abort(); }
}
