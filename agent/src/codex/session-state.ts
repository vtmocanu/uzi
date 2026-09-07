// PRD #1171 (M3, plan §2.7 / item 7): credential-free session-state ownership.
//
// The M3a launcher (`launcher.ts`) hands every provider root a FRESH `CODEX_HOME`
// with no persistence. Pinned Codex writes its session/rollout transcript under
// `$CODEX_HOME/sessions/` and its AUTH material (login blob / tokens / cached model
// catalogs) elsewhere under `$CODEX_HOME` (e.g. `auth.json`). To resume a run across
// a checkpoint/park/shutdown root recreation, uzi preserves ONLY a proven
// CREDENTIAL-FREE session subset and re-authorizes the credential per root
// separately. This module owns that subset copy: a runner-owned per-run store that
// survives root reaping, seeded back into a fresh root before app-server start.
//
// ─── CRITICAL SAFETY RULE: ALLOWLIST, NEVER DENYLIST ────────────────────────────
// We copy ONLY an explicit allowlist of credential-free session artifacts, and NEVER
// auth material. Two independent gates a file MUST pass to be copied:
//
//   (1) it lives under the ONE allowed subtree, `SESSION_ALLOWED_SUBDIR` = "sessions"
//       (the rollout/transcript subtree). Nothing at the `$CODEX_HOME` root — where
//       `auth.json` and cached catalogs live — is ever even scanned.
//   (2) it is a rollout/transcript file by extension (`SESSION_ALLOWED_EXTENSIONS`,
//       `.jsonl`) AND its name carries NONE of the auth-shaped denied substrings
//       (`SESSION_DENY_NAME_SUBSTRINGS`: auth/token/credential/login/refresh/cache/
//       secret/key) AND its extension is not denied (`SESSION_DENY_EXTENSIONS`,
//       `.pem`). If a file's classification is uncertain, it is EXCLUDED — an
//       over-exclusion merely falls back to a fresh session, which is fail-safe;
//       leaking one auth byte is not.
//
// 🔴 PROVISIONAL: the exact pinned-Codex rollout file set is NOT fully characterized
// offline. The allowlist above is deliberately conservative and MUST be confirmed
// against the real app-server in the packaged integration (m3b:packaged) before this
// is trusted in production. Widen the allowlist only with a proven credential-free
// characterization; never widen it speculatively.
//
// ─── TRAVERSAL / SYMLINK SAFETY (openat2-style, fd-anchored) ────────────────────
// The copy walk is ANCHORED on held directory file descriptors on BOTH ends, never on
// re-resolved pathnames. Each level holds an open source dir fd and dest dir fd, and
// every child is opened relative to its parent fd via the Linux magic-symlink
// `/proc/self/fd/<fd>/<name>` with `O_NOFOLLOW` (userland `openat`). Because a held fd
// pins the directory INODE, a concurrent swap of an intermediate directory into a
// symlink between listing a level and descending it cannot redirect resolution out of
// either root — the guarantee the earlier lstat-then-readdir-by-pathname walk could not
// give (it re-opened each level by name, so a raced intermediate symlink escaped BOTH
// roots, and the source here is the UNTRUSTED codexHome). `O_NOFOLLOW` on every open
// additionally rejects a symlinked final component atomically (ELOOP). The TOP-LEVEL
// `sessions/` dir is opened by absolute path with `O_NOFOLLOW | O_DIRECTORY`: a
// symlinked or non-directory `sessions/` is refused (persist throws a static module
// error; adopt no-ops) rather than followed out to the untrusted codexHome root, while
// an ABSENT source yields an empty tally (the fresh-root "nothing to persist" contract).
// Each source file is copied through the OPEN fd — its size taken from an `fstat` on
// that pinned fd and AT MOST that many bytes read — so a file that grows after the stat
// can never be read unboundedly. The copy is bounded on every axis — a per-file byte
// cap, a total byte cap, a file-count cap AND a per-entry scan cap that charges EVERY
// entry examined (not just copied) — so a hostile tree stuffed with non-allowlisted
// entries cannot make the walk run unboundedly even when it copies nothing. A failed
// `persist` removes any partial dest copy before re-throwing, so a breach mid-copy
// leaves nothing a later `adopt`/`inspect` would treat as a resumable store.
//
// This module NEVER logs file contents or a token; diagnostics are static and
// bounded (an error names only which cap was hit, never a path or a byte).

