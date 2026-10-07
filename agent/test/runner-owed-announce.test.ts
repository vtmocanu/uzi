import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import type { Executor } from "../src/executor.js";
import type { TrackingUpdateResult } from "../src/git.js";
import { MAX_OWED_CANDIDATES_PER_RUN } from "../src/git.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
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
    .flatMap((t) => /^Retained heads (.*?) are worker-local/.exec(t)![1]!.split(", "));
}

it("announces each retained head once across checkpoints, a confirmed reconcile and a resume", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(19241);
  claim.claim_generation = 3;
  const restore = publishAfter(3);
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
  announceOwedHeads(flight: AnnounceFlight, heads: string[]): void;
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
