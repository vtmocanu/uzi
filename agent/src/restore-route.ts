// Issue #3644 Gate B (m4a): the restore route's classification and read-only eligibility pass.
//
// After a worker was killed with its source active, the restarted worker's claim finds a predecessor P whose authority slot is
// still active under the dead incarnation. The legacy retained path would reserve an iteration (rewriting P's journal) and then
// be refused by source-use admission. The restore route instead decides, from evidence that never involves P's clone, whether
// an independent restore from the verified pin is possible. NOTHING here reads P's contents or tree, walks its path, or runs
// Git, capture, normalization, reaping or quiescence on it: the only touches of the path are the authority store's own slot-key
// derivation inside `peekSlot`, and the bare's config and objects.
//
// This milestone only DECIDES. The runner turns a refusal into a nonterminal stop that writes nothing, and an eligible verdict
// into a nonterminal stop too (seeding is not enabled in this build); the verdict object is what the seeding transaction will
// re-derive and compare.
import { attemptGeneration } from "./attempt-path.js";
import {
  parseAttemptLedgerEntry, recoveryCaptureKey, stoppedMarkerKey,
  type BareAttributionEntry, type RestorePinSpec, type RestorePinVerdict,
} from "./git.js";
import type { SlotSnapshot } from "./completion-authority.js";
import type { RecoveryRecord } from "./recovery.js";
import { classifyRecoveryOrigin } from "./recovery-origin.js";
import {
  evaluateComposeRestore, runAttributionCensus,
  type BoundaryRead, type BoundaryRecord, type BoundarySlot, type IdentityRefusal, type LiveBoundaryIdentity, type RestoreRefusal,
  type RestoreVerdict,
} from "./restore-boundary.js";

/** Refusals decided from the bare config alone, before any reservation (plan 3.2). */
export type ClassificationRefusal =
  | "uncertain_publication" | "predecessor_not_plain" | "conflicting_attribution" | "predecessor_ledger" | "attribution_unverifiable";

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
}

export type RelationshipResult =
  | { ok: true; clonePath: string; attemptId: string; generation: number }
  | { ok: false; reason: ClassificationRefusal };

/**
 * Plan 3.2, pure over one config snapshot. Module-private until the seeding transaction (m4b) repeats it over its own snapshot,
 * when it is exported.
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
  if (Object.keys(j).sort().join(",") !== "attemptId,clonePath,runId") return refuse("predecessor_not_plain");
  if (typeof j.runId !== "string" || typeof j.clonePath !== "string" || typeof j.attemptId !== "string" || j.runId !== input.runId)
    return refuse("predecessor_not_plain");
  const clonePath = j.clonePath;
  const attemptId = j.attemptId;
  const generation = attemptGeneration(attemptId);
  // The pin is looked up by the attempt's generation: an attempt id that carries none cannot be restored from.
  if (generation === undefined) return refuse("predecessor_not_plain");
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
}

/** What the seeding transaction will re-derive and compare. Contains only identities, never a path of P's clone. */
export interface EligibleVerdict {
  clonePath: string;
  attemptId: string;
  generation: number;
  pin: { captureId: string; sourceSha: string; finalization: boolean; binding: Extract<RestorePinVerdict, { ok: true }>["binding"]; defaultBranch?: string };
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
}

export type RestoreRouteOutcome =
  | { eligible: true; verdict: EligibleVerdict; known: RestoreKnown }
  | { eligible: false; reason: RestoreRouteReason; known: RestoreKnown };

const PIN_EPISODE_FIELDS = ["heldPolicy", "holdSettlement", "finalRequest", "finalAcknowledged", "completionReceipt", "completionSource", "exactHoldBinding"] as const;

/**
 * Plans 3.2 + 3.4, read-only. Order: shape and relationships (config), P's slot (still foreign-owned and not pending), then
 * (a) this process's boundary, (b) G, (c) exactly one pin record, (d) the pin refs and closure in the bare, (e) the census,
 * (f) the compose predicate, (g) the checkpoint ancestry, (h) certified-completion prerequisites. The first failure refuses.
 */
export async function evaluateRestoreRoute(deps: EligibilityDeps, input: EligibilityInput): Promise<RestoreRouteOutcome> {
  const known: RestoreKnown = {};
  const refuse = (reason: RestoreRouteReason): RestoreRouteOutcome => ({ eligible: false, reason, known });
  const config = await deps.readConfig();
  const relationships = classifyRestoreRelationships({ runId: input.runId, branch: input.branch, journalRaw: input.journalRaw,
    entries: config.entries, complete: config.complete });
  if (!relationships.ok) return refuse(relationships.reason);
  const { clonePath, attemptId, generation } = relationships;
  known.pinGeneration = generation;

  let slot: SlotSnapshot | null;
  try { slot = await deps.peekSlot(clonePath); } catch { return refuse("slot_unreadable"); }
  // The route was taken for a foreign-owned slot (active, or with a pending footprint); anything else means it changed.
  if (slot === null || slot.owner.worker === deps.selfIncarnation || (slot.state !== "active" && slot.pending === null)) return refuse("predecessor_changed");
  if (slot.pending !== null) return refuse("predecessor_pending");

  // (a) this process's boundary record.
  const self = deps.processBoundary();
  if (self === undefined) {
    const disabled = deps.processBoundaryDisabledReason();
    known.detail = disabled;
    return refuse(disabled === "procfs_hidden" ? "procfs_hidden" : "self_record_invalid");
  }

  // (c) exactly one ordinary, guarded-or-not pin record of generation G.
  const candidates = (await deps.inspectRecords(input.runId))
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
  const live = await deps.readLive();
  const selfRead = await deps.readBoundary(deps.selfIncarnation);
  known.slot = self.slot;
  known.crossCheck = self.crossCheck;
  const bareBusy = await deps.bareBusy();
  let bareSlot: SlotSnapshot | null;
  try { bareSlot = await deps.peekBareSlot(); } catch { return refuse("bare_busy"); }
  const decision = await evaluateComposeRestore({
    selfIncarnation: deps.selfIncarnation, selfRead, selfLive: live.ok ? live.identity : undefined,
    ...(live.ok ? {} : { selfLiveRefusal: live.reason }),
    predecessor: slot, predecessorFenced: false, readBoundary: deps.readBoundary, bareBusy, bareSlot, census,
  });
  if (!decision.admit) return refuse(decision.reason);
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
    pin: { captureId: pin.captureId, sourceSha: pin.sourceSha, finalization, binding: binding.binding, ...(defaultBranch === undefined ? {} : { defaultBranch }) },
    predecessorSlot: { worker: slot.owner.worker, epoch: slot.epoch, state: slot.state, seq: slot.seq },
    boundary: decision.verdict, selfCrossCheck: self.crossCheck === "ok" ? "ok" : "na", ancestry,
  } };
}
