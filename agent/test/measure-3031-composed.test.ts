// Composed measurement harness for the #3031 terminal producer slot (issue #3442, child C2).
//
// One complete pipeline per measurement, in ONE charged slot with production caps: W0-H
// `captureMaintenanceWip` -> `importMaintenanceTip` (walk, selection, pack, proof, import) ->
// the archive phase (`GitCache.produceRecoveryBundle`, self-contained first, guarded thin fallback
// only on `RecoveryBundleTooLargeError`). The pipeline is driven through the real RunRunner and the
// real RecoveryCoordinator admission (`admitTerminalOrigin`), exactly as the `origin-transfer-*`
// modes of runner-maintenance-terminal-source.test.ts drive it, so the writer-exclusion proof
// (src/maintenance-exclusion.ts) and the quiescence scans (src/run-quiescence.ts) are the real
// ones and their charges land in the slot. Nothing here changes production code.
//
// The measurement tests run only with UZI_MEASURE_3031=1 on Linux. The verdict controls
// (acceptance 9 and 10 of #3442) run without it, on a small fixture, and are the only tests
// allowed to stub exclusion and quiescence: a control run is marked non-eligible for the gate.
//
// Closed list of substitutions in a measurement run (printed in every MEASURE line):
//   - guard-method: RecoveryCoordinator.terminalOriginAdmissionEnabled returns true;
//   - faked-server: the worker API is a faked server behind the real strict WorkerClient.
// Harness interventions that are not Design-7 substitutions are printed too (see `substitutions` in every
// MEASURE line). No Worker object exists in the harness, so `isIdle` is the harness check of sole execution
// in the container and a settled runner (a disclosed fixture precondition; it does not prove production
// Worker-idleness integration).
//
// Environment inputs: UZI_MEASURE_DOCKER_HOST (the Docker daemon the worker is wired to; the
// exclusion proof and the quiescence scans probe it for real), UZI_MEASURE_DEVIATION (free text,
// set when the Git/Node/arch/timing environment is not the worker image's; a would-be PASS is then
// reported as counters_within_limits_elapsed_unverified), TMPDIR (a case-sensitive container-
// internal directory whose ancestors are not group/world-writable or sticky, as /data is).
import { describe, it, afterEach, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import fsSync from "node:fs";
import path from "node:path";
import os from "node:os";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { WorkerClient } from "../src/client.js";
import { MaintenanceResources, RecoveryMaintenance, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { MaintenanceWriterExclusion } from "../src/maintenance-exclusion.js";
import { DindPruneGate } from "../src/dind-prune.js";
import { RECOVERY_MAX_BUNDLE_BYTES, RecoveryBundleTooLargeError } from "../src/git.js";
import {
  maintenanceBootstrapRoots, quiesceRunAttempt, recordMaintenanceBootstrap, setQuiescenceViewForTests,
  snapshotMaintenanceBootstrap, terminalMaintenanceProof, type QuiesceRunRequest,
} from "../src/run-quiescence.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { disableAutoMaintenance } from "./fixture-repo.js";
import { passingExclusion } from "./maintenance-exclusion-fixture.js";
import { client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const MEASURE_ON = process.env.UZI_MEASURE_3031 === "1" && process.platform === "linux";
const MEASURE_SKIP = !MEASURE_ON && "set UZI_MEASURE_3031=1 on Linux to run the composed measurement";
const LINUX_ONLY = process.platform !== "linux" && "needs the Linux-only /proc quiescence and capture machinery";
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const SLOT_ELAPSED_LIMIT_MS = 120_000;
// The largest loose-object count observed in a read-only sample of hosted worker attempt clones
// (17 worker pods listed, 5 with clones; the next largest were 145, 113, 50, 47). It is the
// largest observed in that sample, not a fleet maximum.
const LOOSE_SAMPLE_LARGEST = 196;
const LOOSE_SAMPLE_NOTE = "largest observed in a read-only sample of hosted worker attempt clones, not a fleet maximum";
const LOOSE_LADDER = [392, 784, 1568, 3136, 6272];

const gitEnv: NodeJS.ProcessEnv = {
  ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
  GIT_AUTHOR_NAME: "measure", GIT_AUTHOR_EMAIL: "measure@example.invalid",
  GIT_COMMITTER_NAME: "measure", GIT_COMMITTER_EMAIL: "measure@example.invalid",
};
const sh = (cwd: string, ...args: string[]): string =>
  execFileSync("git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 1024 ** 3 });

type Layout = "shared" | "dissociated";
type Shape = "A" | "B" | "C" | "control";
// Phases in pipeline order. P0/P1 are split at the first runner-owned pin helper launch; P2 and
// P3 stay one phase because Git's status step is joined only after both finish (git.ts).
// transfer_entry is importMaintenanceTip before its first observed phase; after_<phase> is the importer's
// own work between two observed phases (family and descriptor revalidation, scratch handling).
const PHASES = ["preamble", "w0_p0", "w0_p1", "w0_p2_p3", "w0_p4a", "w0_p4b", "w0_p5", "transfer_entry",
  "walk", "after_walk", "selection", "after_selection", "pack", "after_pack", "proof", "after_proof",
  "import", "after_import", "archive", "post_archive"] as const;
type Phase = typeof PHASES[number];
type Dimension = MaintenanceResource;
type Counters = Partial<Record<Dimension, number>>;
type Limits = Record<string, number>;

interface Violation { dimension: string; phase: string; used?: number; limit?: number; refused?: boolean; unlimited?: boolean }
interface EvaluationInput {
  limits: Limits;
  elapsedLimitMs: number;
  elapsedMs: number;
  phases: ReadonlyArray<{ name: string; used: Counters }>;
  refusedCharge?: { resource: string; amount: number; used: number; phase: string };
  /** A charge that failed for any reason other than quota exhaustion (revoked or expired token, cancellation, clock). */
  interruption?: { reason: string; resource: string; phase: string };
  /** Cap refusals that arrive as a typed refusal, not as a refused charge (the decoded verification budget). */
  capRefusals?: Violation[];
  envelopeOverruns?: Violation[];
  refusal?: { phase: string; reason: string };
  incomplete?: string;
  unrun?: string;
  deviation?: string;
}
interface Evaluation {
  verdict: "PASS" | "FAIL" | "UNRUN" | "counters_within_limits_elapsed_unverified";
  elapsed_fidelity: string;
  violations: Violation[];
  refusal?: { phase: string; reason: string; phases_left_unrun: string[] };
  interruption?: { reason: string; resource: string; phase: string; phases_left_unrun: string[] };
  reason?: string;
}

/**
 * The verdict of #3442 item 12. Completion is kept separate from counter compliance: a refusal
 * (layout, quiescence, admission, resource exhaustion) is recorded with its phase and the phases
 * left unrun and is never a counter PASS. Each dimension's limit is compared with the cumulative
 * use at the end of every phase, in pipeline order, so the first phase that crosses it is named.
 */
function evaluate(input: EvaluationInput): Evaluation {
  const fidelity = input.deviation ? `unverified: ${input.deviation}` : "native";
  if (input.unrun !== undefined)
    return { verdict: "UNRUN", elapsed_fidelity: fidelity, violations: [], reason: input.unrun };
  const violations: Violation[] = [];
  const cumulative = new Map<string, number>();
  const named = new Set<string>();
  for (const phase of input.phases) {
    for (const [dimension, used] of Object.entries(phase.used)) {
      const total = (cumulative.get(dimension) ?? 0) + used;
      cumulative.set(dimension, total);
      const limit = input.limits[dimension];
      // A charged dimension with no admitted limit is never skipped: it fails, naming itself.
      if (limit === undefined && used > 0 && !named.has(dimension)) {
        named.add(dimension);
        violations.push({ dimension, phase: phase.name, used: total, unlimited: true });
      } else if (limit !== undefined && total > limit && !named.has(dimension)) {
        named.add(dimension);
        violations.push({ dimension, phase: phase.name, used: total, limit });
      }
    }
  }
  const refused = input.refusedCharge;
  if (refused) {
    const limit = input.limits[refused.resource] ?? Number.NaN;
    if (!named.has(refused.resource))
      violations.push({ dimension: refused.resource, phase: refused.phase, used: refused.used + refused.amount, limit, refused: true });
  }
  violations.push(...(input.capRefusals ?? []), ...(input.envelopeOverruns ?? []));
  if (input.elapsedMs > input.elapsedLimitMs)
    violations.push({ dimension: "elapsedMs", phase: "slot", used: input.elapsedMs, limit: input.elapsedLimitMs });
  const out: Evaluation = { verdict: "PASS", elapsed_fidelity: fidelity, violations };
  if (input.refusal) {
    const at = PHASES.indexOf(input.refusal.phase as Phase);
    out.refusal = { ...input.refusal, phases_left_unrun: at < 0 ? [] : PHASES.slice(at + 1) as string[] };
  }
  if (input.interruption) {
    const at = PHASES.indexOf(input.interruption.phase as Phase);
    out.interruption = { ...input.interruption, phases_left_unrun: at < 0 ? [] : PHASES.slice(at + 1) as string[] };
    out.reason = `interrupted: ${input.interruption.reason} (not a quota overrun)`;
  }
  if (violations.length > 0 || input.refusal || input.interruption || input.incomplete !== undefined) {
    out.verdict = "FAIL";
    if (input.incomplete !== undefined) out.reason = input.incomplete;
  } else if (input.deviation) out.verdict = "counters_within_limits_elapsed_unverified";
  return out;
}

/**
 * Only a genuine quota exhaustion is a resource overrun that names a dimension. A revoked or expired
 * token, a cancellation, a clock failure or any other interruption is recorded with its own reason.
 */
function chargeFailureKind(error: unknown): "quota" | "interruption" {
  return error instanceof Error && error.message === "maintenance_resource_exhausted" ? "quota" : "interruption";
}

// ---------------------------------------------------------------- source precondition (item 2)

function sourcePrecondition(root: string): { ok: true; head: string; commits: number } | { ok: false; reason: string } {
  try {
    if (sh(root, "rev-parse", "--is-inside-work-tree").trim() !== "true") throw new Error("not a work tree");
    const head = sh(root, "rev-parse", "HEAD").trim();
    if (sh(root, "rev-parse", "--is-shallow-repository").trim() === "true")
      return { ok: false, reason: "the source repository checkout is shallow" };
    const commits = Number(sh(root, "rev-list", "--count", "HEAD").trim());
    if (!Number.isSafeInteger(commits) || commits < 1000)
      return { ok: false, reason: `the source repository has ${commits} commits (fewer than 1,000)` };
    return { ok: true, head, commits };
  } catch (error) {
    return { ok: false, reason: "the source repository checkout is inaccessible: " + (error instanceof Error ? error.message.split("\n")[0] : String(error)) };
  }
}

const emitted: Array<Record<string, unknown>> = [];
function emit(record: Record<string, unknown>): Record<string, unknown> {
  emitted.push(record);
  console.log("MEASURE " + JSON.stringify(record));
  return record;
}

function environment() {
  let gitVersion = "unknown";
  try { gitVersion = execFileSync("git", ["--version"], { env: gitEnv, encoding: "utf8" }).trim(); } catch { /* recorded as unknown */ }
  return { git_version: gitVersion, node_version: process.version, platform: `${process.platform}/${process.arch}`,
    deviation: process.env.UZI_MEASURE_DEVIATION || undefined };
}

// ----------------------------------------------------------------- fixture helpers (items 4 and 6)

interface Spec {
  label: string;
  layout: Layout;
  shape: Shape;
  family?: boolean;
  loose?: number;
  decodedCap?: number;
  /** "repo": a clone of this repository's checkout; "tiny": the harness's one-commit origin (controls). */
  origin: "repo" | "tiny";
  /** Real exclusion and quiescence. Only controls run with real=false. */
  real: boolean;
  /** CONTROL ONLY: the source checkout the precondition reads (default: the checkout this test runs from). */
  sourceRoot?: string;
  /** CONTROL ONLY: replaces the archive step with a fault ("skip": no archive outcome at all). */
  controlArchive?: Error | "skip";
  /** CONTROL ONLY: lowers limits at evaluation time, from the phases the run actually charged. */
  controlLimits?: (limits: Limits, phases: ReadonlyArray<{ name: string; used: Counters }>) => Limits;
}

function looseCount(clone: string): number {
  const match = /^count: (\d+)$/m.exec(sh(clone, "count-objects", "-v"));
  return match ? Number(match[1]) : Number.NaN;
}
function countObjects(dir: string): Record<string, number> {
  const out: Record<string, number> = {};
  for (const row of sh(dir, "count-objects", "-v").trim().split("\n")) {
    const [key, value] = row.split(": ");
    if (key && value !== undefined && Number.isFinite(Number(value))) out[key] = Number(value);
  }
  return out;
}

/** Shape A (and B, C, control): the WIP the failed run left in the clone, applied as the test user. */
async function applyShape(clone: string, shape: Shape, loose: number | undefined): Promise<void> {
  const tracked = sh(clone, "ls-files", "-z").split("\0").filter(Boolean).sort();
  const usable: string[] = [];
  for (const rel of tracked) {
    if (!/\.(ts|md|yml|yaml|go|sh|txt)$/.test(rel) || /(^|\/)(core|\.gitattributes|\.gitignore)/.test(rel)) continue;
    const st = await fs.lstat(path.join(clone, rel));
    if (st.isFile() && st.size > 0 && st.size < 64 * 1024) usable.push(rel);
    if (usable.length >= 40) break;
  }
  const edit = (rel: string, n: number) => fs.appendFile(path.join(clone, rel), `\n// wip measurement edit ${n}\n`);
  if (shape === "control") {
    await fs.appendFile(path.join(clone, "README.md"), "\nwip measurement edit\n");
    await fs.writeFile(path.join(clone, "wip-untracked.txt"), "untracked work\n");
    return;
  }
  assert.ok(usable.length >= 23, "the source repository must carry at least 23 small tracked text files");
  if (shape === "B") { await edit(usable[0]!, 0); return; }
  // The staged-only edit below hashes exactly one new loose blob, so the history stops one short.
  if (shape === "C") await addLooseHistory(clone, (loose ?? LOOSE_SAMPLE_LARGEST) - 1);
  for (let i = 0; i < 20; i++) await edit(usable[i]!, i);
  await fs.mkdir(path.join(clone, "wip-measure/nested"), { recursive: true });
  for (let i = 0; i < 5; i++)
    await fs.writeFile(path.join(clone, i < 3 ? "wip-measure" : "wip-measure/nested", `untracked-${i}.txt`), `untracked work ${i}\n`);
  await edit(usable[20]!, 20); sh(clone, "add", "--", usable[20]!); // staged only: index == worktree != HEAD
  await fs.rm(path.join(clone, usable[21]!)); // an unstaged deletion
  if (shape === "C") assert.equal(looseCount(clone), loose ?? LOOSE_SAMPLE_LARGEST, "loose-object count of the fixture clone");
}

/**
 * Shape C: a clone with the loose-object count a long run leaves (clone auto-maintenance is
 * disabled, so loose objects accumulate). Real small commits first, then unreferenced blobs pad
 * the count to exactly `target`, measured with `git count-objects -v` and never recomputed.
 */
async function addLooseHistory(clone: string, target: number): Promise<void> {
  await fs.mkdir(path.join(clone, "wip-history"), { recursive: true });
  for (let i = 0; i < 40 && looseCount(clone) + 6 <= target; i++) {
    await fs.writeFile(path.join(clone, "wip-history", `h${i}.txt`), `history ${i} ${"x".repeat(i * 7)}\n`);
    sh(clone, "add", "--", `wip-history/h${i}.txt`);
    sh(clone, "commit", "-q", "--no-verify", "-m", `wip history ${i}`);
  }
  const pad = target - looseCount(clone);
  assert.ok(pad >= 0, "loose history overshot its target");
  if (pad > 0) {
    const scratch = await fs.mkdtemp(path.join(os.tmpdir(), "loose-pad-"));
    try {
      const files: string[] = [];
      for (let i = 0; i < pad; i++) { const f = path.join(scratch, `b${i}`); await fs.writeFile(f, `pad ${i} ${target}\n`); files.push(f); }
      execFileSync("git", ["-C", clone, "hash-object", "-w", "--stdin-paths"], { env: gitEnv, input: files.join("\n") + "\n", stdio: ["pipe", "ignore", "pipe"] });
    } finally { await fs.rm(scratch, { recursive: true, force: true }); }
  }
  assert.equal(looseCount(clone), target, "loose-object count of the fixture clone");
}

/** The filesystem ancestors of the data directory must satisfy the producer's trusted-ancestor DAC. */
function ancestorProblem(from: string): string | undefined {
  for (let p = from;; p = path.dirname(p)) {
    const st = fsSync.lstatSync(p);
    if (!st.isDirectory() || st.isSymbolicLink() || (st.uid !== process.getuid!() && st.uid !== 0) || (st.mode & 0o5022) !== 0)
      return `ancestor ${p} (mode ${(st.mode & 0o7777).toString(8)}, uid ${st.uid}) is not a trusted ancestor; point TMPDIR at a case-sensitive container-internal directory whose ancestors are not group/world-writable or sticky`;
    if (p === path.dirname(p)) return undefined;
  }
}

/**
 * CONTROL ONLY. A control runs below a shared sticky /tmp, so the trusted-ancestor DAC of the
 * producer's path checks is modeled for the ancestors of the data directory (descendants keep
 * their real dev/ino, mode, ownership and bytes), exactly as the origin-transfer modes of
 * runner-maintenance-terminal-source.test.ts do. A measurement never does this: it preflights the
 * real ancestors and reports UNRUN if they are not trusted.
 */
function projectTrustedAncestors(t: TestContext): void {
  const umask = process.umask(0o077);
  t.after(() => { process.umask(umask); });
  const ancestors = new Set<string>();
  for (let p = path.dirname(git.recoveryRoot);; p = path.dirname(p)) {
    ancestors.add(p);
    if (p === path.dirname(p)) break;
  }
  const descriptors = new Map<number, string>();
  const physical = (p: unknown): string => {
    if (typeof p !== "string") return "";
    const m = /^\/proc\/self\/fd\/([0-9]+)(?:\/(.*))?$/.exec(p);
    return m && descriptors.has(Number(m[1])) ? path.join(descriptors.get(Number(m[1]))!, m[2] ?? "") : p;
  };
  const project = <T extends { mode: number | bigint; uid: number | bigint; gid: number | bigint }>(p: string, st: T): T => {
    if (ancestors.has(p)) {
      const big = typeof st.mode === "bigint";
      st.mode = big ? BigInt(Number(st.mode) & ~0o7022) : Number(st.mode) & ~0o7022;
      st.uid = big ? BigInt(process.getuid!()) : process.getuid!();
      st.gid = big ? BigInt(process.getgid!()) : process.getgid!();
    }
    return st;
  };
  const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => project(physical(args[0]), await lstat(...args)));
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args), p = physical(args[0]);
    descriptors.set(file.fd, p);
    const stat = file.stat.bind(file);
    t.mock.method(file, "stat", async (...a: Parameters<typeof file.stat>) => project(p, (await stat(...a))!));
    return file;
  });
}

