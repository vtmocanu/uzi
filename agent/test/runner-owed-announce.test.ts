import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import type { Executor } from "../src/executor.js";
import type { TrackingUpdateResult } from "../src/git.js";
import { MAX_OWED_CANDIDATES_PER_RUN } from "../src/git.js";
import { noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { RunRunner } from "../src/runner.js";
import { CandidateReservationRefusedError } from "../src/batcher.js";

installHarness();

// Issue #1924: retained-head announcements are deltas, and the per-run owed-candidate cap stops
// the run through the preservation-failure path even when the refusing caller is best-effort.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8" }).trim();
}
/** Discard the branch history above the initial commit and commit different content: a rewrite. */
function rewrite(dir: string, label: string, base: string): string {
  gitIn(dir, ["reset", "--hard", "-q", base]);
  fs.writeFileSync(path.join(dir, "REWRITE.txt"), `${label}\n`);
  gitIn(dir, ["add", "REWRITE.txt"]);
  gitIn(dir, [...IDENT, "commit", "-m", label]);
  return gitIn(dir, ["rev-parse", "HEAD"]);
}
/** publishCheckpoint that leaves the first `unconfirmed` publishes unconfirmed, then confirms. */
function publishAfter(unconfirmed: number): () => void {
  const orig = client.publishCheckpoint.bind(client);
  let calls = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (...args: unknown[]) => {
    const pack = args[2] as Readable | undefined;
    if (pack) for await (const _chunk of pack) { /* let pack-objects exit before the fake ACK */ }
    return ++calls <= unconfirmed ? { ok: false, httpStatus: 500 }
      : { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; };
}
function announcedLabels(runId: string): string[] {
  return api.messages(runId)
    .map((m) => (m.payload as { text?: string }).text ?? "")
    .filter((t) => t.startsWith("Retained heads "))
    .flatMap((t) => /^Retained heads (.*?) are /.exec(t)![1]!.split(", "));
}

it("announces each retained head once across checkpoints, a confirmed reconcile and a resume", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(19241);
  claim.claim_generation = 3;
  const restore = publishAfter(9);
  const heads: string[] = [];
  try {
    const exec: Executor = {
      run: async (ctx) => {
        const base = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
        for (const label of ["w1", "w2", "w3", "w4"]) {
          heads.push(rewrite(ctx.worktreePath, label, base));
          // w1..w3 stay unconfirmed (pinned); w4 confirms, so its reconcile retains w1..w3 again.
          await ctx.checkpoint!({ reap: true });
        }
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    const r = runner(exec, gitlab);
    await r.execute(claim);
    const first = announcedLabels(claim.run_id);
    const labels = heads.slice(0, 3).map((h) => h.slice(0, 12));
    for (const label of labels) {
      assert.equal(first.filter((l) => l === label).length, 1, `head ${label} announced exactly once: ${first.join(",")}`);
    }
    // A resume of the same run on this worker re-reconciles the same pins: nothing new to say.
    const before = announcedLabels(claim.run_id).length;
    const resumed = gitlabClaim(19241);
    resumed.run_id = claim.run_id;
    resumed.claim_generation = 4;
    resumed.checkpoint_tip = heads[3];
    await r.execute(resumed).catch(() => undefined);
    assert.equal(announcedLabels(claim.run_id).length, before, "a resume announces only heads it has not announced");
  } finally {
    restore();
  }
});

for (const reap of [false, true]) {
  it(`the owed-candidate cap stops the run even though the checkpoint caller is best-effort (reap=${reap})`, async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(19242);
    claim.claim_generation = 5;
    git.fetchAgentBranch = (async (): Promise<TrackingUpdateResult> => ({ kind: "not_updated", reason: "owed_limit" })) as unknown as typeof git.fetchAgentBranch;
    let aborted: boolean | undefined;
    let continued = false;
    let clone = "";
    await runner({
      run: async (ctx) => {
        clone = ctx.worktreePath;
        fs.writeFileSync(path.join(clone, "W.txt"), "w\n");
        gitIn(clone, ["add", "W.txt"]);
        gitIn(clone, [...IDENT, "commit", "-m", "work"]);
        await ctx.checkpoint!({ reap });
        aborted = ctx.signal?.aborted;
        // An executor that ignored the refusal would keep rewriting here.
        if (!aborted) continued = true;
        throw new Error("executor unwound after abort");
      },
      killAgentTree: () => {},
    }, gitlab).execute(claim);
    assert.equal(aborted, true, "the refusal aborts the attempt instead of letting the agent continue");
    assert.equal(continued, false);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.equal(failed.length, 1);
    assert.match(failed[0]?.body.failure_reason ?? "", new RegExp(`owed-head limit \\(${MAX_OWED_CANDIDATES_PER_RUN}\\)`));
    assert.equal(fs.existsSync(clone), true, "the working clone is kept");
  });
}

it("an ordinary preservation failure at a checkpoint does not stop the run", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(19243);
  claim.claim_generation = 5;
  git.fetchAgentBranch = (async (): Promise<TrackingUpdateResult> => ({ kind: "not_updated", reason: "preservation_failed" })) as unknown as typeof git.fetchAgentBranch;
  let aborted: boolean | undefined;
  await runner({
    run: async (ctx) => {
      fs.writeFileSync(path.join(ctx.worktreePath, "W.txt"), "w\n");
      gitIn(ctx.worktreePath, ["add", "W.txt"]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "work"]);
      await ctx.checkpoint!({ reap: false });
      aborted = ctx.signal?.aborted;
      return { branch: ctx.branch };
    },
    killAgentTree: () => {},
  }, gitlab).execute(claim);
  assert.equal(aborted, false);
});

