// Worker-owned, authenticated message outbox (PRD #1391 Run A / M1, decisions
// D1/D2/D11/D12).
//
// This module owns the STORE half of the outbox primitive: a per-worker tree at
// `<dataDir>/outbox`. When the api is unreachable the batcher (a later unit, M2)
// spills a run's messages here as fsync'd immutable segments; a drainer replays
// them in `seq` order once the api returns and retires each on success. This unit
// builds ONLY the store + its unit tests — there is no production caller yet.
//
// Authentication (D1). Every record carries a domain-separated HMAC-SHA256 keyed
// from a WORKER-LOCAL secret in `<root>/.key` (32 random bytes, minted once,
// installed no-replace + crash-atomic) — NOT derived from the join token, so a
// join-token rotation never strands existing records. A record that fails the MAC
// is rejected and logged; its own run/seq metadata is untrusted, so a gap marker
// is emitted only when a VALID manifest proves which seq range is missing (a bad
// manifest proves nothing).
//
// Immutability (D12). A segment file is never mutated in place. A rewrite (quota
// eviction here; poison tombstoning in a later unit) writes a NEW versioned file,
// installs the next manifest generation (temp -> fsync -> rename -> dir fsync),
// THEN unlinks the old file. The single per-run manifest is the only metadata
// authority: it lists the records, their seq ranges and the replay cursor.
//
// Hosted single-UID residual (D1). Without a UID split the model process shares
// the worker's uid, can read `.key` and can forge, truncate or delete any run's
// outbox; against a hostile model the outbox guarantees neither integrity nor
// durability there. It protects against OUTAGES (and, on the split-UID runtime,
// against the runner), not against the model. This is documented, not hidden.

/** Per-call context; the opaque identity never crosses disk or protocol boundaries. */
export interface ReportRetirementContext {
  expectedIdentity?: Readonly<object>;
  signal?: AbortSignal;
  deadline?: number;
  eligible?: () => boolean;
}

type ProofBudget = Pick<ReportRetirementContext, "signal" | "deadline">;

function checkProofBudget(budget?: ProofBudget): void {
  budget?.signal?.throwIfAborted();
  if (budget?.deadline !== undefined && Date.now() >= budget.deadline) throw new Error("outbox proof deadline");
}

function fileLifetime(stat: { dev: bigint; ino: bigint; birthtimeNs: bigint }): string {
  return `${stat.dev}:${stat.ino}:${stat.birthtimeNs}`;
}

function fileVersion(stat: { dev: bigint; ino: bigint; birthtimeNs: bigint; size: bigint; mtimeNs: bigint; ctimeNs: bigint }): string {
  return `${fileLifetime(stat)}:${stat.size}:${stat.mtimeNs}:${stat.ctimeNs}`;
}

function terminalRetirementPayload(record: Record<string, unknown>): string {
  return canonicalJson({ run: record.run_id, generation: record.claim_generation,
    body: record.body, fence: record.messages_through_seq, phase: record.phase_at_journal });
}

import { createHash, createHmac, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import { constants as fsConstants, type Dir } from "node:fs";
import path from "node:path";

import type { Logger } from "./log.js";
import type { TerminalRejectionCustodyResponse, OutgoingMessage, PolicyHoldRequest, ImmutablePolicyOutcome } from "./protocol.js";
import { validPolicyCorrelation, isPolicyRefusalTag } from "./provider-policy-refusal.js";
import type { DiskFullVerdict } from "./disk-full.js";

/** The worker-local HMAC secret is exactly this many random bytes. */
const OUTBOX_KEY_BYTES = 32;

/** The `.reserve` file's FALLBACK size in bytes — sized for ONE range record (a
 *  few hundred bytes) with generous slack for the JSON + HMAC + the manifest
 *  rewrite it rides with. On `ENOSPC` during a range-record write the single-flight
 *  writer releases it to free the space, writes the range record, then replenishes
 *  it when space returns. This stays the DEFAULT used when no `reserveBytes` is
 *  supplied (Run A behaviour, byte-for-byte); Run B (#1393 M3) threads a larger
 *  terminal-sized reserve through {@link OutboxOptions.reserveBytes}, and it doubles
 *  as the per-terminal overhead slack in {@link deriveTerminalReserveBytes}. */
export const OUTBOX_RANGE_RESERVE_BYTES = 64 * 1024;

/**
 * PRD #1391 M3 (Run B, D2): derive the terminal-sized reserve from ONE source — the
 * per-record cap times how many hard-max terminal journals must be writable on a full
 * volume, plus a per-record overhead slack ({@link OUTBOX_RANGE_RESERVE_BYTES}, which
 * also covers the single range record Run A's reserve was sized for). At the defaults
 * (4 terminals × 1.25 MiB + 4 × 64 KiB) this is about 5.25 MiB. Exported so `main.ts`
 * computes the reserve from the two config knobs with the derivation living beside the
 * const it depends on.
 */
export function deriveTerminalReserveBytes(reserveTerminals: number, terminalMaxBytes: number): number {
  return reserveTerminals * (terminalMaxBytes + OUTBOX_RANGE_RESERVE_BYTES);
}

// Domain-separation labels. Binding a distinct label into each kind's MAC keeps a
// segment MAC unusable as a manifest MAC and vice-versa, so a record cannot be
// replayed as a different kind even under the same key.
const MAC_DOMAIN_SEGMENT = "uzi.outbox.segment.v1";
const MAC_DOMAIN_RANGE = "uzi.outbox.range.v1";
const MAC_DOMAIN_MANIFEST = "uzi.outbox.manifest.v1";
/** PRD #1391 M3 (Run B): the terminal-journal record's domain label, distinct from the
 *  segment/range/manifest labels so a terminal journal's MAC can never be replayed as a
 *  different record kind under the same worker-local key. */
const MAC_DOMAIN_TERMINAL = "uzi.outbox.terminal.v1";
/** Issue #1742: the most finalize_resume entries one register offers: the api's default
 *  ACTIVE_SNAPSHOT_MAX_ENTRIES (it drops the whole list above its cap). */
const FINALIZE_RESUME_MAX_ENTRIES = 256;

/** Issue #1742: the finalize-pending record's own domain label, so its MAC can never be replayed as
 *  a terminal journal (or any other kind) under the same worker-local key. */
const MAC_DOMAIN_FINALIZE = "uzi.outbox.finalize.v1";
const MAC_DOMAIN_POLICY = "uzi.outbox.policy.v1";
const POLICY_FILE_PREFIX = "policy-";
// Independent control admission: never charged to the evictable segment map.
// 256 records, at most 16 KiB each, across this worker. Unknown files consume slots too.
const POLICY_MAX_BYTES = 16 * 1024;
const POLICY_MAX_RECORDS = 256;
const POLICY_MAX_CONTROL_BYTES = POLICY_MAX_BYTES * POLICY_MAX_RECORDS;

const MANIFEST_FILE = "manifest.json";
const KEY_FILE = ".key";
const RESERVE_FILE = ".reserve";
/** PRD #1391 M3 (Run B): the per-run terminal-journal filename, keyed by claim generation
 *  (D4). It is NOT listed in the message manifest — the journal for a generation is its own
 *  first-writer-wins file, `terminal-<claim_generation>.json`. */
const TERMINAL_FILE_PREFIX = "terminal-";
const TERMINAL_FILE_SUFFIX = ".json";
/** Issue #1742: the per-run finalize-pending record filename, `finalize-<claimGeneration>.json`. */
const FINALIZE_FILE_PREFIX = "finalize-";
const FINALIZE_FILE_SUFFIX = ".json";

/** The fixed reason a quota / ENOSPC drop records; replay expands the range into
 *  one status tombstone per seq carrying this reason (batcher.ts:152-180). */
const DROP_REASON = "outbox quota";

/** PRD #1391 M2 / D2: replay a record in bounded, sequence-ordered chunks so a very
 *  wide range record never materialises one huge tombstone array or posts one oversized
 *  request. A message COUNT well under MAX_BATCH_BYTES (tombstones are ~150 B each, so
 *  500 ≈ ~75 KiB, comfortably below the 512 KiB batch cap). */
const OUTBOX_DRAIN_CHUNK = 500;

/** A record's kind in the manifest. A `segment` carries real messages; a `range`
 *  is a compact stand-in for a dropped seq range that replay expands into one
 *  tombstone per seq (D2). */
type RecordFileKind = "segment" | "range";

/** One record listed by the manifest (the on-disk shape). The seq range and the
 *  file version name the file; the generation and the messages live in the file
 *  itself, not here. */
interface ManifestRecordRef {
  kind: RecordFileKind;
  firstSeq: number;
  lastSeq: number;
  fileVersion: number;
  file: string;
}

/** The per-run manifest — the single metadata authority (D12).
 *
 *  `since` and `updatedAt` and `staleRetired` are carried in addition to the
 *  fields named in the PRD's manifest sketch because the public API requires
 *  durable per-run state that has nowhere else to live: {@link Outbox.depthFor}
 *  reports `since`, {@link Outbox.sweepRetention} ages an empty run off
 *  `updatedAt` (with the test `now` injector, never an fs mtime), and D11's
 *  stale-retired counter is "kept in the run's manifest". */
interface ManifestData {
  version: 1;
  runId: string;
  /** Bumped on every manifest rewrite (each append, eviction, retire, spill-flag
   *  change). Monotonic; survives restart. */
  generation: number;
  records: ManifestRecordRef[];
  /** Last retired seq — a record whose `lastSeq <= cursor` has been replayed. */
  cursor: number;
  spilledUnclean: boolean;
  /** Cumulative count of seqs retired LOCALLY under a stale claim (D11). */
  staleRetired: number;
  /** Epoch ms the run's outbox was created. */
  since: number;
  /** Epoch ms of the last manifest write (retention age reference). */
  updatedAt: number;
}

/** PRD #1391 M3 (Run B, D13): the CLOSED set of reasons a pending terminal journal can be
 *  marked permanently blocked with. `completion_permit_mismatch` — the api refused the
 *  transition against a released/superseded completion permit; `gap_unrecoverable` — the
 *  message-gap fill hit `WORKER_GAP_FILL_MAX` and the api still refuses; `reserve_exhausted`
 *  — the terminal-sized reserve could not admit the journal, so it was sent unjournaled. The
 *  worker surfaces the reason on the heartbeat outbox entry, and the owner resolves it (never a
 *  timer). Exported so M3b's send path names exactly these. Issue #2950 adds `run_not_owned` —
 *  the api answered the structured not-owned 404 (`run_not_owned`) to the /state report, which is
 *  permanent for that claim generation; the journal is kept byte-intact, never resent and never
 *  auto-retired, and the owner/operator resolves it. */
export type TerminalBlockedReason =
  | "completion_permit_mismatch"
  | "gap_unrecoverable"
  | "reserve_exhausted"
  | "run_not_owned";

/** PRD #1391 M3 (Run B): the outcome of {@link Outbox.journalTerminal}. A discriminated union so a
 *  caller (M3b's send path) never confuses "installed durably" with "sent unjournaled": on success
 *  the outcome is journalled (and `adopted` says whether THIS call installed it or adopted an
 *  earlier first-writer for the same generation, D4); on `reserve_exhausted` the terminal-sized
 *  reserve could not admit it, so the caller sends unjournaled + logs, never silently dropping.
 *  `winner_unavailable` retains the selected first writer and defers a competing outcome. */
export type TerminalJournalResult =
  | { journaled: true; adopted: boolean }
  | { journaled: false; reason: "reserve_exhausted" | "winner_unavailable" };

/** PRD #1391 M3 (Run B): one pending terminal journal listed by {@link Outbox.listPendingTerminals}
 *  and folded into the heartbeat outbox entry — the phase captured at journal time (`phase_at_journal`,
 *  what #1390's snapshot reports after a restart), whether the api has permanently refused it
 *  (`blocked` + `blocked_reason`, D13), and when it was installed. */
export interface PendingTerminal {
  run_id: string;
  claim_generation: number;
  phase: string;
  blocked: boolean;
  blocked_reason?: TerminalBlockedReason;
  since: number;
  /** Process-local exact journal observation, never persisted or deletion authority. */
  server_settled?: boolean;
}

/** PRD #1391 M3b: a pending terminal journal's durable payload, returned by
 *  {@link Outbox.readTerminalJournal} for the send path — the already-canonical report `body`, its
 *  `messagesThroughSeq` fence and the phase captured at journal time (`phase_at_journal`), plus its
 *  blocked state (D13). The M3b `resolvePendingTerminal` send path sends `body` verbatim, adding the
 *  fence only when the api advertises `terminal_fence` (D9). */
export interface PendingTerminalJournal {
  body: Record<string, unknown>;
  messagesThroughSeq: number;
  phase: string;
  blocked: boolean;
  blockedReason?: TerminalBlockedReason;
}

/** The on-disk terminal-journal record (D4). Installed crash-atomically and EXCLUSIVELY
 *  (first durable writer for a generation wins), authenticated with {@link MAC_DOMAIN_TERMINAL}.
 *  `body` is the ALREADY-canonical report body (M3b canonicalises before journalling AND before
 *  the first send, so the two are byte-identical); `blocked`/`blocked_reason` are updated in place
 *  by {@link Outbox.markTerminalBlocked} once the api permanently refuses (that is a legitimate
 *  owner update, not a competing first-writer). */
interface TerminalJournalData {
  version: 1;
  run_id: string;
  claim_generation: number;
  phase_at_journal: string;
  messages_through_seq: number;
  body: Record<string, unknown>;
  since: number;
  /** Lifecycle marker; only `installed` today (a retire unlinks the file rather than restamping). */
  state: "installed";
  blocked: boolean;
  blocked_reason?: TerminalBlockedReason;
}

/** In-memory metadata for one pending terminal journal (the file's durable fields minus the body,
 *  which the store never needs to re-read for depth/list/retire). */
interface TerminalMeta {
  fileName: string;
  claimGeneration: number;
  phaseAtJournal: string;
  since: number;
  blocked: boolean;
  blockedReason?: TerminalBlockedReason;
}

/** In-memory working state for one run: its authenticated manifest plus a byte
 *  map (file -> size) for quota accounting, so the quota check never re-stats, and
 *  the run's pending terminal journals keyed by claim generation (D4). */
interface RunState {
  runId: string;
  manifest: ManifestData;
  recordBytes: Map<string, number>;
  terminals: Map<number, TerminalMeta>;
  /** Issue #1742: the run's pending finalize-pending records keyed by claim generation. */
  finalizes: Map<number, FinalizeMeta>;
  policies: Map<number, PolicyMeta>;
  policyProtection: boolean;
}

export interface PendingPolicy {
  run_id: string;
  outcome: ImmutablePolicyOutcome;
  delivery: Readonly<{ claim_generation: number; inventory_generation: number }>;
  since: number;
  server_settled?: true;
}

interface PolicyRecord {
  version: 1;
  run_id: string;
  outcome: ImmutablePolicyOutcome;
  delivery: { claim_generation: number; inventory_generation: number };
  since: number;
}

interface PolicyMeta {
  record: PolicyRecord;
  payload: string;
  lifetime: string;
  fileVersion: string;
  identity: Readonly<object>;
  serverSettled?: true;
}

type PolicyBindingResult =
  | { bound: true; winner: PendingPolicy; context: ReportRetirementContext }
  | { bound: false; reason: string };

export type PolicyJournalResult =
  | { journaled: true; written: boolean; winner: PendingPolicy; context: ReportRetirementContext }
  | { journaled: false; reason: string };

/** Proof covers every original-through-current inventory generation. No source is deleted here.
 * The coordinator obtains fresh source/custody proof outside O, under its existing lock order. */
export interface PolicyRetirementContext extends ReportRetirementContext {
  freshProof?: (observation: PendingPolicy, budget: { signal: AbortSignal; deadline: number }) => Promise<{
    outcome_id: string;
    source_generation: number;
    claim_generation: number;
    inventory_complete: boolean;
    sources_safe: boolean;
    custody_settled: boolean;
  } | undefined>;
}

/** Issue #1742: in-memory metadata for one finalize-pending record. It records only that the
 *  executor of this generation returned success and the worker had begun finalizing; it is never an
 *  outcome and never a terminal journal. */
interface FinalizeMeta {
  claimGeneration: number;
  since: number;
  lifetime?: string;
}

/** Issue #1742: one pending finalize record listed by {@link Outbox.listPendingFinalizes}. */
export interface PendingFinalize {
  run_id: string;
  claim_generation: number;
}

/** Issue #1742: the outcome of {@link Outbox.journalFinalize}. It never throws; a failure is a
 *  `written:false` with a short reason and the run continues exactly as it did before the record. */
export interface FinalizeJournalResult {
  written: boolean;
  reason?: string;
}

/** The on-disk finalize record: identity and time only, no status, report body or phase. */
interface FinalizeRecordData {
  run_id: string;
  claim_generation: number;
  since: number;
}

/** The per-run outbox depth surfaced on the heartbeat (M5) and in `uzi admin
 *  workers`. `pendingTerminal` counts the run's pending (installed, un-retired)
 *  terminal journals (Run B / M3); `blockedReason` is the run's oldest blocked
 *  terminal reason, if any (D13). Both stay 0/unset for a run with no terminal
 *  journal, so a Run A worker's depth is unchanged. */
export interface OutboxDepth {
  runId: string;
  pendingMessages: number;
  pendingTerminal: number;
  pendingPolicy?: number;
  staleRetired: number;
  blockedReason?: string;
  since: number;
}

/** A test/production seam around the one raw write that can hit `ENOSPC` (the
 *  temp-file write of a record). The default just calls `write`; a test injects a
 *  seam that throws `ENOSPC` to exercise the reserve path. */
export type RawWriteSeam = (
  write: () => Promise<void>,
  ctx: { path: string; kind: RecordFileKind | "manifest" | "terminal" | "finalize" | "policy" },
) => Promise<void>;

/** Content-free physical evidence. Unknown files never enter the pending inventory. */
export interface TerminalAuthenticationObservation {
  runId: string;
  /** Exact physical directory name; report runId is normalized separately. */
  physicalRunId?: string;
  generation?: number;
  fileName: string;
  kind: "authenticated" | "mac_failure" | "absent" | "malformed" | "symlink" | "unreadable" | "key_unavailable" | "oversized";
  fingerprint?: { dev: string; ino: string; sha256: string };
}

export interface TerminalObservationPage {
  observations: TerminalAuthenticationObservation[];
  passComplete: boolean;
}

export interface OutboxOptions {
  /** The outbox root, `<dataDir>/outbox`. */
  root: string;
  log: Logger;
  /** Per-run byte quota; the oldest segment of the run is evicted when it binds. */
  runMaxBytes: number;
  /** Worker-total byte quota; the globally-oldest segment is evicted when it binds. */
  maxBytes: number;
  /** Retention window for an empty (fully-retired) run. */
  retentionMs: number;
  /**
   * PRD #1391 M3 (Run B, D2): the physical `.reserve` size in bytes. When supplied the reserve is
   * GROWN to at least this at init (so a deployed Run A worker with a 64 KiB reserve comes up with
   * the larger terminal-sized reserve). Omitted ⇒ {@link OUTBOX_RANGE_RESERVE_BYTES}, preserving
   * Run A's one-range-record reserve byte-for-byte. Derive it from the config knobs with
   * {@link deriveTerminalReserveBytes}.
   */
  reserveBytes?: number;
  /** Bound terminal reads before JSON; defaults to the config's 1.25 MiB cap. */
  terminalMaxBytes?: number;
  now?: () => number;
  /** Optional canonical-path seam for terminal directory safety checks (tests). */
  realpath?: (filePath: string) => Promise<string>;
  /** Optional raw-write seam to simulate `ENOSPC` (tests). */
  rawWrite?: RawWriteSeam;
  /**
   * PRD #1809 D6: classify a failed `.reserve` (re)write as data-volume disk-full (production:
   * DataVolumeGuard.classify, passed the outbox root as the destination). On a full verdict the
   * warning carries `cause: "data_volume_full"`. Only the log changes: the outbox keeps its
   * custody, and a park goes through the claim-fenced state call, never through the outbox.
   */
  classifyWriteFailure?: (err: unknown, destination: string) => Promise<DiskFullVerdict>;
}

/**
 * Thrown by a drain `send` callback when the api refuses a segment because the run
 * was re-claimed before replay completed (a generation bump under #1247). The
 * drainer retires such a record LOCALLY and counts it in `staleRetired`; it never
 * rebinds the frames to a new generation (D11). Dormant in Run A (nothing raises
 * it until #1390 leases the pending set), but implemented so M2 can wire it.
 */
export class StaleClaimError extends Error {
  constructor(message = "stale claim") {
    super(message);
    this.name = "StaleClaimError";
  }
}

/** Stable, key-sorted JSON so a MAC is deterministic regardless of insertion
 *  order. Module-local by design — recovery.ts keeps its own copy; duplicating a
 *  tiny pure helper is cheaper than an exported dependency knip would police. */
function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map((v) => canonicalJson(v)).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).sort();
  const parts: string[] = [];
  for (const k of keys) {
    if (obj[k] === undefined) continue; // omit undefined so an absent optional is stable
    parts.push(`${JSON.stringify(k)}:${canonicalJson(obj[k])}`);
  }
  return `{${parts.join(",")}}`;
}

