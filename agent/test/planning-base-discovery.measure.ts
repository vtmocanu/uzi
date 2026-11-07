// Issue #3543 opt-in measurement of plan cross-check base discovery (not a *.test.ts, so
// `npm test` and every gate never run it). Drive it through
// e2e/planning-base-discovery-measure/run.sh, which runs each job in a memory-limited container:
//
//   UZI_PLANNING_DISCOVERY_MEASURE=1 node --import tsx test/planning-base-discovery.measure.ts job <job.json>
//
// Job kinds: `generate` builds one deterministic fixture, `measure` runs one capture through the
// production GitCache.capturePlanningDiff path, `report` renders the results table, `model`
// prints the analytic entry/path-byte charge totals for a spec without docker or the opt-in env
// (`node --import tsx test/planning-base-discovery.measure.ts '{"kind":"model","spec":"chain-d64"}'`;
// `"at":[3000,2999]` also prints Em/Pm per index and the 7f probe cap pairs). The
// measured helper is the unchanged PLANNING_CAPTURE_HELPER; instrumented modes only PREPEND a
// metering preamble to its `-e` program (the technique git-planning-diff.test.ts's meter uses).
// `verify` (`{"kind":"verify","out":"/abs/results","rows":[{"label":"...","expect":"completed|phase-evidence|<refusal token>"}]}`,
// runnable on the host or in a container) re-reads each row's result and term records, writes
// verify.json and exits nonzero unless every row is valid. `deadlineLift` (validation only, modes
// full and control) lifts the helper's wall-clock bounds in the spawned helper source; see
// planning-deadline-lift.ts. Nothing here changes a production bound. See the README beside
// run.sh for metric definitions.

import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, classifyPlanningCaptureError, gitEnv } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import type { Logger } from "../src/log.js";
import { TickSpawner } from "../src/tick-spawner.js";
import {
  LIFTED_COMMAND_TIMER_MS, LIFTED_HELPER_DEADLINE_MS, LIFTED_OUTER_TIMEOUT_MS, LIFTED_SCOPE_TIMEOUT_MS, PRODUCTION_OUTER_TIMEOUT_MS,
  emptyPhases, isGitArgv, isSizingArgv, liftPlanningHelperDeadline, phaseEvidenceOf, phaseObserverMismatches, verifyRow,
} from "./planning-deadline-lift.js";
import type { Phases } from "./planning-deadline-lift.js";

// The opt-in gate (UZI_PLANNING_DISCOVERY_MEASURE=1) is enforced in main() for every job kind
// except the pure, read-only `model` and `verify` kinds.

// ---------------------------------------------------------------------------------------------
// Fixture generation (deterministic: pinned identities and dates, plumbing-built objects).

const PINNED_ENV: NodeJS.ProcessEnv = {
  ...gitEnv(),
  GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid", GIT_AUTHOR_DATE: "2026-01-01T00:00:00Z",
  GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid", GIT_COMMITTER_DATE: "2026-01-01T00:00:00Z",
};

function git(cwd: string, args: string[], input?: string): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: PINNED_ENV, input, encoding: "utf8", maxBuffer: 256 * 1024 * 1024, stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}

interface Manifest {
  spec: string;
  kind: string;
  description: string;
  commit: string;
  tree: string;
  // Decimal strings: 2^65 - 1 overflows a JS number.
  expectedTreeOccurrences: string;
  expectedLeafOccurrences: string;
  // Analytic base-discovery charges (decimal strings), from the charge model below: every tree
  // entry (leaf or directory, the root's included) per walk occurrence, and the summed full
  // relative-path bytes of those entries. maxChargedPathBytes is the longest single entry's path.
  expectedEntryOccurrences: string;
  expectedPathBytes: string;
  maxChargedPathBytes: string;
  // distinct-* only: F*W distinct blobs (the sizing batch size). distinct-*, fatchain-*: the
  // largest tree object in bytes (the helper caps one object at 2 MiB).
  distinctBlobs?: string;
  maxTreeObjectBytes?: number;
  checkout: boolean;
  generationMs: number;
}

function layout(dir: string): { root: string; clone: string; bare: string } {
  const root = path.join(dir, "root");
  return { root, clone: path.join(root, "runner", "repo", "issue-1"), bare: path.join(root, "repos", "repo.git") };
}

// Build a tree DAG directly in `bare` from the bottom up and return its root tree id.
function mktree(bare: string, lines: string[]): string {
  // No lines is the empty tree; a lone blank line is a git mktree format error.
  return git(bare, ["mktree"], lines.length > 0 ? lines.join("\n") + "\n" : "");
}

// ---------------------------------------------------------------------------------------------
// Analytic charge model (never runs the helper). It enumerates tree entries in the helper's walk
// order: the root's entries in canonical git tree order (names sorted bytewise, a directory
// compared as name + "/"), descending into a directory as soon as its entry is met. Entry i
// (1-based) is charged
//   Em(i) = i                                  entry occurrences, and
//   Pm(i) = sum over entries 1..i of (bytes of its ancestors' "name/" prefix + its own name bytes),
// the planned base-discovery entry cap and path-byte cap charges (the entry cap is checked before
// the path charge of the same entry). A shared directory is charged once per occurrence.

interface ModelNode { entries: { nameLen: number; child?: ModelNode }[] }
// e: entry occurrences below (and in) the node. b: path bytes of that subtree at prefix length 0;
// at prefix length p the subtree charges e*p + b. m: longest single-entry path at prefix 0.
interface NodeStats { e: bigint; b: bigint; m: bigint }
const statsCache = new WeakMap<ModelNode, NodeStats>();
function nodeStats(n: ModelNode): NodeStats {
  const hit = statsCache.get(n);
  if (hit) return hit;
  let e = 0n, b = 0n, m = 0n;
  for (const en of n.entries) {
    const len = BigInt(en.nameLen);
    e += 1n; b += len; if (len > m) m = len;
    if (en.child) {
      const c = nodeStats(en.child);
      e += c.e; b += c.e * (len + 1n) + c.b;
      // An empty subtree (the w0 empty tree) has no entry whose path runs through the directory.
      if (c.e > 0n && len + 1n + c.m > m) m = len + 1n + c.m;
    }
  }
  const stats = { e, b, m };
  statsCache.set(n, stats);
  return stats;
}

interface ChargeModel {
  totalEntries: bigint;
  totalPathBytes: bigint;
  maxEntryBytes: bigint;
  // Pm(i), or null when the walk has fewer than i entries. Skips whole subtrees in closed form.
  prefixAt(i: bigint): bigint | null;
  // First k with Pm(k) > cap and Pm(k), or null when the total never exceeds cap.
  firstExceeding(cap: bigint): { k: bigint; p: bigint } | null;
  // Lazy (i, Pm(i)) stream in walk order; break out of the loop to stop early.
  stream(): Generator<{ i: bigint; p: bigint }>;
}

function dagModel(root: ModelNode): ChargeModel {
  const s = nodeStats(root);
  return {
    totalEntries: s.e, totalPathBytes: s.b, maxEntryBytes: s.m,
    prefixAt(i) {
      if (i > s.e) return null;
      let E = 0n, P = 0n;
      const visit = (n: ModelNode, p: bigint): boolean => {
        for (const en of n.entries) {
          if (E >= i) return true;
          const len = BigInt(en.nameLen);
          E += 1n; P += p + len;
          if (en.child) {
            const c = nodeStats(en.child), cp = p + len + 1n;
            if (E + c.e <= i) { E += c.e; P += c.e * cp + c.b; }
            else if (visit(en.child, cp)) return true;
          }
        }
        return E >= i;
      };
      visit(root, 0n);
      return P;
    },
    firstExceeding(cap) {
      let E = 0n, P = 0n;
      const visit = (n: ModelNode, p: bigint): { k: bigint; p: bigint } | null => {
        for (const en of n.entries) {
          const len = BigInt(en.nameLen);
          E += 1n;
          if (P + p + len > cap) return { k: E, p: P + p + len };
          P += p + len;
          if (en.child) {
            const c = nodeStats(en.child), cp = p + len + 1n;
            if (P + c.e * cp + c.b <= cap) { E += c.e; P += c.e * cp + c.b; }
            else { const r = visit(en.child, cp); if (r) return r; }
          }
        }
        return null;
      };
      return visit(root, 0n);
    },
    *stream() {
      let E = 0n, P = 0n;
      function* walk(n: ModelNode, p: bigint): Generator<{ i: bigint; p: bigint }> {
        for (const en of n.entries) {
          const len = BigInt(en.nameLen);
          E += 1n; P += p + len;
          yield { i: E, p: P };
          if (en.child) yield* walk(en.child, p + len + 1n);
        }
      }
      yield* walk(root, 0n);
    },
  };
}

// A real tree (repo, synthetic-N): `ls-tree -r -t` already lists entries in walk order, each with
// its full path, so the charge of entry i is simply that path's byte length.
function listingModel(pathBytes: number[]): ChargeModel {
  const n = pathBytes.length;
  const cum = Array.from({ length: n + 1 }, () => 0);
  let max = 0;
  for (let i = 0; i < n; i++) { cum[i + 1] = (cum[i] ?? 0) + (pathBytes[i] ?? 0); max = Math.max(max, pathBytes[i] ?? 0); }
  return {
    totalEntries: BigInt(n), totalPathBytes: BigInt(cum[n] ?? 0), maxEntryBytes: BigInt(max),
    prefixAt: (i) => (i > BigInt(n) ? null : BigInt(cum[Number(i)] ?? 0)),
    firstExceeding(cap) {
      for (let k = 1; k <= n; k++) if (BigInt(cum[k] ?? 0) > cap) return { k: BigInt(k), p: BigInt(cum[k] ?? 0) };
      return null;
    },
    *stream() { for (let i = 1; i <= n; i++) yield { i: BigInt(i), p: BigInt(cum[i] ?? 0) }; },
  };
}

function bareListingModel(bare: string, tree: string): ChargeModel {
  // Raw bytes: git() trims, and -z keeps unusual names unquoted.
  const out = execFileSync("git", ["-C", bare, "ls-tree", "-r", "-t", "-z", "--name-only", tree],
    { env: PINNED_ENV, maxBuffer: 1024 * 1024 * 1024, stdio: ["ignore", "pipe", "pipe"] });
  const lens: number[] = [];
  let at = 0;
  while (at < out.length) {
    const end = out.indexOf(0, at);
    if (end < 0) throw new Error("ls-tree -z output is not NUL terminated");
    lens.push(end - at);
    at = end + 1;
  }
  return listingModel(lens);
}

