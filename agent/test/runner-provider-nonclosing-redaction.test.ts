import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms, redactNonClosingReason } from "../src/redact.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { proposal, enrollment } from "./semantic-coverage-fixture.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// Three provider-key sinks of the runner that carry model-authored text to the api: the report_only
// fallback non_closing attempt, the pre-push-refusal hold attempt, and the gatePlan semantic proposal.
// The key is synthetic and assembled at runtime.
const KEY = ["Hd6y", "Nc/3", "uV+9", "&pB1", "<z5>", "eT8m"].join("");
const FORMS = providerKeyForms(KEY);
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

const bound = () => ({
  forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "",
  provider: {
    provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
    auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
  } satisfies ClaimProviderBinding,
});

function assertNoKey(label: string, text: string): void {
  for (const f of FORMS) assert.ok(!text.includes(f), `${label}: form leaked: ${f}`);
  for (const part of ["Hd6y", "eT8m"]) assert.ok(!text.includes(part), `${label}: fragment ${part} leaked`);
}

/** StubExecutor plus an optional extra commit carrying `message`. */
function executor(opts: { message?: string; extra?: Partial<ExecutorResult> }): Executor {
  return new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      if (opts.message !== undefined) {
        fs.writeFileSync(path.join(ctx.worktreePath, "closing.txt"), "work\n");
        execFileSync("git", ["-C", ctx.worktreePath, "add", "closing.txt"]);
        execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", opts.message]);
      }
      return { ...result, ...opts.extra };
    }
  }(nullLogger());
}

const lines = (n: number) => "y".repeat(n);

describe("redactNonClosingReason", () => {
  it("clamps a redaction-lengthened reason to 500 code points and keeps a non-empty one non-empty", () => {
    const redact = (s: string) => s.split("secret1").join("[REDACTED-LONG-MARKER]");
    const out = redactNonClosingReason(redact, lines(495) + "secret1");
    assert.equal(Array.from(out).length, 500);
    assert.equal(redactNonClosingReason(redact, "a"), "a");
    assert.equal(redactNonClosingReason(redact, "secret1"), "[REDACTED-LONG-MARKER]");
    const astral = "\u{1F600}".repeat(600);
    assert.equal(Array.from(redactNonClosingReason((s) => s, astral)).length, 500);
  });
});

describe("provider-bound run: the report_only fallback non_closing attempt redacts and bounds the reason", () => {
  it("fallback attempt", async () => {
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1", "refresh_publication_context_v1"]);
    await client.register("provider-fallback");
    git.hasCommittedCheckpoint = (async () => true) as typeof git.hasCommittedCheckpoint;
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr);
    const claim = gitlabClaim(45054, {
      claim_generation: 7, secrets: bound() as never,
      config: { completion_contract_version: 1, contract_revision: 1, non_closing: { reason: lines(495) + KEY } },
    });
    api.setPublicationState(claim.run_id, [], 1);
    api.setCompletionPermitResponse(true);
    await runner(executor({ extra: { reportOnly: true, summary: "verified" } }), forge.gitlab).execute(claim);
    assert.equal(api.completionAttemptRequests.length, 1, "exactly one fallback attempt");
    const reason = String((api.completionAttemptRequests[0]!.non_closing as { reason: string }).reason);
    assert.ok(reason.startsWith("yyyy"), reason);
    assertNoKey("fallback non_closing.reason", reason);
    assert.ok(Array.from(reason).length >= 1 && Array.from(reason).length <= 500, `length ${Array.from(reason).length}`);
  });
});

describe("provider-bound run: the pre-push refusal hold attempt redacts and bounds the reason", () => {
  it("report-only completion_non_closing_v1 attempt", async () => {
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("provider-prepush");
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    const claim = gitlabClaim(45055, {
      claim_generation: 7, secrets: bound() as never,
      milestones: [{ id: "m1", title: "Build" }],
      config: { completion_contract_version: 1, contract_revision: 1 },
    });
    api.setPublicationState(claim.run_id, [], 1);
    await runner(executor({ message: "Closes #45055", extra: { nonClosing: { reason: lines(495) + KEY } } }), forge.gitlab).execute(claim);
    assert.equal(api.completionHoldRequests.length, 1, "held for history correction");
    assert.ok(api.completionAttemptRequests.length >= 1, "the empty attempt was recorded");
    const reason = String((api.completionAttemptRequests[0]!.non_closing as { reason: string }).reason);
    assert.ok(reason.startsWith("yyyy"), reason);
    assertNoKey("prepush non_closing.reason", reason);
    assert.ok(Array.from(reason).length >= 1 && Array.from(reason).length <= 500, `length ${Array.from(reason).length}`);
  });
});

describe("provider-bound run: the executor-facing recordCompletionAttempt closure redacts and bounds the reason", () => {
  it("long reason ending in the key", async () => {
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("provider-closure");
    const forge = fakeGitlab();
    const claim = gitlabClaim(45057, {
      claim_generation: 7, secrets: bound() as never,
      milestones: [{ id: "m1", title: "Build" }],
      config: { completion_contract_version: 1, contract_revision: 1 },
    });
    api.setPublicationState(claim.run_id, [], 1);
    const exec: Executor = { run: async (ctx) => {
      await ctx.recordCompletionAttempt!({
        declared: [], head: null, worktreeFingerprint: null, nonClosing: { reason: lines(495) + KEY },
      });
      return { branch: ctx.branch };
    } };
    await runner(exec, forge.gitlab).execute(claim);
    assert.ok(api.completionAttemptRequests.length >= 1, "the attempt was recorded");
    const reason = String((api.completionAttemptRequests[0]!.non_closing as { reason: string }).reason);
    assert.ok(reason.startsWith("yyyy"), reason);
    assertNoKey("closure non_closing.reason", reason);
    assert.ok(Array.from(reason).length >= 1 && Array.from(reason).length <= 500, `length ${Array.from(reason).length}`);
  });
});

describe("provider-bound run: the gatePlan semantic proposal is redacted before the compile", () => {
  it("submitSemanticCandidate receives a redacted proposal", async () => {
    git.capturePlanningDiff = async () => Buffer.from("");
    const claim = gitlabClaim(45056, {
      kind: "issue", auto_approve: true, secrets: bound() as never, issue_description: "Implement A",
      semantic_planning: enrollment, plan_cross_check_required: true,
    } as never);
    const seen: unknown[] = [];
    const original = client.submitSemanticCandidate.bind(client);
    client.submitSemanticCandidate = (async (_id: string, _gen: number, _structural: unknown, submitted: unknown) => {
      seen.push(submitted);
      throw new CredentialSwitchSignal();
    }) as typeof client.submitSemanticCandidate;
    const p = proposal();
    p.criteria[0]!.text = `A with ${KEY}`;
    p.criteria[0]!.done_when = `Verified ${FORMS[FORMS.length - 1]!}`;
    const exec: Executor = { run: async (ctx) => {
      await ctx.gatePlan!("Implement A", [{ id: "m1", title: "A" }] as never, undefined, undefined, p as never).catch(() => undefined);
      return { branch: ctx.branch };
    } };
    try { await runner(exec, fakeGitlab().gitlab).execute(claim); }
    finally { client.submitSemanticCandidate = original; }
    assert.equal(seen.length, 1, "the compile was reached");
    const text = JSON.stringify(seen[0]);
    assert.ok(text.includes("A with "), text);
    assertNoKey("semantic proposal", text);
  });
});
