// PRD #1798 M6 (D8, D9, D10, D11, D12, D15, D17): the PR-description publisher and the completion
// interlock's read-replace-write reconcile.
//
// The renderer (pr-description.ts) is pure; this module does the I/O around it: it stages the
// run's description through the api (the only source of publishable text, D7), binds the staged
// version to the PR, reads the forge before and after writing (D11), and acknowledges what the
// forge ended up showing (D9). Every failure here is ADVISORY: nothing in this module fails or
// holds a run. The interlock's own reconcile (reconcileCompletion) reports a typed result and the
// runner decides whether to hold or fail closed. One outcome the runner does act on: on a
// NON-interlocked issue run whose completion block is non-closing, `closingRemains` reports that the PR may still carry a
// closing directive uzi could not remove, or could not rule out because it never saw the body and
// its blind rewrite failed (amended D10), and the runner fails the run closed.
//
// A publication runs under a time budget (the editor pass's remaining deadline plus FORGE_BUDGET_MS);
// every api call, forge call and wait honours it. prepare() runs under one budget; publish() starts a
// fresh one when it is called (after createMergeRequest returned), so a slow create cannot spend the
// publication's forge share. The editor pass's own deadline is fixed at prepare() and never
// restarts. A 409 `stale_claim` or `run_terminal` from any route stops the publication: no further
// api call and no forge write (the interlock's writes are the runner's and are unaffected).
//
// One forge write sits outside the budget: when a scanning publication (see closingRemains) that
// the caller enabled it for (spec.blindFallback: a NON-interlocked run, whose closingRemains the
// runner acts on) never read the PR, it falls back to the blind whole-body non-closing rewrite
// reconcileCompletion uses, under the caller's signal (spec.signal) rather than the spent or failed
// budget. It is a safety write, not a region update. An interlocked run never makes it: its own
// reconcile reads the PR afterwards and has its own blind fallback, so the write would only destroy
// human text.
//
// The publication, in the spec's steps:
//
//   1. prepare(): build the editor context and run the editor pass under ONE deadline, then stage
//      `generated` (pass OK), `lead_only` (pass failed, lead claims present) or `deterministic_only`
//      (neither). A failed stage (never retried) leaves the region deterministic and unversioned.
//   2. initialBody(): the body a NEW PR is created with (no preserved text).
//   3. publish(): read 1. A head or target that differs from the staged snapshot spends the ONE
//      regeneration (a new version for the read's snapshot; the old unbound one stays pending).
//   4. The PR's state: claim.pr_description, else a lookup of the observed region.
//   5. Compose from read 1 (outside text, the human-edit check, D10/D17, the interlock scan, the
//      D15 cap; a cap that shrank the region restages deterministic_only).
//   6. Bind the version with the FINAL region hash.
//   7. Read 2 revalidates: a changed description (same snapshot) spends the ONE recompose; a moved
//      head or target acks the bound version skipped_snapshot_moved and regenerates (step 3's
//      budget). Once a budget is spent, a further change skips the region write (the completion
//      block is still written). A body is never written from a read older than the latest read.
//   8. Write only when the composed body differs from the latest read.
//   9. Re-read: the bound region on the forge → ack `published`; a failed write → `write_failed`;
//      a post-write mismatch is recorded (write_failed), never retried.

import {
  PrDescriptionConflict,
  PrDescriptionMalformedResponse,
  PrDescriptionRateLimited,
  isTransient,
  type WorkerClient,
} from "./client.js";
import type { ForgeClient, MergeRequestDetail } from "./forge.js";
import type { Logger } from "./log.js";
import type { DeliveryContext, PreviousDescriptionFields } from "./pr-description-context.js";
import {
  COMPLETION_END,
  REGION_END,
  REGION_START,
  STALENESS_END,
  STALENESS_START,
  capBody,
  closingDirectiveOutsideCompletion,
  composeBody,
  parseOwnedBlocks,
  type ParsedBody,
  regionSha256,
  renderBody,
  renderCompletionBlock,
  renderRegion,
  type OwnedBlocks,
} from "./pr-description.js";
import type { ComputedSize } from "./pr-size.js";
import type {
  PrDescriptionAckOutcome,
  PrDescriptionState,
  PrDescriptionVerification,
  PrDescriptionVersionDTO,
  RawPrDescriptionFields,
} from "./protocol.js";
import type { PrSummaryClaim } from "./signals.js";
import type { DeliverySummary, DeliverySummaryClaimView, DeliverySummaryInput } from "./summary-runner.js";

// ── Seams ──────────────────────────────────────────────────────────────────────────────────

/** The forge reads and writes the publisher needs (every ForgeClient driver has them). */
export type PublisherForge = Pick<ForgeClient, "getMergeRequest" | "updateMergeRequestDescription">;

/** The four pr-description routes (WorkerClient). Stage is NEVER retried; bind, lookup and ack are
 *  safe to replay and get a bounded retry. */
export type PublisherApi = Pick<
  WorkerClient,
  "stagePrDescription" | "bindPrDescription" | "lookupPrDescription" | "ackPrDescription"
>;

/** The editor pass (SummaryRunner). Null under the stub executor or when none was injected. */
export interface DeliveryPass {
  deliverySummaryDeadline(): number;
  generateDeliverySummary(input: DeliverySummaryInput): Promise<DeliverySummary | null>;
}

export interface PublisherDeps {
  forge: PublisherForge;
  api: PublisherApi;
  pass: DeliveryPass | null;
  log: Pick<Logger, "info" | "warn">;
  /** A run status message (the runner's batcher, kind "status"). Fixed wording only. */
  emit: (text: string) => void;
  /** Wraps each forge call (the runner passes its bounded forge retry); default: one attempt.
   *  `signal` is the publication's budgeted signal: a retry must stop waiting once it aborts. */
  forgeRetry?: <T>(fn: () => Promise<T>, signal?: AbortSignal) => Promise<T>;
  /** Sleep between bind / lookup / ack replays and before a head-lag re-read (tests pass a no-op).
   *  The default wakes early when the publication's signal aborts. */
  sleep?: (ms: number) => Promise<void>;
  /** How long to wait before re-reading once when read 1's head is not the landed head (a forge
   *  may lag a push by a moment, GitLab especially). 0 disables the re-read. Default 2 s. */
  headLagRetryMs?: number;
  /** The forge-and-api share of each of the publication's time budgets (prepare()'s, and the fresh
   *  one publish() starts), added to the editor pass's own deadline. Default FORGE_BUDGET_MS. */
  forgeBudgetMs?: number;
}

