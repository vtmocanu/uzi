// PRD #4323: after a provider-bound Codex claim, the advice pass (ReviewRunner) and the summary
// passes (the RunRunner Now note and delivery editor pass) run on the FROZEN binding through the
// runners' own callbacks, with zero releaseCodex/refreshCodex and zero operation admission. The
// advice factory is injected (no real Codex); the pinned-runtime proof is the M4 row in
// e2e/codex-m4/provider-advice-summary.test.ts.

import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import type { Executor, RunContext } from "../src/executor.js";
import { GitLabClient, type FetchFn } from "../src/forge.js";
import { RunRunner } from "../src/runner.js";
import { ReviewRunner } from "../src/review-runner.js";
import { SummaryRunner } from "../src/summary-runner.js";
import type { ClaimProviderBinding, ClaimResponse } from "../src/protocol.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { CodexExecutor, CODEX_PRODUCTION_PROVIDER, createCodexEpochAuthority, type CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import { selectCodexExecutionAuthority } from "../src/codex/select.js";
import type { AdviceRequest, AdviceResult } from "../src/harness.js";

const TOKEN = "tkn-provider-passes-123456";
// Assembled at runtime: no provider-token-shaped literal in source.
const KEY = ["gw", "provider", "passes", "fixture", "9f2c"].join("-");

const provider: ClaimProviderBinding = {
  provider_id: "passes-provider", revision: 6, engine: "codex", protocol: "openai_responses",
  base_url: "https://gateway.example.test/v1", auth_style: "bearer", credential: KEY,
  model_map: {
    default: { model: "gateway/default-model", context_window: 8192 },
    "gpt-6-sol": { model: "gateway/sol-model", context_window: 32768 },
    "gpt-6-luna": { model: "gateway/luna-model", context_window: 16384 },
  },
};

let api: FakeApi;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;
let homeRoot: string;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "provider-passes-"));
});

afterEach(async () => {
  mock.restoreAll();
  fs.rmSync(homeRoot, { recursive: true, force: true });
  try {
    await api.close();
  } finally {
    fx.cleanup();
  }
});

const claimFor = (iid: number) => makeClaim({
  issue_iid: iid,
  issue_title: `Fix thing ${iid}`,
  repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
  last_seq: 0,
  secrets: { forge_pat: "fixture-forge-pat-000000", provider },
});

/** Counts any vendor credential operation on the worker client: release, refresh and admission. */
function spyVendorOperations(target: WorkerClient): () => number {
  const names = ["releaseCodex", "refreshCodex", "checkCodexOperationAdmission", "retainCodexOperationFlight"] as const;
  const spies = names.map(name => mock.method(target as never, name, (() => { throw new Error(`unexpected vendor operation ${name}`); }) as never));
  return () => spies.reduce((sum, spy) => sum + spy.mock.callCount(), 0);
}

function recordingFactory(rec: { builds: Parameters<CodexAdviceHarnessFactory>[0][]; requests: AdviceRequest[] }, text: (req: AdviceRequest) => string): CodexAdviceHarnessFactory {
  return (async (params: Parameters<CodexAdviceHarnessFactory>[0]) => {
    rec.builds.push(params);
    return {
      kind: "codex",
      async run(request: AdviceRequest): Promise<AdviceResult> {
        rec.requests.push(request);
        return { text: text(request), end: { kind: "terminal", terminal: { outcome: "success", subtype: "success", errors: [],
          metrics: { cost: { kind: "unreported" } } } } } as unknown as AdviceResult;
      },
    };
  }) as unknown as CodexAdviceHarnessFactory;
}

function assertFrozenBinding(params: Parameters<CodexAdviceHarnessFactory>[0], expected: unknown): void {
  assert.equal(params.binding.authMode, "provider");
  if (params.binding.authMode !== "provider") return;
  assert.deepEqual(params.binding, expected, "the pass runs on the executor-bound frozen authority");
  assert.ok(Object.isFrozen(params.binding.providerBinding), "the binding is frozen");
  assert.ok(Object.isFrozen(params.binding.providerBinding.model_map["gpt-6-sol"]), "each mapped entry is frozen");
  assert.equal(params.binding.providerBinding.credential, KEY);
  assert.equal(params.binding.providerBinding.revision, 6);
}

function commitMarker(worktreePath: string): void {
  fs.writeFileSync(path.join(worktreePath, "UZI_RUN.md"), "# provider passes\n");
  execFileSync("git", ["add", "UZI_RUN.md"], { cwd: worktreePath });
  execFileSync("git", ["-c", "user.name=uzi-agent", "-c", "user.email=uzi-agent@uzi.local", "-c", "commit.gpgsign=false", "commit", "-m", "uzi test"], { cwd: worktreePath });
}

