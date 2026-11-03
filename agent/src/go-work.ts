// Managed Go work dir for a Claude run (issue #3870): `<runHome>/.uzi-go-work/g<claimGeneration>`.
//
// The launch points `GOTMPDIR` at one generation's leaf so Go's temporary build trees (`go-build*`,
// `go-link-*`) live in a place the worker can find and remove as a unit when the run parks, instead of
// in `/tmp` or the build cache. This module is only the primitives: preparing a leaf, removing leaves,
// and the leaf-name grammar. Launch wiring and park routing live elsewhere.
//
// Boundary, stated plainly. `.uzi-go-work` is a disposable scratch root, NOT cross-run isolation. A run
// HOME is group-writable and not sticky, so any same-uid process (this run's, or another run's on the
// same worker, which shares the `runner` uids) can rename or replace the parent or a leaf. That is the
// existing shared-UID posture and is accepted here; what this module guarantees is narrower: each step
// is relative to a pinned directory descriptor (HOME, then the parent), so a swap of an ancestor path
// cannot redirect a later create or open outside the pinned directory; a directory this call created is
// verified fresh on its reopened descriptor before the only chmod (a swap in that window is refused, not
// adopted); an existing parent or leaf with the wrong shape is refused rather than repaired (never
// chown, never recursive chmod); and no function here throws.

import fs from "node:fs/promises";
import { constants as fsConstants } from "node:fs";
import path from "node:path";
import type { Logger } from "./log.js";
import { sanitizeForLog } from "./run-quiescence.js";
import { RUNNER_UID, uidSplitActive } from "./runner-uid.js";
import { isParkScratchSubtree, rmHomeSubtree } from "./rmtree.js";

/** The parent directory name inside a run HOME. */
export const GO_WORK_PARENT = ".uzi-go-work";

/** A leaf name: `g` plus a positive decimal generation of at most 16 digits (also a safe integer). */
const LEAF_RE = /^g([1-9][0-9]{0,15})$/;

/** The generation a leaf name encodes, or undefined when `name` is not a grammar-valid leaf. */
export function leafGeneration(name: string): number | undefined {
  const m = LEAF_RE.exec(name);
  if (m === null) return undefined;
  const n = Number(m[1]);
  return Number.isSafeInteger(n) ? n : undefined;
}

/** The leaf name for a claim generation (a positive safe integer). */
export function goWorkLeafName(generation: number): string {
  if (!Number.isSafeInteger(generation) || generation <= 0) {
    throw new Error(`goWorkLeafName: not a positive safe integer: ${String(generation)}`);
  }
  return `g${generation}`;
}

/** Why a managed Go work dir was not provided; a fixed vocabulary, safe to log. */
export type GoWorkSkipReason =
  | "no_generation"
  | "home_unavailable"
  | "parent_create_failed"
  | "parent_refused"
  | "leaf_exists"
  | "leaf_create_failed"
  | "leaf_refused";

export type GoWorkPrepared = { dir: string } | { skipped: true; reason: GoWorkSkipReason };

/** Test-only overrides so a test can inject a failure at one syscall. Production passes none. */
export interface GoWorkFsOps {
  mkdir?: (target: string, options: { mode: number }) => Promise<unknown>;
  open?: (target: string, flags: number) => Promise<import("node:fs/promises").FileHandle>;
  chmod?: (handle: import("node:fs/promises").FileHandle, mode: number) => Promise<void>;
}

const DIR_FLAGS = fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW;
const SPLIT_MODE = 0o2770;
const SINGLE_MODE = 0o700;

const fdPath = (fd: number, name?: string): string => (name === undefined ? `/proc/self/fd/${fd}` : `/proc/self/fd/${fd}/${name}`);

const errCode = (err: unknown): string | undefined => {
  const code = (err as NodeJS.ErrnoException | undefined)?.code;
  return typeof code === "string" ? sanitizeForLog(code, 32) : undefined;
};

type Handle = import("node:fs/promises").FileHandle;

