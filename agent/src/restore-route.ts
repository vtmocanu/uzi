// Issue #3644 Gate B (m4a): the restore route's classification and read-only eligibility pass.
//
// After a worker was killed with its source active, the restarted worker's claim finds a predecessor P whose authority slot is
// still active under the dead incarnation. The legacy retained path would reserve an iteration (rewriting P's journal) and then
// be refused by source-use admission. The restore route instead decides, from evidence that never involves P's clone, whether
// an independent restore from the verified pin is possible. NOTHING here reads P's contents or tree, walks its path, or runs
// Git, capture, normalization, reaping or quiescence on it: the only touches of the path are the authority store's own slot-key
// derivation inside `peekSlot`, and the bare's config and objects.
//
// The pass only DECIDES. The runner turns a refusal into a nonterminal stop that writes nothing; an eligible verdict goes on to the
// seeding transaction (runner.ts restoreFromPin), which re-derives the whole verdict under the authority transaction, the
// recovery journal lock and the bare lock and seeds only if it is unchanged (the verdict object is what it compares).
import { createHash } from "node:crypto";
import { attemptClonePath, attemptGeneration, cloneKeyOf } from "./attempt-path.js";
import {
  parseAttemptLedgerEntry, recoveryCaptureKey, stoppedMarkerKey,
  type BareAttributionEntry, type RestorePinSpec, type RestorePinVerdict,
} from "./git.js";
import type { SlotSnapshot } from "./completion-authority.js";
import type { RecoveryRecord } from "./recovery.js";
import { classifyRecoveryOrigin } from "./recovery-origin.js";
import {
  evaluateComposeRestore, runAttributionCensus,
  selfRecordMatchingLive,
  type BoundaryRead, type BoundaryRecord, type BoundarySlot, type IdentityRefusal, type LiveBoundaryIdentity, type RestoreRefusal,
  type RestoreUnsupported, type RestoreVerdict,
} from "./restore-boundary.js";

/** Refusals decided from the bare config alone, before any reservation (plan 3.2). */
export type ClassificationRefusal =
  | "uncertain_publication" | "predecessor_not_plain" | "conflicting_attribution" | "predecessor_ledger" | "attribution_unverifiable"
  | "predecessor_path";

/** Every reason the route can refuse with. */
export type RestoreRouteReason =
  | ClassificationRefusal | RestoreRefusal
  | "predecessor_changed" | "slot_unreadable" | "no_eligible_pin" | "pin_ambiguous" | "pin_bare_mismatch" | "pin_unbound" | "pin_closure_invalid"
  | "checkpoint_not_ancestor" | "certified_completion_unavailable" | "evaluation_failed";

const SHA40 = /^[0-9a-f]{40}$/;

function namesWholePath(text: string, p: string): boolean {
  for (let i = text.indexOf(p); i >= 0; i = text.indexOf(p, i + 1)) {
    if (!/[A-Za-z0-9_-]/.test(text.charAt(i + p.length))) return true;
  }
  return false;
}

export interface RelationshipInput {
  runId: string;
  branch: string;
  /** P's journal value exactly as discovery read it from the active key. */
  journalRaw: string;
  /** The bare's attribution config, one snapshot. */
  entries: readonly BareAttributionEntry[];
  /** False when the config read hit a cap or an error. */
  complete: boolean;
  /** The runner clone path of this run's canonical key (git.ts runnerClonePath), the only path a canonical journal may name. */
  canonicalClonePath: string;
}

export type RelationshipResult =
  | { ok: true; clonePath: string; attemptId: string; generation: number }
  | { ok: false; reason: ClassificationRefusal; canonicalCandidate?: undefined }
  | { ok: false; reason: "predecessor_not_plain"; canonicalCandidate: true; clonePath: string };

/**
 * Plan 3.2 item 1: a journal that is exactly `{runId, clonePath}` for this run at the canonical clone path is what an attempt-less
 * source writes. It is a canonical CANDIDATE only when the same config snapshot also shows no other attribution of the run: P's key
 * holds exactly this one value, no stopped marker or protected descriptor, no other key or value naming the run or the path, and no
 * same-run ledger entry that is `live` or `reclaimed`. Every other non-attempt shape stays a plain `predecessor_not_plain`.
 */
