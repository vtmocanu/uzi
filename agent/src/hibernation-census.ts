// PRD #4208 M1, worker side: the volume census behind the hibernation inventory.
//
// HibernationInventoryController (hibernation-inventory.ts) takes the exclusion and calls the two seams
// built here once the worker is quiescent and holds the pod-epoch lock exclusively:
//   - the census: a bounded, lstat-only walk of the whole data volume, every node classified into one of the
//     entry kinds below, plus the Docker daemon's inventory on the DinD lane;
//   - the shutdown fingerprint: ONLY the walk fingerprint recomputed under a deadline (no discovery, holds, content
//     proofs or Docker), reported with the digest of the accepted census, so the api compares the volume at
//     hibernation with the volume at termination and server-side state cannot make an untouched volume differ.
//
// WHAT IS CLASSIFIED (the shared vocabulary with the api evaluator; every value matches ^[a-z0-9_:.-]{1,64}$)
//   kind            runtime_lock outbox agent_home jobs lost_found bare_repo runner_ref worktree canonical_clone
//                   retained_attempt journal unsettled_op runner_scratch unknown_path
//   classification  disposable (needs no receipt) | needs_receipt (run-bound: carries run_id and a 40-hex tip)
//                   | protected | unknown | unrecoverable
//   content_state   content_proven (the worktree passed the index-independent proof) | unproven_worktree_bytes
//                   | ref (a content-addressed git ref) | none
//
// TOP LEVEL OF <dataDir> (names from config.ts and the writers' own constants; anything else is unknown_path):
//   runner/                      classified below (git.ts runnerRoot)
//   repos/                       one bare_repo per *.git bare cache (git.ts reposRoot); runner refs inside it are
//                                runner_ref entries
//   recovery, recovery-maintenance, recovery-settlement, recovery-archive
//                                journal: disposable only when empty, else protected (git.ts recoveryRoot etc.)
//   outbox/                      outbox: disposable only when empty, else protected (config.outboxDataDir)
//   agent-home/                  agent_home: disposable only when empty, else unknown (main.ts sdkHomeRoot)
//   jobs/                        jobs: disposable only when empty, else unknown (main.ts jobsRoot)
//   lost+found/                  lost_found: disposable only when empty, else unknown (the filesystem's own)
//   .uzi-authority/              runtime_lock: disposable only when it holds nothing but the lock files worker.lock
//                                and authority.lock; the authenticated authority store (enrollment, heads, slots,
//                                boundaries) is unknown (the plan names only the lock files as disposable)
//   .uzi-epoch.lock, .uzi-epoch-consumed, .uzi-pod-entry
//                                runtime_lock, disposable (the pod-epoch lock and its ledger, pod-epoch-init.cjs)
//
// RUNNER ROOT: discoverEpochRetainedSources + inspectRetainedSources (the reclaim's own read-only classifier),
// fed with the lifecycle and holds the reclaim reads through the same client calls, give each attempt or canonical
// source its disposition. A source the existing machinery would let a reclaim discard is `needs_receipt` here
// (its run's server-side receipt, not elapsed time, is what makes the bytes safe to lose), with the run id and the
// tip the content proof verified. The content proof is GitCache.proveEpochWorktreeContent. Every worktree node the
// proof did not account for (ignored files, anything outside the tracked set, `.git` excluded) is one aggregated
// `unproven_worktree_bytes` entry per worktree. Runner refs (refs/uzi-runner/*) are read in process from the bare's
// loose refs and packed-refs, with the run bound through the retained sources' branch.
//
// COMPLETENESS: any cap, any unreadable path, any mount boundary, any failed Docker request, any lifecycle/holds
// read that failed, an exceeded entry or body cap, or a volume that changed while the census ran makes the result
// `complete: false`. The fingerprint is taken from a final walk after every classification step, so it describes
// the volume the proofs actually saw.
//
// THE WORKER'S OWN TMPDIR is walked and fingerprinted like everything else (no directory is exempt). In the split
// layout it is `runner/.tmp/uzi-worker`: empty is disposable, any content is unknown (fail closed). In the single
// layout it IS the scratch root (`runner/.tmp`), whose leaves are classified by the scratch classifier.
import { createHash } from "node:crypto";
import { promises as fsp, type BigIntStats } from "node:fs";
import http from "node:http";
import path from "node:path";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import type { GitCache, RetainedHoldsSnapshot, RetainedLifecycleSnapshot, RetainedSourceEvidence } from "./git.js";
import type {
  CensusResult, DockerInventory, HibernationCensus, HibernationFingerprint, InventoryEntry,
} from "./hibernation-inventory.js";
import type { PodEpochOwnership } from "./pod-epoch-lock.js";
import { classifyScratchCensus, protectedLeaves, type CensusEntry, type KeptReason } from "./pod-epoch-scratch.js";
import type { RecoveryHoldsResponse, RunOwnershipResponse } from "./protocol.js";

const MAX_NODES = 500_000;
const WALK_MS = 30_000;
const MAX_ENTRIES = 10_000;
/** The api caps the whole body at 1 MiB; the entries get what is left after the envelope and the Docker block. */
const MAX_ENTRY_BYTES = 768 * 1024;
const MAX_UNSAFE_NAME_ENTRIES = 200;
const MAX_GROUPS = 200;
const MAX_REFS = 10_000;
const MAX_REF_BYTES = 256;
const MAX_PACKED_REFS_BYTES = 4 * 1024 * 1024;
const DOCKER_LIST_CAP = 5_000;
const DOCKER_BODY_CAP = 16 * 1024 * 1024;
const DOCKER_REQUEST_MS = 20_000;
const MAX_DISPLAY_PATH = 1024;

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const SHA40 = /^[0-9a-f]{40}$/;
const HEX64 = /^[0-9a-f]{64}$/;
/** The Unicode categories the api rejects: controls (Cc), format characters (Cf: soft hyphen, bidi and zero-width
 *  marks, Arabic number signs, tag characters), line and paragraph separators (Zl, Zp). */
const UNSAFE_CHARS = /\p{Cc}|\p{Cf}|\p{Zl}|\p{Zp}/u;
const RECOVERY_STORES = new Set(["recovery", "recovery-maintenance", "recovery-settlement", "recovery-archive"]);
/** Pure runtime markers with no data value, all written by templates/pod-epoch-init.cjs: `.uzi-epoch.lock` (the flock
 *  target), `.uzi-epoch-consumed` (names.ledger: the consumed pod-uid ledger) and `.uzi-pod-entry` (names.record, written
 *  by writeRecord: the current and previous pod uid). Each is re-initialised on a fresh volume. */
