import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { GitCache, CheckpointSoftDeadlineError, type BoundaryProcessSpawner } from "../src/git.js";
import { GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { RunRunner } from "../src/runner.js";
import { WorkerClient } from "../src/client.js";
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
    await assert.rejects(f.fetch(options), { name: "Error", message: "checkpoint candidate changed during acquisition" });
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
        let latePins = 0;
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
        await assert.rejects(f.cache.withBoundaryProcessSpawner(realSpawner(args => {
          if (fired && args.includes("update-ref") && args.some(a => a.startsWith("refs/uzi-owed/"))) latePins++;
        }), new AbortController().signal,
          () => operation === "fetch" ? f.fetch()
            : f.cache.updateTrackingRef(f.bare, branch, incoming, f.options),
          { softSignal: stop.signal }), CheckpointSoftDeadlineError);
        assert.equal(fired, true);
        assert.equal(latePins, 0, "no ownership pin command may run after pre-mutation stop");
        assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
        assert.equal(fs.readFileSync(f.receipt, "utf8"), receipt);
        assert.deepEqual(fs.readdirSync(path.join(f.bare, "uzi-owed")).sort()
          .map(name => [name, fs.readFileSync(path.join(f.bare, "uzi-owed", name), "utf8")]), metadata);
        assert.deepEqual((await f.cache.enumerateOwedCandidates(f.bare, run)).map(c => c.sha), [first]);
      } finally { f.fx.cleanup(); }
    });
  }
}

const transitions = ["context", "pin", "governance", "pending", "cas", "owner", "committed", "readback"] as const;

// Inject only after a real transition has completed. Generation 2 forces a new context
// write; the incoming SHA distinguishes its pin/readback from displaced-head preservation.
function stopAtTransition(f: Awaited<ReturnType<typeof fixture>>, incoming: string,
  transition: typeof transitions[number], trigger: () => void, generation = 2): BoundaryProcessSpawner {
  const internal = f.cache as unknown as {
    writeOwedFile(b: string, n: string, v: unknown): Promise<void>;
    checkedTrackingOwner(b: string, branch: string, sha: string): Promise<
      { runId: string; generation?: number | null; context?: string } | undefined>;
  };
  const write = internal.writeOwedFile.bind(f.cache);
  internal.writeOwedFile = async (b, n, v) => {
    await write(b, n, v);
    const record = v as { phase?: string; generation?: number };
    if (transition === "context" && n.startsWith("context-") && record.generation === generation) trigger();
    if (n.startsWith("receipt-") && record.phase === transition) trigger();
  };
  const read = internal.checkedTrackingOwner.bind(f.cache);
  internal.checkedTrackingOwner = async (b, name, sha) => {
    const owner = await read(b, name, sha);
    if (transition === "readback" && name === branch && sha === incoming &&
        owner?.runId === run && owner.generation === generation && owner.context) trigger();
    return owner;
  };
  return realSpawner(args => {
    if (transition === "pin" && args.includes("update-ref") &&
        args.includes("refs/uzi-owed/" + run + "/" + incoming)) trigger();
    if (transition === "governance" && args.includes("--replace-all") &&
        args.includes("uzi-trackowner." + branch + ".receiptversion")) trigger();
    if (transition === "cas" && args.includes("update-ref") && args.includes("refs/uzi-runner/" + branch)) trigger();
    if (transition === "owner" && args.includes("--replace-all") &&
        args.includes("uzi-trackowner." + branch + ".owner")) trigger();
  });
}

for (const transition of transitions) {
  it("m1 done import settles ownership after soft stop at " + transition, async () => {
    const f = await fixture();
    try {
      f.commit("first"); await f.fetch();
      const incoming = f.commit("incoming");
      const soft = new AbortController();
      let fired = false;
      const trigger = () => { fired = true; soft.abort(); };
      const options = { context: { ...f.options.context, generation: 2 } };
      const spawner = stopAtTransition(f, incoming, transition, trigger);
      await assert.rejects(f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
        () => f.fetch(options), { softSignal: soft.signal }), CheckpointSoftDeadlineError);
      assert.equal(fired, true, "the intended transition was reached");
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming, 2),
        { kind: "owned", sha: incoming, context: options.context });
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming, 1),
        { kind: "not_owned" });
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), incoming);
      assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "committed");
      assert.ok((await f.cache.enumerateOwedCandidates(f.bare, run)).some(c => c.sha === incoming));
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), incoming,
        "a stop after committed readback retains its one private stage without more Git work");
    } finally { f.fx.cleanup(); }
  });
}

