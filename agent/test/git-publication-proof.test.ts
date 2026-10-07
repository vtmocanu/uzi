import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, ScratchPublicationError, type FetchAgentBranchOptions } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger } from "./helpers.js";

const branch = "agent/issue-2404";
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
  const P = git(writer, "rev-parse", "HEAD");
  const cache = new GitCache(fx.dataDir, nullLogger());
  const bare = await cache.ensureClone(remote);
  const clone = await cache.createOrAttachRunnerClone(bare, 2404, noProofReseed);
  git(clone.path, "commit", "--allow-empty", "-m", "H");
  const H = git(clone.path, "rev-parse", "HEAD");
  const opts: FetchAgentBranchOptions = { context: {
    runId: "proof", generation: 1, branch, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: git(bare, "rev-parse", "refs/remotes/origin/main") },
  } };
  const fetch = async (expectedSha: string) => {
    const result = await cache.fetchAgentBranch(bare, clone.path, branch, "proof", opts);
    assert.ok(result.kind === "updated", JSON.stringify(result));
    assert.equal(result.candidateSha, expectedSha);
    return result;
  };
  await fetch(H);
  const advance = () => {
    git(writer, "commit", "--allow-empty", "-m", "R");
    git(writer, "push", "origin", branch);
    return git(writer, "rev-parse", "HEAD");
  };
  const publish = (originalHead = H, candidate = H, floor: import("../src/git.js").PublicationFloor | undefined = clone.publicationFloor) =>
    cache.pushBranch(bare, branch, "", remote, undefined, { floor: floor ?? { kind: "unverified" }, originalHead, candidate });
  return { fx, remote, writer, cache, bare, clone, P, H, advance, publish, fetch };
}

it("pins the clone floor independently of recovered base and shared tracking", async () => {
  const f = await fixture();
  try {
    assert.deepEqual(f.clone.publicationFloor, { kind: "pinned", oid: f.P });
    const R = f.advance();
    git(f.bare, "fetch", "origin");
    await assert.rejects(f.publish(), (e: unknown) => {
      assert.ok(e instanceof ScratchPublicationError);
      assert.equal(e.kind, "remote_branch_advanced");
      assert.deepEqual((e as unknown as { proof: unknown }).proof, { P: f.P, H: f.H, C: f.H, R, cause: "remote_branch_advanced" });
      return true;
    });
    assert.equal(git(f.remote, "rev-parse", branch), R);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", "refs/uzi-publication-floor"), "");
  } finally { f.fx.cleanup(); }
});

it("a bridge cannot make rewritten original H eligible for cancellation", async () => {
  const f = await fixture();
  try {
    git(f.clone.path, "checkout", "--detach", git(f.remote, "rev-parse", "main"));
    git(f.clone.path, "commit", "--allow-empty", "-m", "rewritten H");
    const H = git(f.clone.path, "rev-parse", "HEAD");
    git(f.clone.path, "branch", "-f", branch, H);
    await f.fetch(H);
    const bridge = await f.cache.bridgeToFloors(f.bare, H, [f.P]);
    assert.equal(bridge.kind, "built");
    if (bridge.kind !== "built") return;
    f.advance();
    await assert.rejects(f.publish(H, bridge.sha), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "remote_candidate_diverged");
  } finally { f.fx.cleanup(); }
});

for (const floor of [{ kind: "absent" }, { kind: "unverified" }] as const) {
  it(`fails closed for ${floor.kind} floor with a divergent remote`, async () => {
    const f = await fixture();
    try {
      f.advance();
      await assert.rejects(f.publish(f.H, f.H, floor), (e: unknown) =>
        e instanceof ScratchPublicationError && e.kind === (floor.kind === "absent" ? "new_remote_candidate_diverged" : "floor_unverified"));
    } finally { f.fx.cleanup(); }
  });
}

it("publishes when the candidate already contains the remote tip", async () => {
  const f = await fixture();
  try {
    const R = f.advance();
    git(f.clone.path, "fetch", f.remote, branch);
    git(f.clone.path, "merge", "--no-edit", "FETCH_HEAD");
    const C = git(f.clone.path, "rev-parse", "HEAD");
    await f.fetch(C);
    await f.publish(f.H, C);
    assert.equal(git(f.remote, "rev-parse", branch), C);
    assert.equal(git(f.remote, "merge-base", R, C), R);
  } finally { f.fx.cleanup(); }
});

