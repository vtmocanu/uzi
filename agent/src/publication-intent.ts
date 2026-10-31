/**
 * Issue #1324 (agent half): the write-ahead PUBLICATION INTENT.
 *
 * Before a run's first external publication attempt (push / merge request), the worker records
 * what it is about to publish: a durable, MAC'd local record in the outbox
 * (`publication-<G>.json`, see Outbox.savePublicationRecord) AND an enrollment on the api
 * (`POST /api/worker/runs/{id}/publication-intent`). A forge outage during that publication then
 * parks the run publication-only instead of failing it, and a resumed flight finishes from the
 * recorded intent rather than redoing the agent's work.
 *
 * This module owns three things and performs no file IO of its own:
 *   1. the canonical intent shape, shared by the local record and the wire (a strict parser);
 *   2. the typed outcomes of enrolling and advancing an intent, so the runner switches on a
 *      discriminated union instead of guessing; and
 *   3. the orchestration order (local record first, then the api, then the ack recorded locally).
 *
 * The one rule behind every outcome: NEVER GUESS. A failure is classified into exactly one of
 *   - `not_enrolled`: provably nothing was written and nothing was sent, so ordinary finalize may
 *     run (with a `publication_resume_unavailable` diagnostic); or
 *   - `uncertain`: the intent may exist (locally or on the api). The caller makes NO external
 *     attempt; a bounded retry or the next register reconciliation decides.
 * Losing or failing to read a local record never implies the enrollment was retired. A local
 * record is discarded only after the api positively answers `publication_enrollment_retired` for
 * that very enrollment_seq.
 */
import { validFrozenHeldPolicy, type FrozenHeldPolicy } from "./frozen-held.js";

/** The intent document version. A reader of any other version refuses it (mismatch). */
const PUBLICATION_INTENT_VERSION = 1;

/** The api's cap on the intent body (`MaxPublicationIntentBytes`); the same bound the table CHECK uses. */
export const MAX_PUBLICATION_INTENT_BYTES = 64 * 1024;

/** The local file additionally carries enrollment_seq, candidate_seq and the MAC. */
export const MAX_PUBLICATION_RECORD_BYTES = MAX_PUBLICATION_INTENT_BYTES + 1024;

/** Refusal reasons the typed publication park report can draw (api `forgeParkRefusalReason`). */
export type PublicationParkRefusal = "publication_enrolled" | "publication_not_enrolled";

/**
 * Typed mismatch between what a record or claim carries and what this worker can honour: an
 * unknown version, an unknown or invalid guard state, an unknown member. The runner turns it into
 * a `publication_intent_mismatch` hold. It is never swallowed and never lets a guard be skipped.
 */
export class PublicationIntentMismatchError extends Error {
  readonly code = "publication_intent_mismatch";
  constructor(detail: string) {
    super(`publication_intent_mismatch: ${detail}`);
    this.name = "PublicationIntentMismatchError";
  }
}

export type PublicationFloorRecord = { readonly kind: "absent" } | { readonly kind: "pinned"; readonly oid: string };

/** A secret finding as the remediation guard remembers it (repo-relative file, never a local path). */
export interface PublicationSecretFinding {
  file: string;
  start_line: number;
  commit: string;
  rule_id: string;
}

export interface PublicationSecretRemediation {
  attempts: number;
  known: PublicationSecretFinding[];
  flagged_commits: string[];
  flagged_findings: PublicationSecretFinding[];
  blocked: PublicationSecretFinding[];
  clean_tip?: string;
  ever_known: boolean;
  withhold_paths: boolean;
  overflow: boolean;
}

/** Every guard input the resumed publication must re-apply. Nothing here is a credential or a path. */
export interface PublicationGuardState {
  frozen_held: FrozenHeldPolicy;
  secret_remediation: PublicationSecretRemediation;
  published_tip?: string;
  published_real_tips: string[];
  landed_checkpoint: boolean;
  ci_fix_human_approved: boolean;
  ci_config_guard_globs: string[] | null;
  last_checkpoint_ref_tip?: string;
}

export interface PublicationSource {
  pin_generation: number;
  checkpoint_key: string | null;
}

export interface PublicationMrMetadata {
  title: string;
  /** The lead's PR summary. */
  summary: string;
  closing: string[];
  contract_revision: number;
}

