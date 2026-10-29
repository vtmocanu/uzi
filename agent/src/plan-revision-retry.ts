import { setTimeout as delay } from "node:timers/promises";
import { TransientRecoveryError } from "./sdk-executor.js";
import { RequestError, RevisionSourceFenceUnavailableError } from "./client.js";
import { PlanRejectedError, PlanRevisionReplacedError, type PlanRevisionOwnerWaitObserver, type PlanRevisionRetryOutcome } from "./executor.js";
import type { RunOwnershipResponse, StateAck, StateRequest } from "./protocol.js";
import type { PlanVerdict, SteeringChannel } from "./steering.js";

export const PLAN_REVISION_RETRY_FEATURE = "plan_revision_retry_v1";

/** A live-flight policy seam, never an operator setting or durable recovery record. */
export interface PlanRevisionRetryPolicy {
  redrives?: number;
  random?: () => number;
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}

export class PlanRevisionRecoveryStoppedError extends Error {
  constructor(message: string, readonly status?: string) {
    super(message);
    this.name = "PlanRevisionRecoveryStoppedError";
  }
}

type Report = (request: StateRequest) => Promise<StateAck>;
interface Source { revision: number; request: StateRequest; report: Report }
interface Active {
  id: number; source: Source; used: number; redriveReady: boolean; cause?: unknown;
  replacement?: Extract<PlanVerdict, { kind: "revise" }>;
}

/** Internal unwind, caught only at the silent-recovery entry points below. */
class PlanRevisionWallSpentError extends Error {}

interface Dependencies {
  generation: number;
  steering: Pick<SteeringChannel, "takeRevisionRecoveryControl" | "awaitRevisionRecoveryEvent" | "retirePersistedRevision" | "setGateRevision">;
  ownership: () => Promise<RunOwnershipResponse>;
  signal: () => AbortSignal;
  checkControl: () => void;
  notice: (text: string) => Promise<void>;
  policy?: PlanRevisionRetryPolicy;
}

const positive = (n: unknown): n is number => typeof n === "number" && Number.isSafeInteger(n) && n > 0;
const sameIDs = (a: readonly number[] | undefined, b: readonly number[]): boolean =>
  a !== undefined && a.length === b.length && a.every((id, i) => id === b[i]);

/** Owns only this flight's human round, failed-turn allowance and pending predecessors.
 * Reclaim neither restores nor infers any of these relationships.
 */
export class PlanRevisionRecovery {
  private source?: Source;
  private active?: Active;
  private predecessors: number[] = [];
  private readonly taken = new Set<number>();
  private readonly limit: number;
  private readonly sleep: NonNullable<PlanRevisionRetryPolicy["sleep"]>;

  constructor(private readonly deps: Dependencies) {
    const redrives = deps.policy?.redrives;
    this.limit = typeof redrives === "number" && Number.isFinite(redrives)
      ? Math.max(0, Math.min(3, Math.floor(redrives))) : 3;
    this.sleep = deps.policy?.sleep ?? (async (ms, signal) => { await delay(ms, undefined, { signal }); });
  }

  /** A same-gate Claude reclaim can retain omitted payload fields from the server's
   * immutable current presentation. This seeds identity only, never old retry state.
   */
  seed(request: StateRequest, revision: number, report: Report): void {
    if (positive(revision) && positive(this.deps.generation))
      this.source = { request: structuredClone(request), revision, report };
  }

  /** A question answer must reach running before this revision's next ownership probe. */
  get hasActiveRevision(): boolean {
    return this.active !== undefined;
  }

  accept(verdict: PlanVerdict): void {
    if (verdict.kind === "revise" && !verdict.automatic && this.source &&
        positive(verdict.sourceGateRevision) && verdict.sourceGateRevision !== this.source.revision)
      throw new PlanRevisionRecoveryStoppedError("stale feedback cannot replace the active plan revision");
    if (verdict.kind !== "revise" || verdict.automatic || !this.source ||
        !positive(verdict.inputId) || verdict.sourceGateRevision !== this.source.revision) {
      this.active = undefined;
      return;
    }
    if (this.taken.has(verdict.inputId))
      throw new PlanRevisionRecoveryStoppedError("duplicate plan revision cannot restart its driver");
    this.taken.add(verdict.inputId);
    this.active = { id: verdict.inputId, source: this.source, used: 0, redriveReady: false };
  }

