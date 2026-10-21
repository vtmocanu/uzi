import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import type { AddressInfo } from "node:net";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import type { BaseAttestationHoldOutcome } from "../src/protocol.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith, TOKEN } from "./runner-harness.js";

installHarness();

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test"];
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
const gitInput = (dir: string, args: string[], input: Buffer): string =>
  execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", input }).trim();

// ── issue #3194 M4: the base-attestation hold ─────────────────────────────────────────────────
//
// A `--require-base` continuation that adopts its own retained work WITHOUT an attestation must
// not run the agent and must not fail: the worker asks the server to hold the run for its owner and
// keeps the clone and custody, whatever the server answers. Only the positive ACK may tell the feed
// the run is held.

/** Land every checkpoint publish in the fixture origin, as the broker does, attesting nothing. */
function landPublishes(branch: string): { tip: () => string | undefined; restore: () => void } {
  let tip: string | undefined;
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string, tipOid: string, pack: Readable,
  ) => {
    const chunks: Buffer[] = [];
    for await (const c of pack) chunks.push(Buffer.from(c as Buffer));
    gitInput(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tipOid]);
    tip = tipOid;
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  return { tip: () => tip, restore: () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; } };
}

/** The retained sources one fresh worker holds for the run: its clones (and whether each still has
 *  leg 1's work in a git checkout), the run's tracking, recovery-episode and checkpoint refs, and
 *  the owed-candidate inventory records. */
interface RetainedSnapshot {
  clones: string[];
  cloneWork: boolean[];
  refs: Map<string, string>;
  owed: string[];
}

type HoldStub = (runId: string, args: { head: string; claimGeneration: number }) => Promise<BaseAttestationHoldOutcome>;

interface ContinuationOptions {
  /** Replace the client's hold request; absent = the real HTTP call (the fake api has no route: 404). */
  hold?: HoldStub;
  /** Reclaim cycles after leg 1, each a fresh claim generation (2, 3, ...) on the same fresh worker. */
  cycles?: number;
  /** The runner's completionHoldReconcileTimeoutMs: the one deadline the whole reconcile spends. */
  reconcileMs?: number;
  /** What the ownership read answers for the cycle at `generation`; default running at that generation. */
  ownership?: (runId: string, generation: number) => void;
}

/** Leg 1 commits work and parks at its wall; each later cycle is the next claim WITHOUT an
 *  attestation, as the server serves it after a requeue (the same run, the next generation). */
