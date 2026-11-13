// @vitest-environment jsdom
// Issues #4166 and #4281: the health chip always opens the popover. The shared health hook is
// mocked with a mutable document so a rerender stands in for the provider's next poll.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { HealthChip } from "./HealthChip";
import { HealthOverviewCard } from "./HealthOverviewCard";
import type { HealthDoc } from "../lib/api";
import { useHealthStatus } from "../lib/useAdminHealth";
import {
  degradedDoc,
  healthySilentDoc,
  overviewNextDoc,
  ownerOnlyDoc,
  ownerOnlyWaitingOwnerId,
  ownerOnlyWaitingRunId,
} from "../mocks/data/health";

vi.mock("../lib/useAdminHealth", () => ({ useHealthStatus: vi.fn() }));

let current: HealthDoc | null = null;

beforeEach(() => {
  vi.mocked(useHealthStatus).mockImplementation(() => ({ doc: current, attentionCount: 0 }));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  current = null;
  vi.clearAllMocks();
});

function mount(doc: HealthDoc | null) {
  current = doc;
  return render(
    <MemoryRouter>
      <HealthChip />
    </MemoryRouter>,
  );
}

function refresh(view: ReturnType<typeof mount>, doc: HealthDoc | null) {
  current = doc;
  view.rerender(
    <MemoryRouter>
      <HealthChip />
    </MemoryRouter>,
  );
}

const chip = () => screen.getByRole("button", { name: /^System health:/ });
const dialog = () => screen.getByRole("dialog", { name: "System health" });
const frame = () => act(() => new Promise<void>((r) => requestAnimationFrame(() => r())));

