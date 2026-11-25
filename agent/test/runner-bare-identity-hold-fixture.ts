// Issue #4627 (unit U3, tests only): the shared rig of the WHOLE-CLAIM-EXIT tests of a held bare storage identity change.
// Production RunRunner.execute() over a real bare + real runner clone (a retained claim), a durable AuthorityRuntime, a real
// planted predecessor-attributed child process and, per harness, a Claude-shaped executor (no `safety`) or a Codex-shaped one
// (the real facade of runner-codex-sinks-fixture.ts). The bare's storage identity is moved through the
// `recoveryOwnerBareIdentity` seam (as runner-recovery-owner-realgit.test.ts does), so the first reservation of the claim
// throws the typed error and the runner holds the run for its owner.
//
// The rig RECORDS everything a claim exit could touch (the clone, HOME with a seeded `.npm/_cacache` blob and a Go work leaf,
// the cache dirs, the provision dir, the authority slots, the bare's whole config and refs, the uzi-owed and refusal files) so a
// test can assert that nothing moved. Test files call installHarness() themselves and cleanupHold() from afterEach.
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { mock, type TestContext } from "node:test";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { reapRunProcesses } from "../src/run-procs.js";
import { setQuiescenceViewForTests, type QuiesceRunRequest, quiesceRunAttempt } from "../src/run-quiescence.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { testLockOptions } from "./authority-test-support.js";
import { scopedRealView } from "./fake-proc.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { codexRig, type CodexRig } from "./runner-codex-sinks-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, runnerWith } from "./runner-harness.js";
import { HERMETIC_VIEW } from "./setup/hermetic-proc.js";
import { treeDigest } from "./restore-route-rig.js";

export const IID = 4627;
export const HOLD_FEATURES = ["recovery_cause_retained_recovery_adoption_failed", "recovery_cause_retained_recovery_exhausted"];
export const GUARDED_FEATURES = [...HOLD_FEATURES, "recovery_inventory_v1", "claim_generation_fence"];
export const GUIDANCE = "storage identity changed";
export const AUTH_TOKEN = "unit-join-" + randomBytes(12).toString("hex");
export const GIT_ENV: NodeJS.ProcessEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
export const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
export const CLAIM_GENERATION = 3;
/** The generation the predecessor's tracking head was produced at. */
export const PREDECESSOR_GENERATION = 2;

export type Harness = "claude" | "codex";
/** `confirmed`: the owner hold is acknowledged; `statusless` / `lost`: it is not (an uncertain park). */
export type Ack = "confirmed" | "statusless" | "lost";

export const gitIn = (dir: string, ...args: string[]): string => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: "pipe", env: GIT_ENV }).trim();

/** Set (to the reason) where alive() cannot read the procfs stat file; the hold test files skip their describes with it. */
export const PROCFS_SKIP: string | false = process.platform !== "linux" || !fs.existsSync("/proc/self/stat")
  ? "requires Linux procfs (/proc/<pid>/stat) for alive()" : false;
/** The `{ skip }` option the hold test files pass to every top-level describe. */
export const procfs = { skip: PROCFS_SKIP };

/** True while the process exists and is not a zombie. */
export function alive(pid: number): boolean {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3) !== "Z";
  } catch { return false; }
}

const planted: number[] = [];

/** A process that is NOT a child of this worker (the real reaper never kills a worker's direct child), carrying `home` and
 *  working in `cwd`, so the run-procs scan attributes it to the run exactly as it would a predecessor's leftover. */
export async function plantChild(home: string, cwd: string): Promise<number> {
  const sh = spawn("sh", ["-c", "sleep 600 >/dev/null 2>&1 & echo $!"], { cwd, env: { PATH: process.env.PATH ?? "", HOME: home }, stdio: ["ignore", "pipe", "ignore"] });
  let out = "";
  sh.stdout.on("data", d => { out += String(d); });
  await new Promise(resolve => sh.on("close", resolve));
  const pid = Number(out.trim());
  assert.ok(Number.isSafeInteger(pid) && pid > 1, `planted pid ${out}`);
  planted.push(pid);
  return pid;
}

