import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { formatAttemptId } from "../../src/attempt-path.js";
import { DindPruneGate } from "../../src/dind-prune.js";
import { GitCache, type OwedCandidate, type PositiveOwedCandidateContext } from "../../src/git.js";
import type { Logger } from "../../src/log.js";
import { Outbox } from "../../src/outbox.js";
import { RetainedSourceServer } from "../../src/ops/retained-source-server.js";
import type { RecoveryExactHold, RecoveryHoldsResponse, RunOwnershipResponse } from "../../src/protocol.js";
import type { RecoveryCoordinator, RecoveryRecord } from "../../src/recovery.js";
import { RunRunner } from "../../src/runner.js";
import { makeFixture, type Fixture } from "../fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "../helpers.js";
import { fixtureFetchTracking } from "../runner-tracking-fixture.js";
import { mkdtempForSocket } from "../short-sock-dir.js";

// issue #3885 M2 end-to-end fixture: ONE data dir shared by a real GitCache, the real RunRunner (and the real
// RecoveryCoordinator it builds) and the real RetainedSourceServer over its Unix socket. Only the API client is
// a fake (recorded), plus the proc view a test chooses. Everything else is real Git and real files.

export const WORKER_TOKEN = "e2e-local-record-worker-token";
export const id = (n: number): string => `31050000-0000-4000-8000-${String(n).padStart(12, "0")}`;
export const RUN = id(4);
export const OTHER_RUN = id(44);
export const REPO = id(2);
export const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

export function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["--no-replace-objects", "-C", dir, "-c", "maintenance.auto=false", "-c", "gc.auto=0",
    "-c", "core.fsmonitor=false", "-c", "user.name=E2E", "-c", "user.email=e2e@example.invalid", "-c", "commit.gpgsign=false", ...args],
  { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"], timeout: 20_000 }).trim();
}

export type GenKind = "ack" | "replay" | "caseB";
export interface GenInfo {
  runId: string;
  generation: number;
  kind: GenKind;
  holdId: string;
  captureId: string;
  head: string;
  attempts: string[];
  context: PositiveOwedCandidateContext;
  /** The synthetic owed/attempted refs this generation's fixture commits hang from (the real cleanup never owns them). */
  looseRefs: string[];
  open: boolean;
  exact: RecoveryExactHold | undefined;
  record: RecoveryRecord;
}

export interface Post { runId: string; generation: number | undefined; evidence: string | undefined; disposition: unknown }

/** The recorded API client shared by the coordinator and the channel server. */
export class E2EClient {
  readonly gens = new Map<string, GenInfo>();
  producing: GenInfo | undefined;
  allowFinal = false;
  runStatus = "completed";
  posts: Post[] = [];
  /** Every api WRITE the coordinator attempted (reserve, upload, release, held publication), in order: a Case B disposition makes none. */
  writes: string[] = [];
  /** Late-bound: the coordinator's authenticated view of a run's records (set once the runner exists). */
  records: (runId: string) => Promise<RecoveryRecord[]> = async () => [];
  hasFeature = (): boolean => true;

  key(runId: string, generation: number): string { return `${runId}/${generation}`; }
  of(runId: string, generation: number): GenInfo | undefined { return this.gens.get(this.key(runId, generation)); }

  async getRunOwnership(runId: string): Promise<RunOwnershipResponse> {
    const generations = [...this.gens.values()].filter(g => g.runId === runId).map(g => g.generation);
    const claim = this.producing?.runId === runId ? this.producing.generation : Math.max(0, ...generations);
    return { status: this.runStatus, claim_generation: claim, inventory_guarded: true } as RunOwnershipResponse;
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    const holds = [...this.gens.values()].filter(g => g.runId === runId && g.open).map(g => ({
      hold_id: g.holdId, generation: g.generation, inventory_guarded: true, has_available_capture: false }));
    return { run_id: runId, holds, run_open_holds: { run_id: runId, count: holds.length } } as RecoveryHoldsResponse;
  }
  async reserveRecoveryCapture(): Promise<{ capture_id: string; state: "preparing" }> {
    this.writes.push("reserve");
    return { capture_id: this.producing!.captureId, state: "preparing" };
  }
  async uploadRecoveryBundle(_run: string, _capture: string, _manifest: unknown, stream: AsyncIterable<unknown>) {
    this.writes.push("upload");
    for await (const chunk of stream) assert.ok(chunk);
    return { capture_id: this.producing!.captureId, state: "available" as const, manifest_bound: true };
  }
  async getRecoveryCaptureStatus(runId: string, captureId: string) {
    const record = (await this.records(runId)).find(r => r.serverCaptureId === captureId) ?? (await this.records(runId))[0];
    return { capture_id: captureId, state: "available" as const, manifest_bound: true, checksum: record?.checksum,
      byte_size: record?.byteSize, expires_at: "2099-01-01T00:00:00Z" };
  }
  async releaseRecoveryCustody(runId: string, generation?: number, evidence?: string, disposition?: unknown) {
    this.writes.push("release");
    this.posts.push({ runId, generation, evidence, disposition });
    if (!this.allowFinal) throw new Error("fixture: lost FINAL ACK");
    return { run_id: runId, generation, released: true, holds_released: 1 };
  }
  async publishHeld(): Promise<never> {
    this.writes.push("publishHeld");
    throw new Error("unexpected held publication"); }
  async getExactRecoveryHold(runId: string, generation: number): Promise<RecoveryExactHold | undefined> {
    return this.of(runId, generation)?.exact;
  }
}