const RUNTIME_FILES = new Set([".uzi-epoch.lock", ".uzi-epoch-consumed", ".uzi-pod-entry"]);
/** The only files of `.uzi-authority/` that are disposable (authority-lock.ts); the authenticated store beside them is not. */
const AUTHORITY_LOCK_FILES = new Set(["worker.lock", "authority.lock"]);

export interface CensusLimits {
  /** Nodes the walk may record before the census is incomplete (default 500,000). */
  maxNodes?: number;
  /** Wall-clock budget of one walk before the census is incomplete (default 30 s). */
  walkMs?: number;
}

export interface HibernationCensusDeps {
  dataDir: string;
  /** Top-level directory names under dataDir, from config.outboxDataDir and main.ts's agent-home and jobs roots. */
  layout: { outbox: string; agentHome: string; jobs: string };
  /** The worker's own TMPDIR (absolute), when inside dataDir. It is walked and fingerprinted; in the split layout a
   *  non-empty one is unknown. */
  workerTmpDirs?: readonly string[];
  /** The scratch reclaim's root for runner leaves (scratchLayout(env).root), when the layout is the shipped one. */
  scratchRoot?: string;
  dockerHost?: string;
  git: Pick<GitCache, "discoverEpochRetainedSources" | "inspectRetainedSources" | "assertNoUnsettledDeletes" |
    "proveEpochWorktreeContent" | "collectEpochScratchProtectedPaths">;
  client: {
    listRecoveryHolds(runId: string, signal?: AbortSignal): Promise<RecoveryHoldsResponse>;
    getRunOwnership(runId: string, options: { signal: AbortSignal; deadline: number }): Promise<RunOwnershipResponse>;
  };
  ownership: Pick<PodEpochOwnership, "beginReclaim"> | undefined;
  limits?: CensusLimits;
}

// ---- walk ------------------------------------------------------------------------------------------------

interface Node {
  /** Relative path bytes decoded as latin1 (one character per byte); "." is the root. */
  rel: string;
  type: "dir" | "file" | "symlink" | "other";
  size: number;
  ctimeMs: number;
  uid: number;
  /** The fingerprint line: path, inode, size, mtime_ns, ctime_ns separated by NUL. */
  line: string;
}

interface Walk {
  nodes: Node[];
  complete: boolean;
  fingerprint: string;
}

class CensusAborted extends Error {}

const SLASH = Buffer.from("/");
const typeOf = (st: BigIntStats): Node["type"] =>
  st.isDirectory() ? "dir" : st.isSymbolicLink() ? "symlink" : st.isFile() ? "file" : "other";

async function walkVolume(dataDir: string, opts: {
  maxNodes: number; walkMs: number; signal: AbortSignal; retain: boolean;
}): Promise<Walk> {
  const root = Buffer.from(path.resolve(dataDir));
  const started = Date.now();
  const nodes: Node[] = [];
  const lines: string[] = [];
  let complete = true;
  const rootStat = await fsp.lstat(root, { bigint: true });
  if (!rootStat.isDirectory()) throw new Error("data root is not a directory");
  const record = (rel: Buffer, st: BigIntStats): void => {
    const key = rel.toString("latin1");
    const line = `${key}\0${st.ino}\0${st.size}\0${st.mtimeNs}\0${st.ctimeNs}`;
    lines.push(line);
    if (opts.retain) nodes.push({ rel: key, type: typeOf(st), size: Number(st.size), ctimeMs: Number(st.ctimeNs / 1_000_000n),
      uid: Number(st.uid), line });
  };
  record(Buffer.from("."), rootStat);
  const stack: Array<{ abs: Buffer; rel: Buffer | undefined; size: bigint }> = [{ abs: root, rel: undefined, size: rootStat.size }];
  let visited = 0;
  // A small directory (its own size is a bound on its entry count) is read in one call, which is several times faster than
  // streaming; a large one is streamed so the listing is never held whole.
  const SMALL_DIR_BYTES = 64n * 1024n;
  const entriesOf = async function* (dir: { abs: Buffer; size: bigint }): AsyncGenerator<Buffer> {
    if (dir.size <= SMALL_DIR_BYTES) {
      for (const name of await fsp.readdir(dir.abs, { encoding: "buffer" } as never) as unknown as Buffer[]) yield name;
      return;
    }
    const handle = await fsp.opendir(dir.abs, { encoding: "buffer" } as never);
    try {
      for (;;) {
        const ent = await handle.read();
        if (!ent) return;
        yield ent.name as unknown as Buffer;
      }
    } finally { await handle.close(); }
  };
  while (stack.length > 0) {
    const dir = stack.pop()!;
    try {
      for await (const name of entriesOf(dir)) {
        if ((++visited & 255) === 0) {
          if (opts.signal.aborted) throw new CensusAborted("census aborted");
          if (Date.now() - started > opts.walkMs) { complete = false; stack.length = 0; break; }
        }
        if (lines.length >= opts.maxNodes) { complete = false; stack.length = 0; break; }
        const abs = Buffer.concat([dir.abs, SLASH, name]);
        const rel = dir.rel === undefined ? name : Buffer.concat([dir.rel, SLASH, name]);
        let st: BigIntStats;
        try { st = await fsp.lstat(abs, { bigint: true }); }
        catch { complete = false; continue; }
        record(rel, st);
        // A node on another device (a mounted directory, a bind-mounted file) is a mount boundary: it is recorded, never
        // entered, and the census is incomplete.
        if (st.dev !== rootStat.dev) { complete = false; continue; }
        if (!st.isDirectory()) continue;
        stack.push({ abs, rel, size: st.size });
      }
    } catch (error) {
      if (error instanceof CensusAborted) throw error;
      complete = false;
    }
    if (opts.signal.aborted) throw new CensusAborted("census aborted");
  }
  lines.sort();
  nodes.sort((a, b) => (a.rel < b.rel ? -1 : a.rel > b.rel ? 1 : 0));
  const fingerprint = createHash("sha256").update(lines.join("\n") + "\n", "latin1").digest("hex");
  return { nodes, complete, fingerprint };
}

// ---- name and entry helpers ---------------------------------------------------------------------------------

interface Shown { text: string; clean: boolean }

/** A path for the wire: the real text when it is valid UTF-8 without a control or bidi character, else every byte that
 *  is not a visible ASCII character (and `%`) percent-escaped. */
