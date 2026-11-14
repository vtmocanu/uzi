// Issue #4201: a durable, per-run, per-operation write-ahead ledger for the in-place credential
// switch apply. The apply route can commit on the server and still lose its reply, so the worker
// must remember that a send was ever attempted: a later "route absent" answer for an operation
// whose send may have committed is NOT proof that nothing happened.
//
// File: <dataDir>/credential-switch-ops/<runId>.json, `{ops: {<opId>: {generation, state, outcome?}}}`.
// Write order for every mutation: temp file -> fsync(file) -> rename -> fsync(dir).
//
// ONE INSTANCE PER WORKER DATA DIR. The per-run lock (`chains`) and the entry cache live in the
// instance, so two instances over the same directory would interleave load+mutate+persist cycles
// and could overwrite each other's entries. Construct the ledger once and share it with every
// runner on that data dir (RunnerOptions.credentialSwitchLedger).
//
// Lifecycle contract for callers: `cleanup` only at a run's terminal state, `pruneGenerationsBelow`
// only with a NEWER claim generation, and neither during an apply. Neither ever drops an entry that
// is still `sending` or `ambiguous` for the current generation: that entry is the only memory that
// a send may have committed.
import { promises as fsp, constants as fsc } from "node:fs";
import path from "node:path";
import type { CredentialSwitchApplyRequest, CredentialSwitchApplyResult } from "./client.js";

export type OpState = "sending" | "ambiguous" | "clean_route_absent" | "settled";

export interface OpEntry {
  generation: number;
  state: OpState;
  outcome?: string;
}

/** The file operations the ledger performs, injectable so tests can observe their order. */
export interface LedgerFs {
  mkdirp(dir: string): Promise<void>;
  readFile(file: string): Promise<string | undefined>;
  /** Create/truncate `file`, write `data`, fsync the file, close it. */
  writeFileSynced(file: string, data: string): Promise<void>;
  rename(from: string, to: string): Promise<void>;
  fsyncDir(dir: string): Promise<void>;
  rm(file: string): Promise<void>;
}

export const defaultLedgerFs: LedgerFs = {
  async mkdirp(dir) { await fsp.mkdir(dir, { recursive: true, mode: 0o700 }); },
  async readFile(file) {
    try { return await fsp.readFile(file, "utf8"); }
    catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw err;
    }
  },
  async writeFileSynced(file, data) {
    const handle = await fsp.open(file, fsc.O_WRONLY | fsc.O_CREAT | fsc.O_TRUNC | fsc.O_NOFOLLOW, 0o600);
    try {
      await handle.writeFile(data, "utf8");
      await handle.sync();
    } finally { await handle.close(); }
  },
  async rename(from, to) { await fsp.rename(from, to); },
  async fsyncDir(dir) {
    const handle = await fsp.open(dir, fsc.O_RDONLY | fsc.O_DIRECTORY | fsc.O_NOFOLLOW);
    try { await handle.sync(); } finally { await handle.close(); }
  },
  async rm(file) { await fsp.rm(file, { force: true }); },
};

const SAFE_ID = /^[A-Za-z0-9._-]{1,128}$/;
const STATES: ReadonlySet<string> = new Set<OpState>(["sending", "ambiguous", "clean_route_absent", "settled"]);

function safeId(kind: string, id: string): string {
  if (!SAFE_ID.test(id) || id === "." || id === "..") throw new Error(`credential switch ledger: invalid ${kind}`);
  return id;
}

function parseFile(text: string): Map<string, OpEntry> {
  const parsed: unknown = JSON.parse(text);
  const ops = (parsed as { ops?: unknown } | null)?.ops;
  if (typeof ops !== "object" || ops === null || Array.isArray(ops)) throw new Error("credential switch ledger: malformed file");
  const out = new Map<string, OpEntry>();
  for (const [opId, raw] of Object.entries(ops)) {
    const e = raw as Partial<OpEntry> | null;
    if (!e || typeof e !== "object" || typeof e.generation !== "number" || !Number.isSafeInteger(e.generation) ||
        typeof e.state !== "string" || !STATES.has(e.state) ||
        (e.outcome !== undefined && typeof e.outcome !== "string"))
      throw new Error("credential switch ledger: malformed entry");
    out.set(opId, { generation: e.generation, state: e.state as OpState, ...(e.outcome !== undefined ? { outcome: e.outcome } : {}) });
  }
  return out;
}

