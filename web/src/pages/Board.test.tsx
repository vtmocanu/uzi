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
      getBoardPrefs: vi.fn(),
      setBoardPrefs: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      listRuns: vi.fn(),
      moveIssue: vi.fn(),
      reorderBoard: vi.fn(),
      promoteIssue: vi.fn(),
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

// renderCard renders one IssueCard. `props` overrides the M6 card-shape props
// (PRD #102): the defaults describe an ordinary PRD card, which is what every
// pre-M6 test in this file is about, so those tests keep asserting what they always
// did while the non-PRD cases opt in explicitly.
function renderCard(
  over: Partial<Card> = {},
  chips: string[] = [],
  maxChips?: number,
  props: { isEligible?: boolean; canPromote?: boolean; promoting?: boolean; onPromote?: () => void } = {},
) {
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
        prdLabel="PRD"
        isEligible={props.isEligible ?? true}
        canPromote={props.canPromote ?? false}
        promoting={props.promoting ?? false}
        onPromote={props.onPromote ?? vi.fn()}
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
    // N1. A bare <span> is role=generic, which ARIA 1.2 forbids naming, so the
    // aria-label above is invalid on it and a conforming AT drops it — Firefox
    // announces only "+2". This assertion is the ONLY instrument in this repo that
    // can see that: jsdom does not enforce ARIA name prohibitions, and Chromium
    // exposes the name regardless, so a browser check passes too. It is asserted on
    // the ATTRIBUTE for exactly that reason.
    expect(more.getAttribute("role")).toBe("img");
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
      runEligibleLabels: ["PRD", "bug"],
      eligibleLabelWaivesPrdLink: true,
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
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
    mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
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
      runEligibleLabels: ["PRD", "bug"],
      eligibleLabelWaivesPrdLink: true,
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
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
    mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
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

    // TWO surfaces since web-ux S5, and this asserts both by name rather than
    // findByText, which now throws on the multiple match. The split is the fix: the
    // visible toast is for people who can see it, and the always-mounted sr-only
    // live region is what a screen reader actually announces. Asserting only one
    // would let the other be deleted silently — and the sr-only one is the half no
    // developer would notice missing.
    const announced = await screen.findAllByText(/#1 moved down in Backlog/);
    expect(announced).toHaveLength(2);

    const live = document.querySelector('div.sr-only[role="status"]') as HTMLElement;
    expect(live).toBeTruthy();
    expect(live.textContent).toMatch(/#1 moved down in Backlog/);
  });

  // web-ux S5. A live region must EXIST before its content changes, or assistive tech
  // has no change to announce. The old markup created the region and its first message
  // in the same render, which is typically silent — and looks correct in the DOM.
  it("mounts the live region before there is anything to announce (S5)", async () => {
    renderBoard();
    await screen.findByText("Backlog");

    const live = document.querySelector('div.sr-only[role="status"]') as HTMLElement;
    expect(live).toBeTruthy();
    expect(live.getAttribute("aria-live")).toBe("polite");
    // Present and EMPTY: the region exists before any announcement has happened.
    expect(live.textContent).toBe("");
  });

  it("does not double-announce: the visible toast stack is not itself a live region (S5)", async () => {
    // Two regions carrying the same string announces it twice. The visible stack is
    // purely visual now.
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(moveBtn(1, "down"));
    await screen.findAllByText(/#1 moved down in Backlog/);

    const regions = document.querySelectorAll('[role="status"][aria-live="polite"]');
    const carrying = [...regions].filter((r) => /#1 moved down/.test(r.textContent ?? ""));
    expect(carrying).toHaveLength(1);
    expect((carrying[0] as HTMLElement).classList.contains("sr-only")).toBe(true);
  });

  // web-ux S6. Reveal was hover/focus-within only, and the card root is not focusable,
  // so on a touch device the only way to reveal these buttons was to focus the title
  // link — which navigates on tap. A touch user therefore could not reorder at all.
  //
  // NEITHER instrument in this repo can observe the defect: jsdom applies no media
  // queries, and agent-browser's device presets set the viewport while
  // matchMedia("(hover: hover)") still returns true, so hover:none cannot be
  // emulated. The class token is the only thing assertable here.
  //
  // That the ARBITRARY VARIANT COMPILES is the other half, and it is not assertable in
  // jsdom either — a mistyped Tailwind variant is silently dropped, leaving this test
  // green over a class with no CSS behind it. Verified at build time instead:
  //   npm run build && grep -c 'hover:none' dist/assets/*.css   -> 1
  it("keeps the reorder buttons reachable on a touch device (S6)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    const reveal = moveBtn(1, "down").parentElement as HTMLElement;

    expect(reveal.classList.contains("opacity-0")).toBe(true);
    expect(reveal.classList.contains("group-hover:opacity-100")).toBe(true);
    // The touch fallback: a coarse pointer has no hover state to reveal anything.
    expect(reveal.classList.contains("[@media(hover:none)]:opacity-100")).toBe(true);
  });

  // web-ux N2. Hovering a card showed a precise insertion edge; hovering the lane's
  // whitespace highlighted the lane and showed nothing at all — so the drop whose
  // outcome is hardest to predict was the one with no feedback.
  it("shows a drop-at-the-end affordance over lane whitespace (N2)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    const lane = laneFor("Backlog");

    // Nothing while no drag is in progress.
    expect(screen.queryByText("Drop at the end")).toBeNull();

    fireEvent.dragStart(screen.getByRole("link", { name: "issue one" }), {
      dataTransfer: { setData: vi.fn(), getData: () => "1" },
    });
    fireEvent.dragOver(lane);
    expect(screen.getByText("Drop at the end")).toBeTruthy();
  });

  it("does not show it while the pointer is over a card, where the edge already answers", async () => {
    // Two indicators at once would contradict each other about where the card lands.
    renderBoard();
    await screen.findByText("Backlog");
    const lane = laneFor("Backlog");

    fireEvent.dragStart(screen.getByRole("link", { name: "issue one" }), {
      dataTransfer: { setData: vi.fn(), getData: () => "1" },
    });
    fireEvent.dragOver(lane);
    expect(screen.getByText("Drop at the end")).toBeTruthy();

    // Over a card: the card-level handler sets insertAt, and the end bar retracts.
    const card = screen.getByRole("link", { name: "issue two" }).closest("div[class*='rounded-lg']") as HTMLElement;
    fireEvent.dragOver(card, { clientY: 5 });
    expect(screen.queryByText("Drop at the end")).toBeNull();
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

// PRD #102 M6 — non-PRD issues on the board.
//
// Driven through the real Board rather than IssueCard, because everything here is
// WIRING: which card set feeds the lanes, which feeds the freeze, and which
// affordance a card gets. A lib-level test of the predicates passes while Board.tsx
// hands the wrong array to either consumer — measured, not assumed: mutating
// Board.tsx to freeze the RENDERED set left all 1316 tests green before this
// describe existed.
describe("Board — non-PRD issues (PRD #102 M6)", () => {
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

  // PRD and non-PRD cards INTERLEAVED by iid in one lane. The interleaving is
  // load-bearing for the freeze case: with the non-PRD cards grouped at one end, an
  // implementation that appended them rather than keeping their positions passes.
  const cards = () => [
    aCard({ iid: 1, title: "issue one", column: "", labels: ["PRD"] }),
    aCard({ iid: 2, title: "issue two", column: "", labels: ["bug"], has_prd_link: false }),
    aCard({ iid: 3, title: "issue three", column: "", labels: ["PRD"] }),
    aCard({ iid: 4, title: "issue four", column: "", labels: [] }),
    aCard({ iid: 5, title: "issue five", column: "", labels: ["PRD"] }),
    aCard({ iid: 6, title: "issue tracker", column: "", labels: ["uzi-self-improve"] }),
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
      prdlessEnabled: true,
      runEligibleLabels: ["PRD", "bug"],
      eligibleLabelWaivesPrdLink: true,
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
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
    mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.moveIssue.mockResolvedValue({ card: aCard({ iid: 1, column: "" }) });
    mockApi.reorderBoard.mockImplementation(async () => ({ board: aBoard() }));
    // The title matters: the assertions read card titles, and aCard's default title
    // is not "issue two". A response that silently renames the card would pass a
    // membership check on iids and fail the one that matters.
    mockApi.promoteIssue.mockResolvedValue({
      card: aCard({ iid: 2, title: "issue two", column: "", labels: ["PRD", "bug"], has_prd_link: false }),
    });
  });

  const renderBoard = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>,
    );

  const titles = () => screen.getAllByRole("link", { name: /^issue / }).map((a) => a.textContent);
  // Open the "Issues" popover (PRD #196 M1 replaced the checkbox with it). Idempotent:
  // opens only when currently closed, so a second call while open does not toggle it
  // shut (fireEvent.click on the button toggles).
  const openIssues = () => {
    const btn = screen.getByRole("button", { name: /^Issues:/ });
    if (btn.getAttribute("aria-expanded") !== "true") fireEvent.click(btn);
  };
  // Open the popover and toggle "Show all other issues" — the old showNonPRD boolean.
  const clickShowAll = () => {
    openIssues();
    fireEvent.click(screen.getByLabelText(/Show all other issues/));
  };

  it("shows PRD plus the default-extra (bug) cards by default (PRD #196 M1)", async () => {
    // Membership is primary ∪ extras, and DEFAULT_BOARD_EXTRA_LABELS is ["bug"], so a
    // fresh board shows the PRD cards AND the bug card — the M1 upgrade behaviour. The
    // unlabelled issue four and the tracker stay off.
    renderBoard();
    await screen.findByText("Backlog");
    expect(titles()).toEqual(["issue one", "issue two", "issue three", "issue five"]);
  });

  it("suppresses an ordinary label that adds zero cards, but keeps a selected one (PRD #196 M6)", async () => {
    // `enhancement` lives only on a PRD card, so it adds nothing to the board — the
    // PRD's "nothing offered that matches zero cards" rule, so it must not appear as a
    // confusing plain 0 row. `bug` (a bug-only card) adds one, so it is offered. A
    // previously-saved extra whose cards have all left the payload (`ghost`) is kept so
    // it stays untickable in place rather than only clearable via Reset.
    mockApi.getBoard.mockResolvedValue({
      board: aBoard({
        cards: [
          aCard({ iid: 1, title: "issue one", column: "", labels: ["PRD", "enhancement"] }),
          aCard({ iid: 2, title: "issue two", column: "", labels: ["bug"], has_prd_link: false }),
        ],
      }),
    });
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: ["ghost"], show_all: false });
    renderBoard();
    await screen.findByText("Backlog");
    openIssues();
    expect(screen.queryByLabelText(/^bug/)).toBeTruthy();
    expect(screen.queryByLabelText(/^enhancement/)).toBeNull();
    expect(screen.queryByLabelText(/^ghost/)).toBeTruthy();
  });

  it("unticking the default extra narrows the board and PUTs the absolute empty set (PRD #196 M3)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    openIssues();
    // The `bug` row is checked by default; unticking it PUTs the absolute empty set to
    // the server (Decision 9), not localStorage.
    fireEvent.click(screen.getByLabelText(/bug/));
    expect(titles()).toEqual(["issue one", "issue three", "issue five"]);
    await waitFor(() =>
      expect(mockApi.setBoardPrefs).toHaveBeenCalledWith("repo-1", { extra_labels: [], show_all: false }),
    );
  });

  it("uses the server-delivered admin default extras over the compiled-in fallback (PRD #196 M2)", async () => {
    const cardsWithDocs = [
      aCard({ iid: 1, title: "issue one", column: "", labels: ["PRD"] }),
      aCard({ iid: 2, title: "issue two", column: "", labels: ["bug"], has_prd_link: false }),
      aCard({ iid: 7, title: "issue seven", column: "", labels: ["documentation"] }),
    ];
    mockApi.getBoard.mockResolvedValue({
      board: aBoard({ cards: cardsWithDocs, board_extra_labels: ["documentation"] }),
    });
    renderBoard();
    await screen.findByText("Backlog");
    // The admin default is now `documentation`, not the const `bug`: the doc card is a
    // member, the bug card is not (the user has no saved override).
    expect(titles()).toEqual(["issue one", "issue seven"]);
    // The inert-default footer reflects the payload default, not DEFAULT_BOARD_EXTRA_LABELS.
    openIssues();
    expect(screen.getByText(/Default: PRD, documentation/)).toBeTruthy();
  });

  it("adds every other open issue with 'Show all other issues' ON", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    clickShowAll();
    expect(titles()).toEqual(["issue one", "issue two", "issue three", "issue four", "issue five"]);
  });

  it("never shows the self-improve tracker, show-all on or off (Decision 13a)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    expect(titles()).not.toContain("issue tracker");
    clickShowAll();
    expect(titles()).not.toContain("issue tracker");
  });

  it("persists 'Show all other issues' to the server (PRD #196 M3)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    clickShowAll();
    await waitFor(() =>
      expect(mockApi.setBoardPrefs).toHaveBeenCalledWith("repo-1", { extra_labels: null, show_all: true }),
    );
  });

  it("reads the server show-all back on load (PRD #196 M3)", async () => {
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: true });
    renderBoard();
    await screen.findByText("Backlog");
    // issue four is unlabelled, so it only appears once show-all is on.
    await waitFor(() => expect(titles()).toContain("issue four"));
  });

  it("applies a server-saved customized extras set over the admin default (PRD #196 M3)", async () => {
    const cardsWithDocs = [
      aCard({ iid: 1, title: "issue one", column: "", labels: ["PRD"] }),
      aCard({ iid: 2, title: "issue two", column: "", labels: ["bug"], has_prd_link: false }),
      aCard({ iid: 7, title: "issue seven", column: "", labels: ["documentation"], has_prd_link: false }),
    ];
    // Admin default is `bug`, but this user has SAVED `documentation` — the absolute
    // set wins (Decision 9), so the doc card is a member and the bug card is not.
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: cardsWithDocs }) });
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: ["documentation"], show_all: false });
    renderBoard();
    await screen.findByText("Backlog");
    await waitFor(() => expect(titles()).toEqual(["issue one", "issue seven"]));
  });

  it("Reset re-adopts the admin default by PUTting extra_labels: null (PRD #196 M3, Decision 9)", async () => {
    // Start from a saved narrowed set so Reset has something to undo.
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: [], show_all: false });
    renderBoard();
    await screen.findByText("Backlog");
    // With the empty absolute set, only PRD cards show.
    await waitFor(() => expect(titles()).toEqual(["issue one", "issue three", "issue five"]));
    openIssues();
    fireEvent.click(screen.getByRole("button", { name: /Reset to default/ }));
    // Reset PUTs the sentinel null (keeping show_all), so the admin default (bug) is
    // re-adopted and the bug card returns.
    await waitFor(() =>
      expect(mockApi.setBoardPrefs).toHaveBeenCalledWith("repo-1", { extra_labels: null, show_all: false }),
    );
    await waitFor(() => expect(titles()).toContain("issue two"));
  });

  it("migrates a legacy showNonPRD:true exactly once and retires the key (open question 4)", async () => {
    // A user who had deliberately widened their board (per-browser, pre-M3) must not be
    // silently narrowed. The server row is pristine, so the legacy key seeds it once.
    store.set("uzi.board.repo-1.showNonPRD", "true");
    renderBoard();
    await screen.findByText("Backlog");
    // Seeded server-side with show-all on, exactly once…
    await waitFor(() =>
      expect(mockApi.setBoardPrefs).toHaveBeenCalledWith("repo-1", { extra_labels: null, show_all: true }),
    );
    expect(mockApi.setBoardPrefs).toHaveBeenCalledTimes(1);
    // …the legacy key is retired so it can never re-trigger…
    expect(store.get("uzi.board.repo-1.showNonPRD")).toBe("false");
    // …and the board is widened (issue four is unlabelled).
    await waitFor(() => expect(titles()).toContain("issue four"));
  });

  // FREEZE-TEST 3 (task #13), at the wiring level. The lib test pins dropIntent's
  // contract; this pins that Board.tsx actually hands it the payload set.
  it("freezes the cards the viewer cannot see, in their existing relative order", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Show-all OFF: the viewer sees the members (1, 2, 3, 5 — bug is a default extra)
    // and moves 5 up one place. The unlabelled #4 stays hidden but must still freeze.
    expect(titles()).toEqual(["issue one", "issue two", "issue three", "issue five"]);
    fireEvent.click(screen.getByRole("button", { name: /Move issue #5 up in/ }));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    const iids = mockApi.reorderBoard.mock.calls[0][1] as number[];
    // The hidden non-PRD cards are IN the freeze — omit them and the server's
    // ClearBoardOrderExcept NULLs their positions, dropping them to the bottom of the
    // lane on this same person's other browser where the toggle is on.
    expect(iids).toContain(2);
    expect(iids).toContain(4);
    // …and in the relative order they already had.
    expect(iids.filter((i) => i === 2 || i === 4)).toEqual([2, 4]);
    // The move really happened, so this is not passing by doing nothing.
    expect(iids.indexOf(5)).toBeLessThan(iids.indexOf(3));
  });

  // D17. Asserted on the CLASS, which is unusual and deliberate: the finding this
  // encodes is a contrast measurement (border-edge 1.39:1 / 1.35:1 against WCAG
  // 1.4.11's 3:1; border-faint 5.16:1 / 5.09:1), and no test in this repo can see a
  // contrast ratio. Pinning the token is the only instrument available, so a silent
  // revert to border-edge reddens here instead of shipping an invisible marker.
  //
  // It also pins the ONE-token rule. Tailwind utilities of equal specificity resolve
  // by stylesheet order, not class-attribute order, so a card carrying both
  // border-edge and border-faint would pick a winner nobody chose — and would look
  // correct in the class list.
  // Minimal preconditions so the missing PRD link is the ONLY thing a gate could block
  // on. The Board only reads workers.length and hasAnthropicToken(secrets).
  const withWorkerAndToken = () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [{ id: "w1" } as unknown as import("../lib/api").Worker] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [{ kind: "anthropic_token" } as unknown as import("../lib/api").SecretMeta],
    });
  };
  const cardOf = (title: string) =>
    screen.getByRole("link", { name: title }).closest("div[class*='rounded-lg']") as HTMLElement;

  it("draws a non-eligible card quiet and an eligible bug card solid (Decision 17, PRD #196 M4)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Show all brings the unlabelled issue four (a non-eligible card) onto the board.
    clickShowAll();

    // classList, not className: these are TOKEN assertions, and a substring check
    // reports "border-edge" present on any card carrying hover:border-edge-strong,
    // which every draggable card does. That is not hypothetical — the first version
    // of this test failed for exactly that reason against correct markup.
    const nonEligible = cardOf("issue four");
    expect(nonEligible.classList.contains("border-dashed")).toBe(true);
    expect(nonEligible.classList.contains("border-faint")).toBe(true);
    expect(nonEligible.classList.contains("border-edge")).toBe(false);

    // issue two (bug) is RUNNABLE now — the eligible set includes bug — so it renders
    // SOLID like a PRD card (mock §4), not dashed.
    const bug = cardOf("issue two");
    expect(bug.classList.contains("border-edge")).toBe(true);
    expect(bug.classList.contains("border-faint")).toBe(false);
    expect(bug.classList.contains("border-dashed")).toBe(false);

    // An ordinary PRD card is untouched: still solid, still border-edge.
    const prd = cardOf("issue one");
    expect(prd.classList.contains("border-edge")).toBe(true);
    expect(prd.classList.contains("border-faint")).toBe(false);
    expect(prd.classList.contains("border-dashed")).toBe(false);
  });

  it("offers Start run on eligible cards and Promote on the non-eligible one (Decision 15, PRD #196 M4)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    clickShowAll();
    const lane = screen.getByText("Backlog").parentElement!.parentElement!;
    // Eligible cards (1,3,5 PRD + 2 bug) offer Start run; the unlabelled #4 is the only
    // non-eligible card and offers Promote instead.
    expect(within(lane).getAllByRole("button", { name: /Start run/ })).toHaveLength(4);
    expect(within(lane).getAllByRole("button", { name: /Promote to PRD/ })).toHaveLength(1);
  });

  it("makes Start ENABLED on a bug card with no PRD link when the waiver is on (PRD #196 M4)", async () => {
    // The waiver mirrors the server: an issue eligible via a NON-PRIMARY label does not
    // need a prds/*.md link. With a worker + token, the missing link is the only thing
    // that could block, and the waiver clears it.
    withWorkerAndToken();
    renderBoard();
    await screen.findByText("Backlog");
    // card 2 (bug, has_prd_link:false) is a default extra → on the board.
    const start = within(cardOf("issue two")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
  });

  it("GATES the same bug card when eligibleLabelWaivesPrdLink is false (PRD #196 M4)", async () => {
    // The scope proof: turn the waiver off and the no-link bug card is blocked again,
    // while a PRD card with a link is unaffected.
    vi.mocked(useAuth).mockReturnValue({
      ...vi.mocked(useAuth)(),
      eligibleLabelWaivesPrdLink: false,
    } as unknown as ReturnType<typeof useAuth>);
    withWorkerAndToken();
    renderBoard();
    await screen.findByText("Backlog");
    const bugStart = within(cardOf("issue two")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(bugStart.disabled).toBe(true));
    const prdStart = within(cardOf("issue one")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    expect(prdStart.disabled).toBe(false);
  });

  it("offers the PRDLESS toggle on an eligible card, not on a non-eligible one (Decision 16, PRD #196 M4)", async () => {
    // prdlessEnabled is true in this describe. card 2 (bug) is eligible and has no PRD
    // link — the shape that shows the toggle. The unlabelled card 4 is not eligible, so
    // the toggle grants nothing and is hidden even once show-all reveals the card.
    renderBoard();
    await screen.findByText("Backlog");
    // card 2 (bug) is a default extra → on the board; eligible → toggle shown.
    expect(screen.getByRole("button", { name: /Mark PRDLESS/ })).toBeTruthy();
    clickShowAll();
    // Still exactly one Mark PRDLESS (card 2's) — the non-eligible card 4 does not get one.
    expect(screen.getAllByRole("button", { name: /Mark PRDLESS/ })).toHaveLength(1);
  });

  it("promotes forge-first and adopts the returned card", async () => {
    // Promote the only non-eligible card, the unlabelled #4.
    mockApi.promoteIssue.mockResolvedValue({
      card: aCard({ iid: 4, title: "issue four", column: "", labels: ["PRD"] }),
    });
    renderBoard();
    await screen.findByText("Backlog");
    // Show all so the non-eligible card 4 is on the board with its Promote button.
    clickShowAll();
    fireEvent.click(screen.getByRole("button", { name: /Promote to PRD/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 4));
    // The promoted card is now an ordinary PRD card, so its Promote button is gone.
    await waitFor(() => expect(screen.queryByRole("button", { name: /Promote to PRD/ })).toBeNull());
    // …and it survives show-all going back off (it is a member by primary now).
    clickShowAll();
    expect(titles()).toContain("issue four");
  });
});

describe("Board attention strip — a run appears in exactly one bucket (#182)", () => {
  // Issue #182 made `waiting_worker` reachable on an awaiting_approval run: the owner
  // answered the gate and the worker has not delivered the next plan version yet. The
  // strip's stuck bucket used to exclude only the `approval_idle` ENUM, which was the
  // same statement as "exclude what the awaiting bucket already shows" ONLY while
  // approval_idle was the sole flag that status could carry. It no longer is.
  const aBoard = (): BoardData =>
    ({
      repo_id: "repo-1",
      path_with_namespace: "grp/proj",
      web_url: "https://gitlab.example.com/grp/proj",
      forge_type: "gitlab",
      columns: [{ label_name: "Planned" }] as BoardData["columns"],
      cards: [aCard({ iid: 7, column: "Planned" })],
      pipeline: null,
    }) as BoardData;

  // Only the four fields the strip's predicates read. RunListItem extends Run and
  // carries ~30 more that no code path here touches.
  const aRun = (over: { id: string; status: string; health: string; issue_iid: number }) =>
    over as unknown as import("../lib/api").RunListItem;

  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      prdLabel: "PRD",
      autopilotLabel: "autopilot",
      prdlessLabel: "PRDLESS",
      prdlessEnabled: false,
      runEligibleLabels: ["PRD", "bug"],
      eligibleLabelWaivesPrdLink: true,
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
    mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
    mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  });

  const renderBoard = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>,
    );

  const strip = () => screen.getByText(/needs approval|needs an answer|looks? stuck|look stuck/);

  it("does not double-list an awaiting_approval run flagged waiting_worker", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-1", status: "awaiting_approval", health: "waiting_worker", issue_iid: 7 })],
    });
    renderBoard();
    await screen.findByText("1 run needs approval");
    // The single load-bearing assertion: one run, one clause. Before the fix this read
    // "1 run needs approval · 1 run looks stuck" for one run.
    expect(strip().textContent).toBe("1 run needs approval");
    // ...and one chip, not two. The strip spreads all three buckets into one .map keyed
    // on r.id, so a double-list is also a duplicate React key.
    expect(screen.getAllByRole("link", { name: "#7 →" })).toHaveLength(1);
  });

  it("still lists a genuinely stuck run — the control", async () => {
    // A running run flagged stalled belongs to the stuck bucket and nothing else. Without
    // this, a predicate that simply returned [] would pass the test above.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-2", status: "running", health: "stalled", issue_iid: 7 })],
    });
    renderBoard();
    await screen.findByText("1 run looks stuck");
    expect(screen.getAllByRole("link", { name: "#7 →" })).toHaveLength(1);
  });

  it("keeps the buckets separate when two different runs are in different states", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "run-1", status: "awaiting_approval", health: "waiting_worker", issue_iid: 7 }),
        aRun({ id: "run-2", status: "running", health: "stalled", issue_iid: 8 }),
      ],
    });
    renderBoard();
    await screen.findByText("1 run needs approval · 1 run looks stuck");
    expect(screen.getAllByRole("link", { name: /#(7|8) →/ })).toHaveLength(2);
  });

  it("does not double-list an awaiting_input run that still carries a flag", async () => {
    // The same shape one status over. The server clears health on every status
    // transition, so this needs a stale flag to occur at all — which is exactly why the
    // exclusion is by status rather than by enumerating the flags each status can hold.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-3", status: "awaiting_input", health: "stalled", issue_iid: 7 })],
    });
    renderBoard();
    await screen.findByText("1 run needs an answer");
    expect(strip().textContent).toBe("1 run needs an answer");
    expect(screen.getAllByRole("link", { name: "#7 →" })).toHaveLength(1);
  });
});
