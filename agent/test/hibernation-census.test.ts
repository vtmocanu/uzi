import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHibernationCensus, type HibernationCensusDeps } from "../src/hibernation-census.js";
import type { CensusResult, InventoryEntry } from "../src/hibernation-inventory.js";
import { assertWireEntries, EMPTY_DOCKER, startFakeDocker, type FakeDocker } from "./hibernation-census-wire.js";

// The volume census (PRD #4208 U4b) over real temp directories, with the git and client seams faked: the top-level
// classification table, the walk's bounds, the fingerprint, unsafe names, runner refs, scratch and the Docker view.

const RUN = "99999999-9999-4999-8999-999999999991";
const TIP = "0123456789abcdef0123456789abcdef01234567";
const LAYOUT = { outbox: "outbox", agentHome: "agent-home", jobs: "jobs" };

let dataDir: string;
let outside: string;
let docker: FakeDocker;
const dockers: FakeDocker[] = [];

beforeEach(async () => {
  const base = fs.mkdtempSync(path.join(process.env.TMPDIR ?? os.tmpdir(), "uzi-census-"));
  dataDir = path.join(base, "data");
  outside = path.join(base, "outside");
  fs.mkdirSync(dataDir);
  fs.mkdirSync(outside);
  docker = await startFakeDocker(EMPTY_DOCKER);
  dockers.push(docker);
});
afterEach(async () => {
  await Promise.all(dockers.splice(0).map(d => d.close()));
  fs.rmSync(path.dirname(dataDir), { recursive: true, force: true });
});

const put = (rel: string, content = "x") => {
  fs.mkdirSync(path.dirname(path.join(dataDir, rel)), { recursive: true });
  fs.writeFileSync(path.join(dataDir, rel), content);
};
const dir = (rel: string) => fs.mkdirSync(path.join(dataDir, rel), { recursive: true });

function fakeGit(over: Partial<HibernationCensusDeps["git"]> = {}): HibernationCensusDeps["git"] {
  return {
    discoverEpochRetainedSources: async () => ({
      groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0,
    }),
    inspectRetainedSources: async () => ({ sources: [], partial: false, unreadable: 0, sampledAt: "" }),
    assertNoUnsettledDeletes: () => {},
    proveEpochWorktreeContent: async () => ({ proven: false, trackedPaths: null, head: null, reason: "content_unproven" as const }),
    collectEpochScratchProtectedPaths: async () => ({ state: "read" as const, paths: [] }),
    ...over,
  };
}

const client: HibernationCensusDeps["client"] = {
  getRunOwnership: async () => ({ status: "completed" }) as never,
  listRecoveryHolds: async (runId: string) => ({ run_id: runId, holds: [], run_open_holds: { run_id: runId, count: 0 } }) as never,
};

function build(over: Partial<HibernationCensusDeps> = {}) {
  return createHibernationCensus({
    dataDir, layout: LAYOUT, git: fakeGit(), client, ownership: undefined, dockerHost: docker.host, ...over,
  });
}
async function run(over: Partial<HibernationCensusDeps> = {}): Promise<CensusResult> {
  const result = await build(over).census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assertWireEntries(result.entries);
  return result;
}
const byPath = (result: CensusResult, p: string): InventoryEntry | undefined => result.entries.find(e => e.path === p);

it("an empty volume is complete with no entries", async () => {
  const result = await run();
  assert.deepEqual(result.entries, []);
  assert.equal(result.complete, true);
  assert.match(result.digest, /^[0-9a-f]{64}$/);
  assert.match(result.fingerprint, /^[0-9a-f]{64}$/);
});

it("classifies every top-level entry type; empty stores are disposable", async () => {
  put(".uzi-authority/worker.lock", "");
  put(".uzi-authority/authority.lock", "");
  put(".uzi-epoch.lock", "");
  dir("outbox"); dir("agent-home"); dir("jobs"); dir("lost+found");
  dir("recovery"); dir("recovery-archive");
  const result = await run();
  const want: Array<[string, string, string]> = [
    [".uzi-authority", "runtime_lock", "disposable"], [".uzi-epoch.lock", "runtime_lock", "disposable"],
    ["outbox", "outbox", "disposable"], ["agent-home", "agent_home", "disposable"], ["jobs", "jobs", "disposable"],
    ["lost+found", "lost_found", "disposable"], ["recovery", "journal", "disposable"], ["recovery-archive", "journal", "disposable"],
  ];
  for (const [p, kind, classification] of want) {
    const e = byPath(result, p);
    assert.equal(e?.kind, kind, p);
    assert.equal(e?.classification, classification, p);
  }
  assert.equal(result.entries.length, want.length);
  assert.equal(result.complete, true);
});

