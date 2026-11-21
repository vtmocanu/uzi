import type { CodexSwitchPreflightResult } from "./client.js";

export interface CodexSwitchOperation {
  claimGeneration: number;
  registerNonce: string;
  opId: string;
  targetSecretId: string;
  /** Increasing local revision of authoritative pending-op observations. */
  revision: number;
}

export interface CodexSwitchAuthority {
  owned: boolean;
  claimGeneration: number;
  registerNonce: string;
  routeAvailable: boolean;
  ledgerAvailable: boolean;
  pending?: CodexSwitchOperation;
}

const reasons = [
  "policy_refusal", "vault_locked", "baseline_missing", "baseline_mismatch",
  "target_moved", "target_mismatch", "credential_disabled", "credential_rejected",
  "credential_unavailable", "credential_changed", "credential_gone",
  "credential_not_decryptable", "credential_busy", "no_pending_switch",
  "generation_mismatch",
] as const;
type SafeReason = typeof reasons[number];
type StopReason = "cancelled" | "deadline" | "unavailable" | "uncertain" | "clear_unconfirmed" | "committed" | "invalid_pending" | "safety_limit";

export type CodexSwitchPreflightDecision =
  | { kind: "ready"; expectedEpoch: number; operation: CodexSwitchOperation }
  | { kind: "continued_A"; operation: CodexSwitchOperation; reason: SafeReason }
  | { kind: "retained_stop"; reason: StopReason }
  | { kind: "fenced" };

interface Bounds { deadline: number; signal: AbortSignal }
export interface CodexSwitchPreflightOptions {
  initial: CodexSwitchOperation;
  signal: AbortSignal;
  now(): number;
  delay(milliseconds: number, bounds: Bounds): Promise<void>;
  probe(operation: CodexSwitchOperation, bounds: Bounds): Promise<CodexSwitchPreflightResult>;
  reconcile(bounds: Bounds): Promise<CodexSwitchAuthority>;
  /** Atomically checks ownership and the exact op, marks failed, and confirms pending clear. */
  confirmFailedClear(operation: CodexSwitchOperation, reason: SafeReason, bounds: Bounds): Promise<boolean>;
}

/**
 * A is already drained and reversibly gated. This unit never applies, tears down,
 * releases a hold, or logs response text. All callbacks must enforce deadline in
 * the injected monotonic clock's domain. Cancellation also stops awaiting a
 * callback that fails to cooperate; adapters must propagate signal to transport.
 */
export async function coordinateCodexSwitchPreflight(o: CodexSwitchPreflightOptions): Promise<CodexSwitchPreflightDecision> {
  const bounds = { deadline: o.now() + 10_000, signal: o.signal };
  const stop = (reason: StopReason): CodexSwitchPreflightDecision => ({ kind: "retained_stop", reason });
  const expired = () => o.now() >= bounds.deadline;
  const check = () => { if (o.signal.aborted || expired()) throw new Error("stopped"); };
  async function call<T>(action: () => Promise<T>): Promise<T> {
    check();
    let abort!: () => void;
    const cancelled = new Promise<never>((_, reject) => {
      abort = () => reject(new Error("cancelled"));
      o.signal.addEventListener("abort", abort, { once: true });
    });
    try { const value = await Promise.race([action(), cancelled]); check(); return value; }
    finally { o.signal.removeEventListener("abort", abort); }
  }
  let current = { ...o.initial };
  let ambiguous = 0;
  let busy = 0;
  const seen = new Set([current.opId]);
  const exact = (r: { claimGeneration: number; registerNonce: string; opId: string }) =>
    r.claimGeneration === current.claimGeneration && r.registerNonce === current.registerNonce && r.opId === current.opId;
  async function reconcile(): Promise<CodexSwitchPreflightDecision | undefined> {
    const a = await call(() => o.reconcile(bounds));
    if (!a.owned || a.claimGeneration !== o.initial.claimGeneration || a.registerNonce !== o.initial.registerNonce)
      return { kind: "fenced" };
    if (!a.routeAvailable || !a.ledgerAvailable) return stop("unavailable");
    const p = a.pending;
    if (!p || p.claimGeneration !== a.claimGeneration || p.registerNonce !== a.registerNonce ||
        !p.opId || !p.targetSecretId || !Number.isSafeInteger(p.revision)) return stop("invalid_pending");
    if (p.opId !== current.opId) {
      if (p.revision <= current.revision || seen.has(p.opId)) return stop("invalid_pending");
      seen.add(p.opId);
      current = { ...p };
      busy = 0;
    } else if (p.revision !== current.revision || p.targetSecretId !== current.targetSecretId) {
      return stop("invalid_pending");
    }
  }
  async function wait(ms: number) {
    await call(() => o.delay(Math.min(ms, bounds.deadline - o.now()), bounds));
  }
  try {
    // Finite even with a broken clock and infinitely many newer operations.
    for (let attempts = 0; attempts < 32; attempts++) {
      const before = await reconcile();
      if (before) return before;
      const requested = current;
      let result: CodexSwitchPreflightResult;
      try { result = await call(() => o.probe({ ...current }, bounds)); }
      catch {
        check();
        result = { kind: "uncertain", detail: "transport" };
      }
      const after = await reconcile();
      if (after) return after;
      if (result.kind === "fenced") return { kind: "fenced" };
      if (result.kind === "uncertain" && ++ambiguous >= 3) return stop("uncertain");
      if (result.kind === "uncertain") {
        await wait(250 * ambiguous);
        continue;
      }
      if (requested.opId !== current.opId) continue;
      if (result.kind === "superseded") return stop("invalid_pending");
      if (!exact(result)) return stop("uncertain");
      if (result.kind === "ready") {
        if (!Number.isSafeInteger(result.expectedEpoch) || result.expectedEpoch <= 0) return stop("uncertain");
        return { kind: "ready", expectedEpoch: result.expectedEpoch, operation: { ...current } };
      }
      if (result.kind === "refused_committed") return stop("committed");
      if (result.kind !== "not_applied") return stop("uncertain");
      if (!reasons.includes(result.reason as SafeReason)) return stop("uncertain");
      const reason = result.reason as SafeReason;
      if (reason === "generation_mismatch") return { kind: "fenced" };
      if (reason === "no_pending_switch") return stop("invalid_pending");
      if (reason === "credential_busy" && ++busy < 6) {
        await wait([250, 500, 1000, 2000, 4000][busy - 1]!);
        continue;
      }
      // Only an exact typed not_applied reaches the op-fenced clear.
      const cleared = await call(() => o.confirmFailedClear({ ...current }, reason, bounds));
      return cleared ? { kind: "continued_A", operation: { ...current }, reason } : stop("clear_unconfirmed");
    }
    return stop("safety_limit");
  } catch {
    return stop(o.signal.aborted ? "cancelled" : expired() ? "deadline" : "uncertain");
  }
}