for (const edge of [
  { operation: "first", transition: "context", generation: 1 },
  { operation: "displaced", transition: "pin", generation: 2 },
  { operation: "bridge", transition: "context", generation: 2 },
  { operation: "bridge", transition: "pending", generation: 2 },
  { operation: "bridge", transition: "readback", generation: 2 },
] as const) {
  it("m3 " + edge.operation + " import settles under its lock after soft stop at " + edge.transition, async () => {
    const f = await fixture();
    let waiter: Promise<void> | undefined;
    try {
      const first = edge.operation === "first" ? undefined : f.commit("established first");
      if (first) await f.fetch();
      else assert.equal(fs.existsSync(f.receipt), false);
      const incoming = f.commit("edge incoming");
      if (edge.operation === "bridge") git(f.bare, "-c", "protocol.file.allow=always",
        "fetch", "--no-tags", f.clone, "refs/heads/" + branch);
      const options = { context: { ...f.options.context, generation: edge.generation } };
      const soft = new AbortController();
      const events: string[] = [];
      let fired = 0;
      let receiptAtStop: unknown;
      let readbackOwner: unknown;
      let readbackError: unknown;
      const trigger = () => {
        if (fired++) return;
        receiptAtStop = fs.existsSync(f.receipt) ? JSON.parse(fs.readFileSync(f.receipt, "utf8")) : undefined;
        events.push("stop");
        soft.abort();
        // Queue a real competing lock holder; it must enter only after committed readback.
        waiter = f.cache.withBareLock(f.bare, async () => { events.push("next_lock"); });
      };
      const spawnTransition = stopAtTransition(f, incoming, edge.transition,
        edge.operation === "displaced" ? () => undefined : trigger, edge.generation);
      const internal = f.cache as unknown as {
        writeOwedFile(b: string, n: string, v: unknown): Promise<void>;
        checkedTrackingOwner(b: string, name: string, sha: string): Promise<unknown>;
      };
      const write = internal.writeOwedFile.bind(f.cache);
      internal.writeOwedFile = async (b, n, v) => {
        await write(b, n, v);
        if (n.startsWith("context-") && (v as { generation?: number }).generation === edge.generation) {
          events.push("incoming_context");
        }
      };
      const read = internal.checkedTrackingOwner.bind(f.cache);
      internal.checkedTrackingOwner = async (b, name, sha) => {
        try {
          const owner = await read(b, name, sha);
          if (name === branch && sha === incoming) {
            readbackOwner = owner;
            events.push("committed_readback");
          }
          return owner;
        } catch (error) { readbackError = error; throw error; }
      };
      const spawner: BoundaryProcessSpawner = edge.operation === "displaced"
        ? realSpawner(args => {
          if (args.includes("update-ref") && args.includes("refs/uzi-owed/" + run + "/" + first)) {
            events.push("displaced_pin");
            trigger();
          }
          if (args.includes("update-ref") && args.includes("refs/uzi-owed/" + run + "/" + incoming)) {
            events.push("incoming_pin");
          }
          if (args.includes("--replace-all") && args.includes("uzi-trackowner." + branch + ".receiptversion")) {
            events.push("governance");
          }
        }) : spawnTransition;
      let failure: unknown;
      await f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
        () => edge.operation === "bridge"
          ? f.cache.updateTrackingRef(f.bare, branch, incoming, options) : f.fetch(options),
        { softSignal: soft.signal }).catch(error => { failure = error; });
      await waiter;
      assert.equal(fired, 1, "the exact real transition fired once");
      assert.ok(failure instanceof CheckpointSoftDeadlineError);
      assert.equal(readbackError, undefined, "the real readback query must not be swallowed");
      const receipt = JSON.parse(fs.readFileSync(f.receipt, "utf8"));
      assert.deepEqual(receipt, { version: 1, branch, runId: run, generation: edge.generation,
        context: receipt.context, trackingSha: incoming, phase: "committed" });
      assert.deepEqual(readbackOwner, { runId: run, generation: edge.generation, context: receipt.context });
      assert.ok(events.indexOf("committed_readback") > events.indexOf("stop"));
      assert.ok(events.indexOf("next_lock") > events.indexOf("committed_readback"));
      if (edge.operation === "first") assert.equal(receiptAtStop, undefined);
      if (edge.operation === "displaced") {
        assert.equal((receiptAtStop as { trackingSha: string }).trackingSha, first);
        assert.ok(events.indexOf("stop") < events.indexOf("incoming_context"));
        assert.ok(events.indexOf("incoming_context") < events.indexOf("incoming_pin"));
        assert.ok(events.indexOf("incoming_pin") < events.indexOf("governance"));
      }
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming, edge.generation),
        { kind: "owned", sha: incoming, context: options.context });
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming,
        edge.generation === 1 ? 2 : 1), { kind: "not_owned" });
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), incoming);
      const candidates = await f.cache.enumerateOwedCandidates(f.bare, run);
      assert.deepEqual(candidates.map(c => c.sha).sort(), first ? [first, incoming].sort() : [incoming]);
      assert.deepEqual(candidates.find(c => c.sha === incoming)?.contexts, [options.context]);
      if (first) assert.deepEqual(candidates.find(c => c.sha === first)?.contexts, [f.options.context]);
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"),
        edge.operation === "bridge" ? "" : incoming);
    } finally {
      await waiter;
      f.fx.cleanup();
    }
  });
}