/** The default forge-and-api share of a publication's time budget (see PublisherDeps.forgeBudgetMs). */
const FORGE_BUDGET_MS = 60_000;
/** The editor pass's share is its deadline, clamped here so a skewed clock cannot stretch the budget. */
const PASS_BUDGET_MAX_MS = 10 * 60_000;

/** The (head, target) a staged version describes. */
export interface PrSnapshot {
  headSha: string;
  targetBranch: string;
}

/** The deterministic facts of a snapshot: the size (pr-size.ts computeSize) and the merge-base the
 *  stage records as base_sha (null when it could not be computed: the version is then not staged). */
export interface SnapshotFacts {
  baseSha: string | null;
  size: ComputedSize;
}

export interface PublicationSpec {
  runId: string;
  claimGeneration: number;
  /** The claim facts the editor pass selects its harness from. */
  claim: DeliverySummaryClaimView;
  repoUrl: string;
  pat: string;
  /** `owner/repo` or `group/sub/project`, for the closing-directive scan (repoPathFromUrl). */
  repoPath?: string;
  /** D17: `own` runs author the completion block; `refresh` runs (mr_rework, a ci_fix adopting an
   *  existing branch) refresh only the region, and only on a PR that already carries uzi markers,
   *  touching the completion block's staleness line alone. */
  mode: "own" | "refresh";
  /** Set for an issue run: when the completion block is non-closing, the whole resulting body is
   *  scanned for a closing directive for this issue (amended D10). */
  interlockIssueIid?: number;
  /** Whether the completion block this run writes closes the issue. */
  completionCloses: boolean;
  /** Whether a scanning publication that never read the PR writes the blind whole-body
   *  non-closing rewrite (see the header). The runner sets it for a NON-interlocked run only: an
   *  interlocked run's reconcile owns the body after publish() and falls back blind on its own.
   *  Absent means off: the unread PR is left as it is and closingRemains reports it. */
  blindFallback?: boolean;
  /** claim.pr_description: the PR's record as the api delivered it. Absent is NOT authoritative. */
  prior?: PrDescriptionState;
  /** The lead's structured claims (D4), stamped with verifiedAtSha. */
  lead?: PrSummaryClaim;
  facts(snapshot: PrSnapshot): Promise<SnapshotFacts>;
  /** The redacted, budgeted editor input (buildDeliveryContext) for a snapshot. */
  context(snapshot: PrSnapshot, deadlineMs: number, previous: PreviousDescriptionFields | null): Promise<DeliveryContext>;
  /** The completion block this run writes, with the D12 staleness line when the two SHAs differ. */
  completion(staleness?: { describedSha: string; headSha: string }): string;
  signal?: AbortSignal;
}

/** What a publication did, for the runner's interlock and logs. */
export interface PublishOutcome {
  /** The last ack the publication sent for its final version (undefined: none was sent). */
  ack?: PrDescriptionAckOutcome;
  /** uzi's own region for this publication (the region a whole-body rewrite writes). */
  region: string;
  /** The head the region on the forge describes after this publication, when known (D12). */
  describedSha?: string;
  /** The body was written. */
  wrote: boolean;
  /** The amended-D10 scan found a closing directive and the body was rewritten whole. */
  interlockRewrite: boolean;
  /**
   * Only for a publication that scans (an `own` issue run whose completion block is non-closing):
   * the PR may still carry a closing directive for the issue outside uzi's completion block. True
   * when the body the PR is last known to carry has one (the rewrite failed, was never attempted,
   * e.g. a publication stopped after reading the PR, or could not be confirmed by a re-read), and
   * when no read of the PR ever succeeded and the blind whole-body non-closing rewrite that then
   * follows failed or was not attempted (spec.blindFallback off, or a stopped publication):
   * nothing observed is not proof of nothing closing. After a blind rewrite lands, the body it
   * wrote is what is judged. False when the publication does not scan. The runner fails such a
   * NON-interlocked run closed (amended D10); an interlocked run's own reconcile owns the body.
   */
  closingRemains: boolean;
}

// ── Small helpers ──────────────────────────────────────────────────────────────────────────

const SHA_RE = /^[0-9a-f]{7,64}$/i;
const SHA40_RE = /^[0-9a-f]{40}$/i;
const LEAD_SUMMARY_MAX_BYTES = 4000;
const REPLAY_ATTEMPTS = 3;
const MAX_ROUNDS = 4;

function toLf(s: string): string {
  return s.replace(/\r\n?/gu, "\n");
}

function sameSha(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}

function sameSnapshot(read: MergeRequestDetail, s: PrSnapshot): boolean {
  return sameSha(read.headSha, s.headSha) && read.targetBranch === s.targetBranch;
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** A wait that ends early (resolving, not rejecting) when `signal` aborts. */
function abortableSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve();
    const done = (): void => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal?.addEventListener("abort", done, { once: true });
  });
}

/** A 409 that means this worker no longer owns the run: the publication stops (Low L1). */
function stopsPublication(e: unknown): boolean {
  return e instanceof PrDescriptionConflict && (e.reason === "stale_claim" || e.reason === "run_terminal");
}

/**
 * Each body read once (Low L2): its parse and its closing-directive scans, keyed by the body text
 * (and, for a scan, the completion block it is scanned against). A publication or a reconcile
 * handles a handful of bodies, so the memo stays small and lives only as long as its owner.
 */
class BodyMemo {
  private readonly parses = new Map<string, ParsedBody>();
  private readonly scans = new Map<string, Map<string, boolean>>();

  parse(body: string): ParsedBody {
    let p = this.parses.get(body);
    if (p === undefined) {
      p = parseOwnedBlocks(body);
      this.parses.set(body, p);
    }
    return p;
  }

  /** closingDirectiveOutsideCompletion(body, issueIid, completion, repoPath), computed once. */
  closingOutside(body: string, issueIid: number, completion: string, repoPath: string | undefined): boolean {
    const key = `${issueIid}\u0000${repoPath ?? ""}\u0000${completion}`;
    let byBody = this.scans.get(key);
    if (byBody === undefined) {
      byBody = new Map();
      this.scans.set(key, byBody);
    }
    let hit = byBody.get(body);
    if (hit === undefined) {
      hit = closingDirectiveOutsideCompletion(body, issueIid, completion, repoPath, this.parse(body));
      byBody.set(body, hit);
    }
    return hit;
  }

  /** The region hash of a body read from the forge, or undefined when it carries no parseable region. */
  regionHash(body: string): string | undefined {
    const p = this.parse(body);
    return p.kind === "ok" && p.region !== undefined ? regionSha256(p.region) : undefined;
  }

  /** The completion block of `body` parses and equals `completion` (line endings aside). */
  carries(body: string, completion: string): boolean {
    const p = this.parse(body);
    return p.kind === "ok" && p.completion !== undefined && toLf(p.completion) === toLf(completion);
  }
}

