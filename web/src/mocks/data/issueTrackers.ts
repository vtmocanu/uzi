import type {
  Board,
  Card,
  IssueTrackerConfig,
  IssueTrackerConnection,
  IssueTrackerProject,
  IssueTrackerState,
  IssueTrackerStateMapping,
  IssueTrackerTag,
  Repo,
} from "../../lib/api";
import { issueKey } from "../../lib/issueTrackerText";
import { daysAgo, minsAgo } from "./time";

// ── Issue trackers (PRD #4173 M6) ─────────────────────────────────────────────
// Seed data for Settings → Issue trackers. No token anywhere: a connection has no
// token field, and the create mock never stores the one it is given.

export const mockIssueTrackerConfig: IssueTrackerConfig = {
  allowed_base_urls: ["https://youtrack.example.com"],
  kinds: ["youtrack"],
};

export const mockIssueTrackerConnection: IssueTrackerConnection = {
  id: "itc-1",
  kind: "youtrack",
  base_url: "https://youtrack.example.com",
  account_id: "1-17",
  account_login: "uzi-bot",
  created_at: daysAgo(12),
  last_verified_at: minsAgo(35),
};

export const mockIssueTrackerProjects: IssueTrackerProject[] = [
  { id: "0-4", short_name: "ATL", name: "Atlas" },
  { id: "0-7", short_name: "OPS", name: "Operations" },
];

// One tag a link can use (the account can apply it and so can everyone), one a link
// must refuse because the account cannot apply it (another user owns it and shares it
// with nobody), and one it must refuse because nobody else can (the account owns it,
// and its only group, a named one with one member, does not count).
export const mockIssueTrackerTags: IssueTrackerTag[] = [
  {
    id: "6-2",
    name: "uzi",
    owner: { id: "1-17", login: "uzi-bot" },
    read: { groups: [{ id: "3-0", name: "All Users", all_users: true, users_count: 0 }], users: [] },
    use: { groups: [{ id: "3-0", name: "All Users", all_users: true, users_count: 0 }], users: [] },
    account_can_apply: true,
    others_can_apply: true,
  },
  {
    id: "6-9",
    name: "triage",
    owner: { id: "1-3", login: "dana" },
    read: { groups: [], users: [{ id: "1-17", login: "uzi-bot" }] },
    use: { groups: [], users: [] },
    account_can_apply: false,
    others_can_apply: false,
  },
  {
    id: "6-12",
    name: "bot-only",
    owner: { id: "1-17", login: "uzi-bot" },
    read: { groups: [{ id: "3-4", name: "Release crew", all_users: false, users_count: 1 }], users: [] },
    use: { groups: [{ id: "3-4", name: "Release crew", all_users: false, users_count: 1 }], users: [] },
    account_can_apply: true,
    others_can_apply: false,
  },
];

// The never-enabled repo the default mock offers "Link to an issue tracker" on. It is
// listed by the default forge connection's project list (as ListProjects lists a
// never-enabled repo) and, until it is linked or enabled, is linkable.
export const mockLinkableRepo: Repo = {
  id: "repo-atlas-web",
  connection_id: "conn-1",
  forge_project_id: 702,
  path_with_namespace: "vtmocanu/atlas-web",
  web_url: "https://gitlab.example.com/vtmocanu/atlas-web",
  default_branch: "main",
  enabled: false,
  repo_skills_enabled: false,
  repo_claudemd_enabled: false,
  repo_devbox_opt_in: false,
  repo_fold_improve_uzi_backlog: false,
  pipeline: null,
  guardrail_override: null,
  guardrail_blocked: false,
  docker_allowlisted: false,
  docker_blocked: false,
  env_var_count: 0,
  issue_tracker_link: null,
  issue_tracker_linkable: false,
};

// The board columns a linked repo's board gets in mock mode at its first enable.
export const mockLinkedBoardColumns = ["Ready", "In progress", "Human Review"];

// The linked project's States. Only Fixed means done, so only it may be the closed
// State; Won't fix and Duplicate are resolved but never closable. Four unresolved
// States, so the open State and three columns can each take a different one.
export const mockIssueTrackerStates: IssueTrackerState[] = [
  { name: "Submitted", resolved: false, archived: false, closable: false, openable: true },
  { name: "Open", resolved: false, archived: false, closable: false, openable: true },
  { name: "In Progress", resolved: false, archived: false, closable: false, openable: true },
  { name: "Review", resolved: false, archived: false, closable: false, openable: true },
  { name: "Fixed", resolved: true, archived: false, closable: true, openable: false },
  { name: "Won't fix", resolved: true, archived: false, closable: false, openable: false },
  { name: "Duplicate", resolved: true, archived: false, closable: false, openable: false },
];

// ── Issue tracker demo scenarios (PRD #4173 M8) ───────────────────────────────
// ?mock=<name> picks one, so every M6 screen can be seen without a YouTrack. Any other
// value (or none) is issue-tracker-unlinked, the default demo. See docs/dev-conventions.md
// "The mock/demo build" for what each one shows and where.
export const ISSUE_TRACKER_SCENARIOS = [
  "issue-tracker-none",
  "issue-tracker-unlinked",
  "issue-tracker-linked",
  "issue-tracker-unmapped",
  "issue-tracker-link-refused",
  "issue-tracker-state-machine",
  "issue-tracker-allowlist-empty",
] as const;
export type IssueTrackerScenario = (typeof ISSUE_TRACKER_SCENARIOS)[number];

export function issueTrackerScenario(name: string): IssueTrackerScenario {
  return (ISSUE_TRACKER_SCENARIOS as readonly string[]).includes(name)
    ? (name as IssueTrackerScenario)
    : "issue-tracker-unlinked";
}

