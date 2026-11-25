// issue #4628: fresh resume, the destination worker's side. An owner Resume of a held run starts it on a separate hosted worker W2
// from durable evidence the server selected: an unsealed capture bundle, or the broker-owned checkpoint ref. The claim carries a
// `fresh_resume` object; this module decides what the destination does with it, in the order the design fixes:
//
//   preempt  a fresh claim skips retained discovery. If the bare already holds ANY local state for the branch (another run's
//            restore composite included) the transfer is abandoned (`local_state_present`): the destination never seeds on top.
//   A        the destination attempt id, minted from the claim generation G' (never a `gx` id).
//   proof    capture: stream the bundle, `git bundle verify`, fetch it; checkpoint ref: the ref must equal the server sha.
//   L0       fresh journal {frid, A, G', sha, seeding}        one `git config --local` write
//   L1       ledger entry A `fresh_pending`                    one write
//   L2       refusal checks, assertBareClear + admission, seed with the fresh normalization (all under the bare lock)
//   L3       POST seeded {G', A, live generation}              the server's receipt, which carries current_authority
//   L4       recovery journal A + ledger `live` + fresh journal `ready`, ONE rename under the bare lock
//
// L4 and the model start happen only when the receipt says this worker still holds the run's live claim. Every failure before the
// receipt abandons the transfer and stops nonterminally: the run is never reported `failed` from here. Failure model: a process or
// container crash. No power-loss durability is claimed (no fsync of the config is added beyond what the existing rewrite does).
//
// FreshReconcileStop (the nonterminal stop) lives in runner.ts beside RestoreRouteStop; this module throws the typed causes below
// and the runner wraps them.

