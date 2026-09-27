import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, gitEnv, runnerGitCarriesWorkerMark } from "../src/git.js";

// issue #1783 (auditor, round 3) — a runner-clone git must never run a program the clone's
// agent-writable `.git/config` plants through partial-clone LAZY FETCH. With
// `extensions.partialClone=<remote>` and that remote's `uploadpack=<program>`, any git that needs
// a missing object asks the "promisor" remote for it, and so spawns the planted program. The
// worker-MARKED subcommands (rev-parse, rev-list, ls-tree, …: runnerGitCarriesWorkerMark) are the
// sharpest case, because the program would inherit the worker nonce and be exempt from every reap;
// but no worker git needs lazy fetch at all (every clone is a `clone --shared` of a full bare), so
// the pin (GIT_NO_LAZY_FETCH=1) rides EVERY worker git. The marked subset additionally carries
// GIT_ALLOW_PROTOCOL naming no protocol: it needs no transport, so none may start. The fixture
// also plants `[protocol "file"] allow = always`, which would re-enable the file transport past a
// `protocol.allow=never` config pin; the env var overrides it. Each marked case runs with every
// pin present, with each pin stripped in turn (the other alone must still block), and with both
// stripped (the control: the plant must fire), so each pin is proven independently.
//
// The plant is written with fs (not `git config`), and the "program" only writes a marker file.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
/** A well-formed object id that is not in the repository. */
const MISSING = "0123456789abcdef0123456789abcdef01234567";

let root: string;
let clone: string;
let marker: string;
let git: GitCache;

type RunGitAsRunner = (cwd: string | undefined, args: string[]) => Promise<string>;
const runAsRunner = (args: string[]): Promise<string> =>
  (git as unknown as { runGitAsRunner: RunGitAsRunner }).runGitAsRunner.call(git, clone, args);

/** The two env pins a marked runner git carries against a lazy fetch. */
const PINS = ["GIT_NO_LAZY_FETCH", "GIT_ALLOW_PROTOCOL"] as const;
type ExecScoped = (command: string, args: string[], options: { env: NodeJS.ProcessEnv }, identity?: string) => Promise<unknown>;

/**
 * Run `args` through the real runGitAsRunner, deleting `strip` from the env it built just before
 * the spawn (the env is otherwise exactly production's). Returns the env the git ran with.
 */
async function runAsRunnerWithout(args: string[], strip: readonly string[]): Promise<NodeJS.ProcessEnv> {
  const self = git as unknown as { execScoped: ExecScoped };
  const orig = self.execScoped;
  let seen: NodeJS.ProcessEnv = {};
  self.execScoped = function (this: unknown, command, argv, options, identity) {
    const env = { ...options.env };
    for (const k of strip) delete env[k];
    seen = env;
    return orig.call(git, command, argv, { ...options, env }, identity);
  };
  try {
    await runAsRunner(args).catch(() => undefined);
  } finally {
    self.execScoped = orig;
  }
  return seen;
}

before(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-no-lazy-fetch-"));
  clone = path.join(root, "clone");
  marker = path.join(root, "planted-ran");
  fs.mkdirSync(clone);
  execFileSync("git", ["-C", clone, "init", "-q", "-b", "main"], { env: ENV, stdio: "pipe" });
  fs.writeFileSync(path.join(clone, "a.txt"), "a\n");
  execFileSync("git", ["-C", clone, "add", "a.txt"], { env: ENV, stdio: "pipe" });
  execFileSync("git", ["-C", clone, ...IDENT, "commit", "-q", "-m", "a"], { env: ENV, stdio: "pipe" });

  // The planted "upload-pack": record that it ran (and whether it carried the worker nonce), fail.
  const program = path.join(root, "planted-uploadpack.sh");
  fs.writeFileSync(program, `#!/bin/sh\necho "ran nonce=\${UZI_WORKER_SPAWN:-}" >> '${marker}'\nexit 1\n`, { mode: 0o755 });
  // A ref at a MISSING object, so resolving it needs that object.
  fs.writeFileSync(path.join(clone, ".git", "refs", "heads", "ghost"), `${MISSING}\n`);
  // The partial-clone plant, written straight into the agent-writable config.
  const cfgPath = path.join(clone, ".git", "config");
  const cfg = fs.readFileSync(cfgPath, "utf8").replace(/repositoryformatversion = 0/, "repositoryformatversion = 1");
  fs.writeFileSync(
    cfgPath,
    `${cfg}[extensions]\n\tpartialClone = evil\n[remote "evil"]\n\turl = ${clone}\n\tpromisor = true\n\tuploadpack = ${program}\n` +
      // Re-enable the file transport past any `protocol.allow` config pin.
      `[protocol "file"]\n\tallow = always\n`,
  );
  git = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
});