it("a non-empty outbox is protected; non-empty agent-home, jobs and lost+found are unknown", async () => {
  put("outbox/run/msg.json"); put("agent-home/.claude/settings.json"); put("jobs/j1/work"); put("lost+found/#123");
  put("recovery/run-1/bundle"); put("recovery-maintenance/x");
  const result = await run();
  const cls = (p: string) => [byPath(result, p)?.kind, byPath(result, p)?.classification, byPath(result, p)?.detail];
  assert.deepEqual(cls("outbox"), ["outbox", "protected", "not_empty"]);
  assert.deepEqual(cls("agent-home"), ["agent_home", "unknown", "not_empty"]);
  assert.deepEqual(cls("jobs"), ["jobs", "unknown", "not_empty"]);
  assert.deepEqual(cls("lost+found"), ["lost_found", "unknown", "not_empty"]);
  assert.deepEqual(cls("recovery"), ["journal", "protected", "not_empty"]);
  assert.deepEqual(cls("recovery-maintenance"), ["journal", "protected", "not_empty"]);
});

it("an unexpected top-level entry is unknown_path and never disposable", async () => {
  put("stray/data"); put("stray-file");
  const result = await run();
  assert.equal(byPath(result, "stray")?.kind, "unknown_path");
  assert.equal(byPath(result, "stray")?.classification, "unknown");
  assert.equal(byPath(result, "stray-file")?.classification, "unknown");
});

it("a symlink is recorded and never followed", async () => {
  fs.writeFileSync(path.join(outside, "secret"), "outside");
  fs.mkdirSync(path.join(outside, "deep"));
  fs.symlinkSync(outside, path.join(dataDir, "jobs"));
  put("outbox/keep"); fs.symlinkSync(outside, path.join(dataDir, "outbox", "link"));
  const seams = build();
  const first = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(byPath(first, "jobs")?.classification, "unknown");
  assert.equal(byPath(first, "jobs")?.detail, "not_directory");
  const before = await seams.fingerprint({ deadlineMs: 60_000 });
  // The target's content is not part of the volume: changing it changes nothing here.
  fs.writeFileSync(path.join(outside, "secret"), "changed, longer");
  fs.writeFileSync(path.join(outside, "deep", "more"), "x");
  const after = await seams.fingerprint({ deadlineMs: 60_000 });
  assert.deepEqual(after, before);
  assert.equal(first.complete, true);
});

it("a node cap makes the census incomplete", async () => {
  for (let i = 0; i < 30; i++) put(`outbox/f${i}`);
  const result = await run({ limits: { maxNodes: 10 } });
  assert.equal(result.complete, false);
  const full = await run();
  assert.equal(full.complete, true);
});

it("the fingerprint changes when a file is rewritten with the same size and a restored mtime (V17)", async () => {
  put("outbox/m.json", "aaaa");
  const file = path.join(dataDir, "outbox/m.json");
  const stamp = 1_700_000_000; // whole seconds: the restored mtime is exact to the nanosecond
  fs.utimesSync(file, stamp, stamp);
  const seams = build();
  const before = await seams.fingerprint({ deadlineMs: 60_000 });
  const again = await seams.fingerprint({ deadlineMs: 60_000 });
  assert.deepEqual(again, before, "an untouched volume is stable");
  const st = fs.statSync(file, { bigint: true });
  fs.writeFileSync(file, "bbbb");
  fs.utimesSync(file, stamp, stamp);
  const after = fs.statSync(file, { bigint: true });
  assert.equal(after.size, st.size);
  assert.equal(after.ino, st.ino);
  assert.equal(after.mtimeNs, st.mtimeNs);
  assert.notEqual(after.ctimeNs, st.ctimeNs);
  const changed = await seams.fingerprint({ deadlineMs: 60_000 });
  assert.notEqual(changed.fingerprint, before.fingerprint);
});

