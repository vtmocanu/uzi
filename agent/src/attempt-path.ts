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

/** `<canonicalPath>.attempt-<attemptId>`: the clone path of one execution attempt of a key
 *  (issue #1783 M2). Throws on a malformed id, so no caller can mint a path the parser would
 *  refuse. */
export function attemptClonePath(canonicalPath: string, attemptId: string): string {
  if (!ATTEMPT_ID_RE.test(attemptId)) throw new Error("malformed attempt id");
  return `${canonicalPath}${ATTEMPT_SEPARATOR}${attemptId}`;
}

/** The generation of an attempt id as a number (`gx`, an unknown generation, is -1). */
function attemptGeneration(attemptId: string): number {
  const g = /-(g[0-9]+|gx)-/.exec(attemptId)?.[1] ?? "gx";
  return g === "gx" ? -1 : Number(g.slice(1));
}

/**
 * Order two attempt ids oldest first: by start timestamp (fixed width, so lexical), then by
 * claim generation NUMERICALLY (g10 after g9; `gx` lowest), then the whole id lexically as a
 * deterministic tie-break. The same order the watcher's backup script (`aid_newer`) uses.
 */
export function compareAttemptIds(a: string, b: string): number {
  const ta = a.slice(0, a.indexOf("-"));
  const tb = b.slice(0, b.indexOf("-"));
  if (ta !== tb) return ta < tb ? -1 : 1;
  const ga = attemptGeneration(a);
  const gb = attemptGeneration(b);
  if (ga !== gb) return ga - gb;
  return a < b ? -1 : a > b ? 1 : 0;
}

/** The two worker-owned sibling prefixes a runner repo dir may hold beside its clones: the
 *  skills plugin dir (skills-plugin.ts) and quarantined residue. */
const SKILLS_ARTIFACT_PREFIX = ".uzi-skills-";
const RESIDUE_ARTIFACT_PREFIX = ".uzi-residue-";

/**
 * True for a retained worker artifact beside the clones in a runner repo dir: a skills plugin
 * sibling (`.uzi-skills-<clone basename>`) or quarantined residue (`.uzi-residue-*`). Neither is
 * ever a clone: a clone key never starts with `.` (a git branch component cannot), and the git
 * layer refuses such a key before it seeds an attempt, so no key or attempt basename can collide.
 */
export function isRetainedArtifactName(name: string): boolean {
  return name.startsWith(SKILLS_ARTIFACT_PREFIX) || name.startsWith(RESIDUE_ARTIFACT_PREFIX);
}

/** A retained artifact's kind and the clone basename it belongs to (`<key>` or
 *  `<key>.attempt-<id>`), or undefined for any other name. */
export function parseRetainedArtifactName(
  name: string,
): { kind: "skills" | "residue"; cloneBasename: string } | undefined {
  for (const [kind, prefix] of [
    ["skills", SKILLS_ARTIFACT_PREFIX],
    ["residue", RESIDUE_ARTIFACT_PREFIX],
  ] as const) {
    if (!name.startsWith(prefix)) continue;
    const rest = name.slice(prefix.length);
    if (rest === "" || rest === "." || rest === ".." || rest.includes(path.sep)) return undefined;
    return { kind, cloneBasename: rest };
  }
  return undefined;
}

/** The clone key and attempt id (when any) a clone BASENAME names: `<key>.attempt-<id>` parses
 *  to both; anything else is taken as a canonical key with no attempt. */
export function parseCloneBasename(base: string): { key: string; attemptId: string | undefined } {
  const m = ATTEMPT_BASENAME_RE.exec(base);
  if (m?.groups) return { key: m.groups.key!, attemptId: m.groups.id! };
  return { key: base, attemptId: undefined };
}
