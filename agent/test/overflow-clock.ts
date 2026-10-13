import assert from "node:assert/strict";
import type { TestContext } from "node:test";

export const NATIVE_LIMIT = 2_147_483_646;
export const REPRO_DELAY = 2_246_483_000;

/** Selective delivery lets watchdog tests omit unrelated idle timers and sample polling once.
 * All requested delays are recorded; overflow really becomes 1ms, as in Node.
 */
export function overflowClock(t: TestContext) {
  let native = 0;
  let monotonic = 100;
  let wall = 1_800_000_000_000;
  const timers = new Set<{
    callback: () => void; delay: number; due: number; interval: boolean; unrefs: number;
    unref(): unknown;
  }>();
  const requests: Array<{ delay: number; interval: boolean; unrefs: number }> = [];
  const schedule = (callback: () => void, delay = 0, interval = false) => {
    const timer = {
      callback, delay, interval, unrefs: 0,
      due: native + (delay > 2_147_483_647 || delay < 1 || !Number.isFinite(delay) ? 1 : Math.trunc(delay)),
      unref() { this.unrefs++; return this; },
    };
    requests.push(timer);
    timers.add(timer);
    return timer as unknown as NodeJS.Timeout;
  };
  t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => schedule(callback, delay));
  t.mock.method(globalThis, "clearTimeout", (timer: unknown) => timers.delete(timer as never));
  t.mock.method(globalThis, "setInterval", (callback: () => void, delay?: number) => schedule(callback, delay, true));
  t.mock.method(globalThis, "clearInterval", (timer: unknown) => timers.delete(timer as never));
  t.mock.method(Date, "now", () => wall);
  const move = (ms: number, wallMs = ms, monotonicMs = ms) => { native += ms; monotonic += monotonicMs; wall += wallMs; };
  return {
    now: () => monotonic,
    requests,
    move,
    jumpWall: (ms: number) => { wall += ms; },
    defaultClock: () => t.mock.method(performance, "now", () => monotonic),
    deliver: (predicate: (delay: number, interval: boolean) => boolean = () => true) => {
      // Snapshot: each wake can schedule a new chunk, which belongs to the next delivery.
      const pending = [...timers];
      for (const timer of pending) {
        if (timer.due > native || !predicate(timer.delay, timer.interval)) continue;
        if (timer.interval) timer.due = native + timer.delay;
        else timers.delete(timer);
        timer.callback();
      }
    },
    pending: () => [...timers],
    assertSafe: (predicate: (delay: number, interval: boolean) => boolean = () => true) => {
      for (const timer of requests.filter(r => predicate(r.delay, r.interval))) {
        assert.ok(timer.delay >= 0 && timer.delay <= NATIVE_LIMIT, `unsafe native delay: ${timer.delay}`);
      }
    },
  };
}

/** Bounded microtask drain; no native timers or elapsed wall time. */
export async function drain() {
  for (let i = 0; i < 100; i++) await Promise.resolve();
}
