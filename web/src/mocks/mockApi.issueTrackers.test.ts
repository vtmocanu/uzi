// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ApiError } from "../lib/apiError";

// The mock half of Settings → Issue trackers (PRD #4173 M6). Each case imports a fresh
// module graph so the in-memory connection list starts from the seed.

// Assembled at runtime so no token-shaped literal sits in the source.
const TOKEN = ["perm", "bW9jaw", "not-a-real-token"].join(":");

beforeEach(() => vi.resetModules());
afterEach(() => vi.resetModules());

async function load() {
  const { mockApi } = await import("./mockApi");
  const { repos } = await import("./mockApi/forge");
  return { mockApi, repos };
}

describe("mock issue tracker connections", () => {
  it("serves the default allowlist, kind and one connection", async () => {
    const { mockApi } = await load();
    expect(await mockApi.issueTrackerConfig()).toEqual({
      allowed_base_urls: ["https://youtrack.example.com"],
      kinds: ["youtrack"],
    });
    const { connections } = await mockApi.listIssueTrackerConnections();
    expect(connections).toHaveLength(1);
    expect(connections[0]).toMatchObject({ kind: "youtrack", base_url: "https://youtrack.example.com", account_login: "uzi-bot" });
    expect(Object.keys(connections[0]).sort()).toEqual(
      ["id", "kind", "base_url", "account_id", "account_login", "created_at", "last_verified_at"].sort(),
    );
  });

  it("never stores or returns the token it was given", async () => {
    const { mockApi } = await load();
    const created = await mockApi.createIssueTrackerConnection("youtrack", "https://youtrack.example.com", TOKEN);
    expect(JSON.stringify(created)).not.toContain(TOKEN);
    expect(JSON.stringify(await mockApi.listIssueTrackerConnections())).not.toContain(TOKEN);
    // Reconnecting with the same base URL replaces the token on the same row.
    const { connections } = await mockApi.listIssueTrackerConnections();
    expect(connections).toHaveLength(1);
    expect(created.connection.id).toBe(connections[0].id);
  });

  it.each([
    ["youtrack", "https://other.example.com", TOKEN, 400, "base URL is not in the allowed issue tracker list"],
    ["youtrack", "https://youtrack.example.com", "", 400, "a token is required"],
    ["jira", "https://youtrack.example.com", TOKEN, 400, "unsupported issue tracker kind"],
  ])("refuses %s %s", async (kind, baseUrl, token, status, message) => {
    const { mockApi } = await load();
    const err = await mockApi.createIssueTrackerConnection(kind, baseUrl, token).catch((e: unknown) => e);
    // A fresh module graph has its own ApiError class, so match it by name.
    expect((err as Error).name).toBe("ApiError");
    expect((err as ApiError).status).toBe(status);
    expect((err as ApiError).message).toContain(message);
  });

  it("deletes an unlinked connection and refuses one a repo is linked to", async () => {
    const { mockApi, repos } = await load();
    const [conn] = (await mockApi.listIssueTrackerConnections()).connections;
    repos[0].issue_tracker_link = {
      connection_id: conn.id,
      project_id: "0-4",
      project_short_name: "ATL",
      tag_id: "6-2",
      tag_name: "uzi",
      linked_at: new Date().toISOString(),
    };
    const err = await mockApi.deleteIssueTrackerConnection(conn.id).catch((e: unknown) => e);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).message).toBe(
      "this connection is linked to a repo and cannot be deleted while it is linked; a link cannot be removed",
    );
    repos[0].issue_tracker_link = null;
    await mockApi.deleteIssueTrackerConnection(conn.id);
    expect((await mockApi.listIssueTrackerConnections()).connections).toEqual([]);
    const gone = await mockApi.deleteIssueTrackerConnection(conn.id).catch((e: unknown) => e);
    expect((gone as ApiError).status).toBe(404);
  });

  it("lists projects and tags for a known connection", async () => {
    const { mockApi } = await load();
    const [conn] = (await mockApi.listIssueTrackerConnections()).connections;
    expect((await mockApi.listIssueTrackerProjects(conn.id)).projects.length).toBeGreaterThan(0);
    const { tags } = await mockApi.listIssueTrackerTags(conn.id);
    expect(tags.some((t) => t.account_can_apply && t.others_can_apply)).toBe(true);
    expect(tags.some((t) => !t.account_can_apply)).toBe(true);
  });
});

