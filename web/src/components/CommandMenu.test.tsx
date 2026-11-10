// @vitest-environment jsdom
// PRD #3875 M1: the command menu, driven through the AppShell that mounts it.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { AppShell } from "./AppShell";
import { Modal } from "./Modal";
import { useCommandMenu } from "./CommandMenuContext";
import { filterCommandItems, type CommandItem } from "./CommandMenu";
import { api, type Repo } from "../lib/api";
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
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
    getAdminHealth: vi.fn().mockRejectedValue(new Error("no health")),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

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
  it("typing 'work' ranks Workers first even with a repo named acme/network; Enter navigates and closes", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    fireEvent.change(input(), { target: { value: "work" } });
    const labels = optionLabels();
    expect(labels[0]).toBe("Workers");
    expect(labels).toContain("Open board: acme/network");
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(path()).toBe("/workers");
    expect(menu()).toBeNull();
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
  });

  it("groups the browsing list under labelled groups and announces the result count while searching", async () => {
    renderShell();
    await openByShortcut();
    await screen.findByText("Open board: acme/network");
    expect(within(menu() as HTMLElement).getByRole("status").textContent).toBe("");
    const groups = screen.getAllByRole("group");
    expect(groups.map((g) => g.getAttribute("aria-labelledby")).every((id) => !!id)).toBe(true);
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
