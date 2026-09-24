import { AsyncResource } from "node:async_hooks";
import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import type { WorkerClient } from "./client.js";
import { RequestError } from "./client.js";
import type { GitCache, RunnerClone, CheckpointOverlayContext, CheckpointRange } from "./git.js";
import {
  gitBasicCredential,
  isNonFastForwardRejection,
  isPushProtectionRejection,
  isWorkflowScopeRejection,
} from "./git.js";
import type { SecretFinding } from "./secret-scan-guard.js";
import type { Executor, ExecutorResult, RunContext, WallParkOutcome, WallParkRefresh } from "./executor.js";
import { PlanRejectedError } from "./executor.js";
import type { BoundaryPermit, BoundaryRequest } from "./harness.js";
import { SinkGate } from "./sink-gate.js";
import {
  TickSpawner,
  processGroupAlive,
  signalProcessGroup,
  type RetainedLock,
  type SurvivingGroup,
  type TickSpawnerTestHooks,
} from "./tick-spawner.js";
import { skillsPluginDir } from "./skills-plugin.js";
import { describeLimit, LimitReachedError } from "./limit.js";
import type { Logger } from "./log.js";
import type {
  ActiveSnapshotPhase,
  AgentSelection,
  AgentSource,
  AgentTemplate,
  AskUserQuestion,
  ClaimCodexSecrets,
  ClaimConfig,
  ClaimResponse,
  IterationBudget,
  Milestone,
  RunOrphanClassificationResponse,
  StateAck,
  StateRequest,
} from "./protocol.js";
import { resolveAgentSelection } from "./protocol.js";
import type { ActiveRunRegistry } from "./active-run-registry.js";
import { deriveCloneKey, resolveRunKind, RUN_KIND_PROFILES } from "./run-kind.js";
import { RecoveryCoordinator, isCodePublishingKind, type RecoveryRecord } from "./recovery.js";
import {
  PUSHED_HEAD_UNRECORDED,
  PredecessorSettler,
  SettlementJournal,
  isSafeSettlementId,
  type RecoverySettleClient,
  type SettlementRecord,
} from "./recovery-settlement.js";
import {
  describeRepoAgentNote,
  detectRepoAgents,
  repoAgentSummaries,
  type DetectedRepoAgents,
} from "./repoagents.js";
import { MessageBatcher } from "./batcher.js";
import type { Outbox } from "./outbox.js";
import {
  installTerminalWriteAhead,
  resolvePendingTerminal,
  sendUnjournaledTerminal,
  type SendTerminalState,
  type TerminalOutboxDeps,
} from "./terminal-resolve.js";
import { rmHomeTree } from "./rmtree.js";
import {
  SteeringChannel,
  PauseNowSignal,
  CredentialSwitchSignal,
  type AnswerVerdict,
  type PlanVerdict,
} from "./steering.js";
import { GitLabClient, ForgejoClient, GitHubClient, type ForgeClient } from "./forge.js";
import { classifyForgeError, withForgeRetry } from "./forge-retry.js";
import { makeRedactor, makeTextRedactor } from "./redact.js";
import { sessionTranscriptResolvable } from "./sdk-session.js";
import { errMessage, RUN_ID_RE, sleep } from "./util.js";
import {
  CHECKPOINT_SCAN_TIMEOUT_MS,
  CapturePathMismatchError,
  ForeignCaptureBlockedError,
  PendingRecoveryCaptureError,
} from "./git.js";
import {
  buildCheckEnv,
  defaultCheckRunner,
  flagGuardPaths,
  guardCriticalMrSection,
  runSelfImproveChecks,
  selfImproveMrSection,
  type CheckRunner,
} from "./self-improve.js";
import { installJsDeps } from "./js-deps.js";
import { detectToolchain, type ToolchainDetection } from "./toolchain-detect.js";
import { isCIConfigPlan } from "./prompt.js";
import { flagCIConfigPaths, DEFAULT_CI_CONFIG_PATHS } from "./ci-config-guard.js";
import { REASON_PROVISION_FAILED } from "./provision-run.js";
import { REASON_NO_TOKEN, TransientRecoveryError } from "./sdk-executor.js";
import { PLAN_MISSING_QUESTION, PLAN_MISSING_QUESTION_HEADER, REASON_PLAN_MISSING } from "./plan-missing.js";

/** Cap on a reported failure_reason, matching the forge error-body cap
 *  (forge.ts) so a runaway SDK error can't bloat the run row or the stream. */
const MAX_FAILURE_REASON_LEN = 512;

/** The three authoritative TERMINAL run statuses. A run in any of these can never
 *  write again, so its retained recovery clone is safe to reclaim. Used by the
 *  handleRecoveryExhausted loop and by phaseClone's foreign-capture probe (#1315). */
const TERMINAL_RUN_STATUSES = new Set(["cancelled", "completed", "failed"]);

/** PRD #1391 Run B M3 (D4): the phase stamped into a run-lane terminal journal (`phase_at_journal`,
 *  what #1390's snapshot reports for a pending outcome after a restart). Every run-lane terminal
 *  report is produced while the run is at the `running` (finalize) phase, so this is the honest
 *  value; kept as one constant so every site agrees. */
const TERMINAL_JOURNAL_PHASE = "running";

/** PRD #1391 Run B M4: the FIXED log line the queued-duplicate claim router emits when it ends the
 *  attempt WITHOUT executing (a terminal/held row, a different generation, a definitive not-owned, or
 *  an exhausted transient-probe budget). One constant so every end-path logs the same greppable
 *  message; the varying detail (run_id, reason, status) rides the structured fields. No terminal
 *  report is ever sent on these paths — the run keeps its authoritative status. */
const QUEUED_DUPLICATE_END_LOG = "queued duplicate claim ended without executing (ownership probe)";

/** PRD #1226 M4 (D6): bounded in-call attempts to capture a VERIFIED completion-hold restore
 *  point before giving up. Mirrors handleRecoveryExhausted's retain-and-retry, but bounded (this
 *  runs synchronously inside the executor's completion loop, not the server-parked recovery loop).
 *  A never-verified capture DOES NOT park: enterCompletionHold clears its preserve flags and returns
 *  false, so the run's normal terminal cleanup runs (no park, no leak). */
const COMPLETION_HOLD_CAPTURE_ATTEMPTS = 3;

/** PRD #1247 M5b (D3): bounded in-call attempts to capture a VERIFIED restore point before a
 *  held-state credential-switch RELEASE. Mirrors COMPLETION_HOLD_CAPTURE_ATTEMPTS — the same
 *  retain-and-retry, bounded because it runs synchronously in executeClaim's catch arm. A capture
 *  that never verifies GIVES UP: enterCredentialSwitch reports `credential_switch_failed`, KEEPS
 *  the preserve flags (no work loss), and returns "gave_up" so the run is left non-terminal for
 *  requeue on the still-standing override — the switch is realized at the reclaim boundary. */
const CREDENTIAL_SWITCH_CAPTURE_ATTEMPTS = 3;

/** PRD #1226 M4 (D5): the STATIC, content-free failure_reason a worker reports when the completion
 *  interlock cannot report completed AND cannot park the run for recovery (the restore point never
 *  verified or the hold ACK was refused). It carries NO model output and NO repository text — the
 *  taxonomy fact is all a human needs to route it, and the committed work is safe on the run's
 *  branch (the finalize push already landed before the permit was requested). */
const COMPLETION_INTERLOCK_UNHELD_REASON =
  "completion interlock: could not verify the final head or park the run for recovery";

/** PRD #1171 m4: a name-based CodexBoundaryError probe. The runner stays HARNESS-AGNOSTIC and
 *  never imports from agent/src/codex/**, so it recognizes the boundary-blocked error — thrown
 *  by the executor-owned safety facade when a durability sink could not reap/reconcile (sink
 *  counter zero, publication blocked) — by its `name`, not by `instanceof`. On a best-effort
 *  sink (park/shutdown/recovery) this lets the runner fold it into the existing publish-failure
 *  outcome; a terminal/finalize sink lets it propagate to the failed-run report. */
function isCodexBoundaryError(err: unknown): boolean {
  return err instanceof Error && err.name === "CodexBoundaryError";
}

/** issue #1597 M1: whether a CodexBoundaryError failed because its boundary DEADLINE elapsed (a
 *  `timeout`-category HarnessError among its `errors`). The runner stays harness-agnostic (no
 *  import from agent/src/codex/**), so `errors` is read defensively by shape: a non-boundary
 *  error, a missing/non-array `errors`, or malformed entries all read as "not a timeout". */
function isCodexBoundaryTimeout(err: unknown): boolean {
  if (!isCodexBoundaryError(err)) return false;
  const errors: unknown = (err as { errors?: unknown }).errors;
  if (!Array.isArray(errors)) return false;
  return errors.some(
    (e: unknown) =>
      typeof e === "object" && e !== null && (e as { category?: unknown }).category === "timeout",
  );
}

/** issue #1597 M1: the closed set of server best-effort skip labels a checkpoint publish can
 *  carry onto the run feed — the `Skipped:` values api/internal/workersvc/service.go returns
 *  (PublishCheckpoint). Anything else the body carries is untrusted free text and is folded to
 *  `other`, so no server-/forge-shaped string reaches the feed verbatim. */
const PUBLISH_SKIP_LABELS = ["unsupported", "no_ref", "not_descendant", "workflow_scope"] as const;
type PublishSkipLabel = (typeof PUBLISH_SKIP_LABELS)[number] | "other";

function publishSkipLabel(raw: unknown): PublishSkipLabel {
  return (PUBLISH_SKIP_LABELS as readonly unknown[]).includes(raw) ? (raw as PublishSkipLabel) : "other";
}

/** issue #1597 M1: why a checkpoint publish did not land. `no_local_tip` = the tracking tip was
 *  unresolved (no tracking ref, or it could not be read — trackingTip swallows a git failure to
 *  null), so checkpointPack had nothing to pack (silent), `skipped` = a 2xx best-effort server skip,
 *  `rejected` = a non-2xx, `aborted` = the caller's signal (permit/deadline/tick) aborted OR the throw
 *  was an AbortError even with no aborted signal (e.g. the client's own request timeout) — silent on
 *  the feed either way; the shutdown sink names the latter `publish_error`, since its permit did not
 *  expire — and `error` = any other throw. */
type PublishFailClass = "no_local_tip" | "skipped" | "rejected" | "aborted" | "error";

/** issue #1597 M1: the typed result of {@link RunRunner.publishCheckpointOutcome}. Only the
 *  allowlisted skip label and the numeric HTTP status are carried — never an error message,
 *  remote text or credential. */
type PublishOutcome =
  | { published: true }
  | { published: false; reason: Exclude<PublishFailClass, "skipped" | "rejected"> }
  | { published: false; reason: "skipped"; skipLabel: PublishSkipLabel }
  | { published: false; reason: "rejected"; httpStatus: number };

/** issue #1597 M1: the class a graceful-shutdown checkpoint ended in, named on the run feed and
 *  in the run log. Published WINS over a later boundary error. `no_local_tip` = the tracking tip
 *  was unresolved (no tracking ref, or it could not be read). issue #1597 M2 adds
 *  `bare_lock_retained`: the checkpoint did not land while a cancelled mid-turn tick had left a git
 *  lock file in the worker bare that could not be proven the tick's own (so it was kept), and
 *  `tick_process_survived`: it did not land while a cancelled tick's process group was still alive
 *  after its SIGKILL. The feed line prints the class verbatim. */
type ShutdownCheckpointOutcome =
  | "published"
  | "timeout"
  | "boundary_blocked"
  | "publish_rejected"
  | "publish_skipped"
  | "publish_error"
  | "no_local_tip"
  | "bare_lock_retained"
  | "tick_process_survived";

/** issue #1597 M1: map the shutdown body's publish result to its shutdown class. An `aborted`
 *  publish is a `timeout` when the permit/deadline signal is what aborted it, else a
 *  `publish_error`. An undefined result (the body never reached the publish) is `publish_error`. */
function shutdownOutcomeOf(
  outcome: PublishOutcome | undefined,
  permitSignal: AbortSignal | undefined,
): ShutdownCheckpointOutcome {
  if (!outcome) return "publish_error";
  if (outcome.published) return "published";
  switch (outcome.reason) {
    case "no_local_tip":
      return "no_local_tip";
    case "skipped":
      return "publish_skipped";
    case "rejected":
      return "publish_rejected";
    case "aborted":
      return permitSignal?.aborted ? "timeout" : "publish_error";
    case "error":
      return "publish_error";
  }
}

/** issue #1597 M2: the class one run of the checkpoint body ended in (the mid-turn tick logs it).
 *  `publish_failed:<reason>` carries the {@link PublishFailClass} of a publish that did not land. */
type CheckpointBodyOutcome =
  | "scan_deferred"
  | "published"
  | "time_gate_closed"
  | "no_new_work"
  | "secret_found"
  | "secret_scan_untrusted"
  | `publish_failed:${Exclude<PublishFailClass, "aborted">}`
  | "aborted";

/** issue #1597 M2: the class one MID-TURN checkpoint tick ended in, logged as
 *  runLog.info("mid-turn checkpoint tick", {outcome}). A local fetch-back alone is never
 *  `published` — only a publish the broker confirmed is. */
/** issue #1597 M2: how long a durable sink waits for a surviving tick process group before it
 *  proceeds anyway (and logs the residual). */
const SURVIVOR_SINK_WAIT_MS = 2_000;

type MidTurnTickOutcome =
  | CheckpointBodyOutcome
  | "git_busy"
  | "gate_busy"
  | "bare_lock_retained"
  | "tick_process_survived";

/** issue #1597 M2: the checkpoint body's options — ctx.checkpoint's plus the tick-only knobs. */
type CheckpointBodyOpts = Parameters<NonNullable<RunContext["checkpoint"]>>[0] & {
  /** No `running` reportState (the mid-turn tick). */
  quiet?: boolean;
  /** The tick's cancellation signal: checked between steps and handed to the publish RPC. */
  signal?: AbortSignal;
  /** Run the secret scan under a tighter sub-scope (the tick's 60s scan deadline). */
  scanScope?: <T>(fn: () => Promise<T>) => Promise<T>;
};

/** issue #1597 M2: the hard cap on one mid-turn tick (probe excluded): its signal aborts at this
 *  deadline, killing every child it spawned. */
const MIDTURN_TICK_TIMEOUT_MS = 120_000;
/** issue #1597 M2 (round 3): how soon a tick is kicked after a publish was deferred out of a Codex
 *  permit (`scan_deferred`), instead of waiting a full tick interval. */
const MIDTURN_KICK_DELAY_MS = 1_000;
/** issue #1597 M2: the sentinel a tick's pre-scope await resolves to when its controller aborted. */
const ABORTED: unique symbol = Symbol("aborted");
/** issue #1597 M2: consecutive git-busy ticks before the deferred line reaches the feed. */
const MIDTURN_BUSY_FEED_AFTER = 3;

function isAbortLikeError(err: unknown): boolean {
  return err instanceof Error && err.name === "AbortError";
}

/** PRD #974 follow-up (#1077): a terminal push_secret_blocked report whose reportState
 *  exhausted its bounded retries and threw. Carrying the typed origin + safe reason through
 *  execute()'s generic catch preserves fail_origin=push_secret_blocked (instead of defaulting
 *  to agent_failure) WITHOUT ever attaching preserved_patch. */
class TerminalReportError extends Error {
  constructor(
    readonly reason: string,
    readonly failOrigin: string,
    cause?: unknown,
  ) {
    super("terminal state report failed", { cause });
    this.name = "TerminalReportError";
  }
}

/**
 * PRD #1416 M3/M4 SEAM — thrown at the FINALIZE bridge sites (the align `fetchAndPush`; the plain
 * push handles the same outcome by a direct call+return) when the branch's history was rewritten
 * at/below the published floor P AND the ancestry bridge B could not be built or validated, so the
 * run cannot be landed with a fast-forward push. Defined and thrown from where
 * `bridgeBareTrackingRefIfDivergent` returns `{kind:"failed"}`. M4 CATCHES it — the align chain is
 * wrapped in a try/catch that routes it to `failHistoryRewritten` — so it is TYPED
 * `history_rewritten` with a scan-gated preserved_patch and NEVER reaches the generic failure path
 * (SC3). Carries P (the published floor) so the typed reason can name it. Local, like
 * TerminalReportError / StaleClaimError. The MID-RUN and PARK/CAPTURE bridge sinks must NEVER throw
 * it (best-effort — a park that loses a bridge is worse than one that fails, D4). */
class HistoryRewrittenError extends Error {
  constructor(readonly publishedTip: string) {
    super(
      `history rewritten at or below the published tip ${publishedTip}; a fast-forward bridge could not be built or validated`,
    );
    this.name = "HistoryRewrittenError";
  }
}

/** PRD #1416 (MR-rework, finding 1) — a data-free unwind sentinel: the post-bridge secret scan
 *  found a trusted secret in the now-pushable `P..B` range and has ALREADY terminally reported
 *  push_secret_blocked (via reportPushSecretBlocked). Thrown from the ALIGN chain's fetchAndPush so
 *  the enclosing align try/catch (which types HistoryRewrittenError) unwinds without pushing, and
 *  caught in the outer finalize catch to STOP rather than re-report. Local, like HistoryRewrittenError. */
class PushSecretBlockedSignal extends Error {
  constructor() {
    super("push_secret_blocked already reported by the post-bridge secret scan");
    this.name = "PushSecretBlockedSignal";
  }
}

/** PRD #1416 M3 — the outcome of {@link RunRunner.bridgeBareTrackingRefIfDivergent} at a
 *  publication boundary. "clean": P (and C) already ancestors of the tracking tip, nothing to do.
 *  "unknown": ancestry could not be read, so do NOT bridge and do NOT fail. "bridged": B was built
 *  + validated, the tracking ref advanced to B, and C advanced to B. "failed": divergent, but B
 *  could not be built or validated (the finalize sinks turn this into a HistoryRewrittenError; the
 *  mid-run/park/capture sinks log it and continue — best-effort, D4). */
type BridgeOutcome =
  | { kind: "clean" }
  | { kind: "unknown" }
  | { kind: "bridged"; bridge: string }
  | { kind: "failed" };

/**
 * PRD #1392 M2 — `ensureClone` exhausted `withForgeRetry` with a TRANSIENT verdict: the forge
 * was unreachable at clone/fetch (a DNS blip, a connection reset, a 5xx), not a permanent
 * rejection (401/403/404). `phaseClone` wraps the `ensureClone` throw in this so `executeClaim`'s
 * catch can PARK the run (`recovery_wait`, cause `forge_unreachable`) instead of failing it — the
 * pre-clone transient park. A PERMANENT verdict is NOT wrapped and still fails immediately. The
 * message carries only the (redacted-at-report) git/forge error text, never a secret.
 */
export class ForgeUnreachableAtCloneError extends Error {
  constructor(message: string, cause?: unknown) {
    super(message, { cause });
    this.name = "ForgeUnreachableAtCloneError";
  }
}

/**
 * PRD #1247 M5b: a /state report came back with the top-level `stale_claim` disposition
 * (`ack.staleClaim`) — a held-state credential switch RELEASED this claim, or a reclaim
 * SUPERSEDED it. This flight no longer owns the run, so it MUST STOP without reporting a
 * terminal state (another claim owns the run now) and without setting a preserve flag
 * (normal teardown — the new claim has its own clone). Thrown from the flight.reportState
 * closure the moment any report is answered stale, and caught in executeClaim's catch chain
 * BEFORE the generic terminal path (mirroring the PauseNowSignal arm). Local, like
 * TerminalReportError: it is thrown and caught entirely within this file.
 */
class StaleClaimError extends Error {
  constructor() {
    super("run claim superseded server-side (stale_claim)");
    this.name = "StaleClaimError";
  }
}

/**
 * PRD #1497 M2 (D5/D16): a fenced /state report came back stale AND the run is SERVER-PARKED at its
 * wall-clock limit — the ACK carried `disposition:"stale_claim"` (the fence rejected this flight's
 * report) together with the run's `status:"paused"` + `hold_reason:"budget_exhausted"` (the sweep
 * parked the row and armed claim_released_at). This is DISTINCT from a plain StaleClaimError: the
 * run was not superseded by a reclaim, it was parked at the wall by the server (a dead/incapable/
 * unresponsive worker's row, D5), so this flight must STOP but RETAIN everything — the run is
 * non-terminal and a safe incarnation will resume it from the last checkpoint. Thrown from the
 * flight.reportState closure the moment any report reads that pair, AHEAD of the StaleClaimError
 * throw; caught in executeClaim's catch chain BEFORE the StaleClaimError arm, where it sets the
 * preserve flags, closes the batcher, reports NOTHING terminal, and keeps clone + HOME. Local, like
 * StaleClaimError: thrown and caught entirely within this file.
 */
class ServerWallParkedError extends Error {
  constructor() {
    super("run parked server-side at its wall-clock limit (budget_exhausted); retaining work and stopping this flight");
    this.name = "ServerWallParkedError";
  }
}

/**
 * PRD #1390 M4 — the env-gated e2e DROP-EXECUTION seam signal. OFF unless the worker is
 * started with `UZI_E2E_DROP_ON_SENTINEL=1` (inert in production); when on, a claim whose
 * issue text carries {@link E2E_DROP_SENTINEL} is dropped right after its first `running`
 * report, BEFORE the clone (no worktree, no recovery journal), to model a live worker that
 * silently loses one execution: the flight ends with NO terminal report, so the run stays
 * `running` for the api's heartbeat missing-run requeue (M2b) to reclaim after the fence, the
 * snapshot entry is removed by the ordinary finally, and the claim loop is paused (via the
 * shared registry) so the e2e can observe the requeued run before a reclaim. Local, thrown
 * and caught entirely within this file, exactly like StaleClaimError. */
class E2EDropExecutionError extends Error {
  constructor() {
    super("e2e drop-execution seam: ending flight with no terminal report");
    this.name = "E2EDropExecutionError";
  }
}

/** PRD #1390 M4: the issue-text sentinel the e2e drop-execution seam keys on (only when
 *  UZI_E2E_DROP_ON_SENTINEL is set). Named like the stub sentinels; inert in production. */
const E2E_DROP_SENTINEL = "UZI_STUB_DROP";

/**
 * PRD #1247 M5b (BLOCKING-2/3 rework): a held-state credential switch could not be CONFIRMED, so
 * the flight must STOP without continuing in place — but, unlike StaleClaimError, it must RETAIN
 * all work (enterCredentialSwitch left the preserve flags SET). Thrown by ctx.attemptCredentialSwitch
 * when enterCredentialSwitch returns "retained_stop" — an unverified give-up whose stamp-clear the
 * server never confirmed, or a release whose outcome is unknown. Continuing in place would risk
 * working a claim the reclaim already owns (release) or stranding every buffered input behind a
 * still-pending switch stamp (give-up); so executeClaim's catch chain ends the flight NON-TERMINAL
 * (no failed report), keeping the clone + HOME so the sweeper's requeue lets a reclaim resume the
 * retained work. Local, like TerminalReportError / StaleClaimError.
 */
class CredentialSwitchRetainedStop extends Error {
  constructor() {
    super("credential switch unconfirmed; retaining work and stopping the flight");
    this.name = "CredentialSwitchRetainedStop";
  }
}

/**
 * PRD #1391 Run B M4: phaseClone's FIRST `running` report came back refused (applied:false) with a
 * TERMINAL status — the run reached completed/failed/cancelled out from under this claim (a racing
 * owner cancel, or an outcome that already landed). Continuing the phase clone would work a claim the
 * run has already left, so STOP: close the batcher, report NO terminal state (a `failed` here would
 * fight the authoritative terminal outcome), and set NO preserve flag (nothing was cloned yet). The
 * distinct `staleClaim` disposition is handled separately by the reportState closure's StaleClaimError
 * throw; this is the non-stale terminal case. Local, thrown and caught entirely within this file.
 */
class RunningAckTerminalError extends Error {
  constructor(readonly runStatus: string) {
    super(`first running report refused with a terminal status (${runStatus})`);
    this.name = "RunningAckTerminalError";
  }
}

/** Map a known failure-reason CONSTANT to the server's fail_origin enum (PRD #69
 *  M7a). Authored WORKER-SIDE from the reason constant the throw site used — it never
 *  parses free text: it matches only the fixed prefixes the two fatal pre-start
 *  throwers emit (provision-run appends `: <detail>` after REASON_PROVISION_FAILED;
 *  sdk-executor throws REASON_NO_TOKEN verbatim), and, by EXACT match, the static
 *  REASON_PLAN_MISSING both executors throw for a planning turn that stayed prose-only
 *  (issue #1593). Ordinary agent failures return undefined, so `fail_origin` is omitted
 *  and the server defaults them to 'agent_failure'. Sent unvalidated; the server
 *  allowlists it. */
export function failOriginForReason(rawReason: string): string | undefined {
  if (rawReason.startsWith(REASON_PROVISION_FAILED)) return "provisioning_failed";
  if (rawReason.startsWith(REASON_NO_TOKEN)) return "credential_unavailable";
  if (rawReason === REASON_PLAN_MISSING) return "plan_missing";
  return undefined;
}

/**
 * PRD #377 M1 — compose the actionable `failure_reason` for a GitHub run whose branch
 * touches `.github/workflows/**`, a path the bot's repo-only PAT cannot push. It names the
 * offending path(s) and points at `docs/github-bot-setup.md`.
 *
 * The path LIST is truncated to fit MAX_FAILURE_REASON_LEN (showing the first paths + an
 * "and N more" tail) — but the doc link, which lives in the fixed suffix, is NEVER cut. The
 * truncation math is done BEFORE assembly (against the budget left after the fixed prefix +
 * suffix), not by blindly slicing the whole string at the end. Exported for a direct
 * truncation unit test. The caller still applies `.slice(0, MAX_FAILURE_REASON_LEN)` as a
 * belt-and-braces net after the doc link is guaranteed to fit.
 */
export function composeWorkflowScopeReason(paths: string[]): string {
  const prefix =
    "This run's branch changes workflow files that uzi's GitHub bot token cannot push " +
    "(its scope is exactly `repo`, without `workflow`, by design): ";
  const suffix =
    ". The change is valid; land it as a human PR (commit the file yourself with a " +
    "workflow-scoped token). See docs/github-bot-setup.md. Your diff is preserved below.";
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  let list = paths.join(", ");
  if (list.length > budget) {
    // Drop trailing paths (replaced by an "and N more" tail) until the list fits the
    // budget. The doc link is in `suffix`, so it is untouched by this truncation.
    let shown = paths.length;
    for (; shown > 0; shown--) {
      const more = paths.length - shown;
      const candidate =
        paths.slice(0, shown).join(", ") + (more > 0 ? `, and ${more} more` : "");
      if (candidate.length <= budget) {
        list = candidate;
        break;
      }
    }
    if (shown === 0) {
      // Pathological: even one path overflows the budget (a single very long path). Keep a
      // hard-truncated first entry so the fixed suffix — and its doc link — still fits.
      list = paths[0]!.slice(0, Math.max(0, budget - 1)) + "…";
    }
  }
  return prefix + list + suffix;
}

/**
 * PRD #974 M2 — compose the actionable, capped `failure_reason` for a GitHub run whose branch
 * carries a secret GitHub Push Protection (GH013) would reject at push. It NAMES the offending
 * commit + path(s) (the first finding, plus an "and N more" tail like composeWorkflowScopeReason)
 * so a human knows exactly what to scrub, says the branch could not be pushed, and — because the
 * diff may carry the detected secret — states that the diff is withheld, pointing the owner at a
 * durable-recovery archive (`uzi run export`) when one exists.
 *
 * The variable part (the finding list) is truncated to fit MAX_FAILURE_REASON_LEN against the
 * budget left after the fixed prefix + suffix — the withheld-diff / recovery pointer in the suffix is
 * NEVER cut. The truncation math is done BEFORE assembly (mirroring composeWorkflowScopeReason),
 * not by slicing the whole string at the end. Exported for a direct cap unit test; the caller
 * still applies `.slice(0, MAX_FAILURE_REASON_LEN)` as a belt-and-braces net.
 */
export function composePushSecretBlockedReason(
  findings: SecretFinding[],
  forgeType?: string,
): string {
  // PRD #1416 (MR-rework, finding 1): the post-bridge scan runs on EVERY forge, so a non-GitHub
  // block must NOT cite "GitHub Push Protection"/"GH013" (there is no such backstop on
  // GitLab/Forgejo). GitHub (or an omitted forge — the top-of-finalize GH013 caller) keeps the
  // original wording so its existing callers + unit tests stay stable.
  const isGitHub = forgeType === undefined || forgeType === "github";
  const prefix = isGitHub
    ? "This run's branch could not be pushed: it carries a secret GitHub Push Protection blocks " +
      "(GH013). Offending: "
    : "This run's branch could not be pushed: the pre-push secret scan detected a secret (a " +
      "rewritten branch was bridged so it could publish). Offending: ";
  const suffix =
    ". The change is otherwise valid; a human can scrub the secret from the commit(s) and " +
    "land it. The diff is withheld because it may carry the detected secret; if a " +
    "durable-recovery archive of this run is available, export it with `uzi run export`.";
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  // One human-readable label per finding: `<short-commit> <path> (<rule>)`. The file path (and
  // rule id) come from gitleaks' report of ATTACKER-authored repo content, so a committed
  // filename can carry control bytes (ESC, newline) that would forge rows / inject ANSI when this
  // failure_reason is later rendered in a CLI/TUI terminal. Strip the C0 control range + DEL at
  // this WRITE site so the stored reason cannot carry them (the render boundary is defense in
  // depth, not the only guard).
  // eslint-disable-next-line no-control-regex
  const stripControl = (s: string): string => s.replace(/[\u0000-\u001f\u007f]/g, "");
  const labels = findings.map((f) => {
    const shortCommit = f.commit ? stripControl(f.commit.slice(0, 8)) : "?";
    const rule = f.ruleId ? ` (${stripControl(f.ruleId)})` : "";
    return `${shortCommit} ${stripControl(f.file)}:${f.startLine}${rule}`;
  });
  let list = labels.join("; ");
  if (list.length > budget) {
    // Drop trailing labels (replaced by an "and N more" tail) until the list fits the budget.
    let shown = labels.length;
    for (; shown > 0; shown--) {
      const more = labels.length - shown;
      const candidate =
        labels.slice(0, shown).join("; ") + (more > 0 ? `; and ${more} more` : "");
      if (candidate.length <= budget) {
        list = candidate;
        break;
      }
    }
    if (shown === 0) {
      // Pathological: even one label overflows the budget. Keep a hard-truncated first label so
      // the fixed suffix (and its withheld-diff / recovery pointer) still fits.
      list = (labels[0] ?? "").slice(0, Math.max(0, budget - 1)) + "…";
    }
  }
  return prefix + list + suffix;
}

/**
 * PRD #456 M2 — the actionable `failure_reason` for a GitHub run whose branch could not be
 * aligned with the current default branch before the finalize push: its `.github/workflows/**`
 * files are BEHIND the default (main advanced them after this run's clone base), the bot's
 * repo-only PAT cannot push while they differ, and uzi's attempt to merge and then rebase the
 * current default into the branch either BOTH conflicted, or the aligned branch could not be
 * pushed without rewriting already-published history (a non-fast-forward the bot cannot
 * force-push — NB2). The run fails without pushing and the agent's diff is preserved (#377's
 * `preserved_patch`) for a human to rebase-and-land.
 *
 * Names the default branch once and points at docs/github-bot-setup.md. The branch name is
 * the only variable part and is clamped against a computed budget (MAX_FAILURE_REASON_LEN
 * minus the fixed prefix + suffix lengths), so the fixed suffix — the doc link and the
 * "Your diff is preserved below." pointer — always fits MAX_FAILURE_REASON_LEN and is never
 * truncated. Exported for a direct length-cap unit test.
 */
export function composeBaseAlignConflictReason(defaultBranch: string): string {
  const db = defaultBranch || "the default branch";
  const prefix = "This run's branch is behind the default branch (";
  const suffix =
    ") on .github/workflows files, which uzi's GitHub bot token cannot push while they " +
    "differ from the default (its scope is `repo`, without `workflow`, by design). uzi tried " +
    "to merge then rebase the current default into the branch to realign those files, but could " +
    "not realign and safely push it, so the run failed without pushing. The work is valid; a " +
    "human can rebase and land it. See docs/github-bot-setup.md. Your diff is preserved below.";
  // Clamp the branch name (the only variable part) against the budget left after the fixed
  // prefix + suffix, so the doc link + preserved-diff pointer in `suffix` always survive.
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  const branch = db.length > budget ? db.slice(0, Math.max(0, budget - 1)) + "…" : db;
  // Belt-and-braces final net, mirroring composeWorkflowScopeReason's caller.
  return (prefix + branch + suffix).slice(0, MAX_FAILURE_REASON_LEN);
}

/**
 * PRD #1416 M4 — the actionable `failure_reason` for a run whose branch was rewritten at or below
 * its published tip P AND could not be bridged to a fast-forward (the bridge B could not be built
 * or validated), so uzi cannot land it. It NAMES P, states that uzi lands work with a fast-forward
 * push and NEVER force-pushes (so a branch rewritten at or below P cannot be landed as it stands),
 * points at docs/github-bot-setup.md, and tells the human where the work is (the run branch / a
 * durable-recovery archive).
 *
 * Unlike composeWorkflowScopeReason / composeBaseAlignConflictReason, it is worded to be ACCURATE
 * whether or not a `preserved_patch` is attached: M4 attaches the diff ONLY from a scan-trusted
 * range (fact 15), and the divergent branch that reaches this path fails the scan floor open, so a
 * patch is usually OMITTED. It therefore NEVER hard-promises "Your diff is preserved below."; it
 * says the committed work is on the run branch and recoverable, which holds in both cases.
 *
 * P is a worker-verified 40-hex OID, so prefix + P + suffix is always well under
 * MAX_FAILURE_REASON_LEN; P is still clamped against the budget left by the fixed parts (so the doc
 * link in the suffix never truncates) and a belt-and-braces final slice mirrors the sibling
 * reasons. Exported for a direct length/content unit test.
 */
export function composeHistoryRewrittenReason(publishedTip: string): string {
  const prefix = "This run's branch was rewritten at or below its published tip ";
  const suffix =
    ". uzi lands work with a fast-forward push and NEVER force-pushes, so a branch rewritten at " +
    "or below that tip cannot be landed as it stands. The committed work is on the run's branch " +
    "and is recoverable (export it with `uzi run export`); a human can restore the published tip " +
    "as an ancestor with `git merge -s ours` and re-push. See docs/github-bot-setup.md.";
  // Clamp P (the only variable part) against the budget left after the fixed prefix + suffix, so
  // the doc link + recovery pointer in `suffix` always survive.
  const budget = MAX_FAILURE_REASON_LEN - prefix.length - suffix.length;
  const tip =
    publishedTip.length > budget
      ? publishedTip.slice(0, Math.max(0, budget - 1)) + "…"
      : publishedTip;
  // Belt-and-braces final net, mirroring composeBaseAlignConflictReason's caller.
  return (prefix + tip + suffix).slice(0, MAX_FAILURE_REASON_LEN);
}

/**
 * PRD #1416 M2: the body of the worker-authoritative safety steer handed to the agent when the
 * runner detects the branch's history was rewritten at/below a published floor. It is uzi's own
 * guidance (armed in-process by maybeSteerOnDivergence), NOT untrusted user text, so both
 * executors render it OUTSIDE the `<follow_up>` fence and without the "never as instructions"
 * framing. A plain multi-line recipe: record the current tip first, then restore the published
 * tip as an ancestor with `git merge -s ours <P>` (tree unchanged, P becomes a parent so the
 * branch fast-forwards again), never rewrite at/below P again, and integrate the default branch
 * with `git merge`. Both SHAs are worker-verified OIDs; no repo-controlled text is rendered.
 *
 * Not exported: it is consumed only by {@link RunRunner.maybeSteerOnDivergence} in this file; the
 * steer content is asserted end-to-end via the armed steer in the runner-divergence tests.
 */
function composeSafetySteer(publishedTip: string, currentTip: string): string {
  return [
    `This branch's history was rewritten at or below its published tip ${publishedTip}. uzi lands`,
    `work with a fast-forward push and NEVER force-pushes, so a branch whose history diverges below`,
    `${publishedTip} cannot be landed as it stands. Fix it now, before any further work:`,
    ``,
    `1. Record your current tip so nothing is lost — it is ${currentTip} (\`git rev-parse HEAD\`).`,
    `2. Restore the published tip as an ancestor WITHOUT changing your tree:`,
    `     git merge -s ours ${publishedTip}`,
    `   Your working tree is left exactly as it is; ${publishedTip} becomes a parent of a new merge`,
    `   commit, so the branch fast-forwards from it again and none of your work is discarded.`,
    `3. From now on, never rebase, amend, squash, or reset any commit at or below ${publishedTip}.`,
    `4. To integrate the default branch, use \`git merge\`, never \`git rebase\`.`,
  ].join("\n");
}

/**
 * PRD #1416 M5 (D5): does a submitted `plan_md` propose an operation that would rewrite history?
 * A WARN-ONLY, case-insensitive regex scan over the plan prose (case-insensitive to catch prose
 * casing). False POSITIVES are explicitly accepted — a plan that says "do NOT `git rebase`" still
 * matches, and that is fine: a plan is prose, one spurious status line costs nothing, and a false
 * NEGATIVE is caught downstream by the M2 mid-run steer and the M3 finalize bridge. Matches the
 * D5 pattern set: `git rebase`, `--amend`, `filter-repo`/`filter-branch`, `reset --hard`, or a
 * forced push (`push` followed on the same line by `--force`/`-f`/`--force-with-lease`).
 *
 * Not exported: consumed only by {@link RunRunner.gatePlan} in this file; its behaviour is
 * asserted end-to-end via the emitted status nudge (and the armed steer) in the M5 plan-gate tests.
 */
function planProposesRewrite(planMd: string): boolean {
  const patterns: RegExp[] = [
    /\bgit\s+rebase\b/i,
    /--amend\b/i,
    /\bfilter-repo\b/i,
    /\bfilter-branch\b/i,
    /\breset\s+--hard\b/i,
    // A forced push: `push` followed on the SAME LINE by a force flag (PRD: push
    // (--force|-f|--force-with-lease)). `--force-with-lease`/`--force` are listed ahead of `-f`
    // so the longest flag wins; `-f\b` will not match inside `--force` (the trailing `\b` fails
    // before the `o`). Bounded to one line ([^\n]*?) so an unrelated later `--force` never pairs
    // with a much earlier `push`.
    /\bpush\b[^\n]*?(?:--force-with-lease|--force|-f)\b/i,
  ];
  return patterns.some((re) => re.test(planMd));
}

/**
 * PRD #1416 M5: the body of the worker-authoritative safety steer armed at the PLAN GATE when the
 * submitted plan proposes rewriting history (planProposesRewrite) on a branch with a published
 * floor P, under AUTO-APPROVE. Distinct from {@link composeSafetySteer}, the M2 DETECTED-rewrite
 * recipe: at plan time there is no rewritten tip H yet, so this is PREVENTIVE — it names P, states
 * that uzi lands work with a fast-forward push and NEVER force-pushes (so the worker cannot land a
 * rewritten branch without bridging), tells the agent not to rebase/amend/squash/reset any commit
 * at or below P, and to integrate the default branch with `git merge`, not `git rebase`. Armed
 * only on the auto-approved branch (a human on the gated branch sees the plan + the status nudge
 * and can revise/reject). uzi's own guidance, NOT untrusted user text, so both executors render it
 * outside the `<follow_up>` fence. P is a worker-verified OID; no repo-controlled text is rendered.
 *
 * Not exported: consumed only by {@link RunRunner.gatePlan} in this file; its content is asserted
 * end-to-end via the armed steer in the M5 plan-gate tests.
 */
function composePlanGateNudge(publishedTip: string): string {
  return [
    `Your plan proposes rewriting history on a branch that is ALREADY PUBLISHED at ${publishedTip}.`,
    `uzi lands work with a fast-forward push and NEVER force-pushes, so the worker cannot land a`,
    `branch that was rewritten at or below ${publishedTip} without bridging. Before you start:`,
    ``,
    `1. Do NOT rebase, amend, squash, or reset any commit at or below ${publishedTip}.`,
    `2. Add new commits on top instead.`,
    `3. To integrate the default branch, use \`git merge\`, never \`git rebase\`.`,
  ].join("\n");
}

/**
 * Thrown when a SEEDED run's clone was cut from a commit that diverges from the one the
 * user planned against AND the run was created with --require-base (PRD #209 M4, Open
 * Question 3). The runner catches it on the generic failure path and reports `failed`
 * with this message, which names both commits. Without --require-base a divergence only
 * warns into the feed and the run implements anyway. The message carries commit SHAs
 * only, never a secret.
 */
export class BaseCommitDivergedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "BaseCommitDivergedError";
  }
}

/**
 * Whether a planned-against commit and the clone's resolved base name the SAME commit,
 * tolerant of git's abbreviated SHAs (PRD #209 M4): a match when either is a prefix of
 * the other, compared case-insensitively. Both sides must be non-empty — an empty side is
 * never a match, so a missing base never reads as "matches everything". Equality is the
 * degenerate prefix case, so it is covered without a special branch.
 */
export function baseCommitsMatch(planned: string, actual: string): boolean {
  const a = planned.trim().toLowerCase();
  const b = actual.trim().toLowerCase();
  if (a === "" || b === "") return false;
  return a.startsWith(b) || b.startsWith(a);
}

/**
 * Evaluate a seeded run's base-commit staleness (PRD #209 M4). It is the whole staleness
 * decision, factored out of the runner so the fail path is directly testable by TYPE (the
 * runner swallows the throw on its generic failure path, so a runner-level test can only
 * see the resulting `failed` state, not the error class). Returns:
 *   - `undefined` when there is no planned commit, or it matches the clone's base
 *     (prefix-tolerant) — the caller proceeds silently;
 *   - a warning STRING naming both commits when they diverge and `requireBase` is false —
 *     the caller emits it to the feed and implements anyway;
 * and THROWS BaseCommitDivergedError when they diverge and `requireBase` is true, so the
 * caller lets it reach the run-failure path and never implements against a diverged base.
 */
export function evaluateBaseStaleness(
  plannedBase: string | undefined,
  actualBase: string,
  requireBase: boolean,
): string | undefined {
  if (!plannedBase || baseCommitsMatch(plannedBase, actualBase)) return undefined;
  const detail =
    `the seeded plan was written against commit ${plannedBase}, but this clone's ` +
    `base commit is ${actualBase}`;
  if (requireBase) {
    throw new BaseCommitDivergedError(
      `${detail}; --require-base is set, so this run will not implement against a diverged base`,
    );
  }
  return (
    `${detail}; the plan may reference files that have moved since it was written — ` +
    `implementing anyway (create the run with --require-base to stop instead)`
  );
}

/**
 * What the per-run executor factory yields for one execution (PRD #42 Decisions
 * 4/5): a freshly-constructed executor plus the per-run HOME to clean on terminal.
 *  - `executor`: built anew for THIS run, so `SdkExecutor.spawnedPids` and
 *    `killAgentTree` are private to it — two concurrent runs can never wipe or kill
 *    each other's subprocess tree (the B1 pre-push reap). Serial runs shared one
 *    instance before; that latent hazard is closed by construction here.
 *  - `homeDir`: the run's private HOME (`agent-home/<runId>`), removed by the runner
 *    when the run reaches a terminal state. Optional so a test/stub factory that owns
 *    its HOME (or needs none) yields undefined and the runner skips the cleanup.
 */
export interface RunExecution {
  executor: Executor;
  homeDir?: string;
}

/** Build a per-execution executor for a run id (called once per `execute`). PRD #1171 M3:
 *  widened with the optional claim Codex binding so the factory's DARK selection seam can
 *  build a Codex executor for an internally-bound claim; absent (the ordinary Claude claim)
 *  takes the exact legacy path. The chat lane's `makeChatExecutor` is a DIFFERENT type and
 *  is untouched. */
export type ExecutorFactory = (runId: string, codex?: ClaimCodexSecrets) => RunExecution;

/**
 * PRD #218 M1 — the worker-shutdown registry entry for one in-flight run. A graceful
 * SIGTERM/SIGINT (`Runner.shutdown`) must fetch each running run's committed work back
 * into the worker bare before the container is killed, so the sweeper requeues a run
 * whose TREE is safe rather than one whose work was destroyed on the next re-claim.
 *  - `cancel`: the run's own per-run AbortController (the executor watches its signal),
 *    so shutdown aborts the SDK turn the same way a steering cancel does.
 *  - `shuttingDown`: the DISCRIMINATOR. Only a shutdown sets it; a user steering-cancel
 *    aborts the same controller with the same error, so the flag — never the error — is
 *    what routes a run to the fetch-back-and-requeue branch instead of today's failure.
 */
interface ActiveRun {
  cancel: AbortController;
  shuttingDown: boolean;
}

/**
 * PRD #949 M2 — the per-run state carrier for RunRunner.execute(). Holds the
 * cross-phase state the extracted phase methods, the (still-inline) back half, and
 * the catch/finally read: the const collaborators built in the prologue plus the
 * mutables the phases fill in (barePath/worktreePath/branch/runnerClone/result and
 * the park/shutdown flags). Unexported on purpose — an exported-but-unused carrier
 * would redden knip (deadcode:agent).
 */
interface RunFlight {
  readonly runId: string;
  readonly executor: Executor;
  readonly runHome: string | undefined;
  readonly runScopedSecrets: string[];
  readonly runLog: Logger;
  readonly redact: ReturnType<typeof makeRedactor>;
  readonly redactText: ReturnType<typeof makeTextRedactor>;
  readonly batcher: MessageBatcher;
  readonly cancel: AbortController;
  readonly steering: SteeringChannel;
  /** PRD #1247 M5b: the claim-lane generation THIS claim holds (from claim.claim_generation).
   *  Threaded onto every mutating report (the reportState closure) and every message batch (the
   *  batcher), so the server's per-query fence can engage. The client's send-gate (fix round E)
   *  decides whether it actually rides the wire — a generation>0 capability/feature worker sends it,
   *  0 (chat's legacy sentinel) is never sent, and a rolled-back api's strict-decode 400 strips it
   *  and retries ONCE. Server-side NOT NULL DEFAULT 0. */
  readonly claimGeneration: number;
  readonly reportState: (
    body: Parameters<WorkerClient["reportState"]>[1],
    signal?: AbortSignal,
  ) => ReturnType<WorkerClient["reportState"]>;
  observedSessionId: string | undefined;
  barePath: string | undefined;
  worktreePath: string | undefined;
  branch: string | undefined;
  active: ActiveRun | undefined;
  parked: boolean;
  /** PRD #1391 Run B M3 (N2/D5): true once ANY terminal outcome for this generation has been
   *  sent/resolved through {@link RunRunner.journalAndSendTerminal} — a run-lane completed/failed
   *  site, the permanent-failure hook, or reportGenericFailure itself. A journaled outcome is FINAL,
   *  so once this latches, reportGenericFailure never reports a SECOND `failed` — even after a 200
   *  RETIRED the journal (which makes `hasPendingTerminal` read false), the exact fall-through this
   *  latch closes. Distinct from `hasPendingTerminal`: that reads the on-disk journal (kept), this
   *  survives the journal's retirement. false until the first terminal resolve. */
  terminalResolved: boolean;
  /** #1539: the outcome of the permanent-failure hook's pre-settle reap, or undefined when the
   *  hook never ran (every ordinary path). true iff the reap confirmed while the run was still
   *  actively-claimed; false on a guard miss or a blocked/failed reap. reportGenericFailure reads it
   *  to settle custody ONCE without a second reap — the hook already reaped between the install and
   *  the send. Gated by {@link RunRunner.permanentFailureReapValid} against `permanentFailureReapSafety`. */
  permanentFailureReap?: boolean;
  /** #1539: the executor safety epoch the permanent-failure hook reaped, captured beside
   *  `permanentFailureReap`. The stale-epoch guard settles only while `executor.safety` still equals
   *  this — CodexExecutor swaps `this.safety` after each checkpoint, so a hook that tripped during a
   *  checkpoint boundary reaped the OLD epoch and must NOT settle against the new provider. `undefined`
   *  for Claude/stub (no safety), which equals the live `executor.safety` and settles as before. */
  permanentFailureReapSafety?: unknown;
  preserveSession: boolean;
  /** PRD #1392 M2: this run parked on a PRE-CLONE forge-unreachable transient error, so it
   *  captured no clone and no model session. Unlike a limit_wait/recovery park, it preserves its
   *  HOME + plugin dir ONLY when a resume transcript is resolvable on this worker
   *  (`preserveSession`), never on `parked` alone (D4). The finally reads it to gate exactly the
   *  two resume-artifact removals; false is the safe default for every other path. */
  preClonePark: boolean;
  /** Retain the only copy of unverified recovery work; never guard non-filesystem cleanup. */
  preserveRecoveryClone: boolean;
  /** Issue #1600: the budget off the latest REFUSED wall park's 409, held until the executor
   *  takes it (RunContext.takeWallParkRefresh). */
  wallParkRefresh?: WallParkRefresh;
  lastPublish: number;
  lastPublishedTip: string | undefined;
  /** PRD #1062 M2 (#1036): the current tip of `refs/uzi-checkpoints/<branch>` as this run last
   *  knows it — seeded from `claim.checkpoint_tip`, advanced to the declared overlay/real tip on
   *  every CONFIRMED publish. Fed to `checkpointPack` as the overlay's `prevCheckpointTip` so a
   *  second sequential overlay carries the prior tip as parent[0] (base-first) and the broker
   *  accepts it as a fast-forward. */
  lastCheckpointRefTip: string | undefined;
  /** issue #1086 (F2 from #1036): the tip of the LAST overlay/real tip we ATTEMPTED to publish
   *  when the publish result was AMBIGUOUS (a thrown error or a non-2xx — the broker may have
   *  ACCEPTED the push before the HTTP ACK was lost). Undefined when no attempt is pending. The
   *  next overlay chains from this (as parent[0]) via `attempted ?? confirmed`, so the chain
   *  reaches the broker's actual ref whether or not the prior push landed; a CONFIRMED publish
   *  clears it. In-memory only — NOT seeded from the claim: `claim.checkpoint_tip` is a
   *  server-confirmed anchor, and a resume self-heals via `lastCheckpointRefTip`. */
  lastAttemptedCheckpointRefTip: string | undefined;
  /** issue #1030: distinct checkpoint-publish failure/skip outcomes already surfaced on
   *  the run feed for THIS run, keyed as `http:<code>` / `skip:<reason>` / `error`. Dedupes
   *  the feed line so the ~20-min time-gated retry of a persistently-failing publish does
   *  not spam the feed. */
  reportedPublishOutcomes: Set<string>;
  /** issue #1597 M2: the per-flight SINK GATE. Every path that moves the run's durable state while
   *  the turn is live (the checkpoint closure, parkForPause, parkForWall, enterCompletionHold and
   *  the credential-switch attempt) runs under `sinkGate.run`; the mid-turn checkpoint tick only
   *  `tryAcquire`s it and is PREEMPTED (aborted, then awaited to full settlement) by a gated path.
   *  See agent/src/sink-gate.ts. */
  readonly sinkGate: SinkGate;
  /** issue #1597 M2: `*.lock` files in the worker bare a cancelled mid-turn tick could not prove
   *  its own, so it RETAINED them (never deleted). While any still EXISTS (re-checked by
   *  RunRunner.retainedBareLockRemains at each tick start and before the shutdown line), later
   *  ticks skip (`bare_lock_retained`) and a shutdown checkpoint that does not land names that
   *  class; entries whose file is gone are dropped. */
  retainedBareLocks: string[];
  /** issue #1597 M2: tick child process groups that were still alive after their SIGKILL and the
   *  bounded settlement wait. While any member is alive, later ticks skip (`tick_process_survived`),
   *  every gated sink first waits (bounded) for them to go, and a shutdown checkpoint that does not
   *  land names that class; gone groups are dropped. */
  survivingTickGroups: SurvivingGroup[];
  /** issue #1597 M2 (round 3): a milestone publish was DEFERRED out of a Codex permit
   *  (`scan_deferred`) and is owed — the next overlay-less checkpoint outside a permit publishes
   *  regardless of the time gate; cleared by any non-aborted scan+publish attempt outside a permit
   *  (published, blocked, or failed). */
  pendingPublish: boolean;
  /** issue #1597 M2 (round 3): set by the running mid-turn ticker — schedule a tick soon. */
  kickMidTurnTick?: () => void;
  runnerClone: RunnerClone | undefined;
  ciFixHumanApproved: boolean;
  result: ExecutorResult | undefined;
  /** PRD #1416 M1: the branch's published forge tip P at claim (fact 2) — the floor at/below
   *  which the branch must never be rewritten, since uzi lands work with a plain fast-forward
   *  push and never force-pushes. Recorded ONCE at claim from `originBranchTip(bare, branch)`
   *  (no fetch). Null/absent when the branch did not exist on the forge at clone (a fresh
   *  issue run). Held here, on the flight, so it survives an executor/session restart —
   *  deliberately NOT on `RunnerClone.baseCommit`, which can point at unpublished recovered
   *  work. Named in every prompt of a run with a published floor; later milestones read it for
   *  the ancestry check and the finalize bridge. */
  publishedTip?: string;
  /** PRD #1416 M1: floor C, initialised to P (`publishedTip`). Advanced to each confirmed
   *  checkpoint tip by later milestones (M2/M3); M1 only seeds it. */
  checkpointFloor?: string;
  /** PRD #1416 M2: the set of fetched tips already steered on for a divergence, so the mid-run
   *  detection emits AT MOST ONE status + steer per distinct tip. A repeated checkpoint tick that
   *  re-fetches the SAME diverged tip emits nothing; a FURTHER rewrite (a new tip) is a new key
   *  and steers again. Lazily initialised on first use. Held on the flight so it survives across
   *  checkpoint ticks within a flight (a fresh flight after a restart re-detects, which is fine —
   *  the finalize bridge in M3 is the correctness backstop). */
  steeredTips?: Set<string>;
}

/** PRD #1390 M2a: the four run states a worker's ActiveSnapshot may report as a `phase`
 *  (a strict subset of RunState — the ones the api re-adopts a run to). A `running` report
 *  and each of the three held-state parks map through here into the active-run registry so
 *  the next snapshot lists this run's real phase; every other status (terminal, limit_wait,
 *  paused, recovery_wait, credential_switch, ...) is not a snapshot phase and leaves the
 *  registry entry unchanged (the run is removed on terminal / requeue). */
const SNAPSHOT_PHASES = new Set<ActiveSnapshotPhase>([
  "running",
  "awaiting_approval",
  "awaiting_input",
  "awaiting_followup",
]);

function snapshotPhaseOf(status: StateRequest["status"]): ActiveSnapshotPhase | undefined {
  return SNAPSHOT_PHASES.has(status as ActiveSnapshotPhase) ? (status as ActiveSnapshotPhase) : undefined;
}

/** issue #1597 M2: test-only seams for the mid-turn checkpoint tick and the scanned publish. */
export interface CheckpointTestHooks {
  /** Awaited just before a tick's git-busy probe (a test can hold a tick in its pre-scope phase). */
  beforeBusyProbe?: () => Promise<void>;
  /** Fires between the pinned secret scan and the pack of an overlay-less checkpoint publish. */
  afterCheckpointScan?: (ctx: { barePath: string; branch: string; range: CheckpointRange }) => Promise<void>;
  /** Rewrite / observe the tick's child processes (see TickSpawnerTestHooks). */
  tickSpawn?: TickSpawnerTestHooks;
  /** Observe every tick's outcome class (the same value logged as "mid-turn checkpoint tick"). */
  onTickOutcome?: (outcome: string) => void;
  /** SIGTERM → SIGKILL grace for a cancelled tick child (default 2s). */
  tickKillGraceMs?: number;
  /** The checkpoint secret-scan deadline (default CHECKPOINT_SCAN_TIMEOUT_MS, 60s). */
  scanDeadlineMs?: number;
  /** Observe the flight bookkeeping right after a confirmed PINNED publish. */
  afterPinnedPublish?: (state: { publishedTip: string; lastPublishedTip?: string; checkpointFloor?: string }) => void;
}

/** Tuning the runner needs beyond the collaborators (defaults keep M2/M3 tests terse). */
export interface RunnerOptions {
  /** How often the steering channel polls /inputs (default 3s). */
  pollMs?: number;
  /** Plan-approval gate cap; 0 disables (default 24h). */
  planApprovalTimeoutMs?: number;
  /** PRD #88: fallback answer deadline in ms. The claim's question_timeout_seconds
   *  takes precedence; this covers a server that does not send one. */
  questionTimeoutMs?: number;
  /** Injected for tests; default opens real GitLab MRs. The worker picks between
   *  this and `forgejo` per claim (`repo.forge_type`, D9). */
  gitlab?: ForgeClient;
  /** Injected for tests; default opens real Forgejo PRs (PRD #65 D9). */
  forgejo?: ForgeClient;
  /** Injected for tests; default opens real GitHub PRs (PRD #238 D9). */
  github?: ForgeClient;
  /** Injected for tests; default is the real `.claude/agents/` parser (PRD #37).
   *  A seam so a test can drive the detection-failure path deterministically. */
  detectRepoAgents?: (worktreePath: string) => Promise<DetectedRepoAgents>;
  /** Injected for tests; default runs the uzi test suites as real subprocesses. A
   *  self_improve run's MR carries these results as its own evidence (PRD #46). */
  checkRunner?: CheckRunner;
  /** PRD #267: min interval between time-based origin checkpoint publishes on the
   *  reap:false path; 0 disables. Default 20m. */
  checkpointIntervalMs?: number;
  /** issue #1597 M2: how often the MID-TURN checkpoint tick fires while `executor.run` is in flight
   *  (a busy probe, then a quiet reap:false checkpoint body — fetch-back, steer/bridge, time-gated
   *  scanned publish). 0 disables. Default 5m (CHECKPOINT_TICK_INTERVAL). */
  checkpointTickIntervalMs?: number;
  /** issue #1597 M2: TEST-ONLY seams for the mid-turn checkpoint machinery. Production passes none. */
  checkpointTestHooks?: CheckpointTestHooks;
  /** PRD #1030 M4: CLIENT-side cap (ms) on the graceful-shutdown durability sequence
   *  (WIP-marker commit + fetch-back + checkpoint publish) so a slow/unreachable forge
   *  cannot hang the shutdown past the k8s termination grace. Default 15s — see the
   *  budget note at the shutdown branch. Injectable so a test can drive the timeout
   *  deterministically against a hanging publish. */
  shutdownPublishTimeoutMs?: number;
  /** Delay between local recovery capture / state-report attempts (capped at 30s). */
  recoveryRetryMs?: number;
  /** PRD #1171 m4: the bounded absolute deadline (ms) for a Codex durability-sink
   *  `withBoundary` (quiesce → reap → action). NEVER unbounded. Default 30s. Only used when the
   *  executor is Codex-selected (`executor.safety`); a Claude/stub run ignores it. */
  codexBoundaryDeadlineMs?: number;
  /** Injectable clock for tests; defaults to Date.now. */
  now?: () => number;
  /** Injectable answer-deadline timer for tests; defaults to setTimeout (unref'd)
   *  paired with a clearTimeout canceller. Lets a test arm and OBSERVE the per-run
   *  answer budget deterministically — the per-run-vs-per-question distinction is a
   *  fact about the `remaining` the second park arms — instead of racing a wall-clock
   *  bound that flakes under CPU contention. Returns a canceller. */
  setTimer?: (cb: () => void, ms: number) => () => void;
  /** issue #1597 M2: the injectable timer that arms the REPEATING mid-turn checkpoint tick (same
   *  cancel-fn shape as `setTimer`; default a real unref'd setTimeout). Deliberately separate from
   *  `setTimer`, whose arm count existing tests pin (e.g. "the answer deadline is armed exactly once
   *  per park") — arming the tick through it would add an arm to every run. The tick's own 120s /
   *  60s deadlines still use `setTimer`; they arm only while a tick runs. */
  setTickTimer?: (cb: () => void, ms: number) => () => void;
  /** PRD #1296 M3 — inject a pre-built recovery coordinator (a fake client/git) for tests;
   *  production builds one from the run lane's own client + git cache + join token. */
  recovery?: RecoveryCoordinator;
  /** issue #1582 M2 — inject a pre-built ancestry-settlement journal for tests; production builds
   *  one over `git.recoverySettlementRoot` keyed from the join token (absent ⇒ disabled). */
  settlement?: SettlementJournal;
  /** issue #1582 M2 — the settle RPC client for tests; production uses the run lane's client. */
  settleClient?: RecoverySettleClient;
  /** PRD #1391 M2 — the worker-owned message outbox the batcher SPILLS to after a
   *  sustained transient outage (instead of tripping). main.ts builds + inits it once
   *  and injects it here + into the Worker + ChatRunner. Undefined ⇒ the batcher keeps
   *  today's trip behaviour (tests that do not exercise spill). */
  outbox?: Outbox;
  /** PRD #1391 M2 — the shared re-arm registry (runId → the live batcher's rearm()).
   *  The runner registers this run's batcher at construction and drops it in the
   *  terminal finally; the worker's drainer calls the hook once the run's segments
   *  retire, returning a still-live batcher to the network. */
  rearm?: Map<string, () => void>;
  /** PRD #1391 M2 — the spill trip window (config.transientTripMs); default is the
   *  batcher's own TRANSIENT_TRIP_MS. Threaded to every run's batcher. */
  transientTripMs?: number;
  /** PRD #1391 M2 — the in-memory spill-buffer cap (config.outboxSpillBufferBytes);
   *  default is the batcher's own 2 MiB. Threaded to every run's batcher. */
  outboxSpillBufferBytes?: number;
  /** PRD #1391 Run B M3 — the per-record terminal canonicaliser cap (config.outboxTerminalMaxBytes).
   *  The write-ahead terminal send path canonicalises each terminal body under this. Default 1.25 MiB. */
  outboxTerminalMaxBytes?: number;
  /** PRD #1391 Run B M3 — the total per-seq gap-fill tombstone budget (config.gapFillMax) before a
   *  hole below the terminal fence is declared unrecoverable and the journal is marked blocked (D13).
   *  Default 10,000. */
  gapFillMax?: number;
  /** PRD #1390 M2a — the shared active-run registry. Each run this runner executes is
   *  registered at `running` (with its claim generation) as it starts, has its phase
   *  updated as it transitions/parks (through the reportState choke point), and is removed
   *  on terminal / requeue. The worker reads the registry to build the ActiveSnapshot.
   *  Undefined ⇒ no tracking (tests that never negotiate the feature). */
  activeRuns?: ActiveRunRegistry;
  /** PRD #1391 Run B M4 — the wall-clock budget for the queued-duplicate ownership-probe retry: a
   *  TRANSIENT probe failure retries with backoff up to this bound (the api's claim grace minus a
   *  margin), a held/queued row is re-probed until it, then the attempt ends without executing.
   *  Measured against the injectable `now`, so a test can shrink it. Default 20s — well under the
   *  api's claimed-never-started grace so a probe never outlives the claim. */
  queuedDuplicateProbeBudgetMs?: number;
}

/**
 * Drives one claimed run through the full M4 workflow:
 *   claim → running → seed runner clone → PLAN turn → approval gate → implement⇄
 *   review loop → worker fetches the agent branch back + pushes + opens MR →
 *   completed | failed
 * and normally tears the runner clone down (keeping the worker bare clone).
 * Unverified recovery capture retains the clone and its ownership journal
 * (verified against preserveRecoveryClone on 2026-09-08, issue #1197).
 * Under PRD #51 (b) the agent commits in the runner clone; the worker is bare-only.
 *
 * The plan gate, follow-up injection, and cancel are steered through a single
 * /inputs poller (SteeringChannel); the executor drives the SDK turns and calls
 * back here to gate/report; and — the primary directive — the WORKER (never the
 * agent) performs the authenticated push + MR with the PAT once the agent signals
 * done.
 */
export class RunRunner {
  private readonly pollMs: number;
  private readonly planApprovalTimeoutMs: number;
  /** PRD #88 fallback answer deadline, used when the claim carries none (an older
   *  server). The claim value wins when present — see questionTimeoutMs. */
  private readonly questionTimeoutMs: number;
  private readonly gitlab: ForgeClient;
  private readonly forgejo: ForgeClient;
  private readonly github: ForgeClient;
  /** PRD #1296 M3 — durable-recovery capture/journal/upload coordinator (D1/D3/D5).
   *  Disabled when the worker has no join token (a token-less test harness). */
  private readonly recovery: RecoveryCoordinator;
  /** issue #1582 M2 — the authenticated ancestry-settlement journal (adopted predecessor holds)
   *  and the settler that drives the api settle for them. Disabled without a worker token. */
  private readonly settlement: SettlementJournal;
  private readonly settler: PredecessorSettler;
  /** PRD #1391 M2 — the worker message outbox the batcher spills to, the shared re-arm
   *  registry, the spill trip window and the spill-buffer cap. All threaded into every
   *  run's MessageBatcher; `outbox`/`rearm` undefined ⇒ today's trip behaviour. */
  private readonly outbox: Outbox | undefined;
  private readonly rearm: Map<string, () => void> | undefined;
  private readonly transientTripMs: number | undefined;
  private readonly outboxSpillBufferBytes: number | undefined;
  /** PRD #1391 Run B M3 — terminal-journal send-path config, threaded into the terminal-resolve deps. */
  private readonly outboxTerminalMaxBytes: number;
  private readonly gapFillMax: number;
  /** PRD #1391 Run B M4 — wall-clock budget for the queued-duplicate ownership-probe retry. */
  private readonly queuedDuplicateProbeBudgetMs: number;
  /** PRD #1390 M2a — the shared active-run registry (runId → phase + claim generation);
   *  undefined ⇒ no snapshot tracking. The worker reads it to build the ActiveSnapshot.
   *  Named distinctly from the `activeRuns` shutdown Map below — that tracks abortable
   *  controllers, this tracks the snapshot phase. */
  private readonly snapshotRegistry: ActiveRunRegistry | undefined;
  private readonly detect: (
    worktreePath: string,
  ) => Promise<DetectedRepoAgents>;
  // Optional test override; production builds it per-run with the scrubbed check env
  // (buildCheckEnv) once the executor's provisioned toolEnv is known (M9).
  private readonly checkRunner?: CheckRunner;
  /** PRD #267: min interval between time-based origin checkpoint publishes on the
   *  reap:false path; 0 disables. */
  private readonly checkpointIntervalMs: number;
  /** PRD #1030 M4: client-side cap (ms) on the graceful-shutdown durability sequence. */
  private readonly shutdownPublishTimeoutMs: number;
  /** issue #1597 M2: the mid-turn checkpoint tick cadence (0 disables). */
  private readonly checkpointTickIntervalMs: number;
  /** issue #1597 M2: test-only seams (undefined in production). */
  private readonly checkpointTestHooks: CheckpointTestHooks | undefined;
  private readonly recoveryRetryMs: number;
  /** PRD #1171 m4: bounded absolute deadline (ms) for a Codex durability-sink withBoundary. */
  private readonly codexBoundaryDeadlineMs: number;
  /** PRD #267: injectable clock (defaults to Date.now), so the time-gate is testable.
   *  Also feeds the PRD #88 answer-deadline math (askUser), so that budget is testable
   *  on the same clock. */
  private readonly now: () => number;
  /** PRD #88: injectable answer-deadline timer (defaults to setTimeout/clearTimeout),
   *  so a test can arm and observe the per-run answer budget without a wall-clock race. */
  private readonly setTimer: (cb: () => void, ms: number) => () => void;
  /** issue #1597 M2: arms the repeating mid-turn checkpoint tick (see RunnerOptions.setTickTimer). */
  private readonly setTickTimer: (cb: () => void, ms: number) => () => void;
  /** PRD #41: absolute plan-approval deadline (epoch ms) per runId, set on the FIRST
   *  gate entry and reused across every revision round so N rounds share ONE budget (not
   *  24h per round). Cleared when the gate resolves terminally (approve/reject/cancel/
   *  timeout) and, defensively, when the run reaches a terminal state. */
  private readonly gateDeadlines = new Map<string, number>();
  /** PRD #88: per-run ABSOLUTE answer deadline, shaped exactly like gateDeadlines —
   *  one budget for the run, not a fresh clock per question, so N questions cannot
   *  extend a parked run indefinitely.
   *
   *  Not durable, and that is worth stating rather than implying: the map is worker
   *  memory, so a worker death re-queues the run and the resuming worker starts a
   *  fresh budget. The honest worst case is QUESTION_TIMEOUT x (RUN_MAX_REQUEUES + 1). */
  private readonly questionDeadlines = new Map<string, number>();
  /** PRD #88: the question id a run is currently parked on. Seeded from the claim on a
   *  resume so a re-park re-uses the SAME id rather than minting a new one — which is
   *  what lets an answer submitted before a worker death still be honoured. */
  private readonly openQuestionIds = new Map<string, string>();
  /** PRD #88: how many times each run has parked, for the LOG LINE only.
   *
   *  Not a guard and not the cap. QUESTION_MAX is enforced in sdk-executor against
   *  its own per-execute counter, and the staleness key is the question id — this
   *  ordinal is never compared to anything. It is deliberately kept off the wire:
   *  a field named for an arrival ordinal, sitting in the question payload, reads as
   *  a staleness discriminator to the next person, and this feature has exactly one
   *  of those. A surface that wants "question 2 of this run" can count `question`
   *  messages in the feed, where the ordinal is a fact rather than a claim. */
  private readonly questionCounts = new Map<string, number>();
  /** PRD #41 (Decision 3): the set of runs that have opened a plan gate at least once.
   *  It distinguishes the FIRST gate (epoch 0 — a verdict already queued when the gate
   *  opens still applies) from a RE-gate after a revision turn (where the epoch must be
   *  advanced so a mid-revision approve of the superseded plan goes stale). Works
   *  regardless of planApprovalTimeoutMs (unlike keying off gateDeadlines). Cleared on a
   *  terminal verdict and, defensively, when the run reaches a terminal state. */
  private readonly gatedRuns = new Set<string>();
  /** PRD #218 M1: the in-flight runs, so a graceful shutdown can abort each and let its
   *  catch fetch the committed work back before the container dies. Registered once the
   *  runner clone exists (there is nothing to fetch back before that) and deregistered
   *  in the terminal finally. */
  private readonly activeRuns = new Map<string, ActiveRun>();
  /** Whole-execution ownership, including factory setup, batcher close and every
   * finally cleanup. A promoted claim can arrive before an old park ACK returns. */
  private readonly executionTails = new Map<string, Promise<void>>();
  /** PRD #218 M1: set by `shutdown()`. Read when a run registers so a run that starts
   *  DURING the shutdown drain (a late claim) is aborted immediately rather than running
   *  to completion past the grace window. */
  private shuttingDownGlobal = false;

  constructor(
    private readonly client: WorkerClient,
    private readonly git: GitCache,
    /** Per-execution executor factory (PRD #42 Decision 4). Called once per
     *  `execute` so each run drives its OWN executor instance. */
    private readonly makeExecutor: ExecutorFactory,
    private readonly log: Logger,
    private readonly batchMs: number,
    /** The worker's join token — redacted from message payloads (it lives in
     *  the worker env, reachable via a /proc read of the parent). */
    private readonly joinToken?: string,
    opts: RunnerOptions = {},
  ) {
    this.pollMs = opts.pollMs ?? 3_000;
    this.planApprovalTimeoutMs = opts.planApprovalTimeoutMs ?? 24 * 60 * 60_000;
    this.questionTimeoutMs = opts.questionTimeoutMs ?? 24 * 60 * 60_000;
    this.gitlab = opts.gitlab ?? new GitLabClient();
    this.forgejo = opts.forgejo ?? new ForgejoClient();
    this.github = opts.github ?? new GitHubClient();
    // PRD #1296 M3 — the recovery coordinator reuses the run lane's client + git cache; its
    // MAC key derives from the worker join token (absent ⇒ disabled). Test-injectable via
    // opts.recovery so a unit test can supply a fake client/git without a real worker.
    this.recovery =
      opts.recovery ??
      new RecoveryCoordinator({
        client: this.client,
        git: this.git,
        log: this.log,
        recoveryRoot: this.git.recoveryRoot,
        workerToken: this.joinToken,
        now: opts.now,
      });
    // issue #1582 M2 — the settlement journal is a SIBLING of recovery/ (never inside it), keyed
    // from the join token under its own domain-separation label.
    this.settlement =
      opts.settlement ??
      new SettlementJournal({
        root: this.git.recoverySettlementRoot,
        workerToken: this.joinToken,
        log: this.log,
        now: opts.now,
      });
    const recovery = this.recovery;
    const gitCache = this.git;
    this.settler = new PredecessorSettler({
      journal: this.settlement,
      client: opts.settleClient ?? this.client,
      cleanup: {
        deleteSettlementRefs: (bare, runId, holdId) => gitCache.deleteSettlementRefs(bare, runId, holdId),
        deleteRecoveryPin: (bare, runId, gen) => gitCache.deleteRecoveryPin(bare, runId, gen),
        forgetGeneration: (runId, gen) => recovery.forgetGeneration(runId, gen),
      },
      log: this.log,
    });
    this.detect = opts.detectRepoAgents ?? detectRepoAgents;
    this.checkRunner = opts.checkRunner;
    // PRD #1391 M2: spill collaborators, threaded into each run's batcher (buildFlight).
    this.outbox = opts.outbox;
    this.rearm = opts.rearm;
    this.transientTripMs = opts.transientTripMs;
    this.outboxSpillBufferBytes = opts.outboxSpillBufferBytes;
    // PRD #1391 Run B M3: terminal send-path knobs. Defaults mirror config.ts (1.25 MiB / 10,000) so
    // a test that injects an outbox but not these knobs still canonicalises + gap-fills sanely.
    this.outboxTerminalMaxBytes = opts.outboxTerminalMaxBytes ?? Math.round(1.25 * 1024 * 1024);
    this.gapFillMax = opts.gapFillMax ?? 10_000;
    // PRD #1391 Run B M4: the queued-duplicate probe retry budget. Clamp a 0/negative override to
    // the default (a non-positive budget would give up before the first probe).
    this.queuedDuplicateProbeBudgetMs =
      opts.queuedDuplicateProbeBudgetMs !== undefined && opts.queuedDuplicateProbeBudgetMs > 0
        ? opts.queuedDuplicateProbeBudgetMs
        : 20_000;
    // PRD #1390 M2a: the shared active-run registry the worker reads to build snapshots.
    this.snapshotRegistry = opts.activeRuns;
    this.checkpointIntervalMs = opts.checkpointIntervalMs ?? 20 * 60_000;
    this.checkpointTickIntervalMs = opts.checkpointTickIntervalMs ?? 5 * 60_000;
    this.checkpointTestHooks = opts.checkpointTestHooks;
    this.shutdownPublishTimeoutMs = opts.shutdownPublishTimeoutMs ?? 15_000;
    this.recoveryRetryMs = Math.max(1, Math.min(opts.recoveryRetryMs ?? 1_000, 30_000));
    // PRD #1171 m4: bounded, never unbounded. Clamp a caller-supplied 0/negative to the default.
    this.codexBoundaryDeadlineMs =
      opts.codexBoundaryDeadlineMs && opts.codexBoundaryDeadlineMs > 0
        ? opts.codexBoundaryDeadlineMs
        : 30_000;
    this.now = opts.now ?? (() => Date.now());
    const realTimer = (cb: () => void, ms: number): (() => void) => {
      const t = setTimeout(cb, ms);
      t.unref?.();
      return () => clearTimeout(t);
    };
    this.setTimer = opts.setTimer ?? realTimer;
    this.setTickTimer = opts.setTickTimer ?? realTimer;
  }

  /** PRD #1296 M3 — restart-safe recovery resume (called once by the worker after
   *  registration). Re-uploads any journaled bundle BYTE-IDENTICALLY with no forge PAT.
   *  Best-effort; never throws to the caller. */
  async resumePendingRecoveries(signal?: AbortSignal): Promise<void> {
    await this.recovery.resumePending(signal).catch((err) => {
      this.log.warn("recovery: resume sweep failed", { error: errMessage(err) });
    });
  }

  /**
   * issue #1582 M2 — the restart/retry sweep of the ancestry-settlement journal (called by the
   * worker after the boot pending-terminal gate, then on a timer). Settles every DUE
   * `pending_settle` record with NO forge credential (the api proves ancestry itself). A record is
   * `pending_settle` only once its successor's completion ACK was OBSERVED
   * ({@link observeSettlementTerminalAck}); a pushed head persisted write-ahead of the report stays
   * `pushed` until then and is never sent, so no outbox pending-terminal check is needed here.
   * Best-effort; never throws.
   */
  async settlePendingPredecessors(signal?: AbortSignal): Promise<void> {
    await this.settler.sweep(signal).catch((err) => {
      this.log.warn("recovery settlement: sweep failed", { error: errMessage(err) });
    });
  }

  /**
   * issue #1582 M2 — apply an observed terminal-report ACK for generation `claimGeneration` of
   * `runId` to that generation's settlement records: a `completed` outcome promotes `pushed` →
   * `pending_settle`; a completion with no pushed head, or a failed/cancelled outcome, moves the
   * records `terminal` (pins kept). Every terminal send calls this BEFORE the outbox journal is
   * retired (the live reportState choke point, and the boot / post-drain / queued-duplicate outbox
   * replays), so a crash between the ACK and the promotion replays and promotes. Never throws.
   */
  async observeSettlementTerminalAck(
    runId: string,
    claimGeneration: number,
    body: StateRequest,
    ack: StateAck,
  ): Promise<void> {
    await this.settler.observeTerminalAck(runId, claimGeneration, body, ack).catch((err) => {
      this.log.warn("recovery settlement: terminal ACK observation failed", { run_id: runId, error: errMessage(err) });
    });
  }

  async execute(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    // Defense in depth (PRD #42): runId becomes a path segment in the per-run HOME
    // (agent-home/<runId>) which the runner fs.rm's on terminal. It is a
    // server-issued UUID, but reject anything not UUID-shaped BEFORE it reaches a
    // path — an empty id would resolve the per-run HOME back to the SHARED root
    // (removing chat's HOME + every run's HOME on terminal) and a separator/`..`
    // would escape it. Same guard provision-run.ts applies to the provisioning dir.
    // Content-free message: never echo the (rejected) id into a log/failure_reason.
    if (!RUN_ID_RE.test(runId))
      throw new Error("refusing to execute a run with an invalid run id");
    const previous = this.executionTails.get(runId);
    let release!: () => void;
    const tail = new Promise<void>((resolve) => { release = resolve; });
    // Install synchronously before factory work. A third claim must queue behind
    // the second even while the second is waiting for the first's cleanup.
    this.executionTails.set(runId, tail);
    try {
      if (previous) {
        await previous;
        // PRD #1391 Run B M4 — the generation-aware queued-duplicate router. A SECOND (or later) claim
        // of this run serialised behind the first must NOT blindly re-execute: the first attempt may
        // have journaled a terminal outcome, or a reclaim may have bumped the generation. Drain this
        // run's pending terminal synchronously, then probe ownership and decide (proceed only on a
        // claimed/running row AT this claim's generation). Ending here sends NO report — the run keeps
        // its authoritative status for the api's own reclaim/sweep to resolve.
        if (!(await this.gateQueuedDuplicate(claim))) return;
        claim = await this.refreshQueuedClaimCursor(claim);
      }
      await this.executeClaim(claim);
    } finally {
      if (this.executionTails.get(runId) === tail) this.executionTails.delete(runId);
      release();
    }
  }

  /**
   * PRD #1391 Run B M4 — the generation-aware queued-duplicate gate (fact 6). Called AFTER the
   * previous same-run execution settled and BEFORE `executeClaim`, only on a serialised duplicate.
   * Returns whether to PROCEED to execute:
   *   1. Synchronously resolve this run's pending terminal journal (if any) — send/retire/stale-retire
   *      per M3 — so a completion the previous attempt journaled lands (and its lease clears) first.
   *   1b. SC4 guard: if the drain LEFT a pending terminal for this run at the claim's generation (a
   *      `messages_pending` keep on an undrained run, a `blocked`/`gap_unrecoverable` journal, or a send
   *      that could not land), END the attempt WITHOUT executing — the outcome is still pending, so a
   *      second execution here would run over a pending outcome (an SC4 violation), even though the
   *      server can still read running@same-generation and the probe below would otherwise PROCEED.
   *   2. Probe `GET /worker/runs/{id}/ownership` (status + additive claim_generation) and decide:
   *      - `claimed`/`running` AT this claim's generation → PROCEED (a `running` row is the case where
   *        #1390 re-adopted this exact claim);
   *      - a TERMINAL status, or a DIFFERENT generation → END (no report, no execute);
   *      - a `queued`/held row at the SAME generation (e.g. SweepClaimedNeverStarted bumps nothing) →
   *        re-probe up to the budget, then END without executing;
   *      - a transient probe failure → retry with backoff up to the budget (the claim grace minus a
   *        margin), then END without executing;
   *      - a definitive 4xx (404 not-owned/reclaimed) → END without executing.
   */
  private async gateQueuedDuplicate(claim: ClaimResponse): Promise<boolean> {
    const runId = claim.run_id;
    // 1. Drain this run's pending terminal(s) first, so a journaled completion lands and clears its
    //    lease before we read ownership. No-op when no usable outbox is wired.
    await this.resolveRunPendingTerminals(runId);

    const claimGen = claim.claim_generation ?? 0;
    // 1b. SC4 guard (fact 6): the drain does NOT always clear the journal — a `blocked`/`gap_unrecoverable`
    //     record is left for the owner (D13), a `messages_pending` keep waits on an undrained run, and a
    //     send that could not land stays installed for a later resolve. In all of those the outcome is
    //     STILL pending, yet the server can read running@same-generation, so the ownership probe below
    //     would PROCEED and RE-EXECUTE the run OVER a pending outcome — the SC4 violation. End the attempt
    //     here instead (no report, no executeClaim): the run keeps its authoritative status, leased/listed
    //     for a later resolve or owner action. Only fall through to the probe once the drain actually
    //     cleared the pending terminal.
    if (claimGen !== undefined && this.outbox && this.outbox.hasPendingTerminal(runId, claimGen)) {
      this.log.info(QUEUED_DUPLICATE_END_LOG, {
        run_id: runId,
        reason: "a pending terminal outcome remains after the drain",
        claim_generation: claimGen,
      });
      return false;
    }
    const deadline = this.now() + this.queuedDuplicateProbeBudgetMs;
    for (;;) {
      if (this.shuttingDownGlobal) {
        this.log.info(QUEUED_DUPLICATE_END_LOG, { run_id: runId, reason: "worker shutting down" });
        return false;
      }
      let probe;
      try {
        probe = await this.client.getRunOwnership(runId);
      } catch (err) {
        // A definitive 4xx (404 not-owned / reclaimed, or any other non-transient 4xx) ends the
        // attempt at once; a transient error (5xx / 429 / network) retries under the budget.
        if (err instanceof RequestError && err.status >= 400 && err.status < 500 && err.status !== 429) {
          this.log.info(QUEUED_DUPLICATE_END_LOG, {
            run_id: runId,
            reason: "ownership probe returned a definitive not-owned/4xx",
            status: err.status,
          });
          return false;
        }
        if (this.now() >= deadline) {
          this.log.warn(QUEUED_DUPLICATE_END_LOG, {
            run_id: runId,
            reason: "ownership probe kept failing up to the grace budget",
            error: errMessage(err),
          });
          return false;
        }
        await sleep(this.recoveryRetryMs);
        continue;
      }
      const status = probe.status;
      // A DIFFERENT generation means a newer claim owns the run — end regardless of status. Only
      // compare when BOTH sides carry a generation; an older api that omits it (undefined) cannot
      // prove a mismatch, so we fall through to the status check.
      if (probe.claim_generation !== undefined && claimGen !== undefined && probe.claim_generation !== claimGen) {
        this.log.info(QUEUED_DUPLICATE_END_LOG, {
          run_id: runId,
          reason: "a different generation owns the run",
          probe_generation: probe.claim_generation,
          claim_generation: claimGen,
          status,
        });
        return false;
      }
      if (TERMINAL_RUN_STATUSES.has(status)) {
        this.log.info(QUEUED_DUPLICATE_END_LOG, { run_id: runId, reason: "run is terminal", status });
        return false;
      }
      if (status === "claimed" || status === "running") {
        return true; // proceed: this worker still holds the claim at this generation
      }
      // A `queued`/held row at the SAME generation: never proceed. Re-probe until the budget, then end
      // (a bounded wait for it to settle, exactly as the PRD prescribes).
      if (this.now() >= deadline) {
        this.log.info(QUEUED_DUPLICATE_END_LOG, {
          run_id: runId,
          reason: "run is held/queued at the same generation",
          status,
        });
        return false;
      }
      await sleep(this.recoveryRetryMs);
    }
  }

  /**
   * PRD #1391 Run B M4: synchronously resolve every pending terminal journal for ONE run (the
   * queued-duplicate gate's step 1). Mirrors the worker's boot resolve: a state-only `client.reportState`
   * send stamped with the journal's generation (so a superseded generation is refused stale_claim and
   * local-retired, D11), letting a completion the previous attempt journaled land before the ownership
   * probe reads the run's status. No-op when no usable outbox is wired.
   */
  private async resolveRunPendingTerminals(runId: string): Promise<void> {
    const outbox = this.outbox;
    if (!outbox || outbox.isDisabled()) return;
    const deps = this.terminalDeps();
    if (!deps) return;
    for (const entry of outbox.listPendingTerminals()) {
      if (entry.run_id !== runId) continue;
      const gen = entry.claim_generation;
      await resolvePendingTerminal(deps, {
        runId,
        claimGeneration: gen,
        // issue #1582 M2: the replayed ACK is applied to the settlement journal BEFORE the resolve
        // retires the outbox entry.
        send: async (body, sig) => {
          const ack = await this.client.reportState(runId, { ...body, claim_generation: gen }, sig);
          await this.observeSettlementTerminalAck(runId, gen, body, ack);
          return ack;
        },
      });
    }
  }

  private async refreshQueuedClaimCursor(claim: ClaimResponse): Promise<ClaimResponse> {
    let after = claim.last_seq;
    for (;;) {
      if (this.shuttingDownGlobal) throw new Error("worker shut down before a queued run could start");
      let page;
      try {
        // The worker-user read surface covers issue runs too. Its cap is 200;
        // consume pages through EOF after the predecessor closed its batcher.
        page = await this.client.getChatRunMessages(claim.run_id, after, 200);
      } catch (err) {
        if (err instanceof RequestError && err.status < 500 && err.status !== 429) throw err;
        await sleep(this.recoveryRetryMs);
        continue;
      }
      if (page.length === 0) return { ...claim, last_seq: after };
      const previous = after;
      for (const message of page) {
        if (!Number.isSafeInteger(message.seq) || message.seq <= previous) {
          throw new Error("queued run message cursor did not advance");
        }
        after = Math.max(after, message.seq);
      }
    }
  }

  private async executeClaim(claim: ClaimResponse): Promise<void> {
    const runId = claim.run_id;
    // PRD #1171 M3 (redactor canary hoist): register the Codex access-token + capability
    // canaries with the logger BEFORE makeExecutor, so a construction error inside the
    // Codex executor factory can never log an unredacted token. Both are secrets; absent on
    // an ordinary Claude claim (empty array ⇒ no-op). They join runScopedSecrets so the
    // terminal eviction (below) covers them, and buildFlight appends them to both redactors.
    const codexCanaries: string[] = claim.secrets.codex
      ? [claim.secrets.codex.access_token, claim.secrets.codex.capability]
      : [];
    for (const s of codexCanaries) this.log.addSecret(s);
    // This run's OWN executor + private HOME (PRD #42 Decisions 4/5), built fresh
    // per execution so nothing subprocess-scoped is shared with a concurrent run. The
    // DARK Codex selection seam reads claim.secrets.codex (absent ⇒ the literal Claude path).
    const { executor, homeDir: runHome } = this.makeExecutor(runId, claim.secrets.codex);
    // Register per-run secrets with the logger so they are scrubbed from any
    // output, then never log the claim payload itself. Tracked in runScopedSecrets
    // and evicted on terminal (Decision 7) so a completed run's PAT/token does not
    // linger in the process-lifetime scrub set; the registry is reference-counted,
    // so evicting these never un-scrubs a still-active sibling run that shares them.
    const gitBasic = gitBasicCredential(
      claim.secrets.forge_pat,
      claim.secrets.forge_username,
    );
    // Defense in depth for gitBasic: the git-over-HTTPS Basic credential
    // (base64(user:pat)) only ever lives in a GIT_CONFIG_VALUE (never argv/logs),
    // but register it too so a future leak through the git env would still be scrubbed.
    const runScopedSecrets = [claim.secrets.forge_pat, gitBasic];
    if (claim.secrets.anthropic_oauth_token)
      runScopedSecrets.push(claim.secrets.anthropic_oauth_token);
    for (const s of runScopedSecrets) this.log.addSecret(s);
    // The codex canaries were ALREADY addSecret'd above (before makeExecutor); append them
    // to runScopedSecrets AFTER the add loop — never inside it — so each is registered
    // exactly ONCE yet still evicted at the terminal (removeSecret is ref-counted, so a
    // second add here would leave a dangling registration the single eviction can't clear).
    runScopedSecrets.push(...codexCanaries);

    const flight = this.buildFlight(
      claim,
      runId,
      executor,
      runHome,
      gitBasic,
      runScopedSecrets,
    );
    const { runLog, batcher, reportState, steering } = flight;
    // PRD #1390 M2a: register this run in the active-run snapshot registry at `running`,
    // at the generation it was claimed at, so the worker's next ActiveSnapshot lists it.
    // The reportState choke point advances its phase as it parks/resumes; the terminal
    // finally removes it (a park that RETURNS from executeClaim — limit_wait/recovery/
    // pre-clone — is a requeue, so it stops being listed there too). Idempotent per run id.
    this.snapshotRegistry?.add(runId, claim.claim_generation ?? 0);
    try {
      await this.phaseClone(claim, flight);
      const sessionId = await this.phaseResume(claim, flight);
      // PRD #1349 M2 (D1/D3): after clone/reseed and BEFORE model work, record this run's exact
      // claim generation into the durable journal (the restore point it starts from) and
      // inventory any prior open holds by exact id + generation. The early generation-evidence
      // pin lets a later empty-turn park or early terminal exit disposition the EXACT hold
      // without inferring `current - 1`; the inventory is observational only (settlement is
      // deferred to the final durable head). Credential-free and best-effort.
      await this.recordRecoveryGenerationEvidence(claim, flight);
      await this.phasePreflightHandoff(claim, flight, sessionId);
      // PRD #1171 m4: the finalize sink. phasePreflightHandoff already ran the
      // security-boundary reap (its killAgentTree?.() — no-op for Codex); this wrapper adds the
      // Codex-ONLY finalize withBoundary so that, for a Codex run, the WHOLE phasePublish
      // (push/base-align/MR, or the pause/not_code/report-only early returns) runs under the
      // held permit — its per-sink reconcile + quiesce+reap close admission and tear down the
      // provider root before any PAT git op. For Claude/stub this is a plain call (the legacy
      // reap already happened at the untouched security boundary). A CodexBoundaryError before
      // a committed publish still propagates to the failed-run report below. Once phasePublish
      // registers the committed terminal callback, however, the pushed branch/open MR is the
      // authoritative outcome and must be reported after the boundary releases.
      let postFinalizeTerminal: (() => Promise<void>) | undefined;
      try {
        await this.withCodexBoundaryOnly(
          executor,
          { boundary: "finalize", deadlineMs: this.codexBoundaryDeadlineMs },
          (permit) => this.phasePublish(
            claim,
            flight,
            permit?.signal,
            executor.safety
              ? (report) => { postFinalizeTerminal = report; }
              : undefined,
          ),
        );
      } catch (err) {
        if (!postFinalizeTerminal || !isCodexBoundaryError(err)) throw err;
        runLog.warn(
          "Codex finalize boundary failed after committed publish; reporting committed terminal outcome",
          { error: errMessage(err) },
        );
      }
      // Once a branch push or MR creation succeeds, its terminal record is irreversible
      // bookkeeping for an already-committed forge side effect. Deliver it only after the
      // Codex boundary has released, with the normal terminal retry schedule and without
      // the boundary's expiring signal, so a near-deadline MR cannot become a failed run.
      await postFinalizeTerminal?.();
    } catch (err) {
      // PRD #35: a usage-limit death is not an ordinary failure. Handled before the
      // generic path below because that path is terminal in both senses — it reports
      // `failed` and it lets the finally erase the session this run wants to resume from.
      if (err instanceof LimitReachedError) {
        // PRD #1349 M2 (F2) / #1539: REAP THIS generation's provider FIRST, BEFORE
        // handleLimitReached reports anything, while the run is still actively-claimed
        // (the catch was entered from a `running` turn). A Codex run's pre-settle reap runs
        // the per-sink credential reconcile inside withBoundary (refreshCodex/releaseCodex),
        // which the api authorizes ONLY while actively-claimed (codexActivelyClaimedStatuses);
        // once handleLimitReached reports `failed` (or the server coerces a park to `failed`)
        // the reconcile is refused (409), which would block the reap and leak the
        // exact-generation hold as source_only. The credentialed settle runs AFTER the report
        // on the non-parked branch below. The PARKED path keeps its OWN park-boundary reconcile
        // (limit_wait is actively-claimed, so its second reconcile is authorized) — a blocked
        // pre-reap here poisons the registry (codex/registry.ts) so the park publish's reap also
        // fails, but the park still stands (D4).
        const limitReaped = await this.reapRecoveryProviderForSettle(
          claim,
          flight,
          runLog,
          "terminal",
        );
        flight.parked = await this.handleLimitReached(
          err,
          claim,
          batcher,
          reportState,
          runLog,
        );
        // parked === true is the ONLY thing that preserves on-disk state; see the
        // carve-out in the finally.
        //
        // PRD #218 M1: fetch the agent's committed work back into the worker bare
        // BEFORE the finally's carve-out — the tracking ref is where the next claim's
        // reseed (M2) reads it from, and it survives the `fs.rm` that the clone does
        // not. Only when the run actually parked (a resume is coming) and a clone
        // existed to fetch from. Best-effort: a park that fails is worse than a park
        // that loses work (D4), so a failed fetch-back must not undo the park.
        if (flight.parked) {
          if (flight.barePath && flight.worktreePath && flight.branch) {
          const barePath = flight.barePath;
          const worktreePath = flight.worktreePath;
          const branch = flight.branch;
          // PRD #1171 m4: route the park durability publish through the Codex reap facade.
          // For a Claude/stub run this is the LITERAL legacy `killAgentTree` reap (belt-and-
          // braces before we read the runner-owned clone — safe today since the executor's
          // run() finally reaps first — kept consistent with the done/shutdown fetch-back
          // sites) followed by the publish body, byte-for-byte. For a Codex run the facade
          // quiesces+reaps the provider root (after its per-sink auth-mode reconcile) and holds
          // the permit across the whole publish body.
          let parkPublished = false;
          try {
            await this.reapForSink(
              executor,
              { boundary: "park", deadlineMs: this.codexBoundaryDeadlineMs },
              async () => {
                // PRD #759 M1: commit any uncommitted work in the runner-owned clone to a
                // clearly-marked THROWAWAY commit (subject-prefixed wip(park):) so the
                // fetch-back carries it to the local tracking ref and the #628 broker publishes
                // it; M2 strips it back to uncommitted at adopt time so it never reaches the MR.
                // Best-effort — commitWipMarker swallows every error and the .catch is belt-and-
                // braces so nothing here can undo the park (D4).
                await this.git.commitWipMarker(worktreePath).catch(() => false);
                await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
                // PRD #1416 M3 (C5): bridge a divergent tracking tip BEFORE the park publish so a
                // reseed on resume adopts B (the rewritten work), not the published tip. Best-effort:
                // a park that fails is worse than a park that loses work (D4), so it NEVER throws —
                // "failed"/"unknown" only log and the park continues.
                await this.bridgeParkSinkBestEffort(barePath, branch, flight, runLog, "limit-park");
                // PRD #628 M2: publish a ONE-SHOT checkpoint to origin so a DIFFERENT worker
                // re-claiming this limit_wait run recovers the committed tree from
                // refs/uzi-checkpoints/<branch> instead of cold-starting from default. Runs
                // AFTER the fetch-back (checkpointPack reads the current tracking ref); an empty
                // park brokers a null/zero pack and publishes nothing. PRD #1062 M2 (#1036): the
                // park path is reaped above, so the overlay's PAT default-fetch is permitted.
                // The publish stays on the join-token seam (checkpointPack local read →
                // client.publishCheckpoint) and never throws; a publish failure must not undo
                // the park (D4).
                parkPublished = await this.publishCheckpointBestEffort(
                  flight,
                  barePath,
                  branch,
                  await this.buildCheckpointOverlay(claim, flight, barePath),
                );
              },
            );
          } catch (err) {
            // A NON-boundary throw propagates exactly as before. A CodexBoundaryError means the
            // Codex boundary could not reap/publish — nothing landed on origin, the same
            // durability consequence as a publish failure (parkPublished stays false); it must
            // NOT undo the park.
            if (!isCodexBoundaryError(err)) throw err;
            runLog.warn("park checkpoint boundary blocked; nothing published to origin", {
              run_id: runId,
              error: errMessage(err),
            });
          }
          // issue #1030: the park-publish result is EXPLICIT on the feed — a success line, and a
          // failure line naming the durability consequence (a resume on another worker restarts
          // from the default branch). The false case covers a real publish failure, an empty
          // park (null pack / no committed work) AND a blocked Codex boundary; in all, nothing
          // landed on refs/uzi-checkpoints/<branch>. publishCheckpointBestEffort ALSO emits the
          // specific HTTP/skip outcome (deduped). This batcher.emit lands because
          // handleLimitReached FLUSHES (not closes) the batcher on the park branch.
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: parkPublished
                ? "park checkpoint published to origin"
                : "park checkpoint NOT published — a resume on another worker will restart from the default branch",
            },
          });
          }
          // Close the batcher on the park path (handleLimitReached deferred the close so the
          // checkpoint-publish outcome above could reach the feed). Closed exactly once here
          // for every parked run, including the edge where the paths above were absent.
          await batcher.close().catch(() => undefined);
          // PRD #1349 M2 (D4): settle this generation's hold before the limit-park requeue. The
          // reapForSink above already reaped the agent tree, so the credentialed fresh-forge
          // comparison is safe: a verified-empty limit park releases its exact hold, work-bearing
          // committed history is promoted into the generation-bound archive, and a failed
          // comparison or upload retains. Best-effort; runs after the park report landed.
          await this.settleRecoveryGeneration(claim, flight, runLog);
        } else {
          // PRD #1349 M2 (F4) / #1539: EVERY non-parked limit outcome, not just the opt-out.
          // handleLimitReached returns parked=false on THREE distinct paths: the usage-limit
          // OPT-OUT (wait_on_limit=false, reported `failed`), a park report that THREW, and a
          // server ACK whose status is not `limit_wait` — including a server that COERCED a park
          // to `failed` because policy refused it. All three land here, and #1539 moves the reap
          // AHEAD of handleLimitReached's report (above), so by the time control reaches this
          // branch the provider is already reaped-or-not while the run was still actively-claimed.
          // Settle the exact-generation hold IFF that pre-report reap confirmed (limitReaped):
          // committed work CAPTURES into the generation-bound archive, a provably-empty outcome
          // RELEASES its exact hold, and a failed/unverifiable comparison RETAINS — never a silent
          // drop. NO post-report retry: the report has already made the run terminal, so a second
          // reap's Codex reconcile would be refused (409); and a blocked pre-reap poisons the
          // registry stickily (codex/registry.ts) so re-reaping cannot recover it anyway, while a
          // Claude/stub killAgentTree cannot fail. Best-effort; runs after the report landed.
          if (limitReaped) await this.settleRecoveryGeneration(claim, flight, runLog);
        }
      } else if (err instanceof TransientRecoveryError) {
        // Retry capture without abandoning the live claim. Only verified local
        // durability permits automatic promotion; shutdown retains uncaptured work
        // behind the worker-owned journal so the next claim recovers it first.
        flight.parked = await this.handleRecoveryExhausted(
          err,
          claim,
          flight,
          executor,
          batcher,
          reportState,
          runLog,
        );
      } else if (flight.active?.shuttingDown) {
        // PRD #218 M1 — the worker is shutting down (SIGTERM/SIGINT) and aborted this
        // run mid-flight. The DISCRIMINATOR is the flag, never the error: a user
        // steering-cancel aborts the same controller with the same REASON_CANCELLED and
        // must still fall through to the generic failure below. The run's tree is
        // already reaped (sdk-executor's run() finally kills the agent tree before this
        // catch is entered); the belt-and-braces reap now lives INSIDE the durability sink
        // below (m4: reapForSink — killAgentTree for Claude, withBoundary for Codex), which
        // always runs on this path because `shuttingDown` is only ever set once the runner
        // clone (hence barePath) exists.
        if (flight.barePath && flight.worktreePath && flight.branch) {
          const barePath = flight.barePath;
          const worktreePath = flight.worktreePath;
          const branch = flight.branch;
          // PRD #1030 M4: publish a FINAL checkpoint on graceful shutdown, mirroring the
          // park path's ordering (commitWipMarker → fetchBackBestEffort → publishCheckpoint).
          // Before M4 this branch was fetch-back ONLY, so a roll-while-running / eviction /
          // OOM / node-drain lost up to a whole ~20-min checkpoint interval of committed
          // work AND any uncommitted mid-milestone edits. Now:
          //   1. commitWipMarker captures uncommitted work into a throwaway `wip(park):`
          //      marker commit (best-effort; the .catch is belt-and-braces so nothing here
          //      undoes the requeue), exactly as the park path does — the shutdown branch
          //      previously did NOT do this, so uncommitted work was lost even from the marker.
          //   2. fetchBackBestEffort moves that tip to the local tracking ref.
          //   3. publishCheckpointBestEffort brokers the delta to refs/uzi-checkpoints/<branch>
          //      AFTER the fetch-back (so checkpointPack reads the current tip), one-shot and
          //      unconditional on the join-token seam (never a git push / PAT), same signature
          //      the park path uses. So a DIFFERENT worker re-claiming this requeued run
          //      recovers the committed tree instead of cold-starting from the default branch.
          //
          // BUDGET (issue #1030 M4 / PRD #1030): this whole best-effort sequence runs inside
          // the k8s termination grace. No terminationGracePeriodSeconds is set on the worker
          // pod (confirmed in controller/internal/kube/materializer.go), so the default 30s
          // applies: after SIGTERM the process must exit before SIGKILL at 30s. The local git
          // steps (marker, fetch-back) are sub-second; the ONLY step that can hang is the
          // publish RPC to the api, and the server-side maxPublishDuration is 60s — longer
          // than the entire grace — so the CLIENT must not simply await it. We cap the whole
          // sequence with a Promise.race against shutdownPublishTimeoutMs (default 15s): that
          // leaves ~15s of the 30s grace for the runtime's own SIGTERM teardown and for the
          // concurrently-aborted sibling runs' sequences (shutdown() aborts every active run
          // at once; their catch branches race the same wall clock, so the cost is the slowest
          // plus contention, not the sum). 15s is generous for a healthy publish yet caps a
          // hung/unreachable forge well short of both the 30s SIGKILL and the 60s server cap.
          // issue #1597 M1: the sequence resolves to a typed ShutdownCheckpointOutcome instead of a
          // boolean, so the feed names WHY a shutdown checkpoint did not land (and a published
          // checkpoint is never misreported by a later boundary error).
          const durability = (async (): Promise<ShutdownCheckpointOutcome> => {
            let publishOutcome: PublishOutcome | undefined;
            let permitSignal: AbortSignal | undefined;
            // issue #1597 M2: a surviving tick process group gets its bounded chance to go first.
            await this.awaitTickSurvivorsGone(flight);
            try {
              // PRD #1171 m4: the shutdown durability publish routes through the reap facade so
              // deadlineMs ≤ the shutdown publish timeout keeps it inside the k8s grace race
              // below. Claude/stub reaps via killAgentTree then runs the body byte-for-byte;
              // Codex quiesces+reaps the provider root (after its per-sink reconcile) and holds
              // the permit across the body.
              await this.reapForSink(
                executor,
                {
                  boundary: "shutdown",
                  deadlineMs: Math.min(this.codexBoundaryDeadlineMs, this.shutdownPublishTimeoutMs),
                },
                async (permit) => {
                  permitSignal = permit?.signal;
                  await this.git.commitWipMarker(worktreePath).catch(() => false);
                  await this.fetchBackBestEffort(barePath, worktreePath, branch, runId, runLog);
                  // PRD #1416 M3 (C6): bridge a divergent tracking tip BEFORE the shutdown publish so
                  // a resume adopts B, not the published tip. Best-effort — a shutdown checkpoint must
                  // never throw (it is inside the k8s termination grace race, D4).
                  await this.bridgeParkSinkBestEffort(barePath, branch, flight, runLog, "shutdown");
                  // PRD #1062 M2 (#1036): the path is reaped above, so the overlay's PAT
                  // default-fetch is permitted — a behind-on-workflows branch checkpoints durably.
                  const shutdownOverlay = await this.buildCheckpointOverlay(claim, flight, barePath);
                  publishOutcome = await this.publishCheckpointOutcome(
                    flight,
                    barePath,
                    branch,
                    shutdownOverlay,
                    permit?.signal,
                  );
                },
              );
            } catch (err) {
              // A NON-boundary throw propagates as before. A CodexBoundaryError must NOT fail the
              // requeue. issue #1597 M1: it no longer implies "nothing landed" — the boundary can
              // error AFTER the body's publish was ACKed (e.g. a late deadline/cleanup failure), and
              // the checkpoint on origin is then real: published WINS.
              if (!isCodexBoundaryError(err)) throw err;
              if (publishOutcome?.published === true) {
                runLog.warn(
                  "shutdown checkpoint boundary failed after the checkpoint was published to origin",
                  { run_id: runId, error: errMessage(err) },
                );
                return "published";
              }
              runLog.warn("shutdown checkpoint boundary blocked; checkpoint not published to origin", {
                run_id: runId,
                error: errMessage(err),
              });
              return isCodexBoundaryTimeout(err) ? "timeout" : "boundary_blocked";
            }
            return shutdownOutcomeOf(publishOutcome, permitSignal);
          })();
          // Claude retains the literal legacy budget race. Codex must not abandon a held
          // permit: its boundary signal cancels supervised children + the upload at the same
          // bounded deadline, and we await actual action/root settlement before terminal
          // safety.dispose or run-home cleanup can proceed.
          // issue #1597 M1: the Claude budget race resolving `undefined` IS the timeout class.
          const raced: ShutdownCheckpointOutcome = executor.safety
            ? await durability
            : ((await this.raceShutdownBudget(durability, this.shutdownPublishTimeoutMs)) ?? "timeout");
          // issue #1597 M2: when a cancelled mid-turn tick left a git lock in the worker bare that
          // could not be proven its own AND that lock STILL exists, a shutdown checkpoint that did
          // not land is named for that cause — the retained lock is what blocks the fetch-back/
          // publish. A lock that has since gone never relabels an unrelated failure.
          const outcome: ShutdownCheckpointOutcome =
            raced === "published"
              ? raced
              : this.tickSurvivorsRemain(flight)
                ? "tick_process_survived"
                : (await this.retainedBareLockRemains(flight))
                  ? "bare_lock_retained"
                  : raced;
          runLog.info("shutdown checkpoint outcome", { run_id: runId, outcome });
          // issue #1030 M4: surface the outcome on the feed the same way the park path does — a
          // direct batcher.emit, NOT deduped (it fires once per shutdown; only the generic
          // publish-failure lines go through the reportPublishOutcome dedupe). This lands
          // because it is emitted BEFORE the single batcher.close() below — the shutdown
          // branch closes the batcher exactly once, further down, never here.
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              // issue #1597 M1: the class only — no error message, remote text or credential. The
              // tail names the likely restart point. A checkpoint this run (or its claim) knows
              // landed is only adopted by a resume while it is still adoptable (runnerCloneForBranch
              // sets it aside when e.g. origin/<branch> exists and it does not descend it), so that
              // case is hedged; with no known checkpoint the default branch is named, as before.
              text:
                outcome === "published"
                  ? "shutdown checkpoint published to origin"
                  : `shutdown checkpoint NOT published (reason: ${outcome}) — a resume on another worker will restart from ${
                      flight.lastCheckpointRefTip
                        ? "the last published checkpoint if it is still adoptable, else the branch or default branch"
                        : "the default branch"
                    }`,
            },
          });
          // PRD #1349 M2 (D4.6): record this generation's restore point in the durable journal
          // so an interrupted run's exact hold can be dispositioned later. A shutdown is bounded
          // by the k8s termination grace (the durability publish above is already raced against
          // shutdownPublishTimeoutMs), so we do NOT run a credentialed fresh-forge capture here —
          // that could exceed the grace and be SIGKILLed. Pin only (local, credential-free,
          // sub-millisecond); settlement is left to the requeued run's reaped terminal or the
          // server reconciler, and an abrupt interruption legitimately retains custody (D3).
          await this.pinRecoveryGeneration(
            claim,
            flight,
            await this.currentRestorePointHead(flight),
          ).catch((e) =>
            runLog.warn("recovery: shutdown generation-evidence pin failed (custody retained)", {
              run_id: runId,
              error: errMessage(e),
            }),
          );
        }
        // PRD #556 M1: a shutdown interrupt now preserves the same two filesystem dirs a
        // park does (the sibling skills plugin dir and the per-run HOME holding the
        // resumable SDK transcript), so a same-worker re-claim within the affinity grace
        // can resume the SDK session instead of restarting it from scratch. Scoped to
        // ONLY those two fs removals in the finally — every other cleanup still runs.
        flight.preserveSession = true;
        runLog.info("run interrupted by worker shutdown; leaving it for requeue");
        await batcher.close().catch(() => undefined);
        // NO reportState: the run stays non-terminal so the server's sweeper requeues
        // it. Reporting `failed` here would turn a recoverable interruption into a dead
        // run — the exact outcome the fetch-back exists to prevent.
      } else if (err instanceof PauseNowSignal) {
        // PRD #1190 M2: a `now` pause aborted a turn OUTSIDE the implement loop's own catch (e.g.
        // during the plan turn), so the signal reached here. Caught BEFORE the generic terminal
        // path (the LimitReachedError precedent above) so a pause NEVER becomes a `failed` run.
        // Try to park it durably; on a park, flight.parked preserves the HOME exactly as a limit
        // park does. If the checkpoint could not be published (handlePausePark reported
        // pause_failed and left the run running), preserve the session and leave it for the
        // sweeper to requeue rather than failing a run the owner asked to pause.
        flight.parked = await this.handlePausePark(claim, flight, { completedCount: 0 });
        if (!flight.parked) {
          flight.preserveSession = true;
          runLog.info(
            "pause could not park this run outside the loop; leaving it for requeue",
          );
        }
        await batcher.close().catch(() => undefined);
      } else if (err instanceof ForgeUnreachableAtCloneError) {
        // PRD #1392 M2: the forge was unreachable at clone/fetch on a TRANSIENT error. Caught
        // BEFORE the generic terminal path (the LimitReachedError/PauseNowSignal precedent) so a
        // transient forge blip NEVER becomes a `failed` run. The handler attempts NO recovery
        // capture (there is nothing to capture pre-clone) and reports the park first, then
        // dispatches on the ack (D10): a confirmed `recovery_wait` park closes the batcher and
        // preserves HOME/session only when a resume transcript is resolvable, `stop` leaves a
        // server-terminal/stale run cleaned up with no re-report, and `fail` falls through to
        // today's failed path (batcher deliberately still open on that arm).
        const outcome = await this.handleForgeUnreachableAtClone(
          err,
          claim,
          flight,
          reportState,
          runLog,
          runHome,
        );
        if (outcome === "fail") {
          await this.reportGenericFailure(claim, flight, err);
        }
      } else if (err instanceof ServerWallParkedError) {
        // PRD #1497 M2 (D5/D16/D17): a fenced report revealed the run was SERVER-PARKED at its
        // wall-clock limit (paused + budget_exhausted). Caught BEFORE the StaleClaimError arm below:
        // unlike a plain stale supersede, KEEP everything — the run is non-terminal and a safe
        // incarnation will resume it from the last checkpoint, so set the preserve flags (retain the
        // clone + HOME), report NO terminal state (a `failed`/`completed` here would fight the park
        // and, per D17, could replay a journaled failure over the park), and close the batcher. The
        // finally's park carve-out then preserves the HOME + plugin dir; preserveRecoveryClone keeps
        // the clone. Mirrors the CredentialSwitchRetainedStop arm's retain-and-stop posture.
        flight.preserveRecoveryClone = true;
        flight.preserveSession = true;
        flight.parked = true;
        runLog.info(
          "run parked server-side at its wall-clock limit; retaining clone + HOME and leaving it non-terminal for a resume",
          { run_id: flight.runId },
        );
        await batcher.close().catch(() => undefined);
      } else if (err instanceof StaleClaimError) {
        // PRD #1247 M5b: a /state report came back with the stale_claim disposition — a held-state
        // credential switch RELEASED this claim, or a reclaim SUPERSEDED it. Another claim owns the
        // run now, so STOP this flight. Caught here BEFORE the generic terminal path (mirroring the
        // PauseNowSignal arm above, but WITHOUT its park): log, close the batcher, and report NO
        // terminal state — a `failed` here would fight the owning claim — and set NO preserve flag
        // (normal teardown: the new claim has its own clone). The finally then runs ordinary cleanup.
        runLog.info("run claim superseded server-side (stale_claim); stopping this flight");
        await batcher.close().catch(() => undefined);
      } else if (err instanceof RunningAckTerminalError) {
        // PRD #1391 Run B M4: phaseClone's first `running` report was refused with a TERMINAL status
        // (the run reached completed/failed/cancelled out from under this claim). STOP with NO
        // terminal report (the authoritative outcome already landed) and NO preserve flag — nothing
        // was cloned before the running report, so the finally runs ordinary teardown. Caught here
        // BEFORE the generic terminal path so this never becomes a `failed` run.
        runLog.info("run reached a terminal status before the phase clone started; stopping this flight", {
          run_id: flight.runId,
          status: err.runStatus,
        });
        await batcher.close().catch(() => undefined);
      } else if (err instanceof CredentialSwitchRetainedStop) {
        // PRD #1247 M5b (BLOCKING-2/3 rework): the in-place ctx.attemptCredentialSwitch could not
        // CONFIRM the switch (an unverified give-up whose stamp-clear the server never confirmed, or
        // a release whose outcome is unknown), so it retained all work and threw to stop rather than
        // continue on a possibly-released claim. Unlike StaleClaimError, KEEP the preserve flags
        // (enterCredentialSwitch left them SET) so the clone + HOME survive: report NO terminal state
        // (a `failed` would fight a claim that may already have moved on) and leave the run
        // NON-TERMINAL so the sweeper requeues it and a reclaim resumes the retained work.
        runLog.info("credential switch unconfirmed; retaining work and leaving the run non-terminal for requeue", {
          run_id: flight.runId,
        });
        await batcher.close().catch(() => undefined);
      } else if (err instanceof CredentialSwitchSignal) {
        // PRD #1247 M5b (data-integrity fix): a SAFETY NET. The switch is now handled IN PLACE by the
        // executor via ctx.attemptCredentialSwitch — the implement loop's turn catch and each idle
        // held-state waiter (plan gate / question / follow-up) call it and, on a give-up, CONTINUE the
        // run on the old token rather than letting the CredentialSwitchSignal reach here. So this arm
        // only fires when the signal escaped that in-place handling: a stub/test executor that does
        // NOT wire attemptCredentialSwitch (it re-throws), or a switch tripping some await not wrapped
        // at B/C. Caught here BEFORE the generic terminal path so a switch NEVER becomes a `failed`
        // run. Enter the same two-phase release; enterCredentialSwitch already set the flight's flags
        // and (on release) reported credential_switch, so NEITHER branch reports a terminal state.
        const outcome = await this.enterCredentialSwitch(claim, flight, runLog);
        if (outcome === "released") {
          // The flight ends: the finally retires the clone and preserves the HOME; the server
          // requeued the run; a reclaim resumes at resume_phase on the newly-chosen token. No
          // terminal report — enterCredentialSwitch already reported credential_switch.
          runLog.info("credential switch released this claim; leaving the run for a reclaim on the new token");
        } else {
          // GAVE UP and the signal reached HERE (not the in-place continue at B/C), so the executor
          // has already unwound and cannot continue on the old token. Make it CONTINUE-SAFE the only
          // way possible from here: NO terminal `failed` report (a switch must never fail a healthy
          // run), and KEEP both preserve flags (enterCredentialSwitch left them set on give-up) so the
          // run is left NON-TERMINAL with its clone + HOME retained for a requeue — the same posture as
          // the worker-shutdown-interrupt arm. The standing override still points at the new token, so
          // the requeued run's reclaim spends it. (After B/C this arm is a last resort — a running
          // give-up continues in place and never reaches here.)
          runLog.info(
            "credential switch did not release the claim and reached the outer catch; leaving the run non-terminal for requeue on the standing override",
          );
        }
        await batcher.close().catch(() => undefined); // idempotent (enterCredentialSwitch may have drained)
      } else if (err instanceof E2EDropExecutionError) {
        // PRD #1390 M4 (e2e drop seam): end the flight with NO terminal report, mirroring the
        // StaleClaimError arm. The run is left `running` (non-terminal) so the api's heartbeat
        // missing-run requeue (M2b) reclaims it after the fence; the finally drops the snapshot
        // entry and (with no clone) retires nothing. pauseClaimForE2E was already latched at the
        // throw site. Reachable only under UZI_E2E_DROP_ON_SENTINEL.
        runLog.info(
          "e2e drop-execution seam: ending flight with no terminal report (run left running for the missing-run requeue)",
        );
        await batcher.close().catch(() => undefined);
      } else {
        await this.reportGenericFailure(claim, flight, err);
      }
    } finally {
      // #1539: AWAIT the permanent-failure hook's settlement BEFORE the Codex registry disposal
      // below. The arms that do NOT go through reportGenericFailure (limit, pause, forge-unreachable,
      // shutdown, stale/credential-switch stops) never await it themselves, so without this a hook
      // reap still queued or running on the boundary `queueTail` would race safety.dispose (which
      // tears the registry down). awaitPermanentFailureSettled resolves immediately when the breaker
      // never tripped, swallows the handler's rejection, and is bounded by the reap deadline, so it
      // never throws and never stalls the finally.
      await batcher.awaitPermanentFailureSettled();
      // PRD #1171 m4 (F1): the FINAL Codex registry disposal, after EVERY durability sink has
      // settled. A Codex executor's run() no longer disposes its registry on the normal path —
      // its post-run sinks (park/shutdown/finalize) reap the provider root through withBoundary,
      // which needs the roots alive — so the runner disposes it here, once, on every path
      // (success, park, shutdown, generic failure). Harness-agnostic: guarded on
      // `executor.safety`, so a Claude/stub run (no safety) is untouched. Best-effort +
      // idempotent (disposeTools is), so a standalone-backstop dispose never double-disposes,
      // and a dispose failure can never convert a completed run into a failed one.
      if (executor.safety) {
        await executor.safety
          .dispose({ boundary: "terminal", deadlineMs: this.codexBoundaryDeadlineMs })
          .catch((e) => runLog.warn("codex terminal dispose failed", { error: errMessage(e) }));
      }
      // PRD #218 M1: drop the shutdown-registry entry. A terminal run (or a parked one)
      // must not stay abortable — shutdown() iterating a stale entry would abort a
      // controller nobody is watching, and the map would leak an entry per run.
      this.activeRuns.delete(runId);
      // PRD #1390 M2a: drop the active-run snapshot entry. Reaching this finally means the
      // run reached a terminal report OR parked-and-returned for a requeue — either way it is
      // no longer executing on this worker, so it must stop being listed in the snapshot. A
      // run parked at a gate (awaiting_approval/awaiting_input/awaiting_followup) never reaches
      // here (its execute promise stays live), so it stays listed in its held phase.
      this.snapshotRegistry?.remove(runId);
      // PRD #1391 M2: drop this run's re-arm registration. The batcher is closed by the
      // time we reach here, so a later drainer retire never needs to re-arm it; any
      // still-pending segments are drained and simply not re-armed (a no-op).
      this.rearm?.delete(runId);
      await steering.stop().catch(() => undefined);
      // PRD #41: drop this run's plan-approval deadline + gate-tracking (normally cleared
      // when the gate resolves terminally, but a run that ends by any other path must not
      // leak either).
      this.gateDeadlines.delete(runId);
      this.gatedRuns.delete(runId);
      // PRD #88: the clarification park's per-run state follows the SAME rule as the
      // gate maps above, and for the same reason main gives below — these are
      // in-memory per-run entries that would otherwise be held for the whole length of
      // a park. Correct on the resume path too: openQuestionIds is re-seeded from
      // claim.open_question_id, so clearing it here loses nothing a resume needs.
      this.questionDeadlines.delete(runId);
      this.openQuestionIds.delete(runId);
      this.questionCounts.delete(runId);
      // Evict this run's secrets from the logger (Decision 7). Reaching this finally
      // means execute() ran to a terminal report OR the run PARKED (PRD #35); a
      // requeue (worker death) still never returns here.
      //
      // This eviction runs on BOTH paths and is deliberately outside the park
      // carve-out below. The secrets are re-delivered on the next claim, and this
      // worker goes on to run other runs in the meantime — leaving a parked run's
      // decrypted PAT and Anthropic token registered in the logger for the days a
      // seven-day window can last is not a cleanup nicety, it is the security half
      // of the same decision.
      //
      // (This block used to assert "reaching this finally means execute() ran to a
      // terminal report … the evict/HOME-cleanup below only ever fire for a run that
      // will not resume". The park is the first path that reaches here and DOES
      // resume, so both sentences are corrected above rather than left to mislead.)
      for (const s of runScopedSecrets) this.log.removeSecret(s);
      // ── The park / shutdown carve-out (PRD #35 Decision 6a; PRD #556 M1) ──────
      // EXACTLY two filesystem removals below are skipped for a parked run, and
      // nothing else in this finally is. The two are what a resume needs: the
      // sibling skills plugin dir, and the per-run HOME that holds the resumable
      // SDK transcript. Preserving only one of them would resume into a session
      // missing its plugins or its transcript.
      //
      // (PRD #556 M1: a WORKER-SHUTDOWN interrupt now ALSO preserves these exact
      // two dirs, via the separate `preserveSession` flag — a same-worker re-claim
      // within the affinity grace resumes the SDK session instead of restarting it.
      // It is deliberately scoped to ONLY these two fs removals, exactly like the
      // park flag; the secret eviction above and the steering.stop() / gate-map
      // deletes stay structurally upstream and unguarded, so neither flag reaches
      // them. The widening warning below applies to `preserveSession` too — it must
      // never be extended to cover any of those.)
      //
      // #1197, verified 2026-09-08: normal cleanup removes the clone and its
      // worker-owned ownership journal. Unverified recovery work keeps both:
      // preserveRecoveryClone guards only this filesystem removal, and the next
      // same-run claim recaptures the retained source before reseeding. A verified
      // tracking snapshot restores PRD #218 M6's normal removal behavior.
      //
      // The other four statements in this block — the steering poller stop, the two
      // gate-map deletes, and the secret eviction above — MUST still run on a park.
      // Guarding the whole block (or returning early) would leave a poller running
      // and a gate deadline registered for what may be days.
      //
      // ⚠ HOW EACH ONE FAILS IF YOU WIDEN `!parked` (or the sibling `!preserveSession`
      // from PRD #556 M1) TO COVER IT, because they do not fail alike and only one of
      // them fails legibly — neither flag may ever reach these upstream statements:
      //   - the SECRET EVICTION fails SILENTLY. Measured by the M1 reviewer: guarding
      //     it passed typecheck and every runner test with exit 0, leaving a parked
      //     run's decrypted PAT and Anthropic token in the logger for the length of
      //     the window. There is now a test for exactly that ("still evicts the
      //     run-scoped secrets ... when the run parks"); it is the only thing
      //     standing between that mistake and a green build.
      //   - `steering.stop()` fails as a PROCESS HANG, not a red assertion: the
      //     poller keeps the event loop alive and the test file never exits. Read
      //     CLAUDE.md before concluding flake — `node --test` prints `ℹ fail 0`
      //     for a timeout, so the tally will say everything passed while the exit
      //     code says otherwise. A hang here is this bug until proven otherwise.
      if (flight.worktreePath && !flight.preserveRecoveryClone) {
        try {
          if (flight.barePath && flight.branch) {
            // issue #1315: retire the clone ATOMICALLY (rename-to-holding, THEN clear the
            // journal) instead of the old removeRunnerClone + clearRecoveryCapture pair.
            // A daemon racing a recursive rm could throw ENOTEMPTY between the two and
            // leave the journal pointing at partial residue, wedging the branch forever.
            // discard: this is the owner's own terminal trash, so dispose the holding dir.
            await this.git.retireRunnerClone(
              flight.barePath,
              flight.worktreePath,
              flight.branch,
              runId,
              { discard: true },
            );
          } else {
            // No bare/branch to key the journal on (a run that never journaled): fall
            // back to the bare recursive remove.
            await this.git.removeRunnerClone(flight.worktreePath);
          }
        } catch (e) {
          runLog.warn("runner clone cleanup failed", { error: errMessage(e) });
        }
      }
      // PRD #1392 M2: whether to KEEP the two resume artifacts (the sibling skills plugin dir
      // and the per-run HOME). Every park/shutdown that has on-disk state to resume keeps them on
      // `parked` OR `preserveSession` as before — EXCEPT a pre-clone forge-unreachable park, which
      // captured no clone and no model session: it keeps them ONLY when a resume transcript is
      // resolvable on this worker (`preserveSession`), never on `parked` alone (D4). So a fresh
      // forge-parked claim and a resume leg without a local transcript still clean up fully.
      const preserveResumeArtifacts = flight.preClonePark
        ? flight.preserveSession
        : flight.parked || flight.preserveSession;
      // Tear down the sibling skills plugin dir the executor synthesized (PRD #16
      // M4). It is OUTSIDE the runner clone, so removeRunnerClone does not reach it;
      // leave it and each run leaks a dir. Best-effort, like the clone cleanup.
      if (flight.worktreePath && !preserveResumeArtifacts) {
        await fs
          .rm(skillsPluginDir(flight.worktreePath), { recursive: true, force: true })
          .catch((e) =>
            runLog.warn("skills plugin cleanup failed", {
              error: errMessage(e),
            }),
          );
      }
      // Remove this run's private HOME (agent-home/<runId>, Decision 5). The SDK
      // session transcript under it is only needed to resume, and a terminal run
      // never resumes. A concurrent sibling's HOME is a distinct dir, untouched.
      //
      // rmHomeTree, not fs.rm (PRD #108 M6, #1607): the Go module cache under this HOME
      // writes its package directories mode 0555, and `force: true` suppresses
      // ENOENT — not the EACCES that unlinking inside a read-only directory
      // raises. Every Go-touching run stranded its module cache (167.3 MB
      // measured for one run). Still best-effort and still swallowing its own
      // error: this is a `finally`, and a cleanup that threw would convert a
      // completed run into a failed one, which is strictly worse than a leak.
      if (runHome && !preserveResumeArtifacts) {
        await rmHomeTree(runHome).catch((e) =>
          runLog.warn("run HOME cleanup failed", { error: errMessage(e) }),
        );
      }
      if (flight.preClonePark) {
        // PRD #1392 M2: a pre-clone forge-unreachable park. It preserves HOME/session only when a
        // resume transcript is resolvable (D4); say which, so an operator reading disk pressure
        // (or the absence of it) can connect it to the park rather than to a leak.
        runLog.info(
          flight.preserveSession
            ? "run parked (forge unreachable at clone); preserving its plugin dir and HOME for a same-worker resume"
            : "run parked (forge unreachable at clone); no clone or session to preserve",
          {
            run_home: runHome,
          },
        );
      } else if (flight.parked) {
        // ~170 MB of Go module cache was measured under a single run HOME, so say
        // what is being held and why — an operator reading disk pressure needs to
        // connect it to a parked run rather than to a leak.
        runLog.info(
          "run parked; preserving its plugin dir and HOME for resume",
          {
            run_home: runHome,
          },
        );
      } else if (flight.preserveSession) {
        // PRD #556 M1: a shutdown-preserved HOME holds the same ~170 MB of Go module
        // cache a parked one does, so log it with its provenance too — an operator
        // reading disk pressure needs to connect the held dir to the shutdown interrupt
        // (a same-worker resume) rather than to a leak.
        runLog.info(
          "run interrupted by worker shutdown; preserving its plugin dir and HOME for a same-worker resume",
          {
            run_home: runHome,
          },
        );
      }
    }
  }

  /** PRD #1391 Run B M3b: the terminal-resolve deps for this runner, or undefined when no usable
   *  outbox is wired (a test without spill, or a store that failed closed) — in which case the
   *  write-ahead terminal send path falls back to today's un-journaled `reportState`. */
  private terminalDeps(): TerminalOutboxDeps | undefined {
    if (!this.outbox || this.outbox.isDisabled()) return undefined;
    return {
      outbox: this.outbox,
      client: this.client,
      gapFillMax: this.gapFillMax,
      terminalMaxBytes: this.outboxTerminalMaxBytes,
      log: this.log,
    };
  }

  /**
   * PRD #1391 Run B M3b: journal a run-lane terminal outcome WRITE-AHEAD (before the first network
   * attempt), then resolve it over `send` (the phase-publish reportState choke point at a run site,
   * or flight.reportState at reportGenericFailure / the permanent-failure hook). Falls back to a
   * direct un-journaled send when no outbox is wired. The caller MUST have closed / final-flushed the
   * batcher first, so `batcher.currentSeq()` is the run's DURABLE emitted tail — the fence source,
   * never a server value.
   */
  private async journalAndSendTerminal(
    flight: RunFlight,
    phase: string,
    body: Parameters<RunFlight["reportState"]>[0],
    send: SendTerminalState,
    // #1539: an optional hook that runs AFTER the durable install and BEFORE the resolve/send (on
    // the no-outbox branch too, before the direct `send`). The permanent-failure hook uses it to abort
    // the attempt and reap the provider WHILE the run is still actively-claimed — the journal is on
    // disk first (D5), the reap runs before the terminal is sent, and the reconcile is authorized
    // because the abort has not yet reported terminal. Every other caller passes nothing, so their
    // behaviour is identical.
    beforeResolve?: () => Promise<void>,
  ): Promise<void> {
    const deps = this.terminalDeps();
    if (!deps) {
      // No usable outbox: run beforeResolve (abort + reap) then send un-journaled exactly as today. A
      // stale ack still THROWS StaleClaimError out of `send` and propagates to executeClaim's catch
      // (there is no journal to stale-retire on this degradation path), so this branch is otherwise
      // byte-for-byte unchanged.
      await beforeResolve?.();
      await send(body);
      flight.terminalResolved = true;
      return;
    }
    // PRD #1391 Run B M3b (B1): the run-lane reportState choke point (flight.reportState, reached
    // through every `send` closure a terminal site passes here) THROWS StaleClaimError on a stale ack
    // instead of RETURNING it — the shape resolvePendingTerminal's catch would otherwise read as a
    // transport failure and KEEP the journal forever, leaking the run's whole outbox tree and holding
    // its terminal_pending lease (and, past the cap, pending_overflow) open (D11). Normalize the throw
    // into the `{applied:false, staleClaim:true}` ack actOnAck stale-retires on, so a superseded
    // generation local-retires the journal UNIFORMLY across lanes (the judge/review/boot lanes already
    // return this shape via raw client.reportState). StaleClaimError is defined and caught entirely
    // within this file — it never leaks into terminal-resolve.ts. A genuine transport error still
    // propagates, and resolvePendingTerminal keeps the journal for a later resolve, exactly as
    // designed. #1539: this wrapper wraps the send passed to BOTH branches (the reserve-exhausted
    // unjournaled send and the journaled resolve), the same as journalAndResolveTerminal does today.
    const wrappedSend: SendTerminalState = async (b, sig) => {
      try {
        return await send(b, sig);
      } catch (err) {
        if (err instanceof StaleClaimError) return { applied: false, staleClaim: true };
        throw err;
      }
    };
    const fence = flight.batcher.currentSeq();
    const installed = await installTerminalWriteAhead(deps, {
      runId: flight.runId,
      claimGeneration: flight.claimGeneration,
      phase,
      messagesThroughSeq: fence,
      body,
    });
    // #1539: the durable install is now on disk. Run the hook (abort + reap for the permanent
    // failure hook) BEFORE the resolve/send.
    await beforeResolve?.();
    if (!installed.journaled) {
      // reserve_exhausted: send unjournaled. A throw here propagates (skipping the latch below), so the
      // executor catch finds NO journal and takes today's fallback — unchanged from journalAndResolveTerminal.
      await sendUnjournaledTerminal(deps, installed.canonical, fence, wrappedSend);
    } else {
      await resolvePendingTerminal(deps, {
        runId: flight.runId,
        claimGeneration: flight.claimGeneration,
        send: wrappedSend,
      });
    }
    // PRD #1391 Run B M3 (N2/D5): latch that a terminal outcome for this generation has resolved, so
    // a later reportGenericFailure never reports a SECOND `failed` — even after a 200 RETIRED the
    // journal (hasPendingTerminal then reads false). Skipped on a throw above (reserve_exhausted's
    // un-journaled send that failed), so that fallback still reports failed as today.
    flight.terminalResolved = true;
  }

  /**
   * PRD #1391 Run B M3 (D5) / #1539: the permanent message-failure hook body, extracted from the
   * batcher's onPermanentFailureReport closure so its ORDERING is unit-testable against the REAL
   * shipping code (runner-terminal-journal.test.ts) rather than a synthetic hand-rolled handler.
   *
   * The order is: (1) DURABLE first-writer-wins install of the `failed` journal, then (2) ABORT the
   * attempt, then (3) REAP this generation's provider WHILE the run is still actively-claimed, then
   * (4) resolve/send the terminal. Steps 2+3 run in the `beforeResolve` hook, between the install and
   * the send, so a completion racing the trip can never reverse the first durable winner (the
   * no-replace install in journalTerminal arbitrates, D4) AND the Codex per-sink credential reconcile
   * inside the reap's withBoundary is authorized (the abort has not yet reported terminal, so the run
   * is still in codexActivelyClaimedStatuses — reaping AFTER the terminal report would be refused 409
   * and leak the hold as source_only, the #1539 bug). The install must NOT move after the abort and
   * the reap must NOT move before the install, or the #1391 completion race reopens.
   *
   * The reap outcome is recorded on the flight (`permanentFailureReap`) together with the safety epoch
   * it reaped (`permanentFailureReapSafety`), so reportGenericFailure's already-journaled arm settles
   * custody ONCE — only when the reap confirmed and the executor's safety epoch has not since swapped
   * (the stale-epoch guard, {@link permanentFailureReapValid}). execute() unwinds into
   * reportGenericFailure, which awaits this settlement, finds the durable journal / the terminalResolved
   * latch, and reports no second `failed` (fact 2). When no outbox is wired this degrades to today's
   * direct `failed` report + abort + reap. The abort is guarded so a concurrent abort (a racing
   * steering-cancel/shutdown) is never doubled.
   *
   * A process-local resolve hold (Outbox.holdTerminalResolve) is taken BEFORE the install and released
   * in the `finally`, so the per-worker drainer (Worker.resolveRunTerminal) cannot send the journaled
   * `failed` during the abort-then-reap window and race this hook's own resolve. If the release reports
   * the drainer skipped a resolve while the journal is still pending, the hook re-drives one resolve so
   * the terminal is not stranded until boot (N4).
   */
  private async handlePermanentFailure(claim: ClaimResponse, flight: RunFlight, reason: string): Promise<void> {
    const outbox = this.outbox;
    if (outbox) outbox.holdTerminalResolve(flight.runId, flight.claimGeneration);
    try {
      await this.journalAndSendTerminal(
        flight,
        TERMINAL_JOURNAL_PHASE,
        {
          status: "failed",
          failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
        },
        (b, sig) => flight.reportState(b, sig),
        async () => {
          // (2) Abort the attempt ONLY AFTER the durable journal is installed (D5/D4), so execute()
          // falls into its catch (→ reportGenericFailure, which awaits this handler's settlement,
          // finds the durable journal / the terminalResolved latch, and does NOT report a second
          // `failed`). (3) Then reap this generation's provider while the run is still actively-claimed
          // and record the outcome + the reaped safety epoch, for reportGenericFailure to settle on.
          if (!flight.cancel.signal.aborted) flight.cancel.abort();
          // Capture the epoch BEFORE the await: reapForSink reads executor.safety synchronously at
          // entry, and a checkpoint can swap it while the reap is in flight. Recording it afterwards
          // would compare the NEW epoch with itself and let the settle run under a provider this
          // reap never quiesced.
          const reapedSafety = flight.executor.safety;
          flight.permanentFailureReap = await this.reapRecoveryProviderForSettle(
            claim,
            flight,
            flight.runLog,
            "terminal",
          );
          flight.permanentFailureReapSafety = reapedSafety;
        },
      );
    } catch (e) {
      flight.runLog.error("could not journal/report the message-transport failure", {
        error: errMessage(e),
      });
      // The install threw before beforeResolve could run, so the fallback abort still fires (guarded
      // so a concurrent abort is never doubled), unwinding execute() into reportGenericFailure.
      if (!flight.cancel.signal.aborted) flight.cancel.abort();
    } finally {
      if (outbox) {
        const { skipped } = outbox.releaseTerminalResolve(flight.runId, flight.claimGeneration);
        // N4: the drainer skipped a resolve while the hold was set and the journal is still pending
        // (this hook's own resolve did not retire it — e.g. a benign 409 running). Re-drive one
        // resolve with the same stale-normalized send so the terminal is not stranded until boot.
        const deps = this.terminalDeps();
        if (skipped && deps && deps.outbox.hasPendingTerminal(flight.runId, flight.claimGeneration)) {
          await resolvePendingTerminal(deps, {
            runId: flight.runId,
            claimGeneration: flight.claimGeneration,
            send: async (b, sig) => {
              try {
                return await flight.reportState(b, sig);
              } catch (err) {
                if (err instanceof StaleClaimError) return { applied: false, staleClaim: true };
                throw err;
              }
            },
          }).catch((e) =>
            flight.runLog.warn("outbox: post-release terminal re-resolve failed; leaving the journal", {
              run_id: flight.runId,
              error: errMessage(e),
            }),
          );
        }
      }
    }
  }

  /** #1539: the stale-epoch guard for the permanent-failure hook's reap. The hook records the
   *  reap outcome AND the executor safety epoch it reaped; a consumer settles custody ONLY IF the reap
   *  confirmed AND the executor's safety epoch is still the one reaped. CodexExecutor swaps
   *  `this.safety = epoch.safety` after each checkpoint (codex/codex-executor.ts), so a hook that
   *  tripped during a checkpoint boundary reaped the OLD epoch while a new provider started — settling
   *  then would run the credentialed fetch against a live newer provider. For Claude/stub both sides
   *  are `undefined` (equal), so it settles as before. Otherwise it fails closed and keeps the hold. */
  private permanentFailureReapValid(flight: RunFlight): boolean {
    return flight.permanentFailureReap === true && flight.executor.safety === flight.permanentFailureReapSafety;
  }

  /**
   * Today's generic terminal FAILED path (extracted verbatim so the pre-clone
   * forge-unreachable park's `fail` arm can reuse it). failure_reason goes straight to
   * reportState, bypassing the batcher's redactor, and the sdk-executor catch-all re-throws raw
   * SDK errors into this path — so scrub it here with the run's own secret set. A plan rejection
   * carries the user's verbatim reason; scrubbing it too is harmless for plain text and a safety
   * net if the user pasted a secret. The caller MUST leave the batcher OPEN — this emits the
   * error line and closes it.
   */
  private async reportGenericFailure(
    claim: ClaimResponse,
    flight: RunFlight,
    err: unknown,
  ): Promise<void> {
    const { batcher, redactText, runLog } = flight;
    const rawReason =
      err instanceof PlanRejectedError
        ? err.reason
        : err instanceof TerminalReportError
          ? err.reason
          : errMessage(err);
    const reason = redactText(rawReason);
    // PRD #69 M7a: derive the TRUSTED failure class from the RAW reason (before
    // redaction) so a fatal pre-start failure (provisioning / no token) carries a
    // structured origin the judge can key on; an ordinary agent failure maps to
    // undefined and the server defaults it to 'agent_failure'. PRD #1077: a
    // TerminalReportError carries its own typed origin (push_secret_blocked) whose
    // terminal report threw after exhausting retries — honor it verbatim.
    const failOrigin =
      err instanceof TerminalReportError
        ? err.failOrigin
        : failOriginForReason(rawReason);
    runLog.error("run failed", { error: reason });
    // PRD #1391 Run B M3 (D5): if the permanent-failure hook tripped, AWAIT its settlement first — it
    // journals `failed` durably and aborts the attempt (which routed us here), so the journal must be
    // observed as installed before the hasPendingTerminal check below. Resolves immediately when the
    // breaker never tripped (every ordinary failure), so this is a no-op on the common path.
    await batcher.awaitPermanentFailureSettled();
    // PRD #1391 Run B M3 (fact 2, D5, N2): a JOURNALED/RESOLVED outcome is FINAL. Do NOT fall through
    // to a SECOND `failed` when EITHER a terminal outcome for this generation has already resolved
    // (the `terminalResolved` latch — set even after a 200 RETIRED the journal, so hasPendingTerminal
    // reads false) OR a write-ahead journal is still installed (the permanent-failure hook's durable
    // `failed`, or a terminal site that journaled before throwing into this catch). The first durable
    // winner stands (the no-replace install arbitrates, D4). Still close the batcher and settle
    // recovery custody (clone cleanup is independent of the report).
    if (
      flight.terminalResolved ||
      (this.outbox && this.outbox.hasPendingTerminal(flight.runId, flight.claimGeneration))
    ) {
      runLog.info("run outcome already journaled write-ahead; not reporting a second failed", {
        run_id: flight.runId,
        claim_generation: flight.claimGeneration,
      });
      await batcher.close().catch(() => undefined);
      // #1539: when the permanent-failure hook handled this terminal it ALREADY reaped the
      // provider between the install and the send (while still actively-claimed), so there is no
      // second reap here — settle custody ONCE, gated by the stale-epoch guard. Any OTHER writer that
      // reaches this arm (the finalize sites at :2600/:2611) already settles via driveRecoveryTerminal
      // under the finalize boundary, so its reapThenSettle here is a redundant backstop kept unchanged.
      if (flight.permanentFailureReap !== undefined) {
        if (this.permanentFailureReapValid(flight)) await this.settleRecoveryGeneration(claim, flight, runLog);
        return;
      }
      await this.reapThenSettleRecoveryGeneration(claim, flight, runLog, "terminal");
      return;
    }
    batcher.emit({
      kind: "error",
      agent: "worker",
      payload: { text: reason },
    });
    await batcher.close().catch(() => undefined);
    // PRD #1349 M2 (D4.5) / #1531: REAP THIS generation's provider FIRST, BEFORE the `failed`
    // report. A steering-cancel and an early agent failure both land here (a cancel aborts the
    // controller with the same error, then reports failed), and neither reaches the finalization
    // pin — so without a disposition the hold leaks. A Codex run's pre-settle reap runs the
    // per-sink credential reconcile (refreshCodex/releaseCodex) INSIDE withBoundary, which the api
    // authorizes ONLY while the run is actively-claimed (codexActivelyClaimedStatuses); once the
    // status is terminal the reconcile is refused (409), which would block the reap and leak the
    // hold as source_only. So reap while the run is still `running`. The reap is deadline-bounded;
    // a Claude/SDK reap is an idempotent killAgentTree on an already-dead tree; a pre-clone
    // failure has no clone and no-ops (returns false). The settle runs AFTER the report below.
    // #1539: this fall-through is reached under the hook when its UNJOURNALED send threw (no latch,
    // no journal) — the hook already reaped, so reuse that outcome under the stale-epoch guard rather
    // than reap a second time (a second reap would be refused if the lost-ack send had actually landed).
    const reaped =
      flight.permanentFailureReap !== undefined
        ? this.permanentFailureReapValid(flight)
        : await this.reapRecoveryProviderForSettle(claim, flight, runLog, "terminal");
    // Cap what lands in the run row (matches the GitLab error-body cap). Journal it WRITE-AHEAD then
    // resolve it (D3): on reserve_exhausted it degrades to today's direct send, whose throw the
    // .catch below still logs; on the journaled path journalAndSendTerminal never throws (a send that
    // fails now leaves the durable journal for a later resolve).
    await this.journalAndSendTerminal(
      flight,
      TERMINAL_JOURNAL_PHASE,
      {
        status: "failed",
        failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
        fail_origin: failOrigin,
      },
      (b, sig) => flight.reportState(b, sig),
    ).catch((e) =>
      runLog.error("could not report failed state", {
        error: errMessage(e),
      }),
    );
    // PRD #1349 M2 (D4.5) / #1531: now the provider is reaped, run the non-status-gated custody
    // settle AFTER the terminal report. The credentialed fresh-forge comparison is safe reaped: a
    // provably empty run RELEASES its exact hold, a run that committed work then failed CAPTURES
    // it, and a failed/unverifiable comparison RETAINS — never a wrong release. Best-effort;
    // skipped on a reap that did not confirm (guard miss or a blocked/failed reap keeps the hold).
    if (reaped) await this.settleRecoveryGeneration(claim, flight, runLog);
  }

  /**
   * PRD #1392 M2 — the pre-clone forge-unreachable park handler (D7/D10). `ensureClone` exhausted
   * its transient-retry budget with a transient verdict, so there is NOTHING to capture: the
   * worker attempts no recovery capture and REPORTS FIRST, its report shape chosen from what the
   * api advertised at `register`:
   *
   *   - `recovery_park_cause`  → the TYPED report {recovery_wait, recovery_cause:forge_unreachable,
   *      claim_generation}, then dispatch on the ack (the api settles custody + park in one locked
   *      transaction — the worker calls no release endpoint).
   *   - `recovery_release_exact_echo` (but NOT recovery_park_cause) → the older-api fallback: an
   *      older api's park touches no custody (fact 13), so first prove an EXACT-generation release
   *      (released && generation===gen && holds_released===1), then send the UNTYPED report, which
   *      still carries claim_generation — the reportState closure (M5b) threads it, and for a capability worker the E send-gate stamps it
   *      regardless of the negotiated `claim_generation_fence` feature (strict-decode strip-and-retry if this older api also predates the /state field). Missing proof (or a
   *      release throw) → today's failed path, never a leaked hold.
   *   - neither token → negotiate nothing, take today's failed path.
   *
   * Returns "parked" (handler closed the batcher, set the flight flags), "stop" (a server-terminal
   * or stale run: batcher closed, no re-report), or "fail" (the caller runs today's failed path
   * with the batcher STILL OPEN).
   */
  private async handleForgeUnreachableAtClone(
    err: ForgeUnreachableAtCloneError,
    claim: ClaimResponse,
    flight: RunFlight,
    reportState: RunFlight["reportState"],
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<"parked" | "stop" | "fail"> {
    const features = this.client.protocolFeatures;
    const gen = claim.claim_generation;
    runLog.warn("forge unreachable at clone; attempting a pre-clone recovery park", {
      run_id: flight.runId,
      detail: err.message,
      protocol_features: features,
    });

    if (features.includes("recovery_park_cause")) {
      // Full #1392 api: the TYPED report. The api's park transaction settles this generation's
      // hold and parks (or fails at the cap) in one shot, so the worker calls NO release endpoint.
      const body: StateRequest = {
        status: "recovery_wait",
        recovery_cause: "forge_unreachable",
        ...(gen !== undefined ? { claim_generation: gen } : {}),
      };
      return await this.reportForgeParkAndDispatch(body, claim, flight, reportState, runLog, runHome);
    }

    if (features.includes("recovery_release_exact_echo")) {
      // Older-api fallback (D7): an older api's park touches NO custody (fact 13), so the worker
      // must first release its own exact-generation hold and require POSITIVE proof of an
      // exact-generation release before it dares park. NO release_evidence — the proof is the
      // echo, and an older api need not accept the field.
      let rel;
      try {
        rel = await this.client.releaseRecoveryCustody(flight.runId, gen);
      } catch (relErr) {
        runLog.warn("forge park fallback: exact release call failed; taking the failed path (no park)", {
          run_id: flight.runId,
          error: errMessage(relErr),
        });
        return "fail";
      }
      const proven =
        gen !== undefined &&
        rel.released === true &&
        rel.generation === gen &&
        rel.holds_released === 1 &&
        rel.retained !== true;
      if (!proven) {
        runLog.warn("forge park fallback: exact release not positively confirmed; taking the failed path (no park)", {
          run_id: flight.runId,
          released: rel.released,
          echoed_generation: rel.generation,
          holds_released: rel.holds_released,
          retained: rel.retained,
        });
        return "fail";
      }
      // With proof, send the UNTYPED recovery_wait report; the flight reportState closure it routes
      // through (reportForgeParkAndDispatch) stamps claim_generation unconditionally (PRD #1247 M5b).
      const body: StateRequest = { status: "recovery_wait" };
      return await this.reportForgeParkAndDispatch(body, claim, flight, reportState, runLog, runHome);
    }

    // Neither token → send NOTHING negotiated; take today's failed path (never park).
    runLog.info("forge park: api advertised no recovery-park feature; taking today's failed path", {
      run_id: flight.runId,
    });
    return "fail";
  }

  /**
   * PRD #1392 M2 — send the forge park report ONCE and dispatch on the ack (D10). A generic 400
   * (an api that rejects the fields) → today's failed path, never park. A transport failure AFTER
   * the send is an UNKNOWN outcome, never a failure: reconcile via the ownership probe.
   */
  private async reportForgeParkAndDispatch(
    body: StateRequest,
    claim: ClaimResponse,
    flight: RunFlight,
    reportState: RunFlight["reportState"],
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<"parked" | "stop" | "fail"> {
    let ack: StateAck;
    try {
      ack = await reportState(body);
    } catch (reportErr) {
      if (reportErr instanceof RequestError && reportErr.status === 400) {
        // The api rejected the negotiated fields (predates them) → today's failed path.
        runLog.warn("forge park report rejected 400; taking the failed path (no park)", {
          run_id: flight.runId,
        });
        return "fail";
      }
      // Transport failure / exhausted-transient throw: the server MAY have committed release +
      // park and lost the ack, so reconcile via the read-only ownership probe until it is known.
      // NEVER clean a possibly-parked session on an unknown outcome (D10).
      runLog.warn("forge park report failed transport; reconciling by ownership probe", {
        run_id: flight.runId,
        error: errMessage(reportErr),
      });
      return await this.reconcileForgeParkByProbe(body, claim, flight, reportState, runLog, runHome);
    }
    return await this.dispatchForgeParkAck(ack, claim, flight, runLog, runHome);
  }

  /**
   * PRD #1392 M2 (D10) — act on the forge park report's ack. reportState reads BOTH a 200 and a
   * 409 for their body, so the ack carries the run's authoritative status plus the top-level
   * `reason`. Precedence: a `stale_claim`/`custody_unsettled` reason (409) is checked before the
   * status, then a `recovery_wait` status (a fresh 200 park OR an idempotent 409 duplicate-park),
   * then any authoritative terminal status, then any other 409 → today's failed path.
   */
  private async dispatchForgeParkAck(
    ack: StateAck,
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<"parked" | "stop" | "fail"> {
    const { batcher } = flight;
    if (ack.reason === "stale_claim") {
      // #1247: the run moved on under this worker. Stop silently, no further report.
      runLog.info("forge park: ack stale_claim; stopping silently", { run_id: flight.runId });
      await batcher.close().catch(() => undefined);
      return "stop";
    }
    if (ack.reason === "custody_unsettled") {
      // Fail-safe: leave the batcher OPEN and take today's failed path.
      runLog.info("forge park: ack custody_unsettled; taking today's failed path", { run_id: flight.runId });
      return "fail";
    }
    if (ack.status === "recovery_wait") {
      await this.finishForgePark(ack.recoveryRetryNotBefore, claim, flight, runLog, runHome);
      return "parked";
    }
    if (ack.status && TERMINAL_RUN_STATUSES.has(ack.status)) {
      // cancelled, or failed when the cap branch committed the terminal state, or any other
      // authoritative terminal status: close the still-open batcher, no park event, no second
      // report. The finally does the (empty, pre-clone) cleanup.
      runLog.info("forge park: ack was authoritative terminal; cleaning up without a park", {
        run_id: flight.runId,
        status: ack.status,
      });
      await batcher.close().catch(() => undefined);
      return "stop";
    }
    // Any other 409 (or an unmodelled non-park, non-terminal status): today's failed path,
    // batcher left OPEN.
    runLog.info("forge park: unrecognised ack; taking today's failed path", {
      run_id: flight.runId,
      status: ack.status,
      reason: ack.reason,
    });
    return "fail";
  }

  /**
   * PRD #1392 M2 (D4) — set `preserveSession` for a same-worker resume ONLY when a resume
   * transcript is resolvable on THIS worker (a resume leg whose session id we can actually
   * resume). A fresh claim (no session id) and a resume leg without a local transcript preserve
   * nothing. Shared by finishForgePark (a CONFIRMED park) and reconcileForgeParkByProbe's
   * worker-shutdown arm (which leaves the run non-terminal for the server to requeue), so both
   * honor the D4 rule identically — HOME + the SDK session are kept when resolvable, else cleaned.
   */
  private async preserveForgeParkSessionIfResolvable(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<void> {
    const sessionId = claim.session_id;
    if (
      runHome &&
      sessionId &&
      (await sessionTranscriptResolvable(runHome, sessionId, runLog))
    ) {
      flight.preserveSession = true;
    }
  }

  /**
   * PRD #1392 M2 (D4/D10) — a CONFIRMED forge park. Emit and flush exactly ONE feed event quoting
   * the acknowledged retry stamp (or "after backoff" when absent), close the batcher, then
   * preserve HOME + the SDK session ONLY when a resume transcript is resolvable on THIS worker
   * (a resume leg whose session id we can actually resume) — a fresh claim and a resume leg
   * without a local transcript preserve nothing. `parked` + `preClonePark` mark the run so the
   * finally treats it as a park (no terminal report) whose resume artifacts follow preserveSession.
   */
  private async finishForgePark(
    retryAt: string | undefined,
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<void> {
    const { batcher } = flight;
    const when = retryAt ? `retry at ${retryAt}` : "retry after backoff";
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `forge unreachable at clone; parked, ${when}` },
    });
    await batcher.flush().catch(() => undefined);
    await batcher.close().catch(() => undefined);
    await this.preserveForgeParkSessionIfResolvable(claim, flight, runLog, runHome);
    flight.parked = true;
    flight.preClonePark = true;
    runLog.info("run parked: forge unreachable at clone", {
      run_id: flight.runId,
      preserve_session: flight.preserveSession,
      retry_not_before: retryAt ?? null,
    });
  }

  /**
   * PRD #1392 M2 (D10) — reconcile a forge park whose report threw AFTER the send (an unknown
   * outcome). The read-only ownership probe is authoritative: `recovery_wait` → the park landed,
   * preserve + emit (quoting the probe's recovery_retry_not_before, or "after backoff"); `running`
   * → the transaction did not land, resend the SAME (idempotent) report and dispatch its ack; an
   * authoritative terminal → today's cleanup (no park); a 404 (stale_claim / not owned / reclaimed)
   * → stop. A transport failure ON the probe is itself unknown, so retain the (possibly parked)
   * session and retry — NEVER clean it — until the outcome is known.
   */
  private async reconcileForgeParkByProbe(
    body: StateRequest,
    claim: ClaimResponse,
    flight: RunFlight,
    reportState: RunFlight["reportState"],
    runLog: Logger,
    runHome: string | undefined,
  ): Promise<"parked" | "stop" | "fail"> {
    const { batcher } = flight;
    for (;;) {
      if (this.shuttingDownGlobal) {
        // Worker draining: leave the (non-terminal) run for the server to requeue; do NOT clean a
        // possibly-parked session. Preserve HOME + the SDK session by the SAME D4 rule
        // finishForgePark uses (resolvable transcript ⇒ keep, else clean) so a same-worker
        // re-claim can resume — without this the finally computes preserveResumeArtifacts=false and
        // removes runHome on this UNKNOWN outcome. The batcher must still close so this execution
        // can drain.
        runLog.info("forge park reconcile interrupted by worker shutdown; leaving the run for requeue", {
          run_id: flight.runId,
        });
        await this.preserveForgeParkSessionIfResolvable(claim, flight, runLog, runHome);
        await batcher.close().catch(() => undefined);
        return "stop";
      }
      let probe;
      try {
        probe = await this.client.getRunOwnership(flight.runId);
      } catch (probeErr) {
        if (probeErr instanceof RequestError && probeErr.status === 404) {
          // Not owned / reclaimed = stale_claim. Stop; do NOT clean a possibly-parked session.
          runLog.info("forge park reconcile: ownership 404 (stale_claim); stopping silently", {
            run_id: flight.runId,
          });
          await batcher.close().catch(() => undefined);
          return "stop";
        }
        runLog.warn("forge park reconcile: ownership probe failed transport; retrying", {
          run_id: flight.runId,
          error: errMessage(probeErr),
        });
        // Do not busy-loop on a sticky cancel (mirrors the recovery loop): waitRecoveryRetry with
        // its default cancelStopsWait=true returns IMMEDIATELY on a stuck cancel, degenerating a
        // simultaneous cancel + api-unreachable into a backoff-free probe storm. Honor the backoff.
        await this.waitRecoveryRetry(flight, false);
        continue;
      }
      const status = probe.status;
      if (status === "recovery_wait") {
        await this.finishForgePark(probe.recovery_retry_not_before, claim, flight, runLog, runHome);
        return "parked";
      }
      if (TERMINAL_RUN_STATUSES.has(status)) {
        runLog.info("forge park reconcile: run is terminal; cleaning up without a park", {
          run_id: flight.runId,
          status,
        });
        await batcher.close().catch(() => undefined);
        return "stop";
      }
      if (status === "running") {
        // The transaction did not land; resend the SAME report (idempotent) and dispatch its ack.
        try {
          const ack = await reportState(body);
          return await this.dispatchForgeParkAck(ack, claim, flight, runLog, runHome);
        } catch (reportErr) {
          if (reportErr instanceof RequestError && reportErr.status === 400) {
            runLog.warn("forge park reconcile: resend rejected 400; taking the failed path (no park)", {
              run_id: flight.runId,
            });
            return "fail";
          }
          runLog.warn("forge park reconcile: resend failed transport; re-probing", {
            run_id: flight.runId,
            error: errMessage(reportErr),
          });
          // Honor the backoff even under a sticky cancel (see the probe-retry site above).
          await this.waitRecoveryRetry(flight, false);
          continue;
        }
      }
      // Any other status (queued, awaiting_*, …): the outcome is not yet known, retain and retry.
      // cancelStopsWait=false so a sticky cancel cannot turn this into a backoff-free retry storm.
      await this.waitRecoveryRetry(flight, false);
    }
  }

  private async phasePublish(
    claim: ClaimResponse,
    flight: RunFlight,
    boundarySignal?: AbortSignal,
    deferCommittedTerminal?: (report: () => Promise<void>) => void,
  ): Promise<void> {
    const { runLog, batcher, redactText, executor, runHome } = flight;
    // PRD #1296 M3 — the source pinned at the finalization boundary (set below, after the
    // fetch-back). Undefined until then and for non-code-publishing / early-return kinds, so
    // the terminal drive is a no-op for report-only / not_code / pause / empty-diff exits.
    let recoveryRecord: RecoveryRecord | undefined;
    // Drive the durable-recovery terminal action off the reported status, once H is pinned:
    // a `completed` full publication releases custody; a `failed` finalization captures +
    // uploads the verified bundle (model-free). Best-effort — it MUST NOT disturb the run's
    // honest terminal reporting (the source is protected by the pin + server hold regardless),
    // so every error is swallowed. Runs AFTER the state report lands.
    const driveRecoveryTerminal = async (
      body: Parameters<RunFlight["reportState"]>[0],
    ): Promise<void> => {
      const status = (body as { status?: string }).status;
      try {
        if (status === "completed") {
          // A completed run published its head (a full publication) OR committed nothing (a
          // no-code completion: report_only / not_code / scope-capped-empty). EITHER way this
          // generation's custody is settled by RELEASING its exact hold. recoveryRecord is set
          // only for a code-publishing run that reached the finalization pin; a no-code completion
          // returns BEFORE that pin, but recordRecoveryGenerationEvidence ALREADY pinned an early
          // generation-evidence record after clone, so releasing here is what settles it —
          // otherwise the leaked `pinned` record survives and a restart's resumePending sweep
          // escalates it to a FALSE needs_action. Guard on the code-publishing kind so a non-code
          // kind (chat/judge), which never pins, stays a no-op; release is best-effort/idempotent
          // and a disabled coordinator no-ops it.
          if (!isCodePublishingKind(resolveRunKind(claim.kind))) return;
          // PRD #1392 M1/M2 (fact 9): stamp the release's evidence class. A full publication
          // (a pushed branch + / or MR) → "publication". A NO-code completion on this SAME
          // branch (report_only / not_code / scope-capped-empty) carries no `branch` and is NOT a
          // publication — its release class is genuinely ambiguous (neither publication nor the
          // fresh-forge no-output proof), so OMIT the evidence (the api stores NULL, which is
          // allowed) rather than mis-stamp it.
          const completedBody = body as StateRequest;
          const releaseEvidence =
            typeof completedBody.branch === "string" && completedBody.branch !== ""
              ? "publication"
              : undefined;
          await this.recovery.release(claim.run_id, claim.claim_generation, releaseEvidence);
        } else if (status === "failed" || status === "cancelled") {
          const capBarePath = flight.barePath;
          if (!capBarePath) return;
          if (recoveryRecord) {
            // Finalization-failure capture: the ORIGINAL committed head H is already pinned.
            const capDefaultBranch =
              claim.repo.default_branch?.trim() ||
              (await this.git.defaultBranchName(capBarePath)) ||
              "main";
            const outcome = await this.recovery.captureAndUpload({
              record: recoveryRecord,
              barePath: capBarePath,
              defaultBranch: capDefaultBranch,
              forgePat: claim.secrets.forge_pat,
              cloneUrl: claim.repo.clone_url,
              forgeUsername: claim.secrets.forge_username,
              signal: boundarySignal,
            });
            runLog.info("recovery: finalization-failure capture outcome", {
              run_id: claim.run_id,
              capture_id: outcome.captureId,
              state: outcome.state,
              reason: outcome.reason,
            });
          } else {
            // PRD #1349 M2 (D4.5): an EARLY failed/cancelled exit that never reached the
            // finalization pin (report_only after a published checkpoint, an undeclared
            // empty-diff fail, ...). Run the SAME exact-generation disposition against the
            // restore-point head BEFORE the early terminal cleanup: a provably empty hold
            // releases, committed work is archived, and a failed/unverifiable forge comparison
            // retains — instead of returning and leaking the hold.
            await this.settleRecoveryGeneration(claim, flight, runLog, boundarySignal);
          }
        }
      } catch (err) {
        runLog.warn("recovery: terminal drive failed (execution reporting is unaffected)", {
          run_id: claim.run_id,
          error: errMessage(err),
        });
      }
    };
    const reportState = async (body: Parameters<RunFlight["reportState"]>[0]) => {
      const res = await flight.reportState(body, boundarySignal);
      await driveRecoveryTerminal(body);
      return res;
    };
    const closeBatcher = () => batcher.close(boundarySignal);
    // PRD #1391 Run B M3b: journal a run-lane TERMINAL report WRITE-AHEAD then resolve it over the
    // `reportState` choke point (which stamps claim_generation and drives recovery). The caller must
    // have closed the batcher first, so the fence is the durable emitted tail. Every terminal site in
    // this phase routes through here instead of a raw `reportState(body)`.
    // issue #1582 M2: a completed report with a pushed branch first persists the successor's pushed
    // head on this run's adopted settlement records as `pushed` (write-ahead of the report itself;
    // never sent). The reportState choke point promotes it to `pending_settle` when it observes the
    // completion ACK, and the settle itself runs only AFTER the terminal outcome resolved (the
    // outbox journal retired / terminalResolved latched), off the terminal send path.
    const journalTerminalReport = async (body: Parameters<RunFlight["reportState"]>[0]) => {
      await this.persistSettlementPushedHead(claim, flight, body);
      await this.journalAndSendTerminal(flight, TERMINAL_JOURNAL_PHASE, body, reportState);
      await this.settleAfterCompletion(claim, flight, body);
    };
    const finishCommittedPublish = async (
      body: Parameters<RunFlight["reportState"]>[0],
      logMessage: string,
      fields: Record<string, unknown>,
    ): Promise<void> => {
      if (deferCommittedTerminal) {
        deferCommittedTerminal(async () => {
          await batcher.close();
          // Journal write-ahead here too (D3): the deferred Codex sink sends through
          // flight.reportState + driveRecoveryTerminal, so wrap that pair as the resolve `send`.
          await this.persistSettlementPushedHead(claim, flight, body);
          await this.journalAndSendTerminal(flight, TERMINAL_JOURNAL_PHASE, body, async (b) => {
            const res = await flight.reportState(b);
            await driveRecoveryTerminal(b);
            return res;
          });
          await this.settleAfterCompletion(claim, flight, body);
          runLog.info(logMessage, fields);
        });
        return;
      }
      await closeBatcher();
      await journalTerminalReport(body);
      runLog.info(logMessage, fields);
    };
    const runId = claim.run_id;
    const result = flight.result!;
    // PRD #1190 M2: an owner-requested pause PARKED the run mid-loop (handlePausePark reported
    // `paused` and set flight.parked). The run is non-terminal and already reported — there is
    // nothing to finalize (no push, no MR, no completion report), and the finally preserves its
    // HOME for resume exactly as a limit park does. Close the batcher (handlePausePark only
    // flushed it) and return. Keyed on the result the executor returned so a non-pause path can
    // never reach this branch.
    if (result.pausedAt) {
      // PRD #1171 m4: needs NO own permit — for a Codex run this whole phasePublish already
      // runs INSIDE the finalize withBoundary (executeClaim wraps the call), and this line is a
      // no-op for Codex (its executor implements no killAgentTree and never sets pausedAt); for
      // Claude it is the literal legacy reap, byte-unchanged.
      executor.killAgentTree?.();
      await closeBatcher().catch(() => undefined);
      runLog.info("run parked on an owner-requested pause; skipping finalization", {
        run_id: runId,
      });
      return;
    }
    // PRD #1226 M3 (D3/D6): the run entered the recoverable COMPLETION HOLD — a repeated
    // no-progress completion attempt, or a post-attempt budget/stall/wall/idle exhaustion, routed
    // to ctx.enterCompletionHold (M4) instead of failing. Like the pause park, the run is
    // non-terminal and the hold seam already handled the transition, so there is nothing to
    // finalize (no push, no MR, no completion report). Reap + close the batcher and return; the
    // finally preserves its HOME for resume. In M3 this branch is dead (the seam is unwired, so
    // completionHeld is never set) — M4 wires the seam and this becomes live.
    if (result.completionHeld) {
      executor.killAgentTree?.();
      await closeBatcher().catch(() => undefined);
      runLog.info("run entered the completion hold; skipping finalization", {
        run_id: runId,
        reason: result.completionHeld.reason,
      });
      return;
    }
    // PRD #1497 M2: the run PARKED at its WALL-CLOCK limit (ctx.parkForWall → "parked", or
    // "undeliverable" so the flight ends non-terminal keeping the work, D17). Like the pause park and
    // the completion hold above, the run is non-terminal and the wall seam already handled it (a
    // `paused` report on "parked", NOTHING on "undeliverable"), so there is nothing to finalize (no
    // push, no MR, no terminal report). Reap + close the batcher and return; the finally preserves
    // clone + HOME (enterWallPark set the flags) for a resume. Keyed on the executor's result so no
    // non-wall path can reach this branch.
    if (result.walled) {
      executor.killAgentTree?.();
      await closeBatcher().catch(() => undefined);
      runLog.info("run parked at its wall-clock limit; skipping finalization", {
        run_id: runId,
        reason: result.walled.reason,
      });
      return;
    }
    // PRD #1247 M5b (data-integrity fix): a held-state credential switch RELEASED the claim IN PLACE
    // (ctx.attemptCredentialSwitch → "released"). enterCredentialSwitch already drained the batcher,
    // reported credential_switch (queued ack), cleared preserveRecoveryClone + set parked, and the
    // server requeued the run for a reclaim at resume_phase on the newly-chosen token. Like the pause
    // park and the completion hold above, the run is non-terminal and already handled, so there is
    // NOTHING to finalize (no push, no MR, no completion report). Reap + close the batcher
    // (idempotent — the release already drained it) and return; the finally retires the clone
    // (preserveRecoveryClone cleared) and preserves the HOME (parked) for the same-worker resume.
    // Keyed on the executor's result so no non-release path can reach this branch.
    if (result.switchReleased) {
      executor.killAgentTree?.();
      await closeBatcher().catch(() => undefined);
      runLog.info("run released for a credential switch in place; skipping finalization", {
        run_id: runId,
      });
      return;
    }
    const runnerClone = flight.runnerClone!;
    const barePath = flight.barePath!;
    const lastPublishedTip = flight.lastPublishedTip;
    const ciFixHumanApproved = flight.ciFixHumanApproved;
    // A ci_fix run that judged the failure not a code problem (PRD #6) completes
    // with the diagnosis and NO push/MR — there is nothing to land.
    if (result.fixVerdict === "not_code") {
      // PRD #1171 m4: needs NO own permit — for a Codex run this runs INSIDE the finalize
      // withBoundary that wraps phasePublish, and this line is a no-op for Codex; for Claude it
      // is the literal legacy reap, byte-unchanged.
      executor.killAgentTree?.();
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "not a code problem: completing with the diagnosis, no merge request",
        },
      });
      await closeBatcher();
      // PRD #1391 Run B M3 (N1): journal write-ahead so an outage keeps the exact not_code
      // completion, not a generic agent_failure.
      await journalTerminalReport({ status: "completed", fix_verdict: "not_code" });
      runLog.info("ci_fix run completed with not_code verdict", {
        run_id: runId,
      });
      return;
    }

    // issue #279: a DECLARED report-only run — the lead's deliverable is a report,
    // command output, or verification result with NO code change to land, so the run
    // completes with its findings and NO push/MR (mirroring the ci_fix not_code path
    // above). killAgentTree was already called at the security boundary above, so we do
    // NOT double-call it here (the not_code block's second call is a redundancy — not
    // copied). Returns before fetchAgentBranch: there is no branch to fetch or push.
    if (result.reportOnly) {
      // issue #299: a report-only completion opens NO branch and NO MR, so if this run
      // ALREADY published committed work to a checkpoint ref on origin
      // (refs/uzi-checkpoints/<branch>), completing report-only would leave that ref
      // orphaned — un-landed, with nothing to supersede it. ADR-0279 documented this as
      // an accepted edge resting on the convention "a genuine zero-code run never
      // checkpoints"; enforce that convention here instead. Detection is the UNION of
      // two signals, each covering a gap the other has:
      //   - lastPublishedTip: a checkpoint THIS worker confirmed-landed mid-run (set only
      //     on a landed publish), which may not yet be mirrored into the bare's local ref.
      //   - hasCommittedCheckpoint: origin's checkpoint ref, mirrored into the bare at
      //     clone/fetch time — catches a checkpoint a PRIOR/cross-worker attempt landed.
      //     Per PRD #759 it IGNORES a marker-only `wip(park):` checkpoint (an abandoned
      //     usage-limit-park WIP marker with no committed milestone below it), while a
      //     real committed milestone still blocks.
      // A genuine zero-code run trips NEITHER (nothing committed ⇒ no pack ⇒ no publish),
      // so it still completes report-only below. Refuse loudly, mirroring the
      // undeclared-empty-diff FAIL path, rather than opening a delete-ref capability.
      const publishedCheckpoint =
        lastPublishedTip !== undefined ||
        (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
      if (publishedCheckpoint) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "report_only was set but this run published a checkpoint to origin; failing to avoid orphaning it",
          },
        });
        await closeBatcher();
        // PRD #1391 Run B M3 (N1): journal write-ahead so the typed orphan-checkpoint failure
        // survives an outage as this exact outcome, not a generic agent_failure.
        await journalTerminalReport({
          status: "failed",
          failure_reason:
            "signal_done was called with report_only, but this run published committed work to a checkpoint ref (refs/uzi-checkpoints/" +
            runnerClone.branch +
            ") on origin. A report-only completion opens no branch or merge request and would orphan that checkpoint. If this run has code to land, call signal_done WITHOUT report_only so the work lands as a merge request; report_only is only valid for a run that committed nothing.",
        });
        runLog.info("run failed: report_only declared after a checkpoint was published", {
          run_id: runId,
        });
        return;
      }
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "report-only run: recording findings; no branch pushed and no merge request opened",
        },
      });
      await closeBatcher();
      // PRD #1391 Run B M3 (N1): journal write-ahead so the report_only completion (report_md) is
      // the durable outcome after an outage, not a generic agent_failure.
      await journalTerminalReport({
        status: "completed",
        report_only: true,
        report_md: result.summary,
      });
      runLog.info("run completed report-only (no MR)", { run_id: runId });
      return;
    }

    // (b) fetch-back (PRD #51 M3): the agent committed in the RUNNER clone, so the
    // worker now fetches the agent branch BACK into its own bare (single-branch
    // refspec, file://+pack transport, protocol.file.allow pinned — the six B2
    // invariants live in git.fetchAgentBranch) before it inspects or pushes. Done
    // AFTER killAgentTree so no agent process is concurrently mutating the clone,
    // and it brings the agent's objects into the worker bare so the push does not
    // depend on the (soon torn-down) runner clone. `trackingRef` is what push +
    // changedFiles read; the runner clone is never a git source for either.
    const trackingRef = await this.git.fetchAgentBranch(
      barePath,
      runnerClone.path,
      result.branch,
      runId,
    );

    // PRD #1296 M3 (D1) — the protected finalization boundary. Pin the ORIGINAL committed
    // head H into the authenticated durable journal FIRST, unconditionally, and BEFORE any
    // workflow overlay / merge / rebase / base-align that could replace the ordinary
    // tracking ref with a transformed H'. H is the pre-align agent tip read from the RUNNER
    // clone (the same source `originalAgentTip` uses further down) — NEVER the post-align
    // tracking ref, `checkpoint_tip`, or the private origin/<default>. Only the six
    // code-publishing kinds open a custody hold (D9), so only they pin here. Local and
    // credential-free: this never waits on the server and never uses a forge PAT. The
    // capture bundle + upload happens later, on a finalization failure, via the terminal
    // drive; a successful publication releases the hold.
    if (isCodePublishingKind(resolveRunKind(claim.kind))) {
      const originalH = await this.git.branchTip(runnerClone.path, result.branch);
      if (originalH) {
        recoveryRecord = await this.recovery.pin({
          runId,
          sourceSha: originalH,
          kind: resolveRunKind(claim.kind),
          branch: result.branch,
          // PRD #1349 M2 (D1): thread the EXACT claim generation so a finalization-failure
          // capture and a completion release target the ONE hold this generation owns. The
          // early generation-evidence pin (recordRecoveryGenerationEvidence) already created
          // this generation's record after clone; matching by generation UPDATES it to the
          // committed head H here rather than minting a duplicate.
          generation: claim.claim_generation,
        });
      } else {
        runLog.warn("recovery: could not resolve the original committed head to pin", {
          run_id: runId,
        });
      }
    }

    // issue #279: an UNDECLARED zero-diff guard, ISSUE runs only. A declared report_only
    // already returned above, so reaching here on an issue run with a confirmed-empty diff
    // is the ambiguous "forgot to commit / should have set report_only" case — a
    // committed-nothing issue run must not open an empty MR.
    if (resolveRunKind(claim.kind) === "issue") {
      const changedForGuard = await this.git.changedFiles(barePath, trackingRef);
      // changedFiles returns null on diff-FAILURE (keep pushing — fail open) and [] on a
      // CONFIRMED-empty diff. report_only is the sanctioned zero-diff success — a declared
      // report_only already returned above, so reaching here undeclared+empty is the
      // ambiguous case; fail with an actionable reason instead of opening an empty MR.
      if (changedForGuard !== null && changedForGuard.length === 0) {
        // PRD #634 M3: an operator scope directive can truncate a run at its very first
        // milestone boundary, before any milestone produced committed work — a legitimate
        // zero-slice, NOT the ambiguous "forgot to commit" failure below. But guard against
        // orphaning: if this run DID publish a checkpoint to origin (a mid-run milestone
        // landed there), there IS committed work to land, so fall through to the push+MR
        // path. Only the genuinely-empty case completes report-only with no MR. Detection
        // reuses the SAME publishedCheckpoint union the declared-report_only path uses,
        // which (PRD #759) ignores a marker-only `wip(park):` checkpoint while a real
        // committed milestone still blocks.
        if (result.scopeCapped) {
          const publishedCheckpoint =
            lastPublishedTip !== undefined ||
            (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
          if (!publishedCheckpoint) {
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "operator scope directive stopped this run before any milestone produced committed work; recording it, no merge request",
              },
            });
            await closeBatcher();
            // PRD #1391 Run B M3 (N1): journal the scope-capped report_only completion write-ahead
            // so its exact outcome survives an outage, not a generic agent_failure.
            await journalTerminalReport({
              status: "completed",
              report_only: true,
              scope_capped: true,
              report_md:
                result.summary ??
                "Stopped by operator scope directive before any committed work; nothing to land.",
            });
            runLog.info("run completed scope-capped with no committed work (no MR)", {
              run_id: runId,
            });
            return;
          }
          // published checkpoint exists → there IS work to land; fall through to push+MR.
        } else {
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: "no changes were committed and report_only was not set; failing",
            },
          });
          await closeBatcher();
          // PRD #1391 Run B M3 (N1): journal write-ahead so the undeclared empty-diff failure is the
          // durable outcome after an outage, not a generic agent_failure.
          await journalTerminalReport({
            status: "failed",
            failure_reason:
              "signal_done was called but no changes were committed, and report_only was not set. If this run's deliverable is a report or command output with no code change, call signal_done with report_only: true.",
          });
          runLog.info("run failed: signal_done with empty diff and no report_only", {
            run_id: runId,
          });
          return;
        }
      }
    }

    // issue #341: a `prompt` run (schedule-fired ad-hoc work) that reaches signal_done
    // having committed NOTHING has no diff to land, so pushing it would open an empty
    // merge request — the #242 pathology, for the schedule path. Unlike the issue-kind
    // guard above, a zero-commit prompt run is not the ambiguous "forgot to commit"
    // failure: an ad-hoc prompt whose deliverable is an investigation/answer legitimately
    // produces no code, so it completes as report-only (no branch, no MR), mirroring the
    // declared report_only terminal above — INCLUDING its issue #299 checkpoint-orphan
    // guard — rather than failing. ADR-0279 §2 forbids broadening the issue-kind gate
    // onto other kinds precisely because they have their own terminal paths; this IS that
    // separate terminal for `prompt`. A prompt run that DID commit falls through to the
    // normal push/MR path unchanged.
    if (claim.kind === "prompt") {
      const changedForPrompt = await this.git.changedFiles(barePath, trackingRef);
      // Preserve the null-vs-[] split: changedFiles returns null on diff-FAILURE (keep
      // pushing — fail open) and [] on a CONFIRMED-empty diff. Only a confirmed-empty
      // diff completes report-only; a diff-failure must NOT be treated as empty.
      if (changedForPrompt !== null && changedForPrompt.length === 0) {
        // issue #299: a report-only completion opens NO branch and NO MR, so if this run
        // ALREADY published committed work to a checkpoint ref on origin
        // (refs/uzi-checkpoints/<branch>), completing report-only would orphan that ref.
        // This mirrors the declared report_only terminal above: detect via the UNION of
        // lastPublishedTip (a checkpoint THIS worker confirmed-landed mid-run) and
        // hasCommittedCheckpoint (origin's checkpoint ref, mirrored into the bare at
        // clone/fetch time — catches a prior/cross-worker landing; per PRD #759 it ignores
        // a marker-only `wip(park):` checkpoint while a real committed milestone still
        // blocks). A genuine zero-code prompt run trips NEITHER and still completes
        // report-only below.
        const publishedCheckpoint =
          lastPublishedTip !== undefined ||
          (await this.git.hasCommittedCheckpoint(barePath, runnerClone.branch));
        if (publishedCheckpoint) {
          batcher.emit({
            kind: "status",
            agent: "worker",
            payload: {
              text: "report_only was set but this run published a checkpoint to origin; failing to avoid orphaning it",
            },
          });
          await closeBatcher();
          // PRD #1391 Run B M3 (N1): journal write-ahead so the prompt-kind orphan-checkpoint failure
          // survives an outage as this exact outcome, not a generic agent_failure.
          await journalTerminalReport({
            status: "failed",
            failure_reason:
              "signal_done was called with report_only, but this run published committed work to a checkpoint ref (refs/uzi-checkpoints/" +
              runnerClone.branch +
              ") on origin. A report-only completion opens no branch or merge request and would orphan that checkpoint. If this run has code to land, call signal_done WITHOUT report_only so the work lands as a merge request; report_only is only valid for a run that committed nothing.",
          });
          runLog.info("prompt run failed: report_only after a checkpoint was published", {
            run_id: runId,
          });
          return;
        }
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "prompt run committed no changes: recording findings; no branch pushed and no merge request opened",
          },
        });
        await closeBatcher();
        // PRD #1391 Run B M3 (N1): journal write-ahead so the prompt report_only completion (its
        // report_md AND any structured proposal) is the durable outcome after an outage, not a
        // generic agent_failure that loses the proposal.
        await journalTerminalReport({
          status: "completed",
          report_only: true,
          report_md: result.summary,
          // PRD #929 M2: a scheduled prompt run's deliverable can be a structured proposal
          // (title + body) the agent conveyed on signal_done, which the server files as a
          // forge issue. Thread it through ONLY when the agent actually produced one — the
          // key is OMITTED otherwise (spread nothing) so a normal/mr-mode prompt run's
          // completion payload is byte-identical to before.
          ...(result.proposal !== undefined
            ? { proposal: result.proposal }
            : {}),
        });
        runLog.info("prompt run completed report-only (no MR): zero-diff", {
          run_id: runId,
          has_proposal: result.proposal !== undefined,
        });
        return;
      }
    }

    // Self-improvement MR evidence (PRD #46 Decision 10): with no CI on the uzi
    // repo, the worker itself runs the test suites and flags any guard-critical
    // path the change touched, folding both into the MR description. Best-effort —
    // gathered before the push so the MR opens with its evidence, and a suite that
    // can't run is reported "skipped", never failing the run.
    let selfImproveSection: string | undefined;
    // PRD #686 M4: uzi's SELF_IMPROVE_CHECKS (go test ./..., web/agent npm test,
    // web build) are hardcoded to uzi's OWN layout and are meaningless against an
    // arbitrary target repo, so this evidence block runs ONLY in dogfood mode. In
    // generic mode it is skipped: selfImproveSection stays unset and no uzi-shaped
    // check/guard-path evidence is produced — the generic plan directive already
    // told the agent to discover and run the target repo's own gates during the run.
    if (claim.kind === "self_improve" && claim.self_improve_dogfood) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "self-improvement: running the test suites for MR evidence",
        },
      });
      // changedFiles returns null when the diff could not be computed → pass null
      // through so the MR section fails CLOSED (a loud "guard-path check unavailable"
      // note) instead of silently suppressing the flag (M5 audit). Under (b) this is
      // a WORKER-BARE tree-diff of the fetched tracking ref (no runner-owned config
      // source read), while the checks below still run in the runner clone.
      const changed = await this.git.changedFiles(barePath, trackingRef);
      // M9: the checks execute agent-authored code as the worker uid, so they run
      // under a SCRUBBED replacement env (no join token / API URL / PAT / OAuth token
      // by construction) with the run's provisioned toolchains on PATH. The frozen
      // `--ignore-scripts` install runs best-effort so vitest/tsc exist; a failure
      // just leaves the check honestly skipped.
      //
      // PRD #121 M2: this used the hardcoded `["web", "agent"]` dir list; it now
      // reuses the same lockfile-driven installer the executor runs pre-plan, so
      // there is ONE install path instead of two that can drift. For the uzi repo
      // that discovery resolves to exactly web/ + agent/ (the only two tracked
      // package.json files, both with a package-lock.json), which is what
      // SELF_IMPROVE_CHECKS pre-flights on — asserted in js-deps.test.ts. The
      // executor has already installed these once; re-running is deliberate, because
      // the agent may have edited a package.json or lockfile during the run and the
      // checks must test what it actually left behind.
      const checkEnv = buildCheckEnv(
        process.env,
        runHome ?? os.tmpdir(),
        result.toolEnv,
      );
      const deps = await installJsDeps(runnerClone.path, checkEnv).catch(
        () => ({ results: [], truncated: false }),
      );
      for (const note of deps.results) {
        runLog.info("self-improve: dependency install", { ...note });
      }
      // A truncated scan means `results` is a PREFIX of the repo's project dirs, so a
      // check may be about to run somewhere provisioning never reached. Say so rather
      // than let the notes above read as full coverage.
      if (deps.truncated) {
        runLog.warn(
          "self-improve: dependency discovery hit its bound; some dirs were not installed",
          {
            installed_dirs: deps.results.length,
          },
        );
      }
      const checkRunner = this.checkRunner ?? defaultCheckRunner(checkEnv);
      const checks = await runSelfImproveChecks(
        runnerClone.path,
        checkRunner,
      );
      selfImproveSection = selfImproveMrSection(
        changed === null ? null : flagGuardPaths(changed),
        checks,
      );
    }

    // Guard-critical flag for an ad-hoc scheduled prompt run (PRD #241 Decision 10,
    // the one worker-side follow-up). self_improve gets this flag by KIND — it is
    // hardcoded API-side to uzi's own repo, a decision the worker CANNOT reproduce:
    // the claim carries no repo-identity field, so there is no clean worker-side
    // "is this the uzi repo" signal to gate on (flagged for the owner in the M8
    // report — a repo-identity flag on the claim would be the API-side alternative).
    // Instead we key the flag on the CHANGED PATHS: GUARD_CRITICAL_PATTERNS match
    // uzi's own security-critical source paths only, so flagGuardPaths fires exactly
    // when a prompt run actually touches uzi's guard surface (i.e. it targets the
    // uzi repo) and is an empty no-op on any other repo. We do NOT run
    // SELF_IMPROVE_CHECKS here — those are uzi's own gate suite and are meaningless
    // against an arbitrary repo. Best-effort, gathered before the push like the
    // self_improve evidence above.
    let promptGuardSection: string | undefined;
    if (claim.kind === "prompt") {
      // null (diff failed) → fail CLOSED with a loud "guard-path check unavailable"
      // note, exactly as the self_improve path does above (M5 audit).
      const changed = await this.git.changedFiles(barePath, trackingRef);
      promptGuardSection = guardCriticalMrSection(
        changed === null ? null : flagGuardPaths(changed),
      );
    }

    // PRD #71 M5 (load-bearing): for an auto-approved ci_fix run (no human in the loop),
    // REFUSE to push a diff that touches a protected CI-config path, or a diff that could
    // not be computed. A human-approved run (manual, or an auto CI-config plan that parked
    // and was approved) is never blocked — a human was in the loop, as in the manual flow.
    if (claim.kind === "ci_fix" && !ciFixHumanApproved) {
      // Capture the narrowed bare path in a const the SAME way the push block does
      // (barePath is an outer `let string | undefined` and TS drops the narrowing here).
      const pushBarePathForGuard = barePath;
      // Worker-side FLOOR: when the claim omits ci_config_paths (a bug or an older
      // server), fall back to the static defaults so the backstop cannot fail OPEN.
      // This floor covers the static defaults only; the server-produced set additionally
      // carries the project's real ci_config_path (see DEFAULT_CI_CONFIG_PATHS).
      const ciConfigPaths = claim.config?.ci_config_paths?.length
        ? claim.config.ci_config_paths
        : DEFAULT_CI_CONFIG_PATHS;
      const changed = await this.git.changedFiles(pushBarePathForGuard, trackingRef);
      const flagged = changed === null ? null : flagCIConfigPaths(changed, ciConfigPaths);
      if (changed === null || (flagged && flagged.length > 0)) {
        const reason =
          changed === null
            ? "auto CI-fix push refused: could not compute the diff to verify it does not edit CI config (failing closed)"
            : `auto CI-fix push refused: an auto-approved fix may not edit CI config (${flagged!.join(", ")}); a CI-config fix needs human approval`;
        batcher.emit({ kind: "status", agent: "worker", payload: { text: reason } });
        runLog.warn("ci-fix: CI-config push guard refused push", { run_id: runId, reason });
        // Fail the run CLOSED — no push, no MR. Throwing here lands on the method's
        // generic catch (the `else` at ~:1203), which reports status:"failed" with this
        // reason and does NOT re-queue (unlike LimitReached/shutdown). Same terminal
        // convention the push/MR failures use.
        throw new Error(reason);
      }
    }

    // PRD #377 M1: a GitHub run whose branch touches .github/workflows/** cannot be
    // pushed by the bot's repo-only PAT (privcheck forbids the workflow scope by design).
    // Detect it here, BEFORE the doomed push, and end the run in a typed `failed` outcome
    // that preserves the agent's diff for a human to land — instead of face-planting into
    // GitHub's opaque "without workflow scope" rejection and discarding the committed work.
    // Serves every forge-pushing kind (the failed path is not issue-gated).
    // The precheck uses branch-only commits; the overlay keeps its separate conservative
    // changedFiles guard below.
    let changedForWf: string[] | null = null;
    let freshDefaultTip: string | undefined;
    if (claim.repo.forge_type === "github") {
      // Capture the narrowed bare path in a const the SAME way the push block does
      // (barePath is an outer `let string | undefined` and TS drops the narrowing here).
      const wfBarePath = barePath;
      // Fetch before the precheck so commit classification sees the current default tip.
      try {
        const defaultBranch = claim.repo.default_branch?.trim() ||
          (await this.git.defaultBranchName(wfBarePath)) || "main";
        freshDefaultTip = await this.git.fetchDefaultTip(
          wfBarePath, defaultBranch, claim.secrets.forge_pat,
          claim.repo.clone_url, claim.secrets.forge_username,
        );
        changedForWf = await this.git.branchWorkflowFiles(wfBarePath, freshDefaultTip, trackingRef);
      } catch (e) {
        runLog.warn("workflow precheck: could not fetch default tip; pushing normally", {
          run_id: runId, error: errMessage(e),
        });
      }
      // D6: a null diff (diff-computation failure) fails OPEN to the normal push — do not
      // fail a possibly-legitimate non-workflow run on an inability to compute the diff.
      const wfHits = changedForWf === null
        ? null
        : changedForWf.filter((file) => file.startsWith(".github/workflows/"));
      if (wfHits && wfHits.length > 0) {
        // Compose an actionable, capped failure_reason that names the offending path(s)
        // (truncating the path LIST if needed, never the doc link) and points at
        // docs/github-bot-setup.md.
        const reason = composeWorkflowScopeReason(wfHits);
        // Preserve the agent's diff so a human can land it without re-deriving it from the
        // transcript. redactText scrubs the run's secrets before it reaches the api; a null
        // diff (best-effort failure) just omits the patch — the typed failure still lands.
        const rawPatch = await this.git.workflowScopeDiff(wfBarePath, trackingRef);
        const patch = rawPatch === null ? undefined : redactText(rawPatch);
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text:
              "branch changes .github/workflows, which the bot token cannot push; failing early and preserving the diff for a human to land",
          },
        });
        runLog.info(
          "run failed: branch touches .github/workflows which the bot PAT cannot push; preserving diff",
          { run_id: runId, paths: wfHits },
        );
        await closeBatcher();
        await journalTerminalReport({
          status: "failed",
          failure_reason: reason.slice(0, MAX_FAILURE_REASON_LEN),
          fail_origin: "workflow_scope_missing",
          preserved_patch: patch,
        });
        return;
      }
    }

    // PRD #974 M2 / #1077: the single terminal reporter for a push_secret_blocked failure.
    // PRD #1391 Run B M3b: journal it WRITE-AHEAD (D3) then resolve it. On the journaled path a send
    // that fails now leaves the durable journal (with the push_secret_blocked origin) for a later
    // resolve, so it never throws — the outcome is captured. Only the reserve_exhausted (or no-outbox)
    // degraded path sends directly and can throw on exhaustion; rethrow a typed sentinel THERE so
    // execute()'s generic catch preserves the push_secret_blocked origin instead of defaulting to
    // agent_failure. NEVER attach preserved_patch (the diff carries the detected secret).
    const reportPushSecretBlocked = async (reason: string): Promise<void> => {
      const capped = reason.slice(0, MAX_FAILURE_REASON_LEN);
      try {
        await journalTerminalReport({
          status: "failed",
          failure_reason: capped,
          fail_origin: "push_secret_blocked",
          preserved_patch: undefined,
        });
      } catch (e) {
        throw new TerminalReportError(capped, "push_secret_blocked", e);
      }
    };

    // PRD #1416 M4 (fact 15): whether the top-of-finalize secret scan below walked a TRUSTWORTHY
    // range. A divergent H (the branch that reaches history_rewritten) fails the scan floor OPEN
    // (secretScanRange returns trusted:false), so the pushable range is NOT secret-scanned. This
    // flag gates whether failHistoryRewritten may attach a preserved_patch: only a scan-trusted
    // range may be preserved, else an UNSCANNED secret could be written into a stored/displayed
    // diff. Defaults false, so a non-github forge (no scan at all) also omits the patch — the
    // conservative, safe default. Set only inside the github scan block below.
    let scanRangeTrusted = false;
    // PRD #974 M2 (load-bearing security): a GitHub run's committed range is scanned for secrets
    // with the pinned gitleaks (default ruleset, all three silencers GitHub Push Protection
    // ignores DISABLED — see git.secretScanRange) BEFORE the doomed push, mirroring the #377
    // workflow-scope guard above. On a TRUSTWORTHY scan with a real finding, fail the run early
    // and typed WITHOUT a preserved_patch (the diff may carry the detected secret; the committed
    // work stays recoverable from the run branch/PVC) instead of face-planting into GitHub's opaque
    // GH013 rejection and discarding the committed work. An UNTRUSTWORTHY scan (broken/empty) fails
    // OPEN: it does NOT block the run, and the GH013 remote backstop at the push below covers a
    // real secret. GitHub-only: GitLab/Forgejo have no equivalent push-side secret rejection.
    if (claim.repo.forge_type === "github") {
      const scanBarePath = barePath;
      const scan = await this.git.secretScanRange(scanBarePath, trackingRef, result.branch, {
        pat: claim.secrets.forge_pat,
        cloneUrl: claim.repo.clone_url,
        username: claim.secrets.forge_username,
      });
      scanRangeTrusted = scan.trusted;
      if (scan.trusted && scan.findings.length > 0) {
        const reason = composePushSecretBlockedReason(scan.findings);
        // Do NOT preserve the diff on a secret block. redactText only scrubs the run's OWN
        // secrets (forge PAT / Anthropic / join token / gitBasic); a gitleaks finding is by
        // definition a DIFFERENT secret whose value we do not even have here (the report is
        // `--redact`'d to file/line/rule only), so a preserved patch would persist the detected
        // secret into runs.preserved_patch and render it in RunView — the exact leak this feature
        // exists to prevent. The committed work stays recoverable from the worker branch/PVC, and
        // the failure_reason names each offending commit+path.
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "branch carries a secret GitHub Push Protection would reject; failing early — the diff is withheld because it may carry the secret",
          },
        });
        runLog.info(
          "run failed: branch carries a secret GitHub Push Protection would reject (GH013); withholding diff (it may carry the secret)",
          {
            run_id: runId,
            findings: scan.findings.map((f) => ({
              commit: f.commit,
              file: f.file,
              rule: f.ruleId,
            })),
          },
        );
        await closeBatcher();
        await reportPushSecretBlocked(reason);
        return;
      }
      if (!scan.trusted) {
        runLog.warn(
          "finalize secret scan: not trustworthy (broken/empty scan); pushing and relying on the GH013 remote backstop",
          { run_id: runId },
        );
      }
    }

    // The single authenticated finalize push (PAT-bearing, worker-owned; the agent never
    // has a credential). Captured once as a closure so the align path (below) and the
    // normal path push through EXACTLY ONE code path — the run must never push twice.
    // PRD #284 Layer A: a transient push failure (a dropped HTTP/2 stream, a 5xx, a
    // connection reset) retries rather than discarding the agent's already-committed work;
    // a permanent rejection (auth, protected branch, non-fast-forward) fails fast and
    // propagates to the catch below. The push is idempotent on retry (non-forced, same
    // commits → "Everything up-to-date"). Capture the narrowed bare path: barePath is an
    // outer `let` (string | undefined) and TS drops the narrowing inside the closure.
    const finalizeBarePath = barePath;
    const pushToOrigin = () =>
      withForgeRetry(
        () =>
          this.git.pushBranch(
            finalizeBarePath,
            result.branch,
            claim.secrets.forge_pat,
            claim.repo.clone_url,
            claim.secrets.forge_username,
          ),
        { log: runLog, signal: boundarySignal },
      );

    // PRD #1416 (MR-rework, finding 1): a bridge NEWLY makes a rewritten branch pushable (a plain
    // rewritten push is non-fast-forward-rejected on every forge). The top-of-finalize scan ran on
    // the divergent H with an unresolvable floor and failed OPEN; now that the tracking ref is B and
    // P is an ancestor of B, P..B is the real, trustworthy push delta. Re-scan it on EVERY forge
    // before pushing B — GitLab/Forgejo have no GH013 backstop, so this worker-side scan is their
    // only gate. On a TRUSTED finding: report push_secret_blocked (no preserved_patch), no push.
    // On untrusted/clean: fail open (unchanged; GH013 still backstops GitHub at the push catch).
    const scanBridgedRangeAndBlock = async (scanBare: string): Promise<"blocked" | "ok"> => {
      const scan = await this.git.secretScanRange(scanBare, trackingRef, result.branch, {
        pat: claim.secrets.forge_pat,
        cloneUrl: claim.repo.clone_url,
        username: claim.secrets.forge_username,
      });
      if (scan.trusted && scan.findings.length > 0) {
        // #1416 (MR-rework, finding 8) — forge_type is optional (an omitted value means GitLab, R8),
        // but composePushSecretBlockedReason defaults undefined → github (relied on by the
        // GitHub-gated top-of-finalize caller). Normalize HERE so an omitted-forge_type GitLab run
        // gets forge-neutral wording, not GH013/"GitHub Push Protection".
        const forgeType =
          claim.repo.forge_type === "forgejo"
            ? "forgejo"
            : claim.repo.forge_type === "github"
              ? "github"
              : "gitlab"; // R8: an omitted forge_type is GitLab, which has no GH013 backstop
        const reason = composePushSecretBlockedReason(scan.findings, forgeType);
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "the bridged branch carries a secret the pre-push scan detected; failing without pushing — the diff is withheld because it may carry the secret",
          },
        });
        runLog.info(
          "run failed: post-bridge secret scan found a secret in the P..B push delta; withholding diff",
          {
            run_id: runId,
            findings: scan.findings.map((f) => ({ commit: f.commit, file: f.file, rule: f.ruleId })),
          },
        );
        await closeBatcher();
        await reportPushSecretBlocked(reason);
        return "blocked";
      }
      if (!scan.trusted) {
        runLog.warn(
          "post-bridge secret scan: not trustworthy (broken/empty); pushing and relying on the GH013 remote backstop where it exists",
          { run_id: runId },
        );
      }
      return "ok";
    };

    // PRD #974 M2 — the GH013 remote backstop. When a finalize push (the normal path OR an
    // align-path push) is rejected by GitHub Push Protection for a secret the pre-push gitleaks
    // scan missed (GitHub's pattern set is broader than gitleaks', and the two are not
    // identical), route it to the SAME typed push_secret_blocked failure instead of losing the
    // typing to the generic catch (raw message, defaulted fail_origin).
    // Do NOT preserve the diff: the push carries a secret, and at the backstop there are no
    // finding spans at all, so a preserved patch would persist the secret into
    // runs.preserved_patch / RunView — the exact leak this feature prevents. The committed work
    // stays recoverable from the run's branch/PVC; the fixed capped reason points a human there
    // (the backstop has no gitleaks findings to name, so it cannot use composePushSecretBlockedReason).
    const failPushSecretBlocked = async () => {
      const reason =
        "the push was rejected by GitHub Push Protection (GH013): it carries a secret. " +
        "The committed work is on the run's branch — scrub the secret from its history and re-push.";
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "push rejected by GitHub Push Protection (GH013); failing (work is on the run branch — scrub the secret and re-push)",
        },
      });
      runLog.info("run failed: push rejected by GitHub Push Protection (GH013)", {
        run_id: runId,
      });
      await closeBatcher();
      await reportPushSecretBlocked(reason);
    };

    // issue #1117 — the mr_rework concurrent-writer disposition. An mr_rework run pushes its
    // rework to the pre-existing MR branch `agent/issue-*` via a non-forced finalize push;
    // when a concurrent same-branch writer (a human, or uzi-watcher landing review fixes)
    // advanced that branch under the run, the push is rejected non-fast-forward. Report
    // `failed` + `branch_moved:true` through the plain reportState wrapper (so the terminal
    // report is dispositioned for custody/recovery, like failPushSecretBlocked /
    // failBaseAlignConflict) instead of letting the non-ff rethrow into the generic
    // agent_failure catch; the server routes it to a non-error cancelled/branch_moved
    // disposition. NO preserved_patch: the branch and its concurrent commits are intact, so
    // there is nothing to hand back — the rework was simply superseded. The reason is a fixed
    // static string (no model output), so no slice/scrub is needed.
    const failBranchMoved = async () => {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "the MR branch was advanced by a concurrent writer; this rework was superseded and not applied (the branch and its commits are intact)",
        },
      });
      runLog.info(
        "run superseded: mr_rework finalize push rejected non-fast-forward because the MR branch was concurrently advanced; reporting branch_moved",
        { run_id: runId },
      );
      await closeBatcher();
      await journalTerminalReport({
        status: "failed",
        failure_reason:
          "The MR branch was advanced by a concurrent writer, so this rework was superseded and not applied. The branch and the concurrent commits are intact.",
        branch_moved: true,
      });
    };

    // PRD #1416 M4: the typed terminal for a run whose branch was rewritten at/below the published
    // floor P AND could not be bridged to a fast-forward (bridgeBareTrackingRefIfDivergent returned
    // {kind:"failed"}, which the finalize sinks throw as HistoryRewrittenError). Mirrors
    // failBaseAlignConflict / failPushSecretBlocked / failBranchMoved: a worker status line, close
    // the batcher, and report `failed` through the reportState WRAPPER (so driveRecoveryTerminal
    // captures + uploads the verified bundle — the #1296 durable-recovery path, per the PRD).
    // fail_origin=history_rewritten, NEVER the generic catch (agent_failure) and NEVER
    // finalize_base_align_conflict. push_secret_blocked keeps precedence: a bridge FAILURE throws
    // BEFORE any push, so a GH013 rejection (which happens only AFTER a successful bridge+push) can
    // never collide with this path.
    //
    // preserved_patch is SECURITY-GATED (fact 15). The divergent H that reaches here failed the
    // top-of-finalize secret scan floor OPEN (scanRangeTrusted=false), so the pushable range was NOT
    // secret-scanned; attaching its diff could persist an UNSCANNED secret into runs.preserved_patch
    // / RunView. So the (redacted) diff is included ONLY when a trustworthy scanned range was
    // available, and OMITTED otherwise — which is the divergent case in practice. The committed work
    // is durably recoverable via the #1296 capture regardless; the preserved_patch is a convenience.
    const failHistoryRewritten = async (publishedTip: string) => {
      let patch: string | undefined;
      if (scanRangeTrusted) {
        // Reuse the same diff helper failBaseAlignConflict uses, redacting the run's own secrets.
        const rawPatch = await this.git.workflowScopeDiff(finalizeBarePath, trackingRef);
        patch = rawPatch === null ? undefined : redactText(rawPatch);
      }
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "the branch's history was rewritten below its published tip and could not be bridged to a fast-forward; failing (uzi never force-pushes) — the committed work is on the run branch and recoverable",
        },
      });
      runLog.info(
        "run failed: history rewritten at or below the published tip; a fast-forward bridge could not be built or validated",
        { run_id: runId, published_tip: publishedTip, preserved_patch: patch !== undefined },
      );
      await closeBatcher();
      await journalTerminalReport({
        status: "failed",
        failure_reason: composeHistoryRewrittenReason(publishedTip).slice(0, MAX_FAILURE_REASON_LEN),
        fail_origin: "history_rewritten",
        preserved_patch: patch,
      });
    };

    // PRD #456 M1: a GitHub run can be merely BEHIND the default branch on
    // .github/workflows/** (main advanced those files after this run's clone base) WITHOUT
    // having touched them. The bot's repo-only PAT push is then rejected atomically —
    // losing ALL the run's work — even though #377's guard above (which fires only when the
    // BRANCH modifies a workflow) did not trip. Align the branch's workflow tree with the
    // FRESH default before pushing: merge first (SHA-preserving, D2), and if the merged push
    // is STILL workflow-scope-rejected fall back to a rebase (the empirically proven #422
    // recovery). On an unresolvable conflict, fail the run typed + preserve the diff (M2)
    // rather than face-plant into GitHub's opaque rejection and discard the committed work.
    // GitHub-only: GitLab/Forgejo impose no workflow-scope rule.
    let alignPushed = false;
    // PRD #1416 M4 (SC3): wrap the whole align chain so a HistoryRewrittenError thrown at its
    // bridge site (git.ts bridgeToFloors → {kind:"failed"} → thrown inside fetchAndPush and
    // rethrown by the arms' push catches, which classify only push-protection/workflow-scope/
    // non-ff) is TYPED history_rewritten here rather than escaping to the generic catch. Declared
    // OUTSIDE this try so the plain-push block below still sees `alignPushed`.
    try {
      if (claim.repo.forge_type === "github") {
        const alignBarePath = barePath;
        const alignDefaultBranch =
          claim.repo.default_branch?.trim() ||
          (await this.git.defaultBranchName(alignBarePath)) ||
          "main";
        // Re-fetch immediately before alignment: default may advance after the precheck.
        // A failed fetch leaves the align target unknown, so try the normal push.
        let defaultTip: string | undefined;
        try {
          defaultTip = await this.git.fetchDefaultTip(
            alignBarePath, alignDefaultBranch, claim.secrets.forge_pat,
            claim.repo.clone_url, claim.secrets.forge_username,
          );
        } catch (e) {
          runLog.warn("finalize base-align: could not refresh default tip; pushing without aligning", {
            run_id: runId, error: errMessage(e),
          });
        }
        let differs = false;
        try {
          if (defaultTip) differs = await this.git.workflowTreeDiffers(
            alignBarePath,
            trackingRef,
            defaultTip,
          );
        } catch (e) {
          runLog.warn(
            "finalize base-align: could not compute the align target; pushing without aligning",
            { run_id: runId, error: errMessage(e) },
          );
        }
        if (defaultTip && differs) {
          // The pre-align committed agent tip — the base every align strategy starts from, so
          // a rebase FALLBACK after a clean merge replays the ORIGINAL commits, not the merge.
          const originalAgentTip = await this.git.branchTip(
            runnerClone.path,
            result.branch,
          );
          if (!originalAgentTip) {
            runLog.warn(
              "finalize base-align: could not resolve the branch tip; pushing without aligning",
              { run_id: runId },
            );
          } else {
            // The conflict-failure path (M2). The abort already ran inside
            // alignBranchWithDefault; here we preserve the diff via #377's preserved_patch and
            // fail typed. We diff the pre-align agent tip (`originalAgentTip`, declared at :1848,
            // non-null under the :1852 guard) — NOT `trackingRef` — so the preserved patch is
            // exactly the agent's human-landable work. Issue #631: when a strategy ALIGNED and
            // then had its push rejected (the overlay's arm (c), or a merge/rebase whose push was
            // rejected) `fetchAndPush` re-fetched the ALIGNED tip into `trackingRef`, so diffing
            // `trackingRef` yielded a SUPERSET (agent work PLUS the aligning strategy's own
            // workflow-subtree/merge changes) if a LATER strategy then conflicted and landed here.
            // Diffing `originalAgentTip` eliminates that superset: its objects were fetched into
            // the worker bare by the finalize `fetchAgentBranch` before any align, so
            // workflowScopeDiff resolves it. In the non-push conflict paths originalAgentTip ==
            // trackingRef, so those are unchanged; in the clobber-safety path (a branch that
            // edited a workflow) originalAgentTip carries that edit, so it is still preserved.
            const defTip = defaultTip;
            const failBaseAlignConflict = async () => {
              const rawPatch = await this.git.workflowScopeDiff(alignBarePath, originalAgentTip);
              const patch = rawPatch === null ? undefined : redactText(rawPatch);
              batcher.emit({
                kind: "status",
                agent: "worker",
                payload: {
                  text: "could not realign the branch with the updated default branch and safely push it (merge and rebase conflicted, or the aligned branch could not be fast-forwarded); failing and preserving the diff for a human to land",
                },
              });
              runLog.info("run failed: finalize base-align conflict; preserving diff", {
                run_id: runId,
              });
              await closeBatcher();
              // PRD #1391 Run B M3 (N1): journal write-ahead so the typed base-align-conflict failure
              // — its fail_origin AND its preserved_patch (the canonicaliser handles the diff size) —
              // survives an outage as this exact outcome, not a generic agent_failure that loses the
              // diff a human needs to land.
              await journalTerminalReport({
                status: "failed",
                failure_reason: composeBaseAlignConflictReason(alignDefaultBranch),
                fail_origin: "finalize_base_align_conflict",
                preserved_patch: patch,
              });
            };

            // Run one align STRATEGY, treating an UNEXPECTED throw (the S3 count-mismatch
            // guard, or any git error) exactly like a `"conflict"` return. This is the whole
            // point of the feature: the agent's work must be PRESERVED on failure, so an
            // unexpected align error must route to failBaseAlignConflict (typed fail + diff),
            // NOT escape to the generic catch below (raw message, no preserved_patch, defaulted
            // fail_origin). Scoped to the align OPERATION only — the push keeps its own
            // handling (workflow-scope → rebase fallback; any other push error rethrows).
            const alignOp = async (strategy: "merge" | "rebase"): Promise<"aligned" | "conflict"> => {
              try {
                return await this.git.alignBranchWithDefault(
                  runnerClone.path,
                  result.branch,
                  originalAgentTip,
                  defTip,
                  strategy,
                );
              } catch (e) {
                runLog.warn(
                  "finalize base-align: unexpected error during align; preserving diff and failing typed",
                  { run_id: runId, strategy, error: errMessage(e) },
                );
                return "conflict";
              }
            };

            // Re-fetch the aligned tip into the worker bare's tracking ref, then push once.
            const fetchAndPush = async () => {
              await this.git.fetchAgentBranch(
                alignBarePath,
                runnerClone.path,
                result.branch,
                runId,
              );
              // PRD #1416 M3 (C4): the align chain re-fetched the CLONE's aligned tip into the
              // tracking ref — and a rebase-fallback align rewrites P (fact 14), so the aligned tip
              // can itself be divergent below P. Bridge it here, AFTER the re-fetch and BEFORE the
              // push, so pushToOrigin pushes B (P is an ancestor of B → fast-forward). One placement
              // covers all three arms (overlay, merge, rebase). A "failed" bridge throws
              // HistoryRewrittenError, which the arm's push catch rethrows (it catches only push-
              // protection / workflow-scope / non-ff); PRD #1416 M4's try/catch wrapping the whole
              // align chain then types it history_rewritten (never the generic catch, SC3).
              const o = await this.bridgeBareTrackingRefIfDivergent(
                alignBarePath,
                result.branch,
                flight,
                runLog,
              );
              if (o.kind === "failed") throw new HistoryRewrittenError(flight.publishedTip!);
              // PRD #1416 (MR-rework, finding 1): re-scan the now-trusted P..B delta before pushing
              // the bridged, aligned tip. On a trusted finding scanBridgedRangeAndBlock has already
              // terminally reported push_secret_blocked, so throw the data-free unwind sentinel — the
              // enclosing align try/catch and the outer finalize catch propagate it without pushing or
              // re-reporting (mirroring how HistoryRewrittenError unwinds this same nested closure).
              if (o.kind === "bridged" && (await scanBridgedRangeAndBlock(alignBarePath)) === "blocked") {
                throw new PushSecretBlockedSignal();
              }
              await pushToOrigin();
              alignPushed = true;
            };

            // Push the aligned branch. Two rejections here are the base-align-conflict path,
            // not a mislabel: a REPEAT workflow-scope rejection means the default's workflow
            // files moved again DURING our align (double-TOCTOU); a NON-FAST-FORWARD rejection
            // means the rebase fallback rewrote the history of an already-published branch (a
            // resume, or the self_improve fixed branch) so this non-forced push cannot
            // fast-forward, and force-push is denied by the guardrails by design. Both preserve
            // the diff and fail typed rather than lose it to the generic catch. Any OTHER push
            // error still rethrows unchanged (a genuine auth/transient/protected-branch failure
            // must not be mislabelled as a base-align conflict). Returns true if it
            // preserved-and-failed (the caller must then `return`), false on a successful push.
            const pushAlignedOrPreserve = async (): Promise<boolean> => {
              try {
                await fetchAndPush();
                return false;
              } catch (e) {
                // PRD #974 M2: an aligned push rejected by GitHub Push Protection (GH013) is a
                // secret the pre-push gitleaks scan missed — route it to the typed
                // push_secret_blocked fail (NO preserved diff: it may carry the detected secret)
                // rather than the base-align-conflict path or the generic catch.
                if (isPushProtectionRejection(e)) {
                  runLog.info(
                    "finalize base-align: aligned push rejected by GitHub Push Protection (GH013); failing typed, no preserved diff (it may carry the secret)",
                    { run_id: runId },
                  );
                  await failPushSecretBlocked();
                  return true;
                }
                const nonFf = isNonFastForwardRejection(e);
                if (!isWorkflowScopeRejection(e) && !nonFf) throw e;
                // Record WHICH cause fired so an operator reading logs can tell the two apart:
                // a repeat workflow-scope rejection (the default's workflow files moved again
                // DURING our align) versus a non-fast-forward (the rebase rewrote an
                // already-published branch's history, a resume or the self_improve fixed
                // branch, that the bot cannot force-push).
                runLog.info(
                  nonFf
                    ? "finalize base-align: aligned push rejected non-fast-forward (rebase rewrote an already-published branch's history the bot cannot force-push); preserving diff and failing typed"
                    : "finalize base-align: aligned push STILL workflow-scope-rejected (default moved again during align); preserving diff and failing typed",
                  { run_id: runId },
                );
                await failBaseAlignConflict();
                return true;
              }
            };

            // Issue #627 — the overlay gate (correctness pin). FRESHLY recompute the branch's
            // workflow-change signal here: the #377 guard earlier FAILS OPEN on a null diff, so
            // a branch that modified a workflow file can still reach this block. Overlaying the
            // default's workflow subtree would then CLOBBER that agent edit, so the overlay is
            // allowed ONLY when the diff succeeded AND the branch provably modified NO workflow
            // file. Any other case (null diff, or a real workflow edit) falls straight into the
            // EXISTING merge → rebase → preserve chain, unchanged.
            // Keep the overlay's tree-diff guard independent of the commit-based precheck.
            const alignChanged = await this.git.changedFiles(alignBarePath, trackingRef);
            const alignWfHits = alignChanged === null
              ? null
              : alignChanged.filter((file) => file.startsWith(".github/workflows/"));
            const canOverlay =
              alignChanged !== null && alignWfHits !== null && alignWfHits.length === 0;

            // Emit once here so the status fires on BOTH the overlay and the fallback paths.
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "branch is behind the default branch on .github/workflows; aligning before pushing",
              },
            });

            // PRIMARY (issue #627): overlay ONLY the default tip's .github/workflows/ subtree
            // onto the agent tip. It cannot conflict and is a fast-forward (original agent SHAs
            // preserved, nothing rebased), and it makes the tip's workflow tree equal main's —
            // all GitHub's tip-vs-default check requires — WITHOUT dragging in main's unrelated
            // changes the way a whole-tree merge/rebase does. Invoked OUTSIDE alignOp on
            // purpose: alignOp maps any throw to "conflict" (→ preserve-and-fail), which is
            // WRONG for the overlay — an overlay error must fall back to merge/rebase, not
            // preserve-and-fail. So the overlay gets its own try/catch here.
            let overlayHandled = false;
            if (canOverlay) {
              let overlayAligned = false;
              try {
                const res = await this.git.alignBranchWithDefault(
                  runnerClone.path,
                  result.branch,
                  originalAgentTip,
                  defTip,
                  "workflow-subtree",
                );
                overlayAligned = res === "aligned"; // the overlay never returns "conflict"
              } catch (e) {
                // (b) the overlay git op threw (a GENUINE unexpected git error) → fall back to
                // merge/rebase, NOT preserve-and-fail. Distinct message from (c) below.
                runLog.warn(
                  "finalize base-align: workflow-subtree overlay errored; falling back to merge/rebase",
                  { run_id: runId, error: errMessage(e) },
                );
              }
              if (overlayAligned) {
                try {
                  await fetchAndPush(); // sets alignPushed = true on success
                  overlayHandled = true;
                } catch (e) {
                  // PRD #974 M2: an overlay push rejected by GitHub Push Protection (GH013) is a
                  // secret gitleaks missed — typed push_secret_blocked fail (NO preserved diff:
                  // it may carry the secret), not a fall-back to merge/rebase (which cannot clear
                  // a secret) nor the generic catch.
                  if (isPushProtectionRejection(e)) {
                    runLog.info(
                      "finalize base-align: workflow-subtree overlay push rejected by GitHub Push Protection (GH013); failing typed, no preserved diff (it may carry the secret)",
                      { run_id: runId },
                    );
                    await failPushSecretBlocked();
                    return;
                  }
                  if (!isWorkflowScopeRejection(e) && !isNonFastForwardRejection(e)) throw e;
                  // (c) the overlay pushed but was STILL rejected (workflow-scope: the default
                  // moved again during our align; or non-fast-forward: a resumed/rewritten
                  // branch) → fall back to merge/rebase. Distinct message from (b) above so an
                  // operator can tell the two failure modes apart.
                  runLog.info(
                    "finalize base-align: workflow-subtree overlay push still rejected; falling back to merge/rebase",
                    { run_id: runId },
                  );
                }
              }
            }

            if (!overlayHandled) {
              const mergeRes = await alignOp("merge");
              if (mergeRes === "aligned") {
                try {
                  await fetchAndPush();
                } catch (e) {
                  // PRD #974 M2: a merge push rejected by GitHub Push Protection (GH013) is a secret
                  // gitleaks missed — typed push_secret_blocked fail (NO preserved diff: it may
                  // carry the secret), not the rebase fallback (which cannot clear a secret) nor
                  // the generic catch.
                  if (isPushProtectionRejection(e)) {
                    runLog.info(
                      "finalize base-align: merge push rejected by GitHub Push Protection (GH013); failing typed, no preserved diff (it may carry the secret)",
                      { run_id: runId },
                    );
                    await failPushSecretBlocked();
                    return;
                  }
                  if (isNonFastForwardRejection(e)) {
                    // Issue #631: a non-fast-forward rejection (an already-published branch — a resume, or
                    // the self_improve fixed branch — whose merge push cannot fast-forward) can't be cleared
                    // by the rebase fallback (it also can't force-push), so preserve the diff and fail typed
                    // rather than escape to the generic catch (raw message, no preserved_patch). This
                    // matches pushAlignedOrPreserve (:1945-1946), which likewise fails typed on non-ff.
                    // (The overlay push catch at :2020 handles non-ff differently — it can still fall
                    // back to merge/rebase — so this arm deliberately does NOT mirror it: once at the
                    // merge, a rebase cannot clear a non-ff on an already-published branch.)
                    runLog.info(
                      "finalize base-align: merge push rejected non-fast-forward; preserving diff and failing typed",
                      { run_id: runId },
                    );
                    await failBaseAlignConflict();
                    return;
                  }
                  if (!isWorkflowScopeRejection(e)) throw e;
                  // The merge did NOT clear GitHub's workflow-scope rejection → the proven
                  // rebase fallback (#422). alignBranchWithDefault rewinds to originalAgentTip
                  // first, so the rebase replays the ORIGINAL agent commits onto the fresh
                  // default rather than the merge commit.
                  runLog.info(
                    "finalize base-align: merge push still workflow-scope-rejected; trying rebase fallback",
                    { run_id: runId },
                  );
                  const rebaseRes = await alignOp("rebase");
                  if (rebaseRes === "aligned") {
                    if (await pushAlignedOrPreserve()) return;
                  } else {
                    await failBaseAlignConflict();
                    return;
                  }
                }
              } else {
                // The merge conflicted (or errored) — a rebase may still replay cleanly where a
                // single merge did not, so try it before giving up.
                runLog.info("finalize base-align: merge conflicted; trying rebase", {
                  run_id: runId,
                });
                const rebaseRes = await alignOp("rebase");
                if (rebaseRes === "aligned") {
                  if (await pushAlignedOrPreserve()) return;
                } else {
                  await failBaseAlignConflict();
                  return;
                }
              }
            }
          }
        }
      }
    } catch (e) {
      // PRD #1416 M4 (SC3): a bridge that could not be built/validated at a finalize push site was
      // thrown as HistoryRewrittenError; type it history_rewritten — never the generic catch
      // (agent_failure), never finalize_base_align_conflict (the align arms call failBaseAlignConflict
      // only for push-classified errors, never for this). Any other error rethrows unchanged.
      if (e instanceof HistoryRewrittenError) {
        await failHistoryRewritten(e.publishedTip);
        return;
      }
      if (e instanceof PushSecretBlockedSignal) {
        // PRD #1416 (MR-rework, finding 1): the align-path post-bridge scan already terminally
        // reported push_secret_blocked inside fetchAndPush; unwind and stop (never re-report, never
        // fall through to the plain-path push below).
        return;
      }
      throw e;
    }

    // PRD #400 M2: a TASK run always pushes its branch back (the deliverable is the
    // commits the user pulls from uzi/task/<id>) but opens a merge request only when
    // it opted in (`uzi handoff --mr` → runs.open_mr → claim.open_mr). Every non-task
    // kind keeps its current MR behaviour unconditionally. This is a distinct
    // kind+flag gate on MR-OPEN only — a no-MR task still pushes, so it does NOT take
    // the report_only path above (which pushes nothing).
    const openMr = claim.kind !== "task" || claim.open_mr === true;

    // The agent signalled done. The WORKER now performs the authenticated push
    // (+ MR when openMr) with the PAT — the agent never had a credential.
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: {
        text: openMr
          ? "work complete; pushing branch and opening merge request"
          : "work complete; pushing branch (no merge request — pull the branch)",
      },
    });
    // The finalize push. Skipped when the PRD #456 align path above already pushed the
    // aligned branch — the run pushes through EXACTLY ONE code path (`pushToOrigin`), so a
    // successful align-push and the normal push converge here without ever double-pushing.
    if (!alignPushed) {
      // PRD #1416 M3 (C3): non-destructively bridge a divergent tracking tip so the plain push
      // fast-forwards. On "bridged" the tracking ref now points at B (P is an ancestor of B), so
      // pushToOrigin pushes B. "clean"/"unknown" proceed unchanged. PRD #1416 M4: on "failed" the
      // divergent tip cannot be bridged, so type it history_rewritten and STOP here (no push) —
      // never the generic catch (SC3). A direct call+return is used because this site is at
      // phasePublish top level, so `return` unwinds the whole finalize (unlike the align site,
      // which throws to unwind its nested fetchAndPush and is caught by the wrap above). The
      // top-of-finalize secret scan ran earlier on the divergent H (its floor was unresolvable, so
      // it failed open); a secret on B is caught by the GH013 remote backstop in the push catch below.
      const o = await this.bridgeBareTrackingRefIfDivergent(
        finalizeBarePath,
        result.branch,
        flight,
        runLog,
      );
      if (o.kind === "failed") {
        await failHistoryRewritten(flight.publishedTip!);
        return;
      }
      // PRD #1416 (MR-rework, finding 1): a bridge just made this rewritten branch pushable, so
      // re-scan the now-trusted P..B delta on EVERY forge before pushing. A trusted finding reports
      // push_secret_blocked and STOPS (a direct return unwinds the whole finalize, like the
      // failHistoryRewritten site above).
      if (o.kind === "bridged" && (await scanBridgedRangeAndBlock(finalizeBarePath)) === "blocked") {
        return;
      }
      try {
        await pushToOrigin();
      } catch (e) {
        // PRD #974 M2 backstop: a GitHub Push Protection (GH013) rejection here means a secret
        // the pre-push gitleaks scan missed — route it to the typed push_secret_blocked fail
        // (NO preserved diff: it may carry the secret) rather than the generic catch. Any OTHER
        // push error rethrows (unchanged behavior).
        if (isPushProtectionRejection(e)) {
          await failPushSecretBlocked();
          return;
        }
        // issue #1117 — mr_rework moved-tip detection. A non-forced finalize push of an
        // mr_rework rework rejected non-fast-forward can mean a concurrent same-branch writer
        // advanced the MR branch under the run; route that distinct case to the branch_moved
        // disposition instead of letting it rethrow into the generic agent_failure catch.
        if (claim.kind === "mr_rework" && isNonFastForwardRejection(e)) {
          // Capture O — the origin branch tip at clone — BEFORE the detection fetch overwrites
          // refs/remotes/origin/<branch> with the fresh remote tip. The base is the ORIGIN tip
          // at clone, NOT runnerClone.baseCommit: on a resume the reseed sets baseCommit to the
          // tracking tip (ahead of origin), which would fail the ancestor check below and
          // silently regress a resumed run back to agent_failure.
          const originAtClone = await this.git
            .originBranchTip(finalizeBarePath, result.branch)
            .catch(() => null);
          let remoteTip: string | null = null;
          try {
            remoteTip = await this.git.fetchDefaultTip(
              finalizeBarePath,
              result.branch,
              claim.secrets.forge_pat,
              claim.repo.clone_url,
              claim.secrets.forge_username,
            );
          } catch {
            // Fetch failed → fall through to the generic throw (fail-safe): never mislabel a
            // real problem as benign.
          }
          // A genuine concurrent-writer advance: the remote tip changed since our clone AND
          // strictly descends from it (advanced forward, not rewritten/rewound). Anything else
          // (unchanged tip, fetch failure, divergent/rewound history) falls through to today's
          // behavior — never a false positive.
          if (
            originAtClone &&
            remoteTip &&
            remoteTip !== originAtClone &&
            (await this.git.isAncestorRef(finalizeBarePath, originAtClone, remoteTip))
          ) {
            await failBranchMoved();
            return;
          }
        }
        throw e;
      }
    }

    // PRD #1416 M3 (Part D): the MR bridge-note, derived from the PUSHED HISTORY — not a
    // flight-local flag. A bridge built before a park is invisible to a reclaimed run's finalize
    // (which builds no new bridge), but it is right here in P..pushedTip, so a reclaim still
    // reports it. Computed ONCE after whichever push landed (plain or align), so it is path-
    // independent. Only meaningful when there is a published floor P. Best-effort: rangeContainsBridge
    // never throws (a git error → false), so this can never fail a finalize whose push already landed.
    let bridged = false;
    if (flight.publishedTip) {
      const pushedTip = await this.git.trackingTip(finalizeBarePath, result.branch);
      if (pushedTip) {
        bridged = await this.git.rangeContainsBridge(
          finalizeBarePath,
          flight.publishedTip,
          pushedTip,
        );
      }
    }
    if (bridged) {
      // Worded GENERICALLY: the branch may have been bridged by THIS worker OR by the agent (the M2
      // steer's `git merge -s ours <P>`), so it never says "the worker bridged it".
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "the branch contains a history bridge (a published commit was restored as an ancestor so the branch fast-forwards); `git log --first-parent` reads as the intended history",
        },
      });
    }

    // PRD #400 M2: a no-MR task completes HERE — the branch is pushed, there is
    // nothing more to open. Report the branch on the completion payload (the same
    // `branch: result.branch` the MR path reports below) so runs.branch carries
    // uzi/task/<id> and the user knows exactly what to pull. No mr_iid/mr_web_url:
    // there is no merge request.
    if (!openMr) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `task complete; pushed ${result.branch} (no merge request — pull the branch)`,
        },
      });
      await finishCommittedPublish({
        status: "completed",
        branch: result.branch,
        prd_done_path: result.prdDonePath,
        milestones_completed: result.milestonesCompleted,
      }, "task run completed (no MR)", { branch: result.branch });
      return;
    }

    // ── PRD #1226 M4 (D5): the completion permit + exact-head bind ──────────────
    // ONLY an INTERLOCKED issue run (claim.config.completion_contract_version != null) runs this;
    // a legacy/non-interlocked run skips the whole block and its MR creation + completed report are
    // byte-for-byte unchanged. Reached AFTER the finalize push landed on origin (the normal push OR
    // the PRD #456 align push), so `H` below is the tip that ACTUALLY landed — never the pre-align
    // candidate. Interlocked runs are ISSUE runs that open MRs, so this sits on the openMr path only;
    // the no-MR task completion above is never interlocked and stays untouched.
    const interlocked = claim.config?.completion_contract_version != null;
    // `Closes #N` renders at MR creation for a LEGACY run only (unchanged). For an INTERLOCKED run it
    // starts false and STAYS false at creation: Closes is NEVER rendered at creation for an interlocked
    // run; it is added ONLY after the PR head is verified to equal H (the verification block below), so
    // a held, unverified-head MR can never carry a closing line (AC: only a verified full delivery
    // contains `Closes #N`).
    const renderCloses = !interlocked;
    // PRD #1227 M2: an OWNER PARTIAL run (the frozen contract's owner decisions deferred ≥1 milestone)
    // is a scope_reduced partial delivery that must NEVER close its issue — not even after PR-head
    // verification. It is threaded to reconcileMrDescription(!isOwnerPartial) below so the verified-head
    // reconcile re-renders the NON-closing partial body instead of adding Closes; mrDescription's own
    // effectiveCloses guard is the belt-and-suspenders. Absent/empty ⇒ false ⇒ the accept-closing and
    // full-delivery paths add Closes after verify exactly as before.
    const isOwnerPartial = (claim.config?.completion_scope?.deferred?.length ?? 0) > 0;
    // H, the exact landed head. Set ONLY on the interlocked granted path; it rides the completed
    // report (`head: completionHead`) and is compared against the created PR's head. Undefined for a
    // legacy run, so JSON.stringify drops it and the legacy completed report is byte-for-byte the same
    // on the wire.
    let completionHead: string | undefined;
    // Park the incomplete/unverifiable interlocked run, else report a typed failure. Shared by the
    // permit-denied and PR-head-mismatch paths (D5's held-or-fail). enterCompletionHold reaps,
    // captures a VERIFIED same-worker restore point and parks on a `paused` ACK (returns true), or
    // parks nothing (returns false); on true we skip finalization exactly like the
    // result.completionHeld branch above, and on false the run follows normal terminal cleanup
    // (enterCompletionHold cleared its preserve flags) so we report a static, content-free failure.
    // `holdReason` is a static hold-reason string, never raw model output.
    const holdOrFailInterlocked = async (holdReason: string): Promise<void> => {
      const held = await this.enterCompletionHold(flight, claim, holdReason, runLog);
      if (held) {
        executor.killAgentTree?.();
        await closeBatcher().catch(() => undefined);
        runLog.info("run entered the completion hold; skipping finalization", {
          run_id: runId,
          reason: holdReason,
        });
        return;
      }
      await closeBatcher();
      // PRD #1391 Run B M3 (N1): journal write-ahead so the completion-interlock failure survives an
      // outage as this exact outcome, not a generic agent_failure.
      await journalTerminalReport({ status: "failed", failure_reason: COMPLETION_INTERLOCK_UNHELD_REASON });
      runLog.info("run failed: completion interlock could not park the incomplete run", {
        run_id: runId,
        reason: holdReason,
      });
    };

    // PRD #1225 (CodeRabbit !1254): fail CLOSED — a terminal failure with NO hold attempt. Used when a
    // would-be hold path could NOT strip `Closes #N` from the MR (the non-closing reconcile write
    // failed). `createMergeRequest` can ADOPT a pre-existing MR that already carries `Closes #N`, so a
    // failed strip cannot guarantee the MR is non-closing; a hold is a nominally non-closing parked
    // state, so we must not enter it here. Failing is the safe direction — loud and terminal — rather
    // than parking a possibly-closing MR on an unverified head that a human could merge.
    const failInterlockedClosed = async (reason: string): Promise<void> => {
      executor.killAgentTree?.();
      await closeBatcher().catch(() => undefined);
      // PRD #1391 Run B M3 (N1): journal write-ahead so the fail-closed completion-interlock outcome
      // survives an outage as this exact outcome, not a generic agent_failure.
      await journalTerminalReport({ status: "failed", failure_reason: COMPLETION_INTERLOCK_UNHELD_REASON });
      runLog.info("run failed closed: completion interlock could not guarantee a non-closing MR", {
        run_id: runId,
        reason,
      });
    };

    if (interlocked) {
      // 1. Capture the EXACT landed head H. pushBranch pushes refs/uzi-runner/<branch>, and BOTH the
      //    normal finalize push (fetchAgentBranch at the top of this method) and the align push
      //    (fetchAndPush's re-fetch of the aligned tip) wrote that ref to the tip they pushed — so
      //    trackingTip reads the landed tip after either path. The frozen contract revision is echoed
      //    verbatim so the server can reject a revision drift. If either is unresolvable a permit
      //    cannot be bound to (run, revision, branch, head), so route to the hold rather than report
      //    completed.
      const head = await this.git.trackingTip(barePath, result.branch);
      const contractRevision = claim.config?.contract_revision;
      if (head === null || contractRevision === undefined) {
        runLog.warn(
          "completion interlock: the landed head or contract revision is unresolvable; holding rather than completing",
          { run_id: runId },
        );
        await holdOrFailInterlocked("completion identity unresolvable");
        return;
      }
      // 2. Request the permit bound to (run, contract_revision, branch, H). A denial is a normal 200
      //    body (granted:false), never a throw — the throw path (a transport/HTTP error) propagates to
      //    the generic catch, which fails the run without falsely completing.
      const permit = await this.client.requestCompletionPermit(runId, {
        contractRevision,
        branch: result.branch,
        head,
        // PRD #1247 M5: stamp the claim-lane generation (the SAME value the reportState closure
        // stamps) so the server refuses to issue a permit for a released/superseded stale flight.
        claimGeneration: flight.claimGeneration,
      });
      // 3. NOT granted: do NOT create the MR, do NOT render Closes, do NOT report completed — hold.
      if (!permit.granted) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text: "completion permit denied; holding the incomplete run instead of opening a merge request",
          },
        });
        await holdOrFailInterlocked("completion permit denied");
        return;
      }
      // 4. Granted: carry H on the completed report below. Closes is NEVER rendered at creation for an
      //    interlocked run — it is added ONLY after the PR head is verified to equal H (the verification
      //    block below re-asserts the canonical `Closes #N` body on a verified head), so a held,
      //    unverified-head MR can never carry a closing line.
      completionHead = head;
    }

    const targetBranch =
      claim.repo.default_branch?.trim() ||
      (await this.git.defaultBranchName(barePath)) ||
      "main";
    // Pick the forge client from the claim's forge_type (absent ⇒ gitlab, R8), so
    // the worker opens an MR on GitLab and a PR on Forgejo/GitHub from the same code
    // path; each client derives its own API base + project from repo.url (D9).
    // createMergeRequest is idempotent: for a ci_fix on an existing agent branch it
    // returns the EXISTING MR/PR (no second one, PRD #6); for a fresh ci-fix/pipeline-N
    // or agent/issue-N branch it opens one. Reporting its iid keeps the fix branch
    // watched so the verification sync can stamp the verdict.
    const forge =
      claim.repo.forge_type === "forgejo"
        ? this.forgejo
        : claim.repo.forge_type === "github"
          ? this.github
          : this.gitlab;
    // PRD #284 Layer A/D3: wrap the WHOLE createMergeRequest call (POST → duplicate
    // → findOpenMr GET) in the retry loop, not just its final thrown status. It is
    // already idempotent — on a duplicate it adopts the existing MR/PR — but a
    // transient findOpenMr failure after a duplicate POST would otherwise fail a run
    // whose MR actually exists; retrying the whole call re-runs it instead.
    const mr = await withForgeRetry(
      () =>
        forge.createMergeRequest({
          repoUrl: claim.repo.url,
          pat: claim.secrets.forge_pat,
          sourceBranch: result.branch,
          targetBranch,
          title: mrTitle(claim, result.scopeCapped, claim.config?.completion_scope),
          description: mrDescription(
            claim,
            result.branch,
            result.agentSelection,
            selfImproveSection,
            promptGuardSection,
            result.gatesUnverified,
            result.gatesDiscoveryTruncated,
            result.scopeCapped,
            renderCloses,
            claim.config?.completion_scope,
            bridged,
          ),
        }, boundarySignal),
      { log: runLog, signal: boundarySignal },
    );
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `merge request opened: !${mr.iid} ${mr.webUrl}` },
    });

    // ── PRD #1226 M4 (D5): PR-head verification (create-then-verify) ────────────
    // For an interlocked run (completionHead set on the granted path) the MR now exists; read its
    // head SHA and require it to equal the permitted head H. A mismatch (the PR points at a different
    // commit than the permit was bound to) or a read that THREW (a ForgeError = cannot verify) must
    // NOT report completed — the run holds with its MR already open. A legacy run has no completionHead
    // and never reads the PR head, so this block is skipped and its completion is unchanged.
    //
    // PRD #1225 (CodeRabbit !1254): the interlocked MR was created WITHOUT a `Closes #N` body, so a
    // freshly-created, unverified-head MR can never carry a closing line — the dangerous
    // create-then-verify state (a human merge closing the issue on an unverified head) is structurally
    // impossible for it. reconcileMrDescription ADDS the canonical `Closes #N` body ONLY after the PR
    // head is verified to equal H, and the head is then RE-READ to bind that add to the verified head
    // (a change in the read→add window strips Closes and holds); if the add FAILS the run HOLDS rather
    // than reporting completion (the completion contract requires the merged MR to carry Closes). The
    // hold branches strip Closes and add the unverified banner FIRST and REQUIRE that write to succeed:
    // `createMergeRequest` can ADOPT a pre-existing MR that already carried `Closes #N`, so a failed
    // strip cannot prove the MR is non-closing and we fail CLOSED instead of holding (stripClosesThenHold
    // → failInterlockedClosed). The whole block is skipped for a legacy run (completionHead ===
    // undefined), so its completion is byte-for-byte unchanged.
    const UNVERIFIED_BANNER =
      "> ⚠️ **Completion unverified.** uzi could not confirm this merge request's head matches the permitted completion head, so the run was held for owner review. This merge request does NOT close its issue and must not be merged as a completion until re-verified.";
    const reconcileMrDescription = async (withCloses: boolean, banner?: string): Promise<boolean> => {
      try {
        const base = mrDescription(
          claim,
          result.branch,
          result.agentSelection,
          selfImproveSection,
          promptGuardSection,
          result.gatesUnverified,
          result.gatesDiscoveryTruncated,
          result.scopeCapped,
          withCloses,
          claim.config?.completion_scope,
          bridged,
        );
        const desc = banner ? `${banner}\n\n${base}` : base;
        await withForgeRetry(
          () =>
            forge.updateMergeRequestDescription(
              claim.repo.url,
              claim.secrets.forge_pat,
              mr.iid,
              desc,
              boundarySignal,
            ),
          { log: runLog, signal: boundarySignal },
        );
        return true;
      } catch (e) {
        runLog.warn(
          "completion interlock: could not reconcile the MR description; leaving the created MR as-is",
          { run_id: runId, error: errMessage(e) },
        );
        return false;
      }
    };
    // PRD #1225 (CodeRabbit !1254): strip any `Closes #N` (writing the unverified banner) BEFORE a
    // hold. If that write FAILS we cannot guarantee the MR is non-closing — `createMergeRequest` can
    // ADOPT a pre-existing MR that already carried `Closes #N` — so fail CLOSED rather than hold a
    // possibly-closing MR on an unverified head.
    const stripClosesThenHold = async (holdReason: string, failReason: string): Promise<void> => {
      if (!(await reconcileMrDescription(false, UNVERIFIED_BANNER))) {
        await failInterlockedClosed(failReason);
        return;
      }
      await holdOrFailInterlocked(holdReason);
    };
    if (completionHead !== undefined) {
      let prHead: string;
      try {
        prHead = await forge.getMergeRequestHead(
          claim.repo.url,
          claim.secrets.forge_pat,
          mr.iid,
          boundarySignal,
        );
      } catch (e) {
        runLog.warn("completion interlock: could not read the PR head to verify it; holding", {
          run_id: runId,
          error: errMessage(e),
        });
        await stripClosesThenHold(
          "pr head unreadable",
          "could not strip Closes from an MR whose head is unreadable",
        );
        return;
      }
      if (prHead !== completionHead) {
        runLog.info("completion interlock: PR head does not match the permitted head; holding", {
          run_id: runId,
        });
        await stripClosesThenHold(
          "pr head mismatch",
          "could not strip Closes from an unverified-head MR",
        );
        return;
      }
      // Verified: ADD the canonical `Closes #N` body. The interlocked MR was created WITHOUT Closes,
      // so this is the ONLY place a completion's closing line is written (it is also a REPAIR on an
      // ADOPTED MR whose body a prior hold rewrote to the unverified variant). The completion contract
      // requires the merged MR to carry Closes, so if this write FAILS we hold rather than report
      // completion.
      // PRD #1227 M2: an OWNER PARTIAL (scope_reduced) run must NEVER close its issue even on a verified
      // head, so it re-renders its NON-closing partial body here (reconcileMrDescription(false)) instead
      // of adding Closes; the head is still verified (permit binding) — only the Closes-add is
      // suppressed. An accept-only/full-delivery run has isOwnerPartial=false and adds Closes as before.
      if (!(await reconcileMrDescription(!isOwnerPartial))) {
        await holdOrFailInterlocked("could not assert the verified-head completion body");
        return;
      }
      // BIND the Closes add to the verified head (CodeRabbit !1254): re-read the PR head AFTER writing
      // Closes. A head change between the first read and the add would otherwise leave `Closes #N` on
      // an unverified head. If the re-read is unreadable, or shows a changed head, strip Closes and
      // hold (fail closed if the strip itself fails) rather than report completion.
      let postHead: string;
      try {
        postHead = await forge.getMergeRequestHead(
          claim.repo.url,
          claim.secrets.forge_pat,
          mr.iid,
          boundarySignal,
        );
      } catch (e) {
        runLog.warn("completion interlock: could not re-verify the PR head after adding Closes; holding", {
          run_id: runId,
          error: errMessage(e),
        });
        await stripClosesThenHold(
          "pr head unreadable after Closes",
          "could not strip Closes after an unreadable re-verify",
        );
        return;
      }
      if (postHead !== completionHead) {
        runLog.info("completion interlock: PR head changed after adding Closes; stripping Closes and holding", {
          run_id: runId,
        });
        await stripClosesThenHold(
          "pr head changed after Closes",
          "could not strip Closes after a post-add head change",
        );
        return;
      }
    }

    // Persist the MR/PR web URL the forge just handed us (PRD #65 D8), so the web
    // links it directly instead of reconstructing the URL by string surgery. Omit
    // it when the forge returned none (mr.webUrl empty) so the server lands NULL and
    // the legacy forgeUrls.ts reconstruction still applies (R8, additive+optional).
    // prd_done_path (PRD #72 M4) rides the same terminal report. Omitted when the
    // executor set nothing — same `|| undefined` shape as mr_web_url on this line,
    // so "old worker" and "moved no PRD" are indistinguishable on the wire by
    // design, and the api treats both as NULL.
    // PRD #265 M1: the finished-milestone ids the lead declared on signal_done ride the
    // same terminal report. Omitted when the executor set nothing (non-issue run, or the
    // lead declared none) — same absent-vs-present discipline as prd_done_path, so the
    // server UNIONs them into milestones_completed only when actually declared and a
    // no-declaration completion is byte-identical to before.
    await finishCommittedPublish({
      status: "completed",
      branch: result.branch,
      mr_iid: mr.iid,
      mr_web_url: mr.webUrl || undefined,
      prd_done_path: result.prdDonePath,
      milestones_completed: result.milestonesCompleted,
      // PRD #634 M3: stamp the scope-capped disposition so the server records
      // stop_kind='scope_capped'. OMITTED (not false) on a normal completion, so the wire
      // shape is unchanged for every non-truncated run.
      scope_capped: result.scopeCapped ? true : undefined,
      // PRD #1226 M4 (D5): the EXACT permitted+verified head H, so the server consumes the
      // completion permit issued for (run, contract_revision, branch, H). Set only for an
      // interlocked run; undefined for a legacy run, so JSON.stringify drops it and the legacy
      // completed report is byte-for-byte unchanged on the wire.
      head: completionHead,
    }, "run completed", { branch: result.branch, mr_iid: mr.iid });
  }

  private buildFlight(
    claim: ClaimResponse,
    runId: string,
    executor: Executor,
    runHome: string | undefined,
    gitBasic: string,
    runScopedSecrets: string[],
  ): RunFlight {
    const runLog = this.log.child({
      run_id: runId,
      issue_iid: claim.issue_iid,
    });
    // Same secret set for both redactors: the batcher scrubs run_message payloads;
    // redactText scrubs strings that reach the API outside a payload (failure_reason,
    // and the PRD #99 agent_label/agent_instance the batcher now carries alongside
    // the payload — `redact` walks inside a payload object and never sees them).
    const secrets = [
      claim.secrets.forge_pat,
      claim.secrets.anthropic_oauth_token,
      this.joinToken,
      gitBasic,
    ];
    // PRD #1171 M3: APPEND the two Codex canaries (access token + capability) so both
    // redactors scrub them from run-message payloads and out-of-payload strings
    // (failure_reason). Append-only — the existing forge_pat/anthropic/join/gitBasic
    // ordering is untouched; absent on an ordinary Claude claim (nothing added).
    if (claim.secrets.codex) {
      secrets.push(claim.secrets.codex.access_token, claim.secrets.codex.capability);
    }
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    // PRD #1247 M5b: the claim-lane generation this claim holds. Server-side NOT NULL DEFAULT 0;
    // `?? 0` covers a pre-#1296 payload that omits the field. Held on the flight and stamped on
    // every mutating report + message batch so the server's per-query fence can engage.
    const claimGeneration = claim.claim_generation ?? 0;
    const batcher = new MessageBatcher(
      this.client,
      runId,
      claim.last_seq,
      this.batchMs,
      runLog,
      redact,
      redactText,
      {
        // PRD #1391 M2: spill to the outbox after a sustained transient outage instead
        // of tripping. Segments carry the claim generation (default 0 when absent) so
        // replay can ride it under #1247's fence (D11). transientTripMs/spillBufferBytes
        // fall back to the batcher's own defaults when unset.
        ...(this.outbox ? { outbox: this.outbox } : {}),
        generation: claim.claim_generation ?? 0,
        ...(this.transientTripMs !== undefined ? { transientTripMs: this.transientTripMs } : {}),
        ...(this.outboxSpillBufferBytes !== undefined
          ? { spillBufferBytes: this.outboxSpillBufferBytes }
          : {}),
      },
    );
    // PRD #1391 M2: register this run's batcher in the shared re-arm registry so the
    // per-worker drainer can return it to the network once its spilled segments retire
    // — reachable while the run is still live and holds pending segments. Dropped in
    // executeClaim's terminal finally.
    this.rearm?.set(runId, () => batcher.rearm());

    // Cancel/shutdown spans the whole run; a `cancel` input aborts it via the
    // steering channel, which the executor's ctx.signal watches.
    const cancel = new AbortController();
    // PRD #41: `notify` lets the steering channel post a feed notice when it discards a
    // verdict/revision written against a stale plan version — wired to the batcher here
    // so the channel never reaches into runner internals.
    const steering = new SteeringChannel(
      this.client,
      runId,
      this.pollMs,
      runLog,
      cancel,
      {
        notify: (text) =>
          batcher.emit({ kind: "status", agent: "worker", payload: { text } }),
        // PRD #1247 M5b: the claim's generation, so the poll loop acts on a credential_switch
        // signal ONLY when it targets THIS claim (a signal for a superseded claim is ignored).
        claimGeneration,
      },
    );
    // issue #552 M3: a graceful `uzi run stop` (PRD #517 M4) consumed into the worker's
    // stopRequested flag is lost if the worker dies before winding the park down. The
    // server re-delivers the durable runs.stop_kind='stopped' fact as claim.stop_pending
    // on every claim; seeding it here reconstructs the sticky stop state so the resumed
    // run's interactive park ends `stopped` immediately (steering.awaitFollowUp's arm-time
    // check) instead of waiting out the idle timeout. Absent ⇒ untouched (today's path).
    if (claim.stop_pending) steering.seedStopRequested();
    // PRD #1190 M2: re-seed a PENDING pause from the durable claim columns (the direct analog of
    // stop_pending above), so a worker that died with a pause pending re-arms it on resume with
    // NO fresh input. The executor reads the seeded mode at its first loop boundary
    // (ctx.pauseModeRequested → steering.getPauseMode) and PARKS there as an ACK-independent
    // fallback (PRD #1190 rework N1): a resumed pause parks at its first boundary even if the
    // running-report ACK's pauseRequested regressed. In the normal case the ACK also re-fires
    // pauseRequested, so the seed is a real safety net rather than the sole trigger.
    if (claim.pause_pending) steering.seedPauseRequested(claim.pause_mode);

    // Last SDK session id the executor observed; carried on EVERY state report so
    // resume survives a lost report.
    // Returns the server's acknowledgement (PRD #35), which every caller here still
    // ignores. Widened from Promise<void> so the park branch can read it without
    // re-plumbing this closure; the annotation is the only change, and awaiting a
    // value nobody binds behaves exactly as before.
    const flight: RunFlight = {
      runId,
      executor,
      runHome,
      runScopedSecrets,
      runLog,
      redact,
      redactText,
      batcher,
      cancel,
      steering,
      claimGeneration,
      observedSessionId: undefined,
      reportState: async (body, signal) => {
        // PRD #1390 M2a: this same choke point is where the run announces every phase
        // transition, so reflect the four snapshot phases (running / awaiting_approval /
        // awaiting_input / awaiting_followup) into the active-run registry BEFORE the report
        // is sent, so a concurrent snapshot build sees the run in its true phase. Every other
        // status leaves the entry unchanged (removed by the terminal finally / requeue).
        const phase = snapshotPhaseOf(body.status);
        if (phase) this.snapshotRegistry?.setPhase(runId, phase);
        // PRD #1247 M5b: stamp the claim-lane generation on EVERY mutating report. This is the
        // single choke point every one of the ~71 report sites goes through (incl. the terminal
        // `failed` report), so the server's per-query fence engages uniformly. Additive/optional:
        // the observedSessionId injection is preserved, and claim_generation rides beside it.
        const stamped: Parameters<WorkerClient["reportState"]>[1] = {
          ...body,
          claim_generation: flight.claimGeneration,
          ...(flight.observedSessionId ? { session_id: flight.observedSessionId } : {}),
        };
        const ack = await this.client.reportState(runId, stamped, signal);
        // PRD #1247 M5b (MINOR-7): the held-state switch signal rides the state ACK too — the
        // advertised SECONDARY transport beside /inputs. Feed it into the SAME generation-checked,
        // idempotent, defer-aware trigger the inputs poll uses (tripCredentialSwitch), so a failing
        // /inputs poll can no longer disable the switch: a report is made far more often than an
        // inputs poll, and the two transports compose (a switch trips at most once). Fed BEFORE the
        // stale_claim check below; the two never co-occur (workerStateAck omits the switch signal
        // from the stale_claim disposition), so ordering is immaterial to correctness.
        if (ack.credentialSwitch) flight.steering.tripCredentialSwitch(ack.credentialSwitch.generation);
        // PRD #1497 M2 (D5/D16): recognise a SERVER-side wall park BEFORE the generic stale handling.
        // A fenced report whose ACK reads disposition:"stale_claim" (the fence rejected it) AND
        // status:"paused" + hold_reason:"budget_exhausted" (the sweep parked the row at the wall)
        // is not an ordinary supersede: the run is parked, non-terminal, and a safe incarnation will
        // resume it. Throw the DISTINCT ServerWallParkedError so executeClaim's catch chain RETAINS
        // clone + HOME and reports nothing terminal — its arm runs AHEAD of the StaleClaimError arm,
        // which would instead run ordinary teardown (removing this worker's clone). Checked before the
        // staleClaim throw below so the pair is caught here, not there.
        if (
          ack.staleClaim &&
          ack.status === "paused" &&
          ack.holdReason === "budget_exhausted"
        ) {
          throw new ServerWallParkedError();
        }
        // PRD #1247 M5b: a stale_claim disposition means a held-state switch RELEASED this claim
        // (or a reclaim SUPERSEDED it) — the flight no longer owns the run and MUST STOP. Throw so
        // executeClaim's catch chain closes the batcher and ends the flight with NO terminal
        // report (another claim owns the run now). A stale ack on the TERMINAL `failed` report is
        // a no-op: that reportState is already `.catch(...)`-guarded, so the throw is swallowed.
        if (ack.staleClaim) throw new StaleClaimError();
        // issue #1582 M2: a TERMINAL report's ACK drives the settlement lifecycle (promote the
        // write-ahead `pushed` head on a completed outcome, else stop the records). Here, inside the
        // send, so it lands BEFORE the terminal resolve retires the outbox journal.
        if (body.status === "completed" || body.status === "failed") {
          await this.observeSettlementTerminalAck(runId, flight.claimGeneration, body, ack);
        }
        return ack;
      },
      barePath: undefined,
      worktreePath: undefined,
      // PRD #267: time-based origin-checkpoint gate state (per run). `lastPublish` starts
      // at run start so the first time-based publish fires ~one interval in; both are
      // updated by ANY origin publish (milestone or time). Decision 9: the publish "new
      // work" test keys on `lastPublishedTip`, NOT the fetch-skip below.
      lastPublish: this.now(),
      lastPublishedTip: undefined,
      // PRD #1062 M2 (#1036): the checkpoint ref tip this run last knows, for the overlay's
      // prevCheckpointTip. Seeded from the persisted tip on the claim (a resume already has an
      // overlay on the ref), advanced on every confirmed publish.
      lastCheckpointRefTip: claim.checkpoint_tip ?? undefined,
      lastAttemptedCheckpointRefTip: undefined,
      // issue #1030: per-run dedupe set for checkpoint-publish outcome feed lines.
      reportedPublishOutcomes: new Set<string>(),
      // issue #1597 M2: the per-flight sink gate (mid-turn tick vs every durable-state path) and the
      // latch a cancelled tick sets when it had to RETAIN a lock file in the worker bare.
      sinkGate: new SinkGate(),
      retainedBareLocks: [],
      survivingTickGroups: [],
      pendingPublish: false,
      // PRD #218 M1: the run's branch, hoisted so the park/shutdown fetch-back in the
      // catch can name it. `runnerClone` is declared inside the try and there is no
      // `result` on those paths, so `runnerClone.branch` is the source of truth and it is
      // copied here the moment the clone exists.
      branch: undefined,
      // PRD #218 M1: this run's shutdown-registry entry, hoisted so the catch can read
      // `active.shuttingDown` to tell a graceful shutdown apart from every other failure.
      active: undefined,
      // PRD #35: set ONLY by a park the server acknowledged as `limit_wait`. It gates
      // the two filesystem removals in the finally and nothing else. Declared here
      // rather than in the catch so the finally can see it; false is the safe default,
      // so every path that never reaches the park logic cleans up exactly as before.
      parked: false,
      // PRD #1391 Run B M3 (N2/D5): no terminal outcome resolved yet. Set by journalAndSendTerminal
      // the moment any completed/failed for this generation is sent/resolved (write-ahead or not),
      // so reportGenericFailure never falls through to a SECOND `failed` once one is final.
      terminalResolved: false,
      // #1539: set by the permanent-failure hook (undefined until then) — the pre-settle reap
      // outcome and the safety epoch it reaped, for reportGenericFailure's one-time custody settle.
      permanentFailureReap: undefined,
      permanentFailureReapSafety: undefined,
      // PRD #556 M1 / #1197: set by shutdown or a pending recovery capture/report.
      // Like `parked`, it gates EXACTLY the two filesystem removals in the
      // finally (the sibling skills plugin dir and the per-run HOME) and nothing else — so
      // a same-worker re-claim within the affinity grace can resume the SDK session. It is
      // a distinct flag from `parked` on purpose: `parked` also drives park-only report
      // semantics, resume seeding, and the park log, none of which apply to a shutdown.
      preserveSession: false,
      // PRD #1392 M2: set true ONLY by the pre-clone forge-unreachable park. false everywhere
      // else, so no other path's HOME preservation semantics change.
      preClonePark: false,
      preserveRecoveryClone: false,
      ciFixHumanApproved: false,
      runnerClone: undefined,
      result: undefined,
    };

    // PRD #1391 Run B M3 (D5) / #1539: a PERMANENT message failure now journals `failed` WRITE-AHEAD
    // (durable), then ABORTS the attempt, then REAPS this generation's provider while the run is still
    // actively-claimed, then resolves/sends the terminal — replacing today's fire-and-forget `failed`
    // report that left the executor running (the split-brain in miniature, fact 1). The handler is
    // ASYNC and trip() captures its promise into batcher.permanentFailureSettled, which
    // reportGenericFailure AWAITS before it checks the journal — so the abort's terminal `failed` is
    // observable ONLY AFTER the outcome is durable, and a completion that races the trip can never
    // reverse the first durable winner (the no-replace install in journalTerminal arbitrates, D4). The
    // reap runs between the install and the send (in beforeResolve), so the Codex reconcile is
    // authorized (still actively-claimed) rather than refused 409 after a terminal report — the #1539
    // fix. Chat keeps today's non-journal behaviour (chat-runner.ts). When no outbox is wired this
    // degrades to today's direct `failed` report + abort + reap.
    // The hook body lives in handlePermanentFailure (a named method) so its install-BEFORE-abort,
    // abort-BEFORE-reap, reap-BEFORE-send ordering is unit-testable against the REAL code — a mutation
    // to any of those orderings reddens a test.
    batcher.onPermanentFailureReport(({ reason }) => this.handlePermanentFailure(claim, flight, reason));

    return flight;
  }

  /** issue #1319 — authoritative owner-derived reclaim for a terminal-owner clone orphan.
   *  Runs the NEW owner-scoped orphan-classification read and quarantines the journaled
   *  residue (RETAINED, discard:false) ONLY when EVERY predicate holds: (a) the owner run is
   *  terminal, (b) its repo equals the claimant's, (c) its OWNER-derived clone branch equals
   *  the journal/current branch, (d) its owner-derived canonical path equals the journaled
   *  path. On a 404 / non-terminal owner / repo|branch|path mismatch / malformed identity /
   *  any transport or probe failure it re-throws `original` so phaseClone fails closed with the
   *  journal and clone UNTOUCHED (never quarantine on an unproven owner). retireRunnerClone's
   *  own pre-rename (ownerRunId, clonePath) re-validation stays authoritative. Serves BOTH
   *  Case B (ForeignCaptureBlockedError, same-slug) and Case A (CapturePathMismatchError,
   *  cross-kind slug divergence). */
  private async reclaimTerminalOrphan(
    barePath: string,
    claim: ClaimResponse,
    journaledPath: string,
    branch: string,
    ownerRunId: string,
    original: Error,
  ): Promise<void> {
    let id: RunOrphanClassificationResponse;
    try {
      id = await this.client.getRunOrphanClassification(claim.run_id, ownerRunId);
    } catch {
      throw original; // 404 (owner not in this owner+repo scope) or any transport error
    }
    if (!TERMINAL_RUN_STATUSES.has(id.status)) throw original;           // (a) terminal owner
    if (id.repo_id !== claim.repo.id) throw original;                    // (b) same repo
    // (c)+(d): the OWNER's own kind/identity must reproduce this branch and the journaled path.
    // mr_rework carries its branch in pipeline_ref (runs.branch is NULL) — mirror the server's
    // claim_assembly normalization. self_improve/prompt derive from the owner runId, so the
    // persisted branch is never the source (predicate (c) IS the equality check for them).
    const ownerKind = resolveRunKind(id.kind);
    const owner = deriveCloneKey({
      kind: ownerKind,
      runId: ownerRunId,
      issueIid: id.issue_iid,
      branch: ownerKind === "mr_rework" ? id.pipeline_ref : id.branch,
      pipelineId: id.pipeline_id,
      pipelineRef: id.pipeline_ref,
      defaultBranch: claim.repo.default_branch,
    });
    if (!owner) throw original;                                          // malformed identity
    if (owner.branch !== branch) throw original;                         // (c) owner-derived branch
    if (this.git.runnerClonePath(barePath, owner.slug) !== journaledPath) throw original; // (d)
    try {
      await this.git.retireRunnerClone(barePath, journaledPath, branch, ownerRunId, { discard: false });
    } catch {
      throw original; // journal moved under us / containment failure -> fail closed
    }
  }

  private async phaseClone(claim: ClaimResponse, flight: RunFlight): Promise<void> {
    const { runLog, reportState, steering, batcher, cancel } = flight;
    const runId = claim.run_id;
    runLog.info("run claimed", {
      repo: claim.repo.url,
      branch: claim.branch ?? null,
    });
    // PRD #1391 Run B M4: CAPTURE the first `running` ack (previously discarded). A stale_claim
    // disposition already threw StaleClaimError inside the reportState closure; here we additionally
    // STOP when the report is refused (applied:false) with a TERMINAL status — the run reached a
    // terminal state out from under this claim (a racing cancel / an outcome that already landed), so
    // continuing the phase clone would work a claim the run has already left. Throw the non-`failed`
    // stop signal rather than clone-and-run on it.
    const runningAck = await reportState({ status: "running" });
    if (!runningAck.applied && runningAck.status !== undefined && TERMINAL_RUN_STATUSES.has(runningAck.status)) {
      runLog.info("first running report refused with a terminal status; stopping the phase clone", {
        run_id: runId,
        status: runningAck.status,
      });
      throw new RunningAckTerminalError(runningAck.status);
    }
    steering.start();

    // PRD #1390 M4 — env-gated e2e DROP-EXECUTION seam. OFF unless UZI_E2E_DROP_ON_SENTINEL
    // is set (so this whole block is inert in production). The run has just reported `running`
    // (its status_since is now), and it is still listed at `running` in the active-run
    // registry (executeClaim registered it before phaseClone). Ending the flight HERE — before
    // ensureClone, so there is no worktree and no recovery journal to retire — models a live
    // worker that silently loses one execution: no terminal report is sent (the run stays
    // `running` for the api's heartbeat missing-run requeue to reclaim after the fence), the
    // ordinary finally drops the snapshot entry, and the claim loop is paused so the e2e can
    // observe the requeued run sitting `queued` before a reclaim. See E2EDropExecutionError.
    if (
      process.env.UZI_E2E_DROP_ON_SENTINEL === "1" &&
      (claim.issue_description?.includes(E2E_DROP_SENTINEL) ||
        claim.issue_title?.includes(E2E_DROP_SENTINEL))
    ) {
      this.snapshotRegistry?.pauseClaimForE2E();
      throw new E2EDropExecutionError();
    }

    // PRD #1392 M2: only `ensureClone` is wrapped in `withForgeRetry` (fact 4). When it exhausts
    // the schedule and rethrows the last raw git/forge error, a TRANSIENT verdict (a DNS blip, a
    // connection reset, a 5xx) becomes a ForgeUnreachableAtCloneError so executeClaim's catch can
    // PARK the run instead of failing it; a PERMANENT verdict (401/403/404) is rethrown unchanged
    // and still fails immediately. `flight.barePath` is assigned ONLY on success, so a park leaves
    // it undefined (no clone, no worktree, no plugin dir exist pre-clone).
    let barePath: string;
    try {
      barePath = flight.barePath = await this.git.ensureClone(
        claim.repo.clone_url,
        claim.secrets.forge_pat,
        claim.secrets.forge_username,
      );
    } catch (err) {
      if (classifyForgeError(err) === "transient") {
        throw new ForgeUnreachableAtCloneError(errMessage(err), err);
      }
      throw err;
    }
    let retained = false;
    try {
      const runnerClone = (flight.runnerClone = await this.runnerCloneForClaim(barePath, claim));
      flight.worktreePath = runnerClone.path;
      flight.branch = runnerClone.branch;
    } catch (err) {
      if (err instanceof PendingRecoveryCaptureError) {
        // The git layer stopped BEFORE rm. Capture this same run's retained source
        // clone with the ordinary recovery loop; never start a model on it first.
        flight.worktreePath = err.clonePath;
        flight.branch = err.branch;
        flight.preserveRecoveryClone = true;
        flight.preserveSession = true;
        retained = true;
      } else if (err instanceof ForeignCaptureBlockedError) {
        // issue #1315/#1319 Case B: the canonical clone is journaled to ANOTHER run's
        // retained capture (a matched canonical pair). The authoritative owner-derived
        // validation decides — a TERMINAL owner in the same repo whose derived branch +
        // canonical path match the journal is reclaimed (residue quarantined, RETAINED),
        // else fail closed. Replaces the old worker-scoped getRunOwnership probe, which
        // 404'd on a worker move (Gap 2).
        await this.reclaimTerminalOrphan(barePath, claim, err.clonePath, err.branch, err.ownerRunId, err);
        const runnerClone = (flight.runnerClone = await this.runnerCloneForClaim(barePath, claim));
        flight.worktreePath = runnerClone.path;
        flight.branch = runnerClone.branch;
      } else if (err instanceof CapturePathMismatchError) {
        // issue #1319 Case A: a claimant-relative clone-path mismatch (the cross-kind slug
        // divergence, e.g. an issue owner's `issue-N` vs this mr_rework's `agent-issue-N`).
        // The SAME owner-derived validation decides; any unmet predicate fails closed.
        await this.reclaimTerminalOrphan(barePath, claim, err.journaledPath, err.branch, err.ownerRunId, err);
        const runnerClone = (flight.runnerClone = await this.runnerCloneForClaim(barePath, claim));
        flight.worktreePath = runnerClone.path;
        flight.branch = runnerClone.branch;
      } else {
        throw err;
      }
    }
    const active: ActiveRun = (flight.active = { cancel, shuttingDown: false });
    this.activeRuns.set(runId, active);
    if (this.shuttingDownGlobal) {
      active.shuttingDown = true;
      cancel.abort();
    }
    if (retained) throw new TransientRecoveryError("recovering retained work before reseeding");

    // PRD #1416 M1: record the published floor P ONCE at claim — the branch's forge tip as of
    // this clone (fact 2), read from the worker bare WITHOUT a fetch. Placed after the whole
    // clone try/catch so it covers the primary AND the reclaim paths (both assign runnerClone);
    // the retained-recovery path threw above and re-claims later, recording P on that pass.
    // Null when the branch did not exist on the forge at clone (a fresh issue run). This is
    // runner-level flight state that survives an executor restart — never RunnerClone.baseCommit,
    // which can point at unpublished recovered work. checkpointFloor C initialises to P; later
    // milestones advance it to each confirmed checkpoint tip.
    flight.publishedTip = (await this.git.originBranchTip(barePath, flight.branch!)) ?? undefined;
    flight.checkpointFloor = flight.publishedTip;

    // Journal ownership before any model can write. The worker-owned bare config
    // survives failed captures, process restarts, and runner-owned clone tampering.
    await this.git.markRecoveryCapture(barePath, flight.worktreePath!, flight.branch!, runId);
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: { text: `runner clone ready on ${flight.branch}` },
    });
  }

  private async phaseResume(
    claim: ClaimResponse,
    flight: RunFlight,
  ): Promise<string | undefined> {
    const { runLog, reportState, batcher, runHome } = flight;
    const runId = claim.run_id;
    const runnerClone = flight.runnerClone!;
    // PRD #218 M3 / #759 M5: say what a resume recovered, in a WORKER status rather than by
    // the lead noticing the tree changed under it. Two axes cross here — WHAT kind of work
    // was recovered, committed vs. uncommitted, and each is honest about a distinct outcome:
    //   - COMMITTED work (priorCommits > 0): the tracking-ref leg (same-worker, its owner
    //     stamp matched THIS run — M2) or the checkpoint leg (cross-worker, #122 M8). The
    //     message says which and names the count.
    //   - UNCOMMITTED WIP (runnerClone.wipRecovered — #759 M2): a `wip(park):` marker was
    //     `reset --soft` back to the working tree, so its edits returned UNCOMMITTED and the
    //     marker never enters the history. priorCommits was computed AFTER that reset (off
    //     the marker's parent), so it NEVER counts the marker — a pure WIP recovery has
    //     priorCommits === 0. This is a PARTIAL, unreviewed snapshot, NOT a committed
    //     milestone, so its wording says to verify it against the plan.
    //   - NOTHING recovered on a RESUME (session id present, seededFrom "default", no WIP):
    //     no origin branch, no tracking ref THIS run owns, no recoverable WIP — a
    //     cross-worker resume (R1) or a diverged checkpoint that could not be applied. The
    //     tree is lost for this run; admit it (the #218 M3 loss notice, unchanged wording).
    //
    // Branch order is load-bearing: the pure-WIP branch (3) MUST precede the loss branch (4).
    // A cross-worker DIVERGED WIP recovery (#759 M2 leg #4) recovers the WIP tree onto the new
    // floor but leaves seededFrom === "default" (the base is the floor, not the checkpoint) with
    // wipRecovered === true. If the loss branch ran first it would FALSELY fire "no earlier work
    // could be recovered" on exactly that successful recovery. With branch 3 ahead of it, the
    // loss notice fires only when NOTHING — committed or WIP — was recovered: the residual
    // #218-M3 loss case.
    const wipRecovered = runnerClone.wipRecovered === true;
    if (runnerClone.seededFrom === "tracking" && runnerClone.priorCommits > 0) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: wipRecovered
            ? `recovered ${runnerClone.priorCommits} commit(s) plus your uncommitted work-in-progress from this run's interrupted attempt`
            : `recovered ${runnerClone.priorCommits} commit(s) of work from this run's interrupted attempt`,
        },
      });
    } else if (runnerClone.seededFrom === "checkpoint" && runnerClone.priorCommits > 0) {
      // PRD #122 M8: seeded off ANOTHER worker's brokered checkpoint (origin's
      // refs/uzi-checkpoints/<branch>) — a cross-worker recovery the lead cannot infer
      // from the tree alone. priorCommits counts what the checkpoint carries. Gated on
      // priorCommits > 0 (like the tracking notice above) so it never claims to have
      // "recovered 0 commit(s) from a checkpoint". #759 M5: a checkpoint can ALSO carry a
      // reset-soft'd WIP tree alongside its commits — mention it when it did.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: wipRecovered
            ? `recovered ${runnerClone.priorCommits} commit(s) plus your uncommitted work-in-progress from a checkpoint on another worker`
            : `recovered ${runnerClone.priorCommits} commit(s) from a checkpoint on another worker`,
        },
      });
    } else if (wipRecovered) {
      // PRD #759 M5: a PURE uncommitted-WIP recovery — no committed milestones came back
      // (priorCommits === 0). This is the #685 shape and covers same-worker tracking with
      // zero commits, a cross-worker clean checkpoint with zero commits, AND the
      // cross-worker DIVERGED cherry-pick leg (seededFrom stays "default"/origin). The
      // recovered content is an UNCOMMITTED, PARTIAL snapshot from an interrupted attempt,
      // NOT a committed milestone — so tell the agent to verify it against the plan before
      // building on it. This branch precedes the loss notice so the diverged case
      // (seededFrom === "default" + wipRecovered) never falsely reports total loss.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            "recovered your uncommitted work-in-progress from this run's interrupted attempt — " +
            "a partial snapshot, so verify it against the plan before continuing",
        },
      });
    } else if (claim.session_id != null && runnerClone.seededFrom === "default") {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            "no earlier work could be recovered for this run on this worker — " +
            "starting from the default branch, so some work may be repeated",
        },
      });
    }
    // PRD #122 M8: a mirrored checkpoint existed but DIVERGED from origin, so origin won
    // and the checkpointed work was set aside. Independent of the seed leg (the base is
    // origin/default here, not the checkpoint) — say so LOUDLY rather than dropping it
    // silently.
    if (runnerClone.checkpointSetAside) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "checkpointed work was set aside — origin diverged; starting from origin",
        },
      });
    }

    // PRD #628 M4: signal the server to CLEAR this run's stale milestones_completed when
    // the reseed recovered NO committed work (seededFrom === "default", equivalently
    // priorCommits === 0). milestones_completed is a monotone union server-side, so pass-1's
    // milestones would otherwise read as "done" while pass-2 re-implements them from the
    // default branch — the "marked done but still working on them" symptom. This is a
    // DEDICATED, ONE-SHOT run-start report: the field rides THIS report only and NEVER a
    // reportIteration heartbeat (the server clears on it, so emitting it per-heartbeat would
    // wipe live progress every iteration). AWAITED because correctness depends on delivery
    // (reportState has bounded retries) — but best-effort in spirit: it must not fail the run,
    // so a failed report is logged and swallowed. Keyed on the TREE signal, NOT the session
    // signal (RESUME_LINEAGE_BREAK_EVENT) — the two diverge once M2 lands, and a re-claim that
    // recovers the tree via checkpoint (seededFrom "checkpoint") legitimately keeps its
    // milestones. Gated on claim.session_id != null (a RESUME), mirroring the tree-loss
    // feed message at ~:603: a brand-new first attempt has no prior milestones to clear,
    // so it must NOT emit a spurious extra run-start report (that perturbs the status-report
    // sequence other runner tests assert, e.g. runner-push-mr).
    if (claim.session_id != null && runnerClone.seededFrom === "default") {
      try {
        await reportState({ status: "running", seeded_from_default: true });
      } catch (e) {
        runLog.warn("could not report seeded_from_default (milestone reset signal)", {
          error: errMessage(e),
        });
      }
    }

    // Resume preflight (issue #105). The claim carries the session id the run last
    // reported, but the transcript it names lives under the per-run HOME on the
    // worker that WROTE it — and a requeued run whose affinity grace lapsed can land
    // on a different worker, where it does not exist. Not only cross-worker: a session
    // is keyed by HOME *and* cwd, so a replaced volume or a changed clone path loses
    // it on the same box. The SDK resolves a resume locally, so an unresolvable id
    // does not start fresh: it fails the very first turn with `error_during_execution`
    // and takes the whole run with it. If it is gone, drop the resume and SAY so —
    // continuing without the earlier context beats losing the run, but only if the
    // feed admits the context is gone rather than quietly re-treading ground.
    //
    // The check globs this HOME's project dirs rather than computing the one the cwd
    // encodes to (sdk-session.ts explains why the computed path would false-absent on
    // a symlinked data dir). A per-run HOME holds exactly one project dir — this run's
    // own clone — so the glob is precise here regardless.
    //
    // Only when this run HAS a private HOME: the stub executor has none (main.ts),
    // which is exactly the "no SDK session to resume" case, so the e2e stub flow is
    // untouched by construction rather than by an executor-kind check here.
    // PRD #88: seed the open question id from the claim. The server re-delivers it
    // from the runs row on every resume, so a worker that picks up a run parked
    // before a death re-parks on the SAME question rather than minting a new id —
    // which is what keeps an answer the user already submitted valid. Without this
    // seeding the identity guard would still be keyed on identity, but the identity
    // itself would change across the requeue, reproducing exactly the silent
    // rejection the clock-based designs were rejected for.
    if (claim.open_question_id)
      this.openQuestionIds.set(runId, claim.open_question_id);

    let sessionId = claim.session_id ?? undefined;
    if (
      sessionId &&
      runHome &&
      !(await sessionTranscriptResolvable(runHome, sessionId, runLog))
    ) {
      sessionId = undefined;
      runLog.warn(
        "resume session transcript is not resolvable here; starting a fresh SDK session",
        {
          run_home: runHome,
          event: RESUME_LINEAGE_BREAK_EVENT,
        },
      );
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          // Says what is true without over-claiming a cause: the usual one is a
          // re-claim by another worker, but the same box loses it too if the cwd
          // changed or the volume was replaced. Both facts the reader needs are
          // stated — the context is gone, AND that is why work may be re-tread.
          text:
            "this run was picked up again, but its earlier session could not be found on this worker — " +
            "continuing WITHOUT its earlier context, so some work may be repeated",
          event: RESUME_LINEAGE_BREAK_EVENT,
        },
      });
    } else if (claim.session_id != null && sessionId != null) {
      // PRD #556 M2 (D5) — the positive resume signal, mutually exclusive with the
      // lineage-break branch above. This fires only when a REAL prior session existed
      // (claim.session_id != null — a resume, not a fresh first attempt and not a
      // seeded run whose claim.session_id is null) AND it RESOLVED on THIS worker's
      // HOME (the preflight above did NOT clear sessionId). Because the guard above
      // requires `runHome` to even attempt resolution, sessionId survives with no
      // runHome (stub executor / e2e) too — but there the resume is a no-op with no
      // transcript to resolve, so guarding on the same runHome condition keeps this
      // silent there, matching the lineage-break guard's intent.
      if (runHome) {
        runLog.info(
          "resume session transcript resolved here; continuing the prior SDK session",
          {
            run_home: runHome,
            event: RESUME_CONTINUED_EVENT,
          },
        );
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: {
            text:
              "this run was picked up again and its earlier session was found on this worker — " +
              "continuing WITH its prior context",
            event: RESUME_CONTINUED_EVENT,
          },
        });
      }
    }
    return sessionId;
  }

  private async phasePreflightHandoff(
    claim: ClaimResponse,
    flight: RunFlight,
    sessionId: string | undefined,
  ): Promise<ExecutorResult> {
    const { runLog, reportState, batcher, steering, cancel, executor } = flight;
    const runId = claim.run_id;
    const runnerClone = flight.runnerClone!;
    const barePath = flight.barePath;
    // Tell the lead whenever the branch it is standing on already carries commits it did
    // not make this turn, so the honest degradation never becomes silently duplicated work.
    // PRD #218 M3 widened the condition to `priorCommits > 0` alone: it previously fired
    // ONLY when the resume was dropped, which missed the case where the session survives but
    // the TREE was recovered from the tracking ref (or is prior pushed work), where the lead
    // equally needs to know its branch is not empty. A fresh run reading its own empty
    // first-attempt branch counts 0 and gets nothing.
    //
    // PRD #209 D7: this note normally reaches the lead in the PLANNING prompt, but a
    // session-less SEEDED run has no planning turn, so it rides the IMPLEMENT prompt instead
    // (threaded on the pre-approved path) — otherwise a requeued seeded run whose transcript
    // was dropped would re-implement cold on a branch that already carries pushed commits,
    // with no prior-work note.
    const priorWork =
      runnerClone.priorCommits > 0
        ? { commits: runnerClone.priorCommits }
        : undefined;

    // PRD #209 (D4): a SEEDED run's plan was authored by the user at create time, so
    // it is approved with NO server-side approve_plan input and — on a fresh seeded
    // run — no SDK session. That is a legitimate "approved, no session" state, NOT the
    // dropped-transcript one the `&& sessionId` guard below protects against: there
    // was never a session to lose. The runner is the layer that can tell the two apart
    // (it holds the preflight result), so it folds `seeded` into planApproved here
    // rather than leaving the executor to read the claim.
    const seeded = claim.plan_source === "seeded";

    // PRD #209 M4 — staleness guard. A seeded run carries the commit the user planned
    // against (claim.planned_base_commit). evaluateBaseStaleness compares it to the
    // clone's resolved base (runnerClone.baseCommit, the same field forwarded into
    // RunContext below): only a seeded run sets planned_base_commit, so an ordinary run
    // yields undefined and proceeds silently. On a divergence it either returns a warning
    // to emit (default) or, under --require-base (claim.require_base_match), THROWS
    // BaseCommitDivergedError BEFORE any implement work — the generic catch-all then
    // fails the run, so it never implements against a diverged base (Open Question 3).
    const staleWarning = evaluateBaseStaleness(
      claim.planned_base_commit ?? undefined,
      runnerClone.baseCommit,
      claim.require_base_match ?? false,
    );
    if (staleWarning) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: staleWarning },
      });
    }

    // PRD #37: parse the checked-out repo's own agent roster and report it on
    // this first post-checkout `running` state report. It rides the STATE report
    // rather than the gate so that an autopilot run — which never parks at
    // awaiting_approval — records what was detected just the same. The roster is
    // inert data: nothing is assembled until a selection picks the repo source.
    const detection = await this.parseRepoAgents(
      runnerClone.path,
      batcher,
      runLog,
    );
    const repoAgents = detection.agents;
    if (detection.ok) {
      // Non-fatal, and fire-and-forget (matching the session-id report below): an
      // INFORMATIONAL roster report must never fail a run. An older API without the
      // repo_agents field 400s DisallowUnknownFields, which the client treats as
      // permanent — awaiting the report here would turn a working run into a failed
      // one over a field the run does not depend on. On a detection FAILURE we send
      // no roster at all, so the column stays NULL ("not reported") rather than `[]`
      // ("scanned, found none") — the two must stay distinguishable.
      void reportState({
        status: "running",
        repo_agents: repoAgentSummaries(repoAgents),
      }).catch((e) =>
        runLog.warn("could not report repo agent roster", {
          error: errMessage(e),
        }),
      );
    }

    // PRD #84 M4: infer the run's requirement set from the checked-out clone with the
    // deterministic scan, ONCE, best-effort. A scan failure must never break a run, so
    // it is caught and yields `undefined` ("emit nothing"). The result rides the same
    // two reports as `milestones` — the CANDIDATE set on the awaiting_approval report and
    // the FROZEN set on the autopilot running report — threaded through gatePlan below.
    let toolchainDetection: ToolchainDetection | undefined;
    try {
      toolchainDetection = await detectToolchain(runnerClone.path);
    } catch (err) {
      runLog.warn("toolchain detection failed; continuing without a requirement set", {
        error: errMessage(err),
      });
    }

    // Cross-run memory (PRD #90): fetch this run's (user, repo) memory so the
    // executor can compose it into the lead's plan prompt as inert, nonce-fenced,
    // untrusted-advisory context. Guarded HARD — a fetch failure (older API, repo-
    // less run 404/409, transport error) or an empty store injects NOTHING and
    // never fails the run: memory is advisory, never load-bearing.
    let memory: Awaited<ReturnType<WorkerClient["getMemory"]>> = [];
    try {
      memory = await this.client.getMemory(runId);
    } catch (err) {
      runLog.warn("could not fetch cross-run memory; continuing without it", {
        error: errMessage(err),
      });
    }

    // PRD #71 M5: did a HUMAN approve this ci_fix plan? On a PRE-APPROVED RESUME the gate
    // does not run this execution, so derive from durable claim state. The server CLEARS
    // auto_approve the moment a run PARKS at the plan gate (SetRunAwaitingApproval,
    // symmetric with the seeded plan_source='agent' decouple), so a resumed run still
    // carrying auto_approve=true was AUTO-approved WITHOUT ever parking (no human), while
    // auto_approve=false means either a manual run or an auto run that parked and was
    // human-approved. A fresh run's gate closure overwrites this precisely.
    flight.ciFixHumanApproved = (claim.auto_approve ?? false) !== true;

    // PRD #759 M4: resume a provably-reviewed approved run without re-plan/re-gate on a
    // dropped-session cross-worker resume. plan_source==='agent' is a POSITIVE allowlist
    // (worker-authored-but-gated; #209 D8 makes plan_source track plan_md provenance) — do
    // NOT write `!== "seeded"`, which fails OPEN on any future unreviewed provenance value.
    // humanApproved is computed FRESH from claim.auto_approve here (NOT reused from the
    // mutable ciFixHumanApproved, which the gate closure reassigns): the server clears
    // auto_approve when a run parks at the plan gate, so auto_approve!==true ⟺ a human saw
    // the gate. recoveryFailed ⟺ the reseed recovered NOTHING: no committed tree AND no WIP
    // snapshot. seededFrom "default" ALONE is not loss — the diverged cross-worker cherry-pick
    // leg recovers the WIP onto the advanced floor with seededFrom "default" + wipRecovered
    // true (ADR-0759; the reseed-feed block above special-cases the same pair as a WIP
    // recovery, not total loss). So wipRecovered is folded in here too — without it a
    // human-approved run whose WIP came back cleanly on the diverged leg would FALSELY re-gate.
    // The re-gate fallback (FLAG D) needs BOTH: an autopilot recovery-failed resume has no
    // human at the gate to protect, but a human-approved recovery-failed run RE-GATES so the
    // human notices the lost tree — #209's loss-detection gate, kept exactly where #209 put it.
    const humanApproved = (claim.auto_approve ?? false) !== true; // false ⟺ a human saw the gate
    const recoveryFailed =
      runnerClone.seededFrom === "default" && runnerClone.wipRecovered !== true; // no tree AND no WIP
    const m4ResumeReviewedPlan =
      (claim.plan_approved ?? false) &&
      claim.plan_source === "agent" &&
      !!claim.plan_md?.trim() &&
      !sessionId &&
      !seeded && // the D4-row-3 dropped-session case
      !(humanApproved && recoveryFailed); // re-gate a human-approved run that lost its tree

    // PRD #1064 M1 (Decision 1): ONE per-run promise chain serializing EVERY `running`
    // report of the run — the immediate `reportProgress` pushes, the turn-boundary
    // `reportIteration`, and the checkpoint report. `reportState` has bounded retries, so a
    // late immediate push (`in_progress: [m2]`) can sit in its retry loop while the next
    // turn's `reportIteration` (`in_progress: []`) is issued; without serialization the two
    // reads could reach the api out of order and resurrect a finished milestone as
    // in-progress. Chaining every report through this single variable makes the reports
    // reach the api in the order they were made. The chain itself NEVER rejects (each link
    // is isolated below) so one failed report cannot break the ordering of later ones.
    let runningReportChain: Promise<unknown> = Promise.resolve();
    const enqueueRunningReport = <T>(task: () => Promise<T>): Promise<T> => {
      // Run `task` after the current tail settles, regardless of its outcome (pass the same
      // handler to both arms — the tail never rejects, but this is belt-and-braces). The
      // returned promise carries THIS task's result (and rejection) for a caller that awaits
      // it; the tail we keep is made non-rejecting so the next enqueue never inherits a
      // rejection from this one.
      const run = runningReportChain.then(task, task);
      runningReportChain = run.then(
        () => undefined,
        () => undefined,
      );
      return run;
    };

    // PRD #122 M6 / issue #1597 M2: the checkpoint BODY, un-gated. ctx.checkpoint (below) runs it
    // under the flight's sink gate; the mid-turn tick calls it DIRECTLY while it already holds the
    // gate via tryAcquire (so it can never self-deadlock), in QUIET mode (no `running` report) with
    // reap:false semantics, its own cancellation signal, and a sub-scope for the 60s secret scan.
    // See the ctx.checkpoint comment for the reap-before-git and best-effort invariants.
    const checkpointBody = async (opts: CheckpointBodyOpts): Promise<CheckpointBodyOutcome> => {
      // `barePath` is the outer `let` (string | undefined); it is set before the run
      // reaches the executor, but narrow it so the closure is honest rather than `!`.
      if (!barePath) return "no_new_work";
      // Decision 6 tip-movement check: has the runner clone's branch tip moved since the
      // last checkpoint wrote the tracking ref? A null trackTip (never checkpointed) or a
      // null cloneTip (unresolvable) is NOT a match, so it falls through to a real fetch.
      const cloneTip = await this.git.branchTip(runnerClone.path, runnerClone.branch);
      const trackTip = await this.git.trackingTip(barePath, runnerClone.branch);
      const tipUnmovedSinceFetch =
        trackTip !== null && cloneTip !== null && trackTip === cloneTip;

      // PRD #267: "new committed work not yet on origin". Depends ONLY on cloneTip (read at
      // the top) and flight.lastPublishedTip, neither of which the fetch-back changes, so it
      // is safe to compute here — before the reap decision — and close over it below.
      const hasNewWork = cloneTip !== null && cloneTip !== flight.lastPublishedTip;

      // PRD #1171 m4: the fetch-back + origin-publish + running-report body, extracted so the
      // reap:true (milestone) path routes it through the Codex reap facade while the reap:false
      // (iteration-boundary) path calls it DIRECTLY (credential-free, no permit). `overlay` is
      // the reap:true `.github/workflows` overlay (undefined off the reaped path and when
      // nothing publishes); its default-tip fetch is a PAT git op, so it is only ever passed on
      // a reaped path (REAP-BEFORE-GIT).
      // issue #1597 M2: the class this body ended in — read by the mid-turn tick (the gated
      // ctx.checkpoint discards it). Default: nothing new to publish.
      let bodyOutcome: CheckpointBodyOutcome = "no_new_work";
      // `inPermit`: this publish runs inside a Codex permit (the reap:true milestone on a Codex run),
      // whose deadline the scan must not push it past (issue #1597 M2 review item 7).
      // `noReport`: skip the running report (the post-permit deferred publish; the milestone's own
      // pass already reported it).
      const doCheckpointPublish = async (
        overlay?: CheckpointOverlayContext,
        inPermit = false,
        noReport = false,
      ): Promise<void> => {
        // issue #1597 M2: a cancelled tick (quiescence, shutdown, a preempting sink) starts nothing.
        if (opts.signal?.aborted) {
          bodyOutcome = "aborted";
          return;
        }
        // Skip ONLY the fetch when there is nothing new to fetch — do NOT return, so the
        // origin-publish gate below still runs (Decision 9: a commit fetched at an earlier
        // iteration can become publish-eligible on a later tip-unmoved iteration).
        if (!tipUnmovedSinceFetch) {
          // Fetch back, credential-free (#218's helper): brings the committed work into
          // refs/uzi-runner/<branch> where the reseed reads it. Best-effort, never fails.
          await this.fetchBackBestEffort(
            barePath,
            runnerClone.path,
            runnerClone.branch,
            runId,
            runLog,
          );
        } else {
          runLog.info("checkpoint fetch skipped: branch tip unmoved since last checkpoint", {
            run_id: runId,
            branch: runnerClone.branch,
          });
        }

        // PRD #1416 M2: on the tip that was just fetched into the bare, detect a history
        // rewrite at/below the published floor P (and floor C) and steer the agent to restore
        // it — never blocks a git command (D2). Read the tip FRESH from the bare tracking ref
        // (refs/uzi-runner/<branch>) since fetchBackBestEffort returns nothing and the
        // top-of-checkpoint trackTip predates this fetch; never the runner clone (that crosses
        // the worker-uid/runner-uid ownership seam branchTip exists to avoid). MID-RUN
        // checkpoint tick only — the finalize/park/capture fetch-backs are M3's territory.
        const fetchedTip = await this.git.trackingTip(barePath, runnerClone.branch);
        await this.maybeSteerOnDivergence(barePath, flight, fetchedTip, batcher, steering, runLog);

        // PRD #1416 M3 (C1): AFTER the steer (which must see the agent's rewritten H) and BEFORE
        // the publish, non-destructively bridge a divergent tracking tip so the checkpoint pack —
        // and therefore a reseed on resume — carries B instead of the rewritten H. Best-effort:
        // a checkpoint must never crash the run (D4), so "failed"/"unknown" only log and continue.
        const bridgeOutcome = await this.bridgeBareTrackingRefIfDivergent(
          barePath,
          runnerClone.branch,
          flight,
          runLog,
        );
        if (bridgeOutcome.kind === "failed" || bridgeOutcome.kind === "unknown") {
          runLog.info("PRD #1416 M3: mid-run checkpoint bridge did not advance the tracking ref", {
            run_id: runId,
            branch: runnerClone.branch,
            outcome: bridgeOutcome.kind,
          });
        }

        // PRD #267: origin-publish gate. The publish is CREDENTIAL-FREE (a pack brokered to the
        // api via publishCheckpoint, no PAT — checkpointPack local objects → client join token)
        // EXCEPT the reap:true `overlay`'s default-tip fetch.
        //   - reap:true  (milestone): publish whenever there is new committed work.
        //   - reap:false (iteration boundary, PRD #267): publish only when the time-gate is
        //     open AND there is new committed work — "new work" keys on lastPublishedTip, NOT
        //     the fetch-skip above, so an idle commit still ships exactly once.
        const timeGateOpen =
          this.checkpointIntervalMs > 0 &&
          this.now() - flight.lastPublish >= this.checkpointIntervalMs;
        let published = false;
        // issue #1597 M2 (round 3): a publish DEFERRED out of a Codex permit (below) is owed: the
        // next overlay-less checkpoint outside a permit publishes regardless of the time gate.
        if (hasNewWork && (opts.reap || timeGateOpen || flight.pendingPublish)) {
          // issue #1597 M2: an overlay-less publish (the mid-turn tick, the iteration boundary, and a
          // milestone whose overlay is undefined) is SCANNED first, over a range PINNED to SHAs, and
          // packs exactly that range. An overlay publish (and every park/shutdown/pause/capture sink,
          // which do not come through here) is byte-unchanged and not scanned.
          if (!overlay && inPermit) {
            // issue #1597 M2 (round 3): NEVER run gitleaks inside a Codex permit. A permit-held child
            // cannot be killed on its own and execScoped's scoped branch ignores `timeout`, while
            // gitleaks' own git-mode `--timeout` yields a silent clean partial scan — so a slow scan
            // could only either push the permit past its deadline (a CodexBoundaryError fails the
            // run) or be trusted wrongly. The fetch-back above already made the milestone durable
            // locally; the remote publish is owed to the next tick OUTSIDE any permit, which is
            // kicked to run soon and publishes regardless of the time gate.
            flight.pendingPublish = true;
            bodyOutcome = "scan_deferred";
            runLog.info("checkpoint publish deferred out of the Codex permit (scan_deferred)", {
              run_id: runId,
              branch: runnerClone.branch,
            });
            flight.kickMidTurnTick?.();
            // Falls through to the running report below (the milestone's progress is reported).
          } else {
            const scanned = overlay
              ? { kind: "unscanned" as const }
              : await this.scanCheckpointForPublish(flight, barePath, runnerClone.branch, opts.scanScope);
            if (opts.signal?.aborted) {
              // Cancelled mid-scan (only the QUIET tick carries a signal, so there is no report to
              // keep): no attempt was made, so the time gate is NOT advanced.
              bodyOutcome = "aborted";
              return;
            }
            if (scanned.kind === "blocked") {
              // Finding or untrusted scan: do NOT publish. The fetch-back above stays (local only,
              // never reported durable); lastPublish advances like any attempt so the next interval
              // retries; lastPublishedTip does NOT advance. Falls through to the running report, so a
              // milestone's progress is still reported.
              flight.lastPublish = this.now();
              // issue #1597 M2 (round 4): a scan+publish ATTEMPT outside a permit settles an owed
              // (deferred) publish — the normal time gate now governs the retry, instead of every
              // tick / iteration boundary bypassing CHECKPOINT_INTERVAL.
              flight.pendingPublish = false;
              bodyOutcome = scanned.outcome;
            } else {
              const outcome = await this.publishCheckpointOutcome(
                flight,
                barePath,
                runnerClone.branch,
                overlay,
                opts.signal,
                scanned.kind === "pinned" ? scanned.range : undefined,
              );
              published = outcome.published;
              bodyOutcome = outcome.published
                ? "published"
                : outcome.reason === "aborted" || opts.signal?.aborted
                  ? "aborted"
                  : `publish_failed:${outcome.reason}`;
              // Advance the time-gate on every ATTEMPT (not just success): bounds broker retry
              // cadence to <= 1 publish/interval/run even under a persistent broker failure. An owed
              // (deferred) publish is settled by the attempt too (issue #1597 M2 round 4), so the gate
              // — not pendingPublish — governs every retry.
              flight.lastPublish = this.now();
              if (bodyOutcome !== "aborted") flight.pendingPublish = false;
              // PRD #267 Fix 1 (Decision 9): advance lastPublishedTip ONLY on a CONFIRMED landed
              // publish, so a transient broker failure leaves hasNewWork true and the time-gate
              // retries the SAME tip at the next interval boundary (bounded loss).
              if (published && scanned.kind === "pinned") {
                // issue #1597 M2 (review item 3): the PINNED path published exactly `range.tipSha`, read
                // from the tracking ref AFTER the fetch-back/bridge — not `cloneTip`, read at the top
                // of this body: the agent may commit (or reset) in between, and recording cloneTip
                // would either republish the same tip next interval or set floor C to a commit that
                // was never published (a false #1416 steer, or a bridge resurrecting a dropped commit).
                // When THIS body bridged, tipSha is the bridge B: C = B (the durable floor) while
                // lastPublishedTip = the fetched H that B wraps (it drives hasNewWork against cloneTip,
                // exactly as the unpinned path keeps H there).
                const tipSha = scanned.range.tipSha;
                const bridgedTip = bridgeOutcome.kind === "bridged" && bridgeOutcome.bridge === tipSha;
                flight.lastPublishedTip = bridgedTip && fetchedTip ? fetchedTip : tipSha;
                flight.checkpointFloor = tipSha;
                this.checkpointTestHooks?.afterPinnedPublish?.({
                  publishedTip: tipSha,
                  lastPublishedTip: flight.lastPublishedTip,
                  checkpointFloor: flight.checkpointFloor,
                });
                if (!opts.reap) {
                  runLog.info("checkpoint published to origin (time-based)", {
                    run_id: runId,
                    branch: runnerClone.branch,
                    tip: tipSha,
                  });
                }
              } else if (published) {
                flight.lastPublishedTip = cloneTip ?? flight.lastPublishedTip;
                // PRD #1416 M3 (C2): advance the checkpoint floor C to the DURABLE published floor on
                // EVERY confirmed publish (PRD line 62). When this tick BRIDGED, C is already B (the
                // helper set it) and cloneTip is the un-bridged H — so DO NOT regress C back to H;
                // otherwise C is the confirmed checkpoint tip cloneTip. lastPublishedTip stays cloneTip
                // (H) above: it drives hasNewWork, a separate concern from the floor.
                flight.checkpointFloor =
                  bridgeOutcome.kind === "bridged"
                    ? bridgeOutcome.bridge
                    : (cloneTip ?? flight.checkpointFloor);
                // PRD #267 M3: make the time-based publish observable, only for the time path so
                // we do not double-log the milestone case.
                if (!opts.reap) {
                  runLog.info("checkpoint published to origin (time-based)", {
                    run_id: runId,
                    branch: runnerClone.branch,
                    tip: cloneTip,
                  });
                }
              }
            }
          }
        } else {
          bodyOutcome = hasNewWork ? "time_gate_closed" : "no_new_work";
        }
        // Report the checkpointed milestone as a `running` report (additive-optional; NO
        // iteration_count so it never regresses the server's GREATEST-merged counter). PRD
        // #267 Fix 2: emit ONLY on real activity (a fetch or a publish); stay silent on a
        // pure-idle checkpoint. issue #1597 M2: the mid-turn tick runs QUIET (no report).
        if (!opts.quiet && !noReport && (!tipUnmovedSinceFetch || published)) {
          // PRD #1064 M1: enqueue onto the per-run chain so this checkpoint report stays
          // ordered behind any pending immediate `reportProgress` push.
          await enqueueRunningReport(() =>
            reportState({
              status: "running",
              ...(opts.progress
                ? {
                    milestones_completed: opts.progress.completed,
                    milestones_in_progress: opts.progress.in_progress,
                    // PRD #1224 M1: project the OPTIONAL per-milestone agent attribution,
                    // omitted when undefined so an old-worker wire shape is preserved.
                    ...(opts.progress.milestones_agents
                      ? { milestones_agents: opts.progress.milestones_agents }
                      : {}),
                  }
                : {}),
            }),
          ).catch((e) =>
            runLog.warn("could not report checkpoint progress", {
              error: errMessage(e),
            }),
          );
        }
      };

      if (opts.reap) {
        // reap:true (milestone). REAP-BEFORE-GIT (Decision 10b, B1/M4 audit): the facade reaps
        // the agent tree — killAgentTree for Claude/stub; withBoundary quiesce+reap (after the
        // per-sink auth-mode reconcile) for Codex — STRICTLY before ANY git below (the
        // credential-free fetch-back AND the #1036 overlay's PAT default-fetch). The overlay is
        // built INSIDE the reaped action (only when it will be used) so its PAT fetch never
        // precedes the reap.
        //
        // CODEX (m4): a cooperative CodexExecutor checkpoint now invokes ctx.checkpoint({reap:true}),
        // so this Codex withBoundary branch IS reached. reapForSink reads executor.safety fresh and a
        // Codex run always sets it, so the per-sink auth-mode reconcile runs and the reap is credentialed
        // (the executor then recreates the reaped provider epoch — see startProviderEpoch). A blocked
        // reconcile (e.g. a transient refresh failure) surfaces a CodexBoundaryError that propagates and
        // fails the run — the intended fail-closed behavior for a credentialed durability boundary.
        await this.reapForSink(
          executor,
          { boundary: "checkpoint", deadlineMs: this.codexBoundaryDeadlineMs },
          async (permit) => {
            const midRunOverlay = hasNewWork
              ? await this.buildCheckpointOverlay(claim, flight, barePath)
              : undefined;
            await doCheckpointPublish(midRunOverlay, permit !== undefined);
          },
        );
        // issue #1597 M2 (round 4): with NO mid-turn ticker running (CHECKPOINT_TICK_INTERVAL=0) a
        // publish deferred out of the Codex permit is made RIGHT HERE, after the permit has ended
        // and before the checkpoint returns — the same scanned path, never inside the permit — so a
        // Codex milestone still publishes like it did before #1597 instead of waiting for the next
        // iteration boundary. (With a ticker running, the kicked tick publishes it.)
        // Skipped once the flight is cancelled (shutdown or a steering cancel aborted flight.cancel
        // while the permit was held): the shutdown sink owns durability from here, and a scan +
        // publish started now would only delay it.
        if (flight.pendingPublish && !flight.kickMidTurnTick && !flight.cancel.signal.aborted) {
          await doCheckpointPublish(undefined, false, true);
        }
      } else {
        // reap:false (iteration boundary): NO reap, NO permit, NO overlay — the agent tree
        // stays ALIVE (a backgrounded dev server survives to the next iteration) and the
        // publish is credential-free, so it is safe with the agent alive. A credential-free
        // fetch-back / join-token publish never mints a permit.
        await doCheckpointPublish(undefined);
      }
      return bodyOutcome;
    };

    const ctx: RunContext = {
      runId,
      kind: resolveRunKind(claim.kind),
      issueIid: claim.issue_iid,
      issueTitle: claim.issue_title,
      issueDescription: claim.issue_description,
      // PRD #381: carry the snapshotted issue-comment set onto the ctx; the SDK
      // executor threads it to buildPlanPrompt for nonce-fenced rendering. Absent/
      // null ⇒ nothing is rendered.
      issueComments: claim.issue_comments,
      // PRD #700 M4: carry the mr_rework run's snapshotted MR review comments onto
      // the ctx, exactly like issueComments; the SDK executor threads it to
      // buildPlanPrompt for nonce-fenced rendering. Absent/null (every non-mr_rework
      // kind) ⇒ nothing is rendered.
      reviewComments: claim.review_comments,
      pipeline: claim.pipeline,
      worktreePath: runnerClone.path,
      branch: runnerClone.branch,
      // PRD #501 REC B: thread the autopilot flag to plan-build time so the plan
      // builders render the no-human-in-the-loop note. Absent ⇒ false.
      autoApprove: claim.auto_approve ?? false,
      // PRD #517 M3: an interactive task run parks at awaiting_followup on signal_done
      // instead of finalizing (see the awaitFollowUp callback below). Absent ⇒ false, so
      // every non-interactive run (and every older server) is byte-identical to today.
      interactive: claim.interactive ?? false,
      // The seed resolved this in the bare; forwarding it is what stops the lead from
      // guessing the branch's parent (judge rec, run 51757591).
      baseCommit: runnerClone.baseCommit,
      defaultBranchCommit: runnerClone.defaultBranchCommit,
      // PRD #1416 M1: the published floor P recorded at claim (flight, restart-surviving),
      // threaded to prompt-build time so both builders name it on a run with a published
      // floor. Absent (fresh branch) ⇒ no note.
      publishedTip: flight.publishedTip,
      emit: (m) => batcher.emit(m),
      // Issue #1583: the claim-secret text redactor, for projections that bound text pre-batcher.
      redactText: flight.redactText,
      oauthToken: claim.secrets.anthropic_oauth_token,
      // PRD #362 M3c: the run-summary model resolved server-side (user-value-wins),
      // and whether the intent summary is already set so the executor skips
      // re-generating it on a resume/re-claim (Decision 3). Both absent on older
      // servers, which the summary hooks tolerate (undefined model → account default,
      // false present → generate).
      summaryModel: claim.summary_model ?? undefined,
      summaryIntentPresent: claim.summary_intent_present ?? false,
      agents: claim.agents,
      repoAgents,
      skills: claim.skills,
      skillsDropped: claim.skills_dropped,
      repoSkillsEnabled: claim.repo.skills_enabled ?? false,
      repoClaudemdEnabled: claim.repo.claudemd_enabled ?? false,
      memory,
      // Issue #297: the self_improve in-flight avoid-set (best-effort; absent ⇒ empty).
      inflightTargets: claim.inflight_targets,
      // PRD #686 D11: the open self-improve MRs' "what was proposed" text (best-effort;
      // absent ⇒ empty, so the picker's non-overlap block is simply omitted).
      openSelfImproveMRs: claim.self_improve_open_mrs,
      // PRD #686 M4: dogfood flag (absent ⇒ generic; older server never sets it).
      selfImproveDogfood: claim.self_improve_dogfood,
      config: claim.config,
      // PRD #1064 M1 (Decision 2): the claim's FROZEN milestone list, the transition-frame
      // title fallback on a pre-approved resume whose loop-scope frozen list is undefined.
      frozenMilestones: claim.milestones,
      // Preflighted above: the claim's id, or undefined when its transcript is not
      // on this worker (issue #105).
      sessionId,
      priorWork,
      // issue #222: this run executed before (it reported a session), so this claim's
      // reseed wiped whatever an earlier attempt left in the tree. Read the RAW
      // claim.session_id, not the `sessionId` var cleared above on a dropped transcript —
      // a dropped-session resume still had its tree destroyed. Same discriminator the
      // reseed feed-status uses (this.emit "starting from the default branch" above).
      resumed: claim.session_id != null,
      // PRD #35 Decision 6b + PRD #209 D4. The RUNNER is the only layer that knows
      // all the facts, which is why it resolves them here rather than the executor
      // reading the claim: the server said the plan is approved, and EITHER a session
      // id arrived that issue #105's transcript check did NOT drop (`sessionId` is
      // cleared above when it did) OR the run is SEEDED. Passing plan_approved through
      // on a dropped-session NON-seeded run would make the executor skip planning for a
      // run whose session is gone — the one case (D4 row 3) where it must re-plan. A
      // seeded run (D4 row 2) is approved with no session by construction, and that is
      // fine because there was never a session to lose.
      planApproved:
        (claim.plan_approved ?? false) &&
        (!!sessionId || seeded || m4ResumeReviewedPlan),
      // PRD #759 M4: a provably-reviewed cross-worker resume skips the re-gate and
      // embeds the persisted plan body on the implement turn. Drives the relaxed
      // preApproved session guard and the embedSeededPlan reviewedResume term.
      reviewedPlanResume: m4ResumeReviewedPlan,
      // PRD #759 M2: the reseed recovered an uncommitted WIP snapshot (a wip(park):
      // marker reset --soft back to the tree), so a cold resumed lead is told to treat
      // the dirty tree as mid-edit and reconcile it against the plan (R1).
      wipRecovered: runnerClone.wipRecovered,
      // PRD #209: the executor relaxes its own session guard for a seeded run and
      // emits the "plan supplied externally" feed line off this.
      seeded,
      approvedPlan: claim.plan_md ?? undefined,
      // The persisted selection, replayed on the claim (PRD #35). Passed through
      // unconditionally rather than gated on planApproved: it is the run's
      // selection whether or not this particular resume skips the gate, and the
      // executor only reads it on the path that has no verdict to supply one.
      approvedSelection: claim.agent_selection,
      signal: cancel.signal,
      // PRD #1190 rework (N2/N1): expose the steering channel's sticky pause/cancel state to the
      // implement loop, because the single shared abort controller (cancel/ctx.signal) fires
      // 'abort' exactly once and cannot deliver a second steering signal. cancelRequested lets the
      // loop honor a cancel that lands after a declined `now`-park (the controller is spent);
      // pauseModeRequested lets it honor a seeded/steered pause at its first boundary independent of
      // the running-report ACK; onPauseNow re-arms the in-flight turn drop for a second `now` pause.
      cancelRequested: () => steering.isCancelled(),
      pauseModeRequested: () => steering.getPauseMode(),
      onPauseNow: (cb) => steering.onPauseNow(cb),
      // PRD #1247 M5b: re-arm the in-flight turn drop for a held-state credential switch (the
      // analog of onPauseNow), and restore the gate phase after a switch resume (D13). Absent
      // resume_phase ⇒ undefined (a fresh run, or an older server), which the executor treats as
      // today's behaviour.
      onCredentialSwitch: (cb) => steering.onCredentialSwitch(cb),
      // PRD #1247 M5b (MAJOR-6): run `fn` with credential-switch trips DEFERRED (the signal held,
      // not tripped) — the executor wraps a plan-REVISION planning turn in this so a switch never
      // releases before gatePlan has persisted the revised plan. Balanced begin/finally-end.
      deferCredentialSwitch: async (fn) => {
        steering.beginCredentialSwitchDefer();
        try {
          return await fn();
        } finally {
          steering.endCredentialSwitchDefer();
        }
      },
      // PRD #1247 M5b (data-integrity fix): attempt the held-state credential switch IN PLACE from
      // wherever the executor was when it tripped (a live implement turn, or an idle gate/question/
      // follow-up waiter), INSTEAD of letting the CredentialSwitchSignal reach the outer catch and
      // end the flight while the run is still healthy (which no sweep requeues → a RUN_TIMEOUT
      // orphan for a running give-up, a strand-until-restart for a held one). Delegates to the SAME
      // two-phase enterCredentialSwitch the outer catch uses. On a GIVE-UP the run CONTINUES on the
      // old token in place, so CLEAR the preserve flags enterCredentialSwitch set up front (its
      // step 2) — a later NORMAL completion must clean up the clone + HOME as usual, not preserve
      // them (D3/D14). On a RELEASE they are already correct (parked=true, preserveRecoveryClone=
      // false) so the finally retires the clone and keeps the HOME for resume. The clear lives HERE,
      // not inside enterCredentialSwitch, because the outer-catch safety net cannot continue in
      // place and must KEEP the flags to leave the run non-terminal for a requeue.
      // issue #1597 M2: gated end to end — from the wip marker captureRecoveryRestorePoint commits to
      // the give-up's undoWipMarker — so a mid-turn tick can never publish that throwaway marker.
      attemptCredentialSwitch: () => this.runGatedSink(flight, async () => {
        const outcome = await this.enterCredentialSwitch(claim, flight, runLog);
        if (outcome === "retained_stop") {
          // The switch could not be confirmed (BLOCKING-2/3 rework). enterCredentialSwitch RETAINED
          // all work (the preserve flags stay set); STOP the flight NON-TERMINAL by throwing to
          // executeClaim's catch chain, rather than continuing in place on a claim the reclaim may
          // already own (release) or whose switch stamp may still be pending (give-up). Do NOT clear
          // the preserve flags or undo the wip marker — the retained work must survive for the reclaim.
          throw new CredentialSwitchRetainedStop();
        }
        if (outcome === "gave_up") {
          flight.preserveRecoveryClone = false;
          flight.preserveSession = false;
          // A DIRTY-tree switch committed a `wip(park):` marker in captureRecoveryRestorePoint; a
          // give-up CONTINUES in place with NO reseed, so the marker must be undone here or it rides
          // into the eventual MR and the restarted turn builds on a throwaway commit — the SAME orphan
          // handlePausePark fixes on the pause-continue path (undoWipMarker's docstring). headIsWipMarker
          // self-guards the blind `reset --mixed HEAD^` so it fires ONLY when HEAD is a marker (a
          // clean-tree switch committed none). Kept HERE, not in enterCredentialSwitch (shared with the
          // outer-catch requeue arm, whose reseed reset-softs the marker) and NOT on the release path
          // (the reclaim's reseed handles it) — the two paths that MUST leave the marker.
          if (flight.worktreePath && (await this.git.headIsWipMarker(flight.worktreePath))) {
            await this.git.undoWipMarker(flight.worktreePath).catch(() => undefined);
          }
        }
        return outcome;
      }),
      resumePhase: claim.resume_phase,
      // Persist the SDK session id the moment the executor learns it, so a
      // re-queued run can resume it. Best-effort.
      onSessionId: (sessionId) => {
        flight.observedSessionId = sessionId;
        void reportState({ status: "running" }).catch((e) =>
          runLog.warn("could not persist session id", {
            error: errMessage(e),
          }),
        );
      },
      // The plan gate: surface the plan, post awaiting_approval, and return the
      // verdict the steering channel resolves (bounded so an abandoned plan
      // fails rather than wedging the worker). An autopilot claim short-circuits
      // to an approve verdict (see gatePlan) — the run never parks at the gate.
      gatePlan: async (planMd, milestones, onAwaitingApproval) => {
        // PRD #71 M5: a CI-config-classified ci_fix plan must NOT take the auto-approve
        // short-circuit — it parks for human review even on an auto-triggered run. We
        // force the gate by passing autoApprove=false for that case; gatePlan is otherwise
        // unchanged. Non-ci_fix and code-plan ci_fix runs keep today's behavior exactly.
        const forceGate = claim.kind === "ci_fix" && isCIConfigPlan(planMd);
        const effectiveAutoApprove = (claim.auto_approve ?? false) && !forceGate;
        const verdict = await this.gatePlan(
          runId,
          planMd,
          milestones,
          batcher,
          steering,
          reportState,
          runLog,
          effectiveAutoApprove,
          repoAgents,
          toolchainDetection,
          // PRD #212: the runner clone path for the gate's runner-uid `git status`.
          // Use runnerClone.path (const, string), NOT the `worktreePath` local
          // (string | undefined — does not narrow in this closure).
          runnerClone.path,
          // PRD #1416 M5: the published floor P for the warn-only plan-gate nudge.
          flight.publishedTip,
          onAwaitingApproval,
        );
        // Human-in-the-loop iff the plan reached an approve verdict via the PARK path
        // (not the auto short-circuit). Read by the pre-push guard below.
        flight.ciFixHumanApproved = verdict.kind === "approve" && !effectiveAutoApprove;
        return verdict;
      },
      // PRD #88 clarification park: surface the question, post awaiting_input, and
      // return the answer the steering channel resolves. An autopilot claim
      // short-circuits to a sentinel answer (see askUser) — such a run never parks.
      askUser: (questions) =>
        this.askUser(
          runId,
          questions,
          batcher,
          steering,
          reportState,
          runLog,
          claim.auto_approve ?? false,
          claim.config ?? null,
        ),
      // Issue #1593: the worker-authored fallback park for a planning turn that stayed
      // prose-only after a nudge. Fixed question, no model text; autopilot never parks.
      askPlanMissing: () =>
        this.askPlanMissing(
          runId,
          batcher,
          steering,
          reportState,
          runLog,
          claim.auto_approve ?? false,
          claim.config ?? null,
        ),
      pullFollowUp: () => steering.pullFollowUp(),
      // PRD #1416 M2: drain the worker-authoritative safety steer the divergence detection
      // (maybeSteerOnDivergence) armed on this same steering channel, in-process. Consumed by
      // both executors at their loop top ahead of the follow-up drain.
      pullSafetySteer: () => steering.pullSafetySteer(),
      // PRD #517 M3: the interactive-task follow-up park. The executor calls this after a
      // clean signal_done on an interactive run (it has already checkpoint-pushed): report
      // awaiting_followup, verify the park took, then BLOCK on the steering channel until
      // the next follow-up (or an idle/cancel end).
      //
      // CONSUME-BEFORE-REPORT ORDERING (the server's SetRunRunning wake guard): the report
      // of `running` that un-parks the run is the loop's NEXT reportIteration, which the
      // executor only reaches AFTER this resolves with a follow-up. steering.awaitFollowUp
      // resolves that follow-up ONLY from the poll loop's post-route service step, i.e.
      // after ConsumeRunInputs stamped consumed_at — so a consumed follow_up input always
      // exists before `running` is reported, and the server admits the wake. This mirrors
      // askUser's settle discipline; the ordering is satisfied by construction here.
      awaitFollowUp: async (idleMs) => {
        // issue #552 M1 (mid-turn wake-guard bug): report `awaiting_followup` — which stamps
        // the open_followup_id watermark — ONLY when the run is genuinely going idle. If a
        // follow-up (or stop/cancel) arrived mid-turn and is already buffered, reporting the
        // park would fold that already-consumed-but-not-yet-applied follow-up INTO the
        // watermark, so its own wake `running` report would then fail the server's
        // `id > watermark` guard and strand a live run at awaiting_followup. Skip the park
        // report in that case and service the buffered outcome directly (the run stays
        // `running`, no spurious park). A follow-up arriving AFTER this point is consumed
        // after the stamp, so its id > watermark and it wakes normally.
        //
        // issue #559 M2: the park report now CARRIES the watermark it wants stamped —
        // `open_followup_id` = the highest follow_up id the worker has already DELIVERED
        // (steering.getLastDeliveredFollowUpId()). The server clamps/floors this instead of
        // deriving MAX(consumed follow_up id) itself. This closes the residual race where a
        // follow-up consumed by the poll loop DURING this report's DB round-trip would fold
        // into a server-derived MAX(consumed) and strand the run: the last-DELIVERED id does
        // NOT advance during the round-trip (the follow-up waiter is armed only AFTER this
        // report returns, below), so the racing follow-up is excluded and its later wake wins.
        if (!steering.hasPendingFollowUpOutcome()) {
          // Read the ACK the same way askUser and the limit park do: the park TOOK only if
          // the server reports `awaiting_followup`. SetRunAwaitingFollowup (M2) matches
          // nothing when the run went terminal under us or is no longer ours (or is not an
          // interactive task) — without this check the worker would block on a follow-up no
          // surface can produce, since the status never changed. Fail loudly instead.
          const ack = await reportState({
            status: "awaiting_followup",
            open_followup_id: steering.getLastDeliveredFollowUpId(),
          });
          const parked = (ack as { status?: string } | undefined)?.status;
          if (parked !== "awaiting_followup") {
            throw new Error(
              `${REASON_FOLLOWUP_NOT_PARKED} (server reports ${parked ?? "an unreadable status"})`,
            );
          }
          runLog.info("interactive task: awaiting follow-up", { run_id: runId });
        } else {
          // issue #559 M3: the SKIP path. A follow-up (or stop/cancel) is already buffered,
          // so we deliberately do NOT report awaiting_followup (that would fold the
          // not-yet-applied follow-up into the watermark — the #558/#552 fix above) and
          // service the buffered outcome directly. But skipping the park report ALSO skips
          // the ACK that, on the non-skip path, caught a mid-turn reclaim or terminal
          // transition (status != awaiting_followup → throw). Restore that ownership/
          // terminality check cheaply with a read-only ownership probe.
          //
          // ONLY a DEFINITIVE answer throws: a terminal status, or a definitive NOT-OWNED
          // (HTTP 404 → reclaimed by another worker). A TRANSIENT error (network / 5xx /
          // anything that is neither a 404 nor a terminal status) logs a warning and
          // PROCEEDS — the non-skip path's reportState has bounded retries and never fails
          // the run on a transient blip, and the run self-heals anyway at the next
          // ACK-checked park report plus the SetRunRunning worker_id pin. We must not
          // introduce a new spurious-failure mode, so a transient probe error is not one.
          let ownershipStatus: string | undefined;
          try {
            ownershipStatus = (await this.client.getRunOwnership(runId)).status;
          } catch (err) {
            if (err instanceof RequestError && err.status === 404) {
              throw new Error(
                `${REASON_FOLLOWUP_NOT_PARKED} (server reports the run is not owned by this worker)`,
              );
            }
            // Transient (network / 5xx / unreadable): proceed and let the next park
            // report's ACK + the SetRunRunning worker_id pin be the backstop.
            runLog.warn(
              "interactive task: ownership probe failed transiently on the follow-up skip path; proceeding",
              { run_id: runId, error: String(err) },
            );
          }
          if (ownershipStatus !== undefined && FOLLOWUP_TERMINAL_STATUSES.has(ownershipStatus)) {
            throw new Error(
              `${REASON_FOLLOWUP_NOT_PARKED} (server reports ${ownershipStatus})`,
            );
          }
        }
        return steering.awaitFollowUp(idleMs);
      },
      // PRD #122 M2: carry the lead's live progress into the `running` report and return
      // the server-computed effective budget from the ack. Async (unlike M4's fire-and-
      // forget void) so the loop can apply the budget — but still fire-and-forget in
      // spirit: reportState has bounded retries, and the try/catch here guarantees a
      // failed report returns undefined ("no budget update") rather than failing the run.
      reportIteration: async (iteration, progress) => {
        try {
          // PRD #1064 M1: enqueue onto the per-run chain so any still-in-flight immediate
          // push (from the prior turn's scan loop) is sent BEFORE this turn-boundary report
          // — awaiting the chain here is what guarantees a pending push is never dropped and
          // never overtakes this snapshot.
          const ack = await enqueueRunningReport(() =>
            reportState({
              status: "running",
              iteration_count: iteration,
              // Omit the fields entirely when the lead has reported no progress, so the
              // wire shape matches an old worker's (additive-optional, never null/[]).
              ...(progress
                ? {
                    milestones_completed: progress.completed,
                    milestones_in_progress: progress.in_progress,
                    // PRD #1224 M1: project the OPTIONAL per-milestone agent attribution,
                    // omitted when undefined so an old-worker wire shape is preserved.
                    ...(progress.milestones_agents
                      ? { milestones_agents: progress.milestones_agents }
                      : {}),
                  }
                : {}),
            }),
          );
          const b: IterationBudget = {};
          if (typeof ack.budgetMaxIterations === "number")
            b.maxIterations = ack.budgetMaxIterations;
          if (typeof ack.budgetWallSeconds === "number")
            b.wallSeconds = ack.budgetWallSeconds;
          // PRD #1189 M1 (D6): carry the served TOTAL wall (frozen budget + extension) off the
          // SAME ACK so the sdk-executor re-arms its hard wall upward when a run is extended
          // while already executing. Kept ALONGSIDE the wallSeconds mapping above (back-compat).
          if (typeof ack.budgetTotalSeconds === "number")
            b.totalWallSeconds = ack.budgetTotalSeconds;
          // PRD #634 M2: carry the operator scope ceiling + fresh completed count off the
          // ACK so m3's loop-top gate can read them off `served`.
          if (typeof ack.scopeCeiling === "number")
            b.scopeCeiling = ack.scopeCeiling;
          if (typeof ack.completedCount === "number")
            b.completedCount = ack.completedCount;
          // PRD #1190 M2: carry the server-decided pause boundary off the SAME ACK so the
          // loop-top pause branch reads it off `served`.
          if (typeof ack.pauseRequested === "boolean")
            b.pauseRequested = ack.pauseRequested;
          // PRD #1226 M4 (D3): carry the server-decided budget_exhausted steer off the SAME ACK,
          // beside pauseRequested, so a live post-attempt interlocked run learns it must enter the
          // completion hold (the loop-top budget-steer branch reads it off `served`).
          if (typeof ack.budgetExhausted === "boolean")
            b.budgetExhausted = ack.budgetExhausted;
          // Without the scope/completed fields in this return guard, a non-budget-scaled
          // run's ACK (no budget fields) would return `undefined` and m3's loop-top gate at
          // `if (served)` would never see the ceiling. This is behavior-preserving for the
          // existing budget logic — sdk-executor.ts's budget block type-checks each field
          // individually, so making `served` truthy more often is inert for budget (it only
          // newly enables m3's scope read).
          return b.maxIterations !== undefined ||
            b.wallSeconds !== undefined ||
            b.totalWallSeconds !== undefined ||
            b.scopeCeiling !== undefined ||
            b.completedCount !== undefined ||
            b.pauseRequested !== undefined ||
            b.budgetExhausted !== undefined
            ? b
            : undefined;
        } catch (e) {
          // PRD #1497 M2: a SERVER-side wall park (recognised by the reportState closure) must NOT be
          // swallowed here — rethrow it so it reaches executeClaim's catch chain, which retains clone
          // + HOME and ends the flight non-terminal. Swallowing it (as with an ordinary transient
          // report error) would let the flight keep running a whole turn on a run the sweep already
          // parked; the fence rejects every write, but the wasted turn is avoided by stopping now.
          if (e instanceof ServerWallParkedError) throw e;
          runLog.warn("could not report iteration", { error: errMessage(e) });
          return undefined;
        }
      },
      // PRD #1064 M1 (Decision 1): push the observed milestone progress to the server the
      // MOMENT the scan loop sees a `report_progress` signal — a `running` report carrying
      // the milestone fields and NO `iteration_count`, exactly the shape the checkpoint
      // closure below already sends mid-run (Resolved facts: `SetRunRunning` only RAISES
      // `iteration_count` via GREATEST and its WHERE guard makes a late push a no-op on a
      // parked/cancelled/finished run, so this is safe).
      //
      // ENQUEUE-AND-RETURN: the report is chained onto the per-run running-report chain (so
      // it serializes with the turn-boundary `reportIteration` and the checkpoint report)
      // and this returns IMMEDIATELY — the scan loop must never block on a network report.
      // A failure is logged and swallowed; an informational field never fails a run.
      reportProgress: (progress) => {
        void enqueueRunningReport(() =>
          reportState({
            status: "running",
            milestones_completed: progress.completed,
            milestones_in_progress: progress.in_progress,
            // PRD #1224 M1: project the OPTIONAL per-milestone agent attribution, omitted
            // when undefined so an old-worker wire shape is preserved.
            ...(progress.milestones_agents
              ? { milestones_agents: progress.milestones_agents }
              : {}),
          }),
        ).catch((e) =>
          runLog.warn("could not report progress", { error: errMessage(e) }),
        );
        return Promise.resolve();
      },
      // PRD #122 M6: durably checkpoint the run's committed milestone work MID-RUN
      // (Decisions 6, 7, 10, 10b). It is the SAME credential-free fetch-back the done and
      // park paths use (#218's fetchAgentBranch), fired at a milestone boundary so a hard
      // crash loses at most "since the last milestone" rather than the whole run.
      //
      // REAP-BEFORE-GIT is the load-bearing invariant (B1/M4 audit): when reaping, the
      // agent tree is killed BEFORE any CREDENTIALED git runs, so a survivor cannot read a
      // credential out of a git child's /proc/environ — the same ordering the done path
      // uses (killAgentTree before fetchBackBestEffort). The pre-reap tip reads below
      // (branchTip/trackingTip) are credential-free local rev-parse, and the only
      // credential-bearing-CLASS op here is the fetch-back, itself credential-free
      // (file://, no PAT) — so a future credentialed git op MUST stay after the reap.
      // Best-effort throughout: a checkpoint must NEVER fail the run.
      checkpoint: (opts) =>
        // issue #1597 M2: gated — waits for (and preempts) an in-flight mid-turn tick.
        this.runGatedSink(
          flight,
          async () => {
            await checkpointBody({ reap: opts.reap, progress: opts.progress });
          },
          () => undefined, // best-effort: the next boundary retries
        ),
      // Issue #281: a cheap fingerprint of the runner clone's committed + working-tree
      // state for the executor's no-progress detector — the runner-owned clone's branch
      // tip (committed work) plus `git status --porcelain` (uncommitted changes). Both are
      // runner-uid reads of the runner-owned clone (branchTip / worktreeStatus), the same
      // reads the checkpoint closure and the plan gate already do. Returns null when EITHER
      // read fails — an unresolvable tip OR an unreadable status — which the executor treats
      // as "cannot assert unchanged" (no trip). worktreeStatus (not planChangedFiles) is used
      // deliberately: planChangedFiles swallows a failed read to [], which the fingerprint
      // would encode identically to a genuinely clean tree, so a persistently failing status
      // could let the detector trip without the tree ever having been verified (CodeRabbit #655).
      worktreeFingerprint: async () => {
        if (!barePath) return null;
        const tip = await this.git.branchTip(runnerClone.path, runnerClone.branch);
        if (tip === null) return null;
        const dirty = await this.git.worktreeStatus(runnerClone.path);
        if (dirty === null) return null;
        return `${tip}\n${dirty.join("\n")}`;
      },
      // PRD #1190 M2: park the run on an owner-requested pause. Delegates to handlePausePark,
      // which publishes a checkpoint FIRST and reports `paused` only if it lands (Decision 8),
      // returning whether the run parked. Called from the implement loop's pause boundary and
      // its `now`-pause turn catch.
      // issue #1597 M2: gated — a tick can never fetch/publish the wip marker this path commits.
      parkForPause: (pausedAt) => this.runGatedSink(flight, () => this.handlePausePark(claim, flight, pausedAt)),
      // PRD #1497 M2 (D4): park the run at its WALL-CLOCK limit — the CAPTURE-FIRST wall park,
      // NOT handlePausePark. Delegates to enterWallPark, which reaps, captures a verified restore
      // point via the SHARED captureHoldContext, reports the wall_park transition, and returns the
      // outcome the executor branches on (parked/undeliverable/refused/cancelled). Called from the
      // implement loop's wall-pause turn catch, its pre-attempt REASON_WALL arm, and the loop-top
      // wall boundary.
      // issue #1597 M2: gated against the mid-turn tick (it reaps and captures a restore point).
      parkForWall: () => this.runGatedSink(flight, () => this.enterWallPark(flight, claim, runLog)),
      // Issue #1600: hand the executor the budget a refused wall park carried, once.
      takeWallParkRefresh: () => {
        const refresh = flight.wallParkRefresh;
        flight.wallParkRefresh = undefined;
        return refresh;
      },
      // PRD #1497 M2: clear a sticky `wall` pause mode after a REFUSED wall_park (the owner extended
      // in the window, so the run continues) — else the next loop boundary would re-route to the
      // wall seam. Delegates to the steering channel; a no-op when no wall is pending.
      clearWallMode: () => steering.clearWallMode(),
      // PRD #1226 M3 (D1/D2): this run is INTERLOCKED when the claim's WORKER-ONLY
      // completion_contract_version is non-null. The executor then runs the structural completion
      // protocol on signal_done instead of finalizing directly. false/absent (legacy run, rollout
      // OFF, or an older server that never sends the key) ⇒ the legacy finalize path, unchanged.
      completionInterlock: claim.config?.completion_contract_version != null,
      // PRD #1226 M3 (D3): the same-session structural completion attempt. Forwards the executor's
      // declaration + head + worktree fingerprint to the worker completion/attempt endpoint, which
      // union-merges the declaration into milestones_completed, recomputes unmet server-side, and
      // records a bounded attempt — returning the server-authoritative unmet set + attempt count.
      recordCompletionAttempt: ({ declared, head, worktreeFingerprint }) =>
        this.client.recordCompletionAttempt(runId, {
          milestonesCompleted: declared,
          head,
          worktreeFingerprint,
          // PRD #1247 M5: stamp the claim-lane generation (the SAME value the reportState closure
          // stamps) so a released/superseded stale flight's attempt records nothing server-side.
          claimGeneration: flight.claimGeneration,
        }),
      // PRD #1226 M4 (D3/D6): the recoverable completion-hold seam is now WIRED. On a repeated
      // no-progress completion attempt, a post-attempt budget/stall/wall/idle exhaustion, or the
      // server's budget_exhausted steer, the executor calls this INSTEAD of throwing a terminal
      // failure. enterCompletionHold reaps, captures a VERIFIED same-worker restore point, requests
      // the hold, and returns true ONLY on a `paused` ACK (parked; the finally preserves the clone
      // and HOME); false means it did NOT park (it cleared its preserve flags), so the executor falls
      // back to the legacy throw and the run's normal terminal cleanup runs. The feature is
      // rollout-OFF (completion_interlock_rollout defaults OFF)
      // until #1232, so this seam is inert in production — completionInterlock above is false.
      // issue #1597 M2: gated against the mid-turn tick (it reaps and captures a restore point).
      enterCompletionHold: (reason) =>
        this.runGatedSink(flight, () => this.enterCompletionHold(flight, claim, reason, runLog)),
      // PRD #1226 M5 (D6): the completion-question LIVE window, wired as a SIBLING to
      // enterCompletionHold. The executor calls THIS at the completion-STALL point (STALL_LIMIT
      // identical no-progress completion attempts) INSTEAD of parking straight away — it authors an
      // awaiting_input question (marked completion_question so the api stamps completion_question_at
      // and the owner continue-decision endpoint resolves it) and gives the owner a live window
      // (completion_hold_window_seconds, default 900s) to continue-with-guidance before the run
      // parks. On expiry (or a park the server won't ACK) it resolves "expired" and the executor
      // routes to enterCompletionHold (M4) — it NEVER throws a timeout. Inert while the interlock is
      // rollout-OFF (completionInterlock above is false, so the stall path is never reached in
      // production). Sources its window from claim.config, exactly like askUser sources its deadline.
      askCompletionQuestion: (unmet) =>
        this.askCompletionQuestion(
          runId,
          unmet,
          batcher,
          steering,
          reportState,
          runLog,
          claim.config ?? null,
        ),
    };

    // PRD #1064 M1: drain the per-run running-report chain before this phase yields control,
    // so any still-in-flight immediate `reportProgress` push reaches the api BEFORE the
    // terminal report. A final SDK frame can carry both `report_progress` and `signal_done`:
    // the scan loop fires reportProgress FIRE-AND-FORGET (not awaited by the executor), so
    // without this drain the terminal report can land first and the queued `running` push then
    // no-ops against the finished run (SetRunRunning's WHERE guard), losing a milestone
    // completion reported only via report_progress. The chain never rejects (each link is
    // isolated) and each `reportState` has bounded retries, so this await is bounded and cannot
    // reject — it only enforces the ordering D1 already promises.
    //
    // It runs in a `finally` so BOTH terminal paths are covered: on success phasePublish sends
    // `completed` next; on a reject the catch in execute() sends `failed` (or takes the
    // park/requeue branch). Draining on the reject path keeps a late push from no-oping against
    // that terminal report too — the original success-only drain left this leg exposed.
    //
    // issue #1597 M2: the MID-TURN checkpoint tick runs only while executor.run is in flight. Its
    // stop() is awaited FIRST in the finally: it cancels the timer, aborts an in-flight tick and
    // waits for FULL settlement (every tick child exited, the sink gate released, the publish
    // rejected, lock custody done) — no Promise.race abandonment — before the report chain drains
    // and before anything after this (the killAgentTree reap, finalize, or the catch's park /
    // shutdown sinks) can touch the clone or the bare. A shutdown aborts flight.cancel, which the
    // tick is linked to, so the same settlement happens before the shutdown branch runs.
    const ticker = barePath
      ? this.startMidTurnTicker(flight, barePath, runnerClone.path, runnerClone.branch, (tickOpts) =>
          checkpointBody({ reap: false, quiet: true, ...tickOpts }),
        )
      : undefined;
    let result: ExecutorResult;
    try {
      result = await executor.run(ctx);
    } finally {
      await ticker?.stop();
      await runningReportChain;
    }

    // Reap any agent-backgrounded subprocess BEFORE the PAT touches a git child
    // env — otherwise a survivor could read the PAT from that child's
    // /proc/environ during the push (M4 audit B1). This run's executor reaps only
    // this run's subprocess tree (per-run instance, Decision 4); a concurrent
    // sibling's tree is untouched. The SDK executor also self-reaps in its run()
    // finally; this is the explicit, load-bearing call at the security boundary.
    executor.killAgentTree?.();
    flight.result = result;
    return result;
  }


  /**
   * PRD #218 M1 — trigger the graceful worker shutdown. SYNCHRONOUS and does NO git:
   * it only flips the global flag (so a run claimed during the drain aborts on
   * registration) and aborts every in-flight run's controller, marking each so its
   * catch takes the fetch-back-and-requeue branch. The fetch-backs themselves run
   * inside each `execute()` as it unwinds — deliberately NOT here, because the caller
   * is a signal handler and `controller.abort()` (main.ts) races the async reap chain;
   * a git fetch started here could read a runner-owned clone the run() unwind is still
   * tearing down (R5). The worker's claim-loop drain (`Promise.allSettled(active)`)
   * then waits for those unwinding executes, and process exit is gated on it, so the
   * fetch-backs complete inside the container's termination grace.
   */
  shutdown(): void {
    this.shuttingDownGlobal = true;
    for (const a of this.activeRuns.values()) {
      a.shuttingDown = true;
      a.cancel.abort();
    }
  }

  /**
   * PRD #218 M1 — fetch the agent branch back into the worker bare, best-effort. The
   * park and shutdown paths share this: both need the interrupted attempt's committed
   * work in `refs/uzi-runner/<branch>` (where M2's reseed reads it) before the clone is
   * removed or the container dies. It is the SAME hardened primitive the done path
   * runs, at a different time — no new trust-boundary crossing. A failure is swallowed
   * with a warn (D4): a fetch-back that fails must not undo the park or block the
   * requeue.
   */
  // ─── PRD #1171 m4: Codex durability-sink boundary helpers ──────────────────────
  /**
   * Route a durability/publication sink through the Codex `withBoundary` reap facade when the
   * executor is Codex-selected (`executor.safety`), else take the LITERAL legacy
   * `killAgentTree` reap branch (Claude/stub — unchanged ordering, cleanup and errors).
   *
   * The runner is HARNESS-AGNOSTIC: it branches ONLY on `!!executor.safety`, never reads
   * authMode/credentials and never imports agent/src/codex/**. Inside a Codex permit it scopes
   * GitCache to `spawnBoundaryProcess`, so every git/gitleaks/pack child reserves and settles a
   * registry-owned boundary-action root rather than merely running beside a held permit.
   * All auth-mode reconciliation lives inside the executor-owned reconcile closure the facade
   * runs BEFORE the reap. A `CodexBoundaryError` (sink counter zero, publication blocked)
   * PROPAGATES to the caller, which decides whether it is a best-effort publish failure or a
   * failed-run report.
   */
  private async reapForSink(
    executor: Executor,
    req: BoundaryRequest,
    action: (permit?: BoundaryPermit) => Promise<void>,
  ): Promise<void> {
    const safety = executor.safety;
    if (safety) {
      await safety.withBoundary(req, async (permit) => {
        await this.git.withBoundaryProcessSpawner(
          (process) => safety.spawnBoundaryProcess(permit, process),
          permit.signal,
          () => action(permit),
        );
      });
    } else {
      executor.killAgentTree?.();
      await action(undefined);
    }
  }

  /**
   * Like {@link reapForSink} but the LEGACY branch does NOT reap — the caller's untouched
   * `killAgentTree` site already reaped (the security-boundary reap for finalize, or
   * `handleRecoveryExhausted`'s reap for recovery). For a Codex run the whole action runs under
   * the held permit (its quiesce+reap closes admission before the credentialed publish).
   */
  private async withCodexBoundaryOnly(
    executor: Executor,
    req: BoundaryRequest,
    action: (permit?: BoundaryPermit) => Promise<void>,
  ): Promise<void> {
    const safety = executor.safety;
    if (safety) {
      await safety.withBoundary(req, async (permit) => {
        await this.git.withBoundaryProcessSpawner(
          (process) => safety.spawnBoundaryProcess(permit, process),
          permit.signal,
          () => action(permit),
        );
      });
    } else {
      await action(undefined); // legacy already reaped at its untouched killAgentTree site
    }
  }

  /**
   * PRD #1030 M4: bound a best-effort promise by a client-side budget, resolving to
   * `undefined` when the budget elapses first (never rejecting). Used to cap the
   * graceful-shutdown durability sequence so a slow/unreachable forge cannot hang the
   * shutdown past the k8s termination grace — see the budget note at the shutdown branch.
   * The timer is armed through `this.setTimer` (unref'd) so a pending budget never keeps
   * the event loop alive, and is cancelled the moment `work` settles so a completed
   * publish leaves no dangling timer.
   */
  private async raceShutdownBudget<T>(
    work: Promise<T>,
    budgetMs: number,
  ): Promise<T | undefined> {
    let cancelTimer: (() => void) | undefined;
    const timeout = new Promise<undefined>((resolve) => {
      cancelTimer = this.setTimer(() => resolve(undefined), budgetMs);
    });
    try {
      return await Promise.race([work, timeout]);
    } finally {
      cancelTimer?.();
    }
  }

  private async fetchBackBestEffort(
    barePath: string,
    worktreePath: string,
    branch: string,
    runId: string,
    runLog: Logger,
  ): Promise<void> {
    await this.git.fetchAgentBranch(barePath, worktreePath, branch, runId).catch((e) =>
      runLog.warn("fetch-back on interruption failed; work may not be recoverable", {
        error: errMessage(e),
      }),
    );
  }

  /**
   * PRD #1416 M2: at the mid-run checkpoint fetch-back, detect whether the branch's history was
   * rewritten at/below a published/checkpoint FLOOR and, if so, steer the agent to restore it —
   * once per distinct fetched tip. It NEVER blocks a git command (D2): it only emits ONE `status`
   * run message and arms ONE worker-authoritative steer that the next implement turn consumes.
   * The finalize bridge (M3) is the correctness backstop; this mid-run steer is prevention and
   * bounds detection at CHECKPOINT_INTERVAL (SC1).
   *
   * Dedup discipline (what makes the tests' "exactly one status and one steer" hold):
   *  - null P (no published floor, fact 17) or a falsy tip → return (nothing to compare).
   *  - test P, and C when it is set and differs from P, with {@link Git.ancestry}; the FIRST
   *    "divergent" is enough — break so a single rewrite never emits twice. "unknown" (a git
   *    error / missing ref) and "ancestor" are NOT divergent, so a transient read never steers.
   *  - dedup by the FETCHED TIP (`flight.steeredTips`): a repeated tick re-fetching the SAME
   *    diverged tip emits nothing; a FURTHER rewrite (a new tip) is a new key and steers again.
   *
   * Called from the mid-run checkpoint fetch-back ONLY (M2 scope); the finalize/park/capture
   * fetch-backs are M3's territory, which adds the ancestry check and the bridge there together.
   */
  private async maybeSteerOnDivergence(
    barePath: string,
    flight: RunFlight,
    tip: string | null,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    runLog: Logger,
  ): Promise<void> {
    const publishedTip = flight.publishedTip;
    if (!publishedTip) return; // no published floor → nothing to have rewritten below (fact 17)
    if (!tip) return; // no fetched tip to compare against
    const floors = [publishedTip];
    if (flight.checkpointFloor && flight.checkpointFloor !== publishedTip) {
      floors.push(flight.checkpointFloor);
    }
    let divergent = false;
    for (const floor of floors) {
      if ((await this.git.ancestry(barePath, floor, tip)) === "divergent") {
        divergent = true;
        break; // one divergent floor is enough — do not emit twice
      }
    }
    if (!divergent) return;
    // Dedup by the fetched tip: at most one status + steer per distinct diverged tip.
    const steered = (flight.steeredTips ??= new Set<string>());
    if (steered.has(tip)) return; // repeated tick, same tip → nothing new
    steered.add(tip);
    batcher.emit({
      kind: "status",
      agent: "worker",
      payload: {
        text: `the branch's history was rewritten below its published tip ${publishedTip.slice(0, 12)}; uzi pushes fast-forward only; steering the agent to restore it`,
      },
    });
    steering.pushSafetySteer(composeSafetySteer(publishedTip, tip));
    runLog.info("PRD #1416 M2: divergence below published floor detected; armed safety steer", {
      run_id: flight.runId,
      published_tip: publishedTip,
      tip,
    });
  }

  /**
   * PRD #1416 M3 — at a PUBLICATION BOUNDARY (finalize push, park, release, capture), non-
   * destructively repair a divergent bare tracking tip H by wrapping it in a synthesised bridge
   * commit B so a rewritten branch FAST-FORWARDS from its published floor P (and checkpoint floor
   * C) WITHOUT a force-push (D4). Reads H from the WORKER BARE tracking ref (worker-uid, via
   * {@link Git.trackingTip}) — NEVER the runner clone (that crosses the ownership seam branchTip
   * avoids). Advances the tracking ref to B and C to B on success, so everything a caller then
   * captures / releases / aligns / pushes off the tracking ref carries B.
   *
   * Outcomes (never thrown for control flow — a git failure inside maps to "failed"/"unknown"):
   *  - "clean"   — no floor to bridge (P null, or P and C are already ancestors of H). Nothing done.
   *  - "unknown" — ancestry could not be determined (a broken read); do NOT bridge, do NOT fail.
   *  - "bridged" — B built AND validated (tree === H's tree, P and H both ancestors of B); the bare
   *                tracking ref was advanced to B and C set to B.
   *  - "failed"  — H is divergent but B could not be built or validated; the ref is left at H.
   */
  private async bridgeBareTrackingRefIfDivergent(
    barePath: string,
    branch: string,
    flight: RunFlight,
    runLog: Logger,
  ): Promise<BridgeOutcome> {
    const publishedTip = flight.publishedTip;
    if (!publishedTip) return { kind: "clean" }; // no published floor (fact 17) — nothing to bridge
    // Read H from the WORKER-owned bare tracking ref; null ⇒ nothing published yet on this seam.
    const H = await this.git.trackingTip(barePath, branch);
    if (!H) return { kind: "clean" };
    // Ancestry of P and of C (when C is set and differs from P) against H, tri-state.
    const floors = [publishedTip];
    if (flight.checkpointFloor && flight.checkpointFloor !== publishedTip) {
      floors.push(flight.checkpointFloor);
    }
    let anyDivergent = false;
    let anyUnknown = false;
    for (const floor of floors) {
      const rel = await this.git.ancestry(barePath, floor, H);
      if (rel === "divergent") anyDivergent = true;
      else if (rel === "unknown") anyUnknown = true;
    }
    if (!anyDivergent) {
      // All ancestor, OR a mix of ancestor+unknown (no divergent): a broken read must NEVER bridge.
      return anyUnknown ? { kind: "unknown" } : { kind: "clean" };
    }
    // Build B over the floors (bridgeToFloors appends only the ones actually missing).
    const bridgeResult = await this.git.bridgeToFloors(barePath, H, floors);
    if (bridgeResult.kind === "noop") {
      // #1416 (MR-rework, finding 4): nothing was actually missing — H already covers every floor (a
      // fast-forward). The caller's divergence read raced ahead of bridgeToFloors' recheck. Treat as
      // clean so a fast-forwardable run is NOT failed as history_rewritten.
      return { kind: "clean" };
    }
    if (bridgeResult.kind === "failed") {
      runLog.warn("PRD #1416 M3: divergence below published floor but the bridge could not be built", {
        run_id: flight.runId,
        published_tip: publishedTip,
        tip: H,
      });
      return { kind: "failed" };
    }
    const bridge = bridgeResult.sha;
    // VALIDATE B before adopting it: tree byte-equal to H's, and P and H both ancestors of B.
    // #1416 M3 — distinguish a TRANSIENT/UNKNOWN read from a DEFINITIVE validation failure. A
    // revParse that returns null (a broken/transient read) or an `ancestry` that returns "unknown"
    // is NOT proof that B is malformed; at the finalize sinks a "failed" throws HistoryRewrittenError
    // and fails the whole run, so a transient read must NOT hard-fail it. Only a DEFINITIVELY
    // malformed B fails: its tree RESOLVES and differs from H's, OR `ancestry` DEFINITIVELY reports
    // "divergent" (P or H provably NOT an ancestor of B). A transient/unknown read → "unknown"
    // (best-effort: do not adopt B, do not throw at finalize).
    const bridgeTree = await this.git.revParse(barePath, `${bridge}^{tree}`);
    const hTree = await this.git.revParse(barePath, `${H}^{tree}`);
    const pRel = await this.git.ancestry(barePath, publishedTip, bridge);
    const hRel = await this.git.ancestry(barePath, H, bridge);
    // A tree read that could not resolve is transient; a resolved-but-different tree is definitive.
    const treeUnknown = bridgeTree === null || hTree === null;
    const treeMismatch = !treeUnknown && bridgeTree !== hTree;
    const definitiveFail = treeMismatch || pRel === "divergent" || hRel === "divergent";
    const transientUnknown = treeUnknown || pRel === "unknown" || hRel === "unknown";
    if (definitiveFail) {
      runLog.warn("PRD #1416 M3: bridge is definitively malformed; NOT adopting it", {
        run_id: flight.runId,
        published_tip: publishedTip,
        tip: H,
        bridge,
        tree_mismatch: treeMismatch,
        p_ancestry: pRel,
        h_ancestry: hRel,
      });
      return { kind: "failed" };
    }
    if (transientUnknown) {
      // A broken/transient read during validation: do NOT adopt B, but do NOT fail the run either
      // (the finalize sinks would throw). Best-effort — the caller leaves the tracking ref at H.
      runLog.warn("PRD #1416 M3: bridge validation read was transient/unknown; NOT adopting it (best-effort)", {
        run_id: flight.runId,
        published_tip: publishedTip,
        tip: H,
        bridge,
        tree_unknown: treeUnknown,
        p_ancestry: pRel,
        h_ancestry: hRel,
      });
      return { kind: "unknown" };
    }
    // Adopt B: advance the bare tracking ref (worker-uid) and the checkpoint floor C.
    try {
      await this.git.updateTrackingRef(barePath, branch, bridge);
    } catch (e) {
      runLog.warn("PRD #1416 M3: could not advance the tracking ref to the bridge", {
        run_id: flight.runId,
        bridge,
        error: errMessage(e),
      });
      return { kind: "failed" };
    }
    flight.checkpointFloor = bridge;
    runLog.info("PRD #1416 M3: history rewritten below the published floor; bridged and advanced the tracking ref", {
      run_id: flight.runId,
      published_tip: publishedTip,
      tip: H,
      bridge,
    });
    return { kind: "bridged", bridge };
  }

  /**
   * PRD #1416 M3 — the BEST-EFFORT bridge wrapper for the park / shutdown / capture sinks (C5-C8).
   * Unlike the finalize sinks (C3/C4), which throw {@link HistoryRewrittenError} on a "failed"
   * bridge, these boundaries must NEVER throw: a park/shutdown/capture that fails is worse than one
   * that loses a bridge (D4). It runs the bridge, logs a "failed"/"unknown" outcome, and returns —
   * the caller then captures/publishes whatever the tracking ref points at (B on success, the
   * un-bridged H otherwise). Swallows any thrown error too, so nothing here can undo a park.
   */
  private async bridgeParkSinkBestEffort(
    barePath: string,
    branch: string,
    flight: RunFlight,
    runLog: Logger,
    sink: string,
  ): Promise<void> {
    try {
      const o = await this.bridgeBareTrackingRefIfDivergent(barePath, branch, flight, runLog);
      if (o.kind === "failed" || o.kind === "unknown") {
        runLog.info("PRD #1416 M3: park/capture bridge did not advance the tracking ref", {
          run_id: flight.runId,
          branch,
          sink,
          outcome: o.kind,
        });
      }
    } catch (e) {
      // Never let a bridge failure undo a park/shutdown/capture (D4).
      runLog.warn("PRD #1416 M3: park/capture bridge threw; continuing best-effort", {
        run_id: flight.runId,
        branch,
        sink,
        error: errMessage(e),
      });
    }
  }

  /**
   * PRD #122 M8 — broker the checkpoint pack to origin, best-effort. Mirrors
   * fetchBackBestEffort for symmetry: compute the delta pack of
   * `<origin|default>..refs/uzi-runner/<branch>` (null ⇒ nothing to publish) and ship it
   * to the api's publish RPC, which lands it at `refs/uzi-checkpoints/<branch>` for another
   * worker to recover cross-worker. Fired on BOTH the milestone (reap:true) checkpoint and
   * the PRD #267 time-gated (reap:false) checkpoint, always AFTER the fetch-back updated the
   * tracking ref checkpointPack reads. Every non-success — a null pack (nothing to publish),
   * a non-2xx ({@link PublishResult} `ok: false`), a best-effort skip, or a thrown error — is
   * swallowed (never fails the run) but, since issue #1030, is also SURFACED: a non-2xx,
   * skip, or thrown error emits a deduped run-feed line (see {@link reportPublishOutcome}); a
   * null pack stays silent because there was genuinely nothing to publish.
   *
   * Returns `true` IFF the publish confirmably LANDED (a 2xx whose body reports
   * `published: true`), so the caller can advance `lastPublishedTip` only on confirmed
   * success (PRD #267 Fix 1): a swallowed failure returns `false`, leaving the tip
   * un-advanced so `hasNewWork` stays true and the time-gate retries at the next interval
   * boundary.
   *
   * issue #1030: a failure or a best-effort SKIP is no longer swallowed silently. A non-2xx
   * (HTTP <code>), a 2xx `{ published: false, skipped: <reason> }`, or a thrown error each
   * emits a `status` line onto the run feed and a `runLog.warn`, deduped per distinct
   * outcome per run (see {@link reportPublishOutcome}). A null pack (nothing to publish —
   * an absent tracking ref or an unmoved tip) is NOT a failure and stays silent.
   */
  /**
   * PRD #1062 M2 (#1036): build the `.github/workflows` overlay context for a checkpoint publish,
   * or undefined when no overlay must be attempted. Undefined for every non-GitHub forge (the
   * overlay closes a GitHub-only `workflow` scope gap) — so `checkpointPack` behaves exactly as
   * today off GitHub. `defaultBranch` is resolved the way the finalize align resolves it. Callers
   * pass the result ONLY on paths where the agent tree is already reaped: the overlay's default
   * fetch is a PAT git op, forbidden while the agent is alive (the REAP-BEFORE-GIT invariant).
   */
  private async buildCheckpointOverlay(
    claim: ClaimResponse,
    flight: RunFlight,
    barePath: string,
  ): Promise<CheckpointOverlayContext | undefined> {
    if (claim.repo.forge_type !== "github") return undefined;
    const defaultBranch =
      claim.repo.default_branch?.trim() ||
      (await this.git.defaultBranchName(barePath)) ||
      "main";
    return {
      defaultBranch,
      pat: claim.secrets.forge_pat,
      cloneUrl: claim.repo.clone_url,
      username: claim.secrets.forge_username,
      // issue #1086 (F2): chain from the ATTEMPTED tip when one is pending (an ambiguous prior
      // publish), else the CONFIRMED tip, so the next overlay descends the broker's actual ref
      // whether or not the prior push landed.
      prevCheckpointTip: flight.lastAttemptedCheckpointRefTip ?? flight.lastCheckpointRefTip,
    };
  }

  /** issue #1597 M2: the checkpoint secret-scan deadline (a test may shorten it). */
  private scanDeadlineMs(): number {
    return this.checkpointTestHooks?.scanDeadlineMs ?? CHECKPOINT_SCAN_TIMEOUT_MS;
  }

  /**
   * issue #1597 M2 (review item 8) — does a lock file a cancelled tick RETAINED still exist? The
   * retention is sticky only while the file is there: a sibling run's momentary packed-refs.lock (or
   * one its owner later removes) must not latch the sink shut for the rest of the run. Paths that
   * are gone are dropped (and the "sink blocked" feed line may be emitted again for a later
   * episode). lstat only; any error other than ENOENT keeps the entry (cannot prove it is gone).
   */
  /** issue #1597 M2: true while a tick process group that outlived its SIGKILL is still alive (the
   *  probe fails safe: unknown = alive). Gone groups are dropped, with one "unblocked" log line. */
  private tickSurvivorsRemain(flight: RunFlight): boolean {
    if (flight.survivingTickGroups.length === 0) return false;
    const probe = (g: SurvivingGroup): boolean =>
      this.checkpointTestHooks?.tickSpawn?.groupAlive?.(g.pgid) ?? processGroupAlive(g.pgid, g.identity);
    const alive = flight.survivingTickGroups.filter(probe);
    if (alive.length === 0) {
      flight.runLog.info("mid-turn checkpoint sink unblocked: the surviving tick process group(s) are gone", {
        run_id: flight.runId,
        pgids: flight.survivingTickGroups.map((g) => g.pgid),
      });
    }
    flight.survivingTickGroups = alive;
    return alive.length > 0;
  }

  /**
   * issue #1597 M2: before a durable sink touches the clone or the bare, wait (bounded) for any
   * surviving tick process group to go, re-sending a CHECKED SIGKILL once. Verdicts:
   *  - `gone`: nothing survives.
   *  - `stuck`: members live on but every group's SIGKILL was confirmed delivered, so they are stuck
   *    in the kernel and can start no new work (at most one in-flight syscall completes).
   *  - `unconfirmed`: a group lives on and its SIGKILL could not be confirmed (EPERM, a failed
   *    runner-uid wrapper): it may still be running.
   */
  private async awaitTickSurvivorsGone(flight: RunFlight): Promise<"gone" | "stuck" | "unconfirmed"> {
    const waitGone = async (ms: number): Promise<boolean> => {
      const deadline = Date.now() + ms;
      while (this.tickSurvivorsRemain(flight)) {
        if (Date.now() >= deadline) return false;
        await new Promise((r) => setTimeout(r, 25));
      }
      return true;
    };
    if (!this.tickSurvivorsRemain(flight)) return "gone";
    if (await waitGone(SURVIVOR_SINK_WAIT_MS)) return "gone";
    const hook = this.checkpointTestHooks?.tickSpawn?.signalGroup;
    for (const g of flight.survivingTickGroups) {
      g.killConfirmed = hook?.(g.pgid, "SIGKILL") ?? signalProcessGroup(g.pgid, g.identity, "SIGKILL");
    }
    if (await waitGone(SURVIVOR_SINK_WAIT_MS)) return "gone";
    const pgids = flight.survivingTickGroups.map((g) => g.pgid);
    if (flight.survivingTickGroups.every((g) => g.killConfirmed)) {
      flight.runLog.warn("durable sink proceeding while a SIGKILLed tick process group is stuck in the kernel", {
        run_id: flight.runId,
        pgids,
      });
      return "stuck";
    }
    flight.runLog.error("a surviving tick process group's SIGKILL could not be confirmed", { run_id: flight.runId, pgids });
    return "unconfirmed";
  }

  /**
   * issue #1597 M2: run a durable sink under the per-flight sink gate, after surviving tick process
   * groups had their bounded chance to go. A `stuck` survivor (SIGKILL confirmed) never blocks a
   * sink. An `unconfirmed` one may still run git: a sink passed `onUnconfirmed` (the best-effort
   * milestone checkpoint, which the next boundary retries) is SKIPPED; the last-chance sinks (park,
   * wall, completion hold, credential switch) proceed, because skipping them loses the only
   * durable copy, and the error line above records the residual.
   */
  private runGatedSink<T>(flight: RunFlight, fn: () => Promise<T>, onUnconfirmed?: () => T): Promise<T> {
    return flight.sinkGate.run(async () => {
      const verdict = await this.awaitTickSurvivorsGone(flight);
      if (verdict === "unconfirmed" && onUnconfirmed) {
        flight.runLog.warn("checkpoint skipped: a surviving tick process group may still be running", {
          run_id: flight.runId,
        });
        return onUnconfirmed();
      }
      return fn();
    });
  }

  private async retainedBareLockRemains(flight: RunFlight): Promise<boolean> {
    if (flight.retainedBareLocks.length === 0) return false;
    const still: string[] = [];
    for (const p of flight.retainedBareLocks) {
      try {
        await fs.lstat(p);
        still.push(p);
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") still.push(p);
      }
    }
    if (still.length === 0) {
      flight.runLog.info("mid-turn checkpoint sink unblocked: the retained lock file(s) are gone", {
        run_id: flight.runId,
        paths: flight.retainedBareLocks,
      });
    }
    flight.retainedBareLocks = still;
    return still.length > 0;
  }

  /**
   * issue #1597 M2 — the pinned SCAN-then-pack gate for an overlay-less checkpoint publish.
   * Resolves the range to SHAs (the same floor checkpointPack excludes), scans exactly that range
   * with gitleaks, fires the test seam, and returns:
   *   - `pinned`  — trusted and clean: publish exactly `range` (checkpointPack's pinned path);
   *   - `blocked` — a finding (`secret_found`) or an untrusted scan (`secret_scan_untrusted`): the
   *                 caller must NOT publish. The feed gets one deduped line naming the class; the
   *                 rule id / commit / path of each finding go to the run log only (the report is
   *                 --redact'd, so the secret itself is never read);
   *   - `none`    — no tracking tip at all: nothing to scan, and checkpointPack will report
   *                 no_local_tip (silent) exactly as before.
   * A tracking tip whose floor does not resolve is `secret_scan_untrusted` — an unscanned range is
   * never published. Never throws.
   *
   * BY DESIGN the park / shutdown / pause / capture publishes (and every overlay publish) are NOT
   * scanned: the api pushes them UNSCANNED to the forge's refs/uzi-checkpoints/<branch>
   * (Service.Publish in api/internal/workersvc/service.go) — they are the run's last chance to be
   * durable before the worker stops working on it, and a blocked or slow scan there would lose the
   * work outright. The scan guards the frequent, mid-run checkpoint stream only; a Codex milestone
   * inside a permit DEFERS its publish to the next tick outside the permit instead of scanning there
   * (`scan_deferred`, see doCheckpointPublish).
   */
  private async scanCheckpointForPublish(
    flight: RunFlight,
    barePath: string,
    branch: string,
    scanScope?: <T>(fn: () => Promise<T>) => Promise<T>,
  ): Promise<
    | { kind: "pinned"; range: CheckpointRange }
    | { kind: "blocked"; outcome: "secret_found" | "secret_scan_untrusted" }
    | { kind: "none" }
  > {
    const untrusted = (why: string): { kind: "blocked"; outcome: "secret_scan_untrusted" } => {
      this.reportPublishOutcome(
        flight,
        "secret:untrusted",
        "checkpoint publish skipped: secret_scan_untrusted",
        { why },
      );
      return { kind: "blocked", outcome: "secret_scan_untrusted" };
    };
    try {
      // Scan floors: the default branch and the last CONFIRMED checkpoint tip (already public).
      const range = await this.git.resolveCheckpointRange(barePath, branch, {
        confirmedTip: flight.lastCheckpointRefTip,
      });
      if (!range) {
        if ((await this.git.trackingTip(barePath, branch)) === null) return { kind: "none" };
        return untrusted("range_unresolved");
      }
      const scan = await (scanScope ?? ((fn) => fn()))(() =>
        this.git.secretScanCheckpointRange(barePath, range, { deadlineMs: this.scanDeadlineMs() }),
      );
      if (scan.findings.length > 0) {
        this.reportPublishOutcome(
          flight,
          "secret:found",
          "checkpoint publish skipped: secret_found",
          {
            trusted: scan.trusted,
            findings: scan.findings.slice(0, 20).map((f) => ({
              rule_id: f.ruleId,
              commit: f.commit,
              path: f.file,
            })),
          },
        );
        return { kind: "blocked", outcome: "secret_found" };
      }
      if (!scan.trusted) return untrusted(scan.reason ?? "scan_untrusted");
      await this.checkpointTestHooks?.afterCheckpointScan?.({ barePath, branch, range });
      return { kind: "pinned", range };
    } catch (e) {
      flight.runLog.warn("checkpoint secret scan threw; not publishing", {
        run_id: flight.runId,
        error: errMessage(e),
      });
      return untrusted("scan_threw");
    }
  }

  /**
   * issue #1597 M2 — arm the MID-TURN checkpoint tick for one executor.run. A repeating timer on the
   * injectable `this.setTickTimer`; a tick that finds the previous one still running is skipped. Each
   * tick:
   *   0. creates its AbortController FIRST — linked to flight.cancel (so shutdown() and a steering
   *      cancel abort it), to stop(), and to a {@link MIDTURN_TICK_TIMEOUT_MS} deadline — and races
   *      every pre-scope await against it; skips while a retained bare lock still exists;
   *   1. probes the runner clone for an in-progress git operation (lstat + one non-blocking gitfile
   *      read — no git child, no credentials); busy ⇒ `git_busy`, and after
   *      {@link MIDTURN_BUSY_FEED_AFTER} consecutive busy ticks ONE deduped feed line (reset once a
   *      tick gets past the probe); a tick stopped during the probe does nothing more;
   *   2. opens a cancellable scope under that controller, with a
   *      {@link TickSpawner} (own process group per child, SIGTERM → SIGKILL, settled()) as the
   *      GitCache boundary spawner;
   *   3. try-acquires the flight's sink gate (miss ⇒ `gate_busy`) — a gated path that wants it
   *      later PREEMPTS this tick and waits for it to settle;
   *   4. runs the checkpoint body QUIET with reap:false semantics (fetch-back → #1416 steer/bridge →
   *      time gate → pinned scan → pack → publish with the tick signal);
   *   5. after settlement, reconciles lock files a SIGKILLed child left in the bare (proven
   *      ownership only — see tick-spawner.ts), under the per-bare lock.
   * Every throw is caught: a tick never fails or stalls the run. `stop()` cancels the timer, aborts
   * the in-flight tick and awaits its full settlement; it never rejects.
   */
  private startMidTurnTicker(
    flight: RunFlight,
    barePath: string,
    clonePath: string,
    branch: string,
    body: (opts: Pick<CheckpointBodyOpts, "signal" | "scanScope">) => Promise<CheckpointBodyOutcome>,
  ): { stop: () => Promise<void> } | undefined {
    if (this.checkpointTickIntervalMs <= 0) return undefined;
    const { runLog, batcher, runId } = flight;
    const hooks = this.checkpointTestHooks;
    let stopped = false;
    let cancelTimer: (() => void) | undefined;
    let inFlight: Promise<void> | undefined;
    let inFlightAbort: AbortController | undefined;
    let busyStreak = 0;
    let busyLineEmitted = false;
    let gateRetryUsed = false;

    const handleLocks = (res: { removed: string[]; retained: RetainedLock[] }): boolean => {
      for (const p of res.removed) {
        runLog.info("mid-turn checkpoint removed a lock file its cancelled child provably owned", {
          run_id: runId,
          path: p,
        });
      }
      if (res.retained.length === 0) return false;
      runLog.warn("mid-turn checkpoint retained a git lock file in the worker bare", {
        run_id: runId,
        retained: res.retained.map((r) => ({
          path: r.path,
          dev: r.dev,
          ino: r.ino,
          size: r.size,
          mtime_ms: r.mtimeMs,
          pre_spawn: r.preSpawn,
          reason: r.reason,
          argv_class: r.argvClass,
        })),
      });
      const wasBlocked = flight.retainedBareLocks.length > 0 || flight.survivingTickGroups.length > 0;
      for (const r of res.retained) {
        if (!flight.retainedBareLocks.includes(r.path)) flight.retainedBareLocks.push(r.path);
      }
      if (!wasBlocked) {
        // One line per blocked EPISODE: it is emitted again only after the retained files are
        // gone (retainedBareLockRemains) and a later cancellation retains again.
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: { text: "mid-turn checkpoint sink blocked: a git lock file remains in the worker repository" },
        });
      }
      return true;
    };

    /** Record tick process groups that survived their SIGKILL; true when any did (sink blocked). */
    const handleSurvivors = (survivors: readonly SurvivingGroup[]): boolean => {
      if (survivors.length === 0) return false;
      runLog.warn("mid-turn checkpoint: a tick process group survived its SIGKILL; blocking the sink", {
        run_id: runId,
        pgids: survivors.map((g) => g.pgid),
      });
      const wasBlocked = flight.retainedBareLocks.length > 0 || flight.survivingTickGroups.length > 0;
      for (const g of survivors) {
        if (!flight.survivingTickGroups.some((x) => x.pgid === g.pgid)) flight.survivingTickGroups.push(g);
      }
      if (!wasBlocked) {
        batcher.emit({
          kind: "status",
          agent: "worker",
          payload: { text: "mid-turn checkpoint sink blocked: a checkpoint process survived being stopped" },
        });
      }
      return true;
    };

    const tick = async (): Promise<MidTurnTickOutcome> => {
      // issue #1597 M2 (review items 2/4): the controller exists BEFORE any await, so stop() (and a
      // shutdown, via flight.cancel) can abort a tick still in its pre-scope phase. That phase
      // spawns nothing and holds nothing, so its awaits are RACED against the abort: teardown never
      // waits on it, even if an fs op there were somehow stuck.
      const ac = new AbortController();
      inFlightAbort = ac;
      const onFlightAbort = (): void => ac.abort();
      if (flight.cancel.signal.aborted) ac.abort();
      else flight.cancel.signal.addEventListener("abort", onFlightAbort, { once: true });
      const cancelDeadline = this.setTimer(() => ac.abort(), MIDTURN_TICK_TIMEOUT_MS);
      const abortedP = new Promise<typeof ABORTED>((resolve) => {
        if (ac.signal.aborted) resolve(ABORTED);
        else ac.signal.addEventListener("abort", () => resolve(ABORTED), { once: true });
      });
      const raceAbort = <T>(p: Promise<T>): Promise<T | typeof ABORTED> => Promise.race([p, abortedP]);
      try {
        if (stopped || ac.signal.aborted) return "aborted";
        if (this.tickSurvivorsRemain(flight)) return "tick_process_survived";
        const blocked = await raceAbort(this.retainedBareLockRemains(flight));
        if (blocked === ABORTED) return "aborted";
        if (blocked) return "bare_lock_retained";
        if ((await raceAbort(hooks?.beforeBusyProbe?.() ?? Promise.resolve())) === ABORTED) return "aborted";
        const probe = await raceAbort(this.git.runnerCloneBusy(clonePath, branch));
        // Re-check after the probe: no full tick may start once the turn has ended.
        if (probe === ABORTED || stopped || ac.signal.aborted) return "aborted";
        if (probe.busy) {
          busyStreak++;
          runLog.info("mid-turn checkpoint deferred: git busy in the runner clone", {
            run_id: runId,
            markers: probe.markers,
            streak: busyStreak,
          });
          if (busyStreak >= MIDTURN_BUSY_FEED_AFTER && !busyLineEmitted) {
            busyLineEmitted = true;
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: "mid-turn checkpoint deferred: a git operation is in progress in the working tree",
              },
            });
          }
          return "git_busy";
        }
        busyStreak = 0;
        busyLineEmitted = false;
        return await scopedTick(ac);
      } finally {
        cancelDeadline();
        flight.cancel.signal.removeEventListener("abort", onFlightAbort);
        if (inFlightAbort === ac) inFlightAbort = undefined;
      }
    };

    // Steps 2-5: the supervised scope, the gate, the quiet checkpoint body, settlement, lock custody.
    const scopedTick = async (ac: AbortController): Promise<MidTurnTickOutcome> => {
      const spawner = new TickSpawner({
        signal: ac.signal,
        barePath,
        branch,
        log: runLog,
        ...(hooks?.tickKillGraceMs !== undefined ? { killGraceMs: hooks.tickKillGraceMs } : {}),
        ...(hooks?.tickSpawn ? { hooks: hooks.tickSpawn } : {}),
      });
      let retainedHere = false;
      let survivedHere = false;
      // Settle + reconcile WHILE the per-bare lock is still held (GitCache.withLock's pre-release
      // hook), so no other bare mutation can run between a SIGKILLed child and its lock's removal.
      const beforeLockRelease = async (key: string): Promise<void> => {
        if (key !== barePath || !ac.signal.aborted) return;
        await spawner.settled();
        if (handleSurvivors(spawner.survivors())) survivedHere = true;
        if (handleLocks(await spawner.reconcileLocks())) retainedHere = true;
      };
      const release = flight.sinkGate.tryAcquire(() => ac.abort());
      try {
        if (!release) return "gate_busy";
        let outcome: MidTurnTickOutcome;
        try {
          outcome = await this.git.withBoundaryProcessSpawner(
            spawner.spawn,
            ac.signal,
            () =>
              body({
                signal: ac.signal,
                scanScope: <T>(fn: () => Promise<T>): Promise<T> => {
                  const scanAc = new AbortController();
                  const onTick = (): void => scanAc.abort();
                  if (ac.signal.aborted) scanAc.abort();
                  else ac.signal.addEventListener("abort", onTick, { once: true });
                  const cancelScan = this.setTimer(() => scanAc.abort(), this.scanDeadlineMs());
                  return this.git
                    .withBoundaryProcessSpawner(spawner.scoped(scanAc.signal), scanAc.signal, fn, {
                      beforeLockRelease,
                    })
                    .finally(() => {
                      cancelScan();
                      ac.signal.removeEventListener("abort", onTick);
                    });
                },
              }),
            { beforeLockRelease },
          );
        } catch (e) {
          runLog.warn("mid-turn checkpoint tick threw", { run_id: runId, error: errMessage(e) });
          outcome = ac.signal.aborted ? "aborted" : "publish_failed:error";
        }
        if (ac.signal.aborted && outcome !== "published") outcome = "aborted";
        // FULL settlement before the gate is released: every child exited, then lock custody for a
        // child cancelled outside a withLock section (pack-objects, rev-parse), under the bare lock.
        await spawner.settled();
        if (handleSurvivors(spawner.survivors())) survivedHere = true;
        if (spawner.cancelledAny()) {
          const res = await this.git
            .withBareLock(barePath, () => spawner.reconcileLocks())
            .catch((e: unknown) => {
              runLog.warn("mid-turn checkpoint lock reconcile failed", { run_id: runId, error: errMessage(e) });
              return { removed: [], retained: [] };
            });
          if (handleLocks(res)) retainedHere = true;
        }
        if (retainedHere) outcome = "bare_lock_retained";
        if (survivedHere) outcome = "tick_process_survived";
        return outcome;
      } finally {
        // Released only now — after settlement — so a preempting sink waits for all of the above.
        release?.();
      }
    };

    const fire = (): void => {
      cancelTimer = undefined;
      if (stopped) return;
      cancelTimer = this.setTickTimer(armedFire, this.checkpointTickIntervalMs);
      if (inFlight) {
        runLog.info("mid-turn checkpoint tick skipped: the previous tick is still running", { run_id: runId });
        return;
      }
      // Declared first: the IIFE reads it after its first await (identity check below).
      let current: Promise<void> | undefined;
      current = (async () => {
        let outcome: MidTurnTickOutcome;
        try {
          outcome = await tick();
        } catch (e) {
          runLog.warn("mid-turn checkpoint tick failed", { run_id: runId, error: errMessage(e) });
          outcome = "publish_failed:error";
        }
        // Cleared BEFORE the outcome is reported, so an observer that fires the next tick at once
        // is never mistaken for an overlap.
        if (inFlight === current) inFlight = undefined;
        runLog.info("mid-turn checkpoint tick", { run_id: runId, outcome });
        // issue #1597 M2 (round 4): an owed (deferred) publish whose tick found the sink gate busy
        // is retried ONCE more soon (a kick) instead of waiting a full interval; a second miss waits
        // for the normal cadence. The flag resets on any tick outcome other than `gate_busy`.
        if (outcome === "gate_busy" && flight.pendingPublish && !gateRetryUsed) {
          gateRetryUsed = true;
          flight.kickMidTurnTick?.();
        } else if (outcome !== "gate_busy") {
          gateRetryUsed = false;
        }
        try {
          hooks?.onTickOutcome?.(outcome);
        } catch {
          /* a test observer never fails a tick */
        }
      })();
      inFlight = current;
    };
    // issue #1597 M2 (MR !1618 review): every arm uses `fire` BOUND to this (permit-free) async
    // context. A real timer runs its callback in the context that ARMED it, and the kick below is
    // armed from inside a Codex permit (the scan_deferred milestone), so an unbound tick would run
    // under that permit's GitCache boundary scope and SinkGate hold: once the permit has ended its
    // aborted signal makes the tick's withBareLock reject before acquiring, and a retained lock
    // would be missed by the reconcile.
    const armedFire = AsyncResource.bind(fire);
    cancelTimer = this.setTickTimer(armedFire, this.checkpointTickIntervalMs);
    // issue #1597 M2 (round 3): a deferred publish asks for a tick SOON (re-arms the timer).
    flight.kickMidTurnTick = () => {
      if (stopped) return;
      cancelTimer?.();
      cancelTimer = this.setTickTimer(armedFire, MIDTURN_KICK_DELAY_MS);
    };

    return {
      stop: async () => {
        stopped = true;
        flight.kickMidTurnTick = undefined;
        cancelTimer?.();
        cancelTimer = undefined;
        inFlightAbort?.abort();
        await inFlight?.catch(() => undefined);
      },
    };
  }

  private async publishCheckpointBestEffort(
    flight: RunFlight,
    barePath: string,
    branch: string,
    overlay?: CheckpointOverlayContext,
    signal?: AbortSignal,
  ): Promise<boolean> {
    // issue #1597 M1: a thin wrapper — every existing caller only needs "did it land".
    return (await this.publishCheckpointOutcome(flight, barePath, branch, overlay, signal)).published;
  }

  /**
   * issue #1597 M1: the typed form of {@link publishCheckpointBestEffort} — same params, same
   * side effects, but returns WHY a publish did not land ({@link PublishOutcome}) so the shutdown
   * sink can name a class on the feed. Feed discipline: `no_local_tip` and `aborted` are silent
   * (runLog only); `skipped` names only an allowlisted label; `rejected` names only the numeric
   * status; `error` never carries the thrown message onto the feed (runLog keeps it). A confirmed
   * publish after a previously-surfaced failure emits ONE recovery line and clears the dedupe set,
   * so a recurring failure is shown again.
   */
  private async publishCheckpointOutcome(
    flight: RunFlight,
    barePath: string,
    branch: string,
    overlay?: CheckpointOverlayContext,
    signal?: AbortSignal,
    /** issue #1597 M2: the scanned range — pack exactly these SHAs (see GitCache.checkpointPack). */
    pinned?: CheckpointRange,
  ): Promise<PublishOutcome> {
    // issue #1086 (F2): two-tip reconciliation. The CONFIRMED tip advances only on a real ACK; an
    // ambiguous result (non-2xx, or a throw after the pack tip is known) records the ATTEMPTED tip
    // so the next overlay chains from it. Caveat: under PERSISTENT consecutive ACK loss the
    // confirmed tip never advances while attempted marches forward, so each pack re-includes the
    // whole un-landed overlay chain; this is bounded by the broker's object cap and self-clears on
    // the first landed ACK.
    let packedTip: string | undefined;
    try {
      // issue #1597 M2: `pinned` is passed only by the scanned (overlay-less) publish; every other
      // caller keeps the unpinned 3-argument call shape.
      const packed = pinned
        ? await this.git.checkpointPack(barePath, branch, overlay, pinned)
        : await this.git.checkpointPack(barePath, branch, overlay);
      // tracking tip unresolved (no tracking ref, or it could not be read) — nothing to pack; not a
      // publish failure, stay silent
      if (!packed) return { published: false, reason: "no_local_tip" };
      packedTip = packed.tipOid;
      const res = await this.client.publishCheckpoint(flight.runId, packed.tipOid, packed.pack, signal);
      if (res.ok && res.body.published === true) {
        // PRD #1062 M2 (#1036): a CONFIRMED publish advances the known checkpoint ref tip to the
        // declared tip (the overlay `O_ov`, or realTip on the no-overlay path), so the NEXT
        // overlay carries it as parent[0] (base-first) and stays a fast-forward the broker takes.
        flight.lastCheckpointRefTip = packed.tipOid;
        // issue #1086 (F2): a confirmed publish reconciles the broker's ref, so clear any pending
        // attempted tip — the confirmed tip is now authoritative.
        flight.lastAttemptedCheckpointRefTip = undefined;
        // issue #1597 M1: a failure/skip line is on the feed, so say it recovered — once — and
        // clear the dedupe set so a recurring failure is surfaced again rather than swallowed.
        if (flight.reportedPublishOutcomes.size > 0) {
          flight.reportedPublishOutcomes.clear();
          flight.runLog.info("checkpoint publishing recovered", { run_id: flight.runId });
          flight.batcher.emit({
            kind: "status",
            agent: "worker",
            payload: { text: "checkpoint publishing recovered — published to origin" },
          });
        }
        return { published: true };
      }
      if (res.ok) {
        // A 2xx that did NOT publish: a best-effort server-side skip. Name the reason.
        // issue #1086 (F2): record NOTHING as attempted — the server definitively did not advance
        // its ref, so the next overlay must keep chaining from the confirmed tip.
        // issue #1597 M1: only an allowlisted label reaches the feed; anything else is `other`.
        const skipLabel = publishSkipLabel(res.body.skipped);
        this.reportPublishOutcome(flight, `skip:${skipLabel}`, `checkpoint publish skipped: ${skipLabel}`);
        return { published: false, reason: "skipped", skipLabel };
      }
      // issue #1086 (F2): a non-2xx is AMBIGUOUS — the broker may have accepted the push before
      // the ACK was lost — so record the attempted tip for the next overlay to chain from.
      flight.lastAttemptedCheckpointRefTip = packed.tipOid;
      this.reportPublishOutcome(
        flight,
        `http:${res.httpStatus}`,
        `checkpoint publish failed: HTTP ${res.httpStatus}`,
      );
      return { published: false, reason: "rejected", httpStatus: res.httpStatus };
    } catch (e) {
      // issue #1086 (F2): a throw is AMBIGUOUS too, but only after the pack tip was obtained — a
      // throw DURING checkpointPack leaves packedTip undefined and records nothing.
      if (packedTip !== undefined) flight.lastAttemptedCheckpointRefTip = packedTip;
      // issue #1597 M1: a deadline/permit abort is an expected bounded stop, not a publish fault —
      // runLog only, never the feed (the shutdown sink names it as `timeout` itself).
      if (signal?.aborted || isAbortLikeError(e)) {
        flight.runLog.info("checkpoint publish aborted", { run_id: flight.runId, error: errMessage(e) });
        return { published: false, reason: "aborted" };
      }
      // issue #1597 M1: the feed line carries the CLASS only — a thrown message can embed remote
      // text or a credentialed URL; the runLog keeps it for operators.
      this.reportPublishOutcome(flight, "error", "checkpoint publish failed: error", {
        error: errMessage(e),
      });
      return { published: false, reason: "error" };
    }
  }

  /**
   * issue #1030: make a checkpoint-publish failure or skip VISIBLE. Always `runLog.warn`s
   * the outcome; emits a run-feed `status` line at most ONCE per distinct outcome key per
   * run, so the ~20-min time-gated retry of a persistently-failing publish does not spam the
   * feed. Reuses the same `batcher.emit({ kind: "status", agent: "worker", … })` mechanism
   * every other worker-authored status line uses. issue #1597 M1: `logFields` reach the runLog
   * only, never the feed.
   */
  private reportPublishOutcome(
    flight: RunFlight,
    key: string,
    text: string,
    logFields: Record<string, unknown> = {},
  ): void {
    flight.runLog.warn(text, { run_id: flight.runId, ...logFields });
    if (flight.reportedPublishOutcomes.has(key)) return;
    flight.reportedPublishOutcomes.add(key);
    flight.batcher.emit({ kind: "status", agent: "worker", payload: { text } });
  }

  /**
   * Handle a usage-limit death (PRD #35). Returns whether the run is PARKED, which
   * is the sole input to the cleanup carve-out above.
   *
   * 🔴 THE ANSWER IS `status === "limit_wait"`, NEVER `applied`.
   *
   * "Not parked" has five causes, and three of them are DESIGNED outcomes rather
   * than errors: the retry budget is exhausted, the computed retry_not_before
   * exceeded RUN_LIMIT_MAX_PARK, or wait_on_limit is false and the server coerced
   * the report. On all three the server FAILS THE RUN AND ANSWERS 200 — a
   * transition was applied, just not the requested one — so `applied` is true while
   * the run is emphatically not parked. Since budget exhaustion is the ordinary end
   * of a run that keeps hitting limits, an `applied`-keyed branch would leak the
   * clone, the plugin dir and up to ~170 MB of HOME on the most common cause of all.
   *
   * Testing one literal positively also makes the default arm "clean up", so an
   * unforeseen cause, a future status, an older server that echoes nothing, and a
   * parse failure all land on the safe side by construction. An enumeration of the
   * five causes would go stale; this cannot.
   *
   * On not-parked the runner sends NO further state report. The server has already
   * decided and recorded the outcome — a `failed` report on top of it would clobber
   * a `cancelled`, and would overwrite the server-composed limit sentence with a
   * worker-composed one.
   *
   * ── Why the feed payload omits Decision 10's `attempt` ───────────────────────
   * The worker CANNOT KNOW IT, and a guess would be wrong by construction. The park
   * count is `runs.limit_wait_count`, which the SERVER increments inside
   * SetRunLimitWait — strictly AFTER this message is emitted, and after the batcher
   * that carries it has been closed. The claim payload does not deliver the previous
   * value either (it carries `requeue_count`, a different counter for worker deaths).
   * So the best a worker could emit is a stale N-1 that disagrees with the run row.
   *
   * Nothing is lost: `limit_wait_count` is on RunDTO and on the web `Run` type, so a
   * renderer showing "attempt N" reads the authoritative value it already has,
   * rather than a snapshot frozen into a feed row that never updates when the run
   * parks again.
   */
  private async handleLimitReached(
    err: LimitReachedError,
    claim: ClaimResponse,
    batcher: MessageBatcher,
    reportState: (body: StateRequest) => Promise<StateAck>,
    runLog: Logger,
  ): Promise<boolean> {
    const detail = describeLimit(err);
    // The structured fields ride BOTH the park and the opt-out failure report. The
    // worker never composes the user-facing sentence from them (Decision 8): it
    // reports the raw type and reset, and the server — which owns the allowlist —
    // writes the reason. Composing it here would carry an unvalidated rateLimitType
    // into the run row as free text by a different route.
    const limitFields = {
      rate_limit_type: err.rateLimitType,
      limit_resets_at: err.resetsAtMs,
    };
    // The feed payload for the two structured kinds (PRD #35 Decision 10). Kept
    // STRUCTURED rather than interpolated into a sentence because the renderer must
    // map `rate_limit_type` through a known-value lookup with a neutral fallback: a
    // feed payload is worker-authored and the server's enum allowlist does not reach
    // it, and you cannot map a value that has been baked into prose without
    // re-parsing the prose — which is worse than not structuring it at all.
    //
    // `resets_at` is an ISO string, matching every other timestamp the web renders
    // and sidestepping the seconds-vs-milliseconds ambiguity that `resetsAt` itself
    // carries. OMITTED ENTIRELY when the reset is unknown, never null, so "unknown"
    // is one shape on the wire rather than two.
    //
    // `attempt` from Decision 10 is deliberately ABSENT — see the note in
    // handleLimitReached's doc comment.
    const feedPayload: Record<string, unknown> = {};
    if (err.rateLimitType !== undefined)
      feedPayload["rate_limit_type"] = err.rateLimitType;
    if (err.resetsAtMs !== undefined)
      feedPayload["resets_at"] = new Date(err.resetsAtMs).toISOString();

    if (!claim.wait_on_limit) {
      // Opted out: fail, but with the structured facts attached so the server can
      // say WHY instead of leaving today's bare "agent run failed:
      // error_during_execution".
      runLog.info("run hit a usage limit and is not opted in to waiting", {
        detail,
      });
      batcher.emit({
        kind: "limit_hit",
        agent: "worker",
        payload: { ...feedPayload },
      });
      await batcher.close().catch(() => undefined);
      // PRD #69 M7a: this opt-out failure is definitionally rate-limit-caused, so
      // stamp the trusted class alongside the structured limit fields. (The server
      // also stamps 'rate_limited' on the parallel limit_wait→non-park path, so the
      // class is recorded regardless of which report shape reached it.)
      await reportState({
        status: "failed",
        fail_origin: "rate_limited",
        ...limitFields,
      }).catch((e) =>
        runLog.error("could not report limit failure", {
          error: errMessage(e),
        }),
      );
      return false;
    }

    runLog.info("run hit a usage limit; requesting a park", { detail });
    batcher.emit({
      kind: "limit_wait",
      agent: "worker",
      payload: { ...feedPayload },
    });
    // issue #1030: FLUSH (not close) here so the limit_wait feed line lands before the state
    // report, exactly as the prior close did — but leave the batcher OPEN so the park
    // durability block in execute() can emit the checkpoint-publish outcome onto the feed.
    // execute()'s park block closes the batcher once, on every parked run. The two
    // non-park returns below still CLOSE it themselves, since execute() emits nothing more
    // on those paths.
    await batcher.flush().catch(() => undefined);

    let ack: StateAck;
    try {
      ack = await reportState({ status: "limit_wait", ...limitFields });
    } catch (e) {
      // The park request never landed. Clean up: this run is not parked, and a
      // preserved HOME nothing will ever claim is an unbounded leak.
      runLog.error(
        "could not report the park; cleaning up as an unparked run",
        { error: errMessage(e) },
      );
      await batcher.close().catch(() => undefined);
      return false;
    }

    if (ack.status !== "limit_wait") {
      runLog.warn("the server did not park this run; cleaning up", {
        applied: ack.applied,
        server_status: ack.status ?? "unknown",
      });
      await batcher.close().catch(() => undefined);
      return false;
    }
    return true;
  }

  /**
   * PRD #1349 M2 — the run's current restore-point head: the committed tip of the runner
   * clone this generation holds. NULL when there is no clone/branch yet (a pre-clone early
   * failure) or the tip is unreadable, so the caller skips disposition rather than guessing.
   */
  private async currentRestorePointHead(flight: RunFlight): Promise<string | null> {
    const clonePath = flight.runnerClone?.path;
    const branch = flight.runnerClone?.branch ?? flight.branch;
    if (!clonePath || !branch) return null;
    return this.git.branchTip(clonePath, branch).catch(() => null);
  }

  /**
   * PRD #1349 M2 (D1/D3) — after clone/reseed and BEFORE model work, record this run's exact
   * claim generation into the durable journal (keyed to the restore point the run starts from)
   * and inventory any prior open custody holds. The generation-evidence pin lets a later
   * empty-turn park or early terminal exit disposition the EXACT hold without inferring
   * `current - 1`; the inventory logs prior holds by exact id + generation but NEVER acts on
   * them (ancestry settlement is deferred to the final durable head at disposition time).
   *
   * Credential-free (a local journal write + a join-token GET, never a forge PAT) and
   * best-effort: the source stays protected by the server hold regardless, so it never
   * disturbs the run's start.
   */
  private async recordRecoveryGenerationEvidence(
    claim: ClaimResponse,
    flight: RunFlight,
  ): Promise<void> {
    if (!this.recovery.enabled) return; // token-less harness: no journal, no inventory call
    if (!isCodePublishingKind(resolveRunKind(claim.kind))) return;
    // issue #1582 M2 (N3): a newer generation started, so an older successor's still-`adopted` /
    // `pushed` settlement records can never settle through it: stop them (pins kept).
    if (claim.claim_generation !== undefined) {
      await this.settler.supersedeOlderGenerations(claim.run_id, claim.claim_generation);
    }
    try {
      const startTip = await this.currentRestorePointHead(flight);
      await this.pinRecoveryGeneration(claim, flight, startTip);
      const holds = await this.recovery.inventoryHolds(claim.run_id);
      if (holds.length > 0) {
        flight.runLog.info("recovery: prior open custody holds inventoried after clone", {
          run_id: claim.run_id,
          claim_generation: claim.claim_generation,
          holds: holds.map((h) => ({
            hold_id: h.hold_id,
            generation: h.generation,
            has_available_capture: h.has_available_capture,
            capture_state: h.capture_state,
          })),
        });
        await this.recordAdoptionEvidence(claim, flight, holds);
      }
    } catch (err) {
      flight.runLog.warn(
        "recovery: generation-evidence pin/inventory failed (source stays protected by the server hold)",
        { run_id: claim.run_id, error: errMessage(err) },
      );
    }
  }

  /**
   * issue #1582 M2 — record ADOPTION evidence for each inventoried OLDER-generation hold whose work
   * this generation adopted: only when the clone was seeded from this run's own tracking ref or its
   * own mirrored checkpoint (never a default/origin reseed). For each hold with generation below
   * the current claim generation, the predecessor's source comes ONLY from its MAC-authenticated
   * recovery-journal record for that exact generation (none/invalid ⇒ skip, the hold stays
   * retained), and only a source contained in the adopted base is kept (a local skip-only filter).
   * The source and adopted tip are pinned under `refs/uzi-settle/...` (both must be present in the
   * bare) before the record is written. Best-effort, local and credential-free.
   */
  private async recordAdoptionEvidence(
    claim: ClaimResponse,
    flight: RunFlight,
    holds: Array<{ hold_id: string; generation: number }>,
  ): Promise<void> {
    if (!this.settlement.enabled) return;
    const clone = flight.runnerClone;
    const barePath = flight.barePath;
    const successor = claim.claim_generation;
    if (!clone || !barePath || successor === undefined) return;
    if (clone.seededFrom !== "tracking" && clone.seededFrom !== "checkpoint") return;
    const seededFrom = clone.seededFrom;
    try {
      const records = await this.recovery.inspect(claim.run_id);
      for (const hold of holds) {
        if (!Number.isSafeInteger(hold.generation) || hold.generation >= successor) continue;
        // Ids the journal would refuse are refused BEFORE any pin, so no orphan refs/uzi-settle pin
        // is left behind and two ids that sanitize alike can never share one.
        if (!isSafeSettlementId(claim.run_id) || !isSafeSettlementId(hold.hold_id)) continue;
        const pred = records.find((r) => r.generation === hold.generation);
        if (!pred) continue; // no authenticated predecessor source → no evidence; hold retained
        // Local pre-filter (it can only skip, never release; the server does the forge proof): record
        // evidence only when the predecessor's source is contained in (an ancestor of, or equal to)
        // the adopted base in the trusted bare. A recovered wip(park) marker is reset --soft out of
        // history, so it is never contained, and neither is any other source the adopted base does
        // not include. Not contained, or the check errors → no pin, no record; the hold is retained.
        if (!/^[0-9a-f]{40}$/.test(pred.sourceSha)) continue;
        if (!(await this.git.isAncestorRef(barePath, pred.sourceSha, clone.baseCommit))) continue;
        const pinned = await this.git.pinSettlementRefs(barePath, claim.run_id, hold.hold_id, {
          source: pred.sourceSha,
          adopted: clone.baseCommit,
        });
        if (!pinned) continue;
        const record: SettlementRecord = {
          version: 1,
          runId: claim.run_id,
          holdId: hold.hold_id,
          predecessorGeneration: hold.generation,
          successorGeneration: successor,
          sourceSha: pred.sourceSha,
          sourceCaptureId: pred.captureId,
          adoptedSha: clone.baseCommit,
          seededFrom,
          branch: clone.branch,
          barePath,
          createdAt: this.now(),
          state: "adopted",
          attempts: 0,
        };
        if (await this.settlement.put(record)) {
          flight.runLog.info("recovery settlement: adoption evidence recorded for a predecessor hold", {
            run_id: claim.run_id,
            hold_id: hold.hold_id,
            predecessor_generation: hold.generation,
            successor_generation: successor,
            seeded_from: seededFrom,
          });
        }
      }
    } catch (err) {
      flight.runLog.warn("recovery settlement: adoption evidence not recorded (predecessor hold retained)", {
        run_id: claim.run_id,
        error: errMessage(err),
      });
    }
  }

  /**
   * issue #1582 M2 — the in-run settle, run AFTER the completed report's terminal outcome resolved
   * (never inside the terminal send). Sends every `pending_settle` record of this run — i.e. only
   * when the reportState choke point observed a completion ACK and promoted it. Aborts with the
   * flight's cancel signal (worker shutdown); the worker sweep retries anything left. Best-effort.
   * The sends are sequential and awaited before the run's finalize returns, so the settle holds the
   * run slot for at most the worker client's HTTP timeout (`httpTimeoutMs`, 30 s by default) per
   * `pending_settle` hold (one POST each, no in-call retry), plus the local journal/ref cleanup.
   */
  private async settleAfterCompletion(
    claim: ClaimResponse,
    flight: RunFlight,
    body: Parameters<RunFlight["reportState"]>[0],
  ): Promise<void> {
    if ((body as StateRequest).status !== "completed") return;
    if (!isCodePublishingKind(resolveRunKind(claim.kind))) return;
    await this.settler.settleRun(claim.run_id, flight.cancel.signal).catch((err) => {
      flight.runLog.warn("recovery settlement: post-completion settle failed (the sweep retries)", {
        run_id: claim.run_id,
        error: errMessage(err),
      });
    });
  }

  /**
   * issue #1582 M2 — BEFORE a completed report with a pushed branch is sent, persist the
   * successor's pushed head on every `adopted` settlement record of this generation and move it to
   * the write-ahead `pushed` state (disposition `publication`), pinning the head under `.../pushed`.
   * `pushed` is NEVER sent: only an observed completion ACK promotes it to `pending_settle`
   * ({@link observeSettlementTerminalAck}). A completion with no branch (report_only / not_code)
   * leaves the records `adopted`; that completion's ACK then moves them terminal. A completion WITH
   * a branch whose pushed head cannot be recorded (no bare, no readable tracking tip, or the
   * `.../pushed` pin failed) moves the record `terminal` / `pushed_head_unrecorded` here, so the
   * owner sees the real cause rather than a later `successor_not_published`. Best-effort.
   */
  private async persistSettlementPushedHead(
    claim: ClaimResponse,
    flight: RunFlight,
    body: Parameters<RunFlight["reportState"]>[0],
  ): Promise<void> {
    if (!this.settlement.enabled) return;
    const b = body as StateRequest;
    if (b.status !== "completed" || typeof b.branch !== "string" || b.branch === "") return;
    const barePath = flight.barePath;
    try {
      const adopted = (await this.settlement.listRun(claim.run_id)).filter(
        (r) => r.state === "adopted" && r.successorGeneration === claim.claim_generation,
      );
      if (adopted.length === 0) return;
      const unrecorded = async (rec: SettlementRecord, why: string, error?: string): Promise<void> => {
        flight.runLog.warn("recovery settlement: successor pushed head not recorded; predecessor hold retained", {
          run_id: claim.run_id,
          hold_id: rec.holdId,
          branch: b.branch,
          reason: PUSHED_HEAD_UNRECORDED,
          detail: why,
          ...(error ? { error } : {}),
        });
        await this.settler.markTerminal(rec, PUSHED_HEAD_UNRECORDED);
      };
      // The interlocked completion carries the exact permitted head; otherwise read the landed tip
      // off the tracking ref the finalize push wrote.
      let pushed: string | null = null;
      let tipError: string | undefined;
      if (typeof b.head === "string" && /^[0-9a-f]{40}$/.test(b.head)) {
        pushed = b.head;
      } else if (barePath) {
        pushed = await this.git.trackingTip(barePath, b.branch).catch((err: unknown) => {
          tipError = errMessage(err);
          return null;
        });
      }
      if (!barePath || !pushed) {
        const why = !barePath ? "no runner bare" : "tracking tip unreadable";
        for (const rec of adopted) await unrecorded(rec, why, tipError);
        return;
      }
      for (const rec of adopted) {
        let pinned = false;
        let pinError: string | undefined;
        try {
          pinned = await this.git.pinSettlementRefs(barePath, rec.runId, rec.holdId, { pushed });
        } catch (err) {
          pinError = errMessage(err);
        }
        if (!pinned) {
          await unrecorded(rec, "pushed pin failed", pinError);
          continue;
        }
        await this.settlement.put({
          ...rec,
          pushedSha: pushed,
          disposition: "publication",
          state: "pushed",
        });
      }
    } catch (err) {
      flight.runLog.warn("recovery settlement: pushed head not persisted (predecessor holds retained)", {
        run_id: claim.run_id,
        error: errMessage(err),
      });
    }
  }

  /**
   * PRD #1349 M2 (D1) — pin `head` as this run's exact-generation restore point in the
   * authenticated journal. Credential-free and local (no network, no PAT), so it is safe on a
   * path where the agent tree may still be alive (an owner pause). Returns the record, or
   * undefined when recovery is disabled, there is no head to pin, or the pin failed — the
   * caller treats undefined as "nothing to drive", never as a release authority.
   */
  private async pinRecoveryGeneration(
    claim: ClaimResponse,
    flight: RunFlight,
    head: string | null,
  ): Promise<RecoveryRecord | undefined> {
    if (!isCodePublishingKind(resolveRunKind(claim.kind))) return undefined;
    const branch = flight.runnerClone?.branch ?? flight.branch;
    if (!head || !branch) return undefined;
    return this.recovery.pin({
      runId: claim.run_id,
      sourceSha: head,
      kind: resolveRunKind(claim.kind),
      branch,
      generation: claim.claim_generation,
    });
  }

  /**
   * PRD #1349 M2 (D4.5) — transfer THIS run's exact clone-only restore-point head into the TRUSTED
   * worker bare and POSITIVELY VERIFY it is present there, BEFORE bundle production reads it. Modeled
   * on the transfer-and-verify half of {@link captureRecoveryRestorePoint} / {@link captureHoldContext}:
   * worktreeStatus (null ⇒ unreadable, cannot assert clean) → dirty? commitWipMarker (REQUIRE the WIP
   * committed) → fetchAgentBranch (THROWS on failure) → verifyRunnerTrackingCovers (POSITIVE verify) →
   * trackingTip. Returns the SHA verified present in the bare (the tracking tip after the transfer),
   * or null on ANY failure — the caller then preserves the source clone and keeps the hold open.
   *
   * The clone-only head only becomes reproducible for {@link GitCache.produceRecoveryBundle}'s
   * trusted-bare guard (a `rev-parse <sha>^{commit}` in the bare) AFTER this transfer; without it the
   * settle would feed produceRecoveryBundle a commit absent from the bare, throwing needs_action while
   * terminal cleanup retires the only source. This is a DISPOSITION source, so it deliberately OMITS
   * the origin publish + park-sink bridge that {@link captureRecoveryRestorePoint} does: it only needs
   * the exact head present + reproducible in the bare, never a resumable resume-seed checkpoint.
   */
  private async transferRestorePointToTrustedBare(
    flight: RunFlight,
    runLog: Logger,
    runId: string,
    generation: number,
  ): Promise<string | null> {
    const barePath = flight.barePath;
    const worktreePath = flight.worktreePath;
    const branch = flight.branch;
    if (!barePath || !worktreePath || !branch) {
      runLog.warn("recovery: settle transfer skipped — no clone paths on the flight; nothing to transfer");
      return null;
    }
    // Distinguish dirty vs clean EXPLICITLY (runner-uid porcelain) rather than trusting
    // commitWipMarker's ambiguous false. A null (unreadable) status cannot assert clean ⇒ do not
    // transfer: retain the clone.
    const status = await this.git.worktreeStatus(worktreePath);
    if (status === null) {
      runLog.warn("recovery: settle transfer — worktree status unreadable (cannot assert clean)");
      return null;
    }
    if (status.length > 0) {
      // DIRTY → commit the WIP marker and REQUIRE it committed; a false here is unambiguously a
      // commit FAILURE (we already know the tree is dirty), so the head is not transferable.
      const committed = await this.git.commitWipMarker(worktreePath);
      if (!committed) {
        runLog.warn("recovery: settle transfer — WIP commit of a dirty tree failed");
        return null;
      }
    }
    // Fetch the run's tip into the worker bare's tracking ref (refs/uzi-runner/<branch>), so the exact
    // clone-only head's objects are now present in the trusted bare. fetchAgentBranch THROWS on
    // failure (unlike the void fetchBackBestEffort), so a failed transfer is caught here.
    try {
      await this.git.fetchAgentBranch(barePath, worktreePath, branch, flight.runId);
    } catch (e) {
      runLog.warn("recovery: settle transfer fetch-back failed", { error: errMessage(e) });
      return null;
    }
    // POSITIVELY VERIFY the bare's tracking ref now covers the run's current HEAD (incl. any WIP
    // marker) before trusting it as the bundle source.
    const verified = await this.git.verifyRunnerTrackingCovers(barePath, worktreePath, branch);
    if (!verified) {
      runLog.warn("recovery: settle transfer — the tracking ref does not cover the run HEAD");
      return null;
    }
    // issue #1507 — do NOT reread the shared, per-branch tracking ref refs/uzi-runner/<branch> here.
    // With WORKER_MAX_CONCURRENT_RUNS > 1 a concurrent run sharing this bare+branch can have moved it
    // to ITS tip since the positive verify above, and a reread (the old trackingTip call) would pin
    // THAT run's commit under THIS run's id+generation. The verify just proved the tracking ref
    // equals this clone's HEAD, so the run's own PRIVATE, single-writer HEAD IS the exact verified
    // SHA — read it from the clone, never the shared ref. (The sibling captureHoldContext reread is
    // deliberately left: there bridgeParkSinkBestEffort MOVES the ref to a synthesised bridge commit
    // between verify and read, so its reread is by-design, and its head feeds a PRD #218
    // owner-stamp-gated same-worker reseed, not a directly-archived cross-run bundle.)
    const head = await this.git.worktreeHead(worktreePath);
    if (head === null) {
      runLog.warn("recovery: settle transfer — verified, but the run HEAD is unresolvable");
      return null;
    }
    // Durably anchor the exact verified head under a run+generation-scoped pin ref, so it stays
    // referenced through bundle production / journal handoff even if another run moves the branch
    // tracking ref afterward. A false anchor (the object is absent in the bare, or the update
    // failed) means no durable replacement exists yet ⇒ retain the source clone and keep the hold
    // open, never a clone-only SHA fed to bundle production.
    const anchored = await this.git.anchorRecoveryHead(barePath, runId, generation, head);
    if (!anchored) {
      runLog.warn("recovery: settle transfer — could not durably anchor the verified head in the trusted bare");
      return null;
    }
    return head;
  }

  /**
   * PRD #1349 M2 (D4) — settle THIS run's exact-generation custody hold at a park / early
   * terminal exit that never reached the finalization pin. Pins the verified restore-point
   * head under the exact claim generation, then runs the SAME fresh-forge disposition the
   * finalization-failure drive uses:
   *
   *   - provably no unpublished committed output (H already reachable from the freshly fetched
   *     forge tip) → RELEASE that exact hold before requeue/cleanup;
   *   - committed work → a generation-bound bundle uploaded through the encrypted archive
   *     service (the reconciler releases the hold once the archive is available);
   *   - a failed/unverifiable forge comparison, an oversize bundle, or an upload failure →
   *     RETAIN the source and the open hold (needs_action), never a fabricated release.
   *
   * The credentialed forge fetch requires the agent tree to be REAPED first (the
   * reap-before-credentialed-git invariant); every caller runs on a reaped path. Best-effort:
   * it MUST NOT disturb the run's honest terminal/park reporting.
   */
  private async settleRecoveryGeneration(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    signal?: AbortSignal,
  ): Promise<void> {
    if (!this.recovery.enabled) return; // token-less harness: nothing to settle
    if (!isCodePublishingKind(resolveRunKind(claim.kind))) return;
    const barePath = flight.barePath;
    if (!barePath) return;
    try {
      // issue #1507 — the pin ref anchoring the transferred head is keyed on this run's exact claim
      // generation; a v1 claim without one falls back to 0. This value need NOT match the journal
      // record's generation (recovery.pin stores claim.claim_generation verbatim — undefined for a
      // v1 claim, never 0): anchorRecoveryHead and deleteRecoveryPin both use THIS local value, so
      // the pin ref's create and delete stay paired regardless of the journal.
      const generation = claim.claim_generation ?? 0;
      const verifiedSha = await this.transferRestorePointToTrustedBare(flight, runLog, claim.run_id, generation);
      if (verifiedSha === null) {
        // Transfer/verification failed: no durable replacement exists in the trusted bare yet, so
        // the runner clone is the only recoverable source. Preserve it and keep the hold OPEN;
        // never feed a clone-only SHA to bundle production (it would fail the trusted-bare guard,
        // and terminal cleanup would then retire the only source). Keep the generation-evidence pin.
        flight.preserveRecoveryClone = true;
        await this.pinRecoveryGeneration(claim, flight, await this.currentRestorePointHead(flight));
        runLog.warn(
          "recovery: could not transfer the restore-point head into the trusted bare; preserving the source clone and retaining the hold",
          { run_id: claim.run_id, claim_generation: claim.claim_generation },
        );
        return;
      }
      // Transfer succeeded — verifiedSha is positively verified present in the bare. Pin THAT SHA
      // (pin advances a pinned record's source), then run the SAME fresh-forge disposition. On a
      // bundle/upload failure AFTER this verified transfer the covered bare tracking ref (and/or a
      // journaled bundle) is the retained source, so the whole clone need not persist; the hold stays
      // open (needs_action). A provably-empty proof releases; committed work archives.
      const record = await this.pinRecoveryGeneration(claim, flight, verifiedSha);
      if (!record) return; // recovery disabled, no head, or pin failed — the hold stays protected
      const defaultBranch =
        claim.repo.default_branch?.trim() ||
        (await this.git.defaultBranchName(barePath)) ||
        "main";
      const outcome = await this.recovery.captureAndUpload({
        record,
        barePath,
        defaultBranch,
        forgePat: claim.secrets.forge_pat,
        cloneUrl: claim.repo.clone_url,
        forgeUsername: claim.secrets.forge_username,
        signal,
      });
      // issue #1507 — a durable replacement now exists on the SUCCESS outcomes (the bundle is
      // archived, or the head was already forge-published — both surface state "uploaded"), so the
      // transient bare pin can go. On any retain (needs_action) LEAVE it as the durable anchor that
      // keeps the retained source's exact head reachable in the bare.
      if (outcome.state === "uploaded") {
        await this.git.deleteRecoveryPin(barePath, claim.run_id, generation);
      }
      runLog.info("recovery: park/early-terminal disposition outcome", {
        run_id: claim.run_id,
        claim_generation: claim.claim_generation,
        capture_id: outcome.captureId,
        state: outcome.state,
        reason: outcome.reason,
      });
    } catch (err) {
      runLog.warn("recovery: park/early-terminal disposition failed (reporting is unaffected)", {
        run_id: claim.run_id,
        error: errMessage(err),
      });
    }
  }

  /**
   * PRD #1349 M2 (F2) — the REAP half of {@link reapThenSettleRecoveryGeneration}, split out
   * (#1531) so a terminal-reporting caller can run the reap FIRST while the run is still
   * actively-claimed and run the non-status-gated settle AFTER the terminal report. The Codex
   * per-sink credential reconcile inside `withBoundary` (refreshCodex/releaseCodex) is authorized
   * by the api ONLY while the run is actively-claimed (codexActivelyClaimedStatuses); once the run
   * is terminal it is refused (409), the blocked reconcile throws `CodexBoundaryError`, and the
   * hold would leak as source_only. Returns `false` on any guard miss (nothing to settle) and
   * `false` on a reap failure (keep the hold); returns `true` only after the reap succeeds, at
   * which point the caller may run the credentialed {@link settleRecoveryGeneration}.
   */
  private async reapRecoveryProviderForSettle(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    boundary: BoundaryRequest["boundary"],
  ): Promise<boolean> {
    if (!this.recovery.enabled) return false; // token-less harness: no credentialed settle, no reap needed
    if (!isCodePublishingKind(resolveRunKind(claim.kind))) return false;
    if (!flight.barePath) return false; // no clone → nothing to settle, so nothing to protect
    try {
      await this.reapForSink(
        flight.executor,
        { boundary, deadlineMs: this.codexBoundaryDeadlineMs },
        async () => {
          // Empty body: the reap itself is the point. The credentialed settle runs OUTSIDE the
          // boundary (mirroring the park path), after the provider root is reaped.
        },
      );
    } catch (err) {
      runLog.warn("recovery: pre-settle reap failed; retaining the generation hold (reporting unaffected)", {
        run_id: claim.run_id,
        error: errMessage(err),
      });
      return false; // provider not confirmed reaped → do NOT run the credentialed fetch
    }
    return true;
  }

  /**
   * PRD #1349 M2 (F2) — reap the agent tree (Codex-aware) BEFORE the credentialed
   * {@link settleRecoveryGeneration}, then settle. Every credentialed settle site that is NOT
   * already behind a reap MUST go through this. settle does a PAT-bearing fresh-forge fetch, and
   * for a CODEX run the provider root is torn down only in executeClaim's `finally` (the FINAL
   * registry disposal, AFTER these catch/handler paths) while `killAgentTree` is a NO-OP — so a
   * bare settle here would race a still-alive Codex provider subprocess, the exact
   * PAT-in-/proc/environ exposure the reap exists to prevent. `reapForSink` reaps via
   * `killAgentTree` for Claude/stub and via `withBoundary` quiesce+reap for Codex (the SAME
   * mechanism the limit-park sink uses), so the fetch always runs reaped; the settle then runs
   * AFTER the boundary releases (mirroring the park path), and the provider stays dead because the
   * agent run has already returned. The finally's FINAL registry disposal is unaffected —
   * `withBoundary` reaps the roots but does NOT dispose the registry, and disposeTools is
   * idempotent. Best-effort: on ANY reap failure (incl. a blocked Codex boundary) it RETAINS the
   * hold and skips the credentialed fetch rather than run it un-reaped, and never disturbs the
   * run's terminal/park reporting.
   */
  private async reapThenSettleRecoveryGeneration(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
    boundary: BoundaryRequest["boundary"],
  ): Promise<void> {
    if (await this.reapRecoveryProviderForSettle(claim, flight, runLog, boundary)) {
      await this.settleRecoveryGeneration(claim, flight, runLog);
    }
  }

  /**
   * #1197, verified 2026-09-08: keep the execution and steering poller active
   * while retrying local capture, and report a promotable recovery_wait ONLY
   * after current HEAD is verified in the worker-owned tracking ref. A failed
   * park report is retried too: live worker heartbeats preclude stale requeue.
   *
   * The worker-owned clone journal fences destructive reseeding after restart.
   * Shutdown retains an unverified clone and its session; cancellation and a
   * confirmed terminal state retain their existing cleanup semantics. The
   * clone-retention flag guards only the clone removal, never secret eviction,
   * registry cleanup or poller shutdown. Park acceptance uses the returned
   * recovery_wait status, including an idempotent 409 after a lost success ACK.
   */
  private async handleRecoveryExhausted(
    err: TransientRecoveryError,
    claim: ClaimResponse,
    flight: RunFlight,
    executor: Executor,
    batcher: MessageBatcher,
    reportState: (body: StateRequest) => Promise<StateAck>,
    runLog: Logger,
  ): Promise<boolean> {
    executor.killAgentTree?.();
    flight.preserveRecoveryClone = true;
    flight.preserveSession = true;
    let capture: { verified: boolean; published: boolean } | undefined;
    let notified = false;
    // #1539: the outcome of the cancel branch's pre-report reap, run ONCE while the run is
    // still actively-claimed. undefined = not yet attempted; true = reaped (settle after the
    // terminal report); false = a blocked/failed reap (RETAIN the hold, still report the cancel).
    let cancelReap: boolean | undefined;
    const terminal = TERMINAL_RUN_STATUSES;
    try {
      for (;;) {
        if (flight.active?.shuttingDown) return false;
        // A live heartbeat cannot requeue an abandoned running row. Keep this
        // execution and its steering poller active while retrying, and stop once
        // ownership or a real terminal state changes underneath it.
        let status: string;
        try {
          status = (await this.client.getRunOwnership(flight.runId)).status;
        } catch (probeError) {
          if (probeError instanceof RequestError && probeError.status === 404) return false;
          await this.waitRecoveryRetry(flight);
          continue;
        }
        if (status !== "running") {
          if (terminal.has(status)) {
            flight.preserveRecoveryClone = false;
            flight.preserveSession = false;
            // PRD #1349 M2 (D4.5) / #1539: a statusless cancel ack (HTTP 204), or a thrown cancel
            // report that actually LANDED, followed by a terminal ownership read arrives here with
            // the cancel branch's reap already done. Settle the exact-generation hold IFF that reap
            // confirmed (cancelReap) — the report is terminal now, so this is the deferred settle
            // that earlier reap earned. No reap here: the run is no longer actively-claimed.
            if (cancelReap) await this.settleRecoveryGeneration(claim, flight, runLog);
          }
          return status === "recovery_wait";
        }
        if (flight.steering.isCancelled()) {
          // PRD #1349 M2 (D4.5) / #1539: REAP THIS generation's provider FIRST, BEFORE the `failed`
          // report, while the run is still actively-claimed (the loop just verified status ===
          // "running"). A Codex run's pre-settle reap runs the per-sink credential reconcile inside
          // withBoundary (refreshCodex/releaseCodex), which the api authorizes ONLY while
          // actively-claimed (codexActivelyClaimedStatuses); once this report makes the run terminal
          // the reconcile is refused (409), which would block the reap and leak the exact-generation
          // hold as source_only. Reap ONCE, no retry: a blocked Codex reconcile poisons the registry
          // stickily (codex/registry.ts), so re-reaping cannot recover it. The killAgentTree at the
          // top of this handler reaps Claude/stub but is a NO-OP for Codex, so this reap is what
          // closes Codex admission. The credentialed settle runs AFTER the terminal report below (on
          // the ack, or on a later terminal ownership read via the early return above).
          if (cancelReap === undefined)
            cancelReap = await this.reapRecoveryProviderForSettle(claim, flight, runLog, "terminal");
          try {
            // Consuming cancel only stamps stop_kind. This existing terminal
            // report is what makes Service route it to CancelRunByWorker.
            const ack = await reportState({ status: "failed", failure_reason: "run cancelled" });
            if (ack.status && terminal.has(ack.status)) {
              flight.preserveRecoveryClone = false;
              flight.preserveSession = false;
              // PRD #1349 M2 (D4.5) / #1539: the provider was reaped above while actively-claimed;
              // now the terminal report has landed, so run the non-status-gated custody settle. A
              // verified-empty run RELEASES its exact hold, committed work CAPTURES it, and a
              // blocked/failed reap (cancelReap false) RETAINS the hold — the cancel is still
              // reported either way.
              if (cancelReap) await this.settleRecoveryGeneration(claim, flight, runLog);
              return false;
            }
            if (ack.status && ack.status !== "running") return false;
          } catch (cancelError) {
            runLog.warn("could not report recovery cancellation; retaining work and retrying", {
              error: errMessage(cancelError),
            });
          }
          // Do not busy-loop on the sticky cancel or its spent abort signal.
          // Shutdown can still stop the retry with clone and session retained.
          await this.waitRecoveryRetry(flight, false);
          continue;
        }
        if (!capture) {
          try {
            const attempt = await this.captureRecoveryRestorePoint(claim, flight, runLog);
            if (attempt.verified) {
              capture = attempt;
              flight.preserveRecoveryClone = false;
            }
          } catch (captureError) {
            runLog.warn("recovery capture failed; retaining work for retry", {
              error: errMessage(captureError),
            });
          }
          if (!capture) {
            if (!notified) {
              batcher.emit({
                kind: "status",
                agent: "worker",
                payload: {
                  text: "Recovery checkpoint could not be verified. Keeping the local work and session and retrying capture before automatic resume.",
                },
              });
              await batcher.flush().catch(() => undefined);
              notified = true;
            }
            await this.waitRecoveryRetry(flight);
            continue;
          }
        }
        // Cancellation/shutdown may have arrived during local git or publish.
        if (flight.active?.shuttingDown || flight.steering.isCancelled()) continue;
        try {
          const ack = await reportState({ status: "recovery_wait" });
          if (ack.status === "recovery_wait") {
            runLog.info("run parked for transient recovery", { detail: err.message });
            batcher.emit({
              kind: "status",
              agent: "worker",
              payload: {
                text: capture.published
                  ? "paused to recover from a transient interruption; the recovery checkpoint is published and it resumes automatically"
                  : "paused to recover from a transient interruption; the recovery checkpoint is saved on this worker and it resumes automatically",
              },
            });
            // PRD #1349 M2 (D3/D4): settle THIS generation's hold before the requeue. The
            // credentialed fresh-forge comparison MUST run reaped: the killAgentTree at the top of
            // this handler is a NO-OP for Codex (whose provider root is disposed only in
            // executeClaim's finally), so REAP FIRST (F2) — Codex-aware, idempotent with the reap
            // captureRecoveryRestorePoint already did. Then a verified-empty restore point releases
            // the exact hold (so a repeated verified-empty recovery_wait cycle grows no unresolved
            // custody), committed work is promoted into the generation-bound archive, and a
            // failed/unverifiable comparison retains. Runs AFTER the park ack lands, mirroring the
            // finalization terminal drive; best-effort so it never disturbs the reported park.
            await this.reapThenSettleRecoveryGeneration(claim, flight, runLog, "shutdown");
            return true;
          }
          if (ack.status && ack.status !== "running") {
            if (terminal.has(ack.status)) {
              flight.preserveRecoveryClone = false;
              flight.preserveSession = false;
            }
            return false;
          }
          // A statusless ACK (including HTTP204) proves neither a park nor a
          // terminal handoff. Retain the session and retry after re-reading
          // ownership: returning while it is still running would strand the row
          // because this healthy worker's heartbeats prevent stale-worker requeue.
        } catch (reportError) {
          // Bounded HTTP retries can fail while this worker keeps heartbeating.
          // Retain ownership and retry the idempotent park until its ACK is known.
          runLog.warn("could not report recovery park; retaining session and retrying", {
            error: errMessage(reportError),
          });
        }
        await this.waitRecoveryRetry(flight);
      }
    } finally {
      await batcher.close().catch(() => undefined);
    }
  }

  private async waitRecoveryRetry(flight: RunFlight, cancelStopsWait = true): Promise<void> {
    // Short slices also observe sticky cancellation after a pause consumed the
    // shared AbortController. An already-aborted pause signal must not busy-loop.
    let remaining = this.recoveryRetryMs;
    while (remaining > 0 && !flight.active?.shuttingDown
      && (!cancelStopsWait || !flight.steering.isCancelled())) {
      const slice = Math.min(250, remaining);
      await sleep(slice, flight.cancel.signal.aborted ? undefined : flight.cancel.signal);
      remaining -= slice;
    }
  }

  /**
   * PRD #1190 M2: park a run on an owner-requested pause (the ctx.parkForPause callback). Called
   * from the implement loop at the server-decided pause boundary and when a `now` pause aborted
   * the in-flight turn. Modeled on handleLimitReached, with two deliberate differences from a
   * limit park (Decision 8):
   *
   *   1. the checkpoint is published FIRST and the park happens ONLY if it lands — a pause whose
   *      committed work cannot be made durable on origin is a promise the resume cannot keep if
   *      the worker rolls, so staying running is the honest fallback; and
   *   2. a failed publish does NOT park — it reports `pause_failed` (the server clears the pending
   *      request and keeps the run running), tells the owner, and returns false so the implement
   *      loop CONTINUES the run (a `now` pause then restarts the aborted turn on the next
   *      iteration).
   *
   * Returns true once the run is durably parked (checkpoint on origin AND the server ACKed
   * `paused`, keyed off the RETURNED status being literally "paused" — the same ack contract as
   * the limit park, never off `applied`); false otherwise. The batcher is only FLUSHED here, never
   * closed: phasePublish closes it on the parked path, and the continue path keeps it open for the
   * rest of the run. NO reap and NO overlay — the run may CONTINUE, so the agent tree must stay
   * alive; the publish is on the credential-free join-token seam (checkpointPack local read →
   * client.publishCheckpoint), exactly like the reap:false mid-run checkpoint, so it is safe with
   * the agent alive.
   *
   * PRD #1171 m4 (pause-sink designation): this sink mints NO Codex permit and is DELIBERATELY
   * left entirely unchanged. It is a credential-free publish (overlay undefined) with no reap and
   * the agent alive — the SAME class as the reap:false checkpoint — so per the m4 structural rule
   * (a path mints a permit IFF it does a PAT-bearing overlay publish or a terminal/finalize reap)
   * it never touches the reapForSink/withCodexBoundaryOnly helpers.
   */
  private async handlePausePark(
    claim: ClaimResponse,
    flight: RunFlight,
    pausedAt: { completedCount: number; total?: number },
  ): Promise<boolean> {
    const { runLog, batcher, reportState } = flight;
    const barePath = flight.barePath;
    const runnerClone = flight.runnerClone;
    const branch = runnerClone?.branch ?? flight.branch;

    // Announce the park on the feed BEFORE the state report (same ordering as the limit park),
    // then FLUSH so the line lands ahead of it.
    batcher.emit({
      kind: "paused",
      agent: "worker",
      payload: {
        completed: pausedAt.completedCount,
        ...(pausedAt.total !== undefined ? { total: pausedAt.total } : {}),
      },
    });
    await batcher.flush().catch(() => undefined);

    // Make the clone's work durable in the BARE tracking ref BEFORE the publish. checkpointPack
    // (inside publishCheckpointBestEffort) packs refs/uzi-runner/<branch> in the bare, NOT the
    // runner clone's refs/heads/<branch>; before this fix handlePausePark published WITHOUT first
    // fetching the clone's newer committed work back into that ref (unlike the sibling limit-wait
    // park), so a `now` pause — or a milestone-boundary pause after commits since the last
    // fetch-back — packed a STALE tip while still recording the NEWER cloneTip below as
    // lastPublishedTip: the work between the two was lost on a cross-worker resume AND the
    // bookkeeping named a tip that was never published. Mirror the limit-wait park: capture any
    // uncommitted edits into a throwaway wip(park): marker, then fetch the clone tip back. NO reap
    // — the run may CONTINUE (Decision 8), so the agent tree must stay alive; the marker (runner-uid
    // local commit) and the fetch-back (file://) are credential-free, safe with the agent alive, the
    // same class as the mid-run reap:false checkpoint.
    //
    // The fetch-back is GATED on the clone carrying NEW work this cycle (its tip moved beyond the
    // reseed base, including a marker just made). When the clone is still AT its base — a `now` pause
    // before any commit, or a resume with no new work — it is DELIBERATELY skipped: an unconditional
    // fetch-back would create a spurious base tracking ref, and the broker would then publish a
    // base-only checkpoint (published:true) and PARK an empty pause, violating Decision 8. Skipping
    // it leaves the ref exactly as the reseed did (absent on a fresh run → checkpointPack null →
    // pause_failed; the recovered tracking tip on a resume → re-published as before), i.e. today's
    // behaviour. This is NOT the rejected base-tip SHORTCUT (which would SKIP the publish and CLAIM
    // durability, unsafe on the seededFrom:"tracking" leg): the publish below always runs; only the
    // redundant fetch-back is skipped when there is nothing new to move.
    let markerCreated = false;
    if (barePath && branch && runnerClone) {
      markerCreated = await this.git.commitWipMarker(runnerClone.path).catch(() => false);
      const preTip = await this.git
        .branchTip(runnerClone.path, branch)
        .catch(() => null);
      if (preTip !== null && preTip !== runnerClone.baseCommit) {
        await this.fetchBackBestEffort(
          barePath,
          runnerClone.path,
          branch,
          flight.runId,
          runLog,
        );
      }
      // PRD #1416 M3 (C7): bridge a divergent tracking tip BEFORE the pause-park publish so a resume
      // adopts B (the rewritten work), not the published tip. handlePausePark is checkpoint-first and
      // does NOT route through the doCheckpointPublish/reapForSink machinery C1/C5/C6 cover, so it is
      // wired here directly; the PauseNowSignal catch and the parkForPause callback both reach it, so
      // this one placement covers both. Best-effort — a park must never throw (D4).
      await this.bridgeParkSinkBestEffort(barePath, branch, flight, runLog, "pause-park");
    }

    // Checkpoint FIRST (Decision 8). An already-durable tip (a prior mid-run publish
    // confirmed-landed the committed work) is a successful pause with NO fresh pack — checkpointPack
    // would return null for an unmoved tip, which must NOT read as a publish failure. Otherwise
    // publish over the join-token seam and require a confirmed landing. An empty/unpublishable pack
    // (a `now` pause before any commit lands durably) does NOT get a clean-park shortcut: it falls
    // through to publishCheckpointBestEffort, whose false result yields pause_failed and keeps the
    // run running — no park without a durable checkpoint on origin (Decision 8). A base-tip shortcut
    // would be UNSAFE on the seededFrom:"tracking" resume leg, where baseCommit is the
    // locally-recovered tracking-ref tip and is durable on origin only if the prior park's
    // best-effort publish actually landed.
    let published = false;
    if (barePath && branch) {
      const cloneTip = runnerClone
        ? await this.git
            .branchTip(runnerClone.path, branch)
            .catch(() => null)
        : null;
      if (cloneTip !== null && cloneTip === flight.lastPublishedTip) {
        published = true;
      } else {
        published = await this.publishCheckpointBestEffort(
          flight,
          barePath,
          branch,
          undefined,
        );
        if (published) flight.lastPublishedTip = cloneTip ?? flight.lastPublishedTip;
      }
    }

    if (!published) {
      // The run CONTINUES (Decision 8), so a wip(park): marker we just made must not stay committed
      // at the clone HEAD: it would ride into the eventual MR and the restarted turn would build on a
      // throwaway commit. Restore its content to the uncommitted tree, the same shape the resume
      // adopt gives a marker. Best-effort; only when we actually created one this call.
      if (markerCreated && runnerClone) {
        await this.git.undoWipMarker(runnerClone.path).catch(() => undefined);
      }
      // Decision 8: no durable checkpoint ⇒ no park. Report pause_failed (the server clears the
      // pending request and keeps the run running), tell the owner the run is still running, and
      // return false so the loop CONTINUES — a `now` pause restarts the aborted turn on the next
      // iteration. The reason rides the worker's own feed message; the pause_failed report carries
      // none (the server intercepts pause_failed BEFORE its status switch).
      runLog.warn("could not publish a pause checkpoint; the run stays running", {
        run_id: flight.runId,
      });
      batcher.emit({
        kind: "pause_failed",
        agent: "worker",
        payload: {
          text: "Could not pause: the checkpoint could not be published. The run is still running and has restarted the interrupted step.",
        },
      });
      await reportState({ status: "pause_failed" }).catch((e) =>
        runLog.error("could not report pause_failed", { error: errMessage(e) }),
      );
      return false;
    }

    let ack: StateAck;
    try {
      ack = await reportState({ status: "paused" });
    } catch (e) {
      // The park report never landed. Not parked: the loop keeps running (its next report
      // self-heals), exactly as handleLimitReached cleans up when its park report throws.
      runLog.error("could not report the pause park; the run stays running", {
        error: errMessage(e),
      });
      return false;
    }
    if (ack.status !== "paused") {
      // The server did not park this run (e.g. it was cancelled concurrently). Key off the
      // RETURNED status being literally "paused", never off `applied` — the same ack contract as
      // the limit park. Cleaned up as unparked; the loop keeps running.
      runLog.warn("the server did not park this run on pause; the run stays running", {
        applied: ack.applied,
        server_status: ack.status ?? "unknown",
      });
      return false;
    }
    flight.parked = true;
    runLog.info("run paused at the owner's request; preserving its HOME for resume", {
      run_id: flight.runId,
    });
    // PRD #1349 M2 (D4): record this generation's checkpointed restore point in the durable
    // journal. This sink deliberately does NOT reap the agent tree (the run may CONTINUE — the
    // reap:false / no-overlay class), so it MUST stay credential-free: a PAT fresh-forge
    // comparison would violate the reap-before-credentialed-git invariant while the agent is
    // alive. Pin only (a local journal write over the credential-free join-token seam, the same
    // safety class as the checkpoint publish above); a resume mints a new generation hold, and
    // the paused generation's hold settles on its reaped resume-terminal or the reconciler.
    await this.pinRecoveryGeneration(
      claim,
      flight,
      await this.currentRestorePointHead(flight),
    ).catch((e) =>
      runLog.warn("recovery: pause generation-evidence pin failed (custody retained)", {
        run_id: flight.runId,
        error: errMessage(e),
      }),
    );
    return true;
  }

  /**
   * Capture dirty work as a WIP marker, fetch it into the worker-owned tracking
   * ref, and positively verify HEAD equality before attempting origin publish.
   * False/throw means the source clone remains the authoritative copy.
   */
  private async captureRecoveryRestorePoint(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
  ): Promise<{ verified: boolean; published: boolean }> {
    const barePath = flight.barePath;
    const worktreePath = flight.worktreePath;
    const branch = flight.branch;
    if (!barePath || !worktreePath || !branch) {
      runLog.warn(
        "recovery capture skipped: no clone paths on the flight; nothing to capture",
      );
      return { verified: false, published: false };
    }
    // Distinguish dirty vs clean EXPLICITLY (runner-uid porcelain) rather than trusting
    // commitWipMarker's ambiguous false (false = a clean tree OR a commit error).
    // worktreeStatus returns null on an UNREADABLE status → cannot assert clean → not
    // verified. Retain this clone and retry; a prior checkpoint may lack its work.
    const status = await this.git.worktreeStatus(worktreePath);
    if (status === null) {
      runLog.warn("recovery capture: worktree status unreadable (cannot assert clean)");
      return { verified: false, published: false };
    }
    if (status.length > 0) {
      // DIRTY → commit the WIP marker and REQUIRE it committed. Because we already know the
      // tree is dirty, a `false` here is unambiguously a commit FAILURE (not the clean-tree
      // no-op case), so the local restore point is not verified for this attempt.
      const committed = await this.git.commitWipMarker(worktreePath);
      if (!committed) {
        runLog.warn("recovery capture: WIP commit of a dirty tree failed");
        return { verified: false, published: false };
      }
    }
    // Fetch the run's tip into the worker bare's tracking ref (refs/uzi-runner/<branch>).
    // fetchAgentBranch THROWS on failure (unlike the void fetchBackBestEffort), so a
    // failed fetch-back is caught here rather than being swallowed.
    try {
      await this.git.fetchAgentBranch(barePath, worktreePath, branch, flight.runId);
    } catch (e) {
      runLog.warn("recovery capture: fetch-back failed", { error: errMessage(e) });
      return { verified: false, published: false };
    }
    // POSITIVELY VERIFY the LOCAL restore point: the bare's tracking ref now covers the
    // run's current HEAD (incl. any WIP marker), so a same-worker reseed recovers exactly
    // this tip. A clean tree whose already-committed tip already matches is a no-op success.
    const verified = await this.git.verifyRunnerTrackingCovers(
      barePath,
      worktreePath,
      branch,
    );
    if (!verified) return { verified: false, published: false };
    // PRD #1416 M3 (C8): bridge a divergent tracking tip AFTER the fetch-back + verify (so the verify
    // still confirms the tracking ref covered the run's HEAD H) and BEFORE the capture publish, so
    // the recovery restore point + its published checkpoint hold B — a reseed on resume adopts B
    // (which descends from P) instead of the rewritten H being set aside. Best-effort — a recovery
    // capture must never throw.
    await this.bridgeParkSinkBestEffort(barePath, branch, flight, runLog, "recovery-capture");
    // Remote publish is SEPARATE and best-effort. The agent tree was already reaped by the
    // caller (handleRecoveryExhausted's killAgentTree, untouched), so the overlay's PAT
    // default-fetch is permitted. publishCheckpointBestEffort surfaces the HTTP/skip outcome
    // (deduped); the caller's park notice states the durability consequence.
    //
    // PRD #1171 m4: for a Codex run the publish region runs under the finalize-class reap
    // facade (withCodexBoundaryOnly): the caller's reap is a no-op for Codex, so the boundary's
    // quiesce+reap (after its per-sink reconcile) is what closes admission before the
    // credentialed publish. captureRecoveryRestorePoint runs in handleRecoveryExhausted's retry
    // loop, so the boundary must be idempotent — a re-quiesce of a closed registry re-asserts
    // emptiness and a re-reap re-invokes each RegisteredRoot.reap (idempotent). A blocked Codex
    // boundary is a best-effort recovery path: it leaves `published` false (the restore point is
    // still VERIFIED locally, so the caller's "saved on this worker" notice fires) and must NOT
    // fail the capture. (Codex never actually reaches recovery in m4 — its executor throws no
    // TransientRecoveryError — so this is defensive wiring.)
    let published = false;
    try {
      await this.withCodexBoundaryOnly(
        flight.executor,
        { boundary: "shutdown", deadlineMs: this.codexBoundaryDeadlineMs },
        async (permit) => {
          const overlay = await this.buildCheckpointOverlay(claim, flight, barePath);
          published = await this.publishCheckpointBestEffort(flight, barePath, branch, overlay, permit?.signal);
        },
      );
    } catch (err) {
      if (!isCodexBoundaryError(err)) throw err;
      runLog.warn("recovery checkpoint boundary blocked; restore point saved locally but not published", {
        error: errMessage(err),
      });
    }
    return { verified, published };
  }

  /**
   * PRD #1226 M4 (D3/D6): enter the recoverable COMPLETION HOLD. Modeled on
   * handleRecoveryExhausted (the hold-loop template) with the D6 FIXED park order:
   *   1. reap the agent tree (REAP-BEFORE-GIT, before any credentialed capture git).
   *   2. set the preserve flags FIRST so no cleanup can strand the clone/session WHILE capture or the
   *      hold ACK is still uncertain — retain-everything is the default through the bounded capture
   *      retry and the hold request. It is held SET only on the true (parked) path; every false-
   *      return path CLEARS both flags before returning (mirroring handleRecoveryExhausted's non-
   *      parked branches), so a run that could NOT be parked follows NORMAL terminal cleanup instead
   *      of leaking its clone + HOME into a finally that skips cleanup while the flags are set.
   *   3-5. captureHoldContext (WIP-commit dirty → fetch-back → verifyRunnerTrackingCovers, REQUIRE
   *      verified; publish best-effort) with a bounded retry; DURING the retries a never-yet-verified
   *      capture retains the live clone (no hold requested), and only on GIVING UP does it clear the
   *      flags and return false so the subsequent terminal outcome cleans up.
   *   6. client.requestCompletionHold({head}) and REQUIRE the RETURNED status === "paused" (the
   *      positive-ack contract, the same as the pause park). A non-paused status or a thrown error
   *      clears the flags and returns false (the run is not parked).
   *   7. only after a paused ACK: mark the flight parked (so the finally's park carve-out preserves
   *      the HOME + plugin dir, the clone staying via preserveRecoveryClone) and return true.
   *
   * Returns true = ENTERED the verified hold (parked; phasePublish's completionHeld branch
   * finalizes nothing and the finally preserves the clone + HOME for a same-worker resume). Returns
   * false = could NOT capture a verified restore point OR the hold ACK was not `paused`; the flags
   * are CLEARED, so the run is NOT parked and its normal terminal outcome (the executor's legacy
   * throw, or phasePublish's typed failure) cleans up the clone + HOME like any other failed run.
   * `reason` is a content-free failure-reason constant, never raw model output.
   */
  private async enterCompletionHold(
    flight: RunFlight,
    claim: ClaimResponse,
    reason: string,
    runLog: Logger,
  ): Promise<boolean> {
    // 1. Reap the agent tree BEFORE any credentialed capture git (REAP-BEFORE-GIT), exactly like
    //    handleRecoveryExhausted. Idempotent — phasePublish's completionHeld branch reaps again.
    flight.executor.killAgentTree?.();
    // 2. Retain EVERYTHING up front: the runner clone is the only copy of the run's work until the
    //    tracking ref verifiably covers HEAD, and the session HOME must survive the same-worker
    //    resume. Set before any destructive possibility so an uncertain capture or a refused hold ACK
    //    can never strand the work WHILE the hold is being attempted (AC: no hold cleans the only Git
    //    work copy when capture or acknowledgement is uncertain). Held SET only on the true (parked)
    //    path below; EVERY false-return path clears both flags first (mirroring
    //    handleRecoveryExhausted's non-parked branches) so a run that could not be parked follows
    //    normal terminal cleanup rather than leaking its clone + HOME.
    flight.preserveRecoveryClone = true;
    flight.preserveSession = true;
    // 3-5. Capture a VERIFIED same-worker restore point, retrying bounded (like recovery's retain-
    //    and-retry, but bounded — this runs inside the executor's completion loop). DURING the retries
    //    an unverified capture retains the live clone and does NOT request a hold; only on GIVING UP
    //    does it clear the flags and return false so the subsequent terminal outcome cleans up.
    let captured:
      | { verified: boolean; published: boolean; mode: "same_worker_only"; head: string | null }
      | undefined;
    for (let attempt = 0; attempt < COMPLETION_HOLD_CAPTURE_ATTEMPTS; attempt++) {
      if (flight.active?.shuttingDown || flight.steering.isCancelled()) break;
      try {
        const result = await this.captureHoldContext(claim, flight, runLog);
        if (result.verified) {
          captured = result;
          break;
        }
      } catch (captureError) {
        runLog.warn("completion hold capture failed; retaining live work for retry", {
          error: errMessage(captureError),
        });
      }
      if (attempt < COMPLETION_HOLD_CAPTURE_ATTEMPTS - 1) await this.waitRecoveryRetry(flight);
    }
    if (!captured || captured.head === null) {
      // Gave up: no verified restore point after the bounded retries. Clear the preserve flags so the
      // NOT-parked run follows normal terminal cleanup instead of leaking its clone + HOME.
      flight.preserveRecoveryClone = false;
      flight.preserveSession = false;
      runLog.warn(
        "completion hold: restore point never verified; not parking (flags cleared for normal terminal cleanup)",
        { run_id: flight.runId },
      );
      return false;
    }
    // 6. Request the hold and REQUIRE the RETURNED status to be literally "paused" (the positive-
    //    ack contract, keyed off the returned status exactly like the pause park — never off
    //    `applied`). requestCompletionHold reads the status off BOTH a 200 and a 409 body without
    //    throwing; a non-paused status or a real transport error means the run is NOT parked, so we
    //    clear the flags (below) and it follows normal terminal cleanup.
    const head = captured.head;
    let status: string;
    try {
      ({ status } = await this.client.requestCompletionHold(flight.runId, {
        head,
        // PRD #1247 M5: stamp the claim-lane generation (the SAME value the reportState closure
        // stamps) so the server refuses to park a released/superseded stale flight's reclaimed run.
        claimGeneration: flight.claimGeneration,
      }));
    } catch (holdError) {
      flight.preserveRecoveryClone = false;
      flight.preserveSession = false;
      runLog.warn("completion hold: hold request failed; not parking (flags cleared for normal terminal cleanup)", {
        run_id: flight.runId,
        error: errMessage(holdError),
      });
      return false;
    }
    if (status !== "paused") {
      flight.preserveRecoveryClone = false;
      flight.preserveSession = false;
      runLog.warn("completion hold: server did not park the run; not parking (flags cleared for normal terminal cleanup)", {
        run_id: flight.runId,
        server_status: status || "unknown",
      });
      return false;
    }
    // 7. Durably held. Mark the flight parked so the finally's park carve-out preserves the HOME and
    //    plugin dir for the same-worker resume (the clone stays via preserveRecoveryClone, set
    //    above). phasePublish's completionHeld branch reaps again (idempotent) and skips finalize.
    flight.parked = true;
    runLog.info("run entered the recoverable completion hold; preserving its clone and HOME for resume", {
      run_id: flight.runId,
      reason,
      published: captured.published,
      head,
    });
    return true;
  }

  /**
   * PRD #1497 M2 (D4): enter the CAPTURE-FIRST WALL PARK — the runner side of ctx.parkForWall,
   * called when the executor's turn tripped at the run's wall-clock limit (a `wall` pause aborted
   * the turn, or a pre-attempt REASON_WALL fired, or a re-claim boundary with pause_mode='wall').
   * Modeled on enterCompletionHold (reap → set-preserve-flags-up-front → bounded VERIFIED capture via
   * the SHARED captureHoldContext), but with THREE differences the wall park requires:
   *
   *   - it reports the WALL_PARK transition (client.reportWallPark → fenced SetRunWallPark), not the
   *     completion hold;
   *   - a capture that never verifies does NOT abandon the park (D4): it parks DEGRADED — the flags
   *     stay set, the clone + HOME are retained, the head is reported EMPTY, and a feed message notes
   *     the latest local work is unverified and lives on this worker only. The wall park NEVER fails
   *     a run and never reports pause_failed (D2/D17);
   *   - it returns the richer WallParkOutcome the executor branches on (parked / undeliverable /
   *     refused / cancelled), not a bool.
   *
   * On "parked"/"undeliverable" the preserve flags STAY set (the run is non-terminal, clone + HOME
   * kept for resume); on "refused" (the owner extended in the window) and "cancelled" they are
   * CLEARED so the continuing/terminal run cleans up normally. This mints NO Codex permit itself —
   * captureHoldContext runs the credentialed publish under its own withCodexBoundaryOnly (harness-
   * aware), the SAME reuse the completion hold relies on.
   */
  private async enterWallPark(
    flight: RunFlight,
    claim: ClaimResponse,
    runLog: Logger,
  ): Promise<WallParkOutcome> {
    const { batcher } = flight;
    // 1. Reap the agent tree BEFORE any credentialed capture git (REAP-BEFORE-GIT), like the
    //    completion hold. Idempotent — phasePublish's walled branch reaps again.
    flight.executor.killAgentTree?.();
    // 2. Retain EVERYTHING up front (D4/D17): the wall park NEVER fails, so — unlike the completion
    //    hold, which clears the flags when it gives up — these STAY set through a degraded park and a
    //    landed/undeliverable park, and are cleared only when the server REFUSES (the owner extended,
    //    so the run continues) or the run was cancelled (terminal cleanup).
    flight.preserveRecoveryClone = true;
    flight.preserveSession = true;
    // 3-5. Capture a VERIFIED same-worker restore point via the SHARED captureHoldContext (do NOT
    //    fork a third WIP-commit-and-fetch-back — the #1190 stale-tip bug's lesson), bounded retry.
    let captured:
      | { verified: boolean; published: boolean; mode: "same_worker_only"; head: string | null }
      | undefined;
    for (let attempt = 0; attempt < COMPLETION_HOLD_CAPTURE_ATTEMPTS; attempt++) {
      if (flight.active?.shuttingDown || flight.steering.isCancelled()) break;
      try {
        const result = await this.captureHoldContext(claim, flight, runLog);
        if (result.verified) {
          captured = result;
          break;
        }
      } catch (captureError) {
        runLog.warn("wall park capture failed; retaining live work for retry", {
          error: errMessage(captureError),
        });
      }
      if (attempt < COMPLETION_HOLD_CAPTURE_ATTEMPTS - 1) await this.waitRecoveryRetry(flight);
    }
    const head = captured?.head ?? null;
    const published = captured?.published ?? false;
    if (head === null) {
      // DEGRADED park (D4): the capture could not be verified after the bounded retry. Park anyway —
      // the flags stay set (clone + HOME retained), the head is reported empty, and a feed message
      // tells the owner the latest local work is unverified and lives on this worker only.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "Reached the time limit and parked. The latest local work could not be verified and lives on this worker only — keep this worker until you decide.",
        },
      });
    }
    // 6. Report the WALL PARK. reportWallPark reads the status off BOTH a 200 (paused) and a 409
    //    (refused) body; a 404 (a reclaim — ErrRunNotOwned) or a transport/5xx error THROWS, which is
    //    an UNDELIVERABLE park (D17): keep the flags, report nothing terminal, end non-terminal.
    let status: string;
    let refresh: WallParkRefresh = {};
    try {
      let budgetTotalSeconds: number | undefined;
      let budgetUsedSeconds: number | undefined;
      ({ status, budgetTotalSeconds, budgetUsedSeconds } = await this.client.reportWallPark(flight.runId, {
        // Empty on a degraded park (the server column is nullable and treats "" as null).
        head: head ?? "",
        published,
        // PRD #1497 M1 (D16): stamp the claim-lane generation so the fence refuses a
        // released/superseded stale flight's reclaimed run (the SAME value the reportState closure stamps).
        claimGeneration: flight.claimGeneration,
      }));
      if (budgetTotalSeconds !== undefined) refresh.totalSeconds = budgetTotalSeconds;
      if (budgetUsedSeconds !== undefined) refresh.usedSeconds = budgetUsedSeconds;
    } catch (err) {
      runLog.warn(
        "wall park report undeliverable; retaining clone + HOME and ending the flight non-terminal (D17)",
        { run_id: flight.runId, error: errMessage(err) },
      );
      return "undeliverable";
    }
    if (status === "paused") {
      // 7. Durably parked. Mark the flight parked so the finally's carve-out preserves the HOME +
      //    plugin dir; preserveRecoveryClone (set above) keeps the clone. phasePublish's walled branch
      //    reaps again (idempotent) and skips finalize.
      flight.parked = true;
      runLog.info("run parked at its wall-clock limit; preserving clone + HOME for resume", {
        run_id: flight.runId,
        published,
        head,
        degraded: head === null,
      });
      return "parked";
    }
    if (status === "cancelled") {
      // The run was cancelled concurrently: it is terminal, nothing to resume, so CLEAR the preserve
      // flags for normal terminal cleanup. The executor ends the run as a cancel.
      flight.preserveRecoveryClone = false;
      flight.preserveSession = false;
      runLog.info("wall park found the run cancelled; ending the run as a cancel", {
        run_id: flight.runId,
      });
      return "cancelled";
    }
    // REFUSED (the owner extended in the request-then-park window, so the deadline moved and
    // SetRunWallPark returned 0 rows): the run CONTINUES. Clear the preserve flags — a continuing run
    // finalizes normally and its clone/HOME are cleaned up by the ordinary terminal path, not preserved.
    flight.preserveRecoveryClone = false;
    flight.preserveSession = false;
    // Issue #1600: keep the refusal's budget for the executor, which re-drives the turn without
    // passing reportIteration and would otherwise re-arm an exhausted wall.
    flight.wallParkRefresh = refresh;
    runLog.info("wall park refused (the owner extended in the window); the run continues", {
      run_id: flight.runId,
      server_status: status || "unknown",
    });
    return "refused";
  }

  /**
   * PRD #1247 M5b (D3, D13, D14): enter the held-state CREDENTIAL SWITCH release. Modeled
   * step-for-step on enterCompletionHold (runner.ts, the reap → set-preserve-flags-up-front →
   * bounded VERIFIED capture → positive-ack state machine), but its terminal outcome is a REQUEUE,
   * not a park:
   *
   *   1. Reap the agent tree BEFORE any credentialed capture git (REAP-BEFORE-GIT), like
   *      enterCompletionHold step 1 — idempotent, run()'s finally reaps again.
   *   2. Set preserveRecoveryClone + preserveSession up front (enterCompletionHold step 2) so a
   *      failed capture cannot strand the run's only copy of work while the release is uncertain.
   *   3-5. Capture a VERIFIED local restore point via captureRecoveryRestorePoint (dirty→WIP commit
   *      required, clean→clean proof, unreadable status→NOT verified; origin publish best-effort),
   *      bounded retry with waitRecoveryRetry between attempts — enterCompletionHold steps 3-5, but
   *      using captureRecoveryRestorePoint (the PRD's named capture) rather than captureHoldContext.
   *   6a. GIVE UP (never verified): report `credential_switch_failed` (best-effort; the server clears
   *      the switch STAMP but leaves the standing override), KEEP both preserve flags (NO committed
   *      work is lost), and return "gave_up". The caller leaves the run NON-TERMINAL for requeue,
   *      exactly like the worker-shutdown-interrupt arm; the standing override still points at the
   *      new token, so a same-worker reclaim resumes on it.
   *   6b. VERIFIED capture: DRAIN the batcher FIRST (the server's fenced append only persists while
   *      claim_released_at IS NULL, so pending messages MUST land before the release moves the run
   *      to `queued`), THEN report `credential_switch` and REQUIRE ack.status === "queued" — the
   *      positive-ack contract, exactly as enterCompletionHold requires "paused". A non-"queued" ack
   *      or a throw (incl. a stale_claim from the reportState closure) is a GIVE-UP: keep the flags,
   *      return "gave_up" — never assume released.
   *   7. Only after the queued ack: preserveRecoveryClone=false (the finally RETIRES the clone — a
   *      cross-worker reclaim re-clones and recovers from the durable tracking ref + best-effort
   *      origin; work is safe) and parked=true (preserve HOME + plugin dir for a same-worker
   *      resume); return "released".
   *
   * DELIBERATE INTERPRETATION vs the PRD's "continue on the old token in place": the SDK executor
   * has no primitive to re-drive an aborted turn IN PLACE, so this realizes the switch at the
   * RECLAIM boundary instead of mid-flight. The verified-capture gate plus the preserved clone/HOME
   * keep committed work safe on both outcomes, and the standing override makes the reclaim spend the
   * newly-chosen token — the codebase-idiomatic equivalent of "continue on the old token", with no
   * committed work lost. Strict continue-in-place would require resuming an aborted SDK session the
   * executor cannot resume in place.
   */
  private async enterCredentialSwitch(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
  ): Promise<"released" | "gave_up" | "retained_stop"> {
    // The generation this switch targets (equals flight.claimGeneration by construction — the
    // steering channel trips only on a generation match). Logged for provenance; the release
    // report's claim_generation is stamped by the reportState closure from flight.claimGeneration.
    const generation = flight.steering.pendingCredentialSwitch();
    // 1. Reap BEFORE any credentialed capture git (idempotent — run()'s finally reaps again).
    flight.executor.killAgentTree?.();
    // 2. Retain EVERYTHING up front so an uncertain capture cannot strand the only copy of work.
    flight.preserveRecoveryClone = true;
    flight.preserveSession = true;
    // 3-5. Capture a VERIFIED local restore point, bounded retry (recovery's retain-and-retry).
    let verified = false;
    for (let attempt = 0; attempt < CREDENTIAL_SWITCH_CAPTURE_ATTEMPTS; attempt++) {
      if (flight.active?.shuttingDown) break;
      try {
        const result = await this.captureRecoveryRestorePoint(claim, flight, runLog);
        if (result.verified) {
          verified = true;
          break;
        }
      } catch (captureError) {
        runLog.warn("credential switch capture failed; retaining live work for retry", {
          error: errMessage(captureError),
        });
      }
      if (attempt < CREDENTIAL_SWITCH_CAPTURE_ATTEMPTS - 1) await this.waitRecoveryRetry(flight);
    }
    if (!verified) {
      // 6a. GIVE UP — but only CONTINUE in place on a POSITIVE clear confirmation (BLOCKING-3
      // rework). Report credential_switch_failed and READ the ack: the server clears the stamp
      // only on a 200 (applied). If the clear is NOT confirmed — a not-applied 409, a network
      // error, or a stale_claim throw (the claim was superseded) — the switch stamp may still be
      // pending, and continuing would let the server's consume-nothing rule strand every
      // answer/follow-up for this claim forever. So RETAIN everything and STOP for requeue instead.
      // KEEP both preserve flags either way (no work loss).
      let cleared = false;
      try {
        const ack = await flight.reportState({ status: "credential_switch_failed" });
        cleared = ack.applied === true;
        if (!cleared) {
          runLog.warn("credential switch give-up: server did not confirm the stamp cleared; retaining and stopping", {
            run_id: flight.runId,
            server_status: ack.status ?? "unknown",
          });
        }
      } catch (e) {
        runLog.warn("credential switch give-up: could not confirm credential_switch_failed cleared the stamp; retaining and stopping", {
          run_id: flight.runId,
          error: errMessage(e),
        });
      }
      if (!cleared) {
        return "retained_stop";
      }
      // Positive clear confirmed: RE-ARM the steering channel so a LATER same-generation switch
      // request (the owner re-clicking "switch token" on the still-open claim) can trip again —
      // the once-only guard would otherwise drop it — then continue in place on the old token.
      flight.steering.rearmCredentialSwitch();
      runLog.warn(
        "credential switch: restore point never verified; the switch stamp is cleared, continuing on the old token (clone + HOME retained)",
        { run_id: flight.runId, generation },
      );
      return "gave_up";
    }
    // 6b. Verified. DRAIN the batcher FIRST — the fenced append persists only while
    // claim_released_at IS NULL, so every pending message MUST land BEFORE the release requeues the
    // run. close() is safe here (not a "reversible drain"): BOTH release outcomes below END this
    // flight — a confirmed release leaves the run for the reclaim, and an UNCONFIRMED release now
    // RETAINS-and-STOPS (it no longer continues in place on a claim the reclaim may already own) —
    // so a closed batcher is never continued past. Idempotent with the caller's own close().
    await flight.batcher.close().catch(() => undefined);
    // THEN report the RELEASE. Accept it off the server's RELEASED disposition — set on BOTH a
    // fresh requeue (status 'queued') AND an idempotent release after a reclaim (applied, status
    // 'running') — not off status === 'queued', which missed the idempotent-after-reclaim success
    // and gave up on a server-confirmed release, leaving the old flight to continue on a claim the
    // reclaim already owned. A throw or an unconfirmed release ⇒ retain-and-stop.
    let released = false;
    try {
      const ack = await flight.reportState({ status: "credential_switch" });
      released = ack.credentialSwitchReleased === true;
      if (!released) {
        runLog.warn("credential switch: server did not confirm the release; retaining and stopping", {
          run_id: flight.runId,
          server_status: ack.status ?? "unknown",
        });
      }
    } catch (releaseError) {
      runLog.warn("credential switch: release report failed; retaining and stopping", {
        run_id: flight.runId,
        error: errMessage(releaseError),
      });
    }
    if (!released) return "retained_stop"; // flags stay SET; STOP — never continue on a possibly-released claim
    // 7. Released. The work is durable on the worker tracking ref (+ best-effort origin), so RETIRE
    // the clone (a cross-worker reclaim re-clones); KEEP the HOME (parked) for a same-worker resume.
    flight.preserveRecoveryClone = false;
    flight.parked = true;
    runLog.info(
      "run released for a credential switch; retiring the clone, preserving HOME for resume",
      { run_id: flight.runId, generation },
    );
    return "released";
  }

  /**
   * PRD #1226 M4 (D6): capture a VERIFIED same-worker-only completion-hold restore point. Modeled
   * on captureRecoveryRestorePoint: worktreeStatus (null ⇒ unverified) → dirty? commitWipMarker
   * (require committed) → fetchAgentBranch (throws on fail) → verifyRunnerTrackingCovers (POSITIVE
   * verify) → publishCheckpointBestEffort (SEPARATE, best-effort). Returns an explicit
   * `same_worker_only` result whose `head` is the captured tracking tip (via git.trackingTip) after
   * a verified capture. `verified:false` (with `head:null`) means the source clone remains the only
   * authoritative copy — the caller retains it and never requests a hold. `published:false` is
   * acceptable (the local verified capture is what gates "captured"); a same-worker resume reseeds
   * from the worker-owned tracking ref, not from origin.
   */
  private async captureHoldContext(
    claim: ClaimResponse,
    flight: RunFlight,
    runLog: Logger,
  ): Promise<{ verified: boolean; published: boolean; mode: "same_worker_only"; head: string | null }> {
    const NONE = {
      verified: false,
      published: false,
      mode: "same_worker_only" as const,
      head: null,
    };
    const barePath = flight.barePath;
    const worktreePath = flight.worktreePath;
    const branch = flight.branch;
    if (!barePath || !worktreePath || !branch) {
      runLog.warn("completion hold capture skipped: no clone paths on the flight; nothing to capture");
      return NONE;
    }
    // Distinguish dirty vs clean EXPLICITLY (runner-uid porcelain) rather than trusting
    // commitWipMarker's ambiguous false. A null (unreadable) status cannot assert clean ⇒ not
    // verified: retain the clone and retry.
    const status = await this.git.worktreeStatus(worktreePath);
    if (status === null) {
      runLog.warn("completion hold capture: worktree status unreadable (cannot assert clean)");
      return NONE;
    }
    if (status.length > 0) {
      // DIRTY → commit the WIP marker and REQUIRE it committed; a false here is unambiguously a
      // commit FAILURE (we already know the tree is dirty), so the local restore point is not
      // verified for this attempt.
      const committed = await this.git.commitWipMarker(worktreePath);
      if (!committed) {
        runLog.warn("completion hold capture: WIP commit of a dirty tree failed");
        return NONE;
      }
    }
    // Fetch the run's tip into the worker bare's tracking ref (refs/uzi-runner/<branch>).
    // fetchAgentBranch THROWS on failure (unlike the void fetchBackBestEffort), so a failed
    // fetch-back is caught here rather than swallowed.
    try {
      await this.git.fetchAgentBranch(barePath, worktreePath, branch, flight.runId);
    } catch (e) {
      runLog.warn("completion hold capture: fetch-back failed", { error: errMessage(e) });
      return NONE;
    }
    // POSITIVELY VERIFY the LOCAL restore point: the bare's tracking ref now covers the run's
    // current HEAD (incl. any WIP marker), so a same-worker reseed recovers exactly this tip.
    const verified = await this.git.verifyRunnerTrackingCovers(barePath, worktreePath, branch);
    if (!verified) return NONE;
    // PRD #1416 M3 (C8): bridge a divergent tracking tip AFTER the fetch-back + verify (so the verify
    // still confirms the tracking ref covered H) and BEFORE the head is read + published, so a
    // credential-switch release captures B, not the rewritten H — the completion-hold head and the
    // published checkpoint both carry B, and a same-worker reclaim's reseed adopts it. Best-effort.
    await this.bridgeParkSinkBestEffort(barePath, branch, flight, runLog, "hold-capture");
    // The captured head is the verified tracking tip. A verified ref whose tip is unresolvable is
    // treated as unverified (retain) — the hold contract requires a real head H for the permit.
    const head = await this.git.trackingTip(barePath, branch);
    if (head === null) {
      runLog.warn("completion hold capture: tracking ref verified but its tip is unresolvable");
      return NONE;
    }
    // Remote publish is SEPARATE and best-effort. The agent tree was already reaped by the caller
    // (enterCompletionHold's killAgentTree), so the overlay's PAT default-fetch is permitted; for a
    // Codex run the publish runs under the finalize-class boundary facade (withCodexBoundaryOnly),
    // idempotent like the recovery path. A blocked boundary leaves `published` false — the restore
    // point is still VERIFIED locally, which is what gates "captured".
    let published = false;
    try {
      await this.withCodexBoundaryOnly(
        flight.executor,
        { boundary: "shutdown", deadlineMs: this.codexBoundaryDeadlineMs },
        async (permit) => {
          const overlay = await this.buildCheckpointOverlay(claim, flight, barePath);
          published = await this.publishCheckpointBestEffort(flight, barePath, branch, overlay, permit?.signal);
        },
      );
    } catch (err) {
      if (!isCodexBoundaryError(err)) throw err;
      runLog.warn("completion hold checkpoint boundary blocked; restore point saved locally but not published", {
        error: errMessage(err),
      });
    }
    return { verified: true, published, mode: "same_worker_only", head };
  }

  /**
   * Parse the clone's `.claude/agents/*.md` (PRD #37), logging every skipped or
   * clamped file to the run stream. Detection is best-effort by construction: a
   * repo without the directory has no agents (ok: true, agents: []), and an
   * enumeration FAILURE (e.g. an unreadable dir) is reported as a warning and a run
   * message with ok: false — neither is ever a run failure, because a repo's
   * optional roster must not be able to stop the run it was detected for.
   *
   * The `ok` flag is what keeps a detection FAILURE distinguishable from a genuinely
   * empty roster at the API: the caller reports `repo_agents: []` only when ok, so a
   * failure leaves the column NULL ("not reported") rather than `[]` ("found none").
   */
  private async parseRepoAgents(
    worktreePath: string,
    batcher: MessageBatcher,
    runLog: Logger,
  ): Promise<{ agents: AgentTemplate[]; ok: boolean }> {
    let detected;
    try {
      detected = await this.detect(worktreePath);
    } catch (err) {
      runLog.warn("repo agent detection failed", { error: errMessage(err) });
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: "could not read the repo's .claude/agents/; continuing with your own agent templates",
        },
      });
      return { agents: [], ok: false };
    }
    for (const note of detected.notes) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: describeRepoAgentNote(note) },
      });
    }
    if (detected.agents.length > 0) {
      const names = detected.agents.map((a) => a.name).join(", ");
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `detected ${detected.agents.length} agent(s) in the repo's .claude/agents/: ${names}`,
        },
      });
    }
    runLog.info("repo agents detected", {
      count: detected.agents.length,
      dropped: detected.notes.length,
    });
    return { agents: detected.agents, ok: true };
  }

  /**
   * Seed the run's RUNNER CLONE + resolve its branch (PRD #51 M3, (b)). An issue run
   * works agent/issue-{iid}. A ci_fix run (PRD #6) targets a fresh
   * `ci-fix/pipeline-{id}` branch when fixing the default branch, or the existing
   * agent branch (updating its MR) when fixing a run branch — keyed by pipeline.ref
   * vs the repo's default branch. The working tree lives ONLY in this clone; the
   * worker fetches the agent branch back from it before pushing (fetchAgentBranch).
   */
  private async runnerCloneForClaim(barePath: string, claim: ClaimResponse) {
    // PRD #218 M2: thread the run id as the tracking-ref OWNERSHIP anchor. The git layer
    // stays claim-agnostic — it consults the tracking ref only when its stamp matches
    // this run id, so neither a fresh run nor a different run on the same issue can
    // inherit a dead run's orphan ref. A requeue/resume keeps the same run_id, so a run
    // resuming its OWN parked work matches; every other run does not.
    const runId = claim.run_id;
    // PRD #1030 M3: a SEPARATE, additional signal (not the runId ownership anchor) — a
    // RESUME is `claim.session_id != null` (a run that executed before, re-claimed after a
    // rate-limit park), distinct from a fresh first attempt or a seeded run (session_id
    // null). Threaded into the clone path to relax ONLY the cross-worker checkpoint
    // ancestry test on an unpushed branch (see runnerCloneForBranch): when `main` advanced
    // during the park, a valid mirrored checkpoint diverges from the moved default and the
    // strict-descendant test would discard the committed milestones and cold-start.
    const resume = claim.session_id != null;
    // issue #1042 M4: the OWNER ANCHOR for the cross-worker checkpoint adopt guard — the tip
    // THIS run last published to its own refs/uzi-checkpoints/<branch> ref, persisted
    // server-side on every publish (M2) and delivered on the claim. runnerCloneForBranch
    // adopts a mirrored checkpoint on the resume/unpushed leg ONLY when this equals the
    // checkpoint's current SHA, so a resumed run that never published its own checkpoint
    // cannot seed off a PRIOR (possibly plan-rejected) run's work. `?? undefined` maps the
    // wire's null (a never-published run) to the "do not adopt" sentinel the git layer reads.
    const expectedCheckpointTip = claim.checkpoint_tip ?? undefined;
    // PRD #983 M4b: the per-kind branch derivations (ci_fix's default-branch vs run-branch
    // choice, self_improve/prompt's fresh-per-cycle run-id branch, task/mr_rework's
    // pre-seeded branch with its loud missing-branch guard) live in RUN_KIND_PROFILES. A
    // row that returns undefined — ci_fix with no pipeline, and the issue/chat/judge kinds
    // that have no cloneBranch — falls through to the issue path below, byte-identically.
    const cloneBranch = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].cloneBranch?.(
      claim,
      runId,
    );
    if (cloneBranch)
      return this.git.runnerCloneForBranch(
        barePath,
        cloneBranch.branch,
        cloneBranch.slug,
        runId,
        resume,
        expectedCheckpointTip,
      );
    if (claim.issue_iid == null)
      throw new Error("issue run claim is missing issue_iid");
    return this.git.createOrAttachRunnerClone(barePath, claim.issue_iid, runId, resume, expectedCheckpointTip);
  }

  /** Post awaiting_approval with the plan and await the steering verdict, bounded.
   *  For an autopilot run, the plan is still recorded but the gate resolves with an
   *  approve verdict immediately — no awaiting_approval report, no /inputs wait. It
   *  also resolves + records the run's DEFAULT agent selection (PRD #37 Decision 6),
   *  since a self-approved run never receives an approve_plan input to carry one.
   *
   *  PRD #41 plan revision: this is called ONCE PER ROUND by the executor's gate loop.
   *  A RE-gate (every round after the first) bumps the steering epoch at the re-report,
   *  so a verdict written against the previous plan version goes stale; the first gate
   *  does not bump (epoch 0). Each call then awaits the epoch-aware event. A `revise`
   *  verdict is RETURNED
   *  to the caller — the executor runs a fresh plan turn with the feedback and calls
   *  gatePlan again; approve/reject/cancel are terminal. The 24h approval budget is an
   *  ABSOLUTE deadline computed on the first entry and threaded across rounds, so N
   *  revision rounds share ONE budget rather than resetting the clock each round. The
   *  autopilot short-circuit is unchanged and never returns a revise. */
  private async gatePlan(
    runId: string,
    planMd: string,
    // PRD #122 M1: the CANDIDATE milestone list from the just-submitted plan. Rides
    // the awaiting_approval report (human-gated) or the autopilot running report as
    // the FROZEN list (Decision 2). Additive-optional — only included when non-empty.
    milestones: Milestone[] | undefined,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    // Ignored by the gate, but typed to match the client (PRD #35): a gate that
    // narrowed the return would make the park branch unable to reuse this closure.
    reportState: (body: StateRequest) => Promise<StateAck>,
    runLog: Logger,
    autoApprove: boolean,
    repoAgents: AgentTemplate[],
    // PRD #84 M4: the plan-time inferred requirement set (deterministic detectToolchain()),
    // or undefined when the scan failed. Rides the same two reports as `milestones` — the
    // CANDIDATE set on the awaiting_approval report and the FROZEN set on the autopilot
    // running report — each field emitted only when its array is non-empty.
    toolchainDetection: ToolchainDetection | undefined,
    // PRD #212: the runner clone path (= runnerClone.path), so the gate can run a
    // runner-uid `git status --porcelain` there to surface plan-turn worktree writes.
    worktreePath: string,
    // PRD #1416 M5: the branch's published floor P (flight.publishedTip), or undefined for a
    // fresh, never-published branch. When set AND the submitted plan proposes rewriting history,
    // the gate emits a warn-only status nudge (both modes) and, under auto-approve, arms the M2
    // safety steer for the first implement turn. The verdict flow is untouched (never rejects).
    publishedTip: string | undefined,
    // PRD #362 M3c: advisory hook fired AFTER the awaiting_approval report persists
    // plan_md, BEFORE the verdict wait (see the RunContext.gatePlan doc). Never invoked
    // on the autopilot branch: that branch DOES persist plan_md durably (RC1 #1197, via
    // its running report / SetRunAutopilotPlan) but never invokes onAwaitingApproval, so
    // it still generates no plan summary.
    onAwaitingApproval?: (planMd: string) => Promise<void>,
  ): Promise<PlanVerdict> {
    batcher.emit({ kind: "plan", agent: "lead", payload: { plan_md: planMd } });
    // Get the plan message onto the stream regardless of mode — it is the audit
    // record of what the agent intended, autopilot or not.
    await batcher.flush().catch(() => undefined);

    // PRD #1416 M5 (D5): a WARN-ONLY plan-gate nudge. When the branch has a published floor P and
    // the submitted plan proposes rewriting history (a case-insensitive regex scan over plan_md),
    // emit ONE visible `status` run message right after the plan — in BOTH modes, so a human sees
    // it next to the plan and an auto-approved run still records it. The verdict flow is untouched:
    // this never rejects and never blocks the plan (false positives are accepted, D5). The steer is
    // armed under auto-approve only, below.
    const proposesRewrite = !!publishedTip && planProposesRewrite(planMd);
    if (proposesRewrite) {
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `the plan proposes rewriting history on a branch published at ${publishedTip!.slice(0, 12)}; the worker lands fast-forward only and cannot land a rewritten branch — prefer \`git merge\``,
        },
      });
    }

    if (autoApprove) {
      // Auto-approve is a VERDICT SOURCE at the existing gate, not a bypass around
      // it: the plan was recorded above; the run just never enters awaiting_approval
      // (no state flicker, no column-automation churn) and never waits on a human.
      // The default selection (repo agents when detected, else the owner's
      // templates) is resolved here, persisted via the running report (the only
      // channel a no-input run has, Decision 6), and stated on the feed. The
      // executor re-resolves the SAME absent-parse to build the identical roster.
      const selection = resolveAgentSelection(
        { status: "absent" },
        repoAgents.length > 0,
      ).selection;
      // Make this report self-contained (F1): carry the roster alongside the selection
      // so both validate + persist atomically, even if the fire-and-forget running
      // roster report above failed and left the column NULL. Gated on length > 0 — on
      // a detection failure repoAgents is [] and the selection resolves to `own`;
      // sending repo_agents: [] would flip NULL ("not reported") to [] ("detected
      // none") and break that deliberate distinction. rosterFor already prefers the
      // reported roster over the column, so this needs no wire change.
      const autopilotState: StateRequest = {
        status: "running",
        agent_selection: selection,
      };
      if (repoAgents.length > 0)
        autopilotState.repo_agents = repoAgentSummaries(repoAgents);
      // PRD #122 M1 (Decision 2): an autopilot run never reports awaiting_approval, so
      // the FROZEN milestone list rides this self-contained running report instead —
      // mirroring the repo_agents conditional above. Only when non-empty (additive-
      // optional, never []). Reporting stays fire-and-forget via the .catch below.
      if (milestones?.length) autopilotState.milestones = milestones;
      // PRD #84 M4: the FROZEN requirement set rides this same self-contained running
      // report (an autopilot run never reports awaiting_approval), each field only when
      // non-empty — same conditional-spread discipline as milestones/repo_agents above.
      Object.assign(autopilotState, toolchainReportFields(toolchainDetection));
      // RC1 (#1197): the approved autopilot plan rides this running report so the
      // server persists it durably via the guarded SetRunAutopilotPlan write — an
      // autopilot run never reports awaiting_approval, the channel the human-gated
      // branch below uses to persist plan_md. This is the durable plan the resume
      // guard reads; without it a resumed autopilot run re-plans instead of
      // implementing (the RC1 incident).
      autopilotState.plan_md = planMd;
      // AWAIT the ack and gate the approve on PROVEN storage — no `.catch` swallow.
      // The ack is the storage proof: HTTP 200 ⟺ the plan is durably stored, because
      // the server errors BEFORE the running write on a 0-row plan refusal (the guarded
      // write). So a transport/4xx failure THROWS out of reportState (and out of
      // gatePlan) — the run must NOT enter implementation with no durable plan; it
      // propagates to execute()'s existing pre-implementation failure handling (no new
      // checkpoint-preservation logic here). A 409 (applied === false) means the run
      // moved on — cancelled/parked concurrently — so the plan was NOT stored; throw
      // here rather than returning an approve verdict. Only a 200 (applied) proceeds.
      const ack = await reportState(autopilotState);
      if (!ack.applied) {
        throw new Error(
          `autopilot plan not durably stored — the run is ${ack.status ?? "no longer running"}`,
        );
      }
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: autopilotSelectionText(selection, repoAgents.length) },
      });
      runLog.info("plan gate: auto-approved (autopilot)", {
        run_id: runId,
        agent_source: selection.source,
      });
      // PRD #1416 M5: on an AUTO-APPROVED run the human never sees the status nudge emitted above,
      // so ALSO arm the M2 worker-authoritative safety steer with a plan-time PREVENTIVE body
      // (composePlanGateNudge — distinct from composeSafetySteer, which references an already-
      // rewritten tip H that does not exist yet at plan time). Both executors drain pullSafetySteer
      // at their loop top, ahead of any follow-up and BEFORE the FIRST buildImplementPrompt, so the
      // first implement turn is reminded not to rewrite at/below P. NOT armed on the human-gated
      // branch below (a human sees the plan + the nudge and can revise/reject). The verdict is
      // UNCHANGED — this arms guidance beside the approve, it does not alter it.
      if (proposesRewrite) steering.pushSafetySteer(composePlanGateNudge(publishedTip!));
      return { kind: "approve", selection: { status: "ok", selection } };
    }

    // PRD #41 round-awareness (Decision 3): the gate epoch is advanced at the
    // awaiting_approval RE-report — the last step before awaiting a new round — NOT
    // when a revise is taken. A revision planning turn runs BETWEEN rounds; bumping
    // early would leave that whole window at the new epoch, so an approve clicked
    // mid-revision would be accepted at the v2 gate (a plan no human saw). Bumping
    // here, after v2 is reported, stamps such a mid-revision approve at the PRIOR
    // epoch, so it is discarded with a feed notice. The FIRST gate for a run does
    // NOT bump: epoch 0 lets a verdict already queued when the gate opens apply.
    // PRD #122 M1: the CANDIDATE milestone list rides the awaiting_approval report so
    // the human approves the breakdown (Decision 2). Only include the key when
    // non-empty — additive-optional, never `null`/`[]`, so a no-milestone run's report
    // stays byte-for-byte as today. The api freezes candidate→frozen at approve.
    // PRD #212: surface the plan turn's worktree writes at the gate. Runner-uid status,
    // best-effort (planChangedFiles swallows errors → []), computed EVERY round so a
    // revision gate reflects that round's tree (a revert between rounds clears the list).
    const planChangedFiles = await this.git.planChangedFiles(worktreePath);
    await reportState({
      status: "awaiting_approval",
      plan_md: planMd,
      ...(milestones?.length ? { milestones } : {}),
      // PRD #84 M4: the CANDIDATE requirement set rides the awaiting_approval report so
      // the server can gate plan-approval on worker eligibility. Each field only when
      // non-empty (additive-optional), matching the milestones conditional above.
      ...toolchainReportFields(toolchainDetection),
      // PRD #212 (Decision 3): ALWAYS send (empty [] when clean), NOT conditionally
      // spread — so each gate round REPLACES the server's list (M1's COALESCE clears on
      // empty), keeping a revision gate from showing a stale earlier round's writes.
      plan_changed_files: planChangedFiles,
    });
    if (this.gatedRuns.has(runId)) steering.bumpEpoch();
    else this.gatedRuns.add(runId);
    const epoch = steering.currentEpoch();
    runLog.info("plan gate: awaiting approval", {
      run_id: runId,
      gate_epoch: epoch,
    });

    // PRD #362 M3c: plan_md is now persisted (the awaiting_approval report above), so the
    // plan-summary POST's stale-write guard (`plan_md = @expected`) can match. Fire the
    // hook HERE — after persist, before the verdict wait — never on the autopilot branch,
    // which returned above. That branch DOES persist plan_md durably (RC1 #1197, via
    // SetRunAutopilotPlan) but never invokes this hook, so it generates no plan summary.
    // ADVISORY: swallow any throw so a
    // summary failure can never wedge the gate or change the run's outcome (the hook
    // itself also swallows internally; this is belt-and-suspenders).
    if (onAwaitingApproval) {
      try {
        await onAwaitingApproval(planMd);
      } catch (e) {
        runLog.warn("plan summary hook failed", {
          run_id: runId,
          error: errMessage(e),
        });
      }
    }

    // A terminal verdict ends the gate → clear the shared per-run gate state. A revise
    // keeps the shared budget/epoch state running; the re-report above does the bump.
    const settle = (v: PlanVerdict): PlanVerdict => {
      if (v.kind !== "revise") {
        this.gateDeadlines.delete(runId);
        this.gatedRuns.delete(runId);
      }
      return v; // NOTE: no bump here — the awaiting_approval re-report bumps.
    };

    if (this.planApprovalTimeoutMs <= 0)
      return settle(await steering.awaitGateEvent(epoch));

    // One absolute deadline across all revision rounds: set it on the first entry and
    // reuse it, so the per-round timer counts down the REMAINING budget (not a fresh 24h).
    let deadlineAt = this.gateDeadlines.get(runId);
    if (deadlineAt === undefined) {
      deadlineAt = Date.now() + this.planApprovalTimeoutMs;
      this.gateDeadlines.set(runId, deadlineAt);
    }
    const remaining = Math.max(0, deadlineAt - Date.now());
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<PlanVerdict>((resolve) => {
      timer = setTimeout(
        () => resolve({ kind: "reject", reason: "plan approval timed out" }),
        remaining,
      );
      timer.unref?.();
    });
    try {
      return settle(
        await Promise.race([steering.awaitGateEvent(epoch), timeout]),
      );
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /**
   * PRD #88 M1 clarification park: emit the question, park the run at
   * awaiting_input, and resolve with the human's answer.
   *
   * Structured to mirror gatePlan, including the ABSOLUTE deadline shared across all
   * of a run's questions. Three things differ, each for a stated reason:
   *
   *  - The question id is minted ONCE per park and REUSED on a resume re-park (seeded
   *    from the claim). Everything about the stale-answer guard rests on that
   *    stability, on both sides of the wire.
   *  - The question message is emitted and flushed BEFORE the state report, so the
   *    question is durable before any surface learns the run parked. A surface that
   *    saw awaiting_input with no question yet would render a park it cannot explain.
   *  - Timeout FAILS the run ("clarification timed out") rather than resolving with a
   *    verdict. The PRD puts a configurable default-action out of scope, so
   *    fail-closed is the fixed choice.
   */
  private async askUser(
    runId: string,
    questions: AskUserQuestion[],
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<unknown>,
    runLog: Logger,
    autoApprove: boolean,
    config: ClaimConfig | null,
  ): Promise<AnswerVerdict> {
    if (autoApprove) {
      // Autopilot is "no human in the loop" (PRD #19), so a park would wedge the run
      // until its deadline with nobody to answer. Resolve immediately with a sentinel
      // and DO NOT report awaiting_input — an autopilot run must never enter the
      // parked state at all, which is what a test can actually observe.
      batcher.emit({
        kind: "status",
        agent: "worker",
        payload: { text: AUTOPILOT_ANSWER_NOTICE },
      });
      runLog.info("clarification: auto-resolved (autopilot)", {
        run_id: runId,
        questions: questions.length,
      });
      return {
        kind: "answer",
        answers: questions.map(() => AUTOPILOT_SENTINEL_ANSWER),
      };
    }

    return this.parkQuestion(
      runId,
      questions,
      "lead",
      undefined,
      batcher,
      steering,
      reportState,
      runLog,
      config,
    );
  }

  /**
   * Issue #1593: the worker-authored plan-missing park. An executor calls this after a
   * planning turn ended in prose only even after a corrective nudge. The question is FIXED
   * (plan-missing.ts) and emitted as the WORKER with `plan_missing: true` and no options; no
   * model text is passed in, so the lead's prose cannot reach the question. The lead's last
   * message reaches the owner only on the executor's status card, as untrusted data.
   *
   * Shares askUser's park body, so the id, the ack check, the settle `running` report and the
   * shared per-run answer deadline (REASON_QUESTION_TIMEOUT) are identical. It does not count
   * against question_max; that cap lives in the executors. An autopilot run never parks: it
   * returns `unattended` WITHOUT reporting awaiting_input, and the executor fails the run.
   */
  private async askPlanMissing(
    runId: string,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<unknown>,
    runLog: Logger,
    autoApprove: boolean,
    config: ClaimConfig | null,
  ): Promise<{ kind: "answer"; answers: string[] } | { kind: "cancel" } | { kind: "unattended" }> {
    if (autoApprove) {
      runLog.info("plan missing: not parked (autopilot)", { run_id: runId });
      return { kind: "unattended" };
    }
    const v = await this.parkQuestion(
      runId,
      [{ header: PLAN_MISSING_QUESTION_HEADER, question: PLAN_MISSING_QUESTION }],
      "worker",
      { plan_missing: true },
      batcher,
      steering,
      reportState,
      runLog,
      config,
    );
    return v.kind === "answer" ? { kind: "answer", answers: v.answers } : { kind: "cancel" };
  }

  /**
   * The clarification park body shared by askUser and askPlanMissing (issue #1593): emit the
   * question as `emitAgent`, park at awaiting_input, and resolve with the human's answer.
   * `extraPayload` is merged AFTER `{question_id, questions}`, so askUser (no extra payload)
   * emits exactly the message it always has. See askUser's doc for the ordering, the ack
   * check and the deadline.
   */
  private async parkQuestion(
    runId: string,
    questions: AskUserQuestion[],
    emitAgent: "lead" | "worker",
    extraPayload: Record<string, unknown> | undefined,
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<unknown>,
    runLog: Logger,
    config: ClaimConfig | null,
  ): Promise<AnswerVerdict> {
    const ordinal = (this.questionCounts.get(runId) ?? 0) + 1;
    this.questionCounts.set(runId, ordinal);

    // Reuse the id this run is already parked on (a resume re-parks on the SAME
    // question); mint one only for a genuinely new question.
    let questionId = this.openQuestionIds.get(runId);
    if (questionId === undefined) {
      questionId = randomUUID();
      this.openQuestionIds.set(runId, questionId);
    }

    batcher.emit({
      kind: "question",
      agent: emitAgent,
      payload: { question_id: questionId, questions, ...extraPayload },
    });
    // Durable before the park is announced — see the doc comment.
    await batcher.flush().catch(() => undefined);

    // Read the ACK, and read it the way PRD #35 established for its own park:
    // `status === "awaiting_input"`, NEVER `applied`. A declined park can still have
    // applied a different transition, so `applied` is true while the park did not
    // happen — StateAck.applied's own doc says diagnostics only.
    //
    // Why this matters here and not before the merge: pre-#35 reportState returned
    // void, so discarding it was correct. The information now exists, and without
    // this check the two parks in this file would be asymmetric — the limit park
    // verifies, the question park does not — which reads as an oversight rather than
    // a decision.
    //
    // What the check buys: SetRunAwaitingInput matches nothing when the run went
    // terminal under us or is no longer ours. Without it the worker would then await
    // an answer NO SURFACE CAN PRODUCE — the status never changed, so no UI, Slack
    // card or CLI ever shows a question — and the run would sit until
    // QUESTION_TIMEOUT and die claiming a human ignored it. Failing here instead
    // turns a silent 24h wait into an immediate, explained failure.
    //
    // A concurrent cancel is the one decline that resolves itself (the steering
    // channel's sticky `cancelled` flag settles the wait independently), so this is
    // belt-and-braces on that path and the only defence on the others.
    const ack = await reportState({
      status: "awaiting_input",
      open_question_id: questionId,
    });
    const parked = (ack as { status?: string } | undefined)?.status;
    if (parked !== "awaiting_input") {
      this.openQuestionIds.delete(runId);
      throw new Error(
        `${REASON_QUESTION_NOT_PARKED} (server reports ${parked ?? "an unreadable status"})`,
      );
    }
    runLog.info("clarification: awaiting answer", {
      run_id: runId,
      question_id: questionId,
      question_ordinal: ordinal,
    });

    // LOAD-BEARING, and it reads as incidental — hence this note. Dropping the open
    // question id here is what makes each park mint a fresh randomUUID, and that in
    // turn is the ONLY reason SubmitInput's "reject an answer unless the run is
    // awaiting_input" check can be described as belt-and-braces rather than as the
    // primary defence. Its server-side counterparts are the two clears in
    // runtime.sql (SetRunRunning and SetRunAwaitingApproval); this is the worker's
    // half of the same invariant — no resolved id is left behind anywhere.
    const settle = (v: AnswerVerdict): AnswerVerdict => {
      this.openQuestionIds.delete(runId);
      if (v.kind === "answer") {
        // PRD #307: a plan-phase clarification resumes into more PLANNING turns, so
        // neither onSessionId (latched once per run) nor reportIteration (implement
        // loop only) fires again — the run would stay stuck at awaiting_input. Emit a
        // `running` report on the answer so the server runs SetRunRunning, which is the
        // one existing transition that clears open_question_id (its consumed-answer
        // guard already passes: ConsumeRunInputs stamps consumed_at in the same
        // RETURNING statement that handed us this answer). Shared by the implement-phase
        // park too: there this same awaiting_input -> running is the one the loop's next
        // reportIteration would otherwise perform, so it is harmless (and equally relies
        // on the consumed-answer guard — it is NOT a running -> running no-op, because the
        // awaiting_input park report is awaited and persisted before settle runs). NOT
        // emitted on cancel/timeout (timeout throws before settle; cancel is guarded out).
        void reportState({ status: "running" }).catch((e) =>
          runLog.warn("could not report running after clarification", {
            error: errMessage(e),
          }),
        );
      }
      return v;
    };

    const timeoutMs = questionTimeoutMs(config, this.questionTimeoutMs);
    if (timeoutMs <= 0) return settle(await steering.awaitAnswer(questionId));

    let deadlineAt = this.questionDeadlines.get(runId);
    if (deadlineAt === undefined) {
      deadlineAt = this.now() + timeoutMs;
      this.questionDeadlines.set(runId, deadlineAt);
    }
    const remaining = Math.max(0, deadlineAt - this.now());
    let cancel: (() => void) | undefined;
    const timeout = new Promise<never>((_resolve, reject) => {
      cancel = this.setTimer(
        () => reject(new Error(REASON_QUESTION_TIMEOUT)),
        remaining,
      );
    });
    try {
      return settle(
        await Promise.race([steering.awaitAnswer(questionId), timeout]),
      );
    } finally {
      cancel?.();
    }
  }

  /**
   * PRD #1226 M5 (D6): the completion-interlock LIVE window. Author a completion-question
   * (awaiting_input, marked `completion_question`), then await the owner's continue decision for up
   * to the claim's `completion_hold_window_seconds` (default 900s). Called at the completion-STALL
   * point ONLY (STALL_LIMIT identical no-progress completion attempts), INSTEAD of routing straight
   * to the hold.
   *
   * Deliberately SEPARATE from askUser, not a reuse of it — two things it MUST NOT share:
   *  - askUser's timeout REJECTS with REASON_QUESTION_TIMEOUT (fail-closed). This window must NEVER
   *    throw: expiry is a normal outcome that routes to the verified park, so the timer here
   *    RESOLVES with `{ outcome: "expired" }` instead.
   *  - askUser's #88 `questionDeadlines`/`questionCounts` bookkeeping. This window is timed by the
   *    completion-hold config, not the clarification deadline, and its own deadline is local. It
   *    shares ONLY `openQuestionIds` — the resume-safe question-id map — because a resumed worker
   *    re-parks on the SAME question id, and clearing it on resolution keeps a later ask_user
   *    unaffected (mirroring askUser's settle cleanup).
   *
   * Resolves `{ outcome: "continue", guidance? }` on an owner ANSWER within the window. The API
   * delivers the owner continue-decision as an `answer` naming the run's open_question_id, carrying
   * `["continue"]` (the sentinel) for empty guidance or `[guidance]` otherwise — so a lone
   * "continue" is treated as no guidance and any other text as guidance. Resolves
   * `{ outcome: "expired" }` on the window elapsing, on a non-answer verdict (a cancel resolves the
   * wait too), or when the server would not ACK the park (unable-to-park → the caller parks).
   */
  private async askCompletionQuestion(
    runId: string,
    unmet: string[],
    batcher: MessageBatcher,
    steering: SteeringChannel,
    reportState: (body: StateRequest) => Promise<unknown>,
    runLog: Logger,
    config: ClaimConfig | null,
  ): Promise<{ outcome: "continue"; guidance?: string } | { outcome: "expired" }> {
    // Reuse the id this run is already parked on (a resume re-parks on the SAME question); mint one
    // only for a genuinely new question. Same resume-safe map askUser uses.
    let questionId = this.openQuestionIds.get(runId);
    if (questionId === undefined) {
      questionId = randomUUID();
      this.openQuestionIds.set(runId, questionId);
    }

    const milestoneLines = unmet.length
      ? unmet.map((id) => `- ${id}`).join("\n")
      : "- (the frozen completion contract is not yet satisfied)";
    batcher.emit({
      kind: "question",
      agent: "lead",
      payload: {
        question_id: questionId,
        questions: [
          {
            header: "Completion blocked",
            question: [
              "This run is structurally blocked: it signalled done, but the frozen completion",
              "contract still has milestone(s) not complete, and repeated completion attempts made",
              "no progress:",
              "",
              milestoneLines,
              "",
              "Continue with guidance (tell it how to finish), or let it park for a later decision?",
            ].join("\n"),
          },
        ],
      },
    });
    // Durable before the park is announced — same ordering askUser documents.
    await batcher.flush().catch(() => undefined);

    // Positive ACK, read the PRD #35 way: status === "awaiting_input", NEVER `applied`. A park the
    // server will not ACK is treated as unable-to-park → "expired" so the caller routes to the hold
    // (rather than awaiting an answer no surface can produce).
    const ack = await reportState({
      status: "awaiting_input",
      open_question_id: questionId,
      completion_question: true,
    });
    const parked = (ack as { status?: string } | undefined)?.status;
    if (parked !== "awaiting_input") {
      this.openQuestionIds.delete(runId);
      runLog.warn("completion question: park not acknowledged, routing to hold", {
        run_id: runId,
        question_id: questionId,
        status: parked ?? "unreadable",
      });
      return { outcome: "expired" };
    }
    runLog.info("completion question: awaiting owner continue decision", {
      run_id: runId,
      question_id: questionId,
    });

    // OWN deadline bookkeeping — NOT this.questionDeadlines/questionCounts (those are #88-specific).
    // The window is sourced from completion_hold_window_seconds (default 900s), NOT
    // question_timeout_seconds.
    const windowMs = completionHoldWindowMs(config, COMPLETION_HOLD_WINDOW_DEFAULT_MS);
    let cancelTimer: (() => void) | undefined;
    // The timer RESOLVES to expired (it never rejects) — expiry is a normal outcome, so this window
    // never throws REASON_QUESTION_TIMEOUT.
    const expiry = new Promise<{ outcome: "expired" }>((resolve) => {
      cancelTimer = this.setTimer(() => resolve({ outcome: "expired" }), windowMs);
    });
    try {
      const verdict = await Promise.race([
        steering.awaitAnswer(questionId),
        expiry,
      ]);
      if ("outcome" in verdict) {
        // The timer fired: the window elapsed with no owner answer → expired (the caller parks). We
        // stop racing the awaitAnswer (a later answer resolves a promise nobody awaits — harmless,
        // the run parks and the channel is discarded); the finally clears the timer and the open id.
        runLog.info("completion question: window expired, routing to hold", {
          run_id: runId,
          question_id: questionId,
        });
        return { outcome: "expired" };
      }
      if (verdict.kind !== "answer") {
        // A cancel resolves the wait too. Treat any non-answer verdict as expired (park), NEVER
        // throw — this window has no fail-closed path.
        return { outcome: "expired" };
      }
      // The owner continued. `["continue"]` is the empty-guidance sentinel; any other text is
      // guidance (the answer is index-aligned with the single question above).
      const answers = verdict.answers;
      const guidance =
        answers.length === 1 && answers[0] === "continue"
          ? undefined
          : answers.join("\n").trim() || undefined;
      runLog.info("completion question: owner chose continue", {
        run_id: runId,
        question_id: questionId,
        has_guidance: guidance !== undefined,
      });
      return { outcome: "continue", guidance };
    } finally {
      cancelTimer?.();
      // Drop the open question id (mirrors askUser's settle cleanup) so a later ask_user mints a
      // fresh id and is unaffected. We do NOT touch questionDeadlines — this window never wrote it.
      this.openQuestionIds.delete(runId);
    }
  }
}

/** PRD #332 / issue #334 — the `run_usage` lineage-break marker. Tagged onto the
 *  worker slog line AND the run-feed status payload emitted when a resume is DROPPED
 *  and the runner starts a FRESH SDK session (the ONLY path that breaks run_usage
 *  resume-lineage). Low-cardinality and stable so a maintainer can aggregate its
 *  frequency — e.g. `count(*) from run_messages where payload->>'event' =
 *  'resume_lineage_break'` — to decide whether #332's deferred Option B is worth a
 *  schema+protocol change. Renaming this literal silently breaks that aggregation. */
export const RESUME_LINEAGE_BREAK_EVENT = "resume_lineage_break";

/** PRD #556 M2 (D5) — the positive counterpart to `resume_lineage_break`. Emitted
 *  when a re-claim RESOLVES its prior SDK session and CONTINUES it (M1 preserves the
 *  per-run HOME on a worker-shutdown interrupt, so a same-worker re-claim within the
 *  affinity grace now finds the transcript and resumes silently — this gives operators
 *  a signal to distinguish "resumed cleanly" from "re-planned"). Purely additive and
 *  mutually exclusive with the lineage-break event; queryable the same way via
 *  `payload->>'event' = 'resume_continued'`. Renaming this literal breaks that query. */
export const RESUME_CONTINUED_EVENT = "resume_continued";

/** The answer an AUTOPILOT run receives instead of parking (PRD #88 Decision 8).
 *  Frozen wording: M5's test asserts it byte-exactly, and the lead is told to record
 *  the assumption precisely so an unattended run's guesses stay auditable. */
export const AUTOPILOT_SENTINEL_ANSWER =
  "no human available — proceed on your best judgment, and note the assumption you made";

const AUTOPILOT_ANSWER_NOTICE =
  "The agent asked a clarifying question, but this is an autopilot run with no human in the loop — it was told to proceed on its best judgment and record the assumption.";

/** Failure reason when a parked run's answer deadline expires (PRD #88 Decision 5a).
 *  Fail-closed: the PRD puts a configurable default action out of scope. */
export const REASON_QUESTION_TIMEOUT = "clarification timed out";

/** The server declined the clarification park (PRD #88). Distinct from a timeout so
 *  an operator can tell "nobody answered" from "the question was never askable" —
 *  the second means the run went terminal or changed hands mid-ask, and no human ever
 *  saw a question to answer. */
const REASON_QUESTION_NOT_PARKED =
  "could not park the run to ask a question";

/** The server declined the interactive-task follow-up park (PRD #517 M3). Mirrors
 *  REASON_QUESTION_NOT_PARKED: the run went terminal or changed hands mid-turn, or the
 *  run is not an interactive task, so the awaiting_followup transition matched nothing and
 *  no surface can produce a follow-up. Fail loudly rather than block on a park that never
 *  took. */
export const REASON_FOLLOWUP_NOT_PARKED =
  "could not park the interactive task to await a follow-up";

/** issue #559 M3: the run statuses that mean the interactive turn must NOT continue —
 *  a run that went terminal under us on the follow-up park-SKIP path. Checked against the
 *  read-only ownership probe's reported status; a terminal answer throws
 *  REASON_FOLLOWUP_NOT_PARKED just as a non-awaiting_followup ACK does on the non-skip path. */
const FOLLOWUP_TERMINAL_STATUSES: ReadonlySet<string> = new Set([
  "completed",
  "failed",
  "cancelled",
]);

/** PRD #84 M4: the additive `StateRequest` fields for a plan-time toolchain detection.
 *  Each array field is included ONLY when non-empty — mirroring the `milestones?.length ?
 *  {milestones} : {}` conditional-spread discipline, so a run that detected nothing (or
 *  whose scan failed and passed `undefined`) reports byte-for-byte as before. `size_class`
 *  is included whenever a detection was computed (it is soft/display-only). */
function toolchainReportFields(
  detection: ToolchainDetection | undefined,
): Partial<StateRequest> {
  if (!detection) return {};
  const fields: Partial<StateRequest> = { size_class: detection.size_class };
  if (detection.required_capabilities.length)
    fields.required_capabilities = detection.required_capabilities;
  if (detection.required_tools.length)
    fields.required_tools = detection.required_tools;
  return fields;
}

/** The effective answer deadline: the server-configured claim value when it is
 *  present and positive, else the worker default. An older server omits it (R8). */
function questionTimeoutMs(
  config: ClaimConfig | null,
  fallbackMs: number,
): number {
  const secs = config?.question_timeout_seconds;
  return typeof secs === "number" && secs > 0 ? secs * 1000 : fallbackMs;
}

/** PRD #1226 M5 (D6): the completion-question live-window default, used when the claim omits
 *  completion_hold_window_seconds (an older server) or sends a non-positive value. 900s = 15m. */
const COMPLETION_HOLD_WINDOW_DEFAULT_MS = 900_000;

/** The effective completion-question window: the server-configured claim value when present and
 *  positive, else the worker default. Sibling to questionTimeoutMs but keyed on the DISTINCT
 *  completion_hold_window_seconds — the completion window and the #88 clarification deadline are
 *  configured independently (see ClaimConfig). */
function completionHoldWindowMs(
  config: ClaimConfig | null,
  fallbackMs: number,
): number {
  const secs = config?.completion_hold_window_seconds;
  return typeof secs === "number" && secs > 0 ? secs * 1000 : fallbackMs;
}

/** MR title from the issue snapshot (never empty). Exported for the direct rendering
 *  unit tests (agent/test/runner-mr-completion-scope.test.ts). */
export function mrTitle(
  claim: ClaimResponse,
  scopeCapped?: { completedCount: number; total?: number },
  // PRD #1227 M2: the run's owner completion decisions. A non-empty `deferred` makes this an
  // owner PARTIAL (scope_reduced) — the MR does NOT complete the issue and gets the `[partial]`
  // prefix. An accept-ONLY run (deferred empty, accepted non-empty) closes the issue and gets NO
  // prefix. Absent/empty ⇒ no owner decision, so the title is byte-identical to today.
  completionScope?: ClaimConfig["completion_scope"],
): string {
  const deferred = completionScope?.deferred ?? [];
  const isOwnerPartial = deferred.length > 0;
  // PRD #634 M3 / PRD #1227 M2: a partial delivery — from an operator scope directive OR an owner
  // scope reduction — is prefixed so the reviewer sees at a glance the MR does not complete the issue.
  const prefix = isOwnerPartial || scopeCapped ? "[partial] " : "";
  const t = claim.issue_title?.trim();
  if (t) return prefix + t;
  // PRD #983 M4b: the per-kind empty-title fallbacks (ci_fix's pipeline line, prompt/
  // task/mr_rework's fixed labels) live in RUN_KIND_PROFILES. A row's undefined — every
  // issue-shaped kind, and ci_fix with no pipeline — takes the `Resolve issue #<iid>`
  // fallback below, never `Resolve issue #null` for the issue-less kinds whose derived
  // issue_title almost always won the trimmed branch above.
  const kindTitle = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].mrTitle?.(claim);
  if (kindTitle !== undefined) return kindTitle;
  return `${prefix}Resolve issue #${claim.issue_iid}`;
}

/** MR body: links + closes the issue (issue run) or links the failing pipeline
 *  (ci_fix, PRD #6), states the primary directive (humans merge), and — when the
 *  run used the repo's own agents (PRD #37 Decision 3b) — a marker so the human
 *  reviewer knows the internal review loop was performed by repo-authored agents,
 *  not by uzi's built-in reviewer. */
export function mrDescription(
  claim: ClaimResponse,
  branch: string,
  agentSelection?: { source: AgentSource; agents: string[] },
  selfImproveSection?: string,
  promptGuardSection?: string,
  gatesUnverified?: string[],
  gatesDiscoveryTruncated?: boolean,
  scopeCapped?: { completedCount: number; total?: number },
  // PRD #1226 M4 (D5): render the `Closes #N` line only when told to. A legacy issue run passes true
  // at MR creation (unchanged); an interlocked run passes false at creation and true ONLY on the
  // verified-head reconcile that ADDS Closes after PR-head verification (PRD #1225). This makes the
  // "no `Closes` on an unverified head" invariant structural — the function cannot emit a closing body
  // on its own. Defaults true so the sole issue-arm caller keeps today's behavior.
  renderCloses = true,
  // PRD #1227 M2/M3: the run's owner completion decisions. `deferred` (non-empty ⇒ owner PARTIAL,
  // scope_reduced) drives a partial-delivery body that lists each deferred milestone + reason and
  // NEVER closes the issue — it takes precedence over the #634 scopeCapped count body. `accepted`
  // (non-empty) appends a warning block naming each owner-accepted unmet criterion (id + text +
  // reason), present in ANY branch — including on a closing accept-only PR. Absent/empty ⇒ no partial,
  // no accept, so the body is byte-identical to today.
  completionScope?: ClaimConfig["completion_scope"],
  // PRD #1416 M3 (Part D): the pushed history contains an ancestry bridge (derived from history via
  // rangeContainsBridge, NOT a flight-local flag). When true, ONE GENERIC sentence is appended to the
  // body of EVERY kind's MR — never "the worker bridged it", because the agent's own `git merge -s
  // ours <P>` bridge is equally possible. Defaults false so a non-bridged MR is byte-identical to today.
  bridged = false,
): string {
  const footer = `Opened automatically by the uzi agent from branch \`${branch}\`. Please review and merge manually — the agent never merges.`;
  // One generic sentence, rendered into whichever body arm runs below (a per-kind body or the issue
  // body) so the note is path- AND kind-independent. Empty when not bridged (body unchanged). #1416
  // FIX 6: the issue arm renders it WITHIN the body (before the `---` footer); the bridgeNote form
  // (with its leading blank line) is kept for the per-kind arm, which appends it to a body that
  // already carries its own footer.
  const bridgeSentence = bridged
    ? "This branch contains a history bridge: a published commit was restored as an ancestor so the branch fast-forwards without a force-push, and `git log --first-parent` still reads as the intended history."
    : "";
  const bridgeNote = bridgeSentence ? `\n\n${bridgeSentence}` : "";
  const repoMarker =
    agentSelection?.source === "repo"
      ? [
          "",
          "> ⚠️ This run used agent definitions from the repository's own " +
            `\`.claude/agents/\` (${agentSelection.agents.join(", ") || "none"}). The internal ` +
            "review was performed by those repo-authored agents, not by uzi's built-in " +
            "reviewer — review this change accordingly.",
        ]
      : [];
  // PRD #983 M4b: the per-kind MR bodies (self_improve's tracking-issue reference,
  // ci_fix's pipeline body, prompt/task/mr_rework's issue-less bodies) live in
  // RUN_KIND_PROFILES.mrBody, each returning its exact array-`.join("\n")` string from
  // one explicit context bag. A row's undefined — ci_fix with no pipeline, and the
  // issue/chat/judge kinds that carry no mrBody — falls through to the issue body below
  // (the scopeCapped / gates / Closes arm), which stays here as the richest arm.
  const kindBody = RUN_KIND_PROFILES[resolveRunKind(claim.kind)].mrBody?.(claim, {
    branch,
    baseBranch: claim.base_branch?.trim(),
    repoMarker,
    footer,
    selfImproveSection,
    promptGuardSection,
  });
  if (kindBody !== undefined) return kindBody + bridgeNote;
  // PRD #1227 M2/M3: the owner completion decisions. `deferred` non-empty ⇒ owner PARTIAL
  // (scope_reduced): the issue is NOT fully delivered. `accepted` non-empty ⇒ owner-waived unmet
  // criteria to name in a warning block. Both absent/empty on a normal run.
  const deferred = completionScope?.deferred ?? [];
  const accepted = completionScope?.accepted ?? [];
  const isOwnerPartial = deferred.length > 0;
  const hasAccepted = accepted.length > 0;
  // An owner partial NEVER closes the issue, regardless of the caller's renderCloses — this makes
  // "a partial never closes" structural in the renderer too, not only in the create-then-verify flow.
  const effectiveCloses = renderCloses && !isOwnerPartial;
  // Body selection, in precedence order:
  //   1. PRD #1227 owner partial (deferred) — partial-delivery body listing each deferred milestone +
  //      reason; NO Closes. Takes precedence over the #634 scopeCapped count body.
  //   2. PRD #634 operator scope (scopeCapped) — the existing count-only partial body, UNCHANGED.
  //   3. normal — `Implements issue #N` with the Closes pair gated on effectiveCloses.
  const body = isOwnerPartial
    ? [
        `Implements part of #${claim.issue_iid} (partial delivery — owner scope decision; this MR does NOT close the issue).`,
        "",
        "> ⚠️ **Partial delivery — owner scope decision (PRD #1227).** The owner reduced this run's",
        "> completion scope. The milestone(s) below were DEFERRED BY THE OWNER and are NOT delivered by",
        "> this merge request, so it does not close the issue:",
        ...deferred.map(
          (d) => `> - \`${d.milestone_id}\` — ${d.title}: ${d.reason}`,
        ),
        ...repoMarker,
      ]
    : scopeCapped
      ? // PRD #634 M3: a partial delivery from an operator scope directive does NOT close the
        // issue — it delivered only the approved slice of milestones — so the closing line is
        // replaced with a partial-delivery statement and a scope-note blockquote is inserted.
        [
          `Implements part of #${claim.issue_iid} (partial delivery — see the scope note below; this MR does NOT close the issue).`,
          "",
          "> ⚠️ **Partial delivery — operator scope directive.** The operator narrowed this run's",
          `> scope mid-flight. ${scopeCapped.completedCount}${typeof scopeCapped.total === "number" ? ` of ${scopeCapped.total}` : ""} approved milestone(s) were completed and`,
          "> are included here; any remaining milestones were deferred to a follow-up run. Review this",
          "> as a partial implementation — it does not complete the issue.",
          ...repoMarker,
        ]
      : [
          `Implements issue #${claim.issue_iid}.`,
          // PRD #1226 M4 (D5): the closing line is CONDITIONAL. When effectiveCloses is true (a legacy
          // run at creation, or an interlocked run's verified-head reconcile — PRD #1225) this spreads
          // to exactly the prior `"", "Closes #N"` pair, so the legacy body is byte-for-byte unchanged.
          ...(effectiveCloses ? ["", `Closes #${claim.issue_iid}`] : []),
          ...repoMarker,
        ];
  // PRD #1227 M3 (D3): WHENEVER the owner accepted unmet criteria, append a warning block naming each
  // by id + criterion text + owner reason. Present in ANY branch above — including on a closing
  // accept-only PR, so a closing PR carries the reason the unmet criteria were waived.
  if (hasAccepted) {
    body.push(
      "",
      "> ⚠️ **Accepted unmet criteria — owner decision (PRD #1227).** The owner accepted the following",
      "> unmet criteria as-is with the reason given; they are NOT met by this merge request:",
      ...accepted.map((a) => `> - \`${a.id}\` — ${a.text}: ${a.reason}`),
    );
  }
  const gatesSection = gatesUnverifiedMrSection(gatesUnverified, gatesDiscoveryTruncated);
  if (gatesSection) body.push("", gatesSection);
  // #1416 FIX 6: render the bridge sentence WITHIN the body, before the `---` footer.
  if (bridgeSentence) body.push("", bridgeSentence);
  body.push("", "---", footer);
  return body.join("\n");
}

/** Issue #293 M2: an "unverified gates" note for the MR body, or "" when every
 *  component's deps installed AND discovery saw the whole tree. Dir names arrive already
 *  clamped (safeDirLabel). The truncation caveat (review F1) fires even when `dirs` is
 *  empty: a capped discovery means components it never reached could be unverified too,
 *  which named dirs alone cannot say. */
function gatesUnverifiedMrSection(dirs?: string[], discoveryTruncated?: boolean): string {
  const named = dirs ?? [];
  if (named.length === 0 && !discoveryTruncated) return "";
  const parts: string[] = [];
  if (named.length > 0) {
    const list = named.map((d) => `\`${d}\``).join(", ");
    parts.push(
      `JS dependencies did not install in: ${list}. Gates that need them (e.g. \`vitest\`, \`knip\`) could not run on this change, so treat those gates as unverified, not passing.`,
    );
  }
  if (discoveryTruncated) {
    parts.push(
      "Dependency discovery stopped at its scan cap, so components beyond it were never checked and their gates may also be unverified.",
    );
  }
  return `> ⚠️ **Quality gates unverified.** ${parts.join(" ")}`;
}

/** Feed text for an autopilot run's resolved default selection (PRD #37 Decision
 *  6). Repo source names the count; own source names the fallback. */
function autopilotSelectionText(
  selection: AgentSelection,
  repoCount: number,
): string {
  return selection.source === "repo"
    ? `autopilot: using the ${repoCount} agent(s) from the repo's .claude/agents/`
    : "autopilot: using your own agent templates";
}
