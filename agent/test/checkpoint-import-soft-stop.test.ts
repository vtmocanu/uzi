import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { GitCache, CheckpointSoftDeadlineError, type BoundaryProcessSpawner } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";

const branch = "agent/issue-3123";
const run = "31233123-1111-2222-3333-444444444444";
function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8" }).trim();
}
function defer() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
// Real Git object/ref/receipt state, with event-controlled cooperative stops.
// The fixture spawner owns exactly one child and joins close before returning cancellation.
function realSpawner(after?: (args: readonly string[]) => void): BoundaryProcessSpawner {
  return async request => {
    const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { after?.(request.argv); resolve({ code: code ?? -1 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; } };
  };
}
async function fixture() {
  const fx = makeFixture();
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await cache.ensureClone(fx.originPath);
  const base = git(bare, "rev-parse", "refs/remotes/origin/main");
  const clone = (await cache.runnerCloneForBranch(bare, branch, "issue-3123", noProofReseed, run)).path;
  const options = { context: { runId: run, generation: 1, branch, kind: "issue" as const, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
  const commit = (label: string) => {
    const sha = git(clone, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
      "commit-tree", git(clone, "rev-parse", base + "^{tree}"), "-p", base, "-m", label);
    git(clone, "update-ref", "refs/heads/" + branch, sha);
    return sha;
  };
  const fetch = (opts = options) => cache.fetchAgentBranch(bare, clone, branch, run, opts);
  const receipt = path.join(bare, "uzi-owed", "receipt-" + createHash("sha256").update(branch).digest("hex") + ".json");
  return { fx, cache, bare, clone, base, options, commit, fetch, receipt };
}

it("m1 expected candidate mismatch preserves ownership and one stage without substituting the newer head", async () => {
  const f = await fixture();
  try {
    const first = f.commit("first"); await f.fetch();
    const newer = f.commit("newer");
    const before = fs.readFileSync(f.receipt, "utf8");
    const options = { ...f.options, expectedCandidateSha: first };
    const result = await f.fetch(options);
    assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
    assert.equal(fs.readFileSync(f.receipt, "utf8"), before);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), newer);
    assert.deepEqual((await f.cache.enumerateOwedCandidates(f.bare, run)).map(c => c.sha), [first]);
  } finally { f.fx.cleanup(); }
});

for (const operation of ["fetch", "bridge"] as const) {
  for (const read of ["ancestry", "producing_context"] as const) {
    it("m1 " + operation + " soft-stops at pre-mutation " + read + " with unchanged ownership", async () => {
      const f = await fixture();
      try {
        const first = f.commit("first"); await f.fetch();
        const incoming = f.commit("next");
        if (operation === "bridge") git(f.bare, "-c", "protocol.file.allow=always",
          "fetch", "--no-tags", f.clone, "refs/heads/" + branch);
        const receipt = fs.readFileSync(f.receipt, "utf8");
        const metadata = fs.readdirSync(path.join(f.bare, "uzi-owed")).sort()
          .map(name => [name, fs.readFileSync(path.join(f.bare, "uzi-owed", name), "utf8")]);
        const stop = new AbortController();
        let fired = false;
        // Inject the stop after the real safe read, retaining the lock and snapshot.
        const internal = f.cache as unknown as {
          ancestry(...args: unknown[]): Promise<unknown>;
          producingContext(...args: unknown[]): Promise<unknown>;
        };
        const key = read === "ancestry" ? "ancestry" : "producingContext";
        const original = internal[key].bind(f.cache);
        internal[key] = async (...args) => {
          const value = await original(...args);
          fired = true; stop.abort();
          return value;
        };
        await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(), new AbortController().signal,
          () => operation === "fetch" ? f.fetch()
            : f.cache.updateTrackingRef(f.bare, branch, incoming, f.options),
          { softSignal: stop.signal }), CheckpointSoftDeadlineError);
        assert.equal(fired, true);
        assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
        assert.equal(fs.readFileSync(f.receipt, "utf8"), receipt);
        assert.deepEqual(fs.readdirSync(path.join(f.bare, "uzi-owed")).sort()
          .map(name => [name, fs.readFileSync(path.join(f.bare, "uzi-owed", name), "utf8")]), metadata);
        assert.deepEqual((await f.cache.enumerateOwedCandidates(f.bare, run)).map(c => c.sha), [first]);
      } finally { f.fx.cleanup(); }
    });
  }
}

for (const transition of ["pin", "governance", "pending", "cas", "owner", "committed"] as const) {
  it("m1 done import settles ownership after soft stop at " + transition, async () => {
    const f = await fixture();
    try {
      f.commit("first"); await f.fetch();
      const incoming = f.commit("incoming");
      const soft = new AbortController();
      let fired = false;
      const trigger = () => { fired = true; soft.abort(); };
      // Disk seam injects only the event; all receipt writes/readbacks remain real.
      const internal = f.cache as unknown as { writeOwedFile(b: string, n: string, v: unknown): Promise<void> };
      const original = internal.writeOwedFile.bind(f.cache);
      internal.writeOwedFile = async (b, n, v) => {
        await original(b, n, v);
        if (n.startsWith("receipt-") && (v as { phase?: string }).phase === transition) trigger();
      };
      const spawner = realSpawner(args => {
        if (transition === "pin" && args.includes("update-ref") && args.some(a => a.startsWith("refs/uzi-owed/"))) trigger();
        if (transition === "governance" && args.some(a => a.endsWith(".receiptversion"))) trigger();
        if (transition === "cas" && args.includes("update-ref") && args.includes("refs/uzi-runner/" + branch)) trigger();
        if (transition === "owner" && args.includes("--replace-all") && args.includes("uzi-trackowner." + branch + ".owner")) trigger();
      });
      await assert.rejects(f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
        () => f.fetch(), { softSignal: soft.signal }), CheckpointSoftDeadlineError);
      assert.equal(fired, true, "the intended transition was reached");
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), incoming);
      assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "committed");
      assert.ok((await f.cache.enumerateOwedCandidates(f.bare, run)).some(c => c.sha === incoming));
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), incoming,
        "a stop after committed readback retains its one private stage without more Git work");
    } finally { f.fx.cleanup(); }
  });
}

