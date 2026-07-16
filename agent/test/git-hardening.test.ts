import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, gitEnv } from "../src/git.js";

// PRD #51 M0 — standalone shared-git hardening (the gitEnv belt), PLUS the M3 (b)
// config-source isolation close. The M0 pins neutralize a FIXED-name code-exec key
// (core.fsmonitor / diff.external / core.pager / core.sshCommand, + the M0-audit
// auth/ref keys) planted in a config source a worker-side git reads, via an inline
// GIT_CONFIG override + GIT_CONFIG_NOSYSTEM + a /dev/null GIT_CONFIG_GLOBAL default.
//
// Under M3 (b) separate-runner-clone the worker is BARE-ONLY: it never runs
// `worktree add`/checkout, so the only worker-side git that reads an on-disk config
// is over its OWN worker-owned bare (changedFiles' `--name-only` bare tree-diff, the
// fetch-back, credential fill). The runner clone is a SEPARATE clone with its own
// config, so a `<bare>/config` plant is NOT even read by the runner's git — the
// structural close (config-source ownership) proven by the isolation test below,
// independent of the gitEnv belt. The functional PoCs therefore exercise worker-BARE
// ops (where a `<bare>/config` plant is read) rather than the retired worktree checkout.

// Collect the inline GIT_CONFIG_* pairs into a key→value map.
function configPairs(env: NodeJS.ProcessEnv): Record<string, string> {
  const out: Record<string, string> = {};
  const n = Number(env.GIT_CONFIG_COUNT ?? "0");
  for (let i = 0; i < n; i++) out[env[`GIT_CONFIG_KEY_${i}`]!] = env[`GIT_CONFIG_VALUE_${i}`]!;
  return out;
}

