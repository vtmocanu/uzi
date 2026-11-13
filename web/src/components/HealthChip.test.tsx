// @vitest-environment jsdom
// Issue #4166: the health chip is a link while everything passes and a popover trigger while
// anything needs attention. The shared health hook is mocked with a mutable document so a
// rerender stands in for the provider's next poll.
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

  it("closes and focuses the link when the last attention item clears while open", () => {
    const view = mount(overviewNextDoc());
    fireEvent.click(chip());
    dialog();
    refresh(view, healthySilentDoc());
    expect(screen.queryByRole("dialog")).toBeNull();
    const link = screen.getByRole("link", { name: /^System health:/ });
    expect(link.getAttribute("href")).toBe("/admin/health");
    expect(document.activeElement).toBe(link);
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
    expect(within(dialog()).queryByText("Paused schedules")).toBeNull();
    refresh(view, degradedDoc());
    const d = dialog();
    expect(within(d).getByText("CI watch capacity")).toBeTruthy();
    expect(within(d).getByText("Paused schedules")).toBeTruthy();
    expect(within(d).queryByText("Recovery storage")).toBeNull();
  });

  it("lists every attention check while the Overview card caps at three", () => {
    const doc = healthySilentDoc();
    const ids = ["fleet.roll", "fleet.capacity", "queue.waiting", "forge.ciwatch", "schedules.paused"];
    for (const c of doc.checks) if (ids.includes(c.id)) c.severity = "warn";
    doc.status = "warn";
    doc.counts = { ...doc.counts, ok: doc.counts.ok - 5, warn: 5 };
    mount(doc);
    fireEvent.click(chip());
    expect(within(dialog()).getAllByRole("listitem")).toHaveLength(5);
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
  it("is a plain link with no popover semantics", () => {
    mount(healthySilentDoc());
    const link = screen.getByRole("link", { name: /^System health:/ });
    expect(link.getAttribute("href")).toBe("/admin/health");
    expect(link.hasAttribute("aria-expanded")).toBe(false);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("renders nothing without a document", () => {
    const { container } = mount(null);
    expect(container.firstChild).toBeNull();
  });
});