function realPrerequisites(): { dockerHost: string } | { unrun: string } {
  const dockerHost = process.env.UZI_MEASURE_DOCKER_HOST;
  if (!dockerHost) return { unrun: "UZI_MEASURE_DOCKER_HOST is unset: a Docker-wired worker is required, and the exclusion proof and quiescence scans probe the daemon for real" };
  if (maintenanceBootstrapRoots() === undefined) {
    try { recordMaintenanceBootstrap(snapshotMaintenanceBootstrap()); } catch { /* recorded below */ }
    if (maintenanceBootstrapRoots() === undefined)
      return { unrun: "the bootstrap process snapshot is unavailable or refused (the measurement must be the only test process; run it as the container's main process)" };
  }
  return { dockerHost };
}

// --------------------------------------------------------------------------- the pipeline run

// Owner ruling (recov2 co-sign): a two-member family may be reached only through the real blocked/inherited
// path, with no injected member. At bed7dd94 it cannot be reached at all, so the variant is UNRUN.
const FAMILY_UNREACHABLE = "admission not reached for a two-member family: it is unreachable through production code: the family input list is " +
  "[genesis] on the normal-failed path and the inherited originals on the blocked path (runner.ts closedSourceObservation, " +
  "`const originals = blocked ? flight.sourceInherited?.family?.originals : [genesis]`), and the inherited originals come " +
  "from an earlier observation built the same way, so every real family has one member; a sibling ledger entry for the run " +
  "makes the real scan refuse (git.ts matchMaintenanceSourceFamily `incomplete known family`). Reaching two members needs an " +
  "injected member or a production change; the measurement does neither; this is a reachability conflict between #3442 Design 5 and current production, for maintainer disposition";