/** The longest prefix of `s` within `max` UTF-8 bytes, cut on a code-point boundary. */
function clampBytes(s: string, max: number): string {
  if (Buffer.byteLength(s, "utf8") <= max) return s;
  let out = "";
  let used = 0;
  for (const ch of s) {
    const n = Buffer.byteLength(ch, "utf8");
    if (used + n > max) break;
    out += ch;
    used += n;
  }
  return out;
}

/** `owner/repo` (or a GitLab group path) from a forge web URL; undefined when it does not parse,
 *  which makes the closing scan treat every qualified reference as this repo's (fail closed). */
export function repoPathFromUrl(url: string): string | undefined {
  try {
    const p = new URL(url).pathname.replace(/\.git$/iu, "").replace(/^\/+|\/+$/gu, "");
    return p || undefined;
  } catch {
    return undefined;
  }
}

/** The lead's verification entries, stamped with the SHA they were reported at. Without a valid
 *  stamp every entry is dropped (the api rejects an unstamped entry). */
function leadVerification(lead: PrSummaryClaim | undefined): PrDescriptionVerification[] {
  const sha = lead?.verifiedAtSha?.trim();
  if (!lead?.verification?.length || !sha || !SHA_RE.test(sha)) return [];
  return lead.verification.map((v) => ({ command: v.command, result: v.result, verified_at_sha: sha.toLowerCase() }));
}

/** D8 rung 2: the lead's claims as RAW fields (`what` + " " + `why` as the summary, clamped to the
 *  api's raw summary cap), or null when the lead declared nothing. */
function leadFields(lead: PrSummaryClaim | undefined): RawPrDescriptionFields | null {
  if (!lead) return null;
  const summary = clampBytes([lead.what, lead.why].filter((s): s is string => !!s?.trim()).map((s) => s.trim()).join(" "), LEAD_SUMMARY_MAX_BYTES);
  const fields: RawPrDescriptionFields = {
    summary,
    changes: [...(lead.changes ?? [])],
    scope_notes: (lead.scope_notes ?? []).map((n) => ({ kind: n.kind, text: n.text })),
    review_pointers: [...(lead.review_pointers ?? [])],
    verification: leadVerification(lead),
  };
  const empty =
    !fields.summary &&
    fields.changes.length === 0 &&
    fields.scope_notes.length === 0 &&
    fields.review_pointers.length === 0 &&
    fields.verification.length === 0;
  return empty ? null : fields;
}

const EMPTY_FIELDS: RawPrDescriptionFields = { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] };

const SIZE_LINE_RE = /^\*\*Size:\*\* [^\n]*$/u;
const PROVENANCE_RE = /^Describes `[0-9a-f]{7}` against .+\.$/u;

/**
 * Whether `region` is exactly uzi's deterministic region: the size line and/or the provenance line
 * and nothing else (renderRegion with no fields). Bodies created before this publisher carry a
 * size-line-only region with no provenance and no version; that shape is uzi's own, so a region
 * with no published version and no matching version (lookup `none`) may be overwritten only when
 * it has this shape. Anything else is treated as a human edit.
 */
export function isDeterministicRegion(region: string): boolean {
  const lines = toLf(region).split("\n");
  if (lines[0] !== REGION_START || lines[lines.length - 1] !== REGION_END) return false;
  const inner = lines.slice(1, -1);
  if (inner.length === 0) return true;
  if (inner.length === 1) return SIZE_LINE_RE.test(inner[0]!) || PROVENANCE_RE.test(inner[0]!);
  return inner.length === 3 && SIZE_LINE_RE.test(inner[0]!) && inner[1] === "" && PROVENANCE_RE.test(inner[2]!);
}

/** The D12 staleness pair (inner markers included) exactly as renderCompletionBlock renders it,
 *  or "" when the two SHAs name the same commit. */
function stalenessPair(describedSha: string, headSha: string): string {
  const block = renderCompletionBlock({ branch: "b", closes: false, kindLine: "", staleness: { describedSha, headSha } });
  const start = block.indexOf(STALENESS_START);
  const end = block.indexOf(STALENESS_END);
  return start >= 0 && end > start ? block.slice(start, end + STALENESS_END.length) : "";
}

/**
 * D17: `completion` (an existing block read from the forge) with ONLY its staleness line replaced
 * by `pair` ("" removes it). Everything else in the block is kept byte for byte. A block without a
 * staleness line gets the pair inserted before its footer rule (or its end marker).
 */
export function withStalenessLine(completion: string, pair: string): string {
  const start = completion.indexOf(STALENESS_START);
  const end = completion.indexOf(STALENESS_END);
  if (start >= 0 && end > start) {
    const tail = end + STALENESS_END.length;
    if (pair) return completion.slice(0, start) + pair + completion.slice(tail);
    // Drop the pair and the blank line that separated it from the next paragraph.
    const after = completion.slice(tail).replace(/^(?:\r?\n){1,2}/u, "");
    return completion.slice(0, start) + after;
  }
  if (!pair) return completion;
  const footer = completion.lastIndexOf("\n---\n");
  const at = footer >= 0 ? footer + 1 : completion.lastIndexOf(`\n${COMPLETION_END}`) + 1;
  if (at <= 0) return completion;
  return `${completion.slice(0, at)}${pair}\n\n${completion.slice(at)}`;
}

async function replay<T>(fn: () => Promise<T>, sleep: (ms: number) => Promise<void>): Promise<T> {
  let last: unknown;
  for (let attempt = 0; attempt < REPLAY_ATTEMPTS; attempt++) {
    try {
      return await fn();
    } catch (e) {
      last = e;
      const retryable = e instanceof PrDescriptionMalformedResponse || isTransient(e);
      if (!retryable || attempt === REPLAY_ATTEMPTS - 1) throw e;
      await sleep(e instanceof PrDescriptionRateLimited && e.retryAfterMs !== undefined ? e.retryAfterMs : 250 * (attempt + 1));
    }
  }
  throw last;
}

// ── The publication ────────────────────────────────────────────────────────────────────────

interface Staged {
  snapshot: PrSnapshot;
  /** undefined: persistence failed (D8 rung 3): nothing to bind or ack. */
  version?: PrDescriptionVersionDTO;
  /** The full region for this version (fields when staged with any). */
  region: string;
  /** The size-and-provenance-only region for the same snapshot (the D15 fallback). */
  sizeOnly: string;
  /** The hash this version was bound with, once bound. */
  boundHash?: string;
  /** The version has been acknowledged (no further ack for it). */
  acked?: boolean;
}