/** The closed hold the server would report: discarded by the owner (Case B), or released with the saved FINAL's receipt (Case A). */
export function closedHold(g: Pick<GenInfo, "runId" | "generation" | "holdId">): RecoveryExactHold {
  return { run_id: g.runId, generation: g.generation, hold_id: g.holdId, repo_id: REPO, inventory_guarded: true, state: "discarded",
    run_status: "completed", current_claim_generation: g.generation, claim_released: true, release_evidence: "owner_discard",
    original_worker_id: id(5), original_worker_identity: "e2e-fixture-worker" };
}
export function releasedHold(g: GenInfo): RecoveryExactHold {
  const request = g.record.finalRequest;
  assert.ok(request, `generation ${g.generation} has no saved FINAL`);
  return { ...closedHold(g), state: "released", release_evidence: request.evidence, final_receipt: request.disposition };
}

const EP_AID = (n: number): string => `20261008T171401Z-g${n}-0123456789abcdef`;
export const EPISODE_ATTEMPT = EP_AID(2);
export const EPISODE_GENERATION = 2;

export interface WorldOptions {
  gens: Array<{ generation: number; kind: GenKind }>;
  /** The other run also owed-pins the episode's tip sha (a shared owed pin). */
  sharedOwedPin: boolean;
  /** Install the unrelated run: a guarded generation, its pins, a retained source and (optionally) the shared pin. */
  otherRun: boolean;
  /** The channel server's logger (default: silent). Only the server uses it. */
  serverLog?: Logger;
  /** Test-scoped overrides of the channel server's cooperative deadline and shutdown wait. */
  serverTimeouts?: { resolveDeadlineMs?: number; shutdownSettleMs?: number };
  /** Wire a REAL, initialised Outbox (under `<dir>/outbox`) into the real RunRunner: its `pendingGenerationReport`
   *  then reads the outbox's own terminal / policy / finalize state instead of answering false. */
  outbox?: boolean;
}

export interface Episode { branch: string; key: string; tip: string; sourcePath: string; successorPath: string }
export interface OtherRun { runId: string; generation: number; clonePath: string; attemptId: string; branch: string; ownedPin: string }

export interface World {
  dir: string;
  fx: Fixture;
  git: GitCache;
  bare: string;
  client: E2EClient;
  runner: RunRunner;
  /** The runner's real outbox when the world was created with `outbox: true`. */
  outbox: Outbox | undefined;
  coordinator: RecoveryCoordinator;
  server: RetainedSourceServer;
  gate: DindPruneGate;
  sock: string;
  flags: { terminal: boolean };
  gens: Map<number, GenInfo>;
  episode: Episode;
  other: OtherRun | undefined;
  stop: () => Promise<void>;
}

function commits(bare: string, tag: string, generation: number): { base: string; head: string; attempts: string[]; looseRefs: string[] } {
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const blob = execFileSync("git", ["-C", bare, "hash-object", "-w", "--stdin"], { input: `retained source of ${tag} generation ${generation}\n`,
    encoding: "utf8", env: GIT_ENV }).trim();
  const tree = execFileSync("git", ["-C", bare, "mktree"], { input: `100755 blob ${blob}\tretained.txt\n`, encoding: "utf8", env: GIT_ENV }).trim();
  const head = gitIn(bare, ["commit-tree", tree, "-p", base, "-m", `original ${tag}`]);
  const one = gitIn(bare, ["commit-tree", tree, "-p", head, "-m", `attempt one ${tag}`]);
  const two = gitIn(bare, ["commit-tree", tree, "-p", one, "-m", `attempt two ${tag}`]);
  const looseRefs = [`refs/owed/${tag}`, `refs/attempted/${tag}-one`, `refs/attempted/${tag}-two`];
  gitIn(bare, ["update-ref", looseRefs[0]!, head]);
  gitIn(bare, ["update-ref", looseRefs[1]!, one]);
  gitIn(bare, ["update-ref", looseRefs[2]!, two]);
  return { base, head, attempts: [one, two], looseRefs };
}

