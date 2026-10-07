import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, CheckpointSoftDeadlineError, type FetchAgentBranchOptions, type TrackingUpdateResult, type PositiveOwedCandidateContext, type OwedCandidateContext } from "../src/git.js";

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-1924";
const RUN = "19241924-1111-2222-3333-444444444444";
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}
function updated(result: TrackingUpdateResult): Extract<TrackingUpdateResult, { kind: "updated" }> {
  assert.equal(result.kind, "updated", JSON.stringify(result));
  assert.ok(result.kind === "updated");
  return result;
}
function root(label: string, parents: string[] = []): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const args = [...IDENT, "commit-tree", tree, "-m", label];
  for (const parent of parents) args.push("-p", parent);
  const sha = gitIn(clone, args);
  gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  return sha;
}
function positiveContext(): PositiveOwedCandidateContext {
  assert.notEqual(opts.context.generation, null);
  assert.ok(!("legacy" in opts.context));
  return opts.context as PositiveOwedCandidateContext;
}
function legacyContext(): void {
  const { generation: _generation, ...identity } = opts.context;
  opts.context = { ...identity, generation: null, legacy: true };
}
async function fetch(): Promise<TrackingUpdateResult> {
  return cache.fetchAgentBranch(bare, clone, BRANCH, opts.context.runId, opts);
}
function tip(): string { return gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]); }
function stamp(): string { return gitIn(bare, ["config", "--local", "--get", `uzi-trackowner.${BRANCH}.owner`]); }
function receiptPath(branch = BRANCH): string {
  const hash = createHash("sha256").update(branch).digest("hex");
  return path.join(bare, "uzi-owed", `receipt-${hash}.json`);
}
async function pins(): Promise<string[]> {
  return (await cache.enumerateOwedCandidates(bare, RUN)).map((c) => c.sha).sort();
}
type ExecOptions = { env: NodeJS.ProcessEnv; timeout?: number; maxBuffer?: number };
type Internals = {
  execScoped(command: string, args: string[], options: ExecOptions): Promise<{ stdout: string; stderr: string }>;
  writeOwedFile(bare: string, name: string, value: unknown): Promise<void>;
};
function faultGit(match: (args: string[]) => boolean, error: Error = Object.assign(new Error("injected git failure"),
  { code: 128, stdout: "", stderr: "injected" })): () => void {
  const internals = cache as unknown as Internals;
  const original = internals.execScoped.bind(cache);
  internals.execScoped = async (command, args, options) => {
    if (command === "git" && match(args)) throw error;
    return original(command, args, options);
  };
  return () => { internals.execScoped = original; };
}
function faultDisk(match: (name: string, value: unknown) => boolean, afterWrite = false): () => void {
  const internals = cache as unknown as Internals;
  const original = internals.writeOwedFile.bind(cache);
  internals.writeOwedFile = async (b, name, value) => {
    if (match(name, value)) {
      if (afterWrite) await original(b, name, value);
      throw new Error("injected metadata I/O failure");
    }
    return original(b, name, value);
  };
  return () => { internals.writeOwedFile = original; };
}
beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-1924", noProofReseed, RUN)).path;
  opts = { context: { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
});
afterEach(() => fx.cleanup());