// Whether the scenario seeds the enabled linked repo (its board, State mapping and run).
export function seedsLinkedRepo(s: IssueTrackerScenario): boolean {
  return s === "issue-tracker-linked" || s === "issue-tracker-unmapped" || s === "issue-tracker-state-machine";
}

// The refusal issue-tracker-link-refused answers every otherwise valid link with: one
// the dialog cannot foresee, so the server's text is what the admin reads.
export const mockLinkRefusal = "this repo already stores issue numbers and cannot be linked to an issue tracker";

// The refusal issue-tracker-state-machine answers a link with, and the linked repo's
// states read: the server's StateMachineRefusal for project ATL, whose State field a
// state machine runs.
export const mockStateMachineProject = "ATL";
export const mockStateMachineRefusal = `project ${mockStateMachineProject} uses a state-machine workflow on its State field; uzi needs a plain State field. Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack.`;

// The enabled repo linked to project ATL with tag uzi (issue-tracker-linked,
// issue-tracker-unmapped and issue-tracker-state-machine).
export const mockLinkedRepo: Repo = {
  ...mockLinkableRepo,
  id: "repo-atlas-mobile",
  forge_project_id: 703,
  path_with_namespace: "vtmocanu/atlas-mobile",
  web_url: "https://gitlab.example.com/vtmocanu/atlas-mobile",
  enabled: true,
  issue_tracker_link: {
    connection_id: mockIssueTrackerConnection.id,
    project_id: "0-4",
    project_short_name: "ATL",
    tag_id: "6-2",
    tag_name: "uzi",
    linked_at: daysAgo(6),
  },
  issue_tracker_linkable: false,
};

// The complete mapping: the open State, the closed State and every board column.
export const mockLinkedMapping: IssueTrackerStateMapping = {
  open_state: "Open",
  closed_state: "Fixed",
  columns: [
    { label_name: "Ready", state: "Submitted" },
    { label_name: "In progress", state: "In Progress" },
    { label_name: "Human Review", state: "Review" },
  ],
};

// issue-tracker-unmapped: Human Review has no State, so a move into it is refused.
export const mockUnmappedMapping: IssueTrackerStateMapping = {
  ...mockLinkedMapping,
  columns: mockLinkedMapping.columns.filter((c) => c.label_name !== "Human Review"),
};

// The linked project's issues, each with its YouTrack State; the board column follows
// from the mapping, as the server derives it. Only the issues tagged uzi are uzi's.
const linkedIssues: { n: number; title: string; tags: string[]; ytState: string; mins: number }[] = [
  { n: 21, title: "Offline cache for the route planner", tags: ["uzi"], ytState: "Open", mins: 90 },
  { n: 22, title: "Push notification opt-in screen", tags: ["uzi", "UX"], ytState: "Submitted", mins: 45 },
  { n: 23, title: "Retry failed photo uploads in the background", tags: ["uzi"], ytState: "In Progress", mins: 20 },
  { n: 24, title: "Dark map tiles for night driving", tags: ["uzi"], ytState: "Review", mins: 8 },
  { n: 25, title: "Crash on rotating the trip summary", tags: ["Bug"], ytState: "Open", mins: 300 },
  { n: 19, title: "Localise the onboarding copy", tags: ["uzi"], ytState: "Fixed", mins: 2880 },
];

// The run the linked scenarios list for ATL-23.
export const mockLinkedRunId = "run-issue-tracker-linked";
export const mockLinkedRunIssue = linkedIssues[2];

function linkedCards(repo: Repo, mapping: IssueTrackerStateMapping): Card[] {
  const short = repo.issue_tracker_link?.project_short_name ?? "ATL";
  return linkedIssues.map((i) => {
    const closed = i.ytState === mapping.closed_state;
    return {
      iid: i.n,
      issue_key: issueKey(short, i.n),
      title: i.title,
      state: closed ? "closed" : "opened",
      labels: [...i.tags],
      web_url: `${mockIssueTrackerConnection.base_url}/issue/${issueKey(short, i.n)}`,
      author: "mira",
      forge_type: "gitlab",
      has_prd_link: false,
      column: closed ? "" : (mapping.columns.find((c) => c.state === i.ytState)?.label_name ?? ""),
      closed,
      conflict: false,
      assignee_ids: [],
      forge_updated_at: minsAgo(i.mins),
      latest_run: null,
      pipeline: null,
    };
  });
}

// The board an unlinked repo opens at its first enable: a plain forge board with the
// columns the other demo repos use, no cards until an issue is imported.
export function mockForgeBoard(repo: Repo): Board {
  return {
    repo_id: repo.id,
    path_with_namespace: repo.path_with_namespace,
    web_url: repo.web_url,
    forge_type: "gitlab",
    initial_import: null,
    columns: ["Ready", "In progress", "Review"].map((label_name, position) => ({ label_name, position })),
    cards: [],
    pipeline: null,
  };
}

// The board of a linked repo. A first enable gets the columns and no cards yet; the
// seeded scenarios pass their mapping to place the project's issues.
export function mockLinkedBoard(repo: Repo, mapping?: IssueTrackerStateMapping): Board {
  return {
    repo_id: repo.id,
    path_with_namespace: repo.path_with_namespace,
    web_url: repo.web_url,
    forge_type: "gitlab",
    initial_import: null,
    columns: mockLinkedBoardColumns.map((label_name, position) => ({ label_name, position })),
    cards: mapping ? linkedCards(repo, mapping) : [],
    pipeline: null,
    eligibility_label: repo.issue_tracker_link?.tag_name ?? "",
  };
}
