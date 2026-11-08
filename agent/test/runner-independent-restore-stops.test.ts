// Issue #3644 Gate B (m4b part 2, plan 3.10): the stops of a RESTORED attempt. S is a fresh attempt under a restore entry; every
// way it can stop without finishing (usage limit, owner pause, graceful shutdown, a disk park, a credential switch releasing the
// claim) ends the claim NONTERMINALLY with S's restore entry and the predecessor P preserved, and the next claim is refused on
// the composite: it never takes the legacy retained route and writes nothing. Production RunRunner + real Git + the durable
// authority through the shared restore rig; the stop is raised from S's executor exactly as the production executors raise it.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { DiskParkSignal } from "../src/cache-cap.js";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { LimitReachedError } from "../src/limit.js";
import { parseRestoreValue } from "../src/restore-journal.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { randomUUID } from "node:crypto";
import { api, client, fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";
import {
  AUTH_TOKEN, BRANCH, CID, GIT_ENV, buildRig, cfgAll, cfgGet, cleanupRig, failed, logged, resetRigRoots, rigDefaults, snapshot, treeDigest, type Rig,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

afterEach(() => { cleanupRig(); });
beforeEach(() => { resetRigRoots(); });

const restoreKey = `uzi-restore.${BRANCH}.journal`;
const markerFile = (r: Rig): string => path.join(r.dataDir, "retained-homes", `${r.runId}.json`);
const configText = (r: Rig): string => fs.readFileSync(path.join(r.bare, "config"), "utf8");

interface StopCase {
  name: string;
  /** What S's executor does once it is running. */
  stop: (r: Rig, ctx: RunContext) => Promise<ExecutorResult>;
  /** Claim fields the stop needs (the pause mode the owner or the sweep set). */
  claim?: Partial<ClaimResponse>;
}

/** S commits work and checkpoints it, as a running attempt does before a park. */
function work(ctx: RunContext): void {
  fs.writeFileSync(path.join(ctx.worktreePath, "S_WORK.txt"), "work of the restored attempt\n");
  execFileSync("git", ["-C", ctx.worktreePath, "add", "S_WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "s work"],
    { env: GIT_ENV, stdio: "pipe" });
}

const pause = async (_r: Rig, ctx: RunContext): Promise<ExecutorResult> => {
  work(ctx);
  // The park publishes its checkpoint first and reports `paused` only if that landed (PRD #1190 Decision 8).
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk);
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${BRANCH}` } };
  };
  await ctx.checkpoint?.({ reap: false });
  const at = { completedCount: 1, total: 3 };
  const parked = await ctx.parkForPause?.(at);
  return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
};

const cases: StopCase[] = [
  { name: "usage limit (limit_wait)", stop: async () => { throw new LimitReachedError({ resetsAtMs: Date.now() + 3_600_000, rateLimitType: "five_hour" }); } },
  { name: "owner pause (the loop's parkForPause)", stop: pause },
  { name: "wall-clock budget park (pause mode wall)", stop: pause, claim: { pause_pending: true, pause_mode: "wall" } },
  { name: "disk park", stop: async () => { throw new DiskParkSignal(false); } },
  { name: "credential switch releasing the claim", stop: async () => { throw new CredentialSwitchSignal(); } },
  { name: "graceful shutdown", stop: async (r, ctx) => {
    r.runner.shutdown();
    await new Promise<void>(resolve => { if (ctx.signal?.aborted) resolve(); else ctx.signal?.addEventListener("abort", () => resolve(), { once: true }); });
    throw new Error("aborted by shutdown");
  } },
];

function stoppingExecutor(holder: { rig?: Rig }, stop: StopCase["stop"]): Executor {
  const stub = new StubExecutor(nullLogger());
  const wrapped = Object.create(stub) as Executor;
  wrapped.run = async ctx => stop(holder.rig!, ctx);
  return wrapped;
}

describe("a restored attempt that stops without finishing keeps S and P and refuses the next claim (issue #3644 3.10)", () => {
  for (const c of cases) {
    it(c.name, async () => {
      const holder: { rig?: Rig } = {};
      const r = await buildRig({ addrAware: true, claim: { wait_on_limit: true, ...c.claim }, successorExecutor: stoppingExecutor(holder, c.stop) });
      holder.rig = r;
      const pBefore = { journal: cfgGet(r, r.journalKey), pLedger: cfgAll(r, r.ledgerKey)[0], home: treeDigest(r.runHome), provision: treeDigest(r.provisionDir) };
      await r.runner.execute(r.claim);
      assert.equal(logged(r, "independent restore seeded").length, 1);
      assert.equal(failed(r).length, 0, "nonterminal");
      assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, 0);
      const value = parseRestoreValue(cfgGet(r, restoreKey)!);
      assert.equal(value.kind, "entry", "S's restore entry is preserved");
      assert.equal(fs.existsSync(value.kind === "entry" ? String(value.remainder.clonePath) : ""), true, "S's clone is kept");
      assert.equal(fs.existsSync(markerFile(r)), true);
      assert.equal(cfgGet(r, r.journalKey), pBefore.journal, "P's key is never written");
      assert.equal(cfgAll(r, r.ledgerKey)[0], pBefore.pLedger);
      assert.equal(fs.existsSync(r.pPath), true, "P's clone is kept");
      assert.equal(treeDigest(r.runHome), pBefore.home, "no park reap or cache drop reached the predecessor's HOME agent-home/<runId>");
      assert.equal(treeDigest(r.provisionDir), pBefore.provision);
      // The re-claim stops on the composite and writes nothing.
      const after = await snapshot(r);
      const config = configText(r);
      api.setOwnershipStatus(r.runId, "running", 3);
      // A shut-down runner refuses every claim, so the graceful-shutdown case re-claims on a restarted worker process.
      const next = c.name === "graceful shutdown"
        ? runnerWith(() => ({ executor: r.executor, homeDir: r.runHome }), fakeGitlab().gitlab, AUTH_TOKEN, r.logs.logger, {
          authority: new AuthorityRuntime({ lifetime: r.lifetime, workerToken: AUTH_TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 }),
          codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5, restoreIdentity: { procRoot: r.sProc, hostname: () => CID.slice(0, 12) } })
        : r.runner;
      await next.execute({ ...r.claim, claim_generation: 3 });
      assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
      assert.equal(failed(r).length, 0);
      assert.equal(configText(r), config);
      assert.deepEqual((await snapshot(r)).journal, after.journal);
      assert.equal(r.factoryCalls.filter(f => f.generation === 3).length, 0, "no executor was built for the refused claim");
    });
  }
});

// ---- a park signal raised after the executor swap acts on S's executor (review NB1) -------------------------------------------

/** Every property read on the executor after `armed.on`, by name: the park sink's reaps and quiescence proofs call methods on it. */
function recordingExecutor(inner: Executor, armed: { on: boolean }, used: string[]): Executor {
  return new Proxy(inner, { get(target, prop, receiver) {
    if (armed.on && typeof prop === "string") used.push(prop);
    return Reflect.get(target, prop, receiver) as unknown;
  } });
}

describe("a park signal raised inside the restore route after the executor swap (issue #3644 review NB1)", () => {
  it("dispatchFailure's park sink runs on S's executor and never on the disposed initial one", async () => {
    const armed = { on: false };
    const initialUsed: string[] = [];
    const successorUsed: string[] = [];
    const r = await buildRig({ addrAware: true, claim: { wait_on_limit: true },
      executor: recordingExecutor(new StubExecutor(nullLogger()), armed, initialUsed),
      successorExecutor: recordingExecutor(new StubExecutor(nullLogger()), armed, successorUsed) });
    const pBefore = { journal: cfgGet(r, r.journalKey), pLedger: cfgAll(r, r.ledgerKey)[0], home: treeDigest(r.runHome), provision: treeDigest(r.provisionDir) };
    // The production guard only raises stop signals, and every other error inside the route is folded into a nonterminal route
    // stop. A park signal can therefore escape only from the guard call a catch block makes: the journal-lock step (after the
    // swap) fails, and the guard that catch calls is made to raise the usage limit.
    const runner = r.runner as unknown as { retainedLifecycleGuard(flight: { restoreExecutorSwapped?: boolean }): void };
    const original = runner.retainedLifecycleGuard.bind(runner);
    let raised = 0;
    let failing = false;
    mock.method(r.recovery(), "withRestoreJournalLock", async () => { failing = true; throw new Error("journal lock step failed after the swap"); });
    mock.method(runner, "retainedLifecycleGuard", (flight: { restoreExecutorSwapped?: boolean }) => {
      if (failing && flight.restoreExecutorSwapped === true && raised === 0) {
        raised++;
        armed.on = true;
        throw new LimitReachedError({ resetsAtMs: Date.now() + 3_600_000, rateLimitType: "five_hour" });
      }
      original(flight);
    });
    await r.runner.execute(r.claim);
    assert.equal(raised, 1, "the park signal was raised after the swap");
    assert.equal(logged(r, "independent restore seeded").length, 0, "the route never finished");
    assert.equal(failed(r).length, 0, "nonterminal");
    assert.deepEqual(initialUsed, [], "the disposed initial executor was never touched by the park: " + initialUsed.join(","));
    assert.ok(successorUsed.length > 0, "the park sink acted on S's executor");
    assert.equal(cfgGet(r, r.journalKey), pBefore.journal, "P's key is never written");
    assert.equal(cfgAll(r, r.ledgerKey)[0], pBefore.pLedger);
    assert.equal(treeDigest(r.runHome), pBefore.home, "P's HOME is untouched");
    assert.equal(treeDigest(r.provisionDir), pBefore.provision);
    assert.equal(fs.existsSync(r.pPath), true, "P's clone is kept");
  });
});