describe("issue1924 M2 owed candidates", () => {
  for (const pinned of [false, true]) {
    it(`packs the proved literal after a foreign shared-branch promotion (pinned=${pinned})`, async () => {
      const h = root("owned H", [base]); updated(await fetch());
      const proof = await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 1);
      assert.ok(proof.kind === "owned");
      const f = root("foreign F", [h]);
      updated(await cache.fetchAgentBranch(bare, clone, BRANCH, "foreign-run", {
        context: { ...positiveContext(), runId: "foreign-run", generation: 2 },
      }));
      assert.equal(tip(), f);
      gitIn(bare, ["update-ref", `refs/remotes/origin/${BRANCH}`, base]);
      const packed = await cache.checkpointPack(bare, BRANCH, undefined,
        pinned ? { tipSha: proof.sha, excludeSha: base } : undefined, undefined, proof.sha);
      assert.ok(packed);
      assert.equal(packed.tipOid, h);
      const chunks: Buffer[] = [];
      for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
      assert.equal(await packed.exited, 0);
      const destination = path.join(fx.dataDir, "pack-proof.git");
      execFileSync("git", ["init", "-q", "--bare", destination], { env: ENV });
      execFileSync("git", ["-C", destination, "index-pack", "--stdin"],
        { env: ENV, input: Buffer.concat(chunks) });
      const commits = gitIn(destination, ["cat-file", "--batch-all-objects",
        "--batch-check=%(objectname) %(objecttype)"]).split("\n")
        .filter(line => line.endsWith(" commit")).map(line => line.split(" ")[0]);
      assert.deepEqual(commits, [h]);
      assert.deepEqual(await pins(), [h], "foreign promotion retains H without attributing F");
    });
  }

  it("pushes the proved literal after another run promotes the shared branch", async () => {
    const h = root("owned push", [base]); updated(await fetch());
    const proof = await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 1);
    assert.ok(proof.kind === "owned");
    const f = root("foreign push", [h]);
    updated(await cache.fetchAgentBranch(bare, clone, BRANCH, "foreign-run", {
      context: { ...positiveContext(), runId: "foreign-run", generation: 2 },
    }));
    await cache.pushBranch(bare, BRANCH, "fixture-pat", fx.originPath, undefined,
      { floor: { kind: "absent" }, originalHead: h, candidate: proof.sha });
    assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h);
    assert.equal(tip(), f);
    assert.deepEqual(await pins(), [h]);
  });

  it("rejects a nonliteral or nonexistent exact publication source", async () => {
    const h = root("source validation", [base]); updated(await fetch());
    for (const invalid of [`refs/uzi-runner/${BRANCH}`, "f".repeat(40)]) {
      await assert.rejects(cache.checkpointPack(bare, BRANCH, undefined, undefined, undefined, invalid));
      await assert.rejects(cache.pushBranch(bare, BRANCH, "fixture-pat", fx.originPath, undefined,
        { floor: { kind: "absent" }, originalHead: h, candidate: invalid }));
    }
  });

  it("legacy calls and invalid trusted identities fail before any mutation", async () => {
    const before = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
    await assert.rejects(Reflect.apply(cache.fetchAgentBranch, cache, [bare, clone, BRANCH, RUN]), /trusted claim context/);
    await assert.rejects(Reflect.apply(cache.updateTrackingRef, cache, [bare, BRANCH, base]), /trusted claim context/);
    for (const patch of [{ runId: "../run" }, { runId: "run.dot" }, { runId: 123 },
      { branch: 123 }, { barePath: 123 }, { defaultIdentity: { ref: "refs/remotes/origin/main", sha: 123 } },
      { generation: 0 }, { generation: 1.5 }]) {
      await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN,
        { context: { ...opts.context, ...patch } as OwedCandidateContext }), /invalid owed candidate context/);
    }
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]), before);
    assert.equal(fs.existsSync(path.join(bare, "uzi-owed")), false);
  });

  it("H -> unrelated H2 -> unrelated H3 retains every head and actual producing generations across restart", async () => {
    const h = root("H"); updated(await fetch());
    opts.context = { ...positiveContext(), generation: 2 };
    const h2 = root("H2");
    const second = updated(await fetch());
    assert.equal(second.divergence, "divergent");
    assert.equal(second.displacedSha, h);
    opts.context = { ...positiveContext(), generation: 3 };
    const h3 = root("H3"); updated(await fetch());
    assert.deepEqual(await pins(), [h, h2, h3].sort());
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const candidates = await cache.enumerateOwedCandidates(bare, RUN);
    assert.deepEqual(candidates.map((c) => [c.sha, c.contexts.map((x) => x.generation)]).sort(),
      [[h, [1]], [h2, [2]], [h3, [3]]].sort());
    const discovery = await cache.discoverOwedCandidates();
    assert.deepEqual(discovery.map((d) => d.context.generation), [1, 2, 3]);
    assert.ok(discovery.every((d) => d.candidates.length === 1));
  });

  it("restart after default rewrite and GC retains owed roots, reuses proof and exports coverage", async () => {
    const h = root("H with unrelated default"); updated(await fetch());
    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    const contextPath = path.join(bare, "uzi-owed", receipt.context);
    const bytes = fs.readFileSync(contextPath, "utf8");
    const tree = gitIn(bare, ["rev-parse", `${h}^{tree}`]);
    const nextDefault = gitIn(bare, [...IDENT, "commit-tree", tree, "-m", "replacement default"]);
    for (const line of gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]).split("\n")) {
      const [ref, sha] = line.split(" ");
      if (sha === base) gitIn(bare, ["update-ref", ref!, nextDefault]);
    }
    gitIn(bare, ["reflog", "expire", "--expire=now", "--all"]);
    gitIn(bare, ["gc", "--prune=now"]);
    assert.throws(() => gitIn(bare, ["cat-file", "-t", base]));
    assert.equal(gitIn(bare, ["cat-file", "-t", h]), "commit");
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.deepEqual(await pins(), [h]);
    assert.deepEqual((await cache.discoverOwedCandidates()).map((d) => d.candidates.map((c) => c.sha)), [[h]]);
    assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 1)).kind, "owned");
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, base)).removedShas, [],
      "pruned default identity is not remote containment proof");
    const h2 = gitIn(clone, [...IDENT, "commit-tree", tree, "-m", "H2 after default pruned"]);
    gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, h2]);
    updated(await fetch());
    assert.deepEqual(await pins(), [h, h2].sort());
    assert.equal(fs.readFileSync(contextPath, "utf8"), bytes);
    const coverage = await cache.buildRecoveryCoverage(bare, positiveContext(), [h, h2], h2);
    const outPath = path.join(fx.dataDir, "pruned-default.bundle");
    assert.equal((await cache.produceRecoveryBundle(bare, { sourceSha: coverage.sha, outPath })).selfContained, true);
    const fresh = path.join(fx.dataDir, "pruned-default-import");
    execFileSync("git", ["init", fresh], { env: ENV, stdio: "pipe" });
    gitIn(fresh, ["fetch", outPath, "refs/heads/recovered-source:refs/heads/recovered"]);
    for (const sha of [h, h2]) {
      assert.equal(gitIn(fresh, ["cat-file", "-t", sha]), "commit");
      gitIn(fresh, ["merge-base", "--is-ancestor", sha, coverage.sha]);
    }
    opts.context = { ...positiveContext(), generation: 2 };
    const before = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
    const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
    const namesBefore = fs.readdirSync(path.join(bare, "uzi-owed")).sort();
    await assert.rejects(fetch());
    await assert.rejects(cache.updateTrackingRef(bare, BRANCH, h2, opts));
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]), before);
    assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
    assert.deepEqual(fs.readdirSync(path.join(bare, "uzi-owed")).sort(), namesBefore);
  });

  for (const mode of ["malformed", "mismatched", "unreadable"] as const) {
    it(`existing ${mode} worker proof refuses admission without mutation`, async () => {
      const h = root("admitted"); updated(await fetch());
      const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
      const contextPath = path.join(bare, "uzi-owed", receipt.context);
      const record = JSON.parse(fs.readFileSync(contextPath, "utf8"));
      if (mode === "malformed") fs.writeFileSync(contextPath, JSON.stringify({ ...record, extra: true }));
      else if (mode === "mismatched") fs.writeFileSync(contextPath, JSON.stringify({ ...record, generation: 2 }));
      else fs.chmodSync(contextPath, 0o644);
      root("refused successor");
      const before = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      await assert.rejects(fetch(), /owed context|unsafe owed metadata/);
      await assert.rejects(cache.updateTrackingRef(bare, BRANCH, h, opts), /owed context|unsafe owed metadata/);
      assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]), before);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      await assert.rejects(cache.enumerateOwedCandidates(bare, RUN), /owed context|unsafe owed metadata/);
      await assert.rejects(cache.discoverOwedCandidates(), /owed context|owed discovery|unsafe owed metadata/);
    });
  }

  it("fresh missing default commit is rejected before metadata, refs or owner mutation", async () => {
    root("unadmitted");
    opts.context = { ...positiveContext(), defaultIdentity: { ref: "refs/remotes/origin/main", sha: "9".repeat(40) } };
    const before = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
    const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
    await assert.rejects(fetch());
    await assert.rejects(cache.updateTrackingRef(bare, BRANCH, base, opts));
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]), before);
    assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
    assert.equal(fs.existsSync(path.join(bare, "uzi-owed")), false);
  });

  it("descendants still retain H until the supplied remote confirmation covers it", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H descendant", [h]); updated(await fetch());
    assert.deepEqual(await pins(), [h, h2].sort());
    gitIn(bare, ["update-ref", `refs/uzi-checkpoints/${BRANCH}`, h2]);
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, "9".repeat(40))).retainedShas, [h, h2].sort());
    assert.deepEqual(await pins(), [h, h2].sort(), "local checkpoint floor is not confirmation");
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, base)).removedShas, []);
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, h2)).removedShas, [h, h2].sort());
    assert.deepEqual(await pins(), []);
  });

  it("a remotely confirmed incoming head needs no owed pin, but promotion still has context and receipt", async () => {
    const h = root("confirmed", [base]);
    // Transfer the object without writing shared tracking state.
    gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
      `file://${clone}`, `refs/heads/${BRANCH}:refs/test-confirmed`]);
    opts.remotelyConfirmedSha = h;
    updated(await fetch());
    assert.deepEqual(await pins(), []);
    assert.equal(tip(), h);
    assert.equal(JSON.parse(fs.readFileSync(receiptPath(), "utf8")).phase, "committed");
  });

  for (const kind of ["metadata", "pin"] as const) {
    it(`${kind} preservation failure keeps the old ref and stamp`, async () => {
      const h = root("H"); updated(await fetch());
      opts.context = { ...positiveContext(), generation: 2 };
      const h2 = root("H2");
      const restore = kind === "metadata"
        ? faultDisk((name, value) => name.startsWith("context-") && (value as { generation?: number }).generation === 2)
        : faultGit((args) => args.includes("update-ref") && args.includes(`refs/uzi-owed/${RUN}/${h2}`));
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "preservation_failed" });
      restore();
      assert.equal(tip(), h);
      assert.equal(stamp(), RUN);
      assert.ok(gitIn(bare, ["for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"]).includes(h2),
        "incoming staging anchor remains after pin failure");
    });
  }

  for (const mode of ["absent", "duplicate", "malformed", "unreadable", "ambiguous_legacy", "case_collision", "failed_enumeration"] as const) {
    it(`existing head with ${mode} ownership refuses fetch without changing the old anchor`, async () => {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
      let restore = () => {};
      const key = `uzi-trackowner.${BRANCH}.owner`;
      if (mode === "duplicate") {
        gitIn(bare, ["config", "--add", key, RUN]); gitIn(bare, ["config", "--add", key, RUN]);
      } else if (mode === "malformed") gitIn(bare, ["config", key, "run with whitespace"]);
      else if (mode === "unreadable") restore = faultGit((args) => args.includes("--list"));
      else if (mode === "ambiguous_legacy" || mode === "case_collision" || mode === "failed_enumeration") {
        gitIn(bare, ["config", "uzi-trackowner.agent-issue-1924", RUN]);
        if (mode === "case_collision") gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH.toUpperCase()}`, base]);
        else if (mode === "ambiguous_legacy") gitIn(bare, ["update-ref", "refs/uzi-runner/agent-issue-1924", base]);
        else restore = faultGit((args) => args.includes("for-each-ref") && args.includes("--format=%(refname)"));
      }
      const incoming = root("new incoming");
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      restore();
      assert.equal(tip(), base);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      assert.deepEqual(await pins(), [incoming]);
      assert.ok(!(await pins()).includes(base), "unknown old head is never attributed to this run");
    });
  }

  it("checked ref enumeration failure is unknown, never absence", async () => {
    const h = root("H"); updated(await fetch());
    root("H2");
    const restore = faultGit((args) => args.includes("for-each-ref") && args.includes(`refs/uzi-runner/${BRANCH}`));
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    restore();
    assert.equal(tip(), h);
  });

  for (const provenance of ["historical", "receipt"] as const) {
    it(`foreign ${provenance} sole anchor survives promotion and GC under its producing owner`, async () => {
      const foreignRun = "foreign-run";
      const h = root("foreign H");
      if (provenance === "historical") {
        gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
          `file://${clone}`, `refs/heads/${BRANCH}:refs/uzi-runner/${BRANCH}`]);
        gitIn(bare, ["config", "--local", `uzi-trackowner.${BRANCH}.owner`, foreignRun]);
      } else {
        opts.context = { ...positiveContext(), runId: foreignRun, generation: 4 };
        updated(await fetch());
        await cache.reconcileOwedCandidates(bare, foreignRun, h);
      }
      assert.deepEqual(await cache.enumerateOwedCandidates(bare, foreignRun), []);
      opts.context = { ...positiveContext(), runId: RUN, generation: 8 };
      // The incoming run's confirmation cannot discharge a foreign owner's debt.
      opts.remotelyConfirmedSha = h;
      const h2 = root("unrelated current H2");
      const result = updated(await fetch());
      assert.equal(result.divergence, "foreign");
      assert.deepEqual(result.retainedShas, [h2]);
      assert.deepEqual(await pins(), [h2]);
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${foreignRun}/${h}`]), h);
      gitIn(bare, ["reflog", "expire", "--expire=now", "--all"]);
      gitIn(bare, ["gc", "--prune=now"]);
      assert.equal(gitIn(bare, ["cat-file", "-t", h]), "commit");
      cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      const discovery = await cache.discoverOwedCandidates();
      const original = discovery.find((entry) => entry.context.runId === foreignRun)!;
      assert.ok(original.candidates.some((candidate) => candidate.sha === h));
      assert.equal(original.context.generation, provenance === "historical" ? null : 4);
      if (provenance === "historical") {
        assert.deepEqual(original.context, { barePath: bare, branch: BRANCH, runId: foreignRun,
          generation: null, kind: null, defaultIdentity: null, origin: "historical", producer: "unknown" });
      }
    });
  }

  for (const failure of ["metadata", "pin"] as const) {
    it(`foreign ${failure} preservation failure keeps the old anchor and stamp`, async () => {
      const h = root("foreign old H");
      gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
        `file://${clone}`, `refs/heads/${BRANCH}:refs/uzi-runner/${BRANCH}`]);
      gitIn(bare, ["config", "--local", `uzi-trackowner.${BRANCH}.owner`, "foreign-run"]);
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      root("current H2");
      const restore = failure === "metadata"
        ? faultDisk((name, value) => name.startsWith("context-") && (value as { runId?: string }).runId === "foreign-run")
        : faultGit((args) => args.includes("update-ref") && args.includes(`refs/uzi-owed/foreign-run/${h}`));
      try {
        assert.deepEqual(await fetch(), { kind: "not_updated", reason: "preservation_failed" });
      } finally { restore(); }
      assert.equal(tip(), h);
      assert.equal(stamp(), "foreign-run");
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
    });
  }

  it("positively proved foreign ownership is distinct and never pinned as the current run", async () => {
    const foreign = root("foreign");
    opts.context = { ...opts.context, runId: "foreign-run" };
    updated(await fetch());
    opts.context = { ...opts.context, runId: RUN };
    const own = root("own");
    assert.equal(updated(await fetch()).divergence, "foreign");
    assert.deepEqual(await pins(), [own]);
    assert.deepEqual((await cache.enumerateOwedCandidates(bare, "foreign-run")).map((c) => c.sha), [foreign]);
  });

  for (const foreign of [false, true]) {
    it(`stamp failure with ${foreign ? "foreign" : "same-run"} old stamp pins H2; pending blocks H3 and reseed`, async () => {
      const h = root("H", [base]);
      if (foreign) opts.context = { ...opts.context, runId: "foreign-run" };
      updated(await fetch());
      opts.context = { ...positiveContext(), runId: RUN, generation: 2 };
      const h2 = root("H2", [base]);
      const oldStamp = stamp();
      const restore = faultGit((args) => args.includes("config") && args.includes("--replace-all") &&
        args.includes(`uzi-trackowner.${BRANCH}.owner`));
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "owner_stamp_failed" });
      restore();
      assert.equal(tip(), h2);
      assert.equal(stamp(), oldStamp);
      assert.ok((await pins()).includes(h2));
      opts.context = { ...positiveContext(), generation: 3 };
      const h3 = root("H3", [base]);
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      const receiptBefore = fs.readFileSync(receiptPath(), "utf8");
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      assert.equal(tip(), h2);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      assert.equal(fs.readFileSync(receiptPath(), "utf8"), receiptBefore);
      cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      const discovery = await cache.discoverOwedCandidates();
      for (const [sha, generation] of [[h2, 2], [h3, 3]] as const) {
        assert.ok(discovery.some((entry) => entry.context.runId === RUN &&
          entry.context.generation === generation && entry.candidates.some((candidate) => candidate.sha === sha)));
      }
      if (foreign) assert.ok(!(await pins()).includes(h));
      assert.notEqual(h, h2);
      await cache.removeRunnerClone(clone);
      const reseed = await cache.runnerCloneForBranch(bare, BRANCH, "issue-1924", noProofReseed, RUN);
      assert.notEqual(reseed.seededFrom, "tracking");
    });
  }

  it("stamp readback failure leaves a pending receipt even when the stamp write landed", async () => {
    root("H"); updated(await fetch());
    const h2 = root("H2");
    let wrote = false;
    const restore = faultGit((args) => {
      if (args.includes("config") && args.includes("--replace-all") &&
          args.includes(`uzi-trackowner.${BRANCH}.owner`)) { wrote = true; return false; }
      return wrote && args.includes("config") && args.includes("--list");
    });
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "owner_stamp_failed" });
    restore();
    assert.equal(tip(), h2);
    assert.equal(JSON.parse(fs.readFileSync(receiptPath(), "utf8")).phase, "pending");
    root("H3");
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
  });

  for (const phase of ["pending", "committed"] as const) {
    for (const readback of [false, true]) {
      it(`receipt ${phase} ${readback ? "readback" : "write"} failure refuses with pinned incoming`, async () => {
        const h = root("H"); updated(await fetch());
        const h2 = root("H2");
        const restore = faultDisk((name, v) => name.startsWith("receipt-") &&
          (v as { phase?: string }).phase === phase, readback);
        assert.deepEqual(await fetch(), { kind: "not_updated",
          reason: phase === "pending" ? "receipt_pending_failed" : "receipt_commit_failed" });
        restore();
        assert.equal(tip(), phase === "pending" ? h : h2);
        assert.ok((await pins()).includes(h2));
        root("H3");
        if (phase === "pending" && !readback) {
          updated(await fetch()); // Failed before pending write: old committed ownership is still valid.
        } else assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      });
    }
  }

  it("CAS failure leaves the original ref and pending ownership unknown", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("update-ref") &&
      args.includes(`refs/uzi-runner/${BRANCH}`));
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "tracking_update_failed" });
    restore();
    assert.equal(tip(), h);
    assert.ok((await pins()).includes(h2));
    root("H3");
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
  });

  it("missing, unreadable and malformed governed receipts cannot fall back to a same-run stamp", async () => {
    const h = root("H"); updated(await fetch());
    const original = fs.readFileSync(receiptPath());
    root("H2");
    for (const mode of ["deleted", "symlink", "malformed", "pending"]) {
      fs.rmSync(receiptPath(), { force: true });
      if (mode === "symlink") fs.symlinkSync(path.join(bare, "config"), receiptPath());
      if (mode === "malformed") fs.writeFileSync(receiptPath(), "{", { mode: 0o600 });
      if (mode === "pending") fs.writeFileSync(receiptPath(),
        JSON.stringify({ ...JSON.parse(original.toString()), phase: "pending" }), { mode: 0o600 });
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      assert.equal(tip(), h);
    }
  });

  for (const phase of ["committed", "same_stamp_failure", "foreign_stamp_failure"] as const) {
    it(`loss of both sidecars after ${phase} never restores historical ownership`, async () => {
      if (phase === "foreign_stamp_failure") opts.context = { ...opts.context, runId: "foreign-run" };
      const h = root("first"); updated(await fetch());
      opts.context = { ...positiveContext(), runId: RUN, generation: 2 };
      let current = h;
      if (phase !== "committed") {
        current = root("stamp failure");
        const restore = faultGit((args) => args.includes("--replace-all") &&
          args.includes(`uzi-trackowner.${BRANCH}.owner`));
        assert.deepEqual(await fetch(), { kind: "not_updated", reason: "owner_stamp_failed" });
        restore();
      }
      fs.unlinkSync(receiptPath());
      fs.unlinkSync(receiptPath().replace("receipt-", "governed-"));
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      assert.equal(gitIn(bare, ["config", "--get", `uzi-trackowner.${BRANCH}.receiptversion`]), "1");
      cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, current), { kind: "not_owned" });
      opts.context = { ...positiveContext(), generation: 3 };
      const incoming = root("independent after sidecar loss");
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      assert.equal(tip(), current);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      assert.ok((await pins()).includes(incoming));
      const candidates = await cache.enumerateOwedCandidates(bare, RUN);
      assert.ok(candidates.every((candidate) => candidate.contexts.every((context) => !("origin" in context))));
      if (phase === "foreign_stamp_failure") assert.ok(!(await pins()).includes(h));
      await cache.removeRunnerClone(clone);
      const reseed = await cache.runnerCloneForBranch(bare, BRANCH, "issue-1924", noProofReseed, RUN);
      assert.notEqual(reseed.seededFrom, "tracking");
    });
  }

  for (const mode of ["malformed", "duplicate", "unreadable"] as const) {
    it(`a ${mode} governance bit with missing sidecars refuses old same-run ownership`, async () => {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
      gitIn(bare, ["config", `uzi-trackowner.${BRANCH}.owner`, RUN]);
      const key = `uzi-trackowner.${BRANCH}.receiptversion`;
      gitIn(bare, ["config", key, mode === "malformed" ? "bad" : "1"]);
      if (mode === "duplicate") gitIn(bare, ["config", "--add", key, "1"]);
      const restore = mode === "unreadable" ? faultGit((args) => args.includes("--list")) : () => {};
      const incoming = root("independent");
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      restore();
      assert.equal(tip(), base);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      assert.deepEqual(await pins(), [incoming]);
    });
  }

  for (const mode of ["write", "readback"] as const) {
    it(`governance flag ${mode} failure retains incoming and displaced without promotion`, async () => {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
      gitIn(bare, ["config", `uzi-trackowner.${BRANCH}.owner`, RUN]);
      const incoming = root("flag failure");
      let flagWrite = false;
      const restore = faultGit((args) => {
        if (args.includes("--replace-all") && args.includes(`uzi-trackowner.${BRANCH}.receiptversion`)) {
          flagWrite = true;
          return mode === "write";
        }
        return mode === "readback" && flagWrite && args.includes("--list");
      });
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "receipt_pending_failed" });
      restore();
      assert.equal(tip(), base);
      assert.equal(stamp(), RUN);
      assert.deepEqual(await pins(), [base, incoming].sort());
      assert.equal(fs.existsSync(receiptPath()), false);
    });
  }

  for (const mode of ["metadata", "pin"] as const) {
    it(`unknown tracking ownership with incoming ${mode} failure keeps stage and old state`, async () => {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
      const incoming = root("independent failure");
      const configBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
      const restore = mode === "metadata" ? faultDisk((name) => name.startsWith("context-"))
        : faultGit((args) => args.includes("update-ref") && args.includes(`refs/uzi-owed/${RUN}/${incoming}`));
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "preservation_failed" });
      restore();
      assert.equal(tip(), base);
      assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), configBefore);
      assert.equal(fs.existsSync(receiptPath()), false);
      assert.ok(gitIn(bare, ["for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"]).includes(incoming));
      assert.equal(gitIn(bare, ["for-each-ref", "--format=%(objectname)", `refs/uzi-owed/${RUN}/`]).includes(base), false);
    });
  }

  for (const abort of [false, true]) {
    it(`stage cleanup ${abort ? "boundary abort throws" : "failure preserves explicit refusal"}`, async () => {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
      const incoming = root("independent cleanup");
      const restore = faultGit((args) => args.includes("update-ref") && args.includes("-d") &&
        args.some((arg) => arg.startsWith("refs/uzi-incoming/")),
      abort ? new CheckpointSoftDeadlineError() : new Error("cleanup failure"));
      if (abort) await assert.rejects(fetch(), CheckpointSoftDeadlineError);
      else assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      restore();
      assert.equal(tip(), base);
      assert.deepEqual(await pins(), [incoming]);
      assert.ok(gitIn(bare, ["for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"]).includes(incoming));
    });
  }

  it("failed stage cleanup preserves the specific pending-receipt refusal", async () => {
    const h = root("owned"); updated(await fetch());
    const incoming = root("receipt failure");
    const restoreDisk = faultDisk((name) => name.startsWith("receipt-"));
    const restoreGit = faultGit((args) => args.includes("update-ref") && args.includes("-d") &&
      args.some((arg) => arg.startsWith("refs/uzi-incoming/")));
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "receipt_pending_failed" });
    restoreGit(); restoreDisk();
    assert.equal(tip(), h);
    assert.deepEqual(await pins(), [h, incoming].sort());
    assert.ok(gitIn(bare, ["for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"]).includes(incoming));
  });

  it("unknown ownership still throws an ordinary incoming transport failure", async () => {
    gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
    root("untransferred");
    const restore = faultGit((args) => args.includes("fetch"));
    await assert.rejects(fetch(), /failed: injected/);
    restore();
    assert.equal(tip(), base);
    assert.deepEqual(await pins(), []);
  });

  it("crash after promotion or stamp but before commit is pending after restart", async () => {
    const h = root("H"); updated(await fetch());
    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    const h2 = root("H2");
    // Admit the object without promoting; then model the durable pending/promotion crash cuts.
    gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
      `file://${clone}`, `refs/heads/${BRANCH}:refs/test-crash`]);
    fs.writeFileSync(receiptPath(), JSON.stringify({ ...receipt, trackingSha: h2, phase: "pending" }));
    for (const promoted of [false, true]) {
      gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, promoted ? h2 : h]);
      cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    }
  });

  it("strict boot discovery reports corrupt metadata and orphan pins instead of an empty queue", async () => {
    root("H"); updated(await fetch());
    const name = path.join(bare, "uzi-owed", JSON.parse(fs.readFileSync(receiptPath(), "utf8")).context);
    const original = fs.readFileSync(name);
    fs.writeFileSync(name, "{}");
    await assert.rejects(cache.discoverOwedCandidates(), /invalid owed context record/);
    fs.writeFileSync(name, original);
    gitIn(bare, ["update-ref", "refs/uzi-owed/orphan-run/" + base, base]);
    await assert.rejects(cache.discoverOwedCandidates(), /no boot discovery context/);
  });

  it("a missing or non-commit server confirmation never clears owed pins", async () => {
    const h = root("H"); updated(await fetch());
    const tree = gitIn(bare, ["rev-parse", `${h}^{tree}`]);
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, tree)).retainedShas, [h]);
    assert.deepEqual((await cache.reconcileOwedCandidates(bare, RUN, "9".repeat(40))).retainedShas, [h]);
  });

  it("relinquish retention uses the actual producing context rather than the new claim generation", async () => {
    const h = root("H"); updated(await fetch());
    opts.context = { ...positiveContext(), generation: 7 };
    updated(await cache.retainCurrentOwedCandidate(bare, opts));
    assert.deepEqual((await cache.enumerateOwedCandidates(bare, RUN))[0]!.contexts.map((c) => c.generation), [1]);
    assert.equal(tip(), h);
  });

  it("bridge promotion pins both original and constructed incoming before checked receipt promotion", async () => {
    const h = root("H"); updated(await fetch());
    const bridge = (await cache.bridgeToFloors(bare, h, [base]));
    assert.equal(bridge.kind, "built");
    assert.ok(bridge.kind === "built");
    updated(await cache.updateTrackingRef(bare, BRANCH, bridge.sha, opts));
    assert.deepEqual(await pins(), [h, bridge.sha].sort());
  });

  it("bridge pin and stamp failures return distinct refusals while retaining old or new anchors", async () => {
    const h = root("H"); updated(await fetch());
    const bridge = await cache.bridgeToFloors(bare, h, [base]);
    assert.ok(bridge.kind === "built");
    let restore = faultGit((args) => args.includes("update-ref") && args.includes(`refs/uzi-owed/${RUN}/${bridge.sha}`));
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, bridge.sha, opts),
      { kind: "not_updated", reason: "preservation_failed" });
    restore();
    assert.equal(tip(), h);
    restore = faultGit((args) => args.includes("--replace-all") && args.includes(`uzi-trackowner.${BRANCH}.owner`));
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, bridge.sha, opts),
      { kind: "not_updated", reason: "owner_stamp_failed" });
    restore();
    assert.equal(tip(), bridge.sha);
    assert.ok((await pins()).includes(bridge.sha));
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, h, opts),
      { kind: "not_updated", reason: "ownership_unknown" });
  });

  it("ordinary staged fetch errors and boundary aborts throw without changing tracking", async () => {
    const h = root("H"); updated(await fetch());
    root("H2");
    let restore = faultGit((args) => args.includes("fetch"));
    await assert.rejects(fetch(), /failed: injected/);
    restore();
    restore = faultGit((args) => args.includes("fetch"), new CheckpointSoftDeadlineError());
    await assert.rejects(fetch(), CheckpointSoftDeadlineError);
    restore();
    assert.equal(tip(), h);
  });

  it("D/F clearing archives a positively foreign ancestor without pinning it to this run", async () => {
    const ancestor = "uzi/self-improve";
    gitIn(bare, ["update-ref", `refs/uzi-runner/${ancestor}`, base]);
    gitIn(bare, ["config", `uzi-trackowner.${ancestor}.owner`, "foreign-run"]);
    const branch = ancestor + "/" + RUN;
    gitIn(clone, ["branch", branch, base]);
    opts.context = { ...opts.context, branch };
    const result = updated(await cache.fetchAgentBranch(bare, clone, branch, RUN, opts));
    assert.equal(result.candidateSha, base);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-archive/uzi-self-improve/${base}`]), base);
    assert.deepEqual(await pins(), [base], "only incoming, not foreign ancestry, is current-run owed");
  });

  it("D/F clearing preserves an owned unconfirmed ancestor with its original context", async () => {
    const h = root("H"); updated(await fetch());
    const branch = BRANCH + "/new";
    gitIn(clone, ["update-ref", "-d", `refs/heads/${BRANCH}`]);
    gitIn(clone, ["branch", branch, base]);
    opts.context = { ...positiveContext(), generation: 2, branch };
    updated(await cache.fetchAgentBranch(bare, clone, branch, RUN, opts));
    assert.deepEqual(await pins(), [h, base].sort());
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-archive/agent-issue-1924/${h}`]), h);
    assert.equal(fs.existsSync(receiptPath()), false, "receipt cleared only after archive");
  });

  it("D/F unknown ownership does not delete the sole ancestor anchor", async () => {
    gitIn(bare, ["update-ref", "refs/uzi-runner/uzi/self-improve", base]);
    const branch = "uzi/self-improve/" + RUN;
    gitIn(clone, ["branch", branch, base]);
    opts.context = { ...opts.context, branch };
    assert.deepEqual(await cache.fetchAgentBranch(bare, clone, branch, RUN, opts),
      { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(gitIn(bare, ["rev-parse", "refs/uzi-runner/uzi/self-improve"]), base);
  });

  it("coverage is deterministic, separate from tracking, and a fresh self-contained bundle resolves every original root", async () => {
    const roots: string[] = [];
    for (let n = 0; n < 3; n++) { roots.push(root("original " + n)); updated(await fetch()); }
    const current = roots[2]!;
    const coverage = await cache.buildRecoveryCoverage(bare, positiveContext(), [roots[2]!, roots[0]!, roots[1]!, roots[0]!], current);
    const again = await cache.buildRecoveryCoverage(bare, positiveContext(), roots, current);
    assert.deepEqual(coverage, again);
    assert.equal(tip(), current);
    assert.deepEqual(await pins(), roots.sort());
    const outPath = path.join(fx.dataDir, "coverage.bundle");
    const bundle = await cache.produceRecoveryBundle(bare, { sourceSha: coverage.sha, outPath });
    assert.equal(bundle.selfContained, true);
    const fresh = path.join(fx.dataDir, "fresh");
    execFileSync("git", ["init", fresh], { env: ENV, stdio: "pipe" });
    gitIn(fresh, ["fetch", outPath, "refs/heads/recovered-source:refs/heads/recovered"]);
    for (const sha of roots) {
      assert.equal(gitIn(fresh, ["cat-file", "-t", sha]), "commit");
      gitIn(fresh, ["merge-base", "--is-ancestor", sha, coverage.sha]);
      gitIn(fresh, ["-c", "core.hooksPath=/dev/null", "checkout", "--detach", sha]);
      assert.equal(fs.readFileSync(path.join(fresh, "README.md"), "utf8"), "# fixture\n");
    }
  });

  it("coverage parent batching includes all roots beyond a single batch", async () => {
    const roots: string[] = [];
    for (let n = 0; n < 35; n++) roots.push(root("batch root " + n));
    // Single-branch pack transfer carries all roots via a fixture-only parent aggregate.
    const all = root("fixture import", roots);
    updated(await fetch());
    const coverage = await cache.buildRecoveryCoverage(bare, positiveContext(), roots, all);
    assert.equal(gitIn(bare, ["rev-parse", `${coverage.sha}^{tree}`]), gitIn(bare, ["rev-parse", `${all}^{tree}`]));
    for (const sha of roots) gitIn(bare, ["merge-base", "--is-ancestor", sha, coverage.sha]);
    assert.equal(tip(), all);
  });
  it("unknown legacy receipts survive restart and link to the actual admitted legacy context", async () => {
    legacyContext();
    const h = root("legacy"); updated(await fetch());
    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    assert.equal(receipt.generation, null);
    const claim = JSON.parse(fs.readFileSync(path.join(bare, "uzi-owed", receipt.context), "utf8"));
    assert.equal(claim.legacy, true);
    assert.equal(claim.generation, null);
    assert.equal(claim.kind, "issue");
    assert.equal("origin" in claim, false);
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const proof = await cache.committedTrackingOwnership(bare, BRANCH, RUN, h);
    assert.equal(proof.kind, "owned");
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 1), { kind: "not_owned" });
    const h2 = root("legacy successor"); updated(await fetch());
    assert.deepEqual(await pins(), [h, h2].sort());
    assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).every(
      (candidate) => candidate.contexts.every((context) => context.generation === null && "legacy" in context)));
  });

  it("historical same-run stamps preserve unknown production before positive adoption and cover every root", async () => {
    gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
    gitIn(bare, ["config", "--local", `uzi-trackowner.${BRANCH}.owner`, RUN]);
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, base), { kind: "not_owned" });
    assert.equal(gitIn(bare, ["config", "--local", "--list"]).includes("receiptversion"), false);
    opts.context = { ...positiveContext(), generation: 8 };
    const h = root("new known claim"); updated(await fetch());
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const candidates = await cache.enumerateOwedCandidates(bare, RUN);
    const historical = candidates.find((candidate) => candidate.sha === base)!.contexts;
    assert.deepEqual(historical, [{ barePath: bare, branch: BRANCH, runId: RUN, generation: null,
      kind: null, defaultIdentity: null, origin: "historical", producer: "unknown" }]);
    assert.deepEqual(candidates.find((candidate) => candidate.sha === h)!.contexts.map((c) => c.generation), [8]);
    const discovery = await cache.discoverOwedCandidates();
    assert.deepEqual(discovery.map((entry) => entry.context.generation), [null, 8]);
    assert.deepEqual(discovery[0]!.candidates.map((c) => c.sha), [base]);
    assert.deepEqual(discovery[1]!.candidates.map((c) => c.sha), [h]);
    const coverage = await cache.buildRecoveryCoverage(bare, positiveContext(), candidates.map((c) => c.sha), h);
    for (const candidate of candidates) gitIn(bare, ["merge-base", "--is-ancestor", candidate.sha, coverage.sha]);
    assert.deepEqual(await pins(), [base, h].sort(), "coverage is local retention, not remote confirmation");
  });

  it("historical ownership transfers normally to an unknown legacy claim without fabricating production", async () => {
    gitIn(bare, ["update-ref", `refs/uzi-runner/${BRANCH}`, base]);
    gitIn(bare, ["config", "--local", `uzi-trackowner.${BRANCH}.owner`, RUN]);
    legacyContext();
    const h = root("legacy adoption"); updated(await fetch());
    const candidates = await cache.enumerateOwedCandidates(bare, RUN);
    assert.equal(candidates.find((c) => c.sha === base)!.contexts[0]!.kind, null);
    assert.equal("origin" in candidates.find((c) => c.sha === base)!.contexts[0]!, true);
    assert.equal("legacy" in candidates.find((c) => c.sha === h)!.contexts[0]!, true);
    assert.deepEqual(await pins(), [base, h].sort());
  });

  it("repeated legacy claims with changed branches and defaults retain immutable distinct context links", async () => {
    legacyContext();
    const h = root("first legacy"); updated(await fetch());
    const firstReceipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    const firstPath = path.join(bare, "uzi-owed", firstReceipt.context);
    const firstBytes = fs.readFileSync(firstPath, "utf8");
    opts.context = { ...opts.context, defaultIdentity: { ref: "refs/heads/alternate-default", sha: h } };
    const h2 = root("changed legacy default"); updated(await fetch());
    const secondReceipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    assert.notEqual(secondReceipt.context, firstReceipt.context);
    assert.equal(fs.readFileSync(firstPath, "utf8"), firstBytes);
    const branch = "agent/issue-1924-other";
    gitIn(clone, ["branch", branch, h2]);
    opts.context = { ...opts.context, branch };
    updated(await cache.fetchAgentBranch(bare, clone, branch, RUN, opts));
    const thirdReceipt = JSON.parse(fs.readFileSync(receiptPath(branch), "utf8"));
    assert.equal(new Set([firstReceipt.context, secondReceipt.context, thirdReceipt.context]).size, 3);
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const candidates = await cache.enumerateOwedCandidates(bare, RUN);
    assert.equal(candidates.find((c) => c.sha === h)!.contexts.length, 1);
    assert.equal(candidates.find((c) => c.sha === h2)!.contexts.length, 2);
    assert.equal(fs.readFileSync(firstPath, "utf8"), firstBytes);
  });

  it("known positive claim contexts remain immutable when branch or default identity changes", async () => {
    const h = root("positive"); updated(await fetch());
    const firstReceipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    const firstPath = path.join(bare, "uzi-owed", firstReceipt.context);
    const firstBytes = fs.readFileSync(firstPath, "utf8");
    opts.context = { ...positiveContext(), defaultIdentity: { ref: "refs/heads/changed-default", sha: h } };
    const h2 = root("second positive"); updated(await fetch());
    const secondReceipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    assert.notEqual(firstReceipt.context, secondReceipt.context);
    assert.equal(fs.readFileSync(firstPath, "utf8"), firstBytes);
    assert.equal((await cache.enumerateOwedCandidates(bare, RUN)).find((c) => c.sha === h)!.contexts[0]!.generation, 1);
    assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, h2, 1)).kind, "owned");
  });

  it("pending legacy receipts refuse ownership without leaking a success SHA", async () => {
    legacyContext();
    const h = root("pending legacy"); updated(await fetch());
    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    fs.writeFileSync(receiptPath(), JSON.stringify({ ...receipt, phase: "pending" }));
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h), { kind: "not_owned" });
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(await pins(), [h]);
  });

  it("coverage rejects legacy and historical provenance before creating an aggregate", async () => {
    const positive = positiveContext();
    legacyContext();
    await assert.rejects(cache.buildRecoveryCoverage(bare, opts.context as PositiveOwedCandidateContext, [base], base),
      /positive claim context/);
    const historical = { ...positive, generation: null, kind: null, defaultIdentity: null,
      origin: "historical", producer: "unknown" };
    await assert.rejects(cache.buildRecoveryCoverage(bare, historical as unknown as PositiveOwedCandidateContext, [base], base),
      /positive claim context/);
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-coverage/"]), "");
    assert.equal(fs.existsSync(path.join(bare, "uzi-owed")), false);
  });

  it("foreign positive generation transfers to a different legacy run with honest provenance", async () => {
    const foreignRun = "foreign-run";
    opts.context = { ...positiveContext(), runId: foreignRun, generation: 4 };
    const foreignContext = { ...opts.context };
    const h = root("foreign positive H"); updated(await fetch());
    // Leave H's owned tracking ref as its sole reachability anchor.
    await cache.reconcileOwedCandidates(bare, foreignRun, h);
    assert.deepEqual(await cache.enumerateOwedCandidates(bare, foreignRun), []);

    opts.context = { ...positiveContext(), runId: RUN };
    legacyContext();
    const h2 = root("different legacy H2");
    const result = updated(await fetch());
    assert.equal(result.candidateSha, h2);
    assert.equal(result.divergence, "foreign");
    assert.deepEqual(result.retainedShas, [h2]);
    assert.equal(tip(), h2);
    assert.equal(stamp(), RUN);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${foreignRun}/${h}`]), h);
    assert.deepEqual((await cache.enumerateOwedCandidates(bare, foreignRun)).map(
      (candidate) => [candidate.sha, candidate.contexts]), [[h, [foreignContext]]]);
    const candidates = await cache.enumerateOwedCandidates(bare, RUN);
    assert.deepEqual(candidates.map((candidate) => candidate.sha), [h2]);
    assert.deepEqual(candidates[0]!.contexts, [opts.context]);

    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    assert.equal(receipt.runId, RUN);
    assert.equal(receipt.generation, null);
    assert.equal(receipt.trackingSha, h2);
    assert.equal(receipt.phase, "committed");
    const context = JSON.parse(fs.readFileSync(path.join(bare, "uzi-owed", receipt.context), "utf8"));
    assert.equal(context.runId, RUN);
    assert.equal(context.generation, null);
    assert.equal(context.legacy, true);
    assert.equal("origin" in context, false);
    const proof = await cache.committedTrackingOwnership(bare, BRANCH, RUN, h2);
    assert.equal(proof.kind, "owned");
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h2, 4), { kind: "not_owned" });
  });

  it("positive generation stays real without a guard flag and cannot downgrade to unknown legacy", async () => {
    opts.context = { ...positiveContext(), generation: 19 };
    const h = root("real generation"); updated(await fetch());
    assert.equal(JSON.parse(fs.readFileSync(receiptPath(), "utf8")).generation, 19);
    assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 19)).kind, "owned");
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 20), { kind: "not_owned" });
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, base), { kind: "not_owned" });
    legacyContext();
    root("feature loss");
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(tip(), h);
    assert.equal(JSON.parse(fs.readFileSync(receiptPath(), "utf8")).generation, 19);
  });

  it("trusted context and default identity are copied before the first await", async () => {
    const h = root("frozen"); const original = positiveContext();
    const identity = { ...original.defaultIdentity };
    const inFlight = fetch();
    opts.context.runId = "other-run";
    opts.context.generation = 90;
    opts.context.kind = "self_improve";
    opts.context.branch = "changed";
    opts.context.defaultIdentity.sha = "9".repeat(40);
    opts.context.defaultIdentity.ref = "refs/heads/changed";
    updated(await inFlight);
    const proof = await cache.committedTrackingOwnership(bare, BRANCH, RUN, h, 1);
    assert.ok(proof.kind === "owned");
    assert.equal(proof.context.kind, "issue");
    assert.deepEqual(proof.context.defaultIdentity, identity);
    assert.deepEqual(await pins(), [h]);
  });

  it("invalid registry kinds and malformed positive generation cannot become legacy admission", async () => {
    for (const patch of [{ kind: "implement" }, { generation: undefined, legacy: true },
      { generation: 0, legacy: true }, { generation: 1, legacy: true },
      { generation: null }, { origin: "historical", generation: null }]) {
      await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN,
        { context: { ...opts.context, ...patch } as unknown as OwedCandidateContext }), /invalid owed candidate context/);
    }
    assert.equal(fs.existsSync(path.join(bare, "uzi-owed")), false);
  });

  it("committed ownership rejects unreadable contexts, bare paths, mismatched runs and forged links", async () => {
    const h = root("committed proof"); updated(await fetch());
    const receipt = JSON.parse(fs.readFileSync(receiptPath(), "utf8"));
    const contextPath = path.join(bare, "uzi-owed", receipt.context);
    const bytes = fs.readFileSync(contextPath);
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, "foreign-run", h), { kind: "not_owned" });
    assert.deepEqual(await cache.committedTrackingOwnership(clone, BRANCH, RUN, h), { kind: "not_owned" });
    fs.rmSync(contextPath);
    fs.symlinkSync(path.join(bare, "config"), contextPath);
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h), { kind: "not_owned" });
    fs.unlinkSync(contextPath);
    fs.writeFileSync(contextPath, bytes, { mode: 0o600 });
    fs.writeFileSync(receiptPath(), JSON.stringify({ ...receipt, context: "../config" }));
    assert.deepEqual(await cache.committedTrackingOwnership(bare, BRANCH, RUN, h), { kind: "not_owned" });
    assert.deepEqual(await pins(), [h], "an owed pin alone does not give committed ownership");
  });

  it("same-run historical D/F ancestors keep unknown roots and foreign ancestors are never current-run pins", async () => {
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent", base]);
    gitIn(bare, ["config", "--local", "uzi-trackowner.agent.owner", RUN]);
    const h = root("historical D/F"); updated(await fetch());
    const old = (await cache.enumerateOwedCandidates(bare, RUN)).find((c) => c.sha === base)!;
    assert.equal(old.contexts[0]!.generation, null);
    assert.equal(old.contexts[0]!.branch, "agent");
    assert.equal("origin" in old.contexts[0]!, true);
    assert.equal(gitIn(bare, ["rev-parse", "refs/uzi-archive/agent/" + base]), base);
    const branch = "foreign/child";
    gitIn(bare, ["update-ref", "refs/uzi-runner/foreign", base]);
    gitIn(bare, ["config", "--local", "uzi-trackowner.foreign.owner", "foreign-run"]);
    gitIn(clone, ["branch", branch, h]);
    opts.context = { ...positiveContext(), branch };
    updated(await cache.fetchAgentBranch(bare, clone, branch, RUN, opts));
    assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).find((c) => c.sha === base)!
      .contexts.every((c) => c.branch !== "foreign"));
    assert.deepEqual(await cache.enumerateOwedCandidates(bare, "foreign-run"), []);
  });

});
