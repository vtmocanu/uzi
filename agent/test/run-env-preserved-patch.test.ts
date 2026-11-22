import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger, recordingLogger } from "./helpers.js";
import type { Logger } from "../src/log.js";
import { capReviewDiff } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { patchScanVerdict } from "../src/run-env-scan.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import type { GitHubClient } from "../src/forge.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
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

const claimFor = (iid: number, vars: ClaimRunEnvVar[] = [var1], redact?: ClaimRunEnvRedact[]) => gitlabClaim(iid, {
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "github" },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: vars, ...(redact === undefined ? {} : { run_env_redact: redact }) },
});

/** What the git layer returns: the whole, uncapped diff. */
const diffOf = (full: string) => (async () => full) as typeof git.workflowScopeDiff;
const header = `diff --git a/${WORKFLOW} b/${WORKFLOW}\n`;
const filler = "+".concat("y".repeat(79), "\n").repeat(8000);
/** The file header and hunk header of an all-added hunk of `n` lines. */
const bigHead = (n: number): string => `${header}index 1111111..2222222 100644\n--- a/${WORKFLOW}\n+++ b/${WORKFLOW}\n@@ -0,0 +1,${n} @@\n`;

async function failedReport(
  iid: number, full: string, vars?: ClaimRunEnvVar[], tweak?: (runner: RunRunner) => void, redact?: ClaimRunEnvRedact[],
): Promise<{ fail_origin?: string; preserved_patch?: string; logged: Array<Record<string, unknown>> }> {
  const { github } = fakeGitHub();
  git.pushBranch = (async () => undefined) as typeof git.pushBranch;
  git.branchWorkflowFiles = (async () => [WORKFLOW]) as typeof git.branchWorkflowFiles;
  git.workflowScopeDiff = diffOf(full);
  const claim = claimFor(iid, vars, redact);
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

// git C-quotes a header path with default settings: "\303\251" for a non-ASCII byte, "\t", "\"" and "\\". The raw
// patch then never holds the decoded value, so the header lines are decoded and searched too. The diffs below
// come from a real `git diff` of a throwaway repository.
function realDiff(files: Array<{ name: string; body?: string; rename?: string }>): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-header-patch-"));
  try {
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const g = (...args: string[]): string => execFileSync("git", ["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", ...args], { env, encoding: "utf8" });
    g("init", "-q", "-b", "main");
    fs.writeFileSync(path.join(dir, "README.md"), "seed\n");
    g("add", "-A");
    g("commit", "-q", "-m", "seed");
    for (const f of files) {
      if (f.rename) g("mv", "README.md", f.name);
      else { fs.mkdirSync(path.dirname(path.join(dir, f.name)), { recursive: true }); fs.writeFileSync(path.join(dir, f.name), f.body ?? "harmless\n"); }
    }
    g("add", "-A");
    return g("diff", "--cached", "-M");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

// A patch that adds any line of 8+ characters of a multi-line secret file is withheld as line_form, structural
// lines included. The key line is derived from a hash.
describe("the preserved patch and the structural lines of a secret file", () => {
  const keyLine = `    client-key-data: ${crypto.createHash("sha256").update("patch-key").digest("base64").replace(/[+/=]/g, "Q")}`;
  const lines = ["apiVersion: v1", "kind: Config", "current-context: example", "clusters:", "- name: example", "    server: https://kubernetes.example.com:6443", keyLine];
  const claim = runEnvClaimForms([{ ...var1, name: "KUBECONFIG_FILE", value: lines.join("\n") }], []);
  const added = (...body: string[]): string => hunkPatch(`@@ -0,0 +1,${body.length} @@`, ...body.map((l) => `+${l}`));

  for (const line of lines.slice(0, 6)) {
    it(`is withheld as line_form when it adds only the structural line ${line.trim()}`, () => {
      const verdict = patchScanVerdict(added(line), claim.needles, claim.lineForms, ["KUBECONFIG_FILE"]);
      assert.deepEqual(verdict, { withhold: true, reason: "line_form", variables: ["KUBECONFIG_FILE"] });
    });
  }

  it("the line-form check alone does not withhold a whitespace-only line", () => {
    assert.deepEqual(patchScanVerdict(added(" ".repeat(8)), claim.needles, claim.lineForms, []), { withhold: false });
    // The run holds a multi-line secret, so the patch is withheld on that ground.
    assert.deepEqual(patchScanVerdict(added(" ".repeat(8)), claim.needles, claim.lineForms, ["KUBECONFIG_FILE"]),
      { withhold: true, reason: "multi_line_secret", variables: ["KUBECONFIG_FILE"] });
  });

  it("is withheld as line_form when it adds the key line", () => {
    const verdict = patchScanVerdict(added(lines[0]!, keyLine), claim.needles, claim.lineForms, ["KUBECONFIG_FILE"]);
    assert.deepEqual(verdict, { withhold: true, reason: "line_form", variables: ["KUBECONFIG_FILE"] });
  });
});

describe("the preserved patch decodes quoted header paths", () => {
  const cases: Array<[string, string]> = [
    ["a non-ASCII letter", ["café", "Kq7Zp2mXw9Lt4s"].join("-")],
    ["a TAB", ["tab", "Kq7Zp2mXw9Lt4s"].join("\t")],
    ["a double quote", ["q", "Kq7Zp2mXw9Lt4s"].join('"')],
    ["a backslash", ["back", "Kq7Zp2mXw9Lt4s"].join("\\")],
  ];
  for (const [label, value] of cases) {
    it(`withholds a patch whose header path carries ${label}`, async () => {
      const full = realDiff([{ name: value }]);
      assert.ok(full.includes('"'), "git quoted the header path");
      assert.ok(!full.includes(value), "the raw patch never holds the decoded value");
      const failed = await failedReport(46230 + cases.findIndex((c) => c[0] === label), full, [{ ...var1, value }]);
      assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
      assert.equal(failed.preserved_patch, undefined);
      // The registered forms already include a JSON-escaped one that equals git's C-quoting of a TAB, quote or
      // backslash, so those may be caught as raw_value; the octal bytes of a non-ASCII letter are caught only by the decoding.
      const reasons = withholdReasons(failed.logged);
      assert.equal(reasons.length, 1);
      assert.ok(reasons[0] === "header_path" || (label !== "a non-ASCII letter" && reasons[0] === "raw_value"), String(reasons[0]));
    });

    it(`decodes the header path carrying ${label} when the only needle form is the value itself`, () => {
      const full = realDiff([{ name: value }]);
      assert.deepEqual(patchScanVerdict(full, [{ name: "DB_PASSWORD", forms: [value] }], [], []),
        { withhold: true, reason: "header_path", variables: ["DB_PASSWORD"] });
      assert.deepEqual(patchScanVerdict(full, [{ name: "DB_PASSWORD", forms: ["no-such-value-anywhere"] }], [], []).withhold, false);
    });
  }

  it("decodes an astral character left literal in a quoted header path (core.quotePath=false)", () => {
    const value = ["x/", "\u{1F511}", "\u0007-", "Kq7Zp2mXw9Lt4s"].join("");
    const quoted = `x/\u{1F511}\\a-Kq7Zp2mXw9Lt4s`;
    const full = [
      `diff --git "a/${quoted}" "b/${quoted}"`, "new file mode 100644", "index 0000000..e69de29",
      `--- /dev/null`, `+++ "b/${quoted}"`, "",
    ].join("\n");
    assert.deepEqual(patchScanVerdict(full, [{ name: "DB_PASSWORD", forms: [value] }], [], []),
      { withhold: true, reason: "header_path", variables: ["DB_PASSWORD"] });
    assert.equal(patchScanVerdict(full, [{ name: "DB_PASSWORD", forms: ["no-such-value-anywhere"] }], [], []).withhold, false);
  });

  it("withholds a Binary files header whose quoted paths carry a non-ASCII secret", () => {
    const value = ["x/", "café-", "Kq7Zp2mXw9Lt4s"].join("");
    const quoted = `x/caf\\303\\251-Kq7Zp2mXw9Lt4s`;
    const full = [
      `diff --git "a/${quoted}" "b/${quoted}"`, "index 0000000..1111111 100644",
      `Binary files "a/${quoted}" and "b/${quoted}" differ`, "",
    ].join("\n");
    const verdict = patchScanVerdict(full, [{ name: "DB_PASSWORD", forms: [value] }], [], []);
    assert.equal(verdict.withhold, true);
    assert.equal(verdict.withhold && verdict.reason, "header_path");
  });

  it("withholds a rename header (rename to) whose target is quoted", async () => {
    const value = ["café", "Kq7Zp2mXw9Lt4s"].join("-");
    const full = realDiff([{ name: value, rename: "README.md" }]);
    assert.match(full, /^rename to "/m);
    const failed = await failedReport(46240, full, [{ ...var1, value }]);
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["header_path"]);
  });

  it("keeps a patch whose quoted header path carries no value", async () => {
    const failed = await failedReport(46241, realDiff([{ name: "café-other.txt" }]), [var1]);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.match(failed.preserved_patch ?? "", /harmless/);
  });
});

describe("the preserved patch fails closed on what the raw text cannot show", () => {
  it("withholds a mixed hunk that carries the secret's lines as context and as added lines", async () => {
    const full = hunkPatch("@@ -1,2 +1,3 @@", ` ${SHORT_LINES[0]}`, `+${SHORT_LINES[1]}`, ` ${SHORT_LINES[2]}`);
    assert.ok(!full.includes(SHORT_LINES.join("\n")), "no prefix-free run of the value in the raw text");
    const failed = await failedReport(46210, full, [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["multi_line_secret"]);
  });

  it("withholds a binary patch", async () => {
    const full = `${header}index 1111111..2222222 100644\nGIT binary patch\nliteral 3\nKc2\n\nliteral 0\nHcmV?d00001\n`;
    const failed = await failedReport(46211, full);
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
    assert.deepEqual(withholdReasons(failed.logged), ["multi_line_secret"]);
  });

  it("withholds a clean patch when the claim's forms were over budget", async () => {
    const clean = hunkPatch("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}");
    const control = await failedReport(46215, clean);
    assert.match(control.preserved_patch ?? "", /\+on: \[push\]/, "control: the same patch is attached when the claim is within budget");
    assert.ok(!control.logged.some((l) => l.msg === OVER_BUDGET_WITHHELD), "control: no over-budget warning");
    const failed = await failedReport(46216, clean, undefined, (runner) => {
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

  it("still attaches a clean patch, hunks and all, for a single-line secret", async () => {
    const failed = await failedReport(46213, hunkPatch("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}"));
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.match(failed.preserved_patch ?? "", /\+on: \[push\]/);
  });

  it("withholds the same clean patch as multi_line_secret when the secret is multi-line", async () => {
    const failed = await failedReport(46217, hunkPatch("@@ -1,2 +1,3 @@", " name: guard", "+on: [push]", " jobs: {}"), [multiVar(SHORT_LINES)]);
    assert.equal(failed.fail_origin, "workflow_scope_missing");
    assert.equal(failed.preserved_patch, undefined);
    assert.deepEqual(withholdReasons(failed.logged), ["multi_line_secret"]);
  });
});

// A multi-line secret whose lines are each under the 8-character line-form floor, split so that no whole-value or
// line form can match: across two files, or across two hunks of one file with elided context between. The run is
// withheld whole whether the secret is live or only a retained older version (run_env_redact).
describe("the preserved patch of a run holding a multi-line secret is always withheld", () => {
  const other: ClaimRunEnvVar = { ...var1, name: "OTHER_TOKEN", value: "unrelated-single-line-value-111111" };
  const otherFile = ".github/workflows/second-guard.yml";
  const section = (file: string, ...body: string[]): string =>
    `diff --git a/${file} b/${file}\nindex 1111111..2222222 100644\n--- a/${file}\n+++ b/${file}\n${body.join("\n")}\n`;
  const splitFile = section(WORKFLOW, "@@ -0,0 +1,2 @@", `+${SHORT_LINES[0]}`, "+ordinary: one") +
    section(otherFile, "@@ -0,0 +1,2 @@", `+${SHORT_LINES[1]}`, `+${SHORT_LINES[2]}`);
  const elided = section(WORKFLOW, "@@ -1,1 +1,2 @@", " name: guard", `+${SHORT_LINES[0]}`,
    "@@ -40,1 +41,3 @@", " jobs: {}", `+${SHORT_LINES[1]}`, `+${SHORT_LINES[2]}`);
  const shapes: Array<[string, string]> = [["split across two files", splitFile], ["split across two hunks with elided context", elided]];

  let n = 46250;
  for (const [label, patch] of shapes) {
    it(`withholds a patch with a live multi-line secret ${label}`, async () => {
      assert.ok(!patch.includes(SHORT_LINES.join("\n")), "no whole-value form in the raw text");
      const failed = await failedReport(n++, patch, [other, multiVar(SHORT_LINES)]);
      assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
      assert.equal(failed.preserved_patch, undefined);
      assert.deepEqual(withholdReasons(failed.logged), ["multi_line_secret"]);
    });

    it(`withholds a patch with a retained multi-line secret version ${label}`, async () => {
      const retainedVersion: ClaimRunEnvRedact = { var_id: var1.var_id, scope: "repo", rev: 1, name: "OLD_BLOB", value: SHORT_LINES.join("\n") };
      const failed = await failedReport(n++, patch, [other], undefined, [retainedVersion]);
      assert.equal(failed.fail_origin, "workflow_scope_missing", "the typed failure still lands");
      assert.equal(failed.preserved_patch, undefined);
      assert.deepEqual(withholdReasons(failed.logged), ["multi_line_secret"]);
    });

    it(`keeps the identical patch ${label} when the run's only secret is single-line`, async () => {
      const failed = await failedReport(n++, patch, [other]);
      assert.equal(failed.fail_origin, "workflow_scope_missing");
      assert.ok(failed.preserved_patch !== undefined, "the patch is attached");
      assert.deepEqual(withholdReasons(failed.logged), []);
    });
  }
});
