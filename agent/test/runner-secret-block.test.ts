import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
import {
  api,
  client,
  fakeGitHub,
  fx,
  git,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// PRD #974 M2 / #1077: the two terminal push_secret_blocked sites (the trusted pre-push
// gitleaks block and the GH013 remote backstop) must persist fail_origin=push_secret_blocked
// and NEVER a preserved_patch — even when the terminal reportState exhausts its bounded
// retries and throws, in which case execute()'s generic catch honors a typed sentinel
// (TerminalReportError) instead of defaulting the origin to agent_failure.
describe("RunRunner — push_secret_blocked typed terminal (PRD #974 M2 / #1077)", () => {
  // A github claim: forge_type=github routes the pre-push gitleaks scan + GH013 backstop,
  // clone_url is the local fixture (git is forge-agnostic).
  const githubClaim = (iid: number) =>
    gitlabClaim(iid, {
      repo: {
        id: "r1",
        url: "https://github.com/org/repo",
        clone_url: fx.originPath,
        forge_type: "github",
      },
    });

  // The github-wired runner (mirrors runner-push-mr.test.ts): a fakeGitHub forge client so
  // no network, StubExecutor so a real branch is committed and finalize proceeds.
  const githubRunner = () => {
    const { github } = fakeGitHub();
    return new RunRunner(
      client,
      git,
      () => ({ executor: new StubExecutor(nullLogger()) }),
      nullLogger(),
      20,
      undefined,
      {
        pollMs: 5,
        planApprovalTimeoutMs: 0,
        github,
      },
    );
  };

  const failedBody = (runId: string) =>
    api.states.find((s) => s.runId === runId && s.body.status === "failed")!.body;

  it("trusted scan finding fails typed as push_secret_blocked, omits preserved_patch, and never pushes", async () => {
    git.secretScanRange = (async () => ({
      trusted: true,
      findings: [
        {
          file: "src/leak.ts",
          startLine: 1,
          commit: "deadbeefcafe",
          ruleId: "generic-api-key",
        },
      ],
    })) as typeof git.secretScanRange;
    let pushed = 0;
    git.pushBranch = (async () => {
      pushed++;
    }) as typeof git.pushBranch;

    const claim = githubClaim(1001);
    await githubRunner().execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined);
    // The trusted-scan block returns BEFORE pushToOrigin — the doomed push never happens.
    assert.strictEqual(pushed, 0, "a trusted secret finding must block the push entirely");
  });

  it("GH013 backstop fails typed as push_secret_blocked and omits preserved_patch", async () => {
    // Fail-open scan (untrustworthy/empty): the pre-push block does not fire, so the run
    // reaches the finalize push and relies on GitHub's GH013 rejection backstop.
    git.secretScanRange = (async () => ({
      trusted: false,
      findings: [],
    })) as typeof git.secretScanRange;
    git.pushBranch = (async () => {
      throw new Error(
        "remote: error: GH013: Repository rule violations found — push cannot contain secrets (push protection)",
      );
    }) as typeof git.pushBranch;

    const claim = githubClaim(1002);
    await githubRunner().execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined);
  });

  it("preserves push_secret_blocked origin when the terminal report throws (trusted-scan path)", async () => {
    // Arm the /state failure at the MOMENT of the terminal report (inside secretScanRange,
    // just before returning the finding) so the earlier non-terminal running reports do not
    // consume the injected budget. With terminalRetrySchedule:[1,1] the terminal report makes
    // three attempts, all 503 → it throws → generic catch → TerminalReportError → the fallback
    // reportState (4th /state call) succeeds and persists the typed origin.
    git.secretScanRange = (async () => {
      api.failStateNext(3, 503);
      return {
        trusted: true,
        findings: [
          {
            file: "src/leak.ts",
            startLine: 1,
            commit: "deadbeefcafe",
            ruleId: "generic-api-key",
          },
        ],
      };
    }) as typeof git.secretScanRange;

    const claim = githubClaim(1003);
    await githubRunner().execute(claim);

    const failed = failedBody(claim.run_id);
    // The compound fix: WITHOUT it, the sentinel's typed origin is lost and the server
    // defaults to agent_failure.
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined);
  });

  it("preserves push_secret_blocked origin when the terminal report throws (GH013 backstop)", async () => {
    git.secretScanRange = (async () => ({
      trusted: false,
      findings: [],
    })) as typeof git.secretScanRange;
    git.pushBranch = (async () => {
      // Arm the failure so the terminal backstop report exhausts its retries and throws.
      api.failStateNext(3, 503);
      throw new Error(
        "remote: error: GH013: push cannot contain secrets (push protection)",
      );
    }) as typeof git.pushBranch;

    const claim = githubClaim(1004);
    await githubRunner().execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined);
  });
});
