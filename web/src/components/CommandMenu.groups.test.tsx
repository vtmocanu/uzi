// @vitest-environment jsdom
// #4373: the command menu's search-only groups (sections, agents, schedules, workers, skills,
// recent runs, docs), driven through the CommandMenu itself.
import { afterEach, beforeEach, describe, it, expect, vi, type Mock } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { CommandMenu } from "./CommandMenu";
import * as built from "./commandMenuItems";
import { api, type AgentTemplate, type Repo, type RunSummaryItem, type Schedule, type Skill, type Worker } from "../lib/api";
import { docsForIndex } from "../lib/docs";
import { setDemoMode } from "../lib/demoMode";
import type { DocHeading } from "../lib/docHeadingsPipeline";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listRuns: vi.fn(),
    getRecoveryHolds: vi.fn(),
    listAgentTemplates: vi.fn(),
    listSchedules: vi.fn(),
    listWorkers: vi.fn(),
    listSkills: vi.fn(),
  },
}));

const mocks = vi.hoisted(() => ({ headingsFor: vi.fn() }));
vi.mock("../lib/docHeadings", () => ({ docHeadingsFor: mocks.headingsFor }));

const mockApi = vi.mocked(api);
const listRunsMock = mockApi.listRuns as unknown as Mock<(p: { view: "summary" }) => Promise<{ runs: RunSummaryItem[] }>>;

const UNSAFE = String.fromCharCode(0x202e);
const BEL = String.fromCharCode(7);

const userDoc = docsForIndex(false)[0];
const operatorDoc = docsForIndex(true).find((d) => !docsForIndex(false).some((u) => u.slug === d.slug))!;

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

const agent = (id: string, name: string) => ({ id, name }) as unknown as AgentTemplate;
const worker = (id: string, name: string) => ({ id, name }) as unknown as Worker;
const skill = (id: string, name: string, scope: Skill["scope"] = "global") => ({ id, name, scope }) as unknown as Skill;
const schedule = (over: Partial<Schedule> & { id: string }) =>
  ({ origin: "user", catalog_slug: null, target: "sweep", labels: null, repo_path: "", ...over }) as unknown as Schedule;
const run = (over: Partial<RunSummaryItem> & { id: string }) =>
  ({ status: "completed", kind: "issue", health: "ok", issue_iid: null, issue_title: "", title: null, ...over }) as unknown as RunSummaryItem;
const repo = (id: string, path: string) => ({ id, path_with_namespace: path }) as unknown as Repo;

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
  mocks.headingsFor.mockImplementation(() => []);
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  listRunsMock.mockResolvedValue({ runs: [] });
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
  mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
  mockApi.listSchedules.mockResolvedValue([]);
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSkills.mockResolvedValue({ skills: [] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.clearAllMocks();
});

function Probe() {
  const loc = useLocation();
  return <output data-testid="loc">{loc.pathname + loc.search + loc.hash}</output>;
}

function mount(isAdmin = false) {
  return render(
    <MemoryRouter initialEntries={["/start"]}>
      <Probe />
      <CommandMenu onClose={() => {}} isAdmin={isAdmin} />
    </MemoryRouter>,
  );
}

const flush = () => act(async () => void (await new Promise((r) => setTimeout(r, 0))));
const input = () => screen.getByRole("combobox");
const labels = () => screen.queryAllByRole("option").map((o) => o.querySelector("span.truncate")?.textContent ?? "");
const optionFor = (label: string) =>
  screen.queryAllByRole("option").find((o) => o.querySelector("span.truncate")?.textContent === label);
const here = () => screen.getByTestId("loc").textContent;

async function search(q: string, isAdmin = false) {
  mount(isAdmin);
  await flush();
  fireEvent.change(input(), { target: { value: q } });
  await flush();
  await flush();
}

describe("sections", () => {
  it("'appear' puts Settings > Appearance first", async () => {
    await search("appear");
    expect(labels()[0]).toBe("Settings › Appearance");
  });

  it("ranks a board for acme/fooslack below Admin > Settings > Slack for an admin", async () => {
    mockApi.listRepos.mockResolvedValue({ repos: [repo("r1", "acme/fooslack")] });
    await search("slack", true);
    const l = labels();
    expect(l).toContain("Open board: acme/fooslack");
    expect(l.indexOf("Admin › Settings › Slack")).toBeGreaterThanOrEqual(0);
    expect(l.indexOf("Admin › Settings › Slack")).toBeLessThan(l.indexOf("Open board: acme/fooslack"));
  });

  it("offers admin sections to admins only", async () => {
    await search("slack", false);
    expect(labels()).not.toContain("Admin › Settings › Slack");
    cleanup();
    await search("slack", true);
    expect(labels()).toContain("Admin › Settings › Slack");
    expect(optionFor("Admin › Settings › Slack")?.textContent).toContain("Admin");
  });
});