const OBJECT_CAP_BYTES = 2 * 1024 * 1024; // the helper's per-object METADATA_LIMIT

// longpath-d<depth>n<namelen>w<width>f<fan>. Depth convention: the root tree is helper depth 0
// (the helper refuses depth > 64 with directory_cap). The root holds `fan` directory entries, all
// naming one shared chain; the chain's deepest tree sits at helper depth `depth`. Along any path
// that is `depth` directories (the fan name, then depth-1 chain names), so 1 <= depth <= 64 and a
// deepest file path has depth+1 components (the helper allows 65). The deepest tree holds `width`
// regular files (`w0`: the empty tree). Every directory and file name is exactly `namelen` bytes:
// a letter (d fan, c chain, f file), the zero-padded index, then x fill.
interface Longpath { depth: number; namelen: number; width: number; fan: number; digits: number }
function parseLongpath(spec: string): Longpath | undefined {
  const m = /^longpath-d(\d{1,9})n(\d{1,9})w(\d{1,9})f(\d{1,9})$/.exec(spec);
  if (!m) return undefined;
  const [depth, namelen, width, fan] = [Number(m[1]), Number(m[2]), Number(m[3]), Number(m[4])];
  if (depth < 1 || depth > 64) throw new Error(`${spec}: depth must be 1..64 (root is depth 0; helper refuses depth > 64)`);
  if (fan < 1) throw new Error(`${spec}: fan must be >= 1`);
  const digits = String(Math.max(width, fan, depth) - 1).length;
  if (namelen < 1 + digits) throw new Error(`${spec}: namelen must be >= ${1 + digits} to hold a letter and a ${digits}-digit index`);
  // Tree objects: "40000 " + name + NUL + 20 per directory entry, "100644 " + ... per file.
  const rootBytes = fan * (namelen + 27), leafBytes = width * (namelen + 28), chainBytes = namelen + 27;
  for (const [label, bytes] of [["root", rootBytes], ["deepest", leafBytes], ["chain", chainBytes]] as const) {
    if (bytes > OBJECT_CAP_BYTES) throw new Error(`${spec}: ${label} tree object would be ${bytes} bytes, over the 2 MiB per-object cap`);
  }
  return { depth, namelen, width, fan, digits };
}
function lpName(letter: string, index: number, p: Longpath): string {
  return (letter + String(index).padStart(p.digits, "0")).padEnd(p.namelen, "x");
}

// distinct-<F>x<W>n<L>: the root holds F directories (`d000000`...), each pointing at its OWN leaf
// tree of W regular files whose names are exactly L bytes (`f`, the zero-padded index, then x
// fill) and whose blobs are all distinct across the fixture: F*W distinct blobs (contents are the
// global index and a newline), so the helper's sizing batch is maximal. Leaf trees are distinct
// objects too (distinct blob ids). Built with git fast-import.
interface Distinct { fan: number; width: number; namelen: number; digits: number }
const DISTINCT_DIR_NAME_LEN = 7;
function parseDistinct(spec: string): Distinct | undefined {
  const m = /^distinct-(\d{1,9})x(\d{1,9})n(\d{1,9})$/.exec(spec);
  if (!m) return undefined;
  const [fan, width, namelen] = [Number(m[1]), Number(m[2]), Number(m[3])];
  if (fan < 1 || fan > 1_000_000) throw new Error(`${spec}: F must be 1..1000000 (directory names are d + 6 digits)`);
  if (width < 1) throw new Error(`${spec}: W must be >= 1`);
  const digits = String(width - 1).length;
  if (namelen < 1 + digits) throw new Error(`${spec}: L must be >= ${1 + digits} to hold the letter and a ${digits}-digit index`);
  if (namelen > 255) throw new Error(`${spec}: L must be <= 255 (a single path component)`);
  // Tree objects: "40000 " + name + NUL + 20 per directory entry, "100644 " + ... per file.
  const rootBytes = fan * (DISTINCT_DIR_NAME_LEN + 27), leafBytes = width * (namelen + 28);
  for (const [label, bytes] of [["root", rootBytes], ["leaf", leafBytes]] as const) {
    if (bytes > OBJECT_CAP_BYTES) throw new Error(`${spec}: ${label} tree object would be ${bytes} bytes, over the 2 MiB per-object cap`);
  }
  return { fan, width, namelen, digits };
}
function distinctFileName(i: number, d: Distinct): string {
  return ("f" + String(i).padStart(d.digits, "0")).padEnd(d.namelen, "x");
}

// fatchain-d<depth>: a chain of `depth` nested directories (root = helper depth 0, deepest tree at
// depth `depth`, 1..64). Every tree holds FATCHAIN_FILLER regular files (one shared blob, names
// `f` + 5 digits, 6 bytes) and, except the deepest, a first-sorting directory entry named "0"
// pointing at the next tree. Each tree object is 28 + 34*FATCHAIN_FILLER bytes, just under 2 MiB.
// The helper descends at the first entry, so every ancestor's tree buffer is live at the deepest
// point while only depth entries have been charged.
const FATCHAIN_FILLER = 60000, FATCHAIN_NAME_LEN = 6, FATCHAIN_DIR_NAME = "0";
function parseFatchain(spec: string): { depth: number; treeBytes: number } | undefined {
  const m = /^fatchain-d(\d{1,9})$/.exec(spec);
  if (!m) return undefined;
  const depth = Number(m[1]);
  if (depth < 1 || depth > 64) throw new Error(`${spec}: depth must be 1..64 (root is depth 0; helper refuses depth > 64)`);
  const treeBytes = FATCHAIN_DIR_NAME.length + 27 + FATCHAIN_FILLER * (FATCHAIN_NAME_LEN + 28);
  if (treeBytes > OBJECT_CAP_BYTES) throw new Error(`${spec}: tree object would be ${treeBytes} bytes, over the 2 MiB per-object cap`);
  return { depth, treeBytes };
}
function fatchainFiller(): string[] {
  return Array.from({ length: FATCHAIN_FILLER }, (_, i) => "f" + String(i).padStart(FATCHAIN_NAME_LEN - 1, "0"));
}

// The shared-DAG fixtures' charge model, built from the spec alone.
function sharedModel(spec: string): ChargeModel | undefined {
  const dist = parseDistinct(spec);
  if (dist) {
    const leaf: ModelNode = { entries: Array.from({ length: dist.width }, () => ({ nameLen: dist.namelen })) };
    return dagModel({ entries: Array.from({ length: dist.fan }, () => ({ nameLen: DISTINCT_DIR_NAME_LEN, child: leaf })) });
  }
  const fat = parseFatchain(spec);
  if (fat) {
    // The filler entry objects are shared by every tree (the model only reads them).
    const filler = Array.from({ length: FATCHAIN_FILLER }, () => ({ nameLen: FATCHAIN_NAME_LEN }));
    let t: ModelNode = { entries: filler };
    for (let level = fat.depth - 1; level >= 0; level--) t = { entries: [{ nameLen: FATCHAIN_DIR_NAME.length, child: t }, ...filler] };
    return dagModel(t);
  }
  let m = /^fanout2-d(\d+)$/.exec(spec);
  if (m) {
    let t: ModelNode = { entries: [{ nameLen: 1 }] };
    for (let k = Number(m[1]) - 1; k >= 0; k--) t = { entries: [{ nameLen: 1, child: t }, { nameLen: 1, child: t }] };
    return dagModel(t);
  }
  m = /^chain-d(\d+)$/.exec(spec);
  if (m) {
    let t: ModelNode = { entries: [{ nameLen: 1 }] };
    for (let k = Number(m[1]) - 1; k >= 0; k--) t = { entries: [{ nameLen: 1, child: t }] };
    return dagModel(t);
  }
  m = /^wide-(\d+)x(\d+)$/.exec(spec);
  if (m) {
    const fan = Number(m[1]), width = Number(m[2]);
    const leaf: ModelNode = { entries: Array.from({ length: width }, () => ({ nameLen: 7 })) };
    return dagModel({ entries: Array.from({ length: fan }, () => ({ nameLen: 7, child: leaf })) });
  }
  const lp = parseLongpath(spec);
  if (lp) {
    let t: ModelNode = { entries: Array.from({ length: lp.width }, () => ({ nameLen: lp.namelen })) };
    for (let level = lp.depth - 1; level >= 1; level--) t = { entries: [{ nameLen: lp.namelen, child: t }] };
    const top = t;
    return dagModel({ entries: Array.from({ length: lp.fan }, () => ({ nameLen: lp.namelen, child: top })) });
  }
  return undefined;
}

function fixtureModel(spec: string, bare?: string, tree?: string): ChargeModel {
  const shared = sharedModel(spec);
  if (shared) return shared;
  if (spec === "repo" || /^synthetic-\d+$/.test(spec)) {
    if (!bare || !tree) throw new Error(`${spec}: the charge model reads the generated tree; pass a fixture dir`);
    return bareListingModel(bare, tree);
  }
  throw new Error(`unknown fixture spec ${spec}`);
}

const BASE_ENTRY_CAP = "baseDiscoveryEntryCap", BASE_PATH_BYTE_CAP = "baseDiscoveryPathByteCap";
const MAX_PROBE_CAP = 999_999_999_999_999n;
const DERIVED_BASIS = "derived-from-refusal (unverified until 7f probe)";

interface DerivedCharge { basis: string; token: string; entries: string; pathBytes: string; entryCap: string | null; pathByteCap: string | null }

