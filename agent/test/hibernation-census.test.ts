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
  await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
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
  // An empty agent-home is disposable and sorts before every blocker: only blockers-first ranking drops it.
  dir("agent-home");
  const result = await run();
  assert.equal(result.complete, false);
  assert.ok(result.entries.length <= 10_000);
  assert.ok(result.entries.every(e => e.classification === "unknown"));
  assert.equal(byPath(result, "agent-home"), undefined);
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
    workerTmpDirs: [path.join(dataDir, "runner/.tmp/uzi-worker")],
    git: fakeGit({ collectEpochScratchProtectedPaths: async () => ({ state: "read", paths: [path.join(scratchRoot, "leaf-b", "x")] }) }),
  });
  // Fail closed: the worker's own TMPDIR holds content here, so it is unknown rather than disposable.
  assert.equal(byPath(result, "runner/.tmp/uzi-worker")?.classification, "unknown");
  assert.equal(byPath(result, "runner/.tmp/uzi-worker")?.detail, "worker_tmpdir_not_empty");
  assert.equal(byPath(result, "runner/.tmp/uzi-runner/leaf-b")?.classification, "protected");
  assert.equal(byPath(result, "runner/.tmp/uzi-runner/leaf-b")?.detail, "protected_record");
  const agg = byPath(result, "runner/.tmp/uzi-runner");
  assert.equal(agg?.classification, "disposable");
  assert.equal(agg?.detail, "leaves_2");
  assert.equal(byPath(result, "runner/.tmp/odd")?.classification, "unknown");
  assert.ok(result.entries.every(e => e.kind === "runner_scratch"), JSON.stringify(result.entries));
});

it("runner scratch: a run-<uuid> leaf is protected run_scratch and never counted as disposable", async () => {
  const scratchRoot = path.join(dataDir, "runner/.tmp/uzi-runner");
  const runLeaf = "run-0b1f3c2e-4d5a-4e6f-8a7b-9c0d1e2f3a4b";
  put(`runner/.tmp/uzi-runner/${runLeaf}/f`); put("runner/.tmp/uzi-runner/leaf-a/f");
  const result = await run({ scratchRoot, git: fakeGit({ collectEpochScratchProtectedPaths: async () => ({ state: "read", paths: [] }) }) });
  const e = byPath(result, `runner/.tmp/uzi-runner/${runLeaf}`);
  assert.equal(e?.classification, "protected");
  assert.equal(e?.detail, "run_scratch");
  assert.equal(byPath(result, "runner/.tmp/uzi-runner/leaf-a")?.classification, undefined, "non-run leaves stay folded into the aggregate");
  const agg = byPath(result, "runner/.tmp/uzi-runner");
  assert.equal(agg?.classification, "disposable");
  assert.equal(agg?.detail, "leaves_1", "only the non-run leaf is counted disposable");
});

it("runner scratch: near-miss run names (not a lowercase uuid) are ordinary leaves, counted and not protected run_scratch", async () => {
  const scratchRoot = path.join(dataDir, "runner/.tmp/uzi-runner");
  const nearMisses = ["run-NOTAUUID", "run-0B1F3C2E-4D5A-4E6F-8A7B-9C0D1E2F3A4B"];
  for (const n of nearMisses) put(`runner/.tmp/uzi-runner/${n}/f`);
  const result = await run({ scratchRoot, git: fakeGit({ collectEpochScratchProtectedPaths: async () => ({ state: "read", paths: [] }) }) });
  for (const n of nearMisses) {
    assert.notEqual(byPath(result, `runner/.tmp/uzi-runner/${n}`)?.detail, "run_scratch", n);
    assert.equal(byPath(result, `runner/.tmp/uzi-runner/${n}`)?.classification, undefined, `${n} stays folded into the aggregate`);
  }
  const agg = byPath(result, "runner/.tmp/uzi-runner");
  assert.equal(agg?.classification, "disposable");
  assert.equal(agg?.detail, "leaves_2");
});

it("the worker's own TMPDIR is in the fingerprint; empty is disposable", async () => {
  dir("runner/.tmp/uzi-worker");
  const tmp = path.join(dataDir, "runner/.tmp/uzi-worker");
  const seams = build({ workerTmpDirs: [tmp] });
  const first = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(byPath(first, "runner/.tmp/uzi-worker")?.classification, "disposable");
  assert.equal(byPath(first, "runner/.tmp/uzi-worker")?.detail, "worker_tmpdir_empty");
  const before = await seams.fingerprint({ deadlineMs: 60_000 });
  fs.writeFileSync(path.join(tmp, "b"), "2");
  assert.notEqual((await seams.fingerprint({ deadlineMs: 60_000 })).fingerprint, before.fingerprint);
});

