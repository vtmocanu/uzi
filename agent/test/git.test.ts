import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, recordingLogger } from "./helpers.js";
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

  // Issue #775: a transient connect-timeout on the claim-time bare clone must be RETRIED
  // (the classifier now reads git's "Failed to connect ..." as transient), and the widened
  // cloneBare cleanup must leave no half-clone behind so each retry starts from a clean bare.
  it("retries a failed-to-connect bare clone and cleans up the dest on give-up", async () => {
    // A closed local TCP port: git-over-HTTP to it yields "Failed to connect to 127.0.0.1
    // port <n> after N ms: Connection refused", which /failed to connect/i classifies transient.
    const port = await new Promise<number>((resolve, reject) => {
      const srv = net.createServer();
      srv.on("error", reject);
      srv.listen(0, "127.0.0.1", () => {
        const addr = srv.address();
        const p = typeof addr === "object" && addr ? addr.port : 0;
        srv.close(() => resolve(p));
      });
    });
    const url = `http://127.0.0.1:${port}/x.git`;

    let sleeps = 0;
    const schedule = [1, 1]; // schedule.length sleeps ⇒ schedule.length + 1 attempts
    const retryingGit = new GitCache(fx.dataDir, nullLogger(), {
      schedule,
      sleep: async () => {
        sleeps++;
      },
    });

    await assert.rejects(retryingGit.ensureClone(url), /failed to connect/i);
    // schedule.length sleeps happened ⇒ the clone was attempted schedule.length + 1 times.
    // If this is 0, the message did not classify transient (no retry) and the target is wrong.
    assert.strictEqual(sleeps, schedule.length);
    // The widened cloneBare cleanup removed the half-populated bare on the final failure.
    assert.strictEqual(fs.existsSync(retryingGit.barePathFor(url)), false);
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

  // PRD #400 M7 — behavioral proof of the "worker push is NON-FORCED" guardrail. pushBranch
  // builds a plain `<src>:refs/heads/<branch>` refspec (no `+`, no `--force`), so a push that
  // is not a fast-forward MUST be refused rather than silently rewriting origin history — the
  // property that stops a task run from clobbering a branch a human (or another run) advanced.
  // This is the behavioral companion to the structural refspec; it drives real local git.
  it("refuses a NON-fast-forward push — the worker push is non-forced, so it can never rewrite origin history (PRD #400 M7)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 400);

    // Cycle 1: commit A in the clone, fetch it back, and push agent/issue-400 to origin.
    fs.writeFileSync(path.join(rc.path, "A.txt"), "a\n");
    gitIn(rc.path, ["add", "A.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "a"]);
    const shaA = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-400", "run-fixture");
    await git.pushBranch(bare, "agent/issue-400", "", fx.originPath);
    assert.strictEqual(
      gitIn(fx.originPath, ["rev-parse", "refs/heads/agent/issue-400"]),
      shaA,
      "precondition: cycle 1 landed the branch at origin",
    );

    // Advance ORIGIN's copy of the branch to a DIVERGENT commit C (a child of A landed
    // directly at origin — e.g. a human pushed meanwhile), so the worker's next push is no
    // longer a fast-forward. Committed on the branch, then back to main so the push target
    // is not the checked-out branch.
    gitIn(fx.originPath, ["checkout", "agent/issue-400"]);
    fs.writeFileSync(path.join(fx.originPath, "ORIGIN.txt"), "moved on at origin\n");
    gitIn(fx.originPath, ["add", "ORIGIN.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "divergent origin commit"]);
    const shaC = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    assert.notStrictEqual(shaC, shaA, "precondition: origin's branch advanced past the pushed tip");

    // The worker meanwhile advances its OWN work to a SIBLING commit B (also a child of A,
    // NOT a descendant of C) and fetches it back, so the tracking ref pushBranch reads is B.
    fs.writeFileSync(path.join(rc.path, "LOCAL.txt"), "local divergent work\n");
    gitIn(rc.path, ["add", "LOCAL.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "divergent local commit"]);
    const shaB = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-400", "run-fixture");
    assert.strictEqual(
      gitIn(bare, ["rev-parse", "refs/uzi-runner/agent/issue-400"]),
      shaB,
      "precondition: the tracking ref pushBranch pushes FROM is the divergent local tip B",
    );

    // PRECONDITION: B and C genuinely diverge — both are direct children of A, so neither
    // descends the other and pushing B onto origin@C can ONLY succeed by forcing. Checked
    // via each commit's parent in the repo that actually holds it (C lives at origin, B in
    // the bare) rather than a cross-repo merge-base, which would throw on a missing object
    // and pass this precondition for the wrong reason.
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", "agent/issue-400^"]), shaA, "precondition: C's parent is A");
    assert.strictEqual(gitIn(bare, ["rev-parse", `${shaB}^`]), shaA, "precondition: B's parent is A");
    assert.notStrictEqual(shaB, shaC, "precondition: B and C are distinct siblings — a genuine divergence");

    // THE ASSERTION: pushBranch must REJECT rather than overwrite. A forced push would
    // silently rewrite origin history; a plain (non-forced) push is refused as non-ff.
    await assert.rejects(
      git.pushBranch(bare, "agent/issue-400", "", fx.originPath),
      /non-fast-forward|\[rejected\]|failed to push/i,
      "a non-forced push MUST refuse a non-fast-forward update — no history rewrite is possible",
    );

    // …and origin's branch is UNTOUCHED — still C, never overwritten to the worker's B.
    assert.strictEqual(
      gitIn(fx.originPath, ["rev-parse", "refs/heads/agent/issue-400"]),
      shaC,
      "origin history must be intact after the refused push (no force was applied)",
    );
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

  // Issue #313 — the RESIDUAL frozen-mirror false-red that survives the #262 fix. On a resume
  // leg, `defaultBranchCommit` flows through `defaultBranchSha` → `defaultBranchRef`, whose
  // fallback chain drops to the FROZEN `refs/heads/main` rung when the fresh tracking refs
  // (`refs/remotes/origin/HEAD` / `refs/remotes/origin/main`) are absent. #262 then advances the
  // clone's `origin/main` to that STALE commit — a strict ancestor of the branch's real base —
  // so `merge-base(origin/main, HEAD)` regresses below the fork point and false-reds the whole
  // backlog. The clamp (issue #313) never lets the ratchet base be a strict ancestor of the
  // branch base: it points `origin/main` at `baseSha` instead. This test builds that exact
  // stale-ancestor topology; it is RED before the clamp and GREEN after (it asserts the desired
  // post-fix state, not the pre-fix stale value).
  const CLAMP_MSG = "runner clone: clamped ratchet base to branch base (stale default ref)";

  it("clamps the clone's origin/main to the branch base when the default ref is a stale ancestor (#313)", async () => {
    // Recording logger so we can also prove the clamp log fires ONLY on the actual-change path.
    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger);
    // First clone pins the bare's refs/heads/main (the frozen mirror) at the initial commit.
    const bare = await git.ensureClone(fx.originPath);
    const initialHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);

    // Advance the fixture ORIGIN's default branch, so the branch's real base will be FRESH.
    fs.writeFileSync(path.join(fx.originPath, "ADVANCE.txt"), "moved on\n");
    gitIn(fx.originPath, ["add", "ADVANCE.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "advance default"]);
    const freshHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(freshHead, initialHead, "precondition: fresh head differs from initial");

    // Seed a first runner clone off the FRESH default, commit, and push agent/issue-313 to
    // origin — so on the reseed the branch tip descends from freshHead (its real base is fresh).
    await git.ensureClone(fx.originPath); // origin-tracking learns freshHead
    const first = await git.createOrAttachRunnerClone(bare, 313);
    assert.strictEqual(first.baseCommit, freshHead, "precondition: first seed is off the fresh default");
    fs.writeFileSync(path.join(first.path, "WORK.txt"), "w\n");
    gitIn(first.path, ["add", "WORK.txt"]);
    gitIn(first.path, [...IDENT, "commit", "-m", "branch work"]);
    const branchTip = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, "agent/issue-313", "run-fixture");
    await git.pushBranch(bare, "agent/issue-313", "", fx.originPath);
    await git.removeRunnerClone(first.path);

    // Refresh the bare so origin-tracking learns agent/issue-313, then STALE the fresh default
    // tracking refs so defaultBranchRef falls through to the frozen refs/heads/main rung. Delete
    // origin/HEAD (the symbolic-ref rung) and origin/main (the remote-tracking rung); refs/heads/main
    // (the frozen mirror, still initialHead) then wins the chain, so defaultBranchName stays "main"
    // (update-ref still fires) while defaultBranchSha resolves the STALE initial commit.
    await git.ensureClone(fx.originPath);
    gitIn(bare, ["symbolic-ref", "-d", "refs/remotes/origin/HEAD"]);
    gitIn(bare, ["update-ref", "-d", "refs/remotes/origin/main"]);
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/heads/main"]), initialHead, "precondition: frozen mirror is the initial commit");

    // Reseed: a resume off origin/agent/issue-313 (baseCommit fresh) whose defaultBranchCommit
    // resolves through the frozen rung to the stale initial commit.
    const rc = await git.createOrAttachRunnerClone(bare, 313);
    assert.strictEqual(rc.baseCommit, branchTip, "precondition: resume base is the fresh branch tip");

    // PRECONDITION: the topology is genuinely the stale-ANCESTOR case — defaultBranchCommit is a
    // STRICT ancestor of baseCommit (ancestor, and not equal), i.e. exactly the #262-residual bug.
    assert.doesNotThrow(
      () => gitIn(bare, ["merge-base", "--is-ancestor", rc.defaultBranchCommit!, rc.baseCommit]),
      "precondition: defaultBranchCommit is an ancestor of baseCommit",
    );
    assert.notStrictEqual(rc.defaultBranchCommit, rc.baseCommit, "precondition: it is a STRICT ancestor (stale), not equal");

    // POST-FIX state (RED before the clamp, GREEN after): the clone's origin/main is the branch
    // base, so the ratchet gates only branch-reachable findings.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "clone origin/main must be clamped to the branch base, not the stale frozen ancestor (#313)",
    );
    assert.strictEqual(
      gitIn(rc.path, ["merge-base", "refs/remotes/origin/main", "HEAD"]),
      rc.baseCommit,
      "merge-base(origin/main, HEAD) must be the branch base so the ratchet spans no third-party backlog",
    );

    // The clamp log fires here (an ACTUAL change: ratchetBase !== defaultBranchCommit).
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg === CLAMP_MSG),
      "the clamp log must fire when the ratchet base was clamped off a stale ancestor",
    );
  });

  // Issue #313 — no-op path A: a FRESH run where defaultBranchCommit === baseCommit. isAncestor is
  // true at equality so the clamp branch is entered, but ratchetBase stays === defaultBranchCommit,
  // so the write is byte-for-byte what #262 would have written and the clamp log does NOT fire.
  it("is a no-op on a fresh run (defaultBranchCommit === baseCommit): clone origin/main unchanged, no clamp log (#313)", async () => {
    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 3131);

    // Fresh seed: the base IS the default tip, so the two coincide (see the fresh-seed test above).
    assert.strictEqual(rc.defaultBranchCommit, rc.baseCommit, "precondition: fresh run, the two commits coincide");
    // The clone's origin/main is the same value #262 would have written — baseCommit (== defaultBranchCommit).
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "fresh run: clone origin/main is the (identical) #262 value, not clamped away",
    );
    // …and the clamp log does NOT fire, because ratchetBase === defaultBranchCommit (no change).
    assert.ok(
      !lines.some((l) => (l as { msg?: string }).msg === CLAMP_MSG),
      "the clamp log must NOT fire on a fresh run where nothing was clamped",
    );
  });

  // Issue #313 — no-op path B: a RESUME where main moved forward on a DIVERGENT line, so
  // defaultBranchCommit is NOT an ancestor of baseSha. GENUINE divergence, built for real: the
  // branch is pushed off the initial commit, THEN origin's main advances on its own line. Neither
  // the fresh main nor the branch tip descends the other (their only common ancestor is the initial
  // commit), so the clamp is a no-op and origin/main keeps defaultBranchCommit — preserving vs-main
  // merge-base semantics.
  it("keeps defaultBranchCommit on a resume where main diverged forward (not an ancestor of baseSha) (#313)", async () => {
    const bare = await git.ensureClone(fx.originPath);

    // Seed the branch off the INITIAL commit, commit, and push agent/issue-3132 to origin.
    const first = await git.createOrAttachRunnerClone(bare, 3132);
    fs.writeFileSync(path.join(first.path, "BRANCH.txt"), "b\n");
    gitIn(first.path, ["add", "BRANCH.txt"]);
    gitIn(first.path, [...IDENT, "commit", "-m", "branch work"]);
    const branchTip = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, "agent/issue-3132", "run-fixture");
    await git.pushBranch(bare, "agent/issue-3132", "", fx.originPath);
    await git.removeRunnerClone(first.path);

    // Advance origin's main on ITS OWN line (a sibling of the branch off the initial commit).
    fs.writeFileSync(path.join(fx.originPath, "MAIN.txt"), "m\n");
    gitIn(fx.originPath, ["add", "MAIN.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "advance main on a divergent line"]);
    const freshMain = gitIn(fx.originPath, ["rev-parse", "HEAD"]);

    // Refresh the bare so origin-tracking learns both the fresh main and agent/issue-3132.
    await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 3132);

    // Resume: base is the branch tip; defaultBranchCommit is the fresh main.
    assert.strictEqual(rc.baseCommit, branchTip, "precondition: resume base is the branch tip");
    assert.strictEqual(rc.defaultBranchCommit, freshMain, "precondition: defaultBranchCommit is the fresh divergent main");

    // PRECONDITION: genuine divergence — defaultBranchCommit is NOT an ancestor of baseCommit
    // (and they are not equal), so the clamp must be a no-op.
    assert.throws(
      () => gitIn(bare, ["merge-base", "--is-ancestor", rc.defaultBranchCommit!, rc.baseCommit]),
      "precondition: defaultBranchCommit is NOT an ancestor of baseCommit (divergent)",
    );

    // The clamp is a no-op: origin/main keeps defaultBranchCommit, so vs-main semantics stand.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.defaultBranchCommit,
      "divergent resume: clone origin/main keeps defaultBranchCommit (clamp is a no-op)",
    );
  });

  // Issue #363 — the clamp (#262/#313) is UNDONE by a later `git fetch`. A plain `git clone`
  // leaves a `remote.origin.fetch` refspec (`+refs/heads/*:refs/remotes/origin/*`) in the runner
  // clone, so an agent that runs `git fetch origin main` / `git fetch origin` re-applies it and
  // drags `refs/remotes/origin/main` back to the FROZEN bare mirror head — exactly the stale
  // ancestor the clamp corrected — re-corrupting the ratchet base mid-run. The fix removes that
  // refspec after clamping, so a fetch updates only FETCH_HEAD and moves no tracking ref. This
  // test builds the SAME stale-ancestor topology as the #313 test, then fetches and re-asserts the
  // clamp survives. RED without the git.ts refspec removal, GREEN with it.
  const buildStaleAncestorClone = async (issue: number): Promise<{ rc: Awaited<ReturnType<GitCache["createOrAttachRunnerClone"]>>; frozen: string }> => {
    const git = new GitCache(fx.dataDir, nullLogger());
    const bare = await git.ensureClone(fx.originPath);
    const initialHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);

    // Advance the fixture ORIGIN's default branch so the branch's real base is FRESH.
    fs.writeFileSync(path.join(fx.originPath, "ADVANCE.txt"), "moved on\n");
    gitIn(fx.originPath, ["add", "ADVANCE.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "advance default"]);
    const freshHead = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(freshHead, initialHead, "precondition: fresh head differs from initial");

    // Seed a first runner clone off the FRESH default, commit, push agent/issue-<issue> to origin.
    await git.ensureClone(fx.originPath);
    const first = await git.createOrAttachRunnerClone(bare, issue);
    assert.strictEqual(first.baseCommit, freshHead, "precondition: first seed is off the fresh default");
    fs.writeFileSync(path.join(first.path, "WORK.txt"), "w\n");
    gitIn(first.path, ["add", "WORK.txt"]);
    gitIn(first.path, [...IDENT, "commit", "-m", "branch work"]);
    const branchTip = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, `agent/issue-${issue}`, "run-fixture");
    await git.pushBranch(bare, `agent/issue-${issue}`, "", fx.originPath);
    await git.removeRunnerClone(first.path);

    // Stale the fresh default tracking refs so defaultBranchRef falls through to the frozen
    // refs/heads/main rung (still the initial commit) — the exact #313 stale-ancestor topology.
    await git.ensureClone(fx.originPath);
    gitIn(bare, ["symbolic-ref", "-d", "refs/remotes/origin/HEAD"]);
    gitIn(bare, ["update-ref", "-d", "refs/remotes/origin/main"]);
    const frozen = gitIn(bare, ["rev-parse", "refs/heads/main"]);
    assert.strictEqual(frozen, initialHead, "precondition: frozen mirror is the initial commit");

    // Reseed: a resume whose defaultBranchCommit resolves through the frozen rung to the stale
    // initial commit, so the clamp fires and pins origin/main to the fresh branch base.
    const rc = await git.createOrAttachRunnerClone(bare, issue);
    assert.strictEqual(rc.baseCommit, branchTip, "precondition: resume base is the fresh branch tip");
    assert.notStrictEqual(rc.defaultBranchCommit, rc.baseCommit, "precondition: stale STRICT ancestor, not equal");
    return { rc, frozen };
  };

  it("the clamp survives a later `git fetch origin main` / `git fetch origin` (no refspec re-applies it) (#363)", async () => {
    const { rc } = await buildStaleAncestorClone(363);

    // The clamp is applied: origin/main is the branch base, not the stale frozen ancestor.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "precondition: origin/main is clamped to the branch base before any fetch",
    );

    // An agent runs BOTH fetch shapes inside the runner clone. Without the refspec removal these
    // drag origin/main back to the frozen mirror head; with it, they touch only FETCH_HEAD.
    gitIn(rc.path, ["fetch", "origin", "main"]);
    gitIn(rc.path, ["fetch", "origin"]);

    // The clamp still holds — origin/main is the branch base, and merge-base(origin/main, HEAD)
    // is the branch base, so the ratchet still gates only branch-introduced findings.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "origin/main must STILL be the branch base after fetch — a re-applied refspec would move it back",
    );
    assert.strictEqual(
      gitIn(rc.path, ["merge-base", "refs/remotes/origin/main", "HEAD"]),
      rc.baseCommit,
      "merge-base(origin/main, HEAD) must still be the branch base after fetch (ratchet base intact)",
    );
  });

  // Negative control: with the wildcard refspec RE-ADDED to the very same clone, the identical
  // `git fetch origin main` DOES drag origin/main back to the frozen bare mirror head — proving the
  // corruption is real and that the test above cannot pass vacuously. It is the ABSENCE of the
  // refspec (the #363 fix) that protects the clamp, nothing else in the topology.
  it("negative control: re-adding the wildcard refspec lets `git fetch` corrupt origin/main back to the frozen mirror (#363)", async () => {
    const { rc, frozen } = await buildStaleAncestorClone(3631);

    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "precondition: origin/main is clamped to the branch base before the refspec is re-added",
    );
    assert.notStrictEqual(rc.baseCommit, frozen, "precondition: the branch base differs from the frozen mirror head");

    // Re-add the exact refspec a plain clone carries, which the #363 fix removed.
    gitIn(rc.path, ["config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"]);
    gitIn(rc.path, ["fetch", "origin", "main"]);

    // The refspec dragged origin/main back to the frozen mirror head — the clamp is undone.
    assert.strictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      frozen,
      "with the refspec present, fetch moves origin/main to the frozen bare mirror head",
    );
    assert.notStrictEqual(
      gitIn(rc.path, ["rev-parse", "refs/remotes/origin/main"]),
      rc.baseCommit,
      "with the refspec present, origin/main is no longer the branch base — the clamp is corrupted",
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

describe("reviewDiff (PRD #400 M4b diff-review)", () => {
  // Build a branch off main with N commits, pushed into the origin fixture BEFORE the
  // bare clone so ensureClone mirrors it into refs/remotes/origin/<branch>.
  function commitOnBranch(branch: string, files: Record<string, string>, message: string): void {
    gitIn(fx.originPath, ["checkout", "-b", branch, "main"]);
    for (const [rel, content] of Object.entries(files)) {
      fs.writeFileSync(path.join(fx.originPath, rel), content);
      gitIn(fx.originPath, ["add", rel]);
    }
    gitIn(fx.originPath, ["commit", "-m", message]);
  }

  it("returns the three-dot diff of a two-commit branch against its base", async () => {
    gitIn(fx.originPath, ["checkout", "-b", "uzi/task/tgt", "main"]);
    fs.writeFileSync(path.join(fx.originPath, "A.txt"), "alpha\n");
    gitIn(fx.originPath, ["add", "A.txt"]);
    gitIn(fx.originPath, ["commit", "-m", "add A"]);
    fs.writeFileSync(path.join(fx.originPath, "B.txt"), "beta\n");
    gitIn(fx.originPath, ["add", "B.txt"]);
    gitIn(fx.originPath, ["commit", "-m", "add B"]);
    gitIn(fx.originPath, ["checkout", "main"]); // leave origin on main

    const bare = await git.ensureClone(fx.originPath);
    const diff = await git.reviewDiff(bare, "main", "uzi/task/tgt");

    // Both commits' changes are present (three-dot from the merge base), added content too.
    assert.match(diff, /A\.txt/);
    assert.match(diff, /B\.txt/);
    assert.match(diff, /\+alpha/);
    assert.match(diff, /\+beta/);
    // The base's own README is unchanged on the branch, so it must NOT appear in the diff.
    assert.ok(!diff.includes("README.md"), "unchanged base files are absent from the diff");
  });

  it("truncates past the byte cap with a marker", async () => {
    // ~650 KiB of added content, comfortably past the 512 KiB cap.
    const big = ("y".repeat(64) + "\n").repeat(10_000);
    commitOnBranch("uzi/task/big", { "BIG.txt": big }, "add BIG");
    gitIn(fx.originPath, ["checkout", "main"]);

    const bare = await git.ensureClone(fx.originPath);
    const diff = await git.reviewDiff(bare, "main", "uzi/task/big");

    assert.match(diff, /diff truncated at \d+ bytes/);
    // The kept payload is bounded near the cap (512 KiB + the short marker), not the full
    // ~650 KiB diff.
    assert.ok(
      Buffer.byteLength(diff, "utf8") <= 512 * 1024 + 256,
      `diff should be capped near 512 KiB, got ${Buffer.byteLength(diff, "utf8")} bytes`,
    );
  });

  it("returns an empty string when the branch matches its base", async () => {
    // A branch at the same commit as main — nothing changed.
    gitIn(fx.originPath, ["branch", "uzi/task/same", "main"]);
    const bare = await git.ensureClone(fx.originPath);
    const diff = await git.reviewDiff(bare, "main", "uzi/task/same");
    assert.equal(diff.trim(), "");
  });

  // issue #403 F3: a handoff created without --base records the SEED COMMIT sha (not a branch
  // name) as its base, so the review diffs only the worker's commits on top of the seed, not
  // the user's seeded HEAD. That raw sha never resolves under refs/remotes/origin/<sha>, so
  // reviewDiff must fall back to using it verbatim as a commit-ish present in the mirror.
  it("resolves a raw commit sha base (the handoff seed commit) — diffs only the post-seed commits", async () => {
    // Build the branch: a SEED commit (the user's seeded HEAD), then two worker commits on top.
    gitIn(fx.originPath, ["checkout", "-b", "uzi/task/sha", "main"]);
    fs.writeFileSync(path.join(fx.originPath, "SEED.txt"), "seeded\n");
    gitIn(fx.originPath, ["add", "SEED.txt"]);
    gitIn(fx.originPath, ["commit", "-m", "seed commit"]);
    const seedSha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    // Worker commits ON TOP of the seed.
    fs.writeFileSync(path.join(fx.originPath, "WORK1.txt"), "one\n");
    gitIn(fx.originPath, ["add", "WORK1.txt"]);
    gitIn(fx.originPath, ["commit", "-m", "worker commit 1"]);
    fs.writeFileSync(path.join(fx.originPath, "WORK2.txt"), "two\n");
    gitIn(fx.originPath, ["add", "WORK2.txt"]);
    gitIn(fx.originPath, ["commit", "-m", "worker commit 2"]);
    gitIn(fx.originPath, ["checkout", "main"]); // leave origin on main

    const bare = await git.ensureClone(fx.originPath);
    // Precondition: the seed sha is present in the mirror (as an ancestor of the reviewed
    // branch) but NOT under refs/remotes/origin/<sha> — the exact shape the fallback handles.
    assert.strictEqual(gitIn(bare, ["cat-file", "-t", seedSha]), "commit");
    const diff = await git.reviewDiff(bare, seedSha, "uzi/task/sha");

    // Only the worker's post-seed commits appear; the seed's own content is the base and absent.
    assert.match(diff, /WORK1\.txt/);
    assert.match(diff, /WORK2\.txt/);
    assert.match(diff, /\+one/);
    assert.match(diff, /\+two/);
    assert.ok(!diff.includes("SEED.txt"), "the seed commit's own content is the base, absent from the diff");
  });
});

describe("planChangedFiles (PRD #212)", () => {
  // The SECURITY-load-bearing test: the gate's `git status --porcelain` MUST go through
  // the runner-uid primitive runGitAsRunner, NEVER a worker-uid helper (runGit/tryGit/
  // tryGitStdout), because status touches the working tree and can fire attacker-chosen
  // .gitattributes clean drivers that exec as the running uid (PRD #51 M0). This is a
  // METHOD-ROUTING spy, not an argv/setpriv check — in the single-uid unit harness
  // uidSplitActive() is false so a runner-uid and a worker-uid `git status` spawn
  // IDENTICAL argv (runner-uid.test.ts:32), which makes an argv inspection vacuous.
  type Priv = {
    runGitAsRunner: (cwd: string | undefined, args: string[]) => Promise<string>;
    runGit: (...a: unknown[]) => Promise<string>;
    tryGit: (...a: unknown[]) => Promise<number>;
    tryGitStdout: (...a: unknown[]) => Promise<string>;
  };

  it("routes through runGitAsRunner, never a worker-uid helper, and preserves the leading status space", async () => {
    const calls: Array<{ cwd: string | undefined; args: string[] }> = [];
    const priv = git as unknown as Priv;
    priv.runGitAsRunner = async (cwd, args) => {
      calls.push({ cwd, args });
      return " M src/app.ts\n?? notes.md\n";
    };
    // Worker-uid helpers MUST NOT be reached — replace each with a throwing spy.
    priv.runGit = async () => {
      throw new Error("worker-uid runGit must not be called for a plan-turn status");
    };
    priv.tryGit = async () => {
      throw new Error("worker-uid tryGit must not be called for a plan-turn status");
    };
    priv.tryGitStdout = async () => {
      throw new Error("worker-uid tryGitStdout must not be called for a plan-turn status");
    };

    const result = await git.planChangedFiles("/some/clone");

    assert.strictEqual(calls.length, 1, "runGitAsRunner called exactly once");
    assert.strictEqual(calls[0]!.cwd, "/some/clone");
    assert.deepStrictEqual(calls[0]!.args, ["status", "--porcelain"]);
    assert.deepStrictEqual(result, [" M src/app.ts", "?? notes.md"]);
    // A leading-space assertion catches any accidental trim of the XY status code.
    assert.strictEqual(result[0], " M src/app.ts");
  });

  it("is best-effort: resolves to [] (never throws) when the status call fails", async () => {
    const priv = git as unknown as Priv;
    priv.runGitAsRunner = async () => {
      throw new Error("git status --porcelain failed: hostile tree");
    };
    const result = await git.planChangedFiles("/some/clone");
    assert.deepStrictEqual(result, []);
  });

  it("parses real porcelain output from a temp repo (untracked file → '?? <file>')", async () => {
    // Under single-uid the harness runGitAsRunner is a passthrough to real git, so this
    // exercises the real `git status --porcelain` parse, not just the spy.
    const repo = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-plan-status-"));
    try {
      gitIn(repo, ["init", "-q", "-b", "main"]);
      gitIn(repo, ["config", "user.email", "t@t"]);
      gitIn(repo, ["config", "user.name", "t"]);
      gitIn(repo, ["config", "commit.gpgsign", "false"]);
      fs.writeFileSync(path.join(repo, "README.md"), "seed\n");
      gitIn(repo, ["add", "README.md"]);
      gitIn(repo, ["commit", "-q", "-m", "seed"]);
      // An untracked file the plan turn "wrote".
      fs.writeFileSync(path.join(repo, "notes.md"), "planning\n");

      const result = await git.planChangedFiles(repo);
      assert.deepStrictEqual(result, ["?? notes.md"]);
    } finally {
      fs.rmSync(repo, { recursive: true, force: true });
    }
  });
});

describe("worktreeStatus (issue #281 / CodeRabbit #655)", () => {
  // The fingerprint-specific porcelain read. Same git invocation as planChangedFiles, but
  // it must return NULL on a failed read — NOT [] — so an unreadable status is never
  // mistaken for a clean tree by the no-progress detector.
  type Priv = { runGitAsRunner: (cwd: string | undefined, args: string[]) => Promise<string> };

  it("returns the porcelain lines on success (leading status space preserved)", async () => {
    const calls: Array<{ cwd: string | undefined; args: string[] }> = [];
    (git as unknown as Priv).runGitAsRunner = async (cwd, args) => {
      calls.push({ cwd, args });
      return " M src/app.ts\n?? notes.md\n";
    };
    const result = await git.worktreeStatus("/some/clone");
    assert.deepStrictEqual(calls[0]!.args, ["status", "--porcelain"]);
    assert.deepStrictEqual(result, [" M src/app.ts", "?? notes.md"]);
  });

  it("returns NULL (not []) when the status read fails, so a failed read is not read as clean", async () => {
    (git as unknown as Priv).runGitAsRunner = async () => {
      throw new Error("git status --porcelain failed: unreadable tree");
    };
    const result = await git.worktreeStatus("/some/clone");
    assert.strictEqual(result, null);
  });
});

// Issue #781 — the worker must not seed a run off a STALE, DISJOINT remote ref, and a
// remote branch deleted upstream must stop seeding stale bases (fetch --prune). Two
// independent guards, both proven here against real local git.
describe("issue #781 — disjoint-ref seed guard + fetch --prune", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
  function refInBare(bare: string, ref: string): boolean {
    try {
      gitIn(bare, ["rev-parse", "--verify", "--quiet", ref]);
      return true;
    } catch {
      return false;
    }
  }

  it("rejects a disjoint origin branch ref and seeds off the default tip instead", async () => {
    // Build a DISJOINT branch at origin matching the seed branch name for issue 55 — an
    // orphan root, so it shares no history (and no content) with main.
    gitIn(fx.originPath, ["checkout", "--orphan", "agent/issue-55"]);
    gitIn(fx.originPath, ["rm", "-rf", "."]);
    fs.writeFileSync(path.join(fx.originPath, "ORPHAN.txt"), "x\n");
    gitIn(fx.originPath, ["add", "."]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "orphan disjoint history"]);
    gitIn(fx.originPath, ["checkout", "main"]);

    // ensureClone fetches refs/remotes/origin/agent/issue-55 into the bare.
    const bare = await git.ensureClone(fx.originPath);
    assert.strictEqual(
      refInBare(bare, "refs/remotes/origin/agent/issue-55"),
      true,
      "precondition: the disjoint ref is fetched into the bare",
    );
    // …and it genuinely shares NO history with default: plain merge-base prints nothing and
    // exits non-zero (gitIn throws on non-zero, hence the try/catch).
    let disjoint = false;
    try {
      disjoint = gitIn(bare, ["merge-base", "refs/remotes/origin/agent/issue-55", "refs/remotes/origin/main"]) === "";
    } catch {
      disjoint = true;
    }
    assert.strictEqual(disjoint, true, "precondition: the ref is genuinely disjoint from default");

    const disjointTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/agent/issue-55"]);
    const defaultTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);

    const rc = await git.createOrAttachRunnerClone(bare, 55);
    assert.strictEqual(rc.seededFrom, "default", "a disjoint origin ref is ignored; the seed falls back to default");
    assert.strictEqual(rc.priorCommits, 0, "a default seed carries no prior commits");
    assert.strictEqual(rc.baseCommit, defaultTip, "seeded off the fresh default tip");
    assert.notStrictEqual(rc.baseCommit, disjointTip, "…explicitly NOT the disjoint ref tip");
  });

  it("keeps a far-ahead owned tracking ref that merely diverges from default (guard is not over-aggressive)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Seed a first runner clone off the initial default and build a tracking ref several
    // commits ahead, owned by run-far. It forks off main's initial commit — so it merely
    // DIVERGES from default; it is not disjoint.
    const first = await git.createOrAttachRunnerClone(bare, 66, "run-far");
    for (const f of ["F1.txt", "F2.txt", "F3.txt"]) {
      fs.writeFileSync(path.join(first.path, f), `${f}\n`);
      gitIn(first.path, ["add", f]);
      gitIn(first.path, [...IDENT, "commit", "-m", `work ${f}`]);
    }
    const tip = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, "agent/issue-66", "run-far"); // tracking owned by run-far
    await git.removeRunnerClone(first.path);

    // Advance origin's main on its OWN line so the tracking ref genuinely diverges (their
    // only common ancestor is the initial commit — neither descends the other).
    fs.writeFileSync(path.join(fx.originPath, "MAIN.txt"), "m\n");
    gitIn(fx.originPath, ["add", "MAIN.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "advance main on its own line"]);
    await git.ensureClone(fx.originPath); // bare learns the fresh divergent main

    // PRECONDITION: the tracking ref shares history with default (a common ancestor exists)
    // but is NOT an ancestor of it — the exact case merge-base --is-ancestor would reject,
    // and the reason sharesHistory uses plain merge-base rather than isAncestor.
    assert.doesNotThrow(
      () => gitIn(bare, ["merge-base", "refs/uzi-runner/agent/issue-66", "refs/remotes/origin/main"]),
      "precondition: tracking ref shares history with default (plain merge-base succeeds)",
    );
    assert.throws(
      () => gitIn(bare, ["merge-base", "--is-ancestor", "refs/uzi-runner/agent/issue-66", "refs/remotes/origin/main"]),
      "precondition: the far-ahead tracking ref is NOT an ancestor of default (is-ancestor would reject it)",
    );

    // Reseed as the SAME run: the guard KEEPS the diverged tracking ref, so it seeds off it.
    const rc = await git.createOrAttachRunnerClone(bare, 66, "run-far");
    assert.strictEqual(rc.seededFrom, "tracking", "a merely-diverged owned tracking ref must be kept, not rejected");
    assert.strictEqual(rc.baseCommit, tip, "…and seeded off the tracking tip");
  });

  it("prunes a deleted origin branch's tracking ref while leaving refs/uzi-runner/* and refs/uzi-checkpoints/* intact", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // Create a throwaway branch at origin and fetch it into the bare.
    gitIn(fx.originPath, ["branch", "throwaway-781", "main"]);
    await git.ensureClone(fx.originPath);
    assert.strictEqual(
      refInBare(bare, "refs/remotes/origin/throwaway-781"),
      true,
      "precondition: the throwaway branch's tracking ref is fetched",
    );

    // Differential setup (catches a config-form regression): plant two custom refs whose
    // origin counterparts do NOT exist. The config form (fetch.prune / remote.origin.prune)
    // would sweep refs/uzi-checkpoints/* on the separate checkpoint mirror fetch; the
    // --prune FLAG confines pruning to refs/remotes/origin/*, so both must survive.
    const mainSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent/issue-999", mainSha]);
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-999", mainSha]);

    // Delete the branch at origin, then refetch: --prune removes the now-dangling tracking ref.
    gitIn(fx.originPath, ["branch", "-D", "throwaway-781"]);
    await git.ensureClone(fx.originPath);

    assert.strictEqual(
      refInBare(bare, "refs/remotes/origin/throwaway-781"),
      false,
      "the deleted origin branch's tracking ref must be pruned (#781)",
    );
    // The differential assertion: custom refs with no origin counterpart SURVIVE the prune —
    // the config form would drop refs/uzi-checkpoints/agent/issue-999 on the mirror fetch.
    assert.strictEqual(
      refInBare(bare, "refs/uzi-runner/agent/issue-999"),
      true,
      "refs/uzi-runner/* must survive the prune (#781)",
    );
    assert.strictEqual(
      refInBare(bare, "refs/uzi-checkpoints/agent/issue-999"),
      true,
      "refs/uzi-checkpoints/* must survive the prune (#781)",
    );
  });
});

