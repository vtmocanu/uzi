import * as fs from "node:fs";
import { createRuntime } from "../templates/pod-epoch-init.cjs";
import type { createRuntime as CreateRuntime, PodEpochOwnership } from "../templates/pod-epoch-init.d.cts";

export type { PodEpochOwnership } from "../templates/pod-epoch-init.d.cts";

// Load the shared plain CJS implementation before the main bootstrap snapshot.
export const createPodEpoch: typeof CreateRuntime = createRuntime;

/** Call only after acquiring epoch ownership, before ensureRunnerTmpdir or writers. */
export function restoreEpochRuntimeTmpdir(
  ownership: PodEpochOwnership,
  environment: NodeJS.ProcessEnv = process.env,
  io: typeof fs = fs,
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
