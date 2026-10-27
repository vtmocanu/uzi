import { createHash } from "node:crypto";
import * as fsp from "node:fs/promises";
import path from "node:path";
import {
  AuthorityRefusal, activeAuthorityTransaction, withAuthorityTransaction,
  type AuthorityTransaction, type WorkerLifetime,
} from "./authority-lock.js";
import {
  leaseEpoch, openAuthorityStore,
  type AuthorityFsOps, type AuthorityStore, type SlotSnapshot, type SourceUseLease,
} from "./completion-authority.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "./git-authority-hooks.js";

// issue #3404 M2 U3 — the process runtime that turns the completion-authority store into the
// GitAuthorityHooks GitCache calls, and the single place a production entry point asks for A.
//
// Lock order is P -> W -> A -> C -> S -> J -> B -> O. The hooks run INSIDE the bare lock B (and
// usually inside C, S and J), so they never acquire A: they require the transaction the outermost
// entry point opened in this async context and refuse with AuthorityRefusal when there is none.
// The one deliberate exception is a settlement that runs after its transaction ended (see
// {@link AuthorityRuntime.withTransaction}).

/** What an entry point needs to open (or borrow) A. Tests substitute an instrumented one. */
export interface AuthorityScope {
  withTransaction<T>(
    fn: (tx: AuthorityTransaction) => Promise<T>,
    options?: { signal?: AbortSignal; timeoutMs?: number },
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
/** A settlement with no active transaction waits at most LATE_ATTEMPTS x LATE_WAIT_MS for A (see #settlement). */
const LATE_WAIT_MS = 20_000;
const LATE_ATTEMPTS = 3;
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
   * acquire A. Outermost entry points call this BEFORE any C, S, J or B they take. A settlement that
   * ran after its transaction ended has no outer lock above it and acquires A here.
   */
  withTransaction<T>(
    fn: (tx: AuthorityTransaction) => Promise<T>,
    options: { signal?: AbortSignal; timeoutMs?: number } = {},
  ): Promise<T> {
    const tx = this.active();
    if (tx) return fn(tx);
    return withAuthorityTransaction(this.#options.lifetime, {
      ...(options.signal ? { signal: options.signal } : {}),
      timeoutMs: options.timeoutMs ?? this.#options.timeoutMs ?? DEFAULT_WAIT_MS,
    }, fn);
  }

  /** The lease this process minted for `clonePath` (what a flight keeps for publication). */
  leaseFor(clonePath: string): SourceUseLease | undefined {
    return this.#leases.get(path.resolve(clonePath));
  }

  /**
   * Bind a flight's source to its executor BEFORE the executor launches anything against the clone.
   * A Codex executor presents its incarnation, which mints a fresh epoch that carries it (or keeps the
   * current one when it already does); a Claude flight keeps the claude lease clone admission minted
   * (admitting the clone when none exists).
   */
  async bindSource(clonePath: string, executorIncarnation?: string): Promise<SourceUseLease> {
    return this.withTransaction(async (tx) => {
      const store = await this.#storeFor(tx);
      const key = path.resolve(clonePath);
      const held = this.#leases.get(key);
      const slot = held ? await this.#slotOf(tx, store, key) : null;
      if (held && slot && this.#ownedActive(store, slot, held) &&
          (executorIncarnation === undefined || slot.owner.executor === executorIncarnation)) return held;
      if (executorIncarnation === undefined) return this.#mint(tx, store, key);
      const lease = await store.admitSourceUse(tx, key, { executor: executorIncarnation });
      this.#leases.set(key, lease);
      return lease;
    });
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

  async #mint(tx: AuthorityTransaction, store: AuthorityStore, key: string): Promise<SourceUseLease> {
    const lease = await store.admitSourceUse(tx, key);
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
    return this.#mint(tx, store, key);
  }

  async #beginBare(bare: string, op: string, footprint: readonly string[]): Promise<AuthoritySettlement> {
    const tx = this.#requireActive(op);
    const store = await this.#storeFor(tx);
    const key = path.resolve(bare);
    const lease = await this.#leaseForPending(tx, store, key, ["active"]);
    const covers = [...footprint];
    const operation = await store.beginPending(tx, lease, {
      kind: "bare_mutation", footprint: boundedText(`${op}: ${covers.join(", ")}`, MAX_FOOTPRINT),
    });
    return this.#settlement(store, [{ lease, operation, covers, bareSlot: true, settled: false }]);
  }

  async #assertBareClear(bare: string, op: string): Promise<void> {
    const tx = this.#requireActive(op);
    const store = await this.#storeFor(tx);
    // A slot that cannot be decoded or authenticated throws here: it blocks the bare, it is not skipped.
    const slot = await this.#slotOf(tx, store, path.resolve(bare));
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
    return {
      settle: async (outcome) => {
        // Settlement runs under the caller's transaction when there is one (every site inside B), and
        // acquires A only when none is active: a child's late close, which holds no lower lock.
        //
        // One more path ends here with no transaction: GitCache.cleanupArchivedRecoveryPin's abandoned
        // continuation (its caller returned on the deadline, its bare lock is still held) settling after the
        // caller's transaction ended. That acquires A beneath B, so the wait is bounded and, on expiry, the
        // settlement fails and the record stays open (a fence, never a clearance) instead of deadlocking.
        const attempt = (timeoutMs: number): Promise<void> => this.withTransaction(async (tx) => {
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
        }, { timeoutMs });
        if (this.active()) { await attempt(LATE_WAIT_MS); return; }
        // At most LATE_ATTEMPTS waits of LATE_WAIT_MS each, and only a timed-out wait is retried.
        for (let n = 1; ; n++) {
          try { await attempt(LATE_WAIT_MS); return; }
          catch (error) {
            if (n >= LATE_ATTEMPTS || !(error instanceof AuthorityRefusal) || error.message !== "authority lock timeout") throw error;
          }
        }
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
