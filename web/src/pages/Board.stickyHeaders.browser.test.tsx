import type {} from "@vitest/browser-playwright";
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { Board } from "./Board";
import { api, type Board as BoardData, type Card } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { aRun } from "./overviewFixtures";

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
      getMySettings: vi.fn(),
      getEffectiveSettings: vi.fn(),
      previewStartSettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => {
  const useAuth = vi.fn();
  // The run-start preview store reads the signed-in user through useOptionalAuth; serve it from
  // the same mocked value so one mockReturnValue(useAuth) drives both.
  return { useAuth, useOptionalAuth: () => useAuth() };
});

const mockApi = vi.mocked(api);

const aCard = (iid: number): Card =>
  ({
    iid,
    title: `Issue number ${iid} with a title long enough to wrap onto a second line`,
    state: "opened",
    labels: ["uzi", "Planned"],
    assignee_ids: [],
    web_url: `https://forge.example.com/g/r/-/issues/${iid}`,
    forge_type: "gitlab",
    author: "someone",
    has_prd_link: true,
    column: "Planned",
    closed: false,
    conflict: false,
    latest_run: null,
    pipeline: null,
    forge_updated_at: "2026-01-01T00:00:00Z",
  }) as Card;

const aBoard = (): BoardData => ({
  repo_id: "repo-1",
  path_with_namespace: "grp/proj",
  web_url: "https://forge.example.com/grp/proj",
  forge_type: "gitlab",
  columns: [{ label_name: "Planned" }, { label_name: "Human Review" }] as BoardData["columns"],
  cards: Array.from({ length: 30 }, (_, i) => aCard(i + 1)),
  pipeline: null,
  bot_forge_user_id: 0,
});

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem("uzi.board.repo-1.hideEmpty", "false");
  vi.mocked(useAuth).mockReturnValue({
    user: null,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.getBoard.mockResolvedValue({ board: aBoard() });
  mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
  mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listRuns.mockResolvedValue({ runs: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: {} } as Awaited<ReturnType<typeof api.getMySettings>>);
});

afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
  localStorage.clear();
});

async function renderBoard(width: number, height: number) {
  await page.viewport(width, height);
  render(
    <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>
    </main>,
  );
  await screen.findByText("Human Review");
  const header = (label: string) => screen.getByText(label).parentElement as HTMLElement;
  const row = header("Closed").parentElement!.parentElement as HTMLElement;
  const toolbar = screen.getByPlaceholderText("Search issues…").closest(".sticky") as HTMLElement;
  return { header, row, toolbar };
}

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r(null)));

const centre = (el: Element) => {
  const r = el.getBoundingClientRect();
  return [r.left + r.width / 2, r.top + r.height / 2] as const;
};

describe("Board column headers stay visible while a tall lane scrolls (#4315)", () => {
  it("pins every lane header, short lanes included, below the sticky toolbar on a wide viewport", async () => {
    const { header, row, toolbar } = await renderBoard(1280, 800);
    window.scrollTo(0, document.documentElement.scrollHeight);
    row.scrollTop = row.scrollHeight;
    await nextFrame();

    const toolbarBottom = toolbar.getBoundingClientRect().bottom;
    for (const label of ["Backlog", "Planned", "Human Review", "Closed"]) {
      const r = header(label).getBoundingClientRect();
      expect(r.top, `${label} header top`).toBeGreaterThanOrEqual(toolbarBottom - 1);
      expect(r.bottom, `${label} header bottom`).toBeLessThanOrEqual(window.innerHeight);
      for (const x of [r.left + 8, r.left + r.width / 2, r.right - 8]) {
        const hit = document.elementFromPoint(x, r.top + r.height / 2);
        expect(header(label).contains(hit), `${label} header is not covered at x=${x}`).toBe(true);
      }
    }
    expect(row.scrollTop).toBeGreaterThan(0);
    expect(row.getBoundingClientRect().bottom).toBeLessThanOrEqual(window.innerHeight);
  });

  it("paints the pinned header over a card's reorder controls scrolled beneath it", async () => {
    const { header, row } = await renderBoard(1280, 800);
    window.scrollTo(0, document.documentElement.scrollHeight);
    await nextFrame();
    const control = screen.getByRole("button", { name: "Move issue #2 down in Planned" });
    const planned = header("Planned");
    row.scrollTop += control.getBoundingClientRect().top - planned.getBoundingClientRect().top;
    await nextFrame();

    const [x, y] = centre(control);
    const r = planned.getBoundingClientRect();
    expect(y, "control sits under the header").toBeGreaterThan(r.top);
    expect(y).toBeLessThan(r.bottom);
    expect(planned.contains(document.elementFromPoint(x, y)), "header is on top").toBe(true);
  });

  it("scrolls a card into view at the nearest edge below the pinned header, not under it", async () => {
    const { header, row } = await renderBoard(1280, 800);
    window.scrollTo(0, document.documentElement.scrollHeight);
    row.scrollTop = row.scrollHeight;
    await nextFrame();
    const control = screen.getByRole("link", { name: /^Issue number 1 with/ });
    control.scrollIntoView({ block: "nearest" });
    await nextFrame();

    expect(row.scrollTop).toBeLessThan(row.scrollHeight - row.clientHeight);
    expect(control.getBoundingClientRect().top).toBeGreaterThanOrEqual(header("Planned").getBoundingClientRect().bottom);
  });

  it("keeps the attention strip's issue links under the pinned toolbar", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          status: "awaiting_approval",
          issue_iid: 5,
          issue_web_url: "https://forge.example.com/g/r/-/issues/5",
        }),
      ],
    } as Awaited<ReturnType<typeof api.listRuns>>);
    const { toolbar } = await renderBoard(1280, 800);
    const link = await screen.findByRole("link", { name: /Open issue #5/ });
    window.scrollTo(0, document.documentElement.scrollHeight);
    await nextFrame();

    const [x, y] = centre(link);
    expect(y, "strip link scrolled under the toolbar").toBeLessThan(toolbar.getBoundingClientRect().bottom);
    expect(toolbar.contains(document.elementFromPoint(x, y)), "toolbar paints over the strip link").toBe(true);
  });

  it("keeps the row a horizontal scroller", async () => {
    const { row } = await renderBoard(1024, 800);
    expect(row.scrollWidth).toBeGreaterThan(row.clientWidth);
    row.scrollLeft = row.scrollWidth;
    expect(row.scrollLeft).toBeGreaterThan(0);
  });

  it("leaves the narrow layout on page scroll with no inner vertical scroller", async () => {
    const { row } = await renderBoard(390, 800);
    expect(row.scrollHeight).toBeLessThanOrEqual(row.clientHeight + 1);
  });
});
