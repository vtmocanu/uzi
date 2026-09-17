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

import { createHmac, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import fs from "node:fs/promises";
import { constants as fsConstants } from "node:fs";
import path from "node:path";

import type { Logger } from "./log.js";
import type { OutgoingMessage } from "./protocol.js";

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

const MANIFEST_FILE = "manifest.json";
const KEY_FILE = ".key";
const RESERVE_FILE = ".reserve";
/** PRD #1391 M3 (Run B): the per-run terminal-journal filename, keyed by claim generation
 *  (D4). It is NOT listed in the message manifest — the journal for a generation is its own
 *  first-writer-wins file, `terminal-<claim_generation>.json`. */
const TERMINAL_FILE_PREFIX = "terminal-";
const TERMINAL_FILE_SUFFIX = ".json";

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
 *  timer). Exported so M3b's send path names exactly these three. */
export type TerminalBlockedReason = "completion_permit_mismatch" | "gap_unrecoverable" | "reserve_exhausted";

/** PRD #1391 M3 (Run B): the outcome of {@link Outbox.journalTerminal}. A discriminated union so a
 *  caller (M3b's send path) never confuses "installed durably" with "sent unjournaled": on success
 *  the outcome is journalled (and `adopted` says whether THIS call installed it or adopted an
 *  earlier first-writer for the same generation, D4); on `reserve_exhausted` the terminal-sized
 *  reserve could not admit it, so the caller sends unjournaled + logs, never silently dropping. */
export type TerminalJournalResult =
  | { journaled: true; adopted: boolean }
  | { journaled: false; reason: "reserve_exhausted" };

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
  staleRetired: number;
  blockedReason?: string;
  since: number;
}

/** A test/production seam around the one raw write that can hit `ENOSPC` (the
 *  temp-file write of a record). The default just calls `write`; a test injects a
 *  seam that throws `ENOSPC` to exercise the reserve path. */