it("the digest is stable for identical entries and changes with the entries", async () => {
  put("stray/a");
  const seams = build();
  const a = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  const b = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(a.digest, b.digest);
  assert.deepEqual(a.entries, b.entries);
  put("stray2/a");
  const c = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.notEqual(c.digest, a.digest);
});

it("a control, bidi or non-UTF-8 name is escaped and unknown", async () => {
  dir("outbox");
  fs.mkdirSync(path.join(dataDir, "bad\u001b[31mname"));
  fs.mkdirSync(path.join(dataDir, "rtl\u202eevil"));
  fs.mkdirSync(Buffer.concat([Buffer.from(dataDir + "/"), Buffer.from([0x66, 0xff, 0x6f])]));
  fs.writeFileSync(path.join(dataDir, "outbox", "in\u0007side"), "x");
  const result = await run();
  const paths = result.entries.map(e => e.path);
  assert.ok(paths.includes("bad%1B[31mname"), JSON.stringify(paths));
  assert.ok(paths.includes("rtl%E2%80%AEevil"), JSON.stringify(paths));
  assert.ok(paths.includes("f%FFo"), JSON.stringify(paths));
  assert.ok(paths.includes("outbox/in%07side"), JSON.stringify(paths));
  for (const e of result.entries.filter(x => x.path.includes("%"))) {
    assert.equal(e.classification, "unknown");
    assert.equal(e.detail, "unsafe_name");
  }
});

it("an entry count over the cap truncates, keeps blockers and is incomplete", async () => {
  for (let i = 0; i < 10_050; i++) fs.mkdirSync(path.join(dataDir, `d${String(i).padStart(5, "0")}`));
  dir("jobs");
  const result = await run();
  assert.equal(result.complete, false);
  assert.ok(result.entries.length <= 10_000);
  assert.ok(result.entries.every(e => e.classification === "unknown"));
});

it("an unsettled runner delete is a protected unsettled_op", async () => {
  dir("runner");
  const result = await run({ git: fakeGit({ assertNoUnsettledDeletes: () => { throw new Error("unsettled"); } }) });
  const e = result.entries.find(x => x.kind === "unsettled_op");
  assert.equal(e?.classification, "protected");
});

it("runner refs bind to their run and tip; a ref with no known run is unknown", async () => {
  const bare = path.join(dataDir, "repos", "o.git");
  fs.mkdirSync(path.join(bare, "refs/uzi-runner/agent"), { recursive: true });
  fs.writeFileSync(path.join(bare, "refs/uzi-runner/agent/issue-7"), `${TIP}\n`);
  fs.writeFileSync(path.join(bare, "refs/uzi-runner/agent/issue-8"), "ref: refs/heads/main\n");
  fs.writeFileSync(path.join(bare, "packed-refs"),
    `# pack-refs\n${"ab".repeat(20)} refs/uzi-runner/agent/issue-9\n${"cd".repeat(20)} refs/heads/main\n`);
  const result = await run({
    git: fakeGit({
      inspectRetainedSources: async () => ({ sources: [], partial: false, unreadable: 0, sampledAt: "" }),
      discoverEpochRetainedSources: async () => ({
        groups: [{ barePath: bare, selector: { runId: RUN, generation: 1, attempt: "a" }, paths: [path.join(dataDir, "runner/r/issue-7.attempt-a")] }],
        inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
      }),
    }),
  });
  // No inspected source carries the branch, so every ref is unbound; add one that does.
  assert.equal(byPath(result, "repos/o.git/refs/uzi-runner/agent/issue-7")?.classification, "unknown");
  const bound = await run({
    git: fakeGit({
      discoverEpochRetainedSources: async () => ({
        groups: [{ barePath: bare, selector: { runId: RUN, generation: 1, attempt: "a" }, paths: [path.join(dataDir, "runner/r/issue-7.attempt-a")] }],
        inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
      }),
      inspectRetainedSources: async () => ({
        sources: [{
          barePath: bare, runId: RUN, branch: "agent/issue-7", key: "k", kind: "attempt", generation: 1, attempt: "a", ledgerState: "live",
          journalState: "plain", markerState: "none", presence: "missing", descriptor: "d".repeat(64), allocatedBytes: null,
          bytesState: "unknown", sources: [], consumers: [], localRecords: [], holds: { state: "read", ids: [] },
          disposition: { kind: "refused", consumers: [] },
        }] as never,
        partial: false, unreadable: 0, sampledAt: "",
      }),
    }),
  });
  const seven = byPath(bound, "repos/o.git/refs/uzi-runner/agent/issue-7");
  assert.equal(seven?.classification, "needs_receipt");
  assert.equal(seven?.run_id, RUN);
  assert.equal(seven?.tip, TIP);
  assert.equal(seven?.content_state, "ref");
  assert.equal(seven?.kind, "runner_ref");
  assert.equal(byPath(bound, "repos/o.git/refs/uzi-runner/agent/issue-8")?.classification, "unknown");
  assert.equal(byPath(bound, "repos/o.git/refs/uzi-runner/agent/issue-9")?.classification, "unknown");
  const bareEntry = byPath(bound, "repos/o.git");
  assert.equal(bareEntry?.kind, "bare_repo");
  assert.equal(bareEntry?.classification, "disposable");
});

