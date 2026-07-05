// Seed data for the fully in-browser mock mode (VITE_UZI_MOCK=1). Everything
// here is plain in-memory state: no request ever leaves the browser. Timestamps
// are derived from Date.now() at module load so relative times ("last seen 2m
// ago") always look fresh in a demo.

import type {
  AdminWorker,
  AgentTemplate,
  Board,
  ForgeConnection,
  LatestRun,
  Repo,
  Run,
  RunListItem,
  RunMessage,
  SecretMeta,
  Skill,
  User,
  Worker,
} from "../lib/api";

const NOW = Date.now();
export const minsAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();
export const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();

// ── Users ────────────────────────────────────────────────────────────────────

export const mockAdmin: User = {
  id: "u-admin",
  email: "vlad@uzi.local",
  display_name: "Vlad",
  is_admin: true,
  is_active: true,
  created_at: daysAgo(41),
  last_login: minsAgo(7),
};

export const mockUsers: User[] = [
  mockAdmin,
  {
    id: "u-mira",
    email: "mira@uzi.local",
    display_name: "Mira Ionescu",
    is_admin: false,
    is_active: true,
    created_at: daysAgo(33),
    last_login: minsAgo(95),
  },
  {
    id: "u-andrei",
    email: "andrei@uzi.local",
    display_name: "Andrei Pop",
    is_admin: false,
    is_active: true,
    created_at: daysAgo(20),
    last_login: daysAgo(1),
  },
  {
    id: "u-dan",
    email: "dan@uzi.local",
    display_name: null,
    is_admin: false,
    is_active: false,
    created_at: daysAgo(18),
    last_login: daysAgo(12),
  },
];

// ── Secrets ──────────────────────────────────────────────────────────────────

export const mockSecrets: SecretMeta[] = [
  { kind: "anthropic_token", created_at: daysAgo(30), updated_at: daysAgo(4) },
];

// ── Forge ────────────────────────────────────────────────────────────────────

export const mockConnection: ForgeConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "uzi-bot",
  bot_forge_user_id: 4021,
  created_at: daysAgo(30),
  last_verified_at: minsAgo(42),
  // Demo happy path: least-privilege ✓ — api-only token, Developer on protected
  // mains. The finding states are exercised in the component tests.
  privilege_status: "ok",
  privilege_checked_at: minsAgo(20),
  privilege_report: {
    checked_at: minsAgo(20),
    status: "ok",
    token: { scopes: ["api"], active: true, violations: [], warnings: [] },
    repos: [
      { repo_id: "repo-uzi", path: "vtmocanu/uzi", role: 30, member: true, violations: [], warnings: [] },
      { repo_id: "repo-atlas", path: "vtmocanu/atlas-api", role: 30, member: true, violations: [], warnings: [] },
    ],
  },
};

export const mockForgeConfig = {
  allowed_base_urls: ["https://gitlab.example.com"],
  forge_types: ["gitlab"],
};

export const mockRepos: Repo[] = [
  {
    id: "repo-uzi",
    connection_id: "conn-1",
    forge_project_id: 118,
    path_with_namespace: "vtmocanu/uzi",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: true,
  },
  {
    id: "repo-atlas",
    connection_id: "conn-1",
    forge_project_id: 204,
    path_with_namespace: "vtmocanu/atlas-api",
    web_url: "https://gitlab.example.com/vtmocanu/atlas-api",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
  },
  {
    id: "repo-www",
    connection_id: "conn-1",
    forge_project_id: 87,
    path_with_namespace: "example/website",
    web_url: "https://gitlab.example.com/example/website",
    default_branch: "main",
    enabled: false,
    repo_skills_enabled: false,
  },
];

// ── Boards ───────────────────────────────────────────────────────────────────

// LIVE_RUN_ID is the seeded run whose message stream is SIMULATED live: the mock
// engine starts a timed script the first time the run view subscribes to it.
// Declared here (above the boards) because a card's latest_run references it.
export const LIVE_RUN_ID = "run-live";

