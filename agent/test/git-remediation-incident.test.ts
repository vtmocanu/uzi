import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, formatRetainedRecoveryDiagnostic } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureTrackingOptions } from "./runner-tracking-fixture.js";

const branch = "agent/issue-2512", key = "issue-2512", runId = "run-2512";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
const ident = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
// Do not let the runner's inherited Git repository/config selectors affect the real fixtures.
const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("GIT_")));
Object.assign(env, { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" });
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}

for (const mode of ["incident", "parent", "absent-lineage"] as const) {
  test("incident regression: " + mode, async t => {
    const fx = makeFixture();
    try {
      const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      // Optional feature access is intentional: every graph-construction API/import exists at bb63830d.
      const feature = cache as any;
      const enabled = typeof feature.recordRemediationScan === "function";
      const bare = await cache.ensureClone(fx.originPath);
      fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
      let clone = (await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId)).path;
      let source: { runId: string; clonePath: string; attemptId?: string } = { runId, clonePath: clone };
      const base = git(clone, "rev-parse", "HEAD");
      const journal = () => JSON.parse(git(bare, "config", "uzi-recovery." + branch + ".clone"));
      const pins = () => git(bare, "for-each-ref", "--format=%(refname) %(objectname)",
        "refs/uzi-owed/", "refs/uzi-coverage/", "refs/uzi-recovery-episode/");
      const context = (await fixtureTrackingOptions(cache, bare, branch, runId)).context;
      async function fetch() {
        const options = { context, ...(enabled && source.attemptId ? { producingSource: source as { runId: string; clonePath: string; attemptId: string } } : {}) };
        const result = await cache.fetchAgentBranch(bare, clone, branch, runId, options);
        assert.equal(result.kind, "updated", JSON.stringify(result));
      }
      async function scan(tip: string, flaggedOIDs: string[], origin?: string) {
        if (enabled) await feature.recordRemediationScan(bare, branch, source, { trusted: true, tip, flaggedOIDs, ...(origin ? { origin } : {}) });
        else assert.equal(journal().remediationRecovery, undefined, "baseline journal never gets candidate-only keys");
      }
      async function commit(label: string, parent: string, before?: string) {
        git(clone, "reset", "--hard", parent);
        fs.writeFileSync(path.join(clone, "progress.txt"), label + "\n");
        git(clone, "add", "progress.txt");
        const tree = git(clone, "write-tree");
        let oid = "", nonce = 0;
        do {
          oid = git(clone, ...ident, "commit-tree", tree, "-p", parent, "-m", label + " nonce " + nonce++);
          assert.ok(nonce < 10000, "bounded deterministic ordering search");
        } while (before && oid >= before);
        git(clone, "reset", "--hard", oid);
        await fetch();
        return oid;
      }
      async function capture(roots: string[], current: string) {
        const c = await cache.buildRecoveryCoverage(bare, context as any, roots, current);
        const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
        await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c.sha, current);
        return { coverage: c.sha, iteration: r.attempts };
      }
      await cache.markRecoveryCapture(bare, clone, branch, runId);
      const p = await commit("flagged marker parent", base);
      fs.writeFileSync(path.join(clone, "marker.txt"), "parked work\n");
      git(clone, "add", "marker.txt");
      assert.equal(await cache.commitWipMarker(clone), true);
      const marker = git(clone, "rev-parse", "HEAD"); await fetch();
      assert.equal(git(bare, "rev-parse", marker + "^"), p);
      const first = await capture([marker], marker);
      const peeled = await cache.prepareRecoverySuccessor(bare, branch, key, source, first.iteration, aid(2));
      assert.equal(git(peeled.path, "rev-parse", "HEAD"), p);
      assert.equal(git(peeled.path, "write-tree"), git(bare, "rev-parse", marker + "^{tree}"));
      assert.equal(git(peeled.path, "status", "--porcelain"), "A  marker.txt");
      assert.deepEqual(journal().peeledMarkers, [marker]);
      clone = peeled.path; source = { runId, clonePath: clone, attemptId: aid(2) };
      await fetch();
      await scan(p, [p], mode === "incident" ? "checkpoint-pending" : "pre-exit");
      let current: string;
      let roots: string[];
      let displaced: string[] = [];
      if (mode === "incident") {
        const before: string[] = [];
        let tip = p;
        for (let i = 0; i < 10; i++) {
          tip = await commit("pending progress " + i, tip, i === 0 ? marker : undefined);
          before.push(tip);
        }
        const r1 = await commit("checkpoint authorized rewrite", base);
        const after: string[] = []; tip = r1;
        for (let i = 0; i < 8; i++) {
          tip = await commit("ordinary progress " + i, tip); after.push(tip);
        }
        await scan(tip, [r1], "pre-exit");
        const r2 = await commit("pre-exit authorized rewrite", base);
        await scan(r2, []);
        current = await commit("progress after grant closure", r2);
        roots = [marker, p, ...before, r1, ...after, r2, current];
        displaced = [before.at(-1)!, after.at(-1)!];
        assert.equal(roots.length, 23);
        // Baseline must encounter the ordinary divergent leaf before the marker's ineligible parent.
        assert.ok(before[0]! < marker);
        if (enabled) {
          const evidence = await feature.readRemediationRecovery(bare, branch, source);
          assert.deepEqual(evidence.edges.map((e: any) => [e.from, e.to, e.grant.origin]),
            [[displaced[0], r1, "checkpoint-pending"], [displaced[1], r2, "pre-exit"]]);
          assert.equal(evidence.active, undefined);
        }
      } else {
        current = await commit("authorized replacement excluding marker parent", base);
        await scan(current, []);
        roots = [marker, current];
        displaced = [p];
      }
      assert.equal(await cache.ancestry(bare, p, current), "divergent");
      assert.equal(await cache.ancestry(bare, marker, current), "divergent");
      assert.notEqual(git(bare, "rev-parse", marker + "^{tree}"), git(bare, "rev-parse", current + "^{tree}"));
      if (mode === "absent-lineage") {
        const j = journal(); delete j.peeledMarkers;
        git(bare, "config", "uzi-recovery." + branch + ".clone", JSON.stringify(j));
      }
      const final = await capture(roots, current);
      // The coverage commit really has the exact 23 (or two) intended leaf parents.
      assert.deepEqual(git(bare, "rev-list", "--parents", "-n", "1", final.coverage).split(" ").slice(1).sort(), [...roots].sort());
      const beforeJournal = journal(), beforePins = pins();
      const head = git(clone, "rev-parse", "HEAD");
      const options = enabled ? { admitRemediationSource: async () => true } : undefined;
      if (mode === "absent-lineage") {
        await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, final.iteration, aid(3), options as any),
          error => {
            assert.equal(formatRetainedRecoveryDiagnostic(error, s => s).diagnostic_code,
              "retained_successor_wrapper_lineage_membership");
            t.diagnostic("observed retained_successor_wrapper_lineage_membership");
            return true;
          });
        assert.deepEqual(journal(), beforeJournal); assert.equal(pins(), beforePins);
        assert.equal(git(clone, "rev-parse", "HEAD"), head);
        return;
      }
      let successor;
      try {
        successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, final.iteration, aid(3), options as any);
      } catch (error) {
        const diagnostic = formatRetainedRecoveryDiagnostic(error, s => s);
        t.diagnostic(JSON.stringify(diagnostic));
        assert.equal(diagnostic.diagnostic_code, mode === "incident"
          ? "retained_successor_wrapper_lineage_marker" : "retained_successor_wrapper_lineage_ancestry",
          "failure must be the exact baseline refusal, never an API/parser/graph failure");
        assert.deepEqual(journal(), beforeJournal); assert.equal(pins(), beforePins);
        assert.equal(git(clone, "rev-parse", "HEAD"), head);
        assert.fail("authorized incident graph must produce successor; observed " + diagnostic.diagnostic_code);
      }
      assert.equal(git(successor.path, "rev-parse", "HEAD"), current);
      assert.equal(git(successor.path, "write-tree"), git(bare, "rev-parse", current + "^{tree}"));
      assert.equal(git(successor.path, "status", "--porcelain"), "");
      assert.equal(successor.adoptedSource, current);
      assert.equal(successor.adoptedContentSource, current);
      assert.equal(pins(), beforePins, "all exact held ref/OID pairs survive successor adoption");
      assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + branch), current);
      assert.deepEqual(journal().peeledMarkers, [marker]);
      const excluded = journal().remediationRecovery.excludedRoots;
      for (const oid of [marker, p, ...displaced]) {
        assert.ok(excluded.includes(oid), "excluded root " + oid);
        assert.equal(await cache.ancestry(bare, oid, current), "divergent");
        assert.ok(!git(successor.path, "rev-list", "HEAD").split("\n").includes(oid));
      }
      assert.ok(!git(successor.path, "rev-list", "HEAD").split("\n").includes(final.coverage));
      t.diagnostic("exact successor HEAD/tree, tracking, pins, lineage and excluded ancestry verified");
    } finally { fx.cleanup(); }
  });
}