it("owed records keep a bare protected", async () => {
  put("repos/o.git/uzi-owed/pin", "x");
  const result = await run();
  assert.equal(byPath(result, "repos/o.git")?.classification, "protected");
});

it("unclaimed runner content is unknown; an empty repo directory is disposable", async () => {
  dir("runner/empty-repo");
  put("runner/r/issue-1/.git/HEAD", "ref: refs/heads/main\n");
  put("runner/r/.uzi-residue-abc/file");
  const result = await run();
  assert.equal(byPath(result, "runner/empty-repo")?.classification, "disposable");
  const clone = byPath(result, "runner/r/issue-1");
  assert.equal(clone?.kind, "canonical_clone");
  assert.equal(clone?.classification, "unknown");
  assert.equal(byPath(result, "runner/r/.uzi-residue-abc")?.kind, "unknown_path");
});

it("a source with a non-uuid run id cannot be a needs_receipt", async () => {
  const clone = path.join(dataDir, "runner/r/issue-1.attempt-a");
  fs.mkdirSync(path.join(clone, ".git"), { recursive: true });
  const result = await run({
    git: fakeGit({
      discoverEpochRetainedSources: async () => ({
        groups: [{ barePath: path.join(dataDir, "repos/o.git"), selector: { runId: "not-a-uuid", generation: 1, attempt: "a" }, paths: [clone] }],
        inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
      }),
      inspectRetainedSources: async () => ({
        sources: [{
          barePath: path.join(dataDir, "repos/o.git"), runId: "not-a-uuid", branch: "agent/issue-1", key: "k", kind: "attempt",
          generation: 1, attempt: "a", ledgerState: "live", journalState: "plain", markerState: "none", presence: "present",
          descriptor: "d".repeat(64), allocatedBytes: 1, bytesState: "measured",
          sources: [{ clonePath: clone, attempt: "a", generation: 1, presence: "present", ledgerState: "live", live: false, allocatedBytes: 1 }],
          consumers: [], localRecords: [], holds: { state: "read", ids: [] }, disposition: { kind: "local_discard" },
        }] as never,
        partial: false, unreadable: 0, sampledAt: "",
      }),
      proveEpochWorktreeContent: async () => ({ proven: true, trackedPaths: new Set<string>(), head: TIP }),
    }),
    ownership: { beginReclaim: () => ({ finish() {} }) as never },
  });
  const e = byPath(result, "runner/r/issue-1.attempt-a");
  assert.equal(e?.classification, "unknown");
  assert.equal(e?.run_id, undefined);
});

it("runner scratch: protected records and foreign names block; ordinary leaves aggregate to one disposable entry", async () => {
  const scratchRoot = path.join(dataDir, "runner/.tmp/uzi-runner");
  put("runner/.tmp/uzi-runner/leaf-a/f"); put("runner/.tmp/uzi-runner/leaf-b/f"); put("runner/.tmp/uzi-runner/leaf-c/f");
  put("runner/.tmp/uzi-worker/live-tmp", "busy");
  put("runner/.tmp/odd/f");
  const result = await run({
    scratchRoot,
    volatileDirs: [path.join(dataDir, "runner/.tmp/uzi-worker")],
    git: fakeGit({ collectEpochScratchProtectedPaths: async () => ({ state: "read", paths: [path.join(scratchRoot, "leaf-b", "x")] }) }),
  });
  assert.equal(byPath(result, "runner/.tmp/uzi-worker")?.classification, "disposable");
  assert.equal(byPath(result, "runner/.tmp/uzi-worker")?.detail, "worker_tmpdir");
  assert.equal(byPath(result, "runner/.tmp/uzi-runner/leaf-b")?.classification, "protected");
  assert.equal(byPath(result, "runner/.tmp/uzi-runner/leaf-b")?.detail, "protected_record");
  const agg = byPath(result, "runner/.tmp/uzi-runner");
  assert.equal(agg?.classification, "disposable");
  assert.equal(agg?.detail, "leaves_2");
  assert.equal(byPath(result, "runner/.tmp/odd")?.classification, "unknown");
  assert.ok(result.entries.every(e => e.kind === "runner_scratch"), JSON.stringify(result.entries));
});

