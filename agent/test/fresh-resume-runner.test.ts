import { after, before, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { parseAttemptLedgerEntry } from "../src/git.js";
import type { ExecutorFactory } from "../src/runner.js";
import { quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { freshJournalKey, parseFreshJournal } from "../src/fresh-journal.js";
import { api, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { HERMETIC_VIEW, restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger } from "./helpers.js";

// issue #4628: the destination flight end to end through the real runner and git layer against the fake api: the seed, the
// receipt, the journal commit, and the stops that must never become a failed run.

installHarness();
before(() => setQuiescenceViewForTests(HERMETIC_VIEW));
after(() => restoreHermeticView());

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const GENERATION = 3;
const CHECKPOINT_REF = "refs/uzi-checkpoints/agent/issue-fresh";

const sh = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, stdio: "pipe" }).trim();
const quiesce = (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => quiesceRunAttempt(req, { docker: { intervalMs: 5, deadlineMs: 3_000, requestTimeoutMs: 2_000 } });

/** One commit ahead of main, published as the broker-owned checkpoint ref in the fixture origin. */
function publishCheckpoint(): string {
  sh(fx.originPath, ["checkout", "-q", "-B", "tmp-cp", "main"]);
  fs.writeFileSync(path.join(fx.originPath, "saved.txt"), "saved by the previous worker\n");
  sh(fx.originPath, ["add", "-A"]);
  sh(fx.originPath, ["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "work saved"]);
  const sha = sh(fx.originPath, ["rev-parse", "HEAD"]);
  sh(fx.originPath, ["update-ref", CHECKPOINT_REF, sha]);
  sh(fx.originPath, ["checkout", "-q", "main"]);
  sh(fx.originPath, ["branch", "-D", "tmp-cp"]);
  return sha;
}

function freshClaimFor(iid: number, sha: string, frid = randomUUID()) {
  return { frid, claim: gitlabClaim(iid, { claim_generation: GENERATION,
    fresh_resume: { id: frid, kind: "checkpoint_ref", checkpoint_ref: CHECKPOINT_REF, sha, evidence_generation: 1 } }) };
}

function bare(): string { return git.barePathFor(fx.originPath); }
function config(key: string): string[] {
  try { return sh(bare(), ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; }
}

function factoryFor(onRun: (ctx: RunContext) => void): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => { started++; onRun(ctx); throw new Error("stop after the model starts"); } },
  });
  return { factory, started: () => started };
}

const run = (factory: ExecutorFactory) => runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), { quiesceRun: quiesce, recoveryRetryMs: 5 });
const reported = (runId: string): string[] => api.states.filter(s => s.runId === runId).map(s => String(s.body.status));

