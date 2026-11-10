import assert from "node:assert/strict";
import { afterEach, beforeEach, test, mock } from "node:test";
import fs from "node:fs";
import fsp from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { runPodEpochShadow } from "../src/pod-epoch-reclaim.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type RetainedSourceEvidence } from "../src/git.js";
import { resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { ENABLE_DELETION_FOR_TEST } from "./pod-epoch-reclaim-support.js";

// The shadow pass over a REAL GitCache (discovery and inspection not stubbed): it may read a bare
// holding a protected episode and a strict-plain live attempt, and must write nothing.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const evidenceBytes = await readFile(new URL("../../fixtures/pod-epoch/v2/positive.json", import.meta.url));
const evidence = JSON.parse(evidenceBytes.toString());
const PLAIN_RUN = "run-strict";
const PLAIN_BRANCH = "agent/issue-3431";
const PROTECTED_RUN = "epoch-run";
const PROTECTED_BRANCH = "agent/issue-3430";

let fx: Fixture;
let git: GitCache;
let bare: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const attemptAt = (n: number) => formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), 1, n.toString(16).padStart(16, "0"));

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});
afterEach(() => { mock.restoreAll(); resetResidueQuarantineForTests(); fx.cleanup(); });

/** A live attempt seeded through production with its exact plain journal (the strict-plain basis). */
async function seedStrictPlain() {
  const attemptId = attemptAt(1);
  const clone = await git.runnerCloneForBranch(bare, PLAIN_BRANCH, "issue-3431", noProofReseed, PLAIN_RUN, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, PLAIN_BRANCH, PLAIN_RUN, attemptId);
  return clone.path;
}

