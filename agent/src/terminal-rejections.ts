import type { WorkerClient } from "./client.js";
import type { Outbox, TerminalAuthenticationObservation } from "./outbox.js";
import type { Logger } from "./log.js";
import { RunDiskLocks } from "./run-disk-locks.js";
import { sleep } from "./util.js";

/** Diagnostic reconciliation owns no lease or recovery authority. Its only inventory is one
 * physical page; failed transports advance that page and retry on the next durable scan. */
export class TerminalRejectionCoordinator {
  private readonly admission = new RunDiskLocks();
  private readonly admitted = new Map<string, number>();
  private wake?: AbortController;
  private running = false;
  private wakePending = false;
  private unsupported = false;

  constructor(
    private readonly outbox: Outbox,
    private readonly client: WorkerClient,
    private readonly log: Logger,
    private readonly diskLocks = new RunDiskLocks(),
    private readonly isActive: (runId: string) => boolean = () => false,
    private readonly intervalMs = 30_000,
    private readonly timeoutMs = 5_000,
  ) {}

  /** Only a coalesced wake: callbacks can run under recovery locks without taking deletion locks. */
  queueReconciliation(_runId?: string, _generation?: number): void {
    this.wakePending = true;
    this.wake?.abort();
  }

  acquireAdmission(signal?: AbortSignal): Promise<() => void> {
    const timeout = AbortSignal.timeout(this.timeoutMs);
    return this.admission.acquire("admission", signal ? AbortSignal.any([signal, timeout]) : timeout);
  }

  /** Registered before dispatch, including runners without an active registry. Bounded by slots. */
  protectExecution(runId: string): () => void {
    const id = runId.toLowerCase();
    this.admitted.set(id, (this.admitted.get(id) ?? 0) + 1);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const count = this.admitted.get(id)!;
      if (count === 1) this.admitted.delete(id);
      else this.admitted.set(id, count - 1);
      this.queueReconciliation();
    };
  }

  private canDelete(runId: string, signal: AbortSignal): boolean {
    return !signal.aborted && this.client.hasFeature("terminal_rejection_report") &&
      !this.admitted.has(runId) && !this.isActive(runId);
  }

  private supported(): boolean {
    if (this.client.hasFeature("terminal_rejection_report")) {
      this.unsupported = false;
      return true;
    }
    if (!this.unsupported) {
      this.unsupported = true;
      this.log.info("terminal rejection reporting unsupported; physical records retained");
    }
    return false;
  }

  /** Single flight, at most one page per turn. Each group gets one POST/GET attempt per pass;
   * one failure never blocks siblings. Sleeps after EOF/errors; yields between full pages. */
  async loop(workerId: string | undefined, signal: AbortSignal): Promise<void> {
    if (this.running) return;
    this.running = true;
    try {
      while (!signal.aborted) {
        let passComplete = true;
        try {
          if (this.supported() && workerId) {
            const page = await this.outbox.scanTerminalObservationsPage();
            passComplete = page.passComplete;
            // The scanner bounds the whole page to 256 observations; filesystem filename
            // limits bound metadata memory. Only one content-free tuple goes on the wire.
            const pending = page.observations.filter(observation =>
              observation.kind === "mac_failure" && observation.generation !== undefined);
            // Consume each physical run/generation before moving to the next page. Removing the
            // group before transport ensures even a failed POST/GET advances fairness.
            while (pending.length && !signal.aborted) {
              const first = pending[0]!;
              const group = pending.filter(o => o.runId === first.runId &&
                o.physicalRunId === first.physicalRunId && o.generation === first.generation);
              for (let i = pending.length - 1; i >= 0; i--) {
                if (group.includes(pending[i]!)) pending.splice(i, 1);
              }
              if (!this.supported()) break;
              try {
                await this.client.reportTerminalRejections({ rejections: [{
                  run_id: first.runId, claim_generation: first.generation!, reason: "mac_failure",
                }] }, signal, this.timeoutMs);
              } catch {
                // A lost diagnostic ACK supplies no authority; a fresh GET can still settle it.
              }
              if (!this.supported() || signal.aborted) break;
              try {
                await this.dispose(first.runId, first.generation!, workerId, group, signal);
              } catch {
                // Keep physical evidence on lock, filesystem, or transport failure.
              }
            }
          }
        } catch {
          // An inaccessible scan retries after the interval, with no diagnostic payload logged.
        }
        this.wake = new AbortController();
        const waitMs = this.wakePending || !passComplete ? 1 : this.intervalMs;
        this.wakePending = false;
        await sleep(waitMs, AbortSignal.any([signal, this.wake.signal]));
        this.wake = undefined;
      }
    } finally {
      this.wake = undefined;
      await this.outbox.closeTerminalObservationScan();
      this.running = false;
    }
  }

  private async dispose(runId: string, generation: number, workerId: string,
    observations: TerminalAuthenticationObservation[], signal: AbortSignal): Promise<void> {
    signal = AbortSignal.any([signal, AbortSignal.timeout(this.timeoutMs)]);
    const releaseAdmission = await this.admission.acquire("admission", signal);
    try {
      if (!this.canDelete(runId, signal)) return;
      // Same normalized run key as execution/reclaim, plus the physical alias when different.
      const releaseRun = await this.diskLocks.acquire(runId, signal);
      try {
        const physical = observations[0]!.physicalRunId ?? runId;
        const releasePhysical = physical === runId ? undefined : await this.diskLocks.acquire(physical, signal);
        try {
          if (!this.canDelete(runId, signal)) return;
          await this.outbox.cleanupRejectedTerminalFiles(runId, generation, workerId, observations, async () => {
            if (!this.canDelete(runId, signal)) return undefined;
            const fresh = await this.client.getTerminalRejectionCustody(runId, generation, workerId, signal, this.timeoutMs);
            return this.canDelete(runId, signal) ? fresh : undefined;
          }, () => this.canDelete(runId, signal));
        } finally { releasePhysical?.(); }
      } finally { releaseRun(); }
    } finally { releaseAdmission(); }
  }
}
