// @vitest-environment jsdom
//
// JobCatalog + EnableJobDialog (PRD #1645 D7): one card per catalog entry, the status
// line, and the per-card enable dialog with its advisory pre-enable label check. The
// page-level flows (the Job filter link, partial failure against a refreshed list, the
// row menu opening a dialog) are in pages/Schedules.test.tsx.
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { JobCatalog, catalogEntryId } from "./JobCatalog";
import type { EnableResult } from "./EnableJobDialog";
import { api, type CatalogEntry, type Repo, type Schedule, type ScheduleCatalog } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { checkRepoLabels: vi.fn(), ensureRepoLabels: vi.fn() } };
});
const mockApi = vi.mocked(api);

beforeEach(() => {
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

const REPOS: Repo[] = [
  { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" } as Repo,
  { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" } as Repo,
  { id: "repo-skills", path_with_namespace: "vtmocanu/skills" } as Repo,
];

function entry(over: Partial<CatalogEntry> = {}): CatalogEntry {
  return {
    slug: "bug-triage",
    name: "Bug triage sweep",
    description: "Daily bug sweep",
    target: "sweep",
    cron: "0 2 * * *",
    timezone: "UTC",
    model: "",
    output_mode: "",
    prompt: "",
    labels: ["bug"],
    guidance: "",
    max_issues: 3,
    auto_approve: true,
    wait_on_limit: true,
    ...over,
  };
}

const PROMPT = entry({ slug: "docs-hygiene", name: "Docs hygiene", target: "prompt", labels: [], max_issues: 0, cron: "0 3 * * 1" });

function row(over: Partial<Schedule>): Schedule {
  return {
    id: "d1",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    enabled: true,
    origin: "default",
    catalog_slug: "bug-triage",
    ...over,
  } as Schedule;
}

type Props = Parameters<typeof JobCatalog>[0];

// Harness: the page owns the open dialog's slug.
function Harness(p: Omit<Props, "openSlug" | "onOpenSlug"> & { initialOpen?: string | null }) {
  const [open, setOpen] = useState<string | null>(p.initialOpen ?? null);
  return <JobCatalog {...p} openSlug={open} onOpenSlug={setOpen} />;
}

// onEnable stays a typed Mock (never widened back to the plain prop type by the
// spread), so a test can read its .mock.calls.
type RenderOver = Partial<Omit<Props, "onEnable" | "onShowJob">> & {
  initialOpen?: string | null;
  onEnable?: Mock<Props["onEnable"]>;
};

function renderCatalog({ onEnable: onEnableOver, ...over }: RenderOver = {}) {
  const onEnable = onEnableOver ?? vi.fn<Props["onEnable"]>(async (_e, ids) => ({ enabled: ids, failed: [] }));
  const onShowJob = vi.fn<Props["onShowJob"]>();
  const catalog: ScheduleCatalog = { entries: [entry(), PROMPT], enablements: [] };
  const props = { catalog, schedules: [] as Schedule[], repos: REPOS, busy: false, ...over, onEnable, onShowJob };
  render(<Harness {...props} />);
  return props;
}

const card = (slug: string) => document.getElementById(catalogEntryId(slug))!;
const openDialog = (name: string) => {
  fireEvent.click(screen.getByRole("button", { name: `Enable ${name} on…` }));
  return screen.getByRole("dialog", { name: `Enable ${name} on` });
};
const primary = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: /^(Enable \d|Checking labels|Enabling)/ });

describe("JobCatalog — cards", () => {
  it("renders one card per entry with the catalog default cadence, options and a focusable id", () => {
    renderCatalog();
    const bug = card("bug-triage");
    expect(bug.getAttribute("tabindex")).toBe("-1");
    expect(within(bug).getByText("Bug triage sweep")).toBeTruthy();
    expect(within(bug).getByText("Daily bug sweep")).toBeTruthy();
    expect(within(bug).getByText(/catalog default/).parentElement!.textContent).toBe("Every day at 02:00 · UTC · catalog default");
    expect(within(bug).getByText("label bug")).toBeTruthy();
    expect(within(bug).getByText("max 3")).toBeTruthy();
    expect(within(bug).getByText("inherit model")).toBeTruthy();
    expect(within(bug).getByLabelText("Baked prompt, read-only")).toBeTruthy();
    expect(within(bug).getByText("sweep")).toBeTruthy();
    expect(within(card("docs-hygiene")).getByText("prompt")).toBeTruthy();
  });

  it("a self_improve entry carries no lock marker", () => {
    renderCatalog({
      catalog: { entries: [entry({ slug: "si", name: "Self-improvement", target: "self_improve", labels: [] })], enablements: [] },
    });
    expect(within(card("si")).getByText("self-improve")).toBeTruthy();
    expect(within(card("si")).queryByLabelText("Baked prompt, read-only")).toBeNull();
  });

  it("'Not enabled' is plain text, not a link or button", () => {
    renderCatalog();
    const status = within(card("bug-triage")).getByText("Not enabled");
    expect(status.closest("button, a")).toBeNull();
    // The card's only button is its Enable on… trigger.
    expect(within(card("bug-triage")).getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual([
      "Enable Bug triage sweep on…",
    ]);
  });

  it("an enabled entry's status is a button counting distinct repos and paused ones; it calls onShowJob", () => {
    const p = renderCatalog({
      schedules: [
        row({ id: "d1" }),
        row({ id: "d2", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", enabled: false }),
        row({ id: "u1", origin: "user", catalog_slug: null, repo_id: "repo-skills" }),
      ],
    });
    const link = within(card("bug-triage")).getByRole("button", { name: "Enabled on 2 repos, 1 paused" });
    fireEvent.click(link);
    expect(p.onShowJob).toHaveBeenCalledWith("bug-triage");
    expect(within(card("docs-hygiene")).getByText("Not enabled")).toBeTruthy();
  });

  it("an empty catalog shows a plain empty state", () => {
    renderCatalog({ catalog: { entries: [], enablements: [] } });
    expect(screen.getByText(/No shipped jobs are available/)).toBeTruthy();
  });
});

describe("EnableJobDialog — picking repos", () => {
  it("offers only repos not yet enabled: enabled ones render checked, disabled and labelled", () => {
    renderCatalog({ schedules: [row({ id: "d1", enabled: false })] });
    const dialog = openDialog("Docs hygiene"); // the uzi row is bug-triage's, not this one's
    expect(within(dialog).getAllByRole("checkbox").every((c) => !(c as HTMLInputElement).disabled)).toBe(true);
    fireEvent.keyDown(document, { key: "Escape" });

    const bug = openDialog("Bug triage sweep");
    const uzi = within(bug).getByRole("checkbox", { name: /vtmocanu\/uzi/ }) as HTMLInputElement;
    expect(uzi.checked).toBe(true);
    expect(uzi.disabled).toBe(true);
    expect(uzi.closest("label")!.textContent).toContain("enabled");
    const atlas = within(bug).getByRole("checkbox", { name: /vtmocanu\/atlas/ }) as HTMLInputElement;
    expect(atlas.checked).toBe(false);
    expect(atlas.disabled).toBe(false);
    // Nothing newly checked: Enable 0 is disabled (never a no-op).
    expect((primary(bug) as HTMLButtonElement).disabled).toBe(true);
    expect(primary(bug).textContent).toBe("Enable 0");
  });

  it("the fan-out receives exactly the newly checked ids", async () => {
    const p = renderCatalog({ schedules: [row({ id: "d1" })] });
    const dialog = openDialog("Docs hygiene");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/skills/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/uzi/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/uzi/ })); // unchecked again
    expect(primary(dialog).textContent).toBe("Enable 2");
    await act(async () => fireEvent.click(primary(dialog)));
    expect(p.onEnable).toHaveBeenCalledTimes(1);
    expect(p.onEnable.mock.calls[0][0].slug).toBe("docs-hygiene");
    expect(p.onEnable.mock.calls[0][1]).toEqual(["repo-atlas", "repo-skills"]);
    // Full success closes the dialog.
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a text filter appears only when the owner has more than 8 repos", () => {
    const many = Array.from({ length: 9 }, (_, i) => ({ id: `r${i}`, path_with_namespace: `org/repo-${i}` }) as Repo);
    renderCatalog({ repos: many.slice(0, 8) });
    expect(within(openDialog("Docs hygiene")).queryByRole("textbox", { name: "Filter repos" })).toBeNull();
    cleanup();

    renderCatalog({ repos: many });
    const dialog = openDialog("Docs hygiene");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Filter repos" }), { target: { value: "repo-7" } });
    expect(within(dialog).getAllByRole("checkbox").map((c) => c.closest("label")!.textContent)).toEqual(["org/repo-7"]);
  });
});

describe("EnableJobDialog — the pre-enable label check (binding plan rev2)", () => {
  it("Enable is disabled on the very render after a sweep repo is checked, before any forge call; a click there does nothing", async () => {
    vi.useFakeTimers();
    const p = renderCatalog();
    const dialog = openDialog("Bug triage sweep");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));

    // Inside the 300 ms debounce: no network call yet, and Enable is already held.
    expect(mockApi.checkRepoLabels).not.toHaveBeenCalled();
    const btn = primary(dialog) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent).toBe("Checking labels…");
    fireEvent.click(btn);
    expect(p.onEnable).not.toHaveBeenCalled();

    await act(async () => vi.advanceTimersByTime(300));
    expect(mockApi.checkRepoLabels).toHaveBeenCalledWith("repo-atlas", ["bug"]);
    expect((primary(dialog) as HTMLButtonElement).disabled).toBe(false);
  });

  it("Enable stays disabled while a check is in flight and is enabled once it settles with a missing label", async () => {
    vi.useFakeTimers();
    let settle: (v: { missing: string[] }) => void = () => {};
    mockApi.checkRepoLabels.mockReturnValue(new Promise((r) => (settle = r)));
    const p = renderCatalog();
    const dialog = openDialog("Bug triage sweep");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/skills/ }));

    await act(async () => vi.advanceTimersByTime(300));
    expect(mockApi.checkRepoLabels).toHaveBeenCalledTimes(1); // in flight
    expect((primary(dialog) as HTMLButtonElement).disabled).toBe(true);
    expect(primary(dialog).textContent).toBe("Checking labels…");

    await act(async () => settle({ missing: ["bug"] }));
    // The warning shows, and it is advisory: Enable is offered anyway.
    expect(within(dialog).getByText(/Label “bug” doesn't exist on vtmocanu\/skills/)).toBeTruthy();
    const btn = primary(dialog) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    expect(btn.textContent).toBe("Enable 1");
    await act(async () => fireEvent.click(btn));
    expect(p.onEnable.mock.calls[0][1]).toEqual(["repo-skills"]);
  });

  it("a prompt entry never holds Enable for a label check", () => {
    renderCatalog();
    const dialog = openDialog("Docs hygiene");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    expect((primary(dialog) as HTMLButtonElement).disabled).toBe(false);
    expect(mockApi.checkRepoLabels).not.toHaveBeenCalled();
  });
});

