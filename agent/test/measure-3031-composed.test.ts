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
// Harness facts that are not substitutions of a production check, also printed: the Worker object
// does not exist here, so `isIdle` is true by construction (the measurement is the only runner).
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

interface Violation { dimension: string; phase: string; used?: number; limit: number; refused?: boolean }
interface EvaluationInput {
  limits: Limits;
  elapsedLimitMs: number;
  elapsedMs: number;
  phases: ReadonlyArray<{ name: string; used: Counters }>;
  refusedCharge?: { resource: string; amount: number; used: number; phase: string };
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
      if (limit !== undefined && total > limit && !named.has(dimension)) {
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
  if (violations.length > 0 || input.refusal || input.incomplete !== undefined) {
    out.verdict = "FAIL";
    if (input.incomplete !== undefined) out.reason = input.incomplete;
  } else if (input.deviation) out.verdict = "counters_within_limits_elapsed_unverified";
  return out;
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

interface RunResult { record: Record<string, unknown>; evaluation: Evaluation }

async function runPipeline(t: TestContext, spec: Spec): Promise<RunResult> {
  const env = environment();
  const head: Record<string, unknown> = { kind: "composed", label: spec.label, layout: spec.layout, shape: spec.shape,
    family: spec.family ? "two-source" : "single-source", ...env };
  const finish = (evaluation: Evaluation, body: Record<string, unknown> = {}): RunResult =>
    ({ record: emit({ ...head, ...body, verdict: evaluation.verdict, verdict_detail: evaluation }), evaluation });
  const unrun = (reason: string, body: Record<string, unknown> = {}) =>
    finish(evaluate({ unrun: reason, limits: {}, elapsedLimitMs: SLOT_ELAPSED_LIMIT_MS, elapsedMs: 0, phases: [], deviation: env.deviation }), { ...body, reason });

  // ---- preconditions: UNRUN with the reason, nothing else asserted
  let source: { head: string; commits: number } | undefined;
  if (spec.origin === "repo") {
    const precondition = sourcePrecondition(REPO_ROOT);
    if (!precondition.ok) return unrun(precondition.reason);
    source = precondition;
  }
  let dockerHost = "http://unused.invalid";
  if (spec.real) {
    const prerequisites = realPrerequisites();
    if ("unrun" in prerequisites) return unrun(prerequisites.unrun);
    dockerHost = prerequisites.dockerHost;
  }
  if (spec.real) {
    const ancestors = ancestorProblem(path.dirname(git.recoveryRoot));
    if (ancestors) return unrun(ancestors);
  } else projectTrustedAncestors(t);
  head.candidate_sha = source?.head;
  head.source_commits = source?.commits;

  if (spec.real) {
    setQuiescenceViewForTests(undefined); // the real /proc, not the hermetic empty view (restored in afterEach)
  }
  if (spec.decodedCap !== undefined) Object.defineProperty(git, "recoveryDecodedLimitBytes", { value: spec.decodedCap });

  // ---- origin: a clone of this repository's checkout (item 2); the runner makes the bare (item 4)
  if (source) {
    const origin = path.join(path.dirname(fx.dataDir), "origin-src");
    execFileSync("git", ["clone", "-q", "--no-local", "--no-hardlinks", REPO_ROOT, origin], { env: gitEnv, stdio: "pipe" });
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
      refusedCharge ??= { resource, amount, used: totals[resource] ?? 0, phase };
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
  const stray: Array<{ phase: string; inSlot: boolean }> = [];
  t.mock.method(git, "produceRecoveryBundle", async (...args: Parameters<typeof produce>) => {
    if (archiveWindow) return produce(...args);
    stray.push({ phase, inSlot: MaintenanceResources.active() && !MaintenanceResources.discoveryActive() });
    throw new Error("fixture bundle production unavailable outside the measured archive phase");
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
  const exclusion = spec.real
    ? new MaintenanceWriterExclusion({ gate: new DindPruneGate(), isIdle: () => true,
        proof: scans => terminalMaintenanceProof(scans, dockerHost), log: nullLogger() })
    : passingExclusion();

  // ---- the runner: a failed run leaves the WIP shape in its attempt clone
  let genesis = "", baseCommit = "", sibling: { path: string; attemptId: string } | undefined;
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

  // The coordinator is handed the observation the runner produced; a two-source family extends the
  // INPUT list of that observation with a real sibling clone and lets the real family scan compute
  // identities and metadata (the normal-failed path itself only ever lists the genesis).
  let siblingBefore: string | undefined;
  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  let notified: Promise<void> | undefined;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator, ...args: Parameters<typeof resume>) {
    if (spec.family) args[1] = Promise.resolve(args[1]).then(async observation => {
      const view = observation as unknown as { genesis: { clonePath: string; attemptId: string; generation: number; runId: string }; family?: { originals: Array<Record<string, unknown>>; metadata: string; observedExecution: unknown } } | undefined;
      if (!view?.family) return observation;
      const coordinates = (runner as unknown as { cloneCoordinates(c: typeof claim): { key: string; branch: string } }).cloneCoordinates(claim);
      const attemptId = view.genesis.attemptId.replace(/^\d{8}T\d{6}Z/, "20000101T000000Z").replace(/-[0-9a-f]{16}$/, "-" + "5".repeat(16));
      const dir = path.join(path.dirname(view.genesis.clonePath), path.basename(view.genesis.clonePath).replace(view.genesis.attemptId, attemptId));
      execFileSync("git", ["clone", "-q", "--shared", "--no-checkout", "--", bare, dir], { env: gitEnv, stdio: "pipe" });
      disableAutoMaintenance(dir, gitEnv);
      sh(dir, "checkout", "-q", "--detach", baseCommit);
      if (spec.layout === "dissociated") await (git as unknown as { materializeRunnerClone(c: string, r: string[]): Promise<void> }).materializeRunnerClone(dir, [sh(dir, "rev-parse", "HEAD").trim()]);
      await applyShape(dir, "B", undefined); // unique uncommitted work held only by the sibling
      await fs.writeFile(path.join(dir, "sibling-unique-untracked.txt"), "work only the sibling holds\n");
      sh(bare, "config", "--add", `uzi-attempts.${coordinates.branch}.entry`,
        JSON.stringify({ attemptId, runId: claim.run_id, clonePath: dir, state: "abandoned" }));
      sibling = { path: dir, attemptId };
      siblingBefore = siblingState(dir);
      fixtures.sibling = { path: dir, count_objects: countObjects(dir) };
      const originals = [...view.family.originals, { ...view.family.originals[0]!, clonePath: dir, attemptId, identity: undefined }];
      const scan = await git.matchMaintenanceSourceFamily(bare, coordinates.branch, coordinates.key, claim.run_id, originals as never);
      if (!scan) return undefined; // the real scan refused the two-source family: no admission, reported below
      return { ...view, family: { ...view.family, metadata: scan.metadata, originals: scan.sources.map(s => ({
        ...originals.find(o => o.clonePath === s.clonePath)!, identity: s.identity })) } } as never;
    });
    notified = resume.apply(this, args);
    return notified;
  });

  // ---- phase markers and the archive, all through wrappers on named GitCache / runner scopes
  let w0Invocations = 0, transferEntered = 0, importerResult: { kind: string; sourceSha: string } | undefined;
  let importerError: unknown, w0Head: string | undefined;
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
        const result = await git.produceRecoveryBundle(bare, { sourceSha: importerResult.sourceSha, outPath,
          guardedDefaultBranch: args[0].defaultBranch });
        archive = { kind: result.selfContained ? "self-contained" : "thin", bytes: result.byteSize, cap_bytes: RECOVERY_MAX_BUNDLE_BYTES,
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
          w0Head = result.head;
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
    bundle_attempts_outside_the_slot_refused: stray.filter(s => !s.inSlot).length };
  if (limitsError !== undefined || admittedCap === undefined) {
    body.admission = { error: limitsError ?? "no slot was admitted", refusals };
    const reason = refusals.length > 0 ? `admission refused before the charged slot: ${refusals.join(",")}` : `no charged slot: ${limitsError ?? "admission not reached"}`;
    return unrun(reason, body);
  }
  const phases = PHASES.map(name => ({ name, used: used[name]! }));
  const slotElapsed = Math.round((slotEnded - (slotStarted ?? started)));
  const failure = (importerError ?? archiveError) as unknown;
  let incomplete: string | undefined, refusal: { phase: string; reason: string } | undefined;
  const failurePhase = refusedCharge?.phase ?? (archiveError ? "archive" : importerError ? "transfer_entry" : refusals.length > 0 ? phase : undefined);
  if (failure !== undefined || refusals.length > 0) {
    const message = failure instanceof RecoveryBundleTooLargeError ? "RecoveryBundleTooLargeError: neither self-contained nor guarded thin bundle fits the per-bundle cap"
      : failure instanceof Error ? failure.message : refusals.join(",");
    refusal = { phase: failurePhase ?? "unknown", reason: refusals.length > 0 ? refusals.join(",") + (failure instanceof Error ? ` (${failure.message})` : "") : message };
  }
  if (w0Invocations === 0 && !refusal) incomplete = "the admission never reached the W0 capture";
  else if (!archive && !refusal) incomplete = "the pipeline ended without an archive outcome";
  const strayInSlot = stray.filter(s => s.inSlot);
  if (strayInSlot.length > 0) incomplete = `a production bundle call inside the charged slot but outside the archive phase was refused (phase ${strayInSlot.map(s => s.phase).join(",")})`;
  const archiveNotApplicable = archiveError instanceof Error && /assertOwedBare|owed bare|not a recovery bare/i.test(archiveError.message);
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
    : evaluate({ limits, elapsedLimitMs: slotMs, elapsedMs: slotElapsed, phases, refusedCharge, capRefusals, envelopeOverruns, refusal, incomplete, deviation: env.deviation });
  const family = spec.family ? {
    members: 2, w0_invocations: w0Invocations,
    per_source: {
      genesis: { role: "A", outcome: w0Head ? "captured by one W0 invocation" : "not captured", w0_head: w0Head },
      sibling: { role: "B", outcome: sibling && siblingBefore === siblingState(sibling.path) ? "protected: retained, uncaptured, bytes and refs unchanged" : "NOT protected", path_retained: !!sibling && fsSync.existsSync(sibling.path) },
    },
    preservation_only: true, whole_family_preservation: false,
    note: "genesis preservation never counts as whole-family preservation (U4); sibling coverage proof is #3440",
  } : undefined;
  const record = emit({
    ...head, ...body,
    candidate_sha: source?.head, source_commits: source?.commits,
    substitutions: ["guard-method: RecoveryCoordinator.terminalOriginAdmissionEnabled", "faked-server: strict WorkerClient over a faked worker API",
      ...(spec.real ? [] : ["CONTROL ONLY: passingExclusion", "CONTROL ONLY: stubbed quiescence verdict", "CONTROL ONLY: trusted-ancestor DAC projection"])],
    harness_facts: ["isIdle is true by construction (no Worker object in the harness)",
      `layout via production selfContained path: ${spec.layout === "dissociated" ? "real GitCache.materializeRunnerClone" : "clone --shared, alternates retained"}`],
    eligible_for_gate: spec.real && spec.origin === "repo",
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
    family: family ?? null,
    refusal: evaluation.refusal ?? null, refusals,
    verdict: evaluation.verdict, verdict_detail: evaluation,
  });
  return { record, evaluation };
}

function siblingState(dir: string): string {
  const entries = fsSync.readdirSync(dir).filter(n => n !== ".git").sort();
  return JSON.stringify([sh(dir, "rev-parse", "HEAD").trim(), sh(dir, "status", "--porcelain=v1", "-z"),
    sh(dir, "for-each-ref", "--format=%(refname) %(objectname)"),
    entries.map(n => { try { return fsSync.statSync(path.join(dir, n)).isFile() ? fsSync.readFileSync(path.join(dir, n)).toString("base64") : n; } catch { return n; } })]);
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
    it(`layout ${layout}, two-source family (genesis shape A, sibling shape B)`, { skip: MEASURE_SKIP, timeout: 120_000 }, async t => {
      await runPipeline(t, { label: `${layout}-family`, layout, shape: "A", family: true, origin: "repo", real: true });
    });
  }

  for (const loose of LOOSE_LADDER) {
    it(`loose-object ladder rung ${loose} (dissociated, shape C)`, { skip: MEASURE_SKIP, timeout: 120_000 }, async t => {
      if (ladder.some(r => !within(r.verdict))) return t.skip("an earlier rung already left the limits");
      const { evaluation } = await runPipeline(t, { label: `ladder-${loose}`, layout: "dissociated", shape: "C", loose, origin: "repo", real: true });
      ladder.push({ loose, verdict: evaluation.verdict });
    });
  }

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
    const slow = evaluate({ ...base, elapsedMs: SLOT_ELAPSED_LIMIT_MS + 1, phases: [] });
    assert.deepEqual(slow.violations.map(v => v.dimension), ["elapsedMs"]);
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
    assert.equal(sourcePrecondition(REPO_ROOT).ok === true || true, true, "the repository checkout is not required by a control");
  });

