// PRD #1798 M1 (D3): the deterministic, repo-generic size line of a merge-request body.
//
//   **Size:** code +A −D · tests +A −D · docs +A −D · config +A −D · generated +A −D · vendored +A −D · N files
//
// Every number comes from git, never from a model: the NUL-delimited numstat of
// `merge-base(target, head)..head` (renames by NEW path, deletions by OLD path, binaries count as a
// file with 0 lines), bucketed by the repository's own linguist attributes at `head` and, where those
// say nothing, by the generic path rules of D3. No repo-specific rule lives here: a repo that wants its
// generated or vendored code classified says so in its `.gitattributes` (uzi does, for `*.sql.go` and
// the uzidocs mirror).
//
// The module is split so the classification is pure and table-testable (`classifyPath`,
// `parseNumstatZ`, `renderSizeLine`), while the git I/O goes through a narrow interface
// (`SizeLineGit`, implemented by GitCache) and an injectable runner (`lookupAttributes`) so the
// "--source unsupported" fallback can be simulated in a test.

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Logger } from "./log.js";
import type { PrDescriptionSize } from "./protocol.js";

export type SizeBucket = "code" | "tests" | "docs" | "config" | "generated" | "vendored";

/** Render order of the buckets (D3). */
const SIZE_BUCKETS: readonly SizeBucket[] = ["code", "tests", "docs", "config", "generated", "vendored"];

/** The three attributes D3 reads, in the order `git check-attr` is asked for them. */
export const SIZE_ATTRIBUTES = ["linguist-generated", "linguist-documentation", "linguist-vendored"] as const;
export type SizeAttribute = (typeof SIZE_ATTRIBUTES)[number];

/** Per-path attribute info exactly as `git check-attr` reports it: `set`, `unset`, `unspecified`, or
 *  the attribute's value (e.g. `true` / `false`). A missing key is treated as `unspecified`. */
export type PathAttributes = Partial<Record<SizeAttribute, string>>;

/** The rendered line when the size cannot be computed faithfully (D3, reviewer revision 1): buckets
 *  that ignore the repository's attributes are never published. */
export const SIZE_UNAVAILABLE = "**Size:** unavailable";

type AttrVerdict = "in" | "out" | "fallthrough";

/** D3: `set` or value `true` → in the bucket; `unset` (`-attr`) or value `false` → explicitly NOT in
 *  it (the bucket's path rule is skipped); anything else (`unspecified`, including `!attr`, or an
 *  unrelated value) → fall through to the path rules. */
function attrVerdict(info: string | undefined): AttrVerdict {
  if (info === "set" || info === "true") return "in";
  if (info === "unset" || info === "false") return "out";
  return "fallthrough";
}

const TEST_SEGMENTS = new Set(["tests", "test", "__tests__", "spec"]);
const DOC_SEGMENTS = new Set(["docs", "doc"]);
const CONFIG_SEGMENTS = new Set([".github", ".forgejo"]);
const LOCKFILES = new Set([
  "package-lock.json",
  "yarn.lock",
  "pnpm-lock.yaml",
  "go.sum",
  "Cargo.lock",
  "poetry.lock",
  "uv.lock",
  "Gemfile.lock",
]);

function isTestPath(base: string, dirs: readonly string[]): boolean {
  return (
    base.endsWith("_test.go") ||
    /\.test\./.test(base) ||
    /\.spec\./.test(base) ||
    /^test_.*\.py$/.test(base) ||
    base.endsWith("_test.py") ||
    base.endsWith("Test.java") ||
    base.endsWith("_spec.rb") ||
    dirs.some((d) => TEST_SEGMENTS.has(d))
  );
}

function isDocPath(base: string, dirs: readonly string[]): boolean {
  return /\.(md|mdx|rst|adoc)$/.test(base) || dirs.some((d) => DOC_SEGMENTS.has(d));
}

function isGeneratedPath(base: string): boolean {
  return (
    LOCKFILES.has(base) ||
    base.endsWith(".pb.go") ||
    /_generated\./.test(base) ||
    /\.gen\./.test(base) ||
    base.endsWith(".min.js") ||
    base.endsWith(".snap")
  );
}

function isConfigPath(base: string, dirs: readonly string[]): boolean {
  return (
    /\.(yml|yaml|toml|json|ini)$/.test(base) ||
    base.startsWith("Dockerfile") ||
    base === "Makefile" ||
    base === "Taskfile.yml" ||
    base === ".gitlab-ci.yml" ||
    dirs.some((d) => CONFIG_SEGMENTS.has(d))
  );
}