async function unattestedContinuation(iid: number, opts: ContinuationOptions = {}) {
  const cycles = opts.cycles ?? 1;
  const branch = `agent/issue-${iid}`;
  const { gitlab } = fakeGitlab();
  const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  const legs: RunContext[] = [];
  const factory: ExecutorFactory = () => ({
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        legs.push(ctx);
        fs.writeFileSync(path.join(ctx.worktreePath, `leg${legs.length}.txt`), `leg ${legs.length}\n`);
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", `leg ${legs.length}`]);
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
  const requests: { runId: string; head: string; claimGeneration: number }[] = [];
  const origHold = client.requestBaseAttestationHold.bind(client);
  (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = async (
    runId: string, args: { head: string; claimGeneration: number },
  ) => {
    requests.push({ runId, ...args });
    return opts.hold ? opts.hold(runId, args) : origHold(runId, args);
  };
  // The continuations run on a FRESH worker, so the first held flight's clone is seeded by that
  // flight alone (a same-worker reclaim would adopt leg 1's retained clone, kept by leg 1's park).
  // Record the clones they seed, so the assertions read the clone a held flight itself adopted.
  const freshDir = fs.mkdtempSync(path.join(os.tmpdir(), "cdr-3194-hold-"));
  const freshGit = new GitCache(freshDir, nullLogger(), undefined, testGitCacheOptions());
  const clonePaths: string[] = [];
  for (const name of ["runnerCloneForBranch", "createOrAttachRunnerClone"] as const) {
    const g = freshGit as unknown as Record<string, (...a: unknown[]) => Promise<{ path: string }>>;
    const real = g[name]!.bind(freshGit);
    g[name] = async (...a: unknown[]) => {
      const out = await real(...a);
      clonePaths.push(out.path);
      return out;
    };
  }
  const landed = landPublishes(branch);
  let leg2States: typeof api.states = [];
  /** Per reclaim cycle: every retained source on the fresh worker after that cycle. */
  const retained: RetainedSnapshot[] = [];
  const snapshot = (): RetainedSnapshot => {
    const runnerRoot = path.join(freshDir, "runner");
    const clones = fs.existsSync(runnerRoot) ? fs.readdirSync(runnerRoot).flatMap((repo) =>
      fs.readdirSync(path.join(runnerRoot, repo)).filter((d) => d === `issue-${iid}` || d.startsWith(`issue-${iid}.`))
        .map((d) => path.join(runnerRoot, repo, d))) : [];
    const bare = freshGit.barePathFor(fx.originPath);
    const refs = new Map(execFileSync("git", ["--git-dir", bare, "for-each-ref", "--format=%(refname) %(objectname)"],
      { encoding: "utf8" }).trim().split("\n").filter(Boolean).map((l) => l.split(" ") as [string, string])
      .filter(([name]) => /^refs\/uzi-(runner|recovery-episode|checkpoints)\//.test(name)));
    const owedDir = path.join(bare, "uzi-owed");
    const owed = fs.existsSync(owedDir) ? fs.readdirSync(owedDir).sort() : [];
    return { clones, cloneWork: clones.map((c) => fs.existsSync(path.join(c, "leg1.txt")) && fs.existsSync(path.join(c, ".git"))), refs, owed };
  };
  try {
    await runnerWith(factory, gitlab).execute(claim);
    assert.equal(legs.length, 1, "sanity: leg 1 ran");
    const before = api.states.length;
    for (let cycle = 1; cycle <= cycles; cycle++) {
      const generation = 1 + cycle;
      const resumed = gitlabClaim(iid, {
        ...claim, claim_generation: generation, checkpoint_tip: landed.tip() ?? null, checkpoint_attested_source: null,
        // The server serves the persisted feed position, so later frames do not reuse earlier seqs.
        last_seq: Math.max(0, ...api.messages(claim.run_id).map((m) => m.seq)),
      });
      (opts.ownership ?? ((runId, gen) => api.setOwnershipStatus(runId, "running", gen)))(claim.run_id, generation);
      await new RunRunner(client, freshGit, factory, nullLogger(), 20, undefined,
        { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, recoveryRetryMs: 5, gitlab,
          ...(opts.reconcileMs !== undefined ? { completionHoldReconcileTimeoutMs: opts.reconcileMs } : {}) }).execute(resumed);
      retained.push(snapshot());
    }
    leg2States = api.states.slice(before).filter((s) => s.runId === claim.run_id);
  } finally {
    landed.restore();
    (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = origHold;
  }
  const clone = clonePaths.at(-1);
  assert.ok(clone, "sanity: the continuation seeded a runner clone");
  const kept = fs.existsSync(path.join(clone, "leg1.txt"));
  fs.rmSync(freshDir, { recursive: true, force: true });
  const feed = api.messages(claim.run_id).map((m) => JSON.stringify(m.payload ?? {}));
  return { runId: claim.run_id, legs, leg2States, requests, clone, kept, feed, retained };
}

function assertRetainedAndStopped(r: Awaited<ReturnType<typeof unattestedContinuation>>, requests = 1): void {
  assert.equal(r.legs.length, 1, "the executor was never invoked on the unattested continuation");
  const terminal = r.leg2States.filter((s) => ["failed", "completed", "cancelled"].includes(s.body.status));
  assert.deepEqual(terminal.map((s) => s.body.status), [], "no terminal state was reported");
  assert.equal(r.requests.length, requests, "the hold request count is bounded");
  assert.ok(r.requests.every((q) => q.claimGeneration === 2), "every request is fenced by this flight's claim generation");
  assert.match(r.requests[0]!.head, /^[0-9a-f]{40}$/, "the request names the adopted source");
  assert.ok(r.kept, `the held flight's clone with the retained work is kept: ${r.clone}`);
}

/** The held report the feed carries only after the server's positive ACK or ownership proof. */
const HELD = "The agent did not start and this worker keeps the work";

function assertNotReportedHeld(r: Awaited<ReturnType<typeof unattestedContinuation>>): void {
  assert.ok(!r.feed.some((t) => t.includes(HELD)), "no held report without the ACK");
  assert.ok(r.feed.some((t) => t.includes("The hold could not be confirmed")), "the feed says the hold is unconfirmed");
}

const unknown: HoldStub = async () => ({ outcome: "unknown", detail: "socket hang up" });

describe("--require-base base-attestation hold (issue #3194 M4)", () => {
  it("a confirmed hold stops before the executor, keeps the clone and tells the owner the next step", async () => {
    const r = await unattestedContinuation(31950, { hold: async () => ({ outcome: "held", status: "paused" }) });
    assertRetainedAndStopped(r);
    assert.ok(r.feed.some((t) => t.includes(HELD) && t.includes(`uzi run recovery ${r.runId}`) &&
      t.includes(`uzi run export ${r.runId}`)), `the feed names the hold and the recovery verbs: ${r.feed.join(" | ")}`);
  });

  it("a fence refusal (stale ownership) retains everything, never retries and never reports the run held", async () => {
    const r = await unattestedContinuation(31951, { hold: async () => ({ outcome: "stale", status: "running" }) });
    assertRetainedAndStopped(r, 1);
    assertNotReportedHeld(r);
  });

  it("a persistently unknown outcome is reconciled at most three times, then retained, never held", async () => {
    const r = await unattestedContinuation(31952, { hold: unknown });
    assertRetainedAndStopped(r, 3);
    assertNotReportedHeld(r);
  });

  it("the reconcile spends one deadline fixed at entry: a slow first answer leaves no time to retry", async () => {
    const r = await unattestedContinuation(31959, {
      reconcileMs: 200,
      hold: async () => { await new Promise((resolve) => setTimeout(resolve, 300)); return { outcome: "unknown", detail: "slow" }; },
    });
    assertRetainedAndStopped(r, 1);
    assertNotReportedHeld(r);
  });

  it("an unknown outcome whose ownership read shows this generation held counts as held", async () => {
    const r = await unattestedContinuation(31955, {
      hold: unknown,
      ownership: (runId, gen) => api.setOwnershipStatus(runId, "paused", gen, undefined, "base_attestation_missing"),
    });
    assertRetainedAndStopped(r, 1);
    assert.ok(r.feed.some((t) => t.includes(HELD)), "the ownership proof lets the feed report the hold");
  });

  it("an unknown outcome whose ownership read names a newer generation is stale: no retry, never held", async () => {
    const r = await unattestedContinuation(31956, {
      hold: unknown,
      ownership: (runId, gen) => api.setOwnershipStatus(runId, "running", gen + 1),
    });
    assertRetainedAndStopped(r, 1);
    assertNotReportedHeld(r);
  });

  it("a paused row with another hold reason is not proof of this hold", async () => {
    const r = await unattestedContinuation(31957, {
      hold: unknown,
      ownership: (runId, gen) => api.setOwnershipStatus(runId, "paused", gen, undefined, "budget_exhausted"),
    });
    assertRetainedAndStopped(r, 3);
    assertNotReportedHeld(r);
  });

  it("a hold request that throws is treated as unknown: retained, not failed", async () => {
    const r = await unattestedContinuation(31953, { hold: async () => { throw new Error("client defect"); } });
    assertRetainedAndStopped(r, 3);
    assertNotReportedHeld(r);
  });

  it("an older server without the route (real HTTP 404) neither executes nor fails the run", async () => {
    const r = await unattestedContinuation(31954);
    assertRetainedAndStopped(r, 3);
    assertNotReportedHeld(r);
  });

  // The residual: a server that never supports the hold. Each flight ends retained and non-terminal,
  // the server's missing-run requeue reclaims the run, and the next flight re-asks. RUN_MAX_REQUEUES
  // defaults to 3, so after the wall park's extend the run gets four held flights (generations 2-5)
  // before the server's over-cap disposition (m4-unknown-residual.md, server side). This pins the
  // worker half through that whole sequence.
  it("through requeue exhaustion on a server without the hold, nothing executes and every retained source survives", async () => {
    const r = await unattestedContinuation(31958, { cycles: 4 });
    assert.equal(r.legs.length, 1, "the executor was never invoked on any reclaim");
    const terminal = r.leg2States.filter((s) => ["failed", "completed", "cancelled"].includes(s.body.status));
    assert.deepEqual(terminal.map((s) => s.body.status), [], "no flight reported a terminal state");
    assert.deepEqual([...new Set(r.requests.map((q) => q.claimGeneration))], [2, 3, 4, 5], "each reclaim re-asked under its own generation");
    assert.equal(r.requests.length, 12, "each flight's attempts stay bounded at three");
    for (const gen of [2, 3, 4, 5]) {
      const heads = new Set(r.requests.filter((q) => q.claimGeneration === gen).map((q) => q.head));
      assert.equal(heads.size, 1, `generation ${gen} re-sent one request identity`);
    }
    assert.ok(!r.feed.some((t) => t.includes(HELD)), "no flight reported the run held");
    // Every source retained by an earlier flight is still there, unchanged, after every later one.
    const last = r.retained.at(-1)!;
    assert.ok(last.clones.length >= 2, `sanity: several flights retained their own clone: ${last.clones.join(", ")}`);
    assert.ok(last.refs.has(`refs/uzi-runner/agent/issue-31958`), "sanity: the tracking ref is retained");
    for (const [i, snap] of r.retained.entries()) {
      for (const later of r.retained.slice(i + 1)) {
        for (const clone of snap.clones) assert.ok(later.clones.includes(clone), `clone ${clone} survives a later reclaim`);
        for (const [ref, oid] of snap.refs) assert.equal(later.refs.get(ref), oid, `${ref} survives a later reclaim unchanged`);
        for (const record of snap.owed) assert.ok(later.owed.includes(record), `owed record ${record} survives a later reclaim`);
      }
    }
    assert.ok(last.cloneWork.every(Boolean), `every retained clone still holds the work: ${JSON.stringify(last)}`);
  });
});

describe("WorkerClient.requestBaseAttestationHold classification (issue #3194 M4)", () => {
  async function answer(
    respond: (req: http.IncomingMessage, res: http.ServerResponse, body: string) => void,
  ): Promise<{ outcome: BaseAttestationHoldOutcome; bodies: string[]; paths: string[] }> {
    const bodies: string[] = [];
    const paths: string[] = [];
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => { body += c; });
      req.on("end", () => { bodies.push(body); paths.push(req.url ?? ""); respond(req, res, body); });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const { port } = server.address() as AddressInfo;
    try {
      const c = new WorkerClient(`http://127.0.0.1:${port}`, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {} });
      const outcome = await c.requestBaseAttestationHold("run-1", { head: "a".repeat(40), claimGeneration: 7 });
      return { outcome, bodies, paths };
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  }
  const json = (res: http.ServerResponse, code: number, body: unknown) => {
    res.writeHead(code, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  };

  it("held only on a 200 naming the base-attestation hold, with the generation on the wire", async () => {
    const r = await answer((_q, res) => json(res, 200, { run: { id: "run-1", status: "paused", hold_reason: "base_attestation_missing" } }));
    assert.deepEqual(r.outcome, { outcome: "held", status: "paused" });
    assert.equal(r.paths[0], "/api/worker/runs/run-1/base-attestation-hold");
    assert.deepEqual(JSON.parse(r.bodies[0]!), { head: "a".repeat(40), claim_generation: 7 });
  });
  it("a 200 for some other paused state is not a confirmation", async () => {
    const r = await answer((_q, res) => json(res, 200, { run: { id: "run-1", status: "paused", hold_reason: "budget_exhausted" } }));
    assert.equal(r.outcome.outcome, "unknown");
  });
  it("a 409 is a stale-ownership refusal carrying the real status", async () => {
    const r = await answer((_q, res) => json(res, 409, { run: { id: "run-1", status: "running" } }));
    assert.deepEqual(r.outcome, { outcome: "stale", status: "running" });
  });
  for (const code of [404, 400, 500, 503]) {
    it(`an HTTP ${code} is unknown, never held`, async () => {
      const r = await answer((_q, res) => json(res, code, { error: "nope" }));
      assert.equal(r.outcome.outcome, "unknown");
      assert.equal(r.bodies.length, 1, "one attempt, no stripped or repeated retry");
    });
  }
  it("a dropped connection is unknown, never thrown", async () => {
    const r = await answer((req) => { req.socket.destroy(); });
    assert.equal(r.outcome.outcome, "unknown");
  });
  it("no claim generation means no request at all", async () => {
    const c = new WorkerClient("http://127.0.0.1:9", TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {} });
    const outcome = await c.requestBaseAttestationHold("run-1", { head: "", claimGeneration: 0 });
    assert.equal(outcome.outcome, "unknown");
  });
});