type RegionSkip = "skipped_human_edit" | "skipped_no_region" | "skipped_malformed" | "skipped_snapshot_moved";

interface Composition {
  /** The body to write; undefined: leave the forge body as it is. */
  body: string | undefined;
  /** The region text the body carries as uzi's (the region written or kept as ours). */
  region?: string;
  /** Why the region was not written. */
  skip?: RegionSkip;
  /** The D15 cap replaced the region with its size-only form. */
  capped: boolean;
  /** The amended-D10 scan forced a whole-body non-closing rewrite. */
  interlockRewrite: boolean;
  /** A refresh on a legacy PR (no markers): nothing is written, nothing is acked. */
  legacyUntouched: boolean;
  describedSha?: string;
  /** The completion block this composition carries as uzi's (the closing scan's expected block). */
  completion?: string;
}

const SKIP_MESSAGES: Record<RegionSkip, string> = {
  skipped_human_edit:
    "PR description: the description region was edited by a person, so uzi left it as it is and refreshed only its completion block",
  skipped_no_region: "PR description: the description region was removed from the PR, so uzi did not put it back",
  skipped_malformed: "PR description: uzi's markers in the PR description are malformed, so uzi left the description region untouched",
  skipped_snapshot_moved: "PR description: the PR head or target moved while uzi was writing the description, so the description region was not updated",
};

export class PrDescriptionPublisher {
  constructor(private readonly deps: PublisherDeps) {}

  /** Steps 1 and 2: stage the description for `snapshot` and return the publication. Never throws. */
  async prepare(spec: PublicationSpec, snapshot: PrSnapshot): Promise<PrDescriptionPublication> {
    const pub = new PrDescriptionPublication(this.deps, spec);
    await pub.stageInitial(snapshot);
    return pub;
  }
}

export class PrDescriptionPublication {
  private staged!: Staged;
  /** The editor pass's ONE deadline (D2), fixed when the publication starts (prepare). */
  private readonly deadline: number | undefined;
  private regenerationLeft = true;
  private recomposeLeft = true;
  private state: PrDescriptionState | null;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly acks: PrDescriptionAckOutcome[] = [];
  /** Every region this publication rendered (each staged version's region and size-only form): a
   *  region on the forge equal to one of them is uzi's own, whatever became of its version. */
  private readonly rendered = new Set<string>();
  /** spec.signal combined with the current phase's time budget (Timing): every api call, forge call
   *  and wait uses it. Set at prepare(), and replaced by a fresh budget when publish() starts. */
  private signal: AbortSignal;
  /** A stale_claim / run_terminal 409 was answered: no further api call and no forge write. */
  private stopped = false;
  private readonly memo = new BodyMemo();
  /** The PR body as last read (or confirmed) from the forge, and the completion block uzi's last
   *  composition expected in it: what `closingRemains` is judged on. */
  private forgeBody: string | undefined;
  private expectedCompletion: string | undefined;

  constructor(
    private readonly deps: PublisherDeps,
    private readonly spec: PublicationSpec,
  ) {
    this.state = spec.prior ?? null;
    this.deadline = deps.pass?.deliverySummaryDeadline();
    this.signal = this.budget();
    this.sleep = deps.sleep ?? ((ms) => abortableSleep(ms, this.signal));
  }

  /** A budget starting now: what is left of the editor pass's deadline (a publish-time regeneration
   *  runs the pass again under that same deadline) plus the forge-and-api share. */
  private budget(): AbortSignal {
    const passMs = this.deadline === undefined ? 0 : Math.min(PASS_BUDGET_MAX_MS, Math.max(0, this.deadline - Date.now()));
    const budget = AbortSignal.timeout(passMs + (this.deps.forgeBudgetMs ?? FORGE_BUDGET_MS));
    return this.spec.signal ? AbortSignal.any([this.spec.signal, budget]) : budget;
  }

  private forgeRetry<T>(fn: () => Promise<T>): Promise<T> {
    return this.deps.forgeRetry ? this.deps.forgeRetry(fn, this.signal) : fn();
  }

  /** Record a 409 that stops the publication; true when `e` was one. */
  private stopOn(e: unknown): boolean {
    if (!stopsPublication(e)) return false;
    if (!this.stopped) {
      this.stopped = true;
      this.deps.log.warn("PR description: the api says this run's claim is no longer live; the publication stops", {
        run_id: this.spec.runId,
        reason: (e as PrDescriptionConflict).reason,
      });
    }
    return true;
  }

  /** uzi's own region for the current staged version. */
  get region(): string {
    return this.staged.region;
  }

  /** The body a NEW PR is created with: the region and the completion block, no preserved text
   *  (D15-capped like any other body). */
  initialBody(completion: string): string {
    return capBody((r) => renderBody(r, completion), this.staged.region, this.staged.sizeOnly).body ?? renderBody(this.staged.sizeOnly, completion);
  }

  /** @internal prepare()'s staging. */
  async stageInitial(snapshot: PrSnapshot): Promise<void> {
    this.staged = await this.stage(snapshot, false);
    this.remember();
  }

  // ── Staging (step 1, and every restage) ──

  private async stage(snapshot: PrSnapshot, deterministic: boolean): Promise<Staged> {
    const { spec, deps } = this;
    let facts: SnapshotFacts | undefined;
    try {
      facts = await spec.facts(snapshot);
    } catch (e) {
      deps.log.warn("PR description: snapshot facts unavailable", { run_id: spec.runId, error: errMsg(e) });
    }
    const sizeLine = facts?.size.line ?? null;
    let source: "generated" | "lead_only" | "deterministic_only" = "deterministic_only";
    let fields: RawPrDescriptionFields = EMPTY_FIELDS;
    if (!deterministic) {
      const generated = await this.editorPass(snapshot);
      const lead = leadFields(spec.lead);
      if (generated) {
        source = "generated";
        fields = { ...generated, scope_notes: generated.scope_notes.map((n) => ({ ...n })), verification: leadVerification(spec.lead) };
      } else if (lead) {
        source = "lead_only";
        fields = lead;
      }
    }
    let version: PrDescriptionVersionDTO | undefined;
    const base = facts?.baseSha ?? null;
    if (this.stopped) {
      // A stopped publication stages nothing more (its region stays deterministic and unversioned).
    } else if (base && SHA40_RE.test(base) && SHA40_RE.test(snapshot.headSha) && snapshot.targetBranch) {
      try {
        // Stage is NEVER retried: a lost response may already have stored a version.
        version = (
          await deps.api.stagePrDescription(
            spec.runId,
            {
              claim_generation: spec.claimGeneration,
              source,
              fields,
              size: facts?.size.size ?? null,
              base_sha: base.toLowerCase(),
              head_sha: snapshot.headSha.toLowerCase(),
              target_branch: snapshot.targetBranch,
            },
            this.signal,
          )
        ).version;
      } catch (e) {
        this.stopOn(e);
        deps.log.warn("PR description: staging failed; the region carries the size line only", {
          run_id: spec.runId,
          error: errMsg(e),
        });
      }
    } else {
      deps.log.warn("PR description: the snapshot has no merge-base or head; the region is not staged", { run_id: spec.runId });
    }
    const input = { sizeLine, headSha: snapshot.headSha, targetBranch: snapshot.targetBranch };
    return {
      snapshot,
      version,
      region: renderRegion({ ...input, source: version?.source }, version?.fields).text,
      sizeOnly: renderRegion(input).text,
    };
  }

