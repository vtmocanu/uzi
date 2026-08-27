import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeFixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
import {
  api,
  client,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runner,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

describe("RunRunner — repo agent detection (PRD #37)", () => {
  /** Run one claim against an origin carrying `files`, returning the state reports
   *  and the status messages that reached the stream. */
  async function runAgainst(files: Record<string, string>) {
    const repoFx = makeFixture(files);
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 31,
        issue_title: "detect the roster",
        repo: {
          id: "r1",
          url: "https://gitlab.example.test/org/repo",
          clone_url: repoFx.originPath,
        },
        last_seq: 0,
        secrets: {
          forge_pat: "fixture-forge-pat-000000",
          anthropic_oauth_token: "dummy-oauth-do-not-scan",
        },
      });
      const repoRunner = new RunRunner(
        client,
        new GitCache(repoFx.dataDir, nullLogger()),
        () => ({ executor: new StubExecutor(nullLogger()) }),
        nullLogger(),
        20,
        undefined,
        {
          pollMs: 5,
          planApprovalTimeoutMs: 0,
          gitlab,
        },
      );
      await repoRunner.execute(claim);
      return {
        states: api.states
          .filter((s) => s.runId === claim.run_id)
          .map((s) => s.body),
        texts: api
          .messages(claim.run_id)
          .filter((m) => m.kind === "status")
          .map((m) => String(m.payload.text)),
      };
    } finally {
      repoFx.cleanup();
    }
  }

  it("reports the parsed roster on a running report, noting every drop", async () => {
    const { states, texts } = await runAgainst({
      ".claude/agents/coder.md":
        "---\nname: coder\ndescription: Implements changes.\nmodel: opus\n---\n\nImplement it.\n",
      ".claude/agents/reviewer.md":
        "---\nname: reviewer\ndescription: Reviews changes.\ntools: Read, WebFetch\n---\n\nReview it.\n",
      ".claude/agents/broken.md": "not an agent file\n",
      // Never loaded through this path: only .claude/agents/*.md is read.
      ".claude/settings.json": '{"permissions":{"allow":["Bash(rm -rf /)"]}}',
    });

    const report = states.find((s) => s.repo_agents !== undefined);
    assert.ok(report, "a state report carries the roster");
    assert.strictEqual(
      report!.status,
      "running",
      "the roster rides a running report, not the gate",
    );
    assert.deepStrictEqual(report!.repo_agents, [
      { name: "coder", description: "Implements changes." },
      { name: "reviewer", description: "Reviews changes." },
    ]);
    // Prompt bodies stay worker-side; only names + descriptions travel.
    assert.ok(!JSON.stringify(report!.repo_agents).includes("Implement it."));

    assert.ok(
      texts.some((t) => t.includes('repo agent "broken" was skipped')),
      texts.join("\n"),
    );
    // WebFetch is HONORED now — reviewer keeps it, so NO tools_filtered note fires.
    assert.ok(
      !texts.some((t) => t.includes("removed WebFetch")),
      texts.join("\n"),
    );
    assert.ok(
      texts.some((t) => t.includes("detected 2 agent(s)")),
      texts.join("\n"),
    );
  });

  it("reports an empty roster (not an absent one) for a repo with no .claude/agents", async () => {
    const { states, texts } = await runAgainst({});
    const report = states.find((s) => s.repo_agents !== undefined);
    // `[]` is "detection ran, found none" — distinct from a pre-feature run's NULL.
    assert.deepStrictEqual(report?.repo_agents, []);
    assert.ok(
      !texts.some((t) => t.includes("repo agent")),
      "no notes when there is nothing to detect",
    );
    assert.ok(
      states.some((s) => s.status === "completed"),
      "the run still completes",
    );
  });

  it("keeps a detection FAILURE distinguishable from an empty roster (no repo_agents reported)", async () => {
    // A detection throw (e.g. an unreadable dir) must NOT be reported as `[]` (which
    // means "scanned, found none"). The worker sends no repo_agents at all, so the
    // column stays NULL, and it says so on the feed. The run still completes.
    const repoFx = makeFixture({});
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 32,
        issue_title: "detection fails",
        repo: {
          id: "r1",
          url: "https://gitlab.example.test/org/repo",
          clone_url: repoFx.originPath,
        },
        last_seq: 0,
        secrets: {
          forge_pat: "fixture-forge-pat-000000",
          anthropic_oauth_token: "dummy-oauth-do-not-scan",
        },
      });
      const runner = new RunRunner(
        client,
        new GitCache(repoFx.dataDir, nullLogger()),
        () => ({ executor: new StubExecutor(nullLogger()) }),
        nullLogger(),
        20,
        undefined,
        {
          pollMs: 5,
          planApprovalTimeoutMs: 0,
          gitlab,
          detectRepoAgents: async () => {
            throw new Error("enumeration failed");
          },
        },
      );
      await runner.execute(claim);

      const states = api.states
        .filter((s) => s.runId === claim.run_id)
        .map((s) => s.body);
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      // NO report carries repo_agents — not even `[]`. The column stays "not reported".
      assert.ok(
        !states.some((s) => s.repo_agents !== undefined),
        "a detection failure reports no roster",
      );
      assert.ok(
        texts.some((t) =>
          t.includes("could not read the repo's .claude/agents/"),
        ),
        texts.join("\n"),
      );
      assert.ok(
        states.some((s) => s.status === "completed"),
        "the run still completes",
      );
    } finally {
      repoFx.cleanup();
    }
  });

  it("autopilot resolves + reports the repo-agent default selection with a feed note (PRD #37)", async () => {
    // A repo shipping agents + an autopilot claim: the self-approve path resolves
    // the default to the repo source, reports it on a running report (the only
    // channel a no-input run has), and states it on the feed — never parking at the
    // gate.
    const repoFx = makeFixture({
      ".claude/agents/coder.md":
        "---\nname: coder\ndescription: c.\n---\n\nbody\n",
      ".claude/agents/reviewer.md":
        "---\nname: reviewer\ndescription: r.\n---\n\nbody\n",
    });
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 33,
        issue_title: "autopilot with repo agents",
        repo: {
          id: "r1",
          url: "https://gitlab.example.test/org/repo",
          clone_url: repoFx.originPath,
        },
        last_seq: 0,
        secrets: {
          forge_pat: "fixture-forge-pat-000000",
          anthropic_oauth_token: "dummy-oauth-do-not-scan",
        },
        auto_approve: true,
      });
      const r = new RunRunner(
        client,
        new GitCache(repoFx.dataDir, nullLogger()),
        () => ({
          executor: new StubExecutor(nullLogger(), { planGate: true }),
        }),
        nullLogger(),
        20,
        undefined,
        { pollMs: 5, planApprovalTimeoutMs: 0, gitlab },
      );
      await r.execute(claim);

      const states = api.states
        .filter((s) => s.runId === claim.run_id)
        .map((s) => s.body);
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      const selectionState = states.find(
        (s) => s.agent_selection !== undefined,
      );
      assert.deepStrictEqual(
        selectionState?.agent_selection,
        { source: "repo", exclusions: [] },
        "autopilot persisted the repo default",
      );
      // F1: the autopilot selection report is self-contained — it carries the roster
      // alongside the selection, so a failed fire-and-forget roster report above does
      // not cost the attribution. (The own-source case below carries NO repo_agents.)
      assert.deepStrictEqual(
        selectionState?.repo_agents?.map((a) => a.name).sort(),
        ["coder", "reviewer"],
        "the repo-source autopilot selection report carries repo_agents",
      );
      assert.ok(
        texts.some((t) =>
          t.includes(
            "autopilot: using the 2 agent(s) from the repo's .claude/agents/",
          ),
        ),
        texts.join("\n"),
      );
      assert.ok(
        !states.some((s) => s.status === "awaiting_approval"),
        "autopilot never parks at the gate",
      );
      assert.ok(
        states.some((s) => s.status === "completed"),
        "the run still completes",
      );
    } finally {
      repoFx.cleanup();
    }
  });

  it("autopilot with no repo agents resolves + reports the OWN default", async () => {
    const { states, texts } = await (async () => {
      const repoFx = makeFixture({});
      try {
        const { gitlab } = fakeGitlab();
        const claim = makeClaim({
          issue_iid: 36,
          issue_title: "autopilot no repo agents",
          repo: {
            id: "r1",
            url: "https://gitlab.example.test/org/repo",
            clone_url: repoFx.originPath,
          },
          last_seq: 0,
          secrets: {
            forge_pat: "fixture-forge-pat-000000",
            anthropic_oauth_token: "dummy-oauth-do-not-scan",
          },
          auto_approve: true,
        });
        const r = new RunRunner(
          client,
          new GitCache(repoFx.dataDir, nullLogger()),
          () => ({
            executor: new StubExecutor(nullLogger(), { planGate: true }),
          }),
          nullLogger(),
          20,
          undefined,
          { pollMs: 5, planApprovalTimeoutMs: 0, gitlab },
        );
        await r.execute(claim);
        return {
          states: api.states
            .filter((s) => s.runId === claim.run_id)
            .map((s) => s.body),
          texts: api
            .messages(claim.run_id)
            .filter((m) => m.kind === "status")
            .map((m) => String(m.payload.text)),
        };
      } finally {
        repoFx.cleanup();
      }
    })();
    const ownSelectionState = states.find(
      (s) => s.agent_selection !== undefined,
    );
    assert.deepStrictEqual(ownSelectionState?.agent_selection, {
      source: "own",
      exclusions: [],
    });
    // F1 guard: on the own default (no repo agents detected) the report must NOT carry
    // repo_agents — sending [] would flip the column from NULL ("not reported") to []
    // ("detected none") and erase that distinction.
    assert.strictEqual(
      ownSelectionState?.repo_agents,
      undefined,
      "the own-source autopilot report carries no repo_agents",
    );
    assert.ok(
      texts.some((t) =>
        t.includes("autopilot: using your own agent templates"),
      ),
      texts.join("\n"),
    );
  });

  it("the MR description carries the repo-agents marker only when the run used repo agents", async () => {
    // A fake executor that reports which roster the implement phase ran with — the
    // stub does not, so the marker is driven directly here (the SDK executor sets it).
    // These inline executors commit nothing; model committed work so both runs reach the
    // push+MR past the issue-run zero-diff guard (issue #279).
    simulateCommittedWork();
    const repoExec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        agentSelection: { source: "repo", agents: ["coder", "auditor"] },
      }),
    };
    const ownExec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        agentSelection: { source: "own", agents: ["coder"] },
      }),
    };

    const repoGl = fakeGitlab();
    await runner(repoExec, repoGl.gitlab).execute(gitlabClaim(34));
    const repoBody = JSON.parse(repoGl.calls[0]!.body ?? "{}");
    assert.match(
      repoBody.description,
      /repository's own `\.claude\/agents\/`/,
      "repo-source MR carries the marker",
    );
    assert.match(
      repoBody.description,
      /coder, auditor/,
      "the marker names the roster",
    );

    const ownGl = fakeGitlab();
    await runner(ownExec, ownGl.gitlab).execute(gitlabClaim(35));
    const ownBody = JSON.parse(ownGl.calls[0]!.body ?? "{}");
    assert.ok(
      !/repository's own/.test(ownBody.description),
      "an own-source MR has no repo marker",
    );
  });
});
