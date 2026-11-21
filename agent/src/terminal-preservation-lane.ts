/**
 * In-lane terminal preservation (#4007 slice B, production-dark): builds the engine's injected effects for one flight
 * and runs the terminal operation inside the run's own terminal lane (the finalize permit, or the generic-failure reap).
 *
 * `preserveInLane` is TOTAL: it never throws and every non-publication outcome is `kept`/`unknown`, so the run's own
 * terminal report is never blocked or reshaped by it. The only thing a caller may take from the result is the head
 * proof ({@link terminalHeadAdmitted}): a completed body may carry `completion_final_head` only when this operation
 * published or recorded and the head is a declared manifest root.
 *
 * Nothing here opens a Codex boundary (no `withBoundary`): quiescence is the caller's already-held permit plus the
 * direct `quiesce` closure the runner supplies.
 */
import fs from "node:fs/promises";
import type { CheckpointRange, GitCache, TerminalDefaultBranch } from "./git.js";
import type { TerminalPreservationResult } from "./client.js";
import { prepareFrozenHeldPinned, type FrozenHeldPolicy, type HeldGit, type TerminalDigestGit } from "./frozen-held.js";
import type { RecoveryCoordinator } from "./recovery.js";
import type { TerminalPreservationRequest } from "./protocol.js";
import { terminalPreservationActive, type TerminalPreservationTestHooks } from "./terminal-preservation.js";
import {
  checkManifestCoverage, classifyTerminalOutput, readTerminalSourceUniverse, TERMINAL_SNAPSHOT_SUFFIX,
  type TerminalOutputLabel, type TerminalUniverse,
} from "./terminal-inventory.js";
import {
  runTerminalOperation, type CaptureOutcome, type PrepareOutcome, type TerminalManifest, type TerminalOperationDeps,
} from "./terminal-operation.js";

/** Upper bound of one lane operation, and the permit time kept back for the run's own terminal work. */
const TERMINAL_LANE_CAP_MS = 120_000;
const TERMINAL_LANE_RESERVE_MS = 30_000;
/** The smallest usable window: a permit that leaves less starts nothing. */
const TERMINAL_LANE_MIN_MS = 5_000;
/** fail_origins that forbid any preservation. */
const TERMINAL_LANE_EXCLUDED_ORIGINS: ReadonlySet<string> = new Set(["push_secret_blocked", "worker_residue_blocked"]);

export interface TerminalLaneResult {
  /** The classification of the run's output; head gating applies only to `branch_source`. */
  label: TerminalOutputLabel;
  outcome: "published" | "recorded" | "kept" | "unknown";
  reason?: string;
  /** Declared manifest root commits, present only for published/recorded. */
  roots?: string[];
}

/** The slice of RunFlight the lane reads and writes. RunFlight satisfies it structurally. */
export interface TerminalLaneFlight {
  runId: string;
  claimGeneration: number;
  runKind: string;
  barePath: string | undefined;
  branch: string | undefined;
  worktreePath: string | undefined;
  attemptId: string | undefined;
  runnerClone?: { attemptId?: string | undefined } | undefined;
  secretBlocked?: boolean;
  /** The encoded forms of the run's provider key (absent or empty for a vendor run). */
  providerKeyForms?: readonly string[];
  terminalPreservation?: TerminalLaneResult;
}

export interface TerminalLaneBody { status: string; fail_origin?: string; failure_reason?: string }

type LaneGit = Pick<GitCache, "captureTerminalSnapshot" | "buildTerminalAggregate" | "listRunBares" | "readInventoryCloneHeads" |
  "readTerminalAttribution" | "readTerminalCloneLocalRefs" | "ancestry" | "revParse" | "defaultBranchTip"> & HeldGit & TerminalDigestGit;

