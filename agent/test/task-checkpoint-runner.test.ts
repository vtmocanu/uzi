import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { ClaimResponse } from "../src/protocol.js";
import type { Executor } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();
const ROOT = "11111111-1111-4111-8111-111111111111";
const CHILD = "22222222-2222-4222-8222-222222222222";
const REPO = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const BRANCH = "uzi/task/11111111-1111-4111-8111-111111111111";
const KEY = "uzi/task/22222222-2222-4222-8222-222222222222";
const REF = "refs/uzi-checkpoints/uzi/task/22222222-2222-4222-8222-222222222222";

function cmd(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
}
function commit(cwd: string, file: string): string {
  fs.writeFileSync(path.join(cwd, file), file);
  cmd(cwd, "add", file);
  cmd(cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "-c", "commit.gpgsign=false", "commit", "-qm", file);
  return cmd(cwd, "rev-parse", "HEAD");
}
function claim(): ClaimResponse {
  return gitlabClaim(1, { run_id: CHILD, kind: "task", issue_iid: null, branch: BRANCH,
    open_mr: false, claim_generation: 2, checkpoint_write_unresolved: false,
    repo: { id: REPO, clone_url: fx.originPath, url: "https://gitlab.example.test/org/repo" },
    checkpoint_coordinates: { version: 1, run_id: CHILD, user_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
      repo_id: REPO, kind: "task", issue_iid: null, work_branch: BRANCH, checkpoint_key: KEY,
      provenance: [CHILD, ROOT] } });
}
function observe(r: RunRunner): Array<[string, number, string, string]> {
  const seen: Array<[string, number, string, string]> = [];
  // Observe the publication boundary while execute drives the real checkpoint/pack paths.
  const internals = r as unknown as { settlement: { enabled: boolean }; settler: {
    observeLivePublication: (...args: [string, number, string, string]) => Promise<number>;
    settleLive: () => Promise<void>;
  } };
  Object.defineProperty(internals.settlement, "enabled", { value: true });
  internals.settler.observeLivePublication = async (...args) => { seen.push(args); return 0; };
  internals.settler.settleLive = async () => {};
  return seen;
}

for (const enrolled of [true, false]) it("execute keeps task branch publication with enrollment=" + enrolled, async () => {
  const c = claim();
  if (!enrolled) delete c.checkpoint_coordinates;
  const { gitlab } = fakeGitlab();
  let clock = 0;
  const requests: Array<{ run: string; generation: number | undefined; tip: string }> = [];
  const packedBranches: string[] = [];
  const pack = git.checkpointPack.bind(git);
  git.checkpointPack = async (...args) => { packedBranches.push(args[1]); return pack(...args); };
  client.publishCheckpoint = async (run, tip, stream, _signal, generation) => {
    for await (const _chunk of stream) { /* drain actual producer */ }
    requests.push({ run, generation, tip });
    return { ok: true, body: { published: true, ref: REF } };
  };
  let committed = "";
  const executor: Executor = { run: async ctx => {
    committed = commit(ctx.worktreePath, "task-work");
    fs.writeFileSync(path.join(ctx.worktreePath, "untracked-wip"), "not committed");
    clock = 1001;
    await ctx.checkpoint!({ reap: false, progress: { completed: ["m1"], in_progress: [] } });
    return { branch: ctx.branch };
  } };
  const r = runner(executor, gitlab, undefined, { checkpointIntervalMs: 1000, now: () => clock });
  const observed = observe(r);
  await r.execute(c);
  assert.ok(api.states.some(s => s.runId === CHILD && s.body.status === "completed"));
  assert.equal(cmd(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), committed);
  assert.deepEqual(packedBranches, enrolled ? [BRANCH] : []);
  assert.deepEqual(requests, enrolled ? [{ run: CHILD, generation: 2, tip: committed }] : []);
  assert.deepEqual(observed, enrolled
    ? [[CHILD, 2, committed, "checkpoint"], [CHILD, 2, committed, "branch"]]
    : [[CHILD, 2, committed, "branch"]]);
  assert.throws(() => cmd(fx.originPath, "cat-file", "-e", committed + ":untracked-wip"));
});

