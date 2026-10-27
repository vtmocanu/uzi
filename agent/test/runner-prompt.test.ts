import { afterEach, describe, it } from "node:test";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import assert from "node:assert/strict";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import type { ClaimResponse, RunKind } from "../src/protocol.js";
import {
  api,
  fx,
  fakeGitlab,
  git,
  installHarness,
  runner,
} from "./runner-harness.js";

installHarness();

// A schedule-fired ad-hoc prompt run (PRD #241 Decision 10 / M8): kind="prompt",
// ISSUE-LESS (issue_iid null), repo set, and issue_description carrying the schedule's
// stored task text. It must claim + execute on a derived per-run branch and open an MR
// whose title + description never render `#null` — the ci_fix shape, not the issue path.
function promptClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    kind: "prompt",
    issue_iid: null,
    issue_title: "Hunt for flaky tests and open an MR",
    issue_description:
      "Find flaky tests across the poller package and open a merge request.",
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

describe("RunRunner — ad-hoc prompt kind (PRD #241 M8)", () => {
  it("the RunKind union accepts \"prompt\"", () => {
    // Compile-time proof the kind is in the union (the assignment fails typecheck
    // otherwise); the runtime assert keeps the test non-empty.
    const k: RunKind = "prompt";
    assert.equal(k, "prompt");
  });

  it("clones onto a derived per-run branch and opens an MR with no #null text", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = promptClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The run completed via the seam (not the issue-path throw), on the derived branch.
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.equal(completed.branch, `uzi/prompt-${claim.run_id}`);
    assert.equal(completed.mr_iid, 42);

    // The MR the worker opened.
    assert.equal(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.source_branch, `uzi/prompt-${claim.run_id}`);

    // Title comes from the scheduler-derived issue_title — never `Work on issue #null`.
    assert.equal(body.title, "Hunt for flaky tests and open an MR");
    assert.ok(!String(body.title).includes("#null"));

    // The description references the ad-hoc prompt run and, crucially, never renders
    // `#null` and never `Closes` an issue (there is none).
    assert.ok(body.description.includes("Ad-hoc scheduled prompt run"));
    assert.ok(!body.description.includes("#null"), "no #null in MR body");
    assert.ok(!/Closes #/.test(body.description), "prompt run closes no issue");
  });

  it("falls back to a non-#null title when the scheduler set no title", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = promptClaim({ issue_title: "" });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.title, "Scheduled prompt run");
    assert.ok(!String(body.title).includes("#null"));
  });
});

