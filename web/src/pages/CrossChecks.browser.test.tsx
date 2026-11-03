import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunsLayout, RunsList } from "./RunsList";
import { api } from "../lib/api";
import { crossChecksApi } from "../mocks/mockApi/crossChecks";
import full from "../../../fixtures/api-contract/cross_check_history.full.json";
import type { CrossCheckHistoryGroupDTO as Group } from "../lib/apiTypes";

vi.mock("../mocks/mockApi/shared", async original => ({
  ...await original<typeof import("../mocks/mockApi/shared")>(),
  requireSession: () => ({ id: "browser-owner" }),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: () => "cross-check-history",
}));

vi.mock("../lib/api", async original => ({
  ...await original<typeof import("../lib/api")>(),
  api: {
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    crossCheckHistoryAvailability: vi.fn().mockResolvedValue({ available: true }),
    listCrossCheckHistory: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: { id: "browser-owner", is_admin: false } }) }));
const title = "Retained\u001b[31m\u202e\nwork\t" + "long-title".repeat(24);
const safeTitle = "Retained work " + "long-title".repeat(24);
function group(older = true): Group {
  const base = full.groups[0];
  return {
    ...base, parent_title: title, parent_id: "00000000-0000-4000-8000-000000000001", parent_status: "completed",
    active: true, total_attempts: 12, matching_attempts: 12, active_attempts: 3, active_hidden: 1,
    active_context: "An older active attempt is outside the loaded slice.", refresh_needed: false,
    older_cursor: older ? "older" : undefined,
    attempts: ["plan", "code"].map(stage => ({
      ...base.attempts[0], stage, round: 2, execution: "running", review: "none",
      historical: false, reason: title, effect: title, next_action: title, model: title,
      child_id: "00000000-0000-4000-8000-000000000002", child_status: "running",
    })),
  };
}
beforeEach(() => {
  localStorage.clear();
  vi.mocked(api.crossCheckHistoryAvailability).mockResolvedValue({ available: true });
  vi.mocked(api.listCrossCheckHistory).mockResolvedValue({ groups: [group()] });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); document.documentElement.removeAttribute("data-theme"); });

it.each([
  ["ANSI escape", "Safe\u001b[31mvalue", "Safevalue"],
  ["bare ESC", "Safevalue\u001b", "Safevalue"],
  ["U202E", "Safe\u202evalue", "Safevalue"],
  ["newline", "Safe\nvalue", "Safe value"],
])("sanitizes %s independently in text and attributes", async (_name, hostile, safe) => {
  vi.mocked(api.listCrossCheckHistory).mockResolvedValue({ groups: [{
    ...group(false), parent_title: hostile,
    attempts: [{ ...group(false).attempts[0], reason: hostile, effect: hostile, next_action: hostile, model: hostile }],
  }] });
  render(<MemoryRouter initialEntries={["/runs?scope=cross-checks"]}>
    <Routes><Route element={<RunsLayout />}><Route path="/runs" element={<RunsList />} /></Route></Routes>
  </MemoryRouter>);
  const article = await screen.findByRole("article");
  const heading = within(article).getByRole("heading", { level: 2 });
  expect(heading.textContent).toBe(safe);
  expect(heading.getAttribute("title")).toBe(safe);
  expect(article.getAttribute("aria-label")).toBe(safe);
  await userEvent.click(within(article).getByRole("button", { name: "Show attempt details" }));
  expect(within(article).getByText("Cause: " + safe).textContent).toBe("Cause: " + safe);
  expect(within(article).getByText("Next action: " + safe).textContent).toBe("Next action: " + safe);
  expect(within(article).getByText(safe, { selector: "dd" }).textContent).toBe(safe);
  expect(article.textContent).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  for (const node of [article, ...article.querySelectorAll("*")]) {
    for (const attribute of node.attributes) expect(attribute.value).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  }
});

it("keeps the batch control focused after all visible continuations finish", async () => {
  const groups = [group(), { ...group(), parent_id: "00000000-0000-4000-8000-000000000003", older_cursor: "other" }];
  vi.mocked(api.listCrossCheckHistory).mockResolvedValueOnce({ groups }).mockResolvedValueOnce({
    groups: groups.map(g => ({ ...g, older_cursor: undefined })),
  });
  render(<MemoryRouter initialEntries={["/runs?scope=cross-checks"]}>
    <Routes><Route element={<RunsLayout />}><Route path="/runs" element={<RunsList />} /></Route></Routes>
  </MemoryRouter>);
  const batch = await screen.findByRole("button", { name: "Load older attempts for visible groups" });
  await userEvent.click(batch);
  await screen.findByText("All matching attempts loaded for visible groups.");
  expect(document.activeElement).toBe(batch);
  expect(batch.getAttribute("aria-disabled")).toBe("true");
  expect(api.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "active", stage: "all", attempt_cursor: ["older", "other"] });
});