describe("loaded lists", () => {
  it("finds an agent, a schedule (a default row by its spaced slug), a worker and a skill but not a product skill", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [agent("a1", "Zorblax reviewer")] });
    mockApi.listSchedules.mockResolvedValue([
      schedule({ id: "s1", labels: ["zorblax"], repo_path: "acme/api" }),
      schedule({ id: "s2", origin: "default", catalog_slug: "zorblax-nightly-watch", repo_path: "" }),
    ]);
    mockApi.listWorkers.mockResolvedValue({ workers: [worker("w1", "zorblax-worker")] });
    mockApi.listSkills.mockResolvedValue({
      skills: [skill("k1", "zorblax playbook"), skill("k2", "zorblax internal", "product")],
    });
    await search("zorblax");
    const l = labels();
    expect(l).toContain("Zorblax reviewer");
    expect(l).toContain("Sweep · label zorblax · acme/api");
    expect(l).toContain("zorblax-worker");
    expect(l).toContain("zorblax playbook");
    expect(l).not.toContain("zorblax internal");
    cleanup();
    await search("zorblax nightly");
    expect(labels()).toContain("zorblax-nightly-watch");
  });

  it("finds a run by its issue number and by its title", async () => {
    listRunsMock.mockResolvedValue({
      runs: [
        run({ id: "r1", issue_iid: 4123, issue_title: "Fix the flux capacitor", repo_path: "acme/api" } as never),
        run({ id: "r2", title: "Quokka chat" }),
        run({ id: "r2", title: "Quokka chat" }),
      ],
    });
    await search("#4123");
    expect(labels()).toContain("#4123 · Fix the flux capacitor");
    expect(optionFor("#4123 · Fix the flux capacitor")?.textContent).toContain("Recent runs · acme/api");
    cleanup();
    await search("quokka");
    expect(labels().filter((x) => x === "Quokka chat")).toHaveLength(1);
  });

  it("finds a doc title, a heading and a non-title H1, and keeps operator doc headings from non-admins", async () => {
    mocks.headingsFor.mockImplementation((slug: string): DocHeading[] => {
      if (slug === userDoc.slug) {
        return [
          { depth: 1, text: userDoc.meta.title, id: "page-title" },
          { depth: 2, text: "Zebrafish tuning", id: "zebrafish-tuning" },
          { depth: 1, text: "Second Quagga", id: "second-quagga" },
          { depth: 2, text: "Footnotes", id: "footnotes" },
        ];
      }
      if (slug === operatorDoc.slug) return [{ depth: 2, text: "Operatorsecret heading", id: "op" }];
      return [];
    });
    await search(userDoc.meta.title.toLowerCase());
    expect(labels()).toContain(userDoc.meta.title);
    expect(labels()).not.toContain(`${userDoc.meta.title} › ${userDoc.meta.title}`);
    cleanup();
    await search("zebrafish");
    expect(labels()).toContain(`${userDoc.meta.title} › Zebrafish tuning`);
    cleanup();
    await search("quagga");
    expect(labels()).toContain(`${userDoc.meta.title} › Second Quagga`);
    cleanup();
    await search("footnotes");
    expect(labels()).not.toContain(`${userDoc.meta.title} › Footnotes`);
    cleanup();
    await search("operatorsecret", false);
    expect(labels()).toEqual([]);
    cleanup();
    await search("operatorsecret", true);
    expect(labels()).toContain(`${operatorDoc.meta.title} › Operatorsecret heading`);
  });

  it("shows only today's groups for an empty query", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [agent("a1", "Zorblax reviewer")] });
    mockApi.listWorkers.mockResolvedValue({ workers: [worker("w1", "zorblax-worker")] });
    mount();
    await flush();
    await flush();
    const groups = screen.getAllByRole("group").map((g) => g.getAttribute("aria-label"));
    expect(groups).toEqual(["Actions", "Go to"]);
    expect(labels()).not.toContain("Zorblax reviewer");
    expect(labels().some((l) => l.includes("›"))).toBe(false);
    expect(mocks.headingsFor).not.toHaveBeenCalled();
  });
});

