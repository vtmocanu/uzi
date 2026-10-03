import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { PassThrough } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, isWorkflowScopeRejection, type BoundaryProcessSpawner } from "../src/git.js";

// PRD #456 M1 — the finalize base-align git helpers, exercised over REAL on-disk repos so a
// genuine merge/rebase conflict is produced (not a stub). The fixture origin ships a real
// `.github/workflows/ci.yml`, and "main advancing on workflows after the clone base" is
// modelled by committing to the fixture origin's main AFTER `ensureClone`.

let fx: Fixture;
let git: GitCache;

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Advance the fixture origin's `main` by writing `files` and committing. Origin is a
 *  non-bare repo checked out on main, so this moves the branch the align target reads. */
function advanceOriginMain(originPath: string, files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(originPath, ["add", "."]);
  gitIn(originPath, [...IDENT, "commit", "-m", msg]);
}

/** Delete `paths` on the fixture origin's `main` and commit — mirrors advanceOriginMain for
 *  the workflow-subtree delete cases (main dropped a workflow file, or the whole directory). */
function deleteOnOriginMain(originPath: string, paths: string[], msg: string): void {
  gitIn(originPath, ["rm", "-r", ...paths]);
  gitIn(originPath, [...IDENT, "commit", "-m", msg]);
}

beforeEach(() => {
  // Origin carries a real workflow file plus a normal file used to force conflicts.
  fx = makeFixture({
    ".github/workflows/ci.yml": "name: ci\non: [push]\njobs: {}\n",
    "conflict.txt": "base\n",
  });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
});

afterEach(() => fx.cleanup());

