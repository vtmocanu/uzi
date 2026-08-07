import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { type SDKMessage, type SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { spawnDetached } from "../src/sdk-spawn.js";
import { nullLogger } from "./helpers.js";
import { StubExecutor, PlanRejectedError, STUB_FAIL_SENTINEL, STUB_ASK_SENTINEL, type Executor } from "../src/executor.js";
import { AUTOPILOT_SENTINEL_ANSWER } from "../src/runner.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import {
  api,
  assistant,
  fakeGitlab,
  git,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  isAlive,
  planThenDoneQuery,
  planWithMilestonesThenDoneQuery,
  resultOk,
  runner,
  waitDead,
} from "./runner-harness.js";

installHarness();

describe("RunRunner — plan gate + steering end to end", () => {
  it("halts at awaiting_approval, resumes on approve, then completes with an MR", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(21);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const states = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    const statuses = states.map((s) => s.status);
    assert.ok(
      statuses.includes("awaiting_approval"),
      "run halted at the plan gate",
    );
    assert.ok(statuses.includes("completed"), "run completed after approval");
    const gate = states.find((s) => s.status === "awaiting_approval")!;
    assert.match(gate.plan_md ?? "", /# PLAN/);
    // The plan was surfaced to the run stream as a `plan` message, once.
    const planMsgs = api
      .messages(claim.run_id)
      .filter((m) => m.kind === "plan");
    assert.strictEqual(planMsgs.length, 1);
    // Completed with an MR on the agent branch (never main).
    const completed = states.find((s) => s.status === "completed")!;
    assert.strictEqual(completed.branch, "agent/issue-21");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(
      JSON.parse(calls[0]!.body ?? "{}").source_branch,
      "agent/issue-21",
    );
    // An iteration heartbeat was reported.
    assert.ok(
      states.some((s) => s.status === "running" && s.iteration_count === 1),
    );
  });

  it("stub executor with planGate halts at awaiting_approval, then completes on approve", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, [
      "running",
      "running",
      "awaiting_approval",
      "running",
      "completed",
    ]);
    const gate = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_approval",
    )!.body;
    assert.match(gate.plan_md ?? "", /Stub plan for issue #24/);
    const completed = api.states.find(
      (s) => s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-24");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after approval");
    // The plan reached the run stream exactly once.
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });

  it("auto-approves the plan gate for an autopilot claim: never awaiting_approval, plan still recorded", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(26, { auto_approve: true });
    // No inputs are set: an autopilot run must resolve the gate itself. If it
    // parked at awaiting_approval it would hang until the (disabled) timeout.
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "autopilot run runs to completion",
    );
    // The plan is still recorded as an audit message even though no human saw it.
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
    const completed = api.states.find(
      (s) => s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after auto-approval");
  });

  it("throws on the UZI_STUB_FAIL sentinel after the gate (drives the E2E failure path)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(28, {
      auto_approve: true,
      issue_description: `implement prds/x.md then ${STUB_FAIL_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "auto-approved before it fails",
    );
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "sentinel run should fail");
    assert.match(failed!.body.failure_reason ?? "", /UZI_STUB_FAIL/);
    assert.strictEqual(calls.length, 0, "no MR when the run fails");
    // The plan is recorded before the failure (the throw is AFTER the gate).
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "plan").length,
      1,
    );
  });

  // PRD #88 M5 / Decision 8. Autopilot is "no human in the loop", so a park would
  // wedge the run until its deadline with nobody able to answer. The observable
  // property is that awaiting_input never appears in the reported states at all —
  // not merely that the run finishes.
  it("auto-resolves an ask_user for an autopilot claim: never awaiting_input", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(31, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    // No inputs are set: nothing could ever answer this question.
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_input"),
      "an autopilot run must never park on a question",
    );
    assert.ok(
      !statuses.includes("awaiting_approval"),
      "and still never parks at the plan gate",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "it runs to completion unattended",
    );
    assert.strictEqual(calls.length, 1, "one MR opened");
    // The "no `question` message" half of Decision 8 deliberately lives in its OWN
    // test below rather than as a fifth assertion here — see the comment there.
  });

  /**
   * Decision 8's OTHER half, split out because as an assertion inside the test above
   * it could not fail for the reason it exists.
   *
   * `assert` throws on the first failure, so any fold that makes an autopilot run
   * park — the obvious one, disabling the short-circuit — reddens the FIRST assertion
   * up there and every later one never evaluates. A control run using that fold
   * therefore establishes only *the run does not park*, and establishes **nothing**
   * about *no message claims a human was consulted*, even though both properties are
   * "covered" by a test that goes red. That is the assert-shadowing class: a green
   * suite and a red mutation can BOTH be true of a property nothing has measured.
   *
   * Discriminating fold, and it is not one anybody would reach for by accident:
   * keep the short-circuit intact (no park, no `awaiting_input`, sentinel still
   * returned) and change only the emitted message kind from `status` to `question`
   * in `RunRunner.askUser`'s autopilot branch. Every assertion above still passes;
   * this one reddens alone.
   *
   * The property is security-relevant rather than cosmetic: an unattended run that
   * files a `question` message is claiming on the permanent feed — and, with Slack
   * linked, in the owner's DM — that a human was consulted about a decision no human
   * ever saw.
   */
  it("an autopilot run emits NO question message: nothing may claim a human was consulted", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(34, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const kinds = api.messages(claim.run_id).map((m) => m.kind);
    // Positive control FIRST, so this can never pass by the run having done nothing:
    // the stub echoes an `answer` only on a path that actually reached ctx.askUser.
    assert.ok(
      kinds.includes("answer"),
      "fixture is inert: the run never reached the clarification path at all",
    );
    assert.strictEqual(
      kinds.filter((k) => k === "question").length,
      0,
      "an autopilot run was auto-resolved with a sentinel, so no `question` message " +
        "may reach the feed — it would tell the owner, on the record and in Slack, " +
        "that they were asked something they were never asked",
    );
  });

  // The byte-exact sentinel answer (frozen: the lead is told to record the assumption
  // it made, and an unattended run's guesses have to stay auditable).
  it("hands the autopilot lead the frozen sentinel answer, verbatim", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(32, {
      auto_approve: true,
      issue_description: `clarify something first: ${STUB_ASK_SENTINEL}`,
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const answer = api.messages(claim.run_id).find((m) => m.kind === "answer");
    assert.ok(answer, "the stub echoes the answer it received");
    assert.deepStrictEqual(
      (answer!.payload as { answers?: unknown }).answers,
      [AUTOPILOT_SENTINEL_ANSWER],
      "the sentinel wording is frozen — M5's test and the implementation must agree byte for byte",
    );
  });

  // The human path through the SAME sentinel: the run parks, an answer resumes it.
  // This is M4's pre-run park, which before the REASON_NO_PLAN fix would have failed
  // the run outright rather than parking.
  it("parks a NON-autopilot run on a pre-plan question and resumes it on the answer", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(33, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
    });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    // The answer must name the open question, so it is supplied by the fake API's
    // input queue once the park has stamped an id; the runner mints it, so the test
    // reads it back off the state report rather than guessing.
    const r = runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    );
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await r.execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      statuses.includes("awaiting_input"),
      "the run must park on the pre-plan question",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "and resume to completion on the answer",
    );
    assert.strictEqual(
      api.messages(claim.run_id).filter((m) => m.kind === "question").length,
      1,
    );
  });

  // B2, WORKER half.
  //
  // ⚠️ THE RATIONALE THIS COMMENT USED TO CARRY WAS FALSIFIED, AND THE TESTS BELOW
  // WERE NEVER WRONG — ONLY INSUFFICIENT. That combination is why nothing went red.
  //
  // It said: clearing open_question_id in SetRunRunning "breaks that chain at its
  // THIRD step, so the worker never receives a resolved id in the first place", and
  // concluded nothing more was needed. Measured false once M4 landed. M4 lets the
  // lead ask BEFORE it plans, and a pre-run park reaches the plan gate with NO
  // intervening `running` report — so SetRunRunning's clear is never reached and the
  // resolved id survives to awaiting_approval (D-AG). The chain was not broken
  // upstream; it had a second entrance that did not exist when this was written.
  //
  // The durable lesson, and the reason this is spelled out rather than quietly
  // rewritten: a test comment asserting "the chain is broken upstream" has a
  // dependency on upstream that NOTHING TRACKS. No gate fires when the premise stops
  // holding, because the tests it justifies are still passing and still correct. The
  // rationale rots while the code stays right.
  //
  // Both clears are now required, and the invariant they jointly carry is stated in
  // runtime.sql: no setter may leave a resolved id behind. Pinned by
  // TestSetRunRunningClearsOpenQuestionLiveDB and
  // TestSetRunAwaitingApprovalClearsOpenQuestionLiveDB — the second being the one
  // these worker-layer tests structurally cannot express, since they cannot reach a
  // plan gate without a running report.
  //
  // Still true and still the reason there is no worker-side guard: the worker cannot
  // tell a resolved id from a live one (a claim reports status "claimed", never
  // "awaiting_input", because ClaimRun is what set it), so any such guard would be a
  // guess. The server-side clears are the only defence.
  //
  // What the worker layer CAN pin, and must, is the property the fix has to leave
  // intact — the two halves of the id lifecycle:
  it("mints a fresh question id when the claim carries none", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(34, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
    });
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const park = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_input",
    );
    assert.ok(
      park?.body.open_question_id,
      "a park must always report a question id — the api rejects one without",
    );
  });

  // The requeue-survival half: a run re-claimed while genuinely still parked re-parks
  // on the SAME id. This is what makes an answer the user submitted before the worker
  // died still valid, and it is the property every clock-based design failed.
  it("re-parks on the SAME question id the claim delivered", async () => {
    const { gitlab } = fakeGitlab();
    const liveID = "question-the-run-is-still-parked-on";
    const claim = gitlabClaim(35, {
      issue_description: `clarify first: ${STUB_ASK_SENTINEL}`,
      open_question_id: liveID,
    });
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) {
        api.setInputs(claim.run_id, [
          input(
            "answer",
            JSON.stringify({
              question_id: body.open_question_id,
              answers: ["proceed"],
            }),
          ),
          input("approve_plan"),
        ]);
      }
    });
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const park = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "awaiting_input",
    );
    assert.strictEqual(
      park?.body.open_question_id,
      liveID,
      "a resumed worker must re-park on the id the claim delivered — minting a new one silently " +
        "invalidates an answer the user already submitted against the live question",
    );
  });

  it("still halts a NON-autopilot claim at the gate (auto_approve absent)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(27); // no auto_approve
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, [
      "running",
      "running",
      "awaiting_approval",
      "running",
      "completed",
    ]);
  });

  it("stub executor with planGate fails verbatim when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(25);
    api.setInputs(claim.run_id, [input("reject_plan", "not this way")]);
    await runner(
      new StubExecutor(nullLogger(), { planGate: true }),
      gitlab,
    ).execute(claim);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.strictEqual(failed!.body.failure_reason, "not this way");
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("fails with the rejection reason (verbatim) when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(22);
    api.setInputs(claim.run_id, [
      input("reject_plan", "please rethink the approach"),
    ]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }),
      gitlab,
    ).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "run should have failed");
    assert.strictEqual(
      failed!.body.failure_reason,
      "please rethink the approach",
    );
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("propagates a PlanRejectedError as a verbatim failure through a custom executor", async () => {
    const { gitlab } = fakeGitlab();
    const rejectExec: Executor = {
      run: async (ctx) => {
        const v = await ctx.gatePlan!("PLAN");
        if (v.kind === "reject") throw new PlanRejectedError(v.reason);
        return { branch: ctx.branch };
      },
    };
    const claim = gitlabClaim(23);
    api.setInputs(claim.run_id, [input("reject_plan", "no")]);
    await runner(rejectExec, gitlab).execute(claim);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.strictEqual(failed!.body.failure_reason, "no");
  });

  it("reaps the agent tree BEFORE the PAT-bearing push (B1 ordering)", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const exec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch }),
      killAgentTree: () => events.push("kill"),
    };
    const claim = gitlabClaim(31);
    const origPush = git.pushBranch.bind(git);
    (git as unknown as { pushBranch: unknown }).pushBranch = async (
      ...args: unknown[]
    ) => {
      events.push("push");
      return (origPush as (...a: unknown[]) => Promise<void>)(...args);
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      (git as unknown as { pushBranch: unknown }).pushBranch = origPush;
    }
    assert.ok(events.includes("kill") && events.includes("push"));
    assert.ok(
      events.indexOf("kill") < events.indexOf("push"),
      "kill must precede push",
    );
  });

  it("kills a real agent-backgrounded survivor before the run completes (B1)", async () => {
    const { gitlab } = fakeGitlab();
    const survivors: number[] = [];
    // Injected spawn stands in for the SDK CLI spawn, launching a real detached
    // `sleep` in its own group — the kind of survivor a `nohup … &` leaves behind.
    const spawn = (_opts: SpawnOptions): { pid?: number } => {
      const p = spawnDetached({
        command: "sleep",
        args: ["30"],
      } as SpawnOptions);
      if (p.pid) survivors.push(p.pid);
      return p;
    };
    // Real plan→done query that also triggers the spawn hook each turn.
    const scripts: SDKMessage[][] = [
      [
        assistant([
          {
            type: "tool_use",
            id: "p",
            name: "mcp__uzi__submit_plan",
            input: { plan_md: "# P" },
          },
        ]),
        resultOk(),
      ],
      [
        assistant([
          {
            type: "tool_use",
            id: "d",
            name: "mcp__uzi__signal_done",
            input: {},
          },
        ]),
        resultOk(),
      ],
    ];
    let turn = 0;
    const queryFn: SdkQueryFn = (params) => {
      const script = scripts[Math.min(turn, scripts.length - 1)]!;
      turn++;
      return (async function* () {
        params.options.spawnClaudeCodeProcess?.({
          command: "x",
          args: [],
        } as never);
        for await (const _ of params.prompt) {
          /* drain */
        }
        for (const m of script) yield m;
      })();
    };
    const claim = gitlabClaim(32);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    const exec = new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn });
    try {
      await runner(exec, gitlab).execute(claim);
      assert.ok(survivors.length >= 1, "at least one survivor was spawned");
      for (const pid of survivors) {
        await waitDead(pid);
        assert.strictEqual(
          isAlive(pid),
          false,
          `survivor ${pid} must be dead after the run`,
        );
      }
      // The run still completed with an MR (the reap did not disturb the happy path).
      assert.ok(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "completed",
        ),
      );
    } finally {
      for (const pid of survivors)
        try {
          process.kill(-pid, "SIGKILL");
        } catch {
          /* already gone */
        }
    }
  });

  it("cancels a live run: a cancel input aborts the executor's signal and fails the run", async () => {
    const { gitlab, calls } = fakeGitlab();
    // An executor that runs until cancelled through ctx.signal.
    const waiter: Executor = {
      run: (ctx) =>
        new Promise((_resolve, reject) => {
          const onAbort = (): void => reject(new Error("run cancelled"));
          if (ctx.signal?.aborted) return onAbort();
          ctx.signal?.addEventListener("abort", onAbort, { once: true });
        }),
    };
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("cancel")]);
    await runner(waiter, gitlab).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "cancelled run reports failed");
    assert.match(failed!.body.failure_reason ?? "", /run cancelled/);
    assert.strictEqual(calls.length, 0, "no MR on cancel");
  });
});

// PRD #122 M1: the CANDIDATE milestone list rides awaiting_approval (human-gated)
// and the FROZEN list rides the autopilot running report (Decision 2). All additive-
// optional: a plan with no milestones is byte-for-byte unchanged from today.
describe("RunRunner — milestones on the plan gate (PRD #122 M1)", () => {
  const MS = [
    { id: "m1", title: "Wire the protocol" },
    { id: "m2", title: "Plumb the executor" },
  ];

  it("carries the candidate milestones on the awaiting_approval report", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(90);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")!;
    assert.ok(gate, "run halted at the plan gate");
    assert.deepStrictEqual(gate.milestones, MS);
  });

  it("BACK-COMPAT: omits the milestones key entirely when the plan carries none", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(91);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(undefined),
      }),
      gitlab,
    ).execute(claim);

    const gate = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((s) => s.status === "awaiting_approval")! as unknown as Record<
      string,
      unknown
    >;
    assert.ok(gate, "run halted at the plan gate");
    assert.ok(
      !("milestones" in gate),
      "no milestones key on the wire (not null/[]), so an old api sees today's shape",
    );
  });

  it("AUTOPILOT: the frozen milestones ride the running report and the run never parks", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(26, { auto_approve: true });
    // No inputs: an autopilot run resolves the gate itself and never awaits a human.
    await runner(
      new SdkExecutor(nullLogger(), homeDir, {
        queryFn: planWithMilestonesThenDoneQuery(MS),
      }),
      gitlab,
    ).execute(claim);

    const bodies = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    assert.ok(
      !bodies.some((s) => s.status === "awaiting_approval"),
      "autopilot run never enters awaiting_approval",
    );
    // The frozen list rides the SAME self-contained running report that carries the
    // autopilot agent_selection (Decision 2).
    const autopilotReport = bodies.find(
      (s) => s.status === "running" && s.agent_selection !== undefined,
    )!;
    assert.ok(autopilotReport, "autopilot agent-selection running report present");
    assert.deepStrictEqual(autopilotReport.milestones, MS);
  });
});