const uziUrl = (iid: number) => `https://gitlab.example.com/vtmocanu/uzi/-/issues/${iid}`;
const atlasUrl = (iid: number) => `https://gitlab.example.com/vtmocanu/atlas-api/-/issues/${iid}`;

// latestRun builds a card's latest_run snapshot (PRD #12 M2). Kept as inline
// literals per card so the seed stays declarative; every id matches a real run
// in mockRuns so the card's "view run" link and history resolve.
function latestRun(fields: Partial<LatestRun> & Pick<LatestRun, "id" | "status">): LatestRun {
  return {
    mr_iid: null,
    failure_reason: null,
    owner_name: "Vlad",
    worker_name: null,
    is_mine: true,
    run_count: 1,
    created_at: minsAgo(30),
    updated_at: minsAgo(30),
    ...fields,
  };
}

export const mockBoards: Record<string, Board> = {
  "repo-uzi": {
    repo_id: "repo-uzi",
    path_with_namespace: "vtmocanu/uzi",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "In progress", position: 1 },
      { label_name: "Review", position: 2 },
    ],
    cards: [
      {
        iid: 31,
        title: "Add CSV export to the runs list",
        state: "opened",
        labels: ["PRD"],
        web_url: uziUrl(31),
        author: "mira",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 29,
        title: "Retry failed forge column moves with backoff",
        state: "opened",
        labels: ["PRD"],
        web_url: uziUrl(29),
        author: "vlad",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 27,
        title: "Dark-mode toggle for the docs section",
        state: "opened",
        labels: ["PRD", "Ready"],
        web_url: uziUrl(27),
        author: "vlad",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 26,
        title: "Board card badges for MR pipeline status",
        state: "opened",
        labels: ["PRD", "Ready"],
        web_url: uziUrl(26),
        author: "andrei",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 24,
        title: "Worker heartbeat metrics endpoint",
        state: "opened",
        labels: ["PRD", "In progress"],
        web_url: uziUrl(24),
        author: "vlad",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        latest_run: latestRun({
          id: LIVE_RUN_ID,
          status: "running",
          worker_name: "laptop",
          created_at: minsAgo(2),
          updated_at: minsAgo(1),
        }),
      },
      {
        iid: 22,
        title: "Per-run cost budget with hard stop",
        state: "opened",
        labels: ["PRD", "In progress", "Review"],
        web_url: uziUrl(22),
        author: "mira",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: true,
        latest_run: null,
      },
      {
        iid: 21,
        title: "Plan-approval notifications via email",
        state: "opened",
        labels: ["PRD", "Review"],
        web_url: uziUrl(21),
        author: "vlad",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: false,
        latest_run: latestRun({
          id: "run-awaiting",
          status: "awaiting_approval",
          worker_name: "laptop",
          created_at: minsAgo(10),
          updated_at: minsAgo(6),
        }),
      },
      {
        iid: 18,
        title: "Run view: fold tool results under their calls",
        state: "closed",
        labels: ["PRD"],
        web_url: uziUrl(18),
        author: "vlad",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        latest_run: latestRun({
          id: "run-done",
          status: "completed",
          mr_iid: 42,
          worker_name: "laptop",
          created_at: minsAgo(225),
          updated_at: minsAgo(184),
        }),
      },
      {
        iid: 15,
        title: "Encrypt per-user Anthropic tokens at rest",
        state: "closed",
        labels: ["PRD"],
        web_url: uziUrl(15),
        author: "vlad",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        latest_run: null,
      },
    ],
  },
  "repo-atlas": {
    repo_id: "repo-atlas",
    path_with_namespace: "vtmocanu/atlas-api",
    web_url: "https://gitlab.example.com/vtmocanu/atlas-api",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "Doing", position: 1 },
    ],
    cards: [
      {
        iid: 9,
        title: "Rate-limit the public search endpoint",
        state: "opened",
        labels: ["PRD"],
        web_url: atlasUrl(9),
        author: "andrei",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 8,
        title: "OpenAPI spec drift check in CI",
        state: "opened",
        labels: ["PRD", "Ready"],
        web_url: atlasUrl(8),
        author: "mira",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        latest_run: null,
      },
      {
        iid: 7,
        title: "Postgres connection pool tuning",
        state: "opened",
        labels: ["PRD", "Doing"],
        web_url: atlasUrl(7),
        author: "vlad",
        has_prd_link: true,
        column: "Doing",
        closed: false,
        conflict: false,
        latest_run: latestRun({
          id: "run-failed",
          status: "failed",
          failure_reason: "run timed out after 2h0m0s (RUN_TIMEOUT)",
          worker_name: "ci-runner-1",
          run_count: 2,
          created_at: daysAgo(1.3),
          updated_at: daysAgo(1.1),
        }),
      },
      {
        iid: 5,
        title: "Healthcheck should ping the DB pool",
        state: "closed",
        labels: ["PRD"],
        web_url: atlasUrl(5),
        author: "vlad",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        latest_run: latestRun({
          id: "run-cancelled",
          status: "cancelled",
          created_at: daysAgo(3.1),
          updated_at: daysAgo(3),
        }),
      },
    ],
  },
};

