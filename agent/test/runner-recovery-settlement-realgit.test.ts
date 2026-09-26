import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { RequestError, type WorkerClient } from "../src/client.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { SettlementJournal, type RecoverySettleClient } from "../src/recovery-settlement.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type {
  ChatClaimResponse,
  ClaimResponse,
  RecoverySettleRequest,
  RecoverySettleResponse,
  StateAck,
  StateRequest,
} from "../src/protocol.js";
import { sleep } from "../src/util.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #1582 M2 — the real-git positive regression: a same-worker interrupted generation (gen1,
// recovery_wait park with committed work) whose gen2 ADOPTS gen1's tip through the REAL GitCache
// seeding (the tracking leg, and the checkpoint leg), publishes a descendant, and completes. The
// settle request must carry the exact hold id, the predecessor generation, gen1's source FROM ITS
// AUTHENTICATED JOURNAL, the adopted tip and the pushed head that descends from both. The runner's
// git, the coordinator's bundle producer and the settlement pins are all the same live GitCache.
// Also the crash boundaries around the post-ACK settle and its restart retry.

const WORKER_TOKEN = "settle-realgit-worker-join-token-0123456789";
const PAT = "fixture-forge-pat-000000"; // the harness claim's forge PAT (a fixture, not a token)
const HOLD_G1 = "11111111-0000-4000-8000-00000000a001";
const HOLD_OTHER = "22222222-0000-4000-8000-00000000a002";
const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitOut(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function refOrNull(dir: string, ref: string): string | null {
  try {
    return gitOut(dir, "rev-parse", "--verify", ref);
  } catch {
    return null;
  }
}
function isAncestor(dir: string, a: string, b: string): boolean {
  try {
    execFileSync("git", ["-C", dir, "merge-base", "--is-ancestor", a, b], { env: GIT_ENV, stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
}
function commitInTree(tree: string, file: string, content: string): string {
  fs.writeFileSync(path.join(tree, file), content);
  gitOut(tree, "add", file);
  gitOut(tree, ...IDENT, "commit", "-q", "-m", `add ${file}`);
  return gitOut(tree, "rev-parse", "HEAD");
}

class FakeSettleClient implements RecoverySettleClient {
  calls: Array<{ runId: string; holdId: string; req: RecoverySettleRequest }> = [];
  constructor(
    private readonly answer: (runId: string, holdId: string) => RecoverySettleResponse | Error,
    private readonly onCall?: () => void,
  ) {}
  async settleRecoveryHold(runId: string, holdId: string, req: RecoverySettleRequest): Promise<RecoverySettleResponse> {
    this.calls.push({ runId, holdId, req });
    this.onCall?.();
    const a = this.answer(runId, holdId);
    if (a instanceof Error) throw a;
    return a;
  }
}
const released = (runId: string, holdId: string): RecoverySettleResponse => ({ run_id: runId, hold_id: holdId, outcome: "released" });
const retained =
  (reason: string) =>
  (runId: string, holdId: string): RecoverySettleResponse => ({ run_id: runId, hold_id: holdId, outcome: "retained", reason });

interface Stores {
  recoveryClient: FakeRecoveryClient;
  coord: RecoveryCoordinator;
  settlement: SettlementJournal;
}
/** The worker's durable stores over the harness dataDir (the SAME paths a restart re-opens). */
function stores(gitCache: GitCache, now?: () => number): Stores {
  const recoveryClient = new FakeRecoveryClient();
  const coord = new RecoveryCoordinator({
    client: recoveryClient,
    git: gitCache, // the REAL GitCache produces the gen1 bundle
    log: nullLogger(),
    recoveryRoot: gitCache.recoveryRoot,
    workerToken: WORKER_TOKEN,
    now: () => 1_700_000_000_000,
  });
  const settlement = new SettlementJournal({
    root: gitCache.recoverySettlementRoot,
    workerToken: WORKER_TOKEN,
    log: nullLogger(),
    now,
  });
  return { recoveryClient, coord, settlement };
}

/** gen1: commit work, then park recovery_wait (a persistently-empty turn). Returns gen1's head.
 *  `opts.file` names the committed file (default GEN1.txt); `opts.dirty` also leaves that file
 *  UNCOMMITTED in the tree, so the park captures it as a `wip(park):` marker on top of the head. */
async function runGen1(s: Stores, claim: ClaimResponse, opts: { file?: string; dirty?: string } = {}): Promise<string> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-g1-"));
  let head = "";
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        head = commitInTree(ctx.worktreePath, opts.file ?? "GEN1.txt", "committed work\n");
        if (opts.dirty) fs.writeFileSync(path.join(ctx.worktreePath, opts.dirty), "uncommitted parked work\n");
        throw new TransientRecoveryError();
      },
    },
  });
  const { gitlab } = fakeGitlab();
  await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: s.coord, settlement: s.settlement }).execute(claim);
  assert.ok(api.states.some((x) => x.runId === claim.run_id && x.body.status === "recovery_wait"), "gen1 parked");
  fs.rmSync(homeRoot, { recursive: true, force: true });
  return head;
}

