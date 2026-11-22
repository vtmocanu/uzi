import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { WorkerClient } from "../src/client.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { CodeCrossCheckStatus } from "../src/code-cross-check-contract.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { client, fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// The lead's code cross-check dispositions carry a model-authored `reason` that is POSTed to the api,
// whose scrub cannot catch a prefixless provider key. The worker redacts it with the run's secret set.
const KEY = ["Wg2k", "Fs/6", "yT+4", "&mC9", "<r1>", "uJ7q"].join("");
const FORMS = providerKeyForms(KEY);
const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});

const record = (head: string, base: string) => ({
  stage: "code" as const, round: 1 as const, candidate_generation: 1, head_commit: head, base_commit: base,
  candidate_digest: "c".repeat(64), checker_run_id: null, checker_harness: "codex", checker_model: "test",
  checker_effort: null, outcome: "completed" as const, reason_class: null, findings: [],
  interrupted_at: null, finalized_at: new Date().toISOString(), deadline_at: new Date(Date.now() + 1000).toISOString(),
});
function commit(ctx: RunContext): void {
  fs.writeFileSync(path.join(ctx.worktreePath, "implementation.txt"), "committed work\n");
  execFileSync("git", ["-C", ctx.worktreePath, "add", "implementation.txt"]);
  execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", "work"]);
}

it("provider-bound run: a code cross-check disposition reason is redacted before the api POST", async (t) => {
  let persisted: CodeCrossCheckStatus = { stage: "code", result: "no_row", repo_instructions_enabled: false };
  t.mock.method(client, "codeCrossCheckStatus", async () => persisted);
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
    assert.ok("head_commit" in snapshot);
    persisted = { ...record(snapshot.head_commit, snapshot.base_commit), checker_run_id: "child", finalized_at: null, dispositions: null,
      findings: [{ id: "F1", severity: "major", path: "implementation.txt", line: 1, title: "Verify work", detail: "one repair" }] };
    return persisted;
  });
  const sent: unknown[] = [];
  t.mock.method(client, "reportCodeCrossCheckDispositions", async (_id: string, _gen: number, batch: Parameters<WorkerClient["reportCodeCrossCheckDispositions"]>[2]) => {
    sent.push(batch);
    persisted = { ...persisted, finalized_at: new Date().toISOString(), dispositions: batch } as CodeCrossCheckStatus;
    return persisted;
  });
  const exec: Executor = {
    killAgentTree: () => {},
    run: async (ctx) => {
      commit(ctx);
      assert.equal((await ctx.codeCrossCheckGate!({ interlocked: false })).action, "repair");
      await ctx.codeCrossCheckRepair!();
      await ctx.reportCrossCheckDispositions!([{ finding_id: "F1", disposition: "addressed", reason: `fixed; header ${KEY} removed` }]);
      return { branch: ctx.branch };
    },
  };
  const claim = gitlabClaim(45054, { code_cross_check_required: true, claim_generation: 1,
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() } } as never);
  await runner(exec, fakeGitlab().gitlab).execute(claim);
  assert.equal(sent.length >= 1, true, "the dispositions were reported");
  const wire = JSON.stringify(sent);
  for (const f of FORMS) assert.ok(!wire.includes(f), `form leaked: ${f}`);
  assert.match(wire, /fixed; header /);
});