// A cap refusal reveals which entry tripped it, so the charge at the refusal follows from the
// model: entry cap: E = cap+1 (refused before that entry's path charge), P = Pm(cap); path-byte
// cap: k = first i with Pm(i) > cap, E = k, P = Pm(k), and k must not exceed the entry cap.
function deriveRefusal(model: ChargeModel, token: string | undefined,
    entryCap: bigint | undefined, pathByteCap: bigint | undefined):
    { derived: DerivedCharge | null; reason?: string; assertionFailed?: boolean } {
  const caps = { entryCap: entryCap?.toString() ?? null, pathByteCap: pathByteCap?.toString() ?? null };
  if (token === "base_discovery_entry_cap") {
    if (entryCap === undefined) return { derived: null, reason: "entry cap value unknown for this job" };
    const p = model.prefixAt(entryCap);
    if (p === null || entryCap + 1n > model.totalEntries) {
      return { derived: null, reason: `fixture has ${model.totalEntries} entries, fewer than entry cap + 1`, assertionFailed: true };
    }
    if (pathByteCap !== undefined && p > pathByteCap) {
      return { derived: null, reason: `Pm(entryCap)=${p} already exceeds the path-byte cap, which would have refused first`, assertionFailed: true };
    }
    return { derived: { basis: DERIVED_BASIS, token, entries: (entryCap + 1n).toString(), pathBytes: p.toString(), ...caps } };
  }
  if (token === "base_discovery_path_byte_cap") {
    if (pathByteCap === undefined) return { derived: null, reason: "path-byte cap value unknown for this job" };
    const r = model.firstExceeding(pathByteCap);
    if (r === null) return { derived: null, reason: "fixture never exceeds the path-byte cap", assertionFailed: true };
    if (entryCap !== undefined && r.k > entryCap) {
      return { derived: null, reason: `k=${r.k} exceeds the entry cap ${entryCap}, which would have refused first`, assertionFailed: true };
    }
    return { derived: { basis: DERIVED_BASIS, token, entries: r.k.toString(), pathBytes: r.p.toString(), ...caps } };
  }
  return { derived: null, reason: "not a base-discovery cap refusal" };
}

interface SharedDag { tree: string; trees: bigint; leaves: bigint; description: string; distinctBlobs?: bigint; maxTreeObjectBytes?: number }

// Stream F*W distinct blobs and the F leaf trees through one `git fast-import` commit (fast-import
// builds the trees); the root tree is that commit's tree. The stream goes through a file so no
// 250 MB string is ever held.
function distinctDag(bare: string, spec: string, d: Distinct): SharedDag {
  const streamPath = path.join(path.dirname(bare), "distinct.fi");
  const fd = fs.openSync(streamPath, "w");
  const parts: string[] = [];
  let pending = 0;
  const flush = (): void => { fs.writeSync(fd, parts.join("")); parts.length = 0; pending = 0; };
  const push = (s: string): void => { parts.push(s); pending += s.length; if (pending >= 8 * 1024 * 1024) flush(); };
  try {
    const msg = "base\n";
    push(`commit refs/heads/distinct-fi\ncommitter Fixture <fixture@example.invalid> 1767225600 +0000\ndata ${msg.length}\n${msg}`);
    let global = 0;
    for (let f = 0; f < d.fan; f++) {
      const dir = `d${String(f).padStart(6, "0")}`;
      for (let w = 0; w < d.width; w++, global++) {
        const content = `${global}\n`;
        push(`M 100644 inline ${dir}/${distinctFileName(w, d)}\ndata ${content.length}\n${content}`);
      }
    }
    push("\n");
    flush();
  } finally { fs.closeSync(fd); }
  const input = fs.openSync(streamPath, "r");
  try {
    execFileSync("git", ["-C", bare, "fast-import", "--quiet", "--force"], { env: PINNED_ENV, stdio: [input, "pipe", "pipe"], maxBuffer: 64 * 1024 * 1024 });
  } finally {
    fs.closeSync(input);
    fs.rmSync(streamPath, { force: true });
  }
  const tree = git(bare, ["rev-parse", "refs/heads/distinct-fi^{tree}"]);
  git(bare, ["update-ref", "-d", "refs/heads/distinct-fi"]);
  const rootSize = Number(git(bare, ["cat-file", "-s", tree]));
  if (rootSize > OBJECT_CAP_BYTES) throw new Error(`${spec}: root tree is ${rootSize} bytes, over the 2 MiB per-object cap`);
  // Every leaf tree has the same size; check the first and the last one.
  let maxTree = rootSize;
  for (const f of [0, d.fan - 1]) {
    const size = Number(git(bare, ["cat-file", "-s", `${tree}:d${String(f).padStart(6, "0")}`]));
    if (size > OBJECT_CAP_BYTES) throw new Error(`${spec}: leaf tree ${f} is ${size} bytes, over the 2 MiB per-object cap`);
    maxTree = Math.max(maxTree, size);
  }
  const n = BigInt(d.fan) * BigInt(d.width);
  return { tree, trees: 1n + BigInt(d.fan), leaves: n, distinctBlobs: n, maxTreeObjectBytes: maxTree,
    description: `EXTRA (beyond spec): ${d.fan} root directories, each its own leaf tree of ${d.width} files with ${d.namelen}-byte names; ` +
      `${n} distinct blobs (maximal sizing batch)` };
}

function fatchainDag(bare: string, spec: string, fat: { depth: number; treeBytes: number }): SharedDag {
  const blob = git(bare, ["hash-object", "-w", "--stdin"], "leaf\n");
  const fillerLines = fatchainFiller().map((n) => `100644 blob ${blob}\t${n}`);
  let t = mktree(bare, fillerLines);
  let maxTree = Number(git(bare, ["cat-file", "-s", t]));
  for (let level = fat.depth - 1; level >= 0; level--) {
    t = mktree(bare, [`040000 tree ${t}\t${FATCHAIN_DIR_NAME}`, ...fillerLines]);
    maxTree = Math.max(maxTree, Number(git(bare, ["cat-file", "-s", t])));
  }
  if (maxTree > OBJECT_CAP_BYTES) throw new Error(`${spec}: a tree is ${maxTree} bytes, over the 2 MiB per-object cap`);
  // The dir entry must be first in canonical order: assert it from git's own listing.
  const first = git(bare, ["ls-tree", "--name-only", t]).split("\n")[0];
  if (first !== FATCHAIN_DIR_NAME) throw new Error(`${spec}: first root entry is ${JSON.stringify(first)}, expected ${FATCHAIN_DIR_NAME}`);
  return { tree: t, trees: BigInt(fat.depth + 1), leaves: BigInt(fat.depth + 1) * BigInt(FATCHAIN_FILLER), maxTreeObjectBytes: maxTree,
    description: `EXTRA (beyond spec): chain of ${fat.depth} nested directories ("${FATCHAIN_DIR_NAME}" sorts first), every tree ` +
      `${FATCHAIN_FILLER} filler files (largest tree ${maxTree} bytes, cap ${OBJECT_CAP_BYTES}); ancestor tree buffers all live at the descent` };
}

function sharedDag(bare: string, spec: string): SharedDag {
  const dist = parseDistinct(spec);
  if (dist) return distinctDag(bare, spec, dist);
  const fat = parseFatchain(spec);
  if (fat) return fatchainDag(bare, spec, fat);
  const blob = git(bare, ["hash-object", "-w", "--stdin"], "leaf\n");
  const lp = parseLongpath(spec);
  if (lp) {
    const checkSize = (oid: string, label: string): void => {
      const size = Number(git(bare, ["cat-file", "-s", oid]));
      if (size > OBJECT_CAP_BYTES) throw new Error(`${spec}: ${label} tree is ${size} bytes, over the 2 MiB per-object cap`);
    };
    let t = mktree(bare, Array.from({ length: lp.width }, (_, i) => `100644 blob ${blob}\t${lpName("f", i, lp)}`));
    checkSize(t, "deepest");
    for (let level = lp.depth - 1; level >= 1; level--) {
      t = mktree(bare, [`040000 tree ${t}\t${lpName("c", level, lp)}`]);
    }
    const root = mktree(bare, Array.from({ length: lp.fan }, (_, j) => `040000 tree ${t}\t${lpName("d", j, lp)}`));
    checkSize(root, "root");
    return { tree: root, trees: 1n + BigInt(lp.fan) * BigInt(lp.depth), leaves: BigInt(lp.fan) * BigInt(lp.width),
      description: `EXTRA (beyond spec): ${lp.fan} root directories share one chain, deepest tree at helper depth ${lp.depth} ` +
        `holding ${lp.width} files; every name ${lp.namelen} bytes` };
  }
  let m = /^fanout2-d(\d+)$/.exec(spec);
  if (m) {
    const depth = Number(m[1]);
    let t = mktree(bare, [`100644 blob ${blob}\tf`]);
    for (let k = depth - 1; k >= 0; k--) t = mktree(bare, [`040000 tree ${t}\ta`, `040000 tree ${t}\tb`]);
    return { tree: t, trees: (1n << BigInt(depth + 1)) - 1n, leaves: 1n << BigInt(depth),
      description: `binary shared DAG, root depth 0, deepest tree at depth ${depth}, ${depth + 1} distinct trees` };
  }
  m = /^chain-d(\d+)$/.exec(spec);
  if (m) {
    const depth = Number(m[1]);
    let t = mktree(bare, [`100644 blob ${blob}\tf`]);
    for (let k = depth - 1; k >= 0; k--) t = mktree(bare, [`040000 tree ${t}\ta`]);
    return { tree: t, trees: BigInt(depth + 1), leaves: 1n,
      description: `linear chain, deepest tree at depth ${depth}` };
  }
  m = /^wide-(\d+)x(\d+)$/.exec(spec);
  if (m) {
    const fan = Number(m[1]), width = Number(m[2]);
    const names = Array.from({ length: width }, (_, i) => `f${String(i).padStart(6, "0")}`);
    const leaf = mktree(bare, names.map((n) => `100644 blob ${blob}\t${n}`));
    const dirs = Array.from({ length: fan }, (_, j) => `d${String(j).padStart(6, "0")}`);
    const t = mktree(bare, dirs.map((d) => `040000 tree ${leaf}\t${d}`));
    return { tree: t, trees: BigInt(fan) + 1n, leaves: BigInt(fan) * BigInt(width),
      description: `EXTRA (beyond spec): one ${width}-entry leaf tree referenced ${fan} times` };
  }
  throw new Error(`unknown fixture spec ${spec}`);
}

function syntheticSize(i: number): number {
  return Number((BigInt(i) * 2654435761n) % 4096n);
}