/** Per-test cleanup: kill every planted child and put the hermetic quiescence view back. */
export function cleanupHold(): void {
  mock.restoreAll();
  setQuiescenceViewForTests(HERMETIC_VIEW);
  for (const pid of planted.splice(0)) { try { process.kill(pid, "SIGKILL"); } catch { /* already gone */ } }
}

export const dirSnapshot = (dir: string): string => !fs.existsSync(dir) ? "[]" : JSON.stringify(fs.readdirSync(dir).sort().map(n => {
  const full = path.join(dir, n);
  return [n, fs.statSync(full).isFile() ? fs.readFileSync(full, "utf8") : "<dir>"];
}));

export interface Snapshot {
  /** `git config --local --list`: the journal, owner record, attempt mode and `uzi-attempts.*` live here. */
  config: string;
  refs: string;
  /** Names and bytes of `<bare>/uzi-owed` and `<bare>/info/uzi-refusals`. */
  owed: string;
  refusals: string;
  owedNames: string[];
  clone: string;
  home: string;
  provision: string;
  /** The skills plugin dir beside the clone (a cache dir the teardown could remove). */
  skills: string;
  /** The seeded rebuildable caches and the Go work leaf, digested on their own. */
  caches: string;
  slot: SlotSnapshot | null;
  bareSlot: SlotSnapshot | null;
  /** The journal, owner record, attempt mode and attempt ledger, each read on its own. */
  journal: string;
  owner: string;
  attemptMode: string;
  attempts: string;
}

export interface HoldOptions {
  harness: Harness;
  ack: Ack;
  /** The claim says its inventory is guarded (a code-publishing retained claim). */
  guarded?: boolean;
  claim?: Record<string, unknown>;
  /** Plant the predecessor-attributed child and a reaping executor (default true). */
  plantChild?: boolean;
  /** Runs after the predecessor's clone exists and before the reservation: builds the tracking head and its owed state. */
  tracked?: (rig: HoldRig) => Promise<void>;
  /** Do not make the default first reservation (the `tracked` hook builds its own journal). */
  skipReserve?: boolean;
  /** Codex only: close the facade's registry with one earlier boundary (its fake provider root reaped), so the terminal
   *  disposal can PROVE. A fresh facade never can (its registry is open), so a Codex flight that exits before any boundary has
   *  an unproven disposal and skips the owed retention: the default, faithful arrangement. */
  closeCodexFacade?: boolean;
  /** `unretained` replaces the write-free owed-tip probe with an always-"unretained" answer, so the exit runs today's
   *  retainCurrentOwedCandidate unconditionally: the BASE outcome a variant is compared against. */
  probeMode?: "real" | "unretained";
  /** Arm one permanent (403) message-batch refusal inside the hold, right after the hold flag is set and before its guidance
   *  flush, so the batcher's breaker trips and the guidance is unconfirmed. */
  failGuidanceFlush?: boolean;
  /** Runs immediately before the claim executes (the mismatch is armed right after it). */
  beforeExecute?: (rig: HoldRig) => Promise<void> | void;
}

export interface HoldRig {
  claim: ClaimResponse;
  harness: Harness;
  bare: string;
  clonePath: string;
  branch: string;
  key: string;
  runId: string;
  home: string;
  provision: string;
  skills: string;
  child: number | undefined;
  authority: AuthorityRuntime;
  codex: CodexRig | undefined;
  registry: ActiveRunRegistry;
  logs: ReturnType<typeof recordingLogger>;
  /** Park bodies the runner sent, in order. */
  parks: Array<Record<string, unknown>>;
  sequence: string[];
  flights: HoldFlight[];
  quiesce: QuiesceRunRequest[];
  counts: {
    holds: number; models: number; reaps: number; kills: number; ledger: number; captures: number; blocks: number; reserves: number;
    freeCaptures: number; retainCalls: number; retainResults: string[]; probeResults: string[]; reclaims: number;
  };
  rejections: unknown[];
  mismatch: { on: boolean };
  runner: ReturnType<typeof runnerWith>;
  /** The factory calls the runner made for this claim. */
  executeClaim(): Promise<void>;
  snapshot(): Promise<Snapshot>;
  owedDir(): string;
  refusalDir(): string;
}