/** Constant-time compare of two hex MACs of equal length. */
function macEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  const ba = Buffer.from(a, "hex");
  const bb = Buffer.from(b, "hex");
  if (ba.length !== bb.length || ba.length === 0) return false;
  return timingSafeEqual(ba, bb);
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function isENOSPC(err: unknown): boolean {
  return (err as NodeJS.ErrnoException | undefined)?.code === "ENOSPC";
}

/** Number of seqs a record covers (also the number of tombstones a range expands
 *  into and the frame count depthFor reports for a segment). */
function seqCount(rec: Pick<ManifestRecordRef, "firstSeq" | "lastSeq">): number {
  return rec.lastSeq - rec.firstSeq + 1;
}

async function pathExists(p: string): Promise<boolean> {
  try {
    await fs.access(p);
    return true;
  } catch {
    return false;
  }
}

/**
 * The worker-owned authenticated message outbox store (PRD #1391 M1). Build it,
 * `init()` it (loads/mints the key, scans runs, computes byte accounting), then
 * `appendSegment` / `appendRangeRecord` to spill and `drainRun` to replay.
 */
export class Outbox {
  private readonly root: string;
  private readonly log: Logger;
  private readonly runMaxBytes: number;
  private readonly maxBytes: number;
  private readonly retentionMs: number;
  /** The configured `.reserve` byte size (Run B's terminal-sized reserve, or the
   *  Run A default when no `reserveBytes` was supplied). */
  private readonly reserveBytes: number;
  private policyReserveEnabled = false;
  private get reserveTargetBytes(): number {
    // Separate policy install/rebind headroom; releasing it never spends terminal fallback capacity.
    return this.reserveBytes + (this.policyReserveEnabled ? 2 * POLICY_MAX_BYTES + OUTBOX_RANGE_RESERVE_BYTES : 0);
  }
  private policyAdmissionTail: Promise<unknown> = Promise.resolve();
  private readonly terminalReadMaxBytes: number;
  private terminalScanRoot?: Dir;
  private terminalScanRun?: { id: string; dir: Dir };
  private terminalScanTail: Promise<unknown> = Promise.resolve();
  private readonly now: () => number;
  private readonly rawWrite: RawWriteSeam;
  private readonly realpath: (filePath: string) => Promise<string>;
  private readonly classifyWriteFailure: OutboxOptions["classifyWriteFailure"];

  private readonly runs = new Map<string, RunState>();
  /** Process-local receipt identities; never persisted or reused for later appends. */
  private readonly deliveryIdentities = new WeakMap<ManifestRecordRef, symbol>();
  private readonly uncleanAtInit: string[] = [];
  /** Per-run promise-chain mutex tails: every mutating op on one run awaits and
   *  extends its run's tail, so a run's ops run strictly one-at-a-time (different
   *  runs stay concurrent). Closes the read-modify-write race in installManifest
   *  where two mutating calls would read the same generation and lose one. */
  private readonly runLocks = new Map<string, Promise<void>>();
  private key: Buffer | undefined;
  /** Set when the store fails closed (key missing/unreadable with records
   *  present, or the root is a symlink): every write is a no-op and drain
   *  replays nothing. */
  private disabled = false;

  constructor(opts: OutboxOptions) {
    this.root = path.resolve(opts.root);
    const terminalMax = opts.terminalMaxBytes ?? Math.round(1.25 * 1024 * 1024);
    if (!Number.isSafeInteger(terminalMax) || terminalMax <= 0 || terminalMax > Number.MAX_SAFE_INTEGER - OUTBOX_RANGE_RESERVE_BYTES - 1) throw new Error("invalid terminal cap");
    this.terminalReadMaxBytes = terminalMax + OUTBOX_RANGE_RESERVE_BYTES;
    this.log = opts.log;
    this.runMaxBytes = opts.runMaxBytes;
    this.maxBytes = opts.maxBytes;
    this.retentionMs = opts.retentionMs;
    // A non-positive override would defeat the reserve; fall back to the Run A default.
    this.reserveBytes =
      opts.reserveBytes !== undefined && Number.isInteger(opts.reserveBytes) && opts.reserveBytes > 0
        ? opts.reserveBytes
        : OUTBOX_RANGE_RESERVE_BYTES;
    this.now = opts.now ?? (() => Date.now());
    this.rawWrite = opts.rawWrite ?? ((write) => write());
    this.realpath = opts.realpath ?? ((filePath) => fs.realpath(filePath));
    this.classifyWriteFailure = opts.classifyWriteFailure;
  }

  // ── lifecycle ───────────────────────────────────────────────────────────────

  /**
   * Load or mint the key, scan the run tree, compute byte accounting, and capture
   * the spill-unclean flags. Idempotent enough to call once at startup. On a
   * fail-closed condition the store is left disabled (logged) and every later
   * method no-ops.
   */
  async init(): Promise<void> {
    try {
      await fs.mkdir(this.root, { recursive: true, mode: 0o700 });
      if (await this.isSymlink(this.root)) {
        this.log.error("outbox: root is a symlink; disabling (following it would escape /data)", {
          path: this.root,
        });
        this.disabled = true;
        return;
      }
    } catch (err) {
      this.log.error("outbox: cannot create root; disabling", { error: errText(err) });
      this.disabled = true;
      return;
    }

    // Enumerate run-dir candidates, refusing any symlink outright (never followed).
    const runDirs: string[] = [];
    let entries: Array<{ name: string; isDir: boolean; isLink: boolean }>;
    try {
      const dirents = await fs.readdir(this.root, { withFileTypes: true });
      entries = dirents.map((d) => ({
        name: d.name,
        isDir: d.isDirectory(),
        isLink: d.isSymbolicLink(),
      }));
    } catch (err) {
      this.log.error("outbox: cannot read root; disabling", { error: errText(err) });
      this.disabled = true;
      return;
    }
    for (const e of entries) {
      if (e.name.startsWith(".")) continue; // .key / .reserve / stray dotfiles
      if (e.isLink) {
        this.log.warn("outbox: refusing symlinked outbox entry (skipped, not followed)", {
          name: e.name,
        });
        continue;
      }
      if (e.isDir) runDirs.push(e.name);
    }

    for (const runId of runDirs) await this.cleanupTerminalInstallTemps(runId);

    // Does any run carry a manifest? Presence of the FILE decides the key policy,
    // regardless of whether that manifest authenticates (a bad manifest still
    // proves records once existed, so minting a fresh key would orphan its MACs).
    let hasRecords = false;
    for (const runId of runDirs) {
      if (await pathExists(path.join(this.runDir(runId), MANIFEST_FILE))) {
        hasRecords = true;
        break;
      }
    }

    // Physical inventory is independent of authentication. Dirents classify entries without
    // following symlinks; only aggregate counts leave this method.
    let physicalJournals = 0;
    let symlinkJournals = 0;
    let otherJournalEntries = 0;
    let unreadableRunDirectories = 0;
    for (const runId of runDirs) {
      try {
        for (const entry of await fs.readdir(this.runDir(runId), { withFileTypes: true })) {
          if (entry.name.startsWith(POLICY_FILE_PREFIX)) {
            hasRecords = true;
            this.ensureInMemoryRun(runId, this.now()).policyProtection = true;
          }
          if (parseTerminalFileName(entry.name) === undefined) continue;
          if (entry.isSymbolicLink()) symlinkJournals++;
          else if (entry.isFile()) physicalJournals++;
          else otherJournalEntries++;
        }
      } catch {
        unreadableRunDirectories++;
      }
    }
    this.log.info("outbox terminal journal inventory", {
      physical_files: physicalJournals,
      symlink_entries: symlinkJournals,
      other_entries: otherJournalEntries,
      run_directories: runDirs.length,
      unreadable_run_directories: unreadableRunDirectories,
    });

    if (!(await this.loadOrMintKey(hasRecords))) {
      this.log.warn("outbox terminal journal load", { outcome: "key_unavailable", authenticated_pending: 0 });
      return;
    }

    // Load every run's manifest (authenticated) and stat its files for accounting,
    // then adopt any pending terminal journals (Run B / M3) — a run can carry a
    // terminal journal with no message manifest, so this loads them independently.
    let authenticated = 0;
    let rejected = 0;
    for (const runId of runDirs) {
      await this.loadRun(runId);
      const result = await this.loadTerminals(runId);
      authenticated += result.authenticated;
      rejected += result.rejected;
      await this.loadFinalizes(runId);
      await this.loadPolicies(runId);
    }

    this.log.info("outbox terminal journal load", {
      outcome: "complete",
      authenticated_pending: this.listPendingTerminals().length,
      authenticated_files: authenticated,
      rejected_files: rejected,
      physical_files: physicalJournals,
    });

    // A best-effort preallocated reserve so a range record — and, with a terminal-sized
    // reserve, the next few terminal journals — can still be written on a full volume
    // (released on ENOSPC, then replenished). GROWS an undersized existing `.reserve`.
    await this.ensureReserve();
  }

