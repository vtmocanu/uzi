// @vitest-environment jsdom
//
// Health tab component tests (PRD #1484 M4, triage-first layout PRD #1648 M3/M4). They mount
// the real AdminHealth page (which wraps itself in AdminShell) and feed it fixtures through
// the mocked api client — the same pattern CustodyBoardAlert.test.tsx uses. Covered: the
// header line (Copy diagnostics), the attention card in its danger / warn / unknown forms or
// the all-clear line, the All checks inventory (disclosure, chips, counts, poll survival,
// in-page jumps with reduced motion), na on a no-hosted-workers fixture, and the Fleet card
// (M5: headers, status/upgrade badge words, skeleton loading, last-good rows on a failed poll).
// Severity is asserted by ACCESSIBLE NAME / TEXT (the badge word, or the sr-only span beside
// the aria-hidden severity shape that carries the word), never a colour class; the tab pip's worded aria-label is asserted; and a hostile worker name /
// blocking reason rendered into a `title=` attribute is asserted on the attribute.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { StrictMode } from "react";

import { AdminHealth } from "./AdminHealth";
import { likelyCause } from "../components/WorkerUpgradeBadge";
import { api, type AdminWorker, type HealthDoc } from "../lib/api";
import {
  ownerOnlyDoc,
  ownerOnlyWaitingOwnerId,
  ownerOnlyWaitingRunId,
  degradedDoc,
  healthySilentDoc,
  incidentDoc,
  incidentFleetWorkers,
  noHostedWorkersDoc,
  unknownDoc,
} from "../mocks/data/health";

// The page reads GET /api/admin/health (via the shared useAdminHealth hook, used by both the
// page and AdminShell's tab pip) and GET /api/admin/workers. Mock only those two.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn(), adminListWorkers: vi.fn() } };
});
const mockApi = vi.mocked(api);

const originalMatchMedia = window.matchMedia;
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");

afterEach(() => {
  cleanup();
  vi.clearAllTimers();
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.clearAllMocks();
  if (originalClipboard) Object.defineProperty(navigator, "clipboard", originalClipboard);
  else Reflect.deleteProperty(navigator, "clipboard");
  window.matchMedia = originalMatchMedia;
});

async function renderHealth(doc: HealthDoc, fleet: AdminWorker[] = [], strict = false) {
  mockApi.getAdminHealth.mockResolvedValue(doc);
  mockApi.adminListWorkers.mockResolvedValue({ workers: fleet });
  const utils = render(
    <MemoryRouter initialEntries={["/admin/health"]}>
      <AdminHealth />
    </MemoryRouter>,
    { wrapper: strict ? StrictMode : undefined },
  );
  // The Copy diagnostics button renders only once the doc resolves, so waiting on it ensures
  // no assertion races the load (and it is unambiguous — one per page).
  await waitFor(() => expect(screen.getByText("Copy diagnostics")).toBeTruthy());
  // Settle the loaded content's mount effects before a test switches to fake timers.
  await act(async () => {});
  return utils;
}

// An attention item (or null) for a check id (the id has a dot, so an attribute selector).
function attentionItem(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector(`[id="c-${id}"]`);
}

// The ids of the attention items, in DOM order.
function attentionIds(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll('[id^="c-"]')).map((el) => el.id.slice(2));
}

// The attention band: the header that holds the verdict heading.
function band(title: string): HTMLElement {
  const header = screen.getByRole("heading", { name: title }).closest("header");
  if (!header) throw new Error(`no band for ${title}`);
  return header;
}

function groupButton(name: string): HTMLElement {
  return screen.getByRole("button", { name });
}

// Recompute a doc's tally + status after a test reshapes its checks (mirrors the fixture's).
function retally(doc: HealthDoc): HealthDoc {
  const counts = { ok: 0, warn: 0, danger: 0, unknown: 0, na: 0 };
  for (const c of doc.checks) counts[c.severity as keyof typeof counts]++;
  const status = counts.danger > 0 ? "danger" : counts.warn + counts.unknown > 0 ? "warn" : "ok";
  return { ...doc, counts, status, blocking: doc.checks.some((c) => c.scope === "instance" && c.severity === "danger") };
}

function withSeverity(doc: HealthDoc, over: Record<string, string>): HealthDoc {
  return retally({ ...doc, checks: doc.checks.map((c) => (over[c.id] ? { ...c, severity: over[c.id] } : c)) });
}

