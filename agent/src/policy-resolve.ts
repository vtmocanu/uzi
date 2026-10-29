import { decodePolicyHoldReply, validatePolicyHoldRequest, type PolicyHoldRequest } from "./protocol.js";
import type { TerminalOutboxDeps } from "./terminal-resolve.js";
import type { PendingTerminalJournal, ReportRetirementContext } from "./outbox.js";

/** Supplied only by a caller that independently verified this run's current claimed/running flight.
 * Boot/drain/sweep omit it. It grants no authority beyond the server's ownership fences. */
export interface CurrentPolicyAuthority {
  claimGeneration: number;
  continuationMode: PolicyHoldRequest["continuation_mode"];
}

export async function resolvePolicyOutcome(
  deps: TerminalOutboxDeps,
  args: { runId: string; claimGeneration: number; signal?: AbortSignal;
    retirementContext?: ReportRetirementContext; currentPolicyAuthority?: CurrentPolicyAuthority },
  journal: PendingTerminalJournal,
): Promise<void> {
  if (journal.server_settled || !deps.client.reportPolicyHold) return;
  try {
    const original = validatePolicyHoldRequest(journal.body);
    let request = original;
    const authority = args.currentPolicyAuthority;
    if (authority && Number.isSafeInteger(authority.claimGeneration) && authority.claimGeneration > original.source_generation) {
      request = validatePolicyHoldRequest({
        outcome_id: original.outcome_id, source_generation: original.source_generation,
        claim_generation: authority.claimGeneration, detail: "resume_rejected",
        continuation_mode: authority.continuationMode, published: false,
      });
    }
    // A single transport attempt per invocation. Errors and pending leave sibling records retryable.
    const reply = decodePolicyHoldReply(await deps.client.reportPolicyHold(args.runId, request, args.signal), request);
    if (reply.disposition === "pending") return;
    // Delivery settlement is bound to the ORIGINAL authenticated physical lifetime.
    await deps.outbox.markTerminalServerSettled(args.runId, args.claimGeneration, args.retirementContext);
    // Custody/source evidence is retained. Delivery ACK is not deletion authority; the existing
    // guarded retirement path may refuse and must keep the original request byte-intact.
    await deps.outbox.retireTerminal(args.runId, args.claimGeneration, args.retirementContext);
  } catch {
    deps.log.warn("outbox: policy delivery unresolved; retaining original outcome", {
      run_id: args.runId, claim_generation: args.claimGeneration,
    });
  }
}
