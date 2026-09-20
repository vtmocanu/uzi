// PRD #1493 M3 — honest advertisement of the `codex_harness_v1` PROTOCOL capability.
//
// A worker may advertise Codex ONLY when all three preconditions hold:
//   1. the installer receipt is intact (probeCodexRuntime — kept UNCHANGED, no-exec);
//   2. the worker→runner→runner-cmd uid split is active (uidSplitActive());
//   3. Landlock is available, OR the command sandbox is in `best-effort` mode on a
//      kernel that reports Landlock UNAVAILABLE (ENOSYS/EOPNOTSUPP) — in which case
//      commands run without filesystem confinement and the worker is "degraded".
//
// A Landlock ERROR (any other errno / ABI < 1) or a failed probe is FATAL even in
// best-effort (D8), so it must NOT advertise — advertising it would produce a
// claimed-then-failed run, the exact dishonesty this milestone removes.
//
// This wrapper spawns `uzi-codex-command-sandbox --probe`, so it lives in its OWN
// module: it must never be imported into codex-runtime-probe.ts, whose no-exec /
// no-network import contract (fs/crypto/path only) is asserted by a test.

import { spawnSync, type SpawnSyncReturns } from "node:child_process";

import type { CommandSandboxMode } from "../config.js";

/** The image-baked command-sandbox binary. Same fixed path the executor launches
 *  (codex-executor.ts's COMMAND_SANDBOX_BIN); duplicated here as a plain literal so
 *  this startup wrapper does not pull the whole executor module into main.ts. */
const COMMAND_SANDBOX_BIN = "/usr/local/bin/uzi-codex-command-sandbox";

/**
 * The `--probe` exit-code contract, kept in lockstep with
 * `agent/codex/supervisor/cmdsandbox/main.go`'s probeExit* constants:
 *   0  → Landlock available (ABI >= 1)
 *   10 → Landlock unavailable (ENOSYS / EOPNOTSUPP)
 *   11 → probe error (any other errno, or an ABI below 1)
 */
const PROBE_EXIT_AVAILABLE = 0;
const PROBE_EXIT_UNAVAILABLE = 10;
const PROBE_EXIT_ERROR = 11;

/** The Landlock probe outcome. `probe-failed` is distinct from the sandbox's own
 *  `error` classification: it means the `--probe` process could not be run at all
 *  (binary missing, spawn error, timeout, or an unexpected exit code). */
export type LandlockProbeOutcome = "available" | "unavailable" | "error" | "probe-failed";

/** The resolved advertisement decision. `advertise` gates `codex_harness_v1`;
 *  `degraded` is true only when advertising in best-effort on a Landlock-unavailable
 *  kernel (commands run unconfined); `reason` is a bounded, credential-free
 *  diagnostic set only when NOT advertising (safe to log). */
export interface CodexHarnessAvailability {
  advertise: boolean;
  degraded: boolean;
  landlock: LandlockProbeOutcome;
  reason?: string;
}

/** Run the one-shot Landlock version probe by invoking the command-sandbox binary
 *  with `--probe`. Never throws; a spawn failure / timeout / unknown code all map to
 *  `probe-failed`. The seam args are injectable so a unit test drives every outcome
 *  without a real binary. */
export function probeLandlockAvailability(
  bin: string = COMMAND_SANDBOX_BIN,
  spawn: (command: string, args: readonly string[]) => Pick<SpawnSyncReturns<Buffer>, "status" | "error"> =
    (command, args) => spawnSync(command, [...args], { stdio: "ignore", timeout: 10_000 }),
): LandlockProbeOutcome {
  let res: Pick<SpawnSyncReturns<Buffer>, "status" | "error">;
  try {
    res = spawn(bin, ["--probe"]);
  } catch {
    return "probe-failed";
  }
  if (res.error || res.status === null || res.status === undefined) return "probe-failed";
  switch (res.status) {
    case PROBE_EXIT_AVAILABLE:
      return "available";
    case PROBE_EXIT_UNAVAILABLE:
      return "unavailable";
    case PROBE_EXIT_ERROR:
      return "error";
    default:
      return "probe-failed";
  }
}

/** Inputs to {@link resolveCodexHarnessAvailability} — the three combined signals
 *  plus the worker's configured sandbox mode. */
export interface ResolveCodexHarnessInput {
  /** probeCodexRuntime(...).capable — the installer-receipt integrity result. */
  readonly receiptCapable: boolean;
  /** probeCodexRuntime(...).reason — the bounded diagnostic when not capable. */
  readonly receiptReason?: string;
  /** uidSplitActive() — whether the worker/runner/runner-cmd split is established. */
  readonly uidSplit: boolean;
  /** The worker-configured command-sandbox mode (config.codexCommandSandbox). */
  readonly mode: CommandSandboxMode;
  /** The Landlock probe outcome (from {@link probeLandlockAvailability}). */
  readonly landlock: LandlockProbeOutcome;
}

/**
 * Combine the receipt probe, uid-split state and Landlock probe with the configured
 * mode into a single advertisement decision. Pure and total (never throws) so it is
 * fully unit-testable; each failing precondition yields a DISTINCT `reason`.
 */
export function resolveCodexHarnessAvailability(input: ResolveCodexHarnessInput): CodexHarnessAvailability {
  const { receiptCapable, receiptReason, uidSplit, mode, landlock } = input;
  if (!receiptCapable) {
    return { advertise: false, degraded: false, landlock, reason: `codex receipt not intact: ${receiptReason ?? "not capable"}` };
  }
  if (!uidSplit) {
    return {
      advertise: false,
      degraded: false,
      landlock,
      reason: "uid split not active (UZI_UID_SPLIT unset); Codex requires the worker/runner/runner-cmd split",
    };
  }
  if (landlock === "available") {
    return { advertise: true, degraded: false, landlock };
  }
  // Landlock is not available. Only best-effort on a Landlock-UNAVAILABLE kernel may
  // advertise (commands run unconfined). An error/probe-failure stays fatal even in
  // best-effort (D8), so it does not advertise.
  if (mode === "best-effort" && landlock === "unavailable") {
    return { advertise: true, degraded: true, landlock };
  }
  const reason = landlock === "unavailable"
    ? "landlock unavailable and command sandbox mode is required"
    : `landlock probe ${landlock} is fatal even in best-effort; not advertising codex_harness_v1`;
  return { advertise: false, degraded: false, landlock, reason };
}
