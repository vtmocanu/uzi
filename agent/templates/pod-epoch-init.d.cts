import type * as fs from "node:fs";
import type { spawn, StdioOptions } from "node:child_process";

type HelperResult = { code: number; output: string };
type OwnershipMode = "shared" | "exclusive" | "unsupported";

export interface PodEpochOwnership {
  readonly mode: OwnershipMode;
  /** Worker runtime capability; absent on legacy shadow-only implementations. */
  beginReclaim?(): PodEpochReclaimScope;
  consume(podUid: string): Promise<{
    result: "fresh" | "existing" | "full";
    mayEvaluate: boolean;
  }>;
  /** Corroboration only, and never true without exclusive epoch ownership. */
  matchesEntry(current: string, previous: string): boolean;
  /** Admission stays closed until this resolves; closes EX before opening SH. */
  reacquireShared(): Promise<void>;
  close(): void;
}

type MutationStdio = Exclude<StdioOptions, string>;

/** Worker-only capability. Never expose it or decorated stdio to an executor. */
export interface PodEpochReclaimScope {
  /** Rechecks live EX ownership and the original held inode; failure stops forever. */
  assertExclusive(): void;
  readonly pendingMutations: number;
  /** Start before a mutation; settle only once its effects/children are definitive. */
  beginMutation(): { settle(): void };
  /**
   * Trusted native spawn seam only; requires a pending ticket.
   * Copies initial stdio (at least stdin/out/err), appending the SAME owned open
   * description. Settle a ticket only after the complete mutating descendant
   * tree is gone and all in-process/persistence effects are positively joined.
   * A direct child's close or EOF alone is not settlement evidence.
   * Never put decorated stdio in logs, env, API responses or executor options.
   */
  inheritStdio(stdio?: Readonly<MutationStdio>): MutationStdio;
  /** Irreversible uncertainty latch; only process exit releases ownership. */
  stop(): void;
  stopped(): boolean;
  assertSettled(): void;
  /** Requires no pending work and no stop; restores ordinary close/SH handover. */
  finish(): void;
}

/** Overrides are for hermetic tests; production uses /data, uid/gid10001 and /usr/bin/flock. */
interface RuntimeOptions {
  root?: string;
  uid?: number;
  gid?: number;
  io?: typeof fs;
  spawnProcess?: typeof spawn;
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<unknown>;
  run?: (args: string[], fd: number | undefined, signal: AbortSignal | undefined, deadline: number) => Promise<HelperResult>;
}

interface PodEpochRuntime {
  /** Read-only advisory snapshot; never grants evaluation or admission authority. */
  inspectConsumption(podUid: string): "fresh" | "existing" | "full" | "uncertain";
  /** Choose the first mode before acquisition; durable consume remains mandatory. */
  acquire(options?: { firstMode?: "shared" | "exclusive"; signal?: AbortSignal }): Promise<PodEpochOwnership>;
  seed(podUid: string, signal?: AbortSignal): Promise<"updated" | "busy" | "unsupported" | "invalid">;
  prepare(): string;
  hold(expected: string, fd?: number, signal?: AbortSignal): Promise<void>;
}

/** The image-baked plain CJS implementation is shared with pre-Node initialization. */
export function createRuntime(options?: RuntimeOptions): PodEpochRuntime;
