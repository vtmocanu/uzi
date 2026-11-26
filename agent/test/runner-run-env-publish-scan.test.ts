import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
import { LimitReachedError } from "../src/limit.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";
import { recordingLogger } from "./helpers.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, assertCustodyRetained, commit, failingWork, heldServer, skipCheckpoints, type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

// PRD 4416 M2: every scanned publish of a run that holds secret variables searches the whole outgoing range
// for the registered forms of their values, before any network write, and withholds on a hit and whenever it
// cannot prove the range clean. Plain values are never needles. Real git; every value is an obvious dummy.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const SECRET = ["rv-Qk7", "Zp2m", "Xw9d", "Lt4s"].join("-");
const OLD_SECRET = ["rv-Hn3b", "Yc6v", "Fd1g", "Ke8j"].join("-");
const PLAIN = ["plain-visible", "value", "9876"].join("-");
const BASE64 = Buffer.from(`ci-user:${SECRET}`).toString("base64");
const DOCKER_CONFIG = JSON.stringify({ auths: { "registry.example.test": { auth: BASE64 } } });

const runEnv = (): ClaimRunEnvVar[] => [
  { var_id: UUID, scope: "repo", rev: 2, name: "DB_PASSWORD", value: SECRET, secret: true, description: "" },
  { var_id: UUID, scope: "repo", rev: 1, name: "REGION", value: PLAIN, secret: false, description: "" },
];
const retired = (): ClaimRunEnvRedact[] => [{ var_id: UUID, scope: "repo", rev: 1, name: "OLD_API_KEY", value: OLD_SECRET }];
const withVars = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000", run_env: runEnv(), run_env_redact: retired() });
const noVars = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

let iid = 46100;

function assertNoValue(label: string, value: unknown): void {
  const text = JSON.stringify(value);
  for (const form of runEnvClaimForms(runEnv(), retired()).redaction) assert.ok(!text.includes(form), `${label}: no registered form of a value appears`);
}

function spyPublish(): { uploads: number } {
  const seen = { uploads: 0 };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    seen.uploads++;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return seen;
}

type Content = { path: string; body: string };
const leak = (body: string): Content => ({ path: "notes.txt", body: `leaked: ${body}\n` });
const CLEAN: Content = { path: "notes.txt", body: "ordinary notes\n" };

/** A run whose executor commits `content`, checkpoints once, and keeps going. */
async function checkpointRun(secrets: object, content: Content, opts: { reap?: boolean } = {}) {
  const seen = spyPublish();
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, { secrets: secrets as never });
  let continued = false;
  const exec: Executor = {
    run: async (ctx) => {
      commit(ctx.worktreePath, content.path, content.body);
      await ctx.checkpoint!({ reap: opts.reap === true });
      continued = true;
      return { branch: ctx.branch };
    },
  };
  await runner(exec, gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 }).execute(claim);
  return { seen, claim, continued: () => continued };
}

