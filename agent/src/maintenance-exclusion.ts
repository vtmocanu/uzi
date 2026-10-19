// Issue #3031 writer exclusion for the model-free terminal producer (ADR-1197, "Terminal-
// maintenance writer exclusion"). It reuses the existing claim gate: close it under
// `terminal_maintenance`, require an idle worker with no in-flight claims, prove no other
// writer exists, and hold that for the producer's whole run boundary. It never signals anything.
import type { DindPruneGate } from "./dind-prune.js";
import type { Logger } from "./log.js";

/** Why exclusion was not established. All refusals happen before any slot charge. */
export type ExclusionRefusal = "exclusion_busy" | "claim_gate_held" | "claims_in_flight" |
  "worker_not_idle" | "maintenance_writer_exclusion_unproven";

export interface MaintenanceExclusionDeps {
  gate: DindPruneGate;
  isIdle: () => boolean;
  /** The deny-only process proof plus, when Docker is wired, the running-container probe, inside
   *  one accounting scope: 1 pass in uncharged discovery, 3 inside the charged slot. */
  proof: (scans: 1 | 3) => Promise<boolean>;
  log: Logger;
}

/** Handed to the producer while exclusion holds. One invocation's lifetime; never renewed. */
export interface MaintenanceExclusion {
  /** Pure in-memory. False for good once this invocation ends, another owner closes the gate,
   *  a claim enters, or the worker stops being idle. No RPC, no Git. */
  open(): boolean;
  /** The authoritative charged proof (C2/C4). Throws on refusal. */
  prove(): Promise<void>;
}

const OWNER = "terminal_maintenance";

export class MaintenanceWriterExclusion {
  private active = false;

  constructor(private readonly deps: MaintenanceExclusionDeps) {}

  /**
   * Run `work`, which must contain the producer's entire run boundary, while no untrusted writer
   * can exist. Cheap refusals and the uncharged one-pass proof come first, so a busy or occupied
   * worker never consumes a slot. After `work` settles (resolved or rejected), `settled()` must
   * confirm the boundary's children actually joined; otherwise the gate stays closed and claims
   * stay gated until the worker restarts (fail closed, never reopened for liveness).
   */
  async run<T>(work: (exclusion: MaintenanceExclusion) => Promise<T>, settled: () => boolean):
    Promise<{ entered: false; reason: ExclusionRefusal } | { entered: true; value: T }> {
    const { gate, isIdle } = this.deps;
    // Single flight, and never alongside another gate owner (prune, maintenance, operator discard).
    if (this.active) return { entered: false, reason: "exclusion_busy" };
    if (gate.claimsClosed()) return { entered: false, reason: "claim_gate_held" };
    this.active = true;
    gate.close(OWNER);
    // Any later close/open by anyone, even one undone before we look, ends this scope for good.
    const generation = gate.generation();
    let live = true, started = false, keepClosed = false;
    const exclusion: MaintenanceExclusion = {
      open: () => {
        if (live && (gate.generation() !== generation || !gate.onlyHeldBy(OWNER) ||
            gate.inFlightClaims() > 0 || !isIdle())) live = false;
        return live;
      },
      prove: async () => {
        if (!exclusion.open() || !await this.deps.proof(3) || !exclusion.open())
          throw new Error("maintenance_writer_exclusion_unproven");
      },
    };
    try {
      // Busy refusals are immediate; the next ordinary pass retries.
      if (gate.inFlightClaims() > 0) return { entered: false, reason: "claims_in_flight" };
      if (!isIdle()) return { entered: false, reason: "worker_not_idle" };
      if (!await this.deps.proof(1) || !exclusion.open())
        return { entered: false, reason: "maintenance_writer_exclusion_unproven" };
      started = true;
      return { entered: true, value: await work(exclusion) };
    } finally {
      live = false;
      if (started && !settled()) {
        keepClosed = true;
        this.deps.log.error("terminal maintenance settlement unconfirmed; claims stay gated until the worker restarts",
          { diagnostic: "maintenance_writer_settlement_unconfirmed" });
      }
      if (!keepClosed) {
        gate.open(OWNER);
        this.active = false;
      }
    }
  }
}
