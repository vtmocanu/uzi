// @vitest-environment jsdom
// PRD #3875 M1: the command menu, driven through the AppShell that mounts it.
import { afterEach, beforeEach, describe, it, expect, vi, type Mock } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { AppShell } from "./AppShell";
import { Modal } from "./Modal";
import { useCommandMenu } from "./CommandMenuContext";
import { CommandMenu, decisionCommandItems, filterCommandItems, type CommandItem } from "./CommandMenu";
import { HELD_WORK_HREF } from "./HeldWorkCard";
import { visibleNavItems } from "./navItems";
import { overviewDecisions } from "../lib/overview";
import {
  api,
  type Repo,
  type RecoveryCustodyHold,
  type RecoveryCustodyHolds,
  type RunStatus,
  type RunSummaryItem,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listConnections: vi.fn(),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    getFindingsStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getRecoveryHolds: vi.fn(),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
    getAdminHealth: vi.fn().mockRejectedValue(new Error("no health")),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
// vi.mocked sees the legacy listRuns overload; the menu calls the summary one.
const listRunsMock = mockApi.listRuns as unknown as Mock<(p: { view: "summary" }) => Promise<{ runs: RunSummaryItem[] }>>;

const baseUser = {
  id: "u1",
  email: "member@uzi.local",
  display_name: "Member",
  is_admin: false,
  is_active: true,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function repo(id: string, path: string): Repo {
  return { id, connection_id: "c1", path_with_namespace: path, enabled: true } as unknown as Repo;
}

function runOf(over: Partial<RunSummaryItem> & { status: RunStatus; id: string }): RunSummaryItem {
  return {
    kind: "issue",
    health: "ok",
    created_at: "2026-07-05T09:00:00Z",
    status_since: "2026-07-05T09:00:00Z",
    issue_iid: null,
    issue_title: null,
    title: null,
    ...over,
  } as unknown as RunSummaryItem;
}

function decisionHold(runId: string, createdAt: string, over: Partial<RecoveryCustodyHold> = {}): RecoveryCustodyHold {
  return {
    id: `hold-${runId}`,
    run_id: runId,
    generation: 1,
    state: "open",
    attention: "source_only",
    worker_id: "w",
    inventory_guarded: false,
    has_available_capture: false,
    created_at: createdAt,
    updated_at: createdAt,
    ...over,
  };
}

function heldHolds(
  agg: Partial<RecoveryCustodyHolds["aggregate"]>,
  holds: RecoveryCustodyHold[] = [],
): RecoveryCustodyHolds {
  return {
    aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0, ...agg },
    holds,
  };
}

const UNSAFE = String.fromCharCode(0x202e);
const BEL = String.fromCharCode(7);

function makeStorage(): Storage {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
}

function signIn(isAdmin: boolean) {
  vi.mocked(useAuth).mockReturnValue({
    user: { ...baseUser, is_admin: isAdmin },
    loading: false,
    logout: vi.fn(),
    refresh: vi.fn(),
  } as unknown as ReturnType<typeof useAuth>);
}

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
  signIn(false);
  mockApi.listRepos.mockResolvedValue({ repos: [repo("r-net", "acme/network")] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  // Implementations persist across tests (clearAllMocks only clears calls), so reset the
  // decision sources explicitly: no runs, no holds.
  listRunsMock.mockResolvedValue({ runs: [] });
  mockApi.getRecoveryHolds.mockResolvedValue(heldHolds({}));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  // Drop an instance-level platform stub so the prototype getter shows through again.
  delete (navigator as unknown as Record<string, unknown>).platform;
});

function stubPlatform(platform: string) {
  Object.defineProperty(navigator, "platform", { configurable: true, value: platform });
}

function Probe({ children }: { children?: React.ReactNode }) {
  const loc = useLocation();
  return (
    <div>
      <output data-testid="path">{loc.pathname}</output>
      {children}
    </div>
  );
}

// A stand-in for a page's Search button (the real OverviewNext one has its own test).
function SearchOpener() {
  const menu = useCommandMenu();
  return menu ? <button onClick={menu.open}>Page search</button> : null;
}

function renderShell(children: React.ReactNode = null, path = "/runs") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <Probe>{children}</Probe>
      </AppShell>
    </MemoryRouter>,
  );
}

type KeyInit = { key?: string; code?: string; ctrlKey?: boolean; metaKey?: boolean; shiftKey?: boolean; altKey?: boolean; isComposing?: boolean };

