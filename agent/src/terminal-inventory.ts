/**
 * Read-side inventory, classification and omission check for #4007 slice B (worker terminal preservation).
 *
 * Production-dark: nothing here has a production caller yet. Later milestones call it only behind
 * `terminalPreservationEnabled()` (see terminal-preservation.ts). Everything in this file is a pure function or a
 * reader; no path writes, retires, repairs or deletes anything.
 */
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { attemptGeneration } from "./attempt-path.js";
import type { GitCache } from "./git.js";
import { canonicalJson, type RecoveryCoordinator } from "./recovery.js";
import { RUN_KINDS, type TerminalPreservationRequest } from "./protocol.js";

// ---- output classification ------------------------------------------------------------------------------

export type TerminalOutputLabel =
  | "branch_source" | "judge_trace" | "report_task" | "chat_job_workspace" | "code_checker_snapshot"
  | "repo_less" | "other_code_kind" | "no_source" | "unknown";

export interface TerminalOutputFacts {
  /** The run kind string. A string outside {@link RUN_KINDS} classifies as `unknown`. */
  kind: string;
  /** The run has no repository. */
  repoLess?: boolean;
  /**
   * The run is bound to an egress profile. The agent's claim carries no egress marker today, so the caller supplies
   * this from whatever authority later milestones define; absent means not egress-bound.
   */
  egressBound?: boolean;
  /** The run adopted a source (a clone it produced work in). Absent means none. */
  adoptedSource?: boolean;
  /** An explicit report-only task (a `task` run that produces a report and no branch). */
  reportOnly?: boolean;
}

const BRANCH_SOURCE_KINDS: ReadonlySet<string> = new Set(["issue", "mr_rework", "self_improve"]);
const OTHER_CODE_KINDS: ReadonlySet<string> = new Set(["ci_fix", "prompt", "task"]);

/** Pure. Every registry run kind maps to a label other than `unknown`. */
export function classifyTerminalOutput(facts: TerminalOutputFacts): TerminalOutputLabel {
  const kind = facts.kind;
  if (!(RUN_KINDS as readonly string[]).includes(kind)) return "unknown";
  if (kind === "task" && facts.reportOnly === true) return "report_task";
  if (kind === "judge") return "judge_trace";
  if (kind === "chat" || kind === "job") return "chat_job_workspace";
  if (kind === "cross_check") return "code_checker_snapshot";
  if (facts.repoLess === true) return "repo_less";
  if (facts.adoptedSource !== true) return "no_source";
  if (BRANCH_SOURCE_KINDS.has(kind) && facts.egressBound !== true) return "branch_source";
  return OTHER_CODE_KINDS.has(kind) || BRANCH_SOURCE_KINDS.has(kind) ? "other_code_kind" : "unknown";
}

// ---- source universe ------------------------------------------------------------------------------------

export type TerminalUnprovedReason =
  | "unknown_read" | "thrown" | "foreign_owner" | "predecessor" | "join_contradiction" | "duplicate"
  | "record_outside_universe" | "foreign_bare" | "cap" | "deadline" | "local_ref" | "non_current_clone_present"
  | "current_missing" | "identity_mismatch" | "ledger_attempt_clone_missing";

export interface TerminalSource {
  sourceId: string;
  attemptId: string;
  clonePath: string;
  head: string;
  /** `dev:ino` of the clone directory. */
  identity: string;
  bare: string;
  branch: string;
  /** What the attempt id embeds (the ledger itself carries no generation); null for a `gx` id. */
  generation: number | null;
  /** Generations of the bound recovery records, sorted. */
  recordGenerations: number[];
}

export interface TerminalRootRef { commit: string; origin: "current_head" | "owed" }

export type TerminalUniverse =
  | { kind: "proved"; sources: TerminalSource[]; roots: TerminalRootRef[]; universeDigest: string }
  | { kind: "unproved"; reason: TerminalUnprovedReason; subreason?: string; detail?: string };

const TERMINAL_UNIVERSE_MAX_BARES = 64;
const TERMINAL_UNIVERSE_MAX_ATTEMPTS = 1024;
const TERMINAL_UNIVERSE_MAX_RECORDS = 256;

type UniverseGit = Pick<GitCache, "listRunBares" | "readInventoryCloneHeads" | "readTerminalAttribution" | "readTerminalCloneLocalRefs">;
type UniverseRecovery = Pick<RecoveryCoordinator, "readTerminalRecords">;

export interface TerminalUniverseInput {
  git: UniverseGit;
  recovery: UniverseRecovery;
  runId: string;
  current: { attemptId: string; clonePath: string; identity: string; bare?: string };
  signal: AbortSignal;
  deadline: number;
}

