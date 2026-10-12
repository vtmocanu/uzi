import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { GitCache, ScratchPublicationError, type BoundaryProcessSpawner } from "../src/git.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

function capture(requests: BoundaryProcessRequest[]): BoundaryProcessSpawner {
  return async (request) => {
    requests.push(request);
    const child = spawn(request.argv[0]!, request.argv.slice(1), {
      cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"],
    });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code) => resolve({ code: code ?? -1 }));
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGKILL"); await completed; } };
  };
}
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}
const remoteCommands = (requests: BoundaryProcessRequest[]) =>
  requests.filter((r) => r.argv.some((a) => ["ls-remote", "fetch", "push"].includes(a)));

for (const branch of ["", "-upload-pack=hostile", "bad..branch", "bad:branch", "bad@{name"]) {
  it(`remote default guard refuses ${JSON.stringify(branch)} without remote calls`, async () => {
    const fx = makeFixture();
    try {
      const cache = new GitCache(fx.dataDir, nullLogger());
      const bare = await cache.ensureClone(fx.originPath);
      const requests: BoundaryProcessRequest[] = [];
      await cache.withBoundaryProcessSpawner(capture(requests), new AbortController().signal, async () => {
        await assert.rejects(cache.fetchDefaultTip(bare, branch), /invalid remote branch/);
      });
      assert.deepEqual(remoteCommands(requests), []);
    } finally { fx.cleanup(); }
  });

  it(`remote scan floor guard refuses ${JSON.stringify(branch)} before fallback or remote calls`, async () => {
    const fx = makeFixture();
    try {
      const cache = new GitCache(fx.dataDir, nullLogger());
      const bare = await cache.ensureClone(fx.originPath);
      const requests: BoundaryProcessRequest[] = [];
      await cache.withBoundaryProcessSpawner(capture(requests), new AbortController().signal, async () => {
        assert.equal(await cache.resolvePublicationFloor(bare, "refs/remotes/origin/main", branch), null);
      });
      assert.deepEqual(remoteCommands(requests), []);
      assert.ok(!requests.some((r) => r.argv.includes("rev-parse")), "invalid input never reaches ref fallback");
    } finally { fx.cleanup(); }
  });

  it(`remote push floor guard refuses ${JSON.stringify(branch)} with a real clean candidate`, async () => {
    const fx = makeFixture();
    try {
      const cache = new GitCache(fx.dataDir, nullLogger());
      const bare = await cache.ensureClone(fx.originPath);
      const candidate = git(bare, "rev-parse", "refs/remotes/origin/main");
      // Explicit public candidate lets malformed branch names reach the floor guard after
      // the real clean-history preflight, rather than failing on a missing tracking ref.
      assert.equal(await cache.scratchPublicationPreflight(bare, branch, candidate), candidate);
      const requests: BoundaryProcessRequest[] = [];
      await cache.withBoundaryProcessSpawner(capture(requests), new AbortController().signal, async () => {
        await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath, undefined, {
          candidate, originalHead: candidate, floor: { kind: "absent" },
        }), (error: unknown) => error instanceof ScratchPublicationError &&
          error.kind === "floor_unverified" && error.step === "floor_refresh");
      });
      assert.deepEqual(remoteCommands(requests), []);
      assert.ok(requests.some((r) => r.argv.includes("rev-list")), "clean-history proof executed");
    } finally { fx.cleanup(); }
  });
}

it("fetchDefaultTip refreshes a literal plus-leading branch instead of treating it as a force refspec", async () => {
  const fx = makeFixture();
  try {
    git(fx.originPath, "branch", "release");
    git(fx.originPath, "branch", "+release");
    const cache = new GitCache(fx.dataDir, nullLogger());
    const bare = await cache.ensureClone(fx.originPath);
    const stale = git(bare, "rev-parse", "refs/remotes/origin/+release");
    git(fx.originPath, "checkout", "+release");
    git(fx.originPath, "commit", "--allow-empty", "-m", "advance literal plus branch");
    const fresh = git(fx.originPath, "rev-parse", "refs/heads/+release");
    assert.notEqual(fresh, stale);
    const requests: BoundaryProcessRequest[] = [];
    await cache.withBoundaryProcessSpawner(capture(requests), new AbortController().signal, async () => {
      assert.equal(await cache.fetchDefaultTip(bare, "+release"), fresh);
    });
    assert.equal(git(bare, "rev-parse", "refs/remotes/origin/+release"), fresh);
    const fetch = remoteCommands(requests).find((r) => r.argv.includes("fetch"));
    assert.ok(fetch);
    assert.deepEqual(fetch.argv.slice(fetch.argv.indexOf("fetch")), ["fetch", "--", "origin", "refs/heads/+release"]);
  } finally { fx.cleanup(); }
});

for (const branch of ["release/nested", "release/café"]) {
  it(`remote guards accept ${branch} and delimit actual argv, replayed by real Git`, async () => {
    const fx = makeFixture();
    try {
      git(fx.originPath, "branch", branch);
      const cache = new GitCache(fx.dataDir, nullLogger());
      const bare = await cache.ensureClone(fx.originPath);
      const candidate = git(bare, "rev-parse", "refs/remotes/origin/main");
      const requests: BoundaryProcessRequest[] = [];
      await cache.withBoundaryProcessSpawner(capture(requests), new AbortController().signal, async () => {
        assert.equal(await cache.fetchDefaultTip(bare, branch), candidate);
        assert.equal(await cache.resolvePublicationFloor(bare, candidate, branch), candidate);
        await cache.pushBranch(bare, branch, "", fx.originPath, undefined, {
          candidate, originalHead: candidate, floor: { kind: "pinned", oid: candidate },
        });
        // The older journal validator intentionally retains its ASCII restriction.
        const workflow = await cache.fetchWorkflowTargetTip(bare, branch);
        assert.deepEqual(workflow, branch === "release/café"
          ? { kind: "unavailable" } : { kind: "present", sha: candidate });
      });
      const remote = remoteCommands(requests);
      const reads = remote.filter((r) => !r.argv.includes("push"));
      assert.equal(reads.filter((r) => r.argv.includes("fetch")).length, branch === "release/café" ? 3 : 4);
      assert.ok(reads.some((r) => r.argv.includes("ls-remote")));
      for (const request of reads) {
        const origin = request.argv.indexOf("origin");
        assert.equal(request.argv[origin - 1], "--", JSON.stringify(request.argv));
        // These captured requests only address this disposable local origin. Execute the
        // exact options/refspecs again to verify real Git accepts the delimiter placement.
        execFileSync(request.argv[0]!, request.argv.slice(1), {
          cwd: request.cwd, env: request.env, stdio: "pipe",
        });
      }
    } finally { fx.cleanup(); }
  });
}