  /** Boot only: init finishes before writers start. The run mutex is instance-local,
   *  not cross-process; another Outbox must not write this root during init.
   *  Runtime physical terminal protection remains conservative. */
  private async cleanupTerminalInstallTemps(runId: string): Promise<void> {
    if (!this.validRunId(runId)) return;
    await this.withRunLock(runId, async () => {
      let removed = false;
      try {
        if (!await this.terminalDirectorySafe(runId)) return;
        const dir = this.runDir(runId);
        // One finite directory snapshot, no retries. A failure stops this run's
        // cleanup; sibling run directories still get their own attempt.
        for (const entry of await fs.readdir(dir, { withFileTypes: true })) {
          if (!entry.isFile()) continue;
          const match = /^(terminal-\d+\.json)\.[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.tmp$/.exec(entry.name);
          if (!match?.[1]) continue;
          const generation = parseTerminalFileName(match[1]);
          if (generation === undefined || match[1] !== terminalFileName(generation)) continue;
          const file = path.join(dir, entry.name);
          if (!(await fs.lstat(file)).isFile()) continue;
          await fs.unlink(file);
          removed = true;
        }
      } catch {
        // Do not include paths, names, contents or filesystem error text.
        this.log.warn("outbox: terminal temp startup cleanup failed", {});
      } finally {
        if (removed) await this.fsyncDir(this.runDir(runId));
      }
    });
  }

  private async loadOrMintKey(hasRecords: boolean): Promise<boolean> {
    const keyPath = path.join(this.root, KEY_FILE);
    if (await this.isSymlink(keyPath)) {
      this.log.error("outbox: .key is a symlink; disabling", { path: keyPath });
      this.disabled = true;
      return false;
    }
    if (await pathExists(keyPath)) {
      try {
        this.key = await this.readKeyFile(keyPath);
        return true;
      } catch (err) {
        // Unreadable/wrong-size key: fail closed rather than mint over it and
        // orphan every existing MAC.
        this.log.error("outbox: .key present but unreadable; failing closed (replaying/writing nothing)", {
          error: errText(err),
        });
        this.disabled = true;
        return false;
      }
    }
    if (hasRecords) {
      // Key-missing-but-records-present: minting a new key would orphan the MACs
      // of every existing record, so refuse (D1 / M1).
      this.log.error("outbox: .key absent but records present; failing closed (replaying/writing nothing)", {
        root: this.root,
      });
      this.disabled = true;
      return false;
    }
    try {
      this.key = await this.mintKey(keyPath);
      return true;
    } catch (err) {
      this.log.error("outbox: could not mint .key; disabling", { error: errText(err) });
      this.disabled = true;
      return false;
    }
  }

  private async loadRun(runId: string): Promise<void> {
    const manifestPath = path.join(this.runDir(runId), MANIFEST_FILE);
    const parsed = await this.readAuthed(manifestPath, MAC_DOMAIN_MANIFEST);
    if (!parsed) return; // absent (nothing to load) or tampered (readAuthed logged)
    const manifest = coerceManifest(parsed);
    if (!manifest || manifest.runId !== runId) {
      this.log.warn("outbox: malformed or cross-run manifest; skipping run", { run_id: runId });
      return;
    }
    const recordBytes = new Map<string, number>();
    for (const rec of manifest.records) {
      try {
        const st = await fs.stat(path.join(this.runDir(runId), rec.file));
        recordBytes.set(rec.file, st.size);
      } catch {
        recordBytes.set(rec.file, 0); // referenced file missing/gone; accounts as 0
      }
    }
    this.runs.set(runId, { runId, manifest, recordBytes, terminals: new Map(), finalizes: new Map(), policies: new Map(), policyProtection: false });
    if (manifest.spilledUnclean) this.uncleanAtInit.push(runId);
  }

  /** Adopt a run's pending terminal journals (`terminal-<generation>.json`) at init (D4). A
   *  journal is authenticated and bound to its directory (embedded `run_id` must match) and its
   *  filename generation (the file names the generation, so a copied journal cannot masquerade
   *  as another generation). A run may carry a terminal journal with NO message manifest, so this
   *  creates the in-memory run entry when loadRun did not. */
  private async loadTerminals(runId: string): Promise<{ authenticated: number; rejected: number }> {
    const result = { authenticated: 0, rejected: 0 };
    if (!this.validRunId(runId)) return result;
    const dir = this.runDir(runId);
    let names: string[];
    try {
      names = await fs.readdir(dir);
    } catch {
      return result;
    }
    for (const name of names) {
      const gen = parseTerminalFileName(name);
      if (gen === undefined) continue;
      const parsed = await this.readTerminalAuthed(runId, gen, name);
      if (!parsed) {
        result.rejected++;
        continue; // absent/symlink/unparseable/MAC-bad (readAuthed logged)
      }
      const meta = coerceTerminal(parsed, runId, gen, name);
      if (!meta) {
        this.log.warn("outbox: malformed or misfiled terminal journal; skipping", { ...(terminalRunUUID(runId) ? { run_id: runId.toLowerCase() } : {}), claim_generation: gen, fact: "malformed" });
        result.rejected++;
        continue;
      }
      const rs = this.ensureInMemoryRun(runId, meta.since);
      rs.terminals.set(meta.claimGeneration, meta);
      result.authenticated++;
    }
    return result;
  }

  /** Issue #1742: adopt a run's authenticated finalize-pending records (`finalize-<generation>.json`)
   *  at init. An unauthenticated, unparseable, misfiled or symlinked record is ignored: not loaded,
   *  left on disk, one warn. The filename is the source of truth for the generation. */
  private async loadFinalizes(runId: string): Promise<void> {
    if (!this.validRunId(runId)) return;
    const dir = this.runDir(runId);
    let names: string[];
    try {
      names = await fs.readdir(dir);
    } catch {
      return;
    }
    for (const name of names) {
      const gen = parseFinalizeFileName(name);
      if (gen === undefined) continue;
      const parsed = await this.readAuthed(path.join(dir, name), MAC_DOMAIN_FINALIZE);
      const meta = parsed ? coerceFinalize(parsed, runId, gen) : null;
      if (!meta) {
        this.log.warn("outbox: unauthenticated or misfiled finalize record ignored (left on disk)", {
          run_id: runId,
          file: name,
        });
        continue;
      }
      meta.lifetime = this.authenticatedLifetimes.get(parsed!);
      this.ensureInMemoryRun(runId, meta.since).finalizes.set(meta.claimGeneration, meta);
    }
  }

  // ── nonterminal policy control records ───────────────────────────────────────

  /** One finite physical scan per admission. Errors, aliases, exhaustion and unknown
   * records retain custody and reject new admission; sibling records are never evicted. */
  private async policyCapacityAvailable(extraBytes: number): Promise<boolean> {
    let count = 0;
    let bytes = 0;
    let scanned = 0;
    try {
      const root = await fs.opendir(this.root);
      try {
        for await (const run of root) {
          if (++scanned > 16384) return false;
          if (run.name.startsWith(".")) continue;
          if (!run.isDirectory() || run.isSymbolicLink() || !await this.terminalDirectorySafe(run.name)) return false;
          const dir = await fs.opendir(this.runDir(run.name));
          for await (const entry of dir) {
            if (++scanned > 16384) return false;
            if (!entry.name.startsWith(POLICY_FILE_PREFIX)) continue;
            const stat = await fs.lstat(path.join(this.runDir(run.name), entry.name));
            if (!stat.isFile() || stat.isSymbolicLink() || stat.size > POLICY_MAX_BYTES) return false;
            count++;
            bytes += stat.size;
            if (count >= POLICY_MAX_RECORDS || bytes + extraBytes > POLICY_MAX_CONTROL_BYTES) return false;
          }
        }
      } finally { /* for-await closes directory handles, including early returns */ }
      return count < POLICY_MAX_RECORDS && bytes + extraBytes <= POLICY_MAX_CONTROL_BYTES;
    } catch { return false; }
  }

  private async policyBlocksFinalize(runId: string, budget?: ProofBudget): Promise<boolean> {
    // Finalize bookkeeping can retire independently of retained source evidence, but only
    // after a complete reauthentication has ruled out unknown or replaced policy controls.
    checkProofBudget(budget);
    await this.loadPolicies(runId, budget);
    checkProofBudget(budget);
    return this.hasUnresolvedPolicy(runId);
  }

  /** One scan of at most 16384 entries; failed authentication protects the run
   * while sibling entries continue. Directory failure or exhaustion retains protection. */
  private async loadPolicies(runId: string, budget?: ProofBudget): Promise<void> {
    if (!this.validRunId(runId)) return;
    let protection = false;
    const authenticated = new Set<number>();
    try {
      if (!await this.terminalDirectorySafe(runId)) throw new Error("unsafe policy directory");
      const dir = await fs.opendir(this.runDir(runId));
      let scanned = 0;
      for await (const entry of dir) {
        checkProofBudget(budget);
        if (++scanned > 16384) {
          protection = true;
          break;
        }
        if (!entry.name.startsWith(POLICY_FILE_PREFIX)) continue;
        this.policyReserveEnabled = true;
        this.ensureInMemoryRun(runId, this.now());
        const generation = parsePolicyFileName(entry.name);
        if (generation === undefined || !await this.authenticatePolicy(runId, generation, budget)) protection = true;
        else authenticated.add(generation);
      }
      if (!await this.terminalDirectorySafe(runId)) protection = true;
    } catch { protection = true; }
    // Publish absence of unknown controls only after the complete anchored scan.
    const run = this.runs.get(runId);
    if (run) run.policyProtection = protection || [...run.policies.keys()].some(source => !authenticated.has(source));
    else if (protection) this.ensureInMemoryRun(runId, this.now()).policyProtection = true;
  }

  /** Reauthentication is the only route to an adopted winner or a fresh opaque identity. */
  private async authenticatePolicy(runId: string, source: number, budget?: ProofBudget): Promise<PolicyMeta | undefined> {
    const rs = this.runs.get(runId);
    const previous = rs?.policies.get(source);
    if (this.disabled || !this.validRunId(runId) || !positiveSafe(source)) return undefined;
    const fail = async () => {
      if (previous) {
        previous.serverSettled = undefined;
        previous.identity = Object.freeze({});
      }
      // Keep the last authenticated logical evidence until an authorized exact unlink.
      // A later unrelated scan or successful write cannot explain its disappearance.
      // A failed read is not absence. Probe only through the anchored filesystem
      // check, and preserve protection for both stale metadata and unknown evidence.
      if (previous || !await this.terminalPositivelyAbsent(runId, policyFileName(source))) {
        this.ensureInMemoryRun(runId, this.now()).policyProtection = true;
      }
      return undefined;
    };
    try {
      if (!await this.terminalDirectorySafe(runId, budget)) return fail();
      const parsed = await this.readAuthed(path.join(this.runDir(runId), policyFileName(source)), MAC_DOMAIN_POLICY, budget);
      const record = parsed && coercePolicyRecord(parsed, runId, source);
      const lifetime = parsed && this.authenticatedLifetimes.get(parsed);
      const version = parsed && this.authenticatedFileVersions.get(parsed);
      if (!record || !lifetime || !version) return fail();
      // Also attest adoption after an earlier install/rename whose fsync failed.
      await this.fsyncDir(this.runDir(runId), true);
      if (!await this.terminalDirectorySafe(runId, budget) ||
          !await this.installedLifetimeMatches(path.join(this.runDir(runId), policyFileName(source)), lifetime, budget, version)) return fail();
      checkProofBudget(budget);
      const payload = canonicalJson(record);
      if (previous && previous.payload === payload && previous.lifetime === lifetime && previous.fileVersion === version) return previous;
      let count = 0;
      for (const run of this.runs.values()) count += run.policies.size;
      if (!previous && count >= POLICY_MAX_RECORDS) return fail();
      const meta: PolicyMeta = { record, payload, lifetime, fileVersion: version, identity: Object.freeze({}) };
      const run = this.ensureInMemoryRun(runId, record.since);
      run.policies.set(source, meta);
      return meta;
    } catch { return fail(); }
  }

  private pendingPolicy(meta: PolicyMeta): PendingPolicy {
    return {
      run_id: meta.record.run_id, outcome: Object.freeze({ ...meta.record.outcome }),
      delivery: Object.freeze({ ...meta.record.delivery }), since: meta.record.since,
      ...(meta.serverSettled ? { server_settled: true as const } : {}),
    };
  }

  /** A failure supplies no send request. Different UUIDs return the actual authenticated winner. */
  async journalPolicy(runId: string, outcome: ImmutablePolicyOutcome): Promise<PolicyJournalResult> {
    if (!this.writable() || !this.validRunId(runId) || !validPolicyOutcome(outcome)) return { journaled: false, reason: "invalid_or_disabled" };
    // Snapshot caller-owned data before waiting for either lock.
    const record: PolicyRecord = { version: 1, run_id: runId, outcome: { ...outcome },
      delivery: { claim_generation: outcome.source_generation, inventory_generation: outcome.source_generation }, since: this.now() };
    const operation = this.policyAdmissionTail.then(() => this.withRunLock(runId, async (): Promise<PolicyJournalResult> => {
      try {
        const source = record.outcome.source_generation;
        const dst = path.join(this.runDir(runId), policyFileName(source));
        const serialized = this.seal(MAC_DOMAIN_POLICY, record);
        if (Buffer.byteLength(serialized) > POLICY_MAX_BYTES) return { journaled: false, reason: "record_too_large" };
        const existing = await this.authenticatePolicy(runId, source);
        if (existing) {
          await this.fsyncDir(this.runDir(runId), true);
          await this.loadPolicies(runId);
          return { journaled: true, written: false, winner: this.pendingPolicy(existing), context: { expectedIdentity: existing.identity } };
        }
        if (!await this.terminalPositivelyAbsent(runId, policyFileName(source))) {
          this.ensureInMemoryRun(runId, record.since).policyProtection = true;
          return { journaled: false, reason: "winner_unavailable" };
        }
        if (!await this.policyCapacityAvailable(Buffer.byteLength(serialized))) return { journaled: false, reason: "control_capacity" };
        this.policyReserveEnabled = true;
        await this.ensureReserve();
        if ((await fs.lstat(this.reservePath())).size < this.reserveTargetBytes) return { journaled: false, reason: "reserve_unavailable" };
        this.ensureInMemoryRun(runId, record.since);
        const adopted = await this.withReserveOnEnospc(async () => {
          await this.rawWrite(() => fs.mkdir(this.runDir(runId), { recursive: true, mode: 0o700 }).then(() => undefined),
            { path: this.runDir(runId), kind: "policy" });
          if (!await this.terminalDirectorySafe(runId)) throw new Error("unsafe policy directory");
          await this.fsyncDir(this.root, true);
          return this.installExclusive(dst, serialized, "policy");
        }, this.reserveBytes);
        const winner = await this.authenticatePolicy(runId, source);
        if (!winner) return { journaled: false, reason: "winner_unavailable" };
        await this.loadPolicies(runId);
        return { journaled: true, written: !adopted, winner: this.pendingPolicy(winner), context: { expectedIdentity: winner.identity } };
      } catch {
        this.ensureInMemoryRun(runId, record.since).policyProtection = true;
        return { journaled: false, reason: "not_durable" };
      }
    }));
    this.policyAdmissionTail = operation.then(() => undefined, () => undefined);
    return operation;
  }

  listPendingPolicies(runId?: string): PendingPolicy[] {
    const entries: PendingPolicy[] = [];
    for (const rs of this.runs.values()) {
      if (runId !== undefined && rs.runId !== runId) continue;
      for (const meta of rs.policies.values()) entries.push(this.pendingPolicy(meta));
    }
    return entries.sort((a, b) => a.run_id.localeCompare(b.run_id) || a.outcome.source_generation - b.outcome.source_generation);
  }

  hasPendingPolicy(runId: string): boolean {
    const rs = this.runs.get(runId);
    return !!rs && (rs.policyProtection || rs.policies.size > 0);
  }

  /** Boot refresh precedes the one-shot finalize snapshot. A complete scan is needed even
   * when all cached policies were ACKed: unknown physical siblings still block admission. */
  async refreshPolicyInventory(signal?: AbortSignal): Promise<void> {
    // oxlint-disable-next-line unicorn/no-useless-spread -- Freeze a bounded set across awaited filesystem scans.
    for (const runId of [...this.runs.keys()]) {
      signal?.throwIfAborted();
      await this.withRunLock(runId, () => this.loadPolicies(runId, { signal }), signal);
    }
  }

  /** Finalize-only admission: settlement grants no source deletion authority. */
  hasUnresolvedPolicy(runId: string): boolean {
    const rs = this.runs.get(runId);
    return !!rs && (rs.policyProtection || [...rs.policies.values()].some(p => p.serverSettled !== true));
  }

  /** Inclusive source-through-current interval; server settlement never removes physical protection. */
  hasPendingPolicyForGeneration(runId: string, generation: number): boolean {
    const rs = this.runs.get(runId);
    if (!rs) return false;
    if (rs.policyProtection) return true;
    return [...rs.policies.values()].some(p => p.record.outcome.source_generation <= generation && generation <= p.record.delivery.claim_generation);
  }

  async bindPolicyDelivery(runId: string, source: number, current: number, expectedIdentity: Readonly<object>): Promise<PolicyBindingResult> {
    if (this.disabled || !positiveSafe(current) || !positiveSafe(source) || source > current) return { bound: false, reason: "invalid_or_disabled" };
    return this.withRunLock<PolicyBindingResult>(runId, async () => {
      const meta = await this.authenticatePolicy(runId, source);
      if (!meta || !expectedIdentity || meta.identity !== expectedIdentity || current < meta.record.delivery.claim_generation) return { bound: false, reason: "identity_or_generation" };
      try {
        if (current === meta.record.delivery.claim_generation) {
          await this.fsyncDir(this.runDir(runId), true);
          await this.loadPolicies(runId);
          return { bound: true, winner: this.pendingPolicy(meta), context: { expectedIdentity: meta.identity } };
        }
        const next: PolicyRecord = { ...meta.record, delivery: { claim_generation: current, inventory_generation: current } };
        const serialized = this.seal(MAC_DOMAIN_POLICY, next);
        if (Buffer.byteLength(serialized) > POLICY_MAX_BYTES) return { bound: false, reason: "record_too_large" };
        // Invalidate old observations before IO, including an uncertain rename/fsync result.
        this.runs.get(runId)!.policyProtection = true;
        meta.serverSettled = undefined;
        meta.identity = Object.freeze({});
        await this.withReserveOnEnospc(() => this.writeFileAtomic(path.join(this.runDir(runId), policyFileName(source)), serialized, "policy", { runId, lifetime: meta.lifetime, version: meta.fileVersion }), this.reserveBytes);
        const winner = await this.authenticatePolicy(runId, source);
        if (!winner) return { bound: false, reason: "winner_unavailable" };
        await this.loadPolicies(runId);
        return { bound: true, winner: this.pendingPolicy(winner), context: { expectedIdentity: winner.identity } };
      } catch {
        this.runs.get(runId)!.policyProtection = true;
        return { bound: false, reason: "not_durable" };
      }
    });
  }

  async readPolicyForRetirement(runId: string, source: number, context: Omit<ReportRetirementContext, "expectedIdentity"> = {}): Promise<
    { request: PolicyHoldRequest; policy: PendingPolicy; lifetime: string; context: ReportRetirementContext } | undefined
  > {
    if (this.disabled) return undefined;
    return this.withRunLock(runId, async () => {
      const meta = await this.authenticatePolicy(runId, source, context);
      if (!meta || !policyContextEligible(context)) return undefined;
      return { request: { ...meta.record.outcome, claim_generation: meta.record.delivery.claim_generation },
        policy: this.pendingPolicy(meta), lifetime: meta.lifetime, context: { ...context, expectedIdentity: meta.identity } };
    }, context.signal);
  }

  async markPolicyServerSettled(runId: string, source: number, context?: ReportRetirementContext): Promise<void> {
    if (this.disabled || !context?.expectedIdentity) return;
    await this.withRunLock(runId, async () => {
      const meta = await this.authenticatePolicy(runId, source, context);
      if (meta && meta.identity === context.expectedIdentity && policyContextEligible(context)) meta.serverSettled = true;
    }, context.signal);
  }

  /** Proof is obtained once outside O; under O reauthenticate the complete exact observation
   * and recheck eligibility after every awaited filesystem proof. Only this report is unlinked. */
  async retirePolicyIfEligible(runId: string, source: number, context: PolicyRetirementContext): Promise<boolean> {
    if (this.disabled || !context.expectedIdentity || !context.freshProof || !context.eligible || !policyContextEligible(context)) return false;
    try {
      const observation = await this.readPolicyForRetirement(runId, source, context);
      if (!observation || observation.policy.server_settled !== true ||
          observation.context.expectedIdentity !== context.expectedIdentity || !policyContextEligible(context)) return false;
      // One proof attempt, bounded to the caller's remaining budget or 30 seconds
      // (one second by default). A failed proof blocks only this report's retirement.
      const deadline = Math.min(context.deadline ?? Date.now() + 1_000, Date.now() + 30_000);
      const controller = new AbortController();
      let timer: ReturnType<typeof setTimeout> | undefined;
      let abort: (() => void) | undefined;
      let proof: Awaited<ReturnType<NonNullable<PolicyRetirementContext["freshProof"]>>>;
      try {
        proof = await Promise.race([
          context.freshProof(observation.policy, { signal: controller.signal, deadline }),
          new Promise<never>((_, reject) => {
            abort = () => { controller.abort(); reject(new Error("policy proof cancelled")); };
            context.signal?.addEventListener("abort", abort, { once: true });
            timer = setTimeout(abort, Math.max(0, deadline - Date.now()));
            if (context.signal?.aborted) abort();
          }),
        ]);
      } finally {
        if (timer) clearTimeout(timer);
        if (abort) context.signal?.removeEventListener("abort", abort);
        controller.abort();
      }
      if (!proof || proof.outcome_id !== observation.request.outcome_id || proof.source_generation !== source ||
          proof.claim_generation !== observation.request.claim_generation || proof.inventory_complete !== true ||
          proof.sources_safe !== true || proof.custody_settled !== true || !policyContextEligible(context)) return false;
      return await this.withRunLock(runId, async () => {
        const meta = await this.authenticatePolicy(runId, source, context);
        if (!meta || meta.serverSettled !== true || meta.identity !== context.expectedIdentity || !policyContextEligible(context)) return false;
        const file = path.join(this.runDir(runId), policyFileName(source));
        if (!await this.installedLifetimeMatches(file, meta.lifetime, context, meta.fileVersion) || !policyContextEligible(context)) return false;
        // Keep conservative physical protection before unlink, including fsync failure.
        this.runs.get(runId)!.policyProtection = true;
        await fs.unlink(file);
        this.runs.get(runId)!.policies.delete(source);
        await this.fsyncDir(this.runDir(runId), true);
        await this.loadPolicies(runId);
        return true;
      }, context.signal);
    } catch { return false; }
  }

  // ── writes ──────────────────────────────────────────────────────────────────

  /**
   * Append one flush as a single immutable segment. Enforces the quota FIRST
   * (evicting the oldest segment to a range record when the run or worker quota
   * binds), then writes the segment and installs the next manifest generation.
   */
  async appendSegment(runId: string, generation: number, msgs: OutgoingMessage[],
    onCommitted?: (identity: symbol) => void, signal?: AbortSignal): Promise<void> {
    if (!this.writable()) return;
    if (msgs.length === 0) return;
    const first = msgs[0];
    const last = msgs[msgs.length - 1];
    if (!first || !last) return;
    await this.withRunLock(runId, async () => {
      const rs = await this.ensureRunState(runId);
      if (!rs) return; // refused (symlink / non-bare runId) or disabled

      const body = {
        version: 1 as const,
        runId,
        firstSeq: first.seq,
        lastSeq: last.seq,
        generation,
        messages: msgs,
      };
      const serialized = this.seal(MAC_DOMAIN_SEGMENT, body);
      const bytes = Buffer.byteLength(serialized, "utf8");

      await this.enforceQuota(runId, bytes);

      const fileVersion = this.nextFileVersion(rs, first.seq, last.seq);
      const file = `seg-${first.seq}-${last.seq}-v${fileVersion}.json`;
      await this.writeFileAtomic(path.join(this.runDir(runId), file), serialized, "segment");

      const ref: ManifestRecordRef = { kind: "segment", firstSeq: first.seq, lastSeq: last.seq, fileVersion, file };
      await this.installManifest(rs, [...rs.manifest.records, ref]);
      rs.recordBytes.set(file, bytes);
      // Trusted synchronous observer: commit evidence is emitted while the run lock is held.
      onCommitted?.(this.deliveryIdentity(ref));
    }, signal);
  }

  /**
   * Append one compact range record covering `firstSeq..lastSeq` (the M2
   * spill-buffer-cap drop path). Uses the reserve on `ENOSPC`: releases it, writes
   * the record, then replenishes it. Replay expands the record into one tombstone
   * per seq so the stream stays contiguous.
   */
  async appendRangeRecord(runId: string, generation: number, firstSeq: number, lastSeq: number,
    onCommitted?: (identity: symbol) => void, signal?: AbortSignal): Promise<void> {
    if (!this.writable()) return;
    await this.withRunLock(runId, async () => {
      const rs = await this.ensureRunState(runId);
      if (!rs) return;
      await this.withReserveOnEnospc(async () => {
        const body = {
          version: 1 as const,
          runId,
          firstSeq,
          lastSeq,
          generation,
          event: "message_dropped" as const,
          reason: DROP_REASON,
        };
        const serialized = this.seal(MAC_DOMAIN_RANGE, body);
        const bytes = Buffer.byteLength(serialized, "utf8");
        const fileVersion = this.nextFileVersion(rs, firstSeq, lastSeq);
        const file = `range-${firstSeq}-${lastSeq}-v${fileVersion}.json`;
        await this.writeFileAtomic(path.join(this.runDir(runId), file), serialized, "range");
        const ref: ManifestRecordRef = { kind: "range", firstSeq, lastSeq, fileVersion, file };
        await this.installManifest(rs, [...rs.manifest.records, ref]);
        rs.recordBytes.set(file, bytes);
        onCommitted?.(this.deliveryIdentity(ref));
      });
    }, signal);
  }

  /** Set the spill-unclean flag when a spill begins (creates the run's manifest if
   *  none exists yet). Surfaced at the next restart via {@link uncleanRuns}. */
  async markSpillUnclean(runId: string, signal?: AbortSignal): Promise<void> {
    if (!this.writable()) return;
    await this.withRunLock(runId, async () => {
      const rs = await this.ensureRunState(runId);
      if (!rs) return;
      if (rs.manifest.spilledUnclean && rs.manifest.generation > 0) return;
      await this.installManifest(rs, rs.manifest.records, { spilledUnclean: true });
    }, signal);
  }

  /** Clear the spill-unclean flag on a clean flush/close. */
  async clearSpillUnclean(runId: string, signal?: AbortSignal): Promise<void> {
    if (!this.writable()) return;
    await this.withRunLock(runId, async () => {
      const rs = this.runs.get(runId);
      if (!rs || !rs.manifest.spilledUnclean) return;
      await this.installManifest(rs, rs.manifest.records, { spilledUnclean: false });
    }, signal);
  }

  // ── drain / replay ────────────────────────────────────────────────────────────

  /**
   * Replay a run's pending records in ascending seq order over `send`, retiring
   * each on success (advance cursor, drop the ref, unlink the file). A range
   * record — or a manifest-referenced segment that is missing or MAC-bad (a VALID
   * manifest proves the gap) — expands to ONE status tombstone per seq, never a
   * hole. A `send` that throws {@link StaleClaimError} retires the record locally
   * and counts it in `staleRetired`; any OTHER throw stops the drain and leaves
   * the rest pending. Returns whether the run is now fully retired and the run's
   * cumulative stale-retired seq count. The optional signal cancels queued lock
   * acquisition only; an acquired drain awaits send and retirement truthfully.
   */
  async drainRun(
    runId: string,
    send: (msgs: OutgoingMessage[], generation: number) => Promise<void>,
    onAcknowledged?: (identity: symbol) => void,
    signal?: AbortSignal,
  ): Promise<{ retired: boolean; staleRetired: number }> {
    if (this.disabled) return { retired: false, staleRetired: 0 };
    return this.withRunLock(runId, async () => {
      const rs = this.runs.get(runId);
      if (!rs) return { retired: true, staleRetired: 0 }; // unknown/rejected run: nothing to replay

      const pending = rs.manifest.records
        .filter((r) => r.lastSeq > rs.manifest.cursor)
        .sort((a, b) => a.firstSeq - b.firstSeq);

      for (const rec of pending) {
        try {
          // Replay each record under the generation it was PRODUCED under (needed for
          // #1247's fence once the api advertises it, D11), so a re-claim's generation
          // bump never rebinds an old attempt's frames. Streamed in bounded chunks so a
          // wide range never allocates one huge array or posts one oversized request.
          await this.replayRecord(rs, rec, send);
          // Only this drain gets evidence, and only after every chunk was ACKed.
          // Observer callbacks are trusted, synchronous and must not throw.
          onAcknowledged?.(this.deliveryIdentity(rec));
        } catch (err) {
          if (err instanceof StaleClaimError) {
            this.log.warn("outbox: record refused as stale claim; retiring locally (frames lost, not rebound)", {
              run_id: runId,
              first_seq: rec.firstSeq,
              last_seq: rec.lastSeq,
            });
            await this.retireRecord(rs, rec, true);
            continue;
          }
          // Any other error: stop, leave this and the rest pending for the next drain.
          return { retired: false, staleRetired: rs.manifest.staleRetired };
        }
        await this.retireRecord(rs, rec, false);
      }
      return { retired: rs.manifest.records.length === 0, staleRetired: rs.manifest.staleRetired };
    }, signal);
  }

  /** Replay one record as bounded, sequence-ordered chunks over `send` (each ≤
   *  OUTBOX_DRAIN_CHUNK messages). A segment replays its own messages under the
   *  segment's generation; a range record — or a segment the valid manifest proves
   *  missing/tampered — replays as per-seq gap tombstones under the record's
   *  generation (0 when the file does not authenticate). Any send throw propagates so
   *  drainRun leaves the record pending; the record is retired by the caller only
   *  after every chunk lands. A crash/partial send re-drains the whole record on the
   *  next pass — dedup on (run_id, seq) absorbs the repeat. */
  private async replayRecord(
    rs: RunState,
    rec: ManifestRecordRef,
    send: (msgs: OutgoingMessage[], generation: number) => Promise<void>,
  ): Promise<void> {
    if (rec.kind === "segment") {
      const seg = await this.readSegmentFile(rs.runId, rec);
      if (seg) {
        for (let i = 0; i < seg.messages.length; i += OUTBOX_DRAIN_CHUNK) {
          await send(seg.messages.slice(i, i + OUTBOX_DRAIN_CHUNK), seg.generation);
        }
        return;
      }
      this.log.warn("outbox: segment missing/tampered; emitting per-seq gap tombstones (manifest proves the range)", {
        run_id: rs.runId,
        first_seq: rec.firstSeq,
        last_seq: rec.lastSeq,
      });
      await this.sendGapChunks(rec, "outbox segment unrecoverable", 0, send);
      return;
    }
    // A range record IS a dropped range; the manifest ref alone proves it. Read the file
    // only for its recorded generation (0 when it does not authenticate OR its
    // authenticated runId does not match this directory — see the runId-binding fix).
    const parsed = await this.readAuthed(path.join(this.runDir(rs.runId), rec.file), MAC_DOMAIN_RANGE);
    const generation =
      parsed && parsed.runId === rs.runId && typeof parsed.generation === "number" ? parsed.generation : 0;
    await this.sendGapChunks(rec, DROP_REASON, generation, send);
  }

  /** Emit one status tombstone per seq in `rec`'s range, chunked to OUTBOX_DRAIN_CHUNK
   *  and sent in ascending order — building only one chunk at a time (never the whole
   *  range) so an arbitrarily wide range never allocates a huge array. */
  private async sendGapChunks(
    rec: Pick<ManifestRecordRef, "firstSeq" | "lastSeq">,
    reason: string,
    generation: number,
    send: (msgs: OutgoingMessage[], generation: number) => Promise<void>,
  ): Promise<void> {
    for (let start = rec.firstSeq; start <= rec.lastSeq; start += OUTBOX_DRAIN_CHUNK) {
      const end = Math.min(start + OUTBOX_DRAIN_CHUNK - 1, rec.lastSeq);
      const chunk: OutgoingMessage[] = [];
      for (let seq = start; seq <= end; seq++) chunk.push(gapTombstone(seq, reason));
      await send(chunk, generation);
    }
  }

  private async retireRecord(rs: RunState, rec: ManifestRecordRef, stale: boolean): Promise<void> {
    const nextRecords = rs.manifest.records.filter((r) => r.file !== rec.file);
    const patch: ManifestPatch = { cursor: Math.max(rs.manifest.cursor, rec.lastSeq) };
    if (stale) patch.staleRetired = rs.manifest.staleRetired + seqCount(rec);
    await this.installManifest(rs, nextRecords, patch);
    rs.recordBytes.delete(rec.file);
    // D12: unlink the old file only AFTER the new manifest generation is installed.
    await fs.rm(path.join(this.runDir(rs.runId), rec.file), { force: true }).catch(() => undefined);
  }

  // ── quota / eviction ──────────────────────────────────────────────────────────

  private async enforceQuota(runId: string, incomingBytes: number): Promise<void> {
    // Run quota: evict this run's oldest segment until the incoming segment fits.
    // The appending run already holds its own lock, so a same-run eviction stays on
    // this in-lock path — unchanged.
    while (this.runBytes(runId) + incomingBytes > this.runMaxBytes) {
      const oldest = this.oldestSegmentInRun(runId);
      if (!oldest) break; // nothing evictable (only ranges, or empty) — write over quota
      await this.evictSegment(runId, oldest);
    }
    // Worker quota: evict the globally-oldest segment across all runs. The victim may
    // belong to a DIFFERENT run whose manifest must be mutated ONLY under its own
    // per-run lock (else its own concurrent append races the eviction on manifest.json
    // and a generation is lost). So a cross-run victim's lock is taken NON-BLOCKING:
    // a victim whose lock is currently held is skipped — never waited on, which would
    // risk an A-waits-B / B-waits-A deadlock and stall this spill on another run's I/O.
    // oldestEvictableSegment already excludes busy victims, so it hands back the oldest
    // segment we can evict without blocking (the appending run's own, or a free run's);
    // when none is free the eviction is deferred and the append proceeds — maxBytes is
    // a SOFT target and a transient overage until the next append is acceptable, the
    // store's existing bounded-degradation model.
    while (this.totalBytes() + incomingBytes > this.maxBytes) {
      const victim = this.oldestEvictableSegment(runId);
      if (!victim) break; // nothing evictable whose lock is free — defer (soft quota)
      if (victim.runId === runId) {
        await this.evictSegment(victim.runId, victim.rec); // same run: A already holds the lock
        continue;
      }
      const release = this.tryRunLock(victim.runId);
      if (!release) break; // raced busy between selection and acquire — defer, never block
      try {
        await this.evictSegment(victim.runId, victim.rec);
      } finally {
        release();
      }
    }
  }

  /** Replace one segment with a compact range record covering its exact seq range
   *  (net bytes drop). D12: write the new range file, install the next manifest
   *  generation with the ref replaced, THEN unlink the old segment. */
  private async evictSegment(runId: string, segRec: ManifestRecordRef): Promise<void> {
    const rs = this.runs.get(runId);
    if (!rs) return;
    const seg = await this.readSegmentFile(runId, segRec);
    const generation = seg ? seg.generation : 0;
    const body = {
      version: 1 as const,
      runId,
      firstSeq: segRec.firstSeq,
      lastSeq: segRec.lastSeq,
      generation,
      event: "message_dropped" as const,
      reason: DROP_REASON,
    };
    const serialized = this.seal(MAC_DOMAIN_RANGE, body);
    const bytes = Buffer.byteLength(serialized, "utf8");
    const fileVersion = segRec.fileVersion + 1;
    const file = `range-${segRec.firstSeq}-${segRec.lastSeq}-v${fileVersion}.json`;
    await this.writeFileAtomic(path.join(this.runDir(runId), file), serialized, "range");

    const ref: ManifestRecordRef = {
      kind: "range",
      firstSeq: segRec.firstSeq,
      lastSeq: segRec.lastSeq,
      fileVersion,
      file,
    };
    // Quota replacement changes storage shape, not the logical delivery obligation.
    this.deliveryIdentities.set(ref, this.deliveryIdentity(segRec));
    const nextRecords = rs.manifest.records.map((r) => (r.file === segRec.file ? ref : r));
    await this.installManifest(rs, nextRecords);
    rs.recordBytes.delete(segRec.file);
    rs.recordBytes.set(file, bytes);
    await fs.rm(path.join(this.runDir(runId), segRec.file), { force: true }).catch(() => undefined);
    this.log.info("outbox: evicted oldest segment to a range record (quota)", {
      run_id: runId,
      first_seq: segRec.firstSeq,
      last_seq: segRec.lastSeq,
    });
  }

  private oldestSegmentInRun(runId: string): ManifestRecordRef | undefined {
    const rs = this.runs.get(runId);
    if (!rs) return undefined;
    let oldest: ManifestRecordRef | undefined;
    for (const r of rs.manifest.records) {
      if (r.kind !== "segment") continue;
      if (!oldest || r.firstSeq < oldest.firstSeq) oldest = r;
    }
    return oldest;
  }

  /** The globally-oldest segment this append may evict WITHOUT blocking, ordered by
   *  (run creation time, firstSeq) as an age proxy (no per-record timestamp is
   *  stored, D12 shape). Candidates are the appending run (whose lock is already
   *  held on this path) plus any OTHER run whose per-run lock is currently free; a
   *  different run whose lock is HELD is skipped, because its manifest must be
   *  mutated only under its own lock and blocking on it could deadlock. With no
   *  contention every run is a candidate, so this is exactly the globally-oldest
   *  segment — the common-case behavior is unchanged. */
  private oldestEvictableSegment(appendingRunId: string): { runId: string; rec: ManifestRecordRef } | undefined {
    let best: { runId: string; rec: ManifestRecordRef; since: number } | undefined;
    for (const rs of this.runs.values()) {
      // Skip a DIFFERENT run whose lock is held — its own op is mutating its manifest.
      if (rs.runId !== appendingRunId && this.runLocks.has(rs.runId)) continue;
      for (const r of rs.manifest.records) {
        if (r.kind !== "segment") continue;
        if (
          !best ||
          rs.manifest.since < best.since ||
          (rs.manifest.since === best.since && r.firstSeq < best.rec.firstSeq)
        ) {
          best = { runId: rs.runId, rec: r, since: rs.manifest.since };
        }
      }
    }
    return best ? { runId: best.runId, rec: best.rec } : undefined;
  }

  private runBytes(runId: string): number {
    const rs = this.runs.get(runId);
    if (!rs) return 0;
    let total = 0;
    for (const b of rs.recordBytes.values()) total += b;
    return total;
  }

  private totalBytes(): number {
    let total = 0;
    for (const rs of this.runs.values()) for (const b of rs.recordBytes.values()) total += b;
    return total;
  }

  // ── retention / removal ────────────────────────────────────────────────────────

  /** Age-delete only runs whose records are all retired (empty), that hold NO pending
   *  terminal journal (Run B / M3, PRD M1), and whose last write is older than
   *  `retentionMs`. A run with undrained records or a pending outcome is never removed
   *  here. */
  async sweepRetention(nowArg?: number): Promise<void> {
    if (this.disabled) return;
    const now = nowArg ?? this.now();
    // Collect first, then remove — never mutate the map mid-iteration.
    const toRemove: string[] = [];
    for (const [runId, rs] of this.runs) {
      if (
        rs.manifest.records.length === 0 &&
        rs.terminals.size === 0 &&
        rs.finalizes.size === 0 && rs.policies.size === 0 && !rs.policyProtection &&
        now - rs.manifest.updatedAt > this.retentionMs
      ) {
        toRemove.push(runId);
      }
    }
    for (const runId of toRemove) {
      const removed = await this.withRunLock(runId, () => this.removeRunIfStillReclaimable(runId, now));
      if (removed) this.log.info("outbox: retention removed fully-retired run", { run_id: runId });
    }
  }

  /** Remove a run's whole subtree (the api reported the run terminal/404). */
  async retireRun(runId: string): Promise<void> {
    await this.withRunLock(runId, () => this.removeRun(runId));
  }

  /** Delete a run ONLY if it is STILL reclaimable, re-checked under the run's lock
   *  against the CURRENT in-memory state — the retention-sweep TOCTOU guard.
   *  {@link sweepRetention} decides a run is removable (records empty, past
   *  retentionMs) OUTSIDE the lock; by the time this runs under the lock a legitimate
   *  {@link appendSegment} may have won the lock chain between that decision and here
   *  and committed a fresh, undrained record. {@link removeRun} deletes the subtree
   *  UNCONDITIONALLY, so calling it here would silently delete that just-committed
   *  record. Re-reading `rs` and re-testing emptiness + age against the SAME `now`
   *  skips a run that is no longer tracked, no longer empty, or no longer past
   *  retentionMs. Returns whether the run was actually removed. {@link retireRun}
   *  (api-said-terminal / api-said-404) keeps calling the unconditional
   *  {@link removeRun}: it must delete regardless of records. */
  private async removeRunIfStillReclaimable(runId: string, now: number): Promise<boolean> {
    const rs = this.runs.get(runId);
    if (!rs) return false; // no longer tracked (already removed / retired)
    if (rs.manifest.records.length !== 0) return false; // a record was appended after the decision
    if (rs.terminals.size !== 0) return false; // a terminal journal was installed after the decision
    if (rs.policies.size !== 0 || rs.policyProtection) return false;
    if (rs.finalizes.size !== 0) return false; // a finalize record was installed after the decision
    if (now - rs.manifest.updatedAt <= this.retentionMs) return false; // no longer past retention
    if (await this.hasPhysicalTerminalProtection(runId)) return false;
    await this.removeRun(runId);
    return true;
  }

  private async removeRun(runId: string): Promise<void> {
    // Guard the recursive delete: a non-bare runId (`..`, a nested path, a
    // separator/NUL) would escape the root, so refuse it here — the last line of
    // defense before `fs.rm(..., { recursive: true })` (H1).
    if (!this.validRunId(runId)) return;
    if (this.hasPendingPolicy(runId) || await this.hasPhysicalTerminalProtection(runId)) return;
    await fs.rm(this.runDir(runId), { recursive: true, force: true }).catch(() => undefined);
    this.runs.delete(runId);
  }

  // ── read surfaces ──────────────────────────────────────────────────────────────

  /** Run ids that still have at least one pending (un-retired) record. */
  runsWithPending(): string[] {
    const out: string[] = [];
    for (const [runId, rs] of this.runs) {
      if (rs.manifest.records.some((r) => r.lastSeq > rs.manifest.cursor)) out.push(runId);
    }
    return out;
  }

  /** PRD #1391 Run B M3b (N3): does this run still hold UNDRAINED message segments (locally-held
   *  deliverable messages the per-worker drainer has not yet replayed)? The terminal send path's
   *  gap-fill consults this before tombstoning a hole below the fence as "unrecoverable": a run
   *  that spilled during the same outage may still have segments covering those seqs, and
   *  tombstoning them would RACE the drainer and destroy recoverable messages. Gap-fill therefore
   *  runs ONLY on a fully-drained run (this returns false); otherwise it keeps the journal for a
   *  later resolve after the drain completes. A run with no message manifest (a terminal-only run)
   *  returns false — the same "nothing undrained" answer as a fully-retired run. */
  hasUndrainedMessages(runId: string): boolean {
    const rs = this.runs.get(runId);
    if (!rs) return false;
    return rs.manifest.records.some((r) => r.lastSeq > rs.manifest.cursor);
  }

  private deliveryIdentity(rec: ManifestRecordRef): symbol {
    let identity = this.deliveryIdentities.get(rec);
    if (identity === undefined) {
      identity = Symbol();
      this.deliveryIdentities.set(rec, identity);
    }
    return identity;
  }

  /** Capture every unretired logical record synchronously, including records
   * currently excluded by the replay cursor. Only a complete owned replay can
   * supply its receipt. Identities live only in this Outbox instance.
   */
  pendingDeliveryIdentities(runId: string): readonly symbol[] {
    return this.runs.get(runId)?.manifest.records.map((rec) => this.deliveryIdentity(rec)) ?? [];
  }

  /** The outbox depth for one run, or undefined if the run is not tracked. */
  depthFor(runId: string): OutboxDepth | undefined {
    const rs = this.runs.get(runId);
    if (!rs) return undefined;
    const m = rs.manifest;
    let pendingMessages = 0;
    for (const r of m.records) if (r.lastSeq > m.cursor) pendingMessages += seqCount(r);
    const blockedReason = oldestBlockedReason(rs);
    return {
      runId,
      pendingMessages,
      // Run B: every installed, un-retired terminal journal for this run (blocked or not).
      pendingTerminal: rs.terminals.size,
      ...(rs.policies.size ? { pendingPolicy: rs.policies.size } : {}),
      staleRetired: m.staleRetired,
      // The run's oldest blocked terminal reason, if any (D13) — omitted when none is blocked.
      ...(blockedReason !== undefined ? { blockedReason } : {}),
      since: m.since,
    };
  }

  /** Runs whose spill-unclean flag was set as observed AT INIT (for M2's restart
   *  log). A same-session markSpillUnclean does not retroactively join this set. */
  uncleanRuns(): string[] {
    return [...this.uncleanAtInit];
  }

  /** Whether the store failed closed at {@link init} (symlinked root, unreadable or
   *  wrong-size `.key`, `.key`-absent-but-records-present, or a mint failure). While
   *  disabled every write is a SILENT no-op and drain replays nothing, so a caller
   *  must treat a disabled store as "no durable store" — the batcher trips rather
   *  than entering spill, which would drop the whole buffer into the void with no
   *  trip and no report. */
  isDisabled(): boolean {
    return this.disabled;
  }

  // ── terminal resolve hold (#1539) ──────────────────────────────────────────
  //
  // A process-local, in-memory hold keyed by (runId, generation). The live run's permanent-failure
  // hook takes it BEFORE it installs its `failed` journal and releases it in a `finally` after its own
  // resolve, so the per-worker DRAINER (`Worker.resolveRunTerminal`, which fires when the run's spilled
  // segments retire) and the heartbeat terminal sweep (issue #1512) cannot send the journaled `failed`
  // during the hook's abort-then-reap window and race the hook's own resolve. It is deliberately NOT gated on `disabled`: it is a coordination flag
  // between two in-process callers, independent of whether the durable store is writable. The boot
  // resolve and RunRunner.resolveRunPendingTerminals ignore the hold — only the drainer and the
  // heartbeat sweep honour it, and they check it BEFORE their live-run check so a skip is always
  // recorded for the hook's release to re-drive.

  /** Terminals whose resolve the live run's own hook is currently driving; the drainer and the
   *  heartbeat terminal sweep (issue #1512) skip these. */
  private readonly heldTerminalResolves = new Set<string>();
  /** (runId, gen) pairs whose drainer or heartbeat-sweep resolve was SKIPPED because the hold was
   *  set, recorded so the hook's release knows a resolve was deferred and can re-drive it once (N4:
   *  no stranding). */
  private readonly skippedHeldResolves = new Set<string>();

  private static holdKey(runId: string, gen: number): string {
    return `${runId}\u0000${gen}`;
  }

  /** Take the resolve hold for (runId, gen). The drainer then skips this terminal until it is released. */
  holdTerminalResolve(runId: string, gen: number): void {
    this.heldTerminalResolves.add(Outbox.holdKey(runId, gen));
  }

  /** Release the resolve hold and report whether the drainer recorded a skip while it was held. When
   *  `skipped` is true and the journal is still pending, the hook re-drives one resolve (N4). */
  releaseTerminalResolve(runId: string, gen: number): { skipped: boolean } {
    const key = Outbox.holdKey(runId, gen);
    this.heldTerminalResolves.delete(key);
    const skipped = this.skippedHeldResolves.delete(key);
    return { skipped };
  }

  /** Whether (runId, gen)'s resolve is currently held by the live run's own hook. */
  isTerminalResolveHeld(runId: string, gen: number): boolean {
    return this.heldTerminalResolves.has(Outbox.holdKey(runId, gen));
  }

  /** Record that the drainer or the heartbeat sweep skipped resolving (runId, gen) because the hold was set. */
  noteHeldSkip(runId: string, gen: number): void {
    this.skippedHeldResolves.add(Outbox.holdKey(runId, gen));
  }

  // ── terminal journals (Run B / M3) ─────────────────────────────────────────────

  /**
   * Journal a run-lane terminal outcome WRITE-AHEAD, before its first network attempt (D3/D4).
   * Installs `terminal-<claimGeneration>.json` crash-atomically and EXCLUSIVELY via the
   * `mintKey`-style no-replace `link()` idiom (temp → fsync → `link()` → EEXIST adopts the
   * first durable winner, never overwriting it → dir fsync), under the run's single-flight lock.
   *
   * `canonicalBody` MUST already be the canonical body (M3b runs {@link canonicalizeTerminalBody}
   * before both this call and the first send, so the two are byte-identical). On a full volume the
   * temp write uses the terminal-sized reserve (release → retry → replenish); if even that cannot
   * admit it the result is `{journaled:false, reason:"reserve_exhausted"}` so the caller sends
   * unjournaled + logs — never a silent drop (D2 / SC2).
   */
  async journalTerminal(
    runId: string,
    claimGeneration: number,
    phaseAtJournal: string,
    messagesThroughSeq: number,
    canonicalBody: Record<string, unknown>,
  ): Promise<TerminalJournalResult> {
    if (this.disabled) {
      this.writable();
      return { journaled: false, reason: "reserve_exhausted" };
    }
    return this.withRunLock(runId, async () => {
      if (!this.validRunId(runId) || !Number.isSafeInteger(claimGeneration) || claimGeneration < 0) return { journaled: false, reason: "reserve_exhausted" };
      const dir = this.runDir(runId);
      if (await this.isSymlink(dir)) {
        this.log.warn("outbox: run dir is a symlink; refusing terminal journal (skipped, not followed)", {
          run_id: runId,
        });
        return { journaled: false, reason: "reserve_exhausted" };
      }
      const winner = this.runs.get(runId)?.terminals.get(claimGeneration);
      if (winner) {
        // Re-authenticate the selected filename before claiming adoption.
        const observed = await this.inspectTerminal(runId, claimGeneration, winner.fileName);
        switch (observed.observation.kind) {
          case "authenticated":
            return { journaled: true, adopted: true };
          case "absent":
            this.runs.get(runId)?.terminals.delete(claimGeneration);
            break;
          case "unreadable":
          case "key_unavailable":
            return { journaled: false, reason: "winner_unavailable" };
          case "mac_failure":
            return { journaled: false, reason: "reserve_exhausted" };
          case "malformed":
          case "oversized":
          case "symlink":
            this.runs.get(runId)?.terminals.delete(claimGeneration);
            return { journaled: false, reason: "reserve_exhausted" };
        }
      }
      if (!this.writable()) return { journaled: false, reason: "reserve_exhausted" };
      const since = this.now();
      const record: TerminalJournalData = {
        version: 1,
        run_id: runId,
        claim_generation: claimGeneration,
        phase_at_journal: phaseAtJournal,
        messages_through_seq: messagesThroughSeq,
        body: canonicalBody,
        since,
        state: "installed",
        blocked: false,
      };
      const serialized = this.seal(MAC_DOMAIN_TERMINAL, record);
      // Reserve the largest closed-vocabulary blocked-state update, so every accepted
      // new journal remains readable after any legitimate blocked-state rewrite.
      const blocked = this.seal(MAC_DOMAIN_TERMINAL, { ...record, blocked: true, blocked_reason: "completion_permit_mismatch" });
      if (Math.max(Buffer.byteLength(serialized), Buffer.byteLength(blocked)) > this.terminalReadMaxBytes) return { journaled: false, reason: "reserve_exhausted" };
      const dst = path.join(dir, terminalFileName(claimGeneration));
      let adopted: boolean;
      try {
        // M3a review fold-in (SC2): the run-dir mkdir (and its dir fsync) can ENOSPC too, so a
        // terminal write-ahead on a FULL volume with an ABSENT run dir would THROW if the mkdir
        // sat before the ENOSPC handling. Wrap EVERY disk step that can hit ENOSPC — the mkdir
        // INCLUDED — in withReserveOnEnospc so releasing the reserve frees the space the directory
        // entry (and the record) needs; if even that cannot admit it the ENOSPC propagates to the
        // catch below and degrades to reserve_exhausted rather than throwing. The mkdir is routed
        // through the same rawWrite ENOSPC seam as the record write so a test can simulate a full
        // volume at the directory-creation step.
        adopted = await this.withReserveOnEnospc(async () => {
          await this.rawWrite(
            async () => {
              await fs.mkdir(dir, { recursive: true, mode: 0o700 });
            },
            { path: dir, kind: "terminal" },
          );
          await this.fsyncDir(this.root);
          return this.installExclusive(dst, serialized, "terminal");
        });
      } catch (err) {
        if (isENOSPC(err)) {
          // Even releasing the reserve could not admit the journal: signal the caller to
          // send unjournaled (SC2), never a silent drop. Nothing is left on disk here.
          this.log.error("outbox: terminal-journal reserve exhausted; caller must send unjournaled", {
            run_id: runId,
            claim_generation: claimGeneration,
          });
          return { journaled: false, reason: "reserve_exhausted" };
        }
        throw err;
      }
      if (adopted) {
        const existing = await this.readTerminalAuthed(runId, claimGeneration);
        const meta = existing && coerceTerminal(existing, runId, claimGeneration, this.runs.get(runId)?.terminals.get(claimGeneration)?.fileName);
        if (!meta) return { journaled: false, reason: "reserve_exhausted" };
        const rs = this.ensureInMemoryRun(runId, meta.since);
        if (!rs.terminals.has(claimGeneration)) rs.terminals.set(claimGeneration, meta);
      }
      // Track it in memory. On an adopt (a first-writer already installed this generation) keep
      // the winner's metadata if we already loaded it; otherwise record ours (same generation).
      const rs = this.ensureInMemoryRun(runId, since);
      if (!rs.terminals.has(claimGeneration)) {
        rs.terminals.set(claimGeneration, {
          fileName: terminalFileName(claimGeneration),
          claimGeneration,
          phaseAtJournal,
          since,
          blocked: false,
        });
      }
      if (adopted) {
        this.log.warn("outbox: a terminal journal for this generation already existed; adopted the first writer", {
          run_id: runId,
          claim_generation: claimGeneration,
        });
      }
      return { journaled: true, adopted };
    });
  }

  /** Every pending (installed, un-retired) terminal journal across all runs, for the heartbeat
   *  outbox report and `uzi admin workers` (M5's `blocked_reason` folds in from here). */
  listPendingTerminals(): PendingTerminal[] {
    const out: PendingTerminal[] = [];
    for (const rs of this.runs.values()) {
      for (const t of rs.terminals.values()) {
        out.push({
          run_id: rs.runId,
          claim_generation: t.claimGeneration,
          phase: t.phaseAtJournal,
          blocked: t.blocked,
          ...(!this.disabled && this.terminalRetirementIdentities.get(t)?.serverSettled === true ? { server_settled: true } : {}),
          ...(t.blockedReason !== undefined ? { blocked_reason: t.blockedReason } : {}),
          since: t.since,
        });
      }
    }
    return out;
  }

  /** Retire a terminal journal (the api applied the transition on a 200, or answered a 409 whose
   *  returned status is terminal): unlink the file and drop it from the pending set (D3). */
  async retireTerminal(runId: string, claimGeneration: number, _context?: ReportRetirementContext): Promise<boolean> {
    if (this.disabled) return false;
    return this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return false;
      // inspectTerminal can observe ENOENT below an unsafe root; that is not safe absence.
      try {
        const root = await fs.lstat(this.root);
        if (!root.isDirectory() || root.isSymbolicLink()) return false;
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") return false;
      }
      const terminals = this.runs.get(runId)?.terminals;
      const pending = terminals?.get(claimGeneration);
      const fileName = pending?.fileName ?? terminalFileName(claimGeneration);
      const { observation, record } = await this.inspectTerminal(runId, claimGeneration, fileName);
      if (observation.kind === "absent") {
        // "absent" also covers ENOENT from ancestry inspection; re-prove it before dropping bookkeeping.
        if (!pending || !await this.terminalPositivelyAbsent(runId, fileName)) return false;
        this.terminalRetirementIdentities.delete(pending);
        terminals?.delete(claimGeneration);
        return true;
      }
      if (!record || observation.kind !== "authenticated") return false;
      try {
        await fs.unlink(path.join(this.runDir(runId), fileName));
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT" ||
            !await this.terminalPositivelyAbsent(runId, fileName)) return false;
      }
      if (pending) this.terminalRetirementIdentities.delete(pending);
      terminals?.delete(claimGeneration);
      return true;
    });
  }

  /** Positive absence of one terminal journal: a missing root or run directory, or a safe run
   *  directory whose journal lstat reports ENOENT. Unsafe ancestry and every other error fail closed. */
  private async terminalPositivelyAbsent(runId: string, fileName: string): Promise<boolean> {
    try {
      const root = await fs.lstat(this.root);
      if (!root.isDirectory() || root.isSymbolicLink()) return false;
    } catch (err) { return (err as NodeJS.ErrnoException).code === "ENOENT"; }
    try {
      await fs.lstat(this.runDir(runId));
    } catch (err) { return (err as NodeJS.ErrnoException).code === "ENOENT"; }
    try {
      if (!await this.terminalDirectorySafe(runId)) return false;
    } catch { return false; }
    try {
      await fs.lstat(path.join(this.runDir(runId), fileName));
      return false;
    } catch (err) { return (err as NodeJS.ErrnoException).code === "ENOENT"; }
  }

  /** Positive absence proof for a successful unjournaled send, under the same run lock. */
  async confirmTerminalAbsent(runId: string, claimGeneration: number): Promise<boolean> {
    if (this.disabled || !this.validRunId(runId)) return false;
    return this.withRunLock(runId, async () => {
      if (this.hasPendingTerminal(runId, claimGeneration) || this.hasPendingPolicy(runId)) return false;
      // Unknown or noncanonical physical aliases cannot prove absence.
      if (await this.hasPhysicalTerminalProtection(runId)) return false;
      try {
        await fs.lstat(path.join(this.runDir(runId), terminalFileName(claimGeneration)));
        return false;
      } catch (err) { return (err as NodeJS.ErrnoException).code === "ENOENT"; }
    });
  }

  /** Positive exact report absence: one run lock, a one-second deadline and at most
   * 16384 root / 256 run entries. Unknown aliases, scan errors and exhaustion retain provenance. */
  async confirmReportsAbsent(runId: string, claimGeneration: number): Promise<boolean> {
    if (this.disabled || !this.validRunId(runId)) return false;
    const signal = AbortSignal.timeout(1_000);
    try {
      return await this.withRunLock(runId, async () => {
        if (this.disabled || signal.aborted || this.hasPendingTerminal(runId, claimGeneration) || this.hasPendingPolicy(runId) ||
            this.runs.get(runId)?.finalizes.has(claimGeneration)) return false;
        try {
          const root = await fs.lstat(this.root);
          if (!root.isDirectory() || root.isSymbolicLink()) return false;
        } catch (err) { return !signal.aborted && (err as NodeJS.ErrnoException).code === "ENOENT"; }
        const root = await fs.opendir(this.root);
        try {
          let ended = false;
          for (let n = 0; n < 16384 && !signal.aborted; n++) {
            const entry = await root.read();
            if (!entry) { ended = true; break; }
            if (entry.name !== runId && terminalRunUUID(entry.name) &&
                entry.name.toLowerCase() === runId.toLowerCase()) return false;
          }
          if (!ended) return false;
        } finally { await root.close(); }
        try {
          await fs.lstat(this.runDir(runId));
        } catch (err) { return !signal.aborted && (err as NodeJS.ErrnoException).code === "ENOENT"; }
        if (!await this.terminalDirectorySafe(runId)) return false;
        const dir = await fs.opendir(this.runDir(runId));
        try {
          for (let n = 0; n < 256 && !signal.aborted; n++) {
            const entry = await dir.read();
            if (!entry) return !signal.aborted;
            if (entry.name.startsWith(POLICY_FILE_PREFIX)) return false;
            const terminal = entry.name.startsWith(TERMINAL_FILE_PREFIX);
            const finalize = entry.name.startsWith(FINALIZE_FILE_PREFIX);
            if (!terminal && !finalize) continue;
            const generation = terminal ? parseTerminalFileName(entry.name) : parseFinalizeFileName(entry.name);
            if (generation === undefined || generation === claimGeneration ||
                entry.name !== (terminal ? terminalFileName(generation) : finalizeFileName(generation)) ||
                !entry.isFile() || entry.isSymbolicLink()) return false;
          }
          return false;
        } finally { await dir.close(); }
      }, signal);
    } catch { return false; }
  }

  /**
   * D11-style LOCAL retire of a terminal journal under a stale claim: unlink the file, increment the
   * run's `stale_retired` counter (the SAME manifest counter the message path uses), write NOTHING to
   * the server, and NEVER rebind the outcome to a new generation. This is M3b's `stale_claim` handler,
   * exposed now as the store primitive.
   */
  async staleRetireTerminal(runId: string, claimGeneration: number): Promise<void> {
    if (this.disabled) return;
    await this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return;
      const rs = this.runs.get(runId);
      if (!rs || !rs.terminals.has(claimGeneration)) return;
      if (!await this.readTerminalAuthed(runId, claimGeneration)) return;
      await fs
        .rm(path.join(this.runDir(runId), rs.terminals.get(claimGeneration)!.fileName), { force: true })
        .catch(() => undefined);
      this.terminalRetirementIdentities.delete(rs.terminals.get(claimGeneration)!);
      rs.terminals.delete(claimGeneration);
      // Reuse the message path's staleRetired mechanism (D11): a durable, monotone per-run count.
      // A terminal outcome is one "frame" lost, so the counter advances by one; installManifest
      // creates manifest.json for a terminal-only run that had none.
      await this.installManifest(rs, rs.manifest.records, { staleRetired: rs.manifest.staleRetired + 1 });
    });
  }

  /**
   * Mark a pending terminal journal permanently `blocked` with a closed-vocab reason (D13), so the
   * heartbeat outbox entry surfaces it and the owner can resolve it (never a timer). Updates the
   * journal file in place (a legitimate owner update of an already-installed outcome, NOT a competing
   * first-writer) and the in-memory metadata. A no-op when the run holds no such journal.
   */
  async markTerminalBlocked(runId: string, claimGeneration: number, reason: TerminalBlockedReason): Promise<void> {
    if (this.disabled) return;
    await this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return;
      const rs = this.runs.get(runId);
      const meta = rs?.terminals.get(claimGeneration);
      if (!rs || !meta) return;
      const dst = path.join(this.runDir(runId), meta.fileName);
      const parsed = await this.readTerminalAuthed(runId, claimGeneration);
      if (parsed) {
        const next = { ...parsed, blocked: true, blocked_reason: reason };
        // Re-seal + rewrite atomically (rename-based): the exclusive first-writer guarantee is only
        // for the INITIAL install; updating blocked-state is the owner's own follow-up write. The
        // rewrite needs temporary-file space too, so it must release the reserve on ENOSPC just like
        // the initial terminal install; otherwise a full volume can strand an unblocked journal.
        const serialized = this.seal(MAC_DOMAIN_TERMINAL, next);
        // Historical journals may have consumed the new writer\'s blocked-state reserve.
        // Keep their readable outcome intact and mark memory only rather than destroy replay.
        if (Buffer.byteLength(serialized) <= this.terminalReadMaxBytes) {
          const binding = this.terminalRetirementIdentities.get(meta);
          const lifetime = await this.withReserveOnEnospc(() => this.writeFileAtomic(dst, serialized, "terminal"));
          // markTerminalBlocked updates the same winner. Only the descriptor of our
          // own rewrite may carry its identity across the atomic rename.
          if (binding && lifetime && binding.lifetime === this.authenticatedLifetimes.get(parsed) &&
              binding.payload === terminalRetirementPayload(parsed)) binding.lifetime = lifetime;
        }
      } else {
        this.log.warn("outbox: terminal journal unreadable while marking blocked; updating in-memory only", {
          run_id: runId,
          claim_generation: claimGeneration,
        });
      }
      meta.blocked = true;
      meta.blockedReason = reason;
    });
  }

  /**
   * PRD #1391 M3b: read a pending terminal journal's DURABLE payload for the send path — the
   * already-canonical report body, its `messages_through_seq` fence and the phase captured at
   * journal time. The M3b `resolvePendingTerminal` send path reads this to send those exact
   * canonical bytes (adding the fence only when the api advertises `terminal_fence`). Returns
   * undefined when the run holds no such journal (already retired / never installed) or the file no
   * longer authenticates (a tampered/absent record proves nothing — never sent).
   */
  /** Capture the authenticated send payload and its opaque lifetime identity under O. */
  async readTerminalJournalForRetirement(runId: string, generation: number,
    context: Omit<ReportRetirementContext, "expectedIdentity"> = {},
  ): Promise<{ journal: PendingTerminalJournal; context: ReportRetirementContext } | undefined> {
    if (this.disabled) return undefined;
    return this.withRunLock(runId, async () => {
      const parsed = await this.readTerminalAuthed(runId, generation, undefined, context);
      const meta = this.runs.get(runId)?.terminals.get(generation);
      const journal = parsed && coerceTerminalRecord(parsed, runId, generation);
      if (!meta || !journal) return undefined;
      const payload = terminalRetirementPayload(parsed!);
      let binding = this.terminalRetirementIdentities.get(meta);
      const lifetime = this.authenticatedLifetimes.get(parsed!);
      if (!lifetime) return undefined;
      if (!binding || binding.payload !== payload || binding.lifetime !== lifetime) {
        binding = { identity: Object.freeze({}), payload, lifetime };
        this.terminalRetirementIdentities.set(meta, binding);
      }
      return { journal, context: { ...context, expectedIdentity: binding.identity } };
    }, context.signal);
  }

  private readonly authenticatedLifetimes = new WeakMap<object, string>();
  private readonly authenticatedFileVersions = new WeakMap<object, string>();
  private readonly terminalRetirementIdentities = new WeakMap<object, {
    identity: Readonly<object>; payload: string; lifetime: string; serverSettled?: boolean;
  }>();

  /** A terminal reply settles admission only for the authenticated journal sent. */
  async markTerminalServerSettled(runId: string, generation: number, context?: ReportRetirementContext): Promise<void> {
    if (this.disabled || !context?.expectedIdentity) return;
    await this.withRunLock(runId, async () => {
      const meta = this.runs.get(runId)?.terminals.get(generation);
      const binding = meta && this.terminalRetirementIdentities.get(meta);
      if (!meta || !binding || binding.identity !== context.expectedIdentity) return;
      const parsed = await this.readTerminalAuthed(runId, generation, undefined, context);
      if (!parsed || this.terminalRetirementIdentities.get(meta) !== binding ||
          this.authenticatedLifetimes.get(parsed) !== binding.lifetime || terminalRetirementPayload(parsed) !== binding.payload ||
          this.runs.get(runId)?.terminals.get(generation) !== meta || context.signal?.aborted ||
          (context.deadline !== undefined && Date.now() >= context.deadline) || context.eligible?.() === false) return;
      binding.serverSettled = true;
    }, context.signal);
  }

  /** Report-only unlink: no asynchronous proof callback runs under O. Once unlink starts,
   * its actual settlement owns O, even when the caller deadline expires. */
  async retireTerminalIfEligible(runId: string, generation: number, context: ReportRetirementContext): Promise<boolean> {
    if (this.disabled || !context.expectedIdentity) return false;
    return this.withRunLock(runId, async () => {
      const meta = this.runs.get(runId)?.terminals.get(generation);
      const binding = meta && this.terminalRetirementIdentities.get(meta);
      if (!meta || !binding || binding.identity !== context.expectedIdentity || meta.blocked) return false;
      const parsed = await this.readTerminalAuthed(runId, generation, undefined, context);
      if (!parsed || this.authenticatedLifetimes.get(parsed) !== binding.lifetime || parsed.blocked || terminalRetirementPayload(parsed) !== binding.payload ||
          this.runs.get(runId)?.terminals.get(generation) !== meta ||
          context.signal?.aborted || (context.deadline !== undefined && Date.now() >= context.deadline) ||
          context.eligible?.() === false) return false;
      const file = path.join(this.runDir(runId), meta.fileName);
      if (!await this.installedLifetimeMatches(file, binding.lifetime, context, this.authenticatedFileVersions.get(parsed)) || context.eligible?.() === false) return false;
      await fs.unlink(file);
      this.terminalRetirementIdentities.delete(meta);
      this.runs.get(runId)?.terminals.delete(generation);
      return true;
    }, context.signal);
  }

  async readTerminalJournal(
    runId: string,
    claimGeneration: number,
  ): Promise<PendingTerminalJournal | undefined> {
    if (this.disabled) return undefined;
    return this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return undefined;
      const parsed = await this.readTerminalAuthed(runId, claimGeneration);
      if (!parsed) return undefined;
      return coerceTerminalRecord(parsed, runId, claimGeneration) ?? undefined;
    });
  }

  /** In-memory check: does this run hold an installed, un-retired terminal journal for the
   *  generation? The executor catch reads it (via the send path) to treat a journaled outcome as
   *  FINAL — no second fallback `failed` once the write-ahead journal exists (fact 2, D5). */
  hasPendingTerminal(runId: string, claimGeneration: number): boolean {
    return this.runs.get(runId)?.terminals.has(claimGeneration) ?? false;
  }

  /** Install a terminal journal crash-atomically and EXCLUSIVELY (D4): temp write → fsync →
   *  no-replace `link()` (EEXIST means a first writer already won this generation) → dir fsync →
   *  remove temp. Returns whether an existing winner was adopted rather than freshly installed.
   *  The reserve/ENOSPC handling now lives in {@link journalTerminal} (and {@link journalFinalize}), wrapping this call AND the
   *  run-dir mkdir together, so the whole sequence uses (and replenishes) the reserve exactly once
   *  on a full volume — the temp write and the `link()` both ride that single release. */
  private async installExclusive(dst: string, serialized: string, kind: "terminal" | "finalize" | "policy"): Promise<boolean> {
    const tmp = `${dst}.${randomUUID()}.tmp`;
    const doWrite = async () => {
      const fh = await fs.open(
        tmp,
        fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_NOFOLLOW,
        0o600,
      );
      try {
        await fh.writeFile(serialized, "utf8");
        await fh.sync();
      } finally {
        await fh.close();
      }
    };
    try {
      await this.rawWrite(doWrite, { path: tmp, kind });
    } catch (err) {
      await fs.rm(tmp, { force: true }).catch(() => undefined);
      throw err;
    }
    let adopted = false;
    try {
      // fs.link is atomic and throws EEXIST if the journal already exists — a no-replace install,
      // so a racing writer for the same generation cannot clobber the first durable winner (D4).
      await fs.link(tmp, dst);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "EEXIST") {
        adopted = true;
      } else {
        await fs.rm(tmp, { force: true }).catch(() => undefined);
        throw err;
      }
    }
    try {
      await this.fsyncDir(path.dirname(dst), kind === "finalize" || kind === "policy");
    } finally {
      // The installed destination stays intact even when strict directory fsync fails.
      await fs.rm(tmp, { force: true }).catch(() => undefined);
    }
    return adopted;
  }

  /**
   * Issue #1742: journal that the executor of `claimGeneration` returned a finalize-bound result,
   * write-ahead of every later finalize step. Installs `finalize-<claimGeneration>.json` with the
   * terminal journal's mechanics (temp write with fsync, no-replace `link()`, directory fsync) under
   * the run's lock, sealed with its own MAC domain. The body is `{run_id, claim_generation, since}`
   * only. Idempotent: an existing record for the generation is adopted (reported as written).
   *
   * Never throws. The `finalize record durable` line is logged only after the directory fsync
   * completed; any failure logs `finalize record not written` and returns `written:false`.
   */
  async journalFinalize(runId: string, claimGeneration: number): Promise<FinalizeJournalResult> {
    const fail = (reason: string): FinalizeJournalResult => {
      this.log.warn("finalize record not written", { run_id: runId, claim_generation: claimGeneration, reason });
      return { written: false, reason };
    };
    try {
      if (this.disabled || !this.key) return fail("outbox_disabled");
      if (!Number.isSafeInteger(claimGeneration) || claimGeneration < 0) return fail("invalid_claim_generation");
      return await this.withRunLock(runId, async () => {
        if (!this.validRunId(runId)) return fail("invalid_run_id");
        const dir = this.runDir(runId);
        if (await this.isSymlink(dir)) return fail("run_dir_symlink");
        let since = this.now();
        const record: FinalizeRecordData = { run_id: runId, claim_generation: claimGeneration, since };
        const serialized = this.seal(MAC_DOMAIN_FINALIZE, record);
        const dst = path.join(dir, finalizeFileName(claimGeneration));
        try {
          await this.withReserveOnEnospc(async () => {
            await this.rawWrite(
              async () => {
                await fs.mkdir(dir, { recursive: true, mode: 0o700 });
              },
              { path: dir, kind: "finalize" },
            );
            await this.fsyncDir(this.root, true);
            await this.installExclusive(dst, serialized, "finalize");
          });
        } catch (err) {
          return fail(isENOSPC(err) ? "enospc" : `write_failed: ${errText(err)}`);
        }
        {
          // Bind new installs and adopted winners to authenticated descriptor metadata.
          const existing = await this.readAuthed(dst, MAC_DOMAIN_FINALIZE);
          const meta = existing ? coerceFinalize(existing, runId, claimGeneration) : null;
          if (!meta) {
            this.runs.get(runId)?.finalizes.delete(claimGeneration);
            return fail("existing_finalize_invalid");
          }
          since = meta.since;
          meta.lifetime = this.authenticatedLifetimes.get(existing!);
          const rs = this.ensureInMemoryRun(runId, since);
          const previous = rs.finalizes.get(claimGeneration);
          if (!previous || previous.lifetime !== meta.lifetime || previous.since !== since) rs.finalizes.set(claimGeneration, meta);
        }
        this.log.info("finalize record durable", { run_id: runId, claim_generation: claimGeneration });
        return { written: true };
      });
    } catch (err) {
      return fail(`unexpected: ${errText(err)}`);
    }
  }

  /** Issue #1742: the finalize records to offer on a register, EXCLUDING a run that also holds a
   *  pending terminal journal (the journal and its #1391 lease win). Read from memory. The api drops
   *  the WHOLE finalize_resume list on a duplicate run_id or on more than its entry cap, so the list
   *  is deduplicated by run (the HIGHEST generation wins) and capped at
   *  {@link FINALIZE_RESUME_MAX_ENTRIES}. The cap keeps the oldest `since` first (run_id tiebreak),
   *  so the selection is deterministic; the omitted records stay on disk and are offered at the NEXT
   *  BOOT's register, not a later one in this process (the snapshot is built once per boot). The cap
   *  mirrors the api's DEFAULT ACTIVE_SNAPSHOT_MAX_ENTRIES, which is operator-configurable: an api
   *  configured below the offered count drops the WHOLE list, and the worker then still retires the
   *  offered records after the accepted register. That is a documented limit. */
  listPendingFinalizes(): PendingFinalize[] {
    const best: { run_id: string; claim_generation: number; since: number }[] = [];
    for (const rs of this.runs.values()) {
      if (rs.terminals.size > 0 || this.hasUnresolvedPolicy(rs.runId)) continue;
      let top: FinalizeMeta | undefined;
      for (const f of rs.finalizes.values()) {
        if (!top || f.claimGeneration > top.claimGeneration) top = f;
      }
      if (top) best.push({ run_id: rs.runId, claim_generation: top.claimGeneration, since: top.since });
    }
    best.sort((a, b) => a.since - b.since || (a.run_id < b.run_id ? -1 : a.run_id > b.run_id ? 1 : 0));
    const omitted = Math.max(0, best.length - FINALIZE_RESUME_MAX_ENTRIES);
    if (omitted > 0) {
      this.log.warn("finalize records over the register cap; omitting the newest", {
        offered: FINALIZE_RESUME_MAX_ENTRIES,
        omitted,
      });
    }
    return best
      .slice(0, FINALIZE_RESUME_MAX_ENTRIES)
      .map((e) => ({ run_id: e.run_id, claim_generation: e.claim_generation }));
  }

  /** All authenticated exact generations for live retry; register's capped view stays separate. */
  listPendingFinalizeGenerations(includeTerminal = false): PendingFinalize[] {
    const entries: PendingFinalize[] = [];
    for (const rs of this.runs.values()) {
      if (this.hasUnresolvedPolicy(rs.runId) || (!includeTerminal && rs.terminals.size > 0)) continue;
      for (const generation of rs.finalizes.keys()) {
        entries.push({ run_id: rs.runId, claim_generation: generation });
      }
    }
    return entries;
  }

  /** Opaque stable identity of the authenticated in-memory record; timestamps are not identity. */
  finalizeRecordIdentity(runId: string, generation: number): Readonly<object> | undefined {
    const meta = this.runs.get(runId)?.finalizes.get(generation);
    return meta?.lifetime ? meta : undefined;
  }

  /** Retry only the authorized exact generation. Recheck custody and synchronous eligibility
   * under the run lock; cancellation also prevents an expired queued waiter from deleting. */
  async retireFinalizeIfEligible(
    runId: string,
    claimGeneration: number,
    eligible: () => boolean,
    signal: AbortSignal,
    expectedIdentity: Readonly<object>,
    reauthenticate = false,
    proofBudget?: { signal: AbortSignal; deadline: number },
  ): Promise<boolean> {
    if (this.disabled) return false;
    return this.withRunLock(runId, async () => {
      const rs = this.runs.get(runId);
      if (!expectedIdentity || rs?.finalizes.get(claimGeneration) !== expectedIdentity ||
          !this.validRunId(runId) || !rs?.finalizes.has(claimGeneration) ||
          rs.terminals.size > 0 || this.hasUnresolvedPolicy(runId) || signal.aborted || !eligible()) return false;
      if (await this.policyBlocksFinalize(runId, proofBudget ?? { signal }) || signal.aborted || !eligible()) return false;
      const file = path.join(this.runDir(runId), finalizeFileName(claimGeneration));
      if (reauthenticate) {
        const parsed = await this.readAuthed(file, MAC_DOMAIN_FINALIZE, proofBudget);
        const authenticated = parsed && coerceFinalize(parsed, runId, claimGeneration);
        const meta = rs.finalizes.get(claimGeneration);
        const lifetime = parsed && this.authenticatedLifetimes.get(parsed);
        if (authenticated && lifetime && meta && lifetime !== meta.lifetime) {
          // A newly authenticated physical record requires a fresh caller proof.
          rs.finalizes.set(claimGeneration, { ...authenticated, lifetime });
          return false;
        }
        if (!authenticated || !meta || !meta.lifetime || this.authenticatedLifetimes.get(parsed!) !== meta.lifetime || authenticated.since !== meta.since ||
            meta !== expectedIdentity || signal.aborted || proofBudget?.signal.aborted ||
            (proofBudget && Date.now() >= proofBudget.deadline) || !eligible()) return false;
        if (!await this.installedLifetimeMatches(file, meta.lifetime, proofBudget, this.authenticatedFileVersions.get(parsed!)) || signal.aborted || !eligible()) return false;
        await fs.unlink(file);
      } else await fs.unlink(file);
      rs.finalizes.delete(claimGeneration);
      return true;
    }, signal);
  }

  /** Issue #1742: retire one finalize record (unlink the file, drop it from the pending set). */
  async retireFinalize(runId: string, claimGeneration: number): Promise<boolean> {
    if (this.disabled) return false;
    return this.withRunLock(runId, async () => {
      if (!this.validRunId(runId) || !this.runs.get(runId)?.finalizes.has(claimGeneration) ||
          this.hasUnresolvedPolicy(runId) || await this.policyBlocksFinalize(runId)) return false;
      try {
        await fs.unlink(path.join(this.runDir(runId), finalizeFileName(claimGeneration)));
      } catch { return false; }
      this.runs.get(runId)?.finalizes.delete(claimGeneration);
      return true;
    });
  }

  /** Compatibility batch API: freeze original identities before any asynchronous check.
   * Attempts are bounded by the stored generation count; a rejected check stops this batch. */
  async retireFinalizes(entries: readonly PendingFinalize[], mayRetire: (runId: string, generation: number) => Promise<boolean>): Promise<void> {
    const candidates = this.listPendingFinalizeGenerations(true).filter(record =>
      entries.some(entry => entry.run_id === record.run_id && record.claim_generation <= entry.claim_generation))
      .map(entry => ({ entry, identity: this.finalizeRecordIdentity(entry.run_id, entry.claim_generation) }));
    const signal = new AbortController().signal;
    for (const { entry, identity } of candidates) {
      if (identity && await mayRetire(entry.run_id, entry.claim_generation)) {
        await this.retireFinalizeIfEligible(entry.run_id, entry.claim_generation, () => true, signal, identity);
      }
    }
  }

  /** Get an existing in-memory run entry, or create one WITHOUT writing a manifest to disk — a run
   *  may carry a terminal journal with no message manifest, and forcing an empty manifest write here
   *  would be a needless durable side effect. */
  private ensureInMemoryRun(runId: string, since: number): RunState {
    const existing = this.runs.get(runId);
    if (existing) return existing;
    const manifest: ManifestData = {
      version: 1,
      runId,
      generation: 0,
      records: [],
      cursor: 0,
      spilledUnclean: false,
      staleRetired: 0,
      since,
      updatedAt: since,
    };
    const rs: RunState = { runId, manifest, recordBytes: new Map(), terminals: new Map(), finalizes: new Map(), policies: new Map(), policyProtection: false };
    this.runs.set(runId, rs);
    return rs;
  }

  // ── manifest / record IO ───────────────────────────────────────────────────────

  private runDir(runId: string): string {
    return path.join(this.root, runId);
  }

  /** True iff `runId` is a bare path component safe to `path.join` under the root.
   *  A `..`, `.`, empty string, or one carrying a separator or NUL would escape
   *  the root for a write AND for the recursive delete in removeRun/retireRun/the
   *  retention sweep. `runId` is a server UUID today, so this is latent — but a
   *  recursive delete must never be handed an escaping component (H1). Rejection
   *  logs a warning; the caller fails closed (no write, no delete). */
  private validRunId(runId: string): boolean {
    if (runId === "" || runId === "." || runId === ".." || runId.includes("/") || runId.includes("\\") || runId.includes("\0") || path.basename(runId) !== runId) {
      this.log.warn("outbox: refusing non-bare runId (path escape guard)", { fact: "invalid_run_id" });
      return false;
    }
    return true;
  }

  /** Serialize all mutating ops on ONE run through a promise-chain mutex: each op
   *  awaits the run's current tail, then becomes the new tail, so two mutating
   *  calls on the same run never interleave (which would let both read the same
   *  manifest generation and lose one). Different runs never share a tail, so they
   *  stay concurrent. The stored tail never rejects, so one op's failure does not
   *  wedge the run's chain (H2). */
  private withRunLock<T>(runId: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    if (signal?.aborted) return Promise.reject(signal.reason);
    const prev = this.runLocks.get(runId) ?? Promise.resolve();
    let started = false;
    let abortQueued: (() => void) | undefined;
    const result = prev.then(() => {
      if (abortQueued) signal?.removeEventListener("abort", abortQueued);
      // A cancelled waiter keeps its place in the chain but never runs fn.
      if (signal?.aborted) throw signal.reason;
      started = true;
      return fn();
    });
    const tail = result.then(
      () => undefined,
      () => undefined,
    );
    this.runLocks.set(runId, tail);
    // Drop the entry once this op is the last in the chain (bounded map growth).
    void tail.then(() => {
      if (this.runLocks.get(runId) === tail) this.runLocks.delete(runId);
    });
    if (!signal) return result;
    // Reject only queued acquisition. Once fn owns the lock, its real result
    // settles the caller; cancellation cannot claim an unfinished write or ACK.
    return new Promise<T>((resolve, reject) => {
      abortQueued = () => {
        if (abortQueued) signal.removeEventListener("abort", abortQueued);
        if (!started) reject(signal.reason);
      };
      signal.addEventListener("abort", abortQueued, { once: true });
      void result.then(resolve, reject);
    });
  }

  /** Non-blocking acquire of a run's per-run lock, for a CROSS-RUN mutation (a
   *  worker-quota eviction whose victim is not the appending run). Returns a
   *  release fn when the run's chain is idle, or null when an op already holds or
   *  awaits it — in which case the caller MUST skip the victim, never block on it
   *  (blocking risks an A-waits-B / B-waits-A deadlock and would stall a spill on
   *  another run's I/O). The check-then-take is synchronous, so no interleaving can
   *  occur between them. While the lock is held a later {@link withRunLock} on the
   *  same run chains behind it; calling the returned release fn drains that chain.
   *  This is what keeps "a victim run's manifest is only mutated under its own
   *  lock" true even across runs. */
  private tryRunLock(runId: string): (() => void) | null {
    if (this.runLocks.has(runId)) return null; // busy — do NOT block
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const tail = held.then(
      () => undefined,
      () => undefined,
    );
    this.runLocks.set(runId, tail);
    void tail.then(() => {
      if (this.runLocks.get(runId) === tail) this.runLocks.delete(runId);
    });
    let released = false;
    return () => {
      if (released) return;
      released = true;
      release();
    };
  }

  private async ensureRunState(runId: string): Promise<RunState | undefined> {
    if (!this.validRunId(runId)) return undefined; // H1: never build a write path from a non-bare runId
    const existing = this.runs.get(runId);
    if (existing) return existing;
    const dir = this.runDir(runId);
    if (await this.isSymlink(dir)) {
      this.log.warn("outbox: run dir is a symlink; refusing (skipped, not followed)", { run_id: runId });
      return undefined;
    }
    await fs.mkdir(dir, { recursive: true, mode: 0o700 });
    await this.fsyncDir(this.root); // make the new run-dir entry durable in root
    const t = this.now();
    const manifest: ManifestData = {
      version: 1,
      runId,
      generation: 0, // the first installManifest bumps this to 1 as it writes the file
      records: [],
      cursor: 0,
      spilledUnclean: false,
      staleRetired: 0,
      since: t,
      updatedAt: t,
    };
    const rs: RunState = { runId, manifest, recordBytes: new Map(), terminals: new Map(), finalizes: new Map(), policies: new Map(), policyProtection: false };
    this.runs.set(runId, rs);
    return rs;
  }

  /** Install the next manifest generation (temp -> fsync -> rename -> dir fsync)
   *  and commit it to the run's in-memory state ONLY on success, so a failed write
   *  never leaves a half-mutated manifest (and a reserve-retry recomputes cleanly). */
  private async installManifest(
    rs: RunState,
    records: ManifestRecordRef[],
    patch: ManifestPatch = {},
  ): Promise<void> {
    const cur = rs.manifest;
    const next: ManifestData = {
      version: 1,
      runId: cur.runId,
      generation: cur.generation + 1,
      records,
      cursor: patch.cursor ?? cur.cursor,
      spilledUnclean: patch.spilledUnclean ?? cur.spilledUnclean,
      staleRetired: patch.staleRetired ?? cur.staleRetired,
      since: cur.since,
      updatedAt: this.now(),
    };
    const serialized = this.seal(MAC_DOMAIN_MANIFEST, next);
    await this.writeFileAtomic(path.join(this.runDir(rs.runId), MANIFEST_FILE), serialized, "manifest");
    rs.manifest = next;
  }

  private nextFileVersion(rs: RunState, firstSeq: number, lastSeq: number): number {
    let max = 0;
    for (const r of rs.manifest.records) {
      if (r.firstSeq === firstSeq && r.lastSeq === lastSeq && r.fileVersion > max) max = r.fileVersion;
    }
    return max + 1;
  }

  private async readSegmentFile(
    runId: string,
    rec: ManifestRecordRef,
  ): Promise<{ generation: number; messages: OutgoingMessage[] } | null> {
    const parsed = await this.readAuthed(path.join(this.runDir(runId), rec.file), MAC_DOMAIN_SEGMENT);
    if (!parsed) return null;
    // Bind the segment body to its manifest record's authenticated seq range. Every
    // record in a run shares one MAC key, so a valid segment from another range in the
    // same run could be swapped in for rec.file; requiring the body's firstSeq/lastSeq
    // to equal rec's — and the messages to be non-empty, in-bounds, strictly increasing,
    // and to open on rec.firstSeq / close on rec.lastSeq — prevents replaying foreign
    // messages or mis-advancing the cursor. Gaps between kept seqs are allowed (dropped
    // seqs live in separate range records). A mismatch returns null, routing replayRecord
    // to sendGapChunks (safe per-seq gap tombstones) — the existing fallback.
    if (
      parsed.version !== 1 ||
      parsed.runId !== runId ||
      typeof parsed.generation !== "number" ||
      parsed.firstSeq !== rec.firstSeq ||
      parsed.lastSeq !== rec.lastSeq ||
      !Array.isArray(parsed.messages)
    ) {
      return null;
    }
    const messages = parsed.messages as OutgoingMessage[];
    if (
      messages.length === 0 ||
      messages[0]!.seq !== rec.firstSeq ||
      messages[messages.length - 1]!.seq !== rec.lastSeq
    ) {
      return null;
    }
    let prevSeq = rec.firstSeq - 1;
    for (const m of messages) {
      if (typeof m?.seq !== "number" || m.seq < rec.firstSeq || m.seq > rec.lastSeq || m.seq <= prevSeq) {
        return null;
      }
      prevSeq = m.seq;
    }
    return { generation: parsed.generation, messages };
  }

  /** Serialize a record body + its domain-separated MAC into the on-disk string. */
  private seal(domain: string, body: unknown): string {
    return JSON.stringify({ ...(body as object), mac: this.computeMac(domain, body) });
  }

  private computeMac(domain: string, body: unknown): string {
    if (!this.key) throw new Error("outbox: MAC key unavailable");
    return createHmac("sha256", this.key).update(domain).update(canonicalJson(body)).digest("hex");
  }

  /** Observe one alias using a bounded no-follow regular-file read. */
  async observeTerminalAuthentication(runId: string, generation: number, fileName = terminalFileName(generation)): Promise<TerminalAuthenticationObservation> {
    if (!terminalRunUUID(runId)) return { runId, generation, fileName, kind: "malformed" };
    return (await this.inspectTerminal(runId, generation, fileName)).observation;
  }

  private async readTerminalAuthed(runId: string, generation: number, fileName = this.runs.get(runId)?.terminals.get(generation)?.fileName ?? terminalFileName(generation), budget?: ProofBudget): Promise<Record<string, unknown> | null> {
    const result = await this.inspectTerminal(runId, generation, fileName, budget);
    try { checkProofBudget(budget); } catch { return null; }
    return result.record ?? null;
  }

  private async terminalDirectorySafe(runId: string, budget?: ProofBudget): Promise<boolean> {
    if (!this.validRunId(runId)) return false;
    checkProofBudget(budget);
    const root = await fs.lstat(this.root);
    checkProofBudget(budget);
    const dir = await fs.lstat(this.runDir(runId));
    checkProofBudget(budget);
    if (!root.isDirectory() || root.isSymbolicLink() || !dir.isDirectory() || dir.isSymbolicLink()) return false;
    const realRoot = await this.realpath(this.root);
    checkProofBudget(budget);
    const realRun = await this.realpath(this.runDir(runId));
    checkProofBudget(budget);
    const expected = path.join(realRoot, runId);
    return realRun === expected || (terminalRunUUID(runId) &&
      path.dirname(realRun) === realRoot &&
      path.basename(realRun).toLowerCase() === path.basename(expected).toLowerCase());
  }

  private async inspectTerminal(runId: string, generation: number | undefined, fileName: string, budget?: ProofBudget): Promise<{ observation: TerminalAuthenticationObservation; record?: Record<string, unknown> }> {
    const observation: TerminalAuthenticationObservation = { runId: terminalRunUUID(runId) ? runId.toLowerCase() : runId, physicalRunId: runId, generation, fileName, kind: "malformed" };
    const result = { observation } as { observation: TerminalAuthenticationObservation; record?: Record<string, unknown> };
    if (!this.validRunId(runId) || generation === undefined || !Number.isSafeInteger(generation) || generation < 0 || parseTerminalFileName(fileName) !== generation) return result;
    let fh: Awaited<ReturnType<typeof fs.open>> | undefined;
    try {
      if (!await this.terminalDirectorySafe(runId, budget)) { observation.kind = "symlink"; return result; }
      const filePath = path.join(this.runDir(runId), fileName);
      checkProofBudget(budget);
      const before = await fs.lstat(filePath, { bigint: true });
      checkProofBudget(budget);
      if (before.isSymbolicLink()) { observation.kind = "symlink"; return result; }
      if (!before.isFile()) { observation.kind = "unreadable"; return result; }
      fh = await fs.open(filePath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
      checkProofBudget(budget);
      const stat = await fh.stat({ bigint: true });
      checkProofBudget(budget);
      if (!stat.isFile() || stat.ino !== before.ino || stat.dev !== before.dev) { observation.kind = "unreadable"; return result; }
      const raw = Buffer.alloc(this.terminalReadMaxBytes + 1);
      let length = 0;
      // At most cap+1 bytes and cap+1 nonempty read attempts; EOF ends sooner.
      while (length < raw.length) {
        checkProofBudget(budget);
        const { bytesRead } = await fh.read(raw, length, raw.length - length, null);
        checkProofBudget(budget);
        if (!bytesRead) break;
        length += bytesRead;
      }
      if (length > this.terminalReadMaxBytes) { observation.kind = "oversized"; return result; }
      checkProofBudget(budget);
      const after = await fs.lstat(filePath, { bigint: true });
      checkProofBudget(budget);
      const final = await fh.stat({ bigint: true });
      checkProofBudget(budget);
      if (after.ino !== stat.ino || after.dev !== stat.dev || final.size !== stat.size || final.mtimeNs !== stat.mtimeNs || final.ctimeNs !== stat.ctimeNs || !await this.terminalDirectorySafe(runId, budget)) { observation.kind = "unreadable"; return result; }
      observation.fingerprint = { dev: String(stat.dev), ino: String(stat.ino), sha256: createHash("sha256").update(raw.subarray(0, length)).digest("hex") };
      if (!this.key) { observation.kind = "key_unavailable"; return result; }
      let parsed: unknown;
      try { parsed = JSON.parse(raw.subarray(0, length).toString("utf8")); } catch { return result; }
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return result;
      const obj = { ...parsed as Record<string, unknown> };
      const mac = obj.mac;
      if (typeof mac !== "string" || !/^[0-9a-f]{64}$/i.test(mac)) return result;
      delete obj.mac;
      if (!macEqual(mac, this.computeMac(MAC_DOMAIN_TERMINAL, obj))) {
        observation.kind = "mac_failure";
        // Only the selected authenticated filename loses trust; sibling aliases and unknown reads preserve retry eligibility.
        if (this.runs.get(runId)?.terminals.get(generation)?.fileName === fileName) this.runs.get(runId)?.terminals.delete(generation);
        if (terminalRunUUID(runId)) this.log.warn("outbox: terminal authentication rejected", { run_id: runId.toLowerCase(), claim_generation: generation, fact: "mac_failure" });
        return result;
      }
      if (!coerceTerminalRecord(obj, runId, generation)) return result;
      observation.kind = "authenticated";
      const lifetime = fileLifetime(stat);
      this.authenticatedLifetimes.set(obj, lifetime);
      this.authenticatedFileVersions.set(obj, fileVersion(stat));
      const meta = this.runs.get(runId)?.terminals.get(generation);
      const binding = meta && this.terminalRetirementIdentities.get(meta);
      if (meta?.fileName === fileName && binding && binding.lifetime !== lifetime) this.terminalRetirementIdentities.delete(meta);
      result.record = obj;
      return result;
    } catch (err) {
      const code = (err as NodeJS.ErrnoException).code;
      observation.kind = code === "ENOENT" ? "absent" : code === "ELOOP" ? "symlink" : "unreadable";
      return result;
    } finally {
      if (observation.kind !== "authenticated" && generation !== undefined) {
        const meta = this.runs.get(runId)?.terminals.get(generation);
        const binding = meta?.fileName === fileName && this.terminalRetirementIdentities.get(meta);
        if (binding) binding.serverSettled = false;
      }
      if (fh) await fh.close();
    }
  }

  /** Physical terminals protect the entire run subtree, including unknown/unsafe aliases.
   * A directory read failure conservatively protects it, independent of authenticated counts. */
  async hasPhysicalTerminalProtection(runId: string): Promise<boolean> {
    return this.hasPhysicalControlProtection(runId);
  }

  private async hasPhysicalControlProtection(runId: string, policiesOnly = false): Promise<boolean> {
    if (!this.validRunId(runId)) return true;
    // Only initial path checks may establish ordinary absence. Once scanning starts,
    // all errors protect the run, including a directory read reporting ENOENT.
    try {
      const root = await fs.lstat(this.root);
      if (!root.isDirectory() || root.isSymbolicLink()) return true;
    } catch (err) {
      return (err as NodeJS.ErrnoException).code !== "ENOENT";
    }
    let runExists = true;
    try {
      await fs.lstat(this.runDir(runId));
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") return true;
      runExists = false;
    }
    const protects = async (physicalRunId: string): Promise<boolean> => {
      if (!await this.terminalDirectorySafe(physicalRunId)) return true;
      const dir = await fs.opendir(this.runDir(physicalRunId));
      try {
        // Stream to EOF or the first protecting entry, without retries or an entry cap.
        while (true) {
          const entry = await dir.read();
          if (!entry) return false;
          if (entry.name.startsWith(POLICY_FILE_PREFIX) || (!policiesOnly && entry.name.startsWith(TERMINAL_FILE_PREFIX))) return true;
        }
      } finally {
        await dir.close();
      }
    };
    try {
      if (runExists && await protects(runId)) return true;
      if (!terminalRunUUID(runId)) return false;
      const root = await fs.opendir(this.root);
      try {
        // Case-alias discovery also ends at EOF or the first protecting alias.
        while (true) {
          const entry = await root.read();
          if (!entry) return false;
          if (entry.name !== runId && terminalRunUUID(entry.name) && entry.name.toLowerCase() === runId.toLowerCase() &&
              await protects(entry.name)) return true;
        }
      } finally {
        await root.close();
      }
    } catch {
      return true;
    }
  }

  /** One persistent cursor, serialized across callers. Each page visits at most 256 physical
   * entries (directories included) and returns at most 256 observations. The cursor advances
   * before transport, so failed requests cannot starve newer files. EOF starts a fresh pass. */
  scanTerminalObservationsPage(): Promise<TerminalObservationPage> {
    const next = this.terminalScanTail.then(() => this.scanTerminalPage());
    this.terminalScanTail = next.catch(() => undefined);
    return next;
  }

  async closeTerminalObservationScan(): Promise<void> {
    await this.terminalScanTail;
    if (this.terminalScanRun) await this.terminalScanRun.dir.close().catch(() => undefined);
    if (this.terminalScanRoot) await this.terminalScanRoot.close().catch(() => undefined);
    this.terminalScanRun = undefined;
    this.terminalScanRoot = undefined;
  }

  private async scanTerminalPage(): Promise<TerminalObservationPage> {
    const page: TerminalObservationPage = { observations: [], passComplete: false };
    try {
      if (!this.terminalScanRoot) {
        if (!(await fs.lstat(this.root)).isDirectory()) throw new Error("unsafe root");
        this.terminalScanRoot = await fs.opendir(this.root);
      }
      for (let visited = 0; visited < 256; visited++) {
        if (this.terminalScanRun) {
          const entry = await this.terminalScanRun.dir.read();
          if (!entry) {
            await this.terminalScanRun.dir.close();
            this.terminalScanRun = undefined;
            continue;
          }
          if (entry.name.startsWith(TERMINAL_FILE_PREFIX)) {
            const result = await this.inspectTerminal(this.terminalScanRun.id, parseTerminalFileName(entry.name), entry.name);
            page.observations.push(result.observation);
          }
        } else {
          const entry = await this.terminalScanRoot.read();
          if (!entry) {
            await this.terminalScanRoot.close();
            this.terminalScanRoot = undefined;
            page.passComplete = true;
            break;
          }
          if (entry.isDirectory() && terminalRunUUID(entry.name)) {
            try {
              if (await this.terminalDirectorySafe(entry.name)) this.terminalScanRun = { id: entry.name, dir: await fs.opendir(this.runDir(entry.name)) };
            } catch { /* One inaccessible run does not block sibling runs this pass. */ }
          }
        }
      }
      return page;
    } catch (err) {
      if (this.terminalScanRun) await this.terminalScanRun.dir.close().catch(() => undefined);
      if (this.terminalScanRoot) await this.terminalScanRoot.close().catch(() => undefined);
      this.terminalScanRun = undefined;
      this.terminalScanRoot = undefined;
      throw err;
    }
  }

  /** Exact aliases only; the coordinator supplies fresh custody authority INSIDE the run lock.
   * Diagnostic POST acknowledgments are not accepted. Unknown observations are never deleted.
   * The worker's run lock serializes worker mutations; the existing single-UID residual applies. */
  async cleanupRejectedTerminalFiles(
    runId: string,
    generation: number,
    workerId: string,
    observations: readonly TerminalAuthenticationObservation[],
    freshCustody: () => Promise<TerminalRejectionCustodyResponse | undefined>,
    canDelete: () => boolean = () => true,
  ): Promise<number> {
    if (!terminalRunUUID(runId) || !terminalRunUUID(workerId) || !Number.isSafeInteger(generation) || generation < 0 || observations.length === 0 || observations.length > 256) return 0;
    runId = runId.toLowerCase();
    workerId = workerId.toLowerCase();
    const physicalRunId = observations[0]!.physicalRunId ?? runId;
    if (!terminalRunUUID(physicalRunId) || physicalRunId.toLowerCase() !== runId || observations.some(o => (o.physicalRunId ?? runId) !== physicalRunId)) return 0;
    // Do not hold admission while queued behind an unrelated outbox write. Busy runs
    // remain protected and are revisited from the physical scan on its next pass.
    const release = this.tryRunLock(physicalRunId);
    if (!release) return 0;
    try {
      if (!canDelete()) return 0;
      const same = (a: TerminalAuthenticationObservation, b: TerminalAuthenticationObservation) =>
        a.kind === "mac_failure" && b.kind === "mac_failure" && a.runId === runId && a.generation === generation &&
        a.fileName === b.fileName && a.fingerprint !== undefined && b.fingerprint !== undefined &&
        a.fingerprint.dev === b.fingerprint.dev && a.fingerprint.ino === b.fingerprint.ino && a.fingerprint.sha256 === b.fingerprint.sha256;
      const names = new Set<string>();
      for (const observed of observations) {
        if (!canDelete()) return 0;
        if (names.has(observed.fileName) || !same(observed, await this.observeTerminalAuthentication(physicalRunId, generation, observed.fileName))) return 0;
        names.add(observed.fileName);
      }
      if (!canDelete()) return 0;
      const custody = await freshCustody();
      if (!canDelete()) return 0;
      if (!custody || custody.run_id !== runId || custody.worker_id !== workerId || custody.generation !== generation ||
          custody.outcome !== "settled" || !custody.complete || !custody.exact_complete || !custody.sibling_complete ||
          !Number.isSafeInteger(custody.exact_count) || custody.exact_count < 1 || custody.exact_count > 256 ||
          custody.exact_holds.length !== custody.exact_count || custody.exact_holds.some(h => !terminalRunUUID(h.id) || (h.state !== "released" && h.state !== "discarded")) ||
          new Set(custody.exact_holds.map(h => h.id)).size !== custody.exact_count || custody.sibling_count !== 0 || custody.sibling_holds.length !== 0) return 0;
      let removed = 0;
      for (const observed of observations) {
        if (!same(observed, await this.observeTerminalAuthentication(physicalRunId, generation, observed.fileName))) return removed;
        if (!canDelete()) return removed;
        await fs.unlink(path.join(this.runDir(physicalRunId), observed.fileName));
        await this.fsyncDir(this.runDir(physicalRunId), true);
        removed++;
      }
      return removed;
    } finally {
      release();
    }
  }

  /** Recheck the installed physical lifetime under O immediately before report unlink. */
  private async installedLifetimeMatches(file: string, lifetime: string, budget?: ProofBudget, version?: string): Promise<boolean> {
    try {
      checkProofBudget(budget);
      const stat = await fs.lstat(file, { bigint: true });
      checkProofBudget(budget);
      return stat.isFile() && !stat.isSymbolicLink() && fileLifetime(stat) === lifetime &&
        (version === undefined || fileVersion(stat) === version);
    } catch { return false; }
  }

  /** Read and authenticate through a no-follow descriptor; finalize reads also bind its physical lifetime. */
  private async readAuthed(filePath: string, domain: string,
    proofBudget?: ProofBudget,
  ): Promise<Record<string, unknown> | null> {
    if (!this.key) return null;
    let raw: Buffer;
    let lifetime: string | undefined;
    let version: string | undefined;
    try {
      const check = () => checkProofBudget(proofBudget);
      check();
      const fh = await fs.open(filePath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW |
        (proofBudget || domain === MAC_DOMAIN_POLICY ? fsConstants.O_NONBLOCK : 0));
      try {
        check();
        if (!proofBudget && domain !== MAC_DOMAIN_POLICY) {
          if (domain === MAC_DOMAIN_FINALIZE) {
            const before = await fh.stat({ bigint: true });
            if (!before.isFile()) return null;
            raw = await fh.readFile();
            const after = await fh.stat({ bigint: true });
            if (after.size !== before.size || after.mtimeNs !== before.mtimeNs || after.ctimeNs !== before.ctimeNs ||
                !await this.installedLifetimeMatches(filePath, fileLifetime(before), undefined, fileVersion(before))) return null;
            lifetime = fileLifetime(before);
            version = fileVersion(before);
          } else raw = await fh.readFile();
        } else {
          const cap = domain === MAC_DOMAIN_POLICY ? POLICY_MAX_BYTES : 1024 * 1024;
          const before = await fh.stat({ bigint: true });
          check();
          if (!before.isFile() || before.size > BigInt(cap)) return null;
          const same = (a: typeof before, b: typeof before) => b.isFile() &&
            a.dev === b.dev && a.ino === b.ino && a.size === b.size &&
            a.mtimeNs === b.mtimeNs && a.ctimeNs === b.ctimeNs;
          const installed = await fs.lstat(filePath, { bigint: true });
          check();
          if (!same(before, installed)) return null;
          const bytes = Buffer.alloc(cap + 1);
          let used = 0;
          // At most cap+1 bytes and cap+2 reads (including EOF); a short read is
          // not EOF. Any failed read stops this single record proof.
          while (used < bytes.length) {
            check();
            const { bytesRead } = await fh.read(bytes, used, Math.min(64 * 1024, bytes.length - used), used);
            check();
            if (bytesRead === 0) break;
            used += bytesRead;
          }
          if (used > cap) return null;
          const final = await fh.stat({ bigint: true });
          check();
          const after = await fs.lstat(filePath, { bigint: true });
          check();
          if (!same(before, final) || !same(before, after)) return null;
          lifetime = fileLifetime(before);
          version = fileVersion(before);
          raw = bytes.subarray(0, used);
        }
      } finally {
        await fh.close();
      }
      check();
    } catch {
      return null; // ENOENT (missing) or ELOOP (symlink) — nothing trustworthy here
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw.toString("utf8"));
    } catch {
      this.log.warn("outbox: unparseable record; refusing", { file: filePath });
      return null;
    }
    if (typeof parsed !== "object" || parsed === null) return null;
    const obj = { ...(parsed as Record<string, unknown>) };
    const mac = obj.mac;
    if (typeof mac !== "string") return null;
    delete obj.mac;
    if (!macEqual(mac, this.computeMac(domain, obj))) {
      this.log.warn("outbox: record MAC mismatch; refusing tampered record", { file: filePath });
      return null;
    }
    if (lifetime) this.authenticatedLifetimes.set(obj, lifetime);
    if (version) this.authenticatedFileVersions.set(obj, version);
    return obj;
  }

  // ── key + reserve ────────────────────────────────────────────────────────────

  private async mintKey(keyPath: string): Promise<Buffer> {
    const key = randomBytes(OUTBOX_KEY_BYTES);
    const tmp = `${keyPath}.${randomUUID()}.tmp`;
    const fh = await fs.open(
      tmp,
      fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_NOFOLLOW,
      0o600,
    );
    try {
      await fh.writeFile(key);
      await fh.sync();
    } finally {
      await fh.close();
    }
    let result: Buffer = key;
    try {
      // fs.link is atomic and throws EEXIST if the key already exists — a
      // no-replace install, so a concurrent minter cannot clobber the winner.
      await fs.link(tmp, keyPath);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "EEXIST") {
        result = await this.readKeyFile(keyPath); // adopt the key that won
      } else {
        await fs.rm(tmp, { force: true }).catch(() => undefined);
        throw err;
      }
    }
    await this.fsyncDir(path.dirname(keyPath));
    await fs.rm(tmp, { force: true }).catch(() => undefined);
    return result;
  }

  private async readKeyFile(keyPath: string): Promise<Buffer> {
    const fh = await fs.open(keyPath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    try {
      const buf = await fh.readFile();
      if (buf.length !== OUTBOX_KEY_BYTES) throw new Error(`.key is ${buf.length} bytes, expected ${OUTBOX_KEY_BYTES}`);
      return buf;
    } finally {
      await fh.close();
    }
  }

  private reservePath(): string {
    return path.join(this.root, RESERVE_FILE);
  }

  /** Preallocate the reserve file, GROWING an undersized existing one to the configured size
   *  (best-effort). A deployed Run A worker whose `.reserve` is the old 64 KiB must come up with
   *  the larger terminal-sized reserve, so this STATS the file and replaces it atomically (temp →
   *  fsync → rename → dir fsync, symlink-refusing — mirroring {@link replenishReserve}'s hardening)
   *  whenever it is absent or smaller than {@link reserveBytes}. An already-large-enough reserve is
   *  left untouched (never shrunk, so an operator who lowered the count keeps the headroom until it
   *  is next consumed and replenished). */
  private async ensureReserve(): Promise<void> {
    try {
      const p = this.reservePath();
      if (await this.isSymlink(p)) {
        this.log.warn("outbox: .reserve is a symlink; refusing to (re)allocate it (never followed)", { path: p });
        return;
      }
      let size = -1;
      try {
        size = (await fs.stat(p)).size;
      } catch {
        size = -1; // absent
      }
      if (size >= this.reserveTargetBytes) return; // already at/above the configured size
      await this.growReserveAtomic();
    } catch (err) {
      this.log.warn("outbox: could not (pre)allocate reserve (range/terminal writes may fail on a full volume)", {
        error: errText(err),
        ...(await this.diskFullCause(err)),
      });
    }
  }

  /** Grow/replace the reserve atomically: temp file (0600, O_EXCL|O_NOFOLLOW) → fsync → rename →
   *  parent-dir fsync, so a crash mid-grow never leaves a truncated `.reserve` (crash-atomicity, D2).
   *  The `.reserve` is opaque padding and carries no MAC. */
  private async growReserveAtomic(): Promise<void> {
    const dst = this.reservePath();
    const tmp = `${dst}.${randomUUID()}.tmp`;
    try {
      const fh = await fs.open(
        tmp,
        fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_NOFOLLOW,
        0o600,
      );
      try {
        await fh.writeFile(Buffer.alloc(this.reserveTargetBytes));
        await fh.sync();
      } finally {
        await fh.close();
      }
    } catch (err) {
      await fs.rm(tmp, { force: true }).catch(() => undefined);
      throw err;
    }
    await fs.rename(tmp, dst);
    await this.fsyncDir(path.dirname(dst));
  }

  private async releaseReserve(preserveBytes = 0): Promise<void> {
    if (preserveBytes === 0) {
      await fs.rm(this.reservePath(), { force: true }).catch(() => undefined);
      return;
    }
    const fh = await fs.open(this.reservePath(), fsConstants.O_WRONLY | fsConstants.O_NOFOLLOW);
    try {
      const stat = await fh.stat();
      if (!stat.isFile() || stat.size < preserveBytes) throw new Error("policy reserve unavailable");
      await fh.truncate(preserveBytes);
      await fh.sync();
    } finally { await fh.close(); }
  }

  private async replenishReserve(): Promise<void> {
    // Plain write (not the seam-wrapped atomic writer): the reserve is opaque
    // padding, needs no MAC, and must stay writable in the ENOSPC retry path. But
    // it opens O_NOFOLLOW like every other write in the module, so a symlinked
    // `.reserve` is never followed (consistency with mintKey/writeFileAtomic).
    try {
      const fh = await fs.open(
        this.reservePath(),
        fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_TRUNC | fsConstants.O_NOFOLLOW,
        0o600,
      );
      try {
        await fh.writeFile(Buffer.alloc(this.reserveTargetBytes));
      } finally {
        await fh.close();
      }
    } catch (err) {
      this.log.warn("outbox: could not replenish reserve", { error: errText(err), ...(await this.diskFullCause(err)) });
    }
  }

  /**
   * PRD #1809 D6: `{cause: "data_volume_full"}` when the classifier attributes `err` to a full
   * data volume, else nothing. Never throws.
   *
   * Why a reserve write that fails on a full volume only tags the log and never parks the run: no
   * run-state transition depends on the outbox being writable. Park and other non-terminal state
   * reports are sent straight to the claim-fenced `POST /runs/{id}/state` (WorkerClient.reportState,
   * stamped with claim_generation), never through the outbox. A terminal report is journaled here
   * write-ahead, but when even the released reserve cannot admit the journal it is sent unjournaled
   * over that same claim-fenced call ({@link journalTerminal}'s `reserve_exhausted`), and the
   * message spill's drop path writes its range record from the same reserve
   * ({@link appendRangeRecord}). So a full outbox volume costs durability across a restart, not a
   * run-state report, and the park belongs to the data-volume handling around the clone/fetch.
   */
  private async diskFullCause(err: unknown): Promise<{ cause?: "data_volume_full" }> {
    if (!this.classifyWriteFailure) return {};
    try {
      return (await this.classifyWriteFailure(err, this.root)) === "data_volume_full" ? { cause: "data_volume_full" } : {};
    } catch {
      return {};
    }
  }

  /** Run `fn`, and on `ENOSPC` release the reserve to free space, retry once, then
   *  replenish the reserve. Used for reserve-backed range and terminal writes (D2). */
  private async withReserveOnEnospc<T>(fn: () => Promise<T>, preserveBytes = 0): Promise<T> {
    try {
      return await fn();
    } catch (err) {
      if (!isENOSPC(err)) throw err;
      this.log.warn("outbox: ENOSPC on a reserve-backed write; releasing reserve to admit it", {});
      await this.releaseReserve(preserveBytes);
      try {
        return await fn();
      } finally {
        await this.replenishReserve();
      }
    }
  }

  // ── atomic file IO ─────────────────────────────────────────────────────────────

  /** Atomic, crash-safe install: temp file (0600, O_EXCL|O_NOFOLLOW) -> write ->
   *  fsync(file) -> rename -> fsync(parent dir). The one raw write that can hit
   *  ENOSPC is routed through the seam so a test can simulate a full volume. */
  private async writeFileAtomic(
    dstPath: string,
    data: string,
    kind: RecordFileKind | "manifest" | "terminal" | "policy",
    expected?: { runId: string; lifetime: string; version: string },
  ): Promise<string | undefined> {
    const dir = path.dirname(dstPath);
    const tmp = `${dstPath}.${randomUUID()}.tmp`;
    let lifetime: string | undefined;
    const doWrite = async () => {
      const fh = await fs.open(
        tmp,
        fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_NOFOLLOW,
        0o600,
      );
      try {
        await fh.writeFile(data, "utf8");
        await fh.sync();
        if (kind === "terminal") lifetime = fileLifetime(await fh.stat({ bigint: true }));
      } finally {
        await fh.close();
      }
    };
    try {
      await this.rawWrite(doWrite, { path: tmp, kind });
    } catch (err) {
      await fs.rm(tmp, { force: true }).catch(() => undefined);
      throw err;
    }
    try {
      if (expected && (!await this.terminalDirectorySafe(expected.runId) ||
          !await this.installedLifetimeMatches(dstPath, expected.lifetime, undefined, expected.version))) {
        throw new Error("policy binding observation changed");
      }
      await fs.rename(tmp, dstPath);
      await this.fsyncDir(dir, kind === "policy");
    } catch (err) {
      await fs.rm(tmp, { force: true }).catch(() => undefined);
      throw err;
    }
    return lifetime;
  }

  private async fsyncDir(dir: string, strict = false): Promise<void> {
    let dh: Awaited<ReturnType<typeof fs.open>> | undefined;
    try {
      dh = await fs.open(dir, fsConstants.O_RDONLY);
      await dh.sync();
    } catch (err) {
      // Finalize attestation requires durable directory entries. Existing message/terminal
      // writes retain their best-effort behavior on filesystems that reject directory fsync.
      if (strict) throw err;
      this.log.debug("outbox: directory fsync skipped", { dir, error: errText(err) });
    } finally {
      if (dh) await dh.close().catch(() => undefined);
    }
  }

  private async isSymlink(p: string): Promise<boolean> {
    try {
      return (await fs.lstat(p)).isSymbolicLink();
    } catch {
      return false; // missing → not a symlink
    }
  }

  private writable(): boolean {
    if (this.disabled || !this.key) {
      this.log.warn("outbox: store disabled; dropping write (fail-closed)", {});
      return false;
    }
    return true;
  }
}

