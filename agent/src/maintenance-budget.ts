import { AsyncLocalStorage } from "node:async_hooks";
import { performance } from "node:perf_hooks";

interface AttemptContext {
  identity?: string;
  budget?: MaintenanceBudget;
  factory?: () => MaintenanceBudget;
  activating?: boolean;
  pendingAbort?: AbortController;
  /** Producer clamps retain the original observation context for late cancellation. */
  producerParent?: AttemptContext;
}

const scope = new AsyncLocalStorage<AttemptContext>();

/** One attempt owns this object from its first cancellation observation through cleanup. */
export class MaintenanceBudget {
  readonly deadline: number;
  readonly signal: AbortSignal;
  private readonly controller = new AbortController();
  private timer: ReturnType<typeof setTimeout> | undefined;
  private readonly inherited: Array<{ signal: AbortSignal; abort: () => void }> = [];
  private inheritedBudget?: () => MaintenanceBudget | undefined;

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

  remainingMs(): number {
    return Math.max(0, Math.min(this.deadline - this.now(), this.inheritedBudget?.()?.remainingMs() ?? Infinity));
  }

  /** A shorter operation consumes this attempt's clock without renewing either bound. */
  cap(maxMs: number): () => number {
    if (!Number.isFinite(maxMs) && maxMs !== Infinity) return () => 0;
    const start = this.remainingMs();
    const floor = start - Math.min(Math.max(0, maxMs), start);
    return () => this.signal.aborted ? 0 : Math.max(0, this.remainingMs() - floor);
  }

  check(): void {
    this.inheritedBudget?.()?.check();
    if (this.remainingMs() <= 0 && !this.signal.aborted) {
      this.controller.abort(new Error("maintenance deadline exhausted"));
    }
    this.signal.throwIfAborted();
  }

  /** Nested scopes retain the attempt object; separate async chains remain isolated. */
  static run<T>(budget: MaintenanceBudget, action: () => T): T {
    const context = scope.getStore();
    const current = context?.budget ?? budget;
    current.check();
    if (context?.identity !== undefined) {
      this.attachBudget(context, current);
      return scope.run(context, action);
    }
    return scope.run({ budget: current }, action);
  }

  /**
   * Producer-only scope: owns its timer/listeners until the action settles, including
   * owned cleanup. No timeout race detaches the action. Ordinary run keeps its semantics.
   */
  static async runProducer<T>(
    action: (budget: MaintenanceBudget) => Promise<T>,
    options: { durationMs?: number; signal?: AbortSignal; now?: () => number } = {},
  ): Promise<T> {
    const parent = scope.getStore();
    const duration = Math.min(120_000, options.durationMs ?? 120_000, parent?.budget?.remainingMs() ?? Infinity);
    const child = new MaintenanceBudget(duration, options.now);
    child.inheritedBudget = () => parent?.budget;
    const signals = [parent?.pendingAbort?.signal, parent?.budget?.signal, options.signal];
    for (const signal of signals) {
      if (!signal) continue;
      const abort = () => child.controller.abort(signal.reason);
      if (signal.aborted) abort();
      else { signal.addEventListener("abort", abort, { once: true }); child.inherited.push({ signal, abort }); }
    }
    try {
      child.check();
      return await scope.run({ budget: child, identity: parent?.identity, producerParent: parent }, async () => {
        const result = await action(child);
        child.check();
        return result;
      });
    } finally { child.dispose(); }
  }

  /** Attach the flight's observation before synchronous abort listeners continue. */
  static attachObserved(identity: string, budget: MaintenanceBudget): void {
    let context = scope.getStore();
    while (context?.producerParent) context = context.producerParent;
    if (context?.identity === identity && !context.budget) this.attachBudget(context, budget);
  }

  /** Status transport is independent of preservation; never use this for FINAL or cleanup. */
  static withoutAttempt<T>(action: () => T): T { return scope.exit(action); }

  static isAttempt(identity: string): boolean {
    const context = scope.getStore();
    return context?.identity === identity || (!!context?.budget && context.identity === undefined);
  }

  /** Install before any await: late observations update every continuation in this attempt. */
  static runAttempt<T>(identity: string, factory: () => MaintenanceBudget, action: () => T): T {
    const parent = scope.getStore();
    if (parent?.identity === identity || (parent?.budget && parent.identity === undefined)) {
      parent.budget?.check();
      return action();
    }
    return scope.run({ identity, factory, pendingAbort: new AbortController() }, action);
  }

  /** Observation constrains time only; it grants no custody authority. */
  static observeCancellation(): MaintenanceBudget | undefined {
    const context = scope.getStore();
    if (!context) return undefined;
    if (context.producerParent) scope.run(context.producerParent, () => this.observeCancellation());
    if (!context.budget && context.factory && !context.activating) {
      context.activating = true;
      try { this.attachBudget(context, context.factory()); }
      finally { context.activating = false; }
    }
    context.budget?.check();
    return context.budget;
  }

  private static attachBudget(context: AttemptContext, budget: MaintenanceBudget): void {
    if (context.budget === budget) return;
    context.budget = budget;
    const relay = context.pendingAbort;
    if (!relay) return;
    const abort = () => relay.abort(budget.signal.reason);
    if (budget.signal.aborted) abort();
    else budget.signal.addEventListener("abort", abort, { once: true });
  }

  /** Stable from attempt entry; obtaining it never starts the cancellation clock. */
  static signal(): AbortSignal | undefined {
    const context = scope.getStore();
    return context?.pendingAbort?.signal ?? context?.budget?.signal;
  }

  /** Only inert queue waits: callers must preserve their slot behind earlier holders. */
  static async waitFor<T>(pending: Promise<T>, operationSignal?: AbortSignal): Promise<T> {
    this.current()?.check();
    const ambient = this.signal();
    const signal = operationSignal && ambient ? AbortSignal.any([ambient, operationSignal]) : operationSignal ?? ambient;
    if (!signal) return pending;
    signal.throwIfAborted();
    let onAbort!: () => void;
    const cancelled = new Promise<never>((_resolve, reject) => {
      onAbort = () => reject(signal.reason);
      signal.addEventListener("abort", onAbort, { once: true });
    });
    try {
      const value = await Promise.race([pending, cancelled]);
      this.current()?.check();
      signal.throwIfAborted();
      return value;
    } finally { signal.removeEventListener("abort", onAbort); }
  }

  static current(): MaintenanceBudget | undefined { return scope.getStore()?.budget; }

  /** Timer cleanup never renews the absolute deadline. */
  dispose(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    for (const { signal, abort } of this.inherited) signal.removeEventListener("abort", abort);
    this.inherited.length = 0;
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

const retainedSwitchSettlement = new AsyncLocalStorage<true>();

/**
 * issue #3181: marks the async chain of the retained credential switch's settlement, and only
 * that chain. SdkExecutor.reapAttributedProcesses bounds its joins by the ambient MaintenanceBudget
 * only while this marker is set, so every other budgeted caller (cancellation and terminal
 * maintenance run under a budget too) keeps the unbounded join.
 */
export const RetainedSwitchSettlement = {
  run<T>(action: () => T): T { return retainedSwitchSettlement.run(true, action); },
  active(): boolean { return retainedSwitchSettlement.getStore() === true; },
};