/** A compiled MR description staged for the candidate; valid only for exactly {candidate, target, contract_revision}. */
export interface PublicationStagedDescription {
  candidate: string;
  target: string;
  contract_revision: number;
  body: string;
}

/** The canonical intent: what the api stores verbatim and what the local record carries. */
export interface PublicationIntentBody {
  v: 1;
  run_id: string;
  repo_id: string;
  branch: string;
  claim_generation: number;
  original_head: string;
  floor: PublicationFloorRecord;
  candidate: string | null;
  source: PublicationSource;
  mr: PublicationMrMetadata;
  guard: PublicationGuardState;
  staged_description?: PublicationStagedDescription;
}

/** The local record: the intent plus the api-assigned sequence numbers (absent until the api ACKed). */
export interface PublicationRecord extends PublicationIntentBody {
  enrollment_seq?: number;
  candidate_seq?: number;
}

/** A record the api acknowledged: both sequence numbers are known. */
export type EnrolledPublicationRecord = PublicationRecord & { enrollment_seq: number; candidate_seq: number };

// ── strict parsing ───────────────────────────────────────────────────────────

const SHA_RE = /^[0-9a-f]{40}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const TASK_KEY_RE = /^uzi\/task\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

const MAX_FINDINGS = 500;
const MAX_SHAS = 256;
const MAX_GLOBS = 256;
const MAX_GLOB_CHARS = 512;
const MAX_TITLE_CHARS = 1024;
const MAX_SUMMARY_CHARS = 16 * 1024;
const MAX_DESCRIPTION_CHARS = 32 * 1024;
const MAX_CLOSING = 64;
const MAX_CLOSING_CHARS = 256;
const MAX_TEXT_CHARS = 1024;

function bad(where: string): never {
  throw new PublicationIntentMismatchError(where);
}

function object(value: unknown, where: string, allowed: readonly string[], required: readonly string[]): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) bad(`${where} is not an object`);
  const out = value as Record<string, unknown>;
  for (const key of Object.keys(out)) if (!allowed.includes(key)) bad(`${where}.${key} is not a known member`);
  for (const key of required) if (!(key in out)) bad(`${where}.${key} is missing`);
  return out;
}

function sha(value: unknown, where: string): string {
  if (typeof value !== "string" || !SHA_RE.test(value)) bad(`${where} is not a 40-hex object id`);
  return value;
}

function text(value: unknown, where: string, max: number, allowEmpty = false): string {
  if (typeof value !== "string" || (!allowEmpty && value.length === 0) || value.length > max) bad(`${where} is not a bounded string`);
  return value;
}

function integer(value: unknown, where: string, min: number): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < min) bad(`${where} is not an integer >= ${min}`);
  return value;
}

function flag(value: unknown, where: string): boolean {
  if (typeof value !== "boolean") bad(`${where} is not a boolean`);
  return value;
}

function list<T>(value: unknown, where: string, max: number, item: (v: unknown, w: string) => T): T[] {
  if (!Array.isArray(value) || value.length > max) bad(`${where} is not a bounded list`);
  return value.map((v, i) => item(v, `${where}[${i}]`));
}

function uuid(value: unknown, where: string): string {
  if (typeof value !== "string" || !UUID_RE.test(value)) bad(`${where} is not a canonical uuid`);
  return value;
}

/** Mirrors the api's validBranchName: non-empty, <= 255 chars, no control or format characters. */
function branchName(value: unknown, where: string): string {
  const s = text(value, where, 255);
  if (/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(s)) bad(`${where} has control or format characters`);
  return s;
}

function finding(value: unknown, where: string): PublicationSecretFinding {
  const o = object(value, where, ["file", "start_line", "commit", "rule_id"], ["file", "start_line", "commit", "rule_id"]);
  return {
    file: text(o.file, `${where}.file`, MAX_TEXT_CHARS, true),
    start_line: integer(o.start_line, `${where}.start_line`, 0),
    commit: text(o.commit, `${where}.commit`, 64, true),
    rule_id: text(o.rule_id, `${where}.rule_id`, 256, true),
  };
}