it("keeps the selected older button and exact focus across a visible newest-window refresh", async () => {
  const newest = { ...group(), parent_title: "Refresh work", total_attempts: 20, matching_attempts: 20,
    attempts: Array.from({ length: 10 }, (_, i) => ({ ...group().attempts[0], round: i + 11 })) };
  vi.mocked(api.listCrossCheckHistory).mockResolvedValueOnce({ groups: [newest] })
    .mockResolvedValueOnce({ groups: [{ ...newest, older_cursor: undefined,
      attempts: Array.from({ length: 10 }, (_, i) => ({ ...group().attempts[0], round: i + 1 })) }] })
    .mockResolvedValueOnce({ groups: [{ ...newest, parent_status: "running",
      attempts: newest.attempts.map(a => a.round === 11 ? { ...a, execution: "finished", effect: "Fresh effect" } : a) }] });
  render(<MemoryRouter initialEntries={["/runs?scope=cross-checks"]}>
    <Routes><Route element={<RunsLayout />}><Route path="/runs" element={<RunsList />} /></Route></Routes>
  </MemoryRouter>);
  const card = within(await screen.findByRole("article", { name: "Refresh work" }));
  await userEvent.click(card.getByRole("button", { name: "Load older attempts" }));
  await card.findByText("20 shown · 0 filtered · 0 remaining attempts");
  const selected = card.getByRole("button", { name: "Plan round 1 · Running" });
  await userEvent.click(selected);
  expect(document.activeElement).toBe(selected);
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await card.findByText("Work run: running");
  expect(card.getByRole("button", { name: "Plan round 1 · Last observed: Running (not refreshed)" })).toBe(selected);
  expect(document.activeElement).toBe(selected);
  expect(selected.getAttribute("aria-pressed")).toBe("true");
  expect(card.getAllByRole("heading", { level: 3 }).map(h => h.textContent)).toEqual(["Plan round 1"]);
  expect(within(card.getByRole("group", { name: "Attempt ladder" })).getAllByRole("button").map(b => b.getAttribute("title")))
    .toEqual(Array.from({ length: 20 }, (_, i) => `Plan round ${i + 1}: ${i < 10 ? "Last observed: Running (not refreshed)" : i === 10 ? "Finished" : "Running"}`));
  expect(card.getByRole("button", { name: "All matching attempts loaded" }).getAttribute("aria-disabled")).toBe("true");
  expect(card.queryByText("Earlier rounds omitted: load older attempts")).toBeNull();
  await userEvent.click(card.getByRole("button", { name: "Plan round 11 · Finished" }));
  expect(card.getByText("Fresh effect", { selector: "dd" })).toBeTruthy();
  expect(api.listCrossCheckHistory).toHaveBeenCalledTimes(3);
});