/** gen2: the StubExecutor commits on top of the adopted tip and completes (push + MR). */
async function runGen2(
  s: Stores,
  claim: ClaimResponse,
  settleClient: RecoverySettleClient,
  outbox?: Outbox,
): Promise<void> {
  const { gitlab } = fakeGitlab();
  await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, nullLogger(), {
    recovery: s.coord,
    settlement: s.settlement,
    settleClient,
    ...(outbox ? { outbox } : {}),
  }).execute(claim);
  assert.ok(api.states.some((x) => x.runId === claim.run_id && x.body.status === "completed"), "gen2 completed");
}

async function mkOutbox(root: string): Promise<Outbox> {
  const outbox = new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

const branchOf = (iid: number) => `agent/issue-${iid}`;
const bare = (): string => git.barePathFor(fx.originPath);

/** Set up gen1 → gen2 on the tracking leg; returns the pieces the assertions need. */
async function trackingScenario(iid: number): Promise<{ s: Stores; gen1Claim: ClaimResponse; gen2Claim: ClaimResponse; gen1Head: string }> {
  const s = stores(git);
  const gen1Claim = gitlabClaim(iid, { claim_generation: 1 });
  const gen1Head = await runGen1(s, gen1Claim);
  const g1 = (await s.coord.inspect(gen1Claim.run_id)).find((r) => r.generation === 1);
  assert.ok(g1, "gen1 left an authenticated journal record");
  assert.equal(g1!.sourceSha, gen1Head, "gen1's journaled source is its committed head");
  assert.equal(refOrNull(bare(), `refs/uzi-runner/${branchOf(iid)}`), gen1Head, "gen1's tip is on the tracking ref");
  const gen2Claim = { ...gen1Claim, claim_generation: 2 } as ClaimResponse;
  // The api inventories gen1's hold (open, this worker's) plus nothing else.
  s.recoveryClient.holds = [{ hold_id: HOLD_G1, generation: 1, has_available_capture: true }];
  return { s, gen1Claim, gen2Claim, gen1Head };
}

describe("real-git adoption → settle (issue #1582 M2)", () => {
  it("tracking leg: gen2 adopts gen1's tip, publishes a descendant, settle carries the exact candidates; released cleans up only gen1", async () => {
    const iid = 6101;
    const { s, gen2Claim, gen1Head } = await trackingScenario(iid);
    const settleClient = new FakeSettleClient(released);
    await runGen2(s, gen2Claim, settleClient);

    const pushed = gitOut(fx.originPath, "rev-parse", branchOf(iid));
    assert.deepEqual(settleClient.calls, [
      {
        runId: gen2Claim.run_id,
        holdId: HOLD_G1,
        req: {
          predecessor_generation: 1,
          successor_generation: 2,
          pushed_sha: pushed,
          source_sha: gen1Head,
          adopted_sha: gen1Head,
        },
      },
    ]);
    assert.notEqual(pushed, gen1Head, "gen2 published a strict descendant");
    assert.ok(isAncestor(fx.originPath, gen1Head, pushed), "the pushed head descends from the source and the adopted tip");
    // released → only the predecessor's evidence + journal + pins are cleaned up.
    assert.deepEqual(await s.settlement.listRun(gen2Claim.run_id), []);
    assert.deepEqual(await s.coord.inspect(gen2Claim.run_id), []);
    for (const kind of ["source", "adopted", "pushed"]) {
      assert.equal(refOrNull(bare(), `refs/uzi-settle/${gen2Claim.run_id}/${HOLD_G1}/${kind}`), null);
    }
    assert.equal(refOrNull(bare(), `refs/uzi-recovery-pin/${gen2Claim.run_id}/1`), null, "gen1's recovery pin is removed");
    assert.deepEqual(s.recoveryClient.releasedGenerations(), [2], "the api settled gen1; the worker released only gen2 itself");
  });

  for (const reason of ["not_ancestor", "ancestry_unknown"]) {
    it(`negative: the api answers ${reason} → everything retained`, async () => {
      const iid = reason === "not_ancestor" ? 6102 : 6103;
      const { s, gen2Claim, gen1Head } = await trackingScenario(iid);
      const settleClient = new FakeSettleClient(retained(reason));
      await runGen2(s, gen2Claim, settleClient);
      assert.equal(settleClient.calls.length, 1);
      const [rec] = await s.settlement.listRun(gen2Claim.run_id);
      assert.equal(rec!.state, reason === "not_ancestor" ? "terminal" : "pending_settle");
      assert.deepEqual((await s.coord.inspect(gen2Claim.run_id)).map((r) => r.generation), [1], "gen1 journal kept");
      assert.equal(refOrNull(bare(), `refs/uzi-settle/${gen2Claim.run_id}/${HOLD_G1}/source`), gen1Head);
      assert.equal(refOrNull(bare(), `refs/uzi-settle/${gen2Claim.run_id}/${HOLD_G1}/adopted`), gen1Head);
      assert.ok(refOrNull(bare(), `refs/uzi-settle/${gen2Claim.run_id}/${HOLD_G1}/pushed`));
    });
  }

  it("checkpoint leg: gen2 adopts this run's own mirrored checkpoint (no tracking ref) and settles the exact candidates", async () => {
    const iid = 6104;
    const s = stores(git);
    const gen1Claim = gitlabClaim(iid, { claim_generation: 1 });
    const gen1Head = await runGen1(s, gen1Claim);
    const branch = branchOf(iid);
    // gen1's tip was published as this run's own checkpoint on the forge; the tracking ref is gone,
    // so the reseed takes the owner-anchored checkpoint leg rather than the tracking leg.
    gitOut(bare(), "push", "-q", fx.originPath, `${gen1Head}:refs/uzi-checkpoints/${branch}`);
    gitOut(bare(), "update-ref", "-d", `refs/uzi-runner/${branch}`);
    s.recoveryClient.holds = [{ hold_id: HOLD_G1, generation: 1, has_available_capture: true }];
    const gen2Claim = {
      ...gen1Claim,
      claim_generation: 2,
      session_id: gen1Claim.session_id ?? "sess-gen1",
      checkpoint_tip: gen1Head,
    } as ClaimResponse;
    const legs: string[] = [];
    const orig = git.createOrAttachRunnerClone.bind(git);
    git.createOrAttachRunnerClone = async (...args: Parameters<GitCache["createOrAttachRunnerClone"]>) => {
      const clone = await orig(...args);
      legs.push(clone.seededFrom);
      return clone;
    };
    const settleClient = new FakeSettleClient(released);
    await runGen2(s, gen2Claim, settleClient);
    assert.deepEqual(legs, ["checkpoint"], "the REAL seeding took the checkpoint leg");
    const pushed = gitOut(fx.originPath, "rev-parse", branch);
    assert.equal(settleClient.calls.length, 1, "the checkpoint adoption produced a settle");
    assert.deepEqual(settleClient.calls[0]!.req, {
      predecessor_generation: 1,
      successor_generation: 2,
      pushed_sha: pushed,
      source_sha: gen1Head,
      adopted_sha: gen1Head,
    });
    assert.ok(isAncestor(fx.originPath, gen1Head, pushed));
    assert.deepEqual(await s.settlement.listRun(gen2Claim.run_id), []);
  });

  it("a recovered wip(park) marker: no generation that adopts past it records evidence for it (gen2 and gen3); a contained sibling still settles", async () => {
    const iid = 6105;
    const branch = branchOf(iid);
    const s = stores(git);
    // gen1 commits work and parks with UNCOMMITTED work → the park commits a real wip(park) marker.
    const gen1Claim = gitlabClaim(iid, { claim_generation: 1 });
    const gen1Head = await runGen1(s, gen1Claim, { dirty: "WIP.txt" });
    const g1 = (await s.coord.inspect(gen1Claim.run_id)).find((r) => r.generation === 1);
    assert.ok(g1, "gen1 left an authenticated journal record");
    const marker = g1!.sourceSha;
    assert.notEqual(marker, gen1Head, "precondition: gen1's journaled source is the marker, not its committed head");
    assert.match(gitOut(bare(), "log", "-1", "--format=%s", marker), /^wip\(park\):/, "precondition: a real wip(park) marker");
    assert.equal(gitOut(bare(), "rev-parse", `${marker}^`), gen1Head, "the marker sits on gen1's committed head");

    // gen2 resumes on the REAL tracking leg, recovers the marker (reset --soft), commits, parks again.
    const seeds: Array<{ seededFrom: string; wipRecovered?: boolean; baseCommit: string }> = [];
    const orig = git.createOrAttachRunnerClone.bind(git);
    git.createOrAttachRunnerClone = async (...args: Parameters<GitCache["createOrAttachRunnerClone"]>) => {
      const clone = await orig(...args);
      seeds.push({ seededFrom: clone.seededFrom, wipRecovered: clone.wipRecovered, baseCommit: clone.baseCommit });
      return clone;
    };
    s.recoveryClient.holds = [{ hold_id: HOLD_G1, generation: 1, has_available_capture: true }];
    const gen2Claim = { ...gen1Claim, claim_generation: 2 } as ClaimResponse;
    const gen2Head = await runGen1(s, gen2Claim, { file: "GEN2.txt" });
    assert.deepEqual(seeds[0], { seededFrom: "tracking", wipRecovered: true, baseCommit: gen1Head }, "gen2 recovered the marker");
    assert.ok(!isAncestor(bare(), marker, gen2Head), "the marker is out of gen2's history");
    assert.deepEqual(await s.settlement.listRun(gen1Claim.run_id), [], "gen2 recorded no evidence for gen1's marker");
    assert.equal(refOrNull(bare(), `refs/uzi-settle/${gen1Claim.run_id}/${HOLD_G1}/source`), null, "no pin for gen1");

    // gen3 resumes NORMALLY on gen2's committed tip (no marker to recover) and completes.
    s.recoveryClient.holds = [
      { hold_id: HOLD_G1, generation: 1, has_available_capture: true },
      { hold_id: HOLD_OTHER, generation: 2, has_available_capture: true },
    ];
    const gen3Claim = { ...gen1Claim, claim_generation: 3 } as ClaimResponse;
    const settleClient = new FakeSettleClient(released);
    await runGen2(s, gen3Claim, settleClient);
    assert.equal(seeds[1]!.seededFrom, "tracking");
    assert.notEqual(seeds[1]!.wipRecovered, true, "gen3 recovered no marker of its own");
    const pushed = gitOut(fx.originPath, "rev-parse", branch);
    assert.deepEqual(
      settleClient.calls.map((c) => [c.holdId, c.req]),
      [
        [
          HOLD_OTHER,
          { predecessor_generation: 2, successor_generation: 3, pushed_sha: pushed, source_sha: gen2Head, adopted_sha: gen2Head },
        ],
      ],
      "only gen2's contained source is settled; gen1's marker is never sent",
    );
    assert.equal(refOrNull(bare(), `refs/uzi-settle/${gen3Claim.run_id}/${HOLD_G1}/source`), null, "no pin for gen1's marker");
    assert.deepEqual(await s.settlement.listRun(gen3Claim.run_id), [], "gen2's hold released; nothing recorded for gen1");
    assert.deepEqual(
      (await s.coord.inspect(gen3Claim.run_id)).map((r) => r.generation),
      [1],
      "gen1's journal (the marker source) is retained",
    );
  });
});

// ── crash boundaries ─────────────────────────────────────────────────────────────

const okPreflight = (): { ok: boolean; missing: string[] } => ({ ok: true, missing: [] });
const idleChat = { execute: async () => {} } as unknown as ChatRunner;
const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
function fakeConfig(dataDir: string): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 5,
    heartbeatIntervalMs: 5,
    chatPollMs: 5,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: {},
    dataDir,
    gapFillMax: 100,
    outboxTerminalMaxBytes: 1 << 20,
  } as unknown as Config;
}
/** A worker-lane client for the restarted worker: idle claims, and every /state (the boot
 *  pending-terminal replay) answers completed. It has NO forge credential of any kind. */
