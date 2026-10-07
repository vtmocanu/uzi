import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Executor } from "../src/executor.js";
import type { FetchAgentBranchOptions, TrackingUpdateResult } from "../src/git.js";
import { LimitReachedError } from "../src/limit.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, deferred } from "./runner-harness.js";

installHarness();

function commit(dir: string): void {
  fs.writeFileSync(path.join(dir, "OWED.txt"), "owed work\n");
  execFileSync("git", ["-C", dir, "add", "OWED.txt"]);
  execFileSync("git", ["-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", "owed work"]);
}

function forbidPublication(): { packs: () => number; brokers: () => number } {
  let packs = 0;
  let brokers = 0;
  git.checkpointPack = async () => { packs++; throw new Error("unexpected pack"); };
  client.publishCheckpoint = async () => { brokers++; throw new Error("unexpected broker"); };
  return { packs: () => packs, brokers: () => brokers };
}

for (const reason of ["preservation_failed", "owner_stamp_failed", "receipt_commit_failed", "ownership_unknown"] as const) {
  it(`checkpoint refusal ${reason} stops publication and retries an unchanged tip`, async () => {
    const claim = gitlabClaim(1924);
    claim.claim_generation = 7;
    const { gitlab } = fakeGitlab();
    const publication = forbidPublication();
    let fetches = 0;
    let clone = "";
    let options: FetchAgentBranchOptions | undefined;
    git.fetchAgentBranch = (async (_bare, _clone, _branch, _runId, opts) => {
      fetches++;
      options = opts;
      return { kind: "not_updated", reason };
    }) as typeof git.fetchAgentBranch;
    const exec: Executor = {
      run: async (ctx) => {
        clone = ctx.worktreePath;
        commit(clone);
        await ctx.checkpoint!({ reap: false });
        await ctx.checkpoint!({ reap: false });
        assert.equal(fetches, 2, "refusal must not satisfy the unchanged-tip shortcut");
        assert.equal(publication.packs(), 0);
        assert.equal(publication.brokers(), 0);
        assert.equal(api.states.some(s => s.body.status === "running" && s.body.checkpoint_contains_latest === true), false);
        throw new Error("stop after checkpoint assertions");
      },
    };
    await runner(exec, gitlab).execute(claim);
    assert.equal(fs.existsSync(clone), true, "refused preservation retains the source clone");
    assert.equal(options?.context.generation, 7);
    assert.equal(options?.context.kind, "issue");
    assert.equal(options?.context.defaultIdentity.ref, "refs/remotes/origin/main");
    assert.match(options?.context.defaultIdentity.sha ?? "", /^[0-9a-f]{40}$/);
    assert.equal(options?.remotelyConfirmedSha, undefined);
  });
}

it("same SHA with a lost owner stamp fetches again and never publishes an unowned head", async () => {
  const claim = gitlabClaim(1925);
  claim.claim_generation = 8;
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  const realFetch = git.fetchAgentBranch.bind(git);
  let fetches = 0;
  git.fetchAgentBranch = (async (...args: Parameters<typeof realFetch>): Promise<TrackingUpdateResult> => {
    fetches++;
    if (fetches === 1) {
      const result = await realFetch(...args);
      assert.equal(result.kind, "updated");
      execFileSync("git", ["-C", args[0], "config", "--local", "--unset-all", `uzi-trackowner.${args[2]}.owner`]);
      return { kind: "not_updated", reason: "owner_stamp_failed" };
    }
    return { kind: "not_updated", reason: "ownership_unknown" };
  }) as typeof git.fetchAgentBranch;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      await ctx.checkpoint!({ reap: false });
      assert.equal(fetches, 2, "equal tracking and clone SHAs cannot bypass the missing committed owner");
      assert.equal(publication.packs(), 0);
      assert.equal(publication.brokers(), 0);
      throw new Error("stop");
    },
  }, gitlab).execute(claim);
});