function secretRemediation(value: unknown, where: string): PublicationSecretRemediation {
  const keys = ["attempts", "known", "flagged_commits", "flagged_findings", "blocked", "clean_tip", "ever_known", "withhold_paths", "overflow"];
  const o = object(value, where, keys, keys.filter((k) => k !== "clean_tip"));
  return {
    attempts: integer(o.attempts, `${where}.attempts`, 0),
    known: list(o.known, `${where}.known`, MAX_FINDINGS, finding),
    flagged_commits: list(o.flagged_commits, `${where}.flagged_commits`, MAX_SHAS, sha),
    flagged_findings: list(o.flagged_findings, `${where}.flagged_findings`, MAX_FINDINGS, finding),
    blocked: list(o.blocked, `${where}.blocked`, MAX_FINDINGS, finding),
    ...(o.clean_tip !== undefined ? { clean_tip: sha(o.clean_tip, `${where}.clean_tip`) } : {}),
    ever_known: flag(o.ever_known, `${where}.ever_known`),
    withhold_paths: flag(o.withhold_paths, `${where}.withhold_paths`),
    overflow: flag(o.overflow, `${where}.overflow`),
  };
}

const FROZEN_HELD_KEYS = ["version", "blocked", "everKnown", "cleanTip", "flaggedCommits", "ciGlobs", "confirmedTip", "checkpointKey", "extraFloors"];

function frozenHeld(value: unknown, where: string): FrozenHeldPolicy {
  const o = object(value, where, FROZEN_HELD_KEYS, ["version", "blocked", "everKnown", "flaggedCommits", "ciGlobs", "extraFloors"]);
  if (!validFrozenHeldPolicy(o)) bad(`${where} is not a valid frozen held policy`);
  if (o.extraFloors.length > MAX_SHAS || (o.ciGlobs !== null && (o.ciGlobs.length > MAX_GLOBS || o.ciGlobs.some((g) => g.length > MAX_GLOB_CHARS)))) {
    bad(`${where} exceeds its bounds`);
  }
  // Copy only the validated members, so no unvalidated property rides along.
  return {
    version: o.version, blocked: o.blocked, everKnown: o.everKnown,
    ...(o.cleanTip !== undefined ? { cleanTip: o.cleanTip } : {}),
    flaggedCommits: [...o.flaggedCommits],
    ciGlobs: o.ciGlobs === null ? null : [...o.ciGlobs],
    ...(o.confirmedTip !== undefined ? { confirmedTip: o.confirmedTip } : {}),
    ...(o.checkpointKey !== undefined ? { checkpointKey: o.checkpointKey } : {}),
    extraFloors: [...o.extraFloors],
  };
}

function guardState(value: unknown, where: string): PublicationGuardState {
  const keys = ["frozen_held", "secret_remediation", "published_tip", "published_real_tips", "landed_checkpoint",
    "ci_fix_human_approved", "ci_config_guard_globs", "last_checkpoint_ref_tip"];
  const o = object(value, where, keys, ["frozen_held", "secret_remediation", "published_real_tips", "landed_checkpoint",
    "ci_fix_human_approved", "ci_config_guard_globs"]);
  const globs = o.ci_config_guard_globs === null ? null
    : list(o.ci_config_guard_globs, `${where}.ci_config_guard_globs`, MAX_GLOBS, (g, w) => text(g, w, MAX_GLOB_CHARS));
  return {
    frozen_held: frozenHeld(o.frozen_held, `${where}.frozen_held`),
    secret_remediation: secretRemediation(o.secret_remediation, `${where}.secret_remediation`),
    ...(o.published_tip !== undefined ? { published_tip: sha(o.published_tip, `${where}.published_tip`) } : {}),
    published_real_tips: list(o.published_real_tips, `${where}.published_real_tips`, MAX_SHAS, sha),
    landed_checkpoint: flag(o.landed_checkpoint, `${where}.landed_checkpoint`),
    ci_fix_human_approved: flag(o.ci_fix_human_approved, `${where}.ci_fix_human_approved`),
    ci_config_guard_globs: globs,
    ...(o.last_checkpoint_ref_tip !== undefined ? { last_checkpoint_ref_tip: sha(o.last_checkpoint_ref_tip, `${where}.last_checkpoint_ref_tip`) } : {}),
  };
}

