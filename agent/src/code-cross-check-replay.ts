// Maintainer-run OFFLINE replay harness for the code cross-check (PRD 3167 M1). It drives the
// existing checker classes unchanged against a frozen corpus of reconstructed pre-fix snapshots and
// scores their findings with a documented proxy. Nothing here is wired into the production runner.
import fs from "node:fs/promises";
import { REPLAY_PROFILES, type ReplayProfile } from "./code-cross-check-replay-profiles.js";
import { constants as FS, type Stats } from "node:fs";
import path from "node:path";
import { execFile } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import type { ClaimResponse } from "./protocol.js";
import type { Logger } from "./log.js";
import type { WorkerClient } from "./client.js";
import { ClaudeCrossCheck } from "./claude-cross-check.js";
import { CodexCrossCheck, CrossCheckMalformedError, codeCrossCheckBrief, validateCodeFindings } from "./codex/cross-check.js";
import { CrossCheckCheckerUnavailableError } from "./codex/model-rejection.js";
import { CrossCheckConfinementError } from "./cross-check-confinement.js";
import { decodeCrossCheckClaim, decodeCodeFindings, type CodeFinding } from "./code-cross-check-contract.js";
import { deriveCodexRunCost } from "./codex/token-accounting.js";
import type { CodexModelUsageEntry } from "./codex/token-accounting.js";
import { SECRET_PATH_PREFIXES } from "./guardrails.js";
import { CODEX_SESSION_GID, COMMAND_UID, RUNNER_UID, uidSplitActive } from "./runner-uid.js";
import { rmHomeTree } from "./rmtree.js";

const replayLog: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return replayLog; },
};

export const CORPUS_DIR = "fixtures/code-cross-check-replay";
/** Source-pinned sha256 of fixtures/code-cross-check-replay/manifest.json. Re-pin when the corpus changes. */
export const CORPUS_MANIFEST_SHA256 = "940db1d7ffba8515370bc581af4224644545dfd60d21c6449a2f5d0e94156f3a";
export const REPLAY_DIFF_ARGS = ["--no-ext-diff", "--no-textconv", "--no-color", "--abbrev=8"] as const;
const REPLAY_IDENTITY = { name: "uzi replay", email: "replay@uzi.invalid", date: "2026-01-01T00:00:00Z" } as const;
export const READER_IMPLEMENTATION = { codex: "codex fileop Read+Search", claude: "claude sdk Read+Grep+Glob" } as const;
export const CREDENTIAL_MODE = { codex: "api_key", claude: "oauth_token" } as const;
export const BRIEF_TOOLS = { codex: "Read and Search", claude: "Read, Grep and Glob" } as const;

export type ReplayFamily = "claude" | "codex";
export const REPLAY_EFFORTS = ["low", "medium", "high", "xhigh", "max"] as const;
export type ReplayEffort = (typeof REPLAY_EFFORTS)[number];
export const REPLAY_OUTCOMES = [
  "completed", "model_timeout", "malformed", "model_error", "checker_unavailable",
  "missing_auth", "interrupted", "confinement_failed",
] as const;
export type ReplayOutcome = (typeof REPLAY_OUTCOMES)[number];

export type ReplayErrorCode =
  | "corpus_integrity" | "brief_drift" | "diff_oversize" | "corpus_objects_missing"
  | "invalid_options" | "records_invalid" | "score_unknown_case"
  | "adjudication_invalid" | "adjudication_duplicate" | "adjudication_unknown_finding"
  | "snapshot_untraversable" | "cleanup_unconfirmed";

/** The message is always the fixed code; file contents and git output are never echoed. */
export class ReplayError extends Error {
  constructor(readonly code: ReplayErrorCode) {
    super(code);
    this.name = "ReplayError";
  }
}

const MIB = 1024 * 1024;
const ASSET_MAX = MIB;
const PLAN_MAX = 256 * 1024;
const TITLE_MAX = 4096;
const BODY_MAX = 256 * 1024;

export interface ExpectedDefect {
  id: string;
  paths: string[];
  line_range: [number, number] | null;
  use_line_range: boolean;
  match: string[];
  detectability: "read_only" | "execution_only";
  rationale: string;
  in_diff: boolean;
  signature: { path: string; literal: string; present: boolean; witness: string | null };
}
export interface ReplayWitness { file: string; path: string; blob_id: string; sha256: string }
export interface ReplayInputs { issueTitle: string; issueBody: string; plan: string; diff: string }
export interface ReplayCase {
  id: string;
  kind: "defect" | "control";
  input_provenance: "reconstructed" | "recorded";
  plan_source: "synthetic" | "recorded";
  source: { pr: number; issue: number };
  squash_sha: string;
  base_sha: string;
  prefix_tree: string;
  prefix_commit: string;
  reversal: "recovered" | "inferred" | "none";
  artifacts: { "issue.md": string; "plan.md": string; "prefix.patch": string; "removed.patch": string };
  witnesses: ReplayWitness[];
  expected: ExpectedDefect[];
  inputs: ReplayInputs;
  prefixPatch: Buffer;
  removedPatch: Buffer;
}
export interface ReplayPins {
  corpus_version: string;
  brief_sha256: { codex: string; claude: string };
  reader_implementation: typeof READER_IMPLEMENTATION;
  credential_mode: typeof CREDENTIAL_MODE;
}
export interface ReplayCorpus {
  profile?: ReplayProfile;
  corpus_version: string;
  brief_sha256: { codex: string; claude: string };
  pins: ReplayPins;
  cases: ReplayCase[];
}

const sha256Hex = (data: string | Uint8Array): string => createHash("sha256").update(data).digest("hex");
const HEX40 = /^[a-f0-9]{40}$/;
const HEX64 = /^[a-f0-9]{64}$/;
const CASE_ID = /^[a-z0-9-]{1,48}$/;
const DEFECT_ID = /^[A-Za-z0-9_-]{1,32}$/;

const integrity = (): never => { throw new ReplayError("corpus_integrity"); };

/** Read at most `max` bytes (max+1 probe, chunked); null when the file is larger. Symlinks,
 * including symlinked ancestors below `root`, and non-regular files are refused. */
