import { AsyncLocalStorage } from "node:async_hooks";
import { createHash } from "node:crypto";
import * as fsp from "node:fs/promises";
import path from "node:path";
import {
  AuthorityRefusal, activeAuthorityTransaction, withAuthorityTransaction,
  type AuthorityTransaction, type WorkerLifetime,
} from "./authority-lock.js";
import {
  leaseEpoch, leaseHarness, openAuthorityStore, peekAuthoritySlot,
  type AuthorityFsOps, type AuthorityStore, type SlotSnapshot, type SourceUseLease,
} from "./completion-authority.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "./git-authority-hooks.js";

// issue #3404 M2 U3 — the process runtime that turns the completion-authority store into the
// GitAuthorityHooks GitCache calls, and the single place a production entry point asks for A.
//
// Lock order is P -> W -> A -> C -> S -> J -> B -> O. The hooks run INSIDE the bare lock B (and
// usually inside C, S and J), so they never acquire A: they require the transaction the outermost
// entry point opened in this async context and refuse with AuthorityRefusal when there is none.
// A settlement that runs with no active transaction (a child's late close, or GitCache's abandoned
// cleanupArchivedRecoveryPin continuation, which may still hold B) never acquires A either: it is queued
// in-process and applied at the start of the next outermost A acquisition (see {@link AuthorityRuntime.withTransaction}).
//
// assertBareClear is the one hook that needs no A: it reads a durable record that is written either by an
// owner that is necessarily dead (W excludes another compatible worker) or, in this process, under the same
// bare lock B the reader holds, and it uses a read-only load that never writes (peekAuthoritySlot).
//
// Ending a source use. A slot a flight's source use left `active` blocks every later worker incarnation (a
// successor cannot advance an active foreign epoch). {@link AuthorityRuntime.endSourceUse} is the live
// owner's one step that moves the slots it admitted for a flight to `ineligible` once the flight's source use
// ended and its worker-owned source-capable tails were positively joined. It is the seam where U4 will
// publish sealed/claude_ready instead when that is proven. A crash never reaches it, so the slot stays active.

/** How long, and until what signal, an outermost entry may wait for A. */
export interface AuthorityWait { signal?: AbortSignal; timeoutMs?: number }

/** What an entry point needs to open (or borrow) A. Tests substitute an instrumented one. */
export interface AuthorityScope {
  withTransaction<T>(
    fn: (tx: AuthorityTransaction) => Promise<T>,
    options?: AuthorityWait,
  ): Promise<T>;
}

export interface AuthorityRuntimeOptions {
  lifetime: WorkerLifetime;
  /** The worker join token; the store derives its MAC key from it. */
  workerToken: string;
  /** Tests only: simulate another worker process. */
  workerIncarnation?: string;
  /** Tests only: inject fsync/rename failures into the store. */
  ops?: AuthorityFsOps;
  /** How long an outermost entry waits for A. A holder can be a recovery pass, so this is
   *  generous; an abort signal still ends the wait. */
  timeoutMs?: number;
  /** Diagnostics only. */
  log?: { warn(message: string, fields?: Record<string, unknown>): void };
}

/** Bounded wait for A at an outermost entry: a recovery pass or a discard may hold it for a while. */
const DEFAULT_WAIT_MS = 600_000;
/** The idle drain of queued late settlements waits at most this long for A (it holds no lower lock). */
const LATE_WAIT_MS = 20_000;
const SKILLS_PREFIX = ".uzi-skills-";
/** The ops that create the path they admit: GitCache creates its parent directory right after admission. */
const SEED_OPS: ReadonlySet<string> = new Set(["seed-canonical", "seed-attempt", "replace"]);
const MAX_FOOTPRINT = 1024;
const MAX_ITEM = 512;
const MAX_RESIDUE = 64;

function digestOf(text: string): string {
  return `sha256:${createHash("sha256").update(text).digest("hex")}`;
}

/** Fit a footprint into the store's bound without losing identity: an oversized one is replaced by its digest. */
function boundedText(text: string, max: number): string {
  return text.length <= max ? text : digestOf(text);
}

/** `.uzi-skills-<name>` is the paired skills dir of the clone `<name>` in the same parent. */
function sourceSlotPath(target: string): string {
  const resolved = path.resolve(target);
  const base = path.basename(resolved);
  return base.startsWith(SKILLS_PREFIX) && base.length > SKILLS_PREFIX.length
    ? path.join(path.dirname(resolved), base.slice(SKILLS_PREFIX.length))
    : resolved;
}

