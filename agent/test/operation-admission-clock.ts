import assert from "node:assert/strict";
import { setImmediate as immediate } from "node:timers/promises";

export async function flushUntil(ready: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 1000; attempt++) {
    if (ready()) return;
    await immediate();
  }
  assert.fail("event barrier did not settle");
}

/** Explicit wakeups, including deliberately early wakeups; no real cooldown sleeps. */
export class AdmissionClock {
  now = 0;
  readonly sleepers = new Set<{ at: number; wake(): void }>();
  readonly sleep = (ms: number, signal: AbortSignal): Promise<void> => {
    signal.throwIfAborted();
    return new Promise((resolve, reject) => {
      const finish = (): void => {
        this.sleepers.delete(entry);
        signal.removeEventListener("abort", abort);
      };
      const entry = { at: this.now + ms, wake: () => { finish(); resolve(); } };
      const abort = (): void => { finish(); reject(signal.reason); };
      signal.addEventListener("abort", abort, { once: true });
      this.sleepers.add(entry);
    });
  };
  advance(ms: number): void {
    this.now += ms;
    for (const sleeper of this.sleepers) if (sleeper.at <= this.now) sleeper.wake();
  }
  early(): void { for (const sleeper of this.sleepers) sleeper.wake(); }
}
