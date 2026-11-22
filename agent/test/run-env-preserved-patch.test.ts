import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { capReviewDiff } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import type { GitHubClient } from "../src/forge.js";
import type { ClaimRunEnvVar } from "../src/protocol.js";
import { api, client, fakeGitHub, fx, git, gitlabClaim, installHarness } from "./runner-harness.js";

installHarness();

// PRD 4416 M2: a preserved patch is searched for the run's secret values on the whole diff, before the byte cap
// that bounds what is stored. Dummy value only.

const SECRET = "dummy-secret-run-value-000000";
const WORKFLOW = ".github/workflows/main-guard.yml";
const var1: ClaimRunEnvVar = { var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "DB_PASSWORD", value: SECRET, secret: true, description: "" };

function githubRunner(github: GitHubClient): RunRunner {
  return new RunRunner(
    client, git,
    () => ({ executor: {
      run: async (ctx) => {
        const file = path.join(ctx.worktreePath, WORKFLOW);
        fs.mkdirSync(path.dirname(file), { recursive: true });
        fs.writeFileSync(file, "name: real branch workflow\n");
        const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
        execFileSync("git", ["-C", ctx.worktreePath, "add", "--", WORKFLOW], { env });
        execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-m", "real workflow edit"], { env });
        return { branch: ctx.branch };
      },
    } }),
    nullLogger(), 20, undefined, { pollMs: 5, planApprovalTimeoutMs: 0, github },
  );
}

const claimFor = (iid: number) => gitlabClaim(iid, {
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "github" },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: [var1] },
});

/** What the git layer returns: the whole diff, or the capped one unless the caller asks for it untruncated. */
const diffOf = (full: string) => (async (_bare: string, _ref: string, opts?: { untruncated?: boolean }) =>
  opts?.untruncated === true ? full : capReviewDiff(full)) as typeof git.workflowScopeDiff;
const header = `diff --git a/${WORKFLOW} b/${WORKFLOW}\n`;
const filler = "+".concat("y".repeat(79), "\n").repeat(8000);

async function failedReport(iid: number, full: string): Promise<{ fail_origin?: string; preserved_patch?: string }> {
  const { github } = fakeGitHub();
  git.pushBranch = (async () => undefined) as typeof git.pushBranch;
  git.branchWorkflowFiles = (async () => [WORKFLOW]) as typeof git.branchWorkflowFiles;
  git.workflowScopeDiff = diffOf(full);
  const claim = claimFor(iid);
  await githubRunner(github).execute(claim);
  return api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
}

describe("the preserved patch check sees the whole diff", () => {
  it("withholds the patch when the value sits past the byte cap", async () => {
    const full = `${header}${filler}+token=${SECRET}\n`;
    assert.ok(capReviewDiff(full).indexOf(SECRET) === -1 && full.includes(SECRET), "the value is cut off by the cap");
    const failed = await failedReport(46201, full);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
  });

  it("keeps a clean patch, capped", async () => {
    const failed = await failedReport(46202, `${header}${filler}+on: [push]\n`);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.match(failed.preserved_patch ?? "", /diff truncated at \d+ bytes/);
  });
});
