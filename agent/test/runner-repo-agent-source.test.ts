import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import type { Executor, RunContext } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
import type { BaseAttestationHoldOutcome, ClaimResponse } from "../src/protocol.js";
import { REPO_AGENT_SOURCE_FEATURE } from "../src/protocol.js";
import { api, client, fakeGitlab, fx, installHarness } from "./runner-harness.js";

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();

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

function newRunner(executor: Executor, dataDir = fx.dataDir): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(
    client,
    new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
    () => ({ executor }),
    nullLogger(),
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

async function withHoldSpy<T>(fn: (requests: HoldRequest[]) => Promise<T>): Promise<T> {
  const requests: HoldRequest[] = [];
  const orig = client.requestRepoAgentSourceHold.bind(client);
  (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = async (
    runId: string, args: { commit: string; claimGeneration: number },
  ): Promise<BaseAttestationHoldOutcome> => {
    requests.push({ runId, ...args });
    return { outcome: "held", status: "paused" };
  };
  try {
    return await fn(requests);
  } finally {
    (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = orig;
  }
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