  fields(inputId: number | undefined): Partial<StateRequest> {
    const a = this.active;
    if (a && inputId !== a.id)
      throw new PlanRevisionRecoveryStoppedError("publication does not answer the active plan revision");
    return a ? {
      expected_gate_revision: a.source.revision,
      revision_input_id: a.id,
      superseded_revision_input_ids: [...this.predecessors],
    } : {};
  }

  /** Called before human revision turns and at fresh epoch admission fences.
   * Repeated source probes spend an allowance only once, when redriveReady is set.
   * A successful clarification does not create a new allowance.
   */
  async beforeTurn(observer?: PlanRevisionOwnerWaitObserver, remainingWallMs?: () => number): Promise<PlanRevisionRetryOutcome> {
    const a = this.active;
    if (!a) return;
    try {
      await this.confirmSource(a, false, remainingWallMs);
      if (a.redriveReady) {
        if (a.used >= this.limit) {
          return this.retry(a.cause, observer, remainingWallMs);
        }
        const reason = a.cause instanceof TransientRecoveryError && a.cause.idleStallKind === "zero_evidence"
          ? a.cause.providerOrigin ? "silence after a provider-reported rate limit or transport retry" : "No model output within the idle timeout" : "Provider transient exhaustion";
        await this.deps.notice(`${reason} during plan revision ${a.id}; re-driving the failed turn (${a.used + 1}/${this.limit}).`);
        await this.confirmSource(a, false, remainingWallMs);
        a.used++;
        a.redriveReady = false;
      }
    } catch (error) {
      if (error instanceof PlanRevisionWallSpentError) return "wall";
      throw error;
    }
  }

  /** The runner admits typed provider exhaustion and measured Codex revision silence only. */
  async retry(cause: unknown, observer?: PlanRevisionOwnerWaitObserver, remainingWallMs?: () => number): Promise<PlanRevisionRetryOutcome> {
    const a = this.active;
    if (!a) throw cause; // missing source/older API keeps existing compatibility behavior
    a.cause = cause;
    try { return await this.retryOwned(a, cause, observer, remainingWallMs); }
    catch (error) {
      if (!(error instanceof PlanRevisionWallSpentError)) throw error;
      // An extension still needs a charged retry, or the exhausted owner wait.
      a.redriveReady = true;
      return "wall";
    }
  }