function show(relLatin1: string): Shown {
  const bytes = Buffer.from(relLatin1, "latin1");
  let text: string | undefined;
  try { text = new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch { text = undefined; }
  if (text !== undefined && !UNSAFE_CHARS.test(text) && text.length <= MAX_DISPLAY_PATH) return { text, clean: true };
  let out = "";
  for (const b of bytes) {
    out += b > 0x20 && b < 0x7f && b !== 0x25 ? String.fromCharCode(b) : `%${b.toString(16).toUpperCase().padStart(2, "0")}`;
    if (out.length >= MAX_DISPLAY_PATH) { out = `${out.slice(0, MAX_DISPLAY_PATH - 1)}~`; break; }
  }
  return { text: out, clean: false };
}

/** A short ASCII machine token; a free-text or hostile value never reaches the wire. */
const token = (value: string, max = 64): string => {
  const safe = value.toLowerCase().replace(/[^a-z0-9_:.-]/g, "_").slice(0, max);
  return safe === "" ? "none" : safe;
};

interface EntryInput {
  kind: string;
  rel: string;
  classification: string;
  run_id?: string;
  tip?: string;
  content_state?: string;
  detail?: string;
}

/** The only constructor of a wire entry: fixed key order (the digest is over the canonical JSON) and the
 *  unsafe-name rule (an escaped path is never anything but unknown). */
function makeEntry(input: EntryInput): InventoryEntry {
  const shown = show(input.rel);
  const unsafe = !shown.clean;
  const entry: InventoryEntry = { kind: input.kind, path: shown.text, classification: unsafe ? "unknown" : input.classification };
  if (!unsafe && input.run_id !== undefined) entry.run_id = input.run_id;
  if (!unsafe && input.tip !== undefined) entry.tip = input.tip;
  if (input.content_state !== undefined) entry.content_state = input.content_state;
  const detail = unsafe ? "unsafe_name" : input.detail;
  if (detail !== undefined) entry.detail = token(detail);
  return entry;
}

// ---- tree index over the walk ---------------------------------------------------------------------------------

class Tree {
  readonly byRel = new Map<string, Node>();
  constructor(readonly nodes: Node[]) { for (const n of nodes) this.byRel.set(n.rel, n); }

  /** [from, to) of the strict descendants of `rel` in the sorted node array (a prefix range is contiguous). */
  private range(rel: string): [number, number] {
    const prefix = `${rel}/`;
    let lo = 0, hi = this.nodes.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.nodes[mid]!.rel < prefix) lo = mid + 1; else hi = mid;
    }
    let end = lo;
    while (end < this.nodes.length && this.nodes[end]!.rel.startsWith(prefix)) end++;
    return [lo, end];
  }
  descendants(rel: string): Node[] { const [a, b] = this.range(rel); return this.nodes.slice(a, b); }
  count(rel: string): number { const [a, b] = this.range(rel); return b - a; }
  /** Direct children of `rel` ("" for the data root), in sorted order. */
  children(rel: string): Node[] {
    const prefix = rel === "" ? "" : `${rel}/`;
    const [a, b] = rel === "" ? [0, this.nodes.length] : this.range(rel);
    const out: Node[] = [];
    for (let i = a; i < b; i++) {
      const n = this.nodes[i]!;
      if (n.rel === "." || n.rel.indexOf("/", prefix.length) !== -1) continue;
      out.push(n);
    }
    return out;
  }
}

const lastSegment = (rel: string): string => rel.slice(rel.lastIndexOf("/") + 1);
/** The absolute path of a latin1 relative path, as bytes (a name need not be UTF-8). */
const absBytes = (dataDir: string, rel: string): Buffer => Buffer.concat([Buffer.from(dataDir), SLASH, Buffer.from(rel, "latin1")]);

// ---- the census ------------------------------------------------------------------------------------------------

/** The retained sources discovery and inspection found: the runner phase fills it, the repos phase binds refs to it. */
interface Retained {
  sources: Array<{ barePath: string; runId: string; branch: string }>;
  complete: boolean;
}

interface Gathered {
  entries: InventoryEntry[];
  complete: boolean;
  uncovered: Set<string>;
  retained: Retained;
}

const classifyEmptyOr = (tree: Tree, node: Node, nonEmpty: string): { classification: string; detail: string } =>
  node.type === "dir" && tree.count(node.rel) === 0 ? { classification: "disposable", detail: "empty" }
    : { classification: node.type === "dir" ? nonEmpty : "unknown", detail: node.type === "dir" ? "not_empty" : "not_directory" };

export interface HibernationCensusSeams {
  census: HibernationCensus;
  fingerprint: HibernationFingerprint;
}

export function createHibernationCensus(deps: HibernationCensusDeps): HibernationCensusSeams {
  const dataDir = path.resolve(deps.dataDir);
  const maxNodes = deps.limits?.maxNodes ?? MAX_NODES;
  const walkMs = deps.limits?.walkMs ?? WALK_MS;
  const workerTmp = new Set<string>();
  for (const dir of deps.workerTmpDirs ?? []) {
    const rel = path.relative(dataDir, path.resolve(dir));
    if (rel !== "" && !rel.startsWith("..") && !path.isAbsolute(rel)) workerTmp.add(Buffer.from(rel, "utf8").toString("latin1"));
  }
  /** The digest of the last accepted census: the shutdown report carries it, never a recomputed one. */
  let censusDigest: string | undefined;

  const run = async (opts: { signal?: AbortSignal; deadlineMs: number }): Promise<CensusResult> => {
    const startedAt = Date.now();
    const deadline = startedAt + opts.deadlineMs;
    const signal = AbortSignal.any([AbortSignal.timeout(opts.deadlineMs), ...(opts.signal ? [opts.signal] : [])]);
    const out: Gathered = { entries: [], complete: true, uncovered: new Set(), retained: { sources: [], complete: true } };
    const walk = await walkVolume(dataDir, { maxNodes, walkMs, signal, retain: true });
    if (!walk.complete) out.complete = false;
    const tree = new Tree(walk.nodes);
    await classifyTop(deps, dataDir, tree, workerTmp, out, signal, deadline);
    unsafeNames(tree, out);
    // The fingerprint is the volume AFTER every proof ran: anything the census itself changed shows up here.
    const after = await walkVolume(dataDir, { maxNodes, walkMs, signal, retain: false });
    if (!after.complete) out.complete = false;
    if (after.fingerprint !== walk.fingerprint) { out.complete = false; out.uncovered.add("volume_changed_during_census"); }
    const docker = await dockerInventory(deps.dockerHost, signal, out);
    return finish(out, after.fingerprint, docker);
  };

  return {
    census: async (opts) => {
      const result = await run(opts);
      censusDigest = result.digest;
      return result;
    },
    // Only the walk: discovery, holds, content proofs and Docker are server-side or process state that can differ on an
    // untouched volume. The digest is the one stored from the accepted census.
    fingerprint: async (opts) => {
      if (censusDigest === undefined) throw new Error("no census to attest");
      const signal = AbortSignal.any([AbortSignal.timeout(opts.deadlineMs), ...(opts.signal ? [opts.signal] : [])]);
      const walk = await walkVolume(dataDir, { maxNodes, walkMs, signal, retain: false });
      if (!walk.complete) throw new Error("fingerprint walk incomplete");
      return { digest: censusDigest, fingerprint: walk.fingerprint };
    },
  };
}