import fsp from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import { constants as FS } from "node:fs";
import type { Stats } from "node:fs";
import { extname, isAbsolute, join, relative, resolve } from "node:path";

import type { SessionPresence } from "../harness.js";

/** The ONE credential-free subtree under `$CODEX_HOME` we ever read or seed. Auth
 *  material lives OUTSIDE it (at the `$CODEX_HOME` root), so scoping to this subdir
 *  is the first allowlist gate. */
export const SESSION_ALLOWED_SUBDIR = "sessions";

/** Allowed rollout/transcript file extensions. Pinned Codex writes rollouts as
 *  JSONL; anything else under `sessions/` is treated as uncertain and EXCLUDED.
 *  Provisional pending m3b:packaged confirmation (see file header). */
export const SESSION_ALLOWED_EXTENSIONS: readonly string[] = [".jsonl"];

/** Auth-shaped name substrings. A file OR directory whose name contains any of these
 *  is excluded even when it otherwise looks like a rollout.
 *
 *  This is a SECONDARY, belt-and-braces gate: the PRIMARY confinement is the
 *  `sessions/` subdir scope (now symlink-guarded on the top-level dir, see FIX 1) plus
 *  the `.jsonl` extension allowlist. The deny-list only adds depth so an `auth-*.jsonl`
 *  (or `access.jsonl`) accidentally written under `sessions/` is still never copied.
 *
 *  It matches ASCII-case-insensitively ONLY (`.toLowerCase()`): homoglyph / Unicode
 *  confusable names (e.g. a Cyrillic `а` in "аuth") are NOT normalized and would slip
 *  past this substring test. That is a DOCUMENTED residual of the PROVISIONAL
 *  content-trust model (see file header) — the primary allowlist, not this deny-list,
 *  is the load-bearing gate, and the real rollout name set must be characterized
 *  against the app-server in m3b:packaged before this is trusted in production. */
export const SESSION_DENY_NAME_SUBSTRINGS: readonly string[] = [
  "auth",
  "access",
  "token",
  "credential",
  "login",
  "refresh",
  "cache",
  "secret",
  "key",
];

/** Denied extensions, excluded regardless of the allowlist (defense in depth: `.pem`
 *  never passes the `.jsonl` allowlist anyway, but the explicit deny documents the
 *  intent from the plan's risk note). */
export const SESSION_DENY_EXTENSIONS: readonly string[] = [".pem"];

/** Bounded copy limits. Injected small in tests; production uses the defaults. */
export interface SessionStoreBounds {
  /** Maximum number of session files copied in one operation. */
  readonly maxFiles: number;
  /** Maximum total bytes copied in one operation. */
  readonly maxTotalBytes: number;
  /** Maximum size of any single copied file. */
  readonly maxFileBytes: number;
  /** Maximum number of directory ENTRIES the copy walk may examine in one operation,
   *  counting EVERY entry lstat'd (files, dirs, non-allowlisted junk), not just the
   *  ones copied. Bounds total work so an adversarial `sessions/` full of
   *  non-allowlisted entries cannot make the walk run unboundedly. Must comfortably
   *  exceed `maxFiles` so a legitimate full-size copy is never starved by the scan. */
  readonly maxScanEntries: number;
}

/** Conservative production caps. A real rollout subtree is a handful of JSONL files;
 *  these exist so a hostile/runaway tree fails closed rather than copying forever. */
export const DEFAULT_SESSION_STORE_BOUNDS: SessionStoreBounds = {
  maxFiles: 10_000,
  maxTotalBytes: 512 * 1024 * 1024, // 512 MiB
  maxFileBytes: 64 * 1024 * 1024, // 64 MiB
  maxScanEntries: 100_000, // 10× maxFiles: headroom for dirs on a legit full copy
};

/** Result of {@link CodexSessionStore.persist}. */
export interface PersistResult {
  readonly files: number;
  readonly bytes: number;
}

/** Result of {@link CodexSessionStore.adopt}. */
export interface AdoptResult {
  readonly files: number;
}

