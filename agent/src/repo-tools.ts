// Tier-2 repo devbox.json packages (PRD #18 M5, Decisions 3 + 4).
//
// When (and ONLY when) the repo owner has opted in (repo_devbox_opt_in), the worker
// may union the packages declared in the cloned repo's own devbox.json with the
// tier-1 profile. This is the "extract the safe fields, drop the rest" discipline:
//
//   - ONLY the `packages` field is read; shell.init_hook, shell.scripts, env, flake
//     references, and every other key are ignored and NEVER executed. Extraction is
//     a comment-tolerant (JSONC) parse — comments are stripped in-process and the
//     result handed to native JSON.parse — with no shell and no `devbox` invocation
//     on the repo file.
//   - Each package is shape-validated (name / name@version), which also drops flake
//     references, paths, and junk a hostile manifest might carry.
//   - Tier-1 (uzi-stored, allowlist-validated) always wins a version conflict.

import fs from "node:fs/promises";
import path from "node:path";

// Mirror of the server's package-token shape (api/internal/toolprofile). A bare
// name or name@version; anything else (flake refs `github:…#pkg`, paths, spaces)
// is rejected.
const PKG_RE = /^[a-zA-Z0-9][a-zA-Z0-9._+-]*(@[a-zA-Z0-9][a-zA-Z0-9._+-]*)?$/;
const MAX_PKG_LEN = 128;
const MAX_REPO_PACKAGES = 64;
// The length/count caps above only bite AFTER a full readFile, so a hostile
// multi-GB devbox.json could OOM the worker before they apply. Stat-gate the read
// on this ceiling; a real manifest is a few KB.
const MAX_DEVBOX_BYTES = 1024 * 1024;

/** The base package name (before any @version). */
function baseName(pkg: string): string {
  const at = pkg.indexOf("@");
  return at >= 0 ? pkg.slice(0, at) : pkg;
}

/**
 * Partition tier-2 (repo devbox.json) packages by the server's Decision 6 denylist
 * (PRD #123 M1b). A package is DROPPED when its base name (before any @version) is in
 * `denied`; otherwise it is KEPT. The denylist ships in the claim
 * (ClaimConfig.denied_tool_packages) so the worker enforces the same credential-CLI
 * policy the server already applies to tier-1 — closing the tier-2 bypass where a
 * repo's own devbox.json could install a logged-in glab/gh/aws/vault/….
 *
 * Base-name matching so a pinned `glab@1.2` is caught when `glab` is denied.
 * Case-INSENSITIVE on the base name so a repo declaring `Glab@1.2` / `GH` is dropped
 * too rather than relying on nixpkgs resolution to fail. The returned tokens keep
 * their original casing — only the comparison is lowercased. Input order is preserved
 * in both outputs. An empty `denied` keeps everything (an older server ships no list ⇒
 * today's behavior). Apply to TIER-2 ONLY — tier-1 is already denylist-checked
 * server-side and must never be filtered here.
 */
export function filterDeniedPackages(
  repoPackages: string[],
  denied: readonly string[],
): { kept: string[]; dropped: string[] } {
  const deniedSet = new Set(denied.map((d) => d.toLowerCase()));
  const kept: string[] = [];
  const dropped: string[] = [];
  for (const pkg of repoPackages) {
    if (deniedSet.has(baseName(pkg).toLowerCase())) dropped.push(pkg);
    else kept.push(pkg);
  }
  return { kept, dropped };
}

/**
 * Strip JSONC (hujson-style) comments and tolerate trailing commas so a
 * comment-bearing devbox.json can be handed to the native JSON.parse validator.
 * A devbox manifest tolerates `//` line comments, `/* … *\/` block comments, and
 * trailing commas; strict JSON.parse throws on all three.
 *
 * String-context aware, modeled on guardrails.ts's `tokenize`: a `//`, `/* … *\/`,
 * or `,` that appears INSIDE a double-quoted JSON string (e.g. a URL value like
 * "git+https://example.com/x") is copied verbatim, NEVER treated as a comment or a
 * trailing separator. JSON strings use only double quotes; backslash escapes
 * (`\\"`, `\\\\`, …) are honored so an escaped quote never ends the string early.
 *
 * Two O(n) passes (comment strip, then trailing-comma drop), both bounded by the
 * 1 MiB stat-gate upstream. This is NOT a validator — malformed input still fails
 * downstream in JSON.parse and falls through to [].
 */
