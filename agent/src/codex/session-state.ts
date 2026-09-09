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
// cap, a total byte cap, a file-count cap, a per-entry scan cap that charges EVERY entry
// examined (not just copied), AND a recursion-DEPTH cap — so a hostile tree cannot make
// the walk run unboundedly even when it copies nothing. The depth cap is load-bearing
// beyond total work: the fd-anchored walk holds an open source AND dest directory fd at
// EVERY level simultaneously (~2·depth open fds), so a deep single-child source chain in
// the untrusted codexHome would otherwise exhaust the process fd table (EMFILE, ≈ depth
// 510 at the default 1024 fd limit) long before the per-entry scan cap — which bounds
// total entries but NOT depth — is reached; the depth cap bounds the simultaneous-fd
// count directly.
//
// ─── PUBLICATION MODEL: immutable generations + one atomically-renamed pointer ──
// The store is NOT a single mutable `sessions/` dir. `persist` writes each fresh copy into its
// OWN immutable generation directory `generations/<genId>/sessions/**` (a private, unique
// `<genId>` this module mints — never model or path input, validated against a closed grammar
// {@link GEN_ID_RE}), and publishes it by writing the winning `<genId>` into a small plain-text
// pointer file `current` by renaming a fresh temp file ONTO it ({@link publishPointer}). The
// pointer is REPLACED by one same-parent atomic rename — it is never unlinked — so a concurrent
// `inspect`/`adopt`, or a crashed PROCESS, always sees `current` naming a complete generation,
// never an absent or half-written one. NO symlink is used as the pointer. This is process-crash
// atomic VISIBILITY, NOT whole-system power-loss durability: no data is fsync'd, so a store lost
// to power loss simply fails safe to a fresh session.
//
// `persist` never touches the previous generation or the pointer before that final rename, so
// ANY failure (a bound breach, an I/O error, a copy refusal) leaves the previous good generation
// still named by `current` and fully discoverable; the incomplete new generation is a
// never-pointed orphan, best-effort removed on the failing call and otherwise reaped later. The
// previous good store is never moved aside, so a double failure can never strand it.
//
// RECLAMATION is reader-safe by FULL SERIALIZATION. The store is per-run / per-worker-local and
// never shared across processes, so an in-process discipline suffices: EVERY operation — persist,
// adopt, inspect and remove — runs under one per-storeDir lock ({@link withStoreLock}), from the
// root/pointer open through completion. So a reader holds the lock across reading `current` and
// opening + copying its generation, and no persist (hence no {@link reapGenerations}, which keeps
// only the new + previous generation) can run in between — there is no read→open window to race
// and no lease bookkeeping. Two persists likewise never overlap. `.current.tmp.*` residue from a
// crashed publish is swept on the next persist ({@link sweepStaleTemps}).
//
// The transaction layer is fd-anchored, and ANCESTOR-safe: the trusted `storeDir` and the
// UNTRUSTED `codexHome` are opened by walking EVERY path component from `/` with
// `O_DIRECTORY | O_NOFOLLOW` ({@link resolveDir}) — a symlinked ancestor at ANY level is refused,
// not just a symlinked final component — and every generation/pointer operation runs relative to
// the held dirfd via `/proc/self/fd/<fd>/<name>`. Only a validated FINAL component is ever created
// (relative to a pinned parent), never a pathname `mkdir -p`. FRESHNESS: a persist that captures
// ZERO artifacts (absent or empty source) still publishes an EMPTY generation and advances
// `current`, so `inspect` becomes absent and a stale session cannot resume — a genuinely
// UNREADABLE `current` (EACCES) instead fails persist closed rather than reap.
//
// This module NEVER logs file contents or a token; diagnostics are static and
// bounded (an error names only which cap was hit, never a path or a byte).

