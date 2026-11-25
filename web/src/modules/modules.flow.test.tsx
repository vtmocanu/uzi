// @vitest-environment jsdom
//
// Runtime web modules, end to end in jsdom (PRD #4686 M1): the REAL App (shell, sidebar,
// command menu, route guards, AuthProvider, ModulesProvider) in a memory DATA router under
// StrictMode, as main.tsx mounts it, against the stateful mockApi. Only `fetch` (for
// /modules.json) and the federation runtime wiring (./federation) are stubbed; the loader,
// registry, guards and error handling are real.
import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, MOCK_MODE: true, api: (await import("../mocks/mockApi")).mockApi };
});

const fed = vi.hoisted(() => ({
  loadModuleRemote: vi.fn(),
  reRegisterModule: vi.fn(),
  registerModuleRemotes: vi.fn(),
  setHostShared: vi.fn(),
}));
vi.mock("./federation", () => fed);

import { appRoutes } from "../appRoutes";
import { mockApi } from "../mocks/mockApi";
import { resetModuleLoaderForTests } from "./loader";
import { resetModuleListForTests } from "./registry";

const base = { version: "1.0.0", page: "Page", icon: "package" };
const LIST = [
  { ...base, id: "wmod", label: "Work Mod", navGroup: "work" },
  { ...base, id: "fmod", label: "Factory Mod", navGroup: "factory" },
  { ...base, id: "mmod", label: "Own Mod", navGroup: "modules" },
  { ...base, id: "admmod", label: "Staff Mod", navGroup: "modules", adminOnly: true },
];

function Page() {
  return <p>module page rendered</p>;
}

let fetchMock: ReturnType<typeof vi.fn>;
function serveList(body: unknown = LIST) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } })),
  );
}

function mount(path: string) {
  const router = createMemoryRouter(appRoutes, { initialEntries: [path] });
  const view = render(
    <StrictMode>
      <RouterProvider router={router} />
    </StrictMode>,
  );
  return { router, ...view };
}

const navLinkLabels = () =>
  within(document.querySelector("nav")!)
    .getAllByRole("link")
    .map((a) => a.textContent ?? "");
const indexOfLabel = (labels: string[], label: string) => labels.findIndex((l) => l.startsWith(label));

beforeEach(async () => {
  localStorage.clear();
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  fed.loadModuleRemote.mockReset().mockResolvedValue({ default: Page, hostContract: 1 });
  fed.reRegisterModule.mockReset();
  fed.registerModuleRemotes.mockReset();
  await mockApi.login("vlad@uzi.local", "x");
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetModuleListForTests();
  resetModuleLoaderForTests();
  vi.restoreAllMocks();
});

describe("the module list fetch", () => {
  it("is one no-cache request per document across a remount and two navigations", async () => {
    serveList();
    const first = mount("/dashboard");
    await screen.findAllByRole("link", { name: "Work Mod" }, { timeout: 5000 });
    await act(() => first.router.navigate("/m/wmod"));
    await screen.findByText("module page rendered");
    await act(() => first.router.navigate("/runs"));
    first.unmount();

    // A remount in the same document reuses the memoized list.
    const second = mount("/dashboard");
    await screen.findAllByRole("link", { name: "Work Mod" }, { timeout: 5000 });
    await act(() => second.router.navigate("/m/fmod"));
    await screen.findByText("module page rendered");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith("/modules.json", { cache: "no-cache" });
    // The parsed list was registered once, however many times the provider mounted.
    expect(fed.registerModuleRemotes).toHaveBeenCalledTimes(1);
    expect(fed.registerModuleRemotes.mock.calls[0][0].map((m: { id: string }) => m.id)).toEqual([
      "wmod", "fmod", "mmod", "admmod",
    ]);
  }, 30000);
});

describe("route guards run before any load", () => {
  it("signed out: /login, and loadRemote is never called", async () => {
    serveList();
    await mockApi.logout();
    const { router } = mount("/m/wmod");
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"), { timeout: 5000 });
    expect(fed.loadModuleRemote).not.toHaveBeenCalled();
  }, 30000);

  it("a non-admin on an adminOnly module: /dashboard, and loadRemote is never called", async () => {
    serveList();
    await mockApi.login("mira@uzi.local", "x");
    const { router } = mount("/m/admmod");
    await waitFor(() => expect(router.state.location.pathname).toBe("/dashboard"), { timeout: 5000 });
    expect(fed.loadModuleRemote).not.toHaveBeenCalled();
  }, 30000);

  it("an admin on an adminOnly module loads it exactly once", async () => {
    serveList();
    mount("/m/admmod");
    await screen.findByText("module page rendered", {}, { timeout: 5000 });
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);
    expect(fed.loadModuleRemote).toHaveBeenCalledWith("admmod/Page");
  }, 30000);

  it("a non-admin loads a public module", async () => {
    serveList();
    await mockApi.login("mira@uzi.local", "x");
    mount("/m/wmod");
    await screen.findByText("module page rendered", {}, { timeout: 5000 });
    expect(fed.loadModuleRemote).toHaveBeenCalledTimes(1);
  }, 30000);

  it("no loadRemote happens on ordinary pages", async () => {
    serveList();
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Work Mod" }, { timeout: 5000 });
    expect(fed.loadModuleRemote).not.toHaveBeenCalled();
  }, 30000);
});

