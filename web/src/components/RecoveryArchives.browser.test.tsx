import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { RecoveryArchivesPanel } from "./RecoveryArchives";
import { api } from "../lib/api";
import type { RecoveryArchiveSummary, Run } from "../lib/apiTypes";
import cases from "../../../fixtures/recovery-prerequisite-closure/cases.json";

vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, getRunArchives: vi.fn(), discardRunArchive: vi.fn() } };
});
const originalTheme = document.documentElement.getAttribute("data-theme");
afterEach(() => {
  cleanup(); vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});
const run = { id: "00000000-0000-0000-0000-000000000002", status: "completed", landing_state: "needs_landing" } as Run;
const matrix = [1100, 375].flatMap(width => ["dawn", "ember"].map(theme => ({ width, theme })));
it.each(matrix)("recovery pin layout and keyboard controls at $width in $theme", async ({ width, theme }) => {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  vi.mocked(api.getRunArchives).mockResolvedValue({ supported: true, legacy: false, has_open_hold: false,
    counts: { available: 1 }, archives: [{ id: "00000000-0000-0000-0000-000000000001", run_id: run.id,
      hold_id: "hold", state: "available", source_sha: cases.pin_tip, prerequisite_shas: [cases.pin_tip],
      prerequisite_closure: cases.cases[3].expected.closure!, created_at: cases.updated_at,
      expires_at: "2099-01-01T00:00:00Z" }],
  } as RecoveryArchiveSummary);
  const { container } = render(<main className="p-4"><RecoveryArchivesPanel run={run} /></main>);
  const download = await screen.findByRole("link", { name: "Export archive" });
  const pin = screen.getByLabelText("Prerequisite protection");
  expect(within(pin).getByText(cases.pin_ref).textContent).toBe(cases.pin_ref);
  expect(within(pin).getByText(cases.pin_tip).textContent).toBe(cases.pin_tip);
  expect(screen.getByText(/Archive expires in:/)).toBeTruthy();
  expect(within(pin).getByText(/Pin record updated:/)).toBeTruthy();
  const guide = within(pin).getByRole("link", { name: "Prerequisite pin restore guide" });
  expect(guide.getAttribute("href")).toContain("docs/run-recovery.md#prerequisite-pins");
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  expect(document.body.scrollWidth).toBeLessThanOrEqual(width);
  for (const element of container.querySelectorAll("li, code, [aria-label='Prerequisite protection']")) {
    expect(element.getBoundingClientRect().right).toBeLessThanOrEqual(width);
  }
  await userEvent.tab();
  expect(document.activeElement).toBe(download);
  expect(download.matches(":focus-visible")).toBe(true);
  expect(getComputedStyle(download).boxShadow).not.toBe("none");
  await userEvent.tab();
  const remove = screen.getByRole("button", { name: "Delete archive" });
  expect(document.activeElement).toBe(remove);
  expect(remove.matches(":focus-visible")).toBe(true);
  await userEvent.type(page.elementLocator(remove), "{Enter}", { skipClick: true });
  const confirmation = screen.getByRole("group", { name: "Delete this recovery archive" });
  await waitFor(() => expect(document.activeElement).toBe(confirmation));
  const description = confirmation.getAttribute("aria-describedby");
  expect(document.getElementById(description!)?.textContent).toContain("only remaining copy");
  await userEvent.type(page.elementLocator(confirmation), "{Escape}", { skipClick: true });
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Delete archive" })));
  expect(api.discardRunArchive).not.toHaveBeenCalled();
  // Continue through the copy control to the guide in the native tab order.
  await userEvent.tab();
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Copy" }));
  await userEvent.tab();
  expect(document.activeElement).toBe(guide);
  expect(guide.matches(":focus-visible")).toBe(true);
  expect(getComputedStyle(guide).boxShadow).not.toBe("none");
});