it("a successful response naming another ref never confirms task durability or settles checkpoint custody", async () => {
  const c = claim();
  const { gitlab } = fakeGitlab();
  let clock = 0;
  const tips: string[] = [];
  client.publishCheckpoint = async (_run, tip, stream) => {
    for await (const _chunk of stream) { /* drain actual producer */ }
    tips.push(tip);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/uzi/task/11111111-1111-4111-8111-111111111111" } };
  };
  const executor: Executor = { run: async ctx => {
    commit(ctx.worktreePath, "task-work");
    clock = 1001;
    await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
    clock = 2002;
    await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
    return { branch: ctx.branch };
  } };
  const r = runner(executor, gitlab, undefined, { checkpointIntervalMs: 1000, now: () => clock });
  const observed = observe(r);
  await r.execute(c);
  assert.equal(tips.length, 2, "unconfirmed publication retries at the next time boundary");
  assert.deepEqual(observed.map(x => x[3]), ["branch"], "only final branch publication is durable");
});

for (const initial of [false, true, undefined, "false"] as const) it(
  "execute freezes task write evidence before credentialed preparation: " + JSON.stringify(initial),
  async () => {
    const c = claim();
    c.checkpoint_write_unresolved = initial as boolean | undefined;
    const { gitlab } = fakeGitlab();
    const ensure = git.ensureClone.bind(git);
    git.ensureClone = async (...args) => {
      c.checkpoint_write_unresolved = initial !== false ? false : true;
      c.checkpoint_tip = "f".repeat(40);
      (c.checkpoint_coordinates as { checkpoint_key: string }).checkpoint_key = BRANCH;
      return ensure(...args);
    };
    let entered = false;
    const executor: Executor = { run: async ctx => {
      entered = true;
      commit(ctx.worktreePath, "frozen-evidence");
      return { branch: ctx.branch };
    } };
    await runner(executor, gitlab).execute(c);
    assert.equal(entered, initial === false);
    if (initial === false) {
      assert.ok(api.states.some(s => s.runId === CHILD && s.body.status === "completed"));
    } else {
      assert.ok(api.states.some(s => s.runId === CHILD && s.body.status === "failed"));
      assert.throws(() => cmd(fx.originPath, "rev-parse", "refs/heads/" + BRANCH));
    }
  });

for (const mode of ["declared-task", "capped-issue", "empty-prompt"] as const) it(
  "report-only guard receives the exact key and work-branch content floor for " + mode,
  async () => {
    const c = mode === "declared-task" ? claim() : gitlabClaim(42, { run_id: CHILD,
      kind: mode === "capped-issue" ? "issue" : "prompt", issue_iid: mode === "capped-issue" ? 42 : null });
    const expectedBranch = mode === "declared-task" ? BRANCH
      : mode === "capped-issue" ? "agent/issue-42" : "uzi/prompt-22222222-2222-4222-8222-222222222222";
    const expectedKey = mode === "declared-task" ? KEY : expectedBranch;
    const { gitlab } = fakeGitlab();
    const checks: Array<{ branch: string; key: string | null | undefined }> = [];
    const original = git.hasCommittedCheckpoint.bind(git);
    git.hasCommittedCheckpoint = async (bare, branch, confirmed, key) => {
      checks.push({ branch, key });
      return original(bare, branch, confirmed, key);
    };
    const executor: Executor = { run: async ctx => ({ branch: ctx.branch,
      ...(mode === "declared-task" ? { reportOnly: true } :
        mode === "capped-issue" ? { scopeCapped: { completedCount: 0, total: 2 } } : {}),
      summary: "No committed work",
    }) };
    await runner(executor, gitlab).execute(c);
    assert.ok(api.states.some(s => s.runId === CHILD && s.body.status === "completed" && s.body.report_only));
    assert.deepEqual(checks, [{ branch: expectedBranch, key: expectedKey }]);
  });