it("single layout: the scratch root is the worker TMPDIR; its content is walked, fingerprinted and never 'empty'", async () => {
  const root = path.join(dataDir, "runner/.tmp");
  dir("runner/.tmp");
  const seams = build({ scratchRoot: root, workerTmpDirs: [root] });
  const census = () => seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  const empty = await census();
  assert.equal(byPath(empty, "runner/.tmp")?.detail, "empty");
  const emptyPrint = (await seams.fingerprint({ deadlineMs: 60_000 })).fingerprint;
  put("runner/.tmp/leaf/f", "aaaa");
  put("runner/.tmp/stray", "loose");
  const full = await census();
  assertWireEntries(full.entries);
  assert.notEqual(byPath(full, "runner/.tmp")?.detail, "empty");
  assert.equal(byPath(full, "runner/.tmp")?.detail, "leaves_1");
  assert.equal(byPath(full, "runner/.tmp/stray")?.classification, "unknown", "a loose file in the scratch root blocks");
  assert.equal(full.entries.filter(e => e.path === "runner/.tmp/leaf").length, 0, "an ordinary leaf is aggregated, not listed twice");
  const before = (await seams.fingerprint({ deadlineMs: 60_000 })).fingerprint;
  assert.notEqual(before, emptyPrint);
  const file = path.join(root, "leaf/f");
  const stamp = fs.statSync(file);
  fs.writeFileSync(file, "bbbb");
  fs.utimesSync(file, stamp.atime, stamp.mtime);
  assert.notEqual((await seams.fingerprint({ deadlineMs: 60_000 })).fingerprint, before, "a rewrite under the scratch root moves the fingerprint");
});

it("the shutdown fingerprint walks only the volume: server-side or scope changes leave the digest and fingerprint alone", async () => {
  put("outbox/m.json", "aaaa");
  dir("runner/r");
  let discoveries = 0;
  let scopeOk = true;
  const seams = build({
    git: fakeGit({
      discoverEpochRetainedSources: async () => {
        discoveries++;
        return { groups: [], inventoryComplete: scopeOk, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 };
      },
    }),
  });
  const accepted = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(accepted.complete, true);
  assert.equal(discoveries, 1);
  scopeOk = false; // a rerun census would now read as incomplete with different entries: none of it is on the volume
  const final = await seams.fingerprint({ deadlineMs: 60_000 });
  assert.equal(final.digest, accepted.digest);
  assert.equal(final.fingerprint, accepted.fingerprint);
  assert.equal(discoveries, 1, "no discovery, holds or content proofs at shutdown");
  fs.writeFileSync(path.join(dataDir, "outbox/m.json"), "bbbbb");
  const changed = await seams.fingerprint({ deadlineMs: 60_000 });
  assert.notEqual(changed.fingerprint, accepted.fingerprint);
  assert.equal(changed.digest, accepted.digest);
});

it("the shutdown fingerprint refuses without an accepted census", async () => {
  await assert.rejects(build().fingerprint({ deadlineMs: 60_000 }), /no census/);
});

it("a bare with a recovery ref, a packed recovery ref or a code snapshot is protected", async () => {
  for (const ns of ["uzi-archive/run/g1", "uzi-settle/run/h/source", "uzi-recovery-pin/x", "uzi-owed/run/abc"]) {
    put(`repos/loose-${ns.split("/")[0]}.git/refs/${ns}`, `${TIP}\n`);
  }
  put("repos/packed.git/packed-refs", `# pack-refs\n${TIP} refs/uzi-archive/run/g2\n`);
  put("repos/snap.git/uzi-code-snapshots/snap1", "x");
  dir("repos/plain.git"); dir("repos/emptysnap.git/uzi-code-snapshots");
  put("repos/plain.git/packed-refs", `${TIP} refs/heads/main\n`);
  const result = await run();
  for (const p of ["repos/loose-uzi-archive.git", "repos/loose-uzi-settle.git", "repos/loose-uzi-recovery-pin.git",
    "repos/loose-uzi-owed.git", "repos/packed.git", "repos/snap.git"]) {
    assert.equal(byPath(result, p)?.classification, "protected", p);
  }
  assert.equal(byPath(result, "repos/plain.git")?.classification, "disposable");
  assert.equal(byPath(result, "repos/emptysnap.git")?.classification, "disposable");
});

