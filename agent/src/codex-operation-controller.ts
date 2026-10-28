import { setTimeout as delay } from "node:timers/promises";
import { CodexOperationAdmissionError } from "./client.js";
import type { CodexOperationDecision, CodexOperationIdentity, CodexOperationRequest } from "./protocol.js";

export interface CodexOperationWaitPolicy {
  now?: () => number;
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}

interface Dependencies extends CodexOperationWaitPolicy {
  supported(): boolean;
  /** Changes on every positive negotiation, including a negotiation later cleared. */
  negotiation(): number;
  read(request: CodexOperationRequest, signal: AbortSignal): Promise<CodexOperationDecision>;
  waiting(operation: CodexOperationIdentity): (() => void) | undefined;
  engage(): void;
}

/** One flight/generation owns this scheduler. Only deadlines are shared, never admits.
 * Each read is bounded by the client's HTTP deadline; failures defer every sibling
 * to a completion-relative deadline. Aborted callers release their queue slot only
 * after any outstanding transport settles, so late replies cannot overlap a new read.
 */
export class CodexOperationController {
  private engaged = false;
  private readonly initialNegotiation: number;
  private tail: Promise<void> = Promise.resolve();
  private nextReadAt = 0;
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal: AbortSignal) => Promise<void>;

  constructor(private readonly generation: number, private readonly deps: Dependencies) {
    this.initialNegotiation = deps.negotiation();
    this.now = deps.now ?? Date.now;
    this.sleep = deps.sleep ?? (async (ms, signal) => { await delay(ms, undefined, { signal }); });
    this.observe();
  }

  private observe(): boolean {
    if (!this.engaged && (this.deps.supported() || this.deps.negotiation() !== this.initialNegotiation)) {
      this.engaged = true;
      this.deps.engage();
    }
    return this.engaged;
  }

  active(): boolean { return this.observe(); }

  async admit(operation: CodexOperationIdentity, signal: AbortSignal, check: () => void,
    prove: () => Promise<void>): Promise<() => boolean> {
    check();
    signal.throwIfAborted();
    if (!this.observe()) return () => !this.observe();
    const request = { ...structuredClone(operation), claim_generation: this.generation };
    // Join the queue once per read, letting independent operations progress between retries.
    for (;;) {
      let release!: () => void;
      const prior = this.tail;
      this.tail = new Promise<void>(resolve => { release = resolve; });
      let clear: (() => void) | undefined;
      let pendingRead: Promise<CodexOperationDecision> | undefined;
      try {
        await this.cancellable(prior, signal);
        check();
        signal.throwIfAborted();
        let remaining = this.nextReadAt - this.now();
        if (remaining > 0) {
          clear = this.deps.waiting(operation);
          // Recompute after an early timer wake. The minimum 1s deadline is set
          // after reads, not after wakeups, so a reached deadline always settles.
          while (remaining > 0) {
            await this.sleep(remaining, signal);
            check();
            signal.throwIfAborted();
            await prove();
            check();
            remaining = this.nextReadAt - this.now();
          }
        }
        let decision: CodexOperationDecision | undefined;
        let negotiation: number | undefined;
        try {
          if (this.deps.supported()) {
            negotiation = this.deps.negotiation();
            pendingRead = this.deps.read(request, signal).then(value => {
              if (value.decision === "wait") this.defer(value);
              return value;
            }, error => { this.nextReadAt = this.now() + 15_000; throw error; });
            decision = await this.cancellable(pendingRead, signal);
            // A worker re-registration or feature loss invalidates the old reply.
            if (!this.deps.supported() || negotiation !== this.deps.negotiation()) decision = undefined;
          }
        } catch (error) {
          // Even a cancelled/obsolete read establishes the shared unavailable
          // deadline. Only the two strict typed refusals leave the wait route.
          this.nextReadAt = this.now() + 15_000;
          if (error instanceof CodexOperationAdmissionError &&
              (error.reason === "authority_lost" || error.reason === "stale_source")) throw error;
        }
        // Set the schedule before checking caller currency: source churn cannot
        // erase an unsuccessful read's deadline.
        if (!decision) this.nextReadAt = this.now() + 15_000;
        else if (decision.decision === "wait") {
          this.defer(decision);
        }
        try {
          check();
          signal.throwIfAborted();
          await prove();
          check();
          signal.throwIfAborted();
        } catch (error) {
          // An admitted reply made obsolete by local control cannot bypass the
          // shared unavailable deadline when its replacement joins the queue.
          if (decision?.decision === "admit") this.nextReadAt = this.now() + 15_000;
          throw error;
        }
        if (decision?.decision === "admit") {
          let invalidated = false;
          const permit = (): boolean => {
            if (!invalidated && (!this.deps.supported() || negotiation !== this.deps.negotiation())) {
              invalidated = true;
              this.nextReadAt = this.now() + 15_000;
            }
            return !invalidated;
          };
          if (permit()) return permit;
        }
      } finally {
        clear?.();
        // A cancelled queued waiter must not release successors ahead of prior.
        void Promise.allSettled([prior, pendingRead]).then(release);
      }
    }
  }

  private defer(decision: Extract<CodexOperationDecision, { decision: "wait" }>): void {
    this.nextReadAt = this.now() + Math.max(1_000, Math.min(15_000,
      Math.max(0, Date.parse(decision.eligible_at) - Date.parse(decision.server_now))));
  }

  private async cancellable<T>(pending: Promise<T>, signal: AbortSignal): Promise<T> {
    signal.throwIfAborted();
    let onAbort!: () => void;
    const aborted = new Promise<never>((_, reject) => {
      onAbort = () => reject(signal.reason);
      signal.addEventListener("abort", onAbort, { once: true });
    });
    try { return await Promise.race([pending, aborted]); }
    finally { signal.removeEventListener("abort", onAbort); }
  }
}
