import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";

// PRD #4416 M2: a cross-check child never receives the lead's variables, but it is sent the lead's
// secret versions (secrets.run_env_redact) so that what it quotes is masked everywhere it leaves.
const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
await fs.mkdir(scratch, { recursive: true });
const LEAD_SECRET = "lead-run-env-" + "value-7Qx2Lm9Kp4";
const MASK = "***REDACTED***";

function claim(stage: "plan" | "code"): ClaimResponse {
  const base = { kind: "cross_check", run_id: "child", claim_generation: 7, last_seq: 0, agents: [],
    issue_iid: null, issue_title: "Issue", issue_description: "Issue DATA",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo",
      clone_url: "https://example.test/repo.git", default_branch: "main" },
    config: { default_model: "sonnet", default_effort: "high" },
    secrets: { forge_pat: "", anthropic_oauth_token: "fixture-token",
      run_env_redact: [{ var_id: "v1", rev: 1, name: "LEAD_TOKEN", value: LEAD_SECRET }] },
  };
  const cross_check = stage === "code"
    ? { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64),
      base_commit: "a".repeat(40), head_commit: "b".repeat(40), plan_md: "Approved plan DATA",
      guidance_snapshot: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
      code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" }
    : { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest",
      deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan", milestones: [], required_capabilities: [],
      required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" };
  return { ...base, cross_check } as unknown as ClaimResponse;
}

for (const stage of ["plan", "code"] as const) {
  it(`${stage} cross-check masks a lead secret the checker quotes, in the status feed and the reported verdict`, async () => {
    const root = await fs.mkdtemp(path.join(scratch, "cross-check-run-env-"));
    const posted: unknown[] = [];
    const verdicts: unknown[] = [];
    const registered = new Set<string>();
    const seenWhileRunning: string[] = [];
    const log = { ...nullLogger(), addSecret: (s: string) => { registered.add(s); }, removeSecret: (s: string) => { registered.delete(s); } };
    const quoted = `the checker quotes ${LEAD_SECRET} here`;
    const text = stage === "code"
      ? JSON.stringify({ findings: [{ id: "F_1", severity: "major", path: "anchor.ts", line: 1, title: "Defect", detail: quoted }] })
      : JSON.stringify({ verdict: "approve", summary: quoted, items: [] });
    const model = { run: async (_c: ClaimResponse, _checkout: string, _home: string, _signal: AbortSignal,
      postUsage: (p: Record<string, unknown>) => Promise<void>, confirm?: () => void) => {
      seenWhileRunning.push(...registered);
      await postUsage({ event: "result", note: quoted });
      confirm?.();
      return text;
    } };
    const client = {
      reportState: async () => ({ applied: true }), getInputs: async () => ({ inputs: [] }),
      postMessages: async (...args: unknown[]) => { posted.push(args); },
      reportCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => { verdicts.push(v); },
      reportCodeCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => { verdicts.push(v); },
      getCodeSnapshotCleanup: async () => { throw new Error("unused"); },
    } as unknown as WorkerClient;
    const git = {
      ensureClone: async () => "/bare", barePathFor: () => "/bare", existingBarePathFor: async () => "/bare",
      runnerCloneAtCommit: async () => root, readBare: async () => ({ text: "diff", truncated: false }),
      removeRunnerClone: async () => {},
    } as unknown as GitCache;
    try {
      await new CrossCheckRunner(client, git, log as never, { homeRoot: root, pollMs: 1, batchMs: 5, modelTimeoutMs: 1000,
        claudeModel: model, model: { run: async () => { assert.fail("wrong family"); } } }).execute(claim(stage));
      assert.ok(seenWhileRunning.includes(LEAD_SECRET), "the lead secret is registered with the logger during the check");
      assert.equal(verdicts.length, 1, "one verdict reported");
      assert.ok(JSON.stringify(verdicts[0]).includes(MASK), "the verdict keeps its text with the secret masked");
      assert.ok(!JSON.stringify(verdicts[0]).includes(LEAD_SECRET), "the reported verdict never carries the lead secret");
      assert.ok(JSON.stringify(posted).includes(MASK), "the status feed row is still delivered, masked");
      assert.ok(!JSON.stringify(posted).includes(LEAD_SECRET), "the status feed never carries the lead secret");
      assert.equal(registered.size, 0, "every registration is evicted when the check ends");
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
}