// Dispatches a keydown at `target` and reports whether a handler called preventDefault.
function press(target: EventTarget, init: KeyInit): boolean {
  const ev = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
  act(() => {
    target.dispatchEvent(ev);
  });
  return ev.defaultPrevented;
}

const CTRL_K: KeyInit = { key: "k", code: "KeyK", ctrlKey: true };
const META_K: KeyInit = { key: "k", code: "KeyK", metaKey: true };

const menu = () => screen.queryByRole("dialog", { name: "Command menu" });
const input = () => screen.getByRole("combobox");
const optionLabels = () =>
  screen.queryAllByRole("option").map((o) => o.querySelector("span.truncate")?.textContent ?? "");
const path = () => screen.getByTestId("path").textContent;

async function openByShortcut() {
  await screen.findByRole("link", { name: "Workers" });
  press(document.body, CTRL_K);
  expect(menu()).not.toBeNull();
}

describe("opening by shortcut", () => {
  it("opens on Ctrl K for a non-Apple platform and not on Meta K", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    expect(press(document.body, META_K)).toBe(false);
    expect(menu()).toBeNull();
    expect(press(document.body, CTRL_K)).toBe(true);
    expect(menu()).not.toBeNull();
  });

  it("opens on Meta K for an Apple platform and not on Ctrl K", async () => {
    stubPlatform("MacIntel");
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    expect(press(document.body, CTRL_K)).toBe(false);
    expect(menu()).toBeNull();
    expect(press(document.body, META_K)).toBe(true);
    expect(menu()).not.toBeNull();
  });

  it("ignores Shift or Alt variants and other keys", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    expect(press(document.body, { ...CTRL_K, shiftKey: true })).toBe(false);
    expect(press(document.body, { ...CTRL_K, altKey: true })).toBe(false);
    expect(press(document.body, { key: "j", code: "KeyJ", ctrlKey: true })).toBe(false);
    expect(menu()).toBeNull();
  });

  it("matches the typed letter: another letter at the K position is not hijacked, a non-Latin layout still opens", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    // Colemak: the KeyK position types "e".
    expect(press(document.body, { key: "e", code: "KeyK", ctrlKey: true })).toBe(false);
    expect(menu()).toBeNull();
    // Cyrillic layout: the KeyK position types a letter that is not Latin.
    expect(press(document.body, { key: "\u043b", code: "KeyK", ctrlKey: true })).toBe(true);
    expect(menu()).not.toBeNull();
  });

  it("treats CapsLock by the typed letter: an uppercase E at the K position stays closed, an uppercase K at another position opens", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    expect(press(document.body, { key: "E", code: "KeyK", ctrlKey: true })).toBe(false);
    expect(menu()).toBeNull();
    expect(press(document.body, { key: "K", code: "KeyN", ctrlKey: true })).toBe(true);
    expect(menu()).not.toBeNull();
  });

  it("ignores a composing Ctrl K", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    expect(press(document.body, { ...CTRL_K, isComposing: true } as KeyInit)).toBe(false);
    expect(menu()).toBeNull();
  });

  it("opens from inside a focused text field", async () => {
    renderShell(<input aria-label="field" />);
    const field = await screen.findByLabelText("field");
    field.focus();
    expect(press(field, CTRL_K)).toBe(true);
    expect(menu()).not.toBeNull();
  });

  it("a second shortcut while open is prevented and leaves one dialog", async () => {
    renderShell();
    await openByShortcut();
    expect(press(input(), CTRL_K)).toBe(true);
    expect(screen.getAllByRole("dialog", { name: "Command menu" })).toHaveLength(1);
  });

  it("does not steal plain k or Escape while closed", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Workers" });
    const seen: boolean[] = [];
    const spy = (e: KeyboardEvent) => seen.push(e.defaultPrevented);
    document.addEventListener("keydown", spy);
    try {
      expect(press(document.body, { key: "k", code: "KeyK" })).toBe(false);
      expect(press(document.body, { key: "Escape", code: "Escape" })).toBe(false);
    } finally {
      document.removeEventListener("keydown", spy);
    }
    expect(seen).toEqual([false, false]);
    expect(menu()).toBeNull();
  });

  it("does nothing while another modal dialog is open", async () => {
    renderShell(
      <Modal label="Other dialog" onClose={() => {}}>
        <div>other</div>
      </Modal>,
    );
    await screen.findByRole("dialog", { name: "Other dialog" });
    expect(press(document.activeElement ?? document.body, CTRL_K)).toBe(false);
    expect(menu()).toBeNull();
  });
});