describe("run variable scan: checkpoint publication (tick, milestone, iteration)", () => {
  const hits: Array<[string, Content]> = [
    ["the value", leak(SECRET)],
    ["the base64 form inside a docker config auth entry", { path: ".docker/config.json", body: DOCKER_CONFIG }],
    ["a retired version delivered only through run_env_redact", leak(OLD_SECRET)],
  ];
  for (const [label, content] of hits) {
    it(`does not upload a checkpoint carrying ${label}, and keeps running`, async () => {
      const { seen, continued, claim } = await checkpointRun(withVars(), content);
      assert.equal(seen.uploads, 0, "ZERO checkpoint uploads");
      assert.equal(continued(), true, "the withhold never stops the executor");
      assertNoValue("run feed", api.messages(claim.run_id));
    });
  }

  it("a done checkpoint (reap) carrying the value is also withheld", async () => {
    assert.equal((await checkpointRun(withVars(), leak(SECRET), { reap: true })).seen.uploads, 0);
  });

  it("a run without variables uploads the very same commit, and does no scan work", async (t) => {
    const probe = t.mock.method(git, "scanObjectsForRunEnv");
    assert.equal((await checkpointRun(noVars(), leak(SECRET))).seen.uploads, 1);
    assert.equal(probe.mock.callCount(), 0);
  });

  it("a PLAIN value in the tree holds nothing", async () => {
    assert.equal((await checkpointRun(withVars(), leak(PLAIN))).seen.uploads, 1);
  });

  it("a clean commit still uploads, and the exact-value scan was reached", async (t) => {
    const probe = t.mock.method(git, "scanObjectsForRunEnv");
    assert.equal((await checkpointRun(withVars(), CLEAN)).seen.uploads, 1);
    assert.ok(probe.mock.callCount() > 0, "the scan ran");
  });

  it("the upload seam alone withholds (the pre-publish scan is bypassed)", async (t) => {
    const real = git.secretScanCheckpointRange.bind(git);
    t.mock.method(git, "secretScanCheckpointRange", (b: string, r: never, o: { deadlineMs?: number; literals: readonly string[] }) =>
      real(b, r, { deadlineMs: o.deadlineMs, literals: o.literals }));
    assert.equal((await checkpointRun(withVars(), leak(SECRET))).seen.uploads, 0, "the guard at the pack seam refuses on its own");
  });

  it("FAILS CLOSED: a scanner that throws, or cannot establish a clean verdict, uploads nothing", async (t) => {
    t.mock.method(git, "scanObjectsForRunEnv", async () => { throw new Error("scanner exploded"); });
    assert.equal((await checkpointRun(withVars(), CLEAN)).seen.uploads, 0, "throwing scanner");
    t.mock.restoreAll();
    t.mock.method(git, "scanObjectsForRunEnv", async () => ({ trusted: false as const, reason: "read_failed" }));
    assert.equal((await checkpointRun(withVars(), CLEAN)).seen.uploads, 0, "untrusted scanner (timeout, git error, overflow, unresolved ref)");
  });
});

function parkRun(secrets: object, content: Content) {
  const seen = spyPublish();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-re-park-"));
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, { wait_on_limit: true, secrets: secrets as never });
  const exec: Executor = {
    run: async (ctx: RunContext) => {
      commit(ctx.worktreePath, content.path, content.body);
      fs.mkdirSync(path.join(homeRoot, ctx.runId), { recursive: true });
      throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
    },
  };
  const done = runnerWith((runId) => ({ homeDir: path.join(homeRoot, runId), executor: exec }), gitlab).execute(claim)
    .finally(() => fs.rmSync(homeRoot, { recursive: true, force: true }));
  return { seen, claim, done };
}

describe("run variable scan: park, shutdown and pause uploads", () => {
  it("a limit park does not upload a checkpoint carrying the value; the run still parks", async (t) => {
    const probe = t.mock.method(git, "scanObjectsForRunEnv");
    const { seen, claim, done } = parkRun(withVars(), leak(SECRET));
    await done;
    assert.equal(seen.uploads, 0);
    assert.ok(probe.mock.callCount() > 0, "the park publish reached the scan");
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "limit_wait"), "the run parked");
  });

  it("a limit park with a clean commit uploads, through the scan", async (t) => {
    const probe = t.mock.method(git, "scanObjectsForRunEnv");
    const { seen, done } = parkRun(withVars(), CLEAN);
    await done;
    assert.equal(seen.uploads, 1);
    assert.ok(probe.mock.callCount() > 0);
  });

  async function shutdown(secrets: object, content: Content) {
    const seen = spyPublish();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets as never });
    let ready!: () => void;
    const started = new Promise<void>((resolve) => { ready = resolve; });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, content.path, content.body);
        ready();
        await new Promise<void>((resolve) => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
        throw new Error("worker shutdown");
      },
    };
    const r = runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, { checkpointTickIntervalMs: 0 });
    const execution = r.execute(claim);
    try {
      await Promise.race([started, execution.then(() => { throw new Error("run ended before readiness"); })]);
      r.shutdown();
      await execution;
    } finally {
      r.shutdown();
      await execution;
    }
    return seen;
  }

  it("a graceful shutdown does not upload a checkpoint carrying the value", async () => {
    assert.equal((await shutdown(withVars(), leak(SECRET))).uploads, 0);
  });

  async function pause(secrets: object, content: Content) {
    const seen = spyPublish();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets as never });
    const out: { parked?: boolean } = {};
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, content.path, content.body);
        await ctx.checkpoint!({ reap: false });
        const at = { completedCount: 1, total: 3 };
        out.parked = await ctx.parkForPause!(at);
        return out.parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN, { checkpointIntervalMs: 0 }).execute(claim);
    return { seen, out };
  }

  it("a pause whose range carries the value uploads nothing and does not park", async () => {
    const { seen, out } = await pause(withVars(), leak(SECRET));
    assert.equal(seen.uploads, 0);
    assert.equal(out.parked, false, "no durable checkpoint, so no park");
  });

  it("a pause with a clean commit uploads and parks as before", async () => {
    const { seen, out } = await pause(withVars(), CLEAN);
    assert.equal(seen.uploads, 1);
    assert.equal(out.parked, true);
  });
});