// The exact refusals of GitCache.assertOwedBare (git.ts), the precondition of the guarded archive call: a
// fixture bare that cannot satisfy it is UNRUN with the reason, not a counter FAIL.
const OWED_BARE_REFUSALS: ReadonlySet<string> = new Set(["owed candidates require a known worker bare path",
  "unsafe worker bare directory", "owed candidates require a bare repository"]);

/**
 * Fixture precondition standing in for Worker idleness: no process in this container other than this one,
 * its ancestors and its own descendants (read from /proc), and a runner with no execution tail but the
 * observed run's own. It is NOT evidence about the production Worker.isIdle integration.
 */
function readParents(): Map<number, number> {
  const parents = new Map<number, number>();
  for (const name of fsSync.readdirSync("/proc")) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = fsSync.readFileSync(`/proc/${name}/stat`, "utf8");
      parents.set(Number(name), Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]));
    } catch { /* exited while scanning */ }
  }
  return parents;
}
/** Pids that are neither `self`, an ancestor of it, nor one of its descendants. */
function foreignPids(parents: ReadonlyMap<number, number>, self: number): number[] {
  const related = new Set<number>([self]);
  for (let p = parents.get(self); p !== undefined && p > 0 && !related.has(p); p = parents.get(p)) related.add(p);
  const descends = (pid: number): boolean => {
    for (let p: number | undefined = pid, i = 0; p !== undefined && p > 0 && i < 128; p = parents.get(p), i++) if (p === self) return true;
    return false;
  };
  return [...parents.keys()].filter(pid => !related.has(pid) && !descends(pid));
}
function runnerTailProblem(runner: unknown, runId: string): string | undefined {
  const tails = (runner as { executionTails: Map<string, unknown> }).executionTails;
  return [...tails.keys()].some(id => id !== runId) ? "the runner holds an execution tail for another run" : undefined;
}
/** The measurement is PID 1 or a descendant of PID 1 through at most the test runner (PID 1 -> runner -> this file). */
function ancestryProblem(parents: ReadonlyMap<number, number>, self: number): string | undefined {
  const parent = parents.get(self) ?? 0;
  if (self === 1 || parent === 1 || (parents.get(parent) ?? 0) === 1) return undefined;
  return "the measurement is not PID 1, nor a child of PID 1, nor a child of PID 1's test runner";
}
function soleExecutionProblem(runner?: unknown, runId?: string, readTree: () => Map<number, number> = readParents): string | undefined {
  const tree = readTree();
  const ancestry = ancestryProblem(tree, process.pid);
  if (ancestry) return ancestry;
  const foreign = foreignPids(tree, process.pid);
  if (foreign.length > 0) return `${foreign.length} process(es) other than the measurement, its ancestors and its descendants run in this container`;
  return runner ? runnerTailProblem(runner, runId ?? "") : undefined;
}

/** The real writer exclusion; its idleness is the harness's sole-execution check, evaluated at entry. */
function realExclusion(runner: () => unknown, runId: string, dockerHost: string, readTree?: () => Map<number, number>): MaintenanceWriterExclusion {
  return new MaintenanceWriterExclusion({ gate: new DindPruneGate(), isIdle: () => soleExecutionProblem(runner(), runId, readTree) === undefined,
    proof: scans => terminalMaintenanceProof(scans, dockerHost), log: nullLogger() });
}

const SUPPRESSED_REASON = "pre-slot production bundle path suppressed by the harness";
/** Gate eligibility. Every run installs the produceRecoveryBundle suppression, so none qualifies for a C2 PASS. */
/** The Spec fields that exist only for controls: a real measurement must set none of them. */
function controlFields(spec: Partial<Spec>): string[] {
  return (["sourceRoot", "controlArchive", "controlLimits", "decodedCap"] as const).filter(key => spec[key] !== undefined);
}

function eligibility(spec: Partial<Spec> & { real: boolean; origin: "repo" | "tiny" }): { eligible: boolean; reasons: string[] } {
  const reasons: string[] = [];
  if (!spec.real) reasons.push("control run (stubbed exclusion and quiescence)");
  if (spec.origin !== "repo") reasons.push("not a clone of the source repository");
  if (controlFields(spec).length > 0) reasons.push("control-only field(s) set: " + controlFields(spec).join(", "));
  reasons.push(SUPPRESSED_REASON);
  return { eligible: reasons.length === 0, reasons };
}

/** The gate-level verdict of C2: only eligible runs can qualify; with none, no qualifying measurement exists. */
function gateSummary(records: ReadonlyArray<Record<string, unknown>>) {
  const runs = records.filter(r => r.kind === "composed" && r.label !== undefined);
  const eligible = runs.filter(r => r.eligible_for_gate === true);
  const verdict = eligible.length === 0 ? "UNRUN" : eligible.some(r => r.verdict === "FAIL" || r.verdict === "UNRUN") ? "FAIL"
    : eligible.every(r => r.verdict === "PASS") ? "PASS" : "counters_within_limits_elapsed_unverified";
  return { kind: "c2_gate_summary", verdict, eligible_runs: eligible.length, runs: runs.length,
    reason: eligible.length === 0 ? "no qualifying measurement is possible: every run depends on a harness intervention that is not a Design-7 substitution (" + SUPPRESSED_REASON + "); per-run counters are diagnostics" : undefined,
    per_run: runs.map(r => ({ label: r.label, verdict: r.verdict, eligible_for_gate: r.eligible_for_gate ?? false })) };
}

// Module-level hooks for controls only. They are not Spec fields, so a real spec stays free of control fields
// and the control-only guard does not count them; each control sets and restores them itself.
const testHooks: { sourceRoot?: string; ancestorProbe?: string } = {};

interface RunResult { record: Record<string, unknown>; evaluation: Evaluation }

