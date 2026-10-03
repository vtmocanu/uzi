import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { failOriginForReason, RunRunner } from "../src/runner.js";
import { REASON_PROVISION_FAILED } from "../src/provision-run.js";
import { REASON_NO_TOKEN } from "../src/sdk-executor.js";
import { REASON_PLAN_MISSING } from "../src/plan-missing.js";
import { REASON_SKILLS_PLUGIN_LOAD_FAILED } from "../src/plugin-errors.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { Executor } from "../src/executor.js";

// PRD #69 M7a: the worker authors the fail_origin from the reason CONSTANT the throw
// site used, never by parsing free text. This pins the two fatal pre-start mappings
// and the "everything else is an ordinary agent failure" default.
describe("failOriginForReason", () => {
  it("maps a provisioning failure (with appended detail) to provisioning_failed", () => {
    assert.strictEqual(
      failOriginForReason(REASON_PROVISION_FAILED),
      "provisioning_failed",
    );
    // provision-run appends `: <detail>` after the constant — the prefix match holds.
    assert.strictEqual(
      failOriginForReason(`${REASON_PROVISION_FAILED}: invalid run id`),
      "provisioning_failed",
    );
  });

  it("maps a missing Anthropic token to credential_unavailable", () => {
    assert.strictEqual(
      failOriginForReason(REASON_NO_TOKEN),
      "credential_unavailable",
    );
  });

  it("maps the plan-missing reason to plan_missing by exact match (issue #1593)", () => {
    assert.strictEqual(failOriginForReason(REASON_PLAN_MISSING), "plan_missing");
    // Exact match, not a prefix: nothing appended to the constant is authored as plan_missing.
    assert.strictEqual(failOriginForReason(`${REASON_PLAN_MISSING}: extra`), undefined);
  });

  it("maps the skills-plugin load failure prefix to skills_plugin_load_failed (issue #1888)", () => {
    assert.strictEqual(
      failOriginForReason(`${REASON_SKILLS_PLUGIN_LOAD_FAILED}: the run's skills plugin failed to load (x)`),
      "skills_plugin_load_failed",
    );
    // Prefix with its `: ` separator only: the bare constant is not an authored reason.
    assert.strictEqual(failOriginForReason(REASON_SKILLS_PLUGIN_LOAD_FAILED), undefined);
  });

  it("returns undefined for an ordinary agent failure (server defaults to agent_failure)", () => {
    assert.strictEqual(failOriginForReason("clone failed: exit 128"), undefined);
    assert.strictEqual(failOriginForReason(""), undefined);
    assert.strictEqual(
      failOriginForReason("the agent ended the planning turn without submitting a plan"),
      undefined,
    );
  });
});

// Issue #1593: a run whose planning turn stayed prose-only fails with the static reason and a
// typed origin — no model text reaches failure_reason.
describe("a run failing REASON_PLAN_MISSING", () => {
  const TOKEN = "tkn-failorigin-1593";
  let api: FakeApi;
  let fx: Fixture;
  let client: WorkerClient;
  let git: GitCache;

  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    const baseUrl = await api.listen();
    fx = makeFixture();
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  });
  afterEach(async () => {
    // try/finally (issue #2020): a throwing close must not skip the fixture cleanup.
    try {
      await api.close();
    } finally {
      fx.cleanup();
    }
  });

  it("reports fail_origin plan_missing and exactly the static failure_reason", async () => {
    const executor: Executor = {
      async run() {
        throw new Error(REASON_PLAN_MISSING);
      },
    };
    const claim = makeClaim({
      issue_iid: 70,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
    });
    await new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed").at(-1)?.body;
    assert.ok(failed, "the run must report failed");
    assert.strictEqual(failed.fail_origin, "plan_missing");
    assert.strictEqual(failed.failure_reason, REASON_PLAN_MISSING);
  });

  it("sanitizes generic failure log, feed and report after redaction without a 256-character cap (#2196)", async () => {
    const secret = "fixture-" + "failure-secret-2196";
    const splitSecret = `${secret.slice(0, 12)}\n${secret.slice(12)}`;
    const raw = `runner-owned session seed failed (exit 1): \u001b[31mfailed\r\n\u202ehidden ${splitSecret} ${"x".repeat(600)}`;
    const expected = `runner-owned session seed failed (exit 1): ?[31mfailed???hidden ***REDACTED*** ${"x".repeat(600)}`;
    const logged: unknown[] = [];
    const log = nullLogger();
    log.error = (message, fields) => {
      if (message === "run failed") logged.push(fields?.error);
    };
    const executor: Executor = { async run() { throw new Error(raw); } };
    const claim = makeClaim({
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      secrets: { forge_pat: secret },
    });
    await new RunRunner(client, git, () => ({ executor }), log, 20, undefined, { pollMs: 5 }).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed").at(-1)?.body;
    assert.ok(failed, "a terminal failure was reported");
    const errors = api.messages(claim.run_id).filter((m) => m.kind === "error");
    assert.equal(errors.length, 1, "the generic failure emitted one error event");
    assert.equal(errors[0]?.payload.text, expected, "the feed keeps the full sanitized diagnostic");
    assert.deepEqual(logged, [expected], "the log shares the sanitized diagnostic");
    assert.equal(failed.failure_reason, expected.slice(0, 512), "only the report is capped, at 512 characters");
    assert.equal(failed.failure_reason?.length, 512);
    assert.equal(failed.fail_origin, undefined, "the original failure classification stays unchanged");
  });
});