function finish(out: Gathered, fingerprint: string, docker: DockerInventory | null): CensusResult {
  let entries = out.entries;
  const key = (e: InventoryEntry) => `${e.path}\0${e.kind}\0${e.detail ?? ""}`;
  const order = (a: InventoryEntry, b: InventoryEntry) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0);
  entries.sort(order);
  let bytes = 0;
  for (const e of entries) bytes += Buffer.byteLength(JSON.stringify(e)) + 1;
  if (entries.length > MAX_ENTRIES || bytes > MAX_ENTRY_BYTES) {
    // Keep every blocker first; the disposable entries are what is dropped.
    out.complete = false;
    const kept: InventoryEntry[] = [];
    let used = 0;
    const ranked = [...entries.filter(e => e.classification !== "disposable"), ...entries.filter(e => e.classification === "disposable")];
    for (const e of ranked) {
      const size = Buffer.byteLength(JSON.stringify(e)) + 1;
      if (kept.length >= MAX_ENTRIES || used + size > MAX_ENTRY_BYTES) break;
      kept.push(e); used += size;
    }
    entries = kept.sort(order);
  }
  const digest = createHash("sha256").update(JSON.stringify(entries)).digest("hex");
  return { complete: out.complete, entries, fingerprint, digest, docker, uncovered: [...out.uncovered].sort() };
}

/** One unknown_path entry per node whose own name carries a control or bidi character (never inside one already listed). */
function unsafeNames(tree: Tree, out: Gathered): void {
  let listed = 0;
  let skipPrefix: string | undefined;
  const already = new Set(out.entries.map(e => e.path));
  for (const n of tree.nodes) {
    if (n.rel === ".") continue;
    if (skipPrefix !== undefined && n.rel.startsWith(skipPrefix)) continue;
    const text = (() => {
      try { return new TextDecoder("utf-8", { fatal: true }).decode(Buffer.from(lastSegment(n.rel), "latin1")); }
      catch { return undefined; }
    })();
    if (text !== undefined && !UNSAFE_CHARS.test(text)) continue;
    if (++listed > MAX_UNSAFE_NAME_ENTRIES) { out.complete = false; return; }
    skipPrefix = `${n.rel}/`;
    // A node an earlier phase already listed (its own entry is unknown/unsafe_name too) is not listed twice.
    if (already.has(show(n.rel).text)) continue;
    out.entries.push(makeEntry({ kind: "unknown_path", rel: n.rel, classification: "unknown", detail: "unsafe_name" }));
  }
}

async function classifyTop(deps: HibernationCensusDeps, dataDir: string, tree: Tree, workerTmp: ReadonlySet<string>,
  out: Gathered, signal: AbortSignal, deadline: number): Promise<void> {
  const { layout } = deps;
  // The retained sources come first: the repos phase binds refs to them and the runner phase skips what they claim.
  const claimed = await classifyRetained(deps, dataDir, tree, out, signal, deadline);
  for (const node of tree.children("")) {
    const name = node.rel;
    const aggregate = (kind: string, classification: string, detail: string) =>
      out.entries.push(makeEntry({ kind, rel: name, classification, detail }));
    if (name === "runner") {
      if (node.type === "dir") await remainingRunner(deps, dataDir, tree, node, claimed, workerTmp, out, signal, deadline);
      else out.entries.push(makeEntry({ kind: "unknown_path", rel: name, classification: "unknown", detail: node.type }));
      continue;
    }
    if (name === "repos") { await classifyRepos(dataDir, tree, node, out); continue; }
    if (RECOVERY_STORES.has(name)) {
      const c = classifyEmptyOr(tree, node, "protected");
      aggregate("journal", c.classification, c.detail);
    } else if (name === layout.outbox) {
      const c = classifyEmptyOr(tree, node, "protected");
      aggregate("outbox", c.classification, c.detail);
    } else if (name === layout.agentHome) {
      const c = classifyEmptyOr(tree, node, "unknown");
      aggregate("agent_home", c.classification, c.detail);
    } else if (name === layout.jobs) {
      const c = classifyEmptyOr(tree, node, "unknown");
      aggregate("jobs", c.classification, c.detail);
    } else if (name === "lost+found") {
      const c = classifyEmptyOr(tree, node, "unknown");
      aggregate("lost_found", c.classification, c.detail);
    } else if (name === ".uzi-authority" && node.type === "dir") {
      const locksOnly = tree.descendants(name).every(n => n.type === "file" && AUTHORITY_LOCK_FILES.has(lastSegment(n.rel)) &&
        n.rel === `${name}/${lastSegment(n.rel)}`);
      aggregate("runtime_lock", locksOnly ? "disposable" : "unknown", locksOnly ? "lock_files" : "authority_store");
    } else if (RUNTIME_FILES.has(name) && node.type === "file") {
      aggregate("runtime_lock", "disposable", "epoch_runtime");
    } else {
      aggregate("unknown_path", "unknown", node.type === "dir" ? `nodes_${tree.count(name)}` : node.type);
    }
  }
}

// ---- repos/ ---------------------------------------------------------------------------------------------------

async function classifyRepos(dataDir: string, tree: Tree, node: Node, out: Gathered): Promise<void> {
  if (node.type !== "dir") {
    out.entries.push(makeEntry({ kind: "unknown_path", rel: node.rel, classification: "unknown", detail: node.type }));
    return;
  }
  const retained = out.retained;
  for (const child of tree.children(node.rel)) {
    if (child.type !== "dir" || !child.rel.endsWith(".git")) {
      out.entries.push(makeEntry({ kind: "unknown_path", rel: child.rel, classification: "unknown",
        detail: child.type === "dir" ? `nodes_${tree.count(child.rel)}` : child.type }));
      continue;
    }
    const why = await protectedBareReason(dataDir, tree, child);
    out.entries.push(makeEntry({ kind: "bare_repo", rel: child.rel,
      classification: why === "unreadable" ? "unknown" : why ? "protected" : retained?.complete === false ? "unknown" : "disposable",
      detail: why ?? (retained?.complete === false ? "retained_unread" : "cache") }));
    await runnerRefs(dataDir, tree, child, retained, out);
  }
}

/** Ref namespaces whose objects are recovery or settlement state (git.ts: refs/uzi-archive, refs/uzi-settle,
 *  refs/uzi-recovery-pin, refs/uzi-owed). */
const PROTECTED_REF_DIRS = ["uzi-archive", "uzi-settle", "uzi-recovery-pin", "uzi-owed"];
const PROTECTED_PACKED_REF = / refs\/uzi-(?:archive|settle|recovery-pin|owed)\//;

