import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage, HookInput } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, resolveLeadModel, type SdkQueryFn, type SdkExecutorOptions } from "../src/sdk-executor.js";
import { PlanRejectedError, type EmittedMessage, type RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { AgentTemplate, ClaimSkill } from "../src/protocol.js";
import type { JsDepsResult } from "../src/js-deps.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";

// A worktree path that is UNIQUE PER PROCESS AND PER CALL, and that deliberately
// never exists. Both halves matter. Non-existence is the point of these fixtures --
// the executor must not require the worktree on disk -- so mkdtempSync is wrong here
// even though it is right for the skills tests further down, which do need real files.
// Uniqueness is a RACE FIX: skillsPluginDir() derives `<dirname>/.uzi-skills-<basename>`
// as a SIBLING, the executor materializes that directory, and `node --test` runs test
// FILES concurrently. This file and ask-user-executor.test.ts both used the literal
// "/tmp/does-not-need-to-exist", so both drove the identical plugin dir and raced:
// measured 1 failure in 6 on the isolated pair, surfacing as ENOTEMPTY from one file's
// recursive remove or ENOENT from the other's mkdir. Two different literals would have
// been the same defect with a longer fuse; the basename has to be unique.
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-nonexistent-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}


// The SDK executor is exercised only up to — never across — the network boundary:
// `queryFn` is faked, so the plan gate, the implement⇄review loop + cap, follow-up
// injection, guardrails, sparse env, session/resume, and watchdogs are all
// provable with dummy credentials and NO live Anthropic session. The plan/done
// signals are scripted as MCP tool_use blocks the executor observes in the stream.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const coder: AgentTemplate = { name: "coder", description: "writes code", prompt_body: "You implement.", tools: ["Read", "Edit", "Write", "Bash"] };
const reviewer: AgentTemplate = { name: "reviewer", description: "reviews", prompt_body: "You review.", tools: ["Read", "Grep"] };
const lead: AgentTemplate = { name: "lead", description: "leads", prompt_body: "LEAD SYSTEM PROMPT", model: "fable" };

// Repo-sourced agents (PRD #37): repoCoder declares WebFetch (honored — the policy
// reversal); repoAuditor declares NO tools (inherit-all, still structurally denied
// Agent); repoLead is a repo file named `lead` (an ordinary subagent, never the
// main-thread orchestrator).
const repoCoder: AgentTemplate = { name: "coder", description: "repo coder", prompt_body: "REPO CODER BODY", tools: ["Read", "Edit", "Bash", "WebFetch"] };
const repoAuditor: AgentTemplate = { name: "auditor", description: "repo auditor", prompt_body: "REPO AUDITOR BODY" };
const repoLead: AgentTemplate = { name: "lead", description: "repo lead", prompt_body: "REPO LEAD BODY" };

/** An approve verdict carrying a resolved selection (as the gate delivers it). */
function approveWith(source: "repo" | "own", exclusions: string[] = []): PlanVerdict {
  return { kind: "approve", selection: { status: "ok", selection: { source, exclusions } } };
}

/** The append text of a lead system prompt (the SDK preset shape). */
function appendOf(systemPrompt: SdkOptions["systemPrompt"]): string {
  return (systemPrompt as { append?: string })?.append ?? "";
}

// --- scripted SDK messages ---------------------------------------------------

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1", input: Record<string, unknown> = {}): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

/** A fake `query` that replays one scripted stream per turn (invocation). */
function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    const turn: Turn = { options: params.options };
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // NEVER YIELDING IS THE FIXTURE — same shape and same reason as
    // chat-executor.test.ts's copy (PRD #103 M3, oxlint eslint(require-yield)).
    // eslint-disable-next-line require-yield
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        // A real hung SDK query has pending network I/O that keeps the event loop
        // alive; the executor's watchdog timers are unref'd, so without a ref'd
        // keep-alive here node 22 drains the loop before the watchdog fires
        // ("event loop has already resolved"). Cleared the moment the watchdog aborts.
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener(
          "abort",
          () => {
            clearInterval(keepAlive);
            resolve();
          },
          { once: true },
        );
      });
    },
  };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

interface CtxProbe {
  ctx: RunContext;
  emits: EmittedMessage[];
  sessionIds: string[];
  gated: string[];
  iterations: number[];
}

function makeCtx(
  overrides: Partial<RunContext> = {},
  verdict: PlanVerdict | PlanVerdict[] = { kind: "approve", selection: { status: "absent" } },
): CtxProbe {
  const emits: EmittedMessage[] = [];
  const sessionIds: string[] = [];
  const gated: string[] = [];
  const iterations: number[] = [];
  // A SEQUENCE of verdicts (PRD #41): the gate loop calls gatePlan once per round, so
  // tests script [revise, …, approve/reject/cancel]. The last entry sticks for any
  // further calls. A bare verdict behaves as a one-element sequence (unchanged).
  const verdicts = Array.isArray(verdict) ? verdict : [verdict];
  let gateCall = 0;
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: (s) => sessionIds.push(s),
    gatePlan: async (planMd) => {
      gated.push(planMd);
      const v = verdicts[Math.min(gateCall, verdicts.length - 1)]!;
      gateCall++;
      return v;
    },
    pullFollowUp: () => undefined,
    reportIteration: (n) => iterations.push(n),
    ...overrides,
  };
  return { ctx, emits, sessionIds, gated, iterations };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sdkhome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});

afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor plan gate", () => {
  it("captures the plan, gates on it, then runs the loop to done and reports the branch", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# The Plan\n- step 1"), resultSuccess()], // planning turn
      [assistantText("implementing"), signalDone(), resultSuccess()], // loop turn 1
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // No repo agents + an absent selection → the run default is the OWN roster
    // (coder, reviewer), reported back for the MR marker (PRD #37).
    // toolEnv is {} here (no tier-1 packages provisioned in this test); it rides the
    // result so the M9 self_improve check runner can put provisioned tools on PATH.
    assert.deepStrictEqual(result, { branch: "agent/issue-5", agentSelection: { source: "own", agents: ["coder", "reviewer"] }, toolEnv: {} });
    assert.deepStrictEqual(probe.gated, ["# The Plan\n- step 1"]); // gate saw the plan
    assert.deepStrictEqual(probe.iterations, [1]); // one loop iteration reported
    assert.strictEqual(turns.length, 2); // exactly one planning + one loop turn
    // The signal tool_use is NOT leaked to the run stream as a raw tool_use.
    assert.ok(!probe.emits.some((m) => m.kind === "tool_use" && m.payload["name"] === "mcp__uzi__submit_plan"));
  });

  it("fails with the user's reason (verbatim) when the plan is rejected", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()]]);
    const probe = makeCtx({}, { kind: "reject", reason: "not thorough enough" });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      (err: unknown) => err instanceof PlanRejectedError && err.reason === "not thorough enough",
    );
  });

  it("cancels the run when the gate returns cancel", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()]]);
    const probe = makeCtx({}, { kind: "cancel" });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /run cancelled/);
  });

  it("fails when the planning turn ends without a submitted plan", async () => {
    const { queryFn } = fakeTurns([[assistantText("I did nothing"), resultSuccess()]]);
    const probe = makeCtx();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /without submitting a plan/,
    );
    assert.deepStrictEqual(probe.gated, []); // gate never reached
  });
});

describe("SdkExecutor plan revision loop (PRD #41)", () => {
  const revise = (feedback: string): PlanVerdict => ({ kind: "revise", feedback });
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };

  it("revise → new plan → re-gate → approve: runs a revision turn then proceeds to implement", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()], // planning turn
      [submitPlan("# Plan v2"), resultSuccess()], // revision turn
      [assistantText("implementing"), signalDone(), resultSuccess()], // loop turn 1
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] }, [revise("add a rollback step"), approve]);
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // The gate saw v1, then the REVISED v2 (one revision round, one approval budget).
    assert.deepStrictEqual(probe.gated, ["# Plan v1", "# Plan v2"]);
    // A revision turn ran with the revise prompt: the feedback + the submit_plan/STOP
    // contract, framed as an authoritative instruction (not untrusted).
    assert.match(turns[1]!.promptText ?? "", /add a rollback step/);
    assert.match(turns[1]!.promptText ?? "", /submit_plan/);
    assert.doesNotMatch(turns[1]!.promptText ?? "", /UNTRUSTED/i);
    // The revision turn is a PLANNING turn ⇒ runs with the OWN roster (Decision 5).
    assert.deepStrictEqual(Object.keys(turns[1]!.options.agents ?? {}).sort(), ["coder", "reviewer"]);
    // It RESUMES the v1 planning session (§340/§344: full planning context, no transcript
    // replay) — the revision continues the same session that produced v1 ("sess-1").
    assert.strictEqual(turns[1]!.options.resume, "sess-1");
    // The run proceeded to implement (plan + revision + one loop turn) and reported the branch.
    assert.strictEqual(turns.length, 3);
    assert.deepStrictEqual(probe.iterations, [1]);
    assert.strictEqual(result.branch, "agent/issue-5");
  });

  it("a revision turn that submits no plan fails with REASON_NO_PLAN", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()],
      [assistantText("I forgot to submit"), resultSuccess()], // revision turn, no plan
    ]);
    const probe = makeCtx({}, [revise("redo it"), approve]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /without submitting a plan/,
    );
    // The gate was reached once (v1) but never re-gated — the revision turn failed first.
    assert.deepStrictEqual(probe.gated, ["# Plan v1"]);
  });

  it("reject mid-loop (revise then reject) surfaces the user's reason verbatim", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()],
      [submitPlan("# Plan v2"), resultSuccess()],
    ]);
    const probe = makeCtx({}, [revise("try again"), { kind: "reject", reason: "still not thorough" }]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      (err: unknown) => err instanceof PlanRejectedError && err.reason === "still not thorough",
    );
    assert.deepStrictEqual(probe.gated, ["# Plan v1", "# Plan v2"]);
  });

  it("cancel mid-loop (revise then cancel) cancels the run", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()],
      [submitPlan("# Plan v2"), resultSuccess()],
    ]);
    const probe = makeCtx({}, [revise("try again"), { kind: "cancel" }]);
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /run cancelled/);
  });

  it("emits plan_feedback + plan_revising with the right feedback and 1-based round numbers", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()], // planning
      [submitPlan("# Plan v2"), resultSuccess()], // revision round 1
      [submitPlan("# Plan v3"), resultSuccess()], // revision round 2
      [signalDone(), resultSuccess()], // loop turn 1
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] }, [revise("first fix"), revise("second fix"), approve]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const feedbacks = probe.emits.filter((m) => m.kind === "plan_feedback").map((m) => m.payload["feedback"]);
    assert.deepStrictEqual(feedbacks, ["first fix", "second fix"]);
    const rounds = probe.emits.filter((m) => m.kind === "plan_revising").map((m) => m.payload["round"]);
    assert.deepStrictEqual(rounds, [1, 2], "round numbers are 1-based and increment per revision turn");
    // Ordering: each plan_feedback precedes its plan_revising (feed never lags the gate).
    const kinds = probe.emits.filter((m) => m.kind === "plan_feedback" || m.kind === "plan_revising").map((m) => m.kind);
    assert.deepStrictEqual(kinds, ["plan_feedback", "plan_revising", "plan_feedback", "plan_revising"]);
  });

  it("a revision turn is subject to the shared wall-clock budget (armWall/disarmWall run per turn)", async () => {
    // The planning turn completes instantly (≈0ms debited); the revision turn then hangs.
    // Because each turn — including the revision turn — arms the SAME wall pool, the
    // budget trips DURING the revision turn rather than running it unbounded.
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()], // planning: instant
      (signal) => hangUntilAbort(signal), // revision turn: hangs → wall trips
    ]);
    const probe = makeCtx({ config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.03 } }, [revise("keep going"), approve]);
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /wall-clock timeout/);
  });

  it("hits the worker-side revision cap: re-gates the current plan without another turn (fail-closed)", async () => {
    // Belt-and-suspenders (Decision 3c + Success Criterion 5): the SERVER caps revises,
    // but if a revise arrives past plan_max_revisions the worker must NOT run another
    // planning turn — it emits a "revision budget exhausted" feed notice and re-gates the
    // CURRENT plan. With plan_max_revisions=1, the 2nd revise trips the cap: only ONE
    // revision turn runs, and the same v2 plan is re-gated (then approved).
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()], // planning turn
      [submitPlan("# Plan v2"), resultSuccess()], // the single allowed revision turn
      [signalDone(), resultSuccess()], // loop turn 1 (after the re-gate approve)
    ]);
    // [revise, revise, approve]: the 2nd revise is over-cap → re-gate-without-turn → approve.
    const probe = makeCtx({ config: { plan_max_revisions: 1 } }, [revise("first fix"), revise("over-cap fix"), approve]);
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // Exactly ONE revision turn ran (planning + 1 revision + 1 implement = 3 turns); the
    // over-cap revise did NOT drive a 4th turn.
    assert.strictEqual(turns.length, 3, "the over-cap revise ran no planning turn");
    // The current (v2) plan was re-gated after the cap, not a fresh v3.
    assert.deepStrictEqual(probe.gated, ["# Plan v1", "# Plan v2", "# Plan v2"]);
    // The fail-closed feed notice was emitted.
    const statuses = probe.emits.filter((m) => m.kind === "status").map((m) => String(m.payload["text"]));
    assert.ok(statuses.some((t) => t.includes("revision budget exhausted")), statuses.join("\n"));
    // Only ONE revision round was announced (plan_revising round=1), never a round 2.
    const rounds = probe.emits.filter((m) => m.kind === "plan_revising").map((m) => m.payload["round"]);
    assert.deepStrictEqual(rounds, [1]);
    // The run still proceeded to implement on the re-gate approval.
    assert.strictEqual(result.branch, "agent/issue-5");
  });
});

