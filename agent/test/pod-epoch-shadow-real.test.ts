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
    assert.equal(partial.verdict, "partial");
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
      assert.equal(result.verdict, "partial");
      assert.equal(result.reason, "inventory_partial");
      assert.equal(result.inventory_complete, false);
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
