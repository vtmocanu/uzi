// Server-fenced DinD maintenance (#1760). No local pressure or retirement trigger.
import { readDindMeterSample, type DindMeterSample } from "./dind-meter.js";
import { DOCKER_BIN, DindPruneGate, dockerExecEnvironment, execDocker, type DockerExec } from "./dind-prune.js";
import type { DindMaintenance, DindMaintenanceReadyACK } from "./protocol.js";
import type { Logger } from "./log.js";
import { errMessage, sleep as abortableSleep } from "./util.js";

const COMMANDS = Object.freeze({
  version: Object.freeze(["version", "--format", "{{.Server.APIVersion}}"]),
  prune: Object.freeze(["volume", "prune", "-f"]),
});
const PHASES = new Set(["requested", "ready", "stopping", "recycling", "complete", "cancelled"]);
const TOTAL_BUDGET_MS = 7 * 60_000;

interface MaintenanceOptions {
  gate: DindPruneGate;
  dockerHost: string;
  heartbeatIntervalMs: number;
  isIdle: () => boolean;
  log: Logger;
  /** API-configured fraction (0, 1]; undefined means unknown. */
  threshold: () => number | undefined;
  readSample?: () => DindMeterSample | null;
  exec?: DockerExec;
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

function valid(op: DindMaintenance): boolean {
  return [op.id, op.nonce, op.deployment_uid, op.pvc_uid, op.register_nonce]
    .every((v) => typeof v === "string" && v.length > 0) &&
    PHASES.has(op.phase) && typeof op.fenced === "boolean" && typeof op.ready_ack === "boolean";
}

function identity(op: DindMaintenance): string {
  return JSON.stringify([op.id, op.nonce, op.deployment_uid, op.pvc_uid, op.register_nonce]);
}

/** Construct for a worker with Docker wiring. The API authorizes maintenance operations
 * only for eligible hosted persistent workers; capability reporting alone cannot authorize one. */
export class DindMaintenanceController {
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly exec: DockerExec;
  private readonly readSample: () => DindMeterSample | null;
  private nonce?: string;
  private enabled = false;
  private registeredAt = 0;
  private lastBusyAt = 0;
  private custody?: { retaining: boolean | undefined; sentAt: number };
  private operation?: DindMaintenance;
  private heldIdentity?: string;
  private disposition?: { identity: string; pruned: boolean; afterMs?: number; afterEpoch?: number };
  private running?: AbortController;
  private revision = 0;
  private releasePending = false;
  private stopping = false;

  constructor(private readonly deps: MaintenanceOptions) {
    this.now = deps.now ?? Date.now;
    this.sleep = deps.sleep ?? abortableSleep;
    this.exec = deps.exec ?? execDocker;
    this.readSample = deps.readSample ?? (() => readDindMeterSample());
  }

  get gate(): DindPruneGate { return this.deps.gate; }

  register(nonce: string | undefined, enabled: boolean): void {
    this.invalidate();
    this.nonce = nonce;
    this.enabled = enabled && !!nonce && !!this.deps.dockerHost;
    this.registeredAt = this.now();
    this.lastBusyAt = this.registeredAt;
    this.custody = undefined;
    this.operation = undefined;
    this.disposition = undefined;
    // A registration cannot release a previously observed server fence.
  }

  observe(operation: DindMaintenance | null | undefined, retaining: boolean | undefined, sentAtMs: number): void {
    this.custody = { retaining, sentAt: sentAtMs };
    const op = operation && valid(operation) ? { ...operation } : undefined;
    const changed = !op || !this.operation || identity(op) !== identity(this.operation) ||
      op.phase !== this.operation.phase || op.fenced !== this.operation.fenced;
    if (changed) this.invalidate();
    this.operation = op;
    if (!op) return; // Unknown, malformed and absent cannot release a held fence.

    const key = identity(op);
    if ((op.phase === "complete" || (op.phase === "cancelled" && !this.stopping)) && key === this.heldIdentity) {
      this.releasePending = true;
      this.releaseIfSettled();
      return;
    }
    if (op.phase === "ready" || op.phase === "stopping" || op.phase === "recycling" ||
      (op.phase === "requested" && op.fenced)) {
      if (!this.deps.gate.heldBy("maintenance") || this.heldIdentity !== key) this.lastBusyAt = this.now();
      this.deps.gate.close("maintenance");
      this.heldIdentity = key;
      if (op.phase === "stopping" || op.phase === "recycling") this.stopping = true;
      this.releasePending = false;
    }
    if (retaining !== false) this.running?.abort();
  }

  noteActivityEnded(): void {
    this.lastBusyAt = this.now();
    this.running?.abort();
  }

  private invalidate(): void {
    this.revision++;
    this.running?.abort();
  }

  private releaseIfSettled(): void {
    if (!this.releasePending || this.running) return;
    this.deps.gate.open("maintenance");
    this.releasePending = false;
    this.heldIdentity = undefined;
    this.stopping = false;
    this.disposition = undefined;
  }

  private safe(): boolean {
    if (!this.deps.isIdle() || this.deps.gate.inFlightClaims() > 0) {
      this.lastBusyAt = this.now();
      return false;
    }
    const op = this.operation;
    const c = this.custody;
    const age = c ? this.now() - c.sentAt : Infinity;
    return this.enabled && !this.stopping && !!op && op.phase === "ready" && op.fenced &&
      op.register_nonce === this.nonce && this.deps.gate.heldBy("maintenance") &&
      this.heldIdentity === identity(op) && this.deps.gate.inFlightClaims() === 0 &&
      !!c && c.retaining === false && age >= 0 && age <= 2 * this.deps.heartbeatIntervalMs &&
      c.sentAt > this.lastBusyAt;
  }