describe("run variable scan: overlay (GitHub milestone) checkpoint upload", () => {
  const githubRepo = () => ({ id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" as const });
  async function overlayMilestone(t: TestContext, secrets: object, content: Content) {
    const seen = spyPublish();
    const { github } = fakeGitHub();
    const { gitlab } = fakeGitlab();
    t.mock.method(git, "fetchDefaultTip", async () => { throw new Error("stub: no network"); });
    const claim = gitlabClaim(iid++, { secrets: secrets as never, repo: githubRepo() });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, content.path, content.body);
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN, { github, checkpointIntervalMs: 1000 }).execute(claim);
    return seen;
  }

  it("a milestone carrying the value uploads nothing", async (t) => {
    assert.equal((await overlayMilestone(t, withVars(), leak(SECRET))).uploads, 0);
  });
});

describe("run variable scan: the final branch push (every forge)", () => {
  const branchOnOrigin = (iidN: number): boolean => {
    try {
      execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "--quiet", `refs/heads/agent/issue-${iidN}`], { stdio: "pipe" });
      return true;
    } catch { return false; }
  };
  async function finalize(secrets: object, content: Content) {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets as never });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, content.path, content.body);
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN).execute(claim);
    return claim;
  }
  const failed = (runId: string) => api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;

  for (const [label, content] of [
    ["the value", leak(SECRET)],
    ["the base64 form in a docker config", { path: ".docker/config.json", body: DOCKER_CONFIG }],
    ["a retired version", leak(OLD_SECRET)],
  ] as Array<[string, Content]>) {
    it(`a GitLab run that committed ${label} fails push_secret_blocked, names the variable and pushes nothing`, async () => {
      const claim = await finalize(withVars(), content);
      assert.equal(branchOnOrigin(claim.issue_iid!), false, "the branch never reached origin");
      const body = failed(claim.run_id);
      assert.ok(body, "the run failed");
      assert.equal(body.fail_origin, "push_secret_blocked");
      assert.equal(body.preserved_patch, undefined);
      assert.match(String(body.failure_reason), /DB_PASSWORD|OLD_API_KEY/, "the variable is named");
      assertNoValue("terminal report", body);
      assertNoValue("run feed", api.messages(claim.run_id));
    });
  }

  it("the push seam alone withholds (every pre-push scan is bypassed), with a fixed typed refusal naming the variable", async (t) => {
    const real = git.secretScanCheckpointRange.bind(git);
    t.mock.method(git, "secretScanCheckpointRange", (b: string, r: never, o: { deadlineMs?: number; literals: readonly string[] }) =>
      real(b, r, { deadlineMs: o.deadlineMs, literals: o.literals }));
    const claim = await finalize(withVars(), leak(SECRET));
    assert.equal(branchOnOrigin(claim.issue_iid!), false, "ZERO pushes");
    const body = failed(claim.run_id);
    assert.equal(body?.fail_origin, "push_secret_blocked");
    assert.match(String(body?.failure_reason), /secret variable DB_PASSWORD/);
    assertNoValue("terminal report", body);
  });

  it("FAILS CLOSED when the push-seam scanner throws: nothing is pushed", async (t) => {
    t.mock.method(git, "scanObjectsForRunEnv", async () => { throw new Error("scanner exploded"); });
    const claim = await finalize(withVars(), CLEAN);
    assert.equal(branchOnOrigin(claim.issue_iid!), false);
    assert.equal(failed(claim.run_id)?.fail_origin, "push_secret_blocked");
  });

  it("a run without variables pushes the same commit; a PLAIN value and a clean commit push too", async () => {
    assert.equal(branchOnOrigin((await finalize(noVars(), leak(SECRET))).issue_iid!), true);
    assert.equal(branchOnOrigin((await finalize(withVars(), leak(PLAIN))).issue_iid!), true);
    assert.equal(branchOnOrigin((await finalize(withVars(), CLEAN)).issue_iid!), true);
  });
});