  const small = (label: string, decodedCap?: number): Spec => ({ label, layout: "dissociated", shape: "control", origin: "tiny", real: false, decodedCap });

  it("control 9 (pipeline): one cap lowered through the decoded-limit seam fails naming that dimension and a phase", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const { record, evaluation } = await runPipeline(t, small("control-lowered-cap", 1));
    assert.equal(record.eligible_for_gate, false, "a control run never qualifies for the gate");
    assert.equal(evaluation.verdict, "FAIL", JSON.stringify(record));
    const decoded = evaluation.violations.find(v => v.dimension === "decoded");
    assert.ok(decoded, JSON.stringify(evaluation));
    assert.ok((PHASES as readonly string[]).includes(decoded.phase), decoded.phase);
    assert.equal(decoded.limit, 1);
  });

  it("control 10 (pipeline): with a lowered configured C the decoded limit follows the admission", { skip: LINUX_ONLY, timeout: 120_000 }, async t => {
    const cap = 123 * 1024 ** 2;
    const { record, evaluation } = await runPipeline(t, small("control-admission-limit", cap));
    assert.equal((record.limits as Limits).decoded, cap, "the MEASURE limit is the admitted decodedCap");
    assert.equal(record.configured_decoded_cap, cap);
    assert.equal(evaluation.verdict, "PASS", JSON.stringify(record));
    assert.ok(Object.keys(record.limits as Limits).includes("rpcAttempts"), "every key of the slot quota is a checked dimension");
  });
});
