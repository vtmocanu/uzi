import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  applySubagentModelOverride,
  assembleAgents,
  LEAD_NAME_RE,
  planTurnSubagents,
  selectSubagents,
  subagentsFromTemplates,
  subagentCanWrite,
  subagentWriteCapabilities,
} from "../src/agents.js";
import { reportIncidentalIssueToolName, FINDINGS_SERVER_NAME } from "../src/findings-tools.js";
import { FORGE_SERVER_NAME } from "../src/forge-tools.js";
import { MEMORY_SERVER_NAME } from "../src/memory-tools.js";
import { SIGNAL_SERVER_NAME } from "../src/signals.js";
import { FINDINGS_NUDGE_APPEND } from "../src/prompt.js";
import type { AgentTemplate } from "../src/protocol.js";

// PRD #457: toDefinition now grants the incidental-findings tool to every non-empty
// allowlist and appends the discovery nudge to every subagent prompt. Reference the
// helper rather than hardcoding the string.
const FINDINGS_TOOL = reportIncidentalIssueToolName();
const withNudge = (body: string) => `${body}\n\n${FINDINGS_NUDGE_APPEND}`;

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
    assert.strictEqual(subagents.coder?.prompt, withNudge("You implement changes."));
    assert.strictEqual(subagents.coder?.description, "writes code");
    assert.deepStrictEqual(subagents.coder?.tools, ["Read", "Edit", "Write", "Bash", "Grep", "Glob", FINDINGS_TOOL]);
    assert.strictEqual(subagents.reviewer?.model, "opus");
  });

  it("PRD #457: grants the findings tool to a non-empty allowlist and appends the nudge (own/builtin path)", () => {
    const { subagents } = assembleAgents([coder, reviewer]);
    // Capability: the tool is appended once, at the end, on a restricted allowlist.
    assert.ok(subagents.reviewer?.tools?.includes(FINDINGS_TOOL), "reviewer gains the findings tool");
    assert.strictEqual(
      subagents.reviewer?.tools?.filter((t) => t === FINDINGS_TOOL).length,
      1,
      "no duplicate findings tool",
    );
    // Discovery: the nudge is appended to the subagent prompt.
    assert.ok(
      subagents.reviewer?.prompt?.includes(FINDINGS_TOOL),
      "the nudge naming the findings tool is in the subagent prompt",
    );
    assert.strictEqual(subagents.reviewer?.prompt, withNudge("You review changes."));
  });

  it("blocks nested Agent spawning, the workflow-signal MCP server, and the memory MCP server on every subagent", () => {
    // PRD #43 M2 + PRD #90: disallowedTools denies nested Agent spawning, the run's
    // signal server (mcp__uzi → submit_plan/signal_done), AND the memory server
    // (mcp__memory → save_memory, lead-only) for every subagent, builtin or custom.
    // Each `mcp__<server>` is a server-level MCP spec removing every tool that
    // in-process server exposes (sdk.d.ts:48).
    const { subagents } = assembleAgents([coder, reviewer]);
    for (const name of Object.keys(subagents)) {
      assert.deepStrictEqual(
        subagents[name]?.disallowedTools,
        ["Agent", "mcp__uzi", "mcp__memory"],
        `${name} must disallow Agent, the uzi signal server, and the memory server`,
      );
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
      // Nested spawning, the signal server, AND the memory server stay blocked even
      // when the subagent inherits all tools (the coder path) — the denial is not a
      // function of the allowlist (PRD #43 M2 + PRD #90).
      assert.deepStrictEqual(subagents.coder?.disallowedTools, ["Agent", "mcp__uzi", "mcp__memory"]);
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
    assert.strictEqual(subagents.lead!.prompt, withNudge("REPO LEAD BODY"));
    // Structural denial still applies via toDefinition — including for
    // repo-sourced (custom) subagents: no nested Agent, no uzi signal server, no memory server.
    for (const name of ["lead", "auditor"]) {
      assert.deepStrictEqual(subagents[name]!.disallowedTools, ["Agent", "mcp__uzi", "mcp__memory"]);
    }
  });

  it("drops excluded names", () => {
    const subagents = subagentsFromTemplates([repoLead, repoAuditor], new Set(["lead"]));
    assert.deepStrictEqual(Object.keys(subagents), ["auditor"]);
  });

  it("PRD #457: grants the findings tool to a restricted repo allowlist and appends the nudge (repo path)", () => {
    const repoReviewer: AgentTemplate = {
      name: "reviewer",
      description: "repo reviewer",
      prompt_body: "REVIEW BODY",
      tools: ["Read", "Grep", "Glob"],
    };
    const subagents = subagentsFromTemplates([repoReviewer], new Set());
    assert.deepStrictEqual(subagents.reviewer!.tools, ["Read", "Grep", "Glob", FINDINGS_TOOL]);
    assert.strictEqual(subagents.reviewer!.prompt, withNudge("REVIEW BODY"));
    // Inherit-all (absent tools) is untouched — the tool is already available.
    const inherit = subagentsFromTemplates([repoAuditor], new Set());
    assert.strictEqual(inherit.auditor!.tools, undefined, "inherit-all is not materialized into an allowlist");
    assert.strictEqual(inherit.auditor!.prompt, withNudge("audit body"), "inherit-all subagent's prompt still carries the nudge");
  });
});