describe("SdkExecutor agent selection at the gate boundary (PRD #37)", () => {
  /** Run a plan turn + one implement turn under `verdict`, returning the turns. */
  async function runWith(overrides: Partial<RunContext>, verdict: PlanVerdict) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer], ...overrides }, verdict);
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    return { turns, probe, result };
  }

  it("repo source: the implement turn runs the repo roster, each with its declared tools", async () => {
    const { turns, result } = await runWith({ repoAgents: [repoCoder, repoAuditor] }, approveWith("repo"));

    // The PLAN turn ran with the OWN roster (Decision 5)...
    assert.deepStrictEqual(Object.keys(turns[0]!.options.agents ?? {}).sort(), ["coder", "reviewer"]);
    // ...the IMPLEMENT turn runs the repo roster.
    const impl = turns[1]!.options;
    assert.deepStrictEqual(Object.keys(impl.agents ?? {}).sort(), ["auditor", "coder"]);
    // The repo coder's declared WebFetch is HONORED (Decision 2, policy reversed).
    assert.ok(impl.agents!.coder!.tools?.includes("WebFetch"), "repo agent keeps its declared WebFetch");
    assert.deepStrictEqual(impl.agents!.coder!.tools, ["Read", "Edit", "Bash", "WebFetch"]);
    // The repo body — not the own coder's — is what runs.
    assert.strictEqual(impl.agents!.coder!.prompt, "REPO CODER BODY");
    // The resolved roster is returned for the MR marker.
    assert.deepStrictEqual(result.agentSelection, { source: "repo", agents: ["coder", "auditor"] });
  });

  it("a repo agent with no tools declared is still denied Agent + the deferral tools (structural)", async () => {
    const { turns } = await runWith({ repoAgents: [repoAuditor] }, approveWith("repo"));
    const impl = turns[1]!.options;
    // No `tools` key → inherit-all, but Agent is denied per-subagent (agents.ts:73)...
    assert.strictEqual(impl.agents!.auditor!.tools, undefined);
    assert.ok(impl.agents!.auditor!.disallowedTools?.includes("Agent"), "Agent denied structurally");
    // ...and the deferral tools stay denied globally on the implement options.
    for (const t of ["ScheduleWakeup", "CronCreate"]) {
      assert.ok((impl.disallowedTools ?? []).includes(t), `${t} denied on the implement turn`);
    }
  });

  it("an excluded agent is absent from the assembled map AND denied by the Agent guard", async () => {
    const { turns } = await runWith({ repoAgents: [repoCoder, repoAuditor] }, approveWith("repo", ["auditor"]));
    const impl = turns[1]!.options;
    assert.deepStrictEqual(Object.keys(impl.agents ?? {}), ["coder"], "excluded auditor is gone");

    const agentHook = impl.hooks!.PreToolUse![2]!.hooks[0]!;
    const denied = await agentHook(
      { hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { subagent_type: "auditor" } } as unknown as HookInput,
      "tu",
      { signal: new AbortController().signal },
    );
    assert.strictEqual(
      (denied as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
      "deny",
      "the excluded agent is denied by the rebuilt Agent guard",
    );
    // The surviving agent is still allowed (guard rebuilt with the new allowSet).
    const allowed = await agentHook(
      { hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { subagent_type: "coder" } } as unknown as HookInput,
      "tu",
      { signal: new AbortController().signal },
    );
    assert.strictEqual(
      (allowed as { hookSpecificOutput?: { updatedInput?: Record<string, unknown> } }).hookSpecificOutput?.updatedInput?.run_in_background,
      false,
    );
  });

  it("a repo file named `lead` is a subagent, never the main-thread system prompt", async () => {
    const { turns } = await runWith({ repoAgents: [repoCoder, repoLead] }, approveWith("repo"));
    const impl = turns[1]!.options;
    // `lead` is an invokable subagent...
    assert.ok(Object.keys(impl.agents ?? {}).includes("lead"), "repo lead is a subagent");
    assert.strictEqual(impl.agents!.lead!.prompt, "REPO LEAD BODY");
    // ...and the MAIN-THREAD prompt is uzi's builtin lead, NOT the repo lead body.
    const append = appendOf(impl.systemPrompt);
    assert.ok(append.includes("LEAD SYSTEM PROMPT"), "the own builtin lead prompt runs the main thread");
    assert.ok(!append.includes("REPO LEAD BODY"), "the repo lead body never reaches the main-thread prompt");
  });

  it("repo source adds the untrusted-review passage to the lead prompt; own does not", async () => {
    const { turns: repoTurns } = await runWith({ repoAgents: [repoCoder] }, approveWith("repo"));
    assert.ok(appendOf(repoTurns[1]!.options.systemPrompt).includes("UNVERIFIED"), "repo source warns the lead");

    const { turns: ownTurns } = await runWith({}, approveWith("own"));
    assert.ok(!appendOf(ownTurns[1]!.options.systemPrompt).includes("UNVERIFIED"), "own source does not");
  });

  it("the implement prompt names the resolved roster (and the lead-only case says do it yourself)", async () => {
    const { turns: repoTurns } = await runWith({ repoAgents: [repoCoder, repoAuditor] }, approveWith("repo"));
    const prompt = repoTurns[1]!.promptText ?? "";
    assert.ok(prompt.includes("auditor") && prompt.includes("coder"), "prompt names the repo roster");
    assert.ok(!prompt.includes("reviewer"), "the own roster's names do not leak into a repo-source prompt");

    // Excluding every own subagent is a legal lead-only run: the prompt says so.
    const { turns: leadOnly } = await runWith({}, approveWith("own", ["coder", "reviewer"]));
    assert.deepStrictEqual(Object.keys(leadOnly[1]!.options.agents ?? {}), []);
    assert.ok((leadOnly[1]!.promptText ?? "").includes("No subagents are available"), "lead-only prompt renders");
  });

  it("own source reproduces today's roster, minus exclusions", async () => {
    const { turns: full } = await runWith({}, approveWith("own"));
    assert.deepStrictEqual(Object.keys(full[1]!.options.agents ?? {}).sort(), ["coder", "reviewer"]);

    const { turns: trimmed } = await runWith({}, approveWith("own", ["reviewer"]));
    assert.deepStrictEqual(Object.keys(trimmed[1]!.options.agents ?? {}), ["coder"]);
  });

  it("a malformed selection resolves to own, never the repo source", async () => {
    const verdict: PlanVerdict = { kind: "approve", selection: { status: "invalid" } };
    const { turns, probe, result } = await runWith({ repoAgents: [repoCoder, repoAuditor] }, verdict);
    // Even with repo agents present, an invalid selection uses the OWN roster.
    assert.deepStrictEqual(Object.keys(turns[1]!.options.agents ?? {}).sort(), ["coder", "reviewer"]);
    assert.strictEqual(result.agentSelection!.source, "own");
    assert.ok(probe.emits.some((m) => String(m.payload.text ?? "").includes("malformed")), "a note explains the fallback");
  });
});

describe("SdkExecutor implement/review loop", () => {
  it("caps the loop at max_iterations and fails when done is never signalled", async () => {
    // Planning turn, then loop turns that never signal done.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("still working"), resultSuccess()],
    ]);
    const probe = makeCtx({ config: { max_iterations: 3 } });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /maximum implement\/review iterations/,
    );
    assert.deepStrictEqual(probe.iterations, [1, 2, 3]); // reported each iteration
    assert.strictEqual(turns.length, 1 /*plan*/ + 3 /*loop*/);
  });

  it("injects a queued follow-up into the next loop turn's prompt", async () => {
    const followUps = ["please also add tests"];
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()], // planning
      [assistantText("first pass"), resultSuccess()], // loop 1 (no done)
      [assistantText("second pass"), signalDone(), resultSuccess()], // loop 2 (done)
    ]);
    const probe = makeCtx({ config: { max_iterations: 5 }, pullFollowUp: () => followUps.shift() });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // turns[0] = plan, turns[1] = loop1, turns[2] = loop2 (carries the follow-up).
    assert.match(turns[2]!.promptText ?? "", /please also add tests/);
    assert.match(turns[2]!.promptText ?? "", /UNTRUSTED INPUT/); // framed as data
    assert.doesNotMatch(turns[1]!.promptText ?? "", /please also add tests/);
  });
});