for (const mode of ["done", "milestone"] as const) {
  it("m1 " + mode + " import applies its transport stop policy under the same lock", async () => {
    const f = await fixture();
    try {
      const incoming = f.commit("incoming");
      const stop = new AbortController();
      const options = { ...f.options, checkpointImportMode: mode };
      await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(args => {
        if (args.includes("fetch")) stop.abort();
      }), new AbortController().signal, () => f.fetch(options), { softSignal: stop.signal }),
      CheckpointSoftDeadlineError);
      const stages = git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/");
      if (mode === "done") {
        assert.equal(stages, incoming);
        assert.equal(fs.existsSync(f.receipt), false);
      } else {
        assert.equal(stages, "");
        assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "committed");
      }
    } finally { f.fx.cleanup(); }
  });
}

it("m1 cancelled bare-lock waiter never runs or permits a third import to overtake", async () => {
  const f = await fixture();
  try {
    const entered = defer(), release = defer();
    const active = f.cache.withBareLock(f.bare, async () => { entered.resolve(); await release.promise; });
    await entered.promise;
    const stop = new AbortController();
    let transports = 0;
    const cancelled = f.cache.withBoundaryProcessSpawner(realSpawner(args => {
      if (args.includes("fetch")) transports++;
    }), new AbortController().signal, () => f.fetch(), { softSignal: stop.signal });
    const rejected = assert.rejects(cancelled, CheckpointSoftDeadlineError);
    stop.abort(); await rejected;
    let third = false;
    const later = f.cache.withBareLock(f.bare, async () => { third = true; });
    await new Promise<void>(r => setImmediate(r));
    assert.equal(third, false); assert.equal(transports, 0);
    release.resolve(); await Promise.all([active, later]);
    assert.equal(third, true);
  } finally { f.fx.cleanup(); }
});


it("m1 bridge promotion settles its receipt before honoring a cooperative stop", async () => {
  const f = await fixture();
  try {
    f.commit("first"); await f.fetch();
    const head = f.commit("bridge");
    // Populate objects without acquiring any new ownership.
    git(f.bare, "-c", "protocol.file.allow=always", "fetch", "--no-tags", f.clone, "refs/heads/" + branch);
    const stop = new AbortController();
    await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(args => {
      if (args.includes("update-ref") && args.includes("refs/uzi-runner/" + branch)) stop.abort();
    }), new AbortController().signal,
    () => f.cache.updateTrackingRef(f.bare, branch, head, f.options), { softSignal: stop.signal }), CheckpointSoftDeadlineError);
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), head);
    assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "committed");
  } finally { f.fx.cleanup(); }
});

it("m1 snapshot metadata and ref creation settle and release their fence before soft stop", async () => {
  const f = await fixture();
  try {
    const head = f.commit("snapshot"); await f.fetch();
    const stop = new AbortController();
    const internal = f.cache as unknown as { writeCodeSnapshotMetadata(b: string, value: unknown): Promise<void> };
    const original = internal.writeCodeSnapshotMetadata.bind(f.cache);
    internal.writeCodeSnapshotMetadata = async (b, value) => { await original(b, value); stop.abort(); };
    await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(), new AbortController().signal,
      () => f.cache.pinCodeSnapshot(f.bare, run, head, f.base), { softSignal: stop.signal }), CheckpointSoftDeadlineError);
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-cross-check/" + run), head);
    assert.equal(fs.readdirSync(path.join(f.bare, "uzi-code-snapshots")).some(n => n.endsWith(".fence")), false);
  } finally { f.fx.cleanup(); }
});

it("m1 pending ownership remains refused after a stopped unknown-owner preservation", async () => {
  const f = await fixture();
  try {
    const first = f.commit("first"); await f.fetch();
    const receipt = JSON.parse(fs.readFileSync(f.receipt, "utf8"));
    fs.writeFileSync(f.receipt, JSON.stringify({ ...receipt, phase: "pending" }));
    const next = f.commit("pending incoming");
    const stop = new AbortController();
    await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(args => {
      if (args.includes("update-ref") && args.some(a => a.startsWith("refs/uzi-owed/"))) stop.abort();
    }), new AbortController().signal, () => f.fetch(), { softSignal: stop.signal }), CheckpointSoftDeadlineError);
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
    assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "pending");
    assert.ok((await f.cache.enumerateOwedCandidates(f.bare, run)).some(c => c.sha === next));
  } finally { f.fx.cleanup(); }
});