describe("isWorkflowScopeRejection", () => {
  it("is true for GitHub's workflow-scope push rejection (both phrasings)", () => {
    assert.strictEqual(
      isWorkflowScopeRejection(
        new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-1 -> agent/issue-1 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/brew.yml` without workflow scope)",
        ),
      ),
      true,
    );
    assert.strictEqual(
      isWorkflowScopeRejection("push rejected: WITHOUT WORKFLOW SCOPE"),
      true,
      "match is case-insensitive",
    );
  });

  it("is false for an unrelated push failure", () => {
    assert.strictEqual(isWorkflowScopeRejection(new Error("[rejected] non-fast-forward")), false);
    assert.strictEqual(isWorkflowScopeRejection(new Error("Authentication failed")), false);
    assert.strictEqual(isWorkflowScopeRejection(undefined), false);
    assert.strictEqual(isWorkflowScopeRejection(null), false);
  });
});

describe("fetchDefaultTip", () => {
  it("fetches the CURRENT default tip from origin (not the claim-time tip) and returns its SHA", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const claimTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    // main advances AFTER the clone — exactly the behind-on-workflows condition.
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" }, "bump ci");
    const freshTip = gitIn(fx.originPath, ["rev-parse", "main"]);
    assert.notStrictEqual(freshTip, claimTip);

    const got = await git.fetchDefaultTip(bare, "main");
    assert.match(got, /^[0-9a-f]{40}$/);
    assert.strictEqual(got, freshTip, "returns the FRESH origin tip, not the stale claim-time one");
    // The remote-tracking ref was updated in the bare as a side effect.
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]), freshTip);
  });
});

describe("fetchWorkflowTargetTip", () => {
  it("distinguishes absent and present while preserving mirrors, tags and FETCH_HEAD", async () => {
    gitIn(fx.originPath, ["branch", "agent/published"]);
    const bare = await git.ensureClone(fx.originPath);
    const beforeRefs = gitIn(bare, ["for-each-ref", "refs/remotes", "refs/tags"]);
    const fetchHead = path.join(bare, "FETCH_HEAD");
    fs.writeFileSync(fetchHead, "fixture sentinel\n");
    assert.deepStrictEqual(await git.fetchWorkflowTargetTip(bare, "agent/missing"), { kind: "absent" });
    gitIn(fx.originPath, ["checkout", "agent/published"]);
    advanceOriginMain(fx.originPath, { "target.txt": "new remote content\n" }, "remote target advances");
    gitIn(fx.originPath, ["tag", "new-target-tag"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    const sha = gitIn(fx.originPath, ["rev-parse", "agent/published"]);
    assert.deepStrictEqual(await git.fetchWorkflowTargetTip(bare, "agent/published"), { kind: "present", sha });
    assert.strictEqual(gitIn(bare, ["for-each-ref", "refs/remotes", "refs/tags"]), beforeRefs);
    assert.strictEqual(fs.readFileSync(fetchHead, "utf8"), "fixture sentinel\n");
    assert.strictEqual(gitIn(bare, ["for-each-ref", "refs/uzi-workflow-target"]), "");
    assert.deepStrictEqual(await git.fetchWorkflowTargetTip(bare, "bad..branch"), { kind: "unavailable" });
    gitIn(bare, ["config", "remote.origin.uploadpack", "false"]);
    assert.deepStrictEqual(await git.fetchWorkflowTargetTip(bare, "agent/published"), { kind: "unavailable" });
  });

  for (const cancellation of ["hard", "soft"] as const) {
    it(`cleans a real fetched ref before lock release after ${cancellation}-signal cancellation`, async () => {
      gitIn(fx.originPath, ["branch", "agent/published"]);
      const bare = await git.ensureClone(fx.originPath);
      assert.strictEqual(gitIn(bare, ["config", "--get", "maintenance.auto"]), "false");
      assert.strictEqual(gitIn(bare, ["config", "--get", "gc.auto"]), "0");
      assert.ok(!fs.existsSync(path.join(bare, "reftable")), "worker bare uses files refs");
      const hard = new AbortController();
      const soft = new AbortController();
      const seen: string[][] = [];
      let tempRef = "";
      let fetched = false;
      let spawnsAfterAbort = 0;
      let releaseObserved = false;
      let cleanBeforeRelease = false;
      const artifacts = () => [
        path.join(bare, tempRef),
        path.join(bare, `${tempRef}.lock`),
        path.join(bare, "logs", tempRef),
        path.join(bare, "logs", `${tempRef}.lock`),
      ];
      const spawner: BoundaryProcessSpawner = async (request) => {
        if (hard.signal.aborted || soft.signal.aborted) {
          spawnsAfterAbort++;
          throw new Error("unexpected subprocess after abort");
        }
        seen.push([...request.argv]);
        const output = execFileSync(request.argv[0]!, request.argv.slice(1), {
          cwd: request.cwd, env: request.env, encoding: "utf8",
        });
        if (request.argv.includes("fetch")) {
          tempRef = request.argv.at(-1)!.split(":")[1]!;
          assert.match(tempRef, /^refs\/uzi-workflow-target\/[0-9a-f-]{36}$/);
          assert.match(fs.readFileSync(path.join(bare, tempRef), "utf8"), /^[0-9a-f]{40}\n$/);
          const packed = path.join(bare, "packed-refs");
          assert.ok(!fs.existsSync(packed) || !fs.readFileSync(packed, "utf8").includes(tempRef));
          // Model only this fresh UUID's residual files after the fetch child has settled.
          for (const artifact of artifacts().slice(1)) {
            fs.mkdirSync(path.dirname(artifact), { recursive: true });
            fs.writeFileSync(artifact, "fixture residue\n");
          }
          fetched = true;
          (cancellation === "hard" ? hard : soft).abort();
        }
        const stdin = new PassThrough();
        const stdout = new PassThrough();
        const stderr = new PassThrough();
        stdout.end(output);
        stderr.end();
        return { stdin, stdout, stderr, completed: Promise.resolve({ code: 0 }) };
      };
      await assert.rejects(git.withBoundaryProcessSpawner(spawner, hard.signal,
        () => git.fetchWorkflowTargetTip(bare, "agent/published"), {
          softSignal: soft.signal,
          beforeLockRelease: async (key) => {
            releaseObserved = key === bare;
            cleanBeforeRelease = fetched && artifacts().every((artifact) => !fs.existsSync(artifact));
          },
        }), (error: unknown) => error instanceof Error && error.name === "AbortError"
          && (cancellation === "hard"
            ? error.message.includes("boundary deadline exceeded")
            : error.message.includes("soft deadline exceeded")));
      assert.strictEqual(spawnsAfterAbort, 0, "no subprocess after abort");
      assert.ok(fetched, "actual local fetch created a loose UUID ref before cancellation");
      assert.ok(releaseObserved, "bare-lock release hook ran");
      assert.ok(cleanBeforeRelease, "UUID ref, own locks and reflog removed before bare lock release");
      assert.strictEqual(seen.at(-1)?.includes("fetch"), true, "fetch was the last subprocess");
      assert.ok(artifacts().every((artifact) => !fs.existsSync(artifact)));
    });
  }

  for (const mode of ["malformed", "fetch-failure", "resolve-failure", "abort"] as const) {
    it(`cleans the dedicated ref and distinguishes ${mode}`, async () => {
      const bare = await git.ensureClone(fx.originPath);
      const seen: string[][] = [];
      const sha = "a".repeat(40);
      const abort = new Error("fixture cancellation");
      abort.name = "AbortError";
      const spawner: BoundaryProcessSpawner = async (request) => {
        const args = request.argv;
        seen.push([...args]);
        let code = 0;
        let output = "";
        if (args.includes("ls-remote")) output = mode === "malformed"
          ? `${sha}\trefs/heads/wrong\n` : `${sha}\trefs/heads/agent/published\n`;
        if (args.includes("fetch")) {
          if (mode === "abort") throw abort;
          if (mode === "fetch-failure") code = 1;
        }
        if (args.includes("rev-parse")) output = mode === "resolve-failure" ? "not a SHA\n" : `${sha}\n`;
        const stdin = new PassThrough();
        const stdout = new PassThrough();
        const stderr = new PassThrough();
        stdout.end(output);
        stderr.end();
        return { stdin, stdout, stderr, completed: Promise.resolve({ code }) };
      };
      const read = () => git.withBoundaryProcessSpawner(spawner, new AbortController().signal,
        () => git.fetchWorkflowTargetTip(bare, "agent/published"));
      if (mode === "abort") await assert.rejects(read(), (error: unknown) => error instanceof Error && error.name === "AbortError");
      else assert.deepStrictEqual(await read(), { kind: "unavailable" });
      assert.ok(seen.some((args) => args.includes("update-ref") && args.includes("-d")), "cleanup attempted after failure");
      for (const args of seen.filter((args) => args.includes("fetch"))) {
        assert.ok(args.includes("--refmap=") && args.includes("--no-tags") && args.includes("--no-write-fetch-head"));
      }
      assert.strictEqual(gitIn(bare, ["for-each-ref", "refs/uzi-workflow-target"]), "");
    });
  }
});

describe("branchWorkflowFiles", () => {
  it("duplicate maintainer and default workflow commits permit the published merged tip", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1870, noProofReseed);
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: duplicate\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "maintainer workflow"]);
    const maintainer = gitIn(rc.path, ["rev-parse", "HEAD"]);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: duplicate\n" }, "independent default workflow");
    const fresh = await git.fetchDefaultTip(bare, "main");
    assert.notStrictEqual(maintainer, fresh);
    gitIn(rc.path, ["fetch", fx.originPath, "main"]);
    gitIn(rc.path, [...IDENT, "merge", "--no-ff", "-m", "merge default", fresh]);
    const published = gitIn(rc.path, ["rev-parse", "HEAD"]);
    gitIn(rc.path, ["push", fx.originPath, "HEAD:refs/heads/agent/issue-1870"]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1870", "duplicate");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref, published), [], "published duplicate blob is permitted");
  });

  it("permits a per-path mixture, deletions, and flags renamed unknown paths", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1871, noProofReseed);
    const odd = ".github/workflows/café\nrelease.yml";
    fs.writeFileSync(path.join(rc.path, odd), "name: target\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "target workflow"]);
    const target = gitIn(rc.path, ["rev-parse", "HEAD"]);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: default\n" }, "default changes");
    const fresh = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: default\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "mix blobs"]);
    let ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1871", "mixed");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref, target), []);
    gitIn(rc.path, ["rm", ".github/workflows/ci.yml"]);
    gitIn(rc.path, ["mv", odd, ".github/workflows/renamed.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "rename and delete"]);
    ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1871", "renamed");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref, target), [".github/workflows/renamed.yml"]);
    assert.strictEqual(await git.branchWorkflowFiles(bare, "f".repeat(40), ref), null);
  });

  it("preserves unicode and newline workflow paths in both diff consumers", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const base = await git.fetchDefaultTip(bare, "main");
    const rc = await git.createOrAttachRunnerClone(bare, 104, noProofReseed);
    const paths = [".github/workflows/café.yml", ".github/workflows/line\nfeed.yml"];
    for (const rel of paths) {
      fs.writeFileSync(path.join(rc.path, rel), "name: branch\n");
    }
    gitIn(rc.path, ["add", "--", ...paths]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "workflow paths"]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-104", "run-paths");
    assert.deepStrictEqual(await git.changedFiles(bare, ref), [...paths].sort());
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, base, ref), [...paths].sort());
  });

  it("reads unicode workflow paths from a root commit", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const base = await git.fetchDefaultTip(bare, "main");
    const rc = await git.createOrAttachRunnerClone(bare, 105, noProofReseed);
    gitIn(rc.path, ["checkout", "--orphan", "root-path"]);
    gitIn(rc.path, ["rm", "-r", "--cached", "."]);
    const workflow = ".github/workflows/café.yml";
    fs.writeFileSync(path.join(rc.path, workflow), "name: root\n");
    gitIn(rc.path, ["add", "--", workflow]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "root workflow"]);
    const root = gitIn(rc.path, ["rev-parse", "HEAD"]);
    gitIn(bare, ["fetch", rc.path, root]);
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, base, root), [workflow]);
  });

  it("classifies tips beyond 256 commits", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const base = await git.fetchDefaultTip(bare, "main");
    const tree = gitIn(bare, ["rev-parse", `${base}^{tree}`]);
    let tip = base;
    for (let n = 0; n < 257; n++) {
      tip = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", tip, "-m", `empty ${n}`]);
    }
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, base, tip), []);
  });

  it("classifies tips with more than 32 parents", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const base = await git.fetchDefaultTip(bare, "main");
    const tree = gitIn(bare, ["rev-parse", `${base}^{tree}`]);
    const parents = Array.from({ length: 33 }, (_, n) =>
      gitIn(bare, [...IDENT, "commit-tree", tree, "-p", base, "-m", `parent ${n}`]));
    const merge = gitIn(bare, [...IDENT, "commit-tree", tree, ...parents.flatMap((p) => ["-p", p]), "-m", "wide merge"]);
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, base, merge), []);
  });

  it("ignores a stale default mirror older than the branch base", async () => {
    const bare = await git.ensureClone(fx.originPath);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: base\n" }, "branch base");
    const base = await git.fetchDefaultTip(bare, "main");
    const rc = await git.createOrAttachRunnerClone(bare, 101, noProofReseed);
    fs.writeFileSync(path.join(rc.path, "impl.ts"), "agent work\n");
    gitIn(rc.path, ["add", "impl.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "agent work"]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-101", "run-stale");
    const old = gitIn(bare, ["rev-parse", `${base}^`]);
    gitIn(bare, ["update-ref", "refs/remotes/origin/main", old]);
    assert.deepStrictEqual(await git.changedFiles(bare, ref), [".github/workflows/ci.yml", "impl.ts"]);
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, base, ref), []);
  });

  it("flags an align subject that restores an older workflow blob from a newer base", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const oldBlob = gitIn(fx.originPath, ["show", "HEAD:.github/workflows/ci.yml"]);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: branch base\n" }, "branch base");
    await git.fetchDefaultTip(bare, "main");
    const rc = await git.createOrAttachRunnerClone(bare, 102, noProofReseed);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: new default\n" }, "new default");
    const fresh = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), `${oldBlob}\n`);
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-102", "run-restore");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), [".github/workflows/ci.yml"]);
  });

  it("exempts a verified align when default removed the entire workflow directory", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 103, noProofReseed);
    deleteOnOriginMain(fx.originPath, [".github/workflows/ci.yml"], "default removes workflows");
    const fresh = await git.fetchDefaultTip(bare, "main");
    gitIn(rc.path, ["rm", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-103", "run-remove");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), []);
  });

  it("flags an unpublished older align, permits its remote blob, and flags a later edit", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: older\n" }, "older default");
    const older = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: older\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${older}`]);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: newest\n" }, "new default");
    const fresh = await git.fetchDefaultTip(bare, "main");
    let ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), [".github/workflows/ci.yml"]);
    const published = gitIn(rc.path, ["rev-parse", "HEAD"]);
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref, published), []);
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: agent edit\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "agent edits workflow"]);
    ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-b");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref, published), [".github/workflows/ci.yml"]);
  });

  it("exempts a current single-parent workflow-only align", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 4, noProofReseed);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: current\n" }, "default advances");
    const fresh = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: current\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-4", "run-current");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), []);
  });

  it("ignores a missing SHA in a commit subject", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 106, noProofReseed);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: current\n" }, "default advances");
    const fresh = await git.fetchDefaultTip(bare, "main");
    const missing = "f".repeat(40);
    assert.throws(
      () => gitIn(bare, ["merge-base", "--is-ancestor", missing, fresh]),
      (error: unknown) => (error as { status?: number }).status === 128,
    );
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: current\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${missing}`]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-106", "run-missing");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), []);
  });

  it("permits mixed edits matching default but flags later edits", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 3, noProofReseed);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: aligned\n" }, "default advances");
    const fresh = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: aligned\n");
    fs.writeFileSync(path.join(rc.path, "impl.ts"), "agent work\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    let ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-3", "run-mixed");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), []);

    // A later workflow edit still differs from the allowed default blob, regardless of subject.
    gitIn(rc.path, ["fetch", fx.originPath, "main"]);
    gitIn(rc.path, ["reset", "--hard", fresh]);
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: later edit\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-3", "run-parent");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), [".github/workflows/ci.yml"]);
  });

  it("ignores a clean merge carrying default workflows and an empty align commit", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 5, noProofReseed);
    const initial = await git.fetchDefaultTip(bare, "main");
    gitIn(rc.path, [...IDENT, "commit", "--allow-empty", "-m", `chore: align .github/workflows with ${initial}`]);
    let ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-5", "run-empty");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, initial, ref), []);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: merged\n" }, "default workflow");
    const fresh = await git.fetchDefaultTip(bare, "main");
    gitIn(rc.path, ["fetch", fx.originPath, "main"]);
    gitIn(rc.path, [...IDENT, "merge", "--no-ff", "-m", `chore: align .github/workflows with ${fresh}`, fresh]);
    ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-5", "run-merge");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), []);
  });

  it("detects a merge resolution that changes workflows against every parent", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 6, noProofReseed);
    const base = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: branch\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "branch workflow"]);
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: default\n" }, "default workflow");
    const fresh = await git.fetchDefaultTip(bare, "main");
    gitIn(rc.path, ["fetch", fx.originPath, "main"]);
    const branch = gitIn(rc.path, ["rev-parse", "HEAD"]);
    const tree = gitIn(rc.path, ["write-tree"]);
    // Build a merge tree with a third resolution, distinct from both parent blobs.
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: resolved\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    const resolvedTree = gitIn(rc.path, ["write-tree"]);
    assert.notStrictEqual(tree, resolvedTree);
    const merge = gitIn(rc.path, [...IDENT, "commit-tree", resolvedTree, "-p", branch, "-p", fresh, "-m", "resolve workflow"]);
    gitIn(bare, ["fetch", rc.path, merge]);
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, merge), [".github/workflows/ci.yml"]);
    assert.notStrictEqual(base, fresh);
  });

  it("rejects a forged align subject with a workflow tree that differs from its named default", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 2, noProofReseed);
    const fresh = await git.fetchDefaultTip(bare, "main");
    fs.writeFileSync(path.join(rc.path, ".github/workflows/ci.yml"), "name: forged\n");
    gitIn(rc.path, ["add", ".github/workflows/ci.yml"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${fresh}`]);
    const ref = await git.fetchAgentBranch(bare, rc.path, "agent/issue-2", "run-c");
    assert.deepStrictEqual(await git.branchWorkflowFiles(bare, fresh, ref), [".github/workflows/ci.yml"]);
  });
});