function generate(spec: string, dir: string, inputs: string): Manifest {
  const started = Date.now();
  fs.rmSync(dir, { recursive: true, force: true });
  const { clone, bare } = layout(dir);
  fs.mkdirSync(path.dirname(clone), { recursive: true });
  fs.mkdirSync(path.dirname(bare), { recursive: true });
  let tree: string, trees: bigint, leaves: bigint, description: string, checkout: boolean;
  let extra: Pick<SharedDag, "distinctBlobs" | "maxTreeObjectBytes"> = {};
  if (spec === "repo" || /^synthetic-\d+$/.test(spec)) {
    const seed = path.join(dir, "seed");
    fs.mkdirSync(seed);
    git(seed, ["init", "--quiet", "--template="]);
    let expectedTree: string | undefined;
    if (spec === "repo") {
      execFileSync("tar", ["-xf", path.join(inputs, "repo.tar"), "-C", seed]);
      expectedTree = fs.readFileSync(path.join(inputs, "repo.tree"), "utf8").trim();
      description = `this repository at ${fs.readFileSync(path.join(inputs, "repo.sha"), "utf8").trim()}`;
    } else {
      const n = Number(spec.slice("synthetic-".length));
      for (let i = 0; i < n; i++) {
        const d = path.join(seed, `d${String(Math.floor(i / 100)).padStart(5, "0")}`);
        if (i % 100 === 0) fs.mkdirSync(d);
        const size = syntheticSize(i);
        const unit = `${i}\n`;
        fs.writeFileSync(path.join(d, `f${String(i % 100).padStart(2, "0")}`), unit.repeat(Math.ceil(size / unit.length) || 0).slice(0, size));
      }
      description = `${n} unique files, 100 per directory, sizes (i*2654435761) mod 4096 bytes`;
    }
    git(seed, ["-c", "core.autocrlf=false", "add", "-A", "-f"]);
    git(seed, ["-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", "base"]);
    tree = git(seed, ["rev-parse", "HEAD^{tree}"]);
    if (expectedTree !== undefined && tree !== expectedTree) {
      throw new Error(`repo fixture tree ${tree} differs from the source tree ${expectedTree}`);
    }
    // D = trees, N = blobs or links (no gitlinks are accepted by the helper).
    const listing = git(seed, ["ls-tree", "-r", "-t", "HEAD"]).split("\n").filter(Boolean);
    trees = BigInt(listing.filter((l) => l.split(" ")[1] === "tree").length) + 1n;
    leaves = BigInt(listing.length) - (trees - 1n);
    execFileSync("git", ["clone", "--quiet", "--bare", "--no-local", seed, bare], { env: PINNED_ENV, stdio: "pipe" });
    fs.rmSync(seed, { recursive: true, force: true });
    checkout = true;
  } else {
    git(path.dirname(bare), ["init", "--quiet", "--bare", "--template=", bare]);
    const dag = sharedDag(bare, spec);
    ({ tree, trees, leaves, description } = dag);
    extra = { distinctBlobs: dag.distinctBlobs, maxTreeObjectBytes: dag.maxTreeObjectBytes };
    const commit = git(bare, ["commit-tree", "-m", "base", tree]);
    git(bare, ["update-ref", "refs/heads/main", commit]);
    git(bare, ["symbolic-ref", "HEAD", "refs/heads/main"]);
    checkout = false;
  }
  git(bare, ["repack", "-a", "-d", "-q"]);
  // Adversarial bases are cloned without a checkout: the exploded working tree cannot exist on
  // disk, and discovery reads only .git and the bare's objects (see the README).
  execFileSync("git", ["clone", "--quiet", "--no-local", ...(checkout ? [] : ["--no-checkout"]), bare, clone],
    { env: PINNED_ENV, stdio: "pipe" });
  const commit = git(clone, ["rev-parse", "HEAD"]);
  const model = fixtureModel(spec, bare, tree);
  const manifest: Manifest = {
    spec, kind: spec.replace(/-.*$/, ""), description, commit, tree,
    expectedTreeOccurrences: trees.toString(), expectedLeafOccurrences: leaves.toString(),
    expectedEntryOccurrences: model.totalEntries.toString(), expectedPathBytes: model.totalPathBytes.toString(),
    maxChargedPathBytes: model.maxEntryBytes.toString(),
    ...(extra.distinctBlobs !== undefined ? { distinctBlobs: extra.distinctBlobs.toString() } : {}),
    ...(extra.maxTreeObjectBytes !== undefined ? { maxTreeObjectBytes: extra.maxTreeObjectBytes } : {}),
    checkout, generationMs: Date.now() - started,
  };
  fs.writeFileSync(path.join(dir, "manifest.json"), JSON.stringify(manifest, null, 2));
  return manifest;
}

// ---------------------------------------------------------------------------------------------
// Helper instrumentation preamble. Rules: prefixed identifiers only (it shares top-level scope
// with the helper's own consts), never write stdout/stderr (the classifier trusts the first
// stderr line), swallow its own write failures, and unref its timer (the helper's success path
// exits only when its event loop drains).

// deadlineScale > 1 (validation only) slows the helper's own clock by that factor: Date.now()
// then advances 1/scale as fast as real time, so the helper's 28 s deadline (and its check()
// calls) last 28 s x scale of real time. Caps and memory bounds are untouched. Every other
// Date.now reader in the helper runs on the slowed clock too: each git command's 5 s timer is
// real time but is clamped to the deadline's remaining helper-ms (so under the last 5 helper-s
// it gets 1/scale of the stretched remainder), and maybeYield's 20 ms cadence becomes 20 x scale
// real ms. The preamble's own timing keeps the real clock (pbdNow).
function preamble(metricsPath: string, injectDeadlineAfterSizing: boolean, deadlineScale: number): string {
  const scaled = deadlineScale > 1;
  return `
const pbdFs = require("node:fs");
const pbdCp = require("node:child_process");
const pbdNow = Date.now;
const pbdT0 = pbdNow();
${scaled ? `const pbdScale = ${deadlineScale};
Date.now = () => pbdT0 + Math.floor((pbdNow() - pbdT0) / pbdScale);` : ""}
const pbdPath = ${JSON.stringify(metricsPath)};
const pbdM = { t0: pbdT0, treeOccurrences: 0, treeEntries: 0, commitBytes: 0, treeBytes: 0,
  objectCheckBytes: 0, sizingBytes: 0, gitSpawns: 0, readSyncBytes: 0, discoveryGitLastCloseMs: null,
  sizingCloseMs: null, boundaryMs: null, boundaryMaxRssKiB: null, boundarySnapshot: null,
  firstPostBoundarySpawnMs: null, maxRssKiB: 0, snapshotMs: 0, injected: false };
let pbdWriting = false;
function pbdSnap() {
  if (pbdWriting) return;
  pbdWriting = true;
  try {
    pbdM.maxRssKiB = process.resourceUsage().maxRSS;
    pbdM.snapshotMs = pbdNow() - pbdT0;
    ${scaled ? `pbdM.clockScale = pbdScale;
    pbdM.helperClockElapsedMs = Date.now() - pbdT0;` : ""}
    pbdFs.writeFileSync(pbdPath + ".tmp", JSON.stringify(pbdM));
    pbdFs.renameSync(pbdPath + ".tmp", pbdPath);
  } catch {} finally { pbdWriting = false; }
}
setInterval(pbdSnap, 100).unref();
process.on("exit", pbdSnap);
function pbdEntries(buf) {
  let at = 0, n = 0;
  while (at < buf.length) { const end = buf.indexOf(0, at); if (end < 0) break; at = end + 21; n++; }
  return n;
}
const pbdOpen = pbdFs.openSync;
pbdFs.openSync = function(p, ...rest) {
  if (!pbdWriting && pbdM.boundaryMs === null && typeof p === "string" && p.endsWith("/index")) {
    pbdM.boundaryMs = pbdNow() - pbdT0;
    pbdM.boundaryMaxRssKiB = process.resourceUsage().maxRSS;
    pbdM.boundarySnapshot = { treeOccurrences: pbdM.treeOccurrences, treeEntries: pbdM.treeEntries,
      commitBytes: pbdM.commitBytes, treeBytes: pbdM.treeBytes, objectCheckBytes: pbdM.objectCheckBytes,
      sizingBytes: pbdM.sizingBytes, gitSpawns: pbdM.gitSpawns };
    pbdSnap();
  }
  return pbdOpen.call(this, p, ...rest);
};
const pbdReadSync = pbdFs.readSync;
pbdFs.readSync = function(...args) {
  const n = pbdReadSync.apply(this, args);
  if (!pbdWriting && typeof n === "number") pbdM.readSyncBytes += n;
  return n;
};
const pbdSpawn = pbdCp.spawn;
pbdCp.spawn = function(...args) {
  const p = pbdSpawn.apply(this, args);
  const argv = Array.isArray(args[1]) ? args[1] : [];
  pbdM.gitSpawns++;
  const discovering = pbdM.boundaryMs === null;
  if (!discovering && pbdM.firstPostBoundarySpawnMs === null) { pbdM.firstPostBoundarySpawnMs = pbdNow() - pbdT0; pbdSnap(); }
  const at = argv.indexOf("cat-file");
  if (discovering && at >= 0) {
    const sub = argv[at + 1];
    const sizing = sub === "--batch-check=%(objectname) %(objecttype) %(objectsize)";
    const kind = sub === "tree" ? "tree" : sub === "commit" ? "commit" : sizing ? "sizing" : "check";
    if (kind === "tree") pbdM.treeOccurrences++;
    const chunks = [];
    const read = p.stdout.read;
    p.stdout.read = function(...readArgs) {
      const b = read.apply(this, readArgs);
      if (Buffer.isBuffer(b)) {
        if (kind === "tree") { pbdM.treeBytes += b.length; chunks.push(b); }
        else if (kind === "commit") pbdM.commitBytes += b.length;
        else if (kind === "sizing") pbdM.sizingBytes += b.length;
        else pbdM.objectCheckBytes += b.length;
      }
      return b;
    };
    // Registered before the helper's own close listener, so it runs first.
    p.on("close", () => {
      if (kind === "tree") { pbdM.treeEntries += pbdEntries(Buffer.concat(chunks)); chunks.length = 0; }
      pbdM.discoveryGitLastCloseMs = pbdNow() - pbdT0;
      if (kind === "sizing") {
        pbdM.sizingCloseMs = pbdM.discoveryGitLastCloseMs;
        ${injectDeadlineAfterSizing ? `// Smoke-only fault: expire the helper deadline after the sizing batch closes and
        // before the index open, so maybeYield()'s check() refuses inside that window.
        pbdM.injected = true;
        Date.now = () => pbdNow() + 3600000;` : ""}
        pbdSnap();
      }
    });
  }
  return p;
};
`;
}

