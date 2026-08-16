// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Findings } from "./Findings";
import { api, ApiError, type IncidentalFinding, type IncidentalFindingBacklog } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listFindings: vi.fn(),
      fileFinding: vi.fn(),
      dismissFinding: vi.fn(),
      findingIssueDraft: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    },
  };
});

const mockApi = vi.mocked(api);

function finding(over: Partial<IncidentalFinding> = {}): IncidentalFinding {
  return {
    finding_id: "find-1",
    location: "api/internal/sweeper.go#sweepLoop",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "open",
    last_title: "Leaked ticker in sweepLoop",
    seen_in_runs: 2,
    ...over,
  };
}

function backlog(over: Partial<IncidentalFindingBacklog> = {}): IncidentalFindingBacklog {
  return {
    bucket: "to_file",
    repo: "",
    run: "",
    open_count: 3,
    findings: [finding()],
    ...over,
  };
}

beforeEach(() => {
  mockApi.listFindings.mockResolvedValue(backlog());
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderFindings(entries: string[] = ["/findings"]) {
  return render(
    <MemoryRouter initialEntries={entries}>
      <Findings />
    </MemoryRouter>,
  );
}

describe("Findings page — repo grouping (PRD #333 M7, D3)", () => {
  it("groups rows under a repo_path header in the All-repos view", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        findings: [
          finding({ finding_id: "f-a", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", last_title: "uzi bug" }),
          finding({ finding_id: "f-b", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api", last_title: "atlas bug" }),
        ],
      }),
    );
    renderFindings();
    await waitFor(() => expect(screen.getByText("uzi bug")).toBeTruthy());
    // Both repo headers render as their own grouping headings.
    expect(screen.getByRole("heading", { name: "vtmocanu/uzi" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "vtmocanu/atlas-api" })).toBeTruthy();
    // The default fetch is to_file, no repo/run filter.
    expect(mockApi.listFindings).toHaveBeenCalledWith("to_file", undefined, undefined);
  });

  it("flattens under a single-repo scope, dropping the repo grouping header", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({ repo: "repo-uzi", findings: [finding({ last_title: "only uzi" })] }),
    );
    renderFindings(["/findings?repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("only uzi")).toBeTruthy());
    // No repo grouping heading in the single-repo view.
    expect(screen.queryByRole("heading", { name: "vtmocanu/uzi" })).toBeNull();
    expect(mockApi.listFindings).toHaveBeenCalledWith("to_file", "repo-uzi", undefined);
  });
});

describe("Findings page — bucket segmented control", () => {
  it("switches the fetched bucket when a tab is clicked", async () => {
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());
    fireEvent.click(screen.getByRole("tab", { name: "Filed" }));
    await waitFor(() => expect(mockApi.listFindings).toHaveBeenCalledWith("filed", undefined, undefined));
  });
});

describe("Findings page — file + stale-card 409", () => {
  it("File flips the row to filed with an issue link", async () => {
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "Leaked ticker" },
    });
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "File" }));

    await waitFor(() => expect(screen.getByRole("link", { name: /Filed #512/ })).toBeTruthy());
    expect(mockApi.fileFinding).toHaveBeenCalledWith("find-1");
    expect(screen.getByRole("link", { name: /Filed #512/ }).getAttribute("href")).toBe(
      "https://gitlab.example.com/vtmocanu/uzi/-/issues/512",
    );
  });

  it("a stale File that 409s shows the friendly 'already resolved' state", async () => {
    mockApi.fileFinding.mockRejectedValue(new ApiError(409, "already filed or being filed"));
    // The 409 path reloads to reconcile; keep the same open row so the friendly badge is what
    // the assertion sees (best-effort — the backlog is the source of truth).
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "File" }));

    await waitFor(() => expect(screen.getByText("already resolved")).toBeTruthy());
  });

  it("renders a null finding_id row display-only, with no File/Dismiss actions", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        bucket: "filed",
        findings: [finding({ finding_id: undefined, status: "filed", filed_issue_iid: 488, last_title: "orphaned filed" })],
      }),
    );
    renderFindings(["/findings?bucket=filed"]);
    await waitFor(() => expect(screen.getByText("orphaned filed")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "File" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Dismiss" })).toBeNull();
  });
});