async function readReplayAsset(root: string, rel: string, max: number): Promise<Buffer | null> {
  try {
    let current = root;
    const rootStat = await fs.lstat(root);
    if (!rootStat.isDirectory()) return integrity();
    const segments = rel.split("/");
    for (let i = 0; i < segments.length; i++) {
      current = path.join(current, segments[i]!);
      const stat = await fs.lstat(current);
      if (stat.isSymbolicLink()) return integrity();
      if (i < segments.length - 1 ? !stat.isDirectory() : !stat.isFile()) return integrity();
    }
    const handle = await fs.open(current, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
    try {
      const chunks: Buffer[] = [];
      let used = 0;
      while (used <= max) {
        const chunk = Buffer.alloc(Math.min(65536, max + 1 - used));
        const { bytesRead } = await handle.read(chunk, 0, chunk.length, null);
        if (bytesRead === 0) break;
        chunks.push(chunk.subarray(0, bytesRead));
        used += bytesRead;
      }
      return used > max ? null : Buffer.concat(chunks);
    } finally { await handle.close(); }
  } catch (error) {
    if (error instanceof ReplayError) throw error;
    return integrity();
  }
}

/** Read an absolute operator-supplied file (records, adjudications) with the same bounds. */
export async function readReplayFile(file: string, max: number): Promise<Buffer | null> {
  const abs = path.resolve(file);
  return readReplayAsset(path.dirname(abs), path.basename(abs), max);
}

function record(v: unknown): Record<string, unknown> {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return integrity();
  return v as Record<string, unknown>;
}
function strict(v: unknown, keys: string[]): Record<string, unknown> {
  const r = record(v);
  if (Object.keys(r).sort().join(",") !== [...keys].sort().join(",")) return integrity();
  return r;
}
function str(v: unknown, max = 4096): string {
  if (typeof v !== "string" || Buffer.byteLength(v) > max) return integrity();
  return v;
}
function hex(v: unknown, re: RegExp): string {
  if (typeof v !== "string" || !re.test(v)) return integrity();
  return v;
}
function bool(v: unknown): boolean {
  if (typeof v !== "boolean") return integrity();
  return v;
}
function int(v: unknown, min = 0): number {
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < min) return integrity();
  return v;
}
function oneOf<T extends string>(v: unknown, values: readonly T[]): T {
  if (typeof v !== "string" || !values.includes(v as T)) return integrity();
  return v as T;
}
function arr(v: unknown, max: number): unknown[] {
  if (!Array.isArray(v) || v.length > max) return integrity();
  return v;
}
function parseJson(bytes: Buffer): unknown {
  try { return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)); } catch { return integrity(); }
}
function repoPath(v: unknown): string {
  const p = str(v, 512);
  if (!p || p.startsWith("/") || p.includes("\0") || p.split("/").some((s) => s === "" || s === "." || s === "..")) return integrity();
  return p;
}

function parseExpected(v: unknown, witnessFiles: Set<string>): ExpectedDefect {
  const e = strict(v, ["id", "paths", "line_range", "use_line_range", "match", "detectability", "rationale", "in_diff", "signature"]);
  const id = str(e.id, 32);
  if (!DEFECT_ID.test(id)) return integrity();
  const paths = arr(e.paths, 16).map(repoPath);
  if (paths.length === 0) return integrity();
  let line_range: [number, number] | null = null;
  if (e.line_range !== null) {
    const range = arr(e.line_range, 2);
    if (range.length !== 2) return integrity();
    const a = int(range[0]);
    const b = int(range[1]);
    if (a > b) return integrity();
    line_range = [a, b];
  }
  const use_line_range = bool(e.use_line_range);
  if (use_line_range && !line_range) return integrity();
  const match = arr(e.match, 8).map((m) => {
    const source = str(m, 512);
    try { new RegExp(source, "i"); } catch { return integrity(); }
    return source;
  });
  const sig = strict(e.signature, ["path", "literal", "present", "witness"]);
  const witness = sig.witness === null ? null : str(sig.witness, 128);
  if (witness !== null && !witnessFiles.has(witness)) return integrity();
  const literal = str(sig.literal, 2048);
  if (!literal) return integrity();
  return {
    id, paths, line_range, use_line_range, match,
    detectability: oneOf(e.detectability, ["read_only", "execution_only"] as const),
    rationale: str(e.rationale, 2048), in_diff: bool(e.in_diff),
    signature: { path: repoPath(sig.path), literal, present: bool(sig.present), witness },
  };
}

