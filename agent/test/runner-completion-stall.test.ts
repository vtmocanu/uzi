import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import { REASON_COMPLETION_STALL } from "../src/completion-attempt.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import {
  api, client, git, fx, fakeGitlab as rawFakeGitlab, gitlabClaim, input, installHarness, runner,
} from "./runner-harness.js";

installHarness();
beforeEach(async () => {
  api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
  await client.register("completion-stall");
});

// Issue #3139 M4: a worker-initiated no-progress stall attempt runs the done-path sequence
// (checkpoint with the done_checkpoint sink, fingerprint, empty-declaration completion attempt,
// completion question) WITHOUT signalling done. The stall attempt's done_checkpoint may legitimately
// capture the stall-time head H0 as its candidate and code snapshot. What must hold, on both lanes
// (Claude: no executor.safety; Codex: executor.safety), is that no later publication, finalize or
// code cross-check authorization consumes that stall-time state: everything published and reviewed
// afterwards comes from the later real done checkpoint at H1, and no done permit stays held once the
// stall attempt returns. The executors below are scripted Executors performing the runner-facing ctx
// call sequence of the SdkExecutor / Codex stall path (done_checkpoint, fingerprint, an empty-declaration
// recordCompletionAttempt, then askCompletionQuestion(unmet, { kind: "no_progress_stall" })); executor-internal
// steps (plan-summary settlement, Codex session persistence) are omitted. The runner behind ctx is real.

const record = (head: string, base: string) => ({
  stage: "code" as const, round: 1 as const, candidate_generation: 1, head_commit: head, base_commit: base,
  candidate_digest: "c".repeat(64), checker_run_id: null, checker_harness: "codex", checker_model: "test",
  checker_effort: null, outcome: "completed" as const, reason_class: null, findings: [],
  interrupted_at: null, finalized_at: new Date().toISOString(), deadline_at: new Date(Date.now() + 1000).toISOString(),
});

function commit(ctx: RunContext, file: string): string {
  fs.writeFileSync(path.join(ctx.worktreePath, file), `${file}\n`);
  execFileSync("git", ["-C", ctx.worktreePath, "add", file]);
  execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-qm", file]);
  return execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
}

const captureAttempt = new AsyncLocalStorage<Parameters<NonNullable<Executor["withCredentialFreeCapture"]>>[4]>();
/** `provider: false` models a resumed flight whose fresh executor has no live provider root yet. */
function codeSafety(provider = true) {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const reservation = provider ? registry.reserveLaunch("provider") : undefined;
  if (reservation) assert.equal(reservation.kind, "reserved");
  if (reservation?.kind === "reserved") registry.registerRoot(reservation.reservation, {
    kind: "provider", reap: async () => ({ ok: true }), dispose: async () => {},
  });
  return createCodexExecutionSafety(registry,
    async () => { throw new Error("unused root spawn"); }, undefined, undefined,
    async request => {
      const [command, ...args] = request.argv;
      assert.ok(command);
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => { await terminal; return { ok: true }; },
          dispose: async () => {
            if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
            await terminal.catch(() => undefined);
          },
        },
        stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
        waitChild: async () => terminal,
      };
    }, undefined, undefined, { reaperAttempt: () => captureAttempt.getStore() });
}

const codexSecrets = { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", capability: "fixture-cap", access_token: "fixture-access" } };

/** Rejects if `action` does not settle promptly: a held done permit or sink gate would park it. The bound is
 *  generous (observed well under 2s) so a loaded CI runner does not redden it; a real hang still fails here. */
async function promptly(label: string, action: () => Promise<unknown>): Promise<void> {
  let timer: NodeJS.Timeout | undefined;
  try {
    await Promise.race([
      action(),
      new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error(`${label} did not acquire promptly`)), 30_000); }),
    ]);
  } finally { clearTimeout(timer); }
}

interface Observed {
  h0?: string;
  h1?: string;
  /** Facts captured the instant the stall attempt returned. */
  afterStall?: { decision: string; originBranchPresent: boolean; forgeCalls: number; completed: boolean };
}

const progress = { completed: ["m1"], in_progress: [] };

