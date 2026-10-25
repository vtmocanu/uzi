import { createRequire } from "node:module";
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
  /** M1 callers leave firstMode unset. Exclusive is reserved for the future verdict pass. */
  acquire(options?: { firstMode?: "shared" | "exclusive"; signal?: AbortSignal }): Promise<PodEpochOwnership>;
  seed(podUid: string, signal?: AbortSignal): Promise<"updated" | "busy" | "unsupported" | "invalid">;
  prepare(): string;
  hold(expected: string, fd?: number, signal?: AbortSignal): Promise<void>;
}

// The exact same implementation runs before Node worker startup without tsx.
const baked = createRequire(import.meta.url)("../templates/pod-epoch-init.cjs") as {
  createRuntime(options?: RuntimeOptions): PodEpochRuntime;
};

export const createPodEpoch = baked.createRuntime;

/** Call only after acquiring epoch ownership, before ensureRunnerTmpdir or writers. */
export function restoreEpochRuntimeTmpdir(
  ownership: PodEpochOwnership,
  environment: NodeJS.ProcessEnv = process.env,
  io: typeof fs = createRequire(import.meta.url)("node:fs") as typeof fs,
): void {
  // Consult the live handle, including unsupported-storage participants.
  if (!["shared", "exclusive", "unsupported"].includes(ownership.mode)) {
    throw new Error("epoch ownership required");
  }
  const saved = environment.UZI_EPOCH_RUNTIME_TMPDIR;
  const split = environment.UZI_UID_SPLIT === "1";
  const expected = "/data/runner/.tmp" + (split ? "/uzi-worker" : "");
  if (saved !== expected) throw new Error("invalid saved epoch runtime TMPDIR");
  for (const directory of ["/data", "/data/runner", "/data/runner/.tmp", expected]) {
    const stat = io.lstatSync(directory);
    if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("unsafe runtime TMPDIR ancestor");
    const mode = stat.mode & 0o7777;
    if (directory === expected && (stat.uid !== 10001 || stat.gid !== 10001 ||
        (split ? mode !== 0o700 : ![0o755, 0o3775].includes(mode)))) {
      throw new Error("unsafe runtime TMPDIR metadata");
    }
  }
  environment.TMPDIR = saved;
  delete environment.UZI_EPOCH_RUNTIME_TMPDIR;
  // Entrypoint rejects caller-supplied npm configuration before installing these
  // bootstrap-only values. Do not override the per-run HOME/cache of later children.
  for (const key of ["npm_config_cache", "npm_config_userconfig", "npm_config_globalconfig",
    "npm_config_logs_max", "npm_config_update_notifier", "npm_config_ignore_scripts", "TSX_DISABLE_CACHE"]) {
    delete environment[key];
  }
}
