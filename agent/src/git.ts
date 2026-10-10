import { execFile, spawn, type ChildProcess } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import { constants as fsConstants, createReadStream, createWriteStream, type Stats } from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { pipeline } from "node:stream/promises";
import { deflateSync } from "node:zlib";
import { PassThrough, Transform, Writable, type Readable } from "node:stream";
import { StringDecoder } from "node:string_decoder";
import { RUN_KINDS, type RunKind } from "./protocol.js";
import type { Logger } from "./log.js";
import type { PlanCrossCheckDiffRefusal } from "./client.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest, BoundaryStep } from "./harness.js";
import { CheckpointChildStartupTimeoutError } from "./harness.js";
import { RUNNER_UID, killRunnerGroup, runnerCommand, runnerPath, runnerTmpdir, uidSplitActive } from "./runner-uid.js";
import { unmarkedSpawnEnv, workerSpawnEnv } from "./worker-spawn-mark.js";
import { rmRunnerTeardownTree } from "./rmtree.js";
import { withForgeRetry } from "./forge-retry.js";
import {
  ATTEMPT_ID_RE,
  attemptClonePath,
  compareAttemptIds,
  formatResidueName,
  isRetainedArtifactName,
  isWithinPath,
  parseAttemptPath,
  parseRetainedArtifactName,
} from "./attempt-path.js";
import { InvalidRecoveryClonePathError, recoveryProgress, recoverySource, sameRecoverySource, type RecoveryBlocker, type RecoveryProgress, type RecoverySource } from "./recovery-progress.js";
import { sanitizeForLog } from "./run-quiescence.js";
import { RECOVERY_DECODED_LIMIT, RecoveryClosureLimitError, verifyClosureFrames, type ClosureBudget, type ClosureObject } from "./recovery-closure.js";
import { REASON_WORKER_RESIDUE_BLOCKED, RunResidueBlockedError, assertNoCredentialedGitWhileQuarantined, assertResidueQuarantineOpen, residueQuarantine } from "./residue-quarantine.js";

import {
  commitsScannedFromStderr,
  gitleaksArgs,
  gitleaksReportWellFormed,
  gitleaksStdinArgs,
  parseGitleaksReport,
  scanIsTrustworthy,
  stripAnsiSgr,
  type SecretFinding,
} from "./secret-scan-guard.js";
import { lookupAttributes, type PathAttributes } from "./pr-size.js";

const execFileAsync = promisify(execFile);

/** A child's stdout re-exposed so its end waits for the exit status. Node ends a child's
 *  stdout BEFORE `close`, so a consumer of the raw stream reads a clean (possibly empty)
 *  EOF from a failed child and a later destroy(err) is a no-op (issue #1739 follow-up: an
 *  empty checkpoint pack uploaded as if valid). `exited()` ends the returned stream only
 *  once the source has ended AND the exit was clean; `exited(err)` errors it instead.
 *  A consumer that abandons the returned stream first (destroys it, or aborts its upload)
 *  tears the source down and calls `onAbandon`, so the producer is not left blocked
 *  writing into a pipe nobody reads.
 *
 *  The source is piped into `out` AT ONCE, not when a consumer first reads (issue #1769): Node
 *  resumes every readable stdio stream of a child when the child exits (child_process
 *  `flushStdio`), so a stdout nobody had started reading by then is drained into nothing and
 *  a late reader sees only EOF. A caller may await something before it reads (the finalize
 *  import starts its `index-pack` consumer first, which under the real supervisor can take
 *  longer than a small `pack-objects` takes to exit and reap); the bytes wait in `out`, with
 *  backpressure pausing the source, instead of being lost. */
function exitGatedStream(
  source: Readable,
  onAbandon?: () => void,
  deferPrematureCloseUntilExit = false,
): { out: Readable; exited: (err?: Error) => void } {
  const out = new PassThrough();
  // A caller may drop the stream unread (tests do; a skipped publish could): an exit error
  // with no listener would be an uncaught exception that kills the worker. Consumers that
  // attach their own listener, or iterate it, still receive the error.
  out.on("error", () => undefined);
  let sourceEnded = false;
  let sourceClosedPrematurely = false;
  let finished = false;
  let exit: { err?: Error } | undefined;
  const settle = (): void => {
    if (!exit || out.destroyed || finished) return;
    if (exit.err) {
      finished = true;
      out.destroy(exit.err);
    } else if (sourceEnded) {
      finished = true;
      out.end();
    } else if (sourceClosedPrematurely) {
      finished = true;
      out.destroy(new Error("git stdout closed before end (premature close)"));
    }
  };
  out.on("close", () => {
    if (finished) return;
    // Abandoned by the consumer before the child settled.
    source.unpipe(out);
    source.destroy();
    onAbandon?.();
  });
  source.on("error", (err) => out.destroy(err));
  // A source that closes without 'end' or 'error' must fail the output stream.
  // Under a supervised boundary, wait for its terminal evidence first so a
  // verified soft timeout is not hidden by the transport closing during dispose.
  source.on("close", () => {
    if (sourceEnded || finished || out.destroyed) return;
    if (deferPrematureCloseUntilExit) {
      // A supervised child can close stdout during a clean soft disposal before
      // its whole-root reap resolves. Its typed timeout verdict wins over close.
      sourceClosedPrematurely = true;
      settle();
      return;
    }
    out.destroy(new Error("git stdout closed before end (premature close)"));
  });
  source.on("end", () => {
    sourceEnded = true;
    settle();
  });
  source.pipe(out, { end: false });
  return {
    out,
    exited: (err) => {
      exit ??= { err };
      settle();
    },
  };
}

/**
 * issue #1863 — the finalize import's writable view of its `index-pack` consumer's stdin.
 * index-pack stops reading at the pack trailer and exits, and Node destroys a child's stdin on
 * its exit, so that stdin can CLOSE (unfinished, with no error) while the producer's
 * {@link exitGatedStream} still waits for pack-objects' exit status. Piping straight into it
 * made `pipeline` report a premature close and fail an import whose both sides exited 0.
 * Through this sink such a close is not a stream failure: later bytes are discarded and the end
 * completes, so the verdict is left to the two exit statuses and the tip probe. An 'error' on
 * the stdin (EPIPE) still errors the sink, and destroying the sink destroys the stdin.
 */
function consumerInputSink(target: Writable): Writable {
  let pending: ((err?: Error | null) => void) | undefined;
  const release = (): void => {
    const cb = pending;
    pending = undefined;
    cb?.();
  };
  const sink = new Writable({
    write(chunk: Buffer, _enc, cb) {
      if (target.destroyed) {
        cb();
        return;
      }
      if (target.write(chunk)) cb();
      else {
        pending = cb;
        target.once("drain", release);
      }
    },
    final(cb) {
      if (target.destroyed || target.writableFinished) {
        cb();
        return;
      }
      pending = cb;
      target.once("finish", release);
      target.end();
    },
    destroy(err, cb) {
      target.destroy();
      cb(err);
    },
  });
  target.on("error", (err) => sink.destroy(err));
  target.on("close", () => {
    if (!target.errored) release();
  });
  return sink;
}

/** Why publication was refused; `exec_failed` is the default for a bare reason with no classification. */
export type ScratchPublicationKind =
  | "tip_unavailable" | "shallow_history" | "missing_objects" | "object_walk_failed"
  | "exec_failed" | "scratch_present" | "floor_unverified" | "checkpoint_range"
  | "remote_changed_during_refresh" | "remote_candidate_diverged"
  | "new_remote_candidate_diverged" | "remote_branch_advanced";
/** The check that refused. */
export type ScratchPublicationStep =
  | "resolve_tip" | "shallow_check" | "object_walk" | "scratch_walk" | "floor_refresh" | "checkpoint_floor";

export class ScratchPublicationError extends Error {
  readonly code = "scratch_publication_refused";
  readonly kind: ScratchPublicationKind;
  readonly step?: ScratchPublicationStep;
  /** One line, control characters replaced with `?` (sanitizeForLog), capped at DETAIL_MAX; never carries env. */
  readonly detail?: string;
  /** The bounded, unsplit, unsanitized failure text the runner redacts: the failure text with leading
   *  whitespace trimmed, or, when that is longer than RAW_DETAIL_MAX UTF-16 units, only a marker naming
   *  its length (never a cut prefix, see rawText). It can span several lines and
   *  can come from a different source than `detail` (e.g. with 800+ leading blank characters in
   *  stderr, `detail` falls back to the message). A caller that redacts must redact this whole text first
   *  (the redactor matches across CR and LF, so a token split by a newline is only caught on unsplit
   *  text; it does NOT match across U+2028/U+2029, so map those to LF before redacting) and only then
   *  pick a line, sanitize and cap: sanitizing or line-splitting first can leave a token prefix or
   *  turn a control character inside a token into `?`. Untrusted text; never log or report it
   *  unredacted. */
  readonly rawDetail?: string;
  constructor(
    reason: string,
    cause?: unknown,
    opts?: {
      kind?: ScratchPublicationKind; step?: ScratchPublicationStep; detail?: string; rawDetail?: string;
    },
  ) {
    super(`scratch_publication_refused: ${reason}`, { cause });
    this.name = "ScratchPublicationError";
    this.kind = opts?.kind ?? "exec_failed";
    if (opts?.step !== undefined) this.step = opts.step;
    if (opts?.detail !== undefined) this.detail = opts.detail;
    if (opts?.rawDetail !== undefined) this.rawDetail = opts.rawDetail;
  }
}

const DETAIL_MAX = 200;
/** Bound on the unsanitized, unsplit text carried as ScratchPublicationError.rawDetail. */
const RAW_DETAIL_MAX = 4096;

/** `text` with leading whitespace trimmed: unsanitized and, on purpose, not split into lines (see
 *  ScratchPublicationError.rawDetail). Text still longer than RAW_DETAIL_MAX is replaced by a marker
 *  naming its length, never cut: the redactor matches a token across line breaks, so a token split
 *  by padding that a cut separates from its suffix would reach the redactor as a bare prefix. */
function rawText(text: string): string {
  const trimmed = String(text).trimStart();
  return trimmed.length > RAW_DETAIL_MAX ? `[${trimmed.length} characters omitted: over the redaction bound]` : trimmed;
}

/** First non-empty line of `text` as a bounded, log-safe detail. Only a prefix of the input
 *  (DETAIL_MAX*4 UTF-16 units) is scanned, because forge stderr can be megabytes; that prefix cut is
 *  by UTF-16 unit and a line cut by it carries no `...` marker. Lines split on CR, LF, U+2028 and
 *  U+2029, and the chosen line goes through sanitizeForLog, which replaces control and bidi code
 *  points with `?`, cuts by code point at its own cap, and appends `...` when it cuts. The whole
 *  result is at most DETAIL_MAX characters. */
function oneLine(text: string): string {
  const prefix = String(text).slice(0, DETAIL_MAX * 4);
  for (const raw of prefix.split(/[\r\n\u2028\u2029]/)) {
    const line = raw.trim();
    if (line) return sanitizeForLog(line, DETAIL_MAX - 3);
  }
  return "";
}

interface ExecFailure {
  /** Numeric git exit status; undefined for timeout, signal, overflow and spawn errors. */
  exitCode?: number;
  timedOut: boolean;
  overflow: boolean;
  spawnError: boolean;
  stdout: string;
  stderr: string;
  detail: string;
  /** The bounded, unsplit, unsanitized failure text the runner redacts (see ScratchPublicationError.rawDetail);
   *  it can come from a different line than `detail`, e.g. stderr with 800+ leading blank characters. */
  rawDetail: string;
}

/** Classify an execScoped rejection (either the execFile or the boundary shape). */
function classifyExecFailure(err: unknown): ExecFailure {
  const e = (err ?? {}) as {
    code?: unknown; signal?: unknown; killed?: unknown; outputExceeded?: unknown;
    stdout?: unknown; stderr?: unknown; message?: unknown;
  };
  const overflow = e.outputExceeded === true || e.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER";
  const signalled = typeof e.signal === "string" && e.signal !== "";
  // Only our own timeout kill (execFile `killed`) is a timeout; a bare signal is reported as such.
  const timedOut = !overflow && e.killed === true;
  const exitCode = !overflow && !timedOut && !signalled && typeof e.code === "number" ? e.code : undefined;
  const spawnError = !overflow && !timedOut && !signalled && typeof e.code === "string";
  const stderr = typeof e.stderr === "string" ? e.stderr : "";
  const stdout = typeof e.stdout === "string" ? e.stdout : "";
  const parts: string[] = [];
  if (exitCode !== undefined) parts.push(`exit ${exitCode}`);
  if (typeof e.signal === "string" && e.signal) parts.push(`signal ${e.signal}`);
  if (timedOut) parts.push("timed out");
  if (overflow) parts.push("output exceeded limit");
  if (spawnError) parts.push(`code ${String(e.code)}`);
  const message = typeof e.message === "string" ? e.message : String(err);
  const text = oneLine(stderr) || oneLine(message);
  if (text) parts.push(text);
  const rawParts = parts.slice(0, text ? -1 : undefined);
  const rawSource = rawText(stderr) || rawText(message);
  if (rawSource) rawParts.push(rawSource);
  return {
    exitCode, timedOut, overflow, spawnError, stdout, stderr,
    detail: oneLine(parts.join("; ")), rawDetail: rawText(rawParts.join("; ")),
  };
}

function execDetail(f: ExecFailure): { detail: string; rawDetail: string } {
  return { detail: f.detail, rawDetail: f.rawDetail };
}

const PUBLICATION_UNPROVEN = "cannot prove scratch-free candidate history";
const MISSING_OBJECT_RE = /missing|bad object|unable to read|corrupt/i;

const GIT_OUTPUT_ABORT_MESSAGE = "permit-held git output collection aborted: boundary deadline exceeded";
const GIT_LOCK_WAIT_ABORT_MESSAGE = "permit-held git lock wait aborted: boundary deadline exceeded";

/** A permit deadline stopped Git work. Keep this distinct from a scratch finding. */
class GitBoundaryAbortError extends Error {
  constructor(message: string, cause?: unknown) {
    super(message, { cause });
    this.name = "AbortError";
  }
}

/** A checkpoint publication used its cooperative budget. The caller may skip
 * only after every started child has settled under the held permit. */
export class CheckpointSoftDeadlineError extends Error {
  constructor(cause?: CheckpointChildStartupTimeoutError) {
    super(cause === undefined
      ? "checkpoint publication soft deadline exceeded"
      : "checkpoint publication soft deadline exceeded (started_deadline; cleanup verified)",
    cause === undefined ? undefined : { cause });
    this.name = "AbortError";
  }
}

function isAbortLike(error: unknown): error is Error {
  return error instanceof Error && error.name === "AbortError";
}

/** Claim-scoped remote floor, captured under the clone/adoption lock. */
export type PublicationFloor =
  | { readonly kind: "pinned"; readonly oid: string }
  | { readonly kind: "absent" }
  | { readonly kind: "unverified" };

export interface PublicationCandidate {
  readonly floor: PublicationFloor;
  readonly originalHead: string;
  readonly candidate: string;
}

/** Only a complete locked proof may create this disposition. */
export class RemoteBranchAdvancedError extends ScratchPublicationError {
  constructor(readonly proof: Readonly<{ P: string; H: string; C: string; R: string; cause: "remote_branch_advanced" }>) {
    super("remote_branch_advanced", undefined, { kind: "remote_branch_advanced", step: "floor_refresh" });
    this.name = "RemoteBranchAdvancedError";
    Object.freeze(proof);
  }
}

/** issue #1783 M2 — {@link GitCache.releaseAttemptInPlace} failed at `stage` (the ledger append,
 *  or the journal clear that follows it). The journal is KEPT in both cases (the clear is the last
 *  step), so the path stays protected. */
export class AttemptReleaseError extends Error {
  constructor(
    readonly stage: "ledger" | "journal",
    cause: unknown,
  ) {
    super(`attempt release failed at the ${stage === "ledger" ? "ledger append" : "journal clear"}: ${cause instanceof Error ? cause.message : String(cause)}`, { cause });
    this.name = "AttemptReleaseError";
  }
}

/** issue #2213 — a destructive step of a runner-clone release/retire/removal found the worker
 *  residue quarantine latched. A latched worker keeps a failed run's clone, journal and holds, so
 *  the step is refused (or, once a move already happened, undone) rather than completed. Not a
 *  RunResidueBlockedError: it never changes how a run is classified, callers only log it. */
export class CloneRetainedByQuarantineError extends Error {
  constructor() {
    super("worker residue quarantine latched: runner clone retained");
    this.name = "CloneRetainedByQuarantineError";
  }
}

export class ScratchProvisionError extends Error {
  readonly code = "scratch_provision_failed";
  constructor(cause: unknown) {
    super(`scratch_provision_failed: ${cause instanceof Error ? cause.message : "unknown filesystem error"}`, { cause });
    this.name = "ScratchProvisionError";
  }
}

// A ROOT-OWNED, non-writable (0555) empty dir BAKED into the worker image (see
// agent/templates/base/Dockerfile, created as root; the image has no `USER` line —
// the entrypoint drops to the non-root worker at runtime). Every worker git
// invocation sets core.hooksPath here (via gitEnv's own GIT_CONFIG pairs, which
// override any config file), so a hook that ANY non-root process tries to plant
// CANNOT be created — a non-root process cannot create a child inside a root-owned
// 0555 dir. That covers every planter regardless of uid: the agent's own Bash and the
// self_improve check-phase test code (both the `runner` uid under the PRD #51 split;
// out-of-worktree writes are the accepted PRD #42 residual) AND the worker itself. So
// no pre-push (or any) hook can fire (M10 audit).
//
// It is NOT created at runtime: a runtime mkdir would land under the SHARED uid and
// be agent-writable — exactly the vector this closes (relocating, not fixing). A
// nonexistent path is harmless (git simply runs no hooks), so this is safe on a
// host/image without the baked dir (e.g. a unit test), where no hooks run anyway.
const EMPTY_GIT_HOOKS_DIR = "/usr/share/uzi-git-nohooks";
const GIT_BIN = "/usr/bin/git";
const GITLEAKS_BIN = "/usr/local/bin/gitleaks";

// PRD #51 M0 — shared-git write→worker-execute hardening. git has several config keys
// whose value is run as a COMMAND during ordinary (even non-credentialed) operations. A
// process that can write a config source a WORKER-side git reads could plant one and get
// code-exec AS THE WORKER (the PAT/token holder) — no PAT required. The M10
// `core.hooksPath` pin closed hooks only; these close the other FIXED-name keys. Landed
// at M0 (pre-split); the threat surface NARROWED under the M3 (b) topology, and both
// states are described so the pins' role is unambiguous.
//
// Under (b) — the SHIPPED design (see the GitCache layout note below) — the worker is
// BARE-ONLY: it clones/fetches with the PAT, fetches the agent branch BACK over
// file://+pack, tree-diffs (`git diff --name-only origin/main...ref`, no working tree),
// and pushes. It NEVER runs `worktree add`/checkout, `status`, or a content diff, and it
// reads ONLY its own WORKER-OWNED bare `<bare>/config`, which the runner CANNOT write
// (config-source ownership). So a runner cannot plant a key the worker reads, and the
// checkout-/working-tree-only keys never fire worker-side at all. (The runner checks out
// its OWN clone as the runner uid via runGitAsRunner — the untrusted uid exec'ing in its
// own tree is not a boundary crossing; runGitAsRunner still uses this gitEnv base, so the
// pins ride it too.) In M0 (no split, shared agentdata, the WORKER itself did the
// checkout) these vectors WERE live worker-side; the pins were the M0 close and stay the
// close on a #58 single-uid start, where there is no split.
//
// We pin the FIXED-name keys via gitEnv's inline GIT_CONFIG_KEY/VALUE pairs, which are
// HIGHEST precedence (last-value-wins over every config file), so our value OVERRIDES a
// plant regardless of which config source it lives in — verified experimentally. Applied
// UNCONDITIONALLY on every gitEnv git (worker AND runner), so they also neutralize a key
// in the runner's own clone config.
//
// The values are the empirically-verified inert overrides (git 2.55). Two groups:
//
// Command-valued keys — the value is a program git runs, so there is NO boolean
// "disabled" form; the pin is a harmless no-op command that wins by precedence:
//   - core.fsmonitor=false — boolean-false disables the monitor hook (it fires on a
//     working-tree scan — `status`/`add`/checkout — which the bare-only worker never does;
//     pinned defensively + for the runner's checkout).
//   - diff.external=true — git runs the value as an external-diff command via the shell,
//     so there is NO "disabled" value; `""`/`false`/`cat` all get exec'd (and error).
//     `true` is a shell builtin no-op → neutralizes the plant, exits 0, and emits no
//     external diff. The worker's only diff (changedFiles) is `--name-only`, which never
//     invokes diff.external anyway, so this is defensive (and covers the runner's git).
//   - core.pager=cat — disables paging (the git-documented no-op pager). Only ever
//     reached with a tty or `--paginate`, neither of which the piped worker git hits, but
//     pinned defensively.
//   - core.sshCommand=ssh — reverts any plant to the default ssh program. The worker's
//     forge transport is https + local-file only (never ssh), so this is pure
//     belt-and-suspenders; it cannot exec an attacker-chosen program.
//
// Auth / ref keys — reachable when a WORKER-side credentialed op hits an auth challenge
// (401/407) and git runs `git credential fill`, or when it enumerates alternate refs. For
// these an EMPTY value is the inert override (verified: plant fires, empty pin neutralizes)
// — added by the M0 audit (MEDIUM/LOW):
//   - credential.helper="" — an empty value RESETS git's accumulated helper list, so a
//     planted `[credential] helper = !evil` is dropped (append-and-reset, not last-wins).
//     The worker authenticates via the http.extraHeader Basic pair, so it needs no helper
//     — dropping the list cannot break its auth.
//   - core.askpass="" — empty overrides a planted askpass so git skips it (it would
//     otherwise run on a password challenge, even with GIT_TERMINAL_PROMPT=0).
//   - core.alternateRefsCommand="" — empty falls back to git's built-in alternate-ref
//     enumeration (no external command). The worker's bare clones carry no
//     `objects/info/alternates`, so it never fires today; pinned defensively.
//
// NOT pinnable / excluded, with the reason:
//   - core.gitProxy — a MULTIVAR: an appended inline entry does NOT override a planted one
//     (verified: planted proxy still fires under an inline `none`/``), and `git://` is the
//     only transport that consults it — which the worker never uses (https + local-file).
//     Excluded: not inline-pinnable AND not reachable.
//   - core.editor / sequence.editor — no worker fire point: every git this env runs is
//     non-interactive (clone/fetch/push/diff/config), and the agent's OWN `git commit` uses
//     the SDK Bash env, NOT gitEnv. Excluded; pin if an interactive git op is ever routed
//     through gitEnv (reviewer nit — my call).
//   - The ARBITRARY-name keys `filter.<name>.*` (smudge/clean/process) / `diff.<name>.*`
//     (command/textconv) / `merge.<name>.driver` CANNOT be blanket-pinned (driver names are
//     attacker-chosen). They only fire on a checkout/content-diff — which the bare-only
//     worker NEVER performs under (b), so they never reach the worker at all; the close is
//     config-source OWNERSHIP (the worker reads only its worker-owned bare config, which the
//     runner cannot write, and the worker keeps NO worktrees, so there is no
//     `config.worktree` to reach). Where the runner checks out its own clone, such a key is
//     the untrusted uid exec'ing in its OWN tree — not a boundary crossing. In M0 (no split,
//     the worker did the checkout) they WERE reachable worker-side; (b) removed that path.
//     issue #1783: such a runner git is still a WORKER-started process, so it must never carry
//     the worker spawn mark (a plant it starts would inherit the nonce and be exempt from the
//     run-quiescence reaper forever): runGitAsRunner marks only runnerGitCarriesWorkerMark's
//     driver-free subcommands.
//   - `remote.<name>.uploadpack` / `.promisor` with `extensions.partialClone` — also
//     arbitrary-name, so not pinnable here; reached by LAZY FETCH of a missing object from any
//     git, even a pure ref read. Closed by GIT_NO_LAZY_FETCH=1 below (and, for the worker-marked
//     runner git, GIT_ALLOW_PROTOCOL naming no protocol, which overrides every `protocol.*` key a
//     planted config sets): issue #1783, round 3.
const GIT_CODE_EXEC_KEY_PINS: ReadonlyArray<readonly [key: string, value: string]> = [
  ["core.fsmonitor", "false"],
  ["diff.external", "true"],
  ["core.pager", "cat"],
  ["core.sshCommand", "ssh"],
  ["credential.helper", ""],
  ["core.askpass", ""],
  ["core.alternateRefsCommand", ""],
];

const GIT_TIMEOUT_MS = 10 * 60_000; // 10m — clones can be large on cold caches.
/** issue #1783: provisioning's `check-ignore` of the scratch dir reads only the local ignore
 *  files, so it is bounded far below GIT_TIMEOUT_MS (a planted FIFO would otherwise stall it). */
const SCRATCH_IGNORE_CHECK_TIMEOUT_MS = 15_000;
const GIT_MAX_BUFFER = 64 * 1024 * 1024;
/** Wall-clock ceiling on the preserved_patch scan (the patch is already byte-capped by
 *  REVIEW_DIFF_MAX_BYTES, so gitleaks stdin finishes in well under a second in practice). */
const PATCH_SCAN_TIMEOUT_MS = 2 * 60_000;

export type BoundaryProcessSpawner = (request: BoundaryProcessRequest) => Promise<BoundaryProcessHandle>;

interface BoundaryProcessScope {
  spawn: BoundaryProcessSpawner;
  signal: AbortSignal;
  /** Checkpoint-only budget. The soft signal may forfeit a queued lock or abort
   * broker I/O; it must never abort an active child output collector. */
  softSignal?: AbortSignal;
  softDeadlineAt?: number;
  /** issue #1597 M2: awaited by {@link GitCache.withLock} AFTER its critical section and BEFORE the
   *  per-bare lock is released, so a scope (the mid-turn checkpoint tick) can settle a cancelled child
   *  and remove a lock file it provably owned while no other bare mutation can interleave. Never
   *  throws into withLock (a failure is swallowed there). Absent on every other scope. */
  beforeLockRelease?: (key: string) => Promise<void>;
}

/** issue #1597 M2: what {@link GitCache.runnerCloneBusy} observed in the runner clone's gitdir. */
export interface RunnerCloneBusy {
  busy: boolean;
  /** The markers observed (gitdir-relative, e.g. `index.lock`, `rebase-merge/`), or `unreadable`
   *  when the probe itself could not read the gitdir. */
  markers: string[];
}

/** issue #1597 M2: a checkpoint range pinned to two 40-hex commit SHAs — the tip that is scanned
 *  and packed, and the floor it is packed against. See {@link GitCache.resolveCheckpointRange}. */
export interface CheckpointRange {
  tipSha: string;
  excludeSha: string;
  /** issue #1597 M2 (round 3): extra floors for the SCAN only (never the pack) — commits whose
   *  content is already public: the default-branch tip and the last CONFIRMED checkpoint tip (when
   *  it is an ancestor of `tipSha`). Resolved to SHAs together with the pack range. INVARIANT:
   *  every commit the pack `tipSha ^excludeSha` carries is either scanned now, reachable from a
   *  previously confirmed checkpoint publish, or reachable from the default branch. */
  scanFloorShas?: string[];
}

/** issue #1597 M2: optional GitCache construction knobs. */
export interface GitCacheOptions {
  /** Physical terminal custody, rooted at the configured outbox by the caller. */
  terminalRecordProtection?: (runId: string) => Promise<boolean>;
  /** The gitleaks executable. Default `"gitleaks"` (PATH outside a boundary scope, the image's
   *  absolute `/usr/local/bin/gitleaks` inside one via resolveBoundaryExecutable). A test injects an
   *  absolute path to a shim. */
  gitleaksBin?: string;
  /** Test-only override of {@link MAX_OWED_CANDIDATES_PER_RUN} so the cap boundary is cheap to
   *  exercise. Production never passes it. */
  maxOwedCandidates?: number;
  /** Test-only stand-in for runner-clone scratch provisioning, for the non-Linux dev loop.
   *  Production never passes it, so the real provisioner runs and fails closed off Linux. */
  scratchProvisioner?: (clonePath: string) => Promise<void>;
  /** issue #1783 M2 — test-only seam for the runner-uid deletions (the retention sweep's, and M3's
   *  canonical-path free). `split` overrides {@link uidSplitActive} for these deletions alone; `run`
   *  executes the ALREADY runner-wrapped delete argv in place of the real spawn. Production never
   *  passes it. The rest drive the REAL spawn/timeout/close-wait path of runRunnerUidDelete
   *  (and so must not be combined with `run`): `spawn` stands in for the detached spawn of the
   *  wrapped argv, `kill` for killRunnerGroup, `timeoutMs` / `closeWaitMs` for
   *  {@link RETENTION_RM_TIMEOUT_MS} / {@link RETENTION_RM_CLOSE_WAIT_MS}. `lstat` replaces the
   *  retention sweep's re-validation lstat in deleteRetainedArtifact (issue #1783 final). */
  retentionDelete?: {
    split?: boolean;
    run?: (command: string, args: string[]) => Promise<void>;
    spawn?: (command: string, args: string[]) => ChildProcess;
    kill?: (pid: number | undefined) => boolean;
    timeoutMs?: number;
    closeWaitMs?: number;
    lstat?: (p: string) => Promise<Stats>;
  };
  /** issue #1783 M3 — test-only fs seam for the canonical-path free (freeCanonicalClonePath):
   *  `lstat` replaces the validation lstat of the repo dir and the canonical path, `rename` the
   *  quarantine rename, `residueUuid` the residue name's uuid. Production never passes it. */
  canonicalFree?: {
    lstat?: (p: string) => Promise<Stats>;
    rename?: (from: string, to: string) => Promise<void>;
    residueUuid?: () => string;
  };
}

/** issue #1597 M2: the mid-turn checkpoint secret scan's hard deadline (all of its git + gitleaks
 *  children together), enforced by OUR kill of the children — never by gitleaks' own `--timeout`
 *  (see runCheckpointGitleaks). The scan never runs inside a Codex permit (see runner.ts). */
export const CHECKPOINT_SCAN_TIMEOUT_MS = 60_000;

// issue #1597 M2 (review item 6) — pre-scan SIZE CAPS on what gitleaks will read for a checkpoint
// scan: the new AND old side of every blob the scanned commits touch (`git log --raw`), so a
// `git rm` of a huge file is charged too. The caps bound MEMORY and the listings we buffer; they
// do NOT bound time. gitleaks v8.30's throughput depends on content, not size: measured here,
// 12 MB of multi-line base64 scanned in ~2.3s while a single 3 MB dense base64 line took ~41s (and
// the audit saw 20 MiB take 586s / 171 MB RSS). Time is bounded only by our own kill at the scan
// deadline, which classifies the scan `deadline` (untrusted, not published). A range over any cap
// is `scan_too_large` → untrusted → NOT published (the fetch-back still keeps it local, and the
// finalize push path, which has its own scan and the GH013 backstop, is unaffected).
//  - 8 MiB per blob: far above any hand-written source file; larger is almost always a
//    generated/binary artefact.
//  - 128 MiB of blob bytes in total: a multi-hour turn's honest delta is orders of magnitude smaller.
//  - 100k blob oids: bounds the listing we buffer (~41 bytes per oid).
/** Largest single blob, old or new side, a checkpoint scan accepts (bytes). */
const CHECKPOINT_SCAN_MAX_BLOB_BYTES = 8 * 1024 * 1024;
/** Largest total of blob bytes, old and new side, a checkpoint scan accepts. */
const CHECKPOINT_SCAN_MAX_TOTAL_BYTES = 128 * 1024 * 1024;
/** Most distinct blob oids (old + new side) the scanned commits may touch. */
const CHECKPOINT_SCAN_MAX_OBJECTS = 100_000;
/** Most merge commits whose own contribution is scanned separately (see scanMergeContribution). */
const CHECKPOINT_SCAN_MAX_MERGES = 32;
/** A gitleaks run that lasted to within this margin of the scan deadline is classed `deadline`. */
const GITLEAKS_DEADLINE_MARGIN_MS = 500;
/** Byte cap on one merge's own-contribution diff fed to `gitleaks stdin`. */
const CHECKPOINT_SCAN_MAX_MERGE_DIFF_BYTES = 32 * 1024 * 1024;

/** issue #1597 M2: why a checkpoint scan is untrusted (run log only; the feed names the class). */
export type CheckpointScanUntrustedReason =
  | "range_invalid"
  | "scan_too_large"
  | "count_failed"
  | "deadline"
  | "too_many_merges"
  | "merge_diff_too_large"
  | "scan_untrusted";

/** issue #1597 M2: the result of {@link GitCache.secretScanCheckpointRange}. */
export interface CheckpointScanResult {
  trusted: boolean;
  findings: SecretFinding[];
  /** Set when `trusted` is false. */
  reason?: CheckpointScanUntrustedReason;
}

/** issue #1597 M2: the most bytes read from a runner clone's `.git` gitfile (a real one is a single
 *  `gitdir: <path>` line). */
const GITFILE_MAX_BYTES = 4096;

/** issue #1597 M2 (review item 2): classify an agent-controlled `.git` entry WITHOUT ever blocking,
 *  from ONE open: O_RDONLY|O_NOFOLLOW|O_NONBLOCK (a FIFO opens at once instead of waiting for a
 *  writer; a symlink is refused with ELOOP), then fstat THAT fd — there is no separate stat-then-open
 *  a swap could race. A regular file (≤ `maxBytes`) is read through the same fd; a directory is
 *  reported as such; anything else (FIFO, socket, device) is `other`. Throws on an open failure. */
async function readGitEntryNoBlock(
  p: string,
  maxBytes: number,
): Promise<{ kind: "file"; text: string } | { kind: "dir" } | { kind: "other" }> {
  const fh = await fs.open(p, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
  try {
    const st = await fh.stat();
    if (st.isDirectory()) return { kind: "dir" };
    if (!st.isFile()) return { kind: "other" };
    if (st.size > maxBytes) throw new Error("file too large");
    const buf = Buffer.alloc(maxBytes);
    const { bytesRead } = await fh.read(buf, 0, maxBytes, 0);
    return { kind: "file", text: buf.subarray(0, bytesRead).toString("utf8") };
  } finally {
    await fh.close();
  }
}

/** issue #1597 M2: the git-busy markers a runner clone's gitdir can carry while a git operation is
 *  in progress. Directory markers end in `/`. The branch ref lock is added per call. */
const RUNNER_CLONE_BUSY_MARKERS = [
  "index.lock",
  "HEAD.lock",
  "packed-refs.lock",
  "MERGE_HEAD",
  "CHERRY_PICK_HEAD",
  "REVERT_HEAD",
  "rebase-merge/",
  "rebase-apply/",
] as const;

// PRD #400 M4b — byte cap on the review diff a ReviewRunner feeds the reviewer model.
// A huge diff must not blow the model's context window or the worker's memory, so the
// diff is truncated at this size with a marker. 512 KiB is generous for a task-run diff
// while staying an order of magnitude under runGit's 64 MiB maxBuffer.
const REVIEW_DIFF_MAX_BYTES = 512 * 1024;

// PRD #1296 M3 (D4/D5) — durable-recovery bundle limits.
// The maximum COMPLETE bundle byte size the worker will produce before it declares the
// source un-archivable within limits (needs_action) rather than emitting an oversized
// artifact the API's 64 MiB ceiling would reject. Mirrors the lead-design default in D4.
export const RECOVERY_MAX_BUNDLE_BYTES = 64 * 1024 * 1024;
// The nominal ~1 MiB chunk size the API splits an upload stream into (D4). The worker
// records chunk_count = ceil(byteSize / this) in the upload manifest so the server can
// verify its ordered-chunk inventory. This MUST agree with the server's chunk size.
export const RECOVERY_CHUNK_BYTES = 1024 * 1024;
// The named ref the produced bundle carries H under (D5: "a named source ref", never
// `--all`/unrelated refs/reflogs). A dedicated, non-branch name that a fresh forge clone
// checks out on `git clone recovered.bundle`, and that can never collide with a real run
// branch or a stale bare mirror ref (it is created and deleted under the bare lock).
export const RECOVERY_BUNDLE_REF = "refs/heads/recovered-source";

/** Inspect at most 16 KiB of retained bytes; never load the pack to parse its header. */
export async function readRecoveryBundleHeader(bundlePath: string, sourceSha: string): Promise<{
  prerequisiteShas: string[]; selfContained: boolean;
}> {
  const file = await fs.open(bundlePath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
  try {
    const st = await file.stat();
    if (!st.isFile()) throw new Error("recovery bundle is not a regular file");
    const bytes = Buffer.alloc(16 * 1024);
    const { bytesRead } = await file.read(bytes, 0, bytes.length, 0);
    const end = bytes.subarray(0, bytesRead).indexOf("\n\n");
    if (end < 0 || end + 6 > bytesRead || !bytes.subarray(end + 2, end + 6).equals(Buffer.from("PACK"))) {
      throw new Error("invalid or excessive recovery bundle header");
    }
    const header = bytes.subarray(0, end);
    // Latin-1 preserves opaque prerequisite comment bytes without masking structural bytes.
    const lines = header.toString("latin1").split("\n");
    const version = lines.shift();
    if (version !== "# v2 git bundle" && version !== "# v3 git bundle") throw new Error("invalid recovery bundle version");
    if (version === "# v3 git bundle" && lines[0] === "@object-format=sha1") lines.shift();
    const prerequisites: string[] = [];
    while (lines[0]?.startsWith("-")) {
      const line = lines.shift()!;
      const match = /^-([0-9a-f]{40}) /.exec(line);
      if (!match || prerequisites.length >= 64 || prerequisites.includes(match[1]!)) throw new Error("invalid recovery prerequisites");
      prerequisites.push(match[1]!);
    }
    if (lines.length !== 1 || lines[0] !== `${sourceSha} ${RECOVERY_BUNDLE_REF}` || !/^[0-9a-f]{40}$/.test(sourceSha)) {
      throw new Error("recovery bundle source/ref mismatch");
    }
    return { prerequisiteShas: prerequisites.sort(), selfContained: prerequisites.length === 0 };
  } finally { await file.close(); }
}

/** The verified-bundle facts the recovery uploader binds into the upload manifest and
 *  journals BEFORE upload (PRD #1296 D5). Produced from trusted bare-object operations
 *  only — no source checkout, no repo-controlled hooks/filters. */
export interface RecoveryBundleResult {
  /** Absolute path of the produced bundle file. */
  bundlePath: string;
  /** Complete-bundle byte size. */
  byteSize: number;
  /** Lowercase hex SHA-256 of the complete bundle bytes. */
  checksum: string;
  /** Expected ordered-chunk inventory = ceil(byteSize / RECOVERY_CHUNK_BYTES). */
  chunkCount: number;
  /** Actual sorted bundle-header dependencies. Guarded cached dependencies retain
   *  local custody; legacy dependencies derive from the fresh forge tip. */
  prerequisiteShas: string[];
  /** The resolved original committed head H (40-hex). */
  sourceSha: string;
  /** True when the verified bundle header declares no external prerequisites and the
   *  bundle carries H's full reachable history within the size limit (D5). */
  selfContained: boolean;
  /** True when H is ALREADY reachable from the fresh forge tip (merge-base(H, forgeTip) ==
   *  H) — i.e. the committed head is already published, so there is NOTHING to archive.
   *  No bundle file is written; the caller releases custody against this verified forge
   *  history (D3's no-unpublished-output disposition) rather than emitting a redundant
   *  full-history bundle. bundlePath is empty in this case. */
  alreadyPublished: boolean;
}

/** PRD #1296 D5 — a produced bundle exceeded RECOVERY_MAX_BUNDLE_BYTES. The caller
 *  retains custody and surfaces needs_action rather than truncating or force-shipping an
 *  oversized artifact. */
export class RecoveryBundleTooLargeError extends Error {
  constructor(readonly byteSize: number, readonly maxBytes: number) {
    super(`recovery bundle is ${byteSize} bytes, over the ${maxBytes}-byte limit`);
    this.name = "RecoveryBundleTooLargeError";
  }
}

// PRD #974 M2 — cap the gitleaks JSON report read in secretScanRange. The report grows with
// the finding COUNT over attacker-authored commits, so an adversarial repo could inflate it
// past memory; an over-cap report is treated as an untrusted scan (fail open to the GH013
// backstop) rather than read into memory. 16 MiB holds far more findings than any honest push.
const SECRET_SCAN_REPORT_MAX_BYTES = 16 * 1024 * 1024;

/** A full 40-hex commit SHA (issue #1597 M2: pinned checkpoint ranges are validated with it). */
const SHA40_RE = /^[0-9a-f]{40}$/;

/** PRD #1798 M5: the bound on a {@link GitCache.readBare} read. */
export interface BoundedReadOptions {
  /** Stop reading once more than this many stdout bytes arrived (the kept prefix is at most this).
   *  Must be finite and non-negative; readBare throws otherwise. */
  maxBytes: number;
  /** Wall-clock bound, default GIT_TIMEOUT_MS; must be positive and finite. The read rejects when it
   *  elapses. The child's termination at that bound differs per spawn path: see GitCache.readBare. */
  timeoutMs?: number;
  /** Aborting rejects the read. What that does to the child: see GitCache.readBare. */
  signal?: AbortSignal;
}

/** PRD #1798 M5: the most git stderr a {@link GitCache.readBare} keeps for its failure message. */
const READ_BARE_STDERR_MAX_BYTES = 64 * 1024;

/** Optional per-call knobs of GitCache.spawnGit. Absent, spawnGit behaves as it always has. */
interface SpawnGitOptions {
  /** Inside a boundary scope: forwarded as BoundaryProcessRequest.timeoutMs, so the spawner
   *  terminates the child's process group when it elapses. Ignored outside a scope. */
  timeoutMs?: number;
  /** Native producer only: retain this cause when its own deadline kills a still-running child. */
  nativeTimeoutError?: Error;
  /** Cap on the stderr kept for the failure message (both paths). Default: the boundary path keeps
   *  up to GIT_MAX_BUFFER, the plain path keeps all of it. */
  stderrMaxBytes?: number;
  /** Trusted verifier traversal environment; never sourced from repository config. */
  env?: NodeJS.ProcessEnv;
  cancelOnAbandon?: boolean;
}

/** PRD #1798 M5: a bounded read's stdout, and whether the bound cut it. */
export interface BoundedRead {
  text: string;
  truncated: boolean;
}

// PRD #1416 M3 — cap on the EXTRA parents (beyond the first) a worker-bridge marker candidate may
// carry in rangeContainsBridge before it is rejected outright. A legitimate bridge has at most 2
// (the published floor P and the checkpoint floor C); the per-parent validation spawns ~2
// `isAncestor` git subprocesses per extra parent, so an attacker-crafted commit with many distinct
// descendant-of-P parents would stall the run's own finalize. 8 is a safe headroom over 2.
const BRIDGE_MAX_EXTRA_PARENTS = 8;

// PRD #51 M3 — (b) separate-runner-clone: the worker-side tracking-ref namespace the
// worker's fetch-back writes the agent branch into. Deliberately NOT refs/heads/* (B2
// invariant 2: the runner's branch is admitted only into a demarcated worker-side
// tracking namespace, never commingled with the bare's heads); pushBranch then pushes
// FROM this ref to origin's refs/heads/<branch>. The worker owns its bare, so this ref
// is worker-controlled and read only by worker-side git on gitEnv.
const RUNNER_TRACKING_PREFIX = "refs/uzi-runner/";
function runnerTrackingRef(branch: string): string {
  return `${RUNNER_TRACKING_PREFIX}${branch}`;
}

// issue #1507 — a run+generation-scoped ANCHOR for the exact restore-point head a park /
// early-terminal settle transfers into the trusted bare. Unlike the per-BRANCH tracking ref
// `refs/uzi-runner/<branch>` (which a concurrent run sharing this bare+branch can move with its own
// `fetchAgentBranch`/`updateTrackingRef`), this ref is keyed on the SETTLING run's id + claim
// generation, so it is immune to another run's fetch and keeps the verified head durably reachable
// through bundle production even after the branch tracking ref moves. The runId is sanitized into a
// SINGLE path component (mirroring `refs/uzi-archive/<sanitized>/<sha>`) and the numeric generation
// is the leaf, so the namespace is D/F-safe and dodges the branch-slash D/F hazard that affected
// `refs/uzi-runner` (issue #887) — it never carries the branch.
const RECOVERY_PIN_PREFIX = "refs/uzi-recovery-pin/";
// issue #2213 — the quarantine archival anchor `refs/uzi-archive/<runId>/g<generation>`. The runId is a
// single sanitized path component and the leaf starts with `g` (the existing refs/uzi-archive leaf is
// a 40-hex sha, which never starts with `g`), so the two uses of the namespace cannot collide or
// D/F-conflict. Nothing in agent/src reads or deletes refs/uzi-archive/*.
function quarantineArchiveRef(runId: string, generation: number): string {
  const rid = runId.replace(/[^A-Za-z0-9_-]/g, "-");
  return `refs/uzi-archive/${rid}/g${Math.max(0, Math.trunc(generation))}`;
}
// issue #1582 M2 — the settlement pins for an older-generation custody hold a same-worker successor
// adopted: `refs/uzi-settle/<runId>/<holdId>/{source,adopted,pushed,published}`. They keep the candidate
// commits a settle request names reachable (`published` is the live-publication tip a
// `/settle-live` request names, issue #1751 M2) (a `--all` gc root, like refs/uzi-recovery-pin) until the
// api releases the hold. Both ids are sanitized into SINGLE safe path components; an id that
// sanitizes to empty yields no ref at all.
const SETTLE_PIN_PREFIX = "refs/uzi-settle/";
export type SettlementPinKind = "source" | "adopted" | "pushed" | "published";
const SETTLEMENT_PIN_KINDS: readonly SettlementPinKind[] = ["source", "adopted", "pushed", "published"];
function settlementPinBase(runId: string, holdId: string): string | null {
  const rid = runId.replace(/[^A-Za-z0-9_-]/g, "-");
  const hid = holdId.replace(/[^A-Za-z0-9_-]/g, "-");
  if (rid === "" || hid === "") return null;
  return `${SETTLE_PIN_PREFIX}${rid}/${hid}/`;
}
function recoveryPinRef(runId: string, generation: number): string {
  const rid = runId.replace(/[^A-Za-z0-9_-]/g, "-");
  return `${RECOVERY_PIN_PREFIX}${rid}/${generation}`;
}

// PRD #218 — the run-identity ANCHOR for a tracking ref. `refs/uzi-runner/<branch>` is
// per-BRANCH and nothing ever deletes it, so on its own it cannot tell "the run that
// wrote this parked its own work" from "a DIFFERENT, permanently-dead run left an orphan
// on the same issue" — the auditor's counterexample (run A parks and dies; a fresh run B
// on the same issue is later killed mid-turn and requeued, and would seed off A's commits
// while claiming to have recovered its own). So alongside the ref we stamp the writing
// run's id into the worker bare's own config, and the reseed reads the tracking ref back
// ONLY when the stamp matches the claiming run. This is a worker-owned `config --local`
// write, the same posture as disableAutoMaintenance — no new trust boundary.
//
// issue #887 — the branch is carried in a git-config SUBSECTION, not flattened into the
// variable name. git parses `section.subsection.variable` by the FIRST and LAST dot, so
// the middle keeps the branch VERBATIM (slashes and dots included) and the variable is the
// fixed `owner`. The earlier form flattened `/`->`-` into the variable name, which collided:
// `uzi/self-improve` and a literal `uzi-self-improve` mapped to the same key, so clearing
// one branch's owner stamp (clearConflictingAncestorTrackingRefs) could wipe the other's
// and cost a later resume its unpushed recovery state. The subsection encoding is reversible
// and collision-free — distinct branches always land in distinct `[uzi-trackowner "<branch>"]`
// blocks. (Legacy two-part stamps on a persistent bare become dead config no read touches.)
function runnerTrackingOwnerKey(branch: string): string {
  return `uzi-trackowner.${branch}.owner`;
}

type OrphanRetireStage = "journal_read" | "journal_validation" | "containment" | "holding_parent" | "rename" | "retained_copy" | "intra_device_rename" | "journal_clear";
export type OrphanStage = OrphanRetireStage | "classification" | "identity" | "branch" | "path" | "quiescence" | "attempt_ledger" | "attempt_journal" | "retirement" | "complete";
export type OrphanReason = "http" | "transport_unknown" | "nonterminal_owner" | "repo_mismatch" | "malformed_identity" | "branch_mismatch" | "path_error" | "path_mismatch" | "quiescence_blocked" | "quiescence_error" | "attempt_release_failure" | "canonical_retirement_failure" | "retained-in-place" | RunnerCloneRetireResult;
export type RunnerCloneRetireResult = "quarantined" | "source-already-absent";
export interface OrphanDiagnostics {
  logger: Logger;
  claimantId: string;
  ownerId: string;
  repoId: string;
  pathShape: "canonical" | "attempt" | "unknown";
}

/** Only fixed vocabulary, UUIDs and a path digest leave this boundary. Diagnostic failures
 * never replace a refusal or turn completed retirement into failure. */
export function emitOrphanDiagnostic(
  context: OrphanDiagnostics,
  journaledPath: string,
  event: "orphan_reclaim_refused" | "orphan_retirement_failed" | "orphan_reclaim_succeeded",
  stage: OrphanStage,
  reason: OrphanReason,
  error?: unknown,
  httpStatus?: number,
): void {
  try {
    const safeId = (id: string): string =>
      typeof id === "string" && id.length === 36 && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id) ? id : "invalid";
    let fingerprint = "0".repeat(64);
    try { fingerprint = createHash("sha256").update(journaledPath).digest("hex"); } catch { /* best effort */ }
    const code = (error as NodeJS.ErrnoException | undefined)?.code;
    const errno = typeof code === "string" && ["EACCES", "EPERM", "ENOENT", "EXDEV", "EIO", "ENOTEMPTY", "EEXIST", "ENOTDIR", "EINVAL", "ENOSPC", "EROFS", "ELOOP", "EMFILE", "ENFILE"].includes(code) ? code : "unknown";
    context.logger[event === "orphan_reclaim_succeeded" ? "info" : "warn"](event, {
      event, stage, reason,
      claimant_id: safeId(context.claimantId), owner_id: safeId(context.ownerId), repo_id: safeId(context.repoId),
      path_shape: context.pathShape === "canonical" || context.pathShape === "attempt" ? context.pathShape : "unknown", path_fingerprint: fingerprint, errno,
      ...(Number.isInteger(httpStatus) && httpStatus! >= 100 && httpStatus! <= 599 ? { http_status: httpStatus } : {}),
    });
  } catch { /* best effort, including hostile logger or error accessors */ }
}

/** A same-run clone survived recovery; the runner must capture it before reseeding. */
export class PendingRecoveryCaptureError extends Error {
  constructor(readonly clonePath: string, readonly branch: string) {
    super("retained recovery work must be captured before reseeding");
    this.name = "PendingRecoveryCaptureError";
  }
}

/**
 * issue #1315 — the canonical clone for this branch is journaled to ANOTHER run's
 * recovery capture. The git layer is claim-agnostic and NEVER probes owner status, so
 * it fails closed here; the runner probes the named `ownerRunId` authoritatively and
 * reclaims (via retireRunnerClone) only when that owner is terminal. This is the ONLY
 * reclaimable guard case: the journaled path equals the computed canonical clone path.
 */
export class ForeignCaptureBlockedError extends Error {
  constructor(
    readonly clonePath: string,
    readonly branch: string,
    readonly ownerRunId: string,
  ) {
    super("refusing to replace a retained clone owned by another run");
    this.name = "ForeignCaptureBlockedError";
  }
}

/**
 * issue #1315/#1319 — the recovery journal for this branch names a clone path that is NOT
 * this branch's CLAIMANT-computed canonical clone (a different clone key — e.g. the
 * cross-kind slug divergence `issue-N` vs `agent-issue-N`). The git layer is claim-agnostic
 * and NEVER probes owner status; it fails closed here carrying `ownerRunId`. This is NO
 * LONGER "never reclaimable": the runner (issue #1319) may catch it and run the authoritative
 * owner-canonical validation — probing the named owner and, ONLY when the owner is terminal
 * AND the owner-derived branch and canonical path match the journal, quarantining the residue
 * and reseeding. On any unmet predicate the runner fails closed exactly as before. The git
 * layer itself still never probes or retires on this error.
 */
export class CapturePathMismatchError extends Error {
  constructor(
    readonly journaledPath: string,
    readonly computedPath: string,
    readonly branch: string,
    readonly ownerRunId: string,
  ) {
    super("recovery journal points at a different clone path than this branch's computed clone");
    this.name = "CapturePathMismatchError";
  }
}

/** issue #1783: the failure-reason prefix of a run whose clone (or the canonical clone path it
 *  must free) could not be proven quiescent or freed. The runner's failOriginForReason maps it to
 *  the fail_origin `worker_residue_blocked`; the runner re-exports it. Defined in
 *  residue-quarantine.ts (issue #2213) so the error classes there share it without an import cycle. */
export { REASON_WORKER_RESIDUE_BLOCKED };

/**
 * issue #1783 M3 — the canonical runner clone path could not be freed for a reseed: the scoped
 * process scan found survivors, an unverified process or a live same-key owner, the path or its
 * repo dir failed validation (not `<runnerRoot>/<repoDir>/<key>`, or the repo dir not a real
 * directory), the delete failed with nothing left to quarantine, or the same-parent quarantine
 * rename (or its confirm) failed. Nothing further is moved or deleted. The run fails typed
 * `worker_residue_blocked`, never the generic agent_failure.
 */
export class CloneResidueBlockedError extends Error {
  readonly detail: string;
  constructor(detail: string) {
    // The detail reaches the run's failure_reason: short, and stripped of control/bidi characters.
    const clean = sanitizeForLog(detail, 160);
    super(`${REASON_WORKER_RESIDUE_BLOCKED}: the canonical runner clone path could not be freed for the reseed (${clean}); what remains there is kept`);
    this.detail = clean;
    this.name = "CloneResidueBlockedError";
  }
}

/**
 * issue #1769 — a sandboxed (Codex) runner clone could not be made self-contained. The
 * Codex command sandbox does not grant the worker bare, so a clone still borrowing objects
 * through `objects/info/alternates` is unusable there; the run fails before the executor
 * starts rather than handing the agent a clone its git cannot read. On this error the
 * clone's alternates file has been restored in place (best-effort) and the temporary
 * `refs/uzi-materialize/*` anchors removed.
 */
export class RunnerCloneMaterializationError extends Error {
  constructor(
    readonly clonePath: string,
    readonly causeMessage: string,
  ) {
    super(`runner clone could not be made self-contained: ${causeMessage}`);
    this.name = "RunnerCloneMaterializationError";
  }
}

/**
 * issue #1769 — the fresh default tip `fetchDefaultTip` brought into the worker bare could not be
 * imported into a SELF-CONTAINED (Codex) runner clone. Such a clone has no alternate into the
 * bare, so the finalize base-align cannot reach the tip's objects until
 * {@link GitCache.ensureRunnerCloneObjects} copies them in; when that copy fails (either side of
 * the pack stream, or the post-import verification), this is thrown once both processes of the
 * import have exited (see that method for how a boundary producer is ended).
 */
export class RunnerCloneImportError extends Error {
  constructor(
    readonly clonePath: string,
    readonly tip: string,
    readonly causeMessage: string,
  ) {
    super(`could not import ${tip} into the runner clone: ${causeMessage}`);
    this.name = "RunnerCloneImportError";
  }
}

function recoveryCaptureKey(branch: string): string {
  return `uzi-recovery.${branch}.clone`;
}

/** The recovery-capture journal value (one per branch, in the bare's `config --local`). A
 *  Docker-wired worker (issue #1783 M2) adds the attempt id; an entry an older worker wrote has
 *  none, and is still read. */
export class ForeignRetainedRecoveryError extends Error {
  constructor() { super("foreign retained recovery"); }
}

export class RetainedRecoveryBlockedError extends Error {
  readonly reason = "source_missing";
  constructor(readonly barePath: string, readonly branch: string, readonly key: string,
    readonly journal: RecoveryJournalEntry) {
    super("known recovery source is missing");
    this.name = "RetainedRecoveryBlockedError";
  }
}

export interface RecoveryJournalEntry extends RecoverySource {
  recovery?: RecoveryProgress;
  retainedSources?: RecoverySource[];
}

// issue #1783 M2 — the per-branch ATTEMPT LEDGER, a MULTI-VALUED `config --local` key in the
// worker bare (on the /data PVC, so it survives a container and a pod restart). Every value is the
// JSON `{"attemptId","runId","clonePath","state"}`, appended with `git config --add`; for one
// attemptId the LAST value wins. `live` is written when an attempt is seeded, `retired` when the
// owner's terminal retire disposed of its clone (or the retention sweep deleted it), `abandoned`
// when a VERIFIED capture released it IN PLACE (the clone stays on disk, unreachable by any
// successor; its work is in the tracking ref, so the retention sweep may dispose of it), and
// `reclaimed` when the terminal-orphan reclaim released a FOREIGN owner's attempt in place WITHOUT
// capturing it: the clone may be the only copy of that run's work, so it is retained FOREVER (the
// attempt-path twin of the unwired worker's foreign quarantine, `discard:false`) — never counted
// toward the abandoned cap and never deleted. Each seed compacts the key to its last value per
// attemptId (see compactAttemptLedger).
// The watcher's backup script (.agents/skills/uzi-watcher/scripts/backup-runs.sh) reads exactly
// this key and these field names: keep them byte-for-byte. It reads `state` only to report a BARE
// backup's attempt as retired, which requires every entry for the run to be `retired` or
// `abandoned`; any other value, including a new one, reads as "attempt state unknown".
function attemptLedgerKey(branch: string): string {
  return `uzi-attempts.${branch}.entry`;
}

/** One attempt ledger state (see {@link attemptLedgerKey}). */
export type AttemptLedgerState = "live" | "abandoned" | "retired" | "reclaimed";

/** One attempt ledger value. */
export interface AttemptLedgerEntry {
  attemptId: string;
  runId: string;
  clonePath: string;
  state: AttemptLedgerState;
}

function parseRecoveryJournal(value: string): RecoveryJournalEntry {
  const parsed: unknown = JSON.parse(value);
  if (typeof parsed !== "object" || parsed === null || !("runId" in parsed) || !("clonePath" in parsed)
      || typeof parsed.runId !== "string" || typeof parsed.clonePath !== "string") {
    throw new Error("invalid retained recovery clone journal");
  }
  const attemptId = "attemptId" in parsed ? parsed.attemptId : undefined;
  if (attemptId !== undefined && (typeof attemptId !== "string" || !ATTEMPT_ID_RE.test(attemptId))) {
    throw new Error("invalid retained recovery clone journal");
  }
  const o = parsed as Record<string, unknown>;
  if (Object.keys(o).some(k => !["runId", "clonePath", "attemptId", "restoreTip", "recovery", "retainedSources"].includes(k))) {
    throw new Error("invalid retained recovery clone journal");
  }
  // Legacy callers classify a mismatched attempt id as CapturePathMismatchError.
  // Episode descriptors require strict identity before they can authorize any write.
  const source: RecoverySource = { ...recoverySource({ runId: parsed.runId, clonePath: parsed.clonePath,
    ...(o.recovery !== undefined || o.retainedSources !== undefined ? { attemptId } : {}),
    ...(o.restoreTip === undefined ? {} : { restoreTip: o.restoreTip }) }),
    ...(attemptId === undefined ? {} : { attemptId }) };
  const recovery = o.recovery === undefined ? undefined : recoveryProgress(o.recovery);
  let retainedSources: RecoverySource[] | undefined;
  if (o.retainedSources !== undefined) {
    if (!Array.isArray(o.retainedSources)) throw new Error("invalid retained sources");
    retainedSources = o.retainedSources.map(recoverySource);
  }
  if (recovery && !sameRecoverySource(source, recovery.source) &&
      !(recovery.successor && sameRecoverySource(source, recovery.successor)) &&
      !retainedSources?.some(s => sameRecoverySource(source, s))) throw new Error("recovery source identity mismatch");
  if (recovery?.stage === "ready-for-model" &&
      (!recovery.successor || !sameRecoverySource(source, recovery.successor))) throw new Error("recovery successor identity mismatch");
  if (recovery && ["captured", "adopting", "ready-for-model"].includes(recovery.stage) &&
      source.restoreTip !== recovery.restoreTip) throw new Error("recovery restore tip mismatch");
  if (recovery && ["adopting", "ready-for-model"].includes(recovery.stage) &&
      recovery.successor?.restoreTip !== recovery.restoreTip) throw new Error("recovery successor tip mismatch");
  if (recovery && (recovery.source.runId !== source.runId ||
      (recovery.successor && recovery.successor.runId !== source.runId))) throw new Error("recovery run identity mismatch");
  if (retainedSources?.some(s => s.runId !== source.runId)) throw new Error("retained run identity mismatch");
  return { ...source, ...(recovery ? { recovery } : {}), ...(retainedSources ? { retainedSources } : {}) };
}

function parseAttemptLedgerEntry(value: string): AttemptLedgerEntry | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    return undefined;
  }
  if (typeof parsed !== "object" || parsed === null) return undefined;
  const o = parsed as Record<string, unknown>;
  if (typeof o.attemptId !== "string" || !ATTEMPT_ID_RE.test(o.attemptId)) return undefined;
  if (typeof o.runId !== "string" || typeof o.clonePath !== "string") return undefined;
  if (o.state !== "live" && o.state !== "abandoned" && o.state !== "retired" && o.state !== "reclaimed") return undefined;
  return { attemptId: o.attemptId, runId: o.runId, clonePath: o.clonePath, state: o.state };
}

/** One ledger value, in the documented field order (the backup script's contract). */
function attemptLedgerValue(entry: AttemptLedgerEntry): string {
  return JSON.stringify({
    attemptId: entry.attemptId,
    runId: entry.runId,
    clonePath: entry.clonePath,
    state: entry.state,
  });
}

/** How many abandoned attempts (each with its skills sibling) the retention sweep keeps per key. */
const RETAINED_ABANDONED_PER_KEY = 3;
/** The private temp-file prefix of the atomic ledger compaction (inside the bare, beside `config`). */
const LEDGER_COMPACT_TMP_PREFIX = "config.uzi-compact-";
/** How many `.uzi-residue-*` entries the retention sweep keeps per clone key. */
const RETAINED_RESIDUE_PER_KEY = 5;
/** The retention sweep's runner-uid delete: the image's root-owned busybox rm (absolute, so it
 *  never resolves from a runner-writable PATH), and its deadline. */
const RETENTION_RM_BIN = "/bin/rm";
const RETENTION_RM_TIMEOUT_MS = 120_000;
/** After the timeout's group kill, how long the delete waits for the rm's 'close' (its stderr
 *  pipe shut: every holder of it gone) before declaring the rm possibly still alive. */
const RETENTION_RM_CLOSE_WAIT_MS = 10_000;

/**
 * issue #1783 — the runner-uid delete timed out and its rm could NOT be shown gone: the child's
 * 'close' did not arrive within {@link RETENTION_RM_CLOSE_WAIT_MS} after the group kill, whether
 * the kill reported success or failure (a failed kill may be ESRCH from a group already gone, so
 * it waits for 'close' too). The GitCache remembers the path until that 'close' arrives. busybox
 * rm walks by PATH, so a surviving rm deletes whatever is later created at that path (a fresh
 * clone reseeded there). A caller must neither quarantine nor reseed at, nor keep deleting beside,
 * a path such an rm may still be walking.
 */
class RunnerDeleteUnsettledError extends Error {
  constructor(detail: string) {
    super(`runner-uid delete timed out and may still be running: ${detail}`);
    this.name = "RunnerDeleteUnsettledError";
  }
}

/**
 * issue #1783 M3 — what the runner hands {@link GitCache.runnerCloneForBranch} so a CANONICAL
 * reseed (an unwired worker, or any canonical-path seed) frees `<runnerRoot>/<repoDir>/<key>`
 * only after a scoped process proof, and quarantines what it cannot delete instead of failing on
 * it. REQUIRED on every entry point (N5): there is no unproven plain-`fs.rm` fallback. A
 * Docker-wired attempt seed never frees the canonical path, so it never calls this.
 */
export interface CanonicalReseedOptions {
  /** The process proof over the canonical path, called under the bare lock AFTER the journal
   *  classification and only when the canonical path exists and is a real directory under
   *  runnerRoot (a symlink or other non-directory there is quarantined without it: renaming the
   *  entry moves nothing a process can be inside). Throws (a {@link CloneResidueBlockedError}) to
   *  block: nothing is removed or moved. */
  beforeFree: (canonicalPath: string) => Promise<void>;
}

/**
 * issue #1783 M2 — what a Docker-wired worker hands {@link GitCache.runnerCloneForBranch} to seed
 * a fresh ATTEMPT path `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>` instead of the fixed
 * canonical `<key>` path. Absent ⇒ today's canonical seed, byte-for-byte.
 */
export interface AttemptSeedOptions {
  /** The attempt id minted for this execution attempt (the marker carries the same id). */
  attemptId: string;
  /** True when `clonePath` belongs to an attempt live on this worker right now. */
  isLive: (clonePath: string) => boolean;
  /** The seed-time recovery sweep, called under the bare lock AFTER the journal classification
   *  and BEFORE anything is seeded, with every NON-LIVE path of this key (the canonical path,
   *  every `<key>.attempt-*` sibling on disk and every path the ledger records for the key).
   *  Throws to block the seed (nothing is moved or seeded). */
  beforeSeed: (nonLivePaths: string[], canonicalPath: string) => Promise<void>;
  /** Called under the bare lock once the attempt path is seeded and its ledger `live` entry is
   *  written, so the caller can register the attempt live before any other seed of the key can
   *  run its sweep. */
  onSeeded?: (clonePath: string) => void;
  /** The retention sweep's quiescence predicate: true when the scoped process scan over `paths`
   *  finds nothing in scope. A false (or a throw) keeps the entry. */
  quiescent: (paths: string[], canonicalPath: string) => Promise<boolean>;
}

// issue #909 — the PRE-#887 flattened owner-key form. Kept ONLY so a resume can still READ a
// stamp a persistent bare wrote under old code during the rollout window. flatten() is lossy
// (`/`, `.` -> `-`), so this key is NOT branch-injective: never WRITE under it, and read it only
// through readTrackingOwner()'s collision guard plus the caller's runId-equality gate.
function legacyFlatTrackingOwnerKey(branch: string): string {
  return `uzi-trackowner.${branch.replace(/[^A-Za-z0-9_-]/g, "-").toLowerCase()}`;
}

/**
 * PRD #1062 M2 (#1036) — the optional context `checkpointPack` needs to build the
 * `.github/workflows` overlay wrapper commit. Supplied by the runner ONLY on GitHub and ONLY
 * on a path where the agent tree is already reaped (a PAT git op — `fetchDefaultTip` — runs
 * here). Undefined ⇒ `checkpointPack` ships the raw tracking tip, byte-for-byte as before.
 */
export interface CheckpointOverlayContext {
  /** The default branch name (already resolved the way the finalize align resolves it). */
  defaultBranch: string;
  /** The forge PAT for the authenticated default-tip fetch. */
  pat?: string;
  /** The repo clone URL, used to scope the PAT's HTTP header. */
  cloneUrl?: string;
  /** The forge bot username for HTTP Basic auth. */
  username?: string;
  /** The current tip of `refs/uzi-checkpoints/<branch>` as the run last knows it. When a
   *  40-hex value, it becomes the overlay's FIRST parent (base-first) so a second sequential
   *  overlay strictly descends the prior one and the broker accepts it as a fast-forward. */
  prevCheckpointTip?: string;
}

export interface RunnerClone {
  /** Separate from recovered baseCommit and any local checkpoint/bridge floor. */
  publicationFloor?: PublicationFloor;
  /** Absolute path to the runner clone's working tree (the ONLY working tree under
   *  (b) — the worker is bare-only). The agent checks out + commits here. */
  path: string;
  /** Branch the runner clone is on — `agent/issue-{iid}`, `ci-fix/…`, or `uzi/…`. */
  branch: string;
  /** How many commits the seeded branch already carries that the default branch does
   *  not. 0 when seeded off the default branch, and 0 when the count could not be
   *  taken (best-effort: this is prompt/feed colour, never load-bearing).
   *
   *  WHAT IT COUNTS DEPENDS ON `seededFrom`, and PRD #218 made that split load-bearing:
   *   - `"origin"`  — prior PUSHED work the clone was based off: an earlier completed
   *     run on the same issue, a RESUMED self_improve cycle's own previously-pushed
   *     commits (its branch is fresh-per-cycle, keyed on runId — #686 M8), or a
   *     human's commits. This is the original contract (issue #105): a run pushes
   *     exactly once, after the executor returns, and a fresh seed `fs.rm`s and
   *     re-clones, so a fresh claim never sees THIS attempt's commits here.
   *   - `"tracking"` — the commits RECOVERED from the interrupted attempt's own work,
   *     read back from `refs/uzi-runner/<branch>` (PRD #218 M2). This INVERTS the
   *     original "NOT the commits the interrupted attempt made" wording on purpose:
   *     on the tracking-ref leg the interrupted attempt's commits are exactly what the
   *     fetch-back preserved and the reseed is recovering, and M3 turns this number
   *     into a lead-facing "recovered N commit(s)" status.
   *   - `"default"` — 0 (the seed IS the default tip). */
  priorCommits: number;
  /** The commit the branch was checked out AT, resolved off the fresh remote-tracking ref
   *  in the bare (`refs/remotes/origin/*`, which every fetch updates) WHERE ONE EXISTS: the
   *  default branch's current tip for a new branch, the branch's own current origin tip on
   *  a resume. Full 40-char SHA.
   *
   *  "Where one exists" is load-bearing, not hedging. `defaultBranchRef` is a fallback chain
   *  and its last three rungs — `refs/heads/main`, `refs/heads/master`, then the bare's own
   *  `HEAD` — are the MIRROR-LAYOUT fallback, i.e. exactly the frozen refs this comment warns
   *  about. On those rungs `baseCommit` IS the frozen mirror, and so is `defaultBranchCommit`,
   *  since defaultBranchSha resolves through the same function.
   *
   *  How reachable that is, stated as what was and was not established: `cloneBare` rewrites
   *  the refspec and fetches before returning, so the remote-tracking refs exist by the time
   *  any normal run resolves a base. But its post-clone `fetch` is NOT covered by the
   *  `fs.rm` that cleans up a failed `clone`, so a bare whose first fetch died can persist
   *  on disk carrying `refs/heads/*` only. Whether a run then reaches `defaultBranchRef` in
   *  that state was NOT determined — it needs a warm bare in that condition. Treat this as a
   *  narrow window rather than a routine path, and do not assume the base is fresh when the
   *  bare has no `refs/remotes/origin/*`.
   *
   *  Load-bearing for the lead, which otherwise has to GUESS the branch's parent from the
   *  clone's local default branch. That ref is NOT a substitute, and not for the reason an
   *  earlier version of this comment gave: the clone's LOCAL `refs/heads/main` is a FROZEN
   *  MIRROR. `cloneBare` rewrites the refspec to `+refs/heads/*:refs/remotes/origin/*`, so the
   *  bare's own `refs/heads/*` never move after the first clone, and the runner clone inherits
   *  that frozen head as its local `main`. Its `origin/main` used to inherit the same frozen
   *  head, but `runnerCloneForBranch` now `update-ref`s `refs/remotes/origin/<default>` after
   *  checkout (issue #262) so the ratchet base is fresh; on a fresh run or ordinary resume that
   *  write is the fresh default head (`defaultBranchCommit`). Issue #313 CLAMPS it: on a resume
   *  leg where `defaultBranchCommit` resolves through the frozen `refs/heads/main` rung to a
   *  stale ancestor of `baseSha`, the ref is set to `baseSha` instead, so the ratchet base is
   *  never a strict ancestor of the branch base; a divergent-forward resume (main moved ahead)
   *  keeps `defaultBranchCommit`. The clone's local `refs/heads/main` is left untouched and
   *  stays frozen.
   *
   *  The drift therefore has no predictable direction — the mirror can sit at, behind, or
   *  ahead of this commit, one per topology — so neither `main..HEAD` nor `main...HEAD` is
   *  reliable, and prompt.ts's note forbids the ref NAME in both dot forms while predicting
   *  no symptom. (This comment used to assert `main..HEAD` reports the default branch's
   *  commits as a DELETION. Measured on a drifted bare, they are ADDITIONS, and the two
   *  dot forms return the same diff; see the block above baseCommitNote in prompt.ts.)
   *
   *  NOT the branch's fork point on a resume — see defaultBranchCommit, which is the
   *  distinction that makes the note correct on the runs that carry prior work. */
  baseCommit: string;
  /** The DEFAULT branch's current tip in the bare. Equal to `baseCommit` on a fresh
   *  branch (the seed IS the default tip); on a resume it is the other commit the lead
   *  may want, because `baseCommit` is then the branch's own previously-pushed tip and
   *  `baseCommit..HEAD` shows only what THIS run added.
   *
   *  Best-effort, exactly like priorCommits: a repo with no resolvable default branch
   *  yields undefined and the prompt says less rather than saying something false. */
  defaultBranchCommit?: string;
  /** Which leg of the reseed resolved the base (PRD #218 M2). `"origin"` when the
   *  branch already existed at origin, `"tracking"` when the base came from the
   *  worker-side tracking ref `refs/uzi-runner/<branch>` (the interrupted attempt's
   *  recovered work), `"default"` when neither applied and the seed is the default
   *  tip. Read by the runner to decide what M3's feed status says: `"tracking"` names
   *  the recovered commit count, `"default"` on a RESUME admits the tree was lost.
   *
   *  PRD #122 M8 adds `"checkpoint"`: on a NOT-ownedHere leg (cross-worker / fresh)
   *  the base came from origin's mirrored `refs/uzi-checkpoints/<branch>` — a DIFFERENT
   *  worker's brokered checkpoint that strictly descends the floor (origin branch, else
   *  default). It is a resume leg like `"tracking"`, so priorCommits counts the recovered
   *  commits. */
  seededFrom: "origin" | "tracking" | "default" | "checkpoint";
  /** PRD #122 M8 — true when a mirrored checkpoint ref existed but LOST to origin/default
   *  because it had diverged (not a strict descendant of the floor). Origin/default wins on
   *  divergence — never a silent merge or discard — and the runner emits a LOUD worker
   *  notice so the set-aside work is not lost silently. Absent/false on every other leg. */
  checkpointSetAside?: boolean;
  /** PRD #759 M2 — true when a `wip(park):` marker (WIP_PARK_COMMIT_PREFIX) was adopted as
   *  the base tip and `git reset --soft`'d back to uncommitted at adopt time, so the
   *  recovered content is present in the working tree as UNCOMMITTED changes and the marker
   *  is NOT in the history the agent builds on. Set on two legs: the same-worker /
   *  cross-worker-clean tracking/checkpoint leg (the adopted tip IS the marker, reset --soft
   *  onto its parent), and the cross-worker DIVERGED leg (the checkpoint marker's delta was
   *  cherry-pick --no-commit'd onto the new floor). Consumed by M5 (feed event distinguishing
   *  WIP-snapshot recovery from committed-milestone recovery) and M4 (recovery-success signal
   *  for the re-gate decision). Absent/false on every other leg. */
  wipRecovered?: boolean;
  /** issue #1783 M2: the attempt id when the clone was seeded at an ATTEMPT path (a
   *  Docker-wired worker); absent for a canonical-path seed. */
  attemptId?: string;
}

/**
 * Warm bare-clone cache (worker-owned) + per-run RUNNER CLONE lifecycle (PRD #51
 * M3, (b) separate-runner-clone).
 *
 * Layout under UZI_DATA_DIR:
 *   repos/<host>+<ns>+<repo>.git   — WORKER-ONLY warm bare clone per repo, kept
 *                                    across runs (config/hooks/refs/objects). The
 *                                    worker is BARE-ONLY: it clones/fetches with the
 *                                    PAT, fetches the agent branch BACK from the
 *                                    runner clone, tree-diffs, and pushes — it never
 *                                    runs worktree add / checkout.
 *   runner/<repo>/<key>            — RUNNER-OWNED clone per run (the ONLY working
 *                                    tree). Seeded from the worker bare via a local
 *                                    `clone --shared` (objects referenced read-only
 *                                    from the bare — the runner cannot corrupt the
 *                                    bare's objects through the alternate); the agent
 *                                    checks out + commits here. Removed on terminal.
 *                                    A Codex (sandboxed) clone is dissociated after
 *                                    setup (repack, alternates removed, issue #1769):
 *                                    its sandbox does not grant the bare.
 *
 * The (b) split closes the shared-git cross-uid channels by construction (B2): no
 * worker-side git ever reads a runner-owned config source (no worktree add checkout,
 * no shared commondir), and the worker fetches the agent branch back over the
 * pack-protocol `file://` transport (never the local-copy optimization that would
 * traverse a runner-planted objects/info/alternates — CVE-2022-39253 class).
 *
 * PAT handling (primary directive): the token is passed to authenticated git ops
 * (clone/fetch/push to origin) via env-scoped config (GIT_CONFIG_KEY/VALUE) only, so
 * it never lands in the process argv (visible via `ps`) and is never written to the
 * bare repo's on-disk config. It is never logged: runGit logs args only, and args
 * never carry the token. The runner-clone seed and the worker's fetch-BACK are LOCAL
 * (no credential).
 */
/** Trusted claim identity supplied by the worker, never reconstructed from runner config. */
type OwedContextIdentity = {
  runId: string;
  branch: string;
  kind: RunKind;
  barePath: string;
  defaultIdentity: { ref: string; sha: string };
};
export type PositiveOwedCandidateContext = OwedContextIdentity & { generation: number; legacy?: never };
export type OwedCandidateContext = PositiveOwedCandidateContext |
  (OwedContextIdentity & { generation: null; legacy: true });
export type HistoricalOwedContext = {
  runId: string; branch: string; barePath: string; generation: null;
  origin: "historical"; producer: "unknown"; kind: null; defaultIdentity: null;
};
type StoredOwedContext = OwedCandidateContext | HistoricalOwedContext;
export type CommittedTrackingOwnership =
  | { kind: "owned"; sha: string; context: OwedCandidateContext }
  | { kind: "not_owned" };
export interface FetchAgentBranchOptions {
  context: OwedCandidateContext;
  remotelyConfirmedSha?: string;
}
export type TrackingUpdateResult =
  | { kind: "updated"; trackingRef: string; candidateSha: string; retainedShas: string[];
      displacedSha?: string; divergence: "none" | "ancestor" | "divergent" | "unknown" | "foreign" }
  | { kind: "not_updated"; reason: "ownership_unknown" | "preservation_failed" | "owed_limit" |
      "receipt_pending_failed" | "tracking_update_failed" | "owner_stamp_failed" | "receipt_commit_failed" };
/** One validated read of a bare's owed metadata (see GitCache.readOwedMetadataUnderLock). */
interface OwedMetadata {
  names: string[];
  contexts: Map<string, StoredOwedContext>;
  records: Array<{ runId: string; sha: string; contextName: string; c: StoredOwedContext }>;
}
export interface OwedCandidate {
  sha: string;
  pinRef: string;
  contexts: StoredOwedContext[];
}
export interface RecoveryCoverage {
  sha: string;
  coverageRef: string;
  fingerprint: string;
  originalShas: string[];
  currentSha: string;
}
type TrackingReceipt = {
  version: 1; branch: string; runId: string; generation: number | null; context: string;
  trackingSha: string; phase: "pending" | "committed";
};
type CheckedTrackingOwner = { runId: string; generation?: number | null; context?: string } | undefined;
const OWED_RUN_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const OWED_OID = /^[0-9a-f]{40}$/;

/**
 * Hard cap on the heads one run may keep pinned as owed candidates (`refs/uzi-owed/<runId>/<sha>`) in
 * a worker bare. An untrusted agent that rewrites its branch repeatedly displaces one head per
 * checkpoint; without a bound the pins (which block `git gc` on the worker PVC), the candidate
 * metadata, and the feed announcements grow with the rewrite count. 64 is far above what a
 * well-behaved run holds between two confirmed checkpoints (a confirmed checkpoint releases every
 * pin it covers) and small enough that the full retained set stays recoverable and announceable in
 * a handful of feed rows. At the cap `pinOwedUnderLock` refuses a NEW pin; a head that is already
 * pinned never consumes capacity.
 */
export const MAX_OWED_CANDIDATES_PER_RUN = 64;

/** The refusal reason for a failed pin: the cap is distinguished so the runner can stop the run. */
function preservationReason(cause: unknown): "owed_limit" | "preservation_failed" {
  return cause instanceof OwedCandidateLimitError ? "owed_limit" : "preservation_failed";
}

/** Thrown under the bare lock by `pinOwedUnderLock` at {@link MAX_OWED_CANDIDATES_PER_RUN}; every
 *  pin-producing caller maps it to the `owed_limit` refusal rather than a generic failure. */
class OwedCandidateLimitError extends Error {
  constructor() {
    super("owed candidate limit reached");
    this.name = "OwedCandidateLimitError";
  }
}

export type InventoryReadCause =
  | "attribution_unreadable" | "clone_ancestor_invalid" | "clone_path_invalid"
  | "clone_head_unreadable" | "git_or_filesystem_error" | "other";

export class GitCache {
  private readonly reposRoot: string;
  /** Runner-owned clone store (the working trees). A distinct /data subtree from the
   *  worker-only `repos/` bare cache, so the M3 ownership carve-out is a clean
   *  boundary: `runner/` is runner-writable, `repos/` stays worker-only. */
  private readonly runnerRoot: string;
  /** Planning-capture budgets handed to the helper; the clone never supplies them. */
  private planningLimits = { snapshotLimit: PLANNING_SNAPSHOT_LIMIT, totalLimit: PLANNING_TOTAL_LIMIT };
  /** issue #1315 — worker-only 0700 holding subtree for atomically-released clones.
   *  A SIBLING of runnerRoot under the SAME dataDir PATH, but OUTSIDE the runner-writable
   *  `runner/<repo>` tree: a retired clone lands here where the untrusted runner uid
   *  cannot reach it. NOTE (issue #1354): sharing the dataDir path does NOT imply the same
   *  device — on a docker-lane (dind) worker `runner/` is a separate emptyDir while this
   *  quarantine sits on the `/data` PVC, so a rename from runnerRoot into here CAN hit
   *  EXDEV; retireRunnerClone carries an intra-device fallback for exactly that. Terminal
   *  trash is disposed from here; a foreign quarantine is retained here for forensics. */
  private readonly runnerHoldingRoot: string;
  /** PRD #1296 M3 — the durable durable-recovery journal + verified-bundle store, a
   *  DISTINCT /data subtree from `repos/` (worker bare) and `runner/` (torn-down clones),
   *  and separate from the unrelated issue #1187 git-config recovery-capture journal.
   *  It survives runner-clone removal and worker restart, so a terminal/restart retry can
   *  re-upload the exact journaled bytes with no forge PAT (D5). Worker-owned. */
  readonly recoveryRoot: string;
  /** issue #1582 M2 — the authenticated ancestry-settlement journal (one record per adopted
   *  predecessor hold). A SIBLING of `recoveryRoot`, never inside it: the recovery restart sweep
   *  treats every entry under `recovery/` as a runId. Worker-owned. */
  readonly recoverySettlementRoot: string;
  /** issue #2213 — the worker-owned 0700 store of verified quarantine archives
   *  (`<root>/<runId>/g<generation>.bundle` plus its manifest). A SIBLING of `recovery/`, never
   *  inside it: it is not custody (nothing reads it to release or retire anything), and the
   *  recovery restart sweep treats every entry under `recovery/` as a runId. */
  readonly recoveryArchiveRoot: string;
  /** Per-bare-path serialization: git's lockfiles can't take parallel mutations. */
  private readonly locks = new Map<string, Promise<unknown>>();
  private readonly boundaryProcesses = new AsyncLocalStorage<BoundaryProcessScope>();
  private readonly recoveryOperations = new AsyncLocalStorage<{ signal: AbortSignal; deadline: number; closureBudget: ClosureBudget }>();

  /** Local recovery clock, including queued locks and actual child settlement. */
  async withRecoveryOperation<T>(signal: AbortSignal, deadline: number, action: (signal: AbortSignal) => Promise<T>): Promise<T> {
    const timeout = new AbortController();
    const combined = AbortSignal.any([signal, timeout.signal]);
    const timer = setTimeout(() => timeout.abort(new Error("recovery deadline exhausted")), Math.max(0, deadline - Date.now()));
    if (Date.now() >= deadline) timeout.abort(new Error("recovery deadline exhausted"));
    try {
      combined.throwIfAborted();
      return await this.recoveryOperations.run({ signal: combined, deadline, closureBudget: this.recoveryOperations.getStore()?.closureBudget ?? { delivered: 0, limit: RECOVERY_DECODED_LIMIT } }, () => action(combined));
    } finally { clearTimeout(timer); }
  }
  /** issue #1597 M2: the gitleaks executable (see {@link GitCacheOptions.gitleaksBin}). */
  private readonly gitleaksBin: string;
  private readonly maxOwedCandidates: number;
  /** See {@link GitCacheOptions.scratchProvisioner}; undefined in production. */
  private readonly scratchProvisioner: ((clonePath: string) => Promise<void>) | undefined;
  /** See {@link GitCacheOptions.retentionDelete}; undefined in production. */
  private readonly retentionDeleteSeam: GitCacheOptions["retentionDelete"];
  private readonly terminalRecordProtection: GitCacheOptions["terminalRecordProtection"];
  /** See {@link GitCacheOptions.canonicalFree}; undefined in production. */
  private readonly canonicalFreeSeam: GitCacheOptions["canonicalFree"];
  /** issue #1783 (N1): the targets (resolved) of every runner-uid delete that settled as a
   *  {@link RunnerDeleteUnsettledError}, each counted until its child's 'close' arrives. In-process
   *  only: the rm walks by path, so a canonical free or reseed at such a path is refused
   *  ({@link assertNoUnsettledDelete}), and the retention sweep keeps such a path or anything
   *  under it ({@link deleteRetainedArtifact}), for as long as that rm may still be running. An
   *  entry is logged (warn) when it is recorded and (info) when its 'close' removes it. */
  private readonly unsettledDeletes = new Map<string, number>();
  /** issue #1597 M2: memoised `--remerge-diff` support probe. */
  private remergeProbe: Promise<boolean> | undefined;

  constructor(
    dataDir: string,
    private readonly log: Logger,
    /** Test-only seam for the ensureClone network-op retry. Undefined in production,
     *  so withForgeRetry falls back to its own FORGE_RETRY_SCHEDULE + real sleep. */
    private readonly retry?: { schedule?: number[]; sleep?: (ms: number) => Promise<void> },
    opts: GitCacheOptions = {},
  ) {
    this.gitleaksBin = opts.gitleaksBin ?? "gitleaks";
    this.maxOwedCandidates = opts.maxOwedCandidates ?? MAX_OWED_CANDIDATES_PER_RUN;
    this.scratchProvisioner = opts.scratchProvisioner;
    this.retentionDeleteSeam = opts.retentionDelete;
    this.canonicalFreeSeam = opts.canonicalFree;
    this.reposRoot = path.join(dataDir, "repos");
    this.runnerRoot = path.join(dataDir, "runner");
    this.terminalRecordProtection = opts.terminalRecordProtection;
    this.runnerHoldingRoot = path.join(dataDir, "runner-quarantine");
    this.recoveryRoot = path.join(dataDir, "recovery");
    this.recoverySettlementRoot = path.join(dataDir, "recovery-settlement");
    this.recoveryArchiveRoot = path.join(dataDir, "recovery-archive");
  }

  /** Scope every subprocess and bare-lock acquisition created by `action` to the
   * permit-bound supervisor and deadline. AsyncLocalStorage keeps concurrent runs
   * isolated while leaving every non-Codex call on the literal legacy path. */
  withBoundaryProcessSpawner<T>(
    spawner: BoundaryProcessSpawner,
    signal: AbortSignal,
    action: () => Promise<T>,
    hooks: { beforeLockRelease?: (key: string) => Promise<void>; softSignal?: AbortSignal; softDeadlineAt?: number } = {},
  ): Promise<T> {
    return this.boundaryProcesses.run(
      { spawn: spawner, signal, ...hooks },
      action,
    );
  }

  /** issue #1597 M2: run `fn` holding the per-bare serialization lock (the same one every bare
   *  mutation takes). Used by the mid-turn checkpoint tick to reconcile a cancelled child's lock
   *  files OUTSIDE its (already-aborted) boundary scope, so no other bare op interleaves. */
  withBareLock<T>(barePath: string, fn: () => Promise<T>): Promise<T> {
    return this.withLock(barePath, fn);
  }

  /**
   * issue #1597 M2 — is a git operation in progress in the runner clone? A metadata-only probe of
   * the clone's gitdir (no git child, no credentials): resolves the gitdir (a `.git` FILE carrying
   * `gitdir: <path>` is followed, relative to the clone), then looks for each marker in
   * {@link RUNNER_CLONE_BUSY_MARKERS} plus `refs/heads/<branch>.lock`. ANY observed marker is busy,
   * whatever its age — there is deliberately no staleness heuristic (an old `index.lock` may belong
   * to a still-running agent git). A probe that cannot read the gitdir (EACCES, a malformed or
   * oversized `.git` file, a symlink/FIFO/socket where `.git` should be, a missing clone) reports
   * busy with the single marker `unreadable`, so a tick never fetches from a clone it could not
   * inspect.
   *
   * The clone is AGENT-CONTROLLED, so the probe must never block (review item 2): `.git` is opened
   * ONCE with O_NOFOLLOW|O_NONBLOCK and classified by `fstat` on that fd (see readGitEntryNoBlock —
   * no stat-then-open window), a gitfile is read through the same fd (at most
   * {@link GITFILE_MAX_BYTES}), and every other check is an `lstat` (never opens, never follows the
   * final symlink). A FIFO at `.git` therefore opens without waiting for a writer and is rejected,
   * instead of parking a libuv thread forever.
   */
  async runnerCloneBusy(clonePath: string, branch: string): Promise<RunnerCloneBusy> {
    const unreadable = (err: unknown): RunnerCloneBusy => {
      this.log.warn("runner clone busy probe could not read the gitdir; treating as busy", {
        clone: clonePath,
        error: gitErrorMessage(err),
      });
      return { busy: true, markers: ["unreadable"] };
    };
    let gitdir = path.join(clonePath, ".git");
    try {
      const entry = await readGitEntryNoBlock(gitdir, GITFILE_MAX_BYTES);
      if (entry.kind === "file") {
        const m = /^gitdir:\s*(.+)$/m.exec(entry.text.trim());
        if (!m) return unreadable(new Error("malformed .git file"));
        gitdir = path.resolve(clonePath, m[1]!.trim());
        if (!(await fs.lstat(gitdir)).isDirectory()) return unreadable(new Error("gitfile target is not a directory"));
      } else if (entry.kind !== "dir") {
        return unreadable(new Error(".git is neither a regular file nor a directory"));
      }
    } catch (err) {
      return unreadable(err);
    }
    const candidates: string[] = [...RUNNER_CLONE_BUSY_MARKERS, `refs/heads/${branch}.lock`];
    const markers: string[] = [];
    for (const marker of candidates) {
      try {
        await fs.lstat(path.join(gitdir, marker));
        markers.push(marker);
      } catch (err) {
        const code = (err as NodeJS.ErrnoException).code;
        if (code === "ENOENT" || code === "ENOTDIR") continue;
        return unreadable(err);
      }
    }
    return { busy: markers.length > 0, markers };
  }

  barePathFor(repoUrl: string): string {
    return path.join(this.reposRoot, bareDirName(repoUrl));
  }

  /** Resolve a recovery journal basename to a real bare directly under the private repos root. */
  async resolveRecoveryBareDir(bareDir: string): Promise<string | undefined> {
    if (
      !bareDir ||
      bareDir === "." ||
      bareDir === ".." ||
      bareDir !== path.basename(bareDir) ||
      bareDir.includes("\\") ||
      bareDir.includes("\0")
    ) {
      return undefined;
    }
    const barePath = path.join(this.reposRoot, bareDir);
    if (path.dirname(barePath) !== this.reposRoot) return undefined;
    try {
      const st = await fs.lstat(barePath);
      if (st.isSymbolicLink() || !st.isDirectory()) return undefined;
      const [realBare, realRoot] = await Promise.all([fs.realpath(barePath), fs.realpath(this.reposRoot)]);
      if (path.dirname(realBare) !== realRoot) return undefined;
    } catch {
      return undefined;
    }
    if (!(await isBareRepo(barePath))) return undefined;
    return barePath;
  }

  /**
   * issue #1742 D4(a) — resolve a journaled bare-dir BASENAME (the `bareDirName` recorded on a
   * finalization-pinned recovery record) to the private bare under this cache's repos root and
   * classify the record's pinned `sourceSha` against it, for the restart sweep. `reposRoot` is
   * private, so the sweep cannot build the path itself; this is the only door, and it refuses a
   * name that is not a plain basename (no separator, not `.`/`..`) so a tampered-but-re-MACed
   * record can never point outside the repos root. The bare must ALSO be a real directory (not a
   * symlink) whose realpath sits DIRECTLY under `realpath(reposRoot)`, so a symlinked
   * `<name>.git` that points elsewhere is `missing_bare` too. `defaultBranch` is only used as a
   * ref name after a strict character check plus `git check-ref-format`.
   *   - `missing_bare`: the name is unsafe or no bare with that name exists;
   *   - `missing_sha`: the bare lacks `sourceSha` as a commit (not verifiable);
   *   - `on_default`: `sourceSha` is an ancestor of the bare's `<defaultBranch>` ref
   *     (`refs/remotes/origin/<defaultBranch>`, which every fetch updates, or the mirror-layout
   *     `refs/heads/<defaultBranch>`), so it is already published there;
   *   - `unpublished`: present in the bare and reachable from neither default ref (a missing
   *     default ref counts as not reachable).
   * Read-only; runs under the bare lock so it never observes a half-written object set.
   */
  async resolveRestartSource(
    bareDir: string,
    sourceSha: string,
    defaultBranch: string,
  ): Promise<{ status: "missing_bare" | "missing_sha" | "on_default" | "unpublished"; barePath?: string }> {
    const barePath = await this.resolveRecoveryBareDir(bareDir);
    if (!barePath) return { status: "missing_bare" };
    if (!/^[0-9a-f]{40}$/.test(sourceSha)) return { status: "missing_sha", barePath };
    return this.withLock(barePath, async () => {
      if ((await this.tryGit(barePath, ["rev-parse", "--verify", "--quiet", `${sourceSha}^{commit}`])) !== 0) {
        return { status: "missing_sha" as const, barePath };
      }
      const db = defaultBranch.trim();
      if (await this.isPlainBranchName(barePath, db)) {
        for (const ref of [`refs/remotes/origin/${db}`, `refs/heads/${db}`]) {
          if (!(await this.refExists(barePath, ref))) continue;
          if ((await this.tryGit(barePath, ["merge-base", "--is-ancestor", sourceSha, ref])) === 0) {
            return { status: "on_default" as const, barePath };
          }
        }
      }
      return { status: "unpublished" as const, barePath };
    });
  }

  /** A journaled default-branch name safe to splice into a ref: printable ASCII only, no leading
   *  `-`, no `@{`, none of the ref-format metacharacters, and accepted by
   *  `git check-ref-format refs/heads/<name>` (which does no `@{-N}` expansion). */
  private async isPlainBranchName(barePath: string, name: string): Promise<boolean> {
    if (!name || name.startsWith("-") || name.includes("@{")) return false;
    if (/[\s~^:?*[\\]|\.\./.test(name)) return false;
    for (const ch of name) {
      const c = ch.charCodeAt(0);
      if (c < 0x21 || c > 0x7e) return false;
    }
    return (await this.tryGit(barePath, ["check-ref-format", `refs/heads/${name}`])) === 0;
  }

  /** Clone the repo bare if absent, else fetch to refresh. Returns the bare path. */
  async ensureClone(repoUrl: string, pat?: string, username?: string): Promise<string> {
    const barePath = this.barePathFor(repoUrl);
    const scope = httpScopeForUrl(repoUrl);
    return this.withLock(barePath, async () => {
      await fs.mkdir(this.reposRoot, { recursive: true });
      if (await isBareRepo(barePath)) {
        this.log.info("repo cache: fetching", { bare: barePath });
        // Issue #134: reassert IDEMPOTENTLY on the warm path, BEFORE the fetch (the first
        // object-writing command, hence the first spawner). cloneBare only runs on the very
        // first clone, and `/data` is persistent — a per-worker PVC in k8s, the `agentdata`
        // volume under compose — so every bare on an already-deployed worker would otherwise
        // never receive these keys at all. That is the exact case the bare-repo reasoning
        // below is about, so writing it only in cloneBare left it applied to none of them.
        await this.disableAutoMaintenance(barePath);
        await withForgeRetry(() => this.fetch(barePath, pat, scope, username), {
          schedule: this.retry?.schedule,
          sleep: this.retry?.sleep,
          log: this.log,
          label: "clone/fetch",
        });
      } else {
        this.log.info("repo cache: cloning bare", { url: repoUrl, bare: barePath });
        await withForgeRetry(() => this.cloneBare(repoUrl, barePath, pat, scope, username), {
          schedule: this.retry?.schedule,
          sleep: this.retry?.sleep,
          log: this.log,
          label: "clone/fetch",
        });
      }
      return barePath;
    });
  }

  /**
   * Push the run's branch to origin using the PAT. This is a WORKER-owned
   * authenticated op — the agent never has a push credential — so it runs here,
   * not through the SDK's guardrailed Bash. The PAT rides the host-scoped
   * extraHeader in the env (off argv, off disk), and the push is never forced.
   * Idempotent on resume: a branch already at origin pushes as up-to-date.
   *
   * Under (b) the agent's commit lives in the RUNNER clone, so the source is the
   * worker-side tracking ref `fetchAgentBranch` wrote (refs/uzi-runner/<branch>),
   * NOT refs/heads/<branch> — the caller MUST fetchAgentBranch first. The ref is
   * resolved to a pinned commit before pushing, keeping the runner's branch out
   * of the bare's heads namespace (B2 invariant 2).
   */
  async pushBranch(
    barePath: string, branch: string, pat: string, repoUrl: string, username?: string,
    publication?: PublicationCandidate,
  ): Promise<void> {
    const scope = httpScopeForUrl(repoUrl);
    await this.withLock(barePath, async () => {
      const tip = publication?.candidate ?? await this.resolveCommitStrict(barePath, runnerTrackingRef(branch));
      const candidate = await this.scratchPublicationPreflight(barePath, branch, tip);
      // Legacy callers have no claim floor and cannot produce a supersession proof.
      const group = publication ?? { floor: { kind: "absent" } as const, originalHead: candidate, candidate };
      await this.refreshScratchPublicationFloor(barePath, branch, group, pat, scope, username);
      try {
        await this.runGit(barePath, ["push", "origin", `${candidate}:refs/heads/${branch}`], pat, scope, username);
      } catch (error) {
        // Stderr only selects a new complete proof; it never establishes movement.
        if (isNonFastForwardRejection(error)) {
          await this.scratchPublicationPreflight(barePath, branch, candidate);
          await this.refreshScratchPublicationFloor(barePath, branch, group, pat, scope, username);
        }
        throw error;
      }
    });
  }

  private async capturePublicationFloor(barePath: string, ref: string): Promise<PublicationFloor> {
    try {
      const { stdout } = await this.execScoped("git", withDir(barePath, [
        "rev-parse", "--verify", "--quiet", ref,
      ]), { env: gitEnv(), timeout: GIT_TIMEOUT_MS });
      const oid = stdout.trim();
      if (!SHA40_RE.test(oid) || await this.resolveCommitStrict(barePath, ref) !== oid) return { kind: "unverified" };
      return Object.freeze({ kind: "pinned", oid });
    } catch (error) {
      const failure = classifyExecFailure(error);
      if (failure.exitCode === 1 && !failure.stdout.trim() && !failure.stderr.trim()) return { kind: "absent" };
      return { kind: "unverified" };
    }
  }

  /** Publication-path commit resolver: unlike revParse it never reads an exec failure as
   *  "absent". Exit 1 with no output is the only absent answer; the caller passes a ref
   *  and `^{commit}` is appended here. */
  private async resolveCommitStrict(barePath: string, ref: string): Promise<string> {
    let stdout: string;
    try {
      ({ stdout } = await this.execScoped("git", withDir(barePath, [
        "rev-parse", "--verify", "--quiet", `${ref}^{commit}`,
      ]), { env: gitEnv(), timeout: GIT_TIMEOUT_MS }));
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      const failure = classifyExecFailure(cause);
      if (failure.exitCode === 1 && !failure.stdout.trim()) {
        throw new ScratchPublicationError("candidate commit is unavailable", cause, {
          kind: "tip_unavailable", step: "resolve_tip", detail: "ref does not resolve to a commit",
        });
      }
      throw new ScratchPublicationError(PUBLICATION_UNPROVEN, cause, {
        kind: "exec_failed", step: "resolve_tip", detail: failure.detail, rawDetail: failure.rawDetail,
      });
    }
    const sha = stdout.trim();
    if (!SHA40_RE.test(sha)) {
      throw new ScratchPublicationError(PUBLICATION_UNPROVEN, undefined, {
        kind: "exec_failed", step: "resolve_tip", detail: oneLine(`unexpected rev-parse output: ${sha}`),
        rawDetail: rawText(`unexpected rev-parse output: ${sha}`),
      });
    }
    return sha;
  }

  /** Refusal for a failed scratch-history check; a boundary abort or a typed refusal passes through. */
  private publicationFailure(
    cause: unknown, step: ScratchPublicationStep, kind: ScratchPublicationKind = "exec_failed",
  ): Error {
    const abort = this.boundaryAbortError(cause);
    if (abort) return abort;
    if (cause instanceof ScratchPublicationError) return cause;
    return new ScratchPublicationError(PUBLICATION_UNPROVEN, cause, {
      kind, step, ...execDetail(classifyExecFailure(cause)),
    });
  }

  /** Public bridge entry point: validate the exact commit before changing custody. */
  async scratchPublicationPreflight(barePath: string, branch: string, candidateSha?: string): Promise<string> {
    try {
      let candidate: string;
      if (candidateSha === undefined) {
        candidate = await this.resolveCommitStrict(barePath, runnerTrackingRef(branch));
      } else {
        if (!SHA40_RE.test(candidateSha)) {
          throw new ScratchPublicationError("candidate commit is unavailable", undefined, {
            kind: "tip_unavailable", step: "resolve_tip", detail: "candidate is not a 40-hex commit SHA",
          });
        }
        candidate = await this.resolveCommitStrict(barePath, candidateSha);
        if (candidate !== candidateSha) {
          throw new ScratchPublicationError("candidate commit is unavailable", undefined, {
            kind: "tip_unavailable", step: "resolve_tip", detail: "resolved commit differs from candidate",
          });
        }
      }
      try {
        const { stdout: shallow } = await this.execScoped("git", withDir(barePath, ["rev-parse", "--is-shallow-repository"]),
          { env: gitEnv(), timeout: GIT_TIMEOUT_MS });
        if (shallow.trim() !== "false") {
          throw new ScratchPublicationError(PUBLICATION_UNPROVEN, undefined, {
            kind: "shallow_history", step: "shallow_check", detail: "history is shallow",
          });
        }
      } catch (cause) {
        throw this.publicationFailure(cause, "shallow_check");
      }
      // Walk all reachable objects without collecting object names. Missing objects,
      // a deadline, or output overflow must all refuse publication.
      try {
        await this.execScoped("git", withDir(barePath, [
          "rev-list", "--objects", "--missing=error", "--quiet", candidate,
        ]), { env: gitEnv(), timeout: 10_000, maxBuffer: 4_096 });
      } catch (cause) {
        const failure = classifyExecFailure(cause);
        const gitExit = failure.exitCode !== undefined && failure.exitCode !== 0;
        throw this.publicationFailure(cause, "object_walk",
          !gitExit ? "exec_failed" : MISSING_OBJECT_RE.test(failure.stderr) ? "missing_objects" : "object_walk_failed");
      }
      // Full history preserves merged side branches and root commits. The pathspec
      // matches both the exact file/symlink and everything below the directory.
      let touched: string;
      try {
        ({ stdout: touched } = await this.execScoped("git", withDir(barePath, [
          "rev-list", "--full-history", "--max-count=1", candidate, "--", ".uzi/scratch",
        ]), { env: gitEnv(), timeout: 10_000, maxBuffer: 4_096 }));
      } catch (cause) {
        throw this.publicationFailure(cause, "scratch_walk");
      }
      if (touched.trim()) {
        throw new ScratchPublicationError(PUBLICATION_UNPROVEN, undefined, {
          kind: "scratch_present", step: "scratch_walk", detail: ".uzi/scratch appears in candidate history",
        });
      }
      return candidate;
    } catch (cause) {
      // An inner check already mapped an abort; re-mapping would wrap the wrapper.
      if (cause instanceof GitBoundaryAbortError || cause instanceof CheckpointSoftDeadlineError) throw cause;
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      if (cause instanceof ScratchPublicationError) throw cause;
      throw new ScratchPublicationError(PUBLICATION_UNPROVEN, cause, {
        kind: "exec_failed", ...execDetail(classifyExecFailure(cause)),
      });
    }
  }

  private async refreshScratchPublicationFloor(
    barePath: string, branch: string, group: PublicationCandidate, pat: string,
    scope: string | undefined, username?: string,
  ): Promise<void> {
    const remoteRef = `refs/heads/${branch}`;
    const scratchRef = `refs/uzi-publication-floor/${branch}`;
    const refuse = (kind: ScratchPublicationKind): never => {
      throw new ScratchPublicationError("cannot verify fresh remote floor", undefined, {
        kind, step: "floor_refresh",
      });
    };
    const observe = async (): Promise<string | null> => {
      const listed = (await this.runGit(barePath, ["ls-remote", "origin", remoteRef], pat, scope, username)).trim();
      if (!listed) return null;
      const oid = listed.slice(0, 40);
      if (!SHA40_RE.test(oid) || listed !== `${oid}\t${remoteRef}`) refuse("floor_unverified");
      return oid;
    };
    const relation = async (floor: string, tip: string): Promise<"ancestor" | "divergent"> => {
      const answer = await this.ancestry(barePath, floor, tip);
      if (answer === "unknown") refuse("floor_unverified");
      return answer as "ancestor" | "divergent";
    };
    try {
      if (group.floor.kind === "unverified") refuse("floor_unverified");
      const R = await observe();
      if (R === null) {
        if (group.floor.kind !== "absent") refuse("floor_unverified");
        if (await observe() !== null) refuse("remote_changed_during_refresh");
        return;
      }
      await this.runGit(barePath, ["fetch", "--refmap=", "origin", `+${remoteRef}:${scratchRef}`], pat, scope, username);
      const fetched = await this.resolveCommitStrict(barePath, scratchRef).catch((error: unknown) => {
        if (error instanceof RunResidueBlockedError || isAbortLike(error)) throw error;
        throw new ScratchPublicationError("cannot verify fresh remote floor", error, {
          kind: "floor_unverified", step: "floor_refresh",
          ...(error instanceof ScratchPublicationError
            ? { detail: error.detail, rawDetail: error.rawDetail }
            : execDetail(classifyExecFailure(error))),
        });
      });
      if (fetched !== R) refuse("remote_changed_during_refresh");
      const { originalHead: H, candidate: C, floor } = group;
      if (floor.kind === "pinned" && await relation(floor.oid, R) !== "ancestor") refuse("floor_unverified");
      const remoteInCandidate = await relation(R, C);
      if (remoteInCandidate === "ancestor") {
        if (await observe() !== R) refuse("remote_changed_during_refresh");
        return;
      }
      if (floor.kind !== "pinned") return refuse("new_remote_candidate_diverged");
      const P = floor.oid;
      // Check H separately: an ancestry bridge in C cannot rehabilitate rewritten H.
      if (P === R || await relation(P, H) !== "ancestor" || await relation(P, C) !== "ancestor") {
        refuse("remote_candidate_diverged");
      }
      await this.scratchPublicationPreflight(barePath, branch, R);
      if (await observe() !== R) refuse("remote_changed_during_refresh");
      throw new RemoteBranchAdvancedError({ P, H, C, R, cause: "remote_branch_advanced" });
    } catch (cause) {
      if (cause instanceof RunResidueBlockedError) throw cause;
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      if (cause instanceof ScratchPublicationError) throw cause;
      throw new ScratchPublicationError("cannot verify fresh remote floor", cause, {
        kind: "floor_unverified", step: "floor_refresh", ...execDetail(classifyExecFailure(cause)),
      });
    } finally {
      // One local cleanup attempt per proof; failure does not authorize publication.
      await this.runGit(barePath, ["update-ref", "-d", scratchRef]).catch((cause: unknown) => {
        if (cause instanceof RunResidueBlockedError || isAbortLike(cause)) throw cause;
        throw new ScratchPublicationError("cannot clean publication floor", cause, {
          kind: "floor_unverified", step: "floor_refresh", ...execDetail(classifyExecFailure(cause)),
        });
      });
    }
  }

  /** The default branch's short name (e.g. `main`), for an MR target. */
  async defaultBranchName(barePath: string): Promise<string | undefined> {
    const ref = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!ref) return undefined;
    return ref.replace(/^refs\/remotes\/origin\//, "").replace(/^refs\/heads\//, "") || undefined;
  }

  /**
   * Seed the run's RUNNER CLONE on branch `agent/issue-{iid}` (PRD #51 M3). The
   * working tree lives ONLY here (the worker is bare-only). If the branch already
   * exists at origin (a resume, or a prior run on the same issue — fetched into the
   * bare's refs/remotes/origin/<branch>), the clone's branch is based off that fresh
   * tip so successive runs build on prior work; else off the repo's default branch.
   */
  async createOrAttachRunnerClone(
    barePath: string,
    issueIid: number,
    reseed: CanonicalReseedOptions,
    runId?: string,
    resume = false,
    expectedCheckpointTip?: string,
    attempt?: AttemptSeedOptions,
    opts?: { selfContained?: boolean },
  ): Promise<RunnerClone> {
    return this.runnerCloneForBranch(barePath, `agent/issue-${issueIid}`, `issue-${issueIid}`, reseed, runId, resume, expectedCheckpointTip, attempt, opts);
  }

  /** The canonical runner-clone path for a clone key under a bare's repo dir: the single
   *  definition of `<runnerRoot>/<repoDir>/<key>`. runnerCloneForBranch and the runner's
   *  owner-canonical reclaim validation (issue #1319) both derive it here, so predicate (d)
   *  compares against exactly what the seed creates. */
  runnerClonePath(barePath: string, key: string): string {
    const repoDir = path.basename(barePath).replace(/\.git$/, "");
    return path.join(this.runnerRoot, repoDir, key);
  }

  /** A report-only checker owns a fresh clone and selects only the immutable candidate SHA. */
  async runnerCloneAtCommit(barePath: string, baseCommit: string, runId: string): Promise<string> {
    if (!/^[a-f0-9]{40}$/.test(baseCommit) || !/^[a-zA-Z0-9_-]+$/.test(runId)) {
      throw new Error("invalid exact-commit checker identity");
    }
    return this.withLock(barePath, async () => {
      const resolved = (await this.runGit(barePath, ["rev-parse", "--verify", `${baseCommit}^{commit}`])).trim();
      if (resolved !== baseCommit) throw new Error("checker base commit unavailable");
      const key = `cross-check-${runId}-${randomUUID()}`;
      const clonePath = this.runnerClonePath(barePath, key);
      await fs.mkdir(path.dirname(clonePath), { recursive: true });
      try {
        await this.runGitAsRunner(undefined, ["clone", "--shared", "--no-checkout", barePath, clonePath]);
        await this.disableAutoMaintenance(clonePath, true);
        await this.runGitAsRunner(clonePath, ["checkout", "--detach", baseCommit]);
        await this.materializeRunnerClone(clonePath, [baseCommit]);
        const head = (await this.runGitAsRunner(clonePath, ["rev-parse", "--verify", "HEAD^{commit}"])).trim();
        if (head !== baseCommit) throw new Error("checker checkout does not match candidate base");
        return clonePath;
      } catch (err) {
        await this.removeRunnerClone(clonePath).catch((cleanup) =>
          this.log.warn("checker clone cleanup failed", { error: gitErrorMessage(cleanup) }));
        throw err;
      }
    });
  }

  /**
   * Seed a RUNNER CLONE for an EXPLICIT branch — the PRD #6 ci_fix targets (a fresh
   * `ci-fix/pipeline-{id}` off the default branch, or an existing `agent/issue-{iid}`
   * run branch, updating its MR) and the PRD #46 self_improve branch.
   * `key` names the on-disk clone dir (branch names carry `/`, so callers pass a
   * filesystem-safe key). The cross-kind same-branch exclusion (server-side) plus the
   * per-run clone dir means a stale dir is simply removed and recloned.
   *
   * Base resolution (PRD #218 M2). Three candidates: `refs/remotes/origin/<branch>`
   * (pushed work), the worker-side tracking ref `refs/uzi-runner/<branch>` (an
   * interrupted attempt's fetched-back work), and the default branch. The tracking ref
   * is consulted ONLY when it is OWNED BY THIS RUN — its stamp (see
   * runnerTrackingOwnerKey) equals `runId`. That anchor is what stops a fresh run, or a
   * different run on the same issue, from inheriting a dead run's orphan ref (it would
   * reintroduce issue #105's silent redo through this very fix). `ownedHere` gates the
   * WHOLE tracking-ref consideration:
   *   - NOT owned here (a fresh run, a stale ref from another run, or `runId` undefined):
   *     the tracking ref is IGNORED entirely — origin/<branch> if it exists, else the
   *     default. This is today's pre-#218 behaviour.
   *   - owned here, origin AND tracking both exist: the tracking ref ONLY when
   *     `merge-base --is-ancestor origin <tracking>` holds (it strictly descends from
   *     origin); on divergence, origin — another worker pushed and silently preferring
   *     local work would drop a published commit.
   *   - owned here, tracking exists but no origin branch (the first-park case): the
   *     tracking ref, with NO ancestry test — there is no competing published work to
   *     protect and the current default tip is not a meaningful reference (it may have
   *     moved far past the fork point, which is exactly the case a uniform ancestor
   *     test discards the recovered work on).
   *
   * PRD #122 M8 adds a FOURTH candidate, `refs/uzi-checkpoints/<branch>`, the CROSS-WORKER
   * signal path: a DIFFERENT worker brokered its checkpoint to origin (M8 publish), and
   * `fetch()` mirrored origin's checkpoint refs into this bare. It matters ONLY on the
   * NOT-ownedHere legs — when THIS run owns the local tracking ref, that ref is
   * equal-or-ahead of any checkpoint (the checkpoint was pushed FROM the tracking state),
   * so the checkpoint adds nothing and the ownedHere legs above are unchanged. On a
   * not-ownedHere leg the floor is the origin branch if pushed, else the default; the
   * checkpoint is preferred ONLY when it is THIS run's own (the owner anchor below) AND
   * strictly descends that floor. On divergence of an owned checkpoint origin/default WINS —
   * never a silent merge or discard — and `checkpointSetAside` is set so the runner emits a
   * loud notice.
   * The git layer is claim-agnostic: `runId` is a plain string the runner threads from
   * `claim.run_id`; nothing here knows about sessions. `runId` undefined ⇒ never owned ⇒
   * today's behaviour, which keeps the no-runId test call sites compiling.
   *
   * PRD #1030 M3 — `resume` is a SEPARATE additional signal the runner threads from
   * `claim.session_id != null` (a resume, not a fresh first attempt). It is NOT the runId
   * ownership anchor and does not affect the ownedHere legs. It relaxes ONLY the
   * cross-worker checkpoint-adoption rule on the not-ownedHere leg: on a resume with NO
   * `origin/<branch>` (an unpushed branch), a mirrored checkpoint that shares history with
   * the default is adopted with no strict-descendant test against the current default, which
   * would wrongly discard a valid checkpoint when `main` advanced during the park. The strict
   * test is KEPT when `origin/<branch>` exists (genuinely competing published work) and for
   * every fresh (non-resume) run. `resume` defaults false ⇒ today's behaviour, which keeps
   * the existing test call sites compiling.
   *
   * issue #1042 M4 / #1059 M1 — checkpoint adoption is gated on an OWNER ANCHOR:
   * `expectedCheckpointTip` (threaded from `claim.checkpoint_tip`, the tip THIS run last
   * published to its own checkpoint ref, persisted server-side on every publish — M2). A
   * checkpoint ref is per-BRANCH, so sharing history with the default is not enough — the
   * mirrored checkpoint might be a PRIOR (possibly plan-rejected) or foreign run's work.
   * #1042 M4 anchored the resume-adopt leg only; #1059 M1 extends the SAME anchor to EVERY
   * adoption leg on the not-ownedHere path — both the resume-relaxed leg and the
   * strict-descendant leg — via a single owner-gated predicate. Adopt (or set aside for the
   * #759 cherry-pick) ONLY when `expectedCheckpointTip` is a non-empty string AND equals the
   * mirrored checkpoint's current SHA; a NULL/empty tip (a run that never published) or a
   * mismatch (a foreign/prior checkpoint) falls through to the origin/default floor, LOUDLY,
   * and is NEVER set aside (so the #759 cherry-pick cannot re-import that foreign work). A
   * same-run legitimate resume still adopts, because its persisted tip advanced with its own
   * checkpoint and still matches. `expectedCheckpointTip` defaults undefined ⇒ no adoption ⇒
   * keeps existing test call sites conservative.
   *
   * The seed is a LOCAL `clone --shared` from the worker bare: fast (objects are
   * referenced read-only from the bare via the clone's objects/info/alternates — the
   * runner cannot corrupt worker-bare objects through it), and the runner's own new
   * commit objects land in the clone's own objects store. The clone is a TRUSTED-source
   * local clone (worker bare → runner clone), so the local optimization here is safe;
   * the untrusted direction (worker fetching BACK from the runner clone) is the one
   * forced onto the pack transport in fetchAgentBranch (B2 invariant 3).
   *
   * issue #1783 M2 — `attempt` (a Docker-wired worker only) seeds a FRESH per-attempt path
   * instead of this canonical one and classifies the journal by identity; see
   * {@link attemptCloneForBranch}. Absent ⇒ everything above, byte-for-byte.
   *
   * issue #1783 M3 — `reseed` (REQUIRED: the runner passes it on every claim, and there is no
   * unproven plain-`fs.rm` fallback) makes the canonical reseed free the canonical path through
   * {@link freeCanonicalClonePath}: a scoped process proof first, then the runner-uid delete, and a
   * same-parent quarantine of whatever the delete could not remove. An attempt seed never uses it.
   *
   * `opts.selfContained` is required for sandboxed commands and planning cross-check
   * capture, whose object reads must remain within the clone:
   * after every ref/checkpoint step, still under this bare's lock, the clone is dissociated
   * from the bare (materializeRunnerClone), because the Codex command sandbox does not grant
   * the bare and planning capture refuses alternates. Default false: ordinary Claude
   * runs keep the shared clone unchanged. It applies to BOTH seeds: the canonical one and
   * (issue #1783) the per-attempt one on a Docker-wired worker.
   */
  async runnerCloneForBranch(
    barePath: string,
    branch: string,
    key: string,
    reseed: CanonicalReseedOptions,
    runId?: string,
    resume = false,
    expectedCheckpointTip?: string,
    attempt?: AttemptSeedOptions,
    opts?: { selfContained?: boolean },
  ): Promise<RunnerClone> {
    if (attempt) return this.attemptCloneForBranch(barePath, branch, key, runId, resume, expectedCheckpointTip, attempt, opts);
    return this.withLock(barePath, async () => {
      const clonePath = this.runnerClonePath(barePath, key);
      // #1197, verified 2026-09-08: the clone is the only remaining copy when a
      // recovery capture failed. The journal is in WORKER-owned bare config, never
      // in the runner-owned clone. An unreadable journal fails closed before rm.
      const pending = await this.readRecoveryCapture(barePath, branch);
      if (pending) {
        // issue #1315 — three fail-closed cases. The git layer NEVER probes owner
        // status and NEVER disposes; it only classifies. Only Case B is reclaimable,
        // and only the runner reclaims, after an authoritative owner probe.
        if (pending.clonePath !== clonePath) {
          // Case A: the journal names a DIFFERENT path than this branch's canonical
          // clone — a claimant-relative clone-path mismatch (e.g. a cross-kind slug); the
          // git layer fails closed here and NEVER probes, but the runner MAY resolve it via
          // authoritative owner validation (issue #1319).
          throw new CapturePathMismatchError(pending.clonePath, clonePath, branch, pending.runId);
        }
        if (pending.runId !== runId) {
          // Case B: the matched canonical pair, owned by ANOTHER run. The only
          // reclaimable condition — the runner probes `pending.runId` and, iff it is
          // terminal, retires the residue before reseeding.
          throw new ForeignCaptureBlockedError(clonePath, branch, pending.runId);
        }
        // Case C: this run's own retained work — capture before reseeding.
        if (!await this.pathPresent(pending.clonePath)) throw new Error("known recovery source is missing; nothing seeded");
        throw new PendingRecoveryCaptureError(clonePath, branch);
      }
      // issue #1783 M3: the journal cases above ran first and threw with the path untouched.
      if (await this.readRecoveryAttemptMode(barePath, key)) {
        throw new Error("recovered key requires a fresh attempt path");
      }
      if ((await this.journaledClonePaths(barePath)).has(clonePath)) throw new Error("retained predecessor blocks canonical reuse");
      await this.freeCanonicalClonePath(clonePath, key, reseed);
      return this.seedRunnerClone(barePath, branch, clonePath, runId, resume, expectedCheckpointTip, opts);
    });
  }

  /**
   * issue #1783 M3 — free the canonical clone path `<runnerRoot>/<repoDir>/<key>` for a reseed,
   * under the caller's bare lock, after the journal classification:
   *
   *   1. Inspect ({@link inspectCanonical}): the path lies under runnerRoot, its parent IS
   *      `<runnerRoot>/<repoDir>` and lstat's as a real directory. Nothing there (ENOENT) ⇒ nothing
   *      to free, no proof needed.
   *   2. A symlink, plain file or any other non-directory AT the canonical path (a runner-uid agent
   *      of another run can plant one: the repo dir is 2775 and not sticky) is never followed and
   *      never deleted through: the entry itself is quarantined by the same-parent rename (step 5;
   *      rename(2) never follows its final component), logged, and the reseed continues.
   *   3. A real directory: the scoped process proof (`reseed.beforeFree`); it throws to block with
   *      nothing moved.
   *   4. The delete, AS THE RUNNER UID under the uid split ({@link runRunnerUidDelete}: the
   *      setpriv-wrapped `/bin/rm -rf -- <canonical>`, bounded by its timeout). The tree is
   *      runner-writable, so its interior is agent-controlled: a path-based recursive delete run as
   *      the WORKER follows a directory swapped for a symlink mid-walk into worker-owned data, while
   *      the runner uid can reach only what it could already delete. Single-uid (#58) has no second
   *      uid and so no boundary: the delete stays the in-process `fs.rm` there. Done when the path
   *      is then gone. A timeout settles only once the killed rm's 'close' arrives; when 'close'
   *      stays absent past a bounded wait after the group kill (whether the kill succeeded or
   *      failed), the rm may still be walking the path, so this BLOCKS right here, before any
   *      rename or reseed, and every later free or reseed of the same path on this GitCache
   *      blocks too until that rm's 'close' arrives.
   *   5. Otherwise (the delete failed, timed out, or left the path present: a root-owned or read-only
   *      directory in the tree, e.g. left by a Docker bind) re-inspect (step 1's checks again), then
   *      rename what is left to `<runnerRoot>/<repoDir>/<formatResidueName(key, uuid)>` in the SAME
   *      parent, as the worker: a rename is non-recursive and never follows. Same parent is
   *      load-bearing: `<repoDir>` is worker-owned 2775 and not sticky (see seedRunnerClone), so the
   *      worker may rename any entry within it, while a cross-parent move (like
   *      createRetireScratchParent's `.retire-*`) must also rewrite the moved directory's `..`,
   *      which needs write on the moved directory itself: EACCES on a root-owned one. A delete that
   *      FAILED yet left nothing to rename has an unknown outcome and blocks.
   *   6. Confirm the canonical path is free (lstat ENOENT).
   *
   * The residue is KEPT: nothing here deletes it. Only the retention sweep may, later, as the runner
   * uid and under the per-key cap (a root-owned residue then fails that delete and stays). Any
   * failure of steps 1, 3, 5 or 6 is a {@link CloneResidueBlockedError}.
   */
  private async freeCanonicalClonePath(canonical: string, key: string, reseed: CanonicalReseedOptions): Promise<void> {
    // An earlier claim's rm on this path may still be walking it: touch nothing (N1).
    this.assertNoUnsettledDelete(canonical);
    const found = await this.inspectCanonical(canonical);
    if (found === "absent") return;
    if (found === "other") {
      const residue = await this.quarantineCanonical(canonical, key);
      this.log.warn("runner clone: the canonical path was not a real directory (a symlink or a file); quarantined the entry itself as residue in the same parent, never followed (kept)", {
        canonical,
        residue,
      });
      return;
    }
    await reseed.beforeFree(canonical);
    let deleteErr: unknown;
    try {
      await this.runRunnerUidDelete(canonical);
    } catch (err) {
      deleteErr = err;
    }
    // An rm that may still be alive walks the canonical path by name: a quarantine rename would
    // hand it nothing new, but the reseed right after would hand it the fresh clone. Block first.
    if (deleteErr instanceof RunnerDeleteUnsettledError) {
      throw new CloneResidueBlockedError(`${deleteErr.message}; nothing quarantined or reseeded`);
    }
    // Re-validate: whatever the delete did, what is at the canonical path is inspected afresh.
    const after = await this.inspectCanonical(canonical);
    if (after === "absent") {
      if (deleteErr === undefined) return;
      throw new CloneResidueBlockedError(`the delete failed and left nothing to quarantine: ${gitErrorMessage(deleteErr)}`);
    }
    const residue = await this.quarantineCanonical(canonical, key);
    this.log.warn("runner clone: the canonical path could not be deleted; quarantined it as residue in the same parent (kept)", {
      canonical,
      residue,
      rm_error: (deleteErr as NodeJS.ErrnoException | undefined)?.code,
      detail: deleteErr === undefined ? "the delete left the path present" : sanitizeForLog(gitErrorMessage(deleteErr)),
    });
  }

  /** issue #1783 (N1) — refuse a path an earlier runner-uid delete may still be walking: its
   *  child settled as a {@link RunnerDeleteUnsettledError} and has not closed since. Throws
   *  {@link CloneResidueBlockedError}. */
  private assertNoUnsettledDelete(target: string): void {
    if (this.unsettledDeletes.has(path.resolve(target))) {
      throw new CloneResidueBlockedError("an earlier runner-uid delete of this path may still be running; nothing freed or reseeded until it exits");
    }
  }

  /** issue #1783 M3 — the same-parent quarantine rename of whatever is at `canonical` (any type:
   *  rename(2) moves the entry itself, never what a symlink points at) to
   *  `formatResidueName(key, uuid)`, then the confirm that the canonical path is free. Returns the
   *  residue path. Throws {@link CloneResidueBlockedError}. */
  private async quarantineCanonical(canonical: string, key: string): Promise<string> {
    let residue: string;
    try {
      residue = path.join(path.dirname(canonical), formatResidueName(key, this.canonicalFreeSeam?.residueUuid?.() ?? randomUUID()));
    } catch (err) {
      throw new CloneResidueBlockedError(`no residue name for the key: ${gitErrorMessage(err)}`);
    }
    if (path.dirname(residue) !== path.dirname(canonical)) throw new CloneResidueBlockedError("the residue name leaves the canonical parent");
    // rename(2) silently replaces an EMPTY directory at the destination: never let it.
    if (await this.pathPresent(residue)) throw new CloneResidueBlockedError(`the residue path already exists: ${path.basename(residue)}`);
    try {
      await (this.canonicalFreeSeam?.rename ?? fs.rename)(canonical, residue);
    } catch (err) {
      throw new CloneResidueBlockedError(`quarantine rename failed: ${gitErrorMessage(err)}`);
    }
    if (await this.pathPresent(canonical)) {
      throw new CloneResidueBlockedError(`the canonical path is still present after the quarantine moved it to ${path.basename(residue)}`);
    }
    return residue;
  }

  /** issue #1783 M3 — what is at the canonical path, validated: it must be
   *  `<runnerRoot>/<repoDir>/<key>`, and `<repoDir>` must lstat as a REAL directory (a symlink is
   *  never followed). `"absent"` when the repo dir or the canonical path does not exist,
   *  `"directory"` for a real directory, `"other"` for a symlink, file or anything else (lstat,
   *  never followed). Throws {@link CloneResidueBlockedError}. */
  private async inspectCanonical(canonical: string): Promise<"absent" | "directory" | "other"> {
    const root = path.resolve(this.runnerRoot);
    const resolved = path.resolve(canonical);
    if (!isWithinPath(resolved, root) || path.dirname(path.dirname(resolved)) !== root) {
      throw new CloneResidueBlockedError("the canonical path is not <runnerRoot>/<repoDir>/<key>");
    }
    const lstat = this.canonicalFreeSeam?.lstat ?? ((p: string) => fs.lstat(p));
    let pst: Stats;
    try {
      pst = await lstat(path.dirname(resolved));
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return "absent";
      throw new CloneResidueBlockedError(`the runner repo dir cannot be inspected: ${gitErrorMessage(err)}`);
    }
    if (pst.isSymbolicLink() || !pst.isDirectory()) throw new CloneResidueBlockedError("the runner repo dir is not a real directory");
    let st: Stats;
    try {
      st = await lstat(resolved);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return "absent";
      throw new CloneResidueBlockedError(`the canonical path cannot be inspected: ${gitErrorMessage(err)}`);
    }
    return st.isDirectory() && !st.isSymbolicLink() ? "directory" : "other";
  }

  /** issue #1783 M2 — lstat reconciliation: true when `p` exists (any type), false on ENOENT;
   *  any other error propagates (fail closed). A pod restart makes the paths vanish while the
   *  journal and ledger (on the PVC) survive, so every path they name is re-checked here. */
  private async pathPresent(p: string): Promise<boolean> {
    return fs.lstat(p).then(
      () => true,
      (err: NodeJS.ErrnoException) => {
        if (err.code === "ENOENT") return false;
        throw err;
      },
    );
  }

  /**
   * issue #1783 M2 — the Docker-wired seed: EVERY execution attempt gets a fresh clone path
   * `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>`. A predecessor's path is never reseeded,
   * executed in, moved or deleted here: prior work reaches the new attempt only through the
   * tracking ref, checkpoint adoption and the journal/capture flow, all resolved in the bare.
   *
   * The recovery journal is classified by IDENTITY — the (runId, repoDir/key) pair plus the
   * attempt grammar — not by literal path equality, so a journal naming the predecessor's
   * attempt path is never a spurious mismatch:
   *   - C′ own retained work: same runId and the journaled path is this key's canonical path or a
   *     valid `<key>.attempt-<id>` (whose id matches the journal's, when it carries one) →
   *     PendingRecoveryCaptureError(journaledPath).
   *   - B′ the same key, another run → ForeignCaptureBlockedError(journaledPath).
   *   - A′ a different key, or a path that parses as neither → CapturePathMismatchError.
   * Mismatch and foreign-owner classification precede the source-presence check. This run's own
   * journal whose known source path is gone fails closed; it is never overwritten by a seed.
   */
  private async attemptCloneForBranch(
    barePath: string,
    branch: string,
    key: string,
    runId: string | undefined,
    resume: boolean,
    expectedCheckpointTip: string | undefined,
    attempt: AttemptSeedOptions,
    opts?: { selfContained?: boolean },
  ): Promise<RunnerClone> {
    // A key must never be mistaken for a retained artifact (or be one): a git branch component
    // cannot start with `.`, so a key derived from a valid branch never does either.
    if (key === "" || key.startsWith(".") || key.includes(path.sep) || isRetainedArtifactName(key)) {
      throw new Error("refusing an unsafe runner clone key");
    }
    if (!ATTEMPT_ID_RE.test(attempt.attemptId)) throw new Error("malformed attempt id");
    return this.withLock(barePath, async () => {
      const canonical = this.runnerClonePath(barePath, key);
      const clonePath = attemptClonePath(canonical, attempt.attemptId);
      const pending = await this.readRecoveryCapture(barePath, branch);
      if (pending) {
        const shape = this.clonePathShape(pending.clonePath, canonical);
        const idMismatch =
          pending.attemptId !== undefined && (shape?.attemptId ?? "") !== pending.attemptId;
        if (!shape || idMismatch) {
          throw new CapturePathMismatchError(pending.clonePath, canonical, branch, pending.runId);
        }
        if (pending.runId !== runId) {
          throw new ForeignCaptureBlockedError(pending.clonePath, branch, pending.runId);
        }
        if (!await this.pathPresent(pending.clonePath)) throw new Error("known recovery source is missing; nothing seeded");
        throw new PendingRecoveryCaptureError(pending.clonePath, branch);
      }
      // The attempt id carries 64 random bits: an existing path is not a collision to paper
      // over, it is something planted. Fail closed and touch nothing.
      if (await this.pathPresent(clonePath)) {
        throw new Error(`${REASON_WORKER_RESIDUE_BLOCKED}: the fresh attempt clone path already exists (${sanitizeForLog(path.basename(clonePath), 160)}); nothing seeded`);
      }
      // Bound the ledger before reading it: the last value per attemptId, gone paths dropped (see compactAttemptLedger).
      await this.compactAttemptLedger(barePath, branch);
      // The seed-time recovery sweep over every NON-LIVE path of this key. It throws to block.
      const ledger = await this.readAttemptLedger(barePath, branch);
      await attempt.beforeSeed(await this.nonLiveKeyPaths(canonical, ledger, attempt.isLive), canonical);
      await fs.mkdir(path.dirname(clonePath), { recursive: true });
      // `live` BEFORE the clone: every attempt dir on disk then has a ledger identity.
      //
      // Crash-window orphans (issue #1783 M2 review, N5). Verified ordering in runner.ts phaseClone:
      // runnerCloneForClaim returns this seed, and the runner writes the recovery journal naming this
      // path (markRecoveryCapture) before any executor or agent starts in it. Between this append
      // and that write the path only ever sees the seed itself (the worker-driven runner-uid clone,
      // checkout and scratch provisioning), the retention sweep below, and a worker-side read of the
      // bare (the runner's originBranchTip). So a
      // `live` entry that no journal names, that is not live on this worker and whose run holds no
      // custody record was interrupted between this append and that journal write: it holds a seed
      // and no agent work, and the retention sweep treats it as disposable under the abandoned cap.
      await this.appendAttemptLedger(barePath, branch, {
        attemptId: attempt.attemptId,
        runId: runId ?? "",
        clonePath,
        state: "live",
      });
      let seeded: RunnerClone;
      try {
        seeded = await this.seedRunnerClone(barePath, branch, clonePath, runId, resume, expectedCheckpointTip, opts);
      } catch (err) {
        // A half-seeded attempt holds no work; release it to the retention sweep.
        await this.appendAttemptLedger(barePath, branch, {
          attemptId: attempt.attemptId,
          runId: runId ?? "",
          clonePath,
          state: "abandoned",
        }).catch(() => undefined);
        throw err;
      }
      attempt.onSeeded?.(clonePath);
      await this.sweepRetainedArtifacts(barePath, canonical, clonePath, attempt).catch((err: unknown) => {
        this.checkRecoveryInterruption();
        this.log.warn("runner clone retention sweep failed; nothing further removed", {
          key,
          error: gitErrorMessage(err),
        });
      });
      return { ...seeded, attemptId: attempt.attemptId };
    });
  }

  /** The identity of a runner clone path relative to one key's canonical path: the canonical
   *  path itself (no attempt id), or a valid `<canonical>.attempt-<id>` (whole-grammar parse,
   *  normalized, directly under runnerRoot/<repoDir>). Anything else ⇒ undefined. */
  private clonePathShape(p: string, canonical: string): { attemptId: string | undefined } | undefined {
    // issue #1783 M3: a retained artifact (`.uzi-skills-*`, quarantined `.uzi-residue-*`) is never
    // a clone of any key. The grammar already cannot produce one (a key never starts with `.`);
    // this is the explicit guard every listing of `<runnerRoot>/<repoDir>` classifies through.
    if (isRetainedArtifactName(path.basename(p))) return undefined;
    if (p === canonical) return { attemptId: undefined };
    const parsed = parseAttemptPath(p, path.resolve(this.runnerRoot));
    if (!parsed) return undefined;
    if (path.join(path.resolve(this.runnerRoot), parsed.repoDir, parsed.key) !== canonical) return undefined;
    return { attemptId: parsed.attemptId };
  }

  /** Every path of the key `canonical` names that is NOT live on this worker and still exists
   *  (lstat): the canonical path and each `<key>.attempt-<id>` sibling on disk, plus every path the
   *  ledger records for it, so the set stays bounded by what is on disk however long the ledger's
   *  history. A gone path is not swept: nothing is left there to kill or tear down by path. */
  private async nonLiveKeyPaths(
    canonical: string,
    ledger: Map<string, AttemptLedgerEntry>,
    isLive: (p: string) => boolean,
  ): Promise<string[]> {
    const parent = path.dirname(canonical);
    const out = new Set<string>();
    if (await this.pathPresent(canonical)) out.add(canonical);
    let names: string[] = [];
    try {
      names = await fs.readdir(parent);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
    }
    for (const name of names) {
      const p = path.join(parent, name);
      if (this.clonePathShape(p, canonical)?.attemptId !== undefined) out.add(p);
    }
    for (const e of ledger.values()) {
      if (this.clonePathShape(e.clonePath, canonical) && (await this.pathPresent(e.clonePath))) out.add(e.clonePath);
    }
    return [...out].filter((p) => !isLive(p)).sort();
  }

  /**
   * issue #1783 M2 — the retention sweep, run at each attempt seed under the bare lock.
   *
   * Attempts: keeps at most {@link RETAINED_ABANDONED_PER_KEY} DISPOSABLE-KIND attempts per key and
   * deletes the oldest beyond that (attempt-id order), each with its `.uzi-skills-*` sibling as a
   * pair (the sibling first, so a failed attempt delete never orphans it). A skills sibling whose
   * attempt dir is already gone and whose ledger entry is not `reclaimed`/`live` is swept too. The disposable kinds are `abandoned` (a verified capture released it in place) and a
   * crash-window orphan: a `live` entry not live on this worker, named by no journal and whose run
   * holds no custody record (see the ordering note in attemptCloneForBranch). `reclaimed` (an
   * uncaptured foreign owner's attempt), `retired`, a live attempt and a dir with no ledger identity
   * are never counted and never deleted.
   *
   * Residue: keeps at most {@link RETAINED_RESIDUE_PER_KEY} `.uzi-residue-<key>.residue-<uuid>`
   * entries of this key (the one grammar, see formatResidueName) and deletes the oldest beyond that,
   * OLDEST by the residue directory's own lstat mtime, the name as the tie-break. A uuid is not
   * sortable, and mtime is what the producer's rename leaves; a runner-uid process that touches a
   * residue can only reorder which disposable residue goes first. Residue is un-journaled by
   * definition and carries no run id: it is disposable when no journal names it (or a path inside
   * it), it is not live, and the scoped scan over it is quiescent.
   *
   * `seeding` (the attempt path this seed just made) is never a candidate.
   * Every deletion goes through {@link deleteRetainedArtifact} (re-validated, runner-uid). A deletion
   * that fails or is refused (e.g. a root-owned dir, a symlinked attempt) is logged and the entry is
   * kept. A residue entry that is a symlink or file is unlinked (the entry itself, never followed).
   */
  private async sweepRetainedArtifacts(
    barePath: string,
    canonical: string,
    seeding: string,
    attempt: AttemptSeedOptions,
  ): Promise<void> {
    const parent = path.dirname(canonical);
    const key = path.basename(canonical);
    const names = await fs.readdir(parent);
    const journaled = [...(await this.journaledClonePaths(barePath))];
    const ledgers = await this.readAllAttemptLedgers(barePath);
    const namedByJournal = (p: string): boolean => journaled.some((j) => isWithinPath(j, p));
    const entryFor = (attemptId: string, p: string): (AttemptLedgerEntry & { branch: string }) | undefined => {
      const e = ledgers.get(attemptId);
      return e && e.clonePath === p ? e : undefined;
    };
    const quiescent = async (paths: string[]): Promise<boolean> => {
      try {
        return await attempt.quiescent(paths, canonical);
      } catch {
        return false;
      }
    };

    // 1. Disposable-kind attempts, oldest first beyond the cap.
    const candidates: Array<{ p: string; attemptId: string; entry: AttemptLedgerEntry & { branch: string } }> = [];
    for (const name of names) {
      const p = path.join(parent, name);
      const id = this.clonePathShape(p, canonical)?.attemptId;
      // The attempt this seed just made is `live` and not yet journaled: never a candidate, whether
      // or not the caller has registered it live yet.
      if (id === undefined || p === seeding) continue;
      const entry = entryFor(id, p);
      if (entry === undefined || entry.runId === "") continue;
      if (entry.state === "abandoned") {
        candidates.push({ p, attemptId: id, entry });
      } else if (entry.state === "live" && !attempt.isLive(p) && !namedByJournal(p) && !(await this.custodyHeld(entry.runId))) {
        candidates.push({ p, attemptId: id, entry });
      }
    }
    candidates.sort((a, b) => compareAttemptIds(a.attemptId, b.attemptId));
    for (const a of candidates.slice(0, Math.max(0, candidates.length - RETAINED_ABANDONED_PER_KEY))) {
      const skillsName = `.uzi-skills-${path.basename(a.p)}`;
      const skills = path.join(parent, skillsName);
      if (namedByJournal(a.p) || attempt.isLive(a.p) || (await this.custodyHeld(a.entry.runId))) continue;
      if (!(await quiescent([a.p, skills]))) continue;
      // The skills sibling FIRST: materializeSkillsPlugin rebuilds it for any attempt that runs, so
      // losing it costs nothing, while an attempt dir deleted before a failed skills delete would
      // leave the skills dir orphaned. A failed attempt delete after this keeps the attempt (still
      // `abandoned`, retried at the next seed) and orphans nothing.
      if (!(await this.deleteRetainedArtifact(parent, skills, (n) => n === skillsName))) continue;
      if (!(await this.deleteRetainedArtifact(parent, a.p, (n) => this.clonePathShape(path.join(parent, n), canonical)?.attemptId === a.attemptId))) continue;
      const { branch: entryBranch, ...entry } = a.entry;
      await this.appendAttemptLedger(barePath, entryBranch, { ...entry, state: "retired" }).catch(() => undefined);
      this.log.info("runner clone retention: deleted a retained attempt", { path: a.p, state: a.entry.state });
    }

    // 1b. Orphaned skills siblings of this key: a `.uzi-skills-<key>.attempt-<id>` whose attempt dir
    // is gone (an older worker's attempt-first delete, or a crash between the two deletes) and whose
    // ledger entry is neither `reclaimed` nor `live` (an absent entry, e.g. compacted away, counts as
    // neither). A skills dir holds a rebuilt plugin tree, never run work. Deleted behind the same
    // journal / live / scoped-quiescence checks and the same re-validated runner-uid delete.
    for (const name of await fs.readdir(parent)) {
      const art = parseRetainedArtifactName(name);
      if (art?.kind !== "skills") continue;
      const attemptDir = path.join(parent, art.cloneBasename);
      const id = this.clonePathShape(attemptDir, canonical)?.attemptId;
      if (id === undefined || attemptDir === seeding) continue;
      if (await this.pathPresent(attemptDir)) continue;
      const state = ledgers.get(id)?.state;
      if (state === "reclaimed" || state === "live") continue;
      const p = path.join(parent, name);
      if (namedByJournal(attemptDir) || namedByJournal(p) || attempt.isLive(attemptDir) || attempt.isLive(p)) continue;
      if (!(await quiescent([p]))) continue;
      if (await this.deleteRetainedArtifact(parent, p, (n) => n === name)) {
        this.log.info("runner clone retention: deleted an orphaned skills sibling", { path: p });
      }
    }

    // 2. This key's residue, oldest (mtime) first beyond the cap.
    const isOwnResidue = (n: string): boolean => {
      const art = parseRetainedArtifactName(n);
      return art?.kind === "residue" && art.key === key;
    };
    const residue: Array<{ p: string; name: string; mtimeMs: number }> = [];
    for (const name of names) {
      if (!isOwnResidue(name)) continue;
      const p = path.join(parent, name);
      const st = await fs.lstat(p).catch(() => undefined);
      if (st) residue.push({ p, name, mtimeMs: st.mtimeMs });
    }
    residue.sort((a, b) => a.mtimeMs - b.mtimeMs || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    for (const r of residue.slice(0, Math.max(0, residue.length - RETAINED_RESIDUE_PER_KEY))) {
      if (namedByJournal(r.p) || attempt.isLive(r.p)) continue;
      if (!(await quiescent([r.p]))) continue;
      if (await this.deleteRetainedArtifact(parent, r.p, isOwnResidue, { unlinkNonDirectory: true })) {
        this.log.info("runner clone retention: deleted quarantined residue", { path: r.p });
      }
    }
  }

  /**
   * issue #1783 M2 — delete ONE retained artifact of the retention sweep. `target` lives in the
   * runner-writable `<runnerRoot>/<repoDir>` tree, so its interior is agent-controlled: a recursive
   * delete run as the WORKER could be steered by a directory swapped for a symlink mid-walk into
   * anything the worker can delete (the bare repo, `recovery/`). So, re-validated immediately before
   * the delete:
   *   - `target` is directly under `parent`, `parent` directly under runnerRoot and a real directory,
   *     and the basename parses as the expected artifact of this key (`isExpected`);
   *   - `target` lstat's as a REAL directory, never a symlink (absent ⇒ nothing to delete). With
   *     `unlinkNonDirectory` (the residue sweep only, issue #1783 M3: a symlink or file quarantined
   *     off the canonical path is residue too) a non-directory is instead removed by a plain
   *     unlink(2) of the entry, as the worker: it never follows, and never recurses.
   * Then, under the uid split, the delete runs AS THE RUNNER UID through the runnerCommand/setpriv
   * wrapper (`/bin/rm -rf -- <target>`, bounded by a timeout that kills the runner group), so a
   * swapped symlink can reach only what the runner uid could already delete. The image's `/bin/rm`
   * is busybox, which has no `--one-file-system`: the uid is the containment. A tree the runner
   * cannot delete (a root-owned subtree) fails the rm and is KEPT, with a log line.
   * Single-uid (#58): there is no second uid and so no boundary to route through (the worker IS the
   * agent's uid there, the #58 accepted posture): the delete stays the in-process fs.rm.
   * A timed-out rm that cannot be shown gone (no 'close' within the bounded wait after the group
   * kill, whether or not the kill succeeded) is rethrown, which stops the whole sweep. A target
   * such an rm (of this sweep or an earlier one) may still be walking, the recorded path itself or
   * anything under it, is refused (kept, logged) before any delete starts.
   * Returns true when the target is gone.
   */
  private async deleteRetainedArtifact(
    parent: string,
    target: string,
    isExpected: (name: string) => boolean,
    opts: { unlinkNonDirectory?: boolean } = {},
  ): Promise<boolean> {
    const refuse = (why: string): false => {
      this.log.warn("runner clone retention: refusing to delete; keeping it", { path: target, reason: why });
      return false;
    };
    const root = path.resolve(this.runnerRoot);
    if (path.dirname(parent) !== root || path.dirname(target) !== parent) return refuse("not a direct child of a runner repo dir");
    if (!isExpected(path.basename(target))) return refuse("not the expected retained artifact of this key");
    // An earlier runner-uid rm of this path (or of a directory above it) may still be walking it:
    // a second rm here would run beside it. Keep the target; a later sweep retries it after 'close'.
    for (const unsettled of this.unsettledDeletes.keys()) {
      if (isWithinPath(target, unsettled)) {
        return refuse(`an earlier runner-uid delete of this path (${unsettled}) may still be running`);
      }
    }
    const lstat = this.retentionDeleteSeam?.lstat ?? ((p: string) => fs.lstat(p));
    try {
      const pst = await lstat(parent);
      if (pst.isSymbolicLink() || !pst.isDirectory()) return refuse("the runner repo dir is not a real directory");
      const st = await lstat(target);
      if (st.isSymbolicLink() || !st.isDirectory()) {
        if (!opts.unlinkNonDirectory) return refuse("not a real directory");
        // issue #1783 M3: a symlink or file quarantined off the canonical path. unlink(2) removes
        // the entry itself and never follows it; a directory swapped in meanwhile fails it (kept).
        await fs.unlink(target);
        return true;
      }
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return true;
      return refuse(gitErrorMessage(err));
    }
    try {
      await this.runRunnerUidDelete(target);
    } catch (err) {
      // An rm that may still be alive: stop the whole sweep (the caller logs "nothing further
      // removed") rather than start another delete beside it.
      if (err instanceof RunnerDeleteUnsettledError) throw err;
      this.log.warn("runner clone retention: could not delete; keeping it", { path: target, error: gitErrorMessage(err) });
      return false;
    }
    return true;
  }

  /** The recursive delete itself (see {@link deleteRetainedArtifact}, and M3's
   *  {@link freeCanonicalClonePath}): runner-uid under the split, in-process single-uid. */
  private async runRunnerUidDelete(target: string): Promise<void> {
    const split = this.retentionDeleteSeam?.split ?? uidSplitActive();
    if (!split) {
      await fs.rm(target, { recursive: true, force: true });
      return;
    }
    const wrapped = runnerCommand(RETENTION_RM_BIN, ["-rf", "--", target], true);
    if (this.retentionDeleteSeam?.run) {
      await this.retentionDeleteSeam.run(wrapped.command, wrapped.args);
      return;
    }
    const seam = this.retentionDeleteSeam;
    const timeoutMs = seam?.timeoutMs ?? RETENTION_RM_TIMEOUT_MS;
    const closeWaitMs = seam?.closeWaitMs ?? RETENTION_RM_CLOSE_WAIT_MS;
    const kill = seam?.kill ?? killRunnerGroup;
    await new Promise<void>((resolve, reject) => {
      // cwd "/" and a fixed root-owned PATH: nothing resolves from a runner-writable dir. Detached,
      // so the setpriv'd rm leads its own group and a timeout can kill it as the runner uid.
      const child = seam?.spawn
        ? seam.spawn(wrapped.command, wrapped.args)
        : spawn(wrapped.command, wrapped.args, {
            cwd: "/",
            env: workerSpawnEnv({ PATH: "/usr/bin:/bin" }),
            detached: true,
            stdio: ["ignore", "ignore", "pipe"],
          });
      let stderr = "";
      child.stderr?.on("data", (c: Buffer) => {
        if (stderr.length < 4096) stderr += c.toString("utf8");
      });
      let settled = false;
      let timedOut = false;
      let closed = false;
      let closeTimer: NodeJS.Timeout | undefined;
      const settle = (err?: Error): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        clearTimeout(closeTimer);
        if (err instanceof RunnerDeleteUnsettledError && !closed) {
          // N1: remember the path until this child's 'close' (the listener below stays attached
          // after settle), so no later claim frees or reseeds where this rm may still walk.
          const key = path.resolve(target);
          const pending = (this.unsettledDeletes.get(key) ?? 0) + 1;
          this.unsettledDeletes.set(key, pending);
          this.log.warn("runner-uid delete recorded as unsettled: it may still be running; no free, reseed or retention delete of this path until it exits", {
            path: key,
            pid: child.pid,
            pending,
          });
          child.once("close", (code: number | null, signal: NodeJS.Signals | null) => {
            const n = (this.unsettledDeletes.get(key) ?? 1) - 1;
            if (n > 0) this.unsettledDeletes.set(key, n);
            else this.unsettledDeletes.delete(key);
            this.log.info("runner-uid delete exited and left the unsettled record", {
              path: key,
              pid: child.pid,
              exit: code ?? signal ?? "abnormal",
              pending: n,
            });
          });
        }
        if (err) reject(err);
        else resolve();
      };
      // The timeout never settles on its own: busybox rm walks by path, so an rm that outlives
      // this call deletes from whatever is next created at the target. It settles only once the
      // child's 'close' proves the group gone, or as a RunnerDeleteUnsettledError when 'close'
      // stays absent past the bounded wait. A failed kill waits too (N2): ESRCH, a group that
      // already exited, reads the same as a kill that failed, and only 'close' tells them apart.
      const timer = setTimeout(() => {
        timedOut = true;
        const killed = kill(child.pid);
        const unsettled = killed
          ? `no exit within ${closeWaitMs}ms of the runner group kill after ${timeoutMs}ms`
          : `group kill of pid ${child.pid ?? "?"} failed after ${timeoutMs}ms; no close within ${closeWaitMs}ms`;
        closeTimer = setTimeout(() => settle(new RunnerDeleteUnsettledError(unsettled)), closeWaitMs);
      }, timeoutMs);
      child.once("error", (err) => {
        // A spawn failure ran nothing. After the timeout, 'error' proves nothing about the rm.
        if (!timedOut) settle(err);
      });
      child.once("close", (code, signal) => {
        closed = true;
        if (timedOut) settle(new Error(`runner-uid delete timed out after ${timeoutMs}ms (killed; exited ${code ?? signal ?? "abnormally"})`));
        else if (code === 0) settle();
        else settle(new Error(`runner-uid delete exited ${code ?? signal ?? "abnormally"}: ${stripAnsiSgr(stderr).slice(0, 512)}`));
      });
    });
  }

  /** The seed itself (base resolution, the local `clone --shared`, checkout, WIP recovery, the
   *  ratchet clamp, scratch provisioning) at `clonePath`, which the caller has freed (canonical)
   *  or proven fresh (attempt). Runs under the caller's bare lock. */
  private async seedRunnerClone(
    barePath: string,
    branch: string,
    clonePath: string,
    runId: string | undefined,
    resume: boolean,
    expectedCheckpointTip: string | undefined,
    opts?: { selfContained?: boolean; recoveryTip?: string },
  ): Promise<RunnerClone> {
    // (A bare block: the body below kept its original indentation when issue #1783 M2 lifted it
    // out of runnerCloneForBranch's lock callback, so its diff stays reviewable.)
    {
      // The clone's parent dir. Under the M4 split it must be group-`runner`-writable so
      // the runner-uid `git clone` can create <key> inside it: /data/runner is
      // worker:runner 3775 (setgid+sticky) from the entrypoint, and the worker runs with umask
      // 002 (main.ts), so this mkdir is 2775 group `runner` (setgid inherited, sticky is not) — the runner creates the
      // clone and the isolated runner-cmd identity can write it through the same group.
      // Single-uid (#58): plain worker dir.
      await fs.mkdir(path.dirname(clonePath), { recursive: true });

      // Resolve the base commit in the BARE (authoritative), per the PRD #218 M2 table
      // documented above. All three candidate refs live in the bare (the clone does not
      // necessarily carry the default branch), so the resolution happens here.
      const originRef = `refs/remotes/origin/${branch}`;
      const publicationFloor = await this.capturePublicationFloor(barePath, originRef);
      const trackingRef = runnerTrackingRef(branch);
      // issue #781 — disjoint-history guard. Resolve the default ref ONCE up front, then
      // qualify EACH candidate base ref: it counts as existing only if it also shares
      // history with the default branch. A candidate that exists but is disjoint from
      // default (a stale ref whose remote counterpart was rebuilt from an orphan root, or
      // a leftover from an unrelated branch reused) is treated as ABSENT, so the seed falls
      // back cleanly to the default tip instead of seeding off unrelated history. The guard
      // qualifies up front (not a post-override): each candidate's downstream logic below is
      // unchanged for a candidate that DOES share history.
      // Best-effort: if the default cannot be resolved we cannot prove disjointness, so
      // every candidate is kept (byte-identical to pre-#781 behaviour on this leg). The
      // not-ownedHere/no-origin else leg still resolves the default authoritatively for its
      // floor below, and throws there if truly unresolvable — unchanged.
      let defaultRef: string | undefined;
      try {
        defaultRef = await this.defaultBranchRef(barePath);
      } catch {
        defaultRef = undefined;
      }
      const originExistsRaw = await this.refExists(barePath, originRef);
      const originDisjoint = originExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, originRef, defaultRef));
      if (originDisjoint) {
        this.log.warn(
          "runner clone: origin branch ref is disjoint from default — ignoring, seeding off default tip",
          { branch, originRef },
        );
      }
      const originExists = originExistsRaw && !originDisjoint;
      // The tracking ref is consulted ONLY when its stamp says THIS run wrote it — the
      // run-identity anchor that gates the whole consideration (see the doc comment). A
      // disjoint tracking ref is treated as absent so it cannot make `ownedHere` true.
      const trackingExistsRaw = await this.refExists(barePath, trackingRef);
      const trackingDisjoint = trackingExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, trackingRef, defaultRef));
      if (trackingDisjoint) {
        this.log.warn(
          "runner clone: tracking ref is disjoint from default — ignoring, seeding off default tip",
          { branch, trackingRef },
        );
      }
      const trackingExists = trackingExistsRaw && !trackingDisjoint;
      const owner = trackingExists
        ? await this.readTrackingOwner(barePath, branch)
        : "";
      const ownedHere = trackingExists && runId !== undefined && owner === runId;
      // PRD #122 M8 cross-worker candidate: origin's checkpoint ref, mirrored into the bare
      // by fetch(). Consulted ONLY on the not-ownedHere legs (see the doc comment). A
      // disjoint checkpoint is treated as absent (so no checkpointSetAside is emitted for it).
      const checkpointRef = `refs/uzi-checkpoints/${branch}`;
      const checkpointExistsRaw = await this.refExists(barePath, checkpointRef);
      const checkpointDisjoint = checkpointExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, checkpointRef, defaultRef));
      if (checkpointDisjoint) {
        this.log.warn(
          "runner clone: checkpoint ref is disjoint from default — ignoring, seeding off default tip",
          { branch, checkpointRef },
        );
      }
      const checkpointExists = checkpointExistsRaw && !checkpointDisjoint;

      let baseRef: string;
      let seededFrom: RunnerClone["seededFrom"];
      let checkpointSetAside = false;
      if (ownedHere && originExists) {
        // Both present: prefer the recovered local work ONLY when it strictly descends
        // from origin; on divergence origin wins so a published commit is never dropped.
        const descends = await this.isAncestor(barePath, originRef, trackingRef);
        baseRef = descends ? trackingRef : originRef;
        seededFrom = descends ? "tracking" : "origin";
      } else if (ownedHere) {
        // First park: recovered work with no competing published branch — no ancestry
        // test, because the current default tip is not a meaningful reference here.
        baseRef = trackingRef;
        seededFrom = "tracking";
      } else {
        // NOT owned here — cross-worker / fresh. Floor = origin branch if pushed, else
        // default. A mirrored checkpoint (PRD #122 M8) is adopted ONLY when it is THIS run's
        // OWN checkpoint (the owner anchor, issue #1059 M1); a foreign/prior or never-published
        // checkpoint is set aside off the floor, LOUDLY, and never re-imported.
        const floorRef = originExists ? originRef : await this.defaultBranchRef(barePath);
        const floorFrom: RunnerClone["seededFrom"] = originExists ? "origin" : "default";
        baseRef = floorRef;
        seededFrom = floorFrom;
        if (checkpointExists) {
          // issue #1059 M1 — a SINGLE owner-gated adopt predicate over BOTH adoption legs.
          // Before this, the resume-adopt leg (Path A below) was owner-anchored (#1042 M4)
          // while the strict-descendant leg (Path B) was NOT: a fresh cross-worker run would
          // adopt ANY mirrored checkpoint that strictly descended the floor, and on divergence
          // set it aside (→ the #759 cherry-pick), regardless of whether the checkpoint was
          // THIS run's own work or a PRIOR/FOREIGN run's. That is the #1059 bug. The owner
          // anchor now gates the whole `checkpointExists` block: `expectedCheckpointTip`
          // (threaded from claim.checkpoint_tip, the tip THIS run last published to its own
          // per-branch checkpoint ref, persisted server-side on every publish — #1042 M2) must
          // be a non-empty string AND equal the mirrored checkpoint's current SHA for ANY
          // adoption or set-aside to happen.
          const checkpointSha = (
            await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointRef}^{commit}`])
          ).trim();
          const ownerMatch =
            expectedCheckpointTip !== undefined &&
            expectedCheckpointTip !== null &&
            expectedCheckpointTip !== "" &&
            expectedCheckpointTip === checkpointSha;
          if (ownerMatch) {
            if (resume && !originExists) {
              // PATH A — PRD #1030 M3 resume-adopt, unchanged. RESUME with an UNPUSHED branch:
              // `main` advancing during a rate-limit park diverges an otherwise-valid mirrored
              // checkpoint from the moved default, and Path B's strict-descendant test would
              // wrongly set it aside and cold-start the run, losing the committed milestones
              // (the #1009 incident). On a resume with no competing published `origin/<branch>`,
              // adopt using the DISJOINT-HISTORY guard ONLY — `checkpointExists` already encodes
              // `sharesHistory(checkpointRef, default)` (a disjoint checkpoint was treated as
              // absent up front), so reaching here means it shares history. No ancestry test
              // against the current default, mirroring the ownedHere first-park leg. The
              // adopt-time wip(park) marker unwrap (adoptedMarker / willRecoverMarker below)
              // still runs on this adopted base because seededFrom is "checkpoint".
              baseRef = checkpointRef;
              seededFrom = "checkpoint";
            } else {
              // PATH B — strict-descendant, NOW owner-gated (#1059 M1 extends #1042 M4's owner
              // anchor to this leg). isAncestor (merge-base --is-ancestor) is TRUE at EQUALITY,
              // so an ancestor test alone would seed a checkpoint that EQUALS the floor as
              // "checkpoint" though nothing was recovered. Require a STRICT descendant:
              // reachable from the floor AND a different commit.
              if (await this.isAncestor(barePath, floorRef, checkpointRef)) {
                const floorSha = (
                  await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${floorRef}^{commit}`])
                ).trim();
                if (checkpointSha !== "" && checkpointSha !== floorSha) {
                  baseRef = checkpointRef;
                  seededFrom = "checkpoint";
                }
                // else EQUAL to the floor: fall through to the floor. Equality is NOT
                // divergence, so checkpointSetAside stays false — nothing was set aside.
              } else {
                // Diverged and OWNED (ownerMatch): the checkpoint is not reachable from the
                // floor but it IS this run's own work, so origin/default WINS loudly and the
                // set-aside drives the #759 wip(park) cherry-pick recovery (leg #4 below).
                checkpointSetAside = true;
              }
            }
          } else {
            // !ownerMatch — a FOREIGN or FRESH (NULL/empty tip) checkpoint. issue #1059: NEVER
            // adopt and NEVER set checkpointSetAside. A checkpoint ref is a per-BRANCH ref, so a
            // mirrored checkpoint that shares history with the default (or even strictly
            // descends it) might be a PRIOR (possibly plan-rejected) or foreign run's work; and
            // checkpointSetAside drives the #759 cherry-pick, which would re-import the very
            // prior/foreign work this guard keeps out. Seed off the origin/default floor set
            // just above and log LOUDLY (structured warn, not a run-feed status) so an operator
            // can see why a present checkpoint was set aside. This folds the two prior warns
            // (the #1042 resume-mismatch warn and the implicit fresh-adopt path) into one.
            this.log.warn(
              "runner clone: checkpoint set aside LOUDLY — mirrored checkpoint tip does not match this run's own persisted checkpoint (owner-anchor guard, issue #1059 M1 extends #1042 M4 to the strict-descendant leg); seeding off the origin/default floor, NOT a prior/foreign run's checkpoint",
              {
                branch,
                checkpointRef,
                checkpoint_sha: checkpointSha,
                expected_checkpoint_tip: expectedCheckpointTip ?? null,
                seeded_from: seededFrom,
              },
            );
          }
        }
      }
      if (opts?.recoveryTip) {
        if (!SHA40_RE.test(opts.recoveryTip) || !defaultRef ||
            !await this.sharesHistory(barePath, opts.recoveryTip, defaultRef)) throw new Error("recovery history cannot be verified");
        baseRef = opts.recoveryTip;
        seededFrom = "tracking";
        checkpointSetAside = false;
      }
      let baseSha = (await this.runGit(barePath, ["rev-parse", "--verify", `${baseRef}^{commit}`])).trim();
      // PRD #1062 M2 (#1036) — peel a `.github/workflows` overlay transport wrapper. When the
      // adopted base is a `ckpt(overlay):` commit (built by checkpointPack for a branch behind
      // `main` on workflows, pushed UNCHANGED by the broker), its swapped `.github/workflows`
      // tree is NOT branch content — it exists only to satisfy GitHub's tip-vs-default
      // workflow-scope check on the push. DISCARD it by re-pointing `baseSha` to the overlay's
      // LAST parent (= realTip, by construction: checkpointPack builds base-FIRST/realTip-LAST).
      // This runs BEFORE the wip-park adoptedMarker logic below, so a `wip(park)` marker stacked
      // under an overlay is still soft-reset on the peeled tip. Order: overlay = discard-by-
      // reparent (outer) → wip-park = keep-by-soft-reset (inner). Best-effort: if the peel can't
      // resolve the parent, leave baseSha as-is and warn (never crash a reseed over the wrapper).
      //
      // PROVENANCE GATE (issue #1036 review): peel ONLY when seededFrom === "checkpoint" — the
      // worker-built, api-brokered ref the agent CANNOT push to. An overlay is only ever
      // synthesised by checkpointPack for that ref; a `ckpt(overlay):` subject on any other base
      // (a tracking/origin/default tip the agent controls) is a FORGERY, and peeling it would
      // DISCARD the agent's real commit (this peel throws the tip's tree away — unlike the
      // wip-park soft-reset, which keeps it). A subject prefix is not provenance across the agent
      // trust boundary; seededFrom is.
      if (seededFrom === "checkpoint" && (await this.isOverlayMarker(barePath, baseSha))) {
        const overlaySha = baseSha;
        // The real tip is the LAST parent. Count parents (`rev-list --parents -n 1` prints
        // "<commit> <p1> [<p2> …]"), then read `<sha>^<n>` where n = parent count.
        const parentLine = (
          await this.tryGitStdout(barePath, ["rev-list", "--parents", "-n", "1", `${overlaySha}^{commit}`])
        ).trim();
        const nParents = parentLine === "" ? 0 : parentLine.split(/\s+/).length - 1;
        const realTip =
          nParents > 0
            ? (
                await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${overlaySha}^${nParents}`])
              ).trim()
            : "";
        // PROVENANCE, part 2 (issue #1036 review F1): the subject prefix + the seededFrom gate
        // are necessary but NOT sufficient. An agent can commit its OWN realTip with a forged
        // `ckpt(overlay):` subject; if that tip ships as a PLAIN checkpoint (not behind on
        // workflows, so checkpointPack builds no overlay) it lands on the worker-built checkpoint
        // ref, so seededFrom is "checkpoint" too — and peeling it would DISCARD the agent's commit.
        // Structural distinction: a genuine overlay differs from its realTip parent ONLY under
        // `.github/workflows/` (all checkpointPack swaps), and non-trivially; a forged work commit
        // differs in real files. Peel ONLY when the diff realTip..overlay is NON-EMPTY and wholly
        // under `.github/workflows/`. This is the unforgeable check: an agent cannot make a commit
        // carrying real work look like a workflow-only swap of its own parent.
        const changed =
          /^[0-9a-f]{40}$/.test(realTip)
            ? (await this.tryGitStdout(barePath, ["diff", "--name-only", realTip, overlaySha])).trim()
            : "";
        const changedPaths = changed === "" ? [] : changed.split("\n");
        const isOverlayStructure =
          changedPaths.length > 0 && changedPaths.every((p) => p.startsWith(".github/workflows/"));
        if (/^[0-9a-f]{40}$/.test(realTip) && isOverlayStructure) {
          baseSha = realTip;
          this.log.info("runner clone: peeled .github/workflows overlay wrapper to its realTip", {
            branch,
            overlay_sha: overlaySha,
            real_tip: realTip,
            seeded_from: seededFrom,
          });
        } else if (/^[0-9a-f]{40}$/.test(realTip)) {
          this.log.warn(
            "runner clone: `ckpt(overlay):` subject but the diff to its last parent is NOT confined to .github/workflows — NOT peeling (forged or non-overlay tip), preserving the checkpoint tip as branch content",
            {
              branch,
              overlay_sha: overlaySha,
              real_tip: realTip,
              changed_paths: changedPaths.length,
              seeded_from: seededFrom,
            },
          );
        } else {
          this.log.warn("runner clone: overlay marker present but its realTip parent did not resolve — not peeling", {
            branch,
            overlay_sha: overlaySha,
            parents: nParents,
            seeded_from: seededFrom,
          });
        }
      }
      // PRD #759 M2 — same-worker + cross-worker-clean recovery. On the tracking leg
      // (same-worker) and the checkpoint leg (cross-worker strict-descendant), `baseSha`
      // itself is the adopted tip, and that tip may be a `wip(park):` marker M1 planted:
      // the throwaway commit auto-saving the pre-park uncommitted tree. When it is, the
      // REAL branch base is the marker's PARENT (the last real commit), so the counts and
      // the ratchet clamp below must sit on the parent, not on the marker (M5 requires the
      // recovered-commit count exclude the marker — it is not committed work). We detect
      // the marker HERE, before those counts are computed, and `reset --soft` the checkout
      // back to the parent after `checkout -b` below, so the marker's tree lands as
      // uncommitted changes and the marker never enters the history the agent builds on.
      const adoptedMarker = (seededFrom === "tracking" || seededFrom === "checkpoint")
        && await this.isWipParkMarker(barePath, baseSha);
      const markerParent = adoptedMarker
        ? (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${baseSha}^^{commit}`])).trim()
        : "";
      // A root-commit marker (no parent) or a failed parent read leaves markerParent empty:
      // there is nothing to reset onto, so treat it as NOT-recovered — keep the marker as
      // the base (byte-identical to today's behaviour), do not reset, and log. Vanishingly
      // rare (the clone base is never empty), but handled rather than crashed.
      if (adoptedMarker && markerParent === "") {
        this.log.warn("runner clone: adopted wip(park) marker has no parent — not recovering (root-commit marker)", {
          branch,
          baseSha,
          seeded_from: seededFrom,
        });
      }
      const willRecoverMarker = adoptedMarker && markerParent !== "";
      // The effective base the branch will actually sit on after the `reset --soft` below.
      // When there is no marker (every non-park leg), effectiveBase === baseSha and every
      // downstream computation is byte-identical to today.
      const effectiveBase = willRecoverMarker ? markerParent : baseSha;
      // How many commits the seed carries ahead of the default branch. On the origin
      // leg that is prior PUSHED work (issue #105); on the tracking leg it is the
      // interrupted attempt's RECOVERED work (PRD #218 M3). Counted in the BARE, which
      // holds every ref. Best-effort by construction — a repo with no resolvable default
      // branch, or any rev-list failure, yields 0 rather than failing a run over colour.
      // Counted from effectiveBase so a recovered wip(park) marker (which reset --soft
      // strips out of history) is never counted (PRD #759 M2/M5).
      const isResumeLeg = seededFrom !== "default";
      const priorCommits = isResumeLeg ? await this.commitsAheadOfDefault(barePath, effectiveBase) : 0;
      // On the default leg the seed already IS the default tip, so no second lookup. On a
      // resume leg they differ, and the difference is exactly what the lead cannot infer.
      const defaultBranchCommit = isResumeLeg ? await this.defaultBranchSha(barePath) : effectiveBase;
      this.log.info("runner clone: seeding", { branch, base: baseRef, seeded_from: seededFrom, prior_commits: priorCommits, path: clonePath });

      // The seed clone + checkout run as the RUNNER uid (PRD #51 M4), so the clone +
      // working tree are runner-owned (the agent commits there; the worker never writes
      // it). `--shared` references the bare's objects read-only (alternate); `--no-checkout`
      // skips populating the stale default so we check the agent branch out at the
      // resolved base SHA (reachable via the alternate) in one step. No PAT (local op).
      // A Codex (selfContained) clone is dissociated from the bare after setup, below.
      await this.runGitAsRunner(undefined, ["clone", "--shared", "--no-checkout", barePath, clonePath]);
      // Issue #134 (production half of #127). ONE detached `git maintenance run --auto
      // --detach` per object-writing command (fetch/commit/push) outlives the git we awaited
      // and keeps writing inside `.git`; it spawns a repack/pack-objects subtree only once
      // `gc.auto`'s threshold is met, which a per-run `--shared` clone never reaches.
      // The terminal cleanup (retireRunnerClone, runner.ts terminal-cleanup block)
      // releases this tree moments after the agent's last commit and our push; issue
      // #1315 made that release an ATOMIC RENAME precisely because `force: true`
      // suppresses ENOENT, not ENOTEMPTY, and a daemon still writing inside `.git` races
      // a recursive rm (issue #1197). The reseed path still deletes recursively, so
      // disabling the daemon below stays load-bearing.
      //
      // As RUNNER, matching the clone: `<clone>/.git/config` is runner-owned, so this
      // rewrites it in place as the same uid. Doing it as WORKER would plant a worker-owned
      // config inside a directory the untrusted runner owns and can replace anyway — no gain,
      // and it breaks the ownership invariant the plant-a-key analysis above rests on. Note
      // the image puts `worker` in the `runner` group, so a worker-uid write here would
      // likely SUCCEED QUIETLY rather than fail loudly.
      await this.disableAutoMaintenance(clonePath, /* asRunner */ true);
      // Issue #234 — plant the SDK agent's commit identity so its very FIRST `git commit`
      // does not fail exit-128 (`unable to auto-detect email address`) on the passwd-less
      // runner uid, which has no /etc/passwd GECOS for git to fall back to. Written
      // repo-local AS THE RUNNER uid, for the same ownership reason disableAutoMaintenance
      // is (the clone is runner-owned; a worker-uid write would break the ownership
      // invariant the plant-a-key analysis above rests on) — and, being repo-local, it
      // additionally covers the AGENT's own git (the SDK Bash tool), which does NOT go
      // through gitEnv. `commit.gpgsign=false` mirrors the stub executor so a signing
      // config reachable from the agent's HOME `.gitconfig` cannot block the commit.
      // Kept as explicit inline calls (NOT folded into disableAutoMaintenance, whose name
      // would hide the identity write).
      await this.runGitAsRunner(clonePath, ["config", "user.name", AGENT_GIT_IDENTITY.name]);
      await this.runGitAsRunner(clonePath, ["config", "user.email", AGENT_GIT_IDENTITY.email]);
      await this.runGitAsRunner(clonePath, ["config", "commit.gpgsign", "false"]);
      await this.runGitAsRunner(clonePath, ["checkout", "-b", branch, baseSha]);
      // PRD #759 M2 — restore a recovered WIP tree to UNCOMMITTED at adopt time. Exactly
      // ONE of the two branches below can run: #3 fires on the tracking/checkpoint legs
      // where `baseSha` IS the marker (adoptedMarker); #4 fires on the not-ownedHere floor
      // leg where the checkpoint DIVERGED and was set aside (checkpointSetAside). They are
      // mutually exclusive by construction — adoptedMarker requires seededFrom
      // tracking|checkpoint, while checkpointSetAside is set only inside the owner-matched
      // Path-B diverged arm (issue #1059 M1), which reassigns neither baseRef nor seededFrom
      // and so leaves seededFrom ∈ {origin, default} — so the structure below can pick at most
      // one.
      let wipRecovered = false;
      // issue #1769: the diverged-checkpoint marker SHA, set only when its cherry-pick applied,
      // so a selfContained clone anchors it before dissociating.
      let recoveredCheckpointMarkerSha = "";
      if (willRecoverMarker) {
        // #3 — SAME-WORKER + CROSS-WORKER-CLEAN. The checkout materialized the marker's
        // tree at HEAD; `reset --soft` moves HEAD back to the marker's parent while leaving
        // the index + working tree at the marker's tree, so the WIP content is present as
        // staged/uncommitted changes and the marker commit is no longer in history — it
        // never enters what the agent builds on, never reaches finalize, never lands in the
        // MR (PRD #759 D3, without the finalize-time rewrite that would collide with ADR
        // #456). runGitAsRunner: a working-tree write in the runner-owned clone.
        await this.runGitAsRunner(clonePath, ["reset", "--soft", markerParent]);
        wipRecovered = true;
        this.log.info("runner clone: recovered wip(park) marker to uncommitted (reset --soft)", {
          branch,
          marker: baseSha,
          parent: markerParent,
          seeded_from: seededFrom,
        });
      } else if (checkpointSetAside && checkpointExists && await this.isWipParkMarker(barePath, checkpointRef)) {
        // #4 — CROSS-WORKER DIVERGED. `main` advanced during the park, so the mirrored
        // checkpoint is not a strict descendant of the floor and the strict-descendant guard
        // set it aside (baseSha = floor). When the checkpoint tip is a `wip(park):` marker we
        // MAY recover just the WIP tree onto the new floor — but only when doing so drops NO
        // committed work. Two things the naive `cherry-pick --no-commit <ref>` got wrong,
        // both fixed here (PRD #759 M2):
        //
        //  (1) DOA — the ref never resolves in the clone. The runner clone is created above
        //      with `git clone --shared --no-checkout`, which copies NONE of the bare's
        //      custom refs, so `refs/uzi-checkpoints/*` is absent from the clone and a
        //      cherry-pick BY REF NAME always `fatal: bad revision`d — this leg could never
        //      recover. The OBJECTS are reachable via the `--shared` alternate; only the ref
        //      NAME is missing. So resolve the marker to a 40-char SHA against the BARE
        //      (mirroring the strict-descendant guard's rev-parse ~:483) and cherry-pick the
        //      SHA, which the clone can name through its alternate. (A Codex clone is
        //      dissociated only after this, with the marker SHA among the anchored objects.)
        //  (2) Silent milestone drop. `cherry-pick --no-commit <marker>` applies ONLY the
        //      marker's diff against ITS OWN parent (the WIP delta). If the diverged
        //      checkpoint carries committed-but-unpushed milestones between the fork point
        //      and the marker (`fork → m1 → wip-marker`, the exact #628 shape), picking only
        //      the tip DROPS m1's content — silently, when m1 touches files disjoint from the
        //      WIP so the pick still applies clean — AND flips checkpointSetAside=false,
        //      suppressing the loud set-aside notice. So gate the recovery on there being NO
        //      committed work below the marker: the marker's PARENT must be an ancestor of the
        //      floor (isAncestor is true at equality — the #685 zero-commit shape where the
        //      marker sits directly on floor_at_park, itself an ancestor of the advanced
        //      floor). If the parent is NOT an ancestor of the floor, committed divergence
        //      lives below the marker: leave it set aside for a human, do NOT cherry-pick.
        const checkpointMarkerSha = (
          await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointRef}^{commit}`])
        ).trim();
        const checkpointMarkerParentSha = checkpointMarkerSha === ""
          ? ""
          : (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointMarkerSha}^^{commit}`])).trim();
        if (checkpointMarkerParentSha === "") {
          // A root-commit marker (no parent), or a failed tip/parent read: no fork point to
          // test against the floor, so we cannot prove that no committed work would be
          // dropped. Keep it set aside (the loud notice is preserved), do not cherry-pick.
          this.log.warn("runner clone: diverged wip(park) checkpoint has no readable parent — left set aside (no recovery)", {
            branch,
            checkpoint: checkpointRef,
            floor: baseSha,
          });
        } else if (await this.isAncestor(barePath, checkpointMarkerParentSha, baseSha)) {
          // The marker sits directly on a floor-ancestor: no committed milestones live below
          // it, so recovering just the WIP delta drops nothing. Cherry-pick the resolved SHA
          // (NOT the ref name — see (1)) `--no-commit` so the WIP tree lands staged/uncommitted
          // on the floor.
          try {
            await this.runGitAsRunner(clonePath, ["cherry-pick", "--no-commit", checkpointMarkerSha]);
            // SUCCESS — the WIP is recovered as uncommitted on the floor. It rides on top of
            // the floor base (priorCommits stays 0 for this leg: no committed work recovered),
            // so seededFrom stays the floor leg. It was recovered, not set aside.
            wipRecovered = true;
            checkpointSetAside = false;
            recoveredCheckpointMarkerSha = checkpointMarkerSha;
            this.log.info("runner clone: recovered diverged wip(park) checkpoint onto new floor (cherry-pick --no-commit)", {
              branch,
              checkpoint: checkpointRef,
              marker: checkpointMarkerSha,
              floor: baseSha,
              seeded_from: seededFrom,
            });
          } catch (err) {
            // FAILURE (merge conflict) — a WIP that touches files main also moved conflicts
            // even with a floor-ancestor parent; that is the genuine unclean case. Guarantee a
            // pristine floor tree (abort the half-applied pick, hard-reset to the floor, clean
            // untracked) and report failure: checkpointSetAside stays true, wipRecovered false,
            // seededFrom unchanged. Required SAFE FAILURE (SC#1(b)): reports failure rather than
            // silently dropping OR force-applying work.
            await this.runGitAsRunner(clonePath, ["cherry-pick", "--abort"]).catch(() => undefined);
            await this.runGitAsRunner(clonePath, ["reset", "--hard", baseSha]).catch(() => undefined);
            await this.runGitAsRunner(clonePath, ["clean", "-fd"]).catch(() => undefined);
            this.log.warn("runner clone: diverged wip(park) checkpoint did NOT apply cleanly onto new floor — recovery failed (set aside)", {
              branch,
              checkpoint: checkpointRef,
              floor: baseSha,
              error: gitErrorMessage(err),
            });
          }
        } else {
          // The marker's parent is NOT an ancestor of the floor: committed-but-unpushed work
          // (m1…mN) lives between the fork point and the marker. Cherry-picking only the tip
          // would silently drop it AND flip off the set-aside notice, reporting a partial
          // recovery as full success. Leave it set aside (checkpointSetAside stays true,
          // wipRecovered false, seededFrom unchanged) so the loud notice tells the human that
          // committed work was left behind.
          this.log.warn("runner clone: diverged wip(park) checkpoint carries committed divergence below the marker — left set aside for a human (no cherry-pick)", {
            branch,
            checkpoint: checkpointRef,
            marker: checkpointMarkerSha,
            markerParent: checkpointMarkerParentSha,
            floor: baseSha,
          });
        }
      }
      // Issue #262 — refresh the clone's default remote-tracking ref to the FRESH default
      // head. The clone's `refs/remotes/origin/<default>` is copied from the bare's
      // `refs/heads/*` (cloneBare rewrites the fetch refspec to
      // `+refs/heads/*:refs/remotes/origin/*`, leaving the bare's own `refs/heads/*` a
      // FROZEN mirror fixed at first clone), so it inherits a stale head even though the
      // branch is checked out at the fresh default tip. golangci-lint's ratchet
      // `issues: {new-from-merge-base: origin/main, whole-files: true}` then computes
      // `merge-base(origin/main[frozen], HEAD[fresh])` = an ancient commit and false-reds
      // the entire pre-existing backlog as branch-introduced. Point the ratchet base at the
      // fresh default head (already resolved in `defaultBranchCommit`, reachable via the
      // `--shared` alternate) so it gates only branch-introduced findings. Local + offline;
      // as RUNNER, matching the surrounding runner-owned writes. `defaultBranchCommit` may be
      // undefined (a repo with no resolvable default branch); guard and skip so we never mask
      // the existing merge-base pre-flight in the Taskfile.
      // Issue #313 — never let the ratchet base be a STRICT ANCESTOR of the branch base. On a
      // resume leg `defaultBranchCommit` can resolve (via defaultBranchSha → defaultBranchRef's
      // fallback chain) to the FROZEN refs/heads/main mirror, a stale ancestor of baseSha; #262
      // then advances origin/main to that stale commit, so merge-base(origin/main, HEAD) regresses
      // below the fork point and false-reds other people's backlog. Clamp to baseSha — the exact
      // base the lead computes by hand (--new-from-merge-base=<baseSha> returns 0 issues).
      // isAncestor is TRUE at equality, so a fresh run (defaultBranchCommit === baseSha) and an
      // ordinary resume both clamp to baseSha with NO behaviour change from #262 on the fresh run
      // (the same value is written); only the frozen-mirror case is corrected. When
      // defaultBranchCommit is NOT an ancestor of baseSha (a resume where main moved forward on a
      // divergent line) keep defaultBranchCommit so vs-main merge-base semantics are preserved.
      // Read ancestry against the worker-owned BARE with the existing worker-uid isAncestor helper
      // (both commits are bare-reachable); the WRITE stays runGitAsRunner into the runner-owned
      // clone, exactly as #262. The no-resolvable-default-branch edge keeps today's skip (origin/main
      // keeps whatever the plain clone copied); it is governed by the Taskfile merge-base pre-flight,
      // out of scope here.
      // Issue #363 — the clamp below is made DURABLE by removing the clone's `remote.origin.fetch`
      // refspec right after it, so a later agent-initiated `git fetch origin main` / `git fetch
      // origin` / `git pull` updates only FETCH_HEAD and cannot move `refs/remotes/origin/<default>`
      // back to the frozen bare mirror head, which would undo the clamp and re-corrupt the ratchet base.
      // PRD #759 M2: clamp against effectiveBase — the real fork point after a wip(park)
      // reset --soft (== baseSha on every non-marker leg, so byte-identical there).
      const defaultBranch = await this.defaultBranchName(barePath);
      let ratchetBase = defaultBranchCommit;
      if (ratchetBase && (await this.isAncestor(barePath, ratchetBase, effectiveBase))) {
        ratchetBase = effectiveBase;
      }
      if (defaultBranch && ratchetBase) {
        await this.runGitAsRunner(clonePath, ["update-ref", `refs/remotes/origin/${defaultBranch}`, ratchetBase]);
        if (ratchetBase !== defaultBranchCommit) {
          this.log.info("runner clone: clamped ratchet base to branch base (stale default ref)", {
            branch,
            baseSha,
            defaultBranchCommit,
            clamped_to: ratchetBase,
          });
        }
      }
      // Issue #363 — make the clamp above durable. `git clone` writes a
      // `remote.origin.fetch` refspec (`+refs/heads/*:refs/remotes/origin/*`); a later
      // agent-initiated `git fetch origin main` / `git fetch origin` / `git pull` would
      // re-apply it and force `refs/remotes/origin/<default>` backward to the frozen bare
      // mirror head, undoing the clamp and re-corrupting the ratchet base. With no
      // configured refspec, a fetch updates only FETCH_HEAD and touches no tracking ref, so
      // the clamp holds for the run's lifetime. `git config --unset-all` exits 5 when the key
      // is absent (a plain clone always has it, so exit 5 is only a defensive edge); treat
      // ONLY exit 5 as non-fatal and rethrow anything else.
      await this.runGitAsRunner(clonePath, ["config", "--unset-all", "remote.origin.fetch"]).catch((err: unknown) => {
        if ((err as { code?: unknown }).code === 5) return;
        throw err;
      });
      // A selfContained clone is dissociated from the bare HERE, after
      // every ref/checkpoint step and still inside this bare's withLock, so no bare maintenance
      // can interleave with the copy. A failure throws RunnerCloneMaterializationError, which
      // fails the run before the executor starts.
      if (opts?.selfContained) {
        const originBranchSha = await this.originBranchTip(barePath, branch);
        const required = [
          baseSha,
          effectiveBase,
          markerParent,
          recoveredCheckpointMarkerSha,
          defaultBranchCommit,
          ratchetBase,
          originBranchSha,
        ].filter((sha): sha is string => typeof sha === "string" && sha !== "");
        await this.materializeRunnerClone(clonePath, [...new Set(required)]);
      }
      if (this.scratchProvisioner) await this.scratchProvisioner(clonePath);
      else await this.provisionRunnerScratch(clonePath);
      // PRD #759 M2: baseCommit is the REAL fork point — effectiveBase, which is the
      // marker's parent when a wip(park) marker was reset --soft'd back to uncommitted, and
      // baseSha (byte-identical) on every other leg. wipRecovered surfaces the recovery to
      // M4/M5.
      return { path: clonePath, branch, publicationFloor, priorCommits, baseCommit: effectiveBase, defaultBranchCommit, seededFrom, checkpointSetAside, wipRecovered };
    }
  }

  /** Provision the per-run artifact directory without following checkout symlinks. */
  private async provisionRunnerScratch(clonePath: string, platform = process.platform): Promise<void> {
    const directoryFlags = fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW;
    const fdPath = (fd: number, name: string): string => `/proc/self/fd/${fd}/${name}`;
    const required = 0o2070; // setgid and group rwx for runner-cmd
    const opened: import("node:fs/promises").FileHandle[] = [];
    const openDirectory = async (name: string): Promise<import("node:fs/promises").FileHandle> => {
      const handle = await fs.open(name, directoryFlags);
      opened.push(handle);
      return handle;
    };
    try {
      // /proc/self/fd is the descriptor-relative pathname bridge used below. Refuse
      // platforms without Linux procfs or no-follow directory opens before any write.
      if (platform !== "linux" || !fsConstants.O_DIRECTORY || !fsConstants.O_NOFOLLOW) {
        throw new Error("Linux no-follow descriptor-relative scratch provisioning is unavailable");
      }
      if ((await fs.statfs("/proc/self/fd")).type !== 0x9fa0) {
        throw new Error("Linux procfs descriptor bridge is unavailable");
      }
      // The index detects a tracked file, directory content, or symlink at the reserved path.
      if ((await this.runGitAsRunner(clonePath, ["ls-files", "--cached", "--", ".uzi/scratch"])).length > 0) {
        throw new Error("tracked .uzi/scratch path");
      }
      const root = await openDirectory(clonePath);
      const rootStat = await root.stat();
      if (uidSplitActive() && (rootStat.gid !== RUNNER_UID || (rootStat.mode & required) !== required)) {
        throw new Error("runner clone lacks runner-group write posture");
      }
      if (!uidSplitActive() && (rootStat.uid !== process.getuid?.() || (rootStat.mode & 0o700) !== 0o700)) {
        throw new Error("runner clone lacks owner write posture");
      }
      const ensureDirectory = async (parent: import("node:fs/promises").FileHandle, name: string) => {
        const childPath = fdPath(parent.fd, name);
        let created = false;
        try {
          await fs.mkdir(childPath, { mode: 0o2770 });
          created = true;
        } catch (err) {
          if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err;
        }
        const child = await openDirectory(childPath);
        if (created) await child.chmod(0o2770);
        const stat = await child.stat();
        if (uidSplitActive() && (stat.gid !== RUNNER_UID || (stat.mode & required) !== required)) {
          throw new Error(`${name} lacks runner-group write posture`);
        }
        if (!uidSplitActive() && (stat.uid !== process.getuid?.() || (stat.mode & 0o700) !== 0o700)) {
          throw new Error(`${name} lacks owner write posture`);
        }
        return child;
      };
      const uzi = await ensureDirectory(root, ".uzi");
      await ensureDirectory(uzi, "scratch");

      // .git is created by the trusted local clone. Hold each directory while opening
      // its child so an agent-writable checkout path cannot redirect the exclude write.
      const git = await openDirectory(fdPath(root.fd, ".git"));
      const info = await openDirectory(fdPath(git.fd, "info"));
      const exclude = await fs.open(
        fdPath(info.fd, "exclude"),
        fsConstants.O_RDWR | fsConstants.O_CREAT | fsConstants.O_APPEND | fsConstants.O_NOFOLLOW,
        0o664,
      );
      try {
        if (!(await exclude.stat()).isFile()) throw new Error("git exclude is not a regular file");
        // The checkout can replace its exclude file before a resume. Bound the read
        // even when the file grows after opening it; an oversized file is unsafe to
        // inspect or append to, so provisioning fails closed.
        const maxExcludeBytes = 64 * 1024;
        const bytes = Buffer.alloc(maxExcludeBytes + 1);
        let used = 0;
        while (used < bytes.length) {
          const { bytesRead } = await exclude.read(bytes, used, bytes.length - used, used);
          if (bytesRead === 0) break;
          used += bytesRead;
        }
        if (used > maxExcludeBytes) throw new Error("git exclude exceeds 64 KiB");
        const existing = bytes.toString("utf8", 0, used);
        const rule = "/.uzi/scratch/";
        if (!existing.split("\n").includes(rule)) {
          const addition = `${existing.length > 0 && !existing.endsWith("\n") ? "\n" : ""}${rule}\n`;
          if (used + Buffer.byteLength(addition) > maxExcludeBytes) throw new Error("git exclude has no room for scratch rule");
          await exclude.writeFile(addition);
        }
      } finally {
        await exclude.close();
      }
      // Git will not descend into an ignored directory. Check the directory
      // itself, rather than one filename a repository could specially ignore
      // while leaving other artifacts stageable. Publication refusal remains
      // necessary if an agent later changes the repository's ignore rules.
      try {
        // Bounded: git opens .git/info/exclude with a plain blocking open, so a FIFO swapped in
        // after the regular-file check above would otherwise hold this for GIT_TIMEOUT_MS. A
        // timeout is a failed check, i.e. the same fail-closed refusal as any other failure.
        await this.runGitAsRunner(clonePath, ["check-ignore", "-q", "--no-index", "--", ".uzi/scratch/"], {
          timeoutMs: SCRATCH_IGNORE_CHECK_TIMEOUT_MS,
        });
      } catch {
        throw new Error("repository ignore rules expose .uzi/scratch to ordinary staging");
      }
    } catch (err) {
      throw new ScratchProvisionError(err);
    } finally {
      for (const handle of opened.reverse()) await handle.close().catch(() => undefined);
    }
  }

  /**
   * issue #1769 — make a runner clone SELF-CONTAINED: copy every object it borrows from the
   * worker bare into its own object store, then retire `objects/info/alternates`. The Codex
   * command sandbox does not grant the bare, so a `--shared` clone is unreadable to git there.
   *
   * Every git/mv/rm runs as the RUNNER uid (the clone is runner-owned); no git, mv or rm runs
   * against the clone as worker (the worker only lstats the alternates paths). Called only
   * by runnerCloneForBranch, inside withLock(barePath). Steps:
   * anchor each required SHA under a temporary `refs/uzi-materialize/<n>`; `repack -a -d`
   * (no `-l`, so borrowed objects are packed locally); rename the alternates file aside;
   * verify with every alternate source disabled (fsck connectivity + `cat-file -e` of HEAD
   * and each required SHA); then drop the anchors and the renamed file (best-effort, logged:
   * the clone is already self-contained by then). On any failure before that the renamed file
   * is moved back and the anchors deleted (best-effort) and a RunnerCloneMaterializationError
   * is thrown. A clone with no alternates file (lstat ENOENT) is left as is; any other lstat
   * error fails closed.
   */
  private async materializeRunnerClone(clonePath: string, requiredShas: readonly string[]): Promise<void> {
    if (this.boundaryProcesses.getStore()) {
      throw new RunnerCloneMaterializationError(clonePath, "materialization must not run inside a permit-held boundary");
    }
    const infoDir = path.join(clonePath, ".git", "objects", "info");
    const alternates = path.join(infoDir, "alternates");
    const parked = path.join(infoDir, "alternates.uzi-materialize");
    let hasAlternates: boolean;
    try {
      hasAlternates = await lstatPresent(alternates);
    } catch (err) {
      throw new RunnerCloneMaterializationError(clonePath, `cannot probe ${alternates}: ${gitErrorMessage(err)}`);
    }
    if (!hasAlternates) {
      this.log.info("runner clone: no alternates, already self-contained (materialization skipped)", { path: clonePath });
      return;
    }
    const started = Date.now();
    const env = materializeEnv(gitEnv());
    const run = async (command: string, args: string[]): Promise<string> => {
      const wrapped = runnerCommand(command, args);
      this.log.debug("runner clone materialize (runner uid)", { command, args });
      const { stdout } = await this.execScoped(wrapped.command, wrapped.args, {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
      }, "command");
      return stdout;
    };
    const git = (args: string[]): Promise<string> => run("git", withDir(clonePath, args));
    const anchors: string[] = [];
    let renamed = false;
    let verified: string[] = [];
    try {
      const head = (await git(["rev-parse", "--verify", "HEAD^{commit}"])).trim();
      verified = [...new Set([head, ...requiredShas])];
      for (const [n, sha] of verified.entries()) {
        const ref = `refs/uzi-materialize/${n}`;
        await git(["update-ref", ref, sha]);
        anchors.push(ref);
      }
      await git(["repack", "-a", "-d", "-q"]);
      await run("/bin/mv", ["-f", alternates, parked]);
      renamed = true;
      await git(["fsck", "--connectivity-only", "--no-dangling", "--no-progress"]);
      for (const sha of verified) {
        await git(["cat-file", "-e", `${sha}^{commit}`]);
      }
    } catch (err) {
      // Fail closed on the probe: only ENOENT means "not parked"; an unreadable path is
      // treated as possibly parked so the restore is still attempted (and logged if it fails).
      const parkedPresent = renamed || (await lstatPresent(parked).catch(() => true));
      if (parkedPresent) {
        await run("/bin/mv", ["-f", parked, alternates]).catch((e: unknown) =>
          this.log.warn("runner clone: could not restore alternates after failed materialization", {
            path: clonePath,
            error: gitErrorMessage(e),
          }),
        );
      }
      for (const ref of anchors) await git(["update-ref", "-d", ref]).catch(() => undefined);
      const cause = gitErrorMessage(err);
      this.log.warn("runner clone: materialization failed (alternates restored)", { path: clonePath, error: cause });
      throw new RunnerCloneMaterializationError(clonePath, cause);
    }
    // Verification passed: the clone no longer needs the bare. Leftover anchors or a leftover
    // parked file are harmless (git never reads alternates.uzi-materialize), so cleanup is
    // best-effort and a failure is logged rather than failing a usable seed.
    for (const ref of anchors) {
      await git(["update-ref", "-d", ref]).catch((e: unknown) =>
        this.log.warn("runner clone: could not delete a materialization anchor", {
          path: clonePath,
          ref,
          error: gitErrorMessage(e),
        }),
      );
    }
    await run("/bin/rm", ["-f", parked]).catch((e: unknown) =>
      this.log.warn("runner clone: could not remove the parked alternates file", {
        path: clonePath,
        error: gitErrorMessage(e),
      }),
    );
    this.log.info("runner clone: materialized (self-contained)", {
      path: clonePath,
      duration_ms: Date.now() - started,
      required: verified.length,
    });
  }

  /**
   * Turn git's detached auto-maintenance off in a repo we own (issue #134).
   *
   * Idempotent, so it is safe on the warm path — which is where it matters: `cloneBare`
   * runs only on the very first clone, and `/data` is persistent (a per-worker PVC in k8s,
   * the `agentdata` volume under compose), so a bare on an already-deployed worker is only
   * ever reached through `ensureClone`'s fetch branch.
   *
   * Both keys, deliberately — neither subsumes the other across the version range. See the
   * note on `gitEnv`'s inline pins, which close the same hole for every git that goes
   * through this module; these repo-local writes additionally cover the AGENT's own git
   * (the SDK Bash tool), which does not use `gitEnv`.
   *
   * `tryGit` rather than `runGit`: this prevents a warning-level directory leak, so it must
   * never be the reason a run fails to seed.
   */
  private async disableAutoMaintenance(repoPath: string, asRunner = false): Promise<void> {
    // No tryGitAsRunner exists; swallow explicitly rather than adding a near-duplicate of
    // tryGit whose only caller would be this one.
    const run = asRunner
      ? (args: string[]) => this.runGitAsRunner(repoPath, args).catch(() => undefined)
      : (args: string[]) => this.tryGit(repoPath, args).then(() => undefined);
    await run(["config", "maintenance.auto", "false"]);
    await run(["config", "gc.auto", "0"]);
    // `core.fsmonitor=false` closes a SECOND detached daemon that the two keys above do
    // not touch. `git fsmonitor--daemon run --detach` is spawned by any git command in a
    // repo where core.fsmonitor is true, reparents to init, and then WATCHES THE
    // DIRECTORY for as long as it lives — so unlike the maintenance child, which holds
    // its lock for milliseconds, this one holds handles indefinitely. Measured
    // 2026-08-03 on this host: a fixture created 2026-07-13 still had its daemon alive
    // 21 DAYS later, holding `data/worktrees/.../issue-55`, which is why `fs.rmSync`
    // left a file-free directory skeleton behind rather than removing the tree.
    //
    // It reaches these repos through `gitEnv()`'s deliberate GIT_CONFIG_GLOBAL
    // passthrough: with the var unset (an ordinary dev shell) git falls back to
    // ~/.gitconfig, and `core.fsmonitor = true` there is a common and reasonable
    // setting. The fixture ORIGIN is already shielded because makeFixture pins
    // GIT_CONFIG_GLOBAL=/dev/null; the repos THIS module creates are not, which is
    // exactly the residue issue #127's retry was left to absorb.
    //
    // Control, same host, same git: a repo carrying only the two keys above spawned 1
    // daemon that held the directory; adding this key spawned 0. Safe by construction —
    // fsmonitor is a `git status` optimisation for large trees, never a correctness
    // input, and these clones are short-lived and small. In a worker container the
    // daemon would die with the container anyway, so this is a no-op there and a leak
    // fix locally.
    await run(["config", "core.fsmonitor", "false"]);
  }

  /** Commits reachable from `sha` but not from the repo's default branch. Best-effort:
   *  any failure (no resolvable default, an unexpected rev-list error) answers 0, so a
   *  caller can treat a non-zero count as "there is prior work here" and nothing else. */
  /** The default branch's tip as a full OID, or undefined when it cannot be resolved.
   *  Best-effort by construction (same posture as commitsAheadOfDefault): this feeds a
   *  prompt note, and a run must never fail because a repo has no default branch.
   *
   *  Inherits defaultBranchRef's fallback chain, so on its mirror-layout rungs this returns
   *  the FROZEN ref rather than a fresh tip — see RunnerClone.baseCommit for the window. */
  private async defaultBranchSha(barePath: string): Promise<string | undefined> {
    const ref = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!ref) return undefined;
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${ref}^{commit}`])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : undefined;
  }

  private async commitsAheadOfDefault(barePath: string, sha: string): Promise<number> {
    const defaultRef = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!defaultRef) return 0;
    const n = Number.parseInt(await this.tryGitStdout(barePath, ["rev-list", "--count", `${defaultRef}..${sha}`]), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  /**
   * Worker fetches the agent branch BACK from the runner clone into the worker bare
   * (PRD #51 M3, B2). The worker is bare-only and the runner clone is runner-owned, so
   * this is the ONE point the worker reads a runner-controlled store — hardened by the
   * six B2 invariants:
   *   - single-branch refspec (`+refs/heads/<branch>:refs/uzi-runner/<branch>`), never
   *     `refs/heads/*` — the runner's whole ref namespace is never admitted (inv. 2);
   *   - `file://` transport forces the PACK protocol (upload-pack over a pipe), so the
   *     local-copy optimization that would traverse a runner-planted
   *     objects/info/alternates is NOT used — this is the specific job of the file://
   *     transport (CVE-2022-39253 class, inv. 3);
   *   - `protocol.file.allow=user` pinned deliberately for that `file://` fetch (the
   *     minimal-privilege value: this is a top-level, user-initiated fetch, which `user`
   *     — git's compiled default since 2.38.1 — allows, while NOT enabling file:// in
   *     the submodule/non-user contexts `always` would);
   *   - the worker's FETCH side (fetch-pack, ref update, object write) runs on gitEnv
   *     (GIT_CONFIG_NOSYSTEM + GIT_CONFIG_GLOBAL=/dev/null + the M0 code-exec-key pins),
   *     so the worker's OWN config governs the process that writes into its bare (inv. 4).
   *     git DOES spawn upload-pack in the runner clone, which reads the runner clone's
   *     repo-local config — but that is safe by construction: `uploadpack.packObjectsHook`
   *     is respected ONLY from PROTECTED config (documented, transport-independent,
   *     stable), so a runner repo-local plant is ignored; and upload-pack performs no
   *     checkout/diff, so the command-valued core.* keys never fire there (and the
   *     worker's inline GIT_CONFIG_* pins are inherited by it regardless);
   *   - `--no-tags`: only the branch, no runner-controlled tag namespace.
   * The agent's new objects transfer into the WORKER bare's own object store, so the
   * subsequent push does not depend on the (torn-down, possibly compromised) runner
   * clone (objects-integrity win). Returns the worker-side tracking ref pushBranch/
   * changedFiles then read.
   */
  async fetchAgentBranch(barePath: string, clonePath: string, branch: string, runId: string,
    opts: FetchAgentBranchOptions): Promise<TrackingUpdateResult> {
    if (!opts) throw new Error("fetchAgentBranch requires trusted claim context");
    opts = this.snapshotOwedOptions(opts);
    await this.validateOwedContext(barePath, branch, opts.context);
    if (runId !== opts.context.runId) throw new Error("fetchAgentBranch run identity mismatch");
    this.validateConfirmedSha(opts.remotelyConfirmedSha);
    // Only a worker-selected clone path is admitted to the pack transport.
    if (!path.isAbsolute(clonePath) || !isWithinPath(path.resolve(clonePath), path.resolve(this.runnerRoot))) {
      throw new Error("fetchAgentBranch requires a trusted runner clone path");
    }
    return this.withLock(barePath, async () => {
      const observed = await this.observeTrackingUnderLock(barePath, branch);
      const stage = `refs/uzi-incoming/${randomUUID()}`;
      // Ordinary transport failures (including boundary aborts) deliberately throw.
      await this.runGit(barePath, [
        "-c", "protocol.file.allow=user", "fetch", "--refmap=", "--no-tags",
        "--no-write-fetch-head", `file://${clonePath}`, `+refs/heads/${branch}:${stage}`,
      ]);
      const incoming = await this.resolveCommitStrict(barePath, stage);
      await this.requireOwedCommit(barePath, incoming);
      let result: TrackingUpdateResult;
      if (observed) {
        result = await this.promoteTrackingUnderLock(barePath, branch, incoming, observed, opts);
      } else {
        // The producing fetch context proves only incoming, never the unknown shared head.
        try {
          await this.pinOwedUnderLock(opts.context, incoming);
          result = { kind: "not_updated", reason: "ownership_unknown" };
        } catch (cause) {
          const abort = this.boundaryAbortError(cause);
          if (abort) throw abort;
          result = { kind: "not_updated", reason: preservationReason(cause) };
        }
      }
      // A refusal can leave stage as the sole anchor if metadata/pinning failed.
      // Delete it only after an independently verified durable anchor exists.
      try {
        if (await this.hasOwedAnchorUnderLock(barePath, opts.context.runId, incoming)) {
          await this.runGit(barePath, ["update-ref", "-d", stage, incoming]);
        } else if (result.kind === "updated" &&
            await this.checkedTrackingSha(barePath, branch) === incoming) {
          await this.runGit(barePath, ["update-ref", "-d", stage, incoming]);
        } else if (result.kind === "not_updated" && result.reason === "owed_limit") {
          // At the cap the incoming head is deliberately unpreserved (the run is stopped and its
          // clone kept), so its stage must not accumulate one ref per refused attempt.
          await this.runGit(barePath, ["update-ref", "-d", stage, incoming]);
        }
      } catch (cause) {
        const abort = this.boundaryAbortError(cause);
        if (abort) throw abort;
        if (result.kind === "updated") throw cause;
        // One cleanup attempt; a harmless stage must not erase the refusal reason.
      }
      return result;
    });
  }

  /**
   * issue #887 — clear a legacy FLAT tracking ref that path-blocks the fetch's dst ref.
   *
   * git's ref store is a directory tree: a ref FILE at `refs/uzi-runner/uzi/self-improve`
   * and a ref DIRECTORY at `refs/uzi-runner/uzi/self-improve/<runId>` cannot coexist,
   * because the leaf file occupies the very path the directory needs (a D/F — directory/
   * file — conflict). Before PRD #774 / ADR 0686 D9, a self_improve run's tracking ref was
   * the flat leaf `refs/uzi-runner/uzi/self-improve`; #774 moved it to the per-run
   * namespace `refs/uzi-runner/uzi/self-improve/<runId>`. On a worker whose persistent bare
   * still carries the pre-#774 flat leaf, that leaf is a strict path-prefix (ancestor) of
   * the new dst, so `fetch … :refs/uzi-runner/uzi/self-improve/<runId>` fails the whole
   * update with "some local refs could not be updated" and the run dies. self_improve is
   * the ONLY run kind whose ref shape changed leaf→namespace, so it is the only observed
   * failure — we clear exactly the ancestor refs that can block dst.
   *
   * The mirror case — a legacy per-run DIRECTORY blocking a new FLAT leaf (a descendant path
   * blocking its own ancestor) — is deliberately OUT OF SCOPE: no run kind moved
   * namespace→leaf, so it does not arise here, and handling it would mean deleting a whole
   * live subtree on a guess.
   *
   * Any conflicting ancestor found is ARCHIVED (its tip may carry unmerged commits) under
   * refs/uzi-archive/<sanitized>/<sha> before it is deleted. Its dangling PRD #218 owner stamp
   * is cleared under the #887 subsection key, and — when no colliding live sibling shares it —
   * under the pre-#887 flattened key too (issue #909). Deepest-first so a partially-migrated
   * bare with several stacked ancestors is cleaned bottom-up.
   */
  private async clearConflictingAncestorTrackingRefs(barePath: string, dst: string, opts: FetchAgentBranchOptions): Promise<boolean> {
    // Only refs inside the tracking namespace can D/F-conflict with a tracking-ref dst.
    if (!dst.startsWith(RUNNER_TRACKING_PREFIX)) return true;
    const suffix = dst.slice(RUNNER_TRACKING_PREFIX.length);
    const parts = suffix.split("/");
    // Cumulative prefixes STRICTLY shorter than the full branch: every part except the last.
    // For suffix "uzi/self-improve/<runId>" this yields the branches "uzi" and
    // "uzi/self-improve", i.e. the candidate refs refs/uzi-runner/uzi and
    // refs/uzi-runner/uzi/self-improve — never the namespace root and never dst itself.
    const candidates: string[] = [];
    let acc = "refs/uzi-runner";
    for (const part of parts.slice(0, -1)) {
      acc = `${acc}/${part}`;
      candidates.push(acc);
    }
    // Preflight the whole ancestor set before deleting anything. A failed read is unknown.
    const conflicts: Array<{ candidate: string; sha: string; branch: string }> = [];
    for (const candidate of candidates.reverse()) {
      const ancestorBranch = candidate.slice(RUNNER_TRACKING_PREFIX.length);
      const observed = await this.observeTrackingUnderLock(barePath, ancestorBranch);
      if (!observed) return false;
      if (!observed.sha) continue;
      if (observed.owner?.runId === opts.context.runId &&
          !(await this.remotelyCovers(barePath, observed.sha, opts.remotelyConfirmedSha))) {
        await this.pinOwedUnderLock(await this.producingContext(barePath, ancestorBranch, observed.owner), observed.sha);
      }
      conflicts.push({ candidate, sha: observed.sha, branch: ancestorBranch });
    }
    for (const { candidate, sha, branch: ancestorBranch } of conflicts) {
      // Archive first so a possibly-unmerged tip is never lost by the delete. The
      // <sanitized>/<sha> shape is D/F-safe within refs/uzi-archive (the sha leaf never
      // collides with a sibling branch's subtree) and idempotent (re-archiving the same
      // tip writes the same ref to the same sha).
      const sanitized = ancestorBranch.replace(/[^A-Za-z0-9_-]/g, "-");
      const archive = `refs/uzi-archive/${sanitized}/${sha}`;
      await this.runGit(barePath, ["update-ref", archive, sha]);
      if (await this.checkedRefSha(barePath, archive) !== sha) throw new Error("tracking archive readback mismatch");
      await this.runGit(barePath, ["update-ref", "-d", candidate, sha]);
      if (await this.checkedRefSha(barePath, candidate) !== undefined) throw new Error("tracking ancestor deletion mismatch");
      // Keep governance permanently; only the archived/deleted branch's receipt is cleared.
      const dir = this.owedDirectory(barePath);
      const receipt = await this.readOwedFile(barePath, `receipt-${this.receiptName(ancestorBranch)}.json`);
      if (receipt !== undefined) {
        await fs.unlink(path.join(dir, `receipt-${this.receiptName(ancestorBranch)}.json`));
        const handle = await fs.open(dir, fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW);
        try { await handle.sync(); } finally { await handle.close(); }
      }
      // Clear the now-dangling PRD #218 owner stamp for the deleted ref. tryGit swallows
      // exit 5 (key absent), which runGit would instead throw on — see the helper notes.
      await this.tryGit(barePath, ["config", "--local", "--unset", runnerTrackingOwnerKey(ancestorBranch)]);
      // issue #909 — a pre-#887 bare may hold the stamp under the FLATTENED key instead. Clear it
      // too, but only when unattributable-to-a-sibling: the flat key is lossy, so unsetting it
      // while a DISTINCT live tracking ref flattens to the same token would wipe THAT branch's
      // stamp (the #887 collision). The ancestor ref was deleted just above, so it is already out
      // of the live set and cannot flag itself; a colliding live sibling still would.
      if (!(await this.flatOwnerKeyAmbiguous(barePath, ancestorBranch))) {
        await this.tryGit(barePath, ["config", "--local", "--unset", legacyFlatTrackingOwnerKey(ancestorBranch)]);
      }
    }
    return true;
  }

  /** PRD #122 M6: the tip of the worker-side tracking ref `fetchAgentBranch` wrote
   *  (refs/uzi-runner/<branch>), or null when it does not exist yet. Used by the
   *  checkpoint no-op check to tell "the branch advanced since the last checkpoint"
   *  from "nothing new to fetch". Best-effort (tryGitStdout): a broken/absent ref
   *  answers null rather than throwing. */
  async trackingTip(barePath: string, branch: string): Promise<string | null> {
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${runnerTrackingRef(branch)}^{commit}`])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /** PRD #1416 M3 — resolve `rev` to a 40-hex OID in the worker bare (e.g. `<sha>^{tree}` for a
   *  tree OID, `<sha>^{commit}` for a commit), or null when it does not resolve. Best-effort
   *  (tryGitStdout): a broken/absent rev answers null rather than throwing. Used by the finalize
   *  bridge to byte-compare a synthesised bridge's tree against H's tree before adopting it. */
  async revParse(barePath: string, rev: string): Promise<string | null> {
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", rev])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /** PRD #1416 M3 — point the worker-side tracking ref `refs/uzi-runner/<branch>` at `sha`
   *  (worker-uid `update-ref`). The finalize/park/capture bridge advances this ref to the
   *  synthesised bridge B, so everything a reseed, capture, align or push then reads off the
   *  tracking ref carries B. Throws on failure (unlike the best-effort reads) so the caller can
   *  fall back to a "failed" outcome rather than silently pushing the un-bridged H. */
  async updateTrackingRef(barePath: string, branch: string, sha: string,
    opts: FetchAgentBranchOptions): Promise<TrackingUpdateResult> {
    if (!opts) throw new Error("updateTrackingRef requires trusted claim context");
    opts = this.snapshotOwedOptions(opts);
    await this.validateOwedContext(barePath, branch, opts.context);
    this.validateConfirmedSha(opts.remotelyConfirmedSha);
    await this.requireOwedCommit(barePath, sha);
    return this.withLock(barePath, async () => {
      const observed = await this.observeTrackingUnderLock(barePath, branch);
      if (!observed) return { kind: "not_updated", reason: "ownership_unknown" };
      return this.promoteTrackingUnderLock(barePath, branch, sha, observed, opts);
    });
  }

  /** issue #1117: the CURRENT origin-tracking tip of `refs/remotes/origin/<branch>`,
   *  read WITHOUT fetching, or null when unresolvable. Mirrors {@link trackingTip} but for
   *  the origin mirror rather than the runner tracking ref. Used at the mr_rework finalize
   *  push to capture O — the origin branch tip as of clone — BEFORE the detection fetch
   *  (`fetchDefaultTip`) overwrites that same ref with the fresh remote tip, so the
   *  concurrent-advance discriminator can compare O against the freshly-fetched tip.
   *  Best-effort (tryGitStdout): a broken/absent ref answers null rather than throwing. */
  async originBranchTip(barePath: string, branch: string): Promise<string | null> {
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `refs/remotes/origin/${branch}^{commit}`])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /** PRD #122 M6: the tip of the runner clone's own `refs/heads/<branch>` (the agent's
   *  committed work), or null when unresolvable. Read as the RUNNER uid — the clone is
   *  runner-owned, so a worker-uid read would hit the B2 dubious-ownership boundary
   *  (git.ts B2 invariants). Best-effort. */
  async branchTip(clonePath: string, branch: string): Promise<string | null> {
    const sha = (await this.runGitAsRunner(clonePath, ["rev-parse", "--verify", `refs/heads/${branch}^{commit}`]).catch(() => "")).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /**
   * issue #1197 (D-RC2c): positively verify the LOCAL restore point after a recovery
   * capture. Compares the WORKER bare's tracking ref (`refs/uzi-runner/<branch>`, what a
   * reseed reads) against the runner clone's current HEAD, and returns true IFF both
   * resolve to the SAME commit — i.e. `fetchAgentBranch` moved the tracking ref up to the
   * run's current tip (including any WIP-marker commit `commitWipMarker` made), so a
   * same-worker reclaim's reseed will recover exactly this tip. A clean tree whose already
   * -committed tip already matches is a no-op success. This is the fetch-back success
   * signal the `void`-returning `fetchBackBestEffort` cannot give.
   *
   * The bare ref read runs worker-uid (the bare is worker-owned, like trackingTip); the
   * HEAD read runs RUNNER-uid (`runGitAsRunner`) because the clone is runner-owned and a
   * worker-uid read there would hit the B2 dubious-ownership boundary (git.ts B2). Reading
   * HEAD is a pure ref read (no checkout/diff), so no attacker-chosen filter driver fires.
   * Swallows every error to `false`: an unresolvable ref, an unreadable clone, or a
   * mismatch all mean "restore point NOT verified", which the caller treats as a capture
   * failure (preserve, do not promote).
   */
  async verifyRunnerTrackingCovers(
    barePath: string,
    worktreePath: string,
    branch: string,
  ): Promise<boolean> {
    try {
      const trackingRef = runnerTrackingRef(branch);
      const bareTip = (
        await this.runGit(barePath, ["rev-parse", "--verify", `${trackingRef}^{commit}`])
      ).trim();
      const headTip = (
        await this.runGitAsRunner(worktreePath, ["rev-parse", "--verify", "HEAD^{commit}"])
      ).trim();
      return /^[0-9a-f]{40}$/.test(bareTip) && bareTip === headTip;
    } catch (err) {
      this.log.warn("recovery restore-point verification failed (→ not verified)", {
        bare: barePath,
        cwd: worktreePath,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /**
   * issue #1507 — the RUNNER clone's current HEAD (`HEAD^{commit}`), read as the RUNNER uid
   * because the clone is runner-owned (a worker-uid read there would hit the B2 dubious-ownership
   * boundary, git.ts B2), or null when unresolvable. This is the EXACT SHA
   * {@link verifyRunnerTrackingCovers} compares the bare tracking ref against, so a settle that has
   * just positively verified coverage can pin THIS value — the run's own PRIVATE, single-writer head
   * — instead of rereading the shared, mutable `refs/uzi-runner/<branch>` (which a concurrent run on
   * the same bare+branch could have moved since the verify). Reading HEAD is a pure ref read (no
   * checkout/diff), so no attacker-chosen filter driver fires. Best-effort: swallows to null.
   */
  async worktreeHead(worktreePath: string): Promise<string | null> {
    const sha = (
      await this.runGitAsRunner(worktreePath, ["rev-parse", "--verify", "HEAD^{commit}"]).catch(() => "")
    ).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /**
   * issue #1507 — durably ANCHOR a KNOWN-verified restore-point head in the trusted bare under a
   * run+generation-scoped pin ref, so a concurrent run moving the shared per-branch tracking ref
   * `refs/uzi-runner/<branch>` cannot leave `sha` dangling before {@link produceRecoveryBundle}
   * resolves it. UNDER THE BARE LOCK: re-confirm `sha` is a real commit present in the bare, then
   * point the pin ref at it. Returns true iff present + pinned; false (→ the caller retains the
   * source clone and keeps the hold open) when the object is absent or the update fails.
   *
   * Worker-uid, local, credential-free (no forge PAT), so it does NOT disturb the
   * reap-before-credentialed-git ordering — the credentialed forge fetch still runs later. The pin
   * ref is named from `runId` + `generation`, never the branch, so it is immune to another run's
   * fetch of the branch tracking ref. It is a custom-namespace ref (a `--all` reachability root, the
   * same posture `refs/uzi-runner` / `refs/uzi-archive` rely on to survive gc), so the anchored
   * object stays reachable through bundle production even after the tracking ref moves.
   */
  async anchorRecoveryHead(
    barePath: string,
    runId: string,
    generation: number,
    sha: string,
  ): Promise<boolean> {
    if (!/^[0-9a-f]{40}$/.test(sha)) return false;
    const pinRef = recoveryPinRef(runId, generation);
    try {
      return await this.withLock(barePath, async () => {
        const present = (
          await this.runGit(barePath, ["rev-parse", "--verify", `${sha}^{commit}`]).catch(() => "")
        ).trim();
        if (present !== sha) return false;
        await this.runGit(barePath, ["update-ref", pinRef, sha]);
        return true;
      });
    } catch (err) {
      this.log.warn("recovery: could not anchor the verified restore-point head in the trusted bare", {
        bare: barePath,
        pin_ref: pinRef,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /**
   * issue #2213 — create-only archive anchor `refs/uzi-archive/<runId>/g<generation>` at `sha`, under
   * the bare lock, for the quarantine archival capture. `update-ref <ref> <sha> ""` creates the ref
   * and refuses to move an existing one; an existing ref already at `sha` is idempotent success. The
   * ref keeps `sha`'s objects reachable (a `--all` gc root). Worker-uid, local, credential-free.
   * Never writes a recovery pin and never deletes anything. Throws only on an unexpected git
   * failure; a conflicting existing ref returns "conflict".
   */
  async createQuarantineArchiveRef(
    barePath: string,
    runId: string,
    generation: number,
    sha: string,
  ): Promise<"created" | "exists" | "conflict" | "absent"> {
    if (!/^[0-9a-f]{40}$/.test(sha)) return "absent";
    const ref = quarantineArchiveRef(runId, generation);
    return this.withLock(barePath, async () => {
      const present = (await this.runGit(barePath, ["rev-parse", "--verify", `${sha}^{commit}`]).catch(() => "")).trim();
      if (present !== sha) return "absent";
      const existing = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", "-q", ref])).trim();
      if (existing !== "") return existing === sha ? "exists" : "conflict";
      await this.runGit(barePath, ["update-ref", ref, sha, ""]);
      return "created";
    });
  }

  /** issue #2213 — the current value of the quarantine archive ref (40-hex), or null when absent.
   *  Read under the bare lock so it is ordered against {@link createQuarantineArchiveRef}. */
  async quarantineArchiveRefTip(barePath: string, runId: string, generation: number): Promise<string | null> {
    const ref = quarantineArchiveRef(runId, generation);
    return this.withLock(barePath, async () => {
      const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", "-q", ref])).trim();
      return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
    });
  }

  /**
   * issue #2213 — prove a bundle reproduces the commit `sha` on its own. The caller passes the
   * bundle BYTES (an immutable in-memory snapshot, not a path a same-uid writer can swap).
   *
   * Completeness is decided from the snapshot bytes alone, never from a repository's object store
   * (a same-uid survivor can write one: a thin bundle plus a planted loose blob would otherwise pass,
   * and git does not check a loose object's content against its name). Steps:
   *   1. a header gate in TS: `# v2 git bundle`, or v3 whose only capability is
   *      `@object-format=sha1`; no prerequisite lines; exactly ONE ref line, naming `sha`; the pack
   *      has the `PACK` signature, version 2 or 3, and its 20-byte trailer is the sha1 of the
   *      preceding bytes;
   *   2. a probe tag object pointing at `sha` is appended to the pack in memory, and
   *      `git index-pack --stdin --strict --check-self-contained-and-connected` (an internal git
   *      option, pinned by tests) runs on it in a FRESH temporary bare repository: exit 0 and stdout
   *      exactly `pack\t<new trailer>\n` prove that every object reachable from `sha` is inside the
   *      pack. The git environment pins `GIT_SHALLOW_FILE` and `GIT_GRAFT_FILE` to `/dev/null`, so a
   *      planted `shallow` or `info/grafts` file in the temporary repository cannot cut the walk
   *      short; the rest of the repository's store can only lower this verdict (never `--fix-thin`);
   *   3. `git bundle list-heads -` over stdin names exactly `sha` (cross-check).
   * SHA-256 repositories are rejected at step 1. Worker-uid, credential-free (`gitEnv()`); the
   * temporary repo is always removed. Returns undefined when verified, else a short reason (never
   * throws).
   */
  async verifyBundleReproduces(bundle: Buffer, sha: string, scratchRoot: string): Promise<string | undefined> {
    let dir: string | undefined;
    const withStdin = async (cwd: string, args: string[], input: Buffer, extraEnv: Record<string, string> = {}): Promise<string> => {
      const full = withDir(cwd, args);
      this.log.debug("git (stdin)", { cwd, args });
      const { stdout } = await this.execScoped("git", full, {
        env: { ...gitEnv(), ...extraEnv },
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
        input,
      });
      return stdout;
    };
    try {
      if (!/^[0-9a-f]{40}$/.test(sha)) return "the committed head is not a 40-hex object id";
      const parsed = GitCache.parseBundleForProbe(bundle, sha);
      if (typeof parsed === "string") return parsed;
      dir = await fs.mkdtemp(path.join(scratchRoot, "verify-"));
      await this.runGit(undefined, ["init", "--bare", "-q", dir]);
      const heads = (await withStdin(dir, ["bundle", "list-heads", "-"], bundle))
        .split("\n")
        .map((l) => l.trim())
        .filter((l) => l !== "");
      const oids = new Set(heads.map((l) => l.split(/\s+/)[0] ?? ""));
      if (oids.size !== 1 || !oids.has(sha)) return "the bundle's heads are not exactly the committed head";
      let out: string;
      try {
        out = await withStdin(
          dir,
          ["index-pack", "--stdin", "--strict", "--check-self-contained-and-connected"],
          parsed.probePack,
          { GIT_SHALLOW_FILE: "/dev/null", GIT_GRAFT_FILE: "/dev/null" },
        );
      } catch (err) {
        return `the bundle is not self-contained and connected: ${sanitizeForLog(gitErrorMessage(err), 120)}`;
      }
      if (out !== `pack\t${parsed.probeTrailerHex}\n`) return "the bundle's connectivity proof did not report the expected pack";
      return undefined;
    } catch (err) {
      return `bundle verification failed: ${sanitizeForLog(gitErrorMessage(err), 120)}`;
    } finally {
      if (dir !== undefined) await fs.rm(dir, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /**
   * issue #2213 — the header gate and in-memory probe-tag append behind {@link verifyBundleReproduces}.
   * Returns the probe pack (the bundle's pack plus one tag object naming `sha`, object count bumped,
   * trailer recomputed) and its trailer in hex, or a short rejection reason.
   */
  private static parseBundleForProbe(bundle: Buffer, sha: string): { probePack: Buffer; probeTrailerHex: string } | string {
    let pos = 0;
    const line = (): string | null => {
      const nl = bundle.indexOf(0x0a, pos);
      if (nl < 0) return null;
      const s = bundle.toString("latin1", pos, nl);
      pos = nl + 1;
      return s;
    };
    const first = line();
    if (first !== "# v2 git bundle" && first !== "# v3 git bundle") return "the bundle header is not a v2 or v3 git bundle";
    const refs: string[] = [];
    let blank = false;
    for (let l = line(); l !== null; l = line()) {
      if (l === "") {
        blank = true;
        break;
      }
      if (l.startsWith("@")) {
        if (first === "# v3 git bundle" && l === "@object-format=sha1") continue;
        return "the bundle declares an unsupported capability";
      }
      if (l.startsWith("-")) return "the bundle has prerequisites, so it is not self-contained";
      if (!/^[0-9a-f]{40} \S/.test(l)) return "the bundle header has a malformed ref line";
      refs.push(l.slice(0, 40));
    }
    if (!blank) return "the bundle header is not terminated";
    if (refs.length !== 1) return "the bundle must carry exactly one head";
    if (refs[0] !== sha) return "the bundle's heads are not exactly the committed head";
    const pack = bundle.subarray(pos);
    if (pack.length < 32 || pack.toString("latin1", 0, 4) !== "PACK") return "the bundle has no pack";
    const version = pack.readUInt32BE(4);
    if (version !== 2 && version !== 3) return "the bundle pack version is unsupported";
    const body = pack.subarray(0, pack.length - 20);
    const trailer = pack.subarray(pack.length - 20);
    if (!createHash("sha1").update(body).digest().equals(trailer)) return "the bundle pack trailer does not match its content";
    const content = Buffer.from(`object ${sha}\ntype commit\ntag uzi-archive-probe\ntagger uzi <uzi> 0 +0000\n\nprobe\n`, "latin1");
    const hdr: number[] = [];
    let size = content.length;
    let b = 0x40 | (size & 0x0f); // OBJ_TAG is type 4, in bits 6-4
    size >>>= 4;
    while (size > 0) {
      hdr.push(b | 0x80);
      b = size & 0x7f;
      size >>>= 7;
    }
    hdr.push(b);
    const grown = Buffer.concat([body, Buffer.from(hdr), deflateSync(content)]);
    grown.writeUInt32BE(pack.readUInt32BE(8) + 1, 8);
    const probeTrailer = createHash("sha1").update(grown).digest();
    return { probePack: Buffer.concat([grown, probeTrailer]), probeTrailerHex: probeTrailer.toString("hex") };
  }

  /**
   * issue #1507 — best-effort remove a pin ref planted by {@link anchorRecoveryHead}, once a durable
   * replacement exists (the bundle was archived, or the head is already forge-published). Never
   * throws: a failed cleanup only leaves a harmless ref on the long-lived bare (the same
   * never-deleted posture `refs/uzi-runner` already carries, PRD #218).
   */
  async deleteRecoveryPin(barePath: string, runId: string, generation: number): Promise<void> {
    await this.tryGit(barePath, ["update-ref", "-d", recoveryPinRef(runId, generation)]);
  }

  /**
   * issue #1582 M2 — pin the settlement candidates for ONE adopted predecessor hold at
   * `refs/uzi-settle/<runId>/<holdId>/<kind>`. UNDER THE BARE LOCK, every named SHA is first
   * re-confirmed as a real commit present in the bare; if ANY is absent (or not 40-hex) nothing is
   * written and false is returned, so the caller records no evidence. Worker-uid, local,
   * credential-free. Never throws.
   */
  async pinSettlementRefs(
    barePath: string,
    runId: string,
    holdId: string,
    shas: Partial<Record<SettlementPinKind, string>>,
  ): Promise<boolean> {
    const base = settlementPinBase(runId, holdId);
    const entries = SETTLEMENT_PIN_KINDS.flatMap((k) => (shas[k] !== undefined ? [[k, shas[k]!] as const] : []));
    if (base === null || entries.length === 0) return false;
    if (!entries.every(([, sha]) => /^[0-9a-f]{40}$/.test(sha))) return false;
    try {
      return await this.withLock(barePath, async () => {
        for (const [, sha] of entries) {
          const present = (
            await this.runGit(barePath, ["rev-parse", "--verify", `${sha}^{commit}`]).catch(() => "")
          ).trim();
          if (present !== sha) return false;
        }
        for (const [kind, sha] of entries) {
          await this.runGit(barePath, ["update-ref", `${base}${kind}`, sha]);
        }
        return true;
      });
    } catch (err) {
      this.log.warn("recovery settlement: could not pin the settlement candidates in the trusted bare", {
        bare: barePath,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /** issue #1751 M2 — best-effort remove ONE settlement pin kind for ONE hold (a live-settle leg
   *  cleared without a release drops only its `published` pin). Never throws. */
  async deleteSettlementPin(barePath: string, runId: string, holdId: string, kind: SettlementPinKind): Promise<void> {
    const base = settlementPinBase(runId, holdId);
    if (base === null) return;
    await this.tryGit(barePath, ["update-ref", "-d", `${base}${kind}`]);
  }

  /** issue #1582 M2 — best-effort remove every settlement pin for ONE hold (after the api
   *  released it), the issue #1751 `published` pin included. Never throws. */
  async deleteSettlementRefs(barePath: string, runId: string, holdId: string): Promise<void> {
    const base = settlementPinBase(runId, holdId);
    if (base === null) return;
    for (const kind of SETTLEMENT_PIN_KINDS) {
      await this.tryGit(barePath, ["update-ref", "-d", `${base}${kind}`]);
    }
  }

  /** Record clone ownership BEFORE running the model, so disk pressure during a
   * later capture cannot prevent the restart guard from knowing whose work it is.
   * The runner clears this journal only after atomically retiring the clone
   * (retireRunnerClone), never before the rename. */
  async markRecoveryCapture(barePath: string, clonePath: string, branch: string, runId: string, attemptId?: string): Promise<void> {
    await this.withLock(barePath, async () => {
      // issue #1783 M2: a Docker-wired worker also journals the attempt id (the same id its path
      // and marker carry). Absent ⇒ today's `{ runId, clonePath }` byte-for-byte.
      await this.assertRecoveryBare(barePath);
      const source = recoverySource({ runId, clonePath, ...(attemptId === undefined ? {} : { attemptId }) });
      const pending = await this.readRecoveryCapture(barePath, branch);
      if (pending && !sameRecoverySource(pending, source)) throw new Error("recovery attribution cannot be overwritten");
      const value: RecoveryJournalEntry = { ...pending, ...source };
      await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), JSON.stringify(value)]);
    });
  }

  private recoverySources(journal: RecoveryJournalEntry): RecoverySource[] {
    const sources = [journal, ...(journal.retainedSources ?? []),
      ...(journal.recovery ? [journal.recovery.source, ...(journal.recovery.successor ? [journal.recovery.successor] : [])] : [])];
    return sources.filter((s, i) => sources.findIndex(p => sameRecoverySource(p, s)) === i);
  }

  /** Protected records are custody evidence only, never seed/adoption authority. */
  private async protectedRecoveries(barePath: string, selectedRunId?: string): Promise<Array<{ branch: string; key: string; journal: RecoveryJournalEntry }>> {
    await this.assertRecoveryBare(barePath);
    const records: Array<{ branch: string; key: string; journal: RecoveryJournalEntry }> = [];
    const seen = new Set<string>();
    const config = await this.runGit(barePath, ["config", "--local", "--no-includes", "--null", "--list"]);
    for (const item of config.split("\0")) {
      const nl = item.indexOf("\n");
      const name = nl < 0 ? item : item.slice(0, nl);
      if (selectedRunId !== undefined ? name !== `uzi-retained.${selectedRunId}.journal` : !name.startsWith("uzi-retained.")) continue;
      const match = /^uzi-retained\.([^.]+)\.journal$/.exec(name);
      if (!match || seen.has(name) || nl < 0) throw new Error("invalid protected recovery descriptor");
      seen.add(name);
      const record = JSON.parse(item.slice(nl + 1));
      if (!record || typeof record !== "object" || Array.isArray(record) || record.version !== 1 ||
          Object.keys(record).sort().join(",") !== "branch,journal,key,version" ||
          typeof record.branch !== "string" || !record.branch || typeof record.key !== "string" ||
          !/^[A-Za-z0-9_-]+$/.test(record.key)) throw new Error("invalid protected recovery descriptor");
      const journal = parseRecoveryJournal(JSON.stringify(record.journal));
      if (journal.runId !== match[1] || (!journal.recovery && !journal.retainedSources?.length)) throw new Error("protected recovery run mismatch");
      await this.runGit(barePath, ["check-ref-format", "--branch", record.branch]);
      await this.attributedRecovery(barePath, record.branch, record.key, journal, journal);
      const active = await this.readRecoveryCapture(barePath, record.branch);
      if (active?.runId === journal.runId && JSON.stringify(active) !== JSON.stringify(journal)) {
        throw new Error("conflicting active and protected recovery descriptor");
      }
      records.push({ branch: record.branch, key: record.key, journal });
    }
    return records;
  }

  /** Validate the complete active snapshot before proving terminal source quiescence. */
  async terminalRetainedSnapshot(barePath: string, branch: string, key: string, expected: Pick<RecoverySource, "runId" | "clonePath">):
    Promise<{ journal: RecoveryJournalEntry; paths: string[] } | undefined> {
    return this.withLock(barePath, async () => {
      await this.protectedRecoveries(barePath);
      const journal = await this.readRecoveryCapture(barePath, branch);
      if (!journal || journal.runId !== expected.runId || journal.clonePath !== expected.clonePath) throw new Error("recovery identity changed");
      if (!journal.recovery && !journal.retainedSources?.length) return undefined;
      await this.checkedRecovery(barePath, branch, key, journal, true);
      return { journal, paths: this.recoverySources(journal).map(s => s.clonePath) };
    });
  }

  /** One bounded pass under the bare lock. A failure stops detachment, keeping active
   * or both descriptors; no pins, bytes or server custody are released. */
  async detachTerminalRetained(barePath: string, branch: string, key: string, expected: RecoveryJournalEntry): Promise<void> {
    expected = parseRecoveryJournal(JSON.stringify(expected));
    await this.withLock(barePath, async () => {
      const records = await this.protectedRecoveries(barePath);
      const prior = records.find(r => r.journal.runId === expected.runId);
      const value = JSON.stringify({ version: 1, branch, key, journal: expected });
      if (prior && JSON.stringify({ version: 1, ...prior }) !== value) throw new Error("conflicting protected recovery descriptor");
      const current = await this.readRecoveryCapture(barePath, branch);
      if (prior) await this.checkedRecovery(barePath, branch, key, expected, true, prior.journal);
      if (prior && (!current || current.runId !== expected.runId)) return;
      if (!current || JSON.stringify(current) !== JSON.stringify(expected)) throw new Error("terminal recovery snapshot changed");
      await this.checkedRecovery(barePath, branch, key, expected, true);
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      const marker = await this.readRecoveryAttemptMode(barePath, key);
      if (!marker) await this.runGit(barePath, ["config", "--local", `uzi-attemptmode.${key}.enabled`, "true"]);
      if (!await this.readRecoveryAttemptMode(barePath, key)) throw new Error("attempt mode persistence failed");
      if (!prior) await this.runGit(barePath, ["config", "--local", `uzi-retained.${expected.runId}.journal`, value]);
      const readback = (await this.protectedRecoveries(barePath)).find(r => r.journal.runId === expected.runId);
      if (!readback || JSON.stringify({ version: 1, ...readback }) !== value) throw new Error("protected recovery readback mismatch");
      await this.checkedRecovery(barePath, branch, key, expected, true, readback.journal);
      for (const source of this.recoverySources(expected)) {
        if (source.attemptId) await this.appendAttemptLedger(barePath, branch, {
          attemptId: source.attemptId, runId: source.runId, clonePath: source.clonePath, state: "reclaimed",
        });
      }
      const final = await this.readRecoveryCapture(barePath, branch);
      if (!final || JSON.stringify(final) !== JSON.stringify(expected)) throw new Error("terminal recovery snapshot changed");
      await this.checkedRecovery(barePath, branch, key, expected, true);
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
    });
  }

  private async assertRecoveryBare(barePath: string): Promise<void> {
    const st = await fs.lstat(path.join(barePath, "config"));
    if (!st.isFile() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o002) !== 0 ||
        ((st.mode & 0o020) !== 0 && (st.gid !== process.getgid?.() || (uidSplitActive() && st.gid === RUNNER_UID)))) {
      throw new Error("unsafe recovery bare config");
    }
    if (await this.resolveRecoveryBareDir(path.basename(barePath)) !== barePath) throw new Error("invalid recovery bare");
    await this.assertOwedBare(barePath);
    const entries = await this.runGit(barePath, ["config", "--local", "--no-includes", "--name-only", "--list"]);
    if (entries.split("\n").some(k => /^(include\.|includeif\.|core\.worktree$|core\.hookspath$|core\.sshcommand$)/i.test(k))) {
      throw new Error("unsafe recovery bare config");
    }
  }

  private async checkedRecovery(
    barePath: string, branch: string, key: string, expected: RecoverySource, allowMissing = false,
    detached?: RecoveryJournalEntry,
  ): Promise<RecoveryJournalEntry> {
    const { journal, repair } = await this.attributedRecovery(barePath, branch, key, expected, detached, true);
    let missing = false;
    // One pass over the recorded sources, no retries. Any unsafe sibling refuses discovery.
    for (const source of this.recoverySources(journal)) {
      let absent = false;
      for (const dir of [path.resolve(this.runnerRoot), path.dirname(source.clonePath), source.clonePath]) {
        try {
          const st = await fs.lstat(dir);
          if (!st.isDirectory() || st.isSymbolicLink()) throw new Error("unsafe recovery source path");
        } catch (err) {
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
          if (repair && sameRecoverySource(source, repair) && dir !== source.clonePath) {
            throw new Error("unsafe recovery successor ancestor");
          }
          absent = true;
          break;
        }
      }
      if (repair && sameRecoverySource(source, repair) && !absent) {
        throw new Error("recovery successor already exists without attribution");
      }
      if (absent) {
        if (journal.recovery?.successor && sameRecoverySource(source, journal.recovery.successor) &&
            !sameRecoverySource(source, journal)) continue; // identity was journaled before creation
        missing = true; continue;
      }
      const st = await fs.lstat(path.join(source.clonePath, ".git"));
      if (!st.isDirectory() || st.isSymbolicLink()) throw new Error("unsafe recovery source path");
    }
    if (missing && (!allowMissing || repair)) throw new RetainedRecoveryBlockedError(barePath, branch, key, journal);
    // checkedRecovery callers hold the bare lock. All siblings and real ancestors
    // have been validated, and the successor leaf was affirmatively ENOENT.
    if (repair) await this.appendAttemptLedger(barePath, branch, {
      attemptId: repair.attemptId!, runId: repair.runId, clonePath: repair.clonePath, state: "live",
    });
    return journal;
  }

  /** Attribution only: indexing cannot repair legacy evidence or confer execution authority. */
  private async attributedRecovery(
    barePath: string, branch: string, key: string, expected: RecoverySource,
    detached?: RecoveryJournalEntry, allowLegacyRepair = false,
  ): Promise<{ journal: RecoveryJournalEntry; repair?: RecoverySource }> {
    await this.assertRecoveryBare(barePath);
    recoverySource({ runId: expected.runId, clonePath: expected.clonePath,
      ...(expected.attemptId === undefined ? {} : { attemptId: expected.attemptId }) });
    const journal = detached ?? await this.readRecoveryCapture(barePath, branch);
    if (!journal || !sameRecoverySource(journal, expected)) throw new Error("recovery identity changed");
    if (!/^[A-Za-z0-9_-]+$/.test(key)) throw new Error("invalid recovery key");
    const canonical = this.runnerClonePath(barePath, key);
    if (journal.recovery || journal.retainedSources?.length || detached) {
      const sources = this.recoverySources(journal);
      for (const [ledgerBranch, raw] of await this.readAllAttemptLedgerRaw(barePath)) {
        const entry = parseAttemptLedgerEntry(raw);
        if (!entry) throw new Error(journal.recovery?.stage === "adopting"
          ? "unreadable or conflicting recovery successor ledger" : "unreadable recovery source ledger");
        for (const source of sources) {
          if ((entry.attemptId === source.attemptId || entry.clonePath === source.clonePath) &&
              (ledgerBranch !== branch || entry.attemptId !== source.attemptId || entry.runId !== source.runId || entry.clonePath !== source.clonePath)) {
            throw new Error("conflicting recovery source ledger");
          }
        }
      }
    }
    let repair: RecoverySource | undefined;
    for (const source of this.recoverySources(journal)) {
      const shape = this.clonePathShape(source.clonePath, canonical);
      if (!shape || shape.attemptId !== source.attemptId || source.runId !== journal.runId) {
        throw new Error("recovery source does not belong to this key");
      }
      if (await this.classifyOwnerClonePath(barePath, branch, key, source.runId, source.clonePath)) continue;
      // Old prepareRecoverySuccessor wrote adopting before its ledger entry. Only
      // that distinct, not-yet-created successor can recover the missing attribution.
      if (!allowLegacyRepair || detached || journal.recovery?.stage !== "adopting" || !journal.recovery.successor ||
          !sameRecoverySource(source, journal.recovery.successor) ||
          sameRecoverySource(source, journal) || !source.attemptId) {
        throw new Error("recovery source does not belong to this key");
      }
      // One ledger scan, no retries; malformed evidence or any conflicting
      // identity (including another branch) refuses repair before any append.
      for (const [, raw] of await this.readAllAttemptLedgerRaw(barePath)) {
        const entry = parseAttemptLedgerEntry(raw);
        if (!entry || entry.attemptId === source.attemptId || entry.clonePath === source.clonePath) {
          throw new Error("unreadable or conflicting recovery successor ledger");
        }
      }
      repair = source;
    }
    return { journal, ...(repair ? { repair } : {}) };
  }

  private checkRecoveryInterruption(): void {
    const operation = this.recoveryOperations.getStore();
    operation?.signal.throwIfAborted();
    if (operation && Date.now() >= operation.deadline) throw new Error("recovery deadline exhausted");
  }

  /** Finite source/probe pass, no retries. A failed source never hides sibling attribution. */
  private async recoveryPhysicalFailure(journal: RecoveryJournalEntry): Promise<InventoryReadCause | undefined> {
    let failure: InventoryReadCause | undefined;
    for (const source of this.recoverySources(journal)) {
      const probes: Array<[string, InventoryReadCause]> = [
        [path.resolve(this.runnerRoot), "clone_ancestor_invalid"],
        [path.dirname(source.clonePath), "clone_ancestor_invalid"],
        [source.clonePath, "clone_path_invalid"],
        [path.join(source.clonePath, ".git"), "clone_head_unreadable"],
      ];
      for (const [dir, cause] of probes) {
        this.checkRecoveryInterruption();
        let st: Stats;
        try { st = await fs.lstat(dir); }
        catch (error) {
          this.checkRecoveryInterruption();
          const code = (error as NodeJS.ErrnoException).code;
          // Only filesystem probe errors are isolated; synthetic/interrupt failures propagate.
          if (!code || !["ENOENT", "ENOTDIR", "EACCES", "EPERM", "EIO", "ELOOP", "ESTALE"].includes(code)) throw error;
          failure ??= cause;
          break;
        }
        this.checkRecoveryInterruption();
        if (!st.isDirectory() || st.isSymbolicLink()) { failure ??= cause; break; }
      }
    }
    return failure;
  }

  private warnRecoveryPhysicalFailure(): void {
    this.log.warn("recovery custody discovery: incomplete physical evidence; attribution remains protected");
  }

  private async writeRecovery(barePath: string, branch: string, journal: RecoveryJournalEntry, guard?: () => void | Promise<void>): Promise<void> {
    const value = JSON.stringify(journal);
    parseRecoveryJournal(value);
    await this.assertRecoveryBare(barePath);
    if (guard) await guard();
    await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), value]);
  }

  private async recoveryClockCurrent(barePath: string, branch: string, journal: RecoveryJournalEntry, handoffComplete = false): Promise<boolean> {
    const recovery = journal.recovery!;
    const now = Date.now();
    const invalid = !Number.isSafeInteger(now) || now < recovery.startedAt;
    if (invalid || (!handoffComplete && now >= recovery.deadline)) {
      await this.writeRecovery(barePath, branch, { ...journal, recovery: { ...recovery,
        stage: "blocked", blocker: invalid ? "clock_invalid" : "budget_exhausted" } });
      return false;
    }
    return true;
  }

  /** Credential-free effective path mode, independent of recovery journal disposition. */
  async recoveryAttemptMode(repoUrl: string, key: string): Promise<boolean> {
    return this.readRecoveryAttemptMode(this.barePathFor(repoUrl), key);
  }

  private async readRecoveryAttemptMode(barePath: string, key: string): Promise<boolean> {
    if (!/^[A-Za-z0-9_-]+$/.test(key)) throw new Error("invalid recovery key");
    await this.assertRecoveryBare(barePath);
    // A successful list distinguishes absence from an unreadable config. Reject
    // duplicate, valueless and unknown markers rather than selecting canonical.
    const marker = `uzi-attemptmode.${key}.enabled`;
    const entries = (await this.runGit(barePath, ["config", "--local", "--no-includes", "--null", "--list"]))
      .split("\0").filter(item => item === marker || item.startsWith(marker + "\n"));
    if (entries.length === 0) return false;
    if (entries.length !== 1 || entries[0] !== marker + "\ntrue") throw new Error("invalid recovery attempt mode");
    return true;
  }

  /** Credential-free discovery before ensureClone/preflight. No remote reads or creation.
   * A recorded successor is evidence only: every claim must reserve a new iteration. */
  async discoverRetainedRecovery(
    repoUrl: string, branch: string, key: string, runId: string,
  ): Promise<{ barePath: string; journal: RecoveryJournalEntry } | undefined> {
    const barePath = this.barePathFor(repoUrl);
    if (!await this.pathPresent(barePath)) return undefined;
    await this.assertRecoveryBare(barePath);
    return this.withLock(barePath, async () => {
      const journal = await this.readRecoveryCapture(barePath, branch);
      if (!journal) return undefined;
      if (journal.runId !== runId) throw new ForeignRetainedRecoveryError();
      await this.checkedRecovery(barePath, branch, key, journal);
      return { barePath, journal };
    });
  }

  /** Precharge one of three total iterations before capture. Legacy journals start at zero.
   * Recreated workers and fresh successors use the original five-minute episode deadline.
   * recoveryRetryMs is the existing base (1..30000ms). Returned backoff is base times
   * 2^(charged attempts - 1), capped at 16x base; caller sleeps outside lock/deadline. */
  async reserveRecoveryIteration(
    barePath: string, branch: string, key: string, expected: RecoverySource, recoveryRetryMs = 1_000,
  ): Promise<RecoveryProgress> {
    if (!Number.isSafeInteger(recoveryRetryMs) || recoveryRetryMs < 1 || recoveryRetryMs > 30_000) throw new Error("invalid recovery backoff");
    return this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected);
      const old = journal.recovery;
      if (old?.stage === "blocked") throw new Error("recovery episode is blocked");
      const now = Date.now();
      if (!Number.isSafeInteger(now) || now < 0 || !Number.isSafeInteger(now + 300_000) || (old && now < old.startedAt)) {
        if (old) await this.writeRecovery(barePath, branch, { ...journal, recovery: { ...old, stage: "blocked", blocker: "clock_invalid" } });
        throw new Error("invalid recovery clock");
      }
      if (old && (old.attempts >= 3 || now >= old.deadline)) {
        await this.writeRecovery(barePath, branch, { ...journal, recovery: { ...old, stage: "blocked", blocker: "budget_exhausted" } });
        throw new Error("recovery budget exhausted");
      }
      const source = old?.source ?? recoverySource({ runId: journal.runId, clonePath: journal.clonePath,
        ...(journal.attemptId ? { attemptId: journal.attemptId } : {}) });
      const recovery: RecoveryProgress = {
        ...old, version: 1, source, attempts: (old?.attempts ?? 0) + 1,
        startedAt: old?.startedAt ?? now, deadline: old?.deadline ?? now + 300_000,
        backoffMs: recoveryRetryMs * 2 ** Math.min(old?.attempts ?? 0, 4), stage: "capturing",
      };
      await this.runGit(barePath, ["config", "--local", `uzi-attemptmode.${key}.enabled`, "true"]);
      await this.writeRecovery(barePath, branch, { ...journal, recovery });
      return recovery;
    });
  }

  /** Check every recorded source against its existing restore pin; one incomplete source refuses all. */
  async verifiedRetainedRecoveryRestorePoint(barePath: string, branch: string, key: string, expected: RecoveryJournalEntry): Promise<boolean> {
    return this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected);
      if (JSON.stringify(journal) !== JSON.stringify(expected)) return false;
      // The finite descriptor and caller's shared recovery-operation deadline bound this pass.
      for (const source of this.recoverySources(journal)) {
        const tip = source.restoreTip ?? (journal.recovery && sameRecoverySource(source, journal.recovery.source)
          ? journal.recovery.restoreTip : undefined);
        if (!tip || !await this.verifiedRecoverySourceBytes(barePath, source, tip)) return false;
      }
      return true;
    });
  }

  async verifiedRecoveryRestorePoint(barePath: string, branch: string, key: string, expected: RecoverySource, tip: string): Promise<boolean> {
    return this.withLock(barePath, async () => {
      await this.checkedRecovery(barePath, branch, key, expected);
      return this.verifiedRecoverySourceBytes(barePath, expected, tip);
    });
  }

  private async verifiedRecoverySourceBytes(barePath: string, expected: RecoverySource, tip: string): Promise<boolean> {
      if (!await this.verifyRecoveryClosure(barePath, tip)) return false;
      if (await this.revParse(barePath, `refs/uzi-recovery-episode/${expected.runId}/${tip}`) !== tip) return false;
      const head = (await this.runGitAsRunner(expected.clonePath, ["rev-parse", "HEAD"])).trim();
      const tree = (await this.runGitAsRunner(expected.clonePath, ["write-tree"])).trim();
      const expectedTree = (await this.runGit(barePath, ["rev-parse", `${tip}^{tree}`])).trim();
      return tree === expectedTree && await this.isAncestor(barePath, head, tip) &&
        (await this.runGitAsRunner(expected.clonePath, ["diff", "--name-only"])).trim() === "" &&
        (await this.runGitAsRunner(expected.clonePath, ["ls-files", "--others", "--exclude-standard"])).trim() === "";
  }

  async verifyRecoveryClosure(barePath: string, tip: string): Promise<boolean> {
    const operation = this.recoveryOperations.getStore();
    operation?.signal.throwIfAborted();
    if (!SHA40_RE.test(tip)) return false;
    const budget = operation?.closureBudget ?? { delivered: 0, limit: RECOVERY_DECODED_LIMIT };
    // Ignore replacement, shallow and graft views, and forbid promisor lazy fetch.
    const env = { ...gitEnv(), GIT_NO_REPLACE_OBJECTS: "1", GIT_SHALLOW_FILE: "/dev/null",
      GIT_GRAFT_FILE: "/dev/null", GIT_NO_LAZY_FETCH: "1", GIT_ALLOW_PROTOCOL: "none" };
    const read = async (args: string[], input?: string): Promise<string> =>
      (await this.execScoped("git", withDir(barePath, ["--no-replace-objects", ...args]),
        { env, timeout: GIT_TIMEOUT_MS, maxBuffer: GIT_MAX_BUFFER, ...(input === undefined ? {} : { input }) })).stdout;
    let producer: Awaited<ReturnType<GitCache["spawnGit"]>> | undefined;
    try {
      if ((await read(["rev-parse", "--verify", `${tip}^{commit}`])).trim() !== tip) return false;
      const listing = await read(["rev-list", "--objects", "--no-object-names", "--missing=error", tip]);
      if (!listing.endsWith("\n")) return false;
      const oids = listing.slice(0, -1).split("\n");
      // Each SHA-only record occupies 41 bytes within the existing 64 MiB output cap.
      if (oids.length > Math.floor(GIT_MAX_BUFFER / 41) || oids.some(oid => !SHA40_RE.test(oid)) ||
          new Set(oids).size !== oids.length || !oids.includes(tip)) return false;
      const input = oids.join("\n") + "\n";
      const metadata = await read(["cat-file", "--batch-check"], input);
      const rows = metadata.split("\n");
      if (rows.pop() !== "" || rows.length !== oids.length) return false;
      const objects: ClosureObject[] = [];
      let declared = 0;
      for (let i = 0; i < rows.length; i++) {
        const match = /^([0-9a-f]{40}) (blob|tree|commit|tag) (0|[1-9][0-9]*)$/.exec(rows[i]!);
        if (!match || match[1] !== oids[i]) return false;
        const size = Number(match[3]);
        if (!Number.isSafeInteger(size) || size > budget.limit - budget.delivered - declared) {
          throw new RecoveryClosureLimitError();
        }
        declared += size;
        objects.push({ oid: match[1]!, type: match[2]!, size });
      }
      operation?.signal.throwIfAborted();
      producer = await this.spawnGit(barePath, ["--no-replace-objects", "cat-file", "--batch"], input,
        { env, timeoutMs: GIT_TIMEOUT_MS, stderrMaxBytes: 4096, cancelOnAbandon: true });
      await verifyClosureFrames(producer.stdout, objects, budget, operation?.signal);
      if (await producer.exited !== 0) return false;
      operation?.signal.throwIfAborted();
      return true;
    } catch (error) {
      // Abandoning the pipe cancels the exact owned producer; settlement precedes lock release.
      if (producer) { producer.cancel(); producer.stdout.destroy(); await producer.exited; }
      operation?.signal.throwIfAborted();
      if (error instanceof RecoveryClosureLimitError) {
        this.log.warn(error.message);
        throw error;
      }
      return false;
    }
  }

  /** Verify the local captured commit's complete object closure and pin BEFORE journaling.
   * Thin bundles imported with local prerequisites are valid here; publication is separate. */
  async recordRecoveryCapture(
    barePath: string, branch: string, key: string, expected: RecoverySource, iteration: number, tip: string,
  ): Promise<void> {
    if (!SHA40_RE.test(tip)) throw new Error("invalid recovery tip");
    await this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected);
      const recovery = journal.recovery;
      if (!recovery || recovery.stage !== "capturing" || recovery.attempts !== iteration ||
          !await this.recoveryClockCurrent(barePath, branch, journal)) throw new Error("capture iteration is not current");
      if (!await this.verifyRecoveryClosure(barePath, tip)) throw new Error("captured object closure is invalid");
      const sourceHead = (await this.runGitAsRunner(expected.clonePath, ["rev-parse", "HEAD"])).trim();
      if (!SHA40_RE.test(sourceHead) || !await this.isAncestor(barePath, sourceHead, tip)) throw new Error("captured history does not cover source");
      await this.runGit(barePath, ["update-ref", "--no-deref", `refs/uzi-recovery-episode/${journal.runId}/${tip}`, tip]);
      if (!await this.recoveryClockCurrent(barePath, branch, journal)) throw new Error("capture iteration is not current");
      await this.writeRecovery(barePath, branch, { ...journal, restoreTip: tip,
        recovery: { ...recovery, restoreTip: tip, stage: "captured" } });
    });
  }

  /** Store only a bounded worker reason code, never provider output or exception text.
   * expectedEpisode null requires absence; omission preserves legacy unguarded callers. */
  async blockRecoveryEpisode(
    barePath: string, branch: string, key: string, expected: RecoverySource,
    reason: RecoveryBlocker, expectedEpisode?: Pick<RecoveryProgress, "startedAt" | "deadline" | "attempts"> | null,
    guard?: () => void,
  ): Promise<void> {
    if (!["capture_failed", "source_missing", "adoption_failed", "budget_exhausted", "clock_invalid", "oversize", "prerequisites_unavailable", "quiescence_failed", "preservation_failed", "decoded_history_limit"].includes(reason)) throw new Error("invalid blocker");
    await this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected, true);
      guard?.();
      if (expectedEpisode === null && journal.recovery) throw new Error("recovery episode changed");
      if (expectedEpisode && (!journal.recovery ||
          journal.recovery.startedAt !== expectedEpisode.startedAt ||
          journal.recovery.deadline !== expectedEpisode.deadline ||
          journal.recovery.attempts !== expectedEpisode.attempts)) throw new Error("recovery episode changed");
      if (journal.recovery?.stage === "blocked") return;
      const now = Date.now();
      // Without an episode only a proven missing source can create the terminal record.
      if (!journal.recovery) {
        let missing = false;
        for (const source of this.recoverySources(journal)) {
          if (!await this.pathPresent(source.clonePath)) missing = true;
        }
        if (reason !== "source_missing" || !missing) throw new Error("no reserved recovery episode");
      }
      if (!journal.recovery && (!Number.isSafeInteger(now) || now < 0 || !Number.isSafeInteger(now + 300_000))) throw new Error("invalid recovery clock");
      const recovery: RecoveryProgress = journal.recovery ?? {
        version: 1, source: recoverySource({ runId: journal.runId, clonePath: journal.clonePath,
          ...(journal.attemptId ? { attemptId: journal.attemptId } : {}) }),
        attempts: 3, startedAt: now, deadline: now + 300_000, backoffMs: 0, stage: "blocked",
      };
      await this.runGit(barePath, ["config", "--local", `uzi-attemptmode.${key}.enabled`, "true"]);
      await this.writeRecovery(barePath, branch, { ...journal,
        recovery: { ...recovery, stage: "blocked", blocker: reason } }, guard);
      guard?.();
    });
  }

  /** Fresh successor, local-only under one bare lock. Never executes in a predecessor.
   * Adopting + successor identity land before clone creation; failure leaves that evidence.
   * Ready and attribution handoff land in ONE config write only after tip/tree/history checks.
   * This return value is usable only by the current caller, never by rediscovery after reclaim. */
  async prepareRecoverySuccessor(
    barePath: string, branch: string, key: string, expected: RecoverySource, iteration: number,
    attemptId: string, opts?: { selfContained?: boolean },
  ): Promise<RunnerClone> {
    if (!ATTEMPT_ID_RE.test(attemptId)) throw new Error("invalid successor attempt");
    return this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected);
      const recovery = journal.recovery;
      if (!recovery || recovery.stage !== "captured" || recovery.attempts !== iteration ||
          !recovery.restoreTip || !await this.recoveryClockCurrent(barePath, branch, journal)) throw new Error("successor iteration is not current");
      const tip = recovery.restoreTip;
      const pin = `refs/uzi-recovery-episode/${journal.runId}/${tip}`;
      if ((await this.runGit(barePath, ["rev-parse", "--verify", pin])).trim() !== tip) throw new Error("recovery pin changed");
      const clonePath = attemptClonePath(this.runnerClonePath(barePath, key), attemptId);
      if (await this.pathPresent(clonePath) || (await this.readAttemptLedger(barePath, branch)).has(attemptId)) throw new Error("successor identity already used");
      const successor: RecoverySource = { runId: journal.runId, clonePath, attemptId, restoreTip: tip };
      const existingSources: RecoverySource[] = [];
      for (const s of this.recoverySources(journal)) {
        if (await this.pathPresent(s.clonePath)) existingSources.push(s);
      }
      const retainedSources = existingSources.map(s => recoverySource({
        runId: s.runId, clonePath: s.clonePath, ...(s.attemptId ? { attemptId: s.attemptId } : {}),
        ...(s.restoreTip ? { restoreTip: s.restoreTip } : {}),
      }));
      await this.appendAttemptLedger(barePath, branch, { attemptId, runId: successor.runId, clonePath, state: "live" });
      await this.writeRecovery(barePath, branch, { ...journal, retainedSources,
        recovery: { ...recovery, successor, stage: "adopting" } });
      const seeded = await this.seedRunnerClone(barePath, branch, clonePath, journal.runId, true, undefined,
        { ...opts, recoveryTip: tip });
      const head = (await this.runGitAsRunner(clonePath, ["rev-parse", "HEAD"])).trim();
      const tree = (await this.runGitAsRunner(clonePath, ["write-tree"])).trim();
      const expectedTree = (await this.runGit(barePath, ["rev-parse", `${tip}^{tree}`])).trim();
      const marker = await this.isWipParkMarker(barePath, tip);
      const expectedHead = marker ? (await this.runGit(barePath, ["rev-parse", `${tip}^`])).trim() : tip;
      if (head !== expectedHead || tree !== expectedTree ||
          (await this.runGitAsRunner(clonePath, ["diff", "--name-only"])).trim() !== "" ||
          !await this.isAncestor(barePath, expectedHead, tip)) throw new Error("successor recovery verification failed");
      if (!await this.recoveryClockCurrent(barePath, branch, { ...journal, retainedSources,
        recovery: { ...recovery, successor, stage: "adopting" } })) throw new Error("successor iteration is not current");
      await this.writeRecovery(barePath, branch, { ...successor, retainedSources,
        recovery: { ...recovery, successor, stage: "ready-for-model" } });
      return { ...seeded, attemptId };
    });
  }

  /** The only budget reset. Call AFTER a successfully settled model turn, with actual
   * processing evidence from the worker. A failed journal write keeps the old budget.
   * This does not release source/pin/custody; explicit owner disposition is separate. */
  async completeRecoveryEpisode(
    barePath: string, branch: string, key: string, expected: RecoverySource,
    evidence: { settled: true; processedEvents: number; attemptId: string },
    guard?: () => void,
  ): Promise<RecoveryJournalEntry> {
    if (evidence.settled !== true || !Number.isSafeInteger(evidence.processedEvents) ||
        evidence.processedEvents <= 0 || evidence.attemptId !== expected.attemptId) throw new Error("model processing evidence required");
    return this.withLock(barePath, async () => {
      const journal = await this.checkedRecovery(barePath, branch, key, expected);
      if (journal.recovery?.stage !== "ready-for-model" ||
          !journal.recovery.successor || !sameRecoverySource(journal, journal.recovery.successor)) throw new Error("episode cannot complete");
      if (!await this.recoveryClockCurrent(barePath, branch, journal, true)) throw new Error("episode cannot complete: invalid recovery clock");
      const { recovery: _completed, ...retained } = journal;
      await this.writeRecovery(barePath, branch, retained, guard);
      return retained;
    });
  }

  /** Explicit owner discard only: drops worker attribution and episode pins without deleting
   * predecessor paths or claiming remote publication. Caller must separately release custody. */
  async discardRetainedRecovery(
    barePath: string, branch: string, key: string, expected: RecoverySource, ownerDiscard: true,
  ): Promise<void> {
    if (ownerDiscard !== true) throw new Error("explicit owner discard required");
    await this.withLock(barePath, async () => {
      recoverySource({ runId: expected.runId, clonePath: expected.clonePath,
        ...(expected.attemptId === undefined ? {} : { attemptId: expected.attemptId }) }); // Validate before constructing the descriptor key.
      const protectedRecord = (await this.protectedRecoveries(barePath, expected.runId))[0];
      if (protectedRecord && (protectedRecord.branch !== branch || protectedRecord.key !== key ||
          !sameRecoverySource(protectedRecord.journal, expected))) throw new Error("protected recovery identity changed");
      const journal = await this.checkedRecovery(barePath, branch, key, expected, true, protectedRecord?.journal);
      for (const source of this.recoverySources(journal)) {
        if (!this.clonePathShape(source.clonePath, this.runnerClonePath(barePath, key))) throw new Error("recovery key mismatch");
      }
      // The worker-owned run namespace retains attribution for every capture, including
      // superseded tips and pins created before a failed journal write. Validate the
      // complete snapshot before deleting anything; sibling run namespaces are excluded.
      const prefix = `refs/uzi-recovery-episode/${journal.runId}/`;
      // Loose enumeration is specific to the files backend. --list also succeeds
      // for an absent setting, without treating a failed config read as absence.
      const config = await this.runGit(barePath, ["config", "--local", "--no-includes", "--null", "--list"]);
      for (const entry of config.split("\0")) {
        const separator = entry.indexOf("\n");
        const name = separator < 0 ? entry : entry.slice(0, separator);
        if (name.toLowerCase() === "extensions.refstorage" &&
            (separator < 0 || entry.slice(separator + 1) !== "files")) throw new Error("unsupported recovery ref backend");
      }
      // Probe outer-to-inner before Git or readdir traverses the loose namespace.
      // Missing ancestors allow packed-only pins; unsafe ancestors refuse cleanup.
      let looseDir: string | undefined = barePath;
      for (const part of ["refs", "uzi-recovery-episode", journal.runId]) {
        looseDir = path.join(looseDir, part);
        let st: Stats;
        try { st = await fs.lstat(looseDir); }
        catch (err) {
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
          looseDir = undefined;
          break;
        }
        if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o002) !== 0 ||
            ((st.mode & 0o020) !== 0 && (st.gid !== process.getgid?.() || (uidSplitActive() && st.gid === RUNNER_UID)))) {
          throw new Error("unsafe recovery pin namespace");
        }
      }
      const listing = await this.runGit(barePath, ["for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", prefix]);
      const pins = listing.trim().split("\n").filter(Boolean).map(line => {
        const [ref, oid, symref, extra] = line.split("\0");
        const tip = ref?.startsWith(prefix) ? ref.slice(prefix.length) : "";
        if (ref === undefined || symref !== "" || extra !== undefined || !SHA40_RE.test(tip) || oid !== tip) throw new Error("invalid recovery pin identity");
        return { ref, tip };
      });
      // Git omits dangling symbolic and broken loose refs. One flat pass, no retries:
      // every loose ref must appear in the validated direct-pin snapshot before deletion.
      // Any failed sibling check refuses the entire discard and keeps all attribution.
      if (looseDir !== undefined) {
        const directRefs = new Set(pins.map(pin => pin.ref));
        for (const name of await fs.readdir(looseDir)) {
          const st = await fs.lstat(path.join(looseDir, name));
          if (!st.isFile() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o002) !== 0 ||
              ((st.mode & 0o020) !== 0 && (st.gid !== process.getgid?.() || (uidSplitActive() && st.gid === RUNNER_UID)))) {
            throw new Error("invalid recovery pin identity");
          }
          // Git's regular SHA lock files are not refs; leave deletion to report contention.
          if (name.endsWith(".lock") && SHA40_RE.test(name.slice(0, -5))) continue;
          if (!SHA40_RE.test(name) || !directRefs.has(prefix + name)) throw new Error("invalid recovery pin identity");
        }
      }
      // One attempt per enumerated pin, no retries. The first failed delete stops
      // cleanup and keeps the journal; a later explicit discard can retry remaining pins.
      for (const { ref, tip } of pins) await this.runGit(barePath, ["update-ref", "--no-deref", "-d", ref, tip]);
      if (protectedRecord) {
        await this.runGit(barePath, ["config", "--local", "--unset-all", `uzi-retained.${expected.runId}.journal`]);
        const active = await this.readRecoveryCapture(barePath, branch);
        if (active && JSON.stringify(active) === JSON.stringify(journal)) {
          await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
        }
      } else {
        await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
      }
    });
  }

  /** Read only worker-journaled/ledger-owned heads. Ref files are size-bounded;
   * no retries, and any failed sibling read refuses the whole verification.
   * Runner config is never consulted, and every traversed component rejects symlinks. */
  async readInventoryCloneHeads(barePath: string, runId: string): Promise<
    { kind: "verified"; heads: string[]; clones: Array<{ clonePath: string; branch: string; runId: string }>; foreignOwners: string[] } | { kind: "unknown"; cause?: InventoryReadCause }
  > {
    let cause: InventoryReadCause = "other";
    let physicalFailure = false;
    let ownPhysicalFailure: InventoryReadCause | undefined;
    const ownRetainedPaths = new Set<string>();
    const inspect = async (journal: RecoveryJournalEntry) => {
      if (journal.runId === runId) {
        for (const source of this.recoverySources(journal)) ownRetainedPaths.add(source.clonePath);
      }
      const failure = await this.recoveryPhysicalFailure(journal);
      if (failure) {
        physicalFailure = true;
        if (journal.runId === runId) ownPhysicalFailure ??= failure;
      }
    };
    function refuse(failure: InventoryReadCause, message: string): never {
      cause = failure;
      throw new Error(message);
    }
    // Classification is scoped to the failing operation, never a phase left by a sibling.
    const atFailure = async <T>(failure: InventoryReadCause, operation: () => Promise<T>): Promise<T> => {
      try { return await operation(); } catch (err) { cause = failure; throw err; }
    };
    try {
      if (typeof runId !== "string" || !OWED_RUN_ID.test(runId) ||
          await atFailure("git_or_filesystem_error", () => this.resolveRecoveryBareDir(path.basename(barePath))) !== barePath) return { kind: "unknown", cause };
      return await this.withLock(barePath, async () => {
        const paths = new Map<string, { branch: string; runId: string }>();
        const foreignOwners = new Set<string>();
        // Keep the initial config read in its operation-scoped error classifier.
        const config = await atFailure("git_or_filesystem_error", () => this.runGit(barePath, ["config", "--local", "--null", "--list"]));
        for (const record of await this.protectedRecoveries(barePath)) {
          await inspect(record.journal);
          if (record.journal.runId !== runId) { foreignOwners.add(record.journal.runId); continue; }
          for (const source of this.recoverySources(record.journal)) paths.set(source.clonePath, { branch: record.branch, runId });
        }
        const entries = config.split("\0");
        const journals = new Map<string, string>();
        for (const item of entries) {
          const nl = item.indexOf("\n");
          const match = /^uzi-recovery\.(.+)\.clone$/.exec(item.slice(0, nl));
          if (!match) continue;
          journals.set(match[1]!, item.slice(nl + 1));
        }
        for (const [branch, value] of journals) {
          // Retirement clears this key to empty. Like readRecoveryCapture, the
          // latest value is authoritative; nonempty invalid attribution still refuses FINAL.
          if (value === "") continue;
          const journal = await (async () => {
            try { return await this.readRecoveryCapture(barePath, branch, entries); }
            catch (err) {
              cause = err instanceof InvalidRecoveryClonePathError ? "clone_path_invalid" : "attribution_unreadable";
              throw err;
            }
          })();
          if (!journal) refuse("attribution_unreadable", "unreadable recovery attribution");
          if (journal.attemptId !== undefined &&
              (parseAttemptPath(journal.clonePath, path.resolve(this.runnerRoot))?.attemptId ?? "") !== journal.attemptId) {
            refuse("clone_path_invalid", "recovery attempt identity disagrees with clone path");
          }
          if (journal.recovery || journal.retainedSources?.length) {
            const primary = parseAttemptPath(journal.clonePath, path.resolve(this.runnerRoot));
            await this.attributedRecovery(barePath, branch, primary?.key ?? path.basename(journal.clonePath), journal);
            await inspect(journal);
          }
          if (journal.runId !== runId) { foreignOwners.add(journal.runId); continue; }
          for (const source of this.recoverySources(journal)) {
            if (source.attemptId !== undefined &&
                parseAttemptPath(source.clonePath, path.resolve(this.runnerRoot))?.attemptId !== source.attemptId) throw new Error("retained source identity mismatch");
            paths.set(source.clonePath, { branch, runId });
          }
        }
        if (ownPhysicalFailure) refuse(ownPhysicalFailure, "incomplete retained recovery source");
        // Unlike advisory backup readers, FINAL cannot skip malformed ledger evidence.
        for (const [, raw] of await atFailure("git_or_filesystem_error", () => this.readAllAttemptLedgerRaw(barePath))) {
          if (!parseAttemptLedgerEntry(raw)) refuse("attribution_unreadable", "unreadable attempt attribution");
        }
        for (const entry of (await atFailure("git_or_filesystem_error", () => this.readAllAttemptLedgers(barePath))).values()) {
          if (entry.runId !== runId) { foreignOwners.add(entry.runId); continue; }
          paths.set(entry.clonePath, { branch: entry.branch, runId });
        }
        const heads = new Set<string>();
        const clones: Array<{ clonePath: string; branch: string; runId: string }> = [];
        clonePaths: for (const [clone, owner] of paths) {
          const parsed = parseAttemptPath(clone, path.resolve(this.runnerRoot));
          const key = parsed?.key ?? path.basename(clone);
          if (!parsed && !/^[A-Za-z0-9_-]+$/.test(key)) refuse("clone_path_invalid", "unknown canonical clone key");
          if (!await atFailure("git_or_filesystem_error", () => this.classifyOwnerClonePath(barePath, owner.branch, key, runId, clone))) {
            refuse("clone_path_invalid", "unknown retained clone path");
          }
          const root = path.resolve(this.runnerRoot);
          if (!path.isAbsolute(clone) || path.resolve(clone) !== clone ||
              path.dirname(path.dirname(clone)) !== root) refuse("clone_path_invalid", "unsafe clone path");
          // Probe outer-to-inner. Only ordinary non-retained absence may skip a clone;
          // retained sources must still be present after the physical prepass.
          for (const dir of [root, path.dirname(clone)]) {
            let st: Stats;
            try { st = await fs.lstat(dir); }
            catch (err) {
              if ((err as NodeJS.ErrnoException).code === "ENOENT" && !ownRetainedPaths.has(clone)) continue clonePaths;
              cause = "clone_ancestor_invalid"; throw err;
            }
            if (!st.isDirectory() || st.isSymbolicLink()) refuse("clone_ancestor_invalid", "unsafe clone parent");
          }
          let st: Stats;
          try { st = await fs.lstat(clone); }
          catch (err) {
            if ((err as NodeJS.ErrnoException).code === "ENOENT") {
              if (ownRetainedPaths.has(clone)) refuse("clone_path_invalid", "missing retained clone");
              continue;
            }
            cause = "git_or_filesystem_error"; throw err;
          }
          if (!st.isDirectory() || st.isSymbolicLink()) refuse("clone_path_invalid", "unsafe clone");
          const head = await atFailure("clone_head_unreadable", async () => {
            const gitdir = path.join(clone, ".git");
            const gs = await fs.lstat(gitdir);
            if (!gs.isDirectory() || gs.isSymbolicLink()) throw new Error("unsafe clone git directory");
            const readRef = async (relative: string): Promise<string> => {
              const parts = relative.split("/");
              for (let i = 1; i < parts.length; i++) {
                const ds = await fs.lstat(path.join(gitdir, ...parts.slice(0, i)));
                if (!ds.isDirectory() || ds.isSymbolicLink()) throw new Error("unsafe ref parent");
              }
              const file = await fs.open(path.join(gitdir, relative), fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
              try {
                const stat = await file.stat();
                if (!stat.isFile() || stat.size > 1024 * 1024) throw new Error("unsafe ref file");
                const bytes = Buffer.alloc(1024 * 1024 + 1);
                const { bytesRead } = await file.read(bytes, 0, bytes.length, 0);
                if (bytesRead > 1024 * 1024) throw new Error("ref exceeds verification bound");
                return bytes.subarray(0, bytesRead).toString("utf8").trim();
              } finally { await file.close(); }
            };
            let head = await readRef("HEAD");
            if (head.startsWith("ref: ")) {
              const ref = head.slice(5);
              if (ref.length > 256 || !/^refs\/heads\/[A-Za-z0-9_./-]+$/.test(ref) ||
                  ref.split("/").some(p => !p || p === "." || p === "..")) throw new Error("unsafe HEAD ref");
              try { head = await readRef(ref); }
              catch (err) {
                if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
                const packed = await readRef("packed-refs");
                head = packed.split("\n").find(line => line.slice(41) === ref)?.slice(0, 40) ?? "";
              }
            }
            if (!SHA40_RE.test(head)) throw new Error("unreadable retained HEAD");
            return head;
          });
          heads.add(head);
          clones.push({ clonePath: clone, ...owner });
        }
        return { kind: "verified", heads: [...heads], clones, foreignOwners: [...foreignOwners] };
      });
    } catch (error) {
      this.checkRecoveryInterruption();
      if (error instanceof Error && (error.name === "AbortError" || error.name === "TimeoutError")) throw error;
      return { kind: "unknown", cause };
    } finally {
      this.checkRecoveryInterruption();
      if (physicalFailure) this.warnRecoveryPhysicalFailure();
    }
  }

  private async readRecoveryCapture(barePath: string, branch: string, snapshot?: string[]): Promise<RecoveryJournalEntry | undefined> {
    // Unlike tryGitStdout, --list succeeds when the key is absent and throws on
    // an unreadable/corrupt config. Never interpret a failed read as no journal.
    const entries = snapshot ?? (await this.runGit(barePath, ["config", "--local", "--null", "--list"])).split("\0");
    const prefix = `${recoveryCaptureKey(branch)}\n`;
    const entry = entries.filter((item) => item.startsWith(prefix)).at(-1);
    const value = entry?.slice(prefix.length);
    if (!value) return undefined;
    return parseRecoveryJournal(value);
  }

  /** Every clone path any branch's recovery journal names (issue #1783 M2: the retention sweep
   *  never deletes a journaled path). Throws on an unreadable config or a malformed journal. */
  private async journaledClonePaths(barePath: string): Promise<Set<string>> {
    const out = new Set<string>();
    let physicalFailure = false;
    try {
      for (const record of await this.protectedRecoveries(barePath)) {
        if (await this.recoveryPhysicalFailure(record.journal)) physicalFailure = true;
        for (const source of this.recoverySources(record.journal)) out.add(source.clonePath);
      }
      for (const item of (await this.runGit(barePath, ["config", "--local", "--null", "--list"])).split("\0")) {
        const nl = item.indexOf("\n");
        if (nl < 0) continue;
        const k = item.slice(0, nl);
        if (!/^uzi-recovery\..+\.clone$/.test(k)) continue;
        const v = item.slice(nl + 1);
        if (v) {
          const journal = parseRecoveryJournal(v);
          if (journal.recovery || journal.retainedSources?.length) {
            const primary = parseAttemptPath(journal.clonePath, path.resolve(this.runnerRoot));
            const branch = k.slice("uzi-recovery.".length, -".clone".length);
            await this.attributedRecovery(barePath, branch, primary?.key ?? path.basename(journal.clonePath), journal);
            if (await this.recoveryPhysicalFailure(journal)) physicalFailure = true;
          }
          for (const source of this.recoverySources(journal)) out.add(source.clonePath);
        }
      }
      return out;
    } finally {
      this.checkRecoveryInterruption();
      if (physicalFailure) this.warnRecoveryPhysicalFailure();
    }
  }

  /** issue #1783 M2: append one attempt-ledger value (see {@link attemptLedgerKey}). Lock-free:
   *  every caller already holds the bare lock. The field order is the documented contract. */
  private async appendAttemptLedger(barePath: string, branch: string, entry: AttemptLedgerEntry): Promise<void> {
    await this.runGit(barePath, ["config", "--local", "--add", attemptLedgerKey(branch), attemptLedgerValue(entry)]);
  }

  /**
   * issue #1783 M2 review (N4, NB2, NB3) — bound one branch's attempt ledger, under the caller's bare
   * lock (the attempt seed). Rewrites `uzi-attempts.<branch>.entry` to the LAST value per attemptId
   * (in last-write order), dropping every unparseable value and every entry whose (absolute) path is
   * gone (lstat ENOENT), whatever its state: after a pod roll `<runnerRoot>` is an emptyDir, so a
   * gone path has nothing left for any reader to find. Two exceptions are always kept:
   *   - an entry any branch's recovery journal still names (the journal is what (d′) and a capture
   *     resolve through the ledger);
   *   - a gone `reclaimed` entry while its run's journal or custody record (`recovery/<runId>`,
   *     `recovery-settlement/<runId>`) exists; with both gone it is dropped.
   * Nothing is rewritten when nothing would change.
   *
   * The rewrite is ATOMIC ({@link rewriteLedgerAtomically}): the full compacted value set lands in
   * one rename of the bare config, under git's own `config.lock`, so a crash part-way can never
   * leave a partial ledger (the old `--replace-all` then N×`--add` could drop a foreign owner's
   * `live` entry a journal still names, wedging (d′)). Best-effort: a failure (including a
   * concurrent git holding `config.lock`) is logged and the ledger is left exactly as it was.
   */
  private async compactAttemptLedger(barePath: string, branch: string): Promise<void> {
    try {
      const key = attemptLedgerKey(branch);
      const raw = (await this.readAllAttemptLedgerRaw(barePath)).filter(([b]) => b === branch).map(([, v]) => v);
      const last = new Map<string, AttemptLedgerEntry>();
      for (const v of raw) {
        const e = parseAttemptLedgerEntry(v);
        if (!e) continue;
        last.delete(e.attemptId);
        last.set(e.attemptId, e);
      }
      const journaled = [...(await this.journaledClonePaths(barePath))];
      const namedByJournal = (p: string): boolean => journaled.some((j) => isWithinPath(j, p) || isWithinPath(p, j));
      const kept: string[] = [];
      for (const e of last.values()) {
        const gone = path.isAbsolute(e.clonePath) && !(await this.pathPresent(e.clonePath));
        const droppable = gone && !namedByJournal(e.clonePath) && !(await this.hasPhysicalTerminalProtection(e.runId)) &&
          (e.state !== "reclaimed" || !(await this.custodyHeld(e.runId)));
        if (!droppable) kept.push(attemptLedgerValue(e));
      }
      if (kept.length === raw.length && kept.every((v, i) => v === raw[i])) return;
      await this.rewriteLedgerAtomically(barePath, key, kept);
    } catch (err) {
      this.checkRecoveryInterruption();
      this.log.warn("attempt ledger compaction failed; the ledger is left as it was", { branch, error: gitErrorMessage(err) });
    }
  }

  /**
   * Replace every value of the multi-valued config `key` in the bare's config with `values`, in ONE
   * rename, following git's own lockfile protocol (lockfile.c): the new content is built in a
   * private temp file (a byte copy of the config, edited with `git config --file`), then published
   * by hard-linking it to `config.lock` — an exclusive create, exactly git's `O_CREAT|O_EXCL` lock,
   * so it fails (EEXIST) while any git holds the lock, and every git writer fails while we hold it —
   * then the config is re-read and compared to the snapshot the edit started from (a writer that
   * committed between the snapshot and our lock aborts us instead of being overwritten), and
   * `config.lock` is renamed over `config`. Any failure before the rename leaves `config` untouched
   * and removes our lock. A process crash between the link and the rename leaves a stale
   * `config.lock` (the same window git's own config writes have), never a partial ledger.
   */
  private async rewriteLedgerAtomically(barePath: string, key: string, values: string[]): Promise<void> {
    const cfgPath = path.join(barePath, "config");
    const lockPath = `${cfgPath}.lock`;
    // A temp file left by a crashed compaction is inert (never read by git); clear it. Only this
    // method creates the prefix, always under the bare lock.
    for (const n of await fs.readdir(barePath)) {
      if (n.startsWith(LEDGER_COMPACT_TMP_PREFIX)) await fs.rm(path.join(barePath, n), { force: true });
    }
    const snapshot = await fs.readFile(cfgPath);
    const mode = (await fs.stat(cfgPath)).mode & 0o777;
    const tmp = path.join(barePath, `${LEDGER_COMPACT_TMP_PREFIX}${randomUUID()}`);
    try {
      await fs.writeFile(tmp, snapshot, { flag: "wx", mode });
      if (values.length === 0) {
        await this.runGit(barePath, ["config", "--file", tmp, "--unset-all", key]);
      } else {
        await this.runGit(barePath, ["config", "--file", tmp, "--replace-all", key, values[0]!]);
        for (const v of values.slice(1)) await this.runGit(barePath, ["config", "--file", tmp, "--add", key, v]);
      }
      await fs.chmod(tmp, mode);
      await fs.link(tmp, lockPath);
      try {
        if (!(await fs.readFile(cfgPath)).equals(snapshot)) throw new Error("the bare config changed during ledger compaction");
        await fs.rename(lockPath, cfgPath);
      } catch (err) {
        await fs.rm(lockPath, { force: true });
        throw err;
      }
    } finally {
      await fs.rm(tmp, { force: true });
    }
  }

  /** issue #1783 M2: one branch's attempt ledger, the LAST value per attemptId winning.
   *  Unparseable values are skipped (the ledger is advisory for backups, and every consumer
   *  here fails closed on a missing entry); an unreadable config throws. */
  private async readAttemptLedger(barePath: string, branch: string): Promise<Map<string, AttemptLedgerEntry>> {
    const out = new Map<string, AttemptLedgerEntry>();
    for (const [b, e] of (await this.readAllAttemptLedgerValues(barePath))) {
      if (b === branch) out.set(e.attemptId, e);
    }
    return out;
  }

  /** Every branch's attempt ledger, the LAST value per attemptId winning (ids are globally
   *  unique: 64 random bits). */
  private async readAllAttemptLedgers(barePath: string): Promise<Map<string, AttemptLedgerEntry & { branch: string }>> {
    const out = new Map<string, AttemptLedgerEntry & { branch: string }>();
    for (const [branch, e] of (await this.readAllAttemptLedgerValues(barePath))) out.set(e.attemptId, { ...e, branch });
    return out;
  }

  private async readAllAttemptLedgerValues(barePath: string): Promise<Array<[string, AttemptLedgerEntry]>> {
    const out: Array<[string, AttemptLedgerEntry]> = [];
    for (const [branch, v] of await this.readAllAttemptLedgerRaw(barePath)) {
      const e = parseAttemptLedgerEntry(v);
      if (e) out.push([branch, e]);
    }
    return out;
  }

  /** Every raw attempt-ledger value, in config order, with its branch. Throws on an unreadable config. */
  private async readAllAttemptLedgerRaw(barePath: string): Promise<Array<[string, string]>> {
    const out: Array<[string, string]> = [];
    for (const item of (await this.runGit(barePath, ["config", "--local", "--null", "--list"])).split("\0")) {
      const nl = item.indexOf("\n");
      const m = /^uzi-attempts\.(.+)\.entry$/.exec(nl < 0 ? item : item.slice(0, nl));
      // Valueless ledger keys remain malformed evidence for strict readers.
      if (m) out.push([m[1]!, nl < 0 ? "" : item.slice(nl + 1)]);
    }
    return out;
  }

  /** Callback errors retain custody; no identity is inferred from terminal body fields. */
  async hasPhysicalTerminalProtection(runId: string): Promise<boolean> {
    try {
      return await this.terminalRecordProtection?.(runId) ?? false;
    } catch {
      return true;
    }
  }

  /** issue #1783 M2: true when a custody or capture record under the recovery stores belongs to
   *  `runId` (a durable-recovery journal/bundle under `recovery/<runId>`, or an ancestry
   *  settlement record under `recovery-settlement/<runId>`). Neither record names a clone path,
   *  so any record of the run holds every retained attempt of that run. An unreadable store
   *  counts as held (fail closed). */
  private async custodyHeld(runId: string): Promise<boolean> {
    if (await this.hasPhysicalTerminalProtection(runId)) return true;
    for (const root of [this.recoveryRoot, this.recoverySettlementRoot]) {
      try {
        if ((await fs.readdir(path.join(root, runId))).length > 0) return true;
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") return true;
      }
    }
    return false;
  }

  /**
   * issue #1783 M2 — release a predecessor attempt IN PLACE: a Docker-wired worker's replacement for
   * retireRunnerClone after a VERIFIED capture (`state` `abandoned`: its work is in the tracking ref,
   * so the retention sweep may later dispose of it), and for a terminal FOREIGN owner's attempt in
   * the orphan reclaim, which captures nothing (`state` `reclaimed`: retained forever, never
   * disposable — see attemptLedgerKey). Under the bare lock it re-validates that the journal still
   * names this exact (runId, clonePath) and refuses otherwise (nothing written), appends the ledger
   * state, and only THEN clears the journal (the caller calls this only once any capture is
   * verified — the #1197 rule). The ledger goes first so the journal never stops protecting a path
   * whose release the ledger does not yet record: a path left `live`, unjournaled and not live here
   * is a crash-window orphan the retention sweep may delete, which an uncaptured `reclaimed` attempt
   * must never become. A failure after the re-validation throws {@link AttemptReleaseError} with the
   * journal kept. It performs NO filesystem operation on the path: a successor never
   * moves or deletes a predecessor's path; only the retention sweep disposes of an `abandoned` one.
   */
  async releaseAttemptInPlace(
    barePath: string,
    clonePath: string,
    branch: string,
    runId: string,
    state: "abandoned" | "reclaimed",
  ): Promise<void> {
    await this.withLock(barePath, async () => {
      const pending = await this.readRecoveryCapture(barePath, branch);
      if (pending?.runId !== runId || pending.clonePath !== clonePath) {
        throw new CapturePathMismatchError(pending?.clonePath ?? "", clonePath, branch, runId);
      }
      // Episodes retain source attribution/accounting; ordinary legacy release still clears.
      // issue #2213: a latch that landed during the lock wait / journal read keeps the journal.
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      const attemptId = pending.attemptId ?? parseAttemptPath(clonePath, path.resolve(this.runnerRoot))?.attemptId;
      if (attemptId !== undefined) {
        try {
          await this.appendAttemptLedger(barePath, branch, { attemptId, runId, clonePath, state });
        } catch (err) {
          throw new AttemptReleaseError("ledger", err);
        }
      } else {
        // A legacy canonical path has no attempt identity; it is never reseeded on a wired worker.
        this.log.info("releasing a legacy canonical clone in place (no attempt ledger entry)", { clone: clonePath, state });
      }
      // issue #2213: recheck right before the clear (the ledger append awaited). It sits OUTSIDE the
      // try so it surfaces as CloneRetainedByQuarantineError, not a journal-stage AttemptReleaseError;
      // nothing is awaited between it and the clear's runGit.
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      try {
        const retainedSources = this.recoverySources(pending).map(s => recoverySource({
          runId: s.runId, clonePath: s.clonePath, ...(s.attemptId ? { attemptId: s.attemptId } : {}),
          ...(s.restoreTip ? { restoreTip: s.restoreTip } : {}),
        }));
        if (pending.recovery || pending.retainedSources?.length) {
          await this.writeRecovery(barePath, branch, { ...pending, retainedSources });
        } else {
          await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
        }
      } catch (err) {
        throw new AttemptReleaseError("journal", err);
      }
    });
  }

  /**
   * issue #1783 M2 — predicate (d′) of the terminal-orphan reclaim: does `journaledPath` belong to
   * the OWNER whose clone key is `ownerKey`? `canonical` when it IS the owner's canonical path;
   * `attempt` when it parses (whole grammar, normalized, directly under the repo dir) as
   * `<that canonical>.attempt-<id>` AND the ledger records that attemptId for `ownerRunId` at
   * exactly this path; undefined otherwise (a different key, a traversal, an id the ledger does
   * not record for the owner) — the caller then fails closed with the path untouched.
   */
  async classifyOwnerClonePath(
    barePath: string,
    branch: string,
    ownerKey: string,
    ownerRunId: string,
    journaledPath: string,
  ): Promise<"canonical" | "attempt" | undefined> {
    const canonical = this.runnerClonePath(barePath, ownerKey);
    const shape = this.clonePathShape(journaledPath, canonical);
    if (!shape) return undefined;
    if (shape.attemptId === undefined) return "canonical";
    const entry = (await this.readAttemptLedger(barePath, branch)).get(shape.attemptId);
    return entry?.runId === ownerRunId && entry.clonePath === journaledPath ? "attempt" : undefined;
  }

  /**
   * PRD #122 M8 — the delta packfile of `<exclude>..refs/uzi-runner/<branch>`, for a
   * brokered origin publish at a checkpoint. Returns `{ tipOid, pack, exited }` where
   * `tipOid` is the tracking-ref tip (the same the checkpoint fetched back), `pack`
   * STREAMS the packfile bytes, and `exited` settles with the producer's exit code;
   * null when there is no tracking ref yet (nothing to publish).
   *
   * The exclude boundary mirrors the reseed's floor: `refs/remotes/origin/<branch>` when
   * origin carries the branch, else the default branch — so the pack carries only what the
   * checkpoint added beyond what origin already has.
   *
   * STREAMS, never buffers: a pack can exceed the 64 MiB `maxBuffer` cap runGit uses, so
   * this spawns `git pack-objects --revs --stdout` and hands back its stdout as a Readable
   * for the client to upload directly. Credential-free: this is a LOCAL read of the
   * WORKER-OWNED bare by the worker uid (base gitEnv, no PAT, no runner-uid switch — the
   * runner does not own the bare). A shared lock is not taken: pack-objects reads a
   * consistent object snapshot, and this is a read, not a mutation.
   */
  async checkpointPack(
    barePath: string,
    branch: string,
    overlay?: CheckpointOverlayContext,
    pinned?: CheckpointRange,
    onStep?: (step: BoundaryStep) => void,
    /** Exact committed ownership snapshot supplied by the runner; never a source ref name. */
    sourceSha?: string,
    /** Judge the actual candidate after overlay/floor validation, before starting a producer. */
    beforePack?: (wantedSha: string) => Promise<void>,
  ): Promise<{ tipOid: string; pack: Readable; exited: Promise<number> } | null> {
    // A pinned range uses literal commit OIDs for the pack floor and candidate.
    // If an overlay is requested, its wrapper becomes the wanted OID while the
    // excluded floor remains pinned.
    if (pinned) {
      if (!SHA40_RE.test(pinned.tipSha) || !SHA40_RE.test(pinned.excludeSha) ||
          (sourceSha !== undefined && sourceSha !== pinned.tipSha)) {
        throw new ScratchPublicationError("pinned checkpoint range must be two 40-hex commit SHAs", undefined, {
          kind: "checkpoint_range", step: "checkpoint_floor", detail: "pinned range is not two 40-hex commit SHAs",
        });
      }
      onStep?.("scratch_preflight");
      await this.scratchPublicationPreflight(barePath, branch, pinned.tipSha);
      let wanted = pinned.tipSha;
      if (overlay) {
        wanted = await this.buildWorkflowOverlay(barePath, branch, pinned.tipSha, overlay, onStep) ?? wanted;
        onStep?.("scratch_preflight");
        await this.scratchPublicationPreflight(barePath, branch, wanted);
      }
      await this.validateCheckpointFloor(barePath, pinned.excludeSha, wanted);
      await beforePack?.(wanted);
      onStep?.("checkpoint_pack");
      const { stdout, exited } = await this.spawnGit(
        barePath,
        ["pack-objects", "--revs", "--stdout"],
        `${wanted}\n^${pinned.excludeSha}\n`,
      );
      return { tipOid: wanted, pack: stdout, exited };
    }
    const realTip = sourceSha ?? await this.trackingTip(barePath, branch);
    if (!realTip) return null;
    onStep?.("scratch_preflight");
    await this.scratchPublicationPreflight(barePath, branch, realTip);
    // An unresolvable floor (e.g. no origin branch and a tip disjoint from the default, where
    // merge-base exits non-zero) is a range that cannot be established: refuse with the typed
    // reason rather than letting a generic git error escape the publication seam.
    let excludeSha: string | null;
    try {
      const excludeRef = await this.checkpointExcludeRef(barePath, branch, realTip);
      excludeSha = await this.revParse(barePath, `${excludeRef}^{commit}`);
    } catch (e) {
      const abort = this.boundaryAbortError(e);
      if (abort) throw abort;
      throw new ScratchPublicationError("checkpoint floor cannot be resolved", e, {
        kind: "checkpoint_range", step: "checkpoint_floor", ...execDetail(classifyExecFailure(e)),
      });
    }

    // PRD #1062 M2 (#1036) — the `.github/workflows` overlay. When an overlay context is
    // supplied (GitHub, agent already reaped — see runner.ts), attempt to build a genuine
    // fast-forward wrapper commit `O_ov` whose `.github/workflows` tree equals the default's,
    // and pack THAT instead of the raw tracking tip, so a branch behind `main` on those files
    // is no longer rejected `workflow_scope` and checkpoints durably. An ordinary overlay error
    // or gate-miss returns null and ships `realTip` (→ the clean workflow-scope skip; #377
    // owns a workflow-MODIFYING branch at finalize). A boundary abort propagates instead:
    // it cannot authorize a raw-tip fallback. When `overlay` is undefined
    // the block is skipped and `realTip` ships, byte-for-byte as before.
    let wantRev = realTip;
    if (overlay) {
      const ov = await this.buildWorkflowOverlay(barePath, branch, realTip, overlay, onStep);
      if (ov) {
        wantRev = ov;
        onStep?.("scratch_preflight");
        await this.scratchPublicationPreflight(barePath, branch, wantRev);
      }
    }

    // Validate the actual commit to be packed; an earlier overlay floor can be
    // reachable through the wrapper's first parent but not through realTip.
    const wanted = wantRev;
    await this.validateCheckpointFloor(barePath, excludeSha, wanted);
    await beforePack?.(wanted);
    onStep?.("checkpoint_pack");
    const { stdout, exited } = await this.spawnGit(
      barePath,
      ["pack-objects", "--revs", "--stdout"],
      `${wanted}\n^${excludeSha}\n`,
    );
    return { tipOid: wantRev, pack: stdout, exited };
  }

  private async validateCheckpointFloor(barePath: string, floor: string | null, candidate: string): Promise<void> {
    if (!floor || !SHA40_RE.test(floor) ||
        await this.revParse(barePath, `${floor}^{commit}`) !== floor ||
        !(await this.isAncestorRef(barePath, floor, candidate))) {
      throw new ScratchPublicationError("checkpoint floor is unavailable or not an ancestor of candidate", undefined, {
        kind: "checkpoint_range", step: "checkpoint_floor", detail: "floor is unavailable or not an ancestor of candidate",
      });
    }
  }

  /** The pack floor is the origin branch when present; otherwise the common
   *  ancestor of the pinned tip and default, so a default advance cannot exclude
   *  an unreachable commit. Shared by checkpointPack and resolveCheckpointRange. */
  private async checkpointExcludeRef(barePath: string, branch: string, tip: string): Promise<string> {
    const originRef = `refs/remotes/origin/${branch}`;
    if (await this.refExists(barePath, originRef)) return originRef;
    const defaultRef = await this.defaultBranchRef(barePath);
    // A branch behind the default still needs an ancestor exclusion. The common
    // base keeps the pack self-contained while the overlay aligns workflow trees.
    const base = (await this.runGit(barePath, ["merge-base", defaultRef, tip])).trim();
    return SHA40_RE.test(base) ? base : defaultRef;
  }

  /**
   * issue #1597 M2 — resolve the checkpoint range to PINNED SHAs: `tipSha` = the tracking ref
   * `refs/uzi-runner/<branch>`, `excludeSha` = the same floor {@link checkpointPack} excludes, each
   * via `rev-parse --verify <ref>^{commit}` and validated as 40-hex; plus the SCAN-only floors
   * ({@link CheckpointRange.scanFloorShas}): the default-branch tip, and `confirmedTip` (the last
   * checkpoint tip a publish CONFIRMED) when it is an ancestor of `tipSha`. Null when the tip or the
   * pack floor does not resolve. Never throws.
   */
  async resolveCheckpointRange(
    barePath: string,
    branch: string,
    opts: { confirmedTip?: string; extraFloors?: string[] } = {},
  ): Promise<CheckpointRange | null> {
    try {
      const tipSha = await this.trackingTip(barePath, branch);
      if (!tipSha) return null;
      const excludeRef = await this.checkpointExcludeRef(barePath, branch, tipSha);
      const excludeSha = await this.revParse(barePath, `${excludeRef}^{commit}`);
      if (!excludeSha) return null;
      const scanFloorShas: string[] = [];
      const defaultRef = await this.defaultBranchRef(barePath).catch(() => undefined);
      const defaultSha = defaultRef ? await this.revParse(barePath, `${defaultRef}^{commit}`) : null;
      if (defaultSha && defaultSha !== excludeSha) scanFloorShas.push(defaultSha);
      const confirmed = opts.confirmedTip;
      if (
        confirmed &&
        SHA40_RE.test(confirmed) &&
        confirmed !== excludeSha &&
        !scanFloorShas.includes(confirmed) &&
        (await this.revParse(barePath, `${confirmed}^{commit}`)) === confirmed &&
        (await this.isAncestor(barePath, confirmed, tipSha))
      ) {
        scanFloorShas.push(confirmed);
      }
      // issue #1932: extra scan floors — ONLY tips proven published (confirmed published real tips,
      // an attempted/confirmed checkpoint tip, the published tip); a caller must never pass a local-only
      // checkpoint floor (bridge), or content under it would go unscanned. Same admission rule as
      // `confirmedTip`: 40-hex, resolves to itself, an ancestor of the tip; a non-ancestor is
      // dropped and duplicates of the exclude floor or another floor are skipped.
      for (const floor of opts.extraFloors ?? []) {
        if (
          SHA40_RE.test(floor) &&
          floor !== excludeSha &&
          !scanFloorShas.includes(floor) &&
          (await this.revParse(barePath, `${floor}^{commit}`)) === floor &&
          (await this.isAncestor(barePath, floor, tipSha))
        ) {
          scanFloorShas.push(floor);
        }
      }
      return { tipSha, excludeSha, scanFloorShas };
    } catch (err) {
      this.log.warn("checkpoint range could not be resolved", { bare: barePath, error: gitErrorMessage(err) });
      return null;
    }
  }

  /**
   * issue #1597 M2 — secret-scan a PINNED checkpoint range before a mid-run checkpoint publish. The
   * scanned revisions are `tipSha ^excludeSha ^<scanFloorShas…>`: content already public (the default
   * branch, a previously confirmed checkpoint) is not re-scanned, so a secret-shaped fixture that is
   * already on the default branch cannot wedge publishing (and the scan reads less). The CALLER
   * treats anything but a trusted, finding-free result as "do not publish" (a checkpoint has no
   * GH013 backstop to fail open to).
   *
   * Park/shutdown/pause/capture publishes are NOT scanned by design: the api pushes them UNSCANNED
   * to the forge's refs/uzi-checkpoints/<branch> (Service.Publish in
   * api/internal/workersvc/service.go) — they are the run's last chance to be durable, and a blocked or slow scan
   * there would lose the work outright. This scan guards the frequent mid-run stream only.
   *
   * Same instrument discipline as the finalize {@link secretScanRange}: gitleaks' embedded default
   * ruleset via an explicit config, the three silencers disabled (`--ignore-gitleaks-allow`, the
   * bare has no working tree, and gitleaks runs from an EMPTY cwd so no `.gitleaksignore` is found),
   * `--redact`, the report size cap, and a liveness gate. Differences, each load-bearing (all
   * measured against gitleaks v8.30.1):
   *  1. SIZE PRE-CHECK over every blob the scanned commits touch, old side included → `scan_too_large`.
   *  2. EXPECTED COUNT. git mode only counts ("N commits scanned") commits whose `git log -p` has at
   *     least one textual hunk in a non-deleted file: a pure rename, `--allow-empty`, a mode-only
   *     chmod, a binary-only change, an empty new file and a whole-file deletion all count 0. So the
   *     expected count is the non-merge commits with a numeric, non-zero `--numstat` line under
   *     `--diff-filter=d` (same default rename detection as gitleaks' log). Known instrument limit:
   *     gitleaks skips NUL-containing (binary) files in git mode; binary assets are not blocked.
   *     Known limit: only ADDED lines are scanned, so a multi-line secret (e.g. a PEM body swapped
   *     in between existing BEGIN/END lines) is not detected when only its middle lines are added,
   *     the same as gitleaks git mode on an ordinary commit.
   *  3. MERGES. git mode neither diffs nor counts merge commits. Each merge's OWN contribution is
   *     scanned separately through `gitleaks stdin`: `git show --remerge-diff --text` for a
   *     two-parent merge (the diff from git's own re-merge of the parents to the recorded result —
   *     exactly an "evil merge"'s content, excluding side-branch commits that are scanned as
   *     ordinary commits or public via the floors); an octopus merge (remerge-diff shows nothing
   *     for it) or a git without remerge-diff falls back to `git diff --text <M>^1 <M>`, a superset.
   *     A merge whose tree equals its FIRST parent's tree (an "ours" merge, e.g. a bridge) contributes
   *     nothing beyond that parent, whose history is scanned as ordinary commits or excluded as
   *     published, so its stdin scan is skipped (counted trusted-clean, never untrusted): remerge-diff
   *     would otherwise show a dropped side edit of an already-public line as ADDED.
   *     Only the diff's ADDED lines are fed (what git mode scans), so context/'-' lines of content
   *     already public cannot wedge publishing. Liveness: gitleaks' `scanned ~N bytes` must equal
   *     the bytes fed.
   *  4. A DEADLINE shared by every child, enforced by OUR kill (exec timeout outside a scope; the
   *     tick's scan sub-scope inside one). git mode is NEVER given `--timeout`: v8.30.1 git mode on
   *     expiry exits 0, still prints "1 commits scanned" and writes `[]` — a silent partial scan.
   *     Any run that reached the deadline is `deadline` (untrusted) whatever it printed.
   * Zero expected commits and zero merges is trusted-clean. Never throws.
   */
  async secretScanCheckpointRange(
    barePath: string,
    range: CheckpointRange,
    opts: { deadlineMs?: number } = {},
  ): Promise<CheckpointScanResult> {
    const label = "checkpoint secret scan";
    const untrusted = (reason: CheckpointScanUntrustedReason, fields: Record<string, unknown> = {}): CheckpointScanResult => {
      this.log.warn(`${label}: untrusted, not publishing`, { barePath, reason, ...fields });
      return { trusted: false, findings: [], reason };
    };
    const floors = range.scanFloorShas ?? [];
    if (
      !SHA40_RE.test(range.tipSha) ||
      !SHA40_RE.test(range.excludeSha) ||
      floors.some((f) => !SHA40_RE.test(f))
    ) {
      return untrusted("range_invalid");
    }
    const deadlineAt = Date.now() + (opts.deadlineMs ?? CHECKPOINT_SCAN_TIMEOUT_MS);
    const remaining = (): number => deadlineAt - Date.now();
    const revs = [range.tipSha, `^${range.excludeSha}`, ...floors.map((f) => `^${f}`)];

    // 1. Size pre-check (bounded buffers; any failure is untrusted).
    const size = await this.checkpointRangeSize(barePath, revs, remaining).catch((err: unknown) => ({
      error: gitErrorMessage(err),
    }));
    if ("error" in size) return untrusted("count_failed", { error: size.error });
    if (size.tooLarge) return untrusted("scan_too_large", size.stats);

    // 2. What gitleaks git mode will count, and the merges it will not walk.
    let expectedCommits: number;
    let merges: string[];
    try {
      expectedCommits = await this.countTextualCommits(barePath, revs, remaining);
      const m = await this.execScoped("git", withDir(barePath, ["rev-list", "--merges", ...revs]), {
        env: gitEnv(),
        timeout: Math.max(1, remaining()),
      });
      merges = m.stdout.split("\n").map((l) => l.trim()).filter(Boolean);
    } catch (err) {
      return untrusted("count_failed", { error: gitErrorMessage(err) });
    }
    if (merges.some((sha) => !SHA40_RE.test(sha))) return untrusted("count_failed");
    if (merges.length > CHECKPOINT_SCAN_MAX_MERGES) return untrusted("too_many_merges", { merges: merges.length });
    if (expectedCommits === 0 && merges.length === 0) return { trusted: true, findings: [] };

    const scratch = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-gl-ckpt-"));
    try {
      const configPath = path.join(scratch, "config.toml");
      await fs.writeFile(configPath, "[extend]\nuseDefault = true\n", "utf8");
      // An EMPTY cwd: gitleaks' default `--gitleaks-ignore-path .` then finds no .gitleaksignore.
      const cwd = await fs.mkdtemp(path.join(scratch, "cwd-"));
      const findings: SecretFinding[] = [];

      if (expectedCommits > 0) {
        const reportPath = path.join(scratch, "git.json");
        const run = await this.runCheckpointGitleaks(
          gitleaksArgs({ sourcePath: barePath, logRange: revs.join(" "), configPath, reportPath }),
          { cwd, reportPath, remaining },
        );
        if (run.kind === "deadline") return untrusted("deadline", { mode: "git" });
        if (run.kind === "unreadable") return untrusted("scan_untrusted", { mode: "git", why: run.why });
        const scannedCommits = commitsScannedFromStderr(run.stderr);
        findings.push(...run.findings);
        if (!scanIsTrustworthy({ stderr: run.stderr, scannedCommits, expectedCommits, execOk: run.execOk }) || /\bskipp/i.test(run.stderr)) {
          if (findings.length > 0) return { trusted: false, findings, reason: "scan_untrusted" };
          return untrusted("scan_untrusted", { mode: "git", expectedCommits, scannedCommits, execOk: run.execOk });
        }
      }

      for (const merge of merges) {
        const res = await this.scanMergeContribution(barePath, merge, { configPath, cwd, scratch, remaining });
        if (res.kind === "untrusted") {
          if (findings.length > 0) return { trusted: false, findings, reason: res.reason };
          return untrusted(res.reason, { merge });
        }
        findings.push(...res.findings);
      }
      this.log.debug(`${label} complete`, { barePath, expectedCommits, merges: merges.length, findings: findings.length });
      return { trusted: true, findings };
    } catch (err) {
      return untrusted("scan_untrusted", { error: gitErrorMessage(err) });
    } finally {
      await fs.rm(scratch, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /** issue #1597 M2 — the number of non-merge commits in `revs` that gitleaks git mode will COUNT:
   *  those with a numeric, non-zero `--numstat` line under `--diff-filter=d` (see
   *  secretScanCheckpointRange item 2 for the measured rule). Bounded output. */
  private async countTextualCommits(barePath: string, revs: string[], remaining: () => number): Promise<number> {
    const out = (
      await this.execScoped(
        "git",
        withDir(barePath, ["log", "--no-merges", "--format=%x00%H", "--numstat", "--diff-filter=d", ...revs]),
        { env: gitEnv(), timeout: Math.max(1, remaining()), maxBuffer: GIT_MAX_BUFFER },
      )
    ).stdout;
    let count = 0;
    for (const block of out.split("\0").slice(1)) {
      const lines = block.split("\n").slice(1);
      const textual = lines.some((l) => {
        const m = /^(\d+)\t(\d+)\t/.exec(l);
        return m !== null && Number(m[1]) + Number(m[2]) > 0;
      });
      if (textual) count++;
    }
    return count;
  }

  /** issue #1597 M2 — the blob bytes gitleaks will read for `revs`, against the CHECKPOINT_SCAN_MAX_*
   *  caps: BOTH sides of every blob the non-merge commits touch (`log --raw --no-renames`, so a
   *  deletion or a modification charges the old blob too), sized with `cat-file --batch-check`.
   *  Merges are bounded separately (their own diff has a byte cap). Bounded listings. */
  private async checkpointRangeSize(
    barePath: string,
    revs: string[],
    remaining: () => number,
  ): Promise<{ tooLarge: boolean; stats: Record<string, unknown> }> {
    let raw: string;
    try {
      raw = (
        await this.execScoped(
          "git",
          withDir(barePath, ["log", "--no-merges", "--raw", "--no-abbrev", "--no-renames", "--format=", ...revs]),
          { env: gitEnv(), timeout: Math.max(1, remaining()), maxBuffer: GIT_MAX_BUFFER },
        )
      ).stdout;
    } catch (err) {
      if (isOutputOverflow(err)) return { tooLarge: true, stats: { why: "raw_listing_over_cap" } };
      throw err;
    }
    const oids = new Set<string>();
    for (const line of raw.split("\n")) {
      // `:<oldmode> <newmode> <oldoid> <newoid> <status>\t<path>`
      const m = /^:\d+ \d+ ([0-9a-f]{40}) ([0-9a-f]{40}) /.exec(line);
      if (!m) continue;
      for (const oid of [m[1]!, m[2]!]) if (!/^0{40}$/.test(oid)) oids.add(oid);
      if (oids.size > CHECKPOINT_SCAN_MAX_OBJECTS) return { tooLarge: true, stats: { blobs: oids.size } };
    }
    if (oids.size === 0) return { tooLarge: false, stats: { blobs: 0 } };
    const checked = await this.execScoped(
      "git",
      withDir(barePath, ["cat-file", "--batch-check=%(objecttype) %(objectsize)"]),
      { env: gitEnv(), timeout: Math.max(1, remaining()), maxBuffer: oids.size * 32 + 1024, input: `${[...oids].join("\n")}\n` },
    );
    let total = 0;
    let largest = 0;
    for (const line of checked.stdout.split("\n")) {
      const [type, sz] = line.trim().split(" ");
      if (type !== "blob") continue; // a gitlink (submodule) oid is "missing": nothing to read
      const n = Number.parseInt(sz ?? "", 10);
      if (!Number.isFinite(n)) throw new Error("unparseable cat-file size");
      total += n;
      if (n > largest) largest = n;
    }
    const stats = { blobs: oids.size, blob_bytes: total, largest_blob: largest };
    return {
      tooLarge: largest > CHECKPOINT_SCAN_MAX_BLOB_BYTES || total > CHECKPOINT_SCAN_MAX_TOTAL_BYTES,
      stats,
    };
  }

  /** issue #1597 M2 — does this git have `--remerge-diff` (git ≥ 2.36)? Only a COMPLETED probe is
   *  cached: a rejected one (a tick's scan scope aborted or hit its deadline, a spawn error) answers
   *  false for that call alone and clears the cache so a later call probes again. Caching the
   *  rejection would pin every later two-parent merge scan on this long-lived cache to the
   *  `diff M^1 M` fallback, which re-adds main's changes and blocks checkpoints on main's fixtures. */
  private remergeDiffSupported(): Promise<boolean> {
    const cached = this.remergeProbe;
    if (cached) return cached;
    const probe: Promise<boolean> = this.execScoped("git", ["version"], { env: gitEnv(), timeout: 10_000 }).then(
      ({ stdout }) => {
        const m = /git version (\d+)\.(\d+)/.exec(stdout);
        return m !== null && (Number(m[1]) > 2 || (Number(m[1]) === 2 && Number(m[2]) >= 36));
      },
      () => {
        // Clear only our own entry: a later call may already have started a fresh probe.
        if (this.remergeProbe === probe) this.remergeProbe = undefined;
        return false;
      },
    );
    this.remergeProbe = probe;
    return probe;
  }

  /** issue #1597 M2 — scan ONE merge commit's own contribution through `gitleaks stdin` (see
   *  secretScanCheckpointRange item 3 for which diff and why). `--text` so binary-looking content is
   *  still shown; no external diff / textconv drivers; byte-capped. An empty own-contribution is
   *  trusted without running the scanner. Liveness: a clean exit, no error token, and gitleaks'
   *  `scanned ~N bytes` equal to the bytes fed. */
  private async scanMergeContribution(
    barePath: string,
    merge: string,
    ctx: { configPath: string; cwd: string; scratch: string; remaining: () => number },
  ): Promise<{ kind: "ok"; findings: SecretFinding[] } | { kind: "untrusted"; reason: CheckpointScanUntrustedReason }> {
    if (ctx.remaining() < 1_000) return { kind: "untrusted", reason: "deadline" };
    let diff: string;
    try {
      const parents = (
        await this.execScoped("git", withDir(barePath, ["rev-list", "--parents", "-n", "1", merge]), {
          env: gitEnv(),
          timeout: Math.max(1, ctx.remaining()),
        })
      ).stdout.trim().split(/\s+/).length - 1;
      // An "ours" merge (tree identical to its first parent's) adds nothing beyond that parent, whose
      // history is scanned as ordinary commits or excluded as published. Its remerge-diff would show
      // the side parent's dropped edits inverted, i.e. content already public as ADDED.
      const trees = (
        await this.execScoped("git", withDir(barePath, ["rev-parse", `${merge}^{tree}`, `${merge}^1^{tree}`]), {
          env: gitEnv(),
          timeout: Math.max(1, ctx.remaining()),
        })
      ).stdout.trim().split(/\s+/);
      if (trees.length === 2 && SHA40_RE.test(trees[0]!) && trees[0] === trees[1]) return { kind: "ok", findings: [] };
      const common = ["--no-color", "--no-ext-diff", "--no-textconv", "--text"];
      const args = parents === 2 && (await this.remergeDiffSupported())
        ? ["show", "--remerge-diff", "--format=", ...common, merge]
        : ["diff", ...common, `${merge}^1`, merge];
      diff = (
        await this.execScoped("git", withDir(barePath, args), {
          env: gitEnv(),
          timeout: Math.max(1, ctx.remaining()),
          maxBuffer: CHECKPOINT_SCAN_MAX_MERGE_DIFF_BYTES,
        })
      ).stdout;
    } catch (err) {
      return { kind: "untrusted", reason: isOutputOverflow(err) ? "merge_diff_too_large" : "count_failed" };
    }
    // Feed gitleaks ONLY the lines the merge ADDS (what git mode scans for an ordinary commit):
    // context and '-' lines would re-flag content already public — e.g. a conflict resolved to the
    // default branch's side shows main's fixture line as context/removal and would wedge publishing
    // forever. File headers are skipped by tracking hunks: a '+' line counts only inside a hunk.
    const added = addedLinesOfDiff(diff);
    if (added.length === 0) return { kind: "ok", findings: [] };
    const reportPath = path.join(ctx.scratch, `merge-${merge}.json`);
    const run = await this.runCheckpointGitleaks(
      gitleaksStdinArgs({ configPath: ctx.configPath, reportPath }),
      { cwd: ctx.cwd, reportPath, remaining: ctx.remaining, input: added, selfTimeout: true },
    );
    if (run.kind === "deadline") return { kind: "untrusted", reason: "deadline" };
    if (run.kind === "unreadable") return { kind: "untrusted", reason: "scan_untrusted" };
    const scanned = /\bscanned ~(\d+) bytes/i.exec(run.stderr);
    const live =
      run.execOk &&
      !/\b(err|error|fatal)\b/i.test(run.stderr) &&
      scanned !== null &&
      Number(scanned[1]) === Buffer.byteLength(added) &&
      !/\bskipp/i.test(run.stderr);
    const findings = run.findings.map((f) => ({ ...f, commit: merge }));
    if (!live && findings.length === 0) return { kind: "untrusted", reason: "scan_untrusted" };
    return { kind: "ok", findings };
  }

  /** issue #1597 M2 — one gitleaks invocation for the checkpoint scan, under OUR hard deadline: the
   *  child is killed at the shared deadline (execFile's timeout outside a boundary scope; inside the
   *  tick's scan sub-scope, that scope's abort — execScoped's scoped branch ignores `timeout`), and
   *  any run that lasted to within {@link GITLEAKS_DEADLINE_MARGIN_MS} of the deadline is `deadline`
   *  whatever it exited with or wrote. `selfTimeout` additionally passes gitleaks' own `--timeout`
   *  (stdin mode only: git mode's `--timeout` produces a silent, clean-looking partial scan). The
   *  report is size-capped. */
  private async runCheckpointGitleaks(
    args: string[],
    ctx: { cwd: string; reportPath: string; remaining: () => number; input?: string; selfTimeout?: boolean },
  ): Promise<
    | { kind: "ran"; execOk: boolean; stderr: string; findings: SecretFinding[] }
    | { kind: "deadline" }
    | { kind: "unreadable"; why: string }
  > {
    const left = ctx.remaining();
    if (left < 1_000) return { kind: "deadline" };
    const fullArgs = ctx.selfTimeout ? [...args, "--timeout", String(Math.max(1, Math.floor(left / 1_000)))] : args;
    const startedAt = Date.now();
    let execOk = true;
    let stderr = "";
    try {
      const res = await this.execScoped(this.gitleaksBin, fullArgs, {
        env: gitEnv(),
        cwd: ctx.cwd,
        maxBuffer: GIT_MAX_BUFFER,
        timeout: left,
        ...(ctx.input !== undefined ? { input: ctx.input } : {}),
      });
      stderr = res.stderr ?? "";
    } catch (err) {
      execOk = false;
      stderr = typeof (err as { stderr?: unknown }).stderr === "string"
        ? (err as { stderr: string }).stderr
        : gitErrorMessage(err);
    }
    if (Date.now() - startedAt >= left - GITLEAKS_DEADLINE_MARGIN_MS) return { kind: "deadline" };
    // gitleaks colours its log lines even when stderr is a pipe (`\x1b[31mERR\x1b[0m`,
    // `\x1b[1mscanned …`), which defeats a word-boundary token match: strip ANSI SGR sequences
    // before any liveness check reads the text (scanIsTrustworthy also strips its own input).
    stderr = stripAnsiSgr(stderr);
    try {
      const st = await fs.stat(ctx.reportPath);
      if (st.size > SECRET_SCAN_REPORT_MAX_BYTES) return { kind: "unreadable", why: "report_over_cap" };
      const raw = await fs.readFile(ctx.reportPath, "utf8");
      // A malformed report parses to [] ("clean"); it is an unreadable scan, never a clean one.
      if (!gitleaksReportWellFormed(raw)) return { kind: "unreadable", why: "report_malformed" };
      return { kind: "ran", execOk, stderr, findings: parseGitleaksReport(raw) };
    } catch {
      return { kind: "unreadable", why: execOk ? "report_unreadable" : "exec_failed" };
    }
  }

  /**
   * PRD #1062 M2 (#1036) — build the `.github/workflows` overlay wrapper commit `O_ov` for a
   * checkpoint, or return null to ship the raw `realTip` (today's behaviour). ALL work is
   * FAIL-SOFT for ordinary overlay errors and gate misses: they return null and ship `realTip`.
   * A boundary abort propagates and never turns into a raw-tip publish.
   *
   * The overlay is a transport wrapper the broker pushes UNCHANGED and adoption peels
   * (`runnerCloneForBranch`, `isOverlayMarker`): its `.github/workflows` subtree is swapped to
   * the default's so GitHub's tip-vs-default workflow-scope check passes, while the real branch
   * content lives on its LAST parent (`realTip`). The base (a prior overlay tip) goes FIRST and
   * `realTip` LAST so the api broker's parent[0]-first strict-descendant DFS over its depth-1
   * store accepts it (see the PRD's broker note — the broker stays byte-unchanged).
   *
   * Synthesis is pure object-DB work via a TEMP INDEX (no live worktree, no filter drivers, no
   * runner-clone perturbation): `read-tree` the real tip, `rm --cached` the workflow set,
   * `read-tree --prefix` the default's subtree back (only when the default HAS one), `write-tree`,
   * then `commit-tree` with a DETERMINISTIC identity + committer date so a no-new-work rebuild
   * yields the same OID. Runs worker-uid on the base git env with NO PAT (the local reads), the
   * PAT is used only by `fetchDefaultTip`.
   */
  private async buildWorkflowOverlay(
    barePath: string,
    branch: string,
    realTip: string,
    overlay: CheckpointOverlayContext,
    onStep?: (step: BoundaryStep) => void,
  ): Promise<string | null> {
    // GATE 1 — the default tip must resolve (a fresh authenticated fetch). An ordinary
    // failure ships realTip; a boundary abort propagates instead of authorizing fallback.
    let defaultTip: string;
    try {
      onStep?.("checkpoint_lock_wait");
      defaultTip = await this.fetchDefaultTip(
        barePath,
        overlay.defaultBranch,
        overlay.pat,
        overlay.cloneUrl,
        overlay.username,
        () => onStep?.("default_fetch"),
      );
    } catch (e) {
      // issue #2213: a quarantine refusal is not "could not resolve": the checkpoint's publish is
      // skipped (the runner's RunResidueBlockedError arm), never shipped on a fallback.
      if (e instanceof RunResidueBlockedError) throw e;
      const abort = this.boundaryAbortError(e);
      if (abort) throw abort;
      this.log.warn("checkpoint overlay: could not resolve the default tip — shipping realTip", {
        branch,
        error: gitErrorMessage(e),
      });
      return null;
    }
    onStep?.("checkpoint_overlay");
    // GATE 2 — only a branch actually behind on `.github/workflows` needs an overlay (reuse
    // #627's exact trigger). Not behind ⇒ ship realTip (the broker accepts the raw tip).
    if (!(await this.workflowTreeDiffers(barePath, realTip, defaultTip))) return null;
    // GATE 3 — the branch must NOT itself have modified a workflow file. `changedFiles` returns
    // null when the diff can't be computed: FAIL-SAFE (cannot verify ⇒ ship realTip, never
    // synthesise a tree that might hide a branch's own workflow edit). A non-empty workflow hit
    // set means #377 owns this branch at finalize; ship realTip → the clean workflow-scope skip.
    const changed = await this.changedFiles(barePath, realTip);
    if (changed === null) return null;
    if (changed.some((file) => file.startsWith(".github/workflows/"))) return null;

    // Temp index + empty throwaway work-tree, both cleaned up in the finally. The work-tree is
    // required ONLY by `read-tree --prefix` (it refuses in a bare repo); WITHOUT `-u` nothing is
    // checked out into it, so no `.gitattributes` filter/smudge driver ever fires — a security
    // property (do NOT add `-u`).
    const tmpIdx = path.join(os.tmpdir(), `uzi-ckpt-idx-${randomUUID()}`);
    const tmpWork = path.join(os.tmpdir(), `uzi-ckpt-work-${randomUUID()}`);
    try {
      await fs.mkdir(tmpWork, { recursive: true });
      const idxEnv: NodeJS.ProcessEnv = { GIT_INDEX_FILE: tmpIdx };
      await this.runGitWithEnv(barePath, ["read-tree", realTip], idxEnv);
      // `--ignore-unmatch` is load-bearing: against a temp index a `git rm --cached` with no
      // match exits non-zero, so this flag makes the no-`.github`/already-deleted edges no-ops
      // (the same flag `alignBranchWithDefault` carries).
      await this.runGitWithEnv(
        barePath,
        ["rm", "--cached", "-r", "--ignore-unmatch", ".github/workflows"],
        idxEnv,
      );
      // Restore the default's workflow subtree ONLY when the default actually has one; an empty
      // ls-tree means the default deleted its whole `.github/workflows/`, and the rm above
      // already equalised to "none" (the #627 deleted-workflows edge).
      const defaultWfTree = (
        await this.runGit(barePath, ["ls-tree", defaultTip, "--", ".github/workflows"])
      ).trim();
      if (defaultWfTree.length > 0) {
        await this.runGitWithEnv(
          barePath,
          ["read-tree", "--prefix=.github/workflows", `${defaultTip}:.github/workflows`],
          { ...idxEnv, GIT_WORK_TREE: tmpWork },
        );
      }
      const newRoot = (await this.runGitWithEnv(barePath, ["write-tree"], idxEnv)).trim();
      if (!/^[0-9a-f]{40}$/.test(newRoot)) return null;
      // If the synthesised root equals realTip's own tree the branch is not actually behind
      // (e.g. realTip has no `.github` and the default has none) ⇒ ship realTip.
      const realTipTree = (await this.runGit(barePath, ["rev-parse", `${realTip}^{tree}`])).trim();
      if (newRoot === realTipTree) return null;
      // Deterministic committer date = realTip's own, so a no-new-work rebuild is byte-identical.
      const committerDate = (
        await this.runGit(barePath, ["show", "-s", "--format=%cI", realTip])
      ).trim();
      // Parent order: base (a prior overlay tip) FIRST, realTip LAST — see the method doc and
      // the PRD's broker note. A missing / non-40-hex prevCheckpointTip ⇒ single parent realTip.
      const parents: string[] = [];
      const prev = overlay.prevCheckpointTip;
      if (prev && /^[0-9a-f]{40}$/.test(prev)) parents.push(prev);
      parents.push(realTip);
      const commitArgs = ["-c", "commit.gpgsign=false", "commit-tree", newRoot];
      for (const p of parents) commitArgs.push("-p", p);
      commitArgs.push(
        "-m",
        `${OVERLAY_COMMIT_PREFIX} align .github/workflows with ${defaultTip} (realTip ${realTip.slice(0, 12)})`,
      );
      const ovSha = (
        await this.runGitWithEnv(barePath, commitArgs, {
          ...idxEnv,
          GIT_AUTHOR_NAME: AGENT_GIT_IDENTITY.name,
          GIT_AUTHOR_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_COMMITTER_NAME: AGENT_GIT_IDENTITY.name,
          GIT_COMMITTER_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_AUTHOR_DATE: committerDate,
          GIT_COMMITTER_DATE: committerDate,
        })
      ).trim();
      if (!/^[0-9a-f]{40}$/.test(ovSha)) return null;
      this.log.info("checkpoint overlay built (.github/workflows aligned to default)", {
        branch,
        real_tip: realTip,
        default_tip: defaultTip,
        overlay_sha: ovSha,
        parents: parents.length,
      });
      return ovSha;
    } catch (e) {
      const abort = this.boundaryAbortError(e);
      if (abort) throw abort;
      this.log.warn("checkpoint overlay synthesis failed — shipping realTip", {
        branch,
        error: gitErrorMessage(e),
      });
      return null;
    } finally {
      await fs.rm(tmpIdx, { force: true }).catch(() => undefined);
      await fs.rm(tmpWork, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /** Remove the run's runner clone (a standalone clone, not a linked worktree — no
   *  bare interaction). The warm bare and the fetched refs/objects are kept. */
  async removeRunnerClone(clonePath: string, ownerRunId?: string): Promise<void> {
    const refuseUnderQuarantine = (): void => {
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
    };
    if (ownerRunId !== undefined) {
      if (await this.hasPhysicalTerminalProtection(ownerRunId)) {
        throw new Error("terminal record custody retains runner clone");
      }
      // issue #2213: a latch that landed during the protection read keeps the clone.
      refuseUnderQuarantine();
    }
    // The pinned teardown awaits several opens and stats before it deletes anything, so a
    // latch landing in that window is rechecked synchronously at each destructive dispatch.
    await rmRunnerTeardownTree(clonePath, {
      allowCloneName: true,
      ...(ownerRunId !== undefined ? { beforeDestroy: refuseUnderQuarantine } : {}),
    });
  }

  /**
   * issue #1315 — ATOMICALLY RELEASE a runner clone the recovery journal points at.
   *
   * The bug this replaces: terminal cleanup did `removeRunnerClone` (a recursive
   * `fs.rm`) then `clearRecoveryCapture` in one try/catch. On Linux, a detached `git
   * maintenance`/`fsmonitor` daemon still holding a file open makes that recursive rm
   * race and throw ENOTEMPTY (`force: true` suppresses ENOENT, not ENOTEMPTY — see the
   * comment above the clone seed), so the journal-clear never ran, the journal survived
   * pointing at partial residue, and a later foreign run wedged forever on the guard.
   *
   * The fix rests on one OS fact: `rename(2)` of a directory whose files a daemon still
   * holds open SUCCEEDS (the open fds keep the old inode alive), where recursive delete
   * races. So we RENAME the canonical clone to a worker-only holding location on the
   * SAME filesystem — a race-free release — and only THEN clear the journal. A crash
   * between the two can only leave a cleared-at-canonical / journal-still-set state that
   * a re-run resolves; it can never leave the journal pointing at residue at canonical.
   *
   * Fail-closed in every disagreement: the move only ever touches a path that EXACTLY
   * equals the journaled `clonePath` (validated before the rename) AND resolves under
   * runnerRoot. `opts.discard` distinguishes the owner's own terminal trash (dispose the
   * holding dir, best-effort) from a foreign quarantine (retain it forever).
   *
   * NOT re-entrant with markRecoveryCapture (which takes its OWN withLock) — this runs
   * the whole validate→rename→clear sequence under one lock via readRecoveryCapture
   * (lock-free) + a direct runGit config write.
   */
  /** Credential-free exact prior retirement proof. Absence alone is never attribution. */
  async completionSourceAlreadyRetired(barePath: string, branch: string, runId: string,
    clonePath: string, attemptId: string | undefined): Promise<boolean> {
    if (!attemptId) return false;
    return this.withLock(barePath, async () => {
      const entry = (await this.readAttemptLedger(barePath, branch)).get(attemptId);
      if (entry?.runId !== runId || entry.clonePath !== clonePath || entry.state !== "retired") return false;
      const open = async (): Promise<boolean> =>
        !await this.hasPhysicalTerminalProtection(runId) && residueQuarantine() === undefined;
      if (!await open()) return false;
      const pending = await this.readRecoveryCapture(barePath, branch);
      if (pending && (pending.runId !== runId || pending.clonePath !== clonePath || pending.attemptId !== attemptId))
        return false; // A successor/adoption owns this branch now.
      if (pending?.recovery || pending?.retainedSources?.length) return false;
      try { await fs.lstat(clonePath); return false; }
      catch (err) { if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err; }
      if (!await open()) return false;
      // Durable retirement already proves disposal; only finish this exact old journal.
      if (pending) await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
      return true;
    });
  }

  async retireRunnerClone(
    barePath: string,
    clonePath: string,
    branch: string,
    ownerRunId: string,
    opts: { discard: boolean; attemptId?: string; orphanDiagnostics?: OrphanDiagnostics;
      verifiedSuccessor?: { key: string; generation: number; expected: RecoveryJournalEntry;
        acknowledged: () => Promise<boolean> };
      completionSource?: { expectedHead: string; canDelete: () => boolean } },
  ): Promise<RunnerCloneRetireResult> {
    if (opts.completionSource && opts.verifiedSuccessor) throw new Error("retirement authorities are mutually exclusive");
    // Completion retains adoption exclusion through holding/scratch disposal as well.
    if (opts.completionSource) return this.withLock(barePath,
      () => this.retireRunnerCloneImpl(barePath, clonePath, branch, ownerRunId, opts, true));
    return this.retireRunnerCloneImpl(barePath, clonePath, branch, ownerRunId, opts);
  }

  private async retireRunnerCloneImpl(
    barePath: string,
    clonePath: string,
    branch: string,
    ownerRunId: string,
    opts: { discard: boolean; attemptId?: string; orphanDiagnostics?: OrphanDiagnostics;
      verifiedSuccessor?: { key: string; generation: number; expected: RecoveryJournalEntry;
        acknowledged: () => Promise<boolean> };
      completionSource?: { expectedHead: string; canDelete: () => boolean } },
    bareLockHeld = false,
  ): Promise<RunnerCloneRetireResult> {
    let stage: OrphanRetireStage = "journal_read";
    const retire = async (): Promise<{ holding?: string; scratch?: string; renamed: boolean; disposition: RunnerCloneRetireResult }> => {
      // 1. Pre-rename pair validation. Require the EXACT (ownerRunId, clonePath) pair
      //    to STILL be journaled before moving anything. A missing/malformed journal, or
      //    a lock-gap rewrite to a different runId/path, moves NOTHING and fails closed.
      const pending = await this.readRecoveryCapture(barePath, branch);
      stage = "journal_validation";
      if (pending?.runId !== ownerRunId || pending.clonePath !== clonePath) {
        throw new CapturePathMismatchError(pending?.clonePath ?? "", clonePath, branch, ownerRunId);
      }
      const verified = opts.verifiedSuccessor;
      let anchored: RecoveryJournalEntry | undefined;
      if (verified) {
        if (!opts.discard || !opts.attemptId || pending.attemptId !== opts.attemptId ||
            !Number.isSafeInteger(verified.generation) || verified.generation <= 0 ||
            typeof verified.acknowledged !== "function" ||
            !ATTEMPT_ID_RE.test(opts.attemptId) || !opts.attemptId.includes(`-g${verified.generation}-`) || pending.recovery ||
            !pending.retainedSources?.length || JSON.stringify(pending) !== JSON.stringify(verified.expected)) {
          throw new Error("completed successor identity or predecessor set changed");
        }
        await this.checkedRecovery(barePath, branch, verified.key, pending);
        const predecessors = pending.retainedSources.filter(s => !sameRecoverySource(s, pending));
        if (!predecessors.length || predecessors.length !== pending.retainedSources.length) {
          throw new Error("invalid successor predecessor set");
        }
        anchored = { ...predecessors[0]!, retainedSources: predecessors.length === 1 ? predecessors : predecessors.slice(1) };
      } else if (pending.recovery || pending.retainedSources?.length) {
        throw new Error("recovery sources require explicit verified disposition before retirement");
      }
      // Each check reads authority once through the trusted runner callback, no retries.
      // A failed check blocks this retirement alone and never releases predecessor evidence.
      const checkSuccessor = async (expected: RecoveryJournalEntry) => {
        if (!verified) return;
        if (await this.hasPhysicalTerminalProtection(ownerRunId)) {
          throw new Error("verified successor FINAL authority unavailable");
        }
        const ledger = (await this.readAttemptLedger(barePath, branch)).get(opts.attemptId!);
        if (ledger?.runId !== ownerRunId || ledger.clonePath !== clonePath || ledger.state !== "live") {
          throw new Error("successor ledger identity changed");
        }
        const current = await this.checkedRecovery(barePath, branch, verified.key, expected);
        if (JSON.stringify(current) !== JSON.stringify(expected)) throw new Error("successor predecessor set changed");
        if (await verified.acknowledged() !== true) throw new Error("verified successor FINAL authority unavailable");
        // The authority callback awaits physical journals; revalidate mutable identity after it.
        const afterLedger = (await this.readAttemptLedger(barePath, branch)).get(opts.attemptId!);
        if (afterLedger?.runId !== ownerRunId || afterLedger.clonePath !== clonePath || afterLedger.state !== "live")
          throw new Error("successor ledger identity changed");
        const afterJournal = await this.checkedRecovery(barePath, branch, verified.key, expected);
        if (JSON.stringify(afterJournal) !== JSON.stringify(expected)) throw new Error("successor predecessor set changed");
        if (await this.hasPhysicalTerminalProtection(ownerRunId)) throw new Error("verified successor FINAL authority unavailable");
        if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      };
      if (opts.completionSource) {
        // Exact ownership and expected HEAD are checked while adoption shares this bare lock.
        const parsed = parseAttemptPath(clonePath, path.resolve(this.runnerRoot));
        const key = parsed?.key ?? path.basename(clonePath);
        if (pending.attemptId !== opts.attemptId ||
            !await this.classifyOwnerClonePath(barePath, branch, key, ownerRunId, clonePath) ||
            await this.worktreeHead(clonePath) !== opts.completionSource.expectedHead ||
            !opts.completionSource.canDelete()) throw new Error("completion source attribution changed");
      }
      // 2. Containment. Only ever move a path strictly UNDER runnerRoot. Resolve both
      //    sides and require a path-separator boundary so a sibling like
      //    `<runnerRoot>-evil` cannot satisfy a bare prefix test. A path outside
      //    runnerRoot (whatever the journal claims) fails closed and moves nothing.
      stage = "containment";
      const resolvedClone = path.resolve(clonePath);
      const resolvedRoot = path.resolve(this.runnerRoot);
      if (!resolvedClone.startsWith(resolvedRoot + path.sep)) {
        throw new CapturePathMismatchError(clonePath, this.runnerRoot, branch, ownerRunId);
      }
      if (opts.discard && await this.hasPhysicalTerminalProtection(ownerRunId)) {
        throw new Error("terminal record custody retains runner clone");
      }
      // 3. Worker-only 0700 holding destination: a SIBLING of runnerRoot under the same
      //    dataDir PATH — but NOT necessarily the same device: on a docker-lane worker
      //    runnerRoot is an emptyDir and this quarantine is on the /data PVC, so the step-4
      //    rename CAN hit EXDEV (issue #1354); the EXDEV branch below is that fallback. NOT
      //    under the runner-writable tree. Sanitized, generated components. Create-or-assert
      //    the parent 0700 BEFORE the rename, so a rename ENOENT below unambiguously means
      //    the SOURCE is missing.
      const holdingDest = path.join(
        this.runnerHoldingRoot,
        `${ownerRunId.replace(/[^A-Za-z0-9_-]/g, "_")}-${randomUUID()}`,
      );
      stage = "holding_parent";
      await fs.mkdir(path.dirname(holdingDest), { recursive: true, mode: 0o700 });
      // 4. Atomic move + ENOENT disambiguation, with an EXDEV fallback (issue #1354).
      let renamed = true;
      let holding: string | undefined;
      let scratch: string | undefined;
      let exdev = false;
      stage = "rename";
      // issue #2213: a latch that landed during the awaits above keeps the clone; nothing moved.
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
      if (opts.completionSource && !opts.completionSource.canDelete()) throw new Error("completion cleanup exclusion changed");
      if (anchored) {
        await checkSuccessor(pending);
        await this.writeRecovery(barePath, branch, anchored, () => checkSuccessor(pending));
        // Git config publishes atomically; sync both the file and its directory before
        // moving the successor so a crash always leaves a discoverable predecessor.
        for (const target of [path.join(barePath, "config"), barePath]) {
          const handle = await fs.open(target, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
          try { await handle.sync(); } finally { await handle.close(); }
        }
        await checkSuccessor(anchored);
      }
      try {
        await fs.rename(clonePath, holdingDest);
      } catch (err) {
        const e = err as NodeJS.ErrnoException;
        if (e.code === "EXDEV") {
          // issue #1354 — runnerRoot and the quarantine share a dataDir PATH but are on
          // DIFFERENT devices on a docker-lane worker (emptyDir vs PVC), so the rename to
          // holdingDest is a cross-device move that cannot happen. Free the canonical with
          // an intra-device atomic rename into a scratch parent under runnerRoot (SAME
          // device — never EXDEV, and it survives a daemon file-hold where a recursive rm
          // races). The #1315 invariant is preserved: the journaled canonical is freed ONLY
          // by an atomic rename, never a direct recursive rm of partial residue.
          exdev = true;
          if (opts.discard) {
            // Hot path (G1 rename-first): the owner's own terminal trash. Free the
            // canonical and copy NOTHING — an fs.cp here would throw on a git fsmonitor
            // socket / FIFO the clone may carry. No holdingDest is used on this path.
            stage = "intra_device_rename";
            const scratchParent = await this.createRetireScratchParent();
            if (opts.completionSource && !opts.completionSource.canDelete()) throw new Error("completion cleanup exclusion changed");
            renamed = await this.renameIntoScratchUnlessLatched(clonePath, scratchParent, undefined,
              anchored ? () => checkSuccessor(anchored!) : undefined);
            scratch = scratchParent;
          } else {
            // Rare foreign-orphan reclaim: the quarantine is RETAINED FOREVER, so
            // cross-crash retention is a hard requirement → copy-before-free. COMPLETE the
            // symlink-safe copy into holdingDest FIRST, while the canonical AND the journal
            // still protect a retry.
            stage = "retained_copy";
            try {
              await fs.cp(clonePath, holdingDest, {
                recursive: true,
                dereference: false,
                verbatimSymlinks: true,
                // dereference:false + verbatimSymlinks:true copy symlinks AS links. The
                // filter skips sockets/FIFOs: fs.cp throws ERR_FS_CP_SOCKET /
                // ERR_FS_CP_FIFO_PIPE on them, and the git fsmonitor daemon plants a UNIX
                // socket in .git/.
                filter: async (src) => {
                  const s = await fs.lstat(src);
                  return !s.isSocket() && !s.isFIFO();
                },
              });
            } catch (copyErr) {
              // Remove ONLY the incomplete copy; the canonical + journal stay intact so a
              // retry can re-copy. No scratch parent exists yet (created below), so this
              // cleanup is scoped strictly to the copy.
              await fs.rm(holdingDest, { recursive: true, force: true }).catch(() => undefined);
              throw copyErr;
            }
            // The completed off-tree copy is recorded. NOW free the canonical with the
            // intra-device atomic rename. A REAL rename failure here must RETAIN the
            // completed holdingDest AND leave the canonical + journal intact — so this
            // rename is deliberately OUTSIDE the copy's cleanup catch above.
            stage = "intra_device_rename";
            const scratchParent = await this.createRetireScratchParent();
            renamed = await this.renameIntoScratchUnlessLatched(clonePath, scratchParent, holdingDest);
            holding = holdingDest; // retained; step 6 no-ops since discard === false
            scratch = scratchParent;
          }
        } else if (e.code === "ENOENT") {
          // The destination parent was asserted in step 3, so a rename ENOENT is about the
          // SOURCE. Confirm: if the source is gone, the canonical is already free — proceed
          // to clear the journal. If the source STILL exists, the ENOENT is a real,
          // unexpected failure (e.g. a destination parent that vanished under us) and must
          // surface with the journal left intact.
          const sourceGone = await fs.lstat(clonePath).then(
            () => false,
            (le: NodeJS.ErrnoException) => {
              if (le.code === "ENOENT") return true;
              throw le;
            },
          );
          if (!sourceGone) throw err;
          renamed = false;
        } else {
          throw err;
        }
      }
      if (!exdev) {
        // Non-EXDEV path (rename success OR a confirmed source-already-free): unchanged
        // behavior — holdingDest is the retired residue iff we renamed it, no scratch.
        holding = renamed ? holdingDest : undefined;
      }
      // 5. Journal clear — ONLY after a confirmed rename or a confirmed source-already-
      //    free. Re-read and clear only if it STILL matches (ownerRunId, clonePath); a
      //    concurrent successor must never have its journal cleared by us.
      stage = "journal_clear";
      const still = await this.readRecoveryCapture(barePath, branch);
      if (residueQuarantine() !== undefined || (opts.completionSource && !opts.completionSource.canDelete())) {
        // issue #2213: a latch landed after the move. Undo it (best-effort), keep the journal and
        // dispose of nothing; the latched worker keeps the clone at its journaled path.
        const movedTo = renamed ? (exdev ? (scratch ? path.join(scratch, "clone") : undefined) : holdingDest) : undefined;
        if (movedTo !== undefined) {
          await fs.rename(movedTo, clonePath).then(
            async () => {
              if (scratch) await fs.rm(scratch, { recursive: true, force: true }).catch(() => undefined);
            },
            (renameErr: unknown) =>
              this.log.warn("retireRunnerClone: worker residue quarantine latched mid-retire and the clone could not be moved back; it is retained at the holding path", {
                clone: clonePath,
                retained_at: movedTo,
                error: gitErrorMessage(renameErr),
              }),
          );
        }
        throw new CloneRetainedByQuarantineError();
      }
      if (anchored) await checkSuccessor(anchored);
      if (!verified && !opts.completionSource && still?.runId === ownerRunId && still.clonePath === clonePath) {
        await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]);
      }
      // Completion keeps its existing journal until physical disposal succeeds, so a caught
      // disposal failure can restore the exact canonical source under the held bare lock.
      // issue #1783 M2: an attempt clone the owner's terminal retire disposed of is `retired` in
      // the ledger (its id is never reused). Contained: by here the clone is moved and the journal
      // cleared, so a failed append must neither fail the completed retire nor skip step 6's
      // disposal of the holding copy.
      if (opts.attemptId !== undefined && !opts.completionSource) {
        await this.appendAttemptLedger(barePath, branch, { attemptId: opts.attemptId, runId: ownerRunId, clonePath, state: "retired" }).catch(
          (err: unknown) =>
            this.log.warn("retireRunnerClone: could not record the attempt as retired; the gone path is compacted later", {
              clone: clonePath,
              attempt_id: opts.attemptId,
              error: gitErrorMessage(err),
            }),
        );
      }
      return { holding, scratch, renamed, disposition: holding ? "quarantined" : "source-already-absent" };
    };
    const result = await (bareLockHeld ? retire() : this.withLock(barePath, retire)).catch((error: unknown) => {
      if (opts.orphanDiagnostics) emitOrphanDiagnostic(opts.orphanDiagnostics, clonePath, "orphan_retirement_failed", stage, "canonical_retirement_failure", error);
      throw error;
    });
    // 6. Normal retirement disposes outside its lock; completion keeps the outer bare lock.
    //    Terminal trash (discard) — best-effort delete the
    //    holding dir. Foreign quarantine (!discard) — RETAIN it forever (never delete).
    //    Always best-effort delete the intra-device scratch parent (issue #1354): by here
    //    the canonical is already free and the journal already cleared, so a partial
    //    scratch rm is harmless residue.
    const { holding, scratch } = result;
    const protectedNow = await this.hasPhysicalTerminalProtection(ownerRunId) ||
      (opts.completionSource !== undefined && !opts.completionSource.canDelete());
    // issue #2213: a latch that landed during the protection read keeps the holding and scratch dirs.
    if (!opts.completionSource && residueQuarantine() !== undefined) {
      if (holding || scratch) {
        this.log.warn("retireRunnerClone: worker residue quarantine latched after the journal clear; the holding/scratch dirs are retained, not disposed", {
          run_id: ownerRunId,
          clone: clonePath,
          holding,
          scratch,
        });
      }
      return result.disposition;
    }
    if (opts.completionSource) {
      const open = (): void => {
        if (protectedNow || residueQuarantine() !== undefined || !opts.completionSource!.canDelete())
          throw new Error("completion disposal protection changed");
      };
      try {
        if (!result.renamed || !opts.discard) throw new Error("completion disposal was not established");
        open();
        if (holding && opts.discard) await fs.rm(holding, { recursive: true, force: true });
        open();
        if (scratch) await fs.rm(scratch, { recursive: true, force: true });
        open();
        // Full disposal succeeded in this held lock. Retry only a metadata write, once;
        // every attempt rechecks exact ownership and exclusion. Failure keeps the receipt.
        const finishMetadata = async (): Promise<void> => {
          const pending = await this.readRecoveryCapture(barePath, branch);
          if (pending?.runId !== ownerRunId || pending.clonePath !== clonePath || pending.attemptId !== opts.attemptId)
            throw new Error("completion source journal changed");
          const canonicalAbsent = await fs.lstat(clonePath).then(() => false, (err: NodeJS.ErrnoException) => {
            if (err.code === "ENOENT") return true;
            throw err;
          });
          if (!canonicalAbsent || await this.hasPhysicalTerminalProtection(ownerRunId))
            throw new Error("completion metadata protection changed");
          open();
          if (opts.attemptId !== undefined) {
            const entry = (await this.readAttemptLedger(barePath, branch)).get(opts.attemptId);
            open();
            if (entry?.runId !== ownerRunId || entry.clonePath !== clonePath)
              throw new Error("completion attempt ledger changed");
            if (entry.state !== "retired") {
              try {
                await this.appendAttemptLedger(barePath, branch,
                  { attemptId: opts.attemptId, runId: ownerRunId, clonePath, state: "retired" });
              } catch (error) { return retryWrite(error); }
            }
          }
          const still = await this.readRecoveryCapture(barePath, branch);
          if (still?.runId !== ownerRunId || still.clonePath !== clonePath || still.attemptId !== opts.attemptId)
            throw new Error("completion source journal changed");
          open();
          try { await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), ""]); }
          catch (error) { return retryWrite(error); }
        };
        let retried = false;
        const retryWrite = async (error: unknown): Promise<void> => {
          if (retried || (error as NodeJS.ErrnoException).code !== "EIO") throw error;
          retried = true;
          await finishMetadata();
        };
        await finishMetadata();
      } catch (error) {
        // One rollback attempt, still under adoption exclusion. Partial deletion or a failed
        // proof/rename retains the journal and remaining residue; no new crash discovery.
        const movedTo = scratch ? path.join(scratch, "clone") : holding;
        if (movedTo) {
          try {
            const pending = await this.readRecoveryCapture(barePath, branch);
            if (pending?.runId !== ownerRunId || pending.clonePath !== clonePath || pending.attemptId !== opts.attemptId)
              throw new Error("completion rollback journal changed");
            const canonicalAbsent = await fs.lstat(clonePath).then(() => false, (err: NodeJS.ErrnoException) => {
              if (err.code === "ENOENT") return true;
              throw err;
            });
            const stat = await fs.lstat(movedTo);
            if (!canonicalAbsent || !stat.isDirectory() || stat.isSymbolicLink() ||
                await this.worktreeHead(movedTo) !== opts.completionSource.expectedHead)
              throw new Error("completion rollback source cannot be verified");
            await fs.rename(movedTo, clonePath);
            if (scratch) await fs.rmdir(scratch).catch(() => undefined);
          } catch (rollbackError) {
            this.log.warn("retireRunnerClone: completion rollback retained residue", {
              clone: clonePath, retained_at: movedTo, error: gitErrorMessage(rollbackError),
            });
          }
        }
        throw error;
      }
      return result.disposition;
    }
    // Recheck trusted FINAL proof separately for each disposal; no retries. A refusal
    // retains the moved successor and does not affect predecessor paths or episode pins.
    const checkDisposal = async () => {
      if (opts.verifiedSuccessor && await opts.verifiedSuccessor.acknowledged() !== true) {
        throw new Error("verified successor FINAL authority unavailable");
      }
      if (await this.hasPhysicalTerminalProtection(ownerRunId)) {
        throw new Error("verified successor FINAL authority unavailable");
      }
      if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
    };
    if (holding && opts.discard && !protectedNow) {
      if (opts.verifiedSuccessor) await checkDisposal();
      await fs.rm(holding, { recursive: true, force: true }).catch((e) =>
        this.log.warn("retireRunnerClone: holding dispose failed", {
          path: holding,
          error: gitErrorMessage(e),
        }),
      );
    }
    if (scratch && !protectedNow) {
      if (opts.verifiedSuccessor) await checkDisposal();
      await fs.rm(scratch, { recursive: true, force: true }).catch(() => undefined);
    }
    return result.disposition;
  }

  /** issue #1354 — create an intra-device scratch parent directly under runnerRoot (the
   *  SAME device as the canonical clone, so a rename into it is never EXDEV and survives a
   *  daemon file-hold where a recursive rm races). The mkdir is NON-recursive, so it throws
   *  EEXIST if a runner pre-planted the path — fail closed, rather than using a recursive
   *  0700 mkdir under the runner-writable tree as an ownership assertion; the lstat then
   *  rejects a symlink or other type surprise. Returns the created scratch parent path. */
  private async createRetireScratchParent(): Promise<string> {
    const scratchParent = path.join(this.runnerRoot, `.retire-${randomUUID()}`);
    await fs.mkdir(scratchParent, { mode: 0o700 }); // non-recursive ⇒ throws on EEXIST — fail closed
    const st = await fs.lstat(scratchParent);
    if (!st.isDirectory()) throw new Error("retire scratch parent is not a directory");
    return scratchParent;
  }

  /** issue #2213 — {@link renameCanonicalOrConfirmFree} into `<scratchParent>/clone`, guarded by a
   *  latch read issued with nothing awaited before the rename. On a latch nothing moved: the empty
   *  scratch parent (and the completed off-tree copy at `copy`, if any) are removed best-effort and
   *  the canonical clone stays. */
  private async renameIntoScratchUnlessLatched(clonePath: string, scratchParent: string, copy?: string,
    guard?: () => Promise<void>): Promise<boolean> {
    if (residueQuarantine() !== undefined) {
      await fs.rm(scratchParent, { recursive: true, force: true }).catch(() => undefined);
      if (copy) await fs.rm(copy, { recursive: true, force: true }).catch(() => undefined);
      throw new CloneRetainedByQuarantineError();
    }
    if (guard) await guard();
    if (residueQuarantine() !== undefined) throw new CloneRetainedByQuarantineError();
    return this.renameCanonicalOrConfirmFree(clonePath, path.join(scratchParent, "clone"));
  }

  /** issue #1354 — atomically rename the canonical clone to `dest`, applying the same
   *  ENOENT disambiguation as the step-4 holding rename: the destination's parent is
   *  created immediately before the call, so a rename ENOENT is about the SOURCE — an lstat
   *  that finds it gone means the canonical is already free (returns false), while a source
   *  that is still present makes the ENOENT a real, unexpected failure that must surface.
   *  Any non-ENOENT error is rethrown. Returns true when the rename actually moved the
   *  canonical. */
  private async renameCanonicalOrConfirmFree(clonePath: string, dest: string): Promise<boolean> {
    try {
      await fs.rename(clonePath, dest);
      return true;
    } catch (err) {
      const e = err as NodeJS.ErrnoException;
      if (e.code !== "ENOENT") throw err;
      const sourceGone = await fs.lstat(clonePath).then(
        () => false,
        (le: NodeJS.ErrnoException) => {
          if (le.code === "ENOENT") return true;
          throw le;
        },
      );
      if (!sourceGone) throw err;
      return false;
    }
  }

  /**
   * Files changed on the agent branch since it diverged from the default branch
   * (three-dot diff against the merge base) — used by a self_improve run to flag
   * guard-critical paths in its MR (PRD #46). Under (b) this is a WORKER-BARE
   * tree-to-tree diff (no working tree, no runner-owned config source read): the
   * caller passes the worker-side tracking ref that fetchAgentBranch wrote, and
   * `--name-only -z` fires no diff drivers and preserves unusual path names.
   * Returns null (NOT []) when the diff cannot be computed, so the caller fails CLOSED — a loud "guard-path check unavailable" note
   * rather than silently raising no flag on a possibly guard-touching MR (M5 audit).
   * An empty list means "computed, nothing changed".
   */
  async changedFiles(barePath: string, trackingRef: string): Promise<string[] | null> {
    try {
      const baseRef = await this.defaultBranchRef(barePath);
      const out = await this.runGit(barePath, ["diff", "--name-only", "-z", `${baseRef}...${trackingRef}`]);
      return out.split("\0").filter(Boolean);
    } catch {
      return null;
    }
  }

  /** Pushed workflow blobs must match the fresh default or the current remote target, per path. */
  async branchWorkflowFiles(
    barePath: string,
    freshDefaultTip: string,
    trackingRef: string,
    targetTip?: string,
  ): Promise<string[] | null> {
    try {
      if (!SHA40_RE.test(freshDefaultTip) || (targetTip !== undefined && !SHA40_RE.test(targetTip))) return null;
      const blobs = async (tip: string): Promise<Map<string, string>> => {
        const out = await this.runGit(barePath, ["ls-tree", "-r", "-z", tip, "--", ".github/workflows/"]);
        const entries = new Map<string, string>();
        for (const entry of out.split("\0")) {
          if (!entry) continue;
          const match = /^\d{6} blob ([0-9a-f]{40})\t([\s\S]+)$/.exec(entry);
          if (!match) throw new Error("unexpected workflow tree entry");
          entries.set(match[2]!, match[1]!);
        }
        return entries;
      };
      const pushed = await blobs(trackingRef);
      const defaults = await blobs(freshDefaultTip);
      const target = targetTip === undefined ? new Map<string, string>() : await blobs(targetTip);
      return [...pushed].filter(([file, oid]) => defaults.get(file) !== oid && target.get(file) !== oid)
        .map(([file]) => file).sort();
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return null;
    }
  }

  /** Read a fresh target without changing claim mirrors, tags or FETCH_HEAD. */
  async fetchWorkflowTargetTip(
    barePath: string, branch: string, pat?: string, repoUrl?: string, username?: string,
  ): Promise<{ kind: "present"; sha: string } | { kind: "absent" } | { kind: "unavailable" }> {
    const scope = repoUrl ? httpScopeForUrl(repoUrl) : undefined;
    try {
      return await this.withLock(barePath, async () => {
        const tempRef = `refs/uzi-workflow-target/${randomUUID()}`;
        try {
          if (!(await this.isPlainBranchName(barePath, branch))) return { kind: "unavailable" } as const;
          const remoteRef = `refs/heads/${branch}`;
          const listed = await this.runGit(barePath, ["ls-remote", "origin", remoteRef], pat, scope, username);
          if (listed === "") return { kind: "absent" } as const;
          const match = /^([0-9a-f]{40})\t([^\n]+)\n?$/.exec(listed);
          if (!match || match[2] !== remoteRef) return { kind: "unavailable" } as const;
          await this.runGit(barePath, ["fetch", "--refmap=", "--no-tags", "--no-write-fetch-head",
            "origin", `+${remoteRef}:${tempRef}`], pat, scope, username);
          const sha = await this.resolveCommitStrict(barePath, tempRef);
          return sha === match[1] ? { kind: "present", sha } as const : { kind: "unavailable" } as const;
        } catch (cause) {
          if (cause instanceof RunResidueBlockedError) throw cause; // issue #2213
          const abort = this.boundaryAbortError(cause);
          if (abort) throw abort;
          return { kind: "unavailable" } as const;
        } finally {
          await this.runGit(barePath, ["update-ref", "-d", tempRef]).catch(async (cause: unknown) => {
            if (!(cause instanceof GitBoundaryAbortError || cause instanceof CheckpointSoftDeadlineError)) throw cause;
            // execScoped has settled every child before reporting abort. The worker-owned
            // files-ref bare has auto-maintenance disabled by gitEnv/disableAutoMaintenance,
            // and withLock still excludes other bare mutations: this fresh UUID stays loose.
            // Attempt exactly these four files, concurrently; no sibling is skipped on failure.
            await Promise.allSettled([
              tempRef, `${tempRef}.lock`, `logs/${tempRef}`, `logs/${tempRef}.lock`,
            ].map((refFile) => fs.rm(path.join(barePath, refFile), { force: true })));
            // A cleanup I/O failure must not turn cancellation into an unavailable snapshot.
            throw cause;
          });
        }
      });
    } catch (cause) {
      if (cause instanceof RunResidueBlockedError) throw cause; // issue #2213
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "unavailable" };
    }
  }

  /**
   * PRD #212: the plan-turn changed-file list for the approval gate. Runs
   * `git status --porcelain` in the runner clone AS THE RUNNER UID (runGitAsRunner),
   * NEVER a worker-uid helper — `git status` touches the working tree and can fire
   * attacker-chosen .gitattributes filter.<name>.clean drivers that exec as the running
   * uid, so a worker-uid status would re-open code-exec as the PAT holder (PRD #51 M0).
   * A runner-uid status in the runner-owned clone is the untrusted uid exec'ing in its
   * own tree — not a boundary crossing (git.ts topology comment ~:39-44).
   *
   * BEST-EFFORT: returns [] on ANY error (a benign git failure, or a pathological/hostile
   * tree that hangs to GIT_TIMEOUT_MS). This is a VISIBILITY feature — it must never throw
   * into the awaiting_approval report, whose reportState await has no .catch and would
   * abort the run instead of parking it at the gate. Mirrors changedFiles → [] on error.
   *
   * Honors .gitignore (no --ignored): the list is bounded to non-ignored
   * tracked-modifications + untracked files — the plan-turn writes worth surfacing.
   * Each returned element is a raw porcelain line ("XY <path>"); the LEADING space of the
   * XY status code is meaningful, so DO NOT trim lines (only split + drop empties). The
   * server (api) re-sanitizes and caps each line; this is not the last line of defense.
   */
  async planChangedFiles(cwd: string): Promise<string[]> {
    try {
      const out = await this.runGitAsRunner(cwd, ["status", "--porcelain"]);
      return out.split("\n").filter((l) => l.length > 0);
    } catch (err) {
      this.log.debug("plan-turn git status failed (best-effort → no changes)", {
        cwd,
        error: gitErrorMessage(err),
      });
      return [];
    }
  }

  /**
   * Owner-cancel inspection only. Clone observations run as the runner uid without optional
   * locks. Status is only a metadata/untracked check: a separate runner-identity reader
   * hashes regular-file bytes and raw symlink target bytes against the trusted bare starting
   * tree, never following targets or trusting the clone's index or object store.
   * Unsupported types/platforms and budgets retain custody.
   * Ignored untracked .gitignore retains custody, even in trusted-ignored directories such as Python virtualenvs.
   * Trusted tracked .gitignore semantics remain unchanged. External excludes are disabled;
   * info/exclude permits only the exact worker-source '/.uzi/scratch/' baseline plus
   * column-zero comments and empty or ASCII-space-only blanks. Uncertain rules retain custody.
   */
  async credentialFreeCancelCleanHead(cwd: string, barePath: string, trustedStart: string): Promise<string | null> {
    const pins = [
      // Compare the real HEAD tree: runner-owned refs/replace must not hide private bytes.
      "--no-replace-objects",
      "--no-optional-locks",
      "-c", "core.fsmonitor=false",
      "-c", "core.untrackedCache=false",
      "-c", "core.ignoreStat=false",
      "-c", "core.checkStat=default",
      "-c", "core.trustctime=true",
      "-c", "core.fileMode=true",
      "-c", "core.ignoreCase=false",
      "-c", "core.excludesFile=/dev/null",
    ];
    const read = (args: string[]) => this.runGitAsRunner(cwd, [...pins, ...args]);
    try {
      if (process.platform !== "linux" || !path.isAbsolute(cwd) ||
          path.resolve(cwd) !== cwd || !/^[0-9a-f]{40}$/.test(trustedStart)) return null;
      // Pure worker-side object enumeration: the private clone cannot supply the tree.
      // Each subprocess has a deadline/output cap; no retries or sibling work.
      const tree = await this.execScoped(GIT_BIN,
        withDir(barePath, ["--no-replace-objects", "ls-tree", "-r", "-l", "-z", trustedStart]), {
          env: workerSpawnEnv(gitEnv()), timeout: 5000, maxBuffer: 2 * 1024 * 1024,
        });
      const manifest: Array<[string, string, string, number]> = [];
      let total = 0;
      for (const entry of tree.stdout.split("\0")) {
        if (!entry) continue;
        const match = /^(100644|100755|120000) blob ([0-9a-f]{40}) +([0-9]+)\t([\s\S]+)$/.exec(entry);
        if (!match) return null;
        // execScoped decodes UTF-8. Refuse replacement characters so distinct raw
        // Git paths cannot collapse to the same runner-reader path.
        if (match[4]!.includes("\uFFFD")) return null;
        const size = Number(match[3]);
        total += size;
        if (!Number.isSafeInteger(size) || size > 4 * 1024 * 1024 ||
            total > 128 * 1024 * 1024 || manifest.length >= 20000) return null;
        manifest.push([match[4]!, match[1]!, match[2]!, size]);
      }
      // config/index/ref reads cannot invoke a clean/process filter. Reading all effective
      // config also sees included config files; any read failure refuses the proof.
      const config = await read(["config", "--null", "--list"]);
      for (const entry of config.split("\0")) {
        const split = entry.indexOf("\n");
        const key = (split < 0 ? entry : entry.slice(0, split)).toLowerCase();
        if (/^filter\..*\.(clean|process)$/.test(key) ||
            key === "core.worktree" || key === "core.sparsecheckout" ||
            key === "extensions.worktreeconfig") return null;
      }
      const flags = await read(["ls-files", "-v", "-z"]);
      if (flags.split("\0").some(entry => entry && (entry[0] === "S" || /^[a-z]/.test(entry)))) return null;
      const index = await read(["ls-files", "--stage", "-z"]);
      const expectedIndex = manifest.map(([name, mode, oid]) => mode + " " + oid + " 0\t" + name + "\0").join("");
      if (index !== expectedIndex) return null;
      const before = (await read(["rev-parse", "--verify", "HEAD^{commit}"])).trim();
      if (before !== trustedStart) return null;
      const status = await read(["status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none"]);
      if (status.length !== 0) return null;
      const ignored = await read(["ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--", ":(glob)**/.gitignore", ":(exclude).uzi/scratch/**"]);
      if (ignored.split("\0").some(entry => entry && !entry.startsWith(".uzi/scratch/"))) return null;
      const wrapped = this.boundaryProcesses.getStore()
        ? { command: process.execPath, args: ["-e", CANCEL_CONTENT_HELPER, cwd] }
        : runnerCommand(process.execPath, ["-e", CANCEL_CONTENT_HELPER, cwd]);
      const input = JSON.stringify(manifest);
      if (Buffer.byteLength(input) > 2 * 1024 * 1024) return null;
      const proof = await this.execScoped(wrapped.command, wrapped.args, {
        env: unmarkedSpawnEnv({ ...gitEnv(), PATH: runnerPath() }),
        cwd, input, timeout: 30000, maxBuffer: 1024,
      }, "command");
      if (proof.stdout !== "clean\n") return null;
      const after = (await read(["rev-parse", "--verify", "HEAD^{commit}"])).trim();
      return status.length === 0 && before === after ? after : null;
    } catch {
      return null;
    }
  }

  /**
   * Issue #281 / CodeRabbit #655: the porcelain read for the no-progress detector's
   * worktree fingerprint. Identical git invocation and runner-uid rationale as
   * planChangedFiles, but returns `null` on a failed read instead of `[]`, so an
   * UNREADABLE status is never mistaken for a genuinely clean tree. planChangedFiles
   * swallows errors to `[]` on purpose (a plan-gate visibility feature that must never
   * throw); the fingerprint needs the opposite — a failed read must NOT look like "no
   * changes", or a run whose status read keeps failing while the lead repeats itself
   * could trip the detector without the tree ever having been verified. The caller
   * treats `null` as "cannot assert unchanged" (no trip).
   */
  async worktreeStatus(cwd: string): Promise<string[] | null> {
    try {
      const out = await this.runGitAsRunner(cwd, ["status", "--porcelain"]);
      return out.split("\n").filter((l) => l.length > 0);
    } catch (err) {
      this.log.debug("fingerprint git status failed (→ null, cannot assert unchanged)", {
        cwd,
        error: gitErrorMessage(err),
      });
      return null;
    }
  }

  /**
   * PRD #759 M1 — commit the runner clone's uncommitted work to a clearly-marked
   * THROWAWAY commit on the park path, so the existing fetch-back + #628 checkpoint
   * broker carry that work off the tree before the reseed's `fs.rm` wipes it. This is
   * the one thing run #685 lacked: every durability layer captures committed commits
   * only, so ~4h of mid-milestone work that had never been committed was lost on park.
   *
   * Runs AS THE RUNNER UID (runGitAsRunner) in the runner-owned clone — never a
   * worker-uid git op. `git status`/`add`/`commit` touch the working tree and can fire
   * attacker-chosen .gitattributes filter drivers that exec as the running uid; a
   * worker-uid write would re-open code-exec as the PAT holder (PRD #51 M0). A runner-uid
   * write in the runner-owned clone is the untrusted uid exec'ing in its own tree — not a
   * boundary crossing (git.ts topology comment ~:39-48). The AGENT_GIT_IDENTITY and
   * `commit.gpgsign=false` planted at seed (~:536-538) mean the commit needs no identity
   * flags and cannot be blocked by a signing config.
   *
   * The subject is prefixed WIP_PARK_COMMIT_PREFIX so it is a recognizable throwaway:
   * M2 detects it on the adopted tip and `git reset --soft <parent>` restores the content
   * to UNCOMMITTED at adopt time, so the marker never enters the history the agent builds
   * on and never reaches the MR. This deliberately reverses PRD #218 D6's "no auto-commit
   * on park" — a decision the maintainer explicitly asked to revisit (PRD #759 M1 / D3):
   * a marked throwaway stripped back to uncommitted at adopt time never masquerades as
   * reviewed work, so #218 D6's "a half-applied edit that survives is worse than one that
   * does not" no longer applies.
   *
   * BEST-EFFORT: every error is caught, logged, and returns `false` rather than thrown —
   * a commit failure must NEVER propagate, because parking is the state that preserves the
   * tree and a failed park loses MORE than a missing WIP commit (D4). Returns `true` only
   * when a marker commit was actually created (an already-clean tree returns `false` —
   * nothing to commit).
   */
  async commitWipMarker(clonePath: string): Promise<boolean> {
    try {
      const status = await this.runGitAsRunner(clonePath, ["status", "--porcelain"]);
      if (status.split("\n").filter((l) => l.length > 0).length === 0) {
        return false; // clean tree — nothing to save
      }
      await this.runGitAsRunner(clonePath, ["add", "-A"]);
      await this.runGitAsRunner(clonePath, [
        "commit",
        "-m",
        `${WIP_PARK_COMMIT_PREFIX} interrupted work auto-saved on usage-limit park (throwaway; restored uncommitted on resume)`,
      ]);
      return true;
    } catch (err) {
      this.log.warn("WIP park auto-commit failed (best-effort → not committed)", {
        cwd: clonePath,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /**
   * PRD #1190 — undo a `commitWipMarker` commit, restoring its content to the UNCOMMITTED
   * working tree (`git reset --mixed HEAD^`, runner uid). The pause park is the one caller
   * that can create a marker and then NOT park (a failed checkpoint publish keeps the run
   * RUNNING, Decision 8): the marker must not stay at the clone HEAD, or it would ride into
   * the eventual MR and the restarted turn would build on a throwaway commit. This is the
   * SAME restore the resume adopt does (#759 M2, `reset --soft` on the adopted marker), run
   * inline here because a continuing run never reseeds. BEST-EFFORT: every error is caught
   * and logged — a failed restore must never propagate, exactly as `commitWipMarker` never
   * propagates a commit failure (D4). Only ever called after `commitWipMarker` returned true,
   * so HEAD^ is the marker's parent (the clone was checked out at a real base commit).
   */
  async undoWipMarker(clonePath: string): Promise<void> {
    try {
      await this.runGitAsRunner(clonePath, ["reset", "--mixed", "HEAD^"]);
    } catch (err) {
      this.log.warn("WIP park marker undo failed (best-effort → marker left at HEAD)", {
        cwd: clonePath,
        error: gitErrorMessage(err),
      });
    }
  }

  /**
   * PRD #1247 M5b (data-integrity fix) — true iff the runner clone's HEAD commit is a
   * `wip(park):` marker (its subject starts with WIP_PARK_COMMIT_PREFIX, the same prefix
   * `commitWipMarker` plants). Read as the RUNNER uid (`runGitAsRunner`, `log -1
   * --format=%s`) because the clone is runner-owned — a worker-uid read would hit the B2
   * dubious-ownership boundary. Reading the subject is a pure object read (no working-tree
   * touch), so no attacker-chosen filter driver fires.
   *
   * This SELF-GUARDS `undoWipMarker` at a CONTINUE-IN-PLACE call site: `undoWipMarker` is a
   * blind `reset --mixed HEAD^` (it does NOT check what HEAD is), so it must run ONLY when
   * HEAD actually IS a marker. The credential-switch give-up-continue path
   * (attemptCredentialSwitch → "gave_up") commits a marker via captureRecoveryRestorePoint
   * on a DIRTY tree but never reseeds, so the marker would otherwise ride into the MR;
   * gating the undo on this check retires it without threading a `markerCreated` flag out of
   * the SHARED captureRecoveryRestorePoint. BEST-EFFORT: any error (unreadable clone, missing
   * HEAD) answers false, so a bad read never triggers a blind reset.
   */
  async headIsWipMarker(clonePath: string): Promise<boolean> {
    try {
      const subject = await this.runGitAsRunner(clonePath, ["log", "-1", "--format=%s"]);
      return subject.startsWith(WIP_PARK_COMMIT_PREFIX);
    } catch (err) {
      this.log.warn("WIP park marker HEAD check failed (best-effort → not a marker)", {
        cwd: clonePath,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /**
   * The unified diff of the reviewed `branch` against `base` (three-dot: the changes on
   * `branch` since it diverged from `base`), for a PRD #400 M4b diff-review run. `branch` is
   * resolved as the bare's remote-tracking ref (`refs/remotes/origin/<name>`, which every
   * fetch updates — the same namespace `changedFiles` reads), so the caller must have
   * fetched the bare (ensureClone) first; the reviewed task branch is pushed to origin (M2),
   * so it exists there. `base` may be EITHER a branch name (resolved the same way, under
   * `refs/remotes/origin/`) OR a commit-ish — the seed commit sha a handoff records as its
   * base when created without --base (issue #403 F3), which does not exist under
   * `refs/remotes/origin/` but is present in the mirror as an ancestor of the reviewed
   * branch.
   *
   * The result is CAPPED at REVIEW_DIFF_MAX_BYTES with a truncation marker: a pathological
   * diff must not blow the reviewer model's context or the worker's memory. `--no-color`
   * keeps the text plain for the model, and `--no-ext-diff` is LOAD-BEARING: gitEnv pins
   * `diff.external=true` (a code-exec-key neutralization), so a plain `git diff` would run
   * that no-op external driver and emit NOTHING — `changedFiles` sidesteps it with
   * `--name-only`; a real patch must disable it explicitly.
   */
  async reviewDiff(barePath: string, base: string, branch: string): Promise<string> {
    // issue #403 F3: `base` is usually a branch name (resolved under refs/remotes/origin/), but a
    // handoff created without --base records the SEED COMMIT sha as its base so the review diffs
    // only the worker's commits, not the user's seeded HEAD. A raw sha does not resolve under
    // refs/remotes/origin/, so fall back to using `base` verbatim — the seed commit is present in
    // the mirror as an ancestor of the reviewed branch.
    const remoteBaseRef = `refs/remotes/origin/${base}`;
    const baseRef = (await this.refExists(barePath, remoteBaseRef)) ? remoteBaseRef : base;
    const branchRef = `refs/remotes/origin/${branch}`;
    const out = await this.runGit(barePath, ["diff", "--no-color", "--no-ext-diff", `${baseRef}...${branchRef}`]);
    const buf = Buffer.from(out, "utf8");
    if (buf.byteLength <= REVIEW_DIFF_MAX_BYTES) return out;
    const marker = `\n… diff truncated at ${REVIEW_DIFF_MAX_BYTES} bytes (${buf.byteLength} total) …\n`;
    // Slice on a byte boundary so a giant diff cannot balloon the string we keep. A cut
    // through a multi-byte rune yields at most one U+FFFD; harmless for review text.
    return buf.subarray(0, REVIEW_DIFF_MAX_BYTES).toString("utf8") + marker;
  }

  /**
   * PRD #1798 M1 — the merge-base of the MR's target branch and the landed head, the base of the
   * size line's diff (D3). `targetBranch` resolves the same way {@link reviewDiff} resolves its base:
   * the bare's origin-tracking ref `refs/remotes/origin/<target>` when it exists, else verbatim.
   * Worker-uid bare read. Throws when no merge-base resolves (the caller renders "unavailable").
   */
  async sizeMergeBase(barePath: string, targetBranch: string, headSha: string): Promise<string> {
    const remoteRef = `refs/remotes/origin/${targetBranch}`;
    const targetRef = (await this.refExists(barePath, remoteRef)) ? remoteRef : targetBranch;
    const sha = (await this.runGit(barePath, ["merge-base", targetRef, headSha])).trim();
    if (!SHA40_RE.test(sha)) throw new Error(`merge-base of ${targetRef} and ${headSha} is not a commit SHA`);
    return sha;
  }

  /**
   * PRD #1798 M1 — raw `git diff -z --numstat -M <base> <head>` of the landed head (D3), parsed by
   * pr-size.ts. A tree-to-tree diff in the worker bare (no working tree). `--no-ext-diff` and
   * `--no-textconv` keep git's own line counts regardless of a repo-declared diff driver (gitEnv pins
   * `diff.external`, see {@link reviewDiff}). Throws on failure.
   */
  async diffNumstatZ(barePath: string, base: string, headSha: string): Promise<string> {
    return this.runGit(barePath, ["diff", "-z", "--numstat", "-M", "--no-ext-diff", "--no-textconv", base, headSha]);
  }

  /**
   * PRD #1798 M5 — a BOUNDED read in the worker bare: `git -C <barePath> <args>` as the worker uid on
   * the credential-free base gitEnv pins (no PAT, no shell), with stdout STREAMED rather than
   * buffered whole. Once more than `maxBytes` has arrived the read stops and the first `maxBytes`,
   * cut back to a UTF-8 code-point boundary, resolve with `truncated: true`. An oversized output is
   * never an error. Rejects on a non-zero exit before the bound (with at most
   * READ_BARE_STDERR_MAX_BYTES of git's stderr in the message), when `timeoutMs` (default
   * GIT_TIMEOUT_MS) elapses, and when `signal` aborts. Throws up front on a non-finite or negative
   * `maxBytes`, a non-positive or non-finite `timeoutMs`, or an already-aborted `signal`.
   *
   * On the bound, the timeout and the abort, the stdout stream is abandoned (destroyed). What
   * that does to the child depends on the spawn path ({@link spawnGit}):
   * - Outside a boundary scope, abandoning the stream kills the child (SIGTERM) at once.
   * - Inside a boundary scope (withBoundaryProcessSpawner) this path does not call the handle's cancel(): abandoning
   *   the stream only unpipes and destroys its stdout, so a child that is still writing meets a
   *   closed pipe on its next write and an idle one keeps running. Its termination is then left to
   *   the `timeoutMs` forwarded to the spawner (BoundaryProcessRequest.timeoutMs, whose contract is
   *   to terminate the child's process group; only the forwarding is tested here, with a fake
   *   spawner) and to the scope's own boundary deadline. An abort through `signal` therefore rejects at once but, in
   *   this path, leaves an idle child alive until that timeout.
   */
  async readBare(barePath: string, args: readonly string[], opts: BoundedReadOptions): Promise<BoundedRead> {
    const what = `git ${args.join(" ")}`;
    if (!Number.isFinite(opts.maxBytes) || opts.maxBytes < 0) {
      throw new Error(`${what}: maxBytes must be a finite, non-negative number`);
    }
    const maxBytes = Math.floor(opts.maxBytes);
    if (opts.signal?.aborted) throw new Error(`${what} aborted before it started`);
    const timeoutMs = opts.timeoutMs ?? GIT_TIMEOUT_MS;
    if (Number.isNaN(timeoutMs) || timeoutMs <= 0) throw new Error(`${what} has no time left`);
    if (!Number.isFinite(timeoutMs)) throw new Error(`${what}: timeoutMs must be finite`);
    const deadlineError = new Error(`${what} exceeded ${timeoutMs}ms`);
    const { stdout } = await this.spawnGit(barePath, [...args], undefined, {
      timeoutMs,
      nativeTimeoutError: deadlineError,
      stderrMaxBytes: READ_BARE_STDERR_MAX_BYTES,
    });
    return new Promise<BoundedRead>((resolve, reject) => {
      const chunks: Buffer[] = [];
      let bytes = 0;
      let settled = false;
      let timer: NodeJS.Timeout | undefined;
      const finish = (result: BoundedRead | undefined, error?: Error): void => {
        if (settled) return;
        settled = true;
        if (timer !== undefined) clearTimeout(timer);
        opts.signal?.removeEventListener("abort", onAbort);
        stdout.removeListener("data", onData);
        stdout.removeListener("end", onEnd);
        stdout.removeListener("error", onError);
        // Abandon the exit-gated stream before it ended: outside a boundary this kills the child;
        // inside one it only closes its stdout (see the docstring).
        if (result === undefined || result.truncated) stdout.destroy();
        if (result === undefined) reject(error);
        else resolve(result);
      };
      const text = (truncated: boolean): BoundedRead => {
        // StringDecoder.write holds back an incomplete trailing sequence: a cut through a
        // multi-byte character drops that character instead of rendering U+FFFD.
        const decoded = new StringDecoder("utf8").write(Buffer.concat(chunks, bytes));
        return { text: decoded, truncated };
      };
      const onData = (chunk: Buffer | string): void => {
        const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
        const room = maxBytes - bytes;
        if (buf.length > room) {
          if (room > 0) chunks.push(buf.subarray(0, room));
          bytes = maxBytes;
          finish(text(true));
          return;
        }
        chunks.push(buf);
        bytes += buf.length;
      };
      const onEnd = (): void => finish(text(false));
      const onError = (error: Error): void => finish(undefined, error);
      const onAbort = (): void => finish(undefined, new Error(`${what} aborted`));
      stdout.on("data", onData);
      stdout.once("end", onEnd);
      stdout.once("error", onError);
      timer = setTimeout(() => finish(undefined, deadlineError), timeoutMs);
      if (opts.signal?.aborted) onAbort();
      else opts.signal?.addEventListener("abort", onAbort, { once: true });
    });
  }

  /**
   * PRD #1798 M1 — the linguist attributes of `paths` at commit `headSha`, via `check-attr --source`
   * with the isolated temp-index fallback for a git without `--source` (the order and the failure
   * rules live in {@link lookupAttributes}). Worker-uid bare reads. Throws on any failure other than
   * "--source unsupported", so the size line renders unavailable rather than ignoring attributes.
   */
  async checkAttrZ(barePath: string, headSha: string, paths: readonly string[]): Promise<Map<string, PathAttributes>> {
    return lookupAttributes((args, opts) => this.sizeAttrGit(barePath, args, opts), headSha, paths);
  }

  /** The attribute-lookup git runner behind {@link checkAttrZ}: `git -C <bare> <args>` with optional stdin
   *  and an optional GIT_INDEX_FILE. The rejection keeps git's raw `stderr` (unlike runGit's wrapped
   *  message, which echoes the args and so would always "mention --source"). */
  private async sizeAttrGit(barePath: string, args: string[], opts: { input?: string; indexFile?: string }): Promise<string> {
    const env = gitEnv();
    if (opts.indexFile) env.GIT_INDEX_FILE = opts.indexFile;
    this.log.debug("git", { cwd: barePath, args });
    try {
      const { stdout } = await this.execScoped("git", withDir(barePath, args), {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
        ...(opts.input === undefined ? {} : { input: opts.input }),
      });
      return stdout;
    } catch (err) {
      const stderr = (err as { stderr?: unknown }).stderr;
      const wrapped = new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`) as Error & { stderr?: string };
      wrapped.stderr = typeof stderr === "string" ? stderr : "";
      throw wrapped;
    }
  }

  /**
   * PRD #377 M1 — the full unified diff of the agent branch (`trackingRef`) against the
   * default-branch base, preserved on a `failed` report when a GitHub run's branch touches
   * `.github/workflows/**` and the bot's repo-only PAT cannot push it. Mirrors
   * `changedFiles`'s base resolution (`defaultBranchRef`, three-dot) but emits a REAL patch
   * instead of a name list.
   *
   * `--no-ext-diff` is LOAD-BEARING for the same reason it is in `reviewDiff`: gitEnv pins
   * `diff.external=true` (a code-exec-key neutralization — see GIT_CODE_EXEC_KEY_PINS), so a
   * plain `git diff` runs that no-op external driver and emits NOTHING; a real patch must
   * disable it explicitly. `changedFiles` sidesteps it via `--name-only`. `--no-color` keeps
   * the text plain.
   *
   * The result is CAPPED at REVIEW_DIFF_MAX_BYTES with a truncation marker, sliced on a byte
   * boundary (a cut through a multi-byte rune yields at most one U+FFFD). It is secret-scrubbed
   * by the CALLER (redactText) before it leaves the worker — this method does not scrub. Returns
   * `null` on ANY failure (try/catch), so the caller can still report the failed outcome without
   * a patch rather than turning a diff-computation error into a second failure.
   */
  async workflowScopeDiff(barePath: string, trackingRef: string): Promise<string | null> {
    try {
      const baseRef = await this.defaultBranchRef(barePath);
      const out = await this.runGit(barePath, [
        "diff",
        "--no-color",
        "--no-ext-diff",
        `${baseRef}...${trackingRef}`,
      ]);
      const buf = Buffer.from(out, "utf8");
      if (buf.byteLength <= REVIEW_DIFF_MAX_BYTES) return out;
      const marker = `\n… diff truncated at ${REVIEW_DIFF_MAX_BYTES} bytes (${buf.byteLength} total) …\n`;
      return buf.subarray(0, REVIEW_DIFF_MAX_BYTES).toString("utf8") + marker;
    } catch {
      return null;
    }
  }

  /**
   * Scan the EXACT text a run is about to persist as its preserved_patch (the redacted
   * {@link workflowScopeDiff} output) through `gitleaks stdin`, with the finalize scan's discipline:
   * an explicit default-ruleset config (no repo `.gitleaks.toml`), inline allows ignored, `--redact`,
   * a neutral scratch cwd. The whole patch is scanned, context and removed lines included, because
   * all of it is stored and displayed. Liveness: a clean exit, no error token, and gitleaks'
   * `scanned ~N bytes` equal to the bytes fed. The caller may attach the patch only when this is
   * trusted AND clean; it never throws (any failure is untrusted).
   */
  async scanPatchForSecrets(patch: string): Promise<{ trusted: boolean; findings: SecretFinding[] }> {
    if (patch.length === 0) return { trusted: true, findings: [] };
    let scratch: string | undefined;
    try {
      scratch = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-gl-patch-"));
      const configPath = path.join(scratch, "config.toml");
      const reportPath = path.join(scratch, "report.json");
      await fs.writeFile(configPath, "[extend]\nuseDefault = true\n", "utf8");
      const deadline = Date.now() + PATCH_SCAN_TIMEOUT_MS;
      const run = await this.runCheckpointGitleaks(gitleaksStdinArgs({ configPath, reportPath }), {
        cwd: scratch,
        reportPath,
        remaining: () => deadline - Date.now(),
        input: patch,
        selfTimeout: true,
      });
      if (run.kind !== "ran") return { trusted: false, findings: [] };
      const scanned = /\bscanned ~(\d+) bytes/i.exec(run.stderr);
      const trusted =
        run.execOk &&
        !/\b(err|error|fatal)\b/i.test(run.stderr) &&
        scanned !== null &&
        Number(scanned[1]) === Buffer.byteLength(patch) &&
        !/\bskipp/i.test(run.stderr);
      return { trusted, findings: run.findings };
    } catch {
      return { trusted: false, findings: [] };
    } finally {
      if (scratch) await fs.rm(scratch, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /**
   * issue #1398 — resolve the FLOOR the finalize secret scan should treat as
   * already-published, so `floor..trackingRef` is the REAL push delta and no
   * already-pushed history is re-scanned. Returns a 40-hex SHA or a ref string, or
   * `null` to mean "fail open" (no trustworthy floor).
   *
   * FIRST push (no `refs/remotes/origin/<branch>` in the worker bare): the whole branch
   * is new, so the floor is the default branch — exactly what a push carries.
   *
   * RESUMED / continued branch (the ref exists): the floor is the CURRENT remote branch
   * tip, and it must be FRESH. The last origin contact was at claim time, so the bare's
   * `refs/remotes/origin/<branch>` mirror can be stale; flooring on the stale mirror (or
   * on the default branch) re-scans already-pushed commits and can rediscover an old
   * inline-allowed synthetic fixture, falsely failing the run as `push_secret_blocked`.
   *
   * NON-CLOBBER (load-bearing; issue #1117 dependency): the mr_rework branch-moved
   * detection reads the CLAIM-TIME `refs/remotes/origin/<branch>` (via originBranchTip)
   * BEFORE its own fetchDefaultTip, so this fresh fetch must NOT touch that ref. A plain
   * `git fetch origin <branch>` would update it via the configured refspec. Even an explicit
   * command-line refspec into a scratch ref is NOT enough on its own: git still performs an
   * OPPORTUNISTIC update of `refs/remotes/origin/<branch>` because the fetched `refs/heads/
   * <branch>` matches the bare's configured `+refs/heads/*:refs/remotes/origin/*` refspec
   * (measured on git 2.54: the mirror moved to the fresh tip). We suppress that with
   * `--refmap=` (empty), which makes git ignore the configured refspecs entirely and rely
   * only on the command-line one, so ONLY the scratch ref `refs/uzi-secret-scan-floor/
   * <branch>` is written and `refs/remotes/origin/<branch>` is left untouched. The scratch
   * ref is deleted on EVERY exit path; its objects survive in the object store (no gc
   * mid-process), so the returned SHA stays usable as the caller's range base after the ref
   * is gone.
   *
   * ANCESTRY / FAIL-OPEN: the fresh tip is used only when it is an ancestor of trackingRef
   * (the push is a fast-forward over it). A diverged/rewound remote, an unresolved tip, or
   * any thrown error (an unreadable bare, a failed fetch) returns `null` — the caller then
   * fails open and relies on the GH013 remote backstop rather than blocking or scanning a
   * wrong range.
   */
  async resolvePublicationFloor(
    barePath: string,
    trackingRef: string,
    branch: string,
    creds?: { pat?: string; cloneUrl?: string; username?: string },
  ): Promise<string | null> {
    const originRef = `refs/remotes/origin/${branch}`;
    if (!(await this.refExists(barePath, originRef))) {
      // FIRST push: the whole branch is new, so the default branch is the floor.
      try {
        return await this.defaultBranchRef(barePath);
      } catch {
        return null;
      }
    }
    // RESUMED branch: floor on a FRESH, verified remote tip fetched into a scratch ref that
    // does NOT clobber refs/remotes/origin/<branch> (see NON-CLOBBER above).
    const scratchRef = `refs/uzi-secret-scan-floor/${branch}`;
    const scope = creds?.cloneUrl ? httpScopeForUrl(creds.cloneUrl) : undefined;
    try {
      return await this.withLock(barePath, async () => {
        try {
          await this.runGit(
            barePath,
            // `--refmap=` (empty) suppresses the bare's configured
            // `+refs/heads/*:refs/remotes/origin/*` refspec, so this fetch does NOT
            // opportunistically clobber refs/remotes/origin/<branch> (the NON-CLOBBER
            // invariant above) — only the explicit scratch ref is written.
            ["fetch", "--refmap=", "origin", `+refs/heads/${branch}:${scratchRef}`],
            creds?.pat,
            scope,
            creds?.username,
          );
          const tip = (
            await this.runGit(barePath, [
              "rev-parse",
              "--verify",
              `${scratchRef}^{commit}`,
            ]).catch(() => "")
          ).trim();
          if (!/^[0-9a-f]{40}$/.test(tip)) return null;
          // Only floor on the fresh tip when the push is a fast-forward over it; a
          // diverged/rewound remote fails open (the delta is not simply floor..tip).
          if (!(await this.isAncestorRef(barePath, tip, trackingRef))) return null;
          return tip;
        } finally {
          // Delete the scratch ref on EVERY exit path (success, fail-open, thrown error). Its
          // objects remain in the object store, so a returned SHA stays usable as the range base.
          await this.runGit(barePath, ["update-ref", "-d", scratchRef]).catch(() => undefined);
        }
      });
    } catch {
      return null;
    }
  }

  /**
   * PRD #974 M2 (load-bearing security) — scan the commit range the finalize push would carry
   * for secrets with the repo's pinned gitleaks, GitLeaks' three silencers DISABLED, and
   * return whether the scan is TRUSTWORTHY plus any findings.
   *
   * Range: `base..head` (TWO-dot — the commits ON the branch and NOT below the publication
   * floor), base = the publication floor (git.resolvePublicationFloor), head = trackingRef.
   * `base..head` equals what a push carries ONLY for a FIRST push of a NEW remote branch
   * (floor = the default branch); on a RESUMED/continued branch the floor is the CURRENT,
   * FRESHLY-FETCHED remote branch tip, so already-pushed commits are EXCLUDED (issue #1398 —
   * flooring on the default branch there re-scanned pushed history and could falsely block on
   * an old inline-allowed fixture). An empty range (0 commits) is the nothing-to-scan case and
   * returns trusted with no findings so the normal push proceeds; a null floor
   * (unreadable/unresolved/diverged) fails OPEN to the GH013 backstop.
   *
   * WHY the BARE, and why the silencers are disabled: GitHub Push Protection (GH013) ignores
   * `.gitleaks.toml`, `.gitleaksignore` and inline `//gitleaks:allow`, so a scan that honored
   * any of them would go green on a secret GitHub still rejects (a vacuous fix). We disable all
   * three: (1) scanning the worker BARE (no working tree) means a repo-shipped `.gitleaks.toml`
   * / `.gitleaksignore` cannot be auto-discovered from disk; (2) an EXPLICIT `-c` config forces
   * gitleaks' embedded default ruleset and skips `.gitleaks.toml` auto-discovery; (3)
   * `--ignore-gitleaks-allow` disables inline allow comments. Proven against all three silencers
   * (issue #974 step 5). The bare's git config is WORKER-authored (not attacker-controlled), so
   * gitleaks' internal `git log -p` cannot fire an attacker-chosen diff driver.
   *
   * TRUST: gitleaks prints "no leaks found" rc 0 on an unresolved/empty range, so a clean
   * verdict is meaningless unless the scan actually walked the range. scanIsTrustworthy is that
   * liveness gate (exec ok, no error token on stderr, the "N commits scanned" line present AND
   * equal to the range length). The caller acts on findings ONLY when trusted; when untrusted it
   * fails OPEN and relies on the GH013 remote backstop.
   *
   * The temp config + JSON report live under os.tmpdir() (worker-writable; the runner's 0700 tmp
   * is not) and are removed in a finally. gitleaks runs under `--exit-code 0`, so a finding is
   * still exit 0 and any nonzero exit is an INSTRUMENT failure (execOk = !error).
   */
  async secretScanRange(
    barePath: string,
    trackingRef: string,
    branch: string,
    creds?: { pat?: string; cloneUrl?: string; username?: string },
  ): Promise<{ trusted: boolean; findings: SecretFinding[] }> {
    // Resolve the range base INSIDE the fail-open envelope: resolvePublicationFloor never
    // throws (it returns null on any unreadable/unresolved/diverged case), because an uncaught
    // throw here would escape to the generic catch and report `failed` with NO preserved_patch —
    // the exact work-loss this feature prevents. A null floor fails open like every other setup
    // step below. The floor is the current FRESH remote branch tip on a resumed branch and the
    // default branch on a first push (issue #1398), so the trust gate below covers the real
    // push delta rather than re-scanning already-pushed history.
    const base = await this.resolvePublicationFloor(barePath, trackingRef, branch, creds);
    if (base === null) {
      this.log.warn(
        "finalize secret scan: could not resolve a trustworthy publication floor; failing open",
        { barePath },
      );
      return { trusted: false, findings: [] };
    }
    return this.scanLogRange(barePath, `${base}..${trackingRef}`, {
      label: "finalize secret scan",
      timeoutMs: GIT_TIMEOUT_MS,
      onUntrusted: "failing open",
    });
  }

  /**
   * The credential-free finalize core of {@link secretScanRange}, its only caller (the checkpoint
   * scan, {@link secretScanCheckpointRange}, runs gitleaks through runCheckpointGitleaks instead):
   * count `logRange`, run gitleaks over it in the bare with the silencers disabled, read the
   * size-capped report and apply the liveness gate. `label` prefixes every log line and
   * `onUntrusted` ends the untrusted ones (the finalize wording is byte-identical to before the split).
   */
  private async scanLogRange(
    barePath: string,
    logRange: string,
    opts: { label: string; timeoutMs: number; onUntrusted: string },
  ): Promise<{ trusted: boolean; findings: SecretFinding[] }> {
    let expectedCommits = 0;
    try {
      const out = await this.runGit(barePath, ["rev-list", "--count", logRange]);
      expectedCommits = Number.parseInt(out.trim(), 10);
      if (Number.isNaN(expectedCommits)) expectedCommits = 0;
    } catch {
      // A failed count is an untrusted scan setup — do NOT block; fail open to the backstop.
      this.log.warn(`${opts.label}: could not count the push range; ${opts.onUntrusted}`, {
        barePath,
      });
      return { trusted: false, findings: [] };
    }
    if (expectedCommits === 0) {
      // Nothing to scan (the push carries no new commit); the normal push proceeds.
      return { trusted: true, findings: [] };
    }

    const scratch = os.tmpdir();
    const configPath = path.join(scratch, `uzi-gl-config-${randomUUID()}.toml`);
    const reportPath = path.join(scratch, `uzi-gl-report-${randomUUID()}.json`);
    try {
      // `[extend] useDefault=true` forces gitleaks' embedded default ruleset and, being an
      // explicit `-c`, skips auto-discovery of the target repo's `.gitleaks.toml`.
      await fs.writeFile(configPath, "[extend]\nuseDefault = true\n", "utf8");
      const args = gitleaksArgs({ sourcePath: barePath, logRange, configPath, reportPath });
      // gitleaks (baked on PATH by the worker image, see agent/templates/*/Dockerfile). Do NOT
      // throw on a nonzero exit: under --exit-code 0 a finding is exit 0, so any nonzero is an
      // instrument failure captured as `error` and folded into the trust gate.
      let execOk = true;
      let stderr = "";
      try {
        const res = await this.execScoped(this.gitleaksBin, args, {
          // gitEnv(): the hardened REPLACEMENT env (no join token / API URL, plus the
          // core.hooksPath / GIT_CONFIG_NOSYSTEM / global=/dev/null pins), so gitleaks'
          // internal `git -C … log -p` runs WITHOUT worker credentials in its environment and
          // cannot fire a planted hook or read a system/global config as the worker uid. gitEnv
          // carries PATH+HOME (+TMPDIR), so gitleaks itself still runs; it needs no secret env.
          env: gitEnv(),
          maxBuffer: GIT_MAX_BUFFER,
          timeout: opts.timeoutMs,
        });
        stderr = res.stderr ?? "";
      } catch (err) {
        execOk = false;
        stderr = typeof (err as { stderr?: unknown }).stderr === "string"
          ? (err as { stderr: string }).stderr
          : gitErrorMessage(err);
      }

      // Read + parse the report; a failed read/parse (fs error, truncated file) is UNTRUSTED —
      // return trusted:false rather than a clean verdict on an unreadable report. SIZE-CAP the
      // read first: the report is O(findings) over ATTACKER-authored commits, so a committed
      // file of millions of secret-shaped lines could balloon it to multiple GB and OOM the
      // worker. An over-cap report is an untrusted scan (fail open to the GH013 backstop), never
      // an OOM. parseGitleaksReport additionally caps the number of findings it materialises.
      let findings: SecretFinding[];
      try {
        const st = await fs.stat(reportPath);
        if (st.size > SECRET_SCAN_REPORT_MAX_BYTES) {
          this.log.warn(
            `${opts.label}: gitleaks report exceeds the size cap; ${opts.onUntrusted}`,
            { barePath, bytes: st.size, cap: SECRET_SCAN_REPORT_MAX_BYTES },
          );
          return { trusted: false, findings: [] };
        }
        const raw = await fs.readFile(reportPath, "utf8");
        if (!gitleaksReportWellFormed(raw)) throw new Error("malformed gitleaks report");
        findings = parseGitleaksReport(raw);
      } catch {
        this.log.warn(`${opts.label}: could not read the gitleaks report; ${opts.onUntrusted}`, {
          barePath,
        });
        return { trusted: false, findings: [] };
      }

      const scannedCommits = commitsScannedFromStderr(stderr);
      const trusted = scanIsTrustworthy({ stderr, scannedCommits, expectedCommits, execOk });
      this.log.debug(`${opts.label} complete`, {
        barePath,
        expectedCommits,
        scannedCommits,
        execOk,
        trusted,
        findings: findings.length,
      });
      return { trusted, findings };
    } finally {
      await fs.rm(configPath, { force: true }).catch(() => undefined);
      await fs.rm(reportPath, { force: true }).catch(() => undefined);
    }
  }

  /**
   * PRD #456 M1 — fetch the CURRENT default-branch tip from origin into the worker bare
   * and return its SHA. This is a WORKER-uid AUTHENTICATED op (mirrors `fetch` at the top
   * of this class): the worker owns the bare and holds the PAT, and the agent never has a
   * push/fetch credential, so this cannot run runner-uid.
   *
   * Why a fresh fetch at all: the finalize `fetchAgentBranch` is a LOCAL `file://` fetch,
   * and the last origin contact was at claim time (`ensureClone`), so
   * `refs/remotes/origin/<default>` in the bare still holds the CLAIM-TIME tip. The align
   * target must be main's tip AS IT IS NOW (main may have advanced its `.github/workflows/**`
   * since the clone base), so we re-fetch it here, immediately before the align.
   *
   * The returned SHA is the align TARGET and is the FRESHLY-FETCHED tip — NEVER
   * `defaultBranchRef`'s frozen-mirror fallback rungs (N2): those can resolve to a stale
   * `refs/heads/main` mirror fixed at first clone, which is exactly what this method exists
   * to bypass. We read `refs/remotes/origin/<default>` (which this fetch just updated),
   * falling back to `FETCH_HEAD`, and validate a 40-hex OID before returning.
   */
  async fetchDefaultTip(
    barePath: string,
    defaultBranch: string,
    pat?: string,
    cloneUrl?: string,
    username?: string,
    onLockAcquired?: () => void,
  ): Promise<string> {
    const scope = cloneUrl ? httpScopeForUrl(cloneUrl) : undefined;
    return this.withLock(barePath, async () => {
      onLockAcquired?.();
      await this.runGit(barePath, ["fetch", "origin", defaultBranch], pat, scope, username);
      // The configured refspec (+refs/heads/*:refs/remotes/origin/*) means a named-branch
      // fetch updates the remote-tracking ref; read the fresh tip off it.
      let sha = (
        await this.runGit(barePath, [
          "rev-parse",
          "--verify",
          `refs/remotes/origin/${defaultBranch}^{commit}`,
        ]).catch(() => "")
      ).trim();
      if (!/^[0-9a-f]{40}$/.test(sha)) {
        // Fallback: some server/refspec shapes leave only FETCH_HEAD current.
        sha = (
          await this.runGit(barePath, ["rev-parse", "--verify", "FETCH_HEAD^{commit}"]).catch(
            () => "",
          )
        ).trim();
      }
      if (!/^[0-9a-f]{40}$/.test(sha)) {
        throw new Error(
          `fetchDefaultTip: could not resolve a 40-hex tip for origin/${defaultBranch}`,
        );
      }
      return sha;
    });
  }

  /**
   * PRD #1296 M3 (D5) — produce a REAL, independently-usable Git bundle carrying the
   * original committed head H under a single named ref, from the trusted WORKER bare.
   *
   * This is a bare-OBJECT operation only: NO source checkout, NO repo-controlled
   * hooks/filters (gitEnv pins core.hooksPath at a root-owned 0555 dir), and it exports
   * EXACTLY one ref (RECOVERY_BUNDLE_REF at H) — never `--all`, unrelated refs, reflogs,
   * filesystem paths or worker config (D1/D6). It cannot capture uncommitted files or the
   * worker HOME: a bundle is an object graph, and only committed objects reachable from H
   * are included.
   *
   * Prerequisite resolution (D5): when `forgeTip` (a FRESH, verified forge default tip the
   * caller fetched under the reap-before-credentialed-git boundary) is supplied and shares
   * a real merge-base with H, that merge-base is the bundle's prerequisite — so a user's
   * CLEAN forge clone (which already has the merge-base) can import it. The runner clone's
   * private `origin/main`, a prior checkpoint wrapper, or an unpublished private base are
   * NEVER used by the legacy `forgeTip` path. The guarded path first attempts a full
   * bundle; only on oversize may it use the direct worker bare's cached default ref.
   * Those cached prerequisites are header-derived and require retained local custody,
   * since they do not prove current forge reachability. When no forge-reachable prerequisite
   * exists (unrelated histories, or H is
   * already fully on the forge), it falls back to a SELF-CONTAINED bundle within the size
   * limit; if that exceeds RECOVERY_MAX_BUNDLE_BYTES it throws RecoveryBundleTooLargeError
   * so the caller retains custody and surfaces needs_action rather than truncating. The size
   * limit is enforced WHILE writing ({@link streamBundleWithCap}): the git child is killed and the
   * partial file removed as soon as the bytes written pass the limit.
   *
   * The bundle is `git bundle verify`d against the bare (a producer self-check that its
   * prerequisites resolve) before the size/checksum are recorded. The transient named ref
   * is created and deleted under the SAME bare lock, so a concurrent op never observes it.
   */
  async produceRecoveryBundle(
    barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; guardedDefaultBranch?: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult> {
    if (opts.guardedDefaultBranch !== undefined && opts.forgeTip !== undefined) {
      throw new Error("guardedDefaultBranch and forgeTip are mutually exclusive");
    }
    const maxBytes = opts.maxBytes ?? RECOVERY_MAX_BUNDLE_BYTES;
    return this.withLock(barePath, async () => {
      if (opts.guardedDefaultBranch !== undefined) await this.assertOwedBare(barePath);
      const graphGit = (args: string[]): Promise<string> => this.runGit(barePath, ["--no-replace-objects", ...args]);
      // Resolve + verify H is a real commit present in the trusted bare. Never trust a
      // caller-supplied SHA blindly; a missing object here means the source is not
      // reproducible and the caller must surface needs_action.
      const h = (
        await graphGit(["rev-parse", "--verify", `${opts.sourceSha}^{commit}`]).catch(
          () => "",
        )
      ).trim();
      if (!/^[0-9a-f]{40}$/.test(h)) {
        throw new Error("produceRecoveryBundle: source commit is not present in the trusted bare");
      }
      // Resolve the prerequisite against the FRESH forge tip only. merge-base guarantees the
      // result is an ancestor of BOTH H and forgeTip, i.e. it is genuinely forge-reachable.
      // When merge-base == H, H is already fully on the forge (already published) — there is
      // nothing to archive; report that so the caller releases custody against verified forge
      // history rather than shipping a redundant full-history bundle.
      let prereqs: string[] = [];
      if (opts.forgeTip && /^[0-9a-f]{40}$/.test(opts.forgeTip)) {
        const mb = (
          await graphGit(["merge-base", h, opts.forgeTip]).catch(() => "")
        ).trim();
        if (/^[0-9a-f]{40}$/.test(mb)) {
          if (mb === h) {
            return {
              bundlePath: "",
              byteSize: 0,
              checksum: "",
              chunkCount: 0,
              prerequisiteShas: [],
              sourceSha: h,
              selfContained: false,
              alreadyPublished: true,
            };
          }
          prereqs = [mb];
        }
      }
      // Create the transient named ref at H, build the bundle from EXACTLY that ref, verify
      // it, then delete the ref in a finally so the bare's namespace is left untouched.
      await this.runGit(barePath, ["update-ref", RECOVERY_BUNDLE_REF, h]);
      try {
        // `-` writes the bundle to stdout so the bytes actually written are counted and capped.
        const produce = async (): Promise<void> => {
          const args = ["--no-replace-objects", "bundle", "create", "-", RECOVERY_BUNDLE_REF];
          for (const p of prereqs) args.push(`^${p}`);
          await this.streamBundleWithCap(barePath, args, opts.outPath, maxBytes);
        };
        try {
          await produce();
        } catch (original) {
          if (!(original instanceof RecoveryBundleTooLargeError) || opts.guardedDefaultBranch === undefined) throw original;
          // Only the direct worker cache is admissible. No network or private runner ref is
          // consulted. Invalid, absent or unrelated cache leaves the original oversized outcome.
          try {
            const branch = opts.guardedDefaultBranch;
            if (!await this.isPlainBranchName(barePath, branch)) throw original;
            const ref = `refs/remotes/origin/${branch}`;
            await graphGit(["check-ref-format", ref]);
            const tip = (await graphGit(["show-ref", "--verify", "--hash", ref])).trim();
            const direct = (await graphGit(["for-each-ref", "--format=%(refname)|%(objectname)|%(objecttype)|%(symref)|END", ref])).trim();
            if (direct !== `${ref}|${tip}|commit||END`) throw original;
            if (!/^[0-9a-f]{40}$/.test(tip) || (await graphGit(["cat-file", "-t", tip])).trim() !== "commit") throw original;
            const bases = (await graphGit(["merge-base", "--all", h, tip])).trim().split("\n");
            if (!bases.length || bases.some(sha => !/^[0-9a-f]{40}$/.test(sha))) throw original;
            prereqs = [...new Set(bases)].sort();
            if (prereqs.includes(h)) {
              const parents = (await graphGit(["rev-list", "--parents", "-n", "1", h])).trim().split(" ");
              if (parents.shift() !== h) throw original;
              prereqs = [...new Set(parents)].sort();
              for (const sha of prereqs) {
                if (!/^[0-9a-f]{40}$/.test(sha) || (await graphGit(["cat-file", "-t", sha])).trim() !== "commit") throw original;
              }
            }
            if (!prereqs.length) throw original;
          } catch { throw original; }
          await produce();
        }
        await graphGit(["bundle", "verify", opts.outPath]);
        // Git may advertise boundary commits other than the selected exclusions.
        prereqs = (await readRecoveryBundleHeader(opts.outPath, h)).prerequisiteShas;
      } catch (err) {
        await fs.rm(opts.outPath, { force: true });
        throw err;
      } finally {
        await this.tryGit(barePath, ["update-ref", "-d", RECOVERY_BUNDLE_REF]);
      }
      // Stream the produced file to compute size + SHA-256 without buffering the whole
      // (up to 64 MiB) bundle in memory.
      const hash = createHash("sha256");
      let byteSize = 0;
      await new Promise<void>((resolve, reject) => {
        const rs = createReadStream(opts.outPath);
        rs.on("data", (c: Buffer | string) => {
          const buf = Buffer.isBuffer(c) ? c : Buffer.from(c);
          byteSize += buf.length;
          hash.update(buf);
        });
        rs.on("error", reject);
        rs.on("end", resolve);
      });
      if (byteSize > maxBytes) {
        await fs.rm(opts.outPath, { force: true });
        throw new RecoveryBundleTooLargeError(byteSize, maxBytes);
      }
      const checksum = hash.digest("hex");
      const chunkCount = Math.max(1, Math.ceil(byteSize / RECOVERY_CHUNK_BYTES));
      return {
        bundlePath: opts.outPath,
        byteSize,
        checksum,
        chunkCount,
        prerequisiteShas: prereqs,
        sourceSha: h,
        selfContained: prereqs.length === 0,
        alreadyPublished: false,
      };
    });
  }

  /**
   * issue #1742 — run `git bundle create - ...` (bundle on stdout) and write it to `outPath`
   * through a byte counter that is a HARD bound on the bytes written: the moment the count passes
   * `maxBytes` the git child is killed, the partial file is removed and
   * {@link RecoveryBundleTooLargeError} (with the count seen so far) is thrown. No chunk that
   * would push the file past `maxBytes` is ever written. Same worker-uid credential-free git env
   * as {@link runGit} (via {@link spawnGit}), no shell, and the same GIT_TIMEOUT_MS bound.
   */
  private async streamBundleWithCap(barePath: string, args: string[], outPath: string, maxBytes: number): Promise<void> {
    const { child, stdout, exited } = await this.spawnGit(barePath, args, undefined, { timeoutMs: GIT_TIMEOUT_MS });
    let seen = 0;
    const counter = new Transform({
      transform(chunk: Buffer, _enc, cb) {
        seen += chunk.length;
        if (seen > maxBytes) cb(new RecoveryBundleTooLargeError(seen, maxBytes));
        else cb(null, chunk);
      },
    });
    const killChild = (): void => {
      if (child && child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    };
    const timer = setTimeout(() => {
      killChild();
      counter.destroy(new Error(`git ${args.join(" ")} exceeded ${GIT_TIMEOUT_MS}ms`));
    }, GIT_TIMEOUT_MS);
    try {
      await pipeline(stdout, counter, createWriteStream(outPath, { mode: 0o600 }));
    } catch (err) {
      killChild();
      stdout.destroy();
      await exited;
      await fs.rm(outPath, { force: true });
      throw err;
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * PRD #456 M1 (D1) — true iff the branch tip's `.github/workflows/` tree DIFFERS from the
   * freshly-fetched default tip. This is the precise trigger for the finalize align: only a
   * behind-on-workflows branch (main moved those files after the clone base, or vice versa)
   * needs realigning, and this keeps the align/conflict surface minimal.
   *
   * WORKER-uid, bare-only. A TWO-dot direct tree compare (`trackingRef` vs `defaultTip`), NOT
   * three-dot — we want "do these two trees' workflow files differ right now", not "what did
   * the branch change since a merge base". `--name-only` correctly sidesteps the pinned
   * `diff.external` code-exec neutralizer (see GIT_CODE_EXEC_KEY_PINS / `changedFiles`), so no
   * `--no-ext-diff` is needed. Ordinary errors return false (FAIL-OPEN to the normal push);
   * boundary cancellation and deadlines propagate.
   */
  async workflowTreeDiffers(
    barePath: string,
    trackingRef: string,
    defaultTip: string,
  ): Promise<boolean> {
    try {
      const out = await this.runGit(barePath, [
        "diff",
        "--name-only",
        trackingRef,
        defaultTip,
        "--",
        ".github/workflows/",
      ]);
      return out.trim() !== "";
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return false;
    }
  }

  /**
   * issue #1769 — make `tip` (the fresh default `fetchDefaultTip` brought into the worker bare)
   * resolvable in the runner clone, so {@link alignBranchWithDefault} can anchor and merge it.
   *
   * A `--shared` (Claude) clone already reaches it through its alternate: the first probe hits
   * and nothing is spawned. A self-contained (Codex) clone has no alternate, and inside the
   * Codex finalize boundary its runner git runs in the command sandbox rooted at the clone,
   * which cannot read the bare. So the objects are STREAMED across instead: a worker-uid
   * `pack-objects --revs --stdout` in the bare (via {@link spawnGit}: credential-free base
   * gitEnv, NO PAT, and a full non-thin pack — no `--thin`) piped into a runner-uid
   * `index-pack --stdin` in the clone. `haves` (commits the clone already holds, e.g. its base
   * and default-branch commits) become `--not` exclusions, after filtering to the ones the bare
   * actually has: `pack-objects --revs` exits 128 on an unknown `--not` object.
   *
   * Ordering inside a boundary: the safety facade admits a `worker_pat` boundary process only
   * while admission is closed and the registry reports no live `command`-kind (model tool) root.
   * Every process this method starts through the boundary, the `cat-file` probes and the
   * `command`-identity consumer included, is registered as a `boundary_action` root, which that
   * guard does not count, so none of them can trip it. The `worker_pat` producer is spawned
   * first all the same, so no runner-identity process of this import is alive when it starts.
   *
   * Both exit statuses are awaited (inside a boundary each completion includes its root's
   * reap). If either side fails or the pipe breaks, the side that failed first is named in the
   * error, the other side is torn down and awaited, and a {@link RunnerCloneImportError} is
   * thrown. How a torn-down side actually ends depends on where it runs:
   *  - outside a boundary, the producer's stdout is destroyed and its child SIGKILLed (the
   *    destroyed stdout's abandon hook in spawnGit may also send it a SIGTERM, before or after
   *    the SIGKILL, while it is still live); the consumer's stdin is destroyed and its child
   *    SIGKILLed;
   *  - inside a boundary, the handle exposes no way to signal the root, so the producer's stdout
   *    is only DESTROYED: `pack-objects` ends at its next write (EPIPE), and a producer that
   *    writes nothing more (still counting objects under `-q`) keeps running until the boundary
   *    deadline aborts the wait and the registry reaps (disposes) its root; a reap that is not
   *    clean poisons the registry. The consumer's stdin is destroyed, so `index-pack` reads EOF
   *    and exits.
   * Either way this method returns only once both exit statuses have settled. On success the
   * clone is re-probed for `tip^{commit}`, and a miss is also an import error.
   */
  async ensureRunnerCloneObjects(barePath: string, clonePath: string, tip: string, haves: string[]): Promise<void> {
    const fail = (cause: string): RunnerCloneImportError => {
      this.log.warn("runner clone: could not import default-tip objects", { path: clonePath, tip, error: cause });
      return new RunnerCloneImportError(clonePath, tip, cause);
    };
    const cloneHasTip = async (): Promise<boolean> => {
      try {
        await this.runGitAsRunner(clonePath, ["cat-file", "-e", `${tip}^{commit}`]);
        return true;
      } catch {
        return false;
      }
    };
    // `tip` and every have are written to pack-objects' rev-list stdin one per line, so only a
    // plain object id is accepted there (never an option-shaped or multi-line value). Checked
    // before the probe too, so a symbolic tip (`HEAD`) the clone happens to resolve is refused
    // rather than silently accepted as already present.
    if (!SHA40_RE.test(tip)) throw fail("tip is not a full object id");
    if (await cloneHasTip()) return;
    const present: string[] = [];
    for (const have of new Set(haves)) {
      if (!have || !SHA40_RE.test(have)) continue;
      if ((await this.tryGit(barePath, ["cat-file", "-e", `${have}^{commit}`])) === 0) present.push(have);
    }
    const revs = `${tip}\n${present.length > 0 ? `--not\n${present.map((h) => `${h}\n`).join("")}` : ""}`;
    const started = Date.now();
    let producer: Awaited<ReturnType<GitCache["spawnGit"]>>;
    try {
      producer = await this.spawnGit(barePath, ["pack-objects", "--revs", "--stdout", "-q"], revs);
    } catch (err) {
      throw fail(`pack-objects could not start: ${gitErrorMessage(err)}`);
    }
    // Which side we tore down because its peer failed, so the error names the side that failed
    // first rather than the one we stopped.
    let producerStopped = false;
    let consumerStopped = false;
    const killProducer = (): void => {
      producer.stdout.destroy();
      if (producer.child && producer.child.exitCode === null && producer.child.signalCode === null) {
        producer.child.kill("SIGKILL");
      }
    };
    const stopProducer = (): void => {
      producerStopped = true;
      killProducer();
    };
    // Which stream errored FIRST: `pipeline` destroys every stream with the first error, so the
    // side that broke is the one whose error event fired before the other's. Only that first
    // producer error is kept as its message (spawnGit destroys stdout with git's stderr on a
    // nonzero exit); a consumer error pipeline copied onto stdout is not the producer's cause.
    let firstBroken: "producer" | "consumer" | undefined;
    let producerError = "";
    producer.stdout.on("error", (err: unknown) => {
      firstBroken ??= "producer";
      if (firstBroken === "producer") producerError ||= gitErrorMessage(err);
    });
    let consumer: Awaited<ReturnType<GitCache["spawnGitAsRunnerWithStdin"]>>;
    try {
      consumer = await this.spawnGitAsRunnerWithStdin(clonePath, ["index-pack", "--stdin"]);
    } catch (err) {
      killProducer();
      await producer.exited;
      throw fail(`index-pack could not start: ${gitErrorMessage(err)}`);
    }
    // Either side exiting nonzero tears the pipe down at once, so the other side sees EOF /
    // EPIPE (or is killed) instead of waiting on a peer that is gone.
    const stopConsumer = (): void => {
      consumerStopped = true;
      consumer.abort();
    };
    consumer.stdin.on("error", () => {
      firstBroken ??= "consumer";
    });
    const producerExit = producer.exited.then((code) => {
      if (code !== 0 && !producerStopped) stopConsumer();
      return code;
    });
    const consumerExit = consumer.exited.then((res) => {
      if (res.code !== 0 && !consumerStopped) stopProducer();
      return res;
    });
    const piped = pipeline(producer.stdout, consumerInputSink(consumer.stdin)).then(
      () => undefined,
      (err: unknown) => {
        // Attribute the break unless an exit handler already did: the producer when its stream
        // errored first, otherwise the consumer (an EPIPE on its stdin). A consumer stdin that
        // closes with no error is not a break here (consumerInputSink): the consumer's exit
        // status judges it. Then tear both sides down.
        if (!producerStopped && !consumerStopped) {
          if (firstBroken === "producer") stopConsumer();
          else stopProducer();
        }
        killProducer();
        consumer.abort();
        return err;
      },
    );
    const [pipeError, producerCode, consumerResult] = await Promise.all([piped, producerExit, consumerExit]);
    const failures: string[] = [];
    const producerFailure = producerCode !== 0
      ? `pack-objects exited ${producerCode}${producerError ? `: ${producerError}` : ""}`
      : undefined;
    const consumerFailure = consumerResult.code !== 0
      ? `index-pack exited ${consumerResult.code}${consumerResult.stderr ? `: ${consumerResult.stderr}` : ""}`
      : undefined;
    // The side that failed on its own first; the side we stopped after it, marked as such.
    if (producerFailure && !producerStopped) failures.push(producerFailure);
    if (consumerFailure && !consumerStopped) failures.push(consumerFailure);
    if (producerFailure && producerStopped) failures.push(`${producerFailure} (stopped after its peer failed)`);
    if (consumerFailure && consumerStopped) failures.push(`${consumerFailure} (stopped after its peer failed)`);
    if (failures.length > 0) throw fail(failures.join("; "));
    if (pipeError !== undefined) {
      // Both sides exited 0 here, yet the stream between them broke: name the side whose stream
      // errored first and both (settled) exit statuses, so the log says where to look.
      const side = firstBroken ? `${firstBroken} side` : "side unknown";
      throw fail(
        `pack stream failed (${side}; pack-objects exited ${producerCode}, index-pack exited ${consumerResult.code}): ${gitErrorMessage(pipeError)}`,
      );
    }
    if (!(await cloneHasTip())) throw fail("the imported pack did not make the tip resolvable in the clone");
    this.log.info("runner clone: imported default-tip objects", {
      path: clonePath,
      tip,
      haves: present.length,
      duration_ms: Date.now() - started,
    });
  }

  /**
   * PRD #456 M1 (B3) — align the run's branch with the fresh default IN THE RUNNER CLONE,
   * never the worker bare. The clone is the ONLY working tree at finalize, and it is
   * RUNNER-owned, so every git op here runs runner-uid (`runGitAsRunner`) — a worker-uid op
   * in the runner clone would hit the B2 dubious-ownership boundary.
   *
   * `baseTip` is the agent's own committed tip BEFORE any align (captured by the caller from
   * `branchTip`). We reset the clone's branch to it before each strategy so (a) leftover
   * UNCOMMITTED artifacts cannot block the op, and (b) a rebase FALLBACK after a clean merge
   * replays the ORIGINAL agent commits rather than the merge commit the merge left on
   * `refs/heads/<branch>`. The committed work is never lost — it lives in the commit object;
   * we only move the branch ref back to it. (This `baseTip` parameter is a small, deliberate
   * deviation from the spec's 4-arg signature; without it a merge→rebase fallback would rebase
   * the merge commit and the S3 commit-count assertion would spuriously fire.)
   *
   * `defaultTip`'s objects are in the worker bare (it received them in `fetchDefaultTip`). A
   * `--shared` (Claude) clone reaches them via its alternate; in a self-contained (Codex) clone,
   * which has no alternate (issue #1769), the caller first imports them with
   * {@link ensureRunnerCloneObjects}. Either way we anchor them under a fixed LOCAL ref via
   * `update-ref` (no `file://` fetch, no PAT, no protocol.file.allow concern) and merge/rebase
   * against that ref, deleting it in a `finally`.
   *
   * `git clean -fd`, NOT `-fdx`: `-x` also deletes IGNORED files (node_modules, build outputs,
   * a local .env), which are not part of the committed work and may be needed / expensive to
   * recreate; removing untracked-but-tracked-eligible files (`-fd`) is enough to unblock a
   * merge/rebase, which only refuses when an UNtracked file would be overwritten.
   *
   * Returns `"aligned"` on a clean result. On a genuine CONFLICT it runs the matching abort
   * and returns `"conflict"`; an unrelated (non-conflict) git failure is RETHROWN. For a
   * rebase it uses `--empty=keep --no-autosquash` and asserts the branch's own commit count is
   * preserved across the replay (S3) — a silent drop throws rather than pushing truncated work.
   *
   * `"workflow-subtree"` (issue #627) is the narrow PRIMARY strategy: it overlays ONLY the
   * default tip's `.github/workflows/` subtree onto the agent tip (making that subtree equal
   * the default's, deletions included) and never conflicts, so it has NO `"conflict"` return —
   * a genuine git error propagates so the caller can fall back to merge/rebase. See its arm.
   */
  async alignBranchWithDefault(
    clonePath: string,
    branch: string,
    baseTip: string,
    defaultTip: string,
    strategy: "merge" | "rebase" | "workflow-subtree",
  ): Promise<"aligned" | "conflict"> {
    const targetRef = "refs/uzi-align/target";
    try {
      // Anchor the fresh default under a local ref (objects reachable via the --shared
      // alternate, or, in a self-contained Codex clone, already imported by
      // ensureRunnerCloneObjects). Not under refs/heads/* so it never pollutes the branch namespace.
      await this.runGitAsRunner(clonePath, ["update-ref", targetRef, defaultTip]);
      // Rewind to the pre-align committed agent tip: clears uncommitted scratch AND undoes a
      // prior merge's commit so each strategy starts from the original agent work.
      await this.runGitAsRunner(clonePath, ["checkout", "--force", branch]);
      await this.runGitAsRunner(clonePath, ["reset", "--hard", baseTip]);
      await this.runGitAsRunner(clonePath, ["clean", "-fd"]);

      if (strategy === "workflow-subtree") {
        // PRD #456 (issue #627) — the PRIMARY, narrow strategy. Overlay ONLY the default
        // tip's `.github/workflows/` subtree onto the agent tip so the branch's workflow tree
        // becomes BYTE-FOR-BYTE equal to the default's (all GitHub's tip-vs-default check
        // needs) WITHOUT dragging in any unrelated change on main. It cannot conflict — the
        // caller gates on the branch never having modified a workflow file (Part B) — so there
        // is no abort/`"conflict"` return here. A genuine git error PROPAGATES (the caller
        // falls back to merge/rebase); only "there are staged changes" is treated as non-error.
        //
        // 1. Remove the branch's entire workflow set from index+worktree (--ignore-unmatch so
        //    it is a no-op when the branch has no such path).
        await this.runGitAsRunner(clonePath, [
          "rm",
          "-r",
          "--ignore-unmatch",
          ".github/workflows",
        ]);
        // 2. Restore the default's files ONLY when the default actually has a workflows tree.
        //    An empty ls-tree means the default deleted its whole `.github/workflows/`; a bare
        //    `checkout <ref> -- .github/workflows` would ERROR there, so skip it — the rm above
        //    already achieved the equalization ("remove them all"). rm-then-restore makes the
        //    index at that path equal the default's tree exactly (extras gone, missing added).
        const defaultWfTree = (
          await this.runGitAsRunner(clonePath, ["ls-tree", targetRef, "--", ".github/workflows"])
        ).trim();
        if (defaultWfTree.length > 0) {
          await this.runGitAsRunner(clonePath, ["checkout", targetRef, "--", ".github/workflows"]);
        }
        // 3. If nothing is staged the branch was already equal — make NO commit and return
        //    aligned (the normal push then proceeds). `diff --cached --name-only` exits 0
        //    whether or not there are changes (unlike `--quiet`), so its stdout is unambiguous.
        const staged = (
          await this.runGitAsRunner(clonePath, ["diff", "--cached", "--name-only"])
        ).trim();
        if (staged.length === 0) {
          return "aligned";
        }
        // 4. A single overlay commit on top of baseTip — a fast-forward, so the original agent
        //    SHAs are preserved and nothing is rebased. Pass an ident explicitly so the commit
        //    succeeds even if the clone lacks user.name/email (matches how the tests commit).
        await this.runGitAsRunner(clonePath, [
          "-c",
          `user.name=${AGENT_GIT_IDENTITY.name}`,
          "-c",
          `user.email=${AGENT_GIT_IDENTITY.email}`,
          "-c",
          "commit.gpgsign=false",
          "commit",
          "-m",
          `chore: align .github/workflows with ${defaultTip}`,
        ]);
        return "aligned";
      }

      if (strategy === "merge") {
        try {
          await this.runGitAsRunner(clonePath, ["merge", "--no-edit", targetRef]);
          return "aligned";
        } catch (err) {
          if (await this.inMerge(clonePath)) {
            await this.runGitAsRunner(clonePath, ["merge", "--abort"]).catch(() => undefined);
            return "conflict";
          }
          throw err; // a non-conflict git failure — not ours to swallow.
        }
      }

      // rebase: count the branch's own commits (ahead of the target) before and after so a
      // silently dropped commit is caught (S3). `--reapply-cherry-picks` re-applies commits
      // whose change already landed on the target (git drops these by default), so a branch
      // whose work overlaps main's new commits keeps ITS commits and the count stays honest —
      // otherwise the S3 count guard would false-trip on safe, landable work.
      const before = await this.countAhead(clonePath, defaultTip, branch);
      try {
        await this.runGitAsRunner(clonePath, [
          "rebase",
          "--empty=keep",
          "--no-autosquash",
          "--reapply-cherry-picks",
          targetRef,
        ]);
      } catch (err) {
        if (await this.inRebase(clonePath)) {
          await this.runGitAsRunner(clonePath, ["rebase", "--abort"]).catch(() => undefined);
          return "conflict";
        }
        throw err; // a non-conflict git failure — rethrow.
      }
      const after = await this.countAhead(clonePath, defaultTip, branch);
      if (after < before) {
        throw new Error(
          `alignBranchWithDefault: rebase dropped commits (${before} → ${after}) — refusing to push truncated work`,
        );
      }
      return "aligned";
    } finally {
      await this.runGitAsRunner(clonePath, ["update-ref", "-d", targetRef]).catch(
        () => undefined,
      );
    }
  }

  /** True while a merge is in progress in `clonePath` (a conflict left it mid-merge).
   *  Checked via the on-disk `.git/MERGE_HEAD` marker rather than a git command so it is
   *  uid-independent (the clone is runner-owned) and cannot be confused by a non-conflict
   *  git failure. The runner clone's `.git` is a real directory (a `clone --shared`). */
  private async inMerge(clonePath: string): Promise<boolean> {
    return pathExists(path.join(clonePath, ".git", "MERGE_HEAD"));
  }

  /** True while a rebase is in progress in `clonePath` (a conflict paused the replay).
   *  Either backend leaves its state dir behind: `rebase-merge` (the default merge backend)
   *  or `rebase-apply` (the am backend). Marker-file check for the same reasons as inMerge. */
  private async inRebase(clonePath: string): Promise<boolean> {
    return (
      (await pathExists(path.join(clonePath, ".git", "rebase-merge"))) ||
      (await pathExists(path.join(clonePath, ".git", "rebase-apply")))
    );
  }

  /** Count of commits reachable from `tip` but not from `base`, runner-uid in the clone.
   *  Any failure answers 0 — the caller only compares before-vs-after, and a symmetric
   *  failure (0 == 0) simply skips the assertion rather than false-failing a good rebase. */
  private async countAhead(clonePath: string, base: string, tip: string): Promise<number> {
    const out = await this.runGitAsRunner(clonePath, [
      "rev-list",
      "--count",
      `${base}..${tip}`,
    ]).catch(() => "0");
    const n = Number.parseInt(out.trim(), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  private async cloneBare(repoUrl: string, dest: string, pat?: string, scope?: string, username?: string): Promise<void> {
    // Widen the cleanup over the WHOLE body: a transient blip on the post-clone
    // authenticated fetch (or the config/disableAutoMaintenance steps) must also rm
    // `dest` before rethrowing. Otherwise a retry would run `git clone --bare` into a
    // non-empty dir → a deterministic "already exists" failure (permanent), converting a
    // transient into a permanent one. This guarantees each retry starts from a clean bare.
    try {
      await this.runGit(undefined, ["clone", "--bare", repoUrl, dest], pat, scope, username);
      // Convert the mirror refspec to remote-tracking so future fetches write to
      // refs/remotes/origin/*. Under (b) the bare's refs/heads/* stay the
      // stale mirror; the agent branch is resolved via refs/remotes/origin/* (resume)
      // and lands back in refs/uzi-runner/* (fetchAgentBranch), never the bare's heads.
      await this.runGit(dest, ["config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"]);
      // Issue #134 — belt for a FRESH bare; the warm path in ensureClone is what covers the
      // deployed fleet. The bare is LONG-LIVED and shared across runs, so auto-maintenance is
      // arguably wanted here. Disabled anyway because a detached gc can run CONCURRENTLY with
      // a claim's fetch — a race nobody chose. The cost is real and tracked: an unmaintained
      // bare never repacks and never prunes, so `gc.autoPackLimit` (default 50) stops guarding
      // pack growth against a fixed-size PVC. The clean fix is a DELIBERATE
      // `git maintenance run --task=incremental-repack` taken inside `withLock(barePath)`,
      // which already serializes every bare operation and so is race-free by construction.
      await this.disableAutoMaintenance(dest);
      await this.fetch(dest, pat, scope, username);
    } catch (err) {
      await fs.rm(dest, { recursive: true, force: true });
      throw err;
    }
  }

  private async fetch(barePath: string, pat?: string, scope?: string, username?: string): Promise<void> {
    // issue #781 — the `--prune` FLAG (NOT the config form `fetch.prune` /
    // `remote.origin.prune`) prunes only the ref namespace of this fetch's configured
    // refspec `+refs/heads/*:refs/remotes/origin/*` — i.e. `refs/remotes/origin/*` — so a
    // remote branch deleted upstream stops seeding stale disjoint bases while
    // `refs/uzi-runner/*` and the separate checkpoint mirror fetch's `refs/uzi-checkpoints/*`
    // stay intact. The config form would additionally prune the locally-mirrored checkpoint
    // refs on the separate mirror fetch just below (whose origin has no `refs/uzi-checkpoints/*`
    // to match), degrading PRD #122 M8 cross-worker recovery.
    await this.runGit(barePath, ["fetch", "--prune", "origin"], pat, scope, username);
    // Refresh origin/HEAD so a remote default-branch change takes effect. Best
    // effort: defaultBranchRef has fallbacks if this symref is absent.
    await this.tryGit(barePath, ["remote", "set-head", "origin", "--auto"]);
    // PRD #122 M8 — mirror origin's brokered checkpoint refs into the bare, BEST-EFFORT.
    // This is the CROSS-WORKER signal path: a worker with no local refs/uzi-runner/<branch>
    // can still pull ANOTHER worker's published refs/uzi-checkpoints/<branch> and seed off
    // it (runnerCloneForBranch's checkpoint candidate). Rides the authenticated fetch (the
    // PAT is available here). Tolerates absence — origin may carry no such refs (fetch
    // succeeds fetching nothing), and the `.catch` covers an older server or a permission
    // edge that would otherwise surface as a fetch error.
    await this.runGit(
      barePath,
      ["fetch", "origin", "+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*"],
      pat,
      scope,
      username,
    ).catch(() => undefined);
  }

  /** Resolve a ref for the repo's default branch (the runner-clone base + the
   *  changedFiles diff base). */
  private async defaultBranchRef(barePath: string): Promise<string> {
    // 1) origin/HEAD, set by `remote set-head --auto`.
    const head = await this.tryGitStdout(barePath, ["symbolic-ref", "refs/remotes/origin/HEAD"]);
    if (head && (await this.refExists(barePath, head))) return head;
    // 2) common defaults, remote-tracking then mirror-layout fallback.
    for (const cand of [
      "refs/remotes/origin/main",
      "refs/remotes/origin/master",
      "refs/heads/main",
      "refs/heads/master",
    ]) {
      if (await this.refExists(barePath, cand)) return cand;
    }
    // 3) last resort: the bare repo's own HEAD.
    if (await this.refExists(barePath, "HEAD")) return "HEAD";
    throw new Error(`cannot resolve default branch for bare repo ${barePath}`);
  }

  private async refExists(barePath: string, ref: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["rev-parse", "--verify", "--quiet", `${ref}^{commit}`])) === 0;
  }

  /** issue #299 / PRD #759 — true when origin's brokered checkpoint ref for `branch`
   *  holds COMMITTED work that a report-only completion would orphan. `fetch()` mirrors
   *  `refs/uzi-checkpoints/<branch>` into the bare best-effort on every fetch, so this
   *  catches a checkpoint any PRIOR/cross-worker attempt landed; the runner pairs it with
   *  its own `landedCheckpoint` to also catch a checkpoint THIS worker published mid-run
   *  (not yet mirrored locally). The report-only completion guard uses the union to refuse
   *  orphaning a published checkpoint.
   *
   *  The marker-only exception (PRD #759): a usage-limit park publishes a throwaway
   *  `wip(park):` marker commit (WIP_PARK_COMMIT_PREFIX) to the checkpoint ref, and nothing
   *  deletes it on resume. A checkpoint whose tip is ONLY such a marker — with no committed
   *  milestone below it — is NOT committed work: it is an abandoned WIP marker, so this
   *  returns false for it and a legitimate report-only completion is no longer failed on
   *  its mere existence. The discriminator is ancestry, not descent: a marker-only checkpoint
   *  is one whose parent is an ancestor-or-equal of the recovery floor; anything else (a
   *  non-marker tip, or a marker whose parent STRICTLY DESCENDS the floor OR has DIVERGED from
   *  it — the "main advanced during a park" shape) is a genuine committed milestone and still
   *  returns true.
   *
   *  Only a SINGLE leading marker is ever stripped, and that is sufficient: the reseed's
   *  `reset --soft` (see runnerClone, git.ts ~654-669) removes an adopted marker from the
   *  branch history, so a resumed branch never carries a marker into the work the agent
   *  builds on. Any subsequent park therefore plants its marker on a parent that is a real
   *  commit or the base — never on another marker — so a one-marker strip cannot miss a
   *  buried committed milestone. */
  async hasCommittedCheckpoint(barePath: string, branch: string): Promise<boolean> {
    const ref = `refs/uzi-checkpoints/${branch}`;
    // 1) No checkpoint ref → nothing to orphan.
    if (!(await this.refExists(barePath, ref))) return false;
    // 2) Resolve the tip. A non-marker tip is a genuine committed checkpoint — return true,
    //    byte-identical to the pre-PRD-759 existence check for every real checkpoint.
    const tip = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${ref}^{commit}`])).trim();
    if (!(await this.isWipParkMarker(barePath, tip))) return true;
    // 3) The tip IS a `wip(park):` marker. The committed work (if any) is its parent and
    //    below. A root-commit marker (no readable parent) has nothing committed below to
    //    orphan → false (mirrors the root-marker handling in runnerClone, git.ts ~585-591).
    const parent = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${tip}^^{commit}`])).trim();
    if (parent === "") return false;
    // Compute the recovery floor similar to the way the reseed does (git.ts ~535): origin's
    // branch if pushed, else the default branch. (Only "similar to": the reseed excludes a
    // DISJOINT origin ref via originExists = raw existence AND non-disjoint, git.ts:475/484,
    // whereas this helper uses raw refExists. That is acceptable here because a disjoint
    // origin floor only pushes toward BLOCK — the safe direction: a disjoint parent is not an
    // ancestor of it, so the committed-work test below returns true.)
    //
    // The parent is a marker-only checkpoint iff it is an ancestor-or-equal of the floor;
    // anything else — the parent STRICTLY descends the floor, OR it DIVERGED from the floor
    // (shared ancestor, neither contains the other; the "main advanced during a park" shape) —
    // is committed work that a report-only completion would orphan, so it still blocks. This
    // mirrors the reseed's diverged-WIP leg (git.ts:713), which uses the same
    // isAncestor(markerParent, floor) discriminator to decide "no committed milestones live
    // below the marker". isAncestor is TRUE at equality, so the parent == floor case is a
    // marker-only checkpoint (allow); it also returns false on a missing/broken ref, which
    // fails safe toward BLOCK here.
    try {
      const floorRef = (await this.refExists(barePath, `refs/remotes/origin/${branch}`))
        ? `refs/remotes/origin/${branch}`
        : await this.defaultBranchRef(barePath);
      // Committed work exists below the marker iff the marker's parent is NOT an
      // ancestor-or-equal of the recovery floor:
      //   parent strictly descends floor → isAncestor(parent,floor) false → block ✓
      //   parent == floor                → isAncestor true (true at equality) → allow ✓
      //   parent is an ancestor of floor → isAncestor true                    → allow ✓
      //   parent and floor DIVERGED      → isAncestor(parent,floor) false → block ✓
      return !(await this.isAncestor(barePath, parent, floorRef));
    } catch (err) {
      // Fail-safe: the floor could not be resolved. Preserve the guard rather than risk
      // orphaning committed work below the marker.
      this.log.warn("hasCommittedCheckpoint: could not resolve recovery floor — preserving guard", {
        branch,
        tip,
        parent,
        error: err instanceof Error ? err.message : String(err),
      });
      return true;
    }
  }

  /** True when `ancestorRef` is an ancestor of (or equal to) `descendantRef` — i.e.
   *  `descendantRef` strictly descends from it (PRD #218 M2). Exit 0 = ancestor, 1 =
   *  not; any other failure (a missing ref) answers false, so a broken candidate never
   *  wins the "strictly descends" branch. */
  private async isAncestor(barePath: string, ancestorRef: string, descendantRef: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["merge-base", "--is-ancestor", ancestorRef, descendantRef])) === 0;
  }

  /** issue #1117: public delegate to {@link isAncestor} — true when `ancestorRef` is an
   *  ancestor of (or equal to) `descendantRef`, i.e. `descendantRef` descends from it; any
   *  failure (a missing ref) answers false. Exposed so the mr_rework finalize path can
   *  confirm that the freshly-fetched remote tip strictly descends from the origin tip at
   *  clone (a genuine concurrent-writer advance) rather than a rewound/diverged history. */
  async isAncestorRef(barePath: string, ancestorRef: string, descendantRef: string): Promise<boolean> {
    return this.isAncestor(barePath, ancestorRef, descendantRef);
  }

  /**
   * PRD #1416 M2: tri-state ancestry of a published/checkpoint FLOOR against the fetched TIP,
   * error-safe. Modeled on {@link isAncestor} (the same `merge-base --is-ancestor` primitive
   * over the best-effort {@link tryGit}) but tri-state and input-validated, because the runner's
   * divergence detection (M2) and the finalize bridge (M3) both hang off it and a false
   * "divergent" on a broken read would arm a spurious steer / bridge.
   *
   *  - "ancestor"  — exit 0: `floor` is an ancestor of (or equal to) `tip`, so the branch still
   *                  fast-forwards from the floor and nothing at/below it was rewritten.
   *  - "divergent" — exit 1: both OIDs resolve but `floor` is NOT an ancestor of `tip` (the
   *                  history at/below the floor was rewritten, or the histories are disjoint), so
   *                  the branch can no longer be landed with a plain fast-forward push.
   *  - "unknown"   — ANY git error, a missing/unresolvable ref, a malformed floor/tip, OR a
   *                  non-completion of the git process (a spawn failure such as ENOENT, a
   *                  timeout/SIGTERM kill, or any signal death). NEVER coerced to "divergent";
   *                  the caller treats unknown as "not divergent".
   *
   * git's contract makes the discrimination reliable (verified): 0 = ancestor, 1 = both commits
   * present but not-an-ancestor, 128 = a missing/invalid commit or a broken repo. Only a GENUINE
   * numeric exit 1 (the process ran to completion and merge-base reported not-an-ancestor) maps to
   * "divergent". A process that never produced a real exit status carries a non-numeric code (a
   * string like "ENOENT" on a spawn failure, or `null` on a signal/timeout kill), which
   * {@link tryGitExit} surfaces as `null` → "unknown" — so a broken read is never mistaken for a
   * data answer. Both inputs must be full 40-hex OIDs (P and C come from originBranchTip/
   * trackingTip; the tip is a fetched tracking tip); a value that is not answers "unknown"
   * without running git.
   */
  async ancestry(
    barePath: string,
    floor: string,
    tip: string,
  ): Promise<"ancestor" | "divergent" | "unknown"> {
    const OID = /^[0-9a-f]{40}$/;
    if (!OID.test(floor) || !OID.test(tip)) return "unknown";
    // tryGitExit (NOT tryGit) so a non-completion — a spawn failure, timeout, or signal kill —
    // surfaces as null rather than tryGit's coerce-to-1, which would misreport it as "divergent".
    const code = await this.tryGitExit(barePath, ["--no-replace-objects", "merge-base", "--is-ancestor", floor, tip]);
    if (code === 0) return "ancestor";
    if (code === 1) return "divergent";
    // 128 / any other exit = a git error or a missing/unresolvable ref, and null = a spawn error,
    // timeout, or signal death → unknown, never divergent.
    return "unknown";
  }

  /**
   * PRD #1416 M3 — build the ancestry BRIDGE commit B that non-destructively restores every
   * missing published/checkpoint FLOOR as an ancestor of a divergent tip H, so a rewritten branch
   * FAST-FORWARDS from its published floor WITHOUT a force-push (D4). Reuses the checkpoint
   * overlay's synthesis technique ({@link buildWorkflowOverlay}, fact 12) — a deterministic,
   * worker-uid, PAT-free `commit-tree` with no working tree — but with H's tree UNCHANGED, so no
   * temp-index edit is needed: B is committed directly onto H's own tree OID.
   *
   * B's parents: `-p tip` FIRST (H is the first parent, so `git log --first-parent` still reads as
   * the agent's history), then `-p <floor>` for EACH floor in `floors` that is NOT already an
   * ancestor of `tip` (`ancestry(bare, floor, tip) === "divergent"`), in the given order. A floor
   * already reachable from H needs no bridge parent and is skipped.
   *
   * Message EXACTLY: `bridge: restore published tip <P> as an ancestor of <H> (tree unchanged)`,
   * where <P> is `floors[0]` (the published floor) and <H> is `tip`, both full 40-hex OIDs — the
   * deterministic marker {@link rangeContainsBridge} parses.
   *
   * IDEMPOTENT (#1416 MR-rework, finding 2): before building anything, if an ADDITIONAL floor `f`
   * (index > 0) is ALREADY a valid superset bridge of H over every other floor — H is an ancestor of
   * `f`, `f`'s tree === H's tree, and every other floor is an ancestor of `f` — that existing `f` is
   * returned UNCHANGED (no new commit). This makes an idle re-bridge of an unchanged H a no-op while
   * PRESERVING `f`'s full lineage (any genuine checkpoint sibling folded into it stays an ancestor of
   * B). It replaces the old tree-equality skip, which unsoundly DROPPED a genuine divergent sibling
   * whose tree merely coincided with H's (equal trees do not imply equal histories).
   *
   * INVARIANT (validated by the caller before adopting B): B's tree === H's tree byte-for-byte, and
   * every appended floor AND H are ancestors of B.
   *
   * @returns a three-way tagged result:
   *   - `{ kind: "built", sha }` — a bridge exists: either a freshly built commit or an existing
   *     idempotent-superset floor. `sha` is a full 40-hex OID the caller validates and adopts.
   *   - `{ kind: "noop" }` — NOTHING was actually missing (every floor already an ancestor of H, a
   *     fast-forward): there was nothing to bridge, and no ancestry read was transiently `"unknown"`.
   *   - `{ kind: "failed" }` — malformed input, a git error, or a transient `"unknown"` ancestry read
   *     that could not be resolved to a clean no-op (best-effort — a bridge that cannot be built never
   *     throws here; the caller decides how to fail). A `"failed"` is NEVER conflated with a clean
   *     no-op, so a fast-forwardable run is not mistaken for a genuine synthesis failure and vice
   *     versa.
   */
  async bridgeToFloors(
    barePath: string,
    tip: string,
    floors: string[],
  ): Promise<{ kind: "built"; sha: string } | { kind: "noop" } | { kind: "failed" }> {
    const OID = /^[0-9a-f]{40}$/;
    if (!OID.test(tip) || floors.length === 0 || !OID.test(floors[0]!)) return { kind: "failed" };
    try {
      // B's tree === H's tree, byte-identical: a tree OID is content-addressed, so committing onto
      // `tip^{tree}` reproduces H's tree exactly (no read-tree/write-tree round-trip needed). Read it
      // up front because the superset-bridge idempotency check below also compares each additional
      // floor's tree to it.
      const tipTree = (await this.runGit(barePath, ["rev-parse", `${tip}^{tree}`])).trim();
      if (!OID.test(tipTree)) return { kind: "failed" };
      // #1416 (MR-rework, finding 2) — IDEMPOTENT SUPERSET-BRIDGE check (replaces the old, UNSOUND
      // tree-equality skip). An idle re-bridge of the SAME divergent H must NOT nest a fresh wrapper
      // every tick. The prior fix skipped any ADDITIONAL floor whose tree merely EQUALLED H's — but
      // equal trees do NOT imply equal histories, so a GENUINE divergent sibling C (e.g. a real
      // checkpoint commit that does NOT descend from H) whose tree coincidentally equals H's would be
      // dropped, and C's lineage would silently stop being an ancestor of B (degrading cross-worker
      // `refs/uzi-checkpoints/<branch>` recovery).
      //
      // Detect TRUE idempotency instead: an additional floor f (index > 0) that is ALREADY a valid
      // bridge of H over every other floor needs no new commit. f qualifies iff ALL hold — H is an
      // ancestor of f (f descends from H), f's tree === H's tree, and every OTHER floor is an ancestor
      // of f (f already covers them). Return that f unchanged: creating nothing preserves f's full
      // lineage, including any genuine C folded into it. Growth stays bounded to one wrapper per
      // GENUINE rewrite — after C advances to the new bridge B, the next idle re-bridge finds B already
      // covers [P, C] and returns it unchanged, so no nesting accrues. Iterate in order and return the
      // first covering candidate (deterministic; floors is at most [P, C] in practice, so at most one).
      for (let i = 1; i < floors.length; i++) {
        const f = floors[i]!;
        if (!OID.test(f)) continue;
        if ((await this.ancestry(barePath, tip, f)) !== "ancestor") continue; // H must descend into f
        const fTree = (await this.tryGitStdout(barePath, ["rev-parse", `${f}^{tree}`])).trim();
        if (!OID.test(fTree) || fTree !== tipTree) continue; // f must preserve H's tree
        let coversAll = true;
        for (let j = 0; j < floors.length; j++) {
          if (j === i) continue;
          const g = floors[j]!;
          if (!OID.test(g) || (await this.ancestry(barePath, g, f)) !== "ancestor") {
            coversAll = false;
            break;
          }
        }
        if (coversAll) return { kind: "built", sha: f }; // f is already a superset bridge of H over all floors — idempotent
      }
      // Only floors NOT already reachable from H become extra parents (a floor already an ancestor
      // needs no bridge parent). "divergent" is the only positive signal — "unknown" (a transient
      // read) and "ancestor" both mean "do not append", so a broken read never synthesises a
      // spurious parent. EVERY divergent floor is appended (no tree-equality skip): a genuine
      // divergent sibling with a coincidentally-equal tree is RETAINED as a parent, so its lineage
      // stays an ancestor of B.
      const missing: string[] = [];
      let anyUnknown = false;
      for (let i = 0; i < floors.length; i++) {
        const floor = floors[i]!;
        if (!OID.test(floor)) continue;
        const rel = await this.ancestry(barePath, floor, tip);
        if (rel === "unknown") {
          anyUnknown = true;
          continue;
        }
        if (rel !== "divergent") continue; // "ancestor" → already covered, do not append
        missing.push(floor);
      }
      // A transient/unknown ancestry read is treated as a synthesis FAILURE, never a clean no-op: if
      // no floor read "divergent" but some read "unknown", we cannot prove H already covers every
      // floor, so returning "noop" would let a fast-forward masquerade for what may be a real
      // divergence a broken read hid.
      if (missing.length === 0) return anyUnknown ? { kind: "failed" } : { kind: "noop" };
      // Deterministic committer/author date = H's own committer date (buildWorkflowOverlay's rule),
      // so a re-bridge of the same H over the same floors yields the same OID.
      const committerDate = (await this.runGit(barePath, ["show", "-s", "--format=%cI", tip])).trim();
      if (committerDate.length === 0) return { kind: "failed" };
      const args = ["-c", "commit.gpgsign=false", "commit-tree", tipTree, "-p", tip];
      for (const floor of missing) args.push("-p", floor);
      args.push(
        "-m",
        `bridge: restore published tip ${floors[0]} as an ancestor of ${tip} (tree unchanged)`,
      );
      const sha = (
        await this.runGitWithEnv(barePath, args, {
          GIT_AUTHOR_NAME: AGENT_GIT_IDENTITY.name,
          GIT_AUTHOR_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_COMMITTER_NAME: AGENT_GIT_IDENTITY.name,
          GIT_COMMITTER_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_AUTHOR_DATE: committerDate,
          GIT_COMMITTER_DATE: committerDate,
        })
      ).trim();
      if (!OID.test(sha)) return { kind: "failed" };
      this.log.info("PRD #1416 M3: ancestry bridge built", {
        tip,
        published_floor: floors[0],
        bridge_parents: missing.length + 1,
        bridge_sha: sha,
      });
      return { kind: "built", sha };
    } catch (e) {
      this.log.warn("PRD #1416 M3: ancestry bridge synthesis failed", {
        tip,
        error: gitErrorMessage(e),
      });
      return { kind: "failed" };
    }
  }

  /**
   * PRD #1416 M3 — the structure-validated bridge DETECTOR (spoof/inflation-hardened). Scans
   * `<publishedTip>..<pushedTip>` with `git log --first-parent` so a merged default-branch SIDE
   * history (a second parent) can neither SPOOF (masquerade as a bridge) nor INFLATE (be walked at
   * all) the detection. Returns true iff some commit in the range is a bridge in one of exactly two
   * FULLY-validated forms; everything else is rejected. History-derived on purpose: a bridge built
   * before a park is invisible to a reclaimed run's flight-local state, but it is right here in the
   * pushed history, so a reclaim's finalize (which builds no new bridge) still reports it.
   *
   *  1. WORKER bridge (carries the deterministic {@link bridgeToFloors} marker). Parse X and Y
   *     (40-hex) from the marker and require ALL of: `X === publishedTip` (this run's P); `Y ===`
   *     the commit's FIRST parent; the commit's tree === its first-parent's tree; between 1 and
   *     {@link BRIDGE_MAX_EXTRA_PARENTS} EXTRA parents (beyond the first — a legitimate bridge has at
   *     most P and C, and a candidate over the bound is rejected WITHOUT scanning its parents, #1416);
   *     EVERY extra parent descends from `publishedTip`; and ≥1 extra parent is
   *     NOT already an ancestor of Y. A marker present but any check failing is REJECTED (never
   *     re-checked as the markerless form).
   *  2. AGENT safety bridge (no marker — the M2 steer had the agent run `git merge -s ours <P>`).
   *     Accepted with NO marker ONLY when the commit's tree === its first-parent's tree AND an
   *     extra parent is EXACTLY `publishedTip`.
   *
   * Why `--first-parent` + full structure validation and NOT a raw marker-or-shape OR: a plain
   * `git merge <default>` into the branch produces a merge whose tree DIFFERS from its first parent
   * and whose side history could carry any text; without `--first-parent` its side commits would be
   * walked, and without the tree/parent checks a spoofed message or an unrelated `-s ours` merge
   * would be miscounted. Returns false (never throws) on any git error or when publishedTip/
   * pushedTip is missing/malformed.
   */
  async rangeContainsBridge(
    barePath: string,
    publishedTip: string,
    pushedTip: string,
  ): Promise<boolean> {
    const OID = /^[0-9a-f]{40}$/;
    if (!OID.test(publishedTip) || !OID.test(pushedTip)) return false;
    // Field AND record separators: NUL (`%x00`) — the ONE byte git FORBIDS in a commit message.
    // 0x1f/0x1e are NOT forbidden (git round-trips both), so a crafted %B carrying either byte could
    // inject a fake field/record separator and mis-split the parse — that was the prior bug. NUL can
    // never appear in %B, so the body (always the LAST field of a record) cannot corrupt the parse.
    // `-z` NUL-terminates each commit entry too, so there is no inter-commit newline to strip: the
    // whole output is a flat NUL-delimited token stream, and every record is exactly four fields
    // (%H,%T,%P,%B) — grouped in fours unambiguously. NUL is written as a printable \u escape here
    // (never a raw control byte in source).
    const NUL = "\u0000";
    let raw: string;
    try {
      raw = await this.runGit(barePath, [
        "log",
        "--first-parent",
        "-z",
        "--format=%H%x00%T%x00%P%x00%B",
        `${publishedTip}..${pushedTip}`,
      ]);
    } catch {
      return false; // an invalid range / missing ref / broken repo → not detectable, never throw
    }
    const markerRe =
      /bridge: restore published tip ([0-9a-f]{40}) as an ancestor of ([0-9a-f]{40}) \(tree unchanged\)/;
    // Each record is exactly four fields (%H,%T,%P,%B); the trailing NUL from `-z` yields one empty
    // trailing token, which the `i + 3 < tokens.length` bound skips.
    const tokens = raw.split(NUL);
    for (let i = 0; i + 3 < tokens.length; i += 4) {
      const tree = tokens[i + 1]!.trim();
      const parents = tokens[i + 2]!.trim().split(/\s+/).filter((p) => OID.test(p));
      const body = tokens[i + 3]!;
      const firstParent = parents[0];
      if (!firstParent || !OID.test(tree)) continue;
      const extras = parents.slice(1);
      // tree === first-parent's tree — the load-bearing "H unchanged" property of a real bridge.
      const fpTree = (
        await this.tryGitStdout(barePath, ["rev-parse", `${firstParent}^{tree}`])
      ).trim();
      const treeEqualsFirstParent = OID.test(fpTree) && tree === fpTree;
      const m = body.match(markerRe);
      if (m) {
        // WORKER bridge — the marker MEANS it must satisfy form 1 in full; a failing marker commit
        // is rejected outright (never re-checked as the markerless agent form).
        const X = m[1]!;
        const Y = m[2]!;
        // #1416 M3 — reject a marker candidate with MORE than a small bound of extra parents OUTRIGHT
        // (do not scan all parents): a legitimate bridge has at most 2 (P and C), and the per-parent
        // scan below spawns ~2 isAncestor git subprocesses per extra parent, so a crafted commit with
        // many descendant-of-P parents would otherwise stall the run's finalize.
        if (
          X !== publishedTip ||
          Y !== firstParent ||
          !treeEqualsFirstParent ||
          extras.length < 1 ||
          extras.length > BRIDGE_MAX_EXTRA_PARENTS
        ) {
          continue;
        }
        let allDescend = true;
        let someNotAncestorOfY = false;
        for (const ep of extras) {
          if (!(await this.isAncestor(barePath, publishedTip, ep))) {
            allDescend = false;
            break;
          }
          if (!(await this.isAncestor(barePath, ep, firstParent))) someNotAncestorOfY = true;
        }
        if (allDescend && someNotAncestorOfY) return true;
        continue;
      }
      // AGENT safety bridge (no marker): tree unchanged AND publishedTip is EXACTLY an extra parent.
      if (treeEqualsFirstParent && extras.includes(publishedTip)) return true;
    }
    return false;
  }

  /** issue #781 — true when `ref` shares any history with the default branch, i.e.
   *  plain `git merge-base <ref> <default>` prints a commit (exit 0). Distinct from
   *  isAncestor (merge-base --is-ancestor), which returns non-zero for BOTH a disjoint
   *  AND a merely-diverged base — so it cannot be reused here without false-rejecting a
   *  legitimately far-ahead resume/first-park base. Only plain merge-base discriminates
   *  disjoint (empty, exit 1) from diverged (a common ancestor exists, exit 0). */
  private async sharesHistory(barePath: string, ref: string, defaultRef: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["merge-base", ref, defaultRef])) === 0;
  }

  /** PRD #759 M2 — is the commit `sha` a `wip(park):` marker (WIP_PARK_COMMIT_PREFIX)?
   *  Reads the commit SUBJECT from the worker-owned bare with a read-only object read
   *  (`log -1 --format=%s`, worker-uid — no working-tree touch, so no filter-driver
   *  fire, safe). Best-effort: any failure (a missing/broken commit) answers false, so a
   *  bad read never routes the reseed onto the reset-soft / cherry-pick recovery path. */
  private async isWipParkMarker(barePath: string, sha: string): Promise<boolean> {
    const subject = await this.tryGitStdout(barePath, ["log", "-1", "--format=%s", `${sha}^{commit}`]);
    return subject.startsWith(WIP_PARK_COMMIT_PREFIX);
  }

  /** PRD #1062 M2 (#1036) — is the commit `sha` a `ckpt(overlay):` transport wrapper
   *  (OVERLAY_COMMIT_PREFIX)? Mirrors isWipParkMarker: a read-only worker-uid subject read,
   *  best-effort (any failure ⇒ false, so a bad read never routes adoption onto the overlay
   *  peel). Adoption peels an overlay by DISCARDING its swapped `.github/workflows` tree and
   *  re-pointing the base to its LAST parent (the real tip) — see runnerCloneForBranch. */
  private async isOverlayMarker(barePath: string, sha: string): Promise<boolean> {
    const subject = await this.tryGitStdout(barePath, ["log", "-1", "--format=%s", `${sha}^{commit}`]);
    return subject.startsWith(OVERLAY_COMMIT_PREFIX);
  }

  // --- git subprocess plumbing -------------------------------------------------

  /** A hard permit abort is not evidence that a candidate contains scratch files. */
  private boundaryAbortError(cause: unknown): Error | undefined {
    const scope = this.boundaryProcesses.getStore();
    if (scope?.signal.aborted) {
      const message = cause instanceof Error && cause.message === GIT_OUTPUT_ABORT_MESSAGE
        ? GIT_OUTPUT_ABORT_MESSAGE
        : "permit-held git operation aborted";
      return new GitBoundaryAbortError(message, cause);
    }
    if (cause instanceof CheckpointSoftDeadlineError) return cause;
    if (cause instanceof CheckpointChildStartupTimeoutError) {
      if (scope?.softDeadlineAt === undefined) return cause;
      this.log.warn("checkpoint child startup timeout recovered", {
        classification: "started_deadline", cleanup: "verified",
      });
      return new CheckpointSoftDeadlineError(cause);
    }
    if (scope?.softSignal?.aborted) return new CheckpointSoftDeadlineError();
    if (cause instanceof GitBoundaryAbortError) return cause;
    // Foreign AbortError messages can carry remote text. Preserve the abort type and
    // original cause without copying that message onto the run-log surface.
    if (isAbortLike(cause)) return new GitBoundaryAbortError("permit-held git operation aborted", cause);
    return undefined;
  }

  private async execScoped(
    command: string,
    args: string[],
    options: { env: NodeJS.ProcessEnv; timeout?: number; maxBuffer?: number; cwd?: string; input?: string | Buffer },
    identity: BoundaryProcessRequest["identity"] = "worker_pat",
  ): Promise<{ stdout: string; stderr: string }> {
    const boundary = this.boundaryProcesses.getStore();
    const recovery = this.recoveryOperations.getStore();
    recovery?.signal.throwIfAborted();
    const { input, ...execOptions } = options;
    if (!boundary) {
      // issue #2213: the second quarantine check, keyed on the credential itself, synchronously
      // before the child is created (nothing awaits between this and execFileAsync).
      assertNoCredentialedGitWhileQuarantined(options.env);
      // issue #1597 M2: optional stdin (the checkpoint scan's cat-file / gitleaks stdin). An EPIPE on
      // an early-exiting child is swallowed here; the exit status carries the failure.
      const pending = execFileAsync(command, args, { ...execOptions,
        ...(recovery ? { signal: recovery.signal, timeout: Math.min(options.timeout ?? Infinity, Math.max(1, recovery.deadline - Date.now())) } : {}),
      });
      const closed = new Promise<void>(resolve => pending.child.once("close", () => resolve()));
      let killTimer: ReturnType<typeof setTimeout> | undefined;
      const abort = () => { killTimer = setTimeout(() => pending.child.kill("SIGKILL"), 1_000); };
      recovery?.signal.addEventListener("abort", abort, { once: true });
      if (input !== undefined) {
        pending.child.stdin?.on("error", () => undefined);
        pending.child.stdin?.end(input);
      }
      try {
        const result = await pending;
        return { stdout: String(result.stdout), stderr: String(result.stderr) };
      } finally {
        await closed;
        if (killTimer) clearTimeout(killTimer);
        recovery?.signal.removeEventListener("abort", abort);
      }
    }
    const cwd = options.cwd ?? (identity === "command" ? commandCwd(args) : "/");
    const executable = resolveBoundaryExecutable(command);
    if (boundary.signal.aborted) throw new GitBoundaryAbortError(GIT_OUTPUT_ABORT_MESSAGE);
    if (boundary.softSignal?.aborted || (boundary.softDeadlineAt !== undefined && Date.now() >= boundary.softDeadlineAt)) {
      throw new CheckpointSoftDeadlineError();
    }
    const remainingSoft = boundary.softDeadlineAt === undefined ? undefined : Math.max(1, boundary.softDeadlineAt - Date.now());
    const childTimeout = recovery
      ? Math.min(options.timeout ?? GIT_TIMEOUT_MS, remainingSoft ?? Infinity, Math.max(1, recovery.deadline - Date.now()))
      : remainingSoft === undefined ? options.timeout : Math.min(options.timeout ?? Infinity, remainingSoft);
    // issue #2213: as above, on the Codex boundary path. boundary.spawn is awaited and its own
    // chain (the tick spawner's lock snapshot, the safety spawn boundary) awaits before the actual
    // spawn, so this check is early; TickSpawner.spawnWith and launchCodexEffectRoot repeat it
    // synchronously immediately before their own spawn call.
    assertNoCredentialedGitWhileQuarantined(options.env);
    let process: BoundaryProcessHandle;
    try {
      process = await boundary.spawn({ argv: [executable, ...args], cwd, env: options.env, identity,
        ...(childTimeout === undefined ? {} : { timeoutMs: childTimeout }),
        ...(remainingSoft === undefined ? {} : { recoverableTimeout: true }),
      });
    } catch (cause) {
      throw this.boundaryAbortError(cause) ?? cause;
    }
    let cancellation: Promise<unknown> | undefined;
    const abortRecovery = () => { cancellation = process.cancel().catch(() => undefined); };
    recovery?.signal.addEventListener("abort", abortRecovery, { once: true });
    if (recovery?.signal.aborted) abortRecovery();
    process.stdin?.on("error", () => undefined);
    process.stdin?.end(input);
    const cap = options.maxBuffer ?? GIT_MAX_BUFFER;
    const collect = (stream: Readable | null): Promise<{ chunks: Buffer[]; oversized: boolean }> =>
      new Promise((resolve, reject) => {
        if (!stream) { resolve({ chunks: [], oversized: false }); return; }
        const chunks: Buffer[] = [];
        let bytes = 0;
        let oversized = false;
        let settled = false;
        const cleanup = (): void => {
          boundary.signal.removeEventListener("abort", onAbort);
          stream.removeListener("data", onData);
          stream.removeListener("end", onEnd);
          stream.removeListener("close", onClose);
          stream.removeListener("error", onError);
        };
        const settle = (
          result: { chunks: Buffer[]; oversized: boolean } | undefined,
          error?: unknown,
        ): void => {
          if (settled) return;
          settled = true;
          cleanup();
          if (error !== undefined) reject(error);
          else resolve(result!);
        };
        const onData = (chunk: Buffer | string): void => {
          const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
          if (oversized) return;
          const remaining = cap - bytes;
          if (buf.length > remaining) {
            if (remaining > 0) chunks.push(buf.subarray(0, remaining));
            oversized = true;
            bytes = cap;
            return;
          }
          chunks.push(buf);
          bytes += buf.length;
        };
        const onEnd = (): void => settle({ chunks, oversized });
        const onClose = (): void => settle(undefined, new Error("subprocess output closed before end"));
        const onError = (error: unknown): void => settle(undefined, error);
        const onAbort = (): void => {
          stream.destroy();
          settle(undefined, new Error(GIT_OUTPUT_ABORT_MESSAGE));
        };
        stream.on("data", onData);
        stream.once("end", onEnd);
        stream.once("close", onClose);
        stream.once("error", onError);
        // Data read before we listened is lost (child_process resumes unread stdio at exit), ended or not:
        // refuse, as for an errored stream or one destroyed without ending. A stream already ended with
        // nothing ever read is a clean empty EOF (the Codex supervisor closes its stdio copies at launch, so
        // a fast, silent git child's pipe can end before the handle is returned).
        if (stream.readableDidRead || stream.errored) onClose();
        else if (stream.readableEnded) onEnd();
        else if (stream.destroyed) onClose();
        if (boundary.signal.aborted) onAbort();
        else boundary.signal.addEventListener("abort", onAbort, { once: true });
      });
    let stdout: { chunks: Buffer[]; oversized: boolean };
    let stderr: { chunks: Buffer[]; oversized: boolean };
    let terminal: { readonly code: number; readonly softTimedOut?: true };
    // A collector can close/error before a timed-out child is fully disposed.
    // Keep the bare lock until the safety owner has verified full-root reap.
    const settled = await Promise.allSettled([collect(process.stdout), collect(process.stderr), process.completed] as const);
    recovery?.signal.removeEventListener("abort", abortRecovery);
    await cancellation;
    recovery?.signal.throwIfAborted();
    if (settled[2].status === "rejected") {
      throw this.boundaryAbortError(settled[2].reason) ?? settled[2].reason;
    }
    terminal = settled[2].value;
    if (boundary.signal.aborted) throw new GitBoundaryAbortError(GIT_OUTPUT_ABORT_MESSAGE);
    if (terminal.softTimedOut) throw new CheckpointSoftDeadlineError();
    if (settled[0].status === "rejected") throw this.boundaryAbortError(settled[0].reason) ?? settled[0].reason;
    if (settled[1].status === "rejected") throw this.boundaryAbortError(settled[1].reason) ?? settled[1].reason;
    stdout = settled[0].value;
    stderr = settled[1].value;
    const out = Buffer.concat(stdout.chunks).toString();
    const err = Buffer.concat(stderr.chunks).toString();
    if (stdout.oversized || stderr.oversized || terminal.code !== 0) {
      const failure = new Error(
        stdout.oversized || stderr.oversized
          ? `subprocess output exceeded ${cap} bytes`
          : `subprocess exited ${terminal.code}`,
      ) as Error & { code?: number; stdout?: string; stderr?: string; outputExceeded?: boolean };
      failure.code = terminal.code;
      if (stdout.oversized || stderr.oversized) failure.outputExceeded = true;
      failure.stdout = out;
      failure.stderr = err;
      throw failure;
    }
    return { stdout: out, stderr: err };
  }

  private async runGit(cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string): Promise<string> {
    const env = gitEnv(pat, scope, username);
    // Log args only; the PAT lives in env (GIT_CONFIG_VALUE_n), never in args.
    this.log.debug("git", { cwd, args });
    try {
      const { stdout } = await this.execScoped("git", withDir(cwd, args), {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
      });
      return stdout;
    } catch (err) {
      // issue #2213: the quarantine refusal reaches the runner unwrapped (a typed worker fault).
      if (err instanceof RunResidueBlockedError) throw err;
      const abort = this.boundaryAbortError(err);
      if (abort) throw abort;
      throw new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`);
    }
  }

  /**
   * PRD #1062 M2 (#1036) — a worker-uid git that carries EXTRA environment on top of the base
   * `gitEnv()` (no PAT): `GIT_INDEX_FILE` for a temp-index synthesis, `GIT_WORK_TREE` for the
   * one op (`read-tree --prefix`) that refuses in a bare repo, and the deterministic
   * `GIT_AUTHOR_*`/`GIT_COMMITTER_*`/`*_DATE` identity for the overlay `commit-tree`. The extra
   * pairs are merged AFTER `gitEnv()` so the security config pins (safe.directory / hooksPath /
   * code-exec-key pins) are never displaced. Same 64 MiB cap + timeout as `runGit`.
   */
  private async runGitWithEnv(
    cwd: string | undefined,
    args: string[],
    extraEnv: NodeJS.ProcessEnv,
  ): Promise<string> {
    const env = { ...gitEnv(), ...extraEnv };
    this.log.debug("git (env)", { cwd, args });
    try {
      const { stdout } = await this.execScoped("git", withDir(cwd, args), {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
      });
      return stdout;
    } catch (err) {
      if (err instanceof RunResidueBlockedError) throw err;
      const abort = this.boundaryAbortError(err);
      if (abort) throw abort;
      throw new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`);
    }
  }

  /**
   * Spawn a git subprocess and STREAM its stdout (PRD #122 M8), for an output that can
   * exceed runGit's 64 MiB `maxBuffer` cap — a checkpoint packfile. Plain `spawn` as the
   * WORKER uid on the credential-free base gitEnv: the worker owns the bare it reads, so
   * there is no runner-uid switch and no PAT. Writes the optional `stdin` and ends it, then
   * returns the child and its stdout so the caller can pipe/drain it.
   *
   * The returned stdout is {@link exitGatedStream}'s: it is piped from the child at once, and
   * it ENDS only once the source has ended AND the child exited 0. A nonzero exit DESTROYS it
   * with an Error carrying git's stderr instead; a spawn error, or (inside a boundary) a rejected
   * completion, destroys it with that error itself; a source that closes before its end (a
   * premature close) destroys it too. So a consumer streaming it (the checkpoint pack upload,
   * the finalize import's `index-pack`) sees the failure rather than a truncated or empty pack
   * ending cleanly. A
   * consumer that abandons the stream (destroys it, or its pipeline fails) tears the producer
   * down: the source is unpiped and destroyed, and outside a boundary a still-live child is
   * killed.
   *
   * `exited` (issue #1769) settles with the exit code once the child has terminated — inside a
   * boundary only after its supervisor root has reaped — and NEVER rejects: a spawn error, a
   * signal death or a rejected completion resolves to -1, so a caller that ignores it cannot
   * leak an unhandled rejection. Stream completion alone does not prove the process is reaped;
   * a caller that must know (e.g. {@link ensureRunnerCloneObjects}) awaits `exited` as well.
   *
   * `opts` (PRD #1798 M5, used by {@link readBare} only) forwards a child timeout to the boundary
   * spawner and caps the stderr kept for the failure message. A checkpoint soft scope also caps
   * a streaming pack child even when its caller omits `opts`.
   */
  private async spawnGit(
    cwd: string,
    args: string[],
    stdin?: string,
    opts: SpawnGitOptions = {},
  ): Promise<{ child?: ChildProcess; stdout: Readable; exited: Promise<number>; cancel: () => void }> {
    const env = { ...gitEnv(), ...opts.env };
    this.log.debug("git (spawn)", { cwd, args });
    const boundary = this.boundaryProcesses.getStore();
    const recovery = this.recoveryOperations.getStore();
    recovery?.signal.throwIfAborted();
    if (boundary) {
      if (boundary.signal.aborted) throw new GitBoundaryAbortError(GIT_OUTPUT_ABORT_MESSAGE);
      if (boundary.softSignal?.aborted || (boundary.softDeadlineAt !== undefined && Date.now() >= boundary.softDeadlineAt)) {
        throw new CheckpointSoftDeadlineError();
      }
      const remainingSoft = boundary.softDeadlineAt === undefined ? undefined : Math.max(1, boundary.softDeadlineAt - Date.now());
      const childTimeout = recovery
        ? Math.min(opts.timeoutMs ?? GIT_TIMEOUT_MS, remainingSoft ?? Infinity, Math.max(1, recovery.deadline - Date.now()))
        : remainingSoft === undefined ? opts.timeoutMs : Math.min(opts.timeoutMs ?? Infinity, remainingSoft);
      let process: BoundaryProcessHandle;
      try {
        process = await boundary.spawn({
          argv: [GIT_BIN, ...withDir(cwd, args)],
          cwd,
          env,
          identity: "worker_pat",
          ...(childTimeout === undefined ? {} : { timeoutMs: childTimeout }),
          ...(remainingSoft === undefined ? {} : { recoverableTimeout: true }),
        });
      } catch (cause) {
        throw this.boundaryAbortError(cause) ?? cause;
      }
      if (!process.stdout) throw new Error("supervised git process has no stdout");
      const stderrCap = opts.stderrMaxBytes ?? GIT_MAX_BUFFER;
      const stderrChunks: Buffer[] = [];
      let stderrBytes = 0;
      process.stderr?.on("data", (c: Buffer | string) => {
        if (stderrBytes >= stderrCap) return;
        const chunk = Buffer.isBuffer(c) ? c : Buffer.from(c);
        const kept = chunk.subarray(0, stderrCap - stderrBytes);
        stderrChunks.push(kept);
        stderrBytes += kept.length;
      });
      let cancellation: Promise<unknown> | undefined;
      const abortRecovery = () => { cancellation ??= process.cancel().catch(() => undefined); };
      const gated = exitGatedStream(process.stdout, opts.cancelOnAbandon ? abortRecovery : undefined, true);
      process.completed.then(({ code, softTimedOut }) => {
        if (softTimedOut) {
          gated.exited(new CheckpointSoftDeadlineError());
          return;
        }
        if (code !== 0) {
          const detail = Buffer.concat(stderrChunks).subarray(0, stderrCap).toString().trim();
          gated.exited(new Error(`git ${args.join(" ")} exited ${code}${detail ? `: ${detail}` : ""}`));
        } else gated.exited();
      }, (error: unknown) => gated.exited(error instanceof Error ? error : new Error(String(error))));
      // A child that exits before reading its stdin (e.g. its repo vanished) must not surface an
      // uncaught EPIPE; the exit status already errors the stream with the failure.
      process.stdin?.on("error", () => undefined);
      process.stdin?.end(stdin ?? "");
      recovery?.signal.addEventListener("abort", abortRecovery, { once: true });
      if (recovery?.signal.aborted) abortRecovery();
      const exited = process.completed.then(({ code }) => code, () => -1).finally(async () => {
        recovery?.signal.removeEventListener("abort", abortRecovery);
        await cancellation;
      });
      return { stdout: gated.out, exited, cancel: abortRecovery };
    }
    const child = spawn("git", withDir(cwd, args), { env });
    let killTimer: ReturnType<typeof setTimeout> | undefined;
    const abortRecovery = () => {
      if (killTimer || child.exitCode !== null || child.signalCode !== null) return;
      child.kill("SIGTERM");
      killTimer = setTimeout(() => child.kill("SIGKILL"), 1_000);
    };
    recovery?.signal.addEventListener("abort", abortRecovery, { once: true });
    const timeoutMs = recovery ? Math.min(opts.timeoutMs ?? Infinity, Math.max(1, recovery.deadline - Date.now())) : opts.timeoutMs;
    let timeoutError: Error | undefined;
    const timeout = timeoutMs === undefined ? undefined : setTimeout(() => {
      if (timeoutMs === opts.timeoutMs && !killTimer && child.exitCode === null && child.signalCode === null) {
        timeoutError = opts.nativeTimeoutError;
      }
      abortRecovery();
    }, timeoutMs);
    child.once("close", () => {
      recovery?.signal.removeEventListener("abort", abortRecovery);
      if (timeout) clearTimeout(timeout);
      if (killTimer) clearTimeout(killTimer);
    });
    if (recovery?.signal.aborted) abortRecovery();
    const exited = new Promise<number>((resolve) => {
      child.once("error", () => resolve(-1));
      child.once("close", (code) => resolve(code ?? -1));
    });
    const gated = exitGatedStream(child.stdout as Readable, () => {
      if (opts.cancelOnAbandon) abortRecovery();
      else if (child.exitCode === null && child.signalCode === null) child.kill();
    });
    const stderrChunks: Buffer[] = [];
    const stderrCap = opts.stderrMaxBytes;
    let stderrBytes = 0;
    child.stderr?.on("data", (c: Buffer) => {
      if (stderrCap === undefined) {
        stderrChunks.push(c);
        return;
      }
      if (stderrBytes >= stderrCap) return;
      const kept = c.subarray(0, stderrCap - stderrBytes);
      stderrChunks.push(kept);
      stderrBytes += kept.length;
    });
    child.on("error", (err) => gated.exited(err));
    child.on("close", (code) => {
      if (timeoutError) gated.exited(timeoutError);
      else if (code !== 0) {
        const detail = Buffer.concat(stderrChunks).toString().trim();
        gated.exited(new Error(`git ${args.join(" ")} exited ${code ?? "signal"}${detail ? `: ${detail}` : ""}`));
      } else gated.exited();
    });
    if (child.stdin) {
      child.stdin.on("error", () => undefined); // see the scoped branch above
      child.stdin.end(stdin ?? "");
    }
    return { child, stdout: gated.out, exited, cancel: abortRecovery };
  }

  /**
   * Capture raw planning bytes as the command identity. Refusals throw; these
   * bytes still require scanPatchForSecrets before any planning consumer.
   */
  async capturePlanningDiff(clonePath: string, baseCommit: string): Promise<Buffer> {
    const root = path.resolve(this.runnerRoot);
    if (!path.isAbsolute(clonePath) || path.resolve(clonePath) !== clonePath
        || !isWithinPath(clonePath, root) || path.dirname(path.dirname(clonePath)) !== root) {
      throw new Error("planning clone must be <runnerRoot>/<repoDir>/<key>");
    }
    if (!/^[a-f0-9]{40}$/.test(baseCommit)) throw new Error("planning base must be immutable 40-hex");
    return this.readBoundedPlanningOutput(clonePath,
      [process.execPath, "-e", PLANNING_CAPTURE_HELPER, clonePath, baseCommit, GIT_BIN,
        JSON.stringify(this.planningLimits)], 30_000);
  }

  /** @internal Test-only: inject small planning-capture budgets. */
  setPlanningLimitsForTest(limits: { snapshotLimit: number; totalLimit: number }): void {
    this.planningLimits = { ...limits };
  }

  /**
   * Transport for the trusted planning reader: argv must never come from clone
   * configuration. Captured bytes still require a trusted clean secret scan.
   * Require a whole-root owner rather than a legacy spawn fallback.
   * @internal Transport for the planning reader; not a source-capture API.
   */
  async readBoundedPlanningOutput(
    cwd: string,
    argv: readonly string[],
    timeoutMs = GIT_TIMEOUT_MS,
  ): Promise<Buffer> {
    const boundary = this.boundaryProcesses.getStore();
    if (!boundary) throw new Error("bounded runner stdout requires a trusted boundary spawner");
    if (!path.isAbsolute(cwd) || !argv[0] || !path.isAbsolute(argv[0])) {
      throw new Error("bounded runner stdout requires trusted absolute executable and clone cwd");
    }
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) throw new Error("invalid stdout deadline");
    if (boundary.signal.aborted) throw new GitBoundaryAbortError(GIT_OUTPUT_ABORT_MESSAGE);
    const env = unmarkedSpawnEnv({ ...gitEnv(), PATH: runnerPath() });
    const tmp = runnerTmpdir();
    if (tmp) env.TMPDIR = tmp;
    const handle = await boundary.spawn({ argv, cwd, env, identity: "command", timeoutMs });
    // Attach both paused collectors before any further await. An async spawner
    // may hand us pipes after child_process flushStdio: refuse any prior read.
    const stop = new AbortController();
    const stdout = collectBoundedRunnerPipe(handle.stdout, stop.signal);
    const stderr = collectBoundedRunnerPipe(handle.stderr, stop.signal);
    const onAbort = (): void => stop.abort(new GitBoundaryAbortError(GIT_OUTPUT_ABORT_MESSAGE));
    boundary.signal.addEventListener("abort", onAbort, { once: true });
    const timer = setTimeout(() => stop.abort(new Error("bounded runner stdout timed out")), timeoutMs);
    let rejectAbort!: (error: unknown) => void;
    const aborted = new Promise<never>((_resolve, reject) => { rejectAbort = reject; });
    const onStop = (): void => rejectAbort(stop.signal.reason);
    stop.signal.addEventListener("abort", onStop, { once: true });
    if (boundary.signal.aborted) onAbort();
    handle.stdin?.on("error", () => undefined);
    handle.stdin?.end();
    const all = Promise.all([stdout.result, stderr.result, handle.completed] as const);
    try {
      const [bytes, diagnostic, terminal] = await Promise.race([all, aborted]);
      if (terminal.code !== 0 || terminal.softTimedOut) throw new Error("bounded runner process failed: " + diagnostic.toString());
      return bytes;
    } catch (error) {
      stop.abort(error);
      // One cancellation attempt; the trusted owner bounds termination/reap.
      // A refused cleanup overrides the output error, and no bytes escape.
      const cleanup = await Promise.allSettled([handle.cancel(), handle.completed]);
      await Promise.allSettled([stdout.result, stderr.result]);
      if (cleanup.some((result) => result.status === "rejected")) {
        throw new Error("bounded runner stdout cleanup failed", { cause: error });
      }
      throw error;
    } finally {
      clearTimeout(timer);
      boundary.signal.removeEventListener("abort", onAbort);
      stop.signal.removeEventListener("abort", onStop);
      stdout.dispose();
      stderr.dispose();
    }
  }

  /**
   * Run a git op as the RUNNER uid (PRD #51 M4) — the runner-clone seed + checkout,
   * which must be runner-owned. NEVER carries a PAT (a local, non-credentialed op), and
   * runs on gitEnv's config pins (safe.directory / hooksPath / M0 code-exec-key pins)
   * but with the RUNNER PATH + the runner's private TMPDIR so `git` resolves and its
   * scratch lands on the runner's 0700 tmp (not the worker's, which the runner cannot
   * write). Single-uid (#58): `runnerCommand` is a passthrough, so this is a plain git.
   * issue #1783: carries the worker spawn mark only for {@link runnerGitCarriesWorkerMark}'s
   * subcommands; every other runner git runs unmarked (its plants stay reapable).
   */
  private async runGitAsRunner(cwd: string | undefined, args: string[], opts: { timeoutMs?: number } = {}): Promise<string> {
    const base = gitEnv();
    // issue #1783 (R4): worker-marked ONLY when the subcommand can run no code the clone
    // configures (runnerGitCarriesWorkerMark), so a concurrent reap never kills that op. Every
    // other runner git here (status, add, commit, checkout, reset, merge, rebase, …) can start an
    // agent-planted filter/driver, which must stay reapable: it runs UNMARKED.
    const runnerEnv: NodeJS.ProcessEnv = { ...base, PATH: runnerPath() };
    const marked = runnerGitCarriesWorkerMark(args);
    const env: NodeJS.ProcessEnv = marked ? workerSpawnEnv(runnerEnv) : unmarkedSpawnEnv(runnerEnv);
    // Belt to gitEnv's GIT_NO_LAZY_FETCH for the marked subset: those reads need no transport at
    // all, so GIT_ALLOW_PROTOCOL names no protocol and git refuses every transport. The env var
    // overrides EVERY `protocol.allow` / `protocol.<name>.allow` config key, so a planted
    // `[protocol "file"] allow = always` in the agent-writable .git/config cannot re-enable one
    // (a `protocol.allow=never` config pin could be: the per-protocol key outranks it). The seed's
    // `clone --no-checkout` reads the bare over the local transport, so it is exempt.
    if (marked && firstRunnerGitSubcommand(args) !== "clone") env.GIT_ALLOW_PROTOCOL = RUNNER_GIT_NO_PROTOCOL;
    const tmp = runnerTmpdir();
    if (tmp) env.TMPDIR = tmp;
    // A permit-scoped subprocess is already launched as the isolated command uid
    // by Codex safety. Applying the legacy setpriv-to-runner wrapper inside that
    // cap-less uid would fail (and would try to cross identities twice).
    const wrapped = this.boundaryProcesses.getStore()
      ? { command: GIT_BIN, args: withDir(cwd, args) }
      : runnerCommand("git", withDir(cwd, args));
    this.log.debug("git (runner uid)", { cwd, args });
    try {
      const { stdout } = await this.execScoped(wrapped.command, wrapped.args, {
        env,
        timeout: opts.timeoutMs ?? GIT_TIMEOUT_MS,
        maxBuffer: GIT_MAX_BUFFER,
      }, "command");
      return stdout;
    } catch (err) {
      // Preserve git's numeric exit code on the wrapped error so a caller can discriminate
      // an expected non-zero status (e.g. `config --unset-all` exit 5 = key absent) from a
      // real failure — the same `.code` idiom `tryGit` reads off the raw execFile error.
      const failure = new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`);
      const code = (err as { code?: unknown }).code;
      if (typeof code === "number") (failure as { code?: number }).code = code;
      throw failure;
    }
  }

  /**
   * issue #1769 — spawn a RUNNER-uid git in `cwd` whose stdin the caller streams (the
   * `index-pack --stdin` consumer of {@link ensureRunnerCloneObjects}). Same env as
   * {@link runGitAsRunner} (credential-free gitEnv pins, runner PATH/TMPDIR). Inside a boundary
   * the child is `boundary.spawn`ed with identity `command` (the command sandbox rooted at the
   * clone; the safety facade registers it as a `boundary_action` root and its `completed`
   * includes that root's reap); outside, `runnerCommand` + a plain spawn. stdout is drained and
   * stderr kept (capped) for the failure message. `exited` never rejects (-1 on a spawn error,
   * a signal death or a rejected completion); `abort` ends the child's input and, outside a
   * boundary, kills it, so the caller can always await `exited` to a terminal state.
   */
  private async spawnGitAsRunnerWithStdin(
    cwd: string,
    args: string[],
  ): Promise<{ stdin: Writable; exited: Promise<{ code: number; stderr: string }>; abort: () => void }> {
    const base = gitEnv();
    const env: NodeJS.ProcessEnv = { ...base, PATH: runnerPath() };
    const tmp = runnerTmpdir();
    if (tmp) env.TMPDIR = tmp;
    this.log.debug("git (runner uid, stdin)", { cwd, args });
    const stderrChunks: Buffer[] = [];
    let stderrBytes = 0;
    const keepStderr = (c: Buffer | string): void => {
      if (stderrBytes >= GIT_MAX_BUFFER) return;
      const chunk = (Buffer.isBuffer(c) ? c : Buffer.from(c)).subarray(0, GIT_MAX_BUFFER - stderrBytes);
      stderrChunks.push(chunk);
      stderrBytes += chunk.length;
    };
    const stderrText = (): string => Buffer.concat(stderrChunks).toString().trim();
    const boundary = this.boundaryProcesses.getStore();
    const recovery = this.recoveryOperations.getStore();
    recovery?.signal.throwIfAborted();
    if (boundary) {
      const process = await boundary.spawn({ argv: [GIT_BIN, ...withDir(cwd, args)], cwd, env, identity: "command",
        ...(recovery ? { timeoutMs: Math.max(1, recovery.deadline - Date.now()) } : {}),
      });
      let cancellation: Promise<unknown> | undefined;
      const abortRecovery = () => { cancellation = process.cancel().catch(() => undefined); };
      recovery?.signal.addEventListener("abort", abortRecovery, { once: true });
      if (recovery?.signal.aborted) abortRecovery();
      void process.completed.finally(() => recovery?.signal.removeEventListener("abort", abortRecovery)).catch(() => undefined);
      if (!process.stdin) {
        // Nothing can be streamed; still settle the root before reporting.
        await process.completed.catch(() => undefined);
        throw new Error("supervised git process has no stdin");
      }
      const stdin = process.stdin;
      stdin.on("error", () => undefined);
      process.stdout?.resume();
      process.stderr?.on("data", keepStderr);
      const exited = process.completed.then(
        ({ code }) => ({ code, stderr: stderrText() }),
        (error: unknown) => ({ code: -1, stderr: stderrText() || gitErrorMessage(error) }),
      ).finally(async () => { await cancellation; });
      return { stdin, exited, abort: () => stdin.destroy() };
    }
    const wrapped = runnerCommand("git", withDir(cwd, args));
    const child = spawn(wrapped.command, wrapped.args, { env, stdio: ["pipe", "pipe", "pipe"] });
    const abortRecovery = () => { child.stdin.destroy(); child.kill("SIGKILL"); };
    recovery?.signal.addEventListener("abort", abortRecovery, { once: true });
    const timer = recovery ? setTimeout(abortRecovery, Math.max(1, recovery.deadline - Date.now())) : undefined;
    child.once("close", () => {
      recovery?.signal.removeEventListener("abort", abortRecovery);
      if (timer) clearTimeout(timer);
    });
    if (recovery?.signal.aborted) abortRecovery();
    child.stdin.on("error", () => undefined);
    child.stdout.resume();
    child.stderr.on("data", keepStderr);
    const exited = new Promise<{ code: number; stderr: string }>((resolve) => {
      child.once("error", (error) => resolve({ code: -1, stderr: stderrText() || gitErrorMessage(error) }));
      child.once("close", (code) => resolve({ code: code ?? -1, stderr: stderrText() }));
    });
    return {
      stdin: child.stdin,
      exited,
      abort: () => {
        child.stdin.destroy();
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      },
    };
  }

  /** Run git, returning the exit code (0 on success) instead of throwing. */
  private async tryGit(cwd: string | undefined, args: string[], pat?: string): Promise<number> {
    // issue #2213: built outside the try so a quarantine refusal is never read as an exit code.
    const env = gitEnv(pat);
    try {
      await this.execScoped("git", withDir(cwd, args), { env, timeout: GIT_TIMEOUT_MS });
      return 0;
    } catch (err) {
      if (err instanceof RunResidueBlockedError) throw err;
      const abort = this.boundaryAbortError(err);
      if (abort) throw abort;
      const code = (err as { code?: unknown }).code;
      return typeof code === "number" ? code : 1;
    }
  }

  /** issue #1416 — like {@link tryGit} but discriminates a GENUINE numeric git exit status from a
   *  non-completion: returns the numeric exit code (0 on success) when the process ran to
   *  completion, or `null` when it never produced a real exit status — a spawn failure (code is a
   *  string, e.g. "ENOENT"), a timeout/SIGTERM kill, or any signal death (code is `null`). tryGit
   *  keeps its historical coerce-to-1 for callers that only care whether the op succeeded; this
   *  sibling exists for callers (ancestry) that MUST NOT treat a broken read as a data answer. */
  private async tryGitExit(cwd: string | undefined, args: string[], pat?: string): Promise<number | null> {
    const env = gitEnv(pat);
    try {
      await this.execScoped("git", withDir(cwd, args), { env, timeout: GIT_TIMEOUT_MS });
      return 0;
    } catch (err) {
      if (err instanceof RunResidueBlockedError) throw err;
      const abort = this.boundaryAbortError(err);
      if (abort) throw abort;
      const code = (err as { code?: unknown }).code;
      return typeof code === "number" ? code : null;
    }
  }

  /** issue #909 — read the tracking-ref owner stamp. Prefer the #887 subsection form; fall back
   *  to the pre-#887 flattened form for a bare stamped under old code during the rollout window.
   *  The fallback is COLLISION-AWARE: the flat key is not branch-injective, so it is consulted
   *  only when no OTHER live tracking ref flattens to the same key. The decisive second guard is
   *  the caller's runId-equality test (a foreign branch's stamp carries a different, globally
   *  unique runId). Returns "" when neither form is present. Best-effort throughout. */
  private async readTrackingOwner(barePath: string, branch: string): Promise<string> {
    try {
      await this.assertOwedBare(barePath);
      const sha = await this.checkedTrackingSha(barePath, branch);
      return sha ? (await this.checkedTrackingOwner(barePath, branch, sha))?.runId ?? "" : "";
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return "";
    }
  }

  /** issue #909 — true when a DISTINCT live tracking ref (refs/uzi-runner/<branch>) flattens to
   *  the same pre-#887 flat owner key as `branch`, making a legacy flat stamp unattributable.
   *  Enumerates the tracking namespace; the ref suffix is the branch. Best-effort. */
  private async flatOwnerKeyAmbiguous(barePath: string, branch: string): Promise<boolean> {
    const token = branch.replace(/[^A-Za-z0-9_-]/g, "-").toLowerCase();
    const out = await this.runGit(barePath, ["for-each-ref", "--format=%(refname)", RUNNER_TRACKING_PREFIX]);
    if (!out.trim()) return false;
    for (const ref of out.split("\n")) {
      if (!ref.startsWith(RUNNER_TRACKING_PREFIX)) continue;
      const other = ref.slice(RUNNER_TRACKING_PREFIX.length);
      if (other === branch) continue;
      if (other.replace(/[^A-Za-z0-9_-]/g, "-").toLowerCase() === token) return true;
    }
    return false;
  }

  /** Worker-only directories are the ownership root; runner-provided Git config is not. */
  private async assertOwedBare(barePath: string): Promise<void> {
    const root = path.resolve(this.reposRoot);
    if (typeof barePath !== "string" || path.dirname(barePath) !== root || path.resolve(barePath) !== barePath) {
      throw new Error("owed candidates require a known worker bare path");
    }
    for (const dir of [root, barePath]) {
      const st = await fs.lstat(dir);
      if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== process.getuid?.() ||
          (st.mode & 0o002) !== 0 ||
          ((st.mode & 0o020) !== 0 && (st.gid !== process.getgid?.() ||
            (uidSplitActive() && st.gid === RUNNER_UID)))) throw new Error("unsafe worker bare directory");
    }
    if ((await this.runGit(barePath, ["rev-parse", "--is-bare-repository"])).trim() !== "true") {
      throw new Error("owed candidates require a bare repository");
    }
  }

  private snapshotOwedOptions(opts: FetchAgentBranchOptions): FetchAgentBranchOptions {
    const c = opts.context;
    return { context: { ...c, defaultIdentity: c?.defaultIdentity && { ...c.defaultIdentity } },
      remotelyConfirmedSha: opts.remotelyConfirmedSha };
  }

  private async validateOwedContext(barePath: string, branch: string, c: StoredOwedContext,
    historical = false, stored = false): Promise<void> {
    const raw = c as unknown as Record<string, unknown>;
    const isHistorical = raw?.origin === "historical";
    const expectedKeys = isHistorical
      ? "barePath,branch,defaultIdentity,generation,kind,origin,producer,runId"
      : c?.generation === null ? "barePath,branch,defaultIdentity,generation,kind,legacy,runId"
        : "barePath,branch,defaultIdentity,generation,kind,runId";
    const validShape = !!c && Object.keys(c).sort().join(",") === expectedKeys;
    const validGeneration = !!c && (isHistorical
      ? historical && c.generation === null && raw.producer === "unknown" &&
        c.kind === null && c.defaultIdentity === null && raw.legacy === undefined
      : (c.generation === null ? raw.legacy === true
        : Number.isSafeInteger(c.generation) && c.generation > 0 && raw.legacy === undefined));
    if (!c || !validShape || typeof c.runId !== "string" || !OWED_RUN_ID.test(c.runId) || !validGeneration ||
        typeof branch !== "string" || typeof barePath !== "string" ||
        typeof c.branch !== "string" || typeof c.barePath !== "string" ||
        c.branch !== branch || c.barePath !== barePath ||
        (!isHistorical && (!(RUN_KINDS as readonly unknown[]).includes(c.kind) ||
          !c.defaultIdentity || Object.keys(c.defaultIdentity).sort().join(",") !== "ref,sha" ||
          typeof c.defaultIdentity.ref !== "string" ||
          !c.defaultIdentity.ref.startsWith("refs/") || typeof c.defaultIdentity.sha !== "string" || !OWED_OID.test(c.defaultIdentity.sha)))) {
      throw new Error("invalid owed candidate context");
    }
    await this.assertOwedBare(barePath);
    if (!(await this.isPlainBranchName(barePath, branch))) throw new Error("invalid owed branch");
    if (c.defaultIdentity) {
      await this.runGit(barePath, ["check-ref-format", c.defaultIdentity.ref]);
      if (!stored) {
        const name = this.contextName(c);
        const record = await this.readOwedFile(barePath, name);
        if (record === undefined) {
          // First admission proves the default identity is an actual commit.
          await this.requireOwedCommit(barePath, c.defaultIdentity.sha);
        } else {
          // Worker-owned immutable context proves identity, not object reachability
          // or remote containment. Its original default commit may have been pruned.
          const proven = await this.parseOwedContext(barePath, record);
          if (this.contextName(proven) !== name || "origin" in proven ||
              proven.runId !== c.runId || proven.branch !== c.branch || proven.barePath !== c.barePath ||
              proven.generation !== c.generation || proven.kind !== c.kind ||
              proven.defaultIdentity.ref !== c.defaultIdentity.ref ||
              proven.defaultIdentity.sha !== c.defaultIdentity.sha) {
            throw new Error("owed context identity mismatch");
          }
        }
      }
    }
  }

  private validateConfirmedSha(sha: string | undefined): void {
    if (sha !== undefined && (typeof sha !== "string" || !OWED_OID.test(sha))) throw new Error("invalid remotely confirmed SHA");
  }

  private async requireOwedCommit(barePath: string, sha: string): Promise<void> {
    if (typeof sha !== "string" || !OWED_OID.test(sha) ||
        (await this.runGit(barePath, ["--no-replace-objects", "cat-file", "-t", sha])).trim() !== "commit") {
      throw new Error("owed candidate is not an actual lowercase commit OID");
    }
  }

  private owedDirectory(barePath: string): string { return path.join(barePath, "uzi-owed"); }
  private receiptName(branch: string): string {
    return createHash("sha256").update(branch).digest("hex");
  }

  private async ensureOwedDirectory(barePath: string): Promise<string> {
    const dir = this.owedDirectory(barePath);
    try {
      await fs.mkdir(dir, { mode: 0o700 });
      const parent = await fs.open(barePath, fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW);
      try { await parent.sync(); } finally { await parent.close(); }
    } catch (cause) {
      if ((cause as NodeJS.ErrnoException).code !== "EEXIST") throw cause;
    }
    const st = await fs.lstat(dir);
    if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0) {
      throw new Error("unsafe owed metadata directory");
    }
    return dir;
  }

  /** ENOENT alone means absent. Bounded regular-file no-follow read, worker ownership required. */
  private async readOwedFile(barePath: string, name: string): Promise<unknown | undefined> {
    const dir = this.owedDirectory(barePath);
    try {
      const st = await fs.lstat(dir);
      if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0) {
        throw new Error("unsafe owed metadata directory");
      }
    } catch (cause) {
      if ((cause as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw cause;
    }
    let file: import("node:fs/promises").FileHandle;
    try {
      file = await fs.open(path.join(dir, name), fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
    } catch (cause) {
      if ((cause as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw cause;
    }
    try {
      const st = await file.stat();
      if (!st.isFile() || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0 || st.size > 65536) {
        throw new Error("unsafe owed metadata file");
      }
      const buf = Buffer.alloc(65537);
      const { bytesRead } = await file.read(buf, 0, buf.length, 0);
      if (bytesRead > 65536) throw new Error("oversized owed metadata");
      return JSON.parse(buf.toString("utf8", 0, bytesRead)) as unknown;
    } finally { await file.close(); }
  }

  /** Temp is exclusive/no-follow, synced before rename; rename and directory sync precede readback. */
  private async writeOwedFile(barePath: string, name: string, value: unknown): Promise<void> {
    const dir = await this.ensureOwedDirectory(barePath);
    const temp = path.join(dir, `.tmp-${randomUUID()}`);
    const file = await fs.open(temp, fsConstants.O_WRONLY | fsConstants.O_CREAT |
      fsConstants.O_EXCL | fsConstants.O_NOFOLLOW, 0o600);
    try {
      await file.writeFile(JSON.stringify(value) + "\n");
      await file.sync();
    } finally { await file.close(); }
    try {
      // Read an existing destination to reject symlinks, FIFOs and foreign-owned files.
      await this.readOwedFile(barePath, name);
      await fs.rename(temp, path.join(dir, name));
      const directory = await fs.open(dir, fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW);
      try { await directory.sync(); } finally { await directory.close(); }
      if (JSON.stringify(await this.readOwedFile(barePath, name)) !== JSON.stringify(value)) {
        throw new Error("owed metadata readback mismatch");
      }
    } finally { await fs.rm(temp, { force: true }); }
  }

  private async removeOwedFile(barePath: string, name: string, allowMissing = false): Promise<void> {
    const dir = this.owedDirectory(barePath);
    try { await fs.unlink(path.join(dir, name)); }
    catch (err) {
      if (allowMissing && (err as NodeJS.ErrnoException).code === "ENOENT") return;
      throw err;
    }
    const handle = await fs.open(dir, fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW);
    try { await handle.sync(); } finally { await handle.close(); }
  }

  private contextName(c: StoredOwedContext): string {
    const identity = [c.runId, c.branch, c.barePath, c.generation, c.kind,
      c.defaultIdentity?.ref ?? null, c.defaultIdentity?.sha ?? null,
      "origin" in c ? "historical" : c.generation === null ? "legacy" : "claim"];
    return `context-${createHash("sha256").update(JSON.stringify(identity)).digest("hex")}.json`;
  }

  private async persistOwedContext(c: StoredOwedContext): Promise<void> {
    const name = this.contextName(c);
    const record = { version: 1, runId: c.runId, branch: c.branch, barePath: c.barePath,
      generation: c.generation, kind: c.kind, defaultIdentity: c.defaultIdentity
        ? { ref: c.defaultIdentity.ref, sha: c.defaultIdentity.sha } : null,
      ...("origin" in c ? { origin: c.origin, producer: c.producer }
        : c.generation === null ? { legacy: true } : {}) };
    const existing = await this.readOwedFile(c.barePath, name);
    if (existing !== undefined) {
      if (JSON.stringify(existing) !== JSON.stringify(record)) throw new Error("owed context identity changed");
      return;
    }
    await this.writeOwedFile(c.barePath, name, record);
  }

  private async parseOwedContext(barePath: string, value: unknown): Promise<StoredOwedContext> {
    const v = value as StoredOwedContext & { version?: unknown };
    const keys = v && Object.keys(v).sort().join(",");
    const expected = v && "origin" in v
      ? "barePath,branch,defaultIdentity,generation,kind,origin,producer,runId,version"
      : v?.generation === null
        ? "barePath,branch,defaultIdentity,generation,kind,legacy,runId,version"
        : "barePath,branch,defaultIdentity,generation,kind,runId,version";
    if (!v || v.version !== 1 || keys !== expected ||
        (v.defaultIdentity !== null && Object.keys(v.defaultIdentity ?? {}).sort().join(",") !== "ref,sha")) {
      throw new Error("invalid owed context record");
    }
    const { version: _version, ...c } = v;
    await this.validateOwedContext(barePath, c.branch, c, true, true);
    return c;
  }

  /** Caller MUST hold the bare lock: the count-then-pin below is only atomic under it. Every
   *  pin-producing path (fetchAgentBranch, promoteTrackingUnderLock, clearConflictingAncestorTrackingRefs,
   *  retainCurrentOwedCandidate) funnels through here. The check precedes every write, so a refusal
   *  leaves the existing pins, metadata and refs untouched. */
  private async pinOwedUnderLock(c: StoredOwedContext, sha: string): Promise<void> {
    await this.requireOwedCommit(c.barePath, sha);
    const ref = `refs/uzi-owed/${c.runId}/${sha}`;
    const held = (await this.runGit(c.barePath, ["for-each-ref", "--format=%(refname) %(objectname)", `refs/uzi-owed/${c.runId}/`]))
      .split("\n").filter(Boolean).map((line) => line.split(" ")[0]!);
    if (!held.includes(ref) && held.length >= this.maxOwedCandidates) throw new OwedCandidateLimitError();
    await this.persistOwedContext(c); // Durable discovery precedes the first pin.
    const context = this.contextName(c);
    const name = `candidate-${c.runId}-${sha}-${context.slice(8, -5)}.json`;
    const record = { version: 1, runId: c.runId, context, sha };
    const old = await this.readOwedFile(c.barePath, name);
    if (old !== undefined && JSON.stringify(old) !== JSON.stringify(record)) throw new Error("invalid owed candidate record");
    if (old === undefined) await this.writeOwedFile(c.barePath, name, record);
    await this.runGit(c.barePath, ["update-ref", ref, sha]);
    if (await this.checkedRefSha(c.barePath, ref) !== sha) throw new Error("owed pin readback mismatch");
  }

  /** Checked enumeration distinguishes a genuinely absent exact ref from a failed read. */
  private async checkedRefSha(barePath: string, ref: string): Promise<string | undefined> {
    const out = await this.runGit(barePath, ["for-each-ref", "--format=%(refname) %(objectname)", ref]);
    const matches = out.trim().split("\n").filter((line) => line.startsWith(ref + " "));
    if (matches.length === 0) return undefined;
    if (matches.length !== 1) throw new Error("ambiguous tracking ref");
    const sha = matches[0]!.slice(ref.length + 1);
    await this.requireOwedCommit(barePath, sha);
    return sha;
  }
  private checkedTrackingSha(barePath: string, branch: string): Promise<string | undefined> {
    return this.checkedRefSha(barePath, runnerTrackingRef(branch));
  }

  private async checkedOwnerStamp(barePath: string, branch: string, allowLegacy: boolean): Promise<string | undefined> {
    const raw = await this.runGit(barePath, ["config", "--local", "--null", "--list"]);
    const values = (key: string): string[] => raw.split("\0")
      .filter((item) => item.startsWith(key + "\n")).map((item) => item.slice(key.length + 1));
    let found = values(runnerTrackingOwnerKey(branch));
    if (found.length === 0 && allowLegacy) {
      const key = legacyFlatTrackingOwnerKey(branch);
      found = raw.split("\0").filter((item) => item.slice(0, item.indexOf("\n")).toLowerCase() === key)
        .map((item) => item.slice(item.indexOf("\n") + 1));
      if (found.length && await this.flatOwnerKeyAmbiguous(barePath, branch)) return undefined;
    }
    return found.length === 1 && OWED_RUN_ID.test(found[0]!) ? found[0] : undefined;
  }

  private async checkedReceiptGovernance(barePath: string, branch: string): Promise<"absent" | "governed" | "invalid"> {
    const key = `uzi-trackowner.${branch}.receiptversion`;
    const raw = await this.runGit(barePath, ["config", "--local", "--null", "--list"]);
    const values = raw.split("\0").filter((item) => item.startsWith(key + "\n"))
      .map((item) => item.slice(key.length + 1));
    return values.length === 0 ? "absent" : values.length === 1 && values[0] === "1" ? "governed" : "invalid";
  }

  private async checkedTrackingOwner(barePath: string, branch: string, sha: string): Promise<CheckedTrackingOwner> {
    const governance = await this.checkedReceiptGovernance(barePath, branch);
    if (governance === "invalid") return undefined;
    const hash = this.receiptName(branch);
    const marker = await this.readOwedFile(barePath, `governed-${hash}.json`);
    const receipt = await this.readOwedFile(barePath, `receipt-${hash}.json`) as TrackingReceipt | undefined;
    if (governance === "absent" && marker === undefined && receipt === undefined) {
      const runId = await this.checkedOwnerStamp(barePath, branch, true);
      return runId ? { runId } : undefined;
    }
    if (JSON.stringify(marker) !== JSON.stringify({ version: 1, branch }) || !receipt ||
        Object.keys(receipt).sort().join(",") !== "branch,context,generation,phase,runId,trackingSha,version" ||
        receipt.version !== 1 || receipt.branch !== branch || typeof receipt.runId !== "string" || !OWED_RUN_ID.test(receipt.runId) ||
        typeof receipt.context !== "string" || !/^context-[0-9a-f]{64}\.json$/.test(receipt.context) ||
        receipt.trackingSha !== sha || receipt.phase !== "committed") return undefined;
    if (await this.checkedOwnerStamp(barePath, branch, false) !== receipt.runId) return undefined;
    const c = await this.parseOwedContext(barePath,
      await this.readOwedFile(barePath, receipt.context));
    if ("origin" in c || c.branch !== branch || c.runId !== receipt.runId ||
        c.generation !== receipt.generation || this.contextName(c) !== receipt.context) return undefined;
    return { runId: receipt.runId, generation: receipt.generation, context: receipt.context };
  }

  private async observeTrackingUnderLock(barePath: string, branch: string):
    Promise<{ sha?: string; owner?: CheckedTrackingOwner } | undefined> {
    try {
      const sha = await this.checkedTrackingSha(barePath, branch);
      const owner = sha ? await this.checkedTrackingOwner(barePath, branch, sha) : undefined;
      if (sha && !owner) return undefined;
      // Even absent refs must not bypass unreadable config/sidecar state.
      if (!sha) {
        await this.checkedOwnerStamp(barePath, branch, false);
        if (await this.checkedReceiptGovernance(barePath, branch) === "invalid") return undefined;
        const hash = this.receiptName(branch);
        const marker = await this.readOwedFile(barePath, `governed-${hash}.json`);
        const receipt = await this.readOwedFile(barePath, `receipt-${hash}.json`);
        if (marker !== undefined && JSON.stringify(marker) !== JSON.stringify({ version: 1, branch })) return undefined;
        if (receipt !== undefined) return undefined;
      }
      return { sha, owner };
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return undefined;
    }
  }

  private async remotelyCovers(barePath: string, sha: string, confirmed: string | undefined): Promise<boolean> {
    if (confirmed === undefined) return false;
    try {
      await this.requireOwedCommit(barePath, confirmed);
      return await this.ancestry(barePath, sha, confirmed) === "ancestor";
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return false;
    }
  }

  private async producingContext(barePath: string, branch: string, owner: CheckedTrackingOwner): Promise<StoredOwedContext> {
    if (!owner) throw new Error("missing producing owner");
    if (!owner.context) {
      return { barePath, branch, runId: owner.runId, generation: null, origin: "historical",
        producer: "unknown", kind: null, defaultIdentity: null };
    }
    const c = await this.parseOwedContext(barePath, await this.readOwedFile(barePath, owner.context));
    if (c.branch !== branch || c.runId !== owner.runId || c.generation !== owner.generation ||
        this.contextName(c) !== owner.context) throw new Error("producing context mismatch");
    return c;
  }

  /** Local committed ownership only; this is never confirmation of remote containment. */
  async committedTrackingOwnership(barePath: string, branch: string, runId: string,
    expectedSha?: string, generation?: number): Promise<CommittedTrackingOwnership> {
    if (typeof runId !== "string" || !OWED_RUN_ID.test(runId) ||
        typeof branch !== "string" || typeof barePath !== "string" ||
        (expectedSha !== undefined && (typeof expectedSha !== "string" || !OWED_OID.test(expectedSha))) ||
        (generation !== undefined && (!Number.isSafeInteger(generation) || generation <= 0))) {
      return { kind: "not_owned" };
    }
    try { await this.assertOwedBare(barePath); }
    catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_owned" };
    }
    return this.withLock(barePath, async () => {
      const observed = await this.observeTrackingUnderLock(barePath, branch);
      if (!observed?.sha || !observed.owner?.context || observed.owner.runId !== runId ||
          (expectedSha !== undefined && observed.sha !== expectedSha) ||
          (generation !== undefined && observed.owner.generation !== generation)) return { kind: "not_owned" };
      try {
        const context = await this.parseOwedContext(barePath,
          await this.readOwedFile(barePath, observed.owner.context));
        if ("origin" in context || this.contextName(context) !== observed.owner.context ||
            context.runId !== runId || context.branch !== branch ||
            context.generation !== observed.owner.generation) return { kind: "not_owned" };
        return { kind: "owned", sha: observed.sha, context };
      } catch (cause) {
        const abort = this.boundaryAbortError(cause);
        if (abort) throw abort;
        return { kind: "not_owned" };
      }
    });
  }

  private async hasOwedAnchorUnderLock(barePath: string, runId: string, sha: string): Promise<boolean> {
    return (await this.enumerateOwedUnderLock(barePath, runId)).some((c) => c.sha === sha);
  }

  private async promoteTrackingUnderLock(barePath: string, branch: string, incoming: string,
    observed: { sha?: string; owner?: CheckedTrackingOwner }, opts: FetchAgentBranchOptions): Promise<TrackingUpdateResult> {
    const c = opts.context;
    if (c.generation === null && observed.owner?.runId === c.runId && observed.owner.generation !== undefined &&
        observed.owner.generation !== null) return { kind: "not_updated", reason: "ownership_unknown" };
    const retainedShas: string[] = [];
    let divergence: "none" | "ancestor" | "divergent" | "unknown" | "foreign" = "none";
    if (observed.sha) divergence = observed.owner?.runId === c.runId
      ? await this.ancestry(barePath, observed.sha, incoming) : "foreign";
    try {
      if (observed.sha && observed.owner &&
          (observed.owner.runId !== c.runId ||
            !(await this.remotelyCovers(barePath, observed.sha, opts.remotelyConfirmedSha)))) {
        // Only this run's confirmation can discharge its own debt.
        await this.pinOwedUnderLock(await this.producingContext(barePath, branch, observed.owner), observed.sha);
        if (observed.owner.runId === c.runId) retainedShas.push(observed.sha);
      }
      await this.persistOwedContext(c);
      if (!(await this.remotelyCovers(barePath, incoming, opts.remotelyConfirmedSha))) {
        await this.pinOwedUnderLock(c, incoming);
        retainedShas.push(incoming);
      }
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: preservationReason(cause) };
    }
    try {
      const key = `uzi-trackowner.${branch}.receiptversion`;
      await this.runGit(barePath, ["config", "--local", "--replace-all", key, "1"]);
      if (await this.checkedReceiptGovernance(barePath, branch) !== "governed") {
        throw new Error("receipt governance readback mismatch");
      }
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: "receipt_pending_failed" };
    }
    try {
      const cleared = await this.clearConflictingAncestorTrackingRefs(barePath, runnerTrackingRef(branch), opts);
      if (!cleared) return { kind: "not_updated", reason: "ownership_unknown" };
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: preservationReason(cause) };
    }
    const receipt: TrackingReceipt = { version: 1, branch, runId: c.runId, generation: c.generation, context: this.contextName(c),
      trackingSha: incoming, phase: "pending" };
    try {
      await this.writeOwedFile(barePath, `governed-${this.receiptName(branch)}.json`, { version: 1, branch });
      await this.writeOwedFile(barePath, `receipt-${this.receiptName(branch)}.json`, receipt);
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: "receipt_pending_failed" };
    }
    try {
      await this.runGit(barePath, ["update-ref", runnerTrackingRef(branch), incoming, observed.sha ?? "0".repeat(40)]);
      if (await this.checkedTrackingSha(barePath, branch) !== incoming) throw new Error("tracking CAS readback mismatch");
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: "tracking_update_failed" };
    }
    try {
      await this.runGit(barePath, ["config", "--local", "--replace-all", runnerTrackingOwnerKey(branch), c.runId]);
      if (await this.checkedOwnerStamp(barePath, branch, false) !== c.runId) throw new Error("owner stamp readback mismatch");
    } catch (cause) {
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: "owner_stamp_failed" };
    }
    try {
      await this.writeOwedFile(barePath, `receipt-${this.receiptName(branch)}.json`, { ...receipt, phase: "committed" });
      const owner = await this.checkedTrackingOwner(barePath, branch, incoming);
      if (owner?.runId !== c.runId || owner.generation !== c.generation || owner.context !== this.contextName(c)) throw new Error("committed owner readback mismatch");
    } catch (cause) {
      // One best-effort invalidation after an indeterminate committed write/readback.
      // No retries: if the disk remains unavailable the original owed pin still survives.
      await this.writeOwedFile(barePath, `receipt-${this.receiptName(branch)}.json`, receipt)
        .catch(() => undefined);
      const abort = this.boundaryAbortError(cause);
      if (abort) throw abort;
      return { kind: "not_updated", reason: "receipt_commit_failed" };
    }
    return { kind: "updated", trackingRef: runnerTrackingRef(branch), candidateSha: incoming,
      retainedShas: [...new Set(retainedShas)].sort(), displacedSha: observed.sha, divergence };
  }

  /** Strict restart enumeration; malformed metadata or pins throw rather than appearing empty. */
  async enumerateOwedCandidates(barePath: string, runId: string): Promise<OwedCandidate[]> {
    if (typeof runId !== "string" || !OWED_RUN_ID.test(runId)) throw new Error("invalid owed run ID");
    await this.assertOwedBare(barePath);
    return this.withLock(barePath, () => this.enumerateOwedUnderLock(barePath, runId));
  }

  private async owedMetadataNames(barePath: string): Promise<string[]> {
    const dir = this.owedDirectory(barePath);
    try {
      const st = await fs.lstat(dir);
      if (!st.isDirectory() || st.isSymbolicLink() || st.uid !== process.getuid?.() || (st.mode & 0o077) !== 0) {
        throw new Error("unsafe owed discovery directory");
      }
      const names = await fs.readdir(dir);
      for (const name of names) {
        if (!/^(?:context-[0-9a-f]{64}|candidate-[A-Za-z0-9_-]+-[0-9a-f]{40}-[0-9a-f]{64}|(?:governed|receipt)-[0-9a-f]{64})\.json$/.test(name) &&
            !/^\.tmp-[0-9a-f-]{36}$/.test(name)) throw new Error("unknown owed metadata entry");
      }
      return names.sort();
    } catch (cause) {
      if ((cause as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw cause;
    }
  }

  /** Read and validate EVERY owed context and candidate file once. Callers that enumerate several
   *  runs of one bare (boot/live discovery) share one result instead of rereading per run or per
   *  context. Any unreadable or malformed entry throws: nothing here ever skips a bad file. */
  private async readOwedMetadataUnderLock(barePath: string): Promise<OwedMetadata> {
    const names = await this.owedMetadataNames(barePath);
    const contexts = new Map<string, StoredOwedContext>();
    const records: OwedMetadata["records"] = [];
    for (const name of names.filter((n) => n.startsWith("context-"))) {
      const c = await this.parseOwedContext(barePath, await this.readOwedFile(barePath, name));
      if (name !== this.contextName(c)) throw new Error("owed context filename mismatch");
      contexts.set(name, c);
    }
    for (const name of names.filter((n) => n.startsWith("candidate-"))) {
      const v = await this.readOwedFile(barePath, name) as { version: number; runId: string; context: string; sha: string };
      if (!v || v.version !== 1 || Object.keys(v).sort().join(",") !== "context,runId,sha,version" ||
          typeof v.runId !== "string" || !OWED_RUN_ID.test(v.runId) || typeof v.sha !== "string" || !OWED_OID.test(v.sha) ||
          typeof v.context !== "string" || !/^context-[0-9a-f]{64}\.json$/.test(v.context) ||
          name !== `candidate-${v.runId}-${v.sha}-${v.context.slice(8, -5)}.json`) throw new Error("invalid owed candidate metadata");
      const c = contexts.get(v.context);
      if (!c || c.runId !== v.runId) throw new Error("owed candidate has no discovery context");
      records.push({ runId: v.runId, sha: v.sha, contextName: v.context, c });
    }
    return { names, contexts, records };
  }

  private async enumerateOwedUnderLock(barePath: string, runId: string, metadata?: OwedMetadata): Promise<OwedCandidate[]> {
    const loaded = metadata ?? await this.readOwedMetadataUnderLock(barePath);
    const records = loaded.records.filter((r) => r.runId === runId);
    const out = await this.runGit(barePath, ["for-each-ref", "--format=%(refname) %(objectname)", `refs/uzi-owed/${runId}/`]);
    const result: OwedCandidate[] = [];
    for (const line of out.trim().split("\n").filter(Boolean)) {
      const m = /^refs\/uzi-owed\/([^/]+)\/([0-9a-f]{40}) ([0-9a-f]{40})$/.exec(line);
      if (!m || m[1] !== runId || m[2] !== m[3]) throw new Error("malformed owed pin");
      const sha = m[2]!;
      await this.requireOwedCommit(barePath, sha);
      const producing = records.filter((r) => r.sha === sha).map((r) => r.c).sort((a, b) =>
        a.generation === null ? (b.generation === null ? this.contextName(a).localeCompare(this.contextName(b)) : -1)
          : b.generation === null ? 1 : a.generation - b.generation || this.contextName(a).localeCompare(this.contextName(b)));
      if (!producing.length) throw new Error("owed pin has no discovery metadata");
      result.push({ sha, pinRef: `refs/uzi-owed/${runId}/${sha}`, contexts: producing });
    }
    return result.sort((a, b) => a.sha.localeCompare(b.sha));
  }

  /** Boot discovery walks only this worker's known bare store, independent of HOME/journals. */
  async discoverOwedCandidates(): Promise<Array<{ context: StoredOwedContext; candidates: OwedCandidate[] }>> {
    const result: Array<{ context: StoredOwedContext; candidates: OwedCandidate[] }> = [];
    let dirs: string[];
    try { dirs = await fs.readdir(this.reposRoot); }
    catch (cause) {
      if ((cause as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw cause;
    }
    for (const name of dirs.sort()) {
      const bare = path.join(path.resolve(this.reposRoot), name);
      await this.assertOwedBare(bare);
      await this.withLock(bare, async () => {
        // One metadata read per bare, and one enumeration (git calls) per run, however many
        // contexts exist: the contexts of a run share its enumeration.
        const metadata = await this.readOwedMetadataUnderLock(bare);
        const allPins = await this.runGit(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-owed/"]);
        const runs = new Set<string>();
        const perRun = new Map<string, OwedCandidate[]>();
        for (const [n, context] of metadata.contexts) {
          runs.add(context.runId);
          let enumerated = perRun.get(context.runId);
          if (!enumerated) {
            enumerated = await this.enumerateOwedUnderLock(bare, context.runId, metadata);
            perRun.set(context.runId, enumerated);
          }
          const candidates = enumerated.filter((candidate) => candidate.contexts.some((c) => this.contextName(c) === n));
          result.push({ context, candidates });
        }
        for (const ref of allPins.trim().split("\n").filter(Boolean)) {
          if (!/^refs\/uzi-owed\/[^/]+\/[0-9a-f]{40}$/.test(ref) || !runs.has(ref.split("/")[2]!)) {
            throw new Error("owed pin has no boot discovery context");
          }
        }
      });
    }
    return result.sort((a, b) => a.context.barePath.localeCompare(b.context.barePath) ||
      a.context.runId.localeCompare(b.context.runId) ||
      (a.context.generation === null ? (b.context.generation === null ? 0 : -1)
        : b.context.generation === null ? 1 : a.context.generation - b.context.generation) ||
      this.contextName(a.context).localeCompare(this.contextName(b.context)));
  }

  /** Before relinquishing a claim, retain its proven current head; no nested bare lock. */
  async retainCurrentOwedCandidate(barePath: string, opts: FetchAgentBranchOptions): Promise<TrackingUpdateResult> {
    opts = this.snapshotOwedOptions(opts);
    await this.validateOwedContext(barePath, opts.context.branch, opts.context);
    this.validateConfirmedSha(opts.remotelyConfirmedSha);
    return this.withLock(barePath, async () => {
      const observed = await this.observeTrackingUnderLock(barePath, opts.context.branch);
      if (!observed?.sha || observed.owner?.runId !== opts.context.runId) {
        return { kind: "not_updated", reason: "ownership_unknown" };
      }
      try {
        const retainedShas: string[] = [];
        if (!(await this.remotelyCovers(barePath, observed.sha, opts.remotelyConfirmedSha))) {
          await this.pinOwedUnderLock(await this.producingContext(barePath, opts.context.branch, observed.owner), observed.sha);
          retainedShas.push(observed.sha);
        }
        return { kind: "updated", trackingRef: runnerTrackingRef(opts.context.branch),
          candidateSha: observed.sha, retainedShas, divergence: "none" };
      } catch (cause) {
        const abort = this.boundaryAbortError(cause);
        if (abort) throw abort;
        return { kind: "not_updated", reason: preservationReason(cause) };
      }
    });
  }

  /** Local archive cleanup is separate from remote publication reconciliation. The caller
   * supplies authenticated FINAL roots and journal pins. One finite pass under the bare lock;
   * unknown recovery-source attribution retains custody; tracking consumers retain contexts.
   * IO failure throws for a later coordinator pass. */
  async cleanupRecoveryGeneration(barePath: string, context: PositiveOwedCandidateContext,
    roots: Array<{ sha: string; contexts: OwedCandidate["contexts"] }>,
    coveragePins: Array<{ fingerprint: string; sha: string; coveredHeads?: string[] }>, recoverySources: string[],
    coveringSha: string, canDelete: () => boolean = () => true,
    authority: "archive" | "completion" | "completion-check" = "archive"): Promise<"removed" | "retained" | "verified"> {
    await this.validateOwedContext(barePath, context.branch, context);
    if (!Number.isSafeInteger(context.generation) || context.generation <= 0) return "retained";
    await this.assertOwedBare(barePath);
    return this.withLock(barePath, async () => {
      const metadata = await this.readOwedMetadataUnderLock(barePath);
      if (metadata.names.some(name => name.startsWith(".tmp-"))) return "retained";
      // Enumerate all physical pins, not just the target run. An orphan candidate after a
      // ref-first crash is valid metadata and remains selectable on the next cleanup pass.
      const allRefs = (await this.runGit(barePath, ["for-each-ref", "--format=%(refname) %(objectname)"]))
        .trim().split("\n").filter(Boolean).map(line => {
          const parts = line.split(" ");
          if (parts.length !== 2 || !OWED_OID.test(parts[1]!)) throw new Error("invalid cleanup ref inventory");
          return { ref: parts[0]!, sha: parts[1]! };
        });
      const runs = new Set([...metadata.contexts.values()].map(c => c.runId));
      for (const pin of allRefs.filter(p => p.ref.startsWith("refs/uzi-owed/"))) {
        const match = /^refs\/uzi-owed\/([^/]+)\/([0-9a-f]{40})$/.exec(pin.ref);
        if (!match || match[2] !== pin.sha || !runs.has(match[1]!)) return "retained";
      }
      for (const run of runs) await this.enumerateOwedUnderLock(barePath, run, metadata);
      // Authenticate receipt/marker consumers even when pending or their tracking ref is gone.
      // Their identity must survive archive cleanup: archive coverage is not publication proof.
      for (const name of metadata.names.filter(n => n.startsWith("receipt-") || n.startsWith("governed-"))) {
        const value = await this.readOwedFile(barePath, name) as TrackingReceipt;
        if (!value || typeof value.branch !== "string" ||
            name !== `${name.startsWith("receipt-") ? "receipt" : "governed"}-${this.receiptName(value.branch)}.json`) return "retained";
        if (name.startsWith("governed-")) {
          if (JSON.stringify(value) !== JSON.stringify({ version: 1, branch: value.branch })) return "retained";
        } else {
          if (Object.keys(value).sort().join(",") !== "branch,context,generation,phase,runId,trackingSha,version" ||
              value.version !== 1 || !OWED_OID.test(value.trackingSha) ||
              !["pending", "committed"].includes(value.phase)) return "retained";
          const consumer = metadata.contexts.get(value.context);
          if (!consumer || consumer.branch !== value.branch || consumer.runId !== value.runId ||
              consumer.generation !== value.generation) return "retained";
        }
      }
      // Config and physical tracking refs are consumers too. An indeterminate tracking owner
      // cannot be treated as publication or as proof that a context is unconsumed.
      const config = await this.runGit(barePath, ["config", "--local", "--null", "--list"]);
      const exact = metadata.records.filter(r => r.runId === context.runId && r.c.generation === context.generation);
      for (const record of exact) {
        if ("origin" in record.c || !roots.some(root => root.sha === record.sha &&
            root.contexts.some(c => !("origin" in c) && this.contextName(c) === record.contextName))) return "retained";
        if (authority === "archive" && await this.ancestry(barePath, record.sha, coveringSha) !== "ancestor") return "retained";
      }
      const ownedPins = allRefs.filter(p => p.ref.startsWith(`refs/uzi-coverage/${context.runId}/${context.generation}/`) ||
        p.ref === recoveryPinRef(context.runId, context.generation));
      for (const pin of ownedPins) {
        const known = pin.ref === recoveryPinRef(context.runId, context.generation)
          ? recoverySources.includes(pin.sha)
          : coveragePins.some(p => /^[0-9a-f]{64}$/.test(p.fingerprint) &&
            pin.ref === `refs/uzi-coverage/${context.runId}/${context.generation}/${p.fingerprint}` && pin.sha === p.sha);
        if (!known) return "retained";
        // Synthetic archives need not parent earlier synthetic commits. The authenticated
        // journal supplies their frozen original roots and tree/disposition sources instead.
        const heads = coveragePins.find(p => p.sha === pin.sha)?.coveredHeads ?? [pin.sha];
        if (!heads.length) return "retained";
        for (const head of heads) {
          if (!OWED_OID.test(head) || (authority === "archive" && await this.ancestry(barePath, head, coveringSha) !== "ancestor")) return "retained";
        }
      }
      const open = (): void => {
        assertResidueQuarantineOpen("git");
        if (!canDelete()) throw new Error("recovery cleanup source protection changed");
      };
      const deleteRef = async (ref: string, sha: string): Promise<void> => {
        open();
        await this.runGit(barePath, ["update-ref", "-d", ref, sha]);
        if (await this.checkedRefSha(barePath, ref) !== undefined) throw new Error("recovery cleanup ref deletion mismatch");
      };
      if (authority === "completion-check") { open(); return "verified"; }
      for (const pin of ownedPins) await deleteRef(pin.ref, pin.sha);
      for (const sha of new Set(exact.map(r => r.sha))) {
        const ref = `refs/uzi-owed/${context.runId}/${sha}`;
        const shared = metadata.records.some(r => r.runId === context.runId && r.sha === sha && !exact.includes(r));
        if (!shared && allRefs.some(p => p.ref === ref)) await deleteRef(ref, sha);
      }
      for (const record of exact) {
        open();
        const name = `candidate-${record.runId}-${record.sha}-${record.contextName.slice(8, -5)}.json`;
        await this.removeOwedFile(barePath, name, true);
      }
      // Only positively unconsumed exact-generation contexts can be garbage-collected.
      // Preserve receipt/marker identity, owner stamps, governed refs and sibling candidates.
      // Unknown config naming conservatively keeps contexts rather than retiring proof.
      for (const [name, c] of metadata.contexts) {
        if (c.runId !== context.runId || c.generation !== context.generation || "origin" in c) continue;
        const hash = this.receiptName(c.branch);
        const consumed = metadata.records.some(r => r.contextName === name && !exact.includes(r)) ||
          metadata.names.some(n => n === `receipt-${hash}.json` || n === `governed-${hash}.json`) ||
          allRefs.some(p => p.ref === runnerTrackingRef(c.branch)) ||
          config.split("\0").some(item => item.startsWith(`uzi-trackowner.${c.branch}.`) ||
            item.startsWith(legacyFlatTrackingOwnerKey(c.branch) + "\n"));
        if (consumed) continue;
        open();
        await this.removeOwedFile(barePath, name, true);
      }
      return "removed";
    });
  }

  /** Only a server-confirmed full commit and positive ancestry proof release an owed pin. */
  async reconcileOwedCandidates(barePath: string, runId: string, remotelyConfirmedSha: string):
    Promise<{ removedShas: string[]; retainedShas: string[] }> {
    if (typeof runId !== "string" || !OWED_RUN_ID.test(runId)) throw new Error("invalid owed run ID");
    this.validateConfirmedSha(remotelyConfirmedSha);
    await this.assertOwedBare(barePath);
    return this.withLock(barePath, async () => {
      const candidates = await this.enumerateOwedUnderLock(barePath, runId);
      const removedShas: string[] = [], retainedShas: string[] = [];
      for (const candidate of candidates) {
        if (await this.remotelyCovers(barePath, candidate.sha, remotelyConfirmedSha)) {
          await this.runGit(barePath, ["update-ref", "-d", candidate.pinRef, candidate.sha]);
          if (await this.checkedRefSha(barePath, candidate.pinRef) !== undefined) throw new Error("owed pin deletion mismatch");
          // Ref first, metadata second: a crash between them leaves harmless metadata, never a
          // pin without discovery metadata. Without this, candidate-*.json would outlive every
          // released pin and enumerateOwedUnderLock would read them all forever.
          for (const ctx of candidate.contexts) {
            const name = `candidate-${runId}-${candidate.sha}-${this.contextName(ctx).slice(8, -5)}.json`;
            if (await this.readOwedFile(barePath, name) !== undefined) await this.removeOwedFile(barePath, name);
          }
          removedShas.push(candidate.sha);
        } else retainedShas.push(candidate.sha);
      }
      return { removedShas, retainedShas };
    });
  }

  /** Recovery-only aggregate, never a tracking/checkpoint/publication ref.
   * Parent batches have at most 32 roots; every round reduces the count, with no retries.
   * A failed construction/proof stops the aggregate, retaining all original owed pins. */
  async buildRecoveryCoverage(barePath: string, context: PositiveOwedCandidateContext, originalShas: string[],
    currentSha: string): Promise<RecoveryCoverage> {
    context = this.snapshotOwedOptions({ context }).context as PositiveOwedCandidateContext;
    if (!Number.isSafeInteger(context.generation) || context.generation <= 0 ||
        "legacy" in context || "origin" in context) throw new Error("recovery coverage requires positive claim context");
    await this.validateOwedContext(barePath, context.branch, context);
    return this.withLock(barePath, async () => {
      const roots = [...new Set(originalShas)].sort();
      if (!roots.length) throw new Error("recovery coverage needs original roots");
      await this.requireOwedCommit(barePath, currentSha);
      for (const root of roots) await this.requireOwedCommit(barePath, root);
      const tree = (await this.runGit(barePath, ["--no-replace-objects", "rev-parse", `${currentSha}^{tree}`])).trim();
      if (!OWED_OID.test(tree) || (await this.runGit(barePath, ["--no-replace-objects", "cat-file", "-t", tree])).trim() !== "tree") {
        throw new Error("recovery current tree unavailable");
      }
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots, currentSha, tree })).digest("hex");
      let parents = [...new Set([...roots, currentSha])].sort();
      let round = 0;
      do {
        const next: string[] = [];
        for (let start = 0; start < parents.length; start += 32) {
          const args = ["--no-replace-objects", "-c", "commit.gpgsign=false", "commit-tree", tree];
          for (const parent of parents.slice(start, start + 32)) args.push("-p", parent);
          args.push("-m", `uzi recovery coverage ${fingerprint} round ${round} batch ${start / 32}`);
          const sha = (await this.runGitWithEnv(barePath, args, {
            GIT_AUTHOR_NAME: AGENT_GIT_IDENTITY.name, GIT_AUTHOR_EMAIL: AGENT_GIT_IDENTITY.email,
            GIT_COMMITTER_NAME: AGENT_GIT_IDENTITY.name, GIT_COMMITTER_EMAIL: AGENT_GIT_IDENTITY.email,
            GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z", GIT_COMMITTER_DATE: "2000-01-01T00:00:00Z",
          })).trim();
          await this.requireOwedCommit(barePath, sha);
          next.push(sha);
        }
        parents = next;
        round++;
      } while (parents.length > 1);
      const sha = parents[0]!;
      for (const root of [...roots, currentSha]) {
        if (await this.ancestry(barePath, root, sha) !== "ancestor") throw new Error("recovery coverage proof failed");
      }
      if ((await this.runGit(barePath, ["--no-replace-objects", "rev-parse", `${sha}^{tree}`])).trim() !== tree) {
        throw new Error("recovery coverage tree mismatch");
      }
      await this.persistOwedContext(context);
      const coverageRef = `refs/uzi-coverage/${context.runId}/${context.generation}/${fingerprint}`;
      await this.runGit(barePath, ["update-ref", coverageRef, sha]);
      if (await this.checkedRefSha(barePath, coverageRef) !== sha) throw new Error("recovery coverage pin mismatch");
      return { sha, coverageRef, fingerprint, originalShas: roots, currentSha };
    });
  }

  private async tryGitStdout(cwd: string | undefined, args: string[]): Promise<string> {
    try {
      const { stdout } = await this.execScoped("git", withDir(cwd, args), { env: gitEnv(), timeout: GIT_TIMEOUT_MS });
      return stdout.trim();
    } catch (err) {
      const abort = this.boundaryAbortError(err);
      if (abort) throw abort;
      return "";
    }
  }

  /** Serialize all mutations on a given bare repo (chained promises per path).
   * A permit-scoped caller waiting behind another run observes its hard or soft abort
   * promptly and forfeits its slot without running `fn`. The stored chain still
   * waits for the prior holder before it settles, so a cancelled waiter can never
   * let a later mutation overtake the holder and violate serialization. */
  private withLock<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.locks.get(key) ?? Promise.resolve();
    const boundary = this.boundaryProcesses.getStore();
    const recovery = this.recoveryOperations.getStore();
    const scope = boundary ? { ...boundary, signal: recovery ? AbortSignal.any([boundary.signal, recovery.signal]) : boundary.signal }
      : recovery;
    const softSignal = boundary?.softSignal;
    let started = false;
    let settled = false;
    let removeAbortListener = (): void => {};
    let resolveResult!: (value: T) => void;
    let rejectResult!: (error: unknown) => void;
    const result = new Promise<T>((resolve, reject) => {
      resolveResult = resolve;
      rejectResult = reject;
    });
    const abortBeforeAcquisition = (): void => {
      if (started || settled) return;
      settled = true;
      removeAbortListener();
      rejectResult(scope?.signal.aborted
        ? new GitBoundaryAbortError(GIT_LOCK_WAIT_ABORT_MESSAGE)
        : new CheckpointSoftDeadlineError());
    };
    if (scope) {
      removeAbortListener = (): void => {
        scope.signal.removeEventListener("abort", abortBeforeAcquisition);
        softSignal?.removeEventListener("abort", abortBeforeAcquisition);
      };
      if (scope.signal.aborted || softSignal?.aborted) abortBeforeAcquisition();
      else {
        scope.signal.addEventListener("abort", abortBeforeAcquisition, { once: true });
        softSignal?.addEventListener("abort", abortBeforeAcquisition, { once: true });
      }
    }
    const run = async (): Promise<void> => {
      if (settled) return;
      if (scope?.signal.aborted || softSignal?.aborted) {
        abortBeforeAcquisition();
        return;
      }
      started = true;
      removeAbortListener();
      let outcome: { ok: true; value: T } | { ok: false; error: unknown };
      try {
        outcome = { ok: true, value: await fn() };
      } catch (error) {
        outcome = { ok: false, error };
      }
      // issue #1597 M2: a scope's pre-release hook runs while this lock is STILL held (the chain
      // below does not advance until `run` returns), so a cancelled tick can settle its children and
      // reconcile lock files before any other bare mutation starts. It never fails the op.
      if (boundary?.beforeLockRelease) {
        await boundary.beforeLockRelease(key).catch(() => undefined);
      }
      settled = true;
      if (outcome.ok) resolveResult(outcome.value);
      else rejectResult(outcome.error);
    };
    const next = prev.then(run, run);
    // Keep the serialization chain alive but swallow its stored result so one
    // failed mutation does not poison every later op on the same repo.
    this.locks.set(key, next.catch(() => undefined));
    return result;
  }
}

// Trusted cancel reader: Linux pinned parents, regular-file descriptors and raw readlink
// bytes; no target traversal, Git, config, filters or writes. One failed path aborts
// the proof. At most 20,000 entries, depth 64, 4 MiB/entry,
// 128 MiB total; synchronous reads check a 28s deadline, execScoped caps at 30s.
const CANCEL_CONTENT_HELPER = String.raw`
const fs = require("node:fs");
const { createHash } = require("node:crypto");
const C = fs.constants, O_PATH = 0x200000;
const root = process.argv[1], deadline = Date.now() + 28000;
const fds = new Set();
let total = 0;
function check() { if (Date.now() > deadline) throw Error("content deadline"); }
function close(fd) { fs.closeSync(fd); fds.delete(fd); }
function fp(fd) { return "/proc/" + process.pid + "/fd/" + fd; }
function open(p, flags) {
  check();
  const fd = fs.openSync(p, flags);
  fds.add(fd);
  return fd;
}
function parts(p) {
  const a = p.split("/");
  if (!p || a.length > 64 || a.some(x => !x || x === "." || x === ".." || x.toLowerCase() === ".git")
      || p.includes("\0") || Buffer.from(p).toString() !== p) throw Error("unsafe path");
  return a;
}
try {
  if (process.platform !== "linux" || !root.startsWith("/") || root.length > 4096 ||
      root.split("/").length > 64) throw Error("unsupported root");
  // Pin every absolute ancestor without following a source-controlled symlink.
  let dir = open("/", O_PATH | C.O_NOFOLLOW);
  for (const part of root.slice(1).split("/")) {
    if (!part || part === "." || part === "..") throw Error("unsafe root");
    const next = open(fp(dir) + "/" + part, O_PATH | C.O_NOFOLLOW);
    if (!fs.fstatSync(next).isDirectory()) throw Error("non-directory root");
    close(dir); dir = next;
  }
  // Fixed metadata traversal is separate from parts(), which forbids manifest .git paths.
  // One failure aborts the proof; exclude reads stop at 64 KiB plus an overflow sentinel.
  let metadata = dir;
  try {
    for (const component of [".git", "info"]) {
      let next;
      try { next = open(fp(metadata) + "/" + component, O_PATH | C.O_NOFOLLOW); }
      catch (e) { if (component === "info" && e.code === "ENOENT") break; throw e; }
      if (!fs.fstatSync(next).isDirectory()) throw Error("unsafe exclude ancestor");
      // O_PATH pins identity but does not prove read authority.
      const readable = open(fp(next) + "/.", C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW);
      close(readable);
      if (metadata !== dir) close(metadata);
      metadata = next;
      if (component !== "info") continue;
      let fd;
      try { fd = open(fp(metadata) + "/exclude", C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK); }
      catch (e) { if (e.code === "ENOENT") break; throw e; }
      try {
        const before = fs.fstatSync(fd, { bigint: true });
        if (!before.isFile() || before.size > 65536n) throw Error("exclude type/cap");
        const bytes = Buffer.alloc(65537);
        let n = 0;
        while (n < bytes.length) {
          check();
          const got = fs.readSync(fd, bytes, n, bytes.length - n, null);
          if (!got) break;
          n += got;
          if (n > 65536) throw Error("exclude cap");
        }
        check();
        const after = fs.fstatSync(fd, { bigint: true });
        if (BigInt(n) !== before.size ||
            ["dev", "ino", "size", "mode", "mtimeNs", "ctimeNs"].some(k => before[k] !== after[k]))
          throw Error("unstable exclude");
        const content = bytes.subarray(0, n);
        if (!require("node:buffer").isUtf8(content) || content.includes(0))
          throw Error("exclude encoding");
        const lines = content.toString("utf8").split("\n");
        for (let i = 0; i < lines.length; i++) {
          check();
          const line = i < lines.length - 1 && lines[i].endsWith("\r")
            ? lines[i].slice(0, -1) : lines[i];
          if (line.includes("\r") ||
              !(line.startsWith("#") || /^ *$/.test(line) || line === "/.uzi/scratch/"))
            throw Error("untrusted exclude rule");
        }
      } finally { close(fd); }
    }
  } finally { if (metadata !== dir) close(metadata); }
  const input = fs.readFileSync(0);
  if (input.length > 2 * 1024 * 1024) throw Error("manifest cap");
  const manifest = JSON.parse(input.toString("utf8"));
  if (!Array.isArray(manifest) || manifest.length > 20000) throw Error("path cap");
  for (const [name, mode, oid, size] of manifest) {
    check();
    if (!["100644", "100755", "120000"].includes(mode) || !/^[0-9a-f]{40}$/.test(oid) ||
        !Number.isSafeInteger(size) || size < 0 || size > 4 * 1024 * 1024) throw Error("unsupported entry");
    total += size;
    if (total > 128 * 1024 * 1024) throw Error("aggregate cap");
    const components = parts(name);
    let parent = dir;
    try {
      for (const component of components.slice(0, -1)) {
        const next = open(fp(parent) + "/" + component, O_PATH | C.O_NOFOLLOW);
        if (!fs.fstatSync(next).isDirectory()) throw Error("non-directory ancestor");
        if (parent !== dir) close(parent);
        parent = next;
      }
      if (mode === "120000") {
        const leaf = fp(parent) + "/" + components.at(-1);
        const before = fs.lstatSync(leaf, { bigint: true });
        if (!before.isSymbolicLink() || before.size !== BigInt(size)) throw Error("link type/size");
        const target = fs.readlinkSync(leaf, { encoding: "buffer" });
        check();
        const after = fs.lstatSync(leaf, { bigint: true });
        if (target.length !== size ||
            createHash("sha1").update("blob " + size + "\0").update(target).digest("hex") !== oid ||
            ["dev", "ino", "size", "mode", "mtimeNs", "ctimeNs"].some(k => before[k] !== after[k]))
          throw Error("content mismatch or unstable link");
        continue;
      }
      const fd = open(fp(parent) + "/" + components.at(-1), C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
      try {
        const before = fs.fstatSync(fd, { bigint: true });
        if (!before.isFile() || before.size !== BigInt(size) ||
            ((before.mode & 0o100n) !== 0n) !== (mode === "100755")) throw Error("file type/size/mode");
        const hash = createHash("sha1").update("blob " + size + "\0");
        const buf = Buffer.alloc(65536);
        let n = 0;
        while (true) {
          check();
          const got = fs.readSync(fd, buf, 0, Math.min(buf.length, size + 1 - n), null);
          if (!got) break;
          n += got;
          if (n > size) throw Error("file grew");
          hash.update(buf.subarray(0, got));
        }
        const after = fs.fstatSync(fd, { bigint: true });
        if (n !== size || hash.digest("hex") !== oid ||
            ["dev", "ino", "size", "mode", "mtimeNs", "ctimeNs"].some(k => before[k] !== after[k]))
          throw Error("content mismatch or unstable file");
      } finally { close(fd); }
    } finally { if (parent !== dir) close(parent); }
  }
  process.stdout.write("clean\n");
} catch {
  process.exitCode = 1;
} finally {
  for (const fd of fds) fs.closeSync(fd);
}
`;

/** Default planning-capture budgets, passed to the helper as an argv JSON argument. */
const PLANNING_SNAPSHOT_LIMIT = 128 * 1024 * 1024;
const PLANNING_TOTAL_LIMIT = 128 * 1024 * 1024;

/**
 * Fixed refusal vocabulary of PLANNING_CAPTURE_HELPER. Each token is a literal
 * the helper emits as `UZI-PLANNING-REFUSAL <token>`; none can carry a file name.
 */
export const PLANNING_REFUSAL_TOKENS: Readonly<Record<string, PlanCrossCheckDiffRefusal>> = {
  untracked_path_cap: "too_many_untracked",
  patch_cap: "diff_too_large", source_cap: "diff_too_large", total_source_cap: "diff_too_large",
  base_source_cap: "diff_too_large", snapshot_cap: "diff_too_large",
  base_object_type_mismatch: "base_unavailable", base_object_integrity: "base_unavailable",
  invalid_base_commit_tree: "base_unavailable", "errno:ENOENT": "base_unavailable",
  unsupported_symlink: "unsupported_entry", unsupported_gitlink: "unsupported_entry",
  non_directory_ancestor: "unsupported_entry", nonregular_source: "unsupported_entry",
  unsupported_base_mode: "unsupported_entry", unsupported_tracked_mode: "unsupported_entry",
  "errno:ELOOP": "diff_failed", "errno:ENOTDIR": "diff_failed",
  capture_deadline: "diff_failed", unsafe_root: "diff_failed", non_directory_root: "diff_failed",
  unsafe_path: "diff_failed", source_changed: "diff_failed", git_pipe_cap: "diff_failed",
  git_deadline: "diff_failed", git_refused: "diff_failed", incomplete_path_metadata: "diff_failed",
  non_utf8_path: "diff_failed", non_utf8_object_filename: "diff_failed", unsafe_object_filename: "diff_failed",
  unsupported_index: "diff_failed", index_checksum: "diff_failed", index_path_cap: "diff_failed",
  short_index: "diff_failed", unsupported_index_flags: "diff_failed", short_index_path: "diff_failed",
  duplicate_index_path: "diff_failed", unsupported_index_extension: "diff_failed",
  short_index_extension: "diff_failed", object_alternates: "diff_failed",
  neutral_attributes_unavailable: "diff_failed", directory_cap: "diff_failed",
  metadata_entry_cap: "diff_failed", invalid_object_identity: "diff_failed", invalid_base_tree: "diff_failed",
  invalid_base_tree_name: "diff_failed", noncanonical_base_tree: "diff_failed", base_path_cap: "diff_failed",
  tracked_path_cap: "diff_failed", ignore_query_cap: "diff_failed", invalid_limits: "diff_failed",
  unclassified: "diff_failed",
};

const PLANNING_PROCESS_FAILED = "bounded runner process failed: ";

/**
 * Map a capturePlanningDiff failure message to a fixed refusal and diagnostic.
 * Only the exact helper protocol line is trusted: the text after the process
 * failure prefix up to the FIRST newline must be `UZI-PLANNING-REFUSAL <token>`
 * with a token from the fixed vocabulary (or a bare errno name). Later lines are
 * never read, so a file name carrying a forged marker cannot influence the
 * result, and no byte of the message reaches the returned diagnostic.
 */
export function classifyPlanningCaptureError(message: string): {
  refusal: PlanCrossCheckDiffRefusal; diagnostic: string;
} {
  if (message === "planning base must be immutable 40-hex") return { refusal: "base_unavailable", diagnostic: "base_invalid" };
  if (message === "bounded runner stdout exceeded 512 KiB") return { refusal: "diff_too_large", diagnostic: "output_cap" };
  if (message === "bounded runner stdout timed out") return { refusal: "diff_failed", diagnostic: "timeout" };
  if (message === "bounded runner stdout cleanup failed") return { refusal: "diff_failed", diagnostic: "cleanup_failed" };
  if (message.startsWith(PLANNING_PROCESS_FAILED)) {
    const rest = message.slice(PLANNING_PROCESS_FAILED.length);
    const line = rest.slice(0, rest.includes("\n") ? rest.indexOf("\n") : rest.length);
    const match = /^UZI-PLANNING-REFUSAL ([a-zA-Z0-9_:]+)$/.exec(line);
    const token = match?.[1];
    if (token !== undefined) {
      const known = Object.hasOwn(PLANNING_REFUSAL_TOKENS, token) ? PLANNING_REFUSAL_TOKENS[token] : undefined;
      if (known) return { refusal: known, diagnostic: token };
      if (/^errno:E[A-Z]+$/.test(token)) return { refusal: "diff_failed", diagnostic: token };
    }
    return { refusal: "diff_failed", diagnostic: "unclassified" };
  }
  return { refusal: "diff_failed", diagnostic: "unclassified" };
}

/**
 * Trusted inline runner program, never loaded from the clone. Linux descriptor
 * traversal is deliberately required. Unsupported indexes/objects fail closed.
 * Metadata: 2 MiB per Git pipe/index, 20,000 tracked paths, 30,000 entries,
 * 2,048 directories, depth 64. Patches: 512 KiB total and per diff pipe.
 * Two independent 128 MiB budgets (limits arrive as a JSON argv argument built
 * by GitCache, never from the clone): the object-store snapshot copy
 * (snapshotLimit, charged per byte read while streaming) and source reads
 * (totalLimit: ignore/index/worktree compare reads, symlink targets, and
 * authenticated commit/tree/blob pipe reads), each plus one overflow sentinel
 * byte. Snapshot bytes never charge the source budget. 4 MiB per worktree file
 * or blob; 200 nonignored untracked files. Object files (including packs) are
 * copied without the 4 MiB file cap, so copying a very large object store can
 * conservatively refuse a large history.
 * Tracked symlinks (mode 120000) are accepted only when unchanged: the link is
 * read with readlink through the still-open pinned parent descriptor and its
 * target hash must equal the base blob id; the target is never opened or
 * followed. Every other symlink case and every gitlink (160000) is refused.
 * Patch pipes likewise consume at most 512 KiB total plus one sentinel byte.
 * One error aborts the entire capture. Every refusal writes, as the very first
 * stderr bytes, one "UZI-PLANNING-REFUSAL <token>" line with a token from a
 * fixed vocabulary (see classifyPlanningCaptureError); free text may follow it.
 * Child commands have a 5s deadline; capture has a 30s whole-root deadline.
 */
const PLANNING_CAPTURE_HELPER = String.raw`
const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");
const { createHash } = require("node:crypto");
const [clone, base, git, limitsJson] = process.argv.slice(1);
const C = fs.constants;
// Linux O_PATH is not exported by every Node build; this helper already uses Linux proc-fd paths.
const O_PATH = 0x200000;
const PATCH_LIMIT = 512 * 1024, METADATA_LIMIT = 2 * 1024 * 1024;
const FILE_LIMIT = 4 * 1024 * 1024, LINK_LIMIT = 4096;
let SNAPSHOT_LIMIT = 0, TOTAL_LIMIT = 0;
const PATH_LIMIT = 20000, ENTRY_LIMIT = 30000, DIR_LIMIT = 2048, DEPTH_LIMIT = 64;
let total = 0, snapshotBytes = 0, entries = 0, dirs = 0, temp, child;
const fds = new Set();
const deadline = Date.now() + 28000;
function refusal(token) { const e = Error(token); e.refusal = token; return e; }
function refuse(token) { throw refusal(token); }
function check() { if (Date.now() > deadline) refuse("capture_deadline"); }
function close(fd) { fs.closeSync(fd); fds.delete(fd); }
function fdpath(fd) { return "/proc/" + process.pid + "/fd/" + fd; }
function directory(parent, name) {
  check();
  const fd = fs.openSync(fdpath(parent) + "/" + name, C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW);
  fds.add(fd); return fd;
}
function pathOnly(p) {
  check();
  const fd = fs.openSync(p, O_PATH | C.O_NOFOLLOW);
  fds.add(fd); return fd;
}
function absoluteDirectory(p) {
  let fd = pathOnly("/");
  for (const part of p.split("/").filter(Boolean)) {
    if (part === "." || part === "..") refuse("unsafe_root");
    const next = pathOnly(fdpath(fd) + "/" + part);
    if (!fs.fstatSync(next).isDirectory()) refuse("non_directory_root");
    close(fd); fd = next;
  }
  // Ancestors need lookup authority only. Acquire read authority at the pinned checkout.
  const root = directory(fd, "."); close(fd); return root;
}
function components(p) {
  const parts = p.split("/");
  if (!p || p.includes("\0") || parts.length > DEPTH_LIMIT + 1 || parts.some(x => !x || x === "." || x === ".." || x.toLowerCase() === ".git")
      || Buffer.from(p).toString() !== p) refuse("unsafe_path");
  return parts;
}
function openFile(root, p) {
  const parts = components(p); let parent = root, owned = false;
  try {
    for (const part of parts.slice(0, -1)) {
      const next = pathOnly(fdpath(parent) + "/" + part);
      const st = fs.fstatSync(next);
      if (!st.isDirectory()) {
        close(next);
        refuse("non_directory_ancestor");
      }
      if (owned) close(parent); parent = next; owned = true;
    }
    const fd = fs.openSync(fdpath(parent) + "/" + parts.at(-1), C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
    fds.add(fd);
    const st = fs.fstatSync(fd);
    if (!st.isFile()) {
      close(fd);
      refuse("nonregular_source");
    }
    return fd;
  } finally { if (owned) close(parent); }
}
// Candidate-loop leaf open that also recognizes a symlink leaf. The pinned
// parent descriptor stays open across the failed O_NOFOLLOW open and the
// readlink, so the link is read through the same directory the open saw. The
// target is only read, never opened or followed. Result: undefined (absent),
// { fd } (regular file) or { link } (raw target bytes, charged to the source
// budget).
function openLeafOrLink(root, p) {
  const parts = components(p); let parent = root, owned = false;
  try {
    for (const part of parts.slice(0, -1)) {
      const next = pathOnly(fdpath(parent) + "/" + part);
      const st = fs.fstatSync(next);
      if (!st.isDirectory()) {
        close(next);
        if (st.isFile()) return undefined;
        refuse("non_directory_ancestor");
      }
      if (owned) close(parent); parent = next; owned = true;
    }
    const leaf = fdpath(parent) + "/" + parts.at(-1);
    let fd;
    try { fd = fs.openSync(leaf, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK); }
    catch (e) {
      if (e.code === "ENOENT") return undefined;
      if (e.code !== "ELOOP") throw e;
      let link;
      try { link = fs.readlinkSync(leaf, { encoding: "buffer" }); }
      catch (r) { if (r.code === "EINVAL" || r.code === "ENOENT") refuse("source_changed"); throw r; }
      if (link.length > LINK_LIMIT) refuse("unsupported_symlink");
      total += link.length; if (total > TOTAL_LIMIT) refuse("total_source_cap");
      return { link };
    }
    fds.add(fd);
    const st = fs.fstatSync(fd);
    if (!st.isFile()) {
      close(fd);
      if (st.isDirectory()) return undefined;
      refuse("nonregular_source");
    }
    return { fd };
  } finally { if (owned) close(parent); }
}
function read(fd, cap) {
  const before = fs.fstatSync(fd, { bigint: true });
  if (before.size > BigInt(cap)) refuse("source_cap");
  const chunks = []; let n = 0;
  while (n <= cap) {
    check();
    const b = Buffer.alloc(Math.min(16384, cap + 1 - n, TOTAL_LIMIT + 1 - total));
    const got = fs.readSync(fd, b, 0, b.length, null);
    if (!got) break;
    n += got; if (n > cap) refuse("source_cap");
    total += got; if (total > TOTAL_LIMIT) refuse("total_source_cap");
    chunks.push(b.subarray(0, got));
  }
  const after = fs.fstatSync(fd, { bigint: true });
  if (before.size !== after.size || before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs)
    refuse("source_changed");
  return Buffer.concat(chunks, n);
}
function optional(root, p, cap) {
  let fd;
  try { fd = openFile(root, p); return read(fd, cap); }
  catch (e) { if (e.code === "ENOENT") return null; throw e; }
  finally { if (fd !== undefined) close(fd); }
}
function cleanup() {
  if (temp) fs.rmSync(temp, { recursive: true, force: true });
  for (const fd of fds) fs.closeSync(fd);
  fds.clear();
}
process.on("SIGTERM", () => {
  if (child) child.kill("SIGKILL");
  try { cleanup(); } finally { process.exit(143); }
});
const env = { ...process.env, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_NO_LAZY_FETCH: "1", GIT_ALLOW_PROTOCOL: "uzi-no-transport",
  GIT_ATTR_NOSYSTEM: "1", GIT_OPTIONAL_LOCKS: "0", GIT_EXTERNAL_DIFF: "",
  GIT_CEILING_DIRECTORIES: "/", GIT_CONFIG_COUNT: "0" };
function command(args, cap = METADATA_LIMIT, input, allowDiff = false, cwd = temp, extra = {}, source = false) {
  check();
  return new Promise((resolve, reject) => {
    const p = spawn(git, ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
      "-c", "core.attributesFile=/dev/null", "-c", "core.excludesFile=/dev/null",
      "-c", "core.quotePath=true", "-c", "diff.external=", ...args],
      { cwd, env: { ...env, ...extra }, stdio: ["pipe", "pipe", "pipe"] });
    child = p;
    let error, bytes = 0, errBytes = 0; const chunks = [];
    const fail = e => {
      error ||= e; p.kill("SIGKILL");
      // Overflow stops reads. Destroy both pipes so close/reap settles even
      // when unread buffered output remains after the child has exited.
      p.stdout.destroy(); p.stderr.destroy();
    };
    function collect(s, stderr) {
      s.on("readable", () => {
        try {
          let n = stderr ? errBytes : bytes;
          while (!error) {
            const b = s.read(Math.min(16384, Math.max(1, s.readableLength), cap + 1 - n,
              source && !stderr ? TOTAL_LIMIT + 1 - total : Infinity));
            if (b === null) break;
            n += b.length;
            if (source && !stderr) {
              total += b.length;
              if (total > TOTAL_LIMIT) { fail(refusal("total_source_cap")); break; }
            }
            if (stderr) errBytes = n; else bytes = n;
            if (n > cap) { fail(refusal("git_pipe_cap")); break; }
            if (!stderr) chunks.push(b);
          }
        } catch (e) { fail(e); }
      });
      s.on("error", fail);
    }
    collect(p.stdout, false); collect(p.stderr, true);
    const timer = setTimeout(() => fail(refusal("git_deadline")), Math.min(5000, deadline - Date.now()));
    p.on("error", fail);
    p.on("close", code => {
      clearTimeout(timer); child = undefined;
      if (error) reject(error);
      else if (code !== 0 && !(allowDiff && code === 1)) reject(refusal("git_refused"));
      else resolve(Buffer.concat(chunks, bytes));
    });
    p.stdin.on("error", fail); p.stdin.end(input);
  });
}
function paths(bytes) {
  if (bytes.length && bytes.at(-1) !== 0) refuse("incomplete_path_metadata");
  const text = bytes.toString("utf8");
  if (!Buffer.from(text).equals(bytes)) refuse("non_utf8_path");
  return bytes.length ? text.slice(0, -1).split("\0") : [];
}
function indexEntries(bytes) {
  const result = new Map();
  if (!bytes) return result;
  if (bytes.length < 32 || bytes.toString("ascii", 0, 4) !== "DIRC"
      || ![2, 3].includes(bytes.readUInt32BE(4))) refuse("unsupported_index");
  const checksum = createHash("sha1").update(bytes.subarray(0, -20)).digest();
  if (!checksum.equals(bytes.subarray(-20))) refuse("index_checksum");
  const count = bytes.readUInt32BE(8);
  if (count > PATH_LIMIT) refuse("index_path_cap");
  let offset = 12;
  for (let i = 0; i < count; i++) {
    const start = offset;
    if (offset + 62 > bytes.length - 20) refuse("short_index");
    const flags = bytes.readUInt16BE(offset + 60);
    if (flags & 0x7000) refuse("unsupported_index_flags");
    const end = bytes.indexOf(0, offset + 62);
    if (end < 0 || end >= bytes.length - 20) refuse("short_index_path");
    const raw = bytes.subarray(offset + 62, end), name = raw.toString("utf8");
    if (!Buffer.from(name).equals(raw)) refuse("non_utf8_path");
    components(name);
    const mode = bytes.readUInt32BE(start + 24);
    if (mode === 0o160000) refuse("unsupported_gitlink");
    if (mode !== 0o100644 && mode !== 0o100755 && mode !== 0o120000) refuse("unsupported_tracked_mode");
    if (result.has(name)) refuse("duplicate_index_path");
    result.set(name, { mode });
    offset = start + Math.ceil((end + 1 - start) / 8) * 8;
  }
  // Optional extensions are skipped; required extensions (e.g. split index) refuse.
  while (offset < bytes.length - 20) {
    if (offset + 8 > bytes.length - 20 || bytes[offset] < 65 || bytes[offset] > 90)
      refuse("unsupported_index_extension");
    offset += 8 + bytes.readUInt32BE(offset + 4);
  }
  if (offset !== bytes.length - 20) refuse("short_index_extension");
  return result;
}
(async () => {
  const limits = JSON.parse(limitsJson ?? "null");
  const keys = limits && typeof limits === "object" && !Array.isArray(limits) ? Object.keys(limits).sort() : [];
  if (keys.join() !== "snapshotLimit,totalLimit" || keys.some(k => !Number.isSafeInteger(limits[k]) || limits[k] <= 0))
    refuse("invalid_limits");
  SNAPSHOT_LIMIT = limits.snapshotLimit; TOTAL_LIMIT = limits.totalLimit;
  const root = absoluteDirectory(clone);
  const gd = directory(root, ".git"), objects = directory(gd, "objects");
  const info = directory(objects, "info");
  // Alternates could redirect base reads outside this clone. Missing objects refuse.
  if (optional(info, "alternates", 65536) !== null) refuse("object_alternates");
  temp = fs.mkdtempSync(path.join(process.env.TMPDIR || "/tmp", "uzi-planning-"));
  fs.chmodSync(temp, 0o700);
  await command(["init", "--quiet", "--template=", temp]);
  // info/attributes has precedence over every captured .gitattributes, including itself.
  fs.mkdirSync(path.join(temp, ".git/info"), { recursive: true });
  fs.writeFileSync(path.join(temp, ".git/info/attributes"), "* diff -filter -text -ident -working-tree-encoding\n");
  const attrNames = ["a/.gitattributes", "b/.gitattributes"];
  const attributes = await command(["check-attr", "-z", "diff", "filter", "--", ...attrNames]);
  const expectedAttributes = Buffer.from(attrNames.map(name => name + "\0diff\0set\0" + name + "\0filter\0unset\0").join(""));
  if (!attributes.equals(expectedAttributes)) refuse("neutral_attributes_unavailable");
  // A directory descriptor pins traversal only: Git can follow links inside it.
  // Git receives only runner-owned copies, never paths into the mutable clone.
  const objectStore = path.join(temp, "object-store");
  fs.mkdirSync(objectStore);
  // Yield to the event loop (so a pending SIGTERM runs cleanup) when more than
  // ~20 ms of synchronous work has passed since the last yield. The clock is shared
  // across chunks, files and directory entries, so many tiny files still yield.
  let lastYield = Date.now();
  async function maybeYield() {
    if (Date.now() - lastYield < 20) return;
    await new Promise(resolve => setImmediate(resolve));
    lastYield = Date.now();
  }
  async function copySnapshotFile(src, target) {
    const before = fs.fstatSync(src, { bigint: true });
    if (before.size > BigInt(SNAPSHOT_LIMIT - snapshotBytes)) refuse("snapshot_cap");
    const out = fs.openSync(target, C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW, 0o600);
    try {
      const buffer = Buffer.alloc(16384);
      for (;;) {
        check();
        const got = fs.readSync(src, buffer, 0, Math.min(buffer.length, SNAPSHOT_LIMIT + 1 - snapshotBytes), null);
        if (!got) break;
        snapshotBytes += got;
        if (snapshotBytes > SNAPSHOT_LIMIT) refuse("snapshot_cap");
        for (let at = 0; at < got;) at += fs.writeSync(out, buffer, at, got - at);
        await maybeYield();
      }
    } finally { fs.closeSync(out); }
    const after = fs.fstatSync(src, { bigint: true });
    if (before.size !== after.size || before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs)
      refuse("source_changed");
  }
  async function snapshot(fd, dst, depth) {
    if (++dirs > DIR_LIMIT || depth > DEPTH_LIMIT) refuse("directory_cap");
    const stream = fs.opendirSync(fdpath(fd), { encoding: "buffer" });
    try {
      let ent;
      while ((ent = stream.readSync())) {
        check(); if (++entries > ENTRY_LIMIT) refuse("metadata_entry_cap");
        await maybeYield();
        const raw = ent.name, name = raw.toString("utf8");
        if (!Buffer.from(name).equals(raw)) refuse("non_utf8_object_filename");
        if (components(name).length !== 1) refuse("unsafe_object_filename");
        const target = path.join(dst, name);
        if (ent.isDirectory()) {
          const sub = directory(fd, name);
          try {
            fs.mkdirSync(target);
            await snapshot(sub, target, depth + 1);
          } finally { close(sub); }
        } else {
          const src = openFile(fd, name);
          try { await copySnapshotFile(src, target); }
          finally { close(src); }
        }
      }
    } finally { stream.closeSync(); }
  }
  await snapshot(objects, objectStore, 0);
  // Check the owned snapshot as well: alternates could have appeared during copying.
  for (const name of ["alternates", "http-alternates"]) {
    if (fs.existsSync(path.join(objectStore, "info", name))) refuse("object_alternates");
  }
  const objectEnv = { GIT_OBJECT_DIRECTORY: objectStore };
  async function object(type, oid, cap) {
    if (!/^[0-9a-f]{40}$/.test(oid)) refuse("invalid_object_identity");
    const description = (await command(["cat-file", "--batch-check=%(objecttype) %(objectsize)"],
      METADATA_LIMIT, oid + "\n", false, temp, objectEnv)).toString();
    const match = /^(commit|tree|blob) (0|[1-9][0-9]*)\n$/.exec(description);
    if (!match || match[1] !== type) refuse("base_object_type_mismatch");
    const size = Number(match[2]);
    if (!Number.isSafeInteger(size) || size > cap || total + size > TOTAL_LIMIT)
      refuse("base_source_cap");
    const bytes = await command(["cat-file", type, oid], size, undefined, false, temp, objectEnv, true);
    if (bytes.length !== size || createHash("sha1").update(type + " " + bytes.length + "\0")
        .update(bytes).digest("hex") !== oid) refuse("base_object_integrity");
    return bytes;
  }
  const commit = await object("commit", base, METADATA_LIMIT);
  const treeHeader = /^tree ([0-9a-f]{40})\n/.exec(commit.subarray(0, 46).toString("ascii"));
  if (!treeHeader || !commit.subarray(0, 46).equals(Buffer.from(treeHeader[0]))) refuse("invalid_base_commit_tree");
  const old = new Map();
  let treeBytes = 0;
  async function tree(oid, prefix, depth) {
    if (++dirs > DIR_LIMIT || depth > DEPTH_LIMIT) refuse("directory_cap");
    const bytes = await object("tree", oid, METADATA_LIMIT - treeBytes);
    treeBytes += bytes.length;
    let at = 0, prior = null;
    const names = new Set();
    while (at < bytes.length) {
      check(); if (++entries > ENTRY_LIMIT) refuse("metadata_entry_cap");
      const space = bytes.indexOf(32, at), end = bytes.indexOf(0, at);
      if (space < at || end <= space || end + 21 > bytes.length) refuse("invalid_base_tree");
      const mode = bytes.toString("ascii", at, space);
      if (mode === "160000") refuse("unsupported_gitlink");
      if (!["40000", "100644", "100755", "120000"].includes(mode)
          || !bytes.subarray(at, space).equals(Buffer.from(mode))) refuse("unsupported_base_mode");
      const raw = bytes.subarray(space + 1, end), name = raw.toString("utf8");
      if (!Buffer.from(name).equals(raw)) refuse("non_utf8_path");
      if (components(name).length !== 1 || names.has(name)) refuse("invalid_base_tree_name");
      names.add(name);
      const key = Buffer.concat([raw, Buffer.from(mode === "40000" ? "/" : "\0")]);
      if (prior && Buffer.compare(prior, key) >= 0) refuse("noncanonical_base_tree");
      prior = key;
      const childOid = bytes.subarray(end + 1, end + 21).toString("hex");
      at = end + 21;
      const rel = prefix + name; components(rel);
      if (mode === "40000") await tree(childOid, rel + "/", depth + 1);
      else {
        old.set(rel, { mode: parseInt(mode, 8), oid: childOid });
        if (old.size > PATH_LIMIT) refuse("base_path_cap");
      }
    }
  }
  await tree(treeHeader[1], "", 0);

  const ixfd = (() => { try { return openFile(gd, "index"); } catch (e) { if (e.code === "ENOENT") return null; throw e; } })();
  let ix = null;
  if (ixfd !== null) {
    try { ix = read(ixfd, METADATA_LIMIT); } finally { close(ixfd); }
  }
  const indexed = indexEntries(ix);
  const tracked = new Set([...old.keys(), ...indexed.keys()]);
  if (tracked.size > PATH_LIMIT) refuse("tracked_path_cap");
  const trackedParents = new Set();
  for (const name of tracked) {
    let end = name.lastIndexOf("/");
    while (end !== -1) {
      trackedParents.add(name.slice(0, end));
      end = name.lastIndexOf("/", end - 1);
    }
  }
  const candidates = new Set(tracked), untracked = [];
  const exclude = optional(gd, "info/exclude", 65536);
  if (exclude) {
    fs.mkdirSync(path.join(temp, ".git/info"), { recursive: true });
    fs.writeFileSync(path.join(temp, ".git/info/exclude"), exclude);
  }
  async function walk(fd, prefix, depth) {
    if (++dirs > DIR_LIMIT || depth > DEPTH_LIMIT) refuse("directory_cap");
    // A symlinked .gitignore is ignored by Git (>= 2.32); the path itself is still a candidate.
    let ignore = null;
    try { ignore = optional(fd, ".gitignore", 65536); }
    catch (e) { if (e.code !== "ELOOP") throw e; }
    if (ignore) {
      const dst = path.join(temp, prefix, ".gitignore");
      fs.mkdirSync(path.dirname(dst), { recursive: true }); fs.writeFileSync(dst, ignore);
    }
    const directoryStream = fs.opendirSync(fdpath(fd), { encoding: "buffer" });
    const names = [];
    try {
      let ent;
      while ((ent = directoryStream.readSync())) {
        check(); if (++entries > ENTRY_LIMIT) refuse("metadata_entry_cap");
        const raw = ent.name, name = raw.toString("utf8");
        if (!Buffer.from(name).equals(raw)) refuse("non_utf8_path");
        if (name.toLowerCase() === ".git") continue;
        const rel = prefix + name; components(rel);
        names.push({ name, rel, dir: ent.isDirectory() });
      }
    } finally { directoryStream.closeSync(); }
    const queries = names.map(x => x.rel + (x.dir ? "/" : ""));
    const queryBytes = Buffer.from(queries.join("\0") + (queries.length ? "\0" : ""));
    if (queryBytes.length > METADATA_LIMIT) refuse("ignore_query_cap");
    const ignored = new Set(paths(await command(["check-ignore", "--no-index", "-z", "--stdin"],
      METADATA_LIMIT, queryBytes, true)));
    for (const item of names) {
      check();
      const needed = tracked.has(item.rel) || trackedParents.has(item.rel);
      if (ignored.has(item.rel + (item.dir ? "/" : "")) && !needed) continue;
      if (item.dir) {
        const sub = directory(fd, item.name);
        try { await walk(sub, item.rel + "/", depth + 1); } finally { close(sub); }
      } else if (!tracked.has(item.rel)) {
        untracked.push(item.rel);
        if (untracked.length > 200) refuse("untracked_path_cap");
      }
    }
  }
  await walk(root, "", 0);
  for (const name of untracked) candidates.add(name);
  let patchBytes = 0;
  for (const name of candidates) {
    check();
    let fd, content = null, mode = 0o100644;
    const previous = old.get(name), staged = indexed.get(name);
    try {
      let leaf;
      try { leaf = openLeafOrLink(root, name); } catch (e) { if (e.code !== "ENOENT") throw e; }
      if (leaf?.link) {
        // Only an unchanged tracked symlink is supported; the target is never opened.
        const unchanged = previous?.mode === 0o120000 && (!staged || staged.mode === 0o120000)
          && createHash("sha1").update("blob " + leaf.link.length + "\0").update(leaf.link).digest("hex") === previous.oid;
        if (!unchanged) refuse("unsupported_symlink");
        continue;
      }
      // A base or index symlink whose worktree path is no longer that symlink.
      if (previous?.mode === 0o120000 || staged?.mode === 0o120000) refuse("unsupported_symlink");
      fd = leaf?.fd;
      if (fd !== undefined) {
        const st = fs.fstatSync(fd, { bigint: true });
        mode = st.mode & 0o111n ? 0o100755 : 0o100644;
        // The mutable index identifies paths only; current bytes must always be read.
        content = read(fd, FILE_LIMIT);
      }
    } finally { if (fd !== undefined) close(fd); }
    if (!previous && content === null) continue;
    if (previous && content !== null && previous.mode === mode
        && createHash("sha1").update("blob " + content.length + "\0").update(content).digest("hex") === previous.oid) continue;
    let before = null;
    if (previous) {
      before = await object("blob", previous.oid, FILE_LIMIT);
    }
    // Git's binary sample convention, before creating any diff inputs.
    if ((before && before.subarray(0, 8000).includes(0)) || (content && content.subarray(0, 8000).includes(0))) continue;
    for (const [side, bytes, fileMode] of [["a", before, previous?.mode], ["b", content, mode]]) {
      if (bytes === null) continue;
      const dst = path.join(temp, side, name);
      fs.mkdirSync(path.dirname(dst), { recursive: true });
      fs.writeFileSync(dst, bytes, { mode: fileMode & 0o777 });
    }
    // Captured attributes are ordinary diff inputs; neutral info/attributes overrides them.
    const patch = await command(["diff", "--no-index", "--no-ext-diff", "--no-textconv",
      "--no-renames", "--src-prefix=", "--dst-prefix=", "--",
      before === null ? "/dev/null" : "a/" + name, content === null ? "/dev/null" : "b/" + name],
      PATCH_LIMIT - patchBytes, undefined, true);
    patchBytes += patch.length;
    if (patchBytes > PATCH_LIMIT) refuse("patch_cap");
    if (!process.stdout.write(patch)) await new Promise(resolve => process.stdout.once("drain", resolve));
    for (const side of ["a", "b"]) fs.rmSync(path.join(temp, side), { recursive: true, force: true });
  }
})().then(() => { cleanup(); }, (error) => {
  let token = "unclassified";
  if (typeof error?.refusal === "string") token = error.refusal;
  else if (typeof error?.code === "string" && /^E[A-Z]+$/.test(error.code)) token = "errno:" + error.code;
  try { cleanup(); } finally {
    // The token line must be the first stderr bytes; the stack may follow it.
    process.stderr.write("UZI-PLANNING-REFUSAL " + token + "\n" + String(error?.stack));
    process.exitCode = 1;
  }
});
`;

function withDir(cwd: string | undefined, args: string[]): string[] {
  return cwd ? ["-C", cwd, ...args] : args;
}

function commandCwd(args: readonly string[]): string {
  const at = args.indexOf("-C");
  const cwd = at >= 0 ? args[at + 1] : undefined;
  if (!cwd || !path.isAbsolute(cwd)) {
    throw new Error("permit-held runner git requires an absolute -C worktree");
  }
  return cwd;
}

/**
 * Paused byte-mode consumption: read at most 512 KiB plus one overflow sentinel
 * per pipe. No flowing collector or truncation of an already-read large chunk.
 */
function collectBoundedRunnerPipe(stream: Readable | null, signal: AbortSignal): {
  result: Promise<Buffer>;
  dispose: () => void;
} {
  const cap = 512 * 1024;
  let dispose = (): void => {};
  const result = new Promise<Buffer>((resolve, reject) => {
    if (!stream) { reject(new Error("bounded runner process has no output pipe")); return; }
    let bytes = 0;
    let settled = false;
    const chunks: Buffer[] = [];
    const fail = (error: unknown): void => {
      if (settled) return;
      settled = true;
      reject(error);
    };
    const drain = (): void => {
      if (settled) return;
      try {
        while (!settled) {
          const size = Math.min(16 * 1024, Math.max(1, stream.readableLength), cap + 1 - bytes);
          const chunk: unknown = stream.read(size);
          if (chunk === null) break;
          if (!Buffer.isBuffer(chunk) || chunk.length > size) {
            throw new Error("bounded runner output is not a byte pipe");
          }
          bytes += chunk.length;
          if (bytes > cap) throw new Error("bounded runner stdout exceeded 512 KiB");
          chunks.push(chunk);
        }
      } catch (error) { fail(error); }
    };
    const end = (): void => {
      if (settled) return;
      drain();
      if (settled) return;
      settled = true;
      resolve(Buffer.concat(chunks, bytes));
    };
    const close = (): void => {
      if (!stream.readableEnded) fail(new Error("bounded runner output closed before end"));
      else end();
    };
    const abort = (): void => fail(signal.reason);
    stream.pause();
    stream.on("readable", drain);
    stream.on("end", end);
    stream.on("close", close);
    // Keep the error listener until whole-root cleanup, even after overflow.
    stream.on("error", fail);
    signal.addEventListener("abort", abort, { once: true });
    dispose = (): void => {
      stream.removeListener("readable", drain);
      stream.removeListener("end", end);
      stream.removeListener("close", close);
      stream.removeListener("error", fail);
      signal.removeEventListener("abort", abort);
    };
    if (stream.readableDidRead || stream.errored || stream.readableObjectMode || stream.readableEncoding) {
      fail(new Error("bounded runner output was consumed or is not a fresh byte pipe"));
    } else if (stream.readableEnded) end();
    else if (stream.destroyed) close();
    else drain();
    if (signal.aborted) abort();
  });
  return { result, dispose: () => dispose() };
}

export function resolveBoundaryExecutable(command: string): string {
  if (command === "git") return GIT_BIN;
  if (command === "gitleaks") return GITLEAKS_BIN;
  if (path.isAbsolute(command)) return command;
  throw new Error("permit-held subprocess executable is not a trusted absolute path");
}

/** The git author identity planted on every runner clone (issue #234) and used by the
 *  M2 stub executor's own commit, kept in one place so the two paths cannot drift. */
export const AGENT_GIT_IDENTITY = { name: "uzi-agent", email: "uzi-agent@uzi.local" } as const;

/**
 * PRD #759 M1 — subject prefix of the throwaway "work-in-progress" commit that
 * `commitWipMarker` plants on the park path so uncommitted work survives the reseed.
 * Kept in one place because the recognition side reads it too: M2 detects this prefix
 * on the adopted tip to `git reset --soft <parent>` the content back to uncommitted (so
 * the marker never enters the history the agent builds on, never reaches finalize, and
 * never lands in the MR), and M5 uses it to distinguish a recovered WIP snapshot from a
 * recovered committed milestone. Do NOT inline the literal string anywhere else.
 *
 * @public — the recognition side (M2 reset-at-adopt, M5 feed event) lands in later
 * milestones, so there is no cross-file static consumer YET; exported here in M1 so
 * those milestones import the one definition rather than re-inlining the literal
 * (issue #597 convention for a deliberately-exported symbol knip cannot yet see a use
 * for, mirroring protocol.ts's @public wire DTOs).
 */
export const WIP_PARK_COMMIT_PREFIX = "wip(park):" as const;

/**
 * PRD #1062 M2 (#1036) — the commit-subject prefix that marks a `.github/workflows` overlay
 * transport wrapper on `refs/uzi-checkpoints/<branch>`. `checkpointPack` prefixes the synthetic
 * wrapper's subject with it; adoption (`runnerCloneForBranch` via `isOverlayMarker`) recognises
 * it and peels the wrapper (discard its swapped `.github` tree, re-point to its last parent =
 * realTip) BEFORE the wip-park soft-reset. Exported so the agent Contract-B tests reference the
 * one definition rather than re-inlining the literal (knip's zero-unused-export gate).
 */
export const OVERLAY_COMMIT_PREFIX = "ckpt(overlay):" as const;

/**
 * git subprocess env. safe.directory=* trusts the daemon-managed dirs
 * (ownership check adds nothing here and breaks when the container UID differs
 * from the volume owner); GIT_TERMINAL_PROMPT=0 turns auth failures into clear
 * errors instead of a hang.
 *
 * When a PAT is supplied it is injected as an http.extraHeader via env-scoped
 * config (GIT_CONFIG_KEY/VALUE), NOT via `git -c`. This is deliberate and
 * load-bearing: `git -c value` lands on git's argv, where the PAT is readable in
 * the container's process table (`ps`, /proc/<pid>/cmdline) during every network
 * op — and in M3 an agent subprocess may be alive during the worker's push. The
 * env path keeps it off argv (env is 0600 per /proc/<pid>/environ), off on-disk
 * config, and out of logs (runGit logs args only). Exported for the secret-flow
 * test. Supported since git 2.31.
 *
 * The header is HTTP **Basic** auth — `Authorization: Basic base64(user:pat)` —
 * NOT `PRIVATE-TOKEN`. GitLab honors PRIVATE-TOKEN only on its REST API; git-over-
 * HTTPS (clone/fetch/push) speaks HTTP Basic, so a PRIVATE-TOKEN header carries no
 * credential and git falls back to a (disabled) terminal prompt and fails. The PAT
 * is the Basic *password*; the username is the bot login when known, else the
 * conventional `oauth2` (GitLab accepts any non-empty username with a PAT).
 *
 * `httpScope` (M4 audit item 9) host-scopes the header so the credential is only
 * sent to the repo's own host: `http.<scope>.extraHeader` where <scope> is e.g.
 * `https://gitlab.example.com/`. Without scoping (`http.extraHeader`), a cross-
 * host redirect would replay it to the redirect target. `followRedirects` is also
 * pinned off so no redirect can carry the credential elsewhere. A hostless URL
 * (local fixture path / scp form) has no scope, so the header falls back to
 * unscoped — harmless because local/file transport ignores http.* config entirely.
 */
/**
 * issue #1783 (R4, auditor M1) — runner-clone git subcommands that may carry the worker spawn
 * mark: they start no filter, diff, merge or signing driver, and (with the pins below) no
 * transport, so no program the clone configures. They are NOT code-free on their own: a missing
 * object would make any of them LAZY-FETCH through a `extensions.partialClone` promisor remote
 * the agent-writable config plants (its `uploadpack` is a program), and a planted program would
 * inherit the nonce. That path is closed by two pins, not by the subcommand choice:
 * GIT_NO_LAZY_FETCH=1 in {@link gitEnv} (every worker git, marked or not), and
 * GIT_ALLOW_PROTOCOL={@link RUNNER_GIT_NO_PROTOCOL} on this marked subset (runGitAsRunner; the
 * seed's local clone exempt), which no `protocol.*` key in the clone's config can override.
 * test/git-no-lazy-fetch.test.ts runs the plant against each pin alone.
 * The runner clone's `.git/config` and `.gitattributes` are agent-writable, and the
 * arbitrary-name driver keys (`filter.<name>.*`,
 * `diff.<name>.*`, `merge.<name>.driver`) and `gpg.program` cannot be pinned off (see the
 * GIT_CODE_EXEC_KEY_PINS note above), so a git that touches the working tree, content-diffs,
 * merges or shows signatures can start an agent-planted program. That program inherits the git's
 * env: were the git worker-marked, the plant would carry the nonce and be exempt from every reap
 * forever. So only these pure ref/object/config/index reads and ref writes are marked; the
 * seed's clone is marked only with `--no-checkout` (no working tree is written). Anything else
 * (status, add, commit, checkout, reset, rm, clean, merge, rebase, cherry-pick, diff, log, …) runs
 * UNMARKED, and every path that runs one after a quiescence proof (the park, shutdown and pause
 * wip markers, the restore-point, hold and settle-transfer captures, finalize's base-align) re-proves
 * before its next credentialed step (runner.ts). Leading `-c key=value` pairs are skipped to find the subcommand.
 */
const RUNNER_GIT_MARKED_SUBCOMMANDS: ReadonlySet<string> = new Set([
  "rev-parse",
  "rev-list",
  "update-ref",
  "config",
  "ls-files",
  "ls-tree",
  "check-ignore",
]);

/** A GIT_ALLOW_PROTOCOL value naming no real protocol: git then allows NO transport. */
const RUNNER_GIT_NO_PROTOCOL = "none";

/** The argv index of a git subcommand: the first index past any leading `-c key=value` pairs
 *  (which may be `args.length`, i.e. past the end, when nothing follows them). */
function firstRunnerGitSubcommandIndex(args: readonly string[]): number {
  let i = 0;
  while (args[i] === "-c" && i + 1 < args.length) i += 2;
  return i;
}

/** The subcommand of a git argv (leading `-c key=value` pairs skipped), or undefined. */
function firstRunnerGitSubcommand(args: readonly string[]): string | undefined {
  return args[firstRunnerGitSubcommandIndex(args)];
}

export function runnerGitCarriesWorkerMark(args: readonly string[]): boolean {
  const i = firstRunnerGitSubcommandIndex(args);
  const sub = args[i];
  if (sub === undefined) return false;
  if (sub === "clone") return args.slice(i + 1).includes("--no-checkout");
  return RUNNER_GIT_MARKED_SUBCOMMANDS.has(sub);
}

/**
 * issue #1783 (R4): the env for a runner-uid git spawned OUTSIDE GitCache, built exactly as
 * runGitAsRunner builds its own: worker-marked only for the driver-free subset
 * (runnerGitCarriesWorkerMark), and for that subset GIT_ALLOW_PROTOCOL pins every transport
 * off, independently of gitEnv's GIT_NO_LAZY_FETCH. `args` is the git argv WITHOUT a leading
 * `-C <dir>` (the subcommand parser skips `-c key=value` pairs only).
 */
export function runnerGitSpawnEnv(args: readonly string[], runnerEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const marked = runnerGitCarriesWorkerMark(args);
  const env: NodeJS.ProcessEnv = marked ? workerSpawnEnv(runnerEnv) : unmarkedSpawnEnv(runnerEnv);
  if (marked && firstRunnerGitSubcommand(args) !== "clone") env.GIT_ALLOW_PROTOCOL = RUNNER_GIT_NO_PROTOCOL;
  return env;
}


export function gitEnv(pat?: string, httpScope?: string, username?: string): NodeJS.ProcessEnv {
  // REPLACEMENT env (M10 audit), NOT a process.env spread. A git subprocess can spawn
  // agent-controlled code (a hook at the default path) as the worker uid, outside the
  // SDK hook system — and gitEnv also injects the forge PAT (GIT_CONFIG_VALUE_n). So
  // the join token (UZI_WORKER_TOKEN[_FILE]) + API URL must be ABSENT BY CONSTRUCTION,
  // or that hook could read them (join token → claim → PAT + Anthropic token). Only
  // what git + git-over-HTTPS demonstrably need is carried, all non-secret:
  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH,
    HOME: process.env.HOME,
    GIT_TERMINAL_PROMPT: "0",
    // Issue #284: pin git's locale to C so push stderr is language-stable for the
    // transient-vs-permanent classifier (forge-retry.ts) — otherwise a localized
    // "Connection reset"/"[rejected]" would slip past the pattern match.
    LANG: "C",
    LC_ALL: "C",
    // Neutralize /etc/gitconfig: a system config source is another place a code-exec
    // key could be planted, and it is outside the inline-pin override guarantee for
    // any key we don't pin. The worker needs nothing from it (PRD #51 M0).
    GIT_CONFIG_NOSYSTEM: "1",
    // issue #1783 (auditor, round 3): never LAZY-FETCH a missing object. A runner clone's
    // agent-writable `.git/config` can declare `extensions.partialClone=<remote>` with that
    // remote's `uploadpack=<program>`; any git that then needs a missing object (even a
    // `rev-parse --verify <sha>^{commit}`) spawns the planted program to fetch it. No worker git
    // needs lazy fetch: the bare is a full clone and every runner clone a `clone --shared` of it,
    // so every object a legitimate op reads is local. With this pin a missing object is an error.
    GIT_NO_LAZY_FETCH: "1",
  };
  // PRD #51 M3 / 5-bis: keep git's scratch (packs, lockfiles) on the worker's private
  // 0700 TMPDIR (set by the entrypoint) rather than a shared sticky /tmp. Carry it only
  // if the image set it; git falls back to /tmp otherwise (a host/test without the
  // entrypoint). The M4 runner-spawned git ops get the RUNNER's TMPDIR via the spawn.
  if (process.env.TMPDIR) env.TMPDIR = process.env.TMPDIR;
  // The "global" config PATH. In the e2e overlay this points at an insteadOf-rewrite
  // file (a config-file path, not a secret) — pass it through untouched. In production
  // it is unset, and we must NOT let git fall back to $HOME/.gitconfig or
  // $XDG_CONFIG_HOME/git/config (either could carry a planted code-exec key outside the
  // pin set), so default it to /dev/null — an empty config that replaces both global
  // lookups. The inline GIT_CONFIG pairs below still OVERRIDE whatever a passed-through
  // file contains (higher precedence than any config file). (PRD #51 M0.)
  env.GIT_CONFIG_GLOBAL = process.env.GIT_CONFIG_GLOBAL || "/dev/null";
  // TLS trust for git-over-HTTPS pushes to the real forge. Carry only if the image set
  // them; never invent, never carry a secret.
  for (const k of ["NIX_SSL_CERT_FILE", "SSL_CERT_FILE", "GIT_SSL_CAINFO"] as const) {
    if (process.env[k]) env[k] = process.env[k];
  }

  // core.hooksPath → the empty dir on EVERY invocation: structurally neutralizes a
  // planted hook regardless of what the agent wrote. safe.directory=* as before.
  // The code-exec key pins (fsmonitor/diff.external/pager/sshCommand) are added
  // UNCONDITIONALLY — the no-PAT paths (changedFiles' `git diff`, `worktree add`)
  // must be covered too, so they cannot sit inside the `if (pat)` block below.
  const pairs: Array<[string, string]> = [
    ["safe.directory", "*"],
    ["core.hooksPath", EMPTY_GIT_HOOKS_DIR],
    // Issue #134. Every object-WRITING git (fetch/commit/push) ends by spawning a detached
    // `git maintenance run --auto --detach` that outlives the process we awaited and keeps
    // writing inside `.git` — so an `fs.rm` of that tree can hit ENOTEMPTY (`force: true`
    // suppresses ENOENT, not ENOTEMPTY). Pinned HERE, inline, rather than only written into
    // each repo's config: these pins are highest precedence, they apply warm or cold, they
    // need no ordering care, and a planted config file cannot override them. The repo-local
    // writes are kept as well — the AGENT's own git (SDK Bash tool) does not go through
    // gitEnv, and that git is the one doing the commits.
    //
    // BOTH keys, and neither subsumes the other across the version range: on git 2.54 (the
    // shipped worker image) `prepare_auto_maintenance` reads ONLY `maintenance.auto`, so
    // `gc.auto=0` alone leaves the spawn intact; on 2.55 it gained a `gc.auto` fallback, so
    // `gc.auto=0` alone suffices there — but an explicit `maintenance.auto=true` re-enables
    // the spawn regardless of `gc.auto`. Setting both is correct on either.
    ["maintenance.auto", "false"],
    ["gc.auto", "0"],
    ...GIT_CODE_EXEC_KEY_PINS.map(([k, v]) => [k, v] as [string, string]),
  ];
  if (pat) {
    // issue #2213: a quarantined worker builds no forge credential into a git child's environment.
    assertResidueQuarantineOpen("git");
    // HTTP Basic (base64(user:pat)) — git-over-HTTPS auth, unlike GitLab's
    // REST-only PRIVATE-TOKEN. Scope the header + pin followRedirects to the repo
    // host so neither the credential nor a redirect can reach another host.
    const headerKey = httpScope ? `http.${httpScope}.extraHeader` : "http.extraHeader";
    const redirKey = httpScope ? `http.${httpScope}.followRedirects` : "http.followRedirects";
    pairs.push([headerKey, `Authorization: Basic ${gitBasicCredential(pat, username)}`]);
    pairs.push([redirKey, "false"]);
  }
  // count starts at 0: the replacement env carries no inherited GIT_CONFIG_COUNT.
  let count = 0;
  for (const [k, v] of pairs) {
    env[`GIT_CONFIG_KEY_${count}`] = k;
    env[`GIT_CONFIG_VALUE_${count}`] = v;
    count++;
  }
  env.GIT_CONFIG_COUNT = String(count);
  return env;
}

/**
 * The base64 credential for git-over-HTTPS Basic auth: base64(`user:pat`), with
 * `user` the bot login when known else the conventional `oauth2`. Exported so the
 * runner can register this exact blob with the redactor / secret registry —
 * defense in depth: it only ever lives in GIT_CONFIG_VALUE_n (never argv/logs),
 * but if future code ever logged the git env, the scrubber would catch it. Keep
 * the fallback in lockstep with gitEnv's, so the two never drift.
 */
export function gitBasicCredential(pat: string, username?: string): string {
  const user = username?.trim() || "oauth2";
  return Buffer.from(`${user}:${pat}`).toString("base64");
}

/**
 * The `http.<scope>.*` prefix that host-scopes credential config to a repo's own
 * host, e.g. `https://gitlab.example.com/` for any https URL on that host.
 * Returns undefined for a hostless URL (local path / scp-style), where http.*
 * config does not apply.
 */
/**
 * PRD #456 M1 — true when a push error is GitHub's workflow-scope rejection: the bot's
 * repo-only PAT is refused when the pushed tip's `.github/workflows/**` tree differs from
 * the default branch. Matches the stable phrase `workflow scope` (case-insensitive), which
 * appears in both `without workflow scope` and the full `refusing to allow a Personal Access
 * Token to create or update workflow .github/workflows/<f> without workflow scope`. Used to
 * decide whether a merge-aligned push that STILL failed warrants the rebase fallback (a
 * genuine workflow-scope reject) versus an unrelated push failure (which rethrows).
 */
export function isWorkflowScopeRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return msg.includes("workflow scope");
}

/**
 * PRD #974 M2 — true when a push error is GitHub Push Protection's secret rejection: the
 * remote refuses the push because a commit in it carries a secret Push Protection detected.
 * GitHub's rejection reads `remote: … GH013 … Push cannot contain secrets` (with a per-secret
 * detail block), so this matches any of the stable tokens `gh013`, `push cannot contain
 * secrets`, `push protection`, or `secret detected` (case-insensitive). Mirrors
 * isWorkflowScopeRejection's shape.
 *
 * This is the remote BACKSTOP for the case the pre-push gitleaks scan (default ruleset) misses
 * a secret GitHub's own scanner catches — GitHub's pattern set is broader than gitleaks' and
 * the two are not identical, so a clean pre-push scan does not guarantee GitHub accepts the
 * push. Routing this rejection to the same typed `push_secret_blocked` fail_origin gives an
 * actionable, typed failure (the committed work stays recoverable from the run branch/PVC) instead
 * of GitHub's opaque remote reject discarding it.
 */
export function isPushProtectionRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return (
    msg.includes("gh013") ||
    msg.includes("push cannot contain secrets") ||
    msg.includes("push protection") ||
    msg.includes("secret detected")
  );
}

/**
 * PRD #456 NB2 — true when a push error is a non-fast-forward rejection: the remote
 * refused a non-forced push because the pushed tip is not a descendant of the branch's
 * already-published tip. This is the resumed / rewritten-history case: when the finalize
 * base-align rebase fallback rewinds to the original agent tip and replays the commits,
 * it rewrites SHAs that were already published at origin (a resume — including a resumed
 * `self_improve` cycle reattaching to its own fresh-per-cycle branch), so the subsequent
 * non-forced push cannot fast-forward. Force-push is
 * denied by the guardrails by design, so the correct outcome is the typed
 * base-align-conflict preserve path (preserved_patch + `finalize_base_align_conflict`),
 * not a push retry — routing here keeps it off the generic catch (raw error, defaulted
 * `fail_origin`, no preserved diff). Matches the stable git phrases `non-fast-forward` and
 * `fetch first` (case-insensitive), the same phrases forge-retry treats as permanent
 * push-failure patterns; deliberately does NOT match the bare `[rejected]` token, which is
 * too broad.
 */
export function isNonFastForwardRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return msg.includes("non-fast-forward") || msg.includes("fetch first");
}

export function httpScopeForUrl(rawUrl: string): string | undefined {
  try {
    const u = new URL(rawUrl);
    if (u.protocol === "https:" || u.protocol === "http:") {
      return `${u.protocol}//${u.host}/`;
    }
  } catch {
    // Not a URL (scp-style or local path) — no http scope.
  }
  return undefined;
}

/** issue #1597 M2: the ADDED lines of a unified diff (without their leading '+'), newline-joined
 *  with a trailing newline, or "" when there are none. A '+' line counts only inside a hunk (after
 *  an `@@` header, until the next `diff ` header), so `+++ b/<path>` file headers are never taken
 *  for content even when an added line itself starts with "++". */
function addedLinesOfDiff(diff: string): string {
  const out: string[] = [];
  let inHunk = false;
  for (const line of diff.split("\n")) {
    if (line.startsWith("diff ")) inHunk = false;
    else if (line.startsWith("@@")) inHunk = true;
    else if (inHunk && line.startsWith("+")) out.push(line.slice(1));
  }
  return out.length === 0 ? "" : `${out.join("\n")}\n`;
}

/** issue #1597 M2: did a bounded exec fail because its output exceeded maxBuffer (execFile's code,
 *  or execScoped's scoped-branch message)? */
function isOutputOverflow(err: unknown): boolean {
  const e = err as { code?: unknown; message?: unknown };
  return e.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" || /output exceeded|maxBuffer/i.test(String(e.message ?? ""));
}

function gitErrorMessage(err: unknown): string {
  const e = err as { stderr?: unknown; message?: unknown };
  const stderr = typeof e.stderr === "string" ? e.stderr.trim() : "";
  if (stderr) return stderr;
  return typeof e.message === "string" ? e.message : String(err);
}

/** issue #1769: the env materializeRunnerClone runs its runner-uid git under — `base` (the
 *  gitEnv() replacement env) with the runner PATH/TMPDIR, and with every env-level alternate
 *  object source removed, so the verification cannot pass through an alternate object
 *  directory whatever `base` holds. */
export function materializeEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...base, PATH: runnerPath() };
  const tmp = runnerTmpdir();
  if (tmp) env.TMPDIR = tmp;
  delete env.GIT_ALTERNATE_OBJECT_DIRECTORIES;
  delete env.GIT_OBJECT_DIRECTORY;
  return env;
}

/** issue #1769: true when `p` exists (lstat, no symlink follow), false ONLY on ENOENT; every
 *  other error (EACCES, EIO, ...) is rethrown so a caller can fail closed. */
async function lstatPresent(p: string): Promise<boolean> {
  try {
    await fs.lstat(p);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw err;
  }
}

async function pathExists(p: string): Promise<boolean> {
  try {
    await fs.stat(p);
    return true;
  } catch {
    return false;
  }
}

async function isBareRepo(p: string): Promise<boolean> {
  return pathExists(path.join(p, "HEAD"));
}

/**
 * Filesystem-safe, collision-free bare-clone dir name for a repo URL: host plus
 * each path segment joined by '+'. '+' is illegal in GitHub/GitLab path
 * segments, so two names collide only for the same repo on the same host.
 * Examples:
 *   https://gitlab.com/org/repo.git  -> gitlab.com+org+repo.git
 *   git@gitlab.com:org/repo          -> gitlab.com+org+repo.git
 *   /tmp/fx/origin                   -> tmp+fx+origin.git    (local path, hostless)
 */
export function bareDirName(rawUrl: string): string {
  const trimmed = rawUrl.replace(/\/+$/, "");
  const [host, repoPath] = splitHostAndPath(trimmed);
  const parts: string[] = [];
  const normHost = host.trim().toLowerCase().replaceAll(":", "%3A");
  if (normHost) parts.push(normHost);
  for (const seg of repoPath.split("/")) {
    if (seg) parts.push(seg);
  }
  let name = parts.join("+");
  if (!name.endsWith(".git")) name += ".git";
  if (name === ".git") name = "repo.git";
  return name;
}

function splitHostAndPath(rawUrl: string): [host: string, path: string] {
  try {
    const u = new URL(rawUrl);
    if (u.protocol && u.host) return [u.host, u.pathname.replace(/^\//, "")];
  } catch {
    // Not a URL — fall through to scp-style / local-path handling.
  }
  let s = rawUrl;
  const at = s.indexOf("@");
  if (at >= 0) s = s.slice(at + 1);
  const colon = s.indexOf(":");
  if (colon >= 0) return [s.slice(0, colon), s.slice(colon + 1)];
  return ["", s];
}
