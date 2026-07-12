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

describe("worktree lifecycle", () => {
  it("creates a worktree on agent/issue-N off the default branch", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 42);

    assert.strictEqual(wt.branch, "agent/issue-42");
    assert.strictEqual(fs.existsSync(path.join(wt.path, "README.md")), true); // origin content checked out
    // A worktree's .git is a file pointing at the shared repo, not a directory.
    assert.strictEqual(fs.statSync(path.join(wt.path, ".git")).isFile(), true);
    assert.match(gitIn(bare, ["rev-parse", "--verify", "refs/heads/agent/issue-42"]), /^[0-9a-f]{40}$/);
  });

  it("attaches to an existing agent/issue-N branch on a later run", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const first = await git.createOrAttachWorktree(bare, 7);

    // Advance the branch beyond the default so attach-vs-recreate is observable.
    fs.writeFileSync(path.join(first.path, "EXTRA.txt"), "x");
    gitIn(first.path, ["add", "EXTRA.txt"]);
    gitIn(first.path, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "extra"]);
    const advancedSha = gitIn(first.path, ["rev-parse", "HEAD"]);

    await git.removeWorktree(bare, first.path);
    const second = await git.createOrAttachWorktree(bare, 7);

    assert.strictEqual(second.branch, "agent/issue-7");
    assert.strictEqual(second.path, first.path);
    // Attached to the advanced branch (has EXTRA.txt), not recreated off default.
    assert.strictEqual(fs.existsSync(path.join(second.path, "EXTRA.txt")), true);
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/heads/agent/issue-7"]), advancedSha);
  });

  it("removes the worktree but keeps the bare clone and branch", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 5);

    await git.removeWorktree(bare, wt.path);

    assert.strictEqual(fs.existsSync(wt.path), false);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
    assert.match(gitIn(bare, ["rev-parse", "--verify", "refs/heads/agent/issue-5"]), /^[0-9a-f]{40}$/);
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