  /** The editor pass for `snapshot`, under the publication's ONE deadline (D2). Null on any failure. */
  private async editorPass(snapshot: PrSnapshot): Promise<DeliverySummary | null> {
    const { pass, log } = this.deps;
    if (!pass || this.deadline === undefined) return null;
    try {
      const previous = this.state?.published_version?.fields ?? null;
      const context = await this.spec.context(snapshot, this.deadline, previous);
      return await pass.generateDeliverySummary({ claim: this.spec.claim, context, deadlineMs: this.deadline });
    } catch (e) {
      log.warn("PR description: the editor pass failed", { run_id: this.spec.runId, error: errMsg(e) });
      return null;
    }
  }

  // ── api calls ──

  private async bind(mrIid: number, staged: Staged, hash: string): Promise<void> {
    if (this.stopped || !staged.version || staged.boundHash !== undefined) return;
    try {
      const res = await replay(
        () =>
          this.deps.api.bindPrDescription(
            this.spec.runId,
            { claim_generation: this.spec.claimGeneration, version_id: staged.version!.id, mr_iid: mrIid, rendered_region_sha256: hash },
            this.signal,
          ),
        this.sleep,
      );
      staged.boundHash = hash;
      this.state = res.pr;
    } catch (e) {
      this.stopOn(e);
      this.deps.log.warn("PR description: binding the version failed", { run_id: this.spec.runId, error: errMsg(e) });
    }
  }

  private async lookup(mrIid: number, hash: string): Promise<{ match: "published" | "pending" | "none" } | undefined> {
    if (this.stopped) return undefined;
    try {
      const res = await replay(
        () =>
          this.deps.api.lookupPrDescription(
            this.spec.runId,
            { claim_generation: this.spec.claimGeneration, mr_iid: mrIid, region_sha256: hash },
            this.signal,
          ),
        this.sleep,
      );
      if (res.pr) this.state = res.pr;
      return { match: res.match };
    } catch (e) {
      this.stopOn(e);
      this.deps.log.warn("PR description: looking up the PR's region failed", { run_id: this.spec.runId, error: errMsg(e) });
      return undefined;
    }
  }

