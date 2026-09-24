// issue #1597 M2 — the per-flight SINK GATE: an async mutex that serialises every path that moves a
// run's durable state (the checkpoint closure, the pause/wall/completion parks and the credential-
// switch give-up) against the MID-TURN checkpoint tick.
//
// Why a gate: the tick fetches the runner clone back into the worker bare and publishes it while the
// agent's turn is live. A park/switch path commits a throwaway `wip(park):` marker, publishes (or
// undoes) it and moves the checkpoint floor; a tick interleaving there could fetch and publish the
// marker the path is about to undo, or race the floor. So:
//   - gated paths `run(fn)`: they WAIT for the gate. If a TICK holds it, they PREEMPT it (abort the
//     tick's controller) and still wait until the tick has fully settled (its children exited, its
//     publish rejected, its lock custody done) — the tick releases only after all of that.
//   - the tick `tryAcquire(preempt)`: non-blocking; a miss is the tick outcome `gate_busy`.
//
// Re-entrant for the OWNER: a gated path that (directly or through a callee) reaches another gated
// path runs it inline instead of deadlocking on itself. Ownership is tracked with AsyncLocalStorage,
// so it follows the owner's async call tree — but AsyncLocalStorage context also flows into timers
// and promises CREATED inside run() that fire after it returned. So the store carries a HOLD token
// that is marked dead on release: a deferred call from a finished holder sees a dead token and
// queues for the gate like anyone else (issue #1597 M2 review item 10).

import { AsyncLocalStorage } from "node:async_hooks";

interface Hold {
  readonly gate: SinkGate;
  live: boolean;
}

const owned = new AsyncLocalStorage<readonly Hold[]>();

export class SinkGate {
  private locked = false;
  private holderPreempt: (() => void) | undefined;
  private readonly waiters: Array<() => void> = [];

  /** Whether the gate is currently held (by a gated path or a tick). */
  isHeld(): boolean {
    return this.locked;
  }

  /** Run `fn` holding the gate, waiting (and preempting a holding tick) as needed. Re-entrant for
   *  the async call tree that already holds it. */
  async run<T>(fn: () => Promise<T>): Promise<T> {
    const store = owned.getStore() ?? [];
    if (store.some((h) => h.gate === this && h.live)) return fn();
    await this.lock();
    const hold: Hold = { gate: this, live: true };
    try {
      return await owned.run([...store.filter((h) => h.live), hold], fn);
    } finally {
      hold.live = false;
      this.unlock();
    }
  }

  /** Non-blocking acquire for the tick. Returns a release function, or null when the gate is held or
   *  a gated path is already queued for it. `preempt` is called (at most once) when a gated path
   *  wants the gate while this holder has it. */
  tryAcquire(preempt: () => void): (() => void) | null {
    if (this.locked || this.waiters.length > 0) return null;
    this.locked = true;
    let fired = false;
    this.holderPreempt = () => {
      if (fired) return;
      fired = true;
      preempt();
    };
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.unlock();
    };
  }

  private async lock(): Promise<void> {
    if (!this.locked) {
      this.locked = true;
      this.holderPreempt = undefined;
      return;
    }
    // A tick holds it: ask it to stop. A gated holder has no preempt and is simply waited for.
    this.holderPreempt?.();
    await new Promise<void>((resolve) => this.waiters.push(resolve));
    this.holderPreempt = undefined;
  }

  private unlock(): void {
    this.holderPreempt = undefined;
    const next = this.waiters.shift();
    if (next) next(); // hand off: the gate stays locked for the next waiter
    else this.locked = false;
  }
}
