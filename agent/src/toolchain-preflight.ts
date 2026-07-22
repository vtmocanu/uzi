import fs from "node:fs";
import path from "node:path";
import { runnerPath } from "./runner-uid.js";

// PRD #92 M3 — boot-time toolchain preflight (the runtime analogue of the M1/M2 build
// assertion). A worker whose `/nix` store is missing the baked toolchain (go/python3/
// gcc/pip/openssl) must FAIL REGISTRATION visibly instead of surfacing `command not found`
// (exit 127) to subagents mid-run. Root cause (see PRD #92): the `seed-nix` init
// container tars the image's `/nix` into the per-worker PVC exactly once, so a worker
// rolled onto a toolchain-changing image runs against a store that never re-seeded —
// its PATH points at a profile hash the stale store lacks.
//
// The check resolves the five tools against `runnerPath(env)` (UZI_RUNNER_PATH || PATH),
// NOT `process.env.PATH`: under the PRD #51 A1 split the worker's own PATH is
// deliberately stripped of `/nix`, and the full image PATH (incl. `/nix`) is handed to
// the runner via UZI_RUNNER_PATH — so checking process.env.PATH would false-fail on
// every correctly-hardened worker. On a #58 single-uid start UZI_RUNNER_PATH is unset
// and runnerPath() falls back to the (unstripped) worker PATH, so the same check holds.

/** The baked worker toolchain every subagent depends on (PRD #83/#89; openssl added
 *  as a broadly-used base crypto/TLS CLI, judge rec run dd06c0ed). */
export const REQUIRED_TOOLS = ["python3", "go", "gcc", "pip", "openssl"] as const;

/** The stable, immutable toolchain handle baked by M1 (`/opt/uzi-toolchain` → the
 *  realized devbox global profile in the store). If this dereference fails the store is
 *  stale — exactly the stranded-PVC case the preflight exists to catch. */
export const STABLE_TOOLCHAIN_PATH = "/opt/uzi-toolchain";

export interface PreflightResult {
  ok: boolean;
  /** The tools not found on the runner PATH, plus the stable-path sentinel if it does
   *  not resolve. Empty ⇒ ok. */
  missing: string[];
}

/**
 * Pure toolchain preflight: no throwing, no process.exit, no logging — so it unit-tests
 * cleanly and the caller decides how to fail loud. Resolves each of REQUIRED_TOOLS to an
 * executable file on `runnerPath(env)` (first hit wins) and asserts `stablePath`
 * dereferences (follows the symlink into the store). Any tool with no hit, and the
 * stable path if it does not resolve, land in `missing`.
 *
 * @param env       the environment to read the runner PATH from (default process.env).
 * @param stablePath the immutable toolchain handle to assert (default
 *   `/opt/uzi-toolchain`); overridable so tests can point it at a temp symlink and stay
 *   hermetic (not dependent on the host having `/opt/uzi-toolchain`).
 */
export function toolchainPreflight(
  env: NodeJS.ProcessEnv = process.env,
  stablePath: string = STABLE_TOOLCHAIN_PATH,
): PreflightResult {
  const missing: string[] = [];
  const dirs = (runnerPath(env) ?? "").split(":").filter((d) => d.length > 0);

  for (const tool of REQUIRED_TOOLS) {
    const found = dirs.some((dir) => {
      try {
        fs.accessSync(path.join(dir, tool), fs.constants.X_OK);
        return true;
      } catch {
        return false;
      }
    });
    if (!found) missing.push(tool);
  }

  // statSync follows the symlink, so a dangling `/opt/uzi-toolchain → <store>` (the
  // stranded-PVC signature: the profile path is absent from the seeded store) throws
  // and is reported, while a healthy image passes.
  try {
    fs.statSync(stablePath);
  } catch {
    missing.push(stablePath);
  }

  return { ok: missing.length === 0, missing };
}
