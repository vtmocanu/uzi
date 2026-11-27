import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { recordingLogger } from "./helpers.js";
import type { Logger } from "../src/log.js";
import { RunRunner } from "../src/runner.js";
import { REDACTION_WITHHELD } from "../src/redact.js";
import { REASON_SKILLS_PLUGIN_LOAD_FAILED } from "../src/plugin-errors.js";
import type { GitHubClient } from "../src/forge.js";
import type { ClaimRunEnvVar } from "../src/protocol.js";
import { api, client, fakeGitHub, fx, git, gitlabClaim, installHarness } from "./runner-harness.js";

installHarness();

// PRD 4416, owner decision B3, through the runner's real terminal path: a redaction that passes its work budget
// withholds the affected text, and the state report keeps its status, run id and typed fail_origin.

const WORKFLOW = ".github/workflows/main-guard.yml";
// A 32 KiB literal registered as the run's forge token and 64 KiB of periodic text: the periodic worst case.
const LONG = "a".repeat(32 * 1024 - 1) + "b";
const PERIODIC = "a".repeat(64 * 1024);

const repo = () => ({ id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "github" }) as const;
const claimFor = (iid: number) => gitlabClaim(iid, {
  repo: repo(),
  secrets: { forge_pat: LONG, anthropic_oauth_token: "dummy-oauth-do-not-scan" },
});
// The same literal as an in-budget run_env secret claim (its registered forms pass the form budget).
const RUN_ENV_VAR: ClaimRunEnvVar = { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "BIG_SECRET", value: LONG, secret: true, description: "" };
const runEnvClaimFor = (iid: number) => gitlabClaim(iid, {
  repo: repo(),
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: [RUN_ENV_VAR] },
});

function runnerWith(github: GitHubClient, run: (ctx: { worktreePath: string; branch: string }) => Promise<unknown>, log: Logger): RunRunner {
  return new RunRunner(
    client, git,
    () => ({ executor: { run: async (ctx) => run(ctx) as never } }),
    log, 20, undefined, { pollMs: 5, planApprovalTimeoutMs: 0, github },
  );
}

describe("the work guard through the runner's terminal path", () => {
  it("state report: an over-budget failure_reason is withheld; status, run id and fail_origin stay intact", async () => {
    const { github } = fakeGitHub();
    const claim = claimFor(46301);
    const { logger } = recordingLogger();
    const runner = runnerWith(github, async () => {
      throw new Error(`${REASON_SKILLS_PLUGIN_LOAD_FAILED}: ${PERIODIC}`);
    }, logger);
    await runner.execute(claim);
    const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.equal(body.status, "failed");
    assert.equal(body.failure_reason, REDACTION_WITHHELD);
    assert.equal(body.fail_origin, "skills_plugin_load_failed", "classified from the raw reason, before redaction");
    assert.ok(!JSON.stringify(body).includes("aaaaaaaa"));
  });

  it("state report with the literal as a run_env claim: withheld, typed origin kept", async () => {
    const { github } = fakeGitHub();
    const claim = runEnvClaimFor(46303);
    const { logger } = recordingLogger();
    const runner = runnerWith(github, async () => {
      throw new Error(`${REASON_SKILLS_PLUGIN_LOAD_FAILED}: ${PERIODIC}`);
    }, logger);
    await runner.execute(claim);
    const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.equal(body.failure_reason, REDACTION_WITHHELD);
    assert.equal(body.fail_origin, "skills_plugin_load_failed");
    assert.ok(!JSON.stringify(body).includes("aaaaaaaa"));
  });

  it("preserved patch: a patch whose redaction passes the budget is not shipped", async () => {
    const { github } = fakeGitHub();
    git.pushBranch = (async () => undefined) as typeof git.pushBranch;
    git.branchWorkflowFiles = (async () => [WORKFLOW]) as typeof git.branchWorkflowFiles;
    const header = `diff --git a/${WORKFLOW} b/${WORKFLOW}\nindex 1111111..2222222 100644\n--- a/${WORKFLOW}\n+++ b/${WORKFLOW}\n@@ -0,0 +1 @@\n`;
    git.workflowScopeDiff = (async () => `${header}+${PERIODIC}\n`) as typeof git.workflowScopeDiff;
    const claim = claimFor(46302);
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(github, async (ctx) => {
      const file = path.join(ctx.worktreePath, WORKFLOW);
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, "name: real branch workflow\n");
      const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
      execFileSync("git", ["-C", ctx.worktreePath, "add", "--", WORKFLOW], { env });
      execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-m", "real workflow edit"], { env });
      return { branch: ctx.branch };
    }, logger);
    await runner.execute(claim);
    const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.equal(body.fail_origin, "workflow_scope_missing");
    assert.equal(body.preserved_patch, undefined);
    assert.ok((lines as Array<Record<string, unknown>>).some((l) => l.msg === "preserved patch withheld: redaction work budget exceeded"));
  });
});