async function runPipeline(t: TestContext, spec: Spec): Promise<RunResult> {
  const env = environment();
  const head: Record<string, unknown> = { kind: "composed", label: spec.label, layout: spec.layout, shape: spec.shape,
    family: spec.family ? "two-source" : "single-source", ...env };
  const finish = (evaluation: Evaluation, body: Record<string, unknown> = {}): RunResult =>
    ({ record: emit({ ...head, ...body, verdict: evaluation.verdict, verdict_detail: evaluation }), evaluation });
  const unrun = (reason: string, body: Record<string, unknown> = {}) =>
    finish(evaluate({ unrun: reason, limits: {}, elapsedLimitMs: SLOT_ELAPSED_LIMIT_MS, elapsedMs: 0, phases: [], deviation: env.deviation }), { ...body, reason });

  // ---- control-only seams never reach a real measurement
  if (spec.real && controlFields(spec).length > 0)
    return unrun("a real measurement carries control-only field(s): " + controlFields(spec).join(", "));

  // ---- the two-source family cannot be reached through production code: UNRUN, nothing else asserted
  if (spec.family) return unrun(FAMILY_UNREACHABLE, { family_reachability: FAMILY_UNREACHABLE });

  // ---- preconditions: UNRUN with the reason, nothing else asserted
  let source: { head: string; commits: number } | undefined;
  if (spec.origin === "repo") {
    const precondition = sourcePrecondition(testHooks.sourceRoot ?? spec.sourceRoot ?? REPO_ROOT);
    if (!precondition.ok) return unrun(precondition.reason);
    source = precondition;
  }
  let dockerHost = "http://unused.invalid";
  if (spec.real) {
    const ancestors = ancestorProblem(testHooks.ancestorProbe ?? path.dirname(git.recoveryRoot));
    if (ancestors) return unrun(ancestors);
    const crowded = soleExecutionProblem();
    if (crowded) return unrun("sole execution is unverified, so the idleness precondition fails: " + crowded);
    const prerequisites = realPrerequisites();
    if ("unrun" in prerequisites) return unrun(prerequisites.unrun);
    dockerHost = prerequisites.dockerHost;
  }
  if (!spec.real) projectTrustedAncestors(t);
  head.candidate_sha = source?.head;
  head.source_commits = source?.commits;

  if (spec.real) {
    setQuiescenceViewForTests(undefined); // the real /proc, not the hermetic empty view (restored in afterEach)
  }
  if (spec.decodedCap !== undefined) Object.defineProperty(git, "recoveryDecodedLimitBytes", { value: spec.decodedCap });

  // ---- origin: a clone of this repository's checkout (item 2); the runner makes the bare (item 4)
  if (source) {
    const origin = path.join(path.dirname(fx.dataDir), "origin-src");
    execFileSync("git", ["clone", "-q", "--no-local", "--no-hardlinks", testHooks.sourceRoot ?? spec.sourceRoot ?? REPO_ROOT, origin], { env: gitEnv, stdio: "pipe" });
    sh(origin, "checkout", "-q", "-B", "main", source.head);
    disableAutoMaintenance(origin, gitEnv);
    fx.originPath = origin;
  }
  const claim = gitlabClaim(3031, { claim_generation: 1, inventory_guarded: true });
  claim.repo.id = "00000000-0000-4000-8000-000000000033";
  const workerId = "00000000-0000-4000-8000-000000000034";
  const holdId = "00000000-0000-4000-8000-000000000032";
  const bare = git.barePathFor(claim.repo.clone_url);

  // ---- substitution 1: the guard method
  t.mock.method(RecoveryCoordinator.prototype as unknown as { terminalOriginAdmissionEnabled(): boolean },
    "terminalOriginAdmissionEnabled", () => true);

  // ---- accounting: every charge lands in the phase current at that moment
  const names = PHASES as readonly string[];
  const used: Record<string, Counters> = Object.fromEntries(names.map(n => [n, {}]));
  const totals: Counters = {};
  let phase: Phase = "preamble", slotStarted: number | undefined, slotEnded: number | undefined;
  let refusedCharge: { resource: string; amount: number; used: number; phase: string } | undefined;
  let interruption: { reason: string; resource: string; phase: string } | undefined;
  const phaseMs: Record<string, number> = {};
  let phaseSince = performance.now();
  const setPhase = (next: Phase) => {
    const now = performance.now();
    phaseMs[phase] = (phaseMs[phase] ?? 0) + (now - phaseSince);
    phase = next; phaseSince = now;
  };
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  t.mock.method(MaintenanceResources, "consume", (resource: Dimension, amount: number) => {
    try { consume(resource, amount); }
    catch (error) {
      if (chargeFailureKind(error) === "quota") refusedCharge ??= { resource, amount, used: totals[resource] ?? 0, phase };
      else interruption ??= { reason: error instanceof Error ? error.message : String(error), resource, phase };
      throw error;
    }
    totals[resource] = (totals[resource] ?? 0) + amount;
    used[phase]![resource] = (used[phase]![resource] ?? 0) + amount;
  });
  const rss = { start: 0, peak: 0 };
  const sampler = setInterval(() => { rss.peak = Math.max(rss.peak, process.memoryUsage.rss()); }, 25);
  sampler.unref();
  t.after(() => clearInterval(sampler));

  // ---- the limits come from the admission (item 12): the persisted slot reservation
  let limits: Limits = {}, admittedCap: number | undefined, slotMs = SLOT_ELAPSED_LIMIT_MS, limitsError: string | undefined;
  const admit = RecoveryMaintenance.prototype.admit;
  t.mock.method(RecoveryMaintenance.prototype, "admit", async function (this: RecoveryMaintenance, ...args: Parameters<typeof admit>) {
    const result = await admit.apply(this, args);
    if (result.ok) {
      slotStarted = performance.now(); rss.start = process.memoryUsage.rss(); rss.peak = rss.start;
      phaseSince = slotStarted; for (const key of Object.keys(phaseMs)) delete phaseMs[key];
      try {
        const table = JSON.parse(await fs.readFile(path.join(path.dirname(git.recoveryRoot), "recovery-maintenance", "complete.json"), "utf8")).value;
        const cycle = table.cycles.find((c: { identity: { run: string } }) => c.identity.run === claim.run_id);
        const slot = cycle.slots[cycle.slots.length - 1];
        limits = { ...slot.reserved }; admittedCap = cycle.config.decodedCap; slotMs = slot.deadline - slot.start;
      } catch (error) { limitsError = error instanceof Error ? error.message : String(error); }
    }
    return result;
  });

  // ---- substitution 2: the faked server behind the real strict WorkerClient
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{ hold_id: holdId, generation: 1, inventory_guarded: true, has_available_capture: false }] }));
  const strict = new WorkerClient("http://origin.test", "fixture-journal-key", "test", nullLogger(), {
    fetch: async input => {
      const url = new URL(String(input));
      if (url.pathname.endsWith("/register")) return Response.json({ worker_id: workerId, protocol_features: [] });
      return Response.json(url.search ? { run_id: claim.run_id, exact_hold: {
        run_id: claim.run_id, hold_id: holdId, generation: 1, repo_id: claim.repo.id,
        original_worker_id: workerId, original_worker_identity: "human-label", live_worker_id: workerId,
        live_run_id: claim.run_id, inventory_guarded: true, state: "open", run_status: "failed",
        current_claim_generation: 1, claim_released: true } } : { run_id: claim.run_id, holds: [{ hold_id: holdId, generation: 1,
        inventory_guarded: true, has_available_capture: false }] });
    },
  });
  await strict.register("worker", undefined, 1, undefined, ["recovery_held_publication_v2"]);
  t.mock.method(client, "observeTerminalMaintenanceHold", strict.observeTerminalMaintenanceHold.bind(strict));
  t.mock.method(client, "isCurrentTerminalMaintenanceHold", strict.isCurrentTerminalMaintenanceHold.bind(strict));

  // ---- the archive phase is the only production bundle call allowed; any other is refused
  let archive: Record<string, unknown> | undefined, archiveWindow = false, archiveError: unknown, archiveEnded: number | undefined;
  const produce = git.produceRecoveryBundle.bind(git);
  const stray: Array<{ phase: string; inSlot: boolean; afterAdmission: boolean }> = [];
  t.mock.method(git, "produceRecoveryBundle", async (...args: Parameters<typeof produce>) => {
    if (archiveWindow) return produce(...args);
    stray.push({ phase, inSlot: MaintenanceResources.active() && !MaintenanceResources.discoveryActive(), afterAdmission: slotStarted !== undefined });
    // Models the ordinary capture refused as oversize (produceBundle records archiveRefusal "oversize").
    throw new RecoveryBundleTooLargeError(RECOVERY_MAX_BUNDLE_BYTES + 1, RECOVERY_MAX_BUNDLE_BYTES);
  });

  // ---- real quiescence (a wrapper that only reads the envelope and the charge around the real call)
  const proofs: Array<{ site: string; phase: string; in_slot: boolean; charged: Counters; envelope?: { metadataBytes: number; adminEntries: number } }> = [];
  const quiesce = async (request: QuiesceRunRequest) => {
    const before = { ...totals };
    const outcome = await quiesceRunAttempt(request);
    const charged: Counters = {};
    for (const key of Object.keys(totals) as Dimension[]) charged[key] = (totals[key] ?? 0) - (before[key] ?? 0);
    proofs.push({ site: request.site ?? "unknown", phase, in_slot: slotStarted !== undefined, charged, envelope: request.producerWorkLimits });
    return outcome;
  };
  const controlQuiesce = async (request: QuiesceRunRequest) => ({
    process: { state: "quiescent" as const, processes: [], killed: [], detail: "" },
    docker: { state: (request.site === "retained_ops" ? "docker_unconfirmed" : "not_wired") as "docker_unconfirmed" | "not_wired", removed: [], detail: "" },
  });
  const exclusion = spec.real ? realExclusion(() => runner, claim.run_id, dockerHost) : passingExclusion();

  // ---- the runner: a failed run leaves the WIP shape in its attempt clone
  let genesis = "", baseCommit = "";
  const fixtures: Record<string, unknown> = {};
  const log = recordingLogger();
  const target = path.join(homeDir, claim.run_id);
  const runner = runnerWith(() => ({
    homeDir: target,
    executor: {
      run: async ctx => {
        genesis = ctx.worktreePath;
        sh(genesis, "config", "user.name", "measure"); sh(genesis, "config", "user.email", "measure@example.invalid");
        baseCommit = sh(genesis, "rev-parse", "HEAD").trim();
        if (spec.layout === "dissociated") await (git as unknown as { materializeRunnerClone(c: string, r: string[]): Promise<void> }).materializeRunnerClone(genesis, [baseCommit]);
        await fs.mkdir(target, { recursive: true });
        throw new Error("ordinary executor failure");
      },
      reapAttributedProcesses: async () => ({ complete: true, killed: [], left: [] }),
    },
  }), fakeGitlab().gitlab, "fixture-journal-key", log.logger, {
    maintenanceExclusion: exclusion, dockerHost, recoveryRetryMs: 1,
    quiesceRun: spec.real ? quiesce : controlQuiesce,
  });

  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  let notified: Promise<void> | undefined;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator, ...args: Parameters<typeof resume>) {
    notified = resume.apply(this, args);
    return notified;
  });

  // ---- phase markers and the archive, all through wrappers on named GitCache / runner scopes
  let w0Invocations = 0, transferEntered = 0, importerResult: { kind: string; sourceSha: string } | undefined;
  let importerError: unknown;
  const observer = (git as unknown as { observeMaintenanceTransferPhase<T>(p: string, a: () => Promise<T>): Promise<T> }).observeMaintenanceTransferPhase.bind(git);
  t.mock.method(git as unknown as { observeMaintenanceTransferPhase: (p: Phase, a: () => Promise<unknown>) => Promise<unknown> }, "observeMaintenanceTransferPhase", async (name: Phase, action: () => Promise<unknown>) => {
    setPhase(name);
    try { return await observer(name, action); } finally { setPhase(`after_${name}` as Phase); }
  });
  const scoped = git.withBoundaryProcessSpawner.bind(git);
  t.mock.method(git as unknown as { withBoundaryProcessSpawner: typeof scoped }, "withBoundaryProcessSpawner", (...args: Parameters<typeof scoped>) => {
    const [spawn, signal, action, hooks] = args;
    return scoped(async request => {
      if (phase === "w0_p0" && request.argv[1] === "-e" && String(request.argv[2]).startsWith("// uzi-wip-pin")) setPhase("w0_p1");
      return spawn(request);
    }, signal, action, hooks);
  });
  const importer = git.importMaintenanceTip.bind(git);
  t.mock.method(git, "importMaintenanceTip", async (...args: Parameters<typeof importer>) => {
    try {
      importerResult = await importer(...args);
      setPhase("archive");
      const outDir = path.join(path.dirname(fx.dataDir), "archive-out");
      await fs.mkdir(outDir, { recursive: true });
      const outPath = path.join(outDir, "producer.bundle");
      archiveWindow = true;
      try {
        // Self-contained first; the guarded thin fallback is taken inside produceRecoveryBundle only
        // on RecoveryBundleTooLargeError.
        if (spec.controlArchive instanceof Error) throw spec.controlArchive;
        const result = spec.controlArchive === "skip" ? undefined : await git.produceRecoveryBundle(bare, { sourceSha: importerResult.sourceSha, outPath,
          guardedDefaultBranch: args[0].defaultBranch });
        if (result) archive = { kind: result.selfContained ? "self-contained" : "thin", bytes: result.byteSize, cap_bytes: RECOVERY_MAX_BUNDLE_BYTES,
          prerequisite_shas: result.prerequisiteShas.length, source_sha: result.sourceSha };
      } catch (error) {
        archiveError = error;
        throw error;
      } finally { archiveWindow = false; archiveEnded = performance.now(); setPhase("post_archive"); }
      return importerResult;
    } catch (error) {
      importerError = error;
      throw error;
    }
  });
  const boundary = runner.withRetainedSourceBoundary.bind(runner);
  t.mock.method(runner, "withRetainedSourceBoundary", async (...[runId, paths, action, signal]: Parameters<typeof boundary>) =>
    boundary(runId, paths, async ctx => {
      // A failed run's own snapshot has already flushed the clone by the time the terminal origin is
      // admitted, so the WIP shape is applied here, inside the boundary and before the capture
      // (as the origin-transfer modes of runner-maintenance-terminal-source.test.ts do).
      await applyShape(genesis, spec.shape, spec.loose);
      fixtures.genesis = { path: genesis, count_objects: countObjects(genesis), alternates: fsSync.existsSync(path.join(genesis, ".git/objects/info/alternates")) };
      fixtures.bare = { count_objects: countObjects(bare) };
      const upload = t.mock.method(client, "uploadRecoveryBundle");
      const reserve = t.mock.method(client, "reserveRecoveryCapture");
      const held = t.mock.method(client, "publishHeld");
      const release = t.mock.method(client, "releaseRecoveryCustody");
      const forwarded: typeof ctx = {
        ...ctx,
        captureWip: async (clone, oldHead, callbacks) => {
          w0Invocations++;
          const result = await ctx.captureWip(clone, oldHead, {
            beforeStep: async (step, when) => {
              if (step === "status" && when === "prepare") setPhase("w0_p0");
              else if (step === "status" && when === "dispatch") setPhase("w0_p2_p3");
              else if (step === "add" && when === "prepare") setPhase("w0_p4a");
              else if (step === "commit" && when === "prepare") setPhase("w0_p4b");
              return callbacks.beforeStep(step, when);
            },
            afterRunnerGit: async step => { await callbacks.afterRunnerGit(step); if (step === "commit") setPhase("w0_p5"); },
          });
          setPhase("transfer_entry");
          return result;
        },
        withMaintenanceTransfer: async transfer => { transferEntered++; return ctx.withMaintenanceTransfer(transfer); },
      };
      const outcome = await action(forwarded);
      for (const spy of [upload, reserve, held, release]) {
        assert.equal(spy.mock.callCount(), 0, "the C2 pipeline grants no FINAL, upload or custody release");
        spy.mock.restore();
      }
      return outcome;
    }, signal));

  // ---- run it: the executor fails, the runner notifies the coordinator, the coordinator admits
  const started = performance.now();
  try {
    await runner.execute(claim);
    if (notified) await notified;
  } finally {
    runner.shutdown();
    await runner.shutdownRecoveries();
  }
  slotEnded = performance.now();
  setPhase("post_archive");
  clearInterval(sampler);
  rss.peak = Math.max(rss.peak, process.memoryUsage.rss());
  const refusals = log.lines.filter(l => (l as { msg?: string }).msg === "recovery: terminal origin refused")
    .map(l => (l as { reason?: string }).reason);

  // ---- assemble the verdict from what was actually observed
  const body: Record<string, unknown> = { fixtures, w0_invocations: w0Invocations, transfer_entered: transferEntered,
    bundle_attempts_refused_before_admission: stray.filter(s => !s.afterAdmission).length,
    bundle_attempts_refused_after_admission: stray.filter(s => s.afterAdmission).length };
  if (limitsError !== undefined || admittedCap === undefined) {
    body.admission = { error: limitsError ?? "no slot was admitted", refusals };
    const reason = refusals.length > 0 ? `admission refused before the charged slot: ${refusals.join(",")}` : `no charged slot: ${limitsError ?? "admission not reached"}`;
    return unrun(reason, body);
  }
  const phases = PHASES.map(name => ({ name, used: used[name]! }));
  if (spec.controlLimits) limits = spec.controlLimits(limits, phases);
  const slotElapsed = Math.round((slotEnded - (slotStarted ?? started)));
  const failure = (importerError ?? archiveError) as unknown;
  let incomplete: string | undefined, refusal: { phase: string; reason: string } | undefined;
  const failurePhase = refusedCharge?.phase ?? interruption?.phase ?? (archiveError ? "archive" : importerError ? "transfer_entry" : refusals.length > 0 ? phase : undefined);
  if (failure !== undefined || refusals.length > 0) {
    const message = failure instanceof RecoveryBundleTooLargeError ? "RecoveryBundleTooLargeError: neither self-contained nor guarded thin bundle fits the per-bundle cap"
      : failure instanceof Error ? failure.message : refusals.join(",");
    refusal = { phase: failurePhase ?? "unknown", reason: refusals.length > 0 ? refusals.join(",") + (failure instanceof Error ? ` (${failure.message})` : "") : message };
  }
  if (w0Invocations === 0 && !refusal) incomplete = "the admission never reached the W0 capture";
  else if (!archive && !refusal) incomplete = "the pipeline ended without an archive outcome";
  const strayInSlot = stray.filter(s => s.afterAdmission);
  if (strayInSlot.length > 0) incomplete = `a production bundle call after slot admission but outside the archive window was refused (phase ${strayInSlot.map(s => s.phase).join(",")})`;
  const archiveNotApplicable = archiveError instanceof Error && OWED_BARE_REFUSALS.has(archiveError.message);
  const envelopeOverruns: Violation[] = [];
  for (const proof of proofs.filter(p => p.in_slot)) for (const dimension of ["metadataBytes", "adminEntries"] as const) {
    const limit = proof.envelope?.[dimension];
    if (limit !== undefined && (proof.charged[dimension] ?? 0) > limit)
      envelopeOverruns.push({ dimension: `familyProof.${dimension}`, phase: proof.phase, used: proof.charged[dimension] ?? 0, limit });
  }
  // The closure budget is configured to the same C as the slot's decoded quota and refuses first, as a
  // typed refusal naming that limit; it is a cap overrun of the `decoded` dimension, not a layout refusal.
  const capRefusals: Violation[] = refusal && /decoded verification limit/.test(refusal.reason)
    ? [{ dimension: "decoded", phase: refusal.phase, limit: limits.decoded ?? admittedCap, refused: true }] : [];
  const evaluation = archiveNotApplicable
    ? evaluate({ unrun: `the fixture cannot satisfy the archive precondition: ${(archiveError as Error).message}`, limits, elapsedLimitMs: slotMs, elapsedMs: slotElapsed, phases, deviation: env.deviation })
    : evaluate({ limits, elapsedLimitMs: slotMs, elapsedMs: slotElapsed, phases, refusedCharge, interruption, capRefusals, envelopeOverruns, refusal, incomplete, deviation: env.deviation });
  const eligible = eligibility(spec);
  const record = emit({
    ...head, ...body,
    candidate_sha: source?.head, source_commits: source?.commits, run_id: claim.run_id,
    substitutions: ["guard-method: RecoveryCoordinator.terminalOriginAdmissionEnabled",
      "faked-server: strict WorkerClient over a faked worker API, with client.hasFeature and client.listRecoveryHolds answered directly by the harness",
      "isIdle: harness check of sole execution in the container and a settled runner, passed to the real MaintenanceWriterExclusion (no Worker object exists; this does not prove production Worker-idleness integration)",
      "produceRecoveryBundle: every call outside the archive window throws RecoveryBundleTooLargeError (models the ordinary pre-slot capture refused as oversize, not a journaled bundle); calls are counted; every run depending on it is gate-ineligible",
      ...(spec.controlLimits ? ["CONTROL ONLY: limits lowered at evaluation time"] : []),
      ...(spec.controlArchive ? ["CONTROL ONLY: archive step replaced by a fault"] : []),
      ...(spec.real ? [] : ["CONTROL ONLY: passingExclusion", "CONTROL ONLY: stubbed quiescence verdict", "CONTROL ONLY: trusted-ancestor DAC projection"])],
    layout_construction: spec.layout === "dissociated" ? "real GitCache.materializeRunnerClone" : "clone --shared, alternates retained",
    uid_mode: process.env.UZI_UID_SPLIT === "1" ? "split-UID requested (not covered by this harness)" : "single-UID (UZI_UID_SPLIT unset); split-UID is not covered",
    archive_phase_note: "harness-placed archive phase (no production caller yet)",
    eligible_for_gate: eligible.eligible, ineligible_reasons: eligible.reasons,
    deviation: env.deviation ?? null, elapsed_fidelity: evaluation.elapsed_fidelity,
    elapsed_ms: slotElapsed, elapsed_to_archive_end_ms: archiveEnded !== undefined && slotStarted !== undefined ? Math.round(archiveEnded - slotStarted) : null,
    pipeline_elapsed_ms: Math.round(performance.now() - started),
    phase_elapsed_ms: Object.fromEntries(Object.entries(phaseMs).map(([k, v]) => [k, Math.round(v)])),
    rss: { start_bytes: rss.start, peak_bytes: rss.peak, delta_bytes: rss.peak - rss.start },
    limits, limit_source: "persisted slot reservation of the admitted ledger cycle (RecoveryMaintenance.configuration over GitCache.recoveryDecodedLimitBytes)",
    configured_decoded_cap: admittedCap, elapsed_limit_ms: slotMs,
    totals, rpcAttempts: totals.rpcAttempts ?? 0,
    phases: Object.fromEntries(PHASES.map(n => [n, used[n]])),
    family_proofs: proofs,
    archive: archive ?? { kind: "none", bytes: 0, cap_bytes: RECOVERY_MAX_BUNDLE_BYTES, error: archiveError instanceof Error ? archiveError.message : undefined },
    transfer: importerResult ?? null,
    loose_objects: spec.shape === "C" ? { target: spec.loose, source: spec.loose === LOOSE_SAMPLE_LARGEST ? LOOSE_SAMPLE_NOTE : "ladder rung above the sample" } : undefined,
    refusal: evaluation.refusal ?? null, refusals, reason: evaluation.reason,
    verdict: evaluation.verdict, verdict_detail: evaluation,
  });
  return { record, evaluation };
}

