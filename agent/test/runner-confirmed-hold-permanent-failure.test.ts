// Issue #4645: a permanent message failure (the batcher breaker) that trips AFTER the server confirmed a hold or a wall park must keep
// that accepted disposition: no `failed` report, no terminal journal, no abort of the flight, no terminal reap. Every row drives the
// production RunRunner.execute() with the real MessageBatcher, once as a no-trip control and once with the breaker tripped.
//
// How the trip is injected: the first MessageBatcher.close() that starts while the flight's holdOrWallParkConfirmed flag is already true
// is the final drain of that path. Right before it runs, the test arms ONE permanent 403 for the next message send and queues a frame,
// so the drain's send is rejected permanently and the real breaker trips after confirmation. The phase-switch row needs this injection
// by construction: production emits nothing on the held arm between the pre-request flush and the final close. The other rows also
// carry their own production frames (the preflight held text, the walled/completion exits), but the injected frame is what proves a
// frame is still queued for the final drain.
//
// Drain sites covered here: the preflight hold (enterPreflightHold), the completion hold exit (phasePublish completionHeld), the
// phase-switch hold (enterPhaseSwitchHold), the interlocked completion hold (holdOrFailInterlocked) and the wall park exit
// (phasePublish walled). The prepush completion-attempt finish that also confirms a hold goes through the same enterCompletionHold
// setter and the same guard; it has no row of its own.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { MessageBatcher } from "../src/batcher.js";
import { GitCache } from "../src/git.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import type { ClaimResponse, BaseAttestationHoldOutcome } from "../src/protocol.js";
import { CONFIRMED_DISPOSITION_TRIP_LOG, RetainedRecoveryStop, RunRunner, type ExecutorFactory } from "../src/runner.js";
import { nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import {
  codexRig, commitInTree, enabledRecovery, FakeCodexExecutor, installRecoveryRootCleanup, spyPublishLands, type CodexRig,
} from "./runner-codex-sinks-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";

installHarness();
installRecoveryRootCleanup();

const FRAME = "frame queued for the final drain after the confirmation";
const HELD_HEAD = "cafef00dcafef00dcafef00dcafef00dcafef00d";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test"];
const gitIn = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();

interface FlightView {
  runId: string;
  batcher: MessageBatcher;
  cancel: AbortController;
  holdOrWallParkConfirmed: boolean;
  workerMemory?: unknown;
  preserveRecoveryClone?: boolean;
  preserveSession?: boolean;
  retainedLocalCustody?: boolean;
  keepGuardedInventoryOpen?: boolean;
  parked?: boolean;
}

interface CodexBaseline { boundaries: number; refresh: number; release: number; spawns: number }

/** What one execute() did, observed through wrappers on the production seams. */
class Observer {
  flight: FlightView | undefined;
  batcher: MessageBatcher | undefined;
  armed = false;
  queuedBefore = -1;
  queuedAfter = -1;
  entries: Array<{ confirmed: boolean; aborted: boolean }> = [];
  abortedAfterHandler: boolean[] = [];
  handlerPromises: Promise<unknown>[] = [];
  inHandler = 0;
  terminalInstalls = 0;
  handlerReaps = 0;
  totalReaps = 0;
  codexBaseline: CodexBaseline | undefined;
  memoryOverlap = false;
  /** The representative memory hold of the memory row; the same object must stay on the flight from before the trip to settlement. */
  /** Kept apart from the stand-in so a handler that clears `memoryStandIn.hold` cannot also move the expected value. */
  readonly memoryHold: object = Object.freeze({});
  readonly memoryStandIn: { hold?: object; feedback: Set<unknown> } = { hold: this.memoryHold, feedback: new Set() };
  memoryAfterHandlerCall: Array<{ memory: unknown; hold: unknown }> = [];
  reconciles: Array<{ inHandler: number; memory: unknown; hold: unknown }> = [];
  stopReasonAfterHandler: unknown;
  constructor(readonly trip: boolean, readonly rig: CodexRig | undefined) {}
}

let current: Observer | undefined;

/** The close hook: the first close after the flag is confirmed becomes the injected final drain. */
function installCloseHook(t: TestContext): void {
  const close = MessageBatcher.prototype.close;
  t.mock.method(MessageBatcher.prototype, "close", async function (this: MessageBatcher, ...args: Parameters<MessageBatcher["close"]>) {
    const obs = current;
    if (obs?.trip && !obs.armed && obs.flight?.holdOrWallParkConfirmed === true) {
      obs.armed = true;
      obs.batcher = this;
      obs.queuedBefore = this.bufferedCount();
      // The memory row installs its representative hold here: after the confirmation, immediately before the injected trip. A hold
      // present from the start of the flight reroutes the hold exit itself (the flight stops retained before the final drain, so
      // nothing is left to trip). From this point it stays on the flight through settlement. Unlike production (preparePreservation
      // retains custody before it sets `hold`), custody is NOT pre-retained here. Of the row's custody flags, retainedLocalCustody is
      // the one only the handler's memory branch sets; preserveRecoveryClone and preserveSession are already set earlier, by
      // enterCompletionHold's capture retention, so the row cannot attribute them to the memory branch.
      if (obs.memoryOverlap) obs.flight!.workerMemory = obs.memoryStandIn;
      api.failMessagesNext(1, 403);
      this.emit({ kind: "status", agent: "worker", payload: { text: FRAME } });
      obs.queuedAfter = this.bufferedCount();
    }
    return close.apply(this, args);
  });
}

const isFlight = (x: unknown): x is FlightView =>
  typeof x === "object" && x !== null && "holdOrWallParkConfirmed" in x && "batcher" in x && "cancel" in x;

function observe(t: TestContext, run: RunRunner, trip: boolean, rig?: CodexRig, memoryOverlap = false): Observer {
  const obs = new Observer(trip, rig);
  obs.memoryOverlap = memoryOverlap;
  current = obs;
  const r = run as unknown as Record<string, (...a: unknown[]) => unknown>;
  const original = (name: string) => r[name]!.bind(run);
  const adopt = (flight: FlightView): void => {
    if (obs.flight === flight) return;
    obs.flight = flight;
    let value = flight.holdOrWallParkConfirmed;
    Object.defineProperty(flight, "holdOrWallParkConfirmed", {
      configurable: true,
      enumerable: true,
      get: () => value,
      set: (next: boolean) => {
        value = next;
        // The baseline is taken the moment the setter confirms, before anything else runs.
        if (next && rig) obs.codexBaseline = {
          boundaries: rig.boundaries.length, refresh: rig.refreshCalls(), release: rig.releaseCalls(), spawns: rig.processSpawns(),
        };
      },
    });
  };
  for (const name of ["enterCompletionHold", "enterPreflightHold", "enterPhaseSwitchHold", "enterWallPark"]) {
    const o = original(name);
    t.mock.method(r, name, (...a: unknown[]) => {
      const flight = a.find(isFlight);
      if (flight) adopt(flight);
      return o(...a);
    });
  }
  const handler = original("handlePermanentFailure");
  t.mock.method(r, "handlePermanentFailure", (...a: unknown[]) => {
    const flight = a[1] as FlightView;
    obs.entries.push({ confirmed: flight.holdOrWallParkConfirmed, aborted: flight.cancel.signal.aborted });
    obs.inHandler++;
    const promise = Promise.resolve(handler(...a));
    // The memory branch of the handler runs synchronously up to its return, so the object is read right after the call.
    obs.memoryAfterHandlerCall.push({ memory: flight.workerMemory, hold: (flight.workerMemory as { hold?: unknown } | undefined)?.hold });
    const settled = promise.finally(() => {
      obs.inHandler--;
      obs.abortedAfterHandler.push(flight.cancel.signal.aborted);
      obs.stopReasonAfterHandler = flight.cancel.signal.reason;
    });
    obs.handlerPromises.push(settled);
    return settled;
  });
  if (obs.memoryOverlap) {
    // The stand-in lacks what the real reconcile needs (the memory runtime, sink receipts), so this spy records the flight's own
    // reconciliation call and the memory object it sees, then returns without calling the original.
    t.mock.method(r, "reconcileWorkerMemory", async (...a: unknown[]) => {
      const flight = a[0] as FlightView;
      obs.reconciles.push({ inHandler: obs.inHandler, memory: flight.workerMemory, hold: (flight.workerMemory as { hold?: unknown } | undefined)?.hold });
    });
  }
  const terminal = original("journalAndSendTerminal");
  t.mock.method(r, "journalAndSendTerminal", (...a: unknown[]) => { obs.terminalInstalls++; return terminal(...a); });
  for (const name of ["reapRecoveryProviderForSettle", "reapForSink"]) {
    const o = original(name);
    t.mock.method(r, name, (...a: unknown[]) => {
      obs.totalReaps++;
      if (obs.inHandler > 0) obs.handlerReaps++;
      return o(...a);
    });
  }
  return obs;
}

/** The real content of the retained source after one execute(): clone HEAD and tree, the seeded work files, HOME and its session files. */
interface Retained {
  clone: boolean;
  home: boolean;
  head: string | undefined;
  parent: string | undefined;
  tree: string | undefined;
  paths: string[];
  files: Record<string, string>;
  session: Record<string, string>;
}

/**
 * What the row's fixture seeded, so the retained content is checked against it and not only against the control. `head` is the exact
 * commit the fixture made (or the planned base when it made none); `parent` is used instead when the executor's own commit (the
 * StubExecutor's) is not known to the test, and then the retained HEAD must sit directly on that base.
 */
interface Seeded { head?: string; parent?: string; files: Record<string, string>; session: Record<string, string> }

const headMatchesSeed = (r: Retained, s: Seeded): boolean => s.head !== undefined ? r.head === s.head : r.parent === s.parent;

interface Built {
  run: RunRunner;
  claim: ClaimResponse;
  go: () => Promise<void>;
  kept: () => Retained;
  seeded: () => Seeded;
  cleanup: () => void;
  rig?: CodexRig;
}

const readFiles = (dir: string, names: string[]): Record<string, string> =>
  Object.fromEntries(names.filter(n => fs.existsSync(path.join(dir, n))).map(n => [n, fs.readFileSync(path.join(dir, n), "utf8")]));

/** Inspects a retained clone (HEAD sha, tree sha, the named work files) and the HOME dir (existence and the named session files). */
function inspectRetained(cloneDir: string | undefined, workFiles: string[], homeDir: string, sessionFiles: string[], cloneProof = true): Retained {
  const clone = cloneDir !== undefined && fs.existsSync(cloneDir) && cloneProof;
  return {
    clone,
    home: fs.existsSync(homeDir),
    head: clone ? gitIn(cloneDir!, ["rev-parse", "HEAD"]) : undefined,
    parent: clone ? (() => { try { return gitIn(cloneDir!, ["rev-parse", "--verify", "-q", "HEAD^"]); } catch { return undefined; } })() : undefined,
    tree: clone ? gitIn(cloneDir!, ["rev-parse", "HEAD^{tree}"]) : undefined,
    paths: clone ? gitIn(cloneDir!, ["ls-tree", "-r", "--name-only", "HEAD"]).split("\n") : [],
    files: clone ? readFiles(cloneDir!, workFiles) : {},
    session: fs.existsSync(homeDir) ? readFiles(homeDir, sessionFiles) : {},
  };
}

async function mkOutbox(tag: string): Promise<Outbox> {
  const outbox = new Outbox({
    root: path.join(fx.dataDir, `outbox-${tag}`), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

const SESSION_FILE = "SESSION_TRANSCRIPT";
const SESSION_TEXT = "resumable session state\n";

function seedHome(): { homeDir: string; root: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-4645-home-"));
  const homeDir = path.join(root, "h");
  fs.mkdirSync(homeDir, { recursive: true });
  fs.writeFileSync(path.join(homeDir, SESSION_FILE), SESSION_TEXT);
  return { homeDir, root };
}

type Builder = (iid: number, logger: ReturnType<typeof recordingLogger>["logger"], outbox: Outbox) => Promise<Built>;

// Row 2: the completion hold exit of phasePublish.
const completionHold: Builder = async (iid, logger, outbox) => {
  const { gitlab } = fakeGitlab();
  const home = seedHome();
  const claim = gitlabClaim(iid);
  const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  api.setCompletionHoldResponse("paused", 200);
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
      mockCommittedTracking(git, HELD_HEAD);
      git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => HELD_HEAD) as typeof git.trackingTip;
      git.checkpointPack = (async () => null) as typeof git.checkpointPack;
      const entered = await ctx.enterCompletionHold!("test completion hold reason");
      assert.equal(entered, true, "the hold was confirmed");
      return { branch: ctx.branch, completionHeld: { reason: "test completion hold reason" } };
    },
  };
  const run = runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, logger, { recoveryRetryMs: 1, outbox });
  return {
    run, claim, go: () => run.execute(claim),
    kept: () => inspectRetained(worktreeDirFor(iid), [], home.homeDir, [SESSION_FILE]),
    seeded: () => ({ head: planned, files: {}, session: { [SESSION_FILE]: SESSION_TEXT } }),
    cleanup: () => fs.rmSync(home.root, { recursive: true, force: true }),
  };
};