/** The stall path's ctx sequence at the current head (H0). Returns the owner outcome. */
async function stallAttempt(ctx: RunContext, seen: Observed, forgeCalls: () => number): Promise<{ outcome: "continue" | "expired" }> {
  seen.h0 = commit(ctx, "stall-work.txt");
  await ctx.checkpoint!({ reap: true, progress, sink: "done_checkpoint" });
  const fingerprint = await ctx.worktreeFingerprint!();
  assert.ok(fingerprint);
  const head = fingerprint.split("\n", 1)[0]!;
  assert.equal(head, seen.h0);
  const { unmet } = await ctx.recordCompletionAttempt!({ declared: [], head, worktreeFingerprint: fingerprint });
  const decision = await ctx.askCompletionQuestion!(unmet, { kind: "no_progress_stall" });
  let originBranchPresent = true;
  try { execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", `refs/heads/${ctx.branch}`], { stdio: "ignore" }); }
  catch { originBranchPresent = false; }
  seen.afterStall = {
    decision: decision.outcome, originBranchPresent, forgeCalls: forgeCalls(),
    completed: api.states.some(s => s.runId === ctx.runId && s.body.status === "completed"),
  };
  // No done permit may stay held: later gated sinks (a milestone checkpoint, an iteration checkpoint) acquire.
  await promptly("milestone checkpoint after the stall attempt", () => ctx.checkpoint!({ reap: true, progress }));
  await promptly("iteration checkpoint after the stall attempt", () => ctx.checkpoint!({ reap: false, progress }));
  return decision;
}

/** The lead's real signal_done sequence at a fresh head (H1), ending in the code cross-check gate. */
async function realDone(ctx: RunContext, seen: Observed): Promise<void> {
  seen.h1 = commit(ctx, "final-work.txt");
  assert.notEqual(seen.h1, seen.h0);
  await ctx.checkpoint!({ reap: true, progress, sink: "done_checkpoint" });
  const fingerprint = await ctx.worktreeFingerprint!();
  assert.ok(fingerprint);
  await ctx.recordCompletionAttempt!({ declared: ["m1", "m2"], head: fingerprint.split("\n", 1)[0]!, worktreeFingerprint: fingerprint });
  assert.deepEqual(await ctx.codeCrossCheckGate!({ interlocked: true }), { action: "proceed" });
}

type Lane = "claude" | "codex";

function lead(lane: Lane, body: (ctx: RunContext) => Promise<ExecutorResult>, resumed = false): Executor {
  let safety = lane === "codex" ? codeSafety(!resumed) : undefined;
  const capture: NonNullable<Executor["withCredentialFreeCapture"]> = (ms, action, _purpose, signal, attempt) =>
    captureAttempt.run(attempt, () => safety!.withCredentialFreeCapture(ms, action, signal));
  const executor: { -readonly [K in keyof Executor]: Executor[K] } = {
    ...(safety ? { safety } : { killAgentTree: () => {} }),
    // Retained recovery requires a callback-held capture, including the runner's exact
    // attempt metadata. A one-shot observed_empty settlement is not capture authority.
    ...(resumed && lane === "codex" ? {
      settleForCredentialFreeCapture: (ms: number) => safety!.settleForCredentialFreeCapture(ms),
      withCredentialFreeCapture: capture,
    } : {}),
    run: async ctx => {
      // Recovery disposes the empty registry it settled; the run itself starts a fresh one, as a real Codex executor does.
      if (resumed && lane === "codex") executor.safety = safety = codeSafety(true);
      return body(ctx);
    },
  };
  return executor;
}

for (const lane of ["claude", "codex"] as const) {
  for (const decision of ["continue", "accept", "partial", "expiry"] as const) {
    describe(`${lane} lane: stall attempt then ${decision}`, () => {
      it("publishes and cross-checks only the later real done checkpoint", { timeout: 60000 }, async (t) => {
        const iid = 3139_000 + (lane === "codex" ? 100 : 0) + ["continue", "accept", "partial", "expiry"].indexOf(decision);
        t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
        const submitted: { head_commit: string; base_commit: string }[] = [];
        t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<WorkerClient["submitCodeCrossCheck"]>[2]) => {
          assert.ok("head_commit" in snapshot);
          submitted.push({ head_commit: snapshot.head_commit, base_commit: snapshot.base_commit });
          return record(snapshot.head_commit, snapshot.base_commit);
        });
        const pins: string[] = [];
        const realPin = git.pinCodeSnapshot.bind(git);
        t.mock.method(git, "pinCodeSnapshot", async (...args: Parameters<typeof git.pinCodeSnapshot>) => {
          pins.push(args[2]);
          return realPin(...args);
        });

        const forge = rawFakeGitlab();
        followSuccessfulPush(git, forge.pr);
        const claim = gitlabClaim(iid, {
          code_cross_check_required: true, claim_generation: 7,
          config: { completion_contract_version: 1, contract_revision: 1, completion_hold_window_seconds: decision === "expiry" ? 0.05 : 30 },
          ...(lane === "codex" ? { secrets: codexSecrets } : {}),
        });
        api.setPublicationState(claim.run_id, [], 1);

        // The server's view: the stall attempt sees one unmet milestone; once the owner decided (or the
        // lead's real done arrives) nothing is unmet. accept/partial are applied server-side by the
        // owner decision and arrive at the worker as the same live-window answer plus a refreshed
        // contract revision and permit scope (same seam as the issue3089 live partial/accept tests).
        let answered = false;
        let realDoneStarted = false;
        api.completionAttemptWire = () => ({
          unmet: answered || realDoneStarted ? [] : ["m2"], attempt_count: api.completionAttemptRequests.length,
        });
        const decided = decision === "accept" || decision === "partial";
        if (decided) {
          api.completionPermitWire = body => ({
            granted: true,
            permit: { id: "stall-permit", contract_revision: 2, branch: body.branch, head: body.head,
              issued_at: "2026-10-17T00:00:00Z", finding_ids: [],
              completion_scope: decision === "partial"
                ? { deferred: [{ milestone_id: "m2", title: "Deferred integration", reason: "Owner postponed integration" }] }
                : { accepted: [{ id: "m2.c1", milestone_id: "m2", text: "Integration check", reason: "Owner accepts check" }] } },
          });
        }
        if (decision !== "expiry") {
          api.onState(claim.run_id, body => {
            if (body.status !== "awaiting_input" || !body.completion_question || !body.open_question_id) return;
            answered = true;
            if (decided) api.setPublicationState(claim.run_id, decision === "partial" ? [{ milestone_id: "m2" }] : [], 2);
            api.setInputs(claim.run_id, [input("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["continue"] }))]);
          });
        }

        const seen: Observed = {};
        const forgeCalls = () => forge.calls.length;
        let permitRequestsBeforeRealDone = -1;
        let submitsBeforeRealDone = -1;
        if (decision !== "expiry") {
          await runner(lead(lane, async ctx => {
            const outcome = await stallAttempt(ctx, seen, forgeCalls);
            assert.equal(outcome.outcome, "continue");
            permitRequestsBeforeRealDone = api.completionPermitRequests.length;
            submitsBeforeRealDone = submitted.length;
            realDoneStarted = true;
            await realDone(ctx, seen);
            return { branch: ctx.branch };
          }), forge.gitlab).execute(claim);
        } else {
          // Expiry: the stall attempt's window elapses, the lead's flight holds with REASON_COMPLETION_STALL.
          let held: boolean | undefined;
          await runner(lead(lane, async ctx => {
            const outcome = await stallAttempt(ctx, seen, forgeCalls);
            assert.equal(outcome.outcome, "expired");
            held = await ctx.enterCompletionHold!(REASON_COMPLETION_STALL);
            return { branch: ctx.branch, ...(held ? { completionHeld: { reason: REASON_COMPLETION_STALL } } : {}) };
          }), forge.gitlab).execute(claim);
          assert.equal(held, true, "the stall expiry entered the completion hold");
          assert.equal(api.completionHoldRequests.length, 1);
          assert.equal(forge.calls.length, 0, "the hold published nothing");
          assert.equal(api.completionPermitRequests.length, 0, "the hold requested no permit");
          assert.equal(submitted.length, 0, "the hold ran no code cross-check");
          assert.ok(!api.states.some(s => s.runId === claim.run_id && s.body.status === "completed"));
          // Owner resume: a NEW flight (new runner, new claim) on the same run; its real signal_done.
          const resumed: Observed = { h0: seen.h0 };
          // The owner's resume: a new claim (next generation) on the same run, which finds the run running.
          api.setOwnershipStatus(claim.run_id, "running", 8);
          answered = true;
          permitRequestsBeforeRealDone = api.completionPermitRequests.length;
          submitsBeforeRealDone = submitted.length;
          realDoneStarted = true;
          // Codex lane: the resumed claim drops secrets.codex so the runner skips the command-sandbox
          // readiness probe this fixture cannot satisfy. The resumed flight keeps Codex safety; the claim-side
          // branches skipped (published-start bookkeeping, session-store probe, harness label) feed neither
          // publication nor the code cross-check, so the H1 provenance asserted below is unaffected.
          await runner(lead(lane, async ctx => {
            await realDone(ctx, resumed);
            return { branch: ctx.branch };
          }, true), forge.gitlab).execute({ ...claim, claim_generation: 8, ...(lane === "codex" ? { secrets: { forge_pat: "fixture-pat" } } : {}) });
          seen.h1 = resumed.h1;
        }

        // The stall attempt itself published and finalized nothing.
        assert.deepEqual(
          { originBranchPresent: seen.afterStall!.originBranchPresent, forgeCalls: seen.afterStall!.forgeCalls, completed: seen.afterStall!.completed },
          { originBranchPresent: false, forgeCalls: 0, completed: false },
        );
        assert.equal(permitRequestsBeforeRealDone, 0, "no permit was requested before the real done");
        assert.equal(submitsBeforeRealDone, 0, "no code cross-check was submitted before the real done");

        // Everything reviewed and published afterwards is the real done checkpoint's head H1.
        const { h0, h1 } = seen;
        assert.ok(h0 && h1 && h0 !== h1, JSON.stringify(api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed").map(s => s.body.failure_reason)));
        assert.deepEqual(submitted.map(s => s.head_commit), [h1], "the cross-check reviewed H1, never the stall-time H0");
        assert.ok(pins.includes(h1), "the review snapshot was pinned at H1");
        assert.equal(api.completionPermitRequests.length, 1);
        assert.equal(api.completionPermitRequests[0]!.body.head, h1, "the permit binds H1");
        const published = execFileSync("git", ["-C", fx.originPath, "rev-parse", `refs/heads/agent/issue-${iid}`], { encoding: "utf8" }).trim();
        assert.equal(published, h1, "the published branch tip is H1");
        assert.equal(forge.pr.head, h1, "the merge request head is H1");
        assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "completed" && s.body.head === h1),
          "the run completed with head H1");
      });
    });
  }
}
