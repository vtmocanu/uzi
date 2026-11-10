// Issue #3644 Gate B (m4a): the disk governor's cache target.
//
// The governor measures, trims and drops a run's rebuildable caches. Those callbacks used to close over the claim's initial
// executor and `runHome`; once a restore swaps the run onto another executor and HOME, a callback that still closes over the
// old pair would write the predecessor's HOME. The target is therefore ONE bindable pair, `{executor, home}`:
//   - every governor callback takes a single snapshot of it through {@link CacheTargetBinding.use} and uses that snapshot for
//     the whole operation (measurement, executor reclaim and the dropRunCaches fallback alike);
//   - unbound means refuse: nothing is measured, trimmed or dropped;
//   - {@link CacheTargetBinding.drainThenBind} swaps the pair only once no callback still holds a snapshot of the old one.
// This module is pure state; the runner decides when to bind (claim generation 1 at registration, a legacy-routed retained
// claim after discovery, the restore route never before its swap).

export interface CacheTarget<E> { readonly executor: E; readonly home: string }

export class CacheTargetBinding<E> {
  #target: CacheTarget<E> | undefined;
  #draining = false;
  #inFlight = 0;
  #idle: Array<() => void> = [];

  /** The bound HOME, or undefined while unbound (or while a drain is pending, so the governor skips the run). */
  home(): string | undefined {
    return this.#draining ? undefined : this.#target?.home;
  }

  /** True when a target is bound and no swap is draining. */
  get bound(): boolean { return !this.#draining && this.#target !== undefined; }

  /** Number of operations currently holding a snapshot (observability for tests and the swap). */
  get inFlight(): number { return this.#inFlight; }

  /** Bind immediately. Only for the moments nothing can hold the old target: registration and legacy routing. */
  bind(target: CacheTarget<E>): void {
    if (this.#draining) throw new Error("cache target swap in progress");
    this.#target = { executor: target.executor, home: target.home };
  }

  /** Replace the bound executor only (the claim's executor is refreshed after the clone phase); no-op while unbound. */
  refreshExecutor(executor: E): void {
    if (this.#target !== undefined) this.#target = { executor, home: this.#target.home };
  }

  /** Unbind immediately; later operations refuse. An operation already holding a snapshot finishes on it. */
  unbind(): void { this.#target = undefined; }

  /**
   * Run `operation` with ONE snapshot of the target, counted in flight for its whole duration. Resolves undefined, running
   * nothing, while unbound or draining.
   */
  async use<T>(operation: (target: CacheTarget<E>) => Promise<T>): Promise<T | undefined> {
    if (this.#draining || this.#target === undefined) return undefined;
    const snapshot = this.#target;
    this.#inFlight++;
    try {
      return await operation(snapshot);
    } finally {
      if (--this.#inFlight === 0) {
        const waiting = this.#idle;
        this.#idle = [];
        for (const wake of waiting) wake();
      }
    }
  }

  /**
   * The swap helper: refuse new operations, wait until every operation holding the old target has finished, then bind
   * `target`. An abort while waiting restores the previous binding and rejects, so a stuck operation can never leave the run
   * half-swapped.
   */
  async drainThenBind(target: CacheTarget<E>, signal?: AbortSignal): Promise<void> {
    if (this.#draining) throw new Error("cache target swap in progress");
    this.#draining = true;
    try {
      while (this.#inFlight > 0) {
        await new Promise<void>((resolve, reject) => {
          if (signal?.aborted) { reject(signal.reason ?? new Error("aborted")); return; }
          const onAbort = (): void => {
            this.#idle = this.#idle.filter(w => w !== wake);
            reject(signal?.reason ?? new Error("aborted"));
          };
          const wake = (): void => { signal?.removeEventListener("abort", onAbort); resolve(); };
          this.#idle.push(wake);
          signal?.addEventListener("abort", onAbort, { once: true });
        });
      }
      this.#target = { executor: target.executor, home: target.home };
    } finally {
      this.#draining = false;
    }
  }
}
