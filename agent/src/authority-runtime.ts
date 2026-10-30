import { AsyncLocalStorage } from "node:async_hooks";
import { createHash } from "node:crypto";
import * as fsp from "node:fs/promises";
import path from "node:path";
import {
  AuthorityRefusal, SourceUseHeldRefusal, activeAuthorityTransaction, withAuthorityTransaction,
  type AuthorityTransaction, type WorkerLifetime,
} from "./authority-lock.js";
import {
  leaseEpoch, leaseHarness, openAuthorityStore, peekAuthoritySlot, processWorkerIncarnation,
  type AuthorityFsOps, type AuthorityStore, type EndedSlotReceipt, type SlotCohort, type SlotSnapshot, type SourceUseLease,
} from "./completion-authority.js";
import {
  sourceCoreDigest, sourceFenceOf, verifyCertifiedSource, verifyPublishedSource,
  type CompletionSourceFence, type CompletionSourceV2,
} from "./completion-source.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "./git-authority-hooks.js";

// issue #3404 M2 U3 — the process runtime that turns the completion-authority store into the
// GitAuthorityHooks GitCache calls, and the single place a production entry point asks for A.
//
// Lock order is P -> W -> A -> C -> S -> J -> B -> O. The hooks run INSIDE the bare lock B (and
// usually inside C, S and J), so they never acquire A: they require the transaction the outermost
// entry point opened in this async context and refuse with AuthorityRefusal when there is none.
// A settlement that runs with no active transaction (a child's late close, or GitCache's abandoned
// cleanupArchivedRecoveryPin continuation, which may still hold B) never acquires A either: it is queued
// in-process and applied by the idle drain the settle schedules (not awaited, no lock held) or at the start of the
// next outermost A acquisition (see {@link AuthorityRuntime.withTransaction}).
//
// assertBareClear reads the bare's durable record. With an authority transaction active (every seed and
// replace site holds A) it reads through the transactional load, which also completes the crash window between
// a slot rename and its head update, so one failed head write can never wedge a repository's seeds. With none
// active it uses a read-only load that never writes (peekAuthoritySlot) and refuses a slot caught in that
// window; its record is written either by an owner that is necessarily dead (W excludes another compatible
// worker) or by this process's own transactions, whose pending records are only ever relaxed (a queued late
// settlement) without the reader's bare lock B: that lock does not serialize those writers.
//
// Ending a source use. A slot a flight's source use left `active` blocks every later worker incarnation (a
// successor cannot advance an active foreign epoch). {@link AuthorityRuntime.endSourceUse} is the live
// owner's one step that moves the slots it admitted for a flight to `ineligible` once the flight's source use
// ended and its worker-owned source-capable tails were positively joined. A flight whose completing source is
// proven publishes sealed/claude_ready through {@link AuthorityRuntime.publishSource} instead. An end that
// could not be applied (A not available in time, a failed write, a running pending record) is kept in an
// in-process queue and retried by the drain until applied or the process exits; a crash never reaches it, so
// the slot stays active.

/** How long, and until what signal, an outermost entry may wait for A. */
export interface AuthorityWait { signal?: AbortSignal; timeoutMs?: number }

/** What an entry point needs to open (or borrow) A. Tests substitute an instrumented one. */
export interface AuthorityScope {
  withTransaction<T>(
    fn: (tx: AuthorityTransaction) => Promise<T>,
    options?: AuthorityWait,
  ): Promise<T>;
  /**
   * issue #3404 M2 U5: the restart-retirement store access. Present on the production {@link AuthorityRuntime};
   * a scope without it (a unit-test double) cannot retire a completed source durably, so a coordinator that has
   * an authority scope and finds none here retains.
   */
  readonly retirement?: RetirementScope;
}

/** What a retirement caller learns about a stored V2 source, read under the active transaction A. */
export interface RetirementView {
  /** The certified source (the certificate verified under this store's key). */
  source: CompletionSourceV2;
  /** `published`: the slot is still sealed/claude_ready for exactly this source. `retiring`: a retirement of it began. */
  phase: "published" | "retiring";
  /** The authenticated cohort manifest the slot binds. */
  cohort: SlotCohort;
  /** Present exactly for `retiring`: the authenticated manifest and the confirmed-deleted prefix length. */
  retiring: { manifest: readonly string[]; deleted: number } | null;
}

/** A retirement entered under A: durable progress, the tombstone, and the fence the destruction must see. */
export interface RetirementHandle {
  readonly fence: CompletionSourceFence;
  /** The authenticated manifest in deletion order; on a resume it is the retiring record's, never a fresh one. */
  readonly manifest: readonly string[];
  /** Confirmed-deleted prefix recorded when the handle was entered. */
  readonly deleted: number;
  advance(deleted: number): Promise<void>;
  /** The durable retired tombstone; requires the whole manifest to be recorded as deleted. */
  finish(): Promise<void>;
}

export interface RetirementScope {
  /** Never mutates a slot (reading may create the private slots directory). Throws AuthorityRefusal for a source that is not published or retiring under this store. */
  inspect(value: unknown): Promise<RetirementView>;
  /**
   * Writes no slot or record (it may create the private slots directory, like `inspect`). Whether ANY authority slot (in any state) covers `clonePath`, read under the active transaction A.
   * Optional so a unit-test double need not carry it; a caller that needs the answer treats its absence as unknown.
   */
  slotExists?(clonePath: string): Promise<boolean>;
  /**
   * Enter retiring for `view` (published: begin; retiring: resume) and register the retiring lease with the
   * runtime, so a destructive record opened for the source's footprint (beginDestructive) is carried by it.
   * `manifest` is used only for a fresh retirement. A running pending record, or a retiring slot this process
   * holds no lease for, refuses.
   */
  enter(view: RetirementView, manifest: readonly string[]): Promise<RetirementHandle>;
}

