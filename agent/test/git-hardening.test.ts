import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, gitEnv } from "../src/git.js";

// PRD #51 M0 — standalone shared-git hardening (no uid split). A process that can
// write a git config source a worker-side git later reads (today: agent-uid over
// the shared `agentdata` volume, `<bare>/config`) could plant a FIXED-name
// code-exec key — core.fsmonitor / diff.external / core.pager / core.sshCommand —
// and get code execution in the worker's non-credentialed `git diff`
// (changedFiles) or `worktree add` (checkout). gitEnv must neutralize each via an
// inline GIT_CONFIG override, plus GIT_CONFIG_NOSYSTEM and a /dev/null
// GIT_CONFIG_GLOBAL default so no unexpected config source is consulted.

// Collect the inline GIT_CONFIG_* pairs into a key→value map.
function configPairs(env: NodeJS.ProcessEnv): Record<string, string> {
  const out: Record<string, string> = {};
  const n = Number(env.GIT_CONFIG_COUNT ?? "0");
  for (let i = 0; i < n; i++) out[env[`GIT_CONFIG_KEY_${i}`]!] = env[`GIT_CONFIG_VALUE_${i}`]!;
  return out;
}

// The FIXED-name code-exec keys M0 pins, with their verified inert override value.
const EXPECTED_PINS: Record<string, string> = {
  "core.fsmonitor": "false",
  "diff.external": "true",
  "core.pager": "cat",
  "core.sshCommand": "ssh",
};

describe("gitEnv M0 hardening (unit)", () => {
  const SAVED = process.env.GIT_CONFIG_GLOBAL;
  afterEach(() => {
    if (SAVED === undefined) delete process.env.GIT_CONFIG_GLOBAL;
    else process.env.GIT_CONFIG_GLOBAL = SAVED;
  });

  it("sets GIT_CONFIG_NOSYSTEM on both the pat and no-pat paths", () => {
    assert.equal(gitEnv().GIT_CONFIG_NOSYSTEM, "1");
    assert.equal(gitEnv("secret-pat", "https://gitlab.example.com/", "bot").GIT_CONFIG_NOSYSTEM, "1");
  });

  it("defaults GIT_CONFIG_GLOBAL to /dev/null when unset, but passes an explicit value through", () => {
    delete process.env.GIT_CONFIG_GLOBAL;
    assert.equal(gitEnv().GIT_CONFIG_GLOBAL, "/dev/null");
    // The e2e overlay's insteadOf-rewrite file must still be honored.
    process.env.GIT_CONFIG_GLOBAL = "/some/e2e/gitconfig";
    assert.equal(gitEnv().GIT_CONFIG_GLOBAL, "/some/e2e/gitconfig");
  });

  it("pins every fixed-name code-exec key unconditionally (no-pat path)", () => {
    const cfg = configPairs(gitEnv());
    for (const [k, v] of Object.entries(EXPECTED_PINS)) {
      assert.equal(cfg[k], v, `${k} must be pinned to ${v} even with no PAT (covers changedFiles/worktree add)`);
    }
  });

  it("keeps the same pins on the pat path, alongside the credential header", () => {
    const cfg = configPairs(gitEnv("secret-pat", "https://gitlab.example.com/", "bot"));
    for (const [k, v] of Object.entries(EXPECTED_PINS)) assert.equal(cfg[k], v, `${k} must stay pinned with a PAT`);
    assert.match(cfg["http.https://gitlab.example.com/.extraHeader"] ?? "", /^Authorization: Basic /);
  });

  it("leaves the existing safe.directory / core.hooksPath / scrub behavior unchanged", () => {
    const env = gitEnv();
    const cfg = configPairs(env);
    assert.equal(cfg["safe.directory"], "*");
    assert.equal(cfg["core.hooksPath"], "/usr/share/uzi-git-nohooks");
    assert.equal(env.PATH, process.env.PATH);
    assert.equal(env.GIT_TERMINAL_PROMPT, "0");
    // The worker/API vars are still absent by construction.
    for (const k of ["UZI_WORKER_TOKEN", "UZI_WORKER_TOKEN_FILE", "UZI_API_URL"]) assert.equal(env[k], undefined);
  });
});

