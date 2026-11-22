import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger, recordingLogger } from "./helpers.js";
import type { Logger } from "../src/log.js";
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

function githubRunner(github: GitHubClient, log: Logger = nullLogger()): RunRunner {
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
    log, 20, undefined, { pollMs: 5, planApprovalTimeoutMs: 0, github },
  );
}

const SCAN_WITHHELD = "preserved patch withheld: run secret scan";
const OVER_BUDGET_WITHHELD = "preserved patch withheld: the run's secret forms were over budget";

const claimFor = (iid: number, vars: ClaimRunEnvVar[] = [var1]) => gitlabClaim(iid, {
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "github" },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: vars },
});

/** What the git layer returns: the whole, uncapped diff. */
const diffOf = (full: string) => (async () => full) as typeof git.workflowScopeDiff;
const header = `diff --git a/${WORKFLOW} b/${WORKFLOW}\n`;
const filler = "+".concat("y".repeat(79), "\n").repeat(8000);
/** The file header and hunk header of an all-added hunk of `n` lines. */
const bigHead = (n: number): string => `${header}index 1111111..2222222 100644\n--- a/${WORKFLOW}\n+++ b/${WORKFLOW}\n@@ -0,0 +1,${n} @@\n`;

async function failedReport(
  iid: number, full: string, vars?: ClaimRunEnvVar[], tweak?: (runner: RunRunner) => void,
): Promise<{ fail_origin?: string; preserved_patch?: string; logged: Array<Record<string, unknown>> }> {
  const { github } = fakeGitHub();
  git.pushBranch = (async () => undefined) as typeof git.pushBranch;
  git.branchWorkflowFiles = (async () => [WORKFLOW]) as typeof git.branchWorkflowFiles;
  git.workflowScopeDiff = diffOf(full);
  const claim = claimFor(iid, vars);
  const { logger, lines } = recordingLogger();
  const runner = githubRunner(github, logger);
  tweak?.(runner);
  await runner.execute(claim);
  const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
  return { ...body, logged: lines as Array<Record<string, unknown>> };
}

/** The distinct `reason` fields of the scan-withheld warnings the run logged. */
const withholdReasons = (logged: Array<Record<string, unknown>>): unknown[] =>
  [...new Set(logged.filter((l) => l.msg === SCAN_WITHHELD).map((l) => l.reason))];

describe("the preserved patch check sees the whole diff", () => {
  it("withholds the patch when the value sits past the byte cap", async () => {
    const full = `${bigHead(8001)}${filler}+token=${SECRET}\n`;
    assert.ok(capReviewDiff(full).indexOf(SECRET) === -1 && full.includes(SECRET), "the value is cut off by the cap");
    const failed = await failedReport(46201, full);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["raw_value"]);
  });

  it("masks a registered secret that straddles the byte cap before the cap cuts it", async () => {
    const cap = 512 * 1024;
    const pat = "fixture-forge-pat-000000";
    const head = bigHead(8002);
    const lead = cap - 10 - head.length - 3;
    const full = `${head}+${"y".repeat(lead)}\n+${pat}\n${filler}`;
    assert.equal(Buffer.byteLength(full.slice(0, head.length + 1 + lead + 2 + 10)), cap, "the cap falls 10 characters into the value");
    const failed = await failedReport(46203, full);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.ok(failed.preserved_patch !== undefined, "the clean patch is kept");
    assert.ok(!failed.preserved_patch.includes("fixture-fo"), "no fragment of the value survives");
  });

  it("keeps a clean patch, capped", async () => {
    const failed = await failedReport(46202, `${bigHead(8001)}${filler}+on: [push]\n`);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.match(failed.preserved_patch ?? "", /diff truncated at \d+ bytes/);
  });
});

// A multi-line value whose lines are each shorter than the minimum registered length: only the whole value (read
// across the hunk's rebuilt image) can find it. Parts are joined at runtime.
const SHORT_LINES = ["ab12", "cd34", "ef56"];
const multiVar = (lines: string[]): ClaimRunEnvVar => ({ ...var1, name: "SVC_BLOB", value: lines.join("\n") });
const hunkPatch = (...body: string[]): string =>
  `${header}index 1111111..2222222 100644\n--- a/${WORKFLOW}\n+++ b/${WORKFLOW}\n${body.join("\n")}\n`;

describe("the preserved patch fails closed on what the raw text cannot show", () => {
  it("withholds a mixed hunk that carries the secret's lines as context and as added lines", async () => {
    const full = hunkPatch("@@ -1,2 +1,3 @@", ` ${SHORT_LINES[0]}`, `+${SHORT_LINES[1]}`, ` ${SHORT_LINES[2]}`);
    assert.ok(!full.includes(SHORT_LINES.join("\n")), "no prefix-free run of the value in the raw text");
    const failed = await failedReport(46210, full, [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["hunk_image_value"]);
  });

  it("withholds a binary patch", async () => {
    const full = `${header}index 1111111..2222222 100644\nGIT binary patch\nliteral 3\nKc2\n\nliteral 0\nHcmV?d00001\n`;
    const failed = await failedReport(46211, full, [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["unparseable_patch"]);
  });

  it("withholds a mixed hunk of CRLF lines whose secret was stored with LF line breaks", async () => {
    const full = hunkPatch("@@ -1,2 +1,3 @@", ` ${SHORT_LINES[0]}\r`, `+${SHORT_LINES[1]}\r`, ` ${SHORT_LINES[2]}\r`);
    assert.ok(!full.includes(SHORT_LINES.join("\r\n")) && !full.includes(SHORT_LINES.join("\n")), "no form of the value in the raw text");
    const failed = await failedReport(46214, full, [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["hunk_image_value"]);
  });

  it("withholds a clean patch when the claim's forms were over budget", async () => {
    const clean = hunkPatch("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}");
    const control = await failedReport(46215, clean, [multiVar(SHORT_LINES)]);
    assert.match(control.preserved_patch ?? "", /\+on: \[push\]/, "control: the same patch is attached when the claim is within budget");
    assert.ok(!control.logged.some((l) => l.msg === OVER_BUDGET_WITHHELD), "control: no over-budget warning");
    const failed = await failedReport(46216, clean, [multiVar(SHORT_LINES)], (runner) => {
      const internals = runner as unknown as { buildFlight(...a: unknown[]): { scanOverBudget?: boolean } };
      const build = internals.buildFlight.bind(runner);
      internals.buildFlight = (...a) => { const f = build(...a); f.scanOverBudget = true; return f; };
    });
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
    assert.ok(failed.logged.some((l) => l.msg === OVER_BUDGET_WITHHELD && l.level === "warn"), "the over-budget warning is logged");
    assert.deepEqual(withholdReasons(failed.logged), [], "the withhold is the budget one, not a scan verdict");
  });

  it("withholds a patch carrying one registered line of a multi-line secret", async () => {
    const lines = ["Qm9k7Zp3Wd", "ab12", "cd34"];
    const failed = await failedReport(46212, hunkPatch("@@ -0,0 +1 @@", `+${lines[0]}`), [multiVar(lines)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["line_form"]);
  });

  it("still attaches a clean patch, hunks and all", async () => {
    const failed = await failedReport(46213, hunkPatch("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}"), [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.match(failed.preserved_patch ?? "", /\+on: \[push\]/);
  });
});
