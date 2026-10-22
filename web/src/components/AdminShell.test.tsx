// @vitest-environment jsdom
//
// PRD #1648 D9: the Admin > Health tab carries the shared count pill with a worded label
// naming every non-zero severity. Asserted by accessible name and visible count, never by
// a colour class. AdminShell is mounted standalone, so useHealthStatus self-fetches.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AdminShell } from "./AdminShell";
import { api, type HealthDoc } from "../lib/api";
import { healthySilentDoc, incidentDoc, unknownDoc, ownerOnlyDoc } from "../mocks/data/health";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn() } };
});
const mockApi = vi.mocked(api);
const originalScrollIntoView = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollIntoView");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
  vi.restoreAllMocks();
  if (originalScrollIntoView) Object.defineProperty(HTMLElement.prototype, "scrollIntoView", originalScrollIntoView);
  else Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
});

async function renderShell(doc: HealthDoc) {
  mockApi.getAdminHealth.mockResolvedValue(doc);
  render(
    <MemoryRouter initialEntries={["/admin/users"]}>
      <AdminShell description="d">
        <div>body</div>
      </AdminShell>
    </MemoryRouter>,
  );
  await waitFor(() => expect(mockApi.getAdminHealth).toHaveBeenCalled());
}

describe("AdminShell agent source pip", () => {
  function sourceDoc(severity: string | null) {
    const doc = healthySilentDoc();
    doc.checks = doc.checks.flatMap((c) => c.id === "agentsource.update"
      ? severity === null ? [] : [{ ...c, severity }]
      : [c]);
    return doc;
  }

  it.each(["warn", "unknown"])("renders a worded %s shape on Instance only", async (severity) => {
    await renderShell(sourceDoc(severity));
    const label = severity === "warn" ? "Agent source update available" : "Agent source update status unavailable";
    const pip = await screen.findByRole("img", { name: label });
    expect(screen.getByRole("link", { name: new RegExp("Instance") }).contains(pip)).toBe(true);
    expect(pip.querySelector(severity === "warn" ? "polygon" : 'circle[fill="none"]')).toBeTruthy();
    expect(pip.classList.contains(severity === "warn" ? "text-warn" : "text-muted")).toBe(true);
  });

  it.each(["ok", "na", "danger", null])("hides the dedicated pip for %s", async (severity) => {
    await renderShell(sourceDoc(severity));
    expect(within(screen.getByRole("link", { name: "Instance" })).queryByRole("img")).toBeNull();
  });

  it("scrolls when dedicated severity or presence changes with aggregate attention unchanged", async () => {
    vi.useFakeTimers();
    const scroll = vi.fn();
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", { value: scroll, configurable: true });
    const warn = sourceDoc("warn");
    const unknown = sourceDoc("unknown");
    const absent = sourceDoc(null);
    // A different check keeps the aggregate count/tone constant across the swap.
    for (const doc of [warn, unknown, absent]) doc.counts = { ok: 20, warn: 1, danger: 1, unknown: 0, na: 0 };
    mockApi.getAdminHealth.mockResolvedValueOnce(warn).mockResolvedValueOnce(unknown).mockResolvedValueOnce(absent);
    render(<MemoryRouter initialEntries={["/admin/settings"]}><AdminShell description={null}>body</AdminShell></MemoryRouter>);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("img", { name: "Agent source update available" })).toBeTruthy();
    scroll.mockClear();
    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(screen.getByRole("img", { name: "Agent source update status unavailable" })).toBeTruthy();
    expect(scroll).toHaveBeenCalled();
    scroll.mockClear();
    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(within(screen.getByRole("link", { name: "Instance" })).queryByRole("img")).toBeNull();
    expect(scroll).toHaveBeenCalled();
  });
});

describe("AdminShell health tab pip", () => {
  it("names danger and warning separately on an incident, with the attention total visible", async () => {
    await renderShell(incidentDoc()); // 3 danger + 1 warn
    const pip = await screen.findByLabelText("4 health checks need attention: 3 danger, 1 warning");
    expect(within(pip).getByText("4")).toBeTruthy();
    // The pip sits inside the Health tab link.
    expect(screen.getByRole("link", { name: /Health/ }).contains(pip)).toBe(true);
  });

  it("retains owner danger in the attention pip", async () => {
    await renderShell(ownerOnlyDoc());
    const pip = await screen.findByLabelText("3 health checks need attention: 2 danger, 1 warning");
    expect(within(pip).getByText("3")).toBeTruthy();
  });

  it("names unknown as unknown rather than folding it into warning", async () => {
    await renderShell(unknownDoc()); // 4 unknown
    expect(await screen.findByLabelText("4 health checks need attention: 4 unknown")).toBeTruthy();
    expect(screen.queryByLabelText(/warning/)).toBeNull();
  });

  it("renders no pip when nothing needs attention", async () => {
    await renderShell(healthySilentDoc());
    await screen.findByRole("link", { name: "Health" });
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });
});