/**
 * PRD #1798 M1 (D3): the bucket of one repo-relative path, given its linguist attributes.
 *
 * 1. Attributes first, precedence generated > vendored > documentation: the first one that is `set`
 *    or `true` wins.
 * 2. Otherwise the generic path rules, first match wins: tests, docs, generated, config, else code.
 *    A docs or generated rule is SKIPPED for a path whose matching attribute is explicitly `unset`
 *    or `false` (vendored has no path rule, so its explicit "not" changes nothing here).
 *
 * Path patterns match the basename (`*_test.go`, `*.md`, lockfiles, ...) or any DIRECTORY segment
 * (`tests/`, `docs/`, `.github/`, ...), case-sensitively as written in D3.
 */
export function classifyPath(filePath: string, attrs: PathAttributes = {}): SizeBucket {
  const generated = attrVerdict(attrs["linguist-generated"]);
  const vendored = attrVerdict(attrs["linguist-vendored"]);
  const documentation = attrVerdict(attrs["linguist-documentation"]);
  if (generated === "in") return "generated";
  if (vendored === "in") return "vendored";
  if (documentation === "in") return "docs";

  const parts = filePath.split("/").filter((p) => p !== "");
  const base = parts.pop() ?? "";
  const dirs = parts;
  if (isTestPath(base, dirs)) return "tests";
  if (documentation !== "out" && isDocPath(base, dirs)) return "docs";
  if (generated !== "out" && isGeneratedPath(base)) return "generated";
  if (isConfigPath(base, dirs)) return "config";
  return "code";
}

/** One file of the diff. `path` is the path the file is classified by: the NEW path of a rename,
 *  the OLD path of a deletion (which numstat reports as the only path). */
export interface NumstatEntry {
  path: string;
  added: number;
  deleted: number;
  binary: boolean;
}

function parseCount(raw: string, record: string): number {
  if (raw === "-") return 0;
  if (!/^\d+$/.test(raw)) throw new Error(`malformed numstat count ${JSON.stringify(raw)} in ${JSON.stringify(record)}`);
  return Number(raw);
}

/**
 * Parse `git diff -z --numstat -M` output. A plain record is `A\tD\tpath\0`; a rename or copy is
 * `A\tD\t\0old\0new\0` (the empty path field announces the two NUL-terminated paths). A binary file
 * reports `-\t-`: it counts as a file with 0 lines. Malformed output throws, so the caller renders the
 * line as unavailable rather than publishing a guess.
 */
export function parseNumstatZ(out: string): NumstatEntry[] {
  const tokens = out.split("\0");
  const entries: NumstatEntry[] = [];
  let i = 0;
  while (i < tokens.length) {
    const record = tokens[i++]!;
    // A trailing NUL leaves one empty token at the end; any other empty record is malformed.
    if (record === "") {
      if (i === tokens.length) break;
      throw new Error("malformed numstat output: empty record");
    }
    const t1 = record.indexOf("\t");
    const t2 = t1 < 0 ? -1 : record.indexOf("\t", t1 + 1);
    if (t1 < 0 || t2 < 0) throw new Error(`malformed numstat record ${JSON.stringify(record)}`);
    const rawAdded = record.slice(0, t1);
    const rawDeleted = record.slice(t1 + 1, t2);
    const binary = rawAdded === "-" && rawDeleted === "-";
    const added = parseCount(rawAdded, record);
    const deleted = parseCount(rawDeleted, record);
    let filePath = record.slice(t2 + 1);
    if (filePath === "") {
      // Rename/copy: old path, then new path. Classified by the NEW path (D3).
      const oldPath = tokens[i++];
      const newPath = tokens[i++];
      if (!oldPath || !newPath) throw new Error("malformed numstat output: truncated rename record");
      filePath = newPath;
    }
    entries.push({ path: filePath, added, deleted, binary });
  }
  return entries;
}

/**
 * Parse `git check-attr -z` output: repeated `path\0attribute\0info\0` triples. Returns the
 * attribute info per path (the path exactly as git echoes it back, which is the path it was fed).
 */
function parseCheckAttrZ(out: string): Map<string, PathAttributes> {
  const tokens = out.split("\0");
  // A well-formed output ends with a NUL, leaving one trailing empty token.
  if (tokens.length > 0 && tokens[tokens.length - 1] === "") tokens.pop();
  if (tokens.length % 3 !== 0) throw new Error("malformed check-attr output: not a sequence of triples");
  const result = new Map<string, PathAttributes>();
  for (let i = 0; i < tokens.length; i += 3) {
    const p = tokens[i]!;
    const attr = tokens[i + 1]! as SizeAttribute;
    const info = tokens[i + 2]!;
    if (!(SIZE_ATTRIBUTES as readonly string[]).includes(attr)) continue;
    const entry = result.get(p) ?? {};
    entry[attr] = info;
    result.set(p, entry);
  }
  return result;
}