export interface TerminalLaneEnv {
  git: LaneGit;
  recovery: Pick<RecoveryCoordinator, "readTerminalRecords" | "terminalOperationStore" | "terminalOperationOwner">;
  client: { publishTerminalPreservation(runId: string, req: TerminalPreservationRequest, signal?: AbortSignal): Promise<TerminalPreservationResult> };
  hooks: TerminalPreservationTestHooks | undefined;
  workerId: string | undefined;
  defaultBranch: string | undefined;
  /**
   * Resolves the default branch when the claim names none (`defaultBranch` undefined). Runs inside the lane's own window:
   * `bound.signal` aborts at the sub-deadline or a permit abort and `bound.deadline` is the sub-deadline (epoch ms). The
   * lane treats an aborted or past-deadline return as a refusal (`default_branch_unresolved`). It returns `resolved` (the
   * name), `absent` (proven no default branch: the only case that falls back to "main") or `unknown` (a read that failed
   * or could not be classified); `unknown` and a throw keep the output as `default_branch_unresolved`.
   */
  lookupDefaultBranch?: (bound: { signal: AbortSignal; deadline: number }) => Promise<TerminalDefaultBranch>;
  /**
   * Reads the default branch's tip under the lane's own window (`bound` as for `lookupDefaultBranch`). Absent: the lane calls
   * `git.defaultBranchTip` directly. A throw is treated as no tip; a read that ends after the window lapsed or its signal aborted refuses the prepare as `deadline`.
   */
  readDefaultBranchTip?: (barePath: string, branch: string, bound: { signal: AbortSignal; deadline: number }) => Promise<string | undefined>;
  egressBound: boolean;
  workerTokenFile?: string;
  /** Run-gate predicates (policy hold, park, claim fence, pending/attempted terminal, permanent-failure reap). */
  skip(): string | undefined;
  /** A prior failure that forbids preservation (excluded fail_origin, secret or residue latch). */
  failureFirst(): string | undefined;
  policy(): FrozenHeldPolicy;
  remediationHold(sha: string): Promise<boolean>;
  ciHold(sha: string): Promise<"clear" | "blocked" | "unknown">;
  /** Direct quiescence proof (never a boundary); throws when not quiescent. Absent when the caller already proved it. */
  quiesce?: (opts: { deadlineAt: number; signal: AbortSignal }) => Promise<void>;
  /** The caller's permit or abort signal, and the permit time left (undefined: unknown). */
  signal?: AbortSignal;
  remainingMs?: number;
  log(step: string, fields: Record<string, unknown>): void;
  now(): number;
}

const result = (label: TerminalOutputLabel, outcome: TerminalLaneResult["outcome"], reason?: string, roots?: string[]): TerminalLaneResult =>
  ({ label, outcome, ...(reason !== undefined ? { reason } : {}), ...(roots !== undefined ? { roots } : {}) });

/** The head proof: published/recorded and `head` is a declared manifest root. Non-branch-source output is unaffected. */
export function terminalHeadAdmitted(flight: Pick<TerminalLaneFlight, "terminalPreservation" | "runKind">, head: string,
  egressBound = false): boolean {
  const r = flight.terminalPreservation;
  const label = r?.label ?? classifyTerminalOutput({ kind: flight.runKind, adoptedSource: true, egressBound });
  if (label !== "branch_source") return true;
  return r !== undefined && (r.outcome === "published" || r.outcome === "recorded") && r.roots !== undefined && r.roots.includes(head);
}

export async function preserveInLane(env: TerminalLaneEnv, flight: TerminalLaneFlight, body: TerminalLaneBody): Promise<TerminalLaneResult> {
  // The gate is the first statement: with it off nothing below runs and nothing is stored on the flight.
  if (!terminalPreservationActive(env.hooks)) return result("unknown", "kept", "disabled");
  const label = classifyTerminalOutput({ kind: flight.runKind, adoptedSource: true, egressBound: env.egressBound });
  try {
    // One operation per flight: a second call (journalTerminalReport after finishCommittedPublish) is the idempotent path.
    if (flight.terminalPreservation !== undefined) return flight.terminalPreservation;
    const out = await run(env, flight, body, label);
    flight.terminalPreservation = out;
    env.log("terminal_preservation", { outcome: out.outcome, reason: out.reason, label: out.label });
    return out;
  } catch (error) {
    const out = result(label, "kept", `lane_error:${error instanceof Error ? error.message.slice(0, 80) : "unknown"}`);
    try { flight.terminalPreservation = out; env.log("terminal_preservation", { outcome: out.outcome, reason: out.reason }); } catch { /* total */ }
    return out;
  }
}

