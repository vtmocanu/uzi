import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, bareDirName, gitEnv } from "../src/git.js";

let fx: Fixture;
let git: GitCache;

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
});

afterEach(() => fx.cleanup());

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}

describe("bareDirName", () => {
  it("maps repo URLs to collision-free directory names", () => {
    assert.strictEqual(bareDirName("https://gitlab.com/org/repo.git"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("https://gitlab.com/org/repo"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("git@gitlab.com:org/repo.git"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("ssh://git@gitlab.com:22/org/repo.git"), "gitlab.com%3A22+org+repo.git");
  });
});

describe("ensureClone", () => {
  it("clones bare on first call and fetches on the second", async () => {
    const bare = await git.ensureClone(fx.originPath);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
    // Second call refreshes the existing cache and returns the same path.
    const bareAgain = await git.ensureClone(fx.originPath);
    assert.strictEqual(bareAgain, bare);
  });

  it("never writes the PAT to the bare repo config", async () => {
    const secret = "super-secret-pat-value-999";
    const bare = await git.ensureClone(fx.originPath, secret);
    const cfg = fs.readFileSync(path.join(bare, "config"), "utf8");
    assert.ok(!cfg.includes(secret));
    assert.ok(!cfg.includes("extraHeader"));
  });
});

describe("runner clone lifecycle (PRD #51 M3, (b) separate-runner-clone)", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
  function refInBare(bare: string, ref: string): boolean {
    try {
      gitIn(bare, ["rev-parse", "--verify", "--quiet", ref]);
      return true;
    } catch {
      return false;
    }
  }

  it("seeds a runner clone on agent/issue-N off the default branch (a real clone, not a worktree)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 42);

    assert.strictEqual(rc.branch, "agent/issue-42");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "README.md")), true); // origin content checked out
    // A clone's .git is a DIRECTORY (its own object store), NOT a worktree pointer file.
    assert.strictEqual(fs.statSync(path.join(rc.path, ".git")).isDirectory(), true);
    // The branch lives in the CLONE; the worker bare is bare-only and does NOT get it.
    assert.match(gitIn(rc.path, ["rev-parse", "--verify", "refs/heads/agent/issue-42"]), /^[0-9a-f]{40}$/);
    assert.strictEqual(
      refInBare(bare, "refs/heads/agent/issue-42"),
      false,
      "the agent branch must NOT appear in the worker bare's heads (worker is bare-only)",
    );
    // The base commit is REPORTED, not just used internally: the lead cannot otherwise
    // know its branch's parent, and the clone's own default branch is not it (see the
    // resume case below). Nothing has committed yet, so the checkout point IS HEAD.
    assert.match(rc.baseCommit, /^[0-9a-f]{40}$/, "baseCommit must be a full resolved SHA");
    assert.strictEqual(rc.baseCommit, gitIn(rc.path, ["rev-parse", "HEAD"]));
    // On a FRESH branch the seed IS the default tip, so the two commits coincide — which
    // is what lets the prompt state one command instead of two.
    assert.strictEqual(rc.defaultBranchCommit, rc.baseCommit);
  });

  it("round-trips: commit in the clone → worker fetch-back → bare tree-diff → push to origin", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 7);

    // The agent commits in the runner clone (the only working tree).
    fs.writeFileSync(path.join(rc.path, "NEW.txt"), "hi\n");
    gitIn(rc.path, ["add", "NEW.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "work"]);
    const agentSha = gitIn(rc.path, ["rev-parse", "HEAD"]);

    // The worker fetches the agent branch BACK into a worker-side tracking ref.
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-7", "run-fixture");
    assert.strictEqual(ref, "refs/uzi-runner/agent/issue-7");
    assert.strictEqual(gitIn(bare, ["rev-parse", ref]), agentSha, "fetch-back landed the agent commit in the worker bare");
    // The fetched objects are now in the worker bare (push does not depend on the clone).
    assert.strictEqual(gitIn(bare, ["cat-file", "-t", agentSha]), "commit");

    // The worker-bare tree-diff sees the changed file (no working tree, --name-only).
    assert.deepStrictEqual(await git.changedFiles(bare, ref), ["NEW.txt"]);

    // pushBranch pushes FROM the tracking ref to origin's refs/heads/<branch>.
    await git.pushBranch(bare, "agent/issue-7", "", fx.originPath);
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", "refs/heads/agent/issue-7"]), agentSha, "branch landed at origin");
  });

  it("plants a git author identity so the agent's first commit succeeds with no ambient identity (#234)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 234);

    // The clone carries the planted identity, written repo-local (see runnerCloneForBranch).
    assert.strictEqual(gitIn(rc.path, ["config", "--get", "user.name"]), "uzi-agent");
    assert.strictEqual(gitIn(rc.path, ["config", "--get", "user.email"]), "uzi-agent@uzi.local");

    // Behavioral proof: a commit with the auto-detect fallback DISABLED (the passwd-less
    // runner uid's real condition) succeeds and is authored by the planted identity.
    fs.writeFileSync(path.join(rc.path, "WORK.txt"), "hi\n");
    gitIn(rc.path, ["add", "WORK.txt"]);
    gitIn(rc.path, ["-c", "user.useConfigOnly=true", "commit", "-m", "agent work"]);
    assert.strictEqual(
      gitIn(rc.path, ["log", "-1", "--pretty=%an <%ae>"]),
      "uzi-agent <uzi-agent@uzi.local>",
    );
  });

  it("without the planted identity, that same commit fails exit 128 — the control the fix removes (#234)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 235);
    // Undo the fix's plant to reconstruct the pre-#234 clone state.
    gitIn(rc.path, ["config", "--unset", "user.name"]);
    gitIn(rc.path, ["config", "--unset", "user.email"]);

    fs.writeFileSync(path.join(rc.path, "WORK.txt"), "hi\n");
    gitIn(rc.path, ["add", "WORK.txt"]);
    let code: number | undefined;
    try {
      gitIn(rc.path, ["-c", "user.useConfigOnly=true", "commit", "-m", "agent work"]);
    } catch (err) {
      code = (err as { status?: number }).status;
    }
    assert.strictEqual(
      code,
      128,
      "no identity + no auto-detect fallback must be the exit-128 failure the fix removes",
    );
  });

  it("resumes off the branch's origin tip when it already exists at origin", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // First cycle: commit + fetch-back + push agent/issue-9 to origin.
    const first = await git.createOrAttachRunnerClone(bare, 9);
    fs.writeFileSync(path.join(first.path, "A.txt"), "a\n");
    gitIn(first.path, ["add", "A.txt"]);
    gitIn(first.path, [...IDENT, "commit", "-m", "a"]);
    const sha1 = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, "agent/issue-9", "run-fixture");
    await git.pushBranch(bare, "agent/issue-9", "", fx.originPath);
    await git.removeRunnerClone(first.path);

    // Refresh the bare so origin-tracking learns agent/issue-9, then reseed.
    await git.ensureClone(fx.originPath);
    const second = await git.createOrAttachRunnerClone(bare, 9);

    assert.strictEqual(second.branch, "agent/issue-9");
    // Resumed off the fresh origin tip (A.txt present), NOT recreated off default.
    assert.strictEqual(fs.existsSync(path.join(second.path, "A.txt")), true);
    assert.strictEqual(gitIn(second.path, ["rev-parse", "HEAD"]), sha1);
    // …and the reported base is the BRANCH's own tip, not the default branch's. This is
    // the case the lead cannot infer: the clone still carries a local default branch, and
    // that ref is a FROZEN MIRROR of the default as it stood at first clone — so a diff
    // written against it spans commits this branch never touched, in whichever direction
    // the mirror has drifted. Note the assertion below compares against the ORIGIN's HEAD,
    // outside the clone, which is the fresh value; the clone's own stale `main` is not
    // what is being checked here.
    assert.strictEqual(second.baseCommit, sha1, "resume seeds off the branch's origin tip");
    const originDefault = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(
      second.baseCommit,
      originDefault,
      "…and that tip is NOT the default branch's, which is exactly why the base has to be reported",
    );
    // The SECOND commit, and the reason it exists: on a resume `baseCommit..HEAD` is only
    // what this run adds, so the branch diff needs the default tip as well. A note carrying
    // one commit is wrong on exactly the runs that carry prior work.
    assert.strictEqual(second.defaultBranchCommit, originDefault, "the default tip is reported alongside the base");
    assert.notStrictEqual(second.defaultBranchCommit, second.baseCommit, "…and on a resume the two differ");
  });

  // Issue #262 — the clone's `origin/main` must track the FRESH default head, not the bare's
  // frozen mirror. cloneBare rewrites the fetch refspec to `+refs/heads/*:refs/remotes/origin/*`,
  // so the bare's `refs/heads/main` freezes at first clone while its `refs/remotes/origin/main`
  // stays fresh; the default `git clone` refspec then copies the FROZEN `refs/heads/*` into the
  // clone's `origin/*`. golangci-lint's ratchet `new-from-merge-base: origin/main` would then
  // compute `merge-base(origin/main[frozen], HEAD[fresh])` = the ancient initial commit and
  // false-red the whole pre-existing backlog. Without the fix `origin/main` would be the frozen
  // initial commit and the merge-base would span the whole backlog; the fix update-refs it to
  // the fresh default head so the ratchet gates only branch-introduced findings.
  it("advances the clone's origin/main to the fresh default head, not the bare's frozen mirror (#262)", async () => {
    // First clone pins the bare's refs/heads/main at the fixture's initial commit.
    const bare = await git.ensureClone(fx.originPath);
    const initialHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);

    // Advance the fixture ORIGIN's default branch with a new commit.
    fs.writeFileSync(path.join(fx.originPath, "ADVANCE.txt"), "moved on\n");
    gitIn(fx.originPath, ["add", "ADVANCE.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "advance default"]);
    const freshHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(freshHead, initialHead, "precondition: the fresh head differs from the initial commit");

    // Refresh the bare: refs/remotes/origin/main moves to freshHead while refs/heads/main
    // stays frozen at initialHead — the exact stale-mirror condition.
    await git.ensureClone(fx.originPath);
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]), freshHead, "precondition: bare origin-tracking is fresh");
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/heads/main"]), initialHead, "precondition: bare's refs/heads/main is the frozen mirror");

    const rc = await git.createOrAttachRunnerClone(bare, 262);

    // The clone's origin/main (the ratchet base) is the FRESH head, not the frozen initial commit.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      freshHead,
      "clone origin/main must be advanced to the fresh default head (#262)",
    );
    // …so the ratchet base resolves to the fresh head: an EMPTY backlog range, not one spanning
    // the whole history back to the frozen initial commit.
    assert.strictEqual(
      gitIn(rc.path, ["merge-base", "refs/remotes/origin/main", "HEAD"]),
      freshHead,
      "merge-base(origin/main, HEAD) must be the fresh head so new-from-merge-base gates only branch findings",
    );
  });

  // Issue #134 (the production half of #127). Any git that writes into a repo spawns a
  // DETACHED `git maintenance run --auto --detach` that outlives the awaited process and keeps
  // writing inside `.git`. removeRunnerClone() (runner.ts:454) `fs.rm`s the clone moments after
  // the agent's last commit and our push, and `force: true` suppresses ENOENT, not ENOTEMPTY.
  // Pinning the CONFIG rather than trying to observe a race: the config is deterministic, the
  // race is not — and #127 spent two agents' effort failing to reproduce the race on demand.
  it("disables git auto-maintenance in BOTH repos it creates, so no detached gc races their removal (#134)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 77);

    for (const [label, repo] of [["worker bare", bare], ["runner clone", rc.path]] as const) {
      assert.strictEqual(
        gitIn(repo, ["config", "--get", "maintenance.auto"]),
        "false",
        `${label}: maintenance.auto must be false — it is the load-bearing key on git 2.54 ` +
          `(the worker image's own version), where gc.auto=0 alone leaves the detached spawn intact`,
      );
      assert.strictEqual(
        gitIn(repo, ["config", "--get", "gc.auto"]),
        "0",
        `${label}: gc.auto=0 is the key on 2.55+, where prepare_auto_maintenance gained a ` +
          `gc.auto fallback. Neither key subsumes the other across the version range.`,
      );
    }
  });

  // The test above only proves the keys land on a FIRST clone. `cloneBare` runs once ever,
  // and `/data` is persistent (a per-worker PVC in k8s, the `agentdata` volume under compose),
  // so every bare on an already-deployed worker is reached ONLY through ensureClone's fetch
  // branch. Writing the keys in cloneBare alone therefore applied them to none of the repos
  // the change is actually about. Caught in review; this is the regression pin.
  it("reasserts auto-maintenance-off on the WARM ensureClone path, not just the first clone (#134)", async () => {
    const bare = await git.ensureClone(fx.originPath);

    // Simulate a bare that predates the fix.
    gitIn(bare, ["config", "--unset", "maintenance.auto"]);
    gitIn(bare, ["config", "--unset", "gc.auto"]);
    // `config --get` EXITS 1 on an unset key, and gitIn throws on non-zero — so read the
    // precondition tolerantly rather than asserting through a throw.
    const getOrEmpty = (repo: string, key: string): string => {
      try {
        return gitIn(repo, ["config", "--get", key]);
      } catch {
        return "";
      }
    };
    assert.strictEqual(getOrEmpty(bare, "maintenance.auto"), "", "precondition: key is unset");

    // Second call takes the isBareRepo branch — no cloneBare involved.
    const again = await git.ensureClone(fx.originPath);
    assert.strictEqual(again, bare, "precondition: same bare, so this exercised the warm path");

    assert.strictEqual(
      gitIn(bare, ["config", "--get", "maintenance.auto"]),
      "false",
      "an existing bare must be reconfigured on use — otherwise every deployed worker keeps " +
        "racing a detached gc against its own claim fetch forever",
    );
    assert.strictEqual(gitIn(bare, ["config", "--get", "gc.auto"]), "0");
  });

  it("removes the runner clone but keeps the worker bare clone", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 5);

    await git.removeRunnerClone(rc.path);

    assert.strictEqual(fs.existsSync(rc.path), false);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
  });
});