// Row 4 (plain) and its Codex variant: a denied permit makes holdOrFailInterlocked park the run (finish(true) closes the batcher).
function interlockedHold(withCodex: boolean): Builder {
  return async (iid, logger, outbox) => {
    const { gitlab } = fakeGitlab();
    const home = seedHome();
    const claim = gitlabClaim(iid, { claim_generation: 7, config: { completion_contract_version: 1, contract_revision: 1 } });
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("confirmed-hold-4645");
    api.setPublicationState(claim.run_id, [], 1);
    api.setCompletionPermitResponse(false, { denyReason: "incomplete" });
    const rig = withCodex ? codexRig() : undefined;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    let committed: string | undefined;
    const executor: Executor = rig
      ? new FakeCodexExecutor(rig.safety, async ctx => {
        fs.writeFileSync(path.join(home.homeDir, "session.marker"), "resume evidence\n");
        commitInTree(ctx.worktreePath, "W4645.txt", "work before the hold\n");
        committed = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
        return { branch: ctx.branch };
      }, undefined, rig.capture)
      : new StubExecutor(nullLogger());
    const run = runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, logger, { recoveryRetryMs: 1, outbox, ...(rig ? { recovery: enabledRecovery().coord } : {}) });
    return {
      run, claim, go: () => run.execute(claim), ...(rig ? { rig } : {}),
      kept: () => inspectRetained(worktreeDirFor(iid), ["W4645.txt"], home.homeDir, [SESSION_FILE, "session.marker"]),
      seeded: (): Seeded => rig
        ? {
          head: committed, files: { "W4645.txt": "work before the hold\n" },
          session: { [SESSION_FILE]: SESSION_TEXT, "session.marker": "resume evidence\n" },
        }
        : { parent: planned, files: {}, session: { [SESSION_FILE]: SESSION_TEXT } },
      cleanup: () => fs.rmSync(home.root, { recursive: true, force: true }),
    };
  };
}

