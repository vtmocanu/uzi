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

/** issue #1783 — the residue uuid: a lowercase RFC 4122 version-4 uuid (the only form
 *  `crypto.randomUUID()` mints), hex and hyphen only. */
const RESIDUE_UUID_SOURCE = "[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}";
const RESIDUE_UUID_RE = new RegExp(`^${RESIDUE_UUID_SOURCE}$`);
/** The whole residue name, anchored at BOTH ends: `.uzi-residue-<key>.residue-<uuid>`. The key is
 *  greedy, so the LAST `.residue-` followed by a complete uuid that ends the name is the separator
 *  (a uuid cannot contain `.residue-`, so formatting then parsing always round-trips). */
const RESIDUE_NAME_RE = new RegExp(`^\\.uzi-residue-(?<key>.+)\\.residue-(?<uuid>${RESIDUE_UUID_SOURCE})$`);

/** True when `s` holds a character no residue name may carry: a C0 or C1 control (newline
 *  included), DEL, or the U+2028/U+2029 line/paragraph separators. */
function hasControlChar(s: string): boolean {
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x2028 || c === 0x2029) return true;
  }
  return false;
}

/** A clone key a residue name may carry: non-empty, no path separator, not `.`/`..`, not
 *  dot-leading (a key never is: a git branch component cannot be), no control character, and
 *  not itself an attempt basename (residue belongs to a KEY, never to one attempt of it). */
function validResidueKey(key: string): boolean {
  return (
    key !== "" &&
    !key.startsWith(".") &&
    !key.includes("/") &&
    !key.includes(path.sep) &&
    !hasControlChar(key) &&
    !ATTEMPT_BASENAME_RE.test(key)
  );
}

/**
 * issue #1783 — the ONE quarantined-residue name, pinned here for M3 to produce:
 * `.uzi-residue-<key>.residue-<uuid>`, `<uuid>` a lowercase v4 uuid (see {@link RESIDUE_UUID_SOURCE}).
 * Residue is un-journaled by definition and carries no run id; the retention sweep keeps at most a
 * fixed number per key, oldest first by the residue directory's own mtime (see git.ts). Throws on a
 * key or uuid the parser would refuse, so no caller can mint a name {@link parseRetainedArtifactName}
 * does not read back as exactly `(key, uuid)`.
 */
export function formatResidueName(key: string, uuid: string): string {
  if (!validResidueKey(key)) throw new Error("refusing an unsafe residue key");
  if (!RESIDUE_UUID_RE.test(uuid)) throw new Error("residue uuid must be a lowercase v4 uuid");
  return `${RESIDUE_ARTIFACT_PREFIX}${key}.residue-${uuid}`;
}

/** A parsed retained artifact: a skills sibling names the clone basename it belongs to (`<key>` or
 *  `<key>.attempt-<id>`); residue names its key and uuid. */
export type RetainedArtifact =
  | { kind: "skills"; cloneBasename: string }
  | { kind: "residue"; key: string; uuid: string };

/** A retained artifact's parse, or undefined for any other name — including every `.uzi-residue-*`
 *  name that is not EXACTLY the pinned grammar (an older or foreign shape, an uppercase or non-v4
 *  uuid, trailing text, a control character or newline anywhere, an unsafe key). Such a name is
 *  still never a clone ({@link isRetainedArtifactName}); the sweep just never counts or deletes it. */
export function parseRetainedArtifactName(name: string): RetainedArtifact | undefined {
  if (name.startsWith(SKILLS_ARTIFACT_PREFIX)) {
    const rest = name.slice(SKILLS_ARTIFACT_PREFIX.length);
    if (rest === "" || rest === "." || rest === ".." || rest.includes(path.sep)) return undefined;
    return { kind: "skills", cloneBasename: rest };
  }
  if (!name.startsWith(RESIDUE_ARTIFACT_PREFIX) || hasControlChar(name)) return undefined;
  const m = RESIDUE_NAME_RE.exec(name);
  if (!m?.groups) return undefined;
  const key = m.groups.key!;
  if (!validResidueKey(key)) return undefined;
  return { kind: "residue", key, uuid: m.groups.uuid! };
}
