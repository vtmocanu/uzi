import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";

const TOKEN = "tkn-runner-123456";

let api: FakeApi;
let baseUrl: string;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1],
  });
});

afterEach(async () => {
  await api.close();
  fx.cleanup();
});

function worktreeDirFor(iid: number): string {
  const repoDir = path.basename(git.barePathFor(fx.originPath)).replace(/\.git$/, "");
  return path.join(fx.dataDir, "worktrees", repoDir, `issue-${iid}`);
}

describe("RunRunner", () => {
  it("drives a claim running→completed, commits in the worktree, streams gapless messages, and cleans up", async () => {
    const claim = makeClaim({ issue_iid: 7, repo: { id: "r1", url: fx.originPath }, last_seq: 0 });
    await new RunRunner(client, git, new StubExecutor(nullLogger()), nullLogger(), 20).execute(claim);

    // State machine: exactly running then completed on agent/issue-7.
    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "completed"]);
    assert.strictEqual(api.states.find((s) => s.body.status === "completed")?.body.branch, "agent/issue-7");

    // Messages are gapless starting at last_seq + 1.
    const seqs = api.messages(claim.run_id).map((m) => m.seq);
    assert.ok(seqs.length > 0);
    assert.deepStrictEqual(seqs, seqs.map((_, i) => i + 1));

    // The stub's commit landed on the branch in the shared bare store.
    const bare = git.barePathFor(fx.originPath);
    const log = execFileSync("git", ["-C", bare, "log", "--oneline", "agent/issue-7"], { encoding: "utf8" });
    assert.ok(log.includes("uzi stub: work on issue #7"));
    const files = execFileSync("git", ["-C", bare, "show", "--name-only", "--format=", "agent/issue-7"], { encoding: "utf8" });
    assert.ok(files.includes("UZI_RUN.md"));

    // Worktree torn down; clone kept.
    assert.strictEqual(fs.existsSync(worktreeDirFor(7)), false);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
  });

  it("reports failed with a reason and still tears the worktree down when the executor throws", async () => {
    const boom: Executor = { run: async () => { throw new Error("kaboom"); } };
    const claim = makeClaim({ issue_iid: 9, repo: { id: "r", url: fx.originPath }, last_seq: 0 });
    await new RunRunner(client, git, boom, nullLogger(), 20).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "failed"]);
    assert.ok(api.states.find((s) => s.body.status === "failed")?.body.failure_reason?.includes("kaboom"));
    assert.strictEqual(fs.existsSync(worktreeDirFor(9)), false);
  });
});
