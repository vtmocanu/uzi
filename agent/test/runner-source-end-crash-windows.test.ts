import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { it } from "node:test";
import { makeTmp } from "./authority-test-support.js";

// Stage names follow M2. Here "Commit" means only the fake transport's accepted
// hold response. PostgreSQL atomicity/replay/scheduling remain M1 LiveDB proofs.
for (const stage of ["BeforeHoldCommit", "AfterCommitBeforeAck", "AfterAckBeforeEnd"]) {
  it(`runner source end crash window: ${stage}`, t => {
    const root = makeTmp(t);
    const manifest = path.join(root, "manifest.json");
    const cwd = fileURLToPath(new URL("..", import.meta.url));
    // Each child gets one attempt, 45s and bounded output. A failed child cannot
    // supply evidence; the next independent test has its own fixture.
    // A nested --test process must bootstrap its own test runner, rather than
    // inherit the parent runner IPC context without its IPC channel.
    const childEnv = { ...process.env };
    delete childEnv.NODE_TEST_CONTEXT;
    const crashed = spawnSync(process.execPath, ["--import", "tsx", "--test", "--test-isolation=none",
      "--test-name-pattern=source end refusal: early / exact-declined",
      fileURLToPath(new URL("./runner-source-end-refusal-sites.test.ts", import.meta.url))],
    { cwd, encoding: "utf8", timeout: 45_000, maxBuffer: 1024 * 1024,
      env: { ...childEnv, UZI_SOURCE_END_CRASH_STAGE: stage, UZI_SOURCE_END_CRASH_MANIFEST: manifest } });
    assert.ifError(crashed.error);
    assert.equal(crashed.signal, null, crashed.stderr);
    assert.equal(crashed.status, 73, "fixture must reach its crash seam: " + crashed.stdout + crashed.stderr);
    assert.ok(fs.existsSync(manifest), crashed.stdout + crashed.stderr);
    const m = JSON.parse(fs.readFileSync(manifest, "utf8"));
    t.after(() => {
      fs.rmSync(path.dirname(m.originPath), { recursive: true, force: true });
      fs.rmSync(m.homeDir, { recursive: true, force: true });
    });
    assert.equal(m.stage, stage);
    assert.equal(m.starts, 1, "only the original wall flight ran an executor");
    assert.equal(m.reports.length, stage === "BeforeHoldCommit" ? 0 : 1);
    const restarted = spawnSync(process.execPath, ["--import", "tsx",
      fileURLToPath(new URL("./runner-source-end-crash-rediscovery.mts", import.meta.url)), manifest],
    { cwd, env: childEnv, encoding: "utf8", timeout: 45_000, maxBuffer: 1024 * 1024 });
    assert.ifError(restarted.error);
    assert.equal(restarted.signal, null, restarted.stderr);
    assert.equal(restarted.status, 0, restarted.stdout + restarted.stderr);
    const result = JSON.parse(restarted.stdout);
    assert.notEqual(result.pid, m.pid);
    assert.equal(result.protectedSources, 2);
  });
}