function parseIssue(bytes: Buffer): { title: string; body: string } {
  let text: string;
  try { text = new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch { return integrity(); }
  const nl = text.indexOf("\n");
  if (!text.startsWith("# ") || nl < 0 || text[nl + 1] !== "\n") return integrity();
  const title = text.slice(2, nl);
  const body = text.slice(nl + 2);
  if (Buffer.byteLength(title) > TITLE_MAX || Buffer.byteLength(body) > BODY_MAX) return integrity();
  return { title, body };
}

async function loadCase(root: string, id: string, expectedSha: string): Promise<ReplayCase> {
  const caseBytes = await readReplayAsset(root, `cases/${id}/case.json`, ASSET_MAX);
  if (!caseBytes || sha256Hex(caseBytes) !== expectedSha) return integrity();
  const c = strict(parseJson(caseBytes), [
    "id", "kind", "input_provenance", "plan_source", "source", "squash_sha", "base_sha", "prefix_tree",
    "prefix_commit", "reversal", "artifacts", "witnesses", "expected",
  ]);
  if (c.id !== id || !CASE_ID.test(id)) return integrity();
  const kind = oneOf(c.kind, ["defect", "control"] as const);
  const input_provenance = oneOf(c.input_provenance, ["reconstructed", "recorded"] as const);
  const plan_source = oneOf(c.plan_source, ["synthetic", "recorded"] as const);
  const reversal = oneOf(c.reversal, ["recovered", "inferred", "none"] as const);
  const reconstructed = plan_source === "synthetic" || reversal !== "none";
  if ((input_provenance === "reconstructed") !== reconstructed) return integrity();
  const src = strict(c.source, ["pr", "issue"]);
  const art = strict(c.artifacts, ["issue.md", "plan.md", "prefix.patch", "removed.patch"]);
  const artifacts = {
    "issue.md": hex(art["issue.md"], HEX64), "plan.md": hex(art["plan.md"], HEX64),
    "prefix.patch": hex(art["prefix.patch"], HEX64), "removed.patch": hex(art["removed.patch"], HEX64),
  };
  const witnesses = arr(c.witnesses, 64).map((raw): ReplayWitness => {
    const w = strict(raw, ["file", "path", "blob_id", "sha256"]);
    const file = str(w.file, 128);
    if (!/^witness\/[A-Za-z0-9._-]{1,64}\.blob$/.test(file)) return integrity();
    return { file, path: repoPath(w.path), blob_id: hex(w.blob_id, HEX40), sha256: hex(w.sha256, HEX64) };
  });
  const witnessFiles = new Set(witnesses.map((w) => w.file));
  if (witnessFiles.size !== witnesses.length) return integrity();
  const expected = arr(c.expected, 32).map((e) => parseExpected(e, witnessFiles));
  if (new Set(expected.map((e) => e.id)).size !== expected.length) return integrity();
  if (kind === "control" ? expected.length !== 0 || reversal !== "none" : expected.length === 0) return integrity();

  // Every asset is read bounded, symlink-refusing and checksum-verified before use.
  const verified = async (name: string, max: number, want: string): Promise<Buffer> => {
    const bytes = await readReplayAsset(root, `cases/${id}/${name}`, max);
    if (!bytes || sha256Hex(bytes) !== want) return integrity();
    return bytes;
  };
  const issue = parseIssue(await verified("issue.md", TITLE_MAX + BODY_MAX + 16, artifacts["issue.md"]));
  const planBytes = await verified("plan.md", PLAN_MAX, artifacts["plan.md"]);
  // The production runner swaps a diff of 1 MiB or more for a notice; the corpus must never need that.
  const prefixPatch = await readReplayAsset(root, `cases/${id}/prefix.patch`, MIB - 1);
  if (!prefixPatch) throw new ReplayError("diff_oversize");
  if (sha256Hex(prefixPatch) !== artifacts["prefix.patch"]) return integrity();
  const removedPatch = await verified("removed.patch", ASSET_MAX, artifacts["removed.patch"]);
  for (const w of witnesses) {
    const blob = await readReplayAsset(root, `cases/${id}/${w.file}`, ASSET_MAX);
    if (!blob || sha256Hex(blob) !== w.sha256) return integrity();
    const gitId = createHash("sha1").update(`blob ${blob.byteLength}\0`).update(blob).digest("hex");
    if (gitId !== w.blob_id) return integrity();
  }
  let plan: string;
  let diff: string;
  try {
    const decoder = new TextDecoder("utf-8", { fatal: true });
    plan = decoder.decode(planBytes);
    diff = decoder.decode(prefixPatch);
  } catch { return integrity(); }
  return {
    id, kind, input_provenance, plan_source, reversal,
    source: { pr: int(src.pr, 1), issue: int(src.issue, 1) },
    squash_sha: hex(c.squash_sha, HEX40), base_sha: hex(c.base_sha, HEX40),
    prefix_tree: hex(c.prefix_tree, HEX40), prefix_commit: hex(c.prefix_commit, HEX40),
    artifacts, witnesses, expected,
    inputs: { issueTitle: issue.title, issueBody: issue.body, plan, diff },
    prefixPatch, removedPatch,
  };
}

/** Load and verify the frozen corpus. `manifestSha256` is injectable so tests can use a temp corpus. */
export async function loadReplayCorpus(checkout: string, opts: { manifestSha256?: string; profile?: ReplayProfile } = {}): Promise<ReplayCorpus> {
  const root = path.join(checkout, CORPUS_DIR);
  const manifestBytes = await readReplayAsset(root, "manifest.json", ASSET_MAX);
  if (!manifestBytes || sha256Hex(manifestBytes) !== (opts.manifestSha256 ?? CORPUS_MANIFEST_SHA256)) return integrity();
  const manifest = strict(parseJson(manifestBytes), ["corpus_version", "pins_sha256", "cases"]);
  const corpus_version = str(manifest.corpus_version, 64);
  const pinsBytes = await readReplayAsset(root, "pins.json", ASSET_MAX);
  if (!pinsBytes || sha256Hex(pinsBytes) !== hex(manifest.pins_sha256, HEX64)) return integrity();
  const p = strict(parseJson(pinsBytes), ["corpus_version", "brief_sha256", "reader_implementation", "credential_mode"]);
  if (p.corpus_version !== corpus_version) return integrity();
  const briefs = strict(p.brief_sha256, ["codex", "claude"]);
  const brief_sha256 = { codex: hex(briefs.codex, HEX64), claude: hex(briefs.claude, HEX64) };
  const reader = strict(p.reader_implementation, ["codex", "claude"]);
  const cred = strict(p.credential_mode, ["codex", "claude"]);
  if (reader.codex !== READER_IMPLEMENTATION.codex || reader.claude !== READER_IMPLEMENTATION.claude ||
      cred.codex !== CREDENTIAL_MODE.codex || cred.claude !== CREDENTIAL_MODE.claude) return integrity();
  const profile = opts.profile ?? "baseline";
  if (profile !== "baseline" && profile !== "revised") throw new ReplayError("brief_drift");
  for (const family of ["codex", "claude"] as const) {
    if (brief_sha256[family] !== REPLAY_PROFILES.baseline[family]
      || REPLAY_PROFILES[profile][family] !== sha256Hex(codeCrossCheckBrief(BRIEF_TOOLS[family]))) throw new ReplayError("brief_drift");
  }
  const entries = arr(manifest.cases, 200).map((raw) => {
    const e = strict(raw, ["id", "case_sha256"]);
    const id = str(e.id, 48);
    if (!CASE_ID.test(id)) return integrity();
    return { id, sha: hex(e.case_sha256, HEX64) };
  });
  if (new Set(entries.map((e) => e.id)).size !== entries.length) return integrity();
  const cases: ReplayCase[] = [];
  for (const e of entries) cases.push(await loadCase(root, e.id, e.sha));
  return {
    corpus_version, brief_sha256: { ...REPLAY_PROFILES[profile] }, profile, cases,
    pins: { corpus_version, brief_sha256, reader_implementation: READER_IMPLEMENTATION, credential_mode: CREDENTIAL_MODE },
  };
}

// ---------------------------------------------------------------------------------------------
// Snapshot materialization

function hermeticEnv(extra: Record<string, string> = {}): Record<string, string> {
  return {
    PATH: process.env.PATH ?? "", GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1", LANG: "C", ...extra,
  };
}
/** Fixed author and committer identity so replay commits are byte-reproducible. */
export function replayIdentityEnv(): Record<string, string> {
  return {
    GIT_AUTHOR_NAME: REPLAY_IDENTITY.name, GIT_AUTHOR_EMAIL: REPLAY_IDENTITY.email, GIT_AUTHOR_DATE: REPLAY_IDENTITY.date,
    GIT_COMMITTER_NAME: REPLAY_IDENTITY.name, GIT_COMMITTER_EMAIL: REPLAY_IDENTITY.email, GIT_COMMITTER_DATE: REPLAY_IDENTITY.date,
  };
}

/** Run git with a hermetic environment and no credentials; resolves stdout bytes. */
export function replayGit(args: string[], o: { cwd?: string; env?: Record<string, string>; signal?: AbortSignal } = {}): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    execFile("git", args, {
      cwd: o.cwd, env: hermeticEnv(o.env), encoding: "buffer", maxBuffer: 16 * MIB, timeout: 120_000, signal: o.signal,
    }, (error, stdout) => { if (error) reject(error); else resolve(stdout); });
  });
}

export interface VerifiedCase {
  /** Scratch directory holding the bare repository and indexes; removed by cleanup(). */
  work: string;
  bare: string;
  /** Exact blob bytes of `path` in the pinned prefix tree. */
  readFile(path: string): Promise<Buffer>;
  cleanup(): Promise<void>;
}

/** Prove every pin of one case against the checkout's own objects (read only, through an alternates
 * link) without exporting a snapshot: objects present, base + prefix.patch -> prefix_tree, the prefix
 * commit id, witness blob ids, removed.patch -> the squash tree, and the regenerated diff bytes. */