function positiveSafe(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function closedObject(value: unknown, allowed: readonly string[], required = allowed): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).every(key => allowed.includes(key)) &&
    required.every(key => Object.hasOwn(value, key));
}

function validPolicyOutcome(value: unknown): value is ImmutablePolicyOutcome {
  const required = ["outcome_id", "source_generation", "detail", "head", "published"];
  const optional = ["tag", "phase", "correlation_id", "session_id", "checkpoint_contains_latest", "expected_gate_revision"];
  if (!closedObject(value, [...required, ...optional], required)) return false;
  const v = value;
  if (typeof v.outcome_id !== "string" || v.outcome_id.length !== 36 ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(v.outcome_id) ||
      v.outcome_id === "00000000-0000-0000-0000-000000000000" || !positiveSafe(v.source_generation) ||
      (v.detail !== "refused" && v.detail !== "resume_rejected")) return false;
  if (v.detail === "refused" && !["tag", "phase", "correlation_id"].every(key => Object.hasOwn(v, key))) return false;
  if (Object.hasOwn(v, "tag") && !isPolicyRefusalTag(v.tag)) return false;
  if (Object.hasOwn(v, "phase") && v.phase !== "planning" && v.phase !== "implementation") return false;
  if (Object.hasOwn(v, "correlation_id") && !validPolicyCorrelation(v.correlation_id)) return false;
  if (Object.hasOwn(v, "session_id") && (typeof v.session_id !== "string" || !v.session_id.trim() ||
      Buffer.byteLength(v.session_id) > 512 || v.session_id.includes("\0") || /[\r\n]/.test(v.session_id))) return false;
  if (typeof v.head !== "string" || (v.head !== "" &&
      ((v.head.length !== 40 && v.head.length !== 64) || /[^0-9a-f]/.test(v.head)))) return false;
  if (typeof v.published !== "boolean" || (v.published && !v.head)) return false;
  if (Object.hasOwn(v, "checkpoint_contains_latest") && typeof v.checkpoint_contains_latest !== "boolean") return false;
  return !Object.hasOwn(v, "expected_gate_revision") || positiveSafe(v.expected_gate_revision);
}