// Row 5 (plain) and its Codex variant: the walled exit of phasePublish after the park ACK.
function wallPark(withCodex: boolean): Builder {
  return async (iid, logger, outbox) => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-4645-wall-"));
    const homeDir = path.join(root, "h");
    api.setWallParkResponse("paused");
    const claim = gitlabClaim(iid);
    const rig = withCodex ? codexRig({ authMode: "subscription" }) : undefined;
    let committed: string | undefined;
    const behavior = async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.mkdirSync(homeDir, { recursive: true });
      fs.writeFileSync(path.join(homeDir, "session.marker"), "resume me\n");
      commitInTree(ctx.worktreePath, "WORK.txt", "work before the wall\n");
      committed = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      await ctx.checkpoint?.({ reap: false });
      const outcome = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
      assert.equal(outcome, "parked");
      return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
    };
    const executor: Executor = rig ? new FakeCodexExecutor(rig.safety, behavior, undefined, rig.capture) : { run: behavior };
    const run = runnerWith(() => ({ executor, homeDir }), gitlab, undefined, logger, { outbox, ...(rig ? { recovery: enabledRecovery().coord } : {}) });
    return {
      run, claim, go: () => run.execute(claim), ...(rig ? { rig } : {}),
      kept: () => inspectRetained(worktreeDirFor(iid), ["WORK.txt"], homeDir, ["session.marker"]),
      seeded: () => ({ head: committed!, files: { "WORK.txt": "work before the wall\n" }, session: { "session.marker": "resume me\n" } }),
      cleanup: () => { restore(); fs.rmSync(root, { recursive: true, force: true }); },
    };
  };
}