export async function verifyCasePins(checkout: string, c: ReplayCase, scratchDir: string, signal?: AbortSignal): Promise<VerifiedCase> {
  await fs.mkdir(scratchDir, { recursive: true });
  let work: string | undefined;
  const cleanup = async (): Promise<void> => {
    if (work) await fs.rm(work, { recursive: true, force: true });
  };
  try {
    const dir = await fs.mkdtemp(path.join(scratchDir, "replay-work-"));
    work = dir;
    const bare = path.join(dir, "bare.git");
    const objects = (await replayGit(["-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", "objects"], { signal }))
      .toString("utf8").trim();
    await replayGit(["init", "--bare", "-q", bare], { signal });
    await fs.mkdir(path.join(bare, "objects/info"), { recursive: true });
    await fs.writeFile(path.join(bare, "objects/info/alternates"), objects + "\n");
    const git = (args: string[], env?: Record<string, string>): Promise<Buffer> =>
      replayGit(["--git-dir", bare, ...args], { env, signal });
    for (const sha of [c.base_sha, c.squash_sha]) {
      try { await git(["cat-file", "-e", `${sha}^{commit}`]); } catch {
        signal?.throwIfAborted();
        throw new ReplayError("corpus_objects_missing");
      }
    }
    const idx = { GIT_INDEX_FILE: path.join(dir, "index") };
    const text = async (args: string[], env?: Record<string, string>): Promise<string> =>
      (await git(args, env)).toString("utf8").trim();
    const prefixFile = path.join(dir, "prefix.patch");
    const removedFile = path.join(dir, "removed.patch");
    await fs.writeFile(prefixFile, c.prefixPatch);
    await fs.writeFile(removedFile, c.removedPatch);
    await git(["read-tree", c.base_sha], idx);
    await git(["apply", "--cached", "--whitespace=nowarn", prefixFile], idx);
    if (await text(["write-tree"], idx) !== c.prefix_tree) return integrity();
    const commit = await text(["commit-tree", c.prefix_tree, "-p", c.base_sha, "-m", `code cross-check replay prefix ${c.id}`],
      replayIdentityEnv());
    if (commit !== c.prefix_commit) return integrity();
    for (const w of c.witnesses) {
      if (await text(["rev-parse", `${c.prefix_tree}:${w.path}`]) !== w.blob_id) return integrity();
    }
    // A control removes nothing: its prefix tree is already the squash tree.
    if (c.removedPatch.length > 0) await git(["apply", "--cached", "--whitespace=nowarn", removedFile], idx);
    if (await text(["write-tree"], idx) !== await text(["rev-parse", `${c.squash_sha}^{tree}`])) return integrity();
    const diff = await git(["diff", ...REPLAY_DIFF_ARGS, c.base_sha, c.prefix_commit]);
    if (!diff.equals(c.prefixPatch)) return integrity();
    return { work: dir, bare, cleanup, readFile: (file) => git(["cat-file", "blob", `${c.prefix_tree}:${file}`]) };
  } catch (error) {
    await cleanup();
    if (error instanceof ReplayError || signal?.aborted) throw error;
    return integrity();
  }
}

export interface MaterializedCase { snapshot: string; cleanup(): Promise<void> }

// ---------------------------------------------------------------------------------------------
// uid-split sharing. Under the worker/runner split the Codex fileop helper runs as the command uid
// (runner-cmd, supplementary member of group runner) and the provider as runner, but every directory the
// replay creates is a worker-owned mkdtemp (0700). Only the run directory (search only), the snapshot
// (read only) and the per-case HOME (the Codex HOME contract) are opened to group runner; the bare
// work repository, the records file and every other path stay private.

/** The uid layout the replay creates directories for. `split`/`gid` are injectable so unit tests can
 * exercise the split modes with a group the test process belongs to; production uses the defaults. */
export interface ReplayUidLayout {
  split: boolean; gid: number;
  /** Test-only observer, awaited just before each snapshot entry's owner/mode change. */
  onShare?: (entry: string) => void | Promise<void>;
}
const defaultReplayLayout = (): ReplayUidLayout => ({ split: uidSplitActive(), gid: RUNNER_UID });

/** Set the group and the exact mode through an O_NOFOLLOW directory handle (never a path-following chown). */
async function shareDirectory(dir: string, gid: number, mode: number): Promise<void> {
  const handle = await fs.open(dir, FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW);
  try {
    // chown first: a non-root chown clears the setgid bit, so the chmod must follow it.
    await handle.chown(process.getuid?.() ?? -1, gid);
    await handle.chmod(mode);
  } finally { await handle.close(); }
}

/** The per-run scratch directory. Single-uid it is the plain 0700 mkdtemp. Under the split it is group
 * runner with mode 2710: search only, so the command and provider uids can reach the snapshot inside it
 * but cannot list or read its other entries. */
export async function createReplayRunDir(root: string, layout: ReplayUidLayout = defaultReplayLayout()): Promise<string> {
  const dir = await fs.mkdtemp(path.join(root, "replay-run-"));
  if (!layout.split) return dir;
  try { await shareDirectory(dir, layout.gid, 0o2710); } catch (error) {
    await fs.rm(dir, { recursive: true, force: true }).catch(() => undefined);
    throw error;
  }
  return dir;
}

/** Group-share one snapshot entry. Each entry is opened O_NOFOLLOW and its dev/ino must match the lstat
 * that typed it, so the chmod/chown acts on the checked handle. The directory listing itself is not
 * handle-bound: it is fs.readdir(entry) by path, and each child is re-checked the same way (lstat, then
 * O_NOFOLLOW open, then dev/ino). That is acceptable because the snapshot root stays a worker-private
 * 0700 directory until this walk finishes, so nothing else can swap entries under it meanwhile. Symlinks are skipped (never followed or mutated); any type other than file or directory is
 * refused. Directories become 2750; files keep their owner bits and gain group read (and group execute
 * where the owner has it), never group write, and lose every other bit.
 *
 * The walk is post-order: a directory's children are narrowed first (the worker owner's rwx is enough to
 * read the directory), and only then is the directory itself given to group runner. Under a permissive
 * umask checkout-index leaves children group-writable, so a pre-order walk would expose group-writable
 * descendants beneath an already-opened parent. The root is therefore opened last. */
async function shareSnapshotEntry(entry: string, typed: Stats, layout: ReplayUidLayout): Promise<void> {
  if (typed.isSymbolicLink()) return;
  const isDir = typed.isDirectory();
  if (!isDir && !typed.isFile()) return integrity();
  const flags = isDir
    ? FS.O_RDONLY | FS.O_DIRECTORY | FS.O_NOFOLLOW
    : FS.O_RDONLY | FS.O_NOFOLLOW | FS.O_NONBLOCK;
  const handle = await fs.open(entry, flags);
  try {
    const st = await handle.stat();
    if (st.dev !== typed.dev || st.ino !== typed.ino || st.isDirectory() !== isDir) return integrity();
    if (isDir) {
      for (const name of await fs.readdir(entry)) {
        const child = path.join(entry, name);
        await shareSnapshotEntry(child, await fs.lstat(child), layout);
      }
    }
    const owner = st.mode & 0o700;
    const mode = isDir ? 0o2750 : owner | ((owner & 0o400) >> 3) | ((owner & 0o100) >> 3);
    await layout.onShare?.(entry);
    await handle.chown(st.uid, layout.gid);
    await handle.chmod(mode);
  } finally { await handle.close(); }
}

/** An identity that must be able to search every ancestor of the run directory. */
export interface TraversalIdentity { uid: number; groups: readonly number[] }
export interface AncestorFacts { path: string; uid: number; gid: number; mode: number }
/** The command uid (supplementary group runner) runs the fileop helper; the provider runs as runner. */
const SNAPSHOT_READERS: readonly TraversalIdentity[] = [
  { uid: COMMAND_UID, groups: [COMMAND_UID, RUNNER_UID] },
  { uid: RUNNER_UID, groups: [RUNNER_UID, CODEX_SESSION_GID] },
];

/** The first ancestor that denies search (execute) to one of `identities`, else null. POSIX precedence:
 * the owner class applies alone when the uid matches, else the group class when any group matches,
 * else other. */
export function snapshotTraversalGap(ancestors: readonly AncestorFacts[], identities: readonly TraversalIdentity[] = SNAPSHOT_READERS): string | null {
  for (const a of ancestors) {
    for (const who of identities) {
      const bit = who.uid === a.uid ? 0o100 : who.groups.includes(a.gid) ? 0o010 : 0o001;
      if ((a.mode & bit) === 0) return a.path;
    }
  }
  return null;
}