/** Thrown by `markSending` when the write-ahead could not be made durable. `neverSent` is true only
 *  when the ledger was readable and positively showed no prior entry for the op. */
class WriteAheadError extends Error {
  constructor(readonly neverSent: boolean, cause: unknown) {
    super("credential switch ledger: write-ahead failed", { cause });
  }
}

export class CredentialSwitchOpLedger {
  private readonly dir: string;
  private readonly cache = new Map<string, Map<string, OpEntry>>();
  private readonly chains = new Map<string, Promise<void>>();
  private dirSynced = false;
  private tmpSeq = 0;

  constructor(private readonly dataDir: string, private readonly fs: LedgerFs = defaultLedgerFs) {
    this.dir = path.join(dataDir, "credential-switch-ops");
  }

  private fileFor(runId: string): string { return path.join(this.dir, `${safeId("run id", runId)}.json`); }

  /** Serialize every load+mutate+persist of one run: a promise chain per run id. */
  private withLock<T>(runId: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.chains.get(runId) ?? Promise.resolve();
    const result = prev.then(fn);
    const tail = result.then(() => undefined, () => undefined);
    this.chains.set(runId, tail);
    void tail.then(() => { if (this.chains.get(runId) === tail) this.chains.delete(runId); });
    return result;
  }

  /** Load a run's entries (cached). Any `sending` entry is a send whose outcome was never recorded:
   *  it becomes `ambiguous` in memory here and is made durable by the next write (see `markSending`). */
  private async load(runId: string): Promise<Map<string, OpEntry>> {
    const cached = this.cache.get(runId);
    if (cached) return cached;
    const text = await this.fs.readFile(this.fileFor(runId));
    const entries = text === undefined ? new Map<string, OpEntry>() : parseFile(text);
    for (const e of entries.values()) if (e.state === "sending") e.state = "ambiguous";
    this.cache.set(runId, entries);
    return entries;
  }

  private async persist(runId: string, entries: Map<string, OpEntry>): Promise<void> {
    const file = this.fileFor(runId);
    if (!this.dirSynced) {
      await this.fs.mkdirp(this.dir);
      await this.fs.fsyncDir(this.dir);
      await this.fs.fsyncDir(this.dataDir);
      this.dirSynced = true;
    }
    const tmp = `${file}.tmp-${process.pid}-${this.tmpSeq++}`;
    const body = JSON.stringify({ ops: Object.fromEntries(entries) });
    try {
      await this.fs.writeFileSynced(tmp, body);
      await this.fs.rename(tmp, file);
    } catch (err) {
      await this.fs.rm(tmp).catch(() => undefined);
      throw err;
    }
    await this.fs.fsyncDir(this.dir);
  }

  /**
   * Write-ahead, durable BEFORE the send. An entry already `ambiguous` (including one found
   * `sending` at load) stays ambiguous; anything else becomes `sending`. Throws when the write
   * cannot be made durable: the caller must not send. Returns whether the op was already ambiguous.
   */
  async markSending(runId: string, opId: string, generation: number): Promise<{ wasAmbiguous: boolean }> {
    safeId("op id", opId);
    this.fileFor(runId); // validates the run id before anything is locked or read
    return this.withLock(runId, async () => {
      let entries: Map<string, OpEntry>;
      try { entries = await this.load(runId); }
      catch (err) { throw new WriteAheadError(false, err); }
      const prior = entries.get(opId);
      // Only "no entry" and a proven clean route_absent are clean. `sending` means an earlier send
      // never recorded its outcome; `settled` means the op got a definite answer once already.
      const wasAmbiguous = prior !== undefined && prior.state !== "clean_route_absent";
      const previous = new Map([...entries].map(([k, v]) => [k, { ...v }] as const));
      if (prior?.state === "settled") entries.set(opId, { ...prior, generation });
      else entries.set(opId, wasAmbiguous ? { ...prior, generation, state: "ambiguous" } : { generation, state: "sending" });
      try {
        await this.persist(runId, entries);
      } catch (err) {
        this.cache.set(runId, previous);
        throw new WriteAheadError(prior === undefined, err);
      }
      return { wasAmbiguous };
    });
  }