// Row 3: the phase-switch hold; the held arm emits nothing between the flush and the close, so the injection supplies the frame.
const phaseSwitchHold: Builder = async (iid, logger, outbox) => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid, { plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null, claim_generation: 1 });
  const executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => ({
      branch: ctx.branch,
      phaseSwitchHeld: {
        cause: "model_mismatch" as const, phase: "implement" as const,
        requested: { model: "sonnet", effort: "medium" }, observed: { model: "claude-opus-5-5", effort: "medium" },
      },
    }),
    closeAndSettlePlanSummary: async () => {},
    killAgentTree: () => {},
  };
  const original = client.requestPhaseSwitchHold.bind(client);
  (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = async () => ({ outcome: "held", status: "paused" });
  const home = seedHome();
  const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  const run = runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, logger, { recoveryRetryMs: 5, phaseSwitchHoldDeadlineMs: 3_000, outbox });
  return {
    run, claim, go: () => run.execute(claim),
    kept: () => inspectRetained(worktreeDirFor(iid), [], home.homeDir, [SESSION_FILE]),
    seeded: () => ({ head: planned, files: {}, session: { [SESSION_FILE]: SESSION_TEXT } }),
    cleanup: () => {
      (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = original;
      fs.rmSync(home.root, { recursive: true, force: true });
    },
  };
};