describe("workflowTreeDiffers", () => {
  it("is true when the branch's workflow tree differs from the fresh default, false when equal", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed);
    // Agent commits a NON-workflow change (the branch never touches workflows).
    fs.writeFileSync(path.join(rc.path, "impl.ts"), "export const x = 1;\n");
    gitIn(rc.path, ["add", "impl.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "impl"]);
    const trackingRef = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");

    // Before main moves, the workflow trees match → no divergence.
    const tipBefore = await git.fetchDefaultTip(bare, "main");
    assert.strictEqual(await git.workflowTreeDiffers(bare, trackingRef, tipBefore), false);

    // main advances a workflow file → divergence.
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" }, "bump ci");
    const tipAfter = await git.fetchDefaultTip(bare, "main");
    assert.strictEqual(await git.workflowTreeDiffers(bare, trackingRef, tipAfter), true);
  });
});

describe("alignBranchWithDefault", () => {
  /** Seed a clone, commit `branchFile` as the agent's non-workflow work, and return the
   *  clone + its committed tip + the fresh default tip after main advances with `mainFiles`. */
  async function setup(
    branchFiles: Record<string, string>,
    mainFiles: Record<string, string>,
  ): Promise<{ bare: string; clonePath: string; baseTip: string; defaultTip: string }> {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed);
    for (const [rel, content] of Object.entries(branchFiles)) {
      fs.writeFileSync(path.join(rc.path, rel), content);
    }
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "agent work"]);
    const baseTip = gitIn(rc.path, ["rev-parse", "HEAD"]);
    advanceOriginMain(fx.originPath, mainFiles, "main advances");
    const defaultTip = await git.fetchDefaultTip(bare, "main");
    return { bare, clonePath: rc.path, baseTip, defaultTip };
  }

  it("merge: aligns a behind-on-workflows branch, keeping the agent's work AND landing the fresh workflow", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge");
    assert.strictEqual(res, "aligned");
    // The clone's branch now carries BOTH the agent's file and the fresh workflow content.
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:.github/workflows/ci.yml"]), "name: ci\non: [pull_request]");
    // A merge is SHA-preserving: the agent's commit is still an ancestor of the tip.
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", baseTip, "HEAD"]) , "");
    // The temporary align-target ref was cleaned up (rev-parse --verify exits non-zero).
    assert.throws(() =>
      execFileSync("git", ["-C", clonePath, "rev-parse", "--verify", "refs/uzi-align/target"], {
        env: ENV,
        stdio: "pipe",
      }),
    );
  });

  it("merge: returns \"conflict\" and aborts when the branch and default edit the same file divergently", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge");
    assert.strictEqual(res, "conflict");
    // Aborted cleanly: no merge in progress, branch back at the agent tip, no conflict markers.
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "MERGE_HEAD")), false);
    assert.strictEqual(gitIn(clonePath, ["rev-parse", "HEAD"]), baseTip);
  });

  it("rebase: aligns cleanly and preserves the agent's commit count (S3)", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase");
    assert.strictEqual(res, "aligned");
    // Rebased onto the fresh default: the default tip is now an ancestor, workflow content fresh.
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", defaultTip, "HEAD"]), "");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:.github/workflows/ci.yml"]), "name: ci\non: [pull_request]");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });

  it("rebase: returns \"conflict\" and aborts on a divergent same-file edit", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase");
    assert.strictEqual(res, "conflict");
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "rebase-merge")), false);
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "rebase-apply")), false);
    assert.strictEqual(gitIn(clonePath, ["rev-parse", "HEAD"]), baseTip, "branch restored to the agent tip");
  });

  it("merge-then-rebase: a rebase fallback after a clean merge replays the ORIGINAL agent commits", async () => {
    // Model the M1 fallback sequence at the git layer: merge cleanly, THEN rebase from the
    // same baseTip. The rebase must start from the original agent work, not the merge commit,
    // so the result is a linear history whose base is the fresh default.
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    assert.strictEqual(
      await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge"),
      "aligned",
    );
    // refs/heads/agent/issue-1 is now the merge commit; the fallback passes the same baseTip.
    assert.strictEqual(
      await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase"),
      "aligned",
    );
    // Linear on top of the fresh default (no merge commit): HEAD has exactly one parent, and
    // the default tip is its ancestor.
    assert.strictEqual(gitIn(clonePath, ["rev-list", "--count", "--merges", `${defaultTip}..HEAD`]), "0");
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", defaultTip, "HEAD"]), "");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });
});