// PRD 4416 M3: a secret containing "/" can be published as a git PATH, which no single tree object holds.
// Every case commits harmless content under a harmless message; only the path joins to the value.
describe("run variable scan: a secret published as a git path", () => {
  const PATH_VALUE = ["rv-Ab3k", "Mq7w-Zx2n", "Fe9t"].join("/");
  const FLAT_VALUE = ["rv-Ab3k-Mq7w", "Zx2n-Fe9t"].join("/");
  const SINGLE_VALUE = ["rv-Ab3k", "Mq7w", "Zx2n", "Fe9t"].join("-");
  const pathVars = (value: string) => ({
    forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000",
    run_env: [{ var_id: UUID, scope: "repo", rev: 1, name: "DB_PASSWORD", value, secret: true, description: "" }] as ClaimRunEnvVar[],
  });
  const G_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const g = (dir: string, ...args: string[]): string =>
    execFileSync("git", ["-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", ...args],
      { env: G_ENV, encoding: "utf8", input: "" }).trim();
  const write = (dir: string, name: string, body = "harmless\n"): void => {
    fs.mkdirSync(path.dirname(path.join(dir, name)), { recursive: true });
    fs.writeFileSync(path.join(dir, name), body);
  };
  const addCommit = (dir: string, name: string): void => { write(dir, name); g(dir, "add", "-A"); g(dir, "commit", "-m", "update"); };

  // Leaves `dir` mid-merge of a side branch into `branch`, both parents holding a commit of their own.
  const startEvilMerge = (dir: string, branch: string): void => {
    g(dir, "checkout", "-q", "-b", "side");
    addCommit(dir, "side.txt");
    g(dir, "checkout", "-q", branch);
    addCommit(dir, "mine.txt");
    g(dir, "merge", "--no-commit", "--no-ff", "side");
  };

  const onOrigin = (n: number): boolean => {
    try {
      execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "--quiet", `refs/heads/agent/issue-${n}`], { stdio: "pipe" });
      return true;
    } catch { return false; }
  };
  async function finalizeWith(value: string, work: (ctx: RunContext) => void) {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: pathVars(value) as never });
    const exec: Executor = { run: async (ctx) => { work(ctx); return { branch: ctx.branch }; } };
    await runner(exec, gitlab, JOIN_TOKEN).execute(claim);
    return claim;
  }
  const failedBody = (runId: string) => api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;
  function assertBlocked(claim: { run_id: string; issue_iid?: number | null }, value: string): void {
    assert.equal(onOrigin(claim.issue_iid!), false, "the branch never reached origin");
    const body = failedBody(claim.run_id);
    assert.ok(body, "the run failed");
    assert.equal(body.fail_origin, "push_secret_blocked", String(body.failure_reason));
    assert.match(String(body.failure_reason), /secret variable DB_PASSWORD/);
    for (const text of [JSON.stringify(body), JSON.stringify(api.messages(claim.run_id))]) assert.ok(!text.includes(value), "no value in any report");
  }

  const cases: Array<[string, string, (ctx: RunContext, value: string) => void]> = [
    ["a nested path whose components join to the value", PATH_VALUE, (ctx, v) => addCommit(ctx.worktreePath, v)],
    ["a file named after a value with no slash", SINGLE_VALUE, (ctx, v) => addCommit(ctx.worktreePath, v)],
    ["a rename of an already-public file to such a path", FLAT_VALUE, (ctx, v) => {
      g(ctx.worktreePath, "mv", "README.md", "moved.md");
      fs.mkdirSync(path.dirname(path.join(ctx.worktreePath, v)), { recursive: true });
      g(ctx.worktreePath, "mv", "moved.md", v);
      g(ctx.worktreePath, "commit", "-m", "update");
    }],
    ["a path an evil merge introduces", PATH_VALUE, (ctx, v) => {
      const dir = ctx.worktreePath;
      g(dir, "checkout", "-q", "-b", "side");
      addCommit(dir, "side.txt");
      g(dir, "checkout", "-q", ctx.branch);
      addCommit(dir, "mine.txt");
      g(dir, "merge", "--no-commit", "--no-ff", "side");
      write(dir, v);
      g(dir, "add", "-A");
      g(dir, "commit", "-m", "merge");
    }],
    ["an empty-tree entry at such a path", PATH_VALUE, (ctx, v) => {
      const dir = ctx.worktreePath;
      addCommit(dir, "src/unrelated.txt"); // an empty tree alone is not a change the run could finish with
      const empty =g(dir, "hash-object", "-t", "tree", "/dev/null");
      const [a, mid, leaf] = v.split("/") as [string, string, string];
      const mk = (input: string): string => execFileSync("git", ["-C", dir, "mktree"], { env: G_ENV, encoding: "utf8", input }).trim();
      const inner = mk(`040000 tree ${empty}\t${leaf}\n`);
      const middle = mk(`040000 tree ${inner}\t${mid}\n`);
      const root = mk(`${g(dir, "ls-tree", "HEAD")}\n040000 tree ${middle}\t${a}\n`);
      const c = g(dir, "commit-tree", root, "-p", "HEAD", "-m", "update");
      g(dir, "reset", "--hard", c);
    }],
    ["an empty-tree entry an evil merge introduces", PATH_VALUE, (ctx, v) => {
      const dir = ctx.worktreePath;
      startEvilMerge(dir, ctx.branch);
      const empty = g(dir, "hash-object", "-t", "tree", "/dev/null");
      const [a, mid, leaf] = v.split("/") as [string, string, string];
      const mk = (input: string): string => execFileSync("git", ["-C", dir, "mktree"], { env: G_ENV, encoding: "utf8", input }).trim();
      const inner = mk(`040000 tree ${empty}\t${leaf}\n`);
      const middle = mk(`040000 tree ${inner}\t${mid}\n`);
      const root = mk(`${g(dir, "ls-tree", g(dir, "write-tree"))}\n040000 tree ${middle}\t${a}\n`);
      const c = g(dir, "commit-tree", root, "-p", "HEAD", "-p", "side", "-m", "merge");
      g(dir, "reset", "--hard", c);
    }],
    ["a gitlink an evil merge introduces", PATH_VALUE, (ctx, v) => {
      const dir = ctx.worktreePath;
      startEvilMerge(dir, ctx.branch);
      g(dir, "update-index", "--add", "--cacheinfo", `160000,${g(dir, "rev-parse", "HEAD")},${v}`);
      g(dir, "commit", "-m", "merge");
    }],
  ];
  for (const [label, value, work] of cases) {
    it(`withholds the final push of ${label}, naming the variable and nothing else`, async () => {
      assertBlocked(await finalizeWith(value, (ctx) => work(ctx, value)), value);
    });
  }

  it("a control publish of an unrelated path still succeeds", async () => {
    const claim = await finalizeWith(PATH_VALUE, (ctx) => addCommit(ctx.worktreePath, "src/app/unrelated.txt"));
    assert.equal(onOrigin(claim.issue_iid!), true);
  });

  it("withholds a checkpoint upload of such a path, and the run keeps going", async () => {
    const seen = spyPublish();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: pathVars(PATH_VALUE) as never });
    let continued = false;
    const exec: Executor = {
      run: async (ctx) => {
        addCommit(ctx.worktreePath, PATH_VALUE);
        await ctx.checkpoint!({ reap: false });
        continued = true;
        return { branch: ctx.branch };
      },
    };
    const rec = recordingLogger();
    await runnerWith(() => ({ executor: exec }), gitlab, JOIN_TOKEN, rec.logger, { checkpointIntervalMs: 1 }).execute(claim);
    assert.equal(seen.uploads, 0, "ZERO checkpoint uploads");
    assert.equal(continued, true);
    const skipped = rec.lines.filter((l) => (l as { msg?: string }).msg === "checkpoint publish skipped: secret_found") as Array<{ findings?: string[] }>;
    assert.ok(skipped.length >= 1, "the withhold is logged as a skipped checkpoint publish with reason secret_found");
    for (const l of skipped) assert.ok(l.findings?.some((f) => f.includes("variable DB_PASSWORD")), "names the variable");
    assert.ok(!JSON.stringify(rec.lines).includes(PATH_VALUE), "no value in any log line");
  });

  it("FAILS CLOSED: an incomplete path pass (path cap reached) pushes nothing", async (t) => {
    const real = git.scanObjectsForRunEnv.bind(git);
    t.mock.method(git, "scanObjectsForRunEnv", (b: string, r: never, n: never, o: { caps?: object } = {}) =>
      real(b, r, n, { ...o, caps: { ...o.caps, maxPaths: 1 } } as never));
    const claim = await finalizeWith(PATH_VALUE, (ctx) => addCommit(ctx.worktreePath, "src/app/unrelated.txt"));
    assert.equal(onOrigin(claim.issue_iid!), false, "ZERO pushes");
    assert.equal(failedBody(claim.run_id)?.fail_origin, "push_secret_blocked");
  });
});

