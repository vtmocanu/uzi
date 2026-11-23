import fs from "node:fs";
import path from "node:path";

/**
 * Same-key exclusion for the retained-recovery inventory proof (issue #4632).
 *
 * The proof runs unmarked runner-uid git in a failed predecessor's retained clone. A same-key attempt seed
 * sweeps every non-live path of the clone key and would see that git as unattributed residue. The proof
 * therefore runs under the repo's bare lock B and leaves an in-memory entry per (bare, cloneKey) until the
 * observe-only probe proves nothing it started is still in scope. Same-key seeds refuse (nonterminally)
 * while an entry stands. The registry here is pure; GitCache owns one instance and the clear logic.
 */

/** Why a same-key operation was refused. */
export type InventoryExclusionReason = "excluded" | "deferred_restore" | "clear_unavailable";

/** A same-key seed or reclaim refused because an inventory proof's work may still be in scope. Nonterminal:
 *  the caller retries later. Deliberately not an AuthorityRefusal. */
export class InventorySourceExclusionRefusal extends Error {
  constructor(
    readonly bare: string,
    readonly key: string,
    readonly clone: string | undefined,
    readonly reason: InventoryExclusionReason,
  ) {
    super(`inventory source exclusion: ${reason} (same-key work refused until the retained inventory proof is settled)`);
    this.name = "InventorySourceExclusionRefusal";
  }
}

// Budgets, measured on this worker: the helper scan overhead is about 0.5 s and `git status --porcelain` p99 is
// 172 ms on an 11k-file tree. The section budget D plus the seed probe must stay within 15 s per queued section,
// against the 30 s flight-boundary A wait (`codexBoundaryDeadlineMs` in runner.ts).

/** Longest a section waits to acquire the repo's bare lock B before refusing with source_lock_unavailable. */
export const INVENTORY_LOCK_ACQUIRE_MS = 30_000;
/** Default budget D of one locked inventory section, from lock acquisition to the end of its body and step 7. */
export const INVENTORY_SECTION_BUDGET_MS = 12_000;
/** Budget of the lazy full clear a section runs on entry, to settle a predecessor's leftover entry. */
export const INVENTORY_LAZY_CLEAR_MS = 2_000;
/** Budget of the after-scan the body runs over the clone before the section may clear (step 7 input). */
export const INVENTORY_SCAN_BUDGET_MS = 3_000;
/** Timeout of the porcelain `git status` the inventory proof runs in the retained clone. */
export const INVENTORY_STATUS_TIMEOUT_MS = 3_000;
/** Least time that must remain before D for step 7 (the full clear) to be attempted at all. */
export const INVENTORY_FULL_PROBE_MIN_MS = 1_000;
/** Budget of the bounded clear a seed site or reclaim admission runs before it refuses. */
export const SEED_CLEAR_BUDGET_MS = 3_000;

/** One installed exclusion: a section's claim that its started work may still be in scope of `clonePath`'s key. */
export interface ExclusionEntry {
  id: string;
  clonePath: string;
  /** True once the section may have started a git process in the clone. */
  gitStarted: boolean;
  /** Wrapped (never rejecting) promises of the work the section started. */
  pending: Set<Promise<unknown>>;
}

const registryKey = (bare: string, cloneKey: string): string => `${bare}\0${cloneKey}`;

export class InventoryExclusionRegistry {
  private readonly entries = new Map<string, Map<string, ExclusionEntry>>();
  private readonly admissions = new Map<string, Map<string, number>>();
  /** Promises (the wrapped ones in `pending`) that have settled; read synchronously by allPendingSettled. */
  private readonly settledFlags = new WeakSet<Promise<unknown>>();

  addEntry(bare: string, cloneKey: string, entry: ExclusionEntry): void {
    const k = registryKey(bare, cloneKey);
    const m = this.entries.get(k) ?? new Map<string, ExclusionEntry>();
    m.set(entry.id, entry);
    this.entries.set(k, m);
  }

  removeEntry(bare: string, cloneKey: string, id: string): void {
    const k = registryKey(bare, cloneKey);
    const m = this.entries.get(k);
    if (!m) return;
    m.delete(id);
    if (m.size === 0) this.entries.delete(k);
  }

  entriesOf(bare: string, cloneKey: string): ExclusionEntry[] {
    return [...(this.entries.get(registryKey(bare, cloneKey))?.values() ?? [])];
  }

  excluded(bare: string, cloneKey: string): boolean {
    return (this.entries.get(registryKey(bare, cloneKey))?.size ?? 0) > 0;
  }

  /** Registers `work` in `entry.pending`. Returns the never-rejecting wrapper whose settlement is tracked. */
  trackPending(entry: ExclusionEntry, work: Promise<unknown>): Promise<void> {
    const mark = (): void => { this.settledFlags.add(wrapper); };
    const wrapper: Promise<void> = work.then(mark, mark);
    entry.pending.add(wrapper);
    return wrapper;
  }

