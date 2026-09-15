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

/** The `.reserve` file is preallocated to this many bytes — sized for ONE range
 *  record (a few hundred bytes) with generous slack for the JSON + HMAC + the
 *  manifest rewrite it rides with. On `ENOSPC` during a range-record write the
 *  single-flight writer releases it to free the space, writes the range record,
 *  then replenishes it when space returns. Run A's reserve is sized for a range
 *  record only; the terminal-journal reserve is Run B (#1393), out of scope. */
export const OUTBOX_RANGE_RESERVE_BYTES = 64 * 1024;

// Domain-separation labels. Binding a distinct label into each kind's MAC keeps a
// segment MAC unusable as a manifest MAC and vice-versa, so a record cannot be
// replayed as a different kind even under the same key.
const MAC_DOMAIN_SEGMENT = "uzi.outbox.segment.v1";
const MAC_DOMAIN_RANGE = "uzi.outbox.range.v1";
const MAC_DOMAIN_MANIFEST = "uzi.outbox.manifest.v1";

const MANIFEST_FILE = "manifest.json";
const KEY_FILE = ".key";
const RESERVE_FILE = ".reserve";

/** The fixed reason a quota / ENOSPC drop records; replay expands the range into
 *  one status tombstone per seq carrying this reason (batcher.ts:152-180). */
const DROP_REASON = "outbox quota";

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

/** In-memory working state for one run: its authenticated manifest plus a byte
 *  map (file -> size) for quota accounting, so the quota check never re-stats. */
interface RunState {
  runId: string;
  manifest: ManifestData;
  recordBytes: Map<string, number>;
}

/** The per-run outbox depth surfaced on the heartbeat (M5) and in `uzi admin
 *  workers`. `pendingTerminal` is ALWAYS 0 in Run A (terminal journaling is Run
 *  B); `blockedReason` is likewise never set here. */
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
  ctx: { path: string; kind: RecordFileKind | "manifest" },
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
    await fs.mkdir(this.root, { recursive: true, mode: 0o700 });
    if (await this.isSymlink(this.root)) {
      this.log.error("outbox: root is a symlink; disabling (following it would escape /data)", {
        path: this.root,
      });
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

    // Load every run's manifest (authenticated) and stat its files for accounting.
    for (const runId of runDirs) {
      await this.loadRun(runId);
    }

    // A best-effort preallocated reserve so a range record can still be written on
    // a full volume (released on ENOSPC, then replenished).
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
    if (!manifest) {
      this.log.warn("outbox: malformed manifest; skipping run", { run_id: runId });
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
    this.runs.set(runId, { runId, manifest, recordBytes });
    if (manifest.spilledUnclean) this.uncleanAtInit.push(runId);
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
        const { messages, generation } = await this.expandRecord(rs, rec);
        try {
          // Replay each record under the generation it was PRODUCED under (needed for
          // #1247's fence once the api advertises it, D11), so a re-claim's generation
          // bump never rebinds an old attempt's frames.
          await send(messages, generation);
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

  /** The messages a record replays as (plus the generation it was produced under):
   *  a segment's own messages, or one tombstone per seq for a range record / a
   *  segment the valid manifest proves is missing. A missing/tampered segment or a
   *  MAC-bad range record has no readable generation, so it replays under 0. */
  private async expandRecord(
    rs: RunState,
    rec: ManifestRecordRef,
  ): Promise<{ messages: OutgoingMessage[]; generation: number }> {
    if (rec.kind === "segment") {
      const seg = await this.readSegmentFile(rs.runId, rec);
      if (seg) return { messages: seg.messages, generation: seg.generation };
      this.log.warn("outbox: segment missing/tampered; emitting per-seq gap tombstones (manifest proves the range)", {
        run_id: rs.runId,
        first_seq: rec.firstSeq,
        last_seq: rec.lastSeq,
      });
      return { messages: gapTombstones(rec, "outbox segment unrecoverable"), generation: 0 };
    }
    // A range record IS a dropped range; the manifest ref alone proves it, so we
    // expand from the ref whether or not the file authenticates. Read the file only
    // for its recorded generation (0 when it does not authenticate).
    const parsed = await this.readAuthed(path.join(this.runDir(rs.runId), rec.file), MAC_DOMAIN_RANGE);
    const generation = parsed && typeof parsed.generation === "number" ? parsed.generation : 0;
    return { messages: gapTombstones(rec, DROP_REASON), generation };
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

  /** Age-delete only runs whose records are all retired (empty) and whose last
   *  write is older than `retentionMs`. A run with undrained records is never
   *  removed here. */
  async sweepRetention(nowArg?: number): Promise<void> {
    if (this.disabled) return;
    const now = nowArg ?? this.now();
    // Collect first, then remove — never mutate the map mid-iteration.
    const toRemove: string[] = [];
    for (const [runId, rs] of this.runs) {
      if (rs.manifest.records.length === 0 && now - rs.manifest.updatedAt > this.retentionMs) toRemove.push(runId);
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
    return {
      runId,
      pendingMessages,
      pendingTerminal: 0, // always 0 in Run A
      staleRetired: m.staleRetired,
      since: m.since,
    };
  }

  /** Runs whose spill-unclean flag was set as observed AT INIT (for M2's restart
   *  log). A same-session markSpillUnclean does not retroactively join this set. */
  uncleanRuns(): string[] {
    return [...this.uncleanAtInit];
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
    const rs: RunState = { runId, manifest, recordBytes: new Map() };
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
    if (parsed.version !== 1 || typeof parsed.generation !== "number" || !Array.isArray(parsed.messages)) {
      return null;
    }
    return { generation: parsed.generation, messages: parsed.messages as OutgoingMessage[] };
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

  /** Preallocate the reserve file if absent (best-effort). */
  private async ensureReserve(): Promise<void> {
    try {
      if (await pathExists(this.reservePath())) return;
      await this.replenishReserve();
    } catch (err) {
      this.log.warn("outbox: could not preallocate reserve (range records may fail on a full volume)", {
        error: errText(err),
      });
    }
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
        await fh.writeFile(Buffer.alloc(OUTBOX_RANGE_RESERVE_BYTES));
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
    kind: RecordFileKind | "manifest",
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

/** One status tombstone per seq in a record's range — the shape the browser
 *  renders (batcher.ts:152-180) and that keeps the stream contiguous (D2). */
function gapTombstones(rec: Pick<ManifestRecordRef, "firstSeq" | "lastSeq">, reason: string): OutgoingMessage[] {
  const out: OutgoingMessage[] = [];
  for (let seq = rec.firstSeq; seq <= rec.lastSeq; seq++) {
    out.push({
      seq,
      kind: "status",
      payload: { text: `message dropped: ${reason} (seq ${seq})`, event: "message_dropped", reason },
    });
  }
  return out;
}
