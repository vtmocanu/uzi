import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import fs from "node:fs";
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
afterEach(() => { resetResidueQuarantineForTests(); fx.cleanup(); });

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