// Issue #887 — a self_improve run's fetch-back writes the tracking ref
// refs/uzi-runner/uzi/self-improve/<runId>. On a worker whose persistent bare still
// carries the pre-#774 FLAT leaf ref refs/uzi-runner/uzi/self-improve, that leaf is a
// strict path-prefix (directory ancestor) of the new dst, so the ref-store cannot create
// the dst directory and the whole fetch aborts ("some local refs could not be updated").
// fetchAgentBranch must clear the conflicting ancestor first (archiving its tip), land the
// agent commit, and leave unrelated sibling tracking refs untouched.
describe("issue #887 — fetchAgentBranch clears a D/F-conflicting legacy ancestor tracking ref", () => {
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
  function refInBare(bare: string, ref: string): boolean {
    try {
      gitIn(bare, ["rev-parse", "--verify", "--quiet", ref]);
      return true;
    } catch {
      return false;
    }
  }

  it("archives + deletes the flat ancestor, fetches the self-improve branch, and spares siblings", async () => {
    const bare = await git.ensureClone(fx.originPath);

    // Seed the legacy pre-#774 FLAT leaf ref at main's commit — the ancestor that
    // path-blocks refs/uzi-runner/uzi/self-improve/<runId>.
    const mainSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    gitIn(bare, ["update-ref", "refs/uzi-runner/uzi/self-improve", mainSha]);
    // Its dangling PRD #218 owner stamp (sanitized branch "uzi-self-improve").
    gitIn(bare, ["config", "--local", "uzi-trackowner.uzi-self-improve", "old-run"]);
    // An UNRELATED sibling tracking ref that must SURVIVE the clear.
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent/issue-999", mainSha]);

    const runId = "37702d9d-887d-49c9-b3cd-7974a3b2ecde";
    const branch = `uzi/self-improve/${runId}`;
    const rc = await git.runnerCloneForBranch(bare, branch, `self-improve-${runId}`, runId);

    // The agent commits in the runner clone.
    fs.writeFileSync(path.join(rc.path, "SI.txt"), "self-improve\n");
    gitIn(rc.path, ["add", "SI.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "self-improve work"]);
    const agentSha = gitIn(rc.path, ["rev-parse", "HEAD"]);

    // Without the fix this THROWS ("some local refs could not be updated"); with it the
    // ancestor is cleared first and the fetch lands.
    const ref = await git.fetchAgentBranch(bare, rc.path, branch, runId);

    assert.strictEqual(ref, `refs/uzi-runner/${branch}`);
    assert.strictEqual(gitIn(bare, ["rev-parse", ref]), agentSha, "the self-improve commit landed in the worker bare");
    // The flat ancestor is gone (it path-blocked dst).
    assert.strictEqual(
      refInBare(bare, "refs/uzi-runner/uzi/self-improve"),
      false,
      "the D/F-conflicting flat ancestor must be deleted",
    );
    // Its tip was archived first so a possibly-unmerged commit is never lost.
    assert.strictEqual(
      gitIn(bare, ["rev-parse", `refs/uzi-archive/uzi-self-improve/${mainSha}`]),
      mainSha,
      "the deleted ancestor's tip must be archived under refs/uzi-archive/<sanitized>/<sha>",
    );
    // The dangling owner-config key was cleared (--get now exits non-zero → gitIn throws).
    assert.throws(
      () => gitIn(bare, ["config", "--local", "--get", "uzi-trackowner.uzi-self-improve"]),
      "the dangling PRD #218 owner stamp must be unset",
    );
    // The unrelated sibling tracking ref is untouched.
    assert.strictEqual(
      refInBare(bare, "refs/uzi-runner/agent/issue-999"),
      true,
      "an unrelated sibling tracking ref must survive",
    );
  });
});
