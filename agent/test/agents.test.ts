import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { assembleAgents, DEFAULT_READONLY_TOOLS } from "../src/agents.js";
import type { AgentTemplate } from "../src/protocol.js";

const coder: AgentTemplate = {
  name: "coder",
  description: "writes code",
  prompt_body: "You implement changes.",
  tools: ["Read", "Edit", "Write", "Bash", "Grep", "Glob"],
  model: null,
};
const reviewer: AgentTemplate = {
  name: "reviewer",
  description: "reviews code",
  prompt_body: "You review changes.",
  // Read-only: no Edit/Write (PRD #3 excludes them from this role).
  tools: ["Read", "Grep", "Glob"],
  model: "opus",
};
const lead: AgentTemplate = {
  name: "lead",
  description: "orchestrates",
  prompt_body: "You are the lead. Coordinate the work.",
  tools: null,
  model: "fable",
};

describe("assembleAgents", () => {
  it("maps every non-lead template to an invokable subagent AgentDefinition", () => {
    const { subagents } = assembleAgents([coder, reviewer]);
    assert.deepStrictEqual(Object.keys(subagents).sort(), ["coder", "reviewer"]);
    assert.strictEqual(subagents.coder?.prompt, "You implement changes.");
    assert.strictEqual(subagents.coder?.description, "writes code");
    assert.deepStrictEqual(subagents.coder?.tools, ["Read", "Edit", "Write", "Bash", "Grep", "Glob"]);
    assert.strictEqual(subagents.reviewer?.model, "opus");
  });

  it("blocks nested Agent spawning on every subagent", () => {
    const { subagents } = assembleAgents([coder, reviewer]);
    for (const name of Object.keys(subagents)) {
      assert.deepStrictEqual(subagents[name]?.disallowedTools, ["Agent"], `${name} must disallow Agent`);
    }
  });

  it("keeps read-only roles read-only via the tools allowlist (no Edit/Write)", () => {
    const { subagents } = assembleAgents([reviewer]);
    const tools = subagents.reviewer?.tools ?? [];
    assert.ok(!tools.includes("Edit"), "reviewer must not have Edit");
    assert.ok(!tools.includes("Write"), "reviewer must not have Write");
    assert.ok(tools.includes("Read"));
  });

  it("fails closed to a read-only allowlist when the template omits tools", () => {
    // Absent/empty tools must NOT inherit everything (fail-open); it defaults to
    // read-only, so a template that forgot to list Edit/Write/Bash can't write.
    for (const tools of [null, [] as string[], undefined]) {
      const { subagents } = assembleAgents([{ ...coder, tools }]);
      assert.deepStrictEqual(subagents.coder?.tools, DEFAULT_READONLY_TOOLS);
      assert.ok(!subagents.coder?.tools?.includes("Edit"));
      assert.ok(!subagents.coder?.tools?.includes("Write"));
      assert.ok(!subagents.coder?.tools?.includes("Bash"));
    }
  });

  it("routes a lead-named template to the lead system prompt, not the subagents", () => {
    const { subagents, leadSystemPrompt, leadModel } = assembleAgents([lead, coder, reviewer]);
    assert.deepStrictEqual(Object.keys(subagents).sort(), ["coder", "reviewer"]);
    assert.ok(!("lead" in subagents), "lead template must not be an invokable subagent");
    assert.strictEqual(leadSystemPrompt, "You are the lead. Coordinate the work.");
    assert.strictEqual(leadModel, "fable");
  });

  it("has no lead system prompt when no lead template is supplied", () => {
    const { leadSystemPrompt, leadModel } = assembleAgents([coder]);
    assert.strictEqual(leadSystemPrompt, undefined);
    assert.strictEqual(leadModel, undefined);
  });

  it("handles an empty template list", () => {
    const { subagents, leadSystemPrompt } = assembleAgents([]);
    assert.deepStrictEqual(subagents, {});
    assert.strictEqual(leadSystemPrompt, undefined);
  });
});