// PRD #929 M2 (agent/worker SEND half): a scheduled prompt run whose deliverable is a
// structured proposal (no code to land) completes report-only and carries the proposal on
// the terminal `completed` report, so the server can file it as a forge issue. The zero-diff
// (report-only) prompt terminal is the send site (runner.ts ~1152). StubExecutor makes a real
// commit so the agent branch fetches back; overriding changedFiles to [] drives the
// confirmed-empty (report-only) leg deterministically.
describe("RunRunner — prompt run proposal delivery (PRD #929 M2)", () => {
  const tmpRoots: string[] = [];
  afterEach(async () => {
    for (const root of tmpRoots.splice(0)) await fs.rm(root, { recursive: true, force: true });
  });

  async function mkOutbox(): Promise<Outbox> {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "runner-prompt-journal-"));
    tmpRoots.push(dir);
    const outbox = new Outbox({
      root: path.join(dir, "outbox"),
      log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024,
      retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    return outbox;
  }

  it("#3469 control runner journals exact executor summary on confirmed-empty prompt completion after 409 running", async () => {
    const { gitlab, calls } = fakeGitlab();
    const outbox = await mkOutbox();
    const claim = promptClaim({ claim_generation: 8 });
    const summary = "Prompt findings\n\nNo repair needed.";
    const stub = new StubExecutor(nullLogger());
    const executor: Executor = {
      run: async (ctx) => ({ ...await stub.run(ctx), summary }),
    };
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    api.failStateWhen(claim.run_id, (body) => body.status === "completed", { httpStatus: 409, runStatus: "running" });

    await runner(executor, gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(outbox.hasPendingTerminal(claim.run_id, 8), true);
    const journal = await outbox.readTerminalJournal(claim.run_id, 8);
    assert.ok(journal);
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 7), false, "journal belongs to this claim generation");
    assert.equal(journal.body.status, "completed");
    assert.equal(journal.body.report_only, true);
    assert.equal(journal.body.report_md, summary);
    assert.ok(!("mr_iid" in journal.body));
    assert.equal(calls.length, 0, "no MR opened on confirmed-empty prompt completion");
  });

  it("#3469 control runner journals exact summary from a genuine no-commit prompt after 409 running", async () => {
    const { gitlab, all } = fakeGitlab();
    const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
    await fs.mkdir(scratch, { recursive: true });
    const dir = await fs.mkdtemp(path.join(scratch, "runner-prompt-no-commit-journal-"));
    tmpRoots.push(dir);
    const outbox = new Outbox({
      root: path.join(dir, "outbox"),
      log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024,
      retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    const claim = promptClaim({ claim_generation: 9 });
    const summary = "Prompt findings\n\nNo commits or repairs needed.";
    let invocations = 0;
    let headBefore: string | null = null;
    let headAfter: string | null = null;
    const executor: Executor = {
      run: async (ctx) => {
        invocations++;
        headBefore = await git.worktreeHead(ctx.worktreePath);
        assert.ok(headBefore, "executor starts at a real commit");
        assert.equal(headBefore, ctx.baseCommit, "fresh prompt starts at its base commit");
        headAfter = await git.worktreeHead(ctx.worktreePath);
        assert.equal(headAfter, headBefore, "summary-only work leaves HEAD unchanged");
        return { branch: ctx.branch, summary };
      },
    };
    api.failStateWhen(claim.run_id, (body) => body.status === "completed", { httpStatus: 409, runStatus: "running" });

    await runner(executor, gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    assert.equal(invocations, 1, "summary executor was invoked");
    assert.ok(headBefore);
    assert.equal(headAfter, headBefore);
    const barePath = git.barePathFor(fx.originPath);
    const branch = `uzi/prompt-${claim.run_id}`;
    assert.equal(await git.trackingTip(barePath, branch), headBefore, "fetch-back contains no new commits");
    assert.deepEqual(await git.changedFiles(barePath, headBefore), [], "real Git diff confirms no changed files");
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 9), true);
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 8), false, "journal belongs to this claim generation");
    const journal = await outbox.readTerminalJournal(claim.run_id, 9);
    assert.ok(journal);
    assert.equal(journal.body.status, "completed");
    assert.equal(journal.body.report_only, true);
    assert.equal(journal.body.report_md, summary);
    assert.ok(!("mr_iid" in journal.body));
    assert.deepEqual(all, [], "no forge MR reads or writes on no-commit prompt completion");
  });

  /** Wrap StubExecutor so the branch is really created (fetchAgentBranch succeeds) while the
   *  ExecutorResult also carries a proposal, exactly as the SDK executor forwards one. */
  function proposalExecutor(
    proposal: { title: string; body: string } | undefined,
  ): Executor {
    const stub = new StubExecutor(nullLogger());
    return {
      run: async (ctx) => {
        const result = await stub.run(ctx);
        return proposal === undefined ? result : { ...result, proposal };
      },
    };
  }

  it("INCLUDES the proposal on the report-only completion when the agent set one", async () => {
    const { gitlab, calls } = fakeGitlab();
    // Confirmed-empty diff ⇒ the zero-diff report-only terminal (no branch push, no MR).
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    const proposal = {
      title: "Flaky poller tests",
      body: "`poller_test.go` is timing-sensitive; propose a deterministic clock.",
    };
    const claim = promptClaim();
    await runner(proposalExecutor(proposal), gitlab).execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.report_only, true);
    assert.deepStrictEqual(completed.proposal, proposal);
    // A proposal delivery is report-only: no branch pushed, no MR opened.
    assert.strictEqual(calls.length, 0, "no MR opened on a proposal (report-only) run");
    assert.ok(
      !("mr_iid" in completed) || completed.mr_iid === undefined,
      "no MR iid on a report-only completion",
    );
  });

  it("OMITS the proposal key entirely on an ordinary zero-diff prompt completion", async () => {
    const { gitlab, calls } = fakeGitlab();
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    const claim = promptClaim();
    // No proposal from the executor ⇒ the completion payload must be byte-identical to today.
    await runner(proposalExecutor(undefined), gitlab).execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.report_only, true);
    assert.ok(
      !("proposal" in completed),
      "proposal absent (not null/undefined) when the agent produced none",
    );
    assert.strictEqual(calls.length, 0);
  });

  it("OMITS the proposal key on a normal push+MR prompt completion", async () => {
    // The committing prompt run falls through to push/MR; a proposal has no place there and
    // the completed report stays byte-identical to today's mr-mode shape.
    const { gitlab } = fakeGitlab();
    const claim = promptClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.ok(!("proposal" in completed), "no proposal key on a normal mr-mode completion");
  });
});