function policyFileName(source: number): string { return `policy-${source}.json`; }
function parsePolicyFileName(name: string): number | undefined {
  const match = /^policy-([1-9][0-9]*)\.json$/.exec(name);
  const source = match && Number(match[1]);
  return positiveSafe(source) && name === policyFileName(source) ? source : undefined;
}

function coercePolicyRecord(value: unknown, runId: string, source: number): PolicyRecord | undefined {
  if (!closedObject(value, ["version", "run_id", "outcome", "delivery", "since"]) ||
      value.version !== 1 || value.run_id !== runId || !validPolicyOutcome(value.outcome) ||
      value.outcome.source_generation !== source || !closedObject(value.delivery, ["claim_generation", "inventory_generation"]) ||
      !positiveSafe(value.delivery.claim_generation) || value.delivery.claim_generation < source ||
      value.delivery.inventory_generation !== value.delivery.claim_generation ||
      typeof value.since !== "number" || !Number.isSafeInteger(value.since) || value.since < 0) return undefined;
  return value as unknown as PolicyRecord;
}

function policyContextEligible(context: ReportRetirementContext): boolean {
  return !context.signal?.aborted && (context.deadline === undefined || Date.now() < context.deadline) &&
    context.eligible?.() !== false;
}

interface ManifestPatch {
  cursor?: number;
  spilledUnclean?: boolean;
  staleRetired?: number;
}

