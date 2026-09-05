// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { Board, IssueCard } from "./Board";
import { api, ApiError, type Board as BoardData, type Card, type LatestRun } from "../lib/api";
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
      createRun: vi.fn(),
      moveIssue: vi.fn(),
      reorderBoard: vi.fn(),
      promoteIssue: vi.fn(),
      syncRepo: vi.fn(),
      configureColumns: vi.fn(),
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
    labels: ["uzi"],
    assignee_ids: [],
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
        uziLabel="uzi"
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

// Issue #562. A title carrying a long unbreakable token (e.g. an env-var assignment)
// overflowed the fixed w-72 lane because the title <Link> could neither wrap nor shrink.
// jsdom does no layout, so pixel overflow is unassertable here; the enforceable unit-level
// guard is that the two utilities the fix depends on are present on the title link. Both are
// required together — break-words lets the token wrap, min-w-0 overrides the flex child's
// default min-width:auto so the wrap can take effect — so both are asserted.
describe("IssueCard — long-token title can wrap within the lane (#562)", () => {
  it("gives the title link min-w-0 and break-words", () => {
    renderCard({ title: "Revert #550: CLAUDE_CODE_ENABLE_TODO_TOOLS=1 is a no-op", iid: 7 });
    const link = screen.getByRole("link", { name: /Revert #550/ });
    expect(link.className).toContain("min-w-0");
    expect(link.className).toContain("break-words");
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
    renderCard({ labels: ["uzi", "autopilot"] }, []);
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
    // The lane is a fixed w-72 (issue #373 reverted #367's flex-1 basis-72); an issue
    // wearing eight labels must not push the card several rows taller than its neighbours.
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

// PRD #764. The card carries a neutral "PRD" presence badge when its issue links a PRD,
// and a runnable card (isEligible) offers Start run \u2014 the positive assertions that keep
// the "old PRDLESS/no-PRD-link badges are gone" checks from being a vacuous green.
describe("IssueCard \u2014 PRD presence badge + runnable marker (PRD #764)", () => {
  it("renders the neutral PRD badge when the card links a PRD", () => {
    renderCard({ has_prd_link: true });
    expect(screen.getByTitle("This issue links a prds/*.md file")).toBeTruthy();
  });

  it("does not render the PRD badge when the card has no PRD link", () => {
    // Paired with the positive above: the badge is keyed on has_prd_link, not always on.
    renderCard({ has_prd_link: false });
    expect(screen.queryByTitle("This issue links a prds/*.md file")).toBeNull();
  });

  it("offers Start run on a runnable (uzi) card \u2014 the runnable marker", () => {
    renderCard({}, [], undefined, { isEligible: true });
    expect(screen.getByRole("button", { name: /Start run/ })).toBeTruthy();
  });

  it("offers Promote instead of Start run on a non-runnable card", () => {
    renderCard({}, [], undefined, { isEligible: false, canPromote: true });
    expect(screen.getByRole("button", { name: /Promote to uzi/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Start run/ })).toBeNull();
  });
});

// Issue #256 M4 (Decision 4/6): every card wears a uniform per-state duration token
// beside its status badge, and the running elapsed no longer lives INSIDE the badge —
// the badge reads a bare "running" while the token carries "running <elapsed>". The
// board's LatestRun has no started_at, so running counts from created_at (the DEGRADED
// variant, Decision 6): no board-specific code, and the helper returns "" for terminal
// runs so those cards carry no token.
describe("IssueCard duration token (issue #256 M4)", () => {
  // Anchor Date.now() so the elapsed is deterministic — the card reads Date.now()
  // inline (Decision 3, riding the existing poll).
  const NOW = Date.parse("2026-07-04T12:04:00Z");
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const aRun = (over: Partial<LatestRun> = {}): LatestRun =>
    ({
      id: "run-1",
      status: "running",
      mr_iid: null,
      mr_web_url: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      owner_name: "someone",
      worker_name: "laptop",
      is_mine: true,
      run_count: 1,
      created_at: "2026-07-04T12:00:00Z",
      updated_at: "2026-07-04T12:00:00Z",
      ...over,
    }) as LatestRun;

  it("renders a bare 'running' badge PLUS a 'running <elapsed>' duration token", () => {
    renderCard({ latest_run: aRun() });
    // The badge lost the elapsed…
    expect(screen.getByText("running")).toBeTruthy();
    // …and it now rides the faint mono token, counted from created_at (4m ago).
    expect(screen.getByText("running 4m")).toBeTruthy();
  });

  it("gives a queued card its 'queued <elapsed>' token", () => {
    renderCard({ latest_run: aRun({ status: "queued" }) });
    expect(screen.getByText("queued 4m")).toBeTruthy();
  });

  it("renders no duration token for a terminal run (Decision 6)", () => {
    // The board's LatestRun carries no started_at/finished_at, so runDurationLabel's
    // static ran-span cannot be computed and it returns "" — no mono token renders.
    renderCard({ latest_run: aRun({ status: "completed" }) });
    expect(screen.queryByText(/^ran /)).toBeNull();
  });

  it("renders no token at all when the card has no run", () => {
    renderCard({ latest_run: null });
    expect(screen.queryByText(/running|queued|ran /)).toBeNull();
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
    cards: [aCard({ iid: 7, column: "In Progress", labels: ["uzi", "In Progress", "bug", "autopilot"] })],
    pipeline: null,
    bot_forge_user_id: 0,
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
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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

  it("chips a card's content labels and not its column or autopilot marker", async () => {
    // The board-level half of M4 (PRD #764): the exclusion set is board.columns + the
    // configured autopilot label. `uzi` is no longer special to chipLabels — it chips.
    renderBoard();
    await screen.findByText("Backlog");
    const lane = laneFor("In Progress");
    expect(within(lane).getByText("bug")).toBeTruthy();
    // `uzi` chips as an ordinary content label now.
    expect(within(lane).getByText("uzi")).toBeTruthy();
    // "autopilot" is the workflow marker (excluded from chips, no board-card badge), and
    // "In Progress" is this card's own column and already names the lane it sits in.
    expect(within(lane).queryByText("autopilot")).toBeNull();
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
    aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"], forge_updated_at: "2026-01-01T00:00:00Z" }),
    aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"], forge_updated_at: "2026-02-01T00:00:00Z" }),
    aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"], forge_updated_at: "2026-03-01T00:00:00Z" }),
    aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"], forge_updated_at: "2026-04-01T00:00:00Z" }),
    aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"], forge_updated_at: "2026-05-01T00:00:00Z" }),
    aCard({ iid: 99, title: "issue closed", column: "", labels: ["uzi"], closed: true, forge_updated_at: "2026-06-01T00:00:00Z" }),
  ];

  const aBoard = (over: Partial<BoardData> = {}): BoardData => ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [{ label_name: "Planned" }] as BoardData["columns"],
    cards: cards(),
    pipeline: null,
    bot_forge_user_id: 0,
    ...over,
  });

  beforeEach(() => {
    store = installStorage();
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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

  // PRD #1034 M4 — the Closed lane is a real drag target. These assert the EMITTED
  // PAYLOAD (what wire string move() sends), not just DOM: close/reopen route through
  // move()/moveIssue ALONE and must never freeze the lane (never call reorderBoard).
  const dropOn = (lane: HTMLElement, iid: number) =>
    fireEvent.drop(lane, { dataTransfer: { getData: () => String(iid) } });

  it("closes an open card dragged onto the Closed lane", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Card 1 is open in Backlog. Dropping it on Closed sends the wire "closed".
    dropOn(laneFor("Closed"), 1);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 1, "closed");
    // A close is a state change, not a reorder — the lane never freezes.
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
  });

  it("reopens a closed card dragged onto a real column", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Card 99 is closed. Dropping it on Planned sends that column's label; the server
    // reads a reopen from the already-closed card and lands it at the bottom.
    dropOn(laneFor("Planned"), 99);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 99, "Planned");
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
  });

  it("reopens a closed card dragged onto Backlog with the wire string open", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    dropOn(laneFor("Backlog"), 99);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 99, "open");
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
  });

  it("no-ops a closed card dropped back onto the Closed lane", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    dropOn(laneFor("Closed"), 99);
    // Closed → Closed is a no-op: nothing is sent to the forge at all.
    await Promise.resolve();
    expect(mockApi.moveIssue).not.toHaveBeenCalled();
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
  });

  // C2 — the mode flip, both directions.
  it("switches the board to Manual after a successful reorder and persists it", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Start somewhere other than Manual so the flip is observable.
    fireEvent.change(screen.getByRole("combobox", { name: "Sort" }), { target: { value: "updated" } });
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
    fireEvent.change(screen.getByRole("combobox", { name: "Sort" }), { target: { value: "title" } });
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
    fireEvent.change(screen.getByRole("combobox", { name: "Sort" }), { target: { value: "updated" } });
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
    fireEvent.change(screen.getByRole("combobox", { name: "Sort" }), { target: { value: "updated" } });
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

  // PRD #1034 / PR #1051 finding B. The reorderRef lock is ONE synchronous lock across
  // ALL board mutations, not reorders alone: a close/reopen and a reorder must serialize
  // in both directions, or a late reorderBoard response setBoard(fresh) overwrites a
  // newer state transition (and symmetrically). M5-2 above covers reorder-vs-reorder;
  // these two cover the cross-mutation directions the fix added. The refusal is SURFACED
  // (the exact "Still saving…" copy) and the refused call never reaches the API — the
  // buttons are deliberately NOT disabled during a mutation (the B1 focus fix), so the
  // reorder click below genuinely reaches applyDrop and the reorderRef guard, not a
  // disabled attribute, is what refuses it.
  const LOCK_ERROR = "Still saving the previous move — try again in a moment.";

  it("refuses a close while a reorder's reorderBoard is still in flight", async () => {
    let releaseReorder: (v: { board: BoardData }) => void = () => {};
    mockApi.reorderBoard.mockImplementation(
      () => new Promise<{ board: BoardData }>((res) => { releaseReorder = res; }),
    );
    renderBoard();
    await screen.findByText("Backlog");

    // Start a within-lane reorder (keyboard path, columnChanged=false so it never calls
    // move()) and leave its reorderBoard hanging — the lock is now held.
    fireEvent.click(moveBtn(1, "down"));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    // Dragging an OPEN card onto Closed must be REFUSED: no close crosses the wire, and
    // the refusal is surfaced verbatim rather than swallowed.
    dropOn(laneFor("Closed"), 2);
    // Refused synchronously (reorderRef checked before the state-move branch), so no
    // close crosses the wire...
    expect(mockApi.moveIssue).not.toHaveBeenCalled();
    // ...and the refusal is surfaced verbatim.
    expect((await screen.findByText(LOCK_ERROR)).textContent).toBe(LOCK_ERROR);

    // Releasing the reorder settles it (mode flips to manual). The refused close was
    // REFUSED, not queued — it stays un-sent even after the lock frees.
    releaseReorder({ board: aBoard() });
    await waitFor(() => expect(store.get("uzi.board.repo-1.sortMode")).toBe('"manual"'));
    expect(mockApi.moveIssue).not.toHaveBeenCalled();
  });

  it("refuses a reorder while a close's moveIssue is still in flight", async () => {
    let releaseMove: (v: { card: Card }) => void = () => {};
    mockApi.moveIssue.mockImplementation(
      () => new Promise<{ card: Card }>((res) => { releaseMove = res; }),
    );
    renderBoard();
    await screen.findByText("Backlog");

    // Start a close (drag open card 1 onto Closed) and leave its moveIssue hanging — the
    // close now holds the SAME lock during its forge write.
    dropOn(laneFor("Closed"), 1);
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 1, "closed");

    // A reorder attempted now must be REFUSED: reorderBoard is never called, and the
    // refusal is surfaced verbatim.
    fireEvent.click(moveBtn(2, "down"));
    // Refused synchronously (the close holds the lock), so no reorder crosses the wire...
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
    // ...and the refusal is surfaced verbatim.
    expect((await screen.findByText(LOCK_ERROR)).textContent).toBe(LOCK_ERROR);

    // Releasing the close settles it; the refused reorder was not queued.
    releaseMove({ card: aCard({ iid: 1, column: "", closed: true }) });
    await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledTimes(1));
    expect(mockApi.reorderBoard).not.toHaveBeenCalled();
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

  // NOTE (#412 M3): the pre-#412 test "keeps the Closed lane in iid order whatever the
  // board mode is" lived here. #412 M2 REMOVED the Closed-lane iid pin (Board.tsx S4), so
  // that assertion now contradicts the shipped feature — Closed flows through sortCards
  // like every other lane. The behaviour is re-covered, at its new (de-pinned) contract,
  // by the "direction toggle (#412)" describe below (Closed honours mode+direction, and
  // the accepted post-drop revert to iid order). The stale test was left red by M2 and is
  // removed here rather than kept asserting behaviour the code no longer has.

  it("degrades a corrupt persisted mode to Manual instead of breaking the board", async () => {
    store.set("uzi.board.repo-1.sortMode", '"not-a-mode"');
    renderBoard();
    await screen.findByText("Backlog");
    expect((screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement).value).toBe("manual");
  });

  // The direction toggle (#412). Nested here so it reuses the outer harness: `store`,
  // renderBoard(), laneFor(), moveBtn(), the cards()/aBoard() fixtures. sortDir persists
  // at `uzi.board.<repo>.sortDir` as a JSON string ('"asc"'/'"desc"'); DEFAULT_SORT_DIR
  // is iid:asc, run:desc, updated:desc, title:asc, manual:desc.
  describe("direction toggle (#412)", () => {
    // Local rendered-order reader (same shape as the outer suite's at :640). Closed cards
    // DO render an anchor matching /^issue /, so this reads the Closed lane too.
    const titlesIn = (lane: HTMLElement) =>
      within(lane)
        .getAllByRole("link", { name: /^issue / })
        .map((a) => a.textContent);

    const dirBtn = () => screen.getByRole("button", { name: /Sort direction/ }) as HTMLButtonElement;
    const setSort = (value: string) =>
      fireEvent.change(screen.getByRole("combobox", { name: "Sort" }), { target: { value } });

    // A clean two-closed-card board: closed iids 15,18 in payload IID-ASCENDING order,
    // plus open cards 1,2 in Backlog so a keyboard reorder has a valid neighbour. No card
    // 99/9, so the Closed lane holds exactly [15,18] and the observation is unambiguous.
    const twoClosed = (): Card[] => [
      aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"], forge_updated_at: "2026-01-01T00:00:00Z" }),
      aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"], forge_updated_at: "2026-02-01T00:00:00Z" }),
      aCard({ iid: 15, title: "issue fifteen", column: "", closed: true, labels: ["uzi"], forge_updated_at: "2026-09-01T00:00:00Z" }),
      aCard({ iid: 18, title: "issue eighteen", column: "", closed: true, labels: ["uzi"], forge_updated_at: "2026-08-01T00:00:00Z" }),
    ];

    // 1. Toggle renders; DISABLED in Manual, ENABLED once a non-manual mode is chosen.
    // The disabled-negative is PAIRED with the enabled-positive on the SAME control, so
    // neither half is vacuous (a control that never renders would fail the presence check).
    it("renders disabled in Manual and enabled in a non-manual mode", async () => {
      renderBoard();
      await screen.findByText("Backlog");
      // Default load is Manual: the control is present/visible but inert.
      const btn = dirBtn();
      expect(btn).not.toBeNull();
      expect(btn.disabled).toBe(true);
      // A non-manual mode enables the SAME control.
      setSort("updated");
      expect(dirBtn().disabled).toBe(false);
    });

    // 2. Clicking the toggle persists sortDir and REVERSES the open lane's rendered order.
    it("persists sortDir and reverses the open lane on click", async () => {
      renderBoard();
      await screen.findByText("Backlog");
      // updated defaults to desc → Backlog renders newest-first = reverse of iid order.
      setSort("updated");
      const desc = titlesIn(laneFor("Backlog"));
      expect(desc).toEqual(["issue four", "issue three", "issue two", "issue one"]);
      // Before the click: desc default, so aria-pressed is "true" and the label reads down.
      expect(dirBtn().getAttribute("aria-pressed")).toBe("true");
      expect(dirBtn().textContent).toBe("↓ Descending");

      fireEvent.click(dirBtn());

      // Persisted to asc…
      expect(store.get("uzi.board.repo-1.sortDir")).toBe('"asc"');
      // …the lane reversed to ascending…
      const asc = titlesIn(laneFor("Backlog"));
      expect(asc).toEqual(["issue one", "issue two", "issue three", "issue four"]);
      // …and the reversal is REAL (non-vacuous): the two orders genuinely differ.
      expect(asc).not.toEqual(desc);
      // …and the control flipped its pressed state and its visible label/name.
      expect(dirBtn().getAttribute("aria-pressed")).toBe("false");
      expect(dirBtn().textContent).toBe("↑ Ascending");
      expect(dirBtn().getAttribute("aria-label")).toBe("Sort direction: ascending");
    });

    // 3. Switching mode RESETS the persisted direction to the new mode's default. updated
    // and run both default desc, so the reset is only observable if the current direction
    // differs — hence: switch to updated, toggle to asc, then switch to run (default desc)
    // so the reset desc≠asc actually changes state and the DOM.
    it("resets the persisted direction to the new mode's default on a mode switch", async () => {
      renderBoard();
      await screen.findByText("Backlog");
      setSort("updated");
      fireEvent.click(dirBtn()); // updated desc → asc
      expect(store.get("uzi.board.repo-1.sortDir")).toBe('"asc"');
      expect(dirBtn().textContent).toBe("↑ Ascending");

      // run defaults to desc: switching resets asc → desc.
      setSort("run");
      expect(store.get("uzi.board.repo-1.sortDir")).toBe('"desc"');
      expect(dirBtn().textContent).toBe("↓ Descending");
    });

    // 4. The Closed lane honours mode + direction — a Manual/asc case and a desc case.
    // Two closed cards (15,18) make the order observable, and the desc reversal proves the
    // #412 pin removal: before M2 the Closed lane was permanently iid-pinned and would NOT
    // reverse.
    it("sorts the Closed lane by mode+direction, reversing on the toggle", async () => {
      mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: twoClosed() }) });
      renderBoard();
      await screen.findByText("Backlog");

      // Manual (default) is identity over the iid-ordered payload → [15,18] ascending.
      expect(titlesIn(laneFor("Closed"))).toEqual(["issue fifteen", "issue eighteen"]);

      // iid asc (its default) keeps [15,18]…
      setSort("iid");
      const closedAsc = titlesIn(laneFor("Closed"));
      expect(closedAsc).toEqual(["issue fifteen", "issue eighteen"]);
      // …and the toggle reverses Closed to desc [18,15]. This is the case the old iid pin
      // could never produce (it re-sorted Closed to iid order in every mode).
      fireEvent.click(dirBtn());
      const closedDesc = titlesIn(laneFor("Closed"));
      expect(closedDesc).toEqual(["issue eighteen", "issue fifteen"]);
      // Non-vacuous: the reversal really changed the lane.
      expect(closedDesc).not.toEqual(closedAsc);
    });

    // 5. Post-drop Closed transition — the accepted S4 behaviour (Design Decision 5, and
    // the rewritten S4 comment in Board.tsx cardsByColumn). A drop calls setSortMode(
    // "manual"); because the drop-freeze excludes closed cards, the OPEN lanes keep their
    // just-frozen order while the CLOSED lane alone reverts from mode+dir order back to iid
    // ascending. This isolated Closed reversion is the KNOWN, ACCEPTED cost of de-pinning —
    // asserted here so a future reader does NOT re-add the pin to "fix" it (doing so
    // silently reverts #412).
    it("reverts the Closed lane to iid order after a drop flips the board to Manual", async () => {
      // Both endpoints return the two-closed board so the closed cards survive the reorder.
      mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: twoClosed() }) });
      mockApi.reorderBoard.mockResolvedValue({ board: aBoard({ cards: twoClosed() }) });
      renderBoard();
      await screen.findByText("Backlog");

      // iid + toggle → desc: Closed shows [18,15] (mode+dir applied).
      setSort("iid");
      fireEvent.click(dirBtn());
      expect(titlesIn(laneFor("Closed"))).toEqual(["issue eighteen", "issue fifteen"]);

      // Reorder an OPEN card. Backlog renders iid-desc here = [2,1], so card 1 is last and
      // its "up" is the valid move (its "down" is disabled at the end of the lane).
      fireEvent.click(moveBtn(1, "up"));
      await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
      // The board is now Manual…
      await waitFor(() => expect(store.get("uzi.board.repo-1.sortMode")).toBe('"manual"'));
      // …and the Closed lane has reverted to iid-ascending [15,18] — Manual is identity over
      // the iid-ordered payload, so the desc order the toggle produced is gone. Accepted.
      expect(titlesIn(laneFor("Closed"))).toEqual(["issue fifteen", "issue eighteen"]);
    });
  });

  // #1060 — a background poll's getBoard can be IN FLIGHT when a card mutation
  // (reorder, column move, close/reopen) applies its authoritative card. Without a
  // guard the older poll response resolves LATER and setBoard(fresh) clobbers the
  // just-applied mutation with stale pre-mutation state (self-healing only ~10s later
  // on the next poll). The fix is a mutation-generation counter bumped at the START
  // AND the SETTLE of every real mutation; poll() captures it before its await and
  // discards any response whose counter moved.
  //
  // These are deterministic — no fake or real timers, no sleeps, no wall-clock. The
  // poll's getBoard is held on a captured resolver (the same deferred-promise idiom
  // the reorder-lock tests use above); the mutation is applied while it hangs, then
  // the poll is released with a STALE board and must be DROPPED. The trailing control
  // test fires a poll that STARTS AFTER the mutation settled and proves that one DOES
  // apply, so the guard is not vacuously passing.
  describe("Board — a late poll does not clobber a just-applied mutation (#1060)", () => {
    // Rendered-order readers (same shape as the outer suite's at :640 / the direction
    // toggle's at :1115). Closed cards DO render an anchor matching /^issue /.
    // queryAllByRole (not getAllByRole) so a lane that ends up EMPTY — e.g. Closed
    // after the only closed card is reopened — reads as [] for a `.not.toContain`
    // assertion instead of throwing on zero matches.
    const titlesInLane = (label: string) =>
      within(laneFor(label))
        .queryAllByRole("link", { name: /^issue / })
        .map((a) => a.textContent);

    // The mount's load() getBoard has already resolved (the test awaits findByText
    // "Backlog" first), so the NEXT getBoard is the poll's. Firing visibilitychange
    // (jsdom's document.hidden is false) runs the poll synchronously up to its await,
    // which is the moment poll() captures the mutation generation — its getBoard then
    // hangs on the resolver we return.
    async function firePollAndCapture(): Promise<(v: { board: BoardData }) => void> {
      let release: (v: { board: BoardData }) => void = () => {};
      mockApi.getBoard.mockImplementationOnce(
        () => new Promise<{ board: BoardData }>((res) => { release = res; }),
      );
      fireEvent(document, new Event("visibilitychange"));
      // getBoard #1 was the mount load(); #2 is this poll (now hung on `release`).
      await waitFor(() => expect(mockApi.getBoard).toHaveBeenCalledTimes(2));
      return release;
    }

    // Resolve the hung poll and let its continuation (the guard, then any setBoard)
    // fully drain before the assertion — on the FIXED path there is no state change to
    // waitFor, so the microtasks are flushed explicitly inside act().
    async function releasePollWith(release: (v: { board: BoardData }) => void, board: BoardData) {
      await act(async () => {
        release({ board });
        await Promise.resolve();
        await Promise.resolve();
      });
    }

    it("keeps a within-lane reorder when a poll started before it resolves stale", async () => {
      // Authoritative post-reorder Backlog order: card 1 moved one place down.
      const reordered = aBoard({
        cards: [
          aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"] }),
          aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"] }),
          aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
          aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
          aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"] }),
        ],
      });
      mockApi.reorderBoard.mockResolvedValue({ board: reordered });

      renderBoard();
      await screen.findByText("Backlog");

      // A poll's getBoard is now in flight (its generation captured at 0).
      const release = await firePollAndCapture();

      // Apply the reorder — it bumps the generation twice (start + settle).
      fireEvent.click(moveBtn(1, "down"));
      await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
      await waitFor(() =>
        expect(titlesInLane("Backlog")).toEqual(["issue two", "issue one", "issue three", "issue four"]),
      );

      // Release the poll with the STALE pre-reorder order — it must be DROPPED.
      await releasePollWith(release, aBoard());

      expect(titlesInLane("Backlog")).toEqual(["issue two", "issue one", "issue three", "issue four"]);
    });

    it("keeps a column move when a poll started before it resolves stale", async () => {
      // Authoritative board: card 1 now lives in Planned.
      const moved = aBoard({
        cards: [
          aCard({ iid: 1, title: "issue one", column: "Planned", labels: ["uzi"] }),
          aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"] }),
          aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
          aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
          aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"] }),
        ],
      });
      mockApi.moveIssue.mockResolvedValue({ card: aCard({ iid: 1, title: "issue one", column: "Planned", labels: ["uzi"] }) });
      mockApi.reorderBoard.mockResolvedValue({ board: moved });

      renderBoard();
      await screen.findByText("Backlog");

      const release = await firePollAndCapture();

      // Drag card 1 onto Planned — a cross-column drop (move() then reorderBoard),
      // bumping the generation at start + settle.
      dropOn(laneFor("Planned"), 1);
      await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(titlesInLane("Planned")).toContain("issue one"));

      // Release the poll with the STALE board (card 1 still in Backlog) — dropped.
      await releasePollWith(release, aBoard());

      expect(titlesInLane("Planned")).toContain("issue one");
      expect(titlesInLane("Backlog")).not.toContain("issue one");
    });

    it("keeps a close when a poll started before it resolves stale", async () => {
      mockApi.moveIssue.mockResolvedValue({
        card: aCard({ iid: 1, title: "issue one", column: "", closed: true, labels: ["uzi"] }),
      });

      renderBoard();
      await screen.findByText("Backlog");

      const release = await firePollAndCapture();

      // Drag open card 1 onto Closed — a close (move() alone), bumping the generation
      // at start + settle.
      dropOn(laneFor("Closed"), 1);
      await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 1, "closed"));
      await waitFor(() => expect(titlesInLane("Closed")).toContain("issue one"));

      // Release the poll with the STALE board (card 1 still open in Backlog) — dropped.
      await releasePollWith(release, aBoard());

      expect(titlesInLane("Closed")).toContain("issue one");
      expect(titlesInLane("Backlog")).not.toContain("issue one");
    });

    it("keeps a reopen when a poll started before it resolves stale", async () => {
      // Card 99 (title "issue closed") is closed in the mount board; reopen it into Planned.
      mockApi.moveIssue.mockResolvedValue({
        card: aCard({ iid: 99, title: "issue closed", column: "Planned", closed: false, labels: ["uzi"] }),
      });

      renderBoard();
      await screen.findByText("Backlog");

      const release = await firePollAndCapture();

      // Drag closed card 99 onto Planned — a reopen (move() alone via the closed
      // branch), bumping the generation at start + settle.
      dropOn(laneFor("Planned"), 99);
      await waitFor(() => expect(mockApi.moveIssue).toHaveBeenCalledWith("repo-1", 99, "Planned"));
      await waitFor(() => expect(titlesInLane("Planned")).toContain("issue closed"));

      // Release the poll with the STALE board (card 99 still closed) — dropped.
      await releasePollWith(release, aBoard());

      expect(titlesInLane("Planned")).toContain("issue closed");
      expect(titlesInLane("Closed")).not.toContain("issue closed");
    });

    // Control: a poll whose getBoard STARTS AFTER the mutation has settled captures the
    // already-bumped generation, so its response is NOT discarded — proving the guard
    // is not vacuously dropping every poll.
    it("APPLIES a poll that starts after the mutation settled (control)", async () => {
      const reordered = aBoard({
        cards: [
          aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"] }),
          aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"] }),
          aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
          aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
          aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"] }),
        ],
      });
      mockApi.reorderBoard.mockResolvedValue({ board: reordered });

      renderBoard();
      await screen.findByText("Backlog");

      // Settle the mutation FIRST.
      fireEvent.click(moveBtn(1, "down"));
      await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
      await waitFor(() =>
        expect(titlesInLane("Backlog")).toEqual(["issue two", "issue one", "issue three", "issue four"]),
      );

      // NOW start the poll — it captures the already-bumped generation.
      const release = await firePollAndCapture();

      // A fresh server board (Backlog 3,4,1,2). No mutation ran while this poll was in
      // flight, so its generation is unchanged and it MUST apply.
      const serverFresh = aBoard({
        cards: [
          aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
          aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
          aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"] }),
          aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"] }),
          aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"] }),
        ],
      });
      await releasePollWith(release, serverFresh);

      expect(titlesInLane("Backlog")).toEqual(["issue three", "issue four", "issue one", "issue two"]);
    });

    // #1060 (rework, !1119 review): promote() and refresh() ALSO replace board state
    // with an authoritative response but did NOT bump the generation, so an in-flight
    // poll still clobbered them — the same defect the applyDrop guard closes. These
    // two prove the bump now extends to those flows.
    it("keeps a promote when a poll started before it resolves stale", async () => {
      // A non-uzi card (5) renders a Promote button only when "show all" is on;
      // promoting it adds the uzi label and swaps Promote → Start run.
      const promotable = aBoard({
        cards: [
          aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"] }),
          aCard({ iid: 5, title: "issue five", column: "", labels: [] }),
        ],
      });
      mockApi.getBoard.mockResolvedValue({ board: promotable });
      mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: true });
      mockApi.promoteIssue.mockResolvedValue({
        card: aCard({ iid: 5, title: "issue five", column: "", labels: ["uzi"] }),
      });

      renderBoard();
      await screen.findByText("Backlog");
      // Card 5 is promotable before we act — the control that the button is reachable.
      expect(screen.getByRole("button", { name: /Promote to uzi/ })).toBeTruthy();

      const release = await firePollAndCapture();

      // Promote card 5 — bumps the generation at start + settle.
      fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
      await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 5));
      await waitFor(() =>
        expect(screen.queryByRole("button", { name: /Promote to uzi/ })).toBeNull(),
      );

      // Release the poll with the STALE board (card 5 still non-uzi) — must be DROPPED,
      // so the promoted card does not revert to its promotable state.
      await releasePollWith(release, promotable);

      expect(screen.queryByRole("button", { name: /Promote to uzi/ })).toBeNull();
    });

    it("keeps a refresh when a poll started before it resolves stale", async () => {
      // Refresh (syncRepo) returns an authoritative board with card 1 moved to Planned.
      const synced = aBoard({
        cards: [
          aCard({ iid: 1, title: "issue one", column: "Planned", labels: ["uzi"] }),
          aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi"] }),
          aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
          aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
          aCard({ iid: 9, title: "issue nine", column: "Planned", labels: ["uzi"] }),
        ],
      });
      mockApi.syncRepo.mockResolvedValue({ board: synced });

      renderBoard();
      await screen.findByText("Backlog");

      const release = await firePollAndCapture();

      // Refresh — bumps the generation at start + settle.
      fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
      await waitFor(() => expect(mockApi.syncRepo).toHaveBeenCalledWith("repo-1"));
      await waitFor(() => expect(titlesInLane("Planned")).toContain("issue one"));

      // Release the poll with the STALE pre-refresh board (card 1 still in Backlog) — dropped.
      await releasePollWith(release, aBoard());

      expect(titlesInLane("Planned")).toContain("issue one");
      expect(titlesInLane("Backlog")).not.toContain("issue one");
    });
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
describe("Board — non-uzi issues (PRD #764)", () => {
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

  // uzi and non-uzi cards INTERLEAVED by iid in one lane. The interleaving is
  // load-bearing for the freeze case: with the non-uzi cards grouped at one end, an
  // implementation that appended them rather than keeping their positions passes.
  const cards = () => [
    aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"], has_prd_link: false }),
    aCard({ iid: 2, title: "issue two", column: "", labels: ["bug"], has_prd_link: false }),
    aCard({ iid: 3, title: "issue three", column: "", labels: ["uzi"] }),
    aCard({ iid: 4, title: "issue four", column: "", labels: [] }),
    aCard({ iid: 5, title: "issue five", column: "", labels: ["uzi"] }),
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
    bot_forge_user_id: 0,
    ...over,
  });

  beforeEach(() => {
    store = installStorage();
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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
      card: aCard({ iid: 2, title: "issue two", column: "", labels: ["uzi", "bug"], has_prd_link: false }),
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

  it("shows only the uzi (runnable) cards by default (PRD #764)", async () => {
    // Membership is the single `uzi` label, so a fresh board shows only the uzi cards.
    // The bug-only card, the unlabelled issue four and the tracker stay off until "Show
    // all other issues" is ticked.
    renderBoard();
    await screen.findByText("Backlog");
    expect(titles()).toEqual(["issue one", "issue three", "issue five"]);
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
    // Show-all OFF: the viewer sees the uzi cards (1, 3, 5) and moves 5 up one place.
    // The hidden non-uzi cards #2 and #4 stay hidden but must still freeze.
    expect(titles()).toEqual(["issue one", "issue three", "issue five"]);
    fireEvent.click(screen.getByRole("button", { name: /Move issue #5 up in/ }));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));

    const iids = mockApi.reorderBoard.mock.calls[0][1] as number[];
    // The hidden non-uzi cards are IN the freeze — omit them and the server's
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

  it("draws a non-uzi card quiet and a uzi card solid (Decision 17, PRD #764)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    // Show all brings the non-uzi cards (the bug card #2 and the unlabelled #4) onto the board.
    clickShowAll();

    // classList, not className: these are TOKEN assertions, and a substring check
    // reports "border-edge" present on any card carrying hover:border-edge-strong,
    // which every draggable card does. That is not hypothetical — the first version
    // of this test failed for exactly that reason against correct markup.
    const nonUziUnlabelled = cardOf("issue four");
    expect(nonUziUnlabelled.classList.contains("border-dashed")).toBe(true);
    expect(nonUziUnlabelled.classList.contains("border-faint")).toBe(true);
    expect(nonUziUnlabelled.classList.contains("border-edge")).toBe(false);

    // issue two (bug, no `uzi`) is NOT runnable now — a selector without `uzi` — so it
    // renders quiet/dashed, not solid.
    const bug = cardOf("issue two");
    expect(bug.classList.contains("border-dashed")).toBe(true);
    expect(bug.classList.contains("border-faint")).toBe(true);
    expect(bug.classList.contains("border-edge")).toBe(false);

    // A uzi card is solid, border-edge.
    const uzi = cardOf("issue one");
    expect(uzi.classList.contains("border-edge")).toBe(true);
    expect(uzi.classList.contains("border-faint")).toBe(false);
    expect(uzi.classList.contains("border-dashed")).toBe(false);
  });

  it("offers Start run on uzi cards and Promote on the non-uzi ones (Decision 15, PRD #764)", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    clickShowAll();
    const lane = screen.getByText("Backlog").parentElement!.parentElement!;
    // uzi cards (1, 3, 5) offer Start run; the non-uzi cards (#2 bug and unlabelled #4)
    // offer Promote instead.
    expect(within(lane).getAllByRole("button", { name: /Start run/ })).toHaveLength(3);
    expect(within(lane).getAllByRole("button", { name: /Promote to uzi/ })).toHaveLength(2);
  });

  it("makes Start ENABLED on a uzi card with no PRD link once a worker + token exist (PRD #764)", async () => {
    // A run no longer requires a PRD link (PRD #764), so a worker + token is all it takes.
    withWorkerAndToken();
    renderBoard();
    await screen.findByText("Backlog");
    // issue one is a uzi card with has_prd_link:false (explicit override) → runnable
    // WITHOUT a PRD link, which is exactly the #764 guarantee this test asserts.
    const start = within(cardOf("issue one")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
  });

  // Issue #856 M3: a completed prior run that still owns an open MR makes the
  // server refuse a fresh run with a coded 409 (issue_has_open_mr). Start catches
  // it, confirms (the message names the MR), and retries with force on confirm.
  const openMRError = () =>
    new ApiError(
      409,
      "issue #1 already has open MR !42 — merge or close it, or leave review comments on the MR to iterate, before starting a new run (pass --force to re-run anyway)",
      { code: "issue_has_open_mr", mr_iid: 42 },
    );

  const renderBoardWithRunRoute = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
          <Route path="/runs/:runId" element={<div>run page</div>} />
        </Routes>
      </MemoryRouter>,
    );

  it("on the open-MR 409, confirms and retries Start with force, then navigates (#856)", async () => {
    withWorkerAndToken();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    mockApi.createRun
      .mockRejectedValueOnce(openMRError())
      .mockResolvedValueOnce({ run: { id: "run-9" } as unknown as import("../lib/api").Run });
    renderBoardWithRunRoute();
    await screen.findByText("Backlog");
    const start = within(cardOf("issue one")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
    fireEvent.click(start);

    // The confirm is web-composed: it names the MR, states the action, and carries
    // no CLI --force jargon (issue #856).
    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    const confirmMsg = confirmSpy.mock.calls[0][0] as string;
    expect(confirmMsg).toContain("!42");
    expect(confirmMsg).toContain("Start a new run anyway?");
    expect(confirmMsg).not.toContain("--force");
    // Retried with force === true, and the run page opened.
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledTimes(2));
    expect(mockApi.createRun.mock.calls[0]).toEqual(["repo-1", 1, undefined]);
    expect(mockApi.createRun.mock.calls[1]).toEqual(["repo-1", 1, true]);
    await screen.findByText("run page");
    confirmSpy.mockRestore();
  });

  it("on the open-MR 409, declining Start does not retry and shows no error (#856)", async () => {
    withWorkerAndToken();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockApi.createRun.mockRejectedValueOnce(openMRError());
    renderBoard();
    await screen.findByText("Backlog");
    const start = within(cardOf("issue one")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
    fireEvent.click(start);

    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    // No retry, and the coded-conflict message is NOT shown as an error toast.
    expect(mockApi.createRun).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/already has open MR/)).toBeNull();
    // The Start control is re-enabled after decline (starting state cleared): a
    // stuck-in-"Starting…" regression would leave it disabled and fail here.
    await waitFor(() => {
      const start2 = within(cardOf("issue one")).getByRole("button", {
        name: /Start run/,
      }) as HTMLButtonElement;
      expect(start2.disabled).toBe(false);
    });
    expect(within(cardOf("issue one")).queryByText("Starting…")).toBeNull();
    confirmSpy.mockRestore();
  });

  it("on the open-MR 409, a confirmed forced retry that fails shows the toast and clears starting (#856)", async () => {
    withWorkerAndToken();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    mockApi.createRun
      .mockRejectedValueOnce(openMRError())
      .mockRejectedValueOnce(new ApiError(500, "boom while forcing"));
    renderBoard();
    await screen.findByText("Backlog");
    const start = within(cardOf("issue one")).getByRole("button", { name: /Start run/ }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
    fireEvent.click(start);

    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    // Retried with force === true, that retry failed, so the toast shows the retry
    // error and the starting state is cleared (button re-enabled).
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledTimes(2));
    expect(mockApi.createRun.mock.calls[1]).toEqual(["repo-1", 1, true]);
    await screen.findByText("boom while forcing");
    await waitFor(() => {
      const start2 = within(cardOf("issue one")).getByRole("button", {
        name: /Start run/,
      }) as HTMLButtonElement;
      expect(start2.disabled).toBe(false);
    });
    confirmSpy.mockRestore();
  });

  it("promotes forge-first and adopts the returned card", async () => {
    // Promote a non-uzi card, the unlabelled #4.
    mockApi.promoteIssue.mockResolvedValue({
      card: aCard({ iid: 4, title: "issue four", column: "", labels: ["uzi"] }),
    });
    renderBoard();
    await screen.findByText("Backlog");
    // Show all so the non-uzi cards are on the board with their Promote buttons.
    clickShowAll();
    // Two Promote buttons (the bug card and the unlabelled one); click the unlabelled #4's.
    fireEvent.click(within(cardOf("issue four")).getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 4));
    // The promoted card is now a uzi card, so exactly one Promote button remains (#2's).
    await waitFor(() => expect(screen.getAllByRole("button", { name: /Promote to uzi/ })).toHaveLength(1));
    // …and it survives show-all going back off (it carries `uzi` now).
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
  const aRun = (over: {
    id: string;
    status: string;
    health: string;
    issue_iid: number;
    is_revising?: boolean;
  }) =>
    // Issue #485 review FIX 2: the strip's overlay <Link> is now named by the run title
    // as well, so the fixture must carry an issue_title the way the real RunListItem does
    // (RunListItem.issue_title is a non-optional string; this cast used to omit it because
    // the strip never read it before).
    ({ issue_title: "Fix the parser", ...over }) as unknown as import("../lib/api").RunListItem;

  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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

  const strip = () =>
    screen.getByText(/needs approval|needs an answer|awaiting follow-up|looks? stuck|look stuck/);

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
    // Issue #485 NB1: the pill's navigational element is now a stretched-link overlay
    // named by its aria-label (the forge ref anchor is a sibling, not a nested <a>), so
    // the per-run chip is this "Open run for issue #7: Fix the parser" link (FIX 2 adds
    // the title to the accessible name) — still exactly one.
    expect(screen.getAllByRole("link", { name: "Open run for issue #7: Fix the parser" })).toHaveLength(1);
  });

  it("still lists a genuinely stuck run — the control", async () => {
    // A running run flagged stalled belongs to the stuck bucket and nothing else. Without
    // this, a predicate that simply returned [] would pass the test above.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-2", status: "running", health: "stalled", issue_iid: 7 })],
    });
    renderBoard();
    await screen.findByText("1 run looks stuck");
    expect(screen.getAllByRole("link", { name: "Open run for issue #7: Fix the parser" })).toHaveLength(1);
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
    expect(screen.getAllByRole("link", { name: /Open run for issue #(7|8)/ })).toHaveLength(2);
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
    expect(screen.getAllByRole("link", { name: "Open run for issue #7: Fix the parser" })).toHaveLength(1);
  });

  it("counts an awaiting_followup run and offers it a jump-chip (PRD #517)", async () => {
    // A follow-up park is a needs-you state carrying the same loud ring (needsHumanAttention)
    // as the two parks above, so the strip must tally it and jump-link it. Reverting the
    // followupRuns bucket drops both the clause and the chip — the strip then undercounts the
    // visible loud card.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-4", status: "awaiting_followup", health: "ok", issue_iid: 7 })],
    });
    renderBoard();
    await screen.findByText("1 run awaiting follow-up");
    expect(strip().textContent).toBe("1 run awaiting follow-up");
    expect(screen.getAllByRole("link", { name: "Open run for issue #7: Fix the parser" })).toHaveLength(1);
  });

  it("drops a REVISING run out of the attention strip, then returns it (issue #750)", async () => {
    // A run re-planning after a revise keeps status === "awaiting_approval" server-side,
    // but its derived is_revising flag flips the EFFECTIVE status to "revising", so the
    // strip's isAwaitingApproval(effectiveRunStatus(r)) predicate excludes it. With
    // nothing else parked, the whole strip is absent.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-5", status: "awaiting_approval", health: "ok", issue_iid: 7, is_revising: true })],
    });
    const view = renderBoard();
    // The board card for issue #7 renders (the fixture card), so wait on it rather than the strip.
    await screen.findByText("#7");
    expect(
      screen.queryByText(/needs approval|needs an answer|awaiting follow-up|looks? stuck|look stuck/),
    ).toBeNull();
    expect(screen.queryByRole("link", { name: "Open run for issue #7: Fix the parser" })).toBeNull();

    // Once the next plan lands (is_revising false), the SAME run returns to needs-approval.
    view.unmount();
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "run-5", status: "awaiting_approval", health: "ok", issue_iid: 7, is_revising: false })],
    });
    renderBoard();
    await screen.findByText("1 run needs approval");
    expect(screen.getAllByRole("link", { name: "Open run for issue #7: Fix the parser" })).toHaveLength(1);
  });

  it("keeps awaiting_followup in its OWN clause, separate from the answer count (PRD #517)", async () => {
    // A follow-up park is not an unanswered question, so it is its own bucket rather than
    // folded into questionRuns: two distinct clauses, two distinct chips.
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "run-3", status: "awaiting_input", health: "ok", issue_iid: 7 }),
        aRun({ id: "run-4", status: "awaiting_followup", health: "ok", issue_iid: 8 }),
      ],
    });
    renderBoard();
    await screen.findByText("1 run needs an answer · 1 run awaiting follow-up");
    expect(screen.getAllByRole("link", { name: /Open run for issue #(7|8)/ })).toHaveLength(2);
  });
});