describe("issue #581: in-process MCP server wiring for subagents", () => {
  // toDefinition derives def.mcpServers (SDK string-reference form) from the RESOLVED
  // tools allowlist: for the servers ["forge","findings"] in that order, it names the
  // server when any allowlist entry starts with `mcp__${server}__`. It runs AFTER the
  // findings-tool append, so any non-empty allowlist already carries the findings tool
  // and therefore always wires the findings server. This is the ONLY thing that lets an
  // allowlisted subagent reach an in-process MCP server registered on options.mcpServers.

  // Parse a builtin's frontmatter `tools:` line exactly as the spec prescribes: the line
  // starting with `tools:`, minus the prefix, split on `,`, each trimmed, empties dropped.
  const parseToolsLine = (raw: string): string[] => {
    const line = raw.split("\n").find((l) => l.startsWith("tools:"));
    assert.ok(line, "builtin must declare a tools: line in its frontmatter");
    return line!
      .slice("tools:".length)
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t.length > 0);
  };

  const factCheckerPath = join(
    import.meta.dirname,
    "..",
    "..",
    "api",
    "internal",
    "agenttmpl",
    "builtins",
    "fact-checker.md",
  );
  const factCheckerTools = parseToolsLine(readFileSync(factCheckerPath, "utf8"));
  const factChecker: AgentTemplate = {
    name: "fact-checker",
    description: "verifies claims",
    prompt_body: "You verify claims.",
    tools: factCheckerTools,
    model: null,
  };

  it("the real fact-checker builtin resolves to mcpServers ['forge','findings'] in deterministic order", () => {
    // Anchor to the shipped builtin so the test breaks if it ever drops the forge grant:
    // the fact-checker is the whole reason this wiring exists (issue #581).
    assert.ok(
      factCheckerTools.some((t) => t.startsWith("mcp__forge__")),
      "the shipped fact-checker must still grant at least one mcp__forge__* tool",
    );
    const { subagents } = assembleAgents([factChecker]);
    // forge is named because the allowlist has mcp__forge__* tools; findings is named
    // because toDefinition appended the findings tool before deriving this list. The
    // order is the loop's fixed [forge, findings], not the allowlist's order.
    assert.deepStrictEqual(subagents["fact-checker"]?.mcpServers, [FORGE_SERVER_NAME, FINDINGS_SERVER_NAME]);
    assert.deepStrictEqual(subagents["fact-checker"]?.mcpServers, ["forge", "findings"]);
  });

  it("a read-only role granted NO forge tool wires only the findings server", () => {
    // reviewer declares Read/Grep/Glob — no mcp__forge__* — so forge is NOT named, but
    // the findings tool appended to every non-empty allowlist wires the findings server.
    const { subagents } = assembleAgents([reviewer]);
    assert.deepStrictEqual(subagents.reviewer?.mcpServers, [FINDINGS_SERVER_NAME]);
  });

  it("an inherit-all subagent (null/[]/undefined tools) leaves mcpServers UNSET", () => {
    // Inherit-all subagents get no allowlist, so toDefinition never derives mcpServers —
    // the top-level options.mcpServers already reaches them via the lead session path.
    for (const tools of [null, [] as string[], undefined]) {
      const { subagents } = assembleAgents([{ ...coder, tools }]);
      assert.strictEqual(subagents.coder?.mcpServers, undefined);
    }
  });

  it("never wires the memory or signal servers, even when a template names their tools", () => {
    // memory/signal are server-DENIED to every subagent (MEMORY_SERVER_DENY /
    // SIGNAL_SERVER_DENY); the wiring loop is deliberately limited to the hardcoded
    // [forge, findings] set — it does NOT naively name every `mcp__<server>__` prefix in
    // the allowlist. A fixture that explicitly lists mcp__memory__* / mcp__uzi__* tools is
    // what makes this a real guard: widening the loop to derive from those prefixes would
    // redden it. (Those tools stay non-callable via the server-level deny regardless.)
    const namesDeniedServers: AgentTemplate = {
      name: "greedy",
      description: "names denied servers in its allowlist",
      prompt_body: "greedy",
      tools: ["Read", "mcp__memory__save_memory", "mcp__uzi__submit_plan", "mcp__forge__get_issue"],
    };
    const { subagents } = assembleAgents([coder, reviewer, factChecker, namesDeniedServers]);
    // The greedy fixture names memory + signal tools AND a forge tool: forge is wired,
    // findings is wired (appended tool), memory/signal are NOT — despite being named.
    assert.deepStrictEqual(subagents.greedy?.mcpServers, [FORGE_SERVER_NAME, FINDINGS_SERVER_NAME]);
    for (const [name, def] of Object.entries(subagents)) {
      if (!def.mcpServers) continue;
      assert.ok(!def.mcpServers.includes(MEMORY_SERVER_NAME), `${name} must not wire the memory server`);
      assert.ok(!def.mcpServers.includes(SIGNAL_SERVER_NAME), `${name} must not wire the signal server`);
      // Positively: the only servers ever wired are forge and findings.
      for (const s of def.mcpServers) {
        assert.ok(
          s === FORGE_SERVER_NAME || s === FINDINGS_SERVER_NAME,
          `${name} wired an unexpected server ${s}`,
        );
      }
    }
  });

  it("the repo-source path derives the same wiring (also flows through toDefinition)", () => {
    // Acknowledged, plan-approved: subagentsFromTemplates (repo path) also derives
    // mcpServers, so a repo agent naming a mcp__forge__* tool reaches the forge server.
    const factCheckerRepoTemplate: AgentTemplate = {
      name: "fact-checker",
      description: "repo fact-checker",
      prompt_body: "REPO FACT CHECKER",
      tools: ["Read", "Grep", "mcp__forge__get_issue"],
    };
    const subagents = subagentsFromTemplates([factCheckerRepoTemplate], new Set());
    assert.ok(
      subagents["fact-checker"]?.mcpServers?.includes(FORGE_SERVER_NAME),
      "a repo agent naming a mcp__forge__* tool wires the forge server",
    );
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
    // PRD #457: a restricted repo allowlist also gains the findings tool + nudge.
    assert.deepStrictEqual(repo.coder!.tools, ["Read", "WebFetch", FINDINGS_TOOL]);
    assert.strictEqual(repo.coder!.prompt, withNudge("REPO CODER"));
    // Exclusions apply to the repo roster too.
    assert.deepStrictEqual(Object.keys(selectSubagents("repo", own, [repoCoder, repoAuditor], ["auditor"])), ["coder"]);
  });
});