class Unproved extends Error {
  constructor(readonly reason: TerminalUnprovedReason, readonly detail?: string, readonly subreason?: string) { super(reason); }
}

const sha256 = (text: string): string => createHash("sha256").update(text).digest("hex");

async function lstatOrAbsent(target: string): Promise<{ present: false } | { present: true; dir: boolean; identity: string }> {
  try {
    const st = await fs.lstat(target);
    return { present: true, dir: st.isDirectory() && !st.isSymbolicLink(), identity: `${st.dev}:${st.ino}` };
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return { present: false };
    throw new Unproved("unknown_read", `lstat ${(err as NodeJS.ErrnoException).code ?? "error"}`);
  }
}

/**
 * Prove the run's complete local source universe, or say why it cannot be proved. Read-only. The only accepted shape
 * today is exactly the current flight's own attempt clone: any other present clone, foreign owner, predecessor,
 * unbound record or unreadable evidence is `unproved`.
 */
export async function readTerminalSourceUniverse(input: TerminalUniverseInput): Promise<TerminalUniverse> {
  const { git, recovery, runId, current, signal, deadline } = input;
  const budget = { signal, deadline };
  const expired = (): boolean => signal.aborted || Date.now() >= deadline;
  try {
    if (expired()) throw new Unproved("deadline");
    const own = await lstatOrAbsent(current.clonePath);
    if (!own.present) throw new Unproved("current_missing", "current clone is absent");
    if (!own.dir || own.identity !== current.identity) throw new Unproved("identity_mismatch", "current clone identity differs");

    const bares = await git.listRunBares();
    if (bares.length > TERMINAL_UNIVERSE_MAX_BARES) throw new Unproved("cap", "bares");
    const sources: TerminalSource[] = [];
    const attemptsAll: Array<{ attemptId: string; clonePath: string; branch: string; bare: string; state: string }> = [];
    const owedAll = new Set<string>();

    // The inventory is read WITHOUT a report-proof budget on purpose: under one it refuses every ledger attempt whose
    // clone is already gone, which would make any run with a retired, deleted attempt unprovable. The own clone's
    // presence is proved separately (lstat above plus the current-source join below) and the budget is enforced here.
    for (const bare of bares) {
      if (expired()) throw new Unproved("deadline");
      const inv = await git.readInventoryCloneHeads(bare, runId, { cloneIdentity: true });
      if (inv.kind !== "verified") throw new Unproved("unknown_read", `inventory ${inv.cause ?? "unknown"}`);
      if (inv.foreignOwners.length > 0) throw new Unproved("foreign_owner", inv.foreignOwners.join(","));
      if (inv.retainedPredecessors?.length) throw new Unproved("predecessor", inv.retainedPredecessors[0]!.clonePath);
      const attr = await git.readTerminalAttribution(bare, runId, budget);
      if (attr.kind !== "verified") throw new Unproved(attr.cause === "deadline" ? "deadline" : "unknown_read", `attribution ${attr.cause}`);
      if (attr.attempts.length > TERMINAL_UNIVERSE_MAX_ATTEMPTS) throw new Unproved("cap", "attempts");

      const ledgerByPath = new Map<string, typeof attr.attempts>();
      for (const attempt of attr.attempts) {
        ledgerByPath.set(attempt.clonePath, [...(ledgerByPath.get(attempt.clonePath) ?? []), attempt]);
        attemptsAll.push({ attemptId: attempt.attemptId, clonePath: attempt.clonePath, branch: attempt.branch, bare, state: attempt.state });
      }
      const presentPaths = new Set<string>();
      for (const clone of inv.clones) {
        if (clone.head === undefined || clone.identity === undefined) throw new Unproved("unknown_read", "clone without head or identity");
        const matches = ledgerByPath.get(clone.clonePath) ?? [];
        if (matches.length > 1) throw new Unproved("duplicate", `ledger paths ${clone.clonePath}`);
        const match = matches[0];
        if (!match || match.branch !== clone.branch) throw new Unproved("join_contradiction", `clone without ledger attempt ${path.basename(clone.clonePath)}`);
        presentPaths.add(clone.clonePath);
        sources.push({
          sourceId: sha256(`${bare}\0${match.attemptId}\0${clone.clonePath}`).slice(0, 32), attemptId: match.attemptId,
          clonePath: clone.clonePath, head: clone.head, identity: clone.identity, bare, branch: clone.branch,
          generation: attemptGeneration(match.attemptId) ?? null, recordGenerations: [],
        });
      }
      // Every ledger attempt with a present clone must have been listed by the inventory.
      for (const attempt of attr.attempts) {
        const seen = await lstatOrAbsent(attempt.clonePath);
        if (seen.present && !presentPaths.has(attempt.clonePath)) throw new Unproved("join_contradiction", `ledger attempt present but unlisted ${attempt.attemptId}`);
        // A live or reclaimed attempt whose clone vanished may have held work: refuse. A retired or abandoned attempt
        // is allowed to be gone (it contributes only owed roots and its bare).
        if (!seen.present && attempt.state !== "retired" && attempt.state !== "abandoned") {
          throw new Unproved("ledger_attempt_clone_missing", `${attempt.attemptId} (${attempt.state})`);
        }
      }
      // A journal row that names an attempt id must name the ledger attempt at that same path.
      for (const row of attr.journal) {
        const at = ledgerByPath.get(row.clonePath)?.[0];
        if (row.attemptId !== undefined && at !== undefined && at.attemptId !== row.attemptId) {
          throw new Unproved("join_contradiction", `journal attempt id differs from the ledger at ${path.basename(row.clonePath)}`);
        }
      }
      // Every other attribution of the run must name a ledger attempt path.
      const named = [...attr.journal.map(j => j.clonePath), ...attr.stoppedMarkers.map(m => m.clonePath), ...attr.protected.flatMap(p => p.clonePaths)];
      for (const clonePath of named) {
        if (!ledgerByPath.has(clonePath)) throw new Unproved("join_contradiction", `attribution outside the ledger ${path.basename(clonePath)}`);
      }
      for (const sha of attr.owedRoots) owedAll.add(sha);
    }
    if (expired()) throw new Unproved("deadline");

    const attemptIds = new Set<string>();
    for (const a of attemptsAll) {
      if (attemptIds.has(a.attemptId)) throw new Unproved("duplicate", `attempt ${a.attemptId} in two ledgers`);
      attemptIds.add(a.attemptId);
    }
    const byPath = new Map<string, string>();
    for (const s of sources) {
      const prior = byPath.get(s.clonePath);
      if (prior !== undefined && prior !== s.identity) throw new Unproved("duplicate", "one path, two identities");
      byPath.set(s.clonePath, s.identity);
    }
    const currentBare = current.bare ?? sources.find(s => s.clonePath === current.clonePath)?.bare;
    if (currentBare === undefined || (current.bare !== undefined && !bares.includes(current.bare))) throw new Unproved("current_missing", "current bare unknown");
    const foreign = sources.find(s => s.bare !== currentBare);
    if (foreign) throw new Unproved("foreign_bare", path.basename(foreign.bare));
    const currentSource = sources.find(s => s.clonePath === current.clonePath);
    if (!currentSource) throw new Unproved("current_missing", "current clone is not attributed");
    if (currentSource.attemptId !== current.attemptId) throw new Unproved("join_contradiction", "current clone is another attempt");
    if (currentSource.identity !== current.identity) throw new Unproved("identity_mismatch", "inventory identity differs");
    if (sources.length > 1) throw new Unproved("non_current_clone_present", `${sources.length - 1} other clone(s)`);

    const read = await recovery.readTerminalRecords(runId, budget);
    if (read.kind !== "records") throw new Unproved(expired() ? "deadline" : "unknown_read", "recovery records");
    if (read.records.length > TERMINAL_UNIVERSE_MAX_RECORDS) throw new Unproved("cap", "records");
    const generations = new Set<number>();
    const explicitRoots = new Set<string>([currentSource.head, ...owedAll]);
    for (const record of read.records) {
      // Binding rule: a record binds to the proved source (the current present attempt) when it is on the same branch
      // AND names the current bare's directory. A record without a bareDir binds only when exactly one bare exists
      // (so it cannot belong to another bare) and the branch matches. A record that matches only an absent or retired
      // attempt is accepted solely when its sourceSha is an explicit root of the universe (the head or an owed root).
      const sameBranch = record.branch === currentSource.branch;
      const bareMatches = record.bareDir !== undefined ? record.bareDir === path.basename(currentSource.bare) : bares.length === 1;
      const absentOk = attemptsAll.some(a => a.branch === record.branch) && explicitRoots.has(record.sourceSha);
      if (!(sameBranch && bareMatches) && !absentOk) throw new Unproved("record_outside_universe", `record ${record.captureId}`);
      if (record.bundlePath !== undefined && path.basename(record.bundlePath) !== `${record.captureId}.bundle`) {
        throw new Unproved("record_outside_universe", `bundle of ${record.captureId}`);
      }
      if (record.branch === currentSource.branch && record.generation !== undefined) generations.add(record.generation);
    }
    currentSource.recordGenerations = [...generations].sort((a, b) => a - b);

    const refs = await git.readTerminalCloneLocalRefs(currentSource.clonePath, currentSource.head);
    if (!refs.ok) throw new Unproved("local_ref", currentSource.clonePath, refs.reason);
    if (expired()) throw new Unproved("deadline");

    const roots: TerminalRootRef[] = [{ commit: currentSource.head, origin: "current_head" }];
    for (const commit of [...owedAll].sort()) if (commit !== currentSource.head) roots.push({ commit, origin: "owed" });
    sources.sort((a, b) => (a.sourceId < b.sourceId ? -1 : 1));
    roots.sort((a, b) => (a.commit === b.commit ? (a.origin < b.origin ? -1 : 1) : a.commit < b.commit ? -1 : 1));
    return { kind: "proved", sources, roots, universeDigest: sha256(canonicalJson({ sources, roots })) };
  } catch (err) {
    if (err instanceof Unproved) {
      return { kind: "unproved", reason: err.reason, ...(err.subreason === undefined ? {} : { subreason: err.subreason }),
        ...(err.detail === undefined ? {} : { detail: err.detail }) };
    }
    return { kind: "unproved", reason: expired() ? "deadline" : "thrown", detail: err instanceof Error ? err.message.slice(0, 200) : "non-error" };
  }
}