describe("failed reads", () => {
  const cases: [string, () => void, string][] = [
    ["agents", () => mockApi.listAgentTemplates.mockRejectedValue(new Error("x")), "Zorblax reviewer"],
    ["schedules", () => mockApi.listSchedules.mockRejectedValue(new Error("x")), "Sweep · label zorblax"],
    ["workers", () => mockApi.listWorkers.mockRejectedValue(new Error("x")), "zorblax-worker"],
    ["skills", () => mockApi.listSkills.mockRejectedValue(new Error("x")), "zorblax playbook"],
  ];
  const all = () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [agent("a1", "Zorblax reviewer")] });
    mockApi.listSchedules.mockResolvedValue([schedule({ id: "s1", labels: ["zorblax"] })]);
    mockApi.listWorkers.mockResolvedValue({ workers: [worker("w1", "zorblax-worker")] });
    mockApi.listSkills.mockResolvedValue({ skills: [skill("k1", "zorblax playbook")] });
    listRunsMock.mockResolvedValue({ runs: [run({ id: "r1", title: "Zorblax chat" })] });
  };
  const everything = ["Zorblax reviewer", "Sweep · label zorblax", "zorblax-worker", "zorblax playbook", "Zorblax chat"];

  it.each(cases)("a failed %s read hides only that group, silently", async (_name, fail, hidden) => {
    all();
    fail();
    await search("zorblax");
    const l = labels();
    expect(l).not.toContain(hidden);
    for (const other of everything.filter((x) => x !== hidden)) expect(l).toContain(other);
    expect(screen.queryByText(/unavailable/)).toBeNull();
  });

  it("a failed run read hides recent runs and decisions", async () => {
    all();
    listRunsMock.mockRejectedValue(new Error("x"));
    await search("zorblax");
    expect(labels()).not.toContain("Zorblax chat");
    expect(labels()).toContain("Zorblax reviewer");
    expect(screen.getByText("Decisions unavailable")).toBeTruthy();
  });

  it("a failed holds read hides only decisions, not recent runs", async () => {
    all();
    mockApi.getRecoveryHolds.mockRejectedValue(new Error("x"));
    await search("zorblax");
    expect(labels()).toContain("Zorblax chat");
    expect(screen.getByText("Decisions unavailable")).toBeTruthy();
  });
});

describe("overflow", () => {
  it("keeps 5 matching workers and counts the rest in a note outside the listbox", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: Array.from({ length: 7 }, (_, i) => worker(`w${i}`, `kestrel-${i}`)),
    });
    await search("kestrel");
    expect(labels().filter((l) => l.startsWith("kestrel-"))).toHaveLength(5);
    const note = screen.getByText("+2 more workers");
    expect(note.closest('[role="listbox"]')).toBeNull();
    expect(note.getAttribute("role")).toBeNull();
    expect(within(screen.getByRole("dialog")).getByRole("status").textContent).toContain("+2 more workers");
  });

  it("joins several overflow counts", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: Array.from({ length: 7 }, (_, i) => worker(`w${i}`, `kestrel-${i}`)),
    });
    mockApi.listAgentTemplates.mockResolvedValue({
      templates: Array.from({ length: 6 }, (_, i) => agent(`a${i}`, `kestrel-agent-${i}`)),
    });
    await search("kestrel");
    expect(screen.getByText("+1 more agent · +2 more workers")).toBeTruthy();
  });
});