/** Validate an untrusted parsed object into a ManifestData (shape only; the MAC is
 *  the integrity gate that already ran in readAuthed). Returns null on any
 *  mismatch. */
function coerceManifest(obj: Record<string, unknown>): ManifestData | null {
  const num = (v: unknown): v is number => typeof v === "number";
  const str = (v: unknown): v is string => typeof v === "string";
  if (obj.version !== 1 || !str(obj.runId)) return null;
  if (!num(obj.generation) || !num(obj.cursor) || !num(obj.staleRetired) || !num(obj.since) || !num(obj.updatedAt)) {
    return null;
  }
  if (typeof obj.spilledUnclean !== "boolean" || !Array.isArray(obj.records)) return null;
  const records: ManifestRecordRef[] = [];
  for (const r of obj.records) {
    if (typeof r !== "object" || r === null) return null;
    const rr = r as Record<string, unknown>;
    if (rr.kind !== "segment" && rr.kind !== "range") return null;
    if (!num(rr.firstSeq) || !num(rr.lastSeq) || !num(rr.fileVersion) || !str(rr.file)) return null;
    records.push({ kind: rr.kind, firstSeq: rr.firstSeq, lastSeq: rr.lastSeq, fileVersion: rr.fileVersion, file: rr.file });
  }
  return {
    version: 1,
    runId: obj.runId,
    generation: obj.generation,
    records,
    cursor: obj.cursor,
    spilledUnclean: obj.spilledUnclean,
    staleRetired: obj.staleRetired,
    since: obj.since,
    updatedAt: obj.updatedAt,
  };
}

