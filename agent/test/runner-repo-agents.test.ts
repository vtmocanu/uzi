import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeFixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { detectRepoAgents } from "../src/repoagents.js";
import { RunRunner } from "../src/runner.js";
import { realProcfsSkip } from "./real-procfs.js";

const HAS_PROCFS = process.platform === "linux";
const repoAgentReadSkip = (label: string): string | false =>
  !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip(`runner repoagents: ${label}`);
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
        new GitCache(repoFx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
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

  it("reports the parsed roster on a running report, noting every drop", { skip: repoAgentReadSkip("parsed roster") }, async () => {
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
        new GitCache(repoFx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
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

  it("autopilot resolves + reports the repo-agent default selection with a feed note (PRD #37)", { skip: repoAgentReadSkip("autopilot repo selection") }, async () => {
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
        new GitCache(repoFx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
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
          new GitCache(repoFx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
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

  describe("second source: .codex/agents (issue #2085)", { skip: repoAgentReadSkip("second source") }, () => {
    const CODER_MD = "---\nname: claude-coder\ndescription: From claude.\n---\n\nbody\n";
    const CODER_TOML =
      'name = "codex-coder"\ndescription = "From codex."\ndeveloper_instructions = "Do it."\n';
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

    async function runWith(
      files: Record<string, string>,
      o: { codex?: boolean; features?: string[]; auto?: boolean; executor?: () => Executor; detect?: RunnerDetect } = {},
    ) {
      const repoFx = makeFixture(files);
      try {
        client.protocolFeatures = o.features ?? [];
        const { gitlab } = fakeGitlab();
        const claim = makeClaim({
          issue_iid: 41,
          issue_title: "roster source",
          repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: repoFx.originPath },
          last_seq: 0,
          secrets: o.codex
            ? CODEX_SECRETS
            : { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
          ...(o.auto ? { auto_approve: true } : {}),
        });
        const r = new RunRunner(
          client,
          new GitCache(repoFx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() })),
          () => ({ executor: o.executor?.() ?? new StubExecutor(nullLogger(), o.auto ? { planGate: true } : undefined) }),
          nullLogger(),
          20,
          undefined,
          { pollMs: 5, planApprovalTimeoutMs: 0, gitlab, ...(o.detect ? { detectRepoAgents: o.detect } : {}) },
        );
        await r.execute(claim);
        return {
          states: api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body),
          texts: api
            .messages(claim.run_id)
            .filter((m) => m.kind === "status")
            .map((m) => String(m.payload.text)),
        };
      } finally {
        client.protocolFeatures = [];
        repoFx.cleanup();
      }
    }
    type RunnerDetect = NonNullable<ConstructorParameters<typeof RunRunner>[6]>["detectRepoAgents"];

    it("reports a .codex/agents roster on a Claude claim when .claude/agents is absent, naming the folder", async () => {
      const { states, texts } = await runWith({ ".codex/agents/c.toml": CODER_TOML });
      const report = states.find((s) => s.repo_agents !== undefined);
      assert.deepStrictEqual(report?.repo_agents, [{ name: "codex-coder", description: "From codex." }]);
      assert.ok(texts.some((t) => t.includes("detected 1 agent(s) in the repo's .codex/agents/: codex-coder")), texts.join("\n"));
    });

    it("a Codex claim uses .codex/agents when both folders exist; a Claude claim uses .claude/agents", async () => {
      const files = { ".claude/agents/c.md": CODER_MD, ".codex/agents/c.toml": CODER_TOML };
      const codex = await runWith(files, { codex: true });
      assert.deepStrictEqual(
        codex.states.find((s) => s.repo_agents !== undefined)?.repo_agents?.map((a) => a.name),
        ["codex-coder"],
      );
      assert.ok(codex.texts.some((t) => t.includes("agent(s) in the repo's .codex/agents/: codex-coder")));
      const claude = await runWith(files);
      assert.deepStrictEqual(
        claude.states.find((s) => s.repo_agents !== undefined)?.repo_agents?.map((a) => a.name),
        ["claude-coder"],
      );
      assert.ok(claude.texts.some((t) => t.includes("agent(s) in the repo's .claude/agents/: claude-coder")));
    });

    it("without the repo_agent_folder feature no report carries a folder key (preflight and autopilot)", async () => {
      const { states } = await runWith({ ".codex/agents/c.toml": CODER_TOML }, { auto: true, features: [] });
      const reports = states.filter((s) => s.repo_agents !== undefined);
      assert.ok(reports.length >= 2, "both the preflight and the autopilot report carry the roster");
      for (const rep of reports) {
        for (const a of rep.repo_agents!) assert.ok(!("folder" in a), JSON.stringify(a));
      }
      assert.ok(!JSON.stringify(reports).includes("folder"));
    });

    it("with the repo_agent_folder feature both reports carry the folder", async () => {
      const { states, texts } = await runWith(
        { ".codex/agents/c.toml": CODER_TOML },
        { auto: true, features: ["repo_agent_folder"] },
      );
      const reports = states.filter((s) => s.repo_agents !== undefined);
      assert.ok(reports.some((r) => r.status === "running" && r.agent_selection === undefined), "preflight report");
      assert.ok(reports.some((r) => r.agent_selection !== undefined), "autopilot report");
      for (const rep of reports) {
        assert.deepStrictEqual(rep.repo_agents, [
          { name: "codex-coder", description: "From codex.", folder: ".codex/agents" },
        ]);
      }
      assert.ok(texts.some((t) => t.includes("autopilot: using the 1 agent(s) from the repo's .codex/agents/")), texts.join("\n"));
    });

    it("a Codex claim on a repo with only .claude/agents reports that folder with the feature on", async () => {
      const { states } = await runWith(
        { ".claude/agents/c.md": CODER_MD },
        { codex: true, auto: true, features: ["repo_agent_folder"] },
      );
      const reports = states.filter((s) => s.repo_agents !== undefined);
      assert.ok(reports.length >= 2, "preflight and autopilot reports");
      for (const rep of reports) {
        assert.deepStrictEqual(rep.repo_agents, [{ name: "claude-coder", description: "From claude.", folder: ".claude/agents" }]);
      }
    });

    it("detection completes before the executor's run is entered", async () => {
      const order: string[] = [];
      await runWith(
        { ".codex/agents/c.toml": CODER_TOML },
        {
          detect: async (p, h) => {
            const r = await detectRepoAgents(p, h);
            // Record completion, after a delay, so a runner that did not await
            // detection would enter the executor's run first.
            await new Promise((resolve) => setTimeout(resolve, 25));
            order.push("detect");
            return r;
          },
          executor: () => ({
            run: async (ctx) => {
              order.push("run");
              return { branch: ctx.branch };
            },
          }),
        },
      );
      assert.deepStrictEqual(order, ["detect", "run"]);
    });

    it("the failure line names both folders", async () => {
      const { texts } = await runWith({}, {
        detect: async () => {
          throw new Error("boom");
        },
      });
      assert.ok(
        texts.some((t) => t.includes("could not read the repo's .claude/agents/ or .codex/agents/; continuing with your own agent templates")),
        texts.join("\n"),
      );
    });
  });

  it("the MR description carries the repo-agents note only when the run used repo agents", async () => {
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
    // PRD #1798 (user decision): one short agents line in the completion block, no agent list.
    assert.ok(
      repoBody.description.includes(
        "Internally reviewed by the repository's own agents, not uzi's built-in reviewer.",
      ),
      "repo-source MR carries the one-line agents note",
    );
    assert.doesNotMatch(
      repoBody.description,
      /coder|auditor/,
      "the note does not list the roster",
    );

    const ownGl = fakeGitlab();
    await runner(ownExec, ownGl.gitlab).execute(gitlabClaim(35));
    const ownBody = JSON.parse(ownGl.calls[0]!.body ?? "{}");
    assert.ok(
      !/repository's own/.test(ownBody.description),
      "an own-source MR has no repo-agents note",
    );
  });
});