function bootClient(events: string[]): WorkerClient {
  return {
    register: async () => ({}),
    heartbeat: async () => {},
    reportState: async (_runId: string, body: StateRequest) => {
      events.push(`state:${body.status}`);
      return { applied: true, status: "completed" } as StateAck;
    },
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    hasFeature: () => false,
    getMessageGaps: async () => ({ gaps: [] }),
    postMessages: async () => {},
  } as unknown as WorkerClient;
}

async function pollUntil(pred: () => boolean | Promise<boolean>, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await pred()) return;
    await sleep(10);
  }
  assert.fail(`timed out waiting for: ${label}`);
}

/** Copy a directory tree (or note its absence) — the on-disk state at the "crash" instant. */
function snapshot(dir: string, into: string): () => void {
  const exists = fs.existsSync(dir);
  if (exists) fs.cpSync(dir, into, { recursive: true });
  return () => {
    fs.rmSync(dir, { recursive: true, force: true });
    if (exists) fs.cpSync(into, dir, { recursive: true });
  };
}

/** Restart a worker over the durable state in fx.dataDir (the forge is unreachable) and wait until
 *  the settlement journal of `runId` is empty. Returns the boot events (`state:*` replays, `settle:*`)
 *  and the settle requests. `onPromote` sees every settlement put of a `pending_settle` record. */
