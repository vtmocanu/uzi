import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { buildAutomaticRevisionPrompt, buildLeadSystemPrompt, SUBAGENT_SAFETY_APPEND } from "../src/prompt.js";
import { crossCheckBrief, codeCrossCheckBrief, crossCheckPrompt } from "../src/codex/cross-check.js";
import type { ClaimResponse } from "../src/protocol.js";

// These are instruction-contract checks, not a mock model or a prediction of its verdict.
const rules = {
  requirements: /Enumerate requirements and acceptance criteria from issue_title and issue_body and repository PRDs linked by the issue/,
  readLimits: /existing [^\n]+within the current deadline and read\/search limits; comments are not supplied\. Do not fetch URLs or add tools/,
  gaps: /omitted or partly covered requirement as warning with mandatory summary prefix Requirement gap:/,
  live: /hosted, live or maintainer-only surfaces as info with mandatory summary prefix Coverage uncertainty:/,
  markers: /Every coverage item must use one of these prefixes; defect items must never use either prefix/,
  regressionTests: /Flag regression tests that cannot fail on the unfixed code/,
  safety: /stated by the issue as a safety, security or data-integrity constraint is a defect: error, no coverage prefix, REVISE/,
  unreadable: /Missing, external, unreadable or oversize requirement sources get a Coverage uncertainty: note in summary, never treated as covered/,
  noDefectCoverage: /No defect found does not mean requirement covered/,
  advisory: /Coverage alone NEVER justifies REVISE or BLOCK/,
  cap: /20-item cap, place defects first; coverage uses only remaining space[\s\S]*"N coverage notes omitted"[\s\S]*never displace a defect/,
  coverageDisposition: /missing or declined (?:prior coverage disposition|coverage dispositions)[\s\S]{0,130}alone never justif(?:ies|y) REVISE or BLOCK/i,
  effects: /(?:environment|env)[\s\S]{0,250}effects?/i,
  rejectBypass: /(?:reject|flag|must not|do not|forbid)[\s\S]{0,160}skip[\s\S]{0,100}select[\s\S]{0,100}narrow[\s\S]{0,100}weaken[\s\S]{0,180}(?:restriction|prohibit)/i,
  scratch: /(?:(?:temporary|scratch)[\s\S]{0,220}(?:relocat|placement)|relocat[\s\S]{0,220}(?:temporary|scratch))[\s\S]{0,220}(?:not|isn't|alone)[\s\S]{0,120}(?:bypass|skip)/i,
  goChildren: /(?:inherit[\s\S]{0,120}Go|Go[\s\S]{0,120}inherit)[\s\S]{0,120}build[\s\S]{0,80}test/i,
  reliedEffects: /(?:name|describe|state|explain)[\s\S]{0,140}effects?[\s\S]{0,100}(?:rel(?:y|ied)|depend)/i,
  noNameProof: /(?:neither|not|never)[\s\S]{0,100}(?:variable|name)[\s\S]{0,200}(?:destination|scratch)[\s\S]{0,200}(?:prove|guarantee|harmless|safe)/i,
  sensitiveEffects: /config[\s\S]{0,100}credentials?[\s\S]{0,100}test[\s\S]{0,160}(?:inspectable|evidence)/i,
  roundOne: /(?:round[ _-]*1|first round|initial)[\s\S]{0,180}(?:not required|no[\s\S]{0,60}requir|need not)/i,
  laterRounds: /(?:round[ _-]*2|subsequent|later round|second)[\s\S]{0,200}Cross-check dispositions/i,
  omission: /(?:missing|incomplete|omission)[\s\S]{0,180}revise[\s\S]{0,160}(?:never|not)[\s\S]{0,60}block/i,
  latestItems: /(?:each|every|one per)[\s\S]{0,140}(?:immediately preceding|most recent|latest|current)[\s\S]{0,100}(?:item|finding)/i,
  olderOptional: /older[\s\S]{0,100}(?:optional|not required|need not)/i,
  declines: /declin[\s\S]{0,130}(?:rationale|reason)[\s\S]{0,130}(?:inspectable|evidence)/i,
  rationaleFirst: /(?:evaluat|consider|assess|read)[\s\S]{0,120}(?:rationale|reason|declin)[\s\S]{0,150}before[\s\S]{0,100}(?:repeat|reissu)/i,
  unverified: /uninspectable[\s\S]{0,220}unverified[\s\S]{0,220}(?:alone|itself)[\s\S]{0,100}(?:cannot|can not|does not)[\s\S]{0,160}(?:exception|exempt)/i,
  noPrecedence: /(?:precedence|priority|hierarchy)[\s\S]{0,140}(?:unchanged|not change|does not|no[\s\S]{0,40}rewrit)|(?:do not|cannot|does not)[\s\S]{0,100}(?:precedence|hierarchy)/i,
  completePlan: /COMPLETE[\s\S]{0,100}(?:plan_md|plan)/i,
  addressed: /addressed[\s\S]{0,100}where[\s\S]{0,80}how/i,
  heading: /(?:exact|literal)[\s\S]{0,100}(?:heading|section)[\s\S]{0,120}Cross-check dispositions|(?:heading|section)[\s\S]{0,120}(?:exact|literal)[\s\S]{0,100}Cross-check dispositions/i,
  supportedDeclines: /(?:supported|evidence.backed|evidenced)[\s\S]{0,100}declin[\s\S]{0,120}(?:accept|valid)|declin[\s\S]{0,140}(?:supported|evidence.backed|inspectable evidence)[\s\S]{0,140}(?:accept|valid)|accept[\s\S]{0,100}(?:supported|evidence.backed|evidenced)[\s\S]{0,100}declin/i,
} satisfies Record<string, RegExp>;

function trusted(text: string): string {
  return text.replace(/^<([a-z_]+_(?:[a-f0-9]+|escaped))>\n[\s\S]*?^<\/\1>$/gm, "");
}
function requireRules(text: string, names: (keyof typeof rules)[]): void {
  for (const name of names) assert.match(trusted(text), rules[name], `trusted instruction: ${name}`);
}
const fixtures = JSON.parse(readFileSync(new URL("../../fixtures/plan-cross-check/cases.json", import.meta.url), "utf8")) as {
  name: string; round: number; plan: string; expected: string; rules: (keyof typeof rules)[];
}[];
test("guidance fixture loader has the exact nonempty case set", () => {
  assert.deepEqual(fixtures.map(c => c.name).sort(), [
    "tmpdir-internal-logs-go-children", "tmpdir-narrows-tests", "scratch-destination-changes-config",
    "initial-no-heading", "later-no-heading", "later-incomplete", "earlier-items-only",
    "supported-decline", "unsupported-authority", "addressed-where-how",
    "four-criteria-three-covered", "regression-cannot-fail", "unreadable-linked-sources",
    "missing-coverage-disposition", "declined-coverage", "partial-requirement", "live-only",
    "omitted-issue-safety", "defects-fill-cap", "no-defect-is-not-covered",
  ].sort());
  for (const c of fixtures) {
    assert.ok(c.plan.trim() && c.expected.trim() && c.rules.length);
    assert.ok(Number.isInteger(c.round) && c.round >= 1 && c.round <= 5);
  }
});

for (const c of fixtures) test(`plan checker instructions decide ${c.name}: ${c.expected}`, () => {
  for (const tools of ["Read and Search", "Read, Grep and Glob"]) requireRules(crossCheckBrief(tools), c.rules);
  const revision = buildAutomaticRevisionPrompt("plan", { summary: c.expected, items: [
    { file: "plan.md", severity: "warning", summary: c.name, rationale: "inspect the cited script" },
  ] }, c.plan);
  requireRules(revision, c.rules.filter(name => revisionRules.includes(name)));
  assert.match(trusted(revision), /Cross-check dispositions/);
});

const revisionRules: (keyof typeof rules)[] = [
  "effects", "rejectBypass", "scratch", "goChildren", "reliedEffects", "noNameProof", "sensitiveEffects",
  "coverageDisposition", "latestItems", "olderOptional", "declines", "unverified", "noPrecedence", "completePlan", "addressed", "heading",
];
test("plan revision puts dispositions and effect guidance outside both DATA fences", () => {
  const advice = { summary: "CURRENT-SUMMARY", items: [
    { file: "Taskfile.yml", severity: "warning" as const, summary: "CURRENT-ITEM", rationale: "CURRENT-EVIDENCE" },
  ] };
  const prior = "# PRIOR-COMPLETE-PLAN\n## Cross-check dispositions\nOLDER-ITEM addressed";
  const text = buildAutomaticRevisionPrompt("plan", advice, prior);
  requireRules(text, revisionRules);
  assert.match(trusted(text), /Cross-check dispositions/);
  assert.match(text, /<advice_([a-f0-9]{16})>\n[\s\S]*CURRENT-ITEM[\s\S]*CURRENT-EVIDENCE[\s\S]*\n<\/advice_\1>/);
  assert.match(text, /<prior_plan_([a-f0-9]{16})>\n# PRIOR-COMPLETE-PLAN[\s\S]*OLDER-ITEM addressed\n<\/prior_plan_\1>/);
  assert.doesNotMatch(trusted(text), /CURRENT-ITEM|OLDER-ITEM/);
});

test("negative controls reject guidance moved into DATA or supplied only to code stage", () => {
  const plan = buildAutomaticRevisionPrompt("plan", "CURRENT-ITEM", "OLDER-ITEM");
  const dataOnly = `<advice_0123456789abcdef>\n${plan}\n</advice_0123456789abcdef>`;
  assert.throws(() => requireRules(dataOnly, ["latestItems"]), /trusted instruction: latestItems/);
  assert.throws(() => requireRules(buildAutomaticRevisionPrompt("code", "advice"), ["latestItems"]),
    /trusted instruction: latestItems/);
  const brief = crossCheckBrief("Read and Search");
  assert.throws(() => requireRules(`<candidate_0123456789abcdef>\n${brief}\n</candidate_0123456789abcdef>`, ["effects"]),
    /trusted instruction: effects/);
  assert.throws(() => requireRules(codeCrossCheckBrief("Read and Search"), ["effects"]), /trusted instruction: effects/);
  // Positive controls make these helper tests red until the plan-specific contract lands.
  requireRules(plan, ["latestItems"]);
  requireRules(brief, ["effects"]);
});

test("code checker and code revision retain their stage-specific contracts", () => {
  for (const text of [codeCrossCheckBrief("Read and Search"), buildAutomaticRevisionPrompt("code", "advice")]) {
    assert.doesNotMatch(trusted(text), /Cross-check dispositions|immediately preceding|scratch placement/i);
  }
  assert.match(buildAutomaticRevisionPrompt("code", { findings: [] }), /report_cross_check_dispositions/);
});

test("worker scratch placement fact reaches both runtime prompts and both plan checker families", () => {
  const runtime = [buildLeadSystemPrompt(undefined, { harness: "claude" }).append,
    buildLeadSystemPrompt(undefined, { harness: "codex" }).append, SUBAGENT_SAFETY_APPEND];
  const checkers = [crossCheckBrief("Read and Search"), crossCheckBrief("Read, Grep and Glob")];
  for (const text of [...runtime, ...checkers, buildAutomaticRevisionPrompt("plan", "advice")]) {
    assert.match(trusted(text), /\.uzi\/scratch\//, "worker scratch destination is trusted, outside DATA");
    assert.match(trusted(text), /temporary[\s\S]{0,220}(?:artifact|file|log)|(?:artifact|file|log)[\s\S]{0,220}temporary/i,
      "placement fact covers temporary artifacts");
  }
  const fact = "Put temporary file-tool and shell artifacts, including test logs and review exports, under `.uzi/scratch/` in this worktree.";
  for (const text of [...runtime, ...checkers]) {
    assert.ok(trusted(text).includes(fact), "same bounded worker-authored placement fact outside DATA");
  }
});

test("both checker families wire the shared plan brief into trusted instructions", () => {
  const claude = readFileSync(new URL("../src/claude-cross-check.ts", import.meta.url), "utf8");
  const codex = readFileSync(new URL("../src/codex/cross-check.ts", import.meta.url), "utf8");
  assert.match(claude, /brief:\s*code\s*\?[\s\S]{0,200}:\s*crossCheckBrief\("Read, Grep and Glob"\)/);
  assert.match(codex, /const BRIEF = crossCheckBrief\("Read and Search"\)/);
  assert.match(codex, /developerInstructions:[\s\S]{0,200}: BRIEF/);
});

test("checker DATA retains complete candidate and explicit round context 1 through 5", () => {
  for (let round = 1; round <= 5; round++) {
    const candidate = { stage: "plan" as const, round, lead_run_id: "lead", candidate_digest: "digest",
      deadline_at: "2099-01-01T00:00:00Z", base_commit: "a".repeat(40), planning_diff: "FULL-DIFF",
      plan_md: "# COMPLETE-CANDIDATE\n## Cross-check dispositions\nCURRENT-ITEM addressed in validation",
      milestones: [{ id: "m1", title: "ALL-MILESTONES" }], required_capabilities: [], required_tools: [], size_class: "s" };
    const claim: ClaimResponse = {
      kind: "cross_check", run_id: "check", issue_iid: null, issue_title: "issue", issue_description: "body",
      repo: { id: "repo", url: "https://example.test/owner/repo",
        clone_url: "https://example.test/owner/repo.git", default_branch: "main" },
      secrets: { forge_pat: "" }, last_seq: 0, agents: [], cross_check: candidate,
    };
    const prompt = crossCheckPrompt(claim);
    const match = prompt.match(/^<candidate_([a-f0-9]{32})>\n([^\n]+)\n<\/candidate_\1>$/m);
    assert.ok(match, "complete candidate inside a nonce DATA fence");
    const expected = JSON.parse(JSON.stringify({ ...candidate, planning_diff: undefined }));
    assert.deepEqual(JSON.parse(match[2]!), expected);
    assert.match(prompt, /^<planning_diff_([a-f0-9]{32})>\nFULL-DIFF\n<\/planning_diff_\1>$/m);
    assert.doesNotMatch(trusted(prompt), /CURRENT-ITEM|COMPLETE-CANDIDATE|FULL-DIFF/);
  }
});