describe("HealthChip popover", () => {
  it("opens a dialog with the verdict, structured rows, since and Open health", () => {
    const doc = ownerOnlyDoc();
    expect(doc.blocking).toBe(false);
    const queue = doc.checks.find((c) => c.id === "queue.waiting")!;
    queue.evidence = [
      Object.assign(
        { label: "Waiting run", value: "Owner: Alex Example (22930000); waited 48m." },
        { run_id: ownerOnlyWaitingRunId, owner_id: ownerOnlyWaitingOwnerId },
      ),
    ];
    mount(doc);
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(chip());
    const d = dialog();
    expect(chip().getAttribute("aria-expanded")).toBe("true");
    expect(chip().getAttribute("aria-controls")).toBe(d.id);
    expect(within(d).getByText("System health: 4 checks need attention; no instance-wide blocker detected")).toBeTruthy();
    expect(within(d).getByText(queue.title)).toBeTruthy();
    expect(within(d).getByText(queue.summary)).toBeTruthy();
    const link = within(d).getByRole("link", { name: "Run 22930000" });
    expect(link.getAttribute("href")).toBe(`/runs/${ownerOnlyWaitingRunId}`);
    expect(d.textContent).toMatch(/for \d+m/);
    expect(within(d).getByRole("link", { name: "Open health" }).getAttribute("href")).toBe("/admin/health");
  });

  it("closes on Escape and returns focus to the chip", () => {
    mount(overviewNextDoc());
    fireEvent.click(chip());
    dialog();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(chip());
  });

  it("keeps focus on an outside control that was clicked", async () => {
    render(<button type="button">elsewhere</button>);
    mount(overviewNextDoc());
    fireEvent.click(chip());
    const other = screen.getByRole("button", { name: "elsewhere" });
    other.focus();
    fireEvent.mouseDown(other);
    expect(screen.queryByRole("dialog")).toBeNull();
    await frame();
    expect(document.activeElement).toBe(other);
  });

  it("returns focus to the chip when the click lands on nothing", async () => {
    mount(overviewNextDoc());
    fireEvent.click(chip());
    (document.activeElement as HTMLElement | null)?.blur();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
    await frame();
    expect(document.activeElement).toBe(chip());
  });

  it("keeps the panel open when the last attention item clears", () => {
    const view = mount(overviewNextDoc());
    fireEvent.click(chip());
    dialog();
    refresh(view, healthySilentDoc());
    expect(within(dialog()).getByText("System health: all 23 checks passing")).toBeTruthy();
    expect(chip().getAttribute("aria-expanded")).toBe("true");
    expect(screen.queryByRole("link", { name: /^System health:/ })).toBeNull();
  });

  it("does not steal focus when attention clears while closed", () => {
    const view = mount(overviewNextDoc());
    const other = document.createElement("button");
    document.body.appendChild(other);
    other.focus();
    refresh(view, healthySilentDoc());
    expect(document.activeElement).toBe(other);
    other.remove();
  });

  it("re-renders the open panel from a refreshed document", () => {
    const view = mount(overviewNextDoc());
    fireEvent.click(chip());
    expect(within(dialog().querySelector<HTMLElement>("[data-health-attention-list]")!).queryByText("Paused schedules")).toBeNull();
    refresh(view, degradedDoc());
    const attention = dialog().querySelector<HTMLElement>("[data-health-attention-list]")!;
    expect(within(attention).getByText("CI watch capacity")).toBeTruthy();
    expect(within(attention).getByText("Paused schedules")).toBeTruthy();
    expect(within(attention).queryByText("Recovery storage")).toBeNull();
  });

  it("shows a collapsed passing-check disclosure beside attention, and omits it at zero", () => {
    const doc = overviewNextDoc();
    const view = mount(doc);
    fireEvent.click(chip());
    const summary = within(dialog()).getByText("22 other checks passing");
    const disclosure = summary.closest("details")!;
    expect(disclosure.open).toBe(false);
    fireEvent.click(summary);
    expect(disclosure.open).toBe(true);
    expect(disclosure.querySelectorAll("[data-passing-check]")).toHaveLength(22);
    expect(within(dialog()).getByText(doc.checks.find((c) => c.severity === "warn")!.title)).toBeTruthy();

    const allWarn = healthySilentDoc();
    for (const check of allWarn.checks) check.severity = "warn";
    allWarn.counts = { ...allWarn.counts, ok: 0, warn: allWarn.checks.length };
    allWarn.status = "warn";
    refresh(view, allWarn);
    expect(dialog().querySelector("details")).toBeNull();
  });

  it("keeps focus inside the open dialog when a live transition removes its focused item", () => {
    const view = mount(overviewNextDoc());
    fireEvent.click(chip());
    const summary = within(dialog()).getByText("22 other checks passing");
    (summary as HTMLElement).focus();
    refresh(view, healthySilentDoc());
    expect(document.activeElement).toBe(dialog());
    const check = dialog().querySelector<HTMLElement>("[data-passing-check]")!;
    // The names are deliberately outside the tab order. Simulate a programmatic focus on a
    // soon-to-be-removed item to keep the defensive live-transition focus path covered.
    check.tabIndex = -1;
    check.focus();
    refresh(view, overviewNextDoc());
    expect(document.activeElement).toBe(dialog());
  });

  it("does not steal outside focus on a live pass/warn transition", () => {
    render(<button type="button">elsewhere</button>);
    const view = mount(overviewNextDoc());
    fireEvent.click(chip());
    const other = screen.getByRole("button", { name: "elsewhere" });
    other.focus();
    refresh(view, healthySilentDoc());
    expect(document.activeElement).toBe(other);
    expect(within(dialog()).getByText("System health: all 23 checks passing")).toBeTruthy();
  });

  it("lists every attention check while the Overview card caps at three", () => {
    const doc = healthySilentDoc();
    const ids = ["fleet.roll", "fleet.capacity", "queue.waiting", "forge.ciwatch", "schedules.paused"];
    for (const c of doc.checks) if (ids.includes(c.id)) c.severity = "warn";
    doc.status = "warn";
    doc.counts = { ...doc.counts, ok: doc.counts.ok - 5, warn: 5 };
    mount(doc);
    fireEvent.click(chip());
    expect(within(dialog().querySelector<HTMLElement>("[data-health-attention-list]")!).getAllByRole("listitem")).toHaveLength(5);
    cleanup();
    current = doc;
    render(
      <MemoryRouter>
        <HealthOverviewCard />
      </MemoryRouter>,
    );
    const card = screen.getByRole("status", { name: "System health" });
    expect(within(card).getAllByRole("listitem")).toHaveLength(3);
    expect(within(card).getByText("and 2 more")).toBeTruthy();
  });

  it("lays out panel evidence in two columns, widening values over 40 characters only there", () => {
    const doc = ownerOnlyDoc();
    const check = doc.checks.find((c) => c.id === "queue.waiting")!;
    check.doc = "worker-capacity";
    check.evidence = [
      { label: "Short", value: "x".repeat(40) },
      { label: "Long image", value: "y".repeat(41) },
    ];
    mount(doc);
    fireEvent.click(chip());
    const row = within(dialog()).getByText(check.title).closest("li")!;
    const grid = row.querySelector("[data-health-evidence-grid]");
    expect(grid).not.toBeNull();
    expect(grid!.className).toContain("grid-cols-2");
    const entries = grid!.querySelectorAll("[data-health-evidence-entry]");
    expect(entries).toHaveLength(3); // two evidence values and the docs link
    expect(entries[0].className).not.toContain("col-span-2");
    expect(entries[1].className).toContain("col-span-2");
    expect(entries[1].textContent).toContain("y".repeat(41));

    cleanup();
    current = doc;
    render(<MemoryRouter><HealthOverviewCard /></MemoryRouter>);
    const card = screen.getByRole("alert", { name: "System health" });
    expect(card.querySelector("[data-health-evidence-grid]")).toBeNull();
    expect(within(card).getByText((_, el) => el?.tagName === "SPAN" && el.textContent === `Long image: ${"y".repeat(41)}`)).toBeTruthy();
  });

  it("keeps the chip-attached fallback width and mobile bottom-sheet classes", () => {
    mount(overviewNextDoc());
    fireEvent.click(chip());
    const cls = dialog().className;
    for (const c of [
      "w-[min(40rem,calc(100vw-32px))]",
      "max-sm:fixed",
      "max-sm:inset-x-4",
      "max-sm:bottom-4",
      "max-sm:w-auto",
      "max-sm:max-h-[calc(100dvh-2rem)]",
    ]) {
      expect(cls).toContain(c);
    }
  });
});

