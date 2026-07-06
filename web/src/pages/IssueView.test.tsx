// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { IssueView } from "./IssueView";
import { api, type Card, type IssueDetail } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// IssueView loads four endpoints and, for the PRDLESS toggle (PRD #22 M4), calls
// setIssuePrdless. Mock the api and useAuth so the test stays offline.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getIssue: vi.fn(),
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      setIssuePrdless: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function anIssue(over: Partial<IssueDetail> = {}): IssueDetail {
  return {
    iid: 7,
    title: "A small typo fix",
    state: "opened",
    labels: ["PRD"],
    web_url: "https://gitlab.example.com/grp/proj/-/issues/7",
    author: "alice",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    description: "no PRD here",
    ...over,
  };
}

function aCard(labels: string[]): Card {
  return {
    iid: 7,
    title: "A small typo fix",
    state: "opened",
    labels,
    web_url: "https://gitlab.example.com/grp/proj/-/issues/7",
    author: "alice",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    latest_run: null,
  };
}

function setAuth(prdlessEnabled: boolean) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  });
}

function renderIssueView() {
  return render(
    <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
      <Routes>
        <Route path="/repos/:repoId/issues/:iid" element={<IssueView />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.listRuns.mockResolvedValue({ runs: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("IssueView PRDLESS toggle (PRD #22 M4)", () => {
  it("hides the toggle when the feature is disabled", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue() });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.queryByText(/PRDLESS/)).toBeNull();
  });

  it("applies the label and adopts the returned card's labels", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue() });
    mockApi.setIssuePrdless.mockResolvedValue({ card: aCard(["PRD", "PRDLESS"]) });
    renderIssueView();

    const applyBtn = await screen.findByText("Mark PRDLESS");
    fireEvent.click(applyBtn);

    await waitFor(() =>
      expect(mockApi.setIssuePrdless).toHaveBeenCalledWith("repo-1", 7, true),
    );
    // After the 200, the label is present, so the affordance flips to "Remove".
    await screen.findByText("Remove PRDLESS");
  });

  it("removes the label when it is already applied", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    mockApi.setIssuePrdless.mockResolvedValue({ card: aCard(["PRD"]) });
    renderIssueView();

    const removeBtn = await screen.findByText("Remove PRDLESS");
    fireEvent.click(removeBtn);

    await waitFor(() =>
      expect(mockApi.setIssuePrdless).toHaveBeenCalledWith("repo-1", 7, false),
    );
    await screen.findByText("Mark PRDLESS");
  });
});

describe("IssueView PRDLESS badge (PRD #22 M3)", () => {
  // The badge is queried by its distinctive title, since the issue also renders a
  // label chip named "PRDLESS" — the title disambiguates the badge from the chip.
  const BADGE_TITLE = "PRD-link gate bypassed by label";

  it("shows the PRDLESS badge, not 'no PRD link', when enabled and labeled", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByTitle(BADGE_TITLE)).toBeTruthy();
    expect(screen.queryByText("no PRD link")).toBeNull();
  });

  it("shows 'no PRD link' when enabled but the label is absent", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("no PRD link")).toBeTruthy();
    expect(screen.queryByTitle(BADGE_TITLE)).toBeNull();
  });

  it("shows 'no PRD link' when the feature is disabled even if labeled", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("no PRD link")).toBeTruthy();
    expect(screen.queryByTitle(BADGE_TITLE)).toBeNull();
  });
});