/** Why a bare cache is more than a cache: owed records, a ref in a recovery namespace (loose or packed) or a code
 *  snapshot. "unreadable" when the packed refs could not be read. */
async function protectedBareReason(dataDir: string, tree: Tree, bare: Node): Promise<string | undefined> {
  if (tree.children(`${bare.rel}/uzi-owed`).length > 0) return "owed_records";
  if (tree.children(`${bare.rel}/uzi-code-snapshots`).length > 0) return "code_snapshots";
  for (const dir of PROTECTED_REF_DIRS) {
    if (tree.descendants(`${bare.rel}/refs/${dir}`).some(n => n.type !== "dir")) return "recovery_refs";
  }
  const packed = tree.byRel.get(`${bare.rel}/packed-refs`);
  if (!packed) return undefined;
  if (packed.type !== "file" || packed.size > MAX_PACKED_REFS_BYTES) return "unreadable";
  try {
    const text = await fsp.readFile(absBytes(dataDir, packed.rel), "latin1");
    return text.split("\n").some(line => PROTECTED_PACKED_REF.test(line)) ? "recovery_refs" : undefined;
  } catch { return "unreadable"; }
}

interface RefRead { name: string; tip: string | undefined; why?: string }

/** refs/uzi-runner/* of one bare, in process: loose ref files (typed, a few bytes) over packed-refs. */
async function runnerRefs(dataDir: string, tree: Tree, bare: Node, retained: Retained | undefined, out: Gathered): Promise<void> {
  const refs = new Map<string, RefRead>();
  const bareAbs = absBytes(dataDir, bare.rel).toString("utf8");
  // Packed refs first, so a loose ref (written later, and always newer) replaces its packed twin.
  const packedNode = tree.byRel.get(`${bare.rel}/packed-refs`);
  if (packedNode) {
    if (packedNode.type !== "file" || packedNode.size > MAX_PACKED_REFS_BYTES) {
      out.complete = false;
      refs.set("packed-refs", { name: "packed-refs", tip: undefined, why: "packed_refs_unreadable" });
    } else {
      try {
        const text = await fsp.readFile(absBytes(dataDir, `${bare.rel}/packed-refs`), "latin1");
        for (const line of text.split("\n")) {
          const m = /^([0-9a-f]{40}|[0-9a-f]{64}) (refs\/uzi-runner\/\S+)$/.exec(line);
          if (m) refs.set(m[2]!, { name: m[2]!, tip: m[1]! });
          else if (/uzi-runner/.test(line) && !line.startsWith("#") && !line.startsWith("^")) {
            refs.set(line, { name: line, tip: undefined, why: "packed_ref_malformed" });
          }
        }
      } catch { out.complete = false; refs.set("packed-refs", { name: "packed-refs", tip: undefined, why: "packed_refs_unreadable" }); }
    }
  }
  const loose = tree.descendants(`${bare.rel}/refs/uzi-runner`);
  for (const n of loose) {
    if (n.type === "dir") continue;
    const name = n.rel.slice(bare.rel.length + 1);
    if (n.type !== "file" || n.size > MAX_REF_BYTES) { refs.set(name, { name, tip: undefined, why: "ref_not_plain" }); continue; }
    try {
      const text = (await fsp.readFile(absBytes(dataDir, n.rel), "latin1")).trim();
      refs.set(name, SHA40.test(text) || HEX64.test(text) ? { name, tip: text } : { name, tip: undefined, why: "ref_not_object_id" });
    } catch { out.complete = false; refs.set(name, { name, tip: undefined, why: "ref_unreadable" }); }
  }
  if (refs.size > MAX_REFS) out.complete = false;
  let listed = 0;
  for (const ref of [...refs.values()].sort((a, b) => (a.name < b.name ? -1 : 1))) {
    if (++listed > MAX_REFS) break;
    const rel = `${bare.rel}/${ref.name}`;
    const runs = new Set((retained?.sources ?? [])
      .filter(s => s.barePath === bareAbs && `refs/uzi-runner/${s.branch}` === ref.name)
      .map(s => s.runId));
    const runId = runs.size === 1 ? [...runs][0]! : undefined;
    if (ref.tip === undefined) {
      out.entries.push(makeEntry({ kind: "runner_ref", rel, classification: "unknown", content_state: "ref", detail: ref.why ?? "ref_unread" }));
    } else if (runId !== undefined && UUID.test(runId) && SHA40.test(ref.tip)) {
      out.entries.push(makeEntry({ kind: "runner_ref", rel, classification: "needs_receipt", run_id: runId, tip: ref.tip,
        content_state: "ref", detail: "tracking_ref" }));
    } else {
      out.entries.push(makeEntry({ kind: "runner_ref", rel, classification: "unknown", content_state: "ref",
        detail: runs.size > 1 ? "ambiguous_run" : runId === undefined ? "unbound_ref" : "run_or_tip_invalid" }));
    }
  }
}

// ---- runner/ ----------------------------------------------------------------------------------------------------

const holdsOf = (response: RecoveryHoldsResponse, runId: string): RetainedHoldsSnapshot => {
  if (!response || response.run_id !== runId || !Array.isArray(response.holds) || response.holds.length > 256) return { state: "unknown" };
  const holds: Array<{ id: string; generation: number }> = [];
  for (const h of response.holds) {
    if (!h || typeof h.hold_id !== "string" || h.hold_id.length === 0 || h.hold_id.length > 160 ||
        !Number.isSafeInteger(h.generation) || h.generation < 0) return { state: "unknown" };
    holds.push({ id: h.hold_id, generation: h.generation });
  }
  return { state: "read", holds };
};

interface Verdict { classification: string; detail: string; recoverable: boolean }

/** The existing disposition of one source mapped onto the wire classes. `recoverable` marks the one case that proceeds to
 *  the content proof: a source the reclaim machinery itself would let a discard take. */
function dispositionOf(src: RetainedSourceEvidence): Verdict {
  const no = (classification: string, detail: string): Verdict => ({ classification, detail, recoverable: false });
  if (src.presence === "unsafe") return no("unknown", "unsafe_source");
  if (src.kind === "protected") return no("protected", "protected_descriptor");
  if (src.localRecords.length > 0) return no("protected", "local_custody_record");
  if (src.holds.state !== "read") return no("unknown", "holds_unknown");
  if (src.holds.ids.length > 0) return no("protected", "open_hold");
  // The reclaim's two bases: the ordinary disposition, or the strict-plain one an epoch inspection adds.
  if (src.disposition?.kind === "local_discard" || src.epochSettlement?.disposition.kind === "local_discard") {
    return { classification: "disposable", detail: "plain_stopped", recoverable: true };
  }
  const disposition = src.disposition ?? src.epochSettlement?.disposition;
  switch (disposition?.kind) {
    case "local_protected_discard": return no("protected", "protected_discard");
    case "owner_hold_discard": return no("protected", "owner_hold");
    case "resolve_local_record": return no("protected", "local_custody_record");
    case "refused": return no("unknown", "disposition_refused");
    default: return no("unknown", "no_disposition");
  }
}