describe("choosing an item", () => {
  it("navigates to each kind's exact path, ids encoded", async () => {
    mocks.headingsFor.mockImplementation((slug) =>
      slug === userDoc.slug ? [{ depth: 2, text: "Zebrafish tuning", id: "zebrafish tuning" }] : [],
    );
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [agent("a/1 x", "Zorblax agent")] });
    mockApi.listSchedules.mockResolvedValue([schedule({ id: "s/1 x", labels: ["zorblax"] })]);
    mockApi.listWorkers.mockResolvedValue({ workers: [worker("w/1 x", "zorblax-worker")] });
    mockApi.listSkills.mockResolvedValue({ skills: [skill("k/1 x", "zorblax playbook")] });
    listRunsMock.mockResolvedValue({ runs: [run({ id: "r/1 x", title: "Zorblax chat" })] });
    const expected: [string, string, string][] = [
      ["zorblax", "Zorblax agent", "/agents/a%2F1%20x"],
      ["zorblax", "Sweep · label zorblax", "/schedules?tab=schedules#schedule-name-s%2F1%20x"],
      ["zorblax", "zorblax-worker", "/workers?tab=workers#worker-row-w%2F1%20x"],
      ["zorblax", "zorblax playbook", "/skills?skill=k%2F1%20x"],
      ["zorblax", "Zorblax chat", "/runs/r%2F1%20x"],
      ["Settings › Appearance".toLowerCase(), "Settings › Appearance", "/settings/appearance"],
      [userDoc.meta.title.toLowerCase(), userDoc.meta.title, `/docs/${userDoc.slug}`],
      ["zebrafish", `${userDoc.meta.title} › Zebrafish tuning`, `/docs/${userDoc.slug}#zebrafish%20tuning`],
    ];
    for (const [q, label, to] of expected) {
      await search(q);
      const opt = optionFor(label);
      expect(opt, label).toBeTruthy();
      fireEvent.click(opt as HTMLElement);
      expect(here(), label).toBe(to);
      cleanup();
    }
  });
});

describe("untrusted text", () => {
  it("strips bidi and control characters from agent, worker, skill and run titles and the run hint", async () => {
    const bad = `${UNSAFE}evil${BEL}`;
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [agent("a1", `${bad} agent`)] });
    mockApi.listWorkers.mockResolvedValue({ workers: [worker("w1", `${bad} worker`)] });
    mockApi.listSkills.mockResolvedValue({ skills: [skill("k1", `${bad} skill`)] });
    listRunsMock.mockResolvedValue({ runs: [run({ id: "r1", title: `${bad} run`, repo_path: `acme/${bad}x` } as never)] });
    await search("evil");
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(4);
    for (const o of options) {
      expect(o.textContent).not.toContain(UNSAFE);
      expect(o.textContent).not.toContain(BEL);
    }
    expect(labels()).toEqual(expect.arrayContaining(["evil agent", "evil worker", "evil skill", "evil run"]));
    expect(optionFor("evil run")?.textContent).toContain("Recent runs · acme/evilx");
  });

  it("masks schedule and run repo paths in demo mode", async () => {
    window.localStorage.setItem("uzi_demo_mode", "1");
    mockApi.listSchedules.mockResolvedValue([schedule({ id: "s1", labels: ["zorblax"], repo_path: "acme/secretrepo" })]);
    listRunsMock.mockResolvedValue({ runs: [run({ id: "r1", title: "Zorblax chat", repo_path: "acme/secretrepo" } as never)] });
    await search("zorblax");
    expect(labels()).toContain("Sweep · label zorblax · demo/secretrepo");
    expect(optionFor("Zorblax chat")?.textContent).toContain("Recent runs · demo/secretrepo");
    expect(document.body.textContent).not.toContain("acme");
  });
});

describe("demo masking is live", () => {
  it("re-masks schedule and run rows when demo mode toggles while the menu is open", async () => {
    mockApi.listSchedules.mockResolvedValue([schedule({ id: "s1", labels: ["zorblax"], repo_path: "acme/secretrepo" })]);
    listRunsMock.mockResolvedValue({ runs: [run({ id: "r1", title: "Zorblax chat", repo_path: "acme/secretrepo" } as never)] });
    await search("zorblax");
    expect(labels()).toContain("Sweep · label zorblax · acme/secretrepo");
    expect(optionFor("Zorblax chat")?.textContent).toContain("Recent runs · acme/secretrepo");
    act(() => setDemoMode(true));
    expect(labels()).toContain("Sweep · label zorblax · demo/secretrepo");
    expect(optionFor("Zorblax chat")?.textContent).toContain("Recent runs · demo/secretrepo");
    expect(document.body.textContent).not.toContain("acme");
    act(() => setDemoMode(false));
  });
});

describe("real docs ranking", () => {
  it("ranks the Board doc above 'Open board: acme/network' for 'board'", async () => {
    mockApi.listRepos.mockResolvedValue({ repos: [repo("r1", "acme/network")] });
    await search("board");
    expect(labels()[0]).toBe("Board");
    expect(optionFor("Board")?.textContent).toContain("Doc");
    expect(labels()).toContain("Open board: acme/network");
  });
});