// ---- manifest coverage ----------------------------------------------------------------------------------

type Manifest = Pick<TerminalPreservationRequest["manifest"], "roots" | "abandoned">;

export type TerminalCoverageResult =
  | { ok: true }
  | { ok: false; reason:
      | "missing_source" | "extra_source" | "missing_root" | "extra_root" | "duplicate_root" | "commit_mismatch"
      | "tree_mismatch" | "duplicate_abandoned" | "unknown_abandoned_class" | "cold_dirty_current_flight"; detail: string };

/**
 * Pure omission check of a manifest against a proved universe. Every universe source and root must be present, nothing
 * else may be; each source's manifest root commit must equal its head; trees are compared against `expectedTrees`
 * (commit to tree) when given. `abandoned` classes are `ignored` or `cold_dirty`, and `cold_dirty` is refused for a
 * current flight (its dirty work is captured, never abandoned).
 */
export function checkManifestCoverage(
  universe: Extract<TerminalUniverse, { kind: "proved" }>,
  manifest: Manifest,
  opts: { currentFlight: boolean; expectedTrees?: ReadonlyMap<string, string> },
): TerminalCoverageResult {
  const fail = (reason: Extract<TerminalCoverageResult, { ok: false }>["reason"], detail: string): TerminalCoverageResult => ({ ok: false, reason, detail });
  const bySource = new Map(universe.sources.map(s => [s.sourceId, s]));
  const abandonedKeys = new Set<string>();
  for (const item of manifest.abandoned) {
    const key = `${item.class}\0${item.source_id}`;
    if (abandonedKeys.has(key)) return fail("duplicate_abandoned", item.source_id);
    abandonedKeys.add(key);
  }
  const seen = new Set<string>();
  const manifestCommits = new Set<string>();
  const sourcesWithRoot = new Set<string>();
  for (const root of manifest.roots) {
    const key = `${root.source_id}\0${root.commit}`;
    if (seen.has(key)) return fail("duplicate_root", root.commit);
    seen.add(key);
    const source = bySource.get(root.source_id);
    if (!source || source.attemptId !== root.attempt_id) return fail("extra_source", root.source_id);
    manifestCommits.add(root.commit);
    sourcesWithRoot.add(root.source_id);
    if (opts.expectedTrees && opts.expectedTrees.get(root.commit) !== root.tree) return fail("tree_mismatch", root.commit);
  }
  for (const source of universe.sources) {
    if (!sourcesWithRoot.has(source.sourceId)) return fail("missing_source", source.sourceId);
    if (!manifest.roots.some(r => r.source_id === source.sourceId && r.commit === source.head)) return fail("commit_mismatch", source.sourceId);
  }
  const universeCommits = new Set(universe.roots.map(r => r.commit));
  for (const commit of universeCommits) if (!manifestCommits.has(commit)) return fail("missing_root", commit);
  for (const commit of manifestCommits) if (!universeCommits.has(commit)) return fail("extra_root", commit);
  for (const item of manifest.abandoned) {
    if (item.class !== "ignored" && item.class !== "cold_dirty") return fail("unknown_abandoned_class", String(item.class));
    if (!bySource.has(item.source_id)) return fail("extra_source", item.source_id);
    if (item.class === "cold_dirty" && opts.currentFlight) return fail("cold_dirty_current_flight", item.source_id);
  }
  return { ok: true };
}
