import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { formatAttemptId, attemptClonePath } from "../src/attempt-path.js";
import { hasRetainedHomeMarker, removeRetainedHomeMarker, writeRetainedHomeMarker } from "../src/retained-homes.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3644 (plan 3.12, audit finding): the orphan-marker finisher must not remove run R's retained-home marker because an
// UNRELATED bare holds an ordinary `retired` ledger entry of another attempt X of R. The marker records the bare and the
// predecessor attempt it belongs to, and the discard asks for exactly that binding.

const ENV = (() => {
  const env: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(process.env)) if (!k.startsWith("GIT_")) env[k] = v;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
})();
const RUN = "11111111-2222-4333-8444-555555555555";
const BRANCH = "agent/issue-3644";
const X_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const OTHER_ID = formatAttemptId(new Date("2026-11-05T12:12:12Z"), 1, "aaaaaaaaaaaaaaaa");

let fx: Fixture;
let cache: GitCache;
let bare2: string;
let dataDir: string;

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare2 = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare2), 0o700);
  fs.chmodSync(bare2, 0o700);
  dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "retained-binding-"));
  // An ordinary retired attempt X of run R in THIS bare: no restore ever ran here.
  const clonePath = attemptClonePath(cache.runnerClonePath(bare2, "issue-3644"), X_ID);
  execFileSync("git", ["-C", bare2, "config", "--add", `uzi-attempts.${BRANCH}.entry`,
    JSON.stringify({ attemptId: X_ID, runId: RUN, clonePath, state: "retired" })], { env: ENV });
});
afterEach(() => { fx.cleanup(); fs.rmSync(dataDir, { recursive: true, force: true }); });

const marker = (barePath: string, attemptId: string) =>
  ({ version: 1 as const, runId: RUN, predecessorGeneration: 1, predecessorAttemptId: attemptId, barePath });
const retry = (attempt: string) => cache.discardStoppedSource({
  selector: { runId: RUN, generation: 1, attempt }, confirm: "0".repeat(64), holds: { state: "read", holds: [] },
  lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, quiesce: async () => true, barePath: bare2,
  hasRetainedHomeMarker: (runId, binding) => hasRetainedHomeMarker(dataDir, runId, binding),
  removeRetainedHomeMarker: (runId) => removeRetainedHomeMarker(dataDir, runId),
});

describe("the orphan marker finisher requires the marker to name this bare and the selector's attempt", () => {
  it("the auditor's V1 probe: a marker of another bare is neither evidence nor removed", async () => {
    await writeRetainedHomeMarker(dataDir, marker("/somewhere/else/bare.git", X_ID));
    const r = await retry(X_ID);
    assert.equal(r.kind === "refused" && r.reason, "not_found", JSON.stringify(r));
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true, "the marker is untouched");
  });

  it("a marker of this bare but another predecessor attempt is not evidence either", async () => {
    await writeRetainedHomeMarker(dataDir, marker(bare2, OTHER_ID));
    const r = await retry(X_ID);
    assert.equal(r.kind === "refused" && r.reason, "not_found", JSON.stringify(r));
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
  });

  it("a malformed marker is never removed by the finisher", async () => {
    fs.mkdirSync(path.join(dataDir, "retained-homes"), { mode: 0o700 });
    fs.writeFileSync(path.join(dataDir, "retained-homes", `${RUN}.json`), "{", { mode: 0o600 });
    const r = await retry(X_ID);
    assert.equal(r.kind === "refused" && r.reason, "not_found", JSON.stringify(r));
    assert.equal(fs.existsSync(path.join(dataDir, "retained-homes", `${RUN}.json`)), true);
  });

  it("control: the marker recording this bare and the selector's attempt is finished and removed", async () => {
    await writeRetainedHomeMarker(dataDir, marker(bare2, X_ID));
    const r = await retry(X_ID);
    assert.equal(r.kind, "done", JSON.stringify(r));
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), false);
  });
});