/** Startup preflight under the split: the run directory must be its own realpath (the helpers require
 * the root to equal its resolved path as their own uid) and every ancestor up to / must be searchable by
 * both reader identities. Nothing the CLI did not create is ever chmod'ed; a gap fails with
 * snapshot_untraversable before any case or provider call. */
export async function assertSnapshotTraversable(runDir: string, identities: readonly TraversalIdentity[] = SNAPSHOT_READERS): Promise<void> {
  const refuse = (): never => { throw new ReplayError("snapshot_untraversable"); };
  try {
    const abs = path.resolve(runDir);
    if (await fs.realpath(abs) !== abs) return refuse();
    const ancestors: AncestorFacts[] = [];
    for (let dir = path.dirname(abs); ; dir = path.dirname(dir)) {
      const st = await fs.lstat(dir);
      if (!st.isDirectory()) return refuse();
      ancestors.push({ path: dir, uid: st.uid, gid: st.gid, mode: st.mode });
      if (dir === path.dirname(dir)) break;
    }
    if (snapshotTraversalGap(ancestors, identities) !== null) refuse();
  } catch (error) {
    if (error instanceof ReplayError) throw error;
    refuse();
  }
}

/** verifyCasePins, then export the pinned prefix tree as a single-commit repository. Files are written
 * with checkout-index (exact blob bytes, no export attributes) and the snapshot commit must reproduce
 * prefix_tree exactly; tracked files that .gitignore matches are staged with -f. */
export async function materializeCase(
  checkout: string, c: ReplayCase, scratchDir: string, signal?: AbortSignal, layout: ReplayUidLayout = defaultReplayLayout(),
): Promise<MaterializedCase> {
  const verified = await verifyCasePins(checkout, c, scratchDir, signal);
  let snapshot: string | undefined;
  const cleanup = async (): Promise<void> => {
    await verified.cleanup();
    if (snapshot) await fs.rm(snapshot, { recursive: true, force: true });
  };
  try {
    const dir = await fs.mkdtemp(path.join(scratchDir, "replay-snap-"));
    snapshot = dir;
    // The snapshot stays a private 0700 mkdtemp through checkout and commit; shareSnapshotEntry opens the
    // root (and everything under it) to group runner only after the prefix_tree check below.
    const idx = { GIT_INDEX_FILE: path.join(verified.work, "snapshot-index") };
    await replayGit(["--git-dir", verified.bare, "read-tree", c.prefix_tree], { env: idx, signal });
    await replayGit(["--git-dir", verified.bare, "--work-tree", dir, "checkout-index", "-a", "-f"], { env: idx, signal });
    await replayGit(["init", "-q"], { cwd: dir, signal });
    await replayGit(["add", "-A", "-f"], { cwd: dir, signal });
    await replayGit(["-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty",
      "-m", `code cross-check replay snapshot ${c.id}`], { cwd: dir, env: replayIdentityEnv(), signal });
    const tree = (await replayGit(["rev-parse", "HEAD^{tree}"], { cwd: dir, signal })).toString("utf8").trim();
    if (tree !== c.prefix_tree) return integrity();
    if (layout.split) await shareSnapshotEntry(dir, await fs.lstat(dir), layout);
    return { snapshot: dir, cleanup };
  } catch (error) {
    await cleanup();
    if (error instanceof ReplayError || signal?.aborted) throw error;
    return integrity();
  }
}

// ---------------------------------------------------------------------------------------------
// Claim construction and the Codex release seam

export interface ReplayRunOptions { model: string; effort: ReplayEffort; timeoutMs: number; repeat: number }

export function buildReplayClaim(
  c: ReplayCase, inputs: ReplayInputs, family: ReplayFamily, opts: ReplayRunOptions,
  auth: { claudeToken?: string; openaiKey?: string }, now: number = Date.now(),
): ClaimResponse {
  const digest = sha256Hex(JSON.stringify({
    squash_sha: c.squash_sha, base_sha: c.base_sha, prefix_tree: c.prefix_tree,
    prefix_commit: c.prefix_commit, artifacts: c.artifacts,
  }));
  return {
    kind: "cross_check",
    run_id: `replay-${c.id}-${family}-${opts.repeat}`,
    issue_iid: c.source.issue,
    issue_title: inputs.issueTitle,
    issue_description: inputs.issueBody,
    claim_generation: 1,
    last_seq: 0,
    agents: [],
    repo: { id: "replay", url: "https://replay.invalid/repo", clone_url: "https://replay.invalid/repo.git" },
    config: { default_model: opts.model, default_effort: opts.effort },
    secrets: family === "claude"
      ? { forge_pat: "", anthropic_oauth_token: auth.claudeToken }
      : { forge_pat: "", codex: { auth_mode: "api_key", access_token: auth.openaiKey ?? "", capability: randomBytes(16).toString("hex") } },
    cross_check: {
      stage: "code",
      lead_run_id: `replay-${c.id}`,
      round: 1,
      candidate_digest: digest,
      deadline_at: new Date(now + opts.timeoutMs + 60_000).toISOString(),
      plan_md: inputs.plan,
      milestones: [],
      required_capabilities: [],
      required_tools: [],
      size_class: "replay",
      base_commit: c.base_sha,
      head_commit: c.prefix_commit,
      code_context: { issue_title: inputs.issueTitle, issue_description: inputs.issueBody },
      guidance_snapshot: "",
      repo_instructions_enabled: false,
      code_diff: inputs.diff,
      model_source: "pin",
      effort_source: "pin",
    },
  } as ClaimResponse;
}

/** Codex replay supports API-key auth only: no subscription release, no refresh. */
export function codexReleaseSeam(openaiKey: string): Pick<WorkerClient, "releaseCodex" | "refreshCodex"> {
  return {
    async releaseCodex(_runId, _req, expected) {
      if (expected.authMode !== "api_key") throw new Error("replay_subscription_unsupported");
      return { auth_mode: "api_key", access_token: openaiKey };
    },
    async refreshCodex() { throw new Error("replay_refresh_unsupported"); },
  };
}

// ---------------------------------------------------------------------------------------------
// The replay loop

export interface ReplayRecord {
  profile?: ReplayProfile;
  case_id: string;
  kind: "defect" | "control";
  input_provenance: "reconstructed" | "recorded";
  family: ReplayFamily;
  repeat: number;
  corpus_version: string;
  checker_version: { uzi_head: string; agent_version: string; corpus_version: string };
  brief_sha256: string;
  model: string;
  effort: ReplayEffort;
  reader_implementation: string;
  credential_mode: string;
  outcome: ReplayOutcome;
  /** Time spent inside checker.run only; null when no checker call was made. */
  latency_ms: number | null;
  cost_usd: number | null;
  findings: CodeFinding[];
}

