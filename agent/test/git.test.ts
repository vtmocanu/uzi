import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
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
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-7");
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

  it("resumes off the branch's origin tip when it already exists at origin", async () => {
    const bare = await git.ensureClone(fx.originPath);
    // First cycle: commit + fetch-back + push agent/issue-9 to origin.
    const first = await git.createOrAttachRunnerClone(bare, 9);
    fs.writeFileSync(path.join(first.path, "A.txt"), "a\n");
    gitIn(first.path, ["add", "A.txt"]);
    gitIn(first.path, [...IDENT, "commit", "-m", "a"]);
    const sha1 = gitIn(first.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, first.path, "agent/issue-9");
    await git.pushBranch(bare, "agent/issue-9", "", fx.originPath);
    await git.removeRunnerClone(first.path);

    // Refresh the bare so origin-tracking learns agent/issue-9, then reseed.
    await git.ensureClone(fx.originPath);
    const second = await git.createOrAttachRunnerClone(bare, 9);

    assert.strictEqual(second.branch, "agent/issue-9");
    // Resumed off the fresh origin tip (A.txt present), NOT recreated off default.
    assert.strictEqual(fs.existsSync(path.join(second.path, "A.txt")), true);
    assert.strictEqual(gitIn(second.path, ["rev-parse", "HEAD"]), sha1);
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
        `${label}: gc.auto=0 covers git predating the maintenance rework, where the detaching ` +
          `process is \`git gc --auto\` itself`,
      );
    }
  });

  it("removes the runner clone but keeps the worker bare clone", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 5);

    await git.removeRunnerClone(rc.path);

    assert.strictEqual(fs.existsSync(rc.path), false);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
  });
});

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

  it("neutralizes hooks: core.hooksPath is the baked root-owned path, NOT a runtime dir", () => {
    const cfg = configPairs(gitEnv());
    assert.equal(cfg["safe.directory"], "*");
    // The empty hooks dir is BAKED into the image as root-owned 0555
    // (agent/templates/base/Dockerfile) — the uzi uid (which the agent shares) cannot
    // create a hook inside it. A runtime-created dir would be under the shared uid and
    // agent-writable, which is the vector this closes. The filesystem perms can't be
    // unit-tested portably (that's the Dockerfile stanza's job); here we pin that
    // git.ts uses the constant baked path and does NOT create it at runtime.
    assert.equal(cfg["core.hooksPath"], "/usr/share/uzi-git-nohooks", "must be the baked constant path");
    assert.ok(
      !fs.existsSync("/usr/share/uzi-git-nohooks"),
      "git.ts must NOT mkdir the hooks dir at runtime (it exists only in the image)",
    );
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