for (const transition of transitions) {
  it("m3 hard signal at " + transition + " retains anchors and refuses subsequent import Git", async () => {
    const f = await fixture();
    try {
      const first = f.commit("first"); await f.fetch();
      const incoming = f.commit("hard incoming");
      const options = { context: { ...f.options.context, generation: 2 } };
      const hard = new AbortController();
      let fired = false, laterGit = 0;
      const spawnAtTransition = stopAtTransition(f, incoming, transition, () => {
        fired = true; hard.abort();
      });
      const spawner: BoundaryProcessSpawner = request => {
        if (fired) laterGit++;
        return spawnAtTransition(request);
      };
      let failure: unknown;
      await f.cache.withBoundaryProcessSpawner(spawner, hard.signal, () => f.fetch(options))
        .catch(error => { failure = error; });
      assert.equal(fired, true, "the intended transition was reached");
      assert.ok(failure instanceof GitBoundaryAbortError);
      assert.equal(failure.checkpointBoundary, "hard");
      assert.equal(failure instanceof CheckpointSoftDeadlineError, false);
      await assert.rejects(f.cache.withBoundaryProcessSpawner(spawner, hard.signal,
        () => f.fetch(options)), GitBoundaryAbortError);
      assert.equal(laterGit, 0, "an aborted hard signal admits no further import subprocess");

      const moved = ["cas", "owner", "committed", "readback"].includes(transition);
      const phase = ["pending", "cas", "owner", "committed"].includes(transition) ? "pending" : "committed";
      const receipt = JSON.parse(fs.readFileSync(f.receipt, "utf8"));
      assert.equal(receipt.phase, phase);
      assert.equal(git(f.clone, "rev-parse", "refs/heads/" + branch), incoming);
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), moved ? incoming : first);
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), incoming);
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + run + "/" + first), first);
      if (transition !== "context") {
        assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + run + "/" + incoming), incoming);
      } else {
        assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)",
          "refs/uzi-owed/" + run + "/" + incoming), "");
      }
      const ownership = await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming, 2);
      assert.deepEqual(ownership, transition === "readback"
        ? { kind: "owned", sha: incoming, context: options.context } : { kind: "not_owned" });
      if (phase === "pending") {
        assert.equal(receipt.trackingSha, incoming);
        assert.equal(receipt.generation, 2);
        assert.deepEqual(await f.fetch(options), { kind: "not_updated", reason: "ownership_unknown" });
        assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "pending");
      }
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
      const imported = f.cache.withBoundaryProcessSpawner(realSpawner(args => {
        if (args.includes("fetch")) stop.abort();
      }), new AbortController().signal, () => f.fetch(options), { softSignal: stop.signal });
      if (mode === "done") await assert.rejects(imported, CheckpointSoftDeadlineError);
      else assert.equal((await imported).kind, "updated", "started milestone finishes under its hard budget");
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
    const retry = await f.cache.pinCodeSnapshot(f.bare, run, head, f.base);
    assert.equal(retry.freshReceipt, undefined, "a settled soft stop cannot turn retry into fresh-empty authority");
    await assert.rejects(f.cache.deleteCodeSnapshot(f.bare, run, head, {
      sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId: run,
        head, pinGeneration: 0, outcome: "failed" },
    }), /fresh no-reader receipt required/);
    assert.equal(await f.cache.codeSnapshotTip(f.bare, run), head);
  } finally { f.fx.cleanup(); }
});

