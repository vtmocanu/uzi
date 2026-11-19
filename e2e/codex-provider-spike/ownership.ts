import type { ChildProcess } from "node:child_process";

/** Install at spawn, before exit can race disposal. Each owned child is killed once, then reaped. */
export function ownChild(child: ChildProcess): () => Promise<void> {
  let closed = false;
  const settlement = new Promise<void>(resolve => child.once("close", () => { closed = true; resolve(); }));
  return async () => {
    if (!closed && child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await settlement;
  };
}