for (const phase of ["fetch", "final-list", "wire"] as const) {
  it(`interleaves an independent writer at ${phase} deterministically`, async () => {
    const f = await fixture();
    try {
      const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
      const original = seam.runGit.bind(f.cache);
      let lists = 0;
      let R = "";
      seam.runGit = async (cwd, args, ...rest) => {
        if (args[0] === "ls-remote") lists++;
        if (!R && ((phase === "fetch" && args[0] === "fetch") ||
            (phase === "final-list" && args[0] === "ls-remote" && lists === 2) ||
            (phase === "wire" && args[0] === "push"))) R = f.advance();
        return original(cwd, args, ...rest);
      };
      await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError &&
        e.kind === (phase === "wire" ? "remote_branch_advanced" : "remote_changed_during_refresh"));
      assert.equal(git(f.remote, "rev-parse", branch), R);
    } finally { f.fx.cleanup(); }
  });
}

it("recovered clone base does not replace its freshly captured remote floor", async () => {
  const f = await fixture();
  try {
    const adopted = await f.cache.createOrAttachRunnerClone(f.bare, 2404, noProofReseed, "proof", true);
    assert.equal(adopted.baseCommit, f.H);
    assert.deepEqual(adopted.publicationFloor, { kind: "pinned", oid: f.P });
  } finally { f.fx.cleanup(); }
});

for (const operation of ["ls-remote", "fetch", "update-ref"] as const) {
  it(`fails closed on ${operation} execution errors`, async () => {
    const f = await fixture();
    try {
      f.advance();
      const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
      const original = seam.runGit.bind(f.cache);
      seam.runGit = async (cwd, args, ...rest) => {
        if (args[0] === operation) throw new Error("untrusted remote/exec diagnostic");
        return original(cwd, args, ...rest);
      };
      await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
    } finally { f.fx.cleanup(); }
  });
}

it("unknown ancestry cannot certify supersession", async () => {
  const f = await fixture();
  try {
    f.advance();
    f.cache.ancestry = async () => "unknown";
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { f.fx.cleanup(); }
});

it("a remote not descending from P stays floor_unverified", async () => {
  const f = await fixture();
  try {
    git(f.writer, "checkout", "--detach", "main");
    git(f.writer, "commit", "--allow-empty", "-m", "rewound remote");
    git(f.writer, "push", "--force", "origin", `HEAD:refs/heads/${branch}`);
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "floor_unverified");
  } finally { f.fx.cleanup(); }
});

it("remote scratch that was subsequently deleted prevents supersession", async () => {
  const f = await fixture();
  try {
    const scratch = path.join(f.writer, ".uzi", "scratch");
    fs.mkdirSync(scratch, { recursive: true });
    fs.writeFileSync(path.join(scratch, "artifact"), "unsafe");
    git(f.writer, "add", "-f", ".uzi/scratch/artifact");
    git(f.writer, "commit", "-m", "scratch added");
    git(f.writer, "rm", "-r", ".uzi/scratch");
    git(f.writer, "commit", "-m", "scratch deleted");
    git(f.writer, "push", "origin", branch);
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "scratch_present");
  } finally { f.fx.cleanup(); }
});

it("movement during local remote preflight invalidates the proof", async () => {
  const f = await fixture();
  try {
    const R = f.advance();
    const original = f.cache.scratchPublicationPreflight.bind(f.cache);
    f.cache.scratchPublicationPreflight = async (bare, name, tip) => {
      const result = await original(bare, name, tip);
      if (tip === R) f.advance();
      return result;
    };
    await assert.rejects(f.publish(), (e: unknown) => e instanceof ScratchPublicationError && e.kind === "remote_changed_during_refresh");
  } finally { f.fx.cleanup(); }
});

it("GitCache explicit candidate pins C on both wire attempts after shared tracking advances", async () => {
  const f = await fixture();
  try {
    const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = seam.runGit.bind(f.cache);
    const pushes: string[] = [];
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "push") {
        pushes.push(args[2]!);
        if (pushes.length === 1) throw new Error("transport unavailable before send");
      }
      return original(cwd, args, ...rest);
    };
    await assert.rejects(f.publish(), /transport unavailable/);
    git(f.clone.path, "commit", "--allow-empty", "-m", "later local tracking tip");
    const later = git(f.clone.path, "rev-parse", "HEAD");
    await f.fetch(later);
    assert.equal(await f.cache.trackingTip(f.bare, branch), later);
    // GitCache accepts the explicit candidate; RunRunner separately rechecks ownership on retry.
    await f.publish();
    assert.deepEqual(pushes, [`${f.H}:refs/heads/${branch}`, `${f.H}:refs/heads/${branch}`]);
    assert.equal(git(f.remote, "rev-parse", branch), f.H);
  } finally { f.fx.cleanup(); }
});