describe("mock issue tracker links and State mappings", () => {
  const LINKABLE = "repo-atlas-web";
  const body = (tag_id = "6-2", project_id = "0-4") => ({ connection_id: "itc-1", project_id, tag_id });

  async function refusal(p: Promise<unknown>) {
    const err = await p.catch((e: unknown) => e);
    expect((err as Error).name).toBe("ApiError");
    return err as ApiError;
  }

  it("lists one never-enabled linkable repo on the default connection", async () => {
    const { mockApi } = await load();
    const { repos } = await mockApi.listProjects("conn-1");
    const linkable = repos.filter((r) => r.issue_tracker_linkable);
    expect(linkable.map((r) => r.id)).toEqual([LINKABLE]);
    expect(linkable[0]).toMatchObject({ enabled: false, issue_tracker_link: null, connection_id: "conn-1" });
  });

  it("links it, after which the repo reads as linked and a second link is refused", async () => {
    const { mockApi } = await load();
    const { link } = await mockApi.linkRepoToIssueTracker(LINKABLE, body());
    expect(link).toMatchObject({
      repo_id: LINKABLE,
      connection_id: "itc-1",
      project_id: "0-4",
      project_short_name: "ATL",
      tag_id: "6-2",
      tag_name: "uzi",
    });
    const repo = (await mockApi.listProjects("conn-1")).repos.find((r) => r.id === LINKABLE)!;
    expect(repo.issue_tracker_linkable).toBe(false);
    expect(repo.issue_tracker_link).toMatchObject({ project_short_name: "ATL", tag_name: "uzi" });
    // Like the server, the board opens only at the first enable (refuseNeverEnabled).
    const neverEnabled = await refusal(mockApi.getBoard(LINKABLE));
    expect(neverEnabled.status).toBe(409);
    expect(neverEnabled.message).toBe("the repo has never been enabled; enable it first");
    await mockApi.setRepoEnabled(LINKABLE, true);
    const { board } = await mockApi.getBoard(LINKABLE);
    expect(board.eligibility_label).toBe("uzi");
    expect(board.columns.map((c) => c.label_name)).toEqual(["Ready", "In progress", "Human Review"]);

    const err = await refusal(mockApi.linkRepoToIssueTracker(LINKABLE, body()));
    expect(err.status).toBe(409);
    expect(err.message).toBe("this repo is already linked to an issue tracker project; a link cannot be changed");
    // A linked connection cannot be deleted.
    expect((await refusal(mockApi.deleteIssueTrackerConnection("itc-1"))).status).toBe(409);
  });

  it.each([
    [
      "a repo enabled before",
      "repo-uzi",
      body(),
      409,
      "only a repo that has never been enabled can be linked to an issue tracker; this one was enabled before",
    ],
    [
      "an unknown project",
      LINKABLE,
      body("6-2", "0-99"),
      400,
      "project not found among the projects this connection can see",
    ],
    [
      "a tag the account cannot apply",
      LINKABLE,
      body("6-9"),
      409,
      "the connection's account cannot apply this tag; in the issue tracker, share the tag's use settings with this account, or with All Users, or pick a tag the account owns",
    ],
    [
      "a tag nobody else can apply",
      LINKABLE,
      body("6-12"),
      409,
      "nobody other than the connection's account can apply this tag; in the issue tracker, share the tag's use settings with named users or with All Users. This only checks who can apply the tag; it does not show who can edit issues in the project",
    ],
  ])("refuses %s with the server's text", async (_name, repoId, req, status, message) => {
    const { mockApi } = await load();
    const err = await refusal(mockApi.linkRepoToIssueTracker(repoId, req));
    expect(err.status).toBe(status);
    expect(err.message).toBe(message);
  });

  it("opens a plain forge board at the first enable of an unlinked repo", async () => {
    const { mockApi } = await load();
    expect((await refusal(mockApi.getBoard(LINKABLE))).status).toBe(409);
    await mockApi.setRepoEnabled(LINKABLE, true);
    const { board } = await mockApi.getBoard(LINKABLE);
    expect(board).toMatchObject({ repo_id: LINKABLE, path_with_namespace: "vtmocanu/atlas-web", cards: [] });
    expect(board.columns.map((c) => c.label_name)).toEqual(["Ready", "In progress", "Review"]);
    expect(board.eligibility_label).toBeUndefined();
  });

  it("refuses to link once the repo was enabled", async () => {
    const { mockApi } = await load();
    await mockApi.setRepoEnabled(LINKABLE, true);
    await mockApi.setRepoEnabled(LINKABLE, false);
    const err = await refusal(mockApi.linkRepoToIssueTracker(LINKABLE, body()));
    expect(err.message).toBe(
      "only a repo that has never been enabled can be linked to an issue tracker; this one was enabled before",
    );
  });

  it("serves States and saves a valid mapping", async () => {
    const { mockApi } = await load();
    await mockApi.linkRepoToIssueTracker(LINKABLE, body());
    // Column States need the board, which the first enable opens.
    await mockApi.setRepoEnabled(LINKABLE, true);
    const view = await mockApi.getIssueTrackerStates(LINKABLE);
    expect(view.mapping).toBeNull();
    expect(view.states.filter((s) => s.closable).map((s) => s.name)).toEqual(["Fixed"]);
    expect(view.states.filter((s) => s.openable).map((s) => s.name)).toEqual(["Submitted", "Open", "In Progress", "Review"]);

    const mapping = {
      open_state: "Open",
      closed_state: "Fixed",
      columns: [
        { label_name: "Ready", state: "Submitted" },
        { label_name: "In progress", state: "In Progress" },
        { label_name: "Human Review", state: "Review" },
      ],
    };
    expect(await mockApi.putIssueTrackerStates(LINKABLE, mapping)).toEqual({ mapping });
    expect((await mockApi.getIssueTrackerStates(LINKABLE)).mapping).toEqual(mapping);
  });

  it.each([
    [
      { open_state: "Open", closed_state: "Won't fix", columns: [] },
      `the state "Won't fix" cannot be the closed state: it must be a resolved state that means done (for example Fixed or Done), never one such as Won't fix`,
    ],
    [{ open_state: "Open", closed_state: "Open", columns: [] }, "the open and the closed state must be different states"],
    [
      { open_state: "Open", closed_state: "Fixed", columns: [{ label_name: "Nope", state: "Review" }] },
      `"Nope" is not a board column of this repo`,
    ],
    [
      { open_state: "Open", closed_state: "Fixed", columns: [{ label_name: "Ready", state: "Open" }] },
      `the state "Open" is mapped more than once; the open state, the closed state and every column need different states`,
    ],
    [
      { open_state: "Fixed", closed_state: "Open", columns: [] },
      `the state "Fixed" cannot be the open state: it must be an unresolved, unarchived state`,
    ],
  ])("refuses an invalid mapping %#", async (mapping, message) => {
    const { mockApi } = await load();
    await mockApi.linkRepoToIssueTracker(LINKABLE, body());
    const err = await refusal(mockApi.putIssueTrackerStates(LINKABLE, mapping));
    expect(err.status).toBe(400);
    expect(err.message).toBe(message);
    expect((await mockApi.getIssueTrackerStates(LINKABLE)).mapping).toBeNull();
  });

  it("refuses States on a repo that is not linked", async () => {
    const { mockApi } = await load();
    for (const p of [
      mockApi.getIssueTrackerStates(LINKABLE),
      mockApi.putIssueTrackerStates(LINKABLE, { open_state: "Open", closed_state: "Fixed", columns: [] }),
    ]) {
      const err = await refusal(p);
      expect(err.status).toBe(409);
      expect(err.message).toBe("this repo is not linked to an issue tracker project");
    }
  });
});
