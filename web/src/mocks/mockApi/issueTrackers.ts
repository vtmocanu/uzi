// Issue tracker connections, repo links and State mappings in mock mode (PRD #4173
// M6): Settings → Issue trackers, the Repos page's link dialog and the States panel.
// Mirrors the server's refusals closely enough to drive the screens: the allowlist,
// a missing token, an unsupported kind, delete while a repo is linked, an unknown id;
// a link on a repo that was enabled before or is already linked, an unknown project
// or tag and the two tag verdicts; and the State mapping checks. The token argument is
// checked for presence and then dropped: nothing here stores, returns or logs it.
//
// The data lives in ../data/issueTrackers.ts (connection, projects, tags, States, the
// linkable and linked repos). The issue-tracker-* scenarios (PRD #4173 M8, see
// issueTrackerScenario) pick what is seeded:
//
//   ?mock=issue-tracker-none           → no connection
//   ?mock=issue-tracker-unlinked       → one connection and the never-enabled linkable
//                                        repo (also the default)
//   ?mock=issue-tracker-linked         → plus an enabled linked repo, completely mapped,
//                                        with keyed cards and a keyed run
//   ?mock=issue-tracker-unmapped       → the same repo with one column left unmapped
//   ?mock=issue-tracker-link-refused   → a valid link is refused (mockLinkRefusal)
//   ?mock=issue-tracker-state-machine  → a link is refused because project ATL's State field
//                                        is run by a state machine; the linked repo's states
//                                        read is refused with the same text
//                                        (mockStateMachineRefusal)
//   ?mock=issue-tracker-allowlist-empty → no allowed base URL and no connection