/** Thrown by `persist` when a bound is exceeded (fail-closed): the copy stops rather
 *  than persisting an unbounded subset. `adopt`/`inspect` are fail-SAFE instead and
 *  never surface this to the caller (they treat it as "adopt what fits" / "unknown").
 *  The message is static and names only the cap — never a path or byte content. */
export class CodexSessionStoreBoundError extends Error {
  constructor(
    readonly bound: keyof SessionStoreBounds,
    readonly limit: number,
  ) {
    super(`codex session store bound exceeded: ${bound} (limit ${limit})`);
    this.name = "CodexSessionStoreBoundError";
  }
}

/** The categories a {@link CodexSessionStoreError} can report. Each is a static token —
 *  NEVER a path, byte, or filesystem detail — so a thrown error names only WHAT failed,
 *  consistent with the "errors name only the cap, never a path" safety rule. */
export type CodexSessionStoreErrorCategory =
  /** The top-level `sessions/` source is a symlink or not a directory (FIX 1 refusal). */
  | "source-not-directory"
  /** A raw filesystem error escaped the copy; wrapped so its `.path` never propagates. */
  | "io-error";

/** Thrown by `persist` for a non-bound failure it must NOT leak details of. Raw `fs`
 *  errors carry a `.path` (the source/dest path), which contradicts the module's
 *  "errors name only the cap, never a path" rule — so `persist` wraps them in this
 *  static, path-free error. Distinct from {@link CodexSessionStoreBoundError}, whose
 *  fail-closed behavior is preserved (bound breaches are re-thrown as-is, not wrapped). */
export class CodexSessionStoreError extends Error {
  constructor(readonly category: CodexSessionStoreErrorCategory) {
    super(`codex session store error: ${category}`);
    this.name = "CodexSessionStoreError";
  }
}

// ─── Allowlist classification ───────────────────────────────────────────────────

function nameHasDenySubstring(name: string): boolean {
  const lower = name.toLowerCase();
  return SESSION_DENY_NAME_SUBSTRINGS.some((deny) => lower.includes(deny));
}

/** The load-bearing allowlist test for a FILE name: allowed extension, not a denied
 *  extension, and no auth-shaped name substring. Anything failing any clause is
 *  excluded (fail-safe → fresh session). */
function isAllowedSessionArtifact(name: string): boolean {
  const ext = extname(name).toLowerCase();
  if (!SESSION_ALLOWED_EXTENSIONS.includes(ext)) return false;
  if (SESSION_DENY_EXTENSIONS.includes(ext)) return false;
  if (nameHasDenySubstring(name)) return false;
  return true;
}

// ─── Confinement helpers ─────────────────────────────────────────────────────────

/** True iff `target` resolves to `root` itself or a descendant of it — the openat2
 *  "resolve within the given dir" confinement, applied to every path we touch. */
function isWithin(root: string, target: string): boolean {
  const rel = relative(root, target);
  return rel === "" || (!rel.startsWith("..") && !isAbsolute(rel));
}

async function tryLstat(path: string): Promise<Stats | undefined> {
  try {
    return await fsp.lstat(path);
  } catch {
    return undefined;
  }
}

interface CopyTally {
  files: number;
  bytes: number;
}

/** Open a single readdir CHILD `name` relative to an already-open parent directory
 *  handle, using the Linux magic-symlink `/proc/self/fd/<fd>/<name>`. Because the
 *  parent fd pins the directory INODE, a concurrent rename/swap of any ancestor path
 *  cannot redirect where `name` resolves — this is `openat(parent, name, …)` emulated
 *  in userland (the load-bearing anti-TOCTOU guard for the whole walk). `O_NOFOLLOW`
 *  in `flags` additionally makes `name` itself un-followable if it is a symlink.
 *
 *  `name` is always a single component straight from `readdir`, which never yields a
 *  separator or `.`/`..`; we still reject those defensively (fail closed) so a bug or a
 *  hostile dirent can never turn this into a multi-component or parent traversal. */
async function openAt(
  parent: FileHandle,
  name: string,
  flags: number,
  mode?: number,
): Promise<FileHandle> {
  if (
    name === "" ||
    name === "." ||
    name === ".." ||
    name.includes("/") ||
    name.includes("\\") ||
    name.includes("\0")
  ) {
    throw new Error("codex session store: refusing suspicious path component");
  }
  return fsp.open(`/proc/self/fd/${parent.fd}/${name}`, flags, mode);
}