describe("fetching", () => {
  it("calls each list once per opening, and never again", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "setInterval"] });
    mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    fireEvent.change(input(), { target: { value: "zorblax" } });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(mockApi.listAgentTemplates).toHaveBeenCalledTimes(1);
    expect(mockApi.listSchedules).toHaveBeenCalledTimes(1);
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(1);
    expect(mockApi.listSkills).toHaveBeenCalledTimes(1);
    expect(listRunsMock).toHaveBeenCalledTimes(1);
  });

  type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void };
  function deferred<T>(): Deferred<T> {
    let resolve: (v: T) => void = () => {};
    const promise = new Promise<T>((r) => (resolve = r));
    return { promise, resolve };
  }

  const lateCases: [string, () => { resolve: () => void }, "agentCommandItems" | "scheduleCommandItems" | "workerCommandItems" | "skillCommandItems" | "runCommandItems"][] = [
    [
      "agents",
      () => {
        const d = deferred<{ templates: AgentTemplate[] }>();
        mockApi.listAgentTemplates.mockReturnValue(d.promise);
        return { resolve: () => d.resolve({ templates: [agent("a1", "x")] }) };
      },
      "agentCommandItems",
    ],
    [
      "schedules",
      () => {
        const d = deferred<Schedule[]>();
        mockApi.listSchedules.mockReturnValue(d.promise);
        return { resolve: () => d.resolve([schedule({ id: "s1" })]) };
      },
      "scheduleCommandItems",
    ],
    [
      "workers",
      () => {
        const d = deferred<{ workers: Worker[] }>();
        mockApi.listWorkers.mockReturnValue(d.promise);
        return { resolve: () => d.resolve({ workers: [worker("w1", "x")] }) };
      },
      "workerCommandItems",
    ],
    [
      "skills",
      () => {
        const d = deferred<{ skills: Skill[] }>();
        mockApi.listSkills.mockReturnValue(d.promise);
        return { resolve: () => d.resolve({ skills: [skill("k1", "x")] }) };
      },
      "skillCommandItems",
    ],
    [
      "runs",
      () => {
        const d = deferred<{ runs: RunSummaryItem[] }>();
        listRunsMock.mockReturnValue(d.promise);
        return { resolve: () => d.resolve({ runs: [run({ id: "r1" })] }) };
      },
      "runCommandItems",
    ],
  ];

  it.each(lateCases)("a late %s response is dropped before its builder runs", async (_name, defer, builder) => {
    const spy = vi.spyOn(built, builder);
    // Control: resolved while the menu is open, the builder runs.
    const open = defer();
    const first = mount();
    await flush();
    open.resolve();
    await flush();
    expect(spy).toHaveBeenCalledTimes(1);
    first.unmount();
    spy.mockClear();
    // Closed before the response lands: the builder never runs.
    const late = defer();
    const second = mount();
    await flush();
    second.unmount();
    late.resolve();
    await flush();
    await flush();
    expect(spy).not.toHaveBeenCalled();
  });

  it("does not store a heading index that lands after the menu closed", async () => {
    const lookup = vi.spyOn(built, "headingLookup");
    // Control: while open, the index is used.
    mount();
    await flush();
    fireEvent.change(input(), { target: { value: "zorblax" } });
    await flush();
    await flush();
    expect(lookup).toHaveBeenCalledTimes(1);
    expect(mocks.headingsFor).toHaveBeenCalled();
    cleanup();
    lookup.mockClear();
    mocks.headingsFor.mockClear();
    // Closed in the same tick as the first query, before the import resolves.
    const view = mount();
    await flush();
    fireEvent.change(input(), { target: { value: "zorblax" } });
    view.unmount();
    await flush();
    await flush();
    expect(lookup).not.toHaveBeenCalled();
    expect(mocks.headingsFor).not.toHaveBeenCalled();
  });
});

// Last in the file: it resets the module registry to make the heading import fail.
describe("heading index failure", () => {
  it("a rejected heading import keeps doc titles but shows no headings", async () => {
    vi.resetModules();
    vi.doMock("../lib/docHeadings", () => {
      throw new Error("heading index unavailable");
    });
    try {
      await search(userDoc.meta.title.toLowerCase());
      expect(labels()).toContain(userDoc.meta.title);
      expect(labels().some((l) => l.startsWith(`${userDoc.meta.title} › `))).toBe(false);
      expect(mocks.headingsFor).not.toHaveBeenCalled();
    } finally {
      vi.doMock("../lib/docHeadings", () => ({ docHeadingsFor: mocks.headingsFor }));
      vi.resetModules();
    }
  });
});