describe("sidebar active state", () => {
  it("lights the board child, not the exact-only Repos parent, on a board route", async () => {
    renderShell(null, "/repos/r-net/board");
    const links = await screen.findAllByRole("link");
    await screen.findByText("acme/network");
    const current = (href: string) =>
      screen
        .getAllByRole("link")
        .filter((a) => a.getAttribute("href") === href)
        .map((a) => a.getAttribute("aria-current"));
    expect(links.length).toBeGreaterThan(0);
    expect(current("/repos/r-net/board")).toEqual(["page"]);
    expect(current("/repos")).toEqual([null]);
  });
});

describe("keyboard and navigation", () => {
  it("typing 'work' ranks Workers first even with a repo named acme/network and a held decision; Enter navigates and closes", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue(
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h1", "2026-07-05T10:00:00Z")]),
    );
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    await screen.findByText("Review held work");
    fireEvent.change(input(), { target: { value: "work" } });
    const labels = optionLabels();
    expect(labels[0]).toBe("Workers");
    expect(labels).toContain("Review held work");
    expect(labels).toContain("Open board: acme/network");
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/workers");
    expect(menu()).toBeNull();
  });

  it("scrolls the first result into view when a new query keeps index 0 active", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    // jsdom has no scrollIntoView; record which option each call scrolls to.
    const scrolled: string[] = [];
    const proto = Element.prototype as { scrollIntoView?: unknown };
    const original = proto.scrollIntoView;
    proto.scrollIntoView = function (this: Element) {
      scrolled.push(this.textContent ?? "");
    };
    try {
      fireEvent.change(input(), { target: { value: "board" } });
      expect(optionLabels()[0]).toBe("Open board: acme/network");
      expect(scrolled.some((t) => t.includes("Open board: acme/network"))).toBe(true);
    } finally {
      if (original === undefined) delete proto.scrollIntoView;
      else proto.scrollIntoView = original;
    }
  });

  it("hides group headings while a query is set and shows 'No matches' for none", async () => {
    renderShell();
    await openByShortcut();
    expect(screen.getByText("Go to")).toBeTruthy();
    expect(screen.getByText("Actions")).toBeTruthy();
    fireEvent.change(input(), { target: { value: "work" } });
    expect(screen.queryByText("Go to")).toBeNull();
    fireEvent.change(input(), { target: { value: "zzzzqq" } });
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toBe("No matches");
    expect(screen.getByRole("listbox").textContent).not.toContain("No matches");
    expect(input().getAttribute("aria-activedescendant")).toBeNull();
    expect(input().getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByRole("listbox").className).not.toContain("p-2");
    fireEvent.change(input(), { target: { value: "work" } });
    expect(input().getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("listbox").className).toContain("p-2");
  });

  it("groups the browsing list under labelled groups and announces the result count while searching", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toBe("");
    const groups = screen.getAllByRole("group");
    // Readable names, not the CSS-uppercased headings.
    expect(groups.map((g) => g.getAttribute("aria-label"))).toEqual(["Actions", "Go to"]);
    expect(screen.getByRole("group", { name: "Go to" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Actions" })).toBeTruthy();
    fireEvent.change(input(), { target: { value: "work" } });
    expect(screen.queryAllByRole("group")).toHaveLength(0);
    const count = screen.getAllByRole("option").length;
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toBe(`${count} results`);
    expect(within(menu() as HTMLElement).getByRole("status").closest('[role="listbox"]')).toBeNull();
  });

  it("keeps the highlight on the same command when repos arrive above it", async () => {
    let resolveRepos: (v: { repos: Repo[] }) => void = () => {};
    mockApi.listRepos.mockReturnValue(new Promise((r) => (resolveRepos = r)));
    renderShell();
    await openByShortcut();
    // Only Start a chat, then Overview and Repos under Go to, so far.
    expect(optionLabels().slice(0, 3)).toEqual(["Start a chat", "Overview", "Repos"]);
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    const before = screen.getAllByRole("option").find((o) => o.getAttribute("aria-selected") === "true");
    const label = before?.querySelector("span.truncate")?.textContent;
    expect(label).toBe("Repos");
    await act(async () => {
      resolveRepos({ repos: [repo("r-net", "acme/network")] });
    });
    await screen.findByText("Open board: acme/network");
    const after = screen.getAllByRole("option").find((o) => o.getAttribute("aria-selected") === "true");
    expect(after?.querySelector("span.truncate")?.textContent).toBe(label);
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/repos");
  });

  it("ArrowDown then Enter runs the second item; aria-activedescendant tracks the active option", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
    expect(input().getAttribute("aria-activedescendant")).toBe(options[0].id);
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    const after = screen.getAllByRole("option");
    expect(after[1].getAttribute("aria-selected")).toBe("true");
    expect(input().getAttribute("aria-activedescendant")).toBe(after[1].id);
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/repos/r-net/board");
    expect(menu()).toBeNull();
  });

  it("ArrowUp from the first item wraps to the last", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    fireEvent.keyDown(input(), { key: "ArrowUp" });
    const options = screen.getAllByRole("option");
    expect(options[options.length - 1].getAttribute("aria-selected")).toBe("true");
  });

  it("clicking an option navigates and closes", async () => {
    renderShell();
    await openByShortcut();
    fireEvent.click(screen.getByRole("option", { name: /^Start a chat/ }));
    expect(path()).toBe("/chat");
    expect(menu()).toBeNull();
  });

  it("the dialog exposes the combobox and listbox wiring", async () => {
    renderShell();
    await openByShortcut();
    const box = input();
    expect(box.getAttribute("aria-expanded")).toBe("true");
    expect(box.getAttribute("aria-autocomplete")).toBe("list");
    expect(screen.getByRole("listbox").id).toBe(box.getAttribute("aria-controls"));
    expect(screen.getByLabelText("Command")).toBe(box);
    expect(document.activeElement).toBe(box);
  });
});

