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
// ─── TRAVERSAL / SYMLINK SAFETY (openat2-style mindset) ─────────────────────────
// All work is confined to the given source/dest roots: every copied path is resolved
// and checked to stay within its root, directories are descended only after an
// `lstat` proves they are not symlinks, and each file is copied through an
// `O_NOFOLLOW` open on BOTH ends so a symlink (planted in the source `sessions/` or
// at a dest path) is rejected atomically rather than followed out of the tree. The
// copy is bounded — a per-file byte cap, a total byte cap, and a file-count cap — so
// a hostile or runaway tree cannot copy unboundedly.
//
// This module NEVER logs file contents or a token; diagnostics are static and
// bounded (an error names only which cap was hit, never a path or a byte).

import fsp from "node:fs/promises";
import { constants as FS } from "node:fs";
import type { Stats } from "node:fs";
import { dirname, extname, isAbsolute, join, relative, resolve } from "node:path";

import type { SessionPresence } from "../harness.js";

/** The ONE credential-free subtree under `$CODEX_HOME` we ever read or seed. Auth
 *  material lives OUTSIDE it (at the `$CODEX_HOME` root), so scoping to this subdir
 *  is the first allowlist gate. */
export const SESSION_ALLOWED_SUBDIR = "sessions";

/** Allowed rollout/transcript file extensions. Pinned Codex writes rollouts as
 *  JSONL; anything else under `sessions/` is treated as uncertain and EXCLUDED.
 *  Provisional pending m3b:packaged confirmation (see file header). */
export const SESSION_ALLOWED_EXTENSIONS: readonly string[] = [".jsonl"];

/** Auth-shaped name substrings (case-insensitive). A file OR directory whose name
 *  contains any of these is excluded even when it otherwise looks like a rollout —
 *  the second allowlist gate, applied belt-and-braces on top of the subdir scope so
 *  an `auth-*.jsonl` accidentally written under `sessions/` is still never copied. */
export const SESSION_DENY_NAME_SUBSTRINGS: readonly string[] = [
  "auth",
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
}

/** Conservative production caps. A real rollout subtree is a handful of JSONL files;
 *  these exist so a hostile/runaway tree fails closed rather than copying forever. */
export const DEFAULT_SESSION_STORE_BOUNDS: SessionStoreBounds = {
  maxFiles: 10_000,
  maxTotalBytes: 512 * 1024 * 1024, // 512 MiB
  maxFileBytes: 64 * 1024 * 1024, // 64 MiB
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

// ─── Confinement + symlink-safe file copy ───────────────────────────────────────

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

/** Copy one file source→dest without following a symlink on EITHER end. `O_NOFOLLOW`
 *  makes the open fail (ELOOP) if the final component is a symlink, so a symlink
 *  planted in the source or a pre-existing symlink at the dest is rejected atomically
 *  rather than read/written through. Bounded by the caller's per-file cap. */
async function copyFileNoFollow(src: string, dest: string): Promise<void> {
  const input = await fsp.open(src, FS.O_RDONLY | FS.O_NOFOLLOW);
  try {
    const data = await input.readFile();
    const output = await fsp.open(
      dest,
      FS.O_WRONLY | FS.O_CREAT | FS.O_TRUNC | FS.O_NOFOLLOW,
      0o600,
    );
    try {
      await output.writeFile(data);
    } finally {
      await output.close();
    }
  } finally {
    await input.close();
  }
}

interface CopyTally {
  files: number;
  bytes: number;
}

/**
 * Recursively copy the allowlisted session artifacts from `srcSessionsDir` into
 * `destSessionsDir`, preserving relative structure. Rejects symlinks (files and
 * dirs), stays within both roots, skips non-regular files and non-allowlisted names,
 * and enforces the bounds.
 *
 * `throwOnBound` distinguishes the two contracts: `persist` passes `true` (fail
 * CLOSED — a breach throws and the partial store is not trusted); `adopt` passes
 * `false` (fail SAFE — it copies what fits and stops).
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

  const walk = async (relDir: string): Promise<boolean> => {
    const absDir = join(srcRoot, relDir);
    let entries;
    try {
      entries = await fsp.readdir(absDir, { withFileTypes: true });
    } catch {
      return true; // an unreadable subdir is skipped (fail-safe), traversal continues
    }
    for (const entry of entries) {
      const relPath = relDir === "" ? entry.name : `${relDir}/${entry.name}`;
      const absSrc = join(srcRoot, relPath);
      if (!isWithin(srcRoot, absSrc)) continue; // confinement

      const st = await tryLstat(absSrc);
      if (!st) continue;
      if (st.isSymbolicLink()) continue; // rejected: never follow out of the roots

      if (st.isDirectory()) {
        if (nameHasDenySubstring(entry.name)) continue; // never descend an auth-shaped dir
        const proceed = await walk(relPath);
        if (!proceed) return false;
        continue;
      }
      if (!st.isFile()) continue; // sockets/fifos/devices excluded
      if (!isAllowedSessionArtifact(entry.name)) continue;

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

      const absDest = join(destRoot, relPath);
      if (!isWithin(destRoot, absDest)) continue; // confinement
      await fsp.mkdir(dirname(absDest), { recursive: true, mode: 0o700 });
      await copyFileNoFollow(absSrc, absDest);
      tally.files += 1;
      tally.bytes += st.size;
    }
    return true;
  };

  await walk("");
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
   */
  async persist(
    codexHome: string,
    storeDir: string,
    opts: { bounds?: SessionStoreBounds } = {},
  ): Promise<PersistResult> {
    const bounds = opts.bounds ?? DEFAULT_SESSION_STORE_BOUNDS;
    await fsp.mkdir(storeDir, { recursive: true, mode: 0o700 });
    await fsp.chmod(storeDir, 0o700);

    const srcSessions = join(codexHome, SESSION_ALLOWED_SUBDIR);
    const destSessions = join(storeDir, SESSION_ALLOWED_SUBDIR);
    // Refresh: clear any prior copy so the store never holds a stale rollout set.
    await fsp.rm(destSessions, { recursive: true, force: true });

    const tally = await copyAllowlistedSessions(srcSessions, destSessions, bounds, true);
    return { files: tally.files, bytes: tally.bytes };
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