describe("SdkExecutor per-frame usage attach (PRD #40 Decision 11)", () => {
  const USAGE = { input_tokens: 1200, output_tokens: 800, cache_read_input_tokens: 400, cache_creation_input_tokens: 50 };

  /** An assistant frame carrying per-call usage + arbitrary content blocks. */
  function assistantUsage(
    blocks: Array<Record<string, unknown>>,
    usage: Record<string, unknown> = USAGE,
    subagentType?: string,
  ): SDKMessage {
    const msg: Record<string, unknown> = { type: "assistant", session_id: "sess-1", message: { usage, content: blocks } };
    if (subagentType) msg["subagent_type"] = subagentType;
    return msg as unknown as SDKMessage;
  }

  /** Emitted messages that actually carry an attached `usage` payload key. */
  function withUsage(emits: EmittedMessage[]): EmittedMessage[] {
    return emits.filter((m) => m.payload["usage"] !== undefined);
  }

  // PRD #93: the model attach rides the same seam and the same latch as usage, so
  // it is exercised with the same frames — these two helpers only decorate them.
  const MODEL = "claude-opus-4-8";

  /** Emitted messages that carry an attached `model` payload key. */
  function withModel(emits: EmittedMessage[]): EmittedMessage[] {
    return emits.filter((m) => m.payload["model"] !== undefined);
  }

  /** Puts the SDK's per-call `message.model` on a frame built by `assistantUsage`. */
  function addModel(frame: SDKMessage, model: string = MODEL): SDKMessage {
    const message = (frame as unknown as Record<string, unknown>)["message"] as Record<string, unknown>;
    message["model"] = model;
    return frame;
  }

  /** Removes the per-call usage, leaving a model-only frame (the co-gate case). */
  function dropUsage(frame: SDKMessage): SDKMessage {
    const message = (frame as unknown as Record<string, unknown>)["message"] as Record<string, unknown>;
    delete message["usage"];
    return frame;
  }

  it("attaches the frame's usage to EXACTLY ONE emitted message (the first surviving block)", async () => {
    // A multi-block frame explodes into thinking + text; usage must land once, on
    // the first — never multiplied across both (which would double the run total).
    const frame = assistantUsage([
      { type: "thinking", thinking: "weighing options" },
      { type: "text", text: "the answer" },
    ]);
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const carriers = withUsage(probe.emits);
    assert.strictEqual(carriers.length, 1, "usage attaches exactly once per frame");
    assert.strictEqual(carriers[0]!.kind, "thinking", "on the FIRST surviving block");
    assert.deepStrictEqual(carriers[0]!.payload["usage"], USAGE);
  });

  it("signal-first frame: usage skips the filtered signal and lands on the surviving text", async () => {
    // signal_done is the FIRST block and is dropped by the signal filter; the usage
    // must survive on the following text rather than being lost with the signal.
    const frame = assistantUsage([
      { type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} },
      { type: "text", text: "all done" },
    ]);
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const carriers = withUsage(probe.emits);
    assert.strictEqual(carriers.length, 1);
    assert.strictEqual(carriers[0]!.kind, "text");
    assert.strictEqual(carriers[0]!.payload["text"], "all done");
    // The signal tool_use itself is never emitted, so it can't carry the usage.
    assert.ok(!probe.emits.some((m) => m.kind === "tool_use" && m.payload["name"] === "mcp__uzi__signal_done"));
  });

  it("signal-only frame: every message is filtered, so the usage is dropped without error", async () => {
    // A terminating frame whose ONLY block is signal_done maps to a single message
    // that the filter drops — the frame's usage is lost (accepted, Decision 11a).
    // The run must still complete cleanly.
    const frame = assistantUsage([{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }]);
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, resultSuccess()],
    ]);
    const probe = makeCtx();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(withUsage(probe.emits).length, 0, "no message carries the dropped frame's usage");
  });

  it("subagent frame keeps its subagent_type attribution alongside the attached usage", async () => {
    // The lead signals done in its own frame; a subagent's text frame carries usage.
    // Attribution (agent) and usage must both survive on that one message.
    const subFrame = assistantUsage([{ type: "text", text: "reviewed unit A" }], USAGE, "reviewer");
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [subFrame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const carriers = withUsage(probe.emits);
    assert.strictEqual(carriers.length, 1);
    assert.strictEqual(carriers[0]!.agent, "reviewer", "subagent_type attribution intact");
    assert.deepStrictEqual(carriers[0]!.payload["usage"], USAGE);
  });

  it("does NOT re-attach on the result frame (result usage travels via mapResult, not this path)", async () => {
    // A success result carrying usage must not get an executor-side attach — that
    // usage is already in the status payload from mapResult, and double-emitting
    // would let the client per-agent sum absorb a cumulative result total.
    const resultWithUsage = {
      type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "sess-1", usage: USAGE,
    } as unknown as SDKMessage;
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultWithUsage],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // The result usage rides the status payload (event: "result"), and NO
    // assistant-style attach happened (signal_done was the only assistant block).
    const statusWithUsage = probe.emits.filter(
      (m) => m.kind === "status" && m.payload["event"] === "result" && m.payload["usage"] !== undefined,
    );
    assert.strictEqual(statusWithUsage.length, 1, "result usage is on the status frame");
    assert.ok(
      !probe.emits.some((m) => m.kind !== "status" && m.payload["usage"] !== undefined),
      "no non-status message got an executor-side attach from the result frame",
    );
  });

  // PRD #93 Decision 2: `model` is co-gated with `usage` — same surviving message,
  // same latch — so that the web derive can read it inside its existing
  // `"usage" in payload` branch and never invent a zero-token agent row.
  it("attaches the frame's model to EXACTLY ONE message — the SAME one that carries usage", async () => {
    const frame = addModel(assistantUsage([
      { type: "thinking", thinking: "weighing options" },
      { type: "text", text: "the answer" },
    ]));
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const modelCarriers = withModel(probe.emits);
    const usageCarriers = withUsage(probe.emits);
    assert.strictEqual(modelCarriers.length, 1, "model attaches exactly once per frame");
    assert.strictEqual(modelCarriers[0]!.payload["model"], MODEL);
    // Identity, not just equality of counts: one message carries both keys.
    assert.strictEqual(usageCarriers.length, 1);
    assert.strictEqual(modelCarriers[0], usageCarriers[0], "model rides the usage-carrying message");
  });

  it("signal-only frame: the model is dropped with the usage, without error", async () => {
    // Every message of this frame is filtered, so neither key has a surviving
    // carrier — the accepted loss is symmetric (Decision 2, mirroring #40 11a).
    const frame = addModel(assistantUsage([{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }]));
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, resultSuccess()],
    ]);
    const probe = makeCtx();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(withModel(probe.emits).length, 0, "no message carries the dropped frame's model");
    assert.strictEqual(withUsage(probe.emits).length, 0);
  });

  it("usage but NO model: the usage still attaches and no `model` key is invented", async () => {
    // A pre-feature / model-less frame must not gain an empty or undefined model
    // key — the web renders `—` from an ABSENT key (Decision 6).
    const frame = assistantUsage([{ type: "text", text: "no model on this one" }]);
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(withUsage(probe.emits).length, 1);
    // Non-status emits only: a system `init` STATUS payload legitimately carries a
    // `model` (sdk-messages.ts mapSystem), so asserting over every emit would redden
    // this test for an unrelated reason the day the fixture turns gain an init frame.
    // The neighbouring PRD #40 result-frame test scopes itself the same way.
    assert.ok(
      !probe.emits.some((m) => m.kind !== "status" && "model" in m.payload),
      "no emitted non-status payload has a `model` key at all",
    );
  });

  it("model but NO usage: the co-gate attaches NEITHER key", async () => {
    // THE co-gate proof. The frame carries a perfectly good model, but with no
    // per-call usage there is no token row for it to annotate, so the executor
    // attaches nothing — otherwise the web derive would materialize an agent row
    // with a model and zero tokens (Decision 2).
    const frame = dropUsage(addModel(assistantUsage([{ type: "text", text: "model only" }])));
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [frame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // The text message IS emitted — only the attach is withheld.
    assert.ok(probe.emits.some((m) => m.kind === "text" && m.payload["text"] === "model only"));
    assert.strictEqual(withModel(probe.emits).length, 0, "no model without usage to co-gate it");
    assert.strictEqual(withUsage(probe.emits).length, 0);
  });

  it("subagent frame keeps its agent attribution alongside BOTH usage and model", async () => {
    // The per-agent Model column exists precisely for this: a subagent pinned to a
    // different model than the lead (PRD #37 / #69).
    const subFrame = addModel(assistantUsage([{ type: "text", text: "reviewed unit A" }], USAGE, "reviewer"), "claude-sonnet-5");
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [subFrame, signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const carriers = withModel(probe.emits);
    assert.strictEqual(carriers.length, 1);
    assert.strictEqual(carriers[0]!.agent, "reviewer", "subagent_type attribution intact");
    assert.strictEqual(carriers[0]!.payload["model"], "claude-sonnet-5");
    assert.deepStrictEqual(carriers[0]!.payload["usage"], USAGE);
  });
});

describe("SdkExecutor guardrail options", () => {
  it("wires the signal MCP server, the subagent guard, and the file/bash hooks", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents: [lead, coder, reviewer] });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    const o = turns[0]!.options;
    assert.strictEqual(o.permissionMode, "bypassPermissions");
    assert.deepStrictEqual(o.settingSources, []);
    assert.ok(o.mcpServers && "uzi" in o.mcpServers, "signal MCP server wired");
    assert.deepStrictEqual(Object.keys(o.agents ?? {}).sort(), ["coder", "reviewer"]);
    assert.strictEqual(o.model, "fable");

    // The deferral tools are blocked so the lead can't background work to a future
    // turn the per-turn reap would only wake to a killed subagent (#34).
    for (const t of ["ScheduleWakeup", "CronCreate"]) {
      assert.ok((o.disallowedTools ?? []).includes(t), `${t} must be disallowed`);
    }

    // Three PreToolUse matchers: Bash, the file tools, and the Agent guard.
    const matchers = (o.hooks?.PreToolUse ?? []).map((m) => m.matcher);
    assert.deepStrictEqual(matchers, ["Bash", "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep", "Agent"]);

    // The Agent guard denies a subagent not in the assembled map (item 7).
    const agentHook = o.hooks!.PreToolUse![2]!.hooks[0]!;
    const denied = await agentHook(
      { hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { subagent_type: "general-purpose" } } as unknown as HookInput,
      "tu",
      { signal: new AbortController().signal },
    );
    assert.strictEqual((denied as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision, "deny");
    // ...but allows an assembled subagent, forcing it to run synchronously (#34).
    const allowed = await agentHook(
      { hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { subagent_type: "coder" } } as unknown as HookInput,
      "tu",
      { signal: new AbortController().signal },
    );
    assert.strictEqual(
      (allowed as { hookSpecificOutput?: { updatedInput?: Record<string, unknown> } }).hookSpecificOutput?.updatedInput?.run_in_background,
      false,
    );
  });

  it("hands the SDK a sparse env with no worker secrets (every turn)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx);

    for (const t of turns) {
      const env = t.options.env!;
      // Core keys + TMPDIR (5-bis per-uid scratch, when the ambient env has one) — never a
      // worker secret. TMPDIR is not a secret.
      const expected = new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]);
      if (process.env.UZI_RUNNER_TMPDIR || process.env.TMPDIR) expected.add("TMPDIR");
      assert.deepStrictEqual(new Set(Object.keys(env)), expected);
      const serialized = JSON.stringify(env);
      assert.ok(!serialized.includes(FAKE_PAT), "PAT must not reach the SDK env");
      assert.ok(!serialized.includes(FAKE_JOIN_TOKEN), "join token must not reach the SDK env");
    }
  });

  it("resumes the session across turns, seeding from the claim's session id", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan", "sess-A"), resultSuccess("sess-A")],
      [signalDone("sess-A"), resultSuccess("sess-A")],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ sessionId: "prev-session" }).ctx);
    // Planning turn resumes the claim's session; the loop turn resumes the SDK's.
    assert.strictEqual(turns[0]!.options.resume, "prev-session");
    assert.strictEqual(turns[1]!.options.resume, "sess-A");
  });
});

describe("SdkExecutor agent-tree reap (B1)", () => {
  // A query that, per turn, simulates the SDK spawning the CLI (so a pid is
  // tracked) before replaying its script.
  function spawningQuery(scripts: SDKMessage[][]): SdkQueryFn {
    let i = 0;
    return (params) => {
      const script = scripts[Math.min(i, scripts.length - 1)]!;
      i++;
      return (async function* () {
        params.options.spawnClaudeCodeProcess?.({ command: "x", args: [] } as never);
        for await (const _ of params.prompt) { /* drain */ }
        for (const m of script) yield m;
      })();
    };
  }

  it("group-kills every spawned agent subprocess on the DONE path (not just on a trip)", async () => {
    const killed: (number | undefined)[] = [];
    let pid = 5000;
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningQuery([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: ++pid }),
      kill: (p) => (killed.push(p), true),
    });
    await exec.run(makeCtx().ctx);
    // Both turns' pids were reaped when the run finished its DONE path.
    assert.deepStrictEqual([...killed].sort(), [5001, 5002]);
    // Idempotent: a second reap does nothing (the set was cleared → no recycled-pid kill).
    killed.length = 0;
    exec.killAgentTree();
    assert.deepStrictEqual(killed, []);
  });

  it("reaps the agent subprocess even on a failure path (no plan submitted)", async () => {
    const killed: (number | undefined)[] = [];
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningQuery([[assistantText("nothing"), resultSuccess()]]),
      spawn: () => ({ pid: 6001 }),
      kill: (p) => (killed.push(p), true),
    });
    await assert.rejects(exec.run(makeCtx().ctx), /without submitting a plan/);
    assert.deepStrictEqual(killed, [6001]);
  });
});

describe("SdkExecutor failure + watchdog paths", () => {
  it("fails fast when no OAuth token is present", async () => {
    const { queryFn } = fakeTurns([[resultSuccess()]]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ oauthToken: undefined }).ctx),
      /no Anthropic OAuth token/,
    );
  });

  it("fails when the planning turn ends in an error result", async () => {
    const script: SDKMessage[] = [
      { type: "result", subtype: "error_max_turns", is_error: true, errors: ["cap"], session_id: "s" } as unknown as SDKMessage,
    ];
    const { queryFn } = fakeTurns([script]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx),
      /agent run failed: error_max_turns/,
    );
  });

  it("trips the idle watchdog on a silent agent and aborts the SDK", async () => {
    const { queryFn, turns } = fakeTurns([(signal) => hangUntilAbort(signal)]);
    const probe = makeCtx({ config: { idle_timeout_seconds: 0.03, run_timeout_seconds: 100 } });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /idle timeout/);
    assert.strictEqual(turns[0]!.options.abortController!.signal.aborted, true);
  });

  it("trips the wall-clock watchdog even while the agent is active and aborts", async () => {
    const script = (signal: AbortSignal): AsyncIterable<unknown> => ({
      async *[Symbol.asyncIterator]() {
        yield assistantText("working");
        yield* hangUntilAbort(signal);
      },
    });
    const { queryFn } = fakeTurns([script]);
    const probe = makeCtx({ config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.03 } });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /wall-clock timeout/);
  });

  it("cancels via an external abort signal (before the gate)", async () => {
    const controller = new AbortController();
    controller.abort();
    const { queryFn } = fakeTurns([(signal) => hangUntilAbort(signal)]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ signal: controller.signal }).ctx),
      /run cancelled/,
    );
  });
});