// ------------------------------------------------------------------------------ measurements

const ladder: Array<{ loose: number; verdict: string }> = [];
const within = (verdict: string) => verdict === "PASS" || verdict === "counters_within_limits_elapsed_unverified";

describe("#3031 composed measurement (C2)", () => {
  afterEach(() => { restoreHermeticView(); });

  for (const layout of ["shared", "dissociated"] as const) {
    for (const shape of ["A", "B", "C"] as const) {
      it(`layout ${layout}, shape ${shape}`, { skip: MEASURE_SKIP, timeout: 120_000 }, async t => {
        const loose = shape === "C" ? LOOSE_SAMPLE_LARGEST : undefined;
        const { evaluation } = await runPipeline(t, { label: `${layout}-${shape}`, layout, shape, loose, origin: "repo", real: true });
        if (shape === "C") ladder.push({ loose: LOOSE_SAMPLE_LARGEST, verdict: evaluation.verdict });
      });
    }
    it(`layout ${layout}, two-source family (UNRUN: unreachable through production code)`, { skip: MEASURE_SKIP, timeout: 120_000 }, async t => {
      const { evaluation } = await runPipeline(t, { label: `${layout}-family`, layout, shape: "A", family: true, origin: "repo", real: true });
      assert.equal(evaluation.verdict, "UNRUN");
    });
  }

  for (const loose of LOOSE_LADDER) {
    it(`loose-object ladder rung ${loose} (dissociated, shape C)`, { skip: MEASURE_SKIP, timeout: 120_000 }, async t => {
      if (ladder.some(r => !within(r.verdict))) return t.skip("an earlier rung already left the limits");
      const { evaluation } = await runPipeline(t, { label: `ladder-${loose}`, layout: "dissociated", shape: "C", loose, origin: "repo", real: true });
      ladder.push({ loose, verdict: evaluation.verdict });
    });
  }

  it("C2 gate summary", { skip: MEASURE_SKIP }, () => {
    emit(gateSummary(emitted));
  });

  it("loose-object ceiling summary", { skip: MEASURE_SKIP }, () => {
    const passing = ladder.filter(r => within(r.verdict)).map(r => r.loose);
    const failing = ladder.filter(r => !within(r.verdict)).map(r => r.loose);
    emit({ kind: "loose_object_ceiling", rungs: ladder, largest_within_every_limit: passing.length ? Math.max(...passing) : null,
      smallest_over_a_limit_or_unrun: failing.length ? Math.min(...failing) : null,
      note: "ceiling = largest loose-object count the harness ran that stayed within every limit; the ladder is bounded, so it is a lower bound unless a rung left the limits",
      sample: `${LOOSE_SAMPLE_LARGEST}: ${LOOSE_SAMPLE_NOTE}` });
  });
});