/** One status tombstone for a single seq — the shape the browser renders
 *  (batcher.ts:152-180) and that keeps the stream contiguous (D2). Built one at a
 *  time by {@link Outbox.sendGapChunks} so a wide range never allocates a huge array. */
function gapTombstone(seq: number, reason: string): OutgoingMessage {
  return {
    seq,
    kind: "status",
    payload: { text: `message dropped: ${reason} (seq ${seq})`, event: "message_dropped", reason },
  };
}

// ── terminal-journal helpers (Run B / M3) ─────────────────────────────────────────

function terminalRunUUID(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

/** The `terminal-<generation>.json` filename for a claim generation (D4). */
function terminalFileName(claimGeneration: number): string {
  return `${TERMINAL_FILE_PREFIX}${claimGeneration}${TERMINAL_FILE_SUFFIX}`;
}

/** The claim generation a terminal-journal filename names, or undefined when the name is not a
 *  terminal journal or its generation is not a non-negative integer. The FILENAME is the source of
 *  truth for the generation, so a journal copied under a different name cannot masquerade as another
 *  generation (loadTerminals cross-checks the embedded field against this). */
function parseTerminalFileName(name: string): number | undefined {
  if (!name.startsWith(TERMINAL_FILE_PREFIX) || !name.endsWith(TERMINAL_FILE_SUFFIX)) return undefined;
  const mid = name.slice(TERMINAL_FILE_PREFIX.length, name.length - TERMINAL_FILE_SUFFIX.length);
  if (!/^\d+$/.test(mid)) return undefined; // no sign, no separators, no leading `+`
  const gen = Number(mid);
  return Number.isSafeInteger(gen) && gen >= 0 ? gen : undefined;
}

/** The `finalize-<generation>.json` filename for a claim generation (issue #1742). */
function finalizeFileName(claimGeneration: number): string {
  return `${FINALIZE_FILE_PREFIX}${claimGeneration}${FINALIZE_FILE_SUFFIX}`;
}

/** The claim generation a finalize-record filename names, or undefined when the name is not one. */
function parseFinalizeFileName(name: string): number | undefined {
  if (!name.startsWith(FINALIZE_FILE_PREFIX) || !name.endsWith(FINALIZE_FILE_SUFFIX)) return undefined;
  const mid = name.slice(FINALIZE_FILE_PREFIX.length, name.length - FINALIZE_FILE_SUFFIX.length);
  if (!/^\d+$/.test(mid)) return undefined;
  const gen = Number(mid);
  // Canonical names only: `finalize-03.json` parses to 3 but is not a name journalFinalize writes.
  return Number.isSafeInteger(gen) && gen >= 0 && String(gen) === mid ? gen : undefined;
}

/** Validate an authenticated finalize-record object: the embedded run id must match the directory
 *  and the embedded generation the filename. Null on any mismatch. */
function coerceFinalize(obj: Record<string, unknown>, runId: string, fileGen: number): FinalizeMeta | null {
  if (obj.run_id !== runId) return null;
  if (typeof obj.claim_generation !== "number" || obj.claim_generation !== fileGen) return null;
  if (typeof obj.since !== "number") return null;
  return { claimGeneration: obj.claim_generation, since: obj.since };
}

/** The set of legal blocked reasons, mirroring {@link TerminalBlockedReason}. */
const TERMINAL_BLOCKED_REASONS: ReadonlySet<string> = new Set<TerminalBlockedReason>([
  "completion_permit_mismatch",
  "gap_unrecoverable",
  "reserve_exhausted",
  "run_not_owned",
]);

/** Validate an authenticated terminal-journal object into in-memory metadata (shape only; the MAC
 *  already gated integrity in readAuthed). Binds the embedded `run_id` to the directory and the
 *  embedded `claim_generation` to the filename, so a copied/misfiled journal is skipped. Returns
 *  null on any mismatch. */
function coerceTerminal(obj: Record<string, unknown>, runId: string, fileGen: number, fileName = terminalFileName(fileGen)): TerminalMeta | null {
  if (obj.version !== 1) return null;
  if (obj.run_id !== runId) return null;
  if (typeof obj.claim_generation !== "number" || obj.claim_generation !== fileGen) return null;
  if (typeof obj.phase_at_journal !== "string") return null;
  if (typeof obj.since !== "number") return null;
  const blocked = obj.blocked === true;
  let blockedReason: TerminalBlockedReason | undefined;
  if (blocked && typeof obj.blocked_reason === "string" && TERMINAL_BLOCKED_REASONS.has(obj.blocked_reason)) {
    blockedReason = obj.blocked_reason as TerminalBlockedReason;
  }
  return {
    fileName,
    claimGeneration: obj.claim_generation,
    phaseAtJournal: obj.phase_at_journal,
    since: obj.since,
    blocked,
    ...(blockedReason !== undefined ? { blockedReason } : {}),
  };
}

/** Validate an authenticated terminal-journal object into the FULL send-path payload (body + fence +
 *  phase + blocked state; the MAC already gated integrity in readAuthed). Binds the embedded `run_id`
 *  to the directory and the embedded `claim_generation` to the filename, so a copied/misfiled journal
 *  is rejected. Returns null on any mismatch — a bad record proves nothing and is never sent. */
function coerceTerminalRecord(
  obj: Record<string, unknown>,
  runId: string,
  fileGen: number,
): PendingTerminalJournal | null {
  if (obj.version !== 1) return null;
  if (obj.run_id !== runId) return null;
  if (typeof obj.claim_generation !== "number" || obj.claim_generation !== fileGen) return null;
  if (typeof obj.phase_at_journal !== "string") return null;
  if (typeof obj.messages_through_seq !== "number") return null;
  if (typeof obj.body !== "object" || obj.body === null || Array.isArray(obj.body)) return null;
  const blocked = obj.blocked === true;
  let blockedReason: TerminalBlockedReason | undefined;
  if (blocked && typeof obj.blocked_reason === "string" && TERMINAL_BLOCKED_REASONS.has(obj.blocked_reason)) {
    blockedReason = obj.blocked_reason as TerminalBlockedReason;
  }
  return {
    body: obj.body as Record<string, unknown>,
    messagesThroughSeq: obj.messages_through_seq,
    phase: obj.phase_at_journal,
    blocked,
    ...(blockedReason !== undefined ? { blockedReason } : {}),
  };
}

/** The run's oldest blocked terminal reason (lowest claim generation among blocked journals), or
 *  undefined when none is blocked. */
function oldestBlockedReason(rs: RunState): TerminalBlockedReason | undefined {
  let best: TerminalMeta | undefined;
  for (const t of rs.terminals.values()) {
    if (!t.blocked || t.blockedReason === undefined) continue;
    if (!best || t.claimGeneration < best.claimGeneration) best = t;
  }
  return best?.blockedReason;
}

// ── terminal canonicaliser (Run B / M3, D-A1 / D2) ────────────────────────────────

/** Fields the canonicaliser must NEVER touch: the run's identity, the fence, the branch/head/MR
 *  coordinates the forge needs verbatim, the completion-permit fields, and the failure class. An
 *  over-cap value here is impossible in practice (they are short scalars) and dropping one would
 *  corrupt the outcome, so they pass through unconditionally. */
const TERMINAL_NEVER_TOUCH: ReadonlySet<string> = new Set([
  "status",
  "claim_generation",
  "messages_through_seq",
  "branch",
  "head",
  "mr_iid",
  "mr_web_url",
  "permit",
  "completion_permit",
  "fail_origin",
]);

/** The visible marker appended where {@link canonicalizeTerminalBody} truncates the preserved patch.
 *  On its own line so it can never merge into a surviving diff line, and self-identifying so a human
 *  landing the patch sees exactly why it is short. */
const PATCH_TRUNCATION_MARKER = "\n…[uzi: preserved_patch truncated to fit the terminal-journal cap]…\n";

/** The JSON-serialised byte size of a value (the cap is on the on-the-wire size, not the raw string
 *  length, so quotes and escapes count). */
function serializedBytes(value: unknown): number {
  return Buffer.byteLength(JSON.stringify(value) ?? "null", "utf8");
}

/**
 * Canonicalise a run-lane TERMINAL report body so the FIRST send and the write-ahead journal are
 * byte-identical, and so the outbox size stays bounded without ever byte-cutting a credential prefix
 * past the api's scrubber (D2 / D-A1). Pure and deterministic: same input ⇒ deep-equal output ⇒
 * identical `JSON.stringify`. M3b calls it once and both sends AND journals the returned object.
 *
 * Per optional field, keyed on a per-field serialised cap of `maxBytes`:
 *   - `preserved_patch` — the only field TRUNCATED (rune-safe, at a line boundary, with a visible
 *     marker), because it is a human-landing diff, not forge-published. Line-boundary truncation is
 *     what makes a secret straddling the cap WHOLLY kept or WHOLLY cut (a token never spans a
 *     newline), never split into an unrecognisable prefix the scrubber no longer matches.
 *   - `proposal` — the forge-published optional: DROPPED WHOLE when over cap (cutting it could leave
 *     a partial credential in an issue the api files).
 *   - `milestones_completed` / `milestones_in_progress` — per-ENTRY drop of any over-cap string.
 *   - every other optional string — DROPPED WHOLE when over cap (the same "drop, not cut" rule).
 *   - {@link TERMINAL_NEVER_TOUCH} fields and every non-string/structured field — passed through
 *     untouched.
 *
 * Exported so M3b's send path runs the identical transform before journalling and before the send.
 */
export function canonicalizeTerminalBody(
  body: Record<string, unknown>,
  maxBytes: number,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(body)) {
    if (v === undefined) continue; // an absent optional stays absent (stable serialization)
    if (TERMINAL_NEVER_TOUCH.has(k)) {
      out[k] = v;
      continue;
    }
    if (k === "preserved_patch" && typeof v === "string") {
      out[k] = serializedBytes(v) > maxBytes ? truncatePatchRuneSafe(v, maxBytes) : v;
      continue;
    }
    if (k === "proposal") {
      if (serializedBytes(v) <= maxBytes) out[k] = v; // else drop whole
      continue;
    }
    if ((k === "milestones_completed" || k === "milestones_in_progress") && Array.isArray(v)) {
      out[k] = v.filter((entry) => serializedBytes(entry) <= maxBytes);
      continue;
    }
    if (typeof v === "string") {
      if (serializedBytes(v) <= maxBytes) out[k] = v; // else drop whole (never cut a credential)
      continue;
    }
    out[k] = v; // non-string scalar / structured field: untouched
  }
  return out;
}

