import type {
  AgentSourceView,
  AgentTemplate,
  BuiltinDefinition,
  Skill,
  ToolAllowlistEntry,
} from "../../lib/api";
import { daysAgo } from "./time";
import { mockAdmin } from "./users";

// ── Tool allowlist + repo tool profiles (PRD #18 M4) ─────────────────────────

export const mockToolAllowlist: ToolAllowlistEntry[] = [
  { id: "tal-kubectl", name: "kubectl", pinned_version: null, note: "For the k8s repos", updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
  { id: "tal-opentofu", name: "opentofu", pinned_version: "1.7", note: null, updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
  { id: "tal-jq", name: "jq", pinned_version: null, note: null, updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
];

// A seed profile so the demo repo shows a couple of selected tools.
export const mockRepoToolProfiles: Record<string, string[]> = {
  "repo-uzi": ["jq", "kubectl"],
};

// ── Agent templates ──────────────────────────────────────────────────────────

// The TRAILING NEWLINE is load-bearing, not formatting (issue #201 M4a F7b).
// Every real builtin `.md` ends with one — agenttmpl's parse->Render round-trip
// pins it byte-for-byte — and without it here the demo's own diff opened with a
// phantom "changed" row: identical text shown once removed and once added,
// because diffLines keeps the newline inside its token. The mock was making the
// diff look broken in a way real data never would.
const builtinBody = (name: string, description: string) =>
  `You are the ${name} agent.\n\n## Role\n\n${description}\n\n## Working agreement\n\n- Stay inside the repository you were given.\n- Report findings tersely; the orchestrator relays them.\n- Never touch \`main\` — all work lands on a branch and goes out as an MR.\n`;

// mockShippedBuiltins is the SHIPPED side of the drift comparison (issue #201
// M4a): what this mock "release" carries under each builtin name, and what Reset
// restores a row to.
//
// IT IS A SEPARATE CONSTANT FROM mockTemplates ON PURPOSE, and that separation is
// the whole fixture design. mockTemplates holds the STORED rows; when the two
// were the same array every row was a pristine clone of its own baseline, so
// nothing could carry the badge on a fresh load and editing the fixture moved
// both sides of the comparison at once. Apart, the mock computes drift honestly
// (mockApi's sameContent) instead of hard-coding a flag, and each stored row
// below can differ in exactly one column.
export const mockShippedBuiltins: BuiltinDefinition[] = [
  {
    name: "coder",
    description: "Implements features, fixes bugs, refactors code. Runs the project's test/lint commands before reporting done.",
    model: null,
    tools: null,
    prompt_body: builtinBody("coder", "Implements features, fixes bugs, refactors code."),
  },
  {
    name: "reviewer",
    description: "Reviews code changes for correctness, style, and edge cases. Reports findings only; never modifies code.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "SendMessage", "TaskUpdate", "TaskList", "TaskGet"],
    prompt_body: builtinBody("reviewer", "Reviews code changes for correctness, style, and edge cases."),
  },
  {
    name: "auditor",
    description: "Audits code for security vulnerabilities and unsafe patterns. Reports findings only; never modifies code.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch"],
    prompt_body: builtinBody("auditor", "Audits code for security vulnerabilities and unsafe patterns."),
  },
  {
    name: "tester",
    description: "Validates changes by exercising them against representative real-world inputs and verifying observable behavior.",
    model: null,
    tools: null,
    prompt_body: builtinBody("tester", "Validates changes against representative real-world inputs."),
  },
  {
    name: "documenter",
    description: "Updates documentation only. Never modifies source code.",
    model: null,
    tools: null,
    prompt_body: builtinBody("documenter", "Updates documentation only."),
  },
  {
    name: "fact-checker",
    description: "Adversarially verifies factual claims in docs, specs, and teammate outputs against authoritative sources.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "WebSearch"],
    prompt_body: builtinBody("fact-checker", "Adversarially verifies factual claims against authoritative sources."),
  },
];

// storedBuiltin builds the STORED row for a shipped builtin: a byte-for-byte
// clone of the shipped definition, then whatever single-column edit the case
// needs. Starting from the shipped side rather than restating it is what keeps a
// "pristine" row genuinely pristine when the shipped text is later reworded.
const storedBuiltin = (
  id: string,
  name: string,
  opts: Partial<AgentTemplate> = {},
): AgentTemplate => {
  const def = mockShippedBuiltins.find((d) => d.name === name);
  if (!def) throw new Error(`storedBuiltin: no shipped definition named ${name}`);
  return {
    id,
    name,
    description: def.description,
    model: def.model,
    tools: def.tools,
    prompt_body: def.prompt_body,
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    // Recomputed on every mock read; the literal is just the resting value.
    differs_from_builtin: false,
    // Provenance (PRD #602 M5) defaults to "embedded" for a pristine builtin; a
    // drifted case overrides it to "admin" (a human edit) or "synced" (from the
    // source repo) via opts, matching the server's origin backfill.
    origin: "embedded",
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
    ...opts,
  };
};

// mockTemplates is the STORED side: the rows the mock list endpoint returns.
//
// Each drifted row differs from its shipped twin in exactly ONE column, so the
// badge, the diff and any comparison written against this data are exercised
// per-column rather than in aggregate. mockTemplateDriftCases (below) names each
// case and is asserted by mockApi.templateDrift.test.ts, so a later fixture edit
// that quietly removes one goes red instead of leaving everything green over a
// case that no longer exists.
export const mockTemplates: AgentTemplate[] = [
  // Pristine control: matches its shipped twin exactly, so it must NOT badge.
  storedBuiltin("t-coder", "coder"),
  // tools ORDER only — the case nothing else pins. Same members, swapped. An
  // admin edit, so origin='admin' → the "differs from shipped" badge (PRD #602 M5).
  storedBuiltin("t-reviewer", "reviewer", {
    tools: ["Read", "Bash", "Grep", "Glob", "WebFetch", "SendMessage", "TaskUpdate", "TaskList", "TaskGet"],
    origin: "admin",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(6),
  }),
  // tools MEMBERSHIP: an admin granted the auditor a tool it does not ship with.
  storedBuiltin("t-auditor", "auditor", {
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "WebSearch"],
    origin: "admin",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(9),
  }),
  // prompt_body only — SYNCED from the source repo (PRD #602 M5): an overridden
  // builtin (scope='builtin', origin='synced'). It shows the SYNCED badge INSTEAD of
  // "differs from shipped" and KEEPS its reset-to-embedded path (there is a shipped
  // twin). Its content still differs from the embedded default, so differs computes true.
  storedBuiltin("t-tester", "tester", {
    prompt_body: `${builtinBody("tester", "Validates changes against representative real-world inputs.")}\n- Always run the live-DB sweep before reporting a suite green.`,
    origin: "synced",
    updated_by: "agent-source",
    updated_at: daysAgo(3),
  }),
  // model only: shipped inherits (null), the stored row pins one.
  storedBuiltin("t-documenter", "documenter", {
    model: "haiku",
    origin: "admin",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(14),
  }),
  // description only.
  storedBuiltin("t-fact-checker", "fact-checker", {
    description: "Adversarially verifies factual claims against authoritative sources, and cites every one.",
    origin: "admin",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(5),
  }),
  // A builtin row with NO shipped twin: a role a later release dropped. It must
  // report no drift (nothing to compare against) AND must not offer Reset, which
  // would answer 409. Absent from mockShippedBuiltins on purpose.
  {
    id: "t-spec-keeper",
    name: "spec-keeper",
    description: "Keeps specs/ in sync with implementation work. Maintains specs/human.md and specs/ai.md.",
    model: null,
    tools: null,
    prompt_body: builtinBody("spec-keeper", "Keeps specs/ in sync with implementation work."),
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    differs_from_builtin: false,
    origin: "embedded",
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
  },
  // A global row: no shipped counterpart, so never badged whatever its content.
  // Admin-authored (origin null) — a plain global, not from the source.
  {
    id: "t-release-notes",
    name: "release-notes",
    description: "Drafts release notes from the merged MRs since the last tag.",
    model: "haiku",
    tools: ["Bash", "Read", "Grep", "Glob"],
    prompt_body: builtinBody("release-notes", "Drafts release notes from the merged MRs since the last tag."),
    is_builtin: false,
    scope: "global",
    user_id: null,
    updated_by: "vlad@uzi.local",
    differs_from_builtin: false,
    origin: null,
    created_at: daysAgo(11),
    updated_at: daysAgo(2),
  },
  // A SYNCED-ONLY role (PRD #602 M5): scope='global', origin='synced', with NO
  // embedded twin. It shows the SYNCED badge and — because there is no shipped
  // default to reset to — offers only Delete (never Reset). differs_from_builtin is
  // false for it (a global has no builtin to compare against).
  {
    id: "t-migration-writer",
    name: "migration-writer",
    description: "Authors and reviews database migrations, keeping goose numbering strict and reversible.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "Edit"],
    prompt_body: builtinBody("migration-writer", "Authors and reviews database migrations."),
    is_builtin: false,
    scope: "global",
    user_id: null,
    updated_by: "agent-source",
    differs_from_builtin: false,
    origin: "synced",
    created_at: daysAgo(4),
    updated_at: daysAgo(1),
  },
  // THE CASE THAT SEPARATES A SCOPE-CHECKING IMPLEMENTATION FROM A NAME-ONLY ONE:
  // a private template whose name collides with a builtin (00048 allows it
  // explicitly). Its content deliberately differs from the shipped `coder`, so a
  // comparison keyed on name alone badges it — and the badge would advertise a
  // Reset that answers 400.
  {
    id: "t-my-coder",
    name: "coder",
    description: "My own coder: same name as the builtin, deliberately different content.",
    model: "sonnet",
    tools: ["Bash", "Read", "Edit"],
    prompt_body: "You are my personal coder. Follow my house style, not the shipped one.\n",
    is_builtin: false,
    scope: "user",
    user_id: mockAdmin.id,
    updated_by: "vlad@uzi.local",
    differs_from_builtin: false,
    origin: null,
    created_at: daysAgo(8),
    updated_at: daysAgo(8),
  },
];

// mockTemplateDriftCases names the discriminating fixture cases above by template
// id. It exists so the set can be ASSERTED rather than assumed: a later edit that
// collapses two cases into one, or drops the removed-builtin row, then reds
// instead of silently shrinking coverage while every test stays green.
export const mockTemplateDriftCases: Record<string, string> = {
  "t-coder": "pristine control — must not badge",
  "t-reviewer": "tools order only",
  "t-auditor": "tools membership",
  "t-tester": "prompt_body only",
  "t-documenter": "model only (shipped inherits, stored pins)",
  "t-fact-checker": "description only",
  "t-spec-keeper": "builtin with no shipped twin",
  "t-release-notes": "global scope — no shipped counterpart",
  "t-my-coder": "user scope colliding with a builtin name",
};

// ── Agent source (PRD #602 M5) ────────────────────────────────────────────────
// The demo agent-source view: a CONFIGURED, private repo with a PENDING staged
// snapshot, so the admin card renders its full shape (status + Sync now + a rich
// staged diff + Approve) with no backend. The staged diff carries one of every
// action so the review chips and the counts are all exercised:
//   - add       (planner: a new synced-only role)
//   - override  (tester: overrides the shipped builtin body)
//   - unchanged (auditor: already matches)
//   - conflict  (release-notes: collides with the admin global of the same name)
//   - remove    (migration-writer: gone from the source; its synced global is removed)
// plus one failed parse (broken-role: ok=false), so `failed` is non-zero.
export const mockAgentSource: AgentSourceView = {
  config: {
    url: "https://github.com/uzi-dev/agent-library.git",
    ref: "v1.2.0",
    folder: ".claude/agents",
    enabled: true,
    interval: "1h",
    credential_configured: true,
  },
  status: {
    last_sync_at: daysAgo(0),
    last_sync_sha: "9f2c17a4e0b3d6c8a1f5",
    last_sync_status: "ok",
    last_applied_at: daysAgo(2),
    last_applied_sha: "3b7d0e51c9a2f4681db0",
    counts: { staged: 3, changed: 4, failed: 1 },
  },
  staged: {
    fetched_at: daysAgo(0),
    fetched_sha: "9f2c17a4e0b3d6c8a1f5",
    source_url: "https://github.com/uzi-dev/agent-library.git",
    source_ref: "v1.2.0",
    roles: [
      {
        name: "planner",
        ok: true,
        description: "Breaks an issue into a sequenced, milestone-shaped plan before any code is written.",
        model: "opus",
        tools: ["Read", "Grep", "Glob", "SendMessage"],
        prompt_body: builtinBody("planner", "Breaks an issue into a sequenced plan before any code is written."),
      },
      {
        name: "tester",
        ok: true,
        description: "Validates changes by exercising them against representative real-world inputs.",
        prompt_body: `${builtinBody("tester", "Validates changes against representative real-world inputs.")}\n- Always run the live-DB sweep before reporting a suite green.`,
        // The raw source body carried control/bidi chars that were stripped for the
        // preview — the review surface flags that the preview under-represents it.
        body_sanitized: true,
      },
      {
        name: "auditor",
        ok: true,
        description: "Audits code for security vulnerabilities and unsafe patterns. Reports findings only.",
        tools: ["Bash", "Read", "Grep", "Glob", "WebFetch"],
        prompt_body: builtinBody("auditor", "Audits code for security vulnerabilities and unsafe patterns."),
      },
      {
        name: "broken-role",
        ok: false,
        reason: "invalid",
        description: "A role whose frontmatter failed to parse — staged with its error, never applied.",
      },
    ],
    diff: [
      { name: "planner", action: "add", detail: "new synced-only role" },
      { name: "tester", action: "override", detail: "overrides a builtin" },
      { name: "auditor", action: "unchanged" },
      { name: "release-notes", action: "conflict", detail: "collides with an admin global template" },
      { name: "migration-writer", action: "remove", detail: "no longer in the source" },
    ],
    counts: { staged: 3, changed: 4, failed: 1 },
    pending: true,
  },
};

// ── Agent skills (PRD #16) ────────────────────────────────────────────────────

// Three scopes, exactly as the real read returns them: a builtin (shipped,
// resettable, never deletable), a global (admin-managed), and one "Mine" skill
// owned by the demo session (admin). The mock reconciler treats the builtin's
// seed body as its reset target.
export const mockSkills: Skill[] = [
  {
    id: "skill-prd-lifecycle",
    name: "prd-lifecycle",
    description:
      "The end-of-run PRD checklist: tick only what the branch's evidence supports, and move the file to prds/done/ when every item is complete.",
    body: [
      "# prd-lifecycle",
      "",
      "At the end of an issue run, scan every unchecked item in the issue's",
      "linked `prds/*.md` file and tick only the ones direct evidence supports.",
      "Move the file to `prds/done/` when (and only when) every item is",
      "complete; a partly-done PRD keeps its ticks and stays where it is.",
      "",
      "## Reviewer half",
      "",
      "Check the PRD diff against what the branch actually changed, and send",
      "back an unsupported completion claim.",
    ].join("\n"),
    scope: "builtin",
    user_id: null,
    updated_by: null,
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
  },
  {
    id: "skill-argo-debug",
    name: "argocd-debugging",
    description:
      "Diagnose a stuck ArgoCD sync: OutOfSync vs Degraded, hook failures, and where to read controller logs.",
    body: "# argocd-debugging\n\nStart from the Application status, then the resource tree…",
    scope: "global",
    user_id: null,
    updated_by: mockAdmin.email,
    created_at: daysAgo(12),
    updated_at: daysAgo(3),
  },
  {
    id: "skill-qdrant-kb",
    name: "qdrant-kb",
    description: "My notes on the team's qdrant knowledge-base schema and the ingest CLI flags.",
    body: "# qdrant-kb\n\nCollections, payload indexes, and the `kb ingest` flags I always forget…",
    scope: "user",
    user_id: mockAdmin.id,
    updated_by: mockAdmin.email,
    created_at: daysAgo(5),
    updated_at: daysAgo(1),
  },
  {
    // Owned by another user (Alex). The admin session sees it in the "Other
    // users" group (view-only — admins can read but not edit others' private
    // skills); signed in as Alex it is their "Mine".
    id: "skill-alex-runbook",
    name: "alex-deploy-runbook",
    description: "Alex's personal runbook for the staging deploy dance.",
    body: "# alex-deploy-runbook\n\nThe order I run the staging promotion steps in…",
    scope: "user",
    user_id: "u-mira",
    updated_by: "mira@uzi.local",
    created_at: daysAgo(4),
    updated_at: daysAgo(2),
  },
];

// Seed allocation: the builtin prd-lifecycle is shared onto the coder template
// so the allocation panel shows a populated union out of the box.
export const mockAllocations: Record<string, { shared: string[]; mine: string[] }> = {
  "t-coder": { shared: ["skill-prd-lifecycle"], mine: [] },
};