  /**
   * Record the post-response state and return the state actually stored. Monotonic under the run
   * lock: a `settled` entry is never degraded, and an `ambiguous` one never becomes
   * `clean_route_absent`. Throws when the change cannot be made durable.
   */
  async record(
    runId: string, opId: string, state: Exclude<OpState, "sending">, outcome?: string, generation?: number,
  ): Promise<OpState> {
    return this.withLock(runId, async () => {
      const entries = await this.load(runId);
      const prior = entries.get(opId);
      // An `ambiguous` record re-creates a missing entry (a prune or cleanup raced the send, or the
      // file was lost): losing the memory of a possibly committed send must never make it clean.
      if (!prior && !(state === "ambiguous" && generation !== undefined))
        throw new Error("credential switch ledger: no entry to record");
      if (prior && state !== "settled" && (prior.state === "settled" || (state === "clean_route_absent" && prior.state === "ambiguous")))
        return prior.state;
      const previous = new Map([...entries].map(([k, v]) => [k, { ...v }] as const));
      entries.set(opId, { generation: prior?.generation ?? generation!, state, ...(outcome !== undefined ? { outcome } : {}) });
      try {
        await this.persist(runId, entries);
      } catch (err) {
        this.cache.set(runId, previous);
        throw err;
      }
      return state;
    });
  }

  /** The current in-memory/disk state of an op, for diagnostics and tests. */
  async stateOf(runId: string, opId: string): Promise<OpEntry | undefined> {
    return this.withLock(runId, async () => {
      const e = (await this.load(runId)).get(opId);
      return e ? { ...e } : undefined;
    });
  }

  /** Drop entries from generations older than `generation`. Never touches the current generation. */
  async pruneGenerationsBelow(runId: string, generation: number): Promise<void> {
    return this.withLock(runId, async () => {
      const entries = await this.load(runId);
      const previous = new Map([...entries].map(([k, v]) => [k, { ...v }] as const));
      let changed = false;
      for (const [opId, e] of entries) if (e.generation < generation) { entries.delete(opId); changed = true; }
      if (!changed) return;
      try {
        await this.persist(runId, entries);
      } catch (err) {
        this.cache.set(runId, previous);
        throw err;
      }
    });
  }

  /**
   * Terminal-state cleanup: drop every entry except those still `sending` or `ambiguous` at
   * `currentGeneration` or later (all of them when it is omitted), and remove the file once nothing
   * is left. The kept entries are the memory that a send may have committed.
   */
  async cleanup(runId: string, currentGeneration?: number): Promise<void> {
    const file = this.fileFor(runId);
    return this.withLock(runId, async () => {
      // A file that cannot be read or parsed is left alone: it may hold an ambiguous entry.
      const entries = await this.load(runId);
      const keep = new Map<string, OpEntry>();
      for (const [opId, e] of entries) {
        if ((e.state === "sending" || e.state === "ambiguous") && (currentGeneration === undefined || e.generation >= currentGeneration))
          keep.set(opId, e);
      }
      if (keep.size > 0) {
        const previous = new Map([...entries].map(([k, v]) => [k, { ...v }] as const));
        try {
          await this.persist(runId, keep);
          this.cache.set(runId, keep);
        } catch (err) {
          this.cache.set(runId, previous);
          throw err;
        }
        return;
      }
      this.cache.delete(runId);
      await this.fs.rm(file);
      await this.fs.fsyncDir(this.dir).catch((err: unknown) => {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
      });
    });
  }
}

/**
 * `priorAmbiguous` rides on a `not_applied`, `superseded` or `fenced` answer when the ledger already held any
 * entry for the op other than `clean_route_absent` before this send (`ambiguous`, a crash-left `sending`, or
 * `settled`, matching `wasAmbiguous` in the write-ahead step): an earlier send of it may have
 * committed, and the caller (the runner, whose own per-call memory starts empty after a restart) must carry that
 * memory unless the answer is itself proof of a non-commit (`superseded`, or a NOT_APPLIED_PROOF_REASONS reason).
 */
export type LedgerApplyResult = (CredentialSwitchApplyResult & { priorAmbiguous?: true }) | { kind: "ledger_unavailable" };

/**
 * The `not_applied` reasons that prove the op can NEVER commit (api credential_switch_apply.go): answered after
 * the server's replay check, for an op that can never become applicable again. `no_pending_switch`: the op is
 * not the pending one, and every new stamp mints a fresh op id (migration 00442 trigger). `generation_mismatch`:
 * permanent because the stamp generation is fixed per op (the only writer that sets the op id sets the stamp
 * generation with it; the other stamp writer only keeps or nulls the stamp), and the step-0 fence plus the
 * increment-only runs.claim_generation rule out a delayed send landing at another generation. Every other
 * not_applied reason leaves the op pending.
 */