/** Produce one guarded generation through the coordinator's own capture path, over the real GitCache. */
async function produce(w: Pick<World, "git" | "bare" | "client" | "coordinator">, runId: string, branch: string, generation: number,
  kind: GenKind, holdId: string): Promise<GenInfo> {
  const { git, bare, client, coordinator } = w;
  const tag = `${runId.slice(-4)}-g${generation}`;
  const c = commits(bare, tag, generation);
  const context: PositiveOwedCandidateContext = { runId, generation, kind: "issue", branch, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: c.base } };
  const info: GenInfo = { runId, generation, kind, holdId, captureId: id(1000 + generation + (runId === RUN ? 0 : 100)), head: c.head,
    attempts: c.attempts, context, looseRefs: c.looseRefs, open: true, exact: undefined, record: undefined as unknown as RecoveryRecord };
  client.gens.set(client.key(runId, generation), info);
  client.producing = info;
  client.allowFinal = kind === "ack";
  // Production-time seams on the real cache, removed again below: the synthetic owed candidate, the recovery pin and no network.
  const real = GitCache.prototype.produceRecoveryBundle;
  const seams = {
    // The synthetic generation root plus the run's REAL owed candidates (the retained episode's tip), as production freezes them.
    enumerateOwedCandidates: async (): Promise<OwedCandidate[]> => [
      { sha: c.head, pinRef: c.looseRefs[0]!, contexts: [context] } as unknown as OwedCandidate,
      ...await GitCache.prototype.enumerateOwedCandidates.call(git, bare, runId),
    ],
    produceRecoveryBundle: async (...args: Parameters<GitCache["produceRecoveryBundle"]>) => {
      const result = await real.apply(git, args);
      gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${runId}/${generation}`, (args[1] as { sourceSha: string }).sourceSha]);
      return result;
    },
    fetchDefaultTip: async () => { throw new Error("credentialed fetch forbidden"); },
    cleanupRecoveryGeneration: undefined,
  };
  Object.assign(git, seams);
  try {
    const frozen = await coordinator.freezeInventory({ context, currentSha: c.head, defaultBranch: "main" });
    assert.ok(frozen, "coordinator must create the initial journal");
    let record = await coordinator["writeExistingRecord"](frozen, cur => ({ ...cur, attemptedHeadSha: c.attempts[0]!, attemptedHeadShas: c.attempts }));
    await coordinator.captureAndUpload({ record, barePath: bare, defaultBranch: "main", onArchiveAccepted: r => { record = r; } });
    record = (await coordinator.inspect(runId)).find(r => r.generation === generation)!;
    assert.ok(record, `generation ${generation} left no record`);
    assert.equal(record.state, "uploaded");
    assert.equal(record.finalAcknowledged === true, kind === "ack", `FINAL ack state of the ${kind} generation`);
    assert.ok(record.finalRequest, "the FINAL request is saved in the journal");
    info.open = false;
    info.record = await coordinator["writeExistingRecord"](record, cur => ({ ...cur, exactHoldBinding: { holdId, repoId: REPO } }));
  } finally {
    for (const key of Object.keys(seams)) delete (git as unknown as Record<string, unknown>)[key];
    client.producing = undefined;
    info.open = false;
  }
  info.exact = kind === "caseB" ? closedHold(info) : releasedHold(info);
  return info;
}

/** A stopped predecessor-capture episode for `runId` (canonical predecessor + attempt successor): the protected retained source. */
async function buildEpisode(git: GitCache, bare: string, runId: string): Promise<Episode> {
  const iid = 2700, branch = `agent/issue-${iid}`, key = `issue-${iid}`;
  const source = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, runId);
  await git.markRecoveryCapture(bare, source.path, branch, runId);
  const src = { runId, clonePath: source.path };
  const reservation = await git.reserveRecoveryIteration(bare, branch, key, src);
  fs.writeFileSync(path.join(source.path, "work.txt"), "retained work\n");
  gitIn(source.path, ["add", "work.txt"]);
  gitIn(source.path, ["commit", "-m", "milestone"]);
  await fixtureFetchTracking(git, bare, source.path, branch, runId);
  const tip = gitIn(source.path, ["rev-parse", "HEAD"]);
  await git.recordRecoveryCapture(bare, branch, key, src, reservation.attempts, tip);
  const successor = await git.prepareRecoverySuccessor(bare, branch, key, src, reservation.attempts, EPISODE_ATTEMPT);
  return { branch, key, tip, sourcePath: source.path, successorPath: successor.path };
}

export async function createWorld(opts: WorldOptions): Promise<World> {
  const dir = mkdtempForSocket("rle-");
  const fx = makeFixture();
  const flags = { terminal: false };
  const git = new GitCache(dir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => flags.terminal }));
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  fs.chmodSync(path.join(bare, "config"), 0o600);
  const client = new E2EClient();
  let outbox: Outbox | undefined;
  if (opts.outbox) {
    outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    assert.equal(outbox.isDisabled(), false, "the fixture outbox must be usable");
  }
  const runner = new RunRunner(client as unknown as ConstructorParameters<typeof RunRunner>[0], git, () => { throw new Error("no executor"); },
    nullLogger(), 20, WORKER_TOKEN, { pollMs: 5, ...(outbox ? { outbox } : {}) });
  const coordinator = runner["recovery"] as RecoveryCoordinator;
  client.records = runId => coordinator.inspect(runId);
  const gens = new Map<number, GenInfo>();
  const branch = "agent/issue-2700";
  const part = { git, bare, client, coordinator };
  // The retained episode exists BEFORE the generations are frozen, so their coverage includes its head (the physical
  // source the FINAL replay's ancestry proof reads).
  const episode = await buildEpisode(git, bare, RUN);
  for (const [index, spec] of opts.gens.entries()) {
    gens.set(spec.generation, await produce(part, RUN, branch, spec.generation, spec.kind, id(100 + index)));
  }
  let other: OtherRun | undefined;
  if (opts.otherRun) {
    await produce(part, OTHER_RUN, "agent/issue-2512", 3, "ack", id(300));
    other = { runId: OTHER_RUN, generation: 3, clonePath: "", attemptId: "", branch: "agent/issue-2512", ownedPin: "" };
  }
  if (other) {
    // The unrelated run's own plain retained source (a dirty attempt clone) and, optionally, the SAME owed sha pinned by it.
    const attemptId = formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, 7)), 1, (8).toString(16).padStart(16, "0"));
    const clone = await git.runnerCloneForBranch(bare, other.branch, "issue-2512", noProofReseed, OTHER_RUN, false, undefined,
      { attemptId, isLive: () => false, beforeSeed: async () => {} });
    await git.markRecoveryCapture(bare, clone.path, other.branch, OTHER_RUN, attemptId);
    fs.writeFileSync(path.join(clone.path, "unpreserved.txt"), "the other run's retained bytes\n");
    assert.equal(await git.retainStoppedAttempt(bare, other.branch, { runId: OTHER_RUN, clonePath: clone.path, attemptId }), "retained");
    other.clonePath = clone.path;
    other.attemptId = attemptId;
    if (opts.sharedOwedPin) {
      const seam = git as unknown as {
        withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T>;
        pinOwedUnderLock: (c: unknown, sha: string) => Promise<void>;
      };
      const context = { runId: OTHER_RUN, branch: other.branch, barePath: bare, generation: 3, kind: "issue",
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]) } };
      await seam.withLock(bare, () => seam.pinOwedUnderLock(context, episode.tip));
      other.ownedPin = `refs/uzi-owed/${OTHER_RUN}/${episode.tip}`;
    }
  }
  client.posts.length = 0;
  client.writes.length = 0;
  client.allowFinal = true;
  for (const g of gens.values()) g.open = false;
  const gate = new DindPruneGate();
  const server = new RetainedSourceServer({
    dataDir: dir, git, gate, runner, log: opts.serverLog ?? nullLogger(), isIdle: () => true,
    ...opts.serverTimeouts,
    client: {
      listRecoveryHolds: runId => client.listRecoveryHolds(runId),
      getRunOwnership: runId => client.getRunOwnership(runId),
    },
  });
  const ac = new AbortController();
  const served = server.serve(ac.signal);
  await server.ready;
  assert.ok(server.listening, "the retained-source channel must be listening");
  return { dir, fx, git, bare, client, runner, outbox, coordinator, server, gate, sock: server.path, flags, gens, episode, other,
    stop: async () => { ac.abort(); await served; fx.cleanup(); fs.rmSync(dir, { recursive: true, force: true }); } };
}

/** A path -> "mode sha256" digest of every entry under `root` (symlinks by target), for byte-equality comparisons. */
export function treeDigest(root: string, skip: (rel: string) => boolean = () => false): string[] {
  const lines: string[] = [];
  const visit = (rel: string): void => {
    if (rel !== "." && skip(rel)) return;
    const full = path.join(root, rel);
    const st = fs.lstatSync(full);
    if (!st.isSymbolicLink() && !st.isDirectory() && !st.isFile()) lines.push(`${rel} special`);
    else if (st.isSymbolicLink()) lines.push(`${rel} link ${fs.readlinkSync(full)}`);
    else if (st.isDirectory()) {
      lines.push(`${rel} dir ${st.mode & 0o7777}`);
      for (const name of fs.readdirSync(full).sort()) visit(path.join(rel, name));
    } else lines.push(`${rel} file ${st.mode & 0o7777} ${createHash("sha256").update(fs.readFileSync(full)).digest("hex")}`);
  };
  visit(".");
  return lines;
}

export function allRefs(bare: string): string[] {
  return gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]).split("\n").filter(Boolean);
}