export interface HoldFlight {
  parked?: boolean; bareIdentityHold?: boolean; preserveRecoveryClone?: boolean; preserveSession?: boolean;
  retainedLocalCustody?: boolean; sourceJoinFailed?: boolean; sourceCleanupJoined?: boolean; keepGuardedInventoryOpen?: boolean; claimGeneration: number; runId: string;
  lastCheckpointRefTip?: string;
  steering: { pendingCredentialSwitch(): number | undefined };
}

/** The seeded HOME: rebuildable caches (a `.npm/_cacache` blob, a Go build cache, a module cache) and a managed Go work leaf. */
function seedHome(home: string): void {
  const files: Array<[string, string]> = [
    [".npm/_cacache/index-v5/entry", "{}"],
    [".npm/_cacache/content-v2/sha512/ab/blob", "cached bytes"],
    [".cache/go-build/0a/0a1b-d", "x".repeat(2048)],
    ["go/pkg/mod/example.com/m@v1.0.0/m.go", "package m\n"],
    [`.uzi-go-work/g${CLAIM_GENERATION}/work`, "go scratch\n"],
    [".claude/projects/-clone/session.jsonl", '{"type":"user"}\n'],
    ["session-proof", "original session"],
  ];
  for (const [rel, body] of files) {
    fs.mkdirSync(path.dirname(path.join(home, rel)), { recursive: true });
    fs.writeFileSync(path.join(home, rel), body);
  }
}
const CACHE_RELS = [".npm/_cacache", ".cache/go-build", "go/pkg/mod", `.uzi-go-work/g${CLAIM_GENERATION}`];

/** The commit-tree helper of the owed tests: a root-like commit on the clone's branch from the default tip's tree. */
let seq = 0;
export function commitOnBranch(rig: Pick<HoldRig, "clonePath" | "branch" | "bare">, label: string, parents?: string[]): string {
  const base = gitIn(rig.bare, "rev-parse", "refs/remotes/origin/main");
  const tree = gitIn(rig.clonePath, "rev-parse", `${base}^{tree}`);
  const ps = (parents ?? [base]).flatMap(p => ["-p", p]);
  const sha = gitIn(rig.clonePath, ...IDENT, "commit-tree", tree, ...ps, "-m", `${label} ${seq++}`);
  gitIn(rig.clonePath, "update-ref", `refs/heads/${rig.branch}`, sha);
  return sha;
}

