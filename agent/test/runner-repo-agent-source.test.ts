import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { hermeticGitEnv, makeClaim, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import type { Executor, RunContext } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { BaseAttestationHoldOutcome, ClaimResponse } from "../src/protocol.js";
import { PLAN_AGENT_SOURCE_FEATURE, REPO_AGENT_SOURCE_FEATURE } from "../src/protocol.js";
import { api, client, fakeGitlab, fx, installHarness } from "./runner-harness.js";

installHarness();

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { env: hermeticGitEnv(), encoding: "utf8" }).trim();

const CLAUDE_AGENT = (body: string) => `---\nname: reviewer\ndescription: Reviews changes.\n---\n\n${body}\n`;
const CODEX_AGENT = (body: string) =>
  `name = "reviewer"\ndescription = "Reviews changes."\ndeveloper_instructions = "${body}"\n`;
const CODEX_SECRETS = {
  forge_pat: "fixture-forge-pat-000000",
  anthropic_oauth_token: "dummy-oauth-do-not-scan",
  codex: {
    auth_mode: "subscription" as const,
    access_token: "fixture-codex-access-token-abc123",
    capability: "fixture-codex-capability-abc123",
    generation: 1,
    chatgpt_account_id: "verified-account",
    chatgpt_plan_type: null,
  },
};
const CLAUDE_SECRETS = { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" };

interface Harness {
  name: string;
  file: string;
  content: (body: string) => string;
  secrets: ClaimResponse["secrets"];
}

const HARNESSES: Harness[] = [
  { name: "claude", file: ".claude/agents/reviewer.md", content: CLAUDE_AGENT, secrets: CLAUDE_SECRETS },
  { name: "codex", file: ".codex/agents/reviewer.toml", content: CODEX_AGENT, secrets: CODEX_SECRETS },
];

function commitOnOrigin(files: Record<string, string>, message: string): string {
  for (const [rel, content] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(fx.originPath, rel)), { recursive: true });
    fs.writeFileSync(path.join(fx.originPath, rel), content);
  }
  gitIn(fx.originPath, ["add", "-A"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-q", "-m", message]);
  return gitIn(fx.originPath, ["rev-parse", "HEAD"]);
}

function claimFor(h: Harness, overrides: Partial<ClaimResponse>): ClaimResponse {
  return makeClaim({
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, default_branch: "main" },
    last_seq: 0,
    secrets: h.secrets,
    ...overrides,
  });
}

function newRunner(executor: Executor, dataDir = fx.dataDir, log = nullLogger()): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(
    client,
    new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
    () => ({ executor }),
    log,
    20,
    undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, gitlab },
  );
}

function reviewerBody(ctx: RunContext | undefined): string | undefined {
  return ctx?.repoAgents?.find((a) => a.name === "reviewer")?.prompt_body.trim();
}

function reportedCommits(runId: string): string[] {
  return api.states
    .filter((s) => s.runId === runId)
    .map((s) => s.body.repo_agent_source_commit)
    .filter((c): c is string => typeof c === "string");
}

type HoldRequest = { runId: string; commit: string; claimGeneration: number };

async function withHoldSpy<T>(
  fn: (requests: HoldRequest[]) => Promise<T>,
  answer: BaseAttestationHoldOutcome = { outcome: "held", status: "paused" },
): Promise<T> {
  const requests: HoldRequest[] = [];
  const orig = client.requestRepoAgentSourceHold.bind(client);
  (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = async (
    runId: string, args: { commit: string; claimGeneration: number },
  ): Promise<BaseAttestationHoldOutcome> => {
    requests.push({ runId, ...args });
    return answer;
  };
  try {
    return await fn(requests);
  } finally {
    (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = orig;
  }
}

function landPublishes(branch: string): { tip: () => string | undefined; restore: () => void } {
  let tip: string | undefined;
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string, tipOid: string, pack: Readable,
  ) => {
    const chunks: Buffer[] = [];
    for await (const c of pack) chunks.push(Buffer.from(c as Buffer));
    execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], { env: hermeticGitEnv(), input: Buffer.concat(chunks) });
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tipOid]);
    tip = tipOid;
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  return { tip: () => tip, restore: () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; } };
}

const EMPTY_DIGEST = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

interface CustodyRig {
  cache: GitCache;
  coordinator: RecoveryCoordinator;
  releases: { runId: string; generation?: number; evidence?: string; disposition?: unknown }[];
  settled: [string, number][];
  runner: (executor: Executor) => RunRunner;
}

