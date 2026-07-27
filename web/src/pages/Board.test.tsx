// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { Board, IssueCard } from "./Board";
import { api, type Board as BoardData, type Card } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// The full Board mounts four endpoints and reads the configured label names off the
// auth context; mock both so the drag test below stays offline.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getBoard: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      listRuns: vi.fn(),
      moveIssue: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function aCard(over: Partial<Card> = {}): Card {
  return {
    iid: 7,
    title: "Add a metrics dashboard",
    state: "opened",
    labels: ["PRD"],
    web_url: "https://gitlab.example.com/g/r/-/issues/7",
    forge_type: "gitlab",
    author: "someone",
    has_prd_link: true,
    column: "todo",
    closed: false,
    conflict: false,
    latest_run: null,
    pipeline: null,
    ...over,
  } as Card;
}

function renderCard(over: Partial<Card> = {}) {
  return render(
    <MemoryRouter>
      <IssueCard
        card={aCard(over)}
        repoId="repo-1"
        gate={{ enabled: true, reason: "" }}
        starting={false}
        onStart={vi.fn()}
        fixCiBusy={false}
        onFixCi={vi.fn()}
        prdlessEnabled={false}
        prdlessLabel="PRDLESS"
        prdlessBusy={false}
        onTogglePrdless={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        dimmed={false}
      />
    </MemoryRouter>,
  );
}

// Issue #124, item 9. A board card's title is the FORGE issue title: writable by anyone who
// can open an issue on the target repo, and the board is where titles sit in a column next
// to each other, which is exactly where a reordered one does the most damage.
//
// This file exists because `Board.tsx` had no test at all — dropping the strip left the
// whole suite green, the same hole the RunView heading was in. `IssueCard` is exported for
// that reason, matching how RunView factors its panels.
describe("IssueCard — the forge title carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of the card title", () => {
    const { container } = renderCard({ title: "Add a \u202Emetrics\u200B dashboard" });
    // Anchored on the iid, which the mutation cannot move: a lookup for the CLEANED title
    // could not match while the format character is present, so the case would red at the
    // lookup instead of at the assertion below.
    expect(screen.getByText(/#7/)).toBeTruthy();
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("Add a metrics dashboard")).toBeTruthy();
  });

  it("leaves an ordinary title byte-identical", () => {
    // The strip must not be a general text mangler — this is the sibling case that catches
    // an over-broad predicate reaching the board.
    const { container } = renderCard({ title: "Add a metrics dashboard" });
    expect(container.textContent).toContain("Add a metrics dashboard");
  });

  it("keeps the card's link keyed on the iid, never on the title", () => {
    // The coordinate rule, asserted rather than assumed: the strip is a DISPLAY transform,
    // so nothing that identifies or round-trips may be built from the stripped string.
    renderCard({ title: "Add a \u202Emetrics dashboard", iid: 7 });
    const link = screen.getByRole("link", { name: /metrics dashboard/ });
    expect(link.getAttribute("href")).toBe("/repos/repo-1/issues/7");
  });
});

// PRD #102 Decision 14a. M1 renames the implicit column's DISPLAY string only. Two
// strings must survive it: `OPEN_KEY` (the internal "") and the literal `"open"`
// move() sends, which the server matches with EqualFold. A blind Open→Backlog replace
// breaks drag-to-Backlog, and nothing else in the suite would have noticed — which is
// why this mounts the whole Board rather than a card.
describe("Board — the Backlog rename is display-only (PRD #102 Decision 14a)", () => {
  const aBoard = (over: Partial<BoardData> = {}): BoardData => ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [{ label_name: "Planned" }, { label_name: "In Progress" }] as BoardData["columns"],
    cards: [aCard({ iid: 7, column: "In Progress", labels: ["PRD", "In Progress", "bug"] })],
    pipeline: null,
    ...over,
  });

  const laneFor = (label: string) => {
    // <lane><header><dot/><span>{label}</span><count/></header><cards/></lane>
    const lane = screen.getByText(label).parentElement?.parentElement;
    if (!lane) throw new Error(`no lane for ${label}`);
    return lane;
  };

  const drop = (lane: HTMLElement, iid: number) =>
    fireEvent.drop(lane, { dataTransfer: { getData: () => String(iid) } });

  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      prdLabel: "PRD",
      autopilotLabel: "autopilot",
      prdlessLabel: "PRDLESS",
      prdlessEnabled: false,
      theme: "ember",
      themeOverride: null,
      defaultTheme: "ember",
      vaultUnlocked: true,
      vaultExists: true,
      hasPassword: true,
      register: vi.fn(),
      login: vi.fn(),
      logout: vi.fn(),
      refresh: vi.fn(),
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.getBoard.mockResolvedValue({ board: aBoard() });
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.moveIssue.mockResolvedValue({ card: aCard({ iid: 7, column: "" }) });
  });

  const renderBoard = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>,
    );

  it("labels the implicit column Backlog and no lane reads Open", async () => {
    const { container } = renderBoard();
    await screen.findByText("Backlog");
    expect(within(container).queryByText("Open")).toBeNull();
  });

  it('still sends the wire string "open" when a card is dropped on Backlog', async () => {
    renderBoard();
    await screen.findByText("Backlog");
    drop(laneFor("Backlog"), 7);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    // Not "Backlog", not "" — the server matches this literal with EqualFold.
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 7, "open");
  });

  it("sends a real column's label unchanged, so only the implicit key is translated", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    drop(laneFor("Planned"), 7);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 7, "Planned");
  });
});