describe("branchTip / trackingTip (PRD #122 M6)", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

  it("branchTip reads the runner clone's own head; trackingTip is null before a fetch-back and the tip after", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 7);

    // The agent commits in the runner clone (the only working tree).
    fs.writeFileSync(path.join(rc.path, "NEW.txt"), "hi\n");
    gitIn(rc.path, ["add", "NEW.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "work"]);
    const agentSha = gitIn(rc.path, ["rev-parse", "HEAD"]);

    // branchTip reads refs/heads/<branch> in the runner-owned clone → the committed tip.
    assert.strictEqual(await git.branchTip(rc.path, "agent/issue-7"), agentSha);

    // No checkpoint has fetched anything back yet, so the tracking ref does not exist.
    assert.strictEqual(await git.trackingTip(bare, "agent/issue-7"), null);

    // After a fetch-back the tracking ref exists and its tip equals the clone's tip — this
    // is exactly the equality the checkpoint no-op check compares.
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-7", "run-fixture");
    assert.strictEqual(await git.trackingTip(bare, "agent/issue-7"), agentSha);
  });

  it("both answer null for a branch that does not exist rather than throwing", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 8);
    assert.strictEqual(await git.branchTip(rc.path, "agent/issue-does-not-exist"), null);
    assert.strictEqual(await git.trackingTip(bare, "agent/issue-does-not-exist"), null);
  });
});