interface PendingPart {
  readonly lease: SourceUseLease;
  readonly operation: string;
  /** Exactly what the pending record covers (refs, or the targets of one source slot). */
  readonly covers: readonly string[];
  /** Bare-mutator slots leave `active` once their mutation is settled ok, so a successor is not wedged. */
  readonly bareSlot: boolean;
  settled: boolean;
}

export class AuthorityRuntime implements AuthorityScope {
  readonly #options: AuthorityRuntimeOptions;
  #store: AuthorityStore | undefined;
  /** The flight-visible lease per normalized slot path, minted by this process. */
  readonly #leases = new Map<string, SourceUseLease>();
  /** Settlements that arrived with no active transaction; applied under the next outermost A. */
  readonly #late: Array<(tx: AuthorityTransaction) => Promise<void>> = [];
  /** Collects the normalized slot paths this runtime admits inside {@link recordAdmissions}. */
  readonly #collector = new AsyncLocalStorage<Set<string>>();
  readonly hooks: GitAuthorityHooks;

  constructor(options: AuthorityRuntimeOptions) {
    this.#options = options;
    this.hooks = {
      admitSourceUse: (clonePath, op) => this.#admit(clonePath, op),
      beginBareMutation: (bare, op, footprint) => this.#beginBare(bare, op, footprint),
      assertBareClear: (bare, op) => this.#assertBareClear(bare, op),
      beginDestructive: (paths, op) => this.#beginDestructive(paths, op),
    };
  }

  /** The transaction of this async context, or undefined. Never acquires A. */
  active(): AuthorityTransaction | undefined {
    return activeAuthorityTransaction(this.#options.lifetime);
  }

  /**
   * Run `fn` inside A: borrow the transaction already active in this async context, otherwise
   * acquire A. Outermost entry points call this BEFORE any C, S, J or B they take. Acquiring A applies
   * the settlements queued by {@link #settlement} first, and after an outermost transaction released
   * with any still queued, a microtask (started from this exit, never from a settle call) acquires A
   * once more to apply them.
   */
  withTransaction<T>(
    fn: (tx: AuthorityTransaction) => Promise<T>,
    options: AuthorityWait = {},
  ): Promise<T> {
    const tx = this.active();
    if (tx) return fn(tx);
    return withAuthorityTransaction(this.#options.lifetime, {
      ...(options.signal ? { signal: options.signal } : {}),
      timeoutMs: options.timeoutMs ?? this.#options.timeoutMs ?? DEFAULT_WAIT_MS,
    }, async (outer) => {
      await this.#applyLate(outer);
      return fn(outer);
    }).then((result) => {
      this.#scheduleDrain();
      return result;
    });
  }

