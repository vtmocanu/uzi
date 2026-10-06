// issue #2213 — the worker-wide residue quarantine latch.
//
// A runner-uid process whose env and cwd cannot be read (a non-dumpable survivor) and that
// nothing ties to another live attempt may read the environment of every child this worker
// starts afterwards: the forge PAT in a git child's env, a provider credential in a Claude
// CLI's env. On a single-uid worker nothing can kill or contain it, so the worker latches: it
// claims nothing, starts no forge-credentialed git child and no new provider turn, until its
// container restarts (the only release: this module has no production reset).
//
// The latch is process-wide, in memory, and set only on a single-uid Linux worker for the one
// cause `unreadable_unattributed`. Every enforcement site calls the synchronous
// {@link assertResidueQuarantineOpen} immediately before the credential-bearing child or
// provider dispatch, so nothing awaits between the check and the start.

import { createLogger, type Logger } from "./log.js";
import { sanitizeForLog, type ProcessQuiescence } from "./run-quiescence.js";
import { uidSplitActive } from "./runner-uid.js";

/** issue #1783: the failure-reason prefix of a run whose finalize boundary could not prove the
 *  clone quiescent (a run-owned process survived the reap, or its attribution was unverifiable),
 *  or whose canonical reseed could not free the canonical clone path (CloneResidueBlockedError).
 *  failOriginForReason maps it to the fail_origin `worker_residue_blocked`. */
export const REASON_WORKER_RESIDUE_BLOCKED = "worker_residue_blocked";

/**
 * issue #1783: the run's clone is not provably quiescent at a boundary that must not proceed
 * without it. At finalize it fails the run (fail_origin `worker_residue_blocked`, the clone kept);
 * at a limit/wall park or a completion hold it means the credentialed sink body was skipped
 * (nothing published, the park stands); at a pause park it means no checkpoint and no park (the
 * pause reports pause_failed and the run continues, Decision 8); at the graceful-shutdown sink it
 * means nothing published (the requeue stands); at a milestone checkpoint it means that
 * checkpoint's publish is skipped (the run continues).
 */
export class RunResidueBlockedError extends Error {
  readonly detail: string;
  /** `preClone` marks a pre-fetch check, which fails before any clone exists: its text must not
   *  claim a clone was kept, and it names the check that actually refused (`home_reap`: the run's
   *  HOME-attributed reap; `worker_wide`: the worker-wide detection scan). The reason prefix is the
   *  same, so failOriginForReason maps every shape. */
  constructor(detail: string, opts: { preClone?: "home_reap" | "worker_wide" } = {}) {
    // The detail reaches the run's failure_reason: short, and stripped of control/bidi characters.
    const clean = sanitizeForLog(detail, 160);
    super(
      opts.preClone === "worker_wide"
        ? `${REASON_WORKER_RESIDUE_BLOCKED}: a process that could not be attributed to any run could not be proven gone by the worker-wide check before the clone fetch (${clean}); no clone was fetched`
        : opts.preClone === "home_reap"
          ? `${REASON_WORKER_RESIDUE_BLOCKED}: the run's HOME-attributed processes could not be proven gone by the HOME reap before the clone fetch (${clean}); no clone was fetched`
          : `${REASON_WORKER_RESIDUE_BLOCKED}: the run's clone could not be proven quiescent (${clean}); the clone is kept for inspection`,
    );
    this.detail = clean;
    this.name = "RunResidueBlockedError";
  }
}

/** Where a quarantine refusal fired (logs only). */
export type ResidueQuarantineRefusalSite = "git" | "provider_turn" | "claim";

/**
 * issue #2213: an action that needs a forge or provider credential was refused because the worker
 * is quarantined. A RunResidueBlockedError, so every `instanceof` site (push fallbacks, checkpoint
 * skips, park marking, canonical interruption) treats it as the residue block it is; the message
 * prefix maps it to fail_origin `worker_residue_blocked`.
 */
export class ResidueQuarantinedError extends RunResidueBlockedError {
  readonly site: ResidueQuarantineRefusalSite;
  constructor(site: ResidueQuarantineRefusalSite, cause: string) {
    super(cause);
    // RunResidueBlockedError set the generic message; replace it with the quarantine wording.
    this.message = `${REASON_WORKER_RESIDUE_BLOCKED}: this worker is quarantined (${this.detail}); no credentialed git or provider turn may start until the worker restarts`;
    this.name = "ResidueQuarantinedError";
    this.site = site;
  }
}