describe("a fresh resume claim on the destination flight", () => {
  it("seeds A from the checkpoint ref, posts the receipt, commits L4 and only then starts the model", async () => {
    simulateCommittedWork();
    const sha = publishCheckpoint();
    const { frid, claim } = freshClaimFor(8101, sha);
    const branch = "agent/issue-8101";
    let atModelStart: { journal: string[]; ledger: string[]; fresh: string[]; head: string; worktree: string; saved: string } | undefined;
    const { factory, started } = factoryFor(ctx => {
      atModelStart = { journal: config(`uzi-recovery.${branch}.clone`), ledger: config(`uzi-attempts.${branch}.entry`),
        fresh: config(freshJournalKey(branch)), head: sh(ctx.worktreePath, ["rev-parse", "HEAD"]), worktree: ctx.worktreePath,
        saved: fs.readFileSync(path.join(ctx.worktreePath, "saved.txt"), "utf8") };
    });
    await run(factory).execute(claim);

    assert.equal(started(), 1);
    assert.ok(reported(claim.run_id).includes("failed"), "control: a failure after the model started IS reported, so the no-failed assertions below can fail");
    assert.ok(atModelStart, "the model started");
    assert.equal(atModelStart.head, sha, "the seeded base is the checkpoint");
    assert.equal(atModelStart.saved, "saved by the previous worker\n");
    const seeded = api.freshResumeCalls.filter(c => c.route === "seeded");
    assert.equal(seeded.length, 1);
    assert.equal(seeded[0]!.frid, frid);
    assert.equal(seeded[0]!.body.claim_generation, GENERATION);
    assert.equal(seeded[0]!.body.live_generation, GENERATION);
    assert.match(String(seeded[0]!.body.attempt_id), /-g3-/, "A is minted from the claim generation, never a gx id");
    assert.equal(api.freshResumeCalls.some(c => c.route === "abandon"), false);
    // L4 was in place before the model started: the recovery journal names A, the ledger says live, the fresh journal says ready.
    assert.deepEqual(JSON.parse(atModelStart.journal.at(-1)!), { runId: claim.run_id, clonePath: atModelStart.worktree, attemptId: seeded[0]!.body.attempt_id });
    assert.equal(parseAttemptLedgerEntry(atModelStart.ledger.at(-1)!)?.state, "live");
    assert.equal(parseFreshJournal(atModelStart.fresh.at(-1)!).stage, "ready");
    assert.equal(parseAttemptLedgerEntry(atModelStart.ledger[0]!)?.state, "fresh_pending", "L1 came first");
    // Milestones are preserved: the clear is keyed on a seed from the default branch, which this is not.
    assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.seeded_from_default === true), false);
    assert.ok(api.messages(claim.run_id).some(m => JSON.stringify(m.payload).includes("fresh resume")), "the feed says where the work came from");
  });

  it("a valid receipt without current authority stops before L4 and the model, and never reports failed", async () => {
    const sha = publishCheckpoint();
    api.freshResume = { authority: false };
    const { claim } = freshClaimFor(8102, sha);
    const branch = "agent/issue-8102";
    const { factory, started } = factoryFor(() => {});
    await run(factory).execute(claim);
    assert.equal(started(), 0, "no model start without authority");
    assert.equal(api.freshResumeCalls.filter(c => c.route === "seeded").length, 1);
    assert.equal(api.freshResumeCalls.some(c => c.route === "abandon"), false, "the transfer is seeded: it is not abandoned");
    assert.deepEqual(config(`uzi-recovery.${branch}.clone`), [], "no L4");
    assert.equal(parseAttemptLedgerEntry(config(`uzi-attempts.${branch}.entry`).at(-1)!)?.state, "fresh_pending");
    assert.equal(parseFreshJournal(config(freshJournalKey(branch)).at(-1)!).stage, "seeding");
    assert.equal(reported(claim.run_id).includes("failed"), false, "failed is never reported from a fresh stop");
  });

  it("a bare that already holds local state for the branch abandons with the claim generation, seeds nothing and reports no failure", async () => {
    const sha = publishCheckpoint();
    const { frid, claim } = freshClaimFor(8103, sha);
    const branch = "agent/issue-8103";
    const b = await git.ensureClone(fx.originPath);
    sh(b, ["config", "--local", `uzi-recovery.${branch}.clone`, JSON.stringify({ runId: "someone-else", clonePath: path.join(fx.dataDir, "runner", "x", "issue-8103") })]);
    const { factory, started } = factoryFor(() => {});
    await run(factory).execute(claim);
    assert.equal(started(), 0);
    const calls = api.freshResumeCalls.map(c => c.route);
    assert.deepEqual(calls, ["abandon"], "abandoned, and nothing else was asked of the server");
    assert.equal(api.freshResumeCalls[0]!.frid, frid);
    assert.equal(api.freshResumeCalls[0]!.body.claim_generation, GENERATION);
    assert.deepEqual(config(freshJournalKey(branch)), [], "nothing was seeded");
    assert.equal(reported(claim.run_id).includes("failed"), false);
  });

  it("an evidence ref that does not equal the server sha abandons before anything is written", async () => {
    publishCheckpoint();
    const { claim } = freshClaimFor(8104, "b".repeat(40));
    const branch = "agent/issue-8104";
    const { factory, started } = factoryFor(() => {});
    await run(factory).execute(claim);
    assert.equal(started(), 0);
    assert.deepEqual(api.freshResumeCalls.map(c => c.route), ["abandon"]);
    assert.deepEqual(config(freshJournalKey(branch)), []);
    assert.deepEqual(config(`uzi-attempts.${branch}.entry`), []);
    assert.equal(reported(claim.run_id).includes("failed"), false);
  });

  it("a receipt the server refuses abandons the transfer and marks the seeded attempt fresh_unbound", async () => {
    const sha = publishCheckpoint();
    api.freshResume = { seededRefusal: "fresh_resume_attempt_mismatch" };
    const { claim } = freshClaimFor(8105, sha);
    const branch = "agent/issue-8105";
    const { factory, started } = factoryFor(() => {});
    await run(factory).execute(claim);
    assert.equal(started(), 0);
    assert.deepEqual(api.freshResumeCalls.map(c => c.route), ["seeded", "abandon"]);
    assert.equal(parseAttemptLedgerEntry(config(`uzi-attempts.${branch}.entry`).at(-1)!)?.state, "fresh_unbound");
    assert.deepEqual(config(`uzi-recovery.${branch}.clone`), []);
    assert.equal(reported(claim.run_id).includes("failed"), false);
  });

  it("a bare whose local state cannot be read abandons the transfer instead of stopping with it claimed", async () => {
    const sha = publishCheckpoint();
    const { frid, claim } = freshClaimFor(8106, sha);
    await git.ensureClone(fx.originPath);
    mock.method(git, "readFreshLocalState", async () => { throw new Error("config unreadable"); });
    try {
      const { factory, started } = factoryFor(() => {});
      await run(factory).execute(claim);
      assert.equal(started(), 0);
      const abandons = api.freshResumeCalls.filter(c => c.route === "abandon");
      assert.equal(abandons.length >= 1, true, "the transfer was given back");
      assert.equal(abandons[0]!.frid, frid);
      assert.equal(reported(claim.run_id).includes("failed"), false);
    } finally { mock.restoreAll(); }
  });

  it("a non-fresh error before the receipt (the clone fails) still abandons the transfer and reports no failure", async () => {
    const sha = publishCheckpoint();
    const { frid, claim } = freshClaimFor(8107, sha);
    mock.method(git, "ensureClone", async () => { throw new Error("disk exploded, not a fresh error"); });
    try {
      const { factory, started } = factoryFor(() => {});
      await run(factory).execute(claim);
      assert.equal(started(), 0);
      const abandons = api.freshResumeCalls.filter(c => c.route === "abandon");
      assert.equal(abandons.length >= 1, true, "the transfer was abandoned through dispatchFailure");
      assert.equal(abandons[0]!.frid, frid);
      assert.equal(reported(claim.run_id).includes("failed"), false);
    } finally { mock.restoreAll(); }
  });

  it("a shutdown before the receipt still POSTs the abandon", async () => {
    const sha = publishCheckpoint();
    const { frid, claim } = freshClaimFor(8108, sha);
    const runner = run(factoryFor(() => {}).factory);
    mock.method(git, "seedFreshAttempt", async () => {
      (runner as unknown as { shutdownSignal: AbortController }).shutdownSignal.abort();
      throw new Error("interrupted by SIGTERM");
    });
    try {
      await runner.execute(claim);
      const abandons = api.freshResumeCalls.filter(c => c.route === "abandon");
      assert.equal(abandons.length >= 1, true, "the shutdown did not abort the abandon");
      assert.equal(abandons[0]!.frid, frid);
      assert.equal(reported(claim.run_id).includes("failed"), false);
    } finally { mock.restoreAll(); }
  });

  it("an abandon the server did not accept leaves the seeded attempt fresh_pending, never fresh_unbound", async () => {
    const sha = publishCheckpoint();
    api.freshResume = { seededRefusal: "fresh_resume_attempt_mismatch", abandonStatus: 500 };
    const { claim } = freshClaimFor(8109, sha);
    const branch = "agent/issue-8109";
    const { factory, started } = factoryFor(() => {});
    await run(factory).execute(claim);
    assert.equal(started(), 0);
    assert.equal(api.freshResumeCalls.some(c => c.route === "abandon"), true);
    assert.equal(parseAttemptLedgerEntry(config(`uzi-attempts.${branch}.entry`).at(-1)!)?.state, "fresh_pending");
    assert.equal(reported(claim.run_id).includes("failed"), false);
  });
});
