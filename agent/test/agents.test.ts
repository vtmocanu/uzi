import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { assembleAgents, LEAD_NAME_RE, selectSubagents, subagentsFromTemplates } from "../src/agents.js";
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

  it("leaves tools UNSET (inherit all) when the template omits tools", () => {
    // PRD #3 wire contract: null/absent/empty tools ⇒ inherit all. The SDK
    // grants the full toolset when `tools` is unset (this is what keeps the
    // builtin coder, which ships with no tools line, write-capable).
    for (const tools of [null, [] as string[], undefined]) {
      const { subagents } = assembleAgents([{ ...coder, tools }]);
      assert.strictEqual(subagents.coder?.tools, undefined);
      // Nested spawning is still blocked regardless of the inherited toolset.
      assert.deepStrictEqual(subagents.coder?.disallowedTools, ["Agent"]);
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

describe("lead pin (PRD #18 Decision 8)", () => {
  // The server guarantees a claim never carries two lead-matching templates: the
  // reserved-name check (M6) refuses to create a global/user template whose name
  // matches LEAD_NAME_RE, so only the seeded builtin lead can exist, and
  // allocation resolution (M7) delivers at most that one. These worker-side tests
  // pin the two ends of that contract: the wire goldens never carry two leads, and
  // assembleAgents deterministically routes exactly one even if it somehow did.
  const leadCount = (names: string[]) => names.filter((n) => LEAD_NAME_RE.test(n)).length;

  for (const golden of ["claim_skills_wire.json", "claim_ci_fix_wire.json"]) {
    it(`${golden} carries at most one lead-matching agent`, () => {
      const p = join(import.meta.dirname, "..", "..", "api", "internal", "workersvc", "testdata", golden);
      const claim = JSON.parse(readFileSync(p, "utf8")) as { agents?: { name: string }[] };
      const names = (claim.agents ?? []).map((a) => a.name);
      assert.ok(leadCount(names) <= 1, `${golden} agents ${JSON.stringify(names)} carry >1 lead`);
    });
  }

  it("routes exactly one lead even if two lead-named templates are (wrongly) delivered", () => {
    // Defense in depth: the server must never send this, but if it did, the first
    // by array order becomes the lead and the rest are ordinary subagents — never
    // two leads, never a crash. `orchestrator` also matches LEAD_NAME_RE.
    const orchestrator: AgentTemplate = { name: "orchestrator", description: "d", prompt_body: "p", tools: null, model: null };
    const { subagents, leadSystemPrompt } = assembleAgents([lead, orchestrator, coder]);
    assert.strictEqual(leadSystemPrompt, lead.prompt_body, "the first lead-matching template wins the main thread");
    assert.ok("orchestrator" in subagents, "the second lead-matching template falls back to a subagent");
    assert.ok(!("lead" in subagents), "the routed lead is never also a subagent");
  });
});

describe("subagentsFromTemplates (PRD #37 repo source)", () => {
  const repoLead: AgentTemplate = { name: "lead", description: "repo lead", prompt_body: "REPO LEAD BODY" };
  const repoAuditor: AgentTemplate = { name: "auditor", description: "repo auditor", prompt_body: "audit body" };

  it("maps EVERY template to a subagent, including one named `lead`", () => {
    const subagents = subagentsFromTemplates([repoLead, repoAuditor], new Set());
    assert.deepStrictEqual(Object.keys(subagents).sort(), ["auditor", "lead"]);
    // A repo `lead` is an ordinary subagent — its body is the subagent prompt, NOT
    // hoisted to a main-thread system prompt (that is assembleAgents' job for own).
    assert.strictEqual(subagents.lead!.prompt, "REPO LEAD BODY");
    // Structural denial still applies via toDefinition.
    assert.ok(subagents.lead!.disallowedTools?.includes("Agent"));
    assert.ok(subagents.auditor!.disallowedTools?.includes("Agent"));
  });

  it("drops excluded names", () => {
    const subagents = subagentsFromTemplates([repoLead, repoAuditor], new Set(["lead"]));
    assert.deepStrictEqual(Object.keys(subagents), ["auditor"]);
  });
});

describe("selectSubagents (PRD #37)", () => {
  const repoCoder: AgentTemplate = { name: "coder", description: "repo coder", prompt_body: "REPO CODER", tools: ["Read", "WebFetch"] };
  const repoAuditor: AgentTemplate = { name: "auditor", description: "repo auditor", prompt_body: "audit" };

  it("own source returns the pre-assembled own subagents minus exclusions", () => {
    const own = assembleAgents([lead, coder, reviewer]).subagents;
    assert.deepStrictEqual(Object.keys(selectSubagents("own", own, [], [])).sort(), ["coder", "reviewer"]);
    assert.deepStrictEqual(Object.keys(selectSubagents("own", own, [], ["reviewer"])), ["coder"]);
  });

  it("repo source returns the repo roster minus exclusions, honoring declared tools", () => {
    const own = assembleAgents([lead, coder, reviewer]).subagents;
    const repo = selectSubagents("repo", own, [repoCoder, repoAuditor], []);
    assert.deepStrictEqual(Object.keys(repo).sort(), ["auditor", "coder"]);
    // The repo coder's WebFetch survives (policy reversed) and its repo body runs.
    assert.deepStrictEqual(repo.coder!.tools, ["Read", "WebFetch"]);
    assert.strictEqual(repo.coder!.prompt, "REPO CODER");
    // Exclusions apply to the repo roster too.
    assert.deepStrictEqual(Object.keys(selectSubagents("repo", own, [repoCoder, repoAuditor], ["auditor"])), ["coder"]);
  });
});

describe("shipped lead builtin", () => {
  // Guard the cross-package contract: the lead template the api ships
  // (api/internal/agenttmpl/builtins/lead.md) must carry a name this worker
  // recognizes as the lead, or it would be registered as an ordinary invokable
  // subagent and its model/prompt would never reach the main thread. Read the
  // real file so a rename on either side fails this test.
  const builtinLead = join(import.meta.dirname, "..", "..", "api", "internal", "agenttmpl", "builtins", "lead.md");

  it("names the lead so LEAD_NAME_RE routes it to the main thread", () => {
    const raw = readFileSync(builtinLead, "utf8");
    const name = raw.match(/^name:\s*(.+)$/m)?.[1]?.trim();
    assert.ok(name, "lead.md must declare a name in its frontmatter");
    assert.ok(LEAD_NAME_RE.test(name), `shipped lead name ${name} must match LEAD_NAME_RE`);
  });
});