for (const sink of ["finalize", "limit-park", "shutdown"] as const) {
  it(`${sink} refuses preservation without packing stale tracking work`, async () => {
    const claim = gitlabClaim(1926, { wait_on_limit: true });
    const { gitlab } = fakeGitlab();
    const publication = forbidPublication();
    git.fetchAgentBranch = (async (): Promise<TrackingUpdateResult> => ({ kind: "not_updated", reason: "preservation_failed" })) as unknown as typeof git.fetchAgentBranch;
    const started = deferred();
    let clone = "";
    const r = runner({
      run: async ctx => {
        clone = ctx.worktreePath;
        commit(clone);
        if (sink === "limit-park") throw new LimitReachedError({ resetsAtMs: Date.now() + 3600_000 });
        if (sink === "shutdown") {
          started.resolve();
          await new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
          throw new Error("shutdown");
        }
        return { branch: ctx.branch };
      },
    }, gitlab);
    const execution = r.execute(claim);
    if (sink === "shutdown") {
      await started.promise;
      r.shutdown();
    }
    await execution;
    assert.equal(publication.packs(), 0);
    assert.equal(publication.brokers(), 0);
    assert.equal(fs.existsSync(clone), true);
    assert.equal(api.states.some(s => s.body.status === "completed"), false);
    assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
    if (sink === "limit-park") assert.equal(api.states.some(s => s.body.status === "limit_wait"), true);
  });
}

it("ownership becoming unknown after a successful fetch skips the checkpoint and continues the run", async () => {
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  const originalOwnership = git.committedTrackingOwnership.bind(git);
  let proved = 0;
  git.committedTrackingOwnership = async (...args) => {
    const owned = await originalOwnership(...args);
    if (owned.kind === "owned" && ++proved > 1) return { kind: "not_owned" };
    return owned;
  };
  let returned = false;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      returned = true;
      throw new Error("stop");
    },
  }, gitlab).execute(gitlabClaim(1928));
  assert.equal(proved, 2);
  assert.equal(returned, true);
  assert.equal(publication.packs(), 0);
  assert.equal(publication.brokers(), 0);
});

it("an ordinary fetch error remains best effort at a checkpoint", async () => {
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  git.fetchAgentBranch = (async () => { throw new Error("ordinary fetch failure"); }) as typeof git.fetchAgentBranch;
  let returned = false;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      returned = true;
      throw new Error("stop");
    },
  }, gitlab).execute(gitlabClaim(1927));
  assert.equal(returned, true);
  assert.equal(publication.packs(), 0);
  assert.equal(publication.brokers(), 0);
});


for (const boundary of ["steer", "bridge"] as const) {
  it(`${boundary} uses the exact owned H when the shared branch moves after its proof`, async () => {
    const claim = gitlabClaim(1933);
    claim.claim_generation = 7;
    const branch = "agent/issue-1933";
    const { gitlab } = fakeGitlab();
    const base = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
    commit(fx.originPath);
    gitIn(fx.originPath, ["checkout", "-q", "main"]);
    const ownership = git.committedTrackingOwnership.bind(git);
    const realBridge = git.bridgeToFloors.bind(git);
    let h = "";
    let f = "";
    let bare = "";
    let proofs = 0;
    let bridgeSource = "";
    let safetySteer: string | undefined;
    let continued = false;
    git.committedTrackingOwnership = async (...args) => {
      const proof = await ownership(...args);
      if (proof.kind === "owned" && ++proofs === (boundary === "steer" ? 2 : 3)) {
        assert.equal(proof.sha, h);
        bare = args[0];
        f = await promoteForeign(bare, branch, h);
      }
      return proof;
    };
    git.bridgeToFloors = async (...args) => {
      bridgeSource = args[1];
      return realBridge(...args);
    };
    await runner({ run: async ctx => {
      gitIn(ctx.worktreePath, ["reset", "--hard", base]);
      fs.writeFileSync(path.join(ctx.worktreePath, "H.txt"), "owned rewrite\n");
      gitIn(ctx.worktreePath, ["add", "H.txt"]);
      gitIn(ctx.worktreePath, ["-c", "user.name=test", "-c", "user.email=test@example.com",
        "-c", "commit.gpgsign=false", "commit", "-m", "owned rewrite"]);
      h = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      await ctx.checkpoint!({ reap: false });
      continued = true;
      safetySteer = ctx.pullSafetySteer?.();
      throw new Error("stop after proof race");
    } }, gitlab).execute(claim);
    assert.equal(continued, true);
    assert.ok(f, "promotion ran between proof and projection");
    assert.ok(safetySteer?.includes(h), "the safety steer names owned H");
    assert.ok(!safetySteer?.includes(f), "the safety steer must not name foreign F");
    if (boundary === "bridge") {
      assert.equal(bridgeSource, h);
      const adopted = gitIn(bare, ["rev-parse", "refs/uzi-runner/" + branch]);
      assert.equal(gitIn(bare, ["rev-parse", adopted + "^{tree}"]),
        gitIn(bare, ["rev-parse", h + "^{tree}"]));
      assert.notEqual(adopted, f);
    } else assert.equal(bridgeSource, "", "foreign ownership refuses the later bridge");
    const candidates = await git.enumerateOwedCandidates(bare, claim.run_id);
    assert.ok(candidates.some(c => c.sha === h));
    assert.ok(candidates.every(c => c.sha !== f));
    assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
  });
}