function floorRecord(value: unknown, where: string): PublicationFloorRecord {
  if (typeof value === "object" && value !== null && (value as { kind?: unknown }).kind === "absent") {
    object(value, where, ["kind"], ["kind"]);
    return { kind: "absent" };
  }
  const o = object(value, where, ["kind", "oid"], ["kind", "oid"]);
  if (o.kind !== "pinned") bad(`${where}.kind is not absent or pinned`);
  return { kind: "pinned", oid: sha(o.oid, `${where}.oid`) };
}

function mrMetadata(value: unknown, where: string): PublicationMrMetadata {
  const keys = ["title", "summary", "closing", "contract_revision"];
  const o = object(value, where, keys, keys);
  return {
    title: text(o.title, `${where}.title`, MAX_TITLE_CHARS, true),
    summary: text(o.summary, `${where}.summary`, MAX_SUMMARY_CHARS, true),
    closing: list(o.closing, `${where}.closing`, MAX_CLOSING, (c, w) => text(c, w, MAX_CLOSING_CHARS)),
    contract_revision: integer(o.contract_revision, `${where}.contract_revision`, 0),
  };
}

function stagedDescription(value: unknown, where: string): PublicationStagedDescription {
  const keys = ["candidate", "target", "contract_revision", "body"];
  const o = object(value, where, keys, keys);
  return {
    candidate: sha(o.candidate, `${where}.candidate`),
    target: branchName(o.target, `${where}.target`),
    contract_revision: integer(o.contract_revision, `${where}.contract_revision`, 0),
    body: text(o.body, `${where}.body`, MAX_DESCRIPTION_CHARS, true),
  };
}

const BODY_KEYS = ["v", "run_id", "repo_id", "branch", "claim_generation", "original_head", "floor", "candidate", "source", "mr", "guard", "staged_description"];
const BODY_REQUIRED = BODY_KEYS.filter((k) => k !== "staged_description");

function parseBody(o: Record<string, unknown>): PublicationIntentBody {
  if (o.v !== PUBLICATION_INTENT_VERSION) bad(`unsupported version ${JSON.stringify(o.v)}`);
  const source = object(o.source, "source", ["pin_generation", "checkpoint_key"], ["pin_generation", "checkpoint_key"]);
  const checkpointKey = source.checkpoint_key;
  if (checkpointKey !== null && (typeof checkpointKey !== "string" || !TASK_KEY_RE.test(checkpointKey))) bad("source.checkpoint_key is invalid");
  const candidate = o.candidate === null ? null : sha(o.candidate, "candidate");
  const body: PublicationIntentBody = {
    v: 1,
    run_id: uuid(o.run_id, "run_id"),
    repo_id: uuid(o.repo_id, "repo_id"),
    branch: branchName(o.branch, "branch"),
    claim_generation: integer(o.claim_generation, "claim_generation", 1),
    original_head: sha(o.original_head, "original_head"),
    floor: floorRecord(o.floor, "floor"),
    candidate,
    source: { pin_generation: integer(source.pin_generation, "source.pin_generation", 0), checkpoint_key: checkpointKey },
    mr: mrMetadata(o.mr, "mr"),
    guard: guardState(o.guard, "guard"),
    ...(o.staged_description !== undefined ? { staged_description: stagedDescription(o.staged_description, "staged_description") } : {}),
  };
  // The staged description is valid only for the exact candidate and contract revision it was compiled for.
  const staged = body.staged_description;
  if (staged && (body.candidate === null || staged.candidate !== body.candidate || staged.contract_revision !== body.mr.contract_revision)) {
    bad("staged_description is not bound to the recorded candidate and contract revision");
  }
  return body;
}

/** Strictly parse an intent (a claim's `publication_resume.intent`, or a record body). Throws a mismatch. */
export function parsePublicationIntent(raw: unknown): PublicationIntentBody {
  return parseBody(object(raw, "intent", BODY_KEYS, BODY_REQUIRED));
}

