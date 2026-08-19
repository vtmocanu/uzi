// PRD-link resolver + input assembly (PRD #362 M3b).
//
// Security-sensitive. The clone this reads from is attacker-influenceable: a
// malicious issue/PRD can try `prds/../../../etc/passwd`, a backslash-smuggled
// segment, a dotfile like `prds/.git/config.md`, or a symlink at `prds/x.md`
// pointing outside the clone. There is no TS equivalent of Go's guard, so this
// PORTS `api/internal/prdpath/prdpath.go` `Validate` faithfully (see
// `validatePrdPath`) and the detector shape of `forgesvc.prdLinkRe`
// (`api/internal/forgesvc/service.go`), then adds two defense-in-depth
// containment checks (prefix + realpath) on top of the ported validator.
//
// Design mirrors Go's `prdpath.Links`: DETECT the `prds/…*.md` core directly in
// prose (a blob-URL prefix or `#`/`?` suffix falls outside the match by
// construction), then VALIDATE the whole core. The detector alone is NOT a
// validator — its charset matches `..`, so validation is what rejects traversal.
//
// `resolvePrdInput` NEVER throws: every failure path returns the nulls fallback
// so the caller falls back to issue title + body. Dependency-light: node
// `fs`/`path` only.

import { promises as fs } from "node:fs";
import path from "node:path";

/** Bounds a PRD path. Ported from Go `prdpath.MaxPathLen` (bytes). */
const MAX_PATH_LEN = 512;

/** Directory every PRD path is rooted at, and the required suffix. */
const ROOT = "prds/";
const EXT = ".md";

/** Cap on bytes read from a resolved PRD file — a PRD is small; this stops a
 *  huge/hostile file from blowing the prompt or memory. */
const MAX_PRD_BYTES = 256 * 1024;

/** Cap on characters the PRD-link detector scans in the untrusted issue
 *  description. A real issue's PRD link appears well within this, so bounding
 *  the scan costs nothing real. Defense-in-depth: the core pattern is already
 *  linear (no nested straddling quantifiers — see `PRD_LINK_RE`), so an attacker
 *  cannot make the scan pathological, and this cap bounds cost even under a
 *  future regex change. */
const MAX_DESC_SCAN = 100_000;

/** Minimal warn sink so this module stays dependency-light and testable; the
 *  real caller (M3c) passes the run's structured logger. */
export interface WarnLogger {
  warn(msg: string, fields?: Record<string, unknown>): void;
}

const NOOP_LOG: WarnLogger = { warn() {} };

/** Reports whether a byte is in the shared per-segment charset `[A-Za-z0-9._-]`.
 *  This is Go `prdpath.pathByte`, minus `\w`'s Unicode reach (deliberate — a path
 *  M4 accepts must be one M5 can find). */
function isPathByte(c: number): boolean {
  return (
    (c >= 0x61 && c <= 0x7a) || // a-z
    (c >= 0x41 && c <= 0x5a) || // A-Z
    (c >= 0x30 && c <= 0x39) || // 0-9
    c === 0x2e || // .
    c === 0x5f || // _
    c === 0x2d // -
  );
}

/** Port of Go `prdpath.validateSegment`: non-empty; not `.`/`..`; not a dotfile;
 *  charset only. */
function validateSegment(seg: string): boolean {
  if (seg === "") return false;
  if (seg === "." || seg === "..") return false;
  if (seg.startsWith(".")) return false; // no dotfiles: .git, .claude, .ssh
  const bytes = Buffer.from(seg, "utf8");
  for (const b of bytes) {
    if (!isPathByte(b)) return false;
  }
  return true;
}

/**
 * Port of Go `prdpath.Validate` (`api/internal/prdpath/prdpath.go`). Reports
 * whether `p` is a well-formed clone-relative PRD file path. Every rule is
 * load-bearing; the redundant traversal rejections (the `.`/`..` segment check,
 * the dotfile-prefix rule, AND the normalize check) are ALL kept deliberately —
 * the Go file documents (measured 2026-07-26) that each independently rejects
 * `..`, and depth is worth more than a line count at a security boundary.
 */