interface BucketTotals {
  added: number;
  deleted: number;
  files: number;
}

const NUMBER_FORMAT = new Intl.NumberFormat("en-US");
const MINUS = "−";
const SEPARATOR = " · ";

/** Per-bucket totals of a diff: the one bucketing both the rendered line and the structured size
 *  read, so the two cannot disagree. */
function bucketTotals(entries: readonly NumstatEntry[], attrs: ReadonlyMap<string, PathAttributes>): Map<SizeBucket, BucketTotals> {
  const totals = new Map<SizeBucket, BucketTotals>();
  for (const e of entries) {
    const bucket = classifyPath(e.path, attrs.get(e.path));
    const t = totals.get(bucket) ?? { added: 0, deleted: 0, files: 0 };
    t.added += e.added;
    t.deleted += e.deleted;
    t.files += 1;
    totals.set(bucket, t);
  }
  return totals;
}

/** A size with every count zero. `zeroSize(true)` is the size stored for "**Size:** unavailable"
 *  (the api refuses a non-zero bucket beside `unavailable`). */
function zeroSize(unavailable: boolean): PrDescriptionSize {
  const z = () => ({ added: 0, deleted: 0 });
  return { unavailable, files: 0, code: z(), tests: z(), docs: z(), config: z(), generated: z(), vendored: z() };
}

/**
 * PRD #1798 D9: the structured size (the api's `PrDescriptionSize`) of the same diff
 * {@link renderSizeLine} renders, from the same bucketing. `files` counts every file (binaries
 * included); each bucket carries its line counts, zero when no file landed in it. An empty diff is
 * `files: 0` with every bucket zero.
 */
export function sizeTotals(entries: readonly NumstatEntry[], attrs: ReadonlyMap<string, PathAttributes>): PrDescriptionSize {
  const size = zeroSize(false);
  size.files = entries.length;
  for (const [bucket, t] of bucketTotals(entries, attrs)) size[bucket] = { added: t.added, deleted: t.deleted };
  return size;
}

/**
 * Render the size line (D3). A bucket is omitted when it has no files at all; a bucket whose only
 * files are binaries (0 lines) still renders as `+0 −0` so the file total adds up. Numbers use en-US
 * thousands grouping ("1,810"), deletions use U+2212 MINUS SIGN, and parts are joined by " · ".
 * The trailing count reads "1 file" / "N files". Returns null for an empty diff (no line at all).
 */
export function renderSizeLine(entries: readonly NumstatEntry[], attrs: ReadonlyMap<string, PathAttributes>): string | null {
  if (entries.length === 0) return null;
  const totals = bucketTotals(entries, attrs);
  const parts: string[] = [];
  for (const bucket of SIZE_BUCKETS) {
    const t = totals.get(bucket);
    if (!t) continue; // no file landed in this bucket
    parts.push(`${bucket} +${NUMBER_FORMAT.format(t.added)} ${MINUS}${NUMBER_FORMAT.format(t.deleted)}`);
  }
  const n = entries.length;
  parts.push(`${NUMBER_FORMAT.format(n)} ${n === 1 ? "file" : "files"}`);
  return `**Size:** ${parts.join(SEPARATOR)}`;
}

/** A git invocation for the attribute lookup: `args` after `git -C <bare>`, optional NUL-joined stdin,
 *  and an optional `GIT_INDEX_FILE`. Resolves to stdout; rejects with an error carrying git's `stderr`
 *  (the `--source`-unsupported detection reads it). */
export type AttrGitRunner = (args: string[], opts: { input?: string; indexFile?: string }) => Promise<string>;

/** True when a `check-attr --source` failure means THIS git does not know `--source` (git < 2.40):
 *  git exits non-zero with `error: unknown option `source=...'`, or prints a `usage:` block that does
 *  not mention `--source`. Any other failure is NOT this case and must not take the fallback. */
export function isSourceUnsupported(err: unknown): boolean {
  const e = err as { stderr?: unknown; message?: unknown };
  const stderr = typeof e.stderr === "string" ? e.stderr : typeof e.message === "string" ? e.message : "";
  if (/unknown option[^\n]*source/i.test(stderr)) return true;
  return /^\s*usage:/im.test(stderr) && !stderr.includes("--source");
}