after(() => fs.rmSync(root, { recursive: true, force: true }));

function plantedRuns(): string[] {
  return fs.existsSync(marker) ? fs.readFileSync(marker, "utf8").split("\n").filter(Boolean) : [];
}

describe("issue #1783: runner-clone git never lazy-fetches through a planted promisor remote", () => {
  it("control: the plant is live — a plain git without the pin runs the planted program", () => {
    // Proves the fixture: without GIT_NO_LAZY_FETCH the same resolution DOES reach the plant.
    fs.rmSync(marker, { force: true });
    try {
      execFileSync("git", ["-C", clone, "rev-parse", "--verify", `${MISSING}^{commit}`], { env: ENV, stdio: "pipe" });
    } catch {
      /* the object stays missing */
    }
    assert.ok(plantedRuns().length > 0, "the unpinned git lazy-fetched through the planted uploadpack");
    fs.rmSync(marker, { force: true });
  });

  it("gitEnv pins GIT_NO_LAZY_FETCH for every worker git", () => {
    assert.equal(gitEnv().GIT_NO_LAZY_FETCH, "1");
    assert.equal(gitEnv("pat-value", "https://example.test/").GIT_NO_LAZY_FETCH, "1");
  });

  for (const args of [
    ["rev-parse", "--verify", "refs/heads/ghost^{commit}"],
    ["rev-parse", "--verify", `${MISSING}^{commit}`],
    ["rev-list", "--count", "ghost"],
    ["ls-tree", "ghost", "--", "x"],
  ]) {
    it(`marked \`git ${args.join(" ")}\` does not execute the planted program`, async () => {
      assert.equal(runnerGitCarriesWorkerMark(args), true, "this argv runs worker-marked");
      fs.rmSync(marker, { force: true });
      await runAsRunner(args).catch(() => undefined);
      assert.deepEqual(plantedRuns(), [], "the planted uploadpack never ran");
    });

    for (const strip of PINS) {
      const kept = PINS.find((p) => p !== strip)!;
      it(`marked \`git ${args.join(" ")}\` with only ${kept} still does not execute the planted program`, async () => {
        fs.rmSync(marker, { force: true });
        const env = await runAsRunnerWithout(args, [strip]);
        assert.equal(env[strip], undefined, `${strip} was stripped`);
        assert.ok(env[kept] !== undefined, `${kept} is carried`);
        assert.deepEqual(plantedRuns(), [], `${kept} alone blocks the planted uploadpack`);
      });
    }

    it(`control: marked \`git ${args.join(" ")}\` with BOTH pins stripped executes the planted program`, async () => {
      fs.rmSync(marker, { force: true });
      await runAsRunnerWithout(args, PINS);
      assert.ok(plantedRuns().length > 0, "without the pins the same runner git reaches the plant");
      fs.rmSync(marker, { force: true });
    });
  }

  it("a marked runner git carries GIT_ALLOW_PROTOCOL; an unmarked one and the seed's clone do not", async () => {
    assert.equal((await runAsRunnerWithout(["rev-parse", "HEAD"], [])).GIT_ALLOW_PROTOCOL, "none");
    assert.equal((await runAsRunnerWithout(["log", "-1"], [])).GIT_ALLOW_PROTOCOL, undefined);
    assert.equal((await runAsRunnerWithout(["clone", "--no-checkout", "/nonexistent", "/nonexistent2"], [])).GIT_ALLOW_PROTOCOL, undefined);
  });

  it("the public marked readers (branchTip, worktreeHead) do not execute the planted program", async () => {
    fs.rmSync(marker, { force: true });
    assert.equal(await git.branchTip(clone, "ghost"), null);
    fs.writeFileSync(path.join(clone, ".git", "HEAD"), "ref: refs/heads/ghost\n");
    try {
      assert.equal(await git.worktreeHead(clone), null);
    } finally {
      fs.writeFileSync(path.join(clone, ".git", "HEAD"), "ref: refs/heads/main\n");
    }
    assert.deepEqual(plantedRuns(), [], "the planted uploadpack never ran");
  });

  it("an UNMARKED runner git (log) does not lazy-fetch either", async () => {
    assert.equal(runnerGitCarriesWorkerMark(["log", "-1", "ghost"]), false);
    fs.rmSync(marker, { force: true });
    await runAsRunner(["log", "-1", "--format=%s", "ghost"]).catch(() => undefined);
    assert.deepEqual(plantedRuns(), [], "the planted uploadpack never ran");
  });
});