import fsp from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import { constants as FS } from "node:fs";
import type { Stats } from "node:fs";
import { extname, isAbsolute, join, relative, resolve } from "node:path";
import { randomBytes } from "node:crypto";

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
  /** Maximum directory-recursion DEPTH of the copy walk (top-level `sessions/` is depth
   *  0; its immediate subdirs are depth 1, and so on). OPTIONAL: when undefined the walk
   *  uses {@link DEFAULT_SESSION_STORE_MAX_DEPTH}, so existing bounds objects need no
   *  change. Unlike `maxScanEntries` (which bounds total entries but NOT depth), this
   *  bounds the number of directory fds held OPEN simultaneously (~2·depth, one source +
   *  one dest per level) so a deep single-child chain in the untrusted source cannot
   *  exhaust the process fd table (EMFILE). A real rollout tree is only a few levels. */
  readonly maxDepth?: number;
}

/** Module-private default recursion-DEPTH cap, used by the copy walk when
 *  `bounds.maxDepth` is undefined. A real rollout tree is only a few levels deep, so 64
 *  is generous while still bounding the ~2·depth directory fds the fd-anchored walk holds
 *  open simultaneously — a deep single-child chain in the untrusted source can no longer
 *  recurse toward the process fd-table limit (EMFILE ≈ depth 510 at the default 1024). */
const DEFAULT_SESSION_STORE_MAX_DEPTH = 64;

/** Conservative production caps. A real rollout subtree is a handful of JSONL files;
 *  these exist so a hostile/runaway tree fails closed rather than copying forever. */