/** Truncate a preserved patch so its JSON-serialised size fits within `maxBytes`, keeping only WHOLE
 *  lines (a rune-safe byte cut, then backed up to the last newline) plus a visible marker. Keeping
 *  whole lines is what guarantees a secret straddling the cut is wholly kept or wholly cut. */
function truncatePatchRuneSafe(patch: string, maxBytes: number): string {
  // Start with a budget below the cap for the marker + JSON quoting, then shrink until the
  // serialised candidate fits. The loop is deterministic, so two invocations agree byte-for-byte.
  let budget = maxBytes - Buffer.byteLength(PATCH_TRUNCATION_MARKER, "utf8") - 2;
  for (let guard = 0; guard < 64; guard++) {
    const prefix = budget > 0 ? lineAlignedRuneSafePrefix(patch, budget) : "";
    const candidate = prefix + PATCH_TRUNCATION_MARKER;
    if (budget <= 0 || serializedBytes(candidate) <= maxBytes) return candidate;
    budget -= 256; // ASCII diffs fit on the first pass; this covers heavy JSON escaping
  }
  return PATCH_TRUNCATION_MARKER;
}

/** The longest prefix of `s` that (a) fits `budgetBytes` UTF-8 bytes without splitting a codepoint
 *  and (b) ends on a newline (whole lines only). Returns "" when no newline falls within the budget,
 *  so content that cannot be line-aligned is dropped rather than risk splitting a secret. */
function lineAlignedRuneSafePrefix(s: string, budgetBytes: number): string {
  const buf = Buffer.from(s, "utf8");
  if (buf.length <= budgetBytes) return s;
  let end = Math.min(budgetBytes, buf.length);
  // Back up out of the middle of a multi-byte UTF-8 sequence (continuation bytes are 0b10xxxxxx).
  while (end > 0 && (buf[end]! & 0xc0) === 0x80) end--;
  // Back up to (and include) the last newline within [0, end).
  let nl = end;
  while (nl > 0 && buf[nl - 1] !== 0x0a) nl--;
  if (nl > 0) return buf.subarray(0, nl).toString("utf8");
  return "";
}