async function run(env: TerminalLaneEnv, flight: TerminalLaneFlight, body: TerminalLaneBody, label: TerminalOutputLabel): Promise<TerminalLaneResult> {
  if (label !== "branch_source") return result(label, "kept", "not_branch_source");
  const skipped = env.skip();
  if (skipped !== undefined) return result(label, "kept", skipped);
  const early = firstFailure(env, body);
  if (early !== undefined) return result(label, "kept", early);
  const attemptId = flight.attemptId ?? flight.runnerClone?.attemptId ?? env.hooks?.attemptId;
  const clonePath = flight.worktreePath, barePath = flight.barePath, branch = flight.branch;
  const workerId = env.workerId;
  if (!attemptId || !clonePath || !barePath || !branch) return result(label, "kept", "source_unknown");
  if (!workerId) return result(label, "kept", "worker_unknown");

  const cap = env.hooks?.capMs ?? TERMINAL_LANE_CAP_MS;
  // The reserve is proportional to the permit, so a short permit (the 30 s Codex boundary) keeps a usable window; an
  // unknown permit remainder runs under the cap alone. A window below the minimum starts nothing.
  const permitMs = env.remainingMs;
  const reserve = permitMs === undefined ? 0
    : Math.min(env.hooks?.reserveMs ?? TERMINAL_LANE_RESERVE_MS, Math.floor(permitMs / 4));
  const sub = permitMs === undefined ? cap : Math.min(cap, permitMs - reserve);
  if (!(sub > 0) || (permitMs !== undefined && sub < Math.min(TERMINAL_LANE_MIN_MS, cap))) return result(label, "kept", "deadline");
  const deadlineAt = env.now() + sub;
  const permitDeadlineAt = permitMs === undefined ? undefined : env.now() + permitMs;
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), sub);
  timer.unref?.();
  const onPermitAbort = (): void => abort.abort();
  if (env.signal?.aborted) abort.abort(); else env.signal?.addEventListener("abort", onPermitAbort, { once: true });
  try {
    // Quiescence is proven directly (never through a boundary) before anything is read or written.
    let quiesced = env.quiesce === undefined;
    if (env.quiesce) {
      try { await env.quiesce({ deadlineAt, signal: abort.signal }); quiesced = true; } catch { return result(label, "kept", "not_quiescent"); }
      // A proof that finished after the sub-deadline or a permit abort starts nothing: no store, universe or capture call.
      if (abort.signal.aborted || env.now() >= deadlineAt) return result(label, "kept", "deadline");
    }
    // A claim without a default branch resolves it here, inside the window; a lookup the window cut short keeps the output
    // rather than guessing "main". Only a lookup that proved no default branch exists falls back to "main"; an unknown
    // answer or a throw keeps the output before any universe, capture, prepare or send.
    let defaultBranch = env.defaultBranch;
    if (defaultBranch === undefined && env.lookupDefaultBranch) {
      let found: TerminalDefaultBranch;
      try { found = await env.lookupDefaultBranch({ signal: abort.signal, deadline: deadlineAt }); } catch { found = { kind: "unknown", detail: "lookup threw" }; }
      if (abort.signal.aborted || env.now() >= deadlineAt) return result(label, "kept", "default_branch_unresolved");
      // Closed mapping: only a proven absence becomes "main" and only a named resolution is used; anything else keeps.
      if (found.kind === "absent") defaultBranch = "main";
      else if (found.kind === "resolved" && found.name) defaultBranch = found.name;
      else return result(label, "kept", "default_branch_unresolved");
    }
    const boundaryValid = (): boolean => !abort.signal.aborted && env.now() < deadlineAt && quiesced;
    const left = (): number => Math.max(1, deadlineAt - env.now());
    let proved: Extract<TerminalUniverse, { kind: "proved" }> | undefined;
    let preparedTip: string | undefined;
    const cloneRef = `refs/uzi-terminal-local/${flight.runId}/g${flight.claimGeneration}`;

    const base: TerminalOperationDeps = {
      enabled: () => terminalPreservationActive(env.hooks),
      store: env.hooks?.store ?? env.recovery.terminalOperationStore(),
      owner: env.hooks?.owner ?? (fn => env.recovery.terminalOperationOwner(flight.runId, flight.claimGeneration)(fn)),
      failureFirst: () => firstFailure(env, body),
      universe: async () => {
        const st = await fs.lstat(clonePath);
        const u = await readTerminalSourceUniverse({
          git: env.git, recovery: env.recovery, runId: flight.runId,
          current: { attemptId, clonePath, identity: `${st.dev}:${st.ino}` }, signal: abort.signal, deadline: deadlineAt,
        });
        if (u.kind === "proved") proved = u;
        return u;
      },
      capture: async (onPhase): Promise<CaptureOutcome> => {
        if (!proved || proved.sources.length !== 1) return { kind: "refused", reason: "universe_shape" };
        const source = proved.sources[0]!;
        const snap = await env.git.captureTerminalSnapshot(clonePath, {
          runId: flight.runId, generation: flight.claimGeneration, deadlineMs: left(), signal: abort.signal, barePath,
          ...(env.workerTokenFile !== undefined ? { workerTokenFile: env.workerTokenFile } : {}),
          onPhase, reprove: async () => boundaryValid(),
          // Cleanup after a lapsed window (at most 2 s, inside the reserve) never outlives the permit and stops on a permit abort.
          ...(permitDeadlineAt !== undefined || env.signal
            ? { cleanupBound: {
              ...(permitDeadlineAt !== undefined ? { deadlineAt: permitDeadlineAt } : {}),
              ...(env.signal ? { signal: env.signal } : {}),
            } } : {}),
        });
        if (snap.remnant) env.log("terminal_snapshot_remnant", { remnant: snap.remnant });
        if (snap.kind === "refused") return { kind: "refused", reason: snap.reason };
        // A clean tree transfers nothing, so the snapshot never reports a phase: the record is still written first.
        if (snap.kind === "clean") {
          await onPhase("capturing", { cloneRef, bareRef: cloneRef });
          await onPhase("captured", { cloneRef, bareRef: cloneRef, commit: snap.root });
        }
        const roots: TerminalManifest["roots"] = [];
        for (const r of proved.roots) {
          const tree = await env.git.revParse(source.bare, `${r.commit}^{tree}`);
          if (!tree) return { kind: "refused", reason: "tree_unreadable" };
          roots.push({ source_id: source.sourceId, attempt_id: source.attemptId, commit: r.commit, tree });
        }
        // A captured snapshot is a commit of its own that the aggregate is built on: it is declared as the one extra root.
        const snapshot = snap.kind === "captured" ? { commit: snap.root, tree: snap.tree } : undefined;
        if (snapshot) roots.push({ source_id: `${source.sourceId}${TERMINAL_SNAPSHOT_SUFFIX}`, attempt_id: source.attemptId, ...snapshot });
        const manifest: TerminalManifest = {
          run_id: flight.runId, generation: flight.claimGeneration, worker_id: workerId, roots,
          abandoned: snap.ignored.files > 0 ? [{ class: "ignored", source_id: source.sourceId, extents: snap.ignored }] : [],
        };
        return { kind: "ok", base: snap.root, extraRoots: proved.roots.map(r => r.commit), manifest, ...(snapshot ? { snapshot } : {}) };
      },
      aggregate: input => env.git.buildTerminalAggregate(barePath, {
        ...input, runId: flight.runId, generation: flight.claimGeneration,
        budget: { signal: abort.signal, deadline: Date.now() + left() },
      }),
      prepare: async (range: CheckpointRange | "resolve", expectDigests, tip): Promise<PrepareOutcome> => {
        let flagged: string[] = [];
        let ciTarget: string | undefined;
        if (defaultBranch && env.readDefaultBranchTip) {
          try { ciTarget = await env.readDefaultBranchTip(barePath, defaultBranch, { signal: abort.signal, deadline: deadlineAt }); } catch { ciTarget = undefined; }
          // A tip read cut short by the window must not proceed with a missing ciTarget.
          if (abort.signal.aborted || env.now() >= deadlineAt) return { reason: "deadline" };
        } else if (defaultBranch) {
          ciTarget = await env.git.defaultBranchTip(barePath, defaultBranch);
        }
        const out = await prepareFrozenHeldPinned({
          git: env.git, barePath, branch, sourceSha: tip, policy: env.policy(), deadlineMs: left(),
          literals: flight.providerKeyForms ?? [],
          ...(defaultBranch ? { defaultBranch } : {}),
          perCommitCi: true, ...(ciTarget ? { ciTarget } : {}), ...(expectDigests ? { expectDigests } : {}),
          onScan: scan => { if (scan.trusted && scan.findings.length) flagged = scan.findings.map(f => f.commit); },
          remediationHold: () => env.remediationHold(tip), ciHold: () => env.ciHold(tip),
        }, range);
        return "pack" in out ? out : { ...out, ...(out.reason === "secret_found" ? { flaggedCommits: flagged } : {}) };
      },
      coverage: (universe, manifest, snapshot) => checkManifestCoverage(universe, manifest, { currentFlight: true, ...(snapshot ? { snapshot } : {}) }),
      // The late gate before the send: the latches and the policy again, then the remediation hold against the prepared tip
      // (a hold that appeared after prepare must stop the send).
      recheck: async () => firstFailure(env, body) ?? (preparedTip !== undefined && await env.remediationHold(preparedTip)
        ? "secret_remediation_pending" : undefined),
      boundaryValid,
      send: (req, signal) => env.client.publishTerminalPreservation(flight.runId, req, signal),
      now: () => env.now(),
    };
    const merged: TerminalOperationDeps = { ...base, ...env.hooks?.deps };
    // Record the tip the engine prepares (hooked or real) so the late recheck can test the hold against it.
    const deps: TerminalOperationDeps = {
      ...merged, prepare: (range, expectDigests, tip) => { preparedTip = tip; return merged.prepare(range, expectDigests, tip); },
    };
    const op = await runTerminalOperation(deps, {
      runId: flight.runId, workerId, generation: flight.claimGeneration, authorityClass: "current_flight",
      clonePath, mode: "initial", signal: abort.signal,
    });
    if (op.outcome === "published" || op.outcome === "recorded") return result(label, op.outcome, undefined, op.manifest.roots.map(r => r.commit));
    if (op.outcome === "unknown") return result(label, "unknown", op.reason);
    if (op.outcome !== "kept") return result(label, "kept", "unexpected_outcome");
    if (op.reason === "secret_found" || op.reason === "secret_found_unlatched") flight.secretBlocked = true;
    return result(label, "kept", op.reason);
  } finally {
    clearTimeout(timer);
    env.signal?.removeEventListener("abort", onPermitAbort);
  }
}

function firstFailure(env: TerminalLaneEnv, body: TerminalLaneBody): string | undefined {
  if (body.fail_origin !== undefined && TERMINAL_LANE_EXCLUDED_ORIGINS.has(body.fail_origin)) return body.fail_origin;
  const latched = env.failureFirst();
  if (latched !== undefined) return latched;
  // Active secret remediation refuses before any universe read, capture or record (and again before the send).
  return env.policy().blocked ? "secret_remediation_pending" : undefined;
}
