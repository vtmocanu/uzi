import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { buildCodexDynamicTools } from "../src/codex/dynamic-tools.js";
import type { RunGrants } from "../src/codex/broker.js";

function grants(allowedTools: readonly string[]): RunGrants {
  return {
    role: "lead",
    phase: "implement",
    allowedTools: new Set(allowedTools),
    allowedSkills: new Set(["review"]),
    isRoot: true,
  };
}

describe("buildCodexDynamicTools: worker-owned app-server callback surface", () => {
  it("registers the deterministic granted subset and hides code-mode lifecycle callback names", () => {
    const specs = buildCodexDynamicTools(grants([
      "signal_done",
      "SubagentStop",
      "Bash",
      "spawn_agent",
      "collaborationwait_agent",
      "apply_patch",
      "Read",
      "Skill",
      "mcp__forge__get_issue",
    ]));
    assert.deepEqual(specs.map((spec) => spec.name), [
      "signal_done",
      "spawn_agent",
      "uzi_apply_patch",
      "uzi_bash",
      "uzi_forge_get_issue",
      "uzi_read",
      "uzi_skill",
    ]);
    assert.equal(specs.some((spec) => ["Bash", "apply_patch", "Read", "Skill"].includes(spec.name)), false);
    for (const spec of specs) {
      assert.equal(spec.type, "function");
      assert.match(spec.name, /^[a-zA-Z0-9_-]{1,128}$/);
      assert.equal(spec.inputSchema.type, "object");
      assert.equal(spec.name.startsWith("mcp__"), false, "pinned app-server reserved MCP names never reach the wire");
    }
  });

  it("pins the screened shell and plan schemas to their required authority-neutral fields", () => {
    const specs = buildCodexDynamicTools(grants(["Bash", "submit_plan", "checkpoint"]));
    const bash = specs.find((spec) => spec.name === "uzi_bash");
    const plan = specs.find((spec) => spec.name === "submit_plan");
    const checkpoint = specs.find((spec) => spec.name === "checkpoint");
    assert.deepEqual(bash?.inputSchema.required, ["command"]);
    assert.deepEqual(plan?.inputSchema.required, ["plan_md"]);
    assert.equal(checkpoint?.inputSchema.additionalProperties, false);
  });

  it("is byte-stable and never mutates the immutable grant", () => {
    const input = grants(["Read", "Skill", "Bash"]);
    const before = [...input.allowedTools];
    assert.equal(JSON.stringify(buildCodexDynamicTools(input)), JSON.stringify(buildCodexDynamicTools(input)));
    assert.deepEqual([...input.allowedTools], before);
  });
});

describe("buildCodexDynamicTools: report_progress milestone attribution (issue #1674)", () => {
  it("accepts milestones_agents entries of {id, agent, agent_label?}", () => {
    const spec = buildCodexDynamicTools(grants(["report_progress"])).find((s) => s.name === "report_progress");
    const props = spec?.inputSchema.properties as Record<string, { type?: string; items?: Record<string, unknown> }>;
    assert.equal(props["milestones_agents"]?.type, "array");
    const item = props["milestones_agents"]!.items!;
    assert.deepEqual(Object.keys(item["properties"] as object).sort(), ["agent", "agent_label", "id"]);
    assert.deepEqual(item["required"], ["id", "agent"]);
    assert.equal(item["additionalProperties"], false);
    assert.equal(spec?.inputSchema.additionalProperties, false, "the top-level schema stays closed");
  });
});