/** Strictly parse a local record: the intent plus both sequence numbers or neither. */
export function parsePublicationRecord(raw: unknown): PublicationRecord {
  const o = object(raw, "record", [...BODY_KEYS, "enrollment_seq", "candidate_seq"], BODY_REQUIRED);
  const body = parseBody(Object.fromEntries(Object.entries(o).filter(([k]) => k !== "enrollment_seq" && k !== "candidate_seq")));
  if ((o.enrollment_seq === undefined) !== (o.candidate_seq === undefined)) bad("enrollment_seq and candidate_seq must be set together");
  if (o.enrollment_seq === undefined) return body;
  return { ...body, enrollment_seq: integer(o.enrollment_seq, "enrollment_seq", 1), candidate_seq: integer(o.candidate_seq, "candidate_seq", 1) };
}

/** The claim's `publication_resume` member (api `PublicationResumePayload`). */
export interface ClaimPublicationResume {
  enrollment_seq: number;
  candidate_seq: number;
  intent: unknown;
  handoff?: unknown;
}

/** Validate a claim's `publication_resume` into a typed record. Throws a mismatch. */
export function parseClaimPublicationResume(claim: ClaimPublicationResume): EnrolledPublicationRecord {
  const body = parsePublicationIntent(claim.intent);
  return { ...body, enrollment_seq: integer(claim.enrollment_seq, "enrollment_seq", 1), candidate_seq: integer(claim.candidate_seq, "candidate_seq", 1) };
}

/** The wire intent: the canonical body only. Sequence numbers travel in the request envelope. */
function wireIntent(record: PublicationRecord): PublicationIntentBody {
  const { enrollment_seq: _e, candidate_seq: _c, ...body } = record;
  return parsePublicationIntent(body);
}

/** Serialized wire intent size in bytes; the api refuses more than {@link MAX_PUBLICATION_INTENT_BYTES}. */
export function publicationIntentBytes(record: PublicationRecord): number {
  return Buffer.byteLength(JSON.stringify(wireIntent(record)), "utf8");
}

/** Same enrollment identity (everything but the candidate and its content), the api's `sameIdentity`. */
export function samePublicationIdentity(a: PublicationIntentBody, b: PublicationIntentBody): boolean {
  return a.run_id === b.run_id && a.repo_id === b.repo_id && a.branch === b.branch &&
    a.claim_generation === b.claim_generation && a.original_head === b.original_head &&
    a.floor.kind === b.floor.kind && (a.floor.kind === "pinned" ? a.floor.oid === (b.floor as { oid?: string }).oid : true);
}

/** How a local record relates to the claim's active enrollment. The claim is the api's single active row. */
export type PublicationClaimRelation =
  | { kind: "no_active_enrollment" }
  | { kind: "in_sync" }
  | { kind: "local_ahead" }
  | { kind: "server_ahead" }
  | { kind: "superseded" };

/**
 * Compare a local record with the claim's active enrollment. `superseded` and
 * `no_active_enrollment` never authorize re-enrolling the old identity or overriding the active
 * candidate; the record is kept until the api positively confirms its enrollment retired.
 */
export function relatePublicationToClaim(record: PublicationRecord, active: ClaimPublicationResume | undefined): PublicationClaimRelation {
  if (!active) return { kind: "no_active_enrollment" };
  if (record.enrollment_seq === undefined || record.candidate_seq === undefined || active.enrollment_seq !== record.enrollment_seq) {
    return { kind: "superseded" };
  }
  if (active.candidate_seq === record.candidate_seq) return { kind: "in_sync" };
  return active.candidate_seq > record.candidate_seq ? { kind: "server_ahead" } : { kind: "local_ahead" };
}

// ── the api seam ─────────────────────────────────────────────────────────────

/** The request body of `POST /api/worker/runs/{id}/publication-intent` (api `PublicationIntentRequest`). */
export interface PublicationIntentRequest {
  claim_generation: number;
  enrollment_seq?: number;
  candidate_seq?: number;
  intent: PublicationIntentBody;
}

/** What one publication-intent POST can come to. Only `acked` and `refused` are definite answers. */
export type PublicationIntentResult =
  | { kind: "acked"; enrollment_seq: number; candidate_seq: number; state: string }
  /** A definite api answer that stored nothing: the status and the machine-readable reason. */
  | { kind: "refused"; status: number; reason: string }
  /** The feature was not negotiated; nothing was sent. */
  | { kind: "unsupported" }
  /** Transport failure, timeout, a 5xx or an unreadable ack AFTER the request may have been applied. */
  | { kind: "unknown"; detail: string };