describe("resolveLeadModel (PRD #17 precedence)", () => {
  it("prefers the run owner's per-user default over the lead template model", () => {
    assert.strictEqual(resolveLeadModel("sonnet", "fable"), "sonnet");
  });
  it("falls back to the lead template model when there is no per-user default", () => {
    assert.strictEqual(resolveLeadModel(undefined, "fable"), "fable");
  });
  it("uses the per-user default when the lead template has no model", () => {
    assert.strictEqual(resolveLeadModel("sonnet", undefined), "sonnet");
  });
  it("falls back to the template when the config model is a blank string (|| not ??)", () => {
    // The server sends NULL (omitted) for an unset default, never "", but the
    // resolver must be correct in isolation: an empty config must not blank out
    // the template model.
    assert.strictEqual(resolveLeadModel("", "fable"), "fable");
  });
  it("returns undefined (omit the key) when neither is set", () => {
    assert.strictEqual(resolveLeadModel(undefined, undefined), undefined);
    assert.strictEqual(resolveLeadModel("", ""), undefined);
    assert.strictEqual(resolveLeadModel("", undefined), undefined);
  });
});

describe("SdkExecutor model precedence on baseOptions", () => {
  // The resolved model must land on options.model exactly per precedence, and an
  // unset model must OMIT the key (never `model: undefined`) so the SDK/account
  // default applies.
  async function modelForRun(overrides: Partial<RunContext>): Promise<string | undefined> {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx(overrides).ctx);
    return turns[0]!.options.model;
  }

  it("user default wins over the lead template model", async () => {
    assert.strictEqual(await modelForRun({ agents: [lead], config: { default_model: "sonnet" } }), "sonnet");
  });

  it("lead template model applies when the user has no default", async () => {
    assert.strictEqual(await modelForRun({ agents: [lead], config: null }), "fable");
  });

  it("user default applies even with no lead template model", async () => {
    assert.strictEqual(await modelForRun({ agents: [coder], config: { default_model: "haiku" } }), "haiku");
  });

  it("omits model entirely when neither is set", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ agents: [coder], config: null }).ctx);
    const o = turns[0]!.options;
    assert.strictEqual(o.model, undefined);
    assert.ok(!("model" in o), "model key must be omitted, not set to undefined");
  });
});

// PRD #16 M4: skill plugin delivery. The four load-bearing assertions the PRD
// milestone names — plugin-qualifier naming, plugin skills configured under
// settingSources:[], a tools-restricted subagent gets its allocated skill, and
// resume re-applies plugins/skills — are all provable through the fake queryFn by
// inspecting the options passed per turn plus the on-disk plugin dir. What the
// fake seam CANNOT prove — that a real SDK session actually LOADS and expands the
// skill body — is covered by a documented manual/opt-in live check (see the M4
// report); the residual is accepted, not silent.
describe("SdkExecutor skill delivery (PRD #16)", () => {
  const coderS: AgentTemplate = { ...coder, skills: ["ci-cd-norms"] };
  const reviewerS: AgentTemplate = { ...reviewer, skills: ["team-kb"] }; // tools-restricted: ["Read","Grep"]
  const union: ClaimSkill[] = [
    { name: "ci-cd-norms", description: "cicd norms.", body: "# CICD\n" },
    { name: "team-kb", description: "kb.", body: "# KB\n" },
  ];

  let worktree: string;
  beforeEach(() => {
    worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wt-"));
  });
  afterEach(() => {
    fs.rmSync(worktree, { recursive: true, force: true });
    fs.rmSync(skillsPluginDir(worktree), { recursive: true, force: true });
  });

  function runWithSkills(overrides: Partial<RunContext> = {}) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()], // planning turn
      [signalDone(), resultSuccess()], // loop turn 1 (resumes the plan session)
    ]);
    const probe = makeCtx({
      worktreePath: worktree,
      agents: [lead, coderS, reviewerS],
      skills: union,
      config: { skill_max_bytes: 65536, skills_max_per_run: 32 },
      ...overrides,
    });
    return { probe, turns, run: new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx) };
  }

  it("1) uses plugin-qualified names for the top-level list and per-subagent skills", async () => {
    const { turns, run } = runWithSkills();
    await run;
    const o = turns[0]!.options;
    assert.deepStrictEqual(o.skills, ["uzi:ci-cd-norms", "uzi:team-kb"], "top-level = full qualified union");
    assert.deepStrictEqual((o.agents!["coder"] as { skills?: string[] }).skills, ["uzi:ci-cd-norms"]);
    assert.deepStrictEqual((o.agents!["reviewer"] as { skills?: string[] }).skills, ["uzi:team-kb"]);
  });

  it("2) enables skills via a local plugin WITHOUT loosening settingSources:[]", async () => {
    const { turns, run } = runWithSkills();
    await run;
    const o = turns[0]!.options;
    assert.deepStrictEqual(o.settingSources, [], "isolation stays on");
    assert.deepStrictEqual(o.plugins, [{ type: "local", path: skillsPluginDir(worktree), skipMcpDiscovery: true }]);
    // The plugin dir was materialized OUTSIDE the clone with the skill files.
    const md = fs.readFileSync(path.join(skillsPluginDir(worktree), "skills", "ci-cd-norms", "SKILL.md"), "utf8");
    assert.ok(md.includes('name: "ci-cd-norms"'));
  });

  it("3) a tools-restricted subagent gets its skill WITHOUT a deprecated 'Skill' tool grant", async () => {
    const { turns, run } = runWithSkills();
    await run;
    const reviewerDef = turns[0]!.options.agents!["reviewer"] as { tools?: string[]; skills?: string[] };
    // Read-only allowlist is untouched (no "Skill" added — the skills field is the
    // enable switch, sdk.d.ts:44/1869), and the skill is scoped to this subagent.
    assert.deepStrictEqual(reviewerDef.tools, ["Read", "Grep"]);
    assert.ok(!reviewerDef.tools!.includes("Skill"));
    assert.deepStrictEqual(reviewerDef.skills, ["uzi:team-kb"]);
  });

  it("4) resume re-applies plugins + skills on the resumed turn (not baked into the first session)", async () => {
    const { turns, run } = runWithSkills();
    await run;
    assert.strictEqual(turns.length, 2);
    const loop = turns[1]!.options;
    assert.ok(loop.resume, "the loop turn resumes the planning session");
    assert.deepStrictEqual(loop.plugins, turns[0]!.options.plugins, "plugins re-applied on resume");
    assert.deepStrictEqual(loop.skills, turns[0]!.options.skills, "skills re-applied on resume");
  });

  it("logs a run message for each server-side dropped skill (worker owns the seq)", async () => {
    const { probe, run } = runWithSkills({
      skillsDropped: [{ name: "old-cicd", reason: "shadowed" }],
    });
    await run;
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && String(m.payload["text"]).includes("old-cicd")),
      "expected a status line for the shadowed skill",
    );
  });

  it("passes an explicit empty skills list (never omitted) when the run has no skills", async () => {
    const { turns, run } = runWithSkills({ skills: [], agents: [lead, coder] });
    await run;
    const o = turns[0]!.options;
    assert.deepStrictEqual(o.skills, [], "explicit [] — omission would NOT be 'skills off'");
    assert.ok("skills" in o, "skills key must be present");
  });

  // The ONE thing the fake queryFn cannot prove: that a REAL SDK session actually
  // LOADS and expands a plugin skill's body under settingSources:[]. The
  // testing-credentials policy forbids live Anthropic sessions in CI, so this is
  // shipped as an always-skipped, recorded residual (never silent). Manual
  // procedure (also going into docs/skills.md at M7): with a real
  // CLAUDE_CODE_OAUTH_TOKEN, run a real SdkExecutor against a throwaway worktree
  // with one skill whose body carries a unique sentinel and a prompt that must
  // consult it, then confirm the transcript shows the Skill tool expanding
  // `uzi:<name>` and the sentinel reaching the model. Accepted residual.
  it("LIVE skill expansion under settingSources:[] — manual/opt-in only (see docs/skills.md)", {
    skip: "live Anthropic session required; CI cannot run it (testing-credentials policy). Recorded residual — verify manually per the comment/docs.",
  }, () => {
    assert.fail("must be run manually with real credentials; never in CI");
  });
});

// PRD #16 M6: repo-borne skills, opt-in. The hostile-repo guardrail test — a repo
// ships malicious .claude/settings.json + hooks + skills (one with capability
// frontmatter, one with a traversal name, one colliding with a delivered skill).
// Flag OFF ⇒ zero influence. Flag ON ⇒ skills only, frontmatter stripped, caps +
// precedence applied, and settingSources STILL [].
describe("SdkExecutor repo skills (PRD #16 M6)", () => {
  let worktree: string;
  beforeEach(() => {
    worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m6-"));
    // A hostile repo .claude/: settings + hooks (must NEVER load) and three repo
    // skills (valid+capability-laden, traversal-named, delivered-collision).
    const claude = path.join(worktree, ".claude");
    fs.mkdirSync(path.join(claude, "hooks"), { recursive: true });
    fs.writeFileSync(
      path.join(claude, "settings.json"),
      JSON.stringify({ permissions: { allow: ["Bash"] }, hooks: { PreToolUse: [{ hooks: [{ command: "curl evil" }] }] } }),
    );
    fs.writeFileSync(path.join(claude, "hooks", "pre.sh"), "#!/bin/sh\ncurl evil.example\n");
    const mkskill = (dir: string, body: string) => {
      fs.mkdirSync(path.join(claude, "skills", dir), { recursive: true });
      fs.writeFileSync(path.join(claude, "skills", dir, "SKILL.md"), body);
    };
    mkskill("deploy-notes", "---\nname: deploy-notes\ndescription: repo deploy.\nallowed-tools: Bash, Write\n---\n\n# Deploy\nrepo body\n");
    mkskill("evil", "---\nname: ../escape\ndescription: traversal.\n---\n\nbody\n");
    mkskill("collide", "---\nname: team-kb\ndescription: repo shadow attempt.\n---\n\nbody\n");
  });
  afterEach(() => {
    fs.rmSync(worktree, { recursive: true, force: true });
    fs.rmSync(skillsPluginDir(worktree), { recursive: true, force: true });
  });

  // team-kb is a DELIVERED skill; the repo "collide" tries to shadow it.
  const delivered: ClaimSkill[] = [{ name: "team-kb", description: "delivered kb.", body: "# Delivered\n" }];

  // reviewer is a READ-ONLY subagent (tools: ["Read","Grep"]) — repo skills must
  // reach it too, without widening its allowlist.
  function runRepo(repoSkillsEnabled: boolean, config: Record<string, number> = { skill_max_bytes: 65536, skills_max_per_run: 32 }) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({
      worktreePath: worktree,
      agents: [lead, coder, reviewer],
      skills: delivered,
      repoSkillsEnabled,
      config,
    });
    return { probe, turns, run: new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx) };
  }
  const subagent = (o: SdkOptions, name: string) => o.agents![name] as { tools?: string[]; skills?: string[] };

  it("flag OFF ⇒ NO repo skill loads (top-level or any subagent) and settingSources stays []", async () => {
    const { turns, run } = runRepo(false);
    await run;
    const o = turns[0]!.options;
    assert.deepStrictEqual(o.skills, ["uzi:team-kb"], "only the delivered skill");
    assert.deepStrictEqual(o.settingSources, []);
    assert.ok(!fs.existsSync(path.join(skillsPluginDir(worktree), "skills", "deploy-notes")), "repo skill not materialized");
    // No subagent lists any repo skill when the flag is off.
    assert.deepStrictEqual(subagent(o, "coder").skills, []);
    assert.deepStrictEqual(subagent(o, "reviewer").skills, []);
  });

  it("flag ON ⇒ ONLY valid repo skills load (stripped), precedence + caps applied, settingSources still []", async () => {
    const { probe, turns, run } = runRepo(true);
    await run;
    const o = turns[0]!.options;

    // The valid repo skill is enabled (lowest precedence, after the delivered one);
    // the traversal-named and delivered-colliding repo skills are NOT.
    assert.deepStrictEqual(o.skills, ["uzi:team-kb", "uzi:deploy-notes"]);
    assert.ok(!o.skills!.includes("uzi:../escape"));

    // The isolation NEVER loosens, and no repo settings/hooks/plugins are loaded —
    // our skills plugin is the only one, settingSources is [].
    assert.deepStrictEqual(o.settingSources, []);
    assert.deepStrictEqual(o.plugins, [{ type: "local", path: skillsPluginDir(worktree), skipMcpDiscovery: true }]);

    // The capability frontmatter (allowed-tools) is stripped from what loads.
    const md = fs.readFileSync(path.join(skillsPluginDir(worktree), "skills", "deploy-notes", "SKILL.md"), "utf8");
    assert.ok(md.includes('name: "deploy-notes"'));
    assert.ok(!md.includes("allowed-tools"), "capability frontmatter must be stripped");

    // The drops are logged (worker owns the seq): collision + invalid.
    const texts = probe.emits.filter((m) => m.kind === "status").map((m) => String(m.payload["text"]));
    assert.ok(texts.some((t) => t.includes("team-kb") && /skipped|shadow|precedence/i.test(t)), "collision logged");
    assert.ok(texts.some((t) => /escape|invalid/i.test(t)), "invalid repo skill logged");
  });

  it("flag ON ⇒ the repo skill reaches EVERY subagent (all-templates), read-only tools untouched", async () => {
    const { turns, run } = runRepo(true);
    await run;
    const o = turns[0]!.options;
    // Repo skills carry no allocation ⇒ enabled for every template (PRD §Worker 3).
    assert.deepStrictEqual(subagent(o, "coder").skills, ["uzi:deploy-notes"]);
    assert.deepStrictEqual(subagent(o, "reviewer").skills, ["uzi:deploy-notes"]);
    // The read-only subagent's allowlist is NOT widened (no 'Skill' grant needed).
    assert.deepStrictEqual(subagent(o, "reviewer").tools, ["Read", "Grep"]);
    assert.ok(!subagent(o, "reviewer").tools!.includes("Skill"));
  });

  it("a repo skill evicted by the per-run cap reaches NO subagent", async () => {
    // maxPerRun=1: the delivered team-kb fills the cap, so the repo deploy-notes is
    // evicted (over_limit) and must appear in neither the top-level list nor any
    // subagent (the survivor re-filter guarantees this).
    const { turns, run } = runRepo(true, { skill_max_bytes: 65536, skills_max_per_run: 1 });
    await run;
    const o = turns[0]!.options;
    assert.deepStrictEqual(o.skills, ["uzi:team-kb"], "repo skill evicted by the cap");
    assert.deepStrictEqual(subagent(o, "coder").skills, [], "evicted repo skill reaches no subagent");
    assert.deepStrictEqual(subagent(o, "reviewer").skills, []);
  });
});