/** The directory shape a managed parent or leaf must have: ours, and group-shared under the split. */
async function hasManagedShape(handle: Handle): Promise<boolean> {
  const st = await handle.stat();
  if (!st.isDirectory() || st.uid !== process.getuid?.()) return false;
  if (uidSplitActive()) return st.gid === RUNNER_UID && (st.mode & 0o7777) === SPLIT_MODE;
  return (st.mode & 0o777) === SINGLE_MODE;
}

class Skip extends Error {
  constructor(
    readonly reason: GoWorkSkipReason,
    readonly code?: string,
  ) {
    super(reason);
  }
}

/**
 * Create the managed Go work leaf for `generation` under `home` and return its path, or report why
 * there is none. Never throws: every failure (and an unusable generation) is a `skipped` result with
 * one bounded warning, and the caller launches without `GOTMPDIR`.
 *
 * Steps run through descriptors: HOME is opened `O_DIRECTORY|O_NOFOLLOW` and held, the parent is created
 * (non-recursive, mode 2770 under the uid split, 0700 otherwise) and opened below it, and the leaf is
 * created non-recursively below the pinned parent. mkdir and the reopen are by name, so a directory
 * this call created is chmodded only after the reopened descriptor passes a freshness check (ours,
 * empty, nlink 2, no mode bits beyond the requested ones); a swapped-in directory is refused untouched.
 * An existing parent with the wrong owner, group or mode is refused, never repaired. An existing leaf
 * is a refusal too (`leaf_exists`): a generation's leaf is made once.
 */
export async function prepareManagedGoWork(
  home: string,
  generation: number | undefined,
  log: Logger,
  fsOps: GoWorkFsOps = {},
): Promise<GoWorkPrepared> {
  const skip = (reason: GoWorkSkipReason, code?: string): GoWorkPrepared => {
    log.warn("managed Go work dir unavailable; launching without GOTMPDIR", {
      reason,
      generation: typeof generation === "number" && Number.isFinite(generation) ? generation : null,
      ...(code !== undefined ? { code } : {}),
    });
    return { skipped: true, reason };
  };
  if (generation === undefined || !Number.isSafeInteger(generation) || generation <= 0) return skip("no_generation");
  const open = fsOps.open ?? ((p: string, flags: number) => fs.open(p, flags));
  const mkdir = fsOps.mkdir ?? ((p: string, o: { mode: number }) => fs.mkdir(p, o));
  const chmod = fsOps.chmod ?? ((h: Handle, mode: number) => h.chmod(mode));
  const mode = uidSplitActive() ? SPLIT_MODE : SINGLE_MODE;
  const opened: Handle[] = [];
  const pin = async (target: string, onFail: GoWorkSkipReason): Promise<Handle> => {
    let h: Handle;
    try {
      h = await open(target, DIR_FLAGS);
    } catch (err) {
      throw new Skip(onFail, errCode(err));
    }
    opened.push(h);
    return h;
  };
  // Create `name` below the pinned `parent`; returns whether this call created it.
  const create = async (parent: Handle, name: string, onFail: GoWorkSkipReason, onExists?: GoWorkSkipReason): Promise<boolean> => {
    try {
      await mkdir(fdPath(parent.fd, name), { mode });
      return true;
    } catch (err) {
      if (errCode(err) === "EEXIST") {
        if (onExists !== undefined) throw new Skip(onExists, "EEXIST");
        return false;
      }
      throw new Skip(onFail, errCode(err));
    }
  };
  const settle = async (handle: Handle, created: boolean, onFail: GoWorkSkipReason, onRefuse: GoWorkSkipReason) => {
    if (created) {
      // The open is by name, so a same-uid process could have swapped another directory into place
      // between our mkdir and the open. Before the fd is chmodded, require it to look like what our
      // mkdir just made: ours, a directory, empty, no hard-link extras, and no mode bits beyond the
      // requested ones (setgid may be inherited from the parent) with the owner rwx bits present.
      let fresh: boolean;
      try {
        const st = await handle.stat();
        fresh =
          st.isDirectory() &&
          st.uid === process.getuid?.() &&
          st.nlink === 2 &&
          (st.mode & 0o700) === 0o700 &&
          (st.mode & ~(mode | 0o2000) & 0o7777) === 0 &&
          (await fs.readdir(fdPath(handle.fd))).length === 0;
      } catch (err) {
        throw new Skip(onRefuse, errCode(err));
      }
      if (!fresh) throw new Skip(onRefuse);
      try {
        await chmod(handle, mode);
      } catch (err) {
        throw new Skip(onFail, errCode(err));
      }
    }
    let ok: boolean;
    try {
      ok = await hasManagedShape(handle);
    } catch (err) {
      throw new Skip(onRefuse, errCode(err));
    }
    if (!ok) throw new Skip(onRefuse);
  };
  try {
    if (!path.isAbsolute(home)) return skip("home_unavailable");
    const homeHandle = await pin(home, "home_unavailable");
    const parentCreated = await create(homeHandle, GO_WORK_PARENT, "parent_create_failed");
    const parent = await pin(fdPath(homeHandle.fd, GO_WORK_PARENT), "parent_refused");
    await settle(parent, parentCreated, "parent_create_failed", "parent_refused");
    const leafName = goWorkLeafName(generation);
    await create(parent, leafName, "leaf_create_failed", "leaf_exists");
    const leaf = await pin(fdPath(parent.fd, leafName), "leaf_refused");
    await settle(leaf, true, "leaf_create_failed", "leaf_refused");
    return { dir: path.join(home, GO_WORK_PARENT, leafName) };
  } catch (err) {
    if (err instanceof Skip) return skip(err.reason, err.code);
    return skip("home_unavailable", errCode(err));
  } finally {
    for (const h of opened) await h.close().catch(() => undefined);
  }
}