/** Read AT MOST `size` bytes from an already-open file handle, against the PINNED fd
 *  (never re-opening by path). `size` comes from an `fstat` on that same fd, so a file
 *  that grows after the stat can never make this read past `size` — the read is bounded
 *  by the size we already accounted, not by the file's current length. Returns only the
 *  bytes actually read (a file that shrank yields fewer). */
async function readBounded(fh: FileHandle, size: number): Promise<Buffer> {
  const buf = Buffer.allocUnsafe(size);
  let offset = 0;
  while (offset < size) {
    const { bytesRead } = await fh.read(buf, offset, size - offset, offset);
    if (bytesRead === 0) break; // EOF: the file is shorter than the fstat size
    offset += bytesRead;
  }
  return offset === size ? buf : buf.subarray(0, offset);
}

/**
 * Recursively copy the allowlisted session artifacts from `srcSessionsDir` into
 * `destSessionsDir`, preserving relative structure. The walk is ANCHORED on held
 * directory file descriptors on BOTH ends: every child is resolved relative to its
 * parent fd via `/proc/self/fd/<fd>/<name>` (see {@link openAt}), and every open uses
 * `O_NOFOLLOW`. Because each level pins its directory inode, a concurrent swap of an
 * intermediate directory into a symlink between listing and descending it cannot
 * redirect resolution out of either root — the openat2 "resolve within the given dir"
 * guarantee the earlier pathname-based re-open (lstat-then-readdir-by-path) could not
 * give. Symlinks (files or dirs) are never followed; non-regular files, non-allowlisted
 * names, and auth-shaped (deny-substring) names/dirs are skipped; the copy is bounded
 * on every axis.
 *
 * `throwOnBound` distinguishes the two contracts: `persist` passes `true` (fail
 * CLOSED — a breach throws and the partial store is not trusted); `adopt` passes
 * `false` (fail SAFE — it copies what fits and stops).
 *
 * The top-level SOURCE `sessions/` is opened directly by absolute path with
 * `O_NOFOLLOW | O_DIRECTORY`: ENOENT is an ABSENT source (empty tally, preserving the
 * fresh-root "nothing to persist" contract), while ELOOP (a symlink) or ENOTDIR (not a
 * dir) is a refusal — `persist` throws {@link CodexSessionStoreError}, `adopt` returns
 * empty. Every entry examined charges the per-entry `visited` counter against
 * `bounds.maxScanEntries`, so an adversarial tree of non-allowlisted entries cannot
 * make the walk run unboundedly even copying zero.
 */