  private sample(): DindMeterSample | null {
    const s = this.readSample();
    if (!s || !Object.values(s).every(Number.isSafeInteger) || s.epochS < 0 ||
      s.bytesTotal <= 0 || s.inodesTotal <= 0 || s.bytesUsed < 0 || s.inodesUsed < 0 ||
      s.bytesUsed > s.bytesTotal || s.inodesUsed > s.inodesTotal) return null;
    const stamp = s.epochS * 1000;
    return stamp >= this.registeredAt && stamp <= this.now() && this.now() - stamp <= 45_000 ? s : null;
  }

  /** Getter revalidates current custody, binding and sample; never waits for Docker. */
  acknowledgement(): DindMaintenanceReadyACK | undefined {
    if (this.running || !this.safe() || !this.operation ||
      this.disposition?.identity !== identity(this.operation)) return undefined;
    const d = this.disposition;
    const s = this.sample();
    const newer = s && d.afterMs !== undefined && s.epochS * 1000 > d.afterMs &&
      s.epochS > (d.afterEpoch ?? 0);
    const threshold = this.deps.threshold();
    const below = d.pruned && newer && threshold !== undefined && Number.isFinite(threshold) &&
      threshold > 0 && threshold <= 1 &&
      s.bytesUsed / s.bytesTotal < threshold && s.inodesUsed / s.inodesTotal < threshold;
    return {
      ...this.operation,
      ...(below ? { phase: "cancelled", reason: "below_threshold" } : {}),
      local_claims: 0, local_executions: 0, custody_clear: true,
      custody_checked_at: new Date(this.custody!.sentAt).toISOString(),
      pruned: d.pruned, resampled_epoch: newer ? s.epochS : 0,
    };
  }

  /** One optional disposition per bound identity. Cache state is irrelevant except
   * its held attempt: serialize commands by waiting for that owner to finish. */
  async tick(signal?: AbortSignal): Promise<void> {
    if (signal?.aborted || this.running || !this.safe() || this.deps.gate.heldBy("cache")) return;
    const op = this.operation!;
    const key = identity(op);
    if (this.disposition?.identity === key) return;
    const controller = new AbortController();
    this.running = controller;
    const abort = () => controller.abort();
    signal?.addEventListener("abort", abort, { once: true });
    const revision = this.revision;
    const current = () => !controller.signal.aborted && revision === this.revision && this.safe();
    const deadline = this.now() + TOTAL_BUDGET_MS;
    const result = { identity: key, pruned: false, afterMs: undefined as number | undefined,
      afterEpoch: undefined as number | undefined };
    // Mark before the first await: ACK retries cannot repeat even a failed command.
    this.disposition = result;
    try {
      const version = (await this.docker(COMMANDS.version, Math.min(30_000, deadline - this.now()), controller.signal)).trim();
      if (!current()) return;
      const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.exec(version);
      if (!match || !Number.isSafeInteger(Number(match[1])) || !Number.isSafeInteger(Number(match[2])) ||
        !(Number(match[1]) > 1 || (Number(match[1]) === 1 && Number(match[2]) >= 42))) return;
      result.afterEpoch = this.sample()?.epochS;
      await this.docker(COMMANDS.prune, Math.min(5 * 60_000, deadline - this.now()), controller.signal);
      result.pruned = true;
      result.afterMs = this.now();
      if (!current()) return;
      const until = Math.min(this.now() + 60_000, deadline);
      // At most 60 one-second waits; missing/failed samples do not block ready ACK.
      while (current()) {
        const s = this.sample();
        if (s && s.epochS * 1000 > result.afterMs && s.epochS > (result.afterEpoch ?? 0)) break;
        const left = until - this.now();
        if (left <= 0) break;
        await this.sleep(Math.min(1000, left), controller.signal);
      }
    } catch (err) {
      this.deps.log.debug("dind-maintenance: optional prune skipped or interrupted", { error: errMessage(err) });
    } finally {
      signal?.removeEventListener("abort", abort);
      this.running = undefined;
      this.releaseIfSettled();
    }
  }

  private docker(argv: readonly string[], timeoutMs: number, signal: AbortSignal): Promise<string> {
    if (!Object.values(COMMANDS).some((a) => a.length === argv.length && a.every((v, i) => v === argv[i]))) {
      throw new Error("dind-maintenance: argv outside frozen allowlist");
    }
    if (signal.aborted || timeoutMs <= 0) throw new Error("dind-maintenance: aborted or deadline exhausted");
    return this.exec(argv, { file: DOCKER_BIN, env: dockerExecEnvironment(this.deps.dockerHost), timeoutMs, signal });
  }

  /** Serial ticks; each failure affects only this controller. One-second cadence,
   * command and sample waits are bounded by tick's total budget. */
  async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      try {
        await this.tick(signal);
        await this.sleep(1000, signal);
      } catch (err) {
        if (signal.aborted) return;
        this.deps.log.warn("dind-maintenance: evaluation failed", { error: errMessage(err) });
        // A broken sleep must not spin an unbounded retry loop.
        return;
      }
    }
  }
}