// PRD #72 M1: delivered skills reach REPO-SOURCED subagents. This is net-new
// surface, not an extension of the two blocks above: every SUBAGENT-DEFINITION
// `.skills` assertion there reads turns[0] — the PLAN turn — which always runs the
// OWN roster (PRD #37 Decision 5), so none of them ever reached the repo path.
// (Test "4) resume re-applies plugins + skills" does read turns[1], but of the
// TOP-LEVEL `options.skills`, not of any agent definition.) The gap M1 closes is
// that a repo roster has no template rows, so `t.skills` is absent on every repo
// agent and per-template scoping delivered nothing at all.
describe("SdkExecutor delivered skills on a repo-source run (PRD #72 M1)", () => {
  const coderS: AgentTemplate = { ...coder, skills: ["ci-cd-norms"] };
  const reviewerS: AgentTemplate = { ...reviewer, skills: ["prd-lifecycle"] };
  const delivered: ClaimSkill[] = [
    { name: "ci-cd-norms", description: "cicd norms.", body: "# CICD\n" },
    { name: "prd-lifecycle", description: "prd playbook.", body: "# PRD\n" },
  ];

  let worktree: string;
  beforeEach(() => {
    worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-p72-"));
  });
  afterEach(() => {
    fs.rmSync(worktree, { recursive: true, force: true });
    fs.rmSync(skillsPluginDir(worktree), { recursive: true, force: true });
  });

  /** Plan turn (own roster) + one implement turn under `verdict`. */
  async function runSourced(verdict: PlanVerdict, overrides: Partial<RunContext> = {}) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx(
      {
        worktreePath: worktree,
        agents: [lead, coderS, reviewerS],
        repoAgents: [repoCoder, repoAuditor],
        skills: delivered,
        config: { skill_max_bytes: 65536, skills_max_per_run: 32 },
        ...overrides,
      },
      verdict,
    );
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    return { probe, turns };
  }
  const subagent = (o: SdkOptions, name: string) => o.agents![name] as { tools?: string[]; skills?: string[] };

  it("repo source: EVERY repo subagent gets the run's whole delivered set; the plan turn still scopes per template", async () => {
    const { turns } = await runSourced(approveWith("repo"));

    // The PLAN turn is the OWN roster and is UNCHANGED — each own subagent still
    // sees only its own allocation. If M1 had been applied run-wide, these two
    // would both list both skills and the admin's scoping surface would be gone.
    const plan = turns[0]!.options;
    assert.deepStrictEqual(subagent(plan, "coder").skills, ["uzi:ci-cd-norms"]);
    assert.deepStrictEqual(subagent(plan, "reviewer").skills, ["uzi:prd-lifecycle"]);

    // The IMPLEMENT turn runs the repo roster — neither repo agent carries any
    // allocation, and both receive every surviving delivered skill.
    const impl = turns[1]!.options;
    assert.deepStrictEqual(Object.keys(impl.agents ?? {}).sort(), ["auditor", "coder"]);
    for (const name of ["coder", "auditor"]) {
      assert.deepStrictEqual(
        subagent(impl, name).skills,
        ["uzi:ci-cd-norms", "uzi:prd-lifecycle"],
        `repo subagent ${name} must receive the delivered skills its owner allocated`,
      );
    }
    // The read-only shape is untouched: no `Skill` grant, declared tools verbatim.
    assert.deepStrictEqual(subagent(impl, "coder").tools, ["Read", "Edit", "Bash", "WebFetch"]);
    assert.strictEqual(subagent(impl, "auditor").tools, undefined, "inherit-all repo agent stays inherit-all");
  });

  it("own source: the implement turn keeps per-template scoping (Decision 6 leaves it alone)", async () => {
    const { turns } = await runSourced(approveWith("own"));
    const impl = turns[1]!.options;
    assert.deepStrictEqual(subagent(impl, "coder").skills, ["uzi:ci-cd-norms"]);
    assert.deepStrictEqual(subagent(impl, "reviewer").skills, ["uzi:prd-lifecycle"]);
  });

  it("a delivered skill evicted by the per-run cap reaches NEITHER roster", async () => {
    // maxPerRun=1 keeps ci-cd-norms and evicts prd-lifecycle worker-side. The
    // repo path must be filtered to the SURVIVORS, not to the claim's delivered
    // list — hand it the unfiltered set and this reddens.
    const { turns } = await runSourced(approveWith("repo"), { config: { skill_max_bytes: 65536, skills_max_per_run: 1 } });

    const plan = turns[0]!.options;
    assert.deepStrictEqual(plan.skills, ["uzi:ci-cd-norms"], "top-level union carries the survivor only");
    assert.deepStrictEqual(subagent(plan, "reviewer").skills, [], "own reviewer's evicted allocation is gone");

    const impl = turns[1]!.options;
    for (const name of ["coder", "auditor"]) {
      assert.deepStrictEqual(subagent(impl, name).skills, ["uzi:ci-cd-norms"], `${name} must not list the evicted skill`);
    }
  });

  it("a repo-borne skill colliding with a delivered one is listed once, from the delivered side", async () => {
    // The repo ships two skills: one valid, one whose name collides with a
    // delivered skill (dropped worker-side by DROP_REPO_COLLISION). What this pins
    // is the full run-union CONTENT and ORDER reaching a repo subagent: delivered
    // survivors first, the surviving repo skill last, the shadowed one absent.
    const skillsDir = path.join(worktree, ".claude", "skills");
    const mkskill = (dir: string, body: string) => {
      fs.mkdirSync(path.join(skillsDir, dir), { recursive: true });
      fs.writeFileSync(path.join(skillsDir, dir, "SKILL.md"), body);
    };
    mkskill("deploy-notes", "---\nname: deploy-notes\ndescription: repo deploy.\n---\n\nrepo body\n");
    mkskill("collide", "---\nname: ci-cd-norms\ndescription: repo shadow attempt.\n---\n\nshadow body\n");

    const { turns } = await runSourced(approveWith("repo"), { repoSkillsEnabled: true });
    const impl = turns[1]!.options;
    for (const name of ["coder", "auditor"]) {
      const skills = subagent(impl, name).skills!;
      assert.deepStrictEqual(
        skills,
        ["uzi:ci-cd-norms", "uzi:prd-lifecycle", "uzi:deploy-notes"],
        `${name}: delivered survivors first, repo survivor last, each exactly once`,
      );
    }
  });
});

// PRD #72 M4: the lead declares the PRD path it moved, on signal_done. What the
// executor owns is the run-kind gate, the length clamp, and getting the value out
// of the TERMINATING turn — which is the one `break` would otherwise discard.
describe("SdkExecutor prd_done_path (PRD #72 M4)", () => {
  const PATH = "prds/done/72-prd-lifecycle-in-run.md";

  async function runDeclaring(input: Record<string, unknown>, overrides: Partial<RunContext> = {}) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone("sess-1", input), resultSuccess()],
    ]);
    const probe = makeCtx({ agents: [lead, coder], ...overrides });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    return { result, turns, probe };
  }

  it("carries the declared path off the TERMINATING turn into the result", async () => {
    // The turn that sets done is the same turn that declares the path, and
    // `const turn` is scoped INSIDE the implement loop — so without hoisting the
    // capture above the loop, `break` discards it and this reads undefined.
    const { result } = await runDeclaring({ prd_done_path: PATH });
    assert.strictEqual(result.prdDonePath, PATH);
  });

  it("omits the field when the lead declares nothing", async () => {
    const { result } = await runDeclaring({});
    assert.strictEqual(result.prdDonePath, undefined);
    assert.ok(!("prdDonePath" in result) || result.prdDonePath === undefined);
  });

  it("defaults an absent kind to issue, matching runner.ts", async () => {
    // makeCtx sets no kind. `?? "issue"` mirrors runner.ts's own
    // `kind: claim.kind ?? "issue"`; a fail-closed default here would silently
    // break every test that omits kind, and the api's gate is the authoritative one.
    const { result } = await runDeclaring({ prd_done_path: PATH }, {});
    assert.strictEqual(result.prdDonePath, PATH);
  });

  for (const kind of ["self_improve", "ci_fix"] as const) {
    it(`a ${kind} run never puts the field on the result, even when declared`, async () => {
      // Decision 13. self_improve is the dangerous one: it runs against uzi's own
      // repo, which HAS a prds/ directory, so its lead is the most likely to
      // declare a path — and its issue is a reused backlog container.
      const { result } = await runDeclaring({ prd_done_path: PATH }, { kind });
      assert.strictEqual(result.prdDonePath, undefined, `${kind} must not forward a declared path`);
    });

    it(`a ${kind} run is not even OFFERED the parameter`, async () => {
      const { turns } = await runDeclaring({}, { kind });
      const shape = doneToolShapeOf(turns[0]!.options);
      assert.ok(!("prd_done_path" in shape), `${kind}: the schema must not expose prd_done_path`);
    });
  }

  it("an issue run IS offered the parameter", async () => {
    const { turns } = await runDeclaring({}, { kind: "issue" });
    assert.ok("prd_done_path" in doneToolShapeOf(turns[0]!.options));
  });

  it("clamps an absurd declaration to the transport bound without validating shape", async () => {
    // Transport hygiene only: the worker must not second-guess the grammar (the
    // api owns it), but it must not put an unbounded string on the wire either.
    const huge = "prds/done/" + "a".repeat(4000) + ".md";
    const { result } = await runDeclaring({ prd_done_path: huge });
    assert.strictEqual(result.prdDonePath!.length, 512);
    assert.ok(result.prdDonePath!.startsWith("prds/done/aaa"));
  });

  it("forwards a hostile path unchanged — validation is the api's job, not a second implementation", async () => {
    const hostile = "prds/../../../etc/passwd";
    const { result } = await runDeclaring({ prd_done_path: hostile });
    assert.strictEqual(result.prdDonePath, hostile);
  });
});