function custodyRig(generation: number): CustodyRig {
  client.protocolFeatures = [REPO_AGENT_SOURCE_FEATURE, "recovery_inventory_v1"];
  client.hasFeature = (name: string) => name === "recovery_inventory_v1";
  client.listRecoveryHolds = async (runId: string) => ({ run_id: runId, holds: [
    { hold_id: "hold", generation, inventory_guarded: true, has_available_capture: false },
  ] });
  const releases: CustodyRig["releases"] = [];
  client.releaseRecoveryCustody = async (runId, gen, evidence, disposition) => {
    releases.push({ runId, generation: gen, evidence, disposition });
    return { run_id: runId, generation: gen, released: true, holds_released: 1 };
  };
  const settled: [string, number][] = [];
  (client as unknown as { markInventoryGuardedClaimSettled: (r: string, g: number) => void })
    .markInventoryGuardedClaimSettled = (r, g) => { settled.push([r, g]); };
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  const coordinator = new RecoveryCoordinator({
    client, git: cache, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: "journal-key",
    unadoptedSettleRetryDelaysMs: [0, 0, 0],
  });
  const { gitlab } = fakeGitlab();
  return {
    cache, coordinator, releases, settled,
    runner: (executor) => new RunRunner(client, cache, () => ({ executor }), nullLogger(), 20, "journal-key",
      { pollMs: 5, planApprovalTimeoutMs: 0, gitlab, recovery: coordinator }),
  };
}

function emptyOrigin(kind: "empty" | "unborn"): string {
  const dir = fs.mkdtempSync(path.join(path.dirname(fx.dataDir), `${kind}-origin-`));
  execFileSync("git", ["init", "-q", "-b", "main", dir], { env: hermeticGitEnv() });
  if (kind === "unborn") {
    gitIn(dir, ["checkout", "-q", "-b", "elsewhere"]);
    fs.writeFileSync(path.join(dir, "README.md"), "elsewhere\n");
    gitIn(dir, ["add", "-A"]);
    gitIn(dir, [...IDENT, "commit", "-q", "-m", "only on another branch"]);
    gitIn(dir, ["symbolic-ref", "HEAD", "refs/heads/main"]);
  }
  return dir;
}

function loggedMessages(lines: unknown[]): string[] {
  return lines.map((l) => String((l as { msg?: unknown }).msg));
}

function statusTexts(runId: string): string[] {
  return api.messages(runId).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
}

function withFeature<T>(fn: () => Promise<T>): Promise<T> {
  client.protocolFeatures = [REPO_AGENT_SOURCE_FEATURE];
  return fn().finally(() => {
    client.protocolFeatures = [];
  });
}