/** Phase one of the runner/ and repos/ census, independent of which of them exists: unsettled deletes and the retained
 *  sources. Returns the volume-relative paths the retained sources claim. */
async function classifyRetained(deps: HibernationCensusDeps, dataDir: string, tree: Tree,
  out: Gathered, signal: AbortSignal, deadline: number): Promise<Set<string>> {
  const rel = (p: string): string | undefined => {
    const r = path.relative(dataDir, path.resolve(p));
    return r === "" || r.startsWith("..") || path.isAbsolute(r) ? undefined : Buffer.from(r, "utf8").toString("latin1");
  };
  // 1. An unsettled runner delete anywhere in this process.
  try { deps.git.assertNoUnsettledDeletes(); }
  catch { out.entries.push(makeEntry({ kind: "unsettled_op", rel: "runner", classification: "protected", detail: "unsettled_runner_delete" })); }

  // 2. Retained sources: the reclaim's own read-only discovery and inspection.
  const claimed = new Set<string>();
  const retained = out.retained;
  const held: { scope?: PodEpochReclaimScope; failed: boolean } = { failed: false };
  const openScope = (): PodEpochReclaimScope | undefined => {
    if (held.scope || held.failed) return held.scope;
    try { held.scope = deps.ownership?.beginReclaim?.(); } catch { held.scope = undefined; }
    if (!held.scope) { held.failed = true; out.uncovered.add("content_proof_unavailable"); }
    return held.scope;
  };
  try {
    const discovered = await deps.git.discoverEpochRetainedSources({ signal, deadline, maxPhysicalPaths: MAX_GROUPS });
    if (!discovered.inventoryComplete || discovered.unreadable > 0 || discovered.overflow > 0) { out.complete = false; retained.complete = false; }
    for (const group of discovered.groups.slice(0, MAX_GROUPS)) {
      if (signal.aborted) throw new CensusAborted("census aborted");
      let lifecycle: RetainedLifecycleSnapshot = { state: "unknown" };
      let holds: RetainedHoldsSnapshot = { state: "unknown" };
      try {
        const row = await deps.client.getRunOwnership(group.selector.runId, { signal, deadline });
        if (row?.status === "completed" || row?.status === "failed" || row?.status === "cancelled") lifecycle = { state: "terminal", status: row.status };
        else if (row && typeof row.status === "string") lifecycle = { state: "not_terminal", status: row.status };
        holds = holdsOf(await deps.client.listRecoveryHolds(group.selector.runId, signal), group.selector.runId);
      } catch (error) {
        if (signal.aborted) throw error;
      }
      if (lifecycle.state === "unknown" || holds.state === "unknown") { out.complete = false; retained.complete = false; }
      const inspected = await deps.git.inspectRetainedSources({
        epoch: true, maxDescriptors: 200, maxPhysicalPaths: group.paths.length, barePath: group.barePath,
        selector: group.selector, holds, lifecycle, isLivePath: () => false, signal, deadline,
        budgetMs: Math.max(0, deadline - Date.now()),
      });
      if (inspected.partial || inspected.unreadable > 0) { out.complete = false; retained.complete = false; }
      for (const src of inspected.sources) {
        retained.sources.push({ barePath: src.barePath, runId: src.runId, branch: src.branch });
        const verdict = dispositionOf(src);
        for (const part of src.sources) {
          const partRel = rel(part.clonePath);
          if (partRel === undefined) { out.complete = false; continue; }
          claimed.add(partRel);
          // The `.uzi-skills-<clone>` sibling is claimed (the runner phase skips it) and listed here: its bytes are
          // unknown unless it is empty, and nothing is listed when it is absent.
          const skillsRel = rel(path.join(path.dirname(part.clonePath), `.uzi-skills-${path.basename(part.clonePath)}`));
          if (skillsRel !== undefined) {
            claimed.add(skillsRel);
            const skills = tree.byRel.get(skillsRel);
            if (skills) {
              const c = classifyEmptyOr(tree, skills, "unknown");
              out.entries.push(makeEntry({ kind: "unknown_path", rel: skillsRel, classification: c.classification,
                detail: c.classification === "disposable" ? "skills_empty" : "skills_not_empty" }));
            }
          }
          const kind = src.kind === "canonical" ? "canonical_clone" : src.kind === "protected" ? "journal" : "retained_attempt";
          if (part.presence === "missing") {
            out.entries.push(makeEntry({ kind, rel: partRel, classification: verdict.recoverable ? "disposable" : verdict.classification,
              detail: verdict.recoverable ? "source_gone" : verdict.detail }));
            continue;
          }
          if (part.presence === "unsafe" || !verdict.recoverable) {
            out.entries.push(makeEntry({ kind, rel: partRel, classification: part.presence === "unsafe" ? "unknown" : verdict.classification,
              content_state: "none", detail: part.presence === "unsafe" ? "unsafe_source" : verdict.detail }));
            continue;
          }
          const proven = await proveWorktree(deps, openScope(), group, part.clonePath, signal, deadline, out);
          if (!proven.proven) {
            out.entries.push(makeEntry({ kind, rel: partRel, classification: "unknown", content_state: "unproven_worktree_bytes",
              detail: proven.reason }));
            continue;
          }
          if (UUID.test(src.runId) && proven.head !== null && SHA40.test(proven.head)) {
            out.entries.push(makeEntry({ kind, rel: partRel, classification: "needs_receipt", run_id: src.runId, tip: proven.head,
              content_state: "content_proven", detail: "plain_stopped" }));
          } else {
            out.entries.push(makeEntry({ kind, rel: partRel, classification: "unknown", content_state: "content_proven",
              detail: "run_or_tip_invalid" }));
          }
          const loose = unaccountedNodes(tree, partRel, proven.trackedPaths);
          if (loose > 0) {
            out.entries.push(makeEntry({ kind: "worktree", rel: partRel, classification: "unknown",
              content_state: "unproven_worktree_bytes", detail: `unaccounted_${loose}` }));
          }
        }
      }
    }
  } catch (error) {
    if (error instanceof CensusAborted || signal.aborted) throw error;
    out.complete = false; retained.complete = false;
  } finally {
    try { held.scope?.finish(); } catch { /* a stopped or pending scope is the controller's exclusion check to see */ }
  }
  return claimed;
}

