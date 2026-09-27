// issue #1783 — the shared grammar for a run's clone paths and execution attempts.
//
// Two facts are needed by the run-quiescence reaper now (M1) and by the per-attempt clone
// seeding later (M2), so they live in one place:
//
//   - WHOLE-COMPONENT path containment. A string prefix test is wrong here: `/x/issue-17`
//     is a prefix of `/x/issue-1769`, and treating a sibling run's clone as "inside" ours
//     would let one run's teardown reach another's processes and containers.
//   - The ATTEMPT id and the `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>` clone path
//     grammar. The id is sortable by time, names the claim generation it ran under, and
//     carries 64 random bits, so two attempts of one run can never collide.

import path from "node:path";

/** The separator between a clone key and its attempt id in an attempt clone's basename. */
export const ATTEMPT_SEPARATOR = ".attempt-";

/** `<UTC yyyymmddThhmmssZ>-g<generation>|gx-<16 lowercase hex>`. */
export const ATTEMPT_ID_RE = /^[0-9]{8}T[0-9]{6}Z-(g[0-9]+|gx)-[0-9a-f]{16}$/;

/** Anchored basename grammar for an attempt clone: `<key>.attempt-<attemptId>`. The key is
 *  greedy, so the LAST `.attempt-` that is followed by a complete id is the separator. */
const ATTEMPT_BASENAME_RE =
  /^(?<key>.+)\.attempt-(?<id>[0-9]{8}T[0-9]{6}Z-(?:g[0-9]+|gx)-[0-9a-f]{16})$/;

/**
 * True when `child` is `root` itself or lies below it, compared by WHOLE path components.
 * Both sides are resolved first, so `..` segments and trailing separators cannot fool it.
 * `/x/issue-17` never contains `/x/issue-1769`, and the reverse holds too.
 */
export function isWithinPath(child: string, root: string): boolean {
  const rel = path.relative(path.resolve(root), path.resolve(child));
  if (rel === "") return true;
  if (path.isAbsolute(rel)) return false;
  const first = rel.split(path.sep)[0];
  return first !== "..";
}

/**
 * Format an attempt id: the UTC second it started, the claim generation it ran under (`gx`
 * when the claim carried none), and 16 lowercase hex characters of randomness. The caller
 * supplies the randomness so the format is testable; it must be 16 hex characters.
 */
export function formatAttemptId(now: Date, claimGeneration: number | undefined, randomHex16: string): string {
  if (!/^[0-9a-f]{16}$/.test(randomHex16)) {
    throw new Error("attempt id randomness must be 16 lowercase hex characters");
  }
  const pad = (n: number, w = 2) => String(n).padStart(w, "0");
  const stamp =
    `${pad(now.getUTCFullYear(), 4)}${pad(now.getUTCMonth() + 1)}${pad(now.getUTCDate())}` +
    `T${pad(now.getUTCHours())}${pad(now.getUTCMinutes())}${pad(now.getUTCSeconds())}Z`;
  const gen =
    claimGeneration !== undefined && Number.isInteger(claimGeneration) && claimGeneration >= 0
      ? `g${claimGeneration}`
      : "gx";
  return `${stamp}-${gen}-${randomHex16}`;
}

/** A parsed attempt clone path. */
export interface AttemptPath {
  repoDir: string;
  key: string;
  attemptId: string;
}

/**
 * Parse `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>`. Anything else is rejected
 * (returns undefined): a path outside runnerRoot, one with more or fewer than two components
 * below it, a `..`/`.` component, or a basename that does not match the anchored grammar.
 */
export function parseAttemptPath(p: string, runnerRoot: string): AttemptPath | undefined {
  if (!path.isAbsolute(p) || !path.isAbsolute(runnerRoot)) return undefined;
  // Refuse a non-normal input outright rather than normalizing it into something valid.
  if (path.normalize(p) !== p || p.endsWith(path.sep)) return undefined;
  const rel = path.relative(path.resolve(runnerRoot), p);
  if (rel === "" || path.isAbsolute(rel)) return undefined;
  const parts = rel.split(path.sep);
  if (parts.length !== 2) return undefined;
  const [repoDir, base] = parts as [string, string];
  if (repoDir === "" || repoDir === "." || repoDir === "..") return undefined;
  const m = ATTEMPT_BASENAME_RE.exec(base);
  if (!m?.groups) return undefined;
  const key = m.groups.key!;
  const attemptId = m.groups.id!;
  if (key === "." || key === ".." || key.includes(path.sep)) return undefined;
  if (!ATTEMPT_ID_RE.test(attemptId)) return undefined;
  return { repoDir, key, attemptId };
}

/**
 * The stable `<repoDir>/<key>` identity of a runner clone path, for either shape: the
 * canonical `<runnerRoot>/<repoDir>/<key>` or an attempt clone `<…>/<key>.attempt-<id>`.
 * Also returns the key's canonical clone path (`<runnerRoot>/<repoDir>/<key>`), which is a
 * target path of every quiescence sweep for the key.
 */
export function cloneKeyOf(clonePath: string): { cloneKey: string; canonicalPath: string } {
  const resolved = path.resolve(clonePath);
  const parent = path.dirname(resolved);
  const base = path.basename(resolved);
  const m = ATTEMPT_BASENAME_RE.exec(base);
  const key = m?.groups?.key ?? base;
  return { cloneKey: `${path.basename(parent)}/${key}`, canonicalPath: path.join(parent, key) };
}