it.each([1280, 375])("preserves production layout, keyboard selection and exact focus at %ipx", async width => {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", "dawn");
  render(<MemoryRouter initialEntries={["/runs?scope=cross-checks"]}>
    <main className="min-w-0 p-4"><Routes><Route element={<RunsLayout />}>
      <Route path="/runs" element={<RunsList />} />
    </Route></Routes></main>
  </MemoryRouter>);
  const article = await screen.findByRole("article");
  const heading = within(article).getByRole("heading", { level: 2 });
  expect(heading.textContent).toBe(safeTitle);
  expect(heading.getAttribute("title")).toBe(safeTitle);
  expect(article.getAttribute("aria-label")).toBe(safeTitle);
  for (const node of [article, ...article.querySelectorAll("*")]) {
    for (const attribute of node.attributes) expect(attribute.value).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  }
  expect(getComputedStyle(article).borderTopWidth).toBe("1px");
  const selector = screen.getByRole("button", { name: "Cross-checks" });
  await userEvent.tab();
  selector.focus();
  expect(document.activeElement).toBe(selector);
  expect(selector.matches(":focus-visible")).toBe(true);
  await userEvent.type(page.elementLocator(selector), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(selector);
  const plan = within(article).getByRole("button", { name: "Plan round 2 · Running" });
  plan.focus();
  await userEvent.type(page.elementLocator(plan), "{Enter}", { skipClick: true });
  await waitFor(() => expect(plan.getAttribute("aria-pressed")).toBe("true"));
  expect(document.activeElement).toBe(plan);
  expect(within(article).getByRole("button", { name: "Hide attempt details" }).getAttribute("aria-expanded")).toBe("true");
  expect(within(article).getByText("Cause: " + safeTitle).textContent).toBe("Cause: " + safeTitle);
  const toggle = within(article).getByRole("button", { name: "Hide attempt details" });
  toggle.focus();
  await userEvent.type(page.elementLocator(toggle), "{Enter}", { skipClick: true });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  expect(document.activeElement).toBe(toggle);
  const older = within(article).getByRole("button", { name: "Load older attempts" });
  vi.mocked(api.listCrossCheckHistory).mockResolvedValueOnce({ groups: [{
    ...group(false), attempts: [{ ...group().attempts[0], round: 1 }],
  }] });
  await userEvent.click(older);
  await waitFor(() => expect(within(article).queryByText("Earlier rounds omitted: load older attempts")).toBeNull());
  expect(document.activeElement).toBe(older);
  expect(older.getAttribute("aria-disabled")).toBe("true");
  expect(within(article).getByRole("button", { name: "Plan round 2 · Running" })).toBe(plan);
  expect(plan.getAttribute("aria-pressed")).toBe("true");
  expect(article.textContent).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  await userEvent.click(toggle);
  for (const node of [article, ...article.querySelectorAll<HTMLElement>("button, a, h2, section, dd")]) {
    const rect = node.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(width);
    if (node.clientWidth > 0) expect(node.scrollWidth).toBeLessThanOrEqual(node.clientWidth + 1);
  }
});

it.each([1280, 375])("renders actual synthetic Plan, Code and childless evidence with keyboard and CSS at %ipx", async width => {
  await page.viewport(width, 900);
  vi.mocked(api.crossCheckHistoryAvailability).mockImplementation(crossChecksApi.crossCheckHistoryAvailability);
  vi.mocked(api.listCrossCheckHistory).mockImplementation(crossChecksApi.listCrossCheckHistory);
  render(<MemoryRouter initialEntries={["/runs?scope=cross-checks"]}>
    <main className="min-w-0 p-4"><Routes><Route element={<RunsLayout />}>
      <Route path="/runs" element={<RunsList />} />
    </Route></Routes></main>
  </MemoryRouter>);
  const article = await screen.findByRole("article", { name: "Synthetic cross-check work run 2" });
  const card = within(article);
  const plan = card.getByRole("button", { name: "Plan round 31 · Running" });
  await userEvent.tab();
  plan.focus();
  await userEvent.type(page.elementLocator(plan), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(plan);
  expect(plan.getAttribute("aria-pressed")).toBe("true");
  expect(card.getByRole("button", { name: "Hide attempt details" }).getAttribute("aria-expanded")).toBe("true");
  expect(card.getByRole("heading", { level: 3 }).textContent).toBe("Plan round 31");
  expect(card.getByRole("link", { name: "Open checker" }).getAttribute("href")).toBe("/runs/10000000-0000-4000-8000-000000000231");
  const code = card.getByRole("button", { name: "Code round 1 · Running" });
  code.focus();
  await userEvent.type(page.elementLocator(code), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(code);
  expect(code.getAttribute("aria-pressed")).toBe("true");
  expect(card.getByRole("heading", { level: 3 }).textContent).toBe("Code round 1");
  expect(card.getByRole("link", { name: "Open checker" }).getAttribute("href")).toBe("/runs/10000000-0000-4000-8000-000000000251");
  const childless = card.getByRole("button", { name: "Plan round 23 · Couldn't start" });
  childless.focus();
  await userEvent.type(page.elementLocator(childless), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(childless);
  expect(card.getByRole("heading", { level: 3 }).textContent).toBe("Plan round 23");
  expect(card.getByText("No checker run available")).toBeTruthy();
  expect(card.queryByRole("link", { name: "Open checker" })).toBeNull();
  expect(card.getByText("Couldn't start", { selector: "dd" })).toBeTruthy();
  expect(card.getByText("No verdict", { selector: "dd" })).toBeTruthy();
  expect(card.getByText("Effect not recorded", { selector: "dd" })).toBeTruthy();
  // Two bounded continuations expose an approval whose effect remains separate.
  for (const remaining of [12, 2]) {
    await userEvent.click(card.getByRole("button", { name: "Load older attempts" }));
    await card.findByText(new RegExp(`${remaining} remaining attempts$`));
  }
  const approved = card.getByRole("button", { name: "Plan round 4 · Finished · Approved" });
  approved.focus();
  await userEvent.type(page.elementLocator(approved), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(approved);
  expect(card.getByText("Finished", { selector: "dd" })).toBeTruthy();
  expect(card.getByText("Approved", { selector: "dd" })).toBeTruthy();
  expect(card.getByText("Effect not recorded", { selector: "dd" })).toBeTruthy();
  expect(card.getByText("Historical approval of an earlier plan candidate.")).toBeTruthy();
  expect(card.getByRole("button", { name: "Plan round 31 · Running" })).toBe(plan);
  expect(card.getByRole("button", { name: "Code round 1 · Running" })).toBe(code);
  expect(getComputedStyle(article).borderTopWidth).toBe("1px");
  for (const node of [article, ...article.querySelectorAll<HTMLElement>("button, a, h2, section, dd")]) {
    const rect = node.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(width);
    if (node.clientWidth > 0) expect(node.scrollWidth).toBeLessThanOrEqual(node.clientWidth + 1);
  }
});