async function copyAllowlistedSessions(
  srcSessionsDir: string,
  destSessionsDir: string,
  bounds: SessionStoreBounds,
  throwOnBound: boolean,
): Promise<CopyTally> {
  const srcRoot = resolve(srcSessionsDir);
  const destRoot = resolve(destSessionsDir);
  const tally: CopyTally = { files: 0, bytes: 0 };
  let visited = 0;

  // Walk one directory level holding BOTH the source and dest directory fds. Returns
  // false to signal a fail-safe stop (a bound tripped with throwOnBound=false); throws
  // on a fail-closed bound breach.
  const walk = async (srcDirFh: FileHandle, destDirFh: FileHandle): Promise<boolean> => {
    let entries;
    try {
      // Listing the fd's own inode (via /proc/self/fd) keeps the read anchored: it
      // enumerates exactly the directory the fd pins, not a re-resolved pathname.
      entries = await fsp.readdir(`/proc/self/fd/${srcDirFh.fd}`, { withFileTypes: true });
    } catch {
      return true; // an unreadable subdir is skipped (fail-safe); the walk continues
    }
    for (const entry of entries) {
      // Charge EVERY entry examined, not just the ones copied, so a `sessions/` stuffed
      // with non-allowlisted junk cannot make this walk run unboundedly.
      visited += 1;
      if (visited > bounds.maxScanEntries) {
        if (throwOnBound) throw new CodexSessionStoreBoundError("maxScanEntries", bounds.maxScanEntries);
        return false; // fail-safe: stop the walk
      }

      const name = entry.name;

      if (entry.isDirectory()) {
        if (nameHasDenySubstring(name)) continue; // never descend an auth-shaped dir
        let srcChild: FileHandle;
        try {
          srcChild = await openAt(srcDirFh, name, FS.O_RDONLY | FS.O_NOFOLLOW | FS.O_DIRECTORY);
        } catch (error) {
          const code = (error as NodeJS.ErrnoException).code;
          if (code === "ELOOP" || code === "ENOTDIR") continue; // symlink / raced away: skip
          throw error;
        }
        try {
          // Create the dest child relative to the dest parent fd (openat-style), then
          // open it O_NOFOLLOW so we descend into a real, in-tree directory.
          try {
            await fsp.mkdir(`/proc/self/fd/${destDirFh.fd}/${name}`, { mode: 0o700 });
          } catch (error) {
            if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
          }
          const destChild = await openAt(
            destDirFh,
            name,
            FS.O_RDONLY | FS.O_NOFOLLOW | FS.O_DIRECTORY,
          );
          try {
            const proceed = await walk(srcChild, destChild);
            if (!proceed) return false;
          } finally {
            await destChild.close();
          }
        } finally {
          await srcChild.close();
        }
        continue;
      }

      if (!entry.isFile()) continue; // symlinks, sockets, fifos, devices excluded
      if (!isAllowedSessionArtifact(name)) continue;

      let srcFile: FileHandle;
      try {
        srcFile = await openAt(srcDirFh, name, FS.O_RDONLY | FS.O_NOFOLLOW);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ELOOP") continue; // raced into a symlink
        throw error;
      }
      try {
        // fstat on the PINNED fd: the size we bound and account is the size of the exact
        // inode we hold open, immune to a post-listing grow/swap of the pathname.
        const st = await srcFile.stat();
        if (!st.isFile()) continue; // not a regular file after all → skip
        if (st.size > bounds.maxFileBytes) {
          if (throwOnBound) throw new CodexSessionStoreBoundError("maxFileBytes", bounds.maxFileBytes);
          continue; // fail-safe: skip the oversized file
        }
        if (tally.files + 1 > bounds.maxFiles) {
          if (throwOnBound) throw new CodexSessionStoreBoundError("maxFiles", bounds.maxFiles);
          return false; // fail-safe: stop
        }
        if (tally.bytes + st.size > bounds.maxTotalBytes) {
          if (throwOnBound) throw new CodexSessionStoreBoundError("maxTotalBytes", bounds.maxTotalBytes);
          return false; // fail-safe: stop
        }

        // Bounded read of AT MOST st.size bytes from the fd (never readFile(), which
        // would read past st.size if the file grew after the fstat).
        const data = await readBounded(srcFile, st.size);

        const destFile = await openAt(
          destDirFh,
          name,
          FS.O_WRONLY | FS.O_CREAT | FS.O_TRUNC | FS.O_NOFOLLOW,
          0o600,
        );
        try {
          await destFile.writeFile(data);
        } finally {
          await destFile.close();
        }

        tally.files += 1;
        tally.bytes += data.length; // account the ACTUAL bytes copied
      } finally {
        await srcFile.close();
      }
    }
    return true;
  };

  // Open the TOP-LEVEL source sessions dir directly by absolute path, O_NOFOLLOW so a
  // symlinked `sessions/` is refused rather than followed out to the untrusted root.
  let srcTop: FileHandle;
  try {
    srcTop = await fsp.open(srcRoot, FS.O_RDONLY | FS.O_NOFOLLOW | FS.O_DIRECTORY);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") return tally; // absent source → nothing to persist (0 files)
    if (code === "ELOOP" || code === "ENOTDIR") {
      // Top-level sessions/ is a symlink or not a directory → refuse.
      if (throwOnBound) throw new CodexSessionStoreError("source-not-directory");
      return tally; // fail-safe: copy nothing
    }
    throw error; // unexpected (EACCES/…): let the caller wrap it
  }
  try {
    // Create + open the top-level DEST sessions dir, O_NOFOLLOW so we never write
    // THROUGH a symlinked dest either.
    await fsp.mkdir(destRoot, { recursive: true, mode: 0o700 });
    const destTop = await fsp.open(destRoot, FS.O_RDONLY | FS.O_NOFOLLOW | FS.O_DIRECTORY);
    try {
      await walk(srcTop, destTop);
    } finally {
      await destTop.close();
    }
  } finally {
    await srcTop.close();
  }

  return tally;
}