// PRD #304 — board search + per-lane paging, the WIRING layer. The paging helper
// (boardColumns.ts) and matchesQuery/highlightSegments (boardCards.ts) are unit-tested
// at the lib level; these assert Board.tsx feeds them the right sets and renders their
// results — most load-bearing, the freeze still uses the UNFILTERED payload while a cap
// or a search hides cards (Decision 2, the same trap PRD #102's freeze tests guard).
describe("Board — search + per-lane paging (PRD #304)", () => {
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

  const aBoard = (over: Partial<BoardData> = {}): BoardData => ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [{ label_name: "Planned" }] as BoardData["columns"],
    cards: [aCard({ iid: 1, title: "issue one", column: "", labels: ["uzi"] })],
    pipeline: null,
    bot_forge_user_id: 0,
    ...over,
  });

  // n PRD cards in the Backlog lane, titled "issue 1".."issue n". A generator because
  // the cap tests need more cards than a lane fits without a wall of literals.
  const backlog = (n: number): Card[] =>
    Array.from({ length: n }, (_, k) => aCard({ iid: k + 1, title: `issue ${k + 1}`, column: "", labels: ["uzi"] }));

  beforeEach(() => {
    store = installStorage();
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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
  const cardCount = () => screen.getAllByRole("link", { name: /^issue \d/ }).length;
  const search = () => screen.getByRole("searchbox");

  // --- M1/M6: per-lane cap, Show more, Collapse, N/M count ---
  it("caps a lane at the per-lane default and reveals a page with Show more, then Collapse resets", async () => {
    // 12 cards, default cap 10: the lane shows 10 and a "Show 2 more" expander.
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: backlog(12) }) });
    renderBoard();
    await screen.findByText("Backlog");

    expect(cardCount()).toBe(10);
    // The capped lane's count badge reads render/total (N/M), not the bare total.
    expect(within(laneFor("Backlog")).getByText("10/12")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Show 2 more/ }));
    expect(cardCount()).toBe(12);
    // Fully revealed: no Show more left, but a Collapse is now offered.
    expect(screen.queryByRole("button", { name: /Show \d+ more/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /Collapse/ }));
    expect(cardCount()).toBe(10);
  });

  it("does not cap or offer Show more on a lane at or under the default", async () => {
    // The control for the cap test: a small lane renders in full with no expander and
    // its badge is the bare count, not N/M.
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: backlog(4) }) });
    renderBoard();
    await screen.findByText("Backlog");
    expect(cardCount()).toBe(4);
    expect(screen.queryByRole("button", { name: /Show \d+ more/ })).toBeNull();
    expect(within(laneFor("Backlog")).getByText("4")).toBeTruthy();
  });

  // --- M2/M6: search filters, drops empty lanes, board-level count, clearing restores ---
  it("filters lanes to matching cards, drops emptied lanes, and shows a result count", async () => {
    const cards = [
      aCard({ iid: 1, title: "alpha one", column: "", labels: ["uzi"] }),
      aCard({ iid: 2, title: "beta two", column: "", labels: ["uzi"] }),
      aCard({ iid: 3, title: "beta three", column: "Planned", labels: ["uzi"] }),
    ];
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards }) });
    renderBoard();
    await screen.findByText("Backlog");
    // Off-search: every card and both working lanes are present.
    expect(screen.getByText("beta two")).toBeTruthy();
    expect(screen.getByText("Planned")).toBeTruthy();

    fireEvent.change(search(), { target: { value: "alpha" } });
    // Only the matching card survives; the non-matching one drops. The title is queried
    // by the link's accessible NAME (the full title) rather than getByText, because the
    // matched substring is split into a <mark> + text and no single node holds it whole.
    expect(screen.getByRole("link", { name: "alpha one" })).toBeTruthy();
    expect(screen.queryByText("beta two")).toBeNull();
    // The Planned lane held only a non-match, so the lane itself drops during search.
    expect(screen.queryByText("Planned")).toBeNull();
    // A board-level result count renders.
    expect(screen.getByText("1 result")).toBeTruthy();

    // Clearing restores the full board — the dropped card and lane return.
    fireEvent.change(search(), { target: { value: "" } });
    expect(screen.getByText("beta two")).toBeTruthy();
    expect(screen.getByText("Planned")).toBeTruthy();
    expect(screen.queryByText("1 result")).toBeNull();
  });

  it("matches on #iid and pluralizes the result count", async () => {
    const cards = [
      aCard({ iid: 42, title: "answer everything", column: "", labels: ["uzi"] }),
      aCard({ iid: 7, title: "lucky", column: "", labels: ["uzi"] }),
    ];
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards }) });
    renderBoard();
    await screen.findByText("Backlog");
    // "#4" is a substring of iid 42 by design; iid 7 does not contain it.
    fireEvent.change(search(), { target: { value: "#4" } });
    expect(screen.getByText("answer everything")).toBeTruthy();
    expect(screen.queryByText("lucky")).toBeNull();
    expect(screen.getByText("1 result")).toBeTruthy();
  });

  // --- M2/M6: highlight in both title and chip ---
  it("wraps the matched substring in a <mark> in both the title and a matching chip", async () => {
    const cards = [aCard({ iid: 1, title: "find alpha here", column: "", labels: ["uzi", "alpha-tag"] })];
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards }) });
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.change(search(), { target: { value: "alpha" } });

    // One hit in the title, one in the chip label — both real <mark> elements carrying
    // the matched substring (semantic, not color-only).
    const marks = screen.getAllByText("alpha");
    expect(marks).toHaveLength(2);
    marks.forEach((m) => expect(m.tagName).toBe("MARK"));
  });

  // --- M4/M6: per-lane preference persists and re-reads on repo change ---
  it("persists the per-lane default to localStorage", async () => {
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.change(screen.getByRole("combobox", { name: "Per lane" }), { target: { value: "5" } });
    expect(store.get("uzi.board.repo-1.perLane")).toBe("5");
  });

  it("re-reads the per-lane default when the route repo id changes without a remount", async () => {
    // The route swaps :id without remounting Board, so a lazy useState initialiser only
    // ran for the first repo — the trap this file's sortMode/hideEmpty comments name.
    // repo-2 has a saved value of 20; navigating to it must adopt that value.
    store.set("uzi.board.repo-2.perLane", "20");
    const NavProbe = () => {
      const nav = useNavigate();
      return <button onClick={() => nav("/repos/repo-2/board")}>go repo2</button>;
    };
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
        <NavProbe />
      </MemoryRouter>,
    );
    await screen.findByText("Backlog");
    // repo-1 has no saved value → the default 10.
    expect((screen.getByRole("combobox", { name: "Per lane" }) as HTMLSelectElement).value).toBe("10");

    fireEvent.click(screen.getByRole("button", { name: "go repo2" }));
    await waitFor(() =>
      expect((screen.getByRole("combobox", { name: "Per lane" }) as HTMLSelectElement).value).toBe("20"),
    );
  });

  // Same route-swap trap for sortMode: the [repoId] effect at Board.tsx re-reads the
  // persisted mode when :id changes without a remount. repo-1 has no saved mode (default
  // "manual"); repo-2 saved "title". Delete the sortMode re-read effect and the combobox
  // stays "manual" after the swap → this fails.
  it("re-reads the persisted sortMode when the route repo id changes without a remount", async () => {
    store.set("uzi.board.repo-2.sortMode", '"title"');
    const NavProbe = () => {
      const nav = useNavigate();
      return <button onClick={() => nav("/repos/repo-2/board")}>go repo2</button>;
    };
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
        <NavProbe />
      </MemoryRouter>,
    );
    await screen.findByText("Backlog");
    // repo-1 has no saved mode → the default "manual".
    expect((screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement).value).toBe("manual");

    fireEvent.click(screen.getByRole("button", { name: "go repo2" }));
    await waitFor(() =>
      expect((screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement).value).toBe("title"),
    );
  });

  // Same route-swap trap for sortDir: the [repoId] effect re-reads the persisted direction
  // for the new repo. repo-2 saved mode "updated" (DEFAULT_SORT_DIR desc) with sortDir
  // "asc" — the OPPOSITE of the mode default, which is what makes the direction assertion
  // non-vacuous: only the re-read effect can produce ascending. Delete the sortDir re-read
  // effect and the toggle stays at repo-1's manual/descending after the swap → this fails.
  it("re-reads the persisted sortDir when the route repo id changes without a remount", async () => {
    store.set("uzi.board.repo-2.sortMode", '"updated"');
    store.set("uzi.board.repo-2.sortDir", '"asc"');
    const NavProbe = () => {
      const nav = useNavigate();
      return <button onClick={() => nav("/repos/repo-2/board")}>go repo2</button>;
    };
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
        <NavProbe />
      </MemoryRouter>,
    );
    await screen.findByText("Backlog");
    // repo-1 has no saved mode → Manual, so the direction toggle is present but disabled.
    expect((screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement).value).toBe("manual");
    expect((screen.getByRole("button", { name: /Sort direction/ }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "go repo2" }));
    // repo-2's persisted mode "updated" adopts, and its persisted "asc" (opposite of the
    // updated=desc default) adopts too — so the toggle reads ascending and is enabled.
    await waitFor(() =>
      expect((screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement).value).toBe("updated"),
    );
    const dir = screen.getByRole("button", { name: /Sort direction/ }) as HTMLButtonElement;
    expect(dir.disabled).toBe(false);
    expect(dir.textContent).toBe("↑ Ascending");
    expect(dir.getAttribute("aria-pressed")).toBe("false");
  });

  // --- M6: THE freeze guard. Cap and/or search hide cards from the render set; the
  // reorder freeze must still submit the UNFILTERED payload order (Decision 2). ---
  it("freezes hidden-by-cap cards: reorder submits iids beyond the visible cap", async () => {
    // 12 cards, default cap 10 → #11 and #12 are hidden. Moving a visible card must
    // still submit every open card's iid, including the two the cap hid.
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards: backlog(12) }) });
    renderBoard();
    await screen.findByText("Backlog");
    expect(cardCount()).toBe(10);

    fireEvent.click(screen.getByRole("button", { name: "Move issue #1 down in Backlog" }));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    const iids = mockApi.reorderBoard.mock.calls[0][1] as number[];
    // The hidden cards are IN the freeze — omit them and the server NULLs their
    // positions, dropping them on any other view of this board.
    expect(iids).toContain(11);
    expect(iids).toContain(12);
    expect(iids).toHaveLength(12);
  });

  it("freezes hidden-by-search cards: reorder submits iids the query filtered out", async () => {
    const cards = [
      aCard({ iid: 1, title: "alpha one", column: "", labels: ["uzi"] }),
      aCard({ iid: 2, title: "beta two", column: "", labels: ["uzi"] }),
      aCard({ iid: 3, title: "alpha three", column: "", labels: ["uzi"] }),
    ];
    mockApi.getBoard.mockResolvedValue({ board: aBoard({ cards }) });
    renderBoard();
    await screen.findByText("Backlog");
    // Search hides #2 (beta); the two alpha cards remain and one can be reordered.
    fireEvent.change(search(), { target: { value: "alpha" } });
    expect(screen.queryByText("beta two")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Move issue #1 down in Backlog" }));
    await waitFor(() => expect(mockApi.reorderBoard).toHaveBeenCalledTimes(1));
    const iids = mockApi.reorderBoard.mock.calls[0][1] as number[];
    // The search-hidden card is still frozen with the rest — payloadCards is unfiltered.
    expect(iids).toContain(2);
    expect(iids).toHaveLength(3);
  });
});

