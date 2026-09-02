// Shared test-only fixtures. Imported by test files (like test-setup.ts's exported
// helpers); not part of the app bundle.

// A promise whose resolution we control, so overlapping loads can be ordered — the
// controllable-promise fixture a hook-consumer test needs to observe the loading /
// stale-guard window between two in-flight fetches.
export interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (err: unknown) => void;
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}
