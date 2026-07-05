import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage, HookInput } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, resolveLeadModel, type SdkQueryFn } from "../src/sdk-executor.js";
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
        signal.addEventListener("abort", () => resolve(), { once: true });
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

function makeCtx(overrides: Partial<RunContext> = {}, verdict: PlanVerdict = { kind: "approve" }): CtxProbe {
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

    assert.deepStrictEqual(result, { branch: "agent/issue-5" });
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