/** A protected episode: an active journal naming two retained attempt sources of another run. */
function seedProtectedEpisode(): string[] {
  const entries = [attemptAt(10), attemptAt(11)].map(attemptId => {
    const clonePath = git.runnerClonePath(bare, "issue-3430") + ".attempt-" + attemptId;
    const entry = { attemptId, runId: PROTECTED_RUN, clonePath, state: "abandoned" as const };
    gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${PROTECTED_BRANCH}.entry`, JSON.stringify(entry)]);
    fs.mkdirSync(clonePath, { recursive: true });
    fs.writeFileSync(path.join(clonePath, "payload"), "retained");
    return entry;
  });
  const source = (i: number) => ({ runId: PROTECTED_RUN, clonePath: entries[i]!.clonePath, attemptId: entries[i]!.attemptId });
  gitIn(bare, ["config", "--local", `uzi-recovery.${PROTECTED_BRANCH}.clone`,
    JSON.stringify({ ...source(0), retainedSources: [source(1)] })]);
  return entries.map(e => e.clonePath);
}

function listing(dir: string): string[] {
  const out: string[] = [];
  const walk = (current: string) => {
    for (const name of fs.readdirSync(current).sort()) {
      const full = path.join(current, name);
      const st = fs.lstatSync(full);
      out.push(`${path.relative(dir, full)} ${st.isDirectory() ? "d" : st.isSymbolicLink() ? "l" : "f"} ${st.size} ${st.mtimeMs}`);
      if (st.isDirectory()) walk(full);
    }
  };
  walk(dir);
  return out;
}

async function daemon() {
  const requests: string[] = [];
  let events: http.ServerResponse | undefined;
  const server = http.createServer((req, res) => {
    requests.push(req.method + " " + req.url);
    if (req.url?.startsWith("/events")) { events = res; res.writeHead(200); res.flushHeaders(); return; }
    res.end(req.url === "/_ping" ? "OK" : JSON.stringify(
      req.url === "/info" ? { Swarm: { LocalNodeState: "inactive" }, LiveRestoreEnabled: false } :
      req.url === "/volumes" ? { Volumes: [] } : []));
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return {
    host: "tcp://127.0.0.1:" + address.port, requests,
    close: async () => {
      events?.destroy();
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}

test("the shadow pass over a real GitCache reads a protected episode and a strict-plain attempt without writing", async () => {
  const plainPath = await seedStrictPlain();
  const protectedPaths = seedProtectedEpisode();
  const runnerDir = path.dirname(plainPath);
  const snapshot = () => ({
    config: gitIn(bare, ["config", "--local", "--list"]),
    // The journal, ledger and descriptor values, each read by its own key family.
    records: gitIn(bare, ["config", "--local", "--get-regexp", "^uzi-"]),
    refs: gitIn(bare, ["for-each-ref"]),
    configFile: fs.readFileSync(path.join(bare, "config"), "utf8"),
    runner: listing(runnerDir),
  });
  assert.ok(protectedPaths.every(p => fs.existsSync(p)));
  const before = snapshot();

  const d = await daemon();
  const seen: RetainedSourceEvidence[] = [];
  let handovers = 0;
  try {
    const parent = new AbortController();
    const ownership: PodEpochOwnership = {
      mode: "exclusive", consume: async () => { throw new Error("consume already performed"); },
      matchesEntry: () => true, reacquireShared: async () => { handovers++; },
      close: () => { throw new Error("must retain ownership"); },
    };
    const result = (await runPodEpochShadow({
      bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
      ownership, consumption: { result: "fresh", mayEvaluate: true }, signal: parent.signal, dockerHost: d.host,
      readEvidence: async () => evidenceBytes, kernelRelease: () => "6.1.0-test",
      client: { listRecoveryHolds: async run_id => ({ run_id, holds: [] }), getRunOwnership: async () => ({ status: "completed" }) },
      git: {
        discoverEpochRetainedSources: opts => git.discoverEpochRetainedSources(opts),
        inspectRetainedSources: async opts => {
          const r = await git.inspectRetainedSources(opts);
          seen.push(...r.sources);
          return r;
        },
      },
    }))!;

    const kinds = seen.map(s => `${s.runId}:${s.kind}`).sort();
    assert.ok(kinds.includes(`${PLAIN_RUN}:attempt`), kinds.join());
    assert.ok(seen.some(s => s.runId === PROTECTED_RUN && s.kind === "protected" && s.journalState === "episode"), kinds.join());
    const plain = seen.find(s => s.runId === PLAIN_RUN)!;
    assert.deepEqual(plain.epochSettlement, { basis: "strict_plain", disposition: { kind: "local_discard" } });
    assert.equal(result.by_basis.strict_plain, 1, "the live attempt is reported under the strict-plain basis");
    assert.deepEqual(result.partial_causes, {});
    assert.equal(result.verdict, "eligible");
    assert.equal(result.authority.result, "attested");
    assert.equal(result.by_basis.discard, 2, "both protected-episode paths are settled-hold discard candidates");
    assert.equal(result.would_delete, 3, "all three examined paths are present candidates");
    assert.equal(result.would_settle, 0);
    assert.equal(result.kept, 0);
    assert.deepEqual(result.list.map(x => [x.run_id, x.basis, x.presence]).sort(), [
      [PLAIN_RUN, "strict_plain", "present"], [PROTECTED_RUN, "discard", "present"], [PROTECTED_RUN, "discard", "present"],
    ].sort());
    assert.ok(result.examined >= 3, "the strict-plain clone and both protected paths were examined");
    assert.equal(handovers, 1);
    assert.ok(d.requests.every(r => r.startsWith("GET ")), d.requests.join());
  } finally { await d.close(); }

  assert.deepEqual(snapshot(), before, "the pass wrote nothing to the bare config, refs or clone directories");
  assert.ok(protectedPaths.every(p => fs.existsSync(p)) && fs.existsSync(plainPath));
});


test("#3775 separate shadow passes on one real GitCache do not share diagnostics", async () => {
  await seedStrictPlain();
  const d = await daemon();
  let capped = true;
  const pass = () => runPodEpochShadow({
    bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
    ownership: { mode: "exclusive", consume: async () => { throw new Error("already consumed"); },
      matchesEntry: () => true, reacquireShared: async () => {}, close: () => {} },
    consumption: { result: "fresh", mayEvaluate: true }, signal: new AbortController().signal,
    dockerHost: d.host, readEvidence: async () => evidenceBytes, kernelRelease: () => "6.1.0-test",
    client: { getRunOwnership: async () => ({ status: "completed" }),
      listRecoveryHolds: async run_id => ({ run_id, holds: [] }) },
    git: { discoverEpochRetainedSources: opts => git.discoverEpochRetainedSources(opts),
      inspectRetainedSources: opts => git.inspectRetainedSources({ ...opts,
        ...(capped ? { measureCaps: { maxEntries: 1 } } : {}) }) },
  });
  try {
    const partial = (await pass())!;
    const saved = JSON.stringify(partial.partial_causes);
    assert.deepEqual(partial.partial_causes, { measurement_entry_cap: 1 });
    // The cap is confined to the one tree: the pass stays eligible and only that tree is kept.
    assert.equal(partial.verdict, "eligible");
    assert.equal(partial.reason, "shadow_complete");
    assert.equal(partial.kept, 1);
    assert.equal(partial.would_delete, 0);
    assert.deepEqual(partial.list, []);
    capped = false;
    const complete = (await pass())!;
    assert.deepEqual(complete.partial_causes, {});
    assert.equal(complete.verdict, "eligible");
    assert.equal(complete.authority.result, "attested");
    assert.equal(complete.would_delete, 1);
    assert.equal(JSON.stringify(partial.partial_causes), saved);
  } finally { await d.close(); }
});

for (const mode of ["entries", "opendir", "ownership", "holds", "multiple"] as const) {
  test("#3775 real shadow partial causes: " + mode, async () => {
    const plainPath = await seedStrictPlain();
    const protectedPaths = seedProtectedEpisode();
    const d = await daemon();
    const holdsCalls: string[] = [];
    const snapshots: Array<{ run: string; lifecycle: unknown; holds: unknown }> = [];
    const original = fsp.opendir;
    let hits = 0;
    if (mode === "opendir" || mode === "multiple") {
      mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
        if (args[0] === plainPath) { hits++; throw Object.assign(new Error("private"), { code: "EACCES" }); }
        return original(...args);
      });
    }
    try {
      const result = (await runPodEpochShadow({
        bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
        ownership: { mode: "exclusive", consume: async () => { throw new Error("already consumed"); },
          matchesEntry: () => true, reacquireShared: async () => {}, close: () => {} },
        consumption: { result: "fresh", mayEvaluate: true }, signal: new AbortController().signal,
        dockerHost: d.host, readEvidence: async () => evidenceBytes, kernelRelease: () => "6.1.0-test",
        client: {
          getRunOwnership: async run => {
            if (run === PLAIN_RUN && mode === "ownership") throw { status: 503 };
            return { status: "completed" };
          },
          listRecoveryHolds: async run_id => {
            holdsCalls.push(run_id);
            if (run_id === PLAIN_RUN && (mode === "holds" || mode === "multiple")) throw { name: "TimeoutError" };
            return { run_id, holds: [] };
          },
        },
        git: {
          discoverEpochRetainedSources: opts => git.discoverEpochRetainedSources(opts),
          inspectRetainedSources: opts => {
            snapshots.push({ run: opts.selector!.runId, lifecycle: opts.lifecycle, holds: opts.holds });
            return git.inspectRetainedSources({ ...opts,
              ...(mode === "entries" && opts.selector!.runId === PLAIN_RUN ? { measureCaps: { maxEntries: 1 } } : {}) });
          },
        },
      }))!;
      // A cap or a permission stop confined to one tree no longer makes the inventory partial.
      const localOnly = mode === "entries" || mode === "opendir";
      assert.equal(result.verdict, localOnly ? "eligible" : "partial");
      assert.equal(result.reason, localOnly ? "shadow_complete" : "inventory_partial");
      assert.equal(result.inventory_complete, localOnly);
      assert.equal(result.authority.result, "attested");
      assert.deepEqual(result.partial_causes, mode === "entries" ? { measurement_entry_cap: 1 } :
        mode === "opendir" ? { measurement_filesystem_error: 1 } :
        mode === "ownership" ? { ownership_read_http_5xx: 1 } :
        mode === "holds" ? { holds_read_timeout: 1 } : { holds_read_timeout: 1, measurement_filesystem_error: 1 });
      assert.deepEqual(result.list.map(c => [c.run_id, c.path, c.presence, c.basis]).sort(),
        protectedPaths.map(p => [PROTECTED_RUN, path.basename(p), "present", "discard"]).sort());
      assert.equal(result.examined, 3);
      assert.equal(result.would_delete, 2);
      assert.equal(result.would_settle, 0);
      assert.equal(result.kept, 1);
      assert.equal(result.by_basis.discard, 2);
      assert.equal(result.by_basis.strict_plain, 0);
      assert.equal(result.list_overflow, 0);
      assert.equal(result.inventory_overflow, 0);
      assert.equal(result.bytes_state, "partial");
      if (mode === "ownership") {
        assert.deepEqual(holdsCalls, [PROTECTED_RUN]);
        assert.deepEqual(snapshots.find(x => x.run === PLAIN_RUN), {
          run: PLAIN_RUN, lifecycle: { state: "unknown" }, holds: { state: "unknown" },
        });
      }
      if (mode === "holds") assert.deepEqual(snapshots.find(x => x.run === PLAIN_RUN)?.lifecycle,
        { state: "terminal", status: "completed" });
      if (mode === "opendir" || mode === "multiple") assert.equal(hits, 1);
    } finally { await d.close(); }
  });
}

// ---- #3851: a stop confined to one retained tree keeps that tree and nothing else ----
const RUN_A = "run-a";
const RUN_B = "run-b";
type InspectOptions = Parameters<GitCache["inspectRetainedSources"]>[0];
type InspectResult = Awaited<ReturnType<GitCache["inspectRetainedSources"]>>;
type ShadowOptions = Parameters<typeof runPodEpochShadow>[0];
const codedError = (code: string) => Object.assign(new Error("private"), { code });

/** An abandoned ledger attempt (no journal names it): a `discard`-basis candidate once its run is terminal. */
function seedAbandoned(runId: string, branch: string, key: string, n: number, nested = false): string {
  const attemptId = attemptAt(n);
  const clonePath = git.runnerClonePath(bare, key) + ".attempt-" + attemptId;
  gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${branch}.entry`,
    JSON.stringify({ attemptId, runId, clonePath, state: "abandoned" })]);
  fs.mkdirSync(clonePath, { recursive: true });
  fs.writeFileSync(path.join(clonePath, "payload"), "retained");
  if (nested) {
    fs.mkdirSync(path.join(clonePath, "sub"));
    fs.writeFileSync(path.join(clonePath, "sub", "inner"), "retained");
  }
  return clonePath;
}
/** T1 (incomplete) and T2 are attempts of run A under different selectors; T3 belongs to run B. */
function seedThree() {
  return {
    t1: seedAbandoned(RUN_A, PROTECTED_BRANCH, "issue-3430", 1, true),
    t2: seedAbandoned(RUN_A, PROTECTED_BRANCH, "issue-3430", 2),
    t3: seedAbandoned(RUN_B, "agent/issue-3432", "issue-3432", 3),
  };
}