async function bootAndSettle(
  runId: string,
  outboxRoot: string,
  onPromote?: (outbox: Outbox) => void,
): Promise<{ events: string[]; calls: FakeSettleClient["calls"]; s2: Stores }> {
  const events: string[] = [];
  const git2 = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const s2 = stores(git2);
  const settleClient2 = new FakeSettleClient((rid, holdId) => {
    events.push(`settle:${holdId}`);
    return released(rid, holdId);
  });
  const outbox2 = await mkOutbox(outboxRoot);
  if (onPromote) {
    const put = s2.settlement.put.bind(s2.settlement);
    s2.settlement.put = async (rec) => {
      if (rec.state === "pending_settle") onPromote(outbox2);
      return put(rec);
    };
  }
  const client2 = bootClient(events);
  const runner2 = new RunRunner(client2, git2, () => { throw new Error("no claims in this test"); }, nullLogger(), 20, undefined, {
    recovery: s2.coord,
    settlement: s2.settlement,
    settleClient: settleClient2,
    outbox: outbox2,
  });
  const worker = new Worker(fakeConfig(fx.dataDir), client2, runner2, idleChat, noJudge, noReview, nullLogger(), okPreflight, outbox2, new Map(), undefined, 60_000);
  const controller = new AbortController();
  const done = worker.run(controller.signal);
  try {
    await pollUntil(async () => (await s2.settlement.listRun(runId)).length === 0, 10_000, "the sweep settled the hold");
  } finally {
    controller.abort();
    await done;
  }
  return { events, calls: settleClient2.calls, s2 };
}