it("an unchanged P with a rewritten candidate is divergence rather than supersession", async () => {
  const f = await fixture();
  try {
    git(f.clone.path, "checkout", "--detach", "main");
    git(f.clone.path, "commit", "--allow-empty", "-m", "candidate without P");
    const C = git(f.clone.path, "rev-parse", "HEAD");
    git(f.clone.path, "branch", "-f", branch, C);
    await f.fetch(C);
    await assert.rejects(f.publish(C, C), (e: unknown) =>
      e instanceof ScratchPublicationError && e.kind === "remote_candidate_diverged");
    assert.equal(git(f.remote, "rev-parse", branch), f.P);
  } finally { f.fx.cleanup(); }
});

it("original H descending from P cannot certify a candidate C that lost P", async () => {
  const f = await fixture();
  try {
    git(f.clone.path, "checkout", "--detach", "main");
    git(f.clone.path, "commit", "--allow-empty", "-m", "candidate without P");
    const C = git(f.clone.path, "rev-parse", "HEAD");
    git(f.clone.path, "branch", "-f", branch, C);
    await f.fetch(C);
    const R = f.advance();
    await assert.rejects(f.publish(f.H, C), (e: unknown) =>
      e instanceof ScratchPublicationError && e.kind === "remote_candidate_diverged");
    assert.equal(git(f.remote, "rev-parse", branch), R);
  } finally { f.fx.cleanup(); }
});

it("a deleted remote with pinned P fails closed rather than treating it as an absent floor", async () => {
  const f = await fixture();
  try {
    git(f.writer, "push", "origin", `:${branch}`);
    await assert.rejects(f.publish(), (e: unknown) =>
      e instanceof ScratchPublicationError && e.kind === "floor_unverified");
    assert.equal(git(f.remote, "for-each-ref", "--format=%(objectname)", `refs/heads/${branch}`), "");
  } finally { f.fx.cleanup(); }
});

for (const failure of ["shallow_history", "missing_objects", "exec_failed"] as const) {
  it(`remote preflight ${failure} cannot certify supersession`, async () => {
    const f = await fixture();
    try {
      const R = f.advance();
      const original = f.cache.scratchPublicationPreflight.bind(f.cache);
      f.cache.scratchPublicationPreflight = async (...args) => {
        if (args[2] === R) throw new ScratchPublicationError("remote history cannot be proved", undefined,
          { kind: failure, step: "object_walk" });
        return original(...args);
      };
      await assert.rejects(f.publish(), (e: unknown) =>
        e instanceof ScratchPublicationError && e.kind === failure);
      assert.equal(git(f.remote, "rev-parse", branch), R);
    } finally { f.fx.cleanup(); }
  });
}

it("transport response loss after accepted push then R advance preserves original P/H/C proof", async () => {
  const f = await fixture();
  try {
    const seam = f.cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = seam.runGit.bind(f.cache);
    let lost = false;
    let R = "";
    const pushes: string[] = [];
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "push") pushes.push(args[2]!);
      const response = await original(cwd, args, ...rest);
      if (args[0] === "push" && !lost) {
        lost = true;
        git(f.writer, "fetch", "origin", branch);
        git(f.writer, "reset", "--hard", "FETCH_HEAD");
        R = f.advance();
        git(f.bare, "fetch", "origin");
        git(f.bare, "update-ref", `refs/uzi-runner/${branch}`, R);
        throw new Error("connection reset after accepted push");
      }
      return response;
    };
    await assert.rejects(f.publish(), /connection reset/);
    await assert.rejects(f.publish(), (e: unknown) => {
      assert.ok(e instanceof ScratchPublicationError);
      assert.equal(e.kind, "remote_branch_advanced");
      assert.deepEqual((e as unknown as { proof: unknown }).proof, { P: f.P, H: f.H, C: f.H, R, cause: "remote_branch_advanced" });
      return true;
    });
    assert.deepEqual(pushes, [`${f.H}:refs/heads/${branch}`]);
    assert.equal(git(f.remote, "merge-base", f.H, R), f.H, "the work really was published before supersession");
  } finally { f.fx.cleanup(); }
});
