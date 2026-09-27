/**
 * PRD #1809 D7: the per-run lock the runner and the disk reclaim share, so a resume never
 * starts while the reclaim is deleting that run's files, and the reclaim never deletes
 * while a claim is setting that run's HOME up.
 *
 * An in-process keyed async mutex: one FIFO queue per run id, nothing shared between run
 * ids, and an idle key holds no memory. It is process memory on purpose: the runner and the
 * reclaim that delete and recreate a run's HOME are both in this worker process, and no
 * other worker process shares this data volume: it is one worker process per data dir
 * (disk-reclaim.ts's model-pass age bound relies on the same).
 *
 * The lock alone does not make a deletion safe; it closes the check-then-delete window. The
 * reclaim takes the lock, re-checks under it that the runner is not executing the run, and
 * only then deletes. The runner registers the run as executing synchronously on entry and
 * then takes the lock before it builds the run's executor, so either the reclaim sees the
 * run as executing and skips it, or the runner waits for the reclaim's deletion to finish
 * and recreates what it needs afterwards.
 */
export class RunDiskLocks {
  /** runId -> the promise the NEXT acquirer waits on (the current holder's release). */
  private readonly tails = new Map<string, Promise<void>>();

  /**
   * Wait for every earlier holder of `runId` to release, then hold it. Returns the release
   * function; calling it more than once is harmless. Never rejects.
   */
  async acquire(runId: string): Promise<() => void> {
    const previous = this.tails.get(runId) ?? Promise.resolve();
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    // Installed synchronously, so a third acquirer queues behind this one even while this
    // one is still waiting for the first.
    const tail = previous.then(() => held);
    this.tails.set(runId, tail);
    await previous;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      release();
      // Nobody queued behind this holder: drop the key so idle runs cost nothing.
      if (this.tails.get(runId) === tail) this.tails.delete(runId);
    };
  }

  /** Run `fn` while holding `runId`'s lock; the lock is released however `fn` settles. */
  async withLock<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    const release = await this.acquire(runId);
    try {
      return await fn();
    } finally {
      release();
    }
  }
}