/** A latched worker's state: what the heartbeat reports. */
export interface ResidueQuarantineState {
  /** The sanitized detail of the unattributed process (at most 160 characters). */
  cause: string;
  /** The run whose check detected it, when any. */
  runId: string | undefined;
  /** The detection site (`pre_clone`, a quiescence site name, `review_pre_fetch`). */
  site: string;
  /** RFC 3339. */
  latchedAt: string;
}

let latched: ResidueQuarantineState | undefined;

/** Max characters of the stored cause. */
const CAUSE_MAX = 160;

/**
 * Latch the worker. The first latch wins; a later one is only logged. Logs once at error level.
 * The caller decides eligibility (single-uid Linux, `unreadable_unattributed`): see
 * {@link latchOnUnattributedUnreadable}.
 */
export function latchResidueQuarantine(
  input: { cause: string; runId?: string; site: string },
  log: Logger = createLogger(),
  now: Date = new Date(),
): void {
  const cause = sanitizeForLog(input.cause, CAUSE_MAX);
  const site = sanitizeForLog(input.site, 64);
  if (latched !== undefined) {
    log.warn("worker residue quarantine already latched; extra detection ignored", { site, cause });
    return;
  }
  latched = { cause, runId: input.runId, site, latchedAt: now.toISOString() };
  log.error(
    "worker residue quarantine latched: an unattributable runner-uid process may read credentials; claiming nothing and starting no credentialed git or provider turn until the worker restarts",
    { run_id: input.runId, site, cause },
  );
}

/** The latch, or undefined while the worker is not quarantined. */
export function residueQuarantine(): ResidueQuarantineState | undefined {
  return latched;
}

/**
 * Synchronously refuse when the worker is quarantined. Call it immediately before the
 * credential-bearing child or provider dispatch: nothing may await between the call and the start.
 */
export function assertResidueQuarantineOpen(site: ResidueQuarantineRefusalSite): void {
  if (latched !== undefined) throw new ResidueQuarantinedError(site, latched.cause);
}

/** True when a git child's env carries the forge credential: any `GIT_CONFIG_VALUE_n` that is a
 *  Basic Authorization header (gitEnv's PAT pair). Keyed on the env, not on the spawn identity,
 *  which defaults to `worker_pat` for PAT-less git too. */
function carriesForgeCredential(childEnv: NodeJS.ProcessEnv): boolean {
  return Object.entries(childEnv).some(
    ([k, v]) => k.startsWith("GIT_CONFIG_VALUE_") && typeof v === "string" && v.startsWith("Authorization: Basic"),
  );
}

/** Refuse (synchronously, typed) to start a child that carries the forge credential while the worker
 *  is quarantined. Call it immediately before the spawn, with nothing awaited in between. */
export function assertNoCredentialedGitWhileQuarantined(childEnv: NodeJS.ProcessEnv): void {
  if (carriesForgeCredential(childEnv)) assertResidueQuarantineOpen("git");
}

/**
 * Latch when a quiescence verdict lists a process with reason `unreadable_unattributed`, and only
 * on a single-uid Linux worker (under the uid split the solitary-kill path and the runner-uid
 * boundary already contain it; off Linux there is no process table to trust). Detection only: the
 * blocking semantics of the verdict are the caller's, unchanged.
 */
export function latchOnUnattributedUnreadable(
  verdict: ProcessQuiescence | undefined,
  ctx: { runId?: string; site: string; log?: Logger },
): boolean {
  if (verdict === undefined || process.platform !== "linux" || uidSplitActive()) return false;
  if (!verdict.processes.some((p) => p.reason === "unreadable_unattributed")) return false;
  latchResidueQuarantine({ cause: verdict.detail, runId: ctx.runId, site: ctx.site }, ctx.log);
  return true;
}

/** TEST ONLY: clear the latch (the production latch has no release but a restart). A source scan in
 *  test/run-quiescence-view-source-scan.test.ts forbids any agent/src reference to this name. */
export function resetResidueQuarantineForTests(): void {
  latched = undefined;
}
