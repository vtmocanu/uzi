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


describe("Codex runtime discovery", () => {
  for (const isRoot of [true, false]) {
    it(`lists only granted skills for ${isRoot ? "root" : "child"}`, () => {
      const input = { ...grants(["Skill"]), isRoot, allowedSkills: new Set(["zebra", "review"]) };
      const spec = buildCodexDynamicTools(input).find(s => s.name === "uzi_skill")!;
      assert.match(spec.description, /Granted skills: review, zebra\./);
      const empty = buildCodexDynamicTools({ ...input, allowedSkills: new Set<string>() })[0]!;
      assert.match(empty.description, /No skills are granted/);
      assert.deepEqual([...input.allowedSkills], ["zebra", "review"], "advertising never changes grants");
    });
  }
  it("advertises the actual shell and POSIX syntax without recommending script execution", () => {
    const spec = buildCodexDynamicTools(grants(["Bash"]))[0]!;
    assert.match(spec.description, /\/bin\/sh -c/);
    assert.match(spec.description, /write POSIX sh syntax/);
    assert.ok(!spec.description.includes("bash <file>") && !spec.description.includes("script"));
  });
});

describe("issue3089 owner request dynamic schema", () => {
  for (const isRoot of [true, false]) for (const ownerDecisionRequested of [true, false]) {
    it(`advertises only eligible root request ${isRoot}/${ownerDecisionRequested}`, () => {
      const spec = buildCodexDynamicTools({ ...grants(["signal_done"]), isRoot, ownerDecisionRequested })[0]!;
      const props = spec.inputSchema.properties as Record<string, Record<string, unknown>>;
      const request = props.owner_decision_requested;
      assert.equal(Boolean(request), isRoot && ownerDecisionRequested);
      if (request) {
        assert.equal(request.additionalProperties, false);
        assert.deepEqual(request.required, ["reason"]);
      }
    });
  }
});

describe("m1 non-closing dynamic schema", () => {
  it("requires a string reason structurally without a raw maxLength cap", () => {
    const spec = buildCodexDynamicTools(grants(["signal_done"])).find(s => s.name === "signal_done")!;
    const props = spec.inputSchema.properties as Record<string, Record<string, unknown>>;
    const declaration = props["non_closing"]!;
    assert.equal(declaration.type, "object");
    assert.deepEqual(declaration.required, ["reason"]);
    assert.equal(declaration.additionalProperties, false);
    assert.deepEqual(declaration.properties, { reason: { type: "string" } });
    assert.match(String(declaration.description), /Issue runs only/);
    assert.ok(!(spec.inputSchema.required as string[] | undefined)?.includes("non_closing"));
  });
});

describe("#3360 report_only dynamic schema", () => {
  it("describes report_only as no-code-change and points to non_closing", () => {
    const spec = buildCodexDynamicTools(grants(["signal_done"])).find(s => s.name === "signal_done")!;
    const props = spec.inputSchema.properties as Record<string, Record<string, unknown>>;
    const declaration = props["report_only"]!;
    assert.equal(declaration.type, "boolean");
    assert.match(String(declaration.description), /no code change/i);
    assert.match(String(declaration.description), /non_closing/);
    assert.match(String(declaration.description), /^Issue runs only:/);
    assert.match(String(declaration.description), /already published to a checkpoint, the committed branch is delivered as a non-closing merge request/);
  });
});

describe("#2986 root-only long command schema", () => {
  for (const isRoot of [true, false]) {
    it(`advertises long_running only with root grant ${isRoot}`, () => {
      const spec = buildCodexDynamicTools({ ...grants(["Bash"]), isRoot }).find(s => s.name === "uzi_bash")!;
      const props = spec.inputSchema.properties as Record<string, { type: string }>;
      assert.equal(props.long_running?.type, isRoot ? "boolean" : undefined);
      assert.deepEqual(spec.inputSchema.required, ["command"]);
      assert.equal(spec.inputSchema.additionalProperties, false);
    });
  }
});

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

  it("advertises optional bounded Read ranges and both path aliases", () => {
    const spec = buildCodexDynamicTools(grants(["Read"])).find(s => s.name === "uzi_read")!;
    assert.deepEqual(spec.inputSchema.properties, {
      path: { type: "string" }, file_path: { type: "string" },
      offset: { type: "integer", minimum: 1, maximum: Number.MAX_SAFE_INTEGER, default: 1 },
      limit: { type: "integer", minimum: 1, maximum: 2000, default: 200 },
    });
    assert.deepEqual(spec.inputSchema.anyOf, [{ required: ["path"] }, { required: ["file_path"] }]);
    assert.equal(spec.inputSchema.required, undefined);
    assert.match(spec.description, /explicit offset or limit allows one fallback after a helper oversize refusal/);
    assert.match(spec.description, /8 MiB.*64 KiB.*incomplete:true.*scan_budget/);
    assert.match(spec.description, /not a snapshot of concurrent changes/);
    assert.equal(spec.inputSchema.additionalProperties, false);
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

it("U3 D9 disposition wire tool has a closed authority-neutral schema", () => {
  assert.equal(buildCodexDynamicTools(grants([])).some(t => t.name === "report_cross_check_dispositions"), false);
  const spec = buildCodexDynamicTools(grants(["mcp__uzi__report_cross_check_dispositions"])).find(t => t.name === "report_cross_check_dispositions")!;
  assert.ok(spec);
  assert.equal(spec.inputSchema.additionalProperties, false);
  assert.deepEqual(spec.inputSchema.required, ["dispositions"]);
  assert.deepEqual(Object.keys(spec.inputSchema.properties as object), ["dispositions"]);
  const batch = (spec.inputSchema.properties as Record<string, { items: Record<string, unknown> }>).dispositions!;
  assert.equal(batch.items.additionalProperties, false);
  assert.deepEqual(Object.keys(batch.items.properties as object).sort(), ["disposition", "finding_id", "reason"]);
  assert.deepEqual(batch.items.required, ["finding_id", "disposition", "reason"]);
});