export interface RunReplaysDeps {
  checkout: string;
  corpus: ReplayCorpus;
  families: ReplayFamily[];
  model: string;
  effort: ReplayEffort;
  repeats: number;
  timeoutMs?: number;
  scratchDir: string;
  env: NodeJS.ProcessEnv;
  checkers?: { claude?: Pick<ClaudeCrossCheck, "run">; codex?: Pick<CodexCrossCheck, "run"> };
  materialize?: typeof materializeCase;
  /** Called once, before the attempt is recorded, when a checker could not confirm its native cleanup: the caller
   *  latches retention of the run directory before any later step can fail. */
  onCleanupUnconfirmed?: () => void;
  /** Test seam for the uid layout of the directories the replay creates; production derives it from the environment. */
  layout?: ReplayUidLayout;
  signal?: AbortSignal;
  now?: () => number;
  writeRecord: (record: ReplayRecord) => Promise<void>;
  checkerVersion: { uzi_head: string; agent_version: string };
}

const MAX_TIMEOUT_MS = 15 * 60_000;

function costOf(family: ReplayFamily, usage: Record<string, unknown>[]): number | null {
  const result = [...usage].reverse().find((p) => p.event === "result");
  if (!result) return null;
  if (family === "claude") {
    const cost = result.total_cost_usd;
    return typeof cost === "number" && Number.isFinite(cost) && cost >= 0 ? cost : null;
  }
  const modelUsage = result.modelUsage;
  if (typeof modelUsage !== "object" || modelUsage === null || Array.isArray(modelUsage)) return null;
  const cost = deriveCodexRunCost(modelUsage as Record<string, CodexModelUsageEntry>);
  return cost.kind === "metered" && Number.isFinite(cost.usd) && cost.usd >= 0 ? cost.usd : null;
}

/** Model identity gate shared by the CLI and the record parser. `env` adds the check that the model
 * does not embed a credential-bearing environment value (values under 8 characters are ignored). */
export function isSafeReplayModel(model: string, env: NodeJS.ProcessEnv = {}): boolean {
  if (!model || Buffer.byteLength(model) > 100 || /[\s\p{Cc}\p{Cf}\uFFFD]/u.test(model) ||
      !/^[\p{L}\p{N}][\p{L}\p{N}._:/-]*$/u.test(model) ||
      /(?:sk-|gh[pousr]_|github_pat_|glpat-|xox[baprs]-)/i.test(model)) return false;
  for (const key of Object.keys(env)) {
    const value = env[key];
    if (/(?:TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)/i.test(key) && value && value.length >= 8 && model.includes(value)) return false;
  }
  return true;
}

const errText = (error: unknown): string => (error instanceof Error ? error.message : "");

function classify(error: unknown, timedOut: boolean, outer: AbortSignal | undefined): ReplayOutcome {
  if (timedOut) return "model_timeout";
  if (outer?.aborted) return "interrupted";
  if (error instanceof CrossCheckCheckerUnavailableError) return "checker_unavailable";
  if (error instanceof CrossCheckMalformedError || /invalid code cross-check|JSON/.test(errText(error))) return "malformed";
  if (error instanceof CrossCheckConfinementError || /confinement|cleanup unconfirmed/.test(errText(error))) return "confinement_failed";
  return "model_error";
}

/** Serial case x family x repeat loop, one attempt each, no retries. Error text is never recorded. */
export async function runReplays(deps: RunReplaysDeps): Promise<ReplayRecord[]> {
  const timeoutMs = deps.timeoutMs ?? MAX_TIMEOUT_MS;
  if (!Number.isInteger(deps.repeats) || deps.repeats < 1 || deps.repeats > 20 ||
      !Number.isInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > MAX_TIMEOUT_MS) throw new ReplayError("invalid_options");
  const now = deps.now ?? Date.now;
  const materialize = deps.materialize ?? materializeCase;
  const layout = deps.layout ?? defaultReplayLayout();
  const defaults: { claude?: ClaudeCrossCheck; codex?: CodexCrossCheck } = {};
  // Built lazily, only for a family that is run and only once its credential is present.
  const checkerFor = (family: ReplayFamily): Pick<ClaudeCrossCheck | CodexCrossCheck, "run"> => {
    const injected = deps.checkers?.[family];
    if (injected) return injected;
    if (family === "claude") return (defaults.claude ??= new ClaudeCrossCheck(replayLog, { secretPaths: SECRET_PATH_PREFIXES }));
    return (defaults.codex ??= new CodexCrossCheck(codexReleaseSeam(deps.env.OPENAI_API_KEY ?? ""), replayLog));
  };
  const records: ReplayRecord[] = [];
  for (const c of deps.corpus.cases) {
    for (const family of deps.families) {
      for (let repeat = 1; repeat <= deps.repeats; repeat++) {
        const token = family === "claude" ? deps.env.CLAUDE_CODE_OAUTH_TOKEN : deps.env.OPENAI_API_KEY;
        let outcome: ReplayOutcome;
        let findings: CodeFinding[] = [];
        let cost: number | null = null;
        let latency: number | null = null;
        // Set when a checker was invoked; an unconfirmed native cleanup stops the replay (below).
        let invoked = false;
        let cleanupConfirmed = false;
        if (!token) outcome = "missing_auth";
        else if (deps.signal?.aborted) outcome = "interrupted";
        else {
          outcome = "completed";
          let materialized: MaterializedCase | undefined;
          let home: string | undefined;
          let timer: NodeJS.Timeout | undefined;
          let timedOut = false;
          const usage: Record<string, unknown>[] = [];
          const pass = new AbortController();
          try {
            try { materialized = await materialize(deps.checkout, c, deps.scratchDir, deps.signal, layout); } catch (error) {
              if (!deps.signal?.aborted) throw error;
              outcome = "interrupted";
            }
            if (materialized) {
              home = await fs.mkdtemp(path.join(deps.scratchDir, "replay-home-"));
              // The Codex HOME contract is worker:runner and group-writable; single-uid stays private.
              if (layout.split) await shareDirectory(home, layout.gid, 0o2770);
              else await fs.chmod(home, 0o700);
              const claim = buildReplayClaim(c, c.inputs, family,
                { model: deps.model, effort: deps.effort, timeoutMs, repeat },
                { claudeToken: deps.env.CLAUDE_CODE_OAUTH_TOKEN, openaiKey: deps.env.OPENAI_API_KEY }, now());
              // The claim must be one the production decoder accepts, or the replay proves nothing.
              decodeCrossCheckClaim(claim.cross_check);
              timer = setTimeout(() => { timedOut = true; pass.abort(); }, timeoutMs);
              const signal = AbortSignal.any(deps.signal ? [deps.signal, pass.signal] : [pass.signal]);
              try {
                let text: string;
                const began = now();
                try {
                  invoked = true;
                  text = await checkerFor(family).run(claim, materialized.snapshot, home, signal,
                    async (payload) => { usage.push({ ...payload }); }, () => { cleanupConfirmed = true; });
                } finally { latency = Math.max(0, now() - began); }
                signal.throwIfAborted();
                if (!cleanupConfirmed) throw new CrossCheckConfinementError("cleanup_callback_missing");
                findings = validateCodeFindings(text);
              } catch (error) {
                outcome = classify(error, timedOut, deps.signal);
              }
              cost = costOf(family, usage);
            }
          } finally {
            if (timer) clearTimeout(timer);
            // A checker that could not confirm its processes stopped may still use the snapshot and HOME:
            // leave both in place (as the production runner keeps the snapshot) and stop the replay below.
            if (!invoked || cleanupConfirmed) {
              await materialized?.cleanup();
              // Under the split the HOME holds runner-owned and command-owned residue the worker cannot
              // unlink: rmHomeTree removes it as those uids and is the plain worker removal otherwise.
              if (home) await rmHomeTree(path.resolve(home));
            }
          }
        }
        const rec: ReplayRecord = {
          case_id: c.id, kind: c.kind, input_provenance: c.input_provenance, family, repeat,
          corpus_version: deps.corpus.corpus_version,
          checker_version: { ...deps.checkerVersion, corpus_version: deps.corpus.corpus_version },
          profile: deps.corpus.profile ?? "baseline", brief_sha256: deps.corpus.brief_sha256[family], model: deps.model, effort: deps.effort,
          reader_implementation: READER_IMPLEMENTATION[family], credential_mode: CREDENTIAL_MODE[family],
          outcome, latency_ms: latency, cost_usd: cost,
          findings: outcome === "completed" ? findings : [],
        };
        if (invoked && !cleanupConfirmed) {
          deps.onCleanupUnconfirmed?.();
          // No further paid call may start while an earlier checker's processes may still be running, and
          // the stop (which keeps the run directory) must not depend on recording the attempt succeeding.
          try { await deps.writeRecord(rec); records.push(rec); } catch { /* the stop below still applies */ }
          throw new ReplayError("cleanup_unconfirmed");
        }
        await deps.writeRecord(rec);
        records.push(rec);
      }
    }
  }
  return records;
}