for (const mode of ["done", "milestone", "bridge"] as const) {
  it("m1 " + mode + " preserves CAS refusal and pending anchors after soft stop", async () => {
    const f = await fixture();
    try {
      const first = f.commit("first"); await f.fetch();
      const incoming = f.commit("CAS refusal");
      if (mode === "bridge") git(f.bare, "-c", "protocol.file.allow=always",
        "fetch", "--no-tags", f.clone, "refs/heads/" + branch);
      const stop = new AbortController();
      let failed = false, laterGit = 0;
      let phaseAtCas: unknown;
      const spawnReal = realSpawner();
      const spawner: BoundaryProcessSpawner = async request => {
        if (failed) laterGit++;
        if (request.argv.includes("update-ref") && request.argv.includes("refs/uzi-runner/" + branch)) {
          phaseAtCas = JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase;
          failed = true; stop.abort();
          // Force a real Git CAS failure without changing the tracking ref.
          const argv = [...request.argv]; argv[argv.length - 1] = "0".repeat(40);
          return spawnReal({ ...request, argv });
        }
        return spawnReal(request);
      };
      const result = await f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
        () => mode === "bridge" ? f.cache.updateTrackingRef(f.bare, branch, incoming, f.options)
          : f.cache.fetchAgentBranch(f.bare, f.clone, branch, run,
            { ...f.options, checkpointImportMode: mode }), { softSignal: stop.signal });
      assert.deepEqual(result, { kind: "not_updated", reason: "tracking_update_failed" });
      assert.equal(failed, true);
      assert.equal(phaseAtCas, "pending");
      assert.equal(laterGit, 0, "no optional Git work on a refused, stopped import");
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
      assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "pending");
      for (const sha of [first, incoming]) {
        assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + run + "/" + sha), sha);
      }
      // Pending is still refused by the real ownership reader, with no repair.
      const pending = fs.readFileSync(f.receipt, "utf8");
      assert.deepEqual(await f.fetch(), { kind: "not_updated", reason: "ownership_unknown" });
      assert.equal(fs.readFileSync(f.receipt, "utf8"), pending);
    } finally { f.fx.cleanup(); }
  });
}

it("m1 cleanup soft stop cannot erase a proven CAS mutation failure", async () => {
  const f = await fixture();
  try {
    const first = f.commit("first"); await f.fetch();
    const incoming = f.commit("CAS then cleanup stop");
    const internals = f.cache as unknown as {
      hasOwedAnchorUnderLock(bare: string, run: string, sha: string): Promise<boolean>;
    };
    internals.hasOwedAnchorUnderLock = async () => { throw new CheckpointSoftDeadlineError(); };
    const spawnReal = realSpawner();
    let refused = false;
    const spawner: BoundaryProcessSpawner = async request => {
      if (request.argv.includes("update-ref") && request.argv.includes("refs/uzi-runner/" + branch)) {
        refused = true;
        const argv = [...request.argv]; argv[argv.length - 1] = "0".repeat(40);
        return spawnReal({ ...request, argv });
      }
      return spawnReal(request);
    };
    const result = await f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
      () => f.fetch());
    assert.equal(refused, true);
    assert.deepEqual(result, { kind: "not_updated", reason: "tracking_update_failed" });
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
    assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "pending");
    for (const sha of [first, incoming]) {
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + run + "/" + sha), sha);
    }
    assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), incoming);
  } finally { f.fx.cleanup(); }
});