// Row 1: the base-attestation preflight hold. Leg 1 parks at its wall; leg 2 is a fresh worker's continuation without an attestation.
const preflightHold: Builder = async (iid, logger, outbox) => {
  const branch = `agent/issue-${iid}`;
  const { gitlab } = fakeGitlab();
  const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  const legs: RunContext[] = [];
  const home = seedHome();
  let committed: string | undefined;
  const factory: ExecutorFactory = () => ({
    homeDir: home.homeDir,
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        legs.push(ctx);
        fs.writeFileSync(path.join(ctx.worktreePath, `leg${legs.length}.txt`), `leg ${legs.length}\n`);
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", `leg ${legs.length}`]);
        committed = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
        if (legs.length === 1) {
          await ctx.parkForWall?.({ completedCount: 0 });
          return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
        }
        return { branch: ctx.branch };
      },
    },
  });
  const claim = gitlabClaim(iid, {
    plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null,
    planned_base_commit: planned, require_base_match: true, claim_generation: 1,
  });
  const origHold = client.requestBaseAttestationHold.bind(client);
  (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = async (): Promise<BaseAttestationHoldOutcome> =>
    ({ outcome: "held", status: "paused" });
  const origPublish = client.publishCheckpoint.bind(client);
  let tip: string | undefined;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, tipOid: string, pack: Readable) => {
    const chunks: Buffer[] = [];
    for await (const c of pack) chunks.push(Buffer.from(c as Buffer));
    execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], { env: GIT_ENV, input: Buffer.concat(chunks) });
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tipOid]);
    tip = tipOid;
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  const freshDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-4645-preflight-"));
  const freshGit = new GitCache(freshDir, nullLogger(), undefined, testGitCacheOptions());
  const resumed = gitlabClaim(iid, { ...claim, claim_generation: 2, checkpoint_tip: null, checkpoint_attested_source: null });
  const run = new RunRunner(client, freshGit, factory, logger, 20, undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, recoveryRetryMs: 5, gitlab, outbox });
  const cloneDirs = (): string[] => {
    const runnerRoot = path.join(freshDir, "runner");
    return fs.existsSync(runnerRoot) ? fs.readdirSync(runnerRoot).flatMap(repo =>
      fs.readdirSync(path.join(runnerRoot, repo)).filter(d => d === `issue-${iid}` || d.startsWith(`issue-${iid}.`)).map(d => path.join(runnerRoot, repo, d))) : [];
  };
  return {
    run, claim: resumed,
    go: async () => {
      // Leg 1 runs on the harness worker, uninstrumented; the observer is attached only to the leg-2 runner.
      const leg1Observer = current;
      current = undefined;
      await runnerWith(factory, gitlab).execute(claim);
      assert.equal(legs.length, 1, "sanity: leg 1 ran");
      const resumedClaim = { ...resumed, checkpoint_tip: tip ?? null, last_seq: Math.max(0, ...api.messages(claim.run_id).map(m => m.seq)) };
      api.setOwnershipStatus(claim.run_id, "running", 2);
      current = leg1Observer;
      await run.execute(resumedClaim);
      assert.equal(legs.length, 1, "the unattested continuation never ran the executor");
    },
    kept: () => {
      const dir = cloneDirs().find(d => fs.existsSync(path.join(d, "leg1.txt")) && fs.existsSync(path.join(d, ".git")));
      return inspectRetained(dir, ["leg1.txt"], home.homeDir, [SESSION_FILE]);
    },
    seeded: () => ({ head: committed!, files: { "leg1.txt": "leg 1\n" }, session: { [SESSION_FILE]: SESSION_TEXT } }),
    cleanup: () => {
      fs.rmSync(home.root, { recursive: true, force: true });
      (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = origHold;
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = origPublish;
      fs.rmSync(freshDir, { recursive: true, force: true });
    },
  };
};

