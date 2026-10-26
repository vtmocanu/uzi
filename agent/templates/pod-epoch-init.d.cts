import type * as fs from "node:fs";
import type { spawn } from "node:child_process";

type HelperResult = { code: number; output: string };
type OwnershipMode = "shared" | "exclusive" | "unsupported";

export interface PodEpochOwnership {
  readonly mode: OwnershipMode;
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