/** Which leaves a removal pass selects. */
export type GoWorkSelect = { kind: "all" } | { kind: "olderThan"; generation: number };

/** What a removal pass did. Names are grammar-valid leaf names. */
export interface GoWorkRemoval {
  /** `absent`: no HOME or no parent; `refused`: the parent was not safe or readable to enumerate. */
  parent: "absent" | "ok" | "refused";
  removed: string[];
  absent: string[];
  failed: string[];
  /** Selected leaves not attempted because the deadline passed. */
  skipped: string[];
  /** The enumeration stopped at `maxEntries`; later entries were not looked at. */
  truncated: boolean;
}

export interface GoWorkRemovalOptions {
  select: GoWorkSelect;
  /** Epoch ms; no leaf removal starts at or after it. */
  deadline: number;
  /** A short fixed label for the log line (why the removal ran). */
  reason: string;
  /** Directory entries one pass reads at most (default {@link DEFAULT_MAX_ENTRIES}). */
  maxEntries?: number;
  /** Test seam: defaults to {@link rmHomeSubtree}. */
  removeSubtree?: typeof rmHomeSubtree;
  /** Test seam: defaults to `fs.opendir`; lets a test fail the enumeration mid-stream. */
  opendir?: (target: string) => Promise<AsyncIterable<{ name: string }> & { close(): Promise<void> }>;
}

const DEFAULT_MAX_ENTRIES = 10_000;
const MAX_LOGGED_NAMES = 8;

/**
 * Remove the selected leaves under `<home>/.uzi-go-work`, never the parent itself. Never throws. Only
 * grammar-valid names are ever selected (`olderThan N` keeps generation N and newer), and each removal
 * goes through {@link rmHomeSubtree}, which pins and walks the leaf by descriptor. The parent is opened
 * pinned under a pinned HOME and enumerated as a stream bounded by `maxEntries`.
 */
export async function removeGoWorkLeaves(home: string, log: Logger, opts: GoWorkRemovalOptions): Promise<GoWorkRemoval> {
  const result: GoWorkRemoval = { parent: "absent", removed: [], absent: [], failed: [], skipped: [], truncated: false };
  try {
    if (opts.select.kind === "olderThan" && !(Number.isSafeInteger(opts.select.generation) && opts.select.generation > 0)) {
      // A bad cutoff would select nothing and look clean; refuse instead of deleting or reporting success.
      result.parent = "refused";
      log.warn("managed Go work dir cleanup refused: invalid olderThan generation", { reason: sanitizeForLog(opts.reason, 64) });
      return result;
    }
    await removeInto(home, opts, result);
  } catch {
    // Defensive: nothing below is expected to throw; an unexpected error is not a clean result.
    result.parent = "refused";
  }
  return finish(log, opts, result);
}

