import { after, afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { HookInput, HookJSONOutput, Options } from "@anthropic-ai/claude-agent-sdk";
import { buildPathGuardHook } from "../src/guardrails.js";
import { buildSdkOptions } from "../src/claude-harness.js";
import { buildFetchToolsServer } from "../src/fetch-tools.js";
import { isolateSdkEnv } from "../src/isolated-executor.js";
import { buildJobResultServer, buildJobSdkOptions, laneJobSurface } from "../src/job-runner.js";
import { runReadOnlyModelPass } from "../src/model-pass.js";
import type { ClaimSkill } from "../src/protocol.js";
import { promptStream } from "../src/sdk-messages.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { materializeSkillsPlugin, qualifiedSkillName } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";
import { FakeAnthropicApi, type MessagesRequest, type ScriptedTurn } from "./fake-anthropic-api.js";

// Issue #1563 — with `verbatimPrompts: true` on every production options builder, the real bundled
// Claude CLI must still behave normally on a lane's FIRST turn: the granted tools are offered, the
// materialized skills plugin is listed to the model, a `Skill` call is invocable, and the advice
// lane (tool-less, deny-all hook) still returns a scripted reply. Every scenario drives the
// production options builder against a scripted local fake of the Messages API; the evidence is
// the provider-bound request bodies. Nothing leaves 127.0.0.1, the key is a placeholder, and all
// paths are disposable mkdtemp roots removed afterwards.

const DRIVE_BUDGET_MS = 50_000;
const SKILL_NAME = "probe-skill";
const SKILL_DESC_MARKER = "CANARY-SKILLDESC-1563-f2b6";
const SKILL_BODY_MARKER = "CANARY-SKILLBODY-1563-91ac";
const DENIED_MARKER = "CANARY-FIRSTTURN-DENIED-1563-3be7";
const OUTSIDE_MARKER = "CANARY-FIRSTTURN-OUTSIDE-1563-8d40";
const ADVICE_REPLY = "ADVICE-REPLY-1563-scripted";
const SKILL: ClaimSkill = {
  name: SKILL_NAME,
  description: `Probe playbook ${SKILL_DESC_MARKER}. Use it when asked for the probe.`,
  body: `# Probe\n\nFollow this playbook: ${SKILL_BODY_MARKER}\n`,
};

const toolNames = (b: MessagesRequest): string[] => (b.tools ?? []).map((t) => t.name);
const has = (b: MessagesRequest, marker: string): boolean => JSON.stringify(b).includes(marker);

describe("first-turn compatibility with verbatimPrompts true (issue #1563)", () => {
  const roots: string[] = [];
  let root: string;
  let home: string;
  let worktree: string;
  let tmp: string;
  let denied: string;
  let outside: string;
  let fake: FakeAnthropicApi;
  let baseUrl: string;
  /** The skill the scripted model calls on its first Skill-bearing request; undefined = none. */
  let skillToCall: string | undefined;
  let skillIssued = false;
  let replyText = "ok";

  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  /** A Skill-bearing request gets the one scripted Skill call, then plain text (also for titles and advice). */
  function script(body: MessagesRequest): ScriptedTurn {
    if (toolNames(body).includes("Skill")) {
      if (skillToCall && !skillIssued) {
        skillIssued = true;
        return { kind: "tool_use", name: "Skill", input: { skill: skillToCall } };
      }
    }
    return { kind: "text", text: replyText };
  }

  beforeEach(async () => {
    root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-firstturn-it-"));
    roots.push(root);
    home = path.join(root, "home");
    worktree = path.join(root, "worktree");
    tmp = path.join(root, "tmp");
    for (const d of [path.join(home, ".claude"), worktree, tmp]) fs.mkdirSync(d, { recursive: true });
    denied = path.join(worktree, "synthetic-denied.txt");
    fs.writeFileSync(denied, `${DENIED_MARKER}\n`);
    const outsideRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-firstturn-outside-"));
    roots.push(outsideRoot);
    outside = path.join(outsideRoot, "synthetic-outside.txt");
    fs.writeFileSync(outside, `${OUTSIDE_MARKER}\n`);
    skillToCall = undefined;
    skillIssued = false;
    replyText = "done";
    fake = new FakeAnthropicApi(script);
    baseUrl = await fake.listen();
  });

  afterEach(async () => {
    await fake.close();
  });

  const env = (): Record<string, string | undefined> => ({
    PATH: process.env.PATH ?? "",
    HOME: home,
    TMPDIR: tmp,
    ANTHROPIC_BASE_URL: baseUrl,
    ANTHROPIC_API_KEY: ["dummy", "key"].join("-"),
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
    DISABLE_TELEMETRY: "1",
    DISABLE_AUTOUPDATER: "1",
  });

  /** The production skills plugin: materialized from a ClaimSkill at the sibling-of-worktree location. */
  async function materialize(): Promise<string> {
    const dir = path.join(root, "skills-plugin");
    await materializeSkillsPlugin(dir, [SKILL]);
    return dir;
  }

  const mention = (): string => `Please read @${denied} and @${outside} and then use the probe skill.`;

  async function run(options: Options, prompt: string): Promise<void> {
    fake.messageBodies.length = 0;
    fake.requests.length = 0;
    skillIssued = false;
    const controller = new AbortController();
    const deadline = AbortSignal.timeout(DRIVE_BUDGET_MS);
    deadline.addEventListener("abort", () => controller.abort());
    try {
      for await (const _ of query({ prompt: promptStream(prompt) as never, options: { ...options, abortController: controller } })) {
        // drain
      }
    } catch (err) {
      assert.fail(`query failed: ${String(err)}\nfake API requests:\n${fake.requestLog()}`);
    }
    assert.ok(!controller.signal.aborted, `the drive hit its ${DRIVE_BUDGET_MS}ms budget\nfake API requests:\n${fake.requestLog()}`);
  }

  /** The main-thread requests (they carry the Skill tool), in order. */
  const mains = (): MessagesRequest[] => fake.messageBodies.filter((b) => toolNames(b).includes("Skill"));

  /** The skill is listed to the model, invocable, and the prompt's @path mentions stayed literal. */
  function assertSkillFirstTurn(): MessagesRequest[] {
    const main = mains();
    assert.ok(main.length >= 2, `expected a request after the Skill call, got ${main.length}\n${fake.requestLog()}`);
    assert.ok(has(main[0]!, SKILL_DESC_MARKER), "the skill listing (its description) is missing from the first main-thread request");
    assert.ok(!has(main[0]!, SKILL_BODY_MARKER), "the skill body was sent before the skill was invoked");
    const second = JSON.stringify(main[1]!.messages);
    assert.ok(second.includes(SKILL_BODY_MARKER), "the Skill call did not return the skill body to the model");
    const results = main[1]!.messages.flatMap((m) =>
      Array.isArray(m.content) ? (m.content as Array<Record<string, unknown>>).filter((c) => c["type"] === "tool_result") : [],
    );
    assert.ok(results.length >= 1 && results.every((r) => r["is_error"] !== true), `the Skill tool_result was an error: ${JSON.stringify(results)}`);
    const all = JSON.stringify(fake.messageBodies);
    assert.ok(!all.includes(DENIED_MARKER) && !all.includes(OUTSIDE_MARKER), "an @path mention expanded to file content");
    return main;
  }

  const sameSet = (got: string[], want: readonly string[], label: string): void => {
    assert.deepEqual([...new Set(got)].sort(), [...new Set(want)].sort(), label);
  };

  it("run lane: granted tools, skill listing and Skill invocation work on the first turn", { timeout: 120_000 }, async () => {
    const plugin = await materialize();
    const guard = buildPathGuardHook(worktree, nullLogger(), [denied], { sdkHomeDir: home });
    const options = buildSdkOptions({
      cwd: worktree,
      env: env(),
      skillsPluginPath: plugin,
      skills: [qualifiedSkillName(SKILL_NAME)],
      systemPrompt: { type: "preset", preset: "claude_code", append: "canary" },
      agents: {},
      mcpServers: {},
      preToolUse: [{ matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep", hooks: [(i: HookInput): Promise<HookJSONOutput> => guard(i)] }],
      model: "claude-sonnet-4-5",
    });
    assert.equal(options.verbatimPrompts, true);
    skillToCall = qualifiedSkillName(SKILL_NAME);
    await run(options, mention());
    const main = assertSkillFirstTurn();
    // Glob and Grep are not listed by name in this bundled CLI's default set, so they are not asserted.
    for (const t of ["Read", "Edit", "Write", "Skill"]) {
      assert.ok(toolNames(main[0]!).includes(t), `the run lane's first request lacks the ${t} tool: ${toolNames(main[0]!).join(",")}`);
    }
  });

  it("non-lane job with product skills: skill listed and invocable, tools equal the job allowlist", { timeout: 120_000 }, async () => {
    const plugin = await materialize();
    const rs = buildJobResultServer({});
    const options = buildJobSdkOptions({
      env: env(),
      systemPrompt: "You are a uzi job worker. Use the Skill tool when a skill matches.",
      workDir: worktree,
      log: nullLogger(),
      secretPaths: [denied],
      resultServer: rs.server,
      toolNames: rs.toolNames,
      model: "claude-sonnet-4-5",
      productSkills: { pluginPath: plugin, names: [qualifiedSkillName(SKILL_NAME)] },
    });
    assert.equal(options.verbatimPrompts, true);
    skillToCall = qualifiedSkillName(SKILL_NAME);
    await run(options, mention());
    const main = assertSkillFirstTurn();
    sameSet(toolNames(main[0]!), options.tools as string[], "first-request tools must equal the job's configured tool list");
  });

  it("lane job with product skills: skill listed and invocable, tools equal lane.surface.tools", { timeout: 120_000 }, async () => {
    const plugin = await materialize();
    const rs = buildJobResultServer({});
    const surface = laneJobSurface([qualifiedSkillName(SKILL_NAME)]);
    const fetchServer = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace: worktree, log: nullLogger() }).server;
    // The init gate is closed until the runner's init-frame check passes it; this drive has no runner,
    // so the gate is opened here to let the Skill call reach the tool-set check instead of REASON_INIT_PENDING.
    const options = buildJobSdkOptions({
      env: isolateSdkEnv(env() as never) as unknown as Record<string, string | undefined>,
      systemPrompt: "You are a uzi job worker. Use the Skill tool when a skill matches.",
      workDir: worktree,
      log: nullLogger(),
      secretPaths: [denied],
      resultServer: rs.server,
      toolNames: rs.toolNames,
      model: "claude-sonnet-4-5",
      productSkills: { pluginPath: plugin, names: [qualifiedSkillName(SKILL_NAME)] },
      lane: { surface, gate: { passed: true }, fetchServer },
    });
    assert.equal(options.verbatimPrompts, true);
    assert.ok(surface.tools.includes("Skill") && surface.skills.includes(qualifiedSkillName(SKILL_NAME)), "the lane surface carries the skill");
    skillToCall = qualifiedSkillName(SKILL_NAME);
    await run(options, mention());
    const main = assertSkillFirstTurn();
    sameSet(toolNames(main[0]!), surface.tools, "first-request tools must equal lane.surface.tools (no widening)");
  });

  /** Production advice options reach the real CLI through a queryFn that only rewrites `env` (the
   *  credential for the fake provider's base URL and dummy key, plus TMPDIR and the DISABLE_* flags)
   *  and `cwd`; every other option, hooks and verbatimPrompts included, is production's. */
  function adviceQuery(flip: boolean): SdkQueryFn {
    return (p) => {
      const e: Record<string, string | undefined> = { ...p.options.env };
      delete e["CLAUDE_CODE_OAUTH_TOKEN"];
      Object.assign(e, { ANTHROPIC_BASE_URL: baseUrl, ANTHROPIC_API_KEY: ["dummy", "key"].join("-"), TMPDIR: tmp, DISABLE_TELEMETRY: "1", DISABLE_AUTOUPDATER: "1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1" });
      const seen = { ...p.options, env: e, cwd: worktree, ...(flip ? { verbatimPrompts: false } : {}) };
      return query({ prompt: p.prompt as never, options: seen }) as ReturnType<SdkQueryFn>;
    };
  }

  async function advice(flip: boolean): Promise<string> {
    fake.messageBodies.length = 0;
    replyText = ADVICE_REPLY;
    return runReadOnlyModelPass({
      token: ["dummy", "oauth"].join("-"),
      model: "claude-sonnet-4-5",
      systemPrompt: "You are a read-only reviewer.",
      prompt: mention(),
      homeRoot: tmp,
      homePrefix: "uzi-advice-",
      label: "review",
      timeoutMs: DRIVE_BUDGET_MS,
      queryFn: adviceQuery(flip),
      denyReason: "advice is read-only",
      log: nullLogger(),
    });
  }

  it("advice lane: ClaudeAdviceHarness returns the scripted reply and injects no @path content", { timeout: 120_000 }, async () => {
    // Control: the same pass with the option flipped off shows the fixture WOULD expand.
    const controlText = await advice(true);
    assert.ok(controlText.includes(ADVICE_REPLY), `control: the pass returned ${JSON.stringify(controlText)}`);
    const controlBodies = JSON.stringify(fake.messageBodies);
    assert.ok(controlBodies.includes(DENIED_MARKER) && controlBodies.includes(OUTSIDE_MARKER), "control: the advice fixture did not expand, so this probe proves nothing");

    const text = await advice(false);
    assert.ok(text.includes(ADVICE_REPLY), `the advice pass did not return the scripted reply: ${JSON.stringify(text)}`);
    const bodies = JSON.stringify(fake.messageBodies);
    assert.ok(!bodies.includes(DENIED_MARKER) && !bodies.includes(OUTSIDE_MARKER), "an @path mention expanded in the advice lane");
    assert.ok(bodies.includes(`@${denied}`) && bodies.includes(`@${outside}`), "the advice prompt was not delivered literally");
  });
});
