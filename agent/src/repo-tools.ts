// Tier-2 repo devbox.json packages (PRD #18 M5, Decisions 3 + 4).
//
// When (and ONLY when) the repo owner has opted in (repo_devbox_opt_in), the worker
// may union the packages declared in the cloned repo's own devbox.json with the
// tier-1 profile. This is the "extract the safe fields, drop the rest" discipline:
//
//   - ONLY the `packages` field is read; shell.init_hook, shell.scripts, env, flake
//     references, and every other key are ignored and NEVER executed. Extraction is
//     pure JSON parsing — no shell, no `devbox` invocation on the repo file.
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

/** The base package name (before any @version). */
function baseName(pkg: string): string {
  const at = pkg.indexOf("@");
  return at >= 0 ? pkg.slice(0, at) : pkg;
}

/**
 * Extract ONLY the shape-valid packages from a repo's devbox.json. A missing,
 * unreadable, or malformed file yields []. NOTHING in the file is executed —
 * init_hook/scripts/flakes/other keys are ignored. Reached only when the owner
 * opted in.
 */
export async function extractRepoDevboxPackages(worktreePath: string): Promise<string[]> {
  let raw: string;
  try {
    raw = await fs.readFile(path.join(worktreePath, "devbox.json"), "utf8");
  } catch {
    return [];
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
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
  const tier1Bases = new Set(tier1.map(baseName));
  const merged = [...tier1];
  for (const p of tier2) {
    if (!tier1Bases.has(baseName(p))) merged.push(p);
  }
  return merged;
}