export const DEFAULT_SESSION_STORE_BOUNDS: SessionStoreBounds = {
  maxFiles: 10_000,
  maxTotalBytes: 512 * 1024 * 1024, // 512 MiB
  maxFileBytes: 64 * 1024 * 1024, // 64 MiB
  maxScanEntries: 100_000, // 10× maxFiles: headroom for dirs on a legit full copy
  maxDepth: DEFAULT_SESSION_STORE_MAX_DEPTH, // caps simultaneous dir fds (~2·depth)
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
 * `bounds.maxScanEntries`, and every descent charges the `depth` argument against
 * `bounds.maxDepth` (default {@link DEFAULT_SESSION_STORE_MAX_DEPTH}) BEFORE the child
 * directory fd is opened, so neither an adversarial wide tree of non-allowlisted entries
 * nor a deep single-child chain (which would hold ~2·depth source+dest dir fds open
 * across the recursion and exhaust the process fd table) can make the walk run
 * unboundedly even copying zero — the walk is bounded on every axis, entries AND depth.
 */
async function copyAllowlistedSessions(
  srcSessionsDir: string,
  destSessionsDir: string,
  bounds: SessionStoreBounds,
  throwOnBound: boolean,
): Promise<CopyTally> {
  const srcRoot = resolve(srcSessionsDir);
  const destRoot = resolve(destSessionsDir);
  const maxDepth = bounds.maxDepth ?? DEFAULT_SESSION_STORE_MAX_DEPTH;
  const tally: CopyTally = { files: 0, bytes: 0 };
  let visited = 0;

  // Walk one directory level holding BOTH the source and dest directory fds. `depth` is
  // the current recursion depth (top-level `sessions/` is 0). Returns false to signal a
  // fail-safe stop (a bound tripped with throwOnBound=false); throws on a fail-closed
  // bound breach.
  const walk = async (
    srcDirFh: FileHandle,
    destDirFh: FileHandle,
    depth: number,
  ): Promise<boolean> => {
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
        // Depth cap BEFORE opening the child dir fd, so we never open even one fd past the
        // limit. The fd-anchored walk holds a source AND dest dir fd at every level
        // simultaneously (~2·depth), so an unbounded-depth chain in the untrusted source
        // would exhaust the process fd table (EMFILE); cap it on the same fail-closed /
        // fail-safe split every other bound uses.
        if (depth + 1 > maxDepth) {
          if (throwOnBound) throw new CodexSessionStoreBoundError("maxDepth", maxDepth);
          return false; // fail-safe: stop the walk
        }
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
            const proceed = await walk(srcChild, destChild, depth + 1);
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
      await walk(srcTop, destTop, 0);
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

// ─── Generation + pointer publication (fd-anchored transaction layer) ───────────

/** Subdirectory under `storeDir` holding the immutable per-persist generation dirs. */
const GENERATIONS_SUBDIR = "generations";
/** Plain-text pointer file naming the current generation (NEVER a symlink). */
const CURRENT_POINTER = "current";
/** Closed grammar for a generation id: `g` + 24 lowercase hex. This module MINTS the id (never
 *  model/path input); it is validated on every read so a corrupt or planted pointer resolves to
 *  "no current generation" (fail-safe → fresh session). */
const GEN_ID_RE = /^g[0-9a-f]{24}$/;
/** The pointer holds a single short id; anything larger is treated as corrupt. */
const POINTER_MAX_BYTES = 64;
/** Temp-file prefix for the atomic pointer publish (same-parent rename source). */
const POINTER_TMP_PREFIX = ".current.tmp.";

function freshGenId(): string {
  return `g${randomBytes(12).toString("hex")}`;
}
function isValidGenId(id: string): boolean {
  return GEN_ID_RE.test(id);
}

async function closeQuietly(fh: FileHandle | undefined): Promise<void> {
  if (fh) await fh.close().catch(() => {});
}

/** Split an absolute path into its non-empty components (`resolve` first drops `.`/`..`). */
function pathComponents(absPath: string): string[] {
  return resolve(absPath).split("/").filter((p) => p.length > 0);
}

/** Open an absolute directory by walking EVERY component from `/` with `O_DIRECTORY | O_NOFOLLOW`,
 *  so a symlinked ancestor at ANY level is refused (ELOOP) — the userland openat2
 *  `RESOLVE_NO_SYMLINKS`. Every component must already exist as a real directory; nothing is
 *  created or followed. Used to pin the trusted `storeDir` and the untrusted `codexHome`. */
async function resolveDir(absPath: string): Promise<FileHandle> {
  let fh = await fsp.open("/", FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
  try {
    for (const part of pathComponents(absPath)) {
      const next = await openAt(fh, part, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
      await fh.close();
      fh = next;
    }
    return fh;
  } catch (error) {
    await closeQuietly(fh);
    throw error;
  }
}

/** Resolve the PARENT of `absPath` ancestor-safely, then create (idempotently) and open ONLY the
 *  final component relative to that pinned parent — never a pathname `mkdir -p`, so no symlinked
 *  ancestor is created through or followed. The parent must already exist. */
async function resolveDirEnsuringFinal(absPath: string, mode: number): Promise<FileHandle> {
  const parts = pathComponents(absPath);
  if (parts.length === 0) {
    throw new Error("codex session store: refusing to operate on the filesystem root");
  }
  const base = parts[parts.length - 1]!;
  const parentFd = await resolveDir(`/${parts.slice(0, -1).join("/")}`);
  try {
    await mkdirAt(parentFd, base, mode); // idempotent (EEXIST tolerated)
    return await openAt(parentFd, base, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
  } finally {
    await closeQuietly(parentFd);
  }
}

/** `mkdir <parentFd>/<name>` (openat-style, relative to a held dirfd), tolerating EEXIST. */
async function mkdirAt(parent: FileHandle, name: string, mode: number): Promise<void> {
  try {
    await fsp.mkdir(`/proc/self/fd/${parent.fd}/${name}`, { mode });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
  }
}

/** `rm -rf <parentFd>/<name>` (relative to a held dirfd). `name` is a single validated
 *  component (a gen id, a temp file, or the allowlist subdir), never a traversal. */
async function rmAt(parent: FileHandle, name: string): Promise<void> {
  await fsp.rm(`/proc/self/fd/${parent.fd}/${name}`, { recursive: true, force: true });
}

/** `lstat <parentFd>/<name>` (relative to a held dirfd); undefined if absent. lstat does NOT
 *  follow a symlinked final component, so a planted symlink is detectable. */
async function lstatAt(parent: FileHandle, name: string): Promise<Stats | undefined> {
  try {
    return await fsp.lstat(`/proc/self/fd/${parent.fd}/${name}`);
  } catch {
    return undefined;
  }
}

/** The classified result of reading the current pointer. */
type PointerRead = { kind: "present"; genId: string } | { kind: "absent" } | { kind: "corrupt" };

/** Read + CLASSIFY the current pointer relative to a held `storeDir` dirfd. ENOENT → `absent`; a
 *  symlinked/non-file/over-long/bad-grammar pointer → `corrupt` (both fail-safe → fresh session);
 *  a GENUINE I/O error (EACCES/EIO/…) THROWS, so callers keep uncertainty distinct from absence
 *  (inspect → "unknown", persist → fail-closed rather than a wrong reap). `faultPointerRead` is a
 *  test-only injected fault (see {@link CodexSessionStoreHooks}); production passes undefined. */
async function readCurrentPointer(
  storeFd: FileHandle,
  faultPointerRead: (() => Error) | undefined,
): Promise<PointerRead> {
  if (faultPointerRead) throw faultPointerRead();
  let fh: FileHandle | undefined;
  try {
    fh = await fsp.open(`/proc/self/fd/${storeFd.fd}/${CURRENT_POINTER}`, FS.O_RDONLY | FS.O_NOFOLLOW);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT") return { kind: "absent" };
    if (code === "ELOOP" || code === "EISDIR" || code === "ENOTDIR") return { kind: "corrupt" };
    throw error; // EACCES / EIO / … — genuine uncertainty, propagated
  }
  try {
    const st = await fh.stat();
    if (!st.isFile() || st.size > POINTER_MAX_BYTES) return { kind: "corrupt" };
    const raw = (await readBounded(fh, st.size)).toString("utf8").trim();
    return isValidGenId(raw) ? { kind: "present", genId: raw } : { kind: "corrupt" };
  } finally {
    await closeQuietly(fh);
  }
}

/** Publish `genId` by writing it to a fresh unique temp file and renaming that ONTO `current` —
 *  one same-parent atomic rename that REPLACES the pointer (it is never unlinked), so a reader
 *  or a crashed PROCESS always sees `current` naming a complete generation. This is process-crash
 *  atomic VISIBILITY only; it is NOT a whole-system power-loss durability guarantee (no fsync) —
 *  a store lost to power loss simply fails safe to a fresh session. On ANY write/rename failure
 *  the temp is unlinked, so no `.current.tmp.*` residue is left. Relative to the held storeDir
 *  dirfd. */
async function publishPointer(storeFd: FileHandle, genId: string): Promise<void> {
  const tmpName = `${POINTER_TMP_PREFIX}${randomBytes(8).toString("hex")}`;
  const tmpPath = `/proc/self/fd/${storeFd.fd}/${tmpName}`;
  let fh: FileHandle | undefined;
  let published = false;
  try {
    fh = await fsp.open(tmpPath, FS.O_WRONLY | FS.O_CREAT | FS.O_EXCL | FS.O_NOFOLLOW, 0o600);
    await fh.writeFile(`${genId}\n`);
    await closeQuietly(fh);
    fh = undefined;
    await fsp.rename(tmpPath, `/proc/self/fd/${storeFd.fd}/${CURRENT_POINTER}`);
    published = true;
  } finally {
    await closeQuietly(fh);
    if (!published) await fsp.rm(tmpPath, { force: true }).catch(() => {}); // never leave a temp
  }
}

/** Remove leftover `.current.tmp.*` files (a crashed publish's residue). Safe with no reader
 *  coordination: a temp is only ever renamed onto `current` by its own creator, so a leftover is
 *  abandoned and read by no one. Bounded by the scan cap. Relative to the held `storeDir` dirfd. */
async function sweepStaleTemps(storeFd: FileHandle, bounds: SessionStoreBounds): Promise<void> {
  let entries;
  try {
    entries = await fsp.readdir(`/proc/self/fd/${storeFd.fd}`, { withFileTypes: true });
  } catch {
    return;
  }
  let scanned = 0;
  for (const entry of entries) {
    if ((scanned += 1) > bounds.maxScanEntries) return;
    if (entry.isFile() && entry.name.startsWith(POINTER_TMP_PREFIX)) {
      await rmAt(storeFd, entry.name).catch(() => {});
    }
  }
}

/** Reap every generation directory except those in `keep` (the new + previous generation).
 *  Reader-safe because every store operation is serialized (see the per-store lock below): no
 *  reader can be mid-read of a generation while a persist reaps. Bounded by the scan cap;
 *  best-effort per entry. Relative to the held `generations/` dirfd. */
async function reapGenerations(
  gensFd: FileHandle,
  keep: Set<string>,
  bounds: SessionStoreBounds,
): Promise<void> {
  let entries;
  try {
    entries = await fsp.readdir(`/proc/self/fd/${gensFd.fd}`, { withFileTypes: true });
  } catch {
    return;
  }
  let scanned = 0;
  for (const entry of entries) {
    if ((scanned += 1) > bounds.maxScanEntries) return;
    if (!entry.isDirectory() || keep.has(entry.name)) continue;
    await rmAt(gensFd, entry.name).catch(() => {});
  }
}

// ─── per-storeDir operation lock (full serialization) ───────────────────────────
// The store is per-run / per-worker-local and is NEVER shared across processes (a different
// worker has no store, per the module doc), so an IN-PROCESS discipline suffices. EVERY store
// operation — persist, adopt, inspect and remove — runs under this per-storeDir lock, from the
// root/pointer open through completion. So a reader holds the lock across reading `current` and
// opening + copying its generation, and no persist (hence no reap) can run in between: there is
// no read→open→lease window to race, and no lease bookkeeping is needed. Two persists likewise
// never overlap. Keys are the resolved storeDir path.
const storeChains = new Map<string, Promise<void>>();
function withStoreLock<T>(storeDir: string, fn: () => Promise<T>): Promise<T> {
  const key = resolve(storeDir);
  const prior = storeChains.get(key) ?? Promise.resolve();
  const run = prior.then(fn, fn); // run after the previous op settles, whatever its outcome
  const tail = run.then(() => {}, () => {}); // the chain tail never rejects
  storeChains.set(key, tail);
  // IDENTITY-SAFE cleanup: when THIS tail settles, drop the key ONLY if it is still the tail. A
  // newer queued op replaces `storeChains.get(key)` synchronously, so an older completion can never
  // delete a newer op's tail; when the LAST op for a key settles, its entry is removed and the
  // registry returns to zero (no retained per-run key after ordinary ops or a terminal remove).
  void tail.then(() => {
    if (storeChains.get(key) === tail) storeChains.delete(key);
  });
  return run;
}

/** TEST-ONLY read-only diagnostic: the number of live per-storeDir lock chains. No `storeChains`
 *  is otherwise observable, so the churn regression uses this to prove the registry returns to
 *  zero after operations complete. It grants no mutation or fault authority. */
export function storeLockRegistrySize(): number {
  return storeChains.size;
}

// ─── Public API ─────────────────────────────────────────────────────────────────

/** TEST-ONLY composition hooks. The production store ({@link CodexSessionStore}) is built with
 *  none. They exist so the regression suite can place DETERMINISTIC barriers (not probabilistic
 *  sampling) around the publication protocol, via {@link createCodexSessionStore}. */
export interface CodexSessionStoreHooks {
  /** When set, the current-pointer read throws this, simulating a GENUINE I/O error (EACCES/EIO)
   *  that the classification must distinguish from a missing/corrupt pointer. */
  readonly faultPointerRead?: () => Error;
}

export interface CodexSessionStoreApi {
  persist(codexHome: string, storeDir: string, opts?: { bounds?: SessionStoreBounds }): Promise<PersistResult>;
  adopt(storeDir: string, codexHome: string): Promise<AdoptResult>;
  inspect(storeDir: string, opts?: { scanCap?: number }): Promise<SessionPresence>;
  remove(storeDir: string): Promise<void>;
}

/**
 * Build a credential-free Codex session store. Every method takes explicit directory paths so it
 * is unit-testable against temp dirs with no real Codex.
 *
 *   - `persist` copies the credential-free session subset OUT of a live root's `$CODEX_HOME` into
 *     a fresh immutable generation and publishes it (survives root reaping).
 *   - `adopt` seeds a fresh root's `$CODEX_HOME/sessions/` from the current generation.
 *   - `inspect` reports whether the current generation holds a resumable session (tri-state).
 *   - `remove` deletes the store at the terminal boundary (credential-free, always removable).
 *
 * On-disk layout under `storeDir`: `generations/<genId>/sessions/**` (immutable per-persist
 * generations) plus a plain-text `current` pointer naming the winning `<genId>`, published by one
 * atomic same-parent rename (see the module header). CONCURRENCY: EVERY operation runs under the
 * per-storeDir operation lock (see above), so a reader holds the lock across reading `current` and
 * copying its generation and no persist (hence no reap) can run in between; two persists never
 * overlap. The store is per-worker-local and NEVER shared across processes, so this in-process
 * lock is sufficient.
 */
export function createCodexSessionStore(hooks: CodexSessionStoreHooks = {}): CodexSessionStoreApi {
  return {
    /**
     * Copy the credential-free session subset from `codexHome` into a FRESH immutable generation
     * and publish it as the current generation with one atomic pointer rename. Only the allowlist
     * is copied; symlinks are rejected; the copy is bounded and throws
     * {@link CodexSessionStoreBoundError} on a breach. `opts.bounds` overrides the default caps.
     *
     * The previous generation and the `current` pointer are never touched until the new
     * generation is fully copied, so ANY failure leaves the previous good generation still
     * current and discoverable; the incomplete new generation is a never-pointed orphan
     * (best-effort removed here, else reaped by the next persist). FRESHNESS: an absent or empty
     * source publishes an EMPTY generation and advances `current`, so `inspect` becomes absent and
     * a stale session cannot resume (this is the established behavior, not a keep-old cache). A
     * genuinely UNREADABLE `current` (EACCES) fails persist closed rather than reap. Persists are
     * serialized per storeDir.
     */
    async persist(codexHome, storeDir, opts = {}) {
      return withStoreLock(storeDir, async () => {
        const bounds = opts.bounds ?? DEFAULT_SESSION_STORE_BOUNDS;
        let storeFd: FileHandle | undefined;
        let codexHomeFd: FileHandle | undefined;
        let gensFd: FileHandle | undefined;
        let newGenFd: FileHandle | undefined;
        let newGenId: string | undefined;
        try {
          // Pin the TRUSTED store dir ancestor-safely; create ONLY its final component; set its
          // mode through the held fd (never a pathname mkdir -p / chmod).
          storeFd = await resolveDirEnsuringFinal(storeDir, 0o700);
          await storeFd.chmod(0o700);

          // Pin the UNTRUSTED codexHome ancestor-safely. ENOENT → empty source (still publish an
          // empty generation for freshness). ELOOP/ENOTDIR (symlinked/odd ancestor) → refuse. A
          // genuine I/O error propagates and fails persist closed (old store left intact).
          let sourceAbsent = false;
          try {
            codexHomeFd = await resolveDir(codexHome);
          } catch (error) {
            const code = (error as NodeJS.ErrnoException).code;
            if (code === "ENOENT") sourceAbsent = true;
            else if (code === "ELOOP" || code === "ENOTDIR") {
              throw new CodexSessionStoreError("source-not-directory");
            } else throw error;
          }

          // Classify the current pointer BEFORE creating anything. A genuine I/O error throws here
          // and fails persist closed, so an unreadable `current` never causes a wrong reap.
          const pointer = await readCurrentPointer(storeFd, hooks.faultPointerRead);
          const prevGenId = pointer.kind === "present" ? pointer.genId : undefined;

          // Build a fresh immutable generation, fd-anchored under storeFd.
          await mkdirAt(storeFd, GENERATIONS_SUBDIR, 0o700);
          gensFd = await openAt(storeFd, GENERATIONS_SUBDIR, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
          newGenId = freshGenId();
          await fsp.mkdir(`/proc/self/fd/${gensFd.fd}/${newGenId}`, { mode: 0o700 });
          newGenFd = await openAt(gensFd, newGenId, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);

          // Copy the allowlisted source into the new generation (fail-closed). A symlinked
          // codexHome/sessions is refused by the copy's own O_NOFOLLOW top open.
          let tally: { files: number; bytes: number } = { files: 0, bytes: 0 };
          if (!sourceAbsent && codexHomeFd) {
            tally = await copyAllowlistedSessions(
              `/proc/self/fd/${codexHomeFd.fd}/${SESSION_ALLOWED_SUBDIR}`,
              `/proc/self/fd/${newGenFd.fd}/${SESSION_ALLOWED_SUBDIR}`,
              bounds,
              true,
            );
          }
          // A generation is always well-formed: ensure a (possibly empty) sessions/ dir.
          await mkdirAt(newGenFd, SESSION_ALLOWED_SUBDIR, 0o700);

          // Publish (even an empty generation — freshness), then reap stale generations keeping the
          // new + previous only, and sweep leftover pointer temps. Reader-safe: the operation lock
          // means no reader is mid-read while this reaps.
          await publishPointer(storeFd, newGenId);
          const keep = new Set<string>([newGenId]);
          if (prevGenId) keep.add(prevGenId);
          await reapGenerations(gensFd, keep, bounds);
          await sweepStaleTemps(storeFd, bounds);
          return { files: tally.files, bytes: tally.bytes };
        } catch (error) {
          // The pointer + previous generations were never touched before the atomic publish, so
          // the previous good store stays current and discoverable. Drop the partial new gen.
          if (gensFd && newGenId) await rmAt(gensFd, newGenId).catch(() => {});
          if (
            error instanceof CodexSessionStoreBoundError ||
            error instanceof CodexSessionStoreError
          ) {
            throw error;
          }
          throw new CodexSessionStoreError("io-error");
        } finally {
          await closeQuietly(newGenFd);
          await closeQuietly(gensFd);
          await closeQuietly(codexHomeFd);
          await closeQuietly(storeFd);
        }
      });
    },

    /**
     * Seed `codexHome/sessions/` from the store's CURRENT generation BEFORE app-server start. A
     * missing store, an absent/invalid/unreadable pointer, a dangling generation, or any I/O error
     * → no-op (0 files), NEVER throws (fail-safe → fresh session). Both roots are anchored by an
     * ancestor-safe component walk (a symlinked ancestor of storeDir or codexHome is refused), and
     * the dest `sessions/` is never written THROUGH a symlink. Runs under the per-storeDir operation
     * lock, so no concurrent persist can reap the generation between reading `current` and copying it.
     */
    async adopt(storeDir, codexHome) {
      return withStoreLock(storeDir, async () => {
        let storeFd: FileHandle | undefined;
        let gensFd: FileHandle | undefined;
        let genFd: FileHandle | undefined;
        let codexHomeFd: FileHandle | undefined;
        try {
          try {
            storeFd = await resolveDir(storeDir);
          } catch {
            return { files: 0 };
          }
          let pointer: PointerRead;
          try {
            pointer = await readCurrentPointer(storeFd, hooks.faultPointerRead);
          } catch {
            return { files: 0 }; // genuine I/O → fail-safe fresh session
          }
          if (pointer.kind !== "present") return { files: 0 };
          try {
            gensFd = await openAt(storeFd, GENERATIONS_SUBDIR, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
            genFd = await openAt(gensFd, pointer.genId, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
          } catch {
            return { files: 0 }; // a dangling pointer → fresh session
          }

          // Pin the dest codexHome ancestor-safely, creating ONLY its final component if a fresh
          // root's home does not exist yet.
          try {
            codexHomeFd = await resolveDir(codexHome);
          } catch (error) {
            if ((error as NodeJS.ErrnoException).code !== "ENOENT") return { files: 0 };
            codexHomeFd = await resolveDirEnsuringFinal(codexHome, 0o700);
          }

          // Never write THROUGH a symlinked (or non-dir) dest `sessions/`: remove such an entry
          // first (rm on a symlink unlinks only the link), then create a real in-tree directory.
          const destStat = await lstatAt(codexHomeFd, SESSION_ALLOWED_SUBDIR);
          if (destStat && (destStat.isSymbolicLink() || !destStat.isDirectory())) {
            await rmAt(codexHomeFd, SESSION_ALLOWED_SUBDIR);
          }
          await mkdirAt(codexHomeFd, SESSION_ALLOWED_SUBDIR, 0o700);

          const tally = await copyAllowlistedSessions(
            `/proc/self/fd/${genFd.fd}/${SESSION_ALLOWED_SUBDIR}`,
            `/proc/self/fd/${codexHomeFd.fd}/${SESSION_ALLOWED_SUBDIR}`,
            DEFAULT_SESSION_STORE_BOUNDS,
            false, // fail-safe: adopt what fits, never throw
          );
          return { files: tally.files };
        } catch {
          return { files: 0 };
        } finally {
          await closeQuietly(genFd);
          await closeQuietly(gensFd);
          await closeQuietly(codexHomeFd);
          await closeQuietly(storeFd);
        }
      });
    },

    /**
     * Tri-state presence of a resumable session in the store's CURRENT generation:
     * "present" when it holds ≥1 session artifact, "absent" when the store/pointer/generation is
     * missing, corrupt or empty (fail-safe → fresh session), "unknown" ONLY on genuine I/O
     * uncertainty (an unreadable storeDir or pointer — EACCES/EIO — or a tree too large to scan
     * within bounds). Runs under the per-storeDir operation lock.
     */
    async inspect(storeDir, opts = {}) {
      return withStoreLock(storeDir, async () => {
        let storeFd: FileHandle | undefined;
        let gensFd: FileHandle | undefined;
        let genFd: FileHandle | undefined;
        try {
          try {
            storeFd = await resolveDir(storeDir);
          } catch (error) {
            const code = (error as NodeJS.ErrnoException).code;
            if (code === "ENOENT" || code === "ENOTDIR" || code === "ELOOP") return "absent";
            return "unknown"; // EACCES/… — genuinely cannot tell
          }
          let pointer: PointerRead;
          try {
            pointer = await readCurrentPointer(storeFd, hooks.faultPointerRead);
          } catch {
            return "unknown"; // genuine I/O reading the pointer is uncertainty, NOT absence
          }
          if (pointer.kind !== "present") return "absent"; // missing/corrupt pointer → fresh
          try {
            gensFd = await openAt(storeFd, GENERATIONS_SUBDIR, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
            genFd = await openAt(gensFd, pointer.genId, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
          } catch (error) {
            const code = (error as NodeJS.ErrnoException).code;
            if (code === "ENOENT" || code === "ENOTDIR") return "absent"; // dangling pointer
            return "unknown";
          }
          try {
            const found = await hasAllowlistedArtifact(
              `/proc/self/fd/${genFd.fd}/${SESSION_ALLOWED_SUBDIR}`,
              opts.scanCap ?? DEFAULT_SESSION_STORE_BOUNDS.maxFiles,
            );
            return found ? "present" : "absent";
          } catch (error) {
            const code = (error as NodeJS.ErrnoException).code;
            if (code === "ENOENT" || code === "ENOTDIR") return "absent";
            return "unknown"; // bound breach or unexpected I/O → uncertainty
          }
        } finally {
          await closeQuietly(genFd);
          await closeQuietly(gensFd);
          await closeQuietly(storeFd);
        }
      });
    },

    /**
     * Delete the store at the terminal boundary (ancestor-safe: the final component is removed
     * relative to a pinned parent). Credential-free, so removed unconditionally. Idempotent: a
     * missing store or parent is a no-op. Runs under the per-storeDir operation lock.
     */
    async remove(storeDir) {
      return withStoreLock(storeDir, async () => {
        const parts = pathComponents(storeDir);
        if (parts.length === 0) return;
        let parentFd: FileHandle | undefined;
        try {
          parentFd = await resolveDir(`/${parts.slice(0, -1).join("/")}`);
        } catch {
          return; // parent gone → nothing to remove
        }
        try {
          await rmAt(parentFd, parts[parts.length - 1]!);
        } finally {
          await closeQuietly(parentFd);
        }
      });
    },
  };
}

/** The credential-free Codex session store (production instance; no test hooks). */
export const CodexSessionStore: CodexSessionStoreApi = createCodexSessionStore();