  private async retryOwned(a: Active, cause: unknown, observer?: PlanRevisionOwnerWaitObserver,
    remainingWallMs?: () => number): Promise<PlanRevisionRetryOutcome> {
    await this.confirmSource(a, false, remainingWallMs);
    // An id-less legacy presentation still has a source ownership fence, but
    // cannot use the exact-presentation expiry restore. Keep its existing recovery path.
    if (!a.source.request.presentation_id) throw cause;
    if (a.used < this.limit) {
      const random = this.deps.policy?.random?.() ?? Math.random();
      const jitter = Number.isFinite(random) ? Math.max(0, Math.min(1, random)) : 0;
      const backoff = Math.min(30_000, [5_000, 10_000, 20_000][a.used]! * (1 + jitter * 0.2));
      await this.wait(backoff, remainingWallMs);
      await this.confirmSource(a, false, remainingWallMs);
      a.redriveReady = true;
      return;
    }

    const restoreSourceGate = async (owned: RunOwnershipResponse): Promise<void> => {
    if (owned.status !== "awaiting_approval") {
      // No plan frame, fresh presentation, epoch or receipt settlement on restoration.
      const restore = { ...a.source.request, status: "awaiting_approval" as const,
        expected_gate_revision: a.source.revision };
      delete restore.adopt_gate_revision;
      delete restore.revision_input_id;
      delete restore.superseded_revision_input_ids;
      await this.publish(restore, a.source.report, remainingWallMs);
    }
    };
    await restoreSourceGate(await this.confirmSource(a, false, remainingWallMs));
    this.deps.steering.setGateRevision(a.source.revision);
    await this.deps.notice(
      `${a.cause instanceof TransientRecoveryError && a.cause.idleStallKind === "zero_evidence" ? a.cause.providerOrigin ? "silence after a provider-reported rate limit or transport retry. " : "No model output within the idle timeout. " : ""}Plan revision retry allowance exhausted (generation ${this.deps.generation}, source gate ${a.source.revision}, pending revision ${a.id}, ${this.limit} automatic re-drives in this flight). Feedback remains pending; no revised plan answering it has persisted. ` +
      "Use an ordinary revise bound to this source gate for a new, normally charged revision, if the server's PLAN_MAX_REVISIONS permits. If that cap is exhausted, no further revise/retry is available on this run. " +
      "Cancel and source-bound reject remain available. Approval and follow-up cannot resume this wait. For further changes after the cap, cancel/reject and dispatch a new run. This live gate remains busy and can defer a worker image roll until it ends or existing drain/shutdown policy acts.",
    );
    for (;;) {
      // Same-flight readoption or a delayed running report must not hide the
      // expired gate again. Retain only the captured source under the same fence.
      await restoreSourceGate(await this.confirmSource(a, false, remainingWallMs));
      // Source probes, restoration and notices are recovery work, not owner waiting.
      // Keep the exhausted readiness if an owner extension refuses the wall park:
      // beforeTurn must re-enter this wait, never admit an uncharged reconstruction.
      this.requireRemainingWall(remainingWallMs);
      let verdict: PlanVerdict | undefined = a.replacement;
      if (!verdict) {
        const abort = new AbortController();
        const signal = AbortSignal.any([this.deps.signal(), abort.signal]);
        try {
          try {
            observer?.(true);
            verdict = await Promise.race([
              this.deps.steering.awaitRevisionRecoveryEvent(a.source.revision, a.id, signal),
              this.sleep(30_000, signal).then(() => undefined),
            ]);
          } finally { observer?.(false); }
        } catch (err) {
          this.control(a);
          throw err;
        } finally {
          abort.abort(new Error("revision recovery wait tick ended"));
        }
      }
      if (!verdict) continue;
      if (verdict.kind === "cancel") { this.deps.checkControl(); throw new Error("run cancelled"); }
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason, a.source.revision);
      if (verdict.kind !== "revise" || verdict.automatic) continue;
      // A spent wall can unwind the source probe. Keep a taken replacement so an
      // extension resumes its proof, without taking feedback twice or losing it.
      a.replacement = verdict;
      await this.confirmSource(a, false, remainingWallMs);
      this.predecessors.push(a.id);
      this.accept(verdict);
      delete a.replacement;
      if (this.active && a.cause instanceof TransientRecoveryError && a.cause.idleStallKind === "zero_evidence") {
        // The replacement still needs a fresh epoch. Charge its first construction
        // at beforeTurn(), without retrying or restoring the exhausted predecessor.
        this.active.cause = a.cause;
        this.active.redriveReady = true;
      }
      throw new PlanRevisionReplacedError(verdict);
    }
  }

  /** Source-fenced sends retain one frozen body after unknown ACKs. A replay can
   * encounter the advanced revision it published, but cannot start provider work.
   */
  async publish(request: StateRequest, report: Report, remainingWallMs?: () => number): Promise<StateAck> {
    const a = this.active;
    if (!a || request.expected_gate_revision === undefined) return report(request);
    let attempted = false;
    for (;;) {
      await this.confirmSource(a, attempted, remainingWallMs);
      let ack: StateAck;
      try { attempted = true; ack = await report(request); }
      catch (err) {
        this.control(a);
        if (err instanceof PlanRevisionRecoveryStoppedError) throw err;
        if (err instanceof RevisionSourceFenceUnavailableError)
          throw new PlanRevisionRecoveryStoppedError("plan revision source-fence support is unavailable");
        if (err instanceof RequestError && err.status >= 400 && err.status < 500 && err.status !== 429)
          throw new PlanRevisionRecoveryStoppedError("source-fenced plan revision publication was refused");
        await this.waitForProof(remainingWallMs);
        continue;
      }
      if (ack.staleClaim || ack.staleRevisionSource)
        throw new PlanRevisionRecoveryStoppedError("plan revision publication lost its source gate or claim", ack.status);
      if (ack.applied === true && ack.status === "awaiting_approval" && positive(ack.gateRevision)) {
        if (request.revision_input_id !== undefined) {
          if (ack.revisionInputId !== request.revision_input_id ||
              !sameIDs(ack.supersededRevisionInputIds, request.superseded_revision_input_ids ?? [])) {
            await this.waitForProof(remainingWallMs); // receipt unknown: replay only, never infer success
            continue;
          }
        } else if (ack.gateRevision !== a.source.revision) {
          throw new PlanRevisionRecoveryStoppedError("source gate changed during revision restoration", ack.status);
        }
        return ack;
      }
      if (ack.status !== undefined && !["running", "awaiting_approval"].includes(ack.status))
        throw new PlanRevisionRecoveryStoppedError("plan revision publication stopped at " + ack.status, ack.status);
      await this.waitForProof(remainingWallMs);
    }
  }

  published(request: StateRequest, ack: StateAck, report: Report): void {
    if (!ack.applied || ack.status !== "awaiting_approval" || !positive(ack.gateRevision)) return;
    if (request.revision_input_id !== undefined) {
      if (ack.revisionInputId !== request.revision_input_id ||
          !sameIDs(ack.supersededRevisionInputIds, request.superseded_revision_input_ids ?? []))
        throw new PlanRevisionRecoveryStoppedError("revision persistence receipt is unconfirmed");
      this.deps.steering.retirePersistedRevision(ack.revisionInputId, ack.supersededRevisionInputIds!);
      if (ack.supersededRevisionInputIds!.length)
        void this.deps.notice(`Revision ${ack.revisionInputId}'s revised plan persisted; ${ack.supersededRevisionInputIds!.length} earlier pending revision(s) (${ack.supersededRevisionInputIds!.slice(0, 20).join(", ")}${ack.supersededRevisionInputIds!.length > 20 ? ", …" : ""}) were superseded, not successfully answered.`).catch(() => undefined);
      this.predecessors = [];
      this.active = undefined;
    }
    const retained = structuredClone(request);
    delete retained.expected_gate_revision;
    delete retained.revision_input_id;
    delete retained.superseded_revision_input_ids;
    delete retained.adopt_gate_revision;
    this.seed(retained, ack.gateRevision, report);
  }

  private control(a: Active): void {
    this.deps.checkControl(); // sticky cancel wins even over a queued reject
    const control = this.deps.steering.takeRevisionRecoveryControl(a.source.revision);
    if (control?.kind === "cancel") throw new Error("run cancelled");
    if (control?.kind === "reject") throw new PlanRejectedError(control.reason, a.source.revision);
  }

  private async wait(ms: number, remainingWallMs?: () => number): Promise<void> {
    while (ms > 0) {
      if (this.active) this.control(this.active);
      const slice = Math.min(250, ms, remainingWallMs?.() ?? Infinity);
      if (slice <= 0) break;
      try { await this.sleep(slice, this.deps.signal()); }
      catch (err) { if (this.active) this.control(this.active); throw err; }
      ms -= slice;
    }
    if (this.active) this.control(this.active);
  }

  private requireRemainingWall(remainingWallMs?: () => number): void {
    if (remainingWallMs !== undefined && remainingWallMs() <= 0) throw new PlanRevisionWallSpentError();
  }

  private async waitForProof(remainingWallMs?: () => number): Promise<void> {
    if (this.active) this.control(this.active);
    this.requireRemainingWall(remainingWallMs);
    await this.wait(250, remainingWallMs);
    this.requireRemainingWall(remainingWallMs);
  }

  private async confirmSource(a: Active, replay = false, remainingWallMs?: () => number): Promise<RunOwnershipResponse> {
    for (;;) {
      this.control(a);
      let owned: RunOwnershipResponse;
      try { owned = await this.deps.ownership(); }
      catch (err) {
        this.control(a);
        if (err instanceof RequestError && err.status === 404)
          throw new PlanRevisionRecoveryStoppedError("plan revision claim is no longer owned");
        await this.waitForProof(remainingWallMs);
        continue;
      }
      this.control(a);
      if ((positive(owned.claim_generation) && owned.claim_generation !== this.deps.generation) || owned.claim_released === true)
        throw new PlanRevisionRecoveryStoppedError("plan revision claim changed or was released", owned.status);
      if (typeof owned.status === "string" && !["running", "awaiting_approval"].includes(owned.status))
        throw new PlanRevisionRecoveryStoppedError("plan revision recovery stopped at " + owned.status, owned.status);
      if (positive(owned.gate_revision) && owned.gate_revision !== a.source.revision && !replay)
        throw new PlanRevisionRecoveryStoppedError("plan revision source gate changed", owned.status);
      this.requireRemainingWall(remainingWallMs);
      if (positive(owned.claim_generation) && positive(owned.gate_revision) &&
          owned.claim_released === false && ["running", "awaiting_approval"].includes(owned.status)) return owned;
      await this.waitForProof(remainingWallMs); // uncertainty grants neither a provider start nor a write
    }
  }
}