export interface PublicationApi {
  registerPublicationIntent(runId: string, request: PublicationIntentRequest, signal?: AbortSignal): Promise<PublicationIntentResult>;
}

// ── the local store seam (implemented by Outbox) ─────────────────────────────

export type PublicationWriteFailure = "outbox_disabled" | "intent_too_large" | "intent_invalid" | "enospc" | "io_error" | "conflict" | "cas_mismatch" | "absent";

/** `unwritten`: this call provably changed nothing. `unverified`: a file may exist or have changed. */
export type PublicationWriteResult =
  | { ok: true; record: PublicationRecord; adopted: boolean }
  | { ok: false; reason: PublicationWriteFailure; state: "unwritten" | "unverified"; detail?: string };

export interface PublicationStore {
  /** Install the record exclusively (no replace). An existing record is adopted and returned. */
  savePublicationRecord(record: PublicationRecord): Promise<PublicationWriteResult>;
  /** Crash-safe compare-and-swap replace: the old record stays valid until the new one is durable. */
  replacePublicationRecord(next: PublicationRecord, expected: { enrollment_seq?: number; candidate_seq?: number }): Promise<PublicationWriteResult>;
  /** Remove the record. The caller calls it only after the api positively confirmed retirement. */
  discardPublicationRecord(runId: string, claimGeneration: number, expected: { enrollment_seq?: number }): Promise<boolean>;
}

// ── outcomes ─────────────────────────────────────────────────────────────────

export const PUBLICATION_RESUME_UNAVAILABLE = "publication_resume_unavailable";

export type PublicationNotEnrolledReason =
  | "outbox_disabled" | "intent_too_large" | "intent_invalid" | "enospc" | "local_write_failed"
  | "server_feature_unavailable" | "server_declined";

export type PublicationUncertainReason =
  /** A file may exist (partial link or fsync), or its absence could not be verified. */
  | "local_write_unverified"
  /** A different record already occupies this run and generation. */
  | "existing_record_conflict"
  /** The record is already acknowledged: an existing enrollment, resume it, do not publish fresh. */
  | "already_enrolled"
  /** The request outcome is unknown after bounded idempotent retries. */
  | "ack_unknown"
  /** The api disagrees: an identity or candidate_seq mismatch, a stale claim, or a retired enrollment at enroll time. */
  | "server_disagrees"
  /** The api acknowledged, but the local record could not carry the sequence numbers. */
  | "ack_not_recorded"
  /** A candidate update could not be saved locally; the previous record is untouched. */
  | "candidate_save_failed"
  | "aborted";

export type PublicationEnrollOutcome =
  | { kind: "enrolled"; record: EnrolledPublicationRecord }
  | { kind: "not_enrolled"; reason: PublicationNotEnrolledReason; diagnostic: typeof PUBLICATION_RESUME_UNAVAILABLE; detail?: string }
  | { kind: "uncertain"; reason: PublicationUncertainReason; record?: PublicationRecord; detail?: string };

export type PublicationAdvanceOutcome =
  | PublicationEnrollOutcome
  /** The api answered `publication_enrollment_retired` for this enrollment_seq. `discarded` says whether the local record was removed. */
  | { kind: "retired"; discarded: boolean };

export interface PublicationDeps {
  store: PublicationStore;
  api: PublicationApi;
  signal?: AbortSignal;
  /** Delays before each idempotent attempt; its length is the attempt bound. Default {@link DEFAULT_RETRY_DELAYS_MS}. */
  retryDelaysMs?: readonly number[];
  sleep?: (ms: number) => Promise<void>;
}

/** Three attempts in all: immediately, then after 200 ms and 1 s. Bounded, with no unbounded loop. */
const DEFAULT_RETRY_DELAYS_MS: readonly number[] = [0, 200, 1000];

const DECLINED_REASONS = new Set(["publication_resume_disabled", "publication_resume_unsupported", "publication_kind_unsupported"]);

const notEnrolled = (reason: PublicationNotEnrolledReason, detail?: string): PublicationEnrollOutcome => ({
  kind: "not_enrolled", reason, diagnostic: PUBLICATION_RESUME_UNAVAILABLE, ...(detail !== undefined ? { detail } : {}),
});