it("a plain standard bare stays a disposable cache with no extra entries", async () => {
  put("repos/std.git/HEAD", "ref: refs/heads/main\n"); put("repos/std.git/config"); put("repos/std.git/packed-refs", `${TIP} refs/heads/main\n`);
  put("repos/std.git/config.lock"); put("repos/std.git/objects/ab/cd"); put("repos/std.git/refs/heads/main", `${TIP}\n`);
  put("repos/std.git/refs/tags/v1", `${TIP}\n`); put("repos/std.git/refs/uzi-checkpoints/agent/issue-1", `${TIP}\n`);
  dir("repos/std.git/hooks"); dir("repos/std.git/info"); dir("repos/std.git/logs");
  const result = await run();
  assert.equal(byPath(result, "repos/std.git")?.classification, "disposable");
  assert.deepEqual(result.entries.filter(e => e.path.startsWith("repos/std.git/") && e.kind === "unknown_path"), []);
});

it("unknown residue inside a bare is its own blocking unknown entry", async () => {
  put("repos/o.git/HEAD", "ref: refs/heads/main\n");
  put("repos/o.git/private-backup.bin", "secret");
  put("repos/o.git/stray-dir/file", "x");
  put("repos/o.git/refs/weird-ns/x", `${TIP}\n`);
  put("repos/o.git/refs/stray-file", "x");
  const result = await run();
  for (const p of ["repos/o.git/private-backup.bin", "repos/o.git/stray-dir", "repos/o.git/refs/weird-ns", "repos/o.git/refs/stray-file"]) {
    const e = byPath(result, p);
    assert.equal(e?.kind, "unknown_path", p);
    assert.equal(e?.classification, "unknown", p);
  }
  assert.equal(byPath(result, "repos/o.git/HEAD"), undefined);
});

it("issue 4481: refusal records and run-attributed incoming refs protect a bare; flat and empty ones do not", async () => {
  put("repos/rec.git/info/uzi-refusals/refusals-run.json", "{}");
  dir("repos/emptyrec.git/info/uzi-refusals");
  put("repos/adm.git/info/uzi-refusals/admission-run-1.json", "{}");
  for (const n of ["final-run-1.json", "cleanup-run-1.json", "incomplete-run.json", "owedpath-run-1.json", ".tmp-x"]) put(`repos/ev-${n.replace(/\W/g, "")}.git/info/uzi-refusals/${n}`, "{}");
  put("repos/nest.git/refs/uzi-incoming/run/3/refused-abc", `${TIP}\n`);
  put("repos/nestp.git/packed-refs", `${TIP} refs/uzi-incoming/run/3/refused-abc\n`);
  put("repos/flat.git/refs/uzi-incoming/0b1c2d3e-aaaa-bbbb-cccc-444444444444", `${TIP}\n`);
  put("repos/flatp.git/packed-refs", `${TIP} refs/uzi-incoming/0b1c2d3e-aaaa-bbbb-cccc-444444444444\n`);
  const result = await run();
  for (const p of ["repos/rec.git", "repos/nest.git", "repos/nestp.git"]) assert.equal(byPath(result, p)?.classification, "protected", p);
  assert.equal(byPath(result, "repos/rec.git")?.detail, "refusal_records");
  assert.equal(byPath(result, "repos/nest.git")?.detail, "recovery_refs");
  assert.equal(byPath(result, "repos/nestp.git")?.detail, "recovery_refs");
  for (const p of ["repos/ev-finalrun1json.git", "repos/ev-cleanuprun1json.git", "repos/ev-incompleterunjson.git", "repos/ev-owedpathrun1json.git", "repos/ev-tmpx.git"]) assert.equal(byPath(result, p)?.detail, "refusal_records", p);
  for (const p of ["repos/adm.git", "repos/emptyrec.git", "repos/flat.git", "repos/flatp.git"]) assert.equal(byPath(result, p)?.classification, "disposable", p);
});