  /** Ack a BOUND version once. Only the six api outcomes exist; a lost compare-and-swap refreshes
   *  the lock version through a lookup and replays. */
  private async ack(mrIid: number, staged: Staged, outcome: PrDescriptionAckOutcome, observed?: string): Promise<void> {
    if (this.stopped || !staged.version || staged.boundHash === undefined || staged.acked) return;
    staged.acked = true;
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const res = await replay(
          () =>
            this.deps.api.ackPrDescription(
              this.spec.runId,
              {
                claim_generation: this.spec.claimGeneration,
                version_id: staged.version!.id,
                outcome,
                expected_lock_version: this.state?.lock_version ?? 0,
                ...(observed ? { observed_region_sha256: observed } : {}),
              },
              this.signal,
            ),
          this.sleep,
        );
        this.state = res.pr;
        this.acks.push(outcome);
        return;
      } catch (e) {
        if (attempt === 0 && e instanceof PrDescriptionConflict && e.reason === "lock_conflict") {
          await this.lookup(mrIid, observed ?? regionSha256(""));
          if (this.stopped) return;
          continue;
        }
        this.stopOn(e);
        this.deps.log.warn("PR description: the ack failed", { run_id: this.spec.runId, outcome, error: errMsg(e) });
        return;
      }
    }
  }

  private async read(mrIid: number): Promise<MergeRequestDetail> {
    const { spec, deps } = this;
    const read = await this.forgeRetry(() => deps.forge.getMergeRequest(spec.repoUrl, spec.pat, mrIid, this.signal));
    this.forgeBody = read.description;
    return read;
  }

  // ── Ownership and composition ──

  /**
   * Whether the region on the forge is uzi's to overwrite (D10 rule 2): it is a region this
   * publication rendered (its current or a superseded version's), the published version's region, a region a pending version rendered (a
   * lost ack, D9 step 4), or (with no published version and no matching version) exactly uzi's
   * deterministic shape. Also refreshes this.state (a lookup answers the PR's record).
   */
  private async ownership(mrIid: number, region: string | undefined): Promise<"ours" | "human"> {
    const hash = regionSha256(region ?? "");
    const published = this.state?.published_version?.rendered_region_sha256;
    if (region !== undefined && this.rendered.has(toLf(region))) {
      if (!this.spec.prior && !this.state) await this.lookup(mrIid, hash);
      return "ours";
    }
    if (region !== undefined && published && published === hash) return "ours";
    const found = await this.lookup(mrIid, hash);
    if (region === undefined) return "ours";
    if (found && found.match !== "none") return "ours";
    return !this.state?.published_version && isDeterministicRegion(region) ? "ours" : "human";
  }

  /** Amended D10: an `own` issue publication whose completion block is non-closing scans the whole
   *  resulting body for a closing directive for the issue. */
  private scans(): boolean {
    return this.spec.mode === "own" && this.spec.interlockIssueIid !== undefined && !this.spec.completionCloses;
  }

  /** `body` has a closing directive for the issue outside `completion` (only when scanning). */
  private closingIn(body: string, completion: string): boolean {
    return this.scans() && this.memo.closingOutside(body, this.spec.interlockIssueIid!, completion, this.spec.repoPath);
  }

  private compose(read: MergeRequestDetail, wantRegion: boolean, owner: "ours" | "human"): Composition {
    const { spec } = this;
    const staged = this.staged;
    const parsed = this.memo.parse(read.description);
    const hasPublished = !!this.state?.published_version;
    const publishedHead = this.state?.published_version?.head_sha;
    let skip: RegionSkip | undefined;
    let writeRegion = wantRegion;
    if (parsed.kind === "malformed") {
      skip = "skipped_malformed";
      writeRegion = false;
    } else if (parsed.kind === "none") {
      if (spec.mode === "refresh") {
        return { body: undefined, capped: false, interlockRewrite: false, legacyUntouched: true };
      }
      if (hasPublished) {
        skip = "skipped_no_region";
        writeRegion = false;
      }
    } else if (parsed.region !== undefined) {
      if (owner === "human") {
        skip = "skipped_human_edit";
        writeRegion = false;
      }
    } else if (hasPublished) {
      skip = "skipped_no_region";
      writeRegion = false;
    }
    const describedFor = (written: boolean): string | undefined => (written ? staged.snapshot.headSha : publishedHead);
    const completionFor = (written: boolean): string => {
      const d = describedFor(written);
      return spec.completion(d ? { describedSha: d, headSha: read.headSha } : undefined);
    };

    // The body with `region` as uzi's region (undefined: keep whatever region the forge shows).
    const build = (region: string | undefined): string | undefined => {
      const completion = completionFor(region !== undefined);
      if (parsed.kind === "malformed") return undefined;
      if (parsed.kind === "none") {
        // own mode only (refresh returned above). A legacy PR with no published version is
        // rewritten whole (D10, today's behaviour); a removed region keeps the text and appends
        // the completion block.
        return region !== undefined ? renderBody(region, completion) : `${read.description.trimEnd()}\n\n${completion}`;
      }
      const parts: OwnedBlocks = parsed;
      if (spec.mode === "refresh") {
        const nextCompletion =
          parts.completion !== undefined ? withStalenessLine(parts.completion, stalenessOf(region !== undefined)) : undefined;
        if (region !== undefined && parts.region === undefined) {
          return insertRegion(parts, region, nextCompletion);
        }
        return composeBody(parts, { region, completion: nextCompletion });
      }
      if (region !== undefined && parts.region === undefined) return insertRegion(parts, region, completion);
      if (parts.completion === undefined) {
        const withRegion = composeBody(parts, { region }) ?? read.description;
        return `${withRegion.trimEnd()}\n\n${completion}`;
      }
      return composeBody(parts, { region, completion });
    };
    const stalenessOf = (written: boolean): string => {
      const d = describedFor(written);
      return d ? stalenessPair(d, read.headSha) : "";
    };

    let body: string | undefined;
    let region: string | undefined;
    let capped = false;
    if (writeRegion) {
      const res = capBody((r) => build(r), staged.region, staged.sizeOnly);
      body = res.body;
      capped = res.capped;
      region = capped ? staged.sizeOnly : staged.region;
    } else {
      body = build(undefined);
    }

    // Amended D10: a non-closing completion for an issue run → scan the WHOLE resulting body for a
    // closing directive uzi does not write; if one remains, preservation yields: rewrite the body
    // whole, with uzi's own region and completion only.
    let interlockRewrite = false;
    let completion = completionFor(region !== undefined);
    if (this.scans()) {
      const ownRegion = region ?? staged.region;
      const result = body ?? read.description;
      if (this.memo.closingOutside(result, spec.interlockIssueIid!, completion, spec.repoPath)) {
        completion = completionFor(true);
        const wholeCompletion = completion;
        const whole = capBody((r) => renderBody(r, wholeCompletion), ownRegion, staged.sizeOnly);
        body = whole.body ?? renderBody(staged.sizeOnly, wholeCompletion);
        region = whole.capped ? staged.sizeOnly : ownRegion;
        capped = capped || whole.capped;
        interlockRewrite = true;
        skip = undefined;
      }
    }
    return {
      body,
      region,
      skip: region === undefined ? skip : undefined,
      capped,
      interlockRewrite,
      legacyUntouched: false,
      describedSha: describedFor(region !== undefined),
      completion,
    };
  }

  // ── Steps 3-9 ──

  /** Publish onto PR `mrIid`. Never throws; every failure is advisory, except that a scanning
   *  publication reports `closingRemains` (the runner fails that run closed). */
  async publish(mrIid: number): Promise<PublishOutcome> {
    // The forge budget starts here, after createMergeRequest returned, not at prepare().
    this.signal = this.budget();
    try {
      return await this.publishInner(mrIid);
    } catch (e) {
      this.deps.log.warn("PR description: publishing failed", { run_id: this.spec.runId, error: errMsg(e) });
      const wrote = await this.blindIfUnread(mrIid);
      return {
        region: this.staged.region,
        wrote,
        interlockRewrite: false,
        ack: this.acks.at(-1),
        closingRemains: this.remainsClosing(),
      };
    }
  }

  /**
   * A scanning publication that never read the PR (read 1 failed, was over the byte cap, or the
   * budget ran out first) cannot tell whether an adopted body closes the issue, so, when
   * spec.blindFallback allows it (a non-interlocked run), it writes the blind whole-body
   * non-closing rewrite reconcileCompletion falls back to: uzi's region and the run's non-closing
   * completion block, nothing preserved. It runs under spec.signal, not the budget (see the
   * header). True when it was written.
   */
  private async blindIfUnread(mrIid: number): Promise<boolean> {
    if (this.spec.blindFallback !== true || !this.scans() || this.forgeBody !== undefined) return false;
    // No current path reaches here stopped with no body read (a stop before read 1 returns from
    // publishInner without throwing); kept so a stopped publication can never make this write.
    if (this.stopped) return false;
    const { spec, deps } = this;
    const completion = spec.completion();
    const body = this.initialBody(completion);
    const write = () => deps.forge.updateMergeRequestDescription(spec.repoUrl, spec.pat, mrIid, body, spec.signal);
    try {
      await (deps.forgeRetry ? deps.forgeRetry(write, spec.signal) : write());
    } catch (e) {
      deps.log.warn("PR description: the PR could not be read, and the non-closing rewrite failed", {
        run_id: spec.runId,
        error: errMsg(e),
      });
      return false;
    }
    this.forgeBody = body;
    this.expectedCompletion = completion;
    deps.log.info("PR description: the PR could not be read, so its body was rewritten whole and non-closing", {
      run_id: spec.runId,
    });
    return true;
  }

  /** The PR may still close the issue (see PublishOutcome.closingRemains). The last body known on
   *  the forge is scanned against the completion block uzi last composed (the run's own block when
   *  none was): a forge block that differs from it is scanned as ordinary text (fail closed). A
   *  scanning publication that knows no body at all fails closed. */
  private remainsClosing(): boolean {
    if (!this.scans()) return false;
    if (this.forgeBody === undefined) return true;
    return this.closingIn(this.forgeBody, this.expectedCompletion ?? this.spec.completion());
  }

  private async restage(snapshot: PrSnapshot, deterministic: boolean): Promise<void> {
    this.staged = await this.stage(snapshot, deterministic);
    this.remember();
  }

  private remember(): void {
    this.rendered.add(toLf(this.staged.region));
    this.rendered.add(toLf(this.staged.sizeOnly));
  }

  private async publishInner(mrIid: number): Promise<PublishOutcome> {
    const { deps } = this;
    if (this.stopped) {
      deps.log.info("PR description: the publication was stopped; the PR is left as it is", { run_id: this.spec.runId });
      // Judged like a stop at bind or lookup, which comes after read 1: a scanning publication reads
      // the PR (a read, no api call and no write) and judges closingRemains on what it shows. An
      // unreadable PR is not rewritten (a stopped publication makes no forge write), so it fails
      // closed.
      if (this.scans()) {
        try {
          await this.read(mrIid);
        } catch (e) {
          deps.log.warn("PR description: a stopped publication could not read the PR", { run_id: this.spec.runId, error: errMsg(e) });
        }
      }
      return { region: this.staged.region, wrote: false, interlockRewrite: false, closingRemains: this.remainsClosing() };
    }
    let read = await this.read(mrIid);
    // Step 3. A forge can lag a push by a moment (GitLab especially): when the head differs, re-read
    // once after a short wait before spending the regeneration on what may be a stale head.
    if (!sameSha(read.headSha, this.staged.snapshot.headSha) && (deps.headLagRetryMs ?? 2000) > 0) {
      await this.sleep(deps.headLagRetryMs ?? 2000);
      this.signal.throwIfAborted();
      read = await this.read(mrIid);
    }
    if (!sameSnapshot(read, this.staged.snapshot)) {
      this.regenerationLeft = false;
      await this.restage({ headSha: read.headSha, targetBranch: read.targetBranch }, false);
    }
    // A region skip forced by a spent budget (the completion block is still written).
    let budgetSkip: RegionSkip | undefined;
    let comp!: Composition;
    let latest = read;
    for (let round = 0; round < MAX_ROUNDS; round++) {
      // Steps 4-5.
      comp = await this.composeFrom(mrIid, read, budgetSkip === undefined);
      if (comp.legacyUntouched) {
        deps.log.info("PR description: a refresh leaves a PR without uzi markers untouched", { run_id: this.spec.runId });
        return { region: this.staged.region, wrote: false, interlockRewrite: false, closingRemains: false };
      }
      if (comp.capped && this.staged.version && this.staged.version.source !== "deterministic_only") {
        // D15: the cap dropped the fields, so the staged version no longer matches what is written.
        // It is still unbound, so it simply stays pending; a deterministic_only version replaces it.
        await this.restage(this.staged.snapshot, true);
        comp = await this.composeFrom(mrIid, read, budgetSkip === undefined);
      }
      // A stopped publication neither binds nor revalidates; step 8 then writes nothing.
      if (this.stopped) break;
      // Step 6: bind with the FINAL region hash (the region this version would write).
      await this.bind(mrIid, this.staged, regionSha256(comp.region ?? this.staged.region));
      // Step 7: read 2 revalidates the snapshot and the description.
      const read2 = await this.read(mrIid);
      latest = read2;
      if (!sameSnapshot(read2, read)) {
        // (b) The head or target moved: the bound version describes a snapshot the PR no longer has.
        await this.ack(mrIid, this.staged, "skipped_snapshot_moved");
        read = read2;
        if (this.regenerationLeft) {
          this.regenerationLeft = false;
          await this.restage({ headSha: read2.headSha, targetBranch: read2.targetBranch }, false);
          continue;
        }
        budgetSkip = "skipped_snapshot_moved";
        comp = await this.composeFrom(mrIid, read, false);
        break;
      }
      if (read2.description !== read.description) {
        // (a) The description changed on the same snapshot.
        read = read2;
        if (this.recomposeLeft) {
          this.recomposeLeft = false;
          const next = await this.composeFrom(mrIid, read, true);
          if (next.region !== undefined && comp.region !== undefined && next.region !== comp.region && this.staged.version) {
            // The recompose changed the region (a different D15 cap): the bound version no longer
            // matches what would be written. skipped_snapshot_moved is the outcome that abandons a
            // bound version superseded this way (there is no "abandoned" outcome to send); the
            // replacement is a deterministic_only version, bound and revalidated with a new read.
            await this.ack(mrIid, this.staged, "skipped_snapshot_moved");
            await this.restage(this.staged.snapshot, true);
            continue;
          }
          comp = next;
          break;
        }
        // The recompose is spent: the region is left, the completion block is still written.
        await this.ack(mrIid, this.staged, "skipped_human_edit");
        budgetSkip = "skipped_human_edit";
        comp = await this.composeFrom(mrIid, read, false);
        break;
      }
      break;
    }
    if (budgetSkip === undefined && comp.region === undefined && comp.skip) {
      await this.ack(mrIid, this.staged, comp.skip);
    }
    const skip = comp.region === undefined ? (budgetSkip ?? comp.skip) : undefined;
    if (skip && !this.stopped) deps.emit(SKIP_MESSAGES[skip]);

    // The body the forge carries now is the latest read (until a write is confirmed), and what the
    // closing scan expects in it is the completion block this composition carries.
    this.forgeBody = latest.description;
    this.expectedCompletion = comp.completion;
    // Step 8: write only when the body composed from the LATEST read differs from it.
    const observedBefore = this.memo.regionHash(latest.description);
    let wrote = false;
    if (this.stopped) {
      deps.log.info("PR description: the publication was stopped; nothing is written", { run_id: this.spec.runId });
      return this.outcome(comp, false, false);
    }
    if (comp.body !== undefined && comp.body !== latest.description) {
      try {
        await this.forgeRetry(() =>
          deps.forge.updateMergeRequestDescription(this.spec.repoUrl, this.spec.pat, mrIid, comp.body!, this.signal),
        );
        wrote = true;
      } catch (e) {
        deps.log.warn("PR description: the forge write failed", { run_id: this.spec.runId, error: errMsg(e) });
        await this.ack(mrIid, this.staged, "write_failed", observedBefore);
        return this.outcome(comp, false, observedBefore === this.staged.boundHash);
      }
    }
    // Step 9: confirm. Without a write the latest read is the confirmation.
    let confirm = latest;
    if (wrote) {
      try {
        confirm = await this.read(mrIid);
      } catch (e) {
        // Unconfirmed: the version stays bound and pending, so the next writer's lookup recovers
        // it (D9 step 4) if the write landed. Never ack published for an unconfirmed version. For
        // closingRemains the forge still carries the pre-write body (the write is unconfirmed).
        this.forgeBody = latest.description;
        deps.log.warn("PR description: could not re-read the PR after writing", { run_id: this.spec.runId, error: errMsg(e) });
        return this.outcome(comp, true, false);
      }
    }
    const observed = this.memo.regionHash(confirm.description);
    const onForge = observed !== undefined && observed === this.staged.boundHash;
    if (onForge) {
      await this.ack(mrIid, this.staged, "published", observed);
    } else if (comp.region !== undefined) {
      // A post-write mismatch (or a region that never reached the forge) is recorded, not retried.
      await this.ack(mrIid, this.staged, "write_failed", observed);
    }
    return this.outcome(comp, wrote, onForge);
  }

  private async composeFrom(mrIid: number, read: MergeRequestDetail, wantRegion: boolean): Promise<Composition> {
    const parsed = this.memo.parse(read.description);
    const region = parsed.kind === "ok" ? parsed.region : undefined;
    const owner = parsed.kind === "malformed" ? "human" : await this.ownership(mrIid, region);
    return this.compose(read, wantRegion, owner);
  }

  private outcome(comp: Composition, wrote: boolean, onForge: boolean): PublishOutcome {
    return {
      ack: this.acks.at(-1),
      region: comp.region ?? this.staged.region,
      describedSha: onForge ? this.staged.snapshot.headSha : comp.region === undefined ? comp.describedSha : undefined,
      wrote,
      interlockRewrite: comp.interlockRewrite,
      closingRemains: this.remainsClosing(),
    };
  }
}