function classifyCanonicalShape(input: RelationshipInput, j: Record<string, unknown>): RelationshipResult {
  const plain: RelationshipResult = { ok: false, reason: "predecessor_not_plain" };
  if (Object.keys(j).sort().join(",") !== "clonePath,runId" || j.runId !== input.runId || j.clonePath !== input.canonicalClonePath) return plain;
  if (!input.complete) return { ok: false, reason: "attribution_unverifiable" };
  const clonePath = input.canonicalClonePath;
  const conflicting: RelationshipResult = { ok: false, reason: "conflicting_attribution" };
  const names = (value: string): boolean => value.includes(input.runId) || namesWholePath(value, clonePath);
  const pKey = recoveryCaptureKey(input.branch);
  const stopped = stoppedMarkerKey(input.runId);
  const descriptor = `uzi-retained.${input.runId}.journal`;
  let pKeyValues = 0;
  for (const { key, value, valueless } of input.entries) {
    if (key === pKey) {
      pKeyValues++;
      if (valueless || value !== input.journalRaw) return conflicting;
      continue;
    }
    if (key === stopped || key === descriptor) return conflicting;
    if (/^uzi-attempts\..+\.entry$/.test(key)) {
      const decoded = valueless ? undefined : parseAttemptLedgerEntry(value);
      if (decoded === undefined) { if (names(value)) return conflicting; continue; }
      if (decoded.runId === input.runId ? decoded.state === "live" || decoded.state === "reclaimed" : decoded.clonePath === clonePath) return conflicting;
      continue;
    }
    if (names(value)) return conflicting;
  }
  if (pKeyValues !== 1) return conflicting;
  return { ok: false, reason: "predecessor_not_plain", canonicalCandidate: true, clonePath };
}

/**
 * Plan 3.2, pure over one config snapshot. Module-private: the seeding transaction repeats it through {@link evaluateRestoreRoute}
 * over its own, fresh snapshot.
 *   shape          the journal is exactly `{runId, clonePath, attemptId}`: a `completionSourceFence` is `uncertain_publication`,
 *                  any other field `predecessor_not_plain` (an episode field, a memory park, a restore point, ...).
 *   relationships  the only attributions of this run are P's key and P's own `live` ledger entry. Refused as
 *                  `conflicting_attribution`: a protected descriptor, a stopped marker, any other key whose value names this run
 *                  or P's path, another same-run ledger entry that is `live` or `reclaimed` (a retained source) or non-retired at
 *                  or above P's generation. Older `retired`/`abandoned` entries of the run are history, not attribution.
 * A value this function cannot decode is judged by whether it NAMES the run or P's path (fail closed); the census decodes every
 * value strictly and refuses the rest as `attribution_unverifiable`.
 */