function stripJsonComments(raw: string): string {
  // Pass 1: drop `//` line and `/* */` block comments that are outside a string.
  let stripped = "";
  let inStr = false;
  let i = 0;
  const n = raw.length;
  while (i < n) {
    const ch = raw[i]!;
    if (inStr) {
      stripped += ch;
      if (ch === "\\" && i + 1 < n) { stripped += raw[i + 1]; i += 2; continue; }
      if (ch === '"') inStr = false;
      i++;
      continue;
    }
    if (ch === '"') { inStr = true; stripped += ch; i++; continue; }
    if (ch === "/" && raw[i + 1] === "/") {
      i += 2;
      while (i < n && raw[i] !== "\n") i++; // stop at (do not consume) the newline
      continue;
    }
    if (ch === "/" && raw[i + 1] === "*") {
      i += 2;
      while (i < n && !(raw[i] === "*" && raw[i + 1] === "/")) i++;
      i += 2; // consume the closing */
      continue;
    }
    stripped += ch;
    i++;
  }
  // Pass 2: drop a trailing comma — a `,` whose next non-whitespace char is } or ]
  // (comments are already gone). String-aware so a comma inside a value is kept.
  let out = "";
  inStr = false;
  const m = stripped.length;
  for (let j = 0; j < m; j++) {
    const ch = stripped[j]!;
    if (inStr) {
      out += ch;
      if (ch === "\\" && j + 1 < m) { out += stripped[j + 1]; j++; continue; }
      if (ch === '"') inStr = false;
      continue;
    }
    if (ch === '"') { inStr = true; out += ch; continue; }
    if (ch === ",") {
      let k = j + 1;
      while (k < m && (stripped[k] === " " || stripped[k] === "\t" || stripped[k] === "\r" || stripped[k] === "\n")) k++;
      if (k < m && (stripped[k] === "}" || stripped[k] === "]")) continue; // drop it
    }
    out += ch;
  }
  return out;
}

/**
 * Extract ONLY the shape-valid packages from a repo's devbox.json. A missing,
 * unreadable, or malformed file yields []. NOTHING in the file is executed —
 * init_hook/scripts/flakes/other keys are ignored. Reached only when the owner
 * opted in.
 */
export async function extractRepoDevboxPackages(worktreePath: string): Promise<string[]> {
  const file = path.join(worktreePath, "devbox.json");
  let raw: string;
  try {
    // Guard BEFORE reading the whole file. stat() follows symlinks, so require a
    // regular file too: a symlink to a device/FIFO (e.g. /dev/zero) reports size 0
    // and would otherwise pass the size check and hang readFile on an unbounded
    // stream. An oversized regular file is rejected without ever loading it.
    const st = await fs.stat(file);
    if (!st.isFile() || st.size > MAX_DEVBOX_BYTES) return [];
    raw = await fs.readFile(file, "utf8");
  } catch {
    return [];
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(stripJsonComments(raw));
  } catch {
    return [];
  }

  const pkgs = (parsed as { packages?: unknown } | null)?.packages;
  const out: string[] = [];
  const add = (s: string): void => {
    if (s.length <= MAX_PKG_LEN && PKG_RE.test(s)) out.push(s);
  };

  if (Array.isArray(pkgs)) {
    // ["python@3.10", "hello", "github:NixOS/nixpkgs#foo"] — the flake ref is dropped
    // by the shape filter.
    for (const p of pkgs) if (typeof p === "string") add(p);
  } else if (pkgs && typeof pkgs === "object") {
    // {"python": {"version":"3.10"}, "hello": "latest"} — take name (+ version when
    // it is a simple string).
    for (const [name, val] of Object.entries(pkgs as Record<string, unknown>)) {
      let version = "";
      if (typeof val === "string") version = val;
      else if (val && typeof val === "object" && typeof (val as { version?: unknown }).version === "string") {
        version = (val as { version: string }).version;
      }
      add(version ? `${name}@${version}` : name);
    }
  }

  return [...new Set(out)].slice(0, MAX_REPO_PACKAGES);
}

/**
 * Union-merge tier-1 (authoritative) with tier-2 (repo) packages: tier-1 wins a
 * version conflict on the same base name, so a tier-2 entry whose base name tier-1
 * already provides is dropped. Preserves tier-1 order, then appends the surviving
 * tier-2 entries.
 */
export function mergeToolPackages(tier1: string[], tier2: string[]): string[] {
  const seenBases = new Set(tier1.map(baseName));
  const merged = [...tier1];
  for (const p of tier2) {
    const base = baseName(p);
    // Skip a base tier-1 already provides AND a base an earlier tier-2 entry
    // already contributed, so two tier-2 versions of one package never both
    // survive (first tier-2 wins the intra-tier-2 conflict).
    if (seenBases.has(base)) continue;
    seenBases.add(base);
    merged.push(p);
  }
  return merged;
}