describe("checkpoint reseed candidate (PRD #122 M8)", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

  /** Commit `file` in the runner clone and return the new HEAD sha. */
  function commit(dir: string, file: string): string {
    fs.writeFileSync(path.join(dir, file), `${file}\n`);
    gitIn(dir, ["add", file]);
    gitIn(dir, [...IDENT, "commit", "-m", `work ${file}`]);
    return gitIn(dir, ["rev-parse", "HEAD"]);
  }

  it("(a) seeds off a mirrored checkpoint that strictly descends the floor when no owned tracking ref exists", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Build a checkpoint commit descending the default branch, land its objects in the
    // bare (as a fetch-back would), and publish it as origin's mirrored checkpoint ref.
    const seed = await git.createOrAttachRunnerClone(bare, 700, "run-A");
    const cpSha = commit(seed.path, "CP.txt");
    await git.fetchAgentBranch(bare, seed.path, "agent/issue-700", "run-A");
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-700", cpSha]);
    // Simulate a DIFFERENT worker: no local refs/uzi-runner/<branch> here.
    gitIn(bare, ["update-ref", "-d", "refs/uzi-runner/agent/issue-700"]);
    await git.removeRunnerClone(seed.path);

    // A fresh cross-worker run (different runId, no origin branch) seeds off the checkpoint.
    const rc = await git.createOrAttachRunnerClone(bare, 700, "run-B");
    assert.strictEqual(rc.seededFrom, "checkpoint");
    assert.strictEqual(rc.baseCommit, cpSha, "baseCommit is the checkpoint tip");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "CP.txt")), true, "checkpointed work checked out");
    assert.notStrictEqual(rc.checkpointSetAside, true);
  });

  it("(b) origin wins and the checkpoint is set aside LOUDLY when the checkpoint diverged from origin", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Cycle 1: push agent/issue-701 to origin at commit A.
    const first = await git.createOrAttachRunnerClone(bare, 701, "run-1");
    const shaA = commit(first.path, "A.txt");
    await git.fetchAgentBranch(bare, first.path, "agent/issue-701", "run-1");
    await git.pushBranch(bare, "agent/issue-701", "", fx.originPath);
    await git.removeRunnerClone(first.path);
    await git.ensureClone(fx.originPath); // refresh origin-tracking so origin/agent/issue-701 exists

    // A DIVERGED checkpoint: a sibling of A off the default branch (neither descends the
    // other). Built on a throwaway branch so its commit objects reach the bare, then
    // republished under issue-701's checkpoint ref.
    const sib = await git.createOrAttachRunnerClone(bare, 7011, "run-sib");
    const cpxSha = commit(sib.path, "CPX.txt");
    await git.fetchAgentBranch(bare, sib.path, "agent/issue-7011", "run-sib");
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-701", cpxSha]);
    await git.removeRunnerClone(sib.path);

    // Reseed issue-701 as a DIFFERENT run: origin exists and the checkpoint diverged from it.
    const rc = await git.createOrAttachRunnerClone(bare, 701, "run-2");
    assert.strictEqual(rc.seededFrom, "origin", "origin wins on divergence");
    assert.strictEqual(rc.baseCommit, shaA);
    assert.strictEqual(rc.checkpointSetAside, true, "the diverged checkpoint is flagged, not dropped silently");
  });

  it("(c) absent checkpoint ⇒ unchanged behaviour (fresh seed off default, no set-aside)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 702, "run-C");
    assert.strictEqual(rc.seededFrom, "default");
    assert.notStrictEqual(rc.checkpointSetAside, true);
  });

  it("(d) ownedHere local tracking ref still wins over a present checkpoint (same-worker work is not overridden)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc1 = await git.createOrAttachRunnerClone(bare, 800, "run-D");
    const shaD = commit(rc1.path, "D.txt");
    await git.fetchAgentBranch(bare, rc1.path, "agent/issue-800", "run-D"); // tracking owned by run-D
    // A checkpoint that WOULD win the not-ownedHere path (it descends the default floor).
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-800", shaD]);
    await git.removeRunnerClone(rc1.path);

    // Reseed as the SAME run: the owned tracking ref is consulted, the checkpoint ignored.
    const rc = await git.createOrAttachRunnerClone(bare, 800, "run-D");
    assert.strictEqual(rc.seededFrom, "tracking", "the checkpoint must not override same-worker local work");
    assert.strictEqual(rc.baseCommit, shaD);
    assert.notStrictEqual(rc.checkpointSetAside, true);
  });

  it("(e) checkpoint EQUAL to the floor ⇒ falls through to the floor (not 'checkpoint'), no set-aside", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // A checkpoint ref pointing at EXACTLY the floor (the default branch tip). isAncestor
    // (merge-base --is-ancestor) is TRUE at equality, so the strict-descendant guard must
    // keep an equal checkpoint from being seeded as "checkpoint" — nothing was recovered —
    // and equality is NOT divergence, so it is not "set aside" either.
    const floorSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/HEAD"]);
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-703", floorSha]);

    const rc = await git.createOrAttachRunnerClone(bare, 703, "run-E");
    assert.notStrictEqual(rc.seededFrom, "checkpoint", "an equal checkpoint recovers nothing");
    assert.strictEqual(rc.seededFrom, "default", "falls through to the default floor");
    assert.strictEqual(rc.baseCommit, floorSha);
    assert.notStrictEqual(rc.checkpointSetAside, true, "equality is not divergence");
  });
});