describe("run variable scan: held publication and the recovery archive", () => {
  async function held(secrets: object, content: Content, steer?: (server: ReturnType<typeof heldServer>) => void) {
    const claim = gitlabClaim(iid++, { claim_generation: 50, inventory_guarded: true, secrets: secrets as never });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    steer?.(server);
    skipCheckpoints();
    const work: Partial<Work> = {};
    const r = runner(failingWork(work, (ctx) => { commit(ctx.worktreePath, content.path, content.body); }), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
    await r.execute(claim);
    return { claim, server, work };
  }

  it("a run whose held range carries the value uploads neither a held pack nor the archive, and keeps custody", { skip: HELD_LINUX_ONLY }, async () => {
    const { claim, server, work } = await held(withVars(), leak(SECRET));
    assert.equal(server.heldCalls.length, 0, "ZERO held uploads");
    assert.equal(server.archiveUploads(), 0, "the recovery archive carries the same objects and is withheld too");
    await assertCustodyRetained(server, claim, work, "value in held range");
  });

  it("the same commit from a run without variables is published held, as before", { skip: HELD_LINUX_ONLY }, async () => {
    const { server } = await held(noVars(), leak(SECRET));
    assert.equal(server.heldCalls.length, 1);
  });

  // A multi-line secret withholds only the preserved patch; the work stays recoverable through custody.
  const SHORT_LINES = ["ab12", "cd34", "ef56"];
  const multiLive = () => ({
    forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000",
    run_env: [{ var_id: UUID, scope: "repo" as const, rev: 1, name: "SVC_BLOB", value: SHORT_LINES.join("\n"), secret: true, description: "" }],
  });
  const multiRetained = () => ({
    forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000",
    run_env: runEnv(),
    run_env_redact: [{ var_id: UUID, scope: "repo" as const, rev: 1, name: "OLD_BLOB", value: SHORT_LINES.join("\n") }],
  });

  // The api refuses the held pack, so the clean work is archived and custody stays open (hold, capture, export).
  const refuseHeld = (server: ReturnType<typeof heldServer>): void => {
    server.heldAnswer = () => ({ ok: false, httpStatus: 409, reason: "ci_config_guard" });
  };
  const shapes: Array<[string, () => object]> = [["live", multiLive], ["retained", multiRetained]];
  for (const [kind, secrets] of shapes) {
    it(`a run holding a ${kind} multi-line secret still publishes its clean held range`, { skip: HELD_LINUX_ONLY }, async () => {
      const { server } = await held(secrets(), CLEAN);
      assert.equal(server.heldCalls.length, 1, "the held pack is uploaded");
    });

    it(`a run holding a ${kind} multi-line secret still archives its clean range and keeps custody when the held pack is refused`, { skip: HELD_LINUX_ONLY }, async () => {
      const { claim, server, work } = await held(secrets(), CLEAN, refuseHeld);
      assert.equal(server.heldCalls.length, 1, "the held pack was offered");
      assert.ok(server.archiveUploads() >= 1, "the recovery archive is uploaded");
      await assertCustodyRetained(server, claim, work, `${kind} multi-line secret, clean range`);
    });
  }
});

describe("run variable scan: preserved_patch", () => {
  const GUARD = { ci_config_guard: true, ci_config_paths: ["deploy/ci.yml"] };
  async function refused(secrets: object, body: string) {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { config: GUARD, secrets: secrets as never });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, ".forgejo/workflows/x.yml", body);
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab).execute(claim);
    return api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
  }

  it("withholds a patch whose raw text carries the value, and attaches the same patch without it", async () => {
    const withValue = await refused(withVars(), `name: ci\n# ${SECRET}\n`);
    assert.ok(withValue, "the guard refused the run");
    assert.equal(withValue.preserved_patch, undefined, "the raw patch carried the value");
    assertNoValue("terminal report", withValue);
    const clean = await refused(withVars(), "name: ci\n");
    assert.match(clean?.preserved_patch ?? "", /workflows\/x\.yml/, "a patch without the value is still attached");
  });

  it("sends the terminal failure reason with a registered secret masked (the guard composes the reason from a path name)", async () => {
    const pat = "fixture-forge-pat-000000";
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { config: GUARD, secrets: withVars() as never });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, `.forgejo/workflows/${pat}.yml`, "name: ci\n");
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed.length > 0, "the guard refused the run");
    for (const s of failed) {
      assert.match(s.body.failure_reason ?? "", /CI-config guard/);
      assert.ok(!(s.body.failure_reason ?? "").includes(pat), "the outbound reason carries no registered secret");
      assert.ok((s.body.failure_reason ?? "").includes("***REDACTED***"), "the secret is masked in place");
    }

    // A long path puts the secret across the display cut (the quoted path is cut to 79 characters, and the
    // secret starts at character 69 of it): no prefix of the secret may survive in the reason or the status.
    const longClaim = gitlabClaim(iid++, { config: GUARD, secrets: withVars() as never });
    const longPath = `.forgejo/workflows/${"y".repeat(49)}${pat}.yml`;
    const longExec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, longPath, "name: ci\n");
        return { branch: ctx.branch };
      },
    };
    await runner(longExec, fakeGitlab().gitlab).execute(longClaim);
    const longFailed = api.states.filter((s) => s.runId === longClaim.run_id && s.body.status === "failed");
    assert.ok(longFailed.length > 0, "the guard refused the long-path run");
    const outbound = [...longFailed.map((s) => s.body.failure_reason ?? ""), JSON.stringify(api.messages(longClaim.run_id))];
    for (const text of outbound) {
      for (let i = 0; i + 8 <= pat.length; i++) {
        assert.ok(!text.includes(pat.slice(i, i + 8)), `a fragment of the secret survives: ${pat.slice(i, i + 8)}`);
      }
    }
    assert.match(longFailed[0]!.body.failure_reason ?? "", /CI-config guard/);
  });

  it("withholds a patch carrying the base64 form", async () => {
    assert.equal((await refused(withVars(), `name: ci\n# ${BASE64}\n`))?.preserved_patch, undefined);
  });
});
