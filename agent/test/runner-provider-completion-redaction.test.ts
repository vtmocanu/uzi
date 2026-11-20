import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor } from "../src/executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

installHarness();

// The report-only completion carries the lead's `summary` (persisted as report_md) and, on a prompt
// run, a structured proposal. Both are model-authored and POSTed to the api, whose pattern scrub
// cannot catch a prefixless provider key, so the worker redacts them first. The key is synthetic.
const KEY = ["Hd6y", "Nc/3", "uV+9", "&pB1", "<z5>", "eT8m"].join("");
const FORMS = providerKeyForms(KEY);

const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});
const bound = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() });
const vendor = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

function assertNoKey(label: string, text: string): void {
  for (const f of FORMS) assert.ok(!text.includes(f), `${label}: form leaked: ${f}`);
  for (const part of ["Hd6y", "eT8m"]) assert.ok(!text.includes(part), `${label}: fragment ${part} leaked`);
}

let iid = 45053;
async function complete(secrets: object, kind: "issue" | "prompt") {
  const { gitlab } = fakeGitlab();
  if (kind === "prompt") git.changedFiles = (async () => []) as typeof git.changedFiles;
  const claim = gitlabClaim(iid++, { secrets: secrets as never, ...(kind === "prompt" ? { kind: "prompt" } : {}) } as never);
  const exec: Executor = {
    run: async (ctx) => ({
      branch: ctx.branch, ...(kind === "issue" ? { reportOnly: true } : {}), summary: `findings: header ${KEY} was echoed`,
      ...(kind === "prompt" ? { proposal: { title: `rotate ${KEY}`, body: `body ${FORMS[FORMS.length - 1]!}` } } : {}),
    }),
  };
  await runner(exec, gitlab).execute(claim);
  return api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")!.body;
}

describe("provider-bound run: report-only completion redacts report_md and the proposal", () => {
  it("issue run report_md", async () => {
    const body = await complete(bound(), "issue");
    assert.match(String(body.report_md), /^findings: header /);
    assertNoKey("report_md", String(body.report_md));
  });

  it("prompt run report_md and proposal", async () => {
    const body = await complete(bound(), "prompt");
    assertNoKey("report_md", String(body.report_md));
    assert.ok(body.proposal, "the proposal still reaches the api");
    assertNoKey("proposal", JSON.stringify(body.proposal));
  });

  // prd_done_path rides the terminal report of an MR completion and of a no-MR task completion;
  // it is a lead-declared string, so it is scrubbed like the summary.
  for (const shape of ["mr", "task-no-mr"] as const) {
    it(`${shape} completion prd_done_path`, async () => {
      const { gitlab } = fakeGitlab();
      simulateCommittedWork();
      const claim = gitlabClaim(iid++, {
        secrets: bound() as never,
        ...(shape === "task-no-mr" ? { kind: "task", issue_iid: null, open_mr: false, branch: `uzi/task/redact-${iid}` } : {}),
      } as never);
      const exec: Executor = {
        run: async (ctx) => ({ branch: ctx.branch, prdDonePath: `prds/done/${KEY}.md`, summary: "ok" }),
      };
      await runner(exec, gitlab).execute(claim);
      const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")!.body;
      assert.match(String(body.prd_done_path), /^prds\/done\//);
      assertNoKey("prd_done_path", String(body.prd_done_path));
    });
  }

  // A scope-capped delivery with no commit (operator scope directive) completes report-only with
  // the lead's summary as report_md.
  it("scope-capped no-commit completion report_md", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: bound() as never } as never);
    const exec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch, scopeCapped: { completedCount: 0, total: 3 }, summary: `stopped; header ${KEY} was echoed`,
      }),
    };
    await runner(exec, gitlab).execute(claim);
    const body = api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")!.body;
    assert.equal(body.scope_capped, true);
    assert.match(String(body.report_md), /^stopped; header /);
    assertNoKey("report_md", String(body.report_md));
  });

  // The lead's non_closing reason is forwarded to the api by the runner's recordCompletionAttempt
  // closure; it is model-authored, so the closure scrubs it. Observed at the WorkerClient seam.
  for (const [label, secrets, scrubbed] of [["provider-bound", bound(), true], ["vendor control", vendor(), false]] as const) {
    it(`non_closing reason on a completion attempt (${label})`, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(iid++, { secrets: secrets as never, config: { completion_contract_version: 1, contract_revision: 1 } } as never);
      const seen: Array<{ reason: string } | undefined> = [];
      const original = client.recordCompletionAttempt.bind(client);
      client.recordCompletionAttempt = (async (id, args, signal) => {
        seen.push(args.nonClosing);
        return original(id, args, signal);
      }) as typeof client.recordCompletionAttempt;
      try {
        const exec: Executor = {
          run: async (ctx) => {
            await ctx.recordCompletionAttempt?.({ declared: [], head: "h", worktreeFingerprint: null, nonClosing: { reason: `defer: ${KEY}` } });
            return { branch: ctx.branch, reportOnly: true, summary: "ok" };
          },
        };
        await runner(exec, gitlab).execute(claim);
      } finally { client.recordCompletionAttempt = original; }
      assert.equal(seen.length, 1, "the closure forwarded one attempt");
      assert.match(seen[0]!.reason, /^defer: /);
      if (scrubbed) assertNoKey("non_closing.reason", seen[0]!.reason);
      else assert.equal(seen[0]!.reason, `defer: ${KEY}`);
    });
  }

  it("control: a vendor run's summary is unchanged", async () => {
    const body = await complete(vendor(), "issue");
    assert.equal(body.report_md, `findings: header ${KEY} was echoed`);
  });
});