// ── Workers ──────────────────────────────────────────────────────────────────

export const mockWorkers: Worker[] = [
  {
    id: "w-laptop",
    name: "laptop",
    status: "online",
    busy: true,
    version: "0.4.2",
    last_heartbeat_at: minsAgo(0.2),
    created_at: daysAgo(14),
  },
  {
    id: "w-ci",
    name: "ci-runner-1",
    status: "offline",
    busy: false,
    version: "0.4.1",
    last_heartbeat_at: daysAgo(2),
    created_at: daysAgo(21),
  },
];

export const mockAdminWorkers: AdminWorker[] = [
  { ...mockWorkers[0], owner_email: mockAdmin.email },
  { ...mockWorkers[1], owner_email: mockAdmin.email },
  {
    id: "w-mira",
    name: "mira-desktop",
    status: "online",
    busy: false,
    version: "0.4.2",
    last_heartbeat_at: minsAgo(0.5),
    created_at: daysAgo(9),
    owner_email: "mira@uzi.local",
  },
];

// ── Agent templates ──────────────────────────────────────────────────────────

const tmpl = (
  id: string,
  name: string,
  description: string,
  opts: Partial<AgentTemplate> = {},
): AgentTemplate => ({
  id,
  name,
  description,
  model: null,
  tools: null,
  prompt_body: `You are the ${name} agent.\n\n## Role\n\n${description}\n\n## Working agreement\n\n- Stay inside the repository you were given.\n- Report findings tersely; the orchestrator relays them.\n- Never touch \`main\` — all work lands on a branch and goes out as an MR.`,
  is_builtin: true,
  updated_by: null,
  created_at: daysAgo(40),
  updated_at: daysAgo(40),
  ...opts,
});

export const mockTemplates: AgentTemplate[] = [
  tmpl("t-coder", "coder", "Implements features, fixes bugs, refactors code. Runs the project's test/lint commands before reporting done."),
  tmpl("t-reviewer", "reviewer", "Reviews code changes for correctness, style, and edge cases. Reports findings only; never modifies code.", {
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "SendMessage", "TaskUpdate", "TaskList", "TaskGet"],
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(6),
  }),
  tmpl("t-auditor", "auditor", "Audits code for security vulnerabilities and unsafe patterns. Reports findings only; never modifies code.", {
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch"],
  }),
  tmpl("t-tester", "tester", "Validates changes by exercising them against representative real-world inputs and verifying observable behavior."),
  tmpl("t-documenter", "documenter", "Updates documentation only. Never modifies source code.", {
    model: "haiku",
  }),
  tmpl("t-fact-checker", "fact-checker", "Adversarially verifies factual claims in docs, specs, and teammate outputs against authoritative sources.", {
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "WebSearch"],
  }),
  tmpl("t-spec-keeper", "spec-keeper", "Keeps specs/ in sync with implementation work. Maintains specs/human.md and specs/ai.md."),
  tmpl("t-release-notes", "release-notes", "Drafts release notes from the merged MRs since the last tag.", {
    is_builtin: false,
    model: "haiku",
    tools: ["Bash", "Read", "Grep", "Glob"],
    updated_by: "vlad@uzi.local",
    created_at: daysAgo(11),
    updated_at: daysAgo(2),
  }),
];