/** `parts` with `region` inserted before the completion block (a body that had none). */
function insertRegion(parts: OwnedBlocks, region: string, completion: string | undefined): string {
  if (parts.completion === undefined) return `${region}\n\n${parts.before}${parts.between}${parts.after}`;
  return `${parts.before}${region}\n\n${parts.between}${completion ?? parts.completion}${parts.after}`;
}

// ── The completion interlock's reconcile (runner.ts phasePublish) ─────────────────────────────

export interface ReconcileArgs {
  forge: PublisherForge;
  repoUrl: string;
  pat: string;
  mrIid: number;
  signal?: AbortSignal;
  /** The completion block to write, with the staleness line for `headSha` when given. */
  completion: (headSha?: string) => string;
  /** uzi's own region for a whole-body rewrite (undefined: the completion block alone). */
  ownRegion: string | undefined;
  /** Set when the block is NON-closing on an issue run: the whole result is scanned for a closing
   *  directive for this issue (amended D10), and a failed read falls back to a blind whole-body
   *  rewrite (which carries no directive by construction). */
  nonClosing?: { issueIid: number; repoPath?: string };
  forgeRetry?: <T>(fn: () => Promise<T>) => Promise<T>;
  log: Pick<Logger, "warn">;
}

/**
 * - `confirmed`: a re-read shows the body carrying EXACTLY the completion block that was written (or,
 *   with nothing to write, the block the read already carried), and, when non-closing, no closing
 *   directive outside it. The block is compared with what was written, never with a block
 *   re-rendered for the head the re-read reports: a head that moved between the write and the
 *   re-read changes the staleness line uzi would render, not what the forge carries.
 * - `blind`: the MR could not be read, so a non-closing whole-body rewrite was written without a
 *   re-read. It carries no closing directive by construction, but nothing confirmed it landed.
 * - `unconfirmed`: nothing is known: the read, the write or its confirmation failed, or the re-read
 *   did not carry the written block.
 * - `closing`: a non-closing reconcile found a closing directive it could not remove.
 */