interface Outcome {
  obs: Observer;
  runId: string;
  outbox: Outbox;
  lines: unknown[];
  retained: Retained;
  seeded: Seeded;
  /** Compared with deepEqual between a tripped run and its no-trip control; per-run values (iid, paths, commit shas) are normalised. */
  world: {
    statuses: string[];
    kept: {
      clone: boolean; home: boolean; headIsSeeded: boolean; tree: string | undefined; paths: string[];
      files: Record<string, string>; session: Record<string, string>;
    };
    custody: Record<string, boolean | undefined>;
    dispose: string[] | undefined;
  };
}

const failedReports = (runId: string) => api.states.filter(s => s.runId === runId && s.body.status === "failed");

async function execRow(t: TestContext, build: Builder, iid: number, trip: boolean, opts: { memoryOverlap?: boolean } = {}): Promise<Outcome> {
  current = undefined;
  const { logger, lines } = recordingLogger();
  const outbox = await mkOutbox(String(iid));
  const built = await build(iid, logger, outbox);
  const obs = observe(t, built.run, trip, built.rig, opts.memoryOverlap === true);
  let kept!: Retained;
  let seeded!: Seeded;
  try {
    await built.go();
    await Promise.allSettled(obs.handlerPromises);
    await obs.batcher?.awaitPermanentFailureSettled();
    kept = built.kept();
    seeded = built.seeded();
  } finally {
    built.cleanup();
    current = undefined;
  }
  const f = obs.flight;
  return {
    obs, runId: built.claim.run_id, outbox, lines, retained: kept, seeded,
    world: {
      statuses: api.states.filter(s => s.runId === built.claim.run_id).map(s => String(s.body.status)),
      kept: {
        clone: kept.clone, home: kept.home, headIsSeeded: headMatchesSeed(kept, seeded),
        // The tree sha is comparable only when the test knows the exact commit; the StubExecutor's own commit embeds per-run content.
        tree: seeded.head !== undefined ? kept.tree : undefined, paths: kept.paths,
        files: kept.files, session: kept.session,
      },
      custody: {
        preserveRecoveryClone: f?.preserveRecoveryClone, preserveSession: f?.preserveSession,
        retainedLocalCustody: f?.retainedLocalCustody, keepGuardedInventoryOpen: f?.keepGuardedInventoryOpen,
        parked: f?.parked, confirmed: f?.holdOrWallParkConfirmed,
      },
      dispose: built.rig ? [...built.rig.disposeBoundaries] : undefined,
    },
  };
}

function assertControl(c: Outcome): void {
  assert.equal(c.obs.entries.length, 0, "the control never tripped the breaker");
  assert.equal(failedReports(c.runId).length, 0, "the control reported no failed");
  assert.equal(c.world.custody.confirmed, true, "the control's disposition was confirmed");
  assertRetainedContent("control", c);
  assert.equal(c.obs.terminalInstalls, 0, "the control installed no terminal");
}

