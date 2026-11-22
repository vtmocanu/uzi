import { coordinateCodexSwitchPreflight, type CodexSwitchOperation, type CodexSwitchAuthority, type CodexSwitchPreflightOptions } from "./codex-switch-preflight.js";
import type { LedgerApplyResult } from "./credential-switch-op-ledger.js";
import { selectCodexBinding, type CodexBinding } from "./codex/select.js";

export interface SwitchBounds {
  signal: AbortSignal;
  deadline: number;
  /** Adapters must call immediately before each effect, including after awaits. */
  guard(): void;
}
export interface CodexSwitchBundle {
  readonly binding: CodexBinding;
  readonly capabilityEpoch: number;
  readonly ordinal: number;
  readonly messageSeq: number;
}
export type CodexRestartResult = { kind: "resumed" | "confirmed_resume_refusal" | "uncertain" };
export type CodexCoordinatorOutcome =
  | { kind: "codex_continued_A" | "codex_resumed_B" | "codex_fresh_B" | "codex_owner_paused" | "codex_retained_stop" | "codex_fenced" | "PolicyContinuationRejected" };

export interface CodexSwitchCoordinatorOptions {
  initial: CodexSwitchOperation;
  signal: AbortSignal;
  now(): number;
  deadline: number;
  previousOrdinal: number;
  preserveConversation: boolean;
  reconcile(bounds: SwitchBounds): Promise<CodexSwitchAuthority>;
  preflight: Pick<CodexSwitchPreflightOptions, "probe" | "delay" | "confirmFailedClear">;
  /** Op-fenced persistence failure notice and confirmed clear; no API refusal reason. */
  confirmPersistFailedClear(operation: CodexSwitchOperation, bounds: SwitchBounds): Promise<boolean>;
  /** Must positively attest a credential-free, usable rollout, never a partial write. */
  persistRollout(bounds: SwitchBounds): Promise<{ kind: "usable"; sessionId: string } | { kind: "failed" }>;
  drainAndSuspend(bounds: SwitchBounds): Promise<void>;
  /** Recheck bounds.guard() immediately before reopening A and after any awaited effect. */
  reopenA(operation: CodexSwitchOperation, bounds: SwitchBounds): Promise<void>;
  /** True only after root AND descendants are proven reaped. */
  teardown(bounds: SwitchBounds): Promise<boolean>;
  reportUnsettled(operation: CodexSwitchOperation, bounds: SwitchBounds): Promise<void>;
  apply(operation: CodexSwitchOperation, bounds: SwitchBounds): Promise<LedgerApplyResult>;
  /** Repeating this op-fenced report is the confirmation mechanism; status alone is insufficient. */
  uncertainHold(operation: CodexSwitchOperation, reason: "not_applied_after_teardown" | "apply_uncertain" | "initialization_failed", bounds: SwitchBounds): Promise<boolean>;
  requestAndConfirmOwnerPause(tuple: Readonly<{ claimGeneration: number; registerNonce: string; expectedEpoch: number }>, bounds: SwitchBounds): Promise<boolean>;
  registerSecrets(bundle: Readonly<CodexSwitchBundle>, bounds: SwitchBounds): Promise<void>;
  publish(bundle: Readonly<CodexSwitchBundle>, operation: CodexSwitchOperation, bounds: SwitchBounds): Promise<void>;
  /** Create fresh root, login B, then resume savedSessionId, in that order. */
  restartAndResume(bundle: Readonly<CodexSwitchBundle>, savedSessionId: string, bounds: SwitchBounds): Promise<CodexRestartResult>;
  startFreshLineage(bundle: Readonly<CodexSwitchBundle>, bounds: SwitchBounds): Promise<void>;
}

/**
 * Inert until Runner supplies adapters. No release/reclaim seam exists.
 * Callbacks must honor the monotonic deadline and signal and finish all their work
 * before returning; this coordinator never races or detaches an effect.
 */