describe("delivered skills reach repo-sourced subagents (PRD #72 M1)", () => {
  // Own templates carry per-template ALLOCATIONS; repo templates never do —
  // detectRepoAgents builds {name, description, prompt_body} and nothing else, so
  // `.skills` is absent by construction. That is the gap M1 closes.
  const ownCoder: AgentTemplate = { ...coder, skills: ["cicd"] };
  const ownReviewer: AgentTemplate = { ...reviewer, skills: ["kb"] };
  const repoCoder: AgentTemplate = { name: "coder", description: "repo coder", prompt_body: "REPO CODER" };
  const repoAuditor: AgentTemplate = { name: "auditor", description: "repo auditor", prompt_body: "audit" };
  // The materialized run union: two delivered survivors plus one repo-borne one.
  const survivors = new Set(["cicd", "kb", "deploy-notes"]);
  const repoSurvivors = ["deploy-notes"];

  it("enables EVERY surviving delivered skill on EVERY repo subagent", () => {
    const subagents = subagentsFromTemplates([repoCoder, repoAuditor], new Set(), survivors, repoSurvivors);
    for (const name of ["coder", "auditor"]) {
      assert.deepStrictEqual(
        subagents[name]!.skills,
        ["uzi:cicd", "uzi:kb", "uzi:deploy-notes"],
        `${name} must receive the run's whole surviving set, not a per-template slice`,
      );
    }
  });

  it("takes the repo survivors from the survivor set, not from repoSkillNames", () => {
    // The PRECONDITION: availableSkills already CONTAINS the repo survivors, so
    // passing them again must change nothing. Reintroducing the union would make
    // this pass too — what it pins is that the repo half is not the SOURCE of
    // deploy-notes here, which is why the fallback case below is the one that
    // exercises repoSkillNames at all.
    const withRepoNames = subagentsFromTemplates([repoAuditor], new Set(), survivors, repoSurvivors).auditor!.skills;
    const withoutRepoNames = subagentsFromTemplates([repoAuditor], new Set(), survivors).auditor!.skills;
    assert.deepStrictEqual(withRepoNames, ["uzi:cicd", "uzi:kb", "uzi:deploy-notes"]);
    assert.deepStrictEqual(withoutRepoNames, withRepoNames, "the survivor set is the whole answer");
  });

  it("never lists a skill absent from the survivor set (dropped by cap or collision)", () => {
    // `kb` is allocated + repo-requested but is NOT in the survivor set (the worker
    // dropped it), so it must be absent from the repo roster AND the own roster.
    const dropped = new Set(["cicd"]);
    const repo = subagentsFromTemplates([repoCoder, repoAuditor], new Set(), dropped, []);
    for (const name of ["coder", "auditor"]) {
      assert.deepStrictEqual(repo[name]!.skills, ["uzi:cicd"], `${name} must not list the dropped kb`);
    }
    const own = assembleAgents([lead, ownCoder, ownReviewer], dropped, []).subagents;
    assert.deepStrictEqual(own.coder!.skills, ["uzi:cicd"]);
    assert.deepStrictEqual(own.reviewer!.skills, [], "the dropped skill is gone from the own path too");
  });

  it("leaves the OWN path scoped per template — allocations stay the scoping surface", () => {
    const own = assembleAgents([lead, ownCoder, ownReviewer], survivors, repoSurvivors).subagents;
    // Each own subagent gets ITS allocation plus the repo-borne skill — never the
    // other subagent's allocation (that would delete the admin's scoping control,
    // PRD #72 Decision 6 "Own-template runs are unchanged").
    assert.deepStrictEqual(own.coder!.skills, ["uzi:cicd", "uzi:deploy-notes"]);
    assert.deepStrictEqual(own.reviewer!.skills, ["uzi:kb", "uzi:deploy-notes"]);
    // And selectSubagents("own", …) hands those definitions through untouched.
    const selected = selectSubagents("own", own, [repoCoder, repoAuditor], [], survivors, repoSurvivors);
    assert.deepStrictEqual(selected.coder!.skills, ["uzi:cicd", "uzi:deploy-notes"]);
    assert.deepStrictEqual(selected.reviewer!.skills, ["uzi:kb", "uzi:deploy-notes"]);
  });

  it("selectSubagents routes the repo source through the run-wide rule", () => {
    const own = assembleAgents([lead, ownCoder, ownReviewer], survivors, repoSurvivors).subagents;
    const repo = selectSubagents("repo", own, [repoCoder, repoAuditor], [], survivors, repoSurvivors);
    assert.deepStrictEqual(repo.coder!.skills, ["uzi:cicd", "uzi:kb", "uzi:deploy-notes"]);
    assert.deepStrictEqual(repo.auditor!.skills, ["uzi:cicd", "uzi:kb", "uzi:deploy-notes"]);
  });

  it("degrades to the repo-borne names when no survivor set is supplied", () => {
    // The `: repoSkillNames` fallback is now the ONLY way to reach pre-#72
    // behaviour, and no other assertion enters that branch. The previous version of
    // this case passed no repo names either, so it asserted [] out of empty in and
    // stayed green under every mutation — it tested nothing.
    const subagents = subagentsFromTemplates([repoCoder], new Set(), undefined, ["deploy-notes"]);
    assert.deepStrictEqual(subagents.coder!.skills, ["uzi:deploy-notes"]);

    // And with neither, the empty case still holds (no crash, no invented names).
    assert.deepStrictEqual(subagentsFromTemplates([repoCoder], new Set()).coder!.skills, []);
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

describe("planTurnSubagents (#203)", () => {
  const WRITE_TOOLS = ["Edit", "Write", "MultiEdit", "NotebookEdit"];
  const architect: AgentTemplate = {
    name: "architect",
    description: "designs",
    prompt_body: "You design.",
    tools: ["Bash", "Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "WebFetch"],
    model: null,
  };
  const inheritCoder: AgentTemplate = {
    name: "coder",
    description: "implements",
    prompt_body: "You implement.",
    tools: null,
    model: null,
  };

  // A template whose declared allowlist is ONLY write tools. Reachable through the
  // documented admin surface — validateTemplateFields enforces no tool policy — and
  // the one input for which filtering alone would emit `tools: []`.
  const writeOnly: AgentTemplate = {
    name: "penman",
    description: "writes",
    prompt_body: "You write.",
    tools: ["Edit", "Write"],
    model: null,
  };

  it("does BOTH operations unconditionally: filters `tools` where present AND unions into `disallowedTools`", () => {
    // Doing only one would leave the run's safety resting on a `disallowedTools`-vs-
    // `tools` precedence the SDK does not document and no test here can settle.
    const { subagents } = assembleAgents([architect, reviewer]);
    const { subagents: planned } = planTurnSubagents(subagents);

    // PRD #457: the non-write findings tool is granted by toDefinition and survives the
    // write-strip, so it rides the plan-turn allowlist alongside the surviving reads.
    assert.deepStrictEqual(planned.architect?.tools, ["Bash", "Read", "WebFetch", FINDINGS_TOOL]);
    for (const t of WRITE_TOOLS) {
      assert.ok(planned.architect?.disallowedTools?.includes(t), `${t} denied`);
    }
    // The structural denials are preserved and stay FIRST — appended-to, not replaced.
    assert.deepStrictEqual(planned.architect?.disallowedTools, [
      "Agent",
      "mcp__uzi",
      "mcp__memory",
      ...WRITE_TOOLS,
    ]);
    // A role declaring no write tool keeps its allowlist verbatim (plus the findings
    // tool) and still gains the denials (unconditional means unconditional).
    assert.deepStrictEqual(planned.reviewer?.tools, ["Read", "Grep", "Glob", FINDINGS_TOOL]);
    for (const t of WRITE_TOOLS) assert.ok(planned.reviewer?.disallowedTools?.includes(t));
  });

  it("leaves an absent `tools` absent — inherit-all survives the transform", () => {
    const { subagents } = assembleAgents([inheritCoder]);
    const { subagents: planned } = planTurnSubagents(subagents);
    assert.strictEqual(planned.coder?.tools, undefined, "inherit-all must not be materialized into an allowlist");
    for (const t of WRITE_TOOLS) assert.ok(planned.coder?.disallowedTools?.includes(t), `${t} denied`);
  });

  it("DROPS an agent whose allowlist is only write tools — it never emits `tools: []`", () => {
    // `[]` would re-open the very precedence question this function exists to close:
    // the repo reads an empty list as INHERIT ALL in three places, so emitting one
    // would either DISCARD the operator's restriction or grant nothing, decided
    // inside the compiled binary. Dropping is the only answer that neither widens
    // the grant nor invents capability the operator excluded.
    const { subagents } = assembleAgents([architect, writeOnly, reviewer]);
    const { subagents: planned, dropped } = planTurnSubagents(subagents);

    assert.deepStrictEqual(dropped, ["penman"]);
    assert.deepStrictEqual(Object.keys(planned).sort(), ["architect", "reviewer"]);
    assert.ok(!("penman" in planned), "the dropped agent is absent from the map, not present-and-empty");
    // The negative that actually matters, stated positively: no surviving entry
    // carries an empty allowlist. A `tools: []` anywhere in this map is the defect.
    for (const [name, def] of Object.entries(planned)) {
      assert.notDeepStrictEqual(def.tools, [], `${name} must not carry an empty allowlist`);
    }
    // The agents that keep a non-write tool are unaffected by the drop.
    assert.deepStrictEqual(planned.architect?.tools, ["Bash", "Read", "WebFetch", FINDINGS_TOOL]);
  });

  it("reports every dropped name, in input order, and drops the whole roster when every agent is write-only", () => {
    const second: AgentTemplate = { ...writeOnly, name: "typist", tools: ["MultiEdit", "NotebookEdit"] };
    const { subagents } = assembleAgents([writeOnly, second]);
    const { subagents: planned, dropped } = planTurnSubagents(subagents);
    assert.deepStrictEqual(dropped, ["penman", "typist"], "input order, so the report is stable");
    assert.deepStrictEqual(planned, {}, "an all-write roster leaves the plan turn with no subagents");
  });

  it("does NOT drop an agent that keeps even one non-write tool", () => {
    // The boundary: one surviving tool is the difference between a narrowed agent
    // and no agent, so it is pinned rather than left to the filter's shape.
    // PRD #457 R1: after the grant + write-strip `barely` is `["Read", findingsTool]`
    // (a real read tool survives besides the findings tool), so it must NOT drop.
    const barely: AgentTemplate = { ...writeOnly, name: "barely", tools: ["Edit", "Write", "Read"] };
    const { subagents } = assembleAgents([barely]);
    const { subagents: planned, dropped } = planTurnSubagents(subagents);
    assert.deepStrictEqual(dropped, []);
    assert.deepStrictEqual(planned.barely?.tools, ["Read", FINDINGS_TOOL]);
  });

  it("PRD #457 R1: a write-only allowlist STILL drops even though the grant adds the (non-write) findings tool", () => {
    // The naive emptiness test would see `kept = [findingsTool]` (non-empty) after the
    // grant and WRONGLY keep the agent; the meaningful-tool test excludes the findings
    // tool so a write-only agent drops exactly as before the grant landed.
    const { subagents } = assembleAgents([writeOnly, reviewer]);
    // Precondition: the grant really did add the findings tool to the write-only agent.
    assert.ok(subagents.penman?.tools?.includes(FINDINGS_TOOL), "write-only agent gained the findings tool");
    const { subagents: planned, dropped } = planTurnSubagents(subagents);
    assert.deepStrictEqual(dropped, ["penman"], "write-only agent drops despite the findings-tool grant");
    assert.ok(!("penman" in planned), "the dropped write-only agent is absent from the plan roster");
    // And the read-only agent RETAINS the findings tool through the plan turn.
    assert.ok(planned.reviewer?.tools?.includes(FINDINGS_TOOL), "read-only agent keeps the findings tool on the plan turn");
  });

  it("BUILDS NEW OBJECTS: the input map, its definitions and their arrays are untouched", () => {
    // The trap this pins. `selectSubagents`'s own-source path copies REFERENCES
    // (`out[name] = def`), so a transform that mutated `def.tools` in place would
    // leak into the IMPLEMENT map and strip the write tools for the whole run —
    // breaking `coder`, the one role whose entire job is writing. Nothing about the
    // plan turn's own assertions would catch that; only this does.
    const { subagents } = assembleAgents([architect, inheritCoder, reviewer]);
    const before = structuredClone(subagents);
    const beforeDefs = Object.fromEntries(Object.entries(subagents).map(([k, v]) => [k, v]));

    const { subagents: planned } = planTurnSubagents(subagents);

    assert.deepStrictEqual(subagents, before, "the input map is deep-equal to its pre-call snapshot");
    for (const name of Object.keys(subagents)) {
      assert.deepStrictEqual(subagents[name]?.tools, before[name]?.tools, `${name}.tools unchanged`);
      assert.deepStrictEqual(
        subagents[name]?.disallowedTools,
        before[name]?.disallowedTools,
        `${name}.disallowedTools unchanged`,
      );
      // Identity, not just value: a fresh object and fresh arrays, so a LATER mutation
      // of the plan map cannot reach the implement map either.
      assert.notStrictEqual(planned[name], beforeDefs[name], `${name} is a new object`);
      assert.notStrictEqual(
        planned[name]?.disallowedTools,
        subagents[name]?.disallowedTools,
        `${name}.disallowedTools is a new array`,
      );
      if (subagents[name]?.tools) {
        assert.notStrictEqual(planned[name]?.tools, subagents[name]?.tools, `${name}.tools is a new array`);
      }
    }
  });

  it("carries every other AgentDefinition field through unchanged", () => {
    // The `model` assertion covers BOTH arms by construction, which is why the two
    // fixtures differ: `reviewer` declares `model: "opus"` (so a transform that
    // dropped the key reddens) and `architect` declares none (so one that INVENTED
    // a model reddens). Neither arm is vacuous; do not "fix" this by giving both
    // fixtures a model, which would retire the second arm.
    const { subagents } = assembleAgents([architect, reviewer]);
    const { subagents: planned } = planTurnSubagents(subagents);
    assert.strictEqual(subagents.reviewer?.model, "opus", "fixture guard: the non-empty model arm is real");
    assert.strictEqual(subagents.architect?.model, undefined, "fixture guard: the absent-model arm is real");
    for (const name of Object.keys(subagents)) {
      assert.strictEqual(planned[name]?.prompt, subagents[name]?.prompt);
      assert.strictEqual(planned[name]?.description, subagents[name]?.description);
      assert.strictEqual(planned[name]?.model, subagents[name]?.model);
      assert.deepStrictEqual(planned[name]?.skills, subagents[name]?.skills);
    }
  });

  it("is a no-op on an empty roster", () => {
    assert.deepStrictEqual(planTurnSubagents({}), { subagents: {}, dropped: [] });
  });
});

describe("subagentCanWrite (PRD #266 M1)", () => {
  const def = (tools: string[] | undefined) =>
    ({ description: "d", prompt: "p", ...(tools ? { tools } : {}) }) as const;

  it("treats an ABSENT tools list as inherit-all → can write", () => {
    assert.strictEqual(subagentCanWrite(def(undefined)), true);
  });

  it("treats an EMPTY tools list as inherit-all → can write", () => {
    assert.strictEqual(subagentCanWrite(def([])), true);
  });

  it("is read-only when a declared allowlist names no write tool", () => {
    assert.strictEqual(subagentCanWrite(def(["Read", "Grep"])), false);
  });

  it("can write when a declared allowlist intersects WRITE_PATH_TOOLS", () => {
    assert.strictEqual(subagentCanWrite(def(["Bash", "Edit"])), true);
  });

  it("counts NotebookEdit-only as write-capable (every write tool, not just Edit/Write)", () => {
    assert.strictEqual(subagentCanWrite(def(["NotebookEdit"])), true);
    assert.strictEqual(subagentCanWrite(def(["MultiEdit"])), true);
  });
});

describe("subagentWriteCapabilities (PRD #266 M1)", () => {
  it("maps every subagent name to its declared write capability", () => {
    // coder inherits all (tools:null) → can write; reviewer declares a read-only
    // allowlist → read-only.
    const { subagents } = assembleAgents([coder, reviewer]);
    assert.deepStrictEqual(subagentWriteCapabilities(subagents), {
      coder: true,
      reviewer: false,
    });
  });

  it("DERIVES capability from the PRE-STRIP defs — the plan turn's stripped map would falsely show coder read-only", () => {
    // The exact bug PRD #266 fixes. `coder` inherits all tools (tools:null) so it CAN
    // write; planTurnSubagents subtracts the write tools for the plan wave, so reading
    // capability off THAT map would report coder as read-only. Capability must come
    // from `assembled.subagents` (pre-strip), where it still reads as write-capable.
    const inheritCoder: AgentTemplate = {
      name: "coder",
      description: "implements",
      prompt_body: "You implement.",
      tools: null,
      model: null,
    };
    const { subagents } = assembleAgents([inheritCoder]);
    const { subagents: planned } = planTurnSubagents(subagents);

    assert.strictEqual(
      subagentWriteCapabilities(subagents).coder,
      true,
      "pre-strip map: coder is write-capable",
    );
    // Sanity: the plan-turn map is where the false negative would come from — coder
    // there carries the write tools in disallowedTools, and subagentCanWrite reads
    // only `tools` (still absent → inherit-all → true), but the roster line MUST be
    // fed the pre-strip map, which this milestone's executor does.
    assert.strictEqual(planned.coder?.tools, undefined, "plan turn left inherit-all intact");
  });
});

describe("applySubagentModelOverride (PRD #305 M4)", () => {
  // Decision 7: calibrate on a PINNED subagent. `reviewer` pins "opus"; the run
  // model is "fable". An unpinned subagent already inherits the run model, so a
  // test on one would pass with or without the flag and prove nothing.
  it("own roster: overwrites every subagent's model with the run model, pin included", () => {
    const { subagents } = assembleAgents([lead, coder, reviewer]);
    // Precondition: reviewer really does carry its own pin before the override.
    assert.strictEqual(subagents.reviewer?.model, "opus", "reviewer pins opus pre-override");

    applySubagentModelOverride(subagents, "fable");

    assert.strictEqual(subagents.reviewer?.model, "fable", "pinned reviewer overridden to run model");
    for (const [name, def] of Object.entries(subagents)) {
      assert.strictEqual(def.model, "fable", `${name} carries the run model`);
    }
  });

  it("own roster: WITHOUT the override the pin is preserved (byte-identical control)", () => {
    const { subagents } = assembleAgents([lead, coder, reviewer]);
    // No call to applySubagentModelOverride: today's behavior, pin wins.
    assert.strictEqual(subagents.reviewer?.model, "opus", "pin preserved when flag is off");
  });

  it("repo roster: overwrites a repo template's pin with the run model", () => {
    const repoCoder: AgentTemplate = {
      name: "coder",
      description: "repo coder",
      prompt_body: "REPO CODER",
    };
    const repoAuditor: AgentTemplate = {
      name: "auditor",
      description: "repo auditor",
      prompt_body: "audit",
      model: "opus",
    };
    const subagents = subagentsFromTemplates([repoCoder, repoAuditor], new Set());
    assert.strictEqual(subagents.auditor?.model, "opus", "repo auditor pins opus pre-override");

    applySubagentModelOverride(subagents, "fable");

    for (const [name, def] of Object.entries(subagents)) {
      assert.strictEqual(def.model, "fable", `repo ${name} carries the run model`);
    }
  });

  it("repo roster: WITHOUT the override the pin is preserved", () => {
    const repoAuditor: AgentTemplate = {
      name: "auditor",
      description: "repo auditor",
      prompt_body: "audit",
      model: "opus",
    };
    const subagents = subagentsFromTemplates([repoAuditor], new Set());
    assert.strictEqual(subagents.auditor?.model, "opus", "repo pin preserved when flag is off");
  });
});