/** The retained source holds the seeded content itself: the clone's HEAD sha and work files, the HOME dir and its session files. */
function assertRetainedContent(label: string, out: Outcome): void {
  const { retained: r, seeded: s } = out;
  assert.equal(r.clone, true, `${label} keeps its clone`);
  assert.equal(r.home, true, `${label} keeps its HOME dir`);
  assert.equal(headMatchesSeed(r, s), true, `${label}: the clone's HEAD is the seeded commit (head ${r.head}, parent ${r.parent}; seeded ${JSON.stringify({ head: s.head, parent: s.parent })})`);
  assert.deepEqual(r.files, s.files, `${label}: the retained work files hold their seeded content`);
  assert.deepEqual(r.session, s.session, `${label}: the HOME session files hold their seeded content`);
}

/** The shared assertions of every confirmed row: the trip happened after the flag, and nothing terminal followed. */
function assertKept(tripped: Outcome, control: Outcome): void {
  const o = tripped.obs;
  assert.equal(o.armed, true, "the permanent rejection was injected after the flag was set");
  assert.ok(o.queuedAfter >= 1, `a frame is queued for the final drain (queued ${o.queuedBefore} -> ${o.queuedAfter})`);
  assert.equal(o.batcher!.isTripped(), true, "the real breaker tripped");
  assert.ok(o.entries.length >= 1, "the permanent-failure handler was invoked");
  assert.deepEqual(o.entries.filter(e => !e.confirmed), [], "every handler entry saw holdOrWallParkConfirmed already true");
  assert.equal(o.terminalInstalls, 0, "journalAndSendTerminal was never called");
  assert.deepEqual(tripped.outbox.listPendingTerminals(), [], "no terminal is pending in the outbox");
  assert.equal(failedReports(tripped.runId).length, 0, "no failed report reached the API");
  assert.equal(o.abortedAfterHandler.length, o.entries.length, "every handler call settled");
  assert.deepEqual(o.abortedAfterHandler, o.entries.map(() => false), "the hook did not abort the flight");
  assert.ok(tripped.lines.some(l => (l as { level?: string; msg?: string }).level === "warn" && (l as { msg?: string }).msg === CONFIRMED_DISPOSITION_TRIP_LOG),
    "the keeping-the-disposition warning was logged");
  assert.equal(o.handlerReaps, 0, "the hook ran no terminal reap");
  assert.equal(api.messages(tripped.runId).some(m => JSON.stringify(m.payload).includes(FRAME)), false, "the injected frame was rejected, never delivered");
  assertRetainedContent("tripped", tripped);
  assert.deepEqual(tripped.world, control.world, "retained clone, HOME, session and custody match the no-trip control");
}

function assertCodexQuiet(tripped: Outcome, control: Outcome): void {
  for (const [label, out] of [["control", control], ["tripped", tripped]] as const) {
    const rig = out.obs.rig!;
    const base = out.obs.codexBaseline;
    assert.ok(base, `${label}: the confirmation baseline was taken`);
    assert.deepEqual(rig.boundaries.slice(base.boundaries), [], `${label}: no withBoundary of any name after the confirmation`);
    assert.equal(rig.refreshCalls(), base.refresh, `${label}: no credential refresh after the confirmation`);
    assert.equal(rig.releaseCalls(), base.release, `${label}: no credential release after the confirmation`);
    assert.equal(rig.processSpawns(), base.spawns, `${label}: no Codex process spawn after the confirmation`);
  }
  assert.deepEqual(tripped.world.dispose, control.world.dispose, "the registry disposal matches the no-trip control");
}