// The FIXED-name code-exec keys M0 pins, with their verified inert override value.
// Command-valued keys take a no-op command; the auth/ref keys added by the M0
// audit (credential.helper / core.askpass / core.alternateRefsCommand) take an
// empty value (reset/disable).
const EXPECTED_PINS: Record<string, string> = {
  "core.fsmonitor": "false",
  "diff.external": "true",
  "core.pager": "cat",
  "core.sshCommand": "ssh",
  "credential.helper": "",
  "core.askpass": "",
  "core.alternateRefsCommand": "",
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
  function runGit(cwd: string, args: string[], env: NodeJS.ProcessEnv): void {
    try {
      execFileSync("git", ["-C", cwd, ...args], { env, stdio: "pipe" });
    } catch {
      // A non-zero exit is fine; we only care whether the marker fired.
    }
  }
  function gitOut(cwd: string, args: string[]): string {
    return execFileSync("git", ["-C", cwd, ...args], { env: plainEnv(), encoding: "utf8" }).trim();
  }
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

  // Seed a runner clone, add a commit with `file`, and fetch the agent branch back
  // into the worker bare — the (b) round-trip that leaves a bare-reachable tracking
  // ref + two commits (base, tip) for the worker-bare diff PoCs below.
  async function commitInCloneAndFetch(
    bare: string,
    iid: number,
    file: string,
    content: string,
  ): Promise<{ ref: string; baseSha: string; tipSha: string }> {
    const rc = await git.createOrAttachRunnerClone(bare, iid);
    const baseSha = gitOut(rc.path, ["rev-parse", "HEAD"]);
    fs.writeFileSync(path.join(rc.path, file), content);
    runGit(rc.path, ["add", file], plainEnv());
    runGit(rc.path, [...IDENT, "commit", "-m", "work"], plainEnv());
    const tipSha = gitOut(rc.path, ["rev-parse", "HEAD"]);
    const ref = await git.fetchAgentBranch(bare, rc.path, `agent/issue-${iid}`);
    return { ref, baseSha, tipSha };
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

  it("diff.external planted in <bare>/config does NOT code-exec in a worker-BARE content diff (baseline proves it would)", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    // Two commits reachable in the bare (via the (b) round-trip), so a content diff
    // in the bare has something for the external driver to run on.
    const { baseSha, tipSha } = await commitInCloneAndFetch(bare, 99, "CH.txt", "changed\n");

    plant(bare, "diff.external", evil);

    // Baseline: a pin-less env runs the planted command on a worker-bare content diff.
    resetMarker();
    runGit(bare, ["diff", baseSha, tipSha], plainEnv());
    assert.equal(fired(), true, "baseline: planted diff.external must fire on a bare content diff (test would be vacuous otherwise)");

    // gitEnv's inline diff.external=true overrides the plant → no code-exec.
    resetMarker();
    runGit(bare, ["diff", baseSha, tipSha], gitEnv());
    assert.equal(fired(), false, "gitEnv must neutralize the planted diff.external on the worker bare");
  });

  it("(b) config-source isolation: a <bare>/config plant is NOT read by the runner clone's git (structural close, no pin needed)", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    // Plant BEFORE seeding, so if the runner clone ever consulted <bare>/config it
    // would inherit the code-exec key.
    plant(bare, "diff.external", evil);
    const rc = await git.createOrAttachRunnerClone(bare, 100);
    // Two commits in the CLONE so a content diff there is possible.
    fs.writeFileSync(path.join(rc.path, "X.txt"), "1\n");
    runGit(rc.path, ["add", "X.txt"], plainEnv());
    runGit(rc.path, [...IDENT, "commit", "-m", "c1"], plainEnv());
    fs.writeFileSync(path.join(rc.path, "X.txt"), "2\n");
    runGit(rc.path, [...IDENT, "commit", "-am", "c2"], plainEnv());

    // Even with a PIN-LESS env, the clone's own config has no diff.external and it
    // does NOT read <bare>/config — so the plant cannot fire in the runner's git.
    // This is the (b) close (config-source ownership), independent of the gitEnv belt.
    resetMarker();
    runGit(rc.path, ["diff", "HEAD~1", "HEAD"], plainEnv());
    assert.equal(fired(), false, "a <bare>/config plant must NOT reach the runner clone's git (separate config source)");
    // Control: the clone's diff really did run (it has the two differing commits).
    assert.match(gitOut(rc.path, ["diff", "--name-only", "HEAD~1", "HEAD"]), /X\.txt/, "the runner clone's diff still works");
  });

  it("changedFiles (worker-BARE --name-only tree-diff) neither code-execs nor breaks with a planted diff.external", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    const { ref } = await commitInCloneAndFetch(bare, 101, "NEWFILE.txt", "hello\n");

    plant(bare, "diff.external", evil);
    resetMarker();
    const changed = await git.changedFiles(bare, ref);
    assert.equal(fired(), false, "changedFiles must not code-exec a planted diff.external");
    assert.deepEqual(changed, ["NEWFILE.txt"], "changedFiles still computes the diff correctly");
  });

  it("(b) invariant 6: a runner-planted uploadpack.packObjectsHook does NOT execute in the worker's file:// fetch-back", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 200);
    fs.writeFileSync(path.join(rc.path, "F.txt"), "x\n");
    runGit(rc.path, ["add", "F.txt"], plainEnv());
    runGit(rc.path, [...IDENT, "commit", "-m", "c"], plainEnv());

    // Plant the hook in the RUNNER clone's OWN config (the surface a compromised runner
    // controls under the split). `uploadpack.packObjectsHook` is honored ONLY from
    // PROTECTED config (git-config(1): "only respected when it is specified in protected
    // configuration"), so a runner REPO-LOCAL plant is ignored regardless of transport —
    // a documented, transport-independent, STABLE gate, NOT version-dependent. (The
    // file://+pack transport's own job is the SEPARATE CVE-2022-39253 alternates vector,
    // invariant 3.) Confirmed on the IMAGE's git (node:22-alpine, git 2.54.0) + host git
    // 2.55.0; kept as a regression guard (B2 invariant 6).
    plant(rc.path, "uploadpack.packObjectsHook", evil);
    resetMarker();
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-200");
    assert.equal(fired(), false, "a runner-planted uploadpack.packObjectsHook must NOT execute in the worker's fetch-back");
    // The mitigation must not break the fetch — the agent branch still lands in the bare.
    assert.match(gitOut(bare, ["rev-parse", ref]), /^[0-9a-f]{40}$/, "the fetch-back still landed the agent branch");
  });

  // `git credential fill` reads a credential request on stdin then consults the
  // credential helpers / askpass to fill missing fields — the reach point for a
  // planted credential.helper / core.askpass when a worker-side credentialed op
  // hits an auth challenge (401/407). Bounded by a timeout so a neutralized run
  // that finds no credential can never hang the suite.
  function credentialFill(bare: string, request: string, env: NodeJS.ProcessEnv): void {
    try {
      execFileSync("git", ["-C", bare, "credential", "fill"], {
        input: request,
        env,
        stdio: ["pipe", "pipe", "pipe"],
        timeout: 15000,
      });
    } catch {
      // A non-zero exit (no credential obtained) or a timeout is fine — the only
      // thing under test is whether the planted marker fired.
    }
  }

  it("credential.helper planted in <bare>/config does NOT code-exec on `git credential fill` (M0 audit MEDIUM; baseline proves it would)", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    plant(bare, "credential.helper", evil);
    const req = "protocol=https\nhost=example.com\n\n";

    resetMarker();
    credentialFill(bare, req, plainEnv());
    assert.equal(fired(), true, "baseline: planted credential.helper must fire on credential fill without the pin");

    // gitEnv's inline credential.helper="" RESETS the accumulated helper list.
    resetMarker();
    credentialFill(bare, req, gitEnv());
    assert.equal(fired(), false, "gitEnv must neutralize the planted credential.helper");
  });

  it("core.askpass planted in <bare>/config does NOT code-exec when git needs a password (M0 audit MEDIUM; baseline proves it would)", async (t) => {
    if (!gitAvailable()) return t.skip("git not available");
    const bare = await git.ensureClone(fx.originPath);
    plant(bare, "core.askpass", evil);
    // username supplied, password missing → git consults askpass for the password.
    const req = "protocol=https\nhost=example.com\nusername=x\n\n";

    resetMarker();
    credentialFill(bare, req, plainEnv());
    assert.equal(fired(), true, "baseline: planted core.askpass must fire when a password is needed");

    // gitEnv's inline core.askpass="" makes git skip the planted askpass.
    resetMarker();
    credentialFill(bare, req, gitEnv());
    assert.equal(fired(), false, "gitEnv must neutralize the planted core.askpass");
  });
});
