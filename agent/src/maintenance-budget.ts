import { AsyncLocalStorage } from "node:async_hooks";
import { performance } from "node:perf_hooks";

const scope = new AsyncLocalStorage<MaintenanceBudget>();

/** One attempt owns this object from its first cancellation observation through cleanup. */
export class MaintenanceBudget {
  readonly deadline: number;
  readonly signal: AbortSignal;
  private readonly controller = new AbortController();
  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(
    durationMs = 600_000,
    private readonly now: () => number = () => performance.now(),
  ) {
    if (!Number.isFinite(durationMs) || durationMs < 0) throw new Error("invalid maintenance budget");
    this.deadline = now() + durationMs;
    this.signal = this.controller.signal;
    this.timer = setTimeout(() => this.controller.abort(new Error("maintenance deadline exhausted")), durationMs);
    this.timer.unref();
  }

  remainingMs(): number { return Math.max(0, this.deadline - this.now()); }

  check(): void {
    if (this.remainingMs() <= 0 && !this.signal.aborted) {
      this.controller.abort(new Error("maintenance deadline exhausted"));
    }
    this.signal.throwIfAborted();
  }

  /** Nested scopes retain the attempt object; separate async chains remain isolated. */
  static run<T>(budget: MaintenanceBudget, action: () => T): T {
    const current = scope.getStore() ?? budget;
    current.check();
    return scope.run(current, action);
  }

  static current(): MaintenanceBudget | undefined { return scope.getStore(); }

  /** Timer cleanup never renews the absolute deadline. */
  dispose(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
  }

  /**
   * Only for inert RPC awaits, never owned subprocesses, filesystem work or maintenance.
   * One late-result cleanup attempt is detached and best effort; it cannot block siblings.
   * The caller must supply a bounded/nonblocking cleanup (for example body.cancel()).
   */
  async raceIO<T>(action: (signal: AbortSignal) => Promise<T>, cancelLate?: (value: T) => void | Promise<void>, operationSignal?: AbortSignal): Promise<T> {
    this.check();
    const signal = operationSignal ? AbortSignal.any([this.signal, operationSignal]) : this.signal;
    signal.throwIfAborted();
    let aborted = false;
    let cleaned = false;
    const cleanup = (value: T): void => {
      if (cleaned || !cancelLate) return;
      cleaned = true;
      void Promise.resolve().then(() => cancelLate(value)).catch(() => undefined);
    };
    let onAbort!: () => void;
    const stopped = new Promise<never>((_resolve, reject) => {
      onAbort = () => { aborted = true; reject(signal.reason); };
      signal.addEventListener("abort", onAbort, { once: true });
    });
    const pending = Promise.resolve().then(() => { this.check(); signal.throwIfAborted(); return action(signal); });
    void pending.then(value => {
      if (aborted) cleanup(value);
    }, () => undefined);
    try {
      const value = await Promise.race([pending, stopped]);
      try { this.check(); signal.throwIfAborted(); } catch (error) { cleanup(value); throw error; }
      return value;
    } finally {
      signal.removeEventListener("abort", onAbort);
    }
  }
}