// ---------------------------------------------------------------------------------------------
// Scoring (pure)

export interface Adjudication { record_key: string; finding_id: string; verdict: "false_positive" | "valid" | "duplicate" }
type ScoreCase = Pick<ReplayCase, "id" | "kind" | "input_provenance" | "expected">;
export interface ScoreCorpus { profile?: ReplayProfile; corpus_version: string; brief_sha256: { codex: string; claude: string }; cases: ScoreCase[] }

export interface DefectScore { case_id: string; id: string; attempted: number; completed: number; matched: number }
export interface GroupAggregate {
  family: ReplayFamily;
  input_provenance: "reconstructed" | "recorded";
  replays: number;
  completed: number;
  outcomes: Record<ReplayOutcome, number>;
  incomplete_rate: number | null;
  defects: DefectScore[];
  matched: number;
  match_rate_completed: number | null;
  match_rate_attempted: number | null;
  execution_only_excluded: number;
  unmatched_findings: number;
  control_findings: number;
  unadjudicated_findings: number;
  adjudicated_findings?: number;
  verified_false_positives?: number;
  cost_usd_sum: number;
  cost_usd_median: number | null;
  cost_unknown: number;
  latency_ms_median: number | null;
  latency_ms_p90: number | null;
  latency_ms_max: number | null;
  model: string[];
  effort: string[];
  reader_implementation: string[];
  credential_mode: string[];
}
export interface Aggregate {
  profile?: ReplayProfile;
  corpus_version: string;
  brief_sha256: { codex: string; claude: string };
  scoring: string;
  groups: GroupAggregate[];
}

const invalidRecords = (): never => { throw new ReplayError("records_invalid"); };

const SCORING_NOTE = "expected-defect matching is a proxy; unadjudicated findings are not confirmed false positives";

export function findingMatches(d: ExpectedDefect, f: CodeFinding): boolean {
  if (!d.paths.includes(f.path)) return false;
  if (d.use_line_range && d.line_range && f.line >= d.line_range[0] && f.line <= d.line_range[1]) return true;
  const text = `${f.title}\n${f.detail}`;
  return d.match.some((source) => new RegExp(source, "i").test(text));
}
const recordKey = (r: ReplayRecord): string => `${r.case_id}/${r.family}/${r.repeat}`;
const distinct = (values: string[]): string[] => [...new Set(values)].sort();
function median(sorted: number[]): number | null {
  if (sorted.length === 0) return null;
  const mid = sorted.length >> 1;
  return sorted.length % 2 ? sorted[mid]! : (sorted[mid - 1]! + sorted[mid]!) / 2;
}
const rate = (n: number, d: number): number | null => (d === 0 ? null : n / d);

export function scoreReplays(corpus: ScoreCorpus, records: ReplayRecord[], adjudications?: Adjudication[]): Aggregate {
  const cases = new Map(corpus.cases.map((c) => [c.id, c]));
  const byKey = new Map<string, ReplayRecord>();
  for (const r of records) {
    const c = cases.get(r.case_id);
    if (!c) throw new ReplayError("score_unknown_case");
    // A record must agree with the loaded corpus; a mismatch is a stale or foreign records file.
    if ((r.profile !== undefined && r.profile !== (corpus.profile ?? "baseline")) || byKey.has(recordKey(r)) || r.corpus_version !== corpus.corpus_version ||
        r.checker_version.corpus_version !== corpus.corpus_version ||
        r.brief_sha256 !== corpus.brief_sha256[r.family] || r.kind !== c.kind ||
        r.input_provenance !== c.input_provenance || (r.outcome !== "completed" && r.findings.length > 0)) invalidRecords();
    byKey.set(recordKey(r), r);
  }
  const verdicts = new Map<string, Adjudication["verdict"]>();
  for (const a of adjudications ?? []) {
    const rec = byKey.get(a.record_key);
    if (!rec || !rec.findings.some((f) => f.id === a.finding_id)) throw new ReplayError("adjudication_unknown_finding");
    const key = `${a.record_key}\0${a.finding_id}`;
    if (verdicts.has(key)) throw new ReplayError("adjudication_duplicate");
    verdicts.set(key, a.verdict);
  }
  const groups = new Map<string, ReplayRecord[]>();
  for (const r of records) {
    const key = `${r.family}\0${cases.get(r.case_id)!.input_provenance}`;
    const list = groups.get(key);
    if (list) list.push(r); else groups.set(key, [r]);
  }
  const out: GroupAggregate[] = [];
  for (const key of [...groups.keys()].sort()) {
    const list = groups.get(key)!;
    const outcomes = Object.fromEntries(REPLAY_OUTCOMES.map((o) => [o, 0])) as Record<ReplayOutcome, number>;
    const defects = new Map<string, DefectScore>();
    let completed = 0, matched = 0, denomAttempted = 0, denomCompleted = 0, excluded = 0;
    let unmatched = 0, control = 0, adjudicated = 0, falsePositives = 0;
    for (const r of list) {
      outcomes[r.outcome]++;
      const done = r.outcome === "completed";
      if (done) completed++;
      const c = cases.get(r.case_id)!;
      const scored = c.kind === "defect" ? c.expected.filter((d) => d.detectability === "read_only") : [];
      denomAttempted += scored.length;
      if (done) denomCompleted += scored.length;
      excluded += c.kind === "defect" ? c.expected.length - scored.length : 0;
      for (const d of scored) {
        const s = defects.get(`${c.id}\0${d.id}`) ?? { case_id: c.id, id: d.id, attempted: 0, completed: 0, matched: 0 };
        s.attempted++;
        if (done) {
          s.completed++;
          if (r.findings.some((f) => findingMatches(d, f))) { s.matched++; matched++; }
        }
        defects.set(`${c.id}\0${d.id}`, s);
      }
      for (const f of r.findings) {
        if (c.kind === "defect" && c.expected.some((d) => findingMatches(d, f))) continue;
        if (c.kind === "control") control++; else unmatched++;
        const verdict = verdicts.get(`${recordKey(r)}\0${f.id}`);
        if (verdict) adjudicated++;
        if (verdict === "false_positive") falsePositives++;
      }
    }
    const reached = list.filter((r) => r.outcome !== "missing_auth");
    const costs = reached.map((r) => r.cost_usd).filter((v): v is number => v !== null).sort((a, b) => a - b);
    const latencies = reached.map((r) => r.latency_ms).filter((v): v is number => v !== null).sort((a, b) => a - b);
    const group: GroupAggregate = {
      family: list[0]!.family, input_provenance: cases.get(list[0]!.case_id)!.input_provenance,
      replays: list.length, completed, outcomes,
      incomplete_rate: rate(list.length - completed, list.length),
      defects: [...defects.values()],
      matched,
      match_rate_completed: rate(matched, denomCompleted),
      match_rate_attempted: rate(matched, denomAttempted),
      execution_only_excluded: excluded,
      unmatched_findings: unmatched, control_findings: control,
      unadjudicated_findings: unmatched + control - adjudicated,
      cost_usd_sum: costs.reduce((a, b) => a + b, 0), cost_usd_median: median(costs),
      cost_unknown: reached.length - costs.length,
      latency_ms_median: median(latencies),
      latency_ms_p90: latencies.length ? latencies[Math.ceil(latencies.length * 0.9) - 1]! : null,
      latency_ms_max: latencies.length ? latencies[latencies.length - 1]! : null,
      model: distinct(list.map((r) => r.model)), effort: distinct(list.map((r) => r.effort)),
      reader_implementation: distinct(list.map((r) => r.reader_implementation)),
      credential_mode: distinct(list.map((r) => r.credential_mode)),
    };
    if (adjudications) {
      group.adjudicated_findings = adjudicated;
      group.verified_false_positives = falsePositives;
    }
    out.push(group);
  }
  return { corpus_version: corpus.corpus_version, profile: corpus.profile ?? "baseline", brief_sha256: corpus.brief_sha256, scoring: SCORING_NOTE, groups: out };
}