// A row's heads count as announced only after the batcher accepted that row.
type AnnounceFlight = {
  owedFeedClosed: boolean;
  owedAnnounced: { heads: Set<string>; archived: Set<string> };
  batcher: { emit(message: unknown): void };
};
const announce = (RunRunner.prototype as unknown as {
  announceOwedHeads(flight: AnnounceFlight, heads: string[], archived?: boolean): void;
}).announceOwedHeads;
const sha = (n: number): string => n.toString(16).padStart(2, "0").repeat(20);
const rowHeads = (message: unknown): string =>
  String((message as { payload: { text: string } }).payload.text);

it("a refused first announcement is re-announced on retry", () => {
  let refuse = true;
  const emitted: unknown[] = [];
  const flight: AnnounceFlight = {
    owedFeedClosed: false,
    owedAnnounced: { heads: new Set(), archived: new Set() },
    batcher: { emit(message) { if (refuse) throw new CandidateReservationRefusedError("cancelled"); emitted.push(message); } },
  };
  assert.throws(() => announce.call(null, flight, [sha(1)]), CandidateReservationRefusedError);
  assert.equal(flight.owedAnnounced.heads.size, 0, "a refused row marks nothing as announced");
  refuse = false;
  announce.call(null, flight, [sha(1)]);
  assert.equal(emitted.length, 1);
});

it("a refusal after an earlier row succeeds re-announces only the refused row", () => {
  const heads = Array.from({ length: 9 }, (_, i) => sha(i + 1)); // two rows: 8 + 1
  let calls = 0;
  let refuseSecond = true;
  const emitted: unknown[] = [];
  const flight: AnnounceFlight = {
    owedFeedClosed: false,
    owedAnnounced: { heads: new Set(), archived: new Set() },
    batcher: { emit(message) {
      calls++;
      if (refuseSecond && calls === 2) throw new CandidateReservationRefusedError("cancelled");
      emitted.push(message);
    } },
  };
  assert.throws(() => announce.call(null, flight, heads), CandidateReservationRefusedError);
  assert.equal(emitted.length, 1, "the first row was accepted");
  assert.equal(flight.owedAnnounced.heads.size, 8, "only the accepted row's heads are marked");
  refuseSecond = false;
  announce.call(null, flight, heads);
  assert.equal(emitted.length, 2, "the retry emits only the refused row");
  assert.ok(rowHeads(emitted[1]).includes(sha(9).slice(0, 12)));
  assert.ok(!rowHeads(emitted[1]).includes(sha(1).slice(0, 12)));
});