async function removeInto(home: string, opts: GoWorkRemovalOptions, result: GoWorkRemoval): Promise<void> {
  const removeSubtree = opts.removeSubtree ?? rmHomeSubtree;
  const maxEntries = opts.maxEntries ?? DEFAULT_MAX_ENTRIES;
  const selected: Array<[number, string]> = [];
  const opened: Handle[] = [];
  try {
    if (!path.isAbsolute(home)) {
      result.parent = "refused";
      return;
    }
    const open = async (target: string): Promise<Handle | "absent"> => {
      try {
        const h = await fs.open(target, DIR_FLAGS);
        opened.push(h);
        return h;
      } catch (err) {
        if (errCode(err) === "ENOENT") return "absent";
        throw err;
      }
    };
    const homeHandle = await open(home);
    const parent = homeHandle === "absent" ? "absent" : await open(fdPath(homeHandle.fd, GO_WORK_PARENT));
    if (parent === "absent") return;
    if (!(await hasManagedShape(parent))) {
      result.parent = "refused";
      return;
    }
    result.parent = "ok";
    // Streamed: entries are consumed one at a time and only selected names are kept.
    const dir = await (opts.opendir ?? ((p: string) => fs.opendir(p)))(fdPath(parent.fd));
    let seen = 0;
    try {
      for await (const ent of dir) {
        if (seen >= maxEntries) {
          result.truncated = true;
          break;
        }
        seen++;
        const gen = leafGeneration(ent.name);
        if (gen === undefined) continue;
        if (opts.select.kind === "olderThan" && !(gen < opts.select.generation)) continue;
        selected.push([gen, ent.name]);
      }
    } finally {
      await dir.close().catch(() => undefined);
    }
  } catch {
    // Anything unreadable or unexpected while opening or enumerating the parent: not safe to treat as clean.
    result.parent = "refused";
    return;
  } finally {
    for (const h of opened) await h.close().catch(() => undefined);
  }

  selected.sort((a, b) => a[0] - b[0]);
  for (const [, leaf] of selected) {
    if (Date.now() >= opts.deadline) {
      result.skipped.push(leaf);
      continue;
    }
    const rel = `${GO_WORK_PARENT}/${leaf}`;
    if (!isParkScratchSubtree(rel)) {
      result.failed.push(leaf);
      continue;
    }
    try {
      const outcome = await removeSubtree(home, rel, { deadline: opts.deadline });
      (outcome === "removed" ? result.removed : result.absent).push(leaf);
    } catch {
      result.failed.push(leaf);
    }
  }
}

function finish(log: Logger, opts: GoWorkRemovalOptions, r: GoWorkRemoval): GoWorkRemoval {
  const names = (list: string[]): string[] => list.slice(0, MAX_LOGGED_NAMES);
  const fields = {
    reason: sanitizeForLog(opts.reason, 64),
    select: opts.select.kind === "all" ? "all" : `olderThan:${opts.select.generation}`,
    parent: r.parent,
    selected: r.removed.length + r.absent.length + r.failed.length + r.skipped.length,
    removed: r.removed.length,
    absent: r.absent.length,
    failed: r.failed.length,
    skipped: r.skipped.length,
    truncated: r.truncated,
    removedNames: names(r.removed),
    failedNames: names(r.failed),
    skippedNames: names(r.skipped),
  };
  if (isGoWorkClean(r)) log.info("managed Go work dir cleanup", fields);
  else log.warn("managed Go work dir cleanup incomplete", fields);
  return r;
}

/**
 * True only when nothing selected is known to remain: the parent is absent, or it was enumerated
 * completely and every selected leaf was removed or already gone. An unsafe parent, a truncated
 * enumeration, a failed removal or a leaf skipped for the deadline is not clean.
 */
export function isGoWorkClean(r: GoWorkRemoval): boolean {
  if (r.parent === "absent") return true;
  return r.parent === "ok" && !r.truncated && r.failed.length === 0 && r.skipped.length === 0;
}
