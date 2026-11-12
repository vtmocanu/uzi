// Issue #4201: a durable, per-run, per-operation write-ahead ledger for the in-place credential
// switch apply. The apply route can commit on the server and still lose its reply, so the worker
// must remember that a send was ever attempted: a later "route absent" answer for an operation
// whose send may have committed is NOT proof that nothing happened.
//
// File: <dataDir>/credential-switch-ops/<runId>.json, `{ops: {<opId>: {generation, state, outcome?}}}`.
// Write order for every mutation: temp file -> fsync(file) -> rename -> fsync(dir).
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
  async record(runId: string, opId: string, state: Exclude<OpState, "sending">, outcome?: string): Promise<OpState> {
    return this.withLock(runId, async () => {
      const entries = await this.load(runId);
      const prior = entries.get(opId);
      if (!prior) throw new Error("credential switch ledger: no entry to record");
      if (state !== "settled" && (prior.state === "settled" || (state === "clean_route_absent" && prior.state === "ambiguous")))
        return prior.state;
      const previous = new Map([...entries].map(([k, v]) => [k, { ...v }] as const));
      entries.set(opId, { generation: prior.generation, state, ...(outcome !== undefined ? { outcome } : {}) });
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

  /** Drop entries from generations older than `generation`. */
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

  /** Remove the run's ledger file and make the removal durable. */
  async cleanup(runId: string): Promise<void> {
    const file = this.fileFor(runId);
    return this.withLock(runId, async () => {
      this.cache.delete(runId);
      await this.fs.rm(file);
      await this.fs.fsyncDir(this.dir).catch((err: unknown) => {
        if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
      });
    });
  }
}

export type LedgerApplyResult = CredentialSwitchApplyResult | { kind: "ledger_unavailable" };

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
  switch (result.kind) {
    case "uncertain":
      await ledger.record(runId, req.opId, "ambiguous", "uncertain").catch(() => undefined);
      return result;
    case "route_absent":
      if (wasAmbiguous) {
        await ledger.record(runId, req.opId, "ambiguous", "route_absent_after_ambiguous").catch(() => undefined);
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
    default:
      // A definite answer. If settling cannot be written the entry stays `sending` on disk, which
      // a later load reads as ambiguous: conservative, never wrong.
      await ledger.record(runId, req.opId, "settled", result.kind).catch(() => undefined);
      return result;
  }
}