// ── Agent skills (PRD #16) ────────────────────────────────────────────────────

// Three scopes, exactly as the real read returns them: a builtin (shipped,
// resettable, never deletable), a global (admin-managed), and one "Mine" skill
// owned by the demo session (admin). The mock reconciler treats the builtin's
// seed body as its reset target.
export const mockSkills: Skill[] = [
  {
    id: "skill-mm-cicd",
    name: "ci-cd-norms",
    description:
      "How CI/CD works at example: myorg/pipelines includes, Harbor registry, ArgoCD GitOps, and how to spot an exception repo.",
    body: [
      "# ci-cd-norms",
      "",
      "The default norm: a thin `.gitlab-ci.yml` that includes a bundle from the",
      "private `myorg/pipelines` project (lint → build → audit → push → cleanup).",
      "Images and OCI charts go to Harbor (`harbor.example.com`). **CI never",
      "deploys** — the ArgoCD app-of-apps in `myorg/k8s/argo-apps` does.",
      "",
      "## Spotting an exception",
      "",
      "No `include:` of `myorg/pipelines` means the repo is an exception. Follow its",
      "local convention; never \"normalize\" it unasked.",
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
    // Owned by another user (Mira). The admin session sees it in the "Other
    // users" group (view-only — admins can read but not edit others' private
    // skills); signed in as Mira it is her "Mine".
    id: "skill-mira-runbook",
    name: "mira-deploy-runbook",
    description: "Mira's personal runbook for the staging deploy dance.",
    body: "# mira-deploy-runbook\n\nThe order I run the staging promotion steps in…",
    scope: "user",
    user_id: "u-mira",
    updated_by: "mira@uzi.local",
    created_at: daysAgo(4),
    updated_at: daysAgo(2),
  },
];

// Seed allocation: the builtin ci-cd-norms is shared onto the coder template
// so the allocation panel shows a populated union out of the box.
export const mockAllocations: Record<string, { shared: string[]; mine: string[] }> = {
  "t-coder": { shared: ["skill-mm-cicd"], mine: [] },
};

// ── Runs ─────────────────────────────────────────────────────────────────────

export const mockRuns: Run[] = [
  {
    id: LIVE_RUN_ID,
    repo_id: "repo-uzi",
    issue_iid: 24,
    issue_title: "Worker heartbeat metrics endpoint",
    issue_description: "Expose worker heartbeat freshness as a metrics endpoint. See prds/13-worker-metrics.md.",
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    worker_id: "w-laptop",
    branch: "agent/issue-24",
    mr_iid: null,
    failure_reason: null,
    plan_md: null,
    claimed_at: minsAgo(1),
    started_at: minsAgo(1),
    finished_at: null,
    created_at: minsAgo(2),
    updated_at: minsAgo(1),
  },
  {
    // run-awaiting is parked at the plan gate (PRD #12): it drives the board's
    // attention strip + loud card and renders the run view's plan-approval panel.
    id: "run-awaiting",
    repo_id: "repo-uzi",
    issue_iid: 21,
    issue_title: "Plan-approval notifications via email",
    issue_description: "Notify a run's owner when their plan is parked awaiting approval. See prds/9-approval-notify.md.",
    status: "awaiting_approval",
    requeue_count: 0,
    iteration_count: 0,
    worker_id: "w-laptop",
    branch: null,
    mr_iid: null,
    failure_reason: null,
    plan_md: SAMPLE_PLAN(),
    claimed_at: minsAgo(9),
    started_at: minsAgo(9),
    finished_at: null,
    created_at: minsAgo(10),
    updated_at: minsAgo(6),
  },
  {
    id: "run-done",
    repo_id: "repo-uzi",
    issue_iid: 18,
    issue_title: "Run view: fold tool results under their calls",
    issue_description: "See prds/11-run-view-ux.md.",
    status: "completed",
    requeue_count: 0,
    iteration_count: 2,
    worker_id: "w-laptop",
    branch: "agent/issue-18",
    mr_iid: 42,
    failure_reason: null,
    plan_md: SAMPLE_PLAN(),
    claimed_at: minsAgo(220),
    started_at: minsAgo(219),
    finished_at: minsAgo(184),
    created_at: minsAgo(225),
    updated_at: minsAgo(184),
  },
  {
    id: "run-failed",
    repo_id: "repo-atlas",
    issue_iid: 7,
    issue_title: "Postgres connection pool tuning",
    issue_description: "See prds/3-pool-tuning.md.",
    status: "failed",
    requeue_count: 1,
    iteration_count: 4,
    worker_id: "w-ci",
    branch: "agent/issue-7",
    mr_iid: null,
    failure_reason: "run timed out after 2h0m0s (RUN_TIMEOUT)",
    plan_md: null,
    claimed_at: daysAgo(1.2),
    started_at: daysAgo(1.2),
    finished_at: daysAgo(1.1),
    created_at: daysAgo(1.3),
    updated_at: daysAgo(1.1),
  },
  {
    id: "run-cancelled",
    repo_id: "repo-atlas",
    issue_iid: 5,
    issue_title: "Healthcheck should ping the DB pool",
    issue_description: "See prds/2-health.md.",
    status: "cancelled",
    requeue_count: 0,
    iteration_count: 0,
    worker_id: null,
    branch: null,
    mr_iid: null,
    failure_reason: null,
    plan_md: null,
    claimed_at: null,
    started_at: null,
    finished_at: daysAgo(3),
    created_at: daysAgo(3.1),
    updated_at: daysAgo(3),
  },
];

export function SAMPLE_PLAN(): string {
  return [
    "## Plan",
    "",
    "1. **Pair tool results to calls by id** in `web/src/components/RunEvent.tsx` — never by adjacency, so parallel calls pair correctly.",
    "2. **Fold each result under its call** with an auto-expanding error state.",
    "3. **Cap the DOM** on very long runs behind a \"show earlier\" expander.",
    "4. **Tests**: pairing (parallel calls), orphan results, cap interaction with folding.",
    "",
    "No schema or API changes. Touches `web/` only.",
  ].join("\n");
}

// runListItem decorates a Run into the list shape the API returns.
export function runListItem(r: Run, ownerEmail?: string): RunListItem {
  const repo = mockRepos.find((x) => x.id === r.repo_id);
  const worker = mockWorkers.find((w) => w.id === r.worker_id);
  return {
    ...r,
    repo_path: repo?.path_with_namespace ?? r.repo_id,
    worker_name: worker?.name ?? null,
    ...(ownerEmail ? { owner_email: ownerEmail } : {}),
  };
}

// ── Seeded message history for the completed run ─────────────────────────────

let doneSeq = 0;
const doneAt = (minAgo: number) => minsAgo(minAgo);
const dm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++doneSeq,
  kind,
  agent,
  payload,
  created_at: doneAt(minAgo),
});