  /** True when every pending promise of `entry` has settled. */
  entrySettled(entry: ExclusionEntry): boolean {
    for (const p of entry.pending) if (!this.settledFlags.has(p)) return false;
    return true;
  }

  /** True when every pending promise of every entry of the key has settled (vacuously true without entries). */
  allPendingSettled(bare: string, cloneKey: string): boolean {
    return this.entriesOf(bare, cloneKey).every((e) => this.entrySettled(e));
  }

  /** Resolves when every pending promise that exists now, across the key's entries, has settled. */
  async settled(bare: string, cloneKey: string): Promise<void> {
    const all: Array<Promise<unknown>> = [];
    for (const e of this.entriesOf(bare, cloneKey)) for (const p of e.pending) all.push(p);
    await Promise.all(all);
  }

  addAdmission(bare: string, cloneKey: string, id: string, startedAt: number = Date.now()): void {
    const k = registryKey(bare, cloneKey);
    const m = this.admissions.get(k) ?? new Map<string, number>();
    m.set(id, startedAt);
    this.admissions.set(k, m);
  }

  removeAdmission(bare: string, cloneKey: string, id: string): void {
    const k = registryKey(bare, cloneKey);
    const m = this.admissions.get(k);
    if (!m) return;
    m.delete(id);
    if (m.size === 0) this.admissions.delete(k);
  }

  /** Age in ms of the oldest reclaim admission of the key, or undefined when there is none. */
  admissionAge(bare: string, cloneKey: string, now: number = Date.now()): number | undefined {
    const m = this.admissions.get(registryKey(bare, cloneKey));
    if (!m || m.size === 0) return undefined;
    return now - Math.min(...m.values());
  }
}

/** Rejects with `Error("deadline")` when `deadlineAt` passes before `p` settles. The timer is always cleared. */
export function withinDeadline<T>(p: Promise<T>, deadlineAt: number): Promise<T> {
  const remaining = deadlineAt - Date.now();
  if (remaining <= 0) { p.catch(() => undefined); return Promise.reject(new Error("deadline")); }
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error("deadline")), remaining); });
  return Promise.race([p, timeout]).finally(() => { if (timer) clearTimeout(timer); });
}

type ProcFs = Pick<typeof fs, "readlinkSync" | "readFileSync" | "readdirSync">;

/**
 * Whether the worker's PID namespace restart boundary is the container's own: PID 1 is `/sbin/tini`, or a
 * `docker-init` whose only child is `/sbin/tini`. Then a worker restart restarts the whole namespace and kills the
 * processes the in-memory exclusion would otherwise have to remember. Any read error means not scoped.
 */
export function detectNamespaceRestartBoundary(procRoot = "/proc", fsImpl: ProcFs = fs): { scoped: boolean; detail: string } {
  const TINI = "/sbin/tini";
  try {
    const exe1 = fsImpl.readlinkSync(path.join(procRoot, "1", "exe"));
    if (exe1 === TINI) return { scoped: true, detail: "pid 1 is /sbin/tini" };
    if (!exe1.endsWith("docker-init")) return { scoped: false, detail: `pid 1 is ${exe1}` };
    const children = childrenOfInit(procRoot, fsImpl);
    if (children.length !== 1) return { scoped: false, detail: `docker-init has ${children.length} children` };
    const childExe = fsImpl.readlinkSync(path.join(procRoot, String(children[0]), "exe"));
    return childExe === TINI
      ? { scoped: true, detail: "pid 1 is docker-init over /sbin/tini" }
      : { scoped: false, detail: `docker-init child is ${childExe}` };
  } catch (err) {
    return { scoped: false, detail: `unreadable: ${(err as NodeJS.ErrnoException).code ?? "error"}` };
  }
}

function childrenOfInit(procRoot: string, fsImpl: ProcFs): number[] {
  let listed: string | undefined;
  try { listed = String(fsImpl.readFileSync(path.join(procRoot, "1", "task", "1", "children"), "utf8")); }
  catch { listed = undefined; }
  if (listed !== undefined) return listed.split(/\s+/).filter((s) => /^[0-9]+$/.test(s)).map(Number);
  const out: number[] = [];
  for (const name of fsImpl.readdirSync(procRoot)) {
    if (!/^[0-9]+$/.test(name) || name === "1") continue;
    let stat: string;
    try { stat = String(fsImpl.readFileSync(path.join(procRoot, name, "stat"), "utf8")); }
    catch (err) {
      // Only a pid that vanished mid-census is skipped; any other failure leaves the census incomplete, so
      // the caller reports not scoped instead of counting a subset of init's children.
      const code = (err as NodeJS.ErrnoException).code;
      if (code === "ENOENT" || code === "ESRCH") continue;
      throw err;
    }
    const rest = stat.slice(stat.lastIndexOf(")") + 1).trim().split(/\s+/);
    if (rest[1] === "1") out.push(Number(name));
  }
  return out;
}
