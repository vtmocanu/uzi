import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import type { AgentDefinition } from "@anthropic-ai/claude-agent-sdk";
import {
  GUARDED_COMMAND_SERVER,
  GUARDED_COMMAND_TOOL,
  shapeGuardedGrants,
} from "../src/claude-command/grants.js";
import { assembleAgents, planTurnSubagents } from "../src/agents.js";
import { buildSdkOptions, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { ASYNC_DEFERRAL_TOOLS } from "../src/guardrails.js";
import { CLAUDE_GUARDED_COMMAND_APPEND, CLAUDE_LONG_COMMAND_APPEND } from "../src/prompt.js";
import type { AgentTemplate } from "../src/protocol.js";

const BUILTINS_DIR = join(import.meta.dirname, "..", "..", "api", "internal", "agenttmpl", "builtins");

/** Parse the shipped builtin templates (frontmatter name/description/tools + body). */
function builtinTemplates(): AgentTemplate[] {
  return readdirSync(BUILTINS_DIR)
    .filter((f) => f.endsWith(".md"))
    .sort()
    .map((f) => {
      const raw = readFileSync(join(BUILTINS_DIR, f), "utf8");
      const m = /^---\n([\s\S]*?)\n---\n([\s\S]*)$/.exec(raw);
      assert.ok(m, `${f} has frontmatter`);
      const fm = new Map<string, string>();
      for (const line of m[1]!.split("\n")) {
        const i = line.indexOf(":");
        if (i > 0) fm.set(line.slice(0, i).trim(), line.slice(i + 1).trim());
      }
      const tools = fm.get("tools");
      return {
        name: fm.get("name") ?? f,
        description: fm.get("description") ?? "",
        prompt_body: m[2]!,
        tools: tools ? tools.split(",").map((t) => t.trim()).filter(Boolean) : null,
        model: null,
      } as AgentTemplate;
    });
}

function def(tools: string[] | undefined, extra: Partial<AgentDefinition> = {}): AgentDefinition {
  const d: AgentDefinition = {
    description: "d",
    prompt: `body\n\n${CLAUDE_LONG_COMMAND_APPEND}\n\nsafety`,
    ...extra,
  };
  if (tools) d.tools = tools;
  return d;
}

describe("shapeGuardedGrants", () => {
  it("replaces plain Bash with the mapped tool, names the server, and swaps the prompt append", () => {
    const { agents, unavailable } = shapeGuardedGrants({ a: def(["Bash", "Read"]) });
    assert.deepEqual(unavailable, []);
    assert.deepEqual(agents.a!.tools, [GUARDED_COMMAND_TOOL, "Read"]);
    assert.deepEqual(agents.a!.mcpServers, [GUARDED_COMMAND_SERVER]);
    assert.ok(agents.a!.prompt.includes(CLAUDE_GUARDED_COMMAND_APPEND));
    assert.ok(!agents.a!.prompt.includes(CLAUDE_LONG_COMMAND_APPEND));
    assert.ok(agents.a!.prompt.startsWith("body\n\n") && agents.a!.prompt.endsWith("\n\nsafety"));
  });

  it("drops scoped Bash entries when plain Bash is also granted and keeps existing servers", () => {
    const { agents } = shapeGuardedGrants({ a: def(["Bash(git status:*)", "Bash", "Read"], { mcpServers: ["findings"] }) });
    assert.deepEqual(agents.a!.tools, [GUARDED_COMMAND_TOOL, "Read"]);
    assert.deepEqual(agents.a!.mcpServers, ["findings", GUARDED_COMMAND_SERVER]);
  });

  it("moves a role whose only shell grants are scoped Bash to unavailable", () => {
    const { agents, unavailable } = shapeGuardedGrants({ s: def(["Bash(git status:*)", "Read"]), ok: def(["Read"]) });
    assert.ok(!("s" in agents));
    assert.equal(unavailable.length, 1);
    assert.equal(unavailable[0]!.name, "s");
    assert.match(unavailable[0]!.reason, /scoped Bash/);
    assert.deepEqual(Object.keys(agents), ["ok"]);
  });

  it("leaves an explicit allowlist without Bash unchanged and adds no server", () => {
    const { agents } = shapeGuardedGrants({ r: def(["Read", "Grep"]) });
    assert.deepEqual(agents.r!.tools, ["Read", "Grep"]);
    assert.equal(agents.r!.mcpServers, undefined);
  });

  it("leaves an inherit-all definition without a tools allowlist", () => {
    const { agents } = shapeGuardedGrants({ c: def(undefined) });
    assert.equal("tools" in agents.c!, false);
    assert.equal(agents.c!.mcpServers, undefined);
    assert.ok(agents.c!.prompt.includes(CLAUDE_GUARDED_COMMAND_APPEND));
  });

  it("denies the mapped tool too when disallowedTools names Bash, and leaves other denies alone", () => {
    const input = { c: def(undefined, { disallowedTools: ["Bash", "Edit"] }), d: def(undefined, { disallowedTools: ["Edit"] }) };
    const { agents } = shapeGuardedGrants(input);
    assert.deepEqual(agents.c!.disallowedTools, ["Bash", "Edit", GUARDED_COMMAND_TOOL]);
    assert.deepEqual(agents.d!.disallowedTools, ["Edit"]);
    assert.deepEqual(input.c.disallowedTools, ["Bash", "Edit"], "input not mutated");
    const again = shapeGuardedGrants({ c: def(undefined, { disallowedTools: ["Bash", GUARDED_COMMAND_TOOL] }) });
    assert.deepEqual(again.agents.c!.disallowedTools, ["Bash", GUARDED_COMMAND_TOOL]);
    const scoped = shapeGuardedGrants({ e: def(undefined, { disallowedTools: ["Bash(rm:*)"] }) });
    assert.deepEqual(scoped.agents.e!.disallowedTools, ["Bash(rm:*)", GUARDED_COMMAND_TOOL], "a scoped Bash deny fails closed");
  });

  it("throws when a definition prompt lacks the long-command append", () => {
    assert.throws(() => shapeGuardedGrants({ x: def(["Bash"], { prompt: "no append here" }) }), /long-command append/);
  });

  it("does not mutate its input", () => {
    const input = { a: def(["Bash", "Read"], { mcpServers: ["findings"] }), c: def(undefined) };
    const snapshot = structuredClone(input);
    shapeGuardedGrants(input);
    assert.deepEqual(input, snapshot);
  });

  it("shapes the real builtin roster: every Bash-granting role maps, none is unavailable", () => {
    const templates = builtinTemplates().filter((t) => t.name !== "lead");
    const { subagents } = assembleAgents(templates);
    const { agents, unavailable } = shapeGuardedGrants(subagents);
    assert.deepEqual(unavailable, []);
    assert.deepEqual(Object.keys(agents), Object.keys(subagents));
    let mapped = 0;
    for (const [name, shaped] of Object.entries(agents)) {
      const before = subagents[name]!;
      assert.ok(!(shaped.tools ?? []).includes("Bash"), `${name} still names plain Bash`);
      if (before.tools?.includes("Bash")) {
        mapped++;
        assert.ok(shaped.tools!.includes(GUARDED_COMMAND_TOOL), `${name} lacks the mapped tool`);
        assert.ok(shaped.mcpServers!.includes(GUARDED_COMMAND_SERVER), `${name} lacks the server`);
      }
      assert.ok(shaped.prompt.includes(CLAUDE_GUARDED_COMMAND_APPEND), `${name} prompt not swapped`);
    }
    assert.ok(mapped >= 5, "the roster has several Bash-granting roles");
  });

  it("shapes the plan-turn roster: write tools stay stripped and only mapped names remain", () => {
    const { subagents } = assembleAgents(builtinTemplates().filter((t) => t.name !== "lead"));
    const plan = planTurnSubagents(subagents);
    const { agents } = shapeGuardedGrants(plan.subagents);
    assert.deepEqual(Object.keys(agents), Object.keys(plan.subagents));
    const architect = agents["architect"]!;
    assert.ok(architect.tools!.includes(GUARDED_COMMAND_TOOL));
    assert.ok(!architect.tools!.includes("Bash"));
    assert.ok(!architect.tools!.includes("Edit") && !architect.tools!.includes("Write"));
    assert.ok(architect.disallowedTools!.includes("Edit"));
  });
});

describe("buildSdkOptions guardedCommand", () => {
  const base = (): ClaudeTurnConfig => ({
    cwd: "/w",
    env: { A: "1" },
    skillsPluginPath: "/p",
    skills: [],
    systemPrompt: "s",
    agents: {},
    mcpServers: {},
    preToolUse: [],
  });

  it("leaves the options identical, with no new keys, when unset", () => {
    const opts = buildSdkOptions(base());
    assert.equal("toolAliases" in opts, false);
    assert.deepEqual(opts.disallowedTools, [...ASYNC_DEFERRAL_TOOLS]);
  });

  it("adds the Bash alias and denies native Bash and Monitor when set", () => {
    const opts = buildSdkOptions({ ...base(), guardedCommand: true });
    assert.deepEqual(opts.toolAliases, { Bash: GUARDED_COMMAND_TOOL });
    assert.deepEqual(opts.disallowedTools, [...ASYNC_DEFERRAL_TOOLS, "Bash", "Monitor"]);
    const rest = { ...opts };
    delete rest.toolAliases;
    rest.disallowedTools = [...ASYNC_DEFERRAL_TOOLS];
    assert.deepEqual(rest, buildSdkOptions(base()), "nothing else changes");
  });
});