describe("checkpointPack (PRD #122 M8)", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

  async function drain(r: Readable): Promise<Buffer> {
    const chunks: Buffer[] = [];
    for await (const c of r) chunks.push(c as Buffer);
    return Buffer.concat(chunks);
  }

  it("returns null with no tracking ref, and a valid non-empty pack whose tipOid is the tracking tip once one exists", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 900, "run-p");

    // No fetch-back yet ⇒ no tracking ref ⇒ nothing to publish.
    assert.strictEqual(await git.checkpointPack(bare, "agent/issue-900"), null);

    // Commit + fetch-back so the tracking ref is ahead of the default branch.
    fs.writeFileSync(path.join(rc.path, "P.txt"), "p\n");
    gitIn(rc.path, ["add", "P.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "p"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-900", "run-p");
    const tip = await git.trackingTip(bare, "agent/issue-900");

    const packed = await git.checkpointPack(bare, "agent/issue-900");
    assert.ok(packed, "a tracking ref ahead of default yields a pack");
    assert.strictEqual(packed!.tipOid, tip, "tipOid is the tracking-ref tip");
    const buf = await drain(packed!.pack);
    assert.ok(buf.length > 0, "the pack is non-empty");
    assert.strictEqual(buf.subarray(0, 4).toString("ascii"), "PACK", "a real packfile starts with the PACK signature");
  });
});