describe("focus return", () => {
  it("Escape closes and returns focus to a focused element that opened it by shortcut", async () => {
    renderShell(<button>probe</button>);
    const probe = await screen.findByRole("button", { name: "probe" });
    probe.focus();
    press(probe, CTRL_K);
    expect(document.activeElement).toBe(input());
    fireEvent.keyDown(input(), { key: "Escape" });
    expect(menu()).toBeNull();
    expect(document.activeElement).toBe(probe);
  });

  it("returns focus to a page button that opened it through the shell context", async () => {
    renderShell(<SearchOpener />);
    const opener = await screen.findByRole("button", { name: "Page search" });
    opener.focus();
    fireEvent.click(opener);
    expect(menu()).not.toBeNull();
    fireEvent.keyDown(input(), { key: "Escape" });
    expect(menu()).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("from the mobile drawer: the drawer unmounts and Escape lands on 'Open navigation'", async () => {
    renderShell();
    const openNav = await screen.findByRole("button", { name: "Open navigation" });
    fireEvent.click(openNav);
    const drawer = screen.getByRole("button", { name: "Close navigation" }).parentElement as HTMLElement;
    const link = Array.from(drawer.querySelectorAll("a")).find((a) => a.getAttribute("href") === "/runs") as HTMLElement;
    link.focus();
    expect(document.activeElement).toBe(link);
    press(link, CTRL_K);
    expect(menu()).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Close navigation" })).toBeNull();
    fireEvent.keyDown(input(), { key: "Escape" });
    expect(menu()).toBeNull();
    expect(document.activeElement).toBe(openNav);
  });
});

describe("Go to gating", () => {
  it("a non-admin never sees Admin", async () => {
    renderShell();
    await openByShortcut();
    expect(optionLabels()).toContain("Workers");
    expect(optionLabels()).not.toContain("Admin");
    fireEvent.change(input(), { target: { value: "admin" } });
    expect(optionLabels()).not.toContain("Admin");
  });

  it("an admin sees Admin with the Admin hint", async () => {
    signIn(true);
    renderShell();
    await openByShortcut();
    fireEvent.change(input(), { target: { value: "admin" } });
    const admin = screen.getAllByRole("option").find((o) => o.querySelector("span.truncate")?.textContent === "Admin");
    expect(admin?.textContent).toContain("Admin");
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/admin");
  });
});

describe("repo data in Actions", () => {
  it("strips bidi and control characters from a repo path", async () => {
    const dirty = `acme/${String.fromCharCode(0x202e)}evil${String.fromCharCode(7)}repo`;
    mockApi.listRepos.mockResolvedValue({ repos: [repo("r-1", dirty)] });
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/evilrepo");
    expect(screen.getByRole("listbox").textContent).not.toContain(String.fromCharCode(0x202e));
  });

  it("masks the path in demo mode, then strips it", async () => {
    window.localStorage.setItem("uzi_demo_mode", "1");
    mockApi.listRepos.mockResolvedValue({ repos: [repo("r-1", `secretco/${String.fromCharCode(0x202e)}billing`)] });
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: demo/billing");
    expect(screen.getByRole("listbox").textContent).not.toContain("secretco");
  });

  it("targets the loaded repo id, whatever the path says", async () => {
    mockApi.listRepos.mockResolvedValue({ repos: [repo("r/9 x", "../admin")] });
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: ../admin");
    fireEvent.change(input(), { target: { value: "board" } });
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/repos/r%2F9%20x/board");
  });

  it("fetches repos once per open", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    const before = mockApi.listRepos.mock.calls.length;
    fireEvent.change(input(), { target: { value: "w" } });
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    expect(mockApi.listRepos.mock.calls.length).toBe(before);
  });

  it("shows 'Boards unavailable' and keeps Start a chat when the repos call fails", async () => {
    mockApi.listRepos.mockRejectedValue(new Error("down"));
    renderShell();
    await openByShortcut();
    expect(await screen.findByText("Boards unavailable")).toBeTruthy();
    expect(optionLabels()).toContain("Start a chat");
    expect(optionLabels().some((l) => l.startsWith("Open board"))).toBe(false);
    expect(optionLabels()).toContain("Workers");
  });

  it("keeps 'Boards unavailable' outside the listbox and visible while a query is set", async () => {
    mockApi.listRepos.mockRejectedValue(new Error("down"));
    renderShell();
    await openByShortcut();
    const note = await screen.findByText("Boards unavailable");
    expect(note.closest('[role="listbox"]')).toBeNull();
    fireEvent.change(input(), { target: { value: "work" } });
    expect(screen.getByText("Boards unavailable")).toBeTruthy();
  });
});

describe("filterCommandItems", () => {
  const item = (id: string, group: CommandItem["group"], label: string): CommandItem => ({
    id,
    group,
    label,
    hint: "",
    to: "/",
    icon: null,
  });

  it("ranks a label prefix above a word start above any other substring", () => {
    const items = [
      item("a", "needs", "Review held work"),
      item("b", "actions", "Open board: acme/network"),
      item("c", "goto", "Workers"),
    ];
    expect(filterCommandItems(items, "work").map((i) => i.id)).toEqual(["c", "a", "b"]);
  });

  it("matches case-insensitively and ignores surrounding whitespace", () => {
    const items = [item("c", "goto", "Workers")];
    expect(filterCommandItems(items, "  WORK ").map((i) => i.id)).toEqual(["c"]);
    expect(filterCommandItems(items, "zz")).toEqual([]);
  });

  it("breaks ties Go to, Actions, Needs you with a query, and reads Needs you first without one", () => {
    const items = [item("n", "needs", "Runs a"), item("a", "actions", "Runs b"), item("g", "goto", "Runs c")];
    expect(filterCommandItems(items, "runs").map((i) => i.id)).toEqual(["g", "a", "n"]);
    expect(filterCommandItems(items, "").map((i) => i.id)).toEqual(["n", "a", "g"]);
  });
});

describe("Admin hint", () => {
  it("lists Admin with the Admin hint for an admin and omits it for others", () => {
    expect(visibleNavItems(true).find((n) => n.key === "admin")?.hint).toBe("Admin");
    expect(visibleNavItems(false).some((n) => n.key === "admin")).toBe(false);
  });
});

describe("Needs you group", () => {
  const T = (h: number) => `2026-07-05T${String(h).padStart(2, "0")}:00:00Z`;
  const optionByLabel = (label: string) =>
    screen.getAllByRole("option").find((o) => o.querySelector("span.truncate")?.textContent === label) as HTMLElement;
  const hintOf = (label: string) => optionByLabel(label).querySelector("span.ml-auto")?.textContent;

  const fixtureRuns = () => [
    runOf({ id: "run-plan", status: "awaiting_approval", issue_iid: 418, status_since: T(9) }),
    runOf({ id: "run-ask", status: "awaiting_input", issue_iid: 409, status_since: T(10) }),
    // An owner-disposition park: a plain pause is moved only by the owner.
    runOf({ id: "run-park", status: "paused", issue_iid: 305, status_since: T(10) }),
    runOf({
      id: "run-pr",
      status: "completed",
      forge_type: "gitlab",
      issue_iid: 543,
      mr_iid: 1043,
      mr_state: "opened",
      repo_id: "r-net",
      finished_at: T(11),
    }),
  ];

  it("lists a plan, a question, a park and a PR with the mock's labels and hints, and a plan choice opens the run page", async () => {
    listRunsMock.mockResolvedValue({ runs: fixtureRuns() });
    renderShell();
    await openByShortcut();
    await screen.findByText("Review plan for #418");
    expect(screen.getByRole("group", { name: "Needs you" })).toBeTruthy();
    expect(optionLabels().slice(0, 4)).toEqual([
      "Review plan for #418",
      "Answer #409",
      "Resume #305",
      "Review PR !1043",
    ]);
    expect(hintOf("Review plan for #418")).toBe("Plan review");
    expect(hintOf("Answer #409")).toBe("Question");
    expect(hintOf("Resume #305")).toBe("Parked");
    expect(hintOf("Review PR !1043")).toBe("Pull request");
    expect(mockApi.listRuns).toHaveBeenCalledWith({ view: "summary" });
    fireEvent.click(optionByLabel("Review plan for #418"));
    expect(path()).toBe("/runs/run-plan");
    expect(menu()).toBeNull();
  });

  it("sends an awaiting_input run and an open PR to their run pages", async () => {
    listRunsMock.mockResolvedValue({ runs: fixtureRuns() });
    renderShell();
    await openByShortcut();
    await screen.findByText("Answer #409");
    fireEvent.click(optionByLabel("Answer #409"));
    expect(path()).toBe("/runs/run-ask");
    press(document.body, CTRL_K);
    await screen.findByText("Review PR !1043");
    fireEvent.click(optionByLabel("Review PR !1043"));
    expect(path()).toBe("/runs/run-pr");
  });

  it("labels a PR by the forge's MR reference, or by its issue when there is no MR number", () => {
    const pr = (over: Partial<RunSummaryItem>) =>
      runOf({ id: "p", status: "completed", mr_state: "opened", repo_id: "r", issue_iid: 543, ...over });
    const labels = (run: RunSummaryItem) => decisionCommandItems(overviewDecisions([run])).map((i) => i.label);
    expect(labels(pr({ forge_type: "gitlab", mr_iid: 1043 }))).toEqual(["Review PR !1043"]);
    expect(labels(pr({ forge_type: "github", mr_iid: 1043 }))).toEqual(["Review PR #1043"]);
    // overviewDecisions only yields PRs that have an MR, so build the no-MR case directly.
    expect(
      decisionCommandItems([
        { kind: "pr", verb: "Review PR", since: T(1), run: pr({ forge_type: "gitlab", mr_iid: null }) },
      ]).map((i) => i.label),
    ).toEqual(["Review PR for #543"]);
  });

  it("keeps an explicit highlight on its Go to item when decisions arrive above it, and Enter goes there", async () => {
    let resolveRuns: (v: { runs: RunSummaryItem[] }) => void = () => {};
    listRunsMock.mockReturnValue(new Promise((r) => (resolveRuns = r)));
    mockApi.listRepos.mockReturnValue(new Promise(() => {}));
    renderShell();
    await openByShortcut();
    expect(optionLabels().slice(0, 3)).toEqual(["Start a chat", "Overview", "Repos"]);
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    await act(async () => {
      resolveRuns({ runs: [fixtureRuns()[0]] });
    });
    await screen.findByText("Review plan for #418");
    const selected = screen.getAllByRole("option").filter((o) => o.getAttribute("aria-selected") === "true");
    expect(selected.map((o) => o.querySelector("span.truncate")?.textContent)).toEqual(["Repos"]);
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/repos");
  });

  it("with no arrow pressed, Enter goes to the first Needs you item once decisions arrive", async () => {
    let resolveRuns: (v: { runs: RunSummaryItem[] }) => void = () => {};
    listRunsMock.mockReturnValue(new Promise((r) => (resolveRuns = r)));
    renderShell();
    await openByShortcut();
    await act(async () => {
      resolveRuns({ runs: [fixtureRuns()[0]] });
    });
    await screen.findByText("Review plan for #418");
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/runs/run-plan");
  });

  it("lists held work with a Custody hint, adding the count above one", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue(
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h1", T(8))]),
    );
    const { unmount } = renderShell();
    await openByShortcut();
    await screen.findByText("Review held work");
    expect(hintOf("Review held work")).toBe("Custody");
    fireEvent.click(optionByLabel("Review held work"));
    expect(path()).toBe("/workers");
    unmount();

    mockApi.getRecoveryHolds.mockResolvedValue(
      heldHolds({ open_holds: 2, decision_needed: 2 }, [decisionHold("h1", T(8)), decisionHold("h2", T(9))]),
    );
    renderShell();
    await openByShortcut();
    await screen.findByText("Review held work");
    expect(hintOf("Review held work")).toBe("Custody · 2");
  });

  it("targets HELD_WORK_HREF for the held item", () => {
    const items = decisionCommandItems(
      overviewDecisions([], heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h1", T(8))])),
    );
    expect(items.map((i) => [i.id, i.to])).toEqual([["needs:held", HELD_WORK_HREF]]);
  });

  it("omits the group when there are no decisions", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    expect(screen.queryByRole("group", { name: "Needs you" })).toBeNull();
    expect(screen.queryByText("Decisions unavailable")).toBeNull();
  });

  it.each([
    ["runs", () => listRunsMock.mockRejectedValue(new Error("down"))],
    ["holds", () => mockApi.getRecoveryHolds.mockRejectedValue(new Error("down"))],
  ])("shows one 'Decisions unavailable' line and no partial list when %s fails", async (_name, fail) => {
    listRunsMock.mockResolvedValue({ runs: fixtureRuns() });
    mockApi.getRecoveryHolds.mockResolvedValue(
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h1", T(8))]),
    );
    fail();
    renderShell();
    await openByShortcut();
    const note = await screen.findByText("Decisions unavailable");
    expect(note.closest('[role="listbox"]')).toBeNull();
    expect(screen.queryByRole("group", { name: "Needs you" })).toBeNull();
    expect(optionLabels().some((l) => l.startsWith("Review") || l.startsWith("Answer"))).toBe(false);
    expect(optionLabels()).toContain("Start a chat");
    expect(optionLabels()).toContain("Workers");
    fireEvent.change(input(), { target: { value: "work" } });
    expect(screen.getByText("Decisions unavailable")).toBeTruthy();
    expect(optionLabels()[0]).toBe("Workers");
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toContain("Decisions unavailable");
  });

  it("announces 'Boards unavailable' too while searching", async () => {
    mockApi.listRepos.mockRejectedValue(new Error("down"));
    renderShell();
    await openByShortcut();
    await screen.findByText("Boards unavailable");
    fireEvent.change(input(), { target: { value: "work" } });
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toContain("Boards unavailable");
  });

  it("fetches runs and holds once per open; a late response after unmount is ignored and a reopen shows only its own decisions", async () => {
    let resolveFirst: (v: { runs: RunSummaryItem[] }) => void = () => {};
    listRunsMock.mockReturnValueOnce(new Promise((r) => (resolveFirst = r)));
    const mount = () =>
      render(
        <MemoryRouter>
          <CommandMenu onClose={() => {}} isAdmin={false} />
        </MemoryRouter>,
      );
    const first = mount();
    expect(mockApi.listRuns).toHaveBeenCalledTimes(1);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(1);
    first.unmount();
    listRunsMock.mockResolvedValueOnce({
      runs: [runOf({ id: "run-fresh", status: "awaiting_approval", issue_iid: 2, status_since: T(9) })],
    });
    mount();
    expect(mockApi.listRuns).toHaveBeenCalledTimes(2);
    await screen.findByText("Review plan for #2");
    await act(async () => {
      resolveFirst({
        runs: [runOf({ id: "run-stale", status: "awaiting_approval", issue_iid: 1, status_since: T(9) })],
      });
    });
    expect(screen.queryByText("Review plan for #1")).toBeNull();
    expect(screen.getByText("Review plan for #2")).toBeTruthy();
  });

  it("shows no Needs you rows while the decisions are still loading", async () => {
    listRunsMock.mockReturnValue(new Promise(() => {}));
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    expect(screen.queryByRole("group", { name: "Needs you" })).toBeNull();
    expect(screen.queryByText("Decisions unavailable")).toBeNull();
  });

  it("matches overviewDecisions on the same fixture: same order, ids and targets", async () => {
    const runs = fixtureRuns();
    const holds = heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h1", T(8))]);
    listRunsMock.mockResolvedValue({ runs });
    mockApi.getRecoveryHolds.mockResolvedValue(holds);
    const decisions = overviewDecisions(runs, holds);
    const expected = decisions.map((d) =>
      d.kind === "held"
        ? ["needs:held", HELD_WORK_HREF]
        : [`needs:${d.kind}:${d.run.id}`, `/runs/${encodeURIComponent(d.run.id)}`],
    );
    expect(decisionCommandItems(decisions).map((i) => [i.id, i.to])).toEqual(expected);
    expect(new Set(expected.map((e) => e[0])).size).toBe(expected.length);
    renderShell();
    await openByShortcut();
    await screen.findByText("Review held work");
    expect(within(screen.getByRole("group", { name: "Needs you" })).getAllByRole("option")).toHaveLength(
      decisions.length,
    );
    expect(optionLabels().slice(0, 5)).toEqual([
      "Review held work",
      "Review plan for #418",
      "Answer #409",
      "Resume #305",
      "Review PR !1043",
    ]);
  });

  describe("hostile text", () => {
    const hostileRun = () =>
      runOf({ id: "run-x", status: "awaiting_approval", title: `Fix${UNSAFE} the${BEL} thing`, issue_iid: null });

    it("strips an issue-less run's title in normal mode", async () => {
      listRunsMock.mockResolvedValue({ runs: [hostileRun()] });
      renderShell();
      await openByShortcut();
      await screen.findByText("Review plan: Fix the thing");
      expect(screen.getByRole("listbox").textContent).not.toContain(UNSAFE);
      expect(screen.getByRole("listbox").textContent).not.toContain(BEL);
    });

    it("strips it in demo mode too", async () => {
      window.localStorage.setItem("uzi_demo_mode", "1");
      listRunsMock.mockResolvedValue({ runs: [hostileRun()] });
      renderShell();
      await openByShortcut();
      await screen.findByText("Review plan: Fix the thing");
      expect(screen.getByRole("listbox").textContent).not.toContain(UNSAFE);
    });

    it("falls back to the bare verb when the issue-less run has no usable title", async () => {
      listRunsMock.mockResolvedValue({
        runs: [runOf({ id: "run-n", status: "awaiting_input", title: UNSAFE + BEL, issue_title: undefined })],
      });
      renderShell();
      await openByShortcut();
      await screen.findByText("Answer");
    });

    it("strips a hold's state label from the held option's title", async () => {
      // An unknown attention renders its (sanitized) value as the state label.
      mockApi.getRecoveryHolds.mockResolvedValue(
        heldHolds({ open_holds: 1, decision_needed: 1 }, [
          decisionHold("h1", T(8), { attention: `Odd${UNSAFE}state${BEL}` as RecoveryCustodyHold["attention"] }),
        ]),
      );
      renderShell();
      await openByShortcut();
      await screen.findByText("Review held work");
      expect(optionByLabel("Review held work").getAttribute("title")).toBe("Oddstate");
    });

    it("re-strips a raw unsafe title and label passed through extraItems", () => {
      const raw: CommandItem = {
        id: "x:1",
        group: "needs",
        label: `Raw${UNSAFE} label${BEL}`,
        hint: "",
        to: "/runs/x",
        icon: null,
        title: `Raw${UNSAFE} title${BEL}`,
      };
      render(
        <MemoryRouter>
          <CommandMenu onClose={() => {}} isAdmin={false} extraItems={[raw]} />
        </MemoryRouter>,
      );
      const opt = screen.getByRole("option", { name: /Raw/ });
      expect(opt.getAttribute("title")).toBe("Raw title");
      expect(opt.textContent).toBe("Raw label");
    });

    it("builds the target from the loaded run id only: a path-like id is encoded, the label never affects it", async () => {
      listRunsMock.mockResolvedValue({
        runs: [runOf({ id: "../admin", status: "awaiting_approval", issue_iid: 5, title: "/admin" })],
      });
      renderShell();
      await openByShortcut();
      await screen.findByText("Review plan for #5");
      fireEvent.click(optionByLabel("Review plan for #5"));
      expect(path()).toBe("/runs/..%2Fadmin");
    });
  });
});