import type { Writable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { FreshConfigLockedError, FreshSeedError, type FreshCommitInput, type FreshSeedInput, type GitCache, type RunnerClone } from "./git.js";
import { freshResumeRefusalReason, type FreshResumeState, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { StateAck } from "./protocol.js";
import { mintAttemptId } from "./run-quiescence.js";
import type { FreshJournal } from "./fresh-journal.js";
import { InventorySourceExclusionRefusal } from "./inventory-exclusion.js";

export const FRESH_RESUME_CAPABILITY = "fresh_resume_v1";

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const SHA40_RE = /^[0-9a-f]{40}$/;

/** True for a hosted slot: `UZI_POD_UID` is a UUID (the same test restore-boundary.ts applies). Only a hosted slot advertises
 *  fresh_resume_v1, because only a hosted persistent worker is a fresh resume destination. */
export function isHostedSlot(env: { UZI_POD_UID?: string | undefined } = process.env): boolean {
  return typeof env.UZI_POD_UID === "string" && UUID_RE.test(env.UZI_POD_UID);
}

/** The capability list this worker adds for fresh resume: empty unless the slot is hosted. */
export function freshResumeCapabilities(env: { UZI_POD_UID?: string | undefined } = process.env): string[] {
  return isHostedSlot(env) ? [FRESH_RESUME_CAPABILITY] : [];
}

/** The claim's `fresh_resume` object, decoded. */
export interface FreshResumeClaim {
  id: string;
  kind: "capture" | "checkpoint_ref";
  captureId?: string;
  checkpointRef?: string;
  sha: string;
  evidenceGeneration: number;
}

/** A `fresh_resume` object that is present and malformed. The run is not failed for it; the caller stops nonterminally. */
export class FreshResumeClaimError extends Error {
  constructor(detail: string) { super(`fresh resume claim is malformed: ${detail}`); this.name = "FreshResumeClaimError"; }
}

/** Decode the claim's `fresh_resume`. Undefined when absent; a malformed present value throws (it is never read as absent). */
export function decodeFreshResumeClaim(raw: unknown): FreshResumeClaim | undefined {
  if (raw === undefined || raw === null) return undefined;
  if (typeof raw !== "object" || Array.isArray(raw)) throw new FreshResumeClaimError("not an object");
  const o = raw as Record<string, unknown>;
  if (typeof o.id !== "string" || !UUID_RE.test(o.id)) throw new FreshResumeClaimError("id");
  if (o.kind !== "capture" && o.kind !== "checkpoint_ref") throw new FreshResumeClaimError("kind");
  if (typeof o.sha !== "string" || !SHA40_RE.test(o.sha)) throw new FreshResumeClaimError("sha");
  if (typeof o.evidence_generation !== "number" || !Number.isSafeInteger(o.evidence_generation) || o.evidence_generation <= 0)
    throw new FreshResumeClaimError("evidence_generation");
  const out: FreshResumeClaim = { id: o.id.toLowerCase(), kind: o.kind, sha: o.sha, evidenceGeneration: o.evidence_generation };
  if (o.kind === "capture") {
    if (typeof o.capture_id !== "string" || !UUID_RE.test(o.capture_id)) throw new FreshResumeClaimError("capture_id");
    out.captureId = o.capture_id;
  } else {
    if (typeof o.checkpoint_ref !== "string" || !o.checkpoint_ref.startsWith("refs/uzi-checkpoints/") || /[\s\0]/.test(o.checkpoint_ref) ||
        o.checkpoint_ref.includes("..")) throw new FreshResumeClaimError("checkpoint_ref");
    out.checkpointRef = o.checkpoint_ref;
  }
  return out;
}

/** True for a park or fail report the server refused because this worker's transfer is claimed and not yet seeded. */
export function isFreshPreSeedAck(ack: Pick<StateAck, "freshResumePreSeed">): boolean {
  return ack.freshResumePreSeed === true;
}

/** The transfer was given back (abandoned) before its receipt; the flight stops nonterminally and the run stays held. */
export class FreshAbandoned extends Error {
  constructor(readonly reason: string, options?: ErrorOptions) { super(`fresh resume abandoned (${reason})`, options); this.name = "FreshAbandoned"; }
}

/** The receipt is valid but this worker no longer holds the run's live claim: L4 and the model start do not happen. */
export class FreshNoAuthority extends Error {
  constructor(readonly receipt: FreshResumeState) { super("fresh resume receipt without current authority"); this.name = "FreshNoAuthority"; }
}

export interface FreshDeps {
  git: Pick<GitCache, "readFreshLocalState" | "fetchFreshBundle" | "seedFreshAttempt" | "commitFreshReceipt" | "markFreshUnbound">;
  client: Pick<WorkerClient, "freshResumeBundle" | "abandonFreshResume" | "reportFreshResumeSeeded">;
  log: Logger;
}

/** The abandon POST's own deadline. It is deliberately independent of the run's lifecycle signals: a shutdown or a cancel is a reason
 *  to abandon, so it must never abort the abandon itself. */
const FRESH_ABANDON_TIMEOUT_MS = 10_000;

/** What an abandon attempt ended as, filled in by {@link preemptFresh} and {@link seedFresh} for the caller even when they throw a
 *  guard error rather than {@link FreshAbandoned}. `closed` is true only when the server answered the abandon as closed. */
export interface FreshAbandonOutcome { closed: boolean }

/** POST abandon, best effort: never throws. True when the server closed the transfer. A 409 means it is no longer ours to abandon. */
export async function abandonFresh(deps: FreshDeps, runId: string, fr: FreshResumeClaim, generation: number, reason: string): Promise<boolean> {
  try {
    await deps.client.abandonFreshResume(runId, fr.id, generation, AbortSignal.timeout(FRESH_ABANDON_TIMEOUT_MS));
    deps.log.warn("fresh resume transfer abandoned", { run_id: runId, transfer: fr.id, reason });
    return true;
  } catch (error) {
    deps.log.warn("fresh resume abandon was not accepted", { run_id: runId, transfer: fr.id, reason, server_reason: freshResumeRefusalReason(error),
      error: error instanceof Error ? error.message : String(error) });
    return false;
  }
}

/** Preemption: a fresh claim skips retained discovery, and abandons when the bare holds any attribution for the branch. A bare
 *  that does not exist yet holds none. Throws {@link FreshAbandoned} after the abandon: `local_state_present` when the bare holds
 *  local state, `local_state_unreadable` when it cannot be read (an unreadable bare cannot prove it is clear). */
export async function preemptFresh(deps: FreshDeps, p: {
  runId: string; fr: FreshResumeClaim; generation: number; barePath: string; branch: string; cachePresent: boolean; outcome?: FreshAbandonOutcome;
}): Promise<void> {
  if (!p.cachePresent) return;
  // A bare that cannot be read cannot prove it is clear: that is a refusal to seed like any other, so it abandons too.
  let state: string[];
  try {
    state = await deps.git.readFreshLocalState(p.barePath, p.branch, p.runId);
  } catch (error) {
    deps.log.warn("fresh resume: the bare's local state could not be read; abandoning", { run_id: p.runId, error: error instanceof Error ? error.message : String(error) });
    const closed = await abandonFresh(deps, p.runId, p.fr, p.generation, "local_state_unreadable");
    if (p.outcome) p.outcome.closed = closed;
    throw new FreshAbandoned("local_state_unreadable", { cause: error });
  }
  if (state.length === 0) return;
  deps.log.warn("fresh resume: the bare holds local state for this branch; abandoning", { run_id: p.runId, state });
  const closed = await abandonFresh(deps, p.runId, p.fr, p.generation, "local_state_present");
  if (p.outcome) p.outcome.closed = closed;
  throw new FreshAbandoned("local_state_present");
}

/** A, the destination attempt id, from G'. A missing or non-positive generation has no valid A: never a `gx` id. */
export function produceFreshAttemptId(generation: number | undefined, now: Date): string | undefined {
  if (typeof generation !== "number" || !Number.isSafeInteger(generation) || generation <= 0) return undefined;
  return mintAttemptId(generation, now);
}

/** Make the evidence present in the bare and return the ref that must equal the server sha. A capture is streamed, verified and
 *  fetched; a checkpoint ref is already mirrored by the clone's fetch and must resolve to the sha. Missing proof throws. */
async function provideEvidence(deps: FreshDeps, p: { runId: string; fr: FreshResumeClaim; generation: number; barePath: string; signal?: AbortSignal }):
  Promise<string> {
  const { fr } = p;
  if (fr.kind === "checkpoint_ref") return fr.checkpointRef!;
  return deps.git.fetchFreshBundle(p.barePath, { frid: fr.id, sha: fr.sha }, async (sink: Writable) => {
    await deps.client.freshResumeBundle(p.runId, fr.id, p.generation, body => pipeline(body, sink), p.signal);
  });
}

export interface FreshSeedParams {
  runId: string;
  fr: FreshResumeClaim;
  /** G', the claim's generation (also the live generation this worker holds now). */
  generation: number;
  /** A, from {@link produceFreshAttemptId}. */
  attemptId: string;
  barePath: string;
  branch: string;
  key: string;
  selfContained: boolean;
  isLive(clonePath: string): boolean;
  beforeSeed: FreshSeedInput["beforeSeed"];
  onSeeded?: FreshSeedInput["onSeeded"];
  /** Wraps the L2 seed only (the runner's authority transaction), never the network calls around it. */
  seedScope?: <T>(fn: () => Promise<T>) => Promise<T>;
  /** Called between steps: it throws for a shutdown, a cancel or a lost claim, which then abandon the transfer. */
  guard?: () => void;
  /** Waits (outside the bare lock) for a retained inventory exclusion to settle; the seed is retried once after it returns. */
  awaitExclusion?: (refusal: InventorySourceExclusionRefusal) => Promise<void>;
  signal?: AbortSignal;
  deadlineMs?: number;
  /** Receives whether the abandon was answered as closed (see {@link FreshAbandonOutcome}). */
  outcome?: FreshAbandonOutcome;
}

export interface FreshSeedResult {
  attemptId: string;
  seeded: RunnerClone;
  receipt: FreshResumeState;
}

/**
 * L0 to L3. Returns the seeded clone and the server's receipt; it does NOT commit (L4): the caller commits only when
 * `receipt.current_authority` is true ({@link commitFreshReceipt}). Any failure before a valid receipt abandons the transfer (best
 * effort) and throws {@link FreshAbandoned}; a `config.lock` that is already there throws {@link FreshConfigLockedError} untouched
 * after the abandon, never removed and never retried.
 */
export async function seedFresh(deps: FreshDeps, p: FreshSeedParams): Promise<FreshSeedResult> {
  const { fr } = p;
  const { generation, attemptId } = p;
  let seeded: RunnerClone | undefined;
  // A guard failure (a shutdown, a cancel, a lost claim) abandons like any other failure and then propagates as itself, so the
  // runner's own stop handling for it still applies.
  let guardFailure: { error: unknown } | undefined;
  const guard = (): void => {
    try { p.guard?.(); } catch (error) { guardFailure = { error }; throw error; }
  };
  try {
    guard();
    const artifactRef = await provideEvidence(deps, { runId: p.runId, fr, generation, barePath: p.barePath, ...(p.signal ? { signal: p.signal } : {}) });
    guard();
    const seed = (): Promise<RunnerClone> => deps.git.seedFreshAttempt({
      barePath: p.barePath, branch: p.branch, key: p.key, runId: p.runId, attemptId, frid: fr.id, generation,
      artifact: { kind: fr.kind, artifactRef, artifactSha: fr.sha }, selfContained: p.selfContained,
      isLive: p.isLive, beforeSeed: p.beforeSeed, ...(p.onSeeded ? { onSeeded: p.onSeeded } : {}),
      ...(p.signal ? { signal: p.signal } : {}), ...(p.deadlineMs !== undefined ? { deadlineMs: p.deadlineMs } : {}),
    });
    const scoped = (): Promise<RunnerClone> => (p.seedScope ? p.seedScope(seed) : seed());
    try {
      seeded = await scoped();
    } catch (first) {
      // issue #4643: one wait outside the bare lock for a retained inventory exclusion, then one retry. The evidence is not provided again.
      if (!(first instanceof InventorySourceExclusionRefusal) || p.awaitExclusion === undefined) throw first;
      try { await p.awaitExclusion(first); } catch (error) { guardFailure = { error }; throw error; }
      guard();
      seeded = await scoped();
    }
    guard();
    const receipt = await deps.client.reportFreshResumeSeeded(p.runId, fr.id,
      { claim_generation: generation, attempt_id: attemptId, live_generation: generation }, p.signal);
    return { attemptId, seeded, receipt };
  } catch (error) {
    const closed = await abandonFresh(deps, p.runId, fr, generation, errorReason(error));
    if (p.outcome) p.outcome.closed = closed;
    // The one rule for `fresh_unbound`: only after the server answered the abandon as closed. A failed abandon leaves A pending for
    // discovery to reconcile. markFreshUnbound moves only a pending entry of this run, so it is a no-op when L1 was never written.
    if (closed) {
      await deps.git.markFreshUnbound({ barePath: p.barePath, branch: p.branch, key: p.key, runId: p.runId, attemptId }).catch(() => undefined);
    }
    if (guardFailure !== undefined) throw guardFailure.error;
    if (error instanceof FreshConfigLockedError) throw error;
    throw new FreshAbandoned(errorReason(error), { cause: error });
  }
}

function errorReason(error: unknown): string {
  if (error instanceof FreshConfigLockedError) return "config_locked";
  if (error instanceof FreshSeedError) return error.reason;
  if (error instanceof InventorySourceExclusionRefusal) return "inventory_source_exclusion";
  const refusal = freshResumeRefusalReason(error);
  if (refusal !== undefined) return refusal;
  if (error instanceof Error && (error.name === "AbortError" || error.name === "TimeoutError")) return "aborted";
  return "seed_failed";
}

/**
 * L4, the receipt commit. Runs only when the server's receipt names this worker as holding the run's live claim: a valid receipt
 * without authority throws {@link FreshNoAuthority} and writes nothing. One rename under the bare lock; a `config.lock` that already
 * exists is a {@link FreshConfigLockedError} (never removed, never retried). m3-B's restart reconciler calls this too.
 */
export async function commitFreshReceipt(deps: Pick<FreshDeps, "git">, receipt: FreshResumeState, input: FreshCommitInput):
  Promise<"committed" | "already"> {
  if (!receipt.current_authority) throw new FreshNoAuthority(receipt);
  return deps.git.commitFreshReceipt(input);
}

/** A restart's reconciliation of an L0 journal could not decide: the stop is nonterminal, custody stays, and nothing is marked unbound. */
export class FreshUnresolved extends Error {
  constructor(readonly reason: string, options?: ErrorOptions) { super(`fresh resume reconciliation stopped (${reason})`, options); this.name = "FreshUnresolved"; }
}

export type FreshOutcome =
  | { kind: "positive"; state: FreshResumeState }
  | { kind: "negative"; reason: string }
  | { kind: "stop"; reason: string };

/**
 * What the server's answer for the journal's own transfer means. Only an exact, unambiguous answer decides:
 *   positive  `seeded`, with this journal's attempt A and generation G'.
 *   negative  `refused` or `cancelled` with no seed ever recorded: the transfer will never bind A.
 *   stop      everything else: a missing record (404), a seeded row naming another attempt or generation, a terminal row that does
 *             carry a seed, or a state this worker does not know. None of these is ever read as negative.
 * The caller maps an unavailable or malformed reply to a stop without calling this.
 */
export function classifyFreshState(journal: Pick<FreshJournal, "attemptId" | "generation">, state: FreshResumeState | null): FreshOutcome {
  if (state === null) return { kind: "stop", reason: "state_missing" };
  if (state.state === "seeded") {
    if (state.seeded_at === null || state.destination_attempt_id === null) return { kind: "stop", reason: "seeded_without_attempt" };
    if (state.destination_attempt_id !== journal.attemptId) return { kind: "stop", reason: "attempt_differs" };
    if (state.seeded_generation !== journal.generation) return { kind: "stop", reason: "generation_differs" };
    return { kind: "positive", state };
  }
  if ((state.state === "refused" || state.state === "cancelled") && state.seeded_at === null) return { kind: "negative", reason: state.state };
  return { kind: "stop", reason: "state_not_decisive" };
}