it("an ordinary later fetch failure can still publish the older owned tip", async () => {
  const claim = gitlabClaim(1934);
  claim.claim_generation = 7;
  const { gitlab } = fakeGitlab();
  const realFetch = git.fetchAgentBranch.bind(git);
  let fetches = 0;
  let h = "";
  let newer = "";
  let continued = false;
  let clock = Date.now();
  const tips: string[] = [];
  git.fetchAgentBranch = async (...args) => {
    if (++fetches > 1) throw new Error("ordinary later fetch error");
    return realFetch(...args);
  };
  client.publishCheckpoint = async (_run, tip, pack) => {
    tips.push(tip);
    for await (const _chunk of pack) {}
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/older" } };
  };
  await runner({ run: async ctx => {
    commit(ctx.worktreePath);
    h = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    await ctx.checkpoint!({ reap: false });
    fs.writeFileSync(path.join(ctx.worktreePath, "NEWER.txt"), "newer work\n");
    gitIn(ctx.worktreePath, ["add", "NEWER.txt"]);
    gitIn(ctx.worktreePath, ["-c", "user.name=test", "-c", "user.email=test@example.com",
      "-c", "commit.gpgsign=false", "commit", "-m", "newer work"]);
    newer = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    await ctx.checkpoint!({ reap: false });
    continued = true;
    throw new Error("stop after older-tip checkpoint");
  } }, gitlab, undefined, { checkpointIntervalMs: 1, now: () => clock += 1000 }).execute(claim);
  assert.equal(continued, true);
  assert.notEqual(newer, h);
  assert.deepEqual(tips, [h, h]);
});

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
}

async function promoteForeign(bare: string, branch: string, h: string): Promise<string> {
  const tree = gitIn(bare, ["rev-parse", h + "^{tree}"]);
  const f = gitIn(bare, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", h, "-m", "foreign work"]);
  const result = await git.updateTrackingRef(bare, branch, f, { context: {
    runId: "foreign-run", generation: 8, branch, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main",
      sha: gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]) },
  } });
  assert.equal(result.kind, "updated", JSON.stringify(result));
  return f;
}

