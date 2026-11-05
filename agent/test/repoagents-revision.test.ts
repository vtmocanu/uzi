import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache } from "../src/git.js";
import { detectRepoAgentsAtRevision } from "../src/repoagents.js";
import { hermeticGitEnv, nullLogger, testGitCacheOptions } from "./helpers.js";

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { env: hermeticGitEnv(), encoding: "utf8" }).trim();

const agent = (name: string, body: string) => `---\nname: ${name}\ndescription: ${name} agent.\n---\n\n${body}\n`;

describe("detectRepoAgentsAtRevision over git objects (issue #3913)", () => {
  let base: string;
  let repo: string;
  let git: GitCache;

  before(() => {
    base = fs.mkdtempSync(path.join(os.tmpdir(), "cdr-3913-rev-"));
    repo = path.join(base, "repo");
    execFileSync("git", ["init", "-q", "-b", "main", repo], { env: hermeticGitEnv() });
    git = new GitCache(path.join(base, "data"), nullLogger(), undefined, testGitCacheOptions());
  });
  after(() => fs.rmSync(base, { recursive: true, force: true }));

  it("test git ignores an inherited repository location", () => {
    const saved = { dir: process.env.GIT_DIR, tree: process.env.GIT_WORK_TREE };
    process.env.GIT_DIR = path.join(base, "foreign.git");
    process.env.GIT_WORK_TREE = base;
    try {
      assert.equal(fs.realpathSync(gitIn(repo, ["rev-parse", "--absolute-git-dir"])), fs.realpathSync(path.join(repo, ".git")));
    } finally {
      if (saved.dir === undefined) delete process.env.GIT_DIR;
      else process.env.GIT_DIR = saved.dir;
      if (saved.tree === undefined) delete process.env.GIT_WORK_TREE;
      else process.env.GIT_WORK_TREE = saved.tree;
    }
  });

  function commit(files: Record<string, string | { symlink: string }>): string {
    for (const [rel, content] of Object.entries(files)) {
      const full = path.join(repo, rel);
      fs.rmSync(full, { recursive: true, force: true });
      fs.mkdirSync(path.dirname(full), { recursive: true });
      if (typeof content === "string") fs.writeFileSync(full, content);
      else fs.symlinkSync(content.symlink, full);
    }
    gitIn(repo, ["add", "-A"]);
    gitIn(repo, [...IDENT, "commit", "-q", "--allow-empty", "-m", "c"]);
    return gitIn(repo, ["rev-parse", "HEAD"]);
  }

  it("reads the committed body, never the working tree", async () => {
    const rev = commit({ ".claude/agents/reviewer.md": agent("reviewer", "Committed body") });
    fs.writeFileSync(path.join(repo, ".claude/agents/reviewer.md"), agent("reviewer", "Working tree body"));
    fs.writeFileSync(path.join(repo, ".claude/agents/extra.md"), agent("extra", "Untracked"));
    const got = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, rev), "claude");
    assert.equal(got.folder, ".claude/agents");
    assert.deepEqual(got.agents.map((a) => [a.name, a.prompt_body.trim()]), [["reviewer", "Committed body"]]);
    gitIn(repo, ["checkout", "--", "."]);
    fs.rmSync(path.join(repo, ".claude/agents/extra.md"));
  });

  it("reads an older revision after the default branch moved on", async () => {
    const old = commit({ ".claude/agents/reviewer.md": agent("reviewer", "Old body") });
    commit({ ".claude/agents/reviewer.md": agent("reviewer", "New body") });
    const got = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, old), "claude");
    assert.equal(got.agents[0]?.prompt_body.trim(), "Old body");
  });

  it("prefers the harness's own folder and falls back to the other", async () => {
    const rev = commit({
      ".claude/agents/reviewer.md": agent("reviewer", "Claude body"),
      ".codex/agents/reviewer.toml": 'name = "codex-reviewer"\ndescription = "Codex."\ndeveloper_instructions = "Codex body"\n',
    });
    const codex = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, rev), "codex");
    assert.equal(codex.folder, ".codex/agents");
    assert.deepEqual(codex.agents.map((a) => a.name), ["codex-reviewer"]);
    commit({ ".codex": "" });
    const fallback = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, gitIn(repo, ["rev-parse", "HEAD"])), "codex");
    assert.equal(fallback.folder, ".claude/agents");
  });

  it("refuses a symlinked folder without falling back, and skips symlinked or oversized files", async () => {
    fs.rmSync(path.join(repo, ".codex"), { recursive: true, force: true });
    const rev = commit({
      "real/agents/x.md": agent("x", "Elsewhere"),
      ".codex": { symlink: "real" },
      ".claude/agents/linked.md": { symlink: "../../real/agents/x.md" },
      ".claude/agents/big.md": agent("big", "y".repeat(70 * 1024)),
    });
    const codex = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, rev), "codex");
    assert.deepEqual(codex, { agents: [], notes: [{ name: "", reason: "unsafe_path", folder: ".codex/agents" }], folder: ".codex/agents" });
    const claude = await detectRepoAgentsAtRevision(git.revisionTreeReader(repo, rev), "claude");
    assert.ok(!claude.agents.some((a) => a.name === "x" || a.name === "linked"));
    assert.ok(claude.notes.some((n) => n.name === "big" && n.reason === "too_large"));
  });

  it("an absent folder yields an empty roster and an unknown revision throws", async () => {
    const empty = fs.mkdtempSync(path.join(base, "empty-"));
    execFileSync("git", ["init", "-q", "-b", "main", empty], { env: hermeticGitEnv() });
    gitIn(empty, [...IDENT, "commit", "-q", "--allow-empty", "-m", "root"]);
    const rev = gitIn(empty, ["rev-parse", "HEAD"]);
    assert.deepEqual(await detectRepoAgentsAtRevision(git.revisionTreeReader(empty, rev), "claude"), { agents: [], notes: [], folder: null });
    await assert.rejects(detectRepoAgentsAtRevision(git.revisionTreeReader(empty, "0123456789abcdef0123456789abcdef01234567"), "claude"));
  });
});