describe("provider-bound Codex claim passes (PRD #4323)", () => {
  it("the RunRunner Now summary and delivery summary run on the frozen binding with zero vendor operations", async () => {
    const claim = claimFor(4323);
    api.nowSummary.set(claim.run_id, true);
    const vendorOps = spyVendorOperations(client);
    const rec = { builds: [] as Parameters<CodexAdviceHarnessFactory>[0][], requests: [] as AdviceRequest[] };
    const forbiddenClaude = (() => assert.fail("Claude route")) as unknown as SdkQueryFn;
    const raw = recordingFactory(rec, () => JSON.stringify({ summary: "Bound summary.", deltas: [] }));
    const selection = selectCodexExecutionAuthority(claim.secrets);
    assert.equal(selection.kind, "codex");
    if (selection.kind !== "codex") throw new Error("Codex provider fixture binding missing");
    const executor = new CodexExecutor(nullLogger(), fx.dataDir, { binding: selection.binding, client, provider: CODEX_PRODUCTION_PROVIDER });
    const notes = () => api.messages(claim.run_id).filter(m => m.kind === "progress_note");
    const fakeGitlab = new GitLabClient({ fetchFn: (async () => ({
      status: 201,
      text: async () => JSON.stringify({ iid: 42, web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42" }),
    })) as FetchFn });
    executor.run = async (ctx: RunContext) => {
      // Model the run-entry publication while retaining the production summary composition.
      (executor as unknown as { currentCodexAuthority: ReturnType<typeof createCodexEpochAuthority> })
        .currentCodexAuthority = createCodexEpochAuthority(selection.binding);
      const until = Date.now() + 4000;
      await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
      while (notes().length === 0 && Date.now() < until) {
        ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "task gate:api", description: "Run the api gate" } } });
        await new Promise(r => setTimeout(r, 25));
      }
      commitMarker(ctx.worktreePath);
      return { branch: ctx.branch };
    };
    const runner = new RunRunner(client, git, () => ({ executor: executor as Executor }), nullLogger(), 20, undefined, {
      pollMs: 5,
      planApprovalTimeoutMs: 0,
      gitlab: fakeGitlab,
      nowSummary: { homeRoot, queryFn: forbiddenClaude, codexAdviceHarnessFactory: raw },
      summaryRunner: new SummaryRunner(nullLogger(), { homeRoot, queryFn: forbiddenClaude, codexAdviceHarnessFactory: raw }),
    });
    await runner.execute(claim);

    assert.equal(notes().length, 1, "the Now summary pass produced its note");
    assert.ok(rec.builds.length >= 1, "at least the Now summary built a harness");
    const now = rec.builds.find(b => b.refresh === "deny");
    assert.ok(now, "the Now summary denies refresh");
    assertFrozenBinding(now, selection.binding);
    assert.ok(rec.builds.some(b => b.refresh === undefined), "the delivery editor pass built a harness at publication");
    for (const build of rec.builds) assertFrozenBinding(build, selection.binding);
    assert.equal(rec.requests.length, rec.builds.length, "one model request per built harness");
    assert.equal(rec.requests[0]!.model, "gpt-6-luna", "the summary model resolves to a role slot the bound map serves");
    assert.equal(vendorOps(), 0, "no release, refresh or operation admission on the worker client");
  });

  it("the ReviewRunner advice pass runs on the frozen binding with zero vendor operations", async () => {
    const vendorOps = spyVendorOperations(client);
    const rec = { builds: [] as Parameters<CodexAdviceHarnessFactory>[0][], requests: [] as AdviceRequest[] };
    const factory = recordingFactory(rec, () => JSON.stringify({ summary: "Reviewed.", findings: [] }));
    const posted: { status: string }[] = [];
    const reviewClient = {
      reportState: async () => {},
      postTaskReview: async (_id: string, review: { status: string }) => { posted.push(review); },
      releaseCodex: client.releaseCodex, refreshCodex: client.refreshCodex,
      checkCodexOperationAdmission: client.checkCodexOperationAdmission, retainCodexOperationFlight: client.retainCodexOperationFlight,
    } as unknown as WorkerClient;
    const claim = { run_id: "provider-review", claim_generation: 2, review_target_run_id: "target", branch: "branch", base_branch: "main",
      repo: { clone_url: "/origin" }, task_review_model: "gpt-6-sol", secrets: { forge_pat: "test-pat", provider } };
    const reviewGit = { ensureClone: async () => "/bare", reviewDiff: async () => "+change" } as unknown as GitCache;
    await new ReviewRunner(reviewClient, reviewGit, nullLogger(), {
      homeRoot, queryFn: (() => assert.fail("Claude route")) as unknown as SdkQueryFn, codexAdviceHarnessFactory: factory,
    }).execute(claim as unknown as ClaimResponse);

    assert.deepEqual(posted.map(r => r.status), ["complete"]);
    assert.equal(rec.builds.length, 1);
    const selection = selectCodexExecutionAuthority(claim.secrets);
    assertFrozenBinding(rec.builds[0]!, selection.kind === "codex" ? selection.binding : undefined);
    assert.equal(rec.builds[0]!.runId, "provider-review");
    assert.equal(rec.builds[0]!.refresh, undefined, "a review pass owns its call; it never borrows a run credential");
    assert.equal(rec.requests.length, 1);
    assert.equal(rec.requests[0]!.model, "gpt-6-sol");
    assert.equal(vendorOps(), 0, "no release, refresh or operation admission on the worker client");
  });
});