async function proveWorktree(deps: HibernationCensusDeps, scope: PodEpochReclaimScope | undefined,
  group: { barePath: string; selector: { runId: string; generation: number | "none"; attempt: string } }, clonePath: string,
  signal: AbortSignal, deadline: number, out: Gathered): Promise<{ proven: boolean; head: string | null; trackedPaths: Set<string> | null; reason: string }> {
  if (!scope) return { proven: false, head: null, trackedPaths: null, reason: "content_proof_unavailable" };
  const result = await deps.git.proveEpochWorktreeContent({ scope, barePath: group.barePath, selector: group.selector, clonePath, signal, deadline });
  if (!result.proven) return { proven: false, head: null, trackedPaths: null, reason: result.reason ?? "content_unproven" };
  if (result.head === null || result.trackedPaths === null) {
    out.complete = false;
    return { proven: false, head: null, trackedPaths: null, reason: "proof_without_head" };
  }
  return { proven: true, head: result.head, trackedPaths: result.trackedPaths, reason: "content_proven" };
}

/** Non-directory nodes under the clone that the proof did not account for: not a tracked path and
 *  not under `.git` (the proof's shape stage allowlisted and typed every entry of it). */
function unaccountedNodes(tree: Tree, cloneRel: string, tracked: ReadonlySet<string> | null): number {
  if (!tracked) return 0;
  const dirs = new Set<string>();
  for (const p of tracked) {
    for (let i = p.indexOf("/"); i !== -1; i = p.indexOf("/", i + 1)) dirs.add(p.slice(0, i));
  }
  const prefix = `${cloneRel}/`;
  let count = 0;
  for (const n of tree.descendants(cloneRel)) {
    const inner = n.rel.slice(prefix.length);
    if (inner === ".git" || inner.startsWith(".git/")) continue;
    if (tracked.has(inner) || dirs.has(inner)) continue;
    // A directory holds no bytes of its own: the worker provisions an empty, excluded `.uzi/scratch` in every clone.
    // What counts is every file, link or special node an unaccounted directory (or the root) contains.
    if (n.type === "dir") continue;
    count++;
  }
  return count;
}

async function remainingRunner(deps: HibernationCensusDeps, dataDir: string, tree: Tree, runner: Node, claimed: ReadonlySet<string>,
  workerTmp: ReadonlySet<string>, out: Gathered, signal: AbortSignal, deadline: number): Promise<void> {
  for (const top of tree.children(runner.rel)) {
    if (top.rel === `${runner.rel}/.tmp`) { await classifyScratch(deps, dataDir, tree, top, workerTmp, out, signal, deadline); continue; }
    if (top.type !== "dir") {
      out.entries.push(makeEntry({ kind: "unknown_path", rel: top.rel, classification: "unknown", detail: top.type }));
      continue;
    }
    const children = tree.children(top.rel);
    if (children.length === 0) {
      out.entries.push(makeEntry({ kind: "unknown_path", rel: top.rel, classification: "disposable", detail: "empty" }));
      continue;
    }
    for (const child of children) {
      if (claimed.has(child.rel)) continue;
      // A directory with its own `.git` that no retained source claims is a canonical clone nobody can bind to a run.
      const bareClone = child.type === "dir" && tree.byRel.get(`${child.rel}/.git`)?.type === "dir";
      out.entries.push(makeEntry({ kind: bareClone ? "canonical_clone" : "unknown_path", rel: child.rel, classification: "unknown",
        detail: bareClone ? "unbound_clone" : child.type === "dir" ? `nodes_${tree.count(child.rel)}` : child.type }));
    }
  }
}

// ---- runner scratch -----------------------------------------------------------------------------------------------

const KEPT_AS: Record<KeptReason, string> = {
  protected_name: "protected", not_directory: "unknown", name_refused: "unknown", active_helper: "protected",
  foreign_owner: "unknown", mount: "unknown", incomplete: "unknown", current_pod: "protected",
  protected_record: "protected", bind_referenced: "protected", held_open: "protected", cap: "unknown",
};

async function classifyScratch(deps: HibernationCensusDeps, dataDir: string, tree: Tree, tmp: Node, workerTmp: ReadonlySet<string>,
  out: Gathered, signal: AbortSignal, deadline: number): Promise<void> {
  const entry = (rel: string, classification: string, detail: string) =>
    out.entries.push(makeEntry({ kind: "runner_scratch", rel, classification, detail }));
  if (tmp.type !== "dir") { entry(tmp.rel, "unknown", tmp.type); return; }
  const rootRel = deps.scratchRoot === undefined ? undefined : (() => {
    const r = path.relative(dataDir, path.resolve(deps.scratchRoot));
    return r === "" || r.startsWith("..") || path.isAbsolute(r) ? undefined : Buffer.from(r, "utf8").toString("latin1");
  })();
  if (rootRel === undefined) out.uncovered.add("scratch_layout_unavailable");
  const leafRoot = rootRel !== undefined ? tree.byRel.get(rootRel) : undefined;
  // Single layout: the scratch root IS runner/.tmp (and the worker's TMPDIR), so its children are leaves, classified below.
  for (const child of leafRoot?.rel === tmp.rel ? [] : tree.children(tmp.rel)) {
    if (workerTmp.has(child.rel)) {
      // The worker's own TMPDIR (split layout) is walked and fingerprinted; only an empty directory is disposable.
      const empty = child.type === "dir" && tree.count(child.rel) === 0;
      entry(child.rel, empty ? "disposable" : "unknown", empty ? "worker_tmpdir_empty" : "worker_tmpdir_not_empty");
      continue;
    }
    if (child.rel === rootRel) continue;
    // Not the runner's leaf root and not the worker's own TMPDIR: nobody here claims it.
    if (child.rel !== leafRoot?.rel) entry(child.rel, tree.count(child.rel) === 0 && child.type === "dir" ? "disposable" : "unknown",
      tree.count(child.rel) === 0 && child.type === "dir" ? "empty" : "unrecognized_scratch_name");
  }
  if (!leafRoot) return;
  if (leafRoot.type !== "dir") { entry(leafRoot.rel, "unknown", leafRoot.type); return; }
  const leaves = tree.children(leafRoot.rel);
  let stored: string[] = [];
  if (leaves.length > 0) {
    try {
      const protectedPaths = await deps.git.collectEpochScratchProtectedPaths({ signal, deadline });
      if (protectedPaths.state === "read") stored = protectedPaths.paths;
      else { out.complete = false; entry(leafRoot.rel, "unknown", "protected_records_unreadable"); return; }
    } catch (error) {
      if (signal.aborted) throw error;
      out.complete = false; entry(leafRoot.rel, "unknown", "protected_records_unreadable"); return;
    }
  }
  const census: CensusEntry[] = leaves.map((leaf) => {
    const below = tree.descendants(leaf.rel);
    return {
      name: lastSegment(leaf.rel), type: leaf.type === "dir" ? "dir" : "other", devIno: `0:${leaf.rel}`, uid: leaf.uid,
      fsDiffers: false, foreignUid: below.some(n => n.uid !== leafRoot.uid), bytes: below.reduce((s, n) => s + n.size, leaf.size),
      sharedBytes: 0, maxCtimeMs: Math.max(leaf.ctimeMs, ...below.map(n => n.ctimeMs)), incomplete: false,
    };
  });
  const names = census.map(c => c.name);
  const censusByName = new Map(census.map(c => [c.name, c] as const));
  const abs = absBytes(dataDir, leafRoot.rel).toString("utf8");
  const classified = classifyScratchCensus({
    census: { rootDevIno: "", rootUid: leafRoot.uid, entries: census, overflow: false },
    // Nothing of this pod is current any more (writers are excluded), so no leaf is held back for its age.
    t0Ms: Number.POSITIVE_INFINITY,
    mountedLeaves: new Set(), boundLeaves: new Set(), heldLeaves: new Set(),
    protectedLeaves: protectedLeaves(stored, abs, names),
  });
  const admitted = new Set(classified.admitted.map(a => a.name));
  let disposable = 0;
  for (const leaf of leaves) {
    const name = lastSegment(leaf.rel);
    if (admitted.has(name)) { disposable++; continue; }
    // Re-derive the single kept reason with the same classifier on this leaf alone.
    const one = classifyScratchCensus({
      census: { rootDevIno: "", rootUid: leafRoot.uid, entries: [censusByName.get(name)!], overflow: false },
      t0Ms: Number.POSITIVE_INFINITY, mountedLeaves: new Set(), boundLeaves: new Set(), heldLeaves: new Set(),
      protectedLeaves: protectedLeaves(stored, abs, [name]),
    });
    const reason = (Object.entries(one.kept) as Array<[KeptReason, number]>).find(([, n]) => n > 0)?.[0] ?? "cap";
    entry(leaf.rel, KEPT_AS[reason], reason);
  }
  if (disposable > 0 || leaves.length === 0) {
    entry(leafRoot.rel, "disposable", leaves.length === 0 ? "empty" : `leaves_${disposable}`);
  }
}