// ------------------------------------------------------------- verdict controls (no env var)

describe("#3031 composed measurement verdict controls", () => {
  const limits: Limits = { decoded: 1000, closureEntries: 100, sourceBytes: 1000, sourceEntries: 100, artifactBytes: 1000,
    metadataBytes: 1000, adminEntries: 100, sources: 10, launches: 100, rpcAttempts: 10 };
  const phase = (name: string, used: Counters) => ({ name, used });
  const base = { limits, elapsedLimitMs: SLOT_ELAPSED_LIMIT_MS, elapsedMs: 1000 };

  it("control 9: an overrun names the dimension and the phase, the archive phase included", () => {
    const mid = evaluate({ ...base, phases: [phase("w0_p0", { sourceEntries: 60 }), phase("walk", { sourceEntries: 60 }), phase("archive", {})] });
    assert.equal(mid.verdict, "FAIL");
    assert.deepEqual(mid.violations.map(v => [v.dimension, v.phase]), [["sourceEntries", "walk"]]);
    const archive = evaluate({ ...base, phases: [phase("import", { artifactBytes: 500 }), phase("archive", { artifactBytes: 600 })] });
    assert.equal(archive.verdict, "FAIL");
    assert.deepEqual(archive.violations.map(v => [v.dimension, v.phase]), [["artifactBytes", "archive"]]);
    const refused = evaluate({ ...base, phases: [phase("walk", { sources: 10 })], refusedCharge: { resource: "sources", amount: 1, used: 10, phase: "proof" } });
    assert.equal(refused.verdict, "FAIL");
    assert.deepEqual(refused.violations.map(v => [v.dimension, v.phase, v.refused]), [["sources", "proof", true]]);
    const unlimited = evaluate({ ...base, limits: { sourceEntries: 100 }, phases: [phase("walk", { sourceEntries: 1, mystery: 5 } as Counters)] });
    assert.equal(unlimited.verdict, "FAIL");
    assert.deepEqual(unlimited.violations.map(v => [v.dimension, v.phase, v.unlimited]), [["mystery", "walk", true]]);
    const slow = evaluate({ ...base, elapsedMs: SLOT_ELAPSED_LIMIT_MS + 1, phases: [] });
    assert.deepEqual(slow.violations.map(v => v.dimension), ["elapsedMs"]);
  });

  it("control 9: quota exhaustion names a dimension; a deadline, cancellation or revoked token is an interruption, never an overrun", () => {
    assert.equal(chargeFailureKind(new Error("maintenance_resource_exhausted")), "quota");
    for (const reason of ["maintenance_token_invalid", "maintenance_clock_invalid", "maintenance deadline exhausted", "This operation was aborted"])
      assert.equal(chargeFailureKind(new Error(reason)), "interruption", reason);
    assert.equal(chargeFailureKind("maintenance_resource_exhausted"), "interruption", "only an Error carrying the quota reason counts");
    const phases = [phase("walk", { sourceEntries: 1 })];
    const quota = evaluate({ ...base, phases, refusedCharge: { resource: "sourceEntries", amount: 1, used: 100, phase: "walk" } });
    assert.deepEqual(quota.violations.map(v => [v.dimension, v.phase, v.refused]), [["sourceEntries", "walk", true]]);
    assert.equal(quota.interruption, undefined);
    const cut = evaluate({ ...base, phases, interruption: { reason: "maintenance_token_invalid", resource: "launches", phase: "selection" } });
    assert.equal(cut.verdict, "FAIL");
    assert.deepEqual(cut.violations, [], "an interruption names no dimension");
    assert.equal(cut.interruption?.phase, "selection");
    assert.match(String(cut.reason), /interrupted: maintenance_token_invalid/);
    assert.ok(cut.interruption?.phases_left_unrun.includes("archive"));
  });

  it("control 9: a refusal is recorded with the phases left unrun and is never a counter PASS", () => {
    const evaluation = evaluate({ ...base, phases: [phase("walk", { sourceEntries: 1 })], refusal: { phase: "selection", reason: "maintenance_source_layout_unsupported" } });
    assert.equal(evaluation.verdict, "FAIL");
    assert.deepEqual(evaluation.refusal?.phases_left_unrun, ["after_selection", "pack", "after_pack", "proof", "after_proof", "import", "after_import", "archive", "post_archive"]);
    assert.equal(evaluate({ ...base, phases: [], incomplete: "no archive outcome" }).verdict, "FAIL");
  });

  it("control 9: within limits is PASS; a deviation reports counters_within_limits_elapsed_unverified; a FAIL stays FAIL", () => {
    const clean = [phase("walk", { sourceEntries: 100 }), phase("archive", { artifactBytes: 1000 })];
    assert.deepEqual([evaluate({ ...base, phases: clean }).verdict, evaluate({ ...base, phases: clean }).elapsed_fidelity], ["PASS", "native"]);
    const deviated = evaluate({ ...base, phases: clean, deviation: "emulated arm64" });
    assert.equal(deviated.verdict, "counters_within_limits_elapsed_unverified");
    assert.equal(deviated.elapsed_fidelity, "unverified: emulated arm64");
    assert.equal(evaluate({ ...base, phases: [phase("walk", { sourceEntries: 101 })], deviation: "emulated arm64" }).verdict, "FAIL");
  });

  it("control 9: an inaccessible or shallow source is UNRUN with the reason", async t => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "measure-3031-source-"));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    const full = path.join(dir, "full");
    sh(dir, "init", "-q", "-b", "main", full);
    await fs.writeFile(path.join(full, "f"), "1");
    sh(full, "add", "."); sh(full, "commit", "-q", "-m", "one"); await fs.appendFile(path.join(full, "f"), "2"); sh(full, "commit", "-q", "-a", "-m", "two");
    const shallow = path.join(dir, "shallow");
    execFileSync("git", ["clone", "-q", "--depth=1", "file://" + full, shallow], { env: gitEnv, stdio: "pipe" });
    for (const [source, expected] of [[shallow, /shallow/], [path.join(dir, "missing"), /inaccessible/], [full, /fewer than 1,000/]] as const) {
      const precondition = sourcePrecondition(source);
      assert.equal(precondition.ok, false);
      if (precondition.ok) continue;
      assert.match(precondition.reason, expected);
      const evaluation = evaluate({ unrun: precondition.reason, limits: {}, elapsedLimitMs: SLOT_ELAPSED_LIMIT_MS, elapsedMs: 0, phases: [] });
      assert.equal(evaluation.verdict, "UNRUN");
      assert.equal(evaluation.reason, precondition.reason);
    }
  });

  it("control: a run that suppresses production bundle calls is never eligible, and with no eligible run the C2 gate is UNRUN", () => {
    const e = eligibility({ real: true, origin: "repo" });
    assert.equal(e.eligible, false);
    assert.ok(e.reasons.includes("pre-slot production bundle path suppressed by the harness"), e.reasons.join());
    const lines = [{ kind: "composed", label: "a", verdict: "PASS", eligible_for_gate: false }, { kind: "composed", label: "b", verdict: "FAIL", eligible_for_gate: false }];
    assert.equal(gateSummary(lines).verdict, "UNRUN");
    assert.equal(gateSummary([...lines, { kind: "composed", label: "c", verdict: "FAIL", eligible_for_gate: true }]).verdict, "FAIL");
    assert.equal(gateSummary([{ kind: "composed", label: "c", verdict: "PASS", eligible_for_gate: true }]).verdict, "PASS");
  });

  it("control: a real orphaned process makes the sole-execution check refuse, in the precondition and in the exclusion", { skip: LINUX_ONLY }, async t => {
    const orphan = Number(execFileSync("sh", ["-c", "sleep 60 >/dev/null 2>&1 & echo $!"], { encoding: "utf8" }).trim());
    t.after(() => { try { process.kill(orphan); } catch { /* gone */ } });
    // The shell exited, so the sleep is no descendant of this process: a foreign writer in the container.
    assert.ok(foreignPids(readParents(), process.pid).includes(orphan), "the orphan is reported foreign");
    assert.match(soleExecutionProblem() ?? "", /other than the measurement|not PID 1/);
    // The precondition is wired into runPipeline before any prerequisite or fixture work. The ancestor
    // probe is pointed at "/" so the (earlier) ancestor check passes wherever TMPDIR lives.
    testHooks.ancestorProbe = "/";
    t.after(() => { testHooks.ancestorProbe = undefined; });
    const { evaluation, record } = await runPipeline(t, { label: "control-sole", layout: "shared", shape: "A", origin: "tiny", real: true });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /sole execution is unverified/);
  });

  it("control: with a clean process tree the real exclusion's idleness follows the runner's execution tails", async () => {
    const clean = () => new Map([[1, 0], [process.pid, 1]]); // only this process: ancestry and foreign checks pass
    const own = realExclusion(() => ({ executionTails: new Map([["this-run", 1]]) }), "this-run", "http://unused.invalid", clean);
    const allowed = await own.run(async () => "ran", () => true);
    assert.notDeepEqual(allowed, { entered: false, reason: "worker_not_idle" }, "the run's own tail is idle: the exclusion goes on to its proof");
    const foreign = realExclusion(() => ({ executionTails: new Map([["other-run", 1]]) }), "this-run", "http://unused.invalid", clean);
    assert.deepEqual(await foreign.run(async () => "ran", () => true), { entered: false, reason: "worker_not_idle" });
    assert.equal(soleExecutionProblem({ executionTails: new Map() }, "this-run", clean), undefined);
  });

  it("control: the measurement must be PID 1 or a child of PID 1 through at most the test runner", () => {
    assert.equal(ancestryProblem(new Map([[1, 0], [20, 1]]), 20), undefined);
    assert.equal(ancestryProblem(new Map([[1, 0], [10, 1], [20, 10]]), 20), undefined);
    assert.equal(ancestryProblem(new Map([[1, 0]]), 1), undefined);
    assert.match(ancestryProblem(new Map([[1, 0], [5, 1], [10, 5], [20, 10]]), 20) ?? "", /not PID 1/);
  });

  it("control: the sole-execution precondition refuses a foreign process and a foreign runner tail", () => {
    const tree = new Map([[1, 0], [10, 1], [20, 10], [30, 20]]); // init, parent, self, own child
    assert.deepEqual(foreignPids(tree, 20), []);
    assert.deepEqual(foreignPids(new Map([...tree, [40, 1]]), 20), [40]);
    assert.deepEqual(foreignPids(new Map([...tree, [40, 30], [50, 40]]), 20), [], "descendants of descendants are the measurement's own");
    assert.match(runnerTailProblem({ executionTails: new Map([["other-run", 1]]) }, "this-run") ?? "", /another run/);
    assert.equal(runnerTailProblem({ executionTails: new Map([["this-run", 1]]) }, "this-run"), undefined);
  });

  it("control: a deviation never turns an incomplete pipeline or a refusal into a counters-within-limits verdict", () => {
    const clean = [phase("walk", { sourceEntries: 1 })];
    assert.equal(evaluate({ ...base, phases: clean, incomplete: "the pipeline ended without an archive outcome", deviation: "emulated" }).verdict, "FAIL");
    assert.equal(evaluate({ ...base, phases: clean, incomplete: "the pipeline ended without an archive outcome" }).verdict, "FAIL");
    assert.equal(evaluate({ ...base, phases: clean, refusal: { phase: "walk", reason: "maintenance_source_layout_unsupported" }, deviation: "emulated" }).verdict, "FAIL");
    assert.equal(evaluate({ ...base, phases: clean, envelopeOverruns: [{ dimension: "familyProof.metadataBytes", phase: "w0_p0", used: 2, limit: 1 }] }).verdict, "FAIL");
    assert.equal(evaluate({ ...base, phases: clean, envelopeOverruns: [{ dimension: "familyProof.adminEntries", phase: "walk", used: 2, limit: 1 }], deviation: "emulated" }).verdict, "FAIL");
  });

  it("control: the real-path preconditions report UNRUN without a daemon and for an untrusted ancestor", async t => {
    const saved = process.env.UZI_MEASURE_DOCKER_HOST;
    delete process.env.UZI_MEASURE_DOCKER_HOST;
    t.after(() => { if (saved !== undefined) process.env.UZI_MEASURE_DOCKER_HOST = saved; });
    const noDaemon = realPrerequisites();
    assert.ok("unrun" in noDaemon && /UZI_MEASURE_DOCKER_HOST is unset/.test(noDaemon.unrun));
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "measure-3031-ancestor-"));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    await fs.chmod(dir, 0o777);
    assert.match(ancestorProblem(path.join(dir)) ?? "", /not a trusted ancestor/);
  });

  it("control: a real spec without control fields runs the source precondition (acceptance 2 on the real path)", async t => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "measure-3031-notrepo-"));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    testHooks.sourceRoot = dir;
    t.after(() => { testHooks.sourceRoot = undefined; });
    const { evaluation, record } = await runPipeline(t, { label: "control-real-source", layout: "shared", shape: "A", origin: "repo", real: true });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /source repository checkout is inaccessible/);
    assert.equal(record.w0_invocations, undefined);
  });

  it("control: a real spec with an untrusted ancestor of the data directory is UNRUN before anything else", async t => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "measure-3031-ancestor-wired-"));
    t.after(() => fs.rm(dir, { recursive: true, force: true }));
    await fs.chmod(dir, 0o777);
    testHooks.ancestorProbe = dir;
    t.after(() => { testHooks.ancestorProbe = undefined; });
    const { evaluation, record } = await runPipeline(t, { label: "control-real-ancestor", layout: "shared", shape: "A", origin: "tiny", real: true });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /not a trusted ancestor/);
  });

  it("control: runPipeline returns UNRUN for a failing source precondition and runs nothing else", async t => {
    const { record, evaluation } = await runPipeline(t, { label: "control-source", layout: "shared", shape: "A", origin: "repo", real: false, sourceRoot: path.join(os.tmpdir(), "measure-3031-no-such-source") });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /inaccessible/);
    assert.equal(record.w0_invocations, undefined);
  });

  it("control: a real measurement spec with a control-only field is refused and is never eligible", async t => {
    const { evaluation, record } = await runPipeline(t, { label: "control-guard", layout: "shared", shape: "A", origin: "tiny", real: true, controlArchive: "skip" });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /control-only field\(s\): controlArchive/);
    for (const spec of [{ sourceRoot: "/x" }, { controlArchive: "skip" as const }, { controlLimits: (l: Limits) => l }, { decodedCap: 1 }]) {
      const e = eligibility({ real: true, origin: "repo", ...spec });
      assert.ok(e.reasons.some(r => r.startsWith("control-only field(s) set")), JSON.stringify(Object.keys(spec)));
    }
  });

  it("control: the two-source family variant is UNRUN with the reachability reason and is never eligible for the gate", async t => {
    const { record, evaluation } = await runPipeline(t, { label: "control-family", layout: "shared", shape: "A", family: true, origin: "repo", real: true });
    assert.equal(evaluation.verdict, "UNRUN");
    assert.match(String(record.reason), /unreachable through production code/);
    assert.equal(record.eligible_for_gate, undefined, "an UNRUN family line carries no eligibility");
    assert.equal(record.w0_invocations, undefined, "nothing else was run or asserted");
  });

  const small = (label: string, decodedCap?: number): Spec => ({ label, layout: "dissociated", shape: "control", origin: "tiny", real: false, decodedCap });

  it("control 9 (pipeline): one cap lowered through the decoded-limit seam fails naming that dimension and a phase", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const { record, evaluation } = await runPipeline(t, small("control-lowered-cap", 1));
    assert.equal(record.eligible_for_gate, false, "a control run never qualifies for the gate");
    assert.ok((record.ineligible_reasons as string[]).includes("pre-slot production bundle path suppressed by the harness"));
    assert.equal(evaluation.verdict, "FAIL", JSON.stringify(record));
    const decoded = evaluation.violations.find(v => v.dimension === "decoded");
    assert.ok(decoded, JSON.stringify(evaluation));
    assert.ok((PHASES as readonly string[]).includes(decoded.phase), decoded.phase);
    assert.equal(decoded.limit, 1);
  });

  it("control 9 (pipeline): a limit that only the archive phase crosses fails naming that dimension and the archive phase", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const before = (l: Limits, phases: ReadonlyArray<{ name: string; used: Counters }>) => ({ ...l,
      artifactBytes: phases.filter(p => PHASES.indexOf(p.name as Phase) < PHASES.indexOf("archive")).reduce((n, p) => n + (p.used.artifactBytes ?? 0), 0) });
    const { evaluation, record } = await runPipeline(t, { ...small("control-archive-cap"), controlLimits: (l, ph) => ({ ...before(l, ph), artifactBytes: before(l, ph).artifactBytes + 1 }) });
    assert.ok((record.phases as Record<string, Counters>).archive?.artifactBytes, "the archive phase charged artifactBytes");
    assert.equal(evaluation.verdict, "FAIL", JSON.stringify(record));
    assert.deepEqual(evaluation.violations.map(v => [v.dimension, v.phase]), [["artifactBytes", "archive"]]);
  });

  it("control (pipeline): an arbitrary archive failure is FAIL, never relabelled UNRUN", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const { evaluation, record } = await runPipeline(t, { ...small("control-archive-fault"), controlArchive: new Error("archive exploded") });
    assert.equal(evaluation.verdict, "FAIL", JSON.stringify(record));
    assert.equal(evaluation.refusal?.phase, "archive");
  });

  it("control (pipeline): an owed-bare refusal of the archive step is UNRUN with the precondition reason", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const { evaluation, record } = await runPipeline(t, { ...small("control-archive-owed-bare"), controlArchive: new Error("unsafe worker bare directory") });
    assert.equal(evaluation.verdict, "UNRUN", JSON.stringify(record));
    assert.match(String(record.reason), /archive precondition/);
  });

  it("control (pipeline): a pipeline that ends without an archive outcome is FAIL, never PASS", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const { evaluation, record } = await runPipeline(t, { ...small("control-archive-none"), controlArchive: "skip" });
    assert.equal(evaluation.verdict, "FAIL", JSON.stringify(record));
    assert.equal(evaluation.reason, "the pipeline ended without an archive outcome");
  });

  it("control 10 (pipeline): with a lowered configured C the decoded limit follows the admission", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const cap = 123 * 1024 ** 2;
    const { record, evaluation } = await runPipeline(t, small("control-admission-limit", cap));
    assert.equal((record.limits as Limits).decoded, cap, "the MEASURE limit is the admitted decodedCap");
    assert.equal(record.configured_decoded_cap, cap);
    // Within every limit and complete; PASS, or the deviation label when UZI_MEASURE_DEVIATION is set.
    assert.ok(within(evaluation.verdict) && evaluation.violations.length === 0 && !evaluation.refusal, JSON.stringify(record));
    assert.notEqual((record.archive as { kind: string }).kind, "none", "the control pipeline reached the archive phase");
    // B3: the whole MEASURE limits object equals the slot reservation persisted by the ledger, read here
    // independently of the harness; no dimension may carry a handwritten limit.
    const table = JSON.parse(await fs.readFile(path.join(path.dirname(git.recoveryRoot), "recovery-maintenance", "complete.json"), "utf8")).value;
    const cycle = table.cycles.find((c: { identity: { run: string } }) => c.identity.run === record.run_id);
    assert.deepEqual(record.limits, cycle.slots[cycle.slots.length - 1].reserved);
    assert.ok(Object.keys(record.limits as Limits).includes("rpcAttempts"), "every key of the slot quota is a checked dimension");
    assert.match(String((record.substitutions as string[]).find(x => x.startsWith("produceRecoveryBundle"))), /RecoveryBundleTooLargeError/);
  });
});