/** The deletion collaborators of a pass that must never reach them: every call is recorded and fails. */
function deletionSpies() {
  const spies = { begins: 0, reserves: 0, proofs: 0, discards: 0 };
  const deletion = {
    enableForTest: ENABLE_DELETION_FOR_TEST,
    authority: { withTransaction: async () => { spies.reserves++; throw new Error("no transaction"); }, active: () => undefined,
      endSourceUse: async () => { throw new Error("no source use"); } } as never,
    reserve: (async () => { spies.reserves++; return undefined; }) as never,
    quiesce: async () => true,
    git: {
      assertNoUnsettledDeletes: () => {},
      readEpochJournalVector: async () => [],
      createEpochDiscardProof: () => { spies.proofs++; throw new Error("no proof"); },
      discardStoppedSource: async () => { spies.discards++; throw new Error("no discard"); },
    },
  } satisfies NonNullable<ShadowOptions["deletion"]>;
  return { spies, deletion };
}

async function shadowPass(tune: {
  inspect?: (opts: InspectOptions, real: (o?: InspectOptions) => Promise<InspectResult>) => Promise<InspectResult>;
  deletion?: ReturnType<typeof deletionSpies>;
} = {}) {
  const d = await daemon();
  const seen: RetainedSourceEvidence[] = [];
  try {
    const options: ShadowOptions = {
      bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
      ownership: { mode: "exclusive", consume: async () => { throw new Error("already consumed"); },
        matchesEntry: () => true, reacquireShared: async () => {}, close: () => {},
        ...(tune.deletion ? { beginReclaim: () => { tune.deletion!.spies.begins++; throw new Error("no reclaim scope"); } } : {}) } as PodEpochOwnership,
      consumption: { result: "fresh", mayEvaluate: true }, signal: new AbortController().signal,
      dockerHost: d.host, readEvidence: async () => evidenceBytes, kernelRelease: () => "6.1.0-test",
      client: { getRunOwnership: async () => ({ status: "completed" }), listRecoveryHolds: async run_id => ({ run_id, holds: [] }) },
      git: {
        discoverEpochRetainedSources: opts => git.discoverEpochRetainedSources(opts),
        inspectRetainedSources: async opts => {
          const real = async (o: InspectOptions = opts) => {
            const r = await git.inspectRetainedSources(o);
            seen.push(...r.sources);
            return r;
          };
          return tune.inspect ? tune.inspect(opts, real) : real();
        },
      },
      ...(tune.deletion ? { deletion: tune.deletion.deletion } : {}),
    };
    return { result: (await runPodEpochShadow(options))!, seen };
  } finally { await d.close(); }
}
const names = (list: Array<{ path: string }>) => list.map(c => c.path).sort();
const baseNames = (...paths: string[]) => paths.map(p => path.basename(p)).sort();
const partFor = (seen: RetainedSourceEvidence[], clonePath: string) => seen.flatMap(s => s.sources).find(p => p.clonePath === clonePath)!;