describe("a permanent message failure after a confirmed hold or wall park keeps the accepted disposition (issue #4645)", () => {
  const rows: Array<[string, Builder, number, boolean]> = [
    ["A1 preflight hold (missing base attestation)", preflightHold, 46450, false],
    ["A2 completion hold", completionHold, 46452, false],
    ["A3 phase-switch hold", phaseSwitchHold, 46454, false],
    ["A4 interlocked completion hold", interlockedHold(false), 46456, false],
    ["A5 wall park", wallPark(false), 46458, false],
    ["C1 interlocked completion hold, Codex", interlockedHold(true), 46460, true],
    ["C2 wall park, Codex", wallPark(true), 46462, true],
  ];
  for (const [name, build, iid, codex] of rows) {
    it(`${name}: a trip after the confirmation reports no failed and keeps the disposition`, async t => {
      installCloseHook(t);
      const control = await execRow(t, build, iid, false);
      assertControl(control);
      const tripped = await execRow(t, build, iid + 1, true);
      if (codex) assertCodexQuiet(tripped, control);
      assertKept(tripped, control);
    });
  }

  it("B memory overlap: workerMemory.hold keeps precedence over a confirmed hold (retain custody, RetainedRecoveryStop, no failed)", async t => {
    installCloseHook(t);
    const o = await execRow(t, completionHold, 46470, true, { memoryOverlap: true });
    assert.equal(o.obs.armed, true);
    assert.equal(o.obs.batcher!.isTripped(), true, "the real breaker tripped");
    assert.ok(o.obs.entries.length >= 1, "the handler ran");
    assert.ok(o.obs.entries.every(e => e.confirmed), "after the confirmation");
    assert.ok(o.obs.abortedAfterHandler.every(a => a), "the memory branch aborted the flight");
    assert.ok(o.obs.stopReasonAfterHandler instanceof RetainedRecoveryStop, "with RetainedRecoveryStop");
    assert.equal(o.world.custody.preserveRecoveryClone, true);
    assert.equal(o.world.custody.preserveSession, true);
    assert.equal(o.world.custody.retainedLocalCustody, true);
    assert.equal(o.world.custody.keepGuardedInventoryOpen, true);
    assert.equal(o.obs.terminalInstalls, 0, "no terminal install");
    assert.equal(failedReports(o.runId).length, 0, "no failed report");
    assert.equal(o.obs.handlerReaps, 0, "no handler reap");
    assert.deepEqual(o.outbox.listPendingTerminals(), []);
    // Memory reconciliation ownership: the handler neither replaced nor cleared the flight's memory, and the flight's own
    // reconciliation still ran for that same object, outside the handler.
    const memory = o.obs.memoryStandIn;
    const hold = o.obs.memoryHold;
    assert.equal(o.obs.flight!.workerMemory, memory, "the flight still holds the same memory object after settlement");
    assert.equal((o.obs.flight!.workerMemory as { hold?: unknown }).hold, hold, "the memory hold is still set after settlement");
    assert.deepEqual(o.obs.memoryAfterHandlerCall.map(m => m.memory === memory && m.hold === hold), o.obs.entries.map(() => true),
      "the handler left flight.workerMemory as the same object with its hold set");
    assert.ok(o.obs.reconciles.length >= 1, "the flight's own memory reconciliation ran");
    assert.deepEqual(o.obs.reconciles.filter(c => c.memory !== memory || c.hold !== hold), [],
      "every reconciliation saw the same memory object with its hold set");
    assert.deepEqual(o.obs.reconciles.filter(c => c.inHandler > 0), [], "the handler did not reconcile memory itself");
  });

  it("D ordinary control: a real-breaker trip on a non-held flight still installs failed, aborts and reaps", async t => {
    const { gitlab } = fakeGitlab();
    const outbox = await mkOutbox("d");
    const { logger } = recordingLogger();
    const claim = gitlabClaim(46480, { claim_generation: 9 });
    const executor: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        api.failMessagesNext(100, 401);
        ctx.emit({ kind: "text", agent: "lead", payload: { text: "hello" } });
        await new Promise<void>((_resolve, reject) => {
          if (ctx.signal?.aborted) return reject(new Error("aborted"));
          ctx.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
        return { branch: ctx.branch };
      },
    };
    const run = runnerWith(() => ({ executor }), gitlab, undefined, logger, { outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 });
    current = undefined;
    const obs = observe(t, run, false);
    await run.execute(claim);
    await Promise.allSettled(obs.handlerPromises);
    current = undefined;
    assert.ok(obs.entries.length >= 1, "the handler ran");
    assert.deepEqual(obs.entries.filter(e => e.confirmed), [], "no confirmed disposition on this flight");
    assert.ok(obs.terminalInstalls >= 1, "a terminal was installed");
    assert.equal(failedReports(claim.run_id).length, 1, "exactly one failed report");
    assert.ok(obs.abortedAfterHandler.every(a => a), "the hook aborted the flight");
    assert.ok(obs.handlerReaps >= 1, "the hook ran the terminal reap");
  });
});
