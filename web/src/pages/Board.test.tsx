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
      reorderBoard: vi.fn(),
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

function renderCard(over: Partial<Card> = {}, chips: string[] = [], maxChips?: number) {
  return render(
    <MemoryRouter>
      <IssueCard
        card={aCard(over)}
        repoId="repo-1"
        chips={chips}
        maxChips={maxChips}
        laneLabel="Backlog"
        canMoveUp={false}
        canMoveDown={false}
        onMoveUp={vi.fn()}
        onMoveDown={vi.fn()}
        insertionEdge={null}
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

// PRD #102 M4. The predicate itself is unit-tested in labelChips.test.ts; these cover
// what the card does with the result, which is the half a pure test cannot see.
describe("IssueCard label chips (PRD #102 M4)", () => {
  it("renders each chip it is handed", () => {
    renderCard({}, ["bug", "security"]);
    expect(screen.getByText("bug")).toBeTruthy();
    expect(screen.getByText("security")).toBeTruthy();
  });

  it("renders no chip row at all when nothing survives the filter", () => {
    // A card whose only labels are workflow markers must not grow an empty row.
    //
    // Asserted on the ROW, not on the chips. The previous version of this test counted
    // `span[title]` and was decoration: with an empty chip list no span renders whether
    // the guard is there or not, so deleting the guard left an empty <div> behind and
    // all eleven tests still passed (reviewer m-1). The row is queryable because it
    // carries role="group" aria-label="Labels", which is also what makes the label set
    // announceable rather than a bare div.
    renderCard({ labels: ["PRD", "autopilot"] }, []);
    expect(screen.queryByRole("group", { name: "Labels" })).toBeNull();
  });

  it("renders the row when there ARE chips — the control for the guard test above", () => {
    // Without this pair, a guard hardcoded to `false` would satisfy the assertion above.
    renderCard({}, ["bug"]);
    expect(screen.getByRole("group", { name: "Labels" })).toBeTruthy();
  });

  it("still accounts for every label when the cap is 0 (Nit-1)", () => {
    // boundedChips' contract is that the remainder is WITHHELD, not lost. Gating the
    // row on shownChips would break exactly here: at a cap of 0 everything is overflow,
    // so the row and its "+N" would both disappear and the labels would vanish with
    // nothing saying so. The guard reads `chips.length`, so the row survives.
    renderCard({}, ["a", "b"], 0);
    const row = screen.getByRole("group", { name: "Labels" });
    expect(row).toBeTruthy();
    expect(screen.getByText("+2")).toBeTruthy();
  });

  it("makes the +N overflow focusable and announced, not hover-only (Nit-4)", () => {
    // The withheld labels rode a `title` alone, which no keyboard or screen-reader user
    // can reach — the same accessibility class as the drag gesture the ↑/↓ buttons
    // exist for, so it gets the same answer.
    renderCard({}, ["a", "b", "c", "d", "e", "f"]);
    const more = screen.getByLabelText("2 more labels: e, f");
    expect(more.getAttribute("tabindex")).toBe("0");
    more.focus();
    expect(document.activeElement).toBe(more);
  });

  it("strips the chip's title ATTRIBUTE, not only its text (#124, m-3)", () => {
    // The assertion-channel trap: a whole-subtree container.textContent assertion CANNOT
    // see an attribute value, so folding title={stripUnsafeChars(l)} to title={l} left
    // twenty tests green. Attributes are only ever covered where someone wrote an
    // explicit check.
    renderCard({}, ["se\u202Ecurity"]);
    const chip = screen.getByText("security");
    expect(chip.getAttribute("title")).toBe("security");
    expect(chip.getAttribute("title") ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("strips the +N title attribute too", () => {
    renderCard({}, ["a", "b", "c", "d", "e\u202E", "f"]);
    const more = screen.getByText("+2");
    expect(more.getAttribute("title") ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(more.getAttribute("aria-label") ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("caps the visible chips and reports the rest as +N rather than dropping them", () => {
    // The lane is a fixed w-72; an issue wearing eight labels must not push the card
    // several rows taller than its neighbours.
    renderCard({}, ["a", "b", "c", "d", "e", "f"]);
    expect(screen.getByText("+2")).toBeTruthy();
    expect(screen.queryByText("e")).toBeNull();
    expect(screen.queryByText("f")).toBeNull();
    // Withheld, not lost: the remainder is on the +N title.
    expect(screen.getByText("+2").getAttribute("title")).toBe("e, f");
  });

  it("strips format characters out of a chip, which is forge-supplied text (#124)", () => {
    const { container } = renderCard({}, ["se\u202Ecurity"]);
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("security")).toBeTruthy();
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
    columns: [
      { label_name: "Planned" },
      { label_name: "In Progress" },
    ] as BoardData["columns"],
    // The card WEARS its column label as well as sitting in that column — without
    // that, the column-exclusion arm of the predicate is never exercised here and a
    // fold that drops it still passes.
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
    mockApi.reorderBoard.mockImplementation(async () => ({ board: aBoard() }));
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

  it("chips a card's content labels and not its column or workflow labels", async () => {
    // The board-level half of M4: the exclusion set is board.columns + the configured
    // PRD/autopilot/PRDLESS names, both of which live here and not in the card.
    renderBoard();
    await screen.findByText("Backlog");
    const lane = laneFor("In Progress");
    expect(within(lane).getByText("bug")).toBeTruthy();
    // "PRD" is the configured membership label; "In Progress" is this card's own
    // column and already names the lane it sits in.
    expect(within(lane).queryByText("PRD")).toBeNull();
    expect(within(lane).getAllByText("In Progress")).toHaveLength(1);
  });
});

// PRD #102 M5. The sort/reorder half of the board, driven through the KEYBOARD
// controls wherever possible — not for accessibility credit but because it is the only
// DOM-drivable path to the reorder logic: simulating an HTML5 drag in jsdom means
// hand-forging dataTransfer and getBoundingClientRect (which returns zeroes, jsdom
// having no layout), while clicking a button does not.
describe("Board — sort modes and manual ordering (PRD #102 M5)", () => {
  // localStorage-backed prefs. This jsdom build does not expose window.localStorage
  // (see mockApi.test.ts), so back it with a Map, the same approach prefs.test.ts uses.
  function installStorage(): Map<string, string> {
    const m = new Map<string, string>();
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
    return m;
  }

  let store: Map<string, string>;

  // Four open cards in the implicit Backlog lane plus one in another column. The
  // Backlog cards' forge_updated_at order is deliberately the REVERSE of their iid
  // order: PRD freeze-test 1 requires a fixture where the mode order and the iid order
  // genuinely differ, because where they coincide an implementation that falls back to
  // iid passes.
  const cards = () => [
    aCard({ iid: 1, title: "issue one", column: "", labels: ["PRD"], forge_updated_at: "2026-01-01T00:00:00Z" }),
    aCard({ iid: 2, title: "issue two", column: "", labels: ["PRD"], forge_updated_at: "2026-02-01T00:00:00Z" }),
    aCard({ iid: 3, title: "issue three", column: "", labels: ["PRD"], forge_updated_at: "2026-03-01T00:00:00Z" }),
    aCard({ iid: 4, title: "issue four", column: "", labels: ["PRD"], forge_updated_at: "2026-04-01T00:00:00Z" }),
    aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["PRD"], forge_updated_at: "2026-05-01T00:00:00Z" }),
    aCard({ iid: 99, title: "issue closed", column: "", labels: ["PRD"], closed: true, forge_updated_at: "2026-06-01T00:00:00Z" }),
  ];

  const aBoard = (over: Partial<BoardData> = {}): BoardData => ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [{ label_name: "Planned" }] as BoardData["columns"],
    cards: cards(),
    pipeline: null,
    ...over,
  });

  beforeEach(() => {
    store = installStorage();
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
    mockApi.moveIssue.mockResolvedValue({ card: aCard({ iid: 1, column: "Planned" }) });
    mockApi.reorderBoard.mockImplementation(async () => ({ board: aBoard() }));
  });

  const renderBoard = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>,
    );

  const laneFor = (label: string) => {
    const lane = screen.getByText(label).parentElement?.parentElement;
    if (!lane) throw new Error(`no lane for ${label}`);
    return lane;
  };

  const moveBtn = (iid: number, dir: "up" | "down") =>
    screen.getByRole("button", { name: new RegExp(`Move issue #${iid} ${dir} in`) });

  const orderSubmitted = () => {
    const calls = mockApi.reorderBoard.mock.calls;
    return calls[calls.length - 1][1] as number[];
  };

  // C1 — PRD freeze-test 2, THE discriminating case.
  it("freezes the WHOLE board on an untouched board in the default Manual mode", async () => {
    // This is the one a mode-gated freeze fails and everything either side of it
    // passes. On an untouched board every board_position is NULL, so writing a single
    // position sorts that one card ahead of every NULL under NULLS LAST, and a card
    // dragged to the BOTTOM of its column renders at the TOP.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    const iids = orderSubmitted();
    // Every OPEN card, not one iid, and not just the lane that changed.
    expect(iids).toHaveLength(5);
    expect(new Set(iids)).toEqual(new Set([1, 2, 3, 4, 9]));
    // …and card 1 really moved one place down within Backlog.
    expect(iids.slice(0, 4)).toEqual([2, 1, 3, 4]);
  });

  it("excludes closed cards from the freeze", async () => {
    // A frozen position on a closed card would ride an issue that later reopens on the
    // forge and drop it at an arbitrary rank in its new column.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    expect(orderSubmitted()).not.toContain(99);
  });

  // C4 — ruling 1 constraint (i): ONE code path.
  it("a keyboard move and the equivalent drop submit an IDENTICAL order", async () => {
    // Two paths that agree today and drift later is the failure the constraint exists
    // to prevent, and only a comparison catches it.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    const viaKeyboard = orderSubmitted();

    cleanup();
    mockApi.reorderBoard.mockClear();
    renderBoard();
    await screen.findByText("Backlog");
    // The equivalent drop: card 1 onto card 2's BOTTOM half. jsdom reports a zero rect,
    // so clientY 0 against top 0 / height 0 resolves to "bottom" — the pinned boundary
    // in insertionEdgeFor, which is why that boundary is asserted in the unit suite.
    const card2 = screen.getByText("issue two").closest("div[draggable]") as HTMLElement;
    fireEvent.drop(card2, { dataTransfer: { getData: () => "1" }, clientY: 0 });
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    expect(orderSubmitted()).toEqual(viaKeyboard);
  });

  // C5 — the buttons are reachable, named and bounded.
  it("exposes keyboard reorder controls named for the card and the direction", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    const down = moveBtn(1, "down");
    // In the tab order without a hover. `hidden`/`display:none` would remove them and
    // recreate the exact problem they exist to solve, so assert focusability rather
    // than a CSS class.
    down.focus();
    expect(document.activeElement).toBe(down);
    expect(down.getAttribute("aria-label")).toBe("Move issue #1 down in Backlog");
  });

  it("disables the reorder controls at the ends of a column", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Backlog holds 1,2,3,4 in payload order: 1 cannot go up, 4 cannot go down.
    expect((moveBtn(1, "up") as HTMLButtonElement).disabled).toBe(true);
    expect((moveBtn(1, "down") as HTMLButtonElement).disabled).toBe(false);
    expect((moveBtn(4, "down") as HTMLButtonElement).disabled).toBe(true);
  });

  it("gives a single-card lane no reorder controls at all", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Card 9 is alone in Planned: both directions are impossible, so neither button
    // should exist rather than existing permanently disabled.
    expect(screen.queryByRole("button", { name: /Move issue #9/ })).toBeNull();
  });

  it("gives a closed card no reorder controls", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    expect(screen.queryByRole("button", { name: /Move issue #99/ })).toBeNull();
  });

  // C2 — the mode flip, both directions.
  it("switches the board to Manual after a successful reorder and persists it", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Start somewhere other than Manual so the flip is observable.
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "updated" } });
    expect(store.get("uzi.board.repo-1.sortMode")).toBe('"updated"');

    // Under `updated` the lane renders 4,3,2,1, so card 4 is FIRST and its up button
    // is disabled; card 1 is last and can move up.
    fireEvent.click(moveBtn(1, "up"));
    await waitFor(() => expect(store.get("uzi.board.repo-1.sortMode")).toBe('"manual"'));
  });

  it("leaves the mode ALONE when the reorder fails", async () => {
    mockApi.reorderBoard.mockRejectedValue(new Error("boom"));
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "title" } });
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    expect(store.get("uzi.board.repo-1.sortMode")).toBe('"title"');
  });

  it("freezes the order of the CURRENT mode, not the iid order", async () => {
    // The trap Decision 7b exists for: dragging while sorted by Last updated must leave
    // every OTHER card where it visually sat. A freeze that fell back to iid would
    // re-sort the board under the user's hand and read as having scrambled it.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "updated" } });
    // Backlog now displays 4,3,2,1 (newest first) — the reverse of iid order.
    fireEvent.click(moveBtn(4, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    expect(orderSubmitted().slice(0, 4)).toEqual([3, 4, 2, 1]);
  });

  it("renders the mode's order, and Manual renders the payload order untouched", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    const titlesIn = (lane: HTMLElement) =>
      within(lane)
        .getAllByRole("link", { name: /^issue / })
        .map((a) => a.textContent);

    // Manual is the identity over the payload: 1,2,3,4 as the server sent them.
    expect(titlesIn(laneFor("Backlog"))).toEqual(["issue one", "issue two", "issue three", "issue four"]);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "updated" } });
    expect(titlesIn(laneFor("Backlog"))).toEqual(["issue four", "issue three", "issue two", "issue one"]);
  });

  // C3 — the forge write goes first, and a failure stops everything.
  it("writes the column label BEFORE freezing on a cross-column drop", async () => {
    const calls: string[] = [];
    mockApi.moveIssue.mockImplementation(async () => {
      calls.push("move");
      return { card: aCard({ iid: 1, column: "Planned" }) };
    });
    mockApi.reorderBoard.mockImplementation(async () => {
      calls.push("reorder");
      return { board: aBoard() };
    });
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.drop(laneFor("Planned"), { dataTransfer: { getData: () => "1" } });
    await waitFor(() => expect(calls).toEqual(["move", "reorder"]));
  });

  it("never freezes when the forge write fails", async () => {
    // Freeze-first would renumber the board around a move that never happened; the
    // existing contract is that a failed forge write leaves NOTHING written.
    mockApi.moveIssue.mockRejectedValue(new Error("forge down"));
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.drop(laneFor("Planned"), { dataTransfer: { getData: () => "1" } });
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
  });

  it("does not touch the forge for a within-column reorder", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).not.toHaveBeenCalled();
  });

  // M5-1. The keyboard path exists for users who cannot see the board, so its toast is
  // not decoration — it IS the feedback. Announcing success on a failed reorder tells
  // exactly the wrong thing to exactly the people who cannot check.
  it("does NOT announce success when the reorder failed", async () => {
    mockApi.reorderBoard.mockRejectedValue(new Error("boom"));
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    // The error surfaces...
    await screen.findByText("Could not save the new order");
    // ...and the success announcement must not.
    expect(screen.queryByText(/#1 moved down/)).toBeNull();
  });

  it("DOES announce success when the reorder succeeded — the control", async () => {
    // Without this pair, deleting the toast entirely would satisfy the assertion above.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await screen.findByText(/#1 moved down in Backlog/);
  });

  it("does not announce a move when there was nothing to move", async () => {
    // dropIntent returns null when the card is gone from the payload — evicted between
    // render and press. A toast here claims a move that never happened.
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: [] }) });
    renderBoard();
    await screen.findByText("Backlog");
    expect(screen.queryByRole("button", { name: /Move issue/ })).toBeNull();
  });

  // M5-2. The buttons were guarded on `reordering` and the drop handlers were not,
  // leaving the hole in the path that is far easier to trigger twice in a second.
  it("refuses a second reorder while one is in flight, from EITHER path", async () => {
    let release: (v: { board: BoardData }) => void = () => {};
    mockApi.reorderBoard.mockImplementation(
      () => new Promise<{ board: BoardData }>((res) => { release = res; }),
    );
    renderBoard();
    await screen.findByText("Backlog");

    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    // A DROP while the first is still in flight. Both orders would otherwise be
    // computed from the same pre-first-drop payload, so the second silently discards
    // the first.
    const card3 = screen.getByText("issue three").closest("div[draggable]") as HTMLElement;
    fireEvent.drop(card3, { dataTransfer: { getData: () => "2" }, clientY: 0 });
    fireEvent.drop(laneFor("Planned"), { dataTransfer: { getData: () => "2" } });
    expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1);

    release({ board: aBoard() });
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
  });

  it("accepts a reorder again once the first has settled", async () => {
    // The guard must RELEASE. A latch that never clears would satisfy the test above
    // and break the feature after one drag.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(2));
  });

  // B1 (browser pass). Disabling a focused button blurs it, so every keyboard reorder
  // dropped focus to <body> and never restored it. At 38 Tab presses to reach the first
  // move button, that made the affordance single-use — defeating the WCAG 2.1.1 reason
  // it exists, and failing 2.4.3 on the way.
  it("keeps focus on the moved card's control after a keyboard reorder", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // JSDOM DOES NOT IMPLEMENT BLUR-ON-DISABLE, so it cannot reproduce B1's trigger on
    // its own: written without the explicit blur below, this test passes against the
    // UNFIXED code — an assertion looking straight at the defect and unable to see it.
    // The blur is therefore simulated, and what this pins is the RESTORE, which is the
    // half that is real code. The trigger is browser-only and stays with the web-ux pass.
    //
    // The reorder is held unresolved so the blur lands while the operation is genuinely
    // in flight; a plain click resolves the whole flow inside fireEvent's act() and
    // there is no in-flight window to blur into.
    let release: (v: { board: BoardData }) => void = () => {};
    mockApi.reorderBoard.mockImplementation(
      () => new Promise<{ board: BoardData }>((res) => { release = res; }),
    );

    const down = moveBtn(1, "down");
    down.focus();
    expect(document.activeElement).toBe(down);

    fireEvent.click(down);
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    // Focus is moved to <body> directly rather than via down.blur(): jsdom refuses to
    // blur a DISABLED element, and the button is disabled by now — the same jsdom/browser
    // divergence one layer down. <body> is where a real browser puts focus when the
    // focused control is disabled, so this reproduces the end state faithfully.
    document.body.tabIndex = -1;
    document.body.focus();
    expect(document.activeElement).toBe(document.body);

    release({ board: aBoard() });
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Move issue #1 down in Backlog");
  });

  it("falls back to the card's other control when the move disabled the pressed one", async () => {
    // A card moved to the end of its column legitimately loses that direction. Focus
    // must land on the sibling rather than on <body>, which is the same failure by a
    // different route.
    mockApi.reorderBoard.mockImplementation(async () => ({
      board: aBoard({ cards: [...cards().filter((c) => c.iid !== 4), cards()[3]] }),
    }));
    renderBoard();
    await screen.findByText("Backlog");
    const down = moveBtn(4, "down");
    down.focus();
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));
  });

  // S1 (browser pass). Measured 17.4 x 12 CSS px with centres 17.4px apart: fails
  // WCAG 2.5.8 and its spacing exception. These ARE the single-pointer alternative
  // SC 2.5.7 requires for users who cannot drag, so the remedy had a smaller target
  // than the gesture it replaces.
  it("gives the reorder controls a 24px minimum target", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    const cls = moveBtn(1, "down").className;
    expect(cls).toContain("min-h-6");
    expect(cls).toContain("min-w-6");
  });

  // S8 (browser pass). Refusing the second gesture beats losing the first move, but
  // silence was not the trade-off: every other rejection path on this board speaks.
  it("says why a second reorder was refused instead of doing nothing visible", async () => {
    let release: (v: { board: BoardData }) => void = () => {};
    mockApi.reorderBoard.mockImplementation(
      () => new Promise<{ board: BoardData }>((res) => { release = res; }),
    );
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    const card3 = screen.getByText("issue three").closest("div[draggable]") as HTMLElement;
    fireEvent.drop(card3, { dataTransfer: { getData: () => "2" }, clientY: 0 });
    await screen.findByText(/Still saving the previous move/);
    expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1);
    release({ board: aBoard() });
  });

  // S4 (browser pass). Closed cards keep a NULL position by design, so the payload
  // returns them in the SQL fallback order and the lane jumped on the first drop from
  // any non-iid mode — including a drop that moved nothing at all.
  it("keeps the Closed lane in iid order whatever the board mode is", async () => {
    const withClosed = () => [
      ...cards(),
      aCard({ iid: 15, title: "issue fifteen", column: "", closed: true, forge_updated_at: "2026-09-01T00:00:00Z" }),
      aCard({ iid: 18, title: "issue eighteen", column: "", closed: true, forge_updated_at: "2026-08-01T00:00:00Z" }),
    ];
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: withClosed() }) });
    renderBoard();
    await screen.findByText("Backlog");

    const closedIids = () =>
      within(laneFor("Closed"))
        .getAllByRole("link", { name: /^issue / })
        .map((a) => a.textContent);

    // Under `updated`, 15 is newer than 18 — so a mode-sorted Closed lane would put 15
    // first. It must stay in iid order either way.
    // 15, 18, 99 — the base fixture already contributes a closed card at iid 99.
    expect(closedIids()).toEqual(["issue fifteen", "issue eighteen", "issue closed"]);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "updated" } });
    expect(closedIids()).toEqual(["issue fifteen", "issue eighteen", "issue closed"]);
  });

  it("degrades a corrupt persisted mode to Manual instead of breaking the board", async () => {
    store.set("uzi.board.repo-1.sortMode", '"not-a-mode"');
    renderBoard();
    await screen.findByText("Backlog");
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("manual");
  });
});