for (const sink of ["limit-park", "scanned-checkpoint"] as const) {
  it(`${sink} uploads H after a real foreign promotion between proof and pack`, async () => {
    const claim = gitlabClaim(1930, { wait_on_limit: true });
    claim.claim_generation = 7;
    const { gitlab } = fakeGitlab();
    const realPack = git.checkpointPack.bind(git);
    const branch = "agent/issue-1930";
    let scannedFloor = "";
    if (sink === "scanned-checkpoint") {
      gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
      commit(fx.originPath);
      scannedFloor = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
      gitIn(fx.originPath, ["checkout", "-q", "main"]);
    }
    let bookkeeping: { publishedTip: string; lastPublishedTip?: string; checkpointFloor?: string } | undefined;
    const chunks: Buffer[] = [];
    let h = "";
    let f = "";
    let bare = "";
    let published = "";
    let continued = false;
    git.checkpointPack = async (...args) => {
      bare = args[0];
      assert.equal(args[5], h, "publisher forwards its exact proof");
      assert.equal(args[3]?.tipSha, sink === "scanned-checkpoint" ? h : undefined);
      if (sink === "scanned-checkpoint") assert.equal(args[3]?.excludeSha, scannedFloor);
      f = await promoteForeign(bare, args[1], h);
      // Move the origin floor after the scan too: the pinned range must remain literal.
      gitIn(bare, ["update-ref", "refs/remotes/origin/" + args[1],
        gitIn(bare, ["rev-parse", "refs/remotes/origin/main"])]);
      return realPack(...args);
    };
    client.publishCheckpoint = async (_run, tip, pack) => {
      assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).some(c => c.sha === h), "H stays pinned before broker confirmation");
      published = tip;
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/proof" } };
    };
    await runner({ run: async ctx => {
      if (sink !== "scanned-checkpoint") commit(ctx.worktreePath);
      if (sink === "scanned-checkpoint") {
        // The origin commit already has OWED.txt: give H distinct committed content.
        fs.writeFileSync(path.join(ctx.worktreePath, "H.txt"), "current H\n");
        gitIn(ctx.worktreePath, ["add", "H.txt"]);
        gitIn(ctx.worktreePath, ["-c", "user.name=test", "-c", "user.email=test@example.com",
          "-c", "commit.gpgsign=false", "commit", "-m", "current H"]);
      }
      h = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      if (sink === "limit-park") throw new LimitReachedError({ resetsAtMs: Date.now() + 3600_000 });
      await ctx.checkpoint!({ reap: false });
      continued = true;
      throw new Error("stop after checkpoint");
    } }, gitlab, undefined, { checkpointIntervalMs: 1, checkpointTestHooks: {
      afterPinnedPublish: state => { bookkeeping = state; },
    } }).execute(claim);
    assert.ok(f, "foreign promotion reached the real pack boundary");
    assert.equal(published, h);
    assert.notEqual(published, f);
    assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).every(c => c.sha !== h), "exact confirmed containment clears H");
    assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).every(c => c.sha !== f));
    const foreign = (await git.enumerateOwedCandidates(bare, "foreign-run")).find(c => c.sha === f);
    assert.ok(foreign, "foreign H2 stays pinned under its original owner");
    assert.ok(foreign.contexts.every(c => c.runId === "foreign-run" && c.generation === 8));
    if (sink === "scanned-checkpoint") {
      assert.equal(continued, true);
      assert.deepEqual(bookkeeping, { publishedTip: h, lastPublishedTip: h, checkpointFloor: h });
      const destination = path.join(fx.dataDir, "scanned-pack.git");
      execFileSync("git", ["init", "-q", "--bare", destination]);
      execFileSync("git", ["-C", destination, "index-pack", "--stdin"], { input: Buffer.concat(chunks) });
      const commits = gitIn(destination, ["cat-file", "--batch-all-objects",
        "--batch-check=%(objectname) %(objecttype)"]).split("\n")
        .filter(line => line.endsWith(" commit")).map(line => line.split(" ")[0]);
      assert.deepEqual(commits, [h], "neither the moved floor nor foreign tip enters the scanned pack");
    }
  });
}

it("a pre-proof unknown receipt refuses the scanned publisher without packing or false bookkeeping", async () => {
  const claim = gitlabClaim(1931);
  claim.claim_generation = 7;
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  let continued = false;
  await runner({ run: async ctx => {
    commit(ctx.worktreePath);
    await ctx.checkpoint!({ reap: false });
    continued = true;
    throw new Error("stop");
  } }, gitlab, undefined, { checkpointIntervalMs: 1, checkpointTestHooks: {
    afterCheckpointScan: async ({ barePath, branch, range }) => {
      const f = await promoteForeign(barePath, branch, range.tipSha);
      assert.notEqual(f, range.tipSha);
    },
  } }).execute(claim);
  assert.equal(continued, true);
  assert.equal(publication.packs(), 0);
  assert.equal(publication.brokers(), 0);
  assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
});

it("finalize pushes H and completes when a foreign promotion happens after the ownership proof", async () => {
  const claim = gitlabClaim(1932);
  claim.claim_generation = 7;
  const { gitlab } = fakeGitlab();
  const realPush = git.pushBranch.bind(git);
  let h = "";
  let f = "";
  let bare = "";
  git.pushBranch = async (...args) => {
    bare = args[0];
    assert.equal(args[5]?.candidate, h);
    f = await promoteForeign(bare, args[1], h);
    await realPush(...args);
  };
  await runner({ run: async ctx => {
    commit(ctx.worktreePath);
    h = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    return { branch: ctx.branch };
  } }, gitlab).execute(claim);
  assert.ok(f, "finalize reached the push boundary");
  assert.equal(gitIn(fx.originPath, ["rev-parse", "agent/issue-1932"]), h);
  assert.notEqual(h, f);
  assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).some(c => c.sha === h));
  assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).every(c => c.sha !== f));
  assert.equal(api.states.some(s => s.body.status === "completed"), true);
});
