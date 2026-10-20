import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { followSuccessfulPush } from "./publication-fixture.js";
import { it } from "node:test";
import type { Executor } from "../src/executor.js";
import type { CodeCrossCheckRecord, CodeCrossCheckStatus } from "../src/code-cross-check-contract.js";
import type { OwedCandidateContext } from "../src/git.js";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

type Submission = Parameters<typeof client.submitCodeCrossCheck>[2];
type ImportArgs = Parameters<typeof git.fetchAgentBranch>;
type ImportResult = Awaited<ReturnType<typeof git.fetchAgentBranch>>;
type Phase = "initial" | "fresh" | "finalize";
const scenarios = [
  "changed clone", "both soft stops", "cross-check off",
  "null head normal H1", "unreadable head normal H1",
  "null head normal null", "unreadable head normal null",
  "cold context", "contract-enabled both soft stops", "independent done allowances",
] as const;

// Every real import and ownership read below uses the harness GitCache. Only the named
// initial/fresh soft stops and initial head read are injected; finalize is never stubbed.
for (const scenario of scenarios) {
  it(`m3 snapshot retry (${scenario === "contract-enabled both soft stops" || scenario === "independent done allowances" ? "contract finalize" : "ordinary finalize"}): ${scenario}`, async t => {
    const budgeted = scenario === "independent done allowances";
    const contractEnabled = scenario === "contract-enabled both soft stops" || budgeted;
    const bothSoftStops = scenario === "both soft stops" || contractEnabled;
    const clock = virtualBoundaryClock();
    if (budgeted) t.mock.method(Date, "now", clock.now);
    const startedAt = clock.now();
    const softArms: number[] = [], maskedChildBudgets: number[][] = [[], []];
    const mutationJumps: Phase[] = [], stoppedErrors: unknown[] = [];
    const hardAttempts: number[] = [], permits: unknown[] = [];
    let maskedAttempt = -1;
    const stoppedStates: Array<{ tracking: string | null; owned: Awaited<ReturnType<typeof git.committedTrackingOwnership>>;
      stages: string[]; source: string | null }> = [];
    const enabled = scenario !== "cross-check off";
    const missingHead = scenario.startsWith("null head") || scenario.startsWith("unreadable head");
    const nullCompletion = scenario.endsWith("normal null");
    const expectedFailure = scenario === "changed clone" || bothSoftStops || nullCompletion;
    const fetch = git.fetchAgentBranch.bind(git);
    const tip = git.branchTip.bind(git);
    const originTip = git.originBranchTip.bind(git);
    let phase: Phase = "initial";
    let active = false, headStopped = false, contextStopped = false;
    let h1 = "", h2 = "", base = "", movedDefault = "", branch = "", runId = "", bare = "";
    let modelTurns = 0;
    let persisted: CodeCrossCheckStatus = { stage: "code", result: "no_row", repo_instructions_enabled: false };
    const submissions: Array<{ generation: number; payload: Submission }> = [];
    const events: string[] = [];
    const attempts: Array<Parameters<typeof client.recordCompletionAttempt>[1]> = [];
    const imports: Array<{ phase: Phase; expected?: string; context: OwedCandidateContext;
      result?: ImportResult; tracking?: string | null; owned?: Awaited<ReturnType<typeof git.committedTrackingOwnership>>;
      owed?: Awaited<ReturnType<typeof git.enumerateOwedCandidates>> }> = [];
    const decisions: unknown[] = [];
    let poisoned: boolean | undefined;
    let importsAtRepeat: [number, number] | undefined;
    let established: OwedCandidateContext | undefined;
    let afterDefaultMove: string | null = null;

    t.mock.method(git, "branchTip", async (...args: Parameters<typeof git.branchTip>) => {
      if (active && phase === "initial" && missingHead && !headStopped) {
        headStopped = true;
        events.push("initial head unavailable");
        if (scenario.startsWith("unreadable")) return tip(args[0], "refs/heads/m3-unreadable-initial");
        return null;
      }
      return tip(...args);
    });
    t.mock.method(git, "originBranchTip", async (...args: Parameters<typeof git.originBranchTip>) => {
      if (active && phase === "initial" && scenario === "cold context" && !contextStopped) {
        contextStopped = true;
        events.push("cold context soft stop");
        throw new CheckpointSoftDeadlineError();
      }
      return originTip(...args);
    });
    t.mock.method(git, "fetchAgentBranch", async (...args: ImportArgs) => {
      if (!active) return fetch(...args);
      const observed: (typeof imports)[number] = {
        phase, expected: args[4].expectedCandidateSha, context: args[4].context,
      };
      imports.push(observed);
      events.push(`${phase} import`);
      if (!budgeted && (phase === "initial" || phase === "fresh" && bothSoftStops)) {
        throw new CheckpointSoftDeadlineError();
      }
      const step = args[4].checkpointStep;
      const stoppedAttempt = budgeted && phase !== "finalize";
      try {
        observed.result = await fetch(args[0], args[1], args[2], args[3], {
          ...args[4],
          checkpointStep: name => {
            step?.(name);
            if (stoppedAttempt && name === "checkpoint_import_mutation" && !mutationJumps.includes(phase)) {
              mutationJumps.push(phase);
              maskedAttempt = phase === "initial" ? 0 : 1;
              clock.advance(85_000);
            }
          },
        });
      } catch (error) {
        if (stoppedAttempt) stoppedErrors.push(error);
        throw error;
      } finally { maskedAttempt = -1; }
      observed.tracking = await git.trackingTip(args[0], args[2]);
      if (observed.result.kind === "updated") {
        observed.owned = await git.committedTrackingOwnership(args[0], args[2], args[3], observed.result.candidateSha);
        observed.owed = await git.enumerateOwedCandidates(args[0], args[3]);
      }
      return observed.result;
    });
    const recordAttempt = client.recordCompletionAttempt.bind(client);
    t.mock.method(client, "recordCompletionAttempt", async (_id: string, body: Parameters<typeof client.recordCompletionAttempt>[1]) => {
      events.push("normal completion recorded");
      attempts.push(structuredClone(body));
      return contractEnabled ? recordAttempt(_id, body) : { unmet: [], attemptCount: 1 };
    });
    const statusReads: CodeCrossCheckStatus[] = [];
    t.mock.method(client, "codeCrossCheckStatus", async () => {
      events.push("checker status");
      statusReads.push(structuredClone(persisted));
      return persisted;
    });
    t.mock.method(client, "submitCodeCrossCheck", async (_id: string, generation: number, payload: Submission) => {
      events.push("checker submit");
      submissions.push({ generation, payload: structuredClone(payload) });
      const hasHead = "head_commit" in payload;
      const record: CodeCrossCheckRecord = {
        stage: "code", round: 1, candidate_generation: generation,
        head_commit: hasHead ? payload.head_commit : null,
        base_commit: hasHead ? payload.base_commit : null,
        candidate_digest: "c".repeat(64), checker_run_id: null,
        checker_harness: "codex", checker_model: "fixture", checker_effort: null,
        outcome: hasHead ? "completed" : "failed",
        reason_class: hasHead ? null : payload.reason_class,
        findings: [], interrupted_at: null, dispositions: [],
        finalized_at: new Date().toISOString(), deadline_at: new Date().toISOString(),
      };
      persisted = record;
      return record;
    });
    let rig: ReturnType<typeof codexRig> | undefined;
    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        modelTurns++;
        rig = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
          armDeadline: clock.armDeadline, now: clock.now,
          boundaryProcess: (_request, startupMs) => {
            if (maskedAttempt >= 0) maskedChildBudgets[maskedAttempt]!.push(startupMs);
            return undefined;
          },
        });
        const boundary = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = (request, action) => boundary(request, permit => {
          if (budgeted && request.sink === "done_checkpoint") {
            hardAttempts.push(request.deadlineMs); permits.push(permit);
          }
          return action(permit);
        });
        branch = ctx.branch; runId = ctx.runId; bare = git.barePathFor(fx.originPath);
        base = (await originTip(bare, "main"))!;
        commitInTree(ctx.worktreePath, "m3.txt", "original candidate\n");
        h1 = (await tip(ctx.worktreePath, branch))!;
        active = true;
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
        events.push("initial checkpoint returned");
        const captureStopped = async () => {
          // Called after checkpoint/gate returns, outside the stopped import ALS scope.
          stoppedStates.push({
            tracking: await git.trackingTip(bare, branch),
            owned: await git.committedTrackingOwnership(bare, branch, runId, h1),
            stages: execFileSync("git", ["-C", bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/"],
              { encoding: "utf8" }).trim().split("\n").filter(Boolean),
            source: await tip(ctx.worktreePath, branch),
          });
        };
        if (budgeted) await captureStopped();
        await ctx.recordCompletionAttempt!({ declared: [], head: nullCompletion ? null : h1, worktreeFingerprint: null });
        if (scenario === "changed clone") {
          commitInTree(ctx.worktreePath, "newer.txt", "distinct newer candidate\n");
          h2 = (await tip(ctx.worktreePath, branch))!;
        }
        phase = "fresh";
        if (enabled) {
          decisions.push(await ctx.codeCrossCheckGate!({ interlocked: true }));
          if (budgeted) await captureStopped();
          const before = imports.length;
          decisions.push(await ctx.codeCrossCheckGate!({ interlocked: true }));
          importsAtRepeat = [before, imports.length];
        }
        if (scenario === "cold context") {
          established = imports.find(i => i.phase === "fresh")?.context;
          commitInTree(fx.originPath, "default-moved.txt", "new default after context establishment\n");
          movedDefault = (await tip(fx.originPath, "main"))!;
          await git.fetchDefaultTip(bare, "main", "fixture-pat", fx.originPath);
          afterDefaultMove = await originTip(bare, "main");
        }
        poisoned = rig.registry.isPoisoned();
        phase = "finalize";
        return { branch };
      },
    };
    const forge = fakeGitlab();
    const claim = gitlabClaim(312310, {
      code_cross_check_required: enabled, claim_generation: 7,
      ...(contractEnabled ? { config: { completion_contract_version: 1, contract_revision: 1 } } : {}),
    });
    let pushedHead: (() => string) | undefined;
    if (contractEnabled) {
      api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
      await client.register("m3-snapshot-retry");
      api.setPublicationState(claim.run_id, [], 1);
      pushedHead = followSuccessfulPush(git, forge.pr);
    }
    const permitRequests = api.completionPermitRequests;
    await runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: (fire, ms) => {
        softArms.push(ms); return clock.arm(fire, ms);
      } },
    }).execute(claim);

    if (budgeted) {
      assert.equal(clock.now() - startedAt, 170_000);
      assert.deepEqual(softArms, [60_000, 60_000]);
      assert.deepEqual(hardAttempts, [90_000, 90_000]);
      assert.equal(permits.length, 2);
      assert.notStrictEqual(permits[0], permits[1], "fresh retry acquires a distinct permit");
      // CodexSafety arms admission and held-action timers for each absolute allowance.
      assert.deepEqual(clock.budgets.filter(b => b.sink === "done_checkpoint").map(b => b.ms),
        [90_000, 90_000, 90_000, 90_000]);
      assert.deepEqual(mutationJumps, ["initial", "fresh"]);
      assert.equal(stoppedErrors.length, 2);
      assert.ok(stoppedErrors.every(error => error instanceof CheckpointSoftDeadlineError));
      for (const budgets of maskedChildBudgets) {
        assert.ok(budgets.length > 0, "real mutation children run after each jump");
        assert.ok(budgets.every(ms => ms === 5_000), "each child inherits the remaining hard allowance without resetting it");
      }
      assert.equal(stoppedStates.length, 2);
      for (const [index, state] of stoppedStates.entries()) {
        assert.equal(state.tracking, h1);
        assert.deepEqual(state.owned, { kind: "owned", sha: h1, context: imports[0]!.context });
        assert.equal(state.source, h1);
        assert.equal(state.stages.length, index + 1, "each stopped attempt retains its own private stage");
        assert.ok(state.stages.every(stage => stage.endsWith(` ${h1}`)));
      }
      assert.ok(stoppedStates[1]!.stages.includes(stoppedStates[0]!.stages[0]!), "fresh retry retains the first attempt's exact private stage");
      assert.equal(imports.length, 3, "initial, one fresh retry, and authoritative finalize only");
      for (const imported of imports) {
        if (imported.phase !== "finalize") assert.equal(imported.expected, h1);
        assert.strictEqual(imported.context, imports[0]!.context);
      }
      assert.ok(Object.isFrozen(imports[0]!.context));
    }
    // Runner advisory catches must not turn a swallowed assertion into a green test.
    assert.equal(modelTurns, 1);
    assert.equal(poisoned, false, "genuine installer reconcile leaves source custody usable");
    assert.match(h1, /^[0-9a-f]{40}$/);
    assert.equal(attempts.length, 1, "exactly the normal completion attempt is recorded");
    assert.deepEqual(attempts[0], {
      milestonesCompleted: [], nonClosing: undefined, head: nullCompletion ? null : h1,
      worktreeFingerprint: null, claimGeneration: 7,
    });
    assert.ok(events.indexOf("initial checkpoint returned") < events.indexOf("normal completion recorded"));
    if (missingHead) assert.equal(headStopped, true);
    if (enabled) {
      assert.ok(events.indexOf("normal completion recorded") < events.indexOf("checker status"));
      assert.ok(events.indexOf("normal completion recorded") < events.indexOf("checker submit"));
      assert.equal(submissions.length, 1, "one checker submission across repeated done and finalize");
      assert.equal(submissions[0]!.generation, 7);
      assert.deepEqual(decisions, [{ action: "proceed" }, { action: "proceed" }]);
      assert.ok(importsAtRepeat);
      assert.equal(importsAtRepeat[0], importsAtRepeat[1], "later done never retries the snapshot");
      if (expectedFailure) {
        assert.deepEqual(submissions[0]!.payload, { reason_class: "snapshot_failed" }, "failed snapshot is SHA-free");
        assert.ok(statusReads.some(s => !("result" in s) && s.outcome === "failed" && s.reason_class === "snapshot_failed"),
          "publication reads the persisted failed row");
      } else {
        assert.deepEqual(submissions[0]!.payload, { head_commit: h1, base_commit: base, repo_instructions_commit: base });
      }
    } else {
      assert.deepEqual(submissions, []);
      assert.deepEqual(statusReads, []);
      assert.equal(imports.filter(i => i.phase === "fresh").length, 0, "off never starts a fresh snapshot loop");
    }

    const fresh = imports.filter(i => i.phase === "fresh");
    const finalized = imports.filter(i => i.phase === "finalize");
    assert.ok(finalized.length > 0, "finalize independently imports");
    const finalImport = finalized[0]!;
    const finalHead = scenario === "changed clone" ? h2 : h1;
    assert.equal(finalImport.result?.kind, "updated", "finalize is a positive real Git import");
    if (finalImport.result?.kind === "updated") assert.equal(finalImport.result.candidateSha, finalHead);
    assert.equal(finalImport.tracking, finalHead);
    assert.deepEqual(finalImport.owned, { kind: "owned", sha: finalHead, context: finalImport.context });
    assert.ok(finalImport.owed?.some(c => c.sha === finalHead && c.pinRef === `refs/uzi-owed/${runId}/${finalHead}`));
    assert.equal(await tip(fx.originPath, branch), finalHead, "real finalize publishes its candidate");
    assert.equal(forge.calls.filter(c => c.method === "POST").length, 1, "actual MR creation is observed outside callbacks");
    assert.ok(api.states.some(s => s.body.status === "completed"), "run reaches completed");
    if (scenario === "changed clone") {
      assert.match(h2, /^[0-9a-f]{40}$/);
      assert.notEqual(h2, h1);
      assert.equal(fresh.length, 0, "clone comparison refuses H2 before a fresh import");
      assert.equal(imports[0]?.expected, h1);
    } else if (bothSoftStops) {
      assert.equal(fresh.length, 1);
      assert.equal(fresh[0]!.expected, h1);
      assert.equal(fresh[0]!.result, undefined);
    } else if (nullCompletion) {
      assert.equal(fresh.length, 0, "null normal completion head cannot authorize a snapshot");
    } else if (enabled) {
      assert.equal(fresh.length, 1);
      assert.equal(fresh[0]!.expected, h1, "normal completion fallback binds exact original H1");
      assert.equal(fresh[0]!.result?.kind, "updated");
    }
    if (expectedFailure) {
      assert.match(forge.pr.description, /Code cross-check incomplete: snapshot failed/);
      assert.doesNotMatch(forge.pr.description, /Code cross-check \(checked|Code cross-check[^\n]*pass/i);
      assert.ok(JSON.stringify(forge.calls).includes("Code cross-check incomplete: snapshot failed"));
    }
    if (contractEnabled) {
      assert.equal(imports.filter(i => i.phase === "initial").length, 1);
      assert.equal(api.completionAttemptRequests.length, 1, "one normal server completion attempt");
      assert.equal(permitRequests.length, 1, "exactly one authoritative finalize permit request");
      assert.equal(permitRequests[0]!.runId, runId);
      assert.deepEqual(permitRequests[0]!.body, {
        contract_revision: 1, branch, head: h1, claim_generation: 7,
      });
      assert.equal(pushedHead?.(), h1, "permit candidate was really pushed");
      assert.equal(forge.pr.head, h1, "forge follows the actual push candidate");
      const completed = api.states.filter(s => s.runId === runId && s.body.status === "completed");
      assert.equal(completed.length, 1);
      assert.equal(completed[0]!.body.head, h1, "completed report binds permitted and verified H1");
      assert.equal(completed[0]!.body.branch, branch);
      assert.equal(completed[0]!.body.mr_iid, 42);
      assert.equal(completed[0]!.body.claim_generation, 7);
      assert.equal(api.completionHoldRequests.length, 0);
      assert.ok(forge.all.some(c => c.method === "GET"), "authoritative finalize reads the PR head");
      assert.match(forge.pr.description, /Code cross-check incomplete: snapshot failed/);
      assert.match(forge.pr.description, /Closes #312310/, "advisory acquisition failure does not revoke verified completion");
    } else {
      assert.deepEqual(permitRequests, [], "ordinary finalize requests no completion permit");
    }
    if (scenario === "cold context") {
      assert.equal(contextStopped, true);
      assert.notEqual(movedDefault, base);
      assert.equal(afterDefaultMove, movedDefault);
      const expected: OwedCandidateContext = {
        runId, branch, kind: "issue", barePath: bare, generation: 7,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
      };
      assert.deepEqual(established, expected);
      assert.ok(Object.isFrozen(established));
      assert.ok(Object.isFrozen(established!.defaultIdentity));
      for (const imported of imports) {
        assert.strictEqual(imported.context, established, "all subsequent imports reuse established context identity");
        assert.deepEqual(imported.context, expected);
        if (imported.owned?.kind === "owned") assert.deepEqual(imported.owned.context, expected);
        for (const candidate of imported.owed ?? []) {
          assert.equal(candidate.pinRef, `refs/uzi-owed/${runId}/${candidate.sha}`);
          assert.ok(candidate.contexts.length > 0);
          for (const producing of candidate.contexts) assert.deepEqual(producing, expected);
        }
      }
    }
    assert.ok(clock.budgets.some(b => b.sink === "done_checkpoint"));
    assert.ok(clock.budgets.filter(b => b.sink === "done_checkpoint").every(b => b.ms === 90_000));
  });
}