it("m3 D/F owned ancestor preservation finishes archive and promotion before a late soft stop", async () => {
  const f = await fixture();
  try {
    const ancestor = f.commit("owned ancestor"); await f.fetch();
    const childBranch = branch + "/new";
    git(f.clone, "update-ref", "-d", "refs/heads/" + branch);
    git(f.clone, "branch", childBranch, f.base);
    const options = { context: { ...f.options.context, generation: 2, branch: childBranch } };
    const soft = new AbortController();
    let preserved = false, archived = false, promoted = false;
    const spawner = realSpawner(args => {
      if (args.includes("update-ref") && args.includes("refs/uzi-owed/" + run + "/" + ancestor)) {
        preserved = true; soft.abort();
      }
      if (args.includes("update-ref") && args.includes("refs/uzi-archive/agent-issue-3123/" + ancestor)) archived = true;
      if (args.includes("update-ref") && args.includes("refs/uzi-runner/" + childBranch)) promoted = true;
    });
    await assert.rejects(f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
      () => f.cache.fetchAgentBranch(f.bare, f.clone, childBranch, run, options),
      { softSignal: soft.signal }), CheckpointSoftDeadlineError);
    assert.equal(preserved, true);
    assert.equal(archived, true);
    assert.equal(promoted, true);
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-archive/agent-issue-3123/" + ancestor), ancestor);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", "refs/uzi-runner/" + branch),
      "refs/uzi-runner/" + childBranch);
    assert.equal(fs.existsSync(f.receipt), false, "ancestor receipt is cleared after archival");
    assert.equal(git(f.bare, "config", "--get", "uzi-trackowner." + branch + ".receiptversion"), "1");
    const candidates = await f.cache.enumerateOwedCandidates(f.bare, run);
    assert.deepEqual(candidates.map(c => c.sha).sort(), [ancestor, f.base].sort());
    assert.deepEqual(candidates.find(c => c.sha === ancestor)?.contexts, [f.options.context]);
    assert.deepEqual(candidates.find(c => c.sha === f.base)?.contexts, [options.context]);
    assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, childBranch, run, f.base, 2),
      { kind: "owned", sha: f.base, context: options.context });
    assert.equal(git(f.clone, "rev-parse", "refs/heads/" + childBranch), f.base);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), f.base);
  } finally { f.fx.cleanup(); }
});

it("m3 unknown owner incoming pin settles after soft stop without adopting existing tracking", async () => {
  const f = await fixture();
  try {
    git(f.bare, "update-ref", "refs/uzi-runner/" + branch, f.base);
    const incoming = f.commit("unknown owner incoming");
    const config = git(f.bare, "config", "--local", "--list");
    const soft = new AbortController();
    let pinned = false;
    const result = await f.cache.withBoundaryProcessSpawner(realSpawner(args => {
      if (args.includes("update-ref") && args.includes("refs/uzi-owed/" + run + "/" + incoming)) {
        pinned = true; soft.abort();
      }
    }), new AbortController().signal, () => f.fetch(), { softSignal: soft.signal });
    assert.equal(pinned, true);
    assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), f.base);
    assert.equal(git(f.bare, "config", "--local", "--list"), config);
    assert.equal(fs.existsSync(f.receipt), false);
    assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, f.base, 1),
      { kind: "not_owned" });
    const candidates = await f.cache.enumerateOwedCandidates(f.bare, run);
    assert.deepEqual(candidates.map(c => c.sha), [incoming]);
    assert.deepEqual(candidates[0]?.contexts, [f.options.context]);
    assert.equal(git(f.clone, "rev-parse", "refs/heads/" + branch), incoming);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"), incoming);
  } finally { f.fx.cleanup(); }
});