describe("settlement crash boundaries (issue #1582 M2)", () => {
  it("(a) completion ACK applied + outbox retired, crash at the post-completion settle → restart: the sweep settles with no PAT", async () => {
    const iid = 6201;
    const { s, gen2Claim, gen1Head } = await trackingScenario(iid);
    const outboxRoot = path.join(fx.dataDir, "outbox");
    const outbox1 = await mkOutbox(outboxRoot);
    const snapRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-crash-"));
    let restore: Array<() => void> = [];
    let pendingAtSettle: boolean | undefined;
    // The "crash": at the instant the post-completion settle is attempted, freeze the durable state
    // (settlement record, gen1 journal, outbox) — nothing the live process does after this survives.
    const crashingClient = new FakeSettleClient(
      () => new Error("process died"),
      () => {
        if (restore.length > 0) return;
        // N4: the in-run settle runs only AFTER the terminal outcome resolved (journal retired).
        pendingAtSettle = outbox1.hasPendingTerminal(gen2Claim.run_id, 2);
        restore = [
          snapshot(outboxRoot, path.join(snapRoot, "outbox")),
          snapshot(git.recoverySettlementRoot, path.join(snapRoot, "settlement")),
          snapshot(git.recoveryRoot, path.join(snapRoot, "recovery")),
        ];
      },
    );
    await runGen2(s, gen2Claim, crashingClient, outbox1);
    assert.equal(crashingClient.calls.length, 1, "the live settle was attempted after the ACK");
    assert.equal(pendingAtSettle, false, "the settle ran after the outbox terminal journal was retired, not inside the send");
    for (const r of restore) r();
    const settleDir = path.join(git.recoverySettlementRoot, gen2Claim.run_id);
    for (const f of fs.readdirSync(settleDir)) {
      const bytes = fs.readFileSync(path.join(settleDir, f), "utf8");
      assert.ok(!bytes.includes(PAT), "no forge PAT in the settlement journal");
      assert.doesNotMatch(bytes, /forge_pat|token/i);
    }
    const [atCrash] = await s.settlement.listRun(gen2Claim.run_id);
    assert.equal(atCrash!.state, "pending_settle", "the ACK was observed before the crash");
    // No forge is reachable after the restart: the settle needs none (server-side proof).
    fs.renameSync(fx.originPath, `${fx.originPath}.gone`);
    try {
      const { events, calls, s2 } = await bootAndSettle(gen2Claim.run_id, outboxRoot);
      assert.deepEqual(events, [`settle:${HOLD_G1}`], "nothing to replay; the sweep settles");
      assert.deepEqual(calls[0]!.req, {
        predecessor_generation: 1,
        successor_generation: 2,
        pushed_sha: gitOut(`${fx.originPath}.gone`, "rev-parse", branchOf(iid)),
        source_sha: gen1Head,
        adopted_sha: gen1Head,
      });
      assert.deepEqual(await s2.coord.inspect(gen2Claim.run_id), [], "gen1's journal removed after the release");
    } finally {
      fs.renameSync(`${fx.originPath}.gone`, fx.originPath);
      fs.rmSync(snapRoot, { recursive: true, force: true });
    }
  });

  it("(c) completion ACK received, crash BEFORE the promotion → boot replays the terminal, promotes before retiring it, then the sweep settles", async () => {
    const iid = 6202;
    const { s, gen2Claim, gen1Head } = await trackingScenario(iid);
    const outboxRoot = path.join(fx.dataDir, "outbox");
    const outbox1 = await mkOutbox(outboxRoot);
    const snapRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-crash-"));
    let restore: Array<() => void> = [];
    // The "crash": the completion ACK came back, and the process dies at the instant it would
    // promote the write-ahead `pushed` record (freeze the durable state BEFORE that write).
    const put = s.settlement.put.bind(s.settlement);
    s.settlement.put = async (rec) => {
      if (rec.state === "pending_settle" && restore.length === 0) {
        restore = [
          snapshot(outboxRoot, path.join(snapRoot, "outbox")),
          snapshot(git.recoverySettlementRoot, path.join(snapRoot, "settlement")),
          snapshot(git.recoveryRoot, path.join(snapRoot, "recovery")),
        ];
      }
      return put(rec);
    };
    await runGen2(s, gen2Claim, new FakeSettleClient(released), outbox1);
    assert.ok(restore.length > 0, "precondition: the promotion was reached");
    for (const r of restore) r();
    const [atCrash] = await s.settlement.listRun(gen2Claim.run_id);
    assert.equal(atCrash!.state, "pushed", "at the crash the record is write-ahead only (never sendable)");
    fs.renameSync(fx.originPath, `${fx.originPath}.gone`);
    try {
      const outboxProbe = await mkOutbox(outboxRoot);
      assert.equal(outboxProbe.hasPendingTerminal(gen2Claim.run_id, 2), true, "precondition: the terminal is still pending at boot");
      const pendingAtPromote: boolean[] = [];
      const { events, calls, s2 } = await bootAndSettle(gen2Claim.run_id, outboxRoot, (ob) =>
        pendingAtPromote.push(ob.hasPendingTerminal(gen2Claim.run_id, 2)),
      );
      assert.deepEqual(pendingAtPromote, [true], "the replay promoted BEFORE the outbox entry was retired");
      assert.deepEqual(events, ["state:completed", `settle:${HOLD_G1}`], "pending terminal replayed FIRST, then the settle");
      assert.deepEqual(calls[0]!.req, {
        predecessor_generation: 1,
        successor_generation: 2,
        pushed_sha: gitOut(`${fx.originPath}.gone`, "rev-parse", branchOf(iid)),
        source_sha: gen1Head,
        adopted_sha: gen1Head,
      });
      // gen2's own exact-generation record is out of scope here (the crash preceded its own release).
      assert.deepEqual(
        (await s2.coord.inspect(gen2Claim.run_id)).filter((r) => r.generation === 1),
        [],
        "gen1's journal removed after the release",
      );
    } finally {
      fs.renameSync(`${fx.originPath}.gone`, fx.originPath);
      fs.rmSync(snapRoot, { recursive: true, force: true });
    }
  });

  for (const [label, first] of [
    ["ancestry_unknown", retained("ancestry_unknown")],
    ["503", () => new RequestError("POST", "/settle", 503, "unavailable")],
    ["network error", () => new TypeError("fetch failed")],
  ] as const) {
    it(`(b) first settle fails transiently (${label}) → restart → the retry releases ONLY the evidenced hold`, async () => {
      const iid = label === "503" ? 6302 : label === "network error" ? 6303 : 6301;
      const { s, gen2Claim } = await trackingScenario(iid);
      // A second open hold with NO authenticated predecessor journal: inventoried, never evidenced.
      s.recoveryClient.holds = [
        { hold_id: HOLD_G1, generation: 1, has_available_capture: true },
        { hold_id: HOLD_OTHER, generation: 0, has_available_capture: false },
      ];
      const settle1 = new FakeSettleClient(first);
      await runGen2(s, gen2Claim, settle1);
      assert.deepEqual(settle1.calls.map((c) => c.holdId), [HOLD_G1]);
      const [rec] = await s.settlement.listRun(gen2Claim.run_id);
      assert.equal(rec!.state, "pending_settle");
      assert.equal(rec!.attempts, 1);
      // Restart: fresh instances over the same dataDir, the clock past the backoff.
      const git2 = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      const later = () => Date.now() + 2 * 60 * 60_000;
      const s2 = stores(git2, later);
      const settle2 = new FakeSettleClient(released);
      const runner2 = new RunRunner(bootClient([]), git2, () => { throw new Error("no claims"); }, nullLogger(), 20, undefined, {
        recovery: s2.coord,
        settlement: s2.settlement,
        settleClient: settle2,
      });
      await runner2.settlePendingPredecessors();
      assert.deepEqual(settle2.calls.map((c) => c.holdId), [HOLD_G1], "only the evidenced hold is ever sent");
      assert.deepEqual(await s2.settlement.listAll(), []);
      assert.deepEqual(await s2.coord.inspect(gen2Claim.run_id), []);
    });
  }
});
