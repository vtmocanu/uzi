// @vitest-environment jsdom
//
// The board of a repo linked to an issue tracker (PRD 4173 M6): the issue key replaces
// `#iid` in the screen-reader labels and toasts, search matches it, eligibility is the
// payload's eligibility_label, and the description names States rather than labels.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { Board } from "./Board";
import { api, type Board as BoardData, type Card, type RunListItem } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getBoard: vi.fn(),
      getBoardPrefs: vi.fn(),
      setBoardPrefs: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      listRuns: vi.fn(),
      createRun: vi.fn(),
      moveIssue: vi.fn(),
      reorderBoard: vi.fn(),
      promoteIssue: vi.fn(),
      syncRepo: vi.fn(),
      configureColumns: vi.fn(),
      getMySettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => {
  const useAuth = vi.fn();
  // The run-start preview store reads the signed-in user through useOptionalAuth; serve it from
  // the same mocked value, as Board.test.tsx does.
  return { useAuth, useOptionalAuth: () => useAuth() };
});

const mockApi = vi.mocked(api);

function aCard(over: Partial<Card>): Card {
  return {
    iid: 1,
    title: "card",
    state: "opened",
    labels: ["send-to-uzi"],
    assignee_ids: [],
    web_url: "https://youtrack.example.com/issue/ABC-1",
    forge_type: "gitlab",
    author: "someone",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    latest_run: null,
    pipeline: null,
    ...over,
  } as Card;
}

const linkedBoard = (): BoardData =>
  ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [],
    cards: [
      aCard({ iid: 1, title: "first card", issue_key: "ABC-1", forge_updated_at: "2026-01-01T00:00:00Z" }),
      aCard({ iid: 2, title: "second card", issue_key: "ABC-2", forge_updated_at: "2026-02-01T00:00:00Z" }),
      // Carries the configured uzi label but not the eligibility tag: not uzi's here.
      aCard({ iid: 3, title: "untagged card", issue_key: "ABC-3", labels: ["uzi"] }),
    ],
    pipeline: null,
    bot_forge_user_id: 0,
    eligibility_label: "send-to-uzi",
  }) as BoardData;

beforeEach(() => {
  const m = new Map<string, string>([["uzi.board.repo-1.hideEmpty", "false"]]);
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage,
  });
  vi.mocked(useAuth).mockReturnValue({
    user: null,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance: {
      mode: "dark",
      light_theme: "hall",
      dark_theme: "ember",
      typeface: "system",
      overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
      defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
    },
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.getMySettings.mockResolvedValue({ settings: {} } as Awaited<ReturnType<typeof api.getMySettings>>);
  mockApi.getBoard.mockResolvedValue({ board: linkedBoard() });
  mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
  mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listRuns.mockResolvedValue({ runs: [] });
  mockApi.reorderBoard.mockImplementation(async () => ({ board: linkedBoard() }));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const renderBoard = () =>
  render(
    <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
      <Routes>
        <Route path="/repos/:id/board" element={<Board />} />
      </Routes>
    </MemoryRouter>,
  );

describe("Board on a repo linked to an issue tracker (PRD 4173 M6)", () => {
  it("describes its columns as issue tracker States, not labels", async () => {
    renderBoard();
    await screen.findByText("first card");
    expect(screen.getByText(/Columns map to issue tracker States/)).toBeTruthy();
    expect(screen.queryByText(/Columns are .* labels/)).toBeNull();
  });

  it("shows only cards carrying the eligibility tag", async () => {
    renderBoard();
    await screen.findByText("first card");
    expect(screen.getByText("second card")).toBeTruthy();
    expect(screen.queryByText("untagged card")).toBeNull();
  });

  it("names the key in the move buttons, keeps focus on it after a reorder, and toasts the key", async () => {
    renderBoard();
    await screen.findByText("first card");
    const down = screen.getByRole("button", { name: /^Move issue ABC-1 down in / });
    expect(screen.queryByRole("button", { name: /Move issue #1 / })).toBeNull();
    down.focus();
    let release: (v: { board: BoardData }) => void = () => {};
    mockApi.reorderBoard.mockImplementation(() => new Promise<{ board: BoardData }>((res) => { release = res; }));
    fireEvent.click(down);
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    document.body.tabIndex = -1;
    document.body.focus();
    release({ board: linkedBoard() });
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));
    expect(document.activeElement?.getAttribute("aria-label")).toMatch(/^Move issue ABC-1 down in /);
    expect((await screen.findAllByText(/ABC-1 moved down in /)).length).toBe(2);
  });

  it("searches by issue key, case-insensitively", async () => {
    renderBoard();
    await screen.findByText("first card");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), { target: { value: "abc-2" } });
    await waitFor(() => expect(screen.queryByText("first card")).toBeNull());
    expect(screen.getByText("second card")).toBeTruthy();
  });

  it("names the run link of the needs-attention strip by the key", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        {
          id: "run-9",
          status: "awaiting_approval",
          health: "ok",
          issue_iid: 1,
          issue_key: "ABC-1",
          issue_title: "Fix the parser",
          kind: "issue",
          forge_type: "gitlab",
          issue_web_url: null,
        } as unknown as RunListItem,
      ],
    });
    renderBoard();
    await screen.findByText("1 run needs approval");
    expect(screen.getByRole("link", { name: "Open run for issue ABC-1: Fix the parser" })).toBeTruthy();
  });

  it("says a created issue opens in the issue tracker with the eligibility tag", async () => {
    renderBoard();
    await screen.findByText("first card");
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));
    const note = await screen.findByText(/Opened in the issue tracker with the/);
    expect(note.textContent).toContain("Opened in the issue tracker with the send-to-uzi tag, so a run can be started from it.");
    expect(screen.queryByText(/Opened on GitLab/)).toBeNull();
  });
});