/** Scan for the FIRST allowlisted artifact under `sessionsDir`, bounded by
 *  `scanCap` entries so a pathological tree cannot make `inspect` walk forever. A
 *  breach throws {@link CodexSessionStoreBoundError}, which `inspect` maps to the
 *  tri-state "unknown". */
async function hasAllowlistedArtifact(sessionsDir: string, scanCap: number): Promise<boolean> {
  const root = resolve(sessionsDir);
  let visited = 0;

  const walk = async (relDir: string): Promise<boolean> => {
    const entries = await fsp.readdir(join(root, relDir), { withFileTypes: true });
    for (const entry of entries) {
      if (++visited > scanCap) throw new CodexSessionStoreBoundError("maxFiles", scanCap);
      const relPath = relDir === "" ? entry.name : `${relDir}/${entry.name}`;
      const abs = join(root, relPath);
      if (!isWithin(root, abs)) continue;
      const st = await tryLstat(abs);
      if (!st || st.isSymbolicLink()) continue;
      if (st.isDirectory()) {
        if (nameHasDenySubstring(entry.name)) continue;
        if (await walk(relPath)) return true;
        continue;
      }
      if (st.isFile() && isAllowedSessionArtifact(entry.name)) return true;
    }
    return false;
  };

  return walk("");
}

// ─── Public API ─────────────────────────────────────────────────────────────────

/**
 * The credential-free Codex session store. Every method takes explicit directory
 * paths so it is unit-testable against temp dirs with no real Codex.
 *
 *   - `persist` copies the credential-free session subset OUT of a live root's
 *     `$CODEX_HOME` into the runner-owned per-run store (survives root reaping).
 *   - `adopt` seeds a fresh root's `$CODEX_HOME/sessions/` from that store.
 *   - `inspect` reports whether the store holds a resumable session (tri-state).
 *   - `remove` deletes the store at the terminal boundary (credential-free, always
 *     removable).
 *
 * The store is per-worker-local and is NEVER serialized through any API/checkpoint:
 * a different worker has no store, so `inspect` returns "absent" and the harness
 * starts fresh over the recovered worktree (a deliberate non-goal, per the plan).
 */