describe("an unknown module", () => {
  it("renders the full sentence inside the shell", async () => {
    serveList();
    mount("/m/nope");
    const sentence = await screen.findByText(/No module named/, {}, { timeout: 5000 });
    expect(sentence.textContent).toBe("No module named nope is configured.");
    // Inside the authenticated shell: the sidebar is still there.
    expect(screen.getAllByRole("link", { name: "Settings" }).length).toBeGreaterThan(0);
    expect(fed.loadModuleRemote).not.toHaveBeenCalled();
  }, 30000);

  it("shows the sentence for any id when the list is empty", async () => {
    serveList([]);
    mount("/m/anything/deeper");
    expect((await screen.findByText(/No module named/, {}, { timeout: 5000 })).textContent).toBe(
      "No module named anything is configured.",
    );
  }, 30000);
});

describe("navigation entries", () => {
  it("appear in the sidebar after the list resolves, placed by group", async () => {
    let release!: () => void;
    fetchMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = () => resolve(new Response(JSON.stringify(LIST), { status: 200 }));
        }),
    );
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Settings" }, { timeout: 5000 });
    expect(screen.queryByRole("link", { name: "Work Mod" })).toBeNull();
    await act(async () => release());
    await screen.findAllByRole("link", { name: "Work Mod" });

    const labels = navLinkLabels();
    const at = (l: string) => indexOfLabel(labels, l);
    // Work entries are appended after Chat and before the first Factory entry.
    expect(at("Chat")).toBeLessThan(at("Work Mod"));
    expect(at("Work Mod")).toBeLessThan(at("Agents"));
    // Factory entries follow Judge; the Modules section follows Factory and precedes Settings.
    expect(at("Judge")).toBeLessThan(at("Factory Mod"));
    expect(at("Factory Mod")).toBeLessThan(at("Own Mod"));
    expect(at("Own Mod")).toBeLessThan(at("Settings"));
    // The admin sees the admin-only module too.
    expect(at("Staff Mod")).toBeGreaterThan(at("Own Mod"));
    expect(at("Staff Mod")).toBeLessThan(at("Settings"));
    expect(document.body.textContent).toContain("Modules");
  }, 30000);

  it("hide admin-only entries from non-admins, and an all-hidden Modules section drops its heading", async () => {
    serveList([{ ...base, id: "admmod", label: "Staff Mod", navGroup: "modules", adminOnly: true }]);
    await mockApi.login("mira@uzi.local", "x");
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Settings" }, { timeout: 5000 });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.queryByRole("link", { name: "Staff Mod" })).toBeNull();
    const headings = Array.from(document.querySelectorAll("nav p")).map((p) => p.textContent);
    expect(headings).not.toContain("Modules");
  }, 30000);

  it("do not remount the bottom cluster when the list resolves", async () => {
    let release!: () => void;
    fetchMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = () => resolve(new Response(JSON.stringify(LIST), { status: 200 }));
        }),
    );
    mount("/dashboard");
    const before = (await screen.findAllByRole("link", { name: "Settings" }, { timeout: 5000 }))[0];
    const beforeSection = before.parentElement;
    const beforeOverview = screen.getAllByRole("link", { name: "Overview" })[0];
    await act(async () => release());
    await screen.findAllByRole("link", { name: "Own Mod" });
    const after = screen.getAllByRole("link", { name: "Settings" })[0];
    expect(after).toBe(before);
    expect(after.parentElement).toBe(beforeSection);
    expect(screen.getAllByRole("link", { name: "Overview" })[0]).toBe(beforeOverview);
  }, 30000);

  it("appear in the command menu when it is opened after the list resolved", async () => {
    serveList();
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Work Mod" }, { timeout: 5000 });
    act(() => {
      document.body.dispatchEvent(
        new KeyboardEvent("keydown", { key: "k", code: "KeyK", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    const dialog = await screen.findByRole("dialog", { name: "Command menu" });
    const options = within(dialog).getAllByRole("option");
    const text = (label: string) => options.find((o) => o.textContent?.includes(label));
    expect(text("Work Mod")?.textContent).toContain("Work");
    expect(text("Factory Mod")?.textContent).toContain("Factory");
    expect(text("Own Mod")?.textContent).toContain("Modules");
    expect(text("Staff Mod")).toBeTruthy();
    // and picking one navigates to it
    fireEvent.click(text("Own Mod")!);
    await screen.findByText("module page rendered");
  }, 30000);

  it("a mixed manifest shows exactly the valid entries in the sidebar and the command menu, one console error per bad entry", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const e = (id: string, over: Record<string, unknown> = {}) => ({
      ...base,
      id,
      label: `L ${id}`,
      navGroup: "work",
      ...over,
    });
    const nopage = e("nopage");
    delete (nopage as Record<string, unknown>).page;
    serveList([
      e("Bad_ID"), // 0 invalid id
      nopage, // 1 missing field
      e("longlabel", { label: "x".repeat(41) }), // 2 bad label
      e("badicon", { icon: "no-such-icon" }), // 3 unknown icon
      e("good-work", { label: "Good Work" }), // 4 valid work
      e("good-work", { label: "Dup Work" }), // 5 later duplicate
      e("nonbool", { adminOnly: "yes" }), // 6 non-boolean adminOnly
      e("oddgroup", { label: "Odd Group", navGroup: "sideways" }), // 7 unknown group -> modules
      e("good-fac", { label: "Good Factory", navGroup: "factory" }), // 8 valid factory
    ]);
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Good Work" }, { timeout: 5000 });

    const hrefs = Array.from(document.querySelector("nav")!.querySelectorAll('a[href^="/m/"]')).map((a) => a.getAttribute("href"));
    expect(hrefs).toEqual(["/m/good-work", "/m/good-fac", "/m/oddgroup"]);
    const labels = navLinkLabels();
    const at = (l: string) => indexOfLabel(labels, l);
    expect(at("Chat")).toBeLessThan(at("Good Work"));
    expect(at("Good Work")).toBeLessThan(at("Agents"));
    expect(at("Judge")).toBeLessThan(at("Good Factory"));
    expect(at("Good Factory")).toBeLessThan(at("Odd Group"));
    expect(at("Odd Group")).toBeLessThan(at("Settings"));
    expect(Array.from(document.querySelectorAll("nav p")).map((p) => p.textContent)).toContain("Modules");
    for (const gone of ["L Bad_ID", "L nopage", "L longlabel", "L badicon", "Dup Work", "L nonbool"]) {
      expect(document.body.textContent).not.toContain(gone);
    }

    act(() => {
      document.body.dispatchEvent(
        new KeyboardEvent("keydown", { key: "k", code: "KeyK", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    const dialog = await screen.findByRole("dialog", { name: "Command menu" });
    const options = within(dialog).getAllByRole("option").map((o) => o.textContent ?? "");
    const group = (label: string) => options.find((o) => o.includes(label)) ?? "";
    expect(group("Good Work")).toContain("Work");
    expect(group("Good Factory")).toContain("Factory");
    expect(group("Odd Group")).toContain("Modules");
    for (const gone of ["L Bad_ID", "L nopage", "L longlabel", "L badicon", "Dup Work", "L nonbool"]) {
      expect(options.some((o) => o.includes(gone))).toBe(false);
    }

    // Seven problems, one line each, each naming its entry (once per document).
    const lines = errors.mock.calls.map((c) => String(c[0])).filter((l) => l.startsWith("uzi modules: "));
    expect(lines).toHaveLength(7);
    const named = [
      ['"Bad_ID"', "invalid id"],
      ["(nopage)", 'missing required field "page"'],
      ["(longlabel)", "label is longer"],
      ["(badicon)", "unknown icon"],
      ["(good-work)", "duplicate id"],
      ["(nonbool)", "adminOnly is not a boolean"],
      ["(oddgroup)", "unknown navGroup"],
    ];
    for (const [name, why] of named) {
      expect(lines.filter((l) => l.includes(name) && l.includes(why))).toHaveLength(1);
    }
  }, 30000);

  it("the command menu hides admin-only module entries from non-admins", async () => {
    serveList();
    await mockApi.login("mira@uzi.local", "x");
    mount("/dashboard");
    await screen.findAllByRole("link", { name: "Work Mod" }, { timeout: 5000 });
    act(() => {
      document.body.dispatchEvent(
        new KeyboardEvent("keydown", { key: "k", code: "KeyK", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    const dialog = await screen.findByRole("dialog", { name: "Command menu" });
    const labels = within(dialog).getAllByRole("option").map((o) => o.textContent ?? "");
    expect(labels.some((l) => l.includes("Own Mod"))).toBe(true);
    expect(labels.some((l) => l.includes("Staff Mod"))).toBe(false);
  }, 30000);
});