for (const interruption of ["metadata", "ref", "hard_after_metadata"] as const) {
  it("m3 interrupted snapshot " + interruption + " retains its fence without deletion authority", async () => {
    const f = await fixture();
    try {
      const head = f.commit("interrupted snapshot"); await f.fetch();
      const hard = new AbortController();
      const injected = new Error("snapshot metadata write interrupted");
      let fired = false, refAttempts = 0;
      const internal = f.cache as unknown as { writeCodeSnapshotMetadata(b: string, value: unknown): Promise<void> };
      const write = internal.writeCodeSnapshotMetadata.bind(f.cache);
      internal.writeCodeSnapshotMetadata = async (b, value) => {
        await write(b, value);
        if (interruption === "metadata") { fired = true; throw injected; }
        if (interruption === "hard_after_metadata") { fired = true; hard.abort(); }
      };
      const spawnReal = realSpawner();
      const spawner: BoundaryProcessSpawner = request => {
        if (request.argv.includes("update-ref") && request.argv.includes("refs/uzi-cross-check/" + run)) {
          refAttempts++;
          if (interruption === "ref") {
            fired = true;
            // A real CAS refusal: the absent ref cannot have this expected old SHA.
            const argv = [...request.argv]; argv[argv.length - 1] = f.base;
            return spawnReal({ ...request, argv });
          }
        }
        return spawnReal(request);
      };
      let result: Awaited<ReturnType<GitCache["pinCodeSnapshot"]>> | undefined;
      let failure: unknown;
      await f.cache.withBoundaryProcessSpawner(spawner, hard.signal,
        () => f.cache.pinCodeSnapshot(f.bare, run, head, f.base))
        .then(value => { result = value; }, error => { failure = error; });
      assert.equal(fired, true);
      assert.equal(result, undefined, "interrupted creation returns no fresh receipt");
      assert.ok(failure instanceof Error);
      if (interruption === "hard_after_metadata") {
        assert.ok(failure instanceof GitBoundaryAbortError);
        assert.equal(failure.checkpointBoundary, "hard");
      } else if (interruption === "metadata") assert.equal(failure, injected);
      assert.equal(failure instanceof CheckpointSoftDeadlineError, false);
      assert.equal(refAttempts, interruption === "ref" ? 1 : 0);
      const dir = path.join(f.bare, "uzi-code-snapshots");
      const metadataPath = path.join(dir, "code-snapshot-" + run + ".json");
      const fence = path.join(dir, "code-snapshot-" + run + ".fence");
      const before = fs.readFileSync(metadataPath, "utf8");
      const metadata = JSON.parse(before);
      assert.equal(metadata.head, head);
      assert.equal(metadata.latestChild, null);
      assert.deepEqual(metadata.readers, []);
      const inode = fs.statSync(fence).ino;
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-cross-check/" + run), "");
      const restarted = new GitCache(f.fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      await assert.rejects(restarted.pinCodeSnapshot(f.bare, run, head, f.base), { code: "EEXIST" });
      await assert.rejects(restarted.deleteCodeSnapshot(f.bare, run, head), /retirement proof required/);
      // Even sealed server evidence alone cannot bypass the interrupted-create fence.
      await assert.rejects(restarted.deleteCodeSnapshot(f.bare, run, head, {
        sealedOutcome: { persisted: true, sealed: true, stage: "code", leadId: run,
          head, pinGeneration: 0, outcome: "failed" },
      }), { code: "EEXIST" });
      assert.equal(fs.readFileSync(metadataPath, "utf8"), before);
      assert.equal(fs.statSync(fence).ino, inode);
      assert.equal(git(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-cross-check/" + run), "");
    } finally { f.fx.cleanup(); }
  });
}

for (const damage of ["foreign_pending", "malformed_pending"] as const) {
  it("m3 " + damage + " refuses matching tracking after soft stop during incoming preservation", async () => {
    const f = await fixture();
    try {
      const ownerRun = damage === "foreign_pending" ? "foreign-run" : run;
      const producing = { ...f.options.context, runId: ownerRun };
      const first = f.commit("pending established first");
      const established = await f.cache.fetchAgentBranch(f.bare, f.clone, branch, ownerRun, { context: producing });
      assert.equal(established.kind, "updated");
      const receipt = JSON.parse(fs.readFileSync(f.receipt, "utf8"));
      // Model an interrupted receipt; malformed pending also carries an unrecognized field.
      fs.writeFileSync(f.receipt, JSON.stringify({ ...receipt, phase: "pending",
        ...(damage === "malformed_pending" ? { extra: true } : {}) }));
      const before = fs.readFileSync(f.receipt);
      const config = fs.readFileSync(path.join(f.bare, "config"));
      const oldMetadata = fs.readdirSync(path.join(f.bare, "uzi-owed")).sort()
        .map(name => [name, fs.readFileSync(path.join(f.bare, "uzi-owed", name))] as const);
      const stagesBefore = git(f.bare, "for-each-ref", "--format=%(refname)", "refs/uzi-incoming/");
      assert.equal(stagesBefore, "");
      const incoming = f.commit("pending preserved incoming");
      const options = { context: { ...f.options.context, generation: 2 } };
      const soft = new AbortController();
      let pinned = 0, trackingWrites = 0, receiptWrites = 0;
      const internal = f.cache as unknown as { writeOwedFile(b: string, n: string, v: unknown): Promise<void> };
      const write = internal.writeOwedFile.bind(f.cache);
      internal.writeOwedFile = async (b, n, v) => {
        await write(b, n, v);
        if (n.startsWith("receipt-")) receiptWrites++;
      };
      let failure: unknown;
      let result: unknown;
      await f.cache.withBoundaryProcessSpawner(realSpawner(args => {
        if (args.includes("update-ref") && args.includes("refs/uzi-owed/" + run + "/" + incoming)) {
          pinned++; soft.abort();
        }
        if (args.includes("update-ref") && args.includes("refs/uzi-runner/" + branch)) trackingWrites++;
      }), new AbortController().signal, () => f.fetch(options), { softSignal: soft.signal })
        .then(value => { result = value; }, error => { failure = error; });
      assert.equal(failure, undefined, "the primary ownership refusal dominates this cooperative stop");
      assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" });
      assert.equal(pinned, 1);
      assert.equal(trackingWrites, 0);
      assert.equal(receiptWrites, 0, "no pending repair or adoption");
      assert.deepEqual(fs.readFileSync(f.receipt), before);
      assert.deepEqual(fs.readFileSync(path.join(f.bare, "config")), config);
      for (const [name, bytes] of oldMetadata) {
        assert.deepEqual(fs.readFileSync(path.join(f.bare, "uzi-owed", name)), bytes);
      }
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
      const stages = git(f.bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/").split("\n");
      assert.equal(stages.length, 1, "exactly one new private stage for this operation");
      assert.ok(stages[0]!.startsWith("refs/uzi-incoming/"));
      assert.equal(stages[0]!.split(" ")[1], incoming);
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + ownerRun + "/" + first), first);
      assert.equal(git(f.bare, "rev-parse", "refs/uzi-owed/" + run + "/" + incoming), incoming);
      const candidates = await f.cache.enumerateOwedCandidates(f.bare, run);
      assert.deepEqual(candidates.find(c => c.sha === incoming)?.contexts, [options.context]);
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, ownerRun, first, 1),
        { kind: "not_owned" });
      assert.deepEqual(await f.cache.committedTrackingOwnership(f.bare, branch, run, incoming, 2),
        { kind: "not_owned" });

      // Exercise the real Runner guard with its cached producing context and the real GitCache.
      const runner = new RunRunner(new WorkerClient("http://unused", "test", "test", nullLogger()), f.cache,
        () => ({ executor: { run: async () => ({ branch }) } }), nullLogger(), 1);
      const subject = runner as unknown as {
        requireTrackingOwned(flight: unknown, bare: string, name: string, expected: string): Promise<string>;
      };
      const flight = { runId: ownerRun, owedContext: Promise.resolve(producing), runLog: nullLogger(),
        preserveRecoveryClone: false, preserveSession: false };
      await assert.rejects(subject.requireTrackingOwned(flight, f.bare, branch, first),
        /tracking preservation refused/);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
      assert.deepEqual(fs.readFileSync(f.receipt), before);
      assert.deepEqual(fs.readFileSync(path.join(f.bare, "config")), config);
    } finally { f.fx.cleanup(); }
  });
}

it("m1 pending ownership remains refused after a stopped unknown-owner preservation", async () => {
  const f = await fixture();
  try {
    const first = f.commit("first"); await f.fetch();
    const receipt = JSON.parse(fs.readFileSync(f.receipt, "utf8"));
    fs.writeFileSync(f.receipt, JSON.stringify({ ...receipt, phase: "pending" }));
    const next = f.commit("pending incoming");
    const stop = new AbortController();
    const result = await f.cache.withBoundaryProcessSpawner(realSpawner(args => {
      if (args.includes("update-ref") && args.some(a => a.startsWith("refs/uzi-owed/"))) stop.abort();
    }), new AbortController().signal, () => f.fetch(), { softSignal: stop.signal });
    assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(git(f.bare, "rev-parse", "refs/uzi-runner/" + branch), first);
    assert.equal(JSON.parse(fs.readFileSync(f.receipt, "utf8")).phase, "pending");
    assert.ok((await f.cache.enumerateOwedCandidates(f.bare, run)).some(c => c.sha === next));
  } finally { f.fx.cleanup(); }
});