describe("HealthChip when nothing needs attention", () => {
  it("opens a passing panel with every applicable check and the Open health link", () => {
    const doc = healthySilentDoc();
    mount(doc);
    const trigger = chip();
    expect(trigger.getAttribute("aria-haspopup")).toBe("dialog");
    fireEvent.click(trigger);
    const panel = dialog();
    expect(within(panel).getByText("System health: all 23 checks passing")).toBeTruthy();
    expect(panel.querySelectorAll("[data-passing-check]")).toHaveLength(doc.counts.ok);
    expect(within(panel).getByRole("link", { name: "Open health" }).getAttribute("href")).toBe("/admin/health");
    expect(within(panel).getByRole("region", { name: "Workers" })).toBeTruthy();
    expect(within(panel).getByRole("region", { name: "Releases and pricing" })).toBeTruthy();
    expect(within(panel).queryByRole("region", { name: "Other" })).toBeNull();
  });

  it("omits na checks, names their count, and groups a future check under Other", () => {
    const doc = healthySilentDoc();
    doc.checks[0].severity = "na";
    doc.checks.push({ ...doc.checks[1], id: "future.check", title: "Future check" });
    doc.counts = { ...doc.counts, na: 1 };
    mount(doc);
    fireEvent.click(chip());
    const panel = dialog();
    expect(panel.querySelectorAll("[data-passing-check]")).toHaveLength(23);
    expect(within(panel).queryByText("Worker image roll")).toBeNull();
    expect(within(panel).getByText("1 not applicable")).toBeTruthy();
    expect(within(within(panel).getByRole("region", { name: "Other" })).getByText("Future check")).toBeTruthy();
  });

  it("ages the checked time without a new health document and omits an invalid time", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-11-13T12:00:05Z"));
    const doc = healthySilentDoc();
    doc.checked_at = "2026-11-13T12:00:00Z";
    const view = mount(doc);
    fireEvent.click(chip());
    expect(dialog().textContent).toContain("checked 5s ago");
    act(() => vi.advanceTimersByTime(10_000));
    expect(dialog().textContent).toContain("checked 15s ago");
    doc.checked_at = "invalid";
    refresh(view, { ...doc });
    expect(dialog().textContent).not.toContain("checked");
  });

  it("strips bidi and control characters from the actual summary title attribute", () => {
    const doc = healthySilentDoc();
    doc.checks[0].summary = "safe\u202E\u0001 summary";
    mount(doc);
    fireEvent.click(chip());
    const item = within(dialog()).getByText(doc.checks[0].title);
    expect(item.getAttribute("title")).toBe("safe summary");
    expect(item.hasAttribute("tabindex")).toBe(false);
    const srSummary = item.closest("li")!.querySelector(".sr-only")!;
    expect(srSummary.textContent).toBe(": safe summary");
    expect(item.getAttribute("title")).not.toContain("\u202E");
    expect(item.getAttribute("title")).not.toContain("\u0001");
  });

  it("renders nothing without a document", () => {
    const { container } = mount(null);
    expect(container.firstChild).toBeNull();
  });
});