/** Make exactly the given tree's measurement stop locally, without touching the others. */
type ShortKind = "chmod" | "entries" | "depth" | "pending";
const shortKinds = { entries: ["entry_cap", { maxEntries: 1 }], depth: ["depth_cap", { maxDepth: 0 }], pending: ["pending_cap", { maxPending: 0 }] } as const;
const shortCause = { chmod: "measurement_filesystem_error", entries: "measurement_entry_cap", depth: "measurement_depth_cap", pending: "measurement_pending_cap" } as const;
function capOnly(kind: ShortKind, attempt: string) {
  return async (opts: InspectOptions, real: (o?: InspectOptions) => Promise<InspectResult>) =>
    real(kind !== "chmod" && opts.selector!.attempt === attempt ? { ...opts, measureCaps: shortKinds[kind][1] } : opts);
}

test("#3851 control: without a shortfall all three trees are candidates", async () => {
  const { t1, t2, t3 } = seedThree();
  const { result } = await shadowPass();
  assert.equal(result.verdict, "eligible");
  assert.deepEqual(result.partial_causes, {});
  assert.equal(result.bytes_state, "measured");
  assert.deepEqual(names(result.list), baseNames(t1, t2, t3));
});

for (const kind of ["chmod", "entries", "depth", "pending"] as const) {
  test("#3851 a tree-local " + kind + " shortfall keeps only that tree and the pass stays eligible", async t => {
    if (kind === "chmod" && process.getuid?.() === 0) { t.skip("root bypasses directory permissions"); return; }
    const { t1, t2, t3 } = seedThree();
    const locked = path.join(t1, "sub");
    if (kind === "chmod") fs.chmodSync(locked, 0o000);
    try {
      const { result, seen } = await shadowPass({ inspect: capOnly(kind, attemptAt(1)) });
      assert.equal(result.verdict, "eligible");
      assert.equal(result.reason, "shadow_complete");
      assert.equal(result.inventory_complete, true);
      assert.equal(result.authority.result, "attested");
      assert.equal(result.bytes_state, "partial");
      assert.deepEqual(result.partial_causes, { [shortCause[kind]]: 1 });
      assert.equal(result.examined, 3);
      assert.equal(result.kept, 1);
      assert.equal(result.would_delete, 2);
      assert.deepEqual(names(result.list), baseNames(t2, t3));
      const short = partFor(seen, t1);
      assert.equal(short.allocatedBytes, null);
      assert.equal(short.measurementShortfall, kind === "chmod" ? "eacces" : shortKinds[kind][0]);
      assert.equal(seen.find(s => s.sources[0]!.clonePath === t1)!.bytesState, "unknown");
      for (const other of [t2, t3]) {
        assert.ok(partFor(seen, other).allocatedBytes! > 0);
        assert.ok(!("measurementShortfall" in partFor(seen, other)));
      }
    } finally { if (kind === "chmod") fs.chmodSync(locked, 0o700); }
  });
}