/** The constant `core.hooksPath` git.ts pins; baked by both worker templates. */
const HOOKS_DIR = "/usr/share/uzi-git-nohooks";

/** The three facts about a path that decide the question below. Split out from the stat so
 *  the baked shape can be asserted at all: an unprivileged test cannot chown to root, which
 *  is the very property the verdict rests on. */
interface HooksDirFacts {
  uid: number;
  /** Raw `stat` mode, type bits included — the verdict masks them off itself. */
  mode: number;
  isDir: boolean;
}

/** `undefined` when nothing is there. Absence is a legitimate answer, not an error. */
function hooksDirFacts(p: string): HooksDirFacts | undefined {
  try {
    const st = fs.lstatSync(p);
    return { uid: st.uid, mode: st.mode, isDir: st.isDirectory() };
  } catch {
    return undefined;
  }
}

/**
 * Did something OTHER than the image create the hooks dir?
 *
 * The invariant is NOT "the path does not exist" — that reading is what made this case
 * fail deterministically inside a worker container, where both templates bake the dir
 * (`mkdir -p … && chmod 0555`, as root). The invariant is that no dir writable by the uid
 * the worker and agent share sits on `core.hooksPath`, because a writable one is a place
 * to plant a hook and that is the whole vector.
 *
 * So: absent ⇒ git.ts created nothing, fine. Present ⇒ it must be in the BAKED shape,
 * root-owned with no write bit. That shape is unreachable at runtime here — an
 * unprivileged process can neither chown to root nor create anything under /usr/share —
 * so its presence is not evidence of a runtime mkdir. Anything else is: a dir this uid
 * owns can be chmod'd back open whatever its current mode, and a root-owned dir carrying
 * a write bit is already open.
 *
 * The mode half is not redundant with the uid half, and the reason is specific to this
 * image: the worker baseline IS root and the entrypoint drops later (PRD #51 A1), so a
 * runtime mkdir inside that window would be root-owned 0755 under the default umask. Only
 * the `chmod 0555` distinguishes the baked stanza from it.
 */
