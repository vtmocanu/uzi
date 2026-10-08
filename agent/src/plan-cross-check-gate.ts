import { setTimeout as delay } from "node:timers/promises";
import {
  RequestError, type WorkerClient, type PlanCrossCheckCandidate, type PlanCrossCheckResponse,
  type PlanCrossCheckStateRequest, type PlanCrossCheckGateReason,
} from "./client.js";
import type { CandidateTransportReservation, MessageBatcher } from "./batcher.js";
import type { StateAck } from "./protocol.js";

/** Execution-local context only. Status proofs never create or replace a presentation. */
export interface CheckedHumanGate {
  phase: "publishinginitial" | "confirmedwait" | "revisionplanning" | "publishingrevised" | "terminal";
  continueWait?: () => Promise<import("./steering.js").PlanVerdict>;
  onApplied?: (ack: StateAck) => void | Promise<void>;
}

export interface PlanCrossCheckTiming {
  preparationMs?: number;
  backoffMs?: readonly [number, number];
  requestMs?: number;
  pollMs?: number;
}

type CandidateResponse = Extract<PlanCrossCheckResponse, { result: "candidate" }>;
export type CheckedPlanDecision =
  | { kind: "approve"; response: CandidateResponse }
  | { kind: "revise"; response: CandidateResponse }
  | { kind: "human"; fields: PlanCrossCheckStateRequest; reservation?: CandidateTransportReservation;
      completePreparation?: (signal: AbortSignal) => Promise<void> };

/** One checker candidate, bounded preparation and submission retries, owner-cancellable polling.
 * Each failed HTTP attempt is awaited through cancellation before any sibling request starts.
 */
interface CheckPlanOptions {
  client: WorkerClient; runId: string; generation: number; candidate: PlanCrossCheckCandidate;
  batcher: MessageBatcher; signal: AbortSignal; timing?: PlanCrossCheckTiming;
  round?: number;
  allowAutomaticRevision?: boolean;
}

export function checkPlan(options: CheckPlanOptions & { allowAutomaticRevision?: false }):
  Promise<Exclude<CheckedPlanDecision, { kind: "revise" }>>;