// ---------------------------------------------------------------------------------------------
// Parent-side sampling: helper process tree RSS from /proc and the container cgroup.

function readText(p: string): string | undefined {
  try { return fs.readFileSync(p, "utf8"); } catch { return undefined; }
}
function cgroupNumber(name: string): number | null {
  const v = readText(`/sys/fs/cgroup/${name}`);
  return v === undefined || !/^\d+\s*$/.test(v) ? null : Number(v.trim());
}
function oomKills(): number | null {
  const v = readText("/sys/fs/cgroup/memory.events");
  const m = v === undefined ? null : /^oom_kill (\d+)$/m.exec(v);
  return m ? Number(m[1]) : null;
}
// Summed VmRSS of rootPid and all its descendants, plus the descendants' pids (root excluded).
function treeScan(rootPid: number): { rss: number; descendants: number[] } {
  const parent = new Map<number, number>(), rss = new Map<number, number>();
  let entries: string[] = [];
  try { entries = fs.readdirSync("/proc"); } catch { return { rss: 0, descendants: [] }; }
  for (const e of entries) {
    if (!/^\d+$/.test(e)) continue;
    const stat = readText(`/proc/${e}/stat`);
    const status = readText(`/proc/${e}/status`);
    if (!stat || !status) continue;
    const after = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
    parent.set(Number(e), Number(after[1]));
    const m = /^VmRSS:\s+(\d+) kB$/m.exec(status);
    rss.set(Number(e), m ? Number(m[1]) * 1024 : 0);
  }
  if (!rss.has(rootPid)) return { rss: 0, descendants: [] };
  let total = 0;
  const descendants: number[] = [];
  for (const pid of rss.keys()) {
    let p: number | undefined = pid;
    for (let hops = 0; p !== undefined && p > 0 && hops < 64; hops++) {
      if (p === rootPid) { total += rss.get(pid) ?? 0; if (pid !== rootPid) descendants.push(pid); break; }
      p = parent.get(p);
    }
  }
  return { rss: total, descendants };
}

function readCmdline(pid: number): string[] {
  const raw = readText(`/proc/${pid}/cmdline`);
  return raw === undefined || raw === "" ? [] : raw.split("\0").filter((a, i, all) => a !== "" || i < all.length - 1);
}

// Cheap parent-side phase observer for lift rows: called from the 10 ms sampler with the helper's
// current descendants. A missed observation stays null; it is never guessed.
function observePhases(phases: Phases, helperPid: number, descendants: number[], startedAt: number): void {
  const t = Date.now() - startedAt;
  const argvs = new Map<number, string[]>();
  for (const pid of descendants) argvs.set(pid, readCmdline(pid));
  if (phases.sizingPid === null) {
    for (const [pid, argv] of argvs) if (isSizingArgv(argv)) { phases.sizingPid = pid; phases.sizingSeenMs = t; break; }
  } else if (phases.sizingGoneMs === null) {
    const argv = argvs.get(phases.sizingPid);
    if (argv === undefined || !isSizingArgv(argv)) phases.sizingGoneMs = t;
  }
  if (phases.sizingGoneMs !== null && phases.postSizingSpawnSeenMs === null) {
    for (const [pid, argv] of argvs) {
      if (pid !== phases.sizingPid && isGitArgv(argv) && !argv.includes("cat-file")) { phases.postSizingSpawnSeenMs = t; break; }
    }
  }
  if (phases.indexOpenSeenMs === null) {
    try {
      for (const fd of fs.readdirSync(`/proc/${helperPid}/fd`)) {
        let target: string;
        try { target = fs.readlinkSync(`/proc/${helperPid}/fd/${fd}`); } catch { continue; }
        if (target.endsWith("/index")) { phases.indexOpenSeenMs = t; break; }
      }
    } catch { /* the helper is gone or its fd table is unreadable */ }
  }
}

const nullLog: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return nullLog; },
};

interface MeasureJob {
  kind: "measure";
  label: string;
  fixture: string;
  mode: "full" | "control" | "cancel";
  out: string;
  tmp: string;
  injectDeadlineAfterSizing?: boolean;
  cancelAfterTreeOccurrences?: number;
  // Validation jobs only: planning limits merged over the helper's defaults and injected with
  // GitCache.setPlanningLimitsForTest. Absent means production defaults, always for production rows.
  limits?: Record<string, number>;
  // Validation only, integer 2..8: slow the helper's clock (and stretch the outer capture and
  // harness timeouts) by this factor so a near-cap base can finish under every production cap.
  // Mode full only; not combinable with injectDeadlineAfterSizing.
  deadlineScale?: number;
  // Validation only, `true`: lift the helper's 28 s deadline and 5 s per-command timer (and the
  // outer 30 s capture and 60 s scope timers) to ~30 min by rewriting the helper source passed to
  // the spawner. Modes full (metered) and control (unmetered); not combinable with deadlineScale,
  // injectDeadlineAfterSizing or limits.
  deadlineLift?: boolean;
}

function validateDeadlineLift(job: MeasureJob): boolean {
  if (job.deadlineLift === undefined) return false;
  if (job.deadlineLift !== true) throw new Error("deadlineLift must be true when present");
  if (job.mode !== "full" && job.mode !== "control") throw new Error("deadlineLift requires mode full or control");
  if (job.deadlineScale !== undefined) throw new Error("deadlineLift and deadlineScale are mutually exclusive");
  if (job.injectDeadlineAfterSizing === true) throw new Error("deadlineLift and injectDeadlineAfterSizing are mutually exclusive");
  if (job.limits !== undefined) throw new Error("deadlineLift and limits are mutually exclusive");
  return true;
}

function validateDeadlineScale(job: MeasureJob): number {
  if (job.deadlineScale === undefined) return 1;
  const s = job.deadlineScale;
  if (typeof s !== "number" || !Number.isInteger(s) || s < 2 || s > 8) throw new Error("deadlineScale must be an integer 2..8");
  if (job.injectDeadlineAfterSizing === true) throw new Error("deadlineScale and injectDeadlineAfterSizing are mutually exclusive");
  // The clock is slowed by the metering preamble, which control does not prepend (its row would
  // run at the production deadline under a stretched label); cancel measures cancellation only.
  if (job.mode !== "full") throw new Error("deadlineScale requires mode full");
  return s;
}

// Positive safe integers only; any key must be a limit the cache already carries (the two
// base-discovery caps are in the helper's PLANNING_LIMITS defaults).
function validateLimits(limits: unknown, known: Record<string, number>): Record<string, number> {
  if (typeof limits !== "object" || limits === null || Array.isArray(limits) || Object.keys(limits).length === 0) {
    throw new Error("limits must be a non-empty JSON object");
  }
  const out: Record<string, number> = {};
  for (const [key, value] of Object.entries(limits)) {
    if (!/^[A-Za-z][A-Za-z0-9]*$/.test(key)) throw new Error(`limits key ${JSON.stringify(key)} is not an identifier`);
    if (!(key in known)) throw new Error(`unknown planning limit ${key}`);
    if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1) throw new Error(`limits.${key} must be a positive safe integer`);
    out[key] = value;
  }
  return out;
}

type HelperMetrics = Record<string, unknown> & {
  treeOccurrences?: number; boundaryMs?: number | null; firstPostBoundarySpawnMs?: number | null;
};

function readJson<T>(p: string): T | undefined {
  try { return JSON.parse(fs.readFileSync(p, "utf8")) as T; } catch { return undefined; }
}
function writeJson(p: string, value: unknown): void {
  fs.writeFileSync(p + ".tmp", JSON.stringify(value, null, 2));
  fs.renameSync(p + ".tmp", p);
}

