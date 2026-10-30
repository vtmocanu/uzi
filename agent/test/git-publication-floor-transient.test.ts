import { it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import path from "node:path";
import type { AddressInfo } from "node:net";
import { execFileSync } from "node:child_process";
import { GitCache, ScratchPublicationError, type PublicationFloor } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Real-Git proof that a transient ls-remote/fetch failure during the publication floor refresh is typed
// `floor_unreachable`, while every other failure keeps its existing `floor_unverified`/movement meaning.

const branch = "agent/issue-1324";
function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, "-c", "maintenance.auto=false", "-c", "gc.auto=0",
    "-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", ...args],
  { encoding: "utf8", stdio: "pipe", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
}

async function fixture() {
  const fx = makeFixture();
  const remote = path.join(fx.dataDir, "remote.git");
  git(fx.dataDir, "clone", "--bare", fx.originPath, remote);
  git(remote, "config", "maintenance.auto", "false");
  const writer = path.join(fx.dataDir, "writer");
  git(fx.dataDir, "clone", remote, writer);
  git(writer, "checkout", "-b", branch);
  git(writer, "commit", "--allow-empty", "-m", "P");
  git(writer, "push", "origin", branch);
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await cache.ensureClone(remote);
  const clone = await cache.createOrAttachRunnerClone(bare, 1324, noProofReseed);
  git(clone.path, "commit", "--allow-empty", "-m", "H");
  const H = git(clone.path, "rev-parse", "HEAD");
  const fetched = await cache.fetchAgentBranch(bare, clone.path, branch, "floor", { context: {
    runId: "floor", generation: 1, branch, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: git(bare, "rev-parse", "refs/remotes/origin/main") },
  } });
  assert.ok(fetched.kind === "updated", JSON.stringify(fetched));
  assert.equal(fetched.candidateSha, H);
  const setRemoteUrl = (url: string) => git(bare, "config", "remote.origin.url", url);
  const publish = (floor: PublicationFloor = clone.publicationFloor ?? { kind: "unverified" }) =>
    cache.pushBranch(bare, branch, "", remote, undefined, { floor, originalHead: H, candidate: H });
  const advance = () => {
    git(writer, "commit", "--allow-empty", "-m", "R");
    git(writer, "push", "origin", branch);
    return git(writer, "rev-parse", "HEAD");
  };
  return { fx, remote, cache, bare, clone, H, setRemoteUrl, publish, advance };
}

// A server that answers every request with a fixed status and a unique marker in the body, so a test
// can prove the marker (stand-in for remote stderr text) never reaches the public fields.
async function statusServer(status: number, body: string) {
  const server = http.createServer((_req, res) => { res.writeHead(status, { "content-type": "text/plain" }); res.end(body); });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return { url: `http://127.0.0.1:${port}/repo.git`, close: () => new Promise<void>((resolve) => server.close(() => resolve())) };
}

function closedPortUrl(): Promise<string> {
  return new Promise((resolve) => {
    const s = http.createServer();
    s.listen(0, "127.0.0.1", () => {
      const port = (s.address() as AddressInfo).port;
      s.close(() => resolve(`http://127.0.0.1:${port}/repo.git`));
    });
  });
}

it("tags a refused connection at ls-remote as floor_unreachable", async () => {
  const f = await fixture();
  try {
    f.setRemoteUrl(await closedPortUrl());
    await assert.rejects(f.publish(), (e: unknown) => {
      assert.ok(e instanceof ScratchPublicationError);
      assert.equal(e.kind, "floor_unreachable");
      assert.equal(e.step, "floor_refresh");
      return true;
    });
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", "refs/uzi-publication-floor"), "");
  } finally { f.fx.cleanup(); }
});

it("tags an HTTP 503 as floor_unreachable and keeps remote text out of message and detail", async () => {
  const f = await fixture();
  const marker = "SECRETMARKER-upstream-body";
  const server = await statusServer(503, marker);
  try {
    f.setRemoteUrl(server.url);
    await assert.rejects(f.publish(), (e: unknown) => {
      assert.ok(e instanceof ScratchPublicationError);
      assert.equal(e.kind, "floor_unreachable");
      assert.ok(!e.message.includes(marker) && !e.message.includes("127.0.0.1"), e.message);
      assert.ok(e.detail !== undefined && !e.detail.includes(marker) && !e.detail.includes("127.0.0.1"), e.detail);
      assert.ok(e.rawDetail !== undefined && e.rawDetail.length > 0);
      return true;
    });
  } finally { await server.close(); f.fx.cleanup(); }
});

it("tags a fetch-stage transient failure (ls-remote succeeded) as floor_unreachable", async () => {
  const f = await fixture();
  try {
    const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = seam.runGit.bind(f.cache);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "fetch") throw new Error("git fetch failed: fatal: unable to access: The requested URL returned error: 503");
      return original(cwd, args, ...rest);
    };
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unreachable");
  } finally { f.fx.cleanup(); }
});

it("keeps an authentication failure as floor_unverified", async () => {
  const f = await fixture();
  const server = await statusServer(401, "denied");
  try {
    f.setRemoteUrl(server.url);
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { await server.close(); f.fx.cleanup(); }
});

it("keeps an unrecognised remote failure as floor_unverified", async () => {
  const f = await fixture();
  try {
    f.setRemoteUrl(path.join(f.fx.dataDir, "does-not-exist.git"));
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { f.fx.cleanup(); }
});

it("keeps a local git failure as floor_unverified even when its text looks transient", async () => {
  const f = await fixture();
  try {
    const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = seam.runGit.bind(f.cache);
    // The cleanup `update-ref -d` runs locally in `finally`; a transient-looking message there must not retype it.
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "update-ref" && args[1] === "-d") throw new Error("git update-ref failed: connection reset by peer");
      return original(cwd, args, ...rest);
    };
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { f.fx.cleanup(); }
});

it("keeps an unverified floor refusal as floor_unverified without touching the remote", async () => {
  const f = await fixture();
  try {
    f.setRemoteUrl(await closedPortUrl());
    await assert.rejects(f.publish({ kind: "unverified" }), (e: unknown) =>
      e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { f.fx.cleanup(); }
});

it("leaves remote_branch_advanced unchanged", async () => {
  const f = await fixture();
  try {
    const R = f.advance();
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "remote_branch_advanced");
    assert.equal(git(f.remote, "rev-parse", branch), R);
  } finally { f.fx.cleanup(); }
});