describe("EnableJobDialog — failure and retry", () => {
  it("a partial failure keeps the dialog open, locks the succeeded repo, shows the error inline and retries only the failed one", async () => {
    const results: EnableResult[] = [
      { enabled: ["repo-atlas"], failed: [{ repoId: "repo-skills", message: "forge said no" }] },
      { enabled: ["repo-skills"], failed: [] },
    ];
    const onEnable = vi.fn<Props["onEnable"]>(async () => results.shift()!);
    renderCatalog({ onEnable });
    const dialog = openDialog("Docs hygiene");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/skills/ }));
    await act(async () => fireEvent.click(primary(dialog)));

    expect(screen.getByRole("dialog")).toBe(dialog);
    const atlas = within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }) as HTMLInputElement;
    expect(atlas.checked && atlas.disabled).toBe(true);
    const skills = within(dialog).getByRole("checkbox", { name: /vtmocanu\/skills/ }) as HTMLInputElement;
    expect(skills.checked).toBe(true);
    expect(skills.disabled).toBe(false);
    expect(within(dialog).getByText("forge said no")).toBeTruthy();
    expect(primary(dialog).textContent).toBe("Enable 1");

    await act(async () => fireEvent.click(primary(dialog)));
    expect(onEnable.mock.calls[1][1]).toEqual(["repo-skills"]);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a refused start (null) leaves the dialog as it was", async () => {
    const onEnable = vi.fn<Props["onEnable"]>(async () => null);
    renderCatalog({ onEnable });
    const dialog = openDialog("Docs hygiene");
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    await act(async () => fireEvent.click(primary(dialog)));
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(primary(dialog).textContent).toBe("Enable 1");
  });
});

describe("EnableJobDialog — focus and dismissal (ExtendTimePopover pattern)", () => {
  it("focus moves into the dialog on open; Escape closes it and returns focus to the trigger", () => {
    renderCatalog();
    const trigger = screen.getByRole("button", { name: "Enable Docs hygiene on…" });
    trigger.focus();
    fireEvent.click(trigger);
    const dialog = screen.getByRole("dialog", { name: "Enable Docs hygiene on" });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(dialog);

    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("an outside click closes it", () => {
    renderCatalog();
    openDialog("Docs hygiene");
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a dialog opened by the page (openSlug) mounts open with focus inside", () => {
    renderCatalog({ initialOpen: "bug-triage" });
    const dialog = screen.getByRole("dialog", { name: "Enable Bug triage sweep on" });
    expect(document.activeElement).toBe(dialog);
    expect(card("bug-triage").contains(dialog)).toBe(true);
  });
});