export function validatePrdPath(p: string): boolean {
  // non-empty
  if (p === "") return false;
  // length <= MaxPathLen (bytes, matching Go's len(p))
  if (Buffer.byteLength(p, "utf8") > MAX_PATH_LEN) return false;
  // Control bytes, DEL, and backslash. NUL is caught here; backslash keeps a
  // Windows-style separator from smuggling a segment past the `/` split.
  for (const b of Buffer.from(p, "utf8")) {
    if (b < 0x20 || b === 0x7f || b === 0x5c /* \\ */) return false;
  }
  // Rooted. Also rejects absolute `/prds/…` (no `prds/` prefix) and non-PRD dirs.
  if (!p.startsWith(ROOT)) return false;
  // Must end in `.md`.
  if (!p.endsWith(EXT)) return false;
  // Whole-string per-segment predicate.
  for (const seg of p.split("/")) {
    if (!validateSegment(seg)) return false;
  }
  // Canonical form: rejects `//`, a trailing `/`, a leading `./`, and traversal.
  // `path.posix` so separators are `/` (Go's `path.Clean`).
  if (path.posix.normalize(p) !== p) return false;
  return true;
}

// Detector for a PRD-path core in prose. We match the `prds/…*.md` core
// DIRECTLY rather than the blob URL around it: the core appears literally in the
// text whether bare or inside a GitHub `/blob/<ref>/` or GitLab `/-/blob/<ref>/`
// URL (`…/blob/main/prds/362-x.md` literally contains `prds/362-x.md`), and any
// `#`/`?` suffix falls outside the match by construction — so no prefix/suffix
// stripping is needed. Matching the core directly avoids the nested straddling
// greedy classes the old blob-prefix pattern carried (`\S+` … `blob/` …
// `[^\s)]+`), which backtracked O(n²) on hostile input — a worker-DoS ReDoS on
// this exact untrusted path (~1.1s blocked at 65k chars, ~64s at 500k). This
// pattern has no quantifiers straddling a literal: `/`-delimited segments (`/`
// is outside the charset) anchored by the `prds/` literal, so it scans
// effectively linearly. The charset is `[A-Za-z0-9._-]` — the SAME per-segment
// set `validatePrdPath` enforces (not `\w`, which is Unicode-wide) — so a
// detected core aligns with the validator. Detection is still liberal (the
// charset matches `..`); `validatePrdPath` is what rejects traversal.
const PRD_LINK_RE = /prds\/(?:[A-Za-z0-9._-]+\/)*[A-Za-z0-9._-]+\.md/g;

// The PRD number encoded in a core's filename basename: both `prds/362-x.md` and
// `prds/done/362-x.md` yield 362. Null when the basename has no leading integer. Used
// to prefer the PRD whose number matches the run's issue iid (the repo convention is
// `prds/<issue-number>-slug.md`).
function prdCoreNumber(core: string): number | null {
  const base = core.slice(core.lastIndexOf("/") + 1);
  const m = /^(\d+)/.exec(base);
  return m ? Number.parseInt(m[1]!, 10) : null;
}

/**
 * Finds a VALID `prds/…*.md` core in `text`. Matches candidate cores directly (the
 * whole match IS the core — a blob-URL prefix and `#`/`?` suffix fall outside it by
 * construction), scanning at most {@link MAX_DESC_SCAN} characters of the untrusted
 * input, and returns a core that passes `validatePrdPath`. Returns null when none is
 * valid.
 *
 * When `preferIid` is given, a valid core whose filename number equals it wins over
 * document order — an issue body that mentions another PRD ("supersedes
 * prds/100-old.md") BEFORE its own would otherwise resolve to the wrong PRD. With no
 * iid match (or no `preferIid`), it falls back to the FIRST valid core, so the common
 * single-link case and any non-conventional numbering are unchanged.
 *
 * This assumes the repo convention `prds/<iid>-slug.md`. In the narrow case where the
 * issue's OWN PRD is misnamed (number != iid) AND another mentioned link's number
 * coincidentally equals the iid, the preference resolves to that other link — a wrong
 * pick the old first-valid pass avoided. Accepted: it is advisory-only (summary text,
 * never implemented code), and the convention holds for essentially every real issue.
 */