export async function coordinateCodexSwitch(o: CodexSwitchCoordinatorOptions): Promise<CodexCoordinatorOutcome> {
  let expectedEpoch: number | undefined;
  let teardownBegan = false;
  let reaped = false;
  let stale = false;
  let committed: CodexSwitchOperation | undefined;
  let priorAmbiguous = false;
  let holdReason: "not_applied_after_teardown" | "apply_uncertain" | "initialization_failed" = "not_applied_after_teardown";
  let current = Object.freeze({ ...o.initial });
  const outcome = (kind: CodexCoordinatorOutcome["kind"]): CodexCoordinatorOutcome => ({ kind });
  const b: SwitchBounds = { signal: o.signal, deadline: o.deadline, guard() {
    if (stale || o.signal.aborted || o.now() >= o.deadline) throw new Error("stopped");
  } };
  const call = async <T>(fn: () => Promise<T>): Promise<T> => {
    b.guard(); const result = await fn(); b.guard(); return result;
  };
  const same = (p: CodexSwitchOperation) => p.opId === current.opId && p.revision === current.revision &&
    p.targetSecretId === current.targetSecretId;
  async function authority(bounds: SwitchBounds = b): Promise<CodexSwitchAuthority> {
    const a = await call(() => o.reconcile(bounds));
    if (!a.owned || a.claimGeneration !== o.initial.claimGeneration || a.registerNonce !== o.initial.registerNonce) {
      stale = true; throw new Error("fenced");
    }
    return a;
  }
  function valid(p: CodexSwitchOperation | undefined): p is CodexSwitchOperation {
    return !!p && p.claimGeneration === o.initial.claimGeneration && p.registerNonce === o.initial.registerNonce &&
      !!p.opId && !!p.targetSecretId && Number.isSafeInteger(p.revision);
  }
  async function exact(): Promise<void> {
    const a = await authority();
    if (!valid(a.pending) || !same(a.pending)) throw new Error("superseded");
  }
  async function appliedAuthority(): Promise<void> {
    const a = await authority();
    if (a.pending !== undefined) throw new Error("pending_after_apply");
  }
  async function pause(): Promise<CodexCoordinatorOutcome> {
    let a = await authority();
    const report = valid(a.pending) && (same(a.pending) || a.pending.revision > current.revision) ? a.pending : a.pending === undefined ? committed : undefined;
    if (report) {
      const ok = await call(() => o.uncertainHold(report, priorAmbiguous ? "apply_uncertain" : holdReason, b));
      if (ok) return outcome("codex_owner_paused");
      a = await authority();
      // A refused op report cannot justify an ordinary pause over a pending operation.
      if (a.pending !== undefined) return outcome("codex_retained_stop");
    } else if (a.pending !== undefined) return outcome("codex_retained_stop");
    if (!reaped || committed || priorAmbiguous || expectedEpoch === undefined) return outcome("codex_retained_stop");
    const tuple = Object.freeze({ claimGeneration: o.initial.claimGeneration, registerNonce: o.initial.registerNonce, expectedEpoch });
    return outcome(await call(() => o.requestAndConfirmOwnerPause(tuple, b)) ? "codex_owner_paused" : "codex_retained_stop");
  }
  try {
    await call(() => o.drainAndSuspend(b));
    let savedSessionId: string | undefined;
    try {
      const saved = await call(() => o.persistRollout(b));
      if (saved.kind === "usable" && saved.sessionId.trim() !== "") savedSessionId = saved.sessionId;
    }
    catch { b.guard(); }
    if (savedSessionId === undefined) {
      await exact();
      const cleared = await call(() => o.confirmPersistFailedClear(current, b));
      if (!cleared) return outcome("codex_retained_stop");
      const afterClear = await authority();
      if (afterClear.pending !== undefined) return outcome("codex_retained_stop");
      await call(() => o.reopenA(current, b));
      return outcome("codex_continued_A");
    }
    const preDeadline = Math.min(o.deadline, o.now() + 10_000);
    const seen = new Set([current.opId]);
    // Serial successors share the same deadline and sticky teardown proof. No sibling work.
    for (let attempts = 0; attempts < 32; attempts++) {
      const a = await authority();
      if (!valid(a.pending)) return teardownBegan ? await pause() : outcome("codex_retained_stop");
      if (!same(a.pending)) {
        if (a.pending.revision <= current.revision || seen.has(a.pending.opId)) return outcome("codex_retained_stop");
        current = Object.freeze({ ...a.pending }); seen.add(current.opId);
      }
      if (!a.routeAvailable || !a.ledgerAvailable)
        return teardownBegan ? await pause() : outcome("codex_retained_stop");
      const pb: SwitchBounds = { signal: o.signal, deadline: preDeadline, guard() {
        b.guard(); if (o.now() >= preDeadline) throw new Error("stopped");
      } };
      const preCall = async <T>(fn: () => Promise<T>): Promise<T> => {
        pb.guard(); const value = await fn(); pb.guard(); return value;
      };
      const pre = await coordinateCodexSwitchPreflight({
        probe: p => preCall(() => o.preflight.probe(p, pb)),
        delay: ms => preCall(() => o.preflight.delay(Math.min(ms, preDeadline - o.now()), pb)),
        initial: current, signal: o.signal, now: o.now,
        reconcile: () => preCall(() => authority(pb)),
        confirmFailedClear: (p, reason) => preCall(async () => {
          if (teardownBegan) { holdReason = "not_applied_after_teardown"; return false; }
          return o.preflight.confirmFailedClear(p, reason, pb);
        }),
      });
      if (pre.kind === "fenced") { stale = true; return outcome("codex_fenced"); }
      if (pre.kind === "continued_A") {
        if (teardownBegan) return await pause();
        current = Object.freeze({ ...pre.operation });
        const afterClear = await authority();
        // Keep A gated while the loop validates any successor against current.
        if (afterClear.pending !== undefined) continue;
        await call(() => o.reopenA(current, b));
        return outcome("codex_continued_A");
      }
      if (pre.kind !== "ready") {
        if (pre.kind === "retained_stop" && ["uncertain", "committed"].includes(pre.reason)) holdReason = "apply_uncertain";
        return teardownBegan ? await pause() : outcome("codex_retained_stop");
      }
      if (expectedEpoch !== undefined && expectedEpoch !== pre.expectedEpoch) return await pause();
      expectedEpoch ??= pre.expectedEpoch;
      current = Object.freeze({ ...pre.operation });
      const atBarrier = await authority();
      if (!valid(atBarrier.pending) || !same(atBarrier.pending)) continue;
      if (!teardownBegan) {
        teardownBegan = true;
        reaped = await call(() => o.teardown(b));
        if (!reaped) {
          await call(() => o.reportUnsettled(current, b));
          return outcome("codex_retained_stop");
        }
      }
      const beforeApply = await authority();
      if (!valid(beforeApply.pending) || !same(beforeApply.pending)) continue;
      if (!beforeApply.routeAvailable || !beforeApply.ledgerAvailable) return await pause();
      holdReason = "apply_uncertain";
      committed = current; // A send may commit even if its reply is lost.
      const applied = await call(() => o.apply(current, b));
      if (applied.kind === "fenced") { stale = true; return outcome("codex_fenced"); }
      if ("priorAmbiguous" in applied && applied.priorAmbiguous) priorAmbiguous = true;
      if (applied.kind === "superseded") { if (!priorAmbiguous) committed = undefined; continue; }
      if (!priorAmbiguous && ["not_applied", "route_absent", "ledger_unavailable"].includes(applied.kind)) {
        committed = undefined; holdReason = "not_applied_after_teardown";
      }
      if (applied.kind !== "codex_applied") return await pause();
      await appliedAuthority();
      const selected = selectCodexBinding({ codex: applied.codex });
      if (selected.kind !== "codex" || !Number.isSafeInteger(applied.ordinal) ||
          applied.ordinal !== o.previousOrdinal + 1 || !Number.isSafeInteger(applied.capabilityEpoch) ||
          applied.capabilityEpoch <= 0 || !Number.isSafeInteger(applied.messageSeq) || applied.messageSeq <= 0 || applied.replay)
        return await pause();
      const bundle = Object.freeze({ binding: Object.freeze(selected.binding), capabilityEpoch: applied.capabilityEpoch,
        ordinal: applied.ordinal, messageSeq: applied.messageSeq });
      await call(() => o.registerSecrets(bundle, b));
      await appliedAuthority();
      await call(() => o.publish(bundle, current, b));
      holdReason = "initialization_failed";
      await appliedAuthority();
      const started = await call(() => o.restartAndResume(bundle, savedSessionId, b));
      if (started.kind === "resumed") return outcome("codex_resumed_B");
      if (started.kind !== "confirmed_resume_refusal") return await pause();
      if (o.preserveConversation) {
        await pause();
        return outcome("PolicyContinuationRejected");
      }
      await appliedAuthority();
      await call(() => o.startFreshLineage(bundle, b));
      return outcome("codex_fresh_B");
    }
    return teardownBegan ? await pause() : outcome("codex_retained_stop");
  } catch {
    if (stale) return outcome("codex_fenced");
    if (o.signal.aborted || o.now() >= o.deadline) return outcome("codex_retained_stop");
    try {
      if (teardownBegan && !reaped) await call(() => o.reportUnsettled(current, b));
      else if (teardownBegan) return await pause();
    } catch { /* Unconfirmed reports retain custody; never retry without a bound. */ }
    return outcome(stale ? "codex_fenced" : "codex_retained_stop");
  }
}