// Resolves to the job's failure flags (empty when the record is sound).
async function measure(job: MeasureJob): Promise<string[]> {
  const deadlineScale = validateDeadlineScale(job);
  const deadlineLift = validateDeadlineLift(job);
  const manifest = readJson<Manifest>(path.join(job.fixture, "manifest.json"));
  if (!manifest) throw new Error(`fixture ${job.fixture} has no manifest`);
  const { root, clone, bare } = layout(job.fixture);
  fs.mkdirSync(job.out, { recursive: true });
  const helperPath = path.join(job.out, `${job.label}.helper.json`);
  const livePath = path.join(job.out, `${job.label}.live.json`);
  const resultPath = path.join(job.out, `${job.label}.json`);
  for (const p of [helperPath, livePath, resultPath]) fs.rmSync(p, { force: true });
  // The helper places its temp tree under TMPDIR (readBoundedPlanningOutput passes runnerTmpdir()).
  fs.rmSync(job.tmp, { recursive: true, force: true });
  fs.mkdirSync(job.tmp, { recursive: true, mode: 0o700 });
  process.env.TMPDIR = job.tmp;
  delete process.env.UZI_RUNNER_TMPDIR;
  const instrumented = job.mode !== "control";

  const live = {
    label: job.label, startedAt: Date.now(), cgroupCurrentAtStart: cgroupNumber("memory.current"),
    oomKillsAtStart: oomKills(), helperPid: null as number | null, treeRssPeak: 0, treeRssDiscoveryPeak: 0,
    cgroupPeakAtBoundary: null as number | null, lastHelper: undefined as HelperMetrics | undefined,
    spawnTmpdir: null as string | null, signals: [] as string[], elapsedMs: 0,
    phases: emptyPhases(),
  };
  let boundarySeen = false;
  const sample = (): void => {
    if (live.helperPid !== null) {
      const scan = treeScan(live.helperPid);
      const rss = scan.rss;
      if (deadlineLift) observePhases(live.phases, live.helperPid, scan.descendants, live.startedAt);
      live.treeRssPeak = Math.max(live.treeRssPeak, rss);
      if (!boundarySeen) live.treeRssDiscoveryPeak = Math.max(live.treeRssDiscoveryPeak, rss);
    }
  };
  const sampler = setInterval(sample, 10);
  let lastPersist = 0;
  const ac = new AbortController();
  // Held in an object: TypeScript does not see closure assignments to a local `let`.
  const st: { cancel?: { trigger: string; at: number }; completion?: { code: number } | { rejected: string }; outcomeAt?: number } = {};
  const watcher = setInterval(() => {
    live.elapsedMs = Date.now() - live.startedAt;
    if (instrumented) {
      const h = readJson<HelperMetrics>(helperPath);
      if (h) live.lastHelper = h;
      if (h && h.boundaryMs !== null && h.boundaryMs !== undefined && !boundarySeen) {
        boundarySeen = true;
        live.cgroupPeakAtBoundary = cgroupNumber("memory.peak");
      }
      if (job.mode === "cancel" && !st.cancel && st.outcomeAt === undefined && h) {
        const n = job.cancelAfterTreeOccurrences ?? 50;
        if (!boundarySeen && (h.treeOccurrences ?? 0) >= n) st.cancel = { trigger: `mid_discovery:treeOccurrences>=${n}`, at: Date.now() };
        else if (boundarySeen && h.firstPostBoundarySpawnMs !== null && h.firstPostBoundarySpawnMs !== undefined) {
          st.cancel = { trigger: "post_boundary:first_capture_spawn", at: Date.now() };
        }
        if (st.cancel) ac.abort();
      }
    }
    if (Date.now() - lastPersist >= 100) {
      lastPersist = Date.now();
      try { writeJson(livePath, live); } catch { /* best effort */ }
    }
  }, 10);

  // Production envelope (runner.ts captureCheckedPlanningDiff): 60 s scope, combined signal,
  // default-grace TickSpawner, scope.abort() in finally, then settled()/survivors().
  // A deadline-scaled validation row stretches the scope timer by the same factor.
  const scope = new AbortController();
  const ownerSignal = AbortSignal.any([ac.signal, scope.signal]);
  const scopeTimer = setTimeout(() => scope.abort(), deadlineLift ? LIFTED_SCOPE_TIMEOUT_MS : 60_000 * deadlineScale);
  const spawner = new TickSpawner({
    signal: ownerSignal, log: nullLog,
    // Observation only: record the pid and delivered signals; returning undefined keeps real delivery.
    hooks: {
      onSpawn: (pid) => { live.helperPid ??= pid; },
      signalGroup: (_pgid, sig) => { live.signals.push(`${sig}@${Date.now() - live.startedAt}ms`); return undefined; },
    },
  });
  // Set when a lift guard throws; the row then fails with DEADLINE_LIFT_REFUSED.
  let liftGuardError: string | null = null;
  const wrapped = async (request: BoundaryProcessRequest): Promise<BoundaryProcessHandle> => {
    live.spawnTmpdir = request.env.TMPDIR ?? null;
    let req = request;
    if (instrumented || deadlineLift) {
      const argv = [...request.argv];
      if (argv[1] !== "-e" || typeof argv[2] !== "string") throw new Error("unexpected planning helper argv");
      // Lift first, so the lift sees the pristine helper source and never the preamble.
      if (deadlineLift) {
        try { argv[2] = liftPlanningHelperDeadline(argv[2]); } catch (e) {
          liftGuardError ??= String((e as Error)?.message ?? e);
          throw e;
        }
      }
      if (instrumented) argv[2] = preamble(helperPath, job.injectDeadlineAfterSizing === true, deadlineScale) + argv[2];
      req = { ...request, argv };
    }
    const handle = await spawner.spawn(req);
    handle.completed.then((c) => { st.completion = { code: c.code }; },
      (e: unknown) => { st.completion = { rejected: String((e as Error)?.message ?? e).slice(0, 200) }; });
    return handle;
  };
  const cache = new GitCache(root, nullLog);
  // GitCache keeps its default limits in the private `planningLimits` field (PLANNING_LIMITS is
  // not exported); reading it records the limits the helper really receives, so production rows
  // carry whatever caps the helper ships with and a validation row replaces only what it names.
  const limitsHolder = cache as unknown as { planningLimits: Record<string, number> };
  if (typeof limitsHolder.planningLimits !== "object" || limitsHolder.planningLimits === null) {
    throw new Error("GitCache.planningLimits is not an object: the harness cannot read the helper's defaults");
  }
  const validation = job.limits !== undefined || deadlineScale > 1 || deadlineLift;
  if (deadlineScale > 1 || deadlineLift) {
    // The outer capture timeout (30 s in capturePlanningDiff) is an argument of the transport
    // method; multiply it by the scale (or replace it for a lift row) and otherwise delegate
    // unchanged (same argv, same cwd).
    const holder = cache as unknown as {
      readBoundedPlanningOutput: (cwd: string, argv: readonly string[], timeoutMs?: number) => Promise<Buffer>;
    };
    const original = holder.readBoundedPlanningOutput.bind(cache);
    holder.readBoundedPlanningOutput = (cwd, argv, timeoutMs) => {
      if (deadlineLift) {
        // Fail closed: only the production 30 s outer timeout is replaced.
        if (timeoutMs !== PRODUCTION_OUTER_TIMEOUT_MS) {
          const msg = `deadlineLift: unexpected outer capture timeout ${String(timeoutMs)}`;
          liftGuardError ??= msg;
          throw new Error(msg);
        }
        return original(cwd, argv, LIFTED_OUTER_TIMEOUT_MS);
      }
      return original(cwd, argv, timeoutMs === undefined ? undefined : timeoutMs * deadlineScale);
    };
  }
  if (job.limits !== undefined) {
    cache.setPlanningLimitsForTest({ ...limitsHolder.planningLimits, ...validateLimits(job.limits, limitsHolder.planningLimits) });
  }
  const effectiveLimits: Record<string, number> = { ...limitsHolder.planningLimits };
  const capOf = (key: string): bigint | undefined => {
    const v = effectiveLimits[key];
    return typeof v === "number" && Number.isSafeInteger(v) && v > 0 ? BigInt(v) : undefined;
  };
  const entryCap = capOf(BASE_ENTRY_CAP), pathByteCap = capOf(BASE_PATH_BYTE_CAP);
  let outcome: Record<string, unknown>;
  let abortToSettledMs: number | null = null;
  const started = Date.now();
  try {
    const bytes = await cache.withBoundaryProcessSpawner(wrapped, ownerSignal,
      () => cache.capturePlanningDiff(clone, manifest.commit, bare));
    st.outcomeAt = Date.now();
    outcome = { kind: "completed", patchBytes: bytes.length };
  } catch (error) {
    const message = String((error as Error)?.message ?? error);
    st.outcomeAt = Date.now();
    const c = classifyPlanningCaptureError(message);
    // `cancelled` only when the abort landed before the capture's outcome.
    const cancelled = st.cancel !== undefined && st.cancel.at <= st.outcomeAt;
    outcome = { kind: cancelled ? "cancelled" : "refused", refusal: c.refusal, token: c.diagnostic, message: message.slice(0, 300) };
  } finally {
    clearTimeout(scopeTimer);
    scope.abort();
  }
  const captureMs = Date.now() - started;
  await spawner.settled();
  const cancel = st.cancel;
  if (cancel) abortToSettledMs = Date.now() - cancel.at;
  clearInterval(watcher);
  sample();
  clearInterval(sampler);
  // A completion handler may still be pending one tick after settlement.
  await new Promise((r) => setImmediate(r));
  const helper = instrumented ? readJson<HelperMetrics>(helperPath) : undefined;
  // Scan the TMPDIR the helper was actually spawned with; flag any mismatch with the job's dir.
  const scanned = live.spawnTmpdir ?? job.tmp;
  const tmpdirMatches = live.spawnTmpdir === job.tmp;
  const leftover = fs.readdirSync(scanned).filter((n) => n.startsWith("uzi-planning-"));
  const survivors = spawner.survivors();
  const oomDelta = (() => { const now = oomKills(); return now === null || live.oomKillsAtStart === null ? null : now - live.oomKillsAtStart; })();
  if (oomDelta !== null && oomDelta > 0) outcome = { ...outcome, kind: "oom", oomKillDelta: oomDelta };
  // A helper signal death that no envelope signal explains (code 128 with nothing sent, no OOM
  // evidence, no cancel) is not a refusal.
  else if (outcome.kind === "refused" && st.completion && "code" in st.completion && st.completion.code === 128
      && live.signals.length === 0) outcome = { ...outcome, kind: "helper_killed_unknown" };
  // Charges (7b/7c/7d). The meter's tree-entry total (`treeEntries`) counts entries of FETCHED
  // trees. It equals the charged entry occurrences only when discovery completed; otherwise it is
  // reported as an upper bound (treeEntriesFetched), never as a charge.
  const model = fixtureModel(manifest.spec, bare, manifest.tree);
  const failures: string[] = [];
  const discoveryCompleted = instrumented && helper?.boundaryMs !== null && helper?.boundaryMs !== undefined;
  const fetched = typeof helper?.treeEntries === "number" ? helper.treeEntries : null;
  let charged: Record<string, unknown> | null = null;
  let derived: DerivedCharge | null = null;
  let derivedReason: string | null = null;
  if (discoveryCompleted) {
    const meterMatch = fetched === null ? null : BigInt(fetched) === model.totalEntries;
    charged = { basis: "analytic", entries: model.totalEntries.toString(), pathBytes: model.totalPathBytes.toString(),
      maxEntryPathBytes: model.maxEntryBytes.toString(), meterEntries: fetched, meterMatch };
    if (meterMatch !== true) failures.push(`METER_MISMATCH: meter fetched ${fetched} tree entries, analytic model ${model.totalEntries}`);
  } else if (outcome.kind === "refused" && typeof outcome.token === "string"
      && (outcome.token === "base_discovery_entry_cap" || outcome.token === "base_discovery_path_byte_cap")) {
    const d = deriveRefusal(model, outcome.token, entryCap, pathByteCap);
    derived = d.derived;
    derivedReason = d.reason ?? null;
    if (d.assertionFailed) failures.push(`DERIVATION_ASSERTION: ${d.reason}`);
    // A cap token with no known cap value means the harness and helper disagree on the key name.
    else if (d.derived === null) failures.push(`CAP_UNKNOWN: ${d.reason}`);
  }
  // A validation row exists to exercise injected caps; the helper rejecting them is a harness error.
  if (validation && outcome.kind === "refused" && outcome.token === "invalid_limits") {
    failures.push("VALIDATION_LIMITS_REJECTED: the helper refused the injected limits (invalid_limits)");
  }
  // Lift rows: the parent-side phase observer decides the evidence; for a metered row the
  // preamble only ever contradicts it (a phase the observer missed is never a mismatch).
  if (liftGuardError !== null) failures.push(`DEADLINE_LIFT_REFUSED: ${liftGuardError}`);
  const phases = deadlineLift ? { ...live.phases } : null;
  const outcomeMs = st.outcomeAt === undefined ? null : st.outcomeAt - live.startedAt;
  const phaseEvidence = deadlineLift ? phaseEvidenceOf(outcome.kind, phases, outcomeMs) : null;
  if (deadlineLift && instrumented && phases !== null) failures.push(...phaseObserverMismatches(phases, live.startedAt, helper));
  if (failures.length > 0) outcome = { ...outcome, flag: failures.join("; ") };
  const result = {
    label: job.label, spec: manifest.spec, fixture: manifest, mode: job.mode,
    validation,
    // null for production rows; otherwise the helper clock, outer capture timeout and harness
    // scope timer were all stretched by this factor (every cap and bound stays at its value).
    deadlineScale: deadlineScale > 1 ? deadlineScale : null,
    // null for every row but a lift row, whose helper deadline, per-command timer, outer capture
    // timeout and harness scope timer were lifted to these values (caps and memory bounds untouched).
    deadlineLift: deadlineLift ? { helperDeadlineMs: LIFTED_HELPER_DEADLINE_MS, commandTimerMs: LIFTED_COMMAND_TIMER_MS,
      outerTimeoutMs: LIFTED_OUTER_TIMEOUT_MS, scopeTimeoutMs: LIFTED_SCOPE_TIMEOUT_MS } : null,
    phases, phaseEvidence,
    limits: { source: job.limits !== undefined ? "job-limits-over-helper-defaults" : "helper-defaults", effective: effectiveLimits,
      entryCap: entryCap?.toString() ?? null, pathByteCap: pathByteCap?.toString() ?? null },
    failed: failures.length > 0, failures,
    treeEntriesFetched: instrumented ? { value: fetched, meaning: discoveryCompleted ? "exact (discovery completed)" : "upper bound on charged entries (discovery incomplete); not a charge" } : null,
    charged,
    // Non-null only for a base-discovery cap refusal; `charge` is null (with the reason) when the
    // cap values are unknown for the job or the model contradicts the token.
    derived: derived !== null || derivedReason !== null ? { charge: derived, reason: derivedReason } : null,
    memLimit: process.env.MEASURE_MEM_LIMIT ?? null, cpus: process.env.MEASURE_CPUS ?? null,
    node: process.version, gitVersion: (() => { try { return execFileSync("git", ["--version"], { encoding: "utf8" }).trim(); } catch { return null; } })(),
    captureMs, outcome,
    discovery: instrumented ? {
      completed: helper?.boundaryMs !== null && helper?.boundaryMs !== undefined,
      ms: helper?.boundaryMs ?? null,
      gitLastCloseMs: helper?.discoveryGitLastCloseMs ?? null,
      sizingCloseMs: helper?.sizingCloseMs ?? null,
      injected: helper?.injected ?? false,
      atBoundary: helper?.boundarySnapshot ?? null,
    } : null,
    helper: helper ?? null,
    helperMaxRssKiB: instrumented ? { discovery: helper?.boundaryMaxRssKiB ?? null, final: helper?.maxRssKiB ?? null } : null,
    treeRss: { discoveryPeakBytes: instrumented ? live.treeRssDiscoveryPeak : null, peakBytes: live.treeRssPeak, sampledEveryMs: 10 },
    cgroup: { currentAtStart: live.cgroupCurrentAtStart, peakAtBoundary: live.cgroupPeakAtBoundary,
      peakAtEnd: cgroupNumber("memory.peak"), oomKillDelta: oomDelta },
    helperCompletion: st.completion ?? null,
    signals: live.signals,
    survivors,
    spawnTmpdir: live.spawnTmpdir,
    tmpdirMatches,
    leftoverTemp: leftover,
    cancel: cancel ? {
      trigger: cancel.trigger, abortToSettledMs, graceMs: 2000,
      landedBeforeOutcome: st.outcomeAt === undefined || cancel.at <= st.outcomeAt,
      cleanupOk: (st.outcomeAt === undefined || cancel.at <= st.outcomeAt) && abortToSettledMs !== null
        && abortToSettledMs <= 2000 && survivors.length === 0 && leftover.length === 0 && tmpdirMatches,
    } : null,
  };
  writeJson(resultPath, result);
  fs.rmSync(job.tmp, { recursive: true, force: true });
  return failures;
}