// ---- docker -------------------------------------------------------------------------------------------------------

function dockerEndpoint(host: string): http.RequestOptions | undefined {
  if (/^unix:\/\/[^?#\s]+$/.test(host)) return { socketPath: host.slice(7) };
  const m = /^tcp:\/\/([^/?#@\s]+):(\d{1,5})$/.exec(host);
  if (!m) return undefined;
  const port = Number(m[2]);
  if (port < 1 || port > 65535) return undefined;
  return { hostname: m[1]!.replace(/^\[|\]$/g, ""), port };
}

/** One GET, bounded in time and in bytes. Any non-200 or oversized or non-JSON response throws. */
function dockerGet(target: http.RequestOptions, p: string, signal: AbortSignal): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const req = http.request({ ...target, method: "GET", path: p, agent: false, maxHeaderSize: 16 * 1024 }, (res) => {
      if (res.statusCode !== 200) { res.resume(); reject(new Error(`docker ${p} status ${res.statusCode}`)); return; }
      const chunks: Buffer[] = [];
      let size = 0;
      res.on("data", (chunk: Buffer) => {
        size += chunk.length;
        if (size > DOCKER_BODY_CAP) { req.destroy(new Error("docker body cap")); return; }
        chunks.push(chunk);
      });
      res.on("error", reject);
      res.on("end", () => {
        try { resolve(JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks)))); }
        catch { reject(new Error("docker body is not JSON")); }
      });
    });
    const timer = setTimeout(() => req.destroy(new Error("docker request timeout")), DOCKER_REQUEST_MS);
    const abort = () => req.destroy(new Error("docker request aborted"));
    signal.addEventListener("abort", abort, { once: true });
    req.on("error", reject);
    req.on("close", () => { clearTimeout(timer); signal.removeEventListener("abort", abort); });
    req.end();
  });
}

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const wire = (value: unknown, max: number): string => {
  const s = typeof value === "string" ? value : "";
  const shown = show(Buffer.from(s, "utf8").toString("latin1"));
  return shown.text.slice(0, max);
};

/** Read-only Docker view (DinD lane). Any failure makes the census incomplete and the block null: an unknown daemon is not an empty one. */
async function dockerInventory(host: string | undefined, signal: AbortSignal, out: Gathered): Promise<DockerInventory | null> {
  const target = host === undefined ? undefined : dockerEndpoint(host);
  if (!target) { out.complete = false; return null; }
  try {
    const containers = await dockerGet(target, "/containers/json?all=1", signal);
    const volumes = await dockerGet(target, "/volumes", signal);
    const images = await dockerGet(target, "/images/json", signal);
    const df = await dockerGet(target, "/system/df", signal);
    const volumeList = isRecord(volumes) ? volumes.Volumes ?? [] : undefined;
    if (!Array.isArray(containers) || !Array.isArray(volumeList) || !Array.isArray(images) || !isRecord(df)) {
      throw new Error("docker response shape");
    }
    if (containers.length > DOCKER_LIST_CAP || volumeList.length > DOCKER_LIST_CAP) { out.complete = false; }
    const result: DockerInventory = {
      containers: containers.slice(0, DOCKER_LIST_CAP).map((c) => {
        const rec = isRecord(c) ? c : {};
        const names = Array.isArray(rec.Names) ? rec.Names : [];
        return {
          id: wire(rec.Id, 64).slice(0, 12),
          state: token(typeof rec.State === "string" ? rec.State : "unknown"),
          name: wire(typeof names[0] === "string" ? names[0].replace(/^\//, "") : "", 128),
        };
      }),
      volumes: volumeList.slice(0, DOCKER_LIST_CAP).map((v) => {
        const rec = isRecord(v) ? v : {};
        const name = typeof rec.Name === "string" ? rec.Name : "";
        const labels = isRecord(rec.Labels) ? rec.Labels : {};
        return { name: wire(name, 128), anonymous: Object.hasOwn(labels, "com.docker.volume.anonymous") || HEX64.test(name) };
      }),
      images_count: images.length,
      build_cache_bytes: (Array.isArray(df.BuildCache) ? df.BuildCache : []).reduce<number>((sum, item) => {
        const size = isRecord(item) && typeof item.Size === "number" && Number.isSafeInteger(item.Size) && item.Size >= 0 ? item.Size : 0;
        return sum + size;
      }, 0),
    };
    return result;
  } catch (error) {
    if (signal.aborted) throw new CensusAborted("census aborted");
    void error;
    out.complete = false;
    return null;
  }
}
