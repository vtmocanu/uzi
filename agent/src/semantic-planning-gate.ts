import { validateCoverageReport } from "./plan-contract-report.js";
import { randomUUID, createHash } from "node:crypto";
import { isDeepStrictEqual } from "node:util";
import type { WorkerClient, PlanCrossCheckCandidate } from "./client.js";
import { parseSemanticCandidate, semanticSourceCaptureSchema, type SemanticSubmission } from "./semantic-plan.js";
import { reviewPlanContract, type BoundedCoverageTestTransport,
  type CoverageReservation, type PlanContractReviewResult } from "./plan-contract-review.js";
import type { SemanticBinding, SemanticReservation } from "./semantic-coverage-wire.js";

type Compiled = Awaited<ReturnType<WorkerClient["submitSemanticCandidate"]>>;
const sameBinding = (a: SemanticBinding, b: SemanticBinding) =>
  a.candidate_digest === b.candidate_digest && a.source_digest === b.source_digest && a.criteria_digest === b.criteria_digest;

/** One execution-local adapter; the server owns identity, replay and all run-wide budgets.
 * Each HTTP operation has the client's finite timeout plus the owner's cancellation.
 * No model admission is inferred from a replay or an ambiguous response.
 */
export class SemanticPlanningGate {
  constructor(private readonly client: WorkerClient, private readonly runId: string,
    private readonly generation: number, private readonly signal: AbortSignal,
    private readonly testTransport?: BoundedCoverageTestTransport) {}

  async compile(structural: PlanCrossCheckCandidate, proposal: SemanticSubmission | undefined,
    sources: readonly unknown[]): Promise<Compiled> {
    const compiled = await this.client.submitSemanticCandidate(this.runId, this.generation, structural, proposal, sources, this.signal);
    if (compiled.plan !== Buffer.from(structural.plan_md).toString("base64")) throw new Error("Foreign semantic plan");
    parseSemanticCandidate(compiled.candidate, { sources: compiled.sources, milestoneIds: structural.milestones.map(m => (m as { id: string }).id) });
    return compiled;
  }

  async review(compiled: Compiled, plan: string, _rawSources: readonly unknown[]): Promise<PlanContractReviewResult> {
    const { candidate, binding } = compiled;
    if (compiled.plan !== Buffer.from(plan).toString("base64")) throw new Error("Foreign semantic plan");
    const sources = compiled.sources.map(s => semanticSourceCaptureSchema.parse(s));
    const status = await this.client.semanticCoverageStatus(this.runId, this.generation, this.signal);
    if (status.report && sameBinding(status, binding)) {
      const report = validateCoverageReport(JSON.stringify(status.report), candidate, sources, binding);
      const complete = candidate.source_outcome === "complete" &&
        report.requirements.every(r => r.status === "represented") && report.criteria.every(c => c.atomic);
      return { outcome: complete ? "complete" : "incomplete",
        reason: complete ? "coverage_complete" : "coverage_incomplete", candidate_digest: candidate.digest,
        report, invocations: 0, tokens: 0, elapsed_ms: 0 };
    }
    let reviewId: string | undefined;
    let last: SemanticReservation | undefined;
    const admissions = new Map<string, SemanticReservation>();
    const result = await reviewPlanContract({
      candidate, sources, plan, binding, testTransport: this.testTransport,
      budget: {
        reserve: async request => {
          // One UUID per invocation, retained if an HTTP layer retries the request.
          const body = { ...binding, request_id: randomUUID(), ...(reviewId ? { review_id: reviewId } : {}),
            supported: this.testTransport?.kind === "bounded-test" };
          const reserved = await this.client.reserveSemanticCoverage(this.runId, this.generation, body, this.signal);
          if (reserved.outcome !== "reserved") return null;
          if (!sameBinding(reserved, binding) || !reserved.review_id || !reserved.invocation_id ||
              reserved.model !== "sonnet" || (reviewId && reserved.review_id !== reviewId))
            throw new Error("Foreign semantic reservation");
          reviewId = reserved.review_id;
          last = reserved;
          admissions.set(reserved.invocation_id, reserved);
          // The reservation's server snapshots must agree with the previously compiled identity.
          const canonical = parseSemanticCandidate(reserved.candidate, { sources: reserved.sources! });
          if (canonical.digest !== candidate.digest || reserved.plan !== Buffer.from(plan).toString("base64") ||
              createHash("sha256").update(Buffer.from(reserved.plan!, "base64")).digest("hex") !== candidate.plan_hash ||
              !isDeepStrictEqual(canonical, candidate) ||
              !isDeepStrictEqual(reserved.sources, sources))
            throw new Error("Foreign semantic snapshots");
          return { id: reserved.invocation_id, model: reserved.model,
            tokens: reserved.max_tokens,
            deadlineMs: Math.min(Date.parse(reserved.deadline), request.deadlineMs) };
        },
        settle: async (reservation: CoverageReservation, usage) => {
          const admitted = admissions.get(reservation.id);
          if (!admitted) throw new Error("Missing semantic admission");
          await this.client.settleSemanticCoverage(this.runId, this.generation, {
            review_id: admitted.review_id!, invocation_id: admitted.invocation_id!, status: usage.status,
            ...(!usage.conservative ? { tokens: usage.tokens } : {}),
          }, this.signal);
        },
      },
    });
    // Settlement is independent of publication. A failed settlement never publishes.
    if (result.report && last?.review_id && last.invocation_id && result.reason !== "coverage_settlement_failed") {
      await this.client.publishSemanticCoverage(this.runId, this.generation, {
        review_id: last.review_id, invocation_id: last.invocation_id, report: result.report,
      }, this.signal);
    }
    return result;
  }
}
