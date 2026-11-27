// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiError } from "../lib/apiError";

// The issue-tracker-* demo scenarios (PRD #4173 M8): each one seeds what one M6 screen
// needs, so the screen can be seen without a YouTrack. Every case loads a fresh module
// graph with ?mock=<name> on the URL, as a page load in mock mode does.

const LINKABLE = "repo-atlas-web";
const LINKED = "repo-atlas-mobile";
const NEVER_ENABLED = "the repo has never been enabled; enable it first";

async function load(scenario: string) {
  window.history.replaceState(null, "", scenario ? `/?mock=${scenario}` : "/");
  vi.resetModules();
  const { mockApi } = await import("./mockApi");
  return mockApi;
}

async function refusal(p: Promise<unknown>) {
  const err = await p.catch((e: unknown) => e);
  // A fresh module graph has its own ApiError class, so match it by name.
  expect((err as Error).name).toBe("ApiError");
  return err as ApiError;
}

const goodLink = { connection_id: "itc-1", project_id: "0-4", tag_id: "6-2" };

afterEach(() => {
  window.history.replaceState(null, "", "/");
  vi.resetModules();
});

describe("issue tracker demo scenarios", () => {
  it("issue-tracker-none: no connection, so the link dialog says to connect one first", async () => {
    const api = await load("issue-tracker-none");
    expect((await api.issueTrackerConfig()).allowed_base_urls).toEqual(["https://youtrack.example.com"]);
    expect((await api.listIssueTrackerConnections()).connections).toEqual([]);
    const { repos } = await api.listProjects("conn-1");
    expect(repos.filter((r) => r.issue_tracker_linkable).map((r) => r.id)).toEqual([LINKABLE]);
    expect(repos.some((r) => r.issue_tracker_link)).toBe(false);
  });

  it("default scenario: a repo setter's response after the first enable is not linkable", async () => {
    const api = await load("");
    await api.setRepoEnabled(LINKABLE, true);
    const { repo } = await api.setRepoSkillsEnabled(LINKABLE, true);
    expect(repo.issue_tracker_linkable).toBe(false);
  });

  it("issue-tracker-allowlist-empty: no allowed base URL and no connection, so no repo offers the link control", async () => {
    const api = await load("issue-tracker-allowlist-empty");
    expect((await api.issueTrackerConfig()).allowed_base_urls).toEqual([]);
    expect((await api.listIssueTrackerConnections()).connections).toEqual([]);
    const { repos } = await api.listProjects("conn-1");
    expect(repos.filter((r) => r.issue_tracker_linkable)).toEqual([]);
    expect(repos.some((r) => r.issue_tracker_link)).toBe(false);
  });

  it.each(["", "issue-tracker-unlinked", "health-degraded"])(
    "issue-tracker-unlinked (also %j): a connection and a never-enabled repo without a board",
    async (scenario) => {
      const api = await load(scenario);
      const { connections } = await api.listIssueTrackerConnections();
      expect(connections.map((c) => [c.kind, c.base_url, c.account_login])).toEqual([
        ["youtrack", "https://youtrack.example.com", "uzi-bot"],
      ]);
      const { repos } = await api.listProjects("conn-1");
      const linkable = repos.filter((r) => r.issue_tracker_linkable);
      expect(linkable.map((r) => [r.id, r.enabled, r.issue_tracker_link])).toEqual([[LINKABLE, false, null]]);
      expect(repos.some((r) => r.issue_tracker_link)).toBe(false);
      const err = await refusal(api.getBoard(LINKABLE));
      expect([err.status, err.message]).toEqual([409, NEVER_ENABLED]);
    },
  );

  it("issue-tracker-linked: an enabled linked repo, fully mapped, with keyed cards, issue and run", async () => {
    const api = await load("issue-tracker-linked");
    const { repos } = await api.listProjects("conn-1");
    const linked = repos.find((r) => r.id === LINKED)!;
    expect(linked).toMatchObject({
      enabled: true,
      issue_tracker_linkable: false,
      issue_tracker_link: { connection_id: "itc-1", project_short_name: "ATL", tag_name: "uzi" },
    });

    const { board } = await api.getBoard(LINKED);
    expect(board.eligibility_label).toBe("uzi");
    const columns = board.columns.map((c) => c.label_name);
    expect(columns).toEqual(["Ready", "In progress", "Human Review"]);
    expect(board.cards.length).toBeGreaterThan(0);
    for (const c of board.cards) {
      expect(c.issue_key).toBe(`ATL-${c.iid}`);
      expect(c.web_url).toBe(`https://youtrack.example.com/issue/ATL-${c.iid}`);
    }
    // Every column holds a card, one open card is not tagged uzi, and one card is closed.
    for (const col of columns) expect(board.cards.some((c) => c.column === col)).toBe(true);
    expect(board.cards.some((c) => !c.closed && !c.labels.includes("uzi"))).toBe(true);
    expect(board.cards.some((c) => c.closed)).toBe(true);

    const view = await api.getIssueTrackerStates(LINKED);
    expect(view.mapping?.columns.map((c) => c.label_name)).toEqual(columns);
    expect(view.mapping).toMatchObject({ open_state: "Open", closed_state: "Fixed" });

    const { issue } = await api.getIssue(LINKED, 23);
    expect(issue).toMatchObject({ issue_key: "ATL-23", eligibility_label: "uzi" });

    const { runs } = await api.listRuns();
    const run = runs.find((r) => r.repo_id === LINKED);
    expect(run).toMatchObject({ issue_iid: 23, issue_key: "ATL-23", repo_path: "vtmocanu/atlas-mobile" });
    expect((await api.listRuns({ repoId: LINKED, issueIid: 23 })).runs.map((r) => r.id)).toEqual([run!.id]);
    // The ATL-23 card shows that run as its latest run.
    expect(board.cards.find((c) => c.iid === 23)?.latest_run).toMatchObject({ id: run!.id, status: run!.status });
    expect(board.cards.filter((c) => c.latest_run).map((c) => c.iid)).toEqual([23]);
    // Unlinked repos' runs and issues carry no key.
    expect(runs.filter((r) => r.repo_id !== LINKED).some((r) => r.issue_key)).toBe(false);
  });

  it("issue-tracker-unmapped: the linked repo has one column without a State", async () => {
    const api = await load("issue-tracker-unmapped");
    const { board } = await api.getBoard(LINKED);
    const view = await api.getIssueTrackerStates(LINKED);
    const mapped = new Set(view.mapping?.columns.map((c) => c.label_name));
    const unmapped = board.columns.map((c) => c.label_name).filter((l) => !mapped.has(l));
    expect(unmapped).toEqual(["Human Review"]);
    // No issue sits in a column no State maps to.
    expect(board.cards.some((c) => c.column === "Human Review")).toBe(false);
    expect(board.cards.every((c) => c.issue_key === `ATL-${c.iid}`)).toBe(true);
  });

  it("issue-tracker-link-refused: a valid link is refused with the server's text", async () => {
    const api = await load("issue-tracker-link-refused");
    const err = await refusal(api.linkRepoToIssueTracker(LINKABLE, goodLink));
    expect([err.status, err.message]).toEqual([
      409,
      "this repo already stores issue numbers and cannot be linked to an issue tracker",
    ]);
    const repo = (await api.listProjects("conn-1")).repos.find((r) => r.id === LINKABLE)!;
    expect(repo).toMatchObject({ issue_tracker_link: null, issue_tracker_linkable: true });
  });

  it("issue-tracker-state-machine: the link and the linked repo's states are refused with the server's text", async () => {
    const api = await load("issue-tracker-state-machine");
    const reason =
      "project ATL uses a state-machine workflow on its State field; uzi needs a plain State field. Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack.";
    const err = await refusal(api.linkRepoToIssueTracker(LINKABLE, goodLink));
    expect([err.status, err.message]).toEqual([409, reason]);
    const repos = (await api.listProjects("conn-1")).repos;
    expect(repos.find((r) => r.id === LINKABLE)).toMatchObject({ issue_tracker_link: null, issue_tracker_linkable: true });
    expect(repos.find((r) => r.id === LINKED)?.issue_tracker_link?.project_short_name).toBe("ATL");
    const states = await refusal(api.getIssueTrackerStates(LINKED));
    expect([states.status, states.message]).toEqual([409, reason]);
    // Only ATL's State field is a state machine: a link to OPS goes through.
    const { link } = await api.linkRepoToIssueTracker(LINKABLE, { ...goodLink, project_id: "0-7" });
    expect(link.project_short_name).toBe("OPS");
  });

  it("issue-tracker-allowlist-empty: no allowed base URL, no connection, and connecting is refused", async () => {
    const api = await load("issue-tracker-allowlist-empty");
    expect(await api.issueTrackerConfig()).toEqual({ allowed_base_urls: [], kinds: ["youtrack"] });
    expect((await api.listIssueTrackerConnections()).connections).toEqual([]);
    const token = ["perm", "bW9jaw", "not-a-real-token"].join(":");
    const err = await refusal(api.createIssueTrackerConnection("youtrack", "https://youtrack.example.com", token));
    expect([err.status, err.message]).toEqual([400, "base URL is not in the allowed issue tracker list"]);
  });

  it("only the linked scenarios seed the linked repo", async () => {
    for (const scenario of ["issue-tracker-none", "issue-tracker-unlinked", "issue-tracker-link-refused"]) {
      const api = await load(scenario);
      const { repos } = await api.listProjects("conn-1");
      expect(repos.some((r) => r.id === LINKED)).toBe(false);
      expect((await refusal(api.getBoard(LINKED))).status).toBe(404);
    }
  });

  it("keeps every scenario's fixtures free of tokens and real hosts", async () => {
    for (const scenario of ["issue-tracker-linked", "issue-tracker-unmapped"]) {
      const api = await load(scenario);
      const text = JSON.stringify([
        await api.listIssueTrackerConnections(),
        await api.getBoard(LINKED),
        await api.getIssueTrackerStates(LINKED),
        (await api.listProjects("conn-1")).repos.filter((r) => r.issue_tracker_link || r.issue_tracker_linkable),
        (await api.listRuns()).runs.filter((r) => r.issue_key),
      ]);
      // YouTrack permanent tokens start with perm: or perm-.
      expect(text).not.toMatch(/perm[:-]/i);
      for (const host of text.match(/https:\/\/[^/"]+/g) ?? []) expect(host).toMatch(/\.example\.com$/);
    }
  });
});