it("the worker's own TMPDIR does not move the fingerprint", async () => {
  put("runner/.tmp/uzi-worker/a", "1");
  const tmp = path.join(dataDir, "runner/.tmp/uzi-worker");
  const seams = build({ volatileDirs: [tmp] });
  const before = await seams.fingerprint({ deadlineMs: 60_000 });
  fs.writeFileSync(path.join(tmp, "b"), "2");
  fs.rmSync(path.join(tmp, "a"));
  assert.deepEqual(await seams.fingerprint({ deadlineMs: 60_000 }), before);
});

it("a volume that changes while the census runs is incomplete", async () => {
  put("outbox/a");
  const result = await run({
    git: fakeGit({
      discoverEpochRetainedSources: async () => {
        fs.writeFileSync(path.join(dataDir, "outbox", "late"), "x");
        return { groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 };
      },
    }),
  });
  assert.equal(result.complete, false);
  assert.ok(result.uncovered.includes("volume_changed_during_census"));
});

it("an incomplete discovery makes the census incomplete", async () => {
  const result = await run({
    git: fakeGit({
      discoverEpochRetainedSources: async () => ({
        groups: [], inventoryComplete: false, physicalPaths: 0, overflow: 0, overflowExact: false, unreadable: 0,
      }),
    }),
  });
  assert.equal(result.complete, false);
});

it("the fingerprint recompute throws at its deadline instead of returning a value", async () => {
  const seams = build({
    git: fakeGit({
      discoverEpochRetainedSources: (opts) => new Promise((_resolve, reject) => {
        opts?.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
      }),
    }),
  });
  dir("runner/r");
  await assert.rejects(seams.fingerprint({ deadlineMs: 50 }));
});

it("reports stopped containers and anonymous volumes from the Docker daemon, read-only", async () => {
  await docker.close();
  docker = await startFakeDocker({
    "/containers/json?all=1": [
      { Id: "c0ffee0000010000000000000000000000000000000000000000000000000000", Names: ["/build-1"], State: "exited" },
      { Id: "abc123abc123abc123", Names: [], State: "running" },
    ],
    "/volumes": { Volumes: [
      { Name: "cache-vol", Labels: null },
      { Name: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Labels: {} },
      { Name: "labelled", Labels: { "com.docker.volume.anonymous": "" } },
    ] },
    "/images/json": [{ Id: "a" }, { Id: "b" }, { Id: "c" }],
    "/system/df": { BuildCache: [{ Size: 1000 }, { Size: 48 }, {}] },
  });
  dockers.push(docker);
  const result = await run();
  assert.deepEqual(result.docker, {
    containers: [
      { id: "c0ffee000001", state: "exited", name: "build-1" },
      { id: "abc123abc123", state: "running", name: "" },
    ],
    volumes: [
      { name: "cache-vol", anonymous: false },
      { name: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", anonymous: true },
      { name: "labelled", anonymous: true },
    ],
    images_count: 3,
    build_cache_bytes: 1048,
  });
  assert.equal(result.complete, true);
  assert.deepEqual([...docker.requests].sort(), [
    "GET /containers/json?all=1", "GET /images/json", "GET /system/df", "GET /volumes",
  ]);
});

it("a failed Docker request makes the census incomplete and reports no Docker block", async () => {
  await docker.close();
  docker = await startFakeDocker({ ...EMPTY_DOCKER, "/system/df": 500 });
  dockers.push(docker);
  const result = await run();
  assert.equal(result.docker, null);
  assert.equal(result.complete, false);
});

it("no Docker endpoint means no Docker block and an incomplete census", async () => {
  const result = await run({ dockerHost: undefined });
  assert.equal(result.docker, null);
  assert.equal(result.complete, false);
});