export type ReconcileResult = "confirmed" | "blind" | "unconfirmed" | "closing";

/**
 * The interlock's reconcile: read, replace ONLY the completion block, write, re-read. Malformed or
 * missing completion markers (and a PR with no markers) get a whole-body rewrite. On a non-closing
 * write the result is scanned (amended D10); a directive uzi does not write forces a whole-body
 * non-closing rewrite, re-read and re-scanned. A failed read on a non-closing write falls back to
 * the blind whole-body rewrite (today's behaviour), reported as `blind`. Never throws.
 */
export async function reconcileCompletion(a: ReconcileArgs): Promise<ReconcileResult> {
  const retry = a.forgeRetry ?? ((fn) => fn());
  const memo = new BodyMemo();
  const read = () => retry(() => a.forge.getMergeRequest(a.repoUrl, a.pat, a.mrIid, a.signal));
  const write = (body: string) => retry(() => a.forge.updateMergeRequestDescription(a.repoUrl, a.pat, a.mrIid, body, a.signal));
  const closingOutside = (body: string, completion: string) =>
    a.nonClosing !== undefined && memo.closingOutside(body, a.nonClosing.issueIid, completion, a.nonClosing.repoPath);
  const blind = async (): Promise<ReconcileResult> => {
    try {
      await write(renderBody(a.ownRegion, a.completion()));
      return "blind";
    } catch (e) {
      a.log.warn("completion interlock: the whole-body rewrite failed", { error: errMsg(e) });
      return "unconfirmed";
    }
  };
  let current: MergeRequestDetail;
  try {
    current = await read();
  } catch (e) {
    a.log.warn("completion interlock: could not read the MR description", { error: errMsg(e) });
    return a.nonClosing ? blind() : "unconfirmed";
  }
  let whole = false;
  let foundDirective = false;
  for (let attempt = 0; attempt < 2; attempt++) {
    // The block this attempt writes, rendered once for the head this read reports. The re-read
    // below is confirmed against THIS block.
    const completion = a.completion(current.headSha);
    let body: string | undefined;
    if (!whole) {
      const parsed = memo.parse(current.description);
      body = parsed.kind === "ok" && parsed.completion !== undefined ? composeBody(parsed, { completion }) : undefined;
      if (body !== undefined && closingOutside(body, completion)) {
        foundDirective = true;
        body = undefined;
      }
    }
    if (body === undefined) {
      whole = true;
      body = renderBody(a.ownRegion, completion);
    }
    if (body !== current.description) {
      try {
        await write(body);
      } catch (e) {
        a.log.warn("completion interlock: could not write the MR description", { error: errMsg(e) });
        return foundDirective ? "closing" : "unconfirmed";
      }
    }
    try {
      current = await read();
    } catch (e) {
      a.log.warn("completion interlock: could not re-read the MR description", { error: errMsg(e) });
      if (!a.nonClosing) return "unconfirmed";
      const r = await blind();
      return r === "blind" || !foundDirective ? r : "closing";
    }
    const carried = memo.carries(current.description, completion);
    const closing = carried && closingOutside(current.description, completion);
    if (carried && !closing) return "confirmed";
    if (closing) foundDirective = true;
    if (!a.nonClosing || whole) break;
    whole = true;
  }
  return foundDirective ? "closing" : "unconfirmed";
}