export async function buildHold(t: TestContext, o: HoldOptions): Promise<HoldRig> {
  const claim = gitlabClaim(IID, {
    claim_generation: CLAIM_GENERATION, worker_recovery_episode: 7, worker_recovery_owner_episode: 0,
    ...(o.guarded ? { inventory_guarded: true } : {}), ...o.claim,
  }) as ClaimResponse;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, IID, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "local retained work\n");
  const key = `issue-${IID}`;
  const source = { runId: claim.run_id, clonePath: clone.path };

  const home = path.join(homeDir, claim.run_id);
  const provision = path.join(fx.dataDir, "provision", claim.run_id);
  const skills = skillsPluginDir(clone.path);
  seedHome(home);
  fs.mkdirSync(provision, { recursive: true });
  fs.writeFileSync(path.join(provision, "marker"), "provision bytes");
  fs.mkdirSync(skills, { recursive: true });
  fs.writeFileSync(path.join(skills, "plugin.json"), "{}");

  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
  const logs = recordingLogger();
  const rig: HoldRig = {
    claim, harness: o.harness, bare, clonePath: clone.path, branch: clone.branch, key, runId: claim.run_id, home, provision, skills,
    child: undefined, authority, codex: undefined, registry: new ActiveRunRegistry(), logs, parks: [], sequence: [], flights: [], quiesce: [],
    counts: { holds: 0, models: 0, reaps: 0, kills: 0, ledger: 0, captures: 0, blocks: 0, reserves: 0, freeCaptures: 0, retainCalls: 0,
      retainResults: [], probeResults: [], reclaims: 0 },
    rejections: [], mismatch: { on: false }, runner: undefined as never,
    executeClaim: async () => undefined,
    owedDir: () => path.join(bare, "uzi-owed"),
    refusalDir: () => path.join(bare, "info", "uzi-refusals"),
    snapshot: async () => undefined as never,
  };

  if (o.tracked) await o.tracked(rig);
  if (!o.skipReserve) await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 7, 0);

  client.protocolFeatures = o.guarded ? GUARDED_FEATURES : HOLD_FEATURES;
  const doPlant = o.plantChild !== false;
  if (doPlant) {
    rig.child = await plantChild(home, clone.path);
    setQuiescenceViewForTests(scopedRealView({ pids: [rig.child] }));
  }
  if (o.harness === "codex") {
    rig.codex = codexRig();
    if (o.closeCodexFacade) await rig.codex.safety.withBoundary({ boundary: "park", deadlineMs: 2000 }, async () => undefined);
  }

  // The identity change: the bare's device number moves by one, as a remounted volume's would.
  const seam = git as unknown as Record<string, (...a: unknown[]) => Promise<string>>;
  const identity = JSON.parse(await seam.recoveryOwnerBareIdentity!(bare)) as string[];
  const real = seam.recoveryOwnerBareIdentity!.bind(git);
  t.mock.method(seam, "recoveryOwnerBareIdentity", async (...args: unknown[]) => {
    if (!rig.mismatch.on) return real(...args);
    const shifted = [...identity];
    shifted[1] = String(BigInt(identity[1]!) + 1n);
    return JSON.stringify(shifted);
  });
  const reserve = git.reserveRecoveryIteration.bind(git);
  t.mock.method(git, "reserveRecoveryIteration", async (...args: Parameters<typeof reserve>) => {
    rig.counts.reserves++;
    try { return await reserve(...args); } catch (error) { rig.rejections.push(error); throw error; }
  });
  const capture = git.recordRecoveryCapture.bind(git);
  t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => { rig.counts.captures++; return capture(...args); });
  const block = git.blockRecoveryEpisode.bind(git);
  t.mock.method(git, "blockRecoveryEpisode", async (...args: Parameters<typeof block>) => { rig.counts.blocks++; return block(...args); });
  const ledger = seam.appendAttemptLedger!.bind(git);
  t.mock.method(seam, "appendAttemptLedger", async (...args: unknown[]) => { rig.counts.ledger++; return ledger(...args); });
  // Observed, never replaced: the retention a claim exit performs.
  const retain = git.retainCurrentOwedCandidate.bind(git);
  t.mock.method(git, "retainCurrentOwedCandidate", async (...args: Parameters<typeof retain>) => {
    rig.counts.retainCalls++;
    const result = await retain(...args);
    rig.counts.retainResults.push(JSON.stringify(result));
    return result;
  });
  if (typeof (git as unknown as { owedTipRetained?: unknown }).owedTipRetained === "function") {
    const probe = (git as unknown as { owedTipRetained: (...a: unknown[]) => Promise<{ kind: string }> }).owedTipRetained.bind(git);
    t.mock.method(git as unknown as Record<string, (...a: unknown[]) => Promise<{ kind: string }>>, "owedTipRetained", async (...args: unknown[]) => {
      const result = o.probeMode === "unretained" ? { kind: "unretained" } : await probe(...args);
      rig.counts.probeResults.push(JSON.stringify(result));
      return result;
    });
  }

  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (runId: Parameters<typeof report>[0], body: Parameters<typeof report>[1], signal?: AbortSignal) => {
    const isPark = body.status === "recovery_wait" && typeof body.recovery_cause === "string";
    rig.sequence.push(isPark ? `${body.status}:${body.recovery_cause}` : String(body.status));
    if (!isPark) return report(runId, body, signal);
    rig.parks.push(body as unknown as Record<string, unknown>);
    if (o.ack === "lost") throw new Error("connection reset");
    if (o.ack === "statusless") return {} as never;
    return { status: body.status, recoveryCause: body.recovery_cause } as never;
  });

  const home0 = home;
  const reaping = (): Partial<Executor> => ({
    reclaimCachesInPlace: async () => { rig.counts.reclaims++; return { dropped: [], absent: [], failed: [], skipped: [] }; },
    killAgentTree: () => { rig.counts.kills++; },
    reapAttributedProcesses: async opts => { rig.counts.reaps++; return reapRunProcesses(home0, undefined, [], opts); },
  });
  const run: Executor["run"] = async () => { rig.counts.models++; throw new Error("must not model"); };
  const makeExecutor = (): Executor => o.harness === "codex"
    ? { sandboxesCommands: true, safety: rig.codex!.safety, settleForCredentialFreeCapture: rig.codex!.settle,
      withCredentialFreeCapture: rig.codex!.capture, run, ...reaping() } as Executor
    : { run, ...reaping() } as Executor;
  rig.runner = runnerWith(() => ({ homeDir: home, executor: makeExecutor() }), fakeGitlab().gitlab, AUTH_TOKEN, logs.logger, {
    recoveryRetryMs: 1, activeRuns: rig.registry, authority,
    quiesceRun: async req => { rig.quiesce.push(req); return quiesceRunAttempt(req); },
  });
  const priv = rig.runner as unknown as Record<string, (...a: unknown[]) => unknown>;
  if (typeof priv.holdForBareIdentityChange === "function") {
    const hold = priv.holdForBareIdentityChange.bind(rig.runner);
    t.mock.method(priv, "holdForBareIdentityChange", (...args: unknown[]) => { rig.counts.holds++; rig.flights.push(args[1] as HoldFlight); if (o.failGuidanceFlush) api.failMessagesNext(1, 403); return hold(...args); });
  }
  const free = priv.runFreeCapture!.bind(rig.runner);
  t.mock.method(priv, "runFreeCapture", (...args: unknown[]) => { rig.counts.freeCaptures++; return free(...args); });

  const readConfig = (key2: string): string => { try { return gitIn(bare, "config", "--local", "--get-all", key2); } catch { return ""; } };
  const regexp = (pattern: string): string => { try { return gitIn(bare, "config", "--local", "--get-regexp", pattern); } catch { return ""; } };
  rig.snapshot = async () => ({
    config: gitIn(bare, "config", "--local", "--list"),
    refs: gitIn(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    owed: dirSnapshot(rig.owedDir()), refusals: dirSnapshot(rig.refusalDir()),
    owedNames: fs.existsSync(rig.owedDir()) ? fs.readdirSync(rig.owedDir()).sort() : [],
    clone: treeDigest(clone.path), home: treeDigest(home), provision: treeDigest(provision), skills: treeDigest(skills),
    caches: CACHE_RELS.map(rel => `${rel}=${fs.existsSync(path.join(home, rel)) ? treeDigest(path.join(home, rel)) : "absent"}`).join("\n"),
    slot: await authority.peekSlot(clone.path), bareSlot: await authority.peekSlot(bare),
    journal: readConfig(`uzi-recovery.${clone.branch}.clone`),
    owner: readConfig(`uzi-recoveryowner.${claim.run_id}.record`),
    attemptMode: regexp("^uzi-attemptmode\\."),
    attempts: regexp("^uzi-attempts\\."),
  });
  rig.executeClaim = async () => {
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation!);
    await o.beforeExecute?.(rig);
    rig.mismatch.on = true;
    await rig.runner.execute(claim);
  };
  return rig;
}