function stubReducedMotion(reduce: boolean) {
  window.matchMedia = ((query: string) => ({
    matches: reduce && query.includes("prefers-reduced-motion: reduce"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
}

describe("AdminHealth — attention card and all-clear line (M3)", () => {
  it("shows both Codex pricing reasons and operator docs without blocking work", async () => {
    const doc = healthySilentDoc();
    expect(doc.checks).toHaveLength(18);
    const pricing = doc.checks.find((c) => c.id === "pricing.codex")!;
    pricing.severity = "warn";
    pricing.summary = "2 Codex models lack usable pricing";
    pricing.evidence = [
      { label: "gpt-5.5", value: "3 runs, no price" },
      { label: "gpt-5.6-sol", value: "2 runs, promotional price expired" },
    ];
    const { container } = await renderHealth(retally(doc));
    const item = attentionItem(container, "pricing.codex")!;
    for (const label of ["gpt-5.5", "gpt-5.6-sol"]) expect(within(item).getByText(label)).toBeTruthy();
    for (const value of ["3 runs, no price", "2 runs, promotional price expired"]) expect(within(item).getByText(value)).toBeTruthy();
    expect(within(item).getByRole("link", { name: "Docs: admin-health" }).getAttribute("href")).toBe("/docs/admin-health");
    expect(band("1 check needs attention").getAttribute("role")).toBe("status");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("all clear (silent): one quiet line, no attention card, no tab pip", async () => {
    const { container } = await renderHealth(healthySilentDoc());
    expect(screen.getByText("All systems normal.")).toBeTruthy();
    expect(screen.getByText("Nothing needs attention.")).toBeTruthy();
    expect(screen.getByText("refreshes every 10 s")).toBeTruthy();
    // No attention band (no alert/status region, no verdict heading) and no attention items.
    expect(container.querySelector('[role="alert"], [role="status"]')).toBeNull();
    expect(screen.queryByRole("heading", { name: /blocking|attention/ })).toBeNull();
    expect(attentionIds(container)).toEqual([]);
    // Every check's shape carries its word; all 18 read OK, none Warn/Danger.
    expect(screen.getAllByText("OK").length).toBe(18);
    expect(screen.queryByText(/^(Warn|Danger|Unknown)$/)).toBeNull();
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });

  it("keeps owner danger evidence and action visible without claiming an instance block", async () => {
    const { container } = await renderHealth(ownerOnlyDoc());
    expect(band("3 checks need attention; no instance-wide blocker detected").getAttribute("role")).toBe("alert");
    const capacity = attentionItem(container, "fleet.capacity")!;
    expect(within(capacity).getByText("2")).toBeTruthy();
    for (const id of ["fleet.capacity", "queue.waiting"]) {
      const item = attentionItem(container, id)!;
      expect(within(item).getAllByText("Waiting run")).toHaveLength(2);
      expect(within(item).getByText(`run ${ownerOnlyWaitingRunId}; owner ${ownerOnlyWaitingOwnerId}; waited 36m; stored reason no online worker can run this — it needs a capability none of your workers has; provision a ca`)).toBeTruthy();
    }
    expect(within(capacity).getByText(/Recover or provision a worker/)).toBeTruthy();
    expect(attentionIds(container)).toEqual(["fleet.capacity", "queue.waiting", "forge.ciwatch"]);
  });

  it("warn (degraded): a warn status band and both warn items expanded with their action", async () => {
    const { container } = await renderHealth(degradedDoc());
    // Every non-ok/non-na check contributes to the neutral attention headline.
    const b = band("2 checks need attention");
    expect(b.getAttribute("role")).toBe("status");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(within(b).getByText("Review the flagged checks below for evidence and what to do.")).toBeTruthy();
    expect(within(b).getByText("2 Warn")).toBeTruthy();
    // Both warn items are fully expanded (no <details>, nothing to click): the action is there.
    expect(container.querySelector("details")).toBeNull();
    expect(attentionIds(container)).toEqual(["forge.ciwatch", "schedules.paused"]);
    const ci = attentionItem(container, "forge.ciwatch")!;
    expect(within(ci).getByText("What to do.")).toBeTruthy();
    expect(within(ci).getByText(/Raise CI_WATCH_MAX_REFS/)).toBeTruthy();
    expect(within(ci).getByText("27 of 20 watched")).toBeTruthy();
    const paused = attentionItem(container, "schedules.paused")!;
    expect(within(paused).getByText(/will not run until they unpause/)).toBeTruthy();
    // Tab pip: present, warning-worded accessible name, count 2. Awaited because AdminShell's
    // own useAdminHealth resolves independently of the page's.
    const pip = await screen.findByLabelText("2 health checks need attention: 2 warning");
    expect(within(pip).getByText("2")).toBeTruthy();
  });

  it("danger (incident): alert band, derived sub line, worst first, Fleet link only on fleet.* items", async () => {
    const { container } = await renderHealth(incidentDoc(), incidentFleetWorkers());
    // Only one of the three danger checks has instance scope.
    const b = band("uzi cannot run work: 1 blocking check");
    expect(b.getAttribute("role")).toBe("alert");
    // The shared sub line makes no workflow claim for owner attention.
    expect(
      within(b).getByText("Work is blocked until these clear. Start with the flagged checks below."),
    ).toBeTruthy();
    expect(within(b).getByText("3 Danger")).toBeTruthy();
    expect(within(b).getByText("1 Warn")).toBeTruthy();
    expect(within(b).queryByText(/OK/)).toBeNull();
    expect(attentionIds(container)).toEqual(["fleet.roll", "fleet.capacity", "queue.waiting", "forge.ciwatch"]);
    // The Fleet link sits on the two fleet.* items only, and its target exists.
    const fleetLinks = screen.getAllByRole("link", { name: /See the affected workers in Fleet/ });
    expect(fleetLinks.length).toBe(2);
    expect(within(attentionItem(container, "fleet.roll")!).getByRole("link", { name: /affected workers/ })).toBeTruthy();
    expect(within(attentionItem(container, "fleet.capacity")!).getByRole("link", { name: /affected workers/ })).toBeTruthy();
    expect(within(attentionItem(container, "queue.waiting")!).queryByRole("link", { name: /affected workers/ })).toBeNull();
    expect(fleetLinks[0].getAttribute("href")).toBe("#fleet");
    const fleet = document.getElementById("fleet")!;
    expect(fleet).toBeTruthy();
    // Activating it scrolls the Fleet card into view (smooth by default).
    fleet.scrollIntoView = vi.fn();
    fireEvent.click(fleetLinks[0]);
    expect(fleet.scrollIntoView).toHaveBeenCalledWith({ block: "start", behavior: "smooth" });
    // The command has a Copy button; the footer carries the check id and its docs link.
    const roll = attentionItem(container, "fleet.roll")!;
    expect(within(roll).getByRole("button", { name: "Copy" })).toBeTruthy();
    expect(within(roll).getByRole("link", { name: "Docs: worker-upgrades" })).toBeTruthy();
    // Tab pip worded as danger, count 4 (3 danger + 1 warn). Awaited for the same reason.
    const pip = await screen.findByLabelText("4 health checks need attention: 3 danger, 1 warning");
    expect(within(pip).getByText("4")).toBeTruthy();
    // The cross-user fleet table is populated across two owners, with the blocking cause.
    expect((await screen.findAllByText("user.a@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("user.c@uzi.local")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("worker: ImagePullBackOff")).length).toBe(4);
  });

  it("orders danger before unknown before warn while retaining severity totals", async () => {
    // slack.socket (after the warn forge.ciwatch in registry order) turned unknown.
    const { container } = await renderHealth(withSeverity(incidentDoc(), { "slack.socket": "unknown" }));
    expect(attentionIds(container)).toEqual(["fleet.roll", "fleet.capacity", "queue.waiting", "slack.socket", "forge.ciwatch"]);
    const b = band("uzi cannot run work: 1 blocking check");
    expect(within(b).getByText("Work is blocked until these clear. Start with the flagged checks below.")).toBeTruthy();
    expect(within(b).getAllByText(/^\d+ (Danger|Unknown|Warn)$/).map((el) => el.textContent)).toEqual([
      "3 Danger",
      "1 Unknown",
      "1 Warn",
    ]);
  });

  it("danger without warnings retains the shared blocker guidance", async () => {
    await renderHealth(withSeverity(incidentDoc(), { "forge.ciwatch": "ok" }));
    const b = band("uzi cannot run work: 1 blocking check");
    expect(within(b).getByText("Work is blocked until these clear. Start with the flagged checks below.")).toBeTruthy();
    expect(within(b).queryByText(/Plus/)).toBeNull();
  });

  it("unknown: Unknown items render expanded in a warn band (a stale signal is never green)", async () => {
    const { container } = await renderHealth(unknownDoc());
    // Overall ranks unknown as warn: the verdict is the warn one, not danger. The headline
    // counts all four attention checks; the pip names their unknown severity separately.
    const b = band("4 checks need attention");
    expect(b.getAttribute("role")).toBe("status");
    expect(within(b).getByText("4 Unknown")).toBeTruthy();
    expect(attentionIds(container).sort()).toEqual(["controller.report", "fleet.capacity", "fleet.roll", "queue.waiting"]);
    const roll = attentionItem(container, "fleet.roll")!;
    expect(within(roll).getByText("Unknown")).toBeTruthy();
    expect(within(roll).getByText(/Check that the worker controller is running/)).toBeTruthy();
    expect(attentionItem(container, "db")).toBeNull();
    expect(await screen.findByLabelText("4 health checks need attention: 4 unknown")).toBeTruthy();
  });

  it("Copy diagnostics copies the whole document as JSON and confirms", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const doc = degradedDoc();
    await renderHealth(doc);
    expect(screen.getByText(/^Checked /)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    expect(writeText).toHaveBeenCalledWith(JSON.stringify(doc, null, 2));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
  });
});

describe("AdminHealth — copy feedback lifecycle", () => {
  for (const kind of ["diagnostics", "command"] as const) {
    it(`clears the exact ${kind} feedback timer on unmount`, async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      const { container, unmount } = await renderHealth(incidentDoc());
      const button = kind === "diagnostics"
        ? screen.getByRole("button", { name: "Copy diagnostics" })
        : within(attentionItem(container, "fleet.roll")!).getByRole("button", { name: "Copy" });
      vi.useFakeTimers();
      const setTimeout = vi.spyOn(window, "setTimeout");
      const clearTimeout = vi.spyOn(window, "clearTimeout");
      await act(async () => { fireEvent.click(button); });
      expect(writeText).toHaveBeenCalledTimes(1);
      expect(button.textContent).toBe("Copied");
      const feedbackCalls = setTimeout.mock.calls
        .map((args, index) => ({ delay: args[1], id: setTimeout.mock.results[index].value }))
        .filter(({ delay }) => delay === 1600);
      expect(feedbackCalls).toHaveLength(1);
      const timerId = feedbackCalls[0].id;
      unmount();
      act(() => { vi.advanceTimersByTime(1700); });
      expect(clearTimeout).toHaveBeenCalledWith(timerId);
    });
  }

  for (const kind of ["diagnostics", "command"] as const) {
    it(`resets ${kind} feedback at 1600 ms and preserves the payload`, async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      const doc = incidentDoc();
      const { container } = await renderHealth(doc);
      const roll = attentionItem(container, "fleet.roll")!;
      const button = kind === "diagnostics"
        ? screen.getByRole("button", { name: "Copy diagnostics" })
        : within(roll).getByRole("button", { name: "Copy" });
      const payload = kind === "diagnostics"
        ? JSON.stringify(doc, null, 2)
        : doc.checks.find((check) => check.id === "fleet.roll")!.command;
      vi.useFakeTimers();
      await act(async () => { fireEvent.click(button); });
      expect(writeText).toHaveBeenCalledExactlyOnceWith(payload);
      expect(button.textContent).toBe("Copied");
      act(() => { vi.advanceTimersByTime(1599); });
      expect(button.textContent).toBe("Copied");
      act(() => { vi.advanceTimersByTime(1); });
      expect(button.textContent).toBe(kind === "diagnostics" ? "Copy diagnostics" : "Copy");
    });

    it(`rearms ${kind} feedback from the latest click`, async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      const { container } = await renderHealth(incidentDoc());
      const button = kind === "diagnostics"
        ? screen.getByRole("button", { name: "Copy diagnostics" })
        : within(attentionItem(container, "fleet.roll")!).getByRole("button", { name: "Copy" });
      vi.useFakeTimers();
      const setTimeout = vi.spyOn(window, "setTimeout");
      const clearTimeout = vi.spyOn(window, "clearTimeout");
      await act(async () => { fireEvent.click(button); });
      expect(writeText).toHaveBeenCalledTimes(1);
      expect(button.textContent).toBe("Copied");
      const firstIndex = setTimeout.mock.calls.findIndex((args) => args[1] === 1600);
      expect(firstIndex).toBeGreaterThanOrEqual(0);
      const firstId = setTimeout.mock.results[firstIndex].value;
      act(() => { vi.advanceTimersByTime(800); });
      await act(async () => { fireEvent.click(button); });
      const feedbackCalls = setTimeout.mock.calls
        .map((args, index) => ({ delay: args[1], id: setTimeout.mock.results[index].value }))
        .filter(({ delay }) => delay === 1600);
      expect(feedbackCalls).toHaveLength(2);
      expect(feedbackCalls[1].id).not.toBe(firstId);
      expect(clearTimeout).toHaveBeenCalledWith(firstId);
      expect(writeText).toHaveBeenCalledTimes(2);
      act(() => { vi.advanceTimersByTime(801); });
      expect(button.textContent).toBe("Copied");
      act(() => { vi.advanceTimersByTime(798); });
      expect(button.textContent).toBe("Copied");
      act(() => { vi.advanceTimersByTime(1); });
      expect(button.textContent).toBe(kind === "diagnostics" ? "Copy diagnostics" : "Copy");
    });
  }

  it("keeps diagnostics and command feedback independent after StrictMode replay", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const { container } = await renderHealth(incidentDoc(), [], true);
    const diagnostics = screen.getByRole("button", { name: "Copy diagnostics" });
    const command = within(attentionItem(container, "fleet.roll")!).getByRole("button", { name: "Copy" });
    vi.useFakeTimers();
    await act(async () => { fireEvent.click(diagnostics); });
    expect(diagnostics.textContent).toBe("Copied");
    expect(command.textContent).toBe("Copy");
    act(() => { vi.advanceTimersByTime(800); });
    await act(async () => { fireEvent.click(command); });
    expect(command.textContent).toBe("Copied");
    expect(writeText).toHaveBeenCalledTimes(2);
    act(() => { vi.advanceTimersByTime(800); });
    expect(diagnostics.textContent).toBe("Copy diagnostics");
    expect(command.textContent).toBe("Copied");
    act(() => { vi.advanceTimersByTime(800); });
    expect(command.textContent).toBe("Copy");
  });

  it("does not schedule feedback when diagnostics clipboard resolves after unmount", async () => {
    let resolveClipboard!: () => void;
    const clipboardPromise = new Promise<void>((resolve) => { resolveClipboard = resolve; });
    const writeText = vi.fn().mockReturnValue(clipboardPromise);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const doc = degradedDoc();
    const { unmount } = await renderHealth(doc);
    vi.useFakeTimers();
    const setTimeout = vi.spyOn(window, "setTimeout");
    fireEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));
    expect(writeText).toHaveBeenCalledWith(JSON.stringify(doc, null, 2));
    expect(setTimeout).not.toHaveBeenCalled();
    unmount();
    setTimeout.mockClear();
    await act(async () => {
      resolveClipboard();
      await clipboardPromise;
    });
    expect(setTimeout).not.toHaveBeenCalled();
  });
});

describe("AdminHealth — All checks inventory (M4)", () => {
  it("counts: header and per-group status, na excluded from every denominator", async () => {
    await renderHealth(healthySilentDoc());
    expect(screen.getByText("18 of 18 passing")).toBeTruthy();
    // workers (5, fleet.rundisk and fleet.quarantine included), queue (2), control (3),
    // integrations (3), housekeeping (5).
    // Exact per-string tallies: control and integrations both read "all 3 passing".
    expect(screen.getAllByText("all 5 passing").length).toBe(2);
    expect(screen.getAllByText("all 2 passing").length).toBe(1);
    expect(screen.getAllByText("all 3 passing").length).toBe(2);
    expect(screen.getAllByText(/^all \d passing$/).length).toBe(5);
  });

  it("incident: attention groups read 'K of N need attention'", async () => {
    await renderHealth(incidentDoc());
    expect(screen.getByText("14 of 18 passing")).toBeTruthy();
    expect(screen.getByText("2 of 5 need attention")).toBeTruthy(); // workers: fleet.rundisk and fleet.quarantine stay ok
    expect(screen.getByText("1 of 2 need attention")).toBeTruthy(); // queue
    expect(screen.getByText("1 of 3 need attention")).toBeTruthy(); // integrations
    expect(screen.getByText("all 3 passing")).toBeTruthy(); // control
  });

  it("groups are collapsed by default; toggling sets aria-expanded and shows summaries", async () => {
    await renderHealth(healthySilentDoc());
    for (const name of ["Workers and capacity", "Queue", "Control plane", "Integrations", "Housekeeping"]) {
      expect(groupButton(name).getAttribute("aria-expanded")).toBe("false");
    }
    expect(screen.queryByText("Database reachable (2ms); schema at head.")).toBeNull();

    const control = groupButton("Control plane");
    fireEvent.click(control);
    expect(control.getAttribute("aria-expanded")).toBe("true");
    const region = document.getElementById(control.getAttribute("aria-controls")!)!;
    expect(within(region).getByText("Database reachable (2ms); schema at head.")).toBeTruthy();
    expect(within(region).getByText("db")).toBeTruthy(); // the check id
    expect(within(region).getAllByText("OK").length).toBe(3);

    fireEvent.click(control);
    expect(control.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("Database reachable (2ms); schema at head.")).toBeNull();
  });

  it("an opened group stays open when the 10 s poll delivers a new document", async () => {
    await renderHealth(healthySilentDoc());
    fireEvent.click(groupButton("Control plane"));
    // The poll: a fresh document object (now degraded), fetched on the poller's tick
    // (usePollWhileVisible re-runs on visibilitychange as well as on its interval).
    mockApi.getAdminHealth.mockResolvedValue(degradedDoc());
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(await screen.findByRole("heading", { name: "2 checks need attention" })).toBeTruthy();
    expect(groupButton("Control plane").getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("Database reachable (2ms); schema at head.")).toBeTruthy();
    expect(groupButton("Queue").getAttribute("aria-expanded")).toBe("false");
  });

  it("an attention chip links to its item and scrolls it into view, smooth by default", async () => {
    const { container } = await renderHealth(incidentDoc());
    const chip = screen.getByRole("link", { name: "Danger Worker image roll" });
    expect(chip.getAttribute("href")).toBe("#c-fleet.roll");
    const target = attentionItem(container, "fleet.roll")!;
    target.scrollIntoView = vi.fn();
    fireEvent.click(chip);
    expect(target.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "smooth" });
    // Focus follows the jump, so the next Tab continues from the item.
    expect(document.activeElement).toBe(target);
    // The warn chip is a link too; passing checks are plain text, not links.
    expect(screen.getByRole("link", { name: "Warn CI watch capacity" }).getAttribute("href")).toBe("#c-forge.ciwatch");
    expect(screen.queryByRole("link", { name: /Database/ })).toBeNull();
  });

  it("the chip jump respects reduced motion", async () => {
    stubReducedMotion(true);
    const { container } = await renderHealth(incidentDoc());
    const target = attentionItem(container, "queue.waiting")!;
    target.scrollIntoView = vi.fn();
    fireEvent.click(screen.getByRole("link", { name: "Danger Runs waiting for a worker" }));
    expect(target.scrollIntoView).toHaveBeenCalledWith({ block: "center", behavior: "auto" });
  });

  it("na (no hosted workers): N/A chips with their word, 'not applicable' in the counts, still all clear", async () => {
    await renderHealth(noHostedWorkersDoc());
    expect(screen.getByText("All systems normal.")).toBeTruthy();
    // fleet.roll, controller.report and slack.socket are N/A (never green, never gone).
    expect(screen.getAllByText("N/A").length).toBe(3);
    expect(screen.getByText("15 of 15 passing, 3 not applicable")).toBeTruthy();
    expect(screen.getByText("all 4 passing, 1 not applicable")).toBeTruthy(); // workers (fleet.rundisk and fleet.quarantine apply)
    expect(screen.getAllByText("all 2 passing, 1 not applicable").length).toBe(2); // control, integrations
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });

  it("a group with both attention and N/A checks counts N/A out of the denominator", async () => {
    // Workers group on the no-hosted fixture: fleet.roll N/A, fleet.capacity (now danger),
    // fleet.disk, fleet.rundisk and fleet.quarantine OK. Applicable = 4, so "1 of 4", never "1 of 5".
    await renderHealth(withSeverity(noHostedWorkersDoc(), { "fleet.capacity": "danger" }));
    expect(screen.getByText("1 of 4 need attention, 1 not applicable")).toBeTruthy();
    expect(screen.queryByText(/^1 of 5 need attention/)).toBeNull();
  });

  it("a group whose checks are all N/A reads 'not applicable'", async () => {
    await renderHealth(withSeverity(noHostedWorkersDoc(), { "forge.ciwatch": "na", "forge.sync": "na" }));
    expect(screen.getByText("not applicable")).toBeTruthy();
    expect(screen.getByText("13 of 13 passing, 5 not applicable")).toBeTruthy();
  });

  it("no interactive element is nested inside another", async () => {
    const { container } = await renderHealth(incidentDoc(), incidentFleetWorkers());
    for (const button of Array.from(container.querySelectorAll("button"))) {
      expect(button.querySelector("a, button, input, select, textarea, [href]")).toBeNull();
    }
    for (const link of Array.from(container.querySelectorAll("a"))) {
      expect(link.closest("button")).toBeNull();
      expect(link.querySelector("a, button")).toBeNull();
    }
  });
});

describe("AdminHealth — untrusted strings in attributes", () => {
  it("strips a hostile worker name and blocking reason before they reach a title= attribute", async () => {
    // Control chars are BUILT at runtime from char codes, never written as raw bytes or
    // \u escapes in this source (a raw NUL makes the file binary; the sourceBytes gate
    // forbids it). RLO is a RIGHT-TO-LEFT OVERRIDE (U+202E), NUL is U+0000, BEL is U+0007 —
    // all of which stripUnsafeChars removes. React escapes the visible text; the title is
    // the sink a rendered-text sweep would miss, so it must be stripped too (issue #124).
    const RLO = String.fromCharCode(0x202e);
    const NUL = String.fromCharCode(0);
    const BEL = String.fromCharCode(7);
    const hostile: AdminWorker = {
      ...incidentFleetWorkers()[0],
      id: "w-hostile",
      name: `${RLO}evil${NUL}name`,
      upgrade_status: "upgrade_failed",
      upgrade_blocking_container: `work${NUL}er`,
      upgrade_blocking_reason: `Pull${RLO}BackOff${BEL}`,
      owner_email: "user.a@uzi.local",
    };
    await renderHealth(incidentDoc(), [hostile]);

    // The worker-name cell's title carries the STRIPPED name (no U+202E, no NUL). Awaited
    // because the fleet table loads on its own fetch, after the health document. toBe with
    // the fully-stripped value is strictly stronger than a "does not contain" check.
    const nameCell = await screen.findByTitle("evilname");
    expect(nameCell.getAttribute("title")).toBe("evilname");

    // The blocking cell's title is stripped too (the hostile reason matches no k8s enum, so
    // likelyCause yields null and the title falls back to the stripped container:reason).
    const blockingCell = screen.getByTitle("worker: PullBackOff");
    expect(blockingCell.getAttribute("title")).toBe("worker: PullBackOff");
  });
});

// ── Fleet card (M5) ──────────────────────────────────────────────────────────────────

// One worker per upgrade status, varied status/version, built from the incident fixture.
function fleetWorker(over: Partial<AdminWorker>): AdminWorker {
  return {
    ...incidentFleetWorkers()[0],
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    ...over,
  };
}

function fleetCard(): HTMLElement {
  const el = document.getElementById("fleet");
  if (!el) throw new Error("no Fleet card");
  return el;
}

// The row of the worker whose (stripped) name title is `name`.
async function fleetRow(name: string): Promise<HTMLElement> {
  const cell = await within(fleetCard()).findByTitle(name);
  const tr = cell.closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

describe("AdminHealth — Fleet card (M5)", () => {
  it.each([
    { name: "DinD-only sample", pending: true, sample: true, ephemeral: false },
    { name: "operation without a sample", pending: true, sample: false, ephemeral: false },
    { name: "ephemeral pressure report", pending: true, sample: true, ephemeral: true },
    { name: "clear worker", pending: false, sample: true, ephemeral: false },
    { name: "clear worker without a sample", pending: false, sample: false, ephemeral: false },
  ])("disk cleanup: $name", async ({ pending, sample, ephemeral }) => {
    await renderHealth(healthySilentDoc(), [fleetWorker({
      name: "disk-worker", ephemeral, cleanup_pending: pending,
      disk_pressure_volumes: pending && sample ? ["dind"] : [],
      stats_disk_dind_bytes: sample ? 1024 : null,
      stats_disk_dind_total_bytes: sample ? 2048 : null,
      stats_disk_dind_inodes: sample ? 100 : null,
      stats_disk_dind_total_inodes: sample ? 100 : null,
    })]);
    const row = await fleetRow("disk-worker");
    const badge = within(row).queryByText("cleanup pending");
    expect(Boolean(badge)).toBe(pending);
    if (pending) expect(badge?.getAttribute("title")).toBe("Docker disk cleanup is pending. Existing runs may finish before cleanup; some workers report pressure only.");
    expect(Boolean(within(row).queryByText(/disk 1\/2 KiB \(inodes 100%\)/))).toBe(sample);
    expect(within(row).queryByText(/cpu .*mem/)).toBeNull();
  });

  it("own card with a sentence-case header and the eight columns, no Kind or Since", async () => {
    await renderHealth(healthySilentDoc(), [fleetWorker({ id: "w-1", name: "base.l-aaaa" })]);
    const card = fleetCard();
    expect(within(card).getByRole("heading", { name: "Fleet, all users" })).toBeTruthy();
    expect(within(card).getByText("Worker status and upgrade blockers across every user")).toBeTruthy();
    expect(screen.queryByText(/Upgrade and Blocking read/)).toBeNull();
    const headers = within(card)
      .getAllByRole("columnheader")
      .map((th) => th.textContent);
    expect(headers).toEqual(["Owner", "Worker", "Status", "Disk", "Version", "Upgrade", "Blocking", "Last seen"]);
    expect(within(card).queryByRole("columnheader", { name: "Kind" })).toBeNull();
    expect(within(card).queryByRole("columnheader", { name: "Since" })).toBeNull();
  });

  it("status and upgrade badge words, kind on the worker cell, version dash, blocking cause", async () => {
    const RLO = String.fromCharCode(0x202e);
    const BEL = String.fromCharCode(7);
    await renderHealth(healthySilentDoc(), [
      fleetWorker({ id: "w-1", name: "w-ok", status: "online", version: "0.84.0", upgrade_status: "up_to_date" }),
      fleetWorker({
        id: "w-2",
        name: "w-old",
        status: "offline",
        version: null,
        upgrade_status: "outdated",
        upgrade_detail: `running 0.83.1,${RLO} target 0.84.0${BEL}`,
      }),
      fleetWorker({ id: "w-3", name: "w-rolling", status: "online", upgrade_status: "upgrading" }),
      fleetWorker({
        id: "w-4",
        name: "w-stuck",
        status: "offline",
        upgrade_status: "upgrade_failed",
        upgrade_detail: "worker: ImagePullBackOff",
        upgrade_blocking_container: "worker",
        upgrade_blocking_reason: "ImagePullBackOff",
      }),
      fleetWorker({ id: "w-5", name: "w-unstamped", status: "online", upgrade_status: "unknown" }),
    ]);

    const ok = await fleetRow("w-ok");
    expect(within(ok).getByText("online")).toBeTruthy();
    expect(within(ok).getByText("up to date")).toBeTruthy();
    expect(within(ok).getByText("0.84.0")).toBeTruthy();
    expect(within(ok).getByText("hosted")).toBeTruthy(); // kind, on the worker cell
    expect(within(ok).getByText("none")).toBeTruthy(); // nothing blocking

    const old = await fleetRow("w-old");
    expect(within(old).getByText("offline")).toBeTruthy();
    // The upgrade badge's title is the STRIPPED detail (no RLO, no BEL).
    expect(within(old).getByText("outdated").getAttribute("title")).toBe("running 0.83.1, target 0.84.0");
    expect(within(old).getAllByText("—").length).toBe(1); // version null

    const rolling = await fleetRow("w-rolling");
    expect(within(rolling).getByText("upgrading").hasAttribute("title")).toBe(false);

    const stuck = await fleetRow("w-stuck");
    expect(within(stuck).getByText("upgrade failed").getAttribute("title")).toBe("worker: ImagePullBackOff");
    // Exactly one blocking text, titled with the human cause.
    const blocking = within(stuck).getByText("worker: ImagePullBackOff");
    expect(blocking.getAttribute("title")).toBe(likelyCause("worker", "ImagePullBackOff", null)!);

    // unknown renders no badge word at all, only a dash.
    const unstamped = await fleetRow("w-unstamped");
    expect(within(unstamped).queryByText(/up to date|outdated|upgrading|upgrade failed|unknown/)).toBeNull();
    expect(within(unstamped).getByText("—")).toBeTruthy();
  });

  it("skeleton rows and aria-busy while the first fetch is pending, then the rows", async () => {
    let resolve!: (v: { workers: AdminWorker[] }) => void;
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    mockApi.adminListWorkers.mockReturnValue(new Promise((r) => (resolve = r)));
    render(
      <MemoryRouter initialEntries={["/admin/health"]}>
        <AdminHealth />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Copy diagnostics")).toBeTruthy());
    const table = within(fleetCard()).getByRole("table");
    expect(table.getAttribute("aria-busy")).toBe("true");
    const body = table.querySelector("tbody")!;
    expect(body.querySelectorAll("tr").length).toBe(3);
    expect(body.querySelectorAll("td").length).toBe(24);
    expect(body.textContent).toBe("");
    // The retired visible copy (with its ellipsis) is gone; a visually hidden caption without
    // the ellipsis is what a screen reader announces instead. Exact-string matches, so the
    // negative cannot match the positive.
    expect(screen.queryByText("Loading fleet\u2026")).toBeNull();
    const caption = within(table).getByText("Loading fleet");
    expect(caption.tagName).toBe("CAPTION");
    expect(caption.classList.contains("sr-only")).toBe(true);

    await act(async () => resolve({ workers: [fleetWorker({ id: "w-1", name: "w-loaded" })] }));
    expect(await within(fleetCard()).findByTitle("w-loaded")).toBeTruthy();
    expect(table.getAttribute("aria-busy")).toBe("false");
    expect(body.querySelectorAll("tr").length).toBe(1);
    expect(within(table).queryByText("Loading fleet")).toBeNull();
  });

  it("empty fleet keeps its text", async () => {
    await renderHealth(healthySilentDoc(), []);
    expect((await within(fleetCard()).findByText("No workers across any user.")).getAttribute("colspan")).toBe("8");
    expect(within(fleetCard()).getByRole("table").getAttribute("aria-busy")).toBe("false");
  });

  it("keeps the last-good rows when a poll fails", async () => {
    await renderHealth(healthySilentDoc(), [fleetWorker({ id: "w-1", name: "w-kept" })]);
    await fleetRow("w-kept");
    const calls = mockApi.adminListWorkers.mock.calls.length;
    mockApi.adminListWorkers.mockRejectedValue(new Error("blip"));
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(mockApi.adminListWorkers.mock.calls.length).toBeGreaterThan(calls));
    expect(within(fleetCard()).getByTitle("w-kept")).toBeTruthy();
    expect(within(fleetCard()).queryByText("No workers across any user.")).toBeNull();
  });
});