type ReconcileFlight = AnnounceFlight & {
  runId: string;
  runLog: { info(message: string, details?: unknown): void; warn(message: string, details?: unknown): void };
};
const reconcile = (RunRunner.prototype as unknown as {
  reconcileConfirmedOwed(flight: ReconcileFlight, bare: string, confirmed: string): Promise<void>;
}).reconcileConfirmedOwed;
function announcementProbe() {
  const emitted: unknown[] = [];
  const logs: string[] = [];
  const flight: ReconcileFlight = {
    runId: "owed-m2", owedFeedClosed: false,
    owedAnnounced: { heads: new Set(), archived: new Set() },
    batcher: { emit(message) { emitted.push(message); } },
    runLog: { info(message) { logs.push(message); }, warn(message) { logs.push(message); } },
  };
  return { flight, emitted, logs };
}
const resolutionRows = (emitted: unknown[]) => emitted.map(rowHeads).filter(t => t.startsWith("Confirmed checkpoint covers previously retained heads "));

it("ordinary and earlier-archive announcements describe distinct evidence", () => {
  const { flight, emitted } = announcementProbe();
  announce.call(null, flight, [sha(1)]);
  announce.call(null, flight, [sha(2)], true);
  assert.match(rowHeads(emitted[0]), /retained locally.*no confirmed checkpoint coverage/);
  assert.match(rowHeads(emitted[0]), /Inspect status with uzi run recovery or uzi run export/);
  assert.doesNotMatch(rowHeads(emitted[0]), /needs action|durable|success/i);
  assert.match(rowHeads(emitted[1]), /^Earlier recovery archive covers retained heads/);
  assert.ok(flight.owedAnnounced.archived.has(sha(2)));
});

it("real Git partial, divergent and full confirmation resolves only covered announced heads", async () => {
  const bare = await git.ensureClone(fx.originPath);
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const branch = "agent/owed-m2";
  const { flight, emitted } = announcementProbe();
  const clone = (await git.runnerCloneForBranch(bare, branch, "issue-m2", noProofReseed, flight.runId)).path;
  const context = { runId: flight.runId, generation: 1, branch, kind: "issue" as const, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } };
  const a = rewrite(clone, "a", base);
  assert.equal((await git.fetchAgentBranch(bare, clone, branch, flight.runId, { context })).kind, "updated");
  const b = rewrite(clone, "b-divergent", base);
  assert.equal((await git.fetchAgentBranch(bare, clone, branch, flight.runId, { context })).kind, "updated");
  announce.call(null, flight, [a]);
  announce.call(null, flight, [b], true);
  const target = { git, announceOwedHeads: announce };
  await reconcile.call(target, flight, bare, base);
  assert.equal(resolutionRows(emitted).length, 0, "a divergent confirmation resolves neither root");
  await reconcile.call(target, flight, bare, a);
  assert.deepEqual((await git.enumerateOwedCandidates(bare, flight.runId)).map(c => c.sha), [b]);
  assert.equal(resolutionRows(emitted).length, 1);
  assert.ok(resolutionRows(emitted)[0]!.includes(a.slice(0, 12)));
  assert.ok(!resolutionRows(emitted)[0]!.includes(b.slice(0, 12)));
  await reconcile.call(target, flight, bare, a);
  assert.equal(resolutionRows(emitted).length, 1, "repeated reconciliation is silent");
  await reconcile.call(target, flight, bare, b);
  assert.equal((await git.enumerateOwedCandidates(bare, flight.runId)).length, 0);
  assert.equal(resolutionRows(emitted).length, 2);
  assert.ok(resolutionRows(emitted)[1]!.includes(b.slice(0, 12)));
  assert.equal(flight.owedAnnounced.heads.size + flight.owedAnnounced.archived.size, 0);
});

