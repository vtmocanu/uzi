/** Relative deadlines use a monotonic clock; native timers never receive an overflowing delay. */
export function chunkedTimeout(
  callback: () => void,
  delayMs: number,
  options: { now?: () => number; unref?: boolean } = {},
): { cancel(): void } {
  const now = options.now ?? (() => performance.now());
  const deadline = now() + delayMs;
  let finished = false;
  let timer: ReturnType<typeof setTimeout>;
  const arm = (): void => {
    const remaining = deadline - now();
    timer = setTimeout(wake, Math.min(2_147_483_646, Math.max(0, Math.ceil(remaining))));
    if (options.unref) timer.unref?.();
  };
  const wake = (): void => {
    if (finished) return;
    if (now() < deadline) {
      arm();
      return;
    }
    finished = true;
    callback();
  };
  arm();
  return {
    cancel(): void {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
    },
  };
}