/**
 * PRD #1798 M1 — the linguist attributes of `paths` as of commit `headSha`, in this exact order
 * (reviewer revision 1):
 *   (a) `git check-attr -z --stdin --source=<headSha> ...`, paths NUL-fed on stdin;
 *   (b) ONLY when (a) failed because `--source` is unsupported: a fresh temporary index under the
 *       worker's temp dir, populated with `read-tree <headSha>`, then `check-attr --cached` against
 *       it; the temp dir is removed in `finally` (never the bare repo's own index);
 *   (c) any other failure throws, so the caller renders the line unavailable.
 * `headSha` must be a commit SHA: git refuses a branch name for `--source`.
 */
export async function lookupAttributes(
  run: AttrGitRunner,
  headSha: string,
  paths: readonly string[],
  tmpRoot: string = os.tmpdir(),
): Promise<Map<string, PathAttributes>> {
  if (paths.length === 0) return new Map();
  if (!/^[0-9a-f]{40}$/.test(headSha)) throw new Error("attribute lookup needs a commit SHA");
  const input = paths.map((p) => `${p}\0`).join("");
  try {
    return parseCheckAttrZ(await run(["check-attr", "-z", "--stdin", `--source=${headSha}`, ...SIZE_ATTRIBUTES], { input }));
  } catch (err) {
    if (!isSourceUnsupported(err)) throw err;
  }
  const dir = await fs.mkdtemp(path.join(tmpRoot, "uzi-size-index-"));
  const indexFile = path.join(dir, "index");
  try {
    await run(["read-tree", headSha], { indexFile });
    return parseCheckAttrZ(await run(["check-attr", "-z", "--stdin", "--cached", ...SIZE_ATTRIBUTES], { input, indexFile }));
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
}

/** The git reads the size line needs (implemented by GitCache). */
export interface SizeLineGit {
  /** merge-base of the target branch (resolved as the bare's origin-tracking ref) and `headSha`. */
  sizeMergeBase(barePath: string, targetBranch: string, headSha: string): Promise<string>;
  /** raw `git diff -z --numstat -M <base> <head>` output. */
  diffNumstatZ(barePath: string, base: string, headSha: string): Promise<string>;
  /** linguist attributes of `paths` at `headSha` (see {@link lookupAttributes}). */
  checkAttrZ(barePath: string, headSha: string, paths: readonly string[]): Promise<Map<string, PathAttributes>>;
}

/** The size line and the same numbers as the api's structured size (PRD #1798 D9). */
export interface ComputedSize {
  /** The rendered line; null only for an empty diff. */
  line: string | null;
  /** The structured totals: `unavailable: true` with every count zero when `line` is
   *  {@link SIZE_UNAVAILABLE}, `files: 0` with every bucket zero for an empty diff. */
  size: PrDescriptionSize;
}

/**
 * PRD #1798 M1 / D9 — the size line for the landed `headSha` against `targetBranch` (null only for an
 * empty diff) plus the structured size the stage request carries, both from one git read and one
 * bucketing. Never throws: any failure (merge-base, numstat, attribute lookup, or a numstat path
 * absent from the attribute lookup's result) is logged at warn and yields
 * `{ line: SIZE_UNAVAILABLE, size: unavailable }`, so the size line never fails a run.
 */
export async function computeSize(
  git: SizeLineGit,
  barePath: string,
  targetBranch: string,
  headSha: string | null,
  log?: Pick<Logger, "warn">,
): Promise<ComputedSize> {
  try {
    if (!headSha || !/^[0-9a-f]{40}$/.test(headSha)) throw new Error("landed head is not a commit SHA");
    const base = await git.sizeMergeBase(barePath, targetBranch, headSha);
    const entries = parseNumstatZ(await git.diffNumstatZ(barePath, base, headSha));
    if (entries.length === 0) return { line: null, size: sizeTotals(entries, new Map()) };
    const paths = [...new Set(entries.map((e) => e.path))];
    const attrs = await git.checkAttrZ(barePath, headSha, paths);
    // A path git did not report attributes for would otherwise be classified by path rules alone:
    // never publish buckets computed without attributes (D3, reviewer revision 1).
    const missing = paths.filter((p) => !attrs.has(p));
    if (missing.length > 0) {
      throw new Error(`check-attr reported no attributes for ${missing.length} of ${paths.length} path(s), e.g. ${JSON.stringify(missing[0])}`);
    }
    return { line: renderSizeLine(entries, attrs), size: sizeTotals(entries, attrs) };
  } catch (err) {
    log?.warn("PR size line unavailable", { error: err instanceof Error ? err.message : String(err) });
    return { line: SIZE_UNAVAILABLE, size: zeroSize(true) };
  }
}