function classifyRestoreRelationships(input: RelationshipInput): RelationshipResult {
  const refuse = (reason: ClassificationRefusal): RelationshipResult => ({ ok: false, reason });
  let journal: unknown;
  try { journal = JSON.parse(input.journalRaw); } catch { return refuse("predecessor_not_plain"); }
  if (typeof journal !== "object" || journal === null || Array.isArray(journal)) return refuse("predecessor_not_plain");
  const j = journal as Record<string, unknown>;
  if (Object.hasOwn(j, "completionSourceFence")) return refuse("uncertain_publication");
  if (Object.keys(j).sort().join(",") !== "attemptId,clonePath,runId") return classifyCanonicalShape(input, j);
  if (typeof j.runId !== "string" || typeof j.clonePath !== "string" || typeof j.attemptId !== "string" || j.runId !== input.runId)
    return refuse("predecessor_not_plain");
  const clonePath = j.clonePath;
  const attemptId = j.attemptId;
  const generation = attemptGeneration(attemptId);
  // The pin is looked up by the attempt's generation: an attempt id that carries none cannot be restored from.
  if (generation === undefined) return refuse("predecessor_not_plain");
  // The seed only accepts a predecessor whose path is `<canonical>.attempt-<its own attemptId>` (a canonical-path P, or a path
  // carrying another id, is not restorable by design), so refuse here, before the executor swap, rather than at the seed.
  if (clonePath !== attemptClonePath(cloneKeyOf(clonePath).canonicalPath, attemptId)) return refuse("predecessor_path");
  if (!input.complete) return refuse("attribution_unverifiable");

  const names = (value: string): boolean => value.includes(input.runId) || namesWholePath(value, clonePath);
  const pKey = recoveryCaptureKey(input.branch);
  const stopped = stoppedMarkerKey(input.runId);
  const descriptor = `uzi-retained.${input.runId}.journal`;
  let pKeyValues = 0;
  const ledger = new Map<string, NonNullable<ReturnType<typeof parseAttemptLedgerEntry>>>();
  for (const entry of input.entries) {
    const { key, value } = entry;
    if (key === pKey) {
      pKeyValues++;
      if (entry.valueless || value !== input.journalRaw) return refuse("conflicting_attribution");
      continue;
    }
    if (key === stopped || key === descriptor) return refuse("conflicting_attribution");
    if (/^uzi-attempts\..+\.entry$/.test(key)) {
      const decoded = entry.valueless ? undefined : parseAttemptLedgerEntry(value);
      if (decoded === undefined) { if (names(value)) return refuse("conflicting_attribution"); continue; }
      if (decoded.runId === input.runId) ledger.set(decoded.attemptId, decoded); // last value per attempt wins
      else if (namesWholePath(decoded.clonePath, clonePath) && decoded.clonePath === clonePath) return refuse("conflicting_attribution");
      continue;
    }
    if (names(value)) return refuse("conflicting_attribution");
  }
  if (pKeyValues !== 1) return refuse("conflicting_attribution");
  const own = ledger.get(attemptId);
  if (own === undefined || own.state !== "live" || own.clonePath !== clonePath) return refuse("predecessor_ledger");
  for (const other of ledger.values()) {
    if (other.attemptId === attemptId) continue;
    if (other.state === "live" || other.state === "reclaimed") return refuse("conflicting_attribution");
    const g = attemptGeneration(other.attemptId);
    if ((g === undefined || g >= generation) && other.state !== "retired") return refuse("conflicting_attribution");
  }
  return { ok: true, clonePath, attemptId, generation };
}

// ---- the read-only eligibility pass (plan 3.4) ---------------------------------------------------------------------------

export interface EligibilityDeps {
  /** The incarnation this process's authority slots are written under. */
  selfIncarnation: string;
  processBoundary(): BoundaryRecord | undefined;
  processBoundaryDisabledReason(): string | undefined;
  /** `boundaries/<sha256(incarnation)>.json` as authenticated; no fallback. */
  readBoundary(incarnation: string): Promise<BoundaryRead>;
  /** S's live identity, re-read now. */
  readLive(): Promise<{ ok: true; identity: LiveBoundaryIdentity } | { ok: false; reason: IdentityRefusal }>;
  /** A read-only slot peek (see AuthorityRuntime.peekSlot). Throwing means unreadable. */
  peekSlot(clonePath: string): Promise<SlotSnapshot | null>;
  /** The authenticated recovery records of the run. */
  inspectRecords(runId: string): Promise<RecoveryRecord[]>;
  verifyPin(spec: RestorePinSpec): Promise<RestorePinVerdict>;
  /** The bare's attribution config under the bare lock; one call per pass. */
  readConfig(): Promise<{ entries: BareAttributionEntry[]; complete: boolean }>;
  /**
   * `assertBareClear`'s read-only equivalent: AuthorityRuntime's transaction-free `hooks.assertBareClear` (a peek of the bare's
   * own slot) refuses while a running pending footprint covers the bare. True means busy, and any failure to tell is busy too.
   */
  bareBusy(): Promise<boolean>;
  /** The bound bare's own authority slot (the same read-only peek); throwing means unreadable, which refuses as busy. */
  peekBareSlot(): Promise<SlotSnapshot | null>;
  /** `git merge-base --is-ancestor tip sha` in the bare only. */
  isAncestor(tip: string, sha: string): Promise<boolean>;
  /** TEST-ONLY: forwarded to evaluateComposeRestore's skipExecutionComparison. Production never sets it. */
  skipExecutionComparison?: boolean;
}