import type {
  IssueTrackerConnection,
  IssueTrackerLinkRequest,
  IssueTrackerStateMapping,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import {
  issueTrackerScenario,
  mockIssueTrackerConfig,
  mockIssueTrackerConnection,
  mockIssueTrackerProjects,
  mockIssueTrackerStates,
  mockIssueTrackerTags,
  mockLinkedMapping,
  mockLinkedRepo,
  mockLinkRefusal,
  mockStateMachineProject,
  mockStateMachineRefusal,
  mockUnmappedMapping,
} from "../data";
import { state } from "../store";
import { awaitingFirstEnable, repos, setIssueTrackerLinkOffered } from "./forge";
import { delay, mockScenario, requireSession } from "./shared";

const scenario = issueTrackerScenario(mockScenario());

// The saved State mapping of each linked repo, keyed by repo id; absent until saved.
const stateMappings = new Map<string, IssueTrackerStateMapping>();
if (scenario === "issue-tracker-linked" || scenario === "issue-tracker-state-machine") stateMappings.set(mockLinkedRepo.id, cloneMapping(mockLinkedMapping));
if (scenario === "issue-tracker-unmapped") stateMappings.set(mockLinkedRepo.id, cloneMapping(mockUnmappedMapping));

const config = {
  allowed_base_urls: scenario === "issue-tracker-allowlist-empty" ? [] : [...mockIssueTrackerConfig.allowed_base_urls],
  kinds: [...mockIssueTrackerConfig.kinds],
};

function findRepo(id: string) {
  const r = repos.find((x) => x.id === id);
  if (!r) throw new ApiError(404, "repo not found");
  return r;
}

function linkedRepo(id: string) {
  const r = findRepo(id);
  if (!r.issue_tracker_link) throw new ApiError(409, "this repo is not linked to an issue tracker project");
  return r;
}

function cloneMapping(m: IssueTrackerStateMapping): IssueTrackerStateMapping {
  return { ...m, columns: m.columns.map((c) => ({ ...c })) };
}

// checkMapping mirrors the server's validateMappingShape and checkMappingStates (the
// project's States are mockIssueTrackerStates) and its board column check, in the
// server's order.
function checkMapping(m: IssueTrackerStateMapping, columns: string[]) {
  const bad = (msg: string) => new ApiError(400, msg);
  const used = new Set([m.open_state]);
  if (used.has(m.closed_state)) throw bad("the open and the closed state must be different states");
  used.add(m.closed_state);
  const labels = new Set<string>();
  for (const c of m.columns) {
    if (labels.has(c.label_name)) throw bad("a column is mapped more than once");
    labels.add(c.label_name);
    if (used.has(c.state)) {
      throw bad(
        `the state ${JSON.stringify(c.state)} is mapped more than once; the open state, the closed state and every column need different states`,
      );
    }
    used.add(c.state);
  }
  const find = (name: string) => {
    const st = mockIssueTrackerStates.find((x) => x.name === name);
    if (!st) throw bad(`the state ${JSON.stringify(name)} is not a state of the linked project`);
    return st;
  };
  if (!find(m.open_state).openable) {
    throw bad(`the state ${JSON.stringify(m.open_state)} cannot be the open state: it must be an unresolved, unarchived state`);
  }
  if (!find(m.closed_state).closable) {
    throw bad(
      `the state ${JSON.stringify(m.closed_state)} cannot be the closed state: it must be a resolved state that means done (for example Fixed or Done), never one such as Won't fix`,
    );
  }
  for (const c of m.columns) {
    if (!find(c.state).openable) {
      throw bad(`the state ${JSON.stringify(c.state)} cannot be a column state: it must be an unresolved, unarchived state`);
    }
  }
  for (const c of m.columns) {
    if (!columns.includes(c.label_name)) throw bad(`${JSON.stringify(c.label_name)} is not a board column of this repo`);
  }
}

let connections: IssueTrackerConnection[] =
  scenario === "issue-tracker-none" || scenario === "issue-tracker-allowlist-empty"
    ? []
    : [{ ...mockIssueTrackerConnection }];

setIssueTrackerLinkOffered(() => config.allowed_base_urls.length > 0 || connections.length > 0);

function findConnection(id: string): IssueTrackerConnection {
  const c = connections.find((x) => x.id === id);
  if (!c) throw new ApiError(404, "connection not found");
  return c;
}

export const issueTrackersApi = {
  issueTrackerConfig: async () => {
    requireSession();
    return delay({ allowed_base_urls: [...config.allowed_base_urls], kinds: [...config.kinds] });
  },
  listIssueTrackerConnections: async () => {
    requireSession();
    return delay({ connections: connections.map((c) => ({ ...c })) });
  },
  createIssueTrackerConnection: async (kind: string, baseUrl: string, token: string) => {
    requireSession();
    if (!config.kinds.includes(kind)) {
      throw new ApiError(400, "unsupported issue tracker kind; supported values: youtrack");
    }
    if (token.trim() === "") throw new ApiError(400, "a token is required");
    if (!config.allowed_base_urls.includes(baseUrl)) {
      throw new ApiError(400, "base URL is not in the allowed issue tracker list");
    }
    const now = new Date().toISOString();
    // Reconnecting with the same base URL replaces the token on the existing row.
    const existing = connections.find((c) => c.kind === kind && c.base_url === baseUrl);
    if (existing) {
      existing.last_verified_at = now;
      return delay({ connection: { ...existing } }, 500);
    }
    const conn: IssueTrackerConnection = {
      ...mockIssueTrackerConnection,
      id: `itc-${Date.now()}`,
      kind,
      base_url: baseUrl,
      created_at: now,
      last_verified_at: now,
    };
    connections = [...connections, conn];
    return delay({ connection: { ...conn } }, 500);
  },
  deleteIssueTrackerConnection: async (id: string) => {
    requireSession();
    findConnection(id);
    if (repos.some((r) => r.issue_tracker_link?.connection_id === id)) {
      throw new ApiError(
        409,
        "this connection is linked to a repo and cannot be deleted while it is linked; a link cannot be removed",
      );
    }
    connections = connections.filter((c) => c.id !== id);
    return delay(null);
  },
  listIssueTrackerProjects: async (id: string) => {
    requireSession();
    findConnection(id);
    return delay({ projects: mockIssueTrackerProjects.map((p) => ({ ...p })) }, 350);
  },
  listIssueTrackerTags: async (id: string) => {
    requireSession();
    findConnection(id);
    return delay({ tags: structuredClone(mockIssueTrackerTags) }, 350);
  },
  linkRepoToIssueTracker: async (repoId: string, body: IssueTrackerLinkRequest) => {
    requireSession();
    const r = findRepo(repoId);
    if (r.issue_tracker_link) {
      throw new ApiError(409, "this repo is already linked to an issue tracker project; a link cannot be changed");
    }
    // The server refuses by its own rule (never enabled), not by the listing flag, which
    // also folds in whether the link control is offered.
    if (!awaitingFirstEnable.has(r.id)) {
      throw new ApiError(
        409,
        "only a repo that has never been enabled can be linked to an issue tracker; this one was enabled before",
      );
    }
    findConnection(body.connection_id);
    const project = mockIssueTrackerProjects.find((p) => p.id === body.project_id);
    if (!project) throw new ApiError(400, "project not found among the projects this connection can see");
    const tag = mockIssueTrackerTags.find((t) => t.id === body.tag_id);
    if (!tag) throw new ApiError(400, "tag not found among the tags this connection's account can see");
    if (!tag.account_can_apply) {
      throw new ApiError(
        409,
        "the connection's account cannot apply this tag; in the issue tracker, share the tag's use settings with this account, or with All Users, or pick a tag the account owns",
      );
    }
    if (!tag.others_can_apply) {
      throw new ApiError(
        409,
        "nobody other than the connection's account can apply this tag; in the issue tracker, share the tag's use settings with named users or with All Users. This only checks who can apply the tag; it does not show who can edit issues in the project",
      );
    }
    // The server's last check, under the repo lock: a repo whose stored rows already
    // carry forge issue numbers cannot take issue tracker ones.
    if (scenario === "issue-tracker-link-refused") throw new ApiError(409, mockLinkRefusal);
    // Only ATL's State field is run by a state machine; another project links as usual.
    if (scenario === "issue-tracker-state-machine" && project.short_name === mockStateMachineProject) {
      throw new ApiError(409, mockStateMachineRefusal);
    }
    const link = {
      connection_id: body.connection_id,
      project_id: project.id,
      project_short_name: project.short_name,
      tag_id: tag.id,
      tag_name: tag.name,
      linked_at: new Date().toISOString(),
    };
    r.issue_tracker_link = link;
    // No board yet: the repo still awaits its first enable, which opens it (forge.ts).
    return delay({ link: { repo_id: r.id, ...link } }, 500);
  },
  getIssueTrackerStates: async (repoId: string) => {
    requireSession();
    const r = linkedRepo(repoId);
    if (scenario === "issue-tracker-state-machine") throw new ApiError(409, mockStateMachineRefusal);
    const saved = stateMappings.get(r.id);
    return delay(
      {
        mapping: saved ? cloneMapping(saved) : null,
        states: mockIssueTrackerStates.map((st) => ({ ...st })),
      },
      350,
    );
  },
  putIssueTrackerStates: async (repoId: string, mapping: IssueTrackerStateMapping) => {
    requireSession();
    const r = linkedRepo(repoId);
    const columns = (state.boards.get(r.id)?.columns ?? []).map((c) => c.label_name);
    checkMapping(mapping, columns);
    stateMappings.set(r.id, cloneMapping(mapping));
    return delay({ mapping: cloneMapping(mapping) }, 400);
  },
};