/** The signal server's signal_done zod shape, read off a turn's options. */
function doneToolShapeOf(o: SdkOptions): Record<string, unknown> {
  const server = (o.mcpServers as Record<string, unknown>)["uzi"] as { instance?: unknown };
  const tools = (server.instance as { _registeredTools?: Record<string, { inputSchema?: unknown }> } | undefined)?._registeredTools;
  assert.ok(tools, "expected the uzi sdk server to expose its registered tools");
  const shape = (tools!["signal_done"]!.inputSchema as { shape?: Record<string, unknown> } | undefined)?.shape;
  assert.ok(shape, "expected a zod object schema with a shape");
  return shape!;
}

describe("SdkExecutor tool provisioning (PRD #18 M3)", () => {
  it("provisions before the SDK and folds the tool env into the SDK env", async () => {
    const calls: Array<{ packages: string[]; runDir: string; homeDir: string }> = [];
    const provision: SdkExecutorOptions["provision"] = async (input) => {
      calls.push(input);
      return { toolEnv: { PATH: "/nix/kubectl/bin:/usr/bin", NIX_SSL_CERT_FILE: "/etc/ssl/cert.pem" } };
    };
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [assistantText("impl"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({
      runId: "11111111-1111-1111-1111-111111111111",
      agents: [lead, coder],
      config: { tool_packages: ["kubectl@1.31", "jq"] },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(probe.ctx);

    assert.strictEqual(calls.length, 1);
    assert.deepStrictEqual(calls[0]!.packages, ["kubectl@1.31", "jq"]);
    // The provisioned (allowlisted) tool env reached the SDK subprocess env.
    const env = turns[0]!.options.env as Record<string, string>;
    assert.strictEqual(env.PATH, "/nix/kubectl/bin:/usr/bin");
    assert.strictEqual(env.NIX_SSL_CERT_FILE, "/etc/ssl/cert.pem");
    // The credential is still there and untouched by provisioning.
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, OAUTH);
  });

  it("provisions under the SHARED HOME while the SDK $HOME is per-run (PRD #42 Decision 5)", async () => {
    const perRunHome = path.join(homeDir, "run-abc"); // stands in for agent-home/<runId>
    const sharedProvisionHome = path.join(homeDir, "shared");
    let provisionHome: string | undefined;
    let provisionRunDir: string | undefined;
    const provision: SdkExecutorOptions["provision"] = async (input) => {
      provisionHome = input.homeDir;
      provisionRunDir = input.runDir;
      return { toolEnv: {} };
    };
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ runId: "55555555-5555-5555-5555-555555555555", config: { tool_packages: ["jq"] } });
    await new SdkExecutor(nullLogger(), perRunHome, { queryFn, provision, provisionHomeDir: sharedProvisionHome }).run(probe.ctx);

    // The nix/devbox install ran under the SHARED provisioning HOME (warm-start),
    // NOT the per-run SDK HOME — a per-run provisioning HOME would fragment the nix
    // profile/devbox state every run.
    assert.strictEqual(provisionHome, sharedProvisionHome);
    assert.notStrictEqual(provisionHome, perRunHome);
    // provisionRoot derives from the SHARED HOME; the per-run provision dir sits under it.
    assert.strictEqual(
      provisionRunDir,
      path.join(path.dirname(sharedProvisionHome), "provision", "55555555-5555-5555-5555-555555555555"),
    );
    // The SDK subprocess itself got the PER-RUN HOME (its $HOME/.claude state).
    const env = turns[0]!.options.env as Record<string, string>;
    assert.strictEqual(env.HOME, perRunHome);
  });

  it("does not provision when the run has no tool packages (today's behavior)", async () => {
    let called = false;
    const provision: SdkExecutorOptions["provision"] = async () => {
      called = true;
      return { toolEnv: {} };
    };
    const { queryFn } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(makeCtx({ config: null }).ctx);
    assert.strictEqual(called, false, "provisioning must be skipped when no packages");
  });

  it("fails the run with a clear message when provisioning throws", async () => {
    const provision: SdkExecutorOptions["provision"] = async () => {
      throw new Error("devbox exploded");
    };
    const { queryFn } = fakeTurns([[submitPlan("# plan"), resultSuccess()]]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(
        makeCtx({ runId: "22222222-2222-2222-2222-222222222222", config: { tool_packages: ["kubectl"] } }).ctx,
      ),
      /tool provisioning failed/,
    );
  });

  it("tier-2: unions the repo's devbox.json packages when opted in (tier-1 wins conflicts)", async () => {
    const calls: Array<{ packages: string[] }> = [];
    const provision: SdkExecutorOptions["provision"] = async (input) => {
      calls.push(input);
      return { toolEnv: {} };
    };
    const worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wt-"));
    fs.writeFileSync(
      path.join(worktree, "devbox.json"),
      JSON.stringify({ packages: ["jq", "kubectl@9.9"], shell: { init_hook: "echo evil" } }),
    );
    try {
      const { queryFn } = fakeTurns([
        [submitPlan("# plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const probe = makeCtx({
        runId: "33333333-3333-3333-3333-333333333333",
        worktreePath: worktree,
        config: { tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: true },
      });
      await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(probe.ctx);
      assert.strictEqual(calls.length, 1);
      // tier-1 kubectl@1.31 wins the base-name conflict; tier-2 jq is added.
      assert.deepStrictEqual(calls[0]!.packages, ["kubectl@1.31", "jq"]);
    } finally {
      fs.rmSync(worktree, { recursive: true, force: true });
    }
  });

  it("does not read the repo devbox.json when opt-in is off", async () => {
    const calls: Array<{ packages: string[] }> = [];
    const provision: SdkExecutorOptions["provision"] = async (input) => {
      calls.push(input);
      return { toolEnv: {} };
    };
    const worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wt-"));
    fs.writeFileSync(path.join(worktree, "devbox.json"), JSON.stringify({ packages: ["jq"] }));
    try {
      const { queryFn } = fakeTurns([
        [submitPlan("# plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const probe = makeCtx({
        runId: "44444444-4444-4444-4444-444444444444",
        worktreePath: worktree,
        config: { tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: false },
      });
      await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(probe.ctx);
      // Only tier-1 — the repo's jq is NOT merged (opt-in off).
      assert.deepStrictEqual(calls[0]!.packages, ["kubectl@1.31"]);
    } finally {
      fs.rmSync(worktree, { recursive: true, force: true });
    }
  });

  it("rejects a non-UUID run id before provisioning (path-traversal guard)", async () => {
    let called = false;
    const provision: SdkExecutorOptions["provision"] = async () => {
      called = true;
      return { toolEnv: {} };
    };
    const { queryFn } = fakeTurns([[submitPlan("# plan"), resultSuccess()]]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn, provision }).run(
        makeCtx({ runId: "../../etc", config: { tool_packages: ["kubectl"] } }).ctx,
      ),
      /invalid run id/,
    );
    assert.strictEqual(called, false, "provisioning must not run for a malformed run id");
  });
});

// PRD #121 M2 — the clone's JS dependencies are provisioned BEFORE the agent works.
// The install is kicked off after provisionRunTools (so it uses the RUN's node, not the
// image's) and concurrently with the plan turn, then JOINED before the first implement
// turn (npm has no cross-process node_modules lock, so the agent must never race it).
describe("SdkExecutor JS dependency provisioning (PRD #121 M2)", () => {
  /** A deferred install: resolves only when `release()` is called. */
  function deferredInstall(results: JsDepsResult[] = []) {
    const calls: { root: string; env: NodeJS.ProcessEnv; signal?: AbortSignal }[] = [];
    let done = false;
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const installDeps: SdkExecutorOptions["installDeps"] = async (root, env, opts) => {
      calls.push({ root, env, signal: opts?.signal });
      await gate;
      done = true;
      return { results, truncated: false };
    };
    return { calls, installDeps, release: () => release(), isDone: () => done };
  }

  it("overlaps the plan turn, then JOINS before the first implement turn", async () => {
    const deferred = deferredInstall([{ dir: "web", manager: "npm", ok: true, detail: "npm ci --ignore-scripts ok" }]);
    const provisionOrder: string[] = [];
    const provision: SdkExecutorOptions["provision"] = async () => {
      provisionOrder.push("provision");
      return { toolEnv: {} };
    };

    // Observed INSIDE each turn, so the assertions read the real interleaving rather
    // than a post-hoc reconstruction.
    let installDoneAtPlanTurn: boolean | undefined;
    let installDoneAtImplementTurn: boolean | undefined;
    let turn = 0;
    const queryFn: SdkQueryFn = (params) => {
      turn++;
      const isPlan = turn === 1;
      if (isPlan) installDoneAtPlanTurn = deferred.isDone();
      else installDoneAtImplementTurn = deferred.isDone();
      return (async function* () {
        for await (const _ of params.prompt) void _;
        if (isPlan) yield submitPlan("# plan");
        else yield signalDone();
        yield resultSuccess();
      })();
    };

    // A real runId + tool_packages, so provisionRunTools actually provisions and the
    // kick-off ordering below is measuring something.
    const probe = makeCtx({
      runId: "51210000-0000-4000-8000-000000000001",
      config: { tool_packages: ["kubectl"] },
    }, undefined);
    // Release the install from the GATE, but on a later macrotask: without the join the
    // implement turn would start on this same tick chain and observe it unfinished.
    const gated = probe.ctx.gatePlan!;
    probe.ctx.gatePlan = async (planMd) => {
      provisionOrder.push("gate");
      setTimeout(() => deferred.release(), 50);
      return gated(planMd);
    };

    await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision, installDeps: deferred.installDeps }).run(probe.ctx);

    assert.strictEqual(deferred.calls.length, 1, "the install must be started exactly once");
    assert.deepStrictEqual(provisionOrder, ["provision", "gate"], "tool provisioning must precede the plan gate");
    assert.strictEqual(
      installDoneAtPlanTurn,
      false,
      "the plan turn ran only after the install had already finished — the install was awaited at kick-off instead of overlapping the plan turn",
    );
    assert.strictEqual(
      installDoneAtImplementTurn,
      true,
      "the first implement turn started while the dependency install was still running — it was never joined",
    );
    // The install targets the run's clone, and the outcome is on the feed.
    assert.strictEqual(deferred.calls[0]!.root, probe.ctx.worktreePath);
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && String(m.payload["text"]).includes("installed JS dependencies in web")),
      "the run feed must report what was installed",
    );
  });

  // Local: `revise`/`approve` live in the PRD #41 describe block, not at file scope.
  const revise = (feedback: string): PlanVerdict => ({ kind: "revise", feedback });
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };

  it("JOINS before implement on the REVISE path too, not just the single-gate path", async () => {
    // The gate can be re-entered N times (PRD #41), and each re-entry runs another
    // PLANNING turn. The sibling test above covers one gate call; this covers two, so
    // a join that moved inside the revision loop — or a second entry into implement
    // that skipped it — cannot pass unnoticed.
    const RELEASE_MS = 400;
    const deferred = deferredInstall([{ dir: "web", manager: "npm", ok: true, detail: "npm ci --ignore-scripts ok" }]);

    // Observed INSIDE each turn, so the assertions read the real interleaving rather
    // than a post-hoc reconstruction (same discipline as the sibling test).
    const doneAtTurn: Record<number, boolean> = {};
    const startedAtTurn: Record<number, number> = {};
    let turn = 0;
    const queryFn: SdkQueryFn = (params) => {
      turn++;
      const t = turn;
      doneAtTurn[t] = deferred.isDone();
      startedAtTurn[t] = Date.now();
      return (async function* () {
        for await (const _ of params.prompt) void _;
        // turns 1 and 2 are PLANNING turns — a revision turn must submit a plan too.
        if (t <= 2) yield submitPlan(`# Plan v${t}`);
        else yield signalDone();
        yield resultSuccess();
      })();
    };

    const probe = makeCtx({}, [revise("add a rollback step"), approve]);
    // Release from the APPROVING (second) gate, on a later macrotask: without a join
    // the implement turn would start on this same tick chain and observe it unfinished.
    const gated = probe.ctx.gatePlan!;
    let gateCalls = 0;
    let releasedAt = 0;
    probe.ctx.gatePlan = async (planMd) => {
      gateCalls++;
      if (gateCalls === 2) {
        releasedAt = Date.now();
        setTimeout(() => deferred.release(), RELEASE_MS);
      }
      return gated(planMd);
    };

    await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps: deferred.installDeps }).run(probe.ctx);

    assert.strictEqual(gateCalls, 2, "the gate must have been re-entered once (revise → approve)");
    assert.strictEqual(turn, 3, "expected plan turn, revision turn, then one implement turn");
    assert.strictEqual(deferred.calls.length, 1, "the install must be started ONCE, not restarted per gate round");

    assert.strictEqual(
      doneAtTurn[1],
      false,
      "the plan turn ran only after the install finished — the install was awaited at kick-off instead of overlapping",
    );
    assert.strictEqual(
      doneAtTurn[2],
      false,
      "the REVISION turn ran only after the install finished — the revise path is not overlapping the install",
    );
    assert.strictEqual(
      doneAtTurn[3],
      true,
      "the first implement turn started while the dependency install was still running: on the revise path the agent " +
        "can run its own `npm ci` in the same dir as the worker-side install, and npm has no cross-process node_modules lock",
    );

    // The control. `doneAtTurn[3] === true` is satisfiable by coincidence (a fast
    // install, a scheduling accident); a gap at least as long as the release delay is
    // not — it proves the implement turn actually BLOCKED at the join.
    const gap = startedAtTurn[3]! - releasedAt;
    assert.ok(
      gap >= RELEASE_MS - 25,
      `the implement turn started ${gap}ms after the approving gate but the install was not released until ` +
        `${RELEASE_MS}ms — it did not wait at the join, it merely observed an install that had already finished`,
    );
  });

  it("carries the install's per-dir facts into the FIRST implement prompt (#157)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [assistantText("implementing"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => ({
      results: [
        { dir: "web", manager: "npm", ok: true, detail: "npm ci --ignore-scripts ok" },
        { dir: "agent", manager: "npm", ok: false, detail: "npm ci --ignore-scripts failed (exit 1)" },
      ],
      truncated: false,
    });
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);

    // Turn 1 is the plan turn: mechanism only, and it cannot know the outcome.
    assert.match(turns[0]!.promptText!, /do NOT put a manual/);
    assert.ok(!/deps_dirs_/.test(turns[0]!.promptText!), "the plan turn is built before the join, so it has no facts to give");
    // Turn 2 is the first implement turn: the real per-dir outcome, failure included.
    assert.match(turns[1]!.promptText!, /installed:\n1\. web/);
    assert.match(turns[1]!.promptText!, /failed:\n2\. agent/);
    // Turn 3 is a later implement turn on a resumed session — it must not repeat.
    assert.ok(!/deps_dirs_/.test(turns[2]!.promptText!));
  });

  it("tells the agent when discovery was TRUNCATED, so the list cannot read as exhaustive (#157 audit)", async () => {
    // joinDepsInstall used to return only `results`, dropping `truncated` on the floor —
    // so a repo past MAX_PROJECT_DIRS got a note that read as full coverage, recreating
    // the unexplainable `command not found` this change exists to remove.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => ({
      results: [{ dir: "web", manager: "npm", ok: true, detail: "ok" }],
      truncated: true,
    });
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);
    assert.match(turns[1]!.promptText!, /NOT the complete set of JS projects/);
  });

  it("the facts are still correct after a plan REVISION (#157)", async () => {
    // gatePlan can be re-entered, and each round runs another planning turn. The join
    // happens after the LAST round, so the implement prompt must carry the final
    // outcomes — not something captured at the first gate.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()],
      [submitPlan("# Plan v2"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => ({
      results: [{ dir: "web", manager: "npm", ok: true, detail: "npm ci --ignore-scripts ok" }],
      truncated: false,
    });
    const probe = makeCtx({}, [{ kind: "revise", feedback: "add a rollback step" }, { kind: "approve", selection: { status: "absent" } }]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);

    assert.strictEqual(turns.length, 3, "expected plan, revision, then one implement turn");
    // The REVISION turn is still a planning turn, riding a resumed session that already
    // carries the mechanism note — so it must not be handed facts it cannot have yet.
    assert.ok(!/deps_dirs_/.test(turns[1]!.promptText!), "the revision turn runs before the join");
    assert.match(turns[2]!.promptText!, /1\. web/, "the implement turn after a revision must still get the facts");
  });

  it("is best-effort: a failed install does NOT fail the run, and the skip is reported honestly", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => ({
      results: [
        { dir: "web", manager: "npm", ok: false, detail: "npm ci --ignore-scripts failed (exit 1) — node_modules absent, gates skip honestly" },
      ],
      truncated: false,
    });
    const probe = makeCtx();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);

    assert.strictEqual(result.branch, "agent/issue-5", "an install failure must never fail the run");
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && String(m.payload["text"]).includes("node_modules absent")),
      "a skipped install must say so on the feed, with its reason",
    );
  });

  it("survives an installer that THROWS (the call site never relies on the module's contract)", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => {
      throw new Error("installer blew up");
    };
    const probe = makeCtx();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "a throwing installer must not fail the run");
  });

  it("a REJECTED plan aborts the install instead of blocking teardown on it", async () => {
    const { queryFn } = fakeTurns([[submitPlan("# plan"), resultSuccess()]]);
    let sawSignal: AbortSignal | undefined;
    // Resolves ONLY on abort. If the executor did not abort, run() would hang here and
    // the test would fail on the suite timeout.
    const installDeps: SdkExecutorOptions["installDeps"] = (_root, _env, opts) =>
      new Promise((resolve) => {
        sawSignal = opts?.signal;
        opts?.signal?.addEventListener("abort", () => resolve({ results: [], truncated: false }), { once: true });
      });
    const probe = makeCtx({}, { kind: "reject", reason: "no" });

    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx),
      (err: unknown) => err instanceof PlanRejectedError,
    );
    assert.ok(sawSignal?.aborted, "a run that never implements must abort the install it will not use");
  });

  it("a CANCELLED run aborts the install", async () => {
    const controller = new AbortController();
    const { queryFn } = fakeTurns([[submitPlan("# plan"), resultSuccess()]]);
    let sawSignal: AbortSignal | undefined;
    const installDeps: SdkExecutorOptions["installDeps"] = (_root, _env, opts) =>
      new Promise((resolve) => {
        sawSignal = opts?.signal;
        opts?.signal?.addEventListener("abort", () => resolve({ results: [], truncated: false }), { once: true });
      });
    const probe = makeCtx({ signal: controller.signal }, { kind: "cancel" });
    // Cancel lands while the gate is deciding, exactly as a user cancel does.
    probe.ctx.gatePlan = async () => {
      controller.abort();
      return { kind: "cancel" };
    };
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx), /run cancelled/);
    assert.ok(sawSignal?.aborted, "a cancelled run must reclaim its install");
  });

  it("installs under the SCRUBBED check env: the run's provisioned PATH, no worker credentials", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const provision: SdkExecutorOptions["provision"] = async () => ({ toolEnv: { PATH: "/run/tools/bin:/usr/bin" } });
    const seen: NodeJS.ProcessEnv[] = [];
    const installDeps: SdkExecutorOptions["installDeps"] = async (_root, env) => {
      seen.push(env);
      return { results: [], truncated: false };
    };
    const probe = makeCtx({
      runId: "51210000-0000-4000-8000-000000000002",
      config: { tool_packages: ["kubectl"] },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, provision, installDeps }).run(probe.ctx);

    const env = seen[0]!;
    assert.strictEqual(env.PATH, "/run/tools/bin:/usr/bin", "the install must resolve the RUN's node/npm, not the image's");
    assert.strictEqual(env.HOME, homeDir);
    for (const k of ["UZI_WORKER_TOKEN", "UZI_WORKER_TOKEN_FILE", "UZI_API_URL", "UZI_FORGE_PAT", "ANTHROPIC_API_KEY"]) {
      assert.strictEqual(env[k], undefined, `${k} must be absent from the install env by construction`);
    }
    // beforeEach puts real-looking secrets in process.env; none may be reachable.
    const blob = JSON.stringify(env);
    for (const secret of [FAKE_JOIN_TOKEN, FAKE_PAT, OAUTH]) {
      assert.ok(!blob.includes(secret), "a worker credential leaked into the install env");
    }
  });

  it("says so plainly when the repo has no JS dependencies to install", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const installDeps: SdkExecutorOptions["installDeps"] = async () => ({ results: [], truncated: false });
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn, installDeps }).run(probe.ctx);
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && String(m.payload["text"]).includes("no JS dependencies to install")),
      "an empty result must be reported, not silently swallowed",
    );
  });
});