export interface EligibilityInput {
  runId: string;
  branch: string;
  /** Basename of the bound bare; a finalization pin must have been taken in this one. */
  bareDirName: string;
  claimDefaultBranch: string | undefined;
  checkpointTip: string | null | undefined;
  /** P's journal value exactly as discovery read it. */
  journalRaw: string;
  /** The 3.4(h) prerequisites of certified completion, each evaluated by the caller from the claim and the client. */
  certified: { guardedRunKind: boolean; inventoryGuarded: boolean; featureNegotiated: boolean; authorityPresent: boolean };
  /** The runner clone path of this run's canonical key; the only path a canonical-shaped journal may name (plan 3.2 item 1). */
  canonicalClonePath: string;
}

/** What the seeding transaction will re-derive and compare. Contains only identities, never a path of P's clone. */
export interface EligibleVerdict {
  clonePath: string;
  attemptId: string;
  generation: number;
  pin: {
    captureId: string; sourceSha: string; finalization: boolean; binding: Extract<RestorePinVerdict, { ok: true }>["binding"]; defaultBranch?: string;
    /** The selected record's identity as the authoritative pass re-authenticates it under J ({@link restorePinStillSelected}). */
    version: 1; state: string; bareDir?: string;
    /** A digest of every record of the generation (selected or not), so a record that appears, vanishes or settles is a difference. */
    recordsDigest: string;
  };
  predecessorSlot: { worker: string; epoch: string; state: string; seq: number };
  boundary: RestoreVerdict;
  selfCrossCheck: "ok" | "na";
  ancestry: "no_checkpoint" | "checkpoint_ancestor";
}

/** Whatever the pass learned before it stopped, for the refusal log. */
export interface RestoreKnown {
  slot?: BoundarySlot;
  crossCheck?: "ok" | "failed" | "na";
  verdict?: RestoreVerdict;
  pinGeneration?: number;
  sourceSha?: string;
  detail?: string;
  /** P's journal is the canonical `{runId, clonePath}` shape (the log carries it when the hosted class outranks it). */
  canonicalCandidate?: true;
}

export type RestoreRouteOutcome =
  | { eligible: true; verdict: EligibleVerdict; known: RestoreKnown }
  | { eligible: false; reason: RestoreRouteReason; known: RestoreKnown; unsupported?: RestoreUnsupported };

const PIN_EPISODE_FIELDS = ["heldPolicy", "holdSettlement", "finalRequest", "finalAcknowledged", "completionReceipt", "completionSource", "exactHoldBinding"] as const;

/** One digest over every authenticated record of generation `generation`: id, state, sha, kind of pin and which settlement-class
 *  fields are present. Two reads agree on it only when no record of the generation appeared, vanished, advanced or settled. */
function generationRecordsDigest(records: readonly RecoveryRecord[], generation: number): string {
  const rows = records.filter(r => r.generation === generation).map(r => JSON.stringify([
    r.captureId, r.version, r.state, r.sourceSha, r.finalizationPin === true, r.bareDir ?? null, r.coverageDigest ?? null,
    ...PIN_EPISODE_FIELDS.map(field => r[field] !== undefined)])).sort();
  return createHash("sha256").update(rows.join("\n")).digest("hex");
}

/**
 * Plan 3.4 step 3, a pure comparison run by the runner under J (the records come from `restoreRecordsUnderJournalLock`): the
 * ONE ordinary pin record of the generation is still the record the eligibility pass selected - same capture id, generation,
 * state, source sha, finalization flag, bare directory and version - none of the coverage, settlement, final or completion
 * fields appeared on it, and no other record of the generation appeared, vanished or changed. Anything else is `pin_changed`.
 */