/** Built field by field: ids, counts, rates and pins only. No finding text, issue text or credential. */
export function sanitizedAggregate(agg: Aggregate): string {
  return JSON.stringify({
    corpus_version: agg.corpus_version,
    profile: agg.profile ?? "baseline", brief_sha256: { codex: agg.brief_sha256.codex, claude: agg.brief_sha256.claude },
    scoring: agg.scoring,
    groups: agg.groups.map((g) => ({
      family: g.family, input_provenance: g.input_provenance, replays: g.replays, completed: g.completed,
      outcomes: { ...g.outcomes }, incomplete_rate: g.incomplete_rate,
      defects: g.defects.map((d) => ({ case_id: d.case_id, id: d.id, attempted: d.attempted, completed: d.completed, matched: d.matched })),
      matched: g.matched, match_rate_completed: g.match_rate_completed, match_rate_attempted: g.match_rate_attempted,
      execution_only_excluded: g.execution_only_excluded, unmatched_findings: g.unmatched_findings,
      control_findings: g.control_findings, unadjudicated_findings: g.unadjudicated_findings,
      ...(g.adjudicated_findings !== undefined ? { adjudicated_findings: g.adjudicated_findings } : {}),
      ...(g.verified_false_positives !== undefined ? { verified_false_positives: g.verified_false_positives } : {}),
      cost_usd_sum: g.cost_usd_sum, cost_usd_median: g.cost_usd_median, cost_unknown: g.cost_unknown,
      latency_ms_median: g.latency_ms_median, latency_ms_p90: g.latency_ms_p90, latency_ms_max: g.latency_ms_max,
      model: [...g.model], effort: [...g.effort], reader_implementation: [...g.reader_implementation],
      credential_mode: [...g.credential_mode],
    })),
  });
}

// ---------------------------------------------------------------------------------------------
// Operator-supplied inputs to `score`

function parseRecord(v: unknown, env: NodeJS.ProcessEnv): ReplayRecord {
  try {
    const r = strict(v, [
      ...(typeof v === "object" && v !== null && "profile" in v ? ["profile"] : []),
      "case_id", "kind", "input_provenance", "family", "repeat", "corpus_version", "checker_version", "brief_sha256",
      "model", "effort", "reader_implementation", "credential_mode", "outcome", "latency_ms", "cost_usd", "findings",
    ]);
    const cv = strict(r.checker_version, ["uzi_head", "agent_version", "corpus_version"]);
    const cost = r.cost_usd === null ? null : r.cost_usd;
    if (cost !== null && (typeof cost !== "number" || !Number.isFinite(cost) || cost < 0)) return invalidRecords();
    const latency = r.latency_ms;
    if (latency !== null && (typeof latency !== "number" || !Number.isFinite(latency) || latency < 0)) return invalidRecords();
    const caseId = str(r.case_id, 48);
    if (!CASE_ID.test(caseId)) return invalidRecords();
    const findings = decodeCodeFindings(r.findings);
    const family = oneOf(r.family, ["claude", "codex"] as const);
    const model = str(r.model, 100);
    if (!isSafeReplayModel(model, env)) return invalidRecords();
    return {
      case_id: caseId, kind: oneOf(r.kind, ["defect", "control"] as const),
      input_provenance: oneOf(r.input_provenance, ["reconstructed", "recorded"] as const),
      family, repeat: int(r.repeat, 1),
      corpus_version: str(r.corpus_version, 64),
      checker_version: { uzi_head: str(cv.uzi_head, 128), agent_version: str(cv.agent_version, 128), corpus_version: str(cv.corpus_version, 64) },
      ...(r.profile === undefined ? {} : { profile: oneOf(r.profile, ["baseline", "revised"] as const) }),
      brief_sha256: hex(r.brief_sha256, HEX64), model,
      effort: oneOf(r.effort, REPLAY_EFFORTS),
      reader_implementation: oneOf(r.reader_implementation, [READER_IMPLEMENTATION[family]]),
      credential_mode: oneOf(r.credential_mode, [CREDENTIAL_MODE[family]]),
      outcome: oneOf(r.outcome, REPLAY_OUTCOMES), latency_ms: latency, cost_usd: cost, findings,
    };
  } catch { return invalidRecords(); }
}

export function parseReplayRecords(text: string, env: NodeJS.ProcessEnv = {}): ReplayRecord[] {
  const out: ReplayRecord[] = [];
  const seen = new Set<string>();
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let value: unknown;
    try { value = JSON.parse(line); } catch { return invalidRecords(); }
    const rec = parseRecord(value, env);
    if (seen.has(recordKey(rec))) return invalidRecords();
    seen.add(recordKey(rec));
    out.push(rec);
  }
  if (out.length === 0) return invalidRecords();
  return out;
}

export function parseAdjudications(text: string): Adjudication[] {
  try {
    const value: unknown = JSON.parse(text);
    return arr(value, 10_000).map((raw) => {
      const a = strict(raw, ["record_key", "finding_id", "verdict"]);
      return {
        record_key: str(a.record_key, 128), finding_id: str(a.finding_id, 64),
        verdict: oneOf(a.verdict, ["false_positive", "valid", "duplicate"] as const),
      };
    });
  } catch { throw new ReplayError("adjudication_invalid"); }
}