it("resolution deduplicates valid announced heads into eight-label rows, excluding unannounced removals", async () => {
  const { flight, emitted } = announcementProbe();
  const heads = Array.from({ length: 10 }, (_, i) => sha(i + 1));
  announce.call(null, flight, heads.slice(0, 9));
  announce.call(null, flight, [heads[0]!], true);
  flight.owedAnnounced.heads.add("invalid");
  const target = {
    git: { reconcileOwedCandidates: async () => ({ removedShas: [...heads, heads[0]!, "invalid"], retainedShas: [] }) },
    announceOwedHeads: announce,
  };
  await reconcile.call(target, flight, "unused", sha(20));
  const rows = resolutionRows(emitted);
  assert.equal(rows.length, 2);
  assert.equal(rows[0]!.match(/[0-9a-f]{12}/g)?.length, 8);
  assert.equal(rows[1]!.match(/[0-9a-f]{12}/g)?.length, 1);
  assert.ok(!rows.join(" ").includes(heads[9]!.slice(0, 12)));
  assert.equal(flight.owedAnnounced.heads.size + flight.owedAnnounced.archived.size, 0);
  await reconcile.call(target, flight, "unused", sha(20));
  assert.equal(resolutionRows(emitted).length, 2, "no new durable or retry ledger");
});

for (const closed of [false, true]) {
  it(`a ${closed ? "closed feed" : "refused resolution"} clears ledgers and preserves remaining announcements`, async () => {
    const { flight, emitted, logs } = announcementProbe();
    announce.call(null, flight, [sha(1)]);
    announce.call(null, flight, [sha(1)], true);
    emitted.length = 0;
    flight.owedFeedClosed = closed;
    flight.batcher.emit = message => {
      if (closed) assert.fail("closed feed must be log-only");
      if (rowHeads(message).startsWith("Confirmed checkpoint covers")) throw new CandidateReservationRefusedError("cancelled");
      emitted.push(message);
    };
    const target = {
      git: { reconcileOwedCandidates: async () => ({ removedShas: [sha(1)], retainedShas: [sha(2)] }) },
      announceOwedHeads: announce,
    };
    await reconcile.call(target, flight, "unused", sha(20));
    assert.ok(!flight.owedAnnounced.heads.has(sha(1)));
    assert.ok(!flight.owedAnnounced.archived.has(sha(1)));
    assert.equal(resolutionRows(emitted).length, 0);
    assert.ok(logs.some(t => /resolution.*not delivered/.test(t)));
    assert.ok(!logs.some(t => /reconciliation failed/.test(t)));
    if (!closed) assert.ok(emitted.map(rowHeads).some(t => t.includes(sha(2).slice(0, 12))));
    else assert.ok(logs.some(t => /retained heads after feed close/.test(t)));
  });
}

it("refused resolution and throwing diagnostics do not block later rows or remaining heads", async () => {
  const { flight, emitted } = announcementProbe();
  const heads = Array.from({ length: 9 }, (_, i) => sha(i + 1));
  announce.call(null, flight, heads);
  emitted.length = 0;
  let refused = false;
  flight.batcher.emit = message => {
    if (!refused) {
      refused = true;
      throw new CandidateReservationRefusedError("cancelled");
    }
    emitted.push(message);
  };
  flight.runLog.info = () => { throw new Error("diagnostic unavailable"); };
  const target = {
    git: { reconcileOwedCandidates: async () => ({ removedShas: heads, retainedShas: [sha(10)] }) },
    announceOwedHeads: announce,
  };
  await reconcile.call(target, flight, "unused", sha(20));
  assert.equal(resolutionRows(emitted).length, 1, "the second row is still attempted");
  assert.ok(resolutionRows(emitted)[0]!.includes(sha(9).slice(0, 12)));
  assert.ok(!resolutionRows(emitted)[0]!.includes(sha(1).slice(0, 12)));
  assert.deepEqual([...flight.owedAnnounced.heads], [sha(10)]);
  assert.ok(emitted.map(rowHeads).some(t => t.startsWith("Retained heads ") && t.includes(sha(10).slice(0, 12))));
});