/** An active journal on an unrelated branch names a tree: a per-record blocker for that tree only. */
function nameTree(branch: string, clonePath: string, attemptId: string) {
  gitIn(bare, ["config", "--local", `uzi-recovery.${branch}.clone`, JSON.stringify({ runId: "run-other", clonePath, attemptId })]);
}
test("#3851 consumers: a tree-scoped blocker is per tree, a run-scoped local record blocks the run's trees", async () => {
  const { t1, t2, t3 } = seedThree();
  const cap = capOnly("entries", attemptOf(t1));
  // (i) a journal naming only T1 (named_by_journal on T1's record) leaves T2 and T3 candidates.
  nameTree("agent/issue-9001", t1, attemptOf(t1));
  let { result } = await shadowPass({ inspect: cap });
  assert.equal(result.verdict, "eligible");
  assert.deepEqual(names(result.list), baseNames(t2, t3));
  // (ii) a consumer that also names T2 blocks T2.
  nameTree("agent/issue-9002", t2, attemptOf(t2));
  ({ result } = await shadowPass({ inspect: cap }));
  assert.equal(result.verdict, "eligible");
  assert.deepEqual(names(result.list), baseNames(t3));
  assert.equal(result.kept, 2);
  // (iii) a run-scoped local custody record for run A blocks T1 and T2 while T3 stays a candidate.
  gitIn(bare, ["config", "--local", "uzi-recovery.agent/issue-9001.clone", ""]);
  gitIn(bare, ["config", "--local", "uzi-recovery.agent/issue-9002.clone", ""]);
  fs.mkdirSync(path.join(git.recoveryRoot, RUN_A), { recursive: true });
  fs.writeFileSync(path.join(git.recoveryRoot, RUN_A, "g1.json"), JSON.stringify({ generation: 1 }));
  ({ result } = await shadowPass({ inspect: cap }));
  assert.equal(result.verdict, "eligible");
  assert.deepEqual(names(result.list), baseNames(t3));
  assert.equal(result.kept, 2);
});
const attemptOf = (clonePath: string) => clonePath.slice(clonePath.lastIndexOf(".attempt-") + ".attempt-".length);