export function checkPlan(options: CheckPlanOptions): Promise<CheckedPlanDecision>;
export async function checkPlan(options: CheckPlanOptions): Promise<CheckedPlanDecision> {
  const { client, runId, generation, candidate, batcher, signal } = options;
  const timing = options.timing;
  const round = options.round ?? 1;
  if (!Number.isSafeInteger(round) || round < 1 || round > 5)
    throw new Error("plan cross-check: invalid round");
  signal.throwIfAborted();
  if (!Number.isSafeInteger(generation) || generation <= 0)
    throw new Error("plan cross-check: invalid claim generation");
  let reservation: CandidateTransportReservation;
  try { reservation = batcher.reserveCandidateTransport(); }
  catch { throw new Error("plan cross-check: transport reservation unavailable"); }
  const cancel = (): void => reservation.cancel();
  signal.addEventListener("abort", cancel, { once: true });
  const request = async <T>(fn: (owned: AbortSignal) => Promise<T>, ms = timing?.requestMs ?? 3000): Promise<T> => {
    signal.throwIfAborted();
    // The client owns and settles HTTP cancellation; never race an unowned POST against a timer.
    return fn(AbortSignal.any([signal, AbortSignal.timeout(ms)]));
  };
  let identity: CandidateResponse | undefined;
  type InvalidIdentity = { invalid: "interrupted" | "checker_failed" };
  const guard = (response: PlanCrossCheckResponse): CandidateResponse | InvalidIdentity => {
    if (response.result === "parked")
      throw new Error("plan cross-check: parked without an established human presentation");
    if (response.result !== "candidate")
      return { invalid: "checker_failed" };
    if (response.candidate_generation !== generation)
      return { invalid: response.candidate_generation < generation ? "interrupted" : "checker_failed" };
    const deadline = Date.parse(response.deadline_at);
    if (response.round !== round ||
        !Number.isSafeInteger(response.candidate_generation) || generation <= 0 ||
        !/^[a-f0-9]{64}$/.test(response.candidate_digest) ||
        response.candidate.base_commit !== candidate.base_commit ||
        !Number.isFinite(deadline) || deadline > Date.now() + 2 * 60 * 60 * 1000 ||
        (identity && (response.candidate_digest !== identity.candidate_digest ||
          response.checker_run_id !== identity.checker_run_id ||
          response.deadline_at !== identity.deadline_at ||
          response.automatic_revision_limit !== identity.automatic_revision_limit ||
          response.automatic_rounds_enabled !== identity.automatic_rounds_enabled ||
          JSON.stringify(response.candidate) !== JSON.stringify(identity.candidate))))
      return { invalid: "checker_failed" };
    identity ??= response;
    return response;
  };
  let preparationAttempts = 0;
  try {
    const prepare = async (owner: AbortSignal): Promise<boolean> => {
      owner.throwIfAborted();
      preparationAttempts++;
      const preparationMs = Math.min(timing?.preparationMs ?? 3000, 3000);
      const owned = AbortSignal.any([owner, AbortSignal.timeout(preparationMs)]);
      const prepared = await reservation.prepare(preparationMs, owned);
      owner.throwIfAborted();
      if (prepared.prepared) {
        const confirmed = await batcher.usage.drainConfirmed(preparationMs, owned);
        owner.throwIfAborted();
        if (batcher.usage.hasUnconfirmedLoss || batcher.usage.inactive)
          throw new Error("plan cross-check: preparation receipts irrecoverably lost");
        return confirmed;
      }
      if (prepared.permanent) {
        const condition = prepared.reason === "usage_unconfirmed" || prepared.reason === "ownership_unknown"
          ? "preparation receipts irrecoverably lost"
          : `transport permanently failed (${prepared.reason ?? "unknown"})`;
        throw new Error(`plan cross-check: ${condition}`);
      }
      return false;
    };
    const heldHuman = (): CheckedPlanDecision => ({ kind: "human", reservation,
      fields: { status: "awaiting_approval", plan_cross_check_gate_reason: "checker_failed",
        plan_cross_check_refusal: "submit_failed" },
      completePreparation: async (owner) => {
        // Only the applied forced gate owns these remaining attempts. Keep the original
        // reservation on exhaustion; MessageBatcher.close preserves its assigned replay.
        while (preparationAttempts < 3) {
          const backoff = preparationAttempts - 1;
          await delay(timing?.backoffMs?.[backoff] ?? preparationAttempts * 1000, undefined, { signal: owner });
          if (await prepare(owner)) return;
        }
        owner.throwIfAborted();
        throw new Error("plan cross-check: preparation ACKs unrecoverable");
      } });
    if (!await prepare(signal)) return heldHuman();
    const checkTransport = async (): Promise<boolean> => {
      signal.throwIfAborted();
      if (batcher.usage.hasUnconfirmedLoss || batcher.usage.inactive)
        throw new Error("plan cross-check: preparation receipts irrecoverably lost");
      const preparationMs = Math.min(timing?.preparationMs ?? 3000, 3000);
      const owned = AbortSignal.any([signal, AbortSignal.timeout(preparationMs)]);
      const prepared = await reservation.prepare(preparationMs, owned);
      signal.throwIfAborted();
      if (!prepared.prepared && prepared.permanent)
        throw new Error(`plan cross-check: transport permanently failed (${prepared.reason ?? "unknown"})`);
      // prepare caches its successful receipt; usage produced while polling has its
      // own ACK check and must join the same held completion if that check fails.
      const confirmed = prepared.prepared &&
        await batcher.usage.drainConfirmed(preparationMs, owned);
      signal.throwIfAborted();
      if (batcher.usage.hasUnconfirmedLoss || batcher.usage.inactive)
        throw new Error("plan cross-check: preparation receipts irrecoverably lost");
      if (!confirmed) preparationAttempts++;
      return confirmed;
    };
    const human = async (reason: PlanCrossCheckGateReason,
      refusal?: PlanCrossCheckStateRequest["plan_cross_check_refusal"]): Promise<CheckedPlanDecision> => {
      if (!await checkTransport()) return heldHuman();
      return { kind: "human", reservation, fields: { status: "awaiting_approval",
        plan_cross_check_gate_reason: reason, ...(refusal ? { plan_cross_check_refusal: refusal } : {}) } };
    };
    let response: PlanCrossCheckResponse | undefined;
    // A no_row GET after POST is only a read-time observation. It cannot release transport.
    for (let attempt = 0; attempt < 3 && response === undefined; attempt++) {
      signal.throwIfAborted();
      if (!await checkTransport()) return heldHuman();
      reservation.markSubmitted();
      try {
        response = await request((owned) => client.submitPlanCrossCheck(runId, generation, candidate, owned, round));
      } catch (error) {
        signal.throwIfAborted();
        if (error instanceof RequestError && error.status >= 400 && error.status < 500) {
          let reason: unknown;
          try { reason = (JSON.parse(error.body) as { reason?: unknown }).reason; } catch { /* absent attestation */ }
          if (error.status === 413) return await human("candidate_refused", "envelope_too_large");
          if (error.status === 400) return await human("candidate_refused",
            reason === "candidate_too_large" ? "candidate_too_large" : "candidate_invalid");
          if (error.status === 409 && (reason === "checker_unavailable" || reason === "interrupted" || reason === "revisions_exhausted"))
            return await human(reason);
          return await human("checker_failed", "submit_failed");
        }
        try {
          const status = await request((owned) => client.planCrossCheckStatus(runId, generation, round, owned));
          if (status.result !== "no_row") response = status;
        } catch (statusError) {
          signal.throwIfAborted();
          if (statusError instanceof Error && statusError.message === "invalid cross-check response")
            return await human("checker_failed", "submit_failed");
        }
        if (response === undefined && attempt < 2)
          await delay(timing?.backoffMs?.[attempt] ?? (attempt + 1) * 1000, undefined, { signal });
      }
    }
    if (response === undefined || response.result === "no_row")
      return await human("checker_failed", "submit_failed");
    let current = guard(response);
    if ("invalid" in current)
      return await human(current.invalid, current.invalid === "checker_failed" ? "submit_failed" : undefined);
    // Bound by the immutable stored deadline and owner cancellation. A transient status failure
    // retains the same identity; every iteration sleeps, including failed reads.
    while (current.verdict === "pending") {
      if (!await checkTransport()) return heldHuman();
      const remaining = Date.parse(current.deadline_at) - Date.now();
      if (remaining <= 0) return await human("timed_out");
      await delay(Math.min(timing?.pollMs ?? 15_000, remaining), undefined, { signal });
      signal.throwIfAborted();
      if (!await checkTransport()) return heldHuman();
      if (Date.now() >= Date.parse(current.deadline_at)) return await human("timed_out");
      let polled: PlanCrossCheckResponse;
      try {
        polled = await request((owned) => client.planCrossCheckStatus(runId, generation, round, owned),
          Math.max(1, Math.min(timing?.requestMs ?? 3000, Date.parse(current.deadline_at) - Date.now())));
      } catch (error) {
        signal.throwIfAborted();
        if (error instanceof RequestError && error.status >= 400 && error.status < 500)
          return await human("checker_failed", "submit_failed");
        // Only the decoder validation failure changes disposition; transport failures retry.
        if (error instanceof Error && error.message === "invalid cross-check response")
          return await human("checker_failed", "submit_failed");
        if (error instanceof Error && error.message === "invalid parked cross-check response") throw error;
        continue;
      }
      const validated = guard(polled);
      if ("invalid" in validated)
        return await human(validated.invalid, validated.invalid === "checker_failed" ? "submit_failed" : undefined);
      current = validated;
    }
    if (!await checkTransport()) return heldHuman();
    if (current.verdict === "approve") {
      if (Date.now() >= Date.parse(current.deadline_at)) return await human("timed_out");
      if (!reservation.release(current))
        return await human("checker_failed", "submit_failed");
      return { kind: "approve", response: current };
    }
    if (current.verdict === "revise" && options.allowAutomaticRevision && current.automatic_rounds_enabled) {
      if (round >= current.automatic_revision_limit + 1) return await human("revisions_exhausted");
      // A decided REVISE was accepted before the server deadline. A delayed ACK does
      // not turn that decision back into a pending timeout; fresh submit revalidates eligibility.
      if (!reservation.release(current))
        throw new Error("plan cross-check: revision settlement receipts unavailable");
      signal.throwIfAborted();
      return { kind: "revise", response: current };
    }
    // Keep the reservation until the applied forced gate fences any delayed submit.
    return await human(current.verdict === "revise" ? "revise" : current.verdict === "block" ? "block" :
      current.reason_class === "" || current.reason_class === "approve" ? "checker_failed" : current.reason_class);
  } catch (error) {
    reservation.cancel();
    throw error;
  } finally {
    signal.removeEventListener("abort", cancel);
  }
}