export interface AuthorityRuntimeOptions {
  lifetime: WorkerLifetime;
  /** The worker join token; the store derives its MAC key from it. */
  workerToken: string;
  /** Tests only: simulate another worker process. */
  workerIncarnation?: string;
  /** Tests only: inject fsync/rename failures into the store. */
  ops?: AuthorityFsOps;
  /** How long after a drain the next one runs while anything is still queued. */
  retryMs?: number;
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
/** A late settlement whose apply failed is re-queued this many times in all, then logged and left `running`. */
const MAX_LATE_ATTEMPTS = 3;
/** While anything is queued, the idle drain runs again after this long. */
const DEFAULT_RETRY_MS = 30_000;
/** The seed ops that may consume a live publication: git.ts runs them after its journal guards found no journal naming the path. */
const CONSUMING_OPS: ReadonlySet<string> = new Set(["seed-canonical", "seed-attempt"]);
const SKILLS_PREFIX = ".uzi-skills-";
/** Retiring-manifest item prefixes (recovery.ts retiringManifest): the journal fence, and a bundle or record file. */
const FENCE_ITEM = "fence:";
const BUNDLE_ITEM = "bundle:";
const RECORD_ITEM = "record:";
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
  readonly #late: Array<{ apply: (tx: AuthorityTransaction) => Promise<void>; attempts: number }> = [];
  /** Source-use ends that could not be applied, per normalized slot path with the lease they must still match. */
  readonly #ends = new Map<string, SourceUseLease>();
  #draining = false;
  #retryTimer: NodeJS.Timeout | undefined;
  /** Collects the normalized slot paths this runtime admits inside {@link recordAdmissions}. */
  readonly #collector = new AsyncLocalStorage<Set<string>>();
  /** Sinks whose flight finished its source use: any guarded admission made inside one is refused, permanently. */
  readonly #retiredSinks = new WeakSet<Set<string>>();
  /**
   * Which sink admitted each normalized slot path, with the epoch it minted. Recorded only after a guarded
   * admission inside a sink actually wrote the slot and set the lease; released only when the end of that
   * epoch is confirmed (see {@link #releaseOwner}). A different sink (or none) cannot admit an owned key.
   */
  readonly #owners = new Map<string, { sink: Set<string>; epoch: string }>();
  /** The operator-discard lease per normalized slot path: an end of that source use tombstones or releases it. */
  readonly #discards = new Map<string, SourceUseLease>();
  /** The footprint items that existed when the operator discard took the slot over, per normalized slot path. */
  readonly #discardPresent = new Map<string, string[]>();
  /** Pending-binder predicates registered by the runners that share this runtime (see {@link registerBinderPending}). */
  readonly #binderPending: Array<(clonePath: string) => boolean> = [];
  readonly hooks: GitAuthorityHooks;
  readonly retirement: RetirementScope;

  constructor(options: AuthorityRuntimeOptions) {
    this.#options = options;
    this.hooks = {
      admitSourceUse: (clonePath, op) => this.#admit(clonePath, op),
      beginBareMutation: (bare, op, footprint) => this.#beginBare(bare, op, footprint),
      assertBareClear: (bare, op) => this.#assertBareClear(bare, op),
      beginDestructive: (paths, op) => this.#beginDestructive(paths, op),
    };
    this.retirement = {
      inspect: (value) => this.#inspectRetirement(value),
      slotExists: (clonePath) => this.#slotExists(clonePath),
      enter: (view, manifest) => this.#enterRetirement(view, manifest),
    };
  }