test("#3851 a multi-part source with one locally short present part and one missing part has no candidate", async () => {
  const [present, missing] = seedProtectedEpisode();
  fs.rmSync(missing!, { recursive: true });
  const control = await shadowPass();
  assert.deepEqual(control.result.list.map(c => [c.path, c.presence]).sort(),
    [[path.basename(present!), "present"], [path.basename(missing!), "missing"]].sort());
  const { result, seen } = await shadowPass({
    inspect: (opts, real) => real(opts.selector!.runId === PROTECTED_RUN ? { ...opts, measureCaps: { maxEntries: 0 } } : opts),
  });
  assert.equal(result.verdict, "eligible");
  assert.equal(partFor(seen, present!).measurementShortfall, "entry_cap");
  assert.equal(seen.find(s => s.runId === PROTECTED_RUN)!.bytesState, "partial");
  assert.deepEqual(result.list, []);
  assert.equal(result.would_delete, 0);
  assert.equal(result.kept, 2);
});

for (const mode of ["opendir-eio", "opendir-emfile", "deadline", "binding", "root-eacces"] as const) {
  test("#3851 a pass-wide " + mode + " failure in another tree still makes the pass partial and mutates nothing", async () => {
    const { t3 } = seedThree();
    const open = fsp.opendir, lstat = fsp.lstat;
    let clock = 0, measuring = false;
    mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (args[0] === t3 && mode === "opendir-eio") throw codedError("EIO");
      if (args[0] === t3 && mode === "opendir-emfile") throw codedError("EMFILE");
      const handle = await open(...args);
      if (args[0] === t3 && mode === "deadline") clock = 100;
      return handle;
    });
    mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
      if (mode === "root-eacces" && measuring && args[0] === t3) throw codedError("EACCES");
      return lstat(...args);
    });
    const deletion = deletionSpies();
    const { result } = await shadowPass({
      deletion,
      inspect: (opts, real) => {
        if (opts.selector!.attempt === attemptAt(1)) return real({ ...opts, measureCaps: { maxEntries: 1 } });
        if (opts.selector!.attempt !== attemptAt(3)) return real(opts);
        if (mode === "deadline") return real({ ...opts, now: () => clock, budgetMs: 100 });
        return real({ ...opts, beforeTree: p => {
          opts.beforeTree!(p);
          if (mode === "binding") throw new Error("binding failed");
          measuring = true;
        } });
      },
    });
    assert.equal(result.verdict, "partial", mode);
    assert.equal(result.reason, "inventory_partial", mode);
    assert.equal(result.inventory_complete, false, mode);
    const cause = mode === "deadline" ? "measurement_deadline" : mode === "binding" ? "tree_binding_failed" : "measurement_filesystem_error";
    assert.equal(result.partial_causes[cause], 1, mode + " is the pass-wide cause");
    assert.equal(result.partial_causes.measurement_entry_cap, 1, "the local shortfall is still only diagnostic");
    assert.deepEqual(deletion.spies, { begins: 0, reserves: 0, proofs: 0, discards: 0 }, mode);
  });
}

test("#3851 the verdict and candidates do not depend on the inspection's diagnostic counters", async () => {
  seedThree();
  const cap = capOnly("entries", attemptOf(git.runnerClonePath(bare, "issue-3430") + ".attempt-" + attemptAt(1)));
  const plain = await shadowPass({ inspect: cap });
  const stripped = await shadowPass({ inspect: async (opts, real) => { const r = await cap(opts, real); delete (r as { partialCauses?: unknown }).partialCauses; return r; } });
  const forged = await shadowPass({ inspect: async (opts, real) => ({ ...await cap(opts, real), partialCauses: { measurement_entry_cap: 9, inspection_budget: 4 } }) });
  for (const other of [stripped, forged]) {
    assert.equal(other.result.verdict, plain.result.verdict);
    assert.equal(other.result.reason, plain.result.reason);
    assert.deepEqual(other.result.list, plain.result.list);
    assert.equal(other.result.would_delete, plain.result.would_delete);
    assert.equal(other.result.kept, plain.result.kept);
  }
  assert.equal(plain.result.verdict, "eligible");
  assert.equal(plain.result.would_delete, 2);
});