export const mockDoneMessages: RunMessage[] = [
  dm("status", null, { event: "init", model: "claude-sonnet-4-6" }, 219),
  dm("text", "lead", { text: "Reading the PRD and the current run-view rendering to scope the fold-results work." }, 218),
  dm("tool_use", "lead", { id: "tu-1", name: "Read", input: { file_path: "prds/11-run-view-ux.md" } }, 218),
  dm("tool_result", "lead", { tool_use_id: "tu-1", content: "# PRD 11 — Run view UX\n\nFold tool results under their calls…" }, 218),
  dm("tool_use", "lead", { id: "tu-2", name: "Grep", input: { pattern: "tool_result", path: "web/src" } }, 217),
  dm("tool_result", "lead", { tool_use_id: "tu-2", content: "web/src/components/RunEvent.tsx:12\nweb/src/components/ActivityFeed.tsx:44" }, 217),
  dm("plan", "lead", { text: SAMPLE_PLAN() }, 216),
  dm("status", null, { text: "plan submitted — awaiting approval" }, 216),
  dm("status", null, { text: "plan approved by vlad@uzi.local" }, 205),
  dm("text", "coder", { text: "Implementing the id-based pairing index and the fold-under-call rendering." }, 204),
  dm("tool_use", "coder", { id: "tu-3", name: "Edit", input: { file_path: "web/src/components/RunEvent.tsx" } }, 203),
  dm("tool_result", "coder", { tool_use_id: "tu-3", content: "ok" }, 203),
  dm("tool_use", "coder", { id: "tu-4", name: "Bash", input: { command: "cd web && npx vitest run src/components/RunEvent.test.tsx" } }, 200),
  dm("tool_result", "coder", { tool_use_id: "tu-4", content: "✓ 14 tests passed" }, 199),
  dm("text", "reviewer", { text: "Pairing is by id, orphan results render standalone, and the cap keeps folding correct at the boundary. One nit: memoize the index. Approved after that." }, 195),
  dm("tool_use", "coder", { id: "tu-5", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 192),
  dm("tool_result", "coder", { tool_use_id: "tu-5", content: "ok" }, 192),
  dm("tool_use", "coder", { id: "tu-6", name: "Bash", input: { command: "cd web && npm run typecheck && npm test" } }, 190),
  dm("tool_result", "coder", { tool_use_id: "tu-6", content: "typecheck clean\n✓ 61 tests passed" }, 188),
  dm("status", null, { text: "pushing branch agent/issue-18 and opening the MR" }, 185),
  dm("status", null, { event: "result", subtype: "success", duration_ms: 2_100_000, num_turns: 38, total_cost_usd: 1.87 }, 184),
];

// ── Awaiting-approval history (parked at the plan gate) ───────────────────────

let awaitSeq = 0;
const am = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++awaitSeq,
  kind,
  agent,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockAwaitingMessages: RunMessage[] = [
  am("status", null, { event: "init", model: "claude-sonnet-4-6" }, 9),
  am("text", "lead", { text: "Reading the PRD for issue #21 and mapping how run-owner notifications should be delivered." }, 9),
  am("tool_use", "lead", { id: "aw-1", name: "Read", input: { file_path: "prds/9-approval-notify.md" } }, 8),
  am("tool_result", "lead", { tool_use_id: "aw-1", content: "# PRD 9 — Plan-approval notifications\n\nEmail the run owner when a plan parks…" }, 8),
  am("plan", "lead", { text: SAMPLE_PLAN() }, 6),
  am("status", null, { text: "plan submitted — awaiting approval" }, 6),
];

// ── Failed-run history (short) ───────────────────────────────────────────────

let failSeq = 0;
const fm = (kind: string, agent: string | null, payload: unknown): RunMessage => ({
  seq: ++failSeq,
  kind,
  agent,
  payload,
  created_at: daysAgo(1.15),
});

export const mockFailedMessages: RunMessage[] = [
  fm("status", null, { event: "init", model: "claude-sonnet-4-6" }),
  fm("text", "lead", { text: "Benchmarking the pool under load before proposing settings." }),
  fm("tool_use", "lead", { id: "f-1", name: "Bash", input: { command: "go test -bench=Pool ./internal/store/..." } }),
  fm("tool_result", "lead", { tool_use_id: "f-1", content: "benchmark hung — no output after 40m", is_error: true }),
  fm("error", null, { text: "run timed out after 2h0m0s (RUN_TIMEOUT)" }),
];