const uncertain = (reason: PublicationUncertainReason, record?: PublicationRecord, detail?: string): PublicationEnrollOutcome => ({
  kind: "uncertain", reason, ...(record ? { record } : {}), ...(detail !== undefined ? { detail } : {}),
});

/** POST the request up to the attempt bound; each attempt is the identical idempotent request. */
async function postBounded(deps: PublicationDeps, runId: string, request: PublicationIntentRequest): Promise<PublicationIntentResult> {
  const delays = deps.retryDelaysMs ?? DEFAULT_RETRY_DELAYS_MS;
  const sleep = deps.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  let last: PublicationIntentResult = { kind: "unknown", detail: "no attempt made" };
  for (const delay of delays) {
    if (deps.signal?.aborted) return { kind: "unknown", detail: "aborted" };
    if (delay > 0) await sleep(delay);
    if (deps.signal?.aborted) return { kind: "unknown", detail: "aborted" };
    last = await deps.api.registerPublicationIntent(runId, request, deps.signal);
    if (last.kind !== "unknown") return last;
  }
  return last;
}

function requestFor(record: PublicationRecord, update: boolean): PublicationIntentRequest {
  return {
    claim_generation: record.claim_generation,
    ...(update ? { enrollment_seq: record.enrollment_seq, candidate_seq: record.candidate_seq } : {}),
    intent: wireIntent(record),
  };
}

function asEnrolled(record: PublicationRecord, ack: { enrollment_seq: number; candidate_seq: number }): EnrolledPublicationRecord {
  return { ...record, enrollment_seq: ack.enrollment_seq, candidate_seq: ack.candidate_seq };
}

/**
 * Enroll a new publication intent: local record first, then the api, then the ack recorded locally.
 * Never throws for an operational failure; every outcome is one of `enrolled`, `not_enrolled`
 * (ordinary finalize may run, carrying the `publication_resume_unavailable` diagnostic) or `uncertain`
 * (make no external attempt). A malformed `intent` is `not_enrolled` / `intent_invalid` (nothing is written).
 */
export async function enrollPublicationIntent(deps: PublicationDeps, intent: PublicationIntentBody): Promise<PublicationEnrollOutcome> {
  let record: PublicationRecord;
  try {
    record = parsePublicationIntent(intent);
    if (publicationIntentBytes(record) > MAX_PUBLICATION_INTENT_BYTES) return notEnrolled("intent_too_large");
  } catch (err) {
    return notEnrolled("intent_invalid", err instanceof Error ? err.message : String(err));
  }
  const saved = await deps.store.savePublicationRecord(record);
  if (!saved.ok) {
    if (saved.reason === "conflict") return uncertain("existing_record_conflict", undefined, saved.detail);
    if (saved.state === "unverified") return uncertain("local_write_unverified", undefined, saved.detail ?? saved.reason);
    switch (saved.reason) {
      case "outbox_disabled": return notEnrolled("outbox_disabled");
      case "intent_too_large": return notEnrolled("intent_too_large");
      case "intent_invalid": return notEnrolled("intent_invalid", saved.detail);
      case "enospc": return notEnrolled("enospc");
      default: return notEnrolled("local_write_failed", saved.detail ?? saved.reason);
    }
  }
  const held = saved.record;
  if (!samePublicationIdentity(held, record)) return uncertain("existing_record_conflict", held);
  if (held.enrollment_seq !== undefined) return uncertain("already_enrolled", held);

  const result = await postBounded(deps, held.run_id, requestFor(held, false));
  switch (result.kind) {
    case "acked": {
      const next = asEnrolled(held, result);
      const written = await deps.store.replacePublicationRecord(next, {});
      if (!written.ok) return uncertain("ack_not_recorded", held, `enrollment_seq=${result.enrollment_seq} candidate_seq=${result.candidate_seq}`);
      return { kind: "enrolled", record: asEnrolled(written.record, result) };
    }
    case "unsupported":
    case "refused": {
      const declined = result.kind === "unsupported" || DECLINED_REASONS.has(result.reason);
      if (!declined) return uncertain("server_disagrees", held, result.kind === "refused" ? `${result.status} ${result.reason}` : undefined);
      // The api definitively holds no enrollment for this identity (a new enrollment was declined).
      // The sequence-less record never reached the api, so removing it retires nothing.
      if (!(await deps.store.discardPublicationRecord(held.run_id, held.claim_generation, {}))) {
        return uncertain("local_write_unverified", held, "declined enrollment left a local record");
      }
      return notEnrolled(result.kind === "unsupported" ? "server_feature_unavailable" : "server_declined",
        result.kind === "refused" ? result.reason : undefined);
    }
    case "unknown":
      return uncertain(deps.signal?.aborted ? "aborted" : "ack_unknown", held, result.detail);
  }
}