export const NOT_APPLIED_PROOF_REASONS: ReadonlySet<string> = new Set(["no_pending_switch", "generation_mismatch"]);

/**
 * Write-ahead -> send -> record -> final classification. When the write-ahead fails NOTHING is sent,
 * and the result is:
 *  - `ledger_unavailable` ONLY when the ledger positively shows the op was never sent before (file
 *    readable, no prior entry for the op) or the run/op id is invalid so nothing can be on disk:
 *    the runner may treat that as proven not-applied.
 *  - `uncertain` when a prior entry exists (ambiguous, sending or settled) or the file cannot be
 *    read or parsed: a possibly-committed op cannot be told from a never-sent one.
 * After a send a ledger failure never turns a definite answer into a guess, except that a
 * `route_absent` that is not recorded as clean (a prior or concurrent ambiguous or settled state,
 * or a failed write) is reported `uncertain`.
 */

export async function applyThroughLedger(
  client: { applyCredentialSwitch(runId: string, req: CredentialSwitchApplyRequest): Promise<CredentialSwitchApplyResult> },
  ledger: CredentialSwitchOpLedger,
  runId: string,
  req: CredentialSwitchApplyRequest,
): Promise<LedgerApplyResult> {
  let wasAmbiguous: boolean;
  try {
    ({ wasAmbiguous } = await ledger.markSending(runId, req.opId, req.claimGeneration));
  } catch (err) {
    if (err instanceof WriteAheadError && !err.neverSent) return { kind: "uncertain", detail: "ledger_unavailable_prior_state_unknown" };
    return { kind: "ledger_unavailable" };
  }
  const result = await client.applyCredentialSwitch(runId, req);
  const flagged = (): LedgerApplyResult => (wasAmbiguous ? { ...result, priorAmbiguous: true } : result);
  switch (result.kind) {
    case "uncertain":
      await ledger.record(runId, req.opId, "ambiguous", "uncertain", req.claimGeneration).catch(() => undefined);
      return result;
    case "route_absent":
      if (wasAmbiguous) {
        await ledger.record(runId, req.opId, "ambiguous", "route_absent_after_ambiguous", req.claimGeneration).catch(() => undefined);
        return { kind: "uncertain", detail: "route_absent_after_ambiguous_send" };
      }
      try {
        // The stored state may differ from the request: another send of this op may have turned
        // it ambiguous or settled while this one was in flight.
        const stored = await ledger.record(runId, req.opId, "clean_route_absent", "route_absent");
        if (stored !== "clean_route_absent") return { kind: "uncertain", detail: "route_absent_after_ambiguous_send" };
      } catch {
        return { kind: "uncertain", detail: "ledger_record_failed" };
      }
      return result;
    case "fenced":
      // `fenced` does not undo an earlier ambiguous send of the same op: the server checks the fence
      // BEFORE it replays a committed op, so that send may have committed. Keep the memory.
      if (wasAmbiguous) await ledger.record(runId, req.opId, "ambiguous", "fenced_after_ambiguous", req.claimGeneration).catch(() => undefined);
      else await ledger.record(runId, req.opId, "settled", result.kind).catch(() => undefined);
      return flagged();
    case "superseded":
      // Answered AFTER the server's replay check, in the same transaction under the run lock: a committed
      // op would have been answered applied or refused_committed. So it proves the op never committed.
      await ledger.record(runId, req.opId, "settled", result.kind).catch(() => undefined);
      return flagged();
    case "not_applied":
      // Only a reason in NOT_APPLIED_PROOF_REASONS is permanent proof that nothing committed. Every other reason
      // (credential_busy, vault_locked, credential_unavailable, ...) leaves the op pending, so a delayed earlier
      // send could still commit: after an ambiguous send keep the memory. A never-ambiguous op settles.
      if (wasAmbiguous && !NOT_APPLIED_PROOF_REASONS.has(result.reason))
        await ledger.record(runId, req.opId, "ambiguous", "not_applied_after_ambiguous", req.claimGeneration).catch(() => undefined);
      else await ledger.record(runId, req.opId, "settled", result.kind).catch(() => undefined);
      return flagged();
    default:
      // A definite answer. If settling cannot be written the entry stays `sending` on disk, which
      // a later load reads as ambiguous: conservative, never wrong.
      await ledger.record(runId, req.opId, "settled", result.kind).catch(() => undefined);
      return result;
  }
}