export const CodexSessionStore = {
  /**
   * Copy the credential-free session subset from `codexHome` into `storeDir`
   * (created mode 0700). The store's `sessions/` copy is refreshed (cleared first),
   * so a stale rollout never lingers. Only the allowlist is copied; symlinks are
   * rejected; the copy is bounded and throws {@link CodexSessionStoreBoundError} on
   * a breach. `opts.bounds` overrides the default caps (used by tests).
   *
   * Atomic-ish on failure: if the copy throws mid-way (a bound breach or an I/O
   * error), the partial `destSessions` subtree is removed before the error propagates,
   * so a failed persist leaves nothing a later `inspect`/`adopt` would treat as a
   * resumable store.
   */
  async persist(
    codexHome: string,
    storeDir: string,
    opts: { bounds?: SessionStoreBounds } = {},
  ): Promise<PersistResult> {
    const bounds = opts.bounds ?? DEFAULT_SESSION_STORE_BOUNDS;
    const srcSessions = join(codexHome, SESSION_ALLOWED_SUBDIR);
    const destSessions = join(storeDir, SESSION_ALLOWED_SUBDIR);
    try {
      await fsp.mkdir(storeDir, { recursive: true, mode: 0o700 });
      await fsp.chmod(storeDir, 0o700);

      // Refresh: clear any prior copy so the store never holds a stale rollout set.
      await fsp.rm(destSessions, { recursive: true, force: true });

      const tally = await copyAllowlistedSessions(srcSessions, destSessions, bounds, true);
      return { files: tally.files, bytes: tally.bytes };
    } catch (error) {
      // FIX 3 (atomic-ish persist): a bound breach or I/O error can throw AFTER some
      // files were already copied, leaving a PARTIAL `destSessions` that a later
      // `inspect()` would report "present" and `adopt()` would resume. Remove it before
      // re-throwing so a failed persist leaves nothing adoptable. Swallow any cleanup
      // error so it never masks the original failure.
      await fsp.rm(destSessions, { recursive: true, force: true }).catch(() => {});

      // Bound breaches keep their fail-closed contract, and the top-level source refusal
      // is already a static, path-free error — re-throw both as-is. Anything else is a
      // raw `fs` error whose `.path` names a filesystem path; wrap it in a static module
      // error so persist never propagates a path (see the module's safety rule).
      if (
        error instanceof CodexSessionStoreBoundError ||
        error instanceof CodexSessionStoreError
      ) {
        throw error;
      }
      throw new CodexSessionStoreError("io-error");
    }
  },

  /**
   * Seed `codexHome/sessions/` from `storeDir` BEFORE app-server start. Missing or
   * corrupt store → no-op (0 files), NEVER throws (fail-safe → fresh session). This
   * is also the cross-worker path: a worker with no store adopts nothing.
   */
  async adopt(storeDir: string, codexHome: string): Promise<AdoptResult> {
    try {
      const srcSessions = join(storeDir, SESSION_ALLOWED_SUBDIR);
      const st = await tryLstat(srcSessions);
      // Absent, a symlink, or not a directory → treat as no store (fresh session).
      if (!st || st.isSymbolicLink() || !st.isDirectory()) return { files: 0 };

      const destSessions = join(codexHome, SESSION_ALLOWED_SUBDIR);
      // FIX 3 (dest-side twin of FIX 1): never write THROUGH a symlinked (or non-dir)
      // dest `sessions/`. `mkdir(..., {recursive})` FOLLOWS a symlink, so a planted
      // `codexHome/sessions -> /elsewhere` would land the copy out of the fresh root.
      // If the dest exists as a symlink or a non-directory, remove that entry first
      // (rm on a symlink unlinks only the link, never its target) so we then create a
      // real, in-tree directory. A legit existing directory is left untouched.
      const destStat = await tryLstat(destSessions);
      if (destStat && (destStat.isSymbolicLink() || !destStat.isDirectory())) {
        await fsp.rm(destSessions, { recursive: true, force: true });
      }
      await fsp.mkdir(destSessions, { recursive: true, mode: 0o700 });
      const tally = await copyAllowlistedSessions(
        srcSessions,
        destSessions,
        DEFAULT_SESSION_STORE_BOUNDS,
        false, // fail-safe: adopt what fits, never throw
      );
      return { files: tally.files };
    } catch {
      // Corrupt/unexpected I/O → no-op. Adoption is best-effort; a fresh session is
      // always a safe fallback.
      return { files: 0 };
    }
  },

  /**
   * Tri-state presence of a resumable session in the store, mirroring
   * {@link SessionPresence}: "present" when the store holds ≥1 session artifact,
   * "absent" when the store is missing/empty/corrupt (fail-safe → fresh session),
   * "unknown" only on a genuine I/O uncertainty (e.g. EACCES, or a tree too large to
   * scan within bounds).
   */
  async inspect(storeDir: string): Promise<SessionPresence> {
    const srcSessions = join(storeDir, SESSION_ALLOWED_SUBDIR);
    try {
      const st = await fsp.lstat(srcSessions);
      if (st.isSymbolicLink() || !st.isDirectory()) return "absent"; // corrupt → absent
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === "ENOENT" || code === "ENOTDIR") return "absent";
      return "unknown"; // EACCES/ELOOP/… — genuinely cannot tell
    }
    try {
      const found = await hasAllowlistedArtifact(srcSessions, DEFAULT_SESSION_STORE_BOUNDS.maxFiles);
      return found ? "present" : "absent";
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === "ENOENT" || code === "ENOTDIR") return "absent";
      return "unknown"; // bound breach or unexpected I/O → uncertainty
    }
  },

  /**
   * Delete the store at the terminal boundary. The session subset is credential-free,
   * so it is removed unconditionally. Idempotent: a missing store is a no-op
   * (`force` swallows ENOENT).
   */
  async remove(storeDir: string): Promise<void> {
    await fsp.rm(storeDir, { recursive: true, force: true });
  },
};