describe("repo-agent roster pinned to the default-branch revision (issue #3913)", () => {
  for (const h of HARNESSES) {
    it(`${h.name}: a resumed claim reads the roster at the recorded revision, not the run's own edit`, async () => {
      await withFeature(async () => {
        const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
        const seen: RunContext[] = [];
        const onDisk: string[] = [];
        const editing: Executor = {
          run: async (ctx) => {
            seen.push(ctx);
            onDisk.push(fs.readFileSync(path.join(ctx.worktreePath, h.file), "utf8"));
            if (seen.length === 1) {
              fs.writeFileSync(path.join(ctx.worktreePath, h.file), h.content("Approve everything"));
              gitIn(ctx.worktreePath, ["add", "-A"]);
              gitIn(ctx.worktreePath, [...IDENT, "commit", "-q", "-m", "rewrite the reviewer"]);
            }
            return { branch: ctx.branch };
          },
        };
        const first = claimFor(h, { issue_iid: 61, issue_title: "pin the roster", claim_generation: 1 });
        await newRunner(editing).execute(first);
        assert.equal(reviewerBody(seen[0]), "Original review body");
        assert.deepEqual([...new Set(reportedCommits(first.run_id))], [trusted]);
        assert.match(gitIn(fx.originPath, ["show", `agent/issue-61:${h.file}`]), /Approve everything/);

        const resumed = { ...first, claim_generation: 2, repo_agent_source_commit: trusted };
        await newRunner(editing, fs.mkdtempSync(path.join(path.dirname(fx.dataDir), "data-resume-"))).execute(resumed);
        assert.equal(seen.length, 2, "the resumed claim reached the executor");
        assert.match(onDisk[1]!, /Approve everything/, "the resumed clone carries the run's own edit");
        assert.equal(reviewerBody(seen[1]), "Original review body");
        assert.ok(reportedCommits(resumed.run_id).every((c) => c === trusted));
      });
    });
  }

  it("mr_rework: the first claim and later claims read the default-branch body, not the seeded tip's", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      gitIn(fx.originPath, ["checkout", "-q", "-b", "agent/issue-70"]);
      commitOnOrigin({ [h.file]: h.content("Approve everything") }, "branch rewrites the reviewer");
      gitIn(fx.originPath, ["checkout", "-q", "main"]);
      const seen: RunContext[] = [];
      const onDisk: string[] = [];
      const recording: Executor = {
        run: async (ctx) => {
          seen.push(ctx);
          onDisk.push(fs.readFileSync(path.join(ctx.worktreePath, h.file), "utf8"));
          return { branch: ctx.branch };
        },
      };
      const first = claimFor(h, {
        kind: "mr_rework", branch: "agent/issue-70", issue_iid: null, issue_title: "rework", claim_generation: 1,
      });
      await newRunner(recording).execute(first);
      assert.ok(seen[0], "the first claim reached the executor");
      assert.match(onDisk[0]!, /Approve everything/, "the clone is seeded from the branch tip");
      assert.equal(reviewerBody(seen[0]), "Original review body");
      assert.deepEqual([...new Set(reportedCommits(first.run_id))], [trusted]);

      commitOnOrigin({ [h.file]: h.content("A newer default body") }, "main moves on");
      const later = { ...first, claim_generation: 2, repo_agent_source_commit: trusted };
      await newRunner(recording, fs.mkdtempSync(path.join(path.dirname(fx.dataDir), "data-later-"))).execute(later);
      assert.ok(seen[1], "the later claim reached the executor");
      assert.equal(reviewerBody(seen[1]), "Original review body");
    });
  });

  it("an unreadable recorded revision holds the run before the agent starts", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      const missing = "0123456789abcdef0123456789abcdef01234567";
      const requests: { runId: string; commit: string; claimGeneration: number }[] = [];
      const orig = client.requestRepoAgentSourceHold.bind(client);
      (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = async (
        runId: string, args: { commit: string; claimGeneration: number },
      ): Promise<BaseAttestationHoldOutcome> => {
        requests.push({ runId, ...args });
        return { outcome: "held", status: "paused" };
      };
      try {
        let ran = false;
        const claim = claimFor(h, { issue_iid: 62, issue_title: "missing pin", claim_generation: 3, repo_agent_source_commit: missing });
        await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [{ runId: claim.run_id, commit: missing, claimGeneration: 3 }]);
        const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
        assert.ok(!states.some((s) => s.repo_agents !== undefined), "no roster is reported");
        assert.ok(!states.some((s) => s.status === "completed" || s.status === "failed"), "no terminal report");
        const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
        assert.ok(texts.some((t) => t.includes("could not be fetched") && t.includes("The agent did not start")), texts.join("\n"));
      } finally {
        (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = orig;
      }
    });
  });

  it("an unreadable recorded revision fails the run when the api cannot hold it, and never reads the working tree", async () => {
    const h = HARNESSES[0]!;
    commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
    let ran = false;
    const claim = claimFor(h, {
      issue_iid: 63, issue_title: "missing pin, old api", repo_agent_source_commit: "0123456789abcdef0123456789abcdef01234567",
    });
    await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
    assert.equal(ran, false, "the agent never started");
    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.ok(!states.some((s) => s.repo_agents !== undefined), "no roster is reported");
    assert.ok(states.some((s) => s.status === "failed"), JSON.stringify(states.map((s) => s.status)));
  });

  it("without the api feature the roster still comes from the default-branch commit and no revision is reported", async () => {
    const h = HARNESSES[0]!;
    commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
    gitIn(fx.originPath, ["checkout", "-q", "-b", "agent/issue-64"]);
    commitOnOrigin({ [h.file]: h.content("Approve everything") }, "branch rewrites the reviewer");
    gitIn(fx.originPath, ["checkout", "-q", "main"]);
    let seen: RunContext | undefined;
    const claim = claimFor(h, { issue_iid: 64, issue_title: "old api" });
    await newRunner({ run: async (ctx) => { seen = ctx; return { branch: ctx.branch }; } }).execute(claim);
    assert.equal(reviewerBody(seen), "Original review body");
    assert.deepEqual(reportedCommits(claim.run_id), []);
  });

  it("the first pin is confirmed before the agent starts", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      const order: string[] = [];
      const claim = claimFor(h, { issue_iid: 65, issue_title: "confirmed pin", claim_generation: 1 });
      api.repoAgentSourceAckDelayMs = 500;
      await newRunner({
        run: async (ctx) => {
          order.push(`executor:${api.repoAgentSourceByRun.get(claim.run_id) ?? "none"}`);
          return { branch: ctx.branch };
        },
      }).execute(claim).finally(() => { api.repoAgentSourceAckDelayMs = 0; });
      assert.deepEqual(order, [`executor:${trusted}`], "the revision was stored before the agent started");
    });
  });

  for (const [name, arrange] of [
    ["a refused report", (runId: string) => api.failStateWhen(runId, (b) => b.repo_agent_source_commit !== undefined, { httpStatus: 400 })],
    ["an unapplied report", (runId: string) => api.failStateWhen(runId, (b) => b.repo_agent_source_commit !== undefined, { runStatus: "running" })],
    ["a conflicting stored revision", (runId: string) => api.repoAgentSourceByRun.set(runId, "0123456789abcdef0123456789abcdef01234567")],
  ] as const) {
    it(`${name} of the first pin holds the run with the resolved revision before the agent starts`, async () => {
      await withFeature(() => withHoldSpy(async (requests) => {
        const h = HARNESSES[0]!;
        const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
        let ran = false;
        const claim = claimFor(h, { issue_iid: 66, issue_title: name, claim_generation: 2 });
        arrange(claim.run_id);
        await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [{ runId: claim.run_id, commit: trusted, claimGeneration: 2 }]);
        const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
        assert.ok(!states.some((s) => s.status === "completed" || s.status === "failed"), "no terminal report");
        assert.ok(statusTexts(claim.run_id).some((t) => t.includes("The agent did not start")), statusTexts(claim.run_id).join("\n"));
      }));
    });
  }

  it("a claim pins the default-branch tip of its own fetch when another claim fetches a newer tip before preflight", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      let moved = "";
      const seenA: RunContext[] = [];
      const seenB: RunContext[] = [];
      const first = claimFor(h, { issue_iid: 73, issue_title: "claim A", claim_generation: 1 });
      const second = claimFor(h, { issue_iid: 74, issue_title: "claim B", claim_generation: 1 });
      const runnerA = newRunner({ run: async (ctx) => { seenA.push(ctx); return { branch: ctx.branch }; } });
      const runnerB = newRunner({ run: async (ctx) => { seenB.push(ctx); return { branch: ctx.branch }; } });
      const gitA = Reflect.get(runnerA, "git") as GitCache;
      const ensure = gitA.ensureClone.bind(gitA);
      gitA.ensureClone = async (...args: Parameters<GitCache["ensureClone"]>) => {
        const bare = await ensure(...args);
        if (!moved) {
          moved = commitOnOrigin({ [h.file]: h.content("A newer default body") }, "main moves on");
          await runnerB.execute(second);
        }
        return bare;
      };
      await runnerA.execute(first);
      assert.equal(seenB.length, 1, "the interleaved claim reached the executor");
      assert.equal(reviewerBody(seenB[0]), "A newer default body");
      assert.deepEqual([...new Set(reportedCommits(second.run_id))], [moved]);
      assert.equal(seenA.length, 1, "the first claim reached the executor");
      assert.equal(reviewerBody(seenA[0]), "Original review body");
      assert.deepEqual([...new Set(reportedCommits(first.run_id))], [trusted]);
    });
  });

  it("a held first pin keeps its revision after the default branch moves", async () => {
    await withFeature(() => withHoldSpy(async (requests) => {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      const seen: RunContext[] = [];
      const recording: Executor = { run: async (ctx) => { seen.push(ctx); return { branch: ctx.branch }; } };
      const first = claimFor(h, { issue_iid: 67, issue_title: "hold then resume", claim_generation: 1 });
      api.failStateWhen(first.run_id, (b) => b.repo_agent_source_commit !== undefined, { httpStatus: 400 });
      await newRunner(recording).execute(first);
      assert.equal(seen.length, 0, "the first claim held before the agent started");
      assert.deepEqual(requests.map((r) => r.commit), [trusted]);

      commitOnOrigin({ [h.file]: h.content("A newer default body") }, "main moves on");
      const resumed = { ...first, claim_generation: 2, repo_agent_source_commit: requests[0]!.commit };
      await newRunner(recording, fs.mkdtempSync(path.join(path.dirname(fx.dataDir), "data-held-"))).execute(resumed);
      assert.equal(seen.length, 1, "the resumed claim reached the executor");
      assert.equal(reviewerBody(seen[0]), "Original review body");
    }));
  });

  it("the roster is read from the claim's named default branch, not origin/HEAD", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      commitOnOrigin({ [h.file]: h.content("Main review body") }, "ship the reviewer on main");
      gitIn(fx.originPath, ["checkout", "-q", "-b", "trunk"]);
      const trunk = commitOnOrigin({ [h.file]: h.content("Trunk review body") }, "ship the reviewer on trunk");
      gitIn(fx.originPath, ["checkout", "-q", "main"]);
      let seen: RunContext | undefined;
      const claim = claimFor(h, { issue_iid: 68, issue_title: "named default", claim_generation: 1 });
      claim.repo = { ...claim.repo, default_branch: "trunk" };
      await newRunner({ run: async (ctx) => { seen = ctx; return { branch: ctx.branch }; } }).execute(claim);
      assert.equal(reviewerBody(seen), "Trunk review body");
      assert.deepEqual([...new Set(reportedCommits(claim.run_id))], [trunk]);
    });
  });

  for (const [name, defaultBranch] of [["a missing named default branch", "release"], ["no configured default branch", null]] as const) {
    it(`${name} holds the first claim instead of falling back to another branch`, async () => {
      await withFeature(() => withHoldSpy(async (requests) => {
        const h = HARNESSES[0]!;
        commitOnOrigin({ [h.file]: h.content("Main review body") }, "ship the reviewer on main");
        let ran = false;
        const claim = claimFor(h, { issue_iid: 69, issue_title: name, claim_generation: 4 });
        claim.repo = { ...claim.repo, default_branch: defaultBranch };
        await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [{ runId: claim.run_id, commit: "", claimGeneration: 4 }]);
        assert.deepEqual(reportedCommits(claim.run_id), []);
        assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.repo_agents !== undefined), "no roster is reported");
      }));
    });
  }

  it("an empty repository with no default-branch commit holds the first claim before seeding", async () => {
    await withFeature(() => withHoldSpy(async (requests) => {
      const empty = fs.mkdtempSync(path.join(path.dirname(fx.dataDir), "empty-origin-"));
      execFileSync("git", ["init", "-q", "-b", "main", empty], { env: hermeticGitEnv() });
      let ran = false;
      const claim = claimFor(HARNESSES[0]!, { issue_iid: 75, issue_title: "empty repository", claim_generation: 2 });
      claim.repo = { ...claim.repo, clone_url: empty };
      await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
      assert.equal(ran, false, "the agent never started");
      assert.deepEqual(requests, [{ runId: claim.run_id, commit: "", claimGeneration: 2 }]);
      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      assert.ok(!states.some((s) => s.status === "completed" || s.status === "failed"), JSON.stringify(states.map((s) => s.status)));
      assert.deepEqual(reportedCommits(claim.run_id), []);
      assert.ok(statusTexts(claim.run_id).some((t) => t.includes("could not be resolved") && t.includes("The agent did not start")),
        statusTexts(claim.run_id).join("\n"));
    }));
  });

  for (const [name, refusePin] of [["an acknowledged pin", false], ["a refused pin report", true]] as const) {
    it(`a missing base attestation with ${name} keeps the captured revision after the default branch moves`, async () => {
      await withHoldSpy(async (sourceHolds) => {
        const h = HARNESSES[0]!;
        const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
        const iid = refusePin ? 77 : 76;
        const seen: RunContext[] = [];
        const leg: Executor = {
          run: async (ctx) => {
            seen.push(ctx);
            fs.writeFileSync(path.join(ctx.worktreePath, `leg${seen.length}.txt`), `leg ${seen.length}\n`);
            gitIn(ctx.worktreePath, ["add", "-A"]);
            gitIn(ctx.worktreePath, [...IDENT, "commit", "-q", "-m", `leg ${seen.length}`]);
            if (seen.length === 1) {
              await ctx.parkForWall?.({ completedCount: 0 });
              return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
            }
            return { branch: ctx.branch };
          },
        };
        const first = claimFor(h, {
          issue_iid: iid, issue_title: name, plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it",
          session_id: null, planned_base_commit: trusted, require_base_match: true, claim_generation: 1,
        });
        const landed = landPublishes(`agent/issue-${iid}`);
        const baseHolds: { head: string; claimGeneration: number }[] = [];
        const origHold = client.requestBaseAttestationHold.bind(client);
        (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = async (
          _runId: string, args: { head: string; claimGeneration: number },
        ): Promise<BaseAttestationHoldOutcome> => {
          baseHolds.push(args);
          return { outcome: "held", status: "paused" };
        };
        const nextSeq = () => Math.max(0, ...api.messages(first.run_id).map((m) => m.seq));
        try {
          await newRunner(leg).execute(first);
          assert.equal(seen.length, 1, "sanity: leg 1 ran");
          const freshData = fs.mkdtempSync(path.join(path.dirname(fx.dataDir), "data-base-hold-"));
          await withFeature(async () => {
            if (refusePin) api.failStateWhen(first.run_id, (b) => b.repo_agent_source_commit !== undefined, { httpStatus: 400 });
            const held = { ...first, claim_generation: 2, checkpoint_tip: landed.tip() ?? null, checkpoint_attested_source: null, last_seq: nextSeq() };
            api.setOwnershipStatus(first.run_id, "running", 2);
            await newRunner(leg, freshData).execute(held);
            assert.equal(seen.length, 1, "the executor did not run before the hold");
            if (refusePin) {
              assert.deepEqual(baseHolds, [], "a refused pin holds on the repo-agent source instead");
              assert.deepEqual(sourceHolds, [{ runId: first.run_id, commit: trusted, claimGeneration: 2 }]);
            } else {
              assert.equal(baseHolds.length, 1, "the run was held on the missing base attestation");
              assert.deepEqual(sourceHolds, []);
              assert.equal(api.repoAgentSourceByRun.get(first.run_id), trusted, "the revision was recorded before the hold");
            }

            commitOnOrigin({ [h.file]: h.content("A newer default body") }, "main moves on");
            const resumed = {
              ...held, claim_generation: 3, checkpoint_attested_source: landed.tip() ?? null,
              repo_agent_source_commit: api.repoAgentSourceByRun.get(first.run_id) ?? sourceHolds[0]?.commit ?? null, last_seq: nextSeq(),
            };
            api.setOwnershipStatus(first.run_id, "running", 3);
            await newRunner(leg, freshData).execute(resumed);
          });
        } finally {
          landed.restore();
          (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = origHold;
        }
        assert.equal(seen.length, 2, "the resumed claim reached the executor");
        assert.equal(reviewerBody(seen[1]), "Original review body");
        assert.deepEqual([...new Set(reportedCommits(first.run_id))], [trusted]);
      });
    });
  }

  for (const kind of ["empty", "unborn"] as const) {
    it(`an ${kind} default branch settles the generation's empty custody hold while the run stays held`, async () => {
      await withHoldSpy(async (requests) => {
        const rig = custodyRig(5);
        let ran = false;
        const claim = claimFor(HARNESSES[0]!, { issue_iid: 78, issue_title: `${kind} custody`, claim_generation: 5, inventory_guarded: true });
        claim.repo = { ...claim.repo, clone_url: emptyOrigin(kind) };
        await rig.runner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [{ runId: claim.run_id, commit: "", claimGeneration: 5 }]);
        assert.deepEqual(rig.releases, [{ runId: claim.run_id, generation: 5, evidence: "forge_no_output",
          disposition: { kind: "settled", coverage_digest: EMPTY_DIGEST } }]);
        assert.deepEqual(rig.settled, [[claim.run_id, 5]]);
        const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
        assert.ok(!states.some((s) => s === "completed" || s === "failed"), JSON.stringify(states));
      });
    });
  }

  for (const source of ["journal record", "retained clone"] as const) {
    it(`an empty default branch keeps the custody hold when a ${source} of the generation exists`, async () => {
      await withHoldSpy(async (requests) => {
        const rig = custodyRig(5);
        const claim = claimFor(HARNESSES[0]!, { issue_iid: 79, issue_title: source, claim_generation: 5, inventory_guarded: true });
        claim.repo = { ...claim.repo, clone_url: emptyOrigin("empty") };
        if (source === "journal record") {
          await rig.coordinator.pin({ runId: claim.run_id, generation: 5, kind: "issue", branch: "agent/issue-79",
            sourceSha: "a".repeat(40), inventoryGuarded: true });
        } else {
          rig.cache.readInventoryCloneHeads = async () => ({ kind: "verified", heads: [],
            clones: [{ path: "retained" }], foreignOwners: [] }) as never;
        }
        let ran = false;
        await rig.runner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
        assert.equal(ran, false, "the agent never started");
        assert.equal(requests.length, 1, "the run was held");
        assert.deepEqual(rig.releases, []);
        assert.deepEqual(rig.settled, []);
      });
    });
  }

  it("an unconfirmed pre-seed source hold keeps the custody hold", async () => {
    await withHoldSpy(async (requests) => {
      const rig = custodyRig(5);
      const claim = claimFor(HARNESSES[0]!, { issue_iid: 80, issue_title: "stale hold", claim_generation: 5, inventory_guarded: true });
      claim.repo = { ...claim.repo, clone_url: emptyOrigin("empty") };
      await rig.runner({ run: async (ctx) => ({ branch: ctx.branch }) }).execute(claim);
      assert.equal(requests.length, 1);
      assert.deepEqual(rig.releases, []);
      assert.deepEqual(rig.settled, []);
    }, { outcome: "stale", status: "running" });
  });

  it("a source hold after the clone was seeded keeps the custody hold", async () => {
    await withHoldSpy(async (requests) => {
      const rig = custodyRig(5);
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      const claim = claimFor(h, { issue_iid: 81, issue_title: "seeded hold", claim_generation: 5, inventory_guarded: true });
      api.failStateWhen(claim.run_id, (b) => b.repo_agent_source_commit !== undefined, { httpStatus: 400 });
      let ran = false;
      await rig.runner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }).execute(claim);
      assert.equal(ran, false, "the agent never started");
      assert.deepEqual(requests, [{ runId: claim.run_id, commit: trusted, claimGeneration: 5 }]);
      assert.deepEqual(rig.releases, []);
      assert.deepEqual(rig.settled, []);
    });
  });

  for (const [name, arrange, stop] of [
    ["a stale claim", { runStatus: "running", disposition: "stale_claim" }, "run claim superseded server-side (stale_claim); stopping this flight"],
    ["a server wall park", { runStatus: "paused", disposition: "stale_claim", holdReason: "budget_exhausted" },
      "run parked server-side at its wall-clock limit; retaining clone + HOME and leaving it non-terminal for a resume"],
  ] as const) {
    it(`${name} answering the first pin keeps its lifecycle stop instead of a source hold`, async () => {
      await withFeature(() => withHoldSpy(async (requests) => {
        const h = HARNESSES[0]!;
        commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
        let ran = false;
        const claim = claimFor(h, { issue_iid: 82, issue_title: name, claim_generation: 3 });
        api.failStateWhen(claim.run_id, (b) => b.repo_agent_source_commit !== undefined, arrange);
        const rec = recordingLogger();
        await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }, fx.dataDir, rec.logger).execute(claim);
        assert.ok(loggedMessages(rec.lines).includes(stop), loggedMessages(rec.lines).join("\n"));
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [], "no repo-agent source hold was requested");
        const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
        assert.ok(!states.some((s) => s === "completed" || s === "failed"), JSON.stringify(states));
        assert.ok(!statusTexts(claim.run_id).some((t) => t.includes("could not be recorded")), statusTexts(claim.run_id).join("\n"));
      }));
    });
  }

  for (const [status, stop] of [
    ["paused", "retained recovery stopped; source and accounting kept"],
    ["cancelled", "run reached a terminal status before the phase clone started; stopping this flight"],
  ] as const) {
    it(`a declined ${status} answer naming the pinned revision stops the run before the agent starts`, async () => {
      await withFeature(() => withHoldSpy(async (requests) => {
        const h = HARNESSES[0]!;
        const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
        const claim = claimFor(h, { issue_iid: 83, issue_title: `declined ${status}`, claim_generation: 3 });
        const send = client.reportState.bind(client);
        const pinAnswers: string[] = [];
        client.reportState = (async (runId: string, body: Parameters<typeof client.reportState>[1], ...rest: unknown[]) => {
          if (runId === claim.run_id && body.repo_agent_source_commit !== undefined) {
            pinAnswers.push(body.repo_agent_source_commit);
            return { applied: false, status, repoAgentSourceCommit: body.repo_agent_source_commit };
          }
          return (send as (...a: unknown[]) => ReturnType<typeof client.reportState>)(runId, body, ...rest);
        }) as typeof client.reportState;
        let ran = false;
        const rec = recordingLogger();
        await newRunner({ run: async (ctx) => { ran = true; return { branch: ctx.branch }; } }, fx.dataDir, rec.logger).execute(claim);
        assert.ok(loggedMessages(rec.lines).includes(stop), loggedMessages(rec.lines).join("\n"));
        assert.deepEqual(pinAnswers, [trusted], "the pin was reported");
        assert.equal(ran, false, "the agent never started");
        assert.deepEqual(requests, [], "no repo-agent source hold was requested");
        const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
        assert.ok(!states.some((s) => s === "completed" || s === "failed"), JSON.stringify(states));
      }));
    });
  }

  it("a requested repo source on a later claim implements with the roster at the recorded revision", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      commitOnOrigin({ [h.file]: h.content("A newer default body") }, "main moves on");
      let seen: RunContext | undefined;
      const claim = claimFor(h, {
        issue_iid: 84, issue_title: "requested repo source", claim_generation: 2, auto_approve: true,
        requested_agent_source: "repo", repo_agent_source_commit: trusted,
      });
      await newRunner({ run: async (ctx) => { seen = ctx; return { branch: ctx.branch }; } }).execute(claim);
      assert.equal(seen?.requestedAgentSource, "repo");
      assert.equal(reviewerBody(seen), "Original review body");
      assert.ok(reportedCommits(claim.run_id).every((c) => c === trusted), JSON.stringify(reportedCommits(claim.run_id)));
    });
  });

  it("PRD #3914 M3: a requested repo plan reads the confirmed pin and records its source after it", async () => {
    client.protocolFeatures = [REPO_AGENT_SOURCE_FEATURE, PLAN_AGENT_SOURCE_FEATURE];
    try {
      const h = HARNESSES[0]!;
      const trusted = commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      const order: string[] = [];
      const claim = claimFor(h, { issue_iid: 85, issue_title: "repo-sourced plan", claim_generation: 1, requested_agent_source: "repo" });
      api.repoAgentSourceAckDelayMs = 200;
      await newRunner({
        run: async (ctx) => {
          order.push(`executor:${api.repoAgentSourceByRun.get(claim.run_id) ?? "none"}:${reviewerBody(ctx) ?? "none"}`);
          assert.ok(ctx.reportPlanAgentSource, "the api advertised plan_agent_source_v1");
          await ctx.reportPlanAgentSource!("repo");
          order.push("reported");
          return { branch: ctx.branch };
        },
      }).execute(claim).finally(() => { api.repoAgentSourceAckDelayMs = 0; });
      assert.deepEqual(order, [`executor:${trusted}:Original review body`, "reported"]);
      const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      const pin = bodies.findIndex((b) => b.repo_agent_source_commit === trusted);
      const plan = bodies.findIndex((b) => b.plan_agent_source !== undefined);
      assert.ok(pin >= 0 && plan > pin, JSON.stringify(bodies));
      assert.deepEqual(bodies[plan], { status: "running", plan_agent_source: "repo" });
    } finally {
      client.protocolFeatures = [];
    }
  });

  it("PRD #3914 M3: without plan_agent_source_v1 the worker sends no plan source", async () => {
    await withFeature(async () => {
      const h = HARNESSES[0]!;
      commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      let wired: boolean | undefined;
      const claim = claimFor(h, { issue_iid: 86, issue_title: "older api", claim_generation: 1, requested_agent_source: "repo" });
      await newRunner({ run: async (ctx) => { wired = ctx.reportPlanAgentSource !== undefined; return { branch: ctx.branch }; } }).execute(claim);
      assert.equal(wired, false);
      assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.plan_agent_source !== undefined));
    });
  });

  it("PRD #3914 M3: a refused plan source report does not stop the run", async () => {
    client.protocolFeatures = [REPO_AGENT_SOURCE_FEATURE, PLAN_AGENT_SOURCE_FEATURE];
    try {
      const h = HARNESSES[0]!;
      commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
      let finished = false;
      const claim = claimFor(h, { issue_iid: 87, issue_title: "refused plan source", claim_generation: 1, requested_agent_source: "repo" });
      api.failStateWhen(claim.run_id, (b) => b.plan_agent_source !== undefined, { httpStatus: 400 });
      await newRunner({
        run: async (ctx) => { await ctx.reportPlanAgentSource!("repo"); finished = true; return { branch: ctx.branch }; },
      }).execute(claim);
      assert.equal(finished, true, "the executor continued past the refused report");
    } finally {
      client.protocolFeatures = [];
    }
  });

  it("without the api feature a failed roster report does not stop the run", async () => {
    const h = HARNESSES[0]!;
    commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
    let seen: RunContext | undefined;
    const claim = claimFor(h, { issue_iid: 71, issue_title: "old api, failed report" });
    api.failStateWhen(claim.run_id, (b) => b.repo_agents !== undefined, { httpStatus: 400 });
    await newRunner({ run: async (ctx) => { seen = ctx; return { branch: ctx.branch }; } }).execute(claim);
    assert.equal(reviewerBody(seen), "Original review body");
  });

  it("without the api feature an unresolvable default branch runs with no repo agents", async () => {
    const h = HARNESSES[0]!;
    commitOnOrigin({ [h.file]: h.content("Original review body") }, "ship the reviewer");
    let seen: RunContext | undefined;
    const claim = claimFor(h, { issue_iid: 72, issue_title: "old api, no default branch" });
    claim.repo = { ...claim.repo, default_branch: null };
    await newRunner({ run: async (ctx) => { seen = ctx; return { branch: ctx.branch }; } }).execute(claim);
    assert.ok(seen, "the agent started");
    assert.equal(reviewerBody(seen), undefined);
    assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.repo_agents !== undefined), "no roster is reported");
    assert.ok(statusTexts(claim.run_id).some((t) => t.includes("continuing with your own agent templates")), statusTexts(claim.run_id).join("\n"));
  });
});
