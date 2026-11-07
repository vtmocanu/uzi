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

// The restore-aware exit of a plain P record: a newer same-run ledger entry in this bare is ordinary history unless the run's
// marker records THIS bare and P's attempt (planRestoreExit's binding).
describe("planRestoreExit: a newer same-run ledger entry plus a marker of another bare or attempt is not restore evidence", () => {
  const S2_ID = formatAttemptId(new Date("2026-11-05T12:12:12Z"), 2, "cccccccccccccccc");
  let pPath: string;
  const ledger = (): Array<{ attemptId: string; state: string }> =>
    execFileSync("git", ["-C", bare2, "config", "--get-all", `uzi-attempts.${BRANCH}.entry`], { env: ENV, encoding: "utf8" })
      .split("\n").filter(Boolean).map((l) => JSON.parse(l));
  beforeEach(() => {
    const canonical = cache.runnerClonePath(bare2, "issue-3644");
    pPath = attemptClonePath(canonical, X_ID);
    const git = (...a: string[]) => execFileSync("git", ["-C", bare2, ...a], { env: ENV });
    git("config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: RUN, clonePath: pPath, attemptId: X_ID }));
    git("config", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify({ attemptId: X_ID, runId: RUN, clonePath: pPath, state: "live" }));
    git("config", "--add", `uzi-attempts.${BRANCH}.entry`,
      JSON.stringify({ attemptId: S2_ID, runId: RUN, clonePath: attemptClonePath(canonical, S2_ID), state: "live" }));
    fs.mkdirSync(path.join(pPath, ".git"), { recursive: true });
  });
  const exit = async (): Promise<{ result: Awaited<ReturnType<typeof retry>>; removed: string[] }> => {
    const internals = cache as unknown as {
      withLock<T>(b: string, fn: () => Promise<T>): Promise<T>;
      gatherRetainedUnderLock(b: string): Promise<{ records: Array<{ digest: string; parts: Array<{ clonePath: string }> }> }>;
    };
    const g = await internals.withLock(bare2, () => internals.gatherRetainedUnderLock(bare2));
    const confirm = g.records.find((r) => r.parts.some((p) => p.clonePath === pPath))!.digest;
    const removed: string[] = [];
    const result = await cache.discardStoppedSource({
      selector: { runId: RUN, generation: 1, attempt: X_ID }, confirm, holds: { state: "read", holds: [] },
      lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, quiesce: async () => true, barePath: bare2,
      hasRetainedHomeMarker: (runId, binding) => hasRetainedHomeMarker(dataDir, runId, binding),
      removeRetainedHomeMarker: async (runId) => { removed.push(runId); await removeRetainedHomeMarker(dataDir, runId); },
    });
    return { result, removed };
  };
  for (const [name, written] of [
    ["another bare", () => marker("/somewhere/else/bare.git", X_ID)],
    ["another attempt id", () => marker(bare2, OTHER_ID)],
  ] as const) {
    it(`a marker recording ${name}: the marker survives and the restore exit retires nothing`, async () => {
      await writeRetainedHomeMarker(dataDir, written());
      const { result, removed } = await exit();
      assert.ok(result.kind === "done" || result.kind === "refused", `ordinary path or refusal: ${JSON.stringify(result)}`);
      assert.deepEqual(removed, [], "the marker is never removed");
      assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
      assert.ok(!ledger().some((e) => e.attemptId === S2_ID && e.state === "retired"), "the newer entry is not retired by a restore exit");
    });
  }
  it("control: the marker recording this bare and P's attempt makes it the restore exit, which removes the marker", async () => {
    await writeRetainedHomeMarker(dataDir, marker(bare2, X_ID));
    const { result, removed } = await exit();
    assert.equal(result.kind, "done", JSON.stringify(result));
    assert.deepEqual(removed, [RUN]);
    assert.ok(ledger().some((e) => e.attemptId === S2_ID && e.state === "retired"));
  });
});