// Is a real git available? The existing git*.test.ts already hard-depend on it,
// so it should be; guard the functional PoC anyway and fail loudly if not.
function gitAvailable(): boolean {
  try {
    execFileSync("git", ["--version"], { stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
}

describe("gitEnv M0 hardening: code-exec keys neutralized in real git (functional PoC)", () => {
  let fx: Fixture;
  let git: GitCache;
  let markerDir: string;
  let marker: string;
  let evil: string;

  // A pin-LESS env that still isolates from the host — used to prove the plant
  // really fires (the baseline vector) so a "not fired" under gitEnv is meaningful.
  function plainEnv(): NodeJS.ProcessEnv {
    return {
      PATH: process.env.PATH,
      HOME: process.env.HOME,
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_SYSTEM: "/dev/null",
      GIT_TERMINAL_PROMPT: "0",
    };
  }

  function fired(): boolean {
    return fs.existsSync(marker);
  }
  function resetMarker(): void {
    fs.rmSync(marker, { force: true });
  }
  // Plant a code-exec key straight into the bare repo's on-disk config (a `printf >`
  // equivalent, bypassing the guardrail screen), simulating the same-uid write.
  function plant(bare: string, key: string, value: string): void {
    execFileSync("git", ["-C", bare, "config", key, value], { env: plainEnv(), stdio: "pipe" });
  }
  function unplant(bare: string, key: string): void {
    execFileSync("git", ["-C", bare, "config", "--unset-all", key], { env: plainEnv(), stdio: "pipe" });
  }
  function runGit(cwd: string, args: string[], env: NodeJS.ProcessEnv): void {
    try {
      execFileSync("git", ["-C", cwd, ...args], { env, stdio: "pipe" });
    } catch {
      // A non-zero exit is fine; we only care whether the marker fired.
    }
  }

  beforeEach(() => {
    if (!gitAvailable()) return;
    fx = makeFixture();
    git = new GitCache(fx.dataDir, nullLogger());
    markerDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m0-marker-"));
    marker = path.join(markerDir, "FIRED");
    evil = path.join(markerDir, "evil.sh");
    fs.writeFileSync(evil, `#!/bin/sh\necho pwned >> ${JSON.stringify(marker)}\nexit 0\n`);
    fs.chmodSync(evil, 0o755);
  });

  afterEach(() => {
    fx?.cleanup();
    if (markerDir) fs.rmSync(markerDir, { recursive: true, force: true });
  });

  it("diff.external planted in <bare>/config does NOT code-exec in a full `git diff` (baseline proves it would)", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 99);
    // A second commit so `git diff HEAD~1..HEAD` has content for the external driver.
    fs.writeFileSync(path.join(wt.path, "README.md"), "# fixture\nchanged\n");
    runGit(wt.path, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-am", "c2"], plainEnv());

    plant(bare, "diff.external", evil);

    // Baseline: a pin-less env runs the planted command — the vector is real.
    resetMarker();
    runGit(wt.path, ["diff", "HEAD~1..HEAD"], plainEnv());
    assert.equal(fired(), true, "baseline: planted diff.external must fire without the pin (test would be vacuous otherwise)");

    // gitEnv's inline diff.external=true overrides the plant → no code-exec.
    resetMarker();
    runGit(wt.path, ["diff", "HEAD~1..HEAD"], gitEnv());
    assert.equal(fired(), false, "gitEnv must neutralize the planted diff.external");
  });

  it("core.fsmonitor planted in <bare>/config does NOT code-exec in `git status` or `worktree add` checkout", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 100);

    plant(bare, "core.fsmonitor", evil);

    // Baseline: fsmonitor fires on an index-refreshing command without the pin.
    resetMarker();
    runGit(wt.path, ["status", "--porcelain"], plainEnv());
    assert.equal(fired(), true, "baseline: planted core.fsmonitor must fire on status without the pin");

    // Through gitEnv: neutralized on status.
    resetMarker();
    runGit(wt.path, ["status", "--porcelain"], gitEnv());
    assert.equal(fired(), false, "gitEnv must neutralize core.fsmonitor on status");

    // And on the checkout that `worktree add` performs — exercised via GitCache,
    // which routes every git through gitEnv. The plant is live in <bare>/config.
    resetMarker();
    const wt2 = await git.worktreeForBranch(bare, "ci-fix/m0-poc", "m0-poc");
    assert.equal(fired(), false, "gitEnv must neutralize core.fsmonitor during worktree add (checkout)");
    assert.ok(fs.existsSync(path.join(wt2.path, "README.md")), "worktree checkout still succeeds");

    unplant(bare, "core.fsmonitor");
  });

  it("changedFiles (the real worker diff path) neither code-execs nor breaks with a planted diff.external", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 101);
    fs.writeFileSync(path.join(wt.path, "NEWFILE.txt"), "hello\n");
    runGit(wt.path, ["add", "NEWFILE.txt"], plainEnv());
    runGit(wt.path, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "add file"], plainEnv());

    plant(bare, "diff.external", evil);
    resetMarker();
    const changed = await git.changedFiles(bare, wt.path);
    assert.equal(fired(), false, "changedFiles must not code-exec a planted diff.external");
    assert.deepEqual(changed, ["NEWFILE.txt"], "changedFiles still computes the diff correctly");
  });
});