export function restorePinStillSelected(records: readonly RecoveryRecord[], runId: string, branch: string, verdict: EligibleVerdict): boolean {
  const candidates = records.filter(r => r.generation === verdict.generation && !r.coverageDigest && classifyRecoveryOrigin(r.captureId) === "ordinary");
  if (candidates.length !== 1) return false;
  const r = candidates[0]!;
  const p = verdict.pin;
  return r.captureId === p.captureId && r.generation === verdict.generation && r.state === p.state && r.sourceSha === p.sourceSha &&
    (r.finalizationPin === true) === p.finalization && r.bareDir === p.bareDir && r.version === p.version &&
    r.runId === runId && r.branch === branch && r.coverageDigest === undefined && PIN_EPISODE_FIELDS.every(field => r[field] === undefined) &&
    generationRecordsDigest(records, verdict.generation) === p.recordsDigest;
}

/**
 * Plans 3.2 + 3.4, read-only. Order: shape and relationships (config), P's slot (still foreign-owned and not pending), then
 * (a) this process's boundary, (b) G, (c) exactly one pin record, (d) the pin refs and closure in the bare, (e) the census,
 * (f) the compose predicate, (g) the checkpoint ancestry, (h) certified-completion prerequisites. The first failure refuses.
 */
export async function evaluateRestoreRoute(deps: EligibilityDeps, input: EligibilityInput): Promise<RestoreRouteOutcome> {
  const known: RestoreKnown = {};
  const refuse = (reason: RestoreRouteReason, unsupported?: RestoreUnsupported): RestoreRouteOutcome =>
    ({ eligible: false, reason, known, ...(unsupported === undefined ? {} : { unsupported }) });
  const config = await deps.readConfig();
  const relationships = classifyRestoreRelationships({ runId: input.runId, branch: input.branch, journalRaw: input.journalRaw,
    entries: config.entries, complete: config.complete, canonicalClonePath: input.canonicalClonePath });
  if (!relationships.ok && relationships.canonicalCandidate !== true) return refuse(relationships.reason);
  const clonePath = relationships.clonePath;
  if (relationships.ok) known.pinGeneration = relationships.generation;
  else known.canonicalCandidate = true;

  let slot: SlotSnapshot | null;
  try { slot = await deps.peekSlot(clonePath); } catch { return refuse("slot_unreadable"); }
  // The route was taken for a foreign-owned slot (active, or with a pending footprint); anything else means it changed.
  if (slot === null || slot.owner.worker === deps.selfIncarnation || (slot.state !== "active" && slot.pending === null)) return refuse("predecessor_changed");
  if (slot.pending !== null) return refuse("predecessor_pending");
  if (!relationships.ok && slot.publication !== null) return refuse("predecessor_pending");

  // Plan 3.2 items 1-2: an unsupported class is decided here, before any pin check, from the quiet foreign-active slot and S's own
  // record. Hosted outranks canonical; P's own boundary record is never read for either.
  const live = await deps.readLive();
  const selfRead = await deps.readBoundary(deps.selfIncarnation);
  if (slot.publication === null) {
    const selfMatch = selfRecordMatchingLive(selfRead, live.ok ? live.identity : undefined, deps.selfIncarnation);
    if (selfMatch !== undefined) { known.slot = selfMatch.slot; known.crossCheck = selfMatch.crossCheck; }
    if (selfMatch?.slot.kind === "hosted") return refuse("hosted_restore_not_delivered", "hosted");
  }
  if (!relationships.ok) return refuse("predecessor_not_plain", "canonical");
  const { attemptId, generation } = relationships;

  // (a) this process's boundary record.
  const self = deps.processBoundary();
  if (self === undefined) {
    const disabled = deps.processBoundaryDisabledReason();
    known.detail = disabled;
    return refuse(disabled === "procfs_hidden" ? "procfs_hidden" : "self_record_invalid");
  }

  // (c) exactly one ordinary, guarded-or-not pin record of generation G.
  const allRecords = await deps.inspectRecords(input.runId);
  const candidates = allRecords
    .filter(r => r.generation === generation && !r.coverageDigest && classifyRecoveryOrigin(r.captureId) === "ordinary");
  if (candidates.length === 0) return refuse("no_eligible_pin");
  if (candidates.length > 1) return refuse("pin_ambiguous");
  const pin = candidates[0]!;
  known.sourceSha = pin.sourceSha;
  if (pin.version !== 1 || pin.runId !== input.runId || pin.branch !== input.branch || typeof pin.generation !== "number" ||
      !SHA40.test(pin.sourceSha) || PIN_EPISODE_FIELDS.some(field => pin[field] !== undefined)) return refuse("no_eligible_pin");
  const finalization = pin.finalizationPin === true;
  if (finalization && pin.bareDir !== input.bareDirName) return refuse("pin_bare_mismatch");
  const defaultBranch = pin.defaultBranch ?? input.claimDefaultBranch;

  // (d) the pin is bound in the bare and its closure verifies.
  const binding = await deps.verifyPin({ runId: input.runId, generation, sourceSha: pin.sourceSha, finalization, branch: input.branch,
    ...(defaultBranch === undefined ? {} : { defaultBranch }) });
  if (!binding.ok) return refuse(binding.reason);

  // (e) the complete attribution census over the same config snapshot.
  const census = await runAttributionCensus({ pairs: config.entries.map(e => ({ key: e.key, value: e.value })), complete: config.complete,
    selfIncarnation: deps.selfIncarnation, predecessorClonePath: clonePath, peekSlot: deps.peekSlot });
  if (!census.ok) return refuse(census.reason);

  // (f) the compose predicate.
  known.slot = self.slot;
  known.crossCheck = self.crossCheck;
  const bareBusy = await deps.bareBusy();
  let bareSlot: SlotSnapshot | null;
  try { bareSlot = await deps.peekBareSlot(); } catch { return refuse("bare_busy"); }
  const decision = await evaluateComposeRestore({
    selfIncarnation: deps.selfIncarnation, selfRead, selfLive: live.ok ? live.identity : undefined,
    ...(live.ok ? {} : { selfLiveRefusal: live.reason }),
    predecessor: slot, predecessorFenced: false, readBoundary: deps.readBoundary, bareBusy, bareSlot, census,
    ...(deps.skipExecutionComparison === true ? { skipExecutionComparison: true } : {}),
  });
  if (!decision.admit) return refuse(decision.reason, decision.unsupported);
  known.verdict = decision.verdict;

  // (g) the claim's checkpoint tip is an ancestor of the pin, in the bare only.
  let ancestry: EligibleVerdict["ancestry"] = "no_checkpoint";
  if (input.checkpointTip !== null && input.checkpointTip !== undefined) {
    if (!SHA40.test(input.checkpointTip) || !await deps.isAncestor(input.checkpointTip, pin.sourceSha)) return refuse("checkpoint_not_ancestor");
    ancestry = "checkpoint_ancestor";
  }

  // (h) certified completion must be available for the restored attempt.
  const c = input.certified;
  if (!(c.guardedRunKind && c.inventoryGuarded && c.featureNegotiated && c.authorityPresent)) return refuse("certified_completion_unavailable");

  return { eligible: true, known, verdict: {
    clonePath, attemptId, generation,
    pin: { captureId: pin.captureId, sourceSha: pin.sourceSha, finalization, binding: binding.binding, ...(defaultBranch === undefined ? {} : { defaultBranch }),
      version: 1, state: pin.state, ...(pin.bareDir === undefined ? {} : { bareDir: pin.bareDir }), recordsDigest: generationRecordsDigest(allRecords, generation) },
    predecessorSlot: { worker: slot.owner.worker, epoch: slot.epoch, state: slot.state, seq: slot.seq },
    boundary: decision.verdict, selfCrossCheck: self.crossCheck === "ok" ? "ok" : "na", ancestry,
  } };
}