  /**
   * A runner registers whether a completion-source binder is still pending for a clone path in its process. The
   * retired tombstone is never written for a path while one is: the finisher in {@link #finishRecordedRetirement}
   * waits, like the coordinator's own retirement does, and the path stays refused as `being retired` until a
   * later admission finishes it.
   */
  registerBinderPending(check: (clonePath: string) => boolean): void {
    this.#binderPending.push(check);
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
    }).finally(() => this.#scheduleDrain());
  }

  async #applyLate(tx: AuthorityTransaction): Promise<void> {
    // Taken only once A is held, so a failed acquisition leaves every queued settlement in place.
    for (const entry of this.#late.splice(0)) {
      try { await entry.apply(tx); }
      catch (error) {
        const reason = error instanceof Error ? error.message : String(error);
        if (entry.attempts + 1 < MAX_LATE_ATTEMPTS) {
          this.#late.push({ apply: entry.apply, attempts: entry.attempts + 1 });
          this.#options.log?.warn("late authority settlement could not be applied; it is queued again", { error: reason });
        } else {
          // Fail closed: the durable record stays `running`, and a successor finds it unresolved.
          this.#options.log?.warn("late authority settlement could not be applied; its record stays open", { error: reason });
        }
      }
    }
    await this.#applyEnds(tx);
  }

  /** Retry the source-use ends that were kept: only for a lease that is still current, with nothing running pending. */
  async #applyEnds(tx: AuthorityTransaction): Promise<void> {
    if (this.#ends.size === 0) return;
    let store: AuthorityStore;
    try { store = await this.#storeFor(tx); }
    catch (error) {
      this.#options.log?.warn("queued source-use ends stay queued: the store could not be opened", { error: error instanceof Error ? error.message : String(error) });
      return;
    }
    for (const [key, lease] of this.#ends) {
      try {
        const result = await this.#endOne(tx, store, key, lease);
        if (result !== "kept") this.#ends.delete(key);
      } catch (error) {
        this.#options.log?.warn("a queued source-use end could not be applied yet", { source: key, error: error instanceof Error ? error.message : String(error) });
      }
    }
  }

  /**
   * Idle drain: schedule one acquisition of A (a microtask, never awaited by anyone, holding no lock) when
   * anything is queued and no transaction is active. Called from every outermost transaction's exit (success
   * or failure), from a settle that found no transaction, and from a queued end. One drain runs at a time; while
   * anything stays queued it is repeated on a timer (unref'd, so it never holds the process open).
   */
  #scheduleDrain(): void {
    if (this.#draining || this.active() || (this.#late.length === 0 && this.#ends.size === 0)) return;
    this.#draining = true;
    queueMicrotask(() => {
      this.withTransaction(async () => undefined, { timeoutMs: LATE_WAIT_MS })
        .catch((error: unknown) => {
          this.#options.log?.warn("queued authority work stays queued until the next transaction", {
            error: error instanceof Error ? error.message : String(error) });
        })
        .finally(() => {
          this.#draining = false;
          this.#armRetry();
        });
    });
  }

  #armRetry(): void {
    if (this.#retryTimer !== undefined || (this.#late.length === 0 && this.#ends.size === 0)) return;
    this.#retryTimer = setTimeout(() => {
      this.#retryTimer = undefined;
      this.#scheduleDrain();
    }, this.#options.retryMs ?? DEFAULT_RETRY_MS);
    this.#retryTimer.unref();
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
   * Permanently refuse every guarded admission made inside `sink` (see {@link recordAdmissions}). The flight calls
   * this when its source use ends, so a callback it left behind (a late git hook, a timer) cannot admit again.
   * It is separate from per-key ownership: the keys the sink admitted stay owned until their end is confirmed.
   */
  retireSink(sink: Set<string>): void {
    this.#retiredSinks.add(sink);
  }

  /**
   * Barrier after {@link retireSink}: every admission runs under A, so taking A once (without the pre-callback drain
   * and without opening the store) proves that an admission which passed its entry check before the retirement has
   * finished and added its path to the sink; one that starts later is refused. Resolves true when A was taken (or this
   * context already holds it, where no other admission can be running); false when A could not be taken within the
   * wait (or the lock could not be verified), in which case the caller may rely only on the paths already named: an
   * admission still running is either before its lease (a later end keeps its key, never queuing or ending it) or after
   * it (ended only if that end gets A after the admission finished, else queued); a key without an end receipt blocks
   * any settlement, so no settlement can cover an incomplete set.
   */
  async awaitSinkQuiescent(wait: AuthorityWait = {}): Promise<boolean> {
    if (this.active()) return true;
    try {
      await withAuthorityTransaction(this.#options.lifetime, {
        ...(wait.signal ? { signal: wait.signal } : {}),
        timeoutMs: wait.timeoutMs ?? this.#options.timeoutMs ?? DEFAULT_WAIT_MS,
      }, async () => undefined);
      return true;
    } catch { return false; }
  }

  /**
   * The worker incarnation this runtime's slots are written under, once its store has been opened by any
   * transaction (every admission opens it); undefined before that. Never acquires A. A memory-park record names it
   * as the slot owner (issue #3638).
   */
  authorityIncarnation(): string | undefined {
    return this.#store?.workerIncarnation ?? this.#options.workerIncarnation;
  }

  /**
   * The epoch `sink`'s flight minted for `clonePath` and has not yet confirmed ended, else undefined. Read-only; a
   * memory-park record binds it as `origin.sourceEpoch` (issue #3638).
   */
  ownedSourceEpoch(clonePath: string, sink: Set<string> | undefined): string | undefined {
    const owner = this.#owners.get(path.resolve(clonePath));
    return owner !== undefined && owner.sink === sink ? owner.epoch : undefined;
  }

  /**
   * issue #3638 D3 - read-only verification of previously recorded ended slots. A is held through the same lock
   * primitive as {@link withTransaction}, but the pre-callback drain of queued late settlements and ends is NOT run and
   * the store is not opened for writing: each slot is read by its `path` through {@link peekAuthoritySlot}, its `authorityKey` is compared with the recorded one, and it must be
   * `ineligible` with exactly the recorded epoch and seq, owned by `worker`. Any difference (a successor epoch, a changed
   * seq, another owner, a missing slot) or any uncertainty (a lock timeout, an unreadable store or slot, a slot one ahead of
   * its head) answers false. It writes nothing, authorizes nothing, and is used only to log whether a predecessor's
   * cooperative settlement still holds.
   */
  async verifyEndedSlots(
    sources: ReadonlyArray<{ path: string; authorityKey: string; epoch: string; seq: number }>, worker: string,
    options: AuthorityWait = {},
  ): Promise<boolean> {
    if (sources.length === 0) return false;
    const read = async (): Promise<boolean> => {
      for (const source of sources) {
        const slot = await peekAuthoritySlot({ dataDir: this.#options.lifetime.dataDir, workerToken: this.#options.workerToken },
          path.resolve(source.path));
        if (!slot || slot.key !== source.authorityKey || slot.state !== "ineligible" || slot.epoch !== source.epoch ||
            slot.seq !== source.seq || slot.owner.worker !== worker) return false;
      }
      return true;
    };
    try {
      if (this.active()) return await read();
      return await withAuthorityTransaction(this.#options.lifetime, {
        ...(options.signal ? { signal: options.signal } : {}),
        timeoutMs: options.timeoutMs ?? LATE_WAIT_MS,
      }, read);
    } catch {
      return false;
    }
  }

  /**
   * issue #3638 D4 - classify, WITHOUT writing or repairing anything, what an admission of `clonePath` made by the flight
   * owning `sink` would meet. A is held through the same lock primitive as {@link withTransaction}, but the pre-callback
   * drain of queued late settlements and ends is NOT run, no idle drain is scheduled and the store is not (re)opened for
   * writing: the slot is read only through {@link peekAuthoritySlot}, which never completes a slot that is one ahead of
   * its head. Every uncertainty (that refusal, a lock timeout, an unreadable slot or store) is `other`, never eligibility.
   *
   * - `foreign_active`: the slot is `active` under another worker incarnation (the refusal bindSource would raise);
   * - `held_unended`: this process's earlier flight (another sink) still owns the key with no confirmed end;
   * - `admissible`: an admission would proceed (no slot, `ineligible`, or this incarnation's own active epoch);
   * - `other`: a pending footprint, retiring, published or retired slot, or anything uncertain.
   * `owner` names the slot's owner incarnation and epoch whenever it was read.
   */
  async classifySourceAdmission(
    clonePath: string, sink: Set<string> | undefined, wait: AuthorityWait = {},
  ): Promise<{ kind: "foreign_active" | "held_unended" | "admissible" | "other"; owner?: { worker: string; epoch: string } }> {
    const key = path.resolve(clonePath);
    const read = async (): Promise<{ kind: "foreign_active" | "held_unended" | "admissible" | "other"; owner?: { worker: string; epoch: string } }> => {
      const slot = await peekAuthoritySlot({ dataDir: this.#options.lifetime.dataDir, workerToken: this.#options.workerToken }, key);
      const held = this.heldByOtherSink(key, sink);
      if (!slot) return { kind: held ? "other" : "admissible" };
      const owner = { worker: slot.owner.worker, epoch: slot.epoch };
      if (slot.pending !== null || slot.state === "retiring" || slot.state === "retired" ||
          slot.state === "sealed" || slot.state === "claude_ready") return { kind: "other", owner };
      const incarnation = this.authorityIncarnation() ?? processWorkerIncarnation();
      if (slot.state === "active") {
        if (slot.owner.worker !== incarnation) return { kind: "foreign_active", owner };
        return { kind: held ? "held_unended" : "admissible", owner };
      }
      return { kind: held ? "other" : "admissible", owner };
    };
    try {
      if (this.active()) return await read();
      return await withAuthorityTransaction(this.#options.lifetime, {
        ...(wait.signal ? { signal: wait.signal } : {}),
        timeoutMs: wait.timeoutMs ?? this.#options.timeoutMs ?? DEFAULT_WAIT_MS,
      }, read);
    } catch {
      return { kind: "other" };
    }
  }

  /** Whether `clonePath` is owned by a sink other than `sink` whose end has not been confirmed. Read-only. */
  heldByOtherSink(clonePath: string, sink: Set<string> | undefined): boolean {
    const owner = this.#owners.get(path.resolve(clonePath));
    return owner !== undefined && owner.sink !== sink;
  }

  /** Refuse, before any effect, an admission made inside a retired sink. */
  #assertSinkLive(): void {
    const sink = this.#collector.getStore();
    if (sink && this.#retiredSinks.has(sink)) {
      throw new SourceUseHeldRefusal("source use refused: the flight that made this admission already ended its source use");
    }
  }

  /** Refuse, before any effect, an admission of a key another sink (or this one's predecessor) still owns. */
  #assertNotOwnedByOther(key: string): void {
    const owner = this.#owners.get(key);
    if (owner && owner.sink !== this.#collector.getStore()) {
      throw new SourceUseHeldRefusal("source use refused: an earlier flight's source use of this clone has no confirmed end");
    }
  }

  #recordOwner(key: string, lease: SourceUseLease): void {
    const sink = this.#collector.getStore();
    if (sink) this.#owners.set(key, { sink, epoch: leaseEpoch(lease) });
  }

  /** The end of `key`'s recorded epoch is confirmed (or the slot is provably no longer that epoch): ownership ends. */
  #releaseOwner(key: string): void {
    this.#owners.delete(key);
  }

  /**
   * Release `key`'s ownership when the slot is provably no longer this worker's active epoch of the recorded
   * owner. Anything uncertain (an unreadable slot) leaves the owner in place.
   */
  async #recheckOwner(tx: AuthorityTransaction, store: AuthorityStore, key: string, known?: SlotSnapshot | null): Promise<void> {
    const owner = this.#owners.get(key);
    if (!owner) return;
    let slot: SlotSnapshot | null;
    try { slot = known !== undefined ? known : await this.#slotOf(tx, store, key); }
    catch { return; }
    if (!slot || slot.state !== "active" || slot.owner.worker !== store.workerIncarnation || slot.epoch !== owner.epoch) {
      this.#releaseOwner(key);
    }
  }

  /**
   * End this process's source use of `paths`: each slot this worker still holds `active` under the lease it
   * minted moves to `ineligible`, so a later worker incarnation may use the path again. The CALLER must have
   * ended the source use and positively joined every worker-owned source-capable tail first; this method
   * only performs the durable step. A slot with a running pending record, a transition that fails, or a wait
   * for A that times out stays `active` (fail closed), is reported in `kept`, and is kept in an in-process
   * queue that the drain retries (only while its lease is still current and nothing running is pending) until
   * it is applied or the process exits. A path this process holds no lease for is only reported. Never throws.
   * Runs under its own A (borrowing the active one) and releases it before returning.
   */
  async endSourceUse(
    paths: Iterable<string>, options: AuthorityWait & { sink?: Set<string>; skipFlightOwned?: boolean } = {},
  ): Promise<{ ended: string[]; kept: string[]; endedSlots: Array<EndedSlotReceipt & { path: string }> }> {
    const keys = [...new Set([...paths].map((p) => path.resolve(p)))];
    const ended: string[] = [];
    const kept: string[] = [];
    // A key another unreleased sink owns is not this caller's to end: reported kept, never queued, never released.
    // Computed before A is taken: a stale answer only keeps a key (fails closed). The `foreign` answer is stable
    // for a key that is already owned when it is computed, because another sink's admission of an owned key is
    // refused while its current owner holds it; a key that was unowned when computed and is admitted meanwhile is
    // not in `foreign`, and the captured-lease check below drops it (this call holds no lease for it).
    // `skipFlightOwned` (a caller that is no flight, e.g. a retained-source discard that may have been refused before
    // admitting anything) keeps every owned key that has a captured lease unless that lease is the key's current
    // discard lease (the operator discard of an unjoined key), which still ends. An owned key with no current lease
    // is kept and rechecked, and its owner is released only when the recheck proves there is no epoch to end.
    const foreign = new Set(keys.filter((key) => options.skipFlightOwned === true && this.#owners.has(key) &&
        this.#discards.get(key) !== this.#leases.get(key) ||
      options.sink !== undefined && this.heldByOtherSink(key, options.sink)));
    if (foreign.size > 0) this.#options.log?.warn("source use left active: another flight owns it", { sources: foreign.size });
    // The durable identity of each slot this call moved to `ineligible` itself: the only positive end evidence.
    const endedSlots: Array<EndedSlotReceipt & { path: string }> = [];
    if (keys.length === 0) return { ended, kept, endedSlots };
    const warn = (message: string, fields: Record<string, unknown>): void => this.#options.log?.warn(message, fields);
    // The leases are captured now: a later end applies only to exactly the epoch this call was asked to end.
    const captured = new Map<string, SourceUseLease>();
    for (const key of keys) { const lease = this.#leases.get(key); if (lease) captured.set(key, lease); }
    const queue = (key: string): void => {
      const lease = captured.get(key);
      if (lease && this.#leases.get(key) === lease) this.#ends.set(key, lease);
    };
    try {
      await this.withTransaction(async (tx) => {
        const store = await this.#storeFor(tx);
        for (const key of keys) {
          if (foreign.has(key)) { kept.push(key); continue; }
          const lease = captured.get(key);
          if (!lease) { kept.push(key); await this.#recheckOwner(tx, store, key); continue; }
          try {
            const result = await this.#endOne(tx, store, key, lease, (receipt) => endedSlots.push({ ...receipt, path: key }));
            if (result === "ended") ended.push(key);
            else if (result === "kept") { kept.push(key); queue(key); }
          } catch (error) {
            kept.push(key);
            queue(key);
            warn("source use left active: it could not be ended", { source: key, error: error instanceof Error ? error.message : String(error) });
          }
        }
      }, options);
    } catch (error) {
      warn("source use left active: the authority transaction was unavailable", { error: error instanceof Error ? error.message : String(error) });
      for (const key of keys) if (!ended.includes(key) && !foreign.has(key)) queue(key);
      this.#scheduleDrain();
      return { ended, kept: keys.filter((key) => !ended.includes(key)), endedSlots };
    }
    return { ended, kept, endedSlots };
  }

  /**
   * One slot's end under A: "ended" (`onEnded` gets the saved slot identity when this call wrote the end),
   * "dropped" (this process holds nothing here to end) or "kept" (retry later).
   */
  async #endOne(
    tx: AuthorityTransaction, store: AuthorityStore, key: string, lease: SourceUseLease,
    onEnded?: (receipt: EndedSlotReceipt) => void,
  ): Promise<"ended" | "dropped" | "kept"> {
    if (this.#leases.get(key) !== lease) { await this.#recheckOwner(tx, store, key); return "dropped"; }
    const slot = await this.#slotOf(tx, store, key);
    if (!slot || slot.epoch !== leaseEpoch(lease) || slot.owner.worker !== store.workerIncarnation) {
      // Someone else's epoch now: this process holds nothing here to end.
      this.#leases.delete(key);
      await this.#recheckOwner(tx, store, key, slot);
      return "dropped";
    }
    if (slot.state === "retiring" && this.#discards.get(key) === lease) return this.#endDiscard(tx, store, key, lease, slot);
    if (slot.state !== "active") { this.#leases.delete(key); await this.#recheckOwner(tx, store, key, slot); return "dropped"; }
    if (slot.pending?.status === "running") {
      this.#options.log?.warn("source use left active: an operation on it is unresolved", { source: key, operation: slot.pending.operation });
      return "kept";
    }
    const receipt = await store.transition(tx, lease, "ineligible");
    this.#leases.delete(key);
    this.#releaseOwner(key);
    onEnded?.(receipt);
    return "ended";
  }

  /**
   * End an operator discard (U5): the slot is tombstoned only when every footprint item is confirmed absent
   * (lstat ENOENT; any other answer counts as present). When nothing the discard found was deleted the store
   * restores the slot exactly as the takeover found it; otherwise (a partial deletion) it leaves `retiring` for
   * `ineligible` or keeps its settled_failed residue. A running pending record keeps it for a later retry.
   */
  async #endDiscard(tx: AuthorityTransaction, store: AuthorityStore, key: string, lease: SourceUseLease, slot: SlotSnapshot): Promise<"ended" | "kept"> {
    if (slot.pending?.status === "running") {
      this.#options.log?.warn("operator discard left retiring: an operation on it is unresolved", { source: key, operation: slot.pending.operation });
      return "kept";
    }
    const presentNow = async (item: string): Promise<boolean> =>
      fsp.lstat(item).then(() => true, (error: NodeJS.ErrnoException) => error.code !== "ENOENT");
    let allGone = true;
    for (const item of slot.retiring?.manifest ?? []) {
      if (await presentNow(item)) { allGone = false; break; }
    }
    // Nothing deleted: every item that existed when the discard took the slot over still exists. The store then
    // puts the slot back exactly as the takeover found it instead of weakening it (a foreign active epoch stays
    // guarded, a settled_failed record stays evidence).
    let nothingDeleted = !allGone;
    if (nothingDeleted) {
      for (const item of this.#discardPresent.get(key) ?? []) {
        if (!(await presentNow(item))) { nothingDeleted = false; break; }
      }
    }
    await store.endOperatorDiscard(tx, lease, { allGone, nothingDeleted });
    this.#leases.delete(key);
    this.#releaseOwner(key);
    this.#discards.delete(key);
    this.#discardPresent.delete(key);
    return "ended";
  }

  // ---- restart retirement (U5) -------------------------------------------------------------

  /**
   * A `retiring` restart-retirement slot whose manifest is CONFIRMED fully deleted needs only its tombstone:
   * write it (taking over a dead owner's retirement first) unless an operation on it is unresolved or a binder
   * for the path is pending in this process. Recorded progress is not the only proof: a crash or failed write
   * after the last record unlink and before the final progress write leaves no record to resume from, so every
   * file item is lstat-confirmed absent here, and progress must already cover the journal fence (progress is
   * recorded only from a Git read of the journal, and the record deletions that follow it come after it).
   * Anything present or unconfirmed leaves the slot as it is and the admission that follows refuses as before.
   * An operator-discard retiring slot (no fence item) is never finished here.
   */
  async #finishRecordedRetirement(tx: AuthorityTransaction, store: AuthorityStore, key: string): Promise<void> {
    const slot = await this.#slotOf(tx, store, key);
    const retiring = slot?.retiring;
    if (!slot || slot.state !== "retiring" || !retiring || slot.pending?.status === "running") return;
    if (this.#binderPending.some((check) => check(key))) return;
    const fenceIndex = retiring.manifest.findIndex((item) => item.startsWith(FENCE_ITEM));
    if (fenceIndex < 0 || retiring.deleted <= fenceIndex) return;
    for (const item of retiring.manifest) {
      if (item.startsWith(FENCE_ITEM)) continue;
      const file = item.startsWith(BUNDLE_ITEM) || item.startsWith(RECORD_ITEM) ? item.slice(item.indexOf(":") + 1) : item;
      const present = await fsp.lstat(file).then(() => true, (error: NodeJS.ErrnoException) => error.code !== "ENOENT");
      if (present) return;
    }
    try {
      let lease = this.#leases.get(key);
      if (slot.owner.worker !== store.workerIncarnation || !lease || leaseEpoch(lease) !== slot.epoch) {
        lease = await store.beginRetirement(tx, key, { expectedEpoch: slot.epoch, expectedDigest: retiring.prior, manifest: [] });
      }
      if (retiring.deleted !== retiring.manifest.length) await store.advanceRetiring(tx, lease, retiring.manifest.length);
      await store.markRetired(tx, lease);
      this.#leases.delete(key);
      this.#discards.delete(key);
    } catch (error) {
      this.#options.log?.warn("a fully deleted retirement could not be tombstoned", { source: key, error: error instanceof Error ? error.message : String(error) });
    }
  }

  async #slotExists(clonePath: string): Promise<boolean> {
    const tx = this.#requireActive("retirement");
    const store = await this.#storeFor(tx);
    return (await this.#slotOf(tx, store, path.resolve(clonePath))) !== null;
  }

  async #inspectRetirement(value: unknown): Promise<RetirementView> {
    const tx = this.#requireActive("retirement");
    const store = await this.#storeFor(tx);
    const source = verifyCertifiedSource(tx, store, value);
    const slot = await this.#slotOf(tx, store, path.resolve(source.clonePath));
    if (!slot) throw new AuthorityRefusal("no authority slot covers the source", "no_slot");
    const digest = sourceCoreDigest(source);
    if (slot.state === "retiring") {
      const { retiring, publication } = slot;
      if (!retiring || !publication || retiring.prior !== digest || publication.sourceCoreDigest !== digest ||
          publication.harness !== source.harness || slot.epoch !== source.sourceUseEpoch) {
        throw new AuthorityRefusal("the retiring slot is not this source's retirement", "slot_mismatch");
      }
      if (slot.pending?.status === "running") throw new AuthorityRefusal("an operation on the retiring source is unresolved", "operation_unresolved");
      return { source, phase: "retiring", cohort: structuredClone(publication.cohort),
        retiring: { manifest: [...retiring.manifest], deleted: retiring.deleted } };
    }
    const { publication } = await verifyPublishedSource(tx, store, value);
    return { source, phase: "published", cohort: structuredClone(publication.cohort), retiring: null };
  }

  async #enterRetirement(view: RetirementView, manifest: readonly string[]): Promise<RetirementHandle> {
    const tx = this.#requireActive("retirement");
    const store = await this.#storeFor(tx);
    const key = path.resolve(view.source.clonePath);
    const digest = sourceCoreDigest(view.source);
    let lease: SourceUseLease;
    if (view.phase === "published") {
      // Restart (published by a dead incarnation) or this worker's own published slot: either way the store
      // verifies the exact epoch and digest and writes retiring + the manifest BEFORE any destruction.
      lease = await store.beginRetirement(tx, key, { expectedEpoch: view.source.sourceUseEpoch, expectedDigest: digest, manifest: [...manifest] });
    } else {
      const slot = await this.#slotOf(tx, store, key);
      if (!slot || slot.state !== "retiring" || slot.epoch !== view.source.sourceUseEpoch || slot.retiring?.prior !== digest) {
        throw new AuthorityRefusal("the retiring slot is not this source's retirement", "slot_mismatch");
      }
      if (slot.owner.worker === store.workerIncarnation) {
        // Our own partial retirement: only the lease this process registered for it may continue it.
        const held = this.#leases.get(key);
        if (!held || leaseEpoch(held) !== slot.epoch) throw new AuthorityRefusal("this worker holds the retiring slot without its lease");
        if (slot.pending?.status === "running") throw new AuthorityRefusal("an operation on the retiring source is unresolved", "operation_unresolved");
        lease = held;
      } else {
        // A dead owner's retirement: taken over unless its helper may still be running (the store refuses that).
        lease = await store.beginRetirement(tx, key, { expectedEpoch: slot.epoch, expectedDigest: digest, manifest: [] });
      }
    }
    // Registered so beginDestructive (the runner-uid delete) carries its record on THIS retiring epoch.
    this.#leases.set(key, lease);
    const recorded = (await this.#slotOf(tx, store, key))?.retiring;
    if (!recorded) throw new AuthorityRefusal("retiring record missing after entry");
    return {
      fence: sourceFenceOf(view.source), manifest: [...recorded.manifest], deleted: recorded.deleted,
      advance: (deleted) => store.advanceRetiring(this.#requireActive("retirement"), lease, deleted),
      finish: async () => {
        await store.markRetired(this.#requireActive("retirement"), lease);
        this.#leases.delete(key);
      },
    };
  }

  /**
   * issue #3404 M2 U4 - PUBLISH the source `lease` covers instead of ending it. One outer A, acquired here by
   * the caller (never beneath J or B): it requires `lease` to be this process's current lease for `clonePath`
   * and its slot to still be this worker's active epoch with nothing pending, then runs `work`, which does
   * the certify / fence / record / slot-transition sequence under that same A. Anything else, or any throw
   * from `work`, publishes nothing: the slot is moved to `ineligible` (best effort; `ended` says whether that
   * write landed, and if it did not the slot stays active, fail closed) and the reason is returned. A stale publisher therefore refuses and writes
   * nothing, and a publisher that lost its A wait is not retried. Never throws.
   */
  async publishSource(
    clonePath: string, lease: SourceUseLease, options: AuthorityWait,
    work: (context: { tx: AuthorityTransaction; store: AuthorityStore }) => Promise<void>,
  ): Promise<{ published: true } | { published: false; reason: string; ended: boolean }> {
    const key = path.resolve(clonePath);
    const reasonOf = (error: unknown): string => error instanceof Error ? error.message : String(error);
    try {
      return await this.withTransaction(async (tx): Promise<{ published: true } | { published: false; reason: string; ended: boolean }> => {
        const store = await this.#storeFor(tx);
        if (this.#leases.get(key) !== lease) return { published: false, reason: "the flight's source lease is not the current lease for its clone", ended: false };
        const slot = await this.#slotOf(tx, store, key);
        if (!slot || !this.#ownedActive(store, slot, lease)) {
          return { published: false, reason: "the source-use epoch is no longer this worker's active epoch", ended: false };
        }
        if (slot.pending !== null) return { published: false, reason: "an operation on the source is unresolved", ended: false };
        try {
          await work({ tx, store });
          this.#leases.delete(key);
          this.#releaseOwner(key);
          return { published: true };
        } catch (error) {
          let ended = false;
          try {
            await store.transition(tx, lease, "ineligible");
            this.#leases.delete(key);
            this.#releaseOwner(key);
            ended = true;
          } catch (demote) {
            this.#options.log?.warn("a failed publication left its source active", { error: reasonOf(demote) });
          }
          return { published: false, reason: reasonOf(error), ended };
        }
      }, options);
    } catch (error) {
      return { published: false, reason: reasonOf(error), ended: false };
    }
  }

  /**
   * The executor-launch gate: refuse unless `lease` is this process's CURRENT lease for `clonePath`, the slot
   * is still this worker's active epoch under it, and the slot's owner carries exactly the launching
   * executor's incarnation (none for a claude flight). A no-A read-only check first; any refusal falls back to a
   * transactional check that waits for A as before.
   */
  async assertLaunchable(
    clonePath: string, lease: SourceUseLease | undefined, executorIncarnation: string | undefined,
    options: AuthorityWait = {},
  ): Promise<void> {
    // A normal launch compares only this process's own lease and slot: try that with no A first (a read-only
    // load) so contention on A (a long recovery pass) cannot refuse a normal run. Anything but a clean match,
    // including a slot in the head crash window or a store this process has not opened, falls back to the
    // transactional check below, which waits for A as before.
    if (await this.#launchableWithoutA(clonePath, lease, executorIncarnation)) return;
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

  async #launchableWithoutA(clonePath: string, lease: SourceUseLease | undefined, executorIncarnation: string | undefined): Promise<boolean> {
    const store = this.#store;
    const key = path.resolve(clonePath);
    if (!store || !lease || this.#leases.get(key) !== lease || this.active()) return false;
    try {
      const slot = await peekAuthoritySlot(
        { dataDir: this.#options.lifetime.dataDir, workerToken: this.#options.workerToken }, key);
      return !!slot && this.#ownedActive(store, slot, lease) && slot.owner.executor === (executorIncarnation ?? null) &&
        leaseHarness(lease) === (executorIncarnation === undefined ? "claude" : "codex");
    } catch { return false; }
  }

  /**
   * Bind a flight's source to its executor BEFORE the executor launches anything against the clone.
   * A Codex executor presents its incarnation, which mints a fresh epoch that carries it (or keeps the
   * current one when it already does); a Claude flight keeps the claude lease clone admission minted
   * (admitting the clone when none exists).
   */
  async bindSource(clonePath: string, executorIncarnation?: string, wait: AuthorityWait = {}): Promise<SourceUseLease> {
    this.#assertSinkLive();
    return this.withTransaction(async (tx) => {
      // Before the store is opened and before the path is named: a refusal leaves no trace.
      this.#assertSinkLive();
      this.#assertNotOwnedByOther(path.resolve(clonePath));
      const store = await this.#storeFor(tx);
      const key = path.resolve(clonePath);
      this.#note(key);
      const held = this.#leases.get(key);
      const slot = held ? await this.#slotOf(tx, store, key) : null;
      // A held lease is kept only when it is this flight's harness: a Claude flight never inherits a Codex lease.
      // A lease with a queued end is never re-adopted: its flight ended, so a later flight needs a fresh epoch
      // (adopting it would let that queued end retire the slot under the new flight).
      if (held && slot && this.#ends.get(key) !== held && this.#ownedActive(store, slot, held) &&
          (executorIncarnation === undefined ? leaseHarness(held) === "claude" : slot.owner.executor === executorIncarnation)) return held;
      if (executorIncarnation === undefined) return this.#mint(tx, store, key);
      // Re-checked inside A after the store and slot I/O above: the sink may have been retired meanwhile.
      this.#assertSinkLive();
      const lease = await store.admitSourceUse(tx, key, { executor: executorIncarnation });
      this.#leases.set(key, lease);
      this.#recordOwner(key, lease);
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
    // Per-key ownership is cleared only here, on a storageId change. openAuthorityStore refuses a storage
    // identity change after enrollment (completion-authority.ts), so ownership is never released mid-life.
    if (this.#store && this.#store.storageId === opened.storageId) return this.#store;
    this.#store = opened;
    this.#leases.clear();
    this.#owners.clear();
    this.#discards.clear();
    this.#discardPresent.clear();
    return opened;
  }

  async #slotOf(tx: AuthorityTransaction, store: AuthorityStore, key: string): Promise<SlotSnapshot | null> {
    return store.readSlot(tx, await store.slotKey(tx, key));
  }

  /** The slot is still this process's own live source-use epoch for `lease`. */
  #ownedActive(store: AuthorityStore, slot: SlotSnapshot, lease: SourceUseLease): boolean {
    return slot.state === "active" && slot.owner.worker === store.workerIncarnation && slot.epoch === leaseEpoch(lease);
  }

  async #mint(
    tx: AuthorityTransaction, store: AuthorityStore, key: string,
    purpose: { destructive?: boolean; consumePublication?: boolean } = {},
  ): Promise<SourceUseLease> {
    // Re-checked inside A, after the caller's store and slot I/O and before the write (a destructive record is not a flight's use).
    if (purpose.destructive !== true) this.#assertSinkLive();
    // Recorded before the admission so a path whose admission wrote the slot and then failed is still named.
    this.#note(key);
    const lease = await store.admitSourceUse(tx, key, purpose);
    this.#leases.set(key, lease);
    // A destructive record's lease is not a flight's source use: only a non-destructive mint is owned.
    if (purpose.destructive !== true) this.#recordOwner(key, lease);
    return lease;
  }

  async #admit(clonePath: string, op: string): Promise<void> {
    const tx = this.#requireActive(op);
    const key = path.resolve(clonePath);
    // Guarded before the store is opened, the seed parent is created or anything is named. The operator
    // discard is unguarded by design: it proves its writers gone itself (opts.quiesce).
    if (op !== "discard") {
      this.#assertSinkLive();
      this.#assertNotOwnedByOther(key);
    }
    const store = await this.#storeFor(tx);
    // A seed's parent (`<runnerRoot>/<repoDir>`) does not exist before a repo's first clone, and the store
    // keys a path only when every ancestor exists. Create it exactly as the seed itself would right after.
    if (SEED_OPS.has(op)) await fsp.mkdir(path.dirname(key), { recursive: true });
    // The guarded operator discard already authorized exactly this clone and its paired skills dir: the one
    // operator-discard takeover opens its own retiring transaction over that footprint, whatever authority
    // state the slot is in (a crashed worker's active epoch, settled_failed residue); a running pending refuses.
    if (op === "discard") {
      this.#note(key);
      const footprint = [key, path.join(path.dirname(key), `${SKILLS_PREFIX}${path.basename(key)}`)];
      const existing: string[] = [];
      for (const item of footprint) {
        if (await fsp.lstat(item).then(() => true, (error: NodeJS.ErrnoException) => error.code !== "ENOENT")) existing.push(item);
      }
      const lease = await store.admitOperatorDiscard(tx, key, footprint);
      this.#leases.set(key, lease);
      this.#discards.set(key, lease);
      this.#discardPresent.set(key, existing);
      return;
    }
    // A restart retirement whose whole manifest is CONFIRMED deleted (lstat; see #finishRecordedRetirement) but whose
    // tombstone was never written (a crash or failed write after the last unlink) is finished here, so it cannot wedge
    // the path's next use. A pending binder for the path in this process defers it: the path stays refused.
    await this.#finishRecordedRetirement(tx, store, key);
    // A normalize works on the source its own epoch already covers: the same open lease keeps its
    // epoch. Every other op starts a new use of the path and mints a fresh epoch first.
    if (op === "normalize") {
      const held = this.#leases.get(key);
      const slot = held ? await this.#slotOf(tx, store, key) : null;
      if (held && slot && this.#ends.get(key) !== held && this.#ownedActive(store, slot, held)) {
        this.#assertSinkLive();
        this.#note(key);
        await store.admitSourceUse(tx, key, { lease: held });
        return;
      }
    }
    // Only the canonical/attempt seeds may consume a live publication (git.ts runs them after the journal guards).
    await this.#mint(tx, store, key, CONSUMING_OPS.has(op) ? { consumePublication: true } : {});
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
    return this.#mint(tx, store, key, { destructive: true });
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
    // With A held (every seed and replace site) read through the transactional load: it completes the crash
    // window between a slot rename and its head update, which the read-only peek refuses and never repairs, so
    // one failed head write would otherwise block every seed of the repository across restarts. With no
    // transaction, peek: a slot that cannot be decoded or authenticated, or one caught in that window, throws
    // here; it blocks the bare, it is not skipped or completed.
    const tx = this.active();
    const slot = tx
      ? await this.#slotOf(tx, await this.#storeFor(tx), path.resolve(bare))
      : await peekAuthoritySlot(
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
        // and the idle drain (scheduled here, not awaited) or the next outermost acquisition applies it. Until then the durable record stays running, fail-closed;
        // if the process dies first a successor finds it unresolved, which is correct.
        const tx = this.active();
        if (tx) { await apply(tx, outcome); return; }
        this.#late.push({ apply: (outer) => apply(outer, outcome), attempts: 0 });
        // Idle drain, started from here: it holds no lock and nobody awaits it (see #scheduleDrain).
        this.#scheduleDrain();
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