function hooksDirVerdict(facts: HooksDirFacts | undefined): { ok: boolean; why: string } {
  if (!facts) return { ok: true, why: `${HOOKS_DIR} is absent, so nothing created it` };
  const mode = (facts.mode & 0o7777).toString(8).padStart(4, "0");
  if (!facts.isDir) return { ok: false, why: `not a directory (mode 0${mode})` };
  if (facts.uid !== 0) return { ok: false, why: `owned by uid ${facts.uid}, not root — a runtime mkdir` };
  if (facts.mode & 0o222) return { ok: false, why: `mode 0${mode} carries a write bit; the baked dir is 0555` };
  return { ok: true, why: `baked shape (root-owned, mode 0${mode})` };
}

describe("gitEnv (M10: scrubbed replacement env + hook neutralization)", () => {
  const WORKER_VARS = ["UZI_WORKER_TOKEN", "UZI_WORKER_TOKEN_FILE", "UZI_API_URL"];
  beforeEach(() => {
    process.env.UZI_WORKER_TOKEN = "join-token-SECRET";
    process.env.UZI_WORKER_TOKEN_FILE = "/run/secrets/worker_token";
    process.env.UZI_API_URL = "http://api:8080";
  });
  afterEach(() => {
    for (const k of WORKER_VARS) delete process.env[k];
  });

  // Collect the inline GIT_CONFIG_* pairs into a key→value map.
  function configPairs(env: NodeJS.ProcessEnv): Record<string, string> {
    const out: Record<string, string> = {};
    const n = Number(env.GIT_CONFIG_COUNT ?? "0");
    for (let i = 0; i < n; i++) out[env[`GIT_CONFIG_KEY_${i}`]!] = env[`GIT_CONFIG_VALUE_${i}`]!;
    return out;
  }

  it("excludes the worker/API vars by construction, keeps PATH + GIT_TERMINAL_PROMPT", () => {
    const env = gitEnv();
    for (const k of WORKER_VARS) assert.equal(env[k], undefined, `${k} must be absent from the git env`);
    assert.equal(env.PATH, process.env.PATH);
    assert.equal(env.GIT_TERMINAL_PROMPT, "0");
    assert.ok(!JSON.stringify(env).includes("join-token-SECRET"), "no worker secret in the git env");
  });

  // Issue #134. Added because a negative control found these pins UNTESTED: removing them
  // from gitEnv broke nothing, while the claim on them is that they close the whole class
  // (highest precedence, warm or cold, unoverridable by a planted config file).
  it("pins auto-maintenance off inline, so no git through this module spawns a detached gc (#134)", () => {
    const cfg = configPairs(gitEnv());
    assert.equal(
      cfg["maintenance.auto"],
      "false",
      "the load-bearing key on git 2.54 (the shipped worker image), where " +
        "prepare_auto_maintenance reads ONLY maintenance.auto",
    );
    assert.equal(
      cfg["gc.auto"],
      "0",
      "the key on 2.55+, which gained a gc.auto fallback — neither subsumes the other across " +
        "the version range, so both are pinned",
    );
    // Still present with a PAT: the credential branch appends and must not displace them.
    const withPat = configPairs(gitEnv("secret-pat", "https://gitlab.example.com/", "bot"));
    assert.equal(withPat["maintenance.auto"], "false");
    assert.equal(withPat["gc.auto"], "0");
  });

  it("neutralizes hooks: core.hooksPath is the baked root-owned path, NOT a runtime dir", () => {
    const cfg = configPairs(gitEnv());
    assert.equal(cfg["safe.directory"], "*");
    // The empty hooks dir is BAKED into the image as root-owned 0555
    // (agent/templates/base/Dockerfile, and jvm/Dockerfile separately — jvm is not
    // `FROM base`). The uzi uid (which the agent shares) cannot create a hook inside it.
    // A runtime-created dir would be under the shared uid and agent-writable, which is
    // the vector this closes. Here we pin that git.ts uses the constant baked path…
    assert.equal(cfg["core.hooksPath"], "/usr/share/uzi-git-nohooks", "must be the baked constant path");
    // …and does not create it at runtime. This USED to assert plain absence, which is
    // false INSIDE a worker container by construction — both templates bake the dir — so
    // the case failed deterministically in the one environment that matters, every run
    // re-triaged it as "1 red is benign", and a real regression could hide behind an
    // accepted red. Absence is not the invariant; the invariant is that no AGENT-WRITABLE
    // hooks dir sits on core.hooksPath. See hooksDirVerdict.
    const verdict = hooksDirVerdict(hooksDirFacts(HOOKS_DIR));
    assert.ok(verdict.ok, `git.ts must NOT mkdir the hooks dir at runtime: ${verdict.why}`);
  });

  // The control for the assertion above, and it is a control rather than a nicety: a
  // predicate that answers "fine" to both states would be a test that cannot fail, which
  // is strictly worse than the deterministic red it replaced. The runtime-created shape is
  // reproduced for real (a temp dir this process owns, 0755); the baked shape has to be
  // supplied as facts, because an unprivileged test cannot chown anything to root — which
  // is the same fact the predicate rests on.
  it("the hooks-dir predicate still REJECTS a runtime-created dir (positive control)", () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hooks-"));
    try {
      const runtime = path.join(tmp, "uzi-git-nohooks");
      fs.mkdirSync(runtime);
      fs.chmodSync(runtime, 0o755); // explicit: the worker runs with umask 002 (main.ts)
      const v = hooksDirVerdict(hooksDirFacts(runtime));
      assert.equal(v.ok, false, `a dir in the runtime-created shape must FAIL, got: ${v.why}`);

      // Absent ⇒ pass. git.ts created nothing, which is the whole invariant.
      assert.equal(hooksDirVerdict(hooksDirFacts(path.join(tmp, "no-such-dir"))).ok, true);

      // The baked shape ⇒ pass. Root-owned with no write bit is not reachable by the uid
      // the worker and agent share, so its presence is not evidence of a runtime mkdir.
      assert.equal(hooksDirVerdict({ uid: 0, mode: 0o040555, isDir: true }).ok, true);

      // …and the two halves are independent. Root-owned but writable still fails (this is
      // also what carries the control above if someone runs the suite AS root, where a
      // runtime-created dir is root-owned too), and a non-root 0555 dir fails as well: an
      // agent that owns a directory can chmod it back.
      assert.equal(hooksDirVerdict({ uid: 0, mode: 0o040755, isDir: true }).ok, false);
      assert.equal(hooksDirVerdict({ uid: 1000, mode: 0o040555, isDir: true }).ok, false);
      // A non-directory on the path is not a hooks dir at all.
      assert.equal(hooksDirVerdict({ uid: 0, mode: 0o100555, isDir: false }).ok, false);
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("carries the scoped credential pair when a PAT is passed (and hooks stay neutralized)", () => {
    const cfg = configPairs(gitEnv("secret-pat", "https://gitlab.example.com/", "bot"));
    assert.match(cfg["http.https://gitlab.example.com/.extraHeader"] ?? "", /^Authorization: Basic /);
    assert.equal(cfg["http.https://gitlab.example.com/.followRedirects"], "false");
    assert.ok(cfg["core.hooksPath"], "core.hooksPath is still set alongside the credential");
    // The PAT rides GIT_CONFIG_VALUE_n (base64 in the header) — never a plain env var.
    for (const [k, v] of Object.entries(gitEnv("secret-pat"))) {
      if (!k.startsWith("GIT_CONFIG_VALUE_")) assert.ok(!String(v).includes("secret-pat"), `PAT leaked into ${k}`);
    }
  });
});