/**
 * Advance an enrolled record's candidate (a transformed candidate C' after the original). The new
 * candidate is saved locally FIRST (a crash-safe replace, with `candidate_seq` + 1) and only then
 * sent, so a lost ACK is recovered by repeating the identical request. A failed local save is
 * `uncertain` / `candidate_save_failed`: the previous record is still the enrollment, so no external
 * attempt may use the new candidate.
 */
export async function advancePublicationCandidate(
  deps: PublicationDeps,
  current: EnrolledPublicationRecord,
  update: { candidate: string; staged_description?: PublicationStagedDescription; guard?: PublicationGuardState; mr?: PublicationMrMetadata },
): Promise<PublicationAdvanceOutcome> {
  let next: EnrolledPublicationRecord;
  try {
    const { staged_description: _drop, ...rest } = current;
    next = parsePublicationRecord({
      ...rest, candidate: update.candidate, candidate_seq: current.candidate_seq + 1,
      ...(update.guard ? { guard: update.guard } : {}), ...(update.mr ? { mr: update.mr } : {}),
      ...(update.staged_description ? { staged_description: update.staged_description } : {}),
    }) as EnrolledPublicationRecord;
    if (publicationIntentBytes(next) > MAX_PUBLICATION_INTENT_BYTES) return uncertain("candidate_save_failed", current, "intent_too_large");
  } catch (err) {
    return uncertain("candidate_save_failed", current, err instanceof Error ? err.message : String(err));
  }
  const written = await deps.store.replacePublicationRecord(next, { enrollment_seq: current.enrollment_seq, candidate_seq: current.candidate_seq });
  if (!written.ok) return uncertain("candidate_save_failed", current, written.detail ?? written.reason);
  return sendEnrolled(deps, next);
}

/** Re-send an enrolled record's current state (a lost candidate ACK, or a register reconciliation). */
export async function resendPublicationRecord(deps: PublicationDeps, record: EnrolledPublicationRecord): Promise<PublicationAdvanceOutcome> {
  return sendEnrolled(deps, record);
}

async function sendEnrolled(deps: PublicationDeps, record: EnrolledPublicationRecord): Promise<PublicationAdvanceOutcome> {
  const result = await postBounded(deps, record.run_id, requestFor(record, true));
  switch (result.kind) {
    case "acked":
      if (result.enrollment_seq !== record.enrollment_seq || result.candidate_seq !== record.candidate_seq) {
        return uncertain("server_disagrees", record, `ack ${result.enrollment_seq}/${result.candidate_seq}`);
      }
      return { kind: "enrolled", record };
    case "refused":
      if (result.reason === "publication_enrollment_retired") {
        // The one positive confirmation: the api says THIS enrollment_seq is retired or completed.
        const discarded = await deps.store.discardPublicationRecord(record.run_id, record.claim_generation, { enrollment_seq: record.enrollment_seq });
        return { kind: "retired", discarded };
      }
      return uncertain("server_disagrees", record, `${result.status} ${result.reason}`);
    case "unsupported":
      return uncertain("server_disagrees", record, "publication_resume_v1 is not negotiated");
    case "unknown":
      return uncertain(deps.signal?.aborted ? "aborted" : "ack_unknown", record, result.detail);
  }
}

/** The refusal reason on a typed publication park's ack, when it is one of the two this feature adds. */
export function publicationParkRefusal(ack: { reason?: string }): PublicationParkRefusal | undefined {
  return ack.reason === "publication_enrolled" || ack.reason === "publication_not_enrolled" ? ack.reason : undefined;
}