// Issue #627 — the NARROW primary strategy: overlay ONLY the default tip's .github/workflows/
// subtree onto the agent tip (byte-for-byte equal, deletions included), fast-forward, no
// conflict. Exercised over REAL on-disk repos.
describe("alignBranchWithDefault — workflow-subtree", () => {
  /** Clone, create the runner clone on agent/issue-1, commit `branchFiles` as the agent's
   *  work, and return the clone + its committed tip + the bare (to fetch the default from). */
  async function seedClone(
    branchFiles: Record<string, string>,
  ): Promise<{ bare: string; clonePath: string; baseTip: string }> {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed);
    for (const [rel, content] of Object.entries(branchFiles)) {
      const target = path.join(rc.path, rel);
      fs.mkdirSync(path.dirname(target), { recursive: true });
      fs.writeFileSync(target, content);
    }
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "agent work"]);
    const baseTip = gitIn(rc.path, ["rev-parse", "HEAD"]);
    return { bare, clonePath: rc.path, baseTip };
  }

  it("add/modify + non-workflow divergence: overlays ONLY the workflow subtree, keeps agent work, fast-forwards", async () => {
    // The branch commits an agent non-workflow file AND diverges conflict.txt; main modifies
    // the workflow file AND diverges conflict.txt too (a whole-tree merge WOULD conflict).
    const { clonePath, baseTip, bare } = await seedClone({
      "impl.ts": "export const x = 1;\n",
      "conflict.txt": "branch side\n",
    });
    const agentCommit = baseTip;
    advanceOriginMain(
      fx.originPath,
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n", "conflict.txt": "main side\n" },
      "main advances",
    );
    const defaultTip = await git.fetchDefaultTip(bare, "main");

    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "workflow-subtree");
    // (a) aligned
    assert.strictEqual(res, "aligned");
    // (b) the workflow file now equals the DEFAULT's content
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:.github/workflows/ci.yml"]), "name: ci\non: [pull_request]");
    // (c) the non-workflow file is still the AGENT's content, NOT main's — only the subtree moved
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:conflict.txt"]), "branch side");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
    // (d) fast-forward: exactly ONE overlay commit, and the original agent commit is an ancestor
    assert.strictEqual(gitIn(clonePath, ["rev-list", "--count", `${baseTip}..HEAD`]), "1");
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", agentCommit, "HEAD"]), "");
    // The temporary align-target ref was cleaned up.
    assert.throws(() =>
      execFileSync("git", ["-C", clonePath, "rev-parse", "--verify", "refs/uzi-align/target"], {
        env: ENV,
        stdio: "pipe",
      }),
    );
  });

  it("delete-one-file: makes the branch's workflow set exactly the default's, dropping the file main deleted", async () => {
    // Seed a SECOND workflow file on origin BEFORE the clone so the branch base carries both.
    advanceOriginMain(fx.originPath, { ".github/workflows/release.yml": "name: release\non: [release]\n" }, "add release wf");
    const { clonePath, baseTip, bare } = await seedClone({ "impl.ts": "export const x = 1;\n" });
    // main deletes ONE of the workflow files.
    deleteOnOriginMain(fx.originPath, [".github/workflows/release.yml"], "drop release wf");
    const defaultTip = await git.fetchDefaultTip(bare, "main");

    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "workflow-subtree");
    assert.strictEqual(res, "aligned");
    // release.yml is gone; ci.yml is present.
    assert.strictEqual(gitIn(clonePath, ["ls-tree", "HEAD", "--", ".github/workflows/release.yml"]), "");
    assert.notStrictEqual(gitIn(clonePath, ["ls-tree", "HEAD", "--", ".github/workflows/ci.yml"]), "");
    // The branch's workflow tree equals the default's exactly (byte-for-byte, same tree SHA).
    assert.strictEqual(
      gitIn(clonePath, ["rev-parse", "HEAD:.github/workflows"]),
      gitIn(clonePath, ["rev-parse", `${defaultTip}:.github/workflows`]),
    );
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });

  it("delete-entire-dir (empty-default edge): removes the branch's workflow tree entirely, returns aligned, no throw", async () => {
    const { clonePath, baseTip, bare } = await seedClone({ "impl.ts": "export const x = 1;\n" });
    // main deletes its ENTIRE .github/workflows/ directory — the case a bare
    // `git checkout <defaultTip> -- .github/workflows` would ERROR on.
    deleteOnOriginMain(fx.originPath, [".github/workflows"], "drop all workflows");
    const defaultTip = await git.fetchDefaultTip(bare, "main");

    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "workflow-subtree");
    assert.strictEqual(res, "aligned");
    // No .github/workflows tree at all on the branch now.
    assert.strictEqual(gitIn(clonePath, ["ls-tree", "HEAD", "--", ".github/workflows"]), "");
    // The agent's work is untouched.
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });
});