  async #applyLate(tx: AuthorityTransaction): Promise<void> {
    // Taken only once A is held, so a failed acquisition leaves every queued settlement in place.
    for (const apply of this.#late.splice(0)) {
      try { await apply(tx); }
      catch (error) {
        // The durable record stays `running` (fail-closed); it is not retried forever.
        this.#options.log?.warn("late authority settlement could not be applied; its record stays open", {
          error: error instanceof Error ? error.message : String(error) });
      }
    }
  }

  /** Called from an outermost transaction's own exit, which holds no lower lock. */
  #scheduleDrain(): void {
    if (this.#late.length === 0 || this.active()) return;
    queueMicrotask(() => {
      this.withTransaction(async () => undefined, { timeoutMs: LATE_WAIT_MS }).catch((error: unknown) => {
        this.#options.log?.warn("late authority settlements stay queued until the next transaction", {
          error: error instanceof Error ? error.message : String(error) });
      });
    });
  }

  /** The lease this process minted for `clonePath` (what a flight keeps for publication). */
  leaseFor(clonePath: string): SourceUseLease | undefined {
    return this.#leases.get(path.resolve(clonePath));
  }

  /**
   * Run `fn` and add the normalized path of every source slot this runtime admits inside it (whether
   * `fn` then succeeds or not) to `sink`. A flight passes its own set, so {@link endSourceUse} later names
   * exactly what the flight admitted, including a path whose clone creation failed after admission.
   */
  recordAdmissions<T>(sink: Set<string>, fn: () => Promise<T>): Promise<T> {
    return this.#collector.run(sink, fn);
  }

  #note(key: string): void {
    this.#collector.getStore()?.add(key);
  }

  /**
   * End this process's source use of `paths`: each slot this worker still holds `active` under the lease it
   * minted moves to `ineligible`, so a later worker incarnation may use the path again. The CALLER must have
   * ended the source use and positively joined every worker-owned source-capable tail first; this method
   * only performs the durable step. A slot with a running pending record, one this process no longer holds,
   * a transition that fails, or a wait for A that times out stays `active` (fail closed) and is reported in
   * `kept`. Never throws. Runs under its own short A and releases it before returning.
   */
  async endSourceUse(
    paths: Iterable<string>, options: AuthorityWait = {},
  ): Promise<{ ended: string[]; kept: string[] }> {
    const keys = [...new Set([...paths].map((p) => path.resolve(p)))];
    const ended: string[] = [];
    const kept: string[] = [];
    if (keys.length === 0) return { ended, kept };
    const warn = (message: string, fields: Record<string, unknown>): void => this.#options.log?.warn(message, fields);
    try {
      await this.withTransaction(async (tx) => {
        const store = await this.#storeFor(tx);
        for (const key of keys) {
          const lease = this.#leases.get(key);
          if (!lease) { kept.push(key); continue; }
          try {
            const slot = await this.#slotOf(tx, store, key);
            if (!slot || slot.epoch !== leaseEpoch(lease) || slot.owner.worker !== store.workerIncarnation) {
              // Someone else's epoch now: this process holds nothing here to end.
              this.#leases.delete(key);
              continue;
            }
            if (slot.state !== "active") { this.#leases.delete(key); continue; }
            if (slot.pending?.status === "running") {
              kept.push(key);
              warn("source use left active: an operation on it is unresolved", { source: key, operation: slot.pending.operation });
              continue;
            }
            await store.transition(tx, lease, "ineligible");
            this.#leases.delete(key);
            ended.push(key);
          } catch (error) {
            kept.push(key);
            warn("source use left active: it could not be ended", { source: key, error: error instanceof Error ? error.message : String(error) });
          }
        }
      }, options);
    } catch (error) {
      warn("source use left active: the authority transaction was unavailable", { error: error instanceof Error ? error.message : String(error) });
      return { ended, kept: keys.filter((key) => !ended.includes(key)) };
    }
    return { ended, kept };
  }

  /**
   * The executor-launch gate: refuse unless `lease` is this process's CURRENT lease for `clonePath`, the slot
   * is still this worker's active epoch under it, and the slot's owner carries exactly the launching
   * executor's incarnation (none for a claude flight). Runs under its own short A.
   */
  async assertLaunchable(
    clonePath: string, lease: SourceUseLease | undefined, executorIncarnation: string | undefined,
    options: AuthorityWait = {},
  ): Promise<void> {
    await this.withTransaction(async (tx) => {
      const store = await this.#storeFor(tx);
      const key = path.resolve(clonePath);
      if (!lease || this.#leases.get(key) !== lease) {
        throw new AuthorityRefusal("executor launch refused: the flight's source lease is not the current lease for its clone");
      }
      const slot = await this.#slotOf(tx, store, key);
      if (!slot || !this.#ownedActive(store, slot, lease)) {
        throw new AuthorityRefusal("executor launch refused: the clone's source-use epoch is no longer this worker's active epoch");
      }
      if (slot.owner.executor !== (executorIncarnation ?? null) ||
          leaseHarness(lease) !== (executorIncarnation === undefined ? "claude" : "codex")) {
        throw new AuthorityRefusal("executor launch refused: the source-use epoch does not carry this executor's incarnation");
      }
    }, options);
  }

  /**
   * Bind a flight's source to its executor BEFORE the executor launches anything against the clone.
   * A Codex executor presents its incarnation, which mints a fresh epoch that carries it (or keeps the
   * current one when it already does); a Claude flight keeps the claude lease clone admission minted
   * (admitting the clone when none exists).
   */
  async bindSource(clonePath: string, executorIncarnation?: string, wait: AuthorityWait = {}): Promise<SourceUseLease> {
    return this.withTransaction(async (tx) => {
      const store = await this.#storeFor(tx);
      const key = path.resolve(clonePath);
      this.#note(key);
      const held = this.#leases.get(key);
      const slot = held ? await this.#slotOf(tx, store, key) : null;
      if (held && slot && this.#ownedActive(store, slot, held) &&
          (executorIncarnation === undefined || slot.owner.executor === executorIncarnation)) return held;
      if (executorIncarnation === undefined) return this.#mint(tx, store, key);
      const lease = await store.admitSourceUse(tx, key, { executor: executorIncarnation });
      this.#leases.set(key, lease);
      return lease;
    }, wait);
  }

  // ---- hooks ----------------------------------------------------------------------------

  #requireActive(op: string): AuthorityTransaction {
    const tx = this.active();
    if (!tx) throw new AuthorityRefusal(`${op} requires an active authority transaction`);
    return tx;
  }

  /**
   * Cache the store so its leases stay valid across transactions, but open (and so re-verify) it
   * under every one: a reset or replacement is still caught by the open's checks.
   */
  async #storeFor(tx: AuthorityTransaction): Promise<AuthorityStore> {
    const opened = await openAuthorityStore(tx, {
      workerToken: this.#options.workerToken,
      ...(this.#options.workerIncarnation === undefined ? {} : { workerIncarnation: this.#options.workerIncarnation }),
      ...(this.#options.ops === undefined ? {} : { ops: this.#options.ops }),
    });
    if (this.#store && this.#store.storageId === opened.storageId) return this.#store;
    this.#store = opened;
    this.#leases.clear();
    return opened;
  }

  async #slotOf(tx: AuthorityTransaction, store: AuthorityStore, key: string): Promise<SlotSnapshot | null> {
    return store.readSlot(tx, await store.slotKey(tx, key));
  }

  /** The slot is still this process's own live source-use epoch for `lease`. */
  #ownedActive(store: AuthorityStore, slot: SlotSnapshot, lease: SourceUseLease): boolean {
    return slot.state === "active" && slot.owner.worker === store.workerIncarnation && slot.epoch === leaseEpoch(lease);
  }

  async #mint(tx: AuthorityTransaction, store: AuthorityStore, key: string, destructive = false): Promise<SourceUseLease> {
    // Recorded before the admission so a path whose admission wrote the slot and then failed is still named.
    this.#note(key);
    const lease = await store.admitSourceUse(tx, key, destructive ? { destructive: true } : {});
    this.#leases.set(key, lease);
    return lease;
  }

  async #admit(clonePath: string, op: string): Promise<void> {
    const tx = this.#requireActive(op);
    const store = await this.#storeFor(tx);
    const key = path.resolve(clonePath);
    // A seed's parent (`<runnerRoot>/<repoDir>`) does not exist before a repo's first clone, and the store
    // keys a path only when every ancestor exists. Create it exactly as the seed itself would right after.
    if (SEED_OPS.has(op)) await fsp.mkdir(path.dirname(key), { recursive: true });
    // A normalize works on the source its own epoch already covers: the same open lease keeps its
    // epoch. Every other op starts a new use of the path and mints a fresh epoch first.
    if (op === "normalize") {
      const held = this.#leases.get(key);
      const slot = held ? await this.#slotOf(tx, store, key) : null;
      if (held && slot && this.#ownedActive(store, slot, held)) {
        this.#note(key);
        await store.admitSourceUse(tx, key, { lease: held });
        return;
      }
    }
    await this.#mint(tx, store, key);
  }

  /** The lease a bare-mutator or destructive record is carried by: this worker's own live one, else a fresh admission. */
  async #leaseForPending(tx: AuthorityTransaction, store: AuthorityStore, key: string,
    states: readonly SlotSnapshot["state"][]): Promise<SourceUseLease> {
    const held = this.#leases.get(key);
    if (held) {
      const slot = await this.#slotOf(tx, store, key);
      if (slot && slot.owner.worker === store.workerIncarnation && slot.epoch === leaseEpoch(held) && states.includes(slot.state)) {
        return held;
      }
    }
    // No live lease of this worker carries the record. A destructive operation must not become the way a
    // published source loses its proof: over a sealed/claude_ready slot it needs that slot's retiring lease
    // (U5 registers it), and the store refuses a destructive admission over any live publication.
    const slot = await this.#slotOf(tx, store, key);
    if (slot && (slot.state === "sealed" || slot.state === "claude_ready")) {
      throw new AuthorityRefusal("a destructive operation over a published source needs its retiring lease");
    }
    return this.#mint(tx, store, key, true);
  }

  async #beginBare(bare: string, op: string, footprint: readonly string[]): Promise<AuthoritySettlement> {
    const tx = this.#requireActive(op);
    const store = await this.#storeFor(tx);
    const key = path.resolve(bare);
    const covers = [...footprint];
    const text = boundedText(`${op}: ${covers.join(", ")}`, MAX_FOOTPRINT);
    // This worker's own live epoch carries the record; otherwise the epoch and the running record are one write,
    // so a crash cannot leave an active bare slot with nothing pending.
    const held = this.#leases.get(key);
    const slot = held ? await this.#slotOf(tx, store, key) : null;
    if (held && slot && slot.owner.worker === store.workerIncarnation && slot.epoch === leaseEpoch(held) && slot.state === "active") {
      const operation = await store.beginPending(tx, held, { kind: "bare_mutation", footprint: text });
      return this.#settlement(store, [{ lease: held, operation, covers, bareSlot: true, settled: false }]);
    }
    const admitted = await store.admitBareMutation(tx, key, { footprint: text });
    this.#leases.set(key, admitted.lease);
    return this.#settlement(store, [{ lease: admitted.lease, operation: admitted.operation, covers, bareSlot: true, settled: false }]);
  }

  async #assertBareClear(bare: string, op: string): Promise<void> {
    // No transaction: see the header comment. A slot that cannot be decoded or authenticated, or one caught in
    // the head crash window, throws here: it blocks the bare, it is not skipped or completed.
    const slot = await peekAuthoritySlot(
      { dataDir: this.#options.lifetime.dataDir, workerToken: this.#options.workerToken }, path.resolve(bare));
    if (slot?.pending?.status === "running") {
      throw new AuthorityRefusal(`${op}: an unresolved ${slot.pending.kind} (${slot.pending.operation}) covers this bare`);
    }
  }

  async #beginDestructive(paths: readonly string[], op: string): Promise<AuthoritySettlement> {
    const tx = this.#requireActive(op);
    const store = await this.#storeFor(tx);
    const groups = new Map<string, string[]>();
    for (const target of paths) {
      const owner = sourceSlotPath(target);
      const group = groups.get(owner);
      if (group) group.push(path.resolve(target)); else groups.set(owner, [path.resolve(target)]);
    }
    if (groups.size === 0) throw new AuthorityRefusal(`${op}: nothing to register`);
    const begun: PendingPart[] = [];
    try {
      for (const [owner, covers] of groups) {
        const lease = await this.#leaseForPending(tx, store, owner, ["active", "retiring"]);
        const operation = await store.beginPending(tx, lease, {
          kind: "destructive", footprint: boundedText(covers.join("\n"), MAX_FOOTPRINT),
        });
        begun.push({ lease, operation, covers, bareSlot: false, settled: false });
      }
    } catch (error) {
      // No effect has started: the records opened so far were never acted on, so they close ok.
      for (const part of begun) await store.settlePending(tx, part.lease, part.operation, { ok: true }).catch(() => undefined);
      throw error;
    }
    return this.#settlement(store, begun);
  }

  #settlement(store: AuthorityStore, parts: PendingPart[]): AuthoritySettlement {
    const apply = async (tx: AuthorityTransaction, outcome: { ok: true } | { ok: false; residue: string[] }): Promise<void> => {
      for (const part of parts) {
        if (part.settled) continue;
        if (outcome.ok) {
          await store.settlePending(tx, part.lease, part.operation, { ok: true });
          part.settled = true;
          if (part.bareSlot) {
            // Leave `active` so a successor worker is not refused by a slot with nothing pending.
            await store.transition(tx, part.lease, "ineligible").catch((error: unknown) => {
              this.#options.log?.warn("bare-mutator slot could not leave active", { error: error instanceof Error ? error.message : String(error) });
            });
          }
        } else {
          await store.settlePending(tx, part.lease, part.operation, { ok: false, residue: residueFor(part, outcome.residue) });
          part.settled = true;
        }
      }
    };
    return {
      settle: async (outcome) => {
        // Under the caller's transaction (every site inside B) it is applied now. With none active (a child's
        // late close, or the continuation GitCache.cleanupArchivedRecoveryPin abandons on its caller's deadline
        // while still holding B) it must NOT acquire A, which would invert the lock order beneath B: it is queued
        // and the next outermost acquisition applies it. Until then the durable record stays running, fail-closed;
        // if the process dies first a successor finds it unresolved, which is correct.
        const tx = this.active();
        if (tx) { await apply(tx, outcome); return; }
        this.#late.push((outer) => apply(outer, outcome));
      },
    };
  }
}

/** The residue a failed part records: the reported items that belong to it, else what it covers; always within the store's bounds. */
function residueFor(part: PendingPart, reported: readonly string[]): string[] {
  const own = reported.filter((item) => part.covers.includes(path.resolve(item)) || part.covers.includes(item));
  const items = (own.length > 0 ? own : part.covers).map((item) => boundedText(item, MAX_ITEM));
  return items.length > MAX_RESIDUE ? [boundedText(part.covers.join("\n"), MAX_ITEM)] : items;
}