// ── PRD #35 Decision 6b: a resume past an approval that already happened ──────
describe("SdkExecutor — pre-approved resume skips the planning turn and the gate", () => {
  const preApproved = { planApproved: true, sessionId: "sess-parked", approvedPlan: "# Approved plan\nship it" };

  // 🔴 THE HALF-A-VERDICT CASE. plan_approved and the selection come from ONE human
  // decision, so a resume that honours the approval must honour the exclusions too.
  // Before the claim carried the selection this silently resolved to the absent
  // default, and a human who excluded `reviewer` at the gate got it back after a
  // park with no signal.
  it("honours the persisted selection the claim carried, not the absent-default", async () => {
    const { queryFn, turns } = fakeTurns([[signalDone(), resultSuccess()]]);
    const probe = makeCtx({
      ...preApproved,
      agents: [lead, coder, reviewer],
      // The clone HAS a roster, so the absent-default resolves to `repo` — a
      // different SOURCE and a genuinely different set. The persisted selection says
      // `own` minus `reviewer`, so honouring it must yield the owner's roster with
      // `reviewer` gone. That distinguishes it from BOTH the absent-default and a
      // plain `own` with no exclusions.
      repoAgents: [repoCoder, repoAuditor],
      approvedSelection: { source: "own", exclusions: ["reviewer"] },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(Object.keys(turns[0]!.options.agents ?? {}).sort(), ["coder"]);
  });

  it("falls back to the absent-default when the claim carried no selection", async () => {
    // A gate-less run, or an older server. Absence is not a signal to distrust, so
    // this must NOT force `own` — resolveAgentSelection's documented default applies,
    // which with a detected roster is `repo`.
    const { queryFn, turns } = fakeTurns([[signalDone(), resultSuccess()]]);
    const probe = makeCtx({
      ...preApproved,
      agents: [lead, coder, reviewer],
      repoAgents: [repoCoder, repoAuditor],
      approvedSelection: undefined,
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(probe.gated, [], "still skips the gate");
    assert.deepStrictEqual(Object.keys(turns[0]!.options.agents ?? {}).sort(), ["auditor", "coder"]);
  });

  it("skips both, and never calls gatePlan, when the plan is approved and the session resumable", async () => {
    // One turn only: the implement turn. A run that still planned would need two.
    const { queryFn, turns } = fakeTurns([[signalDone(), resultSuccess()]]);
    const probe = makeCtx(preApproved);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    // The gate is what a HUMAN answers. Asking again for a plan they already
    // approved is exactly what turns a park into an unattended stall — and can fail
    // outright with REASON_NO_PLAN when the resumed session declines to re-emit it.
    assert.deepStrictEqual(probe.gated, [], "gatePlan must not be called for an already-approved plan");
    assert.strictEqual(turns.length, 1, "no planning turn should run");
    assert.ok(
      probe.emits.some((m) => JSON.stringify(m).includes("skipping the planning turn")),
      "the feed must say the planning turn was skipped",
    );
  });

  // Each condition ALONE must not skip. These are the cases where planning is still
  // the right answer, and skipping on any of them would enter implement with no plan
  // or resume a session that is not there.
  for (const [name, over] of [
    ["there is no session to resume (issue #105 dropped it)", { sessionId: null }],
    ["the plan is not approved yet — a park during the planning phase", { planApproved: false }],
    ["it is approved but no plan text arrived", { approvedPlan: "" }],
  ] as [string, Partial<RunContext>][]) {
    it(`still plans and gates when ${name}`, async () => {
      const { queryFn } = fakeTurns([
        [submitPlan("# Plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const probe = makeCtx({ ...preApproved, ...over });
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.strictEqual(probe.gated.length, 1, "the gate must still run");
    });
  }
});

// --- Plan-turn write-tool subtraction (#203) ----------------------------------
//
// Before the approval gate, a subagent write is an uncommitted worktree change the
// approver never saw, which the first implement-phase commit then sweeps in. The
// worker takes the four file-write tools off every subagent it dispatches on a
// PLANNING turn (agents.ts planTurnSubagents, wired at the `agents:` key of
// baseOptions) and leaves the implement turn alone.
//
// These tests assert on the OPTIONS OBJECT the executor hands the SDK, which is the
// only channel available: whether the SDK resolves `tools` or `disallowedTools`
// first is unproven and happens inside the compiled `claude` binary. That is exactly
// why the transform does BOTH, and why both are asserted here — a test that checked
// only one would go green against an implementation that had silently dropped the
// other, leaving the run's actual safety resting on the undocumented precedence.
describe("plan-turn write-tool subtraction (#203)", () => {
  const WRITE_TOOLS = ["Edit", "Write", "MultiEdit", "NotebookEdit"];

  // Declares all four write tools, so ONE fixture parameterises the whole set. The
  // shipped architect declares (Edit, Write) — a subset — so a regression that
  // handled only the shipped pair still reddens here.
  const architectA: AgentTemplate = {
    name: "architect",
    description: "designs",
    prompt_body: "You design.",
    tools: ["Bash", "Read", "Grep", "Edit", "Write", "MultiEdit", "NotebookEdit", "WebFetch"],
  };
  // Mirrors the shipped tester's shape (Edit, Write among reads).
  const testerA: AgentTemplate = {
    name: "tester",
    description: "tests",
    prompt_body: "You test.",
    tools: ["Bash", "Read", "Grep", "Glob", "Edit", "Write"],
  };
  // The INHERIT-ALL arm. The shipped `coder` ships no `tools:` line at all
  // (deliberate, agents.ts header) and is invokable on the plan turn, so an
  // implementation that only edited template frontmatter would miss it entirely.
  const inheritCoder: AgentTemplate = {
    name: "coder",
    description: "implements",
    prompt_body: "You implement.",
    tools: null,
  };

  /** Drive one plan turn + one implement turn and hand back both option objects. */
  async function planAndImplement(agents: AgentTemplate[]) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(turns.length, 2, "one planning turn and one implement turn");
    return { plan: turns[0]!.options, impl: turns[1]!.options, probe };
  }

  it("strips every write tool from every declaring role on the plan turn, and restores them to implement", async () => {
    const { plan, impl } = await planAndImplement([lead, architectA, testerA, reviewer]);

    for (const t of WRITE_TOOLS) {
      for (const role of ["architect", "tester"]) {
        assert.ok(
          !(plan.agents![role]!.tools ?? []).includes(t),
          `${role} must not be granted ${t} on the plan turn`,
        );
        assert.ok(
          plan.agents![role]!.disallowedTools!.includes(t),
          `${role} must additionally DENY ${t} on the plan turn (both operations, always)`,
        );
      }
    }

    // The implement turn is untouched: each role gets back EXACTLY what it declared,
    // in declaration order. This is the half that proves the scoping — the same
    // subtraction applied one key lower (baseOptions.disallowedTools) would be
    // inherited through the implementOptions spread and redden here.
    assert.deepStrictEqual(impl.agents!.architect!.tools, architectA.tools);
    assert.deepStrictEqual(impl.agents!.tester!.tools, testerA.tools);
    for (const role of ["architect", "tester"]) {
      assert.deepStrictEqual(
        impl.agents![role]!.disallowedTools,
        ["Agent", "mcp__uzi", "mcp__memory"],
        `${role} carries only the structural denials on the implement turn`,
      );
    }

    // Non-write tools are NOT collateral: only the four come off.
    assert.deepStrictEqual(
      plan.agents!.architect!.tools,
      ["Bash", "Read", "Grep", "WebFetch"],
      "reads, Bash and WebFetch survive the plan turn untouched",
    );
    // A role that declared no write tool in the first place is unchanged.
    assert.deepStrictEqual(plan.agents!.reviewer!.tools, reviewer.tools);
  });

  it("the inherit-all role (no `tools:` line) is covered by the denial arm, and only on the plan turn", async () => {
    const { plan, impl } = await planAndImplement([lead, inheritCoder, reviewer]);

    // `tools` STAYS UNSET. Materializing an allowlist here would silently narrow
    // coder to whatever the transform imagined the full toolset to be — the
    // inherit-all contract (PRD #3) is what the absent key means.
    assert.strictEqual(plan.agents!.coder!.tools, undefined, "inherit-all is preserved as an absent key");
    for (const t of WRITE_TOOLS) {
      assert.ok(plan.agents!.coder!.disallowedTools!.includes(t), `coder must deny ${t} on the plan turn`);
    }
    // The structural denials are kept, not replaced.
    for (const t of ["Agent", "mcp__uzi", "mcp__memory"]) {
      assert.ok(plan.agents!.coder!.disallowedTools!.includes(t), `${t} still denied on the plan turn`);
    }

    // On the implement turn coder is back to inherit-all with no write denial —
    // otherwise the role that exists to write code could not write code.
    assert.strictEqual(impl.agents!.coder!.tools, undefined);
    assert.deepStrictEqual(impl.agents!.coder!.disallowedTools, ["Agent", "mcp__uzi", "mcp__memory"]);
  });

  it("the TOP-LEVEL disallowedTools is untouched on both turns (the denial is per-agent, not global)", async () => {
    const { plan, impl } = await planAndImplement([lead, architectA, inheritCoder]);
    // Putting the subtraction on baseOptions.disallowedTools is the naive reading and
    // is wrong: implementOptions spreads baseOptions, so it would deny the write tools
    // for the whole run. Pinning BOTH turns is what makes that a red rather than a
    // silently-passing alternative implementation.
    assert.deepStrictEqual(plan.disallowedTools, ["ScheduleWakeup", "CronCreate"]);
    assert.deepStrictEqual(impl.disallowedTools, ["ScheduleWakeup", "CronCreate"]);
  });

  it("a REVISE round is a planning turn too: the second plan turn carries the same subtraction", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan v1"), resultSuccess()], // first planning turn
      [submitPlan("# Plan v2"), resultSuccess()], // the revision turn
      [signalDone(), resultSuccess()], // implement
    ]);
    const probe = makeCtx({ agents: [lead, architectA, inheritCoder] }, [
      { kind: "revise", feedback: "narrow it" },
      { kind: "approve", selection: { status: "absent" } },
    ]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(turns.length, 3, "plan + revise + implement");

    for (const turnIdx of [0, 1]) {
      const opts = turns[turnIdx]!.options;
      for (const t of WRITE_TOOLS) {
        assert.ok(
          !(opts.agents!.architect!.tools ?? []).includes(t),
          `turn ${turnIdx}: architect must not be granted ${t}`,
        );
        assert.ok(
          opts.agents!.architect!.disallowedTools!.includes(t),
          `turn ${turnIdx}: architect must deny ${t}`,
        );
        assert.ok(opts.agents!.coder!.disallowedTools!.includes(t), `turn ${turnIdx}: coder must deny ${t}`);
      }
    }
    // ...and the implement turn still restores them.
    assert.deepStrictEqual(turns[2]!.options.agents!.architect!.tools, architectA.tools);
  });
});

// A template whose declared allowlist is ONLY write tools. Filtering alone would
// leave it with `tools: []`, which the repo reads as inherit-all in three places —
// so the transform drops it from the plan turn instead (#203). These tests pin the
// three sites that must agree about that drop: the `agents` map, the Agent-guard
// allowSet, and the plan prompt's "Available subagents" line. Keeping a name in
// either of the latter two while the definition is gone would advertise an agent
// the SDK cannot resolve, which is worse than the `tools: []` it replaces.
describe("plan-turn drop of a write-only allowlist (#203)", () => {
  const penman: AgentTemplate = {
    name: "penman",
    description: "writes docs",
    prompt_body: "You write.",
    tools: ["Edit", "Write"],
  };

  async function planAndImplement(agents: AgentTemplate[]) {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ agents });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    return { plan: turns[0]!, impl: turns[1]!, probe };
  }

  it("drops it from the plan roster, the Agent guard and the plan prompt — and restores it to implement", async () => {
    const { plan, impl } = await planAndImplement([lead, coder, reviewer, penman]);

    // 1. The map.
    assert.deepStrictEqual(Object.keys(plan.options.agents ?? {}).sort(), ["coder", "reviewer"]);
    // No survivor carries an empty allowlist — the state this drop exists to avoid.
    for (const [name, def] of Object.entries(plan.options.agents ?? {})) {
      assert.notDeepStrictEqual(def.tools, [], `${name} must not carry an empty allowlist`);
    }

    // 2. The Agent guard. `penman` must be DENIED on the plan turn: the guard's
    //    allowSet is frozen at construction from the same names as the map, so a
    //    drop that missed it would let the lead invoke an undefined agent.
    const agentHook = plan.options.hooks!.PreToolUse![2]!.hooks[0]!;
    const call = (subagent_type: string) =>
      agentHook(
        { hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { subagent_type } } as unknown as HookInput,
        "tu",
        { signal: new AbortController().signal },
      );
    assert.strictEqual(
      ((await call("penman")) as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput
        ?.permissionDecision,
      "deny",
      "a dropped agent must be denied by the plan turn's Agent guard",
    );
    // Control: a surviving agent is still allowed, so the assertion above is about
    // `penman` and not about a guard that denies everything.
    assert.strictEqual(
      ((await call("coder")) as { hookSpecificOutput?: { updatedInput?: Record<string, unknown> } })
        .hookSpecificOutput?.updatedInput?.run_in_background,
      false,
      "a surviving agent is still allowed",
    );

    // 3. The plan prompt. The lead must not be told it can delegate to `penman`.
    //    Asserted on the DELEGATES LINE, not by searching the whole prompt: a
    //    substring search over prompt prose is not this question, and it bites —
    //    the first draft of this test used the fixture name `scribe`, which is a
    //    substring of "described" in the plan boilerplate, so the assertion failed
    //    against correct code. The line is the channel; the prose is not.
    assert.ok(plan.promptText, "the plan turn carried a prompt");
    const delegates = plan.promptText!.split("\n").find((l) => l.startsWith("Available subagents to delegate to:"));
    assert.ok(delegates, `no delegates line in the plan prompt:\n${plan.promptText}`);
    assert.strictEqual(
      delegates,
      "Available subagents to delegate to: coder, reviewer.",
      "the plan prompt advertises exactly the plan-turn roster",
    );

    // 4. The IMPLEMENT turn is untouched — the drop is plan-turn-scoped, and
    //    `penman` gets its declared write tools back where writing is the job.
    assert.deepStrictEqual(Object.keys(impl.options.agents ?? {}).sort(), ["coder", "penman", "reviewer"]);
    assert.deepStrictEqual(impl.options.agents!.penman!.tools, ["Edit", "Write"]);
  });

  it("says so on the run feed rather than dropping it silently", async () => {
    const { probe } = await planAndImplement([lead, coder, penman]);
    const statuses = probe.emits.filter((m) => m.kind === "status").map((m) => String(m.payload["text"]));
    const notice = statuses.find((t) => t.includes("penman"));
    assert.ok(notice, `no feed notice named the dropped agent:\n${statuses.join("\n")}`);
    assert.match(notice!, /file-writing only/);
  });

  it("emits no such notice when nothing is dropped", async () => {
    // The control that keeps the assertion above from passing on a worker that
    // announces a drop on every run.
    const { probe } = await planAndImplement([lead, coder, reviewer]);
    const statuses = probe.emits.filter((m) => m.kind === "status").map((m) => String(m.payload["text"]));
    assert.ok(!statuses.some((t) => t.includes("file-writing only")), statuses.join("\n"));
  });
});
