export function ownedDeadline(expire: () => void, timers: {
  set: (callback: () => void, ms: number) => unknown;
  clear: (handle: unknown) => void;
} = { set: (cb, ms) => setTimeout(cb, ms), clear: h => clearTimeout(h as NodeJS.Timeout) }): () => void {
  const handle = timers.set(expire, 25000);
  return () => timers.clear(handle);
}