it("a refs/uzi-coverage ref (loose or packed) keeps a bare protected", async () => {
  put("repos/cov.git/refs/uzi-coverage/run/1/abc", `${TIP}\n`);
  put("repos/covp.git/packed-refs", `${TIP} refs/uzi-coverage/run/1/abc\n`);
  const result = await run();
  assert.equal(byPath(result, "repos/cov.git")?.classification, "protected");
  assert.equal(byPath(result, "repos/covp.git")?.classification, "protected");
  assert.deepEqual(result.entries.filter(e => e.kind === "unknown_path" && e.path.startsWith("repos/cov")), []);
});

it("recovery-episode and cross-check refs keep a bare protected; a worktrees dir and crash-left scratch refs do not hide", async () => {
  put("repos/ep.git/refs/uzi-recovery-episode/run/tip", `${TIP}\n`);
  put("repos/epp.git/packed-refs", `${TIP} refs/uzi-recovery-episode/run/tip\n`);
  put("repos/cc.git/refs/uzi-cross-check/x", `${TIP}\n`);
  put("repos/ccp.git/packed-refs", `${TIP} refs/uzi-cross-check/x\n`);
  put("repos/wt.git/worktrees/w/index", "staged");
  put("repos/sc.git/refs/uzi-workflow-target/u", `${TIP}\n`);
  put("repos/sc.git/refs/uzi-secret-scan-floor/main", `${TIP}\n`);
  const result = await run();
  for (const p of ["repos/ep.git", "repos/epp.git", "repos/cc.git", "repos/ccp.git"]) assert.equal(byPath(result, p)?.classification, "protected", p);
  assert.equal(byPath(result, "repos/wt.git/worktrees")?.classification, "unknown");
  assert.equal(byPath(result, "repos/sc.git")?.classification, "disposable");
  assert.deepEqual(result.entries.filter(e => e.kind === "unknown_path" && e.path.startsWith("repos/sc.git")), []);
});

it(".uzi-authority: lock files are disposable, any store file is unknown", async () => {
  put(".uzi-authority/worker.lock", ""); put(".uzi-authority/authority.lock", "");
  assert.equal(byPath(await run(), ".uzi-authority")?.classification, "disposable");
  put(".uzi-authority/boundaries/abc.json", "{}");
  const result = await run();
  assert.equal(byPath(result, ".uzi-authority")?.classification, "unknown");
  assert.equal(byPath(result, ".uzi-authority")?.detail, "authority_store");
});

it("a top-level unsafe name is listed once", async () => {
  fs.mkdirSync(path.join(dataDir, "bad\u001bname"));
  const result = await run();
  assert.equal(result.entries.filter(e => e.path === "bad%1Bname").length, 1, JSON.stringify(result.entries));
});

it("format and separator characters (soft hyphen, tag characters, Arabic number sign, line separator) are escaped and unknown", async () => {
  for (const name of ["soft\u00adhyphen", "tag\u{e0041}char", "arabic\u0600sign", "ls\u2028sep", "ps\u2029sep", "mongol\u180evs"]) {
    fs.mkdirSync(path.join(dataDir, name));
  }
  const result = await run();
  assert.equal(result.entries.length, 6, JSON.stringify(result.entries));
  for (const e of result.entries) {
    assert.ok(e.path.includes("%"), e.path);
    assert.equal(e.classification, "unknown");
    assert.equal(e.detail, "unsafe_name");
  }
});

it("an unreadable directory makes the census incomplete", { skip: process.getuid?.() === 0 ? "root ignores directory modes" : false }, async () => {
  put("outbox/sealed/f");
  const sealed = path.join(dataDir, "outbox/sealed");
  fs.chmodSync(sealed, 0o000);
  try {
    assert.equal((await run()).complete, false);
  } finally { fs.chmodSync(sealed, 0o755); }
});

it("a walk over its time budget is incomplete", async () => {
  for (let i = 0; i < 600; i++) put(`outbox/f${i}`);
  const result = await run({ limits: { walkMs: -1 } });
  assert.equal(result.complete, false);
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

it("the census throws at its deadline and the fingerprint walk throws when aborted, instead of returning a value", async () => {
  const seams = build({
    git: fakeGit({
      discoverEpochRetainedSources: (opts) => new Promise((_resolve, reject) => {
        opts?.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
      }),
    }),
  });
  dir("runner/r");
  await assert.rejects(seams.census({ signal: new AbortController().signal, deadlineMs: 50 }));
  const accepted = build();
  await accepted.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  await assert.rejects(accepted.fingerprint({ signal: AbortSignal.abort(), deadlineMs: 60_000 }));
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