export type RawWriteSeam = (
  write: () => Promise<void>,
  ctx: { path: string; kind: RecordFileKind | "manifest" | "terminal" },
) => Promise<void>;

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
  now?: () => number;
  /** Optional raw-write seam to simulate `ENOSPC` (tests). */
  rawWrite?: RawWriteSeam;
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
  private readonly now: () => number;
  private readonly rawWrite: RawWriteSeam;

  private readonly runs = new Map<string, RunState>();
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
    this.root = opts.root;
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

    if (!(await this.loadOrMintKey(hasRecords))) return; // disabled + logged inside

    // Load every run's manifest (authenticated) and stat its files for accounting,
    // then adopt any pending terminal journals (Run B / M3) — a run can carry a
    // terminal journal with no message manifest, so this loads them independently.
    for (const runId of runDirs) {
      await this.loadRun(runId);
      await this.loadTerminals(runId);
    }

    // A best-effort preallocated reserve so a range record — and, with a terminal-sized
    // reserve, the next few terminal journals — can still be written on a full volume
    // (released on ENOSPC, then replenished). GROWS an undersized existing `.reserve`.
    await this.ensureReserve();
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
    this.runs.set(runId, { runId, manifest, recordBytes, terminals: new Map() });
    if (manifest.spilledUnclean) this.uncleanAtInit.push(runId);
  }

  /** Adopt a run's pending terminal journals (`terminal-<generation>.json`) at init (D4). A
   *  journal is authenticated and bound to its directory (embedded `run_id` must match) and its
   *  filename generation (the file names the generation, so a copied journal cannot masquerade
   *  as another generation). A run may carry a terminal journal with NO message manifest, so this
   *  creates the in-memory run entry when loadRun did not. */
  private async loadTerminals(runId: string): Promise<void> {
    if (!this.validRunId(runId)) return;
    const dir = this.runDir(runId);
    let names: string[];
    try {
      names = await fs.readdir(dir);
    } catch {
      return;
    }
    for (const name of names) {
      const gen = parseTerminalFileName(name);
      if (gen === undefined) continue;
      const parsed = await this.readAuthed(path.join(dir, name), MAC_DOMAIN_TERMINAL);
      if (!parsed) continue; // absent/symlink/unparseable/MAC-bad (readAuthed logged)
      const meta = coerceTerminal(parsed, runId, gen);
      if (!meta) {
        this.log.warn("outbox: malformed or misfiled terminal journal; skipping", { run_id: runId, file: name });
        continue;
      }
      const rs = this.ensureInMemoryRun(runId, meta.since);
      rs.terminals.set(meta.claimGeneration, meta);
    }
  }

  // ── writes ──────────────────────────────────────────────────────────────────

  /**
   * Append one flush as a single immutable segment. Enforces the quota FIRST
   * (evicting the oldest segment to a range record when the run or worker quota
   * binds), then writes the segment and installs the next manifest generation.
   */
  async appendSegment(runId: string, generation: number, msgs: OutgoingMessage[]): Promise<void> {
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
    });
  }

  /**
   * Append one compact range record covering `firstSeq..lastSeq` (the M2
   * spill-buffer-cap drop path). Uses the reserve on `ENOSPC`: releases it, writes
   * the record, then replenishes it. Replay expands the record into one tombstone
   * per seq so the stream stays contiguous.
   */
  async appendRangeRecord(runId: string, generation: number, firstSeq: number, lastSeq: number): Promise<void> {
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
      });
    });
  }

  /** Set the spill-unclean flag when a spill begins (creates the run's manifest if
   *  none exists yet). Surfaced at the next restart via {@link uncleanRuns}. */
  async markSpillUnclean(runId: string): Promise<void> {
    if (!this.writable()) return;
    await this.withRunLock(runId, async () => {
      const rs = await this.ensureRunState(runId);
      if (!rs) return;
      if (rs.manifest.spilledUnclean && rs.manifest.generation > 0) return;
      await this.installManifest(rs, rs.manifest.records, { spilledUnclean: true });
    });
  }

  /** Clear the spill-unclean flag on a clean flush/close. */
  async clearSpillUnclean(runId: string): Promise<void> {
    if (!this.writable()) return;
    await this.withRunLock(runId, async () => {
      const rs = this.runs.get(runId);
      if (!rs || !rs.manifest.spilledUnclean) return;
      await this.installManifest(rs, rs.manifest.records, { spilledUnclean: false });
    });
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
   * cumulative stale-retired seq count.
   */
  async drainRun(
    runId: string,
    send: (msgs: OutgoingMessage[], generation: number) => Promise<void>,
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
    });
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
    if (now - rs.manifest.updatedAt <= this.retentionMs) return false; // no longer past retention
    await this.removeRun(runId);
    return true;
  }

  private async removeRun(runId: string): Promise<void> {
    // Guard the recursive delete: a non-bare runId (`..`, a nested path, a
    // separator/NUL) would escape the root, so refuse it here — the last line of
    // defense before `fs.rm(..., { recursive: true })` (H1).
    if (!this.validRunId(runId)) return;
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
    if (!this.writable()) return { journaled: false, reason: "reserve_exhausted" };
    return this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return { journaled: false, reason: "reserve_exhausted" };
      const dir = this.runDir(runId);
      if (await this.isSymlink(dir)) {
        this.log.warn("outbox: run dir is a symlink; refusing terminal journal (skipped, not followed)", {
          run_id: runId,
        });
        return { journaled: false, reason: "reserve_exhausted" };
      }
      await fs.mkdir(dir, { recursive: true, mode: 0o700 });
      await this.fsyncDir(this.root);
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
      const dst = path.join(dir, terminalFileName(claimGeneration));
      let adopted: boolean;
      try {
        adopted = await this.installTerminalExclusive(dst, serialized);
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
      // Track it in memory. On an adopt (a first-writer already installed this generation) keep
      // the winner's metadata if we already loaded it; otherwise record ours (same generation).
      const rs = this.ensureInMemoryRun(runId, since);
      if (!rs.terminals.has(claimGeneration)) {
        rs.terminals.set(claimGeneration, {
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
          ...(t.blockedReason !== undefined ? { blocked_reason: t.blockedReason } : {}),
          since: t.since,
        });
      }
    }
    return out;
  }

  /** Retire a terminal journal (the api applied the transition on a 200, or answered a 409 whose
   *  returned status is terminal): unlink the file and drop it from the pending set (D3). */
  async retireTerminal(runId: string, claimGeneration: number): Promise<void> {
    if (this.disabled) return;
    await this.withRunLock(runId, async () => {
      if (!this.validRunId(runId)) return;
      await fs
        .rm(path.join(this.runDir(runId), terminalFileName(claimGeneration)), { force: true })
        .catch(() => undefined);
      this.runs.get(runId)?.terminals.delete(claimGeneration);
    });
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
      await fs
        .rm(path.join(this.runDir(runId), terminalFileName(claimGeneration)), { force: true })
        .catch(() => undefined);
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
      const dst = path.join(this.runDir(runId), terminalFileName(claimGeneration));
      const parsed = await this.readAuthed(dst, MAC_DOMAIN_TERMINAL);
      if (parsed) {
        const next = { ...parsed, blocked: true, blocked_reason: reason };
        // Re-seal + rewrite atomically (rename-based): the exclusive first-writer guarantee is only
        // for the INITIAL install; updating blocked-state is the owner's own follow-up write.
        await this.writeFileAtomic(dst, this.seal(MAC_DOMAIN_TERMINAL, next), "terminal");
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

  /** Install a terminal journal crash-atomically and EXCLUSIVELY (D4): temp write (through the
   *  ENOSPC-reserve seam so a full volume uses the reserve) → fsync → no-replace `link()` (EEXIST
   *  means a first writer already won this generation) → dir fsync → remove temp. Returns whether an
   *  existing winner was adopted rather than freshly installed. */
  private async installTerminalExclusive(dst: string, serialized: string): Promise<boolean> {
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
      await this.withReserveOnEnospc(() => this.rawWrite(doWrite, { path: tmp, kind: "terminal" }));
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
    await this.fsyncDir(path.dirname(dst));
    await fs.rm(tmp, { force: true }).catch(() => undefined);
    return adopted;
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
    const rs: RunState = { runId, manifest, recordBytes: new Map(), terminals: new Map() };
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
    if (
      runId === "" ||
      runId === "." ||
      runId === ".." ||
      runId.includes("/") ||
      runId.includes("\\") ||
      runId.includes("\0") ||
      path.basename(runId) !== runId
    ) {
      this.log.warn("outbox: refusing non-bare runId (path escape guard)", { run_id: runId });
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
  private withRunLock<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.runLocks.get(runId) ?? Promise.resolve();
    const result = prev.then(() => fn());
    const tail = result.then(
      () => undefined,
      () => undefined,
    );
    this.runLocks.set(runId, tail);
    // Drop the entry once this op is the last in the chain (bounded map growth).
    void tail.then(() => {
      if (this.runLocks.get(runId) === tail) this.runLocks.delete(runId);
    });
    return result;
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
    const rs: RunState = { runId, manifest, recordBytes: new Map(), terminals: new Map() };
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

  /** Read + authenticate a record file. Returns the parsed body (minus `mac`) when
   *  the MAC verifies, else null (a missing file, a symlink refused by O_NOFOLLOW,
   *  an unparseable body, or a MAC mismatch — the last two are logged). */
  private async readAuthed(filePath: string, domain: string): Promise<Record<string, unknown> | null> {
    if (!this.key) return null;
    let raw: Buffer;
    try {
      const fh = await fs.open(filePath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
      try {
        raw = await fh.readFile();
      } finally {
        await fh.close();
      }
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
      if (size >= this.reserveBytes) return; // already at/above the configured size
      await this.growReserveAtomic();
    } catch (err) {
      this.log.warn("outbox: could not (pre)allocate reserve (range/terminal writes may fail on a full volume)", {
        error: errText(err),
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
        await fh.writeFile(Buffer.alloc(this.reserveBytes));
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

  private async releaseReserve(): Promise<void> {
    await fs.rm(this.reservePath(), { force: true }).catch(() => undefined);
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
        await fh.writeFile(Buffer.alloc(this.reserveBytes));
      } finally {
        await fh.close();
      }
    } catch (err) {
      this.log.warn("outbox: could not replenish reserve", { error: errText(err) });
    }
  }

  /** Run `fn`, and on `ENOSPC` release the reserve to free space, retry once, then
   *  replenish the reserve. Used for range-record writes (D2). */
  private async withReserveOnEnospc<T>(fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (err) {
      if (!isENOSPC(err)) throw err;
      this.log.warn("outbox: ENOSPC on a range write; releasing reserve to admit it", {});
      await this.releaseReserve();
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
    kind: RecordFileKind | "manifest" | "terminal",
  ): Promise<void> {
    const dir = path.dirname(dstPath);
    const tmp = `${dstPath}.${randomUUID()}.tmp`;
    const doWrite = async () => {
      const fh = await fs.open(
        tmp,
        fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_NOFOLLOW,
        0o600,
      );
      try {
        await fh.writeFile(data, "utf8");
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
    await fs.rename(tmp, dstPath);
    await this.fsyncDir(dir);
  }

  private async fsyncDir(dir: string): Promise<void> {
    let dh: Awaited<ReturnType<typeof fs.open>> | undefined;
    try {
      dh = await fs.open(dir, fsConstants.O_RDONLY);
      await dh.sync();
    } catch (err) {
      // Some filesystems reject a directory fsync; the rename is already durable
      // for the file bytes, so degrade rather than fail the write.
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
  return Number.isInteger(gen) && gen >= 0 ? gen : undefined;
}

/** The set of legal blocked reasons, mirroring {@link TerminalBlockedReason}. */
const TERMINAL_BLOCKED_REASONS: ReadonlySet<string> = new Set<TerminalBlockedReason>([
  "completion_permit_mismatch",
  "gap_unrecoverable",
  "reserve_exhausted",
]);

/** Validate an authenticated terminal-journal object into in-memory metadata (shape only; the MAC
 *  already gated integrity in readAuthed). Binds the embedded `run_id` to the directory and the
 *  embedded `claim_generation` to the filename, so a copied/misfiled journal is skipped. Returns
 *  null on any mismatch. */
function coerceTerminal(obj: Record<string, unknown>, runId: string, fileGen: number): TerminalMeta | null {
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
    claimGeneration: obj.claim_generation,
    phaseAtJournal: obj.phase_at_journal,
    since: obj.since,
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