export function findValidPrdCore(
  text: string,
  preferIid?: number | null,
): string | null {
  // Bound the untrusted scan (defense-in-depth; see MAX_DESC_SCAN).
  const scanned = (text ?? "").slice(0, MAX_DESC_SCAN);
  // Fresh regex state per call (the shared literal carries the `g` flag).
  const re = new RegExp(PRD_LINK_RE.source, PRD_LINK_RE.flags);
  let firstValid: string | null = null;
  for (const m of scanned.matchAll(re)) {
    const core = m[0];
    if (!core || !validatePrdPath(core)) continue;
    // `!= null` covers both undefined (no hint) and null (an issue/undefined run whose
    // issueIid is null); either way, no preference is applied.
    if (preferIid != null && prdCoreNumber(core) === preferIid) return core;
    if (firstValid === null) firstValid = core;
  }
  return firstValid;
}

export interface PrdInput {
  prdPath: string | null;
  prdText: string | null;
}

const NO_PRD: PrdInput = { prdPath: null, prdText: null };

/** True when `abs` is `root` itself or lives strictly inside it. */
function contained(abs: string, root: string): boolean {
  return abs === root || abs.startsWith(root + path.sep);
}

/**
 * Resolves a run's PRD input from an untrusted `issueDescription` against a
 * `cloneDir`. Finds a valid `prds/*.md` core — preferring the one whose number
 * matches `issueIid` when given (see {@link findValidPrdCore}) — guards path
 * traversal (ported validator) plus two defense-in-depth containment checks
 * (resolved prefix, then realpath to catch a symlink escape), and reads the file
 * bounded to {@link MAX_PRD_BYTES}. NEVER throws — every failure returns the nulls
 * fallback so the caller uses title + body only.
 */
export async function resolvePrdInput(
  issueDescription: string,
  cloneDir: string,
  issueIid?: number | null,
  log: WarnLogger = NOOP_LOG,
): Promise<PrdInput> {
  try {
    const core = findValidPrdCore(issueDescription ?? "", issueIid);
    if (!core) return NO_PRD;

    // Defense-in-depth #1: resolve and require containment in the clone root.
    // A validated core cannot carry `..`, so this cannot fail today — it is a
    // belt-and-braces assertion that survives a future loosening of the validator.
    const root = path.resolve(cloneDir);
    const abs = path.resolve(cloneDir, core);
    if (!contained(abs, root)) {
      log.warn("prd-link: resolved path escapes clone root; ignoring", { core });
      return NO_PRD;
    }

    // Defense-in-depth #2: realpath both, to catch a SYMLINK at prds/x.md that
    // points outside the clone. realpath(abs) throws for a missing file — that
    // lands in the catch below and returns the nulls fallback.
    let realRoot: string;
    let realFile: string;
    try {
      realRoot = await fs.realpath(root);
      realFile = await fs.realpath(abs);
    } catch (err) {
      log.warn("prd-link: PRD file not resolvable; falling back to title+body", {
        core,
        error: (err as Error).message,
      });
      return NO_PRD;
    }
    if (!contained(realFile, realRoot)) {
      log.warn("prd-link: PRD realpath escapes clone root (symlink?); ignoring", { core });
      return NO_PRD;
    }

    // Bounded read: pull at most MAX_PRD_BYTES regardless of on-disk size, so a
    // huge or hostile file cannot blow the prompt or memory.
    const fh = await fs.open(realFile, "r");
    try {
      const buf = Buffer.alloc(MAX_PRD_BYTES);
      const { bytesRead } = await fh.read(buf, 0, MAX_PRD_BYTES, 0);
      const prdText = buf.subarray(0, bytesRead).toString("utf8");
      return { prdPath: core, prdText };
    } finally {
      await fh.close();
    }
  } catch (err) {
    // Last-resort guard: nothing in this function is allowed to throw into the
    // caller. Any unexpected error → the nulls fallback.
    log.warn("prd-link: unexpected error resolving PRD input; falling back", {
      error: (err as Error).message,
    });
    return NO_PRD;
  }
}