// ---------------------------------------------------------------------------------------------
// Report: one Markdown table over every job, merging partial (killed) records as lower bounds.

const MiB = (b: unknown): string => typeof b === "number" ? (b / 1048576).toFixed(1) : "n/a";
const KiBtoMiB = (k: unknown): string => typeof k === "number" ? (k / 1024).toFixed(1) : "n/a";

function report(out: string): void {
  const labels = new Set<string>();
  for (const f of fs.readdirSync(out)) {
    const m = /^(.+?)\.(json|live\.json|helper\.json|term\.json)$/.exec(f);
    // verify.json is the verify job's own output, not a measurement record.
    if (m?.[1] && !f.endsWith(".tmp") && f !== "verify.json") labels.add(m[1]);
  }
  const rows: string[] = [
    "| fixture | mode | mem/cpus | outcome | discovery done | discovery ms | capture ms | tree occ (obs / expected) | treeEntriesFetched (meter; upper bound unless discovery done) | charged entries / path bytes (basis) | discovery object bytes | readSync bytes | helper maxRSS MiB (disc / final) | tree RSS peak MiB (disc / all) | cgroup peak MiB (disc / end) | cancel: settle ms, cleanup |",
    "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
  ];
  for (const label of [...labels].sort()) {
    const r = readJson<Record<string, any>>(path.join(out, `${label}.json`));
    const term = readJson<Record<string, any>>(path.join(out, `${label}.term.json`));
    const liveRec = readJson<Record<string, any>>(path.join(out, `${label}.live.json`));
    const helperRec = readJson<Record<string, any>>(path.join(out, `${label}.helper.json`));
    const [spec = label, base = "?", mem = "?", extra] = label.split("__");
    const mode = extra ? `${base} (${extra})` : base;
    if (!r) {
      // Killed before the result was written: last persisted values are lower bounds.
      const kind = term?.oomKilled === true ? "oom" : term?.killedByHarness === true ? "killed_by_harness"
        : term?.exitCode === "137" ? "killed_unknown" : term ? "harness_error" : "missing";
      const h = helperRec ?? liveRec?.lastHelper;
      const L = (v: unknown): string => (v === undefined || v === null ? "n/a" : `≥${String(v)}`);
      const objBytes = h ? (h.commitBytes ?? 0) + (h.treeBytes ?? 0) + (h.objectCheckBytes ?? 0) + (h.sizingBytes ?? 0) : undefined;
      rows.push(`| ${spec} | ${mode} | ${mem} | ${kind} (exit ${term?.exitCode ?? "?"}) | ${h?.boundaryMs != null ? "yes" : "no/unknown"} | ${h?.boundaryMs ?? "n/a"} | ${L(liveRec?.elapsedMs)} | ${L(h?.treeOccurrences)} / ${readJson<Manifest>(path.join(out, "..", "fixtures", spec, "manifest.json"))?.expectedTreeOccurrences ?? "?"} | fetched ${L(h?.treeEntries)} (not charged) | n/a (no result record) | ${L(objBytes)} | ${L(h?.readSyncBytes)} | ${KiBtoMiB(h?.boundaryMaxRssKiB)} / ${h?.maxRssKiB != null ? "≥" + KiBtoMiB(h.maxRssKiB) : "n/a"} | ${liveRec ? "≥" + MiB(liveRec.treeRssDiscoveryPeak) : "n/a"} / ${liveRec ? "≥" + MiB(liveRec.treeRssPeak) : "n/a"} | ${MiB(liveRec?.cgroupPeakAtBoundary)} / n/a | n/a |`);
      continue;
    }
    let kind: string = r.outcome?.kind ?? "?";
    if (term?.oomKilled === true) kind = "oom";
    const token = (r.outcome?.token ? ` \`${r.outcome.token}\`` : "") + (r.outcome?.flag ? ` **FAIL ${r.outcome.flag}**` : "");
    const stretched = typeof r.deadlineScale === "number" ? `validation (deadline ×${r.deadlineScale}; real ${r.captureMs} ms)` : "validation";
    const mode2 = r.deadlineLift ? `${base} (validation, deadline lifted, ${r.mode === "control" ? "unmetered" : "metered"})`
      : r.validation === true && !mode.includes("validation") ? `${mode} (${stretched})`
      : r.validation === true && typeof r.deadlineScale === "number" ? `${mode} [${stretched}]` : mode;
    const entriesCell = ((): string => {
      const f = r.treeEntriesFetched;
      if (!f) return "n/a";
      return r.discovery?.completed ? `fetched ${f.value ?? "n/a"} (exact)` : `fetched ${f.value ?? "n/a"} (upper bound, not charged)`;
    })();
    const chargedCell = ((): string => {
      if (r.charged) {
        const meter = r.charged.meterMatch === true ? "meter match" : r.charged.meterMatch === false ? "**METER MISMATCH**" : "meter n/a";
        return `${r.charged.entries} / ${r.charged.pathBytes} (analytic; ${meter})`;
      }
      if (r.derived?.charge) return `${r.derived.charge.entries} / ${r.derived.charge.pathBytes} (${r.derived.charge.basis})`;
      if (r.derived) return `n/a (derived: null; ${r.derived.reason ?? "unknown"})`;
      return "n/a";
    })();
    const d = r.discovery;
    const h = r.helper ?? {};
    const exp = r.fixture?.expectedTreeOccurrences ?? "?";
    const occ = d ? `${h.treeOccurrences ?? "n/a"} / ${exp}` : `n/a / ${exp}`;
    const objBytes = d ? String((h.commitBytes ?? 0) + (h.treeBytes ?? 0) + (h.objectCheckBytes ?? 0) + (h.sizingBytes ?? 0)) : "n/a";
    const c = r.cancel ? `${r.cancel.abortToSettledMs}, ${r.cancel.cleanupOk ? "ok" : "NOT ok"} (${r.cancel.trigger}${r.cancel.landedBeforeOutcome === false ? "; after outcome" : ""})` : "";
    // A helper that died by a signal (OOM or otherwise) left only its last snapshot: lower bounds.
    const killed = kind === "oom" || kind === "helper_killed_unknown" || (r.helperCompletion && "rejected" in r.helperCompletion) || r.helperCompletion?.code === 128;
    const B = (v: unknown): string => {
      const text = v === undefined || v === null ? "n/a" : String(v);
      return killed && !text.startsWith("n/a") ? `≥${text}` : text;
    };
    const cgEnd = kind === "oom" ? `OOM at limit ${r.memLimit ?? mem}` : MiB(r.cgroup?.peakAtEnd);
    const helperRss = r.helperMaxRssKiB ? `${KiBtoMiB(r.helperMaxRssKiB.discovery)} / ${B(KiBtoMiB(r.helperMaxRssKiB.final))}` : "n/a";
    rows.push(`| ${spec} | ${mode2} | ${r.memLimit ?? mem}/${r.cpus ?? "?"} | ${kind}${token} | ${d ? (d.completed ? "yes" : "no") : "n/a"} | ${d?.ms ?? "n/a"} | ${r.captureMs} | ${d ? `${B(h.treeOccurrences)} / ${exp}` : occ} | ${entriesCell} | ${chargedCell} | ${d ? B(objBytes) : "n/a"} | ${d ? B(h.readSyncBytes) : "n/a"} | ${helperRss} | ${B(MiB(r.treeRss?.discoveryPeakBytes))} / ${B(MiB(r.treeRss?.peakBytes))} | ${MiB(r.cgroup?.peakAtBoundary)} / ${cgEnd} | ${c} |`);
  }
  const table = rows.join("\n") + "\n";
  fs.writeFileSync(path.join(out, "results.md"), table);
  process.stdout.write(table);
}