// PRD #318 M2 — the COLUMNS settings editor gains grip-handle drag-and-drop reorder,
// replacing the ↑/↓ arrow buttons. These mount the whole Board (the editor is opened
// from the toolbar) so the assertions run against the real ColumnSettings tree.
describe("ColumnSettings reorder (PRD #318 M2)", () => {
  const aBoard = (over: Partial<BoardData> = {}): BoardData => ({
    repo_id: "repo-1",
    path_with_namespace: "grp/proj",
    web_url: "https://gitlab.example.com/grp/proj",
    forge_type: "gitlab",
    columns: [
      { label_name: "Planned" },
      { label_name: "In Progress" },
    ] as BoardData["columns"],
    cards: [aCard({ iid: 7, column: "In Progress", labels: ["uzi", "In Progress", "bug"] })],
    pipeline: null,
    bot_forge_user_id: 0,
    ...over,
  });

  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: null,
      loading: false,
      uziLabel: "uzi",
      autopilotLabel: "autopilot",
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
    mockApi.configureColumns.mockResolvedValue({ board: aBoard() });
  });

  const renderBoard = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <Routes>
          <Route path="/repos/:id/board" element={<Board />} />
        </Routes>
      </MemoryRouter>,
    );

  // The settings panel is the Card whose heading is the <SectionTitle>Columns</SectionTitle>
  // h2 — its parent element wraps the whole editor (rows, inputs, Save button). Anchoring
  // to the heading avoids matching the board lanes, which repeat every column label.
  const openSettings = async () => {
    renderBoard();
    await screen.findByText("Backlog");
    fireEvent.click(screen.getByRole("button", { name: "Columns" }));
    const panel = screen.getByRole("heading", { name: "Columns", level: 2 })
      .parentElement as HTMLElement;
    return panel;
  };

  it("renders a drag grip on each column row", async () => {
    const panel = await openSettings();
    // GripVerticalIcon is aria-hidden, so query structurally: it is the only <svg> the
    // editor renders (the Remove/Add/Save buttons and suggestion chips are text-only),
    // one per column row.
    const grips = panel.querySelectorAll("svg");
    expect(grips.length).toBe(2);
    // Guard against a stray non-grip svg slipping in: the grip is a 2×3 dot grid, six
    // <circle>s, so every column contributes six circles.
    expect(panel.querySelectorAll("circle").length).toBe(12);
  });

  it("no longer exposes the ↑/↓ arrow reorder buttons", async () => {
    const panel = await openSettings();
    // Scope to the editor and anchor the regex: a loose /Move .* up/ would also match the
    // board cards' own "Move issue #N up in <lane>" controls, which live in the same DOM.
    expect(within(panel).queryByLabelText(/^Move .+ up$/)).toBeNull();
    expect(within(panel).queryByLabelText(/^Move .+ down$/)).toBeNull();
    // The Remove control stays.
    expect(within(panel).getAllByRole("button", { name: "Remove" }).length).toBe(2);
  });

  it("persists the reordered label list when a row is dragged to the bottom and saved", async () => {
    mockApi.getBoard.mockResolvedValue({
      board: aBoard({
        columns: [
          { label_name: "Planned" },
          { label_name: "In Progress" },
          { label_name: "Human Review" },
        ] as BoardData["columns"],
      }),
    });
    const panel = await openSettings();
    const rows = within(panel).getAllByRole("listitem");
    expect(rows.length).toBe(3);

    // Drag the FIRST row ("Planned") onto the LAST row ("Human Review"). jsdom's rect is
    // all-zeroes and clientY 0 resolves to the "bottom" edge (insertionEdgeFor(0,0,0)),
    // so the source appends after the last row. The drop reads its source from
    // dataTransfer.getData, which carries the column LABEL, not an iid.
    fireEvent.dragStart(rows[0], { dataTransfer: { setData: vi.fn(), getData: () => "Planned" } });
    fireEvent.drop(rows[2], { dataTransfer: { getData: () => "Planned" }, clientY: 0 });

    fireEvent.click(within(panel).getByRole("button", { name: "Save columns" }));
    await waitFor(() => expect(mockApi.configureColumns).toHaveBeenCalled());
    // from=0, i=2, edge="bottom" → to=3 → from<to → to=2 → moveTo(0,2):
    // ["Planned","In Progress","Human Review"] → ["In Progress","Human Review","Planned"].
    expect(mockApi.configureColumns).toHaveBeenCalledWith("repo-1", [
      { label_name: "In Progress" },
      { label_name: "Human Review" },
      { label_name: "Planned" },
    ]);
  });

  it("compensates the index on a mid-list downward drag", async () => {
    mockApi.getBoard.mockResolvedValue({
      board: aBoard({
        columns: [
          { label_name: "Planned" },
          { label_name: "In Progress" },
          { label_name: "Human Review" },
        ] as BoardData["columns"],
      }),
    });
    const panel = await openSettings();
    const rows = within(panel).getAllByRole("listitem");

    // Drag row 0 ("Planned") DOWN onto row 1 ("In Progress"). jsdom's zero rect makes
    // clientY 0 resolve to the "bottom" edge, so it lands just after "In Progress".
    // This is the case that exercises `if (from < to) to -= 1`: from=0, i=1,
    // edge="bottom" → to=2 → from<to → to=1 → moveTo(0,1) → ["In Progress","Planned",
    // "Human Review"]. WITHOUT the decrement, to would stay 2 and moveTo(0,2) would
    // yield ["In Progress","Human Review","Planned"] — a different, wrong order — so
    // this test fails if that line regresses (the drag-to-bottom test above cannot,
    // because splice clamps identically at the array end).
    fireEvent.dragStart(rows[0], { dataTransfer: { setData: vi.fn(), getData: () => "Planned" } });
    fireEvent.drop(rows[1], { dataTransfer: { getData: () => "Planned" }, clientY: 0 });

    fireEvent.click(within(panel).getByRole("button", { name: "Save columns" }));
    await waitFor(() => expect(mockApi.configureColumns).toHaveBeenCalled());
    expect(mockApi.configureColumns).toHaveBeenCalledWith("repo-1", [
      { label_name: "In Progress" },
      { label_name: "Planned" },
      { label_name: "Human Review" },
    ]);
  });
});
