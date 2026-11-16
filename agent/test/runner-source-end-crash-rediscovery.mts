// Read by an actual fresh process after the runner fixture exits abruptly.
import assert from "node:assert/strict";
import fs from "node:fs";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { GitCache } from "../src/git.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";

const m = JSON.parse(fs.readFileSync(process.argv[2]!, "utf8"));
assert.notEqual(process.pid, m.pid);
const rt = new AuthorityRuntime({
  lifetime: await acquireWorkerLifetime(m.dataDir, testLockOptions()),
  workerToken: m.token, timeoutMs: 5000,
});
const git = new GitCache(m.dataDir, nullLogger(), undefined, testGitCacheOptions({
  authority: rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
}));
const found = await git.discoverRetainedRecovery(m.originPath,
  `agent/issue-${m.iid}`, `issue-${m.iid}`, m.runId);
assert.deepEqual(found, m.retained, "fresh discovery preserves the real wall journal");
assert.equal(found?.journal.memoryPark, undefined, "wall park is not a memory receipt");
assert.equal(m.sources.length, 2);
for (const [i, source] of m.sources.entries()) {
  const before = await rt.peekSlot(source);
  assert.deepEqual(before, m.slots[i]);
  assert.equal(before?.state, "active");
  assert.equal(await rt.sourceEndDeclined(source, new Set()), false, "no inherited witness");
  assert.equal((await rt.classifySourceAdmission(source, new Set())).kind, "foreign_active");
  await assert.rejects(rt.bindSource(source), /active|foreign|held/i);
  assert.deepEqual((await rt.endSourceUse([source])).ended, []);
  assert.deepEqual(await rt.peekSlot(source), before, "old active epoch is protected");
}
process.stdout.write(JSON.stringify({ pid: process.pid, protectedSources: 2 }) + "\n");
