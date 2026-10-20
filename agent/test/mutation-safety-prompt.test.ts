import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { assembleAgents, planTurnSubagents, subagentsFromTemplates } from "../src/agents.js";
import { renderCodexRun } from "../src/codex/render.js";
import { buildLeadSystemPrompt } from "../src/prompt.js";
import type { HarnessAgent } from "../src/harness.js";
import type { AgentTemplate } from "../src/protocol.js";

// #3118: a coder's interrupted mutation survived in a recovery snapshot. The
// worker rule must not depend on a role body, roster source or harness. These
// tests certify prompt delivery, not model compliance or memory containment.
function assertMutationSafety(prompt: string): void {
  const flat = prompt.replace(/\s+/g, " ");
  for (const rule of [
    /deliberate mutation checks only in a disposable scratch export/,
    /including intended uncommitted edits, not a stale committed version/,
    /copy the intended uncommitted candidate files and any required untracked test files into your own export/,
    /Never mutate the primary or shared candidate/,
    /restore command, trap or finally cannot protect it from worker death/,
    /cannot run in isolation, report it as blocked/,
    /files or dependencies are unavailable in permitted scratch, the mutation check remains unvalidated/,
    /never waive it or claim it passed/,
    /Never copy a mutant back or commit it/,
    /keep complete equality and ordering checks/,
    /report only a bounded first mismatch/,
    /custom assertion message or truncated output may still allocate the full diff/,
    /authoring rules, not an enforced memory limit/,
  ]) {
    assert.ok(rule.test(flat), `missing mutation-safety rule: ${rule}`);
  }
}

const templates: AgentTemplate[] = [
  { name: "coder", description: "writes code", prompt_body: "Implement the change." },
  { name: "custom", description: "custom role", prompt_body: "Check the candidate.", tools: ["Read", "Bash"] },
];

describe("mutation-safety guidance reaches every coding actor", () => {
  it("reaches default and customized leads on both harnesses and run kinds", () => {
    for (const harness of ["claude", "codex"] as const) {
      for (const kind of ["issue", "ci_fix", "self_improve"] as const) {
        for (const body of [undefined, "Customized lead body."]) {
          assertMutationSafety(buildLeadSystemPrompt(body, { harness, kind }).append);
        }
      }
    }
  });

  it("reaches Claude own and repo rosters without changing their role bodies", () => {
    const own = assembleAgents(templates).subagents;
    const repo = subagentsFromTemplates(templates, new Set());
    for (const roster of [own, repo, planTurnSubagents(own).subagents]) {
      for (const template of templates) {
        const def = roster[template.name];
        assert.ok(def, `assembled ${template.name}`);
        assert.ok(def.prompt.startsWith(template.prompt_body));
        assertMutationSafety(def.prompt);
      }
    }
  });

  it("reaches Codex subagents with inherited or explicit tools in both phases", () => {
    const agents: Record<string, HarnessAgent> = Object.fromEntries(templates.map((template) => [
      template.name,
      {
        description: template.description,
        prompt: template.prompt_body,
        tools: template.tools ? { kind: "allow", names: template.tools } : { kind: "inherit" },
        deniedTools: [], toolServers: [], skills: [],
      },
    ]));
    for (const phase of ["plan", "implement"] as const) {
      const rendered = renderCodexRun({
        systemPrompt: buildLeadSystemPrompt("Custom lead.", { harness: "codex" }).append,
        prompt: "Work on the candidate.", signal: new AbortController().signal,
        phase, agents, leadSkills: [],
      });
      assertMutationSafety(rendered.leadPrompt.systemPrompt);
      for (const template of templates) {
        const prompt = rendered.perRolePrompts.get(template.name);
        assert.ok(prompt, `rendered ${template.name}`);
        assert.ok(prompt.startsWith(template.prompt_body));
        assertMutationSafety(prompt);
      }
    }
  });
});