// ---------------------------------------------------------------------------------------------

type Job =
  | { kind: "generate"; spec: string; dir: string; inputs: string }
  | MeasureJob
  | { kind: "report"; out: string }
  // Row-validity check over `<out>/<label>.json` + `<label>.term.json` (see verifyRow). Read-only
  // except verify.json; needs no opt-in env and no docker, so it also runs on the host.
  | { kind: "verify"; out: string; rows: { label: string; expect: string }[] }
  // Pure description of the analytic charges for one spec (no helper run, no docker). `fixture`
  // (a generated fixture dir) is required for repo and synthetic-N, whose model reads their tree.
  // Caps and `at` are positive integers or decimal strings; `at` may also be an array of them.
  // A scalar `at` prints `prefixAt`; an array prints `prefixAts` (Em/Pm per index). Either also
  // prints `probes`: the 7f cap pairs for each index i (see describeModel).
  | { kind: "model"; spec: string; fixture?: string; entryCap?: number | string; pathByteCap?: number | string; at?: number | string | (number | string)[] }
  // Absorbs a container's first-start transients so no measurement is the first start.
  | { kind: "noop" };

function describeModel(job: Extract<Job, { kind: "model" }>): Record<string, unknown> {
  const big = (v: number | string | undefined, name: string): bigint | undefined => {
    if (v === undefined) return undefined;
    if (!/^[1-9][0-9]*$/.test(String(v))) throw new Error(`${name} must be a positive integer`);
    return BigInt(String(v));
  };
  let bare: string | undefined, tree: string | undefined;
  if (job.fixture !== undefined) {
    bare = layout(job.fixture).bare;
    tree = readJson<Manifest>(path.join(job.fixture, "manifest.json"))?.tree;
  }
  const model = fixtureModel(job.spec, bare, tree);
  const entryCap = big(job.entryCap, "entryCap"), pathByteCap = big(job.pathByteCap, "pathByteCap");
  const ats = (Array.isArray(job.at) ? job.at : job.at === undefined ? [] : [job.at]).map((v) => big(v, "at") as bigint);
  const out: Record<string, unknown> = {
    spec: job.spec, basis: "analytic",
    expectedEntryOccurrences: model.totalEntries.toString(), expectedPathBytes: model.totalPathBytes.toString(),
    maxChargedPathBytes: model.maxEntryBytes.toString(),
  };
  const point = (i: bigint): { i: string; em: string; pm: string | null } => ({ i: i.toString(), em: i.toString(), pm: model.prefixAt(i)?.toString() ?? null });
  if (ats.length > 0 && !Array.isArray(job.at)) {
    const p = point(ats[0] as bigint);
    out.prefixAt = { i: p.i, pathBytes: p.pm };
  }
  if (ats.length > 0 && Array.isArray(job.at)) out.prefixAts = ats.map(point);
  if (ats.length > 0) {
    // 7f probe pair for index i: (entryCap=i, pathByteCap=Pm(i)-1) must refuse with the path token
    // (entry i is the first whose cumulative path bytes exceed the cap, and it is checked after
    // the entry cap's check of occurrence i passes); (entryCap=i, pathByteCap=Pm(i)) must refuse
    // with the entry token (occurrence i+1 trips the entry cap before it is charged). Null when
    // the walk is too short or the cap would not be a positive integer.
    out.probes = ats.map((i) => {
      const pm = model.prefixAt(i);
      const pair = (cap: bigint): { limits: Record<string, number>; limitsJson: string } => {
        const limits = { [BASE_ENTRY_CAP]: Number(i), [BASE_PATH_BYTE_CAP]: Number(cap) };
        return { limits, limitsJson: JSON.stringify(limits) };
      };
      return {
        i: i.toString(), pm: pm?.toString() ?? null,
        // Caps must fit run.sh's LIMITS_JSON bound of 15 digits.
        refusePathToken: pm !== null && pm > 1n && pm <= MAX_PROBE_CAP ? { token: "base_discovery_path_byte_cap", ...pair(pm - 1n) } : null,
        refuseEntryToken: pm !== null && i < model.totalEntries && pm <= MAX_PROBE_CAP ? { token: "base_discovery_entry_cap", ...pair(pm) } : null,
      };
    });
  }
  if (entryCap !== undefined) out.atEntryCap = { entryCap: entryCap.toString(), ...deriveRefusal(model, "base_discovery_entry_cap", entryCap, pathByteCap) };
  if (pathByteCap !== undefined) out.atPathByteCap = { pathByteCap: pathByteCap.toString(), ...deriveRefusal(model, "base_discovery_path_byte_cap", entryCap, pathByteCap) };
  return out;
}

// Returns whether every row is valid. A missing or unparseable file is an invalid row, never a throw.
function verify(job: Extract<Job, { kind: "verify" }>): boolean {
  if (typeof job.out !== "string" || !path.isAbsolute(job.out) || !Array.isArray(job.rows) || job.rows.length === 0) {
    throw new Error("verify needs an absolute out dir and a non-empty rows array");
  }
  const verdicts = job.rows.map((row) => {
    if (typeof row?.label !== "string" || !/^[A-Za-z0-9._-]+$/.test(row.label) || typeof row.expect !== "string" || row.expect === "") {
      return { label: String(row?.label), expect: String(row?.expect), valid: false, reasons: ["malformed row spec"] };
    }
    const v = verifyRow(readJson<unknown>(path.join(job.out, `${row.label}.json`)),
      readJson<unknown>(path.join(job.out, `${row.label}.term.json`)), row.label, row.expect);
    return { label: row.label, expect: row.expect, ...v };
  });
  const allValid = verdicts.every((v) => v.valid);
  writeJson(path.join(job.out, "verify.json"), { allValid, rows: verdicts });
  const lines = verdicts.map((v) => `${v.valid ? "VALID  " : "INVALID"} ${v.label} [${v.expect}]${v.valid ? "" : ` ${v.reasons.join(" | ")}`}`);
  process.stdout.write(lines.join("\n") + `\nverify: ${allValid ? "all rows valid" : "INVALID rows present"}\n`);
  return allValid;
}

async function main(): Promise<void> {
  const [command, arg] = process.argv.slice(2);
  // `<inline job json>` is accepted for the pure model kind: no job file or opt-in env needed.
  const inline = command !== undefined && command.startsWith("{") ? command : undefined;
  if (inline === undefined && (command !== "job" || !arg)) {
    console.error("usage: planning-base-discovery.measure.ts job <job.json> | '<model job json>'");
    process.exit(2);
  }
  const job = JSON.parse(inline ?? fs.readFileSync(arg as string, "utf8")) as Job;
  if (job.kind === "model") {
    process.stdout.write(JSON.stringify(describeModel(job), null, 2) + "\n");
    return;
  }
  if (job.kind === "verify") {
    if (!verify(job)) process.exitCode = 1;
    return;
  }
  if (inline !== undefined || process.env.UZI_PLANNING_DISCOVERY_MEASURE !== "1") {
    console.error("refusing to run: set UZI_PLANNING_DISCOVERY_MEASURE=1 and pass `job <job.json>` (opt-in measurement harness, issue #3543)");
    process.exit(2);
  }
  if (job.kind === "generate") {
    const m = generate(job.spec, job.dir, job.inputs);
    process.stdout.write(`generated ${m.spec}: tree ${m.tree} trees=${m.expectedTreeOccurrences} leaves=${m.expectedLeafOccurrences} ` +
      `entries=${m.expectedEntryOccurrences} pathBytes=${m.expectedPathBytes} maxPath=${m.maxChargedPathBytes} ` +
      `in ${m.generationMs} ms (sha256 manifest ${createHash("sha256").update(JSON.stringify({ ...m, generationMs: 0 })).digest("hex").slice(0, 12)})\n`);
  } else if (job.kind === "measure") {
    const failures = await measure(job);
    process.stdout.write(`measured ${job.label}\n`);
    if (failures.length > 0) {
      // Visible failure: nonzero exit (run.sh logs it in the job's term record) and the flag in the record.
      console.error(`FAILED ${job.label}: ${failures.join("; ")}`);
      process.exitCode = 1;
    }
  } else if (job.kind === "report") {
    report(job.out);
  } else if (job.kind === "noop") {
    // Kept in the container log so the first-start transient stays inspectable.
    process.stdout.write(`warmup: cgroup memory.current=${cgroupNumber("memory.current")} memory.peak=${cgroupNumber("memory.peak")}\n`);
  } else {
    throw new Error(`unknown job kind ${String((job as { kind?: unknown }).kind)}`);
  }
}

await main();
